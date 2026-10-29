package ledger

import (
	"context"
	"fmt"
)

// Transfer-schedule skip-fee amendment (LG-258).
//
// CreateTransferSchedule fixes a plan's skip-fee switch at creation
// (LG-41); Pause/Resume/Cancel own the lifecycle transitions. This
// module is the orthogonal in-place skip-fee path, alongside the
// amount amendment (LG-253), the interval amendment (LG-254), the
// end-time amendment (LG-255), the fee amendment (LG-256), and the
// fee-account amendment (LG-257): an operator revises a live plan's
// skip-fee switch instead of cancelling it and creating a new one.
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
//     the switch revision simply waits with the plan and governs
//     the first run after resume, exactly as it would govern the
//     next run of an active plan.
//   - A boolean is always legal, so there is no new validation
//     gate: switching on is legal on a plan with a positive fee,
//     a zero fee, or no fee account, and switching off is legal on
//     any plan — the switch only suppresses the policy fee, it
//     never creates a fee that needs an account. The fee amount and
//     fee account are NOT changed by this amendment, so toggling
//     the switch off restores exactly the fee behaviour the plan
//     had before it was switched on, with no reconfiguration.
//   - The amendment touches SkipFee and nothing else: the legs,
//     amount, currency, fee amount, fee account, interval, next
//     run time, end time, run sequence, and run history are
//     unchanged. There is no recomputation to do — unlike the
//     interval amendment (LG-254) there is no derived date to
//     move — so the revision is a single-field write.
//   - fireSchedule builds each run's transfer from the schedule
//     snapshot SweepDue takes at fire time (see SweepDue), so the
//     new switch governs the next successful firing with no
//     firing-path change: already-fired runs are journaled
//     transfers whose fee legs are never rewritten, and a failed
//     run's retry reads the amended switch — the failed run posted
//     nothing, so the retry books exactly once, with or without a
//     policy fee leg as the amended switch dictates, under the same
//     "<ID>/run/<seq>" idempotency key. While the switch is on, a
//     firing books no policy fee leg — SkipFee suppresses the
//     ledger's default fee policy only (see Transfer.SkipFee); an
//     explicit FeeCents still books exactly as for a one-off
//     transfer, so switching the switch on a plan with an explicit
//     fee does not silently drop that fee — and the fee account is
//     untouched by the switch itself; switching it back off resumes
//     policy fee booking from the next firing.
//   - Amending to the current switch value is a no-op: the row is
//     untouched and no audit event fires, so a retried or redundant
//     revision does not pollute the audit trail with a change that
//     changed nothing.
//   - Like the other schedule transitions, the amendment is
//     structural: it bumps neither the ledger version nor the audit
//     chain, and no outbox event fires — the plan was already
//     reported when created. The amendment lands one audit event
//     (transfer_schedule_skip_fee_amended, with the schedule ID,
//     the previous and new switch values, the fee amount, the fee
//     account, the currency, and the run sequence) so the "who
//     flipped the fee switch, and from what to what" trail is
//     tamper-evident. Schedules are snapshot records, so the
//     amended switch rides the schedule row through snapshots
//     exactly like the original — the omitempty skip_fee field
//     carries a switched-on value, and a switch flipped back off
//     round-trips as false — no snapshot change is needed.
//
// Orthogonality: CreateTransferSchedule owns the initial switch,
// Pause/Resume/Cancel own the lifecycle, SweepDue owns firing, and
// this module owns only the switch revision. It composes with the
// fee amendment (LG-256) and the fee-account amendment (LG-257):
// those change what fee books and where it books; this changes
// whether any fee books at all — each touches only its own field,
// and toggling the switch never strands or resurrects a fee
// configuration.

// AmendTransferScheduleSkipFee revises a live transfer schedule's
// skip-fee switch in place. Only active or paused schedules may be
// amended; unknown IDs fail with ErrScheduleNotFound and cancelled
// or completed schedules fail with ErrScheduleBadTransition. A
// boolean is always legal — there is no new validation gate. Only
// SkipFee changes; the legs, amount, currency, fee amount, fee
// account, interval, next run time, end time, run sequence, and
// run history are untouched, and already-fired runs are never
// rewritten — the new switch governs the next successful firing.
// Amending to the current switch value is a no-op. Amending is
// off-journal — it does not bump the ledger version — but it is
// audited as transfer_schedule_skip_fee_amended.
func (l *Ledger) AmendTransferScheduleSkipFee(id string, skipFee bool) (TransferSchedule, error) {
	return l.AmendTransferScheduleSkipFeeCtx(context.Background(), id, skipFee)
}

// AmendTransferScheduleSkipFeeCtx is AmendTransferScheduleSkipFee
// with per-call audit actor attribution (LG-73).
func (l *Ledger) AmendTransferScheduleSkipFeeCtx(ctx context.Context, id string, skipFee bool) (TransferSchedule, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	sched, ok := l.schedules[id]
	if !ok {
		return TransferSchedule{}, ErrScheduleNotFound
	}
	if sched.Status != ScheduleActive && sched.Status != SchedulePaused {
		return TransferSchedule{}, fmt.Errorf("%w: cannot amend a %s schedule's skip-fee switch", ErrScheduleBadTransition, sched.Status)
	}
	if sched.SkipFee == skipFee {
		return *sched, nil
	}
	previous := sched.SkipFee
	sched.SkipFee = skipFee
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "transfer_schedule_skip_fee_amended",
		Actor:         "AmendTransferScheduleSkipFee",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{sched.From, sched.To},
		Details: map[string]any{
			"schedule_id":       id,
			"previous_skip_fee": previous,
			"skip_fee":          skipFee,
			"fee_cents":         sched.FeeCents,
			"fee_account":       string(sched.FeeAccount),
			"currency":          sched.Currency,
			"run_seq":           sched.RunSeq,
		},
	})
	return *sched, nil
}
