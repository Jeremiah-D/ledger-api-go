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

// Audit-sink poll-page-limit amendment tests (LG-350). They reuse the
// audit_sink_test.go harness: sinkCapture, sinkTestConfig (PollInterval
// 10ms), startSinkTestLedger in audit_sink_delivery_amend_test.go,
// mustPostSink, and waitForSink.
//
// The page size is only observable through the value run() reads for
// its next poll (the unexported pollLimit, asserted white-box below)
// plus the delivery path itself: a small limit forces the sink
// through several polls to drain one outbox, so a multi-event fake
// outbox plus a full ordered delivery proves the revised limit is
// the one the run loop actually polls with.

// sinkPollLimitRetunedEvents polls the audit log until at least one
// sink_poll_limit_retuned event is visible, then returns all of them.
func sinkPollLimitRetunedEvents(t *testing.T, auditDir string) []AuditEvent {
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
			if ev.Op == "sink_poll_limit_retuned" {
				out = append(out, ev)
			}
		}
		return len(out) > 0
	})
	return out
}

func sinkPollLimitRetunedCount(t *testing.T, auditDir string) int {
	t.Helper()
	day := time.Now().UTC().Format("2006-01-02")
	events, _, err := ReadAuditLog(auditDir, day)
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	n := 0
	for _, ev := range events {
		if ev.Op == "sink_poll_limit_retuned" {
			n++
		}
	}
	return n
}

func TestSetSinkPollLimitValidationRejectsAndKeepsLive(t *testing.T) {
	cap := &sinkCapture{}
	_, sink, _ := startSinkTestLedger(t, cap, sinkTestConfig(""))

	if got := sink.PollLimit(); got != sinkPollLimit {
		t.Fatalf("PollLimit() = %d, want the %d-event default", got, sinkPollLimit)
	}
	for _, n := range []int{0, -1, -100} {
		if prev, err := sink.SetSinkPollLimit(n); !errors.Is(err, ErrInvalidSinkPollLimit) {
			t.Errorf("SetSinkPollLimit(%d): err = %v, want ErrInvalidSinkPollLimit", n, err)
		} else if prev != sinkPollLimit {
			t.Errorf("SetSinkPollLimit(%d): returned %d, want the live %d", n, prev, sinkPollLimit)
		}
		if got := sink.PollLimit(); got != sinkPollLimit {
			t.Errorf("SetSinkPollLimit(%d): live limit moved to %d, want unchanged %d", n, got, sinkPollLimit)
		}
	}
}

func TestSetSinkPollLimitReturnsPreviousValue(t *testing.T) {
	cap := &sinkCapture{}
	_, sink, _ := startSinkTestLedger(t, cap, sinkTestConfig(""))

	prev, err := sink.SetSinkPollLimit(10)
	if err != nil {
		t.Fatalf("SetSinkPollLimit: %v", err)
	}
	if prev != sinkPollLimit {
		t.Fatalf("returned previous limit = %d, want %d", prev, sinkPollLimit)
	}
	if got := sink.PollLimit(); got != 10 {
		t.Fatalf("PollLimit() = %d, want 10 after the revision", got)
	}
	// The getter follows a second revision the same way.
	prev, err = sink.SetSinkPollLimit(250)
	if err != nil {
		t.Fatalf("SetSinkPollLimit: %v", err)
	}
	if prev != 10 {
		t.Fatalf("returned previous limit = %d, want 10", prev)
	}
	if got := sink.PollLimit(); got != 250 {
		t.Fatalf("PollLimit() = %d, want 250 after the second revision", got)
	}
}

func TestSetSinkPollLimitNoOpWritesNoAudit(t *testing.T) {
	cap := &sinkCapture{}
	_, sink, auditDir := startSinkTestLedger(t, cap, sinkTestConfig(""))

	// One real revision, as a positive control: it must audit.
	if _, err := sink.SetSinkPollLimit(10); err != nil {
		t.Fatalf("SetSinkPollLimit: %v", err)
	}
	events := sinkPollLimitRetunedEvents(t, auditDir)
	if len(events) != 1 {
		t.Fatalf("retuned events = %d, want 1 after the real revision", len(events))
	}

	// A same-value revision is a no-op: same return, no new audit.
	prev, err := sink.SetSinkPollLimit(10)
	if err != nil {
		t.Fatalf("same-value SetSinkPollLimit: %v", err)
	}
	if prev != 10 {
		t.Fatalf("same-value revision returned %d, want the live 10", prev)
	}
	time.Sleep(300 * time.Millisecond) // let any stray event flush
	if n := sinkPollLimitRetunedCount(t, auditDir); n != 1 {
		t.Fatalf("retuned events = %d, want still 1 after the no-op revision", n)
	}
}

func TestSetSinkPollLimitAuditEventFields(t *testing.T) {
	cap := &sinkCapture{}
	_, sink, auditDir := startSinkTestLedger(t, cap, sinkTestConfig(""))

	prev, err := sink.SetSinkPollLimit(10)
	if err != nil {
		t.Fatalf("SetSinkPollLimit: %v", err)
	}
	if prev != sinkPollLimit {
		t.Fatalf("returned previous limit = %d, want %d", prev, sinkPollLimit)
	}
	events := sinkPollLimitRetunedEvents(t, auditDir)
	if len(events) != 1 {
		t.Fatalf("retuned events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Actor != "SetSinkPollLimit" {
		t.Errorf("Actor = %q, want SetSinkPollLimit", ev.Actor)
	}
	if ev.VersionBefore != ev.VersionAfter {
		t.Errorf("version moved %d -> %d, want no bump", ev.VersionBefore, ev.VersionAfter)
	}
	if got := ev.Details["previous_poll_limit"]; got != float64(sinkPollLimit) {
		t.Errorf("previous_poll_limit = %v, want %d", got, sinkPollLimit)
	}
	if got := ev.Details["current_poll_limit"]; got != float64(10) {
		t.Errorf("current_poll_limit = %v, want 10", got)
	}
	// Only the two integer fields ride in the event: no URL or
	// secret material, no other keys.
	if len(ev.Details) != 2 {
		t.Errorf("details = %v, want exactly previous/current poll limit", ev.Details)
	}
}

func TestSetSinkPollLimitCtxActorAttribution(t *testing.T) {
	cap := &sinkCapture{}
	_, sink, auditDir := startSinkTestLedger(t, cap, sinkTestConfig(""))

	ctx := ContextWithActor(context.Background(), "ops-alan")
	if _, err := sink.SetSinkPollLimitCtx(ctx, 10); err != nil {
		t.Fatalf("SetSinkPollLimitCtx: %v", err)
	}
	// A plain call keeps the entrypoint name as the actor.
	if _, err := sink.SetSinkPollLimit(250); err != nil {
		t.Fatalf("SetSinkPollLimit: %v", err)
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
			if ev.Op == "sink_poll_limit_retuned" {
				events = append(events, ev)
			}
		}
		return len(events) == 2
	})
	if events[0].Actor != "ops-alan" {
		t.Fatalf("first event Actor = %q, want ops-alan", events[0].Actor)
	}
	if got := events[0].Details["previous_poll_limit"]; got != float64(sinkPollLimit) {
		t.Fatalf("first previous_poll_limit = %v, want %d", got, sinkPollLimit)
	}
	if got := events[0].Details["current_poll_limit"]; got != float64(10) {
		t.Fatalf("first current_poll_limit = %v, want 10", got)
	}
	if events[1].Actor != "SetSinkPollLimit" {
		t.Fatalf("second event Actor = %q, want SetSinkPollLimit", events[1].Actor)
	}
	if got := events[1].Details["previous_poll_limit"]; got != float64(10) {
		t.Fatalf("second previous_poll_limit = %v, want 10", got)
	}
	if got := events[1].Details["current_poll_limit"]; got != float64(250) {
		t.Fatalf("second current_poll_limit = %v, want 250", got)
	}
}

func TestSetSinkPollLimitNextPollUsesNewPage(t *testing.T) {
	cap := &sinkCapture{}
	l, sink, _ := startSinkTestLedger(t, cap, sinkTestConfig(""))

	// Revise before anything is posted: the sink's next poll must
	// use the new page size. The white-box read is the exact value
	// run() passes to ReadOutboxEvents for that poll.
	if _, err := sink.SetSinkPollLimit(2); err != nil {
		t.Fatalf("SetSinkPollLimit: %v", err)
	}
	if got := sink.pollLimit(); got != 2 {
		t.Fatalf("pollLimit() = %d, want 2: the next poll must read the revised limit", got)
	}

	// A fake outbox with more events than one page: the sink must
	// drain it through several 2-event polls, in order.
	for i := 1; i <= 5; i++ {
		mustPostSink(t, l, "amend-sink-limit-page")
	}
	// Wait on the sink's own counter, not the capture count: the
	// capture records the attempt before the sink marks it sent, so
	// a count-based wait can observe a delivery a hair early.
	waitForSink(t, 10*time.Second, "5 deliveries", func() bool {
		sent, _, _ := sink.Stats()
		return sent == 5
	})
	_, _, _, seqs := cap.snapshot()
	if len(seqs) != 5 || seqs[0] != "1" || seqs[1] != "2" || seqs[2] != "3" || seqs[3] != "4" || seqs[4] != "5" {
		t.Fatalf("seqs = %v, want [1 2 3 4 5]", seqs)
	}
	if sent, failed, dead := sink.Stats(); sent != 5 || failed != 0 || dead != 0 {
		t.Fatalf("Stats = (%d, %d, %d), want (5, 0, 0)", sent, failed, dead)
	}

	// A limit of 1 exercises the multi-poll path maximally: every
	// event rides its own poll, and the stream still drains in
	// order.
	if _, err := sink.SetSinkPollLimit(1); err != nil {
		t.Fatalf("SetSinkPollLimit: %v", err)
	}
	for i := 1; i <= 3; i++ {
		mustPostSink(t, l, "amend-sink-limit-page-1")
	}
	waitForSink(t, 10*time.Second, "8 deliveries", func() bool {
		sent, _, _ := sink.Stats()
		return sent == 8
	})
	_, _, _, seqs = cap.snapshot()
	if len(seqs) != 8 || seqs[5] != "6" || seqs[6] != "7" || seqs[7] != "8" {
		t.Fatalf("seqs = %v, want [1..8] in order", seqs)
	}
}

func TestSetSinkPollLimitInFlightPageUnaffected(t *testing.T) {
	cap := &sinkCapture{}
	// Revise while the first delivery is in flight: the page already
	// fetched must deliver in full under the old limit, and the
	// revision must not disturb it. (The hook is installed before
	// the sink starts, so the first attempt cannot slip past it.)
	var sink *AuditSink
	var once sync.Once
	cap.onAttempt = func() {
		once.Do(func() {
			if _, err := sink.SetSinkPollLimit(1); err != nil {
				t.Errorf("SetSinkPollLimit mid-delivery: %v", err)
			}
		})
	}
	l, s, _ := startSinkTestLedger(t, cap, sinkTestConfig(""))
	sink = s
	// Post 3 events, then revise while the first delivery is in
	// flight: the page already fetched must deliver in full under
	// the old limit, and the revision must not disturb it.
	for i := 1; i <= 3; i++ {
		mustPostSink(t, l, "amend-sink-limit-inflight")
	}
	waitForSink(t, 10*time.Second, "3 deliveries", func() bool {
		sent, _, _ := sink.Stats()
		return sent == 3
	})
	_, _, _, seqs := cap.snapshot()
	if len(seqs) != 3 || seqs[0] != "1" || seqs[1] != "2" || seqs[2] != "3" {
		t.Fatalf("seqs = %v, want [1 2 3]", seqs)
	}
	if sent, failed, dead := sink.Stats(); sent != 3 || failed != 0 || dead != 0 {
		t.Fatalf("Stats = (%d, %d, %d), want (3, 0, 0)", sent, failed, dead)
	}
	if got := sink.PollLimit(); got != 1 {
		t.Fatalf("PollLimit() = %d, want 1 after the mid-delivery revision", got)
	}
}

func TestSetSinkPollLimitPreservesCursorStatsOrder(t *testing.T) {
	cap := &sinkCapture{}
	l, sink, auditDir := startSinkTestLedger(t, cap, sinkTestConfig(""))

	mustPostSink(t, l, "amend-sink-limit-order-1")
	mustPostSink(t, l, "amend-sink-limit-order-2")
	// Revise while the first deliveries are in flight or queued: the
	// cursor, the order, and the counters must all survive it.
	if _, err := sink.SetSinkPollLimit(2); err != nil {
		t.Fatalf("SetSinkPollLimit: %v", err)
	}
	mustPostSink(t, l, "amend-sink-limit-order-3")
	mustPostSink(t, l, "amend-sink-limit-order-4")
	waitForSink(t, 10*time.Second, "4 deliveries", func() bool {
		sent, _, _ := sink.Stats()
		return sent == 4
	})
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
	if got := sink.PollLimit(); got != 2 {
		t.Fatalf("PollLimit() = %d, want 2", got)
	}
}

func TestSetSinkPollLimitConcurrentRevisionNoRace(t *testing.T) {
	cap := &sinkCapture{}
	l, sink, _ := startSinkTestLedger(t, cap, sinkTestConfig(""))

	var wg sync.WaitGroup
	// Concurrent posts and concurrent revisions: -race must stay
	// silent, and every event must still deliver exactly once.
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				mustPostSink(t, l, "amend-sink-limit-race")
			}
		}()
	}
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				_, _ = sink.SetSinkPollLimit(1 + g*10 + i%5)
				_ = sink.PollLimit()
				_ = sink.pollLimit()
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

func TestSetSinkPollLimitDoesNotPersistAcrossRestart(t *testing.T) {
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
	sink, err := StartAuditSink(ctx, l, cfg)
	if err != nil {
		t.Fatalf("StartAuditSink: %v", err)
	}
	if _, err := sink.SetSinkPollLimit(5); err != nil {
		t.Fatalf("SetSinkPollLimit: %v", err)
	}
	if got := sink.PollLimit(); got != 5 {
		t.Fatalf("PollLimit() = %d, want 5 before restart", got)
	}
	sink.Stop()

	// A fresh sink built from the same construction-time config
	// starts on the 100-event default again: the revision does not
	// persist.
	sink2, err := StartAuditSink(ctx, l, cfg)
	if err != nil {
		t.Fatalf("restart StartAuditSink: %v", err)
	}
	defer sink2.Stop()
	if got := sink2.PollLimit(); got != sinkPollLimit {
		t.Fatalf("restarted PollLimit() = %d, want the %d-event default", got, sinkPollLimit)
	}
	// The revision event itself stays in the audit trail (the audit
	// log flushes asynchronously, so poll for it).
	if n := len(sinkPollLimitRetunedEvents(t, auditDir)); n != 1 {
		t.Fatalf("retuned events = %d, want 1 in the audit trail after restart", n)
	}
}
