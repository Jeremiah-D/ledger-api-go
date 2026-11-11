package ledger

import (
	"bytes"
	"encoding/xml"
	"errors"
	"strings"
	"testing"
	"time"
)

func parseCamt055Back(t *testing.T, doc []byte) camt055Document {
	t.Helper()
	var back camt055Document
	if err := xml.Unmarshal(doc, &back); err != nil {
		t.Fatalf("export is not parseable camt.055 XML: %v", err)
	}
	return back
}

func TestExportCAMT055_HappyPath(t *testing.T) {
	l := seedPayoutLedger(t)
	p := submittedPayout(t, l, "po-cxl55-1")
	c, err := l.RequestPayoutCancellation(p.UETR, "DUPL", "night-ops")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	doc, err := l.ExportCAMT055(p.UETR)
	if err != nil {
		t.Fatalf("ExportCAMT055: %v", err)
	}
	if !bytes.HasPrefix(doc, []byte(xml.Header)) {
		t.Errorf("export does not start with the XML header")
	}
	back := parseCamt055Back(t, doc)
	req := back.Request
	if req.Assgnmt.ID != c.MsgID {
		t.Errorf("Assgnmt/Id = %q, want %q", req.Assgnmt.ID, c.MsgID)
	}
	// No BIC registered for saver: assignor falls back to the account ID.
	if req.Assgnmt.Assgnr.Nm != "saver" {
		t.Errorf("Assgnr = %q, want saver (no BIC registered)", req.Assgnmt.Assgnr.Nm)
	}
	if req.Assgnmt.Assgne.Nm != p.Destination {
		t.Errorf("Assgne = %q, want destination %q", req.Assgnmt.Assgne.Nm, p.Destination)
	}
	if req.Assgnmt.CreDtTm != c.RequestedAt.UTC().Format(time.RFC3339) {
		t.Errorf("CreDtTm = %q, want %q", req.Assgnmt.CreDtTm, c.RequestedAt.UTC().Format(time.RFC3339))
	}
	tx := req.Undrlyg.TxInf
	if tx.CxlID != c.MsgID {
		t.Errorf("CxlId = %q, want %q", tx.CxlID, c.MsgID)
	}
	if tx.OrgnlGrpInf.OrgnlMsgID != p.ID {
		t.Errorf("OrgnlMsgId = %q, want %q", tx.OrgnlGrpInf.OrgnlMsgID, p.ID)
	}
	if tx.OrgnlGrpInf.OrgnlMsgNmID != pacs002OrgnlMsgName {
		t.Errorf("OrgnlMsgNmId = %q, want %q", tx.OrgnlGrpInf.OrgnlMsgNmID, pacs002OrgnlMsgName)
	}
	if tx.OrgnlInstrID != p.ID {
		t.Errorf("OrgnlInstrId = %q, want %q", tx.OrgnlInstrID, p.ID)
	}
	if tx.OrgnlUETR != p.UETR {
		t.Errorf("OrgnlUETR = %q, want %q", tx.OrgnlUETR, p.UETR)
	}
	if tx.CxlRsnInf.Rsn == nil || tx.CxlRsnInf.Rsn.Prtry != "DUPL" {
		t.Errorf("CxlRsnInf = %+v, want Prtry DUPL", tx.CxlRsnInf.Rsn)
	}
	// Omitted by construction: no Case, no CtrlData, no
	// OrgnlEndToEndId — the ledger holds none of these.
	for _, tag := range []string{"Case", "CtrlData", "OrgnlEndToEndId"} {
		if strings.Contains(string(doc), "<"+tag+">") || strings.Contains(string(doc), "<"+tag+" ") {
			t.Errorf("export must omit <%s>, found it in the document", tag)
		}
	}
}

func TestExportCAMT055_AssignorBIC(t *testing.T) {
	l := seedPayoutLedger(t)
	if err := l.SetAccountIBAN("saver", "DE89370400440532013000", "DEUTDEFF"); err != nil {
		t.Fatalf("SetAccountIBAN: %v", err)
	}
	p := submittedPayout(t, l, "po-cxl55-2")
	if _, err := l.RequestPayoutCancellation(p.UETR, "DUPL", "night-ops"); err != nil {
		t.Fatalf("request: %v", err)
	}
	doc, err := l.ExportCAMT055(p.UETR)
	if err != nil {
		t.Fatalf("ExportCAMT055: %v", err)
	}
	if got := parseCamt055Back(t, doc).Request.Assgnmt.Assgnr.Nm; got != "DEUTDEFF" {
		t.Errorf("Assgnr = %q, want registered BIC DEUTDEFF", got)
	}
}

func TestExportCAMT055_NoOpen(t *testing.T) {
	l := seedPayoutLedger(t)
	p := submittedPayout(t, l, "po-cxl55-3")
	if _, err := l.ExportCAMT055(p.UETR); !errors.Is(err, ErrNoOpenCancellation) {
		t.Errorf("no cancellation yet: err = %v, want ErrNoOpenCancellation", err)
	}
	if _, err := l.ExportCAMT055("no-such-uetr"); !errors.Is(err, ErrNoOpenCancellation) {
		t.Errorf("unknown UETR: err = %v, want ErrNoOpenCancellation", err)
	}
	// A resolved cancellation is no longer open either.
	if _, err := l.RequestPayoutCancellation(p.UETR, "DUPL", "night-ops"); err != nil {
		t.Fatalf("request: %v", err)
	}
	if _, err := l.ResolvePayoutCancellation(p.UETR, CancellationRejected, "RJCT", "night-ops"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := l.ExportCAMT055(p.UETR); !errors.Is(err, ErrNoOpenCancellation) {
		t.Errorf("resolved cancellation: err = %v, want ErrNoOpenCancellation", err)
	}
}

func TestExportCAMT055_ReadOnlyAndDeterministic(t *testing.T) {
	l := seedPayoutLedger(t)
	p := submittedPayout(t, l, "po-cxl55-4")
	if _, err := l.RequestPayoutCancellation(p.UETR, "DUPL", "night-ops"); err != nil {
		t.Fatalf("request: %v", err)
	}
	before := l.version
	first, err := l.ExportCAMT055(p.UETR)
	if err != nil {
		t.Fatalf("first ExportCAMT055: %v", err)
	}
	second, err := l.ExportCAMT055(p.UETR)
	if err != nil {
		t.Fatalf("second ExportCAMT055: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("repeat export differs:\n%s\n---\n%s", first, second)
	}
	if l.version != before {
		t.Errorf("version = %d, want %d (export must be read-only)", l.version, before)
	}
	if rec, ok := l.GetPayoutCancellation(p.UETR); !ok || rec.Status != CancellationRequested {
		t.Errorf("export mutated the record: %+v", rec)
	}
}
