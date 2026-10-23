package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// pacs010TestLedger builds a ledger with a funded payer, IBANs, and a
// live per-day mandate (m10) with two booked debits (d1, d2) on
// consecutive UTC days — ready for pacs.010 export and reconciliation.
func pacs010TestLedger(t *testing.T) *Ledger {
	t.Helper()
	l := New()
	if _, _, err := l.Post(JournalEntry{
		ID: "fund-cust10", DebitAccount: "cust10", CreditAccount: "equity",
		AmountCents: 1_000_000, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed Post: %v", err)
	}
	mustSetIBAN(t, l, "cust10", testIBANDE, "")
	mustSetIBAN(t, l, "merchant10", testIBANGB, "")
	now := time.Now().UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if _, err := l.CreateMandate(Mandate{
		ID: "m10", Payer: "cust10", Payee: "merchant10",
		MaxAmountCents: 5000, Currency: "USD",
		Frequency:      MandateFrequencyPerDay,
		ValidFrom:      day, ValidUntil: day.Add(96 * time.Hour),
	}); err != nil {
		t.Fatalf("CreateMandate: %v", err)
	}
	for i, id := range []string{"d1", "d2"} {
		if _, err := l.DebitWithMandate("m10", MandateDebit{
			ID: id, AmountCents: 1000 + int64(i)*500,
			CreatedAt: day.Add(time.Duration(10+i*24) * time.Hour),
		}); err != nil {
			t.Fatalf("DebitWithMandate %s: %v", id, err)
		}
	}
	return l
}

// The export renders a pacs.010.001.02 document with GrpHdr and one
// CdtInstr per booked collection, carrying the mandate identity and
// the stable entry-ID EndToEndIds.
func TestExportPacs010(t *testing.T) {
	l := pacs010TestLedger(t)
	doc, err := l.ExportPacs010("m10", Pacs010ExportOptions{})
	if err != nil {
		t.Fatalf("ExportPacs010: %v", err)
	}
	s := string(doc)
	for _, want := range []string{
		`xmlns="urn:iso:std:iso:20022:tech:xsd:pacs.010.001.02"`,
		"<FIDrctDbt>", "<GrpHdr>", "<CdtInstr>",
		"<SttlmMtd>INDA</SttlmMtd>",
		"<MndtId>m10</MndtId>",
		"<EndToEndId>d1</EndToEndId>", "<EndToEndId>d2</EndToEndId>",
		"<CtrlSum>25.00</CtrlSum>", "<NbOfTxs>2</NbOfTxs>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("pacs.010 document missing %q", want)
		}
	}
	// Deterministic MsgId: re-export renders byte-identical.
	doc2, err := l.ExportPacs010("m10", Pacs010ExportOptions{})
	if err != nil {
		t.Fatalf("ExportPacs010 re-export: %v", err)
	}
	if string(doc2) != s {
		t.Fatal("pacs.010 re-export is not byte-identical")
	}
	// Window filter: only d1's day.
	day := time.Now().UTC()
	start := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	doc3, err := l.ExportPacs010("m10", Pacs010ExportOptions{Since: start, Until: start.Add(24 * time.Hour)})
	if err != nil {
		t.Fatalf("ExportPacs010 windowed: %v", err)
	}
	s3 := string(doc3)
	if !strings.Contains(s3, "<EndToEndId>d1</EndToEndId>") || strings.Contains(s3, "<EndToEndId>d2</EndToEndId>") {
		t.Fatalf("windowed export should carry d1 only:\n%s", s3)
	}
	if !strings.Contains(s3, "<CtrlSum>10.00</CtrlSum>") {
		t.Fatalf("windowed CtrlSum wrong:\n%s", s3)
	}
}

// Fail-closed: unknown/inactive mandates, missing IBANs, bad ranges,
// and bad settlement methods all refuse the export.
func TestExportPacs010FailClosed(t *testing.T) {
	l := pacs010TestLedger(t)
	if _, err := l.ExportPacs010("nope", Pacs010ExportOptions{}); !errors.Is(err, ErrMandateNotFound) {
		t.Errorf("unknown mandate = %v, want ErrMandateNotFound", err)
	}
	if _, err := l.SuspendMandate("m10"); err != nil {
		t.Fatalf("SuspendMandate: %v", err)
	}
	if _, err := l.ExportPacs010("m10", Pacs010ExportOptions{}); !errors.Is(err, ErrMandateNotActive) {
		t.Errorf("suspended mandate = %v, want ErrMandateNotActive", err)
	}
	if _, err := l.CancelMandate("m10"); err != nil {
		t.Fatalf("CancelMandate: %v", err)
	}

	l2 := pacs010TestLedger(t)
	// A mandate whose payer has no IBAN refuses.
	if _, err := l2.CreateMandate(Mandate{
		ID: "m-noban", Payer: "noban", Payee: "merchant10",
		MaxAmountCents: 5000, Currency: "USD", Frequency: MandateFrequencyPerDay,
		ValidFrom: time.Now().Add(-time.Hour), ValidUntil: time.Now().Add(72 * time.Hour),
	}); err != nil {
		t.Fatalf("CreateMandate m-noban: %v", err)
	}
	if _, err := l2.DebitWithMandate("m-noban", MandateDebit{ID: "dn1", AmountCents: 100}); err != nil {
		t.Fatalf("DebitWithMandate: %v", err)
	}
	if _, err := l2.ExportPacs010("m-noban", Pacs010ExportOptions{}); !errors.Is(err, ErrMissingAccountIBAN) {
		t.Errorf("missing IBAN = %v, want ErrMissingAccountIBAN", err)
	}

	now := time.Now()
	if _, err := l2.ExportPacs010("m10", Pacs010ExportOptions{Since: now, Until: now.Add(-time.Hour)}); !errors.Is(err, ErrInvalidPacs010Range) {
		t.Errorf("bad range = %v, want ErrInvalidPacs010Range", err)
	}
	if _, err := l2.ExportPacs010("m10", Pacs010ExportOptions{SettlementMethod: "SWIFT"}); !errors.Is(err, ErrPacs010InvalidSettlementMethod) {
		t.Errorf("bad SttlmMtd = %v, want ErrPacs010InvalidSettlementMethod", err)
	}
	// A named settlement method rides the document.
	doc, err := l2.ExportPacs010("m10", Pacs010ExportOptions{SettlementMethod: "COVE"})
	if err != nil {
		t.Fatalf("ExportPacs010 COVE: %v", err)
	}
	if !strings.Contains(string(doc), "<SttlmMtd>COVE</SttlmMtd>") {
		t.Fatal("COVE settlement method not in document")
	}
}

// The export emits one "pacs010_exported" audit event and moves no
// version.
func TestExportPacs010Audit(t *testing.T) {
	al := mustAuditLog(t)
	defer al.Close()
	l := pacs010TestLedger(t)
	_ = l
	l2 := New(WithAuditLog(al))
	if _, _, err := l2.Post(JournalEntry{
		ID: "fund-a10", DebitAccount: "cust10", CreditAccount: "equity",
		AmountCents: 1_000_000, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed Post: %v", err)
	}
	mustSetIBAN(t, l2, "cust10", testIBANDE, "")
	mustSetIBAN(t, l2, "merchant10", testIBANGB, "")
	now := time.Now().UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if _, err := l2.CreateMandate(Mandate{
		ID: "m-audit", Payer: "cust10", Payee: "merchant10",
		MaxAmountCents: 5000, Currency: "USD", Frequency: MandateFrequencyPerDay,
		ValidFrom: day, ValidUntil: day.Add(72 * time.Hour),
	}); err != nil {
		t.Fatalf("CreateMandate: %v", err)
	}
	if _, err := l2.DebitWithMandate("m-audit", MandateDebit{ID: "da1", AmountCents: 1000, CreatedAt: day.Add(10 * time.Hour)}); err != nil {
		t.Fatalf("DebitWithMandate: %v", err)
	}
	vBefore := l2.version
	if _, err := l2.ExportPacs010("m-audit", Pacs010ExportOptions{}); err != nil {
		t.Fatalf("ExportPacs010: %v", err)
	}
	if l2.version != vBefore {
		t.Fatalf("export bumped version: %d -> %d", vBefore, l2.version)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, corrupt, err := ReadAuditLog(al.Dir(), now.UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt events = %d, want 0", corrupt)
	}
	found := false
	for _, ev := range events {
		if ev.Op == "pacs010_exported" {
			found = true
			if ev.Details["mandate_id"] != "m-audit" {
				t.Fatalf("pacs010_exported details = %v", ev.Details)
			}
		}
	}
	if !found {
		t.Fatal("audit chain missing pacs010_exported")
	}
}

// camt054DebitDoc renders a camt.054 debit notification for the test:
// one DBIT Ntry naming the given EndToEndId, booked today.
func camt054DebitDoc(ntfctnID, account, e2e string, cents int64) string {
	posted := time.Now().UTC().Format("2006-01-02")
	dollars := cents / 100
	rem := cents % 100
	return `<?xml version="1.0" encoding="UTF-8"?>
<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.054.001.08">
  <BkToCstmrDbtCdtNtfctn>
    <GrpHdr><MsgId>TEST-054-10</MsgId></GrpHdr>
    <Ntfctn>
      <Id>` + ntfctnID + `</Id>
      <Acct><Id><IBAN>` + account + `</IBAN></Id></Acct>
      <Ntry>
        <Amt Ccy="USD">` + strings.TrimSpace(strings.Join([]string{pacs010TestItoa(dollars), ".", pacs010TestItoaPad(rem)}, "")) + `</Amt>
        <CdtDbtInd>DBIT</CdtDbtInd>
        <BookgDt><Dt>` + posted + `</Dt></BookgDt>
        <NtryDtls><TxDtls>
          <Refs><EndToEndId>` + e2e + `</EndToEndId></Refs>
        </TxDtls></NtryDtls>
      </Ntry>
    </Ntfctn>
  </BkToCstmrDbtCdtNtfctn>
</Document>
`
}

func pacs010TestItoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func pacs010TestItoaPad(n int64) string {
	s := pacs010TestItoa(n)
	if len(s) == 1 {
		return "0" + s
	}
	return s
}

// Reconciliation: with no camt.054 notifications every collection is
// pending; after the bank's debit notification names d1's EndToEndId,
// d1 is notified and d2 stays pending.
func TestReconcilePacs010(t *testing.T) {
	l := pacs010TestLedger(t)
	rep, err := l.ReconcilePacs010("m10", Pacs010ReconOptions{})
	if err != nil {
		t.Fatalf("ReconcilePacs010: %v", err)
	}
	if rep.Notified != 0 || rep.Pending != 2 {
		t.Fatalf("before notifications: notified=%d pending=%d, want 0/2", rep.Notified, rep.Pending)
	}
	for _, it := range rep.Items {
		if it.Status != Pacs010ReconPending {
			t.Fatalf("item %s status = %q, want pending", it.EntryID, it.Status)
		}
	}
	if _, err := l.ReconcilePacs010("nope", Pacs010ReconOptions{}); !errors.Is(err, ErrMandateNotFound) {
		t.Fatalf("unknown mandate = %v, want ErrMandateNotFound", err)
	}

	doc := camt054DebitDoc("NTF-10", "cust10", "d1", 1000)
	if _, err := l.ImportCAMT054("cust10", []byte(doc), CAMT054ImportOptions{}); err != nil {
		t.Fatalf("ImportCAMT054: %v", err)
	}
	rep, err = l.ReconcilePacs010("m10", Pacs010ReconOptions{})
	if err != nil {
		t.Fatalf("ReconcilePacs010 after import: %v", err)
	}
	if rep.Notified != 1 || rep.Pending != 1 {
		t.Fatalf("after notification: notified=%d pending=%d, want 1/1", rep.Notified, rep.Pending)
	}
	byID := map[string]Pacs010ReconItem{}
	for _, it := range rep.Items {
		byID[it.EntryID] = it
	}
	d1 := byID["d1"]
	if d1.Status != Pacs010ReconNotified || d1.NtfctnID != "NTF-10/1" {
		t.Fatalf("d1 = %+v, want notified via NTF-10/1", d1)
	}
	if d1.MatchStatus != string(CAMT054Matched) {
		t.Fatalf("d1 match status = %q, want matched", d1.MatchStatus)
	}
	if byID["d2"].Status != Pacs010ReconPending {
		t.Fatalf("d2 = %+v, want pending", byID["d2"])
	}
	// A suspended mandate's past collections still reconcile: the
	// reconciliation does not gate on mandate status.
	if _, err := l.SuspendMandate("m10"); err != nil {
		t.Fatalf("SuspendMandate: %v", err)
	}
	rep, err = l.ReconcilePacs010("m10", Pacs010ReconOptions{})
	if err != nil {
		t.Fatalf("ReconcilePacs010 suspended: %v", err)
	}
	if rep.Notified != 1 || rep.Pending != 1 {
		t.Fatalf("suspended: notified=%d pending=%d, want 1/1", rep.Notified, rep.Pending)
	}
}

// pain.002 answers pain.001 only: a report naming a pacs.010 MsgId —
// or a pain.008 MsgId — is refused outright.
func TestPain002RefusesPacs010MsgID(t *testing.T) {
	tx := pain002TxFrag("d1", Pain002TxACSC, "", "")
	for _, id := range []string{"pacs010-0-0-m10-d1", "pain008-0-0-m10-x"} {
		doc := pain002TestDocNm(t, id, "pain.001.001.03", tx)
		if _, err := ParsePain002(doc); !errors.Is(err, ErrPain002ForeignMsgID) {
			t.Fatalf("pain.002 naming %q = %v, want ErrPain002ForeignMsgID", id, err)
		}
	}
	// A genuine pain.001 answer still parses.
	doc := pain002TestDocNm(t, "pain001-0-0-x", "pain.001.001.03", tx)
	if _, err := ParsePain002(doc); err != nil {
		t.Fatalf("pain.002 for pain.001 = %v, want nil", err)
	}
}
