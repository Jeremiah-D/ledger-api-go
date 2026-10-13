package ledger

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// validReportSchedule returns a minimal valid reconcile schedule with a
// dir sink, due now.
func validReportSchedule(dir string) ReportSchedule {
	return ReportSchedule{
		ID:        "daily-reconcile",
		Kind:      ReportKindReconcile,
		Interval:  24 * time.Hour,
		NextRunAt: time.Now().Add(-time.Hour),
		Sink:      ReportSink{Dir: dir},
	}
}

func mustCreateReportSchedule(t *testing.T, l *Ledger, s ReportSchedule) ReportSchedule {
	t.Helper()
	rec, dup, err := l.CreateReportSchedule(s)
	if err != nil {
		t.Fatalf("CreateReportSchedule: %v", err)
	}
	if dup {
		t.Fatalf("CreateReportSchedule: unexpected duplicate")
	}
	return rec
}

func TestReportScheduleValidation(t *testing.T) {
	dir := t.TempDir()
	base := validReportSchedule(dir)
	cases := []struct {
		name    string
		mutate  func(*ReportSchedule)
		wantErr error
	}{
		{"empty ID", func(s *ReportSchedule) { s.ID = "" }, ErrReportScheduleEmptyID},
		{"bad kind", func(s *ReportSchedule) { s.Kind = "fortune" }, ErrReportScheduleBadKind},
		{"no sink", func(s *ReportSchedule) { s.Sink = ReportSink{} }, ErrReportScheduleBadSink},
		{"both sinks", func(s *ReportSchedule) {
			s.Sink = ReportSink{URL: "https://example.com/hook", Secret: "s3cr3t", Dir: dir}
		}, ErrReportScheduleBadSink},
		{"URL without secret", func(s *ReportSchedule) {
			s.Sink = ReportSink{URL: "https://example.com/hook"}
		}, ErrReportScheduleBadSink},
		{"bad URL", func(s *ReportSchedule) {
			s.Sink = ReportSink{URL: "notaurl", Secret: "s3cr3t"}
		}, ErrReportScheduleBadSink},
		{"non-http URL", func(s *ReportSchedule) {
			s.Sink = ReportSink{URL: "ftp://example.com/hook", Secret: "s3cr3t"}
		}, ErrReportScheduleBadSink},
		{"unsafe ID with dir sink", func(s *ReportSchedule) { s.ID = "a/b" }, ErrReportScheduleUnsafeID},
		{"bad interval", func(s *ReportSchedule) { s.Interval = 0 }, ErrReportScheduleBadInterval},
		{"zero next_run_at", func(s *ReportSchedule) { s.NextRunAt = time.Time{} }, ErrReportScheduleBadNextRunAt},
		{"ends_at before next_run_at", func(s *ReportSchedule) {
			s.EndsAt = s.NextRunAt.Add(-time.Hour)
		}, ErrReportScheduleBadEndsAt},
		{"statement missing account", func(s *ReportSchedule) {
			s.Kind = ReportKindStatement
		}, ErrReportScheduleBadParams},
		{"statement bad from", func(s *ReportSchedule) {
			s.Kind = ReportKindStatement
			s.Params.Account = "cash"
			s.Params.From = "not-a-time"
		}, ErrReportScheduleBadParams},
		{"statement from after to", func(s *ReportSchedule) {
			s.Kind = ReportKindStatement
			s.Params.Account = "cash"
			s.Params.From = "2026-02-01T00:00:00Z"
			s.Params.To = "2026-01-01T00:00:00Z"
		}, ErrReportScheduleBadParams},
		{"reconcile bad base currency", func(s *ReportSchedule) {
			s.Params.BaseCurrency = "usd"
		}, ErrReportScheduleBadParams},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := New()
			s := base
			tc.mutate(&s)
			if _, _, err := l.CreateReportSchedule(s); !errors.Is(err, tc.wantErr) {
				t.Fatalf("CreateReportSchedule err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestReportScheduleCreateIdempotentAndConflict(t *testing.T) {
	l := New()
	dir := t.TempDir()
	s := validReportSchedule(dir)
	s.ID = "r1"
	s.IdempotencyKey = "key-1"
	rec := mustCreateReportSchedule(t, l, s)

	s2 := s
	s2.ID = "r2" // same key, different ID: replay wins
	rec2, dup, err := l.CreateReportSchedule(s2)
	if err != nil || !dup {
		t.Fatalf("replay: dup=%v err=%v", dup, err)
	}
	if rec2.ID != rec.ID {
		t.Fatalf("replay returned schedule %q, want %q", rec2.ID, rec.ID)
	}

	s3 := s
	s3.IdempotencyKey = "key-2" // same ID, different key: conflict
	if _, _, err := l.CreateReportSchedule(s3); !errors.Is(err, ErrReportScheduleIDConflict) {
		t.Fatalf("conflict err = %v, want %v", err, ErrReportScheduleIDConflict)
	}
}

func TestReportScheduleLifecycle(t *testing.T) {
	l := New()
	s := validReportSchedule(t.TempDir())
	s.ID = "lifecycle"
	mustCreateReportSchedule(t, l, s)

	if _, err := l.PauseReportSchedule("lifecycle"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if _, err := l.PauseReportSchedule("lifecycle"); !errors.Is(err, ErrReportScheduleBadTransition) {
		t.Fatalf("double pause err = %v, want %v", err, ErrReportScheduleBadTransition)
	}
	if _, err := l.ResumeReportSchedule("lifecycle"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if _, err := l.ResumeReportSchedule("lifecycle"); !errors.Is(err, ErrReportScheduleBadTransition) {
		t.Fatalf("double resume err = %v, want %v", err, ErrReportScheduleBadTransition)
	}
	got, err := l.GetReportSchedule("lifecycle")
	if err != nil || got.Status != ScheduleActive {
		t.Fatalf("status = %q, err = %v", got.Status, err)
	}
	if _, err := l.CancelReportSchedule("lifecycle"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := l.CancelReportSchedule("lifecycle"); !errors.Is(err, ErrReportScheduleBadTransition) {
		t.Fatalf("double cancel err = %v, want %v", err, ErrReportScheduleBadTransition)
	}
	if _, err := l.PauseReportSchedule("lifecycle"); !errors.Is(err, ErrReportScheduleBadTransition) {
		t.Fatalf("pause cancelled err = %v, want %v", err, ErrReportScheduleBadTransition)
	}
	if _, err := l.GetReportSchedule("nope"); !errors.Is(err, ErrReportScheduleNotFound) {
		t.Fatalf("get unknown err = %v, want %v", err, ErrReportScheduleNotFound)
	}
}

func TestReportScheduleJSONRoundTrip(t *testing.T) {
	s := validReportSchedule(t.TempDir())
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"interval":"24h0m0s"`) {
		t.Fatalf("interval not rendered as duration string: %s", raw)
	}
	var back ReportSchedule
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Interval != 24*time.Hour {
		t.Fatalf("interval = %v, want 24h", back.Interval)
	}
	// The signing secret never serializes.
	s.Sink.Secret = "s3cr3t"
	raw, _ = json.Marshal(s)
	if strings.Contains(string(raw), "s3cr3t") {
		t.Fatalf("secret leaked into marshaled schedule: %s", raw)
	}
}

// postSomeEntries books two entries so the reconcile report is non-trivial.
func postSomeEntries(t *testing.T, l *Ledger) {
	t.Helper()
	for _, e := range []JournalEntry{
		{ID: "e1", DebitAccount: "cash", CreditAccount: "rev", AmountCents: 1000},
		{ID: "e2", DebitAccount: "cash", CreditAccount: "rev", AmountCents: 2500},
	} {
		if _, _, err := l.Post(e); err != nil {
			t.Fatalf("Post: %v", err)
		}
	}
}

func TestSweepReportsDueDirSinkReconcile(t *testing.T) {
	dir := t.TempDir()
	l := New()
	postSomeEntries(t, l)
	versionBefore := l.Version()
	_, linksBefore := l.ChainHead()

	s := validReportSchedule(dir)
	mustCreateReportSchedule(t, l, s)

	now := time.Now()
	runs := l.SweepReportsDue(now)
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	run := runs[0]
	if run.Status != "fired" || run.Error != "" {
		t.Fatalf("run = %+v, want fired", run)
	}
	if run.Kind != ReportKindReconcile {
		t.Fatalf("kind = %q", run.Kind)
	}
	if run.RunSeq != 1 {
		t.Fatalf("run_seq = %d, want 1", run.RunSeq)
	}

	path := filepath.Join(dir, "daily-reconcile-1.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read report file: %v", err)
	}
	var report ReconciliationReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatalf("unmarshal report: %v", err)
	}
	if len(report.CurrencyTotals) == 0 {
		t.Fatalf("reconcile report has no currency totals")
	}
	if int64(len(raw)) != run.ReportBytes {
		t.Fatalf("report_bytes = %d, file = %d", run.ReportBytes, len(raw))
	}

	// Zero write impact: version and chain links are untouched by the sweep.
	if got := l.Version(); got != versionBefore {
		t.Fatalf("version = %d, want %d (unchanged)", got, versionBefore)
	}
	if _, links := l.ChainHead(); links != linksBefore {
		t.Fatalf("chain links = %d, want %d (unchanged)", links, linksBefore)
	}

	// The schedule advanced past now and the run is in history.
	got, err := l.GetReportSchedule("daily-reconcile")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.RunSeq != 1 || !got.NextRunAt.After(now) {
		t.Fatalf("schedule not advanced: %+v", got)
	}
	hist, err := l.ListReportRuns("daily-reconcile", 0)
	if err != nil || len(hist) != 1 || hist[0].RunSeq != 1 {
		t.Fatalf("history = %+v, err = %v", hist, err)
	}

	// A second sweep finds nothing due.
	if runs := l.SweepReportsDue(now); len(runs) != 0 {
		t.Fatalf("second sweep fired %d runs", len(runs))
	}
}

func TestSweepReportsDueDirSinkStatement(t *testing.T) {
	dir := t.TempDir()
	l := New()
	postSomeEntries(t, l)

	s := validReportSchedule(dir)
	s.ID = "stmt-monthly"
	s.Kind = ReportKindStatement
	s.Params.Account = "cash"
	mustCreateReportSchedule(t, l, s)

	runs := l.SweepReportsDue(time.Now())
	if len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("runs = %+v", runs)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "stmt-monthly-1.json"))
	if err != nil {
		t.Fatalf("read report file: %v", err)
	}
	var stmt AccountStatement
	if err := json.Unmarshal(raw, &stmt); err != nil {
		t.Fatalf("unmarshal statement: %v", err)
	}
	if stmt.Account != "cash" {
		t.Fatalf("statement account = %q, want cash", stmt.Account)
	}
	if len(stmt.Entries) != 2 {
		t.Fatalf("statement entries = %d, want 2", len(stmt.Entries))
	}
}

func TestSweepReportsDueURLSink(t *testing.T) {
	const secret = "report-webhook-secret"
	var mu sync.Mutex
	var gotSig, gotType, gotSched, gotRun string
	var gotBody []byte
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		gotSig, gotType, gotSched, gotRun = r.Header.Get("X-Ledger-Signature"),
			r.Header.Get("X-Ledger-Event-Type"),
			r.Header.Get("X-Ledger-Report-Schedule"),
			r.Header.Get("X-Ledger-Report-Run")
		gotBody = body
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	l := New()
	postSomeEntries(t, l)
	s := validReportSchedule(t.TempDir())
	s.ID = "webhook-daily"
	s.Sink = ReportSink{URL: srv.URL, Secret: secret}
	mustCreateReportSchedule(t, l, s)

	runs := l.SweepReportsDue(time.Now())
	if len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("runs = %+v", runs)
	}
	mu.Lock()
	sig, typ, sched, run, body := gotSig, gotType, gotSched, gotRun, gotBody
	mu.Unlock()
	if typ != "report_generated" || sched != "webhook-daily" || run != "1" {
		t.Fatalf("headers: type=%q sched=%q run=%q", typ, sched, run)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	if want := "sha256=" + hex.EncodeToString(mac.Sum(nil)); sig != want {
		t.Fatalf("signature mismatch: got %q want %q", sig, want)
	}
	var report ReconciliationReport
	if err := json.Unmarshal(body, &report); err != nil {
		t.Fatalf("webhook body not a reconcile report: %v", err)
	}

	// Delivery failure: the run fails, NextRunAt stays put, the next
	// sweep retries the same run sequence.
	mu.Lock()
	fail = true
	mu.Unlock()
	s2 := validReportSchedule(t.TempDir())
	s2.ID = "webhook-flaky"
	s2.NextRunAt = time.Now().Add(-time.Hour)
	s2.Sink = ReportSink{URL: srv.URL, Secret: secret}
	mustCreateReportSchedule(t, l, s2)
	runs = l.SweepReportsDue(time.Now())
	if len(runs) != 1 || runs[0].Status != "failed" {
		t.Fatalf("runs = %+v, want one failed run", runs)
	}
	if !strings.Contains(runs[0].Error, "status 500") {
		t.Fatalf("run error = %q, want status 500", runs[0].Error)
	}
	got, _ := l.GetReportSchedule("webhook-flaky")
	if got.RunSeq != 0 {
		t.Fatalf("failed run advanced RunSeq to %d", got.RunSeq)
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	runs = l.SweepReportsDue(time.Now())
	if len(runs) != 1 || runs[0].Status != "fired" || runs[0].RunSeq != 1 {
		t.Fatalf("retry runs = %+v", runs)
	}
}

func TestSweepReportsDueSkipsMissed(t *testing.T) {
	dir := t.TempDir()
	l := New()
	postSomeEntries(t, l)
	now := time.Now()
	s := validReportSchedule(dir)
	s.ID = "catchup"
	// Five intervals behind: the sweep fires once and counts the four
	// skipped nominal runs (the due one is not "skipped").
	s.NextRunAt = now.Add(-5 * 24 * time.Hour)
	mustCreateReportSchedule(t, l, s)

	runs := l.SweepReportsDue(now)
	if len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("runs = %+v", runs)
	}
	if runs[0].SkippedRuns != 5 {
		t.Fatalf("skipped = %d, want 5", runs[0].SkippedRuns)
	}
	got, _ := l.GetReportSchedule("catchup")
	if !got.NextRunAt.After(now) {
		t.Fatalf("NextRunAt not advanced past now: %v", got.NextRunAt)
	}
}

func TestSweepReportsDueEndsAt(t *testing.T) {
	l := New()
	now := time.Now()
	// A due nominal run inside the plan window still fires; the
	// schedule completes once NextRunAt advances past EndsAt. (A
	// schedule can never be created already-expired: creation
	// fail-fasts on ends_at <= next_run_at, so the sweep's
	// "expired before this run" branch is defensive, mirroring LG-41.)
	s2 := validReportSchedule(t.TempDir())
	s2.ID = "last-run"
	s2.NextRunAt = now.Add(-2 * time.Hour)
	s2.EndsAt = now.Add(-time.Hour)
	mustCreateReportSchedule(t, l, s2)
	runs := l.SweepReportsDue(now)
	if len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("runs = %+v, want one fired run", runs)
	}
	got2, _ := l.GetReportSchedule("last-run")
	if got2.Status != ScheduleCompleted {
		t.Fatalf("status = %q, want completed", got2.Status)
	}
	if runs := l.SweepReportsDue(now.Add(time.Hour)); len(runs) != 0 {
		t.Fatalf("completed schedule fired %d runs", len(runs))
	}
}

func TestSweepReportsDueConcurrent(t *testing.T) {
	l := New()
	postSomeEntries(t, l)
	dir := t.TempDir()
	s := validReportSchedule(dir)
	s.ID = "concurrent"
	s.NextRunAt = time.Now().Add(-time.Hour)
	mustCreateReportSchedule(t, l, s)

	var wg sync.WaitGroup
	results := make([][]ReportRun, 4)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = l.SweepReportsDue(time.Now())
		}(i)
	}
	wg.Wait()
	fired := 0
	for _, r := range results {
		fired += len(r)
	}
	if fired != 1 {
		t.Fatalf("concurrent sweeps fired %d runs, want 1", fired)
	}
	got, _ := l.GetReportSchedule("concurrent")
	if got.RunSeq != 1 {
		t.Fatalf("RunSeq = %d, want 1", got.RunSeq)
	}
}

func TestReportSweeper(t *testing.T) {
	if sw := StartReportSweeper(context.Background(), New(), 0, nil); sw != nil {
		t.Fatalf("non-positive interval must disable the sweeper")
	}
	l := New()
	postSomeEntries(t, l)
	dir := t.TempDir()
	s := validReportSchedule(dir)
	s.ID = "swept"
	s.Interval = 50 * time.Millisecond
	s.NextRunAt = time.Now().Add(-time.Second)
	mustCreateReportSchedule(t, l, s)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	generated := 0
	sw := StartReportSweeper(ctx, l, 20*time.Millisecond, func(g, f int) {
		mu.Lock()
		defer mu.Unlock()
		generated += g
	})
	if sw == nil {
		t.Fatalf("sweeper not started")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		n := generated
		ticks := sw.Ticks()
		mu.Unlock()
		if n >= 1 && ticks >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sweeper never fired (generated=%d ticks=%d)", n, ticks)
		}
		time.Sleep(10 * time.Millisecond)
	}
	sw.Stop()
	sw.Stop() // idempotent
}

func TestReportScheduleAuditEvents(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	postSomeEntries(t, l)

	reportDir := t.TempDir()
	s := validReportSchedule(reportDir)
	mustCreateReportSchedule(t, l, s)
	l.SweepReportsDue(time.Now())
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	events, corrupt, err := ReadAuditLog(dir, time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt = %d", corrupt)
	}
	ops := map[string]string{}
	for _, ev := range events {
		ops[ev.Op] = ev.Actor
	}
	if ops["report_schedule_created"] != "CreateReportSchedule" {
		t.Fatalf("create event: %v", ops)
	}
	if ops["report_generated"] != "SweepReportsDue" {
		t.Fatalf("generated event: %v", ops)
	}
}

func TestReportScheduleRunHistoryBounded(t *testing.T) {
	l := New()
	s := validReportSchedule(t.TempDir())
	rec := mustCreateReportSchedule(t, l, s)
	l.mu.Lock()
	for i := 0; i < maxReportRuns+25; i++ {
		l.recordReportRunLocked(l.reportSchedules[rec.ID], ReportRun{ScheduleID: rec.ID, RunSeq: uint64(i + 1), Status: "fired"})
	}
	l.mu.Unlock()
	hist, err := l.ListReportRuns(rec.ID, 0)
	if err != nil {
		t.Fatalf("ListReportRuns: %v", err)
	}
	if len(hist) != maxReportRuns {
		t.Fatalf("history = %d, want %d", len(hist), maxReportRuns)
	}
	if hist[0].RunSeq != uint64(maxReportRuns+25) {
		t.Fatalf("newest run seq = %d", hist[0].RunSeq)
	}
}

func TestValidateReportScheduleRecord(t *testing.T) {
	good := validReportSchedule(t.TempDir())
	good.Status = ScheduleActive
	if err := validateReportScheduleRecord(&good); err != nil {
		t.Fatalf("valid record: %v", err)
	}
	bad := good
	bad.Kind = "nope"
	if err := validateReportScheduleRecord(&bad); !errors.Is(err, ErrReportScheduleBadKind) {
		t.Fatalf("bad kind: %v", err)
	}
	bad = good
	bad.Status = "weird"
	if err := validateReportScheduleRecord(&bad); err == nil {
		t.Fatalf("bad status accepted")
	}
}
