package ledger

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Dispute representment (LG-179): the payee's second-phase defense
// against a payer-initiated chargeback claim (see dispute.go, LG-129).
//
// A filed claim (dispute_raised) opens a bounded evidence window —
// default 7 days (see SetRepresentmentWindow). Inside the window the
// payee may submit one representment: an evidence package of document
// hashes (a receipt scan, a delivery confirmation, a signed contract —
// the hashes prove the documents existed, the documents themselves
// stay off-ledger) plus a free-form defense statement. After
// submission an operator or arbiter adjudicates: upholding the claim
// refunds the payer (the dispute resolves upheld), reversing the claim
// rejects it (the payee keeps the funds).
//
// The window is fail-closed: a dispute with no representment when the
// window expires is auto-upheld by SweepExpiredRepresentments — the
// original claim stands, favoring the payer who filed it. A submitted
// representment stops the auto-uphold; the dispute then waits for the
// operator's adjudication (the window gates the evidence submission,
// not the adjudicator's decision, which may land after the window).
//
// Orthogonal to dispute.go: the claim-raise logic is untouched.
// Representments never move money themselves — submission books
// nothing (the freeze hold from the raise is still in place);
// adjudication delegates to the dispute resolution's atomic commit
// (see resolveDisputeLocked), so the refund and the hold release land
// together or not at all. One "representment_submitted" and one
// "representment_adjudicated" audit event fire, plus the usual
// dispute_raised/dispute_resolved domain outbox events.
//
// Scope, kept honest:
//   - Representment records are operational state like disputes: they
//     do not round-trip through disaster-recovery snapshots — after a
//     restore, an open dispute's freeze hold is still present (holds
//     do round-trip) but both the dispute and its representment must
//     be re-derived from the audit trail.
//   - The window is measured from the dispute's CreatedAt (the claim
//     filing time), not from the last activity: a payee cannot extend
//     its evidence window by waiting.

// defaultRepresentmentWindow is the payee's evidence window after a
// claim is filed: 7 days, the classic chargeback representment window.
// Configurable per ledger (see SetRepresentmentWindow); the zero
// window is rejected — an evidence window that admits no evidence is
// a misconfiguration, not a policy.
const defaultRepresentmentWindow = 7 * 24 * time.Hour

// maxRepresentmentDocuments caps one representment's evidence package
// at 32 document hashes: enough for a receipt, a delivery proof, and
// a contract, few enough to keep the record bounded.
const maxRepresentmentDocuments = 32

// maxRepresentmentDocumentHashRunes caps one document hash at 128
// UTF-8 characters: enough for a hex SHA-256 digest plus a scheme
// prefix, short enough to keep the record bounded.
const maxRepresentmentDocumentHashRunes = 128

// maxRepresentmentStatementRunes caps the defense statement at 4000
// UTF-8 characters: a real defense fits, a novel does not.
const maxRepresentmentStatementRunes = 4000

// Representment adjudication decisions: what the operator/arbiter
// decides about the original claim after reviewing the evidence.
const (
	// RepresentmentDecisionUpholdClaim keeps the original claim
	// standing: the payer wins, and the dispute resolves upheld (the
	// disputed amount is refunded to the payer).
	RepresentmentDecisionUpholdClaim = "uphold"
	// RepresentmentDecisionReverseClaim reverses the original claim:
	// the payee wins, and the dispute resolves rejected (the payee
	// keeps the funds).
	RepresentmentDecisionReverseClaim = "reverse"
)

// Representment errors.
var (
	// ErrRepresentmentWindowExpired is returned by
	// SubmitRepresentment when the dispute's evidence window already
	// closed: late evidence is rejected fail-closed, never admitted.
	// It is a 409-class error.
	ErrRepresentmentWindowExpired = errors.New("ledger: representment window has expired")
	// ErrRepresentmentAlreadySubmitted is returned by
	// SubmitRepresentment when the dispute already carries a
	// representment with different evidence: one evidence package per
	// dispute. Replaying the same evidence returns the original with
	// duplicate == true.
	ErrRepresentmentAlreadySubmitted = errors.New("ledger: dispute already has a representment")
	// ErrRepresentmentNotSubmitted is returned by
	// AdjudicateRepresentment when the dispute has no representment
	// yet: there is no evidence to adjudicate. It is a 409-class
	// error.
	ErrRepresentmentNotSubmitted = errors.New("ledger: dispute has no representment to adjudicate")
	// ErrRepresentmentAlreadyAdjudicated is returned by
	// AdjudicateRepresentment when the representment is already
	// adjudicated: adjudication is terminal, like resolution.
	ErrRepresentmentAlreadyAdjudicated = errors.New("ledger: representment is already adjudicated")
	// ErrInvalidRepresentmentDecision is returned by
	// AdjudicateRepresentment for a decision outside
	// uphold/reverse. It is a 400-class error.
	ErrInvalidRepresentmentDecision = errors.New("ledger: representment decision must be uphold or reverse")
	// ErrEmptyRepresentmentEvidence is returned by
	// SubmitRepresentment when the evidence package is empty: at
	// least one document hash or a non-empty statement is required —
	// an empty representment defends nothing.
	ErrEmptyRepresentmentEvidence = errors.New("ledger: representment needs at least one document hash or a statement")
	// ErrInvalidRepresentmentDocument is returned by
	// SubmitRepresentment for a document hash that is empty or longer
	// than maxRepresentmentDocumentHashRunes, or when the package
	// exceeds maxRepresentmentDocuments hashes.
	ErrInvalidRepresentmentDocument = errors.New("ledger: representment document hashes must be non-empty, at most 128 characters each, at most 32 per representment")
	// ErrEmptyAdjudicator is returned by AdjudicateRepresentment when
	// the adjudicator identity is empty: every adjudication is
	// attributed to an operator or arbiter.
	ErrEmptyAdjudicator = errors.New("ledger: adjudicator must not be empty")
	// ErrInvalidRepresentmentWindow is returned by
	// SetRepresentmentWindow for a non-positive window.
	ErrInvalidRepresentmentWindow = errors.New("ledger: representment window must be positive")
)

// Representment is the payee's evidence package against one open
// transfer dispute. Adjudicated is false until AdjudicateRepresentment
// records the operator's decision; Decision/Adjudicator/AdjudicatedAt
// are only meaningful then.
type Representment struct {
	DisputeID       string    `json:"dispute_id"`
	DocumentHashes  []string  `json:"document_hashes"`
	Statement       string    `json:"statement,omitempty"`
	SubmittedAt     time.Time `json:"submitted_at"`
	WindowExpiresAt time.Time `json:"window_expires_at"`
	Adjudicated     bool      `json:"adjudicated"`
	Decision        string    `json:"decision,omitempty"`
	Adjudicator     string    `json:"adjudicator,omitempty"`
	AdjudicatedAt   time.Time `json:"adjudicated_at,omitempty"`
}

// representmentWindowExpiry returns the evidence-window deadline of a
// dispute: filed at d.CreatedAt, window l.representmentWindow. The
// window is measured from the claim filing — a payee cannot extend it
// by waiting. Callers must hold l.mu (read lock suffices).
func (l *Ledger) representmentWindowExpiry(d Dispute) time.Time {
	return d.CreatedAt.Add(l.representmentWindow)
}

// SetRepresentmentWindow reconfigures the payee's evidence window: a
// filed claim's representment must arrive within d of the filing. A
// non-positive window is rejected — an evidence window that admits no
// evidence is a misconfiguration, not a policy.
func (l *Ledger) SetRepresentmentWindow(d time.Duration) error {
	if d <= 0 {
		return fmt.Errorf("%w: %s", ErrInvalidRepresentmentWindow, d)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.representmentWindow = d
	return nil
}

// RepresentmentWindow reports the current payee evidence window.
func (l *Ledger) RepresentmentWindow() time.Duration {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.representmentWindow
}

// checkRepresentmentEvidence enforces the evidence contract: at least
// one document hash or a non-empty statement (an empty representment
// defends nothing), at most maxRepresentmentDocuments non-empty hashes
// of at most maxRepresentmentDocumentHashRunes characters, and a
// statement of at most maxRepresentmentStatementRunes characters.
func checkRepresentmentEvidence(docHashes []string, statement string) error {
	if len(docHashes) == 0 && strings.TrimSpace(statement) == "" {
		return ErrEmptyRepresentmentEvidence
	}
	if len(docHashes) > maxRepresentmentDocuments {
		return fmt.Errorf("%w: %d hashes, at most %d", ErrInvalidRepresentmentDocument, len(docHashes), maxRepresentmentDocuments)
	}
	for _, h := range docHashes {
		if h == "" || len([]rune(h)) > maxRepresentmentDocumentHashRunes {
			return fmt.Errorf("%w: hash %q", ErrInvalidRepresentmentDocument, h)
		}
	}
	if len([]rune(statement)) > maxRepresentmentStatementRunes {
		return fmt.Errorf("%w: statement exceeds %d characters", ErrEmptyRepresentmentEvidence, maxRepresentmentStatementRunes)
	}
	return nil
}

// representmentEvidenceEqual reports whether a stored representment
// carries the same evidence as a candidate submission: the document
// hash sets match (order-insensitive — a payee re-listing the same
// documents in another order replays, it does not amend) and the
// statements are identical.
func representmentEvidenceEqual(r Representment, docHashes []string, statement string) bool {
	if r.Statement != statement || len(r.DocumentHashes) != len(docHashes) {
		return false
	}
	have := make(map[string]int, len(r.DocumentHashes))
	for _, h := range r.DocumentHashes {
		have[h]++
	}
	for _, h := range docHashes {
		have[h]--
		if have[h] < 0 {
			return false
		}
	}
	return true
}

// SubmitRepresentment records the payee's evidence package against an
// open transfer dispute: the second phase of the dispute defense.
//
// The check order: field validation (non-empty dispute ID, a
// well-formed evidence package — at least one document hash or a
// non-empty statement), then the dispute lookup (unknown IDs fail
// with ErrDisputeNotFound; resolved disputes fail with
// ErrDisputeAlreadyResolved), then the replay check — resubmitting the
// same evidence returns the original representment with duplicate ==
// true, submitting different evidence fails with
// ErrRepresentmentAlreadySubmitted (one evidence package per dispute)
// — and finally the window: submissions after the evidence window
// closed are rejected with ErrRepresentmentWindowExpired,
// fail-closed.
//
// The commit is atomic under the write lock: the representment
// record lands alone — submission books no journal rows and touches
// no holds; the raise's freeze hold stays in place. One
// "representment_submitted" audit event and one
// representment_submitted outbox event fire, naming the dispute and
// the document count (never the statement or the hashes' contents:
// evidence stays with the operator).
func (l *Ledger) SubmitRepresentment(disputeID string, docHashes []string, statement string) (Representment, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if strings.TrimSpace(disputeID) == "" {
		return Representment{}, false, ErrEmptyDisputeID
	}
	if err := checkRepresentmentEvidence(docHashes, statement); err != nil {
		return Representment{}, false, err
	}
	d, ok := l.disputes[disputeID]
	if !ok {
		return Representment{}, false, ErrDisputeNotFound
	}
	if d.Status != DisputeStatusOpen {
		return Representment{}, false, ErrDisputeAlreadyResolved
	}
	// Replay: the same evidence returns the original; different
	// evidence is a second package, which one dispute never gets.
	if existing, ok := l.representments[disputeID]; ok {
		if representmentEvidenceEqual(existing, docHashes, statement) {
			return existing, true, nil
		}
		return Representment{}, false, ErrRepresentmentAlreadySubmitted
	}

	now := time.Now()
	expiry := l.representmentWindowExpiry(d)
	if now.After(expiry) {
		return Representment{}, false, ErrRepresentmentWindowExpired
	}

	hashes := make([]string, len(docHashes))
	copy(hashes, docHashes)
	r := Representment{
		DisputeID:       disputeID,
		DocumentHashes:  hashes,
		Statement:       statement,
		SubmittedAt:     now,
		WindowExpiresAt: expiry,
	}
	l.representments[disputeID] = r

	pt := l.painTransfers[d.TransferID]
	l.emitAudit(AuditEvent{
		Op:            "representment_submitted",
		Actor:         "SubmitRepresentment",
		TraceID:       disputeID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{pt.From, pt.To},
		Details: map[string]any{
			"dispute_id":       disputeID,
			"transfer_id":      d.TransferID,
			"document_count":   len(hashes),
			"has_statement":    strings.TrimSpace(statement) != "",
			"window_expires":   expiry.UTC().Format(time.RFC3339),
			"amount_cents":     d.AmountCents,
			"currency":         d.Currency,
		},
	})
	l.emitOutboxLocked(OutboxEvent{
		Type:        OutboxRepresentmentSubmitted,
		Account:     pt.To,
		AmountCents: d.AmountCents,
		Currency:    d.Currency,
		Details: map[string]any{
			"dispute_id":     disputeID,
			"transfer_id":    d.TransferID,
			"document_count": len(hashes),
			"window_expires": expiry.UTC().Format(time.RFC3339),
		},
	})

	return r, false, nil
}

// AdjudicateRepresentment records the operator/arbiter's decision on
// a submitted representment and resolves the dispute accordingly:
// uphold keeps the original claim standing (the payer is refunded —
// the dispute resolves upheld), reverse reverses the claim (the payee
// keeps the funds — the dispute resolves rejected).
//
// The check order: field validation (non-empty dispute ID, a
// decision of uphold/reverse, a non-empty adjudicator), then state —
// the dispute must exist and be open, a representment must be
// submitted, and it must not be adjudicated already.
//
// Adjudication delegates to the dispute resolution's atomic commit
// (see resolveDisputeLocked): the upheld refund's partial reversal and
// the freeze hold's release land together, or the resolution fails and
// the dispute stays open with the representment un-adjudicated —
// adjudication never half-commits. On success one
// "representment_adjudicated" audit event (carrying the decision and
// the adjudicator) and one representment_adjudicated outbox event fire,
// on top of the resolution's own dispute_resolved events.
//
// The evidence window gates the submission, not the adjudicator's
// decision: adjudicating after the window expired is allowed as long
// as the evidence arrived in time.
func (l *Ledger) AdjudicateRepresentment(disputeID, decision, adjudicator string) (Dispute, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if strings.TrimSpace(disputeID) == "" {
		return Dispute{}, ErrEmptyDisputeID
	}
	var uphold bool
	switch decision {
	case RepresentmentDecisionUpholdClaim:
		uphold = true
	case RepresentmentDecisionReverseClaim:
		uphold = false
	default:
		return Dispute{}, fmt.Errorf("%w: %q", ErrInvalidRepresentmentDecision, decision)
	}
	if strings.TrimSpace(adjudicator) == "" {
		return Dispute{}, ErrEmptyAdjudicator
	}

	d, ok := l.disputes[disputeID]
	if !ok {
		return Dispute{}, ErrDisputeNotFound
	}
	if d.Status != DisputeStatusOpen {
		return Dispute{}, ErrDisputeAlreadyResolved
	}
	r, ok := l.representments[disputeID]
	if !ok {
		return Dispute{}, ErrRepresentmentNotSubmitted
	}
	if r.Adjudicated {
		return Dispute{}, ErrRepresentmentAlreadyAdjudicated
	}

	versionBefore := l.version
	resolved, err := l.resolveDisputeLocked(disputeID, uphold)
	if err != nil {
		return Dispute{}, err
	}

	now := time.Now()
	r.Adjudicated = true
	r.Decision = decision
	r.Adjudicator = adjudicator
	r.AdjudicatedAt = now
	l.representments[disputeID] = r

	pt := l.painTransfers[d.TransferID]
	l.emitAudit(AuditEvent{
		Op:            "representment_adjudicated",
		Actor:         "AdjudicateRepresentment",
		TraceID:       disputeID,
		VersionBefore: versionBefore,
		VersionAfter:  l.version,
		Accounts:      []AccountID{pt.From, pt.To},
		Details: map[string]any{
			"dispute_id":  disputeID,
			"transfer_id": d.TransferID,
			"decision":    decision,
			"adjudicator": adjudicator,
			"uphold":      uphold,
		},
	})
	l.emitOutboxLocked(OutboxEvent{
		Type:        OutboxRepresentmentAdjudicated,
		Account:     pt.To,
		AmountCents: d.AmountCents,
		Currency:    d.Currency,
		Details: map[string]any{
			"dispute_id":  disputeID,
			"transfer_id": d.TransferID,
			"decision":    decision,
			"adjudicator": adjudicator,
		},
	})

	return resolved, nil
}

// SweepExpiredRepresentments is the fail-closed enforcement of the
// evidence window: every open dispute with no representment whose
// window expired as of now is auto-upheld — the original claim stands
// and the payer is refunded, favoring the party who filed the claim.
// Disputes with a submitted representment are untouched: their
// evidence arrived in time, and the dispute waits for the operator's
// adjudication.
//
// The sweep is a cron entry point (like ExpireHolds): each dispute
// resolves through the dispute resolution's atomic commit, so an
// upheld refund rejected by the reversal's risk gates leaves that
// dispute open and the sweep moves on — the failure is collected and
// reported while the other disputes still resolve. The returned slice
// holds every dispute the sweep upheld, sorted by dispute ID;
// per-dispute failures join into the returned error.
func (l *Ledger) SweepExpiredRepresentments(now time.Time) ([]Dispute, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Collect first, in ID order: deterministic regardless of map
	// iteration order.
	var ids []string
	for id, d := range l.disputes {
		if d.Status != DisputeStatusOpen {
			continue
		}
		if _, has := l.representments[id]; has {
			continue
		}
		if !now.After(l.representmentWindowExpiry(d)) {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var resolved []Dispute
	var errs []error
	for _, id := range ids {
		d, err := l.resolveDisputeLocked(id, true)
		if err != nil {
			errs = append(errs, fmt.Errorf("auto-uphold dispute %q: %w", id, err))
			continue
		}
		resolved = append(resolved, d)
		// The auto-uphold is observable: the resolution already fired
		// dispute_resolved, and this marker names the trigger.
		pt := l.painTransfers[d.TransferID]
		l.emitAudit(AuditEvent{
			Op:            "representment_auto_upheld",
			Actor:         "SweepExpiredRepresentments",
			TraceID:       id,
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Accounts:      []AccountID{pt.From, pt.To},
			Details: map[string]any{
				"dispute_id":  id,
				"transfer_id": d.TransferID,
				"reason":      "evidence window expired with no representment",
			},
		})
	}
	return resolved, errors.Join(errs...)
}

// GetRepresentment returns the representment submitted against the
// dispute, or false when no evidence was ever submitted. The returned
// record's Adjudicated is false until AdjudicateRepresentment records
// the decision.
func (l *Ledger) GetRepresentment(disputeID string) (Representment, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	r, ok := l.representments[disputeID]
	return r, ok
}
