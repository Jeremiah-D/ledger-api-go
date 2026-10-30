package ledger

import (
	"context"
	"fmt"
)

// Transfer-schedule currency amendment (LG-263).
//
// CreateTransferSchedule fixes a plan's base currency at creation
// (LG-41); Pause/Resume/Cancel own the lifecycle transitions. This
// module is the orthogonal in-place base-currency path, alongside
// the amount amendment (LG-253), the interval amendment (LG-254),
// the end-time amendment (LG-255), the fee amendment (LG-256), the
// fee-account amendment (LG-257), the skip-fee amendment (LG-258),
// the memo amendment (LG-259), the business-day cadence amendment
// (LG-260), the FX-pair amendment (LG-261), and the next-run-time
// amendment (LG-262): an operator revises a live plan's booking
// currency instead of cancelling it and creating a new one.
// Amending in place matters for the same reason as those
// amendments: cancelling plus re-creating is a new plan — a fresh
// ID that breaks the plan's run history and its
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
//     the currency revision simply waits with the plan and governs
//     the first run after resume, exactly as it would govern the
//     next run of an active plan.
//   - The new currency reuses the creation gate (normalizeCurrency,
//     the same gate CreateTransferSchedule applies): an empty code
//     normalizes to the default currency — exactly as at creation,
//     so amending with "" moves a EUR plan to USD and is a no-op
//     on a USD plan — and a malformed code fails with
//     ErrInvalidCurrency, leaving the live currency untouched.
//   - The live FX counterpart gate, the symmetric mirror of the
//     FX-pair amendment (LG-261), which judges a new target
//     currency against the live base currency, and of the end-time
//     amendment (LG-255), which judges a new end time against the
//     live next run time: when the plan carries a settlement pair
//     (ToCurrency set), a new base currency equal to that live
//     target fails with ErrCrossCurrencyTransfer and the live
//     currency is untouched. A base currency equal to its own
//     settlement target would make the next run a same-currency
//     transfer carrying an FX pair — unpostable under
//     PostTransfer's own rule — so the gate is judged against the
//     ToCurrency at amendment time, not the creation-time one:
//     the pair may itself have been amended since creation
//     (LG-261), and the amendment must be judged against the pair
//     that will actually settle the next run. A plan with no pair
//     has no counterpart to collide with and amends freely.
//   - The amendment touches Currency and nothing else: the legs,
//     amount, FX pair, fee amount, fee account, skip-fee switch,
//     memo, interval, next run time, end time, run sequence, and
//     run history are unchanged. The per-run fee amount is a
//     bare number of cents, so it rides along unchanged — it is
//     simply charged in the new currency from the next run, the
//     same way the per-run principal is now booked in the new
//     currency. There is no recomputation to do — unlike the
//     interval amendment (LG-254) there is no derived date to
//     move — so the revision is a single-field write.
//   - fireSchedule builds each run's transfer from the schedule
//     snapshot SweepDue takes at fire time (see SweepDue), so the
//     new currency governs the next successful firing with no
//     firing-path change: already-fired runs are journaled
//     transfers whose currency is never rewritten, and a failed
//     run's retry reads the amended currency — the failed run
//     posted nothing, so the retry books exactly once, in the
//     amended currency, under the same "<ID>/run/<seq>"
//     idempotency key.
//   - Amending to the current currency (compared after normalizing
//     the new code) is a no-op: the row is untouched and no audit
//     event fires, so a retried or redundant revision does not
//     pollute the audit trail with a change that changed nothing.
//     The no-op check runs before the counterpart gate, so
//     restating the current currency never errors — the same
//     convention as the FX-pair amendment (LG-261).
//   - Like the other schedule transitions, the amendment is
//     structural: it bumps neither the ledger version nor the audit
//     chain, and no outbox event fires — the plan was already
//     reported when created. The amendment lands one audit event
//     (transfer_schedule_currency_amended, with the schedule ID,
//     the previous and new currencies, the live settlement target
//     for context, and the run sequence) so the "who moved the
//     booking currency, and from what to what" trail is
//     tamper-evident. Schedules are snapshot records, so the
//     amended currency rides the schedule row through snapshots
//     exactly like the original — the currency field carries it
//     with no omitempty gap — no snapshot change is needed.
//
// Orthogonality: CreateTransferSchedule owns the initial currency,
// Pause/Resume/Cancel own the lifecycle, SweepDue owns firing, the
// multi-currency mechanism itself (per-currency balances, the
// accounting equation per currency, cross-currency rejection)
// belongs to LG-18, and this module owns only the plan-currency
// revision. It composes with the other single-field amendments:
// each touches only its own field, and moving the booking
// currency never moves a leg, an amount, a fee, a pair, or a date.
// It is orthogonal to the FX-pair amendment (LG-261), which moves
// the settlement target, this moves the base currency the target
// is judged against — the two gates are symmetric mirrors of each
// other.

// AmendTransferScheduleCurrency revises a live transfer schedule's
// base currency in place. Only active or paused schedules may be
// amended; unknown IDs fail with ErrScheduleNotFound and cancelled
// or completed schedules fail with ErrScheduleBadTransition. The
// new currency reuses the creation gate (normalizeCurrency): an
// empty code normalizes to the default currency, and a malformed
// code fails with ErrInvalidCurrency; a new currency equal to the
// plan's live FX settlement target (when one is set) fails with
// ErrCrossCurrencyTransfer; every rejection leaves the original
// currency untouched. Only Currency changes; the legs, amount, FX
// pair, fees, memo, interval, next run time, end time, run
// sequence, and run history are untouched, and already-fired runs
// are never rewritten — the new currency governs the next
// successful firing. Amending to the current currency is a no-op.
// Amending is off-journal — it does not bump the ledger version —
// but it is audited as transfer_schedule_currency_amended.
func (l *Ledger) AmendTransferScheduleCurrency(id string, currency string) (TransferSchedule, error) {
	return l.AmendTransferScheduleCurrencyCtx(context.Background(), id, currency)
}

// AmendTransferScheduleCurrencyCtx is AmendTransferScheduleCurrency
// with per-call audit actor attribution (LG-73).
func (l *Ledger) AmendTransferScheduleCurrencyCtx(ctx context.Context, id string, currency string) (TransferSchedule, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	sched, ok := l.schedules[id]
	if !ok {
		return TransferSchedule{}, ErrScheduleNotFound
	}
	if sched.Status != ScheduleActive && sched.Status != SchedulePaused {
		return TransferSchedule{}, fmt.Errorf("%w: cannot amend a %s schedule's currency", ErrScheduleBadTransition, sched.Status)
	}
	// Normalize through the creation gate (LG-41, reused): an empty
	// code means the default currency, exactly as at creation, and
	// a malformed code never moves the live currency.
	normalized, err := normalizeCurrency(currency)
	if err != nil {
		return TransferSchedule{}, err
	}
	// No-op first, so restating the current currency never errors —
	// the same convention as the FX-pair amendment (LG-261).
	if sched.Currency == normalized {
		return *sched, nil
	}
	// The live counterpart gate: a new base currency equal to a set
	// settlement target would make the next run a same-currency
	// transfer carrying an FX pair, unpostable under PostTransfer's
	// own rule. Judged against the live ToCurrency, which may itself
	// have been amended since creation.
	if sched.ToCurrency != "" && normalized == sched.ToCurrency {
		return TransferSchedule{}, ErrCrossCurrencyTransfer
	}
	previous := sched.Currency
	sched.Currency = normalized
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "transfer_schedule_currency_amended",
		Actor:         "AmendTransferScheduleCurrency",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{sched.From, sched.To},
		Details: map[string]any{
			"schedule_id":       id,
			"previous_currency": previous,
			"currency":          normalized,
			"to_currency":       sched.ToCurrency,
			"run_seq":           sched.RunSeq,
		},
	})
	return *sched, nil
}
