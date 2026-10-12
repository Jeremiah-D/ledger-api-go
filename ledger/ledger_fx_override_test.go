package ledger

import (
	"errors"
	"fmt"
	"math/big"
	"sync"
	"testing"
	"time"
)

// Helpers for the manual FX rate override tests (LG-69).

func mustOverrideLedger(t *testing.T, opts ...Option) (*Ledger, *AuditLog) {
	t.Helper()
	al := mustAuditLog(t)
	base := []Option{WithAuditLog(al), WithFXAccount("fx-pnl")}
	l := New(append(base, opts...)...)
	return l, al
}

func mustProposeOverride(t *testing.T, l *Ledger, from, to string, num, den int64, ttl time.Duration, reason, proposer string) {
	t.Helper()
	if err := l.SetRateOverride(from, to, big.NewRat(num, den), ttl, reason, proposer); err != nil {
		t.Fatalf("SetRateOverride(%s->%s %d/%d): %v", from, to, num, den, err)
	}
}

func mustApproveOverride(t *testing.T, l *Ledger, from, to, approver string) {
	t.Helper()
	if err := l.ApproveRateOverride(from, to, approver); err != nil {
		t.Fatalf("ApproveRateOverride(%s->%s): %v", from, to, err)
	}
}

// activeOverride is propose+approve in one step: the happy path to an
// active override.
func activeOverride(t *testing.T, l *Ledger, from, to string, num, den int64, ttl time.Duration) {
	t.Helper()
	mustProposeOverride(t, l, from, to, num, den, ttl, "feed quote looks stale", "alice")
	mustApproveOverride(t, l, from, to, "bob")
}

func seedPayerUSD(t *testing.T, l *Ledger, account string, cents int64) {
	t.Helper()
	if _, _, err := l.Post(JournalEntry{ID: "seed-" + account, DebitAccount: AccountID(account), CreditAccount: "bank", AmountCents: cents, Currency: "USD"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func mustFXTransfer(t *testing.T, l *Ledger, id string, cents int64) TransferReceipt {
	t.Helper()
	rcpt, err := l.PostTransfer(Transfer{
		ID: id, From: "payer", To: "payee", AmountCents: cents,
		Currency: "USD", ToCurrency: "EUR",
	})
	if err != nil {
		t.Fatalf("PostTransfer FX: %v", err)
	}
	return rcpt
}

// readAuditOps closes the audit log and returns every recorded event,
// like TestFXRateWatchAuditEvents.
func readAuditOps(t *testing.T, al *AuditLog) []AuditEvent {
	t.Helper()
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, corrupt, err := ReadAuditLog(al.Dir(), time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt=%d, want 0", corrupt)
	}
	return events
}

func countOp(events []AuditEvent, op string) int {
	n := 0
	for _, e := range events {
		if e.Op == op {
			n++
		}
	}
	return n
}

func findOp(events []AuditEvent, op string) *AuditEvent {
	for i, e := range events {
		if e.Op == op {
			return &events[i]
		}
	}
	return nil
}

func TestRateOverrideProposeApproveFlow(t *testing.T) {
	l, _ := mustOverrideLedger(t)
	mustSetRate(t, l, "USD", "EUR", 108, 100) // market 1.08

	mustProposeOverride(t, l, "USD", "EUR", 100, 100, 30*time.Minute, "feed quote looks stale", "alice")
	o, ok := l.GetRateOverride("USD", "EUR")
	if !ok {
		t.Fatal("GetRateOverride: want proposed override")
	}
	if o.Status != RateOverrideStatusPending || o.Proposer != "alice" || o.Reason != "feed quote looks stale" {
		t.Errorf("bad pending override: %+v", o)
	}
	// big.Rat stores the reduced ratio (100/100 -> 1/1), like
	// SetFXRateRat — the value is exact either way.
	if o.Num != 1 || o.Den != 1 || o.MarketNum != 108 || o.MarketDen != 100 {
		t.Errorf("bad rates: %+v", o)
	}
	if o.TTL != (30 * time.Minute).String() {
		t.Errorf("TTL = %q, want %q", o.TTL, (30 * time.Minute).String())
	}
	if !o.ExpiresAt.IsZero() {
		t.Error("pending override must not have ExpiresAt yet")
	}

	mustApproveOverride(t, l, "USD", "EUR", "bob")
	o, _ = l.GetRateOverride("USD", "EUR")
	if o.Status != RateOverrideStatusActive || o.Approver != "bob" {
		t.Errorf("bad active override: %+v", o)
	}
	if o.ExpiresAt.Sub(o.ApprovedAt) != 30*time.Minute {
		t.Errorf("ExpiresAt-ApprovedAt = %v, want 30m", o.ExpiresAt.Sub(o.ApprovedAt))
	}
	if o.EffectiveVersion == 0 {
		// The seed post is version 0 here (no journal yet in this
		// test); the override still records the version it took
		// effect at explicitly.
		t.Logf("EffectiveVersion = %d", o.EffectiveVersion)
	}

	// Approving an already-active override is a no-op, not an error.
	if err := l.ApproveRateOverride("USD", "EUR", "carol"); err != nil {
		t.Errorf("re-approve active: want nil, got %v", err)
	}
	o, _ = l.GetRateOverride("USD", "EUR")
	if o.Approver != "bob" {
		t.Errorf("re-approve must not change approver: %+v", o)
	}
}

func TestRateOverrideMakerChecker(t *testing.T) {
	l, _ := mustOverrideLedger(t)
	mustSetRate(t, l, "USD", "EUR", 108, 100)

	mustProposeOverride(t, l, "USD", "EUR", 100, 100, 0, "stale feed", "alice")
	// The proposer can never approve their own override.
	if err := l.ApproveRateOverride("USD", "EUR", "alice"); !errors.Is(err, ErrRateOverrideSameApprover) {
		t.Errorf("self-approve: want ErrRateOverrideSameApprover, got %v", err)
	}
	o, _ := l.GetRateOverride("USD", "EUR")
	if o.Status != RateOverrideStatusPending {
		t.Errorf("failed approval must leave the override pending: %+v", o)
	}
	// An empty approver defaults to "operator" — still a valid second
	// person when the proposer is someone else.
	mustApproveOverride(t, l, "USD", "EUR", "")
	o, _ = l.GetRateOverride("USD", "EUR")
	if o.Approver != defaultReviewOperator {
		t.Errorf("approver = %q, want %q", o.Approver, defaultReviewOperator)
	}
}

func TestRateOverrideDefaultedIdentitiesBlock(t *testing.T) {
	l, _ := mustOverrideLedger(t)
	mustSetRate(t, l, "USD", "EUR", 108, 100)

	// Both sides default to "operator": maker-checker then correctly
	// refuses, forcing explicit operator names to complete the flow.
	mustProposeOverride(t, l, "USD", "EUR", 100, 100, 0, "stale feed", "")
	if err := l.ApproveRateOverride("USD", "EUR", ""); !errors.Is(err, ErrRateOverrideSameApprover) {
		t.Errorf("defaulted identities: want ErrRateOverrideSameApprover, got %v", err)
	}
}

func TestRateOverrideDeviationCap(t *testing.T) {
	l, _ := mustOverrideLedger(t)
	mustSetRate(t, l, "USD", "EUR", 100, 100) // market 1.00

	// Exactly at the 10% cap: allowed.
	mustProposeOverride(t, l, "USD", "EUR", 110, 100, 0, "at cap", "alice")
	if _, _, err := l.RejectRateOverride("USD", "EUR", "bob", "test cleanup"); err != nil {
		t.Fatalf("RejectRateOverride: %v", err)
	}
	// 11% over: rejected. The earlier reviewer-rejected record is
	// still there — the deviation rejection must not have replaced it.
	err := l.SetRateOverride("USD", "EUR", big.NewRat(111, 100), 0, "too far", "alice")
	if !errors.Is(err, ErrRateOverrideDeviationExceeded) {
		t.Errorf("11%% deviation: want ErrRateOverrideDeviationExceeded, got %v", err)
	}
	if o, ok := l.GetRateOverride("USD", "EUR"); !ok || o.Status != RateOverrideStatusRejected {
		t.Errorf("deviation rejection must leave state untouched: %+v, ok=%v", o, ok)
	}
	// 11% under: rejected too — the cap is symmetric.
	err = l.SetRateOverride("USD", "EUR", big.NewRat(89, 100), 0, "too far", "alice")
	if !errors.Is(err, ErrRateOverrideDeviationExceeded) {
		t.Errorf("-11%% deviation: want ErrRateOverrideDeviationExceeded, got %v", err)
	}

	// A tighter cap is construction-time config.
	l2, _ := mustOverrideLedger(t, WithRateOverrideMaxDeviationBps(500)) // 5%
	mustSetRate(t, l2, "USD", "EUR", 100, 100)
	if err := l2.SetRateOverride("USD", "EUR", big.NewRat(106, 100), 0, "over 5%", "alice"); !errors.Is(err, ErrRateOverrideDeviationExceeded) {
		t.Errorf("6%% vs 5%% cap: want ErrRateOverrideDeviationExceeded, got %v", err)
	}
	mustProposeOverride(t, l2, "USD", "EUR", 104, 100, 0, "within 5%", "alice")
}

func TestRateOverrideDeviationRecheckedAtApproval(t *testing.T) {
	l, _ := mustOverrideLedger(t)
	mustSetRate(t, l, "USD", "EUR", 100, 100)
	mustProposeOverride(t, l, "USD", "EUR", 105, 100, 0, "5% over", "alice")

	// The market moves 20% before the checker decides: approval must
	// refuse — the checker never rubber-stamps a stale price.
	mustSetRate(t, l, "USD", "EUR", 120, 100)
	err := l.ApproveRateOverride("USD", "EUR", "bob")
	if !errors.Is(err, ErrRateOverrideDeviationExceeded) {
		t.Fatalf("approve after market move: want ErrRateOverrideDeviationExceeded, got %v", err)
	}
	o, _ := l.GetRateOverride("USD", "EUR")
	if o.Status != RateOverrideStatusPending {
		t.Errorf("refused approval must leave the override pending: %+v", o)
	}
}

func TestRateOverrideValidation(t *testing.T) {
	l, _ := mustOverrideLedger(t)
	mustSetRate(t, l, "USD", "EUR", 108, 100)
	for _, tc := range []struct {
		name             string
		from, to         string
		rate             *big.Rat
		ttl              time.Duration
		reason, proposer string
	}{
		{"same currency", "USD", "USD", big.NewRat(1, 1), 0, "r", "alice"},
		{"bad from code", "us", "EUR", big.NewRat(1, 1), 0, "r", "alice"},
		{"bad to code", "USD", "EURO", big.NewRat(1, 1), 0, "r", "alice"},
		{"empty from", "", "EUR", big.NewRat(1, 1), 0, "r", "alice"},
		{"nil rate", "USD", "EUR", nil, 0, "r", "alice"},
		{"zero rate", "USD", "EUR", big.NewRat(0, 1), 0, "r", "alice"},
		{"negative rate", "USD", "EUR", big.NewRat(-1, 1), 0, "r", "alice"},
		{"empty reason", "USD", "EUR", big.NewRat(1, 1), 0, "", "alice"},
		{"negative ttl", "USD", "EUR", big.NewRat(1, 1), -time.Minute, "r", "alice"},
	} {
		if err := l.SetRateOverride(tc.from, tc.to, tc.rate, tc.ttl, tc.reason, tc.proposer); !errors.Is(err, ErrInvalidRateOverride) {
			t.Errorf("%s: want ErrInvalidRateOverride, got %v", tc.name, err)
		}
	}
	// A rejected proposal records nothing.
	if _, ok := l.GetRateOverride("USD", "USD"); ok {
		t.Error("invalid proposal must not be recorded")
	}
}

func TestRateOverrideNoMarketRate(t *testing.T) {
	l, _ := mustOverrideLedger(t)
	// No rate at all for the pair.
	if err := l.SetRateOverride("USD", "EUR", big.NewRat(1, 1), 0, "r", "alice"); !errors.Is(err, ErrRateOverrideNoMarketRate) {
		t.Errorf("no market rate: want ErrRateOverrideNoMarketRate, got %v", err)
	}
	// An expired market rate is not a live market rate either.
	if err := l.SetFXRateRat("USD", "JPY", big.NewRat(150, 1), time.Millisecond); err != nil {
		t.Fatalf("SetFXRateRat: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := l.SetRateOverride("USD", "JPY", big.NewRat(150, 1), 0, "r", "alice"); !errors.Is(err, ErrRateOverrideNoMarketRate) {
		t.Errorf("expired market rate: want ErrRateOverrideNoMarketRate, got %v", err)
	}
}

func TestRateOverrideConflict(t *testing.T) {
	l, _ := mustOverrideLedger(t)
	mustSetRate(t, l, "USD", "EUR", 108, 100)

	mustProposeOverride(t, l, "USD", "EUR", 100, 100, 0, "first", "alice")
	// A second proposal while one is pending conflicts.
	if err := l.SetRateOverride("USD", "EUR", big.NewRat(101, 100), 0, "second", "carol"); !errors.Is(err, ErrRateOverrideConflict) {
		t.Errorf("pending conflict: want ErrRateOverrideConflict, got %v", err)
	}
	mustApproveOverride(t, l, "USD", "EUR", "bob")
	// Still conflicts while active — one override per pair at a time.
	if err := l.SetRateOverride("USD", "EUR", big.NewRat(101, 100), 0, "second", "carol"); !errors.Is(err, ErrRateOverrideConflict) {
		t.Errorf("active conflict: want ErrRateOverrideConflict, got %v", err)
	}
	// After expiry the pair is free again.
	if n := l.ExpireRateOverridesAt(time.Now().Add(2 * time.Hour)); n != 1 {
		t.Fatalf("ExpireRateOverridesAt: want 1, got %d", n)
	}
	mustProposeOverride(t, l, "USD", "EUR", 101, 100, 0, "after expiry", "carol")
}

func TestRateOverrideReject(t *testing.T) {
	l, al := mustOverrideLedger(t)
	mustSetRate(t, l, "USD", "EUR", 108, 100)
	mustProposeOverride(t, l, "USD", "EUR", 100, 100, 0, "stale feed", "alice")

	o, changed, err := l.RejectRateOverride("USD", "EUR", "bob", "feed recovered")
	if err != nil || !changed {
		t.Fatalf("RejectRateOverride: %v, changed=%v", changed, err)
	}
	if o.Status != RateOverrideStatusRejected {
		t.Errorf("status = %q, want rejected", o.Status)
	}
	// Idempotent: rejecting again is a no-op.
	o2, changed, err := l.RejectRateOverride("USD", "EUR", "bob", "again")
	if err != nil || changed {
		t.Errorf("re-reject: want (as-is, false, nil), got changed=%v err=%v", changed, err)
	}
	if o2.Status != RateOverrideStatusRejected {
		t.Errorf("re-reject status = %q", o2.Status)
	}
	// Rejecting an active override is refused — it lapses on its own TTL.
	if err := l.SetFXRate("USD", "JPY", 150, 1); err != nil {
		t.Fatalf("SetFXRate: %v", err)
	}
	mustProposeOverride(t, l, "USD", "JPY", 145, 1, 0, "stale feed", "alice")
	mustApproveOverride(t, l, "USD", "JPY", "bob")
	if _, _, err := l.RejectRateOverride("USD", "JPY", "bob", "changed mind"); !errors.Is(err, ErrRateOverrideNotPending) {
		t.Errorf("reject active: want ErrRateOverrideNotPending, got %v", err)
	}
	// Approving a rejected override is refused.
	if err := l.ApproveRateOverride("USD", "EUR", "carol"); !errors.Is(err, ErrRateOverrideNotPending) {
		t.Errorf("approve rejected: want ErrRateOverrideNotPending, got %v", err)
	}
	// Unknown pair.
	if err := l.ApproveRateOverride("EUR", "USD", "carol"); !errors.Is(err, ErrRateOverrideNotFound) {
		t.Errorf("approve unknown: want ErrRateOverrideNotFound, got %v", err)
	}
	if _, _, err := l.RejectRateOverride("EUR", "USD", "carol", ""); !errors.Is(err, ErrRateOverrideNotFound) {
		t.Errorf("reject unknown: want ErrRateOverrideNotFound, got %v", err)
	}

	events := readAuditOps(t, al)
	if n := countOp(events, "fx_rate_override_proposed"); n != 2 {
		t.Errorf("fx_rate_override_proposed = %d, want 2", n)
	}
	if n := countOp(events, "fx_rate_override_rejected"); n != 1 {
		t.Errorf("fx_rate_override_rejected = %d, want 1", n)
	}
	if ev := findOp(events, "fx_rate_override_rejected"); ev == nil || ev.Details["reject_reason"] != "feed recovered" {
		t.Errorf("rejection event missing reject_reason: %+v", ev)
	}
}

func TestRateOverrideConversionUsesOverridePrice(t *testing.T) {
	l, al := mustOverrideLedger(t)
	seedPayerUSD(t, l, "payer", 100000)
	mustSetRate(t, l, "USD", "EUR", 108, 100) // market 1.08
	// 1.00 deviates 7.41% from 1.08 — inside the 10% cap.
	activeOverride(t, l, "USD", "EUR", 100, 100, time.Hour)

	rcpt := mustFXTransfer(t, l, "fx-ovr-1", 10000)
	if rcpt.FX == nil {
		t.Fatal("receipt must disclose FX conversion")
	}
	fx := rcpt.FX
	if !fx.OverrideApplied {
		t.Error("OverrideApplied = false, want true")
	}
	if fx.RateNum != 1 || fx.RateDen != 1 || fx.ConvertedCents != 10000 {
		t.Errorf("conversion priced at override: got %d/%d -> %d, want 1/1 -> 10000",
			fx.RateNum, fx.RateDen, fx.ConvertedCents)
	}
	if fx.MarketRateNum != 108 || fx.MarketRateDen != 100 {
		t.Errorf("market rate disclosure = %d/%d, want 108/100", fx.MarketRateNum, fx.MarketRateDen)
	}
	if fx.OverrideApprovedBy != "bob" {
		t.Errorf("OverrideApprovedBy = %q, want bob", fx.OverrideApprovedBy)
	}
	// The booked legs carry the override price too.
	if rcpt.Entries[0].AmountCents != 10000 || rcpt.Entries[0].Currency != "EUR" {
		t.Errorf("principal leg = %+v, want 10000 EUR", rcpt.Entries[0])
	}
	if n := l.FXRateOverrideConversionCount(); n != 1 {
		t.Errorf("conversion counter = %d, want 1", n)
	}

	events := readAuditOps(t, al)
	if n := countOp(events, "fx_rate_overridden"); n != 1 {
		t.Fatalf("fx_rate_overridden = %d, want 1", n)
	}
	ev := findOp(events, "fx_rate_overridden")
	d := ev.Details
	for k, want := range map[string]string{
		"override_rate": "1/1", "market_rate": "108/100",
		"proposed_by": "alice", "approved_by": "bob",
		"reason": "feed quote looks stale",
	} {
		if d[k] != want {
			t.Errorf("fx_rate_overridden[%q] = %v, want %q", k, d[k], want)
		}
	}
	if d["deviation_bps"] != "740.74" {
		t.Errorf("deviation_bps = %v, want 740.74", d["deviation_bps"])
	}
	if d["converted_cents"] != float64(10000) {
		t.Errorf("converted_cents = %v, want 10000", d["converted_cents"])
	}
	// The transfer event itself discloses the override as well.
	tev := findOp(events, "transfer")
	if tev == nil || tev.Details["override_applied"] != true {
		t.Errorf("transfer event missing override_applied: %+v", tev)
	}
}

func TestRateOverrideConversionInReviewPath(t *testing.T) {
	l, al := mustOverrideLedger(t, WithReviewThreshold(5000))
	seedPayerUSD(t, l, "payer", 100000)
	mustSetRate(t, l, "USD", "EUR", 108, 100)
	activeOverride(t, l, "USD", "EUR", 100, 100, time.Hour)

	// At the review threshold the transfer freezes instead of settling —
	// the frozen legs are still priced at the override.
	rcpt := mustFXTransfer(t, l, "fx-ovr-review", 10000)
	if rcpt.ReviewStatus != ReviewStatusPending {
		t.Fatalf("ReviewStatus = %q, want pending_review", rcpt.ReviewStatus)
	}
	if rcpt.FX == nil || !rcpt.FX.OverrideApplied || rcpt.FX.ConvertedCents != 10000 {
		t.Errorf("review receipt FX disclosure: %+v", rcpt.FX)
	}
	events := readAuditOps(t, al)
	if n := countOp(events, "fx_rate_overridden"); n != 1 {
		t.Errorf("fx_rate_overridden = %d, want 1 (frozen legs priced once)", n)
	}
}

func TestRateOverrideExpiry(t *testing.T) {
	l, al := mustOverrideLedger(t)
	seedPayerUSD(t, l, "payer", 100000)
	mustSetRate(t, l, "USD", "EUR", 108, 100)
	activeOverride(t, l, "USD", "EUR", 100, 100, time.Hour)

	o, _ := l.GetRateOverride("USD", "EUR")
	if o.Status != RateOverrideStatusActive {
		t.Fatalf("status = %q, want active", o.Status)
	}
	// Nothing to expire yet: no audit noise.
	if n := l.ExpireRateOverrides(); n != 0 {
		t.Errorf("ExpireRateOverrides = %d, want 0", n)
	}
	// Deterministic sweep past the TTL.
	if n := l.ExpireRateOverridesAt(o.ExpiresAt.Add(time.Second)); n != 1 {
		t.Fatalf("ExpireRateOverridesAt = %d, want 1", n)
	}
	o, _ = l.GetRateOverride("USD", "EUR")
	if o.Status != RateOverrideStatusExpired {
		t.Errorf("status = %q, want expired", o.Status)
	}
	// A lapsed override no longer prices conversions: market rate again.
	rcpt := mustFXTransfer(t, l, "fx-ovr-exp", 10000)
	if rcpt.FX.OverrideApplied {
		t.Error("expired override must not price the conversion")
	}
	if rcpt.FX.ConvertedCents != 10800 {
		t.Errorf("converted = %d, want 10800 at market", rcpt.FX.ConvertedCents)
	}
	if n := l.FXRateOverrideConversionCount(); n != 0 {
		t.Errorf("conversion counter = %d, want 0", n)
	}

	events := readAuditOps(t, al)
	if n := countOp(events, "fx_rate_override_expired"); n != 1 {
		t.Errorf("fx_rate_override_expired = %d, want 1", n)
	}
	if ev := findOp(events, "fx_rate_override_expired"); ev == nil || ev.Details["expired"] != float64(1) {
		t.Errorf("expiry event: %+v", ev)
	}
}

func TestRateOverrideLazyExpiryInConversionPath(t *testing.T) {
	l, _ := mustOverrideLedger(t)
	seedPayerUSD(t, l, "payer", 100000)
	mustSetRate(t, l, "USD", "EUR", 108, 100)
	// A 1ms TTL lapses before the transfer posts: the conversion path's
	// lazy predicate must see it as inactive even without a sweep.
	activeOverride(t, l, "USD", "EUR", 100, 100, time.Millisecond)
	time.Sleep(50 * time.Millisecond)

	rcpt := mustFXTransfer(t, l, "fx-ovr-lazy", 10000)
	if rcpt.FX.OverrideApplied {
		t.Error("lapsed override must not price the conversion (lazy expiry)")
	}
	if rcpt.FX.ConvertedCents != 10800 {
		t.Errorf("converted = %d, want 10800 at market", rcpt.FX.ConvertedCents)
	}
}

func TestRateOverrideDefaultTTL(t *testing.T) {
	l, _ := mustOverrideLedger(t, WithRateOverrideTTL(2*time.Hour))
	mustSetRate(t, l, "USD", "EUR", 108, 100)

	// Non-positive ttl selects the configured default.
	mustProposeOverride(t, l, "USD", "EUR", 100, 100, 0, "r", "alice")
	o, _ := l.GetRateOverride("USD", "EUR")
	if o.TTL != (2 * time.Hour).String() {
		t.Errorf("TTL = %q, want %q", o.TTL, (2 * time.Hour).String())
	}
	// The built-in default is one hour when unconfigured.
	l2, _ := mustOverrideLedger(t)
	mustSetRate(t, l2, "USD", "EUR", 108, 100)
	mustProposeOverride(t, l2, "USD", "EUR", 100, 100, 0, "r", "alice")
	o2, _ := l2.GetRateOverride("USD", "EUR")
	if o2.TTL != time.Hour.String() {
		t.Errorf("default TTL = %q, want %q", o2.TTL, time.Hour.String())
	}
}

func TestRateOverrideListAndCopy(t *testing.T) {
	l, _ := mustOverrideLedger(t)
	mustSetRate(t, l, "USD", "EUR", 108, 100)
	mustSetRate(t, l, "USD", "JPY", 150, 1)
	mustProposeOverride(t, l, "USD", "JPY", 145, 1, 0, "r", "alice")
	mustProposeOverride(t, l, "USD", "EUR", 100, 100, 0, "r", "alice")
	mustApproveOverride(t, l, "USD", "EUR", "bob")

	all := l.RateOverrides()
	if len(all) != 2 {
		t.Fatalf("RateOverrides = %d, want 2", len(all))
	}
	if all[0].ToCurrency != "EUR" || all[1].ToCurrency != "JPY" {
		t.Errorf("not sorted by (from,to): %+v", all)
	}
	// Copies: mutating the returned record must not affect the ledger.
	all[0].Status = "tampered"
	o, _ := l.GetRateOverride("USD", "EUR")
	if o.Status != RateOverrideStatusActive {
		t.Errorf("ledger mutated through returned copy: %+v", o)
	}
}

func TestWithRateOverrideMaxDeviationBpsPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("want panic for negative deviation cap")
		}
	}()
	_ = New(WithRateOverrideMaxDeviationBps(-1))
}

func TestRateOverrideConcurrent(t *testing.T) {
	l, _ := mustOverrideLedger(t)
	seedPayerUSD(t, l, "payer", 10_000_000)
	mustSetRate(t, l, "USD", "EUR", 108, 100)
	activeOverride(t, l, "USD", "EUR", 100, 100, time.Hour)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			l.GetRateOverride("USD", "EUR")
			l.RateOverrides()
			l.FXRateOverrideConversionCount()
			l.ExpireRateOverrides()
			rcpt, err := l.PostTransfer(Transfer{
				ID: fmt.Sprintf("fx-conc-%d", i), From: "payer", To: "payee",
				AmountCents: 1000, Currency: "USD", ToCurrency: "EUR",
			})
			if err != nil {
				t.Errorf("PostTransfer: %v", err)
				return
			}
			if !rcpt.FX.OverrideApplied || rcpt.FX.ConvertedCents != 1000 {
				t.Errorf("concurrent conversion not priced at override: %+v", rcpt.FX)
			}
		}(i)
	}
	wg.Wait()
	if n := l.FXRateOverrideConversionCount(); n != 8 {
		t.Errorf("conversion counter = %d, want 8", n)
	}
}
