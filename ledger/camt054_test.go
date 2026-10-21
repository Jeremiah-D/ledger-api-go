package ledger

import (
	"strings"
	"testing"
	"time"
)

const testCAMT054Doc = `<?xml version="1.0" encoding="UTF-8"?>
<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.054.001.08">
  <BkToCstmrDbtCdtNtfctn>
    <GrpHdr>
      <MsgId>MSG-CAMT054-001</MsgId>
      <CreDtTm>2026-10-21T10:00:00Z</CreDtTm>
    </GrpHdr>
    <Ntfctn>
      <Id>NTF-2026-10-21-A</Id>
      <Acct><Id><IBAN>acct-notify</IBAN></Id></Acct>
      <Ntry>
        <Amt Ccy="USD">500.00</Amt>
        <CdtDbtInd>CRDT</CdtDbtInd>
        <BookgDt><DtTm>2026-10-21T09:30:00Z</DtTm></BookgDt>
        <NtryDtls>
          <TxDtls>
            <Refs><EndToEndId>xfer-camt054-1</EndToEndId></Refs>
            <RmtInf><Ustrd>vendor payment</Ustrd></RmtInf>
          </TxDtls>
        </NtryDtls>
      </Ntry>
      <Ntry>
        <Amt Ccy="USD">75.25</Amt>
        <CdtDbtInd>DBIT</CdtDbtInd>
        <BookgDt><Dt>2026-10-21</Dt></BookgDt>
        <NtryDtls>
          <TxDtls>
            <Refs><EndToEndId>no-such-transfer</EndToEndId></Refs>
          </TxDtls>
        </NtryDtls>
      </Ntry>
    </Ntfctn>
  </BkToCstmrDbtCdtNtfctn>
</Document>`

func TestParseCAMT054(t *testing.T) {
	ns, err := ParseCAMT054([]byte(testCAMT054Doc))
	if err != nil {
		t.Fatalf("ParseCAMT054: %v", err)
	}
	if len(ns) != 2 {
		t.Fatalf("got %d notifications, want 2", len(ns))
	}
	first := ns[0]
	if first.NtfctnID != "NTF-2026-10-21-A" {
		t.Errorf("NtfctnID = %q", first.NtfctnID)
	}
	if first.Account != "acct-notify" {
		t.Errorf("Account = %q", first.Account)
	}
	if first.Direction != CAMT054DirectionIn {
		t.Errorf("Direction = %q, want in", first.Direction)
	}
	if first.AmountCents != 50000 {
		t.Errorf("AmountCents = %d, want 50000", first.AmountCents)
	}
	if first.Currency != "USD" {
		t.Errorf("Currency = %q", first.Currency)
	}
	if first.EndToEndID != "xfer-camt054-1" {
		t.Errorf("EndToEndID = %q", first.EndToEndID)
	}
	if first.Memo != "vendor payment" {
		t.Errorf("Memo = %q", first.Memo)
	}
	if ns[1].Direction != CAMT054DirectionOut {
		t.Errorf("second Direction = %q, want out", ns[1].Direction)
	}
	if ns[1].AmountCents != 7525 {
		t.Errorf("second AmountCents = %d, want 7525", ns[1].AmountCents)
	}
}

func TestParseCAMT054Invalid(t *testing.T) {
	cases := []struct {
		name string
		doc  string
	}{
		{"empty", ""},
		{"malformed xml", "<Document><BkToCstmrDbtCdtNtfctn>"},
		{"no notifications", `<Document><BkToCstmrDbtCdtNtfctn><GrpHdr><MsgId>x</MsgId></GrpHdr></BkToCstmrDbtCdtNtfctn></Document>`},
		{"missing Ntfctn Id", `<Document><BkToCstmrDbtCdtNtfctn><Ntfctn><Acct><Id><IBAN>a</IBAN></Id></Acct></Ntfctn></BkToCstmrDbtCdtNtfctn></Document>`},
		{"missing account", `<Document><BkToCstmrDbtCdtNtfctn><Ntfctn><Id>n</Id></Ntfctn></BkToCstmrDbtCdtNtfctn></Document>`},
		{"missing amount", `<Document><BkToCstmrDbtCdtNtfctn><Ntfctn><Id>n</Id><Acct><Id><IBAN>a</IBAN></Id></Acct><Ntry><CdtDbtInd>CRDT</CdtDbtInd><BookgDt><Dt>2026-10-21</Dt></BookgDt></Ntry></Ntfctn></BkToCstmrDbtCdtNtfctn></Document>`},
		{"bad amount", `<Document><BkToCstmrDbtCdtNtfctn><Ntfctn><Id>n</Id><Acct><Id><IBAN>a</IBAN></Id></Acct><Ntry><Amt Ccy="USD">zz</Amt><CdtDbtInd>CRDT</CdtDbtInd><BookgDt><Dt>2026-10-21</Dt></BookgDt></Ntry></Ntfctn></BkToCstmrDbtCdtNtfctn></Document>`},
		{"missing currency", `<Document><BkToCstmrDbtCdtNtfctn><Ntfctn><Id>n</Id><Acct><Id><IBAN>a</IBAN></Id></Acct><Ntry><Amt>1.00</Amt><CdtDbtInd>CRDT</CdtDbtInd><BookgDt><Dt>2026-10-21</Dt></BookgDt></Ntry></Ntfctn></BkToCstmrDbtCdtNtfctn></Document>`},
		{"missing date", `<Document><BkToCstmrDbtCdtNtfctn><Ntfctn><Id>n</Id><Acct><Id><IBAN>a</IBAN></Id></Acct><Ntry><Amt Ccy="USD">1.00</Amt><CdtDbtInd>CRDT</CdtDbtInd></Ntry></Ntfctn></BkToCstmrDbtCdtNtfctn></Document>`},
	}
	for _, c := range cases {
		if _, err := ParseCAMT054([]byte(c.doc)); err == nil {
			t.Errorf("%s: expected error, got nil", c.name)
		}
	}
}

func TestImportCAMT054TripleMatch(t *testing.T) {
	l := New()
	// Originate a transfer the notification's EndToEndId will name.
	if _, err := l.PostTransfer(Transfer{
		ID:             "xfer-camt054-1",
		From:           "acct-payer",
		To:             "acct-notify",
		AmountCents:    50000,
		Currency:       "USD",
		IdempotencyKey: "key-camt054-1",
	}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	report, err := l.ImportCAMT054("acct-notify", []byte(testCAMT054Doc), CAMT054ImportOptions{})
	if err != nil {
		t.Fatalf("ImportCAMT054: %v", err)
	}
	if report.Matched != 1 || report.Unmatched != 1 || report.Mismatched != 0 {
		t.Errorf("matched=%d unmatched=%d mismatched=%d, want 1/1/0",
			report.Matched, report.Unmatched, report.Mismatched)
	}
	first := report.Items[0]
	if first.Status != CAMT054Matched {
		t.Fatalf("first item status = %q, want matched", first.Status)
	}
	if first.MatchedKind != "transfer" || first.MatchedID != "xfer-camt054-1" {
		t.Errorf("matched = %s/%s, want transfer/xfer-camt054-1", first.MatchedKind, first.MatchedID)
	}
	second := report.Items[1]
	if second.Status != CAMT054Unmatched {
		t.Errorf("second item status = %q, want unmatched", second.Status)
	}
	// The reconciliation advance is recorded in the match registry.
	rec, ok := l.camt054Matches["camt054/NTF-2026-10-21-A/1"]
	if !ok || rec.status != CAMT054Matched {
		t.Errorf("match registry: %+v, want matched record", rec)
	}
	// Re-importing the same document replays the original report.
	dup, err := l.ImportCAMT054("acct-notify", []byte(testCAMT054Doc), CAMT054ImportOptions{})
	if err != nil {
		t.Fatalf("re-import: %v", err)
	}
	if !dup.Duplicate {
		t.Errorf("re-import: Duplicate = false")
	}
}

func TestImportCAMT054AmountMismatch(t *testing.T) {
	l := New()
	if _, err := l.PostTransfer(Transfer{
		ID:             "xfer-camt054-2",
		From:           "acct-payer",
		To:             "acct-notify",
		AmountCents:    10000,
		Currency:       "USD",
		IdempotencyKey: "key-camt054-2",
	}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	// The notification claims 500.00 for a 100.00 transfer.
	doc := strings.Replace(testCAMT054Doc, "xfer-camt054-1", "xfer-camt054-2", 1)
	start := strings.Index(doc, "<Ntry>")
	mid := strings.Index(doc, "</Ntry>") + len("</Ntry>")
	end := strings.LastIndex(doc, "</Ntfctn>")
	single := doc[:start] + doc[start:mid] + doc[end:]
	report, err := l.ImportCAMT054("acct-notify", []byte(single), CAMT054ImportOptions{})
	if err != nil {
		t.Fatalf("ImportCAMT054: %v", err)
	}
	if len(report.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(report.Items))
	}
	item := report.Items[0]
	if item.Status != CAMT054Mismatched {
		t.Fatalf("status = %q, want mismatched", item.Status)
	}
	if item.MismatchReason != "amount_mismatch" {
		t.Errorf("MismatchReason = %q, want amount_mismatch", item.MismatchReason)
	}
}

func TestImportCAMT054DateOutOfWindow(t *testing.T) {
	l := New()
	if _, err := l.PostTransfer(Transfer{
		ID:             "xfer-camt054-3",
		From:           "acct-payer",
		To:             "acct-notify",
		AmountCents:    50000,
		Currency:       "USD",
		IdempotencyKey: "key-camt054-3",
	}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	// The transfer was created now; the notification is booked a year
	// later — outside the default ±24h window.
	doc := strings.Replace(testCAMT054Doc, "xfer-camt054-1", "xfer-camt054-3", 1)
	doc = strings.Replace(doc, "2026-10-21T09:30:00Z", "2027-10-21T09:30:00Z", 1)
	start := strings.Index(doc, "<Ntry>")
	mid := strings.Index(doc, "</Ntry>") + len("</Ntry>")
	end := strings.LastIndex(doc, "</Ntfctn>")
	single := doc[:start] + doc[start:mid] + doc[end:]
	report, err := l.ImportCAMT054("acct-notify", []byte(single), CAMT054ImportOptions{})
	if err != nil {
		t.Fatalf("ImportCAMT054: %v", err)
	}
	if report.Items[0].Status != CAMT054Mismatched || report.Items[0].MismatchReason != "date_out_of_window" {
		t.Errorf("item = %+v, want mismatched/date_out_of_window", report.Items[0])
	}
	// A wide window resolves it (a fresh document: document-level
	// idempotency would replay the narrow-window report).
	wideDoc := strings.Replace(single, "NTF-2026-10-21-A", "NTF-2026-10-21-B", 1)
	wide, err := l.ImportCAMT054("acct-notify", []byte(wideDoc), CAMT054ImportOptions{MatchWindow: 400 * 24 * time.Hour})
	if err != nil {
		t.Fatalf("wide-window ImportCAMT054: %v", err)
	}
	if wide.Items[0].Status != CAMT054Matched {
		t.Errorf("wide-window status = %q, want matched", wide.Items[0].Status)
	}
}

func TestImportCAMT054PayoutAssociation(t *testing.T) {
	l := New()
	if _, _, err := l.CreatePayout(PayoutInstruction{
		ID:             "po-camt054-1",
		PayeeAccount:   "acct-payee",
		AmountCents:    50000,
		Currency:       "USD",
		Destination:    "bank",
		Maker:          "ops",
		IdempotencyKey: "key-po-1",
	}); err != nil {
		t.Fatalf("CreatePayout: %v", err)
	}
	doc := strings.Replace(testCAMT054Doc, "xfer-camt054-1", "po-camt054-1", 1)
	start := strings.Index(doc, "<Ntry>")
	mid := strings.Index(doc, "</Ntry>") + len("</Ntry>")
	end := strings.LastIndex(doc, "</Ntfctn>")
	single := doc[:start] + doc[start:mid] + doc[end:]
	report, err := l.ImportCAMT054("acct-notify", []byte(single), CAMT054ImportOptions{})
	if err != nil {
		t.Fatalf("ImportCAMT054: %v", err)
	}
	if report.Items[0].Status != CAMT054Matched {
		t.Fatalf("status = %q, want matched", report.Items[0].Status)
	}
	if report.Items[0].MatchedKind != "payout" || report.Items[0].MatchedID != "po-camt054-1" {
		t.Errorf("matched = %s/%s", report.Items[0].MatchedKind, report.Items[0].MatchedID)
	}
}

func TestImportCAMT054WrongAccount(t *testing.T) {
	l := New()
	if _, err := l.ImportCAMT054("acct-other", []byte(testCAMT054Doc), CAMT054ImportOptions{}); err == nil {
		t.Errorf("expected account-mismatch error, got nil")
	}
}

func TestImportCAMT054EmptyAccount(t *testing.T) {
	l := New()
	if _, err := l.ImportCAMT054("", []byte(testCAMT054Doc), CAMT054ImportOptions{}); err == nil {
		t.Errorf("expected empty-account error, got nil")
	}
}
