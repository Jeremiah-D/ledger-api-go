package ledger

import (
	"errors"
	"sort"
)

// Client-money segregation (LG-220, fintech: client money protection).
//
// Some accounts hold money that belongs to customers, not to the
// operator — client deposits, escrowed funds, money awaiting payout
// to a third party. That money must never mix with the operator's
// own funds: a transfer from a client-money account into an own
// account (or the reverse) would let operator spending draw on
// customer funds, or customer funds absorb operator losses. So
// segregation here is a property of the account, enforced at
// posting time, not a separate pot of money somewhere else:
//
//   - MarkClientMoneyAccount opts an account into the client-money
//     class. Every other account is own money by default; there
//     is no third class, so every movement is either within one
//     class (allowed) or across classes (rejected).
//   - A cross-class movement is fail-closed with
//     ErrClientMoneySegregationViolation and zero side effects:
//     no journal row, no balance change, no version bump. The
//     gate runs on Post, on PostTransfer (same-currency, split,
//     batch, and forward transfers all funnel through
//     postTransferLocked), and on cross-currency transfers
//     (postTransferFXLocked commits directly, so it carries its
//     own check). Fee legs are exempt, like the dormant and
//     allowlist gates: a fee recipient is the operator's own
//     revenue account, not a party to the transfer, so the
//     principal legs alone decide the class question.
//   - The gate runs after the idempotency replay check (a replay
//     books nothing new, so it must not fail on an account
//     marked after the original posting) and after the
//     freeze/suspend/close state gates, so those keep their
//     precedence: a frozen account still reports ErrAccountFrozen
//     even when the transfer would also cross classes. Virtual
//     accounts resolve to their parent before the gate, so the
//     parent's class governs, like every other account risk gate.
//
// This is orthogonal to freeze/suspend/close (see freeze.go,
// suspend.go, close.go): those are account state gates — they stop
// all movement through an account for a while or forever. This is
// a fund-ownership gate: a marked account keeps moving freely
// within its class while frozen/suspended/closed accounts stop
// entirely, and marking an account changes nothing about its
// state-gate status. It is orthogonal to the chart of accounts
// (see chart.go): chart gates posting direction, this gates which
// class an account may exchange with.
//
// Marking is structural config, like the cash-class mark (see
// cashflow.go): it does not bump the journal version, it is
// audited ("client_money_marked" / "client_money_unmarked"), and
// re-marking (or un-marking an unmarked account) is an idempotent
// no-op. Marks are in-memory only, like deferred-revenue contracts
// (see deferred_revenue.go): they do not round-trip through
// disaster-recovery snapshots, so the operator re-marks accounts
// on restore — the journaled rows and the audit events are the
// durable record, and the money itself is never at risk from a
// lost mark, only the gate until it is re-applied.
//
// Un-marking is symmetric with marking and deliberately has no
// balance gate: it reclassifies the account for future movements
// and drops it from the segregation report, but it moves no money.
// An operator that un-marks a funded account takes responsibility
// for that reclassification — the audit event records it — and
// the account's balance simply leaves the segregated pool.
//
// The segregation report (see SegregationReport) lists every
// marked account's balance per currency and totals them. The gap
// is a reconciliation assertion, not a funding measurement: the
// ledger models no external segregated bank account, so the
// "required" segregated total is the marked accounts' balances
// from the balance map, and the "actual" segregated total is the
// same sum recomputed independently from the journal rows
// (debit legs add, credit legs subtract). A zero gap means the
// segregated pool reconciles with the books; a non-zero gap
// would mean balances and journal disagree, which no posting
// path can produce — the gate above is what keeps client money
// from ever leaving the pool in the first place.

// Client-money segregation errors.
var (
	// ErrEmptyClientMoneyAccount is returned by
	// MarkClientMoneyAccount and UnmarkClientMoneyAccount when
	// the account is empty.
	ErrEmptyClientMoneyAccount = errors.New("ledger: client-money account must not be empty")
	// ErrClientMoneySegregationViolation is returned by Post and
	// PostTransfer when a movement would cross the client-money /
	// own-money class boundary: exactly one of the two legs is a
	// client-money account.
	ErrClientMoneySegregationViolation = errors.New("ledger: transfer between client-money and own-money accounts is forbidden")
)

// isClientMoneyLocked reports whether account carries the
// client-money mark. Callers must hold l.mu; the read lock
// suffices.
func (l *Ledger) isClientMoneyLocked(account AccountID) bool {
	return l.clientMoneyAccounts[account]
}

// clientMoneyMismatchLocked reports whether a movement between
// debit and credit would cross the segregation boundary: exactly
// one leg is a client-money account. Callers must hold l.mu.
func (l *Ledger) clientMoneyMismatchLocked(debit, credit AccountID) bool {
	return l.isClientMoneyLocked(debit) != l.isClientMoneyLocked(credit)
}

// MarkClientMoneyAccount opts an account into the client-money
// class: from now on it may only exchange with other
// client-money accounts. Marking an already-marked account is an
// idempotent no-op. Structural config: no version bump, audited
// as "client_money_marked".
func (l *Ledger) MarkClientMoneyAccount(account AccountID) error {
	if account == "" {
		return ErrEmptyClientMoneyAccount
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.clientMoneyAccounts[account] = true
	l.emitAudit(AuditEvent{
		Op:       "client_money_marked",
		Actor:    "MarkClientMoneyAccount",
		Accounts: []AccountID{account},
		Details: map[string]any{
			"account": string(account),
		},
	})
	return nil
}

// UnmarkClientMoneyAccount removes the client-money mark from an
// account, returning it to the own-money class for future
// movements. It moves no money: the account's balance simply
// leaves the segregated pool and the segregation report.
// Un-marking an unmarked account is a no-op. Structural config:
// no version bump, audited as "client_money_unmarked".
func (l *Ledger) UnmarkClientMoneyAccount(account AccountID) error {
	if account == "" {
		return ErrEmptyClientMoneyAccount
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.clientMoneyAccounts, account)
	l.emitAudit(AuditEvent{
		Op:       "client_money_unmarked",
		Actor:    "UnmarkClientMoneyAccount",
		Accounts: []AccountID{account},
		Details: map[string]any{
			"account": string(account),
		},
	})
	return nil
}

// IsClientMoneyAccount reports whether account carries the
// client-money mark.
func (l *Ledger) IsClientMoneyAccount(account AccountID) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.isClientMoneyLocked(account)
}

// ClientMoneyAccounts lists the marked accounts, sorted.
func (l *Ledger) ClientMoneyAccounts() []AccountID {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]AccountID, 0, len(l.clientMoneyAccounts))
	for a := range l.clientMoneyAccounts {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ClientMoneyBalance is one marked account's balance in one
// currency, as listed by SegregationReport.
type ClientMoneyBalance struct {
	Account      AccountID `json:"account"`
	Currency     string    `json:"currency"`
	BalanceCents int64     `json:"balance_cents"`
}

// SegregationCurrencyTotal is one currency's slice of the
// segregation report: ClientMoneyCents is the total client money
// in that currency, RequiredCents is the amount that must be
// held segregated (the same total — the marked accounts are the
// segregated holding), SegregatedCents is that total recomputed
// independently from the journal rows, and GapCents is
// RequiredCents minus SegregatedCents. A zero gap means the
// segregated pool reconciles with the books.
type SegregationCurrencyTotal struct {
	Currency         string `json:"currency"`
	ClientMoneyCents int64  `json:"client_money_cents"`
	RequiredCents    int64  `json:"required_cents"`
	SegregatedCents  int64  `json:"segregated_cents"`
	GapCents         int64  `json:"gap_cents"`
}

// SegregationReport is the client-money segregation report: one
// balance row per (marked account, currency) with a balance, the
// per-currency totals, and GapZero, which is true exactly when
// every currency's gap is zero.
type SegregationReport struct {
	Accounts []ClientMoneyBalance       `json:"accounts"`
	Totals   []SegregationCurrencyTotal `json:"totals"`
	GapZero  bool                       `json:"gap_zero"`
}

// SegregationReport builds the segregation report: every marked
// account's balance per currency, sorted by account then
// currency, with per-currency totals and the zero-gap assertion.
// Accounts with no balance in a currency contribute no row for
// that currency; a marked account with no postings at all
// contributes no rows. Read-only: it moves no money and bumps
// nothing.
func (l *Ledger) SegregationReport() SegregationReport {
	l.mu.RLock()
	defer l.mu.RUnlock()
	report := SegregationReport{GapZero: true}
	totals := map[string]*SegregationCurrencyTotal{}
	for key, balance := range l.balances {
		if !l.isClientMoneyLocked(key.account) {
			continue
		}
		report.Accounts = append(report.Accounts, ClientMoneyBalance{
			Account:      key.account,
			Currency:     key.currency,
			BalanceCents: balance,
		})
		total, ok := totals[key.currency]
		if !ok {
			total = &SegregationCurrencyTotal{Currency: key.currency}
			totals[key.currency] = total
		}
		total.ClientMoneyCents += balance
		total.RequiredCents += balance
	}
	// Recompute the segregated total from the journal rows: a
	// debit leg adds to the account's balance, a credit leg
	// subtracts from it. The balance map and the journal must
	// agree — that agreement is the gap assertion.
	for _, e := range l.entries {
		total, ok := totals[e.Currency]
		if !ok {
			continue
		}
		if l.isClientMoneyLocked(e.DebitAccount) {
			total.SegregatedCents += e.AmountCents
		}
		if l.isClientMoneyLocked(e.CreditAccount) {
			total.SegregatedCents -= e.AmountCents
		}
	}
	// Note: the journal loop above only touches currencies that
	// already have a balance-map row, because a marked account's
	// journal rows always leave a balance-map entry behind (a zero
	// balance still occupies a key). Totals therefore cover every
	// currency with client-money activity.
	for _, total := range totals {
		total.GapCents = total.RequiredCents - total.SegregatedCents
		if total.GapCents != 0 {
			report.GapZero = false
		}
		report.Totals = append(report.Totals, *total)
	}
	sort.Slice(report.Accounts, func(i, j int) bool {
		if report.Accounts[i].Account != report.Accounts[j].Account {
			return report.Accounts[i].Account < report.Accounts[j].Account
		}
		return report.Accounts[i].Currency < report.Accounts[j].Currency
	})
	sort.Slice(report.Totals, func(i, j int) bool {
		return report.Totals[i].Currency < report.Totals[j].Currency
	})
	return report
}
