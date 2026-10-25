package ledger

import (
	"errors"
	"strings"
	"testing"
)

// pain002ExportMsgIDOf extracts the GrpHdr MsgId from a rendered
// pain.002 document.
func pain002ExportMsgIDOf(t *testing.T, doc []byte) string {
	t.Helper()
	s := string(doc)
	const open, close = "<MsgId>", "</MsgId>"
	i := strings.Index(s, open)
	j := strings.Index(s, close)
	if i < 0 || j < 0 || j <= i {
		t.Fatalf("exported document has no MsgId:\n%s", s)
	}
	return s[i+len(open) : j]
}

// Golden document: a freshly exported instruction reports every
// transfer PDNG under a pain.002.001.10 root, answering the pain.001
// MsgId.
func TestPain002ExportGoldenDocument(t *testing.T) {
	l := painStatusTestLedger(t)
	msgID := mustExportPain001MsgID(t, l, Pain001ExportOptions{})

	doc, err := l.ExportPain002(msgID)
	if err != nil {
		t.Fatalf("ExportPain002: %v", err)
	}
	s := string(doc)
	for _, want := range []string{
		`<?xml version="1.0" encoding="UTF-8"?>`,
		`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pain.002.001.10">`,
		"<CstmrPmtStsRpt>",
		"<OrgnlMsgId>" + msgID + "</OrgnlMsgId>",
		"<OrgnlMsgNmId>pain.001.001.03</OrgnlMsgNmId>",
		"<GrpSts>PDNG</GrpSts>",
		"<OrgnlEndToEndId>ps1</OrgnlEndToEndId>",
		"<OrgnlEndToEndId>ps2</OrgnlEndToEndId>",
		"<TxSts>PDNG</TxSts>",
		"</Document>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("document missing %q:\n%s", want, s)
		}
	}
	if got := pain002ExportMsgIDOf(t, doc); !strings.HasPrefix(got, "pain002-"+msgID+"-") {
		t.Errorf("report MsgId = %q, want prefix %q", got, "pain002-"+msgID+"-")
	}
}

// The report MsgId is deterministic in (instruction, states): the same
// instruction in the same states re-renders the same MsgId.
func TestPain002ExportDeterministicMsgID(t *testing.T) {
	l := painStatusTestLedger(t)
	msgID := mustExportPain001MsgID(t, l, Pain001ExportOptions{})

	first, err := l.ExportPain002(msgID)
	if err != nil {
		t.Fatalf("first ExportPain002: %v", err)
	}
	second, err := l.ExportPain002(msgID)
	if err != nil {
		t.Fatalf("second ExportPain002: %v", err)
	}
	if got, want := pain002ExportMsgIDOf(t, second), pain002ExportMsgIDOf(t, first); got != want {
		t.Errorf("re-export MsgId = %q, want %q", got, want)
	}
}

// State mapping: accepted -> ACCP, rejected -> RJCT with the bank's
// reason verbatim, settled -> ACSC; a mixed group reports PART.
func TestPain002ExportStateMapping(t *testing.T) {
	l := painStatusTestLedger(t)
	msgID := mustExportPain001MsgID(t, l, Pain001ExportOptions{})
	if _, err := l.RecordPain001Receipts(msgID, []Pain001Receipt{
		{TxID: "ps1", Status: "accepted"},
		{TxID: "ps2", Status: "rejected", Reason: "AC03"},
	}, "test"); err != nil {
		t.Fatalf("RecordPain001Receipts: %v", err)
	}

	doc, err := l.ExportPain002(msgID)
	if err != nil {
		t.Fatalf("ExportPain002: %v", err)
	}
	s := string(doc)
	for _, want := range []string{
		"<GrpSts>PART</GrpSts>",
		"<OrgnlEndToEndId>ps1</OrgnlEndToEndId>",
		"<TxSts>ACCP</TxSts>",
		"<OrgnlEndToEndId>ps2</OrgnlEndToEndId>",
		"<TxSts>RJCT</TxSts>",
		"<AddtlInf>AC03</AddtlInf>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("document missing %q:\n%s", want, s)
		}
	}

	// Settle the accepted leg: ps1 becomes ACSC, ps2 stays RJCT,
	// the group stays PART.
	if _, err := l.SettlePain001Instruction(msgID, "test"); err != nil {
		t.Fatalf("SettlePain001Instruction: %v", err)
	}
	doc, err = l.ExportPain002(msgID)
	if err != nil {
		t.Fatalf("ExportPain002 after settle: %v", err)
	}
	s = string(doc)
	for _, want := range []string{
		"<TxSts>ACSC</TxSts>",
		"<TxSts>RJCT</TxSts>",
		"<GrpSts>PART</GrpSts>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("settled document missing %q:\n%s", want, s)
		}
	}
}

// A fully settled instruction reports ACSC at the group level.
func TestPain002ExportFullySettled(t *testing.T) {
	l := painStatusTestLedger(t)
	msgID := mustExportPain001MsgID(t, l, Pain001ExportOptions{})
	if _, err := l.RecordPain001Receipts(msgID, []Pain001Receipt{
		{TxID: "ps1", Status: "accepted"},
		{TxID: "ps2", Status: "accepted"},
	}, "test"); err != nil {
		t.Fatalf("RecordPain001Receipts: %v", err)
	}
	if _, err := l.SettlePain001Instruction(msgID, "test"); err != nil {
		t.Fatalf("SettlePain001Instruction: %v", err)
	}
	doc, err := l.ExportPain002(msgID)
	if err != nil {
		t.Fatalf("ExportPain002: %v", err)
	}
	s := string(doc)
	if !strings.Contains(s, "<GrpSts>ACSC</GrpSts>") {
		t.Errorf("document missing group ACSC:\n%s", s)
	}
	if strings.Count(s, "<TxSts>ACSC</TxSts>") != 2 {
		t.Errorf("want 2 ACSC transactions:\n%s", s)
	}
}

// A status report about an instruction the ledger never exported is
// a fabrication, not a report: refused fail-closed.
func TestPain002ExportUnknownInstruction(t *testing.T) {
	l := painStatusTestLedger(t)
	if _, err := l.ExportPain002("pain001-nope"); !errors.Is(err, ErrUnknownPainInstruction) {
		t.Errorf("ExportPain002 unknown: err = %v, want ErrUnknownPainInstruction", err)
	}
}

// Round-trip: the generated report parses back through the inbound
// pain.002 parser (LG-162) with the same verdicts — the outbound
// vocabulary is the inbound vocabulary.
func TestPain002ExportRoundTrip(t *testing.T) {
	l := painStatusTestLedger(t)
	msgID := mustExportPain001MsgID(t, l, Pain001ExportOptions{})
	if _, err := l.RecordPain001Receipts(msgID, []Pain001Receipt{
		{TxID: "ps1", Status: "accepted"},
		{TxID: "ps2", Status: "rejected", Reason: "AC03"},
	}, "test"); err != nil {
		t.Fatalf("RecordPain001Receipts: %v", err)
	}
	doc, err := l.ExportPain002(msgID)
	if err != nil {
		t.Fatalf("ExportPain002: %v", err)
	}
	report, err := ParsePain002(doc)
	if err != nil {
		t.Fatalf("ParsePain002(generated): %v", err)
	}
	if report.OrgnlMsgID != msgID {
		t.Errorf("OrgnlMsgID = %q, want %q", report.OrgnlMsgID, msgID)
	}
	if report.OrgnlMsgNmID != "pain.001.001.03" {
		t.Errorf("OrgnlMsgNmID = %q, want pain.001.001.03", report.OrgnlMsgNmID)
	}
	if len(report.Txs) != 2 {
		t.Fatalf("Txs = %d, want 2", len(report.Txs))
	}
	want := map[string]Pain002Tx{
		"ps1": {EndToEndID: "ps1", TxSts: "ACCP"},
		"ps2": {EndToEndID: "ps2", TxSts: "RJCT", ReasonInfo: "AC03"},
	}
	for _, tx := range report.Txs {
		w, ok := want[tx.EndToEndID]
		if !ok {
			t.Errorf("unexpected tx %q", tx.EndToEndID)
			continue
		}
		if tx.TxSts != w.TxSts || tx.ReasonInfo != w.ReasonInfo {
			t.Errorf("tx %q = %+v, want %+v", tx.EndToEndID, tx, w)
		}
	}
}
