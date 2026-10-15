package ledger

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// previewTestSchedule builds an active hourly schedule starting 30
// minutes in the future: nominal firings land at +30m, +90m, +150m, ...
// relative to now, comfortably inside any multi-hour horizon.
func previewTestSchedule(id string) TransferSchedule {
	return TransferSchedule{
		ID:          id,
		From:        "alice",
		To:          "bob",
		AmountCents: 1000,
		Currency:    "USD",
		Interval:    time.Hour,
		NextRunAt:   time.Now().UTC().Add(30 * time.Minute),
	}
}

func createPreviewSchedule(t *testing.T, l *Ledger, s TransferSchedule) TransferSchedule {
	t.Helper()
	got, _, err := l.CreateTransferSchedule(s)
	if err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	return got
}

func TestPreviewSchedulePass(t *testing.T) {
	l := New()
	fundAccount(t, l, "alice", 100000, "USD")
	createPreviewSchedule(t, l, previewTestSchedule("sched-1"))

	p, err := l.PreviewSchedule("sched-1", 3*time.Hour)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(p.Firings) != 3 {
		t.Fatalf("firings = %d, want 3", len(p.Firings))
	}
	for i, f := range p.Firings {
		if !f.Admitted {
			t.Errorf("firing %d: admitted = false, reason = %q", i, f.Reason)
		}
		if f.Code != "" || f.Reason != "" {
			t.Errorf("firing %d: code/reason = %q/%q, want empty on admit", i, f.Code, f.Reason)
		}
		wantSeq := uint64(i + 1)
		if f.RunSeq != wantSeq {
			t.Errorf("firing %d: run_seq = %d, want %d", i, f.RunSeq, wantSeq)
		}
		wantID := fmt.Sprintf("sched-1/run/%d", wantSeq)
		if f.TransferID != wantID {
			t.Errorf("firing %d: transfer_id = %q, want %q", i, f.TransferID, wantID)
		}
		if len(f.Legs) == 0 {
			t.Errorf("firing %d: no legs on admitted run", i)
		}
	}
	// Nominal due times follow the hourly cadence from NextRunAt.
	for i := 1; i < len(p.Firings); i++ {
		if got := p.Firings[i].DueAt.Sub(p.Firings[i-1].DueAt); got != time.Hour {
			t.Errorf("firing gap = %v, want 1h", got)
		}
	}
}

// A plan that outruns its funding: the first two runs admit and the
// third fails overdraft — the preview shows cumulative depletion across
// the horizon, with later firings evaluated against the twin state the
// earlier ones left behind.
func TestPreviewScheduleCumulativeDepletion(t *testing.T) {
	l := New()
	l.EnableOverdraftProtection("alice")
	fundAccount(t, l, "alice", 2500, "USD")
	createPreviewSchedule(t, l, previewTestSchedule("sched-1"))

	p, err := l.PreviewSchedule("sched-1", 3*time.Hour)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(p.Firings) != 3 {
		t.Fatalf("firings = %d, want 3", len(p.Firings))
	}
	for i := 0; i < 2; i++ {
		if !p.Firings[i].Admitted {
			t.Errorf("firing %d: admitted = false, reason = %q", i, p.Firings[i].Reason)
		}
	}
	last := p.Firings[2]
	if last.Admitted {
		t.Fatal("firing 2: admitted = true, want overdraft rejection")
	}
	if last.Code != "overdraft_not_allowed" {
		t.Errorf("firing 2: code = %q, want overdraft_not_allowed", last.Code)
	}
	if !strings.Contains(last.Reason, "overdraw") {
		t.Errorf("firing 2: reason = %q, want overdraft text", last.Reason)
	}
	if len(last.Legs) != 0 {
		t.Errorf("firing 2: legs = %d, want none on rejection", len(last.Legs))
	}
	// The live ledger still holds the full funding: the two admitted
	// firings only moved money on the discarded twin.
	if got := l.BalanceIn("alice", "USD"); got != 2500 {
		t.Errorf("alice = %d, want 2500 (preview moved nothing)", got)
	}
}

// A frozen payer rejects every firing with a stable ops code.
func TestPreviewScheduleFrozen(t *testing.T) {
	l := New()
	fundAccount(t, l, "alice", 100000, "USD")
	createPreviewSchedule(t, l, previewTestSchedule("sched-1"))
	l.Freeze("alice")

	p, err := l.PreviewSchedule("sched-1", 3*time.Hour)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(p.Firings) != 3 {
		t.Fatalf("firings = %d, want 3", len(p.Firings))
	}
	for i, f := range p.Firings {
		if f.Admitted {
			t.Errorf("firing %d: admitted = true, want frozen rejection", i)
		}
		if f.Code != "account_frozen" {
			t.Errorf("firing %d: code = %q, want account_frozen", i, f.Code)
		}
	}
}

// Preview is read-only: no journal writes, no version bump, no audit
// events, no idempotency keys consumed, and the schedule's own state
// (NextRunAt, RunSeq, Status) untouched.
func TestPreviewScheduleReadOnly(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("audit log: %v", err)
	}
	l := New(WithAuditLog(al))
	fundAccount(t, l, "alice", 100000, "USD")
	before := createPreviewSchedule(t, l, previewTestSchedule("sched-1"))
	versionBefore := l.Version()
	balBefore := l.BalanceIn("alice", "USD")

	if _, err := l.PreviewSchedule("sched-1", 3*time.Hour); err != nil {
		t.Fatalf("preview: %v", err)
	}
	if got := l.Version(); got != versionBefore {
		t.Errorf("version = %d, want %d (no bump)", got, versionBefore)
	}
	if got := l.BalanceIn("alice", "USD"); got != balBefore {
		t.Errorf("alice = %d, want %d (no movement)", got, balBefore)
	}
	after, err := l.GetTransferSchedule("sched-1")
	if err != nil {
		t.Fatalf("get schedule: %v", err)
	}
	if after.NextRunAt != before.NextRunAt || after.RunSeq != before.RunSeq || after.Status != before.Status {
		t.Errorf("schedule mutated: before=%+v after=%+v", before, after)
	}
	if _, ok := l.GetByIdempotencyKey("sched-1/run/1"); ok {
		t.Error("preview consumed idempotency key sched-1/run/1 on the live ledger")
	}
	if err := al.Close(); err != nil {
		t.Fatalf("close audit log: %v", err)
	}
	// Drain the async writer, then inspect the trail: the preview must
	// not have emitted any schedule_run_* events or transfer IDs.
	events, corrupt, err := ReadAuditLog(dir, time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt = %d, want 0", corrupt)
	}
	for _, ev := range events {
		if strings.HasPrefix(ev.Op, "schedule_run_") || strings.Contains(ev.TraceID, "/run/") {
			t.Errorf("preview emitted audit event: op=%q trace=%q", ev.Op, ev.TraceID)
		}
	}
}

// Paused and cancelled schedules preview zero firings; unknown IDs are
// an error, not an empty preview.
func TestPreviewScheduleInactive(t *testing.T) {
	l := New()
	fundAccount(t, l, "alice", 100000, "USD")
	createPreviewSchedule(t, l, previewTestSchedule("sched-1"))

	for _, tc := range []struct {
		name   string
		action func(string) error
	}{
		{"paused", func(id string) error { _, err := l.PauseTransferSchedule(id); return err }},
		{"cancelled", func(id string) error { _, err := l.CancelTransferSchedule(id); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "cancelled" {
				// Re-activate before cancelling: pause then resume.
				if _, err := l.ResumeTransferSchedule("sched-1"); err != nil {
					t.Fatalf("resume: %v", err)
				}
			}
			if err := tc.action("sched-1"); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			p, err := l.PreviewSchedule("sched-1", 24*time.Hour)
			if err != nil {
				t.Fatalf("preview: %v", err)
			}
			if len(p.Firings) != 0 {
				t.Errorf("firings = %d, want 0 for %s schedule", len(p.Firings), tc.name)
			}
		})
	}

	if _, err := l.PreviewSchedule("no-such-plan", time.Hour); !errors.Is(err, ErrScheduleNotFound) {
		t.Errorf("unknown id err = %v, want ErrScheduleNotFound", err)
	}
}

// Horizon validation and boundary semantics: zero/negative/too-long
// horizons are rejected; a nominal time exactly at the horizon end is
// included; EndsAt truncates the sequence (a nominal time past EndsAt
// completes the plan instead of firing).
func TestPreviewScheduleHorizonBounds(t *testing.T) {
	l := New()
	fundAccount(t, l, "alice", 100000, "USD")
	for _, h := range []time.Duration{0, -time.Hour, 8761 * time.Hour} {
		if _, err := l.PreviewSchedule("sched-1", h); !errors.Is(err, ErrScheduleBadHorizon) {
			t.Errorf("horizon %v: err = %v, want ErrScheduleBadHorizon", h, err)
		}
	}
	createPreviewSchedule(t, l, previewTestSchedule("sched-1"))

	p, err := l.PreviewSchedule("sched-1", 90*time.Minute)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	// Nominal times at +30m and +90m: both inside the 90m horizon.
	if len(p.Firings) != 2 {
		t.Fatalf("firings = %d, want 2", len(p.Firings))
	}

	// EndsAt exactly on a nominal time: that run still fires (SweepDue
	// completes only when NextRunAt advances *past* EndsAt).
	s := previewTestSchedule("sched-2")
	s.EndsAt = s.NextRunAt.Add(time.Hour)
	createPreviewSchedule(t, l, s)
	p2, err := l.PreviewSchedule("sched-2", 5*time.Hour)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(p2.Firings) != 2 {
		t.Fatalf("ends_at firings = %d, want 2", len(p2.Firings))
	}
}

// Missed runs are skipped, not backfilled: an overdue schedule previews
// one nominal firing in the past (what the next sweep would fire once)
// followed by the future cadence.
func TestPreviewScheduleSkipsMissedRuns(t *testing.T) {
	l := New()
	fundAccount(t, l, "alice", 100000, "USD")
	s := previewTestSchedule("sched-1")
	s.NextRunAt = time.Now().UTC().Add(-5 * time.Hour)
	createPreviewSchedule(t, l, s)

	p, err := l.PreviewSchedule("sched-1", 2*time.Hour)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(p.Firings) < 2 {
		t.Fatalf("firings = %d, want at least 2 (one overdue + future cadence)", len(p.Firings))
	}
	now := time.Now().UTC()
	if !p.Firings[0].DueAt.Before(now) {
		t.Errorf("first firing due_at = %v, want in the past (the overdue run)", p.Firings[0].DueAt)
	}
	for i := 1; i < len(p.Firings); i++ {
		if !p.Firings[i].DueAt.After(now) {
			t.Errorf("firing %d due_at = %v, want in the future", i, p.Firings[i].DueAt)
		}
	}
	// The future cadence resumes at the hourly interval: the missed gap
	// is skipped, not backfilled. (The first gap spans the stale nominal
	// time to the first future run, by design.)
	for i := 2; i < len(p.Firings); i++ {
		if got := p.Firings[i].DueAt.Sub(p.Firings[i-1].DueAt); got < time.Hour-time.Minute || got > time.Hour+time.Minute {
			t.Errorf("firing %d gap = %v, want ~1h (no backfill)", i, got)
		}
	}
}

// Budget impact rides each admitted firing: a budgeted payer shows the
// month's used spend before and after the simulated run.
func TestPreviewScheduleBudgetImpact(t *testing.T) {
	l := New()
	fundAccount(t, l, "alice", 100000, "USD")
	if err := l.SetAccountBudget("alice", "USD", 5000); err != nil {
		t.Fatalf("set budget: %v", err)
	}
	createPreviewSchedule(t, l, previewTestSchedule("sched-1"))

	p, err := l.PreviewSchedule("sched-1", 90*time.Minute)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(p.Firings) != 2 {
		t.Fatalf("firings = %d, want 2", len(p.Firings))
	}
	month := time.Now().UTC().Format("2006-01")
	for i, f := range p.Firings {
		if !f.Admitted {
			t.Fatalf("firing %d: admitted = false, reason = %q", i, f.Reason)
		}
		if len(f.BudgetImpact) != 1 {
			t.Fatalf("firing %d: budget impact rows = %d, want 1", i, len(f.BudgetImpact))
		}
		bi := f.BudgetImpact[0]
		if bi.Month != month || bi.BudgetCents != 5000 {
			t.Errorf("firing %d: impact = %+v, want month %s budget 5000", i, bi, month)
		}
		wantBefore := int64(i * 1000)
		if bi.UsedBeforeCents != wantBefore || bi.UsedAfterCents != wantBefore+1000 {
			t.Errorf("firing %d: used before/after = %d/%d, want %d/%d",
				i, bi.UsedBeforeCents, bi.UsedAfterCents, wantBefore, wantBefore+1000)
		}
	}
	// The live ledger's monthly spend is untouched.
	if got := l.MonthlySpend("alice", "USD", month); got != 0 {
		t.Errorf("monthly spend = %d, want 0 (preview only)", got)
	}
}
