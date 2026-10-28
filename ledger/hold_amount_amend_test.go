package ledger

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestAmendHoldAmountValidation(t *testing.T) {
	if _, err := New().AmendHoldAmount("h-unknown", 5000); !errors.Is(err, ErrHoldNotFound) {
		t.Errorf("unknown hold: err = %v, want ErrHoldNotFound", err)
	}
	// Non-positive amounts and amounts below the settled part.
	l := extendHoldSeed(t, "h1", 1000)
	if _, err := l.CapturePartial(PartialCapture{HoldID: "h1", To: "merchant", CaptureSeq: 1, AmountCents: 300}); err != nil {
		t.Fatalf("CapturePartial: %v", err)
	}
	for _, amount := range []int64{0, -100, 299} {
		want := ErrHoldNonPositiveAmount
		if amount == 299 {
			want = ErrHoldAmountBelowCaptured
		}
		if _, err := l.AmendHoldAmount("h1", amount); !errors.Is(err, want) {
			t.Errorf("amount %d: err = %v, want %v", amount, err, want)
		}
	}
	// Every rejected amendment leaves the hold exactly as it was.
	if h, _ := l.GetHold("h1"); h.AmountCents != 1000 || h.CapturedCents != 300 || h.Status != HoldStatusActive {
		t.Errorf("hold after rejected amendments = %+v, want 1000 of which 300 captured, active", h)
	}
	// Amending to exactly the captured amount is legal: the
	// remainder becomes zero without settling anything new.
	if amended, err := l.AmendHoldAmount("h1", 300); err != nil {
		t.Fatalf("AmendHoldAmount to captured floor: %v", err)
	} else if amended.AmountCents != 300 || amended.CapturedCents != 300 || amended.Status != HoldStatusActive {
		t.Errorf("amended hold = %+v, want 300 of 300 captured, still active", amended)
	}
}

func TestAmendHoldAmountTerminalStates(t *testing.T) {
	// Released.
	l := extendHoldSeed(t, "h1", 6000)
	if _, err := l.Release("h1"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := l.AmendHoldAmount("h1", 5000); !errors.Is(err, ErrHoldNotActive) {
		t.Errorf("released hold: err = %v, want ErrHoldNotActive", err)
	}
	// Fully captured.
	l2 := extendHoldSeed(t, "h2", 6000)
	if _, err := l2.Capture(Capture{ID: "c1", HoldID: "h2", To: "merchant", AmountCents: 6000}); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if _, err := l2.AmendHoldAmount("h2", 5000); !errors.Is(err, ErrHoldNotActive) {
		t.Errorf("captured hold: err = %v, want ErrHoldNotActive", err)
	}
	// Swept-expired.
	l3 := extendHoldSeed(t, "h3", 6000)
	if got := l3.ExpireHoldsAt(time.Now().Add(2 * time.Hour)); got != 1 {
		t.Fatalf("sweep expired = %d, want 1", got)
	}
	if _, err := l3.AmendHoldAmount("h3", 5000); !errors.Is(err, ErrHoldExpired) {
		t.Errorf("swept-expired hold: err = %v, want ErrHoldExpired", err)
	}
	// Lapsed but unswept: still expired, never resurrected.
	l4 := New()
	fundAccount(t, l4, "card", 10000, "")
	if _, _, err := l4.Hold(Hold{ID: "h4", Account: "card", AmountCents: 6000, ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatalf("Hold with past expiry: %v", err)
	}
	if _, err := l4.AmendHoldAmount("h4", 5000); !errors.Is(err, ErrHoldExpired) {
		t.Errorf("lapsed hold: err = %v, want ErrHoldExpired", err)
	}
}

func TestAmendHoldAmountIncreaseCoverage(t *testing.T) {
	// Card 10000, h1 6000, h2 3000: available is 1000, but h1's
	// own reservation does not count against its own increase.
	l := extendHoldSeed(t, "h1", 6000)
	if _, _, err := l.Hold(Hold{ID: "h2", Account: "card", AmountCents: 3000, ExpiresAt: futureExpiry()}); err != nil {
		t.Fatalf("second Hold: %v", err)
	}
	if got := l.Available("card"); got != 1000 {
		t.Fatalf("Available = %d, want 1000", got)
	}
	// To 7000 fits exactly: the other 3000 is the only reservation
	// left standing against the 10000 balance.
	if amended, err := l.AmendHoldAmount("h1", 7000); err != nil {
		t.Fatalf("AmendHoldAmount to 7000: %v", err)
	} else if amended.AmountCents != 7000 {
		t.Errorf("amended amount = %d, want 7000", amended.AmountCents)
	}
	if got := l.Available("card"); got != 0 {
		t.Errorf("Available = %d, want 0 after fitting increase", got)
	}
	// One cent further does not fit, and the hold is untouched.
	if _, err := l.AmendHoldAmount("h1", 7001); !errors.Is(err, ErrInsufficientAvailableFunds) {
		t.Errorf("over-cover increase: err = %v, want ErrInsufficientAvailableFunds", err)
	}
	if h, _ := l.GetHold("h1"); h.AmountCents != 7000 {
		t.Errorf("hold after rejected increase = %+v, want 7000 intact", h)
	}
	// A lone hold that already spans the whole balance cannot grow.
	l2 := extendHoldSeed(t, "h9", 10000)
	if _, err := l2.AmendHoldAmount("h9", 10001); !errors.Is(err, ErrInsufficientAvailableFunds) {
		t.Errorf("full-balance increase: err = %v, want ErrInsufficientAvailableFunds", err)
	}
}

func TestAmendHoldAmountDecreaseAndCapturePartial(t *testing.T) {
	l := extendHoldSeed(t, "h1", 1000)
	if _, err := l.CapturePartial(PartialCapture{HoldID: "h1", To: "merchant", CaptureSeq: 1, AmountCents: 300}); err != nil {
		t.Fatalf("CapturePartial: %v", err)
	}
	before, _ := l.GetHold("h1")
	versionBefore := l.Version()
	// Decrease: always safe, and it frees funds immediately.
	amended, err := l.AmendHoldAmount("h1", 500)
	if err != nil {
		t.Fatalf("AmendHoldAmount decrease: %v", err)
	}
	if amended.AmountCents != 500 || amended.CapturedCents != 300 || amended.Status != HoldStatusActive {
		t.Errorf("amended hold = %+v, want 500 of which 300 captured, active", amended)
	}
	// Nothing but the amount moved.
	if amended.Account != before.Account || amended.Currency != before.Currency ||
		!amended.ExpiresAt.Equal(before.ExpiresAt) || amended.IdempotencyKey != before.IdempotencyKey ||
		!amended.CreatedAt.Equal(before.CreatedAt) {
		t.Errorf("amended hold = %+v, want only amount changed from %+v", amended, before)
	}
	if l.Version() != versionBefore {
		t.Errorf("version moved: amendment is off-journal")
	}
	if got := l.Available("card"); got != 9500 {
		t.Errorf("Available = %d, want 9500 (9700 balance minus 200 remainder)", got)
	}
	// CapturePartial now settles against the new remainder: 200
	// completes the hold, and anything further is refused.
	rec, err := l.CapturePartial(PartialCapture{HoldID: "h1", To: "merchant", CaptureSeq: 2, AmountCents: 200})
	if err != nil {
		t.Fatalf("CapturePartial after amend: %v", err)
	}
	if rec.TotalCapturedCents != 500 || rec.RemainingCents != 0 {
		t.Errorf("receipt = %+v, want 500 total, 0 remaining", rec)
	}
	if h, _ := l.GetHold("h1"); h.Status != HoldStatusCaptured {
		t.Errorf("hold status = %q, want captured", h.Status)
	}
	if _, err := l.CapturePartial(PartialCapture{HoldID: "h1", To: "merchant", CaptureSeq: 3, AmountCents: 1}); !errors.Is(err, ErrHoldNotActive) {
		t.Errorf("capture after full settlement: err = %v, want ErrHoldNotActive", err)
	}
	// Amending to the current amount is a legal no-op.
	l2 := extendHoldSeed(t, "h2", 6000)
	if amended, err := l2.AmendHoldAmount("h2", 6000); err != nil {
		t.Fatalf("AmendHoldAmount same amount: %v", err)
	} else if amended.AmountCents != 6000 {
		t.Errorf("same-amount amend = %d, want 6000", amended.AmountCents)
	}
	if got := l2.Available("card"); got != 4000 {
		t.Errorf("Available = %d, want 4000 after same-amount amend", got)
	}
}

func TestAmendHoldAmountAuditAndSnapshot(t *testing.T) {
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
	if _, err := l.CapturePartial(PartialCapture{HoldID: "h1", To: "merchant", CaptureSeq: 1, AmountCents: 1000}); err != nil {
		t.Fatalf("CapturePartial: %v", err)
	}
	ctx := ContextWithActor(context.Background(), "ops-desk")
	if _, err := l.AmendHoldAmountCtx(ctx, "h1", 8000); err != nil {
		t.Fatalf("AmendHoldAmount: %v", err)
	}
	before, _ := l.GetHold("h1")
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
	if after.AmountCents != 8000 || after.CapturedCents != 1000 || after.Status != HoldStatusActive {
		t.Errorf("restored hold = %+v, want 8000 of which 1000 captured, active", after)
	}
	if after.ExpiresAt.UnixNano() != before.ExpiresAt.UnixNano() || after.Account != before.Account {
		t.Errorf("restored hold = %+v, want expiry/account of %+v", after, before)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["hold_amount_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
	if events[0].Actor != "ops-desk" {
		t.Errorf("event actor = %q, want ops-desk", events[0].Actor)
	}
	d := events[0].Details
	if d["hold_id"] != "h1" || d["previous_amount_cents"] != float64(6000) ||
		d["amount_cents"] != float64(8000) || d["captured_cents"] != float64(1000) {
		t.Errorf("event details = %+v, want h1 6000->8000 with 1000 captured", d)
	}
}
