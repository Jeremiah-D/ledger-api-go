package ledger

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Pre-authorizations on direct-debit mandates (LG-192).
//
// AuthorizePreAuth reserves a slice of a mandate's pull limit *before*
// the money moves: the fintech pre-authorization (auth/capture) flow for
// subscription billing — the merchant checks the customer can pay, the
// issuer guarantees the funds, and the capture settles later. This is
// deliberately *not* a hold (see hold.go): holds reserve an account's
// available balance against arbitrary postings, while a pre-authorization
// is an authorization-domain reservation against the *mandate's*
// frequency window and per-debit cap. The two designs stay orthogonal:
// pre-authorizations never touch balances, available funds, or the
// journal — they are pure mandate-limit occupancy.
//
// Lifecycle: active -> captured | released | expired.
//
//   - AuthorizePreAuth freezes AmountCents inside the mandate's limits:
//     the mandate must be active, the amount must fit the per-debit cap,
//     the currency must match, and the mandate's frequency window must be
//     free — counting *both* posted debits and other active
//     pre-authorizations (see mandateWindowAuthorizationCountLocked), so
//     a pre-authorization and a direct debit can never race for the same
//     window.
//   - CapturePreAuth converts the reservation into a real debit: the
//     capture posts through the exact same path as a direct debit
//     (debitMandateCoreLocked with the mandate gates re-run at the
//     capture timestamp, the preauth itself excluded from the frequency
//     gate, the mandate_of link and memo stamp intact), plus a
//     "preauth_of:<preauth ID>" memo stamp for traceability. The full
//     Post validation sequence runs inside, so a capture can never dodge
//     frozen/overdraft/budget/period controls. Captures are single-shot
//     full captures — no partial capture (unlike hold partials, see
//     hold_partial.go): a merchant wanting two settlements issues two
//     pre-authorizations.
//   - ReleasePreAuth drops the reservation without settling anything;
//     it is idempotent on already-terminal pre-authorizations.
//   - Expiry is lazy by predicate (active && now >= ExpiresAt) plus an
//     explicit ExpirePreAuths sweep for observability, like holds and
//     mandates.
//
// Bookkeeping convention: pre-authorizations are structural, like
// mandates and holds — authorizing, releasing, or expiring one never
// bumps the journal version and never enters the entry chain. The
// capture's *debit* does (it is a journaled posting). Every lifecycle
// move enters the structured audit chain, and each authorization,
// capture, release, and expiry emits a domain outbox envelope. Like
// holds, pre-authorizations are in-memory operational state: they do
// not round-trip through disaster-recovery snapshots — an authorization
// never booked is nothing to reconcile, and its expiry is time-bound
// anyway.

// Pre-authorization statuses. A pre-authorization reserves mandate
// limit only while active and unexpired; every other combination is
// terminal.
const (
	PreAuthStatusActive   = "active"
	PreAuthStatusCaptured = "captured"
	PreAuthStatusReleased = "released"
	PreAuthStatusExpired  = "expired"
)

// Validation and state errors returned by the pre-authorization APIs.
var (
	// ErrEmptyPreAuthID is returned by AuthorizePreAuth when the
	// pre-authorization ID is empty. It is a 400-class error.
	ErrEmptyPreAuthID = errors.New("ledger: pre-authorization ID must not be empty")
	// ErrPreAuthIDWhitespace is returned by AuthorizePreAuth when the
	// ID contains whitespace: the ID round-trips through the
	// "preauth_of:<id>" memo stamp, which is parsed by suffix — an ID
	// with whitespace could not be recovered unambiguously.
	ErrPreAuthIDWhitespace = errors.New("ledger: pre-authorization ID must not contain whitespace")
	// ErrEmptyPreAuthMandateID is returned by AuthorizePreAuth when no
	// mandate is named. It is a 400-class error.
	ErrEmptyPreAuthMandateID = errors.New("ledger: pre-authorization mandate ID must not be empty")
	// ErrPreAuthIDConflict is returned by AuthorizePreAuth when the ID
	// is already used by another pre-authorization. It is a 409-class
	// conflict.
	ErrPreAuthIDConflict = errors.New("ledger: pre-authorization ID already in use")
	// ErrPreAuthNonPositiveAmount is returned by AuthorizePreAuth when
	// the reserved amount is not positive. It is a 400-class error.
	ErrPreAuthNonPositiveAmount = errors.New("ledger: pre-authorization amount must be greater than zero")
	// ErrPreAuthMissingExpiry is returned by AuthorizePreAuth when the
	// pre-authorization carries no ExpiresAt. Authorizations are always
	// time-bound; an open-ended reservation would strand mandate limit
	// forever.
	ErrPreAuthMissingExpiry = errors.New("ledger: pre-authorization must carry an expiry time")
	// ErrPreAuthNotFound is returned for an unknown pre-authorization
	// ID, or when the named pre-authorization belongs to a different
	// mandate than the route's. It is a 404-class error.
	ErrPreAuthNotFound = errors.New("ledger: pre-authorization not found")
	// ErrPreAuthNotActive is returned by CapturePreAuth when the
	// pre-authorization is already captured or released. Captures are
	// single-shot: one capture consumes the pre-authorization.
	ErrPreAuthNotActive = errors.New("ledger: pre-authorization is not active")
	// ErrPreAuthExpired is returned by CapturePreAuth when the
	// pre-authorization's ExpiresAt has passed. Expired authorizations
	// cannot be settled; the merchant must re-authorize.
	ErrPreAuthExpired = errors.New("ledger: pre-authorization has expired")
	// ErrEmptyPreAuthCaptureID is returned by CapturePreAuth when the
	// capture ID is empty. It is a 400-class error.
	ErrEmptyPreAuthCaptureID = errors.New("ledger: pre-authorization capture ID must not be empty")
	// ErrPreAuthCaptureIDConflict is returned by CapturePreAuth when
	// the capture ID is already used as a journal entry ID (captures
	// journal their settlement entry under the capture ID, so the
	// namespaces must not collide).
	ErrPreAuthCaptureIDConflict = errors.New("ledger: pre-authorization capture ID already used as a journal entry ID")
)

// PreAuth is one mandate-limit reservation: the pre-authorization for a
// future direct-debit pull under MandateID.
//
// Currency follows the JournalEntry convention: empty normalizes to the
// default currency, anything else must be a 3-letter uppercase ISO 4217
// code — and it must equal the mandate's currency. Status is one of
// active/captured/released/expired; ExpiresAt is exclusive — the
// pre-authorization is expired once now >= ExpiresAt.
type PreAuth struct {
	ID             string    `json:"id"`
	MandateID      string    `json:"mandate_id"`
	AmountCents    int64     `json:"amount_cents"`
	Currency       string    `json:"currency"`
	ExpiresAt      time.Time `json:"expires_at"`
	CreatedAt      time.Time `json:"created_at"`
	IdempotencyKey string    `json:"idempotency_key,omitempty"`
	Status         string    `json:"status"`
}

// PreAuthCapture settles a pre-authorization as a real direct-debit
// posting: AmountCents is the pre-authorization's full amount —
// captures are single-shot and always settle the whole reservation
// (see the package doc). The settlement is booked as a journal entry
// under ID through the mandate debit path (debit leg = the mandate's
// payee, credit leg = the mandate's payer).
type PreAuthCapture struct {
	ID             string            `json:"id"`
	PreAuthID      string            `json:"preauth_id"`
	IdempotencyKey string            `json:"idempotency_key,omitempty"`
	ExternalRef    string            `json:"external_ref,omitempty"`
	Attachments    []string          `json:"attachments,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
	CreatedAt      time.Time         `json:"created_at,omitempty"`
}

// PreAuthCaptureReceipt reports what CapturePreAuth committed.
//
// Entry is the journaled direct-debit posting (debit leg = the mandate's
// payee, credit leg = the mandate's payer, memo stamped
// "preauth_of:<preauth ID> mandate_of:<mandate ID>"). Duplicate replays
// return the original receipt with Duplicate == true and book nothing
// new.
type PreAuthCaptureReceipt struct {
	CaptureID   string       `json:"capture_id"`
	PreAuthID   string       `json:"preauth_id"`
	MandateID   string       `json:"mandate_id"`
	Entry       JournalEntry `json:"entry"`
	AmountCents int64        `json:"amount_cents"`
	Duplicate   bool         `json:"duplicate"`
}

// PreAuthReport is one pre-authorization row in the reconcile report:
// the reservation with its effective status (lazy expiry applied) and
// its expiry time.
type PreAuthReport struct {
	ID          string    `json:"id"`
	MandateID   string    `json:"mandate_id"`
	AmountCents int64     `json:"amount_cents"`
	Currency    string    `json:"currency"`
	Status      string    `json:"status"`
	ExpiresAt   time.Time `json:"expires_at"`
	CreatedAt   time.Time `json:"created_at"`
}

// preauthMemoPrefix is the memo stamp on the capture's debit entry: the
// "preauth_of:" prefix plus the pre-authorization ID. It precedes the
// mandate_of stamp (see stampMandateMemo): mandate_of stays the memo's
// suffix so mandateIDFromMemo keeps parsing unambiguously, and the
// preauth stamp is searchable through GET /entries?memo=preauth_of:.
const preauthMemoPrefix = "preauth_of:"

// preauthMemoFor returns the memo stamp for a pre-authorization
// capture.
func preauthMemoFor(preauthID string) string {
	return preauthMemoPrefix + preauthID
}

// preauthExpiredLocked reports whether the pre-authorization counts as
// expired at now: its status is still active (terminal states stay
// terminal) and its ExpiresAt has passed. Expiry is lazy by predicate —
// the sweep only marks the rows for observability. Callers must hold
// l.mu.
func preauthExpiredLocked(p PreAuth, now time.Time) bool {
	return p.Status == PreAuthStatusActive && !now.Before(p.ExpiresAt)
}

// preauthEffectiveStatusLocked returns the status reads disclose: the
// stored status, except an active pre-authorization past its ExpiresAt
// reads as expired. The stored row is untouched — only ExpirePreAuths
// persists the transition. Callers must hold l.mu.
func preauthEffectiveStatusLocked(p PreAuth, now time.Time) string {
	if preauthExpiredLocked(p, now) {
		return PreAuthStatusExpired
	}
	return p.Status
}

// preauthWithEffectiveStatus returns a copy of p with Status set to the
// effective status at now. Callers must hold l.mu.
func preauthWithEffectiveStatus(p PreAuth, now time.Time) PreAuth {
	p.Status = preauthEffectiveStatusLocked(p, now)
	return p
}

// AuthorizePreAuth freezes AmountCents of the mandate's pull limit
// without posting anything: the pre-authorization reservation.
//
// Validation runs first (non-empty whitespace-free ID and mandate ID,
// positive amount, valid currency, non-zero ExpiresAt), then the
// idempotency replay check on the pre-authorization namespace (a replay
// returns the original pre-authorization with duplicate == true and
// reserves nothing new), then the mandate gates evaluated at now: the
// mandate must exist and be active, its validity window must cover now,
// the amount must fit the per-debit cap (ErrMandateAmountExceeded),
// the currency must match the mandate's (ErrMandateCurrencyMismatch),
// and the mandate's frequency window must be free of *both* posted
// debits and other active pre-authorizations
// (ErrMandateFrequencyExceeded) — the reservation occupies the window
// exactly like a debit would.
//
// Authorizing an already-expired ExpiresAt is accepted but born expired:
// it reserves nothing and counts as expired everywhere (the same
// convention as Hold). A zero CreatedAt is filled with the current
// time.
//
// The commit is atomic under the write lock: the pre-authorization row,
// the per-mandate index row, and the idempotency index entry land
// together after all checks passed. Authorizing bumps neither the
// ledger version nor the entry chain — pre-authorizations are
// mandate-limit reservations, not postings — but it is audited as
// "preauth_authorized" and emits a preauth_authorized outbox envelope.
func (l *Ledger) AuthorizePreAuth(p PreAuth) (PreAuth, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.authorizePreAuthLocked(p, time.Now())
}

// authorizePreAuthLocked is the lock-held implementation of
// AuthorizePreAuth: see its doc comment for the full contract. Callers
// must hold the write lock; now is the authorization timestamp (the
// CreatedAt default and the mandate-gate evaluation point), so tests
// can pin it.
func (l *Ledger) authorizePreAuthLocked(p PreAuth, now time.Time) (PreAuth, bool, error) {
	if p.ID == "" {
		return PreAuth{}, false, ErrEmptyPreAuthID
	}
	if strings.ContainsAny(p.ID, " \t\n\r") {
		return PreAuth{}, false, ErrPreAuthIDWhitespace
	}
	if p.MandateID == "" {
		return PreAuth{}, false, ErrEmptyPreAuthMandateID
	}
	if p.AmountCents <= 0 {
		return PreAuth{}, false, ErrPreAuthNonPositiveAmount
	}
	currency, err := normalizeCurrency(p.Currency)
	if err != nil {
		return PreAuth{}, false, err
	}
	p.Currency = currency
	if p.ExpiresAt.IsZero() {
		return PreAuth{}, false, ErrPreAuthMissingExpiry
	}
	if _, ok := l.preauths[p.ID]; ok {
		return PreAuth{}, false, fmt.Errorf("%w: pre-authorization %q", ErrPreAuthIDConflict, p.ID)
	}

	if p.IdempotencyKey != "" {
		if id, ok := l.preauthKeys[p.IdempotencyKey]; ok {
			return preauthWithEffectiveStatus(l.preauths[id], now), true, nil
		}
	}

	// The mandate gates: the mandate must exist and authorize pulls at
	// the authorization timestamp. Expired timed freezes release lazily
	// before the frozen gate below, like in Post (see FreezeUntil).
	m, ok := l.mandates[p.MandateID]
	if !ok {
		return PreAuth{}, false, ErrMandateNotFound
	}
	if m.Status != MandateStatusActive {
		return PreAuth{}, false, fmt.Errorf("%w: status %q", ErrMandateNotActive, m.Status)
	}
	if !now.Before(m.ValidUntil) {
		return PreAuth{}, false, ErrMandateExpired
	}
	if now.Before(m.ValidFrom) {
		return PreAuth{}, false, fmt.Errorf("%w: mandate valid from %s", ErrMandateNotActive, m.ValidFrom.UTC().Format(time.RFC3339))
	}
	if p.AmountCents > m.MaxAmountCents {
		return PreAuth{}, false, fmt.Errorf("%w: %d > %d", ErrMandateAmountExceeded, p.AmountCents, m.MaxAmountCents)
	}
	if p.Currency != m.Currency {
		return PreAuth{}, false, fmt.Errorf("%w: %q != %q", ErrMandateCurrencyMismatch, p.Currency, m.Currency)
	}
	if l.mandateWindowAuthorizationCountLocked(p.MandateID, now, m.Frequency, "") > 0 {
		return PreAuth{}, false, fmt.Errorf("%w: %s", ErrMandateFrequencyExceeded, m.Frequency)
	}

	l.releaseExpiredFreezesLocked(now, "write_path")
	if l.closedLocked(m.Payer) || l.closedLocked(m.Payee) {
		return PreAuth{}, false, ErrAccountClosed
	}
	if l.frozenLocked(m.Payer) || l.frozenLocked(m.Payee) {
		return PreAuth{}, false, ErrAccountFrozen
	}
	if l.suspendedLocked(m.Payer) || l.suspendedLocked(m.Payee) {
		return PreAuth{}, false, ErrAccountSuspended
	}

	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	p.Status = PreAuthStatusActive

	l.maybePruneIdempotencyKeys(now)
	l.preauths[p.ID] = p
	l.preauthsByMandate[p.MandateID] = append(l.preauthsByMandate[p.MandateID], p.ID)
	if p.IdempotencyKey != "" {
		l.preauthKeys[p.IdempotencyKey] = p.ID
	}
	l.preauthAuthorizedChanges.Add(1)
	l.emitAudit(AuditEvent{
		Op:            "preauth_authorized",
		Actor:         "AuthorizePreAuth",
		TraceID:       p.ID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{m.Payer, m.Payee},
		Details: map[string]any{
			"mandate_id":   p.MandateID,
			"amount_cents": p.AmountCents,
			"currency":     p.Currency,
			"expires_at":   p.ExpiresAt.UTC().Format(time.RFC3339),
		},
	})
	l.emitOutboxLocked(OutboxEvent{
		Type:        OutboxPreAuthAuthorized,
		Account:     m.Payer,
		AmountCents: p.AmountCents,
		Currency:    p.Currency,
		Details: map[string]any{
			"preauth_id": p.ID,
			"mandate_id": p.MandateID,
			"expires_at": p.ExpiresAt.UTC().Format(time.RFC3339),
		},
	})
	return p, false, nil
}

// CapturePreAuth converts an active pre-authorization into a real
// direct-debit posting: the auth/capture settlement.
//
// The check order mirrors Capture (see hold.go): field validation
// first, then the pre-authorization lookup (unknown, or belonging to a
// different mandate than the route's, fails with ErrPreAuthNotFound),
// then the idempotency replay check on the pre-authorization-capture
// namespace (a replay returns the original receipt with Duplicate ==
// true and books nothing new), then the pre-authorization state checks
// (captured/released -> ErrPreAuthNotActive, expired ->
// ErrPreAuthExpired), then the capture-ID conflict check. The
// settlement then posts through debitMandateCoreLocked — the exact same
// path as a direct debit — so the mandate gates are re-run at the
// capture timestamp (the mandate must still authorize the pull: a
// mandate cancelled or expired after the authorization refuses the
// capture, like a card scheme refuses settlement after the auth
// window), the frequency gate excludes the pre-authorization being
// captured (its own reservation does not block its own settlement), and
// the full Post validation sequence (frozen, overdraft, daily limit,
// budget, period, journal rules) guards the booking. The debit memo
// carries both stamps — "preauth_of:<preauth ID>" ahead of
// "mandate_of:<mandate ID>" — and the mandate_of entry link is
// declared, so the capture is indistinguishable from a direct debit in
// every index.
//
// The commit is atomic under the write lock: the journaled debit, the
// pre-authorization's transition to captured, and the capture
// idempotency index entry land together. Only the debit bumps the
// ledger version and appends a chain link; the state transition itself
// is audited as "preauth_captured" with a preauth_captured outbox
// envelope.
func (l *Ledger) CapturePreAuth(mandateID string, c PreAuthCapture) (PreAuthCaptureReceipt, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if c.ID == "" {
		return PreAuthCaptureReceipt{}, ErrEmptyPreAuthCaptureID
	}
	if c.PreAuthID == "" {
		return PreAuthCaptureReceipt{}, ErrEmptyPreAuthID
	}
	if c.IdempotencyKey != "" {
		if r, ok := l.preauthCaptureKeys[c.IdempotencyKey]; ok {
			r.Duplicate = true
			return r, nil
		}
	}

	now := time.Now()
	p, ok := l.preauths[c.PreAuthID]
	if !ok || p.MandateID != mandateID {
		return PreAuthCaptureReceipt{}, ErrPreAuthNotFound
	}
	if p.Status == PreAuthStatusCaptured || p.Status == PreAuthStatusReleased {
		return PreAuthCaptureReceipt{}, ErrPreAuthNotActive
	}
	if !now.Before(p.ExpiresAt) {
		return PreAuthCaptureReceipt{}, ErrPreAuthExpired
	}
	if _, exists := l.entries[c.ID]; exists {
		return PreAuthCaptureReceipt{}, fmt.Errorf("%w: %q", ErrPreAuthCaptureIDConflict, c.ID)
	}

	// The settlement goes through the mandate debit core — the same
	// authorization gates and the same full Post validation sequence as
	// a direct debit. The pre-authorization's own reservation is
	// excluded from the frequency gate (it is being converted, not
	// competing). The debit memo carries the preauth stamp ahead of the
	// mandate stamp: stampMandateMemo appends "mandate_of:<mandate ID>"
	// after the caller memo, so mandate_of stays the suffix and
	// mandateIDFromMemo keeps parsing unambiguously.
	ts := c.CreatedAt
	if ts.IsZero() {
		ts = now
	}
	debit := MandateDebit{
		ID:          c.ID,
		AmountCents: p.AmountCents,
		Currency:    p.Currency,
		Memo:        preauthMemoFor(p.ID),
		ExternalRef: c.ExternalRef,
		Attachments: c.Attachments,
		Metadata:    c.Metadata,
		CreatedAt:   ts,
	}
	l.maybePruneIdempotencyKeys(now)
	receipt, err := l.debitMandateCoreLockedExcluding(mandateID, debit, p.ID)
	if err != nil {
		return PreAuthCaptureReceipt{}, err
	}

	p.Status = PreAuthStatusCaptured
	l.preauths[p.ID] = p
	captureReceipt := PreAuthCaptureReceipt{
		CaptureID:   c.ID,
		PreAuthID:   p.ID,
		MandateID:   mandateID,
		Entry:       receipt.Entry,
		AmountCents: p.AmountCents,
	}
	if c.IdempotencyKey != "" {
		l.preauthCaptureKeys[c.IdempotencyKey] = captureReceipt
	}
	l.preauthCapturedChanges.Add(1)
	l.emitAudit(AuditEvent{
		Op:            "preauth_captured",
		Actor:         "CapturePreAuth",
		TraceID:       p.ID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		EntryIDs:      []string{c.ID},
		Accounts:      []AccountID{receipt.Entry.CreditAccount, receipt.Entry.DebitAccount},
		Details: map[string]any{
			"mandate_id":   mandateID,
			"amount_cents": p.AmountCents,
			"currency":     p.Currency,
		},
	})
	l.emitOutboxLocked(OutboxEvent{
		Type:        OutboxPreAuthCaptured,
		Account:     receipt.Entry.CreditAccount,
		EntryID:     c.ID,
		AmountCents: p.AmountCents,
		Currency:    p.Currency,
		Details: map[string]any{
			"preauth_id": p.ID,
			"mandate_id": mandateID,
		},
	})
	return captureReceipt, nil
}

// ReleasePreAuth drops the pre-authorization with the given ID without
// settling anything, returning its reserved mandate limit. Release is
// idempotent: a pre-authorization that is already released or expired
// returns as-is with no error. An unknown ID, or a pre-authorization
// under a different mandate than mandateID, fails with ErrPreAuthNotFound.
// A frozen account may still release — releasing frees reserved limit
// rather than moving money (the same exemption as Hold's Release).
func (l *Ledger) ReleasePreAuth(mandateID, id string) (PreAuth, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	p, ok := l.preauths[id]
	if !ok || p.MandateID != mandateID {
		return PreAuth{}, ErrPreAuthNotFound
	}
	if p.Status == PreAuthStatusReleased || p.Status == PreAuthStatusExpired {
		return preauthWithEffectiveStatus(p, time.Now()), nil
	}
	if p.Status == PreAuthStatusCaptured {
		return p, nil
	}
	p.Status = PreAuthStatusReleased
	l.preauths[id] = p
	// Only an actual state transition is audited: replays and releases
	// of already-terminal pre-authorizations book nothing and stay
	// silent.
	m := l.mandates[p.MandateID]
	l.preauthReleasedChanges.Add(1)
	l.emitAudit(AuditEvent{
		Op:            "preauth_release",
		Actor:         "ReleasePreAuth",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{m.Payer, m.Payee},
		Details: map[string]any{
			"mandate_id":   p.MandateID,
			"amount_cents": p.AmountCents,
			"currency":     p.Currency,
		},
	})
	l.emitOutboxLocked(OutboxEvent{
		Type:        OutboxPreAuthReleased,
		Account:     m.Payer,
		AmountCents: p.AmountCents,
		Currency:    p.Currency,
		Details:     map[string]any{"preauth_id": id, "mandate_id": p.MandateID},
	})
	return p, nil
}

// ExpirePreAuths marks every pre-authorization whose ExpiresAt has
// passed as expired and returns how many were marked. Expired
// pre-authorizations were already dead for reservation purposes
// (expiry is lazy by predicate); the sweep exists so operators and the
// reconcile report can observe which authorizations lapsed. It never
// touches captured or released pre-authorizations, and it does not bump
// the ledger version.
func (l *Ledger) ExpirePreAuths() int {
	return l.ExpirePreAuthsAt(time.Now())
}

// ExpirePreAuthsAt is ExpirePreAuths with a caller-supplied timestamp,
// so tests can pin expiry deterministically.
func (l *Ledger) ExpirePreAuthsAt(now time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	marked := 0
	var expired []PreAuth
	for id, p := range l.preauths {
		if preauthExpiredLocked(p, now) {
			p.Status = PreAuthStatusExpired
			l.preauths[id] = p
			marked++
			expired = append(expired, p)
		}
	}
	// A sweep that expires nothing emits no event: the background
	// sweeper ticks on a timer, and zero-expire ticks would drown the
	// audit trail in noise.
	if marked > 0 {
		l.emitAudit(AuditEvent{
			Op:            "preauth_expire",
			Actor:         "ExpirePreAuths",
			TraceID:       fmt.Sprintf("preauth-expire@%d", l.version),
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Details: map[string]any{
				"expired": marked,
			},
		})
		// Domain outbox: unlike the batched audit event, the outbox
		// gets one preauth_expired per pre-authorization — downstream
		// consumers track authorizations individually.
		for _, p := range expired {
			l.emitOutboxLocked(OutboxEvent{
				Type:        OutboxPreAuthExpired,
				Account:     l.mandates[p.MandateID].Payer,
				AmountCents: p.AmountCents,
				Currency:    p.Currency,
				Details:     map[string]any{"preauth_id": p.ID, "mandate_id": p.MandateID},
			})
		}
	}
	return marked
}

// GetPreAuth returns the pre-authorization with the given ID, or false
// when no such pre-authorization exists. The returned Status is the
// effective status: an active pre-authorization past its ExpiresAt reads
// as expired even before ExpirePreAuths marks it (see
// preauthEffectiveStatusLocked).
func (l *Ledger) GetPreAuth(id string) (PreAuth, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	p, ok := l.preauths[id]
	if !ok {
		return PreAuth{}, false
	}
	return preauthWithEffectiveStatus(p, time.Now()), true
}

// ListMandatePreAuths returns every pre-authorization under the mandate
// with its effective status, sorted by ID. An unknown mandate fails
// with ErrMandateNotFound.
func (l *Ledger) ListMandatePreAuths(mandateID string) ([]PreAuth, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if _, ok := l.mandates[mandateID]; !ok {
		return nil, ErrMandateNotFound
	}
	now := time.Now()
	out := make([]PreAuth, 0, len(l.preauthsByMandate[mandateID]))
	for _, id := range l.preauthsByMandate[mandateID] {
		out = append(out, preauthWithEffectiveStatus(l.preauths[id], now))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// PreAuthCounts reports the operational pre-authorization counters: how
// many pre-authorizations were authorized, captured, and released since
// the ledger started. New operations only — idempotent replays are not
// counted. Operational metrics (surfaced on GET /metrics), not books:
// in-memory only, they do not round-trip through snapshots.
func (l *Ledger) PreAuthCounts() (authorized, captured, released uint64) {
	return l.preauthAuthorizedChanges.Load(), l.preauthCapturedChanges.Load(), l.preauthReleasedChanges.Load()
}

// preauthReportsLocked builds the reconcile report's pre-authorizations
// table: every reservation with its effective status, sorted by ID.
// Callers must hold l.mu (either lock suffices).
func (l *Ledger) preauthReportsLocked(now time.Time) []PreAuthReport {
	out := make([]PreAuthReport, 0, len(l.preauths))
	for _, p := range l.preauths {
		eff := preauthWithEffectiveStatus(p, now)
		out = append(out, PreAuthReport{
			ID:          eff.ID,
			MandateID:   eff.MandateID,
			AmountCents: eff.AmountCents,
			Currency:    eff.Currency,
			Status:      eff.Status,
			ExpiresAt:   eff.ExpiresAt,
			CreatedAt:   eff.CreatedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// prunePreAuthKeysLocked evicts pre-authorization and
// pre-authorization-capture idempotency keys older than the TTL,
// mirroring the hold-key expiry: after the TTL, reposting a key books a
// brand-new operation. The pre-authorizations and the capture journal
// entries themselves are untouched — only the replay-detection indexes
// shrink. A non-positive TTL disables pruning. Callers must hold l.mu
// (the write lock; the sweep mutates the indexes).
func (l *Ledger) prunePreAuthKeysLocked(now time.Time) int {
	if l.idempotencyTTL <= 0 {
		return 0
	}
	cutoff := now.Add(-l.idempotencyTTL)
	removed := 0
	for key, id := range l.preauthKeys {
		if p := l.preauths[id]; p.CreatedAt.Before(cutoff) {
			delete(l.preauthKeys, key)
			removed++
		}
	}
	for key, r := range l.preauthCaptureKeys {
		if r.Entry.CreatedAt.Before(cutoff) {
			delete(l.preauthCaptureKeys, key)
			removed++
		}
	}
	return removed
}

// mandateWindowPreAuthCountLocked counts the mandate's *active*
// pre-authorizations whose authorization timestamp falls in [start,
// end): the frequency window's reservation occupancy. Active means not
// captured, released, or expired at the evaluation instant (lazy expiry
// applies, like holds) — terminal pre-authorizations reserve nothing.
// excludeID skips one pre-authorization: a capture excludes the
// reservation it is converting, so the settlement is not blocked by its
// own hold on the window. Callers must hold l.mu.
func (l *Ledger) mandateWindowPreAuthCountLocked(mandateID string, start, end time.Time, excludeID string) int {
	now := time.Now()
	n := 0
	for _, id := range l.preauthsByMandate[mandateID] {
		if id == excludeID {
			continue
		}
		p := l.preauths[id]
		if p.Status != PreAuthStatusActive || preauthExpiredLocked(p, now) {
			continue
		}
		if !p.CreatedAt.Before(start) && p.CreatedAt.Before(end) {
			n++
		}
	}
	return n
}

// mandateWindowAuthorizationCountLocked is the mandate's frequency-gate
// occupancy: posted debits in the window (see
// mandateWindowDebitCountLocked) plus active pre-authorization
// reservations in the same window. Both consume the "at most one pull
// per window" budget — a reservation and a debit can never race for the
// same frequency window. excludeID is the pre-authorization capture
// exclusion (see mandateWindowPreAuthCountLocked); "" excludes nothing.
// Callers must hold l.mu.
func (l *Ledger) mandateWindowAuthorizationCountLocked(mandateID string, ts time.Time, frequency, excludeID string) int {
	start, end := mandateWindow(frequency, ts)
	return l.mandateWindowDebitCountLocked(mandateID, ts, frequency) +
		l.mandateWindowPreAuthCountLocked(mandateID, start, end, excludeID)
}
