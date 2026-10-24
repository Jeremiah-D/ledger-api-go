package ledger

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// Maker rebate tiers (LG-194).
//
// A venue pays its liquidity providers (makers) back a slice of their
// monthly maker volume: the more maker volume an account provides in a
// calendar month, the higher the rebate rate. SetRebateTiers installs
// that rate ladder; RecordMakerTrade reports maker-side fills into the
// month they belong to; RebateQuoteFor prices one account's month, and
// SettleMakerRebates closes a month and returns the accrual schedule
// (the amount to pay each maker for that month).
//
// The ladder is a non-empty slice of RebateTier sorted by MinVolumeCents:
// band i covers [MinVolumeCents_i, MinVolumeCents_{i+1}) and the last
// band is open-ended. The first band must start at zero and rates are in
// basis points (0..10000); a 0 bps bottom band is the normal shape —
// small makers earn nothing until they clear the first threshold. The
// rebate for a month is floor(volume * rateBps / 10000) cents, the same
// overflow-safe basis-point math the transfer fee policy uses (see
// policyFeeCents in transfer.go), priced at the single band the month's
// total volume lands in — not band-by-band like interest tiers, because
// a rebate is a rate on the month's volume, not interest on a balance.
//
// Wash trades are excluded, not rejected: a fill whose maker and
// counterparty are the same account (self-trading / wash volume) is
// recorded in the wash counters and never adds to the month's volume,
// so an account cannot manufacture a higher tier by trading with
// itself. A wash fill still returns no error — exclusion is the
// documented outcome, and RebateQuoteFor discloses the excluded volume
// and trade count alongside the priced volume.
//
// This module is orthogonal to fee splits (see fee_split.go): a fee
// split distributes a fee a transfer already charged; a rebate returns
// money to makers at cycle end. It also does not hook PostTransfer —
// ledger transfers carry no maker/taker side, so the matching engine or
// venue feed reports maker fills through RecordMakerTrade, and the
// operator books the settled payout through the ordinary transfer path.
//
// Settling a month closes it: RecordMakerTrade for a settled month is
// rejected with ErrRebateMonthSettled, and a second SettleMakerRebates
// for the same month returns the stored schedule instead of repricing
// (idempotent — a retry never double-accrues). The tier table and the
// volume registries are process-local structural state, like the
// payee-verification policy: setting the table does not bump the
// journal version and neither table nor volumes round-trip through
// snapshots. Every table change and every settlement is recorded in
// the structured audit log as "maker_rebate_tiers_set",
// "maker_rebate_tiers_cleared", or "maker_rebate_settled".

// RebateTier is one band of the maker-rebate ladder: monthly maker
// volumes from MinVolumeCents (inclusive) up to the next tier's
// MinVolumeCents (exclusive) earn RebateBps per month. MinVolumeCents
// must be strictly ascending across the table and the first band must
// start at zero; RebateBps is the rebate rate in basis points
// (100 = 1% of the month's maker volume).
type RebateTier struct {
	MinVolumeCents int64 `json:"min_volume_cents"`
	RebateBps      int64 `json:"rebate_bps"`
}

// RebateQuote prices one account's month: the volume that counts, the
// band it lands in, the rebate it earns, and the wash volume that was
// excluded from that volume (disclosed, never silently dropped).
type RebateQuote struct {
	Account           AccountID `json:"account"`
	Month             string    `json:"month"`
	VolumeCents       int64     `json:"volume_cents"`
	TierIndex         int       `json:"tier_index"`
	RebateBps         int64     `json:"rebate_bps"`
	RebateCents       int64     `json:"rebate_cents"`
	WashExcludedCents int64     `json:"wash_excluded_cents"`
	WashExcludedTrades int64    `json:"wash_excluded_trades"`
}

// rebateWashStat counts the wash fills excluded from one account's month.
type rebateWashStat struct {
	amountCents int64
	trades      int64
}

var (
	// ErrInvalidRebateTier is returned by SetRebateTiers when the table
	// is malformed: first band not at zero, minima not strictly
	// ascending, a negative minimum, or a rate outside 0..10000 bps.
	ErrInvalidRebateTier = errors.New("ledger: rebate tier table must start at 0 with strictly ascending minima and rates between 0 and 10000 basis points")
	// ErrRebateTiersNotConfigured is returned by RebateQuoteFor and
	// SettleMakerRebates when no tier table is installed.
	ErrRebateTiersNotConfigured = errors.New("ledger: no maker rebate tier table configured")
	// ErrInvalidRebateMonth is returned when a month is not a calendar
	// month in "YYYY-MM" form.
	ErrInvalidRebateMonth = errors.New("ledger: rebate month must be YYYY-MM")
	// ErrRebateMonthSettled is returned by RecordMakerTrade when the
	// fill's month is already settled: settled months are closed.
	ErrRebateMonthSettled = errors.New("ledger: maker rebate month already settled")
	// ErrInvalidMakerTrade is returned by RecordMakerTrade for an empty
	// maker account or a non-positive fill amount.
	ErrInvalidMakerTrade = errors.New("ledger: maker trade needs a non-empty maker account and a positive amount")
)

// validateRebateTiers checks the ladder shape without touching the
// ledger: first band at zero, strictly ascending minima, non-negative
// minima, rates in 0..10000. An empty table is valid — it means "clear".
func validateRebateTiers(tiers []RebateTier) error {
	for i, t := range tiers {
		if t.MinVolumeCents < 0 {
			return fmt.Errorf("%w: band %d has negative minimum %d", ErrInvalidRebateTier, i, t.MinVolumeCents)
		}
		if t.RebateBps < 0 || t.RebateBps > 10000 {
			return fmt.Errorf("%w: band %d rate %d bps out of range", ErrInvalidRebateTier, i, t.RebateBps)
		}
		if i == 0 {
			if t.MinVolumeCents != 0 {
				return fmt.Errorf("%w: first band must start at 0, got %d", ErrInvalidRebateTier, t.MinVolumeCents)
			}
			continue
		}
		if t.MinVolumeCents <= tiers[i-1].MinVolumeCents {
			return fmt.Errorf("%w: band %d minimum %d not above band %d minimum %d",
				ErrInvalidRebateTier, i, t.MinVolumeCents, i-1, tiers[i-1].MinVolumeCents)
		}
	}
	return nil
}

// rebateTiersEqual reports whether two tier tables are identical band
// by band.
func rebateTiersEqual(a, b []RebateTier) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// SetRebateTiers installs (or replaces) the maker-rebate ladder.
// Passing an empty (or nil) table clears it and returns rebate quoting
// to "not configured". Setting the table is structural: it does not
// bump the journal version, and the change is recorded in the audit
// log as "maker_rebate_tiers_set" (or "_cleared"). Re-installing the
// identical table is a silent no-op, so idempotent retries stay silent
// in the compliance trail.
func (l *Ledger) SetRebateTiers(tiers []RebateTier) error {
	if err := validateRebateTiers(tiers); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	if rebateTiersEqual(l.rebateTiers, tiers) {
		return nil
	}
	cp := append([]RebateTier(nil), tiers...)
	l.rebateTiers = cp

	op := "maker_rebate_tiers_set"
	details := map[string]any{}
	if len(cp) > 0 {
		bands := make([]map[string]any, 0, len(cp))
		for _, t := range cp {
			bands = append(bands, map[string]any{
				"min_volume_cents": t.MinVolumeCents,
				"rebate_bps":       t.RebateBps,
			})
		}
		details["tiers"] = bands
	} else {
		op = "maker_rebate_tiers_cleared"
	}
	l.emitAudit(AuditEvent{
		Op:            op,
		Actor:         "SetRebateTiers",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details:       details,
	})
	return nil
}

// RebateTiers returns the installed tier table, or nil when rebate
// quoting is off. The returned slice is a copy.
func (l *Ledger) RebateTiers() []RebateTier {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return append([]RebateTier(nil), l.rebateTiers...)
}

// rebateTierFor returns the band a month's volume lands in: the last
// band whose minimum does not exceed the volume. Callers must pass a
// non-empty table they own (or hold the ledger lock).
func rebateTierFor(volumeCents int64, tiers []RebateTier) (int, int64) {
	idx := 0
	for i, t := range tiers {
		if volumeCents >= t.MinVolumeCents {
			idx = i
		} else {
			break
		}
	}
	return idx, tiers[idx].RebateBps
}

// validateRebateMonth checks "YYYY-MM" shape and a real calendar month.
func validateRebateMonth(month string) error {
	if len(month) != 7 || month[4] != '-' {
		return fmt.Errorf("%w: %q", ErrInvalidRebateMonth, month)
	}
	t, err := time.Parse("2006-01", month)
	if err != nil || t.Format("2006-01") != month {
		return fmt.Errorf("%w: %q", ErrInvalidRebateMonth, month)
	}
	return nil
}

// RecordMakerTrade reports one maker-side fill for maker against
// counterparty, booked into the calendar month of at (UTC; a zero at
// means now). It returns counted=true when the fill added to the
// month's volume, counted=false when it was a wash trade (maker ==
// counterparty) excluded from volume. Wash fills are disclosed, not
// errors. A fill for an already-settled month is rejected — settled
// months are closed — as is a fill with an empty maker account or a
// non-positive amount.
func (l *Ledger) RecordMakerTrade(maker, counterparty AccountID, amountCents int64, at time.Time) (bool, error) {
	if maker == "" || amountCents <= 0 {
		return false, ErrInvalidMakerTrade
	}
	if at.IsZero() {
		at = time.Now()
	}
	month := at.UTC().Format("2006-01")

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.rebateSettled[month] {
		return false, ErrRebateMonthSettled
	}
	if maker == counterparty {
		if l.rebateWash == nil {
			l.rebateWash = make(map[string]map[AccountID]rebateWashStat)
		}
		byAccount := l.rebateWash[month]
		if byAccount == nil {
			byAccount = make(map[AccountID]rebateWashStat)
			l.rebateWash[month] = byAccount
		}
		st := byAccount[maker]
		total, ok := addCents(st.amountCents, amountCents)
		if !ok {
			return false, fmt.Errorf("ledger: wash excluded volume overflows int64")
		}
		st.amountCents = total
		st.trades++
		byAccount[maker] = st
		return false, nil
	}
	if l.rebateVolumes == nil {
		l.rebateVolumes = make(map[string]map[AccountID]int64)
	}
	byAccount := l.rebateVolumes[month]
	if byAccount == nil {
		byAccount = make(map[AccountID]int64)
		l.rebateVolumes[month] = byAccount
	}
	total, ok := addCents(byAccount[maker], amountCents)
	if !ok {
		return false, fmt.Errorf("ledger: maker volume overflows int64")
	}
	byAccount[maker] = total
	return true, nil
}

// MakerVolume returns an account's counted maker volume for a month
// (wash fills excluded), or 0 when nothing was recorded. The month
// must be "YYYY-MM".
func (l *Ledger) MakerVolume(maker AccountID, month string) (int64, error) {
	if err := validateRebateMonth(month); err != nil {
		return 0, err
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.rebateVolumes[month][maker], nil
}

// rebateQuoteLocked prices one account's month under the ledger's
// installed table. Callers must hold the write lock (or a read lock
// for a pure quote) and pass a non-empty table.
func (l *Ledger) rebateQuoteLocked(maker AccountID, month string) RebateQuote {
	volume := l.rebateVolumes[month][maker]
	wash := l.rebateWash[month][maker]
	idx, bps := rebateTierFor(volume, l.rebateTiers)
	return RebateQuote{
		Account:            maker,
		Month:              month,
		VolumeCents:        volume,
		TierIndex:          idx,
		RebateBps:          bps,
		RebateCents:        policyFeeCents(volume, bps),
		WashExcludedCents:  wash.amountCents,
		WashExcludedTrades: wash.trades,
	}
}

// RebateQuoteFor prices one account's month: the counted volume, the
// band it lands in, and the rebate that band earns, plus the wash
// volume excluded from it. It requires an installed tier table and a
// "YYYY-MM" month.
func (l *Ledger) RebateQuoteFor(maker AccountID, month string) (RebateQuote, error) {
	if err := validateRebateMonth(month); err != nil {
		return RebateQuote{}, err
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	if len(l.rebateTiers) == 0 {
		return RebateQuote{}, ErrRebateTiersNotConfigured
	}
	return l.rebateQuoteLocked(maker, month), nil
}

// SettleMakerRebates closes a month and returns its accrual schedule:
// one quote per account with any recorded activity that month (counted
// volume, wash fills, or both), sorted by account for a deterministic
// schedule. Settling is idempotent: a second call for the same month
// returns the stored schedule instead of repricing, so a retry never
// double-accrues, and the month is closed to further fills. The
// settlement is recorded in the audit log as "maker_rebate_settled"
// with the month, the account count, and the total rebate.
func (l *Ledger) SettleMakerRebates(month string) ([]RebateQuote, error) {
	if err := validateRebateMonth(month); err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.rebateTiers) == 0 {
		return nil, ErrRebateTiersNotConfigured
	}
	if l.rebateSettled[month] {
		return append([]RebateQuote(nil), l.rebateSettlements[month]...), nil
	}
	accounts := make(map[AccountID]bool)
	for a := range l.rebateVolumes[month] {
		accounts[a] = true
	}
	for a := range l.rebateWash[month] {
		accounts[a] = true
	}
	sorted := make([]AccountID, 0, len(accounts))
	for a := range accounts {
		sorted = append(sorted, a)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	schedule := make([]RebateQuote, 0, len(sorted))
	var total int64
	for _, a := range sorted {
		q := l.rebateQuoteLocked(a, month)
		var ok bool
		total, ok = addCents(total, q.RebateCents)
		if !ok {
			return nil, fmt.Errorf("ledger: maker rebate settlement total overflows int64")
		}
		schedule = append(schedule, q)
	}
	if l.rebateSettled == nil {
		l.rebateSettled = make(map[string]bool)
	}
	if l.rebateSettlements == nil {
		l.rebateSettlements = make(map[string][]RebateQuote)
	}
	l.rebateSettled[month] = true
	l.rebateSettlements[month] = append([]RebateQuote(nil), schedule...)
	l.emitAudit(AuditEvent{
		Op:            "maker_rebate_settled",
		Actor:         "SettleMakerRebates",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"month":             month,
			"accounts":          len(schedule),
			"total_rebate_cents": total,
		},
	})
	return schedule, nil
}

// MakerRebateSettlement returns the stored accrual schedule for a
// settled month, or ok=false when the month has not been settled.
func (l *Ledger) MakerRebateSettlement(month string) ([]RebateQuote, bool, error) {
	if err := validateRebateMonth(month); err != nil {
		return nil, false, err
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	if !l.rebateSettled[month] {
		return nil, false, nil
	}
	return append([]RebateQuote(nil), l.rebateSettlements[month]...), true, nil
}
