package ledger

import (
	"context"
	"fmt"
)

// Transfer-schedule per-run fee amendment (LG-256).
//
// CreateTransferSchedule fixes a plan's per-run fee at creation
// (LG-41); Pause/Resume/Cancel own the lifecycle transitions. This
// module is the orthogonal in-place fee path, alongside the amount
// amendment (LG-253), the interval amendment (LG-254), and the
// end-time amendment (LG-255): an operator revises a live plan's
// per-run fee instead of cancelling it and creating a new one.
// Amending in place matters for the same reason as those
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
//     the fee revision simply waits with the plan and governs the
//     first run after resume, exactly as it would govern the
//     next run of an active plan.
//   - The new fee reuses the creation gate (see
//     CreateTransferSchedule), via ErrScheduleBadFee: it must be
//     non-negative, and a positive fee requires an existing
//     FeeAccount. FeeAccount itself is NOT changed by this
//     amendment — there is no fee-account amendment, so setting a
//     positive fee on a plan with no FeeAccount is rejected and the
//     original fee is left exactly as it was. Amending to zero is
//     always legal: it drops the explicit fee (the ledger's fee
//     policy then applies at fire time, unless SkipFee is set,
//     exactly as at creation) without touching FeeAccount or
//     SkipFee, so a later amendment back to a positive fee still
//     has the account to charge.
//   - The amendment touches FeeCents and nothing else: the legs,
//     amount, currency, fee account, SkipFee flag, interval, next
//     run time, end time, run sequence, and run history are
//     unchanged. There is no recomputation to do — unlike the
//     interval amendment (LG-254) there is no derived date to
//     move — so the revision is a single-field write.
//   - fireSchedule builds each run's transfer from the schedule
//     snapshot SweepDue takes at fire time (see SweepDue), so the
//     new fee governs the next successful firing with no
//     firing-path change: already-fired runs are journaled
//     transfers whose fee legs are never rewritten, and a failed
//     run's retry reads the amended fee — the failed run posted
//     nothing, so the retry books exactly once, at the new fee,
//     under the same "<ID>/run/<seq>" idempotency key.
//   - Amending to the current fee is a no-op: the row is
//     untouched and no audit event fires, so a retried or redundant
//     revision does not pollute the audit trail with a change that
//     changed nothing. The no-op check runs before the fee gate,
//     so restating the current fee never errors.
//   - Like the other schedule transitions, the amendment is
//     structural: it bumps neither the ledger version nor the audit
//     chain, and no outbox event fires — the plan was already
//     reported when created. The amendment lands one audit event
//     (transfer_schedule_fee_amended, with the schedule ID, the
//     previous and new fees, the fee account, the currency, and
//     the run sequence) so the "who moved the per-run fee, and
//     from what to what" trail is tamper-evident. Schedules are
//     snapshot records, so the amended fee rides the schedule row
//     through snapshots exactly like the original — the omitempty
//     fee_cents field carries it, and a fee cleared to zero
//     round-trips as zero — no snapshot change is needed.
//
// Orthogonality: CreateTransferSchedule owns the initial fee,
// Pause/Resume/Cancel own the lifecycle, SweepDue owns firing, and
// this module owns only the fee revision. It composes with the
// amount amendment (LG-253), the interval amendment (LG-254), and
// the end-time amendment (LG-255): each touches only its own field.

// AmendTransferScheduleFee revises a live transfer schedule's
// per-run fee in place. Only active or paused schedules may be
// amended; unknown IDs fail with ErrScheduleNotFound and cancelled
// or completed schedules fail with ErrScheduleBadTransition. The
// new fee must be non-negative and a positive fee requires an
// existing FeeAccount (ErrScheduleBadFee); FeeAccount itself is not
// changed. Only FeeCents changes; the legs, amount, currency, fee
// account, SkipFee flag, interval, next run time, end time, run
// sequence, and run history are untouched, and already-fired runs
// are never rewritten — the new fee governs the next successful
// firing. Amending to the current fee is a no-op. Amending is
// off-journal — it does not bump the ledger version — but it is
// audited as transfer_schedule_fee_amended.
func (l *Ledger) AmendTransferScheduleFee(id string, feeCents int64) (TransferSchedule, error) {
	return l.AmendTransferScheduleFeeCtx(context.Background(), id, feeCents)
}

// AmendTransferScheduleFeeCtx is AmendTransferScheduleFee with
// per-call audit actor attribution (LG-73).
func (l *Ledger) AmendTransferScheduleFeeCtx(ctx context.Context, id string, feeCents int64) (TransferSchedule, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	sched, ok := l.schedules[id]
	if !ok {
		return TransferSchedule{}, ErrScheduleNotFound
	}
	if sched.Status != ScheduleActive && sched.Status != SchedulePaused {
		return TransferSchedule{}, fmt.Errorf("%w: cannot amend a %s schedule's fee", ErrScheduleBadTransition, sched.Status)
	}
	if sched.FeeCents == feeCents {
		return *sched, nil
	}
	if feeCents < 0 || (feeCents > 0 && sched.FeeAccount == "") {
		return TransferSchedule{}, ErrScheduleBadFee
	}
	previous := sched.FeeCents
	sched.FeeCents = feeCents
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "transfer_schedule_fee_amended",
		Actor:         "AmendTransferScheduleFee",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{sched.From, sched.To},
		Details: map[string]any{
			"schedule_id":        id,
			"previous_fee_cents": previous,
			"fee_cents":          feeCents,
			"fee_account":        string(sched.FeeAccount),
			"currency":           sched.Currency,
			"run_seq":            sched.RunSeq,
		},
	})
	return *sched, nil
}
