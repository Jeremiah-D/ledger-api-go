package ledger

import (
	"errors"
	"time"
)

// Transfer disputes (LG-129): payer-initiated chargeback-style claims
// against a posted transfer.
//
// Raising a dispute freezes the disputed amount on the payee's account
// through an authorization hold (see hold.go): while the dispute is
// open, the payee's *available* balance (see Available) is reduced by
// the disputed amount, so the money cannot be spent or moved — "争议期间
// 资金不可动" — without any extra gating on top of normal hold
// semantics. The freeze moves no money through the journal: like every
// hold it is off-journal (no version bump, no chain link), so raising a
// dispute can never double-charge.
//
// Resolution is terminal and one-way: open -> resolved, never back. An
// upheld dispute refunds the payer by appending a partial reversal of
// the transfer's principal journal entry (see ReverseEntryPartial) for
// the disputed amount, then releases the freeze hold. A rejected dispute
// releases the hold only — the funds stay with the payee.
//
// Distinct from dunning (see dunning.go): dunning is
// merchant-initiated collections escalation on overdue receivables — the
// payee chasing money it is owed. Disputes are payer-initiated
// chargeback-style claims — the payer clawing money back from the payee.
// A dispute freezes the payee's funds and can force a refund; dunning
// never moves money.
//
// Scope, kept honest:
//   - Only same-currency transfer principals are disputable: the
//     transfer is resolved through the pain.001 transfer registry (see
//     pain001.go), which records same-currency transfers (plain and
//     split-chunk principals) after all validation gates passed.
//     Cross-currency transfers settle through the FX clearing account
//     with legs in two currencies, so no single (payer, payee, amount,
//     currency) tuple exists to freeze — they are rejected with
//     ErrDisputeTransferNotFound, like unknown IDs.
//   - Partial disputes are allowed: the disputed amount only has to be
//     positive and at most the transfer's amount. Each dispute books
//     its own freeze hold and, when upheld, its own partial reversal,
//     so several disputes may coexist on one transfer up to the
//     transfer's full amount — the reversal cap (see
//     ReverseEntryPartial) rejects the excess with ErrAlreadyReversed.
//   - A transfer whose principal entry is pending dual-control review
//     (see review.go) cannot be disputed: the money has not moved to
//     the payee yet, so there is nothing to freeze or refund. The
//     review workflow is the correct way to cancel it.
//   - The freeze hold carries a long expiry (see defaultDisputeHoldTTL)
//     so an open dispute keeps freezing the funds for the whole
//     chargeback window. If the hold expires while the dispute is
//     still open, the funds become available again — the dispute
//     record stays open and resolving it still works (Release is
//     idempotent), but the freeze guarantee lapsed with the hold.
//   - The upheld refund is a partial reversal of the transfer's
//     principal entry, not a full ReverseEntry: ReverseEntry would
//     reverse the whole transfer amount, which is wrong for partial
//     disputes. ReverseEntryPartial with the disputed amount nets
//     exactly the disputed cents back to the payer, with the
//     reversal's full Post-order risk gates (frozen, overdraft,
//     daily-limit, period) guarding the way back. If the refund is
//     rejected by those gates, the dispute stays open and the hold
//     stays in place — the resolution never half-commits.

// Dispute lifecycle statuses.
const (
	DisputeStatusOpen     = "open"
	DisputeStatusResolved = "resolved"
)

// Dispute reason codes: the payer's stated cause for the chargeback.
const (
	DisputeReasonUnauthorized = "unauthorized" // the payer did not authorize the transfer
	DisputeReasonNotReceived  = "not_received" // the payer never received the goods/services
	DisputeReasonDuplicate    = "duplicate"    // the transfer was posted twice
	DisputeReasonFraud        = "fraud"        // the transfer is suspected fraud
)

// disputeReasonCodes is the accepted reason-code set for validation.
var disputeReasonCodes = map[string]bool{
	DisputeReasonUnauthorized: true,
	DisputeReasonNotReceived:  true,
	DisputeReasonDuplicate:    true,
	DisputeReasonFraud:        true,
}

// defaultDisputeHoldTTL is the expiry of the freeze hold a raised
// dispute places on the payee's account: 120 days, the classic
// chargeback filing window. Holds are always time-bound (see
// ErrHoldMissingExpiry), and an open-ended freeze would strand the
// payee's available funds forever.
const defaultDisputeHoldTTL = 120 * 24 * time.Hour

// Dispute errors.
var (
	// ErrEmptyDisputeID is returned when the dispute ID is empty.
	ErrEmptyDisputeID = errors.New("ledger: dispute ID must not be empty")
	// ErrEmptyDisputeTransferID is returned when the disputed transfer
	// ID is empty.
	ErrEmptyDisputeTransferID = errors.New("ledger: dispute transfer ID must not be empty")
	// ErrDisputeNonPositiveAmount is returned when the disputed amount
	// is not positive.
	ErrDisputeNonPositiveAmount = errors.New("ledger: dispute amount must be greater than zero")
	// ErrUnknownDisputeReasonCode is returned when the reason code is
	// not one of the known codes (unauthorized, not_received,
	// duplicate, fraud).
	ErrUnknownDisputeReasonCode = errors.New("ledger: unknown dispute reason code")
	// ErrDisputeCurrencyMismatch is returned when the dispute's
	// currency names a different currency than the disputed transfer.
	// Disputes never cross currencies.
	ErrDisputeCurrencyMismatch = errors.New("ledger: dispute currency must match the transfer currency")
	// ErrDisputeTransferNotFound is returned when the disputed
	// transfer ID names no same-currency transfer principal on this
	// ledger. Cross-currency transfers are not disputable (see the
	// package comment) and land here too. It is a 404-class error.
	ErrDisputeTransferNotFound = errors.New("ledger: dispute transfer not found")
	// ErrDisputeTransferPendingReview is returned when the disputed
	// transfer's principal entry is pending dual-control review (see
	// review.go): the money has not moved to the payee yet, so there
	// is nothing to freeze or refund — the review workflow is the
	// correct way to cancel it. It is a 409-class error.
	ErrDisputeTransferPendingReview = errors.New("ledger: cannot dispute a transfer pending review; approve or reject the review first")
	// ErrDisputeAmountExceedsTransfer is returned when the disputed
	// amount is larger than the transfer's amount. Partial disputes are
	// allowed, over-disputes are not.
	ErrDisputeAmountExceedsTransfer = errors.New("ledger: dispute amount exceeds the transfer amount")
	// ErrDisputeNotFound is returned by ResolveDispute and GetDispute
	// for an unknown dispute ID. It is a 404-class error.
	ErrDisputeNotFound = errors.New("ledger: dispute not found")
	// ErrDisputeAlreadyResolved is returned by ResolveDispute when the
	// dispute is already resolved: resolution is terminal and
	// irreversible. It is a 409-class error.
	ErrDisputeAlreadyResolved = errors.New("ledger: dispute is already resolved")
)

// Dispute is one payer-initiated chargeback-style claim against a
// posted same-currency transfer. Status moves open -> resolved exactly
// once; Uphold records the resolution decision (true = the payer won and
// was refunded, false = the claim was rejected and the payee kept the
// funds) and is only meaningful when Status is resolved. HoldID is the
// freeze hold placed on the payee at raise time ("dispute/<ID>").
type Dispute struct {
	ID             string    `json:"id"`
	TransferID     string    `json:"transfer_id"`
	ReasonCode     string    `json:"reason_code"`
	AmountCents    int64     `json:"amount_cents"`
	Currency       string    `json:"currency"`
	Status         string    `json:"status"`
	Uphold         bool      `json:"upheld"`
	HoldID         string    `json:"hold_id"`
	CreatedAt      time.Time `json:"created_at"`
	ResolvedAt     time.Time `json:"resolved_at,omitempty"`
	IdempotencyKey string    `json:"idempotency_key,omitempty"`
}

// disputeHoldIDFor returns the deterministic freeze-hold ID of a
// dispute: "dispute/<dispute ID>". The derivation keeps the hold
// addressable from the dispute record without a separate index.
func disputeHoldIDFor(disputeID string) string {
	return "dispute/" + disputeID
}

// RaiseDispute records a payer-initiated dispute against a posted
// same-currency transfer and freezes the disputed amount on the payee's
// account through an authorization hold (see the package comment).
//
// The check order: field validation (non-empty ID and transfer ID,
// positive amount, known reason code, valid currency), then the
// idempotency replay checks — a repeated dispute ID, or a repeated
// idempotency key under a different ID, returns the original dispute
// with duplicate == true and books nothing new — then the transfer
// resolution (unknown IDs and cross-currency transfers fail with
// ErrDisputeTransferNotFound; pending-review principals fail with
// ErrDisputeTransferPendingReview), the currency match, and the
// amount cap (ErrDisputeAmountExceedsTransfer). Only then is the freeze
// hold placed on the payee: when the payee's available funds cannot
// cover the disputed amount the raise is rejected with
// ErrInsufficientAvailableFunds (a 422-class semantic rejection — the
// request is well-formed, the funds just are not there to freeze),
// and a frozen payee account rejects the hold with ErrAccountFrozen.
//
// The commit is atomic under the write lock: the dispute record, its
// idempotency index entry, and the payee's freeze hold land together
// after all checks passed. The hold is off-journal (see hold.go), so
// raising a dispute bumps neither the ledger version nor the audit
// chain — the version and chain stay the money-movement detectors. The
// raise is still observable: one "dispute_raised" audit event and one
// dispute_raised outbox event fire, both naming the transfer, the
// reason, and the freeze hold.
//
// An empty Currency means the transfer's currency; anything else must
// name the transfer's currency exactly. A zero CreatedAt is filled with
// the current time.
func (l *Ledger) RaiseDispute(d Dispute) (dispute Dispute, duplicate bool, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if d.ID == "" {
		return Dispute{}, false, ErrEmptyDisputeID
	}
	if d.TransferID == "" {
		return Dispute{}, false, ErrEmptyDisputeTransferID
	}
	if d.AmountCents <= 0 {
		return Dispute{}, false, ErrDisputeNonPositiveAmount
	}
	if !disputeReasonCodes[d.ReasonCode] {
		return Dispute{}, false, ErrUnknownDisputeReasonCode
	}
	// An empty currency defers to the transfer's currency (resolved
	// below); a named one must be a valid code — the equality check
	// against the transfer's currency comes after the transfer lookup.
	var currency string
	if d.Currency != "" {
		currency, err = normalizeCurrency(d.Currency)
		if err != nil {
			return Dispute{}, false, err
		}
	}

	// Idempotency: a repeated dispute ID replays the original dispute
	// (the replay returns before the transfer lookup, exactly like the
	// hold replay — it books nothing new), and a repeated idempotency
	// key under a new ID returns the dispute the key created.
	if existing, ok := l.disputes[d.ID]; ok {
		return existing, true, nil
	}
	if d.IdempotencyKey != "" {
		if id, ok := l.disputeKeys[d.IdempotencyKey]; ok {
			return l.disputes[id], true, nil
		}
	}

	// The transfer resolves through the pain.001 transfer registry: it
	// carries the (payer, payee, amount, currency) tuple of every
	// same-currency transfer principal recorded after all validation
	// gates passed (see pain001.go). Raw journal posts and
	// cross-currency transfers are not in the registry — they cannot be
	// disputed.
	pt, ok := l.painTransfers[d.TransferID]
	if !ok {
		return Dispute{}, false, ErrDisputeTransferNotFound
	}
	// The principal journal entry must still exist, and it must be
	// effective: a pending-review transfer has not moved money to the
	// payee, so there is nothing to freeze or refund.
	if entry, ok := l.entries[d.TransferID]; !ok {
		return Dispute{}, false, ErrDisputeTransferNotFound
	} else if entry.PendingReview {
		return Dispute{}, false, ErrDisputeTransferPendingReview
	}

	if currency == "" {
		currency = pt.Currency
	} else if currency != pt.Currency {
		return Dispute{}, false, ErrDisputeCurrencyMismatch
	}
	if d.AmountCents > pt.AmountCents {
		return Dispute{}, false, ErrDisputeAmountExceedsTransfer
	}

	now := time.Now()
	l.maybePruneIdempotencyKeys(now)

	// The freeze hold on the payee: it reserves the disputed amount
	// from the payee's available balance (see Available) without
	// journaling anything, so raising a dispute never double-charges.
	// Its ID derives deterministically from the dispute ID. The hold
	// is placed through the lock-held core so the dispute record and
	// its hold land atomically — a replay above would otherwise be
	// able to observe a dispute without its hold.
	holdID := disputeHoldIDFor(d.ID)
	if _, _, err := l.holdLocked(Hold{
		ID:          holdID,
		Account:     pt.To,
		AmountCents: d.AmountCents,
		Currency:    currency,
		ExpiresAt:   now.Add(defaultDisputeHoldTTL),
		CreatedAt:   now,
	}); err != nil {
		return Dispute{}, false, err
	}

	dispute = Dispute{
		ID:             d.ID,
		TransferID:     d.TransferID,
		ReasonCode:     d.ReasonCode,
		AmountCents:    d.AmountCents,
		Currency:       currency,
		Status:         DisputeStatusOpen,
		HoldID:         holdID,
		CreatedAt:      d.CreatedAt,
		IdempotencyKey: d.IdempotencyKey,
	}
	if dispute.CreatedAt.IsZero() {
		dispute.CreatedAt = now
	}
	l.disputes[d.ID] = dispute
	if d.IdempotencyKey != "" {
		l.disputeKeys[d.IdempotencyKey] = d.ID
	}

	l.emitAudit(AuditEvent{
		Op:            "dispute_raised",
		Actor:         "RaiseDispute",
		TraceID:       d.ID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{pt.From, pt.To},
		Details: map[string]any{
			"transfer_id":  d.TransferID,
			"reason_code":  d.ReasonCode,
			"amount_cents": d.AmountCents,
			"currency":     currency,
			"hold_id":      holdID,
		},
	})
	// Domain outbox: the dispute is a domain fact even though raising
	// it books no journal row — the freeze hold is off-journal by
	// design, and the dispute record is operational state.
	l.emitOutboxLocked(OutboxEvent{
		Type:        OutboxDisputeRaised,
		Account:     pt.To,
		AmountCents: d.AmountCents,
		Currency:    currency,
		Details: map[string]any{
			"dispute_id":  d.ID,
			"transfer_id": d.TransferID,
			"reason_code": d.ReasonCode,
			"hold_id":     holdID,
			"payer":       string(pt.From),
			"payee":       string(pt.To),
		},
	})

	return dispute, false, nil
}

// ResolveDispute terminally resolves an open dispute: open -> resolved,
// irreversibly.
//
//   - uphold == true: the payer wins. The disputed amount is refunded by
//     appending a partial reversal of the transfer's principal journal
//     entry (see ReverseEntryPartial) — debit payer, credit payee, the
//     mirror of the original legs — which nets exactly the disputed
//     cents back to the payer with the reversal's full risk gates
//     guarding the way back. Then the dispute's freeze hold is
//     released.
//   - uphold == false: the claim is rejected. Only the freeze hold is
//     released — the funds stay with the payee; no journal entry is
//     booked.
//
// Only open disputes resolve: resolving twice, or resolving an unknown
// ID, fails with ErrDisputeAlreadyResolved / ErrDisputeNotFound. When
// the upheld refund is rejected by the reversal's risk gates (a frozen
// payee, an overdraft-protected payee whose balance cannot cover the
// refund, the reversal cap after a sibling dispute already refunded),
// the dispute stays open and the hold stays in place — the resolution
// never half-commits: a rejection records nothing, no journal row, no
// state change.
//
// The commit is atomic under the write lock: the refund reversal (when
// upheld), the hold release, and the dispute's transition to resolved
// land together. One "dispute_resolved" audit event (carrying the
// uphold decision and the reason) and one dispute_resolved outbox event
// fire after the commit.
func (l *Ledger) ResolveDispute(id string, uphold bool) (Dispute, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	d, ok := l.disputes[id]
	if !ok {
		return Dispute{}, ErrDisputeNotFound
	}
	if d.Status != DisputeStatusOpen {
		return Dispute{}, ErrDisputeAlreadyResolved
	}

	versionBefore := l.version
	now := time.Now()
	var refundEntryID string
	if uphold {
		// The refund is a partial reversal of the transfer's principal
		// entry for exactly the disputed amount — not a full
		// ReverseEntry, which would reverse the whole transfer and
		// over-refund partial disputes. The lock-held core keeps the
		// reversal inside this resolution's atomic commit.
		reversal, _, err := l.reverseEntryPartialLocked(d.TransferID, d.AmountCents, "")
		if err != nil {
			return Dispute{}, err
		}
		refundEntryID = reversal.ID
	}
	// The freeze hold is always released: on uphold the refund already
	// moved the disputed cents back to the payer, so the reservation
	// is spent; on reject the payee keeps the funds and the reservation
	// lifts. Release is idempotent — an already-released or expired
	// hold returns as-is.
	if _, err := l.releaseLocked(d.HoldID); err != nil {
		return Dispute{}, err
	}

	d.Status = DisputeStatusResolved
	d.Uphold = uphold
	d.ResolvedAt = now
	l.disputes[id] = d

	details := map[string]any{
		"transfer_id":  d.TransferID,
		"reason_code":  d.ReasonCode,
		"amount_cents": d.AmountCents,
		"currency":     d.Currency,
		"hold_id":      d.HoldID,
		"uphold":       uphold,
	}
	if refundEntryID != "" {
		details["refund_entry_id"] = refundEntryID
	}
	// The transfer's payer and payee ride the audit accounts: the
	// dispute moves money between them (on uphold) or unfreezes it
	// (on reject).
	pt := l.painTransfers[d.TransferID]
	l.emitAudit(AuditEvent{
		Op:            "dispute_resolved",
		Actor:         "ResolveDispute",
		TraceID:       id,
		VersionBefore: versionBefore,
		VersionAfter:  l.version,
		Accounts:      []AccountID{pt.From, pt.To},
		Details:       details,
	})
	l.emitOutboxLocked(OutboxEvent{
		Type:        OutboxDisputeResolved,
		Account:     pt.To,
		EntryID:     refundEntryID,
		AmountCents: d.AmountCents,
		Currency:    d.Currency,
		Details: map[string]any{
			"dispute_id":  id,
			"transfer_id": d.TransferID,
			"reason_code": d.ReasonCode,
			"uphold":      uphold,
			"payer":       string(pt.From),
			"payee":       string(pt.To),
		},
	})

	return d, nil
}

// GetDispute returns the dispute with the given ID, or false when no
// such dispute exists. The returned record's Status is "open" until
// ResolveDispute moves it to "resolved" — resolution is terminal.
func (l *Ledger) GetDispute(id string) (Dispute, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	d, ok := l.disputes[id]
	return d, ok
}

// pruneDisputeKeysLocked evicts dispute-raise idempotency keys older
// than the TTL, mirroring the hold key expiry: after the TTL, raising
// with the same key books a brand-new dispute. The disputes themselves
// are untouched — only the replay-detection index shrinks. A
// non-positive TTL disables pruning. Callers must hold l.mu (the write
// lock; the sweep mutates the index).
func (l *Ledger) pruneDisputeKeysLocked(now time.Time) int {
	if l.idempotencyTTL <= 0 {
		return 0
	}
	cutoff := now.Add(-l.idempotencyTTL)
	removed := 0
	for key, id := range l.disputeKeys {
		d, ok := l.disputes[id]
		if !ok || d.CreatedAt.Before(cutoff) {
			delete(l.disputeKeys, key)
			removed++
		}
	}
	return removed
}
