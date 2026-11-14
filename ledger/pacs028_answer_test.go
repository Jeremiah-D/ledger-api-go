package ledger

import (
	"bytes"
	"errors"
	"regexp"
	"strings"
	"testing"
)

// pacs028RequestDoc renders a pacs.028.001.03 status request asking
// about orgnlMsgID (a payout ID) for uetr, from requester. An empty
// requester omits the Reqstr elements entirely.
func pacs028RequestDoc(msgID, orgnlMsgID, orgnlMsgNmID, uetr, requester string) string {
	var sb strings.Builder
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pacs.028.001.03">`)
	sb.WriteString(`<FIToFIPmtStsReq><GrpHdr><MsgId>` + msgID + `</MsgId>`)
	sb.WriteString(`<CreDtTm>2026-11-13T10:00:00Z</CreDtTm>`)
	if requester != "" {
		sb.WriteString(`<Reqstr><Id>` + requester + `</Id></Reqstr>`)
	}
	sb.WriteString(`</GrpHdr><OrgnlGrpInfAndSts><OrgnlMsgId>` + orgnlMsgID + `</OrgnlMsgId>`)
	sb.WriteString(`<OrgnlMsgNmId>` + orgnlMsgNmID + `</OrgnlMsgNmId>`)
	sb.WriteString(`<OrgnlUETR>` + uetr + `</OrgnlUETR>`)
	if requester != "" {
		sb.WriteString(`<Reqstr><Id>` + requester + `</Id></Reqstr>`)
	}
	sb.WriteString(`</OrgnlGrpInfAndSts></FIToFIPmtStsReq></Document>`)
	return sb.String()
}

// parsePacs002Back lives in ledger_pacs002_test.go and is reused
// here: the honest round-trip check that the XML the ledger emits is
// well-formed and carries the fields the answer claims.

// A request the ledger itself exported parses back with every field
// intact: the pacs.028.001.03 namespace, FIToFIPmtStsReq root, the
// deterministic MsgId, OrgnlMsgId as the payout ID, OrgnlMsgNmId
// pacs.008.001.02, OrgnlUETR, and the requester.
func TestParsePacs028_RoundTrip(t *testing.T) {
	l := seedPayoutLedger(t)
	p := draftPayout(t, l, "po-pacs028-ans-1", 250_000)

	exported, err := l.ExportPacs028(p.UETR, "bank-ops")
	if err != nil {
		t.Fatalf("ExportPacs028: %v", err)
	}
	if !strings.Contains(string(exported), `xmlns="urn:iso:std:iso:20022:tech:xsd:pacs.028.001.03"`) {
		t.Error("export missing pacs.028.001.03 namespace")
	}
	if !strings.Contains(string(exported), "FIToFIPmtStsReq") {
		t.Error("export missing FIToFIPmtStsReq root")
	}
	req, err := ParsePacs028(exported)
	if err != nil {
		t.Fatalf("ParsePacs028: %v", err)
	}
	if req.MsgID != pacs028MsgID(p.UETR) {
		t.Errorf("MsgId = %q, want %q", req.MsgID, pacs028MsgID(p.UETR))
	}
	if req.OrgnlMsgID != p.ID {
		t.Errorf("OrgnlMsgId = %q, want payout ID %q", req.OrgnlMsgID, p.ID)
	}
	if req.OrgnlMsgNmID != pacs028OrgnlMsgName {
		t.Errorf("OrgnlMsgNmId = %q, want %q", req.OrgnlMsgNmID, pacs028OrgnlMsgName)
	}
	if req.UETR != p.UETR {
		t.Errorf("UETR = %q, want %q", req.UETR, p.UETR)
	}
	if req.Requester != "bank-ops" {
		t.Errorf("Requester = %q, want bank-ops", req.Requester)
	}
}

func TestParsePacs028_WrongMsgName(t *testing.T) {
	l := seedPayoutLedger(t)
	p := draftPayout(t, l, "po-pacs028-ans-2", 250_000)
	doc := pacs028RequestDoc("REQ-1", p.ID, "pacs.008.001.08", p.UETR, "bank-ops")
	if _, err := ParsePacs028([]byte(doc)); !errors.Is(err, ErrPacs028UnexpectedMsgName) {
		t.Errorf("err = %v, want ErrPacs028UnexpectedMsgName", err)
	}
}

func TestParsePacs028_MissingRequester(t *testing.T) {
	l := seedPayoutLedger(t)
	p := draftPayout(t, l, "po-pacs028-ans-3", 250_000)
	doc := pacs028RequestDoc("REQ-1", p.ID, pacs028OrgnlMsgName, p.UETR, "")
	if _, err := ParsePacs028([]byte(doc)); !errors.Is(err, ErrPacs028RequesterRequired) {
		t.Errorf("err = %v, want ErrPacs028RequesterRequired", err)
	}
}

func TestParsePacs028_Malformed(t *testing.T) {
	for _, doc := range []string{"", "   ", "<Document><broken", "<Document></Document>"} {
		if _, err := ParsePacs028([]byte(doc)); !errors.Is(err, ErrInvalidPacs028) {
			t.Errorf("doc %q: err = %v, want ErrInvalidPacs028", doc, err)
		}
	}
}

func TestParsePacs028_MissingUETR(t *testing.T) {
	l := seedPayoutLedger(t)
	p := draftPayout(t, l, "po-pacs028-ans-4", 250_000)
	doc := pacs028RequestDoc("REQ-1", p.ID, pacs028OrgnlMsgName, "", "bank-ops")
	if _, err := ParsePacs028([]byte(doc)); !errors.Is(err, ErrInvalidPacs028) {
		t.Errorf("err = %v, want ErrInvalidPacs028", err)
	}
}

// An ACSC track answers with a pacs.002 report naming the same UETR
// and TxSts.
func TestAnswerPacs028_ACSC(t *testing.T) {
	l := seedPayoutLedger(t)
	p := draftPayout(t, l, "po-pacs028-ans-5", 250_000)
	if _, err := l.AdvanceGPI(p.UETR, GPIStatusACSC, "", "ops"); err != nil {
		t.Fatalf("AdvanceGPI: %v", err)
	}
	req, err := ParsePacs028([]byte(pacs028RequestDoc("REQ-1", p.ID, pacs028OrgnlMsgName, p.UETR, "bank-ops")))
	if err != nil {
		t.Fatalf("ParsePacs028: %v", err)
	}
	doc, err := l.AnswerPacs028(req)
	if err != nil {
		t.Fatalf("AnswerPacs028: %v", err)
	}
	if !strings.Contains(string(doc), "FIToFIPmtStsRpt") {
		t.Error("answer missing FIToFIPmtStsRpt root")
	}
	back := parsePacs002Back(t, doc)
	if back.Report.Tx.OrgnlUETR != p.UETR {
		t.Errorf("OrgnlUETR = %q, want %q", back.Report.Tx.OrgnlUETR, p.UETR)
	}
	if back.Report.Tx.TxSts != "ACSC" {
		t.Errorf("TxSts = %q, want ACSC", back.Report.Tx.TxSts)
	}
	if back.Report.OrgnlGrpInf.OrgnlMsgID != p.ID {
		t.Errorf("OrgnlMsgId = %q, want payout ID %q", back.Report.OrgnlGrpInf.OrgnlMsgID, p.ID)
	}
}

// An RJCT track answers RJCT carrying the bank's reason code.
func TestAnswerPacs028_RJCT(t *testing.T) {
	l := seedPayoutLedger(t)
	p := draftPayout(t, l, "po-pacs028-ans-6", 250_000)
	if _, err := l.AdvanceGPI(p.UETR, GPIStatusRJCT, "AC04", "ops"); err != nil {
		t.Fatalf("AdvanceGPI: %v", err)
	}
	req, err := ParsePacs028([]byte(pacs028RequestDoc("REQ-1", p.ID, pacs028OrgnlMsgName, p.UETR, "bank-ops")))
	if err != nil {
		t.Fatalf("ParsePacs028: %v", err)
	}
	doc, err := l.AnswerPacs028(req)
	if err != nil {
		t.Fatalf("AnswerPacs028: %v", err)
	}
	back := parsePacs002Back(t, doc)
	if back.Report.Tx.TxSts != "RJCT" {
		t.Errorf("TxSts = %q, want RJCT", back.Report.Tx.TxSts)
	}
	if back.Report.Tx.StsRsnInf == nil || back.Report.Tx.StsRsnInf.Rsn == nil || back.Report.Tx.StsRsnInf.Rsn.Prtry != "AC04" {
		t.Errorf("answer missing RJCT reason AC04: %+v", back.Report.Tx)
	}
}

func TestAnswerPacs028_UnknownUETR(t *testing.T) {
	l := seedPayoutLedger(t)
	req := &Pacs028Request{
		MsgID: "REQ-1", OrgnlMsgID: "po-nope", OrgnlMsgNmID: pacs028OrgnlMsgName,
		UETR: "no-such-uetr", Requester: "bank-ops",
	}
	if _, err := l.AnswerPacs028(req); !errors.Is(err, ErrGPIUnknownUETR) {
		t.Errorf("err = %v, want ErrGPIUnknownUETR", err)
	}
}

func TestAnswerPacs028_ForeignMsgID(t *testing.T) {
	l := seedPayoutLedger(t)
	p := draftPayout(t, l, "po-pacs028-ans-7", 250_000)
	other := draftPayout(t, l, "po-pacs028-ans-8", 250_000)
	req, err := ParsePacs028([]byte(pacs028RequestDoc("REQ-1", other.ID, pacs028OrgnlMsgName, p.UETR, "bank-ops")))
	if err != nil {
		t.Fatalf("ParsePacs028: %v", err)
	}
	if _, err := l.AnswerPacs028(req); !errors.Is(err, ErrPacs028ForeignMsgID) {
		t.Errorf("err = %v, want ErrPacs028ForeignMsgID", err)
	}
}

var pacs028CreDtTmRe = regexp.MustCompile(`<CreDtTm>[^<]*</CreDtTm>`)

// A repeated answer is the same reply: same pacs.002 MsgId (from the
// track snapshot) and same TxSts. Only the report's wall-clock
// CreDtTm moves — ExportPacs002 stamps it from time.Now — so the
// byte comparison masks that one element.
func TestAnswerPacs028_RepeatIdentical(t *testing.T) {
	l := seedPayoutLedger(t)
	p := draftPayout(t, l, "po-pacs028-ans-9", 250_000)
	if _, err := l.AdvanceGPI(p.UETR, GPIStatusACSC, "", "ops"); err != nil {
		t.Fatalf("AdvanceGPI: %v", err)
	}
	req, err := ParsePacs028([]byte(pacs028RequestDoc("REQ-1", p.ID, pacs028OrgnlMsgName, p.UETR, "bank-ops")))
	if err != nil {
		t.Fatalf("ParsePacs028: %v", err)
	}
	first, err := l.AnswerPacs028(req)
	if err != nil {
		t.Fatalf("AnswerPacs028: %v", err)
	}
	second, err := l.AnswerPacs028(req)
	if err != nil {
		t.Fatalf("AnswerPacs028: %v", err)
	}
	mask := func(b []byte) []byte {
		return pacs028CreDtTmRe.ReplaceAll(b, []byte("<CreDtTm></CreDtTm>"))
	}
	if !bytes.Equal(mask(first), mask(second)) {
		t.Errorf("repeat answers differ:\n%s\nvs\n%s", first, second)
	}
	if msgID := parsePacs002Back(t, first).Report.GrpHdr.MsgID; msgID != parsePacs002Back(t, second).Report.GrpHdr.MsgID {
		t.Errorf("repeat MsgId differs: %q vs %q", msgID, parsePacs002Back(t, second).Report.GrpHdr.MsgID)
	}
}

// Answering appends no audit event and bumps no version: the answer
// is a statement of fact about the track, not an attestation.
func TestAnswerPacs028_NoAuditEvent(t *testing.T) {
	l := seedPayoutLedger(t)
	p := draftPayout(t, l, "po-pacs028-ans-10", 250_000)
	req, err := ParsePacs028([]byte(pacs028RequestDoc("REQ-1", p.ID, pacs028OrgnlMsgName, p.UETR, "bank-ops")))
	if err != nil {
		t.Fatalf("ParsePacs028: %v", err)
	}
	beforeVersion := l.Version()
	beforeHead, beforeLinks := l.ChainHead()
	if _, err := l.AnswerPacs028(req); err != nil {
		t.Fatalf("AnswerPacs028: %v", err)
	}
	if got := l.Version(); got != beforeVersion {
		t.Errorf("version = %d after answer, want unchanged %d", got, beforeVersion)
	}
	if head, links := l.ChainHead(); head != beforeHead || links != beforeLinks {
		t.Errorf("chain head moved after answer: (%q,%d) vs (%q,%d)", head, links, beforeHead, beforeLinks)
	}
}
