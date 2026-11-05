package ledger

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Report-sweeper interval runtime revision tests (LG-316). They reuse
// the ledger_report_schedule_test.go harness (validReportSchedule,
// mustCreateReportSchedule, postSomeEntries), the audit_sink_test.go
// waiter (waitForSink), and mustAuditLog from ledger_audit_test.go.

// retunedReportSweepEvents stops the sweeper, closes the audit log
// (which drains the async writer), and returns every
// report_sweep_interval_retuned event recorded in it. The ledger must
// not be used after this call.
func retunedReportSweepEvents(t *testing.T, sw *ReportSweeper, al *AuditLog) []AuditEvent {
	t.Helper()
	sw.Stop()
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
		if ev.Op == "report_sweep_interval_retuned" {
			out = append(out, ev)
		}
	}
	return out
}

// startRetuneTestSweeper starts a sweeper on a ledger with one due
// reconcile schedule (dir sink, 50ms subscription cadence) and an
// audit log attached.
func startRetuneTestSweeper(t *testing.T, interval time.Duration) (*Ledger, *ReportSweeper, *AuditLog, context.CancelFunc) {
	t.Helper()
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	postSomeEntries(t, l)
	dir := t.TempDir()
	s := validReportSchedule(dir)
	s.ID = "retune-swept"
	s.Interval = 50 * time.Millisecond
	s.NextRunAt = time.Now().Add(-time.Second)
	mustCreateReportSchedule(t, l, s)

	ctx, cancel := context.WithCancel(context.Background())
	sw := StartReportSweeper(ctx, l, interval, nil)
	if sw == nil {
		cancel()
		t.Fatalf("sweeper not started")
	}
	return l, sw, al, cancel
}

// waitForTicks waits until the sweeper has completed at least n ticks.
func waitForTicks(t *testing.T, sw *ReportSweeper, n uint64, timeout time.Duration) {
	t.Helper()
	waitForSink(t, timeout, "ticks", func() bool { return sw.Ticks() >= n })
}

func TestSetReportSweepIntervalInvalidRejectedLiveUnchanged(t *testing.T) {
	_, sw, al, cancel := startRetuneTestSweeper(t, 500*time.Millisecond)
	defer cancel()

	for _, d := range []time.Duration{0, -time.Second, -time.Millisecond} {
		if prev, err := sw.SetInterval(d); !errors.Is(err, ErrInvalidReportSweepInterval) {
			t.Errorf("SetInterval(%v): err = %v, want ErrInvalidReportSweepInterval", d, err)
		} else if prev != 500*time.Millisecond {
			t.Errorf("SetInterval(%v): returned %v, want the live 500ms", d, prev)
		}
		if got := sw.Interval(); got != 500*time.Millisecond {
			t.Errorf("SetInterval(%v): live interval moved to %v, want unchanged 500ms", d, got)
		}
	}
	if events := retunedReportSweepEvents(t, sw, al); len(events) != 0 {
		t.Fatalf("rejected revisions wrote %d audit events, want 0", len(events))
	}
}

func TestSetReportSweepIntervalReturnsPreviousValue(t *testing.T) {
	l := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sw := StartReportSweeper(ctx, l, 500*time.Millisecond, nil)
	if sw == nil {
		t.Fatalf("sweeper not started")
	}
	defer sw.Stop()

	if got := sw.Interval(); got != 500*time.Millisecond {
		t.Fatalf("Interval() = %v, want construction-time 500ms", got)
	}
	prev, err := sw.SetInterval(10 * time.Millisecond)
	if err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	if prev != 500*time.Millisecond {
		t.Fatalf("returned previous interval = %v, want 500ms", prev)
	}
	if got := sw.Interval(); got != 10*time.Millisecond {
		t.Fatalf("Interval() = %v, want 10ms after the revision", got)
	}
	// The getter follows a second revision the same way.
	prev, err = sw.SetInterval(250 * time.Millisecond)
	if err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	if prev != 10*time.Millisecond {
		t.Fatalf("returned previous interval = %v, want 10ms", prev)
	}
	if got := sw.Interval(); got != 250*time.Millisecond {
		t.Fatalf("Interval() = %v, want 250ms after the second revision", got)
	}
}

func TestSetReportSweepIntervalSameValueNoOpNoAudit(t *testing.T) {
	_, sw, al, cancel := startRetuneTestSweeper(t, 500*time.Millisecond)
	defer cancel()

	// One real revision, as a positive control: it must audit.
	if _, err := sw.SetInterval(10 * time.Millisecond); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	// A same-value revision is a no-op: same return, no new audit.
	prev, err := sw.SetInterval(10 * time.Millisecond)
	if err != nil {
		t.Fatalf("same-value SetInterval: %v", err)
	}
	if prev != 10*time.Millisecond {
		t.Fatalf("same-value revision returned %v, want the live 10ms", prev)
	}
	if events := retunedReportSweepEvents(t, sw, al); len(events) != 1 {
		t.Fatalf("retuned events = %d, want 1 after the real revision plus the no-op", len(events))
	}
}

func TestSetReportSweepIntervalNextTickUsesNewInterval(t *testing.T) {
	_, sw, al, cancel := startRetuneTestSweeper(t, 500*time.Millisecond)
	defer cancel()
	defer sw.Stop()
	defer func() { _ = al.Close() }()

	// The first tick lands on the old 500ms cadence.
	waitForTicks(t, sw, 1, 3*time.Second)
	if got := sw.Ticks(); got != 1 {
		t.Fatalf("ticks after first wait = %d, want exactly 1 on the 500ms cadence", got)
	}

	// Tighten to 20ms: the next ticks must accelerate — three more
	// ticks land in ~60ms, not ~1.5s on the old rhythm.
	if _, err := sw.SetInterval(20 * time.Millisecond); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	if got := sw.Interval(); got != 20*time.Millisecond {
		t.Fatalf("Interval() = %v, want 20ms after the revision", got)
	}
	start := time.Now()
	waitForTicks(t, sw, 4, 3*time.Second)
	if took := time.Since(start); took > time.Second {
		t.Fatalf("3 ticks after tightening took %s, want < 1s on the 20ms rhythm (would take ~1.5s on the old 500ms rhythm)", took)
	}
}

func TestSetReportSweepIntervalInFlightSweepNotInterrupted(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	postSomeEntries(t, l)

	// A URL sink whose handler blocks until released: the sweep
	// is observably in flight while the POST hangs.
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	s := validReportSchedule(t.TempDir())
	s.ID = "retune-blocked"
	s.Interval = 24 * time.Hour
	s.NextRunAt = time.Now().Add(-time.Second)
	s.Sink = ReportSink{URL: srv.URL, Secret: "test-secret"}
	mustCreateReportSchedule(t, l, s)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sw := StartReportSweeper(ctx, l, 30*time.Millisecond, nil)
	if sw == nil {
		t.Fatalf("sweeper not started")
	}
	defer sw.Stop()
	defer func() { _ = al.Close() }()

	// Wait until the sweep is inside the blocking POST.
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatalf("sweep never entered the blocking webhook")
	}
	// Revise mid-sweep: the in-flight sweep must not be cut short
	// or dropped, and the new interval governs from the next tick.
	if _, err := sw.SetInterval(500 * time.Millisecond); err != nil {
		t.Fatalf("SetInterval mid-sweep: %v", err)
	}
	close(release)

	// The in-flight sweep completes on the old cadence: one tick,
	// one generated run.
	waitForTicks(t, sw, 1, 3*time.Second)
	if got := sw.GeneratedTotal(); got != 1 {
		t.Fatalf("GeneratedTotal = %d, want 1: the in-flight sweep must complete", got)
	}
	got, err := l.GetReportSchedule("retune-blocked")
	if err != nil {
		t.Fatalf("GetReportSchedule: %v", err)
	}
	if got.RunSeq != 1 {
		t.Fatalf("RunSeq = %d, want 1 after the in-flight sweep", got.RunSeq)
	}

	// The next tick follows the loosened 500ms cadence: nothing
	// within 200ms of the completed sweep, then a tick arrives.
	time.Sleep(200 * time.Millisecond)
	if n := sw.Ticks(); n != 1 {
		t.Fatalf("ticks = %d within 200ms of the completed sweep, want still 1 on the loosened 500ms rhythm", n)
	}
	waitForTicks(t, sw, 2, 3*time.Second)
}

func TestSetReportSweepIntervalConcurrentRevisionAndSweepRaceFree(t *testing.T) {
	_, sw, al, cancel := startRetuneTestSweeper(t, 10*time.Millisecond)
	defer cancel()
	defer sw.Stop()
	defer func() { _ = al.Close() }()

	var wg sync.WaitGroup
	// Concurrent revisions from several goroutines while the
	// sweeper ticks underneath: -race must stay silent, and the
	// sweeper must keep ticking on some positive interval.
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				d := time.Duration(10+g*10+i%5) * time.Millisecond
				_, _ = sw.SetInterval(d)
				_ = sw.Interval()
			}
		}(g)
	}
	wg.Wait()
	if got := sw.Interval(); got <= 0 {
		t.Fatalf("Interval() = %v after concurrent revisions, want a positive live value", got)
	}
	waitForTicks(t, sw, 1, 3*time.Second)
	if n := sw.Ticks(); n == 0 {
		t.Fatalf("ticks = 0 after concurrent revisions, want the sweeper still ticking")
	}
}

func TestSetReportSweepIntervalCtxActorAttribution(t *testing.T) {
	_, sw, al, cancel := startRetuneTestSweeper(t, 500*time.Millisecond)
	defer cancel()

	ctx := ContextWithActor(context.Background(), "ops-alan")
	if _, err := sw.SetIntervalCtx(ctx, 10*time.Millisecond); err != nil {
		t.Fatalf("SetIntervalCtx: %v", err)
	}
	// A plain call keeps the entrypoint name as the actor.
	if _, err := sw.SetInterval(250 * time.Millisecond); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	events := retunedReportSweepEvents(t, sw, al)
	if len(events) != 2 {
		t.Fatalf("retuned events = %d, want 2", len(events))
	}
	if events[0].Actor != "ops-alan" {
		t.Fatalf("first event Actor = %q, want ops-alan", events[0].Actor)
	}
	if got := events[0].Details["previous_sweep_interval_ms"]; got != float64(500) {
		t.Fatalf("first previous_sweep_interval_ms = %v, want 500", got)
	}
	if got := events[0].Details["current_sweep_interval_ms"]; got != float64(10) {
		t.Fatalf("first current_sweep_interval_ms = %v, want 10", got)
	}
	if events[1].Actor != "SetInterval" {
		t.Fatalf("second event Actor = %q, want SetInterval", events[1].Actor)
	}
	if got := events[1].Details["previous_sweep_interval_ms"]; got != float64(10) {
		t.Fatalf("second previous_sweep_interval_ms = %v, want 10", got)
	}
	if got := events[1].Details["current_sweep_interval_ms"]; got != float64(250) {
		t.Fatalf("second current_sweep_interval_ms = %v, want 250", got)
	}
}

func TestSetReportSweepIntervalAuditPreviousCurrentExact(t *testing.T) {
	_, sw, al, cancel := startRetuneTestSweeper(t, 500*time.Millisecond)
	defer cancel()

	prev, err := sw.SetInterval(10 * time.Millisecond)
	if err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	if prev != 500*time.Millisecond {
		t.Fatalf("returned previous interval = %v, want 500ms", prev)
	}
	events := retunedReportSweepEvents(t, sw, al)
	if len(events) != 1 {
		t.Fatalf("retuned events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Actor != "SetInterval" {
		t.Errorf("Actor = %q, want SetInterval", ev.Actor)
	}
	if ev.VersionBefore != ev.VersionAfter {
		t.Errorf("version moved %d -> %d, want no bump", ev.VersionBefore, ev.VersionAfter)
	}
	if got := ev.Details["previous_sweep_interval_ms"]; got != float64(500) {
		t.Errorf("previous_sweep_interval_ms = %v, want 500", got)
	}
	if got := ev.Details["current_sweep_interval_ms"]; got != float64(10) {
		t.Errorf("current_sweep_interval_ms = %v, want 10", got)
	}
	// Only the two numeric ms fields ride in the event: no other keys.
	if len(ev.Details) != 2 {
		t.Errorf("details = %v, want exactly previous/current sweep interval ms", ev.Details)
	}
}

func TestSetReportSweepIntervalNoRestartContinuousTicks(t *testing.T) {
	_, sw, al, cancel := startRetuneTestSweeper(t, 20*time.Millisecond)
	defer cancel()
	defer sw.Stop()
	defer func() { _ = al.Close() }()

	// Let the sweeper tick on the original cadence first.
	waitForTicks(t, sw, 2, 3*time.Second)
	ticksBefore := sw.Ticks()
	generatedBefore := sw.GeneratedTotal()
	if generatedBefore == 0 {
		t.Fatalf("GeneratedTotal = 0 after %d ticks, want the due schedule firing", ticksBefore)
	}

	// Revise the cadence: the same goroutine keeps running — the
	// counters never reset and the ticks keep accumulating on the
	// new interval instead of restarting from zero.
	if _, err := sw.SetInterval(30 * time.Millisecond); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	waitForTicks(t, sw, ticksBefore+3, 3*time.Second)
	if got := sw.Ticks(); got < ticksBefore+3 {
		t.Fatalf("Ticks() = %d, want >= %d: ticks must continue, not restart", got, ticksBefore+3)
	}
	if got := sw.GeneratedTotal(); got < generatedBefore {
		t.Fatalf("GeneratedTotal() = %d, want >= %d: counters must keep accumulating across the revision", got, generatedBefore)
	}
	if got := sw.FailedTotal(); got != 0 {
		t.Fatalf("FailedTotal() = %d, want 0", got)
	}
}

func TestSetReportSweepIntervalCountersKeepAccumulating(t *testing.T) {
	_, sw, al, cancel := startRetuneTestSweeper(t, 20*time.Millisecond)
	defer cancel()
	defer sw.Stop()
	defer func() { _ = al.Close() }()

	waitForTicks(t, sw, 2, 3*time.Second)
	ticksBefore, generatedBefore := sw.Ticks(), sw.GeneratedTotal()
	if _, err := sw.SetInterval(25 * time.Millisecond); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	// A revision is not a reset: the counters observed before it
	// are still there, and they grow from there.
	if got := sw.Ticks(); got < ticksBefore {
		t.Fatalf("Ticks() = %d after revision, want >= %d (pre-revision count)", got, ticksBefore)
	}
	if got := sw.GeneratedTotal(); got < generatedBefore {
		t.Fatalf("GeneratedTotal() = %d after revision, want >= %d (pre-revision count)", got, generatedBefore)
	}
	waitForTicks(t, sw, ticksBefore+2, 3*time.Second)
	if got := sw.GeneratedTotal(); got <= generatedBefore {
		t.Fatalf("GeneratedTotal() = %d, want > %d after further ticks", got, generatedBefore)
	}
}
