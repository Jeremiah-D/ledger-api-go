package ledger

import (
	"bytes"
	"encoding/xml"
	"errors"
	"strings"
	"testing"
)

// parsePacs028Back unmarshals an exported pacs.028 document into the
// wire structs: the honest round-trip check that the XML the ledger
// emits is well-formed and carries the fields the export claims.
func parsePacs028Back(t *testing.T, doc []byte) pacs028Document {
	t.Helper()
	var back pacs028Document
	if err := xml.Unmarshal(doc, &back); err != nil {
		t.Fatalf("export is not parseable pacs.028 XML: %v", err)
	}
	return back
}

func TestExportPacs028_KnownUETR(t *testing.T) {
	l := seedPayoutLedger(t)
	p := draftPayout(t, l, "po-pacs028-1", 250_000)

	doc, err := l.ExportPacs028(p.UETR, "ledger-ops")
	if err != nil {
		t.Fatalf("ExportPacs028: %v", err)
	}
	back := parsePacs028Back(t, doc)
	req := back.Request
	if req.GrpHdr.MsgID != pacs028MsgID(p.UETR) {
		t.Errorf("GrpHdr MsgId = %q, want %q", req.GrpHdr.MsgID, pacs028MsgID(p.UETR))
	}
	if req.GrpHdr.CreDtTm == "" {
		t.Error("GrpHdr CreDtTm is empty")
	}
	if req.GrpHdr.Reqstr.ID != "ledger-ops" {
		t.Errorf("GrpHdr Reqstr Id = %q, want ledger-ops", req.GrpHdr.Reqstr.ID)
	}
	if req.OrgnlGrpInf.OrgnlMsgID != p.ID {
		t.Errorf("OrgnlMsgId = %q, want payout ID %q", req.OrgnlGrpInf.OrgnlMsgID, p.ID)
	}
	if req.OrgnlGrpInf.OrgnlMsgNmID != pacs028OrgnlMsgName {
		t.Errorf("OrgnlMsgNmId = %q, want %q", req.OrgnlGrpInf.OrgnlMsgNmID, pacs028OrgnlMsgName)
	}
	if req.OrgnlGrpInf.OrgnlUETR != p.UETR {
		t.Errorf("OrgnlUETR = %q, want %q", req.OrgnlGrpInf.OrgnlUETR, p.UETR)
	}
	if req.OrgnlGrpInf.Reqstr.ID != "ledger-ops" {
		t.Errorf("OrgnlGrpInf Reqstr Id = %q, want ledger-ops", req.OrgnlGrpInf.Reqstr.ID)
	}
	if !strings.Contains(string(doc), "FIToFIPmtStsReq") {
		t.Error("document missing FIToFIPmtStsReq root")
	}
}

func TestExportPacs028_Deterministic(t *testing.T) {
	l := seedPayoutLedger(t)
	p := draftPayout(t, l, "po-pacs028-2", 250_000)

	first, err := l.ExportPacs028(p.UETR, "ledger-ops")
	if err != nil {
		t.Fatalf("ExportPacs028: %v", err)
	}
	second, err := l.ExportPacs028(p.UETR, "ledger-ops")
	if err != nil {
		t.Fatalf("ExportPacs028: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("repeat exports differ:\n%s\nvs\n%s", first, second)
	}
}

func TestExportPacs028_UnknownUETR(t *testing.T) {
	l := seedPayoutLedger(t)
	if _, err := l.ExportPacs028("no-such-uetr", "ledger-ops"); !errors.Is(err, ErrPacs028UnknownUETR) {
		t.Errorf("err = %v, want ErrPacs028UnknownUETR", err)
	}
}

func TestExportPacs028_RequesterRequired(t *testing.T) {
	l := seedPayoutLedger(t)
	p := draftPayout(t, l, "po-pacs028-3", 250_000)
	for _, requester := range []string{"", "   "} {
		if _, err := l.ExportPacs028(p.UETR, requester); !errors.Is(err, ErrPacs028RequesterRequired) {
			t.Errorf("requester %q: err = %v, want ErrPacs028RequesterRequired", requester, err)
		}
	}
}

func TestExportPacs028_ReadOnly(t *testing.T) {
	l := seedPayoutLedger(t)
	p := draftPayout(t, l, "po-pacs028-4", 250_000)
	beforeVersion := l.Version()
	beforeHead, beforeLinks := l.ChainHead()

	if _, err := l.ExportPacs028(p.UETR, "ledger-ops"); err != nil {
		t.Fatalf("ExportPacs028: %v", err)
	}
	if got := l.Version(); got != beforeVersion {
		t.Errorf("version = %d after export, want unchanged %d", got, beforeVersion)
	}
	if head, links := l.ChainHead(); head != beforeHead || links != beforeLinks {
		t.Errorf("chain head moved after export: (%q,%d) vs (%q,%d)", head, links, beforeHead, beforeLinks)
	}
}
