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
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// AccountID identifies an account in the ledger.
type AccountID string

// JournalEntry is a single double-entry journal posting.
//
// Currency is the ISO 4217 alpha-3 code the posting is denominated in
// (e.g. "USD", "EUR", "CNY"). An empty currency on input means the
// default currency (see DefaultCurrency); Post normalizes it before
// committing, so journaled entries always carry an explicit code and the
// audit-chain hash covers it like every other journaled field. One entry
// is always single-currency: debit and credit legs can never span
// currencies, and balances are tracked per (account, currency), so the
// accounting equation is verified per currency, never across them.
type JournalEntry struct {
	ID             string    `json:"id"`
	DebitAccount   AccountID `json:"debit_account"`
	CreditAccount  AccountID `json:"credit_account"`
	AmountCents    int64     `json:"amount_cents"`
	Currency       string    `json:"currency,omitempty"`
	IdempotencyKey string    `json:"idempotency_key,omitempty"`
	// BatchID names the atomic batch this entry was posted in (see
	// PostBatch in batch.go); it is empty for entries posted individually.
	// It is query metadata: the audit chain covers the entry itself, and
	// VerifyChain/Reconcile cover batch entries exactly like ordinary
	// postings.
	BatchID string `json:"batch_id,omitempty"`
	// Memo is a free-form business note attached to the entry at post
	// time (an order ID, an invoice reference, a reconciliation tag).
	// It is part of the journaled record: the audit-chain hash covers
	// it, snapshots carry it, and GET /entries?memo= filters on it. At
	// most 255 UTF-8 characters (see ErrMemoTooLong); longer memos are
	// rejected with a 400 before anything is recorded. Transfers carry
	// the memo on the principal entry (see Transfer.Memo).
	Memo      string    `json:"memo,omitempty"`
	CreatedAt time.Time `json:"created_at"`
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
	ErrMemoTooLong        = errors.New("ledger: memo must be at most 255 UTF-8 characters")
)

// maxMemoRunes caps a journal entry's memo at 255 UTF-8 characters. Long
// enough for an order ID plus a human note, short enough to keep journal
// rows and the audit chain bounded.
const maxMemoRunes = 255

// checkMemoLength rejects memos longer than maxMemoRunes UTF-8
// characters. Callers: validateJournalEntry (Post and PostBatch entries)
// and PostTransfer (transfer-level memo).
func checkMemoLength(memo string) error {
	if utf8.RuneCountInString(memo) > maxMemoRunes {
		return ErrMemoTooLong
	}
	return nil
}

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
	mu sync.RWMutex
	// balances, debitTotals, and creditTotals are keyed by
	// (account, currency): one account can hold several currencies side
	// by side, and the books for each currency stay isolated (see
	// currency.go). The per-account index below stays account-keyed —
	// it maps an account to every entry ID that touched it, in any
	// currency.
	balances     map[accountCurrency]int64
	entries      map[string]JournalEntry   // by entry ID
	byKey        map[string]JournalEntry   // by idempotency key
	byAccount    map[AccountID][]string    // entry IDs per account, in Post order
	debitTotals  map[accountCurrency]int64 // total cents ever debited per (account, currency)
	creditTotals map[accountCurrency]int64 // total cents ever credited per (account, currency)
	frozen       map[AccountID]bool        // risk-control stops; frozen accounts reject new posts (see freeze.go)
	// noOverdraft marks accounts guarded against overdrafts: a Post that
	// would take a protected credit (payer) account below zero is rejected
	// with ErrAccountOverdraft (see overdraft.go). Opt-in per account.
	noOverdraft map[AccountID]bool
	// dailyLimits maps (account, currency) to the account's daily outflow
	// limit in cents, and dailyOutflow accumulates the outflow booked per
	// (account, currency, UTC day). A Post that would take the day's
	// cumulative outflow above the limit is rejected with
	// ErrDailyLimitExceeded (see daily_limit.go). Opt-in per account and
	// currency; structural config, like frozen and noOverdraft.
	dailyLimits  map[dailyLimitKey]int64
	dailyOutflow map[dailyLimitKey]int64
	// lowBalanceThresholds maps (account, currency) to the account's
	// low-balance alert level in cents, and lowBalanceBreached marks the
	// (account, currency) pairs currently in breach (silenced until the
	// balance recovers to the threshold). A posting that takes a balance
	// below its threshold emits one low_balance_breach audit event and
	// bumps lowBalanceBreaches (see low_balance.go). Advisory only —
	// never a rejection. Opt-in per account and currency; structural
	// config, like dailyLimits.
	lowBalanceThresholds map[lowBalanceKey]int64
	lowBalanceBreached   map[lowBalanceKey]bool
	lowBalanceBreaches   uint64
	// parents maps a child account to its parent account in the
	// sub-account hierarchy (see hierarchy.go). Only accounts with an
	// assigned parent appear here; clearing the parent deletes the row.
	// SetParent keeps the map cycle-free, so Rollup's subtree walk always
	// terminates.
	parents map[AccountID]AccountID
	// feeTiers / feeRevenueAccount configure the default transfer fee
	// policy (see WithTransferFeeSchedule and WithTransferFeePolicy in
	// transfer.go): unless a transfer carries an explicit fee or sets
	// SkipFee, PostTransfer books a fee leg of floor(amount * tierRateBps
	// / 10000) cents to feeRevenueAccount, where the tier is the last one
	// whose minimum does not exceed the transfer amount. Empty tiers (or
	// an empty revenue account) disable the policy.
	feeTiers          []FeeTier
	feeRevenueAccount AccountID
	// fxRates is the directional FX rate table (see fx.go): converting
	// fromCurrency to toCurrency multiplies by Num/Den and floors to
	// whole cents. fxAccount is the ledger-wide FX clearing account, the
	// counterparty of cross-currency transfer legs. Both are structural
	// config — like frozen and feeTiers — and survive snapshots.
	fxRates   map[fxPair]ExchangeRate
	fxAccount AccountID
	// transferKeys maps a transfer's idempotency key to the IDs of the
	// journal entries it posted, in commit order (principal, then the fee
	// leg when one was booked), so a replayed transfer returns its full
	// receipt. Keys expire with the TTL like the entry-level index.
	transferKeys map[string][]string
	// holds maps hold IDs to authorization holds (see hold.go). Holds are
	// off-journal reservations: they never appear in entries, the audit
	// chain, or the version counter. holdsByAccount indexes them per
	// account so Available only scans that account's holds.
	holds          map[string]Hold
	holdsByAccount map[AccountID][]string
	// holdKeys maps a hold's idempotency key to its hold ID, and
	// captureKeys maps a capture's idempotency key to its receipt. The
	// two namespaces are independent of each other and of the
	// entry/transfer key namespaces; all of them expire with the TTL.
	holdKeys    map[string]string
	captureKeys map[string]CaptureReceipt
	// sweepKeys maps a sweep's idempotency key to its sweepRecord, so
	// replays rebuild the full receipt (see sweep.go). Its own namespace,
	// like holdKeys/captureKeys, expiring with the TTL.
	sweepKeys map[string]sweepRecord
	// batchKeys maps a batch's idempotency key to its batchRecord, so
	// replays rebuild the full receipt (see batch.go). Its own namespace,
	// expiring with the TTL like every other key index (see
	// pruneBatchKeysLocked in batch.go).
	batchKeys map[string]batchRecord
	// merges is the authoritative registry of committed account merges
	// (see merge.go), keyed by merge ID: every PostMerge appends here,
	// whether or not it carried an idempotency key. It feeds Reconcile's
	// merge history and idempotent replays (via mergeKeys).
	merges map[string]mergeRecord
	// mergeKeys maps a merge's idempotency key to its merge ID. Its own
	// namespace, expiring with the TTL like every other key index (see
	// pruneMergeKeysLocked in merge.go).
	mergeKeys map[string]string
	// audit is the structured compliance audit log (see audit.go). Nil
	// means disabled: operations skip event construction entirely and
	// reads never touch it, so the read path is unaffected.
	audit *AuditLog
	// closedPeriods is the set of locked accounting periods ("2006-01",
	// UTC months; see period.go). Journal entries whose timestamp falls
	// in a closed period are rejected with ErrPeriodClosed by every
	// journal-writing operation, so a closed month's books cannot change.
	// Structural config like frozen — it survives snapshots and is
	// listed in Reconcile.
	closedPeriods  map[string]bool
	chain          []chainLink   // audit chain, one link per successful Post, in order
	version        uint64        // bumped by every successful Post
	idempotencyTTL time.Duration // 0 = never expire idempotency keys
	pruneInterval  time.Duration // min gap between lazy key sweeps
	lastKeyPrune   time.Time
	lastDailyPrune time.Time // last run of the daily-outflow bucket sweep
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

// WithAuditLog attaches a structured audit log (see audit.go): every
// mutating operation and every Reconcile run emits one JSONL event after
// it commits. A nil log disables auditing entirely.
func WithAuditLog(al *AuditLog) Option {
	return func(l *Ledger) {
		l.audit = al
	}
}

// emitAudit enqueues one audit event when the audit log is enabled; it is
// a no-op when disabled. Callers must hold l.mu (either lock suffices) —
// the enqueue never blocks, so emitting under the write lock cannot stall
// the operation. The timestamp is taken here so the recorded time matches
// the commit, not the (asynchronous) flush.
func (l *Ledger) emitAudit(ev AuditEvent) {
	if l.audit == nil {
		return
	}
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now().UTC()
	}
	l.audit.Log(ev)
}

// AuditStats reports the audit log's written/dropped event counters.
// Enabled is false when no audit log is attached (WithAuditLog was never
// used): in that case the counters are zero and meaningless.
func (l *Ledger) AuditStats() (written, dropped uint64, enabled bool) {
	if l.audit == nil {
		return 0, 0, false
	}
	w, d := l.audit.Stats()
	return w, d, true
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
		balances:             make(map[accountCurrency]int64),
		entries:              make(map[string]JournalEntry),
		byKey:                make(map[string]JournalEntry),
		byAccount:            make(map[AccountID][]string),
		debitTotals:          make(map[accountCurrency]int64),
		creditTotals:         make(map[accountCurrency]int64),
		frozen:               make(map[AccountID]bool),
		noOverdraft:          make(map[AccountID]bool),
		dailyLimits:          make(map[dailyLimitKey]int64),
		dailyOutflow:         make(map[dailyLimitKey]int64),
		lowBalanceThresholds: make(map[lowBalanceKey]int64),
		lowBalanceBreached:   make(map[lowBalanceKey]bool),
		parents:              make(map[AccountID]AccountID),
		transferKeys:         make(map[string][]string),
		holds:                make(map[string]Hold),
		holdsByAccount:       make(map[AccountID][]string),
		holdKeys:             make(map[string]string),
		captureKeys:          make(map[string]CaptureReceipt),
		sweepKeys:            make(map[string]sweepRecord),
		batchKeys:            make(map[string]batchRecord),
		merges:               make(map[string]mergeRecord),
		mergeKeys:            make(map[string]string),
		fxRates:              make(map[fxPair]ExchangeRate),
		closedPeriods:        make(map[string]bool),
		pruneInterval:        defaultKeyPruneInterval,
	}
	for _, opt := range opts {
		opt(l)
	}
	return l
}

// validateJournalEntry checks the double-entry fields of e and normalizes
// its currency in place: non-empty ID, non-empty distinct debit/credit
// legs, a positive amount, and a valid ISO 4217 code (empty normalizes to
// the default currency, like Post does). It records nothing, so it is safe
// to run as the admission gate of multi-entry operations (see PostBatch):
// a batch validates every entry through this before the first journal row
// lands. Callers that reuse a validated entry must not mutate its legs,
// amount, or currency afterwards.
func validateJournalEntry(e *JournalEntry) error {
	if e.ID == "" {
		return ErrEmptyID
	}
	if e.DebitAccount == "" {
		return ErrEmptyDebitAccount
	}
	if e.CreditAccount == "" {
		return ErrEmptyCreditAccount
	}
	if e.DebitAccount == e.CreditAccount {
		return ErrSameAccount
	}
	if e.AmountCents <= 0 {
		return ErrNonPositiveAmount
	}
	// The memo is field validation like the legs and the amount: a note
	// longer than maxMemoRunes UTF-8 characters is rejected before
	// anything is recorded.
	if err := checkMemoLength(e.Memo); err != nil {
		return err
	}
	// Currency is field validation, like the legs and the amount: an
	// empty code normalizes to the default currency, anything else must
	// be a 3-letter uppercase ISO 4217 code. Normalization happens here,
	// before the idempotency replay check, so the journal, the replay
	// index, and the audit chain all store the canonical code.
	currency, err := normalizeCurrency(e.Currency)
	if err != nil {
		return err
	}
	e.Currency = currency
	return nil
}

// Post records a journal entry and applies its balance effects.
//
// Double-entry validation runs before anything is recorded: the entry must
// carry both legs of the posting — a non-empty debit account and a
// non-empty credit account, distinct from each other — a positive amount,
// and a valid currency (empty normalizes to the default currency; anything
// else must be a 3-letter uppercase ISO 4217 code), so the debit leg always
// equals the credit leg in one currency (the books balance by
// construction, per currency). A validation failure returns an error and
// records nothing.
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
// after the idempotency replay check for the same reason. A credit
// (payer) account with a configured daily outflow limit (see
// SetDailyLimit) rejects any post that would take the UTC day's
// cumulative outflow above the limit with ErrDailyLimitExceeded; the check
// runs last among the account risk controls, so it only ever evaluates postings
// that book something new. A journal entry whose timestamp falls in a closed
// accounting period (see period.go) is rejected with ErrPeriodClosed; the
// period gate runs last overall, after the idempotency replay check, so
// replaying a key posted before the period closed returns the original
// entry. Rejected posts —
// validation failures, frozen rejections, overdraft rejections,
// daily-limit rejections, and closed-period rejections alike —
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

	if err := validateJournalEntry(&e); err != nil {
		return JournalEntry{}, false, err
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

	// CreatedAt is filled before the daily-limit check: the outflow is
	// booked against the entry's own UTC calendar day, so the timestamp
	// must be final when the limit is evaluated.
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}

	// Daily outflow limits are the last of the account risk controls, after
	// the frozen and overdraft checks: a replay books nothing new, so it
	// returned above; anything reaching this check books a new outflow.
	// The limited side is the credit (payer) account — the account funds
	// leave — matching PostTransfer's payer leg.
	if l.dailyLimitRejectedLocked(e.CreditAccount, e.Currency, e.AmountCents, e.CreatedAt) {
		return JournalEntry{}, false, ErrDailyLimitExceeded
	}

	// The period gate is the last risk control overall, and it is keyed
	// on the entry's own timestamp — the last thing finalized above: a
	// backdated entry landing in a closed accounting period is rejected
	// with ErrPeriodClosed (see period.go). It runs after the idempotency
	// replay check, so replaying a key posted before the period closed
	// returns the original entry instead of failing.
	if err := l.periodRejectedLocked(e.CreatedAt); err != nil {
		return JournalEntry{}, false, err
	}

	l.maybePruneIdempotencyKeys(time.Now())
	l.maybePruneDailyOutflowLocked(time.Now())
	versionBefore := l.version
	l.commitEntryLocked(e)
	l.addDailyOutflowLocked(e.CreditAccount, e.Currency, e.AmountCents, e.CreatedAt)
	// Low-balance alert evaluation: strictly after the atomic commit
	// zone, read-only (see low_balance.go). Advisory only — it can
	// neither fail nor alter this posting.
	l.evaluateLowBalanceLocked([]accountCurrency{
		{account: e.DebitAccount, currency: e.Currency},
		{account: e.CreditAccount, currency: e.Currency},
	}, e.ID, "Post")
	postDetails := map[string]any{
		"amount_cents": e.AmountCents,
		"currency":     e.Currency,
	}
	if e.Memo != "" {
		postDetails["memo"] = e.Memo
	}
	l.emitAudit(AuditEvent{
		Op:            "post",
		Actor:         "Post",
		TraceID:       e.ID,
		VersionBefore: versionBefore,
		VersionAfter:  l.version,
		EntryIDs:      []string{e.ID},
		Accounts:      []AccountID{e.DebitAccount, e.CreditAccount},
		Details:       postDetails,
	})

	return e, false, nil
}

// Balance returns the current net balance (in cents) of the given account
// in the default currency (see DefaultCurrency). Unknown accounts have a
// zero balance. In a multi-currency ledger a single number cannot describe
// an account — use BalanceIn or TrialBalance for a specific currency, or
// TrialBalance's ByCurrency breakdown for the full picture. Balance keeps
// its historical meaning (the default-currency balance) so existing
// readers keep working unchanged.
func (l *Ledger) Balance(a AccountID) int64 {
	return l.BalanceIn(a, DefaultCurrency)
}

// Version returns the ledger's current journal version: the count of
// committed journal entries. Read-only.
func (l *Ledger) Version() uint64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.version
}

// BalanceIn returns the current net balance (in cents) of the given
// account in the given currency. Unknown accounts, and accounts with no
// postings in that currency, have a zero balance. An empty currency means
// the default currency; other codes are looked up as-is (codes that never
// appeared simply report zero).
func (l *Ledger) BalanceIn(a AccountID, currency string) int64 {
	if currency == "" {
		currency = DefaultCurrency
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.balances[accountCurrency{account: a, currency: currency}]
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
	// Hold and capture keys expire on the same schedule, in their own
	// namespaces (see pruneHoldKeysLocked in hold.go). Sweep keys expire
	// on the same schedule too (see pruneSweepKeysLocked in sweep.go),
	// and so do merge keys (see pruneMergeKeysLocked in merge.go) and
	// batch keys (see pruneBatchKeysLocked in batch.go).
	removed += l.pruneHoldKeysLocked(now)
	removed += l.pruneSweepKeysLocked(now)
	removed += l.pruneMergeKeysLocked(now)
	removed += l.pruneBatchKeysLocked(now)
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
// in the default currency (see DefaultCurrency), together with the ledger
// version at read time. Unknown accounts have a zero balance. The version
// is bumped by every successful Post (idempotent replays and rejected
// entries do not count), so two snapshots with the same version are
// guaranteed to show the same balance — a cheap change detector for
// reconciliation jobs. For a specific currency, use BalanceIn; the
// version it pairs with is the same ledger-wide sequence.
func (l *Ledger) Snapshot(a AccountID) (balance int64, version uint64) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.balances[accountCurrency{account: a, currency: DefaultCurrency}], l.version
}

// TrialBalance is the double-entry breakdown of a single account: every cent
// ever debited to it, every cent ever credited from it, and the resulting
// net balance. NetBalance always equals TotalDebits - TotalCredits; across
// the whole ledger, the sum of all debit totals equals the sum of all
// credit totals within each currency — that per-currency equality is the
// accounting equation, and VerifyAccountingEquation checks it.
//
// The top-level totals describe the account in Currency (the default
// currency unless stated otherwise); ByCurrency breaks the same account
// down per currency, sorted by currency code, so multi-currency accounts
// report every currency they hold. A single-currency account's ByCurrency
// holds exactly one row, mirroring the top-level totals.
type TrialBalance struct {
	Account      AccountID `json:"account"`
	Currency     string    `json:"currency"`
	TotalDebits  int64     `json:"total_debits_cents"`
	TotalCredits int64     `json:"total_credits_cents"`
	NetBalance   int64     `json:"net_balance_cents"`
	// ByCurrency is the per-currency breakdown of this account, sorted by
	// currency code. It always covers every currency the account has
	// postings in, including Currency itself.
	ByCurrency []CurrencyTrialBalance `json:"by_currency"`
	Version    uint64                 `json:"version"`
	// Frozen reports whether the account is currently stopped by a
	// risk-control freeze (see Freeze). A frozen account keeps its
	// balances and history; only new postings through it are rejected.
	Frozen bool `json:"frozen"`
	// OverdraftProtected reports whether the account is guarded against
	// overdrafts (see EnableOverdraftProtection): Postings that would
	// take the balance below zero are rejected with ErrAccountOverdraft.
	OverdraftProtected bool `json:"overdraft_protected"`
	// DailyLimitCents reports the account's configured daily outflow
	// limit in the default currency (see SetDailyLimit): postings that
	// would take the UTC day's cumulative outflow above this number are
	// rejected with ErrDailyLimitExceeded. 0 means no limit is configured
	// for the default currency; per-currency limits live in the
	// reconciliation report and on Ledger.DailyLimit.
	DailyLimitCents int64 `json:"daily_limit_cents"`
	// LowBalanceThresholdCents reports the account's configured
	// low-balance alert level in the default currency (see
	// SetLowBalanceThreshold): the first posting that takes the balance
	// below this number emits one low_balance_breach audit event. 0
	// means no threshold is configured for the default currency;
	// per-currency thresholds live in the reconciliation report and on
	// Ledger.LowBalanceThreshold.
	LowBalanceThresholdCents int64 `json:"low_balance_threshold_cents"`
}

// TrialBalance returns the double-entry breakdown of the given account at
// the current ledger version. Unknown accounts report zeros. The version is
// the same sequence Snapshot reports, so a trial balance and a snapshot
// taken at one version describe the same books.
func (l *Ledger) TrialBalance(a AccountID) TrialBalance {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.trialBalanceLocked(a)
}

// trialBalanceLocked builds the trial balance. Callers must hold l.mu; the
// read lock suffices because nothing is mutated. The reconciliation scan
// calls this directly so the per-account rows are built under the same
// read lock as the rest of the report.
func (l *Ledger) trialBalanceLocked(a AccountID) TrialBalance {
	tb := TrialBalance{
		Account:                  a,
		Currency:                 DefaultCurrency,
		Version:                  l.version,
		Frozen:                   l.frozen[a],
		OverdraftProtected:       l.noOverdraft[a],
		DailyLimitCents:          l.dailyLimits[dailyLimitKey{account: a, currency: DefaultCurrency}],
		LowBalanceThresholdCents: l.lowBalanceThresholds[lowBalanceKey{account: a, currency: DefaultCurrency}],
	}
	seen := make(map[string]bool)
	for k := range l.balances {
		if k.account == a {
			seen[k.currency] = true
		}
	}
	for k := range l.debitTotals {
		if k.account == a {
			seen[k.currency] = true
		}
	}
	for k := range l.creditTotals {
		if k.account == a {
			seen[k.currency] = true
		}
	}
	currencies := make([]string, 0, len(seen))
	for c := range seen {
		currencies = append(currencies, c)
	}
	sort.Strings(currencies)
	for _, c := range currencies {
		d := l.debitTotals[accountCurrency{account: a, currency: c}]
		cr := l.creditTotals[accountCurrency{account: a, currency: c}]
		row := CurrencyTrialBalance{
			Currency:     c,
			TotalDebits:  d,
			TotalCredits: cr,
			NetBalance:   l.balances[accountCurrency{account: a, currency: c}],
		}
		tb.ByCurrency = append(tb.ByCurrency, row)
		if c == DefaultCurrency {
			tb.TotalDebits = d
			tb.TotalCredits = cr
			tb.NetBalance = row.NetBalance
		}
	}
	return tb
}

// VerifyAccountingEquation checks the two invariants double-entry
// bookkeeping guarantees, isolated per currency: for every
// (account, currency), net balance == total debits − total credits; and
// within each currency, total debits == total credits (equivalently, the
// sum of all net balances in that currency is zero). Currencies never mix
// in the check — adding USD cents to EUR cents would be meaningless, so
// each currency's books must balance on their own. It returns nil when the
// books balance. Operators can run it after imports or restores; the test
// suite runs it after every scenario, including the concurrent stress test.
func (l *Ledger) VerifyAccountingEquation() error {
	l.mu.RLock()
	defer l.mu.RUnlock()
	seen := make(map[accountCurrency]bool)
	for k := range l.balances {
		seen[k] = true
	}
	for k := range l.debitTotals {
		seen[k] = true
	}
	for k := range l.creditTotals {
		seen[k] = true
	}
	debits := make(map[string]int64)
	credits := make(map[string]int64)
	for k := range seen {
		d := l.debitTotals[k]
		c := l.creditTotals[k]
		if want := d - c; l.balances[k] != want {
			return fmt.Errorf("ledger: account %q (%s) out of balance: net %d != debits %d - credits %d",
				k.account, k.currency, l.balances[k], d, c)
		}
		debits[k.currency] += d
		credits[k.currency] += c
	}
	for currency, d := range debits {
		if c := credits[currency]; d != c {
			return fmt.Errorf("ledger: books do not balance in %s: total debits %d != total credits %d",
				currency, d, c)
		}
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
	return l.ListEntriesFiltered(since, until, cursor, limit, "")
}

// ListEntriesFiltered is ListEntries with an additional memo keyword
// filter (see JournalEntry.Memo): when memo is non-empty, only entries
// whose memo contains it as a substring are returned. The keyword filter
// composes with the time window and the cursor pagination — a cursor from
// a filtered page resumes the same filtered sequence, because the cursor
// names an entry by ID and the filter is re-applied before pagination.
// The match is a case-sensitive substring search: memo notes are
// identifiers (order IDs, invoice references), where case matters.
func (l *Ledger) ListEntriesFiltered(since, until time.Time, cursor string, limit int, memo string) (page []JournalEntry, nextCursor string, err error) {
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
		if memo != "" && !strings.Contains(e.Memo, memo) {
			continue
		}
		filtered = append(filtered, e)
	}
	l.mu.RUnlock()

	return paginateSortedEntries(filtered, afterID, limit)
}
