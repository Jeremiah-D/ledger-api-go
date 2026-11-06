package ledger

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestAmendHoldExpiryValidation(t *testing.T) {
	if _, err := New().AmendHoldExpiry("h-unknown", time.Now().Add(time.Hour)); !errors.Is(err, ErrHoldNotFound) {
		t.Errorf("unknown hold: err = %v, want ErrHoldNotFound", err)
	}
	l := extendHoldSeed(t, "h1", 6000)
	before, _ := l.GetHold("h1")
	now := time.Now()
	for _, newExpiry := range []time.Time{now.Add(-time.Hour), now} {
		if _, err := l.AmendHoldExpiry("h1", newExpiry); !errors.Is(err, ErrHoldExpiryAmendInvalid) {
			t.Errorf("expiry %v: err = %v, want ErrHoldExpiryAmendInvalid", newExpiry, err)
		}
	}
	// A zero expiry is not in the future either.
	if _, err := l.AmendHoldExpiry("h1", time.Time{}); !errors.Is(err, ErrHoldExpiryAmendInvalid) {
		t.Errorf("zero expiry: err = %v, want ErrHoldExpiryAmendInvalid", err)
	}
	// Every rejected amendment leaves the hold exactly as it was.
	if h, _ := l.GetHold("h1"); !h.ExpiresAt.Equal(before.ExpiresAt) {
		t.Errorf("hold after rejected amendments = %+v, want expiry %+v intact", h, before.ExpiresAt)
	}
	// Shortening is the point: an earlier-but-future expiry is legal.
	shortened, err := l.AmendHoldExpiry("h1", time.Now().Add(30*time.Minute))
	if err != nil {
		t.Fatalf("AmendHoldExpiry shorten: %v", err)
	}
	if shortened.ExpiresAt.After(time.Now().Add(31*time.Minute)) || !shortened.ExpiresAt.After(time.Now()) {
		t.Errorf("shortened expiry = %v, want ~30m in the future", shortened.ExpiresAt)
	}
	if shortened.AmountCents != 6000 || shortened.Status != HoldStatusActive {
		t.Errorf("shortened hold = %+v, want 6000, active", shortened)
	}
	// Lengthening past the old deadline is legal too.
	lengthened, err := l.AmendHoldExpiry("h1", time.Now().Add(3*time.Hour))
	if err != nil {
		t.Fatalf("AmendHoldExpiry lengthen: %v", err)
	}
	if !lengthened.ExpiresAt.After(time.Now().Add(2 * time.Hour)) {
		t.Errorf("lengthened expiry = %v, want ~3h in the future", lengthened.ExpiresAt)
	}
}

func TestAmendHoldExpiryTerminalStates(t *testing.T) {
	future := time.Now().Add(2 * time.Hour)
	// Released.
	l := extendHoldSeed(t, "h1", 6000)
	if _, err := l.Release("h1"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := l.AmendHoldExpiry("h1", future); !errors.Is(err, ErrHoldNotActive) {
		t.Errorf("released hold: err = %v, want ErrHoldNotActive", err)
	}
	// Fully captured.
	l2 := extendHoldSeed(t, "h2", 6000)
	if _, err := l2.Capture(Capture{ID: "c1", HoldID: "h2", To: "merchant", AmountCents: 6000}); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if _, err := l2.AmendHoldExpiry("h2", future); !errors.Is(err, ErrHoldNotActive) {
		t.Errorf("captured hold: err = %v, want ErrHoldNotActive", err)
	}
	// Swept-expired.
	l3 := extendHoldSeed(t, "h3", 6000)
	if got := l3.ExpireHoldsAt(time.Now().Add(2 * time.Hour)); got != 1 {
		t.Fatalf("sweep expired = %d, want 1", got)
	}
	if _, err := l3.AmendHoldExpiry("h3", future); !errors.Is(err, ErrHoldExpired) {
		t.Errorf("swept-expired hold: err = %v, want ErrHoldExpired", err)
	}
	// Lapsed but unswept: still expired, never resurrected.
	l4 := New()
	fundAccount(t, l4, "card", 10000, "")
	if _, _, err := l4.Hold(Hold{ID: "h4", Account: "card", AmountCents: 6000, ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatalf("Hold with past expiry: %v", err)
	}
	if _, err := l4.AmendHoldExpiry("h4", future); !errors.Is(err, ErrHoldExpired) {
		t.Errorf("lapsed hold: err = %v, want ErrHoldExpired", err)
	}
}

func TestAmendHoldExpiryMaxDurationCap(t *testing.T) {
	l := extendHoldSeed(t, "h1", 6000)
	if err := l.SetMaxHoldDuration(2 * time.Hour); err != nil {
		t.Fatalf("SetMaxHoldDuration: %v", err)
	}
	before, _ := l.GetHold("h1")
	// Beyond the live cap: rejected, original expiry untouched.
	if _, err := l.AmendHoldExpiry("h1", time.Now().Add(3*time.Hour)); !errors.Is(err, ErrHoldExceedsMaxDuration) {
		t.Errorf("over-cap expiry: err = %v, want ErrHoldExceedsMaxDuration", err)
	}
	if h, _ := l.GetHold("h1"); !h.ExpiresAt.Equal(before.ExpiresAt) {
		t.Errorf("hold after rejected amendment = %+v, want expiry %+v intact", h, before)
	}
	// Exactly at the cap boundary: admitted, like creation.
	if amended, err := l.AmendHoldExpiry("h1", time.Now().Add(2*time.Hour)); err != nil {
		t.Fatalf("AmendHoldExpiry at cap boundary: %v", err)
	} else if !amended.ExpiresAt.After(time.Now().Add(119 * time.Minute)) {
		t.Errorf("boundary expiry = %v, want ~2h in the future", amended.ExpiresAt)
	}
}

func TestAmendHoldExpirySweeperFollowsNewExpiry(t *testing.T) {
	l := extendHoldSeed(t, "h1", 6000)
	// Shorten to 30 minutes: the sweep at the old 1-hour deadline
	// now finds the hold already lapsed, and a sweep at 31 minutes
	// expires it.
	if _, err := l.AmendHoldExpiry("h1", time.Now().Add(30*time.Minute)); err != nil {
		t.Fatalf("AmendHoldExpiry shorten: %v", err)
	}
	if got := l.ExpireHoldsAt(time.Now().Add(31 * time.Minute)); got != 1 {
		t.Fatalf("sweep after shortened expiry = %d, want 1", got)
	}
	// Lengthen past the old deadline: the sweep at the old deadline
	// now finds the hold still alive.
	l2 := extendHoldSeed(t, "h2", 6000)
	if _, err := l2.AmendHoldExpiry("h2", time.Now().Add(3*time.Hour)); err != nil {
		t.Fatalf("AmendHoldExpiry lengthen: %v", err)
	}
	if got := l2.ExpireHoldsAt(time.Now().Add(61 * time.Minute)); got != 0 {
		t.Fatalf("sweep after lengthened expiry = %d, want 0", got)
	}
	if got := l2.ExpireHoldsAt(time.Now().Add(3*time.Hour + time.Minute)); got != 1 {
		t.Fatalf("sweep after new expiry = %d, want 1", got)
	}
}

func TestAmendHoldExpiryAuditAndSnapshot(t *testing.T) {
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
	versionBefore := l.Version()
	newExpiry := time.Now().Add(3 * time.Hour)
	ctx := ContextWithActor(context.Background(), "ops-desk")
	amended, err := l.AmendHoldExpiryCtx(ctx, "h1", newExpiry)
	if err != nil {
		t.Fatalf("AmendHoldExpiry: %v", err)
	}
	// Nothing but the expiry moved.
	if !amended.ExpiresAt.Equal(newExpiry) || amended.AmountCents != before.AmountCents ||
		amended.Account != before.Account || amended.Currency != before.Currency ||
		amended.IdempotencyKey != before.IdempotencyKey || !amended.CreatedAt.Equal(before.CreatedAt) ||
		amended.CapturedCents != before.CapturedCents {
		t.Errorf("amended hold = %+v, want only expiry changed from %+v", amended, before)
	}
	if l.Version() != versionBefore {
		t.Errorf("version moved: amendment is off-journal")
	}
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	after, ok := restored.GetHold("h1")
	if !ok {
		t.Fatal("restored hold missing")
	}
	if !after.ExpiresAt.Equal(newExpiry) || after.AmountCents != 6000 || after.Status != HoldStatusActive {
		t.Errorf("restored hold = %+v, want expiry %v, 6000, active", after, newExpiry)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["hold_expiry_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
	if events[0].Actor != "ops-desk" {
		t.Errorf("event actor = %q, want ops-desk", events[0].Actor)
	}
	d := events[0].Details
	if d["hold_id"] != "h1" || d["amount_cents"] != float64(6000) ||
		d["previous_expires_at"] != before.ExpiresAt.UTC().Format(time.RFC3339) ||
		d["expires_at"] != newExpiry.UTC().Format(time.RFC3339) {
		t.Errorf("event details = %+v, want h1 6000 with previous/new expiries", d)
	}
}
