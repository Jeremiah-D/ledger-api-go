package ledger

import (
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Period auto-close plans (LG-151).
//
// Operators can register a plan that closes an accounting period at a
// scheduled instant instead of closing it by hand: month-end compliance
// without an operator sitting at the terminal at midnight. A plan names
// the "YYYY-MM" period, the UTC instant it should close, and whether the
// close should book the LG-48 closing entries (withClosing=true) or just
// lock the period (withClosing=false).
//
// A background sweeper (see PeriodAutoCloseSweeper below, wired to
// LEDGER_PERIOD_AUTOCLOSE_INTERVAL) fires due plans by calling the same
// close path the POST /periods/{id}/close endpoint uses, so the gates are
// identical: withClosing reuses ClosePeriodWithClosing, which leaves the
// period OPEN and records nothing when any gate fails (frozen account,
// overdraft, next month closed, ...). A failed auto-close therefore marks
// the plan failed and keeps the period open — never a silent half-close.
//
// Plans are structural config: they do not bump the ledger version and
// they round-trip through disaster-recovery snapshots. Every plan
// transition (scheduled, cancelled, executed, failed) is recorded in the
// structured audit chain, so an unattended close is tamper-evident
// exactly like an operator's click.

// Plan statuses for PeriodAutoClosePlan.
const (
	// PeriodAutoClosePending marks a plan registered but not yet fired.
	PeriodAutoClosePending = "pending"
	// PeriodAutoCloseExecuted marks a plan whose close succeeded.
	PeriodAutoCloseExecuted = "executed"
	// PeriodAutoCloseCancelled marks a plan cancelled before firing.
	PeriodAutoCloseCancelled = "cancelled"
	// PeriodAutoCloseFailed marks a plan whose close failed; the period
	// stays open and the failure reason is on the plan.
	PeriodAutoCloseFailed = "failed"
)

// periodAutoCloseExecuting is the internal transitional status a sweeper
// tick assigns before running the close outside the lock, so two
// concurrent sweeps never fire the same plan twice. It never surfaces
// through the query methods: a plan left in this state by a crashed
// sweeper is re-driven as pending on the next tick (see
// SweepPeriodAutoCloses).
const periodAutoCloseExecuting = "executing"

// Validation errors returned by period auto-close plan operations.
var (
	// ErrAutoClosePastTime is returned by SchedulePeriodAutoClose when
	// the requested close instant is not in the future.
	ErrAutoClosePastTime = errors.New("ledger: auto-close time must be in the future")
	// ErrAutoCloseNoPlan is returned by CancelPeriodAutoClose and the
	// plan query when no plan exists for the period.
	ErrAutoCloseNoPlan = errors.New("ledger: no auto-close plan for period")
	// ErrAutoCloseDuplicate is returned by SchedulePeriodAutoClose when
	// a pending plan already exists for the period.
	ErrAutoCloseDuplicate = errors.New("ledger: auto-close plan already pending for period")
	// ErrAutoCloseNotPending is returned by CancelPeriodAutoClose when
	// the plan already fired or was cancelled.
	ErrAutoCloseNotPending = errors.New("ledger: auto-close plan is not pending")
)

// PeriodAutoClosePlan is one registered period auto-close plan. Plans are
// keyed by period ID: at most one live (pending) plan per period.
type PeriodAutoClosePlan struct {
	// PeriodID is the "YYYY-MM" accounting period to close.
	PeriodID string `json:"period"`
	// CloseAt is the UTC instant the sweeper fires the close at.
	CloseAt time.Time `json:"close_at"`
	// WithClosing books the LG-48 closing entries (P&L into
	// retained earnings) in addition to locking the period.
	WithClosing bool `json:"with_closing"`
	// Status is one of the PeriodAutoClose* constants above.
	Status string `json:"status"`
	// CreatedAt is when the plan was registered.
	CreatedAt time.Time `json:"created_at"`
	// DecidedAt is when the plan reached a terminal status
	// (executed, cancelled, failed); zero while pending.
	DecidedAt time.Time `json:"decided_at,omitempty"`
	// FailureReason carries the close error when Status is
	// PeriodAutoCloseFailed.
	FailureReason string `json:"failure_reason,omitempty"`
}

// copy returns a defensive copy of the plan.
func (p *PeriodAutoClosePlan) copy() *PeriodAutoClosePlan {
	cp := *p
	return &cp
}

// SchedulePeriodAutoClose registers a plan to close periodID at the UTC
// instant at. withClosing selects the LG-48 closing-entries close over
// the plain period lock. The instant must be in the future; only one
// pending plan per period is allowed (re-registering over a terminal
// plan replaces it). The registration is recorded in the structured
// audit chain (op "period_autoclose_scheduled").
func (l *Ledger) SchedulePeriodAutoClose(periodID string, at time.Time, withClosing bool) (*PeriodAutoClosePlan, error) {
	return l.SchedulePeriodAutoCloseCtx(context.Background(), periodID, at, withClosing)
}

// SchedulePeriodAutoCloseCtx is SchedulePeriodAutoClose with per-call
// audit actor attribution (LG-73).
func (l *Ledger) SchedulePeriodAutoCloseCtx(ctx context.Context, periodID string, at time.Time, withClosing bool) (*PeriodAutoClosePlan, error) {
	if err := validatePeriodID(periodID); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if !at.After(now) {
		return nil, ErrAutoClosePastTime
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if existing, ok := l.periodAutoCloses[periodID]; ok &&
		(existing.Status == PeriodAutoClosePending || existing.Status == periodAutoCloseExecuting) {
		return nil, ErrAutoCloseDuplicate
	}
	plan := &PeriodAutoClosePlan{
		PeriodID:    periodID,
		CloseAt:     at.UTC(),
		WithClosing: withClosing,
		Status:      PeriodAutoClosePending,
		CreatedAt:   now,
	}
	l.periodAutoCloses[periodID] = plan
	l.emitAuditCtx(ctx, AuditEvent{
		Op:      "period_autoclose_scheduled",
		Actor:   "SchedulePeriodAutoClose",
		TraceID: periodID,
		Details: map[string]any{
			"close_at":     plan.CloseAt.Format(time.RFC3339),
			"with_closing": withClosing,
		},
	})
	return plan.copy(), nil
}

// CancelPeriodAutoClose cancels a pending plan before it fires. The
// cancellation is recorded in the structured audit chain (op
// "period_autoclose_cancelled").
func (l *Ledger) CancelPeriodAutoClose(periodID string) (*PeriodAutoClosePlan, error) {
	return l.CancelPeriodAutoCloseCtx(context.Background(), periodID)
}

// CancelPeriodAutoCloseCtx is CancelPeriodAutoClose with per-call audit
// actor attribution (LG-73).
func (l *Ledger) CancelPeriodAutoCloseCtx(ctx context.Context, periodID string) (*PeriodAutoClosePlan, error) {
	if err := validatePeriodID(periodID); err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	plan, ok := l.periodAutoCloses[periodID]
	if !ok {
		return nil, ErrAutoCloseNoPlan
	}
	if plan.Status != PeriodAutoClosePending {
		return nil, ErrAutoCloseNotPending
	}
	plan.Status = PeriodAutoCloseCancelled
	plan.DecidedAt = time.Now().UTC()
	l.emitAuditCtx(ctx, AuditEvent{
		Op:      "period_autoclose_cancelled",
		Actor:   "CancelPeriodAutoClose",
		TraceID: periodID,
		Details: map[string]any{
			"close_at":     plan.CloseAt.Format(time.RFC3339),
			"with_closing": plan.WithClosing,
		},
	})
	return plan.copy(), nil
}

// PeriodAutoClosePlan returns the plan registered for periodID, or false
// when none exists.
func (l *Ledger) PeriodAutoClosePlan(periodID string) (*PeriodAutoClosePlan, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	plan, ok := l.periodAutoCloses[periodID]
	if !ok {
		return nil, false
	}
	return plan.copy(), true
}

// PeriodAutoClosePlans returns every registered plan, sorted by period
// ID. Plans are small and few, so a defensive copy per plan is cheap.
func (l *Ledger) PeriodAutoClosePlans() []*PeriodAutoClosePlan {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.periodAutoClosesLocked()
}

// periodAutoClosePlansLocked is the unlocked variant; callers must hold
// l.mu (either lock suffices).
func (l *Ledger) periodAutoClosesLocked() []*PeriodAutoClosePlan {
	out := make([]*PeriodAutoClosePlan, 0, len(l.periodAutoCloses))
	for _, p := range l.periodAutoCloses {
		out = append(out, p.copy())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PeriodID < out[j].PeriodID })
	return out
}

// SweepPeriodAutoCloses fires every pending plan with CloseAt <= now and
// returns the number of plans it decided (executed + failed). Due plans
// are marked periodAutoCloseExecuting under the lock first, then each
// close runs outside the lock through the same path as POST
// /periods/{id}/close; the terminal status is recorded afterwards under
// the lock with its audit event. A plan found in the executing state is
// re-driven as pending: a crashed sweeper never leaves a plan stuck.
//
// The sweep never panics on a close failure: the plan is marked failed
// with the error reason, the period stays open (ClosePeriodWithClosing
// leaves no half-close), and the failure is audited (op
// "period_autoclose_failed"). Success is audited as
// "period_autoclose_executed" with the closing-entry count.
func (l *Ledger) SweepPeriodAutoCloses(now time.Time) int {
	return l.sweepPeriodAutoClosesCtx(context.Background(), now)
}

// sweepPeriodAutoClosesCtx is SweepPeriodAutoCloses with a caller context
// for audit actor attribution.
func (l *Ledger) sweepPeriodAutoClosesCtx(ctx context.Context, now time.Time) int {
	l.mu.Lock()
	var due []*PeriodAutoClosePlan
	for id, plan := range l.periodAutoCloses {
		if plan.Status == periodAutoCloseExecuting {
			// A previous sweep died mid-execution: re-drive as pending.
			plan.Status = PeriodAutoClosePending
		}
		if plan.Status != PeriodAutoClosePending {
			continue
		}
		if !plan.CloseAt.After(now) {
			plan.Status = periodAutoCloseExecuting
			due = append(due, plan)
			_ = id
		}
	}
	l.mu.Unlock()

	for _, plan := range due {
		l.executePeriodAutoClose(ctx, plan, now)
	}
	return len(due)
}

// executePeriodAutoClose runs one due plan's close and records the
// terminal status. The close itself takes l.mu, so this must be called
// without the lock held.
func (l *Ledger) executePeriodAutoClose(ctx context.Context, plan *PeriodAutoClosePlan, now time.Time) {
	var closingEntries int
	var closeErr error
	if plan.WithClosing {
		var receipt ClosingReceipt
		receipt, closeErr = l.ClosePeriodWithClosing(plan.PeriodID)
		if closeErr == nil {
			closingEntries = len(receipt.Entries)
		}
	} else {
		closeErr = l.ClosePeriod(plan.PeriodID)
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	// The plan may have been re-registered while the close ran (an
	// operator can replace terminal plans): only touch it if it is still
	// the executing plan this sweep marked.
	current, ok := l.periodAutoCloses[plan.PeriodID]
	if !ok || current != plan {
		return
	}
	details := map[string]any{
		"close_at":        plan.CloseAt.Format(time.RFC3339),
		"with_closing":    plan.WithClosing,
		"closing_entries": closingEntries,
	}
	if closeErr != nil {
		plan.Status = PeriodAutoCloseFailed
		plan.FailureReason = closeErr.Error()
		plan.DecidedAt = now
		details["error"] = closeErr.Error()
		l.emitAuditCtx(ctx, AuditEvent{
			Op:      "period_autoclose_failed",
			Actor:   "PeriodAutoCloseSweeper",
			TraceID: plan.PeriodID,
			Details: details,
		})
		return
	}
	plan.Status = PeriodAutoCloseExecuted
	plan.DecidedAt = now
	l.emitAuditCtx(ctx, AuditEvent{
		Op:      "period_autoclose_executed",
		Actor:   "PeriodAutoCloseSweeper",
		TraceID: plan.PeriodID,
		Details: details,
	})
}

// PeriodAutoCloseSweeper is the background worker that fires due period
// auto-close plans (LG-151). A single goroutine calls
// SweepPeriodAutoCloses on a ticker until its context is cancelled or
// Stop is called — the same shape as the hold-expiry sweeper
// (see hold_sweep.go). Firing is delegated to the plan machinery, so a
// sweep never touches a period whose plan is pending-but-not-due, and
// each tick's decided plans (executed + failed) are reported to onSweep
// so the HTTP server can feed ledger_period_autocloses_total.
//
// The first tick fires after one full interval, not immediately, so a
// freshly started process settles before it starts closing periods. Plans
// whose close failed are terminal: they are not retried on later ticks,
// and the period stays open for the operator to close by hand.
type PeriodAutoCloseSweeper struct {
	l        *Ledger
	interval time.Duration
	onSweep  func(decided int)

	done     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	ticks        atomic.Uint64
	decidedTotal atomic.Uint64
}

// StartPeriodAutoCloseSweeper launches the background goroutine. A
// non-positive interval disables the sweeper: it returns nil and starts
// nothing, so callers can pass a configured interval straight through
// without branching.
func StartPeriodAutoCloseSweeper(ctx context.Context, l *Ledger, interval time.Duration, onSweep func(decided int)) *PeriodAutoCloseSweeper {
	if interval <= 0 {
		return nil
	}
	sw := &PeriodAutoCloseSweeper{
		l:        l,
		interval: interval,
		onSweep:  onSweep,
		done:     make(chan struct{}),
	}
	sw.wg.Add(1)
	go sw.run(ctx)
	return sw
}

func (sw *PeriodAutoCloseSweeper) run(ctx context.Context) {
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
			decided := sw.l.SweepPeriodAutoCloses(time.Now().UTC())
			sw.ticks.Add(1)
			sw.decidedTotal.Add(uint64(decided))
			if sw.onSweep != nil {
				sw.onSweep(decided)
			}
		}
	}
}

// Stop halts the sweeper and waits for its goroutine to exit. It is
// idempotent and safe to call alongside context cancellation.
func (sw *PeriodAutoCloseSweeper) Stop() {
	sw.stopOnce.Do(func() { close(sw.done) })
	sw.wg.Wait()
}

// Ticks reports how many sweeps have completed. DecidedTotal reports how
// many plans have been decided (executed + failed) across all ticks.
func (sw *PeriodAutoCloseSweeper) Ticks() uint64        { return sw.ticks.Load() }
func (sw *PeriodAutoCloseSweeper) DecidedTotal() uint64 { return sw.decidedTotal.Load() }
