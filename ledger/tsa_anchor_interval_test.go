package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TSA timestamp-anchoring interval runtime revision tests (LG-328).
// They reuse newTestTSA / newTSATestLedger / postAnchorEntries from
// ledger_tsa_test.go, secondTestTSAEndpoint / readTSAAnchors from
// tsa_url_amend_test.go, mustAuditLog from ledger_audit_test.go, and
// waitFor from ledger_hold_sweep_test.go.

// startRetuneTestAnchorer starts a timestamp anchorer on a ledger
// with an audit log attached, anchored against the test TSA. The
// interval is long by default so only direct ticks drive anchors
// unless the test waits on real ticks. It returns the test TSA, the
// ledger, the audit directory, the audit log, the anchorer, and the
// worker context's cancel func.
func startRetuneTestAnchorer(t *testing.T, interval time.Duration) (*testTSA, *Ledger, string, *AuditLog, *TimestampAnchorer, context.CancelFunc) {
	t.Helper()
	ts := newTestTSA(t)
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithTSAURL(ts.srv.URL), WithTSATrustCert(ts.cert))
	postAnchorEntries(t, l, "tsa-retune", 3)

	ctx, cancel := context.WithCancel(context.Background())
	a := StartTimestampAnchoring(ctx, l, interval, &RFC3161Client{URL: ts.srv.URL}, nil)
	if a == nil {
		t.Fatal("StartTimestampAnchoring returned nil for positive interval")
	}
	return ts, l, dir, al, a, cancel
}

// waitForAnchorerTicks waits until the anchorer has completed at
// least n ticks, or fails the test on timeout.
func waitForAnchorerTicks(t *testing.T, a *TimestampAnchorer, n uint64, timeout time.Duration) {
	t.Helper()
	waitFor(t, timeout, "TSA anchorer ticks", func() bool { return a.Ticks() >= n })
}

// retunedTSAAnchorIntervalEvents stops the anchorer, closes the audit
// log (which drains the async writer), and returns every
// tsa_anchor_interval_retuned event recorded in it. The ledger must
// not be used after this call.
func retunedTSAAnchorIntervalEvents(t *testing.T, a *TimestampAnchorer, al *AuditLog) []AuditEvent {
	t.Helper()
	a.Stop()
	if err := al.Close(); err != nil {
		t.Fatalf("Close audit log: %v", err)
	}
	events, corrupt, err := ReadAuditLog(al.Dir(), time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt events = %d, want 0", corrupt)
	}
	var out []AuditEvent
	for _, ev := range events {
		if ev.Op == "tsa_anchor_interval_retuned" {
			out = append(out, ev)
		}
	}
	return out
}

func TestSetTSAAnchorIntervalInvalidRejectedAndKeepsLive(t *testing.T) {
	_, _, _, al, a, cancel := startRetuneTestAnchorer(t, time.Hour)
	defer cancel()
	defer func() { _ = al.Close() }()

	for _, d := range []time.Duration{0, -time.Hour, -time.Nanosecond} {
		prev, err := a.SetInterval(d)
		if !errors.Is(err, ErrInvalidTSAAnchorInterval) {
			t.Fatalf("SetInterval(%v): err = %v, want ErrInvalidTSAAnchorInterval", d, err)
		}
		if prev != time.Hour {
			t.Fatalf("SetInterval(%v) returned %v, want the live 1h", d, prev)
		}
		if got := a.Interval(); got != time.Hour {
			t.Fatalf("Interval() = %v after rejected %v, want still 1h", got, d)
		}
	}
	// The Ctx variant rejects the same way.
	if prev, err := a.SetIntervalCtx(context.Background(), 0); !errors.Is(err, ErrInvalidTSAAnchorInterval) || prev != time.Hour {
		t.Fatalf("SetIntervalCtx(0) = (%v, %v), want (1h, ErrInvalidTSAAnchorInterval)", prev, err)
	}
	// Zero is rejected here, unlike the startup-time rule where a
	// non-positive interval disables the worker: a runtime revision
	// never silently stops the anchorer.
	if events := retunedTSAAnchorIntervalEvents(t, a, al); len(events) != 0 {
		t.Fatalf("retuned events = %d, want 0 after only rejected revisions", len(events))
	}
}

func TestSetTSAAnchorIntervalSameValueNoOpNoAudit(t *testing.T) {
	_, _, _, al, a, cancel := startRetuneTestAnchorer(t, time.Hour)
	defer cancel()
	defer func() { _ = al.Close() }()

	prev, err := a.SetInterval(time.Hour)
	if err != nil {
		t.Fatalf("same-value SetInterval: %v", err)
	}
	if prev != time.Hour {
		t.Fatalf("same-value SetInterval returned %v, want 1h", prev)
	}
	if got := a.Interval(); got != time.Hour {
		t.Fatalf("Interval() = %v after same-value revision, want 1h", got)
	}
	if events := retunedTSAAnchorIntervalEvents(t, a, al); len(events) != 0 {
		t.Fatalf("retuned events = %d, want 0 after a same-value no-op", len(events))
	}
}

func TestSetTSAAnchorIntervalReturnsPreviousValue(t *testing.T) {
	_, _, _, al, a, cancel := startRetuneTestAnchorer(t, time.Hour)
	defer cancel()
	defer func() { _ = al.Close() }()

	prev, err := a.SetInterval(5 * time.Minute)
	if err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	if prev != time.Hour {
		t.Fatalf("SetInterval returned %v, want the previous 1h", prev)
	}
	if got := a.Interval(); got != 5*time.Minute {
		t.Fatalf("Interval() = %v, want 5m after the revision", got)
	}
	// A second revision returns the first revision's value.
	if prev, err := a.SetInterval(30 * time.Second); err != nil || prev != 5*time.Minute {
		t.Fatalf("second SetInterval = (%v, %v), want (5m, nil)", prev, err)
	}
}

func TestSetTSAAnchorIntervalNextTickUsesNewInterval(t *testing.T) {
	ts, _, _, al, a, cancel := startRetuneTestAnchorer(t, 500*time.Millisecond)
	defer cancel()
	defer a.Stop()
	defer func() { _ = al.Close() }()

	// The first tick lands on the old 500ms cadence.
	waitForAnchorerTicks(t, a, 1, 3*time.Second)
	if got := a.Ticks(); got != 1 {
		t.Fatalf("ticks after first wait = %d, want exactly 1 on the 500ms cadence", got)
	}

	// Tighten to 20ms: the next ticks must accelerate — three more
	// ticks land in ~60ms, not ~1.5s on the old rhythm.
	if _, err := a.SetInterval(20 * time.Millisecond); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	if got := a.Interval(); got != 20*time.Millisecond {
		t.Fatalf("Interval() = %v, want 20ms after the revision", got)
	}
	start := time.Now()
	waitForAnchorerTicks(t, a, 4, 3*time.Second)
	if took := time.Since(start); took > time.Second {
		t.Fatalf("3 ticks after tightening took %s, want < 1s on the 20ms rhythm (would take ~1.5s on the old 500ms rhythm)", took)
	}
	// Idempotency: a quiet ledger is countersigned once, however
	// often the worker ticks — the accelerated ticks make no new
	// TSA round-trips.
	if got := ts.requests.Load(); got != 1 {
		t.Fatalf("TSA requests = %d after accelerated ticks, want 1 (same head, no re-anchor)", got)
	}
}

// blockingTestAuthority blocks the first Timestamp call until
// release is closed, then delegates to the real RFC3161 client. It
// lets a test hold an anchor observably in flight.
type blockingTestAuthority struct {
	client  *RFC3161Client
	started chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (b *blockingTestAuthority) Timestamp(ctx context.Context, digest [32]byte) (TimestampToken, error) {
	if b.calls.Add(1) == 1 {
		close(b.started)
		select {
		case <-b.release:
		case <-ctx.Done():
			return TimestampToken{}, ctx.Err()
		}
	}
	return b.client.Timestamp(ctx, digest)
}

func TestSetTSAAnchorIntervalInFlightAnchorNotInterrupted(t *testing.T) {
	ts := newTestTSA(t)
	l, _, al := newTSAURLTestLedger(t, ts.srv.URL)
	if err := l.SetTSATrustCert(ts.cert); err != nil {
		t.Fatalf("SetTSATrustCert: %v", err)
	}
	postAnchorEntries(t, l, "tsa-inflight", 3)
	defer func() { _ = al.Close() }()

	auth := &blockingTestAuthority{
		client:  &RFC3161Client{URL: ts.srv.URL},
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := StartTimestampAnchoring(ctx, l, 10*time.Millisecond, auth, nil)
	if a == nil {
		t.Fatal("StartTimestampAnchoring returned nil for positive interval")
	}
	defer a.Stop()

	// Wait until the first anchor is observably in flight inside
	// the authority, then let the ticker fire again while it is
	// blocked: a tick is now due that the loop has not consumed.
	select {
	case <-auth.started:
	case <-time.After(3 * time.Second):
		t.Fatal("first anchor never started")
	}
	time.Sleep(50 * time.Millisecond)

	// Revise mid-anchor: the revision must not be cut short, and
	// neither must the in-flight anchor.
	prev, err := a.SetInterval(500 * time.Millisecond)
	if err != nil {
		t.Fatalf("SetInterval mid-anchor: %v", err)
	}
	if prev != 10*time.Millisecond {
		t.Fatalf("SetInterval mid-anchor returned %v, want 10ms", prev)
	}
	close(auth.release)

	// The in-flight anchor completes (tick 1), and the tick that
	// fired during the revision runs too (tick 2) — via the retune
	// branch's drain. Either way both land well inside the new
	// 500ms cadence.
	waitForAnchorerTicks(t, a, 2, 3*time.Second)
	if got := a.Ticks(); got != 2 {
		t.Fatalf("ticks = %d after the revision, want exactly 2 (in-flight anchor + drained tick)", got)
	}
	// The next tick follows the loosened 500ms cadence: nothing
	// within 200ms of the drained tick, then a tick arrives.
	time.Sleep(200 * time.Millisecond)
	if n := a.Ticks(); n != 2 {
		t.Fatalf("ticks = %d within 200ms of the drained tick, want still 2 on the loosened 500ms rhythm", n)
	}
	// By now both anchor calls have finished: the in-flight anchor
	// and the drained tick (an idempotent success for the same
	// head, which still counts as a completed anchor call).
	if got := a.AnchoredTotal(); got != 2 {
		t.Fatalf("AnchoredTotal() = %d, want 2 (in-flight anchor + drained tick)", got)
	}
	waitForAnchorerTicks(t, a, 3, 3*time.Second)
}

func TestSetTSAAnchorIntervalNoSwallowedFiredTick(t *testing.T) {
	ts := newTestTSA(t)
	l, _, al := newTSAURLTestLedger(t, ts.srv.URL)
	if err := l.SetTSATrustCert(ts.cert); err != nil {
		t.Fatalf("SetTSATrustCert: %v", err)
	}
	postAnchorEntries(t, l, "tsa-drain", 3)
	defer func() { _ = al.Close() }()

	auth := &blockingTestAuthority{
		client:  &RFC3161Client{URL: ts.srv.URL},
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := StartTimestampAnchoring(ctx, l, 10*time.Millisecond, auth, nil)
	if a == nil {
		t.Fatal("StartTimestampAnchoring returned nil for positive interval")
	}
	defer a.Stop()

	select {
	case <-auth.started:
	case <-time.After(3 * time.Second):
		t.Fatal("first anchor never started")
	}
	// Move the head while the first anchor is in flight, and let
	// the ticker fire: the drained tick must anchor the new head,
	// not just bump a counter.
	postAnchorEntries(t, l, "tsa-drain2", 1)
	time.Sleep(50 * time.Millisecond)

	if _, err := a.SetInterval(500 * time.Millisecond); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	close(auth.release)

	waitForAnchorerTicks(t, a, 2, 3*time.Second)
	// The drained tick performed a real anchor of the moved head:
	// both the in-flight anchor and the drained anchor are
	// recorded, and the TSA saw a second request for the new head.
	waitFor(t, 3*time.Second, "drained tick anchor", func() bool { return a.AnchoredTotal() >= 2 })
	if ts.requests.Load() < 2 {
		t.Fatalf("TSA requests = %d, want >= 2 (in-flight anchor + drained anchor of the moved head)", ts.requests.Load())
	}
}

func TestSetTSAAnchorIntervalConcurrentRevisionAndTicksRaceFree(t *testing.T) {
	_, _, _, al, a, cancel := startRetuneTestAnchorer(t, 10*time.Millisecond)
	defer cancel()
	defer a.Stop()
	defer func() { _ = al.Close() }()

	// Concurrent revisions while ticks fire: -race must stay clean
	// and the live value must always be one of the revised values.
	intervals := []time.Duration{10 * time.Millisecond, 15 * time.Millisecond, 20 * time.Millisecond, 25 * time.Millisecond}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				d := intervals[(g+i)%len(intervals)]
				if _, err := a.SetInterval(d); err != nil {
					t.Errorf("SetInterval(%v): %v", d, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()

	got := a.Interval()
	ok := false
	for _, d := range intervals {
		if got == d {
			ok = true
		}
	}
	if !ok {
		t.Fatalf("Interval() = %v after concurrent revisions, want one of %v", got, intervals)
	}

	// Rapid revisions keep resetting the ticker countdown, so few
	// or no ticks may have fired during the storm — that is the
	// retune working as designed, not a stalled worker. Settle on
	// one short interval and prove the worker still ticks.
	if _, err := a.SetInterval(10 * time.Millisecond); err != nil {
		t.Fatalf("SetInterval after concurrent revisions: %v", err)
	}
	waitForAnchorerTicks(t, a, 1, 3*time.Second)
}

func TestSetTSAAnchorIntervalCtxActorAttribution(t *testing.T) {
	_, _, _, al, a, cancel := startRetuneTestAnchorer(t, time.Hour)
	defer cancel()

	ctx := ContextWithActor(context.Background(), "operator-anchoring")
	if _, err := a.SetIntervalCtx(ctx, 5*time.Minute); err != nil {
		t.Fatalf("SetIntervalCtx: %v", err)
	}
	events := retunedTSAAnchorIntervalEvents(t, a, al)
	if len(events) != 1 {
		t.Fatalf("retuned events = %d, want 1", len(events))
	}
	if events[0].Actor != "operator-anchoring" {
		t.Fatalf("audit Actor = %q, want operator-anchoring (LG-73)", events[0].Actor)
	}
}

func TestSetTSAAnchorIntervalAuditPreviousCurrentExact(t *testing.T) {
	_, _, _, al, a, cancel := startRetuneTestAnchorer(t, time.Hour)
	defer cancel()

	if _, err := a.SetInterval(5 * time.Minute); err != nil {
		t.Fatalf("SetInterval(5m): %v", err)
	}
	if _, err := a.SetInterval(30 * time.Second); err != nil {
		t.Fatalf("SetInterval(30s): %v", err)
	}
	events := retunedTSAAnchorIntervalEvents(t, a, al)
	if len(events) != 2 {
		t.Fatalf("retuned events = %d, want 2", len(events))
	}
	want := []struct{ prev, cur float64 }{
		{float64(time.Hour / time.Millisecond), float64(5 * time.Minute / time.Millisecond)},
		{float64(5 * time.Minute / time.Millisecond), float64(30 * time.Second / time.Millisecond)},
	}
	for i, ev := range events {
		if ev.Details["previous_anchor_interval_ms"] != want[i].prev ||
			ev.Details["current_anchor_interval_ms"] != want[i].cur {
			t.Fatalf("event %d details = %v, want previous %v current %v (ms)", i, ev.Details, want[i].prev, want[i].cur)
		}
		if len(ev.Details) != 2 {
			t.Fatalf("event %d details = %v, want exactly previous/current interval ms", i, ev.Details)
		}
		// The audit trail carries intervals only — never token or
		// secret material.
		raw, err := json.Marshal(ev.Details)
		if err != nil {
			t.Fatalf("marshal details: %v", err)
		}
		lowered := strings.ToLower(string(raw))
		for _, banned := range []string{"token", "secret", "der", "signature", "key"} {
			if strings.Contains(lowered, banned) {
				t.Fatalf("event %d details %s contain %q material", i, raw, banned)
			}
		}
	}
}

func TestSetTSAAnchorIntervalNoRestartContinuousTicks(t *testing.T) {
	_, _, _, al, a, cancel := startRetuneTestAnchorer(t, 50*time.Millisecond)
	defer cancel()
	defer a.Stop()
	defer func() { _ = al.Close() }()

	waitForAnchorerTicks(t, a, 2, 3*time.Second)
	if got := a.Ticks(); got != 2 {
		t.Fatalf("Ticks() before revision = %d, want 2", got)
	}

	// The revision moves the cadence only: the same goroutine keeps
	// ticking, the tick counter never resets, and every tick still
	// yields a completed anchor call (an idempotent success for
	// the unchanged head).
	if _, err := a.SetInterval(20 * time.Millisecond); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	waitForAnchorerTicks(t, a, 5, 3*time.Second)
	waitFor(t, 3*time.Second, "anchored ticks after revision", func() bool { return a.AnchoredTotal() >= 5 })
	if got, want := a.Ticks(), a.AnchoredTotal(); got < want {
		t.Fatalf("Ticks() = %d, AnchoredTotal() = %d, want ticks to keep pace with completed anchors", got, want)
	}
}

func TestSetTSAAnchorIntervalGetterFollowsLiveValue(t *testing.T) {
	_, _, _, al, a, cancel := startRetuneTestAnchorer(t, time.Hour)
	defer cancel()
	defer a.Stop()
	defer func() { _ = al.Close() }()

	for _, d := range []time.Duration{5 * time.Minute, 30 * time.Second, time.Hour, time.Minute} {
		if _, err := a.SetInterval(d); err != nil {
			t.Fatalf("SetInterval(%v): %v", d, err)
		}
		if got := a.Interval(); got != d {
			t.Fatalf("Interval() = %v after SetInterval(%v), want %v", got, d, d)
		}
	}
}

// liveURLTestAuthority resolves the TSA endpoint from the ledger on
// every call, mirroring main.go's tsaMetricsAuthority (LG-303): the
// worker never holds a frozen startup endpoint.
type liveURLTestAuthority struct{ l *Ledger }

func (a liveURLTestAuthority) Timestamp(ctx context.Context, digest [32]byte) (TimestampToken, error) {
	return (&RFC3161Client{URL: a.l.TSAURL()}).Timestamp(ctx, digest)
}

func TestSetTSAAnchorIntervalNextTickFollowsNewURL(t *testing.T) {
	ts := newTestTSA(t)
	url2 := secondTestTSAEndpoint(t, ts)
	l, dir, al := newTSAURLTestLedger(t, ts.srv.URL)
	if err := l.SetTSATrustCert(ts.cert); err != nil {
		t.Fatalf("SetTSATrustCert: %v", err)
	}
	postAnchorEntries(t, l, "tsa-urlguard", 3)
	defer func() { _ = al.Close() }()

	// Start on a short cadence and let one tick anchor against the
	// startup endpoint.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := StartTimestampAnchoring(ctx, l, 20*time.Millisecond, liveURLTestAuthority{l: l}, nil)
	if a == nil {
		t.Fatal("StartTimestampAnchoring returned nil for positive interval")
	}
	defer a.Stop()
	waitForAnchorerTicks(t, a, 1, 3*time.Second)
	// Wait for the anchor call itself to finish: Ticks() rises
	// when the tick starts, and the endpoint is read inside
	// AnchorTimestamp — re-sourcing mid-call could let the first
	// tick anchor against the new endpoint instead of the startup
	// one, which would make the guard vacuous.
	waitFor(t, 3*time.Second, "first anchor to complete", func() bool { return a.AnchoredTotal() >= 1 })
	if stored := readTSAAnchors(t, dir); len(stored) != 1 || stored[0].TSAURL != ts.srv.URL {
		t.Fatalf("stored anchors after first tick = %v, want one anchor against the startup endpoint", stored)
	}

	// Re-source the endpoint (LG-303), then retune the cadence
	// (LG-328): the next tick must dial the new TSA, not the
	// startup one — the two knobs are orthogonal.
	if err := l.SetTSAURL(url2); err != nil {
		t.Fatalf("SetTSAURL: %v", err)
	}
	if _, err := a.SetInterval(50 * time.Millisecond); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	waitForAnchorerTicks(t, a, 2, 3*time.Second)
	// Again, wait for the anchor call to land before reading the
	// stored anchors.
	waitFor(t, 3*time.Second, "second anchor to complete", func() bool { return a.AnchoredTotal() >= 2 })

	// The same head was re-anchored against the new endpoint: the
	// idempotency key includes the URL, so the new anchor is a
	// second stored anchor carrying url2.
	stored := readTSAAnchors(t, dir)
	if len(stored) != 2 {
		t.Fatalf("stored anchors = %d, want 2 (startup endpoint + re-sourced endpoint)", len(stored))
	}
	if stored[0].TSAURL != ts.srv.URL || stored[1].TSAURL != url2 {
		t.Fatalf("stored anchor URLs = %q, %q, want %q then %q",
			stored[0].TSAURL, stored[1].TSAURL, ts.srv.URL, url2)
	}
	if st := l.TimestampAnchorStatus(); st.Latest == nil || st.Latest.TSAURL != url2 {
		t.Fatalf("status Latest = %+v, want the new-endpoint anchor", st.Latest)
	}
	ts.checkNoHandleError(t)
}
