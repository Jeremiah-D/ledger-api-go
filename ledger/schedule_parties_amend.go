package ledger

import (
	"context"
	"fmt"
)

// Transfer-schedule payer/payee amendment (LG-364).
//
// CreateTransferSchedule fixes a plan's payer and payee at creation
// (LG-41); Pause/Resume/Cancel own the lifecycle transitions. The
// eleven single-field amendments LG-253..263 already cover the
// amount, interval, end time, fee, fee account, skip-fee, memo,
// business-day cadence, FX pair, next run time, and currency — the
// legs are the last mutable field with no revision entry. This
// module is that parties path: an operator revises a live plan's
// payer or payee in place — a salary account that changed, a
// supplier whose settlement account moved — instead of cancelling
// the plan and creating a new one. Amending in place matters for
// the same reason as the sibling amendments: cancelling plus
// re-creating is a new plan — a fresh ID that breaks the plan's run
// history and its "<ID>/run/<seq>" idempotency lineage — while an
// amendment keeps the same plan, the same legs, and one audit trail
// from creation to completion.
//
// Semantics, deliberately fail-closed throughout:
//
//   - Only an active or paused plan with zero fired runs may be
//     amended. An unknown ID fails with ErrScheduleNotFound; a
//     cancelled or completed plan fails with ErrScheduleBadTransition.
//     A plan that has already fired a run fails with
//     ErrScheduleBadTransition as well (the wrap message names the
//     run sequence): with RunSeq == 0 no run has been journaled, so
//     changing the legs restates nothing — no journaled row
//     references the old accounts. Once a run has fired, its
//     journaled transfer fixes the debit/credit pair that run
//     collected, and moving the payer or payee would split the run
//     history mid-flight; that is out of scope for this amendment.
//     A paused plan with zero runs is amendable — pausing is a hold,
//     not a terminal state — and the new legs govern the first run
//     after resume.
//   - The new party reuses the creation gate (see
//     CreateTransferSchedule): an empty party fails with
//     ErrScheduleEmptyAccount, and a party equal to the live other
//     leg fails with ErrScheduleSameAccount. The same-account check
//     reads the live other leg, so amending the payer to the current
//     payee — or the payee to the current payer — is refused exactly
//     as creation would refuse it. A failed amendment leaves the
//     original legs exactly as they were.
//   - Each method touches exactly one field and nothing else: the
//     payer amendment moves From only, the payee amendment moves To
//     only; amount, currency, fee fields, interval, next run time,
//     end time, run sequence, and status are unchanged.
//     fireSchedule builds each run's transfer from the schedule
//     snapshot SweepDue takes at fire time (see SweepDue), so the
//     new legs govern the next successful firing with no
//     firing-path change: already-fired runs are journaled transfers
//     and are never rewritten, and a failed run's retry reads the
//     amended legs — the failed run posted nothing, so the retry
//     books exactly once, against the new accounts, under the same
//     "<ID>/run/<seq>" idempotency key.
//   - Amending to the current party is a no-op: the row is
//     untouched and no audit event fires, so a retried or redundant
//     revision does not pollute the audit trail with a change that
//     changed nothing.
//   - Like the other schedule transitions, the amendment is
//     structural: it bumps neither the ledger version nor the audit
//     chain, and no outbox event fires — the plan was already
//     reported when created. The amendment lands one audit event
//     (transfer_schedule_payer_amended or
//     transfer_schedule_payee_amended, with the schedule ID, the
//     previous and new party, the currency, and the run sequence,
//     always zero) so the "who moved the payer/payee, and from what
//     to what" trail is tamper-evident. Schedules are snapshot
//     records, so the amended legs ride the schedule row through
//     snapshots exactly like the original — no snapshot change is
//     needed.
//
// Orthogonality: CreateTransferSchedule owns the initial legs,
// Pause/Resume/Cancel own the lifecycle, SweepDue owns firing, the
// eleven LG-253..263 amendments own the other single fields, and
// this module owns only the payer/payee revision. The two methods
// are independent of each other — each moves one leg and leaves the
// other alone — so they compose (any order) because the sweep
// always reads the live schedule row.

// amendTransferScheduleParty is the shared gate for the payer and
// payee amendments: not-found, status, and run-sequence checks in
// the sibling-amendment order, then the creation gate for the new
// party against the live other leg. It returns the schedule and the
// validated new party, or an error with the schedule untouched. The
// caller performs the single-field mutation and its audit event.
func (l *Ledger) amendTransferScheduleParty(id string, newParty AccountID, isPayer bool) (*TransferSchedule, error) {
	sched, ok := l.schedules[id]
	if !ok {
		return nil, ErrScheduleNotFound
	}
	what := "payee"
	if isPayer {
		what = "payer"
	}
	if sched.Status != ScheduleActive && sched.Status != SchedulePaused {
		return nil, fmt.Errorf("%w: cannot amend a %s schedule's %s", ErrScheduleBadTransition, sched.Status, what)
	}
	if sched.RunSeq != 0 {
		return nil, fmt.Errorf("%w: cannot amend %s of a schedule with run_seq %d", ErrScheduleBadTransition, what, sched.RunSeq)
	}
	if newParty == "" {
		return nil, ErrScheduleEmptyAccount
	}
	other := sched.From
	if isPayer {
		other = sched.To
	}
	if newParty == other {
		return nil, ErrScheduleSameAccount
	}
	return sched, nil
}

// AmendTransferSchedulePayer revises a live transfer schedule's
// payer in place. Only active or paused schedules with zero fired
// runs may be amended; unknown IDs fail with ErrScheduleNotFound,
// cancelled or completed schedules and schedules with fired runs
// fail with ErrScheduleBadTransition, and an empty payer or a payer
// equal to the live payee fails with ErrScheduleEmptyAccount or
// ErrScheduleSameAccount. Only From changes; the payee, amount,
// currency, fee fields, interval, next run time, end time, run
// sequence, and status are untouched, and no run has fired yet so
// nothing is restated — the new payer governs every run from the
// next sweep. Amending to the current payer is a no-op. Amending is
// off-journal — it does not bump the ledger version — but it is
// audited as transfer_schedule_payer_amended.
func (l *Ledger) AmendTransferSchedulePayer(id string, newPayer AccountID) (TransferSchedule, error) {
	return l.AmendTransferSchedulePayerCtx(context.Background(), id, newPayer)
}

// AmendTransferSchedulePayerCtx is AmendTransferSchedulePayer with
// per-call audit actor attribution (LG-73).
func (l *Ledger) AmendTransferSchedulePayerCtx(ctx context.Context, id string, newPayer AccountID) (TransferSchedule, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	sched, err := l.amendTransferScheduleParty(id, newPayer, true)
	if err != nil {
		return TransferSchedule{}, err
	}
	if sched.From == newPayer {
		return *sched, nil
	}
	previous := sched.From
	sched.From = newPayer
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "transfer_schedule_payer_amended",
		Actor:         "AmendTransferSchedulePayer",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{sched.From, sched.To},
		Details: map[string]any{
			"schedule_id":    id,
			"previous_payer": previous,
			"payer":          newPayer,
			"currency":       sched.Currency,
			"run_seq":        sched.RunSeq,
		},
	})
	return *sched, nil
}

// AmendTransferSchedulePayee revises a live transfer schedule's
// payee in place. Only active or paused schedules with zero fired
// runs may be amended; unknown IDs fail with ErrScheduleNotFound,
// cancelled or completed schedules and schedules with fired runs
// fail with ErrScheduleBadTransition, and an empty payee or a payee
// equal to the live payer fails with ErrScheduleEmptyAccount or
// ErrScheduleSameAccount. Only To changes; the payer, amount,
// currency, fee fields, interval, next run time, end time, run
// sequence, and status are untouched, and no run has fired yet so
// nothing is restated — the new payee governs every run from the
// next sweep. Amending to the current payee is a no-op. Amending is
// off-journal — it does not bump the ledger version — but it is
// audited as transfer_schedule_payee_amended.
func (l *Ledger) AmendTransferSchedulePayee(id string, newPayee AccountID) (TransferSchedule, error) {
	return l.AmendTransferSchedulePayeeCtx(context.Background(), id, newPayee)
}

// AmendTransferSchedulePayeeCtx is AmendTransferSchedulePayee with
// per-call audit actor attribution (LG-73).
func (l *Ledger) AmendTransferSchedulePayeeCtx(ctx context.Context, id string, newPayee AccountID) (TransferSchedule, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	sched, err := l.amendTransferScheduleParty(id, newPayee, false)
	if err != nil {
		return TransferSchedule{}, err
	}
	if sched.To == newPayee {
		return *sched, nil
	}
	previous := sched.To
	sched.To = newPayee
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "transfer_schedule_payee_amended",
		Actor:         "AmendTransferSchedulePayee",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{sched.From, sched.To},
		Details: map[string]any{
			"schedule_id":    id,
			"previous_payee": previous,
			"payee":          newPayee,
			"currency":       sched.Currency,
			"run_seq":        sched.RunSeq,
		},
	})
	return *sched, nil
}
