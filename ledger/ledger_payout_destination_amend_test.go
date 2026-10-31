package ledger

import (
	"errors"
	"reflect"
	"testing"
)

func TestAmendPayoutDestinationValidation(t *testing.T) {
	l := seedPayoutLedger(t)
	orig := draftPayout(t, l, "po-1", 100_000)

	// Unknown ID.
	if _, err := l.AmendPayoutDestination("nope", "FPS:6222000099999999"); !errors.Is(err, ErrPayoutNotFound) {
		t.Errorf("unknown: err = %v, want ErrPayoutNotFound", err)
	}

	// The new destination reuses the CreatePayout gate: an empty
	// destination is rejected, and the original is untouched.
	if _, err := l.AmendPayoutDestination("po-1", ""); !errors.Is(err, ErrEmptyPayoutDestination) {
		t.Errorf("empty: err = %v, want ErrEmptyPayoutDestination", err)
	}
	if got, _ := l.GetPayout("po-1"); !reflect.DeepEqual(got, orig) {
		t.Errorf("payout after empty destination = %+v, want %+v (untouched)", got, orig)
	}

	// With a registered external mapping (LG-122) for the payee under
	// FPS, a destination naming a different external account is
	// rejected — the amendment must not point a draft at an
	// unregistered account.
	if err := l.SetExternalMapping("saver", "FPS", "6222000012345678", "primary"); err != nil {
		t.Fatalf("SetExternalMapping: %v", err)
	}
	if _, err := l.AmendPayoutDestination("po-1", "FPS:6222000099999999"); !errors.Is(err, ErrPayoutUnknownDestination) {
		t.Errorf("mismatched mapping: err = %v, want ErrPayoutUnknownDestination", err)
	}
	if got, _ := l.GetPayout("po-1"); !reflect.DeepEqual(got, orig) {
		t.Errorf("payout after mismatched destination = %+v, want %+v (untouched)", got, orig)
	}

	// A destination under a system with no registered mapping is
	// free-form, exactly as at creation: no mapping, no constraint.
	if _, err := l.AmendPayoutDestination("po-1", "SWIFT:DEUTDEFFXXX"); err != nil {
		t.Errorf("unmapped system: err = %v, want nil", err)
	}
	// Restore the original destination for the remaining checks.
	if _, err := l.AmendPayoutDestination("po-1", "FPS:6222000012345678"); err != nil {
		t.Fatalf("restore: %v", err)
	}
}

func TestAmendPayoutDestinationStateGate(t *testing.T) {
	// Every non-draft status is rejected: approved, submitted, and
	// cancelled.
	t.Run("approved", func(t *testing.T) {
		l := seedPayoutLedger(t)
		draftPayout(t, l, "po-1", 100_000)
		if _, err := l.ApprovePayout("po-1", "bob"); err != nil {
			t.Fatalf("ApprovePayout: %v", err)
		}
		if _, err := l.AmendPayoutDestination("po-1", "FPS:6222000099999999"); !errors.Is(err, ErrPayoutInvalidTransition) {
			t.Errorf("err = %v, want ErrPayoutInvalidTransition", err)
		}
		if p, _ := l.GetPayout("po-1"); p.Destination != "FPS:6222000012345678" {
			t.Errorf("destination moved to %q after rejected amendment", p.Destination)
		}
	})
	t.Run("submitted", func(t *testing.T) {
		l := seedPayoutLedger(t)
		draftPayout(t, l, "po-1", 100_000)
		approveSubmitPayout(t, l, "po-1", "bob")
		if _, err := l.AmendPayoutDestination("po-1", "FPS:6222000099999999"); !errors.Is(err, ErrPayoutInvalidTransition) {
			t.Errorf("err = %v, want ErrPayoutInvalidTransition", err)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		l := seedPayoutLedger(t)
		draftPayout(t, l, "po-1", 100_000)
		if _, err := l.CancelPayout("po-1"); err != nil {
			t.Fatalf("CancelPayout: %v", err)
		}
		if _, err := l.AmendPayoutDestination("po-1", "FPS:6222000099999999"); !errors.Is(err, ErrPayoutInvalidTransition) {
			t.Errorf("err = %v, want ErrPayoutInvalidTransition", err)
		}
	})

	// The state gate runs before the destination validation and the
	// no-op check: even a same-value or empty amendment is rejected
	// on a departed instruction.
	l := seedPayoutLedger(t)
	draftPayout(t, l, "po-1", 100_000)
	if _, err := l.ApprovePayout("po-1", "bob"); err != nil {
		t.Fatalf("ApprovePayout: %v", err)
	}
	if _, err := l.AmendPayoutDestination("po-1", "FPS:6222000012345678"); !errors.Is(err, ErrPayoutInvalidTransition) {
		t.Errorf("approved, same destination: err = %v, want ErrPayoutInvalidTransition", err)
	}
	if _, err := l.AmendPayoutDestination("po-1", ""); !errors.Is(err, ErrPayoutInvalidTransition) {
		t.Errorf("approved, empty destination: err = %v, want ErrPayoutInvalidTransition", err)
	}
}

func TestAmendPayoutDestinationNoOp(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithPayoutClearingAccount("payout-clearing"))
	seedBalance(t, l, "saver", 1_000_000)
	orig := draftPayout(t, l, "po-1", 100_000)

	// Amending to the current destination is a no-op, not a change.
	if got, err := l.AmendPayoutDestination("po-1", "FPS:6222000012345678"); err != nil || !reflect.DeepEqual(got, orig) {
		t.Errorf("same-destination amendment = %+v, %v, want %+v, nil", got, err, orig)
	}

	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// The no-op writes no audit event.
	if events := auditEventsByOp(t, dir)["payout_destination_amended"]; len(events) != 0 {
		t.Errorf("amend events = %d, want 0 (no-op is silent)", len(events))
	}
}

func TestAmendPayoutDestinationSuccess(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithPayoutClearingAccount("payout-clearing"))
	seedBalance(t, l, "saver", 1_000_000)
	orig := draftPayout(t, l, "po-1", 100_000)
	versionBefore := l.Version()

	amended, err := l.AmendPayoutDestination("po-1", "FPS:6222000099999999")
	if err != nil {
		t.Fatalf("AmendPayoutDestination: %v", err)
	}
	if amended.Destination != "FPS:6222000099999999" {
		t.Errorf("amended destination = %q, want FPS:6222000099999999", amended.Destination)
	}
	// Only the destination moves: amount, payee, currency, maker,
	// status, UETR, and idempotency key are untouched.
	amended.Destination = orig.Destination
	if !reflect.DeepEqual(amended, orig) {
		t.Errorf("amended payout = %+v, want %+v except destination", amended, orig)
	}
	// Off-journal: the ledger version does not move.
	if got := l.Version(); got != versionBefore {
		t.Errorf("version after amendment = %d, want %d (unchanged)", got, versionBefore)
	}
	// The registry carries the new destination, still a draft.
	stored, ok := l.GetPayout("po-1")
	if !ok || stored.Destination != "FPS:6222000099999999" || stored.Status != PayoutDraft {
		t.Errorf("stored payout = %+v,%v, want draft with new destination", stored, ok)
	}
	// The approver reviews the amended destination: approval still
	// works, and the approved instruction carries the new destination.
	approved, err := l.ApprovePayout("po-1", "bob")
	if err != nil {
		t.Fatalf("ApprovePayout after amendment: %v", err)
	}
	if approved.Destination != "FPS:6222000099999999" {
		t.Errorf("approved destination = %q, want amended destination", approved.Destination)
	}

	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["payout_destination_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.TraceID != "po-1" || ev.Actor != "AmendPayoutDestination" {
		t.Errorf("amend event = trace %q actor %q, want po-1/AmendPayoutDestination", ev.TraceID, ev.Actor)
	}
	if ev.Details["previous_destination"] != "FPS:6222000012345678" || ev.Details["destination"] != "FPS:6222000099999999" {
		t.Errorf("amend event details = %v, want previous FPS:6222000012345678 / new FPS:6222000099999999", ev.Details)
	}
	if ev.VersionBefore != versionBefore || ev.VersionAfter != versionBefore {
		t.Errorf("amend event versions = %d->%d, want %d->%d", ev.VersionBefore, ev.VersionAfter, versionBefore, versionBefore)
	}
}
