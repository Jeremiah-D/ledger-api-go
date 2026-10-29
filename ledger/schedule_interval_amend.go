package ledger

import (
	"context"
	"fmt"
	"time"
)

// Transfer-schedule interval amendment (LG-254).
//
// CreateTransferSchedule fixes a plan's cadence at creation (LG-41);
// Pause/Resume/Cancel own the lifecycle transitions. This module is
// the orthogonal in-place interval path, alongside the amount
// amendment (LG-253): an operator revises a live plan's cadence
// instead of cancelling it and creating a new one. Amending in place
// matters for the same reason as the amount amendment: cancelling
// plus re-creating is a new plan — a fresh ID that breaks the plan's
// run history and its "<ID>/run/<seq>" idempotency lineage — while
// an amendment keeps the same plan, the same legs, and one audit
// trail from creation to completion.
//
// Semantics, deliberately fail-closed throughout:
//
//   - Only an active or paused plan may be amended. An unknown ID
//     fails with ErrScheduleNotFound; a cancelled or completed plan
//     fails with ErrScheduleBadTransition. A paused plan is
//     amendable because pausing is a hold, not a terminal state:
//     the interval revision simply waits with the plan and governs
//     the first run after resume, exactly as it would govern the
//     next run of an active plan.
//   - The new interval reuses the creation gate (see
//     CreateTransferSchedule): it must be positive, via
//     ErrScheduleBadInterval. A failed amendment leaves the
//     original interval exactly as it was.
//   - The amendment touches Interval and nothing else: the legs,
//     amount, currency, fee fields, next run time, end time, run
//     sequence, and run history are unchanged. In particular
//     NextRunAt is not recomputed — the next release moment does
//     not move. The new interval takes effect only on the
//     advancement after that next release: fireSchedule builds
//     each run from the schedule snapshot SweepDue takes at fire
//     time (see SweepDue), so the advancement after the next
//     firing reads the amended interval with no firing-path
//     change. Already-fired runs are journaled transfers and are
//     never rewritten, and a failed run's retry keeps its
//     NextRunAt, so the retry fires at the same nominal time and
//     only then advances by the new interval.
//   - For a business-day cadence plan (LG-95), the day conversion
//     reads the interval at fire time too: the advancement after
//     the next firing is T+N business days where N is the new
//     interval in whole days (a sub-day new interval advances one
//     business day), exactly as the fixed-cadence path reads the
//     new duration. No conversion is done at amend time.
//   - EndsAt completion is still judged on the advanced
//     NextRunAt: if the advancement after the next firing (by the
//     new interval) lands past EndsAt, the plan completes, just as
//     it would have under its original cadence.
//   - Amending to the current interval is a no-op: the row is
//     untouched and no audit event fires, so a retried or redundant
//     revision does not pollute the audit trail with a change that
//     changed nothing.
//   - Like the other schedule transitions, the amendment is
//     structural: it bumps neither the ledger version nor the audit
//     chain, and no outbox event fires — the plan was already
//     reported when created. The amendment lands one audit event
//     (transfer_schedule_interval_amended, with the schedule ID,
//     the previous and new intervals, the currency, and the run
//     sequence) so the "who moved the cadence, and from what to
//     what" trail is tamper-evident. Schedules are snapshot
//     records, so the amended interval rides the schedule row
//     through snapshots exactly like the original — the duration
//     string in MarshalJSON carries it — no snapshot change is
//     needed.
//
// Orthogonality: CreateTransferSchedule owns the initial interval,
// Pause/Resume/Cancel own the lifecycle, SweepDue owns firing, and
// this module owns only the interval revision. It composes with the
// amount amendment (LG-253): each touches only its own field.

// AmendTransferScheduleInterval revises a live transfer schedule's
// cadence in place. Only active or paused schedules may be amended;
// unknown IDs fail with ErrScheduleNotFound and cancelled or
// completed schedules fail with ErrScheduleBadTransition. The new
// interval must be positive (ErrScheduleBadInterval). Only Interval
// changes; the legs, amount, currency, fee fields, next run time,
// end time, run sequence, and run history are untouched, and
// NextRunAt is not recomputed — the new interval governs the
// advancement after the next release (including the business-day
// day conversion). Amending to the current interval is a no-op.
// Amending is off-journal — it does not bump the ledger version —
// but it is audited as transfer_schedule_interval_amended.
func (l *Ledger) AmendTransferScheduleInterval(id string, interval time.Duration) (TransferSchedule, error) {
	return l.AmendTransferScheduleIntervalCtx(context.Background(), id, interval)
}

// AmendTransferScheduleIntervalCtx is AmendTransferScheduleInterval
// with per-call audit actor attribution (LG-73).
func (l *Ledger) AmendTransferScheduleIntervalCtx(ctx context.Context, id string, interval time.Duration) (TransferSchedule, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	sched, ok := l.schedules[id]
	if !ok {
		return TransferSchedule{}, ErrScheduleNotFound
	}
	if sched.Status != ScheduleActive && sched.Status != SchedulePaused {
		return TransferSchedule{}, fmt.Errorf("%w: cannot amend a %s schedule's interval", ErrScheduleBadTransition, sched.Status)
	}
	if interval <= 0 {
		return TransferSchedule{}, ErrScheduleBadInterval
	}
	if sched.Interval == interval {
		return *sched, nil
	}
	previous := sched.Interval
	sched.Interval = interval
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "transfer_schedule_interval_amended",
		Actor:         "AmendTransferScheduleInterval",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{sched.From, sched.To},
		Details: map[string]any{
			"schedule_id":      id,
			"previous_interval": previous.String(),
			"interval":          interval.String(),
			"currency":          sched.Currency,
			"run_seq":           sched.RunSeq,
		},
	})
	return *sched, nil
}
