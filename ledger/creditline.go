package ledger

import (
	"errors"
	"sort"
	"strings"
	"time"
)

// Credit lines: opt-in per-account overdraft floors with daily interest
// on utilization (LG-91, fintech).
//
// A credit line lets a protected account's balance go negative down to
// -limit in one currency. It is orthogonal to overdraft protection (see
// overdraft.go): protection without a line floors the account at zero
// with ErrAccountOverdraft; a line lowers the floor to -limit, and
// breaching the floor is rejected with ErrCreditLimitExceeded. The
// credit line never overrides the frozen gate — a frozen account rejects
// postings with ErrAccountFrozen whether or not it carries a line, and
// granting a line is structural config, not a money movement, so the
// frozen check does not apply to GrantCreditLine itself.
//
// Utilization is the negative part of the balance: used = max(0,
// -balance). Available credit is limit - used, saturated at zero.
// Interest accrues daily on utilization at floor(used * rateBps / 10000
// / 365) cents, following the accrual convention (see accrual.go): one
// journal row per (account, currency, UTC day) under the deterministic
// ID "credit-interest/<account>/<currency>/<YYYY-MM-DD>", so re-running a
// day's accrual replays the original row instead of double-charging.
// Interest is charged to the borrower: the interest-income account is
// the debit leg (grows, the payee is debited per the transfer
// convention), the borrower is the credit leg (shrinks — they owe more).
// The income account is fail-closed (see SetCreditInterestIncome): no
// account, no accrual.
//
// Credit lines are structural config, like daily limits and overdraft
// protection: granting, reducing, and revoking never bump the journal
// version, and they round-trip through disaster-recovery snapshots.

// creditLineKey identifies one credit line: an account carries at most
// one line per currency.
type creditLineKey struct {
	account  AccountID
	currency string
}

// CreditLine is one granted credit line: the account may carry a
// negative balance down to -LimitCents in Currency, at RateBps annual
// interest on utilization.
type CreditLine struct {
	Account    AccountID `json:"account"`
	Currency   string    `json:"currency"`
	LimitCents int64     `json:"limit_cents"`
	RateBps    int64     `json:"rate_bps"`
	GrantedAt  time.Time `json:"granted_at"`
}

// Validation and state errors returned by the credit-line APIs.
var (
	ErrEmptyCreditLineAccount = errors.New("ledger: credit line account must not be empty")
	ErrCreditLineNonPositiveLimit = errors.New("ledger: credit line limit must be greater than zero")
	// ErrCreditLineAccountSeparator is returned when the account ID
	// contains '/': the deterministic interest entry IDs join their
	// components with '/', so a '/' inside the account would let two
	// different lines share one interest row (see the accrual convention
	// in accrual.go).
	ErrCreditLineAccountSeparator = errors.New("ledger: credit line account ID must not contain '/'")
	// ErrInvalidCreditLineRate is returned when rateBps is outside
	// 1..10000: a rate above 100% per year is a configuration bug, not a
	// clampable rate (same bound as the accrual rate, see accrual.go).
	ErrInvalidCreditLineRate = errors.New("ledger: credit line rate must be between 1 and 10000 basis points")
	// ErrCreditLineExists is returned by GrantCreditLine when the
	// account already carries a line in the currency: granting is
	// create-only; change an existing line with ReduceCreditLine (or
	// revoke it by reducing to zero and re-grant).
	ErrCreditLineExists = errors.New("ledger: credit line already exists for this account and currency")
	// ErrCreditLineNotFound is returned for operations on a
	// (account, currency) pair with no granted line.
	ErrCreditLineNotFound = errors.New("ledger: no credit line for this account and currency")
	// ErrCreditLineBelowUtilization is returned by ReduceCreditLine
	// when the new limit would sit below the line's current utilization:
	// a line cannot be shrunk past money already drawn.
	ErrCreditLineBelowUtilization = errors.New("ledger: new credit line limit is below current utilization")
	// ErrCreditInterestIncomeNotConfigured is returned by
	// AccrueCreditInterest when no interest-income account is configured
	// (see SetCreditInterestIncome): fail-closed, so interest is never
	// booked against a zero account.
	ErrCreditInterestIncomeNotConfigured = errors.New("ledger: credit interest income account not configured")
)

// creditInterestEntryID builds the deterministic interest entry ID for
// one (account, currency, UTC day): the accrual convention (see
// accrual.go). The account ID cannot contain '/', so the namespace never
// collides across accounts.
func creditInterestEntryID(account AccountID, currency string, asof time.Time) string {
	return "credit-interest/" + string(account) + "/" + currency + "/" + asof.UTC().Format("2006-01-02")
}

// GrantCreditLine grants a credit line of limitCents in currency to
// account, charging rateBps annual interest on utilization. Granting is
// create-only: re-granting an existing (account, currency) line fails
// with ErrCreditLineExists — reduce it (or revoke by reducing to zero
// and re-grant) instead. A zero asof... (no asof here: GrantedAt is the
// current time.)
//
// A protected account (see overdraft.go) with a line may go negative
// down to -limitCents in the line's currency; utilization beyond that is
// rejected with ErrCreditLimitExceeded. An unprotected account ignores
// the line for posting purposes — the line only sets the floor for
// accounts under overdraft protection — but utilization, interest, and
// the Reconcile disclosure still track it. Granting never bumps the
// journal version and is audited as credit_line_granted.
func (l *Ledger) GrantCreditLine(account AccountID, limitCents int64, currency string, rateBps int64) (CreditLine, error) {
	if account == "" {
		return CreditLine{}, ErrEmptyCreditLineAccount
	}
	if strings.Contains(string(account), "/") {
		return CreditLine{}, ErrCreditLineAccountSeparator
	}
	if limitCents <= 0 {
		return CreditLine{}, ErrCreditLineNonPositiveLimit
	}
	cur, err := normalizeCurrency(currency)
	if err != nil {
		return CreditLine{}, err
	}
	if rateBps < 1 || rateBps > 10000 {
		return CreditLine{}, ErrInvalidCreditLineRate
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	k := creditLineKey{account: account, currency: cur}
	if _, exists := l.creditLines[k]; exists {
		return CreditLine{}, ErrCreditLineExists
	}
	cl := CreditLine{
		Account:    account,
		Currency:   cur,
		LimitCents: limitCents,
		RateBps:    rateBps,
		GrantedAt:  time.Now(),
	}
	l.creditLines[k] = cl
	l.emitAudit(AuditEvent{
		Op:            "credit_line_granted",
		Actor:         "GrantCreditLine",
		TraceID:       string(account) + "/" + cur,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{account},
		Details: map[string]any{
			"limit_cents": limitCents,
			"currency":    cur,
			"rate_bps":    rateBps,
		},
	})
	return cl, nil
}

// ReduceCreditLine lowers a credit line's limit to newLimitCents. The
// new limit must not sit below the line's current utilization —
// ErrCreditLineBelowUtilization — because money already drawn cannot be
// un-drawn by shrinking the line. Reducing to zero revokes the line: the
// row is deleted and the zero CreditLine is returned. The change is
// structural config (audited as credit_line_reduced /
// credit_line_revoked) and never bumps the journal version.
func (l *Ledger) ReduceCreditLine(account AccountID, currency string, newLimitCents int64) (CreditLine, error) {
	if account == "" {
		return CreditLine{}, ErrEmptyCreditLineAccount
	}
	if newLimitCents < 0 {
		return CreditLine{}, ErrCreditLineNonPositiveLimit
	}
	cur, err := normalizeCurrency(currency)
	if err != nil {
		return CreditLine{}, err
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	k := creditLineKey{account: account, currency: cur}
	cl, exists := l.creditLines[k]
	if !exists {
		return CreditLine{}, ErrCreditLineNotFound
	}
	used := l.creditLineUsedLocked(k)
	if newLimitCents < used {
		return CreditLine{}, ErrCreditLineBelowUtilization
	}
	if newLimitCents == 0 {
		delete(l.creditLines, k)
		l.emitAudit(AuditEvent{
			Op:            "credit_line_revoked",
			Actor:         "ReduceCreditLine",
			TraceID:       string(account) + "/" + cur,
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Accounts:      []AccountID{account},
			Details:       map[string]any{"currency": cur},
		})
		return CreditLine{}, nil
	}
	cl.LimitCents = newLimitCents
	l.creditLines[k] = cl
	l.emitAudit(AuditEvent{
		Op:            "credit_line_reduced",
		Actor:         "ReduceCreditLine",
		TraceID:       string(account) + "/" + cur,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{account},
		Details: map[string]any{
			"limit_cents": newLimitCents,
			"currency":    cur,
		},
	})
	return cl, nil
}

// GetCreditLine returns the credit line for (account, currency), or
// false when none is granted.
func (l *Ledger) GetCreditLine(account AccountID, currency string) (CreditLine, bool) {
	if currency == "" {
		currency = DefaultCurrency
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	cl, ok := l.creditLines[creditLineKey{account: account, currency: currency}]
	return cl, ok
}

// creditLineUsedLocked is the drawn amount of a credit line: the
// negative part of the account's balance in the line's currency,
// saturated at zero. Callers must hold l.mu; the read lock suffices.
func (l *Ledger) creditLineUsedLocked(k creditLineKey) int64 {
	if bal := l.balances[accountCurrency{account: k.account, currency: k.currency}]; bal < 0 {
		return -bal
	}
	return 0
}

// CreditLineUtilization reports a credit line's drawn amount, remaining
// availability, and limit, all in cents. used is the negative part of
// the account's balance (zero when the balance is non-negative);
// available is limit - used, saturated at zero. ok is false when no
// line is granted for (account, currency); an empty currency means the
// default currency.
func (l *Ledger) CreditLineUtilization(account AccountID, currency string) (used, available, limit int64, ok bool) {
	if currency == "" {
		currency = DefaultCurrency
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	cl, exists := l.creditLines[creditLineKey{account: account, currency: currency}]
	if !exists {
		return 0, 0, 0, false
	}
	used = l.creditLineUsedLocked(creditLineKey{account: account, currency: currency})
	available = cl.LimitCents - used
	if available < 0 {
		available = 0
	}
	return used, available, cl.LimitCents, true
}

// CreditLines lists every granted credit line, sorted by
// (account, currency): the read side of the registry, backing the
// Reconcile disclosure.
func (l *Ledger) CreditLines() []CreditLine {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]CreditLine, 0, len(l.creditLines))
	for _, cl := range l.creditLines {
		out = append(out, cl)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Account != out[j].Account {
			return out[i].Account < out[j].Account
		}
		return out[i].Currency < out[j].Currency
	})
	return out
}

// AccrueCreditInterest books one day's interest on a credit line's
// utilization: debit the configured interest-income account (the payee
// is debited, per the transfer convention), credit the borrower, for
// floor(used * rateBps / 10000 / 365) cents — the same overflow-safe
// basis-point math the transfer fee policy uses (see policyFeeCents),
// divided by the days in a year, like AccrueInterest in accrual.go. asof
// sets both the entry's timestamp and its calendar day (UTC); a zero
// asof means now.
//
// The call is idempotent per (account, currency, calendar day): the
// entry ID is the deterministic
// "credit-interest/<account>/<currency>/<YYYY-MM-DD>", so re-running a
// day's accrual returns the original entry with duplicate == true and
// books nothing new. A dust accrual (zero cents after flooring, or zero
// utilization) books nothing and returns ErrNonPositiveAmount, like
// AccrueInterest.
//
// The entry runs the full Post gate order but posts through the system
// path: it never fires the journal rules engine, never enters
// dual-control review, and the dormant-account stop does not apply (see
// the accrual convention in accrual.go). Note the overdraft interplay:
// the interest charge itself is subject to the credit line's floor —
// interest that would breach -limit is rejected with
// ErrCreditLimitExceeded instead of silently pushing the account past
// its line.
func (l *Ledger) AccrueCreditInterest(account AccountID, currency string, asof time.Time) (JournalEntry, bool, error) {
	if account == "" {
		return JournalEntry{}, false, ErrEmptyCreditLineAccount
	}
	cur, err := normalizeCurrency(currency)
	if err != nil {
		return JournalEntry{}, false, err
	}
	if asof.IsZero() {
		asof = time.Now()
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	k := creditLineKey{account: account, currency: cur}
	cl, exists := l.creditLines[k]
	if !exists {
		return JournalEntry{}, false, ErrCreditLineNotFound
	}
	if l.creditInterestIncome == "" {
		return JournalEntry{}, false, ErrCreditInterestIncomeNotConfigured
	}
	used := l.creditLineUsedLocked(k)
	if used <= 0 {
		return JournalEntry{}, false, ErrNonPositiveAmount
	}
	// Overflow-safe floor(used * rateBps / 10000 / 365): the same
	// basis-point helper the transfer fee policy uses, divided by the
	// days in a year (see AccrueInterest in accrual.go).
	amount := policyFeeCents(used, cl.RateBps) / 365
	if amount <= 0 {
		return JournalEntry{}, false, ErrNonPositiveAmount
	}
	entryID := creditInterestEntryID(account, cur, asof)
	e := JournalEntry{
		ID:             entryID,
		DebitAccount:   l.creditInterestIncome,
		CreditAccount:  account,
		AmountCents:    amount,
		Currency:       cur,
		IdempotencyKey: entryID,
		Memo:           "credit-interest:" + string(account),
		CreatedAt:      asof,
	}
	return l.postSystemLocked(e, false)
}

// creditLineReportsLocked builds the Reconcile credit-line disclosure:
// every granted line, sorted by (account, currency), with the drawn
// utilization and remaining availability. Callers must hold l.mu; the
// read lock suffices.
func (l *Ledger) creditLineReportsLocked() []CreditLineReport {
	out := make([]CreditLineReport, 0, len(l.creditLines))
	for k, cl := range l.creditLines {
		used := l.creditLineUsedLocked(k)
		available := cl.LimitCents - used
		if available < 0 {
			available = 0
		}
		out = append(out, CreditLineReport{
			Account:        cl.Account,
			Currency:       cl.Currency,
			LimitCents:     cl.LimitCents,
			UsedCents:      used,
			AvailableCents: available,
			RateBps:        cl.RateBps,
			GrantedAt:      cl.GrantedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Account != out[j].Account {
			return out[i].Account < out[j].Account
		}
		return out[i].Currency < out[j].Currency
	})
	return out
}

// creditLinesLocked is the sorted credit-line listing used by the
// snapshot config export. Callers must hold l.mu; the read lock
// suffices.
func (l *Ledger) creditLinesLocked() []CreditLine {
	out := make([]CreditLine, 0, len(l.creditLines))
	for _, cl := range l.creditLines {
		out = append(out, cl)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Account != out[j].Account {
			return out[i].Account < out[j].Account
		}
		return out[i].Currency < out[j].Currency
	})
	return out
}

// WithCreditInterestIncome configures the ledger-wide interest-income
// account that credit-line interest accruals debit (see
// AccrueCreditInterest).
func WithCreditInterestIncome(a AccountID) Option {
	return func(l *Ledger) {
		l.creditInterestIncome = a
	}
}

// SetCreditInterestIncome configures (or clears, with the empty account)
// the ledger-wide interest-income account that credit-line interest
// accruals debit. Empty means unconfigured: AccrueCreditInterest is
// fail-closed until the operator configures it. Structural config, like
// feeRevenueAccount — it does not bump the journal version and it
// survives disaster-recovery snapshots.
func (l *Ledger) SetCreditInterestIncome(a AccountID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.creditInterestIncome = a
	l.emitAudit(AuditEvent{
		Op:      "set_credit_interest_income",
		Actor:   "SetCreditInterestIncome",
		TraceID: string(a),
		Details: map[string]any{"interest_income_account": string(a)},
	})
}
