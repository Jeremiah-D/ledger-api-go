package ledger

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Well-known valid test IBANs (mod-97 == 1).
const (
	testIBANDE  = "DE89370400440532013000"
	testIBANGB  = "GB29NWBK60161331926819"
	testIBANFR  = "FR1420041010050500013M02606"
	testIBANNL  = "NL91ABNA0417164300"
	testBICDE   = "DEUTDEFF"
	testBICDE11 = "DEUTDEFF500"
)

func mustSetIBAN(t *testing.T, l *Ledger, account, iban, bic string) {
	t.Helper()
	if err := l.SetAccountIBAN(AccountID(account), iban, bic); err != nil {
		t.Fatalf("SetAccountIBAN(%s): %v", account, err)
	}
}

func mustPostTransferPain(t *testing.T, l *Ledger, id, from, to string, cents int64, currency string, at time.Time) {
	t.Helper()
	tr := Transfer{ID: id, From: AccountID(from), To: AccountID(to), AmountCents: cents, Currency: currency}
	if !at.IsZero() {
		tr.CreatedAt = at
	}
	if _, err := l.PostTransfer(tr); err != nil {
		t.Fatalf("PostTransfer(%s): %v", id, err)
	}
}

func painTestLedger(t *testing.T) *Ledger {
	t.Helper()
	l := New()
	mustSetIBAN(t, l, "alice", testIBANDE, testBICDE)
	mustSetIBAN(t, l, "bob", testIBANGB, "")
	mustSetIBAN(t, l, "carol", testIBANFR, testBICDE11)
	return l
}

// Golden document: MsgId, NbOfTxs, CtrlSum, and both IBANs land in the
// right elements; the EndToEndId is the transfer ID.
func TestPain001GoldenDocument(t *testing.T) {
	l := painTestLedger(t)
	base := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	mustPostTransferPain(t, l, "pay-1", "alice", "bob", 15000, "USD", base)
	mustPostTransferPain(t, l, "pay-2", "alice", "carol", 25050, "USD", base.Add(time.Hour))

	doc, err := l.ExportPain001(Pain001ExportOptions{Currency: "USD"})
	if err != nil {
		t.Fatalf("ExportPain001: %v", err)
	}
	s := string(doc)
	for _, want := range []string{
		`<?xml version="1.0" encoding="UTF-8"?>`,
		`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pain.001.001.03">`,
		"<NbOfTxs>2</NbOfTxs>",
		"<CtrlSum>400.50</CtrlSum>",
		"<EndToEndId>pay-1</EndToEndId>",
		"<EndToEndId>pay-2</EndToEndId>",
		`<InstdAmt Ccy="USD">150.00</InstdAmt>`,
		`<InstdAmt Ccy="USD">250.50</InstdAmt>`,
		"<Dbtr><Nm>alice</Nm></Dbtr>",
		"<IBAN>" + testIBANDE + "</IBAN>",
		"<BIC>" + testBICDE + "</BIC>",
		"<Cdtr><Nm>bob</Nm></Cdtr>",
		"<IBAN>" + testIBANGB + "</IBAN>",
		"<Cdtr><Nm>carol</Nm></Cdtr>",
		"<IBAN>" + testIBANFR + "</IBAN>",
		"<BIC>" + testBICDE11 + "</BIC>",
		"</Document>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("pain.001 doc missing %q\n%s", want, s)
		}
	}
	// The MsgId is deterministic: the same export re-renders
	// byte-identical.
	doc2, err := l.ExportPain001(Pain001ExportOptions{Currency: "USD"})
	if err != nil {
		t.Fatalf("ExportPain001 (2nd): %v", err)
	}
	if string(doc2) != s {
		t.Errorf("re-export not byte-identical")
	}
	// One PmtInf per debtor here (single debtor): group totals match.
	if got := strings.Count(s, "<PmtInf>"); got != 1 {
		t.Errorf("PmtInf blocks = %d, want 1", got)
	}
	if got := strings.Count(s, "<CdtTrfTxInf>"); got != 2 {
		t.Errorf("CdtTrfTxInf blocks = %d, want 2", got)
	}
}

// The IBAN validation matrix: valid mod-97 passes; bad check digits,
// bad characters, bad lengths, and bad country codes fail.
func TestPain001IBANValidationMatrix(t *testing.T) {
	l := New()
	valid := []string{
		testIBANDE, testIBANGB, testIBANFR, testIBANNL,
		"de89370400440532013000",          // lowercase accepted, canonicalized
		"DE89 3704 0044 0532 0130 00",     // spaced groups accepted
		"NO9386011117947",                 // 15 chars, shortest real IBAN
		"MT84MALT011000012345MTLCAST001S", // 31 chars
	}
	for i, iban := range valid {
		acct := AccountID(fmt.Sprintf("a%d", i))
		if err := l.SetAccountIBAN(acct, iban, ""); err != nil {
			t.Errorf("SetAccountIBAN(%q): got %v, want nil", iban, err)
		}
	}
	// Canonical form: uppercased, spaces stripped ("a4" was lowercase,
	// "a5" was spaced groups).
	for _, acct := range []AccountID{"a4", "a5"} {
		reg, ok := l.AccountIBAN(acct)
		if !ok {
			t.Fatalf("AccountIBAN(%s): not found after SetAccountIBAN", acct)
		}
		if reg.IBAN != testIBANDE {
			t.Errorf("stored IBAN = %q, want canonical %q", reg.IBAN, testIBANDE)
		}
	}
	invalid := []string{
		"DE89370400440532013001", // bad mod-97 (last digit changed)
		"DE8937040044053201300",  // one char short (bad mod-97 anyway)
		"DE89",                   // too short
		"DE89370400440532013000X", // too long (35)
		"DE8937040044053201300!", // bad character
		"1289370400440532013000",  // digits where the country code goes
		"",                       // empty
	}
	for _, iban := range invalid {
		if err := l.SetAccountIBAN("b", iban, ""); !errors.Is(err, ErrInvalidIBAN) {
			t.Errorf("SetAccountIBAN(%q): got %v, want ErrInvalidIBAN", iban, err)
		}
	}
	if err := l.SetAccountIBAN("", testIBANDE, ""); !errors.Is(err, ErrEmptyAccountID) {
		t.Errorf("empty account: got %v, want ErrEmptyAccountID", err)
	}
}

// BIC: empty, 8, or 11 alphanumeric; anything else fails.
func TestPain001BICValidation(t *testing.T) {
	l := New()
	for _, bic := range []string{"", "DEUTDEFF", "DEUTDEFF500", "deutdeff"} {
		if err := l.SetAccountIBAN("a", testIBANDE, bic); err != nil {
			t.Errorf("SetAccountIBAN bic %q: got %v, want nil", bic, err)
		}
	}
	for _, bic := range []string{"DEUTDEF", "DEUTDEFF5", "DEUTDEFF5001", "DEUT-EFF", "12345678"} {
		if err := l.SetAccountIBAN("a", testIBANDE, bic); !errors.Is(err, ErrInvalidBIC) {
			t.Errorf("SetAccountIBAN bic %q: got %v, want ErrInvalidBIC", bic, err)
		}
	}
	// BIC canonicalized to uppercase.
	reg, _ := l.AccountIBAN("a")
	_ = reg
	if err := l.SetAccountIBAN("c", testIBANGB, "nwbkGB2l"); err != nil {
		t.Fatalf("SetAccountIBAN: %v", err)
	}
	reg, _ = l.AccountIBAN("c")
	if reg.BIC != "NWBKGB2L" {
		t.Errorf("stored BIC = %q, want uppercased", reg.BIC)
	}
}

// CtrlSum is the exact sum of the exported instructions; cents render
// as exact decimals with no float drift.
func TestPain001CtrlSum(t *testing.T) {
	l := painTestLedger(t)
	base := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	mustPostTransferPain(t, l, "c1", "alice", "bob", 1, "USD", base)          // $0.01
	mustPostTransferPain(t, l, "c2", "alice", "bob", 999999999, "USD", base)   // $9,999,999.99
	mustPostTransferPain(t, l, "c3", "bob", "carol", 333, "USD", base)         // $3.33, other debtor
	doc, err := l.ExportPain001(Pain001ExportOptions{Currency: "USD"})
	if err != nil {
		t.Fatalf("ExportPain001: %v", err)
	}
	s := string(doc)
	// 1 + 999999999 + 333 = 1000000333 cents = 10000003.33
	if !strings.Contains(s, "<NbOfTxs>3</NbOfTxs>") || !strings.Contains(s, "<CtrlSum>10000003.33</CtrlSum>") {
		t.Errorf("GrpHdr totals wrong\n%s", s)
	}
	if !strings.Contains(s, "<InstdAmt Ccy=\"USD\">0.01</InstdAmt>") ||
		!strings.Contains(s, "<InstdAmt Ccy=\"USD\">9999999.99</InstdAmt>") ||
		!strings.Contains(s, "<InstdAmt Ccy=\"USD\">3.33</InstdAmt>") {
		t.Errorf("per-instruction amounts not exact\n%s", s)
	}
	// Two debtors -> two PmtInf blocks, each with its own subtotal.
	if got := strings.Count(s, "<PmtInf>"); got != 2 {
		t.Errorf("PmtInf blocks = %d, want 2 (one per debtor)", got)
	}
	if !strings.Contains(s, "<CtrlSum>10000000.00</CtrlSum>") { // alice: 1 + 999999999
		t.Errorf("alice PmtInf CtrlSum wrong\n%s", s)
	}
}

// The export is read-only: no version bump, no chain link, no balance
// movement — but it does emit its audit event.
func TestPain001ExportIsReadOnly(t *testing.T) {
	al := mustAuditLog(t)
	defer al.Close()
	l := New(WithAuditLog(al))
	mustSetIBAN(t, l, "alice", testIBANDE, "")
	mustSetIBAN(t, l, "bob", testIBANGB, "")
	mustPostTransferPain(t, l, "r1", "alice", "bob", 5000, "USD", time.Time{})

	vBefore := l.Version()
	headBefore, linksBefore := l.ChainHead()
	balBefore := l.BalanceIn("alice", "USD")

	doc, err := l.ExportPain001(Pain001ExportOptions{})
	if err != nil {
		t.Fatalf("ExportPain001: %v", err)
	}
	if len(doc) == 0 {
		t.Fatal("empty document")
	}
	if got := l.Version(); got != vBefore {
		t.Errorf("version moved %d -> %d on a read-only export", vBefore, got)
	}
	if head, links := l.ChainHead(); head != headBefore || links != linksBefore {
		t.Errorf("audit chain moved on a read-only export")
	}
	if got := l.BalanceIn("alice", "USD"); got != balBefore {
		t.Errorf("balance moved %d -> %d on a read-only export", balBefore, got)
	}
	// Idempotency namespace untouched: a fresh key still books.
	if _, err := l.PostTransfer(Transfer{ID: "r2", From: "alice", To: "bob", AmountCents: 100, Currency: "USD", IdempotencyKey: "k-r2"}); err != nil {
		t.Fatalf("PostTransfer after export: %v", err)
	}
}

// Every export emits one structured audit event with the message facts.
func TestPain001AuditEventShape(t *testing.T) {
	al := mustAuditLog(t)
	defer al.Close()
	l := New(WithAuditLog(al))
	mustSetIBAN(t, l, "alice", testIBANDE, "")
	mustSetIBAN(t, l, "bob", testIBANGB, "")
	mustPostTransferPain(t, l, "a1", "alice", "bob", 12000, "USD", time.Time{})
	mustPostTransferPain(t, l, "a2", "alice", "bob", 8000, "USD", time.Time{})

	vBefore := l.Version()
	if _, err := l.ExportPain001(Pain001ExportOptions{Currency: "USD"}); err != nil {
		t.Fatalf("ExportPain001: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, corrupt, err := ReadAuditLog(al.Dir(), time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt = %d, want 0", corrupt)
	}
	var found *AuditEvent
	for i, ev := range events {
		if ev.Op == "pain001_exported" {
			found = &events[i]
		}
	}
	if found == nil {
		t.Fatalf("no pain001_exported audit event; ops seen: %v", opNames(events))
	}
	if found.Actor != "ExportPain001" {
		t.Errorf("actor = %q, want ExportPain001", found.Actor)
	}
	if found.VersionBefore != vBefore || found.VersionAfter != vBefore {
		t.Errorf("version bracket = %d/%d, want %d/%d (read-only)", found.VersionBefore, found.VersionAfter, vBefore, vBefore)
	}
	if len(found.EntryIDs) != 0 {
		t.Errorf("entry_ids = %v, want empty (nothing booked)", found.EntryIDs)
	}
	d := found.Details
	if d["tx_count"] != float64(2) {
		t.Errorf("tx_count = %v, want 2", d["tx_count"])
	}
	if d["ctrl_sum_cents"] != float64(20000) {
		t.Errorf("ctrl_sum_cents = %v, want 20000", d["ctrl_sum_cents"])
	}
	if d["currency"] != "USD" {
		t.Errorf("currency = %v, want USD", d["currency"])
	}
	msgID, _ := d["msg_id"].(string)
	if msgID == "" || found.TraceID != msgID {
		t.Errorf("msg_id detail %q does not match trace_id %q", msgID, found.TraceID)
	}
}

func opNames(events []AuditEvent) []string {
	var out []string
	for _, ev := range events {
		out = append(out, ev.Op)
	}
	return out
}

// Pending-review transfers are excluded from the export (the OFX
// convention); approving the review makes the transfer exportable.
func TestPain001ExcludesPendingReview(t *testing.T) {
	l := painTestLedger(t)
	if err := l.SetReviewThreshold("alice", 100000); err != nil {
		t.Fatalf("SetReviewThreshold: %v", err)
	}
	mustPostTransferPain(t, l, "settle-1", "alice", "bob", 1000, "USD", time.Time{})
	mustPostTransferPain(t, l, "frozen-1", "alice", "bob", 200000, "USD", time.Time{})

	doc, err := l.ExportPain001(Pain001ExportOptions{Currency: "USD"})
	if err != nil {
		t.Fatalf("ExportPain001: %v", err)
	}
	s := string(doc)
	if !strings.Contains(s, "<EndToEndId>settle-1</EndToEndId>") {
		t.Errorf("settled transfer missing from export\n%s", s)
	}
	if strings.Contains(s, "frozen-1") {
		t.Errorf("pending-review transfer leaked into export\n%s", s)
	}
	if !strings.Contains(s, "<NbOfTxs>1</NbOfTxs>") || !strings.Contains(s, "<CtrlSum>10.00</CtrlSum>") {
		t.Errorf("totals should cover only the effective transfer\n%s", s)
	}

	// Approval flips the transfer effective: the next export includes
	// it, with no registry change needed.
	if _, err := l.ApproveTransferReview("frozen-1", "op"); err != nil {
		t.Fatalf("ApproveTransferReview: %v", err)
	}
	doc, err = l.ExportPain001(Pain001ExportOptions{Currency: "USD"})
	if err != nil {
		t.Fatalf("ExportPain001 after approval: %v", err)
	}
	s = string(doc)
	if !strings.Contains(s, "<EndToEndId>frozen-1</EndToEndId>") {
		t.Errorf("approved transfer missing from export\n%s", s)
	}
	if !strings.Contains(s, "<NbOfTxs>2</NbOfTxs>") || !strings.Contains(s, "<CtrlSum>2010.00</CtrlSum>") {
		t.Errorf("totals should cover both transfers after approval\n%s", s)
	}
}

// Fail-closed: a payment whose debtor or creditor has no registered
// IBAN refuses the whole export — no forged identifiers.
func TestPain001MissingIBANFailClosed(t *testing.T) {
	l := painTestLedger(t)
	mustPostTransferPain(t, l, "m1", "alice", "nobody", 1000, "USD", time.Time{})
	if _, err := l.ExportPain001(Pain001ExportOptions{}); !errors.Is(err, ErrMissingAccountIBAN) {
		t.Fatalf("missing creditor IBAN: got %v, want ErrMissingAccountIBAN", err)
	}
	mustPostTransferPain(t, l, "m2", "ghost", "bob", 1000, "USD", time.Time{})
	if _, err := l.ExportPain001(Pain001ExportOptions{}); !errors.Is(err, ErrMissingAccountIBAN) {
		t.Fatalf("missing debtor IBAN: got %v, want ErrMissingAccountIBAN", err)
	}
}

// Filters: debtor, creditor, currency, and the half-open time window.
func TestPain001Filters(t *testing.T) {
	l := painTestLedger(t)
	base := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	mustSetIBAN(t, l, "dave", testIBANNL, "")
	mustPostTransferPain(t, l, "f1", "alice", "bob", 1000, "USD", base)
	mustPostTransferPain(t, l, "f2", "alice", "carol", 2000, "USD", base.Add(24*time.Hour))
	mustPostTransferPain(t, l, "f3", "dave", "bob", 3000, "EUR", base.Add(48*time.Hour))

	cases := []struct {
		name string
		opts Pain001ExportOptions
		want []string
		drop []string
	}{
		{"debtor", Pain001ExportOptions{Debtor: "alice", Currency: "USD"}, []string{"f1", "f2"}, []string{"f3"}},
		{"creditor", Pain001ExportOptions{Creditor: "bob", Currency: "USD"}, []string{"f1"}, []string{"f2", "f3"}},
		{"currency", Pain001ExportOptions{Currency: "EUR"}, []string{"f3"}, []string{"f1", "f2"}},
		{"window", Pain001ExportOptions{Since: base.Add(12 * time.Hour), Until: base.Add(36 * time.Hour)}, []string{"f2"}, []string{"f1", "f3"}},
		{"window-half-open", Pain001ExportOptions{Since: base, Until: base.Add(24 * time.Hour)}, []string{"f1"}, []string{"f2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := l.ExportPain001(tc.opts)
			if err != nil {
				t.Fatalf("ExportPain001: %v", err)
			}
			s := string(doc)
			for _, id := range tc.want {
				if !strings.Contains(s, "<EndToEndId>"+id+"</EndToEndId>") {
					t.Errorf("export missing %s\n%s", id, s)
				}
			}
			for _, id := range tc.drop {
				if strings.Contains(s, "<EndToEndId>"+id+"</EndToEndId>") {
					t.Errorf("export should not contain %s\n%s", id, s)
				}
			}
		})
	}

	if _, err := l.ExportPain001(Pain001ExportOptions{Since: base.Add(time.Hour), Until: base}); !errors.Is(err, ErrInvalidPain001Range) {
		t.Fatalf("inverted range: got %v, want ErrInvalidPain001Range", err)
	}
	if _, err := l.ExportPain001(Pain001ExportOptions{Currency: "XX"}); !errors.Is(err, ErrInvalidCurrency) {
		t.Fatalf("bad currency: got %v, want ErrInvalidCurrency", err)
	}
}

// Multi-currency exports group PmtInf per (debtor, currency); the
// GrpHdr control total is disclosed honestly as mixed.
func TestPain001MultiCurrency(t *testing.T) {
	al := mustAuditLog(t)
	defer al.Close()
	l := New(WithAuditLog(al))
	mustSetIBAN(t, l, "alice", testIBANDE, "")
	mustSetIBAN(t, l, "bob", testIBANGB, "")
	mustPostTransferPain(t, l, "x1", "alice", "bob", 10000, "USD", time.Time{})
	mustPostTransferPain(t, l, "x2", "alice", "bob", 9000, "EUR", time.Time{})

	doc, err := l.ExportPain001(Pain001ExportOptions{})
	if err != nil {
		t.Fatalf("ExportPain001: %v", err)
	}
	s := string(doc)
	if got := strings.Count(s, "<PmtInf>"); got != 2 {
		t.Errorf("PmtInf blocks = %d, want 2 (one per currency)", got)
	}
	if !strings.Contains(s, `<InstdAmt Ccy="USD">100.00</InstdAmt>`) ||
		!strings.Contains(s, `<InstdAmt Ccy="EUR">90.00</InstdAmt>`) {
		t.Errorf("per-currency amounts wrong\n%s", s)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, _, err := ReadAuditLog(al.Dir(), time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	for _, ev := range events {
		if ev.Op == "pain001_exported" && ev.Details["currency"] != "MIXED" {
			t.Errorf("mixed-currency export disclosed currency = %v, want MIXED", ev.Details["currency"])
		}
	}
}

// Split chunks register as individual payment instructions.
func TestPain001SplitChunks(t *testing.T) {
	l := New(WithMaxTransferChunkCents(10000))
	mustSetIBAN(t, l, "alice", testIBANDE, "")
	mustSetIBAN(t, l, "bob", testIBANGB, "")
	if _, err := l.PostTransferSplit(Transfer{ID: "sp1", From: "alice", To: "bob", AmountCents: 25000, Currency: "USD"}); err != nil {
		t.Fatalf("PostTransferSplit: %v", err)
	}
	doc, err := l.ExportPain001(Pain001ExportOptions{Currency: "USD"})
	if err != nil {
		t.Fatalf("ExportPain001: %v", err)
	}
	s := string(doc)
	// 25000 in 10000-cent chunks -> 10000 + 10000 + 5000.
	for _, id := range []string{"sp1/chunk-0", "sp1/chunk-1", "sp1/chunk-2"} {
		if !strings.Contains(s, "<EndToEndId>"+id+"</EndToEndId>") {
			t.Errorf("export missing chunk %s\n%s", id, s)
		}
	}
	if !strings.Contains(s, "<NbOfTxs>3</NbOfTxs>") || !strings.Contains(s, "<CtrlSum>250.00</CtrlSum>") {
		t.Errorf("totals should cover all three chunks\n%s", s)
	}
}

// Snapshot round-trip: the transfer registry and the IBAN registry
// survive export/import, and the export still works after a restore.
func TestPain001SnapshotRoundTrip(t *testing.T) {
	l := painTestLedger(t)
	mustPostTransferPain(t, l, "s1", "alice", "bob", 7777, "USD", time.Time{})

	var sb strings.Builder
	if err := l.ExportSnapshot(&sb); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	l2, err := ImportSnapshot(strings.NewReader(sb.String()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if _, ok := l2.AccountIBAN("alice"); !ok {
		t.Error("IBAN registry lost across snapshot round-trip")
	}
	doc, err := l2.ExportPain001(Pain001ExportOptions{Currency: "USD"})
	if err != nil {
		t.Fatalf("ExportPain001 after restore: %v", err)
	}
	s := string(doc)
	if !strings.Contains(s, "<EndToEndId>s1</EndToEndId>") || !strings.Contains(s, "<CtrlSum>77.77</CtrlSum>") {
		t.Errorf("transfer registry lost across snapshot round-trip\n%s", s)
	}
	// MsgId determinism across the round-trip: same content, same ID.
	doc1, _ := l.ExportPain001(Pain001ExportOptions{Currency: "USD"})
	if string(doc1) != s {
		t.Errorf("export not identical before/after snapshot round-trip")
	}
}

// Raw Post entries are not transfers: they never enter the registry.
func TestPain001IgnoresRawPosts(t *testing.T) {
	l := painTestLedger(t)
	if _, _, err := l.Post(JournalEntry{ID: "raw1", DebitAccount: "bob", CreditAccount: "alice", AmountCents: 5000, Currency: "USD"}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	doc, err := l.ExportPain001(Pain001ExportOptions{Currency: "USD"})
	if err != nil {
		t.Fatalf("ExportPain001: %v", err)
	}
	if strings.Contains(string(doc), "raw1") {
		t.Errorf("raw journal entry leaked into pain.001 export")
	}
	if !strings.Contains(string(doc), "<NbOfTxs>0</NbOfTxs>") {
		t.Errorf("empty export should report zero transactions\n%s", doc)
	}
}
