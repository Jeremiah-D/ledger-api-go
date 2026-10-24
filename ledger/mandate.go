package ledger

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Direct-debit mandates (代扣协议, LG-125).
//
// A Mandate is the standing authorization for a payee (the merchant) to
// pull funds from a payer (the customer): the SEPA-direct-debit /
// autopay agreement behind subscription billing. The mandate names the
// payer and the payee, caps each pull at MaxAmountCents in one currency,
// and bounds the authorization in time (ValidFrom/ValidUntil) and in
// rate (Frequency: at most one debit per UTC day, week, or month).
//
// Lifecycle: active -> suspended | cancelled | expired. Suspension and
// cancellation are operator actions (SuspendMandate, CancelMandate);
// expiry is lazy by predicate (a mandate whose ValidUntil has passed
// counts as expired on every read, like holds) plus an explicit
// ExpireMandates sweep that marks the rows for observability. Expired,
// suspended, and cancelled mandates authorize nothing: DebitWithMandate
// rejects with ErrMandateExpired / ErrMandateNotActive.
//
// DebitWithMandate is the only way to pull under a mandate. The mandate
// gates run first — the mandate must exist, be active, cover the debit's
// booking timestamp, cap the amount, match the currency, and have its
// frequency window free — and then the debit posts through the full Post
// validation sequence (see postLockedGated): field validation,
// sanctions screening, idempotency replay, chart direction, frozen,
// suspended, overdraft, daily limit, budget, period, and journal rules.
// The mandate gates are the authorization; Post's gates are the booking.
// No validation logic is copied: the debit builds a JournalEntry and
// hands it to postLocked, the same internal path Post uses.
//
// Money direction: a direct debit pulls from the payer to the payee, so
// the journal entry debits the payee's account (increased) and credits
// the payer's account (decreased) — the same leg convention as a
// capture settling from the held account to the payee (see hold.go).
//
// Every debit is annotated with the mandate ID on two channels, like
// reversals carry "reversal_of:<id>" on the memo and the reversal_of
// link (see reversal.go, links.go):
//   - memo:   "mandate_of:<mandate ID>", appended after any caller memo
//     (searchable via GET /entries?memo=, which is a substring search);
//   - link:   a mandate_of entry link from the debit entry to the
//     mandate's reference record (see LinkTypeMandateOf).
//
// Mandates are structural config, like external account mappings and
// entry links: creating, suspending, cancelling, or expiring one never
// bumps the journal version, and the registry round-trips through
// disaster-recovery snapshots. Every mandate change — create, suspend,
// cancel, expire, and each debit — enters the structured audit chain
// (LG-32); each debit additionally journals its entry through the
// tamper-evident entry chain via postLocked.

// Mandate statuses. A mandate authorizes debits only while its status is
// active and its validity window covers the debit's booking timestamp;
// every other combination is terminal for authorization purposes.
const (
	MandateStatusActive    = "active"
	MandateStatusSuspended = "suspended"
	MandateStatusCancelled = "cancelled"
	MandateStatusExpired   = "expired"
)

// Mandate frequencies: the rate cap on debits under one mandate. Each
// value authorizes at most one debit per UTC window — the UTC calendar
// day for per-day, the ISO week (Monday 00:00 UTC) for per-week, the UTC
// calendar month for per-month. Window boundaries are UTC, like the
// daily-limit and budget windows.
const (
	MandateFrequencyPerDay   = "per-day"
	MandateFrequencyPerWeek  = "per-week"
	MandateFrequencyPerMonth = "per-month"
)

// Validation and state errors returned by the mandate APIs.
var (
	// ErrEmptyMandateID is returned by CreateMandate when the mandate
	// ID is empty. It is a 400-class error.
	ErrEmptyMandateID = errors.New("ledger: mandate ID must not be empty")
	// ErrMandateIDWhitespace is returned by CreateMandate when the
	// mandate ID contains whitespace. The ID round-trips through the
	// "mandate_of:<id>" memo stamp, which is parsed by suffix — an ID
	// with whitespace could not be recovered unambiguously.
	ErrMandateIDWhitespace = errors.New("ledger: mandate ID must not contain whitespace")
	// ErrMandateIDConflict is returned by CreateMandate when the ID is
	// already used by another mandate or by a journal entry. The
	// mandate namespace and the entry namespace must stay disjoint:
	// mandate_of links name the mandate in their To endpoint, and a
	// collision would make a link endpoint ambiguous.
	ErrMandateIDConflict = errors.New("ledger: mandate ID already in use")
	// ErrEmptyMandatePayer is returned by CreateMandate when the payer
	// account is empty. It is a 400-class error.
	ErrEmptyMandatePayer = errors.New("ledger: mandate payer must not be empty")
	// ErrEmptyMandatePayee is returned by CreateMandate when the payee
	// account is empty. It is a 400-class error.
	ErrEmptyMandatePayee = errors.New("ledger: mandate payee must not be empty")
	// ErrMandateSameParty is returned by CreateMandate when payer and
	// payee are the same account: a direct debit pulls from someone
	// else. It is a 400-class error.
	ErrMandateSameParty = errors.New("ledger: mandate payer and payee must differ")
	// ErrMandateNonPositiveMaxAmount is returned by CreateMandate when
	// the per-debit cap is not positive. It is a 400-class error.
	ErrMandateNonPositiveMaxAmount = errors.New("ledger: mandate max amount must be greater than zero")
	// ErrInvalidMandateFrequency is returned by CreateMandate when the
	// frequency is not one of per-day, per-week, or per-month. It is a
	// 400-class error.
	ErrInvalidMandateFrequency = errors.New("ledger: invalid mandate frequency")
	// ErrMandateInvalidWindow is returned by CreateMandate when
	// ValidUntil is zero or not after ValidFrom. A mandate without an
	// end would authorize pulls forever. It is a 400-class error.
	ErrMandateInvalidWindow = errors.New("ledger: mandate valid_from must be before a non-zero valid_until")
	// ErrMandateNotFound is returned for an unknown mandate ID. It is a
	// 404-class error.
	ErrMandateNotFound = errors.New("ledger: mandate not found")
	// ErrMandateNotActive is returned when an operation needs an active
	// mandate and finds a suspended or cancelled one — or a debit whose
	// booking timestamp predates ValidFrom. It is a 422-class semantic
	// rejection: the request is well-formed, the authorization just
	// does not cover it.
	ErrMandateNotActive = errors.New("ledger: mandate is not active")
	// ErrMandateExpired is returned when an operation needs an active
	// mandate and its ValidUntil has passed. It is a 422-class semantic
	// rejection.
	ErrMandateExpired = errors.New("ledger: mandate has expired")
	// ErrMandateAmountExceeded is returned by DebitWithMandate when the
	// debit amount is above the mandate's per-debit cap. It is a
	// 422-class semantic rejection.
	ErrMandateAmountExceeded = errors.New("ledger: debit amount exceeds mandate maximum")
	// ErrMandateCurrencyMismatch is returned by DebitWithMandate when
	// the debit currency differs from the mandate's. A mandate is
	// single-currency, like every journal entry. It is a 422-class
	// semantic rejection.
	ErrMandateCurrencyMismatch = errors.New("ledger: debit currency does not match mandate currency")
	// ErrMandateFrequencyExceeded is returned by DebitWithMandate when
	// the mandate's frequency window already holds a debit. It is a
	// 422-class semantic rejection.
	ErrMandateFrequencyExceeded = errors.New("ledger: mandate frequency window already used")
)

// Mandate is one direct-debit authorization: the standing agreement for
// Payee to pull funds from Payer.
//
// Currency follows the JournalEntry convention: empty normalizes to the
// default currency, anything else must be a 3-letter uppercase ISO 4217
// code. Status is one of active/suspended/cancelled/expired; a mandate
// counts as expired once its ValidUntil has passed even before
// ExpireMandates marks it (see mandateExpiredLocked). ValidFrom may be
// zero, meaning the mandate is effective since the beginning of time;
// ValidUntil must be non-zero — an authorization without an end is
// rejected at creation.
type Mandate struct {
	ID             string    `json:"id"`
	Payer          AccountID `json:"payer"`
	Payee          AccountID `json:"payee"`
	MaxAmountCents int64     `json:"max_amount_cents"`
	Currency       string    `json:"currency"`
	Frequency      string    `json:"frequency"`
	ValidFrom      time.Time `json:"valid_from"`
	ValidUntil     time.Time `json:"valid_until"`
	Status         string    `json:"status"`
	CreatedAt      time.Time `json:"created_at"`
}

// MandateDebit is one pull request under a mandate: the caller supplies
// the debit's identity, amount, and journal annotations — the legs come
// from the mandate itself (debit the payee, credit the payer), so a
// caller can never pull the wrong pair of accounts. Currency empty
// means the mandate's currency (an explicit different currency is
// rejected); CreatedAt zero means now.
type MandateDebit struct {
	ID             string            `json:"id"`
	AmountCents    int64             `json:"amount_cents"`
	Currency       string            `json:"currency,omitempty"`
	Memo           string            `json:"memo,omitempty"`
	ExternalRef    string            `json:"external_ref,omitempty"`
	Attachments    []string          `json:"attachments,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
	IdempotencyKey string            `json:"idempotency_key,omitempty"`
	CreatedAt      time.Time         `json:"created_at,omitempty"`
}

// MandateDebitReceipt reports what DebitWithMandate committed.
//
// Entry is the journaled debit posting (debit leg = the mandate's
// payee, credit leg = the mandate's payer, memo stamped
// "mandate_of:<mandate ID>"). MandateID names the mandate the debit was
// authorized under. Duplicate replays return the original receipt with
// Duplicate == true and book nothing new.
type MandateDebitReceipt struct {
	MandateID string       `json:"mandate_id"`
	Entry     JournalEntry `json:"entry"`
	Duplicate bool         `json:"duplicate"`
}

// MandateReport is one mandate row in the reconcile report: the mandate
// with its effective status (lazy expiry applied, like the reads) and
// its debit usage — TotalDebits ever, WindowDebits in the current
// frequency window.
type MandateReport struct {
	ID             string    `json:"id"`
	Payer          AccountID `json:"payer"`
	Payee          AccountID `json:"payee"`
	Status         string    `json:"status"`
	MaxAmountCents int64     `json:"max_amount_cents"`
	Currency       string    `json:"currency"`
	Frequency      string    `json:"frequency"`
	ValidFrom      time.Time `json:"valid_from"`
	ValidUntil     time.Time `json:"valid_until"`
	TotalDebits    int       `json:"total_debits"`
	WindowDebits   int       `json:"window_debits"`
}

// mandateMemoPrefix is the memo stamp on every mandate debit entry: the
// "mandate_of:" prefix plus the mandate ID, so debits are searchable
// through GET /entries?memo=mandate_of: (a substring search) and the
// mandate is recoverable from the entry for idempotent replays.
const mandateMemoPrefix = "mandate_of:"

// mandateMemoFor returns the memo stamp for a mandate debit.
func mandateMemoFor(mandateID string) string {
	return mandateMemoPrefix + mandateID
}

// stampMandateMemo appends the mandate stamp to a caller memo: the
// caller's note stays readable and the stamp stays searchable — the
// memo filter is a substring search, so the stamp matches wherever it
// sits. The combined memo is still subject to the 255-rune journal
// memo cap (see validateJournalEntry).
func stampMandateMemo(callerMemo, mandateID string) string {
	stamp := mandateMemoFor(mandateID)
	if callerMemo == "" {
		return stamp
	}
	return callerMemo + " " + stamp
}

// mandateIDFromMemo recovers the mandate ID stamped on a mandate debit
// entry's memo. It reports false when the memo carries no well-formed
// stamp — mandate IDs never contain whitespace (see
// ErrMandateIDWhitespace), so the stamp is always the memo's suffix and
// the parse is unambiguous.
func mandateIDFromMemo(memo string) (string, bool) {
	i := strings.LastIndex(memo, mandateMemoPrefix)
	if i < 0 {
		return "", false
	}
	id := memo[i+len(mandateMemoPrefix):]
	if id == "" || strings.ContainsAny(id, " \t\n\r") {
		return "", false
	}
	return id, true
}

// validMandateFrequency reports whether f is one of the three rate caps.
func validMandateFrequency(f string) bool {
	switch f {
	case MandateFrequencyPerDay, MandateFrequencyPerWeek, MandateFrequencyPerMonth:
		return true
	}
	return false
}

// validateMandateFields checks a mandate's fields and normalizes its
// currency in place, without touching ledger state — so CreateMandate
// and the snapshot import share the same admission gate: a mandate that
// could never have been created is rejected, never installed.
func validateMandateFields(m *Mandate) error {
	if m.ID == "" {
		return ErrEmptyMandateID
	}
	if strings.ContainsAny(m.ID, " \t\n\r") {
		return ErrMandateIDWhitespace
	}
	if m.Payer == "" {
		return ErrEmptyMandatePayer
	}
	if m.Payee == "" {
		return ErrEmptyMandatePayee
	}
	if m.Payer == m.Payee {
		return ErrMandateSameParty
	}
	if m.MaxAmountCents <= 0 {
		return ErrMandateNonPositiveMaxAmount
	}
	currency, err := normalizeCurrency(m.Currency)
	if err != nil {
		return err
	}
	m.Currency = currency
	if !validMandateFrequency(m.Frequency) {
		return fmt.Errorf("%w: %q", ErrInvalidMandateFrequency, m.Frequency)
	}
	if m.ValidUntil.IsZero() || !m.ValidFrom.Before(m.ValidUntil) {
		return ErrMandateInvalidWindow
	}
	return nil
}

// mandateExpiredLocked reports whether the mandate counts as expired at
// now: its status is still active (terminal states stay terminal) and
// its ValidUntil has passed. Expiry is lazy by predicate — the sweep
// only marks the rows for observability. Callers must hold l.mu.
func mandateExpiredLocked(m Mandate, now time.Time) bool {
	return m.Status == MandateStatusActive && !now.Before(m.ValidUntil)
}

// mandateEffectiveStatusLocked returns the status reads disclose: the
// stored status, except an active mandate past its ValidUntil reads as
// expired. The stored row is untouched — only ExpireMandates persists
// the transition. Callers must hold l.mu.
func mandateEffectiveStatusLocked(m Mandate, now time.Time) string {
	if mandateExpiredLocked(m, now) {
		return MandateStatusExpired
	}
	return m.Status
}

// mandateWithEffectiveStatus returns a copy of m with Status set to the
// effective status at now. Callers must hold l.mu.
func mandateWithEffectiveStatus(m Mandate, now time.Time) Mandate {
	m.Status = mandateEffectiveStatusLocked(m, now)
	return m
}

// mandateWindow returns the [start, end) UTC window containing t for the
// frequency: the UTC calendar day, the ISO week starting Monday 00:00
// UTC, or the UTC calendar month. Window boundaries are UTC, like the
// daily-limit and budget windows.
func mandateWindow(frequency string, t time.Time) (start, end time.Time) {
	utc := t.UTC()
	switch frequency {
	case MandateFrequencyPerWeek:
		start = time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
		for start.Weekday() != time.Monday {
			start = start.AddDate(0, 0, -1)
		}
		return start, start.AddDate(0, 0, 7)
	case MandateFrequencyPerMonth:
		start = time.Date(utc.Year(), utc.Month(), 1, 0, 0, 0, 0, time.UTC)
		return start, start.AddDate(0, 1, 0)
	default: // MandateFrequencyPerDay
		start = time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
		return start, start.AddDate(0, 0, 1)
	}
}

// mandateWindowDebitCountLocked counts the mandate's debits whose
// booking timestamp falls in the frequency window containing ts. Each
// frequency value authorizes at most one debit per window, so the gate
// is count == 0. Callers must hold l.mu.
func (l *Ledger) mandateWindowDebitCountLocked(mandateID string, ts time.Time, frequency string) int {
	start, end := mandateWindow(frequency, ts)
	n := 0
	for _, id := range l.mandateDebits[mandateID] {
		if e, ok := l.entries[id]; ok && !e.CreatedAt.Before(start) && e.CreatedAt.Before(end) {
			n++
		}
	}
	return n
}

// CreateMandate installs a direct-debit mandate: the standing
// authorization for Payee to pull from Payer.
//
// Field validation runs first (non-empty whitespace-free ID, distinct
// non-empty payer/payee, positive per-debit cap, valid ISO 4217
// currency, a legal frequency, ValidFrom before a non-zero ValidUntil),
// then the ID-uniqueness check: the ID must be unused both as a mandate
// ID and as a journal entry ID, keeping the mandate and entry
// namespaces disjoint for the mandate_of link endpoints. The mandate is
// born active with CreatedAt = now when zero.
//
// The install is structural, like SetExternalMapping: it does not bump
// the journal version, it round-trips through snapshots, and it is
// recorded as a "mandate_created" audit event plus a mandate_created
// outbox envelope.
func (l *Ledger) CreateMandate(m Mandate) (Mandate, error) {
	if err := validateMandateFields(&m); err != nil {
		return Mandate{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.mandates[m.ID]; ok {
		return Mandate{}, fmt.Errorf("%w: mandate %q", ErrMandateIDConflict, m.ID)
	}
	if _, ok := l.entries[m.ID]; ok {
		return Mandate{}, fmt.Errorf("%w: journal entry %q", ErrMandateIDConflict, m.ID)
	}
	now := time.Now()
	if m.CreatedAt.IsZero() {
		m.CreatedAt = now
	}
	m.Status = MandateStatusActive
	l.mandates[m.ID] = m
	l.emitAudit(AuditEvent{
		Op:            "mandate_created",
		Actor:         "CreateMandate",
		TraceID:       m.ID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{m.Payer, m.Payee},
		Details: map[string]any{
			"max_amount_cents": m.MaxAmountCents,
			"currency":         m.Currency,
			"frequency":        m.Frequency,
			"valid_from":       m.ValidFrom.UTC().Format(time.RFC3339),
			"valid_until":      m.ValidUntil.UTC().Format(time.RFC3339),
		},
	})
	l.emitOutboxLocked(OutboxEvent{
		Type:        OutboxMandateCreated,
		Account:     m.Payer,
		AmountCents: m.MaxAmountCents,
		Currency:    m.Currency,
		Details: map[string]any{
			"mandate_id": m.ID,
			"payee":      string(m.Payee),
			"frequency":  m.Frequency,
		},
	})
	return m, nil
}

// GetMandate returns the mandate with the given ID, or false when no
// such mandate exists. The returned Status is the effective status: an
// active mandate past its ValidUntil reads as expired even before
// ExpireMandates marks it (see mandateEffectiveStatusLocked).
func (l *Ledger) GetMandate(id string) (Mandate, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	m, ok := l.mandates[id]
	if !ok {
		return Mandate{}, false
	}
	return mandateWithEffectiveStatus(m, time.Now()), true
}

// ListMandates returns every mandate with its effective status, sorted
// by ID.
func (l *Ledger) ListMandates() []Mandate {
	l.mu.RLock()
	defer l.mu.RUnlock()
	now := time.Now()
	out := make([]Mandate, 0, len(l.mandates))
	for _, m := range l.mandates {
		out = append(out, mandateWithEffectiveStatus(m, now))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// SuspendMandate moves an active mandate to suspended: it authorizes no
// further debits until cancelled or expired. Suspending a suspended,
// cancelled, or already-marked-expired mandate fails with
// ErrMandateNotActive; a mandate whose ValidUntil has passed fails with
// ErrMandateExpired — it is dead, not pausable. The transition is
// structural (no version bump) and audited as "mandate_suspended".
func (l *Ledger) SuspendMandate(id string) (Mandate, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	m, ok := l.mandates[id]
	if !ok {
		return Mandate{}, ErrMandateNotFound
	}
	now := time.Now()
	if mandateExpiredLocked(m, now) {
		return Mandate{}, ErrMandateExpired
	}
	if m.Status != MandateStatusActive {
		return Mandate{}, fmt.Errorf("%w: status %q", ErrMandateNotActive, m.Status)
	}
	m.Status = MandateStatusSuspended
	l.mandates[id] = m
	l.emitAudit(AuditEvent{
		Op:            "mandate_suspended",
		Actor:         "SuspendMandate",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{m.Payer, m.Payee},
		Details: map[string]any{
			"currency": m.Currency,
		},
	})
	l.emitOutboxLocked(OutboxEvent{
		Type:     OutboxMandateSuspended,
		Account:  m.Payer,
		Currency: m.Currency,
		Details:  map[string]any{"mandate_id": id},
	})
	return m, nil
}

// CancelMandate terminally cancels an active or suspended mandate: it
// authorizes no further debits, ever. Cancelling a cancelled or
// already-marked-expired mandate fails with ErrMandateNotActive; a
// mandate whose ValidUntil has passed fails with ErrMandateExpired.
// The transition is structural (no version bump) and audited as
// "mandate_cancelled".
func (l *Ledger) CancelMandate(id string) (Mandate, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	m, ok := l.mandates[id]
	if !ok {
		return Mandate{}, ErrMandateNotFound
	}
	now := time.Now()
	if mandateExpiredLocked(m, now) {
		return Mandate{}, ErrMandateExpired
	}
	if m.Status != MandateStatusActive && m.Status != MandateStatusSuspended {
		return Mandate{}, fmt.Errorf("%w: status %q", ErrMandateNotActive, m.Status)
	}
	m.Status = MandateStatusCancelled
	l.mandates[id] = m
	l.emitAudit(AuditEvent{
		Op:            "mandate_cancelled",
		Actor:         "CancelMandate",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{m.Payer, m.Payee},
		Details: map[string]any{
			"currency": m.Currency,
		},
	})
	l.emitOutboxLocked(OutboxEvent{
		Type:     OutboxMandateCancelled,
		Account:  m.Payer,
		Currency: m.Currency,
		Details:  map[string]any{"mandate_id": id},
	})
	return m, nil
}

// ExpireMandates marks every mandate whose ValidUntil has passed as
// expired and returns how many were marked. Expired mandates were
// already dead for authorization purposes (expiry is lazy by predicate);
// the sweep exists so operators and the reconcile report can observe
// which mandates lapsed. It never touches suspended or cancelled
// mandates, and it does not bump the ledger version.
func (l *Ledger) ExpireMandates() int {
	return l.ExpireMandatesAt(time.Now())
}

// ExpireMandatesAt is ExpireMandates with a caller-supplied timestamp,
// so tests can pin expiry deterministically.
func (l *Ledger) ExpireMandatesAt(now time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	marked := 0
	var expired []Mandate
	for id, m := range l.mandates {
		if mandateExpiredLocked(m, now) {
			m.Status = MandateStatusExpired
			l.mandates[id] = m
			marked++
			expired = append(expired, m)
		}
	}
	// A sweep that expires nothing emits no event: the background
	// sweeper ticks on a timer, and zero-expire ticks would drown the
	// audit trail in noise.
	if marked > 0 {
		l.emitAudit(AuditEvent{
			Op:            "mandate_expired",
			Actor:         "ExpireMandates",
			TraceID:       fmt.Sprintf("mandate-expire@%d", l.version),
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Details: map[string]any{
				"expired": marked,
			},
		})
		// Domain outbox: unlike the batched audit event, the outbox
		// gets one mandate_expired per mandate — downstream consumers
		// track mandates individually.
		for _, m := range expired {
			l.emitOutboxLocked(OutboxEvent{
				Type:     OutboxMandateExpired,
				Account:  m.Payer,
				Currency: m.Currency,
				Details:  map[string]any{"mandate_id": m.ID},
			})
		}
	}
	return marked
}

// DebitWithMandate pulls AmountCents from the mandate's payer to the
// mandate's payee under the mandate's authorization.
//
// The check order mirrors Post, with the mandate gates first:
//  1. the mandate must exist (ErrMandateNotFound);
//  2. the debit builds its journal entry from the mandate — debit leg =
//     the payee, credit leg = the payer, memo stamped
//     "mandate_of:<mandate ID>" after any caller memo — and clears
//     field validation (see validateJournalEntry);
//  3. the idempotency replay check on Post's byKey namespace: a replay
//     returns the original receipt with Duplicate == true and books
//     nothing new — it runs before the mandate gates below, so a replay
//     succeeds even if the mandate was suspended, cancelled, or expired
//     after the original debit, because the replay books nothing new
//     (the same reason Post's replay check precedes the frozen gate);
//  4. the mandate gates: the mandate must be active
//     (ErrMandateNotActive), the debit's booking timestamp must fall in
//     [ValidFrom, ValidUntil) (ErrMandateNotActive before ValidFrom,
//     ErrMandateExpired after), the amount must not exceed the
//     per-debit cap (ErrMandateAmountExceeded), the currency must match
//     (ErrMandateCurrencyMismatch), and the frequency window must be
//     free (ErrMandateFrequencyExceeded) — active pre-authorizations
//     (see mandate_preauth.go, LG-192) occupy the window exactly like
//     posted debits, so a reservation and a debit can never race for
//     the same window;
//  5. the full Post validation sequence runs inside postLocked —
//     sanctions screening, chart direction, frozen, suspended,
//     overdraft, daily limit, budget, period, journal rules — so a
//     mandate debit can never dodge a booking control the operator's
//     own Post would hit.
//
// The commit is atomic under the write lock: the journal entry (with
// its chain link and version bump, via postLocked), the
// mandate->entries index row, the mandate_of entry link (see
// LinkTypeMandateOf), the debit counter, and the audit/outbox events
// land together. Rejected debits record nothing: no journal row, no
// chain link, no version bump, no index row.
//
// The debit's booking timestamp is d.CreatedAt, defaulting to now: the
// mandate validity window and the frequency window are evaluated at it,
// consistent with the daily-limit and period gates, which key on the
// entry's own timestamp.
//
// Automatic retry (see mandate_retry.go, LG-127): when the mandate
// carries an opt-in retry policy and the debit fails with a retryable
// error (insufficient funds, daily limit, or monthly budget exhausted),
// the failure is recorded as a pending retry instead of dropped —
// SweepMandateRetries fires it later under an independent per-attempt
// idempotency key. Validation and risk-control rejections are terminal
// and never retry.
func (l *Ledger) DebitWithMandate(mandateID string, d MandateDebit) (MandateDebitReceipt, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	receipt, _, err := l.debitWithMandateLocked(mandateID, d, 0, time.Now())
	return receipt, err
}

// debitMandateCoreLocked is the lock-held debit core shared by
// DebitWithMandate (attempt 0) and the retry sweep (attempts >= 1, see
// mandate_retry.go): the mandate gates plus the atomic commit zone. It
// performs no retry bookkeeping itself — the caller decides what a
// failure or a success means for the pending-retry registry. Callers
// must hold the write lock.
func (l *Ledger) debitMandateCoreLocked(mandateID string, d MandateDebit) (MandateDebitReceipt, error) {
	return l.debitMandateCoreLockedExcluding(mandateID, d, "")
}

// debitMandateCoreLockedExcluding is debitMandateCoreLocked with one
// pre-authorization excluded from the frequency gate: a capture
// (see CapturePreAuth in mandate_preauth.go) excludes the reservation
// it is converting, so the settlement is not blocked by its own hold on
// the window. excludeID == "" excludes nothing — the direct-debit and
// retry paths. Callers must hold the write lock.
func (l *Ledger) debitMandateCoreLockedExcluding(mandateID string, d MandateDebit, excludeID string) (MandateDebitReceipt, error) {
	m, ok := l.mandates[mandateID]
	if !ok {
		return MandateDebitReceipt{}, ErrMandateNotFound
	}

	// The legs come from the mandate, not the caller: a direct debit
	// pulls from the payer to the payee, so the journal entry debits the
	// payee's account (increased) and credits the payer's (decreased) —
	// the same leg convention as a capture settling from the held
	// account to the payee. A caller can never pull the wrong pair of
	// accounts under a mandate.
	entry := JournalEntry{
		ID:             d.ID,
		DebitAccount:   m.Payee,
		CreditAccount:  m.Payer,
		AmountCents:    d.AmountCents,
		Currency:       d.Currency,
		Memo:           stampMandateMemo(d.Memo, mandateID),
		ExternalRef:    d.ExternalRef,
		Attachments:    d.Attachments,
		Metadata:       d.Metadata,
		IdempotencyKey: d.IdempotencyKey,
		CreatedAt:      d.CreatedAt,
	}
	// Field validation first, like Post: the entry must be well-formed
	// before the mandate gates or the replay check see it. Currency
	// normalizes here (empty means the default currency), so the
	// mandate-currency comparison below sees the canonical code.
	if err := validateJournalEntry(&entry); err != nil {
		return MandateDebitReceipt{}, err
	}
	// The booking timestamp defaults to now (like Post): the mandate
	// validity window and the frequency window below are evaluated at
	// it, consistent with the daily-limit and period gates, which key
	// on the entry's own timestamp.
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now()
	}

	// Idempotent replay before the mandate gates: the replay books
	// nothing new, so it must not fail on a mandate that changed state
	// after the original debit. The receipt discloses the mandate the
	// original debit was booked under, parsed from its memo stamp — the
	// requested mandate in the normal case; a key the caller reused
	// from a plain Post carries no stamp, and the receipt then names
	// the requested mandate.
	if entry.IdempotencyKey != "" {
		if orig, ok := l.byKey[entry.IdempotencyKey]; ok {
			mid, ok := mandateIDFromMemo(orig.Memo)
			if !ok {
				mid = mandateID
			}
			return MandateDebitReceipt{MandateID: mid, Entry: orig, Duplicate: true}, nil
		}
	}

	// The mandate gates: the authorization for this pull.
	ts := entry.CreatedAt
	if m.Status != MandateStatusActive {
		return MandateDebitReceipt{}, fmt.Errorf("%w: status %q", ErrMandateNotActive, m.Status)
	}
	if !ts.Before(m.ValidUntil) {
		return MandateDebitReceipt{}, ErrMandateExpired
	}
	if ts.Before(m.ValidFrom) {
		return MandateDebitReceipt{}, fmt.Errorf("%w: mandate valid from %s", ErrMandateNotActive, m.ValidFrom.UTC().Format(time.RFC3339))
	}
	if entry.AmountCents > m.MaxAmountCents {
		return MandateDebitReceipt{}, fmt.Errorf("%w: %d > %d", ErrMandateAmountExceeded, entry.AmountCents, m.MaxAmountCents)
	}
	if entry.Currency != m.Currency {
		return MandateDebitReceipt{}, fmt.Errorf("%w: %q != %q", ErrMandateCurrencyMismatch, entry.Currency, m.Currency)
	}
	// The frequency gate counts active pre-authorization reservations
	// alongside posted debits (see
	// mandateWindowAuthorizationCountLocked): a reservation and a debit
	// can never race for the same window. The capturing
	// pre-authorization's own reservation is excluded when excludeID
	// names it — the settlement converts the reservation, it does not
	// compete with it.
	if l.mandateWindowAuthorizationCountLocked(mandateID, ts, m.Frequency, excludeID) > 0 {
		return MandateDebitReceipt{}, fmt.Errorf("%w: %s", ErrMandateFrequencyExceeded, m.Frequency)
	}
	// The mandate_of link pre-check: the debit entry ID must not equal
	// the mandate ID, or the link would be a self-reference. Checked
	// here — before anything books — rather than after the commit.
	if err := validateEntryLinkFields(entry.ID, mandateID, LinkTypeMandateOf); err != nil {
		return MandateDebitReceipt{}, err
	}

	// The full Post gate sequence, unmodified: the mandate gates above
	// are the authorization, this is the booking. postLocked also
	// re-checks the idempotency namespace — unreachable after the
	// replay check above under the same write lock, kept as the
	// defensive second gate.
	posted, duplicate, err := l.postLocked(entry, true)
	if err != nil {
		return MandateDebitReceipt{}, err
	}
	if duplicate {
		mid, ok := mandateIDFromMemo(posted.Memo)
		if !ok {
			mid = mandateID
		}
		return MandateDebitReceipt{MandateID: mid, Entry: posted, Duplicate: true}, nil
	}

	// Atomic commit zone: the index row, the mandate_of link, the
	// counter, and the audit/outbox events land with the journal entry
	// under the one write lock. The link declaration cannot fail here:
	// its fields were pre-validated above and mandates and entries are
	// append-only, so both endpoints exist exactly as checked.
	l.mandateDebits[mandateID] = append(l.mandateDebits[mandateID], posted.ID)
	l.mandateDebitChanges.Add(1)
	if _, _, err := l.declareEntryLinkLocked(context.Background(), posted.ID, mandateID, LinkTypeMandateOf); err != nil {
		return MandateDebitReceipt{}, err
	}
	l.emitAudit(AuditEvent{
		Op:            "mandate_debit",
		Actor:         "DebitWithMandate",
		TraceID:       mandateID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		EntryIDs:      []string{posted.ID},
		Accounts:      []AccountID{m.Payer, m.Payee},
		Details: map[string]any{
			"amount_cents": posted.AmountCents,
			"currency":     posted.Currency,
			"frequency":    m.Frequency,
		},
	})
	l.emitOutboxLocked(OutboxEvent{
		Type:        OutboxMandateDebited,
		Account:     m.Payer,
		EntryID:     posted.ID,
		AmountCents: posted.AmountCents,
		Currency:    posted.Currency,
		Details: map[string]any{
			"mandate_id": mandateID,
			"payee":      string(m.Payee),
		},
	})
	return MandateDebitReceipt{MandateID: mandateID, Entry: posted}, nil
}

// MandateDebits returns the journal entries debited under the mandate,
// in debit order, paged by cursor: cursor names the entry ID the page
// starts after (empty starts at the first debit), limit is 1..1000, and
// nextCursor is empty on the last page. An unknown mandate fails with
// ErrMandateNotFound; an unknown cursor with ErrInvalidCursor.
func (l *Ledger) MandateDebits(mandateID, cursor string, limit int) (page []JournalEntry, nextCursor string, err error) {
	if limit < 1 || limit > maxPageSize {
		return nil, "", ErrInvalidLimit
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	ids := l.mandateDebits[mandateID]
	if _, ok := l.mandates[mandateID]; !ok {
		return nil, "", ErrMandateNotFound
	}
	start := 0
	if cursor != "" {
		found := false
		for i, id := range ids {
			if id == cursor {
				start = i + 1
				found = true
				break
			}
		}
		if !found {
			return nil, "", ErrInvalidCursor
		}
	}
	page = make([]JournalEntry, 0, limit)
	for i := start; i < len(ids) && len(page) < limit; i++ {
		if e, ok := l.entries[ids[i]]; ok {
			page = append(page, e)
		}
	}
	if start+len(page) < len(ids) {
		nextCursor = page[len(page)-1].ID
	}
	return page, nextCursor, nil
}

// MandateDebitCount reports how many mandate debits the ledger has
// posted since it started (see DebitWithMandate): new debits only —
// idempotent replays of a debit key are not counted. Operational
// metric (surfaced on GET /metrics as ledger_mandate_debits_total),
// not books: it is in-memory only and does not round-trip through
// snapshots.
func (l *Ledger) MandateDebitCount() uint64 {
	return l.mandateDebitChanges.Load()
}

// mandateReportsLocked builds the reconcile report's mandates table:
// every mandate with its effective status and debit usage, sorted by
// ID. Callers must hold l.mu (either lock suffices).
func (l *Ledger) mandateReportsLocked(now time.Time) []MandateReport {
	out := make([]MandateReport, 0, len(l.mandates))
	for _, m := range l.mandates {
		eff := mandateWithEffectiveStatus(m, now)
		total := len(l.mandateDebits[m.ID])
		out = append(out, MandateReport{
			ID:             m.ID,
			Payer:          m.Payer,
			Payee:          m.Payee,
			Status:         eff.Status,
			MaxAmountCents: m.MaxAmountCents,
			Currency:       m.Currency,
			Frequency:      m.Frequency,
			ValidFrom:      m.ValidFrom,
			ValidUntil:     m.ValidUntil,
			TotalDebits:    total,
			WindowDebits:   l.mandateWindowDebitCountLocked(m.ID, now, m.Frequency),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// mandatesSnapshotLocked returns the mandates in ID order: the snapshot
// record form of the registry. Callers must hold l.mu (either lock
// suffices; export holds the read lock).
func (l *Ledger) mandatesSnapshotLocked() []Mandate {
	out := make([]Mandate, 0, len(l.mandates))
	for _, m := range l.mandates {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// mandateDebitsSnapshotLocked returns the mandate -> debit-entry index
// in ID order: the snapshot record form. Callers must hold l.mu (either
// lock suffices; export holds the read lock).
func (l *Ledger) mandateDebitsSnapshotLocked() map[string][]string {
	out := make(map[string][]string, len(l.mandateDebits))
	for id, ids := range l.mandateDebits {
		out[id] = append([]string(nil), ids...)
	}
	return out
}
