package ledger

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Audit-sink delivery-policy amendment tests (LG-306). They reuse the
// audit_sink_test.go harness: sinkCapture, sinkTestConfig (PollInterval
// 10ms, InitialBackoff 10ms, MaxBackoff 50ms), newSinkTestLedger,
// mustPostSink, and waitForSink.

// startSinkTestLedger builds a ledger with an audit log attached and a
// running sink, returning the ledger, the sink, and the audit dir.
func startSinkTestLedger(t *testing.T, cap *sinkCapture, cfg SinkConfig) (*Ledger, *AuditSink, string) {
	t.Helper()
	dir := t.TempDir()
	auditDir := filepath.Join(dir, "audit")
	al, err := NewAuditLog(auditDir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	t.Cleanup(func() { _ = al.Close() })
	l, _ := newSinkTestLedger(t, dir, WithAuditLog(al))
	srv := httptest.NewServer(http.HandlerFunc(cap.handler))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if cfg.URL == "" {
		cfg.URL = srv.URL
	}
	sink, err := StartAuditSink(ctx, l, cfg)
	if err != nil {
		t.Fatalf("StartAuditSink: %v", err)
	}
	t.Cleanup(sink.Stop)
	return l, sink, auditDir
}

// sinkRetunedEvents polls the audit log until at least one
// audit_sink_delivery_policy_retuned event is visible, then returns
// all of them.
func sinkRetunedEvents(t *testing.T, auditDir string) []AuditEvent {
	t.Helper()
	day := time.Now().UTC().Format("2006-01-02")
	var out []AuditEvent
	waitForSink(t, 5*time.Second, "retuned audit events", func() bool {
		events, _, err := ReadAuditLog(auditDir, day)
		if err != nil {
			return false
		}
		out = nil
		for _, ev := range events {
			if ev.Op == "audit_sink_delivery_policy_retuned" {
				out = append(out, ev)
			}
		}
		return len(out) > 0
	})
	return out
}

// sinkPolicyEqual compares two resolved policies by value (their
// fields are fresh pointers on every call, so == would compare
// addresses).
func sinkPolicyEqual(a, b SinkDeliveryPolicy) bool {
	return *a.MaxAttempts == *b.MaxAttempts &&
		*a.InitialBackoff == *b.InitialBackoff &&
		*a.MaxBackoff == *b.MaxBackoff &&
		*a.HTTPTimeout == *b.HTTPTimeout
}
func sinkRetunedCount(t *testing.T, auditDir string) int {
	t.Helper()
	day := time.Now().UTC().Format("2006-01-02")
	events, _, err := ReadAuditLog(auditDir, day)
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	n := 0
	for _, ev := range events {
		if ev.Op == "audit_sink_delivery_policy_retuned" {
			n++
		}
	}
	return n
}

func TestSetSinkDeliveryPolicyTightensMaxAttempts(t *testing.T) {
	cap := &sinkCapture{failAll: true}
	cfg := sinkTestConfig("")
	cfg.MaxAttempts = 3
	l, sink, _ := startSinkTestLedger(t, cap, cfg)

	// The pre-revision budget: 3 attempts against a permanent 500.
	mustPostSink(t, l, "amend-sink-max-1")
	waitForSink(t, 10*time.Second, "first dead letter", func() bool {
		_, _, dead := sink.Stats()
		return dead == 1
	})
	if _, failed, dead := sink.Stats(); failed != 3 || dead != 1 {
		t.Fatalf("Stats = (failed %d, dead %d), want (3, 1)", failed, dead)
	}

	// Tighten to 1 and confirm the returned policy is the old one.
	prev, err := sink.SetDeliveryPolicy(SinkDeliveryPolicy{MaxAttempts: ptrInt(1)})
	if err != nil {
		t.Fatalf("SetDeliveryPolicy: %v", err)
	}
	if prev.MaxAttempts == nil || *prev.MaxAttempts != 3 {
		t.Fatalf("returned previous MaxAttempts = %+v, want 3", prev.MaxAttempts)
	}
	if p := sink.DeliveryPolicy(); p.MaxAttempts == nil || *p.MaxAttempts != 1 {
		t.Fatalf("DeliveryPolicy().MaxAttempts = %+v, want 1", p.MaxAttempts)
	}
	mustPostSink(t, l, "amend-sink-max-2")
	waitForSink(t, 10*time.Second, "second dead letter", func() bool {
		_, _, dead := sink.Stats()
		return dead == 2
	})
	if _, failed, dead := sink.Stats(); failed != 4 || dead != 2 {
		t.Fatalf("Stats = (failed %d, dead %d), want (4, 2): the next delivery exhausted at exactly 1 attempt", failed, dead)
	}
}

func TestSetSinkDeliveryPolicyLengthensBackoff(t *testing.T) {
	cap := &sinkCapture{failAll: true}
	cfg := sinkTestConfig("")
	cfg.MaxAttempts = 3
	l, sink, _ := startSinkTestLedger(t, cap, cfg)

	// Lengthen the backoff past the max too, or the merged policy
	// would fail the InitialBackoff <= MaxBackoff gate.
	if _, err := sink.SetDeliveryPolicy(SinkDeliveryPolicy{
		InitialBackoff: ptrDuration(200 * time.Millisecond),
		MaxBackoff:     ptrDuration(500 * time.Millisecond),
	}); err != nil {
		t.Fatalf("SetDeliveryPolicy: %v", err)
	}
	mustPostSink(t, l, "amend-sink-backoff-1")
	waitForSink(t, 10*time.Second, "3 attempts", func() bool { return cap.count() == 3 })
	times := cap.attemptTimes()
	if len(times) != 3 {
		t.Fatalf("attempts = %d, want 3 (the untouched MaxAttempts budget)", len(times))
	}
	// The first retry waited out the lengthened backoff (~200ms, not
	// the original 10ms); the second doubled it (~400ms).
	if gap := times[1].Sub(times[0]); gap < 150*time.Millisecond {
		t.Fatalf("first retry gap = %s, want >= 150ms under the lengthened backoff", gap)
	}
	if gap := times[2].Sub(times[1]); gap < 300*time.Millisecond {
		t.Fatalf("second retry gap = %s, want >= 300ms under the doubled backoff", gap)
	}
}

func TestSetSinkDeliveryPolicyShortensHTTPTimeout(t *testing.T) {
	cap := &sinkCapture{delay: 300 * time.Millisecond}
	cfg := sinkTestConfig("")
	cfg.MaxAttempts = 1
	l, sink, _ := startSinkTestLedger(t, cap, cfg)

	// A 300ms-slow endpoint still delivers under the 10s default.
	mustPostSink(t, l, "amend-sink-timeout-1")
	waitForSink(t, 10*time.Second, "slow delivery to succeed", func() bool {
		sent, _, _ := sink.Stats()
		return sent == 1
	})

	// Shorten the timeout below the endpoint's latency: the next try
	// times out at once and dead-letters after its single attempt.
	if _, err := sink.SetDeliveryPolicy(SinkDeliveryPolicy{
		HTTPTimeout: ptrDuration(50 * time.Millisecond),
	}); err != nil {
		t.Fatalf("SetDeliveryPolicy: %v", err)
	}
	mustPostSink(t, l, "amend-sink-timeout-2")
	waitForSink(t, 10*time.Second, "timed-out dead letter", func() bool {
		_, _, dead := sink.Stats()
		return dead == 1
	})
	if sent, failed, dead := sink.Stats(); sent != 1 || failed != 1 || dead != 1 {
		t.Fatalf("Stats = (%d, %d, %d), want (1, 1, 1)", sent, failed, dead)
	}
}

func TestSetSinkDeliveryPolicyValidationRejectsAndKeepsLive(t *testing.T) {
	cap := &sinkCapture{}
	_, sink, _ := startSinkTestLedger(t, cap, sinkTestConfig(""))
	before := sink.DeliveryPolicy()

	cases := map[string]SinkDeliveryPolicy{
		"zero attempts":    {MaxAttempts: ptrInt(0)},
		"negative attempts": {MaxAttempts: ptrInt(-2)},
		"zero backoff":     {InitialBackoff: ptrDuration(0)},
		"negative backoff": {InitialBackoff: ptrDuration(-time.Second)},
		"zero max backoff": {MaxBackoff: ptrDuration(0)},
		"zero timeout":     {HTTPTimeout: ptrDuration(0)},
		"backoff over max": {
			InitialBackoff: ptrDuration(2 * time.Minute),
			MaxBackoff:     ptrDuration(time.Minute),
		},
	}
	for name, p := range cases {
		if _, err := sink.SetDeliveryPolicy(p); !errors.Is(err, ErrInvalidSinkDeliveryPolicy) {
			t.Errorf("%s: err = %v, want ErrInvalidSinkDeliveryPolicy", name, err)
		}
		if got := sink.DeliveryPolicy(); !sinkPolicyEqual(got, before) {
			t.Errorf("%s: policy moved to %+v, want unchanged %+v", name, got, before)
		}
	}
}

func TestSetSinkDeliveryPolicyDefaultsAndGetter(t *testing.T) {
	cap := &sinkCapture{}
	_, sink, _ := startSinkTestLedger(t, cap, sinkTestConfig(""))
	p := sink.DeliveryPolicy()
	// sinkTestConfig leaves MaxAttempts and HTTPTimeout unset, so
	// they resolve to the documented defaults; its backoffs are the
	// test's own 10ms/50ms, not the defaults.
	if p.MaxAttempts == nil || *p.MaxAttempts != DefaultSinkMaxAttempts {
		t.Errorf("MaxAttempts = %+v, want default %d", p.MaxAttempts, DefaultSinkMaxAttempts)
	}
	if p.InitialBackoff == nil || *p.InitialBackoff != 10*time.Millisecond {
		t.Errorf("InitialBackoff = %+v, want test config 10ms", p.InitialBackoff)
	}
	if p.MaxBackoff == nil || *p.MaxBackoff != 50*time.Millisecond {
		t.Errorf("MaxBackoff = %+v, want test config 50ms", p.MaxBackoff)
	}
	if p.HTTPTimeout == nil || *p.HTTPTimeout != DefaultSinkHTTPTimeout {
		t.Errorf("HTTPTimeout = %+v, want default %v", p.HTTPTimeout, DefaultSinkHTTPTimeout)
	}
}

func TestSetSinkDeliveryPolicyNoOpWritesNoAudit(t *testing.T) {
	cap := &sinkCapture{}
	_, sink, auditDir := startSinkTestLedger(t, cap, sinkTestConfig(""))

	// One real revision, as a positive control: it must audit.
	if _, err := sink.SetDeliveryPolicy(SinkDeliveryPolicy{MaxAttempts: ptrInt(5)}); err != nil {
		t.Fatalf("SetDeliveryPolicy: %v", err)
	}
	events := sinkRetunedEvents(t, auditDir)
	if len(events) != 1 {
		t.Fatalf("retuned events = %d, want 1 after the real revision", len(events))
	}

	// All-nil and same-value revisions are no-ops: no new audit.
	if _, err := sink.SetDeliveryPolicy(SinkDeliveryPolicy{}); err != nil {
		t.Fatalf("all-nil SetDeliveryPolicy: %v", err)
	}
	if _, err := sink.SetDeliveryPolicy(SinkDeliveryPolicy{MaxAttempts: ptrInt(5)}); err != nil {
		t.Fatalf("same-value SetDeliveryPolicy: %v", err)
	}
	if _, err := sink.SetDeliveryPolicyCtx(context.Background(), SinkDeliveryPolicy{}); err != nil {
		t.Fatalf("all-nil SetDeliveryPolicyCtx: %v", err)
	}
	time.Sleep(300 * time.Millisecond) // let any stray event flush
	if n := sinkRetunedCount(t, auditDir); n != 1 {
		t.Fatalf("retuned events = %d, want still 1 after the no-op revisions", n)
	}
}

func TestSetSinkDeliveryPolicyAuditEventFields(t *testing.T) {
	cap := &sinkCapture{}
	_, sink, auditDir := startSinkTestLedger(t, cap, sinkTestConfig(""))

	prev, err := sink.SetDeliveryPolicy(SinkDeliveryPolicy{
		MaxAttempts: ptrInt(4),
		HTTPTimeout: ptrDuration(2 * time.Second),
	})
	if err != nil {
		t.Fatalf("SetDeliveryPolicy: %v", err)
	}
	if prev.MaxAttempts == nil || *prev.MaxAttempts != DefaultSinkMaxAttempts {
		t.Fatalf("returned previous MaxAttempts = %+v, want %d", prev.MaxAttempts, DefaultSinkMaxAttempts)
	}
	events := sinkRetunedEvents(t, auditDir)
	if len(events) != 1 {
		t.Fatalf("retuned events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Actor != "SetDeliveryPolicy" {
		t.Errorf("Actor = %q, want SetDeliveryPolicy", ev.Actor)
	}
	if ev.VersionBefore != ev.VersionAfter {
		t.Errorf("version moved %d -> %d, want no bump", ev.VersionBefore, ev.VersionAfter)
	}
	if got := ev.Details["previous_max_attempts"]; got != float64(DefaultSinkMaxAttempts) {
		t.Errorf("previous_max_attempts = %v, want %d", got, DefaultSinkMaxAttempts)
	}
	if got := ev.Details["current_max_attempts"]; got != float64(4) {
		t.Errorf("current_max_attempts = %v, want 4", got)
	}
	if got := ev.Details["previous_http_timeout_ms"]; got != float64(DefaultSinkHTTPTimeout.Milliseconds()) {
		t.Errorf("previous_http_timeout_ms = %v, want %d", got, DefaultSinkHTTPTimeout.Milliseconds())
	}
	if got := ev.Details["current_http_timeout_ms"]; got != float64(2000) {
		t.Errorf("current_http_timeout_ms = %v, want 2000", got)
	}
	// Only the changed fields are listed: the untouched backoffs are
	// absent from the details.
	if _, ok := ev.Details["previous_initial_backoff_ms"]; ok {
		t.Errorf("unchanged initial_backoff must not appear in the details: %v", ev.Details)
	}
	if _, ok := ev.Details["previous_max_backoff_ms"]; ok {
		t.Errorf("unchanged max_backoff must not appear in the details: %v", ev.Details)
	}
}

func TestSetSinkDeliveryPolicyInFlightKeepsOldPolicy(t *testing.T) {
	cap := &sinkCapture{failFirst: 5}
	cfg := sinkTestConfig("")
	// Default MaxAttempts (10) covers the 5 failures plus the success.
	l, sink, _ := startSinkTestLedger(t, cap, cfg)

	mustPostSink(t, l, "amend-sink-inflight-1")
	// Wait until the first attempt has landed, then revise mid-flight:
	// the delivery must finish under the policy it started with.
	waitForSink(t, 10*time.Second, "first attempt to land", func() bool { return cap.count() >= 1 })
	if _, err := sink.SetDeliveryPolicy(SinkDeliveryPolicy{MaxAttempts: ptrInt(1)}); err != nil {
		t.Fatalf("SetDeliveryPolicy mid-flight: %v", err)
	}
	// The in-flight delivery keeps retrying past the new budget of 1
	// and succeeds on its 6th attempt.
	waitForSink(t, 10*time.Second, "in-flight delivery to succeed", func() bool {
		sent, _, _ := sink.Stats()
		return sent == 1
	})
	if sent, failed, dead := sink.Stats(); sent != 1 || failed != 5 || dead != 0 {
		t.Fatalf("Stats = (%d, %d, %d), want (1, 5, 0): the in-flight delivery kept its old budget", sent, failed, dead)
	}

	// The next delivery reads the new budget: one attempt, then done.
	cap.setFailAll(true)
	mustPostSink(t, l, "amend-sink-inflight-2")
	waitForSink(t, 10*time.Second, "next delivery to dead-letter", func() bool {
		_, _, dead := sink.Stats()
		return dead == 1
	})
	if sent, failed, dead := sink.Stats(); sent != 1 || failed != 6 || dead != 1 {
		t.Fatalf("Stats = (%d, %d, %d), want (1, 6, 1): the next delivery exhausted at exactly 1 attempt", sent, failed, dead)
	}
}

func TestSetSinkDeliveryPolicyPreservesCursorStatsOrder(t *testing.T) {
	cap := &sinkCapture{}
	cfg := sinkTestConfig("")
	l, sink, auditDir := startSinkTestLedger(t, cap, cfg)

	mustPostSink(t, l, "amend-sink-order-1")
	mustPostSink(t, l, "amend-sink-order-2")
	// Revise while the first deliveries are in flight or queued: the
	// cursor, the order, and the counters must all survive it.
	if _, err := sink.SetDeliveryPolicy(SinkDeliveryPolicy{MaxAttempts: ptrInt(2)}); err != nil {
		t.Fatalf("SetDeliveryPolicy: %v", err)
	}
	mustPostSink(t, l, "amend-sink-order-3")
	mustPostSink(t, l, "amend-sink-order-4")
	waitForSink(t, 10*time.Second, "4 deliveries", func() bool { return cap.count() == 4 })
	_, _, _, seqs := cap.snapshot()
	if len(seqs) != 4 || seqs[0] != "1" || seqs[1] != "2" || seqs[2] != "3" || seqs[3] != "4" {
		t.Fatalf("seqs = %v, want [1 2 3 4]", seqs)
	}
	cursorPath := filepath.Join(filepath.Dir(auditDir), "outbox", sinkCursorFilename)
	waitForSink(t, 5*time.Second, "cursor at 4", func() bool {
		return readCursorFile(t, cursorPath) == "4"
	})
	if sent, failed, dead := sink.Stats(); sent != 4 || failed != 0 || dead != 0 {
		t.Fatalf("Stats = (%d, %d, %d), want (4, 0, 0)", sent, failed, dead)
	}
	if p := sink.DeliveryPolicy(); p.MaxAttempts == nil || *p.MaxAttempts != 2 {
		t.Fatalf("DeliveryPolicy().MaxAttempts = %+v, want 2", p.MaxAttempts)
	}
}

func TestSetSinkDeliveryPolicyCtxActorAttribution(t *testing.T) {
	cap := &sinkCapture{}
	_, sink, auditDir := startSinkTestLedger(t, cap, sinkTestConfig(""))

	ctx := ContextWithActor(context.Background(), "ops-alan")
	if _, err := sink.SetDeliveryPolicyCtx(ctx, SinkDeliveryPolicy{
		MaxAttempts: ptrInt(3),
		HTTPTimeout: ptrDuration(5 * time.Second),
	}); err != nil {
		t.Fatalf("SetDeliveryPolicyCtx: %v", err)
	}
	// A plain call keeps the entrypoint name as the actor.
	if _, err := sink.SetDeliveryPolicy(SinkDeliveryPolicy{MaxAttempts: ptrInt(4)}); err != nil {
		t.Fatalf("SetDeliveryPolicy: %v", err)
	}
	var events []AuditEvent
	waitForSink(t, 5*time.Second, "2 retuned events", func() bool {
		day := time.Now().UTC().Format("2006-01-02")
		evs, _, err := ReadAuditLog(auditDir, day)
		if err != nil {
			return false
		}
		events = nil
		for _, ev := range evs {
			if ev.Op == "audit_sink_delivery_policy_retuned" {
				events = append(events, ev)
			}
		}
		return len(events) == 2
	})
	first := events[0]
	if first.Actor != "ops-alan" {
		t.Fatalf("first event Actor = %q, want ops-alan", first.Actor)
	}
	if got := first.Details["previous_max_attempts"]; got != float64(DefaultSinkMaxAttempts) {
		t.Fatalf("previous_max_attempts = %v, want %d", got, DefaultSinkMaxAttempts)
	}
	if got := first.Details["current_max_attempts"]; got != float64(3) {
		t.Fatalf("current_max_attempts = %v, want 3", got)
	}
	if got := first.Details["previous_http_timeout_ms"]; got != float64(DefaultSinkHTTPTimeout.Milliseconds()) {
		t.Fatalf("previous_http_timeout_ms = %v, want %d", got, DefaultSinkHTTPTimeout.Milliseconds())
	}
	if got := first.Details["current_http_timeout_ms"]; got != float64(5000) {
		t.Fatalf("current_http_timeout_ms = %v, want 5000", got)
	}
	if _, ok := first.Details["previous_initial_backoff_ms"]; ok {
		t.Fatalf("unchanged initial_backoff must not appear in the details: %v", first.Details)
	}
	if events[1].Actor != "SetDeliveryPolicy" {
		t.Fatalf("second event Actor = %q, want SetDeliveryPolicy", events[1].Actor)
	}
}

func TestSetSinkDeliveryPolicyConcurrentRevisionNoRace(t *testing.T) {
	cap := &sinkCapture{}
	cfg := sinkTestConfig("")
	l, sink, _ := startSinkTestLedger(t, cap, cfg)

	var wg sync.WaitGroup
	// Concurrent posts and concurrent revisions: -race must stay
	// silent, and every event must still deliver exactly once.
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				mustPostSink(t, l, "amend-sink-race")
			}
		}(g)
	}
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				_, _ = sink.SetDeliveryPolicy(SinkDeliveryPolicy{
					MaxAttempts:    ptrInt(1 + (i % 3)),
					InitialBackoff: ptrDuration(10 * time.Millisecond),
					MaxBackoff:     ptrDuration(50 * time.Millisecond),
					HTTPTimeout:    ptrDuration(10 * time.Second),
				})
				_ = sink.DeliveryPolicy()
			}
		}(g)
	}
	wg.Wait()
	waitForSink(t, 10*time.Second, "40 deliveries", func() bool {
		sent, _, _ := sink.Stats()
		return sent == 40
	})
	if sent, failed, dead := sink.Stats(); sent != 40 || failed != 0 || dead != 0 {
		t.Fatalf("Stats = (%d, %d, %d), want (40, 0, 0)", sent, failed, dead)
	}
}
