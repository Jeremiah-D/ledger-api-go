package ledger

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestAmendCreditLineRateValidation(t *testing.T) {
	l := New()
	if _, err := l.AmendCreditLineRate("ghost", "USD", 800); !errors.Is(err, ErrCreditLineNotFound) {
		t.Errorf("unknown account: err = %v, want ErrCreditLineNotFound", err)
	}
	if _, err := l.GrantCreditLine("trader", 50_000, "USD", 800); err != nil {
		t.Fatalf("GrantCreditLine: %v", err)
	}
	// Granted in USD, not in EUR.
	if _, err := l.AmendCreditLineRate("trader", "EUR", 800); !errors.Is(err, ErrCreditLineNotFound) {
		t.Errorf("ungranted currency: err = %v, want ErrCreditLineNotFound", err)
	}
	// The GrantCreditLine rate gate, applied to the amendment.
	for _, rate := range []int64{0, -5, 10001} {
		if _, err := l.AmendCreditLineRate("trader", "USD", rate); !errors.Is(err, ErrInvalidCreditLineRate) {
			t.Errorf("rate %d: err = %v, want ErrInvalidCreditLineRate", rate, err)
		}
	}
	if _, err := l.AmendCreditLineRate("", "USD", 800); !errors.Is(err, ErrEmptyCreditLineAccount) {
		t.Errorf("empty account: err = %v, want ErrEmptyCreditLineAccount", err)
	}
	if _, err := l.AmendCreditLineRate("trader", "usd", 800); !errors.Is(err, ErrInvalidCurrency) {
		t.Errorf("bad currency: err = %v, want ErrInvalidCurrency", err)
	}
	// Every rejected amendment leaves the line exactly as it was.
	got, ok := l.GetCreditLine("trader", "USD")
	if !ok || got.RateBps != 800 || got.LimitCents != 50_000 {
		t.Errorf("line after rejected amendments = %+v ok=%v, want 50000/800", got, ok)
	}
	// A legal amendment changes the rate and nothing else.
	before := got
	amended, err := l.AmendCreditLineRate("trader", "USD", 1200)
	if err != nil {
		t.Fatalf("AmendCreditLineRate: %v", err)
	}
	if amended.RateBps != 1200 {
		t.Errorf("amended rate = %d, want 1200", amended.RateBps)
	}
	amended.RateBps = before.RateBps
	if amended != before {
		t.Errorf("amended line = %+v, want %+v except the rate", amended, before)
	}
}

func TestAmendCreditLineRateEffectiveNextAccrual(t *testing.T) {
	l := New(WithCreditInterestIncome("interest-income"), WithOverdraftProtection("trader"))
	seedBalance(t, l, "trader", 100_000)
	if _, err := l.GrantCreditLine("trader", 50_000, "USD", 800); err != nil {
		t.Fatalf("grant: %v", err)
	}
	// Draw 40_000: balance -40_000, used = 40_000.
	if _, _, err := l.Post(JournalEntry{
		ID: "draw", DebitAccount: "vendor", CreditAccount: "trader", AmountCents: 140_000,
	}); err != nil {
		t.Fatalf("draw: %v", err)
	}
	day1 := time.Date(2026, 4, 2, 9, 0, 0, 0, time.UTC)
	e1, dup, err := l.AccrueCreditInterest("trader", "USD", day1)
	if err != nil || dup {
		t.Fatalf("day1 accrue: err=%v dup=%v", err, dup)
	}
	// floor(40_000 * 800 / 10000 / 365) = 8 at the original rate.
	if e1.AmountCents != 8 {
		t.Fatalf("day1 interest = %d, want 8", e1.AmountCents)
	}
	if _, err := l.AmendCreditLineRate("trader", "USD", 1600); err != nil {
		t.Fatalf("AmendCreditLineRate: %v", err)
	}
	// The limit and the utilization are untouched by the repricing.
	if used, _, limit, ok := l.CreditLineUtilization("trader", "USD"); !ok || limit != 50_000 || used != 40_008 {
		t.Fatalf("utilization after amend = used %d limit %d ok=%v, want 40008/50000", used, limit, ok)
	}
	day2 := time.Date(2026, 4, 3, 9, 0, 0, 0, time.UTC)
	e2, dup, err := l.AccrueCreditInterest("trader", "USD", day2)
	if err != nil || dup {
		t.Fatalf("day2 accrue: err=%v dup=%v", err, dup)
	}
	// floor(40_008 * 1600 / 10000 / 365) = 17 at the new rate —
	// the old rate would have charged 8 again.
	if e2.AmountCents != 17 {
		t.Errorf("day2 interest = %d, want 17 at the amended rate", e2.AmountCents)
	}
	// The old day replays at the old rate, never repriced.
	if orig, dup, err := l.AccrueCreditInterest("trader", "USD", day1); err != nil || !dup || orig.AmountCents != 8 {
		t.Errorf("day1 replay: err=%v dup=%v amount=%d, want original 8", err, dup, orig.AmountCents)
	}
}

func TestAmendCreditLineRateAuditAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	if _, err := l.GrantCreditLine("trader", 50_000, "USD", 800); err != nil {
		t.Fatalf("grant: %v", err)
	}
	before, _ := l.GetCreditLine("trader", "USD")
	ctx := ContextWithActor(context.Background(), "ops-desk")
	if _, err := l.AmendCreditLineRateCtx(ctx, "trader", "USD", 1200); err != nil {
		t.Fatalf("AmendCreditLineRate: %v", err)
	}
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	after, ok := restored.GetCreditLine("trader", "USD")
	if !ok {
		t.Fatal("restored line missing")
	}
	if after.RateBps != 1200 || after.LimitCents != 50_000 || after.Account != "trader" || after.Currency != "USD" {
		t.Errorf("restored line = %+v, want trader/USD/50000/1200", after)
	}
	// Snapshot serialization strips monotonic clock readings, so
	// compare GrantedAt by instant.
	if after.GrantedAt.UnixNano() != before.GrantedAt.UnixNano() {
		t.Errorf("restored granted_at = %v, want %v", after.GrantedAt, before.GrantedAt)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["credit_line_rate_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
	if events[0].Actor != "ops-desk" {
		t.Errorf("event actor = %q, want ops-desk", events[0].Actor)
	}
	d := events[0].Details
	if d["account"] != "trader" || d["currency"] != "USD" ||
		d["previous_rate_bps"] != float64(800) || d["rate_bps"] != float64(1200) ||
		d["limit_cents"] != float64(50_000) {
		t.Errorf("event details = %+v, want trader/USD 800->1200 limit 50000", d)
	}
}
