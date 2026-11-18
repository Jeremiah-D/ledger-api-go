package ledger

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// SWIFT FIN MT292 cancellation of notification to receive inbound
// processing (LG-452): the inbound half of the MT292 lifecycle,
// the mirror of the LG-435 export — and the FIN twin of the
// camt.058 inbound half (LG-421).
//
// ExportMT292 renders this ledger's own cancellation request —
// the account owner telling its bank to disregard an MT210 notice
// it sent earlier. This module consumes the cancellation request
// a counterparty account owner sent to this ledger, acting as the
// account bank: it cancels the expected receipt LG-448
// registered for the named MT210 notification, and nothing else.
// The two directions are orthogonal by construction:
//
//   - LG-435 exports: the ledger tells its bank to disregard one
//     of its own pre-advices (outbound, read-only).
//   - LG-452 imports: a counterparty tells the ledger to
//     disregard one of its pre-advices (inbound, state-changing
//     only in the expected-receipts registry: one cancellation
//     flag on the stored record).
//
// ParseMT292 builds a new value object — the outbound export
// renders with strings.Builder, so there is no wire struct to
// reuse in reverse (the pain.009 inbound convention, LG-422).
// Parsing is strict, along ParseMT210 (LG-448) and ParseMT900910
// (LG-209): the block structure, the field order, and every field
// value are validated, and anything outside the four-field core
// set (:20: :21: :11S: :79:) is refused — a cancellation must
// not lose content quietly. CRLF line endings are accepted; a
// trailing newline after the `-}` trailer is tolerated. The :79:
// field, once started, consumes every remaining block-4 line as
// a continuation (the MT900/MT910 :72: precedent, LG-209): it is
// the last field, so a continuation line may itself start with a
// colon without being mistaken for a new field.
//
// Association is the original notification, not a transfer: the
// request names it by :21: (the MT210's :20:, the
// expectedReceipts key) and by :11S: (the original message type,
// which must be "210" — the ledger only ever registers MT210
// notifications, so cancelling any other message type has no
// registration to withdraw and is refused fail-closed). The
// :11S: date is shape-checked only (a 6!n YYMMDD calendar date):
// the ledger records no MT210 issue date (the camt.057
// limitation, LG-404), so it is never compared against stored
// state, and no comparison is invented.
//
// A cancellation is a forecast change only, not a booking:
// ApplyMT292 writes no journal rows, bumps no version, moves no
// money, and opens no payout — it only cancels the forecast. The
// cancelled notification stops counting as the fifth
// intraday-liquidity source on its expected value date and every
// later day (see liquidity.go); the record itself is kept, so
// the audit trail can still show what was expected and what
// withdrew it.
//
// Idempotency has two layers, both no-op successes with
// Duplicate set and no second audit event: (a) the same request
// :20: applied before returns the stored apply result, kept in
// the per-ledger mt292Applied registry (in-memory only, like
// camt058Applied: the hash-chained
// `mt210_notification_cancelled` audit events are the durable
// record); (b) a different request :20: cancelling an
// already-cancelled notification returns the stored (cancelled)
// receipt. A request :20: reused for a different original is
// refused with ErrMT292MsgIDConflict — one message reference
// names one request.
//
// Fail-closed parking (the pacs.004 convention, LG-413): an
// unknown original parks in the repair queue for operator
// resolution, and the typed error carries the repair record ID.
// A document the ledger cannot even parse is parked the same way
// by ParkMT292Unparsed — visibility parking, never a silent
// drop of the counterparty's message. Repair records parked by
// this module are visibility parking, not resubmittable
// instructions: their Maker is deliberately empty, so the
// CreatePayout gate can never pass them.
//
// Audit: a fresh cancellation emits `mt210_notification_cancelled`
// with the request's :20:, the original notification's :20:, and
// the reason verbatim — the same op name as the notification it
// withdraws, so the trail reads as one lifecycle (the camt.058
// precedent: `camt057_notification_cancelled`).

// MT292 inbound errors.
var (
	// ErrInvalidMT292 is returned by ParseMT292 for a document
	// the ledger cannot read as a cancellation of notification
	// to receive, and by ApplyMT292 for a cancellation missing
	// its :20:. The HTTP layer parks it in the repair queue
	// and maps it to 400 (parse) or 422 (apply).
	ErrInvalidMT292 = errors.New("ledger: invalid MT292 cancellation of notification to receive")
	// ErrMT292UnsupportedOriginal is returned by ParseMT292
	// (and ApplyMT292, defensively) when the :11S: original
	// message type is not "210": the ledger only ever
	// registers MT210 notifications, so cancelling any other
	// message type has no registration to withdraw. It is a
	// 422-class error.
	ErrMT292UnsupportedOriginal = errors.New("ledger: mt292 inbound: original message type is not 210")
	// ErrMT292UnknownNotification is returned by ApplyMT292
	// when the request's :21: names no notification the ledger
	// ever registered: the ledger never invents a notification
	// in order to cancel it. The request is parked in the
	// repair queue, not dropped.
	ErrMT292UnknownNotification = errors.New("ledger: mt292 inbound: original notification is unknown")
	// ErrMT292MsgIDConflict is returned when a :20: reference
	// already applied names a different original notification:
	// one message reference names one cancellation request. It
	// is a 422-class error.
	ErrMT292MsgIDConflict = errors.New("ledger: mt292 :20: already applied for a different original notification")
)

// MT292Cancellation is one parsed inbound SWIFT FIN MT292
// cancellation of notification to receive: the block-4 core
// fields plus the block 1/2 LT addresses. OriginalDate is the
// :11S: date in YYMMDD, shape-checked only — never compared
// against stored state, which holds no MT210 issue date.
// Reason is the :79: narrative lines joined with "\n",
// carried verbatim, "" when the document states none.
type MT292Cancellation struct {
	SenderLT   string
	ReceiverLT string
	// MsgID is :20:, this message's own reference and the
	// replay key.
	MsgID string `json:"msg_id"`
	// RelatedMsgID is :21:, the original MT210's :20: — the
	// expectedReceipts key of the notification being withdrawn.
	RelatedMsgID string `json:"related_msg_id"`
	// OriginalType is the :11S: original message type, always
	// "210".
	OriginalType string `json:"original_type"`
	// OriginalDate is the :11S: original issue date, YYMMDD.
	OriginalDate string `json:"original_date"`
	// Reason is the :79: narrative, verbatim, "" when absent.
	Reason string `json:"reason,omitempty"`
}

// MT292ApplyResult is the outcome of ApplyMT292.
type MT292ApplyResult struct {
	// Receipt is the cancelled notification's stored receipt (or
	// the stored one on either duplicate path).
	Receipt ExpectedReceipt `json:"receipt"`
	// RepairID is set on the parked fail-closed path (unknown
	// original): the repair-queue record holding the request
	// for operator resolution.
	RepairID string `json:"repair_id,omitempty"`
	// Duplicate reports whether the request was already applied
	// under the same :20:, or the notification was already
	// cancelled by another request: a no-op success, not an
	// error.
	Duplicate bool `json:"duplicate,omitempty"`
}

// mt292Reference validates a :20: or :21: reference: 1–16 SWIFT
// `x` characters, no braces in block-4 content (see
// mt103CheckX), no leading/trailing colon or slash, no double
// slash. The MT210 rule, under the MT292 error identity.
func mt292Reference(ref, field string) error {
	if ref == "" || len(ref) > mt103MaxSenderReference {
		return fmt.Errorf("%w: %s: want 1-%d characters, got %q", ErrInvalidMT292, field, mt103MaxSenderReference, ref)
	}
	if err := mt103CheckX(ref); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrInvalidMT292, field, err)
	}
	if strings.HasPrefix(ref, ":") || strings.HasPrefix(ref, "/") ||
		strings.HasSuffix(ref, ":") || strings.HasSuffix(ref, "/") ||
		strings.Contains(ref, "//") {
		return fmt.Errorf("%w: %s: leading/trailing colon or slash, or double slash: %q", ErrInvalidMT292, field, ref)
	}
	return nil
}

// mt292ParseLT extracts the LT address from a block 1 or block 2
// header line and returns it exactly as written. Block 1 has the
// form `{1:F01<LT><10 session/sequence digits>}`; block 2 has the
// form `{2:I292<LT><5 header characters>}` (priority U, delivery
// monitoring 3, obsolescence period 003). The MT210 rule, under
// the MT292 error identity.
func mt292ParseLT(line, prefix string, tailLen int) (string, error) {
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, "}") {
		return "", fmt.Errorf("%w: %q", ErrInvalidMT292, line)
	}
	inner := line[len(prefix) : len(line)-1]
	if len(inner) <= tailLen {
		return "", fmt.Errorf("%w: %q", ErrInvalidMT292, line)
	}
	lt, tail := inner[:len(inner)-tailLen], inner[len(inner)-tailLen:]
	// Block 1's tail is the session/sequence digits (digit-checked);
	// block 2's tail is header flags (any 5 characters: U3003).
	if tailLen == 10 {
		for _, c := range tail {
			if c < '0' || c > '9' {
				return "", fmt.Errorf("%w: %q", ErrInvalidMT292, line)
			}
		}
	}
	if !mt202ValidLT(lt) {
		return "", fmt.Errorf("%w: %q", ErrInvalidMT292, lt)
	}
	return lt, nil
}

// mt292ParseOriginal parses a :11S: body ("210" plus a 6!n YYMMDD
// original issue date) into its type and date. A type other than
// "210" is refused with ErrMT292UnsupportedOriginal; a malformed
// date is a shape error under ErrInvalidMT292. The date is
// returned in YYMMDD form, exactly as written.
func mt292ParseOriginal(body string) (typ, date string, err error) {
	if len(body) != 9 {
		return "", "", fmt.Errorf("%w: :11S: want 210YYMMDD (9 characters), got %q", ErrInvalidMT292, body)
	}
	if body[:3] != "210" {
		return "", "", fmt.Errorf("%w: :11S: original message type %q, want 210", ErrMT292UnsupportedOriginal, body[:3])
	}
	if _, err := time.Parse("060102", body[3:]); err != nil {
		return "", "", fmt.Errorf("%w: :11S: date %q: %w", ErrInvalidMT292, body[3:], err)
	}
	return "210", body[3:], nil
}

// mt292FieldBody extracts the body of a single-line field at a
// fixed position: the line must start with the tag and carry a
// non-empty body.
func mt292FieldBody(line, tag string, lineNo int) (string, error) {
	if !strings.HasPrefix(line, tag) {
		return "", fmt.Errorf("%w: line %d: want %s field, got %q", ErrInvalidMT292, lineNo, tag, line)
	}
	body := line[len(tag):]
	if body == "" {
		return "", fmt.Errorf("%w: %s field is empty", ErrInvalidMT292, tag)
	}
	return body, nil
}

// mt292CheckReasonLines validates a :79: narrative: at most 35 lines
// of at most 35 SWIFT `x` characters each (braces refused, the
// block-4 framing rule). Empty lines pass — ExportMT292 renders
// them, so Parse ∘ Export must accept them back. (The export-side
// mt292ReasonLines in mt292.go splits and validates a reason
// string; this one validates already-split lines under the
// inbound error identity.)
func mt292CheckReasonLines(lines []string) error {
	if len(lines) > mt292MaxReasonLines {
		return fmt.Errorf("%w: :79: at most %d lines, got %d", ErrInvalidMT292, mt292MaxReasonLines, len(lines))
	}
	for _, ln := range lines {
		if len(ln) > mt292MaxReasonLine {
			return fmt.Errorf("%w: :79: line exceeds %d characters: %q", ErrInvalidMT292, mt292MaxReasonLine, ln)
		}
		if err := mt103CheckX(ln); err != nil {
			return fmt.Errorf("%w: :79: line: %v", ErrInvalidMT292, err)
		}
	}
	return nil
}

// ParseMT292 parses a SWIFT FIN MT292 message produced by
// ExportMT292 (or an equivalent counterparty message) into its
// business fields. Parsing is strict: the block structure, the
// field order, and every field value are validated, and anything
// outside the four-field core set is refused — a cancellation
// must not lose content quietly. CRLF line endings are accepted;
// a trailing newline after the `-}` trailer is tolerated. The
// :11S: original type must be "210": the ledger only ever
// registers MT210 notifications, so any other type is refused
// fail-closed rather than parsed into a cancellation with no
// registration to withdraw.
func ParseMT292(text string) (MT292Cancellation, error) {
	var zero MT292Cancellation
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	lines := strings.Split(text, "\n")
	if len(lines) < 8 {
		return zero, fmt.Errorf("%w: want at least 8 lines (3 headers + {4: + 3 required fields + -}), got %d", ErrInvalidMT292, len(lines))
	}
	senderLT, err := mt292ParseLT(lines[0], "{1:F01", 10)
	if err != nil {
		return zero, err
	}
	receiverLT, err := mt292ParseLT(lines[1], "{2:I292", 5)
	if err != nil {
		return zero, err
	}
	// Block 3: shape-checked only (see the MT202 package doc).
	mur := lines[2]
	if !strings.HasPrefix(mur, "{3:{108:") || !strings.HasSuffix(mur, "}}") {
		return zero, fmt.Errorf("%w: malformed block 3: %q", ErrInvalidMT292, mur)
	}
	if err := mt292Reference(mur[len("{3:{108:"):len(mur)-2], "{108:}"); err != nil {
		return zero, err
	}
	if lines[3] != "{4:" {
		return zero, fmt.Errorf("%w: want {4: on line 4, got %q", ErrInvalidMT292, lines[3])
	}
	if lines[len(lines)-1] != "-}" {
		return zero, fmt.Errorf("%w: want -} trailer, got %q", ErrInvalidMT292, lines[len(lines)-1])
	}
	var m MT292Cancellation
	m.SenderLT, m.ReceiverLT = senderLT, receiverLT
	body := lines[4 : len(lines)-1]
	// :20: — this message's own reference, the replay key.
	refBody, err := mt292FieldBody(body[0], ":20:", 5)
	if err != nil {
		return zero, err
	}
	if err := mt292Reference(refBody, ":20:"); err != nil {
		return zero, err
	}
	m.MsgID = refBody
	// :21: — the original MT210's :20:, the expectedReceipts key.
	relatedBody, err := mt292FieldBody(body[1], ":21:", 6)
	if err != nil {
		return zero, err
	}
	if err := mt292Reference(relatedBody, ":21:"); err != nil {
		return zero, err
	}
	m.RelatedMsgID = relatedBody
	// :11S: — the original message type and issue date.
	origBody, err := mt292FieldBody(body[2], ":11S:", 7)
	if err != nil {
		return zero, err
	}
	typ, date, err := mt292ParseOriginal(origBody)
	if err != nil {
		return zero, err
	}
	m.OriginalType, m.OriginalDate = typ, date
	// :79: — optional, and once started consumes the rest of
	// block 4 as continuations (the MT900/MT910 :72: precedent).
	rest := body[3:]
	if len(rest) > 0 {
		if !strings.HasPrefix(rest[0], ":79:") {
			return zero, fmt.Errorf("%w: unexpected field after :11S:: %q", ErrInvalidMT292, rest[0])
		}
		reason := []string{rest[0][len(":79:"):]}
		reason = append(reason, rest[1:]...)
		if err := mt292CheckReasonLines(reason); err != nil {
			return zero, err
		}
		m.Reason = strings.Join(reason, "\n")
	}
	return m, nil
}

// parkMT292Request parks a parsed MT292 request that failed
// closed, and returns the repair record ID. The snapshot's Maker
// is deliberately empty (see the package comment). The
// Destination names the original notification the request tried
// to withdraw — the :21: the ledger never registered — so the
// operator can find what is missing.
func (l *Ledger) parkMT292Request(req MT292Cancellation, reasonCode, actor string) (string, error) {
	rec, err := l.EnqueueRepair(PayoutInstruction{
		ID:          req.MsgID,
		Destination: "mt292-inbound:" + req.RelatedMsgID,
		// Maker intentionally empty: see the package comment.
	}, reasonCode, actor)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// ParkMT292Unparsed best-effort parks an MT292 document ParseMT292
// refused, when the document still names a notification: the :21:
// reference it carries. It returns the repair record ID, or ""
// when the document names nothing parkable (garbage bytes, an
// empty shell): there is no notification for the operator to find,
// so there is nothing to park. A parking failure (a full queue)
// is returned so the caller can fail loud rather than silently
// drop the counterparty's message. The reason code is
// invalid_format — the document was present but unreadable as a
// cancellation request.
func (l *Ledger) ParkMT292Unparsed(text, actor string) (string, error) {
	id := ""
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(line, ":21:") {
			id = strings.TrimSpace(line[len(":21:"):])
			break
		}
	}
	if id == "" {
		return "", nil
	}
	rec, err := l.EnqueueRepair(PayoutInstruction{
		ID: id,
		// Maker intentionally empty: see the package comment.
		Destination: "mt292-inbound:" + id,
	}, RepairReasonInvalidFormat, actor)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// ApplyMT292 applies one parsed inbound SWIFT FIN MT292
// cancellation of notification to receive: it marks the expected
// receipt LG-448 registered for the named MT210 notification
// cancelled, and nothing else.
//
// The request's :21: must name a registered notification —
// otherwise the request is refused fail-closed and parked in the
// repair queue (unknown_route). The ledger never invents a
// notification in order to cancel it.
//
// Only the forecast changes: no journal rows, no version bump,
// no money moved. The cancelled notification stops counting as
// the fifth intraday-liquidity source (see liquidity.go); the
// record is kept for audit, with Cancelled, CancelledAt,
// CancelMsgID, and the verbatim CancelReason.
//
// Idempotency has two layers, both no-op successes with
// Duplicate set and no second audit event: re-applying a request
// :20: already applied returns the stored apply result, and a
// different request :20: cancelling an already-cancelled
// notification returns the stored (cancelled) receipt. A request
// :20: reused for a different original is refused with
// ErrMT292MsgIDConflict.
//
// Locking: the replay read and the receipt read run under the
// read lock; the cancellation section holds the write lock (the
// re-checks, the registry writes, and the audit event are one
// atomic section); the repair parking runs outside any held lock
// because EnqueueRepair takes l.mu itself.
func (l *Ledger) ApplyMT292(req MT292Cancellation, actor string) (MT292ApplyResult, error) {
	if strings.TrimSpace(req.MsgID) == "" {
		return MT292ApplyResult{}, fmt.Errorf("%w: missing :20:", ErrInvalidMT292)
	}
	if req.OriginalType != "" && req.OriginalType != "210" {
		return MT292ApplyResult{}, fmt.Errorf("%w: :11S: original message type %q, want 210", ErrMT292UnsupportedOriginal, req.OriginalType)
	}
	if actor == "" {
		actor = "ApplyMT292"
	}

	// Replay read and receipt read: the same request :20:
	// applied before is a no-op success, whatever the ledger's
	// current state.
	l.mu.RLock()
	applied, replayed := l.mt292Applied[req.MsgID]
	_, known := l.expectedReceipts[req.RelatedMsgID]
	l.mu.RUnlock()
	if replayed {
		// One :20: names one request: a different original
		// under it is a conflict.
		if applied.Receipt.MsgID != req.RelatedMsgID {
			return MT292ApplyResult{}, fmt.Errorf("%w: :20: %q", ErrMT292MsgIDConflict, req.MsgID)
		}
		return MT292ApplyResult{Receipt: applied.Receipt, Duplicate: true}, nil
	}
	if !known {
		// The parked fail-closed path, outside any held lock:
		// an unknown original goes to the repair queue. The
		// ledger never invents a notification to cancel.
		repairID, err := l.parkMT292Request(req, RepairReasonUnknownRoute, actor)
		if err != nil {
			return MT292ApplyResult{}, fmt.Errorf("%w: %q (repair parking failed: %v)", ErrMT292UnknownNotification, req.RelatedMsgID, err)
		}
		return MT292ApplyResult{RepairID: repairID}, fmt.Errorf("%w: %q", ErrMT292UnknownNotification, req.RelatedMsgID)
	}

	// Cancellation section: re-check the replay registry and
	// the receipt state under the write lock — a racing request
	// for the same :20:, or a racing cancellation of the same
	// notification, converges here instead of double-cancelling.
	l.mu.Lock()
	defer l.mu.Unlock()
	if applied, replayed := l.mt292Applied[req.MsgID]; replayed {
		if applied.Receipt.MsgID != req.RelatedMsgID {
			return MT292ApplyResult{}, fmt.Errorf("%w: :20: %q", ErrMT292MsgIDConflict, req.MsgID)
		}
		return MT292ApplyResult{Receipt: applied.Receipt, Duplicate: true}, nil
	}
	receipt, known := l.expectedReceipts[req.RelatedMsgID]
	if !known {
		// Unreachable: receipts are never deleted, and the
		// read above saw it — but never invent one.
		return MT292ApplyResult{}, fmt.Errorf("%w: %q", ErrMT292UnknownNotification, req.RelatedMsgID)
	}
	if receipt.Cancelled {
		// Second request on an already-cancelled notification:
		// a no-op success returning the stored (cancelled)
		// receipt. Record this request :20: too, so its own
		// replay converges the same way.
		res := MT292ApplyResult{Receipt: receipt, Duplicate: true}
		if l.mt292Applied == nil {
			l.mt292Applied = make(map[string]MT292ApplyResult)
		}
		l.mt292Applied[req.MsgID] = MT292ApplyResult{Receipt: receipt}
		return res, nil
	}
	receipt.Cancelled = true
	receipt.CancelledAt = time.Now().UTC()
	receipt.CancelMsgID = req.MsgID
	receipt.CancelReason = req.Reason
	l.expectedReceipts[req.RelatedMsgID] = receipt
	res := MT292ApplyResult{Receipt: receipt}
	if l.mt292Applied == nil {
		l.mt292Applied = make(map[string]MT292ApplyResult)
	}
	l.mt292Applied[req.MsgID] = res
	l.emitAudit(AuditEvent{
		Op:       "mt210_notification_cancelled",
		Actor:    actor,
		TraceID:  req.MsgID,
		Accounts: []AccountID{receipt.Account},
		Details: map[string]any{
			"msg_id":       req.MsgID,
			"orgnl_msg_id": req.RelatedMsgID,
			"reason":       req.Reason,
		},
	})
	return res, nil
}
