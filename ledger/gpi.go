package ledger

import (
	"crypto/rand"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// SWIFT gpi UETR cross-border payment tracking (LG-165).
//
// Every payout instruction (see payout.go) is a cross-border payment
// initiation: at draft time CreatePayout assigns it a UETR — the
// Unique End-to-end Transaction Reference, a UUID v4 per RFC 4122 —
// and opens a gpi tracking record in the "tracked" state. The record
// mirrors the payment's journey through the correspondent banking
// chain:
//
//	tracked -> ACSP -> ACSC -> ACCP
//	                     \-> RJCT (from tracked, ACSP, or ACSC)
//
//	ACSP  AcceptedSettlementInProcess — the bank took the instruction.
//	ACSC  AcceptedSettlementCompleted — settlement completed.
//	ACCP  AcceptedCustomerProfile     — credited to the beneficiary.
//	      Terminal: the money arrived.
//	RJCT  Rejected — refused, with the bank's reason code.
//	      Terminal: the money is not moving.
//
// The machine is forward-only: a track may jump ahead (a bank verdict
// may skip ACSP) but never moves backward, and same-state advances are
// idempotent no-ops — re-delivered bank verdicts are safe. RJCT
// requires a reason code: a rejection without a reason is refused
// fail-closed, because the audit trail must say why money did not
// move.
//
// Two drivers advance the machine:
//
//  1. The operator, via AdvanceGPI (POST /gpi/track/{uetr}/advance):
//     the human-readable leg — "the bank acknowledged the
//     instruction" — recorded with the operator's identity.
//  2. The bank, via pain.002 status reports (see
//     ApplyPain002Report): when a report's transaction names a
//     transfer linked to a UETR (see LinkGPITransfer), the bank's
//     verdict advances the mirror — ACSC -> ACSC, ACCP -> ACCP,
//     RJCT -> RJCT with the bank's reason code. The EndToEndId stays
//     the pain.001 idempotency key; the gpi track only mirrors, and
//     it never moves backward past a state the operator already
//     recorded (a late ACSC report does not un-settle an ACCP track).
//
// Tracks, the payout index, and the transfer index are in-memory only,
// like payout instructions themselves (see payout.go): the
// hash-chained gpi_status audit events are the durable record, the
// same convention as the ACH return counts (see ach_return.go).

// gpi tracking errors.
var (
	// ErrGPIUnknownUETR is returned when an operation names a UETR the
	// ledger never tracked: tracks are created only by CreatePayout,
	// so an unknown UETR is a client error, not a silent drop.
	ErrGPIUnknownUETR = errors.New("ledger: unknown gpi UETR")
	// ErrGPIInvalidStatus is returned when AdvanceGPI is asked for a
	// status outside the gpi vocabulary
	// (tracked/ACSP/ACSC/ACCP/RJCT): unknown statuses are refused, not
	// coerced — like the receipt vocabulary in pain001_status.go.
	ErrGPIInvalidStatus = errors.New("ledger: invalid gpi status: want tracked, ACSP, ACSC, ACCP or RJCT")
	// ErrGPIInvalidTransition is returned when AdvanceGPI is asked for
	// a move the forward-only machine does not allow: backward moves
	// and moves out of the terminal ACCP/RJCT states. The record is
	// left untouched.
	ErrGPIInvalidTransition = errors.New("ledger: illegal gpi status transition")
	// ErrGPIReasonRequired is returned when AdvanceGPI moves a track to
	// RJCT without a reason code: a rejection must say why, and the
	// reason rides the audit event.
	ErrGPIReasonRequired = errors.New("ledger: gpi rejection requires a reason code")
	// ErrGPITransferConflict is returned by LinkGPITransfer when the
	// transfer is already linked to a different UETR: one transfer
	// carries one UETR end-to-end, so a second link is refused rather
	// than silently re-pointed.
	ErrGPITransferConflict = errors.New("ledger: transfer is already linked to a different gpi UETR")
	// ErrGPIEmptyTransferID is returned by LinkGPITransfer when the
	// transfer ID is empty. It is a 400-class error.
	ErrGPIEmptyTransferID = errors.New("ledger: gpi link transfer ID must not be empty")
)

// gpi lifecycle states (see GPITrack). The numeric order is the
// forward direction of the machine: AdvanceGPI allows any strictly
// forward move, so a bank verdict may skip ACSP, but never a backward
// one. RJCT shares ACCP's rank — both are terminal, and neither may
// move to the other.
const (
	GPIStatusTracked = "tracked"
	GPIStatusACSP    = "ACSP"
	GPIStatusACSC    = "ACSC"
	GPIStatusACCP    = "ACCP"
	GPIStatusRJCT    = "RJCT"
)

// gpiStatusRank orders the states for the forward-only machine.
func gpiStatusRank(s string) (int, bool) {
	switch s {
	case GPIStatusTracked:
		return 0, true
	case GPIStatusACSP:
		return 1, true
	case GPIStatusACSC:
		return 2, true
	case GPIStatusACCP, GPIStatusRJCT:
		return 3, true
	}
	return 0, false
}

// GPIEvent is one recorded transition of a tracked payment: the
// from/to states, when it happened, who drove it, and — for
// rejections — the bank's reason code.
type GPIEvent struct {
	From       string    `json:"from"`
	To         string    `json:"to"`
	At         time.Time `json:"at"`
	Actor      string    `json:"actor,omitempty"`
	ReasonCode string    `json:"reason_code,omitempty"`
}

// GPITrack is the gpi tracking record of one cross-border payment:
// its UETR, the payout instruction that opened it, the optionally
// linked transfer (the pain.002 join key), the current status, the
// rejection reason when rejected, and the full transition history.
type GPITrack struct {
	UETR       string     `json:"uetr"`
	PayoutID   string     `json:"payout_id,omitempty"`
	TransferID string     `json:"transfer_id,omitempty"`
	Status     string     `json:"status"`
	ReasonCode string     `json:"reason_code,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	History    []GPIEvent `json:"history"`
}

// newUETR generates a UETR: a UUID v4 per RFC 4122, rendered in the
// canonical 8-4-4-4-12 lowercase hex form. crypto/rand is the only
// source — a UETR must be unpredictable, so math/rand is not used. A
// read failure is fatal to the process: a payout without a UETR must
// never be created, and there is no honest fallback randomness.
func newUETR() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("ledger: gpi UETR randomness unavailable: %v", err))
	}
	// RFC 4122 section 4.1.3: version 4 (random).
	b[6] = (b[6] & 0x0f) | 0x40
	// RFC 4122 section 4.1.1: variant 10xx.
	b[8] = (b[8] & 0x3f) | 0x80
	const hexd = "0123456789abcdef"
	var sb strings.Builder
	sb.Grow(36)
	for i, v := range b {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			sb.WriteByte('-')
		}
		sb.WriteByte(hexd[v>>4])
		sb.WriteByte(hexd[v&0x0f])
	}
	return sb.String()
}

// registerGPITrackLocked opens the gpi tracking record for a freshly
// drafted payout: status "tracked", indexed by UETR and by payout ID.
// Callers must hold the write lock; called from CreatePayout next to
// the instruction write, so a payout that fails validation opens no
// track.
func (l *Ledger) registerGPITrackLocked(uetr, payoutID string, now time.Time) {
	l.gpiTracks[uetr] = &GPITrack{
		UETR:      uetr,
		PayoutID:  payoutID,
		Status:    GPIStatusTracked,
		CreatedAt: now,
		UpdatedAt: now,
	}
	l.gpiByPayout[payoutID] = uetr
	l.gpiTracked.Add(1)
}

// GetGPITrack returns the tracking record for uetr, or false when no
// such UETR was ever tracked. The returned record is a copy —
// callers cannot mutate ledger state through it.
func (l *Ledger) GetGPITrack(uetr string) (GPITrack, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	t, ok := l.gpiTracks[uetr]
	if !ok {
		return GPITrack{}, false
	}
	cp := *t
	cp.History = append([]GPIEvent(nil), t.History...)
	return cp, true
}

// GPITracks returns every tracking record sorted by UETR: the read
// side of the registry, backing the Reconcile disclosure.
func (l *Ledger) GPITracks() []GPITrack {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]GPITrack, 0, len(l.gpiTracks))
	for _, t := range l.gpiTracks {
		cp := *t
		cp.History = append([]GPIEvent(nil), t.History...)
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UETR < out[j].UETR })
	return out
}

// LinkGPITransfer associates a transfer with a tracked UETR, so bank
// verdicts on that transfer (pain.002 reports naming the transfer's
// EndToEndId) advance the gpi mirror automatically. The link is the
// operator's attestation that this transfer settles the tracked
// payment: an unknown UETR is refused, and a transfer already linked
// to a different UETR is refused rather than silently re-pointed —
// one transfer carries one UETR end-to-end. Re-linking the same pair
// is a no-op. Every link enters the hash-chained audit trail.
func (l *Ledger) LinkGPITransfer(uetr, transferID, actor string) (GPITrack, error) {
	if transferID == "" {
		return GPITrack{}, ErrGPIEmptyTransferID
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.gpiTracks[uetr]
	if !ok {
		return GPITrack{}, fmt.Errorf("%w: %q", ErrGPIUnknownUETR, uetr)
	}
	if existing, linked := l.gpiByTransfer[transferID]; linked && existing != uetr {
		return GPITrack{}, fmt.Errorf("%w: transfer %q already tracked under a different UETR", ErrGPITransferConflict, transferID)
	}
	if t.TransferID == transferID {
		cp := *t
		cp.History = append([]GPIEvent(nil), t.History...)
		return cp, nil
	}
	t.TransferID = transferID
	t.UpdatedAt = time.Now().UTC()
	l.gpiByTransfer[transferID] = uetr
	l.emitAudit(AuditEvent{
		Op:            "gpi_linked",
		Actor:         actor,
		TraceID:       uetr,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"uetr":        uetr,
			"transfer_id": transferID,
			"payout_id":   t.PayoutID,
		},
	})
	cp := *t
	cp.History = append([]GPIEvent(nil), t.History...)
	return cp, nil
}

// AdvanceGPI moves a tracked payment to a new gpi status: the
// operator-driven leg of the state machine (the bank-driven leg is
// the pain.002 auto-advance in ApplyPain002Report).
//
// The machine is forward-only: any strictly forward move is legal —
// a bank acknowledgement may skip ACSP — but backward moves and moves
// out of the terminal ACCP/RJCT states are refused with
// ErrGPIInvalidTransition. Re-advancing to the current status is an
// idempotent no-op, so retried operator calls are safe. Moving to
// RJCT requires a reason code (ErrGPIReasonRequired otherwise): the
// reason rides the audit event, because a rejection must say why.
//
// Every applied transition emits one hash-chained gpi_status audit
// event and appends one history entry; reaching ACCP bumps the
// settled counter, reaching RJCT the rejected counter.
func (l *Ledger) AdvanceGPI(uetr, status, reasonCode, actor string) (GPITrack, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.gpiTracks[uetr]
	if !ok {
		return GPITrack{}, fmt.Errorf("%w: %q", ErrGPIUnknownUETR, uetr)
	}
	toRank, ok := gpiStatusRank(status)
	if !ok {
		return GPITrack{}, fmt.Errorf("%w: %q", ErrGPIInvalidStatus, status)
	}
	fromRank, _ := gpiStatusRank(t.Status)
	if t.Status == status {
		// Idempotent replay: already there, nothing to do.
		cp := *t
		cp.History = append([]GPIEvent(nil), t.History...)
		return cp, nil
	}
	if fromRank == 3 || toRank <= fromRank {
		// Terminal states never move, and the machine never moves
		// backward: a payment that reached ACCP cannot become ACSC
		// again, and a rejected payment cannot be un-rejected.
		return GPITrack{}, fmt.Errorf("%w: %s -> %s", ErrGPIInvalidTransition, t.Status, status)
	}
	if status == GPIStatusRJCT && strings.TrimSpace(reasonCode) == "" {
		return GPITrack{}, ErrGPIReasonRequired
	}
	return l.applyGPITransitionLocked(t, status, strings.TrimSpace(reasonCode), actor), nil
}

// applyGPITransitionLocked applies a validated forward move: state,
// reason, history, audit event, and the terminal counters. Callers
// must hold the write lock and must have validated the move.
func (l *Ledger) applyGPITransitionLocked(t *GPITrack, status, reasonCode, actor string) GPITrack {
	from := t.Status
	now := time.Now().UTC()
	t.Status = status
	t.ReasonCode = reasonCode
	t.UpdatedAt = now
	t.History = append(t.History, GPIEvent{From: from, To: status, At: now, Actor: actor, ReasonCode: reasonCode})
	details := map[string]any{
		"uetr":  t.UETR,
		"from":  from,
		"to":    status,
		"actor": actor,
	}
	if t.PayoutID != "" {
		details["payout_id"] = t.PayoutID
	}
	if t.TransferID != "" {
		details["transfer_id"] = t.TransferID
	}
	if reasonCode != "" {
		// The bank's rejection reason, kept verbatim — it is the
		// bank's vocabulary, like the receipt Reason in
		// pain001_status.go.
		details["reason_code"] = reasonCode
	}
	l.emitAudit(AuditEvent{
		Op:            "gpi_status",
		Actor:         actor,
		TraceID:       t.UETR,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details:       details,
	})
	// LG-185: the gpi transition drives the payout's SLA clock — every
	// advance refreshes the watch's last-progress timestamp, and a
	// terminal transition (ACCP/RJCT) closes it. This covers the
	// operator leg (AdvanceGPI) and the bank leg (the pain.002
	// auto-advance in advanceGPITowardLocked) alike: both funnel
	// through here. Orthogonal to the gpi state tracking itself —
	// this is the SLA compliance layer reading the same transitions.
	l.updateSLAWatchForGPILocked(t.PayoutID, status, now)
	if status == GPIStatusACCP {
		l.gpiSettled.Add(1)
	} else if status == GPIStatusRJCT {
		l.gpiRejected.Add(1)
	}
	cp := *t
	cp.History = append([]GPIEvent(nil), t.History...)
	return cp
}

// advanceGPITowardLocked moves the track linked to transferID toward
// target, but only forward: when the track already sits at or beyond
// the target — the operator recorded a later state, or a verdict was
// already mirrored — it is left alone. This is the pain.002
// auto-advance: the bank's verdict is authoritative going forward, but
// a late report never un-settles a track. Callers must hold the write
// lock. A missing link is a no-op (not every transfer is tracked).
func (l *Ledger) advanceGPITowardLocked(transferID, target, reasonCode, actor string) (GPITrack, bool, error) {
	toRank, ok := gpiStatusRank(target)
	if !ok {
		return GPITrack{}, false, fmt.Errorf("%w: %q", ErrGPIInvalidStatus, target)
	}
	uetr, linked := l.gpiByTransfer[transferID]
	if !linked {
		return GPITrack{}, false, nil
	}
	t, ok := l.gpiTracks[uetr]
	if !ok {
		// Unreachable: links are only created for live tracks and
		// tracks are never deleted. Fail loud rather than silently
		// dropping a bank verdict.
		return GPITrack{}, false, fmt.Errorf("%w: %q", ErrGPIUnknownUETR, uetr)
	}
	fromRank, _ := gpiStatusRank(t.Status)
	if toRank <= fromRank {
		// Already at or beyond the verdict: the mirror stays. A late
		// ACSC report does not move an ACCP track backward.
		cp := *t
		cp.History = append([]GPIEvent(nil), t.History...)
		return cp, false, nil
	}
	if target == GPIStatusRJCT && strings.TrimSpace(reasonCode) == "" {
		return GPITrack{}, false, ErrGPIReasonRequired
	}
	return l.applyGPITransitionLocked(t, target, strings.TrimSpace(reasonCode), actor), true, nil
}

// GPICounts reports the gpi lifecycle counters: payments that started
// tracking, payments that settled (ACCP), and payments that were
// rejected (RJCT). Operational metrics (surfaced on GET /metrics as
// ledger_gpi_tracked_total and friends), not books: in-memory only,
// like the mandate debit counter.
func (l *Ledger) GPICounts() (tracked, settled, rejected uint64) {
	return l.gpiTracked.Load(), l.gpiSettled.Load(), l.gpiRejected.Load()
}
