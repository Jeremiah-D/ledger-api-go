package ledger

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Employee expense reimbursement (LG-217, fintech: expense claims).
//
// An employee spends out of pocket and files a claim; the claim is
// decided by someone else and only then paid. Lifecycle:
//
//	pending -> approved -> closed
//	pending -> rejected
//
// Submitting records the claim — it moves no money. Approving or
// rejecting decides it, and reimbursing books one atomic journal
// entry — debit the configured expense account (the 费用科目),
// credit the configured employee-payable account (the 应付-员工账户)
// — and closes the claim. A closed claim can never be reimbursed
// again, so the same expense can never be paid twice; a decided
// claim (approved, rejected, or closed) can never be re-decided.
//
// Separation of duties: the approver must differ from the
// submitter. The submitter is the employee named on the claim, so
// ApproveExpense and RejectExpense both refuse an approver equal
// to the employee — an employee cannot approve their own expense,
// and cannot reject it either, because a self-rejection would let
// the submitter bury a claim without a second pair of eyes.
//
// This is orthogonal to the generic journal review (see review.go):
// review is dual control over journal entries themselves —
// thresholds, quorums, delegations — while this is the expense
// business flow: submit, decide, pay, with its own document
// lifecycle. A reimbursement entry posted here is an ordinary
// journal row (audit-chain link, version bump), so VerifyChain and
// Reconcile cover reimbursed expenses exactly like ordinary
// postings. It is orthogonal to the scheduled-transfer plan (see
// schedule.go): a schedule repeats a transfer on a cadence, while
// an expense is a single claim paid at most once.
//
// The two booking accounts are ledger-wide configured
// (SetExpenseConfig): a reimbursement with no accounts configured
// is fail-closed, because paying into an unconfigured account
// would silently misstate the books. The expense account and the
// payable account must differ — debiting and crediting the same
// account would be a no-op that still bumps the journal version.
//
// The reimbursement entry posts through the system path
// (postSystemLocked): the full gate order runs (validation,
// denylist, idempotency replay, frozen, overdraft, daily-limit,
// period), so a reimbursement cannot route around the ledger's
// risk controls — a frozen expense or payable account refuses the
// payment and leaves the claim approved, to be paid after the
// freeze lifts, rather than paid into a frozen account.
//
// State is in-memory only, like payout instructions and gift
// cards: claims, the config, and the movement log do not
// round-trip through disaster-recovery snapshots — the journaled
// reimbursement rows do, so the money is safe; only the claim
// metadata is lost. The movement log plus the expense audit
// events are the durable record when the audit log is enabled
// (see audit.go).

// Expense statuses.
const (
	ExpensePending  = "pending"
	ExpenseApproved = "approved"
	ExpenseRejected = "rejected"
	ExpenseClosed   = "closed"
)

// ExpenseConfig names the two booking accounts for reimbursements:
// the expense account debited and the employee-payable account
// credited when a claim is paid.
type ExpenseConfig struct {
	ExpenseAccount         AccountID `json:"expense_account"`
	EmployeePayableAccount AccountID `json:"employee_payable_account"`
}

// Expense is one employee expense claim: the amount claimed, in
// which currency, for what category, by whom, and where it is in
// its lifecycle. Approver is empty until the claim is decided.
// EntryID is empty until the claim is reimbursed.
type Expense struct {
	ID           string    `json:"id"`
	Employee     string    `json:"employee"`
	AmountCents  int64     `json:"amount_cents"`
	Currency     string    `json:"currency"`
	Category     string    `json:"category"`
	Status       string    `json:"status"`
	Approver     string    `json:"approver,omitempty"`
	SubmittedAt  time.Time `json:"submitted_at"`
	DecidedAt    time.Time `json:"decided_at,omitempty"`
	ReimbursedAt time.Time `json:"reimbursed_at,omitempty"`
	EntryID      string    `json:"entry_id,omitempty"`
}

// ExpenseMovement is one atomic claim movement: Kind is "submit",
// "approve", "reject", or "reimburse". Status is the claim's
// status immediately after the movement; Actor is the employee
// for a submission, the approver for a decision, and "ledger" for
// a reimbursement.
type ExpenseMovement struct {
	Kind        string    `json:"kind"`
	ExpenseID   string    `json:"expense_id"`
	AmountCents int64     `json:"amount_cents"`
	Status      string    `json:"status"`
	Actor       string    `json:"actor"`
	At          time.Time `json:"at"`
}

// Expense validation and state errors.
var (
	// ErrInvalidExpenseID is returned when the expense number is
	// empty or contains '/': the reimbursement entry ID joins its
	// components with '/', so a '/' inside the number would let
	// two claims share one reimbursement row.
	ErrInvalidExpenseID = errors.New("ledger: expense id must be non-empty and must not contain '/'")
	// ErrInvalidExpenseEmployee is returned when the employee is empty:
	// every claim needs a submitter for the separation-of-duties check.
	ErrInvalidExpenseEmployee = errors.New("ledger: expense employee must be non-empty")
	// ErrInvalidExpenseAmount is returned when the claimed amount is
	// not positive.
	ErrInvalidExpenseAmount = errors.New("ledger: expense amount must be positive")
	// ErrInvalidExpenseCategory is returned when the expense category
	// is empty: a claim without a category is unauditable.
	ErrInvalidExpenseCategory = errors.New("ledger: expense category must be non-empty")
	// ErrExpenseIDConflict is returned when the expense number is
	// already in use: numbers are unique, and re-submitting one is
	// always a caller bug, never a replay.
	ErrExpenseIDConflict = errors.New("ledger: expense id already in use")
	// ErrExpenseNotFound is returned when an operation names an
	// unknown expense number.
	ErrExpenseNotFound = errors.New("ledger: expense not found")
	// ErrEmptyExpenseApprover is returned when the approver is empty:
	// every decision needs a named approver for the audit trail.
	ErrEmptyExpenseApprover = errors.New("ledger: expense approver must be non-empty")
	// ErrExpenseSelfApproval is returned when the approver equals
	// the employee: an employee cannot decide their own claim.
	ErrExpenseSelfApproval = errors.New("ledger: expense approver must differ from the employee")
	// ErrExpenseInvalidTransition is returned when an operation
	// does not fit the claim's current status: deciding a decided
	// claim, or reimbursing a claim that is not approved.
	ErrExpenseInvalidTransition = errors.New("ledger: expense status does not allow this operation")
	// ErrExpenseAccountsNotConfigured is returned by
	// ReimburseExpense when no expense accounts have been
	// configured with SetExpenseConfig.
	ErrExpenseAccountsNotConfigured = errors.New("ledger: expense accounts are not configured")
	// ErrExpenseSameAccount is returned when the configured expense
	// account equals the employee-payable account.
	ErrExpenseSameAccount = errors.New("ledger: expense account must differ from the employee-payable account")
)

// checkExpenseConfig validates an expense config before it is stored.
func checkExpenseConfig(c ExpenseConfig) error {
	if c.ExpenseAccount == "" || c.EmployeePayableAccount == "" {
		return ErrExpenseAccountsNotConfigured
	}
	if c.ExpenseAccount == c.EmployeePayableAccount {
		return ErrExpenseSameAccount
	}
	return nil
}

// WithExpenseConfig configures the ledger-wide expense booking
// accounts that reimbursements debit and credit (see
// ReimburseExpense). Validation happens at reimbursement time,
// like WithBadDebtExpenseAccount.
func WithExpenseConfig(c ExpenseConfig) Option {
	return func(l *Ledger) {
		l.expenseConfig = c
		l.expenseConfigSet = true
	}
}

// SetExpenseConfig configures (or replaces) the ledger-wide expense
// booking accounts. It validates eagerly: both accounts must be
// non-empty and must differ. Structural config — it does not bump
// the journal version and it survives disaster-recovery snapshots
// like the other fail-closed booking accounts.
func (l *Ledger) SetExpenseConfig(c ExpenseConfig) error {
	if err := checkExpenseConfig(c); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.expenseConfig = c
	l.expenseConfigSet = true
	l.emitAudit(AuditEvent{
		Op:       "expense_config_set",
		Actor:    "ledger",
		Accounts: []AccountID{c.ExpenseAccount, c.EmployeePayableAccount},
		Details: map[string]any{
			"expense_account":          string(c.ExpenseAccount),
			"employee_payable_account": string(c.EmployeePayableAccount),
		},
	})
	return nil
}

// ExpenseConfig returns the configured expense booking accounts,
// reporting whether any were configured.
func (l *Ledger) ExpenseConfig() (ExpenseConfig, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.expenseConfig, l.expenseConfigSet
}

// SubmitExpense files a claim: it records the claim as pending and
// moves no money. The number must be unique, the employee
// non-empty (the employee is the submitter for the
// separation-of-duties check), the amount positive, the currency
// a 3-letter uppercase code (empty means the default currency),
// and the category non-empty.
func (l *Ledger) SubmitExpense(id, employee string, amountCents int64, currency, category string) (Expense, error) {
	if id == "" || strings.Contains(id, "/") {
		return Expense{}, ErrInvalidExpenseID
	}
	if employee == "" {
		return Expense{}, ErrInvalidExpenseEmployee
	}
	if amountCents <= 0 {
		return Expense{}, ErrInvalidExpenseAmount
	}
	cur, err := normalizeCurrency(currency)
	if err != nil {
		return Expense{}, err
	}
	if category == "" {
		return Expense{}, ErrInvalidExpenseCategory
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, exists := l.expenses[id]; exists {
		return Expense{}, ErrExpenseIDConflict
	}
	now := time.Now().UTC()
	exp := Expense{
		ID:          id,
		Employee:    employee,
		AmountCents: amountCents,
		Currency:    cur,
		Category:    category,
		Status:      ExpensePending,
		SubmittedAt: now,
	}
	l.expenses[id] = exp
	l.expenseMovements = append(l.expenseMovements, ExpenseMovement{
		Kind:        "submit",
		ExpenseID:   id,
		AmountCents: amountCents,
		Status:      ExpensePending,
		Actor:       employee,
		At:          now,
	})
	l.emitAudit(AuditEvent{
		Op:      "expense_submitted",
		Actor:   employee,
		TraceID: id,
		Details: map[string]any{
			"expense_id":   id,
			"employee":     employee,
			"amount_cents": amountCents,
			"currency":     cur,
			"category":     category,
		},
	})
	return exp, nil
}

// Expense returns a claim's current record, reporting whether it
// exists. The returned copy cannot mutate the ledger's record.
func (l *Ledger) Expense(id string) (Expense, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	exp, ok := l.expenses[id]
	return exp, ok
}

// ExpensesByEmployee returns one employee's claims in submission
// order (submitted-at, then number for ties): the employee's
// claim history, pending and decided alike.
func (l *Ledger) ExpensesByEmployee(employee string) []Expense {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]Expense, 0)
	for _, exp := range l.expenses {
		if exp.Employee == employee {
			out = append(out, exp)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].SubmittedAt.Equal(out[j].SubmittedAt) {
			return out[i].SubmittedAt.Before(out[j].SubmittedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// decideExpense settles a pending claim one way or the other: the
// shared body of ApproveExpense and RejectExpense. The approver
// must be named and must differ from the employee, and the claim
// must still be pending — a decided claim cannot be re-decided,
// in either direction.
func (l *Ledger) decideExpense(id, approver, status, kind, op string) (Expense, error) {
	if approver == "" {
		return Expense{}, ErrEmptyExpenseApprover
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	exp, ok := l.expenses[id]
	if !ok {
		return Expense{}, ErrExpenseNotFound
	}
	if exp.Status != ExpensePending {
		return Expense{}, ErrExpenseInvalidTransition
	}
	if approver == exp.Employee {
		return Expense{}, ErrExpenseSelfApproval
	}
	now := time.Now().UTC()
	exp.Status = status
	exp.Approver = approver
	exp.DecidedAt = now
	l.expenses[id] = exp
	l.expenseMovements = append(l.expenseMovements, ExpenseMovement{
		Kind:        kind,
		ExpenseID:   id,
		AmountCents: exp.AmountCents,
		Status:      status,
		Actor:       approver,
		At:          now,
	})
	l.emitAudit(AuditEvent{
		Op:      op,
		Actor:   approver,
		TraceID: id,
		Details: map[string]any{
			"expense_id":   id,
			"employee":     exp.Employee,
			"approver":     approver,
			"amount_cents": exp.AmountCents,
			"currency":     exp.Currency,
			"category":     exp.Category,
		},
	})
	return exp, nil
}

// ApproveExpense approves a pending claim: the approver must be
// named and must differ from the employee, and the claim must
// still be pending. Approval moves no money — payment happens at
// ReimburseExpense.
func (l *Ledger) ApproveExpense(id, approver string) (Expense, error) {
	return l.decideExpense(id, approver, ExpenseApproved, "approve", "expense_approved")
}

// RejectExpense rejects a pending claim: the same
// separation-of-duties rule as approval — the approver must be
// named and must differ from the employee — and the claim must
// still be pending. A rejected claim is terminal: it can never be
// approved or reimbursed afterwards.
func (l *Ledger) RejectExpense(id, approver string) (Expense, error) {
	return l.decideExpense(id, approver, ExpenseRejected, "reject", "expense_rejected")
}

// ReimburseExpense pays an approved claim: it books one atomic
// journal entry — debit the configured expense account, credit
// the configured employee-payable account — for the claimed
// amount in the claimed currency, and closes the claim. The entry
// ID is "expense/<number>", so the reimbursement is unique per
// claim by construction.
//
// The call is fail-closed unless every precondition holds: the
// claim exists and is approved, and expense accounts are
// configured and distinct. A failed posting (a frozen account, a
// denylisted account, any other posting gate) leaves the claim
// approved — the payment simply did not happen, and can be
// retried after the gate clears — while a successful posting
// closes the claim, so a second reimbursement attempt is refused
// as an invalid transition instead of paying twice.
//
// A successful reimbursement is audited as "expense_reimbursed"
// with the journal entry ID, so the payment is traceable from the
// claim to the journal row.
func (l *Ledger) ReimburseExpense(id string) (JournalEntry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	exp, ok := l.expenses[id]
	if !ok {
		return JournalEntry{}, ErrExpenseNotFound
	}
	if exp.Status != ExpenseApproved {
		return JournalEntry{}, ErrExpenseInvalidTransition
	}
	if !l.expenseConfigSet {
		return JournalEntry{}, ErrExpenseAccountsNotConfigured
	}
	cfg := l.expenseConfig
	if cfg.ExpenseAccount == "" || cfg.EmployeePayableAccount == "" {
		return JournalEntry{}, ErrExpenseAccountsNotConfigured
	}
	if cfg.ExpenseAccount == cfg.EmployeePayableAccount {
		return JournalEntry{}, ErrExpenseSameAccount
	}
	e := JournalEntry{
		ID:             "expense/" + id,
		DebitAccount:   cfg.ExpenseAccount,
		CreditAccount:  cfg.EmployeePayableAccount,
		AmountCents:    exp.AmountCents,
		Currency:       exp.Currency,
		IdempotencyKey: "expense/" + id,
		Memo:           fmt.Sprintf("expense reimbursement %s", id),
		CreatedAt:      time.Now(),
	}
	posted, duplicate, err := l.postSystemLocked(e, false)
	if err != nil {
		return JournalEntry{}, err
	}
	if duplicate {
		// The reimbursement row already exists (a replay of the
		// entry's own key): the money is already journaled, so
		// the claim closes rather than paying twice.
	}
	now := time.Now().UTC()
	exp.Status = ExpenseClosed
	exp.ReimbursedAt = now
	exp.EntryID = posted.ID
	l.expenses[id] = exp
	l.expenseMovements = append(l.expenseMovements, ExpenseMovement{
		Kind:        "reimburse",
		ExpenseID:   id,
		AmountCents: exp.AmountCents,
		Status:      ExpenseClosed,
		Actor:       "ledger",
		At:          now,
	})
	l.emitAudit(AuditEvent{
		Op:      "expense_reimbursed",
		Actor:   "ledger",
		TraceID: id,
		Details: map[string]any{
			"expense_id":   id,
			"employee":     exp.Employee,
			"approver":     exp.Approver,
			"amount_cents": exp.AmountCents,
			"currency":     exp.Currency,
			"category":     exp.Category,
			"entry_id":     posted.ID,
		},
	})
	return posted, nil
}

// ExpenseMovements returns the claim movement log for one claim,
// in append order: the audit trail of its submit, decision, and
// reimbursement. It returns a copy — callers cannot mutate the log
// through it. An unknown number returns an empty log, like
// GiftCardMovements for an unknown card.
func (l *Ledger) ExpenseMovements(id string) []ExpenseMovement {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]ExpenseMovement, 0)
	for _, m := range l.expenseMovements {
		if m.ExpenseID == id {
			out = append(out, m)
		}
	}
	return out
}
