package ledger

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestAmendPayoutPayeeAccountValidation(t *testing.T) {
	l := seedPayoutLedger(t)
	orig := draftPayout(t, l, "po-1", 100_000)

	// Unknown ID.
	if _, err := l.AmendPayoutPayeeAccount("nope", "spender"); !errors.Is(err, ErrPayoutNotFound) {
		t.Errorf("unknown: err = %v, want ErrPayoutNotFound", err)
	}

	// The new payee reuses the CreatePayout gate: an empty payee is
	// rejected, and the original payee is untouched.
	if _, err := l.AmendPayoutPayeeAccount("po-1", ""); !errors.Is(err, ErrEmptyPayoutAccount) {
		t.Errorf("empty payee: err = %v, want ErrEmptyPayoutAccount", err)
	}
	if got, _ := l.GetPayout("po-1"); !reflect.DeepEqual(got, orig) {
		t.Errorf("payout after empty payee = %+v, want %+v (untouched)", got, orig)
	}
}

func TestAmendPayoutPayeeAccountDestinationRevalidation(t *testing.T) {
	l := seedPayoutLedger(t)
	draftPayout(t, l, "po-1", 100_000)

	// A payee with no registered external mapping accepts the live
	// destination: nothing is registered to contradict it.
	if _, err := l.AmendPayoutPayeeAccount("po-1", "unmapped"); err != nil {
		t.Fatalf("amend to unmapped payee: %v", err)
	}
	if p, _ := l.GetPayout("po-1"); p.PayeeAccount != "unmapped" {
		t.Fatalf("payee = %q, want unmapped", p.PayeeAccount)
	}

	// Move it back, then register a conflicting mapping for the new
	// payee: the live destination names a different external account
	// under the new payee, so the amendment is rejected and the
	// original payee is untouched.
	if _, err := l.AmendPayoutPayeeAccount("po-1", "saver"); err != nil {
		t.Fatalf("amend back to saver: %v", err)
	}
	if err := l.SetExternalMapping("spender", "FPS", "6222000099999999", "primary"); err != nil {
		t.Fatalf("SetExternalMapping: %v", err)
	}
	if _, err := l.AmendPayoutPayeeAccount("po-1", "spender"); !errors.Is(err, ErrPayoutUnknownDestination) {
		t.Errorf("conflicting mapping: err = %v, want ErrPayoutUnknownDestination", err)
	}
	if p, _ := l.GetPayout("po-1"); p.PayeeAccount != "saver" {
		t.Errorf("payee = %q after rejected amendment, want saver (untouched)", p.PayeeAccount)
	}

	// Registering the live destination under the new payee clears the
	// gate: the amendment succeeds.
	if err := l.SetExternalMapping("spender", "FPS", "6222000012345678", "primary"); err != nil {
		t.Fatalf("SetExternalMapping: %v", err)
	}
	if _, err := l.AmendPayoutPayeeAccount("po-1", "spender"); err != nil {
		t.Errorf("matching mapping: err = %v, want nil", err)
	}
}

func TestAmendPayoutPayeeAccountStateGate(t *testing.T) {
	// Every non-draft status is rejected: approved, submitted,
	// settled, failed, cancelled, and expired.
	t.Run("approved", func(t *testing.T) {
		l := seedPayoutLedger(t)
		draftPayout(t, l, "po-1", 100_000)
		if _, err := l.ApprovePayout("po-1", "bob"); err != nil {
			t.Fatalf("ApprovePayout: %v", err)
		}
		if _, err := l.AmendPayoutPayeeAccount("po-1", "spender"); !errors.Is(err, ErrPayoutInvalidTransition) {
			t.Errorf("err = %v, want ErrPayoutInvalidTransition", err)
		}
		if p, _ := l.GetPayout("po-1"); p.PayeeAccount != "saver" {
			t.Errorf("payee moved to %q after rejected amendment", p.PayeeAccount)
		}
	})
	t.Run("submitted", func(t *testing.T) {
		l := seedPayoutLedger(t)
		draftPayout(t, l, "po-1", 100_000)
		approveSubmitPayout(t, l, "po-1", "bob")
		if _, err := l.AmendPayoutPayeeAccount("po-1", "spender"); !errors.Is(err, ErrPayoutInvalidTransition) {
			t.Errorf("err = %v, want ErrPayoutInvalidTransition", err)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		l := seedPayoutLedger(t)
		draftPayout(t, l, "po-1", 100_000)
		if _, err := l.CancelPayout("po-1"); err != nil {
			t.Fatalf("CancelPayout: %v", err)
		}
		if _, err := l.AmendPayoutPayeeAccount("po-1", "spender"); !errors.Is(err, ErrPayoutInvalidTransition) {
			t.Errorf("err = %v, want ErrPayoutInvalidTransition", err)
		}
	})
	t.Run("expired", func(t *testing.T) {
		l := seedPayoutLedger(t)
		draftPayoutWithExpiry(t, l, "po-1", 100_000, time.Now().Add(time.Hour))
		if _, err := l.ApprovePayout("po-1", "bob"); err != nil {
			t.Fatalf("ApprovePayout: %v", err)
		}
		if expired := l.ExpirePayoutsAt(time.Now().Add(2 * time.Hour)); len(expired) != 1 {
			t.Fatalf("ExpirePayoutsAt expired %v, want [po-1]", expired)
		}
		if _, err := l.AmendPayoutPayeeAccount("po-1", "spender"); !errors.Is(err, ErrPayoutInvalidTransition) {
			t.Errorf("err = %v, want ErrPayoutInvalidTransition", err)
		}
	})

	// The state gate runs before the payee validation and the no-op
	// check: even a same-value or empty-payee amendment is rejected
	// on a departed instruction.
	l := seedPayoutLedger(t)
	draftPayout(t, l, "po-1", 100_000)
	if _, err := l.ApprovePayout("po-1", "bob"); err != nil {
		t.Fatalf("ApprovePayout: %v", err)
	}
	if _, err := l.AmendPayoutPayeeAccount("po-1", "saver"); !errors.Is(err, ErrPayoutInvalidTransition) {
		t.Errorf("approved, same payee: err = %v, want ErrPayoutInvalidTransition", err)
	}
	if _, err := l.AmendPayoutPayeeAccount("po-1", ""); !errors.Is(err, ErrPayoutInvalidTransition) {
		t.Errorf("approved, empty payee: err = %v, want ErrPayoutInvalidTransition", err)
	}
}

func TestAmendPayoutPayeeAccountNoOp(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithPayoutClearingAccount("payout-clearing"))
	seedBalance(t, l, "saver", 1_000_000)
	orig := draftPayout(t, l, "po-1", 100_000)

	// Amending to the current payee is a no-op, not a change.
	if got, err := l.AmendPayoutPayeeAccount("po-1", "saver"); err != nil || !reflect.DeepEqual(got, orig) {
		t.Errorf("same-payee amendment = %+v, %v, want %+v, nil", got, err, orig)
	}

	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// The no-op writes no audit event.
	if events := auditEventsByOp(t, dir)["payout_payee_account_amended"]; len(events) != 0 {
		t.Errorf("amend events = %d, want 0 (no-op is silent)", len(events))
	}
}

func TestAmendPayoutPayeeAccountSuccess(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithPayoutClearingAccount("payout-clearing"))
	seedBalance(t, l, "saver", 1_000_000)
	seedBalance(t, l, "spender", 1_000_000)
	orig := draftPayout(t, l, "po-1", 100_000)
	versionBefore := l.Version()

	amended, err := l.AmendPayoutPayeeAccount("po-1", "spender")
	if err != nil {
		t.Fatalf("AmendPayoutPayeeAccount: %v", err)
	}
	if amended.PayeeAccount != "spender" {
		t.Errorf("amended payee = %q, want spender", amended.PayeeAccount)
	}
	// Only the payee moves: amount, currency, destination, maker,
	// status, UETR, and idempotency key are untouched.
	amended.PayeeAccount = orig.PayeeAccount
	if !reflect.DeepEqual(amended, orig) {
		t.Errorf("amended payout = %+v, want %+v except payee", amended, orig)
	}
	// Off-journal: the ledger version does not move.
	if got := l.Version(); got != versionBefore {
		t.Errorf("version after amendment = %d, want %d (unchanged)", got, versionBefore)
	}
	// The registry carries the new payee, still a draft.
	stored, ok := l.GetPayout("po-1")
	if !ok || stored.PayeeAccount != "spender" || stored.Status != PayoutDraft {
		t.Errorf("stored payout = %+v,%v, want draft with payee spender", stored, ok)
	}
	// The amendment emits no outbox event: the draft was already
	// reported when created, and the amend path never calls
	// emitOutboxLocked (verified by inspection of the module).

	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["payout_payee_account_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.TraceID != "po-1" || ev.Actor != "AmendPayoutPayeeAccount" {
		t.Errorf("amend event = trace %q actor %q, want po-1/AmendPayoutPayeeAccount", ev.TraceID, ev.Actor)
	}
	if ev.Details["previous_payee_account"] != "saver" || ev.Details["payee_account"] != "spender" {
		t.Errorf("amend event details = %v, want previous saver / new spender", ev.Details)
	}
	if len(ev.Accounts) != 2 || ev.Accounts[0] != "saver" || ev.Accounts[1] != "spender" {
		t.Errorf("amend event accounts = %v, want [saver spender]", ev.Accounts)
	}
	if ev.VersionBefore != versionBefore || ev.VersionAfter != versionBefore {
		t.Errorf("amend event versions = %d->%d, want %d->%d", ev.VersionBefore, ev.VersionAfter, versionBefore, versionBefore)
	}
}

func TestAmendPayoutPayeeAccountMakerCheckerReadsLivePayee(t *testing.T) {
	// The four-eyes threshold reads the live payee at approval time:
	// a draft on an unthresholded account that is amended onto a
	// thresholded one newly requires a checker who differs from the
	// maker, and one amended off it newly lets the maker self-approve.
	l := seedPayoutLedger(t)
	if err := l.SetReviewThreshold("spender", 200_000); err != nil {
		t.Fatalf("SetReviewThreshold: %v", err)
	}
	draftPayout(t, l, "po-1", 100_000)
	draftPayout(t, l, "po-2", 300_000)
	if _, err := l.AmendPayoutPayeeAccount("po-2", "spender"); err != nil {
		t.Fatalf("AmendPayoutPayeeAccount po-2: %v", err)
	}

	// po-1 stays on saver (no threshold): the maker may self-approve.
	if _, err := l.ApprovePayout("po-1", "alice"); err != nil {
		t.Errorf("unthresholded payee, maker approve: err = %v, want nil", err)
	}
	// po-2 moved onto spender (threshold 200000, amount 300000):
	// the maker may no longer self-approve.
	if _, err := l.ApprovePayout("po-2", "alice"); !errors.Is(err, ErrPayoutMakerCheckerViolation) {
		t.Errorf("amended onto thresholded payee, maker approve: err = %v, want ErrPayoutMakerCheckerViolation", err)
	}
	if _, err := l.ApprovePayout("po-2", "bob"); err != nil {
		t.Errorf("amended onto thresholded payee, checker approve: err = %v, want nil", err)
	}

	// Moving a draft off the thresholded account lets the maker
	// self-approve again.
	draftPayout(t, l, "po-3", 300_000)
	if _, err := l.AmendPayoutPayeeAccount("po-3", "spender"); err != nil {
		t.Fatalf("AmendPayoutPayeeAccount po-3: %v", err)
	}
	if _, err := l.AmendPayoutPayeeAccount("po-3", "saver"); err != nil {
		t.Fatalf("AmendPayoutPayeeAccount po-3 back: %v", err)
	}
	if _, err := l.ApprovePayout("po-3", "alice"); err != nil {
		t.Errorf("amended off thresholded payee, maker approve: err = %v, want nil", err)
	}
}

func TestAmendPayoutPayeeAccountSubmitReadsLivePayee(t *testing.T) {
	// The clearing leg journals against the live payee: amending the
	// payee before approval moves the earmark to the new account.
	l := seedPayoutLedger(t)
	seedBalance(t, l, "spender", 1_000_000)
	draftPayout(t, l, "po-1", 100_000)
	if _, err := l.AmendPayoutPayeeAccount("po-1", "spender"); err != nil {
		t.Fatalf("AmendPayoutPayeeAccount: %v", err)
	}
	approveSubmitPayout(t, l, "po-1", "bob")
	if b := l.Balance("spender"); b != 900_000 {
		t.Errorf("spender balance = %d, want 900000 (earmark left the new payee)", b)
	}
	if b := l.Balance("saver"); b != 1_000_000 {
		t.Errorf("saver balance = %d, want 1000000 (untouched)", b)
	}
	if b := l.Balance("payout-clearing"); b != 100_000 {
		t.Errorf("clearing balance = %d, want 100000", b)
	}
}
