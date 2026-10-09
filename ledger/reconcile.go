package ledger

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"
)

// TrialBalanceDiscrepancy describes one (account, currency) whose net
// balance does not equal its debit totals minus credit totals (the
// per-account, per-currency accounting equation). On a healthy ledger the
// report's discrepancy list is empty; the shape exists so reconciliation
// tooling can distinguish "ran clean" from "did not run".
type TrialBalanceDiscrepancy struct {
	Account           AccountID `json:"account"`
	Currency          string    `json:"currency"`
	TotalDebitsCents  int64     `json:"total_debits_cents"`
	TotalCreditsCents int64     `json:"total_credits_cents"`
	NetBalanceCents   int64     `json:"net_balance_cents"`
	ExpectedNetCents  int64     `json:"expected_net_cents"`
	DifferenceCents   int64     `json:"difference_cents"`
}

// IdempotencyKeyHealth reports the state of the idempotency-key index at
// report time: whether a TTL is configured, and how many keys are older
// than the TTL and thus eligible for a sweep. An unexpired key count that
// keeps growing on a TTL-configured ledger is an operational smell worth
// an operator's attention at end of day.
type IdempotencyKeyHealth struct {
	TTLConfigured   bool   `json:"ttl_configured"`
	TTL             string `json:"ttl"`
	TotalKeys       int    `json:"total_keys"`
	ExpiredEligible int    `json:"expired_eligible"`
}

// AuditChainHealth reports the tamper-evident audit chain at report time:
// whether it recomputes cleanly, the current head, and whether the head is
// consistent with the ledger's own version counter (links == version and
// the newest link's seq == version, which holds under every normal Post:
// idempotent replays append no link and bump no version).
type AuditChainHealth struct {
	VerifyOK         bool   `json:"verify_ok"`
	VerifyError      string `json:"verify_error,omitempty"`
	Head             string `json:"head"`
	Links            uint64 `json:"links"`
	HeadConsistent   bool   `json:"head_consistent"`
	ConsistencyError string `json:"consistency_error,omitempty"`
}

// AccountMerge is one committed account merge (see PostMerge) as reported
// by Reconcile: which source account was consolidated into which target,
// the per-currency legs that moved, and when. Merged sources stay frozen;
// cross-check FromAccount against FrozenAccounts in the same report.
type AccountMerge struct {
	MergeID     string     `json:"merge_id"`
	FromAccount AccountID  `json:"from_account"`
	ToAccount   AccountID  `json:"to_account"`
	Legs        []MergeLeg `json:"legs"`
	CreatedAt   time.Time  `json:"created_at"`
}

// CurrencyHoldTotals is the per-currency rollup of active authorization
// holds (see hold.go) at report time: how many cents are currently
// reserved from available balances, and in how many holds. Currencies are
// never summed together — each currency's reserved funds are reported on
// their own row, sorted by currency code. Expired holds count as inactive
// (expiry is lazy); the rows only cover holds that still reserve funds.
type CurrencyHoldTotals struct {
	Currency    string `json:"currency"`
	HeldCents   int64  `json:"held_cents"`
	ActiveHolds int    `json:"active_holds"`
}

// ReconciliationReport is the end-of-day reconciliation: one consistent,
// read-only snapshot of the whole ledger's health. It answers, in one
// pass, the questions a settlement operator asks at day end: do the books
// balance (accounting equation, per-account and global), is any account
// off from its own trial balance, are idempotency keys accumulating past
// their TTL, and is the audit chain intact and in step with the ledger
// version.
type ReconciliationReport struct {
	GeneratedAt       time.Time `json:"generated_at"`
	Version           uint64    `json:"version"`
	TotalDebitsCents  int64     `json:"total_debits_cents"`
	TotalCreditsCents int64     `json:"total_credits_cents"`
	// CurrencyTotals is the per-currency rollup of the ledger's debit and
	// credit totals, sorted by currency code. The accounting equation
	// (total debits == total credits) holds independently inside each
	// row — currencies are never summed together. TotalDebitsCents and
	// TotalCreditsCents above are the DefaultCurrency row, kept for
	// backward compatibility.
	CurrencyTotals       []CurrencyTotals          `json:"currency_totals"`
	AccountingEquationOK bool                      `json:"accounting_equation_ok"`
	AccountingError      string                    `json:"accounting_error,omitempty"`
	TrialBalances        []TrialBalance            `json:"trial_balances"`
	Discrepancies        []TrialBalanceDiscrepancy `json:"discrepancies"`
	FrozenAccounts       []AccountID               `json:"frozen_accounts"`
	// OverdraftProtectedAccounts lists the accounts currently guarded
	// against overdrafts (see EnableOverdraftProtection). Risk tooling
	// reads this to know which accounts cannot go negative.
	OverdraftProtectedAccounts []AccountID `json:"overdraft_protected_accounts"`
	// DailyLimits lists every configured daily outflow limit (see
	// SetDailyLimit), sorted by (account, currency). Risk tooling reads
	// this to know which accounts are capped on daily outflow and at
	// what level.
	DailyLimits []DailyLimit `json:"daily_limits"`
	// HeldTotals is the per-currency rollup of active authorization
	// holds (see hold.go) at report time, sorted by currency code: the
	// cents currently reserved from available balances. Expired holds
	// count as inactive even before the ExpireHolds sweep.
	HeldTotals      []CurrencyHoldTotals `json:"held_totals"`
	IdempotencyKeys IdempotencyKeyHealth `json:"idempotency_keys"`
	AuditChain      AuditChainHealth     `json:"audit_chain"`
	// FXRates lists the configured FX conversion rates (see SetFXRate),
	// sorted by (from, to): the conversion table a settlement operator
	// audited against. The FX clearing account's own balances are part
	// of the per-account trial balances above, like any other account.
	FXRates []ExchangeRate `json:"fx_rates"`
	// Merges lists every committed account merge (see PostMerge), sorted
	// by merge ID: which source account was consolidated into which
	// target, the per-currency legs, and when. Merged sources stay
	// frozen — the frozen_accounts list in this report shows the
	// resulting stops.
	Merges []AccountMerge `json:"merges"`
}

// Reconcile runs a full read-only scan of the ledger and returns the
// reconciliation report. The scan holds the ledger's read lock from start
// to finish, so the whole report describes one consistent point in time:
// a concurrent Post can never slip between the equation scan and the
// chain check. The caller supplies now (the server passes time.Now()) so
// tests can pin the timestamp and TTL arithmetic stays deterministic.
func (l *Ledger) Reconcile(now time.Time) ReconciliationReport {
	l.mu.RLock()
	defer l.mu.RUnlock()
	report := l.reconcileLocked(now)
	// Reconcile is read-only, so the version brackets are identical — the
	// event still records that reconciliation ran, against which ledger
	// state, and what it found. Emitted under the read lock; the enqueue
	// never blocks.
	l.emitAudit(AuditEvent{
		Op:            "reconcile",
		Actor:         "Reconcile",
		TraceID:       fmt.Sprintf("reconcile@%d", report.Version),
		VersionBefore: report.Version,
		VersionAfter:  report.Version,
		Details: map[string]any{
			"accounting_equation_ok": report.AccountingEquationOK,
			"discrepancies":          len(report.Discrepancies),
			"merges":                 len(report.Merges),
		},
	})
	return report
}

// reconcileLocked performs the scan. Callers must hold l.mu; the read lock
// suffices because the scan mutates nothing.
func (l *Ledger) reconcileLocked(now time.Time) ReconciliationReport {
	report := ReconciliationReport{
		GeneratedAt:                now,
		Version:                    l.version,
		TrialBalances:              make([]TrialBalance, 0, len(l.balances)),
		Discrepancies:              make([]TrialBalanceDiscrepancy, 0),
		FrozenAccounts:             l.frozenAccountsLocked(),
		OverdraftProtectedAccounts: l.overdraftProtectedAccountsLocked(),
		DailyLimits:                l.dailyLimitsLocked(),
		HeldTotals:                 l.heldTotalsLocked(now),
		FXRates:                    l.fxRatesLocked(),
		Merges:                     l.mergesLocked(),
	}

	// Every account that has ever been touched. Net balances, debit
	// totals, and credit totals each cover a different (account, currency)
	// set in principle (a storage-layer corruption could strand a totals
	// row without a balance row, or vice versa), so union all three on
	// the account dimension. Sorting the accounts makes the report
	// deterministic across runs.
	seen := make(map[AccountID]bool)
	for k := range l.balances {
		seen[k.account] = true
	}
	for k := range l.debitTotals {
		seen[k.account] = true
	}
	for k := range l.creditTotals {
		seen[k.account] = true
	}
	accounts := make([]AccountID, 0, len(seen))
	for a := range seen {
		accounts = append(accounts, a)
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i] < accounts[j] })

	// One pass: per-account trial balances, per-account equation checks,
	// and the per-currency debit/credit sums. Currencies are never added
	// together: each currency's books must balance on their own.
	debits := make(map[string]int64)
	credits := make(map[string]int64)
	for _, a := range accounts {
		tb := l.trialBalanceLocked(a)
		report.TrialBalances = append(report.TrialBalances, tb)
		for _, row := range tb.ByCurrency {
			debits[row.Currency] += row.TotalDebits
			credits[row.Currency] += row.TotalCredits
			if expected := row.TotalDebits - row.TotalCredits; row.NetBalance != expected {
				report.Discrepancies = append(report.Discrepancies, TrialBalanceDiscrepancy{
					Account:           a,
					Currency:          row.Currency,
					TotalDebitsCents:  row.TotalDebits,
					TotalCreditsCents: row.TotalCredits,
					NetBalanceCents:   row.NetBalance,
					ExpectedNetCents:  expected,
					DifferenceCents:   row.NetBalance - expected,
				})
				if report.AccountingError == "" {
					report.AccountingError = fmt.Sprintf(
						"ledger: account %q (%s) out of balance: net %d != debits %d - credits %d",
						a, row.Currency, row.NetBalance, row.TotalDebits, row.TotalCredits)
				}
			}
		}
	}
	currencies := make([]string, 0, len(debits))
	for c := range debits {
		currencies = append(currencies, c)
	}
	sort.Strings(currencies)
	for _, c := range currencies {
		d, cr := debits[c], credits[c]
		report.CurrencyTotals = append(report.CurrencyTotals, CurrencyTotals{
			Currency:     c,
			TotalDebits:  d,
			TotalCredits: cr,
		})
		if c == DefaultCurrency {
			report.TotalDebitsCents = d
			report.TotalCreditsCents = cr
		}
		if report.AccountingError == "" && d != cr {
			report.AccountingError = fmt.Sprintf(
				"ledger: books do not balance in %s: total debits %d != total credits %d", c, d, cr)
		}
	}
	report.AccountingEquationOK = report.AccountingError == ""

	// Idempotency-key index health. Expired-eligible means the entry was
	// posted more than the TTL ago — exactly the set a key sweep would
	// evict, mirroring pruneIdempotencyKeysLocked's cutoff.
	report.IdempotencyKeys.TTLConfigured = l.idempotencyTTL > 0
	report.IdempotencyKeys.TTL = l.idempotencyTTL.String()
	report.IdempotencyKeys.TotalKeys = len(l.byKey)
	if report.IdempotencyKeys.TTLConfigured {
		cutoff := now.Add(-l.idempotencyTTL)
		for _, e := range l.byKey {
			if e.CreatedAt.Before(cutoff) {
				report.IdempotencyKeys.ExpiredEligible++
			}
		}
	}

	report.AuditChain = l.auditChainHealthLocked()

	return report
}

// heldTotalsLocked rolls active authorization holds up per currency at
// now, sorted by currency code. Expiry is lazy: holds whose ExpiresAt has
// passed count as inactive even before the ExpireHolds sweep. Callers must
// hold l.mu; the read lock suffices because the scan mutates nothing.
func (l *Ledger) heldTotalsLocked(now time.Time) []CurrencyHoldTotals {
	byCurrency := make(map[string]*CurrencyHoldTotals)
	for _, h := range l.holds {
		if !activeHoldLocked(h, now) {
			continue
		}
		row, ok := byCurrency[h.Currency]
		if !ok {
			row = &CurrencyHoldTotals{Currency: h.Currency}
			byCurrency[h.Currency] = row
		}
		row.HeldCents += h.AmountCents
		row.ActiveHolds++
	}
	currencies := make([]string, 0, len(byCurrency))
	for c := range byCurrency {
		currencies = append(currencies, c)
	}
	sort.Strings(currencies)
	out := make([]CurrencyHoldTotals, 0, len(currencies))
	for _, c := range currencies {
		out = append(out, *byCurrency[c])
	}
	return out
}

// mergesLocked lists every committed account merge, sorted by merge ID,
// for the reconciliation report. Callers must hold l.mu; the read lock
// suffices because the scan mutates nothing.
func (l *Ledger) mergesLocked() []AccountMerge {
	ids := make([]string, 0, len(l.merges))
	for id := range l.merges {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]AccountMerge, 0, len(ids))
	for _, id := range ids {
		rec := l.merges[id]
		out = append(out, AccountMerge{
			MergeID:     rec.mergeID,
			FromAccount: rec.from,
			ToAccount:   rec.to,
			Legs:        rec.legs,
			CreatedAt:   rec.createdAt,
		})
	}
	return out
}

// auditChainHealthLocked recomputes the audit chain and checks the head
// against the ledger version. Callers must hold l.mu; the read lock
// suffices because nothing is mutated.
func (l *Ledger) auditChainHealthLocked() AuditChainHealth {
	h := AuditChainHealth{}
	if err := l.verifyChainLocked(); err != nil {
		h.VerifyError = err.Error()
	} else {
		h.VerifyOK = true
	}

	n := uint64(len(l.chain))
	h.Links = n
	if n > 0 {
		h.Head = hex.EncodeToString(l.chain[n-1].hash[:])
	} else {
		var genesis [32]byte
		h.Head = hex.EncodeToString(genesis[:])
	}

	// Head consistency: one link per successful Post means the link count
	// must equal the ledger version, and the newest link's seq must be
	// that version. A mismatch means the chain and the journal counters
	// desynced — something VerifyChain alone cannot see.
	h.HeadConsistent = n == l.version && (n == 0 || l.chain[n-1].seq == l.version)
	if !h.HeadConsistent {
		if n != l.version {
			h.ConsistencyError = fmt.Sprintf(
				"ledger: audit chain has %d links but ledger version is %d", n, l.version)
		} else {
			h.ConsistencyError = fmt.Sprintf(
				"ledger: audit chain head link seq is %d, want ledger version %d",
				l.chain[n-1].seq, l.version)
		}
	}
	return h
}

// WriteJSON encodes the report as indented JSON to w. The indented form is
// meant for humans: it is what the operator archives from POST /reconcile.
func (r ReconciliationReport) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}
