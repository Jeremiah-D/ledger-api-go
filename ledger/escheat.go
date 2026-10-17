package ledger

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"
)

// Escheatment: unclaimed-property sweep of long-dormant accounts.
//
// Dormancy (see dormancy.go) marks quiet accounts but never moves their
// money. Escheatment is the compliance end of that lifecycle: an
// account that was marked dormant and has stayed inactive past the
// operator's escheat threshold has its positive balances swept to the
// configured escheat (unclaimed-property / regulatory) account, and is
// then frozen so no further writes can land on it. This mirrors
// real-world unclaimed-property law: after a dormancy period the holder
// must remit the funds to the state, not keep them.
//
// EscheatDormant is a batch job: it scans the dormant-account set (so
// DetectDormant must have run first — unmarked accounts are never
// swept), and for every dormant account whose last journal activity is
// strictly older than thresholdDays days it books one journal entry per
// (account, currency) with a positive balance: credit the dormant
// account, debit the escheat account, for the full balance. The entry
// ID is deterministic — "escheat/<account>/<currency>/<YYYY-MM-DD>"
// (UTC date) — so the idempotency key is (account, currency, day).
//
// Each swept account is frozen (see freeze.go) right after its entries
// book: frozen is the reversible risk-control stop, so a later operator
// review can unfreeze. The freeze is what makes the sweep stick —
// without it a fresh posting could land on the zeroed account. A
// successful escheat implies the freeze below was applied by this call:
// the escheat entry runs the frozen gate, so an already-frozen account
// is rejected before any entry books, and ReverseEscheatment can
// therefore restore the exact pre-escheat state by unfreezing.
//
// The sweep posts through the system path (postSystemLocked): the full
// gate order runs (validation, denylist, idempotency replay, frozen,
// overdraft, daily-limit, period), journal rules never fire, and the
// dormant-account stop is skipped — the sweep exists to move dormant
// accounts' money. A reversal of the sweep counts as journal activity,
// so a reversed account is not re-swept until it goes quiet for another
// full threshold.
//
// A mistaken sweep is undone with ReverseEscheatment, which books the
// mirror entry ("<escheat ID>/reversal": debit the escheat account,
// credit the account) and unfreezes the account. The original entry is
// never deleted — the audit chain stays append-only. The account keeps
// its dormant mark: the money is back, but the account is still quiet,
// so operator postings stay rejected until ReactivateAccount.
//
// Escheated balances are disclosed on the Reconcile report (see
// ReconciliationReport.Escheats), recovered from the journaled rows —
// the journal is the truth, so the disclosure can never disagree with
// the books.

var (
	// ErrInvalidEscheatThreshold is returned by EscheatDormant when
	// thresholdDays is not positive.
	ErrInvalidEscheatThreshold = errors.New("ledger: escheat inactivity threshold must be positive")
	// ErrInvalidEscheatAccount is returned by EscheatDormant when the
	// escheat account is empty or contains '/'.
	ErrInvalidEscheatAccount = errors.New("ledger: escheat account must be non-empty and must not contain '/'")
	// ErrEscheatNotFound is returned by ReverseEscheatment when the
	// named entry does not exist or is not an escheat entry.
	ErrEscheatNotFound = errors.New("ledger: escheat entry not found")
)

// EscheatRecord is one swept (account, currency) balance: the amount
// moved, the journal entry that moved it, and whether the sweep was
// later reversed.
type EscheatRecord struct {
	Account     AccountID `json:"account"`
	Currency    string    `json:"currency"`
	AmountCents int64     `json:"amount_cents"`
	EntryID     string    `json:"entry_id"`
	EscheatedAt time.Time `json:"escheated_at"`
	Reversed    bool      `json:"reversed"`
}

// EscheatSkip is one (account, currency) the sweep considered but did
// not move, with the reason: a batch compliance job must not silently
// drop accounts, so every non-swept positive balance is reported.
type EscheatSkip struct {
	Account  AccountID `json:"account"`
	Currency string    `json:"currency"`
	Reason   string    `json:"reason"`
}

// EscheatReport is EscheatDormant's result: the sweep parameters, the
// balances swept (sorted by account, then currency), and the balances
// skipped with reasons.
type EscheatReport struct {
	CheckedAt      time.Time       `json:"checked_at"`
	ThresholdDays  int             `json:"threshold_days"`
	EscheatAccount AccountID       `json:"escheat_account"`
	Escheated      []EscheatRecord `json:"escheated"`
	Skipped        []EscheatSkip   `json:"skipped"`
}

// escheatEntryID builds the deterministic escheat entry ID. The UTC
// calendar date makes one day's sweep idempotent while a new day gets a
// fresh ID.
func escheatEntryID(account AccountID, currency string, asof time.Time) string {
	return "escheat/" + string(account) + "/" + currency + "/" + asof.UTC().Format("2006-01-02")
}

// EscheatDormant sweeps the positive balances of long-dormant accounts
// to the escheat account and freezes the swept accounts. thresholdDays
// is the inactivity threshold in days: only accounts marked dormant
// (see DetectDormant) whose last journal activity is strictly older
// than thresholdDays days are swept. escheatAccount is the
// unclaimed-property / regulatory account receiving the funds.
//
// The sweep is best-effort per (account, currency): a positive balance
// that cannot be swept (frozen account, gate rejection) is reported in
// Skipped with the reason instead of failing the whole run. Zero and
// negative balances are out of scope — only "余额>0" is swept — and are
// not reported. Each booked entry is a full journal row (audit-chain
// link, version bump) with the memo "escheat", and each swept account
// is frozen (audited as "freeze") plus reported with an "escheat"
// audit event.
func (l *Ledger) EscheatDormant(thresholdDays int, escheatAccount AccountID) (EscheatReport, error) {
	if thresholdDays <= 0 {
		return EscheatReport{}, ErrInvalidEscheatThreshold
	}
	if escheatAccount == "" || strings.Contains(string(escheatAccount), "/") {
		return EscheatReport{}, ErrInvalidEscheatAccount
	}
	now := time.Now().UTC()
	// AddDate instead of a duration subtraction: a huge thresholdDays
	// would overflow a time.Duration, while AddDate does calendar
	// arithmetic safely (DetectDormant's convention).
	cutoff := now.AddDate(0, 0, -thresholdDays)

	l.mu.Lock()
	defer l.mu.Unlock()
	report := EscheatReport{
		CheckedAt:      now,
		ThresholdDays:  thresholdDays,
		EscheatAccount: escheatAccount,
		Escheated:      []EscheatRecord{},
		Skipped:        []EscheatSkip{},
	}
	// Sorted account order for a deterministic report.
	accounts := make([]AccountID, 0, len(l.dormant))
	for a := range l.dormant {
		accounts = append(accounts, a)
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i] < accounts[j] })
	for _, a := range accounts {
		rec := l.dormant[a]
		if !rec.LastActivity.Before(cutoff) {
			continue
		}
		if a == escheatAccount {
			// The escheat account itself can never be swept into
			// itself; report every positive balance as skipped so
			// the operator sees the config edge.
			for _, bal := range l.positiveBalancesLocked(a) {
				report.Skipped = append(report.Skipped, EscheatSkip{
					Account: a, Currency: bal.currency, Reason: "escheat_account_itself",
				})
			}
			continue
		}
		// Sweep every positive (account, currency) balance before
		// freezing: the freeze would reject the later currencies'
		// entries through the frozen gate.
		sweptAny := false
		for _, bal := range l.positiveBalancesLocked(a) {
			id := escheatEntryID(a, bal.currency, now)
			e := JournalEntry{
				ID:             id,
				DebitAccount:   escheatAccount,
				CreditAccount:  a,
				AmountCents:    bal.cents,
				Currency:       bal.currency,
				IdempotencyKey: id,
				Memo:           "escheat",
				CreatedAt:      now,
			}
			posted, dup, err := l.postSystemLocked(e, false)
			if err != nil {
				report.Skipped = append(report.Skipped, EscheatSkip{
					Account: a, Currency: bal.currency, Reason: err.Error(),
				})
				continue
			}
			sweptAny = true
			if dup {
				continue
			}
			l.emitAudit(AuditEvent{
				Op:            "escheat",
				Actor:         "EscheatDormant",
				TraceID:       string(a),
				VersionBefore: l.version,
				VersionAfter:  l.version,
				EntryIDs:      []string{posted.ID},
				Accounts:      []AccountID{a, escheatAccount},
				Details: map[string]any{
					"currency":        bal.currency,
					"amount_cents":    bal.cents,
					"entry_id":        posted.ID,
					"duplicate":       dup,
					"threshold_days":  thresholdDays,
					"last_activity":   rec.LastActivity.Format(time.RFC3339),
				},
			})
			report.Escheated = append(report.Escheated, EscheatRecord{
				Account:     a,
				Currency:    bal.currency,
				AmountCents: bal.cents,
				EntryID:     posted.ID,
				EscheatedAt: now,
				Reversed:    false,
			})
		}
		// Freeze the swept account so no further writes land on it.
		// The freeze is idempotent and runs after all of the
		// account's currencies are swept, so the frozen gate never
		// blocks a later currency in the same run.
		if sweptAny && !l.frozenLocked(a) {
			l.freezeCtxLocked(context.Background(), a, time.Time{})
		}
	}
	sort.Slice(report.Escheated, func(i, j int) bool {
		if report.Escheated[i].Account != report.Escheated[j].Account {
			return report.Escheated[i].Account < report.Escheated[j].Account
		}
		return report.Escheated[i].Currency < report.Escheated[j].Currency
	})
	sort.Slice(report.Skipped, func(i, j int) bool {
		if report.Skipped[i].Account != report.Skipped[j].Account {
			return report.Skipped[i].Account < report.Skipped[j].Account
		}
		return report.Skipped[i].Currency < report.Skipped[j].Currency
	})
	return report, nil
}

// positiveBalance is one (currency, cents) pair with a positive
// balance.
type positiveBalance struct {
	currency string
	cents    int64
}

// positiveBalancesLocked returns every currency in which the account
// holds a positive balance, sorted by currency. Callers must hold l.mu.
func (l *Ledger) positiveBalancesLocked(a AccountID) []positiveBalance {
	var out []positiveBalance
	for ac, bal := range l.balances {
		if ac.account == a && bal > 0 {
			out = append(out, positiveBalance{currency: ac.currency, cents: bal})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].currency < out[j].currency })
	return out
}

// ReverseEscheatment reverses one escheat sweep: it books the mirror
// entry — debit the account, credit the escheat account — for the
// original amount, under the deterministic ID "<escheat ID>/reversal",
// and unfreezes the account, restoring the exact pre-escheat state
// (the sweep's freeze was applied by EscheatDormant itself: an
// already-frozen account is rejected by the escheat entry's frozen
// gate, so a booked escheat implies this call's freeze).
//
// One escheat admits exactly one reversal: reversing twice returns the
// existing reversal with duplicate == true. asof sets the reversal's
// timestamp; a zero asof means now. The original entry is never
// deleted — the audit chain stays append-only — and the account keeps
// its dormant mark: the money is back, but the account is still quiet,
// so operator postings stay rejected until ReactivateAccount.
//
// The unfreeze happens before the mirror entry posts (the frozen gate
// would otherwise reject the post). If the post then fails, the account
// stays unfrozen — the unfreeze is an audited, reversible structural
// change, and the deterministic reversal ID makes the retry safe.
func (l *Ledger) ReverseEscheatment(escheatID string, asof time.Time) (JournalEntry, bool, error) {
	if !strings.HasPrefix(escheatID, "escheat/") || strings.HasSuffix(escheatID, "/reversal") {
		return JournalEntry{}, false, ErrEscheatNotFound
	}
	if asof.IsZero() {
		asof = time.Now()
	}
	revID := escheatID + "/reversal"

	l.mu.Lock()
	defer l.mu.Unlock()
	orig, ok := l.entries[escheatID]
	if !ok || orig.Memo != "escheat" {
		return JournalEntry{}, false, ErrEscheatNotFound
	}
	if rev, ok := l.entries[revID]; ok {
		return rev, true, nil
	}
	// The swept account is the credit leg (it was credited to zero it).
	account := orig.CreditAccount
	// Unfreeze first: the frozen gate would reject the mirror entry
	// otherwise. Mirrors UnfreezeCtx's body (which takes the lock
	// itself, so it cannot be called here).
	if _, frozen := l.frozen[account]; frozen {
		delete(l.frozen, account)
		delete(l.frozenUntil, account)
		l.emitAudit(AuditEvent{
			Op:            "unfreeze",
			Actor:         "ReverseEscheatment",
			TraceID:       string(account),
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Accounts:      []AccountID{account},
		})
		l.emitOutboxLocked(OutboxEvent{
			Type:    OutboxAccountUnfrozen,
			Account: account,
		})
		l.recordFreezeEventLocked(AccountFreezeEvent{
			Account: account,
			Kind:    FreezeEventUnfrozen,
			At:      time.Now().UTC(),
		})
	}
	e := JournalEntry{
		ID:             revID,
		DebitAccount:   orig.CreditAccount,
		CreditAccount:  orig.DebitAccount,
		AmountCents:    orig.AmountCents,
		Currency:       orig.Currency,
		IdempotencyKey: revID,
		Memo:           "escheat:reversal",
		CreatedAt:      asof,
	}
	posted, dup, err := l.postSystemLocked(e, false)
	if err != nil {
		return JournalEntry{}, false, err
	}
	l.emitAudit(AuditEvent{
		Op:            "escheat_reversed",
		Actor:         "ReverseEscheatment",
		TraceID:       string(account),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		EntryIDs:      []string{posted.ID},
		Accounts:      []AccountID{account, orig.DebitAccount},
		Details: map[string]any{
			"currency":     orig.Currency,
			"amount_cents": orig.AmountCents,
			"entry_id":     posted.ID,
			"escheat_id":   escheatID,
		},
	})
	return posted, dup, nil
}

// escheatsLocked recovers every escheat sweep from the journaled rows:
// the journal is the truth, so the Reconcile disclosure can never
// disagree with the books. Callers must hold l.mu; the read lock
// suffices.
func (l *Ledger) escheatsLocked() []EscheatRecord {
	out := []EscheatRecord{}
	for id, e := range l.entries {
		if !strings.HasPrefix(id, "escheat/") || strings.HasSuffix(id, "/reversal") {
			continue
		}
		// The remainder is "<account>/<currency>/<YYYY-MM-DD>":
		// account IDs cannot contain '/', the currency is 3 letters,
		// and the date is fixed-width — anything else is not an
		// escheat row, defensive since the ID scheme is the only
		// writer.
		rest := strings.TrimPrefix(id, "escheat/")
		parts := strings.Split(rest, "/")
		if len(parts) != 3 || len(parts[2]) != 10 {
			continue
		}
		_, reversed := l.entries[id+"/reversal"]
		out = append(out, EscheatRecord{
			Account:     e.CreditAccount,
			Currency:    e.Currency,
			AmountCents: e.AmountCents,
			EntryID:     id,
			EscheatedAt: e.CreatedAt,
			Reversed:    reversed,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Account != out[j].Account {
			return out[i].Account < out[j].Account
		}
		if out[i].Currency != out[j].Currency {
			return out[i].Currency < out[j].Currency
		}
		return out[i].EntryID < out[j].EntryID
	})
	return out
}
