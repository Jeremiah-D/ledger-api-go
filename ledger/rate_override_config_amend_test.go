package ledger

import (
	"bytes"
	"context"
	"errors"
	"math/big"
	"testing"
	"time"
)

func TestSetRateOverrideTTLAppliesToLaterProposals(t *testing.T) {
	l, _ := mustOverrideLedger(t)
	mustSetRate(t, l, "USD", "EUR", 108, 100)
	mustSetRate(t, l, "USD", "GBP", 127, 100)

	// Default one hour: a ttl <= 0 proposal stamps 1h.
	mustProposeOverride(t, l, "USD", "EUR", 100, 100, 0, "feed quote looks stale", "alice")
	o, ok := l.GetRateOverride("USD", "EUR")
	if !ok {
		t.Fatal("GetRateOverride: want proposed override")
	}
	if o.TTL != (time.Hour).String() {
		t.Errorf("default TTL = %q, want %q", o.TTL, (time.Hour).String())
	}

	// Revising the default to 45m governs only later proposals.
	if err := l.SetRateOverrideTTL(45 * time.Minute); err != nil {
		t.Fatalf("SetRateOverrideTTL: %v", err)
	}
	mustProposeOverride(t, l, "USD", "GBP", 120, 100, 0, "feed quote looks stale", "alice")
	g, ok := l.GetRateOverride("USD", "GBP")
	if !ok {
		t.Fatal("GetRateOverride GBP: want proposed override")
	}
	if g.TTL != (45 * time.Minute).String() {
		t.Errorf("revised TTL = %q, want %q", g.TTL, (45 * time.Minute).String())
	}

	// The earlier override keeps its stored TTL: lengthening the
	// default never extends a live (or pending) override.
	o, _ = l.GetRateOverride("USD", "EUR")
	if o.TTL != (time.Hour).String() {
		t.Errorf("earlier override TTL = %q, want %q (stored TTL must not move)", o.TTL, (time.Hour).String())
	}
}

func TestSetRateOverrideTTLValidation(t *testing.T) {
	l, _ := mustOverrideLedger(t)
	mustSetRate(t, l, "USD", "EUR", 108, 100)

	if err := l.SetRateOverrideTTL(0); !errors.Is(err, ErrInvalidRateOverrideTTL) {
		t.Errorf("SetRateOverrideTTL(0) = %v, want ErrInvalidRateOverrideTTL", err)
	}
	if err := l.SetRateOverrideTTL(-time.Hour); !errors.Is(err, ErrInvalidRateOverrideTTL) {
		t.Errorf("SetRateOverrideTTL(-1h) = %v, want ErrInvalidRateOverrideTTL", err)
	}

	// The rejected revisions leave the previous TTL exactly as it
	// was: a ttl <= 0 proposal still stamps the one-hour default.
	mustProposeOverride(t, l, "USD", "EUR", 100, 100, 0, "feed quote looks stale", "alice")
	o, _ := l.GetRateOverride("USD", "EUR")
	if o.TTL != (time.Hour).String() {
		t.Errorf("TTL after rejected revisions = %q, want %q", o.TTL, (time.Hour).String())
	}
}

func TestSetRateOverrideTTLNoOpWritesNoAudit(t *testing.T) {
	l, al := mustOverrideLedger(t)

	// Revising to the value already in force is a no-op.
	if err := l.SetRateOverrideTTL(time.Hour); err != nil {
		t.Fatalf("SetRateOverrideTTL(1h) = %v, want nil no-op", err)
	}
	if err := l.SetRateOverrideTTL(30 * time.Minute); err != nil {
		t.Fatalf("SetRateOverrideTTL(30m): %v", err)
	}
	if err := l.SetRateOverrideTTL(30 * time.Minute); err != nil {
		t.Fatalf("SetRateOverrideTTL(30m) again = %v, want nil no-op", err)
	}

	events := readAuditOps(t, al)
	if n := countOp(events, "fx_rate_override_ttl_amended"); n != 1 {
		t.Errorf("fx_rate_override_ttl_amended events = %d, want 1 (no-ops write none)", n)
	}
}

func TestSetRateOverrideMaxDeviationBpsAppliesToProposalsAndApprovals(t *testing.T) {
	l, _ := mustOverrideLedger(t)
	mustSetRate(t, l, "USD", "EUR", 108, 100) // market 1.08
	mustSetRate(t, l, "USD", "GBP", 127, 100) // market 1.27
	mustSetRate(t, l, "USD", "CHF", 90, 100)  // market 0.90

	// 5% above market proposes fine under the default 1000 bps cap.
	mustProposeOverride(t, l, "USD", "EUR", 1134, 1000, 30*time.Minute, "feed quote looks stale", "alice")

	// Tightening the cap to 100 bps (1%) refuses new 5% proposals.
	if err := l.SetRateOverrideMaxDeviationBps(100); err != nil {
		t.Fatalf("SetRateOverrideMaxDeviationBps(100): %v", err)
	}
	if err := l.SetRateOverride("USD", "GBP", big.NewRat(13335, 10000), 30*time.Minute, "feed quote looks stale", "alice"); !errors.Is(err, ErrRateOverrideDeviationExceeded) {
		t.Errorf("5%% GBP proposal under 100 bps cap = %v, want ErrRateOverrideDeviationExceeded", err)
	}

	// The pending 5% EUR override can no longer be approved under
	// the tightened cap — and it stays pending for a re-proposal.
	if err := l.ApproveRateOverride("USD", "EUR", "bob"); !errors.Is(err, ErrRateOverrideDeviationExceeded) {
		t.Errorf("ApproveRateOverride after tightening = %v, want ErrRateOverrideDeviationExceeded", err)
	}
	o, _ := l.GetRateOverride("USD", "EUR")
	if o.Status != RateOverrideStatusPending {
		t.Errorf("override status = %q, want pending after refused approval", o.Status)
	}

	// A 0.5% deviation still proposes and approves under the 1% cap.
	mustProposeOverride(t, l, "USD", "CHF", 9045, 10000, 30*time.Minute, "feed quote looks stale", "alice")
	mustApproveOverride(t, l, "USD", "CHF", "bob")
	o, _ = l.GetRateOverride("USD", "CHF")
	if o.Status != RateOverrideStatusActive {
		t.Errorf("CHF override status = %q, want active", o.Status)
	}
}

func TestSetRateOverrideMaxDeviationBpsValidation(t *testing.T) {
	l, _ := mustOverrideLedger(t)
	mustSetRate(t, l, "USD", "EUR", 108, 100)

	if err := l.SetRateOverrideMaxDeviationBps(-1); !errors.Is(err, ErrInvalidRateOverrideMaxDeviationBps) {
		t.Errorf("SetRateOverrideMaxDeviationBps(-1) = %v, want ErrInvalidRateOverrideMaxDeviationBps", err)
	}

	// The rejected revision leaves the 1000 bps cap in force: a 5%
	// deviation still proposes.
	mustProposeOverride(t, l, "USD", "EUR", 1134, 1000, 30*time.Minute, "feed quote looks stale", "alice")
}

func TestSetRateOverrideMaxDeviationBpsNoOpWritesNoAudit(t *testing.T) {
	l, al := mustOverrideLedger(t)

	if err := l.SetRateOverrideMaxDeviationBps(1000); err != nil {
		t.Fatalf("SetRateOverrideMaxDeviationBps(1000) = %v, want nil no-op", err)
	}
	if err := l.SetRateOverrideMaxDeviationBps(200); err != nil {
		t.Fatalf("SetRateOverrideMaxDeviationBps(200): %v", err)
	}
	if err := l.SetRateOverrideMaxDeviationBps(200); err != nil {
		t.Fatalf("SetRateOverrideMaxDeviationBps(200) again = %v, want nil no-op", err)
	}

	events := readAuditOps(t, al)
	if n := countOp(events, "fx_rate_override_max_deviation_bps_amended"); n != 1 {
		t.Errorf("fx_rate_override_max_deviation_bps_amended events = %d, want 1 (no-ops write none)", n)
	}
}

func TestRateOverrideConfigAmendAuditAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithFXAccount("fx-pnl"))
	mustSetRate(t, l, "USD", "EUR", 108, 100)

	ctx := ContextWithActor(context.Background(), "ops-desk")
	if err := l.SetRateOverrideTTLCtx(ctx, 30*time.Minute); err != nil {
		t.Fatalf("SetRateOverrideTTLCtx: %v", err)
	}
	if err := l.SetRateOverrideMaxDeviationBpsCtx(ctx, 200); err != nil {
		t.Fatalf("SetRateOverrideMaxDeviationBpsCtx: %v", err)
	}

	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}

	// Guardrails do not ride snapshots: the restored ledger is back
	// on the one-hour / 1000 bps defaults and must be re-set.
	mustSetRate(t, restored, "USD", "EUR", 108, 100)
	mustProposeOverride(t, restored, "USD", "EUR", 100, 100, 0, "feed quote looks stale", "alice")
	o, _ := restored.GetRateOverride("USD", "EUR")
	if o.TTL != (time.Hour).String() {
		t.Errorf("restored TTL = %q, want %q (defaults after restore)", o.TTL, (time.Hour).String())
	}
	if err := restored.SetRateOverrideMaxDeviationBps(100); err != nil {
		t.Fatalf("re-set cap on restored ledger: %v", err)
	}

	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)
	ttlEvents := events["fx_rate_override_ttl_amended"]
	if len(ttlEvents) != 1 {
		t.Fatalf("ttl amend events = %d, want 1", len(ttlEvents))
	}
	if ttlEvents[0].Actor != "ops-desk" {
		t.Errorf("ttl event actor = %q, want ops-desk", ttlEvents[0].Actor)
	}
	if ttlEvents[0].Details["previous_ttl"] != "1h0m0s" || ttlEvents[0].Details["ttl"] != "30m0s" {
		t.Errorf("ttl event details = %+v, want 1h0m0s -> 30m0s", ttlEvents[0].Details)
	}
	capEvents := events["fx_rate_override_max_deviation_bps_amended"]
	if len(capEvents) != 1 {
		t.Fatalf("cap amend events = %d, want 1", len(capEvents))
	}
	if capEvents[0].Actor != "ops-desk" {
		t.Errorf("cap event actor = %q, want ops-desk", capEvents[0].Actor)
	}
	if capEvents[0].Details["previous_max_deviation_bps"] != float64(1000) || capEvents[0].Details["max_deviation_bps"] != float64(200) {
		t.Errorf("cap event details = %+v, want 1000 -> 200", capEvents[0].Details)
	}
}
