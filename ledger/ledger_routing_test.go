package ledger

import (
	"errors"
	"testing"
	"time"
)

var routeTestNow = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

func TestRoutePaymentCostUSD(t *testing.T) {
	l := New()
	d, err := l.RoutePayment(RouteRequest{AmountCents: 5000, Currency: "USD", Now: routeTestNow})
	if err != nil {
		t.Fatalf("RoutePayment: %v", err)
	}
	// ACH is the cheapest USD rail (flat 5) — cost strategy picks it.
	if d.Rail != RailACH {
		t.Errorf("Rail = %q, want ach", d.Rail)
	}
	if d.Reason != RouteReasonCheapest {
		t.Errorf("Reason = %q, want cheapest", d.Reason)
	}
	if d.EstimatedCostCents != 5 {
		t.Errorf("EstimatedCostCents = %d, want 5", d.EstimatedCostCents)
	}
	if d.EstimatedDurationMs != (48 * time.Hour).Milliseconds() {
		t.Errorf("EstimatedDurationMs = %d, want 48h in ms", d.EstimatedDurationMs)
	}
	if !d.WouldMeetDeadline {
		t.Errorf("WouldMeetDeadline = false, want true (no deadline)")
	}
	if len(d.Feasible) != 6 {
		t.Errorf("Feasible has %d rows, want 6 (one per rail)", len(d.Feasible))
	}
	// SEPA must be excluded for USD, with the currency reason.
	for _, f := range d.Feasible {
		if f.Rail == RailSEPA && (f.Eligible || f.ExcludedReason != "currency-not-supported") {
			t.Errorf("sepa row = %+v, want excluded currency-not-supported", f)
		}
	}
}

func TestRoutePaymentSpeedUSD(t *testing.T) {
	l := New()
	d, err := l.RoutePayment(RouteRequest{
		AmountCents: 5000, Currency: "USD", Strategy: RouteStrategySpeed, Now: routeTestNow,
	})
	if err != nil {
		t.Fatalf("RoutePayment: %v", err)
	}
	// fednow and rtp tie at 20s and the same cost: canonical order wins.
	if d.Rail != RailFedNow {
		t.Errorf("Rail = %q, want fednow (deterministic tie-break)", d.Rail)
	}
	if d.Reason != RouteReasonFastest {
		t.Errorf("Reason = %q, want fastest", d.Reason)
	}
	if d.EstimatedDurationMs != (20 * time.Second).Milliseconds() {
		t.Errorf("EstimatedDurationMs = %d, want 20s in ms", d.EstimatedDurationMs)
	}
}

func TestRoutePaymentCostEUR(t *testing.T) {
	l := New()
	d, err := l.RoutePayment(RouteRequest{AmountCents: 5000, Currency: "EUR", Now: routeTestNow})
	if err != nil {
		t.Fatalf("RoutePayment: %v", err)
	}
	// USD-only rails are out; sepa (flat 10) beats swift/gpi (1500+).
	if d.Rail != RailSEPA {
		t.Errorf("Rail = %q, want sepa", d.Rail)
	}
	if d.Reason != RouteReasonCheapest {
		t.Errorf("Reason = %q, want cheapest", d.Reason)
	}
}

func TestRoutePaymentDeadline(t *testing.T) {
	l := New()
	d, err := l.RoutePayment(RouteRequest{
		AmountCents: 5000, Currency: "USD",
		Deadline:    routeTestNow.Add(time.Hour),
		Strategy:    RouteStrategyDeadline, Now: routeTestNow,
	})
	if err != nil {
		t.Fatalf("RoutePayment: %v", err)
	}
	// Only the 20s instant rails settle within an hour; fednow wins the
	// cost tie with rtp on canonical order.
	if d.Rail != RailFedNow {
		t.Errorf("Rail = %q, want fednow", d.Rail)
	}
	if d.Reason != RouteReasonDeadlineCheapest {
		t.Errorf("Reason = %q, want deadline-cheapest", d.Reason)
	}
	if !d.WouldMeetDeadline {
		t.Errorf("WouldMeetDeadline = false, want true")
	}
	for _, f := range d.Feasible {
		switch f.Rail {
		case RailFedNow, RailRTP:
			if !f.Eligible {
				t.Errorf("%s should be eligible: %+v", f.Rail, f)
			}
		case RailSWIFT, RailGPI, RailACH:
			if f.Eligible || f.ExcludedReason != "deadline-missed" {
				t.Errorf("%s = %+v, want excluded deadline-missed", f.Rail, f)
			}
		}
	}
}

func TestRoutePaymentNoFeasibleRail(t *testing.T) {
	l := New()
	d, err := l.RoutePayment(RouteRequest{
		AmountCents: 100, Currency: "USD",
		Deadline:    routeTestNow.Add(10 * time.Second),
		Strategy:    RouteStrategyDeadline, Now: routeTestNow,
	})
	if !errors.Is(err, ErrNoFeasibleRail) {
		t.Fatalf("err = %v, want ErrNoFeasibleRail", err)
	}
	if d.Rail != "" {
		t.Errorf("Rail = %q, want empty", d.Rail)
	}
	if d.Reason != RouteReasonNoFeasibleRail {
		t.Errorf("Reason = %q, want no-feasible-rail", d.Reason)
	}
	if d.WouldMeetDeadline {
		t.Errorf("WouldMeetDeadline = true, want false")
	}
	// Even the instant rails miss a 10s deadline (20s promise); the list
	// says why for every rail.
	for _, f := range d.Feasible {
		if f.Eligible || f.ExcludedReason == "" {
			t.Errorf("row %+v must be excluded with a reason", f)
		}
	}
}

func TestRoutePaymentAmountCap(t *testing.T) {
	l := New()
	// $2,000,000 exceeds the $1M FedNow/RTP/ACH cap: only the
	// cross-border rails remain. Both charge 1500 + 5bps; the cost tie
	// breaks toward the shorter duration, so gpi (24h) beats swift
	// (48h).
	d, err := l.RoutePayment(RouteRequest{AmountCents: 200_000_000, Currency: "USD", Now: routeTestNow})
	if err != nil {
		t.Fatalf("RoutePayment: %v", err)
	}
	if d.Rail != RailGPI {
		t.Errorf("Rail = %q, want gpi", d.Rail)
	}
	want := int64(1500) + policyFeeCents(200_000_000, 5)
	if d.EstimatedCostCents != want {
		t.Errorf("EstimatedCostCents = %d, want %d", d.EstimatedCostCents, want)
	}
	for _, f := range d.Feasible {
		if (f.Rail == RailFedNow || f.Rail == RailRTP || f.Rail == RailACH) &&
			(f.Eligible || f.ExcludedReason != "amount-exceeds-cap") {
			t.Errorf("%s = %+v, want excluded amount-exceeds-cap", f.Rail, f)
		}
	}
}

func TestRoutePaymentInvalidRequests(t *testing.T) {
	l := New()
	cases := []struct {
		name string
		req  RouteRequest
	}{
		{"zero amount", RouteRequest{AmountCents: 0}},
		{"negative amount", RouteRequest{AmountCents: -5}},
		{"bad currency", RouteRequest{AmountCents: 100, Currency: "US"}},
		{"bad strategy", RouteRequest{AmountCents: 100, Strategy: "teleport"}},
		{"deadline strategy without deadline", RouteRequest{AmountCents: 100, Strategy: "deadline"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.req.Now = routeTestNow
			if _, err := l.RoutePayment(tc.req); !errors.Is(err, ErrInvalidRouteRequest) {
				t.Errorf("err = %v, want ErrInvalidRouteRequest", err)
			}
		})
	}
}

func TestSetRailConfigOverride(t *testing.T) {
	l := New()
	// Make fednow free: the cost strategy must now prefer it over ACH.
	if err := l.SetRailConfig(RailFedNow, RailCapability{
		Currencies:         []string{"USD"},
		MaxAmountCents:     100_000_000,
		SettlementDuration: 20 * time.Second,
	}); err != nil {
		t.Fatalf("SetRailConfig: %v", err)
	}
	d, err := l.RoutePayment(RouteRequest{AmountCents: 5000, Currency: "USD", Now: routeTestNow})
	if err != nil {
		t.Fatalf("RoutePayment: %v", err)
	}
	if d.Rail != RailFedNow || d.EstimatedCostCents != 0 {
		t.Errorf("routed = (%q, %d), want (fednow, 0)", d.Rail, d.EstimatedCostCents)
	}
	got, err := l.RailConfig(RailFedNow)
	if err != nil {
		t.Fatalf("RailConfig: %v", err)
	}
	if got.FlatFeeCents != 0 || got.SettlementDuration != 20*time.Second {
		t.Errorf("RailConfig = %+v, want the override", got)
	}
	// Reset restores the default: ACH is cheapest again.
	removed, err := l.ResetRailConfig(RailFedNow)
	if err != nil || !removed {
		t.Fatalf("ResetRailConfig = (%v, %v), want (true, nil)", removed, err)
	}
	d2, err := l.RoutePayment(RouteRequest{AmountCents: 5000, Currency: "USD", Now: routeTestNow})
	if err != nil {
		t.Fatalf("RoutePayment after reset: %v", err)
	}
	if d2.Rail != RailACH {
		t.Errorf("after reset, Rail = %q, want ach", d2.Rail)
	}
	removed, err = l.ResetRailConfig(RailFedNow)
	if err != nil || removed {
		t.Errorf("second reset = (%v, %v), want (false, nil)", removed, err)
	}
}

func TestSetRailConfigValidation(t *testing.T) {
	l := New()
	base := RailCapability{Currencies: []string{"USD"}, SettlementDuration: time.Second}
	bad := []RailCapability{
		func() RailCapability { c := base; c.FlatFeeCents = -1; return c }(),
		func() RailCapability { c := base; c.FeeBps = 10001; return c }(),
		func() RailCapability { c := base; c.SettlementDuration = 0; return c }(),
		func() RailCapability { c := base; c.MaxAmountCents = -1; return c }(),
		func() RailCapability { c := base; c.Currencies = []string{"US"}; return c }(),
		func() RailCapability { c := base; c.Currencies = []string{"USD", "USD"}; return c }(),
		func() RailCapability { c := base; c.Rail = "rtp"; return c }(),
	}
	for i, c := range bad {
		if err := l.SetRailConfig(RailFedNow, c); !errors.Is(err, ErrInvalidRailConfig) {
			t.Errorf("case %d: err = %v, want ErrInvalidRailConfig", i, err)
		}
	}
	if err := l.SetRailConfig("zelle", base); !errors.Is(err, ErrInvalidRailConfig) {
		t.Errorf("unknown rail err = %v, want ErrInvalidRailConfig", err)
	}
}

// Every decision lands in the structured audit log — routed or not —
// while the journal, version and balances stay untouched.
func TestRoutePaymentAudit(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	seedBalance(t, l, "alice", 100000)

	vBefore, entriesBefore, balBefore := l.Version(), len(l.Entries()), l.Balance("alice")
	if _, err := l.RoutePayment(RouteRequest{AmountCents: 5000, Currency: "USD", Now: routeTestNow}); err != nil {
		t.Fatalf("routed: %v", err)
	}
	if _, err := l.RoutePayment(RouteRequest{
		AmountCents: 100, Currency: "USD", Deadline: routeTestNow.Add(time.Second),
		Strategy: RouteStrategyDeadline, Now: routeTestNow,
	}); !errors.Is(err, ErrNoFeasibleRail) {
		t.Fatalf("infeasible: %v", err)
	}
	if got := l.Version(); got != vBefore {
		t.Errorf("Version %d -> %d under routing", vBefore, got)
	}
	if got := len(l.Entries()); got != entriesBefore {
		t.Errorf("journal grew %d -> %d under routing", entriesBefore, got)
	}
	if got := l.Balance("alice"); got != balBefore {
		t.Errorf("alice balance %d -> %d under routing", balBefore, got)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	byOp := auditEventsByOp(t, dir)
	decisions := byOp["routing_decision"]
	if len(decisions) != 2 {
		t.Fatalf("routing_decision events = %d, want 2", len(decisions))
	}
	if decisions[0].Details["rail"] != RailACH || decisions[0].Details["outcome"] != "routed" {
		t.Errorf("routed event details = %v", decisions[0].Details)
	}
	if decisions[1].Details["outcome"] != "no-feasible-rail" {
		t.Errorf("infeasible event details = %v", decisions[1].Details)
	}
	if decisions[0].Details["feasible"] == "" {
		t.Errorf("routed event must carry the feasibility list")
	}
}

// Determinism: the same request always routes the same way, and the
// default table admits every vocabulary rail.
func TestRoutePaymentDeterminism(t *testing.T) {
	l := New()
	first := map[[2]int64]string{}
	for _, amt := range []int64{100, 5000, 50_000_000} {
		for _, cur := range []string{"USD", "EUR", "CNY"} {
			d, err := l.RoutePayment(RouteRequest{AmountCents: amt, Currency: cur, Now: routeTestNow})
			if err != nil {
				t.Fatalf("(%d %s): %v", amt, cur, err)
			}
			d2, err := l.RoutePayment(RouteRequest{AmountCents: amt, Currency: cur, Now: routeTestNow})
			if err != nil || d2.Rail != d.Rail || d2.Reason != d.Reason {
				t.Fatalf("(%d %s) not deterministic: %+v vs %+v", amt, cur, d, d2)
			}
			first[[2]int64{amt, int64(len(cur))}] = d.Rail
		}
	}
	for _, rail := range slaRails {
		if _, err := l.RailConfig(rail); err != nil {
			t.Errorf("RailConfig(%q): %v", rail, err)
		}
	}
}
