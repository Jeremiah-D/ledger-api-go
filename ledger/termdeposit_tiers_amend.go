package ledger

import (
	"context"
	"fmt"
	"reflect"
)

// Term-deposit tiered-ladder amendment (LG-249).
//
// OpenTermDeposit fixes a deposit's tiered ladder at open (LG-145);
// MatureTermDeposit and WithdrawTermDeposit own the terminal
// transitions. This module is the orthogonal in-place ladder path:
// an operator reprices a live tiered deposit's bands instead of
// withdrawing it early and opening a new one. Amending in place
// matters for the same reason as the flat-rate amendment (LG-247):
// an early withdrawal plus a re-open is a new deposit — a fresh ID
// and a fresh tenor that breaks the deposit's accrual history —
// while an amendment keeps the same deposit, the same maturity date,
// and one audit trail from open to maturity.
//
// Semantics, deliberately fail-closed throughout:
//
//   - Only an active deposit may be amended. An unknown ID fails with
//     ErrTermDepositNotFound; a matured or early-withdrawn deposit
//     fails with ErrTermDepositNotActive. As in LG-247, there is no
//     lazy-expiry reading here: a deposit whose maturity date has
//     passed but which nobody has matured yet is still active.
//   - Only tiered deposits may be amended. A flat-rate deposit
//     (RateTiers empty) fails with ErrTermDepositRateTiersConflict:
//     the flat rate and the ladder are mutually exclusive, and this
//     is the mirror image of LG-247's flat-only scope — a ladder
//     amendment must never install a flat rate, and it must never
//     turn a flat deposit into a tiered one. There is no flat<->tiered
//     mode switch anywhere in the amendment family.
//   - The replacement is the whole ladder, never a per-band patch:
//     bands carry positional semantics (band i prices the principal
//     slice [MinCents_i, MinCents_{i+1})), so patching one band in
//     place would silently reprice the neighbouring slices. The new
//     ladder is validated as a whole by the shared tier validator
//     (see validateInterestTiers): first band at 0, minima strictly
//     ascending, rates 1..10000 bps. An empty ladder is rejected —
//     unlike SetInterestTiers, where empty means "clear", clearing a
//     deposit's ladder would leave it priceless, and there is no
//     clear-ladder path for a live deposit — so a failed amendment
//     leaves the original ladder exactly as it was.
//   - Amending to the current ladder is a no-op: the row is untouched
//     and no audit event fires, so a retried or redundant revision
//     does not pollute the audit trail with a change that changed
//     nothing.
//   - The amendment touches the ladder and nothing else: the
//     principal, currency, tenor, maturity date, and rollover policy
//     are unchanged, and interest already journaled stays journaled.
//     AccrueTermInterest reads the live deposit row on every accrual
//     (see AccrueTermInterest), so the new ladder takes effect from
//     the very next accrual day with no accrual-path change — and
//     re-running an old day replays the original row at the old
//     ladder instead of repricing it. The ladder is copied
//     defensively, so the caller's slice never aliases the stored
//     record.
//   - Rollover inheritance is generation-time, never retroactive:
//     the sweep copies the parent's live RateTiers into a child row
//     when it rolls (see rolloverTermDepositLocked), so a child
//     generated before an amendment keeps the ladder it was born
//     with — the amendment writes only the parent's row — while a
//     child generated after the amendment inherits the amended
//     ladder, like any new term opened after a reprice. The
//     amendment itself never walks the rollover chain.
//   - Like the other term-deposit transitions, the amendment is
//     structural: it bumps neither the ledger version nor the audit
//     chain, and no outbox event fires — the deposit was already
//     reported when opened. The amendment lands one audit event
//     (term_deposit_tiers_amended, with the deposit ID, the previous
//     and new ladders, the principal, and the currency) so the "who
//     repriced the ladder, and from what to what" trail is
//     tamper-evident. Term deposits are snapshot records, so the
//     amended ladder rides the deposit row through snapshots exactly
//     like the original — no snapshot change is needed.
//
// Orthogonality: OpenTermDeposit owns the initial ladder,
// MatureTermDeposit and WithdrawTermDeposit own the terminal states,
// AccrueTermInterest owns the interest rows, the LG-247 flat-rate
// amendment owns flat repricing, the LG-139 rollover policy owns
// renewals, and this module owns only the tiered-ladder revision.

// termDepositTiersDetails renders a ladder for an audit event, the
// same shape the open path discloses (see openTermDepositLocked).
func termDepositTiersDetails(tiers []InterestTier) []map[string]any {
	out := make([]map[string]any, 0, len(tiers))
	for _, tier := range tiers {
		out = append(out, map[string]any{
			"min_cents": tier.MinCents,
			"rate_bps":  tier.RateBps,
		})
	}
	return out
}

// AmendTermDepositTiers replaces an active tiered term deposit's
// rate ladder in place, as a whole. The new ladder must pass
// validateInterestTiers and must not be empty; a failed amendment
// leaves the deposit exactly as it was. Amending to the current
// ladder is a no-op. Amending is off-journal — it does not bump the
// ledger version — but it is audited as
// term_deposit_tiers_amended.
func (l *Ledger) AmendTermDepositTiers(id string, tiers []InterestTier) (TermDeposit, error) {
	return l.AmendTermDepositTiersCtx(context.Background(), id, tiers)
}

// AmendTermDepositTiersCtx is AmendTermDepositTiers with per-call
// audit actor attribution (LG-73).
func (l *Ledger) AmendTermDepositTiersCtx(ctx context.Context, id string, tiers []InterestTier) (TermDeposit, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	td, ok := l.termDeposits[id]
	if !ok {
		return TermDeposit{}, ErrTermDepositNotFound
	}
	if td.Status != TermDepositActive {
		return TermDeposit{}, fmt.Errorf("%w: status %q", ErrTermDepositNotActive, td.Status)
	}
	if len(td.RateTiers) == 0 {
		return TermDeposit{}, ErrTermDepositRateTiersConflict
	}
	if len(tiers) == 0 {
		return TermDeposit{}, fmt.Errorf("%w: ladder must not be empty", ErrInvalidInterestTier)
	}
	if err := validateInterestTiers(tiers); err != nil {
		return TermDeposit{}, err
	}
	previous := td.RateTiers
	if reflect.DeepEqual(previous, tiers) {
		return td, nil
	}
	// Defensive copy: the caller's slice must not alias the stored
	// record — a later append by the caller would otherwise rewrite
	// the deposit's ladder.
	td.RateTiers = append([]InterestTier(nil), tiers...)
	l.termDeposits[id] = td
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "term_deposit_tiers_amended",
		Actor:         "AmendTermDepositTiers",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{td.Account},
		Details: map[string]any{
			"term_deposit_id":     id,
			"previous_rate_tiers": termDepositTiersDetails(previous),
			"rate_tiers":          termDepositTiersDetails(td.RateTiers),
			"amount_cents":        td.AmountCents,
			"currency":            td.Currency,
		},
	})
	return td, nil
}
