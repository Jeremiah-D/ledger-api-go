package ledger

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestExportPain014_Accepted(t *testing.T) {
	ctx := context.Background()
	l, r := pain013Fixture(t)
	if _, err := l.AcceptRfP(ctx, r.ID, "customer"); err != nil {
		t.Fatalf("AcceptRfP: %v", err)
	}

	doc, err := l.ExportPain014(r.ID, Pain014ExportOptions{})
	if err != nil {
		t.Fatalf("ExportPain014: %v", err)
	}
	s := string(doc)
	for _, want := range []string{
		`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pain.014.001.07">`,
		"<CdtrPmtActvtnReqStsRpt>",
		"<MsgId>pain014-rfp-013</MsgId>",
		"<InitgPty>",
		"<Nm>LEDGER</Nm>",
		"<OrgnlMsgId>pain013-rfp-013</OrgnlMsgId>",
		"<OrgnlMsgNmId>pain.013.001.07</OrgnlMsgNmId>",
		"<OrgnlNbOfTxs>1</OrgnlNbOfTxs>",
		"<OrgnlPmtInfId>pain013-rfp-013</OrgnlPmtInfId>",
		"<OrgnlEndToEndId>rfp-013</OrgnlEndToEndId>",
		"<TxSts>ACCP</TxSts>",
		// Full-amount accept: AccptdAmt is the whole RfP amount.
		`<AccptdAmt Ccy="USD">50.00</AccptdAmt>`,
		// OrgnlTxRef carries the original request's amount.
		`<InstdAmt Ccy="USD">50.00</InstdAmt>`,
		"<OrgnlCreDtTm>" + r.CreatedAt.UTC().Format(time.RFC3339) + "</OrgnlCreDtTm>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("document missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "<StsRsnInf>") {
		t.Error("ACCP document carries a StsRsnInf block: an accept states no refusal reason")
	}
	if strings.Contains(s, "<Prtry>") || strings.Contains(s, "<GrntedPmt>") || strings.Contains(s, "<EarlyPmt>") {
		t.Error("document carries an empty optional element the export never sets")
	}
	// XSD element order: GrpHdr, then the original group, then
	// the original payment instruction; inside TxInfAndSts the
	// verdict precedes the condition status and the reference.
	ordered := func(block string, elems ...string) {
		t.Helper()
		prev := -1
		for _, e := range elems {
			i := strings.Index(block, e)
			if i < 0 {
				t.Errorf("document missing %q", e)
				return
			}
			if i <= prev {
				t.Errorf("element %q out of XSD order", e)
				return
			}
			prev = i
		}
	}
	ordered(s, "<GrpHdr>", "<OrgnlGrpInfAndSts>", "<OrgnlPmtInfAndSts>")
	ordered(s, "<OrgnlEndToEndId>", "<TxSts>", "<PmtCondSts>", "<OrgnlTxRef>")
	if pain014MsgID(r.ID) != "pain014-rfp-013" {
		t.Errorf("pain014MsgID = %q, want pain014-rfp-013", pain014MsgID(r.ID))
	}
}

func TestExportPain014_Rejected_WithReason(t *testing.T) {
	ctx := context.Background()
	l, r := pain013Fixture(t)
	if _, err := l.RejectRfP(ctx, r.ID, "customer"); err != nil {
		t.Fatalf("RejectRfP: %v", err)
	}

	doc, err := l.ExportPain014(r.ID, Pain014ExportOptions{ReasonCode: "AC03"})
	if err != nil {
		t.Fatalf("ExportPain014: %v", err)
	}
	s := string(doc)
	for _, want := range []string{
		"<MsgId>pain014-rfp-013</MsgId>",
		"<OrgnlMsgId>pain013-rfp-013</OrgnlMsgId>",
		"<OrgnlMsgNmId>pain.013.001.07</OrgnlMsgNmId>",
		"<OrgnlEndToEndId>rfp-013</OrgnlEndToEndId>",
		"<TxSts>RJCT</TxSts>",
		// The reason code is carried verbatim, never interpreted.
		// (MarshalIndent separates the elements with newlines,
		// so the Cd value — not the compact nesting — is
		// asserted here; the round-trip test nails the nesting.)
		"<Cd>AC03</Cd>",
		// The original amount is still stated.
		`<InstdAmt Ccy="USD">50.00</InstdAmt>`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("document missing %q:\n%s", want, s)
		}
	}
	if i := strings.Index(s, "<StsRsnInf>"); i < 0 || !strings.Contains(s[i:], "<Cd>AC03</Cd>") {
		t.Error("reason code is not inside a StsRsnInf block")
	}
	if strings.Contains(s, "<PmtCondSts>") || strings.Contains(s, "<AccptdAmt") {
		t.Error("RJCT document carries an accepted amount: a refusal accepts nothing")
	}
}

func TestExportPain014_Rejected_NoReason(t *testing.T) {
	ctx := context.Background()
	l, r := pain013Fixture(t)
	if _, err := l.RejectRfP(ctx, r.ID, "customer"); err != nil {
		t.Fatalf("RejectRfP: %v", err)
	}

	doc, err := l.ExportPain014(r.ID, Pain014ExportOptions{})
	if err != nil {
		t.Fatalf("ExportPain014: %v", err)
	}
	s := string(doc)
	if !strings.Contains(s, "<TxSts>RJCT</TxSts>") {
		t.Errorf("document missing RJCT verdict:\n%s", s)
	}
	if strings.Contains(s, "<StsRsnInf>") {
		t.Error("document carries a StsRsnInf block with no reason code: an empty reason renders no block")
	}
}

func TestExportPain014_AcceptedIgnoresReasonCode(t *testing.T) {
	ctx := context.Background()
	l, r := pain013Fixture(t)
	if _, err := l.AcceptRfP(ctx, r.ID, "customer"); err != nil {
		t.Fatalf("AcceptRfP: %v", err)
	}

	// A reason code on an ACCP export is ignored, not rendered:
	// an accept carries no refusal reason.
	doc, err := l.ExportPain014(r.ID, Pain014ExportOptions{ReasonCode: "AC03"})
	if err != nil {
		t.Fatalf("ExportPain014: %v", err)
	}
	s := string(doc)
	if !strings.Contains(s, "<TxSts>ACCP</TxSts>") {
		t.Errorf("document missing ACCP verdict:\n%s", s)
	}
	if strings.Contains(s, "<StsRsnInf>") || strings.Contains(s, "AC03") {
		t.Errorf("ACCP document renders the ignored reason code:\n%s", s)
	}
}

func TestExportPain014_FailClosed(t *testing.T) {
	ctx := context.Background()
	l, r := pain013Fixture(t)

	// Unknown RfP.
	if _, err := l.ExportPain014("rfp-ghost", Pain014ExportOptions{}); !errors.Is(err, ErrRfPNotFound) {
		t.Errorf("unknown RfP: err = %v, want ErrRfPNotFound", err)
	}

	// Issued: no verdict exists to report yet.
	if _, err := l.ExportPain014(r.ID, Pain014ExportOptions{}); !errors.Is(err, ErrRfPNotIssued) {
		t.Errorf("issued RfP: err = %v, want ErrRfPNotIssued", err)
	}

	// A swept-expired RfP refuses with the expiry error.
	l2, r2 := pain013Fixture(t)
	if n := l2.ExpireRfPs(time.Now().Add(2 * time.Hour)); n != 1 {
		t.Fatalf("ExpireRfPs = %d, want 1", n)
	}
	if _, err := l2.ExportPain014(r2.ID, Pain014ExportOptions{}); !errors.Is(err, ErrRfPExpired) {
		t.Errorf("swept-expired RfP: err = %v, want ErrRfPExpired", err)
	}

	// Lazy expiry: an issued RfP whose deadline has passed reads
	// as expired even before any sweep runs.
	l3, r3 := pain013Fixture(t)
	l3.mu.Lock()
	stale := l3.rfps[r3.ID]
	stale.ExpiresAt = time.Now().Add(-time.Second)
	l3.rfps[r3.ID] = stale
	l3.mu.Unlock()
	if _, err := l3.ExportPain014(r3.ID, Pain014ExportOptions{}); !errors.Is(err, ErrRfPExpired) {
		t.Errorf("lazily expired RfP: err = %v, want ErrRfPExpired", err)
	}

	// A terminal RfP renders its verdict even past its ExpiresAt:
	// the decision was already made.
	l4, r4 := pain013Fixture(t)
	if _, err := l4.AcceptRfP(ctx, r4.ID, "customer"); err != nil {
		t.Fatalf("AcceptRfP: %v", err)
	}
	l4.mu.Lock()
	paid := l4.rfps[r4.ID]
	paid.ExpiresAt = time.Now().Add(-time.Second)
	l4.rfps[r4.ID] = paid
	l4.mu.Unlock()
	doc, err := l4.ExportPain014(r4.ID, Pain014ExportOptions{})
	if err != nil {
		t.Fatalf("paid RfP past expiry: err = %v, want the ACCP export", err)
	}
	if !strings.Contains(string(doc), "<TxSts>ACCP</TxSts>") {
		t.Errorf("paid RfP past expiry missing ACCP verdict:\n%s", doc)
	}
}

func TestExportPain014_RoundTrip(t *testing.T) {
	ctx := context.Background()
	l, r := pain013Fixture(t)
	if _, err := l.AcceptRfP(ctx, r.ID, "customer"); err != nil {
		t.Fatalf("AcceptRfP: %v", err)
	}
	doc, err := l.ExportPain014(r.ID, Pain014ExportOptions{})
	if err != nil {
		t.Fatalf("ExportPain014: %v", err)
	}
	resp, err := ParsePain014(doc)
	if err != nil {
		t.Fatalf("ParsePain014(export): %v\n%s", err, doc)
	}
	if resp.Response != Pain014ResponseAccepted || resp.TxStatus != "ACCP" {
		t.Errorf("round-trip verdict = %q/%q, want accepted/ACCP", resp.Response, resp.TxStatus)
	}
	if resp.MsgID != pain014MsgID(r.ID) {
		t.Errorf("round-trip MsgID = %q, want %q", resp.MsgID, pain014MsgID(r.ID))
	}
	if resp.OrgnlMsgID != pain013MsgID(r.ID) || resp.OrgnlMsgNmID != pain014OrgnlMsgName {
		t.Errorf("round-trip original = %q/%q, want %q/pain.013.001.07", resp.OrgnlMsgID, resp.OrgnlMsgNmID, pain013MsgID(r.ID))
	}
	if resp.EndToEndID != r.ID {
		t.Errorf("round-trip EndToEndID = %q, want %q", resp.EndToEndID, r.ID)
	}
	if !resp.HasAmount || resp.AmountCents != r.AmountCents || resp.Currency != r.Currency {
		t.Errorf("round-trip amount = %v/%d/%q, want 5000/USD", resp.HasAmount, resp.AmountCents, resp.Currency)
	}
	if !resp.HasAcceptedAmount || resp.AcceptedAmountCents != r.AmountCents || resp.AcceptedCurrency != r.Currency {
		t.Errorf("round-trip accepted amount = %v/%d/%q, want full 5000/USD", resp.HasAcceptedAmount, resp.AcceptedAmountCents, resp.AcceptedCurrency)
	}

	// The RJCT direction round-trips too, reason verbatim.
	l2, r2 := pain013Fixture(t)
	if _, err := l2.RejectRfP(ctx, r2.ID, "customer"); err != nil {
		t.Fatalf("RejectRfP: %v", err)
	}
	doc2, err := l2.ExportPain014(r2.ID, Pain014ExportOptions{ReasonCode: "AC03"})
	if err != nil {
		t.Fatalf("ExportPain014: %v", err)
	}
	resp2, err := ParsePain014(doc2)
	if err != nil {
		t.Fatalf("ParsePain014(export): %v\n%s", err, doc2)
	}
	if resp2.Response != Pain014ResponseRejected || resp2.TxStatus != "RJCT" {
		t.Errorf("round-trip verdict = %q/%q, want rejected/RJCT", resp2.Response, resp2.TxStatus)
	}
	if resp2.ReasonCode != "AC03" {
		t.Errorf("round-trip reason = %q, want AC03 verbatim", resp2.ReasonCode)
	}
	if !resp2.HasAmount || resp2.AmountCents != r2.AmountCents {
		t.Errorf("round-trip amount = %v/%d, want 5000", resp2.HasAmount, resp2.AmountCents)
	}
	if resp2.HasAcceptedAmount {
		t.Error("round-trip RJCT carries an accepted amount")
	}
}

func TestExportPain014_DeterministicExceptCreDtTm(t *testing.T) {
	ctx := context.Background()
	l, r := pain013Fixture(t)
	if _, err := l.AcceptRfP(ctx, r.ID, "customer"); err != nil {
		t.Fatalf("AcceptRfP: %v", err)
	}

	first, err := l.ExportPain014(r.ID, Pain014ExportOptions{})
	if err != nil {
		t.Fatalf("first export: %v", err)
	}
	second, err := l.ExportPain014(r.ID, Pain014ExportOptions{})
	if err != nil {
		t.Fatalf("second export: %v", err)
	}
	stripCreDtTm := func(doc []byte) string {
		var lines []string
		for _, line := range strings.Split(string(doc), "\n") {
			if !strings.Contains(line, "<CreDtTm>") {
				lines = append(lines, line)
			}
		}
		return strings.Join(lines, "\n")
	}
	if stripCreDtTm(first) != stripCreDtTm(second) {
		t.Errorf("repeat exports differ apart from CreDtTm:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
	if !strings.Contains(string(second), "<MsgId>pain014-rfp-013</MsgId>") {
		t.Error("repeat export carries a different MsgId")
	}
}

func TestExportPain014_ReadOnly(t *testing.T) {
	ctx := context.Background()
	l, r := pain013Fixture(t)
	if _, err := l.AcceptRfP(ctx, r.ID, "customer"); err != nil {
		t.Fatalf("AcceptRfP: %v", err)
	}

	before := l.Version()
	if _, err := l.ExportPain014(r.ID, Pain014ExportOptions{}); err != nil {
		t.Fatalf("ExportPain014: %v", err)
	}
	if l.Version() != before {
		t.Errorf("version moved %d -> %d on a read-only export", before, l.Version())
	}
	got, ok := l.GetRfP(r.ID)
	if !ok {
		t.Fatal("GetRfP: RfP vanished")
	}
	if got.Status != RfPStatusPaid {
		t.Errorf("RfP status = %q after export, want paid (export reports the verdict, it does not decide it)", got.Status)
	}
}

func TestExportPain014_AuditEvent(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	ctx := context.Background()
	mustPostOFX(t, l, "fund", "customer", "equity", 10000, "USD", time.Now().Add(-time.Hour))
	mustSetIBAN(t, l, "customer", testIBANDE, "DEUTDEFF")
	mustSetIBAN(t, l, "merchant", testIBANGB, "NWBKGB2L")
	if _, _, err := l.CreateRfP(ctx, RfP{
		ID: "rfp-014-audit", Creditor: "merchant", Debtor: "customer",
		AmountCents: 5000, Currency: "USD", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("CreateRfP: %v", err)
	}
	if _, err := l.RejectRfP(ctx, "rfp-014-audit", "customer"); err != nil {
		t.Fatalf("RejectRfP: %v", err)
	}

	if _, err := l.ExportPain014("rfp-014-audit", Pain014ExportOptions{ReasonCode: "AC03"}); err != nil {
		t.Fatalf("ExportPain014: %v", err)
	}
	events := readAuditOps(t, al)
	ev := findOp(events, "pain014_exported")
	if ev == nil {
		t.Fatal("pain014_exported audit event missing")
	}
	if countOp(events, "pain014_exported") != 1 {
		t.Errorf("pain014_exported count = %d, want 1", countOp(events, "pain014_exported"))
	}
	if ev.TraceID != pain014MsgID("rfp-014-audit") {
		t.Errorf("trace_id = %q, want the deterministic MsgId", ev.TraceID)
	}
	if ev.Details["msg_id"] != pain014MsgID("rfp-014-audit") ||
		ev.Details["rfp_id"] != "rfp-014-audit" ||
		ev.Details["tx_sts"] != "RJCT" ||
		ev.Details["reason_code"] != "AC03" {
		t.Errorf("audit details = %v, want msg_id/rfp_id/tx_sts/reason_code", ev.Details)
	}
	if ev.VersionBefore != ev.VersionAfter {
		t.Errorf("audit version moved %d -> %d on a read-only export", ev.VersionBefore, ev.VersionAfter)
	}
}
