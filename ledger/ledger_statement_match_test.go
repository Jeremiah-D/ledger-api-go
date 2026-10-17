package ledger

import (
	"strings"
	"testing"
	"time"
)

// statementMatchDoc renders a minimal SGML-style OFX statement for the
// matcher tests: one line per (fitid, trntype, amount, memo, ref) tuple,
// all posted at the same DTPOSTED.
func statementMatchDoc(posted string, lines ...[5]string) string {
	var sb strings.Builder
	sb.WriteString(`OFXHEADER:100
DATA:OFXSGML
VERSION:102
SECURITY:NONE
ENCODING:USASCII
CHARSET:1252
COMPRESSION:NONE
OLDFILEUID:NONE
NEWFILEUID:NONE

<OFX>
<BANKMSGSRSV1>
<STMTTRNRS>
<STATUS>
<CODE>0
<SEVERITY>INFO
</STATUS>
<STMTRS>
<CURDEF>USD
<BANKACCTFROM>
<BANKID>LEDGER
<ACCTID>cash
<ACCTTYPE>CHECKING
</BANKACCTFROM>
<BANKTRANLIST>
`)
	for _, l := range lines {
		sb.WriteString("<STMTTRN>\n<TRNTYPE>" + l[1] + "\n<DTPOSTED>" + posted + "\n<TRNAMT>" + l[2] +
			"\n<FITID>" + l[0] + "\n<MEMO>" + l[3] + "\n<REFNUM>" + l[4] + "\n</STMTTRN>\n")
	}
	sb.WriteString(`</BANKTRANLIST>
</STMTRS>
</STMTTRNRS>
</BANKMSGSRSV1>
</OFX>
`)
	return sb.String()
}

func mustPostMatch(t *testing.T, l *Ledger, e JournalEntry) {
	t.Helper()
	if _, dup, err := l.Post(e); err != nil || dup {
		t.Fatalf("Post(%s) = dup=%v err=%v", e.ID, dup, err)
	}
}

// A statement line parsed from a real OFX document matches the booked
// row exactly: amount, date, and external ref all agree, so the score is
// a perfect 1 and the matched entry ID names the journaled row.
func TestStatementMatchExactSuccess(t *testing.T) {
	l := New()
	base := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	mustPostMatch(t, l, JournalEntry{
		ID: "m-1", DebitAccount: "cash", CreditAccount: "equity",
		AmountCents: 15000, Currency: "USD",
		Memo: "payroll oct", ExternalRef: "PAY-1042", CreatedAt: base,
	})

	doc := statementMatchDoc("20261016120000",
		[5]string{"fit-1", "CREDIT", "150.00", "payroll oct", "PAY-1042"})
	txns, err := ParseOFX([]byte(doc))
	if err != nil {
		t.Fatalf("ParseOFX: %v", err)
	}
	if len(txns) != 1 {
		t.Fatalf("parsed %d transactions, want 1", len(txns))
	}
	line := NewStatementLineFromOFX(txns[0], "docsha-1")
	if line.ID != "ofx/fit-1" {
		t.Fatalf("line ID = %q, want the stable ofx/<FITID> identity", line.ID)
	}

	report, err := l.MatchStatement("cash", []StatementLine{line}, StatementMatchOptions{})
	if err != nil {
		t.Fatalf("MatchStatement: %v", err)
	}
	if report.Matched != 1 || report.Ambiguous != 0 || report.Unmatched != 0 {
		t.Fatalf("counts = %d/%d/%d, want 1/0/0",
			report.Matched, report.Ambiguous, report.Unmatched)
	}
	item := report.Items[0]
	if item.Status != StatementMatched {
		t.Fatalf("status = %q, want matched", item.Status)
	}
	if item.MatchedEntryID != "m-1" {
		t.Errorf("matched_entry_id = %q, want m-1", item.MatchedEntryID)
	}
	if item.Score < 0.99 || item.Score > 1.0 {
		t.Errorf("score = %v, want ~1.0 for an exact amount/date/ref match", item.Score)
	}
}

// The amount tolerance is inclusive on both sides: a line 5 cents off
// with a 5-cent tolerance is a candidate; 6 cents off is not.
func TestStatementMatchAmountToleranceBoundary(t *testing.T) {
	l := New()
	base := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	mustPostMatch(t, l, JournalEntry{
		ID: "t-1", DebitAccount: "cash", CreditAccount: "equity",
		AmountCents: 10000, Currency: "USD", CreatedAt: base,
	})
	mustPostMatch(t, l, JournalEntry{
		ID: "t-2", DebitAccount: "cash", CreditAccount: "equity",
		AmountCents: 20000, Currency: "USD", CreatedAt: base,
	})

	lines := []StatementLine{
		{ID: "tol-in", Account: "cash", Currency: "USD", Direction: StatementDirectionIn,
			AmountCents: 10005, PostedAt: base},
		{ID: "tol-out", Account: "cash", Currency: "USD", Direction: StatementDirectionIn,
			AmountCents: 20006, PostedAt: base},
	}
	report, err := l.MatchStatement("cash", lines, StatementMatchOptions{AmountToleranceCents: 5})
	if err != nil {
		t.Fatalf("MatchStatement: %v", err)
	}
	if report.Matched != 1 || report.Unmatched != 1 {
		t.Fatalf("counts = %d/%d/%d, want 1/0/1",
			report.Matched, report.Ambiguous, report.Unmatched)
	}
	if got := report.Items[0]; got.Status != StatementMatched || got.MatchedEntryID != "t-1" {
		t.Errorf("tol-in: status=%q entry=%q, want matched/t-1", got.Status, got.MatchedEntryID)
	}
	if got := report.Items[1]; got.Status != StatementUnmatched {
		t.Errorf("tol-out: status=%q, want unmatched (6c > 5c tolerance)", got.Status)
	}
}

// The date window is inclusive: a line exactly one window away is a
// candidate; one second past it is not.
func TestStatementMatchDateWindowBoundary(t *testing.T) {
	l := New()
	base := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	mustPostMatch(t, l, JournalEntry{
		ID: "w-1", DebitAccount: "cash", CreditAccount: "equity",
		AmountCents: 7000, Currency: "USD", CreatedAt: base,
	})

	window := time.Hour
	lines := []StatementLine{
		{ID: "win-edge", Account: "cash", Currency: "USD", Direction: StatementDirectionIn,
			AmountCents: 7000, PostedAt: base.Add(window)},
		{ID: "win-past", Account: "cash", Currency: "USD", Direction: StatementDirectionIn,
			AmountCents: 7000, PostedAt: base.Add(window + time.Second)},
	}
	report, err := l.MatchStatement("cash", lines, StatementMatchOptions{MatchWindow: window})
	if err != nil {
		t.Fatalf("MatchStatement: %v", err)
	}
	if got := report.Items[0]; got.Status != StatementMatched {
		t.Errorf("win-edge: status=%q, want matched (dt == window is inside)", got.Status)
	}
	if got := report.Items[1]; got.Status != StatementUnmatched {
		t.Errorf("win-past: status=%q, want unmatched (dt == window+1s)", got.Status)
	}
}

// Two rows compete for one line: the outcome is ambiguous, and the
// candidate carrying the line's external ref ranks first by score.
func TestStatementMatchAmbiguous(t *testing.T) {
	l := New()
	base := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	mustPostMatch(t, l, JournalEntry{
		ID: "a-1", DebitAccount: "cash", CreditAccount: "equity",
		AmountCents: 5000, Currency: "USD",
		Memo: "acme invoice 99", ExternalRef: "ACME-99", CreatedAt: base,
	})
	mustPostMatch(t, l, JournalEntry{
		ID: "a-2", DebitAccount: "cash", CreditAccount: "equity",
		AmountCents: 5000, Currency: "USD",
		Memo: "unrelated transfer", CreatedAt: base.Add(30 * time.Minute),
	})

	line := StatementLine{
		ID: "amb-1", Account: "cash", Currency: "USD", Direction: StatementDirectionIn,
		AmountCents: 5000, PostedAt: base.Add(15 * time.Minute),
		Memo: "acme invoice", ExternalRef: "ACME-99",
	}
	report, err := l.MatchStatement("cash", []StatementLine{line}, StatementMatchOptions{})
	if err != nil {
		t.Fatalf("MatchStatement: %v", err)
	}
	if report.Ambiguous != 1 || report.Matched != 0 || report.Unmatched != 0 {
		t.Fatalf("counts = %d/%d/%d, want 0/1/0",
			report.Matched, report.Ambiguous, report.Unmatched)
	}
	item := report.Items[0]
	if item.Status != StatementAmbiguous {
		t.Fatalf("status = %q, want ambiguous", item.Status)
	}
	if len(item.Candidates) != 2 {
		t.Fatalf("candidates = %d, want 2", len(item.Candidates))
	}
	// The exact external-ref hit outranks the ref-less row.
	if item.Candidates[0].EntryID != "a-1" || item.Candidates[1].EntryID != "a-2" {
		t.Errorf("candidate order = %q/%q, want a-1 first (external_ref bonus)",
			item.Candidates[0].EntryID, item.Candidates[1].EntryID)
	}
	if item.Score != item.Candidates[0].Score {
		t.Errorf("item score = %v, want the best candidate's %v",
			item.Score, item.Candidates[0].Score)
	}
	if item.MatchedEntryID != "" {
		t.Errorf("matched_entry_id = %q on an ambiguous line, want empty", item.MatchedEntryID)
	}
}

// An unmatched line enters the Reconcile discrepancies through the same
// TrialBalanceDiscrepancy mechanism the equation check uses: the books
// balance internally, and the row carries the bank's claim as the
// difference. Booking the missing row later resolves the line and the
// discrepancy disappears.
func TestStatementMatchUnmatchedSurfacesInReconcile(t *testing.T) {
	l := New()
	base := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	mustPostMatch(t, l, JournalEntry{
		ID: "m-1", DebitAccount: "cash", CreditAccount: "equity",
		AmountCents: 10000, Currency: "USD", CreatedAt: base,
	})
	lines := []StatementLine{
		{ID: "u-1", Account: "cash", Currency: "USD", Direction: StatementDirectionIn,
			AmountCents: 10000, PostedAt: base},
		{ID: "u-2", Account: "cash", Currency: "USD", Direction: StatementDirectionOut,
			AmountCents: 2500, PostedAt: base.Add(2 * time.Hour), Memo: "bank fee"},
	}

	report, err := l.MatchStatement("cash", lines, StatementMatchOptions{})
	if err != nil {
		t.Fatalf("MatchStatement: %v", err)
	}
	if report.Matched != 1 || report.Unmatched != 1 {
		t.Fatalf("counts = %d/%d/%d, want 1/0/1",
			report.Matched, report.Ambiguous, report.Unmatched)
	}

	rec := l.Reconcile(base.Add(3 * time.Hour))
	if !rec.AccountingEquationOK {
		t.Fatalf("equation red on a healthy ledger: %q", rec.AccountingError)
	}
	var stmtRow *TrialBalanceDiscrepancy
	for i := range rec.Discrepancies {
		if rec.Discrepancies[i].DifferenceCents == -2500 {
			stmtRow = &rec.Discrepancies[i]
		}
	}
	if stmtRow == nil {
		t.Fatalf("no statement discrepancy row; discrepancies = %+v", rec.Discrepancies)
	}
	// The books are internally consistent: net == debits - credits.
	if stmtRow.Account != "cash" || stmtRow.Currency != "USD" {
		t.Errorf("row = %q/%q, want cash/USD", stmtRow.Account, stmtRow.Currency)
	}
	if stmtRow.TotalDebitsCents != 10000 || stmtRow.TotalCreditsCents != 0 ||
		stmtRow.NetBalanceCents != 10000 {
		t.Errorf("book figures = %d/%d/%d, want the ledger's 10000/0/10000",
			stmtRow.TotalDebitsCents, stmtRow.TotalCreditsCents, stmtRow.NetBalanceCents)
	}
	if stmtRow.ExpectedNetCents != 7500 {
		t.Errorf("expected net = %d, want 7500 (books minus the 2500 outflow the bank claims)",
			stmtRow.ExpectedNetCents)
	}

	// The operator books the missing row; re-matching resolves the line
	// and the discrepancy clears.
	mustPostMatch(t, l, JournalEntry{
		ID: "m-2", DebitAccount: "expenses", CreditAccount: "cash",
		AmountCents: 2500, Currency: "USD", CreatedAt: base.Add(2 * time.Hour),
	})
	report2, err := l.MatchStatement("cash", lines, StatementMatchOptions{})
	if err != nil {
		t.Fatalf("MatchStatement (re-run): %v", err)
	}
	if report2.Matched != 2 || report2.Unmatched != 0 {
		t.Fatalf("re-run counts = %d/%d/%d, want 2/0/0",
			report2.Matched, report2.Ambiguous, report2.Unmatched)
	}
	if got := report2.Items[1]; got.MatchedEntryID != "m-2" {
		t.Errorf("u-2 re-run: matched_entry_id = %q, want m-2", got.MatchedEntryID)
	}
	// u-1 replayed its recorded match, not a fresh scan.
	if got := report2.Items[0]; got.MatchedEntryID != "m-1" {
		t.Errorf("u-1 re-run: matched_entry_id = %q, want the recorded m-1", got.MatchedEntryID)
	}
	if rec2 := l.Reconcile(base.Add(4 * time.Hour)); len(rec2.Discrepancies) != 0 {
		t.Errorf("discrepancies after resolution = %+v, want empty", rec2.Discrepancies)
	}
	if n := len(l.statementMatches); n != 2 {
		t.Errorf("registry holds %d rows, want 2 (re-imports never duplicate)", n)
	}
}

// Re-importing the same statement replays recorded matches and keeps a
// single registry row per line: matched lines never double-match, and an
// unmatched line surfaces exactly one Reconcile discrepancy.
func TestStatementMatchIdempotentReimport(t *testing.T) {
	l := New()
	base := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	mustPostMatch(t, l, JournalEntry{
		ID: "i-1", DebitAccount: "cash", CreditAccount: "equity",
		AmountCents: 9000, Currency: "USD", CreatedAt: base,
	})
	lines := []StatementLine{
		{ID: "i-line-1", Account: "cash", Currency: "USD", Direction: StatementDirectionIn,
			AmountCents: 9000, PostedAt: base},
		{ID: "i-line-2", Account: "cash", Currency: "USD", Direction: StatementDirectionOut,
			AmountCents: 111, PostedAt: base},
	}

	first, err := l.MatchStatement("cash", lines, StatementMatchOptions{})
	if err != nil {
		t.Fatalf("MatchStatement: %v", err)
	}
	second, err := l.MatchStatement("cash", lines, StatementMatchOptions{})
	if err != nil {
		t.Fatalf("MatchStatement (re-import): %v", err)
	}
	for i := range first.Items {
		a, b := first.Items[i], second.Items[i]
		if a.Status != b.Status || a.MatchedEntryID != b.MatchedEntryID || a.Score != b.Score {
			t.Errorf("item %d differs across re-imports: %+v vs %+v", i, a, b)
		}
	}
	if n := len(l.statementMatches); n != 2 {
		t.Errorf("registry holds %d rows after re-import, want 2", n)
	}

	// One unmatched line, one discrepancy — never two.
	rec := l.Reconcile(base.Add(time.Hour))
	count := 0
	for _, d := range rec.Discrepancies {
		if d.DifferenceCents == -111 {
			count++
		}
	}
	if count != 1 {
		t.Errorf("statement discrepancy rows for i-line-2 = %d, want exactly 1", count)
	}
}

// A line naming a different account than the match target is rejected,
// like the statement imports reject cross-account documents.
func TestStatementMatchAccountMismatch(t *testing.T) {
	l := New()
	line := StatementLine{
		ID: "x-1", Account: "other", Currency: "USD", Direction: StatementDirectionIn,
		AmountCents: 100, PostedAt: time.Now().UTC(),
	}
	if _, err := l.MatchStatement("cash", []StatementLine{line}, StatementMatchOptions{}); err == nil {
		t.Errorf("MatchStatement with a foreign-account line: want an error, got nil")
	}
	if _, err := l.MatchStatement("", []StatementLine{line}, StatementMatchOptions{}); err == nil {
		t.Errorf("MatchStatement with empty account: want ErrEmptyAccountID, got nil")
	}
}

// A camt.053 entry normalizes into a statement line with the stable
// camt053/<stmtID>/<seq> identity and matches like an OFX line.
func TestStatementMatchCAMT053Line(t *testing.T) {
	l := New()
	base := time.Date(2026, 10, 16, 9, 30, 0, 0, time.UTC)
	mustPostMatch(t, l, JournalEntry{
		ID: "c-1", DebitAccount: "cash", CreditAccount: "equity",
		AmountCents: 4200, Currency: "USD",
		Memo: "wire transfer", ExternalRef: "E2E-777", CreatedAt: base,
	})
	txn := CAMT053Transaction{
		Account: "cash", Currency: "USD", StmtID: "stmt-9", Seq: 3,
		Direction: CAMT053DirectionIn, AmountCents: 4200, PostedAt: base,
		Memo: "wire transfer", EndToEndID: "E2E-777",
	}
	line := NewStatementLineFromCAMT053(txn, "camtsha-1")
	if line.ID != "camt053/stmt-9/3" {
		t.Fatalf("line ID = %q, want camt053/<stmtID>/<seq>", line.ID)
	}
	report, err := l.MatchStatement("cash", []StatementLine{line}, StatementMatchOptions{})
	if err != nil {
		t.Fatalf("MatchStatement: %v", err)
	}
	if report.Matched != 1 || report.Items[0].MatchedEntryID != "c-1" {
		t.Errorf("camt053 line: matched=%d entry=%q, want 1/c-1",
			report.Matched, report.Items[0].MatchedEntryID)
	}
}
