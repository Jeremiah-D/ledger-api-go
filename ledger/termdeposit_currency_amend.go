package ledger

import (
	"context"
	"fmt"
	"time"
)

// Term-deposit currency amendment (LG-372).
//
// OpenTermDeposit fixes a deposit's currency at open: the code
// normalizes via normalizeCurrency (termdeposit.go:259-263) and the
// currency is then nailed down — the lock is currency-keyed
// everywhere it matters: termLockedLocked sums active principals
// per (account, currency), the open gate judges coversCents per
// currency, and maturity release and interest both settle in the
// deposit's currency. AmendTermDepositAmount (LG-371) already
// covers the principal, and the rate (LG-247), tier ladder
// (LG-249), rollover policy (LG-250), business-day maturity
// (LG-251), and term days (LG-252) each have their own in-place
// revision, so the currency is the deposit's last field with no
// revision entry. A wrongly-set currency — the opening currency
// mistyped, an old currency retired — could today only be remedied
// by withdrawing the whole deposit early (a penalty, a terminal
// state) and opening a new one. This module is the orthogonal
// in-place currency path: an operator revises the lock's currency
// instead of withdrawing and re-opening. Amending in place matters
// because an early withdrawal plus a re-open is a new deposit —
// a fresh opening date, a fresh rate, a penalty, and a broken
// audit chain — while an amendment keeps the same deposit, the
// same opening date and rate, and one audit trail from open to
// maturity.
//
// Semantics, deliberately fail-closed throughout, along
// AmendHoldCurrency (LG-369) and AmendTermDepositAmount (LG-371):
//
//   - Only an active deposit may be amended. An unknown ID fails
//     with ErrTermDepositNotFound; a matured or early-withdrawn
//     deposit fails with ErrTermDepositNotActive. There is no
//     lazy-expiry reading here, unlike holds: a deposit whose
//     maturity date has passed but which nobody has matured yet
//     is still active — it locks funds and accrues interest
//     until MatureTermDeposit (or the sweep) releases it — so
//     "past maturity" is not a rejection on its own.
//   - The new currency reuses the creation gate
//     (normalizeCurrency): anything that is not a three-letter
//     uppercase ISO 4217 code fails with ErrInvalidCurrency.
//     Unlike creation, an empty code is rejected rather than
//     normalized to the default currency, along LG-369: an
//     amendment that names no currency is a malformed revision,
//     not a request for the default — silently defaulting a EUR
//     deposit to USD would be a destructive surprise. A failed
//     amendment leaves the original currency exactly as it was.
//   - Amending to the current currency (after normalization) is
//     a no-op: it returns the deposit unchanged and writes no
//     audit event, like AmendTermDepositAmount's same-value path.
//     The no-op is checked before the coverage gate: a deposit
//     whose own currency's coverage has drifted since opening
//     (a posting moved the balance after the open) must still
//     no-op cleanly — the gate judges a move, and a no-op moves
//     nothing.
//   - The full principal must fit the new currency's available
//     funds: balance in the new currency minus that currency's
//     other reservations — active holds, pending-review freezes,
//     other active term-deposit locks, and partial freezes —
//     using the same formula AmendHoldCurrency uses for the
//     remainder. The deposit's own lock sits in the old
//     currency, so it is naturally absent from the new
//     currency's termLockedLocked sum: the check judges the move
//     against the new currency alone, and the old currency's
//     lock is released the moment the field changes. A shortfall
//     fails with ErrInsufficientAvailableFunds and the original
//     currency is untouched.
//   - The amendment touches the currency and nothing else: the
//     principal, rate, tier ladder, tenor, maturity date,
//     opening date, business-day flag, rollover policy, and
//     status are unchanged. Interest already journaled stays
//     journaled in the old currency — the journal is sealed
//     history, like a hold's captured cents — and future daily
//     accruals book in the new currency automatically: the
//     accrual entry is built from the live record's currency,
//     so no accrual-path change is needed. The term lock is a
//     live computation (termLockedLocked sums active principals
//     on every read), so the lock follows the amended currency
//     automatically, with no per-currency counter to migrate.
//     A rollover copies the live currency into the child row
//     (termdeposit_rollover.go), so a child generated after the
//     amendment inherits the amended currency, like any new
//     term opened after the revision; the amendment itself never
//     walks the rollover chain.
//   - Like the other term-deposit transitions, the amendment is
//     structural: it bumps neither the ledger version nor the
//     audit chain, and no outbox event fires — the deposit was
//     already reported when opened. The amendment lands one
//     audit event (term_deposit_currency_amended, with the
//     deposit ID, the previous and new currencies, and the
//     principal) so the "who moved the deposit's currency, and
//     from what to what" trail is tamper-evident. Term deposits
//     are snapshot records, so the amended currency rides the
//     deposit row through snapshots exactly like the original —
//     no snapshot change is needed.
//
// This is not FX conversion: no rate is applied, no amount is
// recomputed, and no posted entry is restated. The revision only
// changes which currency the principal is locked in.
//
// Orthogonality: OpenTermDeposit owns the initial currency,
// AmendTermDepositAmount (LG-371) owns the principal (the amount
// locked, not the currency it is locked in),
// AmendTermDepositRate (LG-247) owns the rate,
// AmendTermDepositTiers (LG-249) owns the ladder,
// AmendTermDepositRollover (LG-250) owns the renewal policy,
// AmendTermDepositTermDays (LG-252) owns the tenor (and hence
// the maturity date), WithdrawTermDeposit owns the terminal early
// withdrawal, and this module owns only the currency revision. It
// is orthogonal to AmendHoldCurrency (LG-369), which revises a
// hold's currency, and to AmendPreAuthCurrency (LG-368), which
// revises a pre-authorization's currency.

// AmendTermDepositCurrency sets an active term deposit's currency
// to newCurrency in place. The full principal must fit the new
// currency's available funds; an empty or malformed code fails
// with ErrInvalidCurrency, and amending to the current currency
// is a no-op. A failed amendment leaves the deposit exactly as it
// was. Amending is off-journal — it does not bump the ledger
// version — but a real revision is audited as
// term_deposit_currency_amended.
func (l *Ledger) AmendTermDepositCurrency(id string, newCurrency string) (TermDeposit, error) {
	return l.AmendTermDepositCurrencyCtx(context.Background(), id, newCurrency)
}

// AmendTermDepositCurrencyCtx is AmendTermDepositCurrency with
// per-call audit actor attribution (LG-73).
func (l *Ledger) AmendTermDepositCurrencyCtx(ctx context.Context, id string, newCurrency string) (TermDeposit, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	td, ok := l.termDeposits[id]
	if !ok {
		return TermDeposit{}, ErrTermDepositNotFound
	}
	if td.Status != TermDepositActive {
		return TermDeposit{}, fmt.Errorf("%w: status %q", ErrTermDepositNotActive, td.Status)
	}
	if newCurrency == "" {
		return TermDeposit{}, ErrInvalidCurrency
	}
	normalized, err := normalizeCurrency(newCurrency)
	if err != nil {
		return TermDeposit{}, err
	}
	if normalized == td.Currency {
		return td, nil
	}
	// Coverage gates the move, along AmendHoldCurrency: the full
	// principal must fit the new currency's available funds. The
	// deposit's own lock sits in the old currency, so
	// termLockedLocked in the new currency already excludes it —
	// the move is judged against the new currency alone, never
	// double-counted against itself.
	now := time.Now()
	key := accountCurrency{account: td.Account, currency: normalized}
	availableInNew := l.balances[key] -
		l.heldLocked(td.Account, normalized, now) -
		l.reviewHeldLocked(td.Account, normalized, now) -
		l.termLockedLocked(td.Account, normalized, now) -
		l.partialFrozenLocked(td.Account, normalized)
	if td.AmountCents > availableInNew {
		return TermDeposit{}, ErrInsufficientAvailableFunds
	}
	previous := td.Currency
	td.Currency = normalized
	l.termDeposits[id] = td
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "term_deposit_currency_amended",
		Actor:         "AmendTermDepositCurrency",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{td.Account},
		Details: map[string]any{
			"term_deposit_id":   id,
			"previous_currency": previous,
			"currency":          normalized,
			"amount_cents":      td.AmountCents,
		},
	})
	return td, nil
}
