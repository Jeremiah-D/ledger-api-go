package ledger

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Term deposits: time-bound principal locks with daily interest accrual
// (LG-90, fintech).
//
// A term deposit locks part of an account's available funds until its
// maturity date, like an authorization hold (see hold.go) with a fixed
// tenor. While a deposit is active, its principal counts as reserved in
// AvailableIn, exactly like a hold — the money has not moved through the
// journal, it is just not spendable. At maturity, MatureTermDeposit
// releases the lock; the depositor can then spend the principal. Interest
// is booked day by day by AccrueTermInterest, following the accrual
// convention (see accrual.go): one journal row per (deposit, UTC day)
// under the deterministic ID "term-interest/<id>/<YYYY-MM-DD>", so
// re-running a day's accrual replays the original row instead of
// double-booking.
//
// Term-deposit lifecycle: active -> matured | withdrawn_early. Only an
// active deposit locks funds and accrues interest. Matured and
// early-withdrawn deposits are terminal.
//
// Interest economics: interest is a payment to the depositor, so the
// depositor is the debit leg (balance grows, per the transfer convention
// — the payee is debited) and the ledger's configured interest-expense
// account (see SetTermDepositInterestExpense) is the credit leg. The
// expense account is fail-closed: accruing without one configured is
// rejected with ErrTermDepositInterestAccountNotConfigured instead of
// silently booking against a zero account. Like the other system
// adjusting entries (accruals, dividends), interest rows post through
// the system path: they run the full Post gate order but never fire the
// journal rules engine and never enter dual-control review, and the
// dormant-account stop does not apply. Under a registered chart of
// accounts (see chart.go) the credit leg on an Expense-typed account is
// direction-mismatched — route through a dedicated contra account, like
// the accrual-reversal note in accrual.go.
//
// Early withdrawal (WithdrawTermDeposit) releases the principal before
// maturity and books a penalty leg — debit the ledger's fee-revenue
// account (see feeRevenueAccount), credit the depositor — of
// floor(principal * penaltyBps / 10000) cents, when penaltyBps > 0. The
// fee-revenue account is fail-closed too: a positive penalty without a
// configured account is rejected rather than silently dropped. Interest
// already journaled is never clawed back: an early withdrawal keeps the
// interest rows booked so far, and the penalty is the contract's only
// cost. This is documented rather than hidden, because a clawback would
// rewrite history the audit chain already sealed.
//
// Interest accrues through the maturity instant: AccrueTermInterest
// accepts asof <= MaturesAt and rejects anything later with
// ErrTermDepositMatured. A deposit whose principal was never moved
// accrues nothing past its last booked day — the deterministic IDs make
// accrual idempotent per day, so a daily sweeper (see SweepTermDeposits)
// can run blindly without double-booking.
//
// Idempotency: deposits carry their own key namespace (termDepositKeys),
// independent of Post's byKey and every other operation's namespace,
// expiring with the ledger's idempotency TTL like the others. Opening
// with a replayed key returns the original deposit. Maturing and
// early-withdrawing are idempotent state transitions: re-running them on
// an already-terminal deposit returns the deposit as-is.

// Term-deposit statuses. Only active deposits lock funds and accrue
// interest; the other two are terminal.
const (
	TermDepositActive         = "active"
	TermDepositMatured        = "matured"
	TermDepositWithdrawnEarly = "withdrawn_early"
)

// Validation and state errors returned by the term-deposit APIs.
var (
	ErrEmptyTermDepositID = errors.New("ledger: term deposit ID must not be empty")
	ErrEmptyTermDepositAccount = errors.New("ledger: term deposit account must not be empty")
	ErrTermDepositNonPositiveAmount = errors.New("ledger: term deposit amount must be greater than zero")
	// ErrTermDepositIDSeparator is returned when the deposit ID contains
	// '/': the deterministic interest entry IDs join their components
	// with '/', so a '/' inside the ID would let two different deposits
	// share one interest row (see the accrual convention in accrual.go).
	ErrTermDepositIDSeparator = errors.New("ledger: term deposit ID must not contain '/'")
	// ErrInvalidTermDepositRate is returned when rateBps is outside
	// 1..10000: a rate above 100% per year is a configuration bug, not a
	// clampable rate (same bound as the accrual rate, see accrual.go).
	ErrInvalidTermDepositRate = errors.New("ledger: term deposit rate must be between 1 and 10000 basis points")
	// ErrInvalidTermDays is returned when termDays is outside 1..36500:
	// a non-positive tenor is not a deposit, and more than a century is
	// a data error, not a booking.
	ErrInvalidTermDays = errors.New("ledger: term deposit tenor must be between 1 and 36500 days")
	// ErrInvalidTermDepositPenalty is returned when the early-withdrawal
	// penalty rate is outside 0..10000 basis points.
	ErrInvalidTermDepositPenalty = errors.New("ledger: term deposit penalty must be between 0 and 10000 basis points")
	// ErrTermDepositIDConflict is returned when the deposit ID is
	// already used by another term deposit: deposits are never deleted,
	// so a colliding ID is always a caller bug, never a replay.
	ErrTermDepositIDConflict = errors.New("ledger: term deposit ID already in use")
	// ErrTermDepositNotFound is returned for an unknown deposit ID.
	ErrTermDepositNotFound = errors.New("ledger: term deposit not found")
	// ErrTermDepositNotActive is returned when an operation needs an
	// active deposit but finds a terminal one (matured or withdrawn
	// early). Maturing a matured deposit is a no-op success instead —
	// see MatureTermDeposit.
	ErrTermDepositNotActive = errors.New("ledger: term deposit is not active")
	// ErrTermNotMatured is returned by MatureTermDeposit when the
	// deposit's maturity date has not arrived yet.
	ErrTermNotMatured = errors.New("ledger: term deposit has not matured yet")
	// ErrTermDepositMatured is returned by interest accrual and early
	// withdrawal once the deposit has matured: accrued interest stops
	// at the maturity instant, and a matured deposit is released with
	// MatureTermDeposit, never early-withdrawn.
	ErrTermDepositMatured = errors.New("ledger: term deposit already matured")
	// ErrTermDepositInterestAccountNotConfigured is returned by
	// AccrueTermInterest when no interest-expense account is configured
	// (see SetTermDepositInterestExpense): fail-closed, so interest is
	// never booked against a zero account.
	ErrTermDepositInterestAccountNotConfigured = errors.New("ledger: term deposit interest expense account not configured")
	// ErrTermDepositPenaltyAccountNotConfigured is returned by
	// WithdrawTermDeposit when a positive penalty is due but no fee
	// revenue account is configured: fail-closed, so a penalty is never
	// silently dropped.
	ErrTermDepositPenaltyAccountNotConfigured = errors.New("ledger: fee revenue account not configured for term deposit penalty")
)

// TermDeposit is one time-bound principal lock on an account.
//
// Status is one of active/matured/withdrawn_early. MaturesAt is
// exclusive for interest accrual (accrual accepts asof <= MaturesAt)
// and inclusive for maturity (MatureTermDeposit accepts now >=
// MaturesAt). Only active deposits count toward AvailableIn.
type TermDeposit struct {
	ID             string    `json:"id"`
	Account        AccountID `json:"account"`
	AmountCents    int64     `json:"amount_cents"`
	Currency       string    `json:"currency"`
	RateBps        int64     `json:"rate_bps"`
	TermDays       int64     `json:"term_days"`
	OpenedAt       time.Time `json:"opened_at"`
	MaturesAt      time.Time `json:"matures_at"`
	// BusinessDayMaturity opts into business-day settlement (LG-95): when
	// true and MaturesAt lands on a non-business day, OpenTermDeposit
	// moves it to the next business day. The stored MaturesAt is the
	// contractual date — MatureTermDeposit needs no special case.
	BusinessDayMaturity bool      `json:"business_day_maturity,omitempty"`
	IdempotencyKey      string    `json:"idempotency_key,omitempty"`
	Status              string    `json:"status"`
}

// TermDepositWithdrawal is the receipt of an early withdrawal: the
// terminal deposit plus the penalty that was booked, if any.
type TermDepositWithdrawal struct {
	Deposit       TermDeposit `json:"deposit"`
	PenaltyCents  int64       `json:"penalty_cents"`
	PenaltyEntry  string      `json:"penalty_entry,omitempty"`
	Duplicate     bool        `json:"duplicate"`
}

// termDepositInterestEntryID builds the deterministic interest entry ID
// for one (deposit, UTC day): "term-interest/<id>/<YYYY-MM-DD>", the
// accrual convention (see accrual.go). The deposit ID cannot contain '/',
// so the namespace never collides across deposits.
func termDepositInterestEntryID(id string, asof time.Time) string {
	return "term-interest/" + id + "/" + asof.UTC().Format("2006-01-02")
}

// termDepositInterestPrefix is the ID prefix shared by every interest
// row of one deposit, used to total accrued interest from the journal.
func termDepositInterestPrefix(id string) string {
	return "term-interest/" + id + "/"
}

// termDepositPenaltyEntryID builds the deterministic penalty entry ID:
// "<deposit ID>/penalty". One deposit admits at most one early
// withdrawal, so at most one penalty row exists.
func termDepositPenaltyEntryID(id string) string {
	return id + "/penalty"
}

// OpenTermDeposit locks AmountCents of the account's available funds
// until OpenedAt + TermDays days.
//
// Validation runs first (non-empty ID without '/', non-empty account,
// positive amount, valid currency, 1..36500 term days, 1..10000 rate
// bps), then the idempotency replay check on the term-deposit namespace
// (a replay returns the original deposit with duplicate == true and
// locks nothing new), then the deposit-ID conflict check, then the
// frozen and closed checks (a frozen account rejects the open with
// ErrAccountFrozen after the replay check, like Post — replaying a key
// opened before the freeze returns the original), then the
// available-funds check: the open is rejected with
// ErrInsufficientAvailableFunds when balance - active holds - pending
// review reservations - other active term deposits cannot cover the
// amount. The comparison is overflow-safe (see coversCents).
//
// A zero OpenedAt is filled with the current time; MaturesAt is derived
// as OpenedAt + TermDays*24h. The commit is atomic under the write lock:
// the deposit row and its idempotency index entry land together. Opens
// bump neither the ledger version nor the audit chain — the principal is
// off-journal, like an authorization hold; only interest accruals and
// penalties journal real postings.
func (l *Ledger) OpenTermDeposit(t TermDeposit) (TermDeposit, bool, error) {
	if t.ID == "" {
		return TermDeposit{}, false, ErrEmptyTermDepositID
	}
	if strings.Contains(t.ID, "/") {
		return TermDeposit{}, false, ErrTermDepositIDSeparator
	}
	if t.Account == "" {
		return TermDeposit{}, false, ErrEmptyTermDepositAccount
	}
	if t.AmountCents <= 0 {
		return TermDeposit{}, false, ErrTermDepositNonPositiveAmount
	}
	currency, err := normalizeCurrency(t.Currency)
	if err != nil {
		return TermDeposit{}, false, err
	}
	t.Currency = currency
	if t.TermDays < 1 || t.TermDays > 36500 {
		return TermDeposit{}, false, ErrInvalidTermDays
	}
	if t.RateBps < 1 || t.RateBps > 10000 {
		return TermDeposit{}, false, ErrInvalidTermDepositRate
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if t.IdempotencyKey != "" {
		if id, ok := l.termDepositKeys[t.IdempotencyKey]; ok {
			return l.termDeposits[id], true, nil
		}
	}
	if _, exists := l.termDeposits[t.ID]; exists {
		return TermDeposit{}, false, ErrTermDepositIDConflict
	}

	// A frozen account cannot take on new locks. The check runs after
	// the replay check for the same reason as in Post: a replay locks
	// nothing new, so it must not fail on a freeze that landed after
	// the original open. Expired timed freezes release lazily before
	// the frozen gate, like in Post (see FreezeUntil).
	now := time.Now()
	l.releaseExpiredFreezesLocked(now, "write_path")
	if l.closedLocked(t.Account) {
		return TermDeposit{}, false, ErrAccountClosed
	}
	if l.frozenLocked(t.Account) {
		return TermDeposit{}, false, ErrAccountFrozen
	}
	if l.suspendedLocked(t.Account) {
		return TermDeposit{}, false, ErrAccountSuspended
	}

	// The principal locks available funds: balance - active holds -
	// pending-review reservations - other active term deposits must
	// cover the amount.
	key := accountCurrency{account: t.Account, currency: t.Currency}
	reserved := l.heldLocked(t.Account, t.Currency, now) +
		l.reviewHeldLocked(t.Account, t.Currency, now) +
		l.termLockedLocked(t.Account, t.Currency, now)
	if !coversCents(l.balances[key], reserved, t.AmountCents) {
		return TermDeposit{}, false, ErrInsufficientAvailableFunds
	}

	if t.OpenedAt.IsZero() {
		t.OpenedAt = now
	}
	t.MaturesAt = t.OpenedAt.Add(time.Duration(t.TermDays) * 24 * time.Hour)
	// LG-95: with business-day settlement, a maturity landing on a
	// non-business day settles on the next business day. The adjusted
	// date is stored as the contractual MaturesAt, so maturity checks,
	// interest accrual, and snapshots all see one consistent date.
	if t.BusinessDayMaturity && !l.businessCalendar.IsBusinessDay(t.MaturesAt) {
		t.MaturesAt = l.businessCalendar.NextBusinessDay(t.MaturesAt)
	}
	t.Status = TermDepositActive

	l.maybePruneIdempotencyKeys(now)
	l.termDeposits[t.ID] = t
	if t.IdempotencyKey != "" {
		l.termDepositKeys[t.IdempotencyKey] = t.ID
	}
	l.emitAudit(AuditEvent{
		Op:            "term_deposit_open",
		Actor:         "OpenTermDeposit",
		TraceID:       t.ID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{t.Account},
		Details: map[string]any{
			"amount_cents": t.AmountCents,
			"currency":     t.Currency,
			"rate_bps":     t.RateBps,
			"term_days":    t.TermDays,
			"matures_at":   t.MaturesAt.UTC().Format(time.RFC3339),
		},
	})
	l.emitOutboxLocked(OutboxEvent{
		Type:        OutboxTermDepositOpened,
		Account:     t.Account,
		AmountCents: t.AmountCents,
		Currency:    t.Currency,
		Details: map[string]any{
			"term_deposit_id": t.ID,
			"rate_bps":        t.RateBps,
			"term_days":       t.TermDays,
			"matures_at":      t.MaturesAt.UTC().Format(time.RFC3339),
		},
	})
	return t, false, nil
}

// termLockedLocked sums the active term-deposit principals on (account,
// currency). Only active deposits lock funds — matured and
// early-withdrawn deposits have released their principal. Callers must
// hold l.mu; the read lock suffices because nothing is mutated.
func (l *Ledger) termLockedLocked(a AccountID, currency string, now time.Time) int64 {
	var total int64
	for _, td := range l.termDeposits {
		if td.Account == a && td.Currency == currency && td.Status == TermDepositActive {
			var ok bool
			total, ok = addCents(total, td.AmountCents)
			if !ok {
				// Defensive: the open path's coversCents check already
				// bounds every principal against the int64 balance, so
				// a sum that overflows means corrupted state — saturate
				// rather than wrap, so Available never reports phantom
				// funds.
				return int64(^uint64(0) >> 1)
			}
		}
	}
	_ = now
	return total
}

// GetTermDeposit returns the deposit with the given ID, or false when no
// such deposit exists.
func (l *Ledger) GetTermDeposit(id string) (TermDeposit, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	td, ok := l.termDeposits[id]
	return td, ok
}

// TermDeposits lists every term deposit, sorted by ID: the read side of
// the deposit registry, backing the Reconcile disclosure.
func (l *Ledger) TermDeposits() []TermDeposit {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]TermDeposit, 0, len(l.termDeposits))
	for _, td := range l.termDeposits {
		out = append(out, td)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// AccrueTermInterest books one day's interest on an active term
// deposit: debit the depositor (their balance grows — the payee is
// debited, per the transfer convention), credit the configured
// interest-expense account, for floor(principal * rateBps / 10000 / 365)
// cents — the same overflow-safe basis-point math the transfer fee
// policy uses (see policyFeeCents), divided by the days in a year, like
// AccrueInterest in accrual.go. asof sets both the entry's timestamp
// and its calendar day (UTC); a zero asof means now.
//
// The call is idempotent per (deposit, calendar day): the entry ID is
// the deterministic "term-interest/<id>/<YYYY-MM-DD>", so re-running a
// day's accrual returns the original entry with duplicate == true and
// books nothing new. Interest accrues only while the deposit is active
// and only through the maturity instant: an asof past MaturesAt is
// rejected with ErrTermDepositMatured, and a terminal deposit with
// ErrTermDepositNotActive. A dust accrual (zero cents after flooring)
// books nothing and returns ErrNonPositiveAmount, like AccrueInterest.
//
// The entry runs the full Post gate order but posts through the system
// path: it never fires the journal rules engine, never enters
// dual-control review, and the dormant-account stop does not apply (see
// the package doc comment above for why).
func (l *Ledger) AccrueTermInterest(id string, asof time.Time) (JournalEntry, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	td, ok := l.termDeposits[id]
	if !ok {
		return JournalEntry{}, false, ErrTermDepositNotFound
	}
	if td.Status != TermDepositActive {
		return JournalEntry{}, false, ErrTermDepositNotActive
	}
	if asof.IsZero() {
		asof = time.Now()
	}
	// LG-95: with business-day-only accruals, term-deposit interest
	// follows the same rule as AccrueInterest — no booking on
	// non-business days.
	if err := l.accrualBusinessDayGateLocked(asof); err != nil {
		return JournalEntry{}, false, err
	}
	if asof.After(td.MaturesAt) {
		return JournalEntry{}, false, ErrTermDepositMatured
	}
	if l.tdInterestExpense == "" {
		return JournalEntry{}, false, ErrTermDepositInterestAccountNotConfigured
	}

	// Overflow-safe floor(principal * rateBps / 10000 / 365): the same
	// basis-point helper the transfer fee policy uses, divided by the
	// days in a year (see AccrueInterest in accrual.go).
	amount := policyFeeCents(td.AmountCents, td.RateBps) / 365
	if amount <= 0 {
		return JournalEntry{}, false, ErrNonPositiveAmount
	}
	entryID := termDepositInterestEntryID(id, asof)
	e := JournalEntry{
		ID:             entryID,
		DebitAccount:   td.Account,
		CreditAccount:  l.tdInterestExpense,
		AmountCents:    amount,
		Currency:       td.Currency,
		IdempotencyKey: entryID,
		Memo:           "term-interest:" + id,
		CreatedAt:      asof,
	}
	return l.postSystemLocked(e, false)
}

// TermDepositInterestAccrued totals the interest journaled for one
// deposit: the sum of every "term-interest/<id>/" row. It is the books'
// own record, not the deposit row's — the journal is the single source
// of truth, like dividendsLocked. Unknown deposits report zero.
func (l *Ledger) TermDepositInterestAccrued(id string) int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.termDepositInterestAccruedLocked(id)
}

// termDepositInterestAccruedLocked is TermDepositInterestAccrued's body.
// Callers must hold l.mu; the read lock suffices.
func (l *Ledger) termDepositInterestAccruedLocked(id string) int64 {
	prefix := termDepositInterestPrefix(id)
	var total int64
	for entryID, e := range l.entries {
		if !strings.HasPrefix(entryID, prefix) {
			continue
		}
		var ok bool
		total, ok = addCents(total, e.AmountCents)
		if !ok {
			return int64(^uint64(0) >> 1)
		}
	}
	return total
}

// MatureTermDeposit releases an active deposit's principal back to
// available funds once its maturity date has arrived (now >= MaturesAt).
// Interest already journaled stays journaled; the principal lock simply
// lifts — the money was never moved through the journal, so nothing
// needs to move back.
//
// Maturing is idempotent: re-running it on an already-matured deposit
// returns the deposit as-is. Maturing before maturity is rejected with
// ErrTermNotMatured; maturing a withdrawn-early deposit with
// ErrTermDepositNotActive. The version is untouched (no journal row),
// and the transition is audited.
func (l *Ledger) MatureTermDeposit(id string, now time.Time) (TermDeposit, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	td, ok := l.termDeposits[id]
	if !ok {
		return TermDeposit{}, ErrTermDepositNotFound
	}
	if td.Status == TermDepositMatured {
		return td, nil
	}
	if td.Status != TermDepositActive {
		return TermDeposit{}, ErrTermDepositNotActive
	}
	if now.Before(td.MaturesAt) {
		return TermDeposit{}, ErrTermNotMatured
	}
	td.Status = TermDepositMatured
	l.termDeposits[id] = td
	l.emitAudit(AuditEvent{
		Op:            "term_deposit_mature",
		Actor:         "MatureTermDeposit",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{td.Account},
		Details: map[string]any{
			"amount_cents":           td.AmountCents,
			"currency":               td.Currency,
			"accrued_interest_cents": l.termDepositInterestAccruedLocked(id),
		},
	})
	l.emitOutboxLocked(OutboxEvent{
		Type:        OutboxTermDepositMatured,
		Account:     td.Account,
		AmountCents: td.AmountCents,
		Currency:    td.Currency,
		Details:     map[string]any{"term_deposit_id": id},
	})
	return td, nil
}

// WithdrawTermDeposit releases an active deposit's principal before
// maturity, booking the early-withdrawal penalty when penaltyBps > 0:
// debit the ledger's fee-revenue account (the payee is debited, per the
// transfer convention), credit the depositor, for floor(principal *
// penaltyBps / 10000) cents under the deterministic entry ID
// "<deposit ID>/penalty". A zero penalty rate, or a rate that floors to
// zero cents, books no penalty leg — the withdrawal still releases the
// principal. A positive penalty without a configured fee-revenue account
// is rejected with ErrTermDepositPenaltyAccountNotConfigured: fail-closed,
// so the penalty is never silently dropped.
//
// Interest already journaled is not clawed back: the audit chain sealed
// those rows, and the penalty is the contract's only cost — see the
// package doc comment. Withdrawing a matured deposit is rejected with
// ErrTermDepositMatured (mature it instead); withdrawing a
// withdrawn-early deposit returns the original receipt with Duplicate ==
// true.
func (l *Ledger) WithdrawTermDeposit(id string, penaltyBps int64, now time.Time) (TermDepositWithdrawal, error) {
	if penaltyBps < 0 || penaltyBps > 10000 {
		return TermDepositWithdrawal{}, ErrInvalidTermDepositPenalty
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	td, ok := l.termDeposits[id]
	if !ok {
		return TermDepositWithdrawal{}, ErrTermDepositNotFound
	}
	if td.Status == TermDepositWithdrawnEarly {
		return TermDepositWithdrawal{Deposit: td, Duplicate: true}, nil
	}
	if td.Status == TermDepositMatured {
		return TermDepositWithdrawal{}, ErrTermDepositMatured
	}

	penalty := policyFeeCents(td.AmountCents, penaltyBps)
	var penaltyEntry string
	if penalty > 0 {
		if l.feeRevenueAccount == "" {
			return TermDepositWithdrawal{}, ErrTermDepositPenaltyAccountNotConfigured
		}
		penaltyEntry = termDepositPenaltyEntryID(id)
		e := JournalEntry{
			ID:             penaltyEntry,
			DebitAccount:   l.feeRevenueAccount,
			CreditAccount:  td.Account,
			AmountCents:    penalty,
			Currency:       td.Currency,
			IdempotencyKey: penaltyEntry,
			Memo:           "term-deposit-penalty:" + id,
			CreatedAt:      now,
		}
		// fireRules=false, system path: the penalty is a system
		// adjusting entry like the interest rows, never an operator
		// posting. The deterministic ID makes a retried withdrawal
		// replay the original penalty row instead of double-charging.
		if _, _, err := l.postSystemLocked(e, false); err != nil {
			return TermDepositWithdrawal{}, fmt.Errorf("ledger: term deposit %q penalty: %w", id, err)
		}
	}

	td.Status = TermDepositWithdrawnEarly
	l.termDeposits[id] = td
	l.emitAudit(AuditEvent{
		Op:            "term_deposit_withdraw",
		Actor:         "WithdrawTermDeposit",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{td.Account},
		Details: map[string]any{
			"amount_cents":           td.AmountCents,
			"currency":               td.Currency,
			"penalty_bps":            penaltyBps,
			"penalty_cents":          penalty,
			"accrued_interest_cents": l.termDepositInterestAccruedLocked(id),
		},
	})
	l.emitOutboxLocked(OutboxEvent{
		Type:        OutboxTermDepositWithdrawn,
		Account:     td.Account,
		AmountCents: td.AmountCents,
		Currency:    td.Currency,
		Details: map[string]any{
			"term_deposit_id": id,
			"penalty_bps":     penaltyBps,
			"penalty_cents":   penalty,
		},
	})
	return TermDepositWithdrawal{
		Deposit:      td,
		PenaltyCents: penalty,
		PenaltyEntry: penaltyEntry,
	}, nil
}

// SweepTermDeposits is the operator's daily batch: for every active term
// deposit, it accrues interest through now's UTC day and matures every
// deposit whose maturity has arrived. Accrual is best-effort per deposit
// — a deposit that cannot accrue (dust amount, unconfigured expense
// account) is skipped, never aborts the sweep — because the deterministic
// interest IDs make accrual idempotent: the next sweep retries exactly
// the days that never booked. It returns how many deposits matured and
// how many interest rows were newly booked (replays are not counted).
func (l *Ledger) SweepTermDeposits(now time.Time) (matured, accrued int) {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Collect IDs first: maturing and accruing mutate the map, and the
	// audit below wants the counts anyway.
	ids := make([]string, 0, len(l.termDeposits))
	for id, td := range l.termDeposits {
		if td.Status == TermDepositActive {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		td := l.termDeposits[id]
		if !now.Before(td.MaturesAt) {
			td.Status = TermDepositMatured
			l.termDeposits[id] = td
			matured++
			l.emitAudit(AuditEvent{
				Op:            "term_deposit_mature",
				Actor:         "SweepTermDeposits",
				TraceID:       id,
				VersionBefore: l.version,
				VersionAfter:  l.version,
				Accounts:      []AccountID{td.Account},
				Details: map[string]any{
					"amount_cents":           td.AmountCents,
					"currency":               td.Currency,
					"accrued_interest_cents": l.termDepositInterestAccruedLocked(id),
				},
			})
			l.emitOutboxLocked(OutboxEvent{
				Type:        OutboxTermDepositMatured,
				Account:     td.Account,
				AmountCents: td.AmountCents,
				Currency:    td.Currency,
				Details:     map[string]any{"term_deposit_id": id},
			})
			continue
		}
		// Best-effort: a deposit that cannot accrue today (dust,
		// unconfigured expense account) is skipped, and the next sweep
		// retries it — the deterministic ID means no day ever books
		// twice.
		if l.tdInterestExpense == "" {
			continue
		}
		amount := policyFeeCents(td.AmountCents, td.RateBps) / 365
		if amount <= 0 {
			continue
		}
		entryID := termDepositInterestEntryID(id, now)
		if _, exists := l.entries[entryID]; exists {
			continue
		}
		e := JournalEntry{
			ID:             entryID,
			DebitAccount:   td.Account,
			CreditAccount:  l.tdInterestExpense,
			AmountCents:    amount,
			Currency:       td.Currency,
			IdempotencyKey: entryID,
			Memo:           "term-interest:" + id,
			CreatedAt:      now,
		}
		if _, _, err := l.postSystemLocked(e, false); err != nil {
			continue
		}
		accrued++
	}
	if matured > 0 || accrued > 0 {
		l.emitAudit(AuditEvent{
			Op:            "term_deposit_sweep",
			Actor:         "SweepTermDeposits",
			TraceID:       fmt.Sprintf("term-deposit-sweep@%d", l.version),
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Details: map[string]any{
				"matured": matured,
				"accrued": accrued,
			},
		})
	}
	return matured, accrued
}

// termDepositReportsLocked builds the Reconcile term-deposit
// disclosure: every deposit, sorted by ID, with the interest the journal
// has booked for it (recovered from the deterministic
// "term-interest/<id>/" rows, like the dividend disclosure). Callers
// must hold l.mu; the read lock suffices.
func (l *Ledger) termDepositReportsLocked() []TermDepositReport {
	out := make([]TermDepositReport, 0, len(l.termDeposits))
	for id, td := range l.termDeposits {
		out = append(out, TermDepositReport{
			ID:                   id,
			Account:              td.Account,
			PrincipalCents:       td.AmountCents,
			Currency:             td.Currency,
			RateBps:              td.RateBps,
			TermDays:             td.TermDays,
			Status:               td.Status,
			OpenedAt:             td.OpenedAt,
			MaturesAt:            td.MaturesAt,
			AccruedInterestCents: l.termDepositInterestAccruedLocked(id),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// WithTermDepositInterestExpense configures the ledger-wide
// interest-expense account that term-deposit interest accruals credit
// (see AccrueTermInterest).
func WithTermDepositInterestExpense(a AccountID) Option {
	return func(l *Ledger) {
		l.tdInterestExpense = a
	}
}

// SetTermDepositInterestExpense configures (or clears, with the empty
// account) the ledger-wide interest-expense account that term-deposit
// interest accruals credit. Empty means unconfigured: AccrueTermInterest
// is fail-closed until the operator configures it. Structural config,
// like feeRevenueAccount — it does not bump the journal version and it
// survives disaster-recovery snapshots.
func (l *Ledger) SetTermDepositInterestExpense(a AccountID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tdInterestExpense = a
	l.emitAudit(AuditEvent{
		Op:      "set_term_deposit_interest_expense",
		Actor:   "SetTermDepositInterestExpense",
		TraceID: string(a),
		Details: map[string]any{"interest_expense_account": string(a)},
	})
}

// pruneTermDepositKeysLocked evicts term-deposit idempotency keys older
// than the TTL, mirroring the entry-level key expiry: after the TTL,
// reposting a key books a brand-new deposit. The deposits themselves are
// untouched — only the replay-detection index shrinks. A non-positive TTL
// disables pruning. Callers must hold l.mu (the write lock; the sweep
// mutates the index).
func (l *Ledger) pruneTermDepositKeysLocked(now time.Time) int {
	if l.idempotencyTTL <= 0 {
		return 0
	}
	cutoff := now.Add(-l.idempotencyTTL)
	removed := 0
	for key, id := range l.termDepositKeys {
		if td := l.termDeposits[id]; td.OpenedAt.Before(cutoff) {
			delete(l.termDepositKeys, key)
			removed++
		}
	}
	return removed
}
