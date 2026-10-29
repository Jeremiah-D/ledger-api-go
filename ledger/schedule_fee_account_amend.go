package ledger

import (
	"context"
	"fmt"
)

// Transfer-schedule fee-account amendment (LG-257).
//
// CreateTransferSchedule fixes a plan's fee account at creation
// (LG-41); Pause/Resume/Cancel own the lifecycle transitions. This
// module is the orthogonal in-place fee-account path, alongside the
// amount amendment (LG-253), the interval amendment (LG-254), the
// end-time amendment (LG-255), and the fee amendment (LG-256): an
// operator revises a live plan's fee account instead of cancelling
// it and creating a new one. Amending in place matters for the same
// reason as those amendments: cancelling plus re-creating is a new
// plan — a fresh ID that breaks the plan's run history and its
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
//     the account revision simply waits with the plan and governs
//     the first run after resume, exactly as it would govern the
//     next run of an active plan.
//   - The new account reuses the creation gate (see
//     CreateTransferSchedule), via ErrScheduleBadFee: when the
//     plan's current FeeCents is positive, the new account must be
//     non-empty — clearing the account would strand a positive fee
//     with nowhere to book — so a clearing attempt is rejected
//     and the original account is left exactly as it was. When the
//     fee is zero, an empty value is legal: it clears the account
//     back to the ledger's fee policy at fire time, exactly as a
//     plan created with no fee account behaves (unless SkipFee is
//     set, exactly as at creation), and a later amendment can
//     restore an account before any positive fee is set again.
//   - The amendment touches FeeAccount and nothing else: the legs,
//     amount, currency, fee amount, SkipFee flag, interval, next
//     run time, end time, run sequence, and run history are
//     unchanged. There is no recomputation to do — unlike the
//     interval amendment (LG-254) there is no derived date to
//     move — so the revision is a single-field write.
//   - fireSchedule builds each run's transfer from the schedule
//     snapshot SweepDue takes at fire time (see SweepDue), so the
//     new account governs the next successful firing with no
//     firing-path change: already-fired runs are journaled
//     transfers whose fee legs are never rewritten, and a failed
//     run's retry reads the amended account — the failed run posted
//     nothing, so the retry books exactly once, to the new account,
//     under the same "<ID>/run/<seq>" idempotency key.
//   - Amending to the current account is a no-op: the row is
//     untouched and no audit event fires, so a retried or redundant
//     revision does not pollute the audit trail with a change that
//     changed nothing. The no-op check runs before the fee gate,
//     so restating the current account never errors.
//   - Like the other schedule transitions, the amendment is
//     structural: it bumps neither the ledger version nor the audit
//     chain, and no outbox event fires — the plan was already
//     reported when created. The amendment lands one audit event
//     (transfer_schedule_fee_account_amended, with the schedule ID,
//     the previous and new fee accounts, the fee amount, the
//     currency, and the run sequence) so the "who moved the fee
//     account, and from what to what" trail is tamper-evident.
//     Schedules are snapshot records, so the amended account rides
//     the schedule row through snapshots exactly like the
//     original — the omitempty fee_account field carries it, and an
//     account cleared to empty round-trips as empty — no snapshot
//     change is needed.
//
// Orthogonality: CreateTransferSchedule owns the initial fee
// account, Pause/Resume/Cancel own the lifecycle, SweepDue owns
// firing, and this module owns only the fee-account revision. It
// composes with the fee amendment (LG-256): that changes the fee
// amount, this changes where the fee books — each touches only its
// own field, and the two gates interlock (a positive fee needs an
// account; a cleared account blocks a later positive fee until an
// account is restored).

// AmendTransferScheduleFeeAccount revises a live transfer
// schedule's fee account in place. Only active or paused schedules
// may be amended; unknown IDs fail with ErrScheduleNotFound and
// cancelled or completed schedules fail with ErrScheduleBadTransition.
// When the plan's current fee is positive, the new account must be
// non-empty (ErrScheduleBadFee); when the fee is zero, an empty
// value is legal and clears the account back to ledger-fee policy.
// Only FeeAccount changes; the legs, amount, currency, fee amount,
// SkipFee flag, interval, next run time, end time, run sequence,
// and run history are untouched, and already-fired runs are never
// rewritten — the new account governs the next successful firing.
// Amending to the current account is a no-op. Amending is
// off-journal — it does not bump the ledger version — but it is
// audited as transfer_schedule_fee_account_amended.
func (l *Ledger) AmendTransferScheduleFeeAccount(id string, feeAccount AccountID) (TransferSchedule, error) {
	return l.AmendTransferScheduleFeeAccountCtx(context.Background(), id, feeAccount)
}

// AmendTransferScheduleFeeAccountCtx is
// AmendTransferScheduleFeeAccount with per-call audit actor
// attribution (LG-73).
func (l *Ledger) AmendTransferScheduleFeeAccountCtx(ctx context.Context, id string, feeAccount AccountID) (TransferSchedule, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	sched, ok := l.schedules[id]
	if !ok {
		return TransferSchedule{}, ErrScheduleNotFound
	}
	if sched.Status != ScheduleActive && sched.Status != SchedulePaused {
		return TransferSchedule{}, fmt.Errorf("%w: cannot amend a %s schedule's fee account", ErrScheduleBadTransition, sched.Status)
	}
	if sched.FeeAccount == feeAccount {
		return *sched, nil
	}
	if sched.FeeCents > 0 && feeAccount == "" {
		return TransferSchedule{}, ErrScheduleBadFee
	}
	previous := sched.FeeAccount
	sched.FeeAccount = feeAccount
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "transfer_schedule_fee_account_amended",
		Actor:         "AmendTransferScheduleFeeAccount",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{sched.From, sched.To},
		Details: map[string]any{
			"schedule_id":          id,
			"previous_fee_account": string(previous),
			"fee_account":          string(feeAccount),
			"fee_cents":            sched.FeeCents,
			"currency":             sched.Currency,
			"run_seq":              sched.RunSeq,
		},
	})
	return *sched, nil
}
