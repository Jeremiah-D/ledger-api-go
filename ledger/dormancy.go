package ledger

import (
	"errors"
	"sort"
	"strings"
	"time"
)

// Dormancy: inactive-account lifecycle marking (LG-88).
//
// A dormant account is one with no journal activity for longer than the
// operator's inactivity threshold. Dormancy is a state marking, not fund
// control — it is orthogonal to the freeze (see freeze.go): freezing is
// a reversible risk-control stop on fund movement, dormancy marks that
// the account itself has gone quiet. Marking an account dormant does not
// freeze its funds and does not touch balances, totals, the ledger
// version, or the audit chain; it is structural config, like the
// frozen-account list, and survives snapshots.
//
// A dormant account rejects operator Post and Transfer bookings with
// ErrAccountDormant until ReactivateAccount clears the mark. The gate
// sits in the operator posting paths only: system adjusting entries
// (accruals, dividends, dormancy fees, revaluations, reversals, closing
// entries) keep posting through dormant accounts, because freezing a
// dormant account's own inactivity fee would brick the feature, and
// month-end bookkeeping must not stall on quiet accounts.
//
// DetectDormant scans journal activity and marks the quiet accounts,
// reporting what it marked and auditing each marking. ReactivateAccount
// clears the mark (audited). ChargeDormancyFee books the opt-in monthly
// inactivity fee as an accrual-convention adjusting entry (see
// accrual.go): deterministic monthly ID, cumulative cap, single
// currency. A dormancy fee is reversible through the generic
// ReverseEntry path — no special-cased reversal is needed.

// ErrAccountDormant is returned by the operator Post and Transfer paths
// when either leg's account is marked dormant. Like the frozen and
// closed stops it runs after the idempotency replay check: a replay of
// a key posted before the account went dormant returns the original
// entry instead of failing, because the replay books nothing new.
// Reads are unaffected by design, and ReactivateAccount lifts the stop.
var ErrAccountDormant = errors.New("ledger: account is dormant")

// ErrInvalidInactivityDays is returned by DetectDormant when the
// inactivity threshold is not positive.
var ErrInvalidInactivityDays = errors.New("ledger: inactivity days must be positive")

// DormancyRecord is one account's dormancy marking: the account, when
// it was marked, and its last journal activity at marking time. It is
// structural config — marking never bumps the journal version — and it
// round-trips through snapshots verbatim.
type DormancyRecord struct {
	Account      AccountID `json:"account"`
	MarkedAt     time.Time `json:"marked_at"`
	LastActivity time.Time `json:"last_activity"`
}

// DormancyReport is DetectDormant's result: the scan parameters and the
// accounts newly marked dormant by this run, sorted by account. Accounts
// already dormant stay marked without re-auditing; accounts with recent
// activity are absent.
type DormancyReport struct {
	CheckedAt      time.Time        `json:"checked_at"`
	InactivityDays int              `json:"inactivity_days"`
	Marked         []DormancyRecord `json:"marked"`
}

// dormantLocked reports whether the account is marked dormant. Callers
// must hold l.mu (either lock suffices; write paths hold the write
// lock).
func (l *Ledger) dormantLocked(a AccountID) bool {
	_, ok := l.dormant[a]
	return ok
}

// IsDormant reports whether the account is marked dormant.
func (l *Ledger) IsDormant(a AccountID) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.dormantLocked(a)
}

// dormantCopyLocked returns the dormancy records sorted by account, for
// snapshots (see ExportSnapshot). Callers must hold l.mu.
func (l *Ledger) dormantCopyLocked() []DormancyRecord {
	out := make([]DormancyRecord, 0, len(l.dormant))
	for _, r := range l.dormant {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Account < out[j].Account })
	return out
}

// lastActivityLocked returns the latest journal timestamp touching the
// account (as debit or credit leg), and whether the account has any
// journal activity at all. Accounts that never appear in the journal
// have nothing to be inactive against, so they are never marked
// dormant. Callers must hold l.mu.
func (l *Ledger) lastActivityLocked(a AccountID) (time.Time, bool) {
	ids, ok := l.byAccount[a]
	if !ok || len(ids) == 0 {
		return time.Time{}, false
	}
	var last time.Time
	for _, id := range ids {
		if e, ok := l.entries[id]; ok && e.CreatedAt.After(last) {
			last = e.CreatedAt
		}
	}
	return last, true
}

// DetectDormant scans every account with journal activity and marks the
// ones whose latest activity is strictly older than inactivityDays days
// ago as dormant. Marking is idempotent: already-dormant accounts are
// left alone (no re-audit), and the returned report lists only the
// accounts newly marked by this run.
//
// Marking writes structural state but books nothing: balances, totals,
// the ledger version, and the audit chain are untouched — the "audit
// event" is the marking record itself, one dormant_marked event per
// account naming the account, its last activity, and the threshold. It
// does not freeze funds: a dormant account's money stays where it is,
// and reads (Balance, Snapshot, TrialBalance, Reconcile) are unaffected.
func (l *Ledger) DetectDormant(inactivityDays int) (DormancyReport, error) {
	if inactivityDays <= 0 {
		return DormancyReport{}, ErrInvalidInactivityDays
	}
	now := time.Now().UTC()
	// AddDate instead of a duration subtraction: a huge inactivityDays
	// would overflow a time.Duration, while AddDate does calendar
	// arithmetic safely.
	cutoff := now.AddDate(0, 0, -inactivityDays)

	l.mu.Lock()
	defer l.mu.Unlock()
	report := DormancyReport{
		CheckedAt:      now,
		InactivityDays: inactivityDays,
		Marked:         []DormancyRecord{},
	}
	for a := range l.byAccount {
		if l.dormantLocked(a) {
			continue
		}
		last, ok := l.lastActivityLocked(a)
		if !ok || !last.Before(cutoff) {
			continue
		}
		rec := DormancyRecord{Account: a, MarkedAt: now, LastActivity: last}
		l.dormant[a] = rec
		report.Marked = append(report.Marked, rec)
		l.emitAudit(AuditEvent{
			Op:            "dormant_marked",
			Actor:         "DetectDormant",
			TraceID:       string(a),
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Accounts:      []AccountID{a},
			Details: map[string]any{
				"marked_at":       now.Format(time.RFC3339),
				"last_activity":   last.Format(time.RFC3339),
				"inactivity_days": inactivityDays,
			},
		})
	}
	sort.Slice(report.Marked, func(i, j int) bool { return report.Marked[i].Account < report.Marked[j].Account })
	return report, nil
}

// ReactivateAccount clears an account's dormancy mark, lifting the
// ErrAccountDormant stop on operator Post and Transfer bookings. The
// clearing is audited as dormant_reactivated. Reactivating an account
// that is not dormant is a no-op returning wasDormant == false; unlike
// CloseAccount, dormancy is reversible by design.
func (l *Ledger) ReactivateAccount(a AccountID) (cleared DormancyRecord, wasDormant bool, err error) {
	if a == "" {
		return DormancyRecord{}, false, ErrEmptyAccountID
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	rec, ok := l.dormant[a]
	if !ok {
		return DormancyRecord{}, false, nil
	}
	delete(l.dormant, a)
	l.emitAudit(AuditEvent{
		Op:            "dormant_reactivated",
		Actor:         "ReactivateAccount",
		TraceID:       string(a),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{a},
		Details: map[string]any{
			"marked_at":     rec.MarkedAt.Format(time.RFC3339),
			"last_activity": rec.LastActivity.Format(time.RFC3339),
		},
	})
	return rec, true, nil
}

// Dormancy fee: the opt-in monthly inactivity charge (accrual
// convention, see accrual.go).
//
// ChargeDormancyFee books one month's inactivity fee: debit the account,
// credit the payable (fee-income) account, for amountCents. The entry ID
// is deterministic — "dormant-fee/<account>/<YYYY-MM>" (UTC month) — so
// charging the same month twice is idempotent: the second call returns
// the original entry with duplicate == true and books nothing new.
//
// The fee is an ordinary journal entry: it carries its own audit-chain
// link, bumps the ledger version, and flows through
// VerifyAccountingEquation and Reconcile like a hand-posted entry. It
// runs the full Post gate order (validation, idempotency replay, chart
// direction, frozen, overdraft, daily-limit, budget, period) but never
// fires journal rules and never enters dual-control review (see
// accrual.go for why). The one deliberate exception to the gate order is
// the dormant-account stop itself: the fee charges dormant accounts by
// design, so it posts through the system path that skips that gate. A
// dormancy fee is single-currency by design (the default currency), like
// every accrual.
//
// capCents caps the cumulative net fee for the (account, payable) pair:
// when the already-charged total plus this run would exceed it, nothing
// is booked and ErrDormancyFeeCapExceeded is returned. Zero disables
// capping. A fee is reversible through the generic ReverseEntry path.
var (
	// ErrDormancyFeeCapExceeded is returned when charging the fee would
	// take the cumulative net dormancy fee for the (account, payable)
	// pair above capCents.
	ErrDormancyFeeCapExceeded = errors.New("ledger: dormancy fee would exceed the cumulative fee cap")
	// ErrInvalidDormancyFeeCap is returned when capCents is negative: a
	// cap of zero disables capping.
	ErrInvalidDormancyFeeCap = errors.New("ledger: dormancy fee cap must not be negative")
)

// dormancyFeeEntryID builds the deterministic dormancy-fee entry ID.
// The UTC calendar month makes one month's fee idempotent while a new
// month gets a fresh ID.
func dormancyFeeEntryID(account, payable AccountID, asof time.Time) string {
	return "dormant-fee/" + string(account) + "/" + string(payable) + "/" + asof.UTC().Format("2006-01")
}

// dormancyFeeEntryPrefix is the ID prefix shared by every dormancy fee
// of one (account, payable) pair, used by DormancyFeeTotal to find them.
func dormancyFeeEntryPrefix(account, payable AccountID) string {
	return "dormant-fee/" + string(account) + "/" + string(payable) + "/"
}

// ChargeDormancyFee books one month's inactivity fee for the account.
// asof sets both the entry's timestamp and its calendar month (UTC); a
// zero asof means now. The call is idempotent per calendar month and
// runs the full Post gate order except the dormant-account stop, which
// it deliberately skips: the fee exists to charge dormant accounts.
func (l *Ledger) ChargeDormancyFee(account, payable AccountID, amountCents int64, asof time.Time, capCents int64) (JournalEntry, bool, error) {
	if amountCents <= 0 {
		return JournalEntry{}, false, ErrNonPositiveAmount
	}
	// Reuse the accrual account validation: it rejects empty accounts,
	// indistinguishable legs, and '/' in account IDs (which would break
	// the deterministic "dormant-fee/" ID namespace).
	if err := checkAccrualAccounts(account, payable); err != nil {
		return JournalEntry{}, false, err
	}
	if capCents < 0 {
		return JournalEntry{}, false, ErrInvalidDormancyFeeCap
	}
	if asof.IsZero() {
		asof = time.Now()
	}
	id := dormancyFeeEntryID(account, payable, asof)
	e := JournalEntry{
		ID:             id,
		DebitAccount:   account,
		CreditAccount:  payable,
		AmountCents:    amountCents,
		IdempotencyKey: id,
		Memo:           "dormant-fee",
		CreatedAt:      asof,
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if capCents > 0 {
		total, err := l.dormancyFeeTotalLocked(account, payable, time.Time{}, asof)
		if err != nil {
			return JournalEntry{}, false, err
		}
		// Overflow-safe, like the accrual cap: a sum that does not fit
		// int64 necessarily exceeds any int64 cap.
		sum, ok := addCents(total, amountCents)
		if !ok || sum > capCents {
			return JournalEntry{}, false, ErrDormancyFeeCapExceeded
		}
	}
	// System path: the dormant-account stop is skipped (the fee charges
	// dormant accounts by design); every other gate still applies, and
	// journal rules never fire (accrual convention).
	return l.postSystemLocked(e, false)
}

// DormancyFeeTotal returns the cumulative net dormancy fee for one
// (account, payable) pair over [since, until): the sum of every fee
// entry in the window minus the reversals of those fees. A zero since
// includes all history. The sum is overflow-checked.
func (l *Ledger) DormancyFeeTotal(account, payable AccountID, since, until time.Time) (int64, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.dormancyFeeTotalLocked(account, payable, since, until)
}

// dormancyFeeTotalLocked is DormancyFeeTotal's body. Callers must hold
// l.mu; the read lock suffices because nothing is mutated.
func (l *Ledger) dormancyFeeTotalLocked(account, payable AccountID, since, until time.Time) (int64, error) {
	prefix := dormancyFeeEntryPrefix(account, payable)
	var total int64
	for id, e := range l.entries {
		if !strings.HasPrefix(id, prefix) {
			continue
		}
		// The remainder after the prefix is the UTC month
		// ("2006-01", 7 chars); anything else is not a dormancy-fee
		// row of this pair — defensive, since the ID scheme is the
		// only writer.
		if len(id) != len(prefix)+7 {
			continue
		}
		if e.CreatedAt.Before(since) || !e.CreatedAt.Before(until) {
			continue
		}
		var ok bool
		total, ok = addCents(total, e.AmountCents)
		if !ok {
			return 0, errors.New("ledger: dormancy fee total overflows int64")
		}
		// A reversal of this fee nets it out of the total, mirroring
		// the accrual convention.
		if rev, ok := l.entries[id+"/reversal"]; ok {
			total, ok = addCents(total, -rev.AmountCents)
			if !ok {
				return 0, errors.New("ledger: dormancy fee total overflows int64")
			}
		}
	}
	return total, nil
}
