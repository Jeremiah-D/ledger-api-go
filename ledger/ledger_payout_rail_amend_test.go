package ledger

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestAmendPayoutRailValidation(t *testing.T) {
	l := seedPayoutLedger(t)
	orig := draftPayoutWithRail(t, l, "po-1", 100_000, "swift")
	if orig.Rail != "swift" {
		t.Fatalf("draft rail = %q, want swift", orig.Rail)
	}

	// Unknown ID.
	if _, err := l.AmendPayoutRail("nope", "sepa"); !errors.Is(err, ErrPayoutNotFound) {
		t.Errorf("unknown: err = %v, want ErrPayoutNotFound", err)
	}

	// A rail outside the SLA vocabulary is rejected fail-closed
	// (normalizeRail, sla.go), and the original rail is untouched.
	for _, bad := range []string{"zelle", "fedwire", "x", "swift2"} {
		if _, err := l.AmendPayoutRail("po-1", bad); !errors.Is(err, ErrInvalidRail) {
			t.Errorf("rail %q: err = %v, want ErrInvalidRail", bad, err)
		}
		if got, _ := l.GetPayout("po-1"); !reflect.DeepEqual(got, orig) {
			t.Errorf("payout after rail %q = %+v, want %+v (untouched)", bad, got, orig)
		}
	}

	// The new rail is canonicalized like at draft time: case and
	// surrounding whitespace do not survive.
	if got, err := l.AmendPayoutRail("po-1", "  FedNow "); err != nil || got.Rail != "fednow" {
		t.Errorf("amend to \"  FedNow \" = %+v, %v, want rail fednow", got, err)
	}
	if got, err := l.AmendPayoutRail("po-1", "SEPA"); err != nil || got.Rail != "sepa" {
		t.Errorf("amend to \"SEPA\" = %+v, %v, want rail sepa", got, err)
	}

	// Empty clears the rail back to unspecified: the next submit then
	// defaults to gpi, as a rail-less draft would.
	if got, err := l.AmendPayoutRail("po-1", ""); err != nil || got.Rail != "" {
		t.Errorf("amend to empty = %+v, %v, want rail empty", got, err)
	}
}

func TestAmendPayoutRailStateGate(t *testing.T) {
	// Every non-draft status is rejected: approved, submitted,
	// cancelled, and expired.
	t.Run("approved", func(t *testing.T) {
		l := seedPayoutLedger(t)
		draftPayoutWithRail(t, l, "po-1", 100_000, "swift")
		if _, err := l.ApprovePayout("po-1", "bob"); err != nil {
			t.Fatalf("ApprovePayout: %v", err)
		}
		if _, err := l.AmendPayoutRail("po-1", "sepa"); !errors.Is(err, ErrPayoutInvalidTransition) {
			t.Errorf("err = %v, want ErrPayoutInvalidTransition", err)
		}
		if p, _ := l.GetPayout("po-1"); p.Rail != "swift" {
			t.Errorf("rail moved to %q after rejected amendment", p.Rail)
		}
	})
	t.Run("submitted", func(t *testing.T) {
		l := seedPayoutLedger(t)
		draftPayoutWithRail(t, l, "po-1", 100_000, "swift")
		approveSubmitPayout(t, l, "po-1", "bob")
		if _, err := l.AmendPayoutRail("po-1", "sepa"); !errors.Is(err, ErrPayoutInvalidTransition) {
			t.Errorf("err = %v, want ErrPayoutInvalidTransition", err)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		l := seedPayoutLedger(t)
		draftPayoutWithRail(t, l, "po-1", 100_000, "swift")
		if _, err := l.CancelPayout("po-1"); err != nil {
			t.Fatalf("CancelPayout: %v", err)
		}
		if _, err := l.AmendPayoutRail("po-1", "sepa"); !errors.Is(err, ErrPayoutInvalidTransition) {
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
		if _, err := l.AmendPayoutRail("po-1", "sepa"); !errors.Is(err, ErrPayoutInvalidTransition) {
			t.Errorf("err = %v, want ErrPayoutInvalidTransition", err)
		}
	})

	// The state gate runs before the rail validation and the no-op
	// check: even a same-value or illegal amendment is rejected on a
	// departed instruction.
	l := seedPayoutLedger(t)
	draftPayoutWithRail(t, l, "po-1", 100_000, "swift")
	if _, err := l.ApprovePayout("po-1", "bob"); err != nil {
		t.Fatalf("ApprovePayout: %v", err)
	}
	if _, err := l.AmendPayoutRail("po-1", "swift"); !errors.Is(err, ErrPayoutInvalidTransition) {
		t.Errorf("approved, same rail: err = %v, want ErrPayoutInvalidTransition", err)
	}
	if _, err := l.AmendPayoutRail("po-1", "zelle"); !errors.Is(err, ErrPayoutInvalidTransition) {
		t.Errorf("approved, illegal rail: err = %v, want ErrPayoutInvalidTransition", err)
	}
}

func TestAmendPayoutRailNoOp(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithPayoutClearingAccount("payout-clearing"))
	seedBalance(t, l, "saver", 1_000_000)
	orig := draftPayoutWithRail(t, l, "po-1", 100_000, "swift")

	// Amending to the current rail is a no-op, not a change — and
	// the comparison runs on the normalized rail, so a case variant
	// of the same rail is a no-op too.
	if got, err := l.AmendPayoutRail("po-1", "swift"); err != nil || !reflect.DeepEqual(got, orig) {
		t.Errorf("same-rail amendment = %+v, %v, want %+v, nil", got, err, orig)
	}
	if got, err := l.AmendPayoutRail("po-1", "SWIFT"); err != nil || !reflect.DeepEqual(got, orig) {
		t.Errorf("case-variant amendment = %+v, %v, want %+v, nil", got, err, orig)
	}

	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// The no-op writes no audit event.
	if events := auditEventsByOp(t, dir)["payout_rail_amended"]; len(events) != 0 {
		t.Errorf("amend events = %d, want 0 (no-op is silent)", len(events))
	}
}

func TestAmendPayoutRailSuccess(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithPayoutClearingAccount("payout-clearing"))
	seedBalance(t, l, "saver", 1_000_000)
	orig := draftPayoutWithRail(t, l, "po-1", 100_000, "swift")
	versionBefore := l.Version()

	amended, err := l.AmendPayoutRail("po-1", "sepa")
	if err != nil {
		t.Fatalf("AmendPayoutRail: %v", err)
	}
	if amended.Rail != "sepa" {
		t.Errorf("amended rail = %q, want sepa", amended.Rail)
	}
	// Only the rail moves: payee, amount, currency, destination,
	// maker, status, UETR, and purpose code are untouched.
	amended.Rail = orig.Rail
	if !reflect.DeepEqual(amended, orig) {
		t.Errorf("amended payout = %+v, want %+v except rail", amended, orig)
	}
	// Off-journal: the ledger version does not move.
	if got := l.Version(); got != versionBefore {
		t.Errorf("version after amendment = %d, want %d (unchanged)", got, versionBefore)
	}
	// The registry carries the new rail, still a draft.
	stored, ok := l.GetPayout("po-1")
	if !ok || stored.Rail != "sepa" || stored.Status != PayoutDraft {
		t.Errorf("stored payout = %+v,%v, want draft with rail sepa", stored, ok)
	}
	// The amendment emits no outbox event: the draft was already
	// reported when created, and the amend path never calls
	// emitOutboxLocked (verified by inspection of the module).

	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["payout_rail_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.TraceID != "po-1" || ev.Actor != "AmendPayoutRail" {
		t.Errorf("amend event = trace %q actor %q, want po-1/AmendPayoutRail", ev.TraceID, ev.Actor)
	}
	if ev.Details["previous_rail"] != "swift" || ev.Details["rail"] != "sepa" {
		t.Errorf("amend event details = %v, want previous swift / new sepa", ev.Details)
	}
	if ev.VersionBefore != versionBefore || ev.VersionAfter != versionBefore {
		t.Errorf("amend event versions = %d->%d, want %d->%d", ev.VersionBefore, ev.VersionAfter, versionBefore, versionBefore)
	}
}

func TestAmendPayoutRailEffectiveAtSubmit(t *testing.T) {
	// SubmitPayout reads the live rail when it opens the SLA watch:
	// a draft amended swift -> sepa submits with the watch on sepa.
	l := seedPayoutLedger(t)
	draftPayoutWithRail(t, l, "po-1", 100_000, "swift")
	if _, err := l.AmendPayoutRail("po-1", "sepa"); err != nil {
		t.Fatalf("AmendPayoutRail: %v", err)
	}
	if _, err := l.ApprovePayout("po-1", "bob"); err != nil {
		t.Fatalf("ApprovePayout: %v", err)
	}
	submitted, err := l.SubmitPayout("po-1")
	if err != nil {
		t.Fatalf("SubmitPayout: %v", err)
	}
	if submitted.Rail != "sepa" {
		t.Errorf("submitted rail = %q, want sepa", submitted.Rail)
	}
	watch, ok := l.SLAWatch("po-1")
	if !ok {
		t.Fatalf("no SLA watch opened for po-1")
	}
	if watch.Rail != "sepa" {
		t.Errorf("SLA watch rail = %q, want sepa", watch.Rail)
	}
}

func TestAmendPayoutRailClearedDefaultsToGPIAtSubmit(t *testing.T) {
	// Amending the rail back to empty restores the rail-less draft:
	// the next submit opens the watch on gpi, the cross-border
	// default (payout.go).
	l := seedPayoutLedger(t)
	draftPayoutWithRail(t, l, "po-1", 100_000, "sepa")
	if _, err := l.AmendPayoutRail("po-1", ""); err != nil {
		t.Fatalf("AmendPayoutRail: %v", err)
	}
	if _, err := l.ApprovePayout("po-1", "bob"); err != nil {
		t.Fatalf("ApprovePayout: %v", err)
	}
	if _, err := l.SubmitPayout("po-1"); err != nil {
		t.Fatalf("SubmitPayout: %v", err)
	}
	watch, ok := l.SLAWatch("po-1")
	if !ok {
		t.Fatalf("no SLA watch opened for po-1")
	}
	if watch.Rail != "gpi" {
		t.Errorf("SLA watch rail = %q, want gpi (empty rail defaults)", watch.Rail)
	}
}
