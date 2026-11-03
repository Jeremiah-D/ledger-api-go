package ledger

import (
	"context"
	"errors"
)

// FX spread-income account amendment (LG-296).
//
// WithFXIncomeAccount (LG-93) installs the ledger-wide FX spread-income
// account at construction; this module is the orthogonal runtime path:
// an operator revises the account in place, instead of rebuilding the
// ledger and replaying its journal. Revising in place matters because
// a rebuild is a new ledger — a gap in which the old income account
// (or none) governs spread P&L, plus a fresh identity that breaks the
// audit story — while an amendment keeps the same ledger, the same
// journal, and the same audit trail from old account to new.
//
// Semantics, deliberately fail-closed throughout, along the
// SetFXRateProvider (LG-289) and SetRevaluationAccount structural
// revisions:
//
//   - The revision takes effect immediately for every later
//     cross-currency transfer: both the single-transfer path and the
//     split path resolve the income account through
//     resolveFXIncomeAccountLocked, which reads l.fxIncomeAccount live
//     on every post — no posting-path change was needed, exactly like
//     the LG-289 provider swap. A transfer already journaled keeps the
//     spread leg it booked; only later transfers read the new value.
//   - The per-transfer override still wins: Transfer.FXIncomeAccount
//     is consulted before the ledger-wide account, so an amendment
//     never overrides a transfer that names its own income account.
//   - An empty install clears the account, exactly as at construction:
//     a mid-only ledger legitimately configures none, and after
//     clearing, the next positive-spread transfer fails closed with
//     ErrFXIncomeAccountNotConfigured before any mutation — a transfer
//     must never settle while its P&L has nowhere to go.
//   - A non-empty install equal to the ledger's FX clearing account
//     l.fxAccount is rejected with ErrInvalidFXIncomeAccount and the
//     previous value is left untouched: the spread leg debits the
//     clearing account and credits the income account, so equal
//     accounts would book a self-leg. The post-time gate already
//     rejects that state; the revision gate rejects it early, at
//     amendment time. l.fxAccount has no runtime revision entry, so
//     the comparison stays valid: the gate reads the live l.fxAccount
//     at revision time, and SetFXAccount (LG-297) rejects the mirror
//     install from the other side, so the two accounts can never be
//     made equal through either revision entry. No account-existence
//     check is made at revision time, matching construction:
//     WithFXIncomeAccount does not check existence either.
//   - Revising to the current value is a no-op: no state change, no
//     audit event. The no-op check runs before the clearing-account
//     gate, so re-setting the live value never errors even if the
//     live value was installed by construction next to the FX account.
//   - The amendment is structural config: it bumps neither the ledger
//     version nor the audit chain, and no outbox event fires. It lands
//     one fx_income_account_amended audit event carrying the previous
//     and new accounts, along the fx_rate_provider_replaced detail
//     shape. The Ctx variant attributes the operator (LG-73).
//   - The amended value rides snapshots exactly like the original:
//     snapshot.go already writes, restores, and diffs fxIncomeAccount,
//     and dry-run clones copy the live field — no snapshot or dry-run
//     change is needed.
//
// Orthogonality: WithFXIncomeAccount owns the construction-time
// account, SetFXBidAskRate owns the dealing prices, SetFXRateProvider
// owns the rate source, Transfer.FXIncomeAccount owns the per-transfer
// override, SetFXAccount (LG-297) owns the clearing account, and this
// module owns only the runtime revision of the ledger-wide income
// account. There is no HTTP endpoint for the revision (per LG-241/242):
// the account is in-process structural config.

var (
	// ErrInvalidFXIncomeAccount is returned when the FX spread-income
	// account is set to the ledger's FX clearing account: the spread
	// leg would debit and credit the same account. It is a 400-class
	// error — the caller can fix it by naming a distinct account —
	// and a rejected revision leaves the previous account exactly as
	// it was.
	ErrInvalidFXIncomeAccount = errors.New("ledger: FX income account must differ from the FX account")
)

// SetFXIncomeAccount revises the ledger-wide FX spread-income account
// in place. An empty account clears it (a mid-only ledger legitimately
// configures none); a non-empty account equal to the FX clearing
// account is rejected with ErrInvalidFXIncomeAccount. Revising to the
// current value is a no-op. Later cross-currency transfers credit
// their spread legs to the new account; already-journaled legs are
// untouched. The revision is audited as fx_income_account_amended.
func (l *Ledger) SetFXIncomeAccount(a AccountID) error {
	return l.SetFXIncomeAccountCtx(context.Background(), a)
}

// SetFXIncomeAccountCtx is SetFXIncomeAccount with per-call audit
// actor attribution (LG-73).
func (l *Ledger) SetFXIncomeAccountCtx(ctx context.Context, a AccountID) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	previous := l.fxIncomeAccount
	if a == previous {
		return nil
	}
	if a != "" && a == l.fxAccount {
		return ErrInvalidFXIncomeAccount
	}
	l.fxIncomeAccount = a
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "fx_income_account_amended",
		Actor:         "SetFXIncomeAccount",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"previous_income_account": string(previous),
			"income_account":          string(a),
		},
	})
	return nil
}

// FXIncomeAccount returns the configured ledger-wide FX spread-income
// account, or "" when none is configured.
func (l *Ledger) FXIncomeAccount() AccountID {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.fxIncomeAccount
}
