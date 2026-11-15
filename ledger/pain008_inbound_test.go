package ledger

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// pain008InboundLedger builds a ledger with a funded payer, IBANs
// with BICs on both sides, and one live per-day mandate (m-008)
// whose validity opens 48h ago: the inbound collection date is a
// UTC calendar date (midnight), so the window must already cover
// the start of today (the pacs010InboundLedger convention).
func pain008InboundLedger(t *testing.T) *Ledger {
	t.Helper()
	l := New()
	if _, _, err := l.Post(JournalEntry{ID: "fund-cust", DebitAccount: "cust", CreditAccount: "bank", AmountCents: 1000000, Currency: "USD"}); err != nil {
		t.Fatalf("fund Post: %v", err)
	}
	mustSetIBAN(t, l, "cust", testIBANDE, "DEUTDEFF")
	mustSetIBAN(t, l, "merchant", testIBANGB, "NWBKGB2L")
	now := time.Now()
	if _, err := l.CreateMandate(Mandate{
		ID:             "m-008",
		Payer:          "cust",
		Payee:          "merchant",
		MaxAmountCents: 500000,
		Currency:       "USD",
		Frequency:      MandateFrequencyPerDay,
		ValidFrom:      now.Add(-48 * time.Hour),
		ValidUntil:     now.Add(30 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("CreateMandate m-008: %v", err)
	}
	return l
}

// pain008Tx is one DrctDbtTxInf of a test document.
type pain008Tx struct {
	endToEnd  string
	amount    string
	ccy       string
	mandate   string
	dtOfSgntr string
	dbtrIBAN  string
	remit     string
}

// pain008PmtInf is one PmtInf of a test document: every transaction
// in it shares the sequence type, collection date, and creditor
// scheme, the SEPA grouping rule.
type pain008PmtInf struct {
	seqTp    string
	colltnDt string
	scheme   string
	cdtrIBAN string
	txs      []pain008Tx
}

// pain008Doc renders one inbound pain.008.001.02 document in the
// shape ExportPain008 renders: msgID in GrpHdr (NbOfTxs and
// CtrlSum computed from the transactions), one PmtInf per group
// (PmtMtd DD, PmtTpInf SEPA/CORE/SeqTp, ReqdColltnDt, Cdtr merchant,
// CdtrSchmeId), one DrctDbtTxInf per transaction (EndToEndId,
// InstdAmt, MndtRltdInf, Dbtr cust, RmtInf/Ustrd). Empty fields
// render as empty elements, so parse refusals can be driven per
// field. version selects the namespace (001.02 valid, anything
// else the wrong schema version).
func pain008Doc(version, msgID string, pmts ...pain008PmtInf) string {
	return pain008DocCtrl(version, msgID, "", -1, pmts...)
}

// pain008DocCtrl is pain008Doc with an explicit GrpHdr CtrlSum
// ("" computes it) and NbOfTxs (-1 computes it), so header
// disagreements can be driven.
func pain008DocCtrl(version, msgID, ctrlSum string, nbOfTxs int, pmts ...pain008PmtInf) string {
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pain.008.` + version + `">` + "\n")
	sb.WriteString("<CstmrDrctDbtInitn>\n<GrpHdr>\n")
	sb.WriteString("<MsgId>" + msgID + "</MsgId>\n")
	sb.WriteString("<CreDtTm>2026-11-14T10:00:00Z</CreDtTm>\n")
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
		cdtrIBAN := pmt.cdtrIBAN
		if cdtrIBAN == "" {
			cdtrIBAN = testIBANGB
		}
		sb.WriteString("<PmtInf>\n")
		sb.WriteString(fmt.Sprintf("<PmtInfId>%s-%s-%s</PmtInfId>\n", msgID, pmt.seqTp, pmt.colltnDt))
		sb.WriteString("<PmtMtd>DD</PmtMtd>\n")
		sb.WriteString(fmt.Sprintf("<NbOfTxs>%d</NbOfTxs>\n", len(pmt.txs)))
		sb.WriteString(fmt.Sprintf("<CtrlSum>%s</CtrlSum>\n", pain001Amount(pmtSum)))
		sb.WriteString("<PmtTpInf>\n")
		sb.WriteString("<SvcLvl><Cd>SEPA</Cd></SvcLvl>\n")
		sb.WriteString("<LclInstrm><Cd>CORE</Cd></LclInstrm>\n")
		sb.WriteString(fmt.Sprintf("<SeqTp>%s</SeqTp>\n", pmt.seqTp))
		sb.WriteString("</PmtTpInf>\n")
		sb.WriteString(fmt.Sprintf("<ReqdColltnDt>%s</ReqdColltnDt>\n", pmt.colltnDt))
		sb.WriteString("<Cdtr><Nm>merchant</Nm></Cdtr>\n")
		sb.WriteString(fmt.Sprintf("<CdtrAcct><Id><IBAN>%s</IBAN></Id></CdtrAcct>\n", cdtrIBAN))
		sb.WriteString("<CdtrAgt><FinInstnId><BIC>NWBKGB2L</BIC></FinInstnId></CdtrAgt>\n")
		sb.WriteString("<ChrgBr>SLEV</ChrgBr>\n")
		sb.WriteString(fmt.Sprintf("<CdtrSchmeId><Id><PrtryId><Id>%s</Id></PrtryId></Id></CdtrSchmeId>\n", pmt.scheme))
		for _, tx := range pmt.txs {
			dbtrIBAN := tx.dbtrIBAN
			if dbtrIBAN == "" {
				dbtrIBAN = testIBANDE
			}
			sb.WriteString("<DrctDbtTxInf>\n")
			sb.WriteString(fmt.Sprintf("<PmtId><InstrId>%s</InstrId><EndToEndId>%s</EndToEndId></PmtId>\n", tx.endToEnd, tx.endToEnd))
			sb.WriteString(fmt.Sprintf("<InstdAmt Ccy=\"%s\">%s</InstdAmt>\n", tx.ccy, tx.amount))
			sb.WriteString("<DrctDbtTx>\n<MndtRltdInf>\n")
			sb.WriteString(fmt.Sprintf("<MndtId>%s</MndtId>\n", tx.mandate))
			sb.WriteString(fmt.Sprintf("<DtOfSgntr>%s</DtOfSgntr>\n", tx.dtOfSgntr))
			sb.WriteString("<AmdmntInd>false</AmdmntInd>\n")
			sb.WriteString("</MndtRltdInf>\n</DrctDbtTx>\n")
			sb.WriteString("<DbtrAgt><FinInstnId><BIC>DEUTDEFF</BIC></FinInstnId></DbtrAgt>\n")
			sb.WriteString("<Dbtr><Nm>cust</Nm></Dbtr>\n")
			sb.WriteString(fmt.Sprintf("<DbtrAcct><Id><IBAN>%s</IBAN></Id></DbtrAcct>\n", dbtrIBAN))
			if tx.remit != "" {
				sb.WriteString(fmt.Sprintf("<RmtInf><Ustrd>%s</Ustrd></RmtInf>\n", tx.remit))
			}
			sb.WriteString("</DrctDbtTxInf>\n")
		}
		sb.WriteString("</PmtInf>\n")
	}
	sb.WriteString("</CstmrDrctDbtInitn>\n</Document>\n")
	return sb.String()
}

// pain008Today is today's UTC date, the only collection date the
// inbound intake accepts.
func pain008Today() string {
	return time.Now().UTC().Format("2006-01-02")
}

// pain008ValidDoc is the happy-path document: msg-008-1 collects
// USD 10.00 under m-008, first collection, today, scheme
// DE98ZZZ09999999999, remittance "invoice 42".
func pain008ValidDoc() string {
	return pain008Doc("001.02", "msg-008-1",
		pain008PmtInf{
			seqTp: "FRST", colltnDt: pain008Today(), scheme: "DE98ZZZ09999999999",
			txs: []pain008Tx{{
				endToEnd: "e-008-1", amount: "10.00", ccy: "USD",
				mandate: "m-008", dtOfSgntr: "2026-01-01", remit: "invoice 42",
			}},
		})
}

func TestParsePain008_Valid(t *testing.T) {
	doc, err := ParsePain008([]byte(pain008ValidDoc()))
	if err != nil {
		t.Fatalf("ParsePain008: %v", err)
	}
	if doc.MsgID != "msg-008-1" {
		t.Errorf("MsgID = %q, want msg-008-1", doc.MsgID)
	}
	if doc.TxCount != 1 || doc.CtrlSumCents != 1000 {
		t.Errorf("tx_count/ctrl_sum = %d/%d, want 1/1000", doc.TxCount, doc.CtrlSumCents)
	}
	if len(doc.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(doc.Items))
	}
	d := doc.Items[0]
	if d.EndToEndID != "e-008-1" || d.InstrID != "e-008-1" {
		t.Errorf("ids = %q/%q, want e-008-1/e-008-1", d.EndToEndID, d.InstrID)
	}
	if d.AmountCents != 1000 || d.Currency != "USD" {
		t.Errorf("amount = %d %q, want 1000 USD", d.AmountCents, d.Currency)
	}
	if d.MandateID != "m-008" || d.SeqTp != "FRST" {
		t.Errorf("mandate/seq = %q/%q, want m-008/FRST", d.MandateID, d.SeqTp)
	}
	if d.ReqdColltnDt.UTC().Format("2006-01-02") != pain008Today() {
		t.Errorf("collection date = %v, want today %s", d.ReqdColltnDt, pain008Today())
	}
	if d.CreditorSchemeID != "DE98ZZZ09999999999" {
		t.Errorf("scheme = %q, want DE98ZZZ09999999999", d.CreditorSchemeID)
	}
	if d.CreditorIBAN != testIBANGB || d.DebtorIBAN != testIBANDE {
		t.Errorf("IBANs = %q/%q, want %q/%q", d.CreditorIBAN, d.DebtorIBAN, testIBANGB, testIBANDE)
	}
	if d.Remittance != "invoice 42" || d.DtOfSgntr != "2026-01-01" {
		t.Errorf("remittance/signature = %q/%q, want invoice 42/2026-01-01", d.Remittance, d.DtOfSgntr)
	}
	if d.ServiceLevel != "SEPA" || d.LocalInstrument != "CORE" {
		t.Errorf("svcLvl/lclInstrm = %q/%q, want SEPA/CORE", d.ServiceLevel, d.LocalInstrument)
	}
}

// ExportPain008 renders a document ParsePain008 accepts: the
// inbound parser is the export's mirror, so an export round-trips
// through the parse (parse only — applying an export would
// re-book the same collections under new idempotency keys).
func TestParsePain008_ExportRoundTrip(t *testing.T) {
	l, _ := pain008StatusFixture(t)

	doc, err := l.ExportPain008("m-008", pain008Opts())
	if err != nil {
		t.Fatalf("ExportPain008: %v", err)
	}
	parsed, err := ParsePain008(doc)
	if err != nil {
		t.Fatalf("ParsePain008(export): %v", err)
	}
	if len(parsed.Items) != 2 {
		t.Fatalf("items = %d, want 2 (d-1, d-2)", len(parsed.Items))
	}
	ids := map[string]bool{}
	for _, d := range parsed.Items {
		ids[d.EndToEndID] = true
		if d.MandateID != "m-008" {
			t.Errorf("%s mandate = %q, want m-008", d.EndToEndID, d.MandateID)
		}
		if d.CreditorSchemeID != pain008Opts().CreditorSchemeID {
			t.Errorf("%s scheme = %q, want %q", d.EndToEndID, d.CreditorSchemeID, pain008Opts().CreditorSchemeID)
		}
	}
	if !ids["d-1"] || !ids["d-2"] {
		t.Errorf("entry ids = %v, want d-1 and d-2", ids)
	}
}

func TestParsePain008_WrongNamespace(t *testing.T) {
	// pain.008.001.01 is a real registry version, but the ledger
	// only speaks .001.02: the namespace gate refuses it, and the
	// file still parks in the repair queue under its MsgId.
	bad := pain008Doc("001.01", "msg-008-ns",
		pain008PmtInf{
			seqTp: "FRST", colltnDt: pain008Today(), scheme: "DE98ZZZ09999999999",
			txs: []pain008Tx{{
				endToEnd: "e-008-ns", amount: "10.00", ccy: "USD",
				mandate: "m-008", dtOfSgntr: "2026-01-01",
			}},
		})
	if _, err := ParsePain008([]byte(bad)); !errors.Is(err, ErrInvalidPain008) {
		t.Errorf("wrong namespace: err = %v, want ErrInvalidPain008", err)
	}
	if _, err := ParsePain008([]byte("not xml at all")); !errors.Is(err, ErrInvalidPain008) {
		t.Errorf("garbage: err = %v, want ErrInvalidPain008", err)
	}
	l := pain008InboundLedger(t)
	repairID, err := l.ParkPain008Unparsed([]byte(bad), "ops")
	if err != nil {
		t.Fatalf("ParkPain008Unparsed: %v", err)
	}
	if repairID == "" {
		t.Error("ParkPain008Unparsed returned no repair ID for a names MsgId")
	}
	if _, ok := l.GetRepair(repairID); !ok {
		t.Errorf("repair record %q not in queue", repairID)
	}
	if repairID, err := l.ParkPain008Unparsed([]byte("not xml at all"), "ops"); err != nil || repairID != "" {
		t.Errorf("ParkPain008Unparsed(garbage) = %q, %v, want \"\", nil", repairID, err)
	}
}

func TestParsePain008_CtrlSumMismatch(t *testing.T) {
	// The GrpHdr CtrlSum disagrees with the instructed total:
	// the whole document is refused, never applied on trust.
	bad := pain008DocCtrl("001.02", "msg-008-sum", "999.99", -1,
		pain008PmtInf{
			seqTp: "FRST", colltnDt: pain008Today(), scheme: "DE98ZZZ09999999999",
			txs: []pain008Tx{{
				endToEnd: "e-008-sum", amount: "10.00", ccy: "USD",
				mandate: "m-008", dtOfSgntr: "2026-01-01",
			}},
		})
	if _, err := ParsePain008([]byte(bad)); !errors.Is(err, ErrInvalidPain008) {
		t.Errorf("CtrlSum mismatch: err = %v, want ErrInvalidPain008", err)
	}
	// A NbOfTxs that disagrees with the transaction count is
	// refused the same way.
	bad = pain008DocCtrl("001.02", "msg-008-nb", "", 2,
		pain008PmtInf{
			seqTp: "FRST", colltnDt: pain008Today(), scheme: "DE98ZZZ09999999999",
			txs: []pain008Tx{{
				endToEnd: "e-008-nb", amount: "10.00", ccy: "USD",
				mandate: "m-008", dtOfSgntr: "2026-01-01",
			}},
		})
	if _, err := ParsePain008([]byte(bad)); !errors.Is(err, ErrInvalidPain008) {
		t.Errorf("NbOfTxs mismatch: err = %v, want ErrInvalidPain008", err)
	}
	// A SeqTp outside the vocabulary is a parse refusal, not a
	// per-transaction repair: the document is not a valid
	// pain.008 at all.
	bad = pain008Doc("001.02", "msg-008-seq",
		pain008PmtInf{
			seqTp: "XXXX", colltnDt: pain008Today(), scheme: "DE98ZZZ09999999999",
			txs: []pain008Tx{{
				endToEnd: "e-008-seq", amount: "10.00", ccy: "USD",
				mandate: "m-008", dtOfSgntr: "2026-01-01",
			}},
		})
	if _, err := ParsePain008([]byte(bad)); !errors.Is(err, ErrInvalidPain008) {
		t.Errorf("SeqTp outside vocabulary: err = %v, want ErrInvalidPain008", err)
	}
	// A missing creditor scheme ID is a parse refusal: SEPA
	// mandates CdtrSchmeId on every pain.008.
	bad = pain008Doc("001.02", "msg-008-scheme",
		pain008PmtInf{
			seqTp: "FRST", colltnDt: pain008Today(), scheme: "",
			txs: []pain008Tx{{
				endToEnd: "e-008-scheme", amount: "10.00", ccy: "USD",
				mandate: "m-008", dtOfSgntr: "2026-01-01",
			}},
		})
	if _, err := ParsePain008([]byte(bad)); !errors.Is(err, ErrInvalidPain008) {
		t.Errorf("missing scheme: err = %v, want ErrInvalidPain008", err)
	}
}

func TestApplyPain008_Success(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	// The fixture ledger, rebuilt on the audited ledger: same
	// shape as pain008InboundLedger (funded payer, IBANs, live
	// per-day mandate m-008).
	if _, _, err := l.Post(JournalEntry{ID: "fund-cust", DebitAccount: "cust", CreditAccount: "bank", AmountCents: 1000000, Currency: "USD"}); err != nil {
		t.Fatalf("fund Post: %v", err)
	}
	mustSetIBAN(t, l, "cust", testIBANDE, "DEUTDEFF")
	mustSetIBAN(t, l, "merchant", testIBANGB, "NWBKGB2L")
	now := time.Now()
	if _, err := l.CreateMandate(Mandate{
		ID: "m-008", Payer: "cust", Payee: "merchant",
		MaxAmountCents: 500000, Currency: "USD",
		Frequency:  MandateFrequencyPerDay,
		ValidFrom:  now.Add(-48 * time.Hour),
		ValidUntil: now.Add(30 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("CreateMandate: %v", err)
	}

	doc, err := ParsePain008([]byte(pain008ValidDoc()))
	if err != nil {
		t.Fatalf("ParsePain008: %v", err)
	}
	res, err := l.ApplyPain008(doc, "ops")
	if err != nil {
		t.Fatalf("ApplyPain008: %v", err)
	}
	if res.Duplicate {
		t.Error("Duplicate = true on first apply, want false")
	}
	if len(res.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(res.Items))
	}
	item := res.Items[0]
	if item.Err != nil {
		t.Fatalf("item err = %v", item.Err)
	}
	if item.EntryID != "e-008-1" {
		t.Errorf("entry_id = %q, want e-008-1", item.EntryID)
	}
	if item.Receipt.Entry.AmountCents != 1000 || item.Receipt.Entry.Currency != "USD" {
		t.Errorf("booked = %d %q, want 1000 USD", item.Receipt.Entry.AmountCents, item.Receipt.Entry.Currency)
	}
	if item.Receipt.Entry.CreatedAt.UTC().Format("2006-01-02") != pain008Today() {
		t.Errorf("booked at %v, want today UTC", item.Receipt.Entry.CreatedAt)
	}
	// The instruction lifecycle record is registered under the
	// wire MsgId, in exported state, so pain.002 can answer it.
	rec, ok := l.Pain008Instruction("msg-008-1")
	if !ok {
		t.Fatal("Pain008Instruction(msg-008-1): not registered")
	}
	if rec.State != Pain008StateExported {
		t.Errorf("state = %q, want exported", rec.State)
	}
	if rec.MandateID != "m-008" || rec.TxCount != 1 || rec.CtrlSum != 1000 || rec.Currency != "USD" {
		t.Errorf("instruction = %q/%d/%d/%q, want m-008/1/1000/USD", rec.MandateID, rec.TxCount, rec.CtrlSum, rec.Currency)
	}
	if len(rec.Txs) != 1 || rec.Txs[0].EntryID != "e-008-1" {
		t.Errorf("instruction txs = %+v, want one entry e-008-1", rec.Txs)
	}
	// The batch audit event, with the scheme value verbatim.
	events := readAuditOps(t, al)
	ev := findOp(events, "pain008_initiation_applied")
	if ev == nil {
		t.Fatal("pain008_initiation_applied audit event missing")
	}
	if countOp(events, "pain008_initiation_applied") != 1 {
		t.Errorf("pain008_initiation_applied count = %d, want 1", countOp(events, "pain008_initiation_applied"))
	}
	for k, want := range map[string]any{
		"msg_id": "msg-008-1", "mandate_id": "m-008",
		"tx_count": float64(1), "ctrl_sum_cents": float64(1000),
		"currency": "USD", "creditor_scheme_id": "DE98ZZZ09999999999",
	} {
		if ev.Details[k] != want {
			t.Errorf("audit %s = %v, want %v", k, ev.Details[k], want)
		}
	}
}

// A file naming two mandates parks whole in the repair queue:
// the intake is single-mandate, like the export, and is never
// silently split into batches.
func TestApplyPain008_MultiMandate(t *testing.T) {
	l := pain008InboundLedger(t)
	// A second mandate for the second transaction.
	now := time.Now()
	if _, err := l.CreateMandate(Mandate{
		ID: "m-008b", Payer: "cust", Payee: "merchant",
		MaxAmountCents: 500000, Currency: "USD",
		Frequency:  MandateFrequencyPerDay,
		ValidFrom:  now.Add(-48 * time.Hour),
		ValidUntil: now.Add(30 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("CreateMandate m-008b: %v", err)
	}
	doc, err := ParsePain008([]byte(pain008Doc("001.02", "msg-008-multi",
		pain008PmtInf{
			seqTp: "FRST", colltnDt: pain008Today(), scheme: "DE98ZZZ09999999999",
			txs: []pain008Tx{{
				endToEnd: "e-008-m1", amount: "10.00", ccy: "USD",
				mandate: "m-008", dtOfSgntr: "2026-01-01",
			}},
		},
		pain008PmtInf{
			seqTp: "FRST", colltnDt: pain008Today(), scheme: "DE98ZZZ09999999999",
			txs: []pain008Tx{{
				endToEnd: "e-008-m2", amount: "20.00", ccy: "USD",
				mandate: "m-008b", dtOfSgntr: "2026-01-01",
			}},
		})))
	if err != nil {
		t.Fatalf("ParsePain008: %v", err)
	}
	res, err := l.ApplyPain008(doc, "ops")
	if !errors.Is(err, ErrPain008MultiMandate) {
		t.Fatalf("err = %v, want ErrPain008MultiMandate", err)
	}
	if res.RepairID == "" {
		t.Error("multi-mandate file: no repair ID")
	}
	if _, ok := l.GetRepair(res.RepairID); !ok {
		t.Errorf("repair record %q not in queue", res.RepairID)
	}
	// Nothing booked: neither transaction's entry exists.
	for _, id := range []string{"e-008-m1", "e-008-m2"} {
		if _, ok := l.entries[id]; ok {
			t.Errorf("entry %q booked from a multi-mandate file, want nothing booked", id)
		}
	}
	if _, ok := l.Pain008Instruction("msg-008-multi"); ok {
		t.Error("multi-mandate file registered an instruction, want none")
	}
}

// An unknown mandate parks that transaction in the repair queue
// and books nothing for it.
func TestApplyPain008_UnknownMandate(t *testing.T) {
	l := pain008InboundLedger(t)
	doc, err := ParsePain008([]byte(pain008Doc("001.02", "msg-008-unk",
		pain008PmtInf{
			seqTp: "FRST", colltnDt: pain008Today(), scheme: "DE98ZZZ09999999999",
			txs: []pain008Tx{{
				endToEnd: "e-008-unk", amount: "10.00", ccy: "USD",
				mandate: "m-nope", dtOfSgntr: "2026-01-01",
			}},
		})))
	if err != nil {
		t.Fatalf("ParsePain008: %v", err)
	}
	res, err := l.ApplyPain008(doc, "ops")
	if err != nil {
		t.Fatalf("ApplyPain008: %v", err)
	}
	if len(res.Items) != 1 || res.Items[0].Err == nil {
		t.Fatalf("items = %+v, want one error", res.Items)
	}
	if !errors.Is(res.Items[0].Err, ErrMandateNotFound) {
		t.Errorf("item err = %v, want ErrMandateNotFound", res.Items[0].Err)
	}
	if res.Items[0].RepairID == "" {
		t.Error("unknown mandate: no repair ID")
	}
	if _, ok := l.GetRepair(res.Items[0].RepairID); !ok {
		t.Errorf("repair record %q not in queue", res.Items[0].RepairID)
	}
	if _, ok := l.entries["e-008-unk"]; ok {
		t.Error("entry e-008-unk booked under an unknown mandate, want nothing booked")
	}
}

// A suspended mandate is refused by DebitWithMandate's own gate
// and parks in the repair queue.
func TestApplyPain008_SuspendedMandate(t *testing.T) {
	l := pain008InboundLedger(t)
	if _, err := l.SuspendMandate("m-008"); err != nil {
		t.Fatalf("SuspendMandate: %v", err)
	}
	doc, err := ParsePain008([]byte(pain008ValidDoc()))
	if err != nil {
		t.Fatalf("ParsePain008: %v", err)
	}
	res, err := l.ApplyPain008(doc, "ops")
	if err != nil {
		t.Fatalf("ApplyPain008: %v", err)
	}
	if len(res.Items) != 1 || res.Items[0].Err == nil {
		t.Fatalf("items = %+v, want one error", res.Items)
	}
	if !errors.Is(res.Items[0].Err, ErrMandateNotActive) {
		t.Errorf("item err = %v, want ErrMandateNotActive", res.Items[0].Err)
	}
	if res.Items[0].RepairID == "" {
		t.Error("suspended mandate: no repair ID")
	}
	if _, ok := l.entries["e-008-1"]; ok {
		t.Error("entry e-008-1 booked under a suspended mandate, want nothing booked")
	}
}

// The wire SeqTp must match the derivation from the mandate's
// debit history: a first-collection wire value on a mandate that
// already collected parks in the repair queue, never silently
// rewritten.
func TestApplyPain008_SeqTpMismatch(t *testing.T) {
	l := pain008InboundLedger(t)
	// One prior collection, yesterday, so today derives RCUR.
	yesterday := time.Now().Add(-24 * time.Hour)
	if _, err := l.DebitWithMandate("m-008", MandateDebit{
		ID: "prior-1", AmountCents: 500, CreatedAt: yesterday,
	}); err != nil {
		t.Fatalf("prior DebitWithMandate: %v", err)
	}
	doc, err := ParsePain008([]byte(pain008Doc("001.02", "msg-008-seq2",
		pain008PmtInf{
			seqTp: "FRST", colltnDt: pain008Today(), scheme: "DE98ZZZ09999999999",
			txs: []pain008Tx{{
				endToEnd: "e-008-seq2", amount: "10.00", ccy: "USD",
				mandate: "m-008", dtOfSgntr: "2026-01-01",
			}},
		})))
	if err != nil {
		t.Fatalf("ParsePain008: %v", err)
	}
	res, err := l.ApplyPain008(doc, "ops")
	if err != nil {
		t.Fatalf("ApplyPain008: %v", err)
	}
	if len(res.Items) != 1 || res.Items[0].Err == nil {
		t.Fatalf("items = %+v, want one error", res.Items)
	}
	if !errors.Is(res.Items[0].Err, ErrPain008SeqTpMismatch) {
		t.Errorf("item err = %v, want ErrPain008SeqTpMismatch", res.Items[0].Err)
	}
	if res.Items[0].RepairID == "" {
		t.Error("SeqTp mismatch: no repair ID")
	}
	if _, ok := l.entries["e-008-seq2"]; ok {
		t.Error("entry e-008-seq2 booked with a mismatched SeqTp, want nothing booked")
	}
	// The same file with the derived value books fine.
	doc, err = ParsePain008([]byte(pain008Doc("001.02", "msg-008-seq3",
		pain008PmtInf{
			seqTp: "RCUR", colltnDt: pain008Today(), scheme: "DE98ZZZ09999999999",
			txs: []pain008Tx{{
				endToEnd: "e-008-seq3", amount: "10.00", ccy: "USD",
				mandate: "m-008", dtOfSgntr: "2026-01-01",
			}},
		})))
	if err != nil {
		t.Fatalf("ParsePain008: %v", err)
	}
	res, err = l.ApplyPain008(doc, "ops")
	if err != nil {
		t.Fatalf("ApplyPain008: %v", err)
	}
	if len(res.Items) != 1 || res.Items[0].Err != nil {
		t.Fatalf("items = %+v, want one success", res.Items)
	}
	if res.Items[0].EntryID != "e-008-seq3" {
		t.Errorf("entry_id = %q, want e-008-seq3", res.Items[0].EntryID)
	}
}

// The wire IBANs must equal the mandate's registered IBANs, on
// both sides: a debtor IBAN that is not the payer's, and a
// creditor IBAN that is not the payee's, each park in the repair
// queue.
func TestApplyPain008_IBANMismatch(t *testing.T) {
	l := pain008InboundLedger(t)
	// Debtor side: the wire names the merchant's IBAN for the
	// debtor, which is not the payer's registered IBAN.
	doc, err := ParsePain008([]byte(pain008Doc("001.02", "msg-008-iban1",
		pain008PmtInf{
			seqTp: "FRST", colltnDt: pain008Today(), scheme: "DE98ZZZ09999999999",
			txs: []pain008Tx{{
				endToEnd: "e-008-iban1", amount: "10.00", ccy: "USD",
				mandate: "m-008", dtOfSgntr: "2026-01-01",
				dbtrIBAN: testIBANGB,
			}},
		})))
	if err != nil {
		t.Fatalf("ParsePain008: %v", err)
	}
	res, err := l.ApplyPain008(doc, "ops")
	if err != nil {
		t.Fatalf("ApplyPain008: %v", err)
	}
	if len(res.Items) != 1 || !errors.Is(res.Items[0].Err, ErrPain008IBANMismatch) {
		t.Fatalf("items = %+v, want ErrPain008IBANMismatch", res.Items)
	}
	if res.Items[0].RepairID == "" {
		t.Error("debtor IBAN mismatch: no repair ID")
	}
	// Creditor side: the wire names the customer's IBAN for the
	// creditor, which is not the payee's registered IBAN.
	doc, err = ParsePain008([]byte(pain008Doc("001.02", "msg-008-iban2",
		pain008PmtInf{
			seqTp: "FRST", colltnDt: pain008Today(), scheme: "DE98ZZZ09999999999",
			cdtrIBAN: testIBANDE,
			txs: []pain008Tx{{
				endToEnd: "e-008-iban2", amount: "10.00", ccy: "USD",
				mandate: "m-008", dtOfSgntr: "2026-01-01",
			}},
		})))
	if err != nil {
		t.Fatalf("ParsePain008: %v", err)
	}
	res, err = l.ApplyPain008(doc, "ops")
	if err != nil {
		t.Fatalf("ApplyPain008: %v", err)
	}
	if len(res.Items) != 1 || !errors.Is(res.Items[0].Err, ErrPain008IBANMismatch) {
		t.Fatalf("items = %+v, want ErrPain008IBANMismatch", res.Items)
	}
	if res.Items[0].RepairID == "" {
		t.Error("creditor IBAN mismatch: no repair ID")
	}
	if _, ok := l.entries["e-008-iban1"]; ok {
		t.Error("entry e-008-iban1 booked with a mismatched debtor IBAN")
	}
	if _, ok := l.entries["e-008-iban2"]; ok {
		t.Error("entry e-008-iban2 booked with a mismatched creditor IBAN")
	}
}

// A collection date that is not today parks in the repair queue:
// the ledger has no future-date collection scheduling.
func TestApplyPain008_FutureCollectionDate(t *testing.T) {
	l := pain008InboundLedger(t)
	tomorrow := time.Now().UTC().Add(24 * time.Hour).Format("2006-01-02")
	doc, err := ParsePain008([]byte(pain008Doc("001.02", "msg-008-future",
		pain008PmtInf{
			seqTp: "FRST", colltnDt: tomorrow, scheme: "DE98ZZZ09999999999",
			txs: []pain008Tx{{
				endToEnd: "e-008-future", amount: "10.00", ccy: "USD",
				mandate: "m-008", dtOfSgntr: "2026-01-01",
			}},
		})))
	if err != nil {
		t.Fatalf("ParsePain008: %v", err)
	}
	res, err := l.ApplyPain008(doc, "ops")
	if err != nil {
		t.Fatalf("ApplyPain008: %v", err)
	}
	if len(res.Items) != 1 || !errors.Is(res.Items[0].Err, ErrPain008CollectionDateNotToday) {
		t.Fatalf("items = %+v, want ErrPain008CollectionDateNotToday", res.Items)
	}
	if res.Items[0].RepairID == "" {
		t.Error("future collection date: no repair ID")
	}
	if _, ok := l.entries["e-008-future"]; ok {
		t.Error("entry e-008-future booked for a future collection date")
	}
}

// One bad transaction never blocks the others: the first books
// (FRST), the second — same mandate, same day, correctly wired
// RCUR — hits the mandate's per-day frequency window and parks,
// while the first stays booked.
func TestApplyPain008_PartialSuccess(t *testing.T) {
	l := pain008InboundLedger(t)
	doc, err := ParsePain008([]byte(pain008Doc("001.02", "msg-008-partial",
		pain008PmtInf{
			seqTp: "FRST", colltnDt: pain008Today(), scheme: "DE98ZZZ09999999999",
			txs: []pain008Tx{{
				endToEnd: "e-008-p1", amount: "10.00", ccy: "USD",
				mandate: "m-008", dtOfSgntr: "2026-01-01",
			}},
		},
		pain008PmtInf{
			seqTp: "RCUR", colltnDt: pain008Today(), scheme: "DE98ZZZ09999999999",
			txs: []pain008Tx{{
				endToEnd: "e-008-p2", amount: "20.00", ccy: "USD",
				mandate: "m-008", dtOfSgntr: "2026-01-01",
			}},
		})))
	if err != nil {
		t.Fatalf("ParsePain008: %v", err)
	}
	res, err := l.ApplyPain008(doc, "ops")
	if err != nil {
		t.Fatalf("ApplyPain008: %v", err)
	}
	if len(res.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(res.Items))
	}
	if res.Items[0].Err != nil || res.Items[0].EntryID != "e-008-p1" {
		t.Errorf("first item = %+v, want booked e-008-p1", res.Items[0])
	}
	if !errors.Is(res.Items[1].Err, ErrMandateFrequencyExceeded) {
		t.Errorf("second item err = %v, want ErrMandateFrequencyExceeded", res.Items[1].Err)
	}
	if res.Items[1].RepairID == "" {
		t.Error("second item: no repair ID")
	}
	// Only the first collection registered in the instruction.
	rec, ok := l.Pain008Instruction("msg-008-partial")
	if !ok {
		t.Fatal("Pain008Instruction(msg-008-partial): not registered")
	}
	if rec.TxCount != 1 || rec.CtrlSum != 1000 {
		t.Errorf("instruction tx_count/ctrl_sum = %d/%d, want 1/1000", rec.TxCount, rec.CtrlSum)
	}
}

// The pain.002 answer path (LG-406) answers an inbound file: the
// wire MsgId is the OrgnlMsgId and each EndToEndId is a booked
// entry ID. The wire MsgId here carries the "pain008-" prefix, so
// the report also passes ParsePain002's family gate, which pairs
// the pain.008 name with that MsgId namespace (see the package
// comment): the full parse path answers it end to end.
func TestApplyPain008_Pain002Answers(t *testing.T) {
	l := pain008InboundLedger(t)
	doc, err := ParsePain008([]byte(pain008Doc("001.02", "pain008-in-008-1",
		pain008PmtInf{
			seqTp: "FRST", colltnDt: pain008Today(), scheme: "DE98ZZZ09999999999",
			txs: []pain008Tx{{
				endToEnd: "e-008-1", amount: "10.00", ccy: "USD",
				mandate: "m-008", dtOfSgntr: "2026-01-01",
			}},
		})))
	if err != nil {
		t.Fatalf("ParsePain008: %v", err)
	}
	if _, err := l.ApplyPain008(doc, "ops"); err != nil {
		t.Fatalf("ApplyPain008: %v", err)
	}
	reportDoc := pain002TestDocNm(t, "pain008-in-008-1", "pain.008.001.02",
		pain002TxFrag("e-008-1", "ACSC", "", ""),
	)
	report, err := ParsePain002(reportDoc)
	if err != nil {
		t.Fatalf("ParsePain002: %v", err)
	}
	rec, err := l.ApplyPain002ToPain008(report, "bank")
	if err != nil {
		t.Fatalf("ApplyPain002ToPain008: %v", err)
	}
	if rec.State != Pain008StateAccepted {
		t.Errorf("state = %q, want accepted", rec.State)
	}
	if len(rec.Txs) != 1 || rec.Txs[0].State != Pain008StateAccepted {
		t.Errorf("txs = %+v, want one accepted entry", rec.Txs)
	}
}

// Replaying the same MsgId returns the original receipts with
// Duplicate set, books nothing new, and emits no second batch
// audit event.
func TestApplyPain008_Replay(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	if _, _, err := l.Post(JournalEntry{ID: "fund-cust", DebitAccount: "cust", CreditAccount: "bank", AmountCents: 1000000, Currency: "USD"}); err != nil {
		t.Fatalf("fund Post: %v", err)
	}
	mustSetIBAN(t, l, "cust", testIBANDE, "DEUTDEFF")
	mustSetIBAN(t, l, "merchant", testIBANGB, "NWBKGB2L")
	now := time.Now()
	if _, err := l.CreateMandate(Mandate{
		ID: "m-008", Payer: "cust", Payee: "merchant",
		MaxAmountCents: 500000, Currency: "USD",
		Frequency:  MandateFrequencyPerDay,
		ValidFrom:  now.Add(-48 * time.Hour),
		ValidUntil: now.Add(30 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("CreateMandate: %v", err)
	}
	doc, err := ParsePain008([]byte(pain008ValidDoc()))
	if err != nil {
		t.Fatalf("ParsePain008: %v", err)
	}
	first, err := l.ApplyPain008(doc, "ops")
	if err != nil {
		t.Fatalf("first ApplyPain008: %v", err)
	}
	entriesBefore := len(l.entries)
	second, err := l.ApplyPain008(doc, "ops")
	if err != nil {
		t.Fatalf("replay ApplyPain008: %v", err)
	}
	if !second.Duplicate {
		t.Error("replay Duplicate = false, want true")
	}
	if len(second.Items) != 1 || second.Items[0].EntryID != first.Items[0].EntryID {
		t.Errorf("replay items = %+v, want the original receipt e-008-1", second.Items)
	}
	if !second.Items[0].Duplicate {
		t.Error("replay item Duplicate = false, want true")
	}
	if len(l.entries) != entriesBefore {
		t.Errorf("entries = %d after replay, want %d (nothing new booked)", len(l.entries), entriesBefore)
	}
	events := readAuditOps(t, al)
	if countOp(events, "pain008_initiation_applied") != 1 {
		t.Errorf("pain008_initiation_applied count = %d, want 1 (no second event on replay)", countOp(events, "pain008_initiation_applied"))
	}
}

// A MsgId reused for different terms is a conflict, never a
// silent overwrite.
func TestApplyPain008_MsgIDConflict(t *testing.T) {
	l := pain008InboundLedger(t)
	doc, err := ParsePain008([]byte(pain008ValidDoc()))
	if err != nil {
		t.Fatalf("ParsePain008: %v", err)
	}
	if _, err := l.ApplyPain008(doc, "ops"); err != nil {
		t.Fatalf("first ApplyPain008: %v", err)
	}
	// Same MsgId, a different amount.
	other, err := ParsePain008([]byte(pain008Doc("001.02", "msg-008-1",
		pain008PmtInf{
			seqTp: "FRST", colltnDt: pain008Today(), scheme: "DE98ZZZ09999999999",
			txs: []pain008Tx{{
				endToEnd: "e-008-1", amount: "99.00", ccy: "USD",
				mandate: "m-008", dtOfSgntr: "2026-01-01",
			}},
		})))
	if err != nil {
		t.Fatalf("ParsePain008: %v", err)
	}
	if _, err := l.ApplyPain008(other, "ops"); !errors.Is(err, ErrPain008MsgIDConflict) {
		t.Errorf("err = %v, want ErrPain008MsgIDConflict", err)
	}
}

// A MsgId that already names an exported instruction is a
// conflict: the export's record owns that MsgId.
func TestApplyPain008_ExportedMsgIDConflict(t *testing.T) {
	l, _ := pain008StatusFixture(t)
	msgID := mustExportPain008MsgID(t, l, pain008Opts())
	doc, err := ParsePain008([]byte(pain008Doc("001.02", msgID,
		pain008PmtInf{
			seqTp: "FRST", colltnDt: pain008Today(), scheme: "DE98ZZZ09999999999",
			txs: []pain008Tx{{
				endToEnd: "e-008-conf", amount: "10.00", ccy: "USD",
				mandate: "m-008", dtOfSgntr: "2026-01-01",
			}},
		})))
	if err != nil {
		t.Fatalf("ParsePain008: %v", err)
	}
	if _, err := l.ApplyPain008(doc, "ops"); !errors.Is(err, ErrPain008MsgIDConflict) {
		t.Errorf("err = %v, want ErrPain008MsgIDConflict", err)
	}
	if _, ok := l.entries["e-008-conf"]; ok {
		t.Error("entry e-008-conf booked under an exported MsgId")
	}
}
