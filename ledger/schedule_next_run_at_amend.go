package ledger

import (
	"context"
	"fmt"
	"time"
)

// Transfer-schedule next-run-time amendment (LG-262).
//
// CreateTransferSchedule fixes a plan's next run time at creation
// (LG-41); Pause/Resume/Cancel own the lifecycle transitions. This
// module is the orthogonal in-place next-run-time path, alongside
// the amount amendment (LG-253), the interval amendment (LG-254),
// the end-time amendment (LG-255), the fee amendment (LG-256), the
// fee-account amendment (LG-257), the skip-fee amendment (LG-258),
// the memo amendment (LG-259), the business-day cadence amendment
// (LG-260), and the FX-pair amendment (LG-261): an operator revises
// a live plan's next release moment instead of cancelling it and
// creating a new one. Amending in place matters for the same reason
// as those amendments: cancelling plus re-creating is a new plan —
// a fresh ID that breaks the plan's run history and its
// "<ID>/run/<seq>" idempotency lineage — while an amendment keeps
// the same plan, the same legs, and one audit trail from creation
// to completion.
//
// Semantics, deliberately fail-closed throughout:
//
//   - Only an active or paused plan may be amended. An unknown ID
//     fails with ErrScheduleNotFound; a cancelled or completed plan
//     fails with ErrScheduleBadTransition. A paused plan is
//     amendable because pausing is a hold, not a terminal state:
//     the next-run revision simply waits with the plan and governs
//     the first run after resume, exactly as it would govern the
//     next run of an active plan. NextRunAt does not advance while
//     paused, so a paused plan's revision is never consumed by a
//     firing before resume.
//   - The new next run time reuses the creation gates (see
//     CreateTransferSchedule), judged against the live counterpart
//     — the symmetric mirror of the end-time amendment (LG-255),
//     which judges a new end time against the live NextRunAt:
//     a zero value fails with ErrScheduleBadNextRunAt (a plan
//     with no next run time can never fire), and a new time later
//     than the plan's live EndsAt, when one is set, fails with
//     ErrScheduleBadEndsAt — a next run past the cutoff could
//     never fire, it would complete the plan on sight. "Live" is
//     load-bearing on both sides: EndsAt may itself have been
//     amended since creation, so the gate is judged against the
//     EndsAt at amendment time, not the creation-time one. A
//     failed amendment leaves the original next run time exactly
//     as it was.
//   - Amending to the current next run time is a no-op: the row is
//     untouched and no audit event fires, so a retried or redundant
//     revision does not pollute the audit trail with a change that
//     changed nothing. The no-op check runs before the gates, so
//     restating the current next run time never errors — the same
//     convention as the end-time amendment (LG-255).
//   - The amendment touches NextRunAt and nothing else: the legs,
//     amount, currency, fee fields, FX pair, memo, interval,
//     cadence flag, end time, run sequence, and run history are
//     unchanged. There is no recomputation to do — unlike the
//     interval amendment (LG-254) there is no derived date to
//     move — so the revision is a single-field write. In
//     particular the amendment neither fires immediately nor
//     completes the plan immediately: a new time in the past does
//     not fire until the next sweep, and completion is still
//     judged on the NextRunAt advanced after a firing, against
//     the live EndsAt (see SweepDue).
//   - SweepDue reads the live NextRunAt at fire time, so the new
//     time governs the next firing with no firing-path change: a
//     new time already past fires on the next sweep, with missed
//     nominal runs counted as skipped by the existing skip
//     arithmetic (no backfill — a moved-up plan fires once, not
//     once per missed nominal), and a new time in the future
//     simply delays the next firing. A failed run's retry likewise
//     reads the amended time: the retry fires when the amended
//     NextRunAt is due, under the same "<ID>/run/<seq>"
//     idempotency key. Already-fired runs are journaled transfers
//     whose due times are never rewritten.
//   - Like the other schedule transitions, the amendment is
//     structural: it bumps neither the ledger version nor the audit
//     chain, and no outbox event fires — the plan was already
//     reported when created. The amendment lands one audit event
//     (transfer_schedule_next_run_at_amended, with the schedule ID,
//     the previous and new next run times as RFC3339 strings, the
//     currency, and the run sequence) so the "who moved the next
//     release, and from what to what" trail is tamper-evident.
//     Schedules are snapshot records, so the amended next run time
//     rides the schedule row through snapshots exactly like the
//     original — the next_run_at field carries it with no
//     omitempty gap — no snapshot change is needed.
//
// Orthogonality: CreateTransferSchedule owns the initial next run
// time, Pause/Resume/Cancel own the lifecycle, SweepDue owns
// firing, and this module owns only the next-run-time revision. It
// composes with the other single-field amendments: each touches
// only its own field, and moving the next release never moves a
// leg, an amount, a fee, a pair, or a date bound. It is orthogonal
// to the interval amendment (LG-254), which moves the spacing
// between runs, and to the end-time amendment (LG-255), which moves
// the cutoff — this item moves the single next release moment.

// AmendTransferScheduleNextRunAt revises a live transfer schedule's
// next run time in place. Only active or paused schedules may be
// amended; unknown IDs fail with ErrScheduleNotFound and cancelled
// or completed schedules fail with ErrScheduleBadTransition. A zero
// next run time fails with ErrScheduleBadNextRunAt, and a new time
// later than the plan's live end time (when one is set) fails with
// ErrScheduleBadEndsAt; every rejection leaves the original next
// run time untouched. Only NextRunAt changes; the legs, amount,
// currency, fee fields, FX pair, memo, interval, cadence flag, end
// time, run sequence, and run history are untouched, and the
// amendment neither fires immediately nor completes the plan — the
// new time governs the next sweep's firing, with missed nominal
// runs counted as skipped, not backfilled. Amending to the current
// next run time is a no-op. Amending is off-journal — it does not
// bump the ledger version — but it is audited as
// transfer_schedule_next_run_at_amended.
func (l *Ledger) AmendTransferScheduleNextRunAt(id string, nextRunAt time.Time) (TransferSchedule, error) {
	return l.AmendTransferScheduleNextRunAtCtx(context.Background(), id, nextRunAt)
}

// AmendTransferScheduleNextRunAtCtx is AmendTransferScheduleNextRunAt
// with per-call audit actor attribution (LG-73).
func (l *Ledger) AmendTransferScheduleNextRunAtCtx(ctx context.Context, id string, nextRunAt time.Time) (TransferSchedule, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	sched, ok := l.schedules[id]
	if !ok {
		return TransferSchedule{}, ErrScheduleNotFound
	}
	if sched.Status != ScheduleActive && sched.Status != SchedulePaused {
		return TransferSchedule{}, fmt.Errorf("%w: cannot amend a %s schedule's next run time", ErrScheduleBadTransition, sched.Status)
	}
	// No-op first, so restating the current next run time never
	// errors — the same convention as the end-time amendment.
	if sched.NextRunAt.Equal(nextRunAt) {
		return *sched, nil
	}
	// Creation gates reused, judged against the live counterpart:
	// a zero next run time can never fire, and a next run past a
	// set end time could never fire either — it would complete
	// the plan on sight.
	if nextRunAt.IsZero() {
		return TransferSchedule{}, ErrScheduleBadNextRunAt
	}
	if !sched.EndsAt.IsZero() && nextRunAt.After(sched.EndsAt) {
		return TransferSchedule{}, ErrScheduleBadEndsAt
	}
	previous := sched.NextRunAt
	sched.NextRunAt = nextRunAt
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "transfer_schedule_next_run_at_amended",
		Actor:         "AmendTransferScheduleNextRunAt",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{sched.From, sched.To},
		Details: map[string]any{
			"schedule_id":          id,
			"previous_next_run_at": previous.UTC().Format(time.RFC3339),
			"next_run_at":          nextRunAt.UTC().Format(time.RFC3339),
			"currency":             sched.Currency,
			"run_seq":              sched.RunSeq,
		},
	})
	return *sched, nil
}
