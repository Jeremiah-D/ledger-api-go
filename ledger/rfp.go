package ledger

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Request for Payment (RfP, LG-171, fintech: 即时支付收款请求).
//
// An RfP is the creditor-initiated half of an instant payment: the
// creditor (a merchant, a biller) asks the debtor (the customer) to pay
// a fixed amount, and the debtor accepts or rejects each request
// individually — the FedNow/RTP "request for payment" flow. This is
// the pull-side dual of a transfer: instead of the payer pushing money,
// the payee requests it and the payer authorizes the pull.
//
// Lifecycle: issued -> paid | rejected | expired. The debtor's accept
// and the payout post are atomic inside AcceptRfP: the record jumps
// issued -> paid in one step, emitting rfp_accepted (the authorization)
// and rfp_paid (the payout) audit events in order. There is no
// observable accepted-but-unpaid state — a crash between acceptance and
// posting must never leave money ambiguous.
//
// Money moves only on accept: before that the RfP books no journal
// rows, bumps no version, and appends no hash-chain link — it is pure
// structural state, like a hold (see hold.go). The payout posts
// through the full PostTransfer gate sequence (denylist, replay,
// frozen, overdraft, daily-limit, period, ...), so an accepted RfP can
// still fail to pay when the debtor's account cannot cover it; the RfP
// stays issued and the error propagates to the caller.
//
// Expiry is lazy, like holds: an issued RfP whose ExpiresAt has passed
// counts as expired on every read (accept/reject see ErrRfPExpired),
// plus an explicit ExpireRfPs sweep that marks rows for observability.
//
// Orthogonal to direct-debit mandates (LG-125/127): a mandate is a
// standing authorization for recurring pulls under a cap; an RfP is a
// one-shot request the debtor accepts per payment. RfP is also
// orthogonal to the payout instruction lifecycle (payout.go): the RfP
// posts a plain transfer, not a maker-checker payout.
//
// In-memory only, like payout instructions: RfP records do not
// round-trip through disaster-recovery snapshots — the journaled payout
// rows do, so the money is safe; only the request metadata is lost.
// The rfpID is its own idempotency key: re-creating an existing ID
// returns the original record (duplicate=true), and re-accepting a
// paid RfP returns the original receipt.

// RfP statuses.
const (
	RfPStatusIssued   = "issued"
	RfPStatusPaid     = "paid"
	RfPStatusRejected = "rejected"
	RfPStatusExpired  = "expired"
)

// RfP is a request-for-payment record. The payout transfer, once
// posted, is named "rfp/<id>/payout" and carries the memo
// "rfp_of:<id>" — searchable via GET /entries?memo=, like reversals
// carry "reversal_of:<id>".
type RfP struct {
	ID          string    `json:"id"`
	Creditor    AccountID `json:"creditor"`
	Debtor      AccountID `json:"debtor"`
	AmountCents int64     `json:"amount_cents"`
	Currency    string    `json:"currency"`
	ExpiresAt   time.Time `json:"expires_at"`
	CreatedAt   time.Time `json:"created_at"`
	Status      string    `json:"status"`
	// TransferID names the payout transfer once the RfP is paid.
	TransferID string `json:"transfer_id,omitempty"`
	// paidReceipt is the original payout receipt; re-accepting a paid
	// RfP replays it (in-memory only, never serialized).
	paidReceipt *TransferReceipt
}

// Validation and state errors returned by the RfP APIs.
var (
	// ErrEmptyRfPID is returned by CreateRfP when the ID is empty. It
	// is a 400-class error.
	ErrEmptyRfPID = errors.New("ledger: rfp id must not be empty")
	// ErrRfPIDWhitespace is returned by CreateRfP when the ID contains
	// whitespace. The ID round-trips through the "rfp_of:<id>" memo
	// stamp, which is parsed by suffix — an ID with whitespace could
	// not be recovered unambiguously (see mandate.go).
	ErrRfPIDWhitespace = errors.New("ledger: rfp id must not contain whitespace")
	// ErrEmptyRfPCreditor is returned by CreateRfP when the creditor
	// is empty. It is a 400-class error.
	ErrEmptyRfPCreditor = errors.New("ledger: rfp creditor must not be empty")
	// ErrEmptyRfPDebtor is returned by CreateRfP when the debtor is
	// empty. It is a 400-class error.
	ErrEmptyRfPDebtor = errors.New("ledger: rfp debtor must not be empty")
	// ErrRfPSameParty is returned by CreateRfP when creditor and debtor
	// are the same account: a payment request asks someone else to pay.
	// It is a 400-class error.
	ErrRfPSameParty = errors.New("ledger: rfp creditor and debtor must differ")
	// ErrRfPNonPositiveAmount is returned by CreateRfP when the amount
	// is not positive. It is a 400-class error.
	ErrRfPNonPositiveAmount = errors.New("ledger: rfp amount must be greater than zero")
	// ErrRfPExpiryRequired is returned by CreateRfP when ExpiresAt is
	// zero. An RfP without a deadline could linger forever; instant
	// payments always expire. It is a 400-class error.
	ErrRfPExpiryRequired = errors.New("ledger: rfp expires_at is required")
	// ErrRfPExpiryNotFuture is returned by CreateRfP when ExpiresAt is
	// not in the future. It is a 400-class error.
	ErrRfPExpiryNotFuture = errors.New("ledger: rfp expires_at must be in the future")
	// ErrRfPNotFound is returned for an unknown RfP ID. It is a
	// 404-class error.
	ErrRfPNotFound = errors.New("ledger: rfp not found")
	// ErrRfPNotIssued is returned by AcceptRfP/RejectRfP when the RfP
	// is not in issued status. It is a 422-class semantic rejection:
	// the request is well-formed, the lifecycle just does not allow it.
	ErrRfPNotIssued = errors.New("ledger: rfp is not in issued status")
	// ErrRfPExpired is returned by AcceptRfP/RejectRfP when the RfP's
	// ExpiresAt has passed. It is a 422-class semantic rejection.
	ErrRfPExpired = errors.New("ledger: rfp has expired")
)

// rfpTransferID derives the deterministic payout transfer ID for an
// RfP: "rfp/<id>/payout". Deterministic so a retried accept cannot
// double-book — the transfer ID namespace is reserved by the first
// successful accept.
func rfpTransferID(id string) string { return "rfp/" + id + "/payout" }

// rfpMemo stamps the payout's principal entry with the RfP reference.
func rfpMemo(id string) string { return "rfp_of:" + id }

// rfpExpiredLocked reports whether an issued RfP has passed its expiry
// as of now. The check is lazy: expiry takes effect on read, like
// holds (see hold.go).
func rfpExpiredLocked(r RfP, now time.Time) bool {
	return r.Status == RfPStatusIssued && !now.Before(r.ExpiresAt)
}

// validateRfPFields runs the CreateRfP field checks in fail-fast
// order: identity, parties, amount, currency, expiry.
func validateRfPFields(r RfP, now time.Time) (RfP, error) {
	if strings.TrimSpace(r.ID) == "" {
		return RfP{}, ErrEmptyRfPID
	}
	if strings.ContainsAny(r.ID, " \t\n\r") {
		return RfP{}, ErrRfPIDWhitespace
	}
	if r.Creditor == "" {
		return RfP{}, ErrEmptyRfPCreditor
	}
	if r.Debtor == "" {
		return RfP{}, ErrEmptyRfPDebtor
	}
	if r.Creditor == r.Debtor {
		return RfP{}, ErrRfPSameParty
	}
	if r.AmountCents <= 0 {
		return RfP{}, ErrRfPNonPositiveAmount
	}
	currency, err := normalizeCurrency(r.Currency)
	if err != nil {
		return RfP{}, err
	}
	r.Currency = currency
	if r.ExpiresAt.IsZero() {
		return RfP{}, ErrRfPExpiryRequired
	}
	if !r.ExpiresAt.After(now) {
		return RfP{}, ErrRfPExpiryNotFuture
	}
	return r, nil
}

// CreateRfP issues a request for payment: the creditor asks the debtor
// to pay AmountCents. The call books nothing — no journal rows, no
// version bump, no hash-chain link — and records the rfp_issued audit
// event. The rfpID is its own idempotency key: re-creating an existing
// ID returns the original record with duplicate=true and does not
// re-audit.
func (l *Ledger) CreateRfP(ctx context.Context, r RfP) (RfP, bool, error) {
	now := time.Now()
	var err error
	if r, err = validateRfPFields(r, now); err != nil {
		return RfP{}, false, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if existing, ok := l.rfps[r.ID]; ok {
		return existing, true, nil
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = now
	}
	r.Status = RfPStatusIssued
	l.maybePruneIdempotencyKeys(now)
	l.rfps[r.ID] = r
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "rfp_issued",
		Actor:         "CreateRfP",
		TraceID:       r.ID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{r.Creditor, r.Debtor},
		Details: map[string]any{
			"amount_cents": r.AmountCents,
			"currency":     r.Currency,
			"expires_at":   r.ExpiresAt.UTC().Format(time.RFC3339),
		},
	})
	out := l.rfps[r.ID]
	return out, false, nil
}

// NewRfPID mints a server-generated RfP ID ("rfp-1", "rfp-2", ...).
// The counter is process-lifetime state, like report schedules: RfPs
// are in-memory only, so the sequence restarts with the process.
func (l *Ledger) NewRfPID() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rfpSeq++
	return fmt.Sprintf("rfp-%d", l.rfpSeq)
}

// GetRfP returns the RfP record for id, or false when unknown. An
// issued RfP past its expiry reports the expired status without
// persisting the transition (the sweep does that); callers that need
// the persisted mark use ExpireRfPs.
func (l *Ledger) GetRfP(id string) (RfP, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	r, ok := l.rfps[id]
	if !ok {
		return RfP{}, false
	}
	if rfpExpiredLocked(r, time.Now()) {
		r.Status = RfPStatusExpired
	}
	return r, true
}

// AcceptRfP is the debtor's acceptance: it atomically authorizes the
// payment and posts the payout transfer (debtor -> creditor) through
// the full PostTransfer gate sequence. The audit trail records
// rfp_accepted (the authorization) then rfp_paid (the payout).
// Re-accepting a paid RfP replays the original receipt
// (duplicate=true). When the payout cannot post — frozen account,
// overdraft, daily limit — the RfP stays issued and the PostTransfer
// error propagates, so no money moves on a failed accept.
func (l *Ledger) AcceptRfP(ctx context.Context, id string, actor string) (TransferReceipt, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	r, ok := l.rfps[id]
	if !ok {
		return TransferReceipt{}, ErrRfPNotFound
	}
	if rfpExpiredLocked(r, now) {
		r.Status = RfPStatusExpired
		l.rfps[id] = r
		l.emitAuditCtx(ctx, AuditEvent{
			Op:            "rfp_expired",
			Actor:         "AcceptRfP",
			TraceID:       id,
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Accounts:      []AccountID{r.Creditor, r.Debtor},
		})
		return TransferReceipt{}, ErrRfPExpired
	}
	switch r.Status {
	case RfPStatusPaid:
		dup := *r.paidReceipt
		dup.Duplicate = true
		return dup, nil
	case RfPStatusIssued:
		// proceed below
	default:
		return TransferReceipt{}, ErrRfPNotIssued
	}
	if actor == "" {
		actor = "AcceptRfP"
	}
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "rfp_accepted",
		Actor:         actor,
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{r.Creditor, r.Debtor},
		Details: map[string]any{
			"amount_cents": r.AmountCents,
			"currency":     r.Currency,
		},
	})
	// The payout posts through postTransferLocked directly: we already
	// hold the write lock, and PostTransfer's admission (FX quote) is
	// unnecessary for a same-currency debtor->creditor payment.
	vBefore := l.version
	receipt, err := l.postTransferLocked(Transfer{
		ID:          rfpTransferID(id),
		From:        r.Debtor,
		To:          r.Creditor,
		AmountCents: r.AmountCents,
		Currency:    r.Currency,
		Memo:        rfpMemo(id),
	}, FXRateQuote{})
	if err != nil {
		return TransferReceipt{}, err
	}
	r.Status = RfPStatusPaid
	r.TransferID = receipt.TransferID
	cp := receipt
	r.paidReceipt = &cp
	l.rfps[id] = r
	entryIDs := make([]string, 0, len(receipt.Entries))
	for _, e := range receipt.Entries {
		entryIDs = append(entryIDs, e.ID)
	}
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "rfp_paid",
		Actor:         actor,
		TraceID:       id,
		VersionBefore: vBefore,
		VersionAfter:  l.version,
		EntryIDs:      entryIDs,
		Accounts:      []AccountID{r.Creditor, r.Debtor},
		Details: map[string]any{
			"transfer_id":  receipt.TransferID,
			"amount_cents": r.AmountCents,
			"currency":     r.Currency,
		},
	})
	return receipt, nil
}

// RejectRfP records the debtor's refusal: issued -> rejected, with an
// rfp_rejected audit event. Rejecting an expired RfP reports
// ErrRfPExpired (the sweep marks it expired); anything but issued
// reports ErrRfPNotIssued.
func (l *Ledger) RejectRfP(ctx context.Context, id string, actor string) (RfP, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	r, ok := l.rfps[id]
	if !ok {
		return RfP{}, ErrRfPNotFound
	}
	if rfpExpiredLocked(r, now) {
		r.Status = RfPStatusExpired
		l.rfps[id] = r
		l.emitAuditCtx(ctx, AuditEvent{
			Op:            "rfp_expired",
			Actor:         "RejectRfP",
			TraceID:       id,
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Accounts:      []AccountID{r.Creditor, r.Debtor},
		})
		return RfP{}, ErrRfPExpired
	}
	if r.Status != RfPStatusIssued {
		return RfP{}, ErrRfPNotIssued
	}
	if actor == "" {
		actor = "RejectRfP"
	}
	r.Status = RfPStatusRejected
	l.rfps[id] = r
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "rfp_rejected",
		Actor:         actor,
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{r.Creditor, r.Debtor},
	})
	return r, nil
}

// ExpireRfPs marks every issued RfP past its expiry as expired,
// emitting one rfp_expired audit event per record, and returns the
// count. It is the explicit sweep behind the lazy expiry predicate
// (see ExpireHolds, LG-20). Already-terminal records are untouched.
func (l *Ledger) ExpireRfPs(now time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	expired := 0
	for id, r := range l.rfps {
		if !rfpExpiredLocked(r, now) {
			continue
		}
		r.Status = RfPStatusExpired
		l.rfps[id] = r
		expired++
		l.emitAudit(AuditEvent{
			Op:            "rfp_expired",
			Actor:         "ExpireRfPs",
			TraceID:       id,
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Accounts:      []AccountID{r.Creditor, r.Debtor},
		})
	}
	return expired
}
