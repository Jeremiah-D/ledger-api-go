package ledger

import (
	"encoding/xml"
	"errors"
	"strings"
	"testing"
)

// postPacs003Transfer posts alice -> bob 2500 USD and returns the
// transfer ID, the test-ledger fixture for the pacs.003 tests. The
// debtor is alice (the payer, whose account is pulled) and the
// creditor is bob (the payee, who pulls).
func postPacs003Transfer(t *testing.T, l *Ledger) string {
	t.Helper()
	seedBalance(t, l, "alice", 10000)
	if _, err := l.PostTransfer(Transfer{
		ID:          "p3-tx-1",
		From:        "alice",
		To:          "bob",
		AmountCents: 2500,
	}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	return "p3-tx-1"
}

// pacs003TestOptions is the standard operator input for the happy
// path: both agents named, a mandate, a signature date.
func pacs003TestOptions() Pacs003Options {
	return Pacs003Options{
		DebtorAgent:          "BANKDEBB",
		CreditorAgent:        "BANKCRED",
		MandateID:            "MNDT-2026-001",
		MandateSignatureDate: "2026-01-15",
	}
}

func TestExportPacs003HappyPath(t *testing.T) {
	l := New()
	id := postPacs003Transfer(t, l)

	doc, err := l.ExportPacs003(id, pacs003TestOptions())
	if err != nil {
		t.Fatalf("ExportPacs003: %v", err)
	}
	body := string(doc)
	for _, want := range []string{
		`xmlns="urn:iso:std:iso:20022:tech:xsd:pacs.003.001.08"`,
		"<FIToFICstmrDrctDbt>",
		"<MsgId>p3-tx-1/drctdbt</MsgId>",
		"<NbOfTxs>1</NbOfTxs>",
		"<InstrId>p3-tx-1</InstrId>",
		"<EndToEndId>p3-tx-1</EndToEndId>",
		"<TxId>p3-tx-1</TxId>",
		`<InstdAmt Ccy="USD">25.00</InstdAmt>`,
		"<MndtId>MNDT-2026-001</MndtId>",
		"<DtOfSgntr>2026-01-15</DtOfSgntr>",
		"<CdtrAgt>",
		"<DbtrAgt>",
		"<InitgPty>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("export missing %q:\n%s", want, body)
		}
	}
	// The creditor agent (initiating party + CdtrAgt) rides its
	// operator reference twice; the debtor agent once.
	if n := strings.Count(body, "<Id>BANKCRED</Id>"); n != 2 {
		t.Errorf("creditor agent mentions = %d, want 2 (InitgPty/CdtrAgt)", n)
	}
	if n := strings.Count(body, "<Id>BANKDEBB</Id>"); n != 1 {
		t.Errorf("debtor agent mentions = %d, want 1 (DbtrAgt)", n)
	}
	// The debtor is the payer account, the creditor the payee
	// account — each named as a party and as an account.
	if n := strings.Count(body, "<Id>alice</Id>"); n != 2 {
		t.Errorf("debtor account mentions = %d, want 2 (Dbtr/DbtrAcct)", n)
	}
	if n := strings.Count(body, "<Id>bob</Id>"); n != 2 {
		t.Errorf("creditor account mentions = %d, want 2 (Cdtr/CdtrAcct)", n)
	}
	// The document must be well-formed XML under its own namespace.
	var parsed pacs003Document
	if err := xml.Unmarshal(doc, &parsed); err != nil {
		t.Fatalf("export is not well-formed XML: %v", err)
	}
	if parsed.DrctDbt.GrpHdr.MsgID != "p3-tx-1/drctdbt" {
		t.Errorf("parsed MsgId = %q, want p3-tx-1/drctdbt", parsed.DrctDbt.GrpHdr.MsgID)
	}
	if parsed.DrctDbt.TxInf.InstdAmt.Value != "25.00" || parsed.DrctDbt.TxInf.InstdAmt.Ccy != "USD" {
		t.Errorf("parsed amount = %+v, want USD 25.00", parsed.DrctDbt.TxInf.InstdAmt)
	}
	if parsed.DrctDbt.TxInf.DrctDbtTx.MndtRltdInf.MndtID != "MNDT-2026-001" {
		t.Errorf("parsed mandate = %q, want MNDT-2026-001", parsed.DrctDbt.TxInf.DrctDbtTx.MndtRltdInf.MndtID)
	}
}

func TestExportPacs003OptionalParties(t *testing.T) {
	l := New()
	id := postPacs003Transfer(t, l)

	// Names and scheme ID render when stated…
	doc, err := l.ExportPacs003(id, Pacs003Options{
		DebtorAgent:      "BANKDEBB",
		CreditorAgent:    "BANKCRED",
		MandateID:        "MNDT-2026-001",
		DebtorName:       "Alice Debtor",
		CreditorName:     "Bob Creditor",
		CreditorSchemeID: "SCHEME-9",
	})
	if err != nil {
		t.Fatalf("ExportPacs003: %v", err)
	}
	body := string(doc)
	for _, want := range []string{
		"<Nm>Alice Debtor</Nm>",
		"<Nm>Bob Creditor</Nm>",
		"<CdtrSchmeId>",
		"<Id>SCHEME-9</Id>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("export missing %q:\n%s", want, body)
		}
	}
	// …and are omitted when unstated — the parties are still
	// identified by account.
	doc, err = l.ExportPacs003(id, pacs003TestOptions())
	if err != nil {
		t.Fatalf("ExportPacs003: %v", err)
	}
	body = string(doc)
	for _, want := range []string{"<Nm>", "<CdtrSchmeId>"} {
		if strings.Contains(body, want) {
			t.Errorf("export unexpectedly contains %q:\n%s", want, body)
		}
	}
	// The mandate signature date is omitted when unstated.
	doc, err = l.ExportPacs003(id, Pacs003Options{
		DebtorAgent:   "BANKDEBB",
		CreditorAgent: "BANKCRED",
		MandateID:     "MNDT-2026-001",
	})
	if err != nil {
		t.Fatalf("ExportPacs003: %v", err)
	}
	if strings.Contains(string(doc), "<DtOfSgntr>") {
		t.Errorf("export unexpectedly contains DtOfSgntr:\n%s", doc)
	}
}

func TestExportPacs003FailClosed(t *testing.T) {
	l := New()
	id := postPacs003Transfer(t, l)
	full := pacs003TestOptions()

	if _, err := l.ExportPacs003("no-such-transfer", full); !errors.Is(err, ErrTransferNotFound) {
		t.Errorf("unknown transfer: err=%v, want ErrTransferNotFound", err)
	}
	noDbtrAgt := full
	noDbtrAgt.DebtorAgent = "  "
	if _, err := l.ExportPacs003(id, noDbtrAgt); !errors.Is(err, ErrPacs003MissingDebtorAgent) {
		t.Errorf("missing debtor agent: err=%v, want ErrPacs003MissingDebtorAgent", err)
	}
	noCdtrAgt := full
	noCdtrAgt.CreditorAgent = ""
	if _, err := l.ExportPacs003(id, noCdtrAgt); !errors.Is(err, ErrPacs003MissingCreditorAgent) {
		t.Errorf("missing creditor agent: err=%v, want ErrPacs003MissingCreditorAgent", err)
	}
	noMndt := full
	noMndt.MandateID = ""
	if _, err := l.ExportPacs003(id, noMndt); !errors.Is(err, ErrPacs003MissingMandate) {
		t.Errorf("missing mandate: err=%v, want ErrPacs003MissingMandate", err)
	}
	badDate := full
	badDate.MandateSignatureDate = "15/01/2026"
	if _, err := l.ExportPacs003(id, badDate); !errors.Is(err, ErrPacs003InvalidMandateDate) {
		t.Errorf("bad mandate date: err=%v, want ErrPacs003InvalidMandateDate", err)
	}
	// A cross-currency transfer's principal entry books only the
	// payer-side leg against the FX clearing account — rendering it
	// as a single customer direct debit would misstate the money
	// movement, so it is refused.
	fx := New(WithFXAccount("fx-clearing"))
	mustSetRate(t, fx, "USD", "EUR", 108, 100)
	seedBalance(t, fx, "alice", 10000)
	if _, err := fx.PostTransfer(Transfer{
		ID:          "p3-fx-1",
		From:        "alice",
		To:          "bob",
		AmountCents: 2500,
		Currency:    "USD",
		ToCurrency:  "EUR",
	}); err != nil {
		t.Fatalf("PostTransfer FX: %v", err)
	}
	if _, err := fx.ExportPacs003("p3-fx-1", full); !errors.Is(err, ErrPacs003FXTransfer) {
		t.Errorf("FX transfer: err=%v, want ErrPacs003FXTransfer", err)
	}
}

func TestExportPacs003DeterministicAndReadOnly(t *testing.T) {
	l := New()
	id := postPacs003Transfer(t, l)
	_, versionBefore := l.Snapshot("nobody")
	entriesBefore := len(l.Entries())

	first, err := l.ExportPacs003(id, pacs003TestOptions())
	if err != nil {
		t.Fatalf("first ExportPacs003: %v", err)
	}
	second, err := l.ExportPacs003(id, pacs003TestOptions())
	if err != nil {
		t.Fatalf("second ExportPacs003: %v", err)
	}
	// Same MsgId, stable across identical exports.
	if !strings.Contains(string(first), "<MsgId>p3-tx-1/drctdbt</MsgId>") ||
		!strings.Contains(string(second), "<MsgId>p3-tx-1/drctdbt</MsgId>") {
		t.Error("MsgId not stable across exports")
	}
	// Read-only: no version bump, no journal rows, intact chain.
	if _, v := l.Snapshot("nobody"); v != versionBefore {
		t.Errorf("version = %d, want unchanged %d", v, versionBefore)
	}
	if n := len(l.Entries()); n != entriesBefore {
		t.Errorf("journal entries = %d, want unchanged %d", n, entriesBefore)
	}
}
