package ledger

import (
	"context"
)

// FTP internal counterparty accounts amendment (LG-298).
//
// WithFtpAccounts installs the three internal counterparties of every
// FTP charge at construction; this module is the orthogonal runtime
// path: an operator revises the triple in place, instead of rebuilding
// the ledger and replaying its journal. Revising in place matters
// because a rebuild is a new ledger — a gap in which the old
// counterparties (or none) govern FTP pricing, plus a fresh identity
// that breaks the audit story — while an amendment keeps the same
// ledger, the same journal, and the same audit trail from old triple
// to new.
//
// Semantics, deliberately fail-closed throughout, along the
// SetFXIncomeAccount (LG-296) and SetFXAccount (LG-297) structural
// revisions:
//
//   - The revision takes effect immediately for every later FTP
//     charge and preview: ChargeFtp resolves the debit/credit legs
//     through l.ftpAccounts live on every post (ftp.go:489-491), and
//     FtpPreview gates on ftpAccountsConfiguredLocked (ftp.go:410) —
//     no pricing- or posting-path change was needed, exactly like the
//     LG-289 provider swap. A charge already journaled keeps the legs
//     it booked; only later charges read the new triple.
//   - There is no "clear" operation: the zero triple FtpAccounts{}
//     fails checkFtpAccounts (all three accounts must be non-empty),
//     so a revision only ever swaps one valid triple for another. A
//     ledger that never configured the accounts stays fail-closed
//     with ErrFtpAccountsNotConfigured until a legal triple is
//     installed.
//   - An invalid triple is rejected with the existing
//     ErrInvalidFtpAccounts and the previous triple is left untouched:
//     validation reuses checkFtpAccounts (ftp.go:183-194 — all three
//     accounts non-empty, slash-free, pairwise distinct), so the
//     construction-time and runtime gates can never disagree. No new
//     error is introduced, and no account-existence check is made at
//     revision time, matching construction: WithFtpAccounts does not
//     check existence either.
//   - Revising to the current triple is a no-op: no state change, no
//     audit event. The no-op check runs before validation, so
//     re-setting the live triple never errors — along the LG-296
//     precedent. (Re-setting the zero triple on a ledger that never
//     configured accounts is likewise a no-op, not an error.)
//   - The amendment is structural config: it bumps neither the ledger
//     version nor the audit chain, and no outbox event fires. It lands
//     one ftp_accounts_amended audit event carrying the previous and
//     new pool/expense/revenue accounts, along the
//     fx_income_account_amended detail shape. The Ctx variant
//     attributes the operator (LG-73).
//   - The amended triple rides snapshots exactly like the original:
//     snapshot.go already writes, restores, and diffs ftpAccounts,
//     and there is no DryRunChargeFtp entry, so no snapshot or
//     dry-run change is needed. (Dry-run clones do not copy the FTP
//     config, but no dry-run path prices FTP, so nothing is lost.)
//
// Orthogonality: WithFtpAccounts owns the construction-time triple,
// SetFtpCurve (LG-105) owns the per-currency rate curves, and this
// module owns only the runtime revision of the three internal
// counterparty accounts. There is no HTTP endpoint for the revision
// (per LG-241/242): the accounts are in-process structural config.

// SetFtpAccounts revises the three internal FTP counterparty accounts
// in place. The triple must pass checkFtpAccounts: all three accounts
// non-empty, slash-free, and pairwise distinct — anything else is
// rejected with ErrInvalidFtpAccounts and the previous triple is left
// untouched. Revising to the current triple is a no-op. Later
// FtpPreview/ChargeFtp calls use the new accounts; already-journaled
// charges are untouched. The revision is audited as
// ftp_accounts_amended.
func (l *Ledger) SetFtpAccounts(a FtpAccounts) error {
	return l.SetFtpAccountsCtx(context.Background(), a)
}

// SetFtpAccountsCtx is SetFtpAccounts with per-call audit actor
// attribution (LG-73).
func (l *Ledger) SetFtpAccountsCtx(ctx context.Context, a FtpAccounts) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	previous := l.ftpAccounts
	if a == previous {
		return nil
	}
	if err := checkFtpAccounts(a); err != nil {
		return err
	}
	l.ftpAccounts = a
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "ftp_accounts_amended",
		Actor:         "SetFtpAccounts",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"previous_pool_account":    string(previous.PoolAccount),
			"previous_expense_account": string(previous.ExpenseAccount),
			"previous_revenue_account": string(previous.RevenueAccount),
			"pool_account":             string(a.PoolAccount),
			"expense_account":          string(a.ExpenseAccount),
			"revenue_account":          string(a.RevenueAccount),
		},
	})
	return nil
}

// FtpAccounts returns the configured internal FTP counterparty
// accounts, or the zero FtpAccounts when none is configured.
func (l *Ledger) FtpAccounts() FtpAccounts {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.ftpAccounts
}
