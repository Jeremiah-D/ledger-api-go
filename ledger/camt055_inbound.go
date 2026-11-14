package ledger

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"strings"
)

// ISO 20022 camt.055.001.01 CustomerPaymentCancellationRequest
// inbound processing (LG-419): the customer-side mirror of the
// camt.056 inbound flow (LG-414).
//
// Outbound camt.055 (see ExportCAMT055, LG-389) is this ledger as
// the customer asking its bank to cancel a payment it issued.
// Inbound camt.055 is the opposite direction of the same loop: the
// customer asks this ledger — acting as the customer's bank — to
// cancel a payment instruction, and the ledger opens the same
// PayoutCancellation record the outbound flow opens, which the
// counterparty later resolves with a camt.029 status report (see
// ApplyCAMT029Report). The two directions converge on one record:
// out is "we asked", in is "we were asked".
//
// The parse reuses the outbound wire structs (camt055Document and
// friends, camt055_export.go) in reverse: the .001.01 namespace
// gates the version, like the camt.056 inbound .001.08 gate — a
// document in another version fails the unmarshal. Only the fields
// the apply reads are interpreted; Case and CtrlData are not
// modelled, the export convention.
//
// Association, the LG-417 camt.026 dual-reference shape: the
// request names its instruction by OrgnlUETR and/or OrgnlInstrId.
// The UETR walks the gpi track to the payout; the OrgnlInstrId
// names the payout ID directly (the export sets it to the payout
// ID). At least one must be present; when both are stated they must
// agree on one payout. OrgnlGrpInf/OrgnlMsgId, when stated, must
// name the same payout ID — the export sets it to the payout ID,
// so a request naming another message is about someone else's
// payment.
//
// The apply reuses RequestPayoutCancellation (camt056.go): the
// record opens with status "requested", MsgId "<uetr>/cxl/<seq>",
// and the hash-chained `cancellation_requested` audit event. The
// fail-closed set is inherited whole: unknown UETR, settled (ACCP)
// or terminal (RJCT) gpi track, already-cancelled payout, unissued
// (draft/approved) or failed payout, and an empty reason code.
// Requesting while a cancellation is already open is an idempotent
// no-op returning the open record; a confirmed cancellation
// refuses re-request; a rejected one admits a fresh attempt with a
// new sequence number. RequestedBy names the customer — the
// Assgnr of the inbound document, not the operator who applied
// it — so the record says who asked.
//
// The apply opens a cancellation record and nothing else: no
// payout status move, no journal rows, no money moved. The funds
// move only when the counterparty's camt.029 resolves the record
// (see ApplyCAMT029Report) — "only opens the cancellation, never
// settles the books".
//
// Idempotency is the CxlId (the request's own ID): re-applying the
// same CxlId returns the stored record with Duplicate set, no
// second audit event, no new sequence number (the camt.026
// report-registry convention). A different CxlId while a
// cancellation is open converges on the open record through the
// RequestPayoutCancellation no-op.
//
// Repair queue, the pacs.007/pacs.004 convention: a parsed request
// that fails closed parks in the repair queue and the error carries
// the repair record ID — the operator can see the customer's
// request instead of it vanishing into a bare refusal. Unknown
// UETR (or unknown OrgnlInstrId) parks as unknown_route, an empty
// reason code as missing_field, and the settled/terminal/
// already-cancelled/unissued/disagreeing-reference refusals as
// invalid_format. A document ParseCAMT055 refuses parks too, when
// it still names an instruction (see ParkCAMT055Unparsed): a
// request the ledger cannot even read is operator visibility, not
// a silent drop. The snapshot's Maker is deliberately empty (the
// pacs.007 convention): the record is operator visibility, never a
// resubmittable instruction.

// camt.055 inbound errors.
var (
	// ErrInvalidCAMT055 is returned by ParseCAMT055 when a
	// document is not a parseable camt.055.001.01 customer
	// payment cancellation request: empty input, malformed XML,
	// a wrong schema version, a missing CxlId, or no OrgnlUETR
	// and no OrgnlInstrId. Fail-loud, the ParseCAMT056
	// convention: a half-readable customer file must not
	// partially apply. It is also the apply error when
	// OrgnlGrpInf names a payout other than the resolved one.
	ErrInvalidCAMT055 = errors.New("ledger: invalid camt.055 customer payment cancellation request")
)

// CAMT055Request is the parsed customer camt.055.001.01
// cancellation request: the request's own CxlId and Assgnmt Id,
// the assigning customer, the UETR and message it recalls, and
// the customer's reason code.
type CAMT055Request struct {
	CxlID        string `json:"cxl_id"`
	MsgID        string `json:"msg_id"`
	Assgnr       string `json:"assignor,omitempty"`
	UETR         string `json:"uetr,omitempty"`
	OrgnlInstrID string `json:"orgnl_instr_id,omitempty"`
	OrgnlMsgID   string `json:"orgnl_msg_id,omitempty"`
	OrgnlMsgNmID string `json:"orgnl_msg_nm_id,omitempty"`
	ReasonCode   string `json:"reason_code"`
}

// CAMT055ApplyResult is the outcome of one applied inbound
// camt.055 request: the cancellation record it opened (or the
// open record it converged on), the repair-queue record ID when
// the request failed closed and was parked, and whether the
// application was an idempotent replay of a CxlId already seen.
type CAMT055ApplyResult struct {
	Cancellation PayoutCancellation `json:"cancellation"`
	RepairID     string             `json:"repair_id,omitempty"`
	Duplicate    bool               `json:"duplicate,omitempty"`
}

// ParseCAMT055 parses a customer-sent ISO 20022 camt.055.001.01
// CustomerPaymentCancellationRequest. It validates the document
// shape — the .001.01 namespace, well-formed XML, a non-empty
// CxlId, and at least one of OrgnlUETR / OrgnlInstrId — and
// refuses anything else fail-loud: a half-readable customer file
// must not partially apply.
//
// The reason code is deliberately NOT validated here, unlike
// ParseCAMT056: an empty reason is an apply-layer failure
// (ErrCancellationReasonRequired) that parks in the repair queue
// (missing_field), so the operator sees a customer request that
// forgot to say why instead of a bare parse refusal.
func ParseCAMT055(data []byte) (*CAMT055Request, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("%w: empty document", ErrInvalidCAMT055)
	}
	var doc camt055Document
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidCAMT055, err)
	}
	tx := doc.Request.Undrlyg.TxInf
	cxlID := strings.TrimSpace(tx.CxlID)
	if cxlID == "" {
		return nil, fmt.Errorf("%w: missing CxlId", ErrInvalidCAMT055)
	}
	uetr := strings.TrimSpace(tx.OrgnlUETR)
	instrID := strings.TrimSpace(tx.OrgnlInstrID)
	if uetr == "" && instrID == "" {
		return nil, fmt.Errorf("%w: no OrgnlUETR or OrgnlInstrId", ErrInvalidCAMT055)
	}
	reason := ""
	if tx.CxlRsnInf.Rsn != nil {
		reason = strings.TrimSpace(tx.CxlRsnInf.Rsn.Prtry)
	}
	return &CAMT055Request{
		CxlID:        cxlID,
		MsgID:        strings.TrimSpace(doc.Request.Assgnmt.ID),
		Assgnr:       strings.TrimSpace(doc.Request.Assgnmt.Assgnr.Nm),
		UETR:         uetr,
		OrgnlInstrID: instrID,
		OrgnlMsgID:   strings.TrimSpace(tx.OrgnlGrpInf.OrgnlMsgID),
		OrgnlMsgNmID: strings.TrimSpace(tx.OrgnlGrpInf.OrgnlMsgNmID),
		ReasonCode:   reason,
	}, nil
}

// camt055LenientDocument is the namespace-agnostic wire shape
// ParkCAMT055Unparsed reads: a document ParseCAMT055 refused for
// its version may still name the instruction, and the namespace
// is the only thing the strict parse gates on. Unknown elements
// are skipped, the camt.029 convention.
type camt055LenientDocument struct {
	XMLName xml.Name   `xml:"Document"`
	Request camt055Req `xml:"CstmrPmtCxlReq"`
}

// ParkCAMT055Unparsed best-effort parks a camt.055 document
// ParseCAMT055 refused, when the document still names an
// instruction: the CxlId, UETR, OrgnlInstrId, or OrgnlMsgId it
// carries. It returns the repair record ID, or "" when the
// document names nothing parkable (garbage bytes, an empty
// shell): there is no instruction for the operator to find, so
// there is nothing to park. A parking failure (a full queue) is
// returned so the caller can fail loud rather than silently drop
// the customer's message. The reason code is invalid_format —
// the document was present but unreadable as a request.
func (l *Ledger) ParkCAMT055Unparsed(data []byte, actor string) (string, error) {
	var doc camt055LenientDocument
	if err := xml.Unmarshal(data, &doc); err != nil {
		return "", nil
	}
	tx := doc.Request.Undrlyg.TxInf
	cxlID := strings.TrimSpace(tx.CxlID)
	uetr := strings.TrimSpace(tx.OrgnlUETR)
	instrID := strings.TrimSpace(tx.OrgnlInstrID)
	msgID := strings.TrimSpace(tx.OrgnlGrpInf.OrgnlMsgID)
	if cxlID == "" && uetr == "" && instrID == "" && msgID == "" {
		return "", nil
	}
	id := msgID
	if id == "" {
		id = instrID
	}
	if id == "" {
		id = cxlID
	}
	if id == "" {
		id = uetr
	}
	rec, err := l.EnqueueRepair(PayoutInstruction{
		ID: id,
		// Maker intentionally empty: see the package comment.
		Destination: "camt055-parse-failure:" + cxlID,
	}, RepairReasonInvalidFormat, actor)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// parkCAMT055Request parks a parsed camt.055 request that failed
// closed, and returns the repair record ID. The snapshot names
// the payout the request resolved to when one was identified,
// else the payout ID the customer claimed (OrgnlMsgId, then
// OrgnlInstrId), else the request's own CxlId — so the operator
// can find the payment the request was meant for. A parking
// failure (a full queue) is returned so the caller can fail loud
// rather than silently drop the customer's message. The snapshot's
// Maker is deliberately empty (see the package comment).
func (l *Ledger) parkCAMT055Request(req *CAMT055Request, payoutID, reasonCode, actor string) (string, error) {
	id := payoutID
	if id == "" {
		id = req.OrgnlMsgID
	}
	if id == "" {
		id = req.OrgnlInstrID
	}
	if id == "" {
		id = req.CxlID
	}
	rec, err := l.EnqueueRepair(PayoutInstruction{
		ID:          id,
		Destination: "camt055-request:" + req.CxlID,
	}, reasonCode, actor)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// camt055RepairReason maps a fail-closed apply error to the
// repair reason code its parked record carries.
func camt055RepairReason(err error) string {
	switch {
	case errors.Is(err, ErrCancellationUnknownUETR):
		return RepairReasonUnknownRoute
	case errors.Is(err, ErrCancellationReasonRequired):
		return RepairReasonMissingField
	default:
		return RepairReasonInvalidFormat
	}
}

// ApplyCAMT055 applies a parsed customer camt.055.001.01
// cancellation request: the inbound half of the camt.055 flow,
// the customer-side mirror of the operator-driven
// RequestPayoutCancellation and the inbound camt.056 flow. The
// customer asks to recall an issued-but-unsettled payout; the
// ledger opens a cancellation record for it, and the
// counterparty's later camt.029 report resolves it through the
// existing ApplyCAMT029Report path.
//
// Fail-closed, the RequestPayoutCancellation gates, inherited
// whole: unknown UETR, settled (ACCP) or terminal (RJCT) gpi
// track, already-cancelled payout, unissued (draft/approved) or
// failed payout, and an empty reason code are all refused — and
// parked in the repair queue, the returned result carrying the
// repair record ID. A UETR and an OrgnlInstrId that disagree on
// the payout, and an OrgnlGrpInf naming another payout, are
// refused the same way. Only the cancellation record opens: no
// payout status move, no journal rows, no money moved.
//
// Idempotency is the CxlId: re-applying a CxlId already seen
// returns the stored record with Duplicate set, no second audit
// event, no new sequence number. A different CxlId while a
// cancellation is already open returns the open record through
// the RequestPayoutCancellation no-op (the record is stored
// under the new CxlId too, so its replay converges as well). A
// confirmed cancellation refuses re-request; a rejected one
// admits a fresh attempt with a new sequence number.
//
// RequestedBy names the customer — the Assgnr of the inbound
// document, not the operator who applied it — so the record says
// who asked. The audit event is the inbound op's
// `cancellation_requested`, the RequestPayoutCancellation
// convention. The record is in-memory only; the audit event is
// the durable record.
//
// Locking: the CxlId replay read and the registry write run
// under the write lock; the association reads use the public
// lock-taking getters; RequestPayoutCancellation takes the lock
// itself; the repair parking runs outside any held lock because
// EnqueueRepair takes l.mu itself.
func (l *Ledger) ApplyCAMT055(req *CAMT055Request, actor string) (CAMT055ApplyResult, error) {
	if req == nil {
		return CAMT055ApplyResult{}, fmt.Errorf("%w: nil request", ErrInvalidCAMT055)
	}
	if strings.TrimSpace(req.CxlID) == "" {
		return CAMT055ApplyResult{}, fmt.Errorf("%w: missing CxlId", ErrInvalidCAMT055)
	}
	if actor == "" {
		actor = "ApplyCAMT055"
	}

	// CxlId replay: the request was already applied.
	l.mu.Lock()
	if rec, ok := l.camt055Applied[req.CxlID]; ok {
		l.mu.Unlock()
		return CAMT055ApplyResult{Cancellation: rec, Duplicate: true}, nil
	}
	l.mu.Unlock()

	// The reason code is enforced before association, the
	// RequestPayoutCancellation order: a request that forgot to
	// say why parks as missing_field.
	if strings.TrimSpace(req.ReasonCode) == "" {
		repairID, err := l.parkCAMT055Request(req, "", RepairReasonMissingField, actor)
		if err != nil {
			return CAMT055ApplyResult{}, fmt.Errorf("%w (repair parking failed: %v)", ErrCancellationReasonRequired, err)
		}
		return CAMT055ApplyResult{RepairID: repairID}, ErrCancellationReasonRequired
	}

	// Resolve the reference: UETR first, then OrgnlInstrId. When
	// both are stated they must agree on one payout.
	uetr := strings.TrimSpace(req.UETR)
	payoutID := ""
	if uetr != "" {
		t, ok := l.GetGPITrack(uetr)
		if !ok {
			repairID, err := l.parkCAMT055Request(req, "", RepairReasonUnknownRoute, actor)
			if err != nil {
				return CAMT055ApplyResult{}, fmt.Errorf("%w: %q (repair parking failed: %v)", ErrCancellationUnknownUETR, uetr, err)
			}
			return CAMT055ApplyResult{RepairID: repairID}, fmt.Errorf("%w: %q", ErrCancellationUnknownUETR, uetr)
		}
		payoutID = t.PayoutID
	}
	if instrID := strings.TrimSpace(req.OrgnlInstrID); instrID != "" {
		p, ok := l.GetPayout(instrID)
		if !ok {
			repairID, err := l.parkCAMT055Request(req, "", RepairReasonUnknownRoute, actor)
			if err != nil {
				return CAMT055ApplyResult{}, fmt.Errorf("%w: OrgnlInstrId %q (repair parking failed: %v)", ErrCancellationUnknownUETR, instrID, err)
			}
			return CAMT055ApplyResult{RepairID: repairID}, fmt.Errorf("%w: OrgnlInstrId %q", ErrCancellationUnknownUETR, instrID)
		}
		if payoutID != "" && p.ID != payoutID {
			repairID, err := l.parkCAMT055Request(req, payoutID, RepairReasonInvalidFormat, actor)
			if err != nil {
				return CAMT055ApplyResult{}, fmt.Errorf("%w: OrgnlUETR %q and OrgnlInstrId %q disagree (repair parking failed: %v)", ErrInvalidCAMT055, uetr, instrID, err)
			}
			return CAMT055ApplyResult{RepairID: repairID}, fmt.Errorf("%w: OrgnlUETR %q and OrgnlInstrId %q disagree", ErrInvalidCAMT055, uetr, instrID)
		}
		if payoutID == "" {
			// UETR-less path: the payout must have a track to
			// be cancellable — a payout with no UETR never left
			// the ledger, so there is nothing in flight to
			// recall.
			if p.UETR == "" {
				repairID, err := l.parkCAMT055Request(req, p.ID, RepairReasonInvalidFormat, actor)
				if err != nil {
					return CAMT055ApplyResult{}, fmt.Errorf("%w: payout %q (repair parking failed: %v)", ErrCancellationNotIssued, p.ID, err)
				}
				return CAMT055ApplyResult{RepairID: repairID}, fmt.Errorf("%w: payout %q", ErrCancellationNotIssued, p.ID)
			}
			uetr = p.UETR
			payoutID = p.ID
		}
	}
	// OrgnlGrpInf cross-check: when stated it must name the
	// resolved payout — the export sets it to the payout ID.
	if msgID := strings.TrimSpace(req.OrgnlMsgID); msgID != "" && msgID != payoutID {
		repairID, err := l.parkCAMT055Request(req, payoutID, RepairReasonInvalidFormat, actor)
		if err != nil {
			return CAMT055ApplyResult{}, fmt.Errorf("%w: OrgnlMsgId %q is not the payout ID of UETR %q (repair parking failed: %v)", ErrInvalidCAMT055, msgID, uetr, err)
		}
		return CAMT055ApplyResult{RepairID: repairID}, fmt.Errorf("%w: OrgnlMsgId %q is not the payout ID of UETR %q", ErrInvalidCAMT055, msgID, uetr)
	}

	// The customer asks: RequestedBy names the Assgnr, falling
	// back to the applying operator when the document names none.
	requestedBy := strings.TrimSpace(req.Assgnr)
	if requestedBy == "" {
		requestedBy = actor
	}
	c, err := l.RequestPayoutCancellation(uetr, req.ReasonCode, requestedBy)
	if err != nil {
		repairID, parkErr := l.parkCAMT055Request(req, payoutID, camt055RepairReason(err), actor)
		if parkErr != nil {
			return CAMT055ApplyResult{}, fmt.Errorf("%w (repair parking failed: %v)", err, parkErr)
		}
		return CAMT055ApplyResult{RepairID: repairID}, err
	}

	// Record the CxlId under the opened (or converged-on) record,
	// so a replay returns it instead of opening a new attempt.
	l.mu.Lock()
	if l.camt055Applied == nil {
		l.camt055Applied = make(map[string]PayoutCancellation)
	}
	l.camt055Applied[req.CxlID] = copyCancellation(&c)
	l.mu.Unlock()
	return CAMT055ApplyResult{Cancellation: c}, nil
}

// GetCAMT055Applied returns the cancellation record first opened
// under cxlID, or false when no request was ever applied under it.
// The returned record is a copy — callers cannot mutate ledger
// state through it.
func (l *Ledger) GetCAMT055Applied(cxlID string) (PayoutCancellation, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	c, ok := l.camt055Applied[cxlID]
	if !ok {
		return PayoutCancellation{}, false
	}
	return copyCancellation(&c), true
}
