package ledger

import (
	"context"
	"fmt"
)

// Transfer-schedule business-day cadence amendment (LG-260).
//
// CreateTransferSchedule fixes a plan's advance rhythm at creation
// (LG-95): either a fixed duration (BusinessDayCadence false) or a
// business-day cadence (true — after firing, NextRunAt advances T+N
// business days, N the interval in whole days, a sub-day interval
// advancing one business day, so runs never land on a weekend or a
// holiday). Pause/Resume/Cancel own the lifecycle transitions. This
// module is the orthogonal in-place cadence path, alongside the
// amount amendment (LG-253), the interval amendment (LG-254), the
// end-time amendment (LG-255), the fee amendment (LG-256), the
// fee-account amendment (LG-257), the skip-fee amendment (LG-258),
// and the memo amendment (LG-259): an operator revises a live
// plan's rhythm instead of cancelling it and creating a new one.
// Amending in place matters for the same reason as those
// amendments: cancelling plus re-creating is a new plan — a fresh
// ID that breaks the plan's run history and its "<ID>/run/<seq>"
// idempotency lineage — while an amendment keeps the same plan,
// the same legs, and one audit trail from creation to completion.
//
// Semantics, deliberately fail-closed throughout:
//
//   - Only an active or paused plan may be amended. An unknown ID
//     fails with ErrScheduleNotFound; a cancelled or completed plan
//     fails with ErrScheduleBadTransition. A paused plan is
//     amendable because pausing is a hold, not a terminal state:
//     the rhythm revision simply waits with the plan and governs
//     the first run after resume, exactly as it would govern the
//     next run of an active plan.
//   - A boolean is always legal, so there is no new validation
//     gate (along the skip-fee amendment, LG-258): switching the
//     rhythm on is legal on any plan, and switching it off
//     restores exactly the fixed-duration advance the plan had
//     before, since the interval itself is never changed by this
//     amendment.
//   - The amendment touches BusinessDayCadence and nothing else:
//     the legs, amount, currency, fee amount, fee account,
//     skip-fee switch, memo, interval, next run time, end time,
//     run sequence, and run history are unchanged. In particular
//     there is no recomputation at amend time — unlike the
//     interval amendment (LG-254) there is no derived date to
//     move: NextRunAt keeps its nominal value, and the new rhythm
//     only decides how the next firing advances it. An already
//     fired run's advance is never rewritten: a run fired under
//     the old rhythm keeps the NextRunAt that rhythm produced.
//   - fireSchedule builds each run's transfer from the schedule
//     snapshot SweepDue takes at fire time and reads the live
//     BusinessDayCadence when advancing (see fireSchedule), so
//     the new rhythm governs the next successful firing with no
//     firing-path change: switching on moves the next advance to
//     T+N business days (a Friday run's next run lands on Monday
//     instead of Saturday), switching off moves it back to a
//     fixed duration, and a failed run's retry reads the amended
//     rhythm — the failed run posted nothing and advanced
//     nothing, so the retry books exactly once and advances
//     under the amended rhythm, under the same "<ID>/run/<seq>"
//     idempotency key.
//   - Amending to the current rhythm value is a no-op: the row is
//     untouched and no audit event fires, so a retried or
//     redundant revision does not pollute the audit trail with a
//     change that changed nothing.
//   - Like the other schedule transitions, the amendment is
//     structural: it bumps neither the ledger version nor the audit
//     chain, and no outbox event fires — the plan was already
//     reported when created. The amendment lands one audit event
//     (transfer_schedule_business_day_cadence_amended, with the
//     schedule ID, the previous and new rhythm values, the
//     interval, the currency, and the run sequence) so the "who
//     flipped the settlement rhythm, and from what to what" trail
//     is tamper-evident. Schedules are snapshot records, so the
//     amended rhythm rides the schedule row through snapshots
//     exactly like the original — the omitempty
//     business_day_cadence field carries a switched-on value, and
//     a rhythm flipped back off round-trips as false — no
//     snapshot change is needed.
//
// Orthogonality: CreateTransferSchedule owns the initial rhythm,
// Pause/Resume/Cancel own the lifecycle, SweepDue owns firing, and
// this module owns only the rhythm revision. It composes with the
// interval amendment (LG-254): that changes how far apart runs
// are, this changes whether the gap counts calendar days or
// business days — each touches only its own field, and flipping
// the rhythm never moves a date by itself.

// AmendTransferScheduleBusinessDayCadence revises a live transfer
// schedule's advance rhythm in place. Only active or paused
// schedules may be amended; unknown IDs fail with
// ErrScheduleNotFound and cancelled or completed schedules fail
// with ErrScheduleBadTransition. A boolean is always legal — there
// is no new validation gate. Only BusinessDayCadence changes; the
// legs, amount, currency, fee, interval, next run time, end time,
// run sequence, and run history are untouched, and already-fired
// runs are never rewritten — the new rhythm governs the next
// successful firing's advance, with no recomputation at amend
// time. Amending to the current rhythm value is a no-op. Amending
// is off-journal — it does not bump the ledger version — but it is
// audited as transfer_schedule_business_day_cadence_amended.
func (l *Ledger) AmendTransferScheduleBusinessDayCadence(id string, cadence bool) (TransferSchedule, error) {
	return l.AmendTransferScheduleBusinessDayCadenceCtx(context.Background(), id, cadence)
}

// AmendTransferScheduleBusinessDayCadenceCtx is
// AmendTransferScheduleBusinessDayCadence with per-call audit actor
// attribution (LG-73).
func (l *Ledger) AmendTransferScheduleBusinessDayCadenceCtx(ctx context.Context, id string, cadence bool) (TransferSchedule, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	sched, ok := l.schedules[id]
	if !ok {
		return TransferSchedule{}, ErrScheduleNotFound
	}
	if sched.Status != ScheduleActive && sched.Status != SchedulePaused {
		return TransferSchedule{}, fmt.Errorf("%w: cannot amend a %s schedule's business-day cadence", ErrScheduleBadTransition, sched.Status)
	}
	if sched.BusinessDayCadence == cadence {
		return *sched, nil
	}
	previous := sched.BusinessDayCadence
	sched.BusinessDayCadence = cadence
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "transfer_schedule_business_day_cadence_amended",
		Actor:         "AmendTransferScheduleBusinessDayCadence",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{sched.From, sched.To},
		Details: map[string]any{
			"schedule_id":                   id,
			"previous_business_day_cadence": previous,
			"business_day_cadence":          cadence,
			"interval":                      sched.Interval.String(),
			"currency":                      sched.Currency,
			"run_seq":                       sched.RunSeq,
		},
	})
	return *sched, nil
}
