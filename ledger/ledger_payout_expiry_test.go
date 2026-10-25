package ledger

import (
	"errors"
	"testing"
	"time"
)

func draftPayoutWithExpiry(t *testing.T, l *Ledger, id string, amount int64, expiresAt time.Time) PayoutInstruction {
	t.Helper()
	p, dup, err := l.CreatePayout(PayoutInstruction{
		ID:           id,
		PayeeAccount: "saver",
		AmountCents:  amount,
		Destination:  "FPS:6222000012345678",
		Maker:        "alice",
		ExpiresAt:    expiresAt,
	})
	if err != nil {
		t.Fatalf("CreatePayout: %v", err)
	}
	if dup {
		t.Fatal("duplicate = true on first create")
	}
	if !p.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("ExpiresAt = %v, want %v", p.ExpiresAt, expiresAt)
	}
	return p
}

func TestPayoutExpiry_SweepOnlyApprovedPastExpiry(t *testing.T) {
	l := seedPayoutLedger(t)
	now := time.Now()
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	// Approved and past expiry: expires.
	draftPayoutWithExpiry(t, l, "po-exp", 100_000, past)
	if _, err := l.ApprovePayout("po-exp", "bob"); err != nil {
		t.Fatalf("ApprovePayout: %v", err)
	}
	// Approved but not yet expired: survives.
	draftPayoutWithExpiry(t, l, "po-live", 100_000, future)
	if _, err := l.ApprovePayout("po-live", "bob"); err != nil {
		t.Fatalf("ApprovePayout: %v", err)
	}
	// No expiry set: survives regardless of the sweep time.
	draftPayout(t, l, "po-never", 100_000)
	if _, err := l.ApprovePayout("po-never", "bob"); err != nil {
		t.Fatalf("ApprovePayout: %v", err)
	}
	// Draft past expiry: not swept — only approved instructions expire.
	draftPayoutWithExpiry(t, l, "po-draft", 100_000, past)

	beforeVersion := l.Version()
	expired := l.ExpirePayoutsAt(now)
	if len(expired) != 1 || expired[0].ID != "po-exp" {
		t.Fatalf("expired = %v, want [po-exp]", expired)
	}
	if expired[0].Status != PayoutExpired {
		t.Errorf("status = %q, want expired", expired[0].Status)
	}
	if expired[0].ExpiredAt.IsZero() {
		t.Error("ExpiredAt is zero")
	}
	// Expiry books nothing and bumps nothing.
	if v := l.Version(); v != beforeVersion {
		t.Errorf("version = %d, want %d (expiry must not bump)", v, beforeVersion)
	}
	if b := l.Balance("saver"); b != 1_000_000 {
		t.Errorf("saver balance = %d, want 1000000", b)
	}
	if b := l.Balance("payout-clearing"); b != 0 {
		t.Errorf("clearing balance = %d, want 0", b)
	}
	// The sweep is idempotent and terminal: no second expiry, and the
	// expired instruction can neither submit nor cancel.
	if again := l.ExpirePayoutsAt(now); len(again) != 0 {
		t.Errorf("second sweep expired %v, want none", again)
	}
	if _, err := l.SubmitPayout("po-exp"); !errors.Is(err, ErrPayoutInvalidTransition) {
		t.Errorf("submit expired err = %v, want ErrPayoutInvalidTransition", err)
	}
	if _, err := l.CancelPayout("po-exp"); !errors.Is(err, ErrPayoutInvalidTransition) {
		t.Errorf("cancel expired err = %v, want ErrPayoutInvalidTransition", err)
	}
	// The other instructions are untouched.
	for _, id := range []string{"po-live", "po-never"} {
		p, ok := l.GetPayout(id)
		if !ok || p.Status != PayoutApproved {
			t.Errorf("%s status = %q, want approved", id, p.Status)
		}
	}
	if p, _ := l.GetPayout("po-draft"); p.Status != PayoutDraft {
		t.Errorf("po-draft status = %q, want draft", p.Status)
	}
}

func TestPayoutExpiry_BoundaryIsExpired(t *testing.T) {
	l := seedPayoutLedger(t)
	now := time.Now()
	draftPayoutWithExpiry(t, l, "po-edge", 100_000, now)
	if _, err := l.ApprovePayout("po-edge", "bob"); err != nil {
		t.Fatalf("ApprovePayout: %v", err)
	}
	// Just before expiry: survives. At expiry: expires.
	if expired := l.ExpirePayoutsAt(now.Add(-time.Second)); len(expired) != 0 {
		t.Fatalf("early sweep expired %v, want none", expired)
	}
	if expired := l.ExpirePayoutsAt(now); len(expired) != 1 {
		t.Fatalf("boundary sweep expired %v, want [po-edge]", expired)
	}
}

func TestPayoutExpiry_SubmittedNeverExpires(t *testing.T) {
	l := seedPayoutLedger(t)
	now := time.Now()
	// Expiry is after the (wall-clock) submit but before the sweep.
	draftPayoutWithExpiry(t, l, "po-inflight", 100_000, now.Add(time.Hour))
	if _, err := l.ApprovePayout("po-inflight", "bob"); err != nil {
		t.Fatalf("ApprovePayout: %v", err)
	}
	if _, err := l.SubmitPayout("po-inflight"); err != nil {
		t.Fatalf("SubmitPayout: %v", err)
	}
	if expired := l.ExpirePayoutsAt(now.Add(2 * time.Hour)); len(expired) != 0 {
		t.Errorf("sweep expired submitted %v, want none", expired)
	}
	if p, _ := l.GetPayout("po-inflight"); p.Status != PayoutSubmitted {
		t.Errorf("status = %q, want submitted", p.Status)
	}
}

func TestPayoutExpiry_SubmitPastExpiryLazilyExpires(t *testing.T) {
	l := seedPayoutLedger(t)
	draftPayoutWithExpiry(t, l, "po-late", 100_000, time.Now().Add(-time.Hour))
	if _, err := l.ApprovePayout("po-late", "bob"); err != nil {
		t.Fatalf("ApprovePayout: %v", err)
	}
	beforeVersion := l.Version()
	if _, err := l.SubmitPayout("po-late"); !errors.Is(err, ErrPayoutExpired) {
		t.Fatalf("submit err = %v, want ErrPayoutExpired", err)
	}
	p, _ := l.GetPayout("po-late")
	if p.Status != PayoutExpired {
		t.Errorf("status = %q, want expired", p.Status)
	}
	if v := l.Version(); v != beforeVersion {
		t.Errorf("version = %d, want %d", v, beforeVersion)
	}
	if b := l.Balance("saver"); b != 1_000_000 {
		t.Errorf("saver balance = %d, want 1000000 (no clearing leg)", b)
	}
	if p.ClearingEntryID != "" {
		t.Errorf("ClearingEntryID = %q, want empty", p.ClearingEntryID)
	}
}

func TestPayoutExpiry_SweepReturnsSortedIDs(t *testing.T) {
	l := seedPayoutLedger(t)
	now := time.Now()
	for _, id := range []string{"po-z", "po-a", "po-m"} {
		draftPayoutWithExpiry(t, l, id, 10_000, now.Add(-time.Hour))
		if _, err := l.ApprovePayout(id, "bob"); err != nil {
			t.Fatalf("ApprovePayout %s: %v", id, err)
		}
	}
	expired := l.ExpirePayoutsAt(now)
	if len(expired) != 3 {
		t.Fatalf("expired = %d, want 3", len(expired))
	}
	for i, want := range []string{"po-a", "po-m", "po-z"} {
		if expired[i].ID != want {
			t.Errorf("expired[%d] = %q, want %q", i, expired[i].ID, want)
		}
	}
}
