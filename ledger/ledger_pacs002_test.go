package ledger

import (
	"encoding/xml"
	"errors"
	"testing"
)

// parsePacs002Back unmarshals an exported pacs.002 document into the
// wire structs: the honest round-trip check that the XML the ledger
// emits is well-formed and carries the fields the export claims.
func parsePacs002Back(t *testing.T, doc []byte) pacs002Document {
	t.Helper()
	var back pacs002Document
	if err := xml.Unmarshal(doc, &back); err != nil {
		t.Fatalf("export is not parseable pacs.002 XML: %v", err)
	}
	return back
}

func TestExportPacs002_TrackedReportsACSP(t *testing.T) {
	l := seedPayoutLedger(t)
	p := draftPayout(t, l, "po-pacs002-1", 250_000)

	doc, err := l.ExportPacs002(p.UETR)
	if err != nil {
		t.Fatalf("ExportPacs002: %v", err)
	}
	back := parsePacs002Back(t, doc)
	rpt := back.Report
	if rpt.GrpHdr.MsgID != pacs002MsgID(p.UETR, 0) {
		t.Errorf("MsgId = %q, want %q", rpt.GrpHdr.MsgID, pacs002MsgID(p.UETR, 0))
	}
	if rpt.OrgnlGrpInf.OrgnlMsgID != p.ID {
		t.Errorf("OrgnlMsgId = %q, want payout ID %q", rpt.OrgnlGrpInf.OrgnlMsgID, p.ID)
	}
	if rpt.OrgnlGrpInf.OrgnlMsgNmID != pacs002OrgnlMsgName {
		t.Errorf("OrgnlMsgNmId = %q, want %q", rpt.OrgnlGrpInf.OrgnlMsgNmID, pacs002OrgnlMsgName)
	}
	if rpt.Tx.TxSts != "ACSP" {
		t.Errorf("TxSts = %q, want ACSP for tracked", rpt.Tx.TxSts)
	}
	if rpt.Tx.OrgnlUETR != p.UETR {
		t.Errorf("OrgnlUETR = %q, want %q", rpt.Tx.OrgnlUETR, p.UETR)
	}
	if rpt.Tx.OrgnlEndToEndID != p.ID {
		t.Errorf("OrgnlEndToEndId = %q, want payout ID %q (no transfer linked)", rpt.Tx.OrgnlEndToEndID, p.ID)
	}
	if rpt.Tx.StsRsnInf != nil {
		t.Errorf("StsRsnInf present for non-RJCT report")
	}
}

func TestExportPacs002_StatusProgression(t *testing.T) {
	l := seedPayoutLedger(t)
	p := draftPayout(t, l, "po-pacs002-2", 250_000)

	// tracked -> ACSP -> ACSC -> ACCP: each advance changes the TxSts
	// and the deterministic MsgId.
	cases := []struct{ status, txSts string }{
		{GPIStatusACSP, "ACSP"},
		{GPIStatusACSC, "ACSC"},
		{GPIStatusACCP, "ACCP"},
	}
	for i, c := range cases {
		if _, err := l.AdvanceGPI(p.UETR, c.status, "", "night-ops"); err != nil {
			t.Fatalf("AdvanceGPI to %s: %v", c.status, err)
		}
		doc, err := l.ExportPacs002(p.UETR)
		if err != nil {
			t.Fatalf("ExportPacs002: %v", err)
		}
		back := parsePacs002Back(t, doc)
		if back.Report.Tx.TxSts != c.txSts {
			t.Errorf("after %s: TxSts = %q, want %q", c.status, back.Report.Tx.TxSts, c.txSts)
		}
		wantMsgID := pacs002MsgID(p.UETR, i+1)
		if back.Report.GrpHdr.MsgID != wantMsgID {
			t.Errorf("after %s: MsgId = %q, want %q", c.status, back.Report.GrpHdr.MsgID, wantMsgID)
		}
	}
}

func TestExportPacs002_RJCTCarriesReason(t *testing.T) {
	l := seedPayoutLedger(t)
	p := draftPayout(t, l, "po-pacs002-3", 250_000)
	if _, err := l.AdvanceGPI(p.UETR, GPIStatusRJCT, "AC03", "night-ops"); err != nil {
		t.Fatalf("AdvanceGPI to RJCT: %v", err)
	}
	doc, err := l.ExportPacs002(p.UETR)
	if err != nil {
		t.Fatalf("ExportPacs002: %v", err)
	}
	back := parsePacs002Back(t, doc)
	if back.Report.Tx.TxSts != "RJCT" {
		t.Fatalf("TxSts = %q, want RJCT", back.Report.Tx.TxSts)
	}
	if back.Report.Tx.StsRsnInf == nil || back.Report.Tx.StsRsnInf.Rsn == nil {
		t.Fatal("RJCT report has no StsRsnInf/Rsn")
	}
	if back.Report.Tx.StsRsnInf.Rsn.Prtry != "AC03" {
		t.Errorf("reason = %q, want AC03", back.Report.Tx.StsRsnInf.Rsn.Prtry)
	}
}

func TestExportPacs002_MsgIDDeterministic(t *testing.T) {
	l := seedPayoutLedger(t)
	p := draftPayout(t, l, "po-pacs002-4", 250_000)

	first, err := l.ExportPacs002(p.UETR)
	if err != nil {
		t.Fatalf("ExportPacs002: %v", err)
	}
	second, err := l.ExportPacs002(p.UETR)
	if err != nil {
		t.Fatalf("ExportPacs002: %v", err)
	}
	if parsePacs002Back(t, first).Report.GrpHdr.MsgID != parsePacs002Back(t, second).Report.GrpHdr.MsgID {
		t.Error("repeat exports of the same track state carry different MsgIds")
	}
}

func TestExportPacs002_UnknownUETR(t *testing.T) {
	l := seedPayoutLedger(t)
	if _, err := l.ExportPacs002("no-such-uetr"); !errors.Is(err, ErrPacs002UnknownUETR) {
		t.Errorf("err = %v, want ErrPacs002UnknownUETR", err)
	}
}

func TestExportPacs002_LinkedTransferEndToEnd(t *testing.T) {
	l := seedPayoutLedger(t)
	p := draftPayout(t, l, "po-pacs002-5", 250_000)
	if _, err := l.LinkGPITransfer(p.UETR, "tr-pacs002-5", "night-ops"); err != nil {
		t.Fatalf("LinkGPITransfer: %v", err)
	}
	doc, err := l.ExportPacs002(p.UETR)
	if err != nil {
		t.Fatalf("ExportPacs002: %v", err)
	}
	if got := parsePacs002Back(t, doc).Report.Tx.OrgnlEndToEndID; got != "tr-pacs002-5" {
		t.Errorf("OrgnlEndToEndId = %q, want linked transfer ID", got)
	}
}

func TestExportPacs002_ReadOnly(t *testing.T) {
	l := seedPayoutLedger(t)
	p := draftPayout(t, l, "po-pacs002-6", 250_000)
	beforeVersion := l.Version()
	beforeHead, beforeLinks := l.ChainHead()

	if _, err := l.ExportPacs002(p.UETR); err != nil {
		t.Fatalf("ExportPacs002: %v", err)
	}
	if got := l.Version(); got != beforeVersion {
		t.Errorf("version = %d after export, want unchanged %d", got, beforeVersion)
	}
	if head, links := l.ChainHead(); head != beforeHead || links != beforeLinks {
		t.Errorf("chain head moved after export: (%q,%d) vs (%q,%d)", head, links, beforeHead, beforeLinks)
	}
}
