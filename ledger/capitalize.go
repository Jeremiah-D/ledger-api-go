package ledger

import (
	"errors"
	"strings"
	"time"
)

// Interest capitalization: turning accrued interest into principal.
//
// AccrueInterest books daily adjusting entries — debit interest expense,
// credit interest payable — but the accrued amounts sit in the payable
// account until something happens to them. CapitalizeInterest is that
// something: it moves the outstanding accrued interest for one
// (interest, expense, payable) triple into the principal account —
// debit the payable, credit the principal — which is the compound-
// interest step: the next accrual round prices off the larger principal
// (the caller passes the new, larger principal to AccrueInterest).
//
// The capitalization is an ordinary journal entry: it carries its own
// audit-chain link (the hash covers every journaled field), it bumps
// the ledger version, and it flows through VerifyAccountingEquation
// and Reconcile exactly like a hand-posted entry. It runs the full Post
// gate order — field validation, idempotency replay, chart direction,
// frozen, overdraft, daily-limit, period — with the same deliberate
// exceptions accruals take (see accrual.go): it never fires the journal
// rules engine (system adjusting entries must not cascade derived legs)
// and never enters dual-control review. Unlike the daily accruals,
// capitalization is NOT subject to the business-day gate (see
// accrualBusinessDayGateLocked): it is a period-end adjustment the
// operator runs on their own schedule, not a daily accrual.
//
// Idempotency: the entry ID is deterministic —
// "accrual-capitalization/interest/<expense>/<payable>/<principal>/<YYYY-MM-DD>"
// (UTC date) — so re-running the same day's capitalization returns the
// original entry with duplicate == true and books nothing new. A new
// calendar day gets a fresh ID priced off the then-outstanding total.
//
// After capitalization, AccruedTotal for the triple is zero: the
// outstanding total nets out capitalized amounts (see
// accruedTotalLocked). Reversing a capitalization (ReverseAccrual
// accepts capitalization IDs — they share the "accrual/" prefix — and
// books the mirror entry) restores the payable balance and the total,
// exactly like reversing an accrual. Capitalizing a triple with zero
// outstanding accrual is rejected with ErrNothingToCapitalize rather
// than booking a zero entry.

// Validation errors for interest capitalization.
var (
	// ErrNothingToCapitalize is returned by CapitalizeInterest when
	// the net outstanding accrued interest for the triple is zero:
	// there is nothing to move into principal, so no entry is booked.
	ErrNothingToCapitalize = errors.New("ledger: no outstanding accrued interest to capitalize")
	// ErrInvalidCapitalizationAccount is returned when the principal
	// account is empty, indistinguishable from the expense or payable
	// account, or contains '/': the deterministic capitalization entry
	// ID joins its components with '/', so a '/' inside an account
	// would corrupt the ID scheme.
	ErrInvalidCapitalizationAccount = errors.New("ledger: invalid capitalization principal account")
)

// capitalizationEntryID builds the deterministic capitalization entry
// ID. The UTC calendar date makes one day's capitalization idempotent
// while a new day gets a fresh ID priced off the then-outstanding
// accrued total.
func capitalizationEntryID(expense, payable, principal AccountID, asof time.Time) string {
	return "accrual-capitalization/" + AccrualKindInterest + "/" + string(expense) +
		"/" + string(payable) + "/" + string(principal) + "/" + asof.UTC().Format("2006-01-02")
}

// parseCapitalizationID splits a capitalization entry ID back into its
// components. Account IDs never contain '/', so the three account
// segments between the kind prefix and the date suffix are unambiguous.
// A reversal row ("<id>/reversal") fails the date-suffix shape check
// and is never mistaken for a capitalization.
func parseCapitalizationID(id string) (expense, payable, principal AccountID, ok bool) {
	const prefix = "accrual-capitalization/" + AccrualKindInterest + "/"
	rest, found := strings.CutPrefix(id, prefix)
	if !found || len(rest) < 12 {
		return "", "", "", false
	}
	// The last 11 bytes must be "/YYYY-MM-DD".
	tail := rest[len(rest)-11:]
	if tail[0] != '/' || !validCapitalizationDate(tail[1:]) {
		return "", "", "", false
	}
	segs := strings.Split(rest[:len(rest)-11], "/")
	if len(segs) != 3 {
		return "", "", "", false
	}
	return AccountID(segs[0]), AccountID(segs[1]), AccountID(segs[2]), true
}

// validCapitalizationDate reports whether s is shaped like "2006-01-02".
// It is a shape check, not a calendar check: the only writer is
// capitalizationEntryID, so anything date-shaped here is one of ours.
func validCapitalizationDate(s string) bool {
	if len(s) != 10 || s[4] != '-' || s[7] != '-' {
		return false
	}
	for i := 0; i < 10; i++ {
		if i == 4 || i == 7 {
			continue
		}
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// checkCapitalizationAccounts validates the three accounts of a
// capitalization: the expense/payable pair follows the accrual account
// rules, and the principal must be a distinct, slash-free account.
func checkCapitalizationAccounts(expense, payable, principal AccountID) error {
	if err := checkAccrualAccounts(expense, payable); err != nil {
		return err
	}
	if principal == "" || principal == expense || principal == payable {
		return ErrInvalidCapitalizationAccount
	}
	if strings.Contains(string(principal), "/") {
		return ErrInvalidCapitalizationAccount
	}
	return nil
}

// accrualTripleCurrencyLocked returns the currency of the (kind,
// expense, payable) triple's accrual rows. Accruals are single-currency
// by design (see accrual.go), so the first row's currency is the
// triple's. Callers must hold l.mu; the read lock suffices. Callers
// only call this when the triple has outstanding accruals, so a row
// always exists.
func (l *Ledger) accrualTripleCurrencyLocked(kind string, expense, payable AccountID) string {
	prefix := accrualEntryPrefix(kind, expense, payable)
	for id, e := range l.entries {
		if strings.HasPrefix(id, prefix) {
			return e.Currency
		}
	}
	return ""
}

// CapitalizeInterest moves the outstanding accrued interest for the
// (interest, expense, payable) triple into the principal account:
// debit the interest payable, credit the principal, for the net
// accrued total over (-inf, asof) — accruals minus reversals minus
// earlier capitalizations. asof sets both the entry's timestamp and
// its calendar day (UTC); a zero asof means now.
//
// The call is idempotent per calendar day: re-running the same day
// returns the original entry with duplicate == true. Capitalizing a
// triple with zero outstanding accrual returns ErrNothingToCapitalize
// and books nothing. The entry runs the full Post gate order (frozen,
// overdraft, daily-limit, closed period) but never fires journal rules
// and never enters dual-control review, like every accrual; it is not
// subject to the business-day gate — capitalization is a period-end
// adjustment, not a daily accrual.
func (l *Ledger) CapitalizeInterest(expense, payable, principal AccountID, asof time.Time) (JournalEntry, bool, error) {
	if err := checkCapitalizationAccounts(expense, payable, principal); err != nil {
		return JournalEntry{}, false, err
	}
	if asof.IsZero() {
		asof = time.Now()
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	total, err := l.accruedTotalLocked(expense, payable, AccrualKindInterest, time.Time{}, asof)
	if err != nil {
		return JournalEntry{}, false, err
	}
	if total <= 0 {
		return JournalEntry{}, false, ErrNothingToCapitalize
	}
	id := capitalizationEntryID(expense, payable, principal, asof)
	e := JournalEntry{
		ID:             id,
		DebitAccount:   payable,
		CreditAccount:  principal,
		AmountCents:    total,
		Currency:       l.accrualTripleCurrencyLocked(AccrualKindInterest, expense, payable),
		IdempotencyKey: id,
		Memo:           "accrual-capitalization:" + AccrualKindInterest,
		CreatedAt:      asof,
	}
	// fireRules=false: capitalizations are system adjusting entries
	// and must not cascade through operator-configured journal rules
	// (see the accrual.go package doc comment). The dormant-account
	// stop does not apply — the posting path is the system one, like
	// accruals.
	versionBefore := l.version
	posted, dup, err := l.postSystemLocked(e, false)
	if err != nil {
		return JournalEntry{}, false, err
	}
	if !dup {
		l.emitAudit(AuditEvent{
			Op:            "interest_capitalized",
			Actor:         "CapitalizeInterest",
			TraceID:       id,
			VersionBefore: versionBefore,
			VersionAfter:  l.version,
			Accounts:      []AccountID{expense, payable, principal},
			Details: map[string]any{
				"kind":         AccrualKindInterest,
				"amount_cents": total,
				"currency":     posted.Currency,
			},
		})
	}
	return posted, dup, nil
}
