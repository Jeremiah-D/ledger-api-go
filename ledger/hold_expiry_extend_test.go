package ledger

import (
	"errors"
	"testing"
	"time"
)

func extendHoldSeed(t *testing.T, id string, cents int64) *Ledger {
	t.Helper()
	l := New()
	fundAccount(t, l, "card", 10000, "")
	if _, _, err := l.Hold(Hold{ID: id, Account: "card", AmountCents: cents, ExpiresAt: futureExpiry()}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	return l
}

func TestExtendHoldExpirySuccess(t *testing.T) {
	l := extendHoldSeed(t, "h1", 6000)
	before, _ := l.GetHold("h1")
	versionBefore := l.Version()
	extended, err := l.ExtendHoldExpiry("h1", time.Now().Add(3*time.Hour))
	if err != nil {
		t.Fatalf("ExtendHoldExpiry: %v", err)
	}
	if extended.Status != HoldStatusActive {
		t.Errorf("status = %q, want active", extended.Status)
	}
	if !extended.ExpiresAt.After(before.ExpiresAt) {
		t.Errorf("expiry = %v, want later than %v", extended.ExpiresAt, before.ExpiresAt)
	}
	// The extension moves the deadline and nothing else.
	if extended.AmountCents != 6000 || extended.Currency != before.Currency || extended.CapturedCents != 0 {
		t.Errorf("hold after extension = %+v, want amount/currency/capture untouched", extended)
	}
	if got := l.Available("card"); got != 4000 {
		t.Errorf("Available = %d, want 4000 (reservation unchanged)", got)
	}
	if l.Version() != versionBefore {
		t.Errorf("version moved: extension is off-journal")
	}
	// The extended hold survives a sweep at the old expiry and
	// lapses only at the new one — it composes with the sweep
	// instead of fighting it.
	if got := l.ExpireHoldsAt(before.ExpiresAt); got != 0 {
		t.Errorf("sweep at old expiry expired = %d, want 0", got)
	}
	if h, _ := l.GetHold("h1"); h.Status != HoldStatusActive {
		t.Errorf("status after old-expiry sweep = %q, want active", h.Status)
	}
	if got := l.ExpireHoldsAt(extended.ExpiresAt); got != 1 {
		t.Errorf("sweep at new expiry expired = %d, want 1", got)
	}
}

func TestExtendHoldExpiryOnlyLengthens(t *testing.T) {
	l := extendHoldSeed(t, "h1", 6000)
	before, _ := l.GetHold("h1")
	cases := map[string]time.Time{
		"equal":   before.ExpiresAt,
		"earlier": before.ExpiresAt.Add(-time.Minute),
		"past":    time.Now().Add(-time.Hour),
		"zero":    time.Time{},
	}
	for name, next := range cases {
		if _, err := l.ExtendHoldExpiry("h1", next); !errors.Is(err, ErrInvalidHoldExpiryExtension) {
			t.Errorf("%s: err = %v, want ErrInvalidHoldExpiryExtension", name, err)
		}
	}
	// Every rejected extension leaves the original expiry intact.
	if h, _ := l.GetHold("h1"); h.ExpiresAt.UnixNano() != before.ExpiresAt.UnixNano() || h.Status != HoldStatusActive {
		t.Errorf("hold after rejected extensions = %+v, want original expiry, still active", h)
	}
	// And a valid extension still works afterwards.
	if _, err := l.ExtendHoldExpiry("h1", time.Now().Add(2*time.Hour)); err != nil {
		t.Errorf("valid extension after rejections: %v", err)
	}
}

func TestExtendHoldExpiryTerminalStates(t *testing.T) {
	if _, err := New().ExtendHoldExpiry("h-unknown", time.Now().Add(time.Hour)); !errors.Is(err, ErrHoldNotFound) {
		t.Errorf("unknown hold: err = %v, want ErrHoldNotFound", err)
	}
	// Released.
	l := extendHoldSeed(t, "h1", 6000)
	if _, err := l.Release("h1"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := l.ExtendHoldExpiry("h1", time.Now().Add(2*time.Hour)); !errors.Is(err, ErrHoldNotActive) {
		t.Errorf("released hold: err = %v, want ErrHoldNotActive", err)
	}
	// Fully captured.
	l2 := extendHoldSeed(t, "h2", 6000)
	if _, err := l2.Capture(Capture{ID: "c1", HoldID: "h2", To: "merchant", AmountCents: 6000}); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if _, err := l2.ExtendHoldExpiry("h2", time.Now().Add(2*time.Hour)); !errors.Is(err, ErrHoldNotActive) {
		t.Errorf("captured hold: err = %v, want ErrHoldNotActive", err)
	}
	// Swept-expired.
	l3 := extendHoldSeed(t, "h3", 6000)
	if got := l3.ExpireHoldsAt(time.Now().Add(2 * time.Hour)); got != 1 {
		t.Fatalf("sweep expired = %d, want 1", got)
	}
	if _, err := l3.ExtendHoldExpiry("h3", time.Now().Add(3*time.Hour)); !errors.Is(err, ErrHoldExpired) {
		t.Errorf("swept-expired hold: err = %v, want ErrHoldExpired", err)
	}
	// Lapsed but unswept: still expired, never resurrected.
	l4 := New()
	fundAccount(t, l4, "card", 10000, "")
	if _, _, err := l4.Hold(Hold{ID: "h4", Account: "card", AmountCents: 6000, ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatalf("Hold with past expiry: %v", err)
	}
	if _, err := l4.ExtendHoldExpiry("h4", time.Now().Add(time.Hour)); !errors.Is(err, ErrHoldExpired) {
		t.Errorf("lapsed hold: err = %v, want ErrHoldExpired", err)
	}
}

func TestExtendHoldExpiryPreservesPartialCapture(t *testing.T) {
	l := extendHoldSeed(t, "h1", 1000)
	if _, err := l.CapturePartial(PartialCapture{HoldID: "h1", To: "merchant", CaptureSeq: 1, AmountCents: 300}); err != nil {
		t.Fatalf("CapturePartial: %v", err)
	}
	extended, err := l.ExtendHoldExpiry("h1", time.Now().Add(3*time.Hour))
	if err != nil {
		t.Fatalf("ExtendHoldExpiry: %v", err)
	}
	// The capture position is not reset by moving the deadline.
	if extended.Status != HoldStatusActive || extended.CapturedCents != 300 || extended.AmountCents != 1000 {
		t.Errorf("hold after extension = %+v, want active, 300 captured of 1000", extended)
	}
	if got := l.Available("card"); got != 9000 {
		t.Errorf("Available = %d, want 9000 (700 remainder still reserved)", got)
	}
}

func TestExtendHoldExpiryAudit(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	fundAccount(t, l, "card", 10000, "")
	if _, _, err := l.Hold(Hold{ID: "h1", Account: "card", AmountCents: 6000, ExpiresAt: futureExpiry()}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	before, _ := l.GetHold("h1")
	extended, err := l.ExtendHoldExpiry("h1", time.Now().Add(3*time.Hour))
	if err != nil {
		t.Fatalf("ExtendHoldExpiry: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["hold_expiry_extended"]
	if len(events) != 1 {
		t.Fatalf("extend events = %d, want 1", len(events))
	}
	d := events[0].Details
	if d["hold_id"] != "h1" || d["amount_cents"] != float64(6000) {
		t.Errorf("event details = %+v, want hold h1, 6000 cents", d)
	}
	if d["previous_expires_at"] != before.ExpiresAt.UTC().Format(time.RFC3339) {
		t.Errorf("previous_expires_at = %v, want %v", d["previous_expires_at"], before.ExpiresAt.UTC().Format(time.RFC3339))
	}
	if d["expires_at"] != extended.ExpiresAt.UTC().Format(time.RFC3339) {
		t.Errorf("expires_at = %v, want %v", d["expires_at"], extended.ExpiresAt.UTC().Format(time.RFC3339))
	}
}

// TestExtendHoldExpiryOverCapRejectedAndHoldUntouched pins the LG-359
// cap: with a 30-day cap, an extension past now + 30 days is rejected
// with ErrHoldExceedsMaxDuration and the hold keeps its old expiry.
func TestExtendHoldExpiryOverCapRejectedAndHoldUntouched(t *testing.T) {
	l := extendHoldSeed(t, "h1", 6000)
	before, _ := l.GetHold("h1")
	if err := l.SetMaxHoldDuration(30 * 24 * time.Hour); err != nil {
		t.Fatalf("SetMaxHoldDuration(30d): %v", err)
	}
	if _, err := l.ExtendHoldExpiry("h1", time.Now().Add(31*24*time.Hour)); !errors.Is(err, ErrHoldExceedsMaxDuration) {
		t.Fatalf("over-cap extension err = %v, want ErrHoldExceedsMaxDuration", err)
	}
	if h, _ := l.GetHold("h1"); h.ExpiresAt.UnixNano() != before.ExpiresAt.UnixNano() || h.Status != HoldStatusActive {
		t.Errorf("hold after rejected extension = %+v, want original expiry, still active", h)
	}
	// And a within-cap extension still works afterwards.
	if _, err := l.ExtendHoldExpiry("h1", time.Now().Add(10*24*time.Hour)); err != nil {
		t.Errorf("within-cap extension after rejection: %v", err)
	}
}

// TestExtendHoldExpiryWithinCapSucceeds is the happy path under a cap:
// extending inside now + cap succeeds and moves only the deadline.
func TestExtendHoldExpiryWithinCapSucceeds(t *testing.T) {
	l := extendHoldSeed(t, "h1", 6000)
	if err := l.SetMaxHoldDuration(30 * 24 * time.Hour); err != nil {
		t.Fatalf("SetMaxHoldDuration(30d): %v", err)
	}
	extended, err := l.ExtendHoldExpiry("h1", time.Now().Add(10*24*time.Hour))
	if err != nil {
		t.Fatalf("within-cap extension: %v", err)
	}
	if extended.AmountCents != 6000 || extended.Status != HoldStatusActive {
		t.Errorf("hold after extension = %+v, want amount/status untouched", extended)
	}
}

// TestExtendHoldExpiryCapZeroUnlimited pins that cap == 0 (the
// default) imposes no bound: a year-long extension succeeds.
func TestExtendHoldExpiryCapZeroUnlimited(t *testing.T) {
	l := extendHoldSeed(t, "h1", 6000)
	if got := l.MaxHoldDuration(); got != 0 {
		t.Fatalf("default cap = %v, want 0 (unlimited)", got)
	}
	if _, err := l.ExtendHoldExpiry("h1", time.Now().Add(365*24*time.Hour)); err != nil {
		t.Fatalf("year-long extension with no cap: %v", err)
	}
}

// TestExtendHoldExpiryDisputeHoldExempt pins the LG-359 exemption: a
// dispute freeze hold extends past the cap like its creation — the
// risk-control freeze is not an operator-chosen horizon.
func TestExtendHoldExpiryDisputeHoldExempt(t *testing.T) {
	l := seedDisputeLedger(t)
	if _, _, err := l.RaiseDispute(Dispute{
		ID:          "d1",
		TransferID:  "t1",
		ReasonCode:  DisputeReasonUnauthorized,
		AmountCents: 6000,
	}); err != nil {
		t.Fatalf("RaiseDispute: %v", err)
	}
	if err := l.SetMaxHoldDuration(30 * 24 * time.Hour); err != nil {
		t.Fatalf("SetMaxHoldDuration(30d): %v", err)
	}
	// The freeze currently expires ~120 days out; extending it to
	// ~150 days is far past the 30-day cap and must still succeed.
	extended, err := l.ExtendHoldExpiry("dispute/d1", time.Now().Add(150*24*time.Hour))
	if err != nil {
		t.Fatalf("dispute-hold extension past the cap: %v (the dispute freeze must stay exempt)", err)
	}
	if extended.ExpiresAt.Before(time.Now().Add(149 * 24 * time.Hour)) {
		t.Errorf("dispute hold expiry = %v, want ~150 days out", extended.ExpiresAt)
	}
}

// TestExtendHoldExpiryCapTightenedAfterPlacement pins that the cap is
// read live at extension time: a hold placed before any cap was set
// is judged against the cap in force when it is extended.
func TestExtendHoldExpiryCapTightenedAfterPlacement(t *testing.T) {
	l := New()
	fundAccount(t, l, "card", 10000, "")
	if _, _, err := l.Hold(Hold{ID: "h1", Account: "card", AmountCents: 6000, ExpiresAt: time.Now().Add(10 * 24 * time.Hour)}); err != nil {
		t.Fatalf("Hold (no cap yet): %v", err)
	}
	if err := l.SetMaxHoldDuration(30 * 24 * time.Hour); err != nil {
		t.Fatalf("SetMaxHoldDuration(30d): %v", err)
	}
	if _, err := l.ExtendHoldExpiry("h1", time.Now().Add(31*24*time.Hour)); !errors.Is(err, ErrHoldExceedsMaxDuration) {
		t.Fatalf("over-cap extension after tightening err = %v, want ErrHoldExceedsMaxDuration", err)
	}
	if _, err := l.ExtendHoldExpiry("h1", time.Now().Add(20*24*time.Hour)); err != nil {
		t.Errorf("within-cap extension after tightening: %v", err)
	}
}

// TestExtendHoldExpiryCapBoundaryExactAdmitted pins the inclusive
// boundary, like creation: an expiry exactly at now + cap is
// admitted, not rejected.
func TestExtendHoldExpiryCapBoundaryExactAdmitted(t *testing.T) {
	l := extendHoldSeed(t, "h1", 6000)
	if err := l.SetMaxHoldDuration(30 * 24 * time.Hour); err != nil {
		t.Fatalf("SetMaxHoldDuration(30d): %v", err)
	}
	// The target is computed from this test's now; the call's own
	// now is a hair later, so the target sits at (just inside)
	// now + cap — admitted, like a creation at the boundary.
	if _, err := l.ExtendHoldExpiry("h1", time.Now().Add(30*24*time.Hour)); err != nil {
		t.Fatalf("boundary extension to now+cap: %v", err)
	}
}

// TestExtendHoldExpiryCheckOrderUnchanged pins that the cap check
// runs last: unknown IDs, expired holds, and not-lengthening targets
// report their own errors even when the target is also over the cap —
// and an already-lapsed hold reports expired, never over-cap, because
// both decisions read the same now.
func TestExtendHoldExpiryCheckOrderUnchanged(t *testing.T) {
	l := New()
	fundAccount(t, l, "card", 10000, "")
	if err := l.SetMaxHoldDuration(30 * 24 * time.Hour); err != nil {
		t.Fatalf("SetMaxHoldDuration(30d): %v", err)
	}
	// Unknown ID first, even with an over-cap target.
	if _, err := l.ExtendHoldExpiry("no-such-hold", time.Now().Add(40*24*time.Hour)); !errors.Is(err, ErrHoldNotFound) {
		t.Errorf("unknown hold err = %v, want ErrHoldNotFound", err)
	}
	// A hold whose expiry is already past: born expired, so the
	// extension reports expired — the cap never gets a say.
	if _, _, err := l.Hold(Hold{ID: "h-exp", Account: "card", AmountCents: 1000, ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatalf("Hold with past expiry: %v", err)
	}
	if _, err := l.ExtendHoldExpiry("h-exp", time.Now().Add(40*24*time.Hour)); !errors.Is(err, ErrHoldExpired) {
		t.Errorf("expired hold err = %v, want ErrHoldExpired", err)
	}
	// A not-lengthening target reports its own error, not the cap's.
	l2 := extendHoldSeed(t, "h1", 6000)
	if err := l2.SetMaxHoldDuration(30 * 24 * time.Hour); err != nil {
		t.Fatalf("SetMaxHoldDuration(30d): %v", err)
	}
	before, _ := l2.GetHold("h1")
	if _, err := l2.ExtendHoldExpiry("h1", before.ExpiresAt); !errors.Is(err, ErrInvalidHoldExpiryExtension) {
		t.Errorf("equal target err = %v, want ErrInvalidHoldExpiryExtension", err)
	}
}
