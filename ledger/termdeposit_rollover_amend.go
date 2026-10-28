package ledger

import (
	"context"
	"fmt"
	"reflect"
)

// Term-deposit rollover-policy amendment (LG-250).
//
// OpenTermDeposit fixes a deposit's rollover policy at open (LG-139);
// MatureTermDeposit and WithdrawTermDeposit own the terminal
// transitions. This module is the orthogonal in-place policy path:
// an operator revises a live deposit's renewal terms instead of
// withdrawing it early and opening a new one. Amending in place
// matters for the same reason as the flat-rate (LG-247) and ladder
// (LG-249) amendments: an early withdrawal plus a re-open is a new
// deposit — a fresh ID and a fresh tenor that breaks the deposit's
// accrual history — while an amendment keeps the same deposit, the
// same maturity date, and one audit trail from open to maturity.
//
// Semantics, deliberately fail-closed throughout:
//
//   - Only an active deposit may be amended. An unknown ID fails with
//     ErrTermDepositNotFound; a matured or early-withdrawn deposit
//     fails with ErrTermDepositNotActive. As in LG-247 and LG-249,
//     there is no lazy-expiry reading here: a deposit whose maturity
//     date has passed but which nobody has matured yet is still
//     active.
//   - Only a deposit that already carries a rollover policy may be
//     amended. A deposit with no policy (nil, or mode "none" —
//     OpenTermDeposit normalizes mode "none" to nil) fails with
//     ErrTermDepositRolloverNotConfigured: a rollover policy cannot
//     be installed after opening. Symmetrically, the new policy
//     must itself be a real policy: a nil policy or mode "none"/""
//     is rejected — there is no clear-policy path, so closing
//     rollover means letting the term mature and opening a new one
//     without one.
//   - The replacement is the whole policy, never a per-field patch:
//     mode, term, and cap are one renewal contract, and a partial
//     revision could silently leave a cap the operator meant to
//     raise. The new policy is validated by the shared rollover
//     validator (see validateRolloverPolicy): mode
//     principal/principal_plus_interest, term 1..36500 days, cap
//     >= 1. A failed amendment leaves the original policy exactly
//     as it was.
//   - The new cap must not fall below the rolls already used: a cap
//     below RolloversUsed would retroactively invalidate renewals
//     that already happened, so it fails with
//     ErrInvalidRolloverMaxRollovers and the original policy stands.
//     Raising the cap extends the chain; lowering it to exactly the
//     used count simply ends the chain at the next maturity.
//   - Amending to the current policy is a no-op: the row is untouched
//     and no audit event fires, so a retried or redundant revision
//     does not pollute the audit trail with a change that changed
//     nothing.
//   - The amendment touches the policy and nothing else: the
//     principal, currency, rate, tenor, maturity date, and
//     RolloversUsed are unchanged, and interest already journaled
//     stays journaled. The sweep reads the live policy at maturity
//     (see rolloverTermDepositLocked), so the new policy takes
//     effect from the very next maturity — a new term, mode, or cap
//     for the renewal the sweep is about to open — with no sweep-path
//     change. The policy is copied defensively, so the caller's
//     struct never aliases the stored record.
//   - Rollover children are independent rows: a child generated
//     before an amendment holds its own policy snapshot (the sweep
//     copies the policy onto the child, see
//     rolloverTermDepositLocked), so amending a child's policy never
//     rewrites its parent's row, and amending a parent never walks
//     the rollover chain. (A matured parent cannot be amended at
//     all, so the parent row's policy is frozen at maturity.)
//   - Like the other term-deposit transitions, the amendment is
//     structural: it bumps neither the ledger version nor the audit
//     chain, and no outbox event fires — the deposit was already
//     reported when opened. The amendment lands one audit event
//     (term_deposit_rollover_amended, with the deposit ID, the
//     previous and new policies, the rolls used, the principal, and
//     the currency) so the "who revised the renewal, and from what
//     to what" trail is tamper-evident. Term deposits are snapshot
//     records, so the amended policy rides the deposit row through
//     snapshots exactly like the original — no snapshot change is
//     needed.
//
// Orthogonality: OpenTermDeposit owns the initial policy,
// MatureTermDeposit and WithdrawTermDeposit own the terminal states,
// SweepTermDeposits owns the renewal itself, the LG-139 rollover
// mechanism owns renewals, and this module owns only the policy
// revision.

// AmendTermDepositRollover replaces an active term deposit's
// rollover policy in place, as a whole. The deposit must already
// carry a policy, and the new policy must be a real one (nil and
// mode "none" are rejected — there is no clear-policy path); the
// new cap must not fall below the rolls already used. A failed
// amendment leaves the deposit exactly as it was. Amending to the
// current policy is a no-op. Amending is off-journal — it does not
// bump the ledger version — but it is audited as
// term_deposit_rollover_amended.
func (l *Ledger) AmendTermDepositRollover(id string, policy *RolloverPolicy) (TermDeposit, error) {
	return l.AmendTermDepositRolloverCtx(context.Background(), id, policy)
}

// AmendTermDepositRolloverCtx is AmendTermDepositRollover with
// per-call audit actor attribution (LG-73).
func (l *Ledger) AmendTermDepositRolloverCtx(ctx context.Context, id string, policy *RolloverPolicy) (TermDeposit, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	td, ok := l.termDeposits[id]
	if !ok {
		return TermDeposit{}, ErrTermDepositNotFound
	}
	if td.Status != TermDepositActive {
		return TermDeposit{}, fmt.Errorf("%w: status %q", ErrTermDepositNotActive, td.Status)
	}
	if td.Rollover == nil || td.Rollover.Mode == "" || td.Rollover.Mode == RolloverNone {
		return TermDeposit{}, ErrTermDepositRolloverNotConfigured
	}
	if policy == nil || policy.Mode == "" || policy.Mode == RolloverNone {
		return TermDeposit{}, fmt.Errorf("%w: cannot clear a term deposit rollover policy", ErrInvalidRolloverMode)
	}
	if err := validateRolloverPolicy(policy); err != nil {
		return TermDeposit{}, err
	}
	if policy.MaxRollovers < td.RolloversUsed {
		return TermDeposit{}, fmt.Errorf("%w: max %d below rolls used %d", ErrInvalidRolloverMaxRollovers, policy.MaxRollovers, td.RolloversUsed)
	}
	previous := *td.Rollover
	if reflect.DeepEqual(previous, *policy) {
		return td, nil
	}
	// Defensive copy: the caller's struct must not alias the stored
	// record — a later mutation by the caller would otherwise
	// rewrite the deposit's policy.
	td.Rollover = &RolloverPolicy{
		Mode:         policy.Mode,
		TermDays:     policy.TermDays,
		MaxRollovers: policy.MaxRollovers,
	}
	l.termDeposits[id] = td
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "term_deposit_rollover_amended",
		Actor:         "AmendTermDepositRollover",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{td.Account},
		Details: map[string]any{
			"term_deposit_id":             id,
			"previous_rollover_mode":      string(previous.Mode),
			"previous_rollover_term_days": previous.TermDays,
			"previous_rollover_max":       previous.MaxRollovers,
			"rollover_mode":               string(policy.Mode),
			"rollover_term_days":          policy.TermDays,
			"rollover_max":                policy.MaxRollovers,
			"rollovers_used":              td.RolloversUsed,
			"amount_cents":                td.AmountCents,
			"currency":                    td.Currency,
		},
	})
	return td, nil
}
