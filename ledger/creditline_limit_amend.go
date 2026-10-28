package ledger

import (
	"context"
)

// Credit-line limit amendment (LG-245).
//
// GrantCreditLine (LG-91) is create-only — re-granting an existing
// (account, currency) line fails with ErrCreditLineExists — and
// ReduceCreditLine only lowers a limit, revoking the line entirely
// at zero. Neither owns a raise: an operator who wanted a higher
// limit had to revoke the line and re-grant it, a new line with a
// fresh GrantedAt that breaks the utilization and interest history,
// plus a gap in which the account has no floor at all. This module
// is the orthogonal in-place limit path, in both directions: an
// operator revises a live line's ceiling without touching its
// identity, its rate, or its history.
//
// Semantics, deliberately fail-closed throughout, along the
// GrantCreditLine and ReduceCreditLine gates:
//
//   - Only a granted line may be amended. An empty account fails
//     with ErrEmptyCreditLineAccount; an account with no line in
//     the currency — an unknown account, or a known account that
//     was never granted a line in that currency — fails with
//     ErrCreditLineNotFound, the same lookup miss ReduceCreditLine
//     reports. There is no separate account-existence gate:
//     GrantCreditLine does not check that the account exists
//     either, so "no line" is the only ungranted state.
//   - The new limit follows the GrantCreditLine gate exactly: it
//     must be positive, else ErrCreditLineNonPositiveLimit. Zero
//     is not a revision here — zero is the ReduceCreditLine
//     revocation path, and this module never deletes a line, so
//     an operator who wants the line gone still reduces it to
//     zero. A failed amendment leaves the original limit exactly
//     as it was.
//   - The new limit must not sit below the line's current
//     utilization — ErrCreditLineBelowUtilization, the same gate
//     ReduceCreditLine applies — because money already drawn
//     cannot be un-drawn by shrinking the line. A limit equal to
//     the utilization is legal: the account is at its floor and
//     may not draw further, but nothing already drawn is
//     disturbed.
//   - The amendment touches the limit and nothing else: the rate,
//     the GrantedAt instant, and the interest-income account are
//     unchanged. There is no clawback question: postings already
//     journaled stay journaled, and the overdraft floor reads the
//     live creditLines table on every posting (see
//     overdraftRejectedLocked), so the new limit takes effect
//     from the very next posting with no posting-path change —
//     a raise immediately widens the floor a posting may reach,
//     and a lowering immediately tightens it, while a posting
//     that already cleared under the old limit is never revisited.
//   - Like the other credit-line changes, the amendment is
//     structural config: it bumps neither the ledger version nor
//     the audit chain, and no outbox event fires (ReduceCreditLine
//     follows the same convention). The amendment lands one audit
//     event (credit_line_limit_amended, with the account, the
//     currency, the previous and new limits, and the rate) so the
//     "who moved the ceiling, and from what to what" trail is
//     tamper-evident. Credit lines are snapshot records, so the
//     amended limit rides the line record through snapshots
//     exactly like the original — no snapshot change is needed.
//
// Orthogonality: GrantCreditLine owns the initial limit (and the
// line itself), ReduceCreditLine owns lowering and revocation at
// zero, AmendCreditLineRate (LG-244) owns the interest rate,
// SetCreditInterestIncome owns the income account,
// AccrueCreditInterest owns the charges, and this module owns only
// the limit revision — including the raise, which no earlier path
// offered.

// AmendCreditLineLimit sets a granted credit line's limit to
// newLimitCents in place. The new limit must be positive and must
// not sit below the line's current utilization; a failed amendment
// leaves the line exactly as it was. Amending is off-journal — it
// does not bump the ledger version — but it is audited as
// credit_line_limit_amended.
func (l *Ledger) AmendCreditLineLimit(account AccountID, currency string, newLimitCents int64) (CreditLine, error) {
	return l.AmendCreditLineLimitCtx(context.Background(), account, currency, newLimitCents)
}

// AmendCreditLineLimitCtx is AmendCreditLineLimit with per-call audit
// actor attribution (LG-73).
func (l *Ledger) AmendCreditLineLimitCtx(ctx context.Context, account AccountID, currency string, newLimitCents int64) (CreditLine, error) {
	if account == "" {
		return CreditLine{}, ErrEmptyCreditLineAccount
	}
	cur, err := normalizeCurrency(currency)
	if err != nil {
		return CreditLine{}, err
	}
	if newLimitCents <= 0 {
		return CreditLine{}, ErrCreditLineNonPositiveLimit
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	k := creditLineKey{account: account, currency: cur}
	cl, exists := l.creditLines[k]
	if !exists {
		return CreditLine{}, ErrCreditLineNotFound
	}
	if used := l.creditLineUsedLocked(k); newLimitCents < used {
		return CreditLine{}, ErrCreditLineBelowUtilization
	}
	previous := cl.LimitCents
	cl.LimitCents = newLimitCents
	l.creditLines[k] = cl
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "credit_line_limit_amended",
		Actor:         "AmendCreditLineLimit",
		TraceID:       string(account) + "/" + cur,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{account},
		Details: map[string]any{
			"account":             string(account),
			"currency":            cur,
			"previous_limit_cents": previous,
			"limit_cents":         newLimitCents,
			"rate_bps":            cl.RateBps,
		},
	})
	return cl, nil
}
