package ledger

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestAuditLogFlushIntervalGetterFollowsLiveValue pins the live
// getter: the default interval is DefaultAuditFlushInterval (1s), the
// construction-time WithAuditLogFlushInterval value is reported, and
// a runtime revision moves both the getter and the live field — and
// returns the previous interval.
func TestAuditLogFlushIntervalGetterFollowsLiveValue(t *testing.T) {
	l, al, _ := retuneTestLedger(t, WithAuditLogFlushInterval(5*time.Second))
	if got := al.FlushInterval(); got != 5*time.Second {
		t.Fatalf("FlushInterval() = %v, want 5s", got)
	}
	if got, ok := l.AuditLogFlushInterval(); !ok || got != 5*time.Second {
		t.Fatalf("AuditLogFlushInterval() = %v, %v; want 5s, true", got, ok)
	}
	prev, err := l.SetAuditLogFlushInterval(250 * time.Millisecond)
	if err != nil {
		t.Fatalf("SetAuditLogFlushInterval(250ms): %v", err)
	}
	if prev != 5*time.Second {
		t.Fatalf("SetAuditLogFlushInterval returned previous = %v, want 5s", prev)
	}
	if got := al.FlushInterval(); got != 250*time.Millisecond {
		t.Fatalf("FlushInterval() after revision = %v, want 250ms", got)
	}
	if got, ok := l.AuditLogFlushInterval(); !ok || got != 250*time.Millisecond {
		t.Fatalf("AuditLogFlushInterval() after revision = %v, %v; want 250ms, true", got, ok)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// A log built without the option reports the default.
	_, al2, _ := retuneTestLedger(t)
	if got := al2.FlushInterval(); got != DefaultAuditFlushInterval {
		t.Fatalf("FlushInterval() default = %v, want %v", got, DefaultAuditFlushInterval)
	}
	if err := al2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestSetAuditLogFlushIntervalInvalidRejected pins the fail-closed
// gate: a non-positive interval is rejected and the live interval is
// left untouched, with no amendment event in the trail.
func TestSetAuditLogFlushIntervalInvalidRejected(t *testing.T) {
	l, al, dir := retuneTestLedger(t, WithAuditLogFlushInterval(2*time.Second))
	for _, d := range []time.Duration{0, -time.Second, -time.Minute} {
		if _, err := l.SetAuditLogFlushInterval(d); !errors.Is(err, ErrInvalidAuditLogFlushInterval) {
			t.Fatalf("SetAuditLogFlushInterval(%v) = %v, want ErrInvalidAuditLogFlushInterval", d, err)
		}
	}
	if got := al.FlushInterval(); got != 2*time.Second {
		t.Fatalf("FlushInterval() after rejected revisions = %v, want 2s", got)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := len(auditEventsByOp(t, dir)["audit_log_flush_interval_amended"]); n != 0 {
		t.Fatalf("rejected revisions wrote %d amendment events, want 0", n)
	}
}

// TestSetAuditLogFlushIntervalNoAuditLog pins the unconfigured
// rejection: a ledger with no audit log attached cannot revise an
// interval that does not exist.
func TestSetAuditLogFlushIntervalNoAuditLog(t *testing.T) {
	l := New()
	if _, err := l.SetAuditLogFlushInterval(time.Second); !errors.Is(err, ErrAuditLogNotConfigured) {
		t.Fatalf("SetAuditLogFlushInterval without audit log = %v, want ErrAuditLogNotConfigured", err)
	}
	if _, err := l.SetAuditLogFlushIntervalCtx(ContextWithActor(context.Background(), "ops"), time.Second); !errors.Is(err, ErrAuditLogNotConfigured) {
		t.Fatalf("SetAuditLogFlushIntervalCtx without audit log = %v, want ErrAuditLogNotConfigured", err)
	}
	if got, ok := l.AuditLogFlushInterval(); ok || got != 0 {
		t.Fatalf("AuditLogFlushInterval() without audit log = %v, %v; want 0, false", got, ok)
	}
}

// TestSetAuditLogFlushIntervalSameValueNoop pins the no-op: revising
// to the current interval returns the live value and writes no audit
// event.
func TestSetAuditLogFlushIntervalSameValueNoop(t *testing.T) {
	l, al, dir := retuneTestLedger(t, WithAuditLogFlushInterval(2*time.Second))
	prev, err := l.SetAuditLogFlushInterval(2 * time.Second)
	if err != nil {
		t.Fatalf("SetAuditLogFlushInterval(2s) same value: %v", err)
	}
	if prev != 2*time.Second {
		t.Fatalf("same-value revision returned previous = %v, want 2s", prev)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := len(auditEventsByOp(t, dir)["audit_log_flush_interval_amended"]); n != 0 {
		t.Fatalf("same-value revision wrote %d amendment events, want 0", n)
	}
}

// TestSetAuditLogFlushIntervalCtxActorAttribution pins LG-73
// attribution on the Ctx variant: the operator identity replaces the
// entrypoint name, while the plain setter keeps it. The amendment
// details carry the previous and new intervals in whole milliseconds.
func TestSetAuditLogFlushIntervalCtxActorAttribution(t *testing.T) {
	l, al, dir := retuneTestLedger(t, WithAuditLogFlushInterval(2*time.Second))
	ctx := ContextWithActor(context.Background(), "ops-flush")
	if _, err := l.SetAuditLogFlushIntervalCtx(ctx, 250*time.Millisecond); err != nil {
		t.Fatalf("SetAuditLogFlushIntervalCtx: %v", err)
	}
	if _, err := l.SetAuditLogFlushInterval(5 * time.Second); err != nil {
		t.Fatalf("SetAuditLogFlushInterval: %v", err)
	}
	waitAuditWritten(t, al, 3) // alert + 2 amendments
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	amended := auditEventsByOp(t, dir)["audit_log_flush_interval_amended"]
	if len(amended) != 2 {
		t.Fatalf("audit_log_flush_interval_amended events = %d, want 2", len(amended))
	}
	if amended[0].Actor != "ops-flush" {
		t.Errorf("first amendment actor = %q, want ops-flush", amended[0].Actor)
	}
	if amended[0].Details["previous_flush_interval_ms"] != float64(2000) ||
		amended[0].Details["flush_interval_ms"] != float64(250) {
		t.Errorf("first amendment details = %v, want previous 2000 / 250", amended[0].Details)
	}
	if amended[1].Actor != "SetAuditLogFlushInterval" {
		t.Errorf("second amendment actor = %q, want SetAuditLogFlushInterval", amended[1].Actor)
	}
	if amended[1].Details["previous_flush_interval_ms"] != float64(250) ||
		amended[1].Details["flush_interval_ms"] != float64(5000) {
		t.Errorf("second amendment details = %v, want previous 250 / 5000", amended[1].Details)
	}
}

// TestSetAuditLogFlushIntervalDoesNotBumpVersion pins that the
// revision is log infrastructure tuning, not ledger state: the ledger
// version does not move, and the trail holds exactly the alert and
// the one amendment — no journal entry, no outbox event, no extra
// audit noise.
func TestSetAuditLogFlushIntervalDoesNotBumpVersion(t *testing.T) {
	l, al, dir := retuneTestLedger(t)
	before := l.Version()
	if _, err := l.SetAuditLogFlushInterval(250 * time.Millisecond); err != nil {
		t.Fatalf("SetAuditLogFlushInterval: %v", err)
	}
	if got := l.Version(); got != before {
		t.Fatalf("Version() = %d after revision, want %d (unchanged)", got, before)
	}
	waitAuditWritten(t, al, 2) // alert + amendment
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, corrupt, err := ReadAuditLog(dir, todayAuditDay())
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt = %d, want 0", corrupt)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2 (alert + amendment)", len(events))
	}
}

// TestSetAuditLogFlushIntervalWritePathUnaffected pins that a long
// interval never gates the write path: events are written the moment
// they are logged — only the fsync cadence moves, not the Write
// syscall — so a 60s interval still lands events on disk promptly.
func TestSetAuditLogFlushIntervalWritePathUnaffected(t *testing.T) {
	_, al, dir := retuneTestLedger(t, WithAuditLogFlushInterval(60*time.Second))
	for i := 0; i < 3; i++ {
		if !al.Log(AuditEvent{Op: "post", Actor: "TestFlush", TraceID: fmt.Sprintf("write-%d", i)}) {
			t.Fatalf("Log(write-%d) dropped on a quiet log", i)
		}
	}
	waitAuditWritten(t, al, 4) // alert + 3 posts
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, corrupt, err := ReadAuditLog(dir, todayAuditDay())
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt = %d, want 0", corrupt)
	}
	if len(events) != 4 {
		t.Fatalf("events = %d, want 4 (alert + 3 posts)", len(events))
	}
}

// TestSetAuditLogFlushIntervalCountTriggerStillWorks pins that the
// every-128-events sync stays independent of the time-based cadence:
// with a 60s interval a burst of 200 events still lands on disk
// without waiting for a tick. Sync timing itself is not observable
// from outside the writer — the pin is that the write path never
// waits for the ticker — and the burst exercises the count-trigger
// code path (pending >= 128) mid-interval.
func TestSetAuditLogFlushIntervalCountTriggerStillWorks(t *testing.T) {
	_, al, dir := retuneTestLedger(t, WithAuditLogFlushInterval(60*time.Second))
	for i := 0; i < 200; i++ {
		if !al.Log(AuditEvent{Op: "post", Actor: "TestFlush", TraceID: fmt.Sprintf("burst-%d", i)}) {
			t.Fatalf("Log(burst-%d) dropped on a quiet log", i)
		}
	}
	// All 200 must be written before any tick fires (the ticker is
	// 60s out) and before Close.
	waitAuditWritten(t, al, 201) // alert + 200 posts
	events, corrupt, err := ReadAuditLog(dir, todayAuditDay())
	if err != nil {
		t.Fatalf("ReadAuditLog before Close: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt = %d, want 0", corrupt)
	}
	if len(events) != 201 {
		t.Fatalf("events on disk before Close = %d, want 201 (alert + 200 posts)", len(events))
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestSetAuditLogFlushIntervalConcurrentRevisionAndLog pins -race
// safety: concurrent revisions, concurrent Log calls, and the writer
// rebuilding its ticker must neither race nor break the hash chain.
func TestSetAuditLogFlushIntervalConcurrentRevisionAndLog(t *testing.T) {
	l, al, dir := retuneTestLedger(t, WithAuditLogFlushInterval(50*time.Millisecond))
	intervals := []time.Duration{25 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond}
	var wg sync.WaitGroup
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				if _, err := l.SetAuditLogFlushInterval(intervals[(r+i)%len(intervals)]); err != nil {
					t.Errorf("SetAuditLogFlushInterval: %v", err)
					return
				}
			}
		}(r)
	}
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				if !al.Log(AuditEvent{Op: "post", Actor: "TestFlush", TraceID: fmt.Sprintf("w%d-%d", w, i)}) {
					t.Errorf("Log(w%d-%d) dropped on a quiet log", w, i)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	waitAuditWritten(t, al, 101) // alert + 100 posts (amendments may add more)
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, corrupt, err := ReadAuditLog(dir, todayAuditDay())
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt = %d, want 0", corrupt)
	}
	// Every posted event must be present exactly once.
	seen := make(map[string]bool, len(events))
	for _, ev := range events {
		if ev.TraceID != "" {
			seen[ev.TraceID] = true
		}
	}
	for w := 0; w < 4; w++ {
		for i := 0; i < 25; i++ {
			if !seen[fmt.Sprintf("w%d-%d", w, i)] {
				t.Fatalf("missing posted event w%d-%d after concurrent revisions", w, i)
			}
		}
	}
	rep, err := VerifyAuditLog(dir)
	if err != nil {
		t.Fatalf("VerifyAuditLog: %v", err)
	}
	if !rep.Ok || rep.FirstBreak != nil || rep.CheckedEntries != len(events) {
		t.Fatalf("chain after concurrent revisions: ok=%v checked=%d firstBreak=%+v", rep.Ok, rep.CheckedEntries, rep.FirstBreak)
	}
}

// TestSetAuditLogFlushIntervalCloseDoesNotDeadlock pins the
// flushMu/Close serialization (along LG-309): a revision racing
// Close must return, and a revision after Close takes the closed
// path — updating the live field without touching a dead ticker —
// instead of blocking on the writer forever.
func TestSetAuditLogFlushIntervalCloseDoesNotDeadlock(t *testing.T) {
	l, al, _ := retuneTestLedger(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = l.SetAuditLogFlushInterval(100 * time.Millisecond)
	}()
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("revision racing Close did not return within 10s")
	}
	// After Close the revision takes the closed path: it still
	// returns the previous value and moves the getter, with no
	// writer left to rebuild. Whichever side won the race, the
	// racing revision installed 100ms first.
	prev, err := l.SetAuditLogFlushInterval(200 * time.Millisecond)
	if err != nil {
		t.Fatalf("SetAuditLogFlushInterval after Close: %v", err)
	}
	if prev != 100*time.Millisecond {
		t.Fatalf("previous after Close = %v, want 100ms", prev)
	}
	if got, ok := l.AuditLogFlushInterval(); !ok || got != 200*time.Millisecond {
		t.Fatalf("AuditLogFlushInterval() after Close = %v, %v; want 200ms, true", got, ok)
	}
}

// TestSetAuditLogFlushIntervalRestartRestoresConstructionValue pins
// that the revision is live-only: a fresh AuditLog built with the
// same construction-time options reports the construction value, not
// the revised one — the amendment events stay in the trail for
// traceability, but the interval itself does not persist.
func TestSetAuditLogFlushIntervalRestartRestoresConstructionValue(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir, WithAuditLogFlushInterval(5*time.Second))
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	prev, err := l.SetAuditLogFlushInterval(2 * time.Second)
	if err != nil {
		t.Fatalf("SetAuditLogFlushInterval: %v", err)
	}
	if prev != 5*time.Second {
		t.Fatalf("previous = %v, want 5s", prev)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	al2, err := NewAuditLog(dir, WithAuditLogFlushInterval(5*time.Second))
	if err != nil {
		t.Fatalf("NewAuditLog (restart): %v", err)
	}
	defer func() {
		if err := al2.Close(); err != nil {
			t.Fatalf("Close (restart): %v", err)
		}
	}()
	if got := al2.FlushInterval(); got != 5*time.Second {
		t.Fatalf("FlushInterval() after restart = %v, want 5s (construction value)", got)
	}
}
