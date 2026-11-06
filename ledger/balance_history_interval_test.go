package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Balance-history sampling interval runtime revision tests (LG-330).
// They reuse mustAuditLog from ledger_audit_test.go,
// postForHistory from ledger_balance_history_test.go, and waitFor
// from ledger_hold_sweep_test.go.

// startRetuneTestBalanceHistoryRecorder starts a balance-history
// recorder on a fresh ledger with an audit log, one posted entry
// (cash/USD + equity/USD = 2 points per tick), and a long retention
// window, so every tick leaves two more rows in dir for the test to
// count.
func startRetuneTestBalanceHistoryRecorder(t *testing.T, interval time.Duration) (*Ledger, *BalanceHistoryRecorder, *AuditLog, string, context.CancelFunc) {
	t.Helper()
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	postForHistory(t, l, "bhr330-e1", "cash", "equity", 500, "USD", "bhr330-k1")
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	r := StartBalanceHistoryRecorder(ctx, l, BalanceHistoryConfig{
		Dir:      dir,
		Interval: interval,
		MaxAgeMs: int64(90 * 24 * time.Hour / time.Millisecond),
	}, nil)
	if r == nil {
		t.Fatal("StartBalanceHistoryRecorder returned nil for a valid config")
	}
	return l, r, al, dir, cancel
}

// retunedBalanceHistoryIntervalEvents stops the recorder, closes the
// audit log (which drains the async writer), and returns every
// balance_history_interval_retuned event recorded in it. The ledger
// must not be used after this call.
func retunedBalanceHistoryIntervalEvents(t *testing.T, r *BalanceHistoryRecorder, al *AuditLog) []AuditEvent {
	t.Helper()
	r.Stop()
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
		if ev.Op == "balance_history_interval_retuned" {
			out = append(out, ev)
		}
	}
	return out
}

// historyRows returns how many rows the series holds in dir — 2 per
// successful tick while the ledger holds one entry (cash/USD +
// equity/USD).
func historyRows(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	for _, acct := range []AccountID{"cash", "equity"} {
		got, err := QueryBalanceHistory(dir, acct, "USD", time.Time{}, time.Time{})
		if err != nil {
			t.Fatalf("QueryBalanceHistory: %v", err)
		}
		n += len(got)
	}
	return n
}

func TestSetBalanceHistoryIntervalInvalidRejectedAndKeepsLive(t *testing.T) {
	_, r, al, _, cancel := startRetuneTestBalanceHistoryRecorder(t, time.Hour)
	defer cancel()

	for _, d := range []time.Duration{0, -time.Hour, -time.Nanosecond} {
		prev, err := r.SetInterval(d)
		if !errors.Is(err, ErrInvalidBalanceHistoryInterval) {
			t.Fatalf("SetInterval(%v): err = %v, want ErrInvalidBalanceHistoryInterval", d, err)
		}
		if prev != time.Hour {
			t.Fatalf("SetInterval(%v) returned %v, want the live 1h", d, prev)
		}
		if got := r.Interval(); got != time.Hour {
			t.Fatalf("Interval() = %v after rejected %v, want still 1h", got, d)
		}
	}
	// The Ctx variant rejects the same way.
	if prev, err := r.SetIntervalCtx(context.Background(), 0); !errors.Is(err, ErrInvalidBalanceHistoryInterval) || prev != time.Hour {
		t.Fatalf("SetIntervalCtx(0) = (%v, %v), want (1h, ErrInvalidBalanceHistoryInterval)", prev, err)
	}
	// Zero is rejected here, unlike the startup-time disable rule:
	// zero would stop the whole worker, and a runtime revision
	// never silently disables it.
	if events := retunedBalanceHistoryIntervalEvents(t, r, al); len(events) != 0 {
		t.Fatalf("retuned events = %d, want 0 after only rejected revisions", len(events))
	}
}

func TestSetBalanceHistoryIntervalSameValueNoOpNoAudit(t *testing.T) {
	_, r, al, _, cancel := startRetuneTestBalanceHistoryRecorder(t, time.Hour)
	defer cancel()

	prev, err := r.SetInterval(time.Hour)
	if err != nil {
		t.Fatalf("same-value SetInterval: %v", err)
	}
	if prev != time.Hour {
		t.Fatalf("same-value revision returned %v, want the live 1h", prev)
	}
	// One real revision, then the same value again: only the real
	// one audits.
	if _, err := r.SetInterval(30 * time.Minute); err != nil {
		t.Fatalf("SetInterval(30m): %v", err)
	}
	if prev, err := r.SetInterval(30 * time.Minute); err != nil || prev != 30*time.Minute {
		t.Fatalf("same-value SetInterval(30m) = (%v, %v), want (30m, nil)", prev, err)
	}
	if events := retunedBalanceHistoryIntervalEvents(t, r, al); len(events) != 1 {
		t.Fatalf("retuned events = %d, want 1 after the real revision plus the no-ops", len(events))
	}
}

func TestSetBalanceHistoryIntervalReturnsPreviousValue(t *testing.T) {
	_, r, al, _, cancel := startRetuneTestBalanceHistoryRecorder(t, 500*time.Millisecond)
	defer cancel()
	defer r.Stop()
	defer func() { _ = al.Close() }()

	prev, err := r.SetInterval(20 * time.Millisecond)
	if err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	if prev != 500*time.Millisecond {
		t.Fatalf("returned previous interval = %v, want 500ms", prev)
	}
	if got := r.Interval(); got != 20*time.Millisecond {
		t.Fatalf("Interval() = %v after the revision, want 20ms", got)
	}
}

func TestSetBalanceHistoryIntervalNextTickUsesNewInterval(t *testing.T) {
	_, r, al, dir, cancel := startRetuneTestBalanceHistoryRecorder(t, 500*time.Millisecond)
	defer cancel()
	defer r.Stop()
	defer func() { _ = al.Close() }()

	// The first tick lands on the old 500ms cadence.
	waitFor(t, 3*time.Second, "first tick", func() bool { return r.Ticks() >= 1 })

	// Tighten to 20ms: the next ticks must accelerate — three more
	// ticks land in ~60ms, not ~1.5s on the old rhythm.
	if _, err := r.SetInterval(20 * time.Millisecond); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	if got := r.Interval(); got != 20*time.Millisecond {
		t.Fatalf("Interval() = %v, want 20ms after the revision", got)
	}
	start := time.Now()
	waitFor(t, 3*time.Second, "4 ticks", func() bool { return r.Ticks() >= 4 })
	if took := time.Since(start); took > time.Second {
		t.Fatalf("3 ticks after tightening took %s, want < 1s on the 20ms rhythm (would take ~1.5s on the old 500ms rhythm)", took)
	}
	// And the accelerated ticks really land rows.
	if n := historyRows(t, dir); n < 8 {
		t.Fatalf("rows = %d after the accelerated ticks, want at least 8 (4 ticks x 2 points)", n)
	}
}

func TestSetBalanceHistoryIntervalInFlightSamplingNotInterrupted(t *testing.T) {
	al := mustAuditLog(t)
	defer func() { _ = al.Close() }()
	l := New(WithAuditLog(al))
	postForHistory(t, l, "bhr330-e1", "cash", "equity", 500, "USD", "bhr330-k1")
	dir := t.TempDir()

	// The first onTick call blocks the run loop inside the callback,
	// so the sample is in flight (its rows are already on disk, its
	// counters already advanced) while the revision lands.
	release := make(chan struct{})
	var calls atomic.Uint64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := StartBalanceHistoryRecorder(ctx, l, BalanceHistoryConfig{
		Dir:      dir,
		Interval: 100 * time.Millisecond,
		MaxAgeMs: int64(90 * 24 * time.Hour / time.Millisecond),
	}, func(_ int, _ error) {
		if calls.Add(1) == 1 {
			select {
			case <-release:
			case <-time.After(10 * time.Second):
			}
		}
	})
	if r == nil {
		t.Fatal("StartBalanceHistoryRecorder returned nil for a valid config")
	}
	defer r.Stop()

	// The first sample lands and the loop blocks in onTick.
	waitFor(t, 3*time.Second, "first tick", func() bool { return r.Ticks() >= 1 })

	// Revise while that sample is still in flight: the revision
	// must not cut it short, and it never calls tick() while
	// holding r.mu, so it cannot stall behind it either.
	if _, err := r.SetInterval(5 * time.Minute); err != nil {
		t.Fatalf("SetInterval during an in-flight sample: %v", err)
	}
	close(release)

	// The in-flight sample completes: its rows are on disk and its
	// counters are continuous — one tick, two points.
	waitFor(t, 3*time.Second, "in-flight tick to finish", func() bool { return calls.Load() >= 1 })
	if got := r.Ticks(); got != 1 {
		t.Fatalf("Ticks() = %d, want 1 (the in-flight sample, counted once)", got)
	}
	if got := r.PointsTotal(); got != 2 {
		t.Fatalf("PointsTotal() = %d, want 2 (cash/USD + equity/USD from the in-flight sample)", got)
	}
	if n := historyRows(t, dir); n != 2 {
		t.Fatalf("rows = %d, want 2 from the in-flight sample", n)
	}
	// The worker keeps ticking on the revised cadence afterwards:
	// loosen back to 20ms and the next ticks land without a
	// restart (ticks only grow, never reset).
	if _, err := r.SetInterval(20 * time.Millisecond); err != nil {
		t.Fatalf("SetInterval(20ms): %v", err)
	}
	waitFor(t, 3*time.Second, "ticks after the revision", func() bool { return r.Ticks() >= 3 })
	if got := r.Interval(); got != 20*time.Millisecond {
		t.Fatalf("Interval() = %v after the revision, want 20ms", got)
	}
}

func TestSetBalanceHistoryIntervalDoesNotSwallowTriggeredTick(t *testing.T) {
	al := mustAuditLog(t)
	defer func() { _ = al.Close() }()
	l := New(WithAuditLog(al))
	postForHistory(t, l, "bhr330-e1", "cash", "equity", 500, "USD", "bhr330-k1")
	dir := t.TempDir()

	// The first onTick call blocks the run loop inside the
	// callback, so the next ticker firing buffers in ticker.C
	// (cap 1) while the loop cannot consume it.
	release := make(chan struct{})
	var calls atomic.Uint64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := StartBalanceHistoryRecorder(ctx, l, BalanceHistoryConfig{
		Dir:      dir,
		Interval: 100 * time.Millisecond,
		MaxAgeMs: int64(90 * 24 * time.Hour / time.Millisecond),
	}, func(_ int, _ error) {
		if calls.Add(1) == 1 {
			select {
			case <-release:
			case <-time.After(10 * time.Second):
			}
		}
	})
	if r == nil {
		t.Fatal("StartBalanceHistoryRecorder returned nil for a valid config")
	}
	defer r.Stop()

	// The first sample lands and the loop blocks in onTick.
	waitFor(t, 3*time.Second, "first tick", func() bool { return r.Ticks() >= 1 })
	// Let the next tick fire while the loop is still blocked: it
	// buffers in ticker.C, unconsumed. (250ms clears the 200ms
	// firing with margin for a slow scheduler.)
	time.Sleep(250 * time.Millisecond)

	// Revise while that tick is pending, then release the loop.
	if _, err := r.SetInterval(time.Hour); err != nil {
		t.Fatalf("SetInterval with a tick pending: %v", err)
	}
	close(release)

	// The pending tick must still run exactly once — either
	// straight off the ticker channel or via the retune branch's
	// drain — and nothing further fires on the loosened 1h cadence.
	// (10s, not 3s: under a loaded -race full-suite run the worker
	// goroutine can be starved for seconds after the release, and
	// the tick still lands — a short timeout would report that
	// scheduling delay as a swallowed tick.)
	waitFor(t, 10*time.Second, "2 ticks", func() bool { return r.Ticks() >= 2 })
	if got := r.Ticks(); got != 2 {
		t.Fatalf("Ticks() = %d, want exactly 2 (first sample + drained tick)", got)
	}
	if n := historyRows(t, dir); n != 4 {
		t.Fatalf("rows = %d, want 4 (2 ticks x 2 points)", n)
	}
	time.Sleep(300 * time.Millisecond)
	if got := r.Ticks(); got != 2 {
		t.Fatalf("Ticks() = %d after settling, want still 2 on the loosened 1h rhythm", got)
	}
	if n := historyRows(t, dir); n != 4 {
		t.Fatalf("rows = %d after settling, want still 4", n)
	}
}

func TestSetBalanceHistoryIntervalMaxAgePruningUnchangedAfterRetune(t *testing.T) {
	al := mustAuditLog(t)
	defer func() { _ = al.Close() }()
	l := New(WithAuditLog(al))
	postForHistory(t, l, "bhr330-e1", "cash", "equity", 500, "USD", "bhr330-k1")
	dir := t.TempDir()

	// A stale day-file from the recorder's own naming scheme, last
	// modified 48h ago, plus a foreign file of the same age that no
	// pruning must ever touch.
	stale := filepath.Join(dir, "balance-history-2020-01-01.jsonl")
	if err := os.WriteFile(stale, []byte("{\"account\":\"cash\",\"currency\":\"USD\",\"balance_cents\":1,\"version\":1,\"at\":\"2020-01-01T00:00:00Z\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(foreign, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(foreign, old, old); err != nil {
		t.Fatal(err)
	}

	// A 1h retention window: the first tick prunes the stale file.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := StartBalanceHistoryRecorder(ctx, l, BalanceHistoryConfig{
		Dir:      dir,
		Interval: 100 * time.Millisecond,
		MaxAgeMs: int64(time.Hour / time.Millisecond),
	}, nil)
	if r == nil {
		t.Fatal("StartBalanceHistoryRecorder returned nil for a valid config")
	}
	defer r.Stop()

	waitFor(t, 3*time.Second, "first tick", func() bool { return r.Ticks() >= 1 })
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale file still present after the first tick (err = %v), want pruned by the unchanged 1h window", err)
	}

	// Revise the cadence and let more ticks land: pruning still
	// follows the original 1h window — the fresh day-file is never
	// pruned, the foreign file is never touched, and no extra files
	// disappear.
	if _, err := r.SetInterval(20 * time.Millisecond); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	waitFor(t, 3*time.Second, "ticks after the revision", func() bool { return r.Ticks() >= 3 })
	if n := historyRows(t, dir); n < 6 {
		t.Fatalf("rows = %d, want at least 6 (3 ticks x 2 points) — the fresh day-file must survive pruning", n)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("foreign file missing after the revision (err = %v), want untouched", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("files = %v, want exactly [today's day-file, notes.txt]", names)
	}
}

func TestSetBalanceHistoryIntervalConcurrentRevisionAndTicksRaceFree(t *testing.T) {
	_, r, al, dir, cancel := startRetuneTestBalanceHistoryRecorder(t, 10*time.Millisecond)
	defer cancel()
	defer r.Stop()
	defer func() { _ = al.Close() }()

	var wg sync.WaitGroup
	// Concurrent revisions from several goroutines while the worker
	// ticks underneath, plus direct tick() calls from another
	// goroutine: -race must stay silent, and the worker must keep
	// sampling on some positive interval.
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				d := time.Duration(10+g*10+i%5) * time.Millisecond
				_, _ = r.SetInterval(d)
				_ = r.Interval()
			}
		}(g)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			r.tick()
		}
	}()
	wg.Wait()
	if got := r.Interval(); got <= 0 {
		t.Fatalf("Interval() = %v after concurrent revisions, want a positive live value", got)
	}
	// The revision storm rebuilds the ticker on every revision, so
	// the countdown restarts while revisions are still landing and
	// no tick may have fired yet; once the storm ends the last
	// rebuild's ticker fires within one live interval.
	waitFor(t, 3*time.Second, "a tick after the concurrent revisions", func() bool {
		return r.Ticks() >= 1
	})
	if n := historyRows(t, dir); n < 2 {
		t.Fatalf("rows = %d after the concurrent revisions, want at least 2 (one tick landed)", n)
	}
}

func TestSetBalanceHistoryIntervalCtxActorAttribution(t *testing.T) {
	_, r, al, _, cancel := startRetuneTestBalanceHistoryRecorder(t, time.Hour)
	defer cancel()

	ctx := ContextWithActor(context.Background(), "ops-alan")
	if _, err := r.SetIntervalCtx(ctx, 30*time.Minute); err != nil {
		t.Fatalf("SetIntervalCtx: %v", err)
	}
	// A plain call keeps the entrypoint name as the actor.
	if _, err := r.SetInterval(2 * time.Hour); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	events := retunedBalanceHistoryIntervalEvents(t, r, al)
	if len(events) != 2 {
		t.Fatalf("retuned events = %d, want 2", len(events))
	}
	if events[0].Actor != "ops-alan" {
		t.Fatalf("first event Actor = %q, want ops-alan", events[0].Actor)
	}
	if got := events[0].Details["previous_history_interval_ms"]; got != float64(time.Hour/time.Millisecond) {
		t.Fatalf("first previous_history_interval_ms = %v, want %v", got, float64(time.Hour/time.Millisecond))
	}
	if got := events[0].Details["current_history_interval_ms"]; got != float64(30*time.Minute/time.Millisecond) {
		t.Fatalf("first current_history_interval_ms = %v, want %v", got, float64(30*time.Minute/time.Millisecond))
	}
	if events[1].Actor != "SetInterval" {
		t.Fatalf("second event Actor = %q, want SetInterval", events[1].Actor)
	}
	if got := events[1].Details["previous_history_interval_ms"]; got != float64(30*time.Minute/time.Millisecond) {
		t.Fatalf("second previous_history_interval_ms = %v, want %v", got, float64(30*time.Minute/time.Millisecond))
	}
	if got := events[1].Details["current_history_interval_ms"]; got != float64(2*time.Hour/time.Millisecond) {
		t.Fatalf("second current_history_interval_ms = %v, want %v", got, float64(2*time.Hour/time.Millisecond))
	}
}

func TestSetBalanceHistoryIntervalAuditPreviousCurrentExact(t *testing.T) {
	_, r, al, _, cancel := startRetuneTestBalanceHistoryRecorder(t, time.Hour)
	defer cancel()

	prev, err := r.SetInterval(30 * time.Minute)
	if err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	if prev != time.Hour {
		t.Fatalf("returned previous interval = %v, want 1h", prev)
	}
	events := retunedBalanceHistoryIntervalEvents(t, r, al)
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
	if got := ev.Details["previous_history_interval_ms"]; got != float64(time.Hour/time.Millisecond) {
		t.Errorf("previous_history_interval_ms = %v, want %v", got, float64(time.Hour/time.Millisecond))
	}
	if got := ev.Details["current_history_interval_ms"]; got != float64(30*time.Minute/time.Millisecond) {
		t.Errorf("current_history_interval_ms = %v, want %v", got, float64(30*time.Minute/time.Millisecond))
	}
	// Only the two numeric ms fields ride in the event: no balance
	// or account material, and no other keys at all.
	if len(ev.Details) != 2 {
		t.Errorf("details = %v, want exactly previous/current history interval ms", ev.Details)
	}
	for k, v := range ev.Details {
		lk := strings.ToLower(k)
		if strings.Contains(lk, "balance") || strings.Contains(lk, "account") || strings.Contains(lk, "token") || strings.Contains(lk, "secret") || strings.Contains(lk, "key") {
			t.Errorf("details key %q looks like balance or secret material", k)
		}
		// The values are millisecond integers, not balances.
		if _, ok := v.(float64); !ok {
			t.Errorf("details[%q] = %T, want a numeric millisecond value", k, v)
		}
	}
	// No balance material rides anywhere else in the event either.
	blob, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	for _, bad := range []string{"cash", "equity", "balance_cents", "500"} {
		if strings.Contains(string(blob), bad) {
			t.Errorf("audit event contains %q — balance material must never ride in a retune event", bad)
		}
	}
}

func TestSetBalanceHistoryIntervalNoRestartContinuousTicks(t *testing.T) {
	_, r, al, dir, cancel := startRetuneTestBalanceHistoryRecorder(t, 20*time.Millisecond)
	defer cancel()
	defer r.Stop()
	defer func() { _ = al.Close() }()

	// Let the recorder sample on the original interval first.
	waitFor(t, 3*time.Second, "first ticks", func() bool { return r.Ticks() >= 2 })
	ticksBefore := r.Ticks()
	rowsBefore := historyRows(t, dir)

	// Revise without restarting: the same goroutine keeps ticking
	// (ticks only grow) and the counters are continuous.
	if _, err := r.SetInterval(10 * time.Millisecond); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	waitFor(t, 3*time.Second, "ticks after the revision", func() bool {
		return r.Ticks() >= ticksBefore+2
	})
	if got := r.Ticks(); got < ticksBefore+2 {
		t.Fatalf("Ticks() = %d after the revision, want at least %d", got, ticksBefore+2)
	}
	if n := historyRows(t, dir); n < rowsBefore+4 {
		t.Fatalf("rows = %d after the revision, want at least %d (series continues, none lost)", n, rowsBefore+4)
	}
	if got := r.Interval(); got != 10*time.Millisecond {
		t.Fatalf("Interval() = %v after the revision, want 10ms", got)
	}
}

func TestSetBalanceHistoryIntervalGetterFollowsLiveValue(t *testing.T) {
	_, r, al, _, cancel := startRetuneTestBalanceHistoryRecorder(t, time.Hour)
	defer cancel()
	defer r.Stop()
	defer func() { _ = al.Close() }()

	if got := r.Interval(); got != time.Hour {
		t.Fatalf("Interval() = %v at start, want 1h", got)
	}
	if prev, err := r.SetInterval(30 * time.Minute); err != nil || prev != time.Hour {
		t.Fatalf("SetInterval(30m) = (%v, %v), want (1h, nil)", prev, err)
	}
	if got := r.Interval(); got != 30*time.Minute {
		t.Fatalf("Interval() = %v after tightening, want 30m", got)
	}
	// A rejected revision leaves the getter in place.
	if _, err := r.SetInterval(0); !errors.Is(err, ErrInvalidBalanceHistoryInterval) {
		t.Fatalf("SetInterval(0): err = %v, want ErrInvalidBalanceHistoryInterval", err)
	}
	if got := r.Interval(); got != 30*time.Minute {
		t.Fatalf("Interval() = %v after rejected revision, want still 30m", got)
	}
}
