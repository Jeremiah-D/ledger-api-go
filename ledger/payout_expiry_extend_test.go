package ledger

import (
	"errors"
	"testing"
	"time"
)

func TestExtendPayoutExpirySuccess(t *testing.T) {
	l := seedPayoutLedger(t)
	before := time.Now().Add(time.Hour).Truncate(time.Second)
	draftPayoutWithExpiry(t, l, "po-1", 100_000, before)
	approved, err := l.ApprovePayout("po-1", "bob")
	if err != nil {
		t.Fatalf("ApprovePayout: %v", err)
	}
	versionBefore := l.Version()
	extended := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	got, err := l.ExtendPayoutExpiry("po-1", extended)
	if err != nil {
		t.Fatalf("ExtendPayoutExpiry: %v", err)
	}
	// The extension moves the deadline and nothing else.
	if !got.ExpiresAt.Equal(extended) {
		t.Errorf("ExpiresAt = %v, want %v", got.ExpiresAt, extended)
	}
	if got.AmountCents != 100_000 || got.Currency != "USD" ||
		got.PayeeAccount != "saver" || got.Destination != "FPS:6222000012345678" ||
		got.Maker != "alice" || got.Approver != "bob" || got.Status != PayoutApproved ||
		!got.ApprovedAt.Equal(approved.ApprovedAt) || got.UETR != approved.UETR ||
		got.ClearingEntryID != "" || got.SettlementEntryID != "" {
		t.Errorf("extended payout = %+v, want only ExpiresAt changed", got)
	}
	if l.Version() != versionBefore {
		t.Error("version moved: extension is off-journal")
	}
	// The registry carries the new deadline.
	stored, ok := l.GetPayout("po-1")
	if !ok || !stored.ExpiresAt.Equal(extended) || stored.Status != PayoutApproved {
		t.Errorf("stored payout = %+v,%v, want approved with expiry %v", stored, ok, extended)
	}
	// The sweep reads the live deadline: a sweep at the old
	// deadline expires nothing, and one at the new deadline
	// expires the instruction.
	if expired := l.ExpirePayoutsAt(before); len(expired) != 0 {
		t.Errorf("sweep at old expiry expired %v, want none", expired)
	}
	if p, _ := l.GetPayout("po-1"); p.Status != PayoutApproved {
		t.Errorf("status after old-expiry sweep = %q, want approved", p.Status)
	}
	if expired := l.ExpirePayoutsAt(extended); len(expired) != 1 || expired[0].ID != "po-1" {
		t.Errorf("sweep at new expiry expired %v, want [po-1]", expired)
	}
	if p, _ := l.GetPayout("po-1"); p.Status != PayoutExpired {
		t.Errorf("status after new-expiry sweep = %q, want expired", p.Status)
	}
}

func TestExtendPayoutExpiryRejections(t *testing.T) {
	now := time.Now()
	before := now.Add(time.Hour).Truncate(time.Second)
	newer := now.Add(3 * time.Hour).Truncate(time.Second)

	// Unknown ID.
	l := New()
	if _, err := l.ExtendPayoutExpiry("nope", newer); !errors.Is(err, ErrPayoutNotFound) {
		t.Errorf("unknown: err = %v, want ErrPayoutNotFound", err)
	}

	// A never-expires instruction has no deadline to move, and the
	// extension must not invent one.
	ln := seedPayoutLedger(t)
	draftPayout(t, ln, "po-never", 100_000)
	if _, err := ln.ApprovePayout("po-never", "bob"); err != nil {
		t.Fatalf("ApprovePayout: %v", err)
	}
	if _, err := ln.ExtendPayoutExpiry("po-never", newer); !errors.Is(err, ErrPayoutExpiryNotSet) {
		t.Errorf("no expiry: err = %v, want ErrPayoutExpiryNotSet", err)
	}
	if p, _ := ln.GetPayout("po-never"); !p.ExpiresAt.IsZero() {
		t.Errorf("never-expires instruction gained an expiry: %v", p.ExpiresAt)
	}

	// Equal, earlier, and past new deadlines all fail, and the
	// original deadline is untouched.
	for _, tc := range []struct {
		name string
		next time.Time
	}{
		{"equal", before},
		{"earlier", now.Add(30 * time.Minute)},
		{"past", now.Add(-time.Hour)},
	} {
		ll := seedPayoutLedger(t)
		draftPayoutWithExpiry(t, ll, "po-1", 100_000, before)
		if _, err := ll.ApprovePayout("po-1", "bob"); err != nil {
			t.Fatalf("ApprovePayout: %v", err)
		}
		if _, err := ll.ExtendPayoutExpiry("po-1", tc.next); !errors.Is(err, ErrPayoutExpiryExtensionNotLater) {
			t.Errorf("%s: err = %v, want ErrPayoutExpiryExtensionNotLater", tc.name, err)
		}
		if p, _ := ll.GetPayout("po-1"); !p.ExpiresAt.Equal(before) {
			t.Errorf("%s: expiry moved to %v after rejected extension", tc.name, p.ExpiresAt)
		}
	}

	// Every non-approved status is rejected: draft, submitted,
	// settled, failed, cancelled, returned, and expired.
	t.Run("draft", func(t *testing.T) {
		l := seedPayoutLedger(t)
		draftPayoutWithExpiry(t, l, "po-1", 100_000, before)
		if _, err := l.ExtendPayoutExpiry("po-1", newer); !errors.Is(err, ErrPayoutInvalidTransition) {
			t.Errorf("err = %v, want ErrPayoutInvalidTransition", err)
		}
	})
	t.Run("submitted", func(t *testing.T) {
		l := seedPayoutLedger(t)
		draftPayoutWithExpiry(t, l, "po-1", 100_000, before)
		approveSubmitPayout(t, l, "po-1", "bob")
		if _, err := l.ExtendPayoutExpiry("po-1", newer); !errors.Is(err, ErrPayoutInvalidTransition) {
			t.Errorf("err = %v, want ErrPayoutInvalidTransition", err)
		}
	})
	t.Run("settled", func(t *testing.T) {
		l := seedPayoutLedger(t)
		settlePayoutForReturn(t, l, "po-1", 100_000)
		if _, err := l.ExtendPayoutExpiry("po-1", newer); !errors.Is(err, ErrPayoutInvalidTransition) {
			t.Errorf("err = %v, want ErrPayoutInvalidTransition", err)
		}
	})
	t.Run("failed", func(t *testing.T) {
		l := seedPayoutLedger(t)
		draftPayoutWithExpiry(t, l, "po-1", 100_000, before)
		approveSubmitPayout(t, l, "po-1", "bob")
		if _, err := l.FailPayout("po-1", "bank rejected"); err != nil {
			t.Fatalf("FailPayout: %v", err)
		}
		if _, err := l.ExtendPayoutExpiry("po-1", newer); !errors.Is(err, ErrPayoutInvalidTransition) {
			t.Errorf("err = %v, want ErrPayoutInvalidTransition", err)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		l := seedPayoutLedger(t)
		draftPayoutWithExpiry(t, l, "po-1", 100_000, before)
		if _, err := l.ApprovePayout("po-1", "bob"); err != nil {
			t.Fatalf("ApprovePayout: %v", err)
		}
		if _, err := l.CancelPayout("po-1"); err != nil {
			t.Fatalf("CancelPayout: %v", err)
		}
		if _, err := l.ExtendPayoutExpiry("po-1", newer); !errors.Is(err, ErrPayoutInvalidTransition) {
			t.Errorf("err = %v, want ErrPayoutInvalidTransition", err)
		}
	})
	t.Run("returned", func(t *testing.T) {
		l := seedPayoutLedger(t)
		p := settlePayoutForReturn(t, l, "po-1", 100_000)
		ret, err := ParsePacs004(pacs004InboundDoc(t, "rtr-1", p.ID, p.UETR, "1000.00", "USD", Pacs004ReturnCustomerRequest))
		if err != nil {
			t.Fatalf("ParsePacs004: %v", err)
		}
		if _, err := l.ApplyPacs004Return(ret, "operator-1"); err != nil {
			t.Fatalf("ApplyPacs004Return: %v", err)
		}
		if _, err := l.ExtendPayoutExpiry("po-1", newer); !errors.Is(err, ErrPayoutInvalidTransition) {
			t.Errorf("err = %v, want ErrPayoutInvalidTransition", err)
		}
	})
	t.Run("expired", func(t *testing.T) {
		l := seedPayoutLedger(t)
		draftPayoutWithExpiry(t, l, "po-1", 100_000, now.Add(-time.Hour))
		if _, err := l.ApprovePayout("po-1", "bob"); err != nil {
			t.Fatalf("ApprovePayout: %v", err)
		}
		if expired := l.ExpirePayoutsAt(now); len(expired) != 1 {
			t.Fatalf("sweep expired %v, want [po-1]", expired)
		}
		if _, err := l.ExtendPayoutExpiry("po-1", newer); !errors.Is(err, ErrPayoutInvalidTransition) {
			t.Errorf("err = %v, want ErrPayoutInvalidTransition", err)
		}
	})
}

func TestExtendPayoutExpiryStaleDeadline(t *testing.T) {
	l := seedPayoutLedger(t)
	// Approved, past its expiry, but neither the sweep nor a submit
	// attempt has marked it expired yet: still live, still extendable.
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	draftPayoutWithExpiry(t, l, "po-1", 100_000, past)
	if _, err := l.ApprovePayout("po-1", "bob"); err != nil {
		t.Fatalf("ApprovePayout: %v", err)
	}
	extended := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	got, err := l.ExtendPayoutExpiry("po-1", extended)
	if err != nil {
		t.Fatalf("ExtendPayoutExpiry: %v", err)
	}
	if !got.ExpiresAt.Equal(extended) || got.Status != PayoutApproved {
		t.Errorf("extended = %+v, want approved with expiry %v", got, extended)
	}
	// The sweep reads the live deadline: nothing expires at the old
	// deadline, and the instruction expires at the new one.
	if expired := l.ExpirePayoutsAt(past); len(expired) != 0 {
		t.Errorf("sweep at old expiry expired %v, want none", expired)
	}
	if expired := l.ExpirePayoutsAt(extended); len(expired) != 1 || expired[0].ID != "po-1" {
		t.Errorf("sweep at new expiry expired %v, want [po-1]", expired)
	}
}

func TestExtendPayoutExpiryAudit(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := seedPayoutLedger(t, WithAuditLog(al))
	before := time.Now().Add(time.Hour).Truncate(time.Second)
	draftPayoutWithExpiry(t, l, "po-1", 100_000, before)
	if _, err := l.ApprovePayout("po-1", "bob"); err != nil {
		t.Fatalf("ApprovePayout: %v", err)
	}
	extended := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	if _, err := l.ExtendPayoutExpiry("po-1", extended); err != nil {
		t.Fatalf("ExtendPayoutExpiry: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["payout_expiry_extended"]
	if len(events) != 1 {
		t.Fatalf("extend events = %d, want 1", len(events))
	}
	d := events[0].Details
	if d["payout_id"] != "po-1" {
		t.Errorf("event details = %+v, want payout po-1", d)
	}
	if d["previous_expires_at"] != before.UTC().Format(time.RFC3339) {
		t.Errorf("previous_expires_at = %v, want %v", d["previous_expires_at"], before.UTC().Format(time.RFC3339))
	}
	if d["expires_at"] != extended.UTC().Format(time.RFC3339) {
		t.Errorf("expires_at = %v, want %v", d["expires_at"], extended.UTC().Format(time.RFC3339))
	}
}
