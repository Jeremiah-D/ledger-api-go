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

// Audit-sink idle-poll-interval amendment tests (LG-314). They reuse
// the audit_sink_test.go harness: sinkCapture (PollInterval 10ms in
// sinkTestConfig, overridden to 500ms where the tests need a slow
// rhythm), startSinkTestLedger in
// audit_sink_delivery_amend_test.go, mustPostSink, and waitForSink.

// sinkPollIntervalRetunedEvents polls the audit log until at least
// one sink_poll_interval_retuned event is visible, then returns all
// of them.
func sinkPollIntervalRetunedEvents(t *testing.T, auditDir string) []AuditEvent {
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
			if ev.Op == "sink_poll_interval_retuned" {
				out = append(out, ev)
			}
		}
		return len(out) > 0
	})
	return out
}

func sinkPollIntervalRetunedCount(t *testing.T, auditDir string) int {
	t.Helper()
	day := time.Now().UTC().Format("2006-01-02")
	events, _, err := ReadAuditLog(auditDir, day)
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	n := 0
	for _, ev := range events {
		if ev.Op == "sink_poll_interval_retuned" {
			n++
		}
	}
	return n
}

func TestSetSinkPollIntervalTightensIdlePoll(t *testing.T) {
	cap := &sinkCapture{}
	cfg := sinkTestConfig("")
	cfg.PollInterval = 500 * time.Millisecond
	// Revise mid-delivery, while the sink is still blocked in
	// client.Do: the sink's next idle sleep is then built from the
	// tightened value. (Revising from the test goroutine after the
	// delivery lands would race the sink entering its next 500ms
	// sleep.)
	var sink *AuditSink
	var once sync.Once
	cap.onAttempt = func() {
		once.Do(func() {
			if _, err := sink.SetSinkPollInterval(10 * time.Millisecond); err != nil {
				t.Errorf("SetSinkPollInterval mid-delivery: %v", err)
			}
		})
	}
	l, s, _ := startSinkTestLedger(t, cap, cfg)
	sink = s

	mustPostSink(t, l, "amend-sink-poll-tight-1")
	waitForSink(t, 5*time.Second, "first delivery", func() bool { return cap.count() == 1 })
	if got := sink.PollInterval(); got != 10*time.Millisecond {
		t.Fatalf("PollInterval() = %v, want 10ms after the mid-delivery revision", got)
	}

	// The next event rides the tightened rhythm: it must arrive well
	// inside the old 500ms cadence.
	posted := time.Now()
	mustPostSink(t, l, "amend-sink-poll-tight-2")
	waitForSink(t, 5*time.Second, "second delivery", func() bool { return cap.count() == 2 })
	times := cap.attemptTimes()
	if len(times) != 2 {
		t.Fatalf("attempts = %d, want 2", len(times))
	}
	if gap := times[1].Sub(posted); gap > 200*time.Millisecond {
		t.Fatalf("second delivery took %s after posting, want < 200ms on the tightened 10ms rhythm (would take ~500ms on the old rhythm)", gap)
	}
}

func TestSetSinkPollIntervalLoosensIdlePoll(t *testing.T) {
	cap := &sinkCapture{}
	cfg := sinkTestConfig("")
	cfg.PollInterval = 10 * time.Millisecond
	l, sink, _ := startSinkTestLedger(t, cap, cfg)

	mustPostSink(t, l, "amend-sink-poll-loose-1")
	waitForSink(t, 5*time.Second, "first delivery", func() bool { return cap.count() == 1 })

	prev, err := sink.SetSinkPollInterval(500 * time.Millisecond)
	if err != nil {
		t.Fatalf("SetSinkPollInterval: %v", err)
	}
	if prev != 10*time.Millisecond {
		t.Fatalf("returned previous interval = %v, want 10ms", prev)
	}
	// Let the sink wake from its last 10ms sleep and enter a 500ms
	// one under the new value before posting: otherwise the event
	// could land on the tail of the old rhythm.
	time.Sleep(50 * time.Millisecond)

	posted := time.Now()
	mustPostSink(t, l, "amend-sink-poll-loose-2")
	// The loosened rhythm must hold the event back: nothing within
	// 200ms of posting.
	time.Sleep(200 * time.Millisecond)
	if n := cap.count(); n != 1 {
		t.Fatalf("delivered %d events within 200ms of posting, want still 1 under the loosened 500ms rhythm", n)
	}
	waitForSink(t, 5*time.Second, "second delivery", func() bool { return cap.count() == 2 })
	times := cap.attemptTimes()
	if gap := times[1].Sub(posted); gap < 200*time.Millisecond {
		t.Fatalf("second delivery took only %s after posting, want >= 200ms under the loosened 500ms rhythm", gap)
	}
}

func TestSetSinkPollIntervalValidationRejectsAndKeepsLive(t *testing.T) {
	cap := &sinkCapture{}
	cfg := sinkTestConfig("")
	cfg.PollInterval = 500 * time.Millisecond
	_, sink, _ := startSinkTestLedger(t, cap, cfg)

	for _, d := range []time.Duration{0, -time.Second, -time.Millisecond} {
		if prev, err := sink.SetSinkPollInterval(d); !errors.Is(err, ErrInvalidSinkPollInterval) {
			t.Errorf("SetSinkPollInterval(%v): err = %v, want ErrInvalidSinkPollInterval", d, err)
		} else if prev != 500*time.Millisecond {
			t.Errorf("SetSinkPollInterval(%v): returned %v, want the live 500ms", d, prev)
		}
		if got := sink.PollInterval(); got != 500*time.Millisecond {
			t.Errorf("SetSinkPollInterval(%v): live interval moved to %v, want unchanged 500ms", d, got)
		}
	}
}

func TestSetSinkPollIntervalReturnsPreviousValue(t *testing.T) {
	cap := &sinkCapture{}
	cfg := sinkTestConfig("")
	cfg.PollInterval = 500 * time.Millisecond
	_, sink, _ := startSinkTestLedger(t, cap, cfg)

	if got := sink.PollInterval(); got != 500*time.Millisecond {
		t.Fatalf("PollInterval() = %v, want construction-time 500ms", got)
	}
	prev, err := sink.SetSinkPollInterval(10 * time.Millisecond)
	if err != nil {
		t.Fatalf("SetSinkPollInterval: %v", err)
	}
	if prev != 500*time.Millisecond {
		t.Fatalf("returned previous interval = %v, want 500ms", prev)
	}
	if got := sink.PollInterval(); got != 10*time.Millisecond {
		t.Fatalf("PollInterval() = %v, want 10ms after the revision", got)
	}
	// The getter follows a second revision the same way.
	prev, err = sink.SetSinkPollInterval(250 * time.Millisecond)
	if err != nil {
		t.Fatalf("SetSinkPollInterval: %v", err)
	}
	if prev != 10*time.Millisecond {
		t.Fatalf("returned previous interval = %v, want 10ms", prev)
	}
	if got := sink.PollInterval(); got != 250*time.Millisecond {
		t.Fatalf("PollInterval() = %v, want 250ms after the second revision", got)
	}
}

func TestSetSinkPollIntervalNoOpWritesNoAudit(t *testing.T) {
	cap := &sinkCapture{}
	cfg := sinkTestConfig("")
	cfg.PollInterval = 500 * time.Millisecond
	_, sink, auditDir := startSinkTestLedger(t, cap, cfg)

	// One real revision, as a positive control: it must audit.
	if _, err := sink.SetSinkPollInterval(10 * time.Millisecond); err != nil {
		t.Fatalf("SetSinkPollInterval: %v", err)
	}
	events := sinkPollIntervalRetunedEvents(t, auditDir)
	if len(events) != 1 {
		t.Fatalf("retuned events = %d, want 1 after the real revision", len(events))
	}

	// A same-value revision is a no-op: same return, no new audit.
	prev, err := sink.SetSinkPollInterval(10 * time.Millisecond)
	if err != nil {
		t.Fatalf("same-value SetSinkPollInterval: %v", err)
	}
	if prev != 10*time.Millisecond {
		t.Fatalf("same-value revision returned %v, want the live 10ms", prev)
	}
	time.Sleep(300 * time.Millisecond) // let any stray event flush
	if n := sinkPollIntervalRetunedCount(t, auditDir); n != 1 {
		t.Fatalf("retuned events = %d, want still 1 after the no-op revision", n)
	}
}

func TestSetSinkPollIntervalAuditEventFields(t *testing.T) {
	cap := &sinkCapture{}
	cfg := sinkTestConfig("")
	cfg.PollInterval = 500 * time.Millisecond
	_, sink, auditDir := startSinkTestLedger(t, cap, cfg)

	prev, err := sink.SetSinkPollInterval(10 * time.Millisecond)
	if err != nil {
		t.Fatalf("SetSinkPollInterval: %v", err)
	}
	if prev != 500*time.Millisecond {
		t.Fatalf("returned previous interval = %v, want 500ms", prev)
	}
	events := sinkPollIntervalRetunedEvents(t, auditDir)
	if len(events) != 1 {
		t.Fatalf("retuned events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Actor != "SetSinkPollInterval" {
		t.Errorf("Actor = %q, want SetSinkPollInterval", ev.Actor)
	}
	if ev.VersionBefore != ev.VersionAfter {
		t.Errorf("version moved %d -> %d, want no bump", ev.VersionBefore, ev.VersionAfter)
	}
	if got := ev.Details["previous_poll_interval_ms"]; got != float64(500) {
		t.Errorf("previous_poll_interval_ms = %v, want 500", got)
	}
	if got := ev.Details["current_poll_interval_ms"]; got != float64(10) {
		t.Errorf("current_poll_interval_ms = %v, want 10", got)
	}
	// Only the two numeric ms fields ride in the event: no URL or
	// secret material, no other keys.
	if len(ev.Details) != 2 {
		t.Errorf("details = %v, want exactly previous/current poll interval ms", ev.Details)
	}
}

func TestSetSinkPollIntervalCtxActorAttribution(t *testing.T) {
	cap := &sinkCapture{}
	cfg := sinkTestConfig("")
	cfg.PollInterval = 500 * time.Millisecond
	_, sink, auditDir := startSinkTestLedger(t, cap, cfg)

	ctx := ContextWithActor(context.Background(), "ops-alan")
	if _, err := sink.SetSinkPollIntervalCtx(ctx, 10*time.Millisecond); err != nil {
		t.Fatalf("SetSinkPollIntervalCtx: %v", err)
	}
	// A plain call keeps the entrypoint name as the actor.
	if _, err := sink.SetSinkPollInterval(250 * time.Millisecond); err != nil {
		t.Fatalf("SetSinkPollInterval: %v", err)
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
			if ev.Op == "sink_poll_interval_retuned" {
				events = append(events, ev)
			}
		}
		return len(events) == 2
	})
	if events[0].Actor != "ops-alan" {
		t.Fatalf("first event Actor = %q, want ops-alan", events[0].Actor)
	}
	if got := events[0].Details["previous_poll_interval_ms"]; got != float64(500) {
		t.Fatalf("first previous_poll_interval_ms = %v, want 500", got)
	}
	if got := events[0].Details["current_poll_interval_ms"]; got != float64(10) {
		t.Fatalf("first current_poll_interval_ms = %v, want 10", got)
	}
	if events[1].Actor != "SetSinkPollInterval" {
		t.Fatalf("second event Actor = %q, want SetSinkPollInterval", events[1].Actor)
	}
	if got := events[1].Details["previous_poll_interval_ms"]; got != float64(10) {
		t.Fatalf("second previous_poll_interval_ms = %v, want 10", got)
	}
	if got := events[1].Details["current_poll_interval_ms"]; got != float64(250) {
		t.Fatalf("second current_poll_interval_ms = %v, want 250", got)
	}
}

func TestSetSinkPollIntervalInFlightSleepNotInterrupted(t *testing.T) {
	cap := &sinkCapture{}
	cfg := sinkTestConfig("")
	cfg.PollInterval = 500 * time.Millisecond
	l, sink, _ := startSinkTestLedger(t, cap, cfg)

	mustPostSink(t, l, "amend-sink-poll-sleep-1")
	waitForSink(t, 5*time.Second, "first delivery", func() bool { return cap.count() == 1 })
	// The sink is now inside a 500ms idle sleep (it entered the
	// sleep right after the first delivery).
	time.Sleep(100 * time.Millisecond)

	posted := time.Now()
	mustPostSink(t, l, "amend-sink-poll-sleep-2")
	if _, err := sink.SetSinkPollInterval(10 * time.Millisecond); err != nil {
		t.Fatalf("SetSinkPollInterval: %v", err)
	}
	// The in-flight sleep is not woken or shortened by the revision:
	// nothing arrives within 200ms of posting, even though the live
	// interval is now 10ms.
	time.Sleep(200 * time.Millisecond)
	if n := cap.count(); n != 1 {
		t.Fatalf("delivered %d events within 200ms of posting, want still 1: the in-flight 500ms sleep must run out", n)
	}
	waitForSink(t, 5*time.Second, "second delivery", func() bool { return cap.count() == 2 })
	times := cap.attemptTimes()
	if gap := times[1].Sub(posted); gap < 200*time.Millisecond {
		t.Fatalf("second delivery took only %s after posting, want >= 200ms: it must wait out the old 500ms sleep", gap)
	}
}

func TestSetSinkPollIntervalDeliveryUnaffected(t *testing.T) {
	cap := &sinkCapture{}
	cfg := sinkTestConfig("")
	cfg.PollInterval = 500 * time.Millisecond
	l, sink, _ := startSinkTestLedger(t, cap, cfg)

	// Revise immediately after posting, while the event is still
	// queued or in flight: the delivery itself must be untouched.
	mustPostSink(t, l, "amend-sink-poll-unaffected-1")
	if _, err := sink.SetSinkPollInterval(10 * time.Millisecond); err != nil {
		t.Fatalf("SetSinkPollInterval: %v", err)
	}
	// Wait on the sink's own counter, not the capture count: the
	// capture records the attempt before the sink marks it sent, so
	// a count-based wait can observe the delivery a hair early.
	waitForSink(t, 5*time.Second, "delivery", func() bool {
		sent, _, _ := sink.Stats()
		return sent == 1
	})
	if sent, failed, dead := sink.Stats(); sent != 1 || failed != 0 || dead != 0 {
		t.Fatalf("Stats = (%d, %d, %d), want (1, 0, 0)", sent, failed, dead)
	}
	_, _, _, seqs := cap.snapshot()
	if len(seqs) != 1 || seqs[0] != "1" {
		t.Fatalf("seqs = %v, want [1]", seqs)
	}
}

func TestSetSinkPollIntervalPreservesCursorStatsOrder(t *testing.T) {
	cap := &sinkCapture{}
	cfg := sinkTestConfig("")
	cfg.PollInterval = 500 * time.Millisecond
	l, sink, auditDir := startSinkTestLedger(t, cap, cfg)

	mustPostSink(t, l, "amend-sink-poll-order-1")
	mustPostSink(t, l, "amend-sink-poll-order-2")
	// Revise while the first deliveries are in flight or queued: the
	// cursor, the order, and the counters must all survive it.
	if _, err := sink.SetSinkPollInterval(10 * time.Millisecond); err != nil {
		t.Fatalf("SetSinkPollInterval: %v", err)
	}
	mustPostSink(t, l, "amend-sink-poll-order-3")
	mustPostSink(t, l, "amend-sink-poll-order-4")
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
	if got := sink.PollInterval(); got != 10*time.Millisecond {
		t.Fatalf("PollInterval() = %v, want 10ms", got)
	}
}

func TestSetSinkPollIntervalConcurrentRevisionNoRace(t *testing.T) {
	cap := &sinkCapture{}
	cfg := sinkTestConfig("")
	l, sink, _ := startSinkTestLedger(t, cap, cfg)

	var wg sync.WaitGroup
	// Concurrent posts and concurrent revisions: -race must stay
	// silent, and every event must still deliver exactly once.
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				mustPostSink(t, l, "amend-sink-poll-race")
			}
		}()
	}
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				d := time.Duration(10+g*10+i%5) * time.Millisecond
				_, _ = sink.SetSinkPollInterval(d)
				_ = sink.PollInterval()
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

func TestSetSinkPollIntervalDoesNotPersistAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	auditDir := filepath.Join(dir, "audit")
	al, err := NewAuditLog(auditDir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	t.Cleanup(func() { _ = al.Close() })
	l, _ := newSinkTestLedger(t, dir, WithAuditLog(al))

	cap := &sinkCapture{}
	srv := httptest.NewServer(http.HandlerFunc(cap.handler))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	cfg := sinkTestConfig(srv.URL)
	cfg.PollInterval = 500 * time.Millisecond
	sink, err := StartAuditSink(ctx, l, cfg)
	if err != nil {
		t.Fatalf("StartAuditSink: %v", err)
	}
	if _, err := sink.SetSinkPollInterval(10 * time.Millisecond); err != nil {
		t.Fatalf("SetSinkPollInterval: %v", err)
	}
	if got := sink.PollInterval(); got != 10*time.Millisecond {
		t.Fatalf("PollInterval() = %v, want 10ms before restart", got)
	}
	sink.Stop()

	// A fresh sink built from the same construction-time config
	// starts on 500ms again: the revision does not persist.
	sink2, err := StartAuditSink(ctx, l, cfg)
	if err != nil {
		t.Fatalf("restart StartAuditSink: %v", err)
	}
	defer sink2.Stop()
	if got := sink2.PollInterval(); got != 500*time.Millisecond {
		t.Fatalf("restarted PollInterval() = %v, want construction-time 500ms", got)
	}
	// The revision event itself stays in the audit trail (the audit
	// log flushes asynchronously, so poll for it).
	if n := len(sinkPollIntervalRetunedEvents(t, auditDir)); n != 1 {
		t.Fatalf("retuned events = %d, want 1 in the audit trail after restart", n)
	}
}
