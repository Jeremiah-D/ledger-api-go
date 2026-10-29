package ledger

import (
	"context"
	"fmt"
	"time"
)

// Term-deposit term-days amendment (LG-252).
//
// OpenTermDeposit fixes a deposit's tenor at open (LG-90);
// MatureTermDeposit and WithdrawTermDeposit own the terminal
// transitions. This module is the orthogonal in-place tenor path:
// an operator revises a live deposit's term instead of withdrawing
// it early and opening a new one. Amending in place matters for the
// same reason as the flat-rate (LG-247), ladder (LG-249),
// rollover-policy (LG-250), and business-day-maturity (LG-251)
// amendments: an early withdrawal plus a re-open is a new deposit —
// a fresh ID and a fresh tenor that breaks the deposit's accrual
// history — while an amendment keeps the same deposit, the same
// principal, and one audit trail from open to maturity.
//
// Semantics, deliberately fail-closed throughout:
//
//   - Only an active deposit may be amended. An unknown ID fails with
//     ErrTermDepositNotFound; a matured or early-withdrawn deposit
//     fails with ErrTermDepositNotActive. As in LG-247, LG-249,
//     LG-250, and LG-251, there is no lazy-expiry reading here: a
//     deposit whose maturity date has passed but which nobody has
//     matured yet is still active.
//   - The new term reuses the open path's bounds (see
//     validateTermDeposit): 1..36500 days, via ErrInvalidTermDays.
//     A failed amendment leaves the original term exactly as it was.
//   - The amendment touches the term and the maturity date, and
//     nothing else: the principal, currency, rate, tier ladder,
//     business-day flag, rollover policy, and RolloversUsed are
//     unchanged, and interest already journaled stays journaled.
//     Booked daily rows are never repriced — their deterministic
//     entry IDs ("term-interest/<id>/<YYYY-MM-DD>") mean re-running
//     an old day replays the original row, and the accrual path
//     reads the live deposit row on every call (see
//     AccrueTermInterest), so future accruals stop at the amended
//     MaturesAt with no accrual-path change: an asof past the new
//     maturity is rejected with ErrTermDepositMatured, exactly as
//     before, and accrual at the new maturity still books.
//   - MaturesAt is recomputed exactly as at opening, via the LG-251
//     helper (see termDepositMaturesAt): OpenedAt + new TermDays
//     days, moved to the next business day when the stored
//     business-day flag is on and the raw date is not a business
//     day. The stored flag is applied, never re-derived: shortening
//     a business-day deposit can land its raw date on a weekend,
//     and the stored flag is what moves it. The live calendar is
//     used — the same calendar the open path reads — so a calendar
//     change between open and amendment moves the recomputation
//     with it, exactly as a fresh open would.
//   - If the recomputed maturity is already in the past, the
//     amendment itself books no maturity payout: it only moves the
//     date. The deposit becomes maturable at the next sweep (see
//     SweepTermDeposits), which matures it like any deposit whose
//     date has arrived.
//   - Amending to the current term is a no-op: the row is untouched
//     and no audit event fires, so a retried or redundant revision
//     does not pollute the audit trail with a change that changed
//     nothing.
//   - Rollover child rows are independent deposit rows with their
//     own term snapshots (the sweep copies the renewal term onto
//     the child, see rolloverTermDepositLocked), so amending a
//     child's term never rewrites its parent's row, and amending a
//     parent never walks the rollover chain. (A matured parent
//     cannot be amended at all, so the parent row's term is frozen
//     at maturity.)
//   - Like the other term-deposit transitions, the amendment is
//     structural: it bumps neither the ledger version nor the audit
//     chain, and no outbox event fires — the deposit was already
//     reported when opened. The amendment lands one audit event
//     (term_deposit_term_days_amended, with the deposit ID, the
//     previous and new terms, the previous and new maturity dates,
//     the stored business-day flag, the principal, and the
//     currency) so the "who moved the tenor, and from what to
//     what" trail is tamper-evident. Term deposits are snapshot
//     records, so the amended term and maturity ride the deposit
//     row through snapshots exactly like the originals — no
//     snapshot change is needed.
//
// Orthogonality: OpenTermDeposit owns the initial tenor,
// MatureTermDeposit and WithdrawTermDeposit own the terminal states,
// AccrueTermInterest owns the interest rows, SweepTermDeposits owns
// maturity, and this module owns only the term revision.

// AmendTermDepositTermDays revises an active term deposit's term in
// place, recomputing MaturesAt exactly as at opening: OpenedAt +
// new term days, applying the stored business-day flag calendar
// adjustment via the LG-251 helper. Only the term and the maturity
// date change; the principal, currency, rate, tier ladder, flag,
// rollover policy, and RolloversUsed are untouched, and interest
// already journaled stays journaled. A recomputed maturity already
// in the past books no payout — the deposit simply becomes
// maturable at the next sweep. Amending to the current term is a
// no-op. Amending is off-journal — it does not bump the ledger
// version — but it is audited as term_deposit_term_days_amended.
func (l *Ledger) AmendTermDepositTermDays(id string, termDays int64) (TermDeposit, error) {
	return l.AmendTermDepositTermDaysCtx(context.Background(), id, termDays)
}

// AmendTermDepositTermDaysCtx is AmendTermDepositTermDays with
// per-call audit actor attribution (LG-73).
func (l *Ledger) AmendTermDepositTermDaysCtx(ctx context.Context, id string, termDays int64) (TermDeposit, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	td, ok := l.termDeposits[id]
	if !ok {
		return TermDeposit{}, ErrTermDepositNotFound
	}
	if td.Status != TermDepositActive {
		return TermDeposit{}, fmt.Errorf("%w: status %q", ErrTermDepositNotActive, td.Status)
	}
	if termDays < 1 || termDays > 36500 {
		return TermDeposit{}, ErrInvalidTermDays
	}
	if td.TermDays == termDays {
		return td, nil
	}
	previousTerm := td.TermDays
	previousMatures := td.MaturesAt
	td.TermDays = termDays
	td.MaturesAt = termDepositMaturesAt(td.OpenedAt, termDays, td.BusinessDayMaturity, l.businessCalendar)
	l.termDeposits[id] = td
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "term_deposit_term_days_amended",
		Actor:         "AmendTermDepositTermDays",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{td.Account},
		Details: map[string]any{
			"term_deposit_id":       id,
			"previous_term_days":    previousTerm,
			"term_days":             termDays,
			"previous_matures_at":   previousMatures.UTC().Format(time.RFC3339),
			"matures_at":            td.MaturesAt.UTC().Format(time.RFC3339),
			"business_day_maturity": td.BusinessDayMaturity,
			"amount_cents":          td.AmountCents,
			"currency":              td.Currency,
		},
	})
	return td, nil
}
