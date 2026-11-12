package ledger

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
)

// stripCreDtTm removes the single wall-clock CreDtTm line so repeat
// exports can be compared byte-for-byte: the MsgId is the stable
// identifier, CreDtTm honestly records when each document was created
// (the camt.053 HTTP-test precedent).
var camt060CreDtTmLine = regexp.MustCompile(`(?m)^  <CreDtTm>[^<]*</CreDtTm>\n`)

func stripCAMT060CreDtTm(doc string) string {
	return camt060CreDtTmLine.ReplaceAllString(doc, "")
}

func TestCAMT060ExportGoldenDocument(t *testing.T) {
	l := painTestLedger(t)
	base := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	mustPostTransferPain(t, l, "pay-1", "alice", "bob", 15000, "USD", base)

	doc, err := l.ExportCAMT060(CAMT060ExportOptions{
		Account:  "alice",
		FromDate: base,
		ToDate:   base.Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(doc)
	for _, want := range []string{
		`<?xml version="1.0" encoding="UTF-8"?>`,
		`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.060.001.03">`,
		"<AcctRptgReq>",
		"<MsgId>camt060-alice-",
		"<CreDtTm>",
		"<RptgReq>",
		"<IBAN>" + testIBANDE + "</IBAN>",
		"<RptgPrd>",
		"<FrToDt>",
		"<FrDtTm>2026-10-20T09:00:00Z</FrDtTm>",
		"<ToDtTm>2026-10-21T09:00:00Z</ToDtTm>",
		"</Document>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("export missing %q, body:\n%s", want, s)
		}
	}
	// RptgReq/Id is the MsgId: the request and the message share one
	// deterministic identity.
	msgID := camt060MsgID("alice", base, base.Add(24*time.Hour))
	if !strings.Contains(s, "<Id>"+msgID+"</Id>") {
		t.Errorf("RptgReq/Id = %q missing, body:\n%s", msgID, s)
	}
	// No report-type invention: the request names the account and the
	// window, never a camt.052/053/054 preference the operator did
	// not state.
	for _, forbidden := range []string{"ReqdMsgNmId", "ReqTp", "camt.052", "camt.053", "camt.054"} {
		if strings.Contains(s, forbidden) {
			t.Errorf("export must not carry %q, body:\n%s", forbidden, s)
		}
	}
}

func TestCAMT060ExportNoWindowOmitsRptgPrd(t *testing.T) {
	l := painTestLedger(t)
	doc, err := l.ExportCAMT060(CAMT060ExportOptions{Account: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	s := string(doc)
	if strings.Contains(s, "<RptgPrd>") || strings.Contains(s, "FrToDt") {
		t.Errorf("windowless export must omit RptgPrd, body:\n%s", s)
	}
	if !strings.Contains(s, "<MsgId>camt060-alice-0-0</MsgId>") {
		t.Errorf("windowless MsgId must be camt060-alice-0-0, body:\n%s", s)
	}
}

func TestCAMT060ExportDeterministic(t *testing.T) {
	l := painTestLedger(t)
	base := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	opts := CAMT060ExportOptions{Account: "alice", FromDate: base, ToDate: base.Add(time.Hour)}
	first, err := l.ExportCAMT060(opts)
	if err != nil {
		t.Fatal(err)
	}
	second, err := l.ExportCAMT060(opts)
	if err != nil {
		t.Fatal(err)
	}
	// Byte-identical modulo the honest wall-clock CreDtTm line; the
	// MsgId is the stable identifier.
	if stripCAMT060CreDtTm(string(first)) != stripCAMT060CreDtTm(string(second)) {
		t.Errorf("repeat export differs (modulo CreDtTm):\n%s\nvs\n%s", first, second)
	}
	// A moved window changes the MsgId.
	moved, err := l.ExportCAMT060(CAMT060ExportOptions{Account: "alice", FromDate: base.Add(time.Hour), ToDate: base.Add(2 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(moved), "<MsgId>"+camt060MsgID("alice", base, base.Add(time.Hour))+"</MsgId>") {
		t.Error("moved window must change the MsgId")
	}
}

func TestCAMT060ExportUnknownAccount(t *testing.T) {
	l := painTestLedger(t)
	if _, err := l.ExportCAMT060(CAMT060ExportOptions{Account: "void"}); !errors.Is(err, ErrUnknownCAMT060Account) {
		t.Errorf("err = %v, want ErrUnknownCAMT060Account", err)
	}
}

func TestCAMT060ExportNoIBANRefused(t *testing.T) {
	l := New()
	base := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	// "ops" is known (it has journal entries) but has no registered
	// IBAN: the request must name the real IBAN, never the internal ID.
	mustPostOFX(t, l, "x1", "ops", "equity", 15000, "USD", base)
	if _, err := l.ExportCAMT060(CAMT060ExportOptions{Account: "ops"}); !errors.Is(err, ErrMissingAccountIBAN) {
		t.Errorf("err = %v, want ErrMissingAccountIBAN", err)
	}
}

func TestCAMT060ExportIBANOnlyAccountKnown(t *testing.T) {
	l := painTestLedger(t)
	// "carol" has a registered IBAN but no journal entries: the IBAN
	// registration alone makes the account known.
	doc, err := l.ExportCAMT060(CAMT060ExportOptions{Account: "carol"})
	if err != nil {
		t.Fatalf("IBAN-registered account must export: %v", err)
	}
	if !strings.Contains(string(doc), "<IBAN>"+testIBANFR+"</IBAN>") {
		t.Errorf("export names the wrong IBAN, body:\n%s", doc)
	}
}

func TestCAMT060ExportValidation(t *testing.T) {
	l := painTestLedger(t)
	base := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	if _, err := l.ExportCAMT060(CAMT060ExportOptions{}); !errors.Is(err, ErrEmptyAccountID) {
		t.Errorf("empty account: err = %v, want ErrEmptyAccountID", err)
	}
	if _, err := l.ExportCAMT060(CAMT060ExportOptions{Account: "alice", FromDate: base.Add(time.Hour), ToDate: base}); !errors.Is(err, ErrInvalidCAMT060Export) {
		t.Errorf("inverted window: err = %v, want ErrInvalidCAMT060Export", err)
	}
	// A lone `until` cannot be expressed: FrToDt requires a start.
	if _, err := l.ExportCAMT060(CAMT060ExportOptions{Account: "alice", ToDate: base}); !errors.Is(err, ErrInvalidCAMT060Export) {
		t.Errorf("lone until: err = %v, want ErrInvalidCAMT060Export", err)
	}
	// The renderer enforces the same gates directly.
	if _, err := RenderCAMT060Request("alice", testIBANDE, "m", base.Add(time.Hour), base); !errors.Is(err, ErrInvalidCAMT060Export) {
		t.Errorf("renderer inverted window: err = %v, want ErrInvalidCAMT060Export", err)
	}
	if _, err := RenderCAMT060Request("", testIBANDE, "m", base, base); !errors.Is(err, ErrInvalidCAMT060Export) {
		t.Errorf("renderer empty account: err = %v, want ErrInvalidCAMT060Export", err)
	}
}

func TestCAMT060ExportReadOnly(t *testing.T) {
	l := painTestLedger(t)
	base := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	mustPostTransferPain(t, l, "pay-1", "alice", "bob", 15000, "USD", base)
	before := l.version
	entries := len(l.Entries())
	if _, err := l.ExportCAMT060(CAMT060ExportOptions{Account: "alice", FromDate: base}); err != nil {
		t.Fatal(err)
	}
	if l.version != before {
		t.Errorf("version = %d, want %d (export must be read-only)", l.version, before)
	}
	if len(l.Entries()) != entries {
		t.Errorf("entries = %d, want %d (export must book nothing)", len(l.Entries()), entries)
	}
}
