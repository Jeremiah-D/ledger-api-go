package ledger

import (
	"encoding/xml"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// parsePacs004Back unmarshals an exported pacs.004 document into the
// wire structs: the honest round-trip check that the XML the ledger
// emits is well-formed and carries the fields the export claims.
func parsePacs004Back(t *testing.T, doc []byte) pacs004Document {
	t.Helper()
	var back pacs004Document
	if err := xml.Unmarshal(doc, &back); err != nil {
		t.Fatalf("export is not parseable pacs.004 XML: %v", err)
	}
	return back
}

// settlePayoutForReturn drafts, approves, submits and settles one
// payout, returning the settled instruction. The statement line
// evidences the outflow, the ledger_payout_test.go convention.
func settlePayoutForReturn(t *testing.T, l *Ledger, id string, amount int64) PayoutInstruction {
	t.Helper()
	draftPayout(t, l, id, amount)
	p := approveSubmitPayout(t, l, id, "bob")
	var err error
	p, err = l.SettlePayout(id, statementLineForPayout(p, p.SubmittedAt), StatementMatchOptions{})
	if err != nil {
		t.Fatalf("SettlePayout: %v", err)
	}
	if p.Status != PayoutSettled {
		t.Fatalf("status = %q, want settled", p.Status)
	}
	return p
}

func TestExportPacs004_SettledHappyPath(t *testing.T) {
	l := seedPayoutLedger(t)
	p := settlePayoutForReturn(t, l, "po-pacs004-1", 250_000)

	doc, err := l.ExportPacs004(p.UETR, Pacs004ReturnDuplicate)
	if err != nil {
		t.Fatalf("ExportPacs004: %v", err)
	}
	back := parsePacs004Back(t, doc)
	rtr := back.Return
	if rtr.GrpHdr.MsgID != pacs004MsgID(p.UETR) {
		t.Errorf("MsgId = %q, want %q", rtr.GrpHdr.MsgID, pacs004MsgID(p.UETR))
	}
	if rtr.OrgnlGrpInf.OrgnlMsgID != p.ID {
		t.Errorf("OrgnlMsgId = %q, want payout ID %q", rtr.OrgnlGrpInf.OrgnlMsgID, p.ID)
	}
	if rtr.OrgnlGrpInf.OrgnlMsgNmID != pacs002OrgnlMsgName {
		t.Errorf("OrgnlMsgNmId = %q, want %q", rtr.OrgnlGrpInf.OrgnlMsgNmID, pacs002OrgnlMsgName)
	}
	tx := rtr.TxInf
	if tx.OrgnlUETR != p.UETR {
		t.Errorf("OrgnlUETR = %q, want %q", tx.OrgnlUETR, p.UETR)
	}
	if tx.OrgnlEndToEndID != p.ID {
		t.Errorf("OrgnlEndToEndId = %q, want payout ID %q", tx.OrgnlEndToEndID, p.ID)
	}
	if tx.RtrdIntrBkSttlmAmt.Ccy != p.Currency {
		t.Errorf("amount Ccy = %q, want %q", tx.RtrdIntrBkSttlmAmt.Ccy, p.Currency)
	}
	if tx.RtrdIntrBkSttlmAmt.Value != pain001Amount(p.AmountCents) {
		t.Errorf("amount = %q, want %q", tx.RtrdIntrBkSttlmAmt.Value, pain001Amount(p.AmountCents))
	}
	if tx.RtrRsnInf.Rsn == nil || tx.RtrRsnInf.Rsn.Prtry != Pacs004ReturnDuplicate {
		t.Errorf("RtrRsnInf/Rsn/Prtry = %+v, want %q", tx.RtrRsnInf.Rsn, Pacs004ReturnDuplicate)
	}
	// Repeat exports are byte-stable apart from CreDtTm: the MsgId is
	// deterministic on the payout, not unique per call.
	second, err := l.ExportPacs004(p.UETR, Pacs004ReturnDuplicate)
	if err != nil {
		t.Fatalf("ExportPacs004 repeat: %v", err)
	}
	if parsePacs004Back(t, second).Return.GrpHdr.MsgID != rtr.GrpHdr.MsgID {
		t.Error("repeat export carries a different MsgId")
	}
}

func TestExportPacs004_ReasonVocabulary(t *testing.T) {
	l := seedPayoutLedger(t)
	p := settlePayoutForReturn(t, l, "po-pacs004-2", 100_000)

	// Every admitted code renders.
	for _, code := range []string{
		Pacs004ReturnDuplicate,
		Pacs004ReturnWrongAmount,
		Pacs004ReturnInvalidAccount,
		Pacs004ReturnCustomerRequest,
	} {
		doc, err := l.ExportPacs004(p.UETR, code)
		if err != nil {
			t.Errorf("reason %q: ExportPacs004: %v", code, err)
			continue
		}
		if got := parsePacs004Back(t, doc).Return.TxInf.RtrRsnInf.Rsn.Prtry; got != code {
			t.Errorf("reason %q: Prtry = %q", code, got)
		}
	}
	// A reason is required: a return must say why.
	if _, err := l.ExportPacs004(p.UETR, ""); !errors.Is(err, ErrPacs004ReasonRequired) {
		t.Errorf("empty reason: err = %v, want ErrPacs004ReasonRequired", err)
	}
	if _, err := l.ExportPacs004(p.UETR, "   "); !errors.Is(err, ErrPacs004ReasonRequired) {
		t.Errorf("blank reason: err = %v, want ErrPacs004ReasonRequired", err)
	}
	// Unknown codes are refused, not coerced.
	for _, code := range []string{"FRAD", "ACCP", "dupl"} {
		if _, err := l.ExportPacs004(p.UETR, code); !errors.Is(err, ErrPacs004InvalidReason) {
			t.Errorf("reason %q: err = %v, want ErrPacs004InvalidReason", code, err)
		}
	}
}

func TestExportPacs004_UnknownUETR(t *testing.T) {
	l := seedPayoutLedger(t)
	if _, err := l.ExportPacs004("no-such-uetr", Pacs004ReturnDuplicate); !errors.Is(err, ErrPacs004UnknownUETR) {
		t.Errorf("err = %v, want ErrPacs004UnknownUETR", err)
	}
}

func TestExportPacs004_UnsettledRefused(t *testing.T) {
	l := seedPayoutLedger(t)

	// Draft: never left the ledger.
	draft := draftPayout(t, l, "po-pacs004-draft", 50_000)
	if _, err := l.ExportPacs004(draft.UETR, Pacs004ReturnCustomerRequest); !errors.Is(err, ErrPacs004NotSettled) {
		t.Errorf("draft: err = %v, want ErrPacs004NotSettled", err)
	}
	// Submitted: in flight, money has not settled — camt.056 is the
	// recall path, not pacs.004.
	sub := approveSubmitPayout(t, l, "po-pacs004-draft", "bob")
	if _, err := l.ExportPacs004(sub.UETR, Pacs004ReturnCustomerRequest); !errors.Is(err, ErrPacs004NotSettled) {
		t.Errorf("submitted: err = %v, want ErrPacs004NotSettled", err)
	}
	// Failed: the bank rejected it — there is nothing to return.
	draftPayout(t, l, "po-pacs004-fail", 50_000)
	approveSubmitPayout(t, l, "po-pacs004-fail", "bob")
	if _, err := l.FailPayout("po-pacs004-fail", "beneficiary bank rejected: account closed"); err != nil {
		t.Fatalf("FailPayout: %v", err)
	}
	failed, _ := l.GetPayout("po-pacs004-fail")
	if _, err := l.ExportPacs004(failed.UETR, Pacs004ReturnCustomerRequest); !errors.Is(err, ErrPacs004NotSettled) {
		t.Errorf("failed: err = %v, want ErrPacs004NotSettled", err)
	}
}

// TestExportPacs004_ReadOnly is the contract test: generating a
// pacs.004 return must not bump the ledger version, must not append
// to the audit chain, and must not mutate the payout — the export is
// a statement of fact, not an operator attestation.
func TestExportPacs004_ReadOnly(t *testing.T) {
	al := mustAuditLog(t)
	defer al.Close()
	l := seedPayoutLedger(t, WithAuditLog(al))
	p := settlePayoutForReturn(t, l, "po-pacs004-ro", 175_000)

	// Flush the async audit writer so the settle events are counted
	// before the export runs.
	time.Sleep(500 * time.Millisecond)
	beforeVersion := l.version
	beforeWritten, _, _ := l.AuditStats()
	beforePayout, ok := l.GetPayout(p.ID)
	if !ok {
		t.Fatal("settled payout vanished before export")
	}

	doc, err := l.ExportPacs004(p.UETR, Pacs004ReturnWrongAmount)
	if err != nil {
		t.Fatalf("ExportPacs004: %v", err)
	}
	if len(doc) == 0 {
		t.Fatal("empty export document")
	}

	// Flush again: any event the export emitted would land now.
	time.Sleep(500 * time.Millisecond)
	if l.version != beforeVersion {
		t.Errorf("export bumped version: %d -> %d", beforeVersion, l.version)
	}
	if afterWritten, _, _ := l.AuditStats(); afterWritten != beforeWritten {
		t.Errorf("export appended %d audit event(s): %d -> %d", afterWritten-beforeWritten, beforeWritten, afterWritten)
	}
	afterPayout, ok := l.GetPayout(p.ID)
	if !ok {
		t.Fatal("settled payout vanished after export")
	}
	if !reflect.DeepEqual(beforePayout, afterPayout) {
		t.Error("export mutated the payout instruction")
	}
}

func TestExportPacs004_XMLHonest(t *testing.T) {
	l := seedPayoutLedger(t)
	p := settlePayoutForReturn(t, l, "po-pacs004-xml", 42_50)

	doc, err := l.ExportPacs004(p.UETR, Pacs004ReturnInvalidAccount)
	if err != nil {
		t.Fatalf("ExportPacs004: %v", err)
	}
	raw := string(doc)
	for _, want := range []string{
		`urn:iso:std:iso:20022:tech:xsd:pacs.004.001.02`,
		"<PmtRtr>",
		"<RtrdIntrBkSttlmAmt Ccy=\"USD\">42.50</RtrdIntrBkSttlmAmt>",
		"<OrgnlUETR>" + p.UETR + "</OrgnlUETR>",
		"<Prtry>AC03</Prtry>",
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("export missing %q", want)
		}
	}
}
