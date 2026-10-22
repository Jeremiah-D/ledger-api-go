package ledger

import (
	"encoding/xml"
	"errors"
	"strings"
	"testing"
)

// submittedPayout drafts, approves, and submits a payout: the only
// payout state with money in flight, hence the only one a camt.056
// cancellation may address.
func submittedPayout(t *testing.T, l *Ledger, id string) PayoutInstruction {
	t.Helper()
	draftPayout(t, l, id, 250_000)
	return approveSubmitPayout(t, l, id, "bob")
}

func TestRequestPayoutCancellation_HappyPath(t *testing.T) {
	l := seedPayoutLedger(t)
	p := submittedPayout(t, l, "po-cxl-1")

	c, err := l.RequestPayoutCancellation(p.UETR, "DUPL", "night-ops")
	if err != nil {
		t.Fatalf("RequestPayoutCancellation: %v", err)
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
	if c.RequestedBy != "night-ops" {
		t.Errorf("requested_by = %q, want night-ops", c.RequestedBy)
	}
	if c.PayoutID != p.ID {
		t.Errorf("payout_id = %q, want %q", c.PayoutID, p.ID)
	}
}

func TestRequestPayoutCancellation_IdempotentReplay(t *testing.T) {
	l := seedPayoutLedger(t)
	p := submittedPayout(t, l, "po-cxl-2")

	first, err := l.RequestPayoutCancellation(p.UETR, "DUPL", "night-ops")
	if err != nil {
		t.Fatalf("first request: %v", err)
	}
	second, err := l.RequestPayoutCancellation(p.UETR, "DUPL", "night-ops")
	if err != nil {
		t.Fatalf("replay request: %v", err)
	}
	if second.MsgID != first.MsgID || second.Seq != 1 {
		t.Errorf("replay created a new record: %+v", second)
	}
}

func TestRequestPayoutCancellation_EmptyReason(t *testing.T) {
	l := seedPayoutLedger(t)
	p := submittedPayout(t, l, "po-cxl-3")
	if _, err := l.RequestPayoutCancellation(p.UETR, "  ", "night-ops"); !errors.Is(err, ErrCancellationReasonRequired) {
		t.Errorf("err = %v, want ErrCancellationReasonRequired", err)
	}
}

func TestRequestPayoutCancellation_UnknownUETR(t *testing.T) {
	l := seedPayoutLedger(t)
	if _, err := l.RequestPayoutCancellation("no-such-uetr", "DUPL", "night-ops"); !errors.Is(err, ErrCancellationUnknownUETR) {
		t.Errorf("err = %v, want ErrCancellationUnknownUETR", err)
	}
}

func TestRequestPayoutCancellation_NotIssued(t *testing.T) {
	l := seedPayoutLedger(t)
	p := draftPayout(t, l, "po-cxl-4", 250_000) // draft, never submitted
	if _, err := l.RequestPayoutCancellation(p.UETR, "DUPL", "night-ops"); !errors.Is(err, ErrCancellationNotIssued) {
		t.Errorf("err = %v, want ErrCancellationNotIssued", err)
	}
}

func TestRequestPayoutCancellation_SettledTrack(t *testing.T) {
	l := seedPayoutLedger(t)
	p := submittedPayout(t, l, "po-cxl-5")
	if _, err := l.AdvanceGPI(p.UETR, GPIStatusACCP, "", "night-ops"); err != nil {
		t.Fatalf("AdvanceGPI to ACCP: %v", err)
	}
	if _, err := l.RequestPayoutCancellation(p.UETR, "DUPL", "night-ops"); !errors.Is(err, ErrCancellationSettled) {
		t.Errorf("err = %v, want ErrCancellationSettled", err)
	}
}

func TestRequestPayoutCancellation_RejectedTrack(t *testing.T) {
	l := seedPayoutLedger(t)
	p := submittedPayout(t, l, "po-cxl-6")
	if _, err := l.AdvanceGPI(p.UETR, GPIStatusRJCT, "AC03", "night-ops"); err != nil {
		t.Fatalf("AdvanceGPI to RJCT: %v", err)
	}
	if _, err := l.RequestPayoutCancellation(p.UETR, "DUPL", "night-ops"); !errors.Is(err, ErrCancellationTerminal) {
		t.Errorf("err = %v, want ErrCancellationTerminal", err)
	}
}

// parseCamt056Back unmarshals an exported camt.056 document into the
// wire structs: the honest round-trip check on the export.
func parseCamt056Back(t *testing.T, doc []byte) camt056Document {
	t.Helper()
	var back camt056Document
	if err := xml.Unmarshal(doc, &back); err != nil {
		t.Fatalf("export is not parseable camt.056 XML: %v", err)
	}
	return back
}

func TestExportCancellationRequest(t *testing.T) {
	l := seedPayoutLedger(t)
	p := submittedPayout(t, l, "po-cxl-7")
	c, err := l.RequestPayoutCancellation(p.UETR, "DUPL", "night-ops")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	doc, err := l.ExportCancellationRequest(p.UETR)
	if err != nil {
		t.Fatalf("ExportCancellationRequest: %v", err)
	}
	back := parseCamt056Back(t, doc)
	req := back.Request
	if req.Assgnmt.ID != c.MsgID {
		t.Errorf("Assgnmt/Id = %q, want %q", req.Assgnmt.ID, c.MsgID)
	}
	tx := req.Undrlyg.TxInf
	if tx.CxlID != c.MsgID {
		t.Errorf("CxlId = %q, want %q", tx.CxlID, c.MsgID)
	}
	if tx.OrgnlUETR != p.UETR {
		t.Errorf("OrgnlUETR = %q, want %q", tx.OrgnlUETR, p.UETR)
	}
	if tx.OrgnlGrpInf.OrgnlMsgID != p.ID {
		t.Errorf("OrgnlMsgId = %q, want %q", tx.OrgnlGrpInf.OrgnlMsgID, p.ID)
	}
	if tx.OrgnlGrpInf.OrgnlMsgNmID != pacs002OrgnlMsgName {
		t.Errorf("OrgnlMsgNmId = %q, want %q", tx.OrgnlGrpInf.OrgnlMsgNmID, pacs002OrgnlMsgName)
	}
	if tx.CxlRsn.Rsn == nil || tx.CxlRsn.Rsn.Prtry != "DUPL" {
		t.Errorf("CxlRsn = %+v, want Prtry DUPL", tx.CxlRsn.Rsn)
	}
	if req.Assgnmt.Assgte.Nm != p.Destination {
		t.Errorf("Assgte = %q, want destination %q", req.Assgnmt.Assgte.Nm, p.Destination)
	}
	// No BIC registered for saver: assignor falls back to the account ID.
	if req.Assgnmt.Assgnr.Nm != "saver" {
		t.Errorf("Assgnr = %q, want saver (no BIC registered)", req.Assgnmt.Assgnr.Nm)
	}
}

func TestExportCancellationRequest_AssignorBIC(t *testing.T) {
	l := seedPayoutLedger(t)
	if err := l.SetAccountIBAN("saver", "DE89370400440532013000", "DEUTDEFF"); err != nil {
		t.Fatalf("SetAccountIBAN: %v", err)
	}
	p := submittedPayout(t, l, "po-cxl-8")
	if _, err := l.RequestPayoutCancellation(p.UETR, "DUPL", "night-ops"); err != nil {
		t.Fatalf("request: %v", err)
	}
	doc, err := l.ExportCancellationRequest(p.UETR)
	if err != nil {
		t.Fatalf("ExportCancellationRequest: %v", err)
	}
	if got := parseCamt056Back(t, doc).Request.Assgnmt.Assgnr.Nm; got != "DEUTDEFF" {
		t.Errorf("Assgnr = %q, want registered BIC DEUTDEFF", got)
	}
}

func TestExportCancellationRequest_NoOpen(t *testing.T) {
	l := seedPayoutLedger(t)
	p := submittedPayout(t, l, "po-cxl-9")
	if _, err := l.ExportCancellationRequest(p.UETR); !errors.Is(err, ErrNoOpenCancellation) {
		t.Errorf("no cancellation yet: err = %v, want ErrNoOpenCancellation", err)
	}
	if _, err := l.ExportCancellationRequest("no-such-uetr"); !errors.Is(err, ErrNoOpenCancellation) {
		t.Errorf("unknown UETR: err = %v, want ErrNoOpenCancellation", err)
	}
}

// camt029Doc renders a minimal camt.029.001.03 for the apply tests.
func camt029Doc(t *testing.T, uetr, payoutID, cxlSts, reasonCd, reasonInfo string) []byte {
	t.Helper()
	var sb strings.Builder
	sb.WriteString(xml.Header)
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.029.001.03">`)
	sb.WriteString(`<FIToFICxlStsRpt><Assgnmt><Id>rpt-1</Id></Assgnmt><RslvdCxl>`)
	sb.WriteString(`<OrgnlUETR>` + uetr + `</OrgnlUETR>`)
	sb.WriteString(`<OrgnlMsgId>` + payoutID + `</OrgnlMsgId>`)
	sb.WriteString(`<CxlSts><Cd>` + cxlSts + `</Cd></CxlSts>`)
	if reasonCd != "" || reasonInfo != "" {
		sb.WriteString(`<CxlStsRsnInf><Rsn><Cd>` + reasonCd + `</Cd></Rsn>`)
		sb.WriteString(`<AddtlInf>` + reasonInfo + `</AddtlInf></CxlStsRsnInf>`)
	}
	sb.WriteString(`</RslvdCxl></FIToFICxlStsRpt></Document>`)
	return []byte(sb.String())
}

func TestParseCAMT029(t *testing.T) {
	rpt, err := ParseCAMT029(camt029Doc(t, "uetr-x", "po-x", "ACCR", "", ""))
	if err != nil {
		t.Fatalf("ParseCAMT029: %v", err)
	}
	if rpt.Resolution != CancellationConfirmed {
		t.Errorf("resolution = %q, want confirmed", rpt.Resolution)
	}
	if rpt.OrgnlUETR != "uetr-x" {
		t.Errorf("orgnl_uetr = %q", rpt.OrgnlUETR)
	}

	rpt, err = ParseCAMT029(camt029Doc(t, "uetr-y", "po-y", "RJCR", "ARDT", "duplicate of earlier instruction"))
	if err != nil {
		t.Fatalf("ParseCAMT029: %v", err)
	}
	if rpt.Resolution != CancellationRejected {
		t.Errorf("resolution = %q, want rejected", rpt.Resolution)
	}
	if camt029ResolutionReason(rpt) != "ARDT: duplicate of earlier instruction" {
		t.Errorf("reason = %q", camt029ResolutionReason(rpt))
	}
}

func TestParseCAMT029_Invalid(t *testing.T) {
	for name, doc := range map[string][]byte{
		"empty":      {},
		"malformed":  []byte("<Document><FIToFICxlStsRpt>"),
		"no rslvdcxl": []byte(xml.Header + `<Document><FIToFICxlStsRpt><Assgnmt><Id>x</Id></Assgnmt></FIToFICxlStsRpt></Document>`),
		"empty uetr": camt029Doc(t, "", "po-x", "ACCR", "", ""),
		"bad cxlsts": camt029Doc(t, "uetr-x", "po-x", "PEND", "", ""),
	} {
		if _, err := ParseCAMT029(doc); !errors.Is(err, ErrInvalidCAMT029) {
			t.Errorf("%s: err = %v, want ErrInvalidCAMT029", name, err)
		}
	}
}

func TestApplyCAMT029_Confirmed(t *testing.T) {
	l := seedPayoutLedger(t)
	p := submittedPayout(t, l, "po-cxl-10")
	if _, err := l.RequestPayoutCancellation(p.UETR, "DUPL", "night-ops"); err != nil {
		t.Fatalf("request: %v", err)
	}
	rpt, err := ParseCAMT029(camt029Doc(t, p.UETR, p.ID, "ACCR", "", ""))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c, err := l.ApplyCAMT029Report(rpt, "night-ops")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if c.Status != CancellationConfirmed {
		t.Errorf("status = %q, want confirmed", c.Status)
	}
	// The payout is marked cancelled: the instruction is withdrawn.
	got, ok := l.GetPayout(p.ID)
	if !ok {
		t.Fatal("payout vanished")
	}
	if got.Status != PayoutCancelled {
		t.Errorf("payout status = %q, want cancelled", got.Status)
	}
	// The gpi track is untouched: cancellation is not a bank
	// settlement verdict.
	track, ok := l.GetGPITrack(p.UETR)
	if !ok {
		t.Fatal("gpi track vanished")
	}
	if track.Status != GPIStatusTracked {
		t.Errorf("gpi status = %q, want untouched tracked", track.Status)
	}
	// Export now refuses: the cancellation is resolved.
	if _, err := l.ExportCancellationRequest(p.UETR); !errors.Is(err, ErrNoOpenCancellation) {
		t.Errorf("export after confirm: err = %v, want ErrNoOpenCancellation", err)
	}
}

func TestApplyCAMT029_Rejected(t *testing.T) {
	l := seedPayoutLedger(t)
	p := submittedPayout(t, l, "po-cxl-11")
	if _, err := l.RequestPayoutCancellation(p.UETR, "DUPL", "night-ops"); err != nil {
		t.Fatalf("request: %v", err)
	}
	rpt, err := ParseCAMT029(camt029Doc(t, p.UETR, p.ID, "RJCR", "ARDT", "already released"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c, err := l.ApplyCAMT029Report(rpt, "night-ops")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if c.Status != CancellationRejected {
		t.Errorf("status = %q, want rejected", c.Status)
	}
	if c.ResolutionCode != "ARDT: already released" {
		t.Errorf("resolution_code = %q", c.ResolutionCode)
	}
	// The payout stays submitted: the refusal changes nothing.
	got, _ := l.GetPayout(p.ID)
	if got.Status != PayoutSubmitted {
		t.Errorf("payout status = %q, want still submitted", got.Status)
	}
}

func TestApplyCAMT029_NoOpen(t *testing.T) {
	l := seedPayoutLedger(t)
	p := submittedPayout(t, l, "po-cxl-12")
	rpt, err := ParseCAMT029(camt029Doc(t, p.UETR, p.ID, "ACCR", "", ""))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := l.ApplyCAMT029Report(rpt, "night-ops"); !errors.Is(err, ErrNoOpenCancellation) {
		t.Errorf("no open cancellation: err = %v, want ErrNoOpenCancellation", err)
	}
	rpt2, _ := ParseCAMT029(camt029Doc(t, "no-such-uetr", "po-x", "ACCR", "", ""))
	if _, err := l.ApplyCAMT029Report(rpt2, "night-ops"); !errors.Is(err, ErrCancellationUnknownUETR) {
		t.Errorf("unknown UETR: err = %v, want ErrCancellationUnknownUETR", err)
	}
}

func TestResolvePayoutCancellation_IdempotentReplay(t *testing.T) {
	l := seedPayoutLedger(t)
	p := submittedPayout(t, l, "po-cxl-13")
	if _, err := l.RequestPayoutCancellation(p.UETR, "DUPL", "night-ops"); err != nil {
		t.Fatalf("request: %v", err)
	}
	first, err := l.ResolvePayoutCancellation(p.UETR, CancellationConfirmed, "bank accepted", "night-ops")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	second, err := l.ResolvePayoutCancellation(p.UETR, CancellationConfirmed, "bank accepted", "night-ops")
	if err != nil {
		t.Fatalf("replay resolve: %v", err)
	}
	if second.MsgID != first.MsgID {
		t.Errorf("replay changed the record: %+v", second)
	}
}

func TestResolvePayoutCancellation_ConflictingResolution(t *testing.T) {
	l := seedPayoutLedger(t)
	p := submittedPayout(t, l, "po-cxl-14")
	if _, err := l.RequestPayoutCancellation(p.UETR, "DUPL", "night-ops"); err != nil {
		t.Fatalf("request: %v", err)
	}
	if _, err := l.ResolvePayoutCancellation(p.UETR, CancellationConfirmed, "", "night-ops"); err != nil {
		t.Fatalf("resolve confirmed: %v", err)
	}
	if _, err := l.ResolvePayoutCancellation(p.UETR, CancellationRejected, "ARDT", "night-ops"); !errors.Is(err, ErrCancellationAlreadyResolved) {
		t.Errorf("conflicting resolution: err = %v, want ErrCancellationAlreadyResolved", err)
	}
}

func TestRequestPayoutCancellation_RetryAfterRejection(t *testing.T) {
	l := seedPayoutLedger(t)
	p := submittedPayout(t, l, "po-cxl-15")
	if _, err := l.RequestPayoutCancellation(p.UETR, "DUPL", "night-ops"); err != nil {
		t.Fatalf("request: %v", err)
	}
	if _, err := l.ResolvePayoutCancellation(p.UETR, CancellationRejected, "ARDT", "night-ops"); err != nil {
		t.Fatalf("resolve rejected: %v", err)
	}
	// A fresh attempt after rejection: new sequence, new MsgId.
	c, err := l.RequestPayoutCancellation(p.UETR, "CUST", "day-ops")
	if err != nil {
		t.Fatalf("retry request: %v", err)
	}
	if c.Seq != 2 || c.MsgID != cancellationMsgID(p.UETR, 2) {
		t.Errorf("retry = %+v, want seq 2", c)
	}
	if c.Status != CancellationRequested {
		t.Errorf("status = %q, want requested", c.Status)
	}
}

func TestRequestPayoutCancellation_AlreadyCancelled(t *testing.T) {
	l := seedPayoutLedger(t)
	p := submittedPayout(t, l, "po-cxl-16")
	if _, err := l.RequestPayoutCancellation(p.UETR, "DUPL", "night-ops"); err != nil {
		t.Fatalf("request: %v", err)
	}
	if _, err := l.ResolvePayoutCancellation(p.UETR, CancellationConfirmed, "", "night-ops"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := l.RequestPayoutCancellation(p.UETR, "DUPL", "night-ops"); !errors.Is(err, ErrCancellationAlreadyCancelled) {
		t.Errorf("re-request after confirm: err = %v, want ErrCancellationAlreadyCancelled", err)
	}
}

func TestResolvePayoutCancellation_InvalidResolution(t *testing.T) {
	l := seedPayoutLedger(t)
	p := submittedPayout(t, l, "po-cxl-17")
	if _, err := l.RequestPayoutCancellation(p.UETR, "DUPL", "night-ops"); err != nil {
		t.Fatalf("request: %v", err)
	}
	if _, err := l.ResolvePayoutCancellation(p.UETR, "maybe", "", "night-ops"); !errors.Is(err, ErrInvalidCancellationResolution) {
		t.Errorf("err = %v, want ErrInvalidCancellationResolution", err)
	}
}
