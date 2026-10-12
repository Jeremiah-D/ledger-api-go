package ledger

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"time"
)

// FX revaluation (汇兑重估).
//
// A multi-currency ledger keeps each currency's books isolated, but at
// period end an operator wants to know what the foreign-currency balances
// are worth in the reporting (base) currency — and to recognize the
// unrealized FX gain or loss in profit and loss. RevalueBalances does
// that: for every (account, currency) with currency != baseCurrency, it
// converts the balance at the current (currency -> base) rate from the FX
// rate table (see fx.go) and books the *change* in base-currency carrying
// value since the last revaluation.
//
// The carrying value is tracked per (account, currency, base) in
// Ledger.revalBaselines, and it always equals the account's booked
// base-currency balance: the first revaluation books the full converted
// value (the initial recognition of the foreign balance in base terms),
// and every later revaluation books only the delta. Each revaluation is
// one ordinary journal entry in the base currency:
//
//	gain (delta > 0): debit  account@base |delta|, credit revalAccount@base |delta|
//	loss (delta < 0): debit  revalAccount@base |delta|, credit account@base |delta|
//
// The account's base-currency row is its unrealized FX carrying value;
// the revaluation P&L account accumulates the recognized gains/losses.
// Both legs stay in the base currency, so the per-currency accounting
// equation holds by construction and the foreign-currency balances are
// never touched. When a foreign balance returns to zero, the delta
// reverses the full carrying value to zero, so no stale value lingers.
//
// The entry ID is deterministic — "<asof unix>/<account>/<currency>/reval"
// — so re-running a revaluation for the same asOf is idempotent: existing
// entries are reported as skipped_duplicate and nothing is booked twice.
// (The asOf is rendered as Unix seconds; two runs within the same second
// share the ID namespace, which is what makes a retry safe.)
//
// RevalueBalances is best-effort per (account, currency), not atomic
// across accounts: each item reports its own outcome
// (revalued | baseline_set | unchanged | skipped_duplicate |
// skipped_no_rate | skipped_rate_expired | failed), so one frozen account
// or one missing rate never blocks the other ninety-nine. The report
// lists every item, and only items that actually booked entries appear in
// Entries.
//
// The revaluation entries are ordinary journal entries — chain link,
// version bump, accounting equation — committed directly, *not* through
// Post: journal rules (LG-47) do not fire and dual-control review (LG-42)
// does not apply, exactly like period-closing entries (LG-48). The Post
// risk gates do apply per entry, in Post order: frozen, overdraft,
// daily-limit, period (keyed on the asOf, so a revaluation can never book
// into a closed period). Like Post, the run fires the balance-change hook
// and evaluates low-balance alerts after committing.
//
// Baseline semantics to be aware of:
//   - The first revaluation books the full converted value as the initial
//     recognition of the foreign balance in base-currency terms; only
//     later runs book pure FX deltas.
//   - New money posted between revaluations is recognized at the current
//     rate inside the next delta. For clean FX-only attribution, run the
//     revaluation at period end after all postings.
//   - An account whose foreign balance returns to zero gets its carrying
//     value reversed to zero (a loss/gain entry), so no stale carrying
//     value lingers.
//   - A rate that is missing or expired at asOf skips the account without
//     touching its baseline; the next run with a live rate measures the
//     full move since the last successful revaluation.

// Revaluation errors.
var (
	// ErrRevaluationAccountNotConfigured is returned by RevalueBalances
	// when no revaluation P&L account is configured (see
	// SetRevaluationAccount). Fail-closed: without a P&L account the
	// gain/loss legs have nowhere to go. It is a 400-class error — the
	// operator can fix it by configuring the account.
	ErrRevaluationAccountNotConfigured = errors.New("ledger: revaluation P&L account not configured")
	// ErrRevaluationIDConflict is returned for one revaluation item when
	// its deterministic entry ID is already taken by a journal entry
	// that is not a genuine revaluation entry (memo mismatch): somebody
	// posted into the revaluation ID namespace by hand. The item is
	// reported as failed; other items proceed.
	ErrRevaluationIDConflict = errors.New("ledger: revaluation entry ID already used by an unrelated journal entry")
)

// revalKey identifies one revaluation track: an account's balance in a
// foreign currency, remeasured into a base currency.
type revalKey struct {
	account  AccountID
	currency string // foreign currency
	base     string // base (reporting) currency
}

// revalBaseline is the last remeasured carrying value of one revalKey:
// the foreign balance and its base-currency value at the last successful
// revaluation, and the rate that produced it.
type revalBaseline struct {
	foreignBalance int64
	baseValue      int64
	rateNum        int64
	rateDen        int64
	rateVersion    uint64
	revaluedAt     time.Time
}

// RevaluationPosition is one (account, currency) revaluation baseline as
// reported by Reconcile's revaluation_impact group and carried through
// disaster-recovery snapshots: the last remeasured carrying value and the
// rate that produced it.
type RevaluationPosition struct {
	Account        AccountID `json:"account"`
	Currency       string    `json:"currency"`
	BaseCurrency   string    `json:"base_currency"`
	ForeignBalance int64     `json:"foreign_balance_cents"`
	BaseValue      int64     `json:"base_value_cents"`
	RateNum        int64     `json:"rate_num"`
	RateDen        int64     `json:"rate_den"`
	RateVersion    uint64    `json:"rate_version"`
	RevaluedAt     time.Time `json:"revalued_at"`
}

// Revaluation item statuses reported per (account, currency).
const (
	RevalStatusRevalued          = "revalued"             // entry booked
	RevalStatusUnchanged         = "unchanged"            // delta zero: nothing booked
	RevalStatusSkippedDuplicate  = "skipped_duplicate"    // entry ID already journaled (idempotent)
	RevalStatusSkippedNoRate     = "skipped_no_rate"      // no (currency -> base) rate configured
	RevalStatusSkippedRateExpiry = "skipped_rate_expired" // rate expired at asOf
	RevalStatusFailed            = "failed"               // gate rejection or ID conflict; see Error
)

// RevaluationItem is the outcome for one (account, currency) in a
// RevalueBalances run.
type RevaluationItem struct {
	Account        AccountID `json:"account"`
	Currency       string    `json:"currency"`
	BaseCurrency   string    `json:"base_currency"`
	ForeignBalance int64     `json:"foreign_balance_cents"`
	RateNum        int64     `json:"rate_num"`
	RateDen        int64     `json:"rate_den"`
	RateVersion    uint64    `json:"rate_version"`
	PriorBaseValue int64     `json:"prior_base_value_cents"`
	NewBaseValue   int64     `json:"new_base_value_cents"`
	// DeltaCents is the signed booked amount: positive is a gain (the
	// foreign balance gained value in base terms), negative a loss. Zero
	// unless Status == "revalued".
	DeltaCents int64  `json:"delta_cents"`
	EntryID    string `json:"entry_id,omitempty"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
}

// RevaluationReport is the result of one RevalueBalances run.
type RevaluationReport struct {
	AsOf         time.Time         `json:"as_of"`
	BaseCurrency string            `json:"base_currency"`
	// RateVersion is the FX rate-table snapshot version (see fx_snapshot.go)
	// in effect for this run.
	RateVersion uint64             `json:"rate_version"`
	Items       []RevaluationItem  `json:"items"`
	// Entries are the journal entries this run committed, in commit order.
	Entries     []JournalEntry     `json:"entries"`
}

// WithRevaluationAccount configures the P&L account that receives the
// gain/loss legs of FX revaluations (see RevalueBalances).
func WithRevaluationAccount(a AccountID) Option {
	return func(l *Ledger) {
		l.revalAccount = a
	}
}

// SetRevaluationAccount configures (or clears, with the empty account) the
// P&L account that receives the gain/loss legs of FX revaluations. It is
// structural config, like the FX account: setting it does not bump the
// journal version, it survives disaster-recovery snapshots, and the change
// is recorded in the structured audit log.
func (l *Ledger) SetRevaluationAccount(a AccountID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.revalAccount = a
	l.emitAudit(AuditEvent{
		Op:      "set_revaluation_account",
		Actor:   "SetRevaluationAccount",
		TraceID: string(a),
		Details: map[string]any{"revaluation_account": string(a)},
	})
}

// RevaluationAccount returns the configured revaluation P&L account, or
// "" when revaluation is not configured.
func (l *Ledger) RevaluationAccount() AccountID {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.revalAccount
}

// revalEntryIDFor returns the deterministic journal entry ID of the
// revaluation of (account, currency -> base) at asOf.
func revalEntryIDFor(asOfUnix int64, account AccountID, currency string) string {
	return fmt.Sprintf("%d/%s/%s/reval", asOfUnix, account, currency)
}

// revalMemoFor returns the memo stamped on every revaluation entry. The
// "reval:" prefix makes revaluations searchable through
// GET /entries?memo=reval:, and the full memo lets RevalueBalances tell a
// genuine revaluation entry apart from a hand-posted ID collision.
func revalMemoFor(currency, base string, asOfUnix int64) string {
	return fmt.Sprintf("reval:%s->%s:asof=%d", currency, base, asOfUnix)
}

// subCents returns a-b and whether the difference fits in an int64.
func subCents(a, b int64) (int64, bool) {
	if b == math.MinInt64 {
		// -b overflows; a-b overflows unless a == MinInt64 (then 0).
		if a == math.MinInt64 {
			return 0, true
		}
		return 0, false
	}
	return addCents(a, -b)
}

// RevalueBalances remeasures every non-base-currency balance into
// baseCurrency at the current FX rate table and books the unrealized FX
// gain/loss since the last revaluation (see the package comment). asOf is
// the measurement time: it timestamps the entries, keys their IDs, and
// gates the period check. A zero asOf means now.
//
// It returns ErrInvalidCurrency for a malformed base currency and
// ErrRevaluationAccountNotConfigured when no P&L account is configured.
// Per-item problems (missing/expired rate, gate rejections, ID conflicts)
// are reported in the returned report, never as a returned error: the run
// is best-effort per (account, currency).
func (l *Ledger) RevalueBalances(asOf time.Time, baseCurrency string) (RevaluationReport, error) {
	base, err := normalizeCurrency(baseCurrency)
	if err != nil {
		return RevaluationReport{}, err
	}
	if asOf.IsZero() {
		asOf = time.Now()
	}
	asOfUnix := asOf.Unix()

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.revalAccount == "" {
		return RevaluationReport{}, ErrRevaluationAccountNotConfigured
	}

	report := RevaluationReport{
		AsOf:         asOf,
		BaseCurrency: base,
		RateVersion:  l.fxSnapshotVersion,
	}

	// Candidates: every (account, currency) with a balance in a foreign
	// currency, plus every revaluation track in this base (so a track
	// whose foreign balance returned to zero gets its carrying value
	// reversed to zero instead of lingering).
	type candidate struct {
		account  AccountID
		currency string
	}
	seen := make(map[candidate]bool)
	var candidates []candidate
	for k := range l.balances {
		if k.currency == base {
			continue
		}
		c := candidate{account: k.account, currency: k.currency}
		if !seen[c] {
			seen[c] = true
			candidates = append(candidates, c)
		}
	}
	for k := range l.revalBaselines {
		if k.base != base {
			continue
		}
		c := candidate{account: k.account, currency: k.currency}
		if !seen[c] {
			seen[c] = true
			candidates = append(candidates, c)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].account != candidates[j].account {
			return candidates[i].account < candidates[j].account
		}
		return candidates[i].currency < candidates[j].currency
	})

	l.maybePruneIdempotencyKeys(asOf)
	l.maybePruneDailyOutflowLocked(asOf)
	versionBefore := l.version
	var committed []JournalEntry
	allTouched := []accountCurrency{}
	allOld := map[accountCurrency]int64{}
	// recordOld snapshots the current balance of each touched
	// (account, currency) the first time it is touched, so the
	// balance-change hook reports exact per-account deltas even though
	// entries commit one by one through the loop.
	recordOld := func(acs ...accountCurrency) {
		for _, ac := range acs {
			if _, ok := allOld[ac]; !ok {
				allOld[ac] = l.balances[ac]
				allTouched = append(allTouched, ac)
			}
		}
	}

	for _, c := range candidates {
		item := RevaluationItem{
			Account:        c.account,
			Currency:       c.currency,
			BaseCurrency:   base,
			ForeignBalance: l.balances[accountCurrency{account: c.account, currency: c.currency}],
		}
		entryID := revalEntryIDFor(asOfUnix, c.account, c.currency)
		expectedMemo := revalMemoFor(c.currency, base, asOfUnix)

		// Idempotency: the deterministic ID makes a repeated run for the
		// same asOf a no-op. The memo check tells a genuine revaluation
		// entry apart from a hand-posted ID collision.
		if existing, taken := l.entries[entryID]; taken {
			if existing.Memo == expectedMemo {
				item.Status = RevalStatusSkippedDuplicate
				item.EntryID = entryID
				report.Items = append(report.Items, item)
				continue
			}
			item.Status = RevalStatusFailed
			item.Error = ErrRevaluationIDConflict.Error()
			report.Items = append(report.Items, item)
			continue
		}

		rate, ok := l.fxRates[fxPair{from: c.currency, to: base}]
		if !ok {
			item.Status = RevalStatusSkippedNoRate
			report.Items = append(report.Items, item)
			continue
		}
		// A rate past its expiry at the measurement time is never
		// applied silently — same rule as cross-currency transfers.
		if !rate.ExpiresAt.IsZero() && !asOf.Before(rate.ExpiresAt) {
			item.Status = RevalStatusSkippedRateExpiry
			item.RateNum = rate.Num
			item.RateDen = rate.Den
			item.RateVersion = l.fxSnapshotVersion
			report.Items = append(report.Items, item)
			continue
		}
		item.RateNum = rate.Num
		item.RateDen = rate.Den
		item.RateVersion = l.fxSnapshotVersion

		newValue, ok := fxConvertSignedCents(item.ForeignBalance, rate.Num, rate.Den)
		if !ok {
			item.Status = RevalStatusFailed
			item.Error = ErrAmountOverflow.Error()
			report.Items = append(report.Items, item)
			continue
		}
		item.NewBaseValue = newValue

		key := revalKey{account: c.account, currency: c.currency, base: base}
		baseline, hasBaseline := l.revalBaselines[key]
		// The prior carrying value is the last remeasured value, or zero
		// on the first revaluation — so the first run books the full
		// converted value (initial recognition), and every later run
		// books only the delta. The booked base-currency balance always
		// equals the baseline.
		var priorValue int64
		if hasBaseline {
			priorValue = baseline.baseValue
		}
		item.PriorBaseValue = priorValue

		delta, ok := subCents(newValue, priorValue)
		if !ok {
			item.Status = RevalStatusFailed
			item.Error = ErrAmountOverflow.Error()
			report.Items = append(report.Items, item)
			continue
		}
		if delta == 0 {
			// Nothing moved: refresh the baseline's bookkeeping (the
			// foreign balance may have changed with an offsetting rate
			// move) without booking.
			l.revalBaselines[key] = revalBaseline{
				foreignBalance: item.ForeignBalance,
				baseValue:      newValue,
				rateNum:        rate.Num,
				rateDen:        rate.Den,
				rateVersion:    l.fxSnapshotVersion,
				revaluedAt:     asOf,
			}
			item.Status = RevalStatusUnchanged
			report.Items = append(report.Items, item)
			continue
		}

		amount := delta
		if amount < 0 {
			if amount == math.MinInt64 {
				item.Status = RevalStatusFailed
				item.Error = ErrAmountOverflow.Error()
				report.Items = append(report.Items, item)
				continue
			}
			amount = -amount
		}
		entry := JournalEntry{
			ID:         entryID,
			AmountCents: amount,
			Currency:   base,
			Memo:       expectedMemo,
			CreatedAt:  asOf,
		}
		if delta > 0 {
			// Gain: the foreign balance is worth more base currency.
			// The account's carrying value grows (debit) and the P&L
			// recognizes the gain (credit).
			entry.DebitAccount = c.account
			entry.CreditAccount = l.revalAccount
		} else {
			// Loss: mirror image.
			entry.DebitAccount = l.revalAccount
			entry.CreditAccount = c.account
		}
		if err := validateJournalEntry(&entry); err != nil {
			item.Status = RevalStatusFailed
			item.Error = err.Error()
			report.Items = append(report.Items, item)
			continue
		}
		// The Post risk gates, in Post order, on the revaluation entry.
		// Journal rules deliberately do not fire and the entry never
		// enters dual-control review (see the package comment).
		var gateErr error
		switch {
		case l.closedLocked(entry.DebitAccount) || l.closedLocked(entry.CreditAccount):
			gateErr = ErrAccountClosed
		case l.frozenLocked(entry.DebitAccount) || l.frozenLocked(entry.CreditAccount):
			gateErr = ErrAccountFrozen
		case l.overdraftRejectedLocked(entry):
			gateErr = ErrAccountOverdraft
		case l.dailyLimitRejectedLocked(entry.CreditAccount, entry.Currency, entry.AmountCents, entry.CreatedAt):
			gateErr = ErrDailyLimitExceeded
		default:
			gateErr = l.periodRejectedLocked(entry.CreatedAt)
		}
		if gateErr != nil {
			item.Status = RevalStatusFailed
			item.Error = gateErr.Error()
			report.Items = append(report.Items, item)
			continue
		}

		recordOld(
			accountCurrency{account: entry.DebitAccount, currency: base},
			accountCurrency{account: entry.CreditAccount, currency: base},
		)
		l.commitEntryLocked(entry)
		l.addDailyOutflowLocked(entry.CreditAccount, entry.Currency, entry.AmountCents, entry.CreatedAt)
		l.revalBaselines[key] = revalBaseline{
			foreignBalance: item.ForeignBalance,
			baseValue:      newValue,
			rateNum:        rate.Num,
			rateDen:        rate.Den,
			rateVersion:    l.fxSnapshotVersion,
			revaluedAt:     asOf,
		}
		committed = append(committed, entry)
		item.DeltaCents = delta
		item.EntryID = entryID
		item.Status = RevalStatusRevalued
		report.Items = append(report.Items, item)
	}
	report.Entries = committed

	if len(committed) > 0 {
		entryIDs := make([]string, 0, len(committed))
		accounts := []AccountID{}
		seenAccounts := map[AccountID]bool{}
		for _, e := range committed {
			entryIDs = append(entryIDs, e.ID)
			for _, a := range []AccountID{e.DebitAccount, e.CreditAccount} {
				if !seenAccounts[a] {
					seenAccounts[a] = true
					accounts = append(accounts, a)
				}
			}
		}
		// Low-balance alert evaluation: strictly after the commit zone,
		// read-only (see low_balance.go). Advisory only.
		l.evaluateBalanceWatchesLocked(allTouched, fmt.Sprintf("revaluation@%d", asOfUnix), "RevalueBalances")
		l.emitAudit(AuditEvent{
			Op:            "revaluation",
			Actor:         "RevalueBalances",
			TraceID:       fmt.Sprintf("revaluation@%d", asOfUnix),
			VersionBefore: versionBefore,
			VersionAfter:  l.version,
			EntryIDs:      entryIDs,
			Accounts:      accounts,
			Details: map[string]any{
				"as_of":             asOf.UTC().Format(time.RFC3339),
				"base_currency":     base,
				"rate_version":      l.fxSnapshotVersion,
				"items":             len(report.Items),
				"entries_committed": len(committed),
			},
		})
		// Domain outbox: one entry_posted per revaluation entry.
		l.emitOutboxEntriesLocked(committed)
		// Balance-change notification: strictly after the commit zone,
		// advisory only — async dispatch, hook failures isolated.
		l.fireBalanceHooksLocked(allTouched, allOld, fmt.Sprintf("revaluation@%d", asOfUnix))
	}

	return report, nil
}

// revaluationPositionsLocked lists every revaluation baseline, sorted by
// (account, currency, base), for the reconciliation report and for
// snapshots. Callers must hold l.mu; the read lock suffices.
func (l *Ledger) revaluationPositionsLocked() []RevaluationPosition {
	out := make([]RevaluationPosition, 0, len(l.revalBaselines))
	for k, b := range l.revalBaselines {
		out = append(out, RevaluationPosition{
			Account:        k.account,
			Currency:       k.currency,
			BaseCurrency:   k.base,
			ForeignBalance: b.foreignBalance,
			BaseValue:      b.baseValue,
			RateNum:        b.rateNum,
			RateDen:        b.rateDen,
			RateVersion:    b.rateVersion,
			RevaluedAt:     b.revaluedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Account != out[j].Account {
			return out[i].Account < out[j].Account
		}
		if out[i].Currency != out[j].Currency {
			return out[i].Currency < out[j].Currency
		}
		return out[i].BaseCurrency < out[j].BaseCurrency
	})
	return out
}
