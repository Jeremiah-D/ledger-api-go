package ledger

import (
	"errors"
	"testing"
)

func newExpenseLedger(t *testing.T) *Ledger {
	t.Helper()
	l := New(WithExpenseConfig(ExpenseConfig{
		ExpenseAccount:         "travel-expense",
		EmployeePayableAccount: "payable-employees",
	}))
	return l
}

func TestExpenseHappyPathPaysOnce(t *testing.T) {
	l := newExpenseLedger(t)
	exp, err := l.SubmitExpense("EXP-001", "alice", 42000, "USD", "travel")
	if err != nil {
		t.Fatalf("SubmitExpense: %v", err)
	}
	if exp.Status != ExpensePending {
		t.Fatalf("submitted status: got %q, want pending", exp.Status)
	}
	if _, err := l.ApproveExpense("EXP-001", "alice"); !errors.Is(err, ErrExpenseSelfApproval) {
		t.Fatalf("self-approval: got %v, want ErrExpenseSelfApproval", err)
	}
	// A refused self-approval leaves the claim pending and decidable.
	approved, err := l.ApproveExpense("EXP-001", "bob")
	if err != nil {
		t.Fatalf("ApproveExpense: %v", err)
	}
	if approved.Status != ExpenseApproved || approved.Approver != "bob" {
		t.Fatalf("approved claim: %+v", approved)
	}
	posted, err := l.ReimburseExpense("EXP-001")
	if err != nil {
		t.Fatalf("ReimburseExpense: %v", err)
	}
	if posted.DebitAccount != "travel-expense" || posted.CreditAccount != "payable-employees" {
		t.Fatalf("reimbursement legs: debit %q credit %q", posted.DebitAccount, posted.CreditAccount)
	}
	if posted.AmountCents != 42000 || posted.Currency != "USD" {
		t.Fatalf("reimbursement amount/currency: %d %q", posted.AmountCents, posted.Currency)
	}
	if posted.ID != "expense/EXP-001" || posted.IdempotencyKey != "expense/EXP-001" {
		t.Fatalf("reimbursement id/key: %q %q", posted.ID, posted.IdempotencyKey)
	}
	if got := l.BalanceIn("travel-expense", "USD"); got != 42000 {
		t.Fatalf("expense balance: got %d, want 42000", got)
	}
	if got := l.BalanceIn("payable-employees", "USD"); got != -42000 {
		t.Fatalf("payable balance: got %d, want -42000", got)
	}
	closed, ok := l.Expense("EXP-001")
	if !ok || closed.Status != ExpenseClosed || closed.EntryID != posted.ID {
		t.Fatalf("closed claim: %+v ok=%v", closed, ok)
	}
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain after reimbursement: %v", err)
	}
	// Paying twice is refused, and books nothing new.
	if _, err := l.ReimburseExpense("EXP-001"); !errors.Is(err, ErrExpenseInvalidTransition) {
		t.Fatalf("second reimbursement: got %v, want ErrExpenseInvalidTransition", err)
	}
	if got := l.BalanceIn("travel-expense", "USD"); got != 42000 {
		t.Fatalf("balance moved on refused second payment: %d", got)
	}
}

func TestExpenseDecisionsAreTerminal(t *testing.T) {
	l := newExpenseLedger(t)
	if _, err := l.SubmitExpense("EXP-010", "alice", 1000, "USD", "meals"); err != nil {
		t.Fatalf("SubmitExpense: %v", err)
	}
	if _, err := l.RejectExpense("EXP-010", "bob"); err != nil {
		t.Fatalf("RejectExpense: %v", err)
	}
	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"approve-after-reject", func() error { _, err := l.ApproveExpense("EXP-010", "carol"); return err }},
		{"reject-after-reject", func() error { _, err := l.RejectExpense("EXP-010", "carol"); return err }},
		{"reimburse-rejected", func() error { _, err := l.ReimburseExpense("EXP-010"); return err }},
	} {
		if err := tc.call(); !errors.Is(err, ErrExpenseInvalidTransition) {
			t.Fatalf("%s: got %v, want ErrExpenseInvalidTransition", tc.name, err)
		}
	}
	if _, err := l.SubmitExpense("EXP-011", "alice", 1000, "USD", "meals"); err != nil {
		t.Fatalf("SubmitExpense: %v", err)
	}
	if _, err := l.ApproveExpense("EXP-011", "bob"); err != nil {
		t.Fatalf("ApproveExpense: %v", err)
	}
	if _, err := l.RejectExpense("EXP-011", "carol"); !errors.Is(err, ErrExpenseInvalidTransition) {
		t.Fatalf("reject-after-approve: got %v, want ErrExpenseInvalidTransition", err)
	}
	if _, err := l.ApproveExpense("EXP-011", "carol"); !errors.Is(err, ErrExpenseInvalidTransition) {
		t.Fatalf("approve-after-approve: got %v, want ErrExpenseInvalidTransition", err)
	}
}

func TestExpenseSelfRejectionRefused(t *testing.T) {
	l := newExpenseLedger(t)
	if _, err := l.SubmitExpense("EXP-020", "alice", 1000, "USD", "meals"); err != nil {
		t.Fatalf("SubmitExpense: %v", err)
	}
	if _, err := l.RejectExpense("EXP-020", "alice"); !errors.Is(err, ErrExpenseSelfApproval) {
		t.Fatalf("self-rejection: got %v, want ErrExpenseSelfApproval", err)
	}
	if exp, _ := l.Expense("EXP-020"); exp.Status != ExpensePending {
		t.Fatalf("refused self-rejection changed status: %q", exp.Status)
	}
}

func TestExpenseValidationAndConflicts(t *testing.T) {
	l := newExpenseLedger(t)
	cases := []struct {
		name string
		call func() error
		want error
	}{
		{"empty id", func() error { _, err := l.SubmitExpense("", "alice", 1000, "USD", "meals"); return err }, ErrInvalidExpenseID},
		{"slash id", func() error { _, err := l.SubmitExpense("EXP/1", "alice", 1000, "USD", "meals"); return err }, ErrInvalidExpenseID},
		{"empty employee", func() error { _, err := l.SubmitExpense("EXP-030", "", 1000, "USD", "meals"); return err }, ErrInvalidExpenseEmployee},
		{"zero amount", func() error { _, err := l.SubmitExpense("EXP-030", "alice", 0, "USD", "meals"); return err }, ErrInvalidExpenseAmount},
		{"negative amount", func() error { _, err := l.SubmitExpense("EXP-030", "alice", -5, "USD", "meals"); return err }, ErrInvalidExpenseAmount},
		{"empty category", func() error { _, err := l.SubmitExpense("EXP-030", "alice", 1000, "USD", ""); return err }, ErrInvalidExpenseCategory},
		{"lowercase currency", func() error { _, err := l.SubmitExpense("EXP-030", "alice", 1000, "usd", "meals"); return err }, ErrInvalidCurrency},
		{"empty approver", func() error { _, err := l.ApproveExpense("EXP-030", ""); return err }, ErrEmptyExpenseApprover},
		{"unknown approve", func() error { _, err := l.ApproveExpense("EXP-404", "bob"); return err }, ErrExpenseNotFound},
		{"unknown reimburse", func() error { _, err := l.ReimburseExpense("EXP-404"); return err }, ErrExpenseNotFound},
	}
	for _, tc := range cases {
		if err := tc.call(); !errors.Is(err, tc.want) {
			t.Fatalf("%s: got %v, want %v", tc.name, err, tc.want)
		}
	}
	if _, err := l.SubmitExpense("EXP-031", "alice", 1000, "USD", "meals"); err != nil {
		t.Fatalf("SubmitExpense: %v", err)
	}
	if _, err := l.SubmitExpense("EXP-031", "bob", 2000, "USD", "travel"); !errors.Is(err, ErrExpenseIDConflict) {
		t.Fatalf("duplicate number: got %v, want ErrExpenseIDConflict", err)
	}
	// Reimbursing a merely pending claim is refused.
	if _, err := l.ReimburseExpense("EXP-031"); !errors.Is(err, ErrExpenseInvalidTransition) {
		t.Fatalf("reimburse pending: got %v, want ErrExpenseInvalidTransition", err)
	}
}

func TestExpenseReimburseFailClosedWithoutConfig(t *testing.T) {
	l := New()
	if _, err := l.SubmitExpense("EXP-040", "alice", 1000, "USD", "meals"); err != nil {
		t.Fatalf("SubmitExpense: %v", err)
	}
	if _, err := l.ApproveExpense("EXP-040", "bob"); err != nil {
		t.Fatalf("ApproveExpense: %v", err)
	}
	if _, err := l.ReimburseExpense("EXP-040"); !errors.Is(err, ErrExpenseAccountsNotConfigured) {
		t.Fatalf("unconfigured reimbursement: got %v, want ErrExpenseAccountsNotConfigured", err)
	}
	// The failed payment leaves the claim approved, payable later.
	if exp, _ := l.Expense("EXP-040"); exp.Status != ExpenseApproved {
		t.Fatalf("status after failed reimbursement: %q", exp.Status)
	}
	if err := l.SetExpenseConfig(ExpenseConfig{ExpenseAccount: "travel-expense", EmployeePayableAccount: "travel-expense"}); !errors.Is(err, ErrExpenseSameAccount) {
		t.Fatalf("same-account config: got %v, want ErrExpenseSameAccount", err)
	}
	if err := l.SetExpenseConfig(ExpenseConfig{ExpenseAccount: "", EmployeePayableAccount: "payable-employees"}); !errors.Is(err, ErrExpenseAccountsNotConfigured) {
		t.Fatalf("empty-account config: got %v, want ErrExpenseAccountsNotConfigured", err)
	}
}

func TestExpenseFrozenAccountBlocksPayment(t *testing.T) {
	l := newExpenseLedger(t)
	if _, err := l.SubmitExpense("EXP-050", "alice", 5000, "USD", "travel"); err != nil {
		t.Fatalf("SubmitExpense: %v", err)
	}
	if _, err := l.ApproveExpense("EXP-050", "bob"); err != nil {
		t.Fatalf("ApproveExpense: %v", err)
	}
	l.Freeze("travel-expense")
	if _, err := l.ReimburseExpense("EXP-050"); err == nil {
		t.Fatal("frozen expense account: reimbursement unexpectedly succeeded")
	}
	if exp, _ := l.Expense("EXP-050"); exp.Status != ExpenseApproved {
		t.Fatalf("status after frozen refusal: %q", exp.Status)
	}
	l.Unfreeze("travel-expense")
	if _, err := l.ReimburseExpense("EXP-050"); err != nil {
		t.Fatalf("ReimburseExpense after unfreeze: %v", err)
	}
	if exp, _ := l.Expense("EXP-050"); exp.Status != ExpenseClosed {
		t.Fatalf("status after payment: %q", exp.Status)
	}
}

func TestExpenseMovementsAndEmployeeListing(t *testing.T) {
	l := newExpenseLedger(t)
	if _, err := l.SubmitExpense("EXP-060", "alice", 1000, "USD", "meals"); err != nil {
		t.Fatalf("SubmitExpense: %v", err)
	}
	if _, err := l.SubmitExpense("EXP-061", "alice", 2000, "USD", "travel"); err != nil {
		t.Fatalf("SubmitExpense: %v", err)
	}
	if _, err := l.SubmitExpense("EXP-062", "bob", 3000, "USD", "meals"); err != nil {
		t.Fatalf("SubmitExpense: %v", err)
	}
	if _, err := l.ApproveExpense("EXP-060", "carol"); err != nil {
		t.Fatalf("ApproveExpense: %v", err)
	}
	if _, err := l.ReimburseExpense("EXP-060"); err != nil {
		t.Fatalf("ReimburseExpense: %v", err)
	}
	movs := l.ExpenseMovements("EXP-060")
	if len(movs) != 3 {
		t.Fatalf("movements: got %d, want 3", len(movs))
	}
	for i, want := range []string{"submit", "approve", "reimburse"} {
		if movs[i].Kind != want {
			t.Fatalf("movement %d: got %q, want %q", i, movs[i].Kind, want)
		}
	}
	if movs[2].Status != ExpenseClosed || movs[2].Actor != "ledger" {
		t.Fatalf("reimburse movement: %+v", movs[2])
	}
	alice := l.ExpensesByEmployee("alice")
	if len(alice) != 2 || alice[0].ID != "EXP-060" || alice[1].ID != "EXP-061" {
		t.Fatalf("alice claims: %+v", alice)
	}
	if got := l.ExpensesByEmployee("nobody"); len(got) != 0 {
		t.Fatalf("unknown employee listing: %+v", got)
	}
	if got := l.ExpenseMovements("EXP-404"); len(got) != 0 {
		t.Fatalf("unknown claim movements: %+v", got)
	}
}

func TestExpenseDefaultCurrency(t *testing.T) {
	l := newExpenseLedger(t)
	exp, err := l.SubmitExpense("EXP-070", "alice", 1000, "", "meals")
	if err != nil {
		t.Fatalf("SubmitExpense: %v", err)
	}
	if exp.Currency != DefaultCurrency {
		t.Fatalf("empty currency: got %q, want default %q", exp.Currency, DefaultCurrency)
	}
}
