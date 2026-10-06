// Package ledger implements a minimal in-memory double-entry ledger with
// idempotent posting.
//
// A journal entry always touches exactly two accounts: the debit account is
// increased by the entry amount and the credit account is decreased by the
// same amount, so the sum of all balances is invariantly zero. Amounts are
// stored as integer cents; floating point is never used for money.
//
// Posting is idempotent: when a JournalEntry carries an IdempotencyKey that
// was already posted, the original entry is returned and no second booking
// occurs.
package ledger

import (
	"encoding/base64"
	"errors"
	"sort"
	"sync"
	"time"
)

// AccountID identifies an account in the ledger.
type AccountID string

// JournalEntry is a single double-entry journal posting.
type JournalEntry struct {
	ID             string    `json:"id"`
	DebitAccount   AccountID `json:"debit_account"`
	CreditAccount  AccountID `json:"credit_account"`
	AmountCents    int64     `json:"amount_cents"`
	IdempotencyKey string    `json:"idempotency_key,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// Validation errors returned by Post.
var (
	ErrEmptyID            = errors.New("ledger: journal entry ID must not be empty")
	ErrEmptyDebitAccount  = errors.New("ledger: debit account must not be empty")
	ErrEmptyCreditAccount = errors.New("ledger: credit account must not be empty")
	ErrSameAccount        = errors.New("ledger: debit and credit accounts must differ")
	ErrNonPositiveAmount  = errors.New("ledger: amount must be greater than zero")
	ErrInvalidCursor      = errors.New("ledger: pagination cursor is invalid")
	ErrInvalidLimit       = errors.New("ledger: limit must be between 1 and 1000")
)

// maxPageSize caps a single ListEntries page.
const maxPageSize = 1000

// Ledger is an in-memory double-entry ledger. It is safe for concurrent use.
//
// Every successful Post bumps the ledger's version, a monotonically
// increasing sequence number. Versioned snapshots let reconciliation
// consumers detect whether anything changed between two reads: if the
// version is identical, the balance necessarily is too.
type Ledger struct {
	mu       sync.Mutex
	balances map[AccountID]int64
	entries  map[string]JournalEntry // by entry ID
	byKey    map[string]JournalEntry // by idempotency key
	version  uint64                  // bumped by every successful Post
}

// New returns an empty Ledger.
func New() *Ledger {
	return &Ledger{
		balances: make(map[AccountID]int64),
		entries:  make(map[string]JournalEntry),
		byKey:    make(map[string]JournalEntry),
	}
}

// Post records a journal entry and applies its balance effects.
//
// The entry must have a non-empty ID, distinct non-empty debit and credit
// accounts, and an AmountCents greater than zero; otherwise an error is
// returned and nothing is recorded.
//
// If e.IdempotencyKey is non-empty and the same key was posted before, Post
// returns the originally posted entry with duplicate == true and does not
// book anything again. A zero CreatedAt is filled with the current time.
//
// On a first-time post, the debit account's balance increases by AmountCents
// and the credit account's balance decreases by AmountCents.
func (l *Ledger) Post(e JournalEntry) (posted JournalEntry, duplicate bool, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if e.ID == "" {
		return JournalEntry{}, false, ErrEmptyID
	}
	if e.DebitAccount == "" {
		return JournalEntry{}, false, ErrEmptyDebitAccount
	}
	if e.CreditAccount == "" {
		return JournalEntry{}, false, ErrEmptyCreditAccount
	}
	if e.DebitAccount == e.CreditAccount {
		return JournalEntry{}, false, ErrSameAccount
	}
	if e.AmountCents <= 0 {
		return JournalEntry{}, false, ErrNonPositiveAmount
	}

	if e.IdempotencyKey != "" {
		if orig, ok := l.byKey[e.IdempotencyKey]; ok {
			return orig, true, nil
		}
	}

	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}

	l.entries[e.ID] = e
	if e.IdempotencyKey != "" {
		l.byKey[e.IdempotencyKey] = e
	}
	l.balances[e.DebitAccount] += e.AmountCents
	l.balances[e.CreditAccount] -= e.AmountCents
	l.version++

	return e, false, nil
}

// Balance returns the current net balance (in cents) of the given account.
// Unknown accounts have a zero balance.
func (l *Ledger) Balance(a AccountID) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.balances[a]
}

// GetByIdempotencyKey returns the entry previously posted with the given
// idempotency key, or false if the key has never been posted.
func (l *Ledger) GetByIdempotencyKey(key string) (JournalEntry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.byKey[key]
	return e, ok
}

// Entries returns a copy of all posted journal entries.
func (l *Ledger) Entries() []JournalEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]JournalEntry, 0, len(l.entries))
	for _, e := range l.entries {
		out = append(out, e)
	}
	return out
}

// Snapshot returns the current net balance (in cents) of the given account
// together with the ledger version at read time. Unknown accounts have a
// zero balance. The version is bumped by every successful Post (idempotent
// replays and rejected entries do not count), so two snapshots with the same
// version are guaranteed to show the same balance — a cheap change detector
// for reconciliation jobs.
func (l *Ledger) Snapshot(a AccountID) (balance int64, version uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.balances[a], l.version
}

// ListEntries returns up to limit journal entries whose CreatedAt falls in
// [since, until), sorted by (CreatedAt, ID). Pagination is cursor-based:
// pass the nextCursor returned by the previous call to continue; an empty
// cursor starts from the beginning. The returned nextCursor is empty when
// the last page has been returned.
//
// The cursor is opaque (base64url of the last returned entry's ID) and the
// resume position is defined by sort order, so entries posted between two
// pages are never duplicated or skipped: the ledger is append-only, so an
// entry named by a cursor can never move. A cursor naming an entry outside
// the requested window is rejected with ErrInvalidCursor.
func (l *Ledger) ListEntries(since, until time.Time, cursor string, limit int) (page []JournalEntry, nextCursor string, err error) {
	if limit < 1 || limit > maxPageSize {
		return nil, "", ErrInvalidLimit
	}
	var afterID string
	if cursor != "" {
		decoded, decErr := base64.RawURLEncoding.DecodeString(cursor)
		if decErr != nil || len(decoded) == 0 {
			return nil, "", ErrInvalidCursor
		}
		afterID = string(decoded)
	}

	l.mu.Lock()
	filtered := make([]JournalEntry, 0, len(l.entries))
	for _, e := range l.entries {
		if e.CreatedAt.Before(since) {
			continue
		}
		if !e.CreatedAt.Before(until) {
			continue
		}
		filtered = append(filtered, e)
	}
	l.mu.Unlock()

	sort.Slice(filtered, func(i, j int) bool {
		if !filtered[i].CreatedAt.Equal(filtered[j].CreatedAt) {
			return filtered[i].CreatedAt.Before(filtered[j].CreatedAt)
		}
		return filtered[i].ID < filtered[j].ID
	})

	start := 0
	if afterID != "" {
		found := false
		for i, e := range filtered {
			if e.ID == afterID {
				start = i + 1
				found = true
				break
			}
		}
		if !found {
			return nil, "", ErrInvalidCursor
		}
	}

	end := start + limit
	if end > len(filtered) {
		end = len(filtered)
	}
	page = filtered[start:end]
	if end < len(filtered) {
		nextCursor = base64.RawURLEncoding.EncodeToString([]byte(page[len(page)-1].ID))
	}
	return page, nextCursor, nil
}
