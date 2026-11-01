package ledger

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestAmendPayoutExpiresAtValidation(t *testing.T) {
	l := seedPayoutLedger(t)
	orig := draftPayoutWithExpiry(t, l, "po-1", 100_000, time.Now().Add(48*time.Hour))

	// Unknown ID.
	if _, err := l.AmendPayoutExpiresAt("nope", time.Now().Add(time.Hour)); !errors.Is(err, ErrPayoutNotFound) {
		t.Errorf("unknown: err = %v, want ErrPayoutNotFound", err)
	}

	// A non-zero deadline must lie in the future: a past deadline
	// and "now" are both rejected, and the original deadline is
	// untouched. Creation does not check futureness, so the
	// revision gate is deliberately stricter than the create gate.
	for _, bad := range []time.Time{time.Now().Add(-time.Hour), time.Now()} {
		if _, err := l.AmendPayoutExpiresAt("po-1", bad); !errors.Is(err, ErrInvalidPayoutExpiresAt) {
			t.Errorf("expiry %v: err = %v, want ErrInvalidPayoutExpiresAt", bad, err)
		}
		if got, _ := l.GetPayout("po-1"); !reflect.DeepEqual(got, orig) {
			t.Errorf("payout after expiry %v = %+v, want %+v (untouched)", bad, got, orig)
		}
	}
}

func TestAmendPayoutExpiresAtStateGate(t *testing.T) {
	// Every non-draft status is rejected: approved, submitted,
	// cancelled, and expired.
	t.Run("approved", func(t *testing.T) {
		l := seedPayoutLedger(t)
		draftPayoutWithExpiry(t, l, "po-1", 100_000, time.Now().Add(48*time.Hour))
		if _, err := l.ApprovePayout("po-1", "bob"); err != nil {
			t.Fatalf("ApprovePayout: %v", err)
		}
		if _, err := l.AmendPayoutExpiresAt("po-1", time.Now().Add(72*time.Hour)); !errors.Is(err, ErrPayoutInvalidTransition) {
			t.Errorf("err = %v, want ErrPayoutInvalidTransition", err)
		}
		if p, _ := l.GetPayout("po-1"); !p.ExpiresAt.After(time.Now().Add(47 * time.Hour)) {
			t.Errorf("expiry moved to %v after rejected amendment", p.ExpiresAt)
		}
	})
	t.Run("submitted", func(t *testing.T) {
		l := seedPayoutLedger(t)
		draftPayoutWithExpiry(t, l, "po-1", 100_000, time.Now().Add(48*time.Hour))
		approveSubmitPayout(t, l, "po-1", "bob")
		if _, err := l.AmendPayoutExpiresAt("po-1", time.Now().Add(72*time.Hour)); !errors.Is(err, ErrPayoutInvalidTransition) {
			t.Errorf("err = %v, want ErrPayoutInvalidTransition", err)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		l := seedPayoutLedger(t)
		draftPayoutWithExpiry(t, l, "po-1", 100_000, time.Now().Add(48*time.Hour))
		if _, err := l.CancelPayout("po-1"); err != nil {
			t.Fatalf("CancelPayout: %v", err)
		}
		if _, err := l.AmendPayoutExpiresAt("po-1", time.Now().Add(72*time.Hour)); !errors.Is(err, ErrPayoutInvalidTransition) {
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
		if _, err := l.AmendPayoutExpiresAt("po-1", time.Now().Add(72*time.Hour)); !errors.Is(err, ErrPayoutInvalidTransition) {
			t.Errorf("err = %v, want ErrPayoutInvalidTransition", err)
		}
	})

	// The state gate runs before the expiry validation and the
	// no-op check: even a same-value or past amendment is rejected
	// on a departed instruction.
	l := seedPayoutLedger(t)
	orig := draftPayoutWithExpiry(t, l, "po-1", 100_000, time.Now().Add(48*time.Hour))
	if _, err := l.ApprovePayout("po-1", "bob"); err != nil {
		t.Fatalf("ApprovePayout: %v", err)
	}
	if _, err := l.AmendPayoutExpiresAt("po-1", orig.ExpiresAt); !errors.Is(err, ErrPayoutInvalidTransition) {
		t.Errorf("approved, same expiry: err = %v, want ErrPayoutInvalidTransition", err)
	}
	if _, err := l.AmendPayoutExpiresAt("po-1", time.Now().Add(-time.Hour)); !errors.Is(err, ErrPayoutInvalidTransition) {
		t.Errorf("approved, past expiry: err = %v, want ErrPayoutInvalidTransition", err)
	}
}

func TestAmendPayoutExpiresAtNoOp(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithPayoutClearingAccount("payout-clearing"))
	seedBalance(t, l, "saver", 1_000_000)
	future := time.Now().Add(48 * time.Hour)
	orig := draftPayoutWithExpiry(t, l, "po-1", 100_000, future)

	// Amending to the current deadline is a no-op, not a change.
	if got, err := l.AmendPayoutExpiresAt("po-1", future); err != nil || !reflect.DeepEqual(got, orig) {
		t.Errorf("same-expiry amendment = %+v, %v, want %+v, nil", got, err, orig)
	}

	// Zero and zero are the same no-op: a never-expires draft
	// cleared again stays untouched.
	never := draftPayout(t, l, "po-2", 100_000)
	if got, err := l.AmendPayoutExpiresAt("po-2", time.Time{}); err != nil || !reflect.DeepEqual(got, never) {
		t.Errorf("zero-to-zero amendment = %+v, %v, want %+v, nil", got, err, never)
	}

	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// The no-ops write no audit event.
	if events := auditEventsByOp(t, dir)["payout_expires_at_amended"]; len(events) != 0 {
		t.Errorf("amend events = %d, want 0 (no-op is silent)", len(events))
	}
}

func TestAmendPayoutExpiresAtSuccess(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithPayoutClearingAccount("payout-clearing"))
	seedBalance(t, l, "saver", 1_000_000)
	orig := draftPayoutWithExpiry(t, l, "po-1", 100_000, time.Now().Add(48*time.Hour))
	versionBefore := l.Version()

	// Setting a later deadline: only ExpiresAt moves.
	later := time.Now().Add(72 * time.Hour)
	amended, err := l.AmendPayoutExpiresAt("po-1", later)
	if err != nil {
		t.Fatalf("AmendPayoutExpiresAt: %v", err)
	}
	if !amended.ExpiresAt.Equal(later) {
		t.Errorf("amended expiry = %v, want %v", amended.ExpiresAt, later)
	}
	// Only the deadline moves: amount, currency, destination,
	// payee, maker, status, UETR, and idempotency key are untouched.
	amended.ExpiresAt = orig.ExpiresAt
	if !reflect.DeepEqual(amended, orig) {
		t.Errorf("amended payout = %+v, want %+v except expiry", amended, orig)
	}
	// Off-journal: the ledger version does not move.
	if got := l.Version(); got != versionBefore {
		t.Errorf("version after amendment = %d, want %d (unchanged)", got, versionBefore)
	}
	// The registry carries the new deadline, still a draft.
	stored, ok := l.GetPayout("po-1")
	if !ok || !stored.ExpiresAt.Equal(later) || stored.Status != PayoutDraft {
		t.Errorf("stored payout = %+v,%v, want draft with expiry %v", stored, ok, later)
	}
	// The amendment emits no outbox event: the draft was already
	// reported when created, and the amend path never calls
	// emitOutboxLocked (verified by inspection of the module).

	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["payout_expires_at_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.TraceID != "po-1" || ev.Actor != "AmendPayoutExpiresAt" {
		t.Errorf("amend event = trace %q actor %q, want po-1/AmendPayoutExpiresAt", ev.TraceID, ev.Actor)
	}
	if ev.Details["previous_expires_at"] != orig.ExpiresAt.UTC().Format(time.RFC3339) ||
		ev.Details["expires_at"] != later.UTC().Format(time.RFC3339) {
		t.Errorf("amend event details = %v, want previous %v / new %v",
			ev.Details, orig.ExpiresAt.UTC().Format(time.RFC3339), later.UTC().Format(time.RFC3339))
	}
	if ev.VersionBefore != versionBefore || ev.VersionAfter != versionBefore {
		t.Errorf("amend event versions = %d->%d, want %d->%d", ev.VersionBefore, ev.VersionAfter, versionBefore, versionBefore)
	}
}

func TestAmendPayoutExpiresAtClear(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithPayoutClearingAccount("payout-clearing"))
	seedBalance(t, l, "saver", 1_000_000)
	orig := draftPayoutWithExpiry(t, l, "po-1", 100_000, time.Now().Add(48*time.Hour))

	// Zero clears the deadline back to "never expires".
	cleared, err := l.AmendPayoutExpiresAt("po-1", time.Time{})
	if err != nil {
		t.Fatalf("AmendPayoutExpiresAt: %v", err)
	}
	if !cleared.ExpiresAt.IsZero() {
		t.Errorf("cleared expiry = %v, want zero", cleared.ExpiresAt)
	}
	cleared.ExpiresAt = orig.ExpiresAt
	if !reflect.DeepEqual(cleared, orig) {
		t.Errorf("cleared payout = %+v, want %+v except expiry", cleared, orig)
	}

	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["payout_expires_at_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
	// A zero deadline is recorded as null in the audit details.
	if events[0].Details["previous_expires_at"] != orig.ExpiresAt.UTC().Format(time.RFC3339) ||
		events[0].Details["expires_at"] != nil {
		t.Errorf("clear event details = %v, want previous %v / new null",
			events[0].Details, orig.ExpiresAt.UTC().Format(time.RFC3339))
	}
}

func TestAmendPayoutExpiresAtTakesEffectFromApproval(t *testing.T) {
	// The revised deadline governs from approval: the sweep and
	// the lazy submit expiry both read the live ExpiresAt, so a
	// draft revised onto a later deadline survives a sweep past
	// the original deadline and expires only past the revised one.
	l := seedPayoutLedger(t)
	orig := time.Now().Add(time.Hour)
	draftPayoutWithExpiry(t, l, "po-1", 100_000, orig)
	if _, err := l.AmendPayoutExpiresAt("po-1", time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AmendPayoutExpiresAt: %v", err)
	}
	if _, err := l.ApprovePayout("po-1", "bob"); err != nil {
		t.Fatalf("ApprovePayout: %v", err)
	}
	// Past the original deadline but before the revised one: the
	// instruction survives.
	if expired := l.ExpirePayoutsAt(time.Now().Add(2 * time.Hour)); len(expired) != 0 {
		t.Fatalf("expired = %v, want [] (revised deadline not reached)", expired)
	}
	// Past the revised deadline: it expires.
	if expired := l.ExpirePayoutsAt(time.Now().Add(73 * time.Hour)); len(expired) != 1 || expired[0].ID != "po-1" {
		t.Fatalf("expired = %v, want [po-1]", expired)
	}
}
