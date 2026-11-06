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

// Balance-history retention window runtime revision tests (LG-331).
// They reuse mustAuditLog from ledger_audit_test.go,
// postForHistory from ledger_balance_history_test.go, historyRows
// and startRetuneTestBalanceHistoryRecorder from
// balance_history_interval_test.go, and waitFor from
// ledger_hold_sweep_test.go.

// startMaxAgeTestRecorder starts a balance-history recorder on l
// with the given cadence and retention window, so tests can pick a
// window other than the 90-day default startRetuneTestBalanceHistoryRecorder
// uses.
func startMaxAgeTestRecorder(t *testing.T, l *Ledger, interval, maxAge time.Duration) (*BalanceHistoryRecorder, string, context.CancelFunc) {
	t.Helper()
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	r := StartBalanceHistoryRecorder(ctx, l, BalanceHistoryConfig{
		Dir:      dir,
		Interval: interval,
		MaxAgeMs: int64(maxAge / time.Millisecond),
	}, nil)
	if r == nil {
		t.Fatal("StartBalanceHistoryRecorder returned nil for a valid config")
	}
	return r, dir, cancel
}

// retunedBalanceHistoryMaxAgeEvents stops the recorder, closes the
// audit log (which drains the async writer), and returns every
// balance_history_max_age_retuned event recorded in it. The ledger
// must not be used after this call.
func retunedBalanceHistoryMaxAgeEvents(t *testing.T, r *BalanceHistoryRecorder, al *AuditLog) []AuditEvent {
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
		if ev.Op == "balance_history_max_age_retuned" {
			out = append(out, ev)
		}
	}
	return out
}

// writeStaleHistoryFile drops a recorder-named day-file into dir with
// its mtime age old, so the next tick's pruning can (or must not)
// remove it depending on the live window.
func writeStaleHistoryFile(t *testing.T, dir, name string, age time.Duration) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("{\"account\":\"cash\",\"currency\":\"USD\",\"balance_cents\":1,\"version\":1,\"at\":\"2020-01-01T00:00:00Z\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-age)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSetBalanceHistoryMaxAgeInvalidRejectedAndKeepsLive(t *testing.T) {
	_, r, al, _, cancel := startRetuneTestBalanceHistoryRecorder(t, time.Hour)
	defer cancel()

	for _, d := range []time.Duration{0, -time.Hour, -time.Nanosecond} {
		prev, err := r.SetMaxAge(d)
		if !errors.Is(err, ErrInvalidBalanceHistoryMaxAge) {
			t.Fatalf("SetMaxAge(%v): err = %v, want ErrInvalidBalanceHistoryMaxAge", d, err)
		}
		if prev != 90*24*time.Hour {
			t.Fatalf("SetMaxAge(%v) returned %v, want the live 90d", d, prev)
		}
		if got := r.MaxAge(); got != 90*24*time.Hour {
			t.Fatalf("MaxAge() = %v after rejected %v, want still 90d", got, d)
		}
	}
	// The Ctx variant rejects the same way.
	if prev, err := r.SetMaxAgeCtx(context.Background(), 0); !errors.Is(err, ErrInvalidBalanceHistoryMaxAge) || prev != 90*24*time.Hour {
		t.Fatalf("SetMaxAgeCtx(0) = (%v, %v), want (90d, ErrInvalidBalanceHistoryMaxAge)", prev, err)
	}
	// Zero is rejected here, unlike the startup-time normalize
	// rule: zero would make the next tick's pruning delete the
	// whole history, and a runtime revision never silently
	// normalizes.
	if events := retunedBalanceHistoryMaxAgeEvents(t, r, al); len(events) != 0 {
		t.Fatalf("retuned events = %d, want 0 after only rejected revisions", len(events))
	}
}

func TestSetBalanceHistoryMaxAgeSameValueNoOpNoAudit(t *testing.T) {
	_, r, al, _, cancel := startRetuneTestBalanceHistoryRecorder(t, time.Hour)
	defer cancel()

	prev, err := r.SetMaxAge(90 * 24 * time.Hour)
	if err != nil {
		t.Fatalf("same-value SetMaxAge: %v", err)
	}
	if prev != 90*24*time.Hour {
		t.Fatalf("same-value revision returned %v, want the live 90d", prev)
	}
	// One real revision, then the same value again: only the real
	// one audits.
	if _, err := r.SetMaxAge(30 * 24 * time.Hour); err != nil {
		t.Fatalf("SetMaxAge(30d): %v", err)
	}
	if prev, err := r.SetMaxAge(30 * 24 * time.Hour); err != nil || prev != 30*24*time.Hour {
		t.Fatalf("same-value SetMaxAge(30d) = (%v, %v), want (30d, nil)", prev, err)
	}
	if events := retunedBalanceHistoryMaxAgeEvents(t, r, al); len(events) != 1 {
		t.Fatalf("retuned events = %d, want 1 after the real revision plus the no-ops", len(events))
	}
}

func TestSetBalanceHistoryMaxAgeReturnsPreviousValue(t *testing.T) {
	_, r, al, _, cancel := startRetuneTestBalanceHistoryRecorder(t, time.Hour)
	defer cancel()
	defer r.Stop()
	defer func() { _ = al.Close() }()

	prev, err := r.SetMaxAge(30 * 24 * time.Hour)
	if err != nil {
		t.Fatalf("SetMaxAge: %v", err)
	}
	if prev != 90*24*time.Hour {
		t.Fatalf("returned previous window = %v, want 90d", prev)
	}
	if got := r.MaxAge(); got != 30*24*time.Hour {
		t.Fatalf("MaxAge() = %v after the revision, want 30d", got)
	}
}

func TestSetBalanceHistoryMaxAgeShortenPrunesNextTick(t *testing.T) {
	_, r, al, dir, cancel := startRetuneTestBalanceHistoryRecorder(t, 20*time.Millisecond)
	defer cancel()
	defer r.Stop()
	defer func() { _ = al.Close() }()

	stale := writeStaleHistoryFile(t, dir, "balance-history-2020-01-01.jsonl", 48*time.Hour)

	// The first tick lands on the original 90d window: the stale
	// file survives it.
	waitFor(t, 3*time.Second, "first tick", func() bool { return r.Ticks() >= 1 })
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("stale file missing after the first tick (err = %v), want kept under the 90d window", err)
	}

	// Shorten to 1h: the next tick's regular pruning removes it.
	if _, err := r.SetMaxAge(time.Hour); err != nil {
		t.Fatalf("SetMaxAge(1h): %v", err)
	}
	waitFor(t, 3*time.Second, "stale file pruned", func() bool {
		_, err := os.Stat(stale)
		return os.IsNotExist(err)
	})
	// The fresh day-file survives: only files past the new window
	// are pruned.
	if n := historyRows(t, dir); n < 2 {
		t.Fatalf("rows = %d after the shortening, want at least 2 (the fresh day-file must survive)", n)
	}
}

func TestSetBalanceHistoryMaxAgeLengthenKeepsNextTick(t *testing.T) {
	al := mustAuditLog(t)
	defer func() { _ = al.Close() }()
	l := New(WithAuditLog(al))
	postForHistory(t, l, "bhm331-e1", "cash", "equity", 500, "USD", "bhm331-k1")

	// Control: a 1h window prunes the 48h-old file on its tick.
	control, controlDir, controlCancel := startMaxAgeTestRecorder(t, l, time.Hour, time.Hour)
	defer controlCancel()
	defer control.Stop()
	controlStale := writeStaleHistoryFile(t, controlDir, "balance-history-2020-01-01.jsonl", 48*time.Hour)
	control.tick()
	if _, err := os.Stat(controlStale); !os.IsNotExist(err) {
		t.Fatalf("control stale file still present after the tick (err = %v), want pruned by the 1h window", err)
	}

	// Lengthen first, then tick: the same-age file survives, even
	// though the old 1h window would have pruned it.
	r, dir, cancel := startMaxAgeTestRecorder(t, l, time.Hour, time.Hour)
	defer cancel()
	defer r.Stop()
	stale := writeStaleHistoryFile(t, dir, "balance-history-2020-01-01.jsonl", 48*time.Hour)
	if _, err := r.SetMaxAge(90 * 24 * time.Hour); err != nil {
		t.Fatalf("SetMaxAge(90d): %v", err)
	}
	r.tick()
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("stale file missing after the lengthened tick (err = %v), want kept under the 90d window", err)
	}
	if n := historyRows(t, dir); n < 2 {
		t.Fatalf("rows = %d after the lengthened tick, want at least 2 (the tick still sampled)", n)
	}
}

func TestSetBalanceHistoryMaxAgeRevisionDeletesNoFiles(t *testing.T) {
	_, r, al, dir, cancel := startRetuneTestBalanceHistoryRecorder(t, time.Hour)
	defer cancel()
	defer r.Stop()
	defer func() { _ = al.Close() }()

	stale := writeStaleHistoryFile(t, dir, "balance-history-2020-01-01.jsonl", 48*time.Hour)
	foreign := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(foreign, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Shorten drastically — but no tick fires on the 1h cadence, so
	// the directory must be byte-for-byte unchanged: the revision
	// itself never sweeps.
	if _, err := r.SetMaxAge(time.Minute); err != nil {
		t.Fatalf("SetMaxAge(1m): %v", err)
	}
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("stale file missing after the revision with no tick (err = %v), want untouched", err)
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
		t.Fatalf("files = %v after the revision, want exactly [stale day-file, notes.txt]", names)
	}
}

func TestSetBalanceHistoryMaxAgeInFlightSamplingNotInterrupted(t *testing.T) {
	al := mustAuditLog(t)
	defer func() { _ = al.Close() }()
	l := New(WithAuditLog(al))
	postForHistory(t, l, "bhm331-e1", "cash", "equity", 500, "USD", "bhm331-k1")
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

	// Revise the window while that sample is still in flight: the
	// revision must not cut it short, and it never calls tick()
	// while holding r.mu, so it cannot stall behind it either.
	if _, err := r.SetMaxAge(30 * 24 * time.Hour); err != nil {
		t.Fatalf("SetMaxAge during an in-flight sample: %v", err)
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
	// The worker keeps ticking on the unchanged cadence afterwards:
	// the window revision never restarts it (ticks only grow).
	waitFor(t, 3*time.Second, "ticks after the revision", func() bool { return r.Ticks() >= 3 })
	if got := r.MaxAge(); got != 30*24*time.Hour {
		t.Fatalf("MaxAge() = %v after the revision, want 30d", got)
	}
}

func TestSetBalanceHistoryMaxAgeConcurrentRevisionAndTicksRaceFree(t *testing.T) {
	_, r, al, dir, cancel := startRetuneTestBalanceHistoryRecorder(t, 10*time.Millisecond)
	defer cancel()
	defer r.Stop()
	defer func() { _ = al.Close() }()

	var wg sync.WaitGroup
	// Concurrent revisions from several goroutines while the worker
	// ticks underneath, plus direct tick() calls from another
	// goroutine: -race must stay silent, and the worker must keep
	// sampling on some positive window.
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				d := time.Duration(24*(1+g*10+i%5)) * time.Hour
				_, _ = r.SetMaxAge(d)
				_ = r.MaxAge()
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
	if got := r.MaxAge(); got <= 0 {
		t.Fatalf("MaxAge() = %v after concurrent revisions, want a positive live value", got)
	}
	waitFor(t, 3*time.Second, "a tick after the concurrent revisions", func() bool {
		return r.Ticks() >= 1
	})
	if n := historyRows(t, dir); n < 2 {
		t.Fatalf("rows = %d after the concurrent revisions, want at least 2 (one tick landed)", n)
	}
}

func TestSetBalanceHistoryMaxAgeCtxActorAttribution(t *testing.T) {
	_, r, al, _, cancel := startRetuneTestBalanceHistoryRecorder(t, time.Hour)
	defer cancel()

	ctx := ContextWithActor(context.Background(), "ops-alan")
	if _, err := r.SetMaxAgeCtx(ctx, 30*24*time.Hour); err != nil {
		t.Fatalf("SetMaxAgeCtx: %v", err)
	}
	// A plain call keeps the entrypoint name as the actor.
	if _, err := r.SetMaxAge(60 * 24 * time.Hour); err != nil {
		t.Fatalf("SetMaxAge: %v", err)
	}
	events := retunedBalanceHistoryMaxAgeEvents(t, r, al)
	if len(events) != 2 {
		t.Fatalf("retuned events = %d, want 2", len(events))
	}
	if events[0].Actor != "ops-alan" {
		t.Fatalf("first event Actor = %q, want ops-alan", events[0].Actor)
	}
	if got := events[0].Details["previous_max_age_ms"]; got != float64(90*24*time.Hour/time.Millisecond) {
		t.Fatalf("first previous_max_age_ms = %v, want %v", got, float64(90*24*time.Hour/time.Millisecond))
	}
	if got := events[0].Details["current_max_age_ms"]; got != float64(30*24*time.Hour/time.Millisecond) {
		t.Fatalf("first current_max_age_ms = %v, want %v", got, float64(30*24*time.Hour/time.Millisecond))
	}
	if events[1].Actor != "SetMaxAge" {
		t.Fatalf("second event Actor = %q, want SetMaxAge", events[1].Actor)
	}
	if got := events[1].Details["previous_max_age_ms"]; got != float64(30*24*time.Hour/time.Millisecond) {
		t.Fatalf("second previous_max_age_ms = %v, want %v", got, float64(30*24*time.Hour/time.Millisecond))
	}
	if got := events[1].Details["current_max_age_ms"]; got != float64(60*24*time.Hour/time.Millisecond) {
		t.Fatalf("second current_max_age_ms = %v, want %v", got, float64(60*24*time.Hour/time.Millisecond))
	}
}

func TestSetBalanceHistoryMaxAgeAuditPreviousCurrentExact(t *testing.T) {
	_, r, al, _, cancel := startRetuneTestBalanceHistoryRecorder(t, time.Hour)
	defer cancel()

	prev, err := r.SetMaxAge(30 * 24 * time.Hour)
	if err != nil {
		t.Fatalf("SetMaxAge: %v", err)
	}
	if prev != 90*24*time.Hour {
		t.Fatalf("returned previous window = %v, want 90d", prev)
	}
	events := retunedBalanceHistoryMaxAgeEvents(t, r, al)
	if len(events) != 1 {
		t.Fatalf("retuned events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Actor != "SetMaxAge" {
		t.Errorf("Actor = %q, want SetMaxAge", ev.Actor)
	}
	if ev.VersionBefore != ev.VersionAfter {
		t.Errorf("version moved %d -> %d, want no bump", ev.VersionBefore, ev.VersionAfter)
	}
	if got := ev.Details["previous_max_age_ms"]; got != float64(90*24*time.Hour/time.Millisecond) {
		t.Errorf("previous_max_age_ms = %v, want %v", got, float64(90*24*time.Hour/time.Millisecond))
	}
	if got := ev.Details["current_max_age_ms"]; got != float64(30*24*time.Hour/time.Millisecond) {
		t.Errorf("current_max_age_ms = %v, want %v", got, float64(30*24*time.Hour/time.Millisecond))
	}
	// Only the two numeric ms fields ride in the event: no balance
	// or account material, and no other keys at all.
	if len(ev.Details) != 2 {
		t.Errorf("details = %v, want exactly previous/current max age ms", ev.Details)
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

func TestSetBalanceHistoryMaxAgeNoRestartContinuousTicks(t *testing.T) {
	_, r, al, dir, cancel := startRetuneTestBalanceHistoryRecorder(t, 20*time.Millisecond)
	defer cancel()
	defer r.Stop()
	defer func() { _ = al.Close() }()

	// Let the recorder sample on the original window first.
	waitFor(t, 3*time.Second, "first ticks", func() bool { return r.Ticks() >= 2 })
	ticksBefore := r.Ticks()
	rowsBefore := historyRows(t, dir)

	// Revise without restarting: the same goroutine keeps ticking
	// (ticks only grow) and the counters are continuous.
	if _, err := r.SetMaxAge(30 * 24 * time.Hour); err != nil {
		t.Fatalf("SetMaxAge: %v", err)
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
	if got := r.MaxAge(); got != 30*24*time.Hour {
		t.Fatalf("MaxAge() = %v after the revision, want 30d", got)
	}
}

func TestSetBalanceHistoryMaxAgeGetterFollowsLiveValue(t *testing.T) {
	_, r, al, _, cancel := startRetuneTestBalanceHistoryRecorder(t, time.Hour)
	defer cancel()
	defer r.Stop()
	defer func() { _ = al.Close() }()

	if got := r.MaxAge(); got != 90*24*time.Hour {
		t.Fatalf("MaxAge() = %v at start, want 90d", got)
	}
	if prev, err := r.SetMaxAge(30 * 24 * time.Hour); err != nil || prev != 90*24*time.Hour {
		t.Fatalf("SetMaxAge(30d) = (%v, %v), want (90d, nil)", prev, err)
	}
	if got := r.MaxAge(); got != 30*24*time.Hour {
		t.Fatalf("MaxAge() = %v after shortening, want 30d", got)
	}
	// A rejected revision leaves the getter in place.
	if _, err := r.SetMaxAge(0); !errors.Is(err, ErrInvalidBalanceHistoryMaxAge) {
		t.Fatalf("SetMaxAge(0): err = %v, want ErrInvalidBalanceHistoryMaxAge", err)
	}
	if got := r.MaxAge(); got != 30*24*time.Hour {
		t.Fatalf("MaxAge() = %v after rejected revision, want still 30d", got)
	}
}
