package ledger

import (
	"encoding/xml"
	"errors"
	"strings"
	"testing"
)

// camt055InboundDoc renders a minimal camt.055 inbound document for
// the given schema version: the customer's cancellation request
// cxlID, from assignor, recalling uetr (payoutID) for reason. An
// empty uetr or payoutID omits that element, so callers can build
// the UETR-less (OrgnlInstrId-only) and reference-less shapes.
func camt055InboundDoc(t *testing.T, version, cxlID, assignor, uetr, payoutID, reason string) []byte {
	t.Helper()
	var sb strings.Builder
	sb.WriteString(xml.Header)
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.055.` + version + `">`)
	sb.WriteString(`<CstmrPmtCxlReq><Assgnmt><Id>` + cxlID + `</Id>`)
	sb.WriteString(`<Assgnr><Nm>` + assignor + `</Nm></Assgnr>`)
	sb.WriteString(`<Assgne><Nm>our-bank</Nm></Assgne>`)
	sb.WriteString(`<CreDtTm>2026-11-14T10:00:00Z</CreDtTm></Assgnmt>`)
	sb.WriteString(`<Undrlyg><TxInf><CxlId>` + cxlID + `</CxlId>`)
	if uetr != "" {
		sb.WriteString(`<OrgnlUETR>` + uetr + `</OrgnlUETR>`)
	}
	sb.WriteString(`<OrgnlGrpInf><OrgnlMsgId>` + payoutID + `</OrgnlMsgId>`)
	sb.WriteString(`<OrgnlMsgNmId>pacs.008.001.02</OrgnlMsgNmId></OrgnlGrpInf>`)
	if payoutID != "" {
		sb.WriteString(`<OrgnlInstrId>` + payoutID + `</OrgnlInstrId>`)
	}
	sb.WriteString(`<CxlRsnInf><Rsn><Prtry>` + reason + `</Prtry></Rsn></CxlRsnInf>`)
	sb.WriteString(`</TxInf></Undrlyg></CstmrPmtCxlReq></Document>`)
	return []byte(sb.String())
}

// applyInboundCAMT055 parses and applies one inbound camt.055 for
// the submitted payout's UETR, the inbound-apply test convention.
func applyInboundCAMT055(t *testing.T, l *Ledger, cxlID, assignor, uetr, payoutID, reason, actor string) CAMT055ApplyResult {
	t.Helper()
	req, err := ParseCAMT055(camt055InboundDoc(t, "001.01", cxlID, assignor, uetr, payoutID, reason))
	if err != nil {
		t.Fatalf("ParseCAMT055: %v", err)
	}
	res, err := l.ApplyCAMT055(req, actor)
	if err != nil {
		t.Fatalf("ApplyCAMT055: %v", err)
	}
	return res
}

func TestParseCAMT055(t *testing.T) {
	req, err := ParseCAMT055(camt055InboundDoc(t, "001.01", "cxl-in-1", "ACME Corp", "uetr-x", "po-x", "DUPL"))
	if err != nil {
		t.Fatalf("ParseCAMT055: %v", err)
	}
	if req.CxlID != "cxl-in-1" {
		t.Errorf("cxl_id = %q, want cxl-in-1", req.CxlID)
	}
	if req.MsgID != "cxl-in-1" {
		t.Errorf("msg_id = %q, want cxl-in-1", req.MsgID)
	}
	if req.Assgnr != "ACME Corp" {
		t.Errorf("assignor = %q, want ACME Corp", req.Assgnr)
	}
	if req.UETR != "uetr-x" {
		t.Errorf("uetr = %q, want uetr-x", req.UETR)
	}
	if req.OrgnlInstrID != "po-x" {
		t.Errorf("orgnl_instr_id = %q, want po-x", req.OrgnlInstrID)
	}
	if req.OrgnlMsgID != "po-x" {
		t.Errorf("orgnl_msg_id = %q, want po-x", req.OrgnlMsgID)
	}
	if req.OrgnlMsgNmID != "pacs.008.001.02" {
		t.Errorf("orgnl_msg_nm_id = %q, want pacs.008.001.02", req.OrgnlMsgNmID)
	}
	if req.ReasonCode != "DUPL" {
		t.Errorf("reason_code = %q, want DUPL", req.ReasonCode)
	}
}

func TestParseCAMT055_Invalid(t *testing.T) {
	for name, doc := range map[string][]byte{
		"empty":           {},
		"malformed":       []byte("<Document><CstmrPmtCxlReq>"),
		"wrong version":   camt055InboundDoc(t, "001.02", "cxl-in-1", "ACME Corp", "uetr-x", "po-x", "DUPL"),
		"no request":      []byte(xml.Header + `<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.055.001.01"></Document>`),
		"missing cxl id":  []byte(xml.Header + `<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.055.001.01"><CstmrPmtCxlReq><Assgnmt><Id>x</Id></Assgnmt><Undrlyg><TxInf><OrgnlUETR>uetr-x</OrgnlUETR><OrgnlGrpInf><OrgnlMsgId>po-x</OrgnlMsgId></OrgnlGrpInf><CxlRsnInf><Rsn><Prtry>DUPL</Prtry></Rsn></CxlRsnInf></TxInf></Undrlyg></CstmrPmtCxlReq></Document>`),
		"no reference":    camt055InboundDoc(t, "001.01", "cxl-in-1", "ACME Corp", "", "", "DUPL"),
		"empty uetr only": camt055InboundDoc(t, "001.01", "cxl-in-1", "ACME Corp", "", "po-x", "DUPL"),
	} {
		// "empty uetr only" is valid: the OrgnlInstrId carries the
		// reference. Every other case must fail.
		_, err := ParseCAMT055(doc)
		if name == "empty uetr only" {
			if err != nil {
				t.Errorf("%s: err = %v, want nil (OrgnlInstrId-only is valid)", name, err)
			}
			continue
		}
		if !errors.Is(err, ErrInvalidCAMT055) {
			t.Errorf("%s: err = %v, want ErrInvalidCAMT055", name, err)
		}
	}
}

func TestApplyCAMT055_HappyPath(t *testing.T) {
	l := seedPayoutLedger(t)
	p := submittedPayout(t, l, "po-c55-in-1")

	res := applyInboundCAMT055(t, l, "cxl-55-1", "ACME Corp", p.UETR, p.ID, "DUPL", "ops")
	c := res.Cancellation
	if res.Duplicate {
		t.Errorf("duplicate = true, want false on first apply")
	}
	if res.RepairID != "" {
		t.Errorf("repair_id = %q, want empty on success", res.RepairID)
	}
	if c.Status != CancellationRequested {
		t.Errorf("status = %q, want requested", c.Status)
	}
	if c.Seq != 1 {
		t.Errorf("seq = %d, want 1", c.Seq)
	}
	if c.MsgID != cancellationMsgID(p.UETR, 1) {
		t.Errorf("msg_id = %q, want %q", c.MsgID, cancellationMsgID(p.UETR, 1))
	}
	if c.ReasonCode != "DUPL" {
		t.Errorf("reason_code = %q, want DUPL", c.ReasonCode)
	}
	if c.PayoutID != p.ID {
		t.Errorf("payout_id = %q, want %q", c.PayoutID, p.ID)
	}
	// RequestedBy names the customer, not the operator who applied
	// the request.
	if c.RequestedBy != "ACME Corp" {
		t.Errorf("requested_by = %q, want ACME Corp (the customer)", c.RequestedBy)
	}
	if c.RequestedAt.IsZero() {
		t.Errorf("requested_at is zero, want the receipt time")
	}
	// The payout itself is untouched: only the cancellation
	// opened, nothing was settled or cancelled.
	if got, _ := l.GetPayout(p.ID); got.Status != PayoutSubmitted {
		t.Errorf("payout status = %q, want submitted (no direct settlement)", got.Status)
	}
	// The open cancellation renders through the outbound export:
	// one record, two shapes.
	if _, err := l.ExportCAMT055(p.UETR); err != nil {
		t.Errorf("ExportCAMT055 after inbound apply: %v", err)
	}
}

// By OrgnlInstrId alone the request still lands (the UETR-less
// path), and two references that disagree are refused fail-closed
// and parked.
func TestApplyCAMT055_ByInstrIDAndMismatch(t *testing.T) {
	l := seedPayoutLedger(t)
	p := submittedPayout(t, l, "po-c55-in-2")
	other := submittedPayout(t, l, "po-c55-in-2b")

	req, err := ParseCAMT055(camt055InboundDoc(t, "001.01", "cxl-55-2", "ACME Corp", "", p.ID, "DUPL"))
	if err != nil {
		t.Fatalf("ParseCAMT055: %v", err)
	}
	res, err := l.ApplyCAMT055(req, "ops")
	if err != nil {
		t.Fatalf("ApplyCAMT055 by InstrId: %v", err)
	}
	if res.Cancellation.UETR != p.UETR || res.Cancellation.PayoutID != p.ID {
		t.Errorf("by-InstrId cancellation = %+v, want uetr %q payout %q", res.Cancellation, p.UETR, p.ID)
	}

	// Disagreeing references: the UETR names p, the InstrId names
	// another payout.
	req, err = ParseCAMT055(camt055InboundDoc(t, "001.01", "cxl-55-2b", "ACME Corp", p.UETR, other.ID, "DUPL"))
	if err != nil {
		t.Fatalf("ParseCAMT055: %v", err)
	}
	res, err = l.ApplyCAMT055(req, "ops")
	if !errors.Is(err, ErrInvalidCAMT055) {
		t.Errorf("disagreeing references: err = %v, want ErrInvalidCAMT055", err)
	}
	if res.RepairID == "" {
		t.Errorf("disagreeing references: repair_id empty, want a parked record")
	}
	if _, ok := l.GetRepair(res.RepairID); !ok {
		t.Errorf("repair %q not found in the queue", res.RepairID)
	}
}

// CxlId replay returns the stored record with Duplicate set and
// opens nothing new; a different CxlId while a cancellation is
// open converges on the open record.
func TestApplyCAMT055_CxlIDReplayAndOpenNoop(t *testing.T) {
	l := seedPayoutLedger(t)
	p := submittedPayout(t, l, "po-c55-in-3")

	first := applyInboundCAMT055(t, l, "cxl-55-3", "ACME Corp", p.UETR, p.ID, "DUPL", "ops")
	seqBefore := l.cancellationSeqs[p.UETR]

	req, err := ParseCAMT055(camt055InboundDoc(t, "001.01", "cxl-55-3", "ACME Corp", p.UETR, p.ID, "DUPL"))
	if err != nil {
		t.Fatalf("ParseCAMT055: %v", err)
	}
	replay, err := l.ApplyCAMT055(req, "ops")
	if err != nil {
		t.Fatalf("replay ApplyCAMT055: %v", err)
	}
	if !replay.Duplicate {
		t.Errorf("duplicate = false, want true on CxlId replay")
	}
	if replay.Cancellation.MsgID != first.Cancellation.MsgID || replay.Cancellation.Seq != 1 {
		t.Errorf("replay = %+v, want the stored record", replay.Cancellation)
	}
	if l.cancellationSeqs[p.UETR] != seqBefore {
		t.Errorf("replay opened a new attempt: seq %d, want %d", l.cancellationSeqs[p.UETR], seqBefore)
	}

	// A crossing request under a new CxlId: the open no-op.
	second := applyInboundCAMT055(t, l, "cxl-55-3b", "OTHER Corp", p.UETR, p.ID, "CUST", "ops")
	if second.Cancellation.MsgID != first.Cancellation.MsgID || second.Cancellation.Seq != 1 {
		t.Errorf("crossing request = %+v, want the open record", second.Cancellation)
	}
	if second.Cancellation.ReasonCode != "DUPL" {
		t.Errorf("crossing request reason = %q, want DUPL (the open record's reason)", second.Cancellation.ReasonCode)
	}
	// Its replay converges too.
	if got, ok := l.GetCAMT055Applied("cxl-55-3b"); !ok || got.MsgID != first.Cancellation.MsgID {
		t.Errorf("GetCAMT055Applied(cxl-55-3b) = %+v, %v, want the open record", got, ok)
	}
}

// A rejected cancellation admits a fresh attempt with a new
// sequence number; a confirmed one refuses re-request.
func TestApplyCAMT055_ResolvedLifecycle(t *testing.T) {
	l := seedPayoutLedger(t)
	p := submittedPayout(t, l, "po-c55-in-4")

	applyInboundCAMT055(t, l, "cxl-55-4", "ACME Corp", p.UETR, p.ID, "DUPL", "ops")
	if _, err := l.ResolvePayoutCancellation(p.UETR, CancellationRejected, "CUST", "ops"); err != nil {
		t.Fatalf("resolve rejected: %v", err)
	}
	second := applyInboundCAMT055(t, l, "cxl-55-4b", "ACME Corp", p.UETR, p.ID, "DUPL", "ops")
	if second.Cancellation.Seq != 2 {
		t.Errorf("seq after rejection = %d, want 2", second.Cancellation.Seq)
	}
	if second.Cancellation.MsgID != cancellationMsgID(p.UETR, 2) {
		t.Errorf("msg_id after rejection = %q, want %q", second.Cancellation.MsgID, cancellationMsgID(p.UETR, 2))
	}
	if _, err := l.ResolvePayoutCancellation(p.UETR, CancellationConfirmed, "", "ops"); err != nil {
		t.Fatalf("resolve confirmed: %v", err)
	}
	req, err := ParseCAMT055(camt055InboundDoc(t, "001.01", "cxl-55-4c", "ACME Corp", p.UETR, p.ID, "DUPL"))
	if err != nil {
		t.Fatalf("ParseCAMT055: %v", err)
	}
	res, err := l.ApplyCAMT055(req, "ops")
	if !errors.Is(err, ErrCancellationAlreadyCancelled) {
		t.Errorf("after confirm: err = %v, want ErrCancellationAlreadyCancelled", err)
	}
	if res.RepairID == "" {
		t.Errorf("after confirm: repair_id empty, want a parked record")
	}
}

// Fail-closed, parked: unknown UETR, settled track, unissued
// payout, empty reason, and a foreign OrgnlMsgId all refuse with
// the ledger error and a repair-queue record the operator can see.
func TestApplyCAMT055_FailClosedParksRepair(t *testing.T) {
	l := seedPayoutLedger(t)
	p := submittedPayout(t, l, "po-c55-in-5")
	draft := draftPayout(t, l, "po-c55-in-5d", 250_000)
	settled := submittedPayout(t, l, "po-c55-in-5s")
	if _, err := l.AdvanceGPI(settled.UETR, GPIStatusACCP, "", "night-ops"); err != nil {
		t.Fatalf("AdvanceGPI ACCP: %v", err)
	}

	cases := []struct {
		name    string
		doc     []byte
		wantErr error
	}{
		{"unknown uetr", camt055InboundDoc(t, "001.01", "cxl-55-5a", "ACME Corp", "no-such-uetr", "po-x", "DUPL"), ErrCancellationUnknownUETR},
		{"unknown instr id", camt055InboundDoc(t, "001.01", "cxl-55-5b", "ACME Corp", "", "no-such-payout", "DUPL"), ErrCancellationUnknownUETR},
		{"settled", camt055InboundDoc(t, "001.01", "cxl-55-5c", "ACME Corp", settled.UETR, settled.ID, "DUPL"), ErrCancellationSettled},
		{"draft", camt055InboundDoc(t, "001.01", "cxl-55-5d", "ACME Corp", draft.UETR, draft.ID, "DUPL"), ErrCancellationNotIssued},
		{"empty reason", camt055InboundDoc(t, "001.01", "cxl-55-5e", "ACME Corp", p.UETR, p.ID, ""), ErrCancellationReasonRequired},
		// Foreign OrgnlMsgId: the OrgnlInstrId names p, but the
		// OrgnlGrpInf names someone else's message.
		{"foreign orgnl msg id", []byte(strings.Replace(string(camt055InboundDoc(t, "001.01", "cxl-55-5f", "ACME Corp", p.UETR, p.ID, "DUPL")), "<OrgnlMsgId>"+p.ID+"</OrgnlMsgId>", "<OrgnlMsgId>po-someone-else</OrgnlMsgId>", 1)), ErrInvalidCAMT055},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := ParseCAMT055(tc.doc)
			if err != nil {
				// The empty-reason and foreign-MsgId cases parse
				// fine; anything failing here is a test bug.
				t.Fatalf("ParseCAMT055: %v", err)
			}
			res, err := l.ApplyCAMT055(req, "ops")
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("err = %v, want %v", err, tc.wantErr)
			}
			if res.RepairID == "" {
				t.Fatalf("repair_id empty, want a parked record")
			}
			rec, ok := l.GetRepair(res.RepairID)
			if !ok {
				t.Fatalf("repair %q not found in the queue", res.RepairID)
			}
			if rec.Status != RepairStatusOpen {
				t.Errorf("repair status = %q, want open", rec.Status)
			}
		})
	}
}

// A document ParseCAMT055 refuses still parks when it names an
// instruction; garbage that names nothing parks nothing.
func TestParkCAMT055Unparsed(t *testing.T) {
	l := seedPayoutLedger(t)
	p := submittedPayout(t, l, "po-c55-in-6")

	// Wrong version, identifiers intact: parked.
	id, err := l.ParkCAMT055Unparsed(camt055InboundDoc(t, "001.02", "cxl-55-6", "ACME Corp", p.UETR, p.ID, "DUPL"), "ops")
	if err != nil {
		t.Fatalf("ParkCAMT055Unparsed: %v", err)
	}
	if id == "" {
		t.Errorf("repair_id empty, want a parked record for the wrong-version doc")
	}
	if _, ok := l.GetRepair(id); !ok {
		t.Errorf("repair %q not found in the queue", id)
	}
	// Garbage bytes: nothing to park.
	if id, err := l.ParkCAMT055Unparsed([]byte("<Document><broken"), "ops"); err != nil || id != "" {
		t.Errorf("garbage: id = %q err = %v, want empty id, nil error", id, err)
	}
}
