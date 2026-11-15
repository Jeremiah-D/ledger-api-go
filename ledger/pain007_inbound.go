package ledger

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ISO 20022 pain.007.001.02 CustomerPaymentCancellationRequest
// inbound processing (LG-424): the inbound half of the pain.007
// lifecycle, the mirror of the LG-401 export.
//
// ExportPain007 renders this ledger's own cancellation request —
// the ledger asking the bank to cancel a payment it sent. This
// module consumes the cancellation request a counterparty customer
// sent to this ledger, acting as the customer's bank: it executes
// the cancellation in-ledger by reversing the original transfer.
// The two directions are orthogonal by construction:
//
//   - LG-401 exports: the ledger asks the bank to cancel one of
//     its own transfers (outbound, read-only, no ledger state).
//   - LG-424 imports: the customer asks the ledger to cancel a
//     transfer (inbound, state-changing: one reversing journal
//     entry, the money moves back).
//
// ParsePain007 reuses the export-side wire structs (pain007x*,
// pain007.go:194-248) in reverse: the .001.02 namespace gates the
// version, like the camt.056 inbound .001.08 gate — a document in
// another version fails the unmarshal. The reason code is admitted
// only from the pain007ValidReason vocabulary when stated; an
// unstated reason stays empty, the export convention (the Rsn
// block is omitted, never defaulted), so a document this ledger's
// own export renders parses back.
//
// Association is the EndToEndId, the pain.001 convention: the
// OrgnlEndToEndId names the transfer, and the OrgnlTxRef amount
// must equal the original transfer's amount — an amount that
// differs means the request points at a different payment (the
// FedNow pacs.008 amount-mismatch precedent), so it fails closed
// into the repair queue instead of reversing the wrong transfer.
// The OrgnlTxRef IBANs are carried on the parsed request and the
// audit event; they do not gate the reversal, which is located
// by the transfer ID alone. The OrgnlMsgId, when stated, is
// carried the same way — informational, never invented.
//
// The reversal runs through ReverseEntryWithMetadata
// (reversal.go:189) on the transfer's principal journal entry
// (the entry ID is the transfer ID, the pain.001 EndToEndId
// convention), with the idempotency key derived from the
// pain.007 GrpHdr MsgId ("pain007-apply:<MsgId>"), so a re-sent
// request replays without booking a second reversal. The reversal
// entry carries the request's provenance as metadata
// (pain007_msg_id, pain007_reason_code) when the values fit the
// metadata limits; the audit event is the durable record and
// always carries the full MsgId, so an overlong MsgId loses the
// metadata stamp, never the reversal.
//
// An already-reversed transfer is an idempotent no-op: the stored
// reversal is returned with Duplicate set, no second booking, no
// second audit event. That covers both the MsgId replay (the
// byKey check inside ReverseEntryWithMetadata) and a transfer
// the operator already reversed under another key (the genuine
// full-reversal check below). A partial reversal is not a full
// one: the full reversal is still refused with
// ErrAlreadyReversed, exactly like a direct ReverseEntry call —
// correct the outstanding partials first.
//
// Fail-closed parking (the pacs.004 / camt.055 convention): an
// unknown original transfer, an amount that does not match the
// original, and an illegal reason code all park in the repair
// queue, and the returned result carries the repair record ID.
// Every parked request also emits a `pain007_reversal_rejected`
// audit event, so the trail shows the refusal, not just the
// queue row. A document ParsePain007 refuses parks the same way
// through ParkPain007Unparsed when it still names a request.
// Repair records parked by this module are visibility parking,
// not resubmittable instructions: their Maker is deliberately
// empty, so the CreatePayout gate can never pass them.
//
// No partial cancellation: the OrgnlTxRef amount is the original
// transaction's amount, not a cancellation amount — a full
// reversal is the only semantics this message carries. The
// transfer state machine is untouched beyond the reversal entry
// itself: whether the bank accepts the cancellation is confirmed
// by a pain.002 / pacs.002 receipt, never by this application.

// pain.007 inbound errors.
var (
	// ErrInvalidPain007 is returned by ParsePain007 for malformed
	// XML or a document missing a required field, and by
	// ApplyPain007 for a hand-built request that fails the same
	// gates: the counterparty sent something the ledger cannot
	// even read as a cancellation request. The HTTP layer parks
	// it in the repair queue and maps it to 400.
	ErrInvalidPain007 = errors.New("ledger: invalid pain.007 customer payment cancellation request")
	// ErrPain007UnknownOriginal is returned by ApplyPain007 when
	// the OrgnlEndToEndId names no transfer the ledger ever
	// registered: a cancellation can only address a payment the
	// ledger knows, and the ledger never invents a transfer to
	// reverse. The request parks in the repair queue. It is a
	// 404-class error.
	ErrPain007UnknownOriginal = errors.New("ledger: pain.007 inbound: unknown original transfer")
	// ErrPain007AmountMismatch is returned by ApplyPain007 when
	// the OrgnlTxRef amount differs from the original transfer's
	// amount: the request points at a different payment than the
	// one it names, so it fails closed into the repair queue
	// instead of reversing the wrong transfer (the FedNow
	// pacs.008 amount-mismatch precedent). It is a 422-class
	// error.
	ErrPain007AmountMismatch = errors.New("ledger: pain.007 inbound: original transaction reference amount does not match the original transfer")
)

// Pain007Request is one parsed inbound pain.007.001.02 customer
// payment cancellation request: the request's identity, the
// original transfer it cancels, the stated reason, and the
// original transaction reference the identity check runs on.
type Pain007Request struct {
	// MsgID is the GrpHdr MsgId: the audit trace, the repair
	// record key, and the idempotency-key derivation.
	MsgID string `json:"msg_id"`
	// OrgnlEndToEndID is TxInfAndSts/OrgnlEndToEndId: the
	// transfer to reverse, the pain.001 EndToEndId.
	OrgnlEndToEndID string `json:"orgnl_end_to_end_id"`
	// ReasonCode is CxlStsRsnInf/Rsn/Prtry when stated, one of
	// the admitted pain007ValidReason codes; empty when the
	// document states no reason.
	ReasonCode string `json:"reason_code,omitempty"`
	// AmountCents is OrgnlTxRef/Amt/InstdAmt in minor units;
	// Currency is its Ccy attribute. Both are required: the
	// amount is the identity check against the original
	// transfer.
	AmountCents int64  `json:"amount_cents"`
	Currency    string `json:"currency"`
	// DebtorIBAN is OrgnlTxRef/DbtrAcct/Id/IBAN and CreditorIBAN
	// OrgnlTxRef/CdtrAcct/Id/IBAN, when stated: carried for the
	// audit trail, never the source of the booking.
	DebtorIBAN   string `json:"debtor_iban,omitempty"`
	CreditorIBAN string `json:"creditor_iban,omitempty"`
	// OrgnlMsgID is OrgnlGrpInfAndSts/OrgnlMsgId when stated:
	// the original instruction's MsgId, informational.
	OrgnlMsgID string `json:"orgnl_msg_id,omitempty"`
}

// Pain007ApplyResult is the outcome of ApplyPain007.
type Pain007ApplyResult struct {
	// Reversal is the booked reversing journal entry (or the
	// stored one on the duplicate path).
	Reversal JournalEntry `json:"reversal"`
	// RepairID is set on the parked fail-closed path: the
	// repair-queue record holding the request for operator
	// resolution.
	RepairID string `json:"repair_id,omitempty"`
	// Duplicate reports whether the transfer was already
	// reversed: a no-op success, not an error.
	Duplicate bool `json:"duplicate,omitempty"`
}

// pain007ParseAmount parses an ISO 20022 decimal amount
// ("1234.56") into minor units, the pacs010ParseAmount shape:
// the integer part holds at most 12 digits, the fraction at most
// two digits and is right-padded ("5" means 50 cents), the value
// must be positive. Anything else is a parse refusal — a
// cancellation amount the ledger cannot read exactly is never
// rounded into an identity check.
func pain007ParseAmount(body string) (int64, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return 0, fmt.Errorf("%w: empty amount", ErrInvalidPain007)
	}
	intPart := body
	fracPart := ""
	if i := strings.IndexByte(body, '.'); i >= 0 {
		intPart, fracPart = body[:i], body[i+1:]
		if strings.IndexByte(fracPart, '.') >= 0 {
			return 0, fmt.Errorf("%w: amount %q has two decimal points", ErrInvalidPain007, body)
		}
	}
	if intPart == "" || len(intPart) > 12 || len(fracPart) > 2 {
		return 0, fmt.Errorf("%w: amount %q", ErrInvalidPain007, body)
	}
	for _, c := range intPart + fracPart {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("%w: amount %q is not numeric", ErrInvalidPain007, body)
		}
	}
	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: amount %q", ErrInvalidPain007, body)
	}
	frac := int64(0)
	if fracPart != "" {
		if len(fracPart) == 1 {
			fracPart += "0"
		}
		frac, err = strconv.ParseInt(fracPart, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%w: amount %q", ErrInvalidPain007, body)
		}
	}
	cents := whole*100 + frac
	if cents <= 0 {
		return 0, fmt.Errorf("%w: amount %q is not positive", ErrInvalidPain007, body)
	}
	return cents, nil
}

// validatePain007Request fail-closes on a cancellation request
// ParsePain007 would refuse, so ApplyPain007 stays safe on
// hand-built requests too: the parse gate and the apply gate
// share one check, never a copy.
func validatePain007Request(req Pain007Request) error {
	if strings.TrimSpace(req.MsgID) == "" {
		return fmt.Errorf("%w: missing GrpHdr/MsgId", ErrInvalidPain007)
	}
	if strings.TrimSpace(req.OrgnlEndToEndID) == "" {
		return fmt.Errorf("%w: missing OrgnlEndToEndId", ErrInvalidPain007)
	}
	if req.ReasonCode != "" && !pain007ValidReason(req.ReasonCode) {
		return fmt.Errorf("%w: %q", ErrInvalidPain007Reason, req.ReasonCode)
	}
	if req.AmountCents <= 0 {
		return fmt.Errorf("%w: non-positive original transaction reference amount", ErrInvalidPain007)
	}
	if strings.TrimSpace(req.Currency) == "" {
		return fmt.Errorf("%w: empty original transaction reference amount currency", ErrInvalidPain007)
	}
	if _, err := normalizeCurrency(req.Currency); err != nil {
		return fmt.Errorf("%w: currency %q", ErrInvalidPain007, req.Currency)
	}
	return nil
}

// ParsePain007 parses one inbound pain.007.001.02 customer
// payment cancellation request into a Pain007Request, reusing
// the export-side pain007x* wire structs in reverse. It
// validates the whole document before returning anything: a
// missing MsgId, a missing OrgnlEndToEndId, a stated reason code
// outside the admitted vocabulary, or a missing, unreadable, or
// non-positive OrgnlTxRef amount are all parse refusals —
// ApplyPain007 never sees a half-read request. The OrgnlTxRef
// IBANs and the OrgnlMsgId are carried verbatim when stated and
// never gated: the transfer ID locates the payment, not the
// reference block.
func ParsePain007(data []byte) (Pain007Request, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return Pain007Request{}, fmt.Errorf("%w: empty document", ErrInvalidPain007)
	}
	var doc pain007xDocument
	if err := xml.Unmarshal(data, &doc); err != nil {
		return Pain007Request{}, fmt.Errorf("%w: %v", ErrInvalidPain007, err)
	}
	req := doc.CstmrPmtCxlReq
	msgID := strings.TrimSpace(req.GrpHdr.MsgID)
	if msgID == "" {
		return Pain007Request{}, fmt.Errorf("%w: missing GrpHdr/MsgId", ErrInvalidPain007)
	}
	tx := req.OrgnlPmtInfAndSts.TxInfAndSts
	endToEnd := strings.TrimSpace(tx.OrgnlEndToEndID)
	if endToEnd == "" {
		return Pain007Request{}, fmt.Errorf("%w: missing OrgnlEndToEndId", ErrInvalidPain007)
	}
	reason := ""
	if tx.CxlStsRsnInf != nil {
		reason = strings.TrimSpace(tx.CxlStsRsnInf.Rsn.Prtry)
	}
	if reason != "" && !pain007ValidReason(reason) {
		return Pain007Request{}, fmt.Errorf("%w: %q", ErrInvalidPain007Reason, reason)
	}
	amount, err := pain007ParseAmount(tx.OrgnlTxRef.Amt.InstdAmt.Value)
	if err != nil {
		return Pain007Request{}, err
	}
	currency := strings.TrimSpace(tx.OrgnlTxRef.Amt.InstdAmt.Ccy)
	if currency == "" {
		return Pain007Request{}, fmt.Errorf("%w: empty original transaction reference amount currency", ErrInvalidPain007)
	}
	if _, err := normalizeCurrency(currency); err != nil {
		return Pain007Request{}, fmt.Errorf("%w: currency %q", ErrInvalidPain007, currency)
	}
	orgnlMsgID := ""
	if req.OrgnlGrpInfAndSts != nil {
		orgnlMsgID = strings.TrimSpace(req.OrgnlGrpInfAndSts.OrgnlMsgID)
	}
	return Pain007Request{
		MsgID:           msgID,
		OrgnlEndToEndID: endToEnd,
		ReasonCode:      reason,
		AmountCents:     amount,
		Currency:        currency,
		DebtorIBAN:      strings.TrimSpace(tx.OrgnlTxRef.DbtrAcct.ID.IBAN),
		CreditorIBAN:    strings.TrimSpace(tx.OrgnlTxRef.CdtrAcct.ID.IBAN),
		OrgnlMsgID:      orgnlMsgID,
	}, nil
}

// pain007LenientDocument is the namespace-agnostic wire shape
// ParkPain007Unparsed reads: a document ParsePain007 refused
// for its version may still name the request, and the namespace
// is the only thing the strict parse gates on. Unknown elements
// are skipped, the camt.029 convention.
type pain007LenientDocument struct {
	XMLName        xml.Name             `xml:"Document"`
	CstmrPmtCxlReq pain007LenientCxlReq `xml:"CstmrPmtCxlReq"`
}

type pain007LenientCxlReq struct {
	GrpHdr            pain007LenientGrpHdr `xml:"GrpHdr"`
	OrgnlPmtInfAndSts pain007LenientPmtInf `xml:"OrgnlPmtInfAndSts"`
}

type pain007LenientGrpHdr struct {
	MsgID string `xml:"MsgId"`
}

type pain007LenientPmtInf struct {
	TxInfAndSts pain007LenientTx `xml:"TxInfAndSts"`
}

type pain007LenientTx struct {
	OrgnlEndToEndID string `xml:"OrgnlEndToEndId"`
}

// ParkPain007Unparsed best-effort parks a pain.007 document
// ParsePain007 refused, when the document still names a request:
// the GrpHdr MsgId or the OrgnlEndToEndId it carries. It returns
// the repair record ID, or "" when the document names nothing
// parkable (garbage bytes, an empty shell): there is no request
// for the operator to find, so there is nothing to park. A
// parking failure (a full queue) is returned so the caller can
// fail loud rather than silently drop the customer's message.
// The reason code is invalid_format — the document was present
// but unreadable as a cancellation request.
func (l *Ledger) ParkPain007Unparsed(data []byte, actor string) (string, error) {
	var doc pain007LenientDocument
	if err := xml.Unmarshal(data, &doc); err != nil {
		return "", nil
	}
	id := strings.TrimSpace(doc.CstmrPmtCxlReq.GrpHdr.MsgID)
	if id == "" {
		id = strings.TrimSpace(doc.CstmrPmtCxlReq.OrgnlPmtInfAndSts.TxInfAndSts.OrgnlEndToEndID)
	}
	if id == "" {
		return "", nil
	}
	rec, err := l.EnqueueRepair(PayoutInstruction{
		ID: id,
		// Maker intentionally empty: see the package comment.
		Destination: "pain007-inbound:" + id,
	}, RepairReasonInvalidFormat, actor)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// parkPain007Request parks a parsed pain.007 request that failed
// closed, and returns the repair record ID. The snapshot names
// the request's MsgId — the request's own identity — so the
// operator can find the cancellation it was meant for. The
// snapshot's Maker is deliberately empty (see the package
// comment).
func (l *Ledger) parkPain007Request(req Pain007Request, reasonCode, actor string) (string, error) {
	id := strings.TrimSpace(req.MsgID)
	if id == "" {
		id = strings.TrimSpace(req.OrgnlEndToEndID)
	}
	rec, err := l.EnqueueRepair(PayoutInstruction{
		ID: id,
		// Maker intentionally empty: see the package comment.
		Destination: "pain007-inbound:" + id,
	}, reasonCode, actor)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// pain007ApplyKey derives the reversal's idempotency key from the
// pain.007 GrpHdr MsgId: a re-sent request replays on the same
// key instead of booking a second reversal.
func pain007ApplyKey(msgID string) string {
	return "pain007-apply:" + msgID
}

// pain007ReversalMetadata builds the provenance metadata stamped
// on the reversal entry: the requesting message and its reason.
// A value over the metadata length limit is left out rather than
// truncating a correlation identifier — the audit event carries
// the full MsgId either way.
func pain007ReversalMetadata(req Pain007Request) map[string]string {
	meta := map[string]string{}
	if req.MsgID != "" && utf8.RuneCountInString(req.MsgID) <= maxMetadataValueRunes {
		meta["pain007_msg_id"] = req.MsgID
	}
	if req.ReasonCode != "" {
		meta["pain007_reason_code"] = req.ReasonCode
	}
	return meta
}

// emitPain007Rejected emits the `pain007_reversal_rejected`
// audit event for a request that failed closed and parked in the
// repair queue. Callers must not hold l.mu: the event lands
// under a short write lock of its own.
func (l *Ledger) emitPain007Rejected(req Pain007Request, actor string, err error) {
	l.mu.Lock()
	l.emitAudit(AuditEvent{
		Op:      "pain007_reversal_rejected",
		Actor:   actor,
		TraceID: req.MsgID,
		Details: map[string]any{
			"msg_id":              req.MsgID,
			"orgnl_end_to_end_id": req.OrgnlEndToEndID,
			"reason_code":         req.ReasonCode,
			"reason":              err.Error(),
		},
	})
	l.mu.Unlock()
}

// ApplyPain007 executes one parsed inbound pain.007.001.02
// customer payment cancellation request: it reverses the
// original transfer's principal journal entry through
// ReverseEntryWithMetadata. The transfer must be registered —
// an unknown OrgnlEndToEndId fails closed into the repair queue,
// never invents a transfer — and the OrgnlTxRef amount must
// equal the original transfer's amount, or the request points
// at a different payment and parks the same way. An
// already-reversed transfer is an idempotent no-op returning the
// stored reversal with Duplicate set: no second booking, no
// second audit event. A fresh reversal emits the
// `pain007_reversal_applied` audit event (details msg_id /
// orgnl_end_to_end_id / reason_code / amount_cents / entry_id),
// distinct from the `reverse` event ReverseEntryWithMetadata
// itself emits, so the trail tells an inbound cancellation apart
// from an operator-driven reversal.
//
// The reversal's own risk gates (frozen, overdraft, daily
// limit, velocity, period) are ReverseEntryWithMetadata's,
// unmodified: a refusal there is an operational refusal, not a
// bad document, so it propagates without repair parking.
//
// Locking: the validation runs on the parsed values alone; the
// transfer lookup, the amount check, and the genuine-reversal
// read run under the read lock; ReverseEntryWithMetadata takes
// the write lock itself; the inbound audit event runs under a
// short write lock of its own; the repair parking runs outside
// any held lock because EnqueueRepair takes l.mu itself.
func (l *Ledger) ApplyPain007(req Pain007Request, actor string) (Pain007ApplyResult, error) {
	if actor == "" {
		actor = "ApplyPain007"
	}
	if err := validatePain007Request(req); err != nil {
		repairID, parkErr := l.parkPain007Request(req, RepairReasonInvalidFormat, actor)
		if parkErr != nil {
			return Pain007ApplyResult{}, fmt.Errorf("%w (repair parking failed: %v)", err, parkErr)
		}
		l.emitPain007Rejected(req, actor, err)
		return Pain007ApplyResult{RepairID: repairID}, err
	}

	// The transfer lookup and the amount identity check, under
	// the read lock: both fail closed into the repair queue.
	l.mu.RLock()
	pt, ok := l.painTransfers[req.OrgnlEndToEndID]
	l.mu.RUnlock()
	if !ok {
		err := fmt.Errorf("%w: %q", ErrPain007UnknownOriginal, req.OrgnlEndToEndID)
		repairID, parkErr := l.parkPain007Request(req, RepairReasonUnknownRoute, actor)
		if parkErr != nil {
			return Pain007ApplyResult{}, fmt.Errorf("%w (repair parking failed: %v)", err, parkErr)
		}
		l.emitPain007Rejected(req, actor, err)
		return Pain007ApplyResult{RepairID: repairID}, err
	}
	if req.AmountCents != pt.AmountCents {
		err := fmt.Errorf("%w: request %d vs transfer %q %d", ErrPain007AmountMismatch, req.AmountCents, req.OrgnlEndToEndID, pt.AmountCents)
		repairID, parkErr := l.parkPain007Request(req, RepairReasonInvalidFormat, actor)
		if parkErr != nil {
			return Pain007ApplyResult{}, fmt.Errorf("%w (repair parking failed: %v)", err, parkErr)
		}
		l.emitPain007Rejected(req, actor, err)
		return Pain007ApplyResult{RepairID: repairID}, err
	}

	// An already-reversed transfer is an idempotent no-op: the
	// stored reversal comes back, nothing new is booked, no
	// second audit event fires.
	l.mu.RLock()
	if existing, taken := l.entries[reversalIDFor(req.OrgnlEndToEndID)]; taken && isGenuineReversalOf(existing, req.OrgnlEndToEndID) {
		l.mu.RUnlock()
		return Pain007ApplyResult{Reversal: existing, Duplicate: true}, nil
	}
	l.mu.RUnlock()

	reversal, duplicate, err := l.ReverseEntryWithMetadata(req.OrgnlEndToEndID, pain007ApplyKey(req.MsgID), pain007ReversalMetadata(req))
	if err != nil {
		if errors.Is(err, ErrAlreadyReversed) {
			// A racing application — or a transfer the
			// operator already reversed under another key —
			// converged here: the stored reversal is the
			// no-op outcome, unless only a partial reversal
			// is outstanding, in which case the refusal
			// stands.
			l.mu.RLock()
			existing, taken := l.entries[reversalIDFor(req.OrgnlEndToEndID)]
			l.mu.RUnlock()
			if taken && isGenuineReversalOf(existing, req.OrgnlEndToEndID) {
				return Pain007ApplyResult{Reversal: existing, Duplicate: true}, nil
			}
		}
		return Pain007ApplyResult{}, err
	}
	if duplicate {
		// A replay on the MsgId-derived key: the original
		// reversal comes back, and the original application
		// already emitted the inbound audit event.
		return Pain007ApplyResult{Reversal: reversal, Duplicate: true}, nil
	}
	// The inbound direction marker, after ReverseEntry commits,
	// under a separately acquired write lock: the reversal is
	// already durable when the marker lands.
	l.mu.Lock()
	l.emitAudit(AuditEvent{
		Op:            "pain007_reversal_applied",
		Actor:         actor,
		TraceID:       req.MsgID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		EntryIDs:      []string{reversal.ID},
		Accounts:      []AccountID{reversal.DebitAccount, reversal.CreditAccount},
		Details: map[string]any{
			"msg_id":              req.MsgID,
			"orgnl_end_to_end_id": req.OrgnlEndToEndID,
			"reason_code":         req.ReasonCode,
			"amount_cents":        reversal.AmountCents,
			"entry_id":            reversal.ID,
		},
	})
	l.mu.Unlock()
	return Pain007ApplyResult{Reversal: reversal}, nil
}
