package ledger

import (
	"strings"
	"sync"
	"testing"
	"time"
)

const testPacs008Doc = `<?xml version="1.0" encoding="UTF-8"?>
<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pacs.008.001.08">
  <FIToFICstmrCdtTrf>
    <GrpHdr>
      <MsgId>MSG-FEDNOW-001</MsgId>
      <CreDtTm>2026-10-21T10:00:00Z</CreDtTm>
      <NbOfTxs>2</NbOfTxs>
      <SttlmInf><SttlmMtd>INDA</SttlmMtd></SttlmInf>
    </GrpHdr>
    <CdtTrfTxInf>
      <PmtId>
        <EndToEndId>E2E-INSTANT-001</EndToEndId>
        <TxId>TX-FEDNOW-001</TxId>
      </PmtId>
      <IntrBkSttlmAmt Ccy="USD">250.00</IntrBkSttlmAmt>
      <Dbtr><Nm>Sender Corp</Nm></Dbtr>
      <DbtrAcct><Id><Othr><Id>sender-acct-9</Id></Othr></Id></DbtrAcct>
      <Cdtr><Nm>Receiver LLC</Nm></Cdtr>
      <CdtrAcct><Id><Othr><Id>acct-payee</Id></Othr></Id></CdtrAcct>
      <RmtInf><Ustrd>Invoice 4242</Ustrd></RmtInf>
    </CdtTrfTxInf>
    <CdtTrfTxInf>
      <PmtId>
        <EndToEndId>E2E-INSTANT-002</EndToEndId>
      </PmtId>
      <IntrBkSttlmAmt Ccy="USD">99.99</IntrBkSttlmAmt>
      <CdtrAcct><Id><IBAN>acct-payee-iban</IBAN></Id></CdtrAcct>
    </CdtTrfTxInf>
  </FIToFICstmrCdtTrf>
</Document>`

func TestParsePacs008(t *testing.T) {
	txs, err := ParsePacs008([]byte(testPacs008Doc))
	if err != nil {
		t.Fatalf("ParsePacs008: %v", err)
	}
	if len(txs) != 2 {
		t.Fatalf("got %d transactions, want 2", len(txs))
	}
	first := txs[0]
	if first.EndToEndID != "E2E-INSTANT-001" {
		t.Errorf("EndToEndID = %q, want E2E-INSTANT-001", first.EndToEndID)
	}
	if first.AmountCents != 25000 {
		t.Errorf("AmountCents = %d, want 25000", first.AmountCents)
	}
	if first.Currency != "USD" {
		t.Errorf("Currency = %q, want USD", first.Currency)
	}
	if first.CreditorAccount != "acct-payee" {
		t.Errorf("CreditorAccount = %q, want acct-payee", first.CreditorAccount)
	}
	if first.DebtorAccount != "sender-acct-9" {
		t.Errorf("DebtorAccount = %q, want sender-acct-9", first.DebtorAccount)
	}
	if first.Remittance != "Invoice 4242" {
		t.Errorf("Remittance = %q, want 'Invoice 4242'", first.Remittance)
	}
	if first.MsgID != "MSG-FEDNOW-001" {
		t.Errorf("MsgID = %q, want MSG-FEDNOW-001", first.MsgID)
	}
	// IBAN account resolution on the second transaction.
	if txs[1].CreditorAccount != "acct-payee-iban" {
		t.Errorf("IBAN CreditorAccount = %q, want acct-payee-iban", txs[1].CreditorAccount)
	}
	if txs[1].AmountCents != 9999 {
		t.Errorf("AmountCents = %d, want 9999", txs[1].AmountCents)
	}
}

func TestParsePacs008Invalid(t *testing.T) {
	cases := []struct {
		name string
		doc  string
	}{
		{"empty", ""},
		{"malformed xml", "<Document><FIToFICstmrCdtTrf>"},
		{"no transactions", `<Document><FIToFICstmrCdtTrf><GrpHdr><MsgId>x</MsgId></GrpHdr></FIToFICstmrCdtTrf></Document>`},
		{"missing EndToEndId", `<Document><FIToFICstmrCdtTrf><CdtTrfTxInf><PmtId><TxId>x</TxId></PmtId><IntrBkSttlmAmt Ccy="USD">1.00</IntrBkSttlmAmt><CdtrAcct><Id><Othr><Id>a</Id></Othr></Id></CdtrAcct></CdtTrfTxInf></FIToFICstmrCdtTrf></Document>`},
		{"missing amount", `<Document><FIToFICstmrCdtTrf><CdtTrfTxInf><PmtId><EndToEndId>e</EndToEndId></PmtId><CdtrAcct><Id><Othr><Id>a</Id></Othr></Id></CdtrAcct></CdtTrfTxInf></FIToFICstmrCdtTrf></Document>`},
		{"bad amount", `<Document><FIToFICstmrCdtTrf><CdtTrfTxInf><PmtId><EndToEndId>e</EndToEndId></PmtId><IntrBkSttlmAmt Ccy="USD">abc</IntrBkSttlmAmt><CdtrAcct><Id><Othr><Id>a</Id></Othr></Id></CdtrAcct></CdtTrfTxInf></FIToFICstmrCdtTrf></Document>`},
		{"zero amount", `<Document><FIToFICstmrCdtTrf><CdtTrfTxInf><PmtId><EndToEndId>e</EndToEndId></PmtId><IntrBkSttlmAmt Ccy="USD">0.00</IntrBkSttlmAmt><CdtrAcct><Id><Othr><Id>a</Id></Othr></Id></CdtrAcct></CdtTrfTxInf></FIToFICstmrCdtTrf></Document>`},
		{"missing currency", `<Document><FIToFICstmrCdtTrf><CdtTrfTxInf><PmtId><EndToEndId>e</EndToEndId></PmtId><IntrBkSttlmAmt>1.00</IntrBkSttlmAmt><CdtrAcct><Id><Othr><Id>a</Id></Othr></Id></CdtrAcct></CdtTrfTxInf></FIToFICstmrCdtTrf></Document>`},
		{"missing creditor", `<Document><FIToFICstmrCdtTrf><CdtTrfTxInf><PmtId><EndToEndId>e</EndToEndId></PmtId><IntrBkSttlmAmt Ccy="USD">1.00</IntrBkSttlmAmt></CdtTrfTxInf></FIToFICstmrCdtTrf></Document>`},
	}
	for _, c := range cases {
		if _, err := ParsePacs008([]byte(c.doc)); err == nil {
			t.Errorf("%s: expected error, got nil", c.name)
		}
	}
}

// seedFedNowLedger builds a ledger with a known payee account (via a
// journaled entry) and a configured FedNow settlement account.
func seedFedNowLedger(t *testing.T) *Ledger {
	t.Helper()
	l := New()
	if _, _, err := l.Post(JournalEntry{
		ID:            "seed-payee",
		DebitAccount:  "acct-payee",
		CreditAccount: "acct-seed",
		AmountCents:   1000,
		Currency:      "USD",
	}); err != nil {
		t.Fatalf("seed Post: %v", err)
	}
	l.SetFedNowSettlementAccount("acct-fednow-settle")
	return l
}

func TestProcessPacs008HappyPath(t *testing.T) {
	l := seedFedNowLedger(t)
	results, err := l.ProcessPacs008([]byte(testPacs008Doc), "test-actor")
	if err != nil {
		t.Fatalf("ProcessPacs008: %v", err)
	}
	// The second transaction's payee (acct-payee-iban) is unknown and
	// has no association: it must be rejected, not booked.
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	first := results[0]
	if first.Err != nil {
		t.Fatalf("first item: %v", first.Err)
	}
	if first.EntryID != "fednow-pacs008/E2E-INSTANT-001" {
		t.Errorf("EntryID = %q", first.EntryID)
	}
	if first.Association.Kind != Pacs008AssociatedNone {
		t.Errorf("Association.Kind = %q, want none", first.Association.Kind)
	}
	// The receipt is booked debit-payee / credit-settlement.
	e, ok := l.entries[first.EntryID]
	if !ok {
		t.Fatalf("entry %q not journaled", first.EntryID)
	}
	if e.DebitAccount != "acct-payee" || e.CreditAccount != "acct-fednow-settle" {
		t.Errorf("legs = %s/%s", e.DebitAccount, e.CreditAccount)
	}
	if e.AmountCents != 25000 || e.Currency != "USD" {
		t.Errorf("amount = %d %s", e.AmountCents, e.Currency)
	}
	if e.ExternalRef != "E2E-INSTANT-001" {
		t.Errorf("ExternalRef = %q", e.ExternalRef)
	}
	// Payee balance went up by the receipt.
	if bal := l.BalanceIn("acct-payee", "USD"); bal != 1000+25000 {
		t.Errorf("payee balance = %d, want %d", bal, 1000+25000)
	}
	second := results[1]
	if second.Err == nil {
		t.Fatalf("second item: expected unknown-payee rejection, got nil")
	}
	if !strings.Contains(second.Err.Error(), "unknown") {
		t.Errorf("second item error = %v, want unknown payee", second.Err)
	}
	postings, rejections := l.FedNowStats()
	if postings != 1 {
		t.Errorf("postings = %d, want 1", postings)
	}
	if len(rejections) != 1 {
		t.Errorf("rejections = %v, want 1 reason", rejections)
	}
}

func TestProcessPacs008TransferAssociation(t *testing.T) {
	l := seedFedNowLedger(t)
	// Originate a transfer; the pacs.008 names its ID as EndToEndId.
	if _, err := l.PostTransfer(Transfer{
		ID:             "xfer-fednow-1",
		From:           "acct-payer",
		To:             "acct-payee",
		AmountCents:    25000,
		Currency:       "USD",
		IdempotencyKey: "key-xfer-1",
	}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	doc := strings.Replace(testPacs008Doc, "E2E-INSTANT-001", "xfer-fednow-1", 1)
	// The second transaction would hit the unknown-payee path; drop it
	// for this test by parsing only the first CdtTrfTxInf.
	start := strings.Index(doc, "<CdtTrfTxInf>")
	mid := strings.Index(doc, "</CdtTrfTxInf>") + len("</CdtTrfTxInf>")
	end := strings.LastIndex(doc, "</FIToFICstmrCdtTrf>")
	single := doc[:start] + doc[start:mid] + doc[end:]
	results, err := l.ProcessPacs008([]byte(single), "test-actor")
	if err != nil {
		t.Fatalf("ProcessPacs008: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	r := results[0]
	if r.Err != nil {
		t.Fatalf("item: %v", r.Err)
	}
	if r.Association.Kind != Pacs008AssociatedTransfer {
		t.Errorf("Association.Kind = %q, want transfer", r.Association.Kind)
	}
	if r.Association.ID != "xfer-fednow-1" {
		t.Errorf("Association.ID = %q", r.Association.ID)
	}
}

func TestProcessPacs008AmountMismatch(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	if _, _, err := l.Post(JournalEntry{
		ID: "seed-payee", DebitAccount: "acct-payee", CreditAccount: "acct-seed",
		AmountCents: 1000, Currency: "USD",
	}); err != nil {
		t.Fatalf("seed Post: %v", err)
	}
	l.SetFedNowSettlementAccount("acct-fednow-settle")
	if _, err := l.PostTransfer(Transfer{
		ID:             "xfer-fednow-2",
		From:           "acct-payer",
		To:             "acct-payee",
		AmountCents:    10000,
		Currency:       "USD",
		IdempotencyKey: "key-xfer-2",
	}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	// The notification claims 250.00 for a 100.00 transfer.
	doc := strings.Replace(testPacs008Doc, "E2E-INSTANT-001", "xfer-fednow-2", 1)
	start := strings.Index(doc, "<CdtTrfTxInf>")
	mid := strings.Index(doc, "</CdtTrfTxInf>") + len("</CdtTrfTxInf>")
	end := strings.LastIndex(doc, "</FIToFICstmrCdtTrf>")
	single := doc[:start] + doc[start:mid] + doc[end:]
	results, err := l.ProcessPacs008([]byte(single), "test-actor")
	if err != nil {
		t.Fatalf("ProcessPacs008: %v", err)
	}
	if results[0].Err == nil {
		t.Fatalf("expected amount-mismatch rejection, got nil")
	}
	if !strings.Contains(results[0].Err.Error(), "does not match") {
		t.Errorf("error = %v", results[0].Err)
	}
	if _, ok := l.entries["fednow-pacs008/xfer-fednow-2"]; ok {
		t.Errorf("mismatched notification must not book")
	}
	// The explicit rejection is in the audit chain.
	if err := al.Close(); err != nil {
		t.Fatalf("audit close: %v", err)
	}
	events, corrupt, err := ReadAuditLog(dir, time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt = %d, want 0", corrupt)
	}
	found := false
	for _, ev := range events {
		if ev.Op == "fednow_pacs008_rejected" {
			found = true
			if ev.Actor != "test-actor" {
				t.Errorf("rejection actor = %q, want test-actor", ev.Actor)
			}
		}
	}
	if !found {
		t.Errorf("no fednow_pacs008_rejected audit event")
	}
	// And the per-reason counter moved.
	_, rejections := l.FedNowStats()
	if len(rejections) == 0 {
		t.Errorf("no rejection counters recorded")
	}
}

func TestProcessPacs008NoSettlementAccount(t *testing.T) {
	l := New()
	if _, _, err := l.Post(JournalEntry{
		ID: "seed", DebitAccount: "acct-payee", CreditAccount: "acct-seed",
		AmountCents: 1000, Currency: "USD",
	}); err != nil {
		t.Fatalf("seed Post: %v", err)
	}
	// No settlement account configured: fail-closed.
	start := strings.Index(testPacs008Doc, "<CdtTrfTxInf>")
	mid := strings.Index(testPacs008Doc, "</CdtTrfTxInf>") + len("</CdtTrfTxInf>")
	end := strings.LastIndex(testPacs008Doc, "</FIToFICstmrCdtTrf>")
	single := testPacs008Doc[:start] + testPacs008Doc[start:mid] + testPacs008Doc[end:]
	results, err := l.ProcessPacs008([]byte(single), "test-actor")
	if err != nil {
		t.Fatalf("ProcessPacs008: %v", err)
	}
	if results[0].Err == nil || !strings.Contains(results[0].Err.Error(), "not configured") {
		t.Errorf("expected settlement-account rejection, got %v", results[0].Err)
	}
}

func TestProcessPacs008IdempotentReplay(t *testing.T) {
	l := seedFedNowLedger(t)
	start := strings.Index(testPacs008Doc, "<CdtTrfTxInf>")
	mid := strings.Index(testPacs008Doc, "</CdtTrfTxInf>") + len("</CdtTrfTxInf>")
	end := strings.LastIndex(testPacs008Doc, "</FIToFICstmrCdtTrf>")
	single := testPacs008Doc[:start] + testPacs008Doc[start:mid] + testPacs008Doc[end:]
	first, err := l.ProcessPacs008([]byte(single), "a1")
	if err != nil {
		t.Fatalf("first ProcessPacs008: %v", err)
	}
	second, err := l.ProcessPacs008([]byte(single), "a2")
	if err != nil {
		t.Fatalf("second ProcessPacs008: %v", err)
	}
	if !second[0].Duplicate {
		t.Errorf("replay: Duplicate = false")
	}
	if second[0].EntryID != first[0].EntryID {
		t.Errorf("replay EntryID = %q, want %q", second[0].EntryID, first[0].EntryID)
	}
	postings, _ := l.FedNowStats()
	if postings != 1 {
		t.Errorf("postings = %d after replay, want 1", postings)
	}
	// One journaled entry for the EndToEndId, not two.
	count := 0
	for id := range l.entries {
		if id == "fednow-pacs008/E2E-INSTANT-001" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("journaled entries for E2E-INSTANT-001 = %d, want 1", count)
	}
}

func TestProcessPacs008Concurrent(t *testing.T) {
	l := seedFedNowLedger(t)
	start := strings.Index(testPacs008Doc, "<CdtTrfTxInf>")
	mid := strings.Index(testPacs008Doc, "</CdtTrfTxInf>") + len("</CdtTrfTxInf>")
	end := strings.LastIndex(testPacs008Doc, "</FIToFICstmrCdtTrf>")
	single := testPacs008Doc[:start] + testPacs008Doc[start:mid] + testPacs008Doc[end:]
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := l.ProcessPacs008([]byte(single), "race"); err != nil {
				t.Errorf("concurrent ProcessPacs008: %v", err)
			}
		}()
	}
	wg.Wait()
	postings, _ := l.FedNowStats()
	if postings != 1 {
		t.Errorf("postings = %d after 8 concurrent, want 1", postings)
	}
}

func TestFedNowSettlementAccountSnapshotRoundTrip(t *testing.T) {
	l := seedFedNowLedger(t)
	var buf strings.Builder
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	l2, err := ImportSnapshot(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if l2.fedNowSettlementAccount != "acct-fednow-settle" {
		t.Errorf("settlement account = %q after round-trip", l2.fedNowSettlementAccount)
	}
}

func TestPacs008MemoTruncation(t *testing.T) {
	tx := Pacs008Transaction{
		MsgID:           "m",
		EndToEndID:      "e",
		AmountCents:     1,
		Currency:        "USD",
		CreditorAccount: "a",
		Remittance:      strings.Repeat("x", 1000),
	}
	m := pacs008Memo(tx)
	if len([]rune(m)) > 255 {
		t.Errorf("memo = %d runes, want <= 255", len([]rune(m)))
	}
}
