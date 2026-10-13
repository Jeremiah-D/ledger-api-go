package ledger

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"
)

// stubFXProvider is the caller-implemented FXRateProvider for tests:
// canned quotes, an optional fetch error, and a call counter.
type stubFXProvider struct {
	rates []MarketRate
	err   error
	calls int
}

func (s *stubFXProvider) FetchRates(ctx context.Context) ([]MarketRate, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return s.rates, nil
}

// providerTestLedger builds a ledger with an audit log and the given
// provider installed. The caller must Close the returned AuditLog after
// the last tick (closing flushes the async queue before ReadAuditLog).
func providerTestLedger(t *testing.T, p *stubFXProvider) (*Ledger, *AuditLog) {
	t.Helper()
	al := mustAuditLog(t)
	l := New(WithAuditLog(al), WithFXRateProvider(p))
	return l, al
}

// startProviderWatch starts an FX-rate watch on the ledger with a long
// interval so only the test's explicit w.tick() calls run the refresh.
func startProviderWatch(t *testing.T, ctx context.Context, l *Ledger) *FXRateWatch {
	t.Helper()
	w := StartFXRateWatch(ctx, l, time.Hour, 0, nil)
	if w == nil {
		t.Fatal("StartFXRateWatch returned nil")
	}
	return w
}

// providerAuditEvents closes the audit log and returns every recorded
// event in order.
func providerAuditEvents(t *testing.T, al *AuditLog) []AuditEvent {
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

func TestFXRateProviderRefreshMovesPrice(t *testing.T) {
	p := &stubFXProvider{rates: []MarketRate{
		{From: "USD", To: "EUR", Rate: big.NewRat(12, 10)},
	}}
	l, al := providerTestLedger(t, p)
	if err := l.SetFXRate("USD", "EUR", 11, 10); err != nil {
		t.Fatalf("SetFXRate: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := startProviderWatch(t, ctx, l)
	defer w.Stop()

	w.tick()

	r, ok := l.FXRate("USD", "EUR")
	if !ok {
		t.Fatal("USD->EUR rate missing after provider refresh")
	}
	// big.Rat reduces to lowest terms: 12/10 is stored as 6/5.
	if r.Num != 6 || r.Den != 5 {
		t.Errorf("rate = %d/%d, want 6/5", r.Num, r.Den)
	}
	if got := l.FXRateRefreshCount(); got != 1 {
		t.Errorf("refreshes = %d, want 1", got)
	}
	if got := l.FXRateMovedCount(); got != 1 {
		t.Errorf("moves = %d, want 1", got)
	}
	if got := l.FXRateProviderFailCount(); got != 0 {
		t.Errorf("failures = %d, want 0", got)
	}
	// The refresh goes through the SetFXRateRat write path: the table
	// snapshot version moves with it.
	if v := l.FXRateSnapshotVersion(); v == 0 {
		t.Error("snapshot version = 0 after provider refresh, want > 0")
	}

	var moved *AuditEvent
	for _, e := range providerAuditEvents(t, al) {
		if e.Op == "fx_rate_moved" {
			ev := e
			moved = &ev
		}
	}
	if moved == nil {
		t.Fatal("no fx_rate_moved audit event")
	}
	if moved.Actor != "FXRateWatch" {
		t.Errorf("actor = %q, want FXRateWatch", moved.Actor)
	}
	d := moved.Details
	if d["from_currency"] != "USD" || d["to_currency"] != "EUR" {
		t.Errorf("pair details = %v, want USD->EUR", d)
	}
	if d["old_rate"] != "11/10" || d["new_rate"] != "6/5" {
		t.Errorf("rate details = old %v new %v, want 11/10 -> 6/5", d["old_rate"], d["new_rate"])
	}
	if d["provider"] == nil || d["provider"] == "" {
		t.Errorf("moved event missing provider source: %v", d)
	}
}

func TestFXRateProviderNewPair(t *testing.T) {
	p := &stubFXProvider{rates: []MarketRate{
		{From: "USD", To: "JPY", Rate: big.NewRat(150, 1)},
	}}
	l, al := providerTestLedger(t, p)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := startProviderWatch(t, ctx, l)
	defer w.Stop()

	w.tick()

	r, ok := l.FXRate("USD", "JPY")
	if !ok {
		t.Fatal("USD->JPY rate missing after provider refresh")
	}
	if r.Num != 150 || r.Den != 1 {
		t.Errorf("rate = %d/%d, want 150/1", r.Num, r.Den)
	}
	var moved *AuditEvent
	for _, e := range providerAuditEvents(t, al) {
		if e.Op == "fx_rate_moved" {
			ev := e
			moved = &ev
		}
	}
	if moved == nil {
		t.Fatal("no fx_rate_moved audit event for new pair")
	}
	if moved.Details["old_rate"] != "none" {
		t.Errorf("old_rate = %v, want \"none\" for a new pair", moved.Details["old_rate"])
	}
	if moved.Details["new_rate"] != "150/1" {
		t.Errorf("new_rate = %v, want 150/1", moved.Details["new_rate"])
	}
}

func TestFXRateProviderSkipsOverriddenPair(t *testing.T) {
	p := &stubFXProvider{rates: []MarketRate{
		{From: "USD", To: "EUR", Rate: big.NewRat(12, 10)},
		{From: "USD", To: "GBP", Rate: big.NewRat(8, 10)},
	}}
	l, al := providerTestLedger(t, p)
	if err := l.SetFXRate("USD", "EUR", 11, 10); err != nil {
		t.Fatalf("SetFXRate: %v", err)
	}
	if err := l.SetFXRate("USD", "GBP", 79, 100); err != nil {
		t.Fatalf("SetFXRate: %v", err)
	}
	// Maker-checker override on USD->EUR: 115/100 deviates ~4.5% from
	// 11/10, inside the default 10% cap.
	if err := l.SetRateOverride("USD", "EUR", big.NewRat(115, 100), time.Hour, "feed dislocation", "alice"); err != nil {
		t.Fatalf("SetRateOverride: %v", err)
	}
	if err := l.ApproveRateOverride("USD", "EUR", "bob"); err != nil {
		t.Fatalf("ApproveRateOverride: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := startProviderWatch(t, ctx, l)
	defer w.Stop()

	w.tick()

	// The override is stored separately from the market table: assert
	// it is active, and that the feed's quote never landed in the
	// table — the market rate stays 11/10, not the provider's 12/10.
	if o, ok := l.GetRateOverride("USD", "EUR"); !ok || o.Status != RateOverrideStatusActive {
		t.Fatalf("override not active: %+v, ok=%v", o, ok)
	}
	r, ok := l.FXRate("USD", "EUR")
	if !ok {
		t.Fatal("USD->EUR rate missing after tick")
	}
	if r.Num != 11 || r.Den != 10 {
		t.Errorf("overridden pair's market rate = %d/%d, want 11/10 (feed skipped)", r.Num, r.Den)
	}
	// The pair without an override follows the feed.
	r, ok = l.FXRate("USD", "GBP")
	if !ok {
		t.Fatal("USD->GBP rate missing after tick")
	}
	// big.Rat reduces 8/10 to 4/5 on the way in.
	if r.Num != 4 || r.Den != 5 {
		t.Errorf("USD->GBP rate = %d/%d, want 4/5", r.Num, r.Den)
	}
	if got := l.FXRateRefreshCount(); got != 1 {
		t.Errorf("refreshes = %d, want 1 (EUR skipped)", got)
	}
	if got := l.FXRateMovedCount(); got != 1 {
		t.Errorf("moves = %d, want 1 (EUR skipped)", got)
	}
	for _, e := range providerAuditEvents(t, al) {
		if e.Op == "fx_rate_moved" && e.Details["to_currency"] == "EUR" {
			t.Errorf("fx_rate_moved emitted for the overridden pair: %+v", e.Details)
		}
	}
}

func TestFXRateProviderFailureKeepsWorkerAlive(t *testing.T) {
	p := &stubFXProvider{err: errors.New("feed unreachable")}
	l, al := providerTestLedger(t, p)
	if err := l.SetFXRate("USD", "EUR", 11, 10); err != nil {
		t.Fatalf("SetFXRate: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := startProviderWatch(t, ctx, l)
	defer w.Stop()

	w.tick() // the fetch fails
	if p.calls != 1 {
		t.Fatalf("provider calls = %d, want 1", p.calls)
	}

	// The feed recovers: the next tick fetches and installs normally —
	// the failure did not kill the worker.
	p.err = nil
	p.rates = []MarketRate{{From: "USD", To: "EUR", Rate: big.NewRat(12, 10)}}
	w.tick()

	if got := w.Ticks(); got != 2 {
		t.Errorf("ticks = %d, want 2", got)
	}
	if p.calls != 2 {
		t.Errorf("provider calls = %d, want 2", p.calls)
	}
	if got := l.FXRateProviderFailCount(); got != 1 {
		t.Errorf("failures = %d, want 1", got)
	}
	r, ok := l.FXRate("USD", "EUR")
	if !ok || r.Num != 6 || r.Den != 5 {
		t.Errorf("rate after recovery = %+v, want 6/5", r)
	}

	var failed, moved bool
	for _, e := range providerAuditEvents(t, al) {
		switch e.Op {
		case "fx_rate_provider_failed":
			failed = true
			if e.Actor != "FXRateWatch" {
				t.Errorf("failed event actor = %q, want FXRateWatch", e.Actor)
			}
			if e.Details["error"] == nil || e.Details["error"] == "" {
				t.Errorf("failed event missing error detail: %v", e.Details)
			}
		case "fx_rate_moved":
			moved = true
		}
	}
	if !failed {
		t.Error("no fx_rate_provider_failed audit event: the failure was silent")
	}
	if !moved {
		t.Error("no fx_rate_moved audit event after recovery")
	}
}

func TestFXRateWatchNilProviderUnchanged(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al)) // no provider: static table behavior
	if err := l.SetFXRate("USD", "EUR", 11, 10); err != nil {
		t.Fatalf("SetFXRate: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := startProviderWatch(t, ctx, l)
	defer w.Stop()

	w.tick()

	r, ok := l.FXRate("USD", "EUR")
	if !ok || r.Num != 11 || r.Den != 10 {
		t.Errorf("static rate changed without provider: %+v", r)
	}
	if got := l.FXRateRefreshCount(); got != 0 {
		t.Errorf("refreshes = %d, want 0", got)
	}
	if got := l.FXRateMovedCount(); got != 0 {
		t.Errorf("moves = %d, want 0", got)
	}
	if got := l.FXRateProviderFailCount(); got != 0 {
		t.Errorf("failures = %d, want 0", got)
	}
	for _, e := range providerAuditEvents(t, al) {
		if e.Op == "fx_rate_moved" || e.Op == "fx_rate_provider_failed" {
			t.Errorf("unexpected provider event with nil provider: %s", e.Op)
		}
	}
}

func TestFXRateProviderUnchangedQuoteNoChurn(t *testing.T) {
	p := &stubFXProvider{rates: []MarketRate{
		{From: "USD", To: "EUR", Rate: big.NewRat(11, 10)}, // same as the table
	}}
	l, al := providerTestLedger(t, p)
	defer al.Close()
	if err := l.SetFXRate("USD", "EUR", 11, 10); err != nil {
		t.Fatalf("SetFXRate: %v", err)
	}
	snapBefore := l.FXRateSnapshotVersion()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := startProviderWatch(t, ctx, l)
	defer w.Stop()

	w.tick()
	w.tick()

	if got := l.FXRateRefreshCount(); got != 0 {
		t.Errorf("refreshes = %d, want 0 (unchanged quote)", got)
	}
	if got := l.FXRateMovedCount(); got != 0 {
		t.Errorf("moves = %d, want 0 (unchanged quote)", got)
	}
	if v := l.FXRateSnapshotVersion(); v != snapBefore {
		t.Errorf("snapshot version %d -> %d on unchanged quotes: churn", snapBefore, v)
	}
}

func TestFXRateProviderTTLRestampWithoutMove(t *testing.T) {
	p := &stubFXProvider{rates: []MarketRate{
		{From: "USD", To: "EUR", Rate: big.NewRat(11, 10), TTL: time.Hour},
	}}
	l, al := providerTestLedger(t, p)
	defer al.Close()
	if err := l.SetFXRate("USD", "EUR", 11, 10); err != nil { // no expiry
		t.Fatalf("SetFXRate: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := startProviderWatch(t, ctx, l)
	defer w.Stop()

	w.tick()

	r, ok := l.FXRate("USD", "EUR")
	if !ok {
		t.Fatal("USD->EUR rate missing after tick")
	}
	if r.ExpiresAt.IsZero() {
		t.Error("TTL quote did not re-stamp the rate expiry")
	}
	if got := l.FXRateRefreshCount(); got != 1 {
		t.Errorf("refreshes = %d, want 1", got)
	}
	if got := l.FXRateMovedCount(); got != 0 {
		t.Errorf("moves = %d, want 0 (price did not move)", got)
	}
}

func TestFXRateProviderIgnoresInvalidQuotes(t *testing.T) {
	p := &stubFXProvider{rates: []MarketRate{
		{From: "USD", To: "EUR", Rate: nil},                                // nil rate
		{From: "USD", To: "USD", Rate: big.NewRat(2, 1)},                   // same-currency pair
		{From: "USD", To: "GBP", Rate: big.NewRat(-1, 1)},                  // negative rate
		{From: "USD", To: "CHF", Rate: big.NewRat(9, 10), TTL: -time.Hour}, // negative TTL
		{From: "USD", To: "JPY", Rate: big.NewRat(150, 1)},                 // valid
	}}
	l, al := providerTestLedger(t, p)
	defer al.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := startProviderWatch(t, ctx, l)
	defer w.Stop()

	w.tick()

	for _, pair := range [][2]string{{"USD", "EUR"}, {"USD", "GBP"}, {"USD", "CHF"}} {
		if _, ok := l.FXRate(pair[0], pair[1]); ok {
			t.Errorf("invalid quote installed a rate for %s->%s", pair[0], pair[1])
		}
	}
	r, ok := l.FXRate("USD", "JPY")
	if !ok || r.Num != 150 || r.Den != 1 {
		t.Errorf("valid quote not installed: %+v, ok=%v", r, ok)
	}
	if got := l.FXRateMovedCount(); got != 1 {
		t.Errorf("moves = %d, want 1 (only the valid quote)", got)
	}
}
