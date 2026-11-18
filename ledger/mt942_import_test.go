package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// mt942Doc renders a minimal MT942 interim transaction report for
// the tests: :20: :25: :28C: [:34F:] :13D: (:61: [:86:])* :90D:
// :90C:. lineBlocks are the full ":61:..." / ":86:..." lines in
// order; empty floor omits :34F:.
func mt942Doc(ref, acct, floor, dt13 string, lineBlocks []string, debit, credit string) string {
	lines := []string{":20:" + ref, ":25:" + acct, ":28C:1/1"}
	if floor != "" {
		lines = append(lines, ":34F:"+floor)
	}
	lines = append(lines, ":13D:"+dt13)
	lines = append(lines, lineBlocks...)
	lines = append(lines, ":90D:"+debit, ":90C:"+credit)
	return strings.Join(lines, "\n")
}

// mt942Seed posts one journal row for the tests.
func mt942Seed(t *testing.T, l *Ledger, id, debit, credit string, cents int64, at time.Time) {
	t.Helper()
	if _, _, err := l.Post(JournalEntry{
		ID: id, DebitAccount: AccountID(debit), CreditAccount: AccountID(credit),
		AmountCents: cents, Currency: "USD", CreatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
}

// Round-trip: a report our own ExportMT942 renders parses back to
// the same statement, and importing it reports zero mismatches —
// including through the mt942-system external mapping, which names
// the account by its IBAN in :25:.
func TestParseMT942RoundTrip(t *testing.T) {
	l := New()
	iban := "DE89370400440532013000"
	if err := l.SetExternalMapping("acct1", ExternalSystemMT942, iban, "test"); err != nil {
		t.Fatal(err)
	}
	post := func(id, debit, credit string, cents int64, at time.Time, pending bool) {
		if _, _, err := l.Post(JournalEntry{
			ID: id, DebitAccount: AccountID(debit), CreditAccount: AccountID(credit),
			AmountCents: cents, Currency: "USD", CreatedAt: at, PendingReview: pending,
		}); err != nil {
			t.Fatal(err)
		}
	}
	post("bal-e0", "acct1", "bank", 40000, time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC), false)
	post("bal-e1", "acct1", "bank", 100000, time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC), false)
	post("bal-e2", "bank", "acct1", 20000, time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC), false)
	post("bal-e3", "acct1", "bank", 50000, time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC), false)
	post("bal-e4", "acct1", "bank", 99900, time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC), true)

	doc, err := l.ExportMT942(MT942ExportOptions{
		Account: "acct1", StatementID: "1", SequenceID: "1",
		Since:      time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		Until:      time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC),
		Currency:   "USD",
		FloorLimit: &MT942FloorLimit{Credit: true, Currency: "USD", AmountCents: 100000},
	})
	if err != nil {
		t.Fatal(err)
	}
	// CRLF is tolerated: the bank's downloader may hand us CRLF.
	stmt, err := ParseMT942(strings.ReplaceAll(doc, "\n", "\r\n"))
	if err != nil {
		t.Fatalf("ParseMT942(round-trip): %v\ndoc:\n%s", err, doc)
	}
	if stmt.TransactionReference != "1" || stmt.AccountIdentification != iban {
		t.Errorf("header = %q/%q, want 1/%s", stmt.TransactionReference, stmt.AccountIdentification, iban)
	}
	if stmt.StatementNumber != "1" || stmt.SequenceNumber != "1" {
		t.Errorf(":28C: = %q/%q, want 1/1", stmt.StatementNumber, stmt.SequenceNumber)
	}
	if want := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC); !stmt.ReportDateTime.Equal(want) {
		t.Errorf(":13D: = %v, want %v", stmt.ReportDateTime, want)
	}
	if stmt.FloorLimit == nil || *stmt.FloorLimit != (MT942FloorLimit{Credit: true, Currency: "USD", AmountCents: 100000}) {
		t.Errorf(":34F: = %+v, want {true USD 100000}", stmt.FloorLimit)
	}
	if len(stmt.Lines) != 3 {
		t.Fatalf("lines = %d, want 3", len(stmt.Lines))
	}
	if stmt.Lines[0].Reference != "bal-e1" || stmt.Lines[1].Reference != "bal-e2" || stmt.Lines[2].Reference != "bal-e3" {
		t.Errorf("line refs = %q/%q/%q, want bal-e1/bal-e2/bal-e3", stmt.Lines[0].Reference, stmt.Lines[1].Reference, stmt.Lines[2].Reference)
	}
	if stmt.DebitSummary != (MT942Summary{Count: 1, AmountCents: 20000}) {
		t.Errorf(":90D: = %+v, want {1 20000}", stmt.DebitSummary)
	}
	if stmt.CreditSummary != (MT942Summary{Count: 2, AmountCents: 150000}) {
		t.Errorf(":90C: = %+v, want {2 150000}", stmt.CreditSummary)
	}

	rep, err := l.ImportMT942("acct1", []byte(doc))
	if err != nil {
		t.Fatalf("ImportMT942(round-trip): %v", err)
	}
	if rep.Matched != 3 || rep.Mismatched != 0 || rep.Unmatched != 0 {
		t.Errorf("matches = %d/%d/%d, want 3/0/0", rep.Matched, rep.Mismatched, rep.Unmatched)
	}
	if len(rep.SummaryMismatches) != 0 {
		t.Errorf("summary mismatches = %+v, want none", rep.SummaryMismatches)
	}
	if rep.Currency != "USD" || rep.DebitTotalCents != 20000 || rep.CreditTotalCents != 150000 {
		t.Errorf("report = %s %d/%d, want USD 20000/150000", rep.Currency, rep.DebitTotalCents, rep.CreditTotalCents)
	}
	if rep.Duplicate {
		t.Errorf("first import: duplicate = true, want false")
	}
}

// A trailer that does not foot the :61: lines is never parsed —
// whether the amount is wrong or the count is wrong.
func TestParseMT942TrailerMismatch(t *testing.T) {
	lines := []string{":61:2610051005C100,00NTRF//tm-e1"}
	for name, pair := range map[string][2]string{
		"amount": {"00,00", "1101,00"},
		"count":  {"00,00", "2100,00"},
	} {
		debit, credit := pair[0], pair[1]
		doc := mt942Doc("TM-1", "acct1", "", "2610101200", lines, debit, credit)
		if _, err := ParseMT942(doc); err == nil {
			t.Errorf("%s: ParseMT942 accepted a trailer that does not foot the lines", name)
		} else if !errors.Is(err, ErrInvalidMT942) {
			t.Errorf("%s: err = %v, want ErrInvalidMT942", name, err)
		}
	}
}

// Field order is canonical: :34F: after :13D:, and :90D: before
// the :61: lines, are both framing errors.
func TestParseMT942OrderError(t *testing.T) {
	late34F := strings.Join([]string{
		":20:ORD-1", ":25:acct1", ":28C:1/1", ":13D:2610101200",
		":34F:USDC1000,00", ":90D:00,00", ":90C:00,00",
	}, "\n")
	if _, err := ParseMT942(late34F); err == nil {
		t.Errorf("ParseMT942 accepted :34F: after :13D:")
	} else if !errors.Is(err, ErrInvalidMT942) {
		t.Errorf("late :34F:: err = %v, want ErrInvalidMT942", err)
	}

	earlyTrailer := strings.Join([]string{
		":20:ORD-2", ":25:acct1", ":28C:1/1", ":13D:2610101200",
		":90D:00,00", ":90C:00,00", ":61:2610051005C100,00NTRF//ord-e1",
	}, "\n")
	if _, err := ParseMT942(earlyTrailer); err == nil {
		t.Errorf("ParseMT942 accepted :90D: before the :61: lines")
	} else if !errors.Is(err, ErrInvalidMT942) {
		t.Errorf("early trailer: err = %v, want ErrInvalidMT942", err)
	}
}

// An MT942 report carries no balance tags and no trailing :86::
// a :60F: field and a :86: after :90C: are both refused.
func TestParseMT942UnknownField(t *testing.T) {
	balanceTag := strings.Join([]string{
		":20:UF-1", ":25:acct1", ":28C:1/1", ":60F:C261005USD100,00",
		":13D:2610101200", ":90D:00,00", ":90C:00,00",
	}, "\n")
	if _, err := ParseMT942(balanceTag); err == nil {
		t.Errorf("ParseMT942 accepted a :60F: balance tag")
	} else if !errors.Is(err, ErrInvalidMT942) {
		t.Errorf(":60F:: err = %v, want ErrInvalidMT942", err)
	}

	trailing86 := mt942Doc("UF-2", "acct1", "", "2610101200", nil, "00,00", "00,00") + "\n:86:TRAILING"
	if _, err := ParseMT942(trailing86); err == nil {
		t.Errorf("ParseMT942 accepted a trailing :86: after :90C:")
	} else if !errors.Is(err, ErrInvalidMT942) {
		t.Errorf("trailing :86:: err = %v, want ErrInvalidMT942", err)
	}
}

// CRLF line endings and one trailing newline are tolerated; an
// empty document is not.
func TestParseMT942CRLFAndEmpty(t *testing.T) {
	doc := mt942Doc("CRLF-1", "acct1", "", "2610101200",
		[]string{":61:2610051005C100,00NTRF//crlf-e1"}, "00,00", "1100,00")
	crlf := strings.ReplaceAll(doc, "\n", "\r\n") + "\n"
	if _, err := ParseMT942(crlf); err != nil {
		t.Errorf("ParseMT942(CRLF + trailing newline): %v", err)
	}
	for _, empty := range []string{"", "  \n  "} {
		if _, err := ParseMT942(empty); !errors.Is(err, ErrInvalidMT942) {
			t.Errorf("ParseMT942(%q): err = %v, want ErrInvalidMT942", empty, err)
		}
	}
}

// One matched line, one amount-mismatched line, one unmatched
// line: the counts, the mismatch reason, and both trailer-vs-fold
// differences land in the report.
func TestImportMT942PartialMatch(t *testing.T) {
	l := New()
	day := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	mt942Seed(t, l, "pm-e1", "acct1", "bank", 10000, day)
	mt942Seed(t, l, "pm-e2", "bank", "acct1", 5000, day.Add(time.Hour))
	doc := mt942Doc("PM-1", "acct1", "USDC1000,00", "2610101200", []string{
		":61:2610051005C100,00NTRF//pm-e1",
		":61:2610051005D60,00NTRF//pm-e2",
		":86:BANK FEE NOTE",
		":61:2610051005C25,00NTRF//pm-nope",
	}, "160,00", "2125,00")

	rep, err := l.ImportMT942("acct1", []byte(doc))
	if err != nil {
		t.Fatalf("ImportMT942: %v", err)
	}
	if rep.Matched != 1 || rep.Mismatched != 1 || rep.Unmatched != 1 {
		t.Errorf("matches = %d/%d/%d, want 1/1/1", rep.Matched, rep.Mismatched, rep.Unmatched)
	}
	if len(rep.Lines) != 3 {
		t.Fatalf("lines = %d, want 3", len(rep.Lines))
	}
	if rep.Lines[0].Status != CAMT053Matched || rep.Lines[0].EntryID != "pm-e1" {
		t.Errorf("line 0 = %+v, want matched pm-e1", rep.Lines[0])
	}
	if rep.Lines[1].Status != CAMT053Mismatched || rep.Lines[1].Reason == "" {
		t.Errorf("line 1 = %+v, want mismatched with a reason", rep.Lines[1])
	}
	if rep.Lines[1].Supplementary != "BANK FEE NOTE" {
		t.Errorf("line 1 supplementary = %q, want BANK FEE NOTE", rep.Lines[1].Supplementary)
	}
	if rep.Lines[2].Status != CAMT053Unmatched {
		t.Errorf("line 2 = %+v, want unmatched", rep.Lines[2])
	}
	// The journal folds 5000 debit / 10000 credit over the window;
	// the trailers claim 6000 / 12500 — both sides differ.
	if len(rep.SummaryMismatches) != 2 {
		t.Fatalf("summary mismatches = %+v, want 2", rep.SummaryMismatches)
	}
	d, c := rep.SummaryMismatches[0], rep.SummaryMismatches[1]
	if d.Side != "D" || d.ReportedCents != 6000 || d.LedgerCents != 5000 {
		t.Errorf("debit mismatch = %+v, want D 6000 vs 5000", d)
	}
	if c.Side != "C" || c.ReportedCents != 12500 || c.LedgerCents != 10000 {
		t.Errorf("credit mismatch = %+v, want C 12500 vs 10000", c)
	}
}

// A report naming a different account than the import target is
// refused; a report for an unknown account is refused with the
// unknown-account error.
func TestImportMT942WrongAndUnknownAccount(t *testing.T) {
	l := New()
	mt942Seed(t, l, "wa-e1", "acct1", "bank", 10000, time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC))
	doc := mt942Doc("WA-1", "acct1", "", "2610101200",
		[]string{":61:2610051005C100,00NTRF//wa-e1"}, "00,00", "1100,00")

	if _, err := l.ImportMT942("acct2", []byte(doc)); !errors.Is(err, ErrInvalidMT942) {
		t.Errorf("wrong account: err = %v, want ErrInvalidMT942", err)
	}

	ghost := mt942Doc("WA-2", "ghost-acct", "", "2610101200", nil, "00,00", "00,00")
	if _, err := l.ImportMT942("ghost-acct", []byte(ghost)); !errors.Is(err, ErrMT942UnknownAccount) {
		t.Errorf("unknown account: err = %v, want ErrMT942UnknownAccount", err)
	}
}

// Re-importing the same document returns the original report with
// Duplicate set — a re-uploaded report never double-counts.
func TestImportMT942Duplicate(t *testing.T) {
	l := New()
	mt942Seed(t, l, "dup-e1", "acct1", "bank", 10000, time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC))
	doc := mt942Doc("DUP-1", "acct1", "", "2610101200",
		[]string{":61:2610051005C100,00NTRF//dup-e1"}, "00,00", "1100,00")

	first, err := l.ImportMT942("acct1", []byte(doc))
	if err != nil {
		t.Fatalf("first import: %v", err)
	}
	if first.Duplicate {
		t.Errorf("first import: duplicate = true, want false")
	}
	second, err := l.ImportMT942("acct1", []byte(doc))
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if !second.Duplicate {
		t.Errorf("second import: duplicate = false, want true")
	}
	if second.DocumentSHA256 != first.DocumentSHA256 || second.Matched != first.Matched {
		t.Errorf("duplicate report differs from the original: %+v vs %+v", second, first)
	}
}

// The :34F: floor limit and the per-line :86: text ride along in
// the report verbatim; a line without :86: carries none.
func TestImportMT942FloorLimitAndSupplementary(t *testing.T) {
	l := New()
	day := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	mt942Seed(t, l, "fl-e1", "acct1", "bank", 10000, day)
	mt942Seed(t, l, "fl-e2", "bank", "acct1", 5000, day.Add(time.Hour))
	doc := mt942Doc("FL-1", "acct1", "USDD250,00", "2610101200", []string{
		":61:2610051005C100,00NTRF//fl-e1",
		":86:INVOICE 42 PAID",
		":61:2610051005D50,00NTRF//fl-e2",
	}, "150,00", "1100,00")

	rep, err := l.ImportMT942("acct1", []byte(doc))
	if err != nil {
		t.Fatalf("ImportMT942: %v", err)
	}
	if rep.FloorLimit == nil || *rep.FloorLimit != (MT942FloorLimit{Credit: false, Currency: "USD", AmountCents: 25000}) {
		t.Errorf("floor limit = %+v, want {false USD 25000}", rep.FloorLimit)
	}
	if len(rep.Lines) != 2 {
		t.Fatalf("lines = %d, want 2", len(rep.Lines))
	}
	if rep.Lines[0].Supplementary != "INVOICE 42 PAID" {
		t.Errorf("line 0 supplementary = %q, want INVOICE 42 PAID", rep.Lines[0].Supplementary)
	}
	if rep.Lines[1].Supplementary != "" {
		t.Errorf("line 1 supplementary = %q, want empty", rep.Lines[1].Supplementary)
	}
	if rep.Matched != 2 {
		t.Errorf("matched = %d, want 2", rep.Matched)
	}
	if len(rep.SummaryMismatches) != 0 {
		t.Errorf("summary mismatches = %+v, want none", rep.SummaryMismatches)
	}
}

// Single currency: a report naming EUR through :34F: never
// matches a USD journal row, and the trailer fold in EUR finds
// nothing to fold.
func TestImportMT942SingleCurrency(t *testing.T) {
	l := New()
	mt942Seed(t, l, "sc-e1", "acct1", "bank", 10000, time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC))
	doc := mt942Doc("SC-1", "acct1", "EURD500,00", "2610101200",
		[]string{":61:2610051005C100,00NTRF//sc-e1"}, "00,00", "1100,00")

	rep, err := l.ImportMT942("acct1", []byte(doc))
	if err != nil {
		t.Fatalf("ImportMT942: %v", err)
	}
	if rep.Currency != "EUR" {
		t.Errorf("currency = %q, want EUR", rep.Currency)
	}
	if rep.Unmatched != 1 || rep.Matched != 0 {
		t.Errorf("matches = %d/%d/%d, want 0/0/1", rep.Matched, rep.Mismatched, rep.Unmatched)
	}
	if len(rep.SummaryMismatches) != 1 {
		t.Fatalf("summary mismatches = %+v, want 1", rep.SummaryMismatches)
	}
	m := rep.SummaryMismatches[0]
	if m.Side != "C" || m.ReportedCents != 10000 || m.LedgerCents != 0 {
		t.Errorf("summary mismatch = %+v, want C 10000 vs 0", m)
	}
}

// An empty interim report — zero :61: lines, zero trailers —
// parses and imports cleanly.
func TestImportMT942EmptyReport(t *testing.T) {
	l := New()
	mt942Seed(t, l, "em-e1", "acct1", "bank", 10000, time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC))
	doc := mt942Doc("EM-1", "acct1", "", "2610101200", nil, "00,00", "00,00")

	rep, err := l.ImportMT942("acct1", []byte(doc))
	if err != nil {
		t.Fatalf("ImportMT942: %v", err)
	}
	if rep.Matched != 0 || rep.Mismatched != 0 || rep.Unmatched != 0 {
		t.Errorf("matches = %d/%d/%d, want 0/0/0", rep.Matched, rep.Mismatched, rep.Unmatched)
	}
	if len(rep.Lines) != 0 {
		t.Errorf("lines = %d, want 0", len(rep.Lines))
	}
	if rep.Currency != "" {
		t.Errorf("currency = %q, want empty (no :34F:)", rep.Currency)
	}
	if len(rep.SummaryMismatches) != 0 {
		t.Errorf("summary mismatches = %+v, want none (no lines to bound the window)", rep.SummaryMismatches)
	}
}
