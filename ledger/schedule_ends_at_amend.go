package ledger

import (
	"context"
	"fmt"
	"time"
)

// Transfer-schedule end-time amendment (LG-255).
//
// CreateTransferSchedule fixes a plan's end time at creation (LG-41);
// Pause/Resume/Cancel own the lifecycle transitions. This module is
// the orthogonal in-place end-time path, alongside the amount
// amendment (LG-253) and the interval amendment (LG-254): an operator
// revises a live plan's cutoff instead of cancelling it and creating
// a new one. Amending in place matters for the same reason as those
// amendments: cancelling plus re-creating is a new plan — a fresh ID
// that breaks the plan's run history and its "<ID>/run/<seq>"
// idempotency lineage — while an amendment keeps the same plan, the
// same legs, and one audit trail from creation to completion.
//
// Semantics, deliberately fail-closed throughout:
//
//   - Only an active or paused plan may be amended. An unknown ID
//     fails with ErrScheduleNotFound; a cancelled or completed plan
//     fails with ErrScheduleBadTransition. A paused plan is
//     amendable because pausing is a hold, not a terminal state:
//     the end-time revision simply waits with the plan and governs
//     the first run after resume, exactly as it would govern the
//     next run of an active plan.
//   - The new end time reuses the creation gate (see
//     CreateTransferSchedule): a zero value is legal and means the
//     cutoff is removed — the plan becomes open-ended — and any
//     non-zero value must be after the plan's live NextRunAt, via
//     ErrScheduleBadEndsAt. "Live" is load-bearing: NextRunAt
//     advances on every firing, so the gate is judged against the
//     NextRunAt at amendment time, not the one the plan was created
//     with. A failed amendment leaves the original end time exactly
//     as it was.
//   - Amending to the current end time is a no-op: the row is
//     untouched and no audit event fires, so a retried or redundant
//     revision does not pollute the audit trail with a change that
//     changed nothing. The no-op check runs before the After gate,
//     so restating the current end time never errors even when the
//     live NextRunAt has since advanced to the end-time boundary
//     (an active plan whose NextRunAt merely equals its EndsAt has
//     not completed — completion needs NextRunAt past EndsAt).
//   - The amendment touches EndsAt and nothing else: the legs,
//     amount, currency, fee fields, interval, next run time, run
//     sequence, and run history are unchanged. In particular
//     NextRunAt is not recomputed — the next release moment does
//     not move, and already-fired runs are journaled transfers
//     that are never rewritten.
//   - Completion is still judged on the advanced NextRunAt, with no
//     firing-path change: fireSchedule reads the live EndsAt at
//     fire time (see SweepDue), so after the next firing the plan
//     completes exactly when its advanced NextRunAt lands past the
//     amended end time, and a plan whose cutoff was cleared to zero
//     never completes on end time again. A failed run's retry keeps
//     its NextRunAt, so the retry fires at the same nominal time
//     and only then advances — against the amended end time.
//   - Like the other schedule transitions, the amendment is
//     structural: it bumps neither the ledger version nor the audit
//     chain, and no outbox event fires — the plan was already
//     reported when created. The amendment lands one audit event
//     (transfer_schedule_ends_at_amended, with the schedule ID,
//     the previous and new end times as RFC3339 strings, the
//     currency, and the run sequence) so the "who moved the cutoff,
//     and from what to what" trail is tamper-evident. Schedules
//     are snapshot records, so the amended end time rides the
//     schedule row through snapshots exactly like the original —
//     the omitempty ends_at field carries it, and a cleared
//     (zero) end time round-trips as zero — no snapshot change is
//     needed.
//
// Orthogonality: CreateTransferSchedule owns the initial end time,
// Pause/Resume/Cancel own the lifecycle, SweepDue owns firing, and
// this module owns only the end-time revision. It composes with the
// amount amendment (LG-253) and the interval amendment (LG-254):
// each touches only its own field.

// AmendTransferScheduleEndsAt revises a live transfer schedule's
// end time in place. Only active or paused schedules may be amended;
// unknown IDs fail with ErrScheduleNotFound and cancelled or
// completed schedules fail with ErrScheduleBadTransition. A zero end
// time removes the cutoff (open-ended); a non-zero end time must be
// after the live NextRunAt (ErrScheduleBadEndsAt). Only EndsAt
// changes; the legs, amount, currency, fee fields, interval, next
// run time, run sequence, and run history are untouched, and
// NextRunAt is not recomputed — completion is still judged on the
// NextRunAt advanced after the next firing. Amending to the current
// end time is a no-op. Amending is off-journal — it does not bump
// the ledger version — but it is audited as
// transfer_schedule_ends_at_amended.
func (l *Ledger) AmendTransferScheduleEndsAt(id string, endsAt time.Time) (TransferSchedule, error) {
	return l.AmendTransferScheduleEndsAtCtx(context.Background(), id, endsAt)
}

// AmendTransferScheduleEndsAtCtx is AmendTransferScheduleEndsAt
// with per-call audit actor attribution (LG-73).
func (l *Ledger) AmendTransferScheduleEndsAtCtx(ctx context.Context, id string, endsAt time.Time) (TransferSchedule, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	sched, ok := l.schedules[id]
	if !ok {
		return TransferSchedule{}, ErrScheduleNotFound
	}
	if sched.Status != ScheduleActive && sched.Status != SchedulePaused {
		return TransferSchedule{}, fmt.Errorf("%w: cannot amend a %s schedule's ends_at", ErrScheduleBadTransition, sched.Status)
	}
	if sched.EndsAt.Equal(endsAt) {
		return *sched, nil
	}
	if !endsAt.IsZero() && !endsAt.After(sched.NextRunAt) {
		return TransferSchedule{}, ErrScheduleBadEndsAt
	}
	previous := sched.EndsAt
	sched.EndsAt = endsAt
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "transfer_schedule_ends_at_amended",
		Actor:         "AmendTransferScheduleEndsAt",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{sched.From, sched.To},
		Details: map[string]any{
			"schedule_id":      id,
			"previous_ends_at": previous.UTC().Format(time.RFC3339),
			"ends_at":          endsAt.UTC().Format(time.RFC3339),
			"currency":         sched.Currency,
			"run_seq":          sched.RunSeq,
		},
	})
	return *sched, nil
}
