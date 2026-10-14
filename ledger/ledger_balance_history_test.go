package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// postForHistory posts one entry for tests, failing fast on error.
func postForHistory(t *testing.T, l *Ledger, id, debit, credit string, cents int64, currency, key string) {
	t.Helper()
	e := JournalEntry{
		ID:             id,
		DebitAccount:   AccountID(debit),
		CreditAccount:  AccountID(credit),
		AmountCents:    cents,
		Currency:       currency,
		IdempotencyKey: key,
		CreatedAt:      time.Now().UTC(),
	}
	if _, dup, err := l.Post(e); err != nil || dup {
		t.Fatalf("Post %s = dup=%v err=%v", id, dup, err)
	}
}

func TestSnapshotBalances(t *testing.T) {
	l := New()
	postForHistory(t, l, "bh-e1", "cash", "equity", 12500, "USD", "bh-k1")
	postForHistory(t, l, "bh-e2", "cash", "equity", 300, "EUR", "bh-k2")
	postForHistory(t, l, "bh-e3", "ar", "revenue", 7000, "USD", "bh-k3")

	points := l.SnapshotBalances()
	// (account, currency) keys touched: cash/USD, cash/EUR, equity/USD,
	// equity/EUR, ar/USD, revenue/USD.
	if len(points) != 6 {
		t.Fatalf("len(points) = %d, want 6", len(points))
	}
	// Deterministic ordering by (account, currency).
	for i := 1; i < len(points); i++ {
		a, b := points[i-1], points[i]
		if a.Account > b.Account || (a.Account == b.Account && a.Currency > b.Currency) {
			t.Fatalf("points not sorted at %d: %+v > %+v", i, a, b)
		}
	}
	byKey := map[string]BalanceHistoryPoint{}
	for _, p := range points {
		byKey[string(p.Account)+"/"+p.Currency] = p
	}
	if got := byKey["cash/USD"].BalanceCents; got != 12500 {
		t.Errorf("cash/USD = %d, want 12500", got)
	}
	if got := byKey["cash/EUR"].BalanceCents; got != 300 {
		t.Errorf("cash/EUR = %d, want 300", got)
	}
	if got := byKey["equity/USD"].BalanceCents; got != -12500 {
		t.Errorf("equity/USD = %d, want -12500", got)
	}
	for _, p := range points {
		if p.Version != 3 {
			t.Errorf("point %+v: Version = %d, want 3", p, p.Version)
		}
		if p.At.IsZero() {
			t.Errorf("point %+v: At is zero", p)
		}
	}
	// Read-only: sampling must not bump the version.
	if v := l.SnapshotBalances()[0].Version; v != 3 {
		t.Errorf("resample Version = %d, want 3 (no bump)", v)
	}
}

func TestBalanceHistoryWriteAndQuery(t *testing.T) {
	dir := t.TempDir()
	day1 := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

	write := func(pts []BalanceHistoryPoint) {
		t.Helper()
		if err := WriteBalanceHistory(dir, pts); err != nil {
			t.Fatalf("WriteBalanceHistory: %v", err)
		}
	}
	write([]BalanceHistoryPoint{
		{Account: "cash", Currency: "USD", BalanceCents: 100, Version: 1, At: day1},
		{Account: "cash", Currency: "EUR", BalanceCents: 50, Version: 1, At: day1},
		{Account: "ar", Currency: "USD", BalanceCents: 70, Version: 1, At: day1},
	})
	write([]BalanceHistoryPoint{
		{Account: "cash", Currency: "USD", BalanceCents: 200, Version: 2, At: day2},
		{Account: "cash", Currency: "USD", BalanceCents: 250, Version: 3, At: day2.Add(time.Hour)},
	})

	// Verify the on-disk shape: one file per day, one JSON object per
	// line, append-only.
	for _, name := range []string{"balance-history-2026-10-09.jsonl", "balance-history-2026-10-10.jsonl"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, line := range bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n")) {
			var m map[string]any
			if err := json.Unmarshal(line, &m); err != nil {
				t.Fatalf("%s: line not JSON: %v", name, err)
			}
		}
	}

	// Full query for one (account, currency).
	got, err := QueryBalanceHistory(dir, "cash", "USD", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("QueryBalanceHistory: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if got[0].BalanceCents != 100 || got[1].BalanceCents != 200 || got[2].BalanceCents != 250 {
		t.Errorf("unexpected series: %+v", got)
	}
	// Ordered by (At, Version).
	for i := 1; i < len(got); i++ {
		if got[i].At.Before(got[i-1].At) {
			t.Errorf("out of order at %d", i)
		}
	}

	// since/until window: only the day-2 morning tick qualifies.
	since := day2
	until := day2.Add(30 * time.Minute)
	got, err = QueryBalanceHistory(dir, "cash", "USD", since, until)
	if err != nil {
		t.Fatalf("window query: %v", err)
	}
	if len(got) != 1 || got[0].BalanceCents != 200 {
		t.Errorf("window query = %+v, want the 200-cent tick", got)
	}

	// Currency isolation.
	got, err = QueryBalanceHistory(dir, "cash", "EUR", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("currency query: %v", err)
	}
	if len(got) != 1 || got[0].BalanceCents != 50 {
		t.Errorf("EUR query = %+v, want one 50-cent point", got)
	}

	// Account isolation.
	got, err = QueryBalanceHistory(dir, "ar", "USD", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("account query: %v", err)
	}
	if len(got) != 1 || got[0].BalanceCents != 70 {
		t.Errorf("ar query = %+v, want one 70-cent point", got)
	}

	// Unknown account: empty, not an error.
	got, err = QueryBalanceHistory(dir, "nope", "USD", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("unknown account query: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("unknown account query = %+v, want empty", got)
	}

	// Validation.
	if _, err := QueryBalanceHistory("", "cash", "USD", time.Time{}, time.Time{}); err != ErrBalanceHistoryNotConfigured {
		t.Errorf("empty dir err = %v, want ErrBalanceHistoryNotConfigured", err)
	}
	if _, err := QueryBalanceHistory(dir, "", "USD", time.Time{}, time.Time{}); err == nil {
		t.Error("empty account: want error, got nil")
	}
	if _, err := QueryBalanceHistory(dir, "cash", "usd!", time.Time{}, time.Time{}); err == nil {
		t.Error("bad currency: want error, got nil")
	}
}

func TestBalanceHistoryQueryToleratesTornTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "balance-history-2026-10-10.jsonl")
	good := `{"account":"cash","currency":"USD","balance_cents":100,"version":1,"at":"2026-10-10T12:00:00Z"}` + "\n"
	// Simulate a tick racing the read: the last line has no trailing
	// newline and is truncated mid-object.
	torn := `{"account":"cash","currency":"USD","balance_cents":2`
	if err := os.WriteFile(path, []byte(good+torn), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := QueryBalanceHistory(dir, "cash", "USD", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("torn tail: %v", err)
	}
	if len(got) != 1 || got[0].BalanceCents != 100 {
		t.Errorf("torn tail = %+v, want the one good point", got)
	}
}

func TestBalanceHistoryQueryRejectsCorruptMiddleLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "balance-history-2026-10-10.jsonl")
	good := `{"account":"cash","currency":"USD","balance_cents":100,"version":1,"at":"2026-10-10T12:00:00Z"}` + "\n"
	if err := os.WriteFile(path, []byte(good+"not json at all\n"+good), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := QueryBalanceHistory(dir, "cash", "USD", time.Time{}, time.Time{}); err == nil {
		t.Error("corrupt middle line: want error, got nil")
	}
}

func TestBalanceHistoryPrune(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "balance-history-2026-01-01.jsonl")
	fresh := filepath.Join(dir, "balance-history-2026-10-10.jsonl")
	foreign := filepath.Join(dir, "other-2026-01-01.jsonl")
	for _, p := range []string{old, fresh, foreign} {
		if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Age the files: old is ancient, fresh is now.
	ancient := time.Now().Add(-200 * 24 * time.Hour)
	if err := os.Chtimes(old, ancient, ancient); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(foreign, ancient, ancient); err != nil {
		t.Fatal(err)
	}

	removed, err := PruneBalanceHistory(dir, 90*24*time.Hour)
	if err != nil {
		t.Fatalf("PruneBalanceHistory: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("old history file not pruned")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("fresh history file wrongly pruned")
	}
	// Foreign files are never ours to delete, however old.
	if _, err := os.Stat(foreign); err != nil {
		t.Error("foreign file wrongly pruned")
	}
	// A file matching the glob but not the date format is left alone even
	// with a zero retention window.
	nearMiss := filepath.Join(dir, "balance-history-notes.jsonl")
	if err := os.WriteFile(nearMiss, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ancient2 := time.Now().Add(-300 * 24 * time.Hour)
	if err := os.Chtimes(nearMiss, ancient2, ancient2); err != nil {
		t.Fatal(err)
	}
	if _, err := PruneBalanceHistory(dir, 0); err != nil {
		t.Fatalf("zero-age prune: %v", err)
	}
	if _, err := os.Stat(nearMiss); err != nil {
		t.Error("non-date history file wrongly pruned")
	}
}

func TestBalanceHistoryPruneMissingDir(t *testing.T) {
	removed, err := PruneBalanceHistory(filepath.Join(t.TempDir(), "nope"), time.Hour)
	if err != nil {
		t.Fatalf("missing dir: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0", removed)
	}
}

func TestBalanceHistoryRecorderDisabled(t *testing.T) {
	l := New()
	ctx := context.Background()
	if r := StartBalanceHistoryRecorder(ctx, l, BalanceHistoryConfig{}, nil); r != nil {
		t.Error("empty config: want nil recorder")
	}
	if r := StartBalanceHistoryRecorder(ctx, l, BalanceHistoryConfig{Dir: t.TempDir()}, nil); r != nil {
		t.Error("zero interval: want nil recorder")
	}
	if r := StartBalanceHistoryRecorder(ctx, l, BalanceHistoryConfig{Dir: t.TempDir(), Interval: -time.Second}, nil); r != nil {
		t.Error("negative interval: want nil recorder")
	}
}

func TestBalanceHistoryRecorderTicks(t *testing.T) {
	l := New()
	postForHistory(t, l, "bhr-e1", "cash", "equity", 500, "USD", "bhr-k1")
	dir := t.TempDir()

	var mu sync.Mutex
	var tickErrs []error
	var tickPoints []int
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := StartBalanceHistoryRecorder(ctx, l, BalanceHistoryConfig{
		Dir:      dir,
		Interval: 20 * time.Millisecond,
		MaxAgeMs: int64(24 * time.Hour / time.Millisecond),
	}, func(points int, err error) {
		mu.Lock()
		defer mu.Unlock()
		tickPoints = append(tickPoints, points)
		tickErrs = append(tickErrs, err)
	})
	if r == nil {
		t.Fatal("recorder is nil")
	}
	defer r.Stop()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && r.Ticks() < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	if r.Ticks() < 2 {
		t.Fatalf("Ticks() = %d, want >= 2", r.Ticks())
	}
	// Stop the recorder before asserting: no more ticks can fire, so
	// tickPoints, tickErrs, and the on-disk rows are stable and mutually
	// consistent (every tick that wrote a row also ran onTick before the
	// recorder goroutine exited). Stop must run while mu is free — a tick
	// in flight blocks on mu inside onTick, and Stop waits for the
	// recorder goroutine to exit, so taking mu first would deadlock the
	// suite.
	r.Stop()

	mu.Lock()
	defer mu.Unlock()
	for i, err := range tickErrs {
		if err != nil {
			t.Fatalf("tick %d err = %v", i, err)
		}
	}
	// cash/USD + equity/USD = 2 points per tick.
	for i, n := range tickPoints {
		if n != 2 {
			t.Errorf("tick %d wrote %d points, want 2", i, n)
		}
	}
	if r.PointsTotal() != uint64(2*len(tickPoints)) {
		t.Errorf("PointsTotal() = %d, want %d", r.PointsTotal(), 2*len(tickPoints))
	}

	// The day file exists and holds the appended rows.
	got, err := QueryBalanceHistory(dir, "cash", "USD", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("query after ticks: %v", err)
	}
	if len(got) != len(tickPoints) {
		t.Errorf("series len = %d, want %d (one row per tick)", len(got), len(tickPoints))
	}

	// Stop is idempotent and the goroutine exits on cancel.
	r.Stop()
	cancel()
}

func TestBalanceHistoryRecorderFailedTickSurvives(t *testing.T) {
	l := New()
	postForHistory(t, l, "bhf-e1", "cash", "equity", 500, "USD", "bhf-k1")
	// A path that exists as a regular file: MkdirAll fails, every tick
	// errors, the worker must keep ticking anyway.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var errs atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := StartBalanceHistoryRecorder(ctx, l, BalanceHistoryConfig{
		Dir:      blocker,
		Interval: 20 * time.Millisecond,
	}, func(_ int, err error) {
		if err != nil {
			errs.Add(1)
		}
	})
	if r == nil {
		t.Fatal("recorder is nil")
	}
	defer r.Stop()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && errs.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if errs.Load() == 0 {
		t.Fatal("want at least one failed tick, got none")
	}
	if r.Ticks() != 0 {
		t.Errorf("Ticks() = %d, want 0 (failed ticks do not count)", r.Ticks())
	}
}

func TestBalanceHistorySurvivesSnapshotRestore(t *testing.T) {
	l := New()
	postForHistory(t, l, "bhs-e1", "cash", "equity", 1000, "USD", "bhs-k1")
	dir := t.TempDir()

	// Record pre-restore history straight from the original ledger.
	if err := WriteBalanceHistory(dir, l.SnapshotBalances()); err != nil {
		t.Fatalf("pre-restore write: %v", err)
	}
	pre, err := QueryBalanceHistory(dir, "cash", "USD", time.Time{}, time.Time{})
	if err != nil || len(pre) == 0 {
		t.Fatalf("pre-restore query: %v (%d points)", err, len(pre))
	}

	// Disaster-recovery round trip.
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(&buf)
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	postForHistory(t, restored, "bhs-e2", "cash", "equity", 250, "USD", "bhs-k2")

	// The restored ledger's worker appends to the SAME directory: old
	// rows stay, new rows follow — the series continues, it is not reset.
	if err := WriteBalanceHistory(dir, restored.SnapshotBalances()); err != nil {
		t.Fatalf("post-restore write: %v", err)
	}
	got, err := QueryBalanceHistory(dir, "cash", "USD", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("post-restore query: %v", err)
	}
	if len(got) != len(pre)+1 {
		t.Fatalf("series len = %d, want %d (pre-restore rows + 1 new)", len(got), len(pre)+1)
	}
	// First rows are the pre-restore samples (1000), last are post (1250).
	if got[0].BalanceCents != 1000 {
		t.Errorf("first row = %d, want 1000 (pre-restore preserved)", got[0].BalanceCents)
	}
	last := got[len(got)-1]
	if last.BalanceCents != 1250 {
		t.Errorf("last row = %d, want 1250 (post-restore appended)", last.BalanceCents)
	}
	// The journal version continues across the restore, and the series
	// records it.
	if last.Version != 2 {
		t.Errorf("last row version = %d, want 2", last.Version)
	}
}

func TestBalanceHistoryConcurrent(t *testing.T) {
	l := New()
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var onTickErrs atomic.Int64
	r := StartBalanceHistoryRecorder(ctx, l, BalanceHistoryConfig{
		Dir:      dir,
		Interval: 10 * time.Millisecond,
	}, func(_ int, err error) {
		if err != nil {
			onTickErrs.Add(1)
		}
	})
	if r == nil {
		t.Fatal("recorder is nil")
	}
	defer r.Stop()

	var wg sync.WaitGroup
	errs := make(chan error, 256)
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				e := JournalEntry{
					ID:             fmt.Sprintf("bhc-%d-%02d", w, i),
					DebitAccount:   "cash",
					CreditAccount:  "equity",
					AmountCents:    100,
					Currency:       "USD",
					IdempotencyKey: fmt.Sprintf("bhc-k-%d-%02d", w, i),
					CreatedAt:      time.Now().UTC(),
				}
				if _, dup, err := l.Post(e); err != nil || dup {
					errs <- fmt.Errorf("post %s: dup=%v err=%w", e.ID, dup, err)
				}
			}
		}(w)
	}
	for q := 0; q < 4; q++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				_, _ = QueryBalanceHistory(dir, "cash", "USD", time.Time{}, time.Time{})
				time.Sleep(5 * time.Millisecond)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent post: %v", err)
	}
	if onTickErrs.Load() != 0 {
		t.Errorf("tick errors = %d, want 0", onTickErrs.Load())
	}
	if r.Ticks() == 0 {
		t.Error("want at least one tick during concurrent load")
	}
}

// The day-file naming is the query's file-selection key: a name that is
// not ours must never be parsed as a day.
func TestBalanceHistoryDayParsing(t *testing.T) {
	for _, name := range []string{
		"balance-history-2026-10-10.jsonl",
		"balance-history-2026-01-01.jsonl",
	} {
		day, ok := balanceHistoryDay(name)
		if !ok {
			t.Errorf("%s: not parsed", name)
		} else if got := day.Format("2006-01-02"); !strings.HasPrefix(name, "balance-history-"+got) {
			t.Errorf("%s: parsed as %s", name, got)
		}
	}
	for _, name := range []string{
		"balance-history-.jsonl",
		"balance-history-2026-13-99.jsonl",
		"balance-history-2026-10-10.jsonl.gz",
		"audit-2026-10-10.jsonl",
		"balance-history-x",
		"",
	} {
		if _, ok := balanceHistoryDay(name); ok {
			t.Errorf("%s: parsed as ours, want rejection", name)
		}
	}
}
