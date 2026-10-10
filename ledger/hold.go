package ledger

import (
	"errors"
	"fmt"
	"time"
)

// Authorization holds (auth/capture), the fintech pre-authorization flow.
//
// A Hold reserves part of an account's balance without moving any money
// through the journal: while a hold is active, the account's *available*
// balance (see Available) is reduced by the held amount, but the net
// balance and the debit/credit totals are untouched. A later Capture
// settles up to the held amount as an ordinary double-entry posting from
// the held account (the cardholder) to a payee, and the un-captured
// remainder is released back to available automatically. Release drops a
// hold without settling anything.
//
// Holds are off-journal by design: they create no journal rows, no
// audit-chain links, and no ledger-version bumps (version is the
// balance-change detector; a hold changes no balance). Only Capture, which
// journals a real posting, bumps the version and appends a chain link.
//
// Hold lifecycle: active -> captured | released | expired. A hold whose
// ExpiresAt has passed is expired; expiry is lazy (see activeHoldLocked)
// plus an explicit ExpireHolds sweep for observability. Release is
// idempotent: releasing a released or expired hold is a no-op returning
// the hold as-is. Capture is single-shot: one capture consumes the hold
// (amount <= held); a second capture fails with ErrHoldNotActive.
//
// Idempotency namespaces are per operation: holds use holdKeys and
// captures use captureKeys, independent of Post's byKey namespace and of
// PostTransfer's transferKeys. The same key string used in two different
// namespaces refers to two unrelated operations — keys are only ever
// compared within their own namespace. Like the entry-level keys, hold
// and capture keys honor the ledger's idempotency TTL (see
// WithIdempotencyTTL): after the TTL elapses, reposting a key books a
// brand-new hold or capture.
//
// Risk-control interplay:
//   - Frozen accounts cannot create holds and cannot be capture legs
//     (ErrAccountFrozen), mirroring Post. The frozen check runs after the
//     idempotency replay check, so a replay books nothing new and never
//     fails on a freeze. Release is exempt: it frees reserved funds
//     instead of moving money, so a frozen account can still release —
//     stranding reserved funds behind a freeze would turn a risk control
//     into a funds-availability incident.
//   - Holds consume *available* balance: Hold is rejected with
//     ErrInsufficientAvailableFunds when balance - active held < amount.
//     For overdraft-protected accounts this is also the overdraft guard —
//     their balance can never go below zero, so the available check
//     subsumes it. Unprotected accounts may carry negative balances (the
//     sign convention), in which case any positive hold fails the
//     available check.
//   - Captures are ordinary double-entry postings from the held account
//     (the payer) to the payee: they run the same frozen and overdraft
//     checks as Post. Note the reservation is advisory, not a balance
//     lock: a concurrent posting can move the held account's balance
//     between the hold and the capture, so a capture on a protected
//     account whose balance dropped below the capture amount is rejected
//     with ErrAccountOverdraft even though the hold was valid when
//     placed. Holds reserve available funds; they do not escrow them.

// Hold statuses. A hold is active only while its status is active and its
// ExpiresAt is in the future; every other combination is terminal.
const (
	HoldStatusActive   = "active"
	HoldStatusCaptured = "captured"
	HoldStatusReleased = "released"
	HoldStatusExpired  = "expired"
)

// Validation and state errors returned by the hold APIs.
var (
	ErrEmptyHoldID           = errors.New("ledger: hold ID must not be empty")
	ErrEmptyHoldAccount      = errors.New("ledger: hold account must not be empty")
	ErrHoldNonPositiveAmount = errors.New("ledger: hold amount must be greater than zero")
	// ErrHoldMissingExpiry is returned when a hold carries no ExpiresAt.
	// Authorizations are always time-bound; an open-ended hold would
	// strand available funds forever.
	ErrHoldMissingExpiry = errors.New("ledger: hold must carry an expiry time")
	// ErrInsufficientAvailableFunds is returned by Hold when the account's
	// available balance (net balance minus active holds) cannot cover the
	// hold amount. It is a 422-class semantic rejection: the request is
	// well-formed, the funds just are not there.
	ErrInsufficientAvailableFunds = errors.New("ledger: insufficient available funds for hold")
	// ErrHoldNotFound is returned by Release and Capture for an unknown
	// hold ID.
	ErrHoldNotFound = errors.New("ledger: hold not found")
	// ErrHoldNotActive is returned by Capture when the hold is already
	// captured or released. Captures are single-shot: one capture
	// consumes the hold.
	ErrHoldNotActive = errors.New("ledger: hold is not active")
	// ErrHoldExpired is returned by Capture when the hold's ExpiresAt has
	// passed. Expired authorizations cannot be settled; the merchant must
	// re-authorize.
	ErrHoldExpired = errors.New("ledger: hold has expired")
	// ErrCaptureExceedsHold is returned by Capture when the capture
	// amount is larger than the held amount.
	ErrCaptureExceedsHold       = errors.New("ledger: capture amount exceeds held amount")
	ErrEmptyCaptureID           = errors.New("ledger: capture ID must not be empty")
	ErrEmptyCaptureHoldID       = errors.New("ledger: capture hold ID must not be empty")
	ErrEmptyCaptureToAccount    = errors.New("ledger: capture payee account must not be empty")
	ErrCaptureNonPositiveAmount = errors.New("ledger: capture amount must be greater than zero")
	ErrCaptureSameAccount       = errors.New("ledger: capture payee and held account must differ")
	// ErrCaptureIDConflict is returned when the capture ID is already
	// used as a journal entry ID (captures journal their settlement
	// entry under the capture ID, so the namespaces must not collide).
	ErrCaptureIDConflict = errors.New("ledger: capture ID already used as a journal entry ID")
)

// Hold is one funds-authorization reservation on an account.
//
// Currency follows the JournalEntry convention: empty normalizes to the
// default currency, anything else must be a 3-letter uppercase ISO 4217
// code, and the hold only ever touches that currency's available balance.
// Status is one of active/captured/released/expired; ExpiresAt is
// exclusive — the hold is expired once now >= ExpiresAt.
type Hold struct {
	ID             string    `json:"id"`
	Account        AccountID `json:"account"`
	AmountCents    int64     `json:"amount_cents"`
	Currency       string    `json:"currency"`
	ExpiresAt      time.Time `json:"expires_at"`
	CreatedAt      time.Time `json:"created_at"`
	IdempotencyKey string    `json:"idempotency_key,omitempty"`
	Status         string    `json:"status"`
}

// Capture settles part of an authorization hold as a real money movement:
// AmountCents moves from the held account (the payer) to To (the payee),
// and the un-captured remainder of the hold is released automatically.
// The settlement is booked as a single double-entry journal entry under
// ID (DebitAccount = To, CreditAccount = the hold's account).
type Capture struct {
	ID             string    `json:"id"`
	HoldID         string    `json:"hold_id"`
	To             AccountID `json:"to_account"`
	AmountCents    int64     `json:"amount_cents"`
	IdempotencyKey string    `json:"idempotency_key,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// CaptureReceipt reports what Capture committed.
//
// Entry is the journaled settlement posting. ReleasedCents is the
// un-captured remainder of the hold, released back to available funds by
// this capture. Duplicate replays return the original receipt with
// Duplicate == true and book nothing new.
type CaptureReceipt struct {
	CaptureID     string       `json:"capture_id"`
	HoldID        string       `json:"hold_id"`
	Entry         JournalEntry `json:"entry"`
	CapturedCents int64        `json:"captured_cents"`
	ReleasedCents int64        `json:"released_cents"`
	Duplicate     bool         `json:"duplicate"`
}

// Hold reserves AmountCents of the account's available funds until
// ExpiresAt.
//
// Validation runs first (non-empty ID and account, positive amount, valid
// currency, non-zero ExpiresAt), then the idempotency replay check on the
// hold namespace (a replay returns the original hold with duplicate ==
// true and reserves nothing new), then the frozen check, then the
// available-funds check: the hold is rejected with
// ErrInsufficientAvailableFunds when balance - active held < amount. The
// comparison is overflow-safe (see coversCents): a required total that
// does not fit in int64 can never be covered by any balance.
//
// A hold whose ExpiresAt is already in the past is accepted but born
// expired: it reserves nothing and counts as expired everywhere. A zero
// CreatedAt is filled with the current time.
//
// The commit is atomic under the write lock: the hold row, its
// idempotency index entry, and the per-account hold index land together
// after all checks passed. Holds bump neither the ledger version nor the
// audit chain — they are off-journal reservations, not postings.
func (l *Ledger) Hold(h Hold) (held Hold, duplicate bool, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if h.ID == "" {
		return Hold{}, false, ErrEmptyHoldID
	}
	if h.Account == "" {
		return Hold{}, false, ErrEmptyHoldAccount
	}
	if h.AmountCents <= 0 {
		return Hold{}, false, ErrHoldNonPositiveAmount
	}
	currency, err := normalizeCurrency(h.Currency)
	if err != nil {
		return Hold{}, false, err
	}
	h.Currency = currency
	if h.ExpiresAt.IsZero() {
		return Hold{}, false, ErrHoldMissingExpiry
	}

	if h.IdempotencyKey != "" {
		if id, ok := l.holdKeys[h.IdempotencyKey]; ok {
			return l.holds[id], true, nil
		}
	}

	// A frozen account cannot take on new reservations. The check runs
	// after the replay check for the same reason as in Post: a replay
	// reserves nothing new, so it must not fail on a freeze that landed
	// after the original hold.
	if l.frozenLocked(h.Account) {
		return Hold{}, false, ErrAccountFrozen
	}

	// Holds consume available funds, not raw balance: balance - active
	// held must cover the amount. For overdraft-protected accounts this
	// check also guards the zero floor (their balance can never go
	// below zero), so no separate overdraft check is needed here.
	now := time.Now()
	key := accountCurrency{account: h.Account, currency: h.Currency}
	if !coversCents(l.balances[key], l.heldLocked(h.Account, h.Currency, now), h.AmountCents) {
		return Hold{}, false, ErrInsufficientAvailableFunds
	}

	if h.CreatedAt.IsZero() {
		h.CreatedAt = now
	}
	h.Status = HoldStatusActive

	l.maybePruneIdempotencyKeys(now)
	l.holds[h.ID] = h
	l.holdsByAccount[h.Account] = append(l.holdsByAccount[h.Account], h.ID)
	if h.IdempotencyKey != "" {
		l.holdKeys[h.IdempotencyKey] = h.ID
	}
	l.emitAudit(AuditEvent{
		Op:            "hold",
		Actor:         "Hold",
		TraceID:       h.ID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{h.Account},
		Details: map[string]any{
			"amount_cents": h.AmountCents,
			"currency":     h.Currency,
			"expires_at":   h.ExpiresAt.UTC().Format(time.RFC3339),
		},
	})

	return h, false, nil
}

// coversCents reports whether balance covers need on top of already
// reserved cents: balance >= reserved + need. The sum is computed with
// addCents, so a required total that overflows int64 reports false — no
// int64 balance could cover it — instead of wrapping around.
func coversCents(balance, reserved, need int64) bool {
	total, ok := addCents(reserved, need)
	if !ok {
		return false
	}
	return balance >= total
}

// activeHoldLocked reports whether the hold is currently active: its
// status is active and its ExpiresAt is still in the future. Expired
// holds count as inactive even before ExpireHolds sweeps them — expiry is
// lazy by predicate, the sweep only marks the rows for observability.
// Callers must hold l.mu.
func activeHoldLocked(h Hold, now time.Time) bool {
	return h.Status == HoldStatusActive && now.Before(h.ExpiresAt)
}

// heldLocked sums the active holds on (account, currency) at now.
// Callers must hold l.mu; the read lock suffices because nothing is
// mutated.
func (l *Ledger) heldLocked(a AccountID, currency string, now time.Time) int64 {
	var total int64
	for _, id := range l.holdsByAccount[a] {
		if h := l.holds[id]; h.Currency == currency && activeHoldLocked(h, now) {
			total += h.AmountCents
		}
	}
	return total
}

// AvailableIn returns the account's available funds (in cents) in the
// given currency: net balance minus active authorization holds. Expired
// holds count as inactive even before the ExpireHolds sweep. An empty
// currency means the default currency. Unknown accounts report zero.
func (l *Ledger) AvailableIn(a AccountID, currency string) int64 {
	if currency == "" {
		currency = DefaultCurrency
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	now := time.Now()
	return l.balances[accountCurrency{account: a, currency: currency}] - l.heldLocked(a, currency, now)
}

// Available returns the account's available funds (in cents) in the
// default currency: net balance minus active authorization holds. See
// AvailableIn.
func (l *Ledger) Available(a AccountID) int64 {
	return l.AvailableIn(a, DefaultCurrency)
}

// GetHold returns the hold with the given ID, or false when no such hold
// exists. The returned hold's Status reflects its lifecycle state; use
// ExpireHolds to mark expired holds explicitly (reads treat them as
// expired regardless).
func (l *Ledger) GetHold(id string) (Hold, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	h, ok := l.holds[id]
	return h, ok
}

// Release drops the hold with the given ID without settling anything,
// returning its reserved funds to available. Release is idempotent: a
// hold that is already released or expired returns as-is with no error,
// and an unknown ID fails with ErrHoldNotFound. A frozen account may
// still release — releasing frees funds rather than moving money.
func (l *Ledger) Release(id string) (Hold, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	h, ok := l.holds[id]
	if !ok {
		return Hold{}, ErrHoldNotFound
	}
	if h.Status == HoldStatusReleased || h.Status == HoldStatusExpired {
		return h, nil
	}
	if h.Status == HoldStatusCaptured {
		return h, nil
	}
	h.Status = HoldStatusReleased
	l.holds[id] = h
	// Only an actual state transition is audited: replays and releases
	// of already-terminal holds book nothing and stay silent.
	l.emitAudit(AuditEvent{
		Op:            "hold_release",
		Actor:         "Release",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{h.Account},
		Details: map[string]any{
			"amount_cents": h.AmountCents,
			"currency":     h.Currency,
		},
	})
	return h, nil
}

// ExpireHolds marks every hold whose ExpiresAt has passed as expired and
// returns how many were marked. Expired holds were already inactive for
// available-balance purposes (expiry is lazy by predicate); the sweep
// exists so operators and the reconcile report can observe which holds
// lapsed. It never touches active, captured, or released holds, and it
// does not bump the ledger version.
func (l *Ledger) ExpireHolds() int {
	return l.ExpireHoldsAt(time.Now())
}

// ExpireHoldsAt is ExpireHolds with a caller-supplied timestamp, so tests
// can pin expiry deterministically.
func (l *Ledger) ExpireHoldsAt(now time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	marked := 0
	for id, h := range l.holds {
		if h.Status == HoldStatusActive && !now.Before(h.ExpiresAt) {
			h.Status = HoldStatusExpired
			l.holds[id] = h
			marked++
		}
	}
	// A sweep that expires nothing emits no event: the background
	// sweeper ticks on a timer, and zero-expire ticks would drown the
	// audit trail in noise.
	if marked > 0 {
		l.emitAudit(AuditEvent{
			Op:            "hold_expire",
			Actor:         "ExpireHolds",
			TraceID:       fmt.Sprintf("hold-expire@%d", l.version),
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Details: map[string]any{
				"expired": marked,
			},
		})
	}
	return marked
}

// Capture settles up to the held amount as a real money movement and
// consumes the hold.
//
// The risk checks mirror Post and run in the same order: field validation
// first, then the idempotency replay check on the capture namespace (a
// replay returns the original receipt with Duplicate == true and books
// nothing new — the replay succeeds even if the hold has since been
// frozen), then the hold lookup and state checks (unknown -> ErrHoldNotFound,
// captured/released -> ErrHoldNotActive, expired -> ErrHoldExpired, amount
// above the held amount -> ErrCaptureExceedsHold), then the capture-ID
// conflict check, then the frozen check on both settlement legs, then the
// overdraft check on the held (payer) account.
//
// The commit is atomic under the write lock: the settlement journal entry
// (DebitAccount = To, CreditAccount = the hold's account, in the hold's
// currency — captures never cross currencies), the hold's transition to
// captured, and the capture idempotency index entry land together. The
// un-captured remainder (held - captured) is released implicitly: a
// captured hold no longer counts toward available funds. Unlike Hold and
// Release, Capture journals a real posting, so it bumps the ledger
// version and appends one audit-chain link, exactly like Post.
//
// Note the reservation is advisory: a posting concurrent with the hold
// can move the held account's balance before the capture lands. The
// capture is an ordinary double-entry posting subject to the same frozen
// and overdraft checks as Post — it does not re-verify that the held
// amount is still covered by available funds.
func (l *Ledger) Capture(c Capture) (CaptureReceipt, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if c.ID == "" {
		return CaptureReceipt{}, ErrEmptyCaptureID
	}
	if c.HoldID == "" {
		return CaptureReceipt{}, ErrEmptyCaptureHoldID
	}
	if c.To == "" {
		return CaptureReceipt{}, ErrEmptyCaptureToAccount
	}
	if c.AmountCents <= 0 {
		return CaptureReceipt{}, ErrCaptureNonPositiveAmount
	}

	if c.IdempotencyKey != "" {
		if r, ok := l.captureKeys[c.IdempotencyKey]; ok {
			r.Duplicate = true
			return r, nil
		}
	}

	h, ok := l.holds[c.HoldID]
	if !ok {
		return CaptureReceipt{}, ErrHoldNotFound
	}
	now := time.Now()
	if h.Status == HoldStatusCaptured || h.Status == HoldStatusReleased {
		return CaptureReceipt{}, ErrHoldNotActive
	}
	if !now.Before(h.ExpiresAt) {
		return CaptureReceipt{}, ErrHoldExpired
	}
	if c.To == h.Account {
		return CaptureReceipt{}, ErrCaptureSameAccount
	}
	if c.AmountCents > h.AmountCents {
		return CaptureReceipt{}, ErrCaptureExceedsHold
	}
	if _, exists := l.entries[c.ID]; exists {
		return CaptureReceipt{}, ErrCaptureIDConflict
	}

	if l.frozenLocked(h.Account) || l.frozenLocked(c.To) {
		return CaptureReceipt{}, ErrAccountFrozen
	}

	entry := JournalEntry{
		ID:            c.ID,
		DebitAccount:  c.To,
		CreditAccount: h.Account,
		AmountCents:   c.AmountCents,
		Currency:      h.Currency,
		CreatedAt:     c.CreatedAt,
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = now
	}
	// Overdraft protection guards the payer's settlement posting, exactly
	// like Post: a capture that would take a protected held account below
	// zero is rejected. The comparison never subtracts, so it cannot
	// overflow (see overdraft.go).
	if l.overdraftRejectedLocked(entry) {
		return CaptureReceipt{}, ErrAccountOverdraft
	}

	// The period gate is keyed on the capture entry's final timestamp
	// (see period.go): a capture posting into a closed accounting period
	// is rejected with ErrPeriodClosed. It runs after the idempotency
	// replay check above, so replaying a capture key posted before the
	// period closed returns the original receipt.
	if err := l.periodRejectedLocked(entry.CreatedAt); err != nil {
		return CaptureReceipt{}, err
	}

	l.maybePruneIdempotencyKeys(now)
	versionBefore := l.version
	// Pre-commit balance snapshot for the opt-in balance-change hook
	// (see balance_hook.go): nil when no hook is registered.
	hookTouched := touchedAccounts([]JournalEntry{entry})
	hookOld := l.balanceSnapshotLocked(hookTouched)
	l.commitEntryLocked(entry)
	h.Status = HoldStatusCaptured
	l.holds[c.HoldID] = h

	receipt := CaptureReceipt{
		CaptureID:     c.ID,
		HoldID:        c.HoldID,
		Entry:         entry,
		CapturedCents: c.AmountCents,
		ReleasedCents: h.AmountCents - c.AmountCents,
	}
	if c.IdempotencyKey != "" {
		l.captureKeys[c.IdempotencyKey] = receipt
	}
	// Low-balance alert evaluation: strictly after the atomic commit
	// zone, read-only (see low_balance.go). Advisory only.
	l.evaluateLowBalanceLocked(touchedAccounts([]JournalEntry{entry}), c.HoldID, "Capture")
	l.emitAudit(AuditEvent{
		Op:            "hold_capture",
		Actor:         "Capture",
		TraceID:       c.HoldID,
		VersionBefore: versionBefore,
		VersionAfter:  l.version,
		EntryIDs:      []string{entry.ID},
		Accounts:      []AccountID{h.Account, c.To},
		Details: map[string]any{
			"captured_cents": entry.AmountCents,
			"released_cents": receipt.ReleasedCents,
			"currency":       h.Currency,
		},
	})
	// Balance-change notification: strictly after the atomic commit
	// zone, advisory only — async dispatch, hook failures isolated.
	l.fireBalanceHooksLocked(hookTouched, hookOld, c.HoldID)

	return receipt, nil
}

// pruneHoldKeysLocked evicts hold and capture idempotency keys older than
// the TTL, mirroring the entry-level key expiry: after the TTL, reposting
// a hold or capture key books a brand-new operation. The holds and the
// capture journal entries themselves are untouched — only the
// replay-detection indexes shrink. A non-positive TTL disables pruning.
// Callers must hold l.mu (the write lock; the sweep mutates the indexes).
func (l *Ledger) pruneHoldKeysLocked(now time.Time) int {
	if l.idempotencyTTL <= 0 {
		return 0
	}
	cutoff := now.Add(-l.idempotencyTTL)
	removed := 0
	for key, id := range l.holdKeys {
		if h := l.holds[id]; h.CreatedAt.Before(cutoff) {
			delete(l.holdKeys, key)
			removed++
		}
	}
	for key, r := range l.captureKeys {
		if r.Entry.CreatedAt.Before(cutoff) {
			delete(l.captureKeys, key)
			removed++
		}
	}
	return removed
}
