package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"sync"
	"testing"
	"time"
)

func testSchedule(now time.Time) TransferSchedule {
	return TransferSchedule{
		ID:          "sched-1",
		From:        "alice",
		To:          "bob",
		AmountCents: 1000,
		Currency:    "USD",
		Interval:    time.Hour,
		NextRunAt:   now.Add(-time.Minute),
	}
}

func TestCreateTransferScheduleValidation(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	cases := []struct {
		name   string
		mutate func(*TransferSchedule)
		want   error
	}{
		{"empty id", func(s *TransferSchedule) { s.ID = "" }, ErrScheduleEmptyID},
		{"empty from", func(s *TransferSchedule) { s.From = "" }, ErrScheduleEmptyAccount},
		{"same account", func(s *TransferSchedule) { s.To = s.From }, ErrScheduleSameAccount},
		{"zero amount", func(s *TransferSchedule) { s.AmountCents = 0 }, ErrScheduleNonPositiveAmount},
		{"negative amount", func(s *TransferSchedule) { s.AmountCents = -5 }, ErrScheduleNonPositiveAmount},
		{"bad currency", func(s *TransferSchedule) { s.Currency = "US" }, ErrInvalidCurrency},
		{"to_currency equals currency", func(s *TransferSchedule) { s.ToCurrency = "USD" }, ErrCrossCurrencyTransfer},
		{"negative fee", func(s *TransferSchedule) { s.FeeCents = -1 }, ErrScheduleBadFee},
		{"fee without account", func(s *TransferSchedule) { s.FeeCents = 10 }, ErrScheduleBadFee},
		{"zero interval", func(s *TransferSchedule) { s.Interval = 0 }, ErrScheduleBadInterval},
		{"negative interval", func(s *TransferSchedule) { s.Interval = -time.Hour }, ErrScheduleBadInterval},
		{"zero next_run_at", func(s *TransferSchedule) { s.NextRunAt = time.Time{} }, ErrScheduleBadNextRunAt},
		{"ends_at before next_run_at", func(s *TransferSchedule) { s.EndsAt = s.NextRunAt.Add(-time.Hour) }, ErrScheduleBadEndsAt},
		{"ends_at equals next_run_at", func(s *TransferSchedule) { s.EndsAt = s.NextRunAt }, ErrScheduleBadEndsAt},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := New()
			s := testSchedule(now)
			tc.mutate(&s)
			if _, _, err := l.CreateTransferSchedule(s); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestCreateTransferScheduleConflict(t *testing.T) {
	l := New()
	now := time.Now().UTC()
	s := testSchedule(now)
	if _, _, err := l.CreateTransferSchedule(s); err != nil {
		t.Fatalf("create: %v", err)
	}
	s2 := testSchedule(now)
	s2.AmountCents = 999
	if _, _, err := l.CreateTransferSchedule(s2); !errors.Is(err, ErrScheduleIDConflict) {
		t.Errorf("duplicate id err = %v, want ErrScheduleIDConflict", err)
	}
}

func TestCreateTransferScheduleIdempotent(t *testing.T) {
	l := New()
	now := time.Now().UTC()
	s := testSchedule(now)
	s.IdempotencyKey = "key-1"
	got1, dup1, err := l.CreateTransferSchedule(s)
	if err != nil || dup1 {
		t.Fatalf("create: err=%v dup=%v", err, dup1)
	}
	s2 := testSchedule(now)
	s2.AmountCents = 4242 // different params, same key: replay wins
	s2.IdempotencyKey = "key-1"
	got2, dup2, err := l.CreateTransferSchedule(s2)
	if err != nil || !dup2 {
		t.Fatalf("replay: err=%v dup=%v", err, dup2)
	}
	if got2.AmountCents != got1.AmountCents {
		t.Errorf("replay returned different schedule: %+v vs %+v", got2, got1)
	}
}

func TestSweepDueFires(t *testing.T) {
	l := New()
	now := time.Now().UTC().Truncate(time.Second)
	fundAccount(t, l, "alice", 100000, "USD")

	s := testSchedule(now)
	if _, _, err := l.CreateTransferSchedule(s); err != nil {
		t.Fatalf("create: %v", err)
	}
	runs := l.SweepDue(now)
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	run := runs[0]
	if run.Status != "fired" || run.Duplicate {
		t.Errorf("run = %+v, want fired non-duplicate", run)
	}
	if run.RunSeq != 1 || run.TransferID != "sched-1/run/1" {
		t.Errorf("run identity = %+v", run)
	}
	if run.SkippedRuns != 0 {
		t.Errorf("skipped = %d, want 0", run.SkippedRuns)
	}
	if got := l.BalanceIn("alice", "USD"); got != 99000 {
		t.Errorf("alice = %d, want 99000", got)
	}
	if got := l.BalanceIn("bob", "USD"); got != 1000 {
		t.Errorf("bob = %d, want 1000", got)
	}

	got, err := l.GetTransferSchedule("sched-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.RunSeq != 1 {
		t.Errorf("run_seq = %d, want 1", got.RunSeq)
	}
	if !got.NextRunAt.After(now) {
		t.Errorf("next_run_at = %v, want after now", got.NextRunAt)
	}
	if got.Status != ScheduleActive {
		t.Errorf("status = %q, want active", got.Status)
	}

	// Second sweep before the next run: nothing due.
	if runs := l.SweepDue(now); len(runs) != 0 {
		t.Errorf("second sweep runs = %d, want 0", len(runs))
	}
}

func TestSweepDueNotDue(t *testing.T) {
	l := New()
	now := time.Now().UTC()
	fundAccount(t, l, "alice", 100000, "USD")
	s := testSchedule(now)
	s.NextRunAt = now.Add(time.Hour)
	if _, _, err := l.CreateTransferSchedule(s); err != nil {
		t.Fatalf("create: %v", err)
	}
	if runs := l.SweepDue(now); len(runs) != 0 {
		t.Errorf("runs = %d, want 0", len(runs))
	}
	if got := l.BalanceIn("alice", "USD"); got != 100000 {
		t.Errorf("alice moved without a due run: %d", got)
	}
}

func TestSweepDueSkipsMissedRuns(t *testing.T) {
	l := New()
	now := time.Now().UTC().Truncate(time.Second)
	fundAccount(t, l, "alice", 100000, "USD")
	s := testSchedule(now)
	s.NextRunAt = now.Add(-5 * time.Hour) // five intervals behind
	if _, _, err := l.CreateTransferSchedule(s); err != nil {
		t.Fatalf("create: %v", err)
	}
	runs := l.SweepDue(now)
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	// One run fires (the oldest due); the five missed nominal runs are
	// skipped, never caught up.
	if runs[0].SkippedRuns != 5 {
		t.Errorf("skipped = %d, want 5", runs[0].SkippedRuns)
	}
	if got := l.BalanceIn("bob", "USD"); got != 1000 {
		t.Errorf("bob = %d, want exactly one run's 1000", got)
	}
	got, _ := l.GetTransferSchedule("sched-1")
	wantNext := now.Add(time.Hour)
	if !got.NextRunAt.Equal(wantNext) {
		t.Errorf("next_run_at = %v, want %v", got.NextRunAt, wantNext)
	}
}

func TestSweepDueFailedRunRetries(t *testing.T) {
	l := New()
	now := time.Now().UTC()
	fundAccount(t, l, "alice", 100000, "USD")
	s := testSchedule(now)
	if _, _, err := l.CreateTransferSchedule(s); err != nil {
		t.Fatalf("create: %v", err)
	}
	l.Freeze("alice")
	runs := l.SweepDue(now)
	if len(runs) != 1 || runs[0].Status != "failed" {
		t.Fatalf("runs = %+v, want one failed run", runs)
	}
	if runs[0].Error == "" {
		t.Error("failed run has no error recorded")
	}
	// The failed run commits nothing: RunSeq stays 0 so the next sweep
	// retries the same run under the same idempotency key.
	got, _ := l.GetTransferSchedule("sched-1")
	if got.RunSeq != 0 {
		t.Errorf("run_seq after failure = %d, want 0 (committed only on fire)", got.RunSeq)
	}
	if !got.NextRunAt.Equal(s.NextRunAt) {
		t.Errorf("next_run_at moved on failure: %v", got.NextRunAt)
	}

	l.Unfreeze("alice")
	runs = l.SweepDue(now)
	if len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("retry runs = %+v, want one fired run", runs)
	}
	if runs[0].RunSeq != 1 {
		t.Errorf("retried run_seq = %d, want 1 (same run retried)", runs[0].RunSeq)
	}
	if got := l.BalanceIn("bob", "USD"); got != 1000 {
		t.Errorf("bob = %d, want 1000 (exactly one booking)", got)
	}
	// A failed run never blocks other schedules.
}

func TestSweepDueFailedRunDoesNotBlockOthers(t *testing.T) {
	l := New()
	now := time.Now().UTC()
	fundAccount(t, l, "alice", 100000, "USD")
	fundAccount(t, l, "carol", 100000, "USD")
	bad := testSchedule(now)
	bad.ID = "bad"
	if _, _, err := l.CreateTransferSchedule(bad); err != nil {
		t.Fatalf("create bad: %v", err)
	}
	good := testSchedule(now)
	good.ID = "good"
	good.From = "carol"
	if _, _, err := l.CreateTransferSchedule(good); err != nil {
		t.Fatalf("create good: %v", err)
	}
	l.Freeze("alice")
	runs := l.SweepDue(now)
	if len(runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(runs))
	}
	byID := map[string]ScheduleRun{}
	for _, r := range runs {
		byID[r.ScheduleID] = r
	}
	if byID["bad"].Status != "failed" {
		t.Errorf("bad run = %+v, want failed", byID["bad"])
	}
	if byID["good"].Status != "fired" {
		t.Errorf("good run = %+v, want fired", byID["good"])
	}
}

func TestSweepDueDuplicateAfterCrash(t *testing.T) {
	l := New()
	now := time.Now().UTC()
	fundAccount(t, l, "alice", 100000, "USD")
	s := testSchedule(now)
	if _, _, err := l.CreateTransferSchedule(s); err != nil {
		t.Fatalf("create: %v", err)
	}
	runs := l.SweepDue(now)
	if len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("first sweep = %+v", runs)
	}
	// Simulate a crash between the transfer commit and the schedule
	// state advance: rewind NextRunAt and RunSeq as if the finalize
	// never happened. The next sweep must replay the same run under the
	// same idempotency key.
	l.mu.Lock()
	l.schedules["sched-1"].NextRunAt = s.NextRunAt
	l.schedules["sched-1"].RunSeq = 0
	l.mu.Unlock()

	runs = l.SweepDue(now)
	if len(runs) != 1 {
		t.Fatalf("second sweep runs = %d, want 1", len(runs))
	}
	if !runs[0].Duplicate {
		t.Errorf("replay run = %+v, want duplicate=true", runs[0])
	}
	if got := l.BalanceIn("bob", "USD"); got != 1000 {
		t.Errorf("bob = %d, want 1000: the replay must not double-book", got)
	}
}

func TestSchedulePauseResumeCancel(t *testing.T) {
	l := New()
	now := time.Now().UTC()
	fundAccount(t, l, "alice", 100000, "USD")
	s := testSchedule(now)
	if _, _, err := l.CreateTransferSchedule(s); err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := l.PauseTransferSchedule("sched-1"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if runs := l.SweepDue(now); len(runs) != 0 {
		t.Errorf("paused schedule fired: %+v", runs)
	}
	if _, err := l.PauseTransferSchedule("sched-1"); !errors.Is(err, ErrScheduleBadTransition) {
		t.Errorf("double pause err = %v, want bad transition", err)
	}
	if _, err := l.ResumeTransferSchedule("sched-1"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if runs := l.SweepDue(now); len(runs) != 1 {
		t.Errorf("resumed schedule runs = %d, want 1", len(runs))
	}
	if _, err := l.CancelTransferSchedule("sched-1"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	got, _ := l.GetTransferSchedule("sched-1")
	if got.Status != ScheduleCancelled {
		t.Errorf("status = %q, want cancelled", got.Status)
	}
	if _, err := l.ResumeTransferSchedule("sched-1"); !errors.Is(err, ErrScheduleBadTransition) {
		t.Errorf("resume cancelled err = %v, want bad transition", err)
	}
	if _, err := l.PauseTransferSchedule("nope"); !errors.Is(err, ErrScheduleNotFound) {
		t.Errorf("pause unknown err = %v, want not found", err)
	}
}

func TestScheduleEndsAtCompletes(t *testing.T) {
	l := New()
	now := time.Now().UTC().Truncate(time.Second)
	fundAccount(t, l, "alice", 100000, "USD")
	s := testSchedule(now)
	s.EndsAt = now.Add(90 * time.Minute) // two runs fit: T+0, T+60m
	if _, _, err := l.CreateTransferSchedule(s); err != nil {
		t.Fatalf("create: %v", err)
	}
	if runs := l.SweepDue(now); len(runs) != 1 {
		t.Fatalf("first sweep = %d runs", len(runs))
	}
	if runs := l.SweepDue(now.Add(time.Hour)); len(runs) != 1 {
		t.Fatalf("second sweep = %d runs", len(runs))
	}
	got, _ := l.GetTransferSchedule("sched-1")
	if got.Status != ScheduleCompleted {
		t.Errorf("status = %q, want completed", got.Status)
	}
	if got := l.BalanceIn("bob", "USD"); got != 2000 {
		t.Errorf("bob = %d, want 2000", got)
	}
	// Completed schedules never fire again.
	if runs := l.SweepDue(now.Add(3 * time.Hour)); len(runs) != 0 {
		t.Errorf("completed schedule fired: %+v", runs)
	}
}

func TestScheduleRunHistoryBounded(t *testing.T) {
	l := New()
	now := time.Now().UTC()
	fundAccount(t, l, "alice", 100000000, "USD")
	s := testSchedule(now)
	s.Interval = time.Nanosecond
	if _, _, err := l.CreateTransferSchedule(s); err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 0; i < maxScheduleRuns+5; i++ {
		now = now.Add(time.Second)
		runs := l.SweepDue(now)
		if len(runs) != 1 {
			t.Fatalf("sweep %d: %d runs", i, len(runs))
		}
	}
	runs, err := l.ListScheduleRuns("sched-1", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(runs) != maxScheduleRuns {
		t.Errorf("history = %d runs, want capped at %d", len(runs), maxScheduleRuns)
	}
	// Newest first.
	if runs[0].RunSeq <= runs[len(runs)-1].RunSeq {
		t.Errorf("history not newest-first: %d then %d", runs[0].RunSeq, runs[len(runs)-1].RunSeq)
	}
	limited, err := l.ListScheduleRuns("sched-1", 3)
	if err != nil || len(limited) != 3 {
		t.Errorf("limited list = %d, err = %v", len(limited), err)
	}
	if _, err := l.ListScheduleRuns("nope", 0); !errors.Is(err, ErrScheduleNotFound) {
		t.Errorf("unknown runs err = %v, want not found", err)
	}
}

func TestSweepDueConcurrentSerializesPerSchedule(t *testing.T) {
	l := New()
	now := time.Now().UTC()
	fundAccount(t, l, "alice", 100000000, "USD")
	s := testSchedule(now)
	if _, _, err := l.CreateTransferSchedule(s); err != nil {
		t.Fatalf("create: %v", err)
	}
	const workers = 8
	var wg sync.WaitGroup
	results := make([][]ScheduleRun, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = l.SweepDue(now)
		}(i)
	}
	wg.Wait()
	total := 0
	seqs := map[uint64]bool{}
	for _, rs := range results {
		for _, r := range rs {
			total++
			if seqs[r.RunSeq] {
				t.Errorf("run_seq %d fired twice: double-fire under concurrency", r.RunSeq)
			}
			seqs[r.RunSeq] = true
		}
	}
	if total != 1 {
		t.Errorf("total runs = %d, want exactly 1 (per-schedule serialization)", total)
	}
	if got := l.BalanceIn("bob", "USD"); got != 1000 {
		t.Errorf("bob = %d, want exactly one run's 1000", got)
	}
}

func TestScheduleSweeper(t *testing.T) {
	l := New()
	fundAccount(t, l, "alice", 100000, "USD")
	now := time.Now().UTC()
	s := testSchedule(now)
	if _, _, err := l.CreateTransferSchedule(s); err != nil {
		t.Fatalf("create: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	fired, failed := 0, 0
	sw := StartScheduleSweeper(ctx, l, 10*time.Millisecond, func(fi, fa int) {
		mu.Lock()
		fired += fi
		failed += fa
		mu.Unlock()
	})
	if sw == nil {
		t.Fatal("sweeper is nil for a positive interval")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		f := fired
		mu.Unlock()
		if f >= 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	sw.Stop()
	sw.Stop() // idempotent
	mu.Lock()
	defer mu.Unlock()
	if fired < 1 {
		t.Errorf("sweeper fired %d runs, want >= 1", fired)
	}
	if failed != 0 {
		t.Errorf("sweeper failed %d runs, want 0", failed)
	}
	if sw.Ticks() == 0 {
		t.Error("sweeper recorded no ticks")
	}
	if StartScheduleSweeper(ctx, l, 0, nil) != nil {
		t.Error("non-positive interval must disable the sweeper")
	}
}

func TestReconcileReportsActiveSchedules(t *testing.T) {
	l := New()
	now := time.Now().UTC()
	s1 := testSchedule(now)
	s1.ID = "a"
	s2 := testSchedule(now)
	s2.ID = "b"
	if _, _, err := l.CreateTransferSchedule(s1); err != nil {
		t.Fatalf("create a: %v", err)
	}
	if _, _, err := l.CreateTransferSchedule(s2); err != nil {
		t.Fatalf("create b: %v", err)
	}
	if _, err := l.PauseTransferSchedule("b"); err != nil {
		t.Fatalf("pause b: %v", err)
	}
	report := l.Reconcile(now)
	if report.ActiveTransferSchedules != 1 {
		t.Errorf("active schedules = %d, want 1", report.ActiveTransferSchedules)
	}
}

func TestTransferScheduleJSONRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	s := testSchedule(now)
	s.Status = ScheduleActive
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back TransferSchedule
	// encoding/json here is the stdlib: exercise the custom marshalers.
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Interval != time.Hour {
		t.Errorf("interval = %v, want 1h", back.Interval)
	}
	if !schedulesEqual(&s, &back) {
		t.Errorf("round trip mismatch:\n%+v\n%+v", s, back)
	}
	if !bytes.Contains(data, []byte(`"interval":"1h0m0s"`)) {
		t.Errorf("interval not rendered as duration string: %s", data)
	}
	var bad TransferSchedule
	if err := json.Unmarshal([]byte(`{"id":"x","interval":"not-a-duration"}`), &bad); err == nil {
		t.Error("bad interval unmarshals without error")
	}
}

func TestScheduleSnapshotRoundTrip(t *testing.T) {
	l := New()
	now := time.Now().UTC().Truncate(time.Second)
	fundAccount(t, l, "alice", 100000, "USD")
	s := testSchedule(now)
	s.IdempotencyKey = "sched-key-1"
	if _, _, err := l.CreateTransferSchedule(s); err != nil {
		t.Fatalf("create: %v", err)
	}
	s2 := testSchedule(now)
	s2.ID = "sched-2"
	s2.Status = ScheduleActive
	if _, _, err := l.CreateTransferSchedule(s2); err != nil {
		t.Fatalf("create 2: %v", err)
	}
	if _, err := l.PauseTransferSchedule("sched-2"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	l.SweepDue(now) // advance sched-1 state: RunSeq=1, NextRunAt moved

	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("export: %v", err)
	}
	restored, err := ImportSnapshot(&buf)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if !snapshotLedgersEqual(l, restored) {
		t.Error("restored ledger differs from the original")
	}
	got, err := restored.GetTransferSchedule("sched-1")
	if err != nil {
		t.Fatalf("get restored: %v", err)
	}
	if got.RunSeq != 1 || got.Status != ScheduleActive {
		t.Errorf("restored sched-1 = %+v, want run_seq 1 active", got)
	}
	got2, err := restored.GetTransferSchedule("sched-2")
	if err != nil || got2.Status != SchedulePaused {
		t.Errorf("restored sched-2 = %+v, err = %v; want paused", got2, err)
	}
	// Creation idempotency survives the restore.
	_, dup, err := restored.CreateTransferSchedule(s)
	if err != nil || !dup {
		t.Errorf("replay after restore: dup=%v err=%v, want dup=true", dup, err)
	}
	// The restored ledger keeps firing on the surviving state.
	runs := restored.SweepDue(now.Add(2 * time.Hour))
	if len(runs) != 1 || runs[0].ScheduleID != "sched-1" {
		t.Errorf("restored sweep = %+v, want sched-1's next run", runs)
	}
}

func TestScheduleSnapshotRejectsCorrupt(t *testing.T) {
	l := New()
	now := time.Now().UTC()
	s := testSchedule(now)
	if _, _, err := l.CreateTransferSchedule(s); err != nil {
		t.Fatalf("create: %v", err)
	}
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("export: %v", err)
	}
	// Corrupt the schedule record's interval.
	corrupt := bytes.Replace(buf.Bytes(), []byte(`"interval":"1h0m0s"`), []byte(`"interval":"bogus"`), 1)
	if _, err := ImportSnapshot(bytes.NewReader(corrupt)); err == nil {
		t.Error("import accepted a corrupt schedule interval")
	}
}
