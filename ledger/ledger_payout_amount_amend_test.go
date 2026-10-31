package ledger

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestAmendPayoutAmountValidation(t *testing.T) {
	l := seedPayoutLedger(t)
	orig := draftPayout(t, l, "po-1", 100_000)

	// Unknown ID.
	if _, err := l.AmendPayoutAmount("nope", 200_000); !errors.Is(err, ErrPayoutNotFound) {
		t.Errorf("unknown: err = %v, want ErrPayoutNotFound", err)
	}

	// The new amount reuses the CreatePayout gate: zero and negative
	// amounts are rejected, and the original amount is untouched.
	for _, bad := range []int64{0, -1, -100_000} {
		if _, err := l.AmendPayoutAmount("po-1", bad); !errors.Is(err, ErrPayoutNonPositiveAmount) {
			t.Errorf("amount %d: err = %v, want ErrPayoutNonPositiveAmount", bad, err)
		}
		if got, _ := l.GetPayout("po-1"); !reflect.DeepEqual(got, orig) {
			t.Errorf("payout after amount %d = %+v, want %+v (untouched)", bad, got, orig)
		}
	}
}

func TestAmendPayoutAmountStateGate(t *testing.T) {
	// Every non-draft status is rejected: approved, submitted,
	// settled, failed, cancelled, and expired.
	t.Run("approved", func(t *testing.T) {
		l := seedPayoutLedger(t)
		draftPayout(t, l, "po-1", 100_000)
		if _, err := l.ApprovePayout("po-1", "bob"); err != nil {
			t.Fatalf("ApprovePayout: %v", err)
		}
		if _, err := l.AmendPayoutAmount("po-1", 200_000); !errors.Is(err, ErrPayoutInvalidTransition) {
			t.Errorf("err = %v, want ErrPayoutInvalidTransition", err)
		}
		if p, _ := l.GetPayout("po-1"); p.AmountCents != 100_000 {
			t.Errorf("amount moved to %d after rejected amendment", p.AmountCents)
		}
	})
	t.Run("submitted", func(t *testing.T) {
		l := seedPayoutLedger(t)
		draftPayout(t, l, "po-1", 100_000)
		approveSubmitPayout(t, l, "po-1", "bob")
		if _, err := l.AmendPayoutAmount("po-1", 200_000); !errors.Is(err, ErrPayoutInvalidTransition) {
			t.Errorf("err = %v, want ErrPayoutInvalidTransition", err)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		l := seedPayoutLedger(t)
		draftPayout(t, l, "po-1", 100_000)
		if _, err := l.CancelPayout("po-1"); err != nil {
			t.Fatalf("CancelPayout: %v", err)
		}
		if _, err := l.AmendPayoutAmount("po-1", 200_000); !errors.Is(err, ErrPayoutInvalidTransition) {
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
		if _, err := l.AmendPayoutAmount("po-1", 200_000); !errors.Is(err, ErrPayoutInvalidTransition) {
			t.Errorf("err = %v, want ErrPayoutInvalidTransition", err)
		}
	})

	// The state gate runs before the amount validation and the no-op
	// check: even a same-value or non-positive amendment is rejected
	// on a departed instruction.
	l := seedPayoutLedger(t)
	draftPayout(t, l, "po-1", 100_000)
	if _, err := l.ApprovePayout("po-1", "bob"); err != nil {
		t.Fatalf("ApprovePayout: %v", err)
	}
	if _, err := l.AmendPayoutAmount("po-1", 100_000); !errors.Is(err, ErrPayoutInvalidTransition) {
		t.Errorf("approved, same amount: err = %v, want ErrPayoutInvalidTransition", err)
	}
	if _, err := l.AmendPayoutAmount("po-1", 0); !errors.Is(err, ErrPayoutInvalidTransition) {
		t.Errorf("approved, zero amount: err = %v, want ErrPayoutInvalidTransition", err)
	}
}

func TestAmendPayoutAmountNoOp(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithPayoutClearingAccount("payout-clearing"))
	seedBalance(t, l, "saver", 1_000_000)
	orig := draftPayout(t, l, "po-1", 100_000)

	// Amending to the current amount is a no-op, not a change.
	if got, err := l.AmendPayoutAmount("po-1", 100_000); err != nil || !reflect.DeepEqual(got, orig) {
		t.Errorf("same-amount amendment = %+v, %v, want %+v, nil", got, err, orig)
	}

	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// The no-op writes no audit event.
	if events := auditEventsByOp(t, dir)["payout_amount_amended"]; len(events) != 0 {
		t.Errorf("amend events = %d, want 0 (no-op is silent)", len(events))
	}
}

func TestAmendPayoutAmountSuccess(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithPayoutClearingAccount("payout-clearing"))
	seedBalance(t, l, "saver", 1_000_000)
	orig := draftPayout(t, l, "po-1", 100_000)
	versionBefore := l.Version()

	amended, err := l.AmendPayoutAmount("po-1", 250_000)
	if err != nil {
		t.Fatalf("AmendPayoutAmount: %v", err)
	}
	if amended.AmountCents != 250_000 {
		t.Errorf("amended amount = %d, want 250000", amended.AmountCents)
	}
	// Only the amount moves: payee, currency, destination, maker,
	// status, UETR, and idempotency key are untouched.
	amended.AmountCents = orig.AmountCents
	if !reflect.DeepEqual(amended, orig) {
		t.Errorf("amended payout = %+v, want %+v except amount", amended, orig)
	}
	// Off-journal: the ledger version does not move.
	if got := l.Version(); got != versionBefore {
		t.Errorf("version after amendment = %d, want %d (unchanged)", got, versionBefore)
	}
	// The registry carries the new amount, still a draft.
	stored, ok := l.GetPayout("po-1")
	if !ok || stored.AmountCents != 250_000 || stored.Status != PayoutDraft {
		t.Errorf("stored payout = %+v,%v, want draft with amount 250000", stored, ok)
	}
	// The amendment emits no outbox event: the draft was already
	// reported when created, and the amend path never calls
	// emitOutboxLocked (verified by inspection of the module).

	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["payout_amount_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.TraceID != "po-1" || ev.Actor != "AmendPayoutAmount" {
		t.Errorf("amend event = trace %q actor %q, want po-1/AmendPayoutAmount", ev.TraceID, ev.Actor)
	}
	if ev.Details["previous_amount_cents"] != float64(100_000) || ev.Details["amount_cents"] != float64(250_000) {
		t.Errorf("amend event details = %v, want previous 100000 / new 250000", ev.Details)
	}
	if ev.VersionBefore != versionBefore || ev.VersionAfter != versionBefore {
		t.Errorf("amend event versions = %d->%d, want %d->%d", ev.VersionBefore, ev.VersionAfter, versionBefore, versionBefore)
	}
}

func TestAmendPayoutAmountMakerCheckerReadsLiveAmount(t *testing.T) {
	// The four-eyes threshold reads the live amount at approval time:
	// a draft below the threshold that is amended across it newly
	// requires a checker who differs from the maker, and one amended
	// back below it newly lets the maker self-approve.
	l := seedPayoutLedger(t)
	if err := l.SetReviewThreshold("saver", 200_000); err != nil {
		t.Fatalf("SetReviewThreshold: %v", err)
	}
	draftPayout(t, l, "po-1", 100_000)
	draftPayout(t, l, "po-2", 300_000)

	// po-1 below the threshold: the maker may self-approve.
	if _, err := l.AmendPayoutAmount("po-1", 250_000); err != nil {
		t.Fatalf("AmendPayoutAmount: %v", err)
	}
	if _, err := l.ApprovePayout("po-1", "alice"); !errors.Is(err, ErrPayoutMakerCheckerViolation) {
		t.Errorf("amended across threshold, maker approve: err = %v, want ErrPayoutMakerCheckerViolation", err)
	}
	if _, err := l.ApprovePayout("po-1", "bob"); err != nil {
		t.Errorf("amended across threshold, checker approve: err = %v, want nil", err)
	}

	// po-2 above the threshold: amending below it lets the maker
	// self-approve.
	if _, err := l.AmendPayoutAmount("po-2", 150_000); err != nil {
		t.Fatalf("AmendPayoutAmount: %v", err)
	}
	if _, err := l.ApprovePayout("po-2", "alice"); err != nil {
		t.Errorf("amended below threshold, maker approve: err = %v, want nil", err)
	}
}
