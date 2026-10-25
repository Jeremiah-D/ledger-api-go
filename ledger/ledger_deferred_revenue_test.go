package ledger

import (
	"errors"
	"testing"
	"time"
)

func newDeferredRevenueLedger(t *testing.T) *Ledger {
	t.Helper()
	return New(WithDeferredRevenueConfig(DeferredRevenueConfig{
		CashAccount:      "cash-operating",
		LiabilityAccount: "deferred-revenue",
		RevenueAccount:   "revenue-subscriptions",
	}))
}

func TestDeferredRevenueStraightLineTrueUp(t *testing.T) {
	l := newDeferredRevenueLedger(t)
	contract, err := l.ReceiveDeferredRevenue("acme", 10001, "USD", 3)
	if err != nil {
		t.Fatalf("ReceiveDeferredRevenue: %v", err)
	}
	if contract.ID == "" || contract.Status != DeferredActive || contract.RemainingCents != 10001 {
		t.Fatalf("received contract: %+v", contract)
	}
	// Receipt books cash against the liability only: revenue is untouched.
	if got := l.BalanceIn("revenue-subscriptions", "USD"); got != 0 {
		t.Fatalf("revenue balance after receipt: got %d, want 0", got)
	}
	if got := l.BalanceIn("cash-operating", "USD"); got != 10001 {
		t.Fatalf("cash balance after receipt: got %d, want 10001", got)
	}
	if got := l.BalanceIn("deferred-revenue", "USD"); got != -10001 {
		t.Fatalf("liability balance after receipt: got %d, want -10001", got)
	}

	// Periods 1-2 recognize floor(10001/3) = 3333 each.
	posted, err := l.RecognizeRevenue(contract.ID, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("RecognizeRevenue period 1: %v", err)
	}
	if posted.AmountCents != 3333 || posted.DebitAccount != "deferred-revenue" || posted.CreditAccount != "revenue-subscriptions" {
		t.Fatalf("period 1 entry: %+v", posted)
	}
	posted, err = l.RecognizeRevenue(contract.ID, time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("RecognizeRevenue period 2: %v", err)
	}
	if posted.AmountCents != 3333 {
		t.Fatalf("period 2 amount: got %d, want 3333", posted.AmountCents)
	}
	// Final period true-ups to the remainder: 10001 - 6666 = 3335.
	posted, err = l.RecognizeRevenue(contract.ID, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("RecognizeRevenue period 3: %v", err)
	}
	if posted.AmountCents != 3335 {
		t.Fatalf("final period amount: got %d, want 3335 (true-up)", posted.AmountCents)
	}
	done, ok := l.DeferredRevenue(contract.ID)
	if !ok || done.Status != DeferredCompleted || done.RecognizedCents != 10001 || done.RemainingCents != 0 {
		t.Fatalf("completed contract: %+v ok=%v", done, ok)
	}
	if got := l.BalanceIn("deferred-revenue", "USD"); got != 0 {
		t.Fatalf("liability balance after completion: got %d, want 0", got)
	}
	if got := l.BalanceIn("revenue-subscriptions", "USD"); got != -10001 {
		t.Fatalf("revenue balance after completion: got %d, want -10001", got)
	}
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Fatalf("VerifyAccountingEquation: %v", err)
	}
	// Recognizing a completed contract is refused, and books nothing.
	if _, err := l.RecognizeRevenue(contract.ID, time.Now()); !errors.Is(err, ErrDeferredRevenueInvalidTransition) {
		t.Fatalf("recognize completed: got %v, want ErrDeferredRevenueInvalidTransition", err)
	}
	movements := l.DeferredRevenueMovements(contract.ID)
	if len(movements) != 4 || movements[0].Kind != "receive" || movements[3].Kind != "recognize" {
		t.Fatalf("movements: %+v", movements)
	}
}

func TestDeferredRevenueTerminateRefundAndRecognize(t *testing.T) {
	l := newDeferredRevenueLedger(t)
	refunded, err := l.ReceiveDeferredRevenue("beta", 12000, "USD", 4)
	if err != nil {
		t.Fatalf("ReceiveDeferredRevenue: %v", err)
	}
	if _, err := l.RecognizeRevenue(refunded.ID, time.Now()); err != nil {
		t.Fatalf("RecognizeRevenue: %v", err)
	}
	// Empty mode defaults to refund: the remaining 9000 goes back to cash.
	posted, err := l.TerminateDeferredRevenue(refunded.ID, "")
	if err != nil {
		t.Fatalf("TerminateDeferredRevenue refund: %v", err)
	}
	if posted.AmountCents != 9000 || posted.DebitAccount != "deferred-revenue" || posted.CreditAccount != "cash-operating" {
		t.Fatalf("refund entry: %+v", posted)
	}
	settled, _ := l.DeferredRevenue(refunded.ID)
	if settled.Status != DeferredRefunded || settled.RemainingCents != 0 || settled.RecognizedCents != 3000 {
		t.Fatalf("refunded contract: %+v", settled)
	}
	if got := l.BalanceIn("cash-operating", "USD"); got != 3000 {
		t.Fatalf("cash after refund: got %d, want 3000 (12000 - 9000)", got)
	}
	if _, err := l.TerminateDeferredRevenue(refunded.ID, "refund"); !errors.Is(err, ErrDeferredRevenueInvalidTransition) {
		t.Fatalf("second termination: got %v, want ErrDeferredRevenueInvalidTransition", err)
	}

	kept, err := l.ReceiveDeferredRevenue("gamma", 5000, "USD", 2)
	if err != nil {
		t.Fatalf("ReceiveDeferredRevenue: %v", err)
	}
	posted, err = l.TerminateDeferredRevenue(kept.ID, "recognize")
	if err != nil {
		t.Fatalf("TerminateDeferredRevenue recognize: %v", err)
	}
	if posted.AmountCents != 5000 || posted.CreditAccount != "revenue-subscriptions" {
		t.Fatalf("recognize-termination entry: %+v", posted)
	}
	settled, _ = l.DeferredRevenue(kept.ID)
	if settled.Status != DeferredRecognized || settled.RecognizedCents != 5000 || settled.RemainingCents != 0 {
		t.Fatalf("recognized contract: %+v", settled)
	}
}

func TestDeferredRevenueValidationAndFailClosed(t *testing.T) {
	l := newDeferredRevenueLedger(t)
	if _, err := l.ReceiveDeferredRevenue("", 1000, "USD", 2); !errors.Is(err, ErrInvalidDeferredCustomer) {
		t.Fatalf("empty customer: got %v, want ErrInvalidDeferredCustomer", err)
	}
	if _, err := l.ReceiveDeferredRevenue("acme", 0, "USD", 2); !errors.Is(err, ErrInvalidDeferredAmount) {
		t.Fatalf("zero amount: got %v, want ErrInvalidDeferredAmount", err)
	}
	if _, err := l.ReceiveDeferredRevenue("acme", 1000, "USD", 0); !errors.Is(err, ErrInvalidDeferredPeriods) {
		t.Fatalf("zero periods: got %v, want ErrInvalidDeferredPeriods", err)
	}
	if _, err := l.RecognizeRevenue("defrev-9999", time.Now()); !errors.Is(err, ErrDeferredRevenueNotFound) {
		t.Fatalf("unknown contract: got %v, want ErrDeferredRevenueNotFound", err)
	}
	contract, err := l.ReceiveDeferredRevenue("acme", 1000, "USD", 1)
	if err != nil {
		t.Fatalf("ReceiveDeferredRevenue: %v", err)
	}
	if _, err := l.TerminateDeferredRevenue(contract.ID, "keep"); !errors.Is(err, ErrInvalidDeferredTerminateMode) {
		t.Fatalf("bad terminate mode: got %v, want ErrInvalidDeferredTerminateMode", err)
	}
	// A single-period contract completes on its first recognition.
	if _, err := l.RecognizeRevenue(contract.ID, time.Now()); err != nil {
		t.Fatalf("RecognizeRevenue single period: %v", err)
	}
	if done, _ := l.DeferredRevenue(contract.ID); done.Status != DeferredCompleted {
		t.Fatalf("single-period contract: %+v", done)
	}

	// No config, no booking: receipt is fail-closed.
	bare := New()
	if _, err := bare.ReceiveDeferredRevenue("acme", 1000, "USD", 2); !errors.Is(err, ErrDeferredRevenueAccountsNotConfigured) {
		t.Fatalf("unconfigured receipt: got %v, want ErrDeferredRevenueAccountsNotConfigured", err)
	}
	if err := bare.SetDeferredRevenueConfig(DeferredRevenueConfig{
		CashAccount: "cash", LiabilityAccount: "cash", RevenueAccount: "rev",
	}); !errors.Is(err, ErrDeferredRevenueSameAccount) {
		t.Fatalf("duplicate accounts: got %v, want ErrDeferredRevenueSameAccount", err)
	}
}
