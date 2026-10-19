package ledger

import (
	"errors"
	"strings"
	"time"
)

// Fixed-asset depreciation: systematic monthly amortization of
// capitalized assets (opt-in).
//
// A fixed asset books one journal entry per (asset, calendar month):
// debit the depreciation expense account, credit the accumulated
// depreciation (contra-asset) account. The entry ID is deterministic —
// "depr/<assetID>/<YYYY-MM>" (UTC month) — so re-running the month's
// depreciation is idempotent: the second call returns the original entry
// with duplicate == true and books nothing new.
//
// A depreciation entry is an ordinary journal entry: it carries its own
// audit-chain link (the hash covers every journaled field), it bumps the
// ledger version, and it flows through VerifyAccountingEquation and
// Reconcile exactly like a hand-posted entry. It runs the full Post gate
// order — field validation, idempotency replay, chart direction, frozen,
// overdraft, daily-limit, period — with two deliberate exceptions, the
// same two accruals take (see accrual.go): it never fires the journal
// rules engine (system-generated adjusting entries must not cascade
// derived legs off operator-configured rules), and it never enters
// dual-control review (review routing lives on PostTransfer's path only).
// The depreciation method rides in the memo ("depreciation:straight-line",
// "depreciation:double-declining"), so depreciation rows are discoverable
// through the ordinary journal listing and the ?memo= filter.
//
// Depreciation never exceeds the depreciable base (cost minus salvage):
// the computed monthly charge is capped at the remaining base, and a
// fully depreciated asset books nothing — Depreciate returns an empty
// JournalEntry with a nil error. Reversing a depreciation month with the
// ordinary ReverseEntry reduces the accumulated total (the reversal nets
// against the original in AccumulatedDepreciation), so a corrected month
// can be re-run.
//
// This module complements accruals (see accrual.go, LG-55): accruals are
// systematic expense pre-accruals (interest, fees), depreciation is
// systematic amortization of capitalized fixed assets.
//
// Amounts are int64 cents and the asset's currency rides on the entry
// (see Currency); every computation below is overflow-checked — a sum
// that does not fit int64 is a data error, not a wrappable number.

// Depreciation methods.
const (
	// DepreciationStraightLine spreads (cost - salvage) evenly over the
	// useful life: floor(base / months) per month, with the final month
	// trued up to the remaining base so the lifetime total is exact.
	DepreciationStraightLine = "straight-line"
	// DepreciationDoubleDeclining charges floor(bookValue * 2 / months)
	// per month against the declining book value (the 200% declining
	// balance method on a monthly cadence), floored at the salvage
	// value — it never depreciates below salvage.
	DepreciationDoubleDeclining = "double-declining"
)

// FixedAsset is the master record for one capitalized asset. ID doubles
// as the depreciation entry-ID namespace ("depr/<ID>/<YYYY-MM>"), so it
// must not contain '/'. Account is the capitalized asset account the
// depreciation is computed against; the expense and accumulated
// depreciation legs are supplied per Depreciate call so one asset can
// post into different books (e.g. GAAP vs tax) without re-registration.
type FixedAsset struct {
	ID               string    `json:"id"`
	Account          AccountID `json:"account"`
	CostCents        int64     `json:"cost_cents"`
	Currency         string    `json:"currency"`
	SalvageCents     int64     `json:"salvage_cents"`
	UsefulLifeMonths int64     `json:"useful_life_months"`
	Method           string    `json:"method"`
}

// Validation errors for fixed-asset depreciation.
var (
	// ErrInvalidAssetID is returned when the asset ID is empty or
	// contains '/': the deterministic depreciation entry ID joins its
	// components with '/', so a '/' inside the ID would make two
	// different assets share one entry namespace.
	ErrInvalidAssetID = errors.New("ledger: fixed asset ID must be non-empty and must not contain '/'")
	// ErrInvalidAssetAccount is returned when the asset account is empty.
	ErrInvalidAssetAccount = errors.New("ledger: fixed asset account must be non-empty")
	// ErrInvalidAssetCost is returned when the capitalized cost is not
	// positive: there is nothing to depreciate.
	ErrInvalidAssetCost = errors.New("ledger: fixed asset cost must be positive")
	// ErrInvalidSalvage is returned when the salvage value is negative
	// or not below cost: the depreciable base (cost - salvage) must be
	// positive.
	ErrInvalidSalvage = errors.New("ledger: salvage value must be non-negative and below cost")
	// ErrInvalidUsefulLife is returned when the useful life is not a
	// positive number of months.
	ErrInvalidUsefulLife = errors.New("ledger: useful life must be a positive number of months")
	// ErrInvalidDepreciationMethod is returned when the method is not
	// "straight-line" or "double-declining".
	ErrInvalidDepreciationMethod = errors.New("ledger: depreciation method must be \"straight-line\" or \"double-declining\"")
	// ErrFixedAssetExists is returned when registering an asset ID that
	// is already registered: re-registration is rejected so a typo
	// cannot silently replace an asset's master record.
	ErrFixedAssetExists = errors.New("ledger: fixed asset ID is already registered")
	// ErrFixedAssetNotFound is returned when depreciating or querying an
	// asset ID that was never registered.
	ErrFixedAssetNotFound = errors.New("ledger: fixed asset not found")
	// ErrInvalidDepreciationAccount is returned when a depreciation leg
	// account is empty or contains '/'.
	ErrInvalidDepreciationAccount = errors.New("ledger: depreciation leg accounts must be non-empty and must not contain '/'")
)

// depreciationEntryPrefix is the entry-ID namespace for one asset's
// depreciation months: "depr/<assetID>/".
func depreciationEntryPrefix(assetID string) string {
	return "depr/" + assetID + "/"
}

// depreciationEntryID is the deterministic idempotent entry ID for one
// asset's calendar month: "depr/<assetID>/<YYYY-MM>".
func depreciationEntryID(assetID string, asof time.Time) string {
	return depreciationEntryPrefix(assetID) + asof.UTC().Format("2006-01")
}

func validDepreciationMethod(method string) bool {
	return method == DepreciationStraightLine || method == DepreciationDoubleDeclining
}

// RegisterFixedAsset validates and stores a fixed-asset master record.
// Registration is structural config: it does not bump the journal
// version. Registering a duplicate ID fails with ErrFixedAssetExists.
func (l *Ledger) RegisterFixedAsset(asset FixedAsset) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := validateFixedAsset(asset); err != nil {
		return err
	}
	if _, ok := l.fixedAssets[asset.ID]; ok {
		return ErrFixedAssetExists
	}
	l.fixedAssets[asset.ID] = asset
	return nil
}

func validateFixedAsset(asset FixedAsset) error {
	if asset.ID == "" || strings.Contains(asset.ID, "/") {
		return ErrInvalidAssetID
	}
	if asset.Account == "" {
		return ErrInvalidAssetAccount
	}
	if asset.CostCents <= 0 {
		return ErrInvalidAssetCost
	}
	if asset.SalvageCents < 0 || asset.SalvageCents >= asset.CostCents {
		return ErrInvalidSalvage
	}
	if asset.UsefulLifeMonths <= 0 {
		return ErrInvalidUsefulLife
	}
	if !validDepreciationMethod(asset.Method) {
		return ErrInvalidDepreciationMethod
	}
	if _, err := normalizeCurrency(asset.Currency); err != nil {
		return err
	}
	return nil
}

// GetFixedAsset returns the registered master record for an asset ID.
func (l *Ledger) GetFixedAsset(assetID string) (FixedAsset, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	asset, ok := l.fixedAssets[assetID]
	if !ok {
		return FixedAsset{}, ErrFixedAssetNotFound
	}
	return asset, nil
}

// AccumulatedDepreciation returns the net depreciation booked for an
// asset across all months: the sum of its "depr/<assetID>/<YYYY-MM>"
// entries, net of any reversals of those entries (a reversed month nets
// to zero, so the month can be re-run). Amounts are int64 cents in the
// asset's currency; the sum is overflow-checked.
func (l *Ledger) AccumulatedDepreciation(assetID string) (int64, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if _, ok := l.fixedAssets[assetID]; !ok {
		return 0, ErrFixedAssetNotFound
	}
	return l.accumulatedDepreciationLocked(assetID)
}

// accumulatedDepreciationLocked is AccumulatedDepreciation's body.
// Callers must hold l.mu (read or write): it scans the journal for this
// asset's depreciation namespace. Depreciation is a monthly batch job,
// not a hot path, so a full journal scan is acceptable.
func (l *Ledger) accumulatedDepreciationLocked(assetID string) (int64, error) {
	prefix := depreciationEntryPrefix(assetID)
	var total int64
	for id, entry := range l.entries {
		if !strings.HasPrefix(id, prefix) {
			continue
		}
		rest := strings.TrimPrefix(id, prefix)
		var amount int64
		switch {
		case rest == "":
			continue
		case !strings.Contains(rest, "/"):
			// "YYYY-MM": one booked depreciation month.
			amount = entry.AmountCents
		case strings.HasSuffix(rest, "/reversal") &&
			!strings.Contains(strings.TrimSuffix(rest, "/reversal"), "/"):
			// "YYYY-MM/reversal": nets against its month, so a
			// reversed month contributes zero and Depreciate can
			// re-run it.
			amount = -entry.AmountCents
		default:
			// Foreign entry sharing the namespace prefix (e.g. a
			// hand-posted ID) — not one of ours, skip it.
			continue
		}
		var ok bool
		total, ok = addCents(total, amount)
		if !ok {
			return 0, errDepreciationOverflow()
		}
	}
	return total, nil
}

// errDepreciationOverflow reports an overflow while summing depreciation.
func errDepreciationOverflow() error {
	return errors.New("ledger: accumulated depreciation overflows int64")
}

// Depreciate books one month's depreciation for a registered asset:
// debit the expense account, credit the accumulated depreciation
// account. asof selects the calendar month (UTC); a zero asof means now.
//
// The entry ID is the deterministic "depr/<assetID>/<YYYY-MM>", so
// re-running the same month is idempotent: the second call returns the
// original entry with duplicate == true and books nothing new.
//
// The monthly charge is capped at the remaining depreciable base
// (cost - salvage - accumulated): depreciation never takes the book
// value below salvage. A fully depreciated asset books nothing —
// Depreciate returns an empty JournalEntry with a nil error.
//
// The charge is an ordinary journal entry: it runs the full Post gate
// order (validation, idempotency replay, chart direction, frozen,
// overdraft, daily-limit, period), carries its own audit-chain link,
// bumps the version, and flows through VerifyAccountingEquation and
// Reconcile. It never fires the journal rules engine and never enters
// dual-control review (see the package doc comment).
func (l *Ledger) Depreciate(assetID string, expense, accumulated AccountID, asof time.Time) (JournalEntry, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.depreciateLocked(assetID, expense, accumulated, asof)
}

// depreciateLocked is Depreciate's body. Callers must hold l.mu (the
// write lock): it computes the charge and posts atomically.
func (l *Ledger) depreciateLocked(assetID string, expense, accumulated AccountID, asof time.Time) (JournalEntry, bool, error) {
	asset, ok := l.fixedAssets[assetID]
	if !ok {
		return JournalEntry{}, false, ErrFixedAssetNotFound
	}
	if expense == "" || accumulated == "" ||
		strings.Contains(string(expense), "/") || strings.Contains(string(accumulated), "/") {
		return JournalEntry{}, false, ErrInvalidDepreciationAccount
	}
	if asof.IsZero() {
		asof = time.Now()
	}
	id := depreciationEntryID(assetID, asof)

	booked, err := l.accumulatedDepreciationLocked(assetID)
	if err != nil {
		return JournalEntry{}, false, err
	}
	base, ok := addCents(asset.CostCents, -asset.SalvageCents)
	if !ok {
		return JournalEntry{}, false, errDepreciationOverflow()
	}
	remaining, ok := addCents(base, -booked)
	if !ok {
		return JournalEntry{}, false, errDepreciationOverflow()
	}
	// Fully depreciated: book nothing. An empty entry with a nil error
	// is the "nothing to do" signal — distinct from duplicate == true,
	// which means "already booked this month".
	if remaining <= 0 {
		return JournalEntry{}, false, nil
	}

	amount := monthlyDepreciation(asset, booked)
	if amount <= 0 {
		// Dust true-up: the computed charge rounded to zero but base
		// remains (e.g. double-declining on a tiny book value) — book
		// the remainder so the asset can finish depreciating.
		amount = remaining
	} else if amount > remaining {
		// Cap at the remaining depreciable base: depreciation never
		// takes the book value below salvage.
		amount = remaining
	}

	e := JournalEntry{
		ID:             id,
		DebitAccount:   expense,
		CreditAccount:  accumulated,
		AmountCents:    amount,
		Currency:       asset.Currency,
		IdempotencyKey: id,
		Memo:           "depreciation:" + asset.Method,
		CreatedAt:      asof,
	}

	// fireRules=false: depreciation entries are system adjusting entries
	// and must not cascade through operator-configured journal rules (see
	// the package doc comment). They still run the full gate order —
	// validation, idempotency replay, chart direction, frozen,
	// overdraft, daily-limit, period — commit a full journal row, and
	// bump the version. Dual-control review is never consulted: review
	// routing lives on PostTransfer's path only.
	return l.postSystemLocked(e, false)
}

// monthlyDepreciation computes one month's charge for an asset given the
// net depreciation already booked. It never exceeds the remaining base;
// the caller caps and trues up.
func monthlyDepreciation(asset FixedAsset, booked int64) int64 {
	base := asset.CostCents - asset.SalvageCents
	switch asset.Method {
	case DepreciationStraightLine:
		// Even spread with floor division; the caller trues up the
		// final month via the remaining-base cap.
		return base / asset.UsefulLifeMonths
	case DepreciationDoubleDeclining:
		// 200% declining balance on a monthly cadence: 2/usefulLife
		// applied to the current book value each month.
		bookValue := asset.CostCents - booked
		if bookValue <= 0 {
			return 0
		}
		// Overflow-safe: bookValue <= CostCents <= MaxInt64, and
		// UsefulLifeMonths >= 1, so bookValue*2 cannot overflow
		// unless CostCents is within 2 of MaxInt64 — guard anyway.
		doubled, ok := addCents(bookValue, bookValue)
		if !ok {
			return 0
		}
		return doubled / asset.UsefulLifeMonths
	default:
		return 0
	}
}
