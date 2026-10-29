package ledger

import (
	"context"
	"fmt"
)

// Transfer-schedule per-run amount amendment (LG-253).
//
// CreateTransferSchedule fixes a plan's per-run amount at creation
// (LG-41); Pause/Resume/Cancel own the lifecycle transitions. This
// module is the orthogonal in-place amount path: an operator revises
// a live plan's per-run amount instead of cancelling it and creating
// a new one. Amending in place matters for the same reason as the
// term-deposit amendments (LG-247, LG-249–LG-252): cancelling plus
// re-creating is a new plan — a fresh ID that breaks the plan's run
// history and its "<ID>/run/<seq>" idempotency lineage — while an
// amendment keeps the same plan, the same legs, and one audit trail
// from creation to completion.
//
// Semantics, deliberately fail-closed throughout:
//
//   - Only an active or paused plan may be amended. An unknown ID
//     fails with ErrScheduleNotFound; a cancelled or completed plan
//     fails with ErrScheduleBadTransition. A paused plan is
//     amendable because pausing is a hold, not a terminal state:
//     the amount revision simply waits with the plan and governs
//     the first run after resume, exactly as it would govern the
//     next run of an active plan.
//   - The new amount reuses the creation gate (see
//     CreateTransferSchedule): it must be positive, via
//     ErrScheduleNonPositiveAmount. A failed amendment leaves the
//     original amount exactly as it was.
//   - The amendment touches AmountCents and nothing else: the legs,
//     currency, fee fields, interval, next run time, end time,
//     run sequence, and run history are unchanged. There is no
//     recomputation to do — unlike the term-days amendment (LG-252)
//     there is no derived date to move — so the revision is a
//     single-field write.
//   - fireSchedule builds each run's transfer from the schedule
//     snapshot SweepDue takes at fire time (see SweepDue), so the
//     new amount governs the next successful firing with no
//     firing-path change: already-fired runs are journaled
//     transfers and are never rewritten, and a failed run's retry
//     reads the amended amount — the failed run posted nothing, so
//     the retry books exactly once, at the new amount, under the
//     same "<ID>/run/<seq>" idempotency key.
//   - Amending to the current amount is a no-op: the row is
//     untouched and no audit event fires, so a retried or redundant
//     revision does not pollute the audit trail with a change that
//     changed nothing.
//   - Like the other schedule transitions, the amendment is
//     structural: it bumps neither the ledger version nor the audit
//     chain, and no outbox event fires — the plan was already
//     reported when created. The amendment lands one audit event
//     (transfer_schedule_amount_amended, with the schedule ID, the
//     previous and new amounts, the currency, the legs, and the run
//     sequence) so the "who moved the per-run amount, and from what
//     to what" trail is tamper-evident. Schedules are snapshot
//     records, so the amended amount rides the schedule row through
//     snapshots exactly like the original — no snapshot change is
//     needed.
//
// Orthogonality: CreateTransferSchedule owns the initial amount,
// Pause/Resume/Cancel own the lifecycle, SweepDue owns firing, and
// this module owns only the amount revision. The interval revision
// (LG-254) is a separate single-field amendment and composes with
// this one: each touches only its own field.

// AmendTransferScheduleAmount revises a live transfer schedule's
// per-run amount in place. Only active or paused schedules may be
// amended; unknown IDs fail with ErrScheduleNotFound and cancelled
// or completed schedules fail with ErrScheduleBadTransition. The
// new amount must be positive (ErrScheduleNonPositiveAmount).
// Only AmountCents changes; the legs, currency, fee fields,
// interval, next run time, end time, run sequence, and run history
// are untouched, and already-fired runs are never rewritten — the
// new amount governs the next successful firing. Amending to the
// current amount is a no-op. Amending is off-journal — it does not
// bump the ledger version — but it is audited as
// transfer_schedule_amount_amended.
func (l *Ledger) AmendTransferScheduleAmount(id string, amountCents int64) (TransferSchedule, error) {
	return l.AmendTransferScheduleAmountCtx(context.Background(), id, amountCents)
}

// AmendTransferScheduleAmountCtx is AmendTransferScheduleAmount with
// per-call audit actor attribution (LG-73).
func (l *Ledger) AmendTransferScheduleAmountCtx(ctx context.Context, id string, amountCents int64) (TransferSchedule, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	sched, ok := l.schedules[id]
	if !ok {
		return TransferSchedule{}, ErrScheduleNotFound
	}
	if sched.Status != ScheduleActive && sched.Status != SchedulePaused {
		return TransferSchedule{}, fmt.Errorf("%w: cannot amend a %s schedule's amount", ErrScheduleBadTransition, sched.Status)
	}
	if amountCents <= 0 {
		return TransferSchedule{}, ErrScheduleNonPositiveAmount
	}
	if sched.AmountCents == amountCents {
		return *sched, nil
	}
	previous := sched.AmountCents
	sched.AmountCents = amountCents
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "transfer_schedule_amount_amended",
		Actor:         "AmendTransferScheduleAmount",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{sched.From, sched.To},
		Details: map[string]any{
			"schedule_id":          id,
			"previous_amount_cents": previous,
			"amount_cents":          amountCents,
			"currency":              sched.Currency,
			"run_seq":               sched.RunSeq,
		},
	})
	return *sched, nil
}
