package ledger

import (
	"encoding/xml"
	"errors"
	"strings"
	"testing"
)

// parsePacs007Back unmarshals an exported pacs.007 document into the
// wire structs: the honest round-trip check that the XML the ledger
// emits is well-formed and carries the fields the export claims.
func parsePacs007Back(t *testing.T, doc []byte) pacs007Document {
	t.Helper()
	var back pacs007Document
	if err := xml.Unmarshal(doc, &back); err != nil {
		t.Fatalf("export is not parseable pacs.007 XML: %v", err)
	}
	return back
}

func TestExportPacs007_SettledHappyPath(t *testing.T) {
	l := seedPayoutLedger(t)
	p := settlePayoutForReturn(t, l, "po-pacs007-1", 250_000)

	doc, err := l.ExportPacs007(p.UETR, Pacs007ReversalFraudulent)
	if err != nil {
		t.Fatalf("ExportPacs007: %v", err)
	}
	back := parsePacs007Back(t, doc)
	rvsl := back.Reversal
	if rvsl.GrpHdr.MsgID != pacs007MsgID(p.UETR) {
		t.Errorf("MsgId = %q, want %q", rvsl.GrpHdr.MsgID, pacs007MsgID(p.UETR))
	}
	if rvsl.OrgnlGrpInf.OrgnlMsgID != p.ID {
		t.Errorf("OrgnlMsgId = %q, want payout ID %q", rvsl.OrgnlGrpInf.OrgnlMsgID, p.ID)
	}
	if rvsl.OrgnlGrpInf.OrgnlMsgNmID != pacs002OrgnlMsgName {
		t.Errorf("OrgnlMsgNmId = %q, want %q", rvsl.OrgnlGrpInf.OrgnlMsgNmID, pacs002OrgnlMsgName)
	}
	tx := rvsl.TxInf
	if tx.RvslID != pacs007MsgID(p.UETR) {
		t.Errorf("RvslId = %q, want %q", tx.RvslID, pacs007MsgID(p.UETR))
	}
	if tx.OrgnlUETR != p.UETR {
		t.Errorf("OrgnlUETR = %q, want %q", tx.OrgnlUETR, p.UETR)
	}
	if tx.OrgnlEndToEndID != p.ID {
		t.Errorf("OrgnlEndToEndId = %q, want payout ID %q", tx.OrgnlEndToEndID, p.ID)
	}
	if tx.RvsdAmt.Ccy != p.Currency || tx.RvsdAmt.Value != pain001Amount(p.AmountCents) {
		t.Errorf("RvsdIntrBkSttlmAmt = %s %q, want %s %q", tx.RvsdAmt.Ccy, tx.RvsdAmt.Value, p.Currency, pain001Amount(p.AmountCents))
	}
	if tx.RtrRsnInf.Rsn == nil || tx.RtrRsnInf.Rsn.Prtry != Pacs007ReversalFraudulent {
		t.Errorf("RtrRsnInf/Rsn/Prtry = %+v, want %q", tx.RtrRsnInf.Rsn, Pacs007ReversalFraudulent)
	}
	// OrgnlTxRef references the original instruction by amount and
	// end-to-end ID only — no party data rides a reversal report.
	if tx.OrgnlTxRef.OrgnlAmt.Ccy != p.Currency || tx.OrgnlTxRef.OrgnlAmt.Value != pain001Amount(p.AmountCents) {
		t.Errorf("OrgnlTxRef amount = %s %q, want %s %q", tx.OrgnlTxRef.OrgnlAmt.Ccy, tx.OrgnlTxRef.OrgnlAmt.Value, p.Currency, pain001Amount(p.AmountCents))
	}
	if tx.OrgnlTxRef.OrgnlEndToEndID != p.ID {
		t.Errorf("OrgnlTxRef OrgnlEndToEndId = %q, want %q", tx.OrgnlTxRef.OrgnlEndToEndID, p.ID)
	}
	// Repeat exports are byte-stable apart from CreDtTm: the MsgId
	// is deterministic on the payout, not unique per call.
	second, err := l.ExportPacs007(p.UETR, Pacs007ReversalFraudulent)
	if err != nil {
		t.Fatalf("ExportPacs007 repeat: %v", err)
	}
	if parsePacs007Back(t, second).Reversal.GrpHdr.MsgID != rvsl.GrpHdr.MsgID {
		t.Error("repeat export carries a different MsgId")
	}
}

func TestExportPacs007_ReasonVocabulary(t *testing.T) {
	l := seedPayoutLedger(t)
	p := settlePayoutForReturn(t, l, "po-pacs007-2", 100_000)

	for _, code := range []string{
		Pacs007ReversalDuplicate,
		Pacs007ReversalWrongAmount,
		Pacs007ReversalFraudulent,
		Pacs007ReversalCustomerRequest,
	} {
		if _, err := l.ExportPacs007(p.UETR, code); err != nil {
			t.Errorf("ExportPacs007 reason %q: %v", code, err)
		}
	}
	// AC03 is a pacs.004 return code, not a reversal code: the
	// vocabularies are closed and distinct.
	if _, err := l.ExportPacs007(p.UETR, "AC03"); !errors.Is(err, ErrPacs007InvalidReason) {
		t.Errorf("reason AC03: err = %v, want ErrPacs007InvalidReason", err)
	}
	if _, err := l.ExportPacs007(p.UETR, "  "); !errors.Is(err, ErrPacs007ReasonRequired) {
		t.Errorf("empty reason: err = %v, want ErrPacs007ReasonRequired", err)
	}
}

func TestExportPacs007_FailClosed(t *testing.T) {
	l := seedPayoutLedger(t)

	if _, err := l.ExportPacs007("no-such-uetr", Pacs007ReversalDuplicate); !errors.Is(err, ErrPacs007UnknownUETR) {
		t.Errorf("unknown UETR: err = %v, want ErrPacs007UnknownUETR", err)
	}

	// A drafted but unsettled payout cannot be reversed: the
	// camt.056 recall is the honest path before settlement.
	draftPayout(t, l, "po-pacs007-3", 50_000)
	p, ok := l.GetPayout("po-pacs007-3")
	if !ok {
		t.Fatal("drafted payout missing")
	}
	if _, err := l.ExportPacs007(p.UETR, Pacs007ReversalDuplicate); !errors.Is(err, ErrPacs007NotSettled) {
		t.Errorf("unsettled payout: err = %v, want ErrPacs007NotSettled", err)
	}
}

func TestExportPacs007_DistinctFromPacs004(t *testing.T) {
	// The reversal and the return of the same settled payout carry
	// different deterministic MsgIds and different roots: a reader
	// can never mistake one for the other.
	l := seedPayoutLedger(t)
	p := settlePayoutForReturn(t, l, "po-pacs007-4", 75_000)

	rvsl, err := l.ExportPacs007(p.UETR, Pacs007ReversalDuplicate)
	if err != nil {
		t.Fatalf("ExportPacs007: %v", err)
	}
	rtr, err := l.ExportPacs004(p.UETR, Pacs004ReturnDuplicate)
	if err != nil {
		t.Fatalf("ExportPacs004: %v", err)
	}
	if strings.Contains(string(rvsl), "PmtRtr") || strings.Contains(string(rtr), "FIToFIPmtRvsl") {
		t.Error("reversal and return documents share a root element")
	}
	if parsePacs007Back(t, rvsl).Reversal.GrpHdr.MsgID == parsePacs004Back(t, rtr).Return.GrpHdr.MsgID {
		t.Error("reversal and return share a MsgId")
	}
}
