package ledger

import (
	"context"
	"fmt"
	"time"
)

// Term-deposit business-day-maturity amendment (LG-251).
//
// OpenTermDeposit fixes a deposit's business-day settlement flag at
// open (LG-95); MatureTermDeposit and WithdrawTermDeposit own the
// terminal transitions. This module is the orthogonal in-place flag
// path: an operator toggles whether a live deposit's maturity
// settles on business days, instead of withdrawing it early and
// opening a new one. Amending in place matters for the same reason
// as the flat-rate (LG-247), ladder (LG-249), and rollover-policy
// (LG-250) amendments: an early withdrawal plus a re-open is a new
// deposit — a fresh ID and a fresh tenor that breaks the deposit's
// accrual history — while an amendment keeps the same deposit, the
// same principal, and one audit trail from open to maturity.
//
// Semantics, deliberately fail-closed throughout:
//
//   - Only an active deposit may be amended. An unknown ID fails with
//     ErrTermDepositNotFound; a matured or early-withdrawn deposit
//     fails with ErrTermDepositNotActive. As in LG-247, LG-249, and
//     LG-250, there is no lazy-expiry reading here: a deposit whose
//     maturity date has passed but which nobody has matured yet is
//     still active.
//   - The amendment touches the flag and the maturity date, and
//     nothing else: the principal, currency, rate, tier ladder, term,
//     and rollover policy are unchanged, and interest already
//     journaled stays journaled. Booked daily rows are never
//     repriced — their deterministic entry IDs
//     ("term-interest/<id>/<YYYY-MM-DD>") mean re-running an old day
//     replays the original row, and the accrual path reads the live
//     deposit row on every call (see AccrueTermInterest), so future
//     accruals stop at the amended MaturesAt with no accrual-path
//     change: an asof past the new maturity is rejected with
//     ErrTermDepositMatured, exactly as before.
//   - MaturesAt is recomputed exactly as at opening (see
//     termDepositMaturesAt): OpenedAt + TermDays days, moved to the
//     next business day when the new flag is on and the raw date is
//     not a business day, and restored to the raw date when the flag
//     is turned off. The live calendar is used — the same calendar
//     the open path reads — so a calendar change between open and
//     amendment moves the recomputation with it, exactly as a fresh
//     open would.
//   - Amending to the current flag is a no-op: the row is untouched
//     and no audit event fires, so a retried or redundant revision
//     does not pollute the audit trail with a change that changed
//     nothing. But a flag change whose recomputed date happens to
//     equal the stored date is still a real amendment with an audit
//     event: the flag itself is contractual state — a later term
//     amendment (LG-252) applies the stored flag when it recomputes
//     the maturity — so the row did change even though the date did
//     not.
//   - If the recomputed maturity is already in the past, the
//     amendment itself books no maturity payout: it only moves the
//     date. The deposit becomes maturable at the next sweep (see
//     SweepTermDeposits), which matures it like any deposit whose
//     date has arrived.
//   - Like the other term-deposit transitions, the amendment is
//     structural: it bumps neither the ledger version nor the audit
//     chain, and no outbox event fires — the deposit was already
//     reported when opened. The amendment lands one audit event
//     (term_deposit_business_day_maturity_amended, with the deposit
//     ID, the previous and new flags, the previous and new maturity
//     dates, the term, the principal, and the currency) so the "who
//     moved the settlement date, and from what to what" trail is
//     tamper-evident. Term deposits are snapshot records, so the
//     amended flag and maturity ride the deposit row through
//     snapshots exactly like the originals — no snapshot change is
//     needed.
//
// Orthogonality: OpenTermDeposit owns the initial flag,
// MatureTermDeposit and WithdrawTermDeposit own the terminal states,
// AccrueTermInterest owns the interest rows, SweepTermDeposits owns
// maturity, and this module owns only the flag revision.

// termDepositMaturesAt recomputes a deposit's maturity date exactly as
// OpenTermDeposit derives it at open (see openTermDepositLocked):
// OpenedAt + TermDays*24h, moved to the next business day when the
// business-day flag is on and the raw date is not a business day.
// The calendar is the ledger's live one, as at open.
func termDepositMaturesAt(openedAt time.Time, termDays int64, businessDay bool, cal BusinessDayCalendar) time.Time {
	matures := openedAt.Add(time.Duration(termDays) * 24 * time.Hour)
	if businessDay && !cal.IsBusinessDay(matures) {
		matures = cal.NextBusinessDay(matures)
	}
	return matures
}

// AmendTermDepositBusinessDayMaturity toggles an active term
// deposit's business-day settlement flag in place, recomputing
// MaturesAt exactly as at opening: OpenedAt + TermDays days, moved
// to the next business day when the flag is on and the raw date is
// not a business day, restored to the raw date when the flag is off.
// Only the flag and the maturity date change; the principal,
// currency, rate, tier ladder, term, and rollover policy are
// untouched, and interest already journaled stays journaled. A
// recomputed maturity already in the past books no payout — the
// deposit simply becomes maturable at the next sweep. Amending to
// the current flag is a no-op. Amending is off-journal — it does
// not bump the ledger version — but it is audited as
// term_deposit_business_day_maturity_amended.
func (l *Ledger) AmendTermDepositBusinessDayMaturity(id string, enabled bool) (TermDeposit, error) {
	return l.AmendTermDepositBusinessDayMaturityCtx(context.Background(), id, enabled)
}

// AmendTermDepositBusinessDayMaturityCtx is
// AmendTermDepositBusinessDayMaturity with per-call audit actor
// attribution (LG-73).
func (l *Ledger) AmendTermDepositBusinessDayMaturityCtx(ctx context.Context, id string, enabled bool) (TermDeposit, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	td, ok := l.termDeposits[id]
	if !ok {
		return TermDeposit{}, ErrTermDepositNotFound
	}
	if td.Status != TermDepositActive {
		return TermDeposit{}, fmt.Errorf("%w: status %q", ErrTermDepositNotActive, td.Status)
	}
	if td.BusinessDayMaturity == enabled {
		return td, nil
	}
	previousMatures := td.MaturesAt
	previous := td.BusinessDayMaturity
	td.BusinessDayMaturity = enabled
	td.MaturesAt = termDepositMaturesAt(td.OpenedAt, td.TermDays, enabled, l.businessCalendar)
	l.termDeposits[id] = td
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "term_deposit_business_day_maturity_amended",
		Actor:         "AmendTermDepositBusinessDayMaturity",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{td.Account},
		Details: map[string]any{
			"term_deposit_id":                id,
			"previous_business_day_maturity": previous,
			"business_day_maturity":          enabled,
			"previous_matures_at":            previousMatures.UTC().Format(time.RFC3339),
			"matures_at":                     td.MaturesAt.UTC().Format(time.RFC3339),
			"term_days":                      td.TermDays,
			"amount_cents":                   td.AmountCents,
			"currency":                       td.Currency,
		},
	})
	return td, nil
}
