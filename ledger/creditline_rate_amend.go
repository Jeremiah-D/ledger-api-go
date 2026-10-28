package ledger

import (
	"context"
)

// Credit-line interest rate amendment (LG-244).
//
// GrantCreditLine (LG-91) fixes a line's annual interest rate at
// grant time; ReduceCreditLine owns the limit (and revocation at
// zero). This module is the orthogonal in-place rate path: an
// operator reprices a live line's interest instead of revoking it
// and re-granting. Amending in place matters because a
// revoke-and-regrant is a new line — a fresh GrantedAt that breaks
// the utilization and interest history, plus a gap in which the
// account has no floor at all — while an amendment keeps the same
// line, the same floor, and one audit trail from grant to every
// accrual under it.
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
//   - The new rate follows the GrantCreditLine gate exactly:
//     1..10000 basis points, else ErrInvalidCreditLineRate — a
//     rate above 100% per year is a configuration bug, not a
//     clampable rate. A failed amendment leaves the original
//     rate exactly as it was.
//   - The amendment touches the rate and nothing else: the limit,
//     the drawn utilization, the GrantedAt instant, and the
//     interest-income account are unchanged. There is no
//     clawback question: interest already journaled stays
//     journaled, and AccrueCreditInterest reads the live RateBps
//     on every accrual (see AccrueCreditInterest), so the new
//     rate takes effect from the very next accrual with no
//     accrual-path change — the next day's charge prices the
//     current utilization at the new rate, and re-running an old
//     day replays the original row at the old rate instead of
//     repricing it.
//   - Like the other credit-line changes, the amendment is
//     structural config: it bumps neither the ledger version nor
//     the audit chain, and no outbox event fires (ReduceCreditLine
//     follows the same convention). The amendment lands one audit
//     event (credit_line_rate_amended, with the account, the
//     currency, the previous and new rates, and the limit) so the
//     "who repriced the line, and from what to what" trail is
//     tamper-evident. Credit lines are snapshot records, so the
//     amended rate rides the line record through snapshots
//     exactly like the original — no snapshot change is needed.
//
// Orthogonality: GrantCreditLine owns the initial rate (and the
// line itself), ReduceCreditLine owns the limit and revocation,
// SetCreditInterestIncome owns the income account,
// AccrueCreditInterest owns the charges, and this module owns only
// the interest-rate revision.

// AmendCreditLineRate sets a granted credit line's annual interest
// rate to newRateBps in place. The new rate must lie in 1..10000
// basis points; a failed amendment leaves the line exactly as it
// was. Amending is off-journal — it does not bump the ledger
// version — but it is audited as credit_line_rate_amended.
func (l *Ledger) AmendCreditLineRate(account AccountID, currency string, newRateBps int64) (CreditLine, error) {
	return l.AmendCreditLineRateCtx(context.Background(), account, currency, newRateBps)
}

// AmendCreditLineRateCtx is AmendCreditLineRate with per-call audit
// actor attribution (LG-73).
func (l *Ledger) AmendCreditLineRateCtx(ctx context.Context, account AccountID, currency string, newRateBps int64) (CreditLine, error) {
	if account == "" {
		return CreditLine{}, ErrEmptyCreditLineAccount
	}
	cur, err := normalizeCurrency(currency)
	if err != nil {
		return CreditLine{}, err
	}
	if newRateBps < 1 || newRateBps > 10000 {
		return CreditLine{}, ErrInvalidCreditLineRate
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	k := creditLineKey{account: account, currency: cur}
	cl, exists := l.creditLines[k]
	if !exists {
		return CreditLine{}, ErrCreditLineNotFound
	}
	previous := cl.RateBps
	cl.RateBps = newRateBps
	l.creditLines[k] = cl
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "credit_line_rate_amended",
		Actor:         "AmendCreditLineRate",
		TraceID:       string(account) + "/" + cur,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{account},
		Details: map[string]any{
			"account":           string(account),
			"currency":          cur,
			"previous_rate_bps": previous,
			"rate_bps":          newRateBps,
			"limit_cents":       cl.LimitCents,
		},
	})
	return cl, nil
}
