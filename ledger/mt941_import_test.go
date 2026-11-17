package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// mt941Doc renders a minimal MT941 balance report for the tests:
// :20: :25: :28C: :13D: [:60F:] :90D: :90C: :62F: :64: :65:
// [:86:]. Empty opening omits :60F:; empty supp omits :86:.
func mt941Doc(ref, acct, stmt, seq, dt13, opening, debit, credit, closing, avail, fwd, supp string) string {
	lines := []string{
		":20:" + ref,
		":25:" + acct,
		":28C:" + stmt + "/" + seq,
		":13D:" + dt13,
	}
	if opening != "" {
		lines = append(lines, ":60F:"+opening)
	}
	lines = append(lines, ":90D:"+debit, ":90C:"+credit, ":62F:"+closing, ":64:"+avail, ":65:"+fwd)
	if supp != "" {
		lines = append(lines, ":86:"+supp)
	}
	return strings.Join(lines, "\n")
}

// mt941Seed posts one inflow to acct1 (150,00 on 2026-10-05) so the
// ledger balance at any later :13D: is 150,00.
func mt941Seed(t *testing.T, l *Ledger) {
	t.Helper()
	if _, _, err := l.Post(JournalEntry{
		ID: "mt941-e1", DebitAccount: "acct1", CreditAccount: "bank",
		AmountCents: 15000, Currency: "USD",
		CreatedAt: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
}

// Round-trip: a report our own ExportMT941 renders parses back to
// the same statement, and importing it reports zero mismatches —
// including through the mt941-system external mapping, which names
// the account by its IBAN in :25:.
func TestParseMT941RoundTrip(t *testing.T) {
	l := New()
	iban := "DE89370400440532013000"
	if err := l.SetExternalMapping("acct1", ExternalSystemMT941, iban, "test"); err != nil {
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

	doc, err := l.ExportMT941(MT941ExportOptions{
		Account: "acct1", StatementID: "1", SequenceID: "1",
		Since:    time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		Until:    time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC),
		Currency: "USD",
	})
	if err != nil {
		t.Fatal(err)
	}
	// CRLF is tolerated: the bank's downloader may hand us CRLF.
	stmt, err := ParseMT941(strings.ReplaceAll(doc, "\n", "\r\n"))
	if err != nil {
		t.Fatalf("ParseMT941(round-trip): %v\ndoc:\n%s", err, doc)
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
	if stmt.OpeningBalance == nil || mt941Signed(*stmt.OpeningBalance) != 40000 {
		t.Errorf("opening = %+v, want +40000", stmt.OpeningBalance)
	}
	if stmt.DebitSummary != (MT942Summary{Count: 1, AmountCents: 20000}) {
		t.Errorf(":90D: = %+v, want {1 20000}", stmt.DebitSummary)
	}
	if stmt.CreditSummary != (MT942Summary{Count: 2, AmountCents: 150000}) {
		t.Errorf(":90C: = %+v, want {2 150000}", stmt.CreditSummary)
	}
	if mt941Signed(stmt.ClosingBalance) != 170000 || stmt.ClosingBalance.Currency != "USD" {
		t.Errorf(":62F: = %+v, want +170000 USD", stmt.ClosingBalance)
	}
	if mt941Signed(stmt.AvailableBalance) != 170000 || mt941Signed(stmt.ForwardBalance) != 170000 {
		t.Errorf(":64:/:65: = %+v / %+v, want +170000 both", stmt.AvailableBalance, stmt.ForwardBalance)
	}

	rep, err := l.ImportMT941("acct1", []byte(doc))
	if err != nil {
		t.Fatalf("ImportMT941(round-trip): %v", err)
	}
	if len(rep.BalanceMismatches) != 0 {
		t.Errorf("mismatches = %+v, want none", rep.BalanceMismatches)
	}
	if rep.ClosingBalanceCents != 170000 || rep.Currency != "USD" {
		t.Errorf("report closing = %d %s, want 170000 USD", rep.ClosingBalanceCents, rep.Currency)
	}
	if rep.Duplicate {
		t.Errorf("first import: duplicate = true, want false")
	}
}

// A :62F: that disagrees with the ledger is recorded, not
// fail-closed: the import succeeds and the mismatch carries both
// sides of the gap.
func TestImportMT941BalanceMismatch(t *testing.T) {
	l := New()
	mt941Seed(t, l)
	// The report foots (0 + 200,00 − 0 = 200,00) but the ledger
	// holds 150,00 at the :13D: instant.
	doc := mt941Doc("MISM-1", "acct1", "1", "1", "2610101200", "",
		"00,00", "2200,00", "C261010USD200,00", "C261010USD200,00", "C261010USD200,00", "")
	rep, err := l.ImportMT941("acct1", []byte(doc))
	if err != nil {
		t.Fatalf("ImportMT941: %v", err)
	}
	if len(rep.BalanceMismatches) != 1 {
		t.Fatalf("mismatches = %+v, want 1", rep.BalanceMismatches)
	}
	m := rep.BalanceMismatches[0]
	if m.Currency != "USD" || m.ReportedCents != 20000 || m.LedgerCents != 15000 {
		t.Errorf("mismatch = %+v, want USD/20000/15000", m)
	}
	if want := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC); !m.At.Equal(want) {
		t.Errorf("mismatch At = %v, want %v", m.At, want)
	}
}

// An unbalanced report — :62F: != :60F: + :90C: − :90D: — is a 400.
func TestParseMT941Unbalanced(t *testing.T) {
	// Opening 400,00 + credits 1500,00 − debits 200,00 = 1700,00,
	// but :62F: claims 1600,00.
	doc := mt941Doc("UNBAL-1", "acct1", "1", "1", "2610100000", "C261001USD400,00",
		"1200,00", "21500,00", "C261010USD1600,00", "C261010USD1600,00", "C261010USD1600,00", "")
	if _, err := ParseMT941(doc); !errors.Is(err, ErrInvalidMT941) {
		t.Errorf("ParseMT941 unbalanced: err = %v, want ErrInvalidMT941", err)
	}
}

// A :64: that does not restate :62F: is a 400.
func TestParseMT941AvailableMismatch(t *testing.T) {
	doc := mt941Doc("AVAIL-1", "acct1", "1", "1", "2610100000", "C261001USD400,00",
		"1200,00", "21500,00", "C261010USD1700,00", "C261010USD1600,00", "C261010USD1700,00", "")
	if _, err := ParseMT941(doc); !errors.Is(err, ErrInvalidMT941) {
		t.Errorf("ParseMT941 :64: mismatch: err = %v, want ErrInvalidMT941", err)
	}
}

// A report for an account the ledger does not hold is refused
// fail-closed with the unknown-account error (the 422 class).
func TestImportMT941UnknownAccount(t *testing.T) {
	l := New()
	doc := mt941Doc("GHOST-1", "ghost-acct", "1", "1", "2610101200", "",
		"00,00", "1150,00", "C261010USD150,00", "C261010USD150,00", "C261010USD150,00", "")
	if _, err := l.ImportMT941("ghost-acct", []byte(doc)); !errors.Is(err, ErrMT941UnknownAccount) {
		t.Errorf("ImportMT941 unknown account: err = %v, want ErrMT941UnknownAccount", err)
	}
}

// A :13D: that is not a valid instant is a 400.
func TestParseMT941InvalidReportDateTime(t *testing.T) {
	doc := mt941Doc("BADDT-1", "acct1", "1", "1", "9913322500", "",
		"00,00", "1150,00", "C991332USD150,00", "C991332USD150,00", "C991332USD150,00", "")
	if _, err := ParseMT941(doc); !errors.Is(err, ErrInvalidMT941) {
		t.Errorf("ParseMT941 bad :13D:: err = %v, want ErrInvalidMT941", err)
	}
}

// The :90D:/:90C: summaries ride along in the report verbatim:
// counts and totals per side.
func TestImportMT941SummariesEchoed(t *testing.T) {
	l := New()
	mt941Seed(t, l)
	// Opening absent: 500,00 − 300,00 = 200,00, but the ledger
	// holds 150,00 — the summaries echo regardless of the gap.
	doc := mt941Doc("SUM-1", "acct1", "1", "1", "2610101200", "",
		"2300,00", "3500,00", "C261010USD200,00", "C261010USD200,00", "C261010USD200,00", "")
	rep, err := l.ImportMT941("acct1", []byte(doc))
	if err != nil {
		t.Fatalf("ImportMT941: %v", err)
	}
	if rep.DebitCount != 2 || rep.DebitTotalCents != 30000 {
		t.Errorf("debit summary = %d/%d, want 2/30000", rep.DebitCount, rep.DebitTotalCents)
	}
	if rep.CreditCount != 3 || rep.CreditTotalCents != 50000 {
		t.Errorf("credit summary = %d/%d, want 3/50000", rep.CreditCount, rep.CreditTotalCents)
	}
}

// Re-importing the same document returns the original report with
// Duplicate set — a re-uploaded report never double-counts.
func TestImportMT941IdempotentReplay(t *testing.T) {
	l := New()
	mt941Seed(t, l)
	doc := mt941Doc("IDEM-1", "acct1", "1", "1", "2610101200", "",
		"00,00", "1150,00", "C261010USD150,00", "C261010USD150,00", "C261010USD150,00", "")
	first, err := l.ImportMT941("acct1", []byte(doc))
	if err != nil {
		t.Fatalf("first ImportMT941: %v", err)
	}
	if first.Duplicate {
		t.Errorf("first import: duplicate = true, want false")
	}
	second, err := l.ImportMT941("acct1", []byte(doc))
	if err != nil {
		t.Fatalf("second ImportMT941: %v", err)
	}
	if !second.Duplicate {
		t.Errorf("second import: duplicate = false, want true")
	}
	if second.DocumentSHA256 != first.DocumentSHA256 {
		t.Errorf("SHA-256 = %q, want %q", second.DocumentSHA256, first.DocumentSHA256)
	}
	if second.ClosingBalanceCents != first.ClosingBalanceCents {
		t.Errorf("replay closing = %d, want %d", second.ClosingBalanceCents, first.ClosingBalanceCents)
	}
}

// The :86: free text is echoed verbatim in the report.
func TestImportMT941SupplementaryEcho(t *testing.T) {
	l := New()
	mt941Seed(t, l)
	supp := "Balance per nostro statement 2026-10, ref OPS-77"
	doc := mt941Doc("SUPP-1", "acct1", "1", "1", "2610101200", "",
		"00,00", "1150,00", "C261010USD150,00", "C261010USD150,00", "C261010USD150,00", supp)
	rep, err := l.ImportMT941("acct1", []byte(doc))
	if err != nil {
		t.Fatalf("ImportMT941: %v", err)
	}
	if rep.Supplementary != supp {
		t.Errorf("supplementary = %q, want %q", rep.Supplementary, supp)
	}
}

// A mixed-currency report — :60F: in USD, :62F: in EUR — is a 400:
// an MT941 report is single-currency.
func TestParseMT941CurrencyMismatch(t *testing.T) {
	doc := mt941Doc("CCY-1", "acct1", "1", "1", "2610100000", "C261001USD400,00",
		"1200,00", "21500,00", "C261010EUR1700,00", "C261010EUR1700,00", "C261010EUR1700,00", "")
	if _, err := ParseMT941(doc); !errors.Is(err, ErrInvalidMT941) {
		t.Errorf("ParseMT941 currency mismatch: err = %v, want ErrInvalidMT941", err)
	}
}
