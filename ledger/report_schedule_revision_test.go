package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func ptrReportParams(v ReportParams) *ReportParams { return &v }
func ptrReportSink(v ReportSink) *ReportSink       { return &v }

// revisedReportScheduleEvents closes the audit log and returns every
// report_schedule_revised event recorded in it. The ledger must not be
// used after this call.
func revisedReportScheduleEvents(t *testing.T, al *AuditLog) []AuditEvent {
	t.Helper()
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
		if ev.Op == "report_schedule_revised" {
			out = append(out, ev)
		}
	}
	return out
}

func TestReviseReportScheduleUnknownID(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	patch := ReportScheduleRevision{Interval: ptrDuration(time.Hour)}
	if _, err := l.ReviseReportSchedule("no-such-schedule", patch); !errors.Is(err, ErrReportScheduleNotFound) {
		t.Fatalf("ReviseReportSchedule unknown ID err = %v, want %v", err, ErrReportScheduleNotFound)
	}
	if events := revisedReportScheduleEvents(t, al); len(events) != 0 {
		t.Fatalf("rejected revision wrote %d audit events, want 0", len(events))
	}
}

func TestReviseReportScheduleTerminalStatesRejected(t *testing.T) {
	l := New()
	dir := t.TempDir()
	// Cancelled: cancel then revise.
	cancelled := mustCreateReportSchedule(t, l, validReportSchedule(dir))
	if _, err := l.CancelReportSchedule(cancelled.ID); err != nil {
		t.Fatalf("CancelReportSchedule: %v", err)
	}
	patch := ReportScheduleRevision{Interval: ptrDuration(time.Hour)}
	if _, err := l.ReviseReportSchedule(cancelled.ID, patch); !errors.Is(err, ErrReportScheduleBadTransition) {
		t.Fatalf("revise cancelled err = %v, want %v", err, ErrReportScheduleBadTransition)
	}
	got, err := l.GetReportSchedule(cancelled.ID)
	if err != nil {
		t.Fatalf("GetReportSchedule: %v", err)
	}
	if got.Interval != 24*time.Hour || got.Status != ScheduleCancelled {
		t.Fatalf("cancelled schedule moved: %+v", got)
	}

	// Completed: one sweep past EndsAt completes the schedule.
	l2 := New()
	dir2 := t.TempDir()
	s := validReportSchedule(dir2)
	s.ID = "completing"
	s.NextRunAt = time.Now().Add(-2 * time.Hour)
	s.EndsAt = time.Now().Add(-time.Hour)
	mustCreateReportSchedule(t, l2, s)
	if runs := l2.SweepReportsDue(time.Now()); len(runs) != 1 {
		t.Fatalf("sweep runs = %d, want 1", len(runs))
	}
	got2, err := l2.GetReportSchedule(s.ID)
	if err != nil {
		t.Fatalf("GetReportSchedule: %v", err)
	}
	if got2.Status != ScheduleCompleted {
		t.Fatalf("status after sweep = %s, want completed", got2.Status)
	}
	if _, err := l2.ReviseReportSchedule(s.ID, patch); !errors.Is(err, ErrReportScheduleBadTransition) {
		t.Fatalf("revise completed err = %v, want %v", err, ErrReportScheduleBadTransition)
	}
	got2, _ = l2.GetReportSchedule(s.ID)
	if got2.Interval != 24*time.Hour {
		t.Fatalf("completed schedule moved: interval = %v", got2.Interval)
	}
}

func TestReviseReportSchedulePausedRevisable(t *testing.T) {
	l := New()
	dir := t.TempDir()
	s := validReportSchedule(dir)
	now := time.Now()
	mustCreateReportSchedule(t, l, s)
	if _, err := l.PauseReportSchedule(s.ID); err != nil {
		t.Fatalf("PauseReportSchedule: %v", err)
	}
	// Revise while paused; the first run after resume uses the new
	// interval.
	if _, err := l.ReviseReportSchedule(s.ID, ReportScheduleRevision{Interval: ptrDuration(2 * time.Hour)}); err != nil {
		t.Fatalf("ReviseReportSchedule paused: %v", err)
	}
	if _, err := l.ResumeReportSchedule(s.ID); err != nil {
		t.Fatalf("ResumeReportSchedule: %v", err)
	}
	if runs := l.SweepReportsDue(now); len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("sweep after resume runs = %+v, want one fired run", runs)
	}
	got, err := l.GetReportSchedule(s.ID)
	if err != nil {
		t.Fatalf("GetReportSchedule: %v", err)
	}
	if got.Interval != 2*time.Hour || got.RunSeq != 1 {
		t.Fatalf("after resume run: interval = %v run_seq = %d", got.Interval, got.RunSeq)
	}
	// The advance used the new 2h interval, not the old 24h one.
	if !got.NextRunAt.After(now) || got.NextRunAt.After(now.Add(3*time.Hour)) {
		t.Fatalf("next_run_at after resume run = %v, want within ~2h of now", got.NextRunAt)
	}
}

func TestReviseReportScheduleIntervalKeepsNextRunAt(t *testing.T) {
	l := New()
	dir := t.TempDir()
	now := time.Now()
	s := validReportSchedule(dir)
	s.NextRunAt = now.Add(-time.Hour)
	mustCreateReportSchedule(t, l, s)
	before, err := l.GetReportSchedule(s.ID)
	if err != nil {
		t.Fatalf("GetReportSchedule: %v", err)
	}
	revised, err := l.ReviseReportSchedule(s.ID, ReportScheduleRevision{Interval: ptrDuration(time.Hour)})
	if err != nil {
		t.Fatalf("ReviseReportSchedule: %v", err)
	}
	if !revised.NextRunAt.Equal(before.NextRunAt) {
		t.Fatalf("interval revision moved next_run_at: %v -> %v", before.NextRunAt, revised.NextRunAt)
	}
	if revised.Interval != time.Hour {
		t.Fatalf("interval = %v, want 1h", revised.Interval)
	}
	// The next advance uses the new interval: due at now-1h, a 1h
	// cadence lands the next run at ~now+1h, not ~now+24h.
	if runs := l.SweepReportsDue(now); len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("sweep runs = %+v, want one fired run", runs)
	}
	got, _ := l.GetReportSchedule(s.ID)
	if !got.NextRunAt.After(now) || got.NextRunAt.After(now.Add(61*time.Minute)) {
		t.Fatalf("next_run_at after run = %v, want within ~1h of now", got.NextRunAt)
	}
}

func TestReviseReportScheduleNextRunAtMovesTrigger(t *testing.T) {
	l := New()
	dir := t.TempDir()
	now := time.Now()
	s := validReportSchedule(dir)
	s.NextRunAt = now.Add(24 * time.Hour) // not due for a day
	mustCreateReportSchedule(t, l, s)
	if runs := l.SweepReportsDue(now); len(runs) != 0 {
		t.Fatalf("sweep before revision runs = %d, want 0", len(runs))
	}
	// Move the trigger into the past: the next sweep fires at once.
	past := now.Add(-time.Hour)
	if _, err := l.ReviseReportSchedule(s.ID, ReportScheduleRevision{NextRunAt: ptrTime(past)}); err != nil {
		t.Fatalf("ReviseReportSchedule: %v", err)
	}
	if runs := l.SweepReportsDue(now); len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("sweep after revision runs = %+v, want one fired run", runs)
	}
	got, _ := l.GetReportSchedule(s.ID)
	if got.RunSeq != 1 {
		t.Fatalf("run_seq = %d, want 1", got.RunSeq)
	}
}

func TestReviseReportScheduleEndsAt(t *testing.T) {
	l := New()
	dir := t.TempDir()
	now := time.Now()
	s := validReportSchedule(dir)
	s.NextRunAt = now.Add(time.Hour)
	s.EndsAt = now.Add(48 * time.Hour)
	mustCreateReportSchedule(t, l, s)

	// Tightening EndsAt to at or before the effective NextRunAt is
	// rejected by the creation gate; the live schedule is untouched.
	for _, bad := range []time.Time{now, now.Add(time.Hour)} {
		if _, err := l.ReviseReportSchedule(s.ID, ReportScheduleRevision{EndsAt: ptrTime(bad)}); !errors.Is(err, ErrReportScheduleBadEndsAt) {
			t.Fatalf("EndsAt %v err = %v, want %v", bad, err, ErrReportScheduleBadEndsAt)
		}
	}
	got, _ := l.GetReportSchedule(s.ID)
	if !got.EndsAt.Equal(s.EndsAt) {
		t.Fatalf("rejected EndsAt revision moved ends_at to %v", got.EndsAt)
	}

	// Widening extends the lifetime: the schedule survives a sweep
	// past the old EndsAt and completes only past the new one.
	if _, err := l.ReviseReportSchedule(s.ID, ReportScheduleRevision{EndsAt: ptrTime(now.Add(96 * time.Hour))}); err != nil {
		t.Fatalf("widen EndsAt: %v", err)
	}
	// First sweep at now+49h: NextRunAt (now+1h) is due and the run
	// advances to now+73h — past the old EndsAt, before the new one.
	l.SweepReportsDue(now.Add(49 * time.Hour))
	got, _ = l.GetReportSchedule(s.ID)
	if got.Status != ScheduleActive {
		t.Fatalf("status after sweep past old ends_at = %s, want active", got.Status)
	}
	// Second sweep far past the new EndsAt completes the schedule.
	l.SweepReportsDue(now.Add(200 * time.Hour))
	got, _ = l.GetReportSchedule(s.ID)
	if got.Status != ScheduleCompleted {
		t.Fatalf("status after sweep past new ends_at = %s, want completed", got.Status)
	}
}

func TestReviseReportScheduleEndsAtClears(t *testing.T) {
	l := New()
	dir := t.TempDir()
	s := validReportSchedule(dir)
	s.EndsAt = time.Now().Add(48 * time.Hour)
	mustCreateReportSchedule(t, l, s)
	revised, err := l.ReviseReportSchedule(s.ID, ReportScheduleRevision{EndsAt: ptrTime(time.Time{})})
	if err != nil {
		t.Fatalf("clear EndsAt: %v", err)
	}
	if !revised.EndsAt.IsZero() {
		t.Fatalf("ends_at = %v, want zero (cleared)", revised.EndsAt)
	}
}

func TestReviseReportScheduleParamsValidation(t *testing.T) {
	l := New()
	dir := t.TempDir()
	// Statement schedule: dropping the account is rejected.
	stmt := validReportSchedule(dir)
	stmt.ID = "monthly-statement"
	stmt.Kind = ReportKindStatement
	stmt.Params = ReportParams{Account: "cash"}
	mustCreateReportSchedule(t, l, stmt)
	if _, err := l.ReviseReportSchedule(stmt.ID, ReportScheduleRevision{Params: ptrReportParams(ReportParams{})}); !errors.Is(err, ErrReportScheduleBadParams) {
		t.Fatalf("statement without account err = %v, want %v", err, ErrReportScheduleBadParams)
	}
	got, _ := l.GetReportSchedule(stmt.ID)
	if got.Params.Account != "cash" {
		t.Fatalf("rejected params revision moved account to %q", got.Params.Account)
	}

	// Reconcile schedule: an invalid base currency is rejected.
	rec := validReportSchedule(dir)
	rec.ID = "daily-reconcile-2"
	mustCreateReportSchedule(t, l, rec)
	if _, err := l.ReviseReportSchedule(rec.ID, ReportScheduleRevision{Params: ptrReportParams(ReportParams{BaseCurrency: "usd"})}); !errors.Is(err, ErrReportScheduleBadParams) {
		t.Fatalf("bad base currency err = %v, want %v", err, ErrReportScheduleBadParams)
	}
	got, _ = l.GetReportSchedule(rec.ID)
	if got.Params.BaseCurrency != "" {
		t.Fatalf("rejected params revision moved base_currency to %q", got.Params.BaseCurrency)
	}

	// A valid params revision lands.
	if _, err := l.ReviseReportSchedule(rec.ID, ReportScheduleRevision{Params: ptrReportParams(ReportParams{BaseCurrency: "USD"})}); err != nil {
		t.Fatalf("valid params revision: %v", err)
	}
	got, _ = l.GetReportSchedule(rec.ID)
	if got.Params.BaseCurrency != "USD" {
		t.Fatalf("base_currency = %q, want USD", got.Params.BaseCurrency)
	}
}

func TestReviseReportScheduleSinkValidation(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	dir := t.TempDir()
	s := validReportSchedule(dir)
	mustCreateReportSchedule(t, l, s)

	// dir -> URL without a secret is rejected; the live sink is
	// untouched.
	if _, err := l.ReviseReportSchedule(s.ID, ReportScheduleRevision{Sink: ptrReportSink(ReportSink{URL: "https://example.com/hook"})}); !errors.Is(err, ErrReportScheduleBadSink) {
		t.Fatalf("URL without secret err = %v, want %v", err, ErrReportScheduleBadSink)
	}
	got, _ := l.GetReportSchedule(s.ID)
	if got.Sink.Dir == "" || got.Sink.URL != "" {
		t.Fatalf("rejected sink revision moved sink to %+v", got.Sink)
	}

	// dir -> URL with a secret lands, and the secret appears nowhere
	// in the audit trail or in the serialized schedule.
	revised, err := l.ReviseReportSchedule(s.ID, ReportScheduleRevision{Sink: ptrReportSink(ReportSink{URL: "https://example.com/hook", Secret: "s3cr3t"})})
	if err != nil {
		t.Fatalf("URL sink with secret: %v", err)
	}
	if revised.Sink.URL != "https://example.com/hook" {
		t.Fatalf("sink URL = %q, want the webhook", revised.Sink.URL)
	}
	events := revisedReportScheduleEvents(t, al)
	if len(events) != 1 {
		t.Fatalf("report_schedule_revised events = %d, want 1", len(events))
	}
	detailsJSON, err := json.Marshal(events[0].Details)
	if err != nil {
		t.Fatalf("marshal audit details: %v", err)
	}
	if strings.Contains(string(detailsJSON), "s3cr3t") {
		t.Fatalf("audit details leak the sink secret: %s", detailsJSON)
	}
	if events[0].Details["previous_sink"] != "dir" || events[0].Details["current_sink"] != "url" {
		t.Fatalf("sink audit details = %v, want dir -> url", events[0].Details)
	}
	raw, err := json.Marshal(revised)
	if err != nil {
		t.Fatalf("marshal revised schedule: %v", err)
	}
	if strings.Contains(string(raw), "s3cr3t") {
		t.Fatalf("serialized schedule leaks the sink secret: %s", raw)
	}
}

func TestReviseReportScheduleBadInterval(t *testing.T) {
	l := New()
	dir := t.TempDir()
	s := validReportSchedule(dir)
	mustCreateReportSchedule(t, l, s)
	for _, bad := range []time.Duration{0, -time.Hour} {
		if _, err := l.ReviseReportSchedule(s.ID, ReportScheduleRevision{Interval: ptrDuration(bad)}); !errors.Is(err, ErrReportScheduleBadInterval) {
			t.Fatalf("interval %v err = %v, want %v", bad, err, ErrReportScheduleBadInterval)
		}
	}
	// A zero NextRunAt is rejected the same way.
	if _, err := l.ReviseReportSchedule(s.ID, ReportScheduleRevision{NextRunAt: ptrTime(time.Time{})}); !errors.Is(err, ErrReportScheduleBadNextRunAt) {
		t.Fatalf("zero next_run_at err = %v, want %v", err, ErrReportScheduleBadNextRunAt)
	}
	got, _ := l.GetReportSchedule(s.ID)
	if got.Interval != 24*time.Hour || got.NextRunAt.IsZero() {
		t.Fatalf("rejected revisions moved the live schedule: %+v", got)
	}
}

func TestReviseReportScheduleNoOp(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	dir := t.TempDir()
	s := validReportSchedule(dir)
	mustCreateReportSchedule(t, l, s)
	before, _ := l.GetReportSchedule(s.ID)

	// An empty patch is a no-op.
	if got, err := l.ReviseReportSchedule(s.ID, ReportScheduleRevision{}); err != nil || got.ID != s.ID {
		t.Fatalf("empty patch: got %+v err %v", got, err)
	}
	// A patch identical to the live values is a no-op.
	same := ReportScheduleRevision{
		Interval:  ptrDuration(before.Interval),
		NextRunAt: ptrTime(before.NextRunAt),
		Params:    ptrReportParams(before.Params),
		Sink:      ptrReportSink(before.Sink),
	}
	if got, err := l.ReviseReportSchedule(s.ID, same); err != nil || got.Interval != before.Interval {
		t.Fatalf("same-values patch: got %+v err %v", got, err)
	}
	if events := revisedReportScheduleEvents(t, al); len(events) != 0 {
		t.Fatalf("no-op revisions wrote %d audit events, want 0", len(events))
	}
}

func TestReviseReportScheduleCtxOperatorAndAuditDetails(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	dir := t.TempDir()
	s := validReportSchedule(dir)
	before := l.Version()
	mustCreateReportSchedule(t, l, s)
	live, _ := l.GetReportSchedule(s.ID)

	newNext := live.NextRunAt.Add(48 * time.Hour)
	ctx := ContextWithActor(context.Background(), "ops-ana")
	revised, err := l.ReviseReportScheduleCtx(ctx, s.ID, ReportScheduleRevision{
		Interval:  ptrDuration(48 * time.Hour),
		NextRunAt: ptrTime(newNext),
	})
	if err != nil {
		t.Fatalf("ReviseReportScheduleCtx: %v", err)
	}
	if revised.Interval != 48*time.Hour || !revised.NextRunAt.Equal(newNext) {
		t.Fatalf("revised schedule = %+v", revised)
	}
	// The plain variant keeps the entrypoint name as the actor.
	if _, err := l.ReviseReportSchedule(s.ID, ReportScheduleRevision{Interval: ptrDuration(72 * time.Hour)}); err != nil {
		t.Fatalf("ReviseReportSchedule: %v", err)
	}
	events := revisedReportScheduleEvents(t, al)
	if len(events) != 2 {
		t.Fatalf("report_schedule_revised events = %d, want 2", len(events))
	}
	if events[0].Actor != "ops-ana" {
		t.Errorf("Ctx revision actor = %q, want ops-ana", events[0].Actor)
	}
	if events[1].Actor != "ReviseReportSchedule" {
		t.Errorf("plain revision actor = %q, want ReviseReportSchedule", events[1].Actor)
	}
	if events[0].TraceID != s.ID || events[0].Details["schedule_id"] != s.ID {
		t.Errorf("event identity = %q / %v, want %s", events[0].TraceID, events[0].Details["schedule_id"], s.ID)
	}
	if events[0].Details["previous_interval"] != "24h0m0s" || events[0].Details["current_interval"] != "48h0m0s" {
		t.Errorf("interval audit details = %v, want 24h0m0s -> 48h0m0s", events[0].Details)
	}
	if events[0].Details["previous_next_run_at"] != live.NextRunAt.UTC().Format(time.RFC3339) ||
		events[0].Details["current_next_run_at"] != newNext.UTC().Format(time.RFC3339) {
		t.Errorf("next_run_at audit details = %v", events[0].Details)
	}
	if events[0].Details["kind"] != "reconcile" {
		t.Errorf("kind audit detail = %v, want reconcile", events[0].Details["kind"])
	}
	if _, ok := events[0].Details["previous_params"]; ok {
		t.Errorf("unchanged params should not appear in audit details: %v", events[0].Details)
	}
	if events[0].VersionAfter != events[0].VersionBefore || events[0].VersionBefore != before {
		t.Errorf("revision bumped version: %d -> %d (was %d)", events[0].VersionBefore, events[0].VersionAfter, before)
	}
	if l.Version() != before {
		t.Errorf("ledger version moved across revisions: %d -> %d", before, l.Version())
	}
}

func TestReviseReportSchedulePreservesRunSeqAndHistory(t *testing.T) {
	l := New()
	dir := t.TempDir()
	now := time.Now()
	s := validReportSchedule(dir)
	s.NextRunAt = now.Add(-time.Hour)
	mustCreateReportSchedule(t, l, s)
	if runs := l.SweepReportsDue(now); len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("sweep runs = %+v, want one fired run", runs)
	}
	before, _ := l.GetReportSchedule(s.ID)
	runsBefore, err := l.ListReportRuns(s.ID, 0)
	if err != nil {
		t.Fatalf("ListReportRuns: %v", err)
	}
	if _, err := l.ReviseReportSchedule(s.ID, ReportScheduleRevision{
		Interval: ptrDuration(48 * time.Hour),
		Params:   ptrReportParams(ReportParams{BaseCurrency: "USD"}),
	}); err != nil {
		t.Fatalf("ReviseReportSchedule: %v", err)
	}
	got, _ := l.GetReportSchedule(s.ID)
	if got.RunSeq != before.RunSeq || !got.NextRunAt.Equal(before.NextRunAt) || !got.CreatedAt.Equal(before.CreatedAt) {
		t.Fatalf("revision moved run state: before %+v after %+v", before, got)
	}
	runsAfter, err := l.ListReportRuns(s.ID, 0)
	if err != nil {
		t.Fatalf("ListReportRuns: %v", err)
	}
	if len(runsAfter) != len(runsBefore) || runsAfter[0] != runsBefore[0] {
		t.Fatalf("revision changed run history: before %+v after %+v", runsBefore, runsAfter)
	}
}

func TestReviseReportScheduleConcurrentWithSweep(t *testing.T) {
	l := New()
	dir := t.TempDir()
	s := validReportSchedule(dir)
	mustCreateReportSchedule(t, l, s)
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				iv := time.Duration(1+g) * time.Hour
				if _, err := l.ReviseReportSchedule(s.ID, ReportScheduleRevision{Interval: ptrDuration(iv)}); err != nil {
					t.Errorf("concurrent revise: %v", err)
					return
				}
			}
		}(g)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				l.SweepReportsDue(time.Now())
			}
		}()
	}
	wg.Wait()
	got, err := l.GetReportSchedule(s.ID)
	if err != nil {
		t.Fatalf("GetReportSchedule: %v", err)
	}
	if got.Interval <= 0 || got.Interval > 4*time.Hour {
		t.Fatalf("final interval = %v, want one of the revised values", got.Interval)
	}
	if got.Status != ScheduleActive && got.Status != ScheduleCompleted {
		t.Fatalf("final status = %s", got.Status)
	}
}
