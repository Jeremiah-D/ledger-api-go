package ledger

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// pain002TestLedger builds a ledger with an audit log, IBANs, and two
// posted transfers (ps1/ps2, alice -> bob), ready for pain.001 export.
func pain002TestLedger(t *testing.T) (*Ledger, *AuditLog) {
	t.Helper()
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	mustSetIBAN(t, l, "alice", testIBANDE, "")
	mustSetIBAN(t, l, "bob", testIBANGB, "")
	mustPostTransferPain(t, l, "ps1", "alice", "bob", 5000, "USD", time.Time{})
	mustPostTransferPain(t, l, "ps2", "alice", "bob", 7000, "USD", time.Time{})
	return l, al
}

// pain002TxFrag renders one TxInfAndSts fragment for the test report.
func pain002TxFrag(e2e, txSts, reasonCode, reasonInfo string) string {
	var sb strings.Builder
	sb.WriteString("      <TxInfAndSts>\n")
	fmt.Fprintf(&sb, "        <OrgnlEndToEndId>%s</OrgnlEndToEndId>\n", e2e)
	fmt.Fprintf(&sb, "        <TxSts>%s</TxSts>\n", txSts)
	if reasonCode != "" || reasonInfo != "" {
		sb.WriteString("        <StsRsnInf>\n")
		if reasonCode != "" {
			fmt.Fprintf(&sb, "          <Rsn><Cd>%s</Cd></Rsn>\n", reasonCode)
		}
		if reasonInfo != "" {
			fmt.Fprintf(&sb, "          <AddtlInf>%s</AddtlInf>\n", reasonInfo)
		}
		sb.WriteString("        </StsRsnInf>\n")
	}
	sb.WriteString("      </TxInfAndSts>")
	return sb.String()
}

// pain002TestDocNm renders a pain.002.001.03 test document answering
// orgnlMsgID with an explicit OrgnlMsgNmId.
func pain002TestDocNm(t *testing.T, orgnlMsgID, msgNmID string, txFrags ...string) []byte {
	t.Helper()
	return []byte(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pain.002.001.03">
  <CstmrPmtStsRpt>
    <GrpHdr>
      <MsgId>RPT-001</MsgId>
      <CreDtTm>2026-10-21T10:00:00Z</CreDtTm>
    </GrpHdr>
    <OrgnlGrpInfAndSts>
      <OrgnlMsgId>` + orgnlMsgID + `</OrgnlMsgId>
      <OrgnlMsgNmId>` + msgNmID + `</OrgnlMsgNmId>
      <GrpSts>ACSC</GrpSts>
    </OrgnlGrpInfAndSts>
    <OrgnlPmtInfAndSts>
      <OrgnlPmtInfId>PMTINF-1</OrgnlPmtInfId>
` + strings.Join(txFrags, "\n") + `
    </OrgnlPmtInfAndSts>
  </CstmrPmtStsRpt>
</Document>`)
}

// pain002TestDoc renders a pain.002.001.03 test document answering a
// pain.001.001.03 message.
func pain002TestDoc(t *testing.T, orgnlMsgID string, txFrags ...string) []byte {
	t.Helper()
	return pain002TestDocNm(t, orgnlMsgID, "pain.001.001.03", txFrags...)
}

// pain002TxState finds a transfer's state in a PainInstruction.
func pain002TxState(t *testing.T, rec PainInstruction, txID string) Pain001TxState {
	t.Helper()
	for _, tx := range rec.Txs {
		if tx.TxID == txID {
			return tx
		}
	}
	t.Fatalf("transfer %q not in instruction", txID)
	return Pain001TxState{}
}

// ACSC advances to accepted, RJCT advances to rejected with the bank's
// reason code verbatim in the audit event; a mixed instruction stays
// exported.
func TestPain002ApplyACSCAndRJCT(t *testing.T) {
	l, al := pain002TestLedger(t)
	msgID := mustExportPain001MsgID(t, l, Pain001ExportOptions{})

	doc := pain002TestDoc(t, msgID,
		pain002TxFrag("ps1", "ACSC", "", ""),
		pain002TxFrag("ps2", "RJCT", "AC01", "Account identifier invalid"),
	)
	report, err := ParsePain002(doc)
	if err != nil {
		t.Fatalf("ParsePain002: %v", err)
	}
	if report.OrgnlMsgID != msgID {
		t.Errorf("OrgnlMsgID = %q, want %q", report.OrgnlMsgID, msgID)
	}
	if len(report.Txs) != 2 {
		t.Fatalf("len(Txs) = %d, want 2", len(report.Txs))
	}

	rec, err := l.ApplyPain002Report(report, "bank-status")
	if err != nil {
		t.Fatalf("ApplyPain002Report: %v", err)
	}
	if got := pain002TxState(t, rec, "ps1").State; got != Pain001StateAccepted {
		t.Errorf("ps1 state = %q, want %q", got, Pain001StateAccepted)
	}
	ps2 := pain002TxState(t, rec, "ps2")
	if ps2.State != Pain001StateRejected {
		t.Errorf("ps2 state = %q, want %q", ps2.State, Pain001StateRejected)
	}
	if ps2.Reason != "AC01: Account identifier invalid" {
		t.Errorf("ps2 reason = %q, want %q", ps2.Reason, "AC01: Account identifier invalid")
	}
	// Mixed accepted/rejected: the instruction stays exported.
	if rec.State != Pain001StateExported {
		t.Errorf("instruction state = %q, want %q", rec.State, Pain001StateExported)
	}

	// The RJCT reason code lands verbatim in the hash-chained audit
	// event.
	events := pain001StatusEvents(t, al)
	found := false
	for _, ev := range events {
		if ev.Details["tx_id"] == "ps2" && ev.Details["to"] == Pain001StateRejected {
			found = true
			if ev.Details["reason"] != "AC01: Account identifier invalid" {
				t.Errorf("audit reason = %q, want verbatim bank reason", ev.Details["reason"])
			}
		}
	}
	if !found {
		t.Error("no pain001_status audit event for the ps2 rejection")
	}
}

// PDNG is not a verdict: the transfer stays exported and nothing is
// recorded.
func TestPain002PDNGNoOp(t *testing.T) {
	l, al := pain002TestLedger(t)
	msgID := mustExportPain001MsgID(t, l, Pain001ExportOptions{})

	doc := pain002TestDoc(t, msgID,
		pain002TxFrag("ps1", "PDNG", "", ""),
		pain002TxFrag("ps2", "PDNG", "", ""),
	)
	report, err := ParsePain002(doc)
	if err != nil {
		t.Fatalf("ParsePain002: %v", err)
	}
	rec, err := l.ApplyPain002Report(report, "bank-status")
	if err != nil {
		t.Fatalf("ApplyPain002Report: %v", err)
	}
	if rec.State != Pain001StateExported {
		t.Errorf("instruction state = %q, want %q", rec.State, Pain001StateExported)
	}
	for _, txID := range []string{"ps1", "ps2"} {
		if got := pain002TxState(t, rec, txID).State; got != Pain001StateExported {
			t.Errorf("%s state = %q, want %q", txID, got, Pain001StateExported)
		}
	}
	// Only the export's own registered -> exported event exists.
	if events := pain001StatusEvents(t, al); len(events) != 1 {
		t.Errorf("pain001_status events = %d, want 1 (export only)", len(events))
	}
}

// A TxSts outside the vocabulary refuses the whole report.
func TestParsePain002InvalidTxSts(t *testing.T) {
	l, _ := pain002TestLedger(t)
	msgID := mustExportPain001MsgID(t, l, Pain001ExportOptions{})

	doc := pain002TestDoc(t, msgID, pain002TxFrag("ps1", "XXXX", "", ""))
	if _, err := ParsePain002(doc); !errors.Is(err, ErrInvalidPain002TxSts) {
		t.Errorf("ParsePain002 err = %v, want ErrInvalidPain002TxSts", err)
	}
}

// A report answering a non-pain.001 message is refused: applying it
// would misattribute the bank's verdict.
func TestParsePain002UnexpectedMsgName(t *testing.T) {
	l, _ := pain002TestLedger(t)
	msgID := mustExportPain001MsgID(t, l, Pain001ExportOptions{})

	doc := pain002TestDocNm(t, msgID, "pain.008.001.02", pain002TxFrag("ps1", "ACSC", "", ""))
	if _, err := ParsePain002(doc); !errors.Is(err, ErrPain002UnexpectedMsgName) {
		t.Errorf("ParsePain002 err = %v, want ErrPain002UnexpectedMsgName", err)
	}
}

// A report for an instruction the ledger never exported is a client
// error, not a silent drop.
func TestApplyPain002UnknownInstruction(t *testing.T) {
	l, _ := pain002TestLedger(t)
	msgID := mustExportPain001MsgID(t, l, Pain001ExportOptions{})

	doc := pain002TestDoc(t, "PAIN001-NEVER-EXPORTED", pain002TxFrag("ps1", "ACSC", "", ""))
	_ = msgID // the real instruction exists; the report names another
	report, err := ParsePain002(doc)
	if err != nil {
		t.Fatalf("ParsePain002: %v", err)
	}
	if _, err := l.ApplyPain002Report(report, "bank-status"); !errors.Is(err, ErrUnknownPainInstruction) {
		t.Errorf("ApplyPain002Report err = %v, want ErrUnknownPainInstruction", err)
	}
}

// ACCP (accepted, settlement pending) maps to the ledger's single
// accepted state; completion is confirmed by the operator, not
// inferred from the bank's wording.
func TestPain002ACCPMapsToAccepted(t *testing.T) {
	l, _ := pain002TestLedger(t)
	msgID := mustExportPain001MsgID(t, l, Pain001ExportOptions{})

	doc := pain002TestDoc(t, msgID, pain002TxFrag("ps1", "ACCP", "", ""))
	report, err := ParsePain002(doc)
	if err != nil {
		t.Fatalf("ParsePain002: %v", err)
	}
	rec, err := l.ApplyPain002Report(report, "bank-status")
	if err != nil {
		t.Fatalf("ApplyPain002Report: %v", err)
	}
	if got := pain002TxState(t, rec, "ps1").State; got != Pain001StateAccepted {
		t.Errorf("ps1 state = %q, want %q", got, Pain001StateAccepted)
	}
	// The unmentioned ps2 stays exported.
	if got := pain002TxState(t, rec, "ps2").State; got != Pain001StateExported {
		t.Errorf("ps2 state = %q, want %q", got, Pain001StateExported)
	}
}

// Re-applying the same report is idempotent: same-state receipt replay
// records nothing new.
func TestPain002ReplayIdempotent(t *testing.T) {
	l, al := pain002TestLedger(t)
	msgID := mustExportPain001MsgID(t, l, Pain001ExportOptions{})

	doc := pain002TestDoc(t, msgID,
		pain002TxFrag("ps1", "ACSC", "", ""),
		pain002TxFrag("ps2", "RJCT", "AM09", "Wrong amount"),
	)
	report, err := ParsePain002(doc)
	if err != nil {
		t.Fatalf("ParsePain002: %v", err)
	}
	if _, err := l.ApplyPain002Report(report, "bank-status"); err != nil {
		t.Fatalf("first ApplyPain002Report: %v", err)
	}
	rec, err := l.ApplyPain002Report(report, "bank-status")
	if err != nil {
		t.Fatalf("second ApplyPain002Report: %v", err)
	}
	if got := pain002TxState(t, rec, "ps1").State; got != Pain001StateAccepted {
		t.Errorf("ps1 state = %q, want %q", got, Pain001StateAccepted)
	}
	// Export (1) + first apply (2 transitions) = 3 events; the replay
	// adds none.
	if events := pain001StatusEvents(t, al); len(events) != 3 {
		t.Errorf("pain001_status events = %d, want 3 (replay must add none)", len(events))
	}
}

// An empty document is refused fail-loud, not treated as an empty
// no-op report.
func TestParsePain002Empty(t *testing.T) {
	if _, err := ParsePain002(nil); !errors.Is(err, ErrInvalidPain002) {
		t.Errorf("ParsePain002(nil) err = %v, want ErrInvalidPain002", err)
	}
	if _, err := ParsePain002([]byte("   \n  ")); !errors.Is(err, ErrInvalidPain002) {
		t.Errorf("ParsePain002(blank) err = %v, want ErrInvalidPain002", err)
	}
}
