package ledger

import (
	"bytes"
	"context"
	"math/big"
	"strings"
	"testing"
	"time"
)

// TestSetFXRateProviderSwapAffectsRunningWatch is the LG-289 bug
// test: the watch used to capture the provider at StartFXRateWatch
// time, so swapping the source afterwards left every later tick
// polling the old feed. A running watch must follow the swap.
func TestSetFXRateProviderSwapAffectsRunningWatch(t *testing.T) {
	a := &stubFXProvider{rates: []MarketRate{
		{From: "USD", To: "EUR", Rate: big.NewRat(12, 10)},
	}}
	l, al := providerTestLedger(t, a)
	if err := l.SetFXRate("USD", "EUR", 11, 10); err != nil {
		t.Fatalf("SetFXRate: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := startProviderWatch(t, ctx, l)
	defer w.Stop()

	w.tick() // provider A moves 11/10 -> 6/5
	if r, ok := l.FXRate("USD", "EUR"); !ok || r.Num != 6 || r.Den != 5 {
		t.Fatalf("rate after A tick = %+v, want 6/5", r)
	}
	if a.calls != 1 {
		t.Fatalf("A calls = %d, want 1", a.calls)
	}

	b := &stubFXProvider{rates: []MarketRate{
		{From: "USD", To: "EUR", Rate: big.NewRat(13, 10)},
	}}
	versionBefore := l.Version()
	l.SetFXRateProvider(b)

	// The swap itself re-refreshes nothing and changes no stored
	// row: the table still holds A's 6/5 until the next tick.
	if r, ok := l.FXRate("USD", "EUR"); !ok || r.Num != 6 || r.Den != 5 {
		t.Fatalf("rate right after swap = %+v, want 6/5 (untouched)", r)
	}
	if got := l.FXRateRefreshCount(); got != 1 {
		t.Errorf("refreshes right after swap = %d, want 1", got)
	}
	if got := l.Version(); got != versionBefore {
		t.Errorf("Version after swap = %d, want %d (structural, no bump)", got, versionBefore)
	}

	w.tick() // the next tick must poll B, not A
	if a.calls != 1 {
		t.Errorf("A calls after swap tick = %d, want 1 (old source retired)", a.calls)
	}
	if b.calls != 1 {
		t.Fatalf("B calls after swap tick = %d, want 1", b.calls)
	}
	if r, ok := l.FXRate("USD", "EUR"); !ok || r.Num != 13 || r.Den != 10 {
		t.Fatalf("rate after B tick = %+v, want 13/10", r)
	}
	if got := l.FXRateRefreshCount(); got != 2 {
		t.Errorf("refreshes = %d, want 2", got)
	}

	var replaced *AuditEvent
	for _, e := range providerAuditEvents(t, al) {
		if e.Op == "fx_rate_provider_replaced" {
			ev := e
			replaced = &ev
		}
	}
	if replaced == nil {
		t.Fatal("no fx_rate_provider_replaced audit event")
	}
	if replaced.Actor != "SetFXRateProvider" {
		t.Errorf("actor = %q, want SetFXRateProvider", replaced.Actor)
	}
	if replaced.Details["previous_provider"] != true || replaced.Details["provider"] != true {
		t.Errorf("provider presence = %+v, want previous true, new true", replaced.Details)
	}
}

// TestSetFXRateProviderNilClearsProvider pins the constructor nil
// semantics (LG-74) for the runtime path: installing nil clears the
// source and the watch falls back to static-table behavior.
func TestSetFXRateProviderNilClearsProvider(t *testing.T) {
	a := &stubFXProvider{rates: []MarketRate{
		{From: "USD", To: "EUR", Rate: big.NewRat(12, 10)},
	}}
	l, al := providerTestLedger(t, a)
	if err := l.SetFXRate("USD", "EUR", 11, 10); err != nil {
		t.Fatalf("SetFXRate: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := startProviderWatch(t, ctx, l)
	defer w.Stop()

	w.tick()
	if a.calls != 1 {
		t.Fatalf("A calls = %d, want 1", a.calls)
	}

	l.SetFXRateProvider(nil)
	w.tick() // static-table tick: no fetch, no move
	if a.calls != 1 {
		t.Errorf("A calls after nil tick = %d, want 1 (source cleared)", a.calls)
	}
	if r, ok := l.FXRate("USD", "EUR"); !ok || r.Num != 6 || r.Den != 5 {
		t.Errorf("rate after nil tick = %+v, want 6/5 (static table kept)", r)
	}
	if got := l.FXRateRefreshCount(); got != 1 {
		t.Errorf("refreshes = %d, want 1", got)
	}

	// The static table itself is still operator-writable.
	// (SetFXRate stores the pair unreduced, unlike provider
	// quotes, which arrive via big.Rat.)
	if err := l.SetFXRate("USD", "EUR", 14, 10); err != nil {
		t.Fatalf("SetFXRate after clear: %v", err)
	}
	if r, ok := l.FXRate("USD", "EUR"); !ok || r.Num != 14 || r.Den != 10 {
		t.Errorf("rate after manual set = %+v, want 14/10", r)
	}

	var replaced *AuditEvent
	for _, e := range providerAuditEvents(t, al) {
		if e.Op == "fx_rate_provider_replaced" {
			ev := e
			replaced = &ev
		}
	}
	if replaced == nil {
		t.Fatal("no fx_rate_provider_replaced audit event for the clear")
	}
	if replaced.Details["previous_provider"] != true || replaced.Details["provider"] != false {
		t.Errorf("provider presence = %+v, want previous true, new false", replaced.Details)
	}
}

// TestSetFXRateProviderSameInstanceNoOpAndAudit: reinstalling the
// same instance is silent, a real swap lands exactly one event
// attributed to the Ctx operator (LG-73), and the plain twin keeps
// the entrypoint actor.
func TestSetFXRateProviderSameInstanceNoOpAndAudit(t *testing.T) {
	a := &stubFXProvider{rates: []MarketRate{
		{From: "USD", To: "EUR", Rate: big.NewRat(12, 10)},
	}}
	l, al := providerTestLedger(t, a)
	versionBefore := l.Version()

	l.SetFXRateProvider(a) // same instance: no-op

	b := &stubFXProvider{rates: []MarketRate{
		{From: "USD", To: "EUR", Rate: big.NewRat(13, 10)},
	}}
	ctx := ContextWithActor(context.Background(), "ops-desk")
	l.SetFXRateProviderCtx(ctx, b)
	l.SetFXRateProvider(b) // same instance again: no-op

	if got := l.Version(); got != versionBefore {
		t.Errorf("Version after swaps = %d, want %d (structural, no bump)", got, versionBefore)
	}
	events := providerAuditEvents(t, al)
	var replaced []AuditEvent
	for _, e := range events {
		if e.Op == "fx_rate_provider_replaced" {
			replaced = append(replaced, e)
		}
	}
	if len(replaced) != 1 {
		t.Fatalf("replaced events = %d, want 1 (both same-instance installs are silent)", len(replaced))
	}
	if replaced[0].Actor != "ops-desk" {
		t.Errorf("event actor = %q, want ops-desk", replaced[0].Actor)
	}
	if replaced[0].Details["previous_provider"] != true || replaced[0].Details["provider"] != true {
		t.Errorf("provider presence = %+v, want previous true, new true", replaced[0].Details)
	}
}

// TestSetFXRateProviderPlainActor: the non-Ctx twin attributes the
// event to the entrypoint name when no operator is carried.
func TestSetFXRateProviderPlainActor(t *testing.T) {
	a := &stubFXProvider{}
	l, al := providerTestLedger(t, a)
	b := &stubFXProvider{}
	l.SetFXRateProvider(b)
	var replaced *AuditEvent
	for _, e := range providerAuditEvents(t, al) {
		if e.Op == "fx_rate_provider_replaced" {
			ev := e
			replaced = &ev
		}
	}
	if replaced == nil {
		t.Fatal("no fx_rate_provider_replaced audit event")
	}
	if replaced.Actor != "SetFXRateProvider" {
		t.Errorf("actor = %q, want SetFXRateProvider", replaced.Actor)
	}
}

// TestSetFXRateProviderDirectRefreshUsesNewProvider: the direct
// refresh path reads the live field too — no watch needed.
func TestSetFXRateProviderDirectRefreshUsesNewProvider(t *testing.T) {
	a := &stubFXProvider{rates: []MarketRate{
		{From: "USD", To: "EUR", Rate: big.NewRat(12, 10)},
	}}
	l, al := providerTestLedger(t, a)
	defer al.Close()

	l.refreshFXRatesFromProvider(context.Background(), time.Now())
	if r, ok := l.FXRate("USD", "EUR"); !ok || r.Num != 6 || r.Den != 5 {
		t.Fatalf("rate after direct A refresh = %+v, want 6/5", r)
	}

	b := &stubFXProvider{rates: []MarketRate{
		{From: "USD", To: "EUR", Rate: big.NewRat(13, 10)},
	}}
	l.SetFXRateProvider(b)
	l.refreshFXRatesFromProvider(context.Background(), time.Now())
	if a.calls != 1 {
		t.Errorf("A calls = %d, want 1 (direct refresh follows the swap)", a.calls)
	}
	if b.calls != 1 {
		t.Fatalf("B calls = %d, want 1", b.calls)
	}
	if r, ok := l.FXRate("USD", "EUR"); !ok || r.Num != 13 || r.Den != 10 {
		t.Fatalf("rate after direct B refresh = %+v, want 13/10", r)
	}
}

// TestSetFXRateProviderInstallsFromNil: a ledger built without a
// provider starts polling on the first tick after the install.
func TestSetFXRateProviderInstallsFromNil(t *testing.T) {
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
	if got := l.FXRateRefreshCount(); got != 0 {
		t.Fatalf("refreshes before install = %d, want 0", got)
	}

	p := &stubFXProvider{rates: []MarketRate{
		{From: "USD", To: "EUR", Rate: big.NewRat(12, 10)},
	}}
	l.SetFXRateProvider(p)
	w.tick()
	if p.calls != 1 {
		t.Fatalf("provider calls = %d, want 1", p.calls)
	}
	if r, ok := l.FXRate("USD", "EUR"); !ok || r.Num != 6 || r.Den != 5 {
		t.Fatalf("rate after install tick = %+v, want 6/5", r)
	}

	var replaced *AuditEvent
	for _, e := range providerAuditEvents(t, al) {
		if e.Op == "fx_rate_provider_replaced" {
			ev := e
			replaced = &ev
		}
	}
	if replaced == nil {
		t.Fatal("no fx_rate_provider_replaced audit event for the install")
	}
	if replaced.Details["previous_provider"] != false || replaced.Details["provider"] != true {
		t.Errorf("provider presence = %+v, want previous false, new true", replaced.Details)
	}
}

// TestSetFXRateProviderNotCarriedBySnapshot: the provider is a
// caller-held adapter, not ledger state — a snapshot round-trip
// carries no provider row, the restored ledger polls nothing, and
// the operator must re-set the provider after restore (LG-287
// snapshot guardrails).
func TestSetFXRateProviderNotCarriedBySnapshot(t *testing.T) {
	a := &stubFXProvider{rates: []MarketRate{
		{From: "USD", To: "EUR", Rate: big.NewRat(12, 10)},
	}}
	l, al := providerTestLedger(t, a)
	defer al.Close()
	if err := l.SetFXRate("USD", "EUR", 11, 10); err != nil {
		t.Fatalf("SetFXRate: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := startProviderWatch(t, ctx, l)
	defer w.Stop()
	w.tick()
	if a.calls != 1 {
		t.Fatalf("A calls = %d, want 1", a.calls)
	}

	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	if strings.Contains(buf.String(), "fxRateProvider") {
		t.Error("snapshot carries an fxRateProvider row: the provider must not round-trip")
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if !snapshotLedgersEqual(l, restored) {
		t.Fatal("restored ledger state differs from source")
	}

	// The restored ledger has no provider: its tick never calls A
	// and the rate table stays exactly as snapshotted.
	w2 := startProviderWatch(t, ctx, restored)
	defer w2.Stop()
	w2.tick()
	if a.calls != 1 {
		t.Errorf("A calls after restored tick = %d, want 1 (no provider survived)", a.calls)
	}
	if r, ok := restored.FXRate("USD", "EUR"); !ok || r.Num != 6 || r.Den != 5 {
		t.Errorf("restored rate after tick = %+v, want 6/5", r)
	}
	if got := restored.FXRateRefreshCount(); got != 0 {
		t.Errorf("restored refreshes = %d, want 0", got)
	}

	// Re-setting the provider after restore resumes polling.
	a.rates = []MarketRate{{From: "USD", To: "EUR", Rate: big.NewRat(13, 10)}}
	restored.SetFXRateProvider(a)
	w2.tick()
	if a.calls != 2 {
		t.Fatalf("A calls after re-set tick = %d, want 2", a.calls)
	}
	if r, ok := restored.FXRate("USD", "EUR"); !ok || r.Num != 13 || r.Den != 10 {
		t.Fatalf("restored rate after re-set tick = %+v, want 13/10", r)
	}
}
