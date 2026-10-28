package ledger

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestAmendCreditLineLimitValidation(t *testing.T) {
	l := New()
	if _, err := l.AmendCreditLineLimit("ghost", "USD", 80_000); !errors.Is(err, ErrCreditLineNotFound) {
		t.Errorf("unknown account: err = %v, want ErrCreditLineNotFound", err)
	}
	if _, err := l.GrantCreditLine("trader", 50_000, "USD", 800); err != nil {
		t.Fatalf("GrantCreditLine: %v", err)
	}
	// Granted in USD, not in EUR.
	if _, err := l.AmendCreditLineLimit("trader", "EUR", 80_000); !errors.Is(err, ErrCreditLineNotFound) {
		t.Errorf("ungranted currency: err = %v, want ErrCreditLineNotFound", err)
	}
	// The GrantCreditLine limit gate, applied to the amendment:
	// zero is the ReduceCreditLine revocation path, not a revision.
	for _, limit := range []int64{0, -5} {
		if _, err := l.AmendCreditLineLimit("trader", "USD", limit); !errors.Is(err, ErrCreditLineNonPositiveLimit) {
			t.Errorf("limit %d: err = %v, want ErrCreditLineNonPositiveLimit", limit, err)
		}
	}
	if _, err := l.AmendCreditLineLimit("", "USD", 80_000); !errors.Is(err, ErrEmptyCreditLineAccount) {
		t.Errorf("empty account: err = %v, want ErrEmptyCreditLineAccount", err)
	}
	if _, err := l.AmendCreditLineLimit("trader", "usd", 80_000); !errors.Is(err, ErrInvalidCurrency) {
		t.Errorf("bad currency: err = %v, want ErrInvalidCurrency", err)
	}
	// Every rejected amendment leaves the line exactly as it was.
	got, ok := l.GetCreditLine("trader", "USD")
	if !ok || got.LimitCents != 50_000 || got.RateBps != 800 {
		t.Errorf("line after rejected amendments = %+v ok=%v, want 50000/800", got, ok)
	}
	// A legal amendment changes the limit and nothing else.
	before := got
	amended, err := l.AmendCreditLineLimit("trader", "USD", 80_000)
	if err != nil {
		t.Fatalf("AmendCreditLineLimit: %v", err)
	}
	if amended.LimitCents != 80_000 {
		t.Errorf("amended limit = %d, want 80000", amended.LimitCents)
	}
	amended.LimitCents = before.LimitCents
	if amended != before {
		t.Errorf("amended line = %+v, want %+v except the limit", amended, before)
	}
}

func TestAmendCreditLineLimitBelowUtilization(t *testing.T) {
	l := New(WithOverdraftProtection("trader"))
	seedBalance(t, l, "trader", 100_000)
	if _, err := l.GrantCreditLine("trader", 50_000, "USD", 800); err != nil {
		t.Fatalf("grant: %v", err)
	}
	// Draw 40_000: balance -40_000, used = 40_000.
	if _, _, err := l.Post(JournalEntry{
		ID: "draw", DebitAccount: "vendor", CreditAccount: "trader", AmountCents: 140_000, Currency: "USD",
	}); err != nil {
		t.Fatalf("draw: %v", err)
	}
	// Shrinking below the drawn amount is refused, and the
	// original limit survives the refusal.
	if _, err := l.AmendCreditLineLimit("trader", "USD", 39_999); !errors.Is(err, ErrCreditLineBelowUtilization) {
		t.Errorf("below utilization: err = %v, want ErrCreditLineBelowUtilization", err)
	}
	if got, ok := l.GetCreditLine("trader", "USD"); !ok || got.LimitCents != 50_000 {
		t.Errorf("line after refused shrink = %+v ok=%v, want limit 50000", got, ok)
	}
	// A limit exactly equal to the utilization is legal: the
	// account sits at its floor, but nothing drawn is disturbed.
	if _, err := l.AmendCreditLineLimit("trader", "USD", 40_000); err != nil {
		t.Fatalf("amend to utilization: %v", err)
	}
	if _, _, err := l.Post(JournalEntry{
		ID: "over-floor", DebitAccount: "vendor", CreditAccount: "trader", AmountCents: 1, Currency: "USD",
	}); !errors.Is(err, ErrCreditLimitExceeded) {
		t.Errorf("one cent past the tightened floor: err = %v, want ErrCreditLimitExceeded", err)
	}
}

func TestAmendCreditLineLimitEffectiveNextPosting(t *testing.T) {
	// Raising the limit immediately widens the floor: a posting
	// that would have breached the old limit now clears.
	l := New(WithOverdraftProtection("trader"))
	seedBalance(t, l, "trader", 100_000)
	if _, err := l.GrantCreditLine("trader", 50_000, "USD", 800); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, _, err := l.Post(JournalEntry{
		ID: "draw", DebitAccount: "vendor", CreditAccount: "trader", AmountCents: 140_000, Currency: "USD",
	}); err != nil {
		t.Fatalf("draw: %v", err)
	}
	if _, err := l.AmendCreditLineLimit("trader", "USD", 80_000); err != nil {
		t.Fatalf("AmendCreditLineLimit raise: %v", err)
	}
	if _, _, err := l.Post(JournalEntry{
		ID: "draw-raised", DebitAccount: "vendor", CreditAccount: "trader", AmountCents: 35_000, Currency: "USD",
	}); err != nil {
		t.Fatalf("draw under raised limit: %v", err)
	}
	if used, _, limit, ok := l.CreditLineUtilization("trader", "USD"); !ok || limit != 80_000 || used != 75_000 {
		t.Fatalf("utilization after raise = used %d limit %d ok=%v, want 75000/80000", used, limit, ok)
	}

	// Lowering the limit immediately tightens the floor, while a
	// posting already journaled under the old limit stays put.
	l2 := New(WithOverdraftProtection("trader"))
	seedBalance(t, l2, "trader", 100_000)
	if _, err := l2.GrantCreditLine("trader", 50_000, "USD", 800); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, _, err := l2.Post(JournalEntry{
		ID: "draw", DebitAccount: "vendor", CreditAccount: "trader", AmountCents: 140_000, Currency: "USD",
	}); err != nil {
		t.Fatalf("draw: %v", err)
	}
	if _, err := l2.AmendCreditLineLimit("trader", "USD", 45_000); err != nil {
		t.Fatalf("AmendCreditLineLimit lower: %v", err)
	}
	if _, _, err := l2.Post(JournalEntry{
		ID: "draw-past", DebitAccount: "vendor", CreditAccount: "trader", AmountCents: 10_000, Currency: "USD",
	}); !errors.Is(err, ErrCreditLimitExceeded) {
		t.Fatalf("draw past lowered limit: err = %v, want ErrCreditLimitExceeded", err)
	}
	// Drawing exactly to the new floor still clears.
	if _, _, err := l2.Post(JournalEntry{
		ID: "draw-to-floor", DebitAccount: "vendor", CreditAccount: "trader", AmountCents: 5_000, Currency: "USD",
	}); err != nil {
		t.Fatalf("draw to lowered floor: %v", err)
	}
	if bal := l2.Balance("trader"); bal != -45_000 {
		t.Fatalf("balance after lowered-floor draw = %d, want -45000", bal)
	}
}

func TestAmendCreditLineLimitAuditAndSnapshot(t *testing.T) {
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
	if _, err := l.AmendCreditLineLimitCtx(ctx, "trader", "USD", 75_000); err != nil {
		t.Fatalf("AmendCreditLineLimit: %v", err)
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
	if after.LimitCents != 75_000 || after.RateBps != 800 || after.Account != "trader" || after.Currency != "USD" {
		t.Errorf("restored line = %+v, want trader/USD/75000/800", after)
	}
	// Snapshot serialization strips monotonic clock readings, so
	// compare GrantedAt by instant.
	if after.GrantedAt.UnixNano() != before.GrantedAt.UnixNano() {
		t.Errorf("restored granted_at = %v, want %v", after.GrantedAt, before.GrantedAt)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["credit_line_limit_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
	if events[0].Actor != "ops-desk" {
		t.Errorf("event actor = %q, want ops-desk", events[0].Actor)
	}
	d := events[0].Details
	if d["account"] != "trader" || d["currency"] != "USD" ||
		d["previous_limit_cents"] != float64(50_000) || d["limit_cents"] != float64(75_000) ||
		d["rate_bps"] != float64(800) {
		t.Errorf("event details = %+v, want trader/USD 50000->75000 rate 800", d)
	}
}
