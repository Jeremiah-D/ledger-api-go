package ledger

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// sweepReportRuns drives n sweeps of a due-now daily schedule at 24h
// steps and returns the schedule's run history (newest first). Each
// sweep fires exactly one run: NextRunAt advances arithmetically past
// the sweep time, so the next 24h step is due again.
func sweepReportRuns(t *testing.T, l *Ledger, dir string, n int) []ReportRun {
	t.Helper()
	postSomeEntries(t, l)
	s := validReportSchedule(dir)
	mustCreateReportSchedule(t, l, s)
	now := time.Now()
	for i := 0; i < n; i++ {
		runs := l.SweepReportsDue(now.Add(time.Duration(i) * 24 * time.Hour))
		if len(runs) != 1 {
			t.Fatalf("sweep %d: fired %d runs, want 1", i, len(runs))
		}
		if runs[0].Status != "fired" {
			t.Fatalf("sweep %d: run = %+v, want fired", i, runs[0])
		}
	}
	hist, err := l.ListReportRuns(s.ID, 0)
	if err != nil {
		t.Fatalf("ListReportRuns: %v", err)
	}
	return hist
}

func reportRunSeqs(hist []ReportRun) []uint64 {
	seqs := make([]uint64, len(hist))
	for i, r := range hist {
		seqs[i] = r.RunSeq
	}
	return seqs
}

// TestSetReportRunHistoryDepthInvalidRejected pins the fail-closed
// gate: a depth below 1 is rejected and the live depth is left
// untouched. Zero is rejected, not a "keep nothing" state — a zero
// depth would make ListReportRuns return nothing forever, which is
// switching the query surface off, not a retention policy (along
// LG-336).
func TestSetReportRunHistoryDepthInvalidRejected(t *testing.T) {
	l := New()
	if got := l.ReportRunHistoryDepth(); got != defaultMaxReportRuns {
		t.Fatalf("default depth = %d, want %d", got, defaultMaxReportRuns)
	}
	for _, n := range []int{0, -1, -100} {
		prev, err := l.SetReportRunHistoryDepth(n)
		if !errors.Is(err, ErrInvalidReportRunHistoryDepth) {
			t.Fatalf("SetReportRunHistoryDepth(%d) = (%d, %v), want ErrInvalidReportRunHistoryDepth", n, prev, err)
		}
		if prev != defaultMaxReportRuns {
			t.Fatalf("SetReportRunHistoryDepth(%d) prev = %d, want %d (live)", n, prev, defaultMaxReportRuns)
		}
	}
	// The Ctx variant rejects the same way.
	if prev, err := l.SetReportRunHistoryDepthCtx(context.Background(), 0); !errors.Is(err, ErrInvalidReportRunHistoryDepth) || prev != defaultMaxReportRuns {
		t.Fatalf("SetReportRunHistoryDepthCtx(0) = (%d, %v), want (%d, ErrInvalidReportRunHistoryDepth)", prev, err, defaultMaxReportRuns)
	}
	if got := l.ReportRunHistoryDepth(); got != defaultMaxReportRuns {
		t.Fatalf("depth after rejected revisions = %d, want %d", got, defaultMaxReportRuns)
	}
}

// TestSetReportRunHistoryDepthSameValueNoop pins the no-op:
// revising to the current depth returns the live value with a nil
// error and writes no audit event.
func TestSetReportRunHistoryDepthSameValueNoop(t *testing.T) {
	auditDir := t.TempDir()
	al, err := NewAuditLog(auditDir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	prev, err := l.SetReportRunHistoryDepth(defaultMaxReportRuns)
	if err != nil {
		t.Fatalf("SetReportRunHistoryDepth(%d) same value: %v", defaultMaxReportRuns, err)
	}
	if prev != defaultMaxReportRuns {
		t.Fatalf("prev = %d, want %d", prev, defaultMaxReportRuns)
	}
	waitAuditWritten(t, al, 1) // the log's startup alert only
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := len(auditEventsByOp(t, auditDir)["report_runs_retuned"]); n != 0 {
		t.Fatalf("same-value revision wrote %d retune events, want 0", n)
	}
}

// TestSetReportRunHistoryDepthGetterFollowsLiveValue pins that the
// getter tracks the live value through repeated revisions in both
// directions, including a return to the default.
func TestSetReportRunHistoryDepthGetterFollowsLiveValue(t *testing.T) {
	l := New()
	for _, n := range []int{365, 30, 200, 100} {
		prev, err := l.SetReportRunHistoryDepth(n)
		if err != nil {
			t.Fatalf("SetReportRunHistoryDepth(%d): %v", n, err)
		}
		_ = prev
		if got := l.ReportRunHistoryDepth(); got != n {
			t.Fatalf("getter after revision to %d = %d", n, got)
		}
	}
}

// TestSetReportRunHistoryDepthAuditDetailsExact pins the audit
// payload: exactly the two integers, previous and current — no
// schedule content ever rides in a retune event. (Integers decode
// as float64 through the audit log's JSON round-trip, along the
// audit_log_max_bytes tests.)
func TestSetReportRunHistoryDepthAuditDetailsExact(t *testing.T) {
	auditDir := t.TempDir()
	al, err := NewAuditLog(auditDir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	if _, err := l.SetReportRunHistoryDepth(365); err != nil {
		t.Fatalf("SetReportRunHistoryDepth(365): %v", err)
	}
	if _, err := l.SetReportRunHistoryDepth(30); err != nil {
		t.Fatalf("SetReportRunHistoryDepth(30): %v", err)
	}
	waitAuditWritten(t, al, 3) // alert + 2 retunes
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	retuned := auditEventsByOp(t, auditDir)["report_runs_retuned"]
	if len(retuned) != 2 {
		t.Fatalf("report_runs_retuned events = %d, want 2", len(retuned))
	}
	want := []map[string]any{
		{"previous_depth": float64(100), "current_depth": float64(365)},
		{"previous_depth": float64(365), "current_depth": float64(30)},
	}
	for i, ev := range retuned {
		if len(ev.Details) != 2 {
			t.Fatalf("retune[%d] details keys = %v, want exactly previous_depth + current_depth", i, ev.Details)
		}
		for k, v := range want[i] {
			if ev.Details[k] != v {
				t.Errorf("retune[%d] details[%q] = %v, want %v", i, k, ev.Details[k], v)
			}
		}
		if ev.Actor != "system" {
			t.Errorf("retune[%d] actor = %q, want system (plain setter default)", i, ev.Actor)
		}
	}
}

// TestSetReportRunHistoryDepthCtxActorAttribution pins the
// attribution on the Ctx variant: the operator identity replaces
// the default actor, while the plain setter keeps the "system"
// default.
func TestSetReportRunHistoryDepthCtxActorAttribution(t *testing.T) {
	auditDir := t.TempDir()
	al, err := NewAuditLog(auditDir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	ctx := ContextWithActor(context.Background(), "ops-reports")
	if prev, err := l.SetReportRunHistoryDepthCtx(ctx, 365); err != nil || prev != defaultMaxReportRuns {
		t.Fatalf("SetReportRunHistoryDepthCtx(365) = (%d, %v), want (100, nil)", prev, err)
	}
	if prev, err := l.SetReportRunHistoryDepth(30); err != nil || prev != 365 {
		t.Fatalf("SetReportRunHistoryDepth(30) = (%d, %v), want (365, nil)", prev, err)
	}
	waitAuditWritten(t, al, 3) // alert + 2 retunes
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	retuned := auditEventsByOp(t, auditDir)["report_runs_retuned"]
	if len(retuned) != 2 {
		t.Fatalf("report_runs_retuned events = %d, want 2", len(retuned))
	}
	if retuned[0].Actor != "ops-reports" {
		t.Errorf("Ctx retune actor = %q, want ops-reports", retuned[0].Actor)
	}
	if retuned[1].Actor != "system" {
		t.Errorf("plain retune actor = %q, want system", retuned[1].Actor)
	}
}

// TestReportRunHistoryDepthWidenKeepsMoreRuns pins that widening
// the depth keeps more runs going forward: with the depth at 3,
// three sweeps retain all three runs; after widening to 10, a
// fourth sweep is retained too instead of evicting the oldest.
// (The backlog's "widen to 3, 4th run keeps all 4" phrasing is
// imprecise: a depth of 3 evicts past 3 at record time, so the
// widening must land before the 4th run — the test proves exactly
// that ordering.)
func TestReportRunHistoryDepthWidenKeepsMoreRuns(t *testing.T) {
	l := New()
	if _, err := l.SetReportRunHistoryDepth(3); err != nil {
		t.Fatalf("SetReportRunHistoryDepth(3): %v", err)
	}
	dir := t.TempDir()
	postSomeEntries(t, l)
	s := validReportSchedule(dir)
	mustCreateReportSchedule(t, l, s)
	now := time.Now()
	for i := 0; i < 3; i++ {
		if runs := l.SweepReportsDue(now.Add(time.Duration(i) * 24 * time.Hour)); len(runs) != 1 {
			t.Fatalf("sweep %d: fired %d runs, want 1", i, len(runs))
		}
	}
	hist, err := l.ListReportRuns(s.ID, 0)
	if err != nil {
		t.Fatalf("ListReportRuns: %v", err)
	}
	if got := reportRunSeqs(hist); len(got) != 3 || got[0] != 3 || got[2] != 1 {
		t.Fatalf("history seqs at depth 3 = %v, want [3 2 1]", got)
	}
	if _, err := l.SetReportRunHistoryDepth(10); err != nil {
		t.Fatalf("SetReportRunHistoryDepth(10): %v", err)
	}
	if runs := l.SweepReportsDue(now.Add(72 * time.Hour)); len(runs) != 1 || runs[0].RunSeq != 4 {
		t.Fatalf("4th sweep = %+v, want one run with seq 4", runs)
	}
	hist, err = l.ListReportRuns(s.ID, 0)
	if err != nil {
		t.Fatalf("ListReportRuns: %v", err)
	}
	if got := reportRunSeqs(hist); len(got) != 4 || got[0] != 4 || got[3] != 1 {
		t.Fatalf("history seqs after widening = %v, want [4 3 2 1]", got)
	}
}

// TestReportRunHistoryDepthTightenTrimsImmediately pins the
// tighten rule: narrowing the depth trims every subscription's
// stored history at once, oldest first, and the next
// ListReportRuns already sees the new depth — while the schedule's
// RunSeq and fields are untouched.
func TestReportRunHistoryDepthTightenTrimsImmediately(t *testing.T) {
	l := New()
	dir := t.TempDir()
	postSomeEntries(t, l)
	s := validReportSchedule(dir)
	mustCreateReportSchedule(t, l, s)
	now := time.Now()
	for i := 0; i < 4; i++ {
		if runs := l.SweepReportsDue(now.Add(time.Duration(i) * 24 * time.Hour)); len(runs) != 1 {
			t.Fatalf("sweep %d: fired %d runs, want 1", i, len(runs))
		}
	}
	// Capture the subscription after the sweeps, before the
	// revision: the sweeps legitimately advanced RunSeq and
	// NextRunAt, so only a change past this point would be the
	// revision's doing.
	before, err := l.GetReportSchedule(s.ID)
	if err != nil {
		t.Fatalf("GetReportSchedule: %v", err)
	}
	if before.RunSeq != 4 {
		t.Fatalf("RunSeq before revision = %d, want 4", before.RunSeq)
	}
	if prev, err := l.SetReportRunHistoryDepth(2); err != nil || prev != defaultMaxReportRuns {
		t.Fatalf("SetReportRunHistoryDepth(2) = (%d, %v), want (100, nil)", prev, err)
	}
	hist, err := l.ListReportRuns(s.ID, 0)
	if err != nil {
		t.Fatalf("ListReportRuns: %v", err)
	}
	if len(hist) != 2 {
		t.Fatalf("history = %d runs, want 2 right after tightening", len(hist))
	}
	if got := reportRunSeqs(hist); got[0] != 4 || got[1] != 3 {
		t.Fatalf("history seqs = %v, want [4 3] (newest kept)", got)
	}
	// The revision never touches the subscription itself.
	after, err := l.GetReportSchedule(s.ID)
	if err != nil {
		t.Fatalf("GetReportSchedule: %v", err)
	}
	if after.RunSeq != before.RunSeq || after.RunSeq != 4 {
		t.Errorf("RunSeq = %d, want 4 (unchanged)", after.RunSeq)
	}
	if after.Kind != before.Kind || after.Interval != before.Interval ||
		!after.NextRunAt.Equal(before.NextRunAt) || after.Sink != before.Sink ||
		after.Status != before.Status {
		t.Errorf("subscription changed by a depth revision: before=%+v after=%+v", before, after)
	}
}

// TestReportRunHistoryDepthTightenToOneKeepsNewest pins the
// boundary: a depth of 1 keeps exactly the newest run.
func TestReportRunHistoryDepthTightenToOneKeepsNewest(t *testing.T) {
	l := New()
	hist := sweepReportRuns(t, l, t.TempDir(), 4)
	if len(hist) != 4 {
		t.Fatalf("history = %d runs, want 4 before tightening", len(hist))
	}
	if _, err := l.SetReportRunHistoryDepth(1); err != nil {
		t.Fatalf("SetReportRunHistoryDepth(1): %v", err)
	}
	hist, err := l.ListReportRuns("daily-reconcile", 0)
	if err != nil {
		t.Fatalf("ListReportRuns: %v", err)
	}
	if len(hist) != 1 || hist[0].RunSeq != 4 {
		t.Fatalf("history = %+v, want exactly run 4", hist)
	}
}

// TestReportRunHistoryDepthWidenDoesNotReviveTrimmed pins that
// widening never revives history already evicted: after trimming
// to 2 and widening to 10, one more sweep leaves 3 runs (the two
// survivors plus the new one) — runs 1 and 2 stay gone.
func TestReportRunHistoryDepthWidenDoesNotReviveTrimmed(t *testing.T) {
	l := New()
	dir := t.TempDir()
	postSomeEntries(t, l)
	s := validReportSchedule(dir)
	mustCreateReportSchedule(t, l, s)
	now := time.Now()
	for i := 0; i < 4; i++ {
		if runs := l.SweepReportsDue(now.Add(time.Duration(i) * 24 * time.Hour)); len(runs) != 1 {
			t.Fatalf("sweep %d: fired %d runs, want 1", i, len(runs))
		}
	}
	if _, err := l.SetReportRunHistoryDepth(2); err != nil {
		t.Fatalf("tighten to 2: %v", err)
	}
	if _, err := l.SetReportRunHistoryDepth(10); err != nil {
		t.Fatalf("widen to 10: %v", err)
	}
	// A far-future sweep fires exactly one more run (run 5).
	if runs := l.SweepReportsDue(now.Add(30 * 24 * time.Hour)); len(runs) != 1 || runs[0].RunSeq != 5 {
		t.Fatalf("far-future sweep = %+v, want one run with seq 5", runs)
	}
	hist, err := l.ListReportRuns(s.ID, 0)
	if err != nil {
		t.Fatalf("ListReportRuns: %v", err)
	}
	if got := reportRunSeqs(hist); len(got) != 3 || got[0] != 5 || got[1] != 4 || got[2] != 3 {
		t.Fatalf("history seqs = %v, want [5 4 3] (trimmed runs not revived)", got)
	}
}

// TestReportRunHistoryDepthConcurrentRevisionAndSweep pins that
// concurrent revisions race cleanly with concurrent sweeps and
// lists: every depth read is under l.mu, so the race detector is
// the assertion — the final depth is one of the values written,
// and the history never exceeds it.
func TestReportRunHistoryDepthConcurrentRevisionAndSweep(t *testing.T) {
	l := New()
	dir := t.TempDir()
	postSomeEntries(t, l)
	s := validReportSchedule(dir)
	mustCreateReportSchedule(t, l, s)
	depths := []int{1, 2, 3, 100}
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				_, _ = l.SetReportRunHistoryDepth(depths[(g+i)%len(depths)])
			}
		}(g)
	}
	now := time.Now()
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				_ = l.SweepReportsDue(now.Add(time.Duration(i*5+j) * 24 * time.Hour))
				_, _ = l.ListReportRuns(s.ID, 0)
			}
		}(i)
	}
	wg.Wait()
	got := l.ReportRunHistoryDepth()
	for _, d := range depths {
		if got == d {
			return
		}
	}
	t.Errorf("live depth = %d, want one of %v", got, depths)
}
