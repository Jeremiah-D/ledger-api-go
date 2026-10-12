package ledger

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func mustPostOFX(t *testing.T, l *Ledger, id string, debit, credit AccountID, cents int64, currency string, at time.Time) JournalEntry {
	t.Helper()
	e, dup, err := l.Post(JournalEntry{
		ID:           id,
		DebitAccount: debit,
		CreditAccount: credit,
		AmountCents:  cents,
		Currency:     currency,
		CreatedAt:    at,
	})
	if err != nil {
		t.Fatalf("Post(%s): %v", id, err)
	}
	if dup {
		t.Fatalf("Post(%s): unexpected duplicate", id)
	}
	return e
}

// The OFX view is account-centric: the account as the debit leg means
// money was added to it (CREDIT, positive amount); the account as the
// credit leg means money left it (DEBIT, negative amount). Debit always
// increases the account's balance in this ledger's bookkeeping, so the
// mapping is the honest one.
func TestOFXTrnTypeSignMapping(t *testing.T) {
	l := New()
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	mustPostOFX(t, l, "e1", "cash", "equity", 15000, "USD", base)
	mustPostOFX(t, l, "e2", "rent", "cash", 20000, "USD", base.Add(time.Hour))

	doc, checkpoint, err := l.ExportOFX(OFXExportOptions{Account: "cash"})
	if err != nil {
		t.Fatalf("ExportOFX: %v", err)
	}
	s := string(doc)
	if checkpoint != "e2" {
		t.Fatalf("checkpoint = %q, want %q", checkpoint, "e2")
	}
	for _, want := range []string{
		"OFXHEADER:100", "VERSION:211", "<OFX>", "</OFX>",
		"<CURDEF>USD</CURDEF>",
		"<TRNTYPE>CREDIT</TRNTYPE>", "<TRNAMT>150.00</TRNAMT>", "<FITID>e1</FITID>",
		"<TRNTYPE>DEBIT</TRNTYPE>", "<TRNAMT>-200.00</TRNAMT>", "<FITID>e2</FITID>",
		"<DTPOSTED>20261001120000</DTPOSTED>",
		"<DTPOSTED>20261001130000</DTPOSTED>",
		"<BALAMT>-50.00</BALAMT>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("OFX doc missing %q\n%s", want, s)
		}
	}
	// The account-centric mapping must be exact: e1 is CREDIT (cash was
	// the debit leg), e2 is DEBIT (cash was the credit leg).
	if strings.Count(s, "<TRNTYPE>CREDIT</TRNTYPE>") != 1 || strings.Count(s, "<TRNTYPE>DEBIT</TRNTYPE>") != 1 {
		t.Errorf("TRNTYPE mapping not exact\n%s", s)
	}
}

func TestOFXMemoRefnumEscaping(t *testing.T) {
	l := New()
	base := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	e := JournalEntry{
		ID:           "e9",
		DebitAccount: "cash",
		CreditAccount: "equity",
		AmountCents:  100,
		Currency:     "USD",
		Memo:         "a & b <c> \"d\"",
		ExternalRef:  "ref&<1>",
		CreatedAt:    base,
	}
	if _, _, err := l.Post(e); err != nil {
		t.Fatalf("Post: %v", err)
	}
	doc, _, err := l.ExportOFX(OFXExportOptions{Account: "cash", Currency: "USD"})
	if err != nil {
		t.Fatalf("ExportOFX: %v", err)
	}
	s := string(doc)
	if !strings.Contains(s, "<MEMO>a &amp; b &lt;c&gt; &quot;d&quot;</MEMO>") {
		t.Errorf("memo not XML-escaped\n%s", s)
	}
	if !strings.Contains(s, "<REFNUM>ref&amp;&lt;1&gt;</REFNUM>") {
		t.Errorf("refnum not XML-escaped\n%s", s)
	}
	if strings.Contains(s, "a & b") {
		t.Errorf("raw unescaped memo text leaked into OFX\n%s", s)
	}
}

// Multiple currencies land in separate statement segments, sorted by
// currency code: the per-currency books never mix.
func TestOFXMultiCurrencySegmentation(t *testing.T) {
	l := New()
	base := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	mustPostOFX(t, l, "u1", "cash", "equity", 10000, "USD", base)
	mustPostOFX(t, l, "e1", "cash", "equity", 9000, "EUR", base)
	mustPostOFX(t, l, "c1", "cash", "equity", 130000, "CNY", base)

	doc, _, err := l.ExportOFX(OFXExportOptions{Account: "cash"})
	if err != nil {
		t.Fatalf("ExportOFX: %v", err)
	}
	s := string(doc)
	if got := strings.Count(s, "<STMTTRNRS>"); got != 3 {
		t.Fatalf("statement segments = %d, want 3", got)
	}
	usd := strings.Index(s, "<CURDEF>USD</CURDEF>")
	eur := strings.Index(s, "<CURDEF>EUR</CURDEF>")
	cny := strings.Index(s, "<CURDEF>CNY</CURDEF>")
	if usd < 0 || eur < 0 || cny < 0 || !(cny < eur && eur < usd) {
		t.Fatalf("segments not sorted by currency code\n%s", s)
	}
	if !strings.Contains(s, "<TRNAMT>1300.00</TRNAMT>") {
		t.Errorf("CNY amount not formatted as decimal yuan\n%s", s)
	}
}

func TestOFXTimeRangeWindow(t *testing.T) {
	l := New()
	base := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	mustPostOFX(t, l, "d1", "cash", "equity", 10000, "USD", base)
	mustPostOFX(t, l, "d2", "cash", "equity", 20000, "USD", base.Add(48*time.Hour))
	mustPostOFX(t, l, "d3", "cash", "equity", 30000, "USD", base.Add(96*time.Hour))

	doc, _, err := l.ExportOFX(OFXExportOptions{
		Account: "cash",
		Since:   base.Add(24 * time.Hour),
		Until:   base.Add(72 * time.Hour),
	})
	if err != nil {
		t.Fatalf("ExportOFX: %v", err)
	}
	s := string(doc)
	if strings.Count(s, "<STMTTRN>") != 1 || !strings.Contains(s, "<FITID>d2</FITID>") {
		t.Fatalf("window should contain only d2\n%s", s)
	}
	// DTSTART/DTEND reflect the requested half-open window...
	if !strings.Contains(s, "<DTSTART>20261005000000</DTSTART>") ||
		!strings.Contains(s, "<DTEND>20261007000000</DTEND>") {
		t.Errorf("DTSTART/DTEND do not reflect the requested window\n%s", s)
	}
	// ...and the balance folds history strictly before until (d1 + d2).
	if !strings.Contains(s, "<BALAMT>300.00</BALAMT>") {
		t.Errorf("LEDGERBAL should fold entries before until (100+200)\n%s", s)
	}
}

// Long statements page by FITID checkpoint: the pages concatenate to
// the full ordered list with no duplicates and no skips, even with
// interleaved currencies.
func TestOFXResumeCheckpoint(t *testing.T) {
	l := New()
	base := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	ids := []string{}
	for i := 0; i < 6; i++ {
		id := fmt.Sprintf("p%d", i)
		cur := "USD"
		if i%2 == 1 {
			cur = "EUR"
		}
		mustPostOFX(t, l, id, "cash", "equity", int64(1000*(i+1)), cur, base.Add(time.Duration(i)*time.Minute))
		ids = append(ids, id)
	}

	doc1, cp1, err := l.ExportOFX(OFXExportOptions{Account: "cash", AfterEntryID: "p1"})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if cp1 != "p5" {
		t.Fatalf("page-1 checkpoint = %q, want p5", cp1)
	}
	s1 := string(doc1)
	for _, id := range []string{"p2", "p3", "p4", "p5"} {
		if !strings.Contains(s1, "<FITID>"+id+"</FITID>") {
			t.Errorf("page 1 missing FITID %s", id)
		}
	}
	if strings.Contains(s1, "<FITID>p0</FITID>") || strings.Contains(s1, "<FITID>p1</FITID>") {
		t.Errorf("page 1 leaked rows at or before the checkpoint")
	}

	doc2, cp2, err := l.ExportOFX(OFXExportOptions{Account: "cash", AfterEntryID: cp1})
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if cp2 != "" {
		t.Fatalf("page-2 checkpoint = %q, want empty (no rows left)", cp2)
	}
	if strings.Count(string(doc2), "<STMTTRN>") != 0 {
		t.Errorf("page 2 should be empty\n%s", doc2)
	}

	// A full export and the paged rows must contain exactly the same
	// FITIDs: no dup, no skip.
	full, _, err := l.ExportOFX(OFXExportOptions{Account: "cash"})
	if err != nil {
		t.Fatalf("full export: %v", err)
	}
	sf := string(full)
	for _, id := range ids {
		if strings.Count(sf, "<FITID>"+id+"</FITID>") != 1 {
			t.Errorf("FITID %s not exactly-once in full export", id)
		}
	}

	// An unknown checkpoint fails loud instead of silently re-exporting
	// from the start (which would duplicate rows downstream).
	if _, _, err := l.ExportOFX(OFXExportOptions{Account: "cash", AfterEntryID: "nope"}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("unknown checkpoint: got %v, want ErrInvalidCursor", err)
	}
}

// Pending-review rows are journal rows that never touched balances: the
// statement must not show them, and the folded LEDGERBAL must agree
// with the live balance.
func TestOFXExcludesPendingReview(t *testing.T) {
	l := New()
	if _, _, err := l.Post(JournalEntry{ID: "settled", DebitAccount: "cash", CreditAccount: "equity", AmountCents: 50000, Currency: "USD"}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if err := l.SetReviewThreshold("cash", 1000); err != nil {
		t.Fatalf("SetReviewThreshold: %v", err)
	}
	if _, err := l.PostTransfer(Transfer{ID: "t1", From: "cash", To: "rent", AmountCents: 90000, Currency: "USD"}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if got := l.Balance("cash"); got != 50000 {
		t.Fatalf("Balance(cash) = %d, want 50000 (review pending is not effective)", got)
	}
	doc, _, err := l.ExportOFX(OFXExportOptions{Account: "cash", Currency: "USD"})
	if err != nil {
		t.Fatalf("ExportOFX: %v", err)
	}
	s := string(doc)
	if strings.Count(s, "<STMTTRN>") != 1 || !strings.Contains(s, "<FITID>settled</FITID>") {
		t.Errorf("statement should show only the effective row\n%s", s)
	}
	if !strings.Contains(s, "<BALAMT>500.00</BALAMT>") {
		t.Errorf("LEDGERBAL must agree with the live balance\n%s", s)
	}
}

func TestOFXEmptyAndInvalid(t *testing.T) {
	l := New()

	// Unknown accounts export a valid empty statement, not an error.
	doc, cp, err := l.ExportOFX(OFXExportOptions{Account: "ghost", Currency: "USD"})
	if err != nil {
		t.Fatalf("ExportOFX unknown account: %v", err)
	}
	if cp != "" {
		t.Fatalf("checkpoint for empty statement = %q, want empty", cp)
	}
	s := string(doc)
	if !strings.Contains(s, "<CURDEF>USD</CURDEF>") || strings.Contains(s, "<STMTTRN>") {
		t.Fatalf("empty statement malformed\n%s", s)
	}
	if !strings.Contains(s, "<BALAMT>0.00</BALAMT>") {
		t.Fatalf("empty statement balance not zero\n%s", s)
	}

	if _, _, err := l.ExportOFX(OFXExportOptions{}); !errors.Is(err, ErrEmptyAccountID) {
		t.Fatalf("empty account: got %v, want ErrEmptyAccountID", err)
	}
	if _, _, err := l.ExportOFX(OFXExportOptions{Account: "cash", Currency: "XX"}); !errors.Is(err, ErrInvalidCurrency) {
		t.Fatalf("bad currency: got %v, want ErrInvalidCurrency", err)
	}
	base := time.Now()
	if _, _, err := l.ExportOFX(OFXExportOptions{Account: "cash", Since: base.Add(time.Hour), Until: base}); !errors.Is(err, ErrInvalidOFXRange) {
		t.Fatalf("inverted range: got %v, want ErrInvalidOFXRange", err)
	}
}

// Regression: the resume checkpoint is matched by scan, not binary
// search — rows are sorted by (CreatedAt, ID), which is not ID-monotonic,
// so a checkpoint whose ID sorts earlier than its position must still
// resume correctly.
func TestOFXCheckpointNotIDOrdered(t *testing.T) {
	l := New()
	base := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	// IDs deliberately out of time order: "z" posted first, "a" second.
	mustPostOFX(t, l, "z", "cash", "equity", 100, "USD", base)
	mustPostOFX(t, l, "a", "cash", "equity", 200, "USD", base.Add(time.Minute))
	mustPostOFX(t, l, "m", "cash", "equity", 300, "USD", base.Add(2*time.Minute))

	_, cp, err := l.ExportOFX(OFXExportOptions{Account: "cash", Currency: "USD"})
	if err != nil {
		t.Fatalf("full export: %v", err)
	}
	if cp != "m" {
		t.Fatalf("checkpoint = %q, want m", cp)
	}
	// Resume from "z" (time-first, ID-last): must yield a, m.
	doc, cp2, err := l.ExportOFX(OFXExportOptions{Account: "cash", Currency: "USD", AfterEntryID: "z"})
	if err != nil {
		t.Fatalf("resume from z: %v", err)
	}
	if cp2 != "m" {
		t.Fatalf("resume checkpoint = %q, want m", cp2)
	}
	s := string(doc)
	if strings.Count(s, "<STMTTRN>") != 2 || !strings.Contains(s, "<FITID>a</FITID>") || !strings.Contains(s, "<FITID>m</FITID>") {
		t.Fatalf("resume from z should yield a, m\n%s", s)
	}
	// Resume from "a": yields only m.
	doc, _, err = l.ExportOFX(OFXExportOptions{Account: "cash", Currency: "USD", AfterEntryID: "a"})
	if err != nil {
		t.Fatalf("resume from a: %v", err)
	}
	if strings.Count(string(doc), "<STMTTRN>") != 1 || !strings.Contains(string(doc), "<FITID>m</FITID>") {
		t.Fatalf("resume from a should yield only m\n%s", doc)
	}
}

// Same CreatedAt ordering ties break on ID: deterministic page order.
func TestOFXSameTimestampOrderDeterministic(t *testing.T) {	l := New()
	at := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	for _, id := range []string{"z", "m", "a"} {
		mustPostOFX(t, l, id, "cash", "equity", 100, "USD", at)
	}
	doc, _, err := l.ExportOFX(OFXExportOptions{Account: "cash", Currency: "USD"})
	if err != nil {
		t.Fatalf("ExportOFX: %v", err)
	}
	s := string(doc)
	ia, im, iz := strings.Index(s, "<FITID>a</FITID>"), strings.Index(s, "<FITID>m</FITID>"), strings.Index(s, "<FITID>z</FITID>")
	if !(ia < im && im < iz) {
		t.Fatalf("same-timestamp rows not ordered by ID\n%s", s)
	}
}
