package ledger

import (
	"context"
	"fmt"
)

// Transfer-schedule FX-pair amendment (LG-261).
//
// CreateTransferSchedule fixes a plan's FX settlement pair at creation
// (LG-41); Pause/Resume/Cancel own the lifecycle transitions. This
// module is the orthogonal in-place FX-pair path, alongside the amount
// amendment (LG-253), the interval amendment (LG-254), the end-time
// amendment (LG-255), the fee amendment (LG-256), the fee-account
// amendment (LG-257), the skip-fee amendment (LG-258), the memo
// amendment (LG-259), and the business-day cadence amendment (LG-260):
// an operator revises a live plan's FX settlement pair instead of
// cancelling it and creating a new one. Amending in place matters for
// the same reason as those amendments: cancelling plus re-creating is
// a new plan — a fresh ID that breaks the plan's run history and its
// "<ID>/run/<seq>" idempotency lineage — while an amendment keeps the
// same plan, the same legs, and one audit trail from creation to
// completion.
//
// Semantics, deliberately fail-closed throughout:
//
//   - Only an active or paused plan may be amended. An unknown ID
//     fails with ErrScheduleNotFound; a cancelled or completed plan
//     fails with ErrScheduleBadTransition. A paused plan is
//     amendable because pausing is a hold, not a terminal state:
//     the pair revision simply waits with the plan and governs the
//     first run after resume, exactly as it would govern the next
//     run of an active plan.
//   - The pair is revised atomically, as one pair, never one field
//     at a time: (empty target currency, empty settlement account)
//     is legal and clears the pair back to same-currency settlement;
//     (non-empty target currency, any settlement account) sets the
//     pair, where an empty settlement account means "fall back to
//     the ledger-level FX account at fire time", exactly as
//     PostTransfer treats an empty Transfer.FXAccount; (empty
//     target currency, non-empty settlement account) is an orphan
//     settlement account — an account with no currency to settle
//     into — and fails with ErrScheduleBadFXPair, leaving the
//     original pair exactly as it was. There is no partial
//     application: every gate runs before either field moves.
//   - The new target currency reuses the creation gate
//     (normalizeCurrency, the same gate CreateTransferSchedule
//     applies): a malformed code fails with ErrInvalidCurrency and
//     the live pair is untouched. A normalized target currency equal
//     to the schedule's own Currency fails with
//     ErrCrossCurrencyTransfer and the live pair is untouched —
//     the pair must name a genuinely foreign settlement currency.
//     An explicit settlement account equal to From or To fails
//     with ErrInvalidFXAccount, PostTransfer's own rule, and the
//     live pair is untouched: the clearing account must differ
//     from both legs. An empty settlement account is exempt from
//     that rule — it names no account, so there is nothing to
//     collide with, and the ledger-level account governs at fire
//     time under PostTransfer's own checks.
//   - The amendment touches ToCurrency and FXAccount and nothing
//     else: the legs, amount, currency, fee amount, fee account,
//     skip-fee switch, memo, interval, next run time, end time,
//     run sequence, and run history are unchanged. There is no
//     recomputation to do — unlike the interval amendment (LG-254)
//     there is no derived date to move — so the revision is a
//     paired-field write.
//   - fireSchedule builds each run's transfer from the schedule
//     snapshot SweepDue takes at fire time (see SweepDue), so the
//     new pair governs the next successful firing with no
//     firing-path change: already-fired runs are journaled
//     transfers whose FX legs are never rewritten, and a failed
//     run's retry reads the amended pair — the failed run posted
//     nothing, so the retry books exactly once, at the amended
//     pair, under the same "<ID>/run/<seq>" idempotency key.
//   - Amending to the current pair (compared after normalizing the
//     new target currency) is a no-op: the row is untouched and no
//     audit event fires, so a retried or redundant revision does
//     not pollute the audit trail with a change that changed
//     nothing. The no-op check runs before the pair gates, so
//     restating the current pair never errors — the same
//     convention as the fee-account amendment (LG-257).
//   - Like the other schedule transitions, the amendment is
//     structural: it bumps neither the ledger version nor the audit
//     chain, and no outbox event fires — the plan was already
//     reported when created. The amendment lands one audit event
//     (transfer_schedule_fx_pair_amended, with the schedule ID,
//     the previous and new target currencies and settlement
//     accounts, the schedule currency, and the run sequence) so
//     the "who moved the settlement pair, and from what to what"
//     trail is tamper-evident. Schedules are snapshot records, so
//     the amended pair rides the schedule row through snapshots
//     exactly like the original — the omitempty pair fields carry
//     a set pair, and a pair cleared back to empty round-trips as
//     empty — no snapshot change is needed.
//
// Orthogonality: CreateTransferSchedule owns the initial pair,
// Pause/Resume/Cancel own the lifecycle, SweepDue owns firing, the
// FX mechanism itself (rates, conversion, settlement legs) belongs
// to LG-27, and this module owns only the pair revision. It
// composes with the other single-field amendments: each touches
// only its own fields, and moving the settlement pair never moves
// a leg, an amount, a fee, or a date.

// AmendTransferScheduleFxPair revises a live transfer schedule's FX
// settlement pair in place. Only active or paused schedules may be
// amended; unknown IDs fail with ErrScheduleNotFound and cancelled
// or completed schedules fail with ErrScheduleBadTransition. The
// pair is revised atomically: (empty, empty) clears back to
// same-currency settlement, (non-empty target, any account) sets
// the pair (an empty account falls back to the ledger-level FX
// account at fire time), and (empty target, non-empty account) is
// an orphan settlement account that fails with
// ErrScheduleBadFXPair. The new target currency reuses the creation
// gate (malformed codes fail with ErrInvalidCurrency; a target
// equal to the schedule's own currency fails with
// ErrCrossCurrencyTransfer), and an explicit settlement account
// equal to From or To fails with ErrInvalidFXAccount; every
// rejection leaves the original pair untouched. Only ToCurrency
// and FXAccount change; the legs, amount, currency, fees, memo,
// interval, next run time, end time, run sequence, and run history
// are untouched, and already-fired runs are never rewritten — the
// new pair governs the next successful firing. Amending to the
// current pair is a no-op. Amending is off-journal — it does not
// bump the ledger version — but it is audited as
// transfer_schedule_fx_pair_amended.
func (l *Ledger) AmendTransferScheduleFxPair(id string, toCurrency string, fxAccount AccountID) (TransferSchedule, error) {
	return l.AmendTransferScheduleFxPairCtx(context.Background(), id, toCurrency, fxAccount)
}

// AmendTransferScheduleFxPairCtx is AmendTransferScheduleFxPair with
// per-call audit actor attribution (LG-73).
func (l *Ledger) AmendTransferScheduleFxPairCtx(ctx context.Context, id string, toCurrency string, fxAccount AccountID) (TransferSchedule, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	sched, ok := l.schedules[id]
	if !ok {
		return TransferSchedule{}, ErrScheduleNotFound
	}
	if sched.Status != ScheduleActive && sched.Status != SchedulePaused {
		return TransferSchedule{}, fmt.Errorf("%w: cannot amend a %s schedule's FX pair", ErrScheduleBadTransition, sched.Status)
	}
	// Normalize the new target currency for comparison (the creation
	// gate, LG-41, reused): a malformed code never moves the live
	// pair. Empty stays empty — it means "clear", not the default
	// currency, so a cleared pair is genuinely same-currency.
	normalized := ""
	if toCurrency != "" {
		to, err := normalizeCurrency(toCurrency)
		if err != nil {
			return TransferSchedule{}, err
		}
		normalized = to
	}
	// No-op first, so restating the current pair never errors.
	if sched.ToCurrency == normalized && sched.FXAccount == fxAccount {
		return *sched, nil
	}
	// Validate the whole pair before touching either field: an
	// orphan account, a same-currency target, or a leg-colliding
	// account never partially applies.
	if normalized == "" && fxAccount != "" {
		return TransferSchedule{}, ErrScheduleBadFXPair
	}
	if normalized != "" && normalized == sched.Currency {
		return TransferSchedule{}, ErrCrossCurrencyTransfer
	}
	if fxAccount != "" && (fxAccount == sched.From || fxAccount == sched.To) {
		return TransferSchedule{}, ErrInvalidFXAccount
	}
	previousToCurrency := sched.ToCurrency
	previousFXAccount := sched.FXAccount
	sched.ToCurrency = normalized
	sched.FXAccount = fxAccount
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "transfer_schedule_fx_pair_amended",
		Actor:         "AmendTransferScheduleFxPair",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{sched.From, sched.To},
		Details: map[string]any{
			"schedule_id":          id,
			"previous_to_currency": previousToCurrency,
			"to_currency":          normalized,
			"previous_fx_account":  string(previousFXAccount),
			"fx_account":           string(fxAccount),
			"currency":             sched.Currency,
			"run_seq":              sched.RunSeq,
		},
	})
	return *sched, nil
}
