package ledger

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Scheduled reports (LG-72).
//
// A ReportSchedule is a recurring report subscription — compliance packs,
// client statements, treasury snapshots: every Interval, starting at
// NextRunAt, the ledger generates one report (ReconcileWithOptions or
// GenerateStatement) read-only and delivers it to the schedule's sink.
//
// Generation is read-only against financial state: no version bump, no
// journal entries, no audit-chain links. The operational audit trail
// still records report_generated / report_run_failed (like every other
// operator automation), and Reconcile itself emits its own reconcile
// audit event when it runs — the "zero write impact" promise covers the
// ledger's money state, not the observability trail.
//
// Delivery targets:
//   - URL sink: one signed HTTPS POST per sweep, HMAC-SHA256(secret,
//     body) as X-Ledger-Signature: sha256=<hex> — the exact LG-67
//     convention, so the same receiver infrastructure works. The secret
//     is mandatory with a URL: unsigned webhooks can be forged, so
//     creation fails fast without one. One attempt per sweep; a failed
//     delivery records the run as failed and the next sweep retries it
//     with NextRunAt unchanged, mirroring the LG-41 transfer-schedule
//     failure semantics. URL delivery is at-least-once: a crash between
//     a 2xx and the run-state advance replays the same report on the
//     next sweep.
//   - Dir sink: the report JSON is written atomically (temp file +
//     rename) to <dir>/<schedule-id>-<run-seq>.json. Retries overwrite
//     the same path, so dir delivery is idempotent by construction.
//
// Missed runs are skipped, not caught up (LG-41 convention): the sweep
// fires one run and advances NextRunAt arithmetically past now, counting
// the skipped nominal runs on the run record. Run history is bounded to
// the newest 100 runs per schedule; history is operational state.
//
// Report schedules are process-lifetime operational state: they do not
// survive disaster-recovery snapshots (LG-24) — the backlog for this
// item does not ask for persistence, and baking unrequested restore
// semantics into the snapshot contract would be invention. The schedule
// definition (not its run history) is what an operator recreates after
// a restore.

var (
	// ErrReportScheduleEmptyID is returned when a report schedule is
	// created with an empty ID.
	ErrReportScheduleEmptyID = errors.New("ledger: report schedule ID must not be empty")
	// ErrReportScheduleIDConflict is returned when a report schedule ID
	// is already in use.
	ErrReportScheduleIDConflict = errors.New("ledger: report schedule ID already exists")
	// ErrReportScheduleBadKind is returned when the report kind is not
	// "reconcile" or "statement".
	ErrReportScheduleBadKind = errors.New("ledger: report kind must be \"reconcile\" or \"statement\"")
	// ErrReportScheduleBadParams is returned when the schedule params
	// do not fit the report kind.
	ErrReportScheduleBadParams = errors.New("ledger: report schedule params invalid for report kind")
	// ErrReportScheduleBadSink is returned when the delivery sink is not
	// exactly one of {URL+Secret, Dir}.
	ErrReportScheduleBadSink = errors.New("ledger: report sink must be exactly one of a signed URL or a directory")
	// ErrReportScheduleBadInterval is returned when the schedule
	// interval is not positive.
	ErrReportScheduleBadInterval = errors.New("ledger: report schedule interval must be positive")
	// ErrReportScheduleBadNextRunAt is returned when a report schedule
	// has no next run time.
	ErrReportScheduleBadNextRunAt = errors.New("ledger: report schedule next_run_at must be set")
	// ErrReportScheduleBadEndsAt is returned when a report schedule's
	// ends_at is set but does not fall after its next_run_at.
	ErrReportScheduleBadEndsAt = errors.New("ledger: report schedule ends_at must be after next_run_at")
	// ErrReportScheduleNotFound is returned when a report schedule ID
	// names no known schedule.
	ErrReportScheduleNotFound = errors.New("ledger: report schedule not found")
	// ErrReportScheduleBadTransition is returned when a pause/resume/
	// cancel is not allowed from the schedule's current status.
	ErrReportScheduleBadTransition = errors.New("ledger: report schedule status transition not allowed")
	// ErrReportScheduleUnsafeID is returned when a dir-sink schedule ID
	// is not usable as a file name (it names the delivery file).
	ErrReportScheduleUnsafeID = errors.New("ledger: report schedule ID must not contain path separators with a dir sink")
)

// ReportKind selects what a ReportSchedule generates.
type ReportKind string

const (
	// ReportKindReconcile generates a ReconcileWithOptions report (LG-13,
	// LG-31): the full reconciliation scan, optionally folded into a
	// base currency.
	ReportKindReconcile ReportKind = "reconcile"
	// ReportKindStatement generates a GenerateStatement account
	// statement (LG-71): opening/closing balances, period entries, fee
	// summary, and event summary for one account.
	ReportKindStatement ReportKind = "statement"
)

// ReportParams carries kind-specific inputs for a ReportSchedule.
// Fields that do not apply to the schedule's kind are ignored.
type ReportParams struct {
	// BaseCurrency is the reconcile report's base currency (LG-31
	// ReconcileOptions.BaseCurrency): every currency totals row is
	// folded into it. Empty means no conversion.
	BaseCurrency string `json:"base_currency,omitempty"`
	// Account is the statement's account. Required for the statement
	// kind, ignored for reconcile.
	Account AccountID `json:"account,omitempty"`
	// From and To are the statement's [from, to) window as RFC3339
	// timestamps. Zero values take GenerateStatement's defaults (zero
	// to = now, zero from = first day of to's UTC month).
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
}

// validateForKind checks the params against the schedule's kind.
func (p ReportParams) validateForKind(kind ReportKind) error {
	switch kind {
	case ReportKindReconcile:
		if p.BaseCurrency != "" {
			if _, err := normalizeCurrency(p.BaseCurrency); err != nil {
				return fmt.Errorf("%w: base currency %q: %v", ErrReportScheduleBadParams, p.BaseCurrency, err)
			}
		}
		return nil
	case ReportKindStatement:
		if p.Account == "" {
			return fmt.Errorf("%w: statement report requires an account", ErrReportScheduleBadParams)
		}
		var from, to time.Time
		if p.From != "" {
			var err error
			if from, err = time.Parse(time.RFC3339, p.From); err != nil {
				return fmt.Errorf("%w: bad from %q: %v", ErrReportScheduleBadParams, p.From, err)
			}
		}
		if p.To != "" {
			var err error
			if to, err = time.Parse(time.RFC3339, p.To); err != nil {
				return fmt.Errorf("%w: bad to %q: %v", ErrReportScheduleBadParams, p.To, err)
			}
		}
		if !from.IsZero() && !to.IsZero() && from.After(to) {
			return fmt.Errorf("%w: from %q is after to %q", ErrReportScheduleBadParams, p.From, p.To)
		}
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrReportScheduleBadKind, kind)
	}
}

// ReportSink is where a generated report goes. Exactly one delivery
// target must be configured: a signed URL webhook, or a directory the
// report JSON is written into.
type ReportSink struct {
	// URL is the webhook endpoint receiving the report JSON as a POST.
	// The delivery follows the LG-67 convention: HMAC-SHA256(secret,
	// body) hex-encoded as X-Ledger-Signature: sha256=<hex>, plus
	// X-Ledger-Event-Type: report_generated,
	// X-Ledger-Report-Schedule: <id>, and X-Ledger-Report-Run: <seq>.
	URL string `json:"url,omitempty"`
	// Secret signs URL deliveries. It never serializes (json:"-"):
	// schedules are process-lifetime state and the secret must not leak
	// into logs or marshaled schedule records.
	Secret string `json:"-"`
	// Dir is the directory the report JSON is written into, one file
	// per run: <dir>/<schedule-id>-<run-seq>.json, atomically.
	Dir string `json:"dir,omitempty"`
}

// Validate rejects an unusable sink: both or neither target set, a
// non-absolute-http(s) URL, a URL without a secret (unsigned webhooks
// can be forged — fail fast, never silently forward unsigned), or a
// directory that cannot be created. Validation runs at creation time so
// a misconfigured sink never silently drops reports.
func (s ReportSink) Validate() error {
	urlSet, dirSet := s.URL != "", s.Dir != ""
	switch {
	case urlSet && dirSet:
		return fmt.Errorf("%w: set exactly one of URL or Dir", ErrReportScheduleBadSink)
	case !urlSet && !dirSet:
		return fmt.Errorf("%w: no delivery target configured", ErrReportScheduleBadSink)
	case urlSet:
		u, err := url.Parse(s.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("%w: invalid URL %q: want an absolute http(s) URL", ErrReportScheduleBadSink, s.URL)
		}
		if s.Secret == "" {
			return fmt.Errorf("%w: URL sink requires a signing secret", ErrReportScheduleBadSink)
		}
		return nil
	default:
		if err := os.MkdirAll(s.Dir, 0o755); err != nil {
			return fmt.Errorf("%w: cannot create sink dir %q: %v", ErrReportScheduleBadSink, s.Dir, err)
		}
		return nil
	}
}

// sinkName is the delivery target's short name for audit events.
func (s ReportSink) sinkName() string {
	if s.URL != "" {
		return "url"
	}
	return "dir"
}

// ReportSchedule is one recurring report subscription.
type ReportSchedule struct {
	// ID is the operator-chosen schedule identifier. Dir-sink delivery
	// files are named after it ("<id>-<run-seq>.json"), so it must not
	// contain path separators when a Dir sink is configured.
	ID string `json:"id"`
	// Kind selects the report: "reconcile" or "statement".
	Kind ReportKind `json:"kind"`
	// Params carries the kind-specific inputs.
	Params ReportParams `json:"params"`
	// Interval is the fixed cadence between runs. Serialized as a Go
	// duration string ("24h") — see MarshalJSON.
	Interval time.Duration `json:"interval"`
	// NextRunAt is the nominal due time of the next run. A schedule
	// created with a past NextRunAt fires on the first sweep.
	NextRunAt time.Time `json:"next_run_at"`
	// EndsAt is optional: the schedule completes once NextRunAt advances
	// past it. Zero means "no end".
	EndsAt time.Time `json:"ends_at,omitempty"`
	// Sink is where the generated report is delivered.
	Sink ReportSink `json:"sink"`
	// Status is the lifecycle state (see ScheduleStatus, shared with
	// transfer schedules: active/paused/cancelled/completed).
	Status ScheduleStatus `json:"status"`
	// RunSeq is the sequence number the next run will use. It only moves
	// forward, so a crash between delivery and the state advance replays
	// the same run number on the next sweep.
	RunSeq uint64 `json:"run_seq"`
	// CreatedAt is when the schedule was created.
	CreatedAt time.Time `json:"created_at"`
	// IdempotencyKey makes schedule creation idempotent: creating with a
	// key that was already used returns the existing schedule.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// reportScheduleJSON is ReportSchedule without its methods: the shadow
// structs in MarshalJSON/UnmarshalJSON embed this instead of
// ReportSchedule itself, so the promoted method set cannot recurse.
type reportScheduleJSON ReportSchedule

// MarshalJSON renders Interval as a Go duration string ("24h") instead of
// a nanosecond count. The shadow's explicit Interval field (depth 0)
// wins over the promoted one (depth 1).
func (s ReportSchedule) MarshalJSON() ([]byte, error) {
	type shadow struct {
		reportScheduleJSON
		Interval string `json:"interval"`
	}
	return json.Marshal(shadow{reportScheduleJSON(s), s.Interval.String()})
}

// UnmarshalJSON parses the duration-string Interval. A missing or
// unparseable interval is an error: schedules fail fast on corrupt state
// instead of firing at a zero cadence.
func (s *ReportSchedule) UnmarshalJSON(data []byte) error {
	type shadow struct {
		reportScheduleJSON
		Interval string `json:"interval"`
	}
	var sh shadow
	if err := json.Unmarshal(data, &sh); err != nil {
		return err
	}
	iv, err := time.ParseDuration(sh.Interval)
	if err != nil {
		return fmt.Errorf("ledger: report schedule interval: %w", err)
	}
	*s = ReportSchedule(sh.reportScheduleJSON)
	s.Interval = iv
	return nil
}

// ReportRun is one generated (or failed) execution of a ReportSchedule.
type ReportRun struct {
	// ScheduleID names the schedule this run belongs to.
	ScheduleID string `json:"schedule_id"`
	// RunSeq is the run's sequence number.
	RunSeq uint64 `json:"run_seq"`
	// Kind is the report kind that ran.
	Kind ReportKind `json:"kind"`
	// DueAt is the nominal due time that triggered this run.
	DueAt time.Time `json:"due_at"`
	// FiredAt is when the sweep executed the run.
	FiredAt time.Time `json:"fired_at"`
	// Status is "fired" or "failed".
	Status string `json:"status"`
	// Error carries the generation or delivery failure on a failed run.
	Error string `json:"error,omitempty"`
	// ReportBytes is the delivered report's JSON size on a fired run.
	ReportBytes int64 `json:"report_bytes,omitempty"`
	// SkippedRuns counts nominal runs between the previous NextRunAt and
	// now that were skipped instead of caught up (LG-41 convention).
	SkippedRuns int64 `json:"skipped_runs,omitempty"`
}

// maxReportRuns bounds the per-schedule run history: the newest runs
// stay, the oldest are evicted. Mirrors maxScheduleRuns (LG-41).
const maxReportRuns = 100

// ReportSweeper is the background worker that generates due report
// schedules on a ticker: the in-process version of polling
// SweepReportsDue on a timer. It mirrors ScheduleSweeper (see
// schedule.go): a single goroutine that calls l.SweepReportsDue(
// time.Now()) every interval until its context is cancelled or Stop is
// called. The first tick fires after one full interval, not immediately.
//
// A non-positive interval disables the sweeper: it returns nil and starts
// nothing.
type ReportSweeper struct {
	l        *Ledger
	interval time.Duration
	onSweep  func(generated, failed int)

	done     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	ticks         atomic.Uint64
	generatedTotal atomic.Uint64
	failedTotal   atomic.Uint64
}

// StartReportSweeper launches a background goroutine that calls
// l.SweepReportsDue(time.Now()) every interval until ctx is cancelled or
// Stop is called. After each tick it invokes onSweep (when non-nil) with
// the fired/failed run counts, so the caller can feed its own metrics.
func StartReportSweeper(ctx context.Context, l *Ledger, interval time.Duration, onSweep func(generated, failed int)) *ReportSweeper {
	if interval <= 0 {
		return nil
	}
	sw := &ReportSweeper{
		l:        l,
		interval: interval,
		onSweep:  onSweep,
		done:     make(chan struct{}),
	}
	sw.wg.Add(1)
	go sw.run(ctx)
	return sw
}

func (sw *ReportSweeper) run(ctx context.Context) {
	defer sw.wg.Done()
	ticker := time.NewTicker(sw.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sw.done:
			return
		case <-ticker.C:
			now := time.Now()
			generated, failed := 0, 0
			for _, r := range sw.l.SweepReportsDue(now) {
				if r.Status == "failed" {
					failed++
				} else {
					generated++
				}
			}
			sw.ticks.Add(1)
			sw.generatedTotal.Add(uint64(generated))
			sw.failedTotal.Add(uint64(failed))
			if sw.onSweep != nil {
				sw.onSweep(generated, failed)
			}
		}
	}
}

// Stop halts the sweeper and waits for its goroutine to exit. It is
// idempotent and safe to call alongside context cancellation.
func (sw *ReportSweeper) Stop() {
	sw.stopOnce.Do(func() { close(sw.done) })
	sw.wg.Wait()
}

// Ticks reports how many sweeps have completed. GeneratedTotal and
// FailedTotal report the cumulative run outcomes across all ticks.
func (sw *ReportSweeper) Ticks() uint64          { return sw.ticks.Load() }
func (sw *ReportSweeper) GeneratedTotal() uint64 { return sw.generatedTotal.Load() }
func (sw *ReportSweeper) FailedTotal() uint64    { return sw.failedTotal.Load() }

// CreateReportSchedule registers a recurring report subscription. The
// schedule starts active: the first SweepReportsDue at or past NextRunAt
// generates run 0. Validation runs before anything is recorded, and a
// creation carrying an idempotency key that was already used returns the
// existing schedule with duplicate=true instead of recording a second
// subscription.
//
// Cadence is a fixed interval ("24h", "168h"); cron expressions are
// intentionally not supported — fixed cadences cover daily/weekly/monthly
// reporting without a cron parser's edge cases.
func (l *Ledger) CreateReportSchedule(s ReportSchedule) (ReportSchedule, bool, error) {
	if err := s.Sink.Validate(); err != nil {
		return ReportSchedule{}, false, err
	}
	if s.Kind != ReportKindReconcile && s.Kind != ReportKindStatement {
		return ReportSchedule{}, false, fmt.Errorf("%w: %q", ErrReportScheduleBadKind, s.Kind)
	}
	if err := s.Params.validateForKind(s.Kind); err != nil {
		return ReportSchedule{}, false, err
	}
	if s.Sink.Dir != "" && s.ID != "" && s.ID != filepath.Base(s.ID) {
		return ReportSchedule{}, false, fmt.Errorf("%w: %q", ErrReportScheduleUnsafeID, s.ID)
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	// Idempotency replay runs after field validation but before the ID
	// conflict check, mirroring CreateTransferSchedule: a replayed key
	// returns the recorded schedule byte-for-byte.
	if s.IdempotencyKey != "" {
		if id, ok := l.reportScheduleKeys[s.IdempotencyKey]; ok {
			if existing, exists := l.reportSchedules[id]; exists {
				return *existing, true, nil
			}
		}
	}
	if s.ID == "" {
		return ReportSchedule{}, false, ErrReportScheduleEmptyID
	}
	if _, exists := l.reportSchedules[s.ID]; exists {
		return ReportSchedule{}, false, ErrReportScheduleIDConflict
	}
	if s.Interval <= 0 {
		return ReportSchedule{}, false, ErrReportScheduleBadInterval
	}
	if s.NextRunAt.IsZero() {
		return ReportSchedule{}, false, ErrReportScheduleBadNextRunAt
	}
	if !s.EndsAt.IsZero() && !s.EndsAt.After(s.NextRunAt) {
		return ReportSchedule{}, false, ErrReportScheduleBadEndsAt
	}

	s.Status = ScheduleActive
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now().UTC()
	}
	rec := &ReportSchedule{
		ID:             s.ID,
		Kind:           s.Kind,
		Params:         s.Params,
		Interval:       s.Interval,
		NextRunAt:      s.NextRunAt,
		EndsAt:         s.EndsAt,
		Sink:           s.Sink,
		Status:         s.Status,
		CreatedAt:      s.CreatedAt,
		IdempotencyKey: s.IdempotencyKey,
	}
	l.reportSchedules[s.ID] = rec
	if s.IdempotencyKey != "" {
		l.reportScheduleKeys[s.IdempotencyKey] = s.ID
	}
	l.emitAudit(AuditEvent{
		Op:      "report_schedule_created",
		Actor:   "CreateReportSchedule",
		TraceID: s.ID,
		Details: map[string]any{
			"kind":     string(s.Kind),
			"interval": s.Interval.String(),
			"sink":     s.Sink.sinkName(),
		},
	})
	return *rec, false, nil
}

// GetReportSchedule returns a copy of the named report schedule.
func (l *Ledger) GetReportSchedule(id string) (ReportSchedule, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	sched, ok := l.reportSchedules[id]
	if !ok {
		return ReportSchedule{}, ErrReportScheduleNotFound
	}
	return *sched, nil
}

// PauseReportSchedule holds an active report schedule: SweepReportsDue
// skips it until resumed. Only active schedules can be paused.
func (l *Ledger) PauseReportSchedule(id string) (ReportSchedule, error) {
	return l.setReportScheduleStatus(id, ScheduleActive, SchedulePaused, "report_schedule_paused")
}

// ResumeReportSchedule returns a paused report schedule to active. Only
// paused schedules can be resumed; a schedule whose NextRunAt fell
// behind while paused generates once on the next sweep and skips the
// missed runs — pausing does not bank up reports.
func (l *Ledger) ResumeReportSchedule(id string) (ReportSchedule, error) {
	return l.setReportScheduleStatus(id, SchedulePaused, ScheduleActive, "report_schedule_resumed")
}

// CancelReportSchedule stops a report schedule permanently. Only active
// or paused schedules can be cancelled; completed schedules are already
// terminal.
func (l *Ledger) CancelReportSchedule(id string) (ReportSchedule, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	sched, ok := l.reportSchedules[id]
	if !ok {
		return ReportSchedule{}, ErrReportScheduleNotFound
	}
	if sched.Status != ScheduleActive && sched.Status != SchedulePaused {
		return ReportSchedule{}, fmt.Errorf("%w: cannot cancel a %s schedule", ErrReportScheduleBadTransition, sched.Status)
	}
	sched.Status = ScheduleCancelled
	l.emitAudit(AuditEvent{
		Op:      "report_schedule_cancelled",
		Actor:   "CancelReportSchedule",
		TraceID: id,
		Details: map[string]any{"run_seq": sched.RunSeq},
	})
	return *sched, nil
}

func (l *Ledger) setReportScheduleStatus(id string, from, to ScheduleStatus, op string) (ReportSchedule, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	sched, ok := l.reportSchedules[id]
	if !ok {
		return ReportSchedule{}, ErrReportScheduleNotFound
	}
	if sched.Status != from {
		return ReportSchedule{}, fmt.Errorf("%w: cannot move a %s schedule to %s", ErrReportScheduleBadTransition, sched.Status, to)
	}
	sched.Status = to
	l.emitAudit(AuditEvent{
		Op:      op,
		Actor:   op,
		TraceID: id,
		Details: map[string]any{"run_seq": sched.RunSeq},
	})
	return *sched, nil
}

// ListReportRuns returns the report schedule's run history, newest first,
// capped at limit (limit <= 0 means all). Unknown schedule IDs are an
// error.
func (l *Ledger) ListReportRuns(id string, limit int) ([]ReportRun, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if _, ok := l.reportSchedules[id]; !ok {
		return nil, ErrReportScheduleNotFound
	}
	runs := l.reportScheduleRuns[id]
	out := make([]ReportRun, 0, len(runs))
	for i := len(runs) - 1; i >= 0; i-- {
		out = append(out, runs[i])
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// recordReportRunLocked appends a run to the schedule's bounded history.
// Callers must hold the write lock.
func (l *Ledger) recordReportRunLocked(sched *ReportSchedule, run ReportRun) {
	runs := append(l.reportScheduleRuns[sched.ID], run)
	if len(runs) > maxReportRuns {
		runs = runs[len(runs)-maxReportRuns:]
	}
	l.reportScheduleRuns[sched.ID] = runs
}

// SweepReportsDue generates every active report schedule whose NextRunAt
// is at or before now, returning one ReportRun per due schedule. It never
// holds the ledger lock across report generation: schedules are
// snapshotted under the lock, generated without it (generation takes the
// read lock itself), and finalized under it. The per-schedule firing
// flag serializes concurrent sweeps of the same schedule, and the run
// sequence only advances after a successful delivery — a crash between
// delivery and finalization replays the same run number on the next
// sweep.
func (l *Ledger) SweepReportsDue(now time.Time) []ReportRun {
	l.mu.Lock()
	due := make([]ReportSchedule, 0)
	ids := make([]string, 0, len(l.reportSchedules))
	for id := range l.reportSchedules {
		ids = append(ids, id)
	}
	sort.Strings(ids) // deterministic generation order
	for _, id := range ids {
		sched := l.reportSchedules[id]
		if sched.Status != ScheduleActive || l.reportScheduleFiring[id] {
			continue
		}
		if sched.NextRunAt.After(now) {
			continue
		}
		// The schedule expired before this run: complete it without
		// generating.
		if !sched.EndsAt.IsZero() && sched.NextRunAt.After(sched.EndsAt) {
			sched.Status = ScheduleCompleted
			l.emitAudit(AuditEvent{
				Op:      "report_schedule_completed",
				Actor:   "SweepReportsDue",
				TraceID: id,
				Details: map[string]any{"reason": "ends_at passed", "run_seq": sched.RunSeq},
			})
			continue
		}
		l.reportScheduleFiring[id] = true
		// The run sequence is derived, not stored, until the run
		// finalizes: if the process crashes between delivery and the
		// finalize below, the stored RunSeq still names the
		// unfinalized run, so the next sweep retries it.
		cp := *sched
		cp.RunSeq = sched.RunSeq + 1
		due = append(due, cp)
	}
	l.mu.Unlock()

	runs := make([]ReportRun, 0, len(due))
	for _, s := range due {
		runs = append(runs, l.fireReportSchedule(s, now))
	}
	return runs
}

// fireReportSchedule executes one due run: generates the report
// read-only, marshals it, delivers it to the sink, then finalizes the
// schedule state under the write lock. It must not be called with l.mu
// held — generation takes the read lock itself.
func (l *Ledger) fireReportSchedule(s ReportSchedule, now time.Time) ReportRun {
	run := ReportRun{
		ScheduleID: s.ID,
		RunSeq:     s.RunSeq,
		Kind:       s.Kind,
		DueAt:      s.NextRunAt,
		FiredAt:    now,
	}

	body, genErr := l.generateReport(s, now)
	if genErr != nil {
		return l.finalizeReportRun(s, run, now, 0, genErr)
	}
	if err := s.deliverReport(body, run); err != nil {
		return l.finalizeReportRun(s, run, now, 0, err)
	}
	return l.finalizeReportRun(s, run, now, int64(len(body)), nil)
}

// generateReport builds the schedule's report without touching ledger
// state: ReconcileWithOptions and GenerateStatement both run under the
// read lock and never bump the version or append to the journal. A
// generation error fails the run before anything is delivered.
func (l *Ledger) generateReport(s ReportSchedule, now time.Time) ([]byte, error) {
	switch s.Kind {
	case ReportKindReconcile:
		report, err := l.ReconcileWithOptions(now, ReconcileOptions{BaseCurrency: s.Params.BaseCurrency})
		if err != nil {
			return nil, err
		}
		return json.Marshal(report)
	case ReportKindStatement:
		var from, to time.Time
		if s.Params.From != "" {
			from, _ = time.Parse(time.RFC3339, s.Params.From) // validated at creation
		}
		if s.Params.To != "" {
			to, _ = time.Parse(time.RFC3339, s.Params.To)
		}
		stmt, err := l.GenerateStatement(s.Params.Account, from, to)
		if err != nil {
			return nil, err
		}
		return json.Marshal(stmt)
	default:
		return nil, fmt.Errorf("%w: %q", ErrReportScheduleBadKind, s.Kind)
	}
}

// deliverReport pushes the report to the schedule's sink: a signed URL
// webhook, or an atomic file write. It must not be called with l.mu
// held — the URL path does network I/O.
func (s ReportSchedule) deliverReport(body []byte, run ReportRun) error {
	if s.Sink.URL != "" {
		return postReportWebhook(s.Sink.URL, s.Sink.Secret, s.ID, run.RunSeq, body)
	}
	path := filepath.Join(s.Sink.Dir, fmt.Sprintf("%s-%d.json", s.ID, run.RunSeq))
	tmp, err := os.CreateTemp(s.Sink.Dir, ".report-*")
	if err != nil {
		return fmt.Errorf("ledger: report sink dir: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("ledger: report sink dir: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("ledger: report sink dir: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("ledger: report sink dir: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("ledger: report sink dir: %w", err)
	}
	return nil
}

// reportHTTPClient bounds one webhook delivery attempt; retries happen
// on the next sweep, not inside a delivery.
var reportHTTPClient = &http.Client{Timeout: 10 * time.Second}

// postReportWebhook POSTs one report to the URL sink with the LG-67
// signature convention: HMAC-SHA256(secret, body) hex-encoded as
// X-Ledger-Signature: sha256=<hex>. Any 2xx is success; anything else is
// an error the sweep records as a failed run (retried next sweep).
func postReportWebhook(rawURL, secret, scheduleID string, runSeq uint64, body []byte) error {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("ledger: report webhook: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Ledger-Signature", sig)
	req.Header.Set("X-Ledger-Event-Type", "report_generated")
	req.Header.Set("X-Ledger-Report-Schedule", scheduleID)
	req.Header.Set("X-Ledger-Report-Run", strconv.FormatUint(runSeq, 10))
	resp, err := reportHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("ledger: report webhook: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("ledger: report webhook: status %d", resp.StatusCode)
	}
	return nil
}

// finalizeReportRun records the run under the write lock and emits the
// audit event. A failed run keeps its NextRunAt: the next sweep retries
// the same run sequence, so a transient generation or delivery failure
// never skips a report. On success the run sequence commits and
// NextRunAt advances arithmetically past now (missed nominal runs count
// as skipped, never caught up).
func (l *Ledger) finalizeReportRun(s ReportSchedule, run ReportRun, now time.Time, reportBytes int64, runErr error) ReportRun {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.reportScheduleFiring, s.ID)
	sched, ok := l.reportSchedules[s.ID]
	if !ok {
		// No report-schedule deletion API exists; this is defensive.
		return run
	}
	run.ReportBytes = reportBytes
	if runErr != nil {
		run.Status = "failed"
		run.Error = runErr.Error()
		l.recordReportRunLocked(sched, run)
		l.emitAudit(AuditEvent{
			Op:      "report_run_failed",
			Actor:   "SweepReportsDue",
			TraceID: s.ID,
			Details: map[string]any{
				"schedule_id": s.ID,
				"kind":        string(s.Kind),
				"run_seq":     s.RunSeq,
				"error":       runErr.Error(),
			},
		})
		return run
	}

	run.Status = "fired"
	// Commit the run sequence only now that the report is delivered: a
	// crash before this point leaves RunSeq naming the unfinalized run,
	// and the next sweep retries it.
	sched.RunSeq = s.RunSeq
	// Advance NextRunAt past now arithmetically (no spin when far
	// behind): the next run lands one interval after the last missed
	// nominal run, and every nominal run in between counts as skipped.
	elapsed := now.Sub(sched.NextRunAt) // >= 0: the schedule was due
	whole := elapsed / s.Interval        // missed nominal runs, >= 0
	run.SkippedRuns = int64(whole)
	sched.NextRunAt = now.Add(s.Interval - elapsed%s.Interval)
	if !sched.EndsAt.IsZero() && sched.NextRunAt.After(sched.EndsAt) {
		sched.Status = ScheduleCompleted
	}
	l.recordReportRunLocked(sched, run)
	l.emitAudit(AuditEvent{
		Op:      "report_generated",
		Actor:   "SweepReportsDue",
		TraceID: s.ID,
		Details: map[string]any{
			"schedule_id":  s.ID,
			"kind":         string(s.Kind),
			"run_seq":      s.RunSeq,
			"due_at":       s.NextRunAt.UTC().Format(time.RFC3339),
			"skipped_runs": run.SkippedRuns,
			"sink":         s.Sink.sinkName(),
			"report_bytes": reportBytes,
		},
	})
	return run
}

// activeReportScheduleCountLocked counts report schedules currently
// firing on their cadence. Callers must hold l.mu (either lock
// suffices).
func (l *Ledger) activeReportScheduleCountLocked() int {
	n := 0
	for _, s := range l.reportSchedules {
		if s.Status == ScheduleActive {
			n++
		}
	}
	return n
}

// validateReportScheduleRecord checks one report schedule decoded from
// an external source: every field the constructor validates, so a
// corrupt record can never resurrect a schedule CreateReportSchedule
// would have rejected. (Report schedules are process-lifetime state and
// are not snapshotted; this validator exists for future persistence and
// for defense in depth on any record a caller hands back in.)
func validateReportScheduleRecord(s *ReportSchedule) error {
	if s.ID == "" {
		return ErrReportScheduleEmptyID
	}
	if s.Kind != ReportKindReconcile && s.Kind != ReportKindStatement {
		return fmt.Errorf("%w: %q", ErrReportScheduleBadKind, s.Kind)
	}
	if err := s.Params.validateForKind(s.Kind); err != nil {
		return err
	}
	if err := s.Sink.Validate(); err != nil {
		return err
	}
	if s.Sink.Dir != "" && s.ID != filepath.Base(s.ID) {
		return fmt.Errorf("%w: %q", ErrReportScheduleUnsafeID, s.ID)
	}
	if s.Interval <= 0 {
		return ErrReportScheduleBadInterval
	}
	if s.NextRunAt.IsZero() {
		return ErrReportScheduleBadNextRunAt
	}
	if !s.EndsAt.IsZero() && !s.EndsAt.After(s.NextRunAt) {
		return ErrReportScheduleBadEndsAt
	}
	switch s.Status {
	case ScheduleActive, SchedulePaused, ScheduleCancelled, ScheduleCompleted:
	default:
		return fmt.Errorf("ledger: report schedule %q has invalid status %q", s.ID, s.Status)
	}
	return nil
}
