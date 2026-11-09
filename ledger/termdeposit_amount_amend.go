package ledger

import (
	"context"
	"fmt"
	"time"
)

// Term-deposit principal amendment (LG-371).
//
// OpenTermDeposit fixes a deposit's principal at open; the rate
// (LG-247), the tier ladder (LG-249), the rollover policy (LG-250),
// the business-day maturity (LG-251), and the term days (LG-252)
// each have their own in-place revision — the principal is the one
// core field with no revision path at all. This module is the
// orthogonal in-place principal path: an operator tops a live
// deposit up, or draws part of it down, instead of withdrawing the
// whole deposit early and opening a new one. Amending in place
// matters because an early withdrawal plus a re-open is a new
// deposit — a fresh opening date, a fresh rate, a penalty, and a
// broken audit chain — while an amendment keeps the same deposit,
// the same opening date and rate, and one audit trail from open to
// maturity.
//
// Semantics, deliberately fail-closed throughout, along
// AmendTermDepositRate (LG-247) and AmendHoldAmount (LG-240):
//
//   - Only an active deposit may be amended. An unknown ID fails
//     with ErrTermDepositNotFound; a matured or early-withdrawn
//     deposit fails with ErrTermDepositNotActive. There is no
//     lazy-expiry reading here, unlike mandates: a deposit whose
//     maturity date has passed but which nobody has matured yet
//     is still active — it locks funds and accrues interest
//     until MatureTermDeposit (or the sweep) releases it — so
//     "past maturity" is not a rejection on its own.
//   - The new principal must be positive: zero or negative fails
//     with ErrTermDepositNonPositiveAmount, the same gate
//     OpenTermDeposit applies at open. A top-up is gated on the
//     funds available excluding this deposit — balance minus
//     holds, minus pending-review reservations, minus other
//     active term-deposit locks, minus partial freezes — along
//     the AmendHoldAmount increment gate (hold_amount_amend.go):
//     the increase is never double-counted against the deposit's
//     own current lock, so a deposit may always be topped up to
//     exactly the funds its account can cover. A top-up that
//     does not fit fails with ErrInsufficientAvailableFunds and
//     the live principal is untouched. A draw-down carries no
//     funds gate — releasing liquidity cannot fail — but it must
//     still be positive: a draw-down is not an early withdrawal,
//     so it books no penalty and never closes the deposit.
//   - Amending to the current principal is a no-op: the row is
//     untouched and no audit event fires, so a retried or
//     redundant revision does not pollute the audit trail with a
//     change that changed nothing.
//   - The amendment touches the principal and nothing else: the
//     currency, rate, tier ladder, tenor, maturity date, opening
//     date, business-day flag, rollover policy, and status are
//     unchanged — the maturity date does not move with the
//     principal (only a term-days revision recomputes it) — and
//     interest already journaled stays journaled. The daily
//     interest function prices the live principal
//     (termDepositDailyInterestCents), so the new principal
//     takes effect from the very next accrual day with no
//     accrual-path change, and re-running an old day replays
//     the original row at the old principal instead of
//     repricing it. The term lock is a live computation
//     (termLockedLocked sums active principals on every read),
//     so the lock follows the amended principal automatically:
//     a top-up locks more, a draw-down releases funds, and the
//     next AvailableIn read already reflects the change. A
//     rollover copies the live principal into the child row
//     (termdeposit_rollover.go), so a child generated after the
//     amendment inherits the amended principal, like any new
//     term opened after a top-up; the amendment itself never
//     walks the rollover chain.
//   - Like the other term-deposit transitions, the amendment is
//     structural: it bumps neither the ledger version nor the
//     audit chain, and no outbox event fires — the deposit was
//     already reported when opened. The amendment lands one
//     audit event (term_deposit_amount_amended, with the deposit
//     ID, the previous and new principals, and the currency) so
//     the "who moved the principal, and from what to what" trail
//     is tamper-evident. Term deposits are snapshot records, so
//     the amended principal rides the deposit row through
//     snapshots exactly like the original — no snapshot change
//     is needed.
//
// Orthogonality: OpenTermDeposit owns the initial principal,
// AmendTermDepositRate (LG-247) owns the rate (the price of the
// principal, not the principal itself), AmendTermDepositTiers
// (LG-249) owns the ladder, AmendTermDepositRollover (LG-250)
// owns the renewal policy, AmendTermDepositTermDays (LG-252)
// owns the tenor (and hence the maturity date),
// WithdrawTermDeposit owns the terminal early withdrawal (with
// penalty and closure — this amendment is the in-flight
// principal adjustment without either), and this module owns
// only the principal revision.

// AmendTermDepositAmount sets an active term deposit's principal to
// newAmountCents in place. A top-up must fit the funds available
// excluding this deposit's own lock; a draw-down carries no funds
// gate but must stay positive. A failed amendment leaves the deposit
// exactly as it was, and amending to the current principal is a
// no-op. Amending is off-journal — it does not bump the ledger
// version — but a real revision is audited as
// term_deposit_amount_amended.
func (l *Ledger) AmendTermDepositAmount(id string, newAmountCents int64) (TermDeposit, error) {
	return l.AmendTermDepositAmountCtx(context.Background(), id, newAmountCents)
}

// AmendTermDepositAmountCtx is AmendTermDepositAmount with per-call
// audit actor attribution (LG-73).
func (l *Ledger) AmendTermDepositAmountCtx(ctx context.Context, id string, newAmountCents int64) (TermDeposit, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	td, ok := l.termDeposits[id]
	if !ok {
		return TermDeposit{}, ErrTermDepositNotFound
	}
	if td.Status != TermDepositActive {
		return TermDeposit{}, fmt.Errorf("%w: status %q", ErrTermDepositNotActive, td.Status)
	}
	if newAmountCents <= 0 {
		return TermDeposit{}, ErrTermDepositNonPositiveAmount
	}
	if newAmountCents > td.AmountCents {
		// Coverage gates increases only, along AmendHoldAmount:
		// the new principal must fit the funds available with
		// this deposit's own lock subtracted out first, so the
		// increase is never double-counted against itself.
		now := time.Now()
		key := accountCurrency{account: td.Account, currency: td.Currency}
		availableExcludingSelf := l.balances[key] -
			l.heldLocked(td.Account, td.Currency, now) -
			l.reviewHeldLocked(td.Account, td.Currency, now) -
			(l.termLockedLocked(td.Account, td.Currency, now) - td.AmountCents) -
			l.partialFrozenLocked(td.Account, td.Currency)
		if newAmountCents > availableExcludingSelf {
			return TermDeposit{}, ErrInsufficientAvailableFunds
		}
	}
	previous := td.AmountCents
	if newAmountCents == previous {
		return td, nil
	}
	td.AmountCents = newAmountCents
	l.termDeposits[id] = td
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "term_deposit_amount_amended",
		Actor:         "AmendTermDepositAmount",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{td.Account},
		Details: map[string]any{
			"term_deposit_id":       id,
			"previous_amount_cents": previous,
			"amount_cents":          newAmountCents,
			"currency":              td.Currency,
		},
	})
	return td, nil
}
