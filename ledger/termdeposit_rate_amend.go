package ledger

import (
	"context"
	"fmt"
)

// Term-deposit fixed-rate amendment (LG-247).
//
// OpenTermDeposit fixes a deposit's flat rate at open; MatureTermDeposit
// and WithdrawTermDeposit own the terminal transitions. This module is
// the orthogonal in-place rate path: an operator reprices a live flat-rate
// deposit instead of withdrawing it early and opening a new one. Amending
// in place matters because an early withdrawal plus a re-open is a new
// deposit — a fresh ID and a fresh tenor that breaks the deposit's
// accrual history — while an amendment keeps the same deposit, the same
// maturity date, and one audit trail from open to maturity.
//
// Semantics, deliberately fail-closed throughout:
//
//   - Only an active deposit may be amended. An unknown ID fails with
//     ErrTermDepositNotFound; a matured or early-withdrawn deposit
//     fails with ErrTermDepositNotActive. There is no lazy-expiry
//     reading here, unlike mandates: a deposit whose maturity date has
//     passed but which nobody has matured yet is still active — it
//     locks funds and accrues interest until MatureTermDeposit (or the
//     sweep) releases it — so "past maturity" is not a rejection on
//     its own.
//   - The new rate must lie in 1..10000 basis points, the same gate
//     OpenTermDeposit applies to the original rate, so an amendment
//     can never install a rate the deposit could not have been opened
//     with. A failed amendment leaves the original rate exactly as it
//     was.
//   - Only flat-rate deposits may be amended. A tiered deposit
//     (RateTiers non-empty, LG-145) fails with
//     ErrTermDepositRateTiersConflict: the flat rate and the ladder are
//     mutually exclusive, and repricing a ladder is a different
//     operation (a ladder revision, a later item), not a mode switch
//     smuggled in through a flat-rate amendment. Amending a tiered
//     deposit must never flatten it, and amending a flat deposit must
//     never install a ladder.
//   - Amending to the current rate is a no-op: the row is untouched and
//     no audit event fires, so a retried or redundant reprice does not
//     pollute the audit trail with a change that changed nothing.
//   - The amendment touches the rate and nothing else: the principal,
//     currency, tenor, maturity date, and rollover policy are
//     unchanged, and interest already journaled stays journaled.
//     AccrueTermInterest reads the live deposit row on every accrual
//     (see AccrueTermInterest), so the new rate takes effect from the
//     very next accrual day with no accrual-path change — and
//     re-running an old day replays the original row at the old rate
//     instead of repricing it. Term deposits do not compound (a day's
//     interest prices the principal only, never the interest already
//     booked), so there is no compounding interaction for a repriced
//     deposit to disturb.
//   - Rollover inheritance is generation-time, never retroactive: the
//     sweep copies the parent's live RateBps into a child row when it
//     rolls (see rolloverTermDepositLocked), so a child generated
//     before an amendment keeps the rate it was born with — the
//     amendment writes only the parent's row — while a child
//     generated after the amendment inherits the amended rate, like
//     any new term opened after a reprice. The amendment itself never
//     walks the rollover chain.
//   - Like the other term-deposit transitions, the amendment is
//     structural: it bumps neither the ledger version nor the audit
//     chain, and no outbox event fires — the deposit was already
//     reported when opened. The amendment lands one audit event
//     (term_deposit_rate_amended, with the deposit ID, the previous
//     and new rates, the principal, and the currency) so the "who
//     repriced the deposit, and from what to what" trail is
//     tamper-evident. Term deposits are snapshot records, so the
//     amended rate rides the deposit row through snapshots exactly
//     like the original — no snapshot change is needed.
//
// Orthogonality: OpenTermDeposit owns the initial rate,
// MatureTermDeposit and WithdrawTermDeposit own the terminal states,
// AccrueTermInterest owns the interest rows, the LG-145 tiered ladder
// owns tiered pricing, the LG-139 rollover policy owns renewals, and
// this module owns only the flat-rate revision.

// AmendTermDepositRate sets an active flat-rate term deposit's rate to
// newRateBps in place. The new rate must lie in 1..10000 basis points;
// a failed amendment leaves the deposit exactly as it was. Amending to
// the current rate is a no-op. Amending is off-journal — it does not
// bump the ledger version — but it is audited as
// term_deposit_rate_amended.
func (l *Ledger) AmendTermDepositRate(id string, newRateBps int64) (TermDeposit, error) {
	return l.AmendTermDepositRateCtx(context.Background(), id, newRateBps)
}

// AmendTermDepositRateCtx is AmendTermDepositRate with per-call audit
// actor attribution (LG-73).
func (l *Ledger) AmendTermDepositRateCtx(ctx context.Context, id string, newRateBps int64) (TermDeposit, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	td, ok := l.termDeposits[id]
	if !ok {
		return TermDeposit{}, ErrTermDepositNotFound
	}
	if td.Status != TermDepositActive {
		return TermDeposit{}, fmt.Errorf("%w: status %q", ErrTermDepositNotActive, td.Status)
	}
	if newRateBps < 1 || newRateBps > 10000 {
		return TermDeposit{}, fmt.Errorf("%w: %d", ErrInvalidTermDepositRate, newRateBps)
	}
	if len(td.RateTiers) > 0 {
		return TermDeposit{}, ErrTermDepositRateTiersConflict
	}
	previous := td.RateBps
	if newRateBps == previous {
		return td, nil
	}
	td.RateBps = newRateBps
	l.termDeposits[id] = td
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "term_deposit_rate_amended",
		Actor:         "AmendTermDepositRate",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{td.Account},
		Details: map[string]any{
			"term_deposit_id":   id,
			"previous_rate_bps": previous,
			"rate_bps":          newRateBps,
			"amount_cents":      td.AmountCents,
			"currency":          td.Currency,
		},
	})
	return td, nil
}
