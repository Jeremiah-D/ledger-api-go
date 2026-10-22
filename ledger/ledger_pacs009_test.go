package ledger

import (
	"encoding/xml"
	"errors"
	"strings"
	"testing"
)

// postPacs009Transfer posts alice -> bob 2500 USD and returns the
// transfer ID, the test-ledger fixture for the pacs.009 tests.
func postPacs009Transfer(t *testing.T, l *Ledger) string {
	t.Helper()
	seedBalance(t, l, "alice", 10000)
	if _, err := l.PostTransfer(Transfer{
		ID:          "p9-tx-1",
		From:        "alice",
		To:          "bob",
		AmountCents: 2500,
	}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	return "p9-tx-1"
}

func TestExportPacs009HappyPath(t *testing.T) {
	l := New()
	id := postPacs009Transfer(t, l)

	doc, err := l.ExportPacs009(id, Pacs009Options{})
	if err != nil {
		t.Fatalf("ExportPacs009: %v", err)
	}
	body := string(doc)
	for _, want := range []string{
		`xmlns="urn:iso:std:iso:20022:tech:xsd:pacs.009.001.06"`,
		"<FICdtTrf>",
		"<MsgId>p9-tx-1/ficdt</MsgId>",
		"<NbOfTxs>1</NbOfTxs>",
		"<SttlmMtd>INDA</SttlmMtd>",
		"<InstrId>p9-tx-1</InstrId>",
		"<EndToEndId>p9-tx-1</EndToEndId>",
		"<TxId>p9-tx-1</TxId>",
		`<IntrBkSttlmAmt Ccy="USD">25.00</IntrBkSttlmAmt>`,
		"<IntrBkSttlmDt>",
		"<DbtrAgt>",
		"<CdtrAgt>",
		"<ChrgsBr>SLEV</ChrgsBr>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("export missing %q:\n%s", want, body)
		}
	}
	// The debtor side rides the payer account, the creditor side the
	// payee account — three FinInstnId blocks each (MarshalIndent
	// breaks Othr/Id onto separate lines, so count the Id elements).
	if n := strings.Count(body, "<Id>alice</Id>"); n != 3 {
		t.Errorf("payer agent mentions = %d, want 3 (InstgAgt/Dbtr/DbtrAgt)", n)
	}
	if n := strings.Count(body, "<Id>bob</Id>"); n != 3 {
		t.Errorf("payee agent mentions = %d, want 3 (InstdAgt/CdtrAgt/Cdtr)", n)
	}
	// The document must be well-formed XML under its own namespace.
	var parsed pacs009Document
	if err := xml.Unmarshal(doc, &parsed); err != nil {
		t.Fatalf("export is not well-formed XML: %v", err)
	}
	if parsed.Trf.GrpHdr.MsgID != "p9-tx-1/ficdt" {
		t.Errorf("parsed MsgId = %q, want p9-tx-1/ficdt", parsed.Trf.GrpHdr.MsgID)
	}
	if parsed.Trf.TxInf.IntrBkSttlmAmt.Value != "25.00" || parsed.Trf.TxInf.IntrBkSttlmAmt.Ccy != "USD" {
		t.Errorf("parsed amount = %+v, want USD 25.00", parsed.Trf.TxInf.IntrBkSttlmAmt)
	}
}

func TestExportPacs009Options(t *testing.T) {
	l := New()
	id := postPacs009Transfer(t, l)

	doc, err := l.ExportPacs009(id, Pacs009Options{
		SettlementMethod: "COVE",
		ChargesBearer:    "DEBT",
	})
	if err != nil {
		t.Fatalf("ExportPacs009: %v", err)
	}
	body := string(doc)
	if !strings.Contains(body, "<SttlmMtd>COVE</SttlmMtd>") {
		t.Errorf("export missing <SttlmMtd>COVE</SttlmMtd>:\n%s", body)
	}
	if !strings.Contains(body, "<ChrgsBr>DEBT</ChrgsBr>") {
		t.Errorf("export missing <ChrgsBr>DEBT</ChrgsBr>:\n%s", body)
	}
	if strings.Contains(body, "<SttlmMtd>INDA</SttlmMtd>") || strings.Contains(body, "<ChrgsBr>SLEV</ChrgsBr>") {
		t.Errorf("export leaked defaults:\n%s", body)
	}
}

func TestExportPacs009FailClosed(t *testing.T) {
	l := New()
	id := postPacs009Transfer(t, l)

	if _, err := l.ExportPacs009("no-such-transfer", Pacs009Options{}); !errors.Is(err, ErrTransferNotFound) {
		t.Errorf("unknown transfer: err=%v, want ErrTransferNotFound", err)
	}
	if _, err := l.ExportPacs009(id, Pacs009Options{SettlementMethod: "WIRE"}); !errors.Is(err, ErrPacs009InvalidSettlementMethod) {
		t.Errorf("bad settlement method: err=%v, want ErrPacs009InvalidSettlementMethod", err)
	}
	if _, err := l.ExportPacs009(id, Pacs009Options{ChargesBearer: "NOBODY"}); !errors.Is(err, ErrPacs009InvalidChargesBearer) {
		t.Errorf("bad charges bearer: err=%v, want ErrPacs009InvalidChargesBearer", err)
	}
	// A cross-currency transfer's principal entry books only the
	// payer-side leg against the FX clearing account — rendering it as
	// a single FI-to-FI credit transfer would misstate the money
	// movement, so it is refused.
	fx := New(WithFXAccount("fx-clearing"))
	mustSetRate(t, fx, "USD", "EUR", 108, 100)
	seedBalance(t, fx, "alice", 10000)
	if _, err := fx.PostTransfer(Transfer{
		ID:          "p9-fx-1",
		From:        "alice",
		To:          "bob",
		AmountCents: 2500,
		Currency:    "USD",
		ToCurrency:  "EUR",
	}); err != nil {
		t.Fatalf("PostTransfer FX: %v", err)
	}
	if _, err := fx.ExportPacs009("p9-fx-1", Pacs009Options{}); !errors.Is(err, ErrPacs009FXTransfer) {
		t.Errorf("FX transfer: err=%v, want ErrPacs009FXTransfer", err)
	}
}

func TestExportPacs009DeterministicAndReadOnly(t *testing.T) {
	l := New()
	id := postPacs009Transfer(t, l)
	_, versionBefore := l.Snapshot("nobody")
	entriesBefore := len(l.Entries())

	first, err := l.ExportPacs009(id, Pacs009Options{})
	if err != nil {
		t.Fatalf("first ExportPacs009: %v", err)
	}
	second, err := l.ExportPacs009(id, Pacs009Options{})
	if err != nil {
		t.Fatalf("second ExportPacs009: %v", err)
	}
	// Same MsgId, stable across identical exports.
	if !strings.Contains(string(first), "<MsgId>p9-tx-1/ficdt</MsgId>") ||
		!strings.Contains(string(second), "<MsgId>p9-tx-1/ficdt</MsgId>") {
		t.Error("MsgId not stable across exports")
	}
	// Read-only: no version bump, no journal rows, intact chain.
	if _, v := l.Snapshot("nobody"); v != versionBefore {
		t.Errorf("version = %d, want unchanged %d", v, versionBefore)
	}
	if n := len(l.Entries()); n != entriesBefore {
		t.Errorf("journal entries = %d, want unchanged %d", n, entriesBefore)
	}
	if err := l.VerifyChain(); err != nil {
		t.Errorf("VerifyChain after export: %v", err)
	}
}
