package ledger

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestAmendHoldCurrencySuccess(t *testing.T) {
	l := extendHoldSeed(t, "h1", 6000)
	fundAccount(t, l, "card", 10000, "EUR")
	before, _ := l.GetHold("h1")
	versionBefore := l.Version()
	got, err := l.AmendHoldCurrency("h1", "EUR")
	if err != nil {
		t.Fatalf("AmendHoldCurrency: %v", err)
	}
	// The amendment moves the currency and nothing else.
	if got.Currency != "EUR" {
		t.Errorf("currency = %q, want EUR", got.Currency)
	}
	if got.AmountCents != 6000 || got.CapturedCents != 0 || got.Status != HoldStatusActive ||
		!got.ExpiresAt.Equal(before.ExpiresAt) || got.IdempotencyKey != before.IdempotencyKey ||
		!got.CreatedAt.Equal(before.CreatedAt) {
		t.Errorf("amended hold = %+v, want 6000 active with original expiry/key/created_at", got)
	}
	if got.ID != "h1" || got.Account != "card" {
		t.Errorf("amended hold = %+v, want h1/card", got)
	}
	if l.Version() != versionBefore {
		t.Error("version moved: amendment is off-journal")
	}
	// The stored row carries the new currency, and the reservation
	// moved with it: USD available is whole again, EUR available
	// is reduced by the 6000 now reserved in EUR.
	stored, ok := l.GetHold("h1")
	if !ok || stored.Currency != "EUR" || stored.Status != HoldStatusActive {
		t.Errorf("stored hold = %+v,%v, want active with currency EUR", stored, ok)
	}
	if got := l.AvailableIn("card", "USD"); got != 10000 {
		t.Errorf("AvailableIn USD = %d, want 10000 after the reservation left USD", got)
	}
	if got := l.AvailableIn("card", "EUR"); got != 4000 {
		t.Errorf("AvailableIn EUR = %d, want 4000 after the reservation moved to EUR", got)
	}
	// Capture settles the live currency: the amended hold settles
	// in EUR, not the originally placed USD.
	receipt, err := l.Capture(Capture{ID: "c1", HoldID: "h1", To: "merchant", AmountCents: 6000})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if receipt.Entry.Currency != "EUR" || receipt.Entry.AmountCents != 6000 {
		t.Errorf("capture entry = %+v, want 6000 EUR", receipt.Entry)
	}
}

func TestAmendHoldCurrencyCoverage(t *testing.T) {
	// Card holds 10000 USD and only 5000 EUR: the 6000 remainder
	// does not fit EUR, and the hold is untouched.
	l := extendHoldSeed(t, "h1", 6000)
	fundAccount(t, l, "card", 5000, "EUR")
	if _, err := l.AmendHoldCurrency("h1", "EUR"); !errors.Is(err, ErrInsufficientAvailableFunds) {
		t.Errorf("short EUR: err = %v, want ErrInsufficientAvailableFunds", err)
	}
	if h, _ := l.GetHold("h1"); h.Currency != "USD" || h.AmountCents != 6000 {
		t.Errorf("hold after rejected amendment = %+v, want 6000 USD intact", h)
	}
	if got := l.AvailableIn("card", "USD"); got != 4000 {
		t.Errorf("AvailableIn USD = %d, want 4000 after rejected amendment", got)
	}
	// Exactly enough EUR fits: the boundary is inclusive.
	l2 := extendHoldSeed(t, "h1", 6000)
	fundAccount(t, l2, "card", 6000, "EUR")
	if amended, err := l2.AmendHoldCurrency("h1", "EUR"); err != nil {
		t.Fatalf("AmendHoldCurrency to exactly-covered EUR: %v", err)
	} else if amended.Currency != "EUR" {
		t.Errorf("amended currency = %q, want EUR", amended.Currency)
	}
	if got := l2.AvailableIn("card", "EUR"); got != 0 {
		t.Errorf("AvailableIn EUR = %d, want 0 after fitting move", got)
	}
}

func TestAmendHoldCurrencyValidation(t *testing.T) {
	if _, err := New().AmendHoldCurrency("h-unknown", "EUR"); !errors.Is(err, ErrHoldNotFound) {
		t.Errorf("unknown hold: err = %v, want ErrHoldNotFound", err)
	}
	// Empty and malformed codes all fail, and the original
	// currency is untouched.
	for _, code := range []string{"", "eur", "EU", "EURO", "E1R"} {
		l := extendHoldSeed(t, "h1", 6000)
		if _, err := l.AmendHoldCurrency("h1", code); !errors.Is(err, ErrInvalidCurrency) {
			t.Errorf("code %q: err = %v, want ErrInvalidCurrency", code, err)
		}
		if h, _ := l.GetHold("h1"); h.Currency != "USD" {
			t.Errorf("code %q: currency moved to %q after rejected amendment, want USD", code, h.Currency)
		}
	}
}

func TestAmendHoldCurrencyTerminalStates(t *testing.T) {
	// Released.
	l := extendHoldSeed(t, "h1", 6000)
	if _, err := l.Release("h1"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := l.AmendHoldCurrency("h1", "EUR"); !errors.Is(err, ErrHoldNotActive) {
		t.Errorf("released hold: err = %v, want ErrHoldNotActive", err)
	}
	// Fully captured.
	l2 := extendHoldSeed(t, "h2", 6000)
	if _, err := l2.Capture(Capture{ID: "c1", HoldID: "h2", To: "merchant", AmountCents: 6000}); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if _, err := l2.AmendHoldCurrency("h2", "EUR"); !errors.Is(err, ErrHoldNotActive) {
		t.Errorf("captured hold: err = %v, want ErrHoldNotActive", err)
	}
	// Swept-expired.
	l3 := extendHoldSeed(t, "h3", 6000)
	if got := l3.ExpireHoldsAt(time.Now().Add(2 * time.Hour)); got != 1 {
		t.Fatalf("sweep expired = %d, want 1", got)
	}
	if _, err := l3.AmendHoldCurrency("h3", "EUR"); !errors.Is(err, ErrHoldExpired) {
		t.Errorf("swept-expired hold: err = %v, want ErrHoldExpired", err)
	}
	// Lapsed but unswept: still expired, never resurrected.
	l4 := New()
	fundAccount(t, l4, "card", 10000, "")
	if _, _, err := l4.Hold(Hold{ID: "h4", Account: "card", AmountCents: 6000, ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatalf("Hold with past expiry: %v", err)
	}
	if _, err := l4.AmendHoldCurrency("h4", "EUR"); !errors.Is(err, ErrHoldExpired) {
		t.Errorf("lapsed hold: err = %v, want ErrHoldExpired", err)
	}
}

func TestAmendHoldCurrencyPartialCapture(t *testing.T) {
	// A partially captured hold moves only its remainder: 6000
	// USD of which 2000 already settled, so 4000 must fit EUR.
	l := extendHoldSeed(t, "h1", 6000)
	if _, err := l.CapturePartial(PartialCapture{HoldID: "h1", To: "merchant", CaptureSeq: 1, AmountCents: 2000}); err != nil {
		t.Fatalf("CapturePartial: %v", err)
	}
	// 3000 EUR does not cover the 4000 remainder.
	fundAccount(t, l, "card", 3000, "EUR")
	if _, err := l.AmendHoldCurrency("h1", "EUR"); !errors.Is(err, ErrInsufficientAvailableFunds) {
		t.Errorf("short EUR: err = %v, want ErrInsufficientAvailableFunds", err)
	}
	if h, _ := l.GetHold("h1"); h.Currency != "USD" || h.CapturedCents != 2000 {
		t.Errorf("hold after rejected amendment = %+v, want 6000 USD with 2000 captured", h)
	}
	// 4000 EUR covers it exactly, and the settled part stays put.
	l2 := extendHoldSeed(t, "h1", 6000)
	if _, err := l2.CapturePartial(PartialCapture{HoldID: "h1", To: "merchant", CaptureSeq: 1, AmountCents: 2000}); err != nil {
		t.Fatalf("CapturePartial: %v", err)
	}
	fundAccount(t, l2, "card", 4000, "EUR")
	amended, err := l2.AmendHoldCurrency("h1", "EUR")
	if err != nil {
		t.Fatalf("AmendHoldCurrency: %v", err)
	}
	if amended.Currency != "EUR" || amended.CapturedCents != 2000 || amended.AmountCents != 6000 {
		t.Errorf("amended hold = %+v, want 6000 EUR with 2000 captured", amended)
	}
	// The remaining 4000 settles in EUR afterwards.
	receipt, err := l2.CapturePartial(PartialCapture{HoldID: "h1", To: "merchant", CaptureSeq: 2, AmountCents: 4000})
	if err != nil {
		t.Fatalf("CapturePartial remainder: %v", err)
	}
	if receipt.Entry.Currency != "EUR" || receipt.Entry.AmountCents != 4000 {
		t.Errorf("remainder capture entry = %+v, want 4000 EUR", receipt.Entry)
	}
}

func TestAmendHoldCurrencyNoop(t *testing.T) {
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
	got, err := l.AmendHoldCurrency("h1", "USD")
	if err != nil {
		t.Fatalf("AmendHoldCurrency same value: %v", err)
	}
	if got.Currency != "USD" || got.Status != HoldStatusActive {
		t.Errorf("no-op hold = %+v, want USD active", got)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if events := auditEventsByOp(t, dir)["hold_currency_amended"]; len(events) != 0 {
		t.Errorf("amend events = %d, want 0 for a no-op", len(events))
	}
}

func TestAmendHoldCurrencyAudit(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	fundAccount(t, l, "card", 10000, "")
	fundAccount(t, l, "card", 10000, "EUR")
	if _, _, err := l.Hold(Hold{ID: "h1", Account: "card", AmountCents: 6000, ExpiresAt: futureExpiry()}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	ctx := ContextWithActor(context.Background(), "ops-alice")
	if _, err := l.AmendHoldCurrencyCtx(ctx, "h1", "EUR"); err != nil {
		t.Fatalf("AmendHoldCurrencyCtx: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["hold_currency_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
	if events[0].Actor != "ops-alice" {
		t.Errorf("event actor = %q, want ops-alice", events[0].Actor)
	}
	d := events[0].Details
	if d["hold_id"] != "h1" || d["previous_currency"] != "USD" ||
		d["currency"] != "EUR" || d["amount_cents"] != float64(6000) ||
		d["captured_cents"] != float64(0) || d["account"] != "card" {
		t.Errorf("event details = %+v, want h1 USD->EUR 6000 captured 0 card", d)
	}
}

func TestAmendHoldCurrencySnapshot(t *testing.T) {
	l := extendHoldSeed(t, "h1", 6000)
	fundAccount(t, l, "card", 10000, "EUR")
	before, _ := l.GetHold("h1")
	if _, err := l.AmendHoldCurrency("h1", "EUR"); err != nil {
		t.Fatalf("AmendHoldCurrency: %v", err)
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
	if after.Currency != "EUR" {
		t.Errorf("restored currency = %q, want EUR", after.Currency)
	}
	if after.ID != before.ID || after.Account != before.Account ||
		after.AmountCents != before.AmountCents || after.CapturedCents != before.CapturedCents ||
		after.Status != before.Status || !after.ExpiresAt.Equal(before.ExpiresAt) {
		t.Errorf("restored hold = %+v, want %+v except the currency", after, before)
	}
}
