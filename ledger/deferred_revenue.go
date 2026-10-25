package ledger

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// Deferred revenue recognition (LG-218, fintech: revenue recognition).
//
// A customer pays upfront for a service delivered over time — an
// annual subscription, a multi-month implementation — and the cash
// is not income yet. Booking it as revenue on receipt would overstate
// the period's earnings and leave nothing to match against the cost
// of delivering later periods; booking it nowhere would lose the
// obligation entirely. So receipt records a liability, and revenue
// is recognized one period at a time as the service is delivered:
//
//	receive    -> debit cash, credit deferred-revenue liability
//	recognize  -> debit deferred-revenue liability, credit revenue
//	terminate  -> refund: debit liability, credit cash (money back)
//	             recognize: debit liability, credit revenue (kept)
//
// The schedule is straight-line over a caller-chosen number of
// periods: each RecognizeRevenue call recognizes exactly one
// period's share. The caller drives timing — one call per delivered
// period, or per completed milestone — so progress-based delivery
// is expressed by when calls happen, not by a calendar the ledger
// would have to interpret: with no period length in the schedule,
// "everything due as of" a date is undefined, and inventing one
// would recognize revenue the caller never approved. The asOf
// argument therefore stamps the journal entry and the movement,
// it never decides how many periods to recognize.
//
// Each period's share is floor(total / periods); the final period
// true-ups to the remaining balance, so the recognized total over
// the contract's life is exactly the contract amount — never a cent
// short from repeated flooring, never a cent over. A contract that
// reaches its last period is completed; a contract terminated early
// refunds the remaining balance by default, or recognizes it at
// once when the caller explicitly chooses "recognize" — an early
// termination never silently drops the remaining liability, and
// never recognizes it without being told to.
//
// This is orthogonal to the settlement cutoff (see
// settlement_cutoff.go): that delays when a transfer settles, this
// delays when received cash becomes income. It is orthogonal to
// the scheduled-transfer plan (see schedule.go): that repeats a
// transfer on a cadence, this spreads one receipt's income over
// periods. It is orthogonal to accruals (see accrual.go): an
// accrual books income before cash arrives, this books cash
// before income is earned — opposite directions of the same
// matching principle.
//
// The three booking accounts are ledger-wide configured
// (SetDeferredRevenueConfig): a receipt or recognition with no
// accounts configured is fail-closed, because booking into an
// unconfigured account would silently misstate the books. All
// three accounts must be distinct — debiting and crediting the
// same account would be a no-op that still bumps the journal
// version, and a liability that equals the revenue account would
// let recognition net to nothing.
//
// State is in-memory only, like payout instructions and expense
// claims: contracts and the movement log do not round-trip
// through disaster-recovery snapshots — the journaled receipt,
// recognition, and termination rows do, so the money is safe;
// only the contract metadata is lost. The movement log plus the
// deferred-revenue audit events are the durable record when the
// audit log is enabled (see audit.go).

// Deferred revenue contract statuses.
const (
	DeferredActive     = "active"
	DeferredCompleted  = "completed"
	DeferredRefunded   = "refunded"
	DeferredRecognized = "recognized"
)

// Deferred revenue termination modes.
const (
	DeferredTerminateRefund    = "refund"
	DeferredTerminateRecognize = "recognize"
)

// DeferredRevenueConfig names the three booking accounts for
// deferred revenue: the cash account debited on receipt (and
// credited on a refund termination), the liability account
// credited on receipt and debited on recognition or termination,
// and the revenue account credited on recognition.
type DeferredRevenueConfig struct {
	CashAccount      AccountID `json:"cash_account"`
	LiabilityAccount AccountID `json:"liability_account"`
	RevenueAccount   AccountID `json:"revenue_account"`
}

// DeferredRevenueContract is one upfront receipt spread over
// periods: the customer, the total received, how many periods it
// spans, how much has been recognized, and where it is in its
// lifecycle. RemainingCents is the liability still owed in
// service — total minus recognized, minus any terminated remainder
// (which is zero for an active contract, and zero once a contract
// leaves active, since termination settles the whole remainder).
// EntryID is the receipt's journal entry ID.
type DeferredRevenueContract struct {
	ID                string    `json:"id"`
	Customer          string    `json:"customer"`
	TotalCents        int64     `json:"total_cents"`
	Currency          string    `json:"currency"`
	Periods           int       `json:"periods"`
	PeriodsRecognized int       `json:"periods_recognized"`
	RecognizedCents   int64     `json:"recognized_cents"`
	RemainingCents    int64     `json:"remaining_cents"`
	Status            string    `json:"status"`
	EntryID           string    `json:"entry_id"`
	CreatedAt         time.Time `json:"created_at"`
	SettledAt         time.Time `json:"settled_at,omitempty"`
}

// DeferredRevenueMovement is one atomic contract movement: Kind
// is "receive", "recognize", "terminate-refund", or
// "terminate-recognize". Status is the contract's status
// immediately after the movement.
type DeferredRevenueMovement struct {
	Kind        string    `json:"kind"`
	ContractID  string    `json:"contract_id"`
	AmountCents int64     `json:"amount_cents"`
	Status      string    `json:"status"`
	Actor       string    `json:"actor"`
	At          time.Time `json:"at"`
}

// Deferred revenue validation and state errors.
var (
	// ErrInvalidDeferredCustomer is returned when the customer is
	// empty: every contract needs a named customer for the audit
	// trail.
	ErrInvalidDeferredCustomer = errors.New("ledger: deferred revenue customer must be non-empty")
	// ErrInvalidDeferredAmount is returned when the received amount
	// is not positive.
	ErrInvalidDeferredAmount = errors.New("ledger: deferred revenue amount must be positive")
	// ErrInvalidDeferredPeriods is returned when the period count
	// is less than one: a contract with no periods could never
	// recognize anything.
	ErrInvalidDeferredPeriods = errors.New("ledger: deferred revenue periods must be at least 1")
	// ErrDeferredRevenueNotFound is returned when an operation
	// names an unknown contract.
	ErrDeferredRevenueNotFound = errors.New("ledger: deferred revenue contract not found")
	// ErrDeferredRevenueInvalidTransition is returned when an
	// operation does not fit the contract's current status:
	// recognizing or terminating a contract that is not active.
	ErrDeferredRevenueInvalidTransition = errors.New("ledger: deferred revenue status does not allow this operation")
	// ErrDeferredRevenueAccountsNotConfigured is returned by
	// receipt, recognition, and termination when no deferred
	// revenue accounts have been configured.
	ErrDeferredRevenueAccountsNotConfigured = errors.New("ledger: deferred revenue accounts are not configured")
	// ErrDeferredRevenueSameAccount is returned when the
	// configured booking accounts are not all distinct.
	ErrDeferredRevenueSameAccount = errors.New("ledger: deferred revenue accounts must all differ")
	// ErrInvalidDeferredTerminateMode is returned when the
	// termination mode is neither empty, "refund", nor "recognize".
	ErrInvalidDeferredTerminateMode = errors.New("ledger: deferred revenue terminate mode must be empty, refund, or recognize")
)

// checkDeferredRevenueConfig validates a deferred revenue config
// before it is stored.
func checkDeferredRevenueConfig(c DeferredRevenueConfig) error {
	if c.CashAccount == "" || c.LiabilityAccount == "" || c.RevenueAccount == "" {
		return ErrDeferredRevenueAccountsNotConfigured
	}
	if c.CashAccount == c.LiabilityAccount || c.CashAccount == c.RevenueAccount || c.LiabilityAccount == c.RevenueAccount {
		return ErrDeferredRevenueSameAccount
	}
	return nil
}

// WithDeferredRevenueConfig configures the ledger-wide deferred
// revenue booking accounts (see ReceiveDeferredRevenue).
// Validation happens at booking time, like
// WithBadDebtExpenseAccount.
func WithDeferredRevenueConfig(c DeferredRevenueConfig) Option {
	return func(l *Ledger) {
		l.deferredRevenueConfig = c
		l.deferredRevenueConfigSet = true
	}
}

// SetDeferredRevenueConfig configures (or replaces) the
// ledger-wide deferred revenue booking accounts. It validates
// eagerly: all three accounts must be non-empty and distinct.
// Structural config — it does not bump the journal version and
// it survives disaster-recovery snapshots like the other
// fail-closed booking accounts.
func (l *Ledger) SetDeferredRevenueConfig(c DeferredRevenueConfig) error {
	if err := checkDeferredRevenueConfig(c); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.deferredRevenueConfig = c
	l.deferredRevenueConfigSet = true
	l.emitAudit(AuditEvent{
		Op:       "deferred_revenue_config_set",
		Actor:    "ledger",
		Accounts: []AccountID{c.CashAccount, c.LiabilityAccount, c.RevenueAccount},
		Details: map[string]any{
			"cash_account":      string(c.CashAccount),
			"liability_account": string(c.LiabilityAccount),
			"revenue_account":   string(c.RevenueAccount),
		},
	})
	return nil
}

// DeferredRevenueConfig returns the configured deferred revenue
// booking accounts, reporting whether any were configured.
func (l *Ledger) DeferredRevenueConfig() (DeferredRevenueConfig, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.deferredRevenueConfig, l.deferredRevenueConfigSet
}

// deferredRevenueBooking returns the configured booking accounts
// or a fail-closed error; callers must hold the write lock.
func (l *Ledger) deferredRevenueBooking() (DeferredRevenueConfig, error) {
	if !l.deferredRevenueConfigSet {
		return DeferredRevenueConfig{}, ErrDeferredRevenueAccountsNotConfigured
	}
	cfg := l.deferredRevenueConfig
	if err := checkDeferredRevenueConfig(cfg); err != nil {
		return DeferredRevenueConfig{}, err
	}
	return cfg, nil
}

// ReceiveDeferredRevenue records an upfront receipt as a deferred
// revenue liability: it books one atomic journal entry — debit
// the configured cash account, credit the configured liability
// account — for the received amount, and opens a contract
// spanning periods periods. No income is recognized here: the
// revenue account is untouched by receipt, and the contract's
// recognized total starts at zero.
//
// Contract IDs are ledger-assigned ("defrev-0001", …) in receipt
// order: the caller names the customer, not the contract, because
// one customer routinely holds several overlapping contracts and
// caller-chosen numbers would collide across them.
//
// The call is fail-closed unless every precondition holds: a
// non-empty customer, a positive amount, a valid currency, at
// least one period, and configured, distinct booking accounts. A
// failed posting (a frozen account, a denylisted account, any
// other posting gate) opens no contract — the receipt simply did
// not happen, and can be retried after the gate clears.
func (l *Ledger) ReceiveDeferredRevenue(customer string, amountCents int64, currency string, periods int) (DeferredRevenueContract, error) {
	if customer == "" {
		return DeferredRevenueContract{}, ErrInvalidDeferredCustomer
	}
	if amountCents <= 0 {
		return DeferredRevenueContract{}, ErrInvalidDeferredAmount
	}
	cur, err := normalizeCurrency(currency)
	if err != nil {
		return DeferredRevenueContract{}, err
	}
	if periods < 1 {
		return DeferredRevenueContract{}, ErrInvalidDeferredPeriods
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	cfg, err := l.deferredRevenueBooking()
	if err != nil {
		return DeferredRevenueContract{}, err
	}
	l.deferredRevenueSeq++
	id := fmt.Sprintf("defrev-%04d", l.deferredRevenueSeq)
	e := JournalEntry{
		ID:             "defrev/" + id + "/receipt",
		DebitAccount:   cfg.CashAccount,
		CreditAccount:  cfg.LiabilityAccount,
		AmountCents:    amountCents,
		Currency:       cur,
		IdempotencyKey: "defrev/" + id + "/receipt",
		Memo:           fmt.Sprintf("deferred revenue receipt %s", id),
		CreatedAt:      time.Now(),
	}
	posted, _, err := l.postSystemLocked(e, false)
	if err != nil {
		return DeferredRevenueContract{}, err
	}
	now := time.Now().UTC()
	contract := DeferredRevenueContract{
		ID:             id,
		Customer:       customer,
		TotalCents:     amountCents,
		Currency:       cur,
		Periods:        periods,
		RemainingCents: amountCents,
		Status:         DeferredActive,
		EntryID:        posted.ID,
		CreatedAt:      now,
	}
	l.deferredRevenue[id] = contract
	l.deferredRevenueMovements = append(l.deferredRevenueMovements, DeferredRevenueMovement{
		Kind:        "receive",
		ContractID:  id,
		AmountCents: amountCents,
		Status:      DeferredActive,
		Actor:       "ledger",
		At:          now,
	})
	l.emitAudit(AuditEvent{
		Op:      "deferred_revenue_received",
		Actor:   "ledger",
		TraceID: id,
		Details: map[string]any{
			"contract_id":  id,
			"customer":     customer,
			"amount_cents": amountCents,
			"currency":     cur,
			"periods":      periods,
			"entry_id":     posted.ID,
		},
	})
	return contract, nil
}

// DeferredRevenue returns a contract's current record, reporting
// whether it exists. The returned copy cannot mutate the ledger's
// record.
func (l *Ledger) DeferredRevenue(id string) (DeferredRevenueContract, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	contract, ok := l.deferredRevenue[id]
	return contract, ok
}

// DeferredRevenuesByCustomer returns one customer's contracts in
// receipt order (created-at, then ID for ties): the customer's
// deferred history, active and settled alike.
func (l *Ledger) DeferredRevenuesByCustomer(customer string) []DeferredRevenueContract {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]DeferredRevenueContract, 0)
	for _, c := range l.deferredRevenue {
		if c.Customer == customer {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// RecognizeRevenue recognizes exactly one period's share of an
// active contract: it books one atomic journal entry — debit the
// configured liability account, credit the configured revenue
// account — and advances the contract. A non-final period
// recognizes floor(total / periods); the final period recognizes
// the whole remaining balance (the true-up), so the recognized
// total over the contract's life is exactly the contract amount.
// Recognizing the final period completes the contract.
//
// asOf stamps the journal entry and the movement; a zero asOf
// means now. asOf never decides how many periods to recognize —
// see the package comment: one call, one period, always.
//
// The call is fail-closed unless every precondition holds: the
// contract exists and is active, and booking accounts are
// configured and distinct. A failed posting leaves the contract
// untouched — the period simply was not recognized, and can be
// retried after the gate clears — while a successful posting
// advances the contract, so a retried call recognizes the next
// period instead of paying the same period twice.
func (l *Ledger) RecognizeRevenue(id string, asOf time.Time) (JournalEntry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	contract, ok := l.deferredRevenue[id]
	if !ok {
		return JournalEntry{}, ErrDeferredRevenueNotFound
	}
	if contract.Status != DeferredActive {
		return JournalEntry{}, ErrDeferredRevenueInvalidTransition
	}
	cfg, err := l.deferredRevenueBooking()
	if err != nil {
		return JournalEntry{}, err
	}
	amount := contract.TotalCents / int64(contract.Periods)
	if contract.PeriodsRecognized+1 >= contract.Periods {
		amount = contract.RemainingCents
	}
	stamp := asOf
	if stamp.IsZero() {
		stamp = time.Now()
	}
	period := contract.PeriodsRecognized + 1
	e := JournalEntry{
		ID:             fmt.Sprintf("defrev/%s/recognize/%d", id, period),
		DebitAccount:   cfg.LiabilityAccount,
		CreditAccount:  cfg.RevenueAccount,
		AmountCents:    amount,
		Currency:       contract.Currency,
		IdempotencyKey: fmt.Sprintf("defrev/%s/recognize/%d", id, period),
		Memo:           fmt.Sprintf("deferred revenue recognition %s period %d", id, period),
		CreatedAt:      stamp,
	}
	posted, _, err := l.postSystemLocked(e, false)
	if err != nil {
		return JournalEntry{}, err
	}
	now := time.Now().UTC()
	contract.PeriodsRecognized = period
	contract.RecognizedCents += amount
	contract.RemainingCents -= amount
	if period >= contract.Periods {
		contract.Status = DeferredCompleted
		contract.SettledAt = now
	}
	l.deferredRevenue[id] = contract
	l.deferredRevenueMovements = append(l.deferredRevenueMovements, DeferredRevenueMovement{
		Kind:        "recognize",
		ContractID:  id,
		AmountCents: amount,
		Status:      contract.Status,
		Actor:       "ledger",
		At:          now,
	})
	l.emitAudit(AuditEvent{
		Op:      "deferred_revenue_recognized",
		Actor:   "ledger",
		TraceID: id,
		Details: map[string]any{
			"contract_id":      id,
			"customer":         contract.Customer,
			"period":           period,
			"amount_cents":     amount,
			"recognized_cents": contract.RecognizedCents,
			"remaining_cents":  contract.RemainingCents,
			"currency":         contract.Currency,
			"entry_id":         posted.ID,
			"status":           contract.Status,
		},
	})
	return posted, nil
}

// TerminateDeferredRevenue settles an active contract early. The
// mode selects what happens to the remaining liability: "" or
// "refund" returns it to the customer (debit liability, credit
// cash) and marks the contract refunded; "recognize" books it as
// income at once (debit liability, credit revenue) and marks the
// contract recognized. Any other mode is refused without touching
// the contract, and the empty mode defaults to refund — an early
// termination never silently drops money, and never recognizes it
// unless explicitly told to.
//
// A failed posting leaves the contract active — the termination
// simply did not happen, and can be retried after the gate
// clears — while a successful posting settles the whole
// remainder, so the contract can never be terminated twice.
func (l *Ledger) TerminateDeferredRevenue(id, mode string) (JournalEntry, error) {
	if mode == "" {
		mode = DeferredTerminateRefund
	}
	if mode != DeferredTerminateRefund && mode != DeferredTerminateRecognize {
		return JournalEntry{}, ErrInvalidDeferredTerminateMode
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	contract, ok := l.deferredRevenue[id]
	if !ok {
		return JournalEntry{}, ErrDeferredRevenueNotFound
	}
	if contract.Status != DeferredActive {
		return JournalEntry{}, ErrDeferredRevenueInvalidTransition
	}
	cfg, err := l.deferredRevenueBooking()
	if err != nil {
		return JournalEntry{}, err
	}
	var credit AccountID
	var status, kind, op string
	if mode == DeferredTerminateRefund {
		credit = cfg.CashAccount
		status = DeferredRefunded
		kind = "terminate-refund"
		op = "deferred_revenue_terminated_refund"
	} else {
		credit = cfg.RevenueAccount
		status = DeferredRecognized
		kind = "terminate-recognize"
		op = "deferred_revenue_terminated_recognize"
	}
	e := JournalEntry{
		ID:             "defrev/" + id + "/terminate",
		DebitAccount:   cfg.LiabilityAccount,
		CreditAccount:  credit,
		AmountCents:    contract.RemainingCents,
		Currency:       contract.Currency,
		IdempotencyKey: "defrev/" + id + "/terminate",
		Memo:           fmt.Sprintf("deferred revenue termination %s %s", id, mode),
		CreatedAt:      time.Now(),
	}
	posted, _, err := l.postSystemLocked(e, false)
	if err != nil {
		return JournalEntry{}, err
	}
	now := time.Now().UTC()
	amount := contract.RemainingCents
	if mode == DeferredTerminateRecognize {
		contract.RecognizedCents += amount
	}
	contract.RemainingCents = 0
	contract.Status = status
	contract.SettledAt = now
	l.deferredRevenue[id] = contract
	l.deferredRevenueMovements = append(l.deferredRevenueMovements, DeferredRevenueMovement{
		Kind:        kind,
		ContractID:  id,
		AmountCents: amount,
		Status:      status,
		Actor:       "ledger",
		At:          now,
	})
	l.emitAudit(AuditEvent{
		Op:      op,
		Actor:   "ledger",
		TraceID: id,
		Details: map[string]any{
			"contract_id":  id,
			"customer":     contract.Customer,
			"mode":         mode,
			"amount_cents": amount,
			"currency":     contract.Currency,
			"entry_id":     posted.ID,
			"status":       status,
		},
	})
	return posted, nil
}

// DeferredRevenueMovements returns the contract movement log for
// one contract, in append order: the audit trail of its receipt,
// recognitions, and termination. It returns a copy — callers
// cannot mutate the log through it. An unknown contract returns
// an empty log, like ExpenseMovements for an unknown claim.
func (l *Ledger) DeferredRevenueMovements(id string) []DeferredRevenueMovement {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]DeferredRevenueMovement, 0)
	for _, m := range l.deferredRevenueMovements {
		if m.ContractID == id {
			out = append(out, m)
		}
	}
	return out
}
