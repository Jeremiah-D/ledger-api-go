package ledger

import (
	"context"
	"errors"
)

// FX clearing account amendment (LG-297).
//
// WithFXAccount installs the ledger-wide FX clearing account at
// construction; this module is the orthogonal runtime path: an operator
// revises the account in place, instead of rebuilding the ledger and
// replaying its journal. Revising in place matters because a rebuild
// is a new ledger — a gap in which the old clearing account (or none)
// governs cross-currency settlement, plus a fresh identity that breaks
// the audit story — while an amendment keeps the same ledger, the same
// journal, and the same audit trail from old account to new.
//
// Semantics, deliberately fail-closed throughout, along the
// SetFXIncomeAccount (LG-296) and SetFXRateProvider (LG-289)
// structural revisions:
//
//   - The revision takes effect immediately for every later
//     cross-currency transfer: both the single-transfer path and the
//     split path resolve the clearing account through l.fxAccount live
//     on every post (fx.go:958-960) — no posting-path change was
//     needed, exactly like the LG-289 provider swap. A transfer
//     already journaled keeps the clearing legs it booked; only later
//     transfers read the new value.
//   - The per-transfer override still wins: Transfer.FXAccount is
//     consulted before the ledger-wide account, so an amendment never
//     overrides a transfer that names its own clearing account.
//   - An empty install clears the account, exactly as at construction:
//     a ledger legitimately configures none, and after clearing, the
//     next cross-currency transfer fails closed with
//     ErrFXAccountNotConfigured before any mutation — a transfer must
//     never settle while its clearing legs have nowhere to go.
//   - A non-empty install equal to the ledger's FX spread-income
//     account l.fxIncomeAccount is rejected with
//     ErrFXAccountEqualsIncomeAccount and the previous value is left
//     untouched: the spread leg debits the clearing account and
//     credits the income account, so equal accounts would book a
//     self-leg. The post-time gate already rejects that state
//     (resolveFXIncomeAccountLocked, fx_spread.go:276-290); the
//     revision gate rejects it early, at amendment time.
//     ErrInvalidFXAccount (fx.go:85) is not reused: it carries the
//     payer/payee meaning. No account-existence check is made at
//     revision time, matching construction: WithFXAccount does not
//     check existence either.
//   - Revising to the current value is a no-op: no state change, no
//     audit event. The no-op check runs before the income-account
//     gate, so re-setting the live value never errors even if the
//     live value was installed by construction next to the income
//     account.
//   - The amendment is structural config: it bumps neither the ledger
//     version nor the audit chain, and no outbox event fires. It lands
//     one fx_account_amended audit event carrying the previous and new
//     accounts, along the fx_rate_provider_replaced detail shape. The
//     Ctx variant attributes the operator (LG-73).
//   - The amended value rides snapshots exactly like the original:
//     snapshot.go already writes, restores, and diffs fxAccount, and
//     dry-run clones copy the live field — no snapshot or dry-run
//     change is needed.
//
// Orthogonality: WithFXAccount owns the construction-time account,
// SetFXIncomeAccount (LG-296) owns the spread P&L credit account,
// SetFXBidAskRate owns the dealing prices, SetFXRateProvider owns the
// rate source, Transfer.FXAccount owns the per-transfer override, and
// this module owns only the runtime revision of the ledger-wide
// clearing account. There is no HTTP endpoint for the revision (per
// LG-241/242): the account is in-process structural config.

var (
	// ErrFXAccountEqualsIncomeAccount is returned when the FX clearing
	// account is set to the ledger's FX spread-income account: the
	// spread leg would debit and credit the same account. It is a
	// 400-class error — the caller can fix it by naming a distinct
	// account — and a rejected revision leaves the previous account
	// exactly as it was.
	ErrFXAccountEqualsIncomeAccount = errors.New("ledger: FX account must differ from the FX income account")
)

// SetFXAccount revises the ledger-wide FX clearing account in place.
// An empty account clears it (a ledger legitimately configures none);
// a non-empty account equal to the FX spread-income account is
// rejected with ErrFXAccountEqualsIncomeAccount. Revising to the
// current value is a no-op. Later cross-currency transfers settle
// their clearing legs through the new account; already-journaled legs
// are untouched. The revision is audited as fx_account_amended.
func (l *Ledger) SetFXAccount(a AccountID) error {
	return l.SetFXAccountCtx(context.Background(), a)
}

// SetFXAccountCtx is SetFXAccount with per-call audit actor
// attribution (LG-73).
func (l *Ledger) SetFXAccountCtx(ctx context.Context, a AccountID) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	previous := l.fxAccount
	if a == previous {
		return nil
	}
	if a != "" && a == l.fxIncomeAccount {
		return ErrFXAccountEqualsIncomeAccount
	}
	l.fxAccount = a
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "fx_account_amended",
		Actor:         "SetFXAccount",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"previous_account": string(previous),
			"account":          string(a),
		},
	})
	return nil
}

// FXAccount returns the configured ledger-wide FX clearing account, or
// "" when none is configured.
func (l *Ledger) FXAccount() AccountID {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.fxAccount
}
