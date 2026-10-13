package ledger

import (
	"errors"
	"sort"
	"strings"
	"time"
)

// Account statement generation (LG-71).
//
// GenerateStatement builds a read-only bank-style statement for one
// account over a half-open time window [from, to): opening and closing
// balances per currency, the period's effective journal rows in
// (CreatedAt, ID) order, a per-currency rollup of the transfer fee legs
// the account paid or received, and a structured event summary (freeze
// transitions, dual-control transfer reviews, overdraft-protection
// state).
//
// Read-only, strictly: the scan holds the read lock from start to
// finish, so the statement describes one consistent point in time. It
// bumps no version, books nothing, appends no audit-chain link, and
// triggers no rules or hooks — the same read-only contract as Reconcile.
// (Reconcile additionally emits an audit event saying it ran; a
// statement is a pure query and does not, so generating one leaves no
// trace at all.)
//
// Balance semantics: opening and closing balances fold the effective
// journal prefix with CreatedAt < from / < to (see balanceAtLocked in
// ofx.go) — the time-domain twin of BalanceAt's version-prefix fold.
// Pending-review rows are journaled but not effective (they never
// touched balances), so — like the OFX statement export — the period
// entry list excludes them, and opening + the period rows' net always
// equals closing. Re-export after approval shows the settled rows.

// ErrInvalidStatementRange is returned by GenerateStatement when from is
// after to: an inverted statement window. An empty window (from == to)
// is valid and yields an empty period with opening == closing balances.
var ErrInvalidStatementRange = errors.New("ledger: statement range is invalid: from is after to")

// AccountFreezeEvent is one freeze-state transition of an account,
// recorded in memory by the freeze paths (see freeze.go) so read-only
// reports can show the account's freeze history for a period. Kind is
// "frozen" (manual Freeze or timed FreezeUntil), "unfrozen" (manual
// Unfreeze), or "auto_released" (a timed freeze whose deadline passed,
// released by the lazy write-path check, the background sweeper, or
// ExpireFrozenAccounts). AutoUnfreezeAt is set on "frozen" when the
// freeze carried a deadline; Trigger names the release path on
// "auto_released" (the same trigger recorded on the audit event).
//
// This is an operational in-memory index, not books: it does not
// round-trip through snapshots (like the FrozenAutoReleases counter),
// and the structured audit log (Op "freeze" / "unfreeze" /
// "frozen_auto_released") remains the durable record.
type AccountFreezeEvent struct {
	Account        AccountID `json:"account"`
	Kind           string    `json:"kind"`
	At             time.Time `json:"at"`
	AutoUnfreezeAt time.Time `json:"auto_unfreeze_at,omitempty"`
	Trigger        string    `json:"trigger,omitempty"`
}

// Freeze event kinds recorded on AccountFreezeEvent.
const (
	FreezeEventFrozen       = "frozen"
	FreezeEventUnfrozen     = "unfrozen"
	FreezeEventAutoReleased = "auto_released"
)

// recordFreezeEventLocked appends one freeze-state transition to the
// in-memory history. Callers must hold the write lock.
func (l *Ledger) recordFreezeEventLocked(ev AccountFreezeEvent) {
	l.freezeEvents = append(l.freezeEvents, ev)
}

// StatementPeriod is the statement's half-open window [from, to).
type StatementPeriod struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// StatementCurrencyBalance is one (currency, balance) row of the
// statement's opening or closing balances, sorted by currency code.
// Currencies are never summed together — each currency's books stay
// isolated, like every other per-currency report in this ledger.
type StatementCurrencyBalance struct {
	Currency     string `json:"currency"`
	BalanceCents int64  `json:"balance_cents"`
}

// StatementFeeSummary is the per-currency rollup of the transfer fee
// legs (see Transfer.FeeCents / the fee policy in transfer.go) that
// touched the account in the statement period. A fee leg is the journal
// row with ID "<transfer ID>/fee": the payer (credit leg) pays the fee
// to the fee account (debit leg). PaidCents is what left the account as
// fees; ReceivedCents is what the account collected as the fee account.
// Pending-review transfers never booked fee legs, so they contribute
// nothing here.
type StatementFeeSummary struct {
	Currency      string `json:"currency"`
	PaidCents     int64  `json:"paid_cents"`
	ReceivedCents int64  `json:"received_cents"`
	EntryCount    int    `json:"entry_count"`
}

// StatementTransferReview is one dual-control transfer review (see
// review.go) involving the account that was created in the statement
// period. Status is one of "pending_review", "approved", "rejected", or
// "expired" (the ReviewStatus* constants). ReviewedAt/ReviewedBy are set
// once an operator (or the expiry sweeper, as "system") decides the
// review.
type StatementTransferReview struct {
	TransferID  string    `json:"transfer_id"`
	Status      string    `json:"status"`
	AmountCents int64     `json:"amount_cents"`
	Currency    string    `json:"currency"`
	CreatedAt   time.Time `json:"created_at"`
	ReviewedAt  time.Time `json:"reviewed_at,omitempty"`
	ReviewedBy  string    `json:"reviewed_by,omitempty"`
}

// StatementEventSummary is the structured event summary of the
// statement period. Every list is drawn from the ledger's structured
// in-memory sources — never fabricated:
//
//   - FreezeEvents: the account's freeze/unfreeze/auto-release
//     transitions with At in [from, to), in chronological order.
//   - TransferReviews: dual-control reviews involving the account
//     (as payer or payee) created in [from, to), with ReviewCounts
//     tallying them by status.
//   - OverdraftProtected: whether the account is currently guarded
//     against overdrafts (see EnableOverdraftProtection). Overdraft
//     rejections themselves are not listed: rejected posts record
//     nothing by design (see freeze.go's ErrAccountFrozen doc and
//     overdraft.go), so there is no structured source to draw them
//     from, and inventing them would be a lie.
//   - CurrentlyFrozen: whether the account is frozen at generation
//     time, for context on the period's tail.
type StatementEventSummary struct {
	FreezeEvents       []AccountFreezeEvent      `json:"freeze_events"`
	TransferReviews    []StatementTransferReview `json:"transfer_reviews"`
	ReviewCounts       map[string]int            `json:"review_counts"`
	OverdraftProtected bool                      `json:"overdraft_protected"`
	CurrentlyFrozen    bool                      `json:"currently_frozen"`
}

// AccountStatement is one account's statement over [from, to): the
// read-only output of GenerateStatement.
type AccountStatement struct {
	Account         AccountID               `json:"account"`
	Period          StatementPeriod         `json:"period"`
	OpeningBalances []StatementCurrencyBalance `json:"opening_balances"`
	ClosingBalances []StatementCurrencyBalance `json:"closing_balances"`
	// Entries are the period's effective journal rows touching the
	// account (either leg) with CreatedAt in [from, to), sorted by
	// (CreatedAt, ID) — the same ordering the cursor-paged entry lists
	// use. Pending-review rows are excluded (they never touched
	// balances); the entry JSON carries no pending_review marker here
	// because none can appear.
	Entries []JournalEntry `json:"entries"`
	// FeeSummary is the per-currency fee-leg rollup, sorted by currency
	// code. Empty when the account paid and received no transfer fees in
	// the period.
	FeeSummary   []StatementFeeSummary `json:"fee_summary"`
	EventSummary StatementEventSummary `json:"event_summary"`
	GeneratedAt  time.Time             `json:"generated_at"`
	// Version is the ledger version the statement was read at: a
	// balance-change detector for statement consumers (see Ledger's doc
	// comment) — the same version Snapshot reports.
	Version uint64 `json:"version"`
}

// statementMonthStart returns the first instant of the UTC month
// containing t.
func statementMonthStart(t time.Time) time.Time {
	y, m, _ := t.UTC().Date()
	return time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
}

// GenerateStatement builds the account statement for [from, to). A zero
// to means now; a zero from means the first day of to's UTC month at
// 00:00 — the "current month to date" default the HTTP layer relies on.
// An empty account ID is rejected with ErrEmptyAccountID; from after to
// with ErrInvalidStatementRange. Unknown accounts yield a valid empty
// statement (accounts are implicit in this ledger, like every other read
// path): empty balances, entries, fee summary, and event lists.
//
// The whole generation runs under the single read lock: the statement
// is one consistent point-in-time view, and concurrent Posts can never
// slip between the balance fold and the entry scan.
func (l *Ledger) GenerateStatement(account AccountID, from, to time.Time) (*AccountStatement, error) {
	if account == "" {
		return nil, ErrEmptyAccountID
	}
	if to.IsZero() {
		to = time.Now().UTC()
	}
	if from.IsZero() {
		from = statementMonthStart(to)
	}
	if from.After(to) {
		return nil, ErrInvalidStatementRange
	}

	l.mu.RLock()
	defer l.mu.RUnlock()

	stmt := &AccountStatement{
		Account:         account,
		Period:          StatementPeriod{From: from, To: to},
		OpeningBalances: []StatementCurrencyBalance{},
		ClosingBalances: []StatementCurrencyBalance{},
		Entries:         []JournalEntry{},
		FeeSummary:      []StatementFeeSummary{},
		GeneratedAt:     time.Now().UTC(),
		Version:         l.version,
	}

	// Period rows: effective journal rows touching the account in
	// [from, to), in (CreatedAt, ID) order. Pending-review rows are
	// journaled but not effective — a statement showing them would
	// disagree with the ledger balances, so they are skipped exactly
	// like the OFX export skips them.
	var rows []JournalEntry
	for _, id := range l.byAccount[account] {
		e := l.entries[id]
		if e.PendingReview {
			continue
		}
		if e.CreatedAt.Before(from) || !e.CreatedAt.Before(to) {
			continue
		}
		rows = append(rows, e)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].CreatedAt.Equal(rows[j].CreatedAt) {
			return rows[i].ID < rows[j].ID
		}
		return rows[i].CreatedAt.Before(rows[j].CreatedAt)
	})
	// rows stays nil when the period is empty: keep the initialized
	// empty (non-nil) slice so the JSON encodes [] rather than null.
	if rows != nil {
		stmt.Entries = rows
	}

	// Currencies with any effective activity before to: the union that
	// opening and closing balances are reported over, sorted.
	seen := make(map[string]bool)
	for _, id := range l.byAccount[account] {
		e := l.entries[id]
		if e.PendingReview || !e.CreatedAt.Before(to) {
			continue
		}
		seen[e.Currency] = true
	}
	currencies := make([]string, 0, len(seen))
	for c := range seen {
		currencies = append(currencies, c)
	}
	sort.Strings(currencies)
	for _, c := range currencies {
		stmt.OpeningBalances = append(stmt.OpeningBalances, StatementCurrencyBalance{
			Currency:     c,
			BalanceCents: l.balanceAtLocked(account, c, from),
		})
		stmt.ClosingBalances = append(stmt.ClosingBalances, StatementCurrencyBalance{
			Currency:     c,
			BalanceCents: l.balanceAtLocked(account, c, to),
		})
	}

	// Fee summary: the period's fee legs (ID "<transfer ID>/fee") that
	// touched the account, rolled up per currency.
	fees := make(map[string]*StatementFeeSummary)
	for _, e := range rows {
		if !strings.HasSuffix(e.ID, "/fee") {
			continue
		}
		row, ok := fees[e.Currency]
		if !ok {
			row = &StatementFeeSummary{Currency: e.Currency}
			fees[e.Currency] = row
		}
		// The fee leg debits the fee account and credits the payer:
		// money left the account exactly when it is the credit leg.
		if e.CreditAccount == account {
			row.PaidCents += e.AmountCents
		}
		if e.DebitAccount == account {
			row.ReceivedCents += e.AmountCents
		}
		row.EntryCount++
	}
	feeCurrencies := make([]string, 0, len(fees))
	for c := range fees {
		feeCurrencies = append(feeCurrencies, c)
	}
	sort.Strings(feeCurrencies)
	for _, c := range feeCurrencies {
		stmt.FeeSummary = append(stmt.FeeSummary, *fees[c])
	}

	// Event summary: freeze transitions in the window (chronological —
	// the history is appended in event order), reviews involving the
	// account created in the window, and the current risk-control state.
	summary := StatementEventSummary{
		FreezeEvents:    []AccountFreezeEvent{},
		TransferReviews: []StatementTransferReview{},
		ReviewCounts:    map[string]int{},
	}
	for _, ev := range l.freezeEvents {
		if ev.Account != account {
			continue
		}
		if ev.At.Before(from) || !ev.At.Before(to) {
			continue
		}
		summary.FreezeEvents = append(summary.FreezeEvents, ev)
	}
	for _, r := range l.reviews {
		if r.Transfer.From != account && r.Transfer.To != account {
			continue
		}
		if r.CreatedAt.Before(from) || !r.CreatedAt.Before(to) {
			continue
		}
		summary.TransferReviews = append(summary.TransferReviews, StatementTransferReview{
			TransferID:  r.TransferID,
			Status:      r.Status,
			AmountCents: r.Transfer.AmountCents,
			Currency:    r.Transfer.Currency,
			CreatedAt:   r.CreatedAt,
			ReviewedAt:  r.ReviewedAt,
			ReviewedBy:  r.ReviewedBy,
		})
		summary.ReviewCounts[r.Status]++
	}
	sort.Slice(summary.TransferReviews, func(i, j int) bool {
		if summary.TransferReviews[i].CreatedAt.Equal(summary.TransferReviews[j].CreatedAt) {
			return summary.TransferReviews[i].TransferID < summary.TransferReviews[j].TransferID
		}
		return summary.TransferReviews[i].CreatedAt.Before(summary.TransferReviews[j].CreatedAt)
	})
	summary.OverdraftProtected = l.noOverdraft[account]
	summary.CurrentlyFrozen = l.frozen[account]
	stmt.EventSummary = summary

	return stmt, nil
}
