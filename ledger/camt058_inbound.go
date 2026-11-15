package ledger

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ISO 20022 camt.058.001.08 Notification to Receive Cancellation
// Advice inbound processing (LG-421): the inbound half of the
// camt.058 lifecycle, the mirror of the LG-404 export.
//
// ExportCAMT058 renders this ledger's own cancellation advice —
// the account owner telling its bank to disregard a notification
// to receive it sent earlier. This module consumes the advice a
// counterparty account owner sent to this ledger, acting as the
// account bank: it cancels the expected receipt LG-420 registered
// for the named camt.057 notification, and nothing else. The two
// directions are orthogonal by construction:
//
//   - LG-404 exports: the ledger tells its bank to disregard one
//     of its own pre-advices (outbound, read-only).
//   - LG-421 imports: a counterparty tells the ledger to
//     disregard one of its pre-advices (inbound, state-changing
//     only in the expected-receipts registry: one cancellation
//     flag on the stored record).
//
// ParseCAMT058 reuses the camt058.go wire structs, reading them
// in the reverse direction: the same `camt.058.001.08`
// `NtfctnToRcvCxlAdvc` document. The namespace rides the
// Document element, so a document in the wrong schema version
// fails the unmarshal — the version gate is the namespace, not a
// string check (the camt.056 inbound convention, LG-414).
//
// Association is the original notification, not a transfer: the
// advice names it twice — OrgnlMsgId (the GrpHdr MsgId of the
// camt.057 it cancels) and OrgnlNtfctnId (that notification's
// Ntfctn Id) — and both must agree with the stored receipt. An
// advice naming a notification the ledger never registered is
// refused fail-closed and parked in the repair queue, never
// auto-built and then cancelled: the ledger never invents a
// notification in order to withdraw it. An advice whose
// OrgnlNtfctnId disagrees with the stored receipt's
// NotificationID is refused the same way: the advice must name
// the same notification both ways.
//
// A cancellation is a forecast change only, not a booking:
// ApplyCAMT058 writes no journal rows, bumps no version, moves
// no money, and opens no payout — it only cancels the forecast.
// The cancelled notification stops counting as the fifth
// intraday-liquidity source on its expected value date and every
// later day (see liquidity.go); the record itself is kept, so
// the audit trail can still show what was expected and what
// withdrew it.
//
// Idempotency has two layers, both no-op successes with
// Duplicate set and no second audit event: (a) the same advice
// MsgId applied before returns the stored apply result, kept in
// the per-ledger camt058Applied registry (in-memory only, like
// camt055Applied: the hash-chained
// `camt057_notification_cancelled` audit events are the durable
// record); (b) a different advice MsgId cancelling an
// already-cancelled notification returns the stored (cancelled)
// receipt. An advice MsgId reused for a different original is
// refused with ErrCAMT058MsgIDConflict — one message ID names
// one advice.
//
// Fail-closed parking (the pacs.004 convention, LG-413): an
// unknown original parks in the repair queue for operator
// resolution, and the typed error carries the repair record ID.
// A document the ledger cannot even parse is parked the same way
// by ParkCAMT058Unparsed — visibility parking, never a silent
// drop of the counterparty's message. Repair records parked by
// this module are visibility parking, not resubmittable
// instructions: their Maker is deliberately empty, so the
// CreatePayout gate can never pass them.
//
// Audit: a fresh cancellation emits `camt057_notification_cancelled`
// with the advice's MsgId, the original notification's MsgId, and
// the reason verbatim — the same op name as the notification it
// withdraws, so the trail reads as one lifecycle.

// camt.058 inbound errors.
var (
	// ErrInvalidCAMT058 is returned by ParseCAMT058 for malformed
	// XML or a document missing a required field, and by
	// ApplyCAMT058 when an advice's OrgnlNtfctnId disagrees with
	// the stored receipt's NotificationID: the counterparty sent
	// something the ledger cannot read as a cancellation advice,
	// or an advice that does not name the same notification both
	// ways. The HTTP layer parks it in the repair queue and maps
	// it to 400 (parse) or 422 (apply).
	ErrInvalidCAMT058 = errors.New("ledger: invalid camt.058 notification to receive cancellation advice")
	// ErrCAMT058UnknownNotification is returned by ApplyCAMT058
	// when the advice's OrgnlMsgId names no notification the
	// ledger ever registered: the ledger never invents a
	// notification in order to cancel it. The advice is parked
	// in the repair queue, not dropped.
	ErrCAMT058UnknownNotification = errors.New("ledger: camt.058 inbound: original notification is unknown")
	// ErrCAMT058MsgIDConflict is returned when a GrpHdr MsgId
	// already applied names a different original notification:
	// one message ID names one advice. It is a 422-class error.
	ErrCAMT058MsgIDConflict = errors.New("ledger: camt.058 MsgId already applied for a different original notification")
)

// CAMT058Advice is one parsed inbound camt.058.001.08
// notification to receive cancellation advice: the advice's own
// MsgId, the original notification it withdraws (OrgnlMsgId and
// OrgnlNtfctnId, both required), and the operator's free-form
// cancellation reason carried verbatim — never interpreted,
// never translated, empty when the document states none (the
// camt058.go free-form-reason convention).
type CAMT058Advice struct {
	// MsgID is the advice's GrpHdr MsgId: the audit trace and
	// the replay key.
	MsgID string `json:"msg_id"`
	// OrgnlMsgID is OrgnlNtfctn/OrgnlMsgId: the GrpHdr MsgId of
	// the camt.057 notification being withdrawn.
	OrgnlMsgID string `json:"orgnl_msg_id"`
	// OrgnlNtfctnID is OrgnlNtfctn/OrgnlNtfctnId: that
	// notification's Ntfctn Id, which must agree with the stored
	// receipt's NotificationID.
	OrgnlNtfctnID string `json:"orgnl_ntfctn_id"`
	// Reason is CxlRsn/Rsn/Prtry verbatim, "" when absent.
	Reason string `json:"reason,omitempty"`
}

// CAMT058ApplyResult is the outcome of ApplyCAMT058.
type CAMT058ApplyResult struct {
	// Receipt is the cancelled notification's stored receipt (or
	// the stored one on either duplicate path).
	Receipt ExpectedReceipt `json:"receipt"`
	// RepairID is set on the parked fail-closed paths (unknown
	// original, OrgnlNtfctnId disagreement): the repair-queue
	// record holding the advice for operator resolution.
	RepairID string `json:"repair_id,omitempty"`
	// Duplicate reports whether the advice was already applied
	// under the same MsgId, or the notification was already
	// cancelled by another advice: a no-op success, not an
	// error.
	Duplicate bool `json:"duplicate,omitempty"`
}

// ParseCAMT058 parses one inbound camt.058.001.08 notification
// to receive cancellation advice into a CAMT058Advice, reusing
// the camt058.go wire structs in the reverse direction. It
// validates the whole document before returning anything: a
// missing MsgId, a missing OrgnlMsgId, or a missing
// OrgnlNtfctnId is a parse refusal — ApplyCAMT058 never sees a
// half-read advice. The reason is carried verbatim and is
// optional: an absent CxlRsn block reads as "".
func ParseCAMT058(data []byte) (CAMT058Advice, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return CAMT058Advice{}, fmt.Errorf("%w: empty document", ErrInvalidCAMT058)
	}
	var doc camt058xDocument
	if err := xml.Unmarshal(data, &doc); err != nil {
		return CAMT058Advice{}, fmt.Errorf("%w: %v", ErrInvalidCAMT058, err)
	}
	advc := doc.NtfctnToRcvCxlAdvc
	msgID := strings.TrimSpace(advc.GrpHdr.MsgID)
	if msgID == "" {
		return CAMT058Advice{}, fmt.Errorf("%w: missing GrpHdr/MsgId", ErrInvalidCAMT058)
	}
	orgnlMsgID := strings.TrimSpace(advc.OrgnlNtfctn.OrgnlMsgId)
	if orgnlMsgID == "" {
		return CAMT058Advice{}, fmt.Errorf("%w: missing OrgnlNtfctn/OrgnlMsgId", ErrInvalidCAMT058)
	}
	orgnlNtfctnID := strings.TrimSpace(advc.OrgnlNtfctn.OrgnlNtfctnId)
	if orgnlNtfctnID == "" {
		return CAMT058Advice{}, fmt.Errorf("%w: missing OrgnlNtfctn/OrgnlNtfctnId", ErrInvalidCAMT058)
	}
	reason := ""
	if advc.CxlRsn != nil {
		reason = advc.CxlRsn.Rsn.Prtry
	}
	return CAMT058Advice{
		MsgID:         msgID,
		OrgnlMsgID:    orgnlMsgID,
		OrgnlNtfctnID: orgnlNtfctnID,
		Reason:        reason,
	}, nil
}

// camt058LenientDocument is the namespace-agnostic wire shape
// ParkCAMT058Unparsed reads: a document ParseCAMT058 refused
// for its version may still name the advice, and the namespace
// is the only thing the strict parse gates on. Unknown elements
// are skipped, the camt.029 convention.
type camt058LenientDocument struct {
	XMLName            xml.Name           `xml:"Document"`
	NtfctnToRcvCxlAdvc camt058LenientAdvc `xml:"NtfctnToRcvCxlAdvc"`
}

type camt058LenientAdvc struct {
	GrpHdr      camt058LenientGrpHdr      `xml:"GrpHdr"`
	OrgnlNtfctn camt058LenientOrgnlNtfctn `xml:"OrgnlNtfctn"`
}

type camt058LenientGrpHdr struct {
	MsgID string `xml:"MsgId"`
}

type camt058LenientOrgnlNtfctn struct {
	OrgnlMsgId string `xml:"OrgnlMsgId"`
}

// ParkCAMT058Unparsed best-effort parks a camt.058 document
// ParseCAMT058 refused, when the document still names an advice:
// the GrpHdr MsgId or the OrgnlMsgId it carries. It returns the
// repair record ID, or "" when the document names nothing
// parkable (garbage bytes, an empty shell): there is no advice
// for the operator to find, so there is nothing to park. A
// parking failure (a full queue) is returned so the caller can
// fail loud rather than silently drop the counterparty's message.
// The reason code is invalid_format — the document was present
// but unreadable as a cancellation advice.
func (l *Ledger) ParkCAMT058Unparsed(data []byte, actor string) (string, error) {
	var doc camt058LenientDocument
	if err := xml.Unmarshal(data, &doc); err != nil {
		return "", nil
	}
	id := strings.TrimSpace(doc.NtfctnToRcvCxlAdvc.GrpHdr.MsgID)
	if id == "" {
		id = strings.TrimSpace(doc.NtfctnToRcvCxlAdvc.OrgnlNtfctn.OrgnlMsgId)
	}
	if id == "" {
		return "", nil
	}
	rec, err := l.EnqueueRepair(PayoutInstruction{
		ID: id,
		// Maker intentionally empty: see the package comment.
		Destination: "camt058-inbound:" + id,
	}, RepairReasonInvalidFormat, actor)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// parkCAMT058Advice parks a parsed camt.058 advice that failed
// closed, and returns the repair record ID. The snapshot's Maker
// is deliberately empty (see the package comment).
func (l *Ledger) parkCAMT058Advice(req CAMT058Advice, reasonCode, actor string) (string, error) {
	rec, err := l.EnqueueRepair(PayoutInstruction{
		ID:          req.MsgID,
		Destination: "camt058-inbound:" + req.MsgID,
		// Maker intentionally empty: see the package comment.
	}, reasonCode, actor)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// ApplyCAMT058 applies one parsed inbound camt.058.001.08
// notification to receive cancellation advice: it marks the
// expected receipt LG-420 registered for the named camt.057
// notification cancelled, and nothing else.
//
// The advice's OrgnlMsgId must name a registered notification
// and its OrgnlNtfctnId must agree with that receipt's
// NotificationID — otherwise the advice is refused fail-closed
// and parked in the repair queue (unknown_route for an unknown
// original, invalid_format for a disagreeing notification ID).
// The ledger never invents a notification in order to cancel
// it.
//
// Only the forecast changes: no journal rows, no version bump,
// no money moved. The cancelled notification stops counting as
// the fifth intraday-liquidity source (see liquidity.go); the
// record is kept for audit, with Cancelled, CancelledAt,
// CancelMsgID, and the verbatim CancelReason.
//
// Idempotency has two layers, both no-op successes with
// Duplicate set and no second audit event: re-applying an advice
// MsgId already applied returns the stored apply result, and a
// different advice MsgId cancelling an already-cancelled
// notification returns the stored (cancelled) receipt. An advice
// MsgId reused for a different original is refused with
// ErrCAMT058MsgIDConflict.
//
// Locking: the replay read and the receipt read run under the
// read lock; the cancellation section holds the write lock (the
// re-checks, the registry writes, and the audit event are one
// atomic section); the repair parking runs outside any held lock
// because EnqueueRepair takes l.mu itself.
func (l *Ledger) ApplyCAMT058(req CAMT058Advice, actor string) (CAMT058ApplyResult, error) {
	if strings.TrimSpace(req.MsgID) == "" {
		return CAMT058ApplyResult{}, fmt.Errorf("%w: missing GrpHdr/MsgId", ErrInvalidCAMT058)
	}
	if actor == "" {
		actor = "ApplyCAMT058"
	}

	// Replay read and receipt read: the same advice MsgId
	// applied before is a no-op success, whatever the ledger's
	// current state.
	l.mu.RLock()
	applied, replayed := l.camt058Applied[req.MsgID]
	stored, known := l.expectedReceipts[req.OrgnlMsgID]
	l.mu.RUnlock()
	if replayed {
		// One MsgId names one advice: a different original
		// under it is a conflict.
		if applied.Receipt.MsgID != req.OrgnlMsgID || applied.Receipt.NotificationID != req.OrgnlNtfctnID {
			return CAMT058ApplyResult{}, fmt.Errorf("%w: MsgId %q", ErrCAMT058MsgIDConflict, req.MsgID)
		}
		return CAMT058ApplyResult{Receipt: applied.Receipt, Duplicate: true}, nil
	}
	if !known {
		// The parked fail-closed path, outside any held lock:
		// an unknown original goes to the repair queue. The
		// ledger never invents a notification to cancel.
		repairID, err := l.parkCAMT058Advice(req, RepairReasonUnknownRoute, actor)
		if err != nil {
			return CAMT058ApplyResult{}, fmt.Errorf("%w: %q (repair parking failed: %v)", ErrCAMT058UnknownNotification, req.OrgnlMsgID, err)
		}
		return CAMT058ApplyResult{RepairID: repairID}, fmt.Errorf("%w: %q", ErrCAMT058UnknownNotification, req.OrgnlMsgID)
	}
	if stored.NotificationID != req.OrgnlNtfctnID {
		// The advice must name the same notification both
		// ways: an OrgnlNtfctnId that disagrees with the
		// stored receipt parks as invalid_format.
		repairID, err := l.parkCAMT058Advice(req, RepairReasonInvalidFormat, actor)
		if err != nil {
			return CAMT058ApplyResult{}, fmt.Errorf("%w: OrgnlNtfctnId %q is not the notification ID of %q (repair parking failed: %v)", ErrInvalidCAMT058, req.OrgnlNtfctnID, req.OrgnlMsgID, err)
		}
		return CAMT058ApplyResult{RepairID: repairID}, fmt.Errorf("%w: OrgnlNtfctnId %q is not the notification ID of %q", ErrInvalidCAMT058, req.OrgnlNtfctnID, req.OrgnlMsgID)
	}

	// Cancellation section: re-check the replay registry and
	// the receipt state under the write lock — a racing advice
	// for the same MsgId, or a racing cancellation of the same
	// notification, converges here instead of double-cancelling.
	l.mu.Lock()
	defer l.mu.Unlock()
	if applied, replayed := l.camt058Applied[req.MsgID]; replayed {
		if applied.Receipt.MsgID != req.OrgnlMsgID || applied.Receipt.NotificationID != req.OrgnlNtfctnID {
			return CAMT058ApplyResult{}, fmt.Errorf("%w: MsgId %q", ErrCAMT058MsgIDConflict, req.MsgID)
		}
		return CAMT058ApplyResult{Receipt: applied.Receipt, Duplicate: true}, nil
	}
	receipt, known := l.expectedReceipts[req.OrgnlMsgID]
	if !known {
		// Unreachable: receipts are never deleted, and the
		// read above saw it — but never invent one.
		return CAMT058ApplyResult{}, fmt.Errorf("%w: %q", ErrCAMT058UnknownNotification, req.OrgnlMsgID)
	}
	if receipt.NotificationID != req.OrgnlNtfctnID {
		return CAMT058ApplyResult{}, fmt.Errorf("%w: OrgnlNtfctnId %q is not the notification ID of %q", ErrInvalidCAMT058, req.OrgnlNtfctnID, req.OrgnlMsgID)
	}
	if receipt.Cancelled {
		// Second advice on an already-cancelled notification:
		// a no-op success returning the stored (cancelled)
		// receipt. Record this advice MsgId too, so its own
		// replay converges the same way.
		res := CAMT058ApplyResult{Receipt: receipt, Duplicate: true}
		if l.camt058Applied == nil {
			l.camt058Applied = make(map[string]CAMT058ApplyResult)
		}
		l.camt058Applied[req.MsgID] = CAMT058ApplyResult{Receipt: receipt}
		return res, nil
	}
	receipt.Cancelled = true
	receipt.CancelledAt = time.Now().UTC()
	receipt.CancelMsgID = req.MsgID
	receipt.CancelReason = req.Reason
	l.expectedReceipts[req.OrgnlMsgID] = receipt
	res := CAMT058ApplyResult{Receipt: receipt}
	if l.camt058Applied == nil {
		l.camt058Applied = make(map[string]CAMT058ApplyResult)
	}
	l.camt058Applied[req.MsgID] = res
	l.emitAudit(AuditEvent{
		Op:       "camt057_notification_cancelled",
		Actor:    actor,
		TraceID:  req.MsgID,
		Accounts: []AccountID{receipt.Account},
		Details: map[string]any{
			"msg_id":       req.MsgID,
			"orgnl_msg_id": req.OrgnlMsgID,
			"reason":       req.Reason,
		},
	})
	return res, nil
}

// GetCAMT058Applied returns the apply result first returned under
// advice msgID, or false when no advice was ever applied under
// it. The returned result is a copy — callers cannot mutate
// ledger state through it.
func (l *Ledger) GetCAMT058Applied(msgID string) (CAMT058ApplyResult, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	res, ok := l.camt058Applied[msgID]
	if !ok {
		return CAMT058ApplyResult{}, false
	}
	res.Receipt.Items = append([]ExpectedReceiptItem(nil), res.Receipt.Items...)
	return res, true
}
