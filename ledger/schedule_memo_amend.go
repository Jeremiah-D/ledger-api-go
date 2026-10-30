package ledger

import (
	"context"
	"fmt"
)

// Transfer-schedule memo amendment (LG-259).
//
// CreateTransferSchedule fixes a plan's memo at creation (LG-41);
// Pause/Resume/Cancel own the lifecycle transitions. This module is
// the orthogonal in-place memo path, alongside the amount amendment
// (LG-253), the interval amendment (LG-254), the end-time amendment
// (LG-255), the fee amendment (LG-256), the fee-account amendment
// (LG-257), and the skip-fee amendment (LG-258): an operator revises
// a live plan's memo instead of cancelling it and creating a new
// one. Amending in place matters for the same reason as those
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
//     the memo revision simply waits with the plan and governs the
//     first run after resume, exactly as it would govern the next
//     run of an active plan.
//   - The new memo reuses the creation gate (checkMemoLength, the
//     same gate CreateTransferSchedule applies): a memo longer than
//     maxMemoRunes UTF-8 characters fails with ErrMemoTooLong and
//     the live memo is untouched. An empty string is legal and
//     means "clear": the plan's runs then carry no memo, exactly
//     as a plan created without one. The no-op judgement is by
//     equality, not by emptiness — amending to the memo the plan
//     already carries (including "" on a memo-less plan) is a
//     no-op that leaves the row untouched and fires no audit
//     event, so a retried or redundant revision does not pollute
//     the audit trail with a change that changed nothing.
//   - The amendment touches Memo and nothing else: the legs,
//     amount, currency, fee amount, fee account, skip-fee switch,
//     interval, next run time, end time, run sequence, and run
//     history are unchanged. There is no recomputation to do —
//     unlike the interval amendment (LG-254) there is no derived
//     date to move — so the revision is a single-field write.
//   - SweepDue snapshots the live schedule row under the lock at
//     fire time and fireSchedule builds each run's transfer from
//     that snapshot, so the new memo governs the next successful
//     firing with no firing-path change: the fired transfer's
//     principal entry carries the amended memo, already-fired runs
//     are journaled transfers whose entries are never rewritten,
//     and a failed run's retry reads the amended memo — the failed
//     run posted nothing, so the retry books exactly once, with
//     the amended memo, under the same "<ID>/run/<seq>"
//     idempotency key.
//   - Like the other schedule transitions, the amendment is
//     structural: it bumps neither the ledger version nor the audit
//     chain, and no outbox event fires — the plan was already
//     reported when created. The amendment lands one audit event
//     (transfer_schedule_memo_amended, with the schedule ID, the
//     previous and new memos, the currency, and the run sequence)
//     so the "who reworded the run memo, and from what to what"
//     trail is tamper-evident. Schedules are snapshot records, so
//     the amended memo rides the schedule row through snapshots
//     exactly like the original — the omitempty memo field carries
//     a set memo, and a memo cleared back to "" round-trips as ""
//     (the field is absent on the wire and empty on restore, which
//     is the same value) — no snapshot change is needed.
//
// Orthogonality: CreateTransferSchedule owns the initial memo,
// Pause/Resume/Cancel own the lifecycle, SweepDue owns firing, and
// this module owns only the memo revision. It composes with the
// other single-field amendments: each touches only its own field,
// and rewording the memo never moves a leg, an amount, a fee, or a
// date.

// AmendTransferScheduleMemo revises a live transfer schedule's memo
// in place. Only active or paused schedules may be amended; unknown
// IDs fail with ErrScheduleNotFound and cancelled or completed
// schedules fail with ErrScheduleBadTransition. The new memo reuses
// the creation length gate (overlong memos fail with ErrMemoTooLong
// and leave the live memo untouched); an empty string is legal and
// clears the memo. Only Memo changes; the legs, amount, currency,
// fee, interval, next run time, end time, run sequence, and run
// history are untouched, and already-fired runs are never rewritten
// — the new memo governs the next successful firing. Amending to
// the current memo is a no-op. Amending is off-journal — it does not
// bump the ledger version — but it is audited as
// transfer_schedule_memo_amended.
func (l *Ledger) AmendTransferScheduleMemo(id string, memo string) (TransferSchedule, error) {
	return l.AmendTransferScheduleMemoCtx(context.Background(), id, memo)
}

// AmendTransferScheduleMemoCtx is AmendTransferScheduleMemo with
// per-call audit actor attribution (LG-73).
func (l *Ledger) AmendTransferScheduleMemoCtx(ctx context.Context, id string, memo string) (TransferSchedule, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	sched, ok := l.schedules[id]
	if !ok {
		return TransferSchedule{}, ErrScheduleNotFound
	}
	if sched.Status != ScheduleActive && sched.Status != SchedulePaused {
		return TransferSchedule{}, fmt.Errorf("%w: cannot amend a %s schedule's memo", ErrScheduleBadTransition, sched.Status)
	}
	// Validate before touching anything: an overlong memo never
	// moves the live memo (the creation gate, LG-41, reused).
	if err := checkMemoLength(memo); err != nil {
		return TransferSchedule{}, err
	}
	if sched.Memo == memo {
		return *sched, nil
	}
	previous := sched.Memo
	sched.Memo = memo
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "transfer_schedule_memo_amended",
		Actor:         "AmendTransferScheduleMemo",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{sched.From, sched.To},
		Details: map[string]any{
			"schedule_id":   id,
			"previous_memo": previous,
			"memo":          memo,
			"currency":      sched.Currency,
			"run_seq":       sched.RunSeq,
		},
	})
	return *sched, nil
}
