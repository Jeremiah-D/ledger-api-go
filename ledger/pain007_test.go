package ledger

import (
	"errors"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

// stripPain007CreDtTm removes the single wall-clock CreDtTm line so
// repeat exports can be compared byte-for-byte: the MsgId is the
// stable identifier, CreDtTm honestly records when each document was
// created (the camt.053 HTTP-test precedent).
var pain007CreDtTmLine = regexp.MustCompile(`(?m)^  <CreDtTm>[^<]*</CreDtTm>\n`)

func stripPain007CreDtTm(doc string) string {
	return pain007CreDtTmLine.ReplaceAllString(doc, "")
}

func TestPain007GoldenDocument(t *testing.T) {
	l := painTestLedger(t)
	base := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	mustPostTransferPain(t, l, "pay-1", "alice", "bob", 15000, "USD", base)

	doc, err := l.ExportPain007(Pain007ExportOptions{TransferID: "pay-1", ReasonCode: "CUST"})
	if err != nil {
		t.Fatal(err)
	}
	s := string(doc)
	for _, want := range []string{
		`<?xml version="1.0" encoding="UTF-8"?>`,
		`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pain.007.001.02">`,
		"<CstmrPmtCxlReq>",
		"<MsgId>pay-1/cxl</MsgId>",
		"<CreDtTm>",
		"<Nm>LEDGER</Nm>",
		"<OrgnlEndToEndId>pay-1</OrgnlEndToEndId>",
		`<InstdAmt Ccy="USD">150.00</InstdAmt>`,
		"<IBAN>" + testIBANDE + "</IBAN>",
		"<IBAN>" + testIBANGB + "</IBAN>",
		"<Prtry>CUST</Prtry>",
		"</Document>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("export missing %q, body:\n%s", want, s)
		}
	}
	// Never exported via pain.001: no original MsgId is invented.
	if strings.Contains(s, "OrgnlMsgId") || strings.Contains(s, "OrgnlPmtInfId") {
		t.Errorf("unexported transfer must not carry an original MsgId, body:\n%s", s)
	}
	// No reason given: the Rsn block is omitted, never defaulted.
	doc2, err := l.ExportPain007(Pain007ExportOptions{TransferID: "pay-1"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(doc2), "CxlStsRsnInf") || strings.Contains(string(doc2), "<Prtry>") {
		t.Errorf("reasonless export must omit CxlStsRsnInf, body:\n%s", doc2)
	}
}

func TestPain007OriginalMsgIDRecovered(t *testing.T) {
	l := painTestLedger(t)
	base := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	mustPostTransferPain(t, l, "pay-1", "alice", "bob", 15000, "USD", base)
	mustPostTransferPain(t, l, "pay-2", "alice", "carol", 25050, "USD", base.Add(time.Hour))

	// Export the original instruction first: the registry now holds
	// its deterministic MsgId.
	msgID := mustExportPain001MsgID(t, l, Pain001ExportOptions{Currency: "USD"})

	doc, err := l.ExportPain007(Pain007ExportOptions{TransferID: "pay-1"})
	if err != nil {
		t.Fatal(err)
	}
	s := string(doc)
	if !strings.Contains(s, "<OrgnlMsgId>"+msgID+"</OrgnlMsgId>") {
		t.Errorf("export missing OrgnlMsgId %q, body:\n%s", msgID, s)
	}
	if !strings.Contains(s, "<OrgnlMsgNmId>pain.001.001.03</OrgnlMsgNmId>") {
		t.Errorf("export missing OrgnlMsgNmId pain.001.001.03, body:\n%s", s)
	}
	// The original PmtInfId re-derives the pain.001 formula:
	// "<msgID>-<debtor>-<currency>".
	if !strings.Contains(s, "<OrgnlPmtInfId>"+msgID+"-alice-USD</OrgnlPmtInfId>") {
		t.Errorf("export missing derived OrgnlPmtInfId, body:\n%s", s)
	}
}

func TestPain007Deterministic(t *testing.T) {
	l := painTestLedger(t)
	base := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	mustPostTransferPain(t, l, "pay-1", "alice", "bob", 15000, "USD", base)

	first, err := l.ExportPain007(Pain007ExportOptions{TransferID: "pay-1", ReasonCode: "DUPL"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := l.ExportPain007(Pain007ExportOptions{TransferID: "pay-1", ReasonCode: "DUPL"})
	if err != nil {
		t.Fatal(err)
	}
	// Byte-identical modulo the honest wall-clock CreDtTm line; the
	// MsgId is the stable identifier.
	if stripPain007CreDtTm(string(first)) != stripPain007CreDtTm(string(second)) {
		t.Errorf("repeat export differs (modulo CreDtTm):\n%s\nvs\n%s", first, second)
	}
}

func TestPain007UnknownTransfer(t *testing.T) {
	l := painTestLedger(t)
	if _, err := l.ExportPain007(Pain007ExportOptions{TransferID: "no-such"}); !errors.Is(err, ErrUnknownPain007Transfer) {
		t.Errorf("err = %v, want ErrUnknownPain007Transfer", err)
	}
}

func TestPain007PendingReviewRefused(t *testing.T) {
	l := painTestLedger(t)
	if err := l.SetReviewThreshold("alice", 100000); err != nil {
		t.Fatalf("SetReviewThreshold: %v", err)
	}
	mustPostTransferPain(t, l, "frozen-1", "alice", "bob", 200000, "USD", time.Time{})
	if _, err := l.ExportPain007(Pain007ExportOptions{TransferID: "frozen-1"}); !errors.Is(err, ErrPain007PendingReview) {
		t.Errorf("err = %v, want ErrPain007PendingReview", err)
	}
	// Approval flips it effective: the cancellation request is now
	// issuable.
	if _, err := l.ApproveTransferReview("frozen-1", "op"); err != nil {
		t.Fatalf("ApproveTransferReview: %v", err)
	}
	if _, err := l.ExportPain007(Pain007ExportOptions{TransferID: "frozen-1"}); err != nil {
		t.Errorf("approved transfer must be cancellable: %v", err)
	}
}

func TestPain007AlreadyReversedRefused(t *testing.T) {
	l := painTestLedger(t)
	base := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	mustPostTransferPain(t, l, "pay-1", "alice", "bob", 15000, "USD", base)
	if _, _, err := l.ReverseEntry("pay-1", "rev-key-1"); err != nil {
		t.Fatalf("ReverseEntry: %v", err)
	}
	if _, err := l.ExportPain007(Pain007ExportOptions{TransferID: "pay-1"}); !errors.Is(err, ErrAlreadyReversed) {
		t.Errorf("err = %v, want ErrAlreadyReversed", err)
	}
}

func TestPain007MissingIBANFailClosed(t *testing.T) {
	l := New()
	base := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	// No IBANs registered at all: the request must name real IBANs,
	// never the internal account IDs.
	mustPostTransferPain(t, l, "pay-1", "dave", "erin", 15000, "USD", base)
	if _, err := l.ExportPain007(Pain007ExportOptions{TransferID: "pay-1"}); !errors.Is(err, ErrMissingAccountIBAN) {
		t.Errorf("err = %v, want ErrMissingAccountIBAN", err)
	}
}

func TestPain007Validation(t *testing.T) {
	l := painTestLedger(t)
	if _, err := l.ExportPain007(Pain007ExportOptions{}); !errors.Is(err, ErrInvalidPain007Export) {
		t.Errorf("empty transfer ID: err = %v, want ErrInvalidPain007Export", err)
	}
	if _, err := l.ExportPain007(Pain007ExportOptions{TransferID: "pay-1", ReasonCode: "NOPE"}); !errors.Is(err, ErrInvalidPain007Reason) {
		t.Errorf("bad reason: err = %v, want ErrInvalidPain007Reason", err)
	}
	// The admitted vocabulary is exactly the four verified codes.
	if got := pain007ReasonCodes(); !reflect.DeepEqual(got, []string{"AM09", "CUST", "DUPL", "FRAD"}) {
		t.Errorf("reason vocabulary = %v, want [AM09 CUST DUPL FRAD]", got)
	}
	for _, code := range []string{"DUPL", "AM09", "FRAD", "CUST"} {
		if !pain007ValidReason(code) {
			t.Errorf("valid reason %q rejected", code)
		}
	}
}

func TestPain007ReadOnly(t *testing.T) {
	l := painTestLedger(t)
	base := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	mustPostTransferPain(t, l, "pay-1", "alice", "bob", 15000, "USD", base)
	before := l.version
	entries := len(l.Entries())
	if _, err := l.ExportPain007(Pain007ExportOptions{TransferID: "pay-1"}); err != nil {
		t.Fatal(err)
	}
	if l.version != before {
		t.Errorf("version = %d, want %d (export must be read-only)", l.version, before)
	}
	if len(l.Entries()) != entries {
		t.Errorf("entries = %d, want %d (export must book nothing)", len(l.Entries()), entries)
	}
}
