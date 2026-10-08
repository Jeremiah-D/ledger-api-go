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
	"errors"
	"fmt"
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

// defaultKeyPruneInterval is the minimum time between two lazy idempotency-key
// sweeps when a TTL is configured. It keeps Post amortized O(1): without it,
// every Post would scan the whole key table.
const defaultKeyPruneInterval = time.Minute

// Ledger is an in-memory double-entry ledger. It is safe for concurrent use.
//
// Every successful Post bumps the ledger's version, a monotonically
// increasing sequence number. Versioned snapshots let reconciliation
// consumers detect whether anything changed between two reads: if the
// version is identical, the balance necessarily is too.
//
// Idempotency keys are kept in a separate index so replays can be detected
// without scanning the journal. The index grows with every distinct key, so
// a TTL can be configured (WithIdempotencyTTL): keys older than the TTL are
// eligible for eviction, which bounds memory in long-running processes. A
// zero TTL disables expiry entirely.
//
// A per-account index maps each account to the IDs of the entries that
// touched it (as debit or credit leg), so per-account listing scans only
// that account's entries instead of the whole journal.
//
// Every successful Post also appends one link to the tamper-evident audit
// chain (see chain.go): a SHA-256 link of the entry onto the previous
// link's hash. The chain makes silent rewrites of journaled entries
// detectable via VerifyChain.
//
// The mutex is an RWMutex: reads (Balance, Snapshot, ListEntries, ...) take
// the read lock and run concurrently, while Post and ExpireIdempotencyKeys
// take the write lock. Reads never block each other, only writers.
type Ledger struct {
	mu             sync.RWMutex
	balances       map[AccountID]int64
	entries        map[string]JournalEntry // by entry ID
	byKey          map[string]JournalEntry // by idempotency key
	byAccount      map[AccountID][]string  // entry IDs per account, in Post order
	debitTotals    map[AccountID]int64     // total cents ever debited per account
	creditTotals   map[AccountID]int64     // total cents ever credited per account
	frozen         map[AccountID]bool      // risk-control stops; frozen accounts reject new posts (see freeze.go)
	// noOverdraft marks accounts guarded against overdrafts: a Post that
	// would take a protected credit (payer) account below zero is rejected
	// with ErrAccountOverdraft (see overdraft.go). Opt-in per account.
	noOverdraft map[AccountID]bool
	// feeRateBps / feeRevenueAccount configure the default transfer fee
	// policy (see WithTransferFeePolicy in transfer.go): unless a transfer
	// carries an explicit fee or sets SkipFee, PostTransfer books a fee leg
	// of floor(amount * feeRateBps / 10000) cents to feeRevenueAccount.
	feeRateBps        int64
	feeRevenueAccount AccountID
	// transferKeys maps a transfer's idempotency key to the IDs of the
	// journal entries it posted, in commit order (principal, then the fee
	// leg when one was booked), so a replayed transfer returns its full
	// receipt. Keys expire with the TTL like the entry-level index.
	transferKeys       map[string][]string
	chain          []chainLink             // audit chain, one link per successful Post, in order
	version        uint64                  // bumped by every successful Post
	idempotencyTTL time.Duration           // 0 = never expire idempotency keys
	pruneInterval  time.Duration           // min gap between lazy key sweeps
	lastKeyPrune   time.Time
}

// Option configures a Ledger.
type Option func(*Ledger)

// WithIdempotencyTTL sets how long an idempotency key is retained after the
// entry was posted. Keys older than ttl are eligible for eviction via
// ExpireIdempotencyKeys and via lazy pruning on Post. A non-positive ttl
// disables expiry (the default).
func WithIdempotencyTTL(ttl time.Duration) Option {
	return func(l *Ledger) {
		l.idempotencyTTL = ttl
	}
}

// WithOverdraftProtection marks the given accounts as protected from
// overdrafts from the start: any Post that would take a protected credit
// (payer) account's balance below zero is rejected with
// ErrAccountOverdraft. See EnableOverdraftProtection for the per-account
// semantics; the same option can be combined with WithIdempotencyTTL.
func WithOverdraftProtection(accounts ...AccountID) Option {
	return func(l *Ledger) {
		for _, a := range accounts {
			l.noOverdraft[a] = true
		}
	}
}

// New returns an empty Ledger. Options configure behavior; by default
// idempotency keys never expire.
func New(opts ...Option) *Ledger {
	l := &Ledger{
		balances:      make(map[AccountID]int64),
		entries:       make(map[string]JournalEntry),
		byKey:         make(map[string]JournalEntry),
		byAccount:     make(map[AccountID][]string),
		debitTotals:   make(map[AccountID]int64),
		creditTotals:  make(map[AccountID]int64),
		frozen:        make(map[AccountID]bool),
		noOverdraft:   make(map[AccountID]bool),
		transferKeys:  make(map[string][]string),
		pruneInterval: defaultKeyPruneInterval,
	}
	for _, opt := range opts {
		opt(l)
	}
	return l
}

// Post records a journal entry and applies its balance effects.
//
// Double-entry validation runs before anything is recorded: the entry must
// carry both legs of the posting — a non-empty debit account and a
// non-empty credit account, distinct from each other — and a positive
// amount, so the debit leg always equals the credit leg (the books balance
// by construction). A validation failure returns an error and records
// nothing.
//
// If e.IdempotencyKey is non-empty and the same key was posted before, Post
// returns the originally posted entry with duplicate == true and does not
// book anything again. A zero CreatedAt is filled with the current time.
//
// A frozen account (see freeze.go) rejects any post that would book a new
// entry through it with ErrAccountFrozen. The frozen check runs after the
// idempotency replay check: replaying a key that was posted before the
// freeze returns the original entry instead of failing, because the replay
// books nothing new. A credit (payer) account under overdraft protection
// (see overdraft.go) rejects any post that would take its balance below
// zero with ErrAccountOverdraft; the check runs after the frozen check and
// after the idempotency replay check for the same reason. Rejected posts —
// validation failures, frozen rejections, and overdraft rejections alike —
// record nothing: no journal row, no chain link, no version bump.
//
// The commit is atomic: while holding the ledger's single mutex, Post
// applies every effect of the entry at once — the journal row, the
// idempotency index, both net balances, both debit/credit totals, and the
// audit-chain link — then bumps the version. Either all of them land or (on
// validation failure) none do; readers never observe a half-posted entry.
//
// Idempotent replays append no chain link: the chain records journaled
// entries, and a replay journals nothing new.
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

	if l.frozenLocked(e.DebitAccount) || l.frozenLocked(e.CreditAccount) {
		return JournalEntry{}, false, ErrAccountFrozen
	}

	// Overdraft protection is a risk control, not bookkeeping validation:
	// it runs after the frozen check (a frozen account fails 403 before
	// the overdraft question even arises) and after the idempotency replay
	// check above (a replay books nothing new, so it must not fail on an
	// account that was protected after the original posting).
	if l.overdraftRejectedLocked(e) {
		return JournalEntry{}, false, ErrAccountOverdraft
	}

	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}

	l.maybePruneIdempotencyKeys(time.Now())
	l.commitEntryLocked(e)

	return e, false, nil
}

// Balance returns the current net balance (in cents) of the given account.
// Unknown accounts have a zero balance.
func (l *Ledger) Balance(a AccountID) int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.balances[a]
}

// GetByIdempotencyKey returns the entry previously posted with the given
// idempotency key, or false if the key has never been posted.
func (l *Ledger) GetByIdempotencyKey(key string) (JournalEntry, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	e, ok := l.byKey[key]
	return e, ok
}

// ExpireIdempotencyKeys evicts idempotency keys whose entry was posted more
// than the configured TTL ago, and returns how many were removed. It is a
// no-op returning 0 when no TTL is configured. Expired keys are forgotten
// entirely: reposting the same key afterwards books a brand-new entry
// (duplicate == false), so callers must pick a TTL longer than any retry or
// reconciliation window they rely on. The journal itself is untouched — only
// the replay-detection index shrinks.
func (l *Ledger) ExpireIdempotencyKeys() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.pruneIdempotencyKeysLocked(time.Now())
}

// maybePruneIdempotencyKeys runs a lazy key sweep at most once per
// pruneInterval. It amortizes expiry across Posts so no background goroutine
// is needed. Callers must hold l.mu.
func (l *Ledger) maybePruneIdempotencyKeys(now time.Time) {
	if l.idempotencyTTL <= 0 {
		return
	}
	if now.Sub(l.lastKeyPrune) < l.pruneInterval {
		return
	}
	l.lastKeyPrune = now
	l.pruneIdempotencyKeysLocked(now)
}

// pruneIdempotencyKeysLocked removes keys older than the TTL. A non-positive
// TTL disables pruning. Callers must hold l.mu.
func (l *Ledger) pruneIdempotencyKeysLocked(now time.Time) int {
	if l.idempotencyTTL <= 0 {
		return 0
	}
	cutoff := now.Add(-l.idempotencyTTL)
	removed := 0
	for key, e := range l.byKey {
		if e.CreatedAt.Before(cutoff) {
			delete(l.byKey, key)
			// A transfer's receipt index expires with its principal key:
			// after the TTL, reposting the transfer key books brand-new
			// entries, exactly like the entry-level contract.
			delete(l.transferKeys, key)
			removed++
		}
	}
	return removed
}

// Entries returns a copy of all posted journal entries.
func (l *Ledger) Entries() []JournalEntry {
	l.mu.RLock()
	defer l.mu.RUnlock()
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
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.balances[a], l.version
}

// TrialBalance is the double-entry breakdown of a single account: every cent
// ever debited to it, every cent ever credited from it, and the resulting
// net balance. NetBalance always equals TotalDebits - TotalCredits; across
// the whole ledger, the sum of all debit totals equals the sum of all
// credit totals — that equality is the accounting equation, and
// VerifyAccountingEquation checks it.
type TrialBalance struct {
	Account      AccountID `json:"account"`
	TotalDebits  int64     `json:"total_debits_cents"`
	TotalCredits int64     `json:"total_credits_cents"`
	NetBalance   int64     `json:"net_balance_cents"`
	Version      uint64    `json:"version"`
	// Frozen reports whether the account is currently stopped by a
	// risk-control freeze (see Freeze). A frozen account keeps its
	// balances and history; only new postings through it are rejected.
	Frozen bool `json:"frozen"`
	// OverdraftProtected reports whether the account is guarded against
	// overdrafts (see EnableOverdraftProtection): Postings that would
	// take the balance below zero are rejected with ErrAccountOverdraft.
	OverdraftProtected bool `json:"overdraft_protected"`
}

// TrialBalance returns the double-entry breakdown of the given account at
// the current ledger version. Unknown accounts report zeros. The version is
// the same sequence Snapshot reports, so a trial balance and a snapshot
// taken at one version describe the same books.
func (l *Ledger) TrialBalance(a AccountID) TrialBalance {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return TrialBalance{
		Account:            a,
		TotalDebits:        l.debitTotals[a],
		TotalCredits:       l.creditTotals[a],
		NetBalance:         l.balances[a],
		Version:            l.version,
		Frozen:             l.frozen[a],
		OverdraftProtected: l.noOverdraft[a],
	}
}

// VerifyAccountingEquation checks the two invariants double-entry
// bookkeeping guarantees: for every account, net balance == total debits −
// total credits; and globally, total debits == total credits (equivalently,
// the sum of all net balances is zero). It returns nil when the books
// balance. Operators can run it after imports or restores; the test suite
// runs it after every scenario, including the concurrent stress test.
func (l *Ledger) VerifyAccountingEquation() error {
	l.mu.RLock()
	defer l.mu.RUnlock()
	seen := make(map[AccountID]bool)
	for a := range l.balances {
		seen[a] = true
	}
	for a := range l.debitTotals {
		seen[a] = true
	}
	for a := range l.creditTotals {
		seen[a] = true
	}
	var debits, credits int64
	for a := range seen {
		if want := l.debitTotals[a] - l.creditTotals[a]; l.balances[a] != want {
			return fmt.Errorf("ledger: account %q out of balance: net %d != debits %d - credits %d",
				a, l.balances[a], l.debitTotals[a], l.creditTotals[a])
		}
		debits += l.debitTotals[a]
		credits += l.creditTotals[a]
	}
	if debits != credits {
		return fmt.Errorf("ledger: books do not balance: total debits %d != total credits %d", debits, credits)
	}
	return nil
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
	afterID, err := decodeListCursor(cursor)
	if err != nil {
		return nil, "", err
	}

	l.mu.RLock()
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
	l.mu.RUnlock()

	return paginateSortedEntries(filtered, afterID, limit)
}
