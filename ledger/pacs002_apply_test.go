package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// pacs002TestDoc renders a pacs.002.001.03 FIToFIPmtStsRpt answering
// orgnlMsgID (a `<payout>/pacs008` MsgId) for uetr with the given
// TxSts and optional RJCT reason code.
func pacs002TestDoc(t *testing.T, orgnlMsgID, msgNmID, uetr, txSts, reason string) []byte {
	t.Helper()
	var sb strings.Builder
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pacs.002.001.03">`)
	sb.WriteString(`<FIToFIPmtStsRpt><GrpHdr><MsgId>RPT-1</MsgId>`)
	sb.WriteString(`<CreDtTm>2026-11-13T10:00:00Z</CreDtTm></GrpHdr>`)
	sb.WriteString(`<OrgnlGrpInfAndSts><OrgnlMsgId>` + orgnlMsgID + `</OrgnlMsgId>`)
	sb.WriteString(`<OrgnlMsgNmId>` + msgNmID + `</OrgnlMsgNmId></OrgnlGrpInfAndSts>`)
	sb.WriteString(`<TxInfAndSts><OrgnlEndToEndId>po-pacs002-apply-1</OrgnlEndToEndId>`)
	sb.WriteString(`<OrgnlUETR>` + uetr + `</OrgnlUETR>`)
	sb.WriteString(`<TxSts>` + txSts + `</TxSts>`)
	if reason != "" {
		sb.WriteString(`<StsRsnInf><Rsn><Prtry>` + reason + `</Prtry></Rsn></StsRsnInf>`)
	}
	sb.WriteString(`</TxInfAndSts></FIToFIPmtStsRpt></Document>`)
	return []byte(sb.String())
}

// pacs002TestReport parses doc and applies it, failing the test on
// either error: the happy-path funnel for the apply tests.
func pacs002TestReport(t *testing.T, l *Ledger, doc []byte, actor string) GPITrack {
	t.Helper()
	report, err := ParsePacs002(doc)
	if err != nil {
		t.Fatalf("ParsePacs002: %v", err)
	}
	track, err := l.ApplyPacs002Report(report, actor)
	if err != nil {
		t.Fatalf("ApplyPacs002Report: %v", err)
	}
	return track
}

// gpiStatusEvents reads the audit log and returns the gpi_status
// events in order. The log is closed first, flushing the queue, so
// this is the last thing a test does with that ledger.
func gpiStatusEvents(t *testing.T, al *AuditLog) []AuditEvent {
	t.Helper()
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, corrupt, err := ReadAuditLog(al.Dir(), time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt = %d, want 0", corrupt)
	}
	var out []AuditEvent
	for _, ev := range events {
		if ev.Op == "gpi_status" {
			out = append(out, ev)
		}
	}
	return out
}

// The parser accepts the pacs.002.001.03 document shape: the
// namespace, the FIToFIPmtStsRpt root, the pacs.008.001.02 message
// name, and the `<payout>/pacs008` MsgId.
func TestParsePacs002_Valid(t *testing.T) {
	doc := pacs002TestDoc(t, "po-pacs002-apply-1/pacs008", pacs002OrgnlMsgName, "uetr-1", "ACSP", "")
	if !strings.Contains(string(doc), `xmlns="urn:iso:std:iso:20022:tech:xsd:pacs.002.001.03"`) {
		t.Fatal("test doc missing pacs.002.001.03 xmlns")
	}
	if !strings.Contains(string(doc), "<FIToFIPmtStsRpt>") {
		t.Fatal("test doc missing <FIToFIPmtStsRpt>")
	}
	report, err := ParsePacs002(doc)
	if err != nil {
		t.Fatalf("ParsePacs002: %v", err)
	}
	if report.MsgID != "RPT-1" {
		t.Errorf("MsgId = %q, want RPT-1", report.MsgID)
	}
	if report.OrgnlMsgID != "po-pacs002-apply-1/pacs008" {
		t.Errorf("OrgnlMsgId = %q, want po-pacs002-apply-1/pacs008", report.OrgnlMsgID)
	}
	if report.OrgnlMsgNmID != "pacs.008.001.02" {
		t.Errorf("OrgnlMsgNmId = %q, want pacs.008.001.02", report.OrgnlMsgNmID)
	}
	if report.UETR != "uetr-1" {
		t.Errorf("UETR = %q, want uetr-1", report.UETR)
	}
	if report.TxSts != "ACSP" {
		t.Errorf("TxSts = %q, want ACSP", report.TxSts)
	}
}

// A report answering another message family is refused: the
// OrgnlMsgNmId gate keeps pain.001/pacs.010 verdicts out of the gpi
// mirror.
func TestParsePacs002_WrongMsgName(t *testing.T) {
	doc := pacs002TestDoc(t, "po-pacs002-apply-1/pacs008", "pain.001.001.03", "uetr-1", "ACSP", "")
	if _, err := ParsePacs002(doc); !errors.Is(err, ErrPacs002UnexpectedMsgName) {
		t.Errorf("ParsePacs002 err = %v, want ErrPacs002UnexpectedMsgName", err)
	}
}

// An OrgnlMsgId outside the `<payout>/pacs008` shape is foreign: a
// pain.008 MsgId, a bare payout ID, or a suffix-only MsgId never
// reaches the track. (An empty MsgId is missing, not foreign — see
// TestParsePacs002_Malformed.)
func TestParsePacs002_ForeignMsgID(t *testing.T) {
	for _, msgID := range []string{"pain008-123", "po-pacs002-apply-1", "/pacs008"} {
		doc := pacs002TestDoc(t, msgID, pacs002OrgnlMsgName, "uetr-1", "ACSP", "")
		if _, err := ParsePacs002(doc); !errors.Is(err, ErrPacs002ForeignMsgID) {
			t.Errorf("OrgnlMsgId %q: err = %v, want ErrPacs002ForeignMsgID", msgID, err)
		}
	}
}

// TxSts outside the gpi vocabulary is refused, not coerced: PDNG is
// not a pacs.002 verdict, and neither is an invented code.
func TestParsePacs002_InvalidTxSts(t *testing.T) {
	for _, txSts := range []string{"PDNG", "XXXX", ""} {
		doc := pacs002TestDoc(t, "po-pacs002-apply-1/pacs008", pacs002OrgnlMsgName, "uetr-1", txSts, "")
		if _, err := ParsePacs002(doc); !errors.Is(err, ErrInvalidPacs002TxSts) {
			t.Errorf("TxSts %q: err = %v, want ErrInvalidPacs002TxSts", txSts, err)
		}
	}
}

// Malformed XML, an empty document, and a missing UETR are all
// refused fail-loud.
func TestParsePacs002_Malformed(t *testing.T) {
	if _, err := ParsePacs002([]byte("<Document><broken")); !errors.Is(err, ErrInvalidPacs002) {
		t.Errorf("malformed XML: err = %v, want ErrInvalidPacs002", err)
	}
	if _, err := ParsePacs002(nil); !errors.Is(err, ErrInvalidPacs002) {
		t.Errorf("empty document: err = %v, want ErrInvalidPacs002", err)
	}
	doc := pacs002TestDoc(t, "po-pacs002-apply-1/pacs008", pacs002OrgnlMsgName, "", "ACSP", "")
	if _, err := ParsePacs002(doc); !errors.Is(err, ErrInvalidPacs002) {
		t.Errorf("missing UETR: err = %v, want ErrInvalidPacs002", err)
	}
}

// ACSP, ACSC, and ACCP each advance the track one level, in order.
func TestApplyPacs002Report_AdvancesOneLevel(t *testing.T) {
	l := seedPayoutLedger(t)
	p := draftPayout(t, l, "po-pacs002-apply-1", 250_000)
	msgID := pacs008ExportMsgID(p.ID)

	cases := []struct{ txSts, status string }{
		{"ACSP", GPIStatusACSP},
		{"ACSC", GPIStatusACSC},
		{"ACCP", GPIStatusACCP},
	}
	for i, c := range cases {
		doc := pacs002TestDoc(t, msgID, pacs002OrgnlMsgName, p.UETR, c.txSts, "")
		track := pacs002TestReport(t, l, doc, "bank-status")
		if track.Status != c.status {
			t.Errorf("after %s: status = %q, want %q", c.txSts, track.Status, c.status)
		}
		if len(track.History) != i+1 {
			t.Errorf("after %s: history has %d entries, want %d", c.txSts, len(track.History), i+1)
		}
	}
}

// RJCT carries the bank's reason code verbatim into the gpi_status
// audit event, and the verdict leaves the payout instruction alone:
// the payout stays draft, no funds move on a status report.
func TestApplyPacs002Report_RJCTReasonVerbatim(t *testing.T) {
	al := mustAuditLog(t)
	l := seedPayoutLedger(t, WithAuditLog(al))
	p := draftPayout(t, l, "po-pacs002-apply-1", 250_000)

	doc := pacs002TestDoc(t, pacs008ExportMsgID(p.ID), pacs002OrgnlMsgName, p.UETR, "RJCT", "AC04")
	track := pacs002TestReport(t, l, doc, "bank-status")
	if track.Status != GPIStatusRJCT {
		t.Fatalf("status = %q, want RJCT", track.Status)
	}
	if track.ReasonCode != "AC04" {
		t.Errorf("ReasonCode = %q, want AC04", track.ReasonCode)
	}
	if got := track.History[len(track.History)-1]; got.ReasonCode != "AC04" {
		t.Errorf("history reason = %q, want AC04", got.ReasonCode)
	}
	po, ok := l.GetPayout(p.ID)
	if !ok {
		t.Fatal("payout vanished after RJCT apply")
	}
	if po.Status != "draft" {
		t.Errorf("payout status = %q after RJCT, want draft (verdict must not drive payout)", po.Status)
	}
	events := gpiStatusEvents(t, al)
	if len(events) != 1 {
		t.Fatalf("gpi_status events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Details["reason_code"] != "AC04" {
		t.Errorf("audit reason_code = %v, want AC04", ev.Details["reason_code"])
	}
	if ev.Details["to"] != "RJCT" {
		t.Errorf("audit to = %v, want RJCT", ev.Details["to"])
	}
	if ev.Details["uetr"] != p.UETR {
		t.Errorf("audit uetr = %v, want %q", ev.Details["uetr"], p.UETR)
	}
}

// RJCT without a reason code is refused and the track is untouched.
func TestApplyPacs002Report_RJCTWithoutReason(t *testing.T) {
	l := seedPayoutLedger(t)
	p := draftPayout(t, l, "po-pacs002-apply-1", 250_000)

	doc := pacs002TestDoc(t, pacs008ExportMsgID(p.ID), pacs002OrgnlMsgName, p.UETR, "RJCT", "")
	report, err := ParsePacs002(doc)
	if err != nil {
		t.Fatalf("ParsePacs002: %v", err)
	}
	if _, err := l.ApplyPacs002Report(report, "bank-status"); !errors.Is(err, ErrGPIReasonRequired) {
		t.Errorf("ApplyPacs002Report err = %v, want ErrGPIReasonRequired", err)
	}
	track, _ := l.GetGPITrack(p.UETR)
	if track.Status != GPIStatusTracked || len(track.History) != 0 {
		t.Errorf("track moved on refused RJCT: status=%q history=%d", track.Status, len(track.History))
	}
}

// An unknown UETR is refused: a report can only advance a payment
// the ledger tracks.
func TestApplyPacs002Report_UnknownUETR(t *testing.T) {
	l := seedPayoutLedger(t)
	p := draftPayout(t, l, "po-pacs002-apply-1", 250_000)

	doc := pacs002TestDoc(t, pacs008ExportMsgID(p.ID), pacs002OrgnlMsgName, "no-such-uetr", "ACSP", "")
	report, err := ParsePacs002(doc)
	if err != nil {
		t.Fatalf("ParsePacs002: %v", err)
	}
	if _, err := l.ApplyPacs002Report(report, "bank-status"); !errors.Is(err, ErrGPIUnknownUETR) {
		t.Errorf("ApplyPacs002Report err = %v, want ErrGPIUnknownUETR", err)
	}
}

// A well-shaped MsgId naming another payout is foreign: the UETR is
// the join key, the MsgId is the cross-check, and a mismatch refuses
// the report without touching the track.
func TestApplyPacs002Report_ForeignMsgID(t *testing.T) {
	l := seedPayoutLedger(t)
	p := draftPayout(t, l, "po-pacs002-apply-1", 250_000)

	doc := pacs002TestDoc(t, "po-other-payout/pacs008", pacs002OrgnlMsgName, p.UETR, "ACSP", "")
	report, err := ParsePacs002(doc)
	if err != nil {
		t.Fatalf("ParsePacs002: %v", err)
	}
	if _, err := l.ApplyPacs002Report(report, "bank-status"); !errors.Is(err, ErrPacs002ForeignMsgID) {
		t.Errorf("ApplyPacs002Report err = %v, want ErrPacs002ForeignMsgID", err)
	}
	track, _ := l.GetGPITrack(p.UETR)
	if track.Status != GPIStatusTracked || len(track.History) != 0 {
		t.Errorf("track moved on foreign MsgId: status=%q history=%d", track.Status, len(track.History))
	}
}

// Re-applying the same report is idempotent: the same-state advance
// is a no-op, so no new history entry and no new gpi_status audit
// event.
func TestApplyPacs002Report_ReplayIdempotent(t *testing.T) {
	al := mustAuditLog(t)
	l := seedPayoutLedger(t, WithAuditLog(al))
	p := draftPayout(t, l, "po-pacs002-apply-1", 250_000)

	doc := pacs002TestDoc(t, pacs008ExportMsgID(p.ID), pacs002OrgnlMsgName, p.UETR, "ACSC", "")
	first := pacs002TestReport(t, l, doc, "bank-status")
	if first.Status != GPIStatusACSC {
		t.Fatalf("first apply: status = %q, want ACSC", first.Status)
	}
	second := pacs002TestReport(t, l, doc, "bank-status")
	if second.Status != GPIStatusACSC {
		t.Errorf("replay: status = %q, want ACSC", second.Status)
	}
	if len(second.History) != 1 {
		t.Errorf("replay: history has %d entries, want 1", len(second.History))
	}
	events := gpiStatusEvents(t, al)
	if len(events) != 1 {
		t.Errorf("gpi_status events = %d, want 1 (replay must add none)", len(events))
	}
}

// A terminal track never moves again: ACCP followed by a late ACSC
// report is refused and the track stays settled.
func TestApplyPacs002Report_TerminalRejectsBackward(t *testing.T) {
	l := seedPayoutLedger(t)
	p := draftPayout(t, l, "po-pacs002-apply-1", 250_000)
	msgID := pacs008ExportMsgID(p.ID)

	pacs002TestReport(t, l, pacs002TestDoc(t, msgID, pacs002OrgnlMsgName, p.UETR, "ACCP", ""), "bank-status")
	doc := pacs002TestDoc(t, msgID, pacs002OrgnlMsgName, p.UETR, "ACSC", "")
	report, err := ParsePacs002(doc)
	if err != nil {
		t.Fatalf("ParsePacs002: %v", err)
	}
	if _, err := l.ApplyPacs002Report(report, "bank-status"); !errors.Is(err, ErrGPIInvalidTransition) {
		t.Errorf("ApplyPacs002Report err = %v, want ErrGPIInvalidTransition", err)
	}
	track, _ := l.GetGPITrack(p.UETR)
	if track.Status != GPIStatusACCP {
		t.Errorf("status = %q after refused backward move, want ACCP", track.Status)
	}
}
