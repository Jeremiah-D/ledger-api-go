package ledger

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"
)

// TrialBalanceDiscrepancy describes one account whose net balance does not
// equal its debit totals minus credit totals (the per-account accounting
// equation). On a healthy ledger the report's discrepancy list is empty;
// the shape exists so reconciliation tooling can distinguish "ran clean"
// from "did not run".
type TrialBalanceDiscrepancy struct {
	Account           AccountID `json:"account"`
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

// ReconciliationReport is the end-of-day reconciliation: one consistent,
// read-only snapshot of the whole ledger's health. It answers, in one
// pass, the questions a settlement operator asks at day end: do the books
// balance (accounting equation, per-account and global), is any account
// off from its own trial balance, are idempotency keys accumulating past
// their TTL, and is the audit chain intact and in step with the ledger
// version.
type ReconciliationReport struct {
	GeneratedAt          time.Time                 `json:"generated_at"`
	Version              uint64                    `json:"version"`
	TotalDebitsCents     int64                     `json:"total_debits_cents"`
	TotalCreditsCents    int64                     `json:"total_credits_cents"`
	AccountingEquationOK bool                      `json:"accounting_equation_ok"`
	AccountingError      string                    `json:"accounting_error,omitempty"`
	TrialBalances        []TrialBalance            `json:"trial_balances"`
	Discrepancies        []TrialBalanceDiscrepancy `json:"discrepancies"`
	FrozenAccounts       []AccountID               `json:"frozen_accounts"`
	IdempotencyKeys      IdempotencyKeyHealth      `json:"idempotency_keys"`
	AuditChain           AuditChainHealth          `json:"audit_chain"`
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
	return l.reconcileLocked(now)
}

// reconcileLocked performs the scan. Callers must hold l.mu; the read lock
// suffices because the scan mutates nothing.
func (l *Ledger) reconcileLocked(now time.Time) ReconciliationReport {
	report := ReconciliationReport{
		GeneratedAt:    now,
		Version:        l.version,
		TrialBalances:  make([]TrialBalance, 0, len(l.balances)),
		Discrepancies:  make([]TrialBalanceDiscrepancy, 0),
		FrozenAccounts: l.frozenAccountsLocked(),
	}

	// Every account that has ever been touched. Net balances, debit
	// totals, and credit totals each cover a different account set in
	// principle (a storage-layer corruption could strand a totals row
	// without a balance row, or vice versa), so union all three. Sorting
	// the accounts makes the report deterministic across runs.
	seen := make(map[AccountID]bool)
	for a := range l.balances {
		seen[a] = true
	}
	for a := range l.debitTotals {
		seen[a] = true
	}
	for a := range l.creditTotals {
		seen[a] = true
	}
	accounts := make([]AccountID, 0, len(seen))
	for a := range seen {
		accounts = append(accounts, a)
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i] < accounts[j] })

	// One pass: per-account trial balances, per-account equation checks,
	// and the global debit/credit sums.
	var debits, credits int64
	for _, a := range accounts {
		d := l.debitTotals[a]
		c := l.creditTotals[a]
		net := l.balances[a]
		report.TrialBalances = append(report.TrialBalances, TrialBalance{
			Account:      a,
			TotalDebits:  d,
			TotalCredits: c,
			NetBalance:   net,
			Version:      l.version,
			Frozen:       l.frozen[a],
		})
		if expected := d - c; net != expected {
			report.Discrepancies = append(report.Discrepancies, TrialBalanceDiscrepancy{
				Account:           a,
				TotalDebitsCents:  d,
				TotalCreditsCents: c,
				NetBalanceCents:   net,
				ExpectedNetCents:  expected,
				DifferenceCents:   net - expected,
			})
			if report.AccountingError == "" {
				report.AccountingError = fmt.Sprintf(
					"ledger: account %q out of balance: net %d != debits %d - credits %d",
					a, net, d, c)
			}
		}
		debits += d
		credits += c
	}
	report.TotalDebitsCents = debits
	report.TotalCreditsCents = credits
	if report.AccountingError == "" && debits != credits {
		report.AccountingError = fmt.Sprintf(
			"ledger: books do not balance: total debits %d != total credits %d", debits, credits)
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
