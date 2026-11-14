package ledger

import (
	"encoding/xml"
	"errors"
	"strings"
	"testing"
)

// camt056InboundDoc renders a minimal camt.056 inbound document for
// the given schema version: the counterparty's cancellation request
// for uetr, naming payoutID, for reason.
func camt056InboundDoc(t *testing.T, version, msgID, assignor, uetr, payoutID, reason string) []byte {
	t.Helper()
	var sb strings.Builder
	sb.WriteString(xml.Header)
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.056.` + version + `">`)
	sb.WriteString(`<FIToFICxlReq><Assgnmt><Id>` + msgID + `</Id>`)
	sb.WriteString(`<Assgnr><Nm>` + assignor + `</Nm></Assgnr>`)
	sb.WriteString(`<Assgte><Nm>our-bank</Nm></Assgte>`)
	sb.WriteString(`<CreDtTm>2026-11-13T10:00:00Z</CreDtTm></Assgnmt>`)
	sb.WriteString(`<Undrlyg><TxInf><CxlId>` + msgID + `</CxlId>`)
	sb.WriteString(`<OrgnlUETR>` + uetr + `</OrgnlUETR>`)
	sb.WriteString(`<OrgnlGrpInf><OrgnlMsgId>` + payoutID + `</OrgnlMsgId>`)
	sb.WriteString(`<OrgnlMsgNmId>pacs.008.001.02</OrgnlMsgNmId></OrgnlGrpInf>`)
	sb.WriteString(`<CxlRsn><Rsn><Prtry>` + reason + `</Prtry></Rsn></CxlRsn>`)
	sb.WriteString(`</TxInf></Undrlyg></FIToFICxlReq></Document>`)
	return []byte(sb.String())
}

func TestParseCAMT056(t *testing.T) {
	req, err := ParseCAMT056(camt056InboundDoc(t, "001.08", "cxl-in-1", "BANKDEFFXXX", "uetr-x", "po-x", "DUPL"))
	if err != nil {
		t.Fatalf("ParseCAMT056: %v", err)
	}
	if req.MsgID != "cxl-in-1" {
		t.Errorf("msg_id = %q, want cxl-in-1", req.MsgID)
	}
	if req.Assgnr != "BANKDEFFXXX" {
		t.Errorf("assignor = %q, want BANKDEFFXXX", req.Assgnr)
	}
	if req.UETR != "uetr-x" {
		t.Errorf("uetr = %q, want uetr-x", req.UETR)
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

func TestParseCAMT056_Invalid(t *testing.T) {
	for name, doc := range map[string][]byte{
		"empty":        {},
		"malformed":    []byte("<Document><FIToFICxlReq>"),
		"wrong version": camt056InboundDoc(t, "001.02", "cxl-in-1", "BANKDEFFXXX", "uetr-x", "po-x", "DUPL"),
		"no request": []byte(xml.Header + `<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.056.001.08"></Document>`),
		"empty uetr":   camt056InboundDoc(t, "001.08", "cxl-in-1", "BANKDEFFXXX", "", "po-x", "DUPL"),
		"empty reason": camt056InboundDoc(t, "001.08", "cxl-in-1", "BANKDEFFXXX", "uetr-x", "po-x", ""),
	} {
		if _, err := ParseCAMT056(doc); !errors.Is(err, ErrInvalidCAMT056) {
			t.Errorf("%s: err = %v, want ErrInvalidCAMT056", name, err)
		}
	}
}

// applyInboundCAMT056 parses and applies one inbound camt.056 for
// the submitted payout's UETR, the inbound-apply test convention.
func applyInboundCAMT056(t *testing.T, l *Ledger, msgID, assignor, uetr, payoutID, reason, actor string) PayoutCancellation {
	t.Helper()
	req, err := ParseCAMT056(camt056InboundDoc(t, "001.08", msgID, assignor, uetr, payoutID, reason))
	if err != nil {
		t.Fatalf("ParseCAMT056: %v", err)
	}
	c, err := l.ApplyCAMT056Request(req, actor)
	if err != nil {
		t.Fatalf("ApplyCAMT056Request: %v", err)
	}
	return c
}

func TestApplyCAMT056Request_HappyPath(t *testing.T) {
	l := seedPayoutLedger(t)
	p := submittedPayout(t, l, "po-cxl-in-1")

	c := applyInboundCAMT056(t, l, "cxl-in-1", "BANKDEFFXXX", p.UETR, p.ID, "DUPL", "ops")
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
	// RequestedBy names the counterparty, not the operator who
	// applied the request.
	if c.RequestedBy != "BANKDEFFXXX" {
		t.Errorf("requested_by = %q, want BANKDEFFXXX (the counterparty)", c.RequestedBy)
	}
	if c.RequestedAt.IsZero() {
		t.Errorf("requested_at is zero, want the receipt time")
	}
}

func TestApplyCAMT056Request_IdempotentReplay(t *testing.T) {
	l := seedPayoutLedger(t)
	p := submittedPayout(t, l, "po-cxl-in-2")

	first := applyInboundCAMT056(t, l, "cxl-in-2", "BANKDEFFXXX", p.UETR, p.ID, "DUPL", "ops")
	second := applyInboundCAMT056(t, l, "cxl-in-2", "BANKDEFFXXX", p.UETR, p.ID, "DUPL", "ops")
	if second.MsgID != first.MsgID || second.Seq != 1 {
		t.Errorf("replay created a new record: %+v", second)
	}
	if second.RequestedBy != "BANKDEFFXXX" {
		t.Errorf("replay requested_by = %q, want BANKDEFFXXX", second.RequestedBy)
	}
}

func TestApplyCAMT056Request_UnknownUETR(t *testing.T) {
	l := seedPayoutLedger(t)
	req, err := ParseCAMT056(camt056InboundDoc(t, "001.08", "cxl-in-3", "BANKDEFFXXX", "no-such-uetr", "po-x", "DUPL"))
	if err != nil {
		t.Fatalf("ParseCAMT056: %v", err)
	}
	if _, err := l.ApplyCAMT056Request(req, "ops"); !errors.Is(err, ErrCancellationUnknownUETR) {
		t.Errorf("err = %v, want ErrCancellationUnknownUETR", err)
	}
}

func TestApplyCAMT056Request_NotIssued(t *testing.T) {
	l := seedPayoutLedger(t)
	p := draftPayout(t, l, "po-cxl-in-4", 250_000) // draft, never submitted
	req, err := ParseCAMT056(camt056InboundDoc(t, "001.08", "cxl-in-4", "BANKDEFFXXX", p.UETR, p.ID, "DUPL"))
	if err != nil {
		t.Fatalf("ParseCAMT056: %v", err)
	}
	if _, err := l.ApplyCAMT056Request(req, "ops"); !errors.Is(err, ErrCancellationNotIssued) {
		t.Errorf("err = %v, want ErrCancellationNotIssued", err)
	}
}

func TestApplyCAMT056Request_SettledTrack(t *testing.T) {
	l := seedPayoutLedger(t)
	p := submittedPayout(t, l, "po-cxl-in-5")
	if _, err := l.AdvanceGPI(p.UETR, GPIStatusACCP, "", "night-ops"); err != nil {
		t.Fatalf("AdvanceGPI to ACCP: %v", err)
	}
	req, err := ParseCAMT056(camt056InboundDoc(t, "001.08", "cxl-in-5", "BANKDEFFXXX", p.UETR, p.ID, "DUPL"))
	if err != nil {
		t.Fatalf("ParseCAMT056: %v", err)
	}
	if _, err := l.ApplyCAMT056Request(req, "ops"); !errors.Is(err, ErrCancellationSettled) {
		t.Errorf("err = %v, want ErrCancellationSettled", err)
	}
}

func TestApplyCAMT056Request_RejectedTrack(t *testing.T) {
	l := seedPayoutLedger(t)
	p := submittedPayout(t, l, "po-cxl-in-6")
	if _, err := l.AdvanceGPI(p.UETR, GPIStatusRJCT, "AC03", "night-ops"); err != nil {
		t.Fatalf("AdvanceGPI to RJCT: %v", err)
	}
	req, err := ParseCAMT056(camt056InboundDoc(t, "001.08", "cxl-in-6", "BANKDEFFXXX", p.UETR, p.ID, "DUPL"))
	if err != nil {
		t.Fatalf("ParseCAMT056: %v", err)
	}
	if _, err := l.ApplyCAMT056Request(req, "ops"); !errors.Is(err, ErrCancellationTerminal) {
		t.Errorf("err = %v, want ErrCancellationTerminal", err)
	}
}

func TestApplyCAMT056Request_AlreadyCancelled(t *testing.T) {
	l := seedPayoutLedger(t)
	p := submittedPayout(t, l, "po-cxl-in-7")
	if _, err := l.RequestPayoutCancellation(p.UETR, "DUPL", "night-ops"); err != nil {
		t.Fatalf("request: %v", err)
	}
	if _, err := l.ResolvePayoutCancellation(p.UETR, CancellationConfirmed, "", "night-ops"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	req, err := ParseCAMT056(camt056InboundDoc(t, "001.08", "cxl-in-7", "BANKDEFFXXX", p.UETR, p.ID, "DUPL"))
	if err != nil {
		t.Fatalf("ParseCAMT056: %v", err)
	}
	if _, err := l.ApplyCAMT056Request(req, "ops"); !errors.Is(err, ErrCancellationAlreadyCancelled) {
		t.Errorf("err = %v, want ErrCancellationAlreadyCancelled", err)
	}
}

// The counterparty's camt.029 still resolves a cancellation the
// counterparty itself opened: the inbound request and the outbound
// resolution share one record.
func TestApplyCAMT056Request_ThenCAMT029Resolves(t *testing.T) {
	l := seedPayoutLedger(t)
	p := submittedPayout(t, l, "po-cxl-in-8")
	applyInboundCAMT056(t, l, "cxl-in-8", "BANKDEFFXXX", p.UETR, p.ID, "DUPL", "ops")

	rpt, err := ParseCAMT029(camt029Doc(t, p.UETR, p.ID, "ACCR", "", ""))
	if err != nil {
		t.Fatalf("ParseCAMT029: %v", err)
	}
	c, err := l.ApplyCAMT029Report(rpt, "ops")
	if err != nil {
		t.Fatalf("ApplyCAMT029Report: %v", err)
	}
	if c.Status != CancellationConfirmed {
		t.Errorf("status = %q, want confirmed", c.Status)
	}
	if c.RequestedBy != "BANKDEFFXXX" {
		t.Errorf("requested_by after resolve = %q, want BANKDEFFXXX", c.RequestedBy)
	}
	got, _ := l.GetPayout(p.ID)
	if got.Status != PayoutCancelled {
		t.Errorf("payout status = %q, want cancelled", got.Status)
	}
}
