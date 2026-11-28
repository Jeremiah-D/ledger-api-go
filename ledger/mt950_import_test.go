package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// mt950Doc renders a minimal MT950 interbank statement for the
// tests: :20: :25: :28C: :60F: (:61: [:86:])* :62F: :64: :65:
// [:86:]. lineBlocks are the full ":61:..." / ":86:..." lines in
// order; empty owner omits the trailing :86:.
func mt950Doc(ref, acct, opening string, lineBlocks []string, closing, avail, fwd, owner string) string {
	lines := []string{":20:" + ref, ":25:" + acct, ":28C:1/1", ":60F:" + opening}
	lines = append(lines, lineBlocks...)
	lines = append(lines, ":62F:"+closing, ":64:"+avail, ":65:"+fwd)
	if owner != "" {
		lines = append(lines, ":86:"+owner)
	}
	return strings.Join(lines, "\n")
}

// mt950Seed posts one journal row for the tests.
func mt950Seed(t *testing.T, l *Ledger, id, debit, credit string, cents int64, at time.Time) {
	t.Helper()
	if _, _, err := l.Post(JournalEntry{
		ID: id, DebitAccount: AccountID(debit), CreditAccount: AccountID(credit),
		AmountCents: cents, Currency: "USD", CreatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
}

// Round-trip: a statement our own ExportMT950 renders parses back
// to the same statement, and importing it reports zero mismatches —
// including through the mt950-system external mapping, which names
// the account by its IBAN in :25:.
func TestParseMT950RoundTrip(t *testing.T) {
	l := New()
	iban := "DE89370400440532013000"
	if err := l.SetExternalMapping("acct1", ExternalSystemMT950, iban, "test"); err != nil {
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

	doc, err := l.ExportMT950(MT950ExportOptions{
		Account: "acct1", StatementID: "1", SequenceID: "1",
		Since:    time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		Until:    time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC),
		Currency: "USD",
	})
	if err != nil {
		t.Fatal(err)
	}
	// CRLF is tolerated: the bank's downloader may hand us CRLF.
	stmt, err := ParseMT950(strings.ReplaceAll(doc, "\n", "\r\n"))
	if err != nil {
		t.Fatalf("ParseMT950(round-trip): %v\ndoc:\n%s", err, doc)
	}
	if stmt.TransactionReference != "1" || stmt.AccountIdentification != iban {
		t.Errorf("header = %q/%q, want 1/%s", stmt.TransactionReference, stmt.AccountIdentification, iban)
	}
	if stmt.StatementNumber != "1" || stmt.SequenceNumber != "1" {
		t.Errorf(":28C: = %q/%q, want 1/1", stmt.StatementNumber, stmt.SequenceNumber)
	}
	if mt950Signed(stmt.OpeningBalance) != 40000 || stmt.OpeningBalance.Currency != "USD" {
		t.Errorf(":60F: = %+v, want +40000 USD", stmt.OpeningBalance)
	}
	if mt950Signed(stmt.ClosingBalance) != 170000 {
		t.Errorf(":62F: = %+v, want +170000", stmt.ClosingBalance)
	}
	if mt950Signed(stmt.AvailableBalance) != 170000 || mt950Signed(stmt.ForwardBalance) != 170000 {
		t.Errorf(":64:/:65: = %+v/%+v, want +170000/+170000", stmt.AvailableBalance, stmt.ForwardBalance)
	}
	if len(stmt.Lines) != 3 {
		t.Fatalf("lines = %d, want 3", len(stmt.Lines))
	}
	if stmt.Lines[0].Reference != "bal-e1" || stmt.Lines[1].Reference != "bal-e2" || stmt.Lines[2].Reference != "bal-e3" {
		t.Errorf("line refs = %q/%q/%q, want bal-e1/bal-e2/bal-e3", stmt.Lines[0].Reference, stmt.Lines[1].Reference, stmt.Lines[2].Reference)
	}
	if stmt.OwnerInfo != "" {
		t.Errorf("owner info = %q, want empty (the ledger holds none)", stmt.OwnerInfo)
	}

	rep, err := l.ImportMT950("acct1", []byte(doc))
	if err != nil {
		t.Fatalf("ImportMT950(round-trip): %v", err)
	}
	if rep.Matched != 3 || rep.Mismatched != 0 || rep.Unmatched != 0 {
		t.Errorf("matches = %d/%d/%d, want 3/0/0", rep.Matched, rep.Mismatched, rep.Unmatched)
	}
	if len(rep.BalanceMismatches) != 0 {
		t.Errorf("balance mismatches = %+v, want none", rep.BalanceMismatches)
	}
	if rep.Currency != "USD" || rep.OpeningBalanceCents != 40000 || rep.ClosingBalanceCents != 170000 {
		t.Errorf("report = %s %d/%d, want USD 40000/170000", rep.Currency, rep.OpeningBalanceCents, rep.ClosingBalanceCents)
	}
	if rep.AvailableBalanceCents != 170000 || rep.ForwardBalanceCents != 170000 {
		t.Errorf("available/forward = %d/%d, want 170000/170000", rep.AvailableBalanceCents, rep.ForwardBalanceCents)
	}
	if rep.Duplicate {
		t.Errorf("first import: duplicate = true, want false")
	}
}

// A closing balance that does not reconcile with the opening
// balance and the lines is never parsed — whether the amount is
// wrong or the sign is wrong.
func TestParseMT950ClosingMismatch(t *testing.T) {
	lines := []string{":61:2610051005C100,00NTRF//cm-e1"}
	for name, closing := range map[string]string{
		"amount": "C261006USD150,00",
		"sign":   "D261006USD200,00",
	} {
		doc := mt950Doc("CM-1", "acct1", "C261005USD100,00", lines, closing, closing, closing, "")
		if _, err := ParseMT950(doc); err == nil {
			t.Errorf("%s: ParseMT950 accepted a closing balance that does not foot", name)
		} else if !errors.Is(err, ErrInvalidMT950) {
			t.Errorf("%s: err = %v, want ErrInvalidMT950", name, err)
		}
	}
}

// :64: and :65: must restate :62F: with the same signed amount
// and the same currency.
func TestParseMT950RestateMismatch(t *testing.T) {
	lines := []string{":61:2610051005C100,00NTRF//rs-e1"}
	closing := "C261006USD200,00"
	for name, pair := range map[string][2]string{
		"available amount": {"C261006USD199,00", closing},
		"available sign":   {"D261006USD200,00", closing},
		"forward currency": {closing, "C261006EUR200,00"},
	} {
		doc := mt950Doc("RS-1", "acct1", "C261005USD100,00", lines, closing, pair[0], pair[1], "")
		if _, err := ParseMT950(doc); err == nil {
			t.Errorf("%s: ParseMT950 accepted a :64:/:65: that does not restate :62F:", name)
		} else if !errors.Is(err, ErrInvalidMT950) {
			t.Errorf("%s: err = %v, want ErrInvalidMT950", name, err)
		}
	}
}

// Single currency: an opening balance in EUR against a closing
// balance in USD is refused.
func TestParseMT950MixedCurrency(t *testing.T) {
	doc := mt950Doc("MC-1", "acct1", "C261005EUR100,00",
		[]string{":61:2610051005C100,00NTRF//mc-e1"},
		"C261006USD200,00", "C261006USD200,00", "C261006USD200,00", "")
	if _, err := ParseMT950(doc); err == nil {
		t.Errorf("ParseMT950 accepted a mixed-currency statement")
	} else if !errors.Is(err, ErrInvalidMT950) {
		t.Errorf("mixed currency: err = %v, want ErrInvalidMT950", err)
	}
}

// Field order is canonical: :60F: after a :61: line, and :62F:
// before the :61: lines, are both framing errors.
func TestParseMT950OrderError(t *testing.T) {
	late60F := strings.Join([]string{
		":20:ORD-1", ":25:acct1", ":28C:1/1",
		":61:2610051005C100,00NTRF//ord-e1",
		":60F:C261005USD100,00",
		":62F:C261006USD200,00", ":64:C261006USD200,00", ":65:C261006USD200,00",
	}, "\n")
	if _, err := ParseMT950(late60F); err == nil {
		t.Errorf("ParseMT950 accepted :60F: after a :61: line")
	} else if !errors.Is(err, ErrInvalidMT950) {
		t.Errorf("late :60F:: err = %v, want ErrInvalidMT950", err)
	}

	earlyClosing := strings.Join([]string{
		":20:ORD-2", ":25:acct1", ":28C:1/1", ":60F:C261005USD100,00",
		":62F:C261006USD200,00", ":64:C261006USD200,00", ":65:C261006USD200,00",
		":61:2610051005C100,00NTRF//ord-e2",
	}, "\n")
	if _, err := ParseMT950(earlyClosing); err == nil {
		t.Errorf("ParseMT950 accepted :62F: before the :61: lines")
	} else if !errors.Is(err, ErrInvalidMT950) {
		t.Errorf("early :62F:: err = %v, want ErrInvalidMT950", err)
	}
}

// An MT950 statement carries no :34F: and no :90D:/:90C: — those
// tags are refused, as is any content after the trailing :86:.
func TestParseMT950UnknownField(t *testing.T) {
	floorTag := strings.Join([]string{
		":20:UF-1", ":25:acct1", ":28C:1/1", ":60F:C261005USD100,00",
		":34F:USDC1000,00",
		":61:2610051005C100,00NTRF//uf-e1",
		":62F:C261006USD200,00", ":64:C261006USD200,00", ":65:C261006USD200,00",
	}, "\n")
	if _, err := ParseMT950(floorTag); err == nil {
		t.Errorf("ParseMT950 accepted a :34F: floor limit")
	} else if !errors.Is(err, ErrInvalidMT950) {
		t.Errorf(":34F:: err = %v, want ErrInvalidMT950", err)
	}

	trailing := mt950Doc("UF-2", "acct1", "C261005USD0,00", nil,
		"C261006USD0,00", "C261006USD0,00", "C261006USD0,00", "OWNER INFO") + "\n:61:2610051005C100,00NTRF//uf-e2"
	if _, err := ParseMT950(trailing); err == nil {
		t.Errorf("ParseMT950 accepted content after the trailing :86:")
	} else if !errors.Is(err, ErrInvalidMT950) {
		t.Errorf("trailing content: err = %v, want ErrInvalidMT950", err)
	}
}

// CRLF line endings and one trailing newline are tolerated; an
// empty document is not.
func TestParseMT950CRLFAndEmpty(t *testing.T) {
	doc := mt950Doc("CRLF-1", "acct1", "C261005USD0,00",
		[]string{":61:2610051005C100,00NTRF//crlf-e1"},
		"C261006USD100,00", "C261006USD100,00", "C261006USD100,00", "")
	crlf := strings.ReplaceAll(doc, "\n", "\r\n") + "\n"
	if _, err := ParseMT950(crlf); err != nil {
		t.Errorf("ParseMT950(CRLF + trailing newline): %v", err)
	}
	for _, empty := range []string{"", "  \n  "} {
		if _, err := ParseMT950(empty); !errors.Is(err, ErrInvalidMT950) {
			t.Errorf("ParseMT950(%q): err = %v, want ErrInvalidMT950", empty, err)
		}
	}
}

// One matched line, one amount-mismatched line, one unmatched
// line: the counts, the mismatch reason, and the closing-balance
// difference land in the report, while the opening balance agrees.
func TestImportMT950PartialMatch(t *testing.T) {
	l := New()
	day := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	mt950Seed(t, l, "pm-e1", "acct1", "bank", 10000, day)
	mt950Seed(t, l, "pm-e2", "bank", "acct1", 5000, day.Add(time.Hour))
	doc := mt950Doc("PM-1", "acct1", "C261005USD0,00", []string{
		":61:2610051005C100,00NTRF//pm-e1",
		":61:2610051005D60,00NTRF//pm-e2",
		":86:BANK FEE NOTE",
		":61:2610051005C25,00NTRF//pm-nope",
	}, "C261006USD65,00", "C261006USD65,00", "C261006USD65,00", "")

	rep, err := l.ImportMT950("acct1", []byte(doc))
	if err != nil {
		t.Fatalf("ImportMT950: %v", err)
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
	// The opening balance agrees (no rows before 2026-10-05);
	// the closing balance claims 6500 while the journal folds
	// 10000 - 5000 = 5000 at the end of 2026-10-06.
	if len(rep.BalanceMismatches) != 1 {
		t.Fatalf("balance mismatches = %+v, want 1", rep.BalanceMismatches)
	}
	m := rep.BalanceMismatches[0]
	if m.ReportedCents != 6500 || m.LedgerCents != 5000 {
		t.Errorf("closing mismatch = %+v, want 6500 vs 5000", m)
	}
	if want := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC); !m.At.Equal(want) {
		t.Errorf("closing mismatch at = %v, want %v", m.At, want)
	}
}

// A journal row dated before the opening day, which the statement
// does not know about, disagrees with both balances: the opening
// fold and the closing fold each land in the report.
func TestImportMT950OpeningMismatch(t *testing.T) {
	l := New()
	mt950Seed(t, l, "pre-e0", "acct1", "bank", 7000, time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC))
	mt950Seed(t, l, "om-e1", "acct1", "bank", 10000, time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC))
	doc := mt950Doc("OM-1", "acct1", "C261005USD0,00",
		[]string{":61:2610051005C100,00NTRF//om-e1"},
		"C261006USD100,00", "C261006USD100,00", "C261006USD100,00", "")

	rep, err := l.ImportMT950("acct1", []byte(doc))
	if err != nil {
		t.Fatalf("ImportMT950: %v", err)
	}
	if rep.Matched != 1 {
		t.Errorf("matched = %d, want 1", rep.Matched)
	}
	if len(rep.BalanceMismatches) != 2 {
		t.Fatalf("balance mismatches = %+v, want 2", rep.BalanceMismatches)
	}
	opening, closing := rep.BalanceMismatches[0], rep.BalanceMismatches[1]
	if opening.ReportedCents != 0 || opening.LedgerCents != 7000 {
		t.Errorf("opening mismatch = %+v, want 0 vs 7000", opening)
	}
	if want := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC); !opening.At.Equal(want) {
		t.Errorf("opening mismatch at = %v, want %v", opening.At, want)
	}
	if closing.ReportedCents != 10000 || closing.LedgerCents != 17000 {
		t.Errorf("closing mismatch = %+v, want 10000 vs 17000", closing)
	}
	if want := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC); !closing.At.Equal(want) {
		t.Errorf("closing mismatch at = %v, want %v", closing.At, want)
	}
}

// A statement naming a different account than the import target
// is refused; a statement for an unknown account is refused with
// the unknown-account error.
func TestImportMT950WrongAndUnknownAccount(t *testing.T) {
	l := New()
	mt950Seed(t, l, "wa-e1", "acct1", "bank", 10000, time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC))
	doc := mt950Doc("WA-1", "acct1", "C261005USD0,00",
		[]string{":61:2610051005C100,00NTRF//wa-e1"},
		"C261006USD100,00", "C261006USD100,00", "C261006USD100,00", "")

	if _, err := l.ImportMT950("acct2", []byte(doc)); !errors.Is(err, ErrInvalidMT950) {
		t.Errorf("wrong account: err = %v, want ErrInvalidMT950", err)
	}

	ghost := mt950Doc("WA-2", "ghost-acct", "C261005USD0,00", nil,
		"C261006USD0,00", "C261006USD0,00", "C261006USD0,00", "")
	if _, err := l.ImportMT950("ghost-acct", []byte(ghost)); !errors.Is(err, ErrMT950UnknownAccount) {
		t.Errorf("unknown account: err = %v, want ErrMT950UnknownAccount", err)
	}
}

// Re-importing the same document returns the original report with
// Duplicate set — a re-uploaded statement never double-counts.
func TestImportMT950Duplicate(t *testing.T) {
	l := New()
	mt950Seed(t, l, "dup-e1", "acct1", "bank", 10000, time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC))
	doc := mt950Doc("DUP-1", "acct1", "C261005USD0,00",
		[]string{":61:2610051005C100,00NTRF//dup-e1"},
		"C261006USD100,00", "C261006USD100,00", "C261006USD100,00", "")

	first, err := l.ImportMT950("acct1", []byte(doc))
	if err != nil {
		t.Fatalf("first import: %v", err)
	}
	if first.Duplicate {
		t.Errorf("first import: duplicate = true, want false")
	}
	second, err := l.ImportMT950("acct1", []byte(doc))
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

// mt950ExportLedger is a bare ledger wired to an audit log, with
// acct1 reachable under the mt950-system external mapping (the
// :25: account the correspondent bank names), so an MT950
// statement can be imported with no journal rows at all.
func mt950ExportLedger(t *testing.T) *Ledger {
	t.Helper()
	l := mt202ExportTestLedger(t, mustAuditLog(t))
	if err := l.SetExternalMapping("acct1", ExternalSystemMT950, "DE89370400440532013000", "test"); err != nil {
		t.Fatal(err)
	}
	return l
}

// mt950ExportDoc renders a one-line MT950 statement citing
// MT202-REF-1: opening 0, one :61: line, and a :62F:/:64:/:65:
// that foot it. mark is the C/D mark, amount the 15d comma
// amount, closing the signed closing-balance tag body.
func mt950ExportDoc(stmtRef, mark, amount, closing string) string {
	return mt950Doc(stmtRef, "DE89370400440532013000", "C261005USD0,00",
		[]string{":61:2610051005" + mark + amount + "NTRF//MT202-REF-1"},
		closing, closing, closing, "")
}

// A :61: D line citing an outbound MT202 this ledger rendered
// (second form, LG-488) matches against the export registry: no
// journal row exists, so EntryID stays empty and the :20: is
// carried as ExportReference, with the export's amount and the
// credit leg in the report.
func TestImportMT950ExportMatch(t *testing.T) {
	l := mt950ExportLedger(t)
	if _, err := l.ExportMT202Registered(mt202Sample(), "ledger-operator"); err != nil {
		t.Fatalf("ExportMT202Registered: %v", err)
	}
	rep, err := l.ImportMT950("acct1", []byte(mt950ExportDoc("STMT-EX", "D", "2500,00", "D261005USD2500,00")))
	if err != nil {
		t.Fatalf("ImportMT950: %v", err)
	}
	if rep.Matched != 1 || rep.Mismatched != 0 || rep.Unmatched != 0 {
		t.Fatalf("match counts = %d/%d/%d, want 1/0/0", rep.Matched, rep.Mismatched, rep.Unmatched)
	}
	m := rep.Lines[0]
	if m.Status != CAMT053Matched {
		t.Fatalf("line = %+v, want matched", m)
	}
	if m.EntryID != "" {
		t.Errorf("entry_id = %q, want empty (the export posts no journal row)", m.EntryID)
	}
	if m.ExportReference != "MT202-REF-1" {
		t.Errorf("export_reference = %q, want MT202-REF-1", m.ExportReference)
	}
	if m.EntryAmountCents != 250000 {
		t.Errorf("entry amount = %d, want 250000 (the export's amount)", m.EntryAmountCents)
	}
	if m.EntryCredit {
		t.Errorf("entry credit = true, want false (an outbound MT202 is the credit leg)")
	}
	if m.Reason != "" {
		t.Errorf("reason = %q, want empty on a match", m.Reason)
	}
}

// A :61: D line citing an outbound MT202 whose amount disagrees
// is mismatched, with both sides in the report and the :20:
// named.
func TestImportMT950ExportAmountMismatch(t *testing.T) {
	l := mt950ExportLedger(t)
	if _, err := l.ExportMT202Registered(mt202Sample(), "ledger-operator"); err != nil {
		t.Fatalf("ExportMT202Registered: %v", err)
	}
	rep, err := l.ImportMT950("acct1", []byte(mt950ExportDoc("STMT-EXAM", "D", "2400,00", "D261005USD2400,00")))
	if err != nil {
		t.Fatalf("ImportMT950: %v", err)
	}
	if rep.Mismatched != 1 || rep.Lines[0].Status != CAMT053Mismatched {
		t.Fatalf("line = %+v, want mismatched", rep.Lines[0])
	}
	m := rep.Lines[0]
	if m.ExportReference != "MT202-REF-1" || m.EntryAmountCents != 250000 {
		t.Errorf("line = %+v, want export MT202-REF-1 at 250000", m)
	}
	if m.Reason != "amount differs: statement 240000, export 250000" {
		t.Errorf("reason = %q, want the both-amounts reason", m.Reason)
	}
}

// A :61: C line citing an outbound MT202 is a direction mismatch:
// the export is the credit leg (money out), a C line claims money
// in.
func TestImportMT950ExportDirectionMismatch(t *testing.T) {
	l := mt950ExportLedger(t)
	if _, err := l.ExportMT202Registered(mt202Sample(), "ledger-operator"); err != nil {
		t.Fatalf("ExportMT202Registered: %v", err)
	}
	rep, err := l.ImportMT950("acct1", []byte(mt950ExportDoc("STMT-EXDM", "C", "2500,00", "C261005USD2500,00")))
	if err != nil {
		t.Fatalf("ImportMT950: %v", err)
	}
	if rep.Mismatched != 1 || rep.Lines[0].Status != CAMT053Mismatched {
		t.Fatalf("line = %+v, want mismatched", rep.Lines[0])
	}
	m := rep.Lines[0]
	if m.ExportReference != "MT202-REF-1" || m.EntryCredit {
		t.Errorf("line = %+v, want export MT202-REF-1 with entry_credit false", m)
	}
	if m.Reason != "direction differs: statement and export disagree on the C/D mark" {
		t.Errorf("reason = %q, want the direction reason", m.Reason)
	}
}

// Journal first (LG-488): a reference that is both a journal key
// (the seeded entry's ID) and an outbound MT202 :20: matches the
// journal row — ExportReference stays empty.
func TestImportMT950JournalBeatsExport(t *testing.T) {
	l := mt950ExportLedger(t)
	if _, _, err := l.Post(JournalEntry{
		ID: "MT202-REF-1", DebitAccount: "acct1", CreditAccount: "bank",
		AmountCents: 250000, Currency: "USD",
		CreatedAt: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ExportMT202Registered(mt202Sample(), "ledger-operator"); err != nil {
		t.Fatalf("ExportMT202Registered: %v", err)
	}
	rep, err := l.ImportMT950("acct1", []byte(mt950ExportDoc("STMT-JB", "D", "2500,00", "D261005USD2500,00")))
	if err != nil {
		t.Fatalf("ImportMT950: %v", err)
	}
	m := rep.Lines[0]
	// The journal row is the debit leg (money in) while the line
	// is a debit, so the journal path mismatches on direction —
	// and the export tier is never reached.
	if m.Status != CAMT053Mismatched || m.EntryID != "MT202-REF-1" {
		t.Fatalf("line = %+v, want mismatched on MT202-REF-1", m)
	}
	if m.ExportReference != "" {
		t.Errorf("export_reference = %q, want empty (journal wins)", m.ExportReference)
	}
	if m.Reason != "direction differs: statement and journal disagree on the C/D mark" {
		t.Errorf("reason = %q, want the journal direction reason", m.Reason)
	}
}

// A :61: line citing a :20: this ledger never rendered is still
// unmatched, with the journal reason.
func TestImportMT950ExportUnregistered(t *testing.T) {
	l := mt950ExportLedger(t)
	rep, err := l.ImportMT950("acct1", []byte(mt950ExportDoc("STMT-NOEX", "D", "2500,00", "D261005USD2500,00")))
	if err != nil {
		t.Fatalf("ImportMT950: %v", err)
	}
	m := rep.Lines[0]
	if m.Status != CAMT053Unmatched {
		t.Fatalf("line = %+v, want unmatched", m)
	}
	if m.ExportReference != "" {
		t.Errorf("export_reference = %q, want empty", m.ExportReference)
	}
	if m.Reason != "no journal entry carries this reference" {
		t.Errorf("reason = %q, want the journal reason", m.Reason)
	}
}

// Restart recovery (LG-488): the export registry is in-memory, so
// a statement arriving before the outbound MT202 is re-exported
// is unmatched; re-exporting the same :20: with the same terms
// restores it (a warm re-export is a Duplicate), and a fresh
// statement document then matches.
func TestImportMT950ExportRestartRecovery(t *testing.T) {
	l := mt950ExportLedger(t)

	first, err := l.ImportMT950("acct1", []byte(mt950ExportDoc("STMT-R1", "D", "2500,00", "D261005USD2500,00")))
	if err != nil {
		t.Fatalf("first import: %v", err)
	}
	if first.Lines[0].Status != CAMT053Unmatched {
		t.Fatalf("first line = %+v, want unmatched (empty registry)", first.Lines[0])
	}

	if _, err := l.ExportMT202Registered(mt202Sample(), "ledger-operator"); err != nil {
		t.Fatalf("re-export: %v", err)
	}
	again, err := l.ExportMT202Registered(mt202Sample(), "ledger-operator")
	if err != nil {
		t.Fatalf("re-export again: %v", err)
	}
	if !again.Duplicate {
		t.Errorf("re-export: duplicate = false, want true")
	}

	second, err := l.ImportMT950("acct1", []byte(mt950ExportDoc("STMT-R2", "D", "2500,00", "D261005USD2500,00")))
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if second.Lines[0].Status != CAMT053Matched || second.Lines[0].ExportReference != "MT202-REF-1" {
		t.Fatalf("second line = %+v, want matched via export MT202-REF-1", second.Lines[0])
	}
}

// The trailing :86: owner info rides along in the report
// verbatim, alongside the per-line :86: text.
func TestImportMT950OwnerInfo(t *testing.T) {
	l := New()
	day := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	mt950Seed(t, l, "oi-e1", "acct1", "bank", 10000, day)
	doc := mt950Doc("OI-1", "acct1", "C261005USD0,00",
		[]string{
			":61:2610051005C100,00NTRF//oi-e1",
			":86:INVOICE 42 PAID",
		},
		"C261006USD100,00", "C261006USD100,00", "C261006USD100,00",
		"STATEMENT FOR OCTOBER")

	rep, err := l.ImportMT950("acct1", []byte(doc))
	if err != nil {
		t.Fatalf("ImportMT950: %v", err)
	}
	if rep.OwnerInfo != "STATEMENT FOR OCTOBER" {
		t.Errorf("owner info = %q, want STATEMENT FOR OCTOBER", rep.OwnerInfo)
	}
	if len(rep.Lines) != 1 || rep.Lines[0].Supplementary != "INVOICE 42 PAID" {
		t.Errorf("line supplementary = %+v, want INVOICE 42 PAID", rep.Lines)
	}
	if rep.Matched != 1 || len(rep.BalanceMismatches) != 0 {
		t.Errorf("matches = %d, balance mismatches = %+v, want 1 and none", rep.Matched, rep.BalanceMismatches)
	}
}
