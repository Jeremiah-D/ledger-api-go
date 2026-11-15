package ledger

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// pain001InboundLedger builds a ledger with a funded payer and
// IBANs with BICs on both sides, ready for pain.001 initiation
// through PostTransfer: cust holds USD 10,000.00, merchant holds
// nothing yet.
func pain001InboundLedger(t *testing.T) *Ledger {
	t.Helper()
	l := New()
	if _, _, err := l.Post(JournalEntry{ID: "fund-cust", DebitAccount: "cust", CreditAccount: "bank", AmountCents: 1000000, Currency: "USD"}); err != nil {
		t.Fatalf("fund Post: %v", err)
	}
	mustSetIBAN(t, l, "cust", testIBANDE, "DEUTDEFF")
	mustSetIBAN(t, l, "merchant", testIBANGB, "NWBKGB2L")
	return l
}

// pain001Tx is one CdtTrfTxInf of a test document.
type pain001Tx struct {
	endToEnd string
	amount   string
	ccy      string
	cdtrIBAN string
}

// pain001PmtInf is one PmtInf of a test document: every transaction
// in it shares the execution date and debtor, the export's
// grouping rule.
type pain001PmtInf struct {
	exctnDt  string
	dbtrIBAN string
	pmtMtd   string
	txs      []pain001Tx
}

// pain001Doc renders one inbound pain.001.001.03 document in the
// shape ExportPain001 renders: msgID in GrpHdr (NbOfTxs and
// CtrlSum computed from the transactions), one PmtInf per group
// (PmtMtd TRF, ReqdExctnDt, Dbtr cust, DbtrAcct, DbtrAgt), one
// CdtTrfTxInf per transaction (EndToEndId, InstdAmt, Cdtr
// merchant, CdtrAcct, CdtrAgt). Empty fields render as empty
// elements, so parse refusals can be driven per field. version
// selects the namespace (001.03 valid, anything else the wrong
// schema version).
func pain001Doc(version, msgID string, pmts ...pain001PmtInf) string {
	return pain001DocCtrl(version, msgID, "", -1, pmts...)
}

// pain001DocCtrl is pain001Doc with an explicit GrpHdr CtrlSum
// ("" computes it) and NbOfTxs (-1 computes it), so header
// disagreements can be driven.
func pain001DocCtrl(version, msgID, ctrlSum string, nbOfTxs int, pmts ...pain001PmtInf) string {
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pain.001.` + version + `">` + "\n")
	sb.WriteString("<CstmrCdtTrfInitn>\n<GrpHdr>\n")
	sb.WriteString("<MsgId>" + msgID + "</MsgId>\n")
	sb.WriteString("<CreDtTm>2026-11-15T10:00:00Z</CreDtTm>\n")
	total := 0
	var sum int64
	for _, pmt := range pmts {
		total += len(pmt.txs)
		for _, tx := range pmt.txs {
			sum += pacs010TestCents(tx.amount)
		}
	}
	if nbOfTxs < 0 {
		nbOfTxs = total
	}
	if ctrlSum == "" {
		ctrlSum = pain001Amount(sum)
	}
	sb.WriteString(fmt.Sprintf("<NbOfTxs>%d</NbOfTxs>\n", nbOfTxs))
	sb.WriteString(fmt.Sprintf("<CtrlSum>%s</CtrlSum>\n", ctrlSum))
	sb.WriteString("<InitgPty><Nm>LEDGER</Nm></InitgPty>\n</GrpHdr>\n")
	for _, pmt := range pmts {
		var pmtSum int64
		for _, tx := range pmt.txs {
			pmtSum += pacs010TestCents(tx.amount)
		}
		exctnDt := pmt.exctnDt
		if exctnDt == "" {
			exctnDt = pain001Today()
		}
		dbtrIBAN := pmt.dbtrIBAN
		if dbtrIBAN == "" {
			dbtrIBAN = testIBANDE
		}
		pmtMtd := pmt.pmtMtd
		if pmtMtd == "" {
			pmtMtd = "TRF"
		}
		sb.WriteString("<PmtInf>\n")
		sb.WriteString(fmt.Sprintf("<PmtInfId>%s-%s</PmtInfId>\n", msgID, exctnDt))
		sb.WriteString(fmt.Sprintf("<PmtMtd>%s</PmtMtd>\n", pmtMtd))
		sb.WriteString(fmt.Sprintf("<NbOfTxs>%d</NbOfTxs>\n", len(pmt.txs)))
		sb.WriteString(fmt.Sprintf("<CtrlSum>%s</CtrlSum>\n", pain001Amount(pmtSum)))
		sb.WriteString(fmt.Sprintf("<ReqdExctnDt>%s</ReqdExctnDt>\n", exctnDt))
		sb.WriteString("<Dbtr><Nm>cust</Nm></Dbtr>\n")
		sb.WriteString(fmt.Sprintf("<DbtrAcct><Id><IBAN>%s</IBAN></Id></DbtrAcct>\n", dbtrIBAN))
		sb.WriteString("<DbtrAgt><FinInstnId><BIC>DEUTDEFF</BIC></FinInstnId></DbtrAgt>\n")
		for _, tx := range pmt.txs {
			cdtrIBAN := tx.cdtrIBAN
			if cdtrIBAN == "" {
				cdtrIBAN = testIBANGB
			}
			sb.WriteString("<CdtTrfTxInf>\n")
			sb.WriteString(fmt.Sprintf("<PmtId><EndToEndId>%s</EndToEndId></PmtId>\n", tx.endToEnd))
			sb.WriteString(fmt.Sprintf("<Amt><InstdAmt Ccy=\"%s\">%s</InstdAmt></Amt>\n", tx.ccy, tx.amount))
			sb.WriteString("<Cdtr><Nm>merchant</Nm></Cdtr>\n")
			sb.WriteString(fmt.Sprintf("<CdtrAcct><Id><IBAN>%s</IBAN></Id></CdtrAcct>\n", cdtrIBAN))
			sb.WriteString("<CdtrAgt><FinInstnId><BIC>NWBKGB2L</BIC></FinInstnId></CdtrAgt>\n")
			sb.WriteString("</CdtTrfTxInf>\n")
		}
		sb.WriteString("</PmtInf>\n")
	}
	sb.WriteString("</CstmrCdtTrfInitn>\n</Document>\n")
	return sb.String()
}

// pain001Today is today's UTC date, the only execution date the
// inbound intake accepts.
func pain001Today() string {
	return time.Now().UTC().Format("2006-01-02")
}

// pain001ValidDoc is the happy-path document: msg-001-1 pays
// merchant USD 100.00 (e-001-1) and USD 50.00 (e-001-2) from cust,
// today.
func pain001ValidDoc() string {
	return pain001Doc("001.03", "msg-001-1",
		pain001PmtInf{
			txs: []pain001Tx{
				{endToEnd: "e-001-1", amount: "100.00", ccy: "USD"},
				{endToEnd: "e-001-2", amount: "50.00", ccy: "USD"},
			},
		})
}

func TestParsePain001_Valid(t *testing.T) {
	doc, err := ParsePain001([]byte(pain001ValidDoc()))
	if err != nil {
		t.Fatalf("ParsePain001: %v", err)
	}
	if doc.MsgID != "msg-001-1" || doc.TxCount != 2 || doc.CtrlSumCents != 15000 {
		t.Errorf("doc = %+v, want msg-001-1/2/15000", doc)
	}
	if len(doc.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(doc.Items))
	}
	first := doc.Items[0]
	if first.EndToEndID != "e-001-1" || first.AmountCents != 10000 || first.Currency != "USD" {
		t.Errorf("first = %+v, want e-001-1/10000/USD", first)
	}
	if first.DebtorIBAN != testIBANDE || first.CreditorIBAN != testIBANGB {
		t.Errorf("first IBANs = %q/%q, want %q/%q", first.DebtorIBAN, first.CreditorIBAN, testIBANDE, testIBANGB)
	}
	if first.DebtorBIC != "DEUTDEFF" || first.CreditorBIC != "NWBKGB2L" {
		t.Errorf("first BICs = %q/%q, want DEUTDEFF/NWBKGB2L", first.DebtorBIC, first.CreditorBIC)
	}
	if first.ReqdExctnDt.UTC().Format("2006-01-02") != pain001Today() {
		t.Errorf("execution date = %v, want today UTC", first.ReqdExctnDt)
	}
	if first.PmtInfID == "" || first.DebtorName != "cust" || first.CreditorName != "merchant" {
		t.Errorf("first context = %+v, want PmtInfId set, cust/merchant", first)
	}
}

// The parser reads back what the exporter renders: a document
// ExportPain001 produces parses with the same transfer IDs,
// amounts, and currencies.
func TestParsePain001_ExportRoundTrip(t *testing.T) {
	l := painStatusTestLedger(t)
	rendered, err := l.ExportPain001(Pain001ExportOptions{})
	if err != nil {
		t.Fatalf("ExportPain001: %v", err)
	}
	doc, err := ParsePain001(rendered)
	if err != nil {
		t.Fatalf("ParsePain001(exported): %v", err)
	}
	if doc.TxCount != 2 || doc.CtrlSumCents != 12000 {
		t.Errorf("doc = %+v, want 2 txs / 12000 cents", doc)
	}
	if len(doc.Items) != 2 || doc.Items[0].EndToEndID != "ps1" || doc.Items[1].EndToEndID != "ps2" {
		t.Errorf("items = %+v, want ps1/ps2", doc.Items)
	}
	if doc.Items[0].AmountCents != 5000 || doc.Items[1].AmountCents != 7000 {
		t.Errorf("amounts = %d/%d, want 5000/7000", doc.Items[0].AmountCents, doc.Items[1].AmountCents)
	}
}

func TestParsePain001_WrongNamespace(t *testing.T) {
	l := pain001InboundLedger(t)
	doc := pain001Doc("001.09", "msg-001-ns",
		pain001PmtInf{txs: []pain001Tx{{endToEnd: "e-001-ns", amount: "10.00", ccy: "USD"}}})
	if _, err := ParsePain001([]byte(doc)); err == nil {
		t.Error("wrong namespace parsed without error")
	}
	// The unparsed document still names its initiation, so it
	// parks in the repair queue for operator resolution.
	repairID, err := l.ParkPain001Unparsed([]byte(doc), "ops")
	if err != nil {
		t.Fatalf("ParkPain001Unparsed: %v", err)
	}
	if repairID == "" {
		t.Fatal("ParkPain001Unparsed returned no repair record")
	}
	rec, ok := l.GetRepair(repairID)
	if !ok {
		t.Fatalf("GetRepair(%q): not found", repairID)
	}
	if rec.Status != RepairStatusOpen || rec.ReasonCode != RepairReasonInvalidFormat {
		t.Errorf("repair = %+v, want open/invalid_format", rec)
	}
	if rec.Instruction.Destination != "pain001-inbound:msg-001-ns" {
		t.Errorf("repair destination = %q, want pain001-inbound:msg-001-ns", rec.Instruction.Destination)
	}
}

func TestParsePain001_CtrlSumMismatch(t *testing.T) {
	doc := pain001DocCtrl("001.03", "msg-001-sum", "999.99", -1,
		pain001PmtInf{txs: []pain001Tx{{endToEnd: "e-001-sum", amount: "10.00", ccy: "USD"}}})
	if _, err := ParsePain001([]byte(doc)); err == nil {
		t.Error("CtrlSum mismatch parsed without error")
	}
}

func TestParsePain001_NonTRFMethod(t *testing.T) {
	doc := pain001Doc("001.03", "msg-001-mtd",
		pain001PmtInf{pmtMtd: "CHK", txs: []pain001Tx{{endToEnd: "e-001-mtd", amount: "10.00", ccy: "USD"}}})
	if _, err := ParsePain001([]byte(doc)); err == nil {
		t.Error("PmtMtd CHK parsed without error")
	}
}

func TestApplyPain001_Success(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	if _, _, err := l.Post(JournalEntry{ID: "fund-cust", DebitAccount: "cust", CreditAccount: "bank", AmountCents: 1000000, Currency: "USD"}); err != nil {
		t.Fatalf("fund Post: %v", err)
	}
	mustSetIBAN(t, l, "cust", testIBANDE, "DEUTDEFF")
	mustSetIBAN(t, l, "merchant", testIBANGB, "NWBKGB2L")

	doc, err := ParsePain001([]byte(pain001ValidDoc()))
	if err != nil {
		t.Fatalf("ParsePain001: %v", err)
	}
	res, err := l.ApplyPain001(doc, "ops")
	if err != nil {
		t.Fatalf("ApplyPain001: %v", err)
	}
	if res.Duplicate {
		t.Error("Duplicate = true on first apply, want false")
	}
	if len(res.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(res.Items))
	}
	for i, want := range []struct {
		id  string
		amt int64
	}{{"e-001-1", 10000}, {"e-001-2", 5000}} {
		item := res.Items[i]
		if item.Err != nil {
			t.Fatalf("item %d err = %v", i, item.Err)
		}
		if item.EntryID != want.id {
			t.Errorf("item %d entry_id = %q, want %q", i, item.EntryID, want.id)
		}
		if item.Receipt.TransferID != want.id || len(item.Receipt.Entries) == 0 || item.Receipt.Entries[0].AmountCents != want.amt {
			t.Errorf("item %d receipt = %+v, want transfer %q amount %d", i, item.Receipt, want.id, want.amt)
		}
		if item.From != "cust" || item.To != "merchant" {
			t.Errorf("item %d legs = %q -> %q, want cust -> merchant", i, item.From, item.To)
		}
	}
	// The money moved: cust paid 150.00, merchant received it.
	if got := l.BalanceIn("cust", "USD"); got != 1000000-15000 {
		t.Errorf("cust balance = %d, want %d", got, 1000000-15000)
	}
	if got := l.BalanceIn("merchant", "USD"); got != 15000 {
		t.Errorf("merchant balance = %d, want 15000", got)
	}
	// The instruction lifecycle record is registered under the
	// wire MsgId, in exported state, so pain.002 can answer it.
	rec, ok := l.PainInstruction("msg-001-1")
	if !ok {
		t.Fatal("PainInstruction(msg-001-1): not registered")
	}
	if rec.State != Pain001StateExported {
		t.Errorf("state = %q, want exported", rec.State)
	}
	if rec.TxCount != 2 || rec.CtrlSum != 15000 || rec.Currency != "USD" {
		t.Errorf("instruction = %d/%d/%q, want 2/15000/USD", rec.TxCount, rec.CtrlSum, rec.Currency)
	}
	if len(rec.Txs) != 2 || rec.Txs[0].TxID != "e-001-1" || rec.Txs[1].TxID != "e-001-2" {
		t.Errorf("instruction txs = %+v, want e-001-1/e-001-2", rec.Txs)
	}
	// The booked transfers export again through the outbound
	// path: the inbound booking feeds the export lifecycle.
	rendered, err := l.ExportPain001(Pain001ExportOptions{})
	if err != nil {
		t.Fatalf("ExportPain001 after apply: %v", err)
	}
	if !strings.Contains(string(rendered), "e-001-1") || !strings.Contains(string(rendered), "e-001-2") {
		t.Errorf("re-export missing booked transfers:\n%s", rendered)
	}
	// The batch audit event.
	events := readAuditOps(t, al)
	ev := findOp(events, "pain001_initiation_applied")
	if ev == nil {
		t.Fatal("pain001_initiation_applied audit event missing")
	}
	if countOp(events, "pain001_initiation_applied") != 1 {
		t.Errorf("pain001_initiation_applied count = %d, want 1", countOp(events, "pain001_initiation_applied"))
	}
	for k, want := range map[string]any{
		"msg_id": "msg-001-1", "tx_count": float64(2),
		"ctrl_sum_cents": float64(15000), "currency": "USD",
		"rejected_count": float64(0),
	} {
		if ev.Details[k] != want {
			t.Errorf("audit %s = %v, want %v", k, ev.Details[k], want)
		}
	}
}

// An IBAN no account registered parks that transaction; the other
// transaction still books.
func TestApplyPain001_UnknownIBAN(t *testing.T) {
	l := pain001InboundLedger(t)
	doc, err := ParsePain001([]byte(pain001Doc("001.03", "msg-001-unk",
		pain001PmtInf{txs: []pain001Tx{
			{endToEnd: "e-001-ok", amount: "10.00", ccy: "USD"},
			{endToEnd: "e-001-unk", amount: "20.00", ccy: "USD", cdtrIBAN: testIBANFR},
		}})))
	if err != nil {
		t.Fatalf("ParsePain001: %v", err)
	}
	res, err := l.ApplyPain001(doc, "ops")
	if err != nil {
		t.Fatalf("ApplyPain001: %v", err)
	}
	if res.Items[0].Err != nil {
		t.Errorf("good item err = %v", res.Items[0].Err)
	}
	bad := res.Items[1]
	if !errors.Is(bad.Err, ErrPain001UnknownIBAN) {
		t.Errorf("bad item err = %v, want ErrPain001UnknownIBAN", bad.Err)
	}
	if bad.RepairID == "" {
		t.Error("bad item has no repair record")
	}
	if _, ok := l.GetRepair(bad.RepairID); !ok {
		t.Errorf("GetRepair(%q): not found", bad.RepairID)
	}
	if _, ok := l.entries["e-001-unk"]; ok {
		t.Error("entry e-001-unk booked for an unknown IBAN")
	}
	if got := l.BalanceIn("merchant", "USD"); got != 1000 {
		t.Errorf("merchant balance = %d, want 1000 (only the good transfer)", got)
	}
}

// Same-party, zero, negative, and currency-less transactions are
// refused per transaction after parsing; the good transaction in
// the same file still books.
func TestApplyPain001_PerItemRefusals(t *testing.T) {
	l := pain001InboundLedger(t)
	doc, err := ParsePain001([]byte(pain001Doc("001.03", "msg-001-ref",
		pain001PmtInf{txs: []pain001Tx{
			{endToEnd: "e-001-good", amount: "100.00", ccy: "USD"},
			{endToEnd: "e-001-same", amount: "10.00", ccy: "USD", cdtrIBAN: testIBANDE},
			{endToEnd: "e-001-zero", amount: "0.00", ccy: "USD"},
			{endToEnd: "e-001-neg", amount: "-5.00", ccy: "USD"},
			{endToEnd: "e-001-noccy", amount: "25.00", ccy: ""},
		}})))
	if err != nil {
		t.Fatalf("ParsePain001: %v", err)
	}
	res, err := l.ApplyPain001(doc, "ops")
	if err != nil {
		t.Fatalf("ApplyPain001: %v", err)
	}
	if len(res.Items) != 5 {
		t.Fatalf("items = %d, want 5", len(res.Items))
	}
	if res.Items[0].Err != nil {
		t.Errorf("good item err = %v", res.Items[0].Err)
	}
	wantErrs := []error{
		ErrTransferSameAccount, ErrTransferNonPositiveAmount,
		ErrTransferNonPositiveAmount, ErrPain001EmptyCurrency,
	}
	for i, want := range wantErrs {
		item := res.Items[i+1]
		if !errors.Is(item.Err, want) {
			t.Errorf("item %d err = %v, want %v", i+1, item.Err, want)
		}
		if item.RepairID == "" {
			t.Errorf("item %d has no repair record", i+1)
		}
		if _, ok := l.entries[item.Instruction.EndToEndID]; ok {
			t.Errorf("entry %q booked for a refused transaction", item.Instruction.EndToEndID)
		}
	}
	if got := l.BalanceIn("merchant", "USD"); got != 10000 {
		t.Errorf("merchant balance = %d, want 10000 (only the good transfer)", got)
	}
}

// A PmtInf dated tomorrow parks its transactions; a PmtInf dated
// today in the same file still books.
func TestApplyPain001_FutureExecutionDate(t *testing.T) {
	l := pain001InboundLedger(t)
	tomorrow := time.Now().UTC().Add(24 * time.Hour).Format("2006-01-02")
	doc, err := ParsePain001([]byte(pain001Doc("001.03", "msg-001-fut",
		pain001PmtInf{txs: []pain001Tx{{endToEnd: "e-001-today", amount: "10.00", ccy: "USD"}}},
		pain001PmtInf{exctnDt: tomorrow, txs: []pain001Tx{{endToEnd: "e-001-tomorrow", amount: "20.00", ccy: "USD"}}},
	)))
	if err != nil {
		t.Fatalf("ParsePain001: %v", err)
	}
	res, err := l.ApplyPain001(doc, "ops")
	if err != nil {
		t.Fatalf("ApplyPain001: %v", err)
	}
	if res.Items[0].Err != nil {
		t.Errorf("today item err = %v", res.Items[0].Err)
	}
	future := res.Items[1]
	if !errors.Is(future.Err, ErrPain001ExecutionDateNotToday) {
		t.Errorf("future item err = %v, want ErrPain001ExecutionDateNotToday", future.Err)
	}
	if future.RepairID == "" {
		t.Error("future item has no repair record")
	}
	if _, ok := l.entries["e-001-tomorrow"]; ok {
		t.Error("entry e-001-tomorrow booked for a future execution date")
	}
}

// A frozen payer is PostTransfer's gate: the transaction parks
// instead of booking.
func TestApplyPain001_FrozenPayer(t *testing.T) {
	l := pain001InboundLedger(t)
	l.Freeze("cust")
	doc, err := ParsePain001([]byte(pain001ValidDoc()))
	if err != nil {
		t.Fatalf("ParsePain001: %v", err)
	}
	res, err := l.ApplyPain001(doc, "ops")
	if err != nil {
		t.Fatalf("ApplyPain001: %v", err)
	}
	for i, item := range res.Items {
		if !errors.Is(item.Err, ErrAccountFrozen) {
			t.Errorf("item %d err = %v, want ErrAccountFrozen", i, item.Err)
		}
		if item.RepairID == "" {
			t.Errorf("item %d has no repair record", i)
		}
	}
	if len(l.entries) != 1 {
		t.Errorf("entries = %d, want 1 (only the funding post)", len(l.entries))
	}
}

func TestApplyPain001_Replay(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	if _, _, err := l.Post(JournalEntry{ID: "fund-cust", DebitAccount: "cust", CreditAccount: "bank", AmountCents: 1000000, Currency: "USD"}); err != nil {
		t.Fatalf("fund Post: %v", err)
	}
	mustSetIBAN(t, l, "cust", testIBANDE, "DEUTDEFF")
	mustSetIBAN(t, l, "merchant", testIBANGB, "NWBKGB2L")

	doc, err := ParsePain001([]byte(pain001ValidDoc()))
	if err != nil {
		t.Fatalf("ParsePain001: %v", err)
	}
	first, err := l.ApplyPain001(doc, "ops")
	if err != nil {
		t.Fatalf("first ApplyPain001: %v", err)
	}
	entriesBefore := len(l.entries)
	second, err := l.ApplyPain001(doc, "ops")
	if err != nil {
		t.Fatalf("replay ApplyPain001: %v", err)
	}
	if !second.Duplicate {
		t.Error("replay Duplicate = false, want true")
	}
	if len(second.Items) != 2 || second.Items[0].EntryID != first.Items[0].EntryID {
		t.Errorf("replay items = %+v, want the original receipts", second.Items)
	}
	if !second.Items[0].Duplicate {
		t.Error("replay item Duplicate = false, want true")
	}
	if len(l.entries) != entriesBefore {
		t.Errorf("entries = %d after replay, want %d (nothing new booked)", len(l.entries), entriesBefore)
	}
	events := readAuditOps(t, al)
	if countOp(events, "pain001_initiation_applied") != 1 {
		t.Errorf("pain001_initiation_applied count = %d, want 1 (no second event on replay)", countOp(events, "pain001_initiation_applied"))
	}
}

// A MsgId reused for different terms is a conflict, never a
// silent overwrite.
func TestApplyPain001_MsgIDConflict(t *testing.T) {
	l := pain001InboundLedger(t)
	doc, err := ParsePain001([]byte(pain001ValidDoc()))
	if err != nil {
		t.Fatalf("ParsePain001: %v", err)
	}
	if _, err := l.ApplyPain001(doc, "ops"); err != nil {
		t.Fatalf("first ApplyPain001: %v", err)
	}
	other, err := ParsePain001([]byte(pain001Doc("001.03", "msg-001-1",
		pain001PmtInf{txs: []pain001Tx{{endToEnd: "e-001-1", amount: "99.00", ccy: "USD"}}},
	)))
	if err != nil {
		t.Fatalf("ParsePain001: %v", err)
	}
	if _, err := l.ApplyPain001(other, "ops"); !errors.Is(err, ErrPain001MsgIDConflict) {
		t.Errorf("err = %v, want ErrPain001MsgIDConflict", err)
	}
}

// A MsgId that already names an exported instruction is a
// conflict: the export's record owns that MsgId.
func TestApplyPain001_ExportedMsgIDConflict(t *testing.T) {
	l := painStatusTestLedger(t)
	msgID := mustExportPain001MsgID(t, l, Pain001ExportOptions{})
	doc, err := ParsePain001([]byte(pain001Doc("001.03", msgID,
		pain001PmtInf{txs: []pain001Tx{{endToEnd: "e-001-conf", amount: "10.00", ccy: "USD"}}},
	)))
	if err != nil {
		t.Fatalf("ParsePain001: %v", err)
	}
	if _, err := l.ApplyPain001(doc, "ops"); !errors.Is(err, ErrPain001MsgIDConflict) {
		t.Errorf("err = %v, want ErrPain001MsgIDConflict", err)
	}
	if _, ok := l.entries["e-001-conf"]; ok {
		t.Error("entry e-001-conf booked under an exported MsgId")
	}
}

// Two transactions in one file sharing an EndToEndId: the second
// replays the first through the shared idempotency key instead of
// booking twice.
func TestApplyPain001_DuplicateEndToEndInOneFile(t *testing.T) {
	l := pain001InboundLedger(t)
	doc, err := ParsePain001([]byte(pain001Doc("001.03", "msg-001-dup",
		pain001PmtInf{txs: []pain001Tx{
			{endToEnd: "e-001-dup", amount: "10.00", ccy: "USD"},
			{endToEnd: "e-001-dup", amount: "10.00", ccy: "USD"},
		}})))
	if err != nil {
		t.Fatalf("ParsePain001: %v", err)
	}
	res, err := l.ApplyPain001(doc, "ops")
	if err != nil {
		t.Fatalf("ApplyPain001: %v", err)
	}
	if len(res.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(res.Items))
	}
	if res.Items[0].Err != nil {
		t.Fatalf("first item err = %v", res.Items[0].Err)
	}
	if !res.Items[1].Duplicate {
		t.Error("second item Duplicate = false, want true (same idempotency key)")
	}
	if res.Items[1].EntryID != res.Items[0].EntryID {
		t.Errorf("second entry_id = %q, want %q", res.Items[1].EntryID, res.Items[0].EntryID)
	}
	if got := l.BalanceIn("merchant", "USD"); got != 1000 {
		t.Errorf("merchant balance = %d, want 1000 (booked once)", got)
	}
}
