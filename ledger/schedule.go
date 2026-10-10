package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Scheduled transfers (LG-41).
//
// A TransferSchedule is a recurring transfer plan — payroll, rent, loan
// servicing: every Interval, starting at NextRunAt, the ledger fires one
// PostTransfer through the full atomic validation chain
// (field validation, fee policy, idempotency, frozen/overdraft/daily-limit
// /period risk checks). Each run is idempotent under the key
// "<schedule ID>/run/<seq>", so a crash between the transfer commit and
// the schedule-state advance can never double-book: the next sweep
// replays the same key and PostTransfer returns the original receipt.
//
// SweepDue(now) is the single entry point: it scans for due schedules and
// fires them. A schedule fires at most once per sweep, and the firing flag
// serializes concurrent sweeps of the same schedule — two goroutines
// sweeping at once can never double-fire one schedule (each gets a
// distinct run sequence). A run that fails validation (frozen account,
// overdraft, closed period, ...) is recorded as failed in the run history
// and the audit log, and the schedule keeps its NextRunAt so the next
// sweep retries the same run — a failed run never silently advances past
// an obligation, and it never blocks other schedules.
//
// Missed runs are skipped, not caught up: if the sweeper was down for
// five intervals, the next sweep fires one run and advances NextRunAt past
// now, counting the skipped runs on the run record. Catching up by firing
// N backdated transfers could drain an account the operator did not expect
// to drain; the skipped count keeps the gap visible.
//
// Schedules are structural state like holds and merges: they survive
// disaster-recovery snapshots (see snapshot.go) but the in-memory run
// history does not — history is operational, the schedule definition and
// its RunSeq/NextRunAt/Status are what a restore needs to keep firing.

var (
	// ErrScheduleEmptyID is returned when a schedule is created with an
	// empty ID.
	ErrScheduleEmptyID = errors.New("ledger: schedule ID must not be empty")
	// ErrScheduleIDConflict is returned when a schedule ID is already in
	// use by another schedule.
	ErrScheduleIDConflict = errors.New("ledger: schedule ID already exists")
	// ErrScheduleEmptyAccount is returned when a schedule names an empty
	// from or to account.
	ErrScheduleEmptyAccount = errors.New("ledger: schedule from and to accounts must not be empty")
	// ErrScheduleSameAccount is returned when a schedule's from and to
	// accounts are the same.
	ErrScheduleSameAccount = errors.New("ledger: schedule from and to accounts must differ")
	// ErrScheduleNonPositiveAmount is returned when a schedule amount is
	// not positive.
	ErrScheduleNonPositiveAmount = errors.New("ledger: schedule amount must be positive")
	// ErrScheduleBadFee is returned when a schedule carries a negative
	// fee, or a positive fee without a fee account.
	ErrScheduleBadFee = errors.New("ledger: schedule fee must be non-negative and needs a fee account when positive")
	// ErrScheduleBadInterval is returned when a schedule interval is not
	// positive.
	ErrScheduleBadInterval = errors.New("ledger: schedule interval must be positive")
	// ErrScheduleBadNextRunAt is returned when a schedule has no next run
	// time.
	ErrScheduleBadNextRunAt = errors.New("ledger: schedule next_run_at must be set")
	// ErrScheduleBadEndsAt is returned when a schedule's ends_at is set
	// but does not fall after its next_run_at.
	ErrScheduleBadEndsAt = errors.New("ledger: schedule ends_at must be after next_run_at")
	// ErrScheduleNotFound is returned when a schedule ID names no known
	// schedule.
	ErrScheduleNotFound = errors.New("ledger: transfer schedule not found")
	// ErrScheduleBadTransition is returned when a pause/resume/cancel is
	// not allowed from the schedule's current status.
	ErrScheduleBadTransition = errors.New("ledger: schedule status transition not allowed")
)

// ScheduleStatus is the lifecycle state of a TransferSchedule.
type ScheduleStatus string

const (
	// ScheduleActive fires on its cadence. This is the creation state.
	ScheduleActive ScheduleStatus = "active"
	// SchedulePaused is held by the operator: SweepDue skips it, and its
	// NextRunAt does not advance while paused.
	SchedulePaused ScheduleStatus = "paused"
	// ScheduleCancelled is terminal: the operator stopped the plan.
	ScheduleCancelled ScheduleStatus = "cancelled"
	// ScheduleCompleted is terminal: EndsAt passed and the last run
	// fired (or the plan expired before its first run).
	ScheduleCompleted ScheduleStatus = "completed"
)

// TransferSchedule is one recurring transfer plan.
type TransferSchedule struct {
	// ID is the operator-chosen plan identifier. Run transfer IDs derive
	// from it ("<ID>/run/<seq>").
	ID string `json:"id"`
	// From is the payer account, To the payee — the same legs every run
	// posts.
	From AccountID `json:"from_account"`
	To   AccountID `json:"to_account"`
	// AmountCents is the per-run principal in the schedule's currency.
	AmountCents int64 `json:"amount_cents"`
	// Currency is the ISO 4217 code every leg is booked in (empty
	// normalizes to the default currency, like PostTransfer).
	Currency string `json:"currency"`
	// ToCurrency and FXAccount mirror Transfer's cross-currency fields:
	// set both for an FX-settled recurring transfer. The rate is looked
	// up at fire time, so a rate configured after the schedule was
	// created still applies — and a missing rate fails the run (recorded,
	// retried next sweep) instead of failing the schedule.
	ToCurrency string    `json:"to_currency,omitempty"`
	FXAccount  AccountID `json:"fx_account,omitempty"`
	// FeeCents / FeeAccount / SkipFee mirror Transfer's fee fields: an
	// explicit per-run fee, or the ledger's fee policy when unset.
	FeeCents   int64     `json:"fee_cents,omitempty"`
	FeeAccount AccountID `json:"fee_account,omitempty"`
	SkipFee    bool      `json:"skip_fee,omitempty"`
	// Memo rides every run's principal entry (see Transfer.Memo).
	Memo string `json:"memo,omitempty"`
	// Interval is the fixed cadence between runs. Serialized as a Go
	// duration string ("24h") — see MarshalJSON.
	Interval time.Duration `json:"interval"`
	// NextRunAt is the nominal due time of the next run. A schedule
	// created with a past NextRunAt fires on the first sweep.
	NextRunAt time.Time `json:"next_run_at"`
	// EndsAt is optional: the plan completes once NextRunAt advances past
	// it. Zero means "no end".
	EndsAt time.Time `json:"ends_at,omitempty"`
	// IdempotencyKey makes schedule creation idempotent: creating with a
	// key that was already used returns the existing schedule.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	// Status is the lifecycle state (see ScheduleStatus).
	Status ScheduleStatus `json:"status"`
	// RunSeq is the sequence number the next run will use. It only moves
	// forward, so run transfer IDs ("<ID>/run/<seq>") are never reused —
	// the idempotency story of SweepDue.
	RunSeq uint64 `json:"run_seq"`
	// CreatedAt is when the schedule was created.
	CreatedAt time.Time `json:"created_at"`
}

// transferScheduleJSON is TransferSchedule without its methods: the
// shadow structs in MarshalJSON/UnmarshalJSON embed this instead of
// TransferSchedule itself, so the promoted method set cannot recurse.
type transferScheduleJSON TransferSchedule

// MarshalJSON renders Interval as a Go duration string ("24h") instead of
// a nanosecond count. The shadow's explicit Interval field (depth 0)
// wins over the promoted one (depth 1) — encoding/json always prefers
// the shallower field.
func (s TransferSchedule) MarshalJSON() ([]byte, error) {
	type shadow struct {
		transferScheduleJSON
		Interval string `json:"interval"`
	}
	return json.Marshal(shadow{transferScheduleJSON(s), s.Interval.String()})
}

// UnmarshalJSON parses the duration-string Interval. A missing or
// unparseable interval is an error: schedules fail fast on corrupt
// state instead of firing at a zero cadence.
func (s *TransferSchedule) UnmarshalJSON(data []byte) error {
	type shadow struct {
		transferScheduleJSON
		Interval string `json:"interval"`
	}
	var sh shadow
	if err := json.Unmarshal(data, &sh); err != nil {
		return err
	}
	iv, err := time.ParseDuration(sh.Interval)
	if err != nil {
		return fmt.Errorf("ledger: schedule interval: %w", err)
	}
	*s = TransferSchedule(sh.transferScheduleJSON)
	s.Interval = iv
	return nil
}

// ScheduleRun is one fired (or failed) execution of a TransferSchedule.
type ScheduleRun struct {
	// ScheduleID names the plan this run belongs to.
	ScheduleID string `json:"schedule_id"`
	// RunSeq is the run's sequence number: its transfer ID is
	// "<schedule ID>/run/<seq>".
	RunSeq uint64 `json:"run_seq"`
	// TransferID is the journal transfer ID the run posted (or attempted).
	TransferID string `json:"transfer_id"`
	// DueAt is the nominal due time that triggered this run — the
	// schedule's NextRunAt at fire time. The journal entry itself is
	// booked at FiredAt: money moves when the sweep runs, not backdated
	// to the nominal time.
	DueAt time.Time `json:"due_at"`
	// FiredAt is when the sweep executed the run.
	FiredAt time.Time `json:"fired_at"`
	// Status is "fired" or "failed".
	Status string `json:"status"`
	// Error carries the PostTransfer rejection on a failed run.
	Error string `json:"error,omitempty"`
	// Duplicate is true when the run replayed an already-fired run: the
	// transfer had committed before a crash lost the schedule-state
	// advance, and the retry's idempotency key returned the original
	// receipt. Money moved exactly once.
	Duplicate bool `json:"duplicate,omitempty"`
	// SkippedRuns counts nominal runs between the previous NextRunAt and
	// now that were skipped instead of caught up (see the package doc).
	SkippedRuns int64 `json:"skipped_runs,omitempty"`
}

// maxScheduleRuns bounds the per-schedule run history: the newest runs
// stay, the oldest are evicted. History is operational — it does not
// survive snapshots.
const maxScheduleRuns = 100

// ScheduleSweeper is the background worker that fires due transfer
// schedules on a ticker: the in-process version of polling SweepDue on a
// timer. It mirrors HoldSweeper (see hold_sweep.go): a single goroutine
// that calls l.SweepDue(time.Now()) every interval until its context is
// cancelled or Stop is called. The first tick fires after one full
// interval, not immediately.
//
// A non-positive interval disables the sweeper: it returns nil and starts
// nothing.
type ScheduleSweeper struct {
	l        *Ledger
	interval time.Duration
	onSweep  func(fired, failed int)

	done     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	ticks       atomic.Uint64
	firedTotal  atomic.Uint64
	failedTotal atomic.Uint64
}

// StartScheduleSweeper launches a background goroutine that calls
// l.SweepDue(time.Now()) every interval until ctx is cancelled or Stop
// is called. After each tick it invokes onSweep (when non-nil) with the
// fired/failed run counts, so the caller can feed its own metrics — the
// HTTP server wires this to ledger_scheduled_transfers_total.
func StartScheduleSweeper(ctx context.Context, l *Ledger, interval time.Duration, onSweep func(fired, failed int)) *ScheduleSweeper {
	if interval <= 0 {
		return nil
	}
	sw := &ScheduleSweeper{
		l:        l,
		interval: interval,
		onSweep:  onSweep,
		done:     make(chan struct{}),
	}
	sw.wg.Add(1)
	go sw.run(ctx)
	return sw
}

func (sw *ScheduleSweeper) run(ctx context.Context) {
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
			fired, failed := 0, 0
			for _, r := range sw.l.SweepDue(now) {
				if r.Status == "failed" {
					failed++
				} else {
					fired++
				}
			}
			sw.ticks.Add(1)
			sw.firedTotal.Add(uint64(fired))
			sw.failedTotal.Add(uint64(failed))
			if sw.onSweep != nil {
				sw.onSweep(fired, failed)
			}
		}
	}
}

// Stop halts the sweeper and waits for its goroutine to exit. It is
// idempotent and safe to call alongside context cancellation.
func (sw *ScheduleSweeper) Stop() {
	sw.stopOnce.Do(func() { close(sw.done) })
	sw.wg.Wait()
}

// Ticks reports how many sweeps have completed. FiredTotal and
// FailedTotal report the cumulative run outcomes across all ticks.
func (sw *ScheduleSweeper) Ticks() uint64        { return sw.ticks.Load() }
func (sw *ScheduleSweeper) FiredTotal() uint64   { return sw.firedTotal.Load() }
func (sw *ScheduleSweeper) FailedTotal() uint64 { return sw.failedTotal.Load() }

// CreateTransferSchedule registers a recurring transfer plan. The plan
// starts active: the first SweepDue at or past NextRunAt fires run 0.
// Validation runs before anything is recorded, and a creation carrying an
// idempotency key that was already used returns the existing schedule
// with duplicate=true instead of recording a second plan.
//
// Cadence is a fixed interval ("24h", "168h"); cron expressions are
// intentionally not supported — fixed cadences cover payroll/rent/loan
// servicing without a cron parser's edge cases.
func (l *Ledger) CreateTransferSchedule(s TransferSchedule) (TransferSchedule, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if !s.EndsAt.IsZero() && !s.EndsAt.After(s.NextRunAt) {
		return TransferSchedule{}, false, ErrScheduleBadEndsAt
	}
	// Idempotency replay runs after field validation but before the ID
	// conflict check, mirroring PostTransfer: a replayed key returns the
	// recorded schedule byte-for-byte, even though the request carries a
	// schedule ID that is already in use (by the original schedule).
	if s.IdempotencyKey != "" {
		if id, ok := l.scheduleKeys[s.IdempotencyKey]; ok {
			if existing, exists := l.schedules[id]; exists {
				return *existing, true, nil
			}
		}
	}
	if s.ID == "" {
		return TransferSchedule{}, false, ErrScheduleEmptyID
	}
	if _, exists := l.schedules[s.ID]; exists {
		return TransferSchedule{}, false, ErrScheduleIDConflict
	}
	if s.From == "" || s.To == "" {
		return TransferSchedule{}, false, ErrScheduleEmptyAccount
	}
	if s.From == s.To {
		return TransferSchedule{}, false, ErrScheduleSameAccount
	}
	if s.AmountCents <= 0 {
		return TransferSchedule{}, false, ErrScheduleNonPositiveAmount
	}
	currency, err := normalizeCurrency(s.Currency)
	if err != nil {
		return TransferSchedule{}, false, err
	}
	s.Currency = currency
	if s.ToCurrency != "" {
		to, err := normalizeCurrency(s.ToCurrency)
		if err != nil {
			return TransferSchedule{}, false, err
		}
		if to == currency {
			return TransferSchedule{}, false, ErrCrossCurrencyTransfer
		}
		s.ToCurrency = to
	}
	if s.FeeCents < 0 || (s.FeeCents > 0 && s.FeeAccount == "") {
		return TransferSchedule{}, false, ErrScheduleBadFee
	}
	if err := checkMemoLength(s.Memo); err != nil {
		return TransferSchedule{}, false, err
	}
	if s.Interval <= 0 {
		return TransferSchedule{}, false, ErrScheduleBadInterval
	}
	if s.NextRunAt.IsZero() {
		return TransferSchedule{}, false, ErrScheduleBadNextRunAt
	}
	if !s.EndsAt.IsZero() && !s.EndsAt.After(s.NextRunAt) {
		return TransferSchedule{}, false, ErrScheduleBadEndsAt
	}
	// Idempotency replay runs after validation, mirroring PostTransfer:
	// a replayed key returns the recorded schedule byte-for-byte.
	if s.IdempotencyKey != "" {
		if id, ok := l.scheduleKeys[s.IdempotencyKey]; ok {
			if existing, exists := l.schedules[id]; exists {
				return *existing, true, nil
			}
		}
	}

	s.Status = ScheduleActive
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now().UTC()
	}
	rec := &TransferSchedule{
		ID:             s.ID,
		From:           s.From,
		To:             s.To,
		AmountCents:    s.AmountCents,
		Currency:       s.Currency,
		ToCurrency:     s.ToCurrency,
		FXAccount:      s.FXAccount,
		FeeCents:       s.FeeCents,
		FeeAccount:     s.FeeAccount,
		SkipFee:        s.SkipFee,
		Memo:           s.Memo,
		Interval:       s.Interval,
		NextRunAt:      s.NextRunAt,
		EndsAt:         s.EndsAt,
		IdempotencyKey: s.IdempotencyKey,
		Status:         s.Status,
		CreatedAt:      s.CreatedAt,
	}
	l.schedules[s.ID] = rec
	if s.IdempotencyKey != "" {
		l.scheduleKeys[s.IdempotencyKey] = s.ID
	}
	l.emitAudit(AuditEvent{
		Op:       "schedule_created",
		Actor:    "CreateTransferSchedule",
		TraceID:  s.ID,
		Accounts: []AccountID{s.From, s.To},
		Details: map[string]any{
			"amount_cents": s.AmountCents,
			"currency":     s.Currency,
			"interval":     s.Interval.String(),
			"next_run_at":  s.NextRunAt.UTC().Format(time.RFC3339),
		},
	})
	return *rec, false, nil
}

// GetTransferSchedule returns a copy of the named schedule.
func (l *Ledger) GetTransferSchedule(id string) (TransferSchedule, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	sched, ok := l.schedules[id]
	if !ok {
		return TransferSchedule{}, ErrScheduleNotFound
	}
	return *sched, nil
}

// PauseTransferSchedule holds an active schedule: SweepDue skips it until
// resumed. Only active schedules can be paused.
func (l *Ledger) PauseTransferSchedule(id string) (TransferSchedule, error) {
	return l.setScheduleStatus(id, ScheduleActive, SchedulePaused, "schedule_paused")
}

// ResumeTransferSchedule returns a paused schedule to active. Only paused
// schedules can be resumed; a plan whose NextRunAt fell behind while
// paused fires once on the next sweep and skips the missed runs (see the
// package doc) — pausing does not bank up obligations.
func (l *Ledger) ResumeTransferSchedule(id string) (TransferSchedule, error) {
	return l.setScheduleStatus(id, SchedulePaused, ScheduleActive, "schedule_resumed")
}

// CancelTransferSchedule stops a schedule permanently. Only active or
// paused schedules can be cancelled; completed schedules are already
// terminal.
func (l *Ledger) CancelTransferSchedule(id string) (TransferSchedule, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	sched, ok := l.schedules[id]
	if !ok {
		return TransferSchedule{}, ErrScheduleNotFound
	}
	if sched.Status != ScheduleActive && sched.Status != SchedulePaused {
		return TransferSchedule{}, fmt.Errorf("%w: cannot cancel a %s schedule", ErrScheduleBadTransition, sched.Status)
	}
	sched.Status = ScheduleCancelled
	l.emitAudit(AuditEvent{
		Op:      "schedule_cancelled",
		Actor:   "CancelTransferSchedule",
		TraceID: id,
		Details: map[string]any{"run_seq": sched.RunSeq},
	})
	return *sched, nil
}

func (l *Ledger) setScheduleStatus(id string, from, to ScheduleStatus, op string) (TransferSchedule, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	sched, ok := l.schedules[id]
	if !ok {
		return TransferSchedule{}, ErrScheduleNotFound
	}
	if sched.Status != from {
		return TransferSchedule{}, fmt.Errorf("%w: cannot move a %s schedule to %s", ErrScheduleBadTransition, sched.Status, to)
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

// ListScheduleRuns returns the schedule's run history, newest first,
// capped at limit (limit <= 0 means all). Unknown schedule IDs are an
// error.
func (l *Ledger) ListScheduleRuns(id string, limit int) ([]ScheduleRun, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if _, ok := l.schedules[id]; !ok {
		return nil, ErrScheduleNotFound
	}
	runs := l.scheduleRuns[id]
	out := make([]ScheduleRun, 0, len(runs))
	for i := len(runs) - 1; i >= 0; i-- {
		out = append(out, runs[i])
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// activeScheduleCountLocked counts schedules currently firing on their
// cadence. Callers must hold l.mu (either lock suffices).
func (l *Ledger) activeScheduleCountLocked() int {
	n := 0
	for _, s := range l.schedules {
		if s.Status == ScheduleActive {
			n++
		}
	}
	return n
}

// recordScheduleRunLocked appends a run to the schedule's bounded history.
// Callers must hold the write lock.
func (l *Ledger) recordScheduleRunLocked(sched *TransferSchedule, run ScheduleRun) {
	runs := append(l.scheduleRuns[sched.ID], run)
	if len(runs) > maxScheduleRuns {
		runs = runs[len(runs)-maxScheduleRuns:]
	}
	l.scheduleRuns[sched.ID] = runs
}

// SweepDue fires every active schedule whose NextRunAt is at or before
// now, returning one ScheduleRun per fired schedule. It never holds the
// ledger lock across the PostTransfer call: schedules are snapshotted
// under the lock, fired without it, and finalized under it. The per-
// schedule firing flag serializes concurrent sweeps of the same schedule,
// and the run's idempotency key makes a crash between commit and
// finalization replay-safe (see the package doc).
func (l *Ledger) SweepDue(now time.Time) []ScheduleRun {
	l.mu.Lock()
	due := make([]TransferSchedule, 0)
	ids := make([]string, 0, len(l.schedules))
	for id := range l.schedules {
		ids = append(ids, id)
	}
	sort.Strings(ids) // deterministic fire order
	for _, id := range ids {
		sched := l.schedules[id]
		if sched.Status != ScheduleActive || l.scheduleFiring[id] {
			continue
		}
		if sched.NextRunAt.After(now) {
			continue
		}
		// The plan expired before this run: complete it without firing.
		if !sched.EndsAt.IsZero() && sched.NextRunAt.After(sched.EndsAt) {
			sched.Status = ScheduleCompleted
			l.emitAudit(AuditEvent{
				Op:      "schedule_completed",
				Actor:   "SweepDue",
				TraceID: id,
				Details: map[string]any{"reason": "ends_at passed", "run_seq": sched.RunSeq},
			})
			continue
		}
		l.scheduleFiring[id] = true
		// The run sequence is derived, not stored, until the run
		// finalizes: if the process crashes between the transfer commit
		// and the finalize below, the stored RunSeq still names the
		// unfinalized run, so the next sweep retries it under the same
		// idempotency key and PostTransfer replays instead of
		// double-booking.
		cp := *sched
		cp.RunSeq = sched.RunSeq + 1
		due = append(due, cp)
	}
	l.mu.Unlock()

	runs := make([]ScheduleRun, 0, len(due))
	for _, s := range due {
		runs = append(runs, l.fireSchedule(s, now))
	}
	return runs
}

// fireSchedule executes one due run: builds the transfer from the
// schedule snapshot, posts it through the full PostTransfer chain, then
// finalizes the schedule state under the write lock. It must not be
// called with l.mu held — PostTransfer takes the lock itself.
func (l *Ledger) fireSchedule(s TransferSchedule, now time.Time) ScheduleRun {
	transferID := fmt.Sprintf("%s/run/%d", s.ID, s.RunSeq)
	t := Transfer{
		ID:             transferID,
		From:           s.From,
		To:             s.To,
		AmountCents:    s.AmountCents,
		Currency:       s.Currency,
		ToCurrency:     s.ToCurrency,
		FXAccount:      s.FXAccount,
		FeeCents:       s.FeeCents,
		FeeAccount:     s.FeeAccount,
		SkipFee:        s.SkipFee,
		Memo:           s.Memo,
		IdempotencyKey: transferID,
		CreatedAt:      now,
	}
	receipt, err := l.PostTransfer(t)

	run := ScheduleRun{
		ScheduleID: s.ID,
		RunSeq:     s.RunSeq,
		TransferID: transferID,
		DueAt:      s.NextRunAt,
		FiredAt:    now,
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.scheduleFiring, s.ID)
	sched, ok := l.schedules[s.ID]
	if !ok {
		// No schedule deletion API exists; this is defensive.
		return run
	}
	if err != nil {
		// Failed runs keep their NextRunAt: the next sweep retries the
		// same run sequence with the same idempotency key. The failure
		// is recorded in the run history and the audit log so it can
		// never pass silently, and other schedules already fired — one
		// plan's rejection never blocks the rest of the sweep.
		run.Status = "failed"
		run.Error = err.Error()
		l.recordScheduleRunLocked(sched, run)
		l.emitAudit(AuditEvent{
			Op:       "schedule_run_failed",
			Actor:    "SweepDue",
			TraceID:  transferID,
			Accounts: []AccountID{s.From, s.To},
			Details: map[string]any{
				"schedule_id": s.ID,
				"run_seq":     s.RunSeq,
				"error":       err.Error(),
			},
		})
		return run
	}

	run.Status = "fired"
	run.Duplicate = receipt.Duplicate
	// Commit the run sequence only now that the run is final: a crash
	// before this point leaves RunSeq naming the unfinalized run, and
	// the next sweep retries it under the same idempotency key.
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
	l.recordScheduleRunLocked(sched, run)
	l.emitAudit(AuditEvent{
		Op:       "schedule_run_fired",
		Actor:    "SweepDue",
		TraceID:  transferID,
		Accounts: []AccountID{s.From, s.To},
		EntryIDs: entryIDsOf(receipt),
		Details: map[string]any{
			"schedule_id":  s.ID,
			"run_seq":      s.RunSeq,
			"due_at":       s.NextRunAt.UTC().Format(time.RFC3339),
			"duplicate":    receipt.Duplicate,
			"skipped_runs": run.SkippedRuns,
		},
	})
	return run
}

// entryIDsOf extracts the journal entry IDs from a transfer receipt for
// audit linkage.
func entryIDsOf(r TransferReceipt) []string {
	ids := make([]string, 0, len(r.Entries))
	for _, e := range r.Entries {
		ids = append(ids, e.ID)
	}
	return ids
}

// validateScheduleRecord checks one schedule decoded from a snapshot:
// every field the constructor validates, so a corrupt or hand-edited
// snapshot can never resurrect a schedule that CreateTransferSchedule
// would have rejected.
func validateScheduleRecord(s *TransferSchedule) error {
	if s.ID == "" {
		return ErrScheduleEmptyID
	}
	if s.From == "" || s.To == "" {
		return ErrScheduleEmptyAccount
	}
	if s.From == s.To {
		return ErrScheduleSameAccount
	}
	if s.AmountCents <= 0 {
		return ErrScheduleNonPositiveAmount
	}
	if _, err := normalizeCurrency(s.Currency); err != nil {
		return err
	}
	if s.ToCurrency != "" {
		if _, err := normalizeCurrency(s.ToCurrency); err != nil {
			return err
		}
	}
	if s.FeeCents < 0 || (s.FeeCents > 0 && s.FeeAccount == "") {
		return ErrScheduleBadFee
	}
	if err := checkMemoLength(s.Memo); err != nil {
		return err
	}
	if s.Interval <= 0 {
		return ErrScheduleBadInterval
	}
	if s.NextRunAt.IsZero() {
		return ErrScheduleBadNextRunAt
	}
	if !s.EndsAt.IsZero() && !s.EndsAt.After(s.NextRunAt) {
		return ErrScheduleBadEndsAt
	}
	switch s.Status {
	case ScheduleActive, SchedulePaused, ScheduleCancelled, ScheduleCompleted:
	default:
		return fmt.Errorf("ledger: schedule %q has invalid status %q", s.ID, s.Status)
	}
	return nil
}
