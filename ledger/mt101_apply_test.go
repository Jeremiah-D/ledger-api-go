package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// mt101Item is one sequence B payment of a test MT101 document:
// transferID as :21:, amount as the :32B: amount body
// ("100,00"), ccy as the :32B: currency, creditorBIC as :57A:
// (when non-empty), creditorAccount as :57D: (when creditorBIC
// is empty), creditorIBAN as :59:, chargeBearer as :71A: (an
// empty chargeBearer omits :71A: entirely).
type mt101Item struct {
	transferID      string
	amount          string
	ccy             string
	creditorBIC     string
	creditorAccount string
	creditorIBAN    string
	chargeBearer    string
}

// mt101Doc renders one inbound SWIFT FIN MT101 message: msgID as
// :20:, debtorIBAN as :50H:, debtorBIC as :52A: (omitted when
// empty), valueDate as the :30: body ("261115"), and one sequence
// B per item. An empty valueDate defaults to "261115".
func mt101Doc(msgID, debtorIBAN, debtorBIC, valueDate string, items ...mt101Item) string {
	if valueDate == "" {
		valueDate = "261115"
	}
	var sb strings.Builder
	sb.WriteString("{1:F01BANKUS33XXX0000000000}\n")
	sb.WriteString("{2:I101BANKDEFFXXXXU3003}\n")
	sb.WriteString("{3:{108:" + msgID + "}}\n")
	sb.WriteString("{4:\n")
	sb.WriteString(":15A:\n")
	sb.WriteString(":20:" + msgID + "\n")
	sb.WriteString(":28D:1/1\n")
	sb.WriteString(":50H:/" + debtorIBAN + "\n")
	if debtorBIC != "" {
		sb.WriteString(":52A:" + debtorBIC + "\n")
	}
	sb.WriteString(":30:" + valueDate + "\n")
	for _, it := range items {
		sb.WriteString(":15B:\n")
		sb.WriteString(":21:" + it.transferID + "\n")
		sb.WriteString(":32B:" + it.ccy + it.amount + "\n")
		if it.creditorBIC != "" {
			sb.WriteString(":57A:" + it.creditorBIC + "\n")
		} else {
			sb.WriteString(":57D:" + it.creditorAccount + "\n")
		}
		sb.WriteString(":59:/" + it.creditorIBAN + "\n")
		if it.chargeBearer != "" {
			sb.WriteString(":71A:" + it.chargeBearer + "\n")
		}
	}
	sb.WriteString("-}")
	return sb.String()
}

// mt101ValidDoc is the happy-path document: msg-101-1 pays
// merchant USD 100.00 (tx-101-1) and merchant2 USD 50.00
// (tx-101-2) from cust, value date 261115.
func mt101ValidDoc() string {
	return mt101Doc("MT101APPLY0001", testIBANDE, "DEUTDEFF", "",
		mt101Item{transferID: "tx-101-1", amount: "100,00", ccy: "USD", creditorBIC: "NWBKGB2L", creditorIBAN: testIBANGB, chargeBearer: "SHA"},
		mt101Item{transferID: "tx-101-2", amount: "50,00", ccy: "USD", creditorBIC: "DEUTDEFF", creditorIBAN: testIBANFR, chargeBearer: "OUR"},
	)
}

// mt101UnknownIBAN is a valid IBAN no account in the MT101 apply
// fixture registered, so it names an unknown account.
const mt101UnknownIBAN = "DE75512108001245126199"

// mt101ApplyLedger builds a ledger with a funded payer and IBANs
// on both sides, ready for MT101 application through
// PostTransfer: cust holds USD 10,000.00, merchant and merchant2
// hold nothing yet; merchant3 holds nothing and carries no BIC,
// so its payments render :57D:.
func mt101ApplyLedger(t *testing.T, al *AuditLog) *Ledger {
	t.Helper()
	l := New(WithAuditLog(al))
	if _, _, err := l.Post(JournalEntry{ID: "fund-cust", DebitAccount: "cust", CreditAccount: "bank", AmountCents: 1000000, Currency: "USD"}); err != nil {
		t.Fatalf("fund Post: %v", err)
	}
	mustSetIBAN(t, l, "cust", testIBANDE, "DEUTDEFF")
	mustSetIBAN(t, l, "merchant", testIBANGB, "NWBKGB2L")
	mustSetIBAN(t, l, "merchant2", testIBANFR, "DEUTDEFF")
	mustSetIBAN(t, l, "merchant3", testIBANNL, "")
	return l
}

// applyMT101Doc parses doc and applies it, failing the test on
// parse errors: the gates under test are the apply gates, not the
// parse gates.
func applyMT101Doc(t *testing.T, l *Ledger, doc string) (MT101ApplyResult, error) {
	t.Helper()
	m, err := ParseMT101(doc)
	if err != nil {
		t.Fatalf("ParseMT101: %v", err)
	}
	return l.ApplyMT101(m, "correspondent-bank")
}

func TestParseMT101_RoundTrip(t *testing.T) {
	l := painTestLedger(t)
	base := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	mustPostTransferPain(t, l, "pay-1", "alice", "bob", 15000, "USD", base)
	mustPostTransferPain(t, l, "pay-2", "alice", "carol", 25050, "USD", base.Add(time.Hour))

	exported, err := l.ExportMT101(mt101Opts("alice"))
	if err != nil {
		t.Fatalf("ExportMT101: %v", err)
	}
	doc, err := ParseMT101(exported)
	if err != nil {
		t.Fatalf("ParseMT101(export): %v", err)
	}
	if doc.SenderLT != "DEUTDEFFXXX" || doc.ReceiverLT != "DEUTDEFFXXX" {
		t.Errorf("LTs = %q/%q, want DEUTDEFFXXX/DEUTDEFFXXX", doc.SenderLT, doc.ReceiverLT)
	}
	if doc.MsgID != mt101Ref(t, exported) {
		t.Errorf(":20: = %q, want %q", doc.MsgID, mt101Ref(t, exported))
	}
	if got := mt940ValueDate(doc.ValueDate); got != "261112" {
		t.Errorf(":30: = %q, want 261112", got)
	}
	if doc.DebtorIBAN != testIBANDE || doc.DebtorBIC != "DEUTDEFF" {
		t.Errorf("debtor = %q/%q, want %q/DEUTDEFF", doc.DebtorIBAN, doc.DebtorBIC, testIBANDE)
	}
	if len(doc.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(doc.Items))
	}
	first := doc.Items[0]
	if first.TransferID != "pay-1" || first.AmountCents != 15000 || first.Currency != "USD" {
		t.Errorf("first = %+v, want pay-1/15000/USD", first)
	}
	// bob has no registered BIC: the export renders :57D: with
	// the To account ID, and the parse carries it back.
	if first.CreditorAccount != "bob" || first.CreditorBIC != "" {
		t.Errorf("first :57a: = %q/%q, want :57D: bob", first.CreditorBIC, first.CreditorAccount)
	}
	if first.CreditorIBAN != testIBANGB || first.ChargeBearer != "SHA" {
		t.Errorf("first creditor = %q/%q, want %q/SHA", first.CreditorIBAN, first.ChargeBearer, testIBANGB)
	}
	second := doc.Items[1]
	if second.TransferID != "pay-2" || second.AmountCents != 25050 || second.Currency != "USD" {
		t.Errorf("second = %+v, want pay-2/25050/USD", second)
	}
	// carol has a registered BIC: the export renders :57A:.
	if second.CreditorBIC != "DEUTDEFF500" || second.CreditorAccount != "" {
		t.Errorf("second :57a: = %q/%q, want :57A: DEUTDEFF500", second.CreditorBIC, second.CreditorAccount)
	}
	if second.CreditorIBAN != testIBANFR {
		t.Errorf("second creditor IBAN = %q, want %q", second.CreditorIBAN, testIBANFR)
	}
	// A sequence B without :71A: defaults to SHA, the export's
	// default charge bearer.
	noBearer := mt101Doc("MT101APPLY0001", testIBANDE, "DEUTDEFF", "",
		mt101Item{transferID: "tx-101-1", amount: "100,00", ccy: "USD", creditorBIC: "NWBKGB2L", creditorIBAN: testIBANGB},
	)
	parsed, err := ParseMT101(noBearer)
	if err != nil {
		t.Fatalf("ParseMT101(no :71A:): %v", err)
	}
	if parsed.Items[0].ChargeBearer != "SHA" {
		t.Errorf("missing :71A: = %q, want SHA", parsed.Items[0].ChargeBearer)
	}
}

func TestParseMT101_RejectsBadBlockStructure(t *testing.T) {
	good := mt101ValidDoc()

	cases := []struct {
		name string
		doc  string
	}{
		{"block 2 is MT202", strings.Replace(good, "{2:I101", "{2:I202", 1)},
		{"missing block 3", strings.Replace(good, "{3:{108:MT101APPLY0001}}\n", "", 1)},
		{"missing {4:", strings.Replace(good, "{4:\n", "", 1)},
		{"missing trailer", strings.TrimSuffix(good, "-}")},
		{"field order swapped", strings.Replace(good,
			":28D:1/1\n:50H:/"+testIBANDE+"\n",
			":50H:/"+testIBANDE+"\n:28D:1/1\n", 1)},
		{"missing :30:", strings.Replace(good, ":30:261115\n", "", 1)},
		{":28D: is not 1/1", strings.Replace(good, ":28D:1/1\n", ":28D:1/2\n", 1)},
		{"empty :20:", strings.Replace(good, ":20:MT101APPLY0001\n", ":20:\n", 1)},
		{":32B: dot decimal", strings.Replace(good, ":32B:USD100,00\n", ":32B:USD100.00\n", 1)},
		{":32B: zero amount", strings.Replace(good, ":32B:USD100,00\n", ":32B:USD0,00\n", 1)},
		{":32B: lowercase currency", strings.Replace(good, ":32B:USD100,00\n", ":32B:usd100,00\n", 1)},
		{":30: impossible date", strings.Replace(good, ":30:261115\n", ":30:991331\n", 1)},
		{"duplicate :21:", strings.Replace(good, ":21:tx-101-2\n", ":21:tx-101-1\n", 1)},
		{"too few lines", "{1:F01BANKUS33XXX0000000000}"},
		{"empty document", "  "},
	}
	// No sequence B at all: a single-payment document with its
	// only sequence B removed.
	single := mt101Doc("MT101APPLY0001", testIBANDE, "DEUTDEFF", "",
		mt101Item{transferID: "tx-101-1", amount: "100,00", ccy: "USD", creditorBIC: "NWBKGB2L", creditorIBAN: testIBANGB, chargeBearer: "SHA"},
	)
	noSeqB := strings.Replace(single, ":15B:\n:21:tx-101-1\n:32B:USD100,00\n:57A:NWBKGB2L\n:59:/"+testIBANGB+"\n:71A:SHA\n", "", 1)
	if _, err := ParseMT101(noSeqB); err == nil {
		t.Errorf("no sequence B: parsed without error, want refusal")
	}
	for _, tc := range cases {
		if _, err := ParseMT101(tc.doc); err == nil {
			t.Errorf("%s: parsed without error, want refusal", tc.name)
		}
	}
	// The value cases carry their own error identity.
	if _, err := ParseMT101(strings.Replace(good, ":30:261115\n", ":30:991331\n", 1)); !errors.Is(err, ErrMT101ValueDate) {
		t.Errorf("impossible date: err = %v, want ErrMT101ValueDate", err)
	}
	if _, err := ParseMT101(strings.Replace(good, ":32B:USD100,00\n", ":32B:USD0,00\n", 1)); !errors.Is(err, ErrMT101Amount) {
		t.Errorf("zero amount: err = %v, want ErrMT101Amount", err)
	}
	if _, err := ParseMT101(strings.Replace(good, ":32B:USD100,00\n", ":32B:usd100,00\n", 1)); !errors.Is(err, ErrMT101Currency) {
		t.Errorf("lowercase currency: err = %v, want ErrMT101Currency", err)
	}
}

func TestParseMT101_RejectsUnknownField(t *testing.T) {
	good := mt101ValidDoc()

	// A :70: narrative is out of the core set: refused, not
	// parsed with the field quietly discarded — a PainTransfer
	// has no memo field.
	with70 := strings.Replace(good, "-}", ":70:Invoice 123\n-}", 1)
	if _, err := ParseMT101(with70); !errors.Is(err, ErrMT101Field) {
		t.Errorf(":70: field: err = %v, want ErrMT101Field", err)
	}
	// An optional :72: sender-to-receiver field is out of the
	// core set too.
	with72 := strings.Replace(good, "-}", ":72:/BNK/Do not execute\n-}", 1)
	if _, err := ParseMT101(with72); !errors.Is(err, ErrMT101Field) {
		t.Errorf(":72: field: err = %v, want ErrMT101Field", err)
	}
	// :23E: is likewise out of scope.
	with23E := strings.Replace(good, "-}", ":23E:PHOB\n-}", 1)
	if _, err := ParseMT101(with23E); !errors.Is(err, ErrMT101Field) {
		t.Errorf(":23E: field: err = %v, want ErrMT101Field", err)
	}
	// An unknown :99: inside sequence B is an unknown field, not
	// a continuation.
	with99 := strings.Replace(good, ":71A:SHA\n", ":71A:SHA\n:99:extra\n", 1)
	if _, err := ParseMT101(with99); !errors.Is(err, ErrMT101Field) {
		t.Errorf(":99: in sequence B: err = %v, want ErrMT101Field", err)
	}
	// A :57B: variant is not one of the export's :57a: forms.
	with57B := strings.Replace(good, ":57A:NWBKGB2L\n", ":57B:Some Bank\n", 1)
	if _, err := ParseMT101(with57B); !errors.Is(err, ErrMT101Field) {
		t.Errorf(":57B: field: err = %v, want ErrMT101Field", err)
	}
}

func TestParseMT101_AcceptsCRLF(t *testing.T) {
	doc := mt101ValidDoc()

	crlf := strings.ReplaceAll(doc, "\n", "\r\n")
	if _, err := ParseMT101(crlf); err != nil {
		t.Errorf("CRLF: %v", err)
	}
	// A trailing newline after the trailer is tolerated.
	if _, err := ParseMT101(doc + "\n"); err != nil {
		t.Errorf("trailing newline: %v", err)
	}
}

func TestApplyMT101_BooksTransfers(t *testing.T) {
	al := mustAuditLog(t)
	l := mt101ApplyLedger(t, al)

	res, err := applyMT101Doc(t, l, mt101ValidDoc())
	if err != nil {
		t.Fatalf("ApplyMT101: %v", err)
	}
	if res.Duplicate {
		t.Error("duplicate = true on first apply")
	}
	if res.MsgID != "MT101APPLY0001" {
		t.Errorf("msg_id = %q, want MT101APPLY0001", res.MsgID)
	}
	if len(res.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(res.Items))
	}
	first, second := res.Items[0], res.Items[1]
	if first.Err != nil || second.Err != nil {
		t.Fatalf("item errors = %v / %v, want nil", first.Err, second.Err)
	}
	if first.EntryID != "tx-101-1" || second.EntryID != "tx-101-2" {
		t.Errorf("entry IDs = %q/%q, want tx-101-1/tx-101-2", first.EntryID, second.EntryID)
	}
	if first.From != "cust" || first.To != "merchant" {
		t.Errorf("first legs = %q->%q, want cust->merchant", first.From, first.To)
	}
	if second.From != "cust" || second.To != "merchant2" {
		t.Errorf("second legs = %q->%q, want cust->merchant2", second.From, second.To)
	}
	if first.Duplicate || second.Duplicate {
		t.Error("duplicate = true on first apply, want false")
	}
	// The money moved: cust paid 150.00, each merchant received
	// its own payment in full.
	if got := l.BalanceIn("cust", "USD"); got != 1000000-15000 {
		t.Errorf("cust balance = %d, want %d", got, 1000000-15000)
	}
	if got := l.BalanceIn("merchant", "USD"); got != 10000 {
		t.Errorf("merchant balance = %d, want 10000", got)
	}
	if got := l.BalanceIn("merchant2", "USD"); got != 5000 {
		t.Errorf("merchant2 balance = %d, want 5000", got)
	}
	// Audit: one mt101_transfer_booked event per booked
	// transfer, each with the exact details — the :20: on the
	// durable record.
	events := readAuditOps(t, al)
	if countOp(events, "mt101_transfer_booked") != 2 {
		t.Errorf("mt101_transfer_booked count = %d, want 2", countOp(events, "mt101_transfer_booked"))
	}
	ev := findOp(events, "mt101_transfer_booked")
	if ev == nil {
		t.Fatal("mt101_transfer_booked event missing")
	}
	if ev.Actor != "correspondent-bank" {
		t.Errorf("actor = %q, want correspondent-bank (ApplyMT101)", ev.Actor)
	}
	if ev.TraceID != "MT101APPLY0001" {
		t.Errorf("trace = %q, want MT101APPLY0001", ev.TraceID)
	}
	if ev.Details["msg_id"] != "MT101APPLY0001" || ev.Details["transfer_id"] != "tx-101-1" ||
		ev.Details["amount"] != float64(10000) || ev.Details["currency"] != "USD" {
		t.Errorf("details = %v, want msg/transfer/amount/currency", ev.Details)
	}
}

func TestApplyMT101_RefusesUnknownDebtor(t *testing.T) {
	al := mustAuditLog(t)
	l := mt101ApplyLedger(t, al)
	// mt101UnknownIBAN names no registered account: the ledger
	// never debits an unknown account.
	doc := mt101Doc("MT101APPLY0001", mt101UnknownIBAN, "DEUTDEFF", "",
		mt101Item{transferID: "tx-101-1", amount: "100,00", ccy: "USD", creditorBIC: "NWBKGB2L", creditorIBAN: testIBANGB, chargeBearer: "SHA"},
	)
	_, err := applyMT101Doc(t, l, doc)
	if !errors.Is(err, ErrMT101UnknownAccount) {
		t.Errorf("err = %v, want ErrMT101UnknownAccount", err)
	}
	if got := l.BalanceIn("merchant", "USD"); got != 0 {
		t.Errorf("merchant balance = %d, want 0 (nothing booked)", got)
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt101_transfer_booked") != 0 {
		t.Errorf("mt101_transfer_booked count = %d, want 0", countOp(events, "mt101_transfer_booked"))
	}
}

func TestApplyMT101_RefusesUnknownCreditor(t *testing.T) {
	al := mustAuditLog(t)
	l := mt101ApplyLedger(t, al)
	// mt101UnknownIBAN names no registered account: the ledger
	// never credits an unknown account.
	doc := mt101Doc("MT101APPLY0001", testIBANDE, "DEUTDEFF", "",
		mt101Item{transferID: "tx-101-1", amount: "100,00", ccy: "USD", creditorBIC: "NWBKGB2L", creditorIBAN: mt101UnknownIBAN, chargeBearer: "SHA"},
	)
	_, err := applyMT101Doc(t, l, doc)
	if !errors.Is(err, ErrMT101UnknownAccount) {
		t.Errorf("err = %v, want ErrMT101UnknownAccount", err)
	}
	if got := l.BalanceIn("cust", "USD"); got != 1000000 {
		t.Errorf("cust balance = %d, want 1000000 (nothing booked)", got)
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt101_transfer_booked") != 0 {
		t.Errorf("mt101_transfer_booked count = %d, want 0", countOp(events, "mt101_transfer_booked"))
	}
}

func TestApplyMT101_RefusesBadChargeBearer(t *testing.T) {
	// :71A: outside OUR/BEN/SHA is a parse refusal.
	doc := mt101Doc("MT101APPLY0001", testIBANDE, "DEUTDEFF", "",
		mt101Item{transferID: "tx-101-1", amount: "100,00", ccy: "USD", creditorBIC: "NWBKGB2L", creditorIBAN: testIBANGB, chargeBearer: "SHAR"},
	)
	if _, err := ParseMT101(doc); !errors.Is(err, ErrMT101ChargeBearer) {
		t.Errorf("SHAR :71A:: err = %v, want ErrMT101ChargeBearer", err)
	}
	// A hand-built document with a bad charge bearer is refused
	// before anything moves: the parse gate and the apply gate
	// share one check.
	parsed, err := ParseMT101(mt101ValidDoc())
	if err != nil {
		t.Fatalf("ParseMT101: %v", err)
	}
	parsed.Items[0].ChargeBearer = "XXX"
	al := mustAuditLog(t)
	l := mt101ApplyLedger(t, al)
	if _, err := l.ApplyMT101(parsed, "correspondent-bank"); !errors.Is(err, ErrInvalidMT101) {
		t.Errorf("hand-built bad bearer: err = %v, want ErrInvalidMT101", err)
	}
	// The same fail-closed posture covers a :52A: or :57A: that
	// disagrees with the registry: the BIC must name the
	// registered servicing bank, never an unverifiable third
	// party.
	parsed, err = ParseMT101(mt101ValidDoc())
	if err != nil {
		t.Fatalf("ParseMT101: %v", err)
	}
	parsed.DebtorBIC = "NWBKGB2L"
	if _, err := l.ApplyMT101(parsed, "correspondent-bank"); !errors.Is(err, ErrInvalidMT101) {
		t.Errorf("mismatched :52A:: err = %v, want ErrInvalidMT101", err)
	}
	parsed, err = ParseMT101(mt101ValidDoc())
	if err != nil {
		t.Fatalf("ParseMT101: %v", err)
	}
	parsed.Items[0].CreditorBIC = "DEUTDEFF"
	if _, err := l.ApplyMT101(parsed, "correspondent-bank"); !errors.Is(err, ErrInvalidMT101) {
		t.Errorf("mismatched :57A:: err = %v, want ErrInvalidMT101", err)
	}
	parsed, err = ParseMT101(mt101ValidDoc())
	if err != nil {
		t.Fatalf("ParseMT101: %v", err)
	}
	parsed.Items[0].CreditorBIC = ""
	parsed.Items[0].CreditorAccount = "merchant2"
	if _, err := l.ApplyMT101(parsed, "correspondent-bank"); !errors.Is(err, ErrInvalidMT101) {
		t.Errorf("mismatched :57D:: err = %v, want ErrInvalidMT101", err)
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt101_transfer_booked") != 0 {
		t.Errorf("mt101_transfer_booked count = %d, want 0", countOp(events, "mt101_transfer_booked"))
	}
}

func TestApplyMT101_ParksGatedItemWithoutBlockingOthers(t *testing.T) {
	al := mustAuditLog(t)
	l := mt101ApplyLedger(t, al)
	// Freeze the first creditor only: its transfer parks, the
	// second books.
	l.Freeze("merchant")

	res, err := applyMT101Doc(t, l, mt101ValidDoc())
	if err != nil {
		t.Fatalf("ApplyMT101: %v", err)
	}
	if res.Duplicate {
		t.Error("duplicate = true on first apply")
	}
	if len(res.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(res.Items))
	}
	parked, booked := res.Items[0], res.Items[1]
	if !errors.Is(parked.Err, ErrAccountFrozen) {
		t.Errorf("parked err = %v, want ErrAccountFrozen", parked.Err)
	}
	if parked.RepairID == "" {
		t.Error("parked RepairID is empty, want a repair record")
	}
	if _, ok := l.GetRepair(parked.RepairID); !ok {
		t.Errorf("GetRepair(%q): not found", parked.RepairID)
	}
	if parked.EntryID != "" {
		t.Errorf("parked EntryID = %q, want empty", parked.EntryID)
	}
	if booked.Err != nil {
		t.Fatalf("booked err = %v, want nil", booked.Err)
	}
	if booked.EntryID != "tx-101-2" {
		t.Errorf("booked EntryID = %q, want tx-101-2", booked.EntryID)
	}
	// The gated transfer moved nothing; the other moved in full.
	if got := l.BalanceIn("merchant", "USD"); got != 0 {
		t.Errorf("merchant balance = %d, want 0", got)
	}
	if got := l.BalanceIn("merchant2", "USD"); got != 5000 {
		t.Errorf("merchant2 balance = %d, want 5000", got)
	}
	if got := l.BalanceIn("cust", "USD"); got != 1000000-5000 {
		t.Errorf("cust balance = %d, want %d", got, 1000000-5000)
	}
	// Audit: exactly one booked event — the parked transfer
	// leaves its trail in the repair queue, not in the booked
	// events.
	events := readAuditOps(t, al)
	if countOp(events, "mt101_transfer_booked") != 1 {
		t.Errorf("mt101_transfer_booked count = %d, want 1", countOp(events, "mt101_transfer_booked"))
	}
}

func TestApplyMT101_ReplaySameReferenceNoop(t *testing.T) {
	al := mustAuditLog(t)
	l := mt101ApplyLedger(t, al)
	doc := mt101ValidDoc()
	if _, err := applyMT101Doc(t, l, doc); err != nil {
		t.Fatalf("ApplyMT101: %v", err)
	}
	// Same :20:, same terms: an idempotent no-op returning the
	// stored outcomes, with no second audit event.
	res, err := applyMT101Doc(t, l, doc)
	if err != nil {
		t.Fatalf("ApplyMT101 replay: %v", err)
	}
	if !res.Duplicate {
		t.Error("duplicate = false on replay, want true")
	}
	for i, item := range res.Items {
		if !item.Duplicate {
			t.Errorf("item %d duplicate = false, want true", i)
		}
		if item.Err != nil {
			t.Errorf("item %d err = %v, want nil", i, item.Err)
		}
	}
	if got := l.BalanceIn("merchant", "USD"); got != 10000 {
		t.Errorf("merchant balance = %d, want 10000 (no second booking)", got)
	}
	if got := l.BalanceIn("merchant2", "USD"); got != 5000 {
		t.Errorf("merchant2 balance = %d, want 5000 (no second booking)", got)
	}
	// Same :20:+:21:, different amount: a conflict, never a
	// silent overwrite.
	conflict := mt101Doc("MT101APPLY0001", testIBANDE, "DEUTDEFF", "",
		mt101Item{transferID: "tx-101-1", amount: "200,00", ccy: "USD", creditorBIC: "NWBKGB2L", creditorIBAN: testIBANGB, chargeBearer: "SHA"},
		mt101Item{transferID: "tx-101-2", amount: "50,00", ccy: "USD", creditorBIC: "DEUTDEFF", creditorIBAN: testIBANFR, chargeBearer: "OUR"},
	)
	if _, err := applyMT101Doc(t, l, conflict); !errors.Is(err, ErrMT101MsgIDConflict) {
		t.Errorf("conflict amount: err = %v, want ErrMT101MsgIDConflict", err)
	}
	// Same :20:+:21:, different charge bearer: a conflict too.
	conflict = mt101Doc("MT101APPLY0001", testIBANDE, "DEUTDEFF", "",
		mt101Item{transferID: "tx-101-1", amount: "100,00", ccy: "USD", creditorBIC: "NWBKGB2L", creditorIBAN: testIBANGB, chargeBearer: "BEN"},
		mt101Item{transferID: "tx-101-2", amount: "50,00", ccy: "USD", creditorBIC: "DEUTDEFF", creditorIBAN: testIBANFR, chargeBearer: "OUR"},
	)
	if _, err := applyMT101Doc(t, l, conflict); !errors.Is(err, ErrMT101MsgIDConflict) {
		t.Errorf("conflict bearer: err = %v, want ErrMT101MsgIDConflict", err)
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt101_transfer_booked") != 2 {
		t.Errorf("mt101_transfer_booked count = %d, want 2 after replay and conflicts", countOp(events, "mt101_transfer_booked"))
	}
}
