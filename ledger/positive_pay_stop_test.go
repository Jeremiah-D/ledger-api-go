package ledger

import (
	"context"
	"errors"
	"testing"
)

// Positive-pay stop payment (LG-355): a lost/stolen check is stopped
// before presentment, so it enters the exception queue instead of
// clearing — and the stop never rewrites the register.

// positivePayStopAuditEvents returns the positive_pay_stop and
// positive_pay_stop_released events in order, closing the audit log
// first (like the other amend tests).
func positivePayStopAuditEvents(t *testing.T, al *AuditLog) []AuditEvent {
	t.Helper()
	var out []AuditEvent
	for _, e := range providerAuditEvents(t, al) {
		if e.Op == "positive_pay_stop" || e.Op == "positive_pay_stop_released" {
			out = append(out, e)
		}
	}
	return out
}

// newPositivePayStopTestLedger builds a ledger with an audit log. The
// caller must not use the audit log after positivePayStopAuditEvents
// closes it.
func newPositivePayStopTestLedger(t *testing.T) (*Ledger, *AuditLog) {
	t.Helper()
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { al.Close() }) // idempotent: safe after providerAuditEvents closes it
	return New(WithAuditLog(al)), al
}

func TestStopIssuedCheckUnknownCheckRefused(t *testing.T) {
	l := New()
	if err := l.StopIssuedCheck("checking", "9999"); !errors.Is(err, ErrPositivePayCheckNotRegistered) {
		t.Fatalf("StopIssuedCheck(unregistered) = %v, want ErrPositivePayCheckNotRegistered", err)
	}
	// The failed stop leaves nothing behind: registering the check
	// later and presenting it clears normally.
	mustRegisterCheck(t, l, issuedCheck("9999", 1000, "Acme"))
	res, err := l.PresentCheck(presentedCheck("9999", 1000, "Acme"))
	if err != nil || res.Disposition != PositivePayCleared {
		t.Fatalf("PresentCheck after refused stop: disposition = %q err = %v, want cleared", res.Disposition, err)
	}
}

func TestStopIssuedCheckClearedCheckRefused(t *testing.T) {
	l := New()
	mustRegisterCheck(t, l, issuedCheck("1001", 50000, "Acme Supplies"))
	res, err := l.PresentCheck(presentedCheck("1001", 50000, "Acme Supplies"))
	if err != nil || res.Disposition != PositivePayCleared {
		t.Fatalf("PresentCheck: disposition = %q err = %v, want cleared", res.Disposition, err)
	}
	if err := l.StopIssuedCheck("checking", "1001"); !errors.Is(err, ErrPositivePayCheckCleared) {
		t.Fatalf("StopIssuedCheck(cleared) = %v, want ErrPositivePayCheckCleared", err)
	}
}

func TestStoppedCheckPresentmentEntersException(t *testing.T) {
	l := New()
	mustRegisterCheck(t, l, issuedCheck("1001", 50000, "Acme Supplies"))
	if err := l.StopIssuedCheck("checking", "1001"); err != nil {
		t.Fatalf("StopIssuedCheck: %v", err)
	}
	// A full field match would clear unstopped — stopped, it must not.
	res, err := l.PresentCheck(presentedCheck("1001", 50000, "Acme Supplies"))
	if err != nil {
		t.Fatalf("PresentCheck: %v", err)
	}
	if res.Disposition != PositivePayDispositionException || res.Exception == nil {
		t.Fatalf("disposition = %q exception = %+v, want exception", res.Disposition, res.Exception)
	}
	if res.Exception.Reason != PositivePayReasonStopped || res.Exception.Status != PositivePayExceptionPending {
		t.Fatalf("exception = %+v, want reason stopped / status pending", res.Exception)
	}
	// A re-presentment returns the same pending exception, no duplicate.
	res2, err := l.PresentCheck(presentedCheck("1001", 50000, "Acme Supplies"))
	if err != nil || res2.Disposition != PositivePayDispositionException || res2.Exception == nil {
		t.Fatalf("re-present stopped: disposition = %q err = %v, want exception", res2.Disposition, err)
	}
	if res2.Exception != res.Exception {
		t.Fatalf("re-present returned a different exception object (want the same pending one)")
	}
	// The register entry is still uncleared.
	if l.issuedChecks[positivePayKey{account: "checking", number: "1001"}].cleared {
		t.Fatalf("stopped presentment marked the register entry cleared")
	}
}

func TestStoppedCheckDecidePayClears(t *testing.T) {
	l := New()
	mustRegisterCheck(t, l, issuedCheck("1001", 50000, "Acme Supplies"))
	if err := l.StopIssuedCheck("checking", "1001"); err != nil {
		t.Fatalf("StopIssuedCheck: %v", err)
	}
	if _, err := l.PresentCheck(presentedCheck("1001", 50000, "Acme Supplies")); err != nil {
		t.Fatalf("PresentCheck: %v", err)
	}
	if err := l.DecidePositivePayException("checking", "1001", PositivePayDecisionPay); err != nil {
		t.Fatalf("DecidePositivePayException(pay): %v", err)
	}
	res, err := l.PresentCheck(presentedCheck("1001", 50000, "Acme Supplies"))
	if err != nil || res.Disposition != PositivePayCleared {
		t.Fatalf("present after pay decision: disposition = %q err = %v, want cleared", res.Disposition, err)
	}
}

func TestStoppedCheckDecideReturnBounces(t *testing.T) {
	l := New()
	mustRegisterCheck(t, l, issuedCheck("1001", 50000, "Acme Supplies"))
	if err := l.StopIssuedCheck("checking", "1001"); err != nil {
		t.Fatalf("StopIssuedCheck: %v", err)
	}
	if _, err := l.PresentCheck(presentedCheck("1001", 50000, "Acme Supplies")); err != nil {
		t.Fatalf("PresentCheck: %v", err)
	}
	if err := l.DecidePositivePayException("checking", "1001", PositivePayDecisionReturn); err != nil {
		t.Fatalf("DecidePositivePayException(return): %v", err)
	}
	res, err := l.PresentCheck(presentedCheck("1001", 50000, "Acme Supplies"))
	if err != nil || res.Disposition != PositivePayReturned {
		t.Fatalf("present after return decision: disposition = %q err = %v, want returned", res.Disposition, err)
	}
}

func TestDuplicateStopIsNoOp(t *testing.T) {
	l, al := newPositivePayStopTestLedger(t)
	mustRegisterCheck(t, l, issuedCheck("1001", 50000, "Acme Supplies"))
	if err := l.StopIssuedCheck("checking", "1001"); err != nil {
		t.Fatalf("StopIssuedCheck: %v", err)
	}
	if err := l.StopIssuedCheckCtx(context.Background(), "checking", "1001"); err != nil {
		t.Fatalf("duplicate StopIssuedCheck = %v, want nil no-op", err)
	}
	events := positivePayStopAuditEvents(t, al)
	stops := 0
	for _, e := range events {
		if e.Op == "positive_pay_stop" {
			stops++
		}
	}
	if stops != 1 {
		t.Fatalf("positive_pay_stop events = %d, want 1 (duplicate stop is silent)", stops)
	}
}

func TestReleaseStopRestoresComparison(t *testing.T) {
	l := New()
	mustRegisterCheck(t, l, issuedCheck("1001", 50000, "Acme Supplies"))
	if err := l.StopIssuedCheck("checking", "1001"); err != nil {
		t.Fatalf("StopIssuedCheck: %v", err)
	}
	if err := l.ReleaseStopIssuedCheck("checking", "1001"); err != nil {
		t.Fatalf("ReleaseStopIssuedCheck: %v", err)
	}
	res, err := l.PresentCheck(presentedCheck("1001", 50000, "Acme Supplies"))
	if err != nil || res.Disposition != PositivePayCleared {
		t.Fatalf("present after release: disposition = %q err = %v, want cleared", res.Disposition, err)
	}
	// Releasing a check that is not stopped is a silent no-op.
	if err := l.ReleaseStopIssuedCheck("checking", "1001"); err != nil {
		t.Fatalf("second ReleaseStopIssuedCheck = %v, want nil no-op", err)
	}
}

func TestReleaseStopDoesNotClearPendingException(t *testing.T) {
	l := New()
	mustRegisterCheck(t, l, issuedCheck("1001", 50000, "Acme Supplies"))
	if err := l.StopIssuedCheck("checking", "1001"); err != nil {
		t.Fatalf("StopIssuedCheck: %v", err)
	}
	res, err := l.PresentCheck(presentedCheck("1001", 50000, "Acme Supplies"))
	if err != nil || res.Disposition != PositivePayDispositionException {
		t.Fatalf("present while stopped: disposition = %q err = %v, want exception", res.Disposition, err)
	}
	if err := l.ReleaseStopIssuedCheck("checking", "1001"); err != nil {
		t.Fatalf("ReleaseStopIssuedCheck: %v", err)
	}
	// The exception the stop produced is still pending: releasing the
	// stop must never silently clear it into a posting.
	res, err = l.PresentCheck(presentedCheck("1001", 50000, "Acme Supplies"))
	if err != nil || res.Disposition != PositivePayDispositionException || res.Exception == nil {
		t.Fatalf("present after release: disposition = %q err = %v, want the same pending exception", res.Disposition, err)
	}
	if res.Exception.Status != PositivePayExceptionPending || res.Exception.Reason != PositivePayReasonStopped {
		t.Fatalf("exception after release = %+v, want pending/stopped", res.Exception)
	}
}

func TestStopDoesNotModifyRegister(t *testing.T) {
	// While stopped, even a mismatched presentment reports the stop —
	// the stop takes precedence without touching the register.
	l := New()
	mustRegisterCheck(t, l, issuedCheck("1001", 50000, "Acme Supplies"))
	if err := l.StopIssuedCheck("checking", "1001"); err != nil {
		t.Fatalf("StopIssuedCheck: %v", err)
	}
	res, err := l.PresentCheck(presentedCheck("1001", 49999, "Acme Supplies"))
	if err != nil || res.Exception == nil || res.Exception.Reason != PositivePayReasonStopped {
		t.Fatalf("present mismatched while stopped: exception = %+v err = %v, want reason stopped", res.Exception, err)
	}
	// After release the register still compares on its original
	// fields: the same mismatch is an amount mismatch, and a full
	// match clears.
	l2 := New()
	mustRegisterCheck(t, l2, issuedCheck("1001", 50000, "Acme Supplies"))
	if err := l2.StopIssuedCheck("checking", "1001"); err != nil {
		t.Fatalf("StopIssuedCheck: %v", err)
	}
	if err := l2.ReleaseStopIssuedCheck("checking", "1001"); err != nil {
		t.Fatalf("ReleaseStopIssuedCheck: %v", err)
	}
	res, err = l2.PresentCheck(presentedCheck("1001", 49999, "Acme Supplies"))
	if err != nil || res.Exception == nil || res.Exception.Reason != PositivePayReasonAmountMismatch {
		t.Fatalf("present mismatched after release: exception = %+v err = %v, want reason amount_mismatch", res.Exception, err)
	}
	l3 := New()
	mustRegisterCheck(t, l3, issuedCheck("1001", 50000, "Acme Supplies"))
	if err := l3.StopIssuedCheck("checking", "1001"); err != nil {
		t.Fatalf("StopIssuedCheck: %v", err)
	}
	if err := l3.ReleaseStopIssuedCheck("checking", "1001"); err != nil {
		t.Fatalf("ReleaseStopIssuedCheck: %v", err)
	}
	res, err = l3.PresentCheck(presentedCheck("1001", 50000, "Acme Supplies"))
	if err != nil || res.Disposition != PositivePayCleared {
		t.Fatalf("present full match after release: disposition = %q err = %v, want cleared", res.Disposition, err)
	}
}

func TestStopAndReleaseAudit(t *testing.T) {
	l, al := newPositivePayStopTestLedger(t)
	mustRegisterCheck(t, l, issuedCheck("1001", 50000, "Acme Supplies"))
	ctx := ContextWithActor(context.Background(), "ops-ana")
	if err := l.StopIssuedCheckCtx(ctx, "checking", "1001"); err != nil {
		t.Fatalf("StopIssuedCheckCtx: %v", err)
	}
	if err := l.ReleaseStopIssuedCheckCtx(ctx, "checking", "1001"); err != nil {
		t.Fatalf("ReleaseStopIssuedCheckCtx: %v", err)
	}
	events := positivePayStopAuditEvents(t, al)
	if len(events) != 2 {
		t.Fatalf("stop/release events = %d, want 2", len(events))
	}
	if events[0].Op != "positive_pay_stop" || events[0].Details["check_number"] != "1001" {
		t.Fatalf("first event = %+v, want positive_pay_stop for 1001", events[0])
	}
	if events[1].Op != "positive_pay_stop_released" || events[1].Details["check_number"] != "1001" {
		t.Fatalf("second event = %+v, want positive_pay_stop_released for 1001", events[1])
	}
	for _, e := range events {
		if e.Actor != "ops-ana" {
			t.Fatalf("audit actor = %q, want %q (ctx attribution)", e.Actor, "ops-ana")
		}
		if len(e.Accounts) != 1 || e.Accounts[0] != "checking" {
			t.Fatalf("audit accounts = %v, want [checking]", e.Accounts)
		}
	}
}
