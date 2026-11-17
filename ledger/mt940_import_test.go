package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// mt940Doc renders a minimal MT940 customer statement for the
// tests: :20: :25: :28C: :60F: (:61: [:86:])* :62F: :64:.
// Each entry in bodyLines is one raw line (":61:..." or ":86:...")
// in order; :28C: is fixed to 1/1.
func mt940Doc(ref, acct, opening, closing, avail string, bodyLines ...string) string {
	lines := []string{
		":20:" + ref,
		":25:" + acct,
		":28C:1/1",
		":60F:" + opening,
	}
	lines = append(lines, bodyLines...)
	lines = append(lines, ":62F:"+closing, ":64:"+avail)
	return strings.Join(lines, "\n")
}

// mt940Line renders one :61: body: YYMMDD value date, MMDD entry
// date, D/C mark, 15d comma amount, NTRF, 16x reference.
func mt940Line(valueDate, entryDate, mark, amount, ref string) string {
	return ":61:" + valueDate + entryDate + mark + amount + "NTRF//" + ref
}

// mt940Seed posts one inflow to acct1 (150,00 on 2026-10-05) so the
// ledger balance at the end of any later closing day is 150,00.
// The entry carries no idempotency key, so its :61: reference is
// its ID, "mt940-e1".
func mt940Seed(t *testing.T, l *Ledger) {
	t.Helper()
	if _, _, err := l.Post(JournalEntry{
		ID: "mt940-e1", DebitAccount: "acct1", CreditAccount: "bank",
		AmountCents: 15000, Currency: "USD",
		CreatedAt: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
}

// Round-trip: a statement our own ExportMT940 renders parses back
// to the same statement, and importing it matches every :61: line
// and reports zero mismatches — including through the mt940-system
// external mapping, which names the account by its IBAN in :25:.
func TestParseMT940_RoundTrip(t *testing.T) {
	l := New()
	iban := "DE89370400440532013000"
	if err := l.SetExternalMapping("acct1", ExternalSystemMT940, iban, "test"); err != nil {
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

	doc, err := l.ExportMT940(MT940ExportOptions{
		Account: "acct1", StatementID: "1", SequenceID: "1",
		Since:    time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		Until:    time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC),
		Currency: "USD",
	})
	if err != nil {
		t.Fatal(err)
	}
	// CRLF is tolerated: the bank's downloader may hand us CRLF.
	stmt, err := ParseMT940(strings.ReplaceAll(doc, "\n", "\r\n"))
	if err != nil {
		t.Fatalf("ParseMT940(round-trip): %v\ndoc:\n%s", err, doc)
	}
	if stmt.TransactionReference != "1" || stmt.AccountIdentification != iban {
		t.Errorf("header = %q/%q, want 1/%s", stmt.TransactionReference, stmt.AccountIdentification, iban)
	}
	if stmt.StatementNumber != "1" || stmt.SequenceNumber != "1" {
		t.Errorf(":28C: = %q/%q, want 1/1", stmt.StatementNumber, stmt.SequenceNumber)
	}
	if mt940Signed(stmt.OpeningBalance) != 40000 {
		t.Errorf("opening = %+v, want +40000", stmt.OpeningBalance)
	}
	if len(stmt.Lines) != 3 {
		t.Fatalf("lines = %d, want 3", len(stmt.Lines))
	}
	if stmt.Lines[0].Reference != "bal-e1" || stmt.Lines[0].AmountCents != 100000 || !stmt.Lines[0].Credit {
		t.Errorf("line 1 = %+v, want bal-e1/+100000/C", stmt.Lines[0])
	}
	if stmt.Lines[1].Reference != "bal-e2" || stmt.Lines[1].AmountCents != 20000 || stmt.Lines[1].Credit {
		t.Errorf("line 2 = %+v, want bal-e2/20000/D", stmt.Lines[1])
	}
	if stmt.Lines[2].Reference != "bal-e3" || stmt.Lines[2].AmountCents != 50000 || !stmt.Lines[2].Credit {
		t.Errorf("line 3 = %+v, want bal-e3/+50000/C", stmt.Lines[2])
	}
	if mt940Signed(stmt.ClosingBalance) != 170000 || stmt.ClosingBalance.Currency != "USD" {
		t.Errorf(":62F: = %+v, want +170000 USD", stmt.ClosingBalance)
	}
	if mt940Signed(stmt.AvailableBalance) != 170000 {
		t.Errorf(":64: = %+v, want +170000", stmt.AvailableBalance)
	}

	rep, err := l.ImportMT940("acct1", []byte(doc))
	if err != nil {
		t.Fatalf("ImportMT940(round-trip): %v", err)
	}
	if len(rep.BalanceMismatches) != 0 {
		t.Errorf("mismatches = %+v, want none", rep.BalanceMismatches)
	}
	if rep.Matched != 3 || rep.Mismatched != 0 || rep.Unmatched != 0 {
		t.Errorf("match counts = %d/%d/%d, want 3/0/0", rep.Matched, rep.Mismatched, rep.Unmatched)
	}
	for i, m := range rep.Lines {
		if m.Status != CAMT053Matched || m.EntryID == "" {
			t.Errorf("line %d: status = %q entry = %q, want matched with an entry", i, m.Status, m.EntryID)
		}
	}
	if rep.ClosingBalanceCents != 170000 || rep.Currency != "USD" {
		t.Errorf("report closing = %d %s, want 170000 USD", rep.ClosingBalanceCents, rep.Currency)
	}
	if rep.Duplicate {
		t.Errorf("first import: duplicate = true, want false")
	}
}

// A statement whose :64: comes before :62F: is out of order — a
// 400, not a re-reading.
func TestParseMT940_RejectsBadFieldOrder(t *testing.T) {
	doc := strings.Join([]string{
		":20:ORDER-1",
		":25:acct1",
		":28C:1/1",
		":60F:C261001USD400,00",
		":61:2610011001C1500,00NTRF//L1",
		":64:C261001USD1900,00",
		":62F:C261001USD1900,00",
	}, "\n")
	if _, err := ParseMT940(doc); !errors.Is(err, ErrInvalidMT940) {
		t.Errorf("ParseMT940 bad order: err = %v, want ErrInvalidMT940", err)
	}
}

// An unbalanced statement — :62F: != :60F: + signed lines — is a
// 400.
func TestParseMT940_RejectsUnbalancedStatement(t *testing.T) {
	// Opening 400,00 + credit 1500,00 − debit 200,00 = 1700,00,
	// but :62F: claims 1600,00.
	doc := mt940Doc("UNBAL-1", "acct1", "C261001USD400,00", "C261001USD1600,00", "C261001USD1600,00",
		mt940Line("261001", "1001", "C", "1500,00", "L1"),
		mt940Line("261002", "1002", "D", "200,00", "L2"))
	if _, err := ParseMT940(doc); !errors.Is(err, ErrInvalidMT940) {
		t.Errorf("ParseMT940 unbalanced: err = %v, want ErrInvalidMT940", err)
	}
}

// A :64: that does not restate :62F: is a 400.
func TestParseMT940_RejectsAvailableMismatch(t *testing.T) {
	doc := mt940Doc("AVAIL-1", "acct1", "C261001USD400,00", "C261001USD1700,00", "C261001USD1600,00",
		mt940Line("261001", "1001", "C", "1500,00", "L1"),
		mt940Line("261002", "1002", "D", "200,00", "L2"))
	if _, err := ParseMT940(doc); !errors.Is(err, ErrInvalidMT940) {
		t.Errorf("ParseMT940 :64: mismatch: err = %v, want ErrInvalidMT940", err)
	}
}

// CRLF line endings are accepted.
func TestParseMT940_AcceptsCRLF(t *testing.T) {
	doc := mt940Doc("CRLF-1", "acct1", "C261005USD0,00", "C261005USD150,00", "C261005USD150,00",
		mt940Line("261005", "1005", "C", "150,00", "mt940-e1"))
	stmt, err := ParseMT940(strings.ReplaceAll(doc, "\n", "\r\n"))
	if err != nil {
		t.Fatalf("ParseMT940(CRLF): %v", err)
	}
	if stmt.TransactionReference != "CRLF-1" || len(stmt.Lines) != 1 {
		t.Errorf("parsed = %+v, want ref CRLF-1 with 1 line", stmt)
	}
	if stmt.Lines[0].Reference != "mt940-e1" {
		t.Errorf("line reference = %q, want mt940-e1", stmt.Lines[0].Reference)
	}
}

// MT940 carries no forward balance: a :65: field is outside the
// field set and is refused, like any other unknown field.
func TestParseMT940_RejectsUnknownField(t *testing.T) {
	doc := mt940Doc("FWD-1", "acct1", "C261005USD0,00", "C261005USD150,00", "C261005USD150,00",
		mt940Line("261005", "1005", "C", "150,00", "mt940-e1")) + "\n:65:C261005USD150,00"
	if _, err := ParseMT940(doc); !errors.Is(err, ErrInvalidMT940) {
		t.Errorf("ParseMT940 :65:: err = %v, want ErrInvalidMT940", err)
	}
}

// The :62F: closing balance reconciles with the ledger balance
// folded at the end of the closing day: agreement reports no
// mismatch, disagreement reports both sides of the gap — and the
// import still succeeds.
func TestImportMT940_ReconcilesBalances(t *testing.T) {
	l := New()
	mt940Seed(t, l)

	agree := mt940Doc("BAL-OK", "acct1", "C261005USD0,00", "C261005USD150,00", "C261005USD150,00",
		mt940Line("261005", "1005", "C", "150,00", "mt940-e1"))
	rep, err := l.ImportMT940("acct1", []byte(agree))
	if err != nil {
		t.Fatalf("ImportMT940(agree): %v", err)
	}
	if len(rep.BalanceMismatches) != 0 {
		t.Errorf("mismatches = %+v, want none", rep.BalanceMismatches)
	}
	if rep.ClosingBalanceCents != 15000 {
		t.Errorf("closing = %d, want 15000", rep.ClosingBalanceCents)
	}

	// The statement foots (0 + 150,00 + 50,00 = 200,00) but the
	// ledger holds 150,00 at the end of the closing day.
	disagree := mt940Doc("BAL-GAP", "acct1", "C261005USD0,00", "C261005USD200,00", "C261005USD200,00",
		mt940Line("261005", "1005", "C", "150,00", "mt940-e1"),
		mt940Line("261005", "1005", "C", "50,00", "bank-fee-x"))
	rep, err = l.ImportMT940("acct1", []byte(disagree))
	if err != nil {
		t.Fatalf("ImportMT940(disagree): %v", err)
	}
	if len(rep.BalanceMismatches) != 1 {
		t.Fatalf("mismatches = %+v, want 1", rep.BalanceMismatches)
	}
	m := rep.BalanceMismatches[0]
	if m.Currency != "USD" || m.ReportedCents != 20000 || m.LedgerCents != 15000 {
		t.Errorf("mismatch = %+v, want USD/20000/15000", m)
	}
	if want := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC); !m.At.Equal(want) {
		t.Errorf("mismatch At = %v, want %v (end of the closing day)", m.At, want)
	}
}

// A line whose reference names a journaled row with a different
// amount or direction is mismatched, and a line naming no row is
// unmatched — both are reported, and neither fails the import.
func TestImportMT940_ReportsLineMismatch(t *testing.T) {
	l := New()
	mt940Seed(t, l)

	// Line 1 names the seeded entry but claims 120,00 against the
	// journaled 150,00; line 2 names no journaled row. The
	// statement foots (0 + 120,00 + 30,00 = 150,00).
	doc := mt940Doc("LINE-MM", "acct1", "C261005USD0,00", "C261005USD150,00", "C261005USD150,00",
		mt940Line("261005", "1005", "C", "120,00", "mt940-e1"),
		mt940Line("261005", "1005", "C", "30,00", "no-such-ref"))
	rep, err := l.ImportMT940("acct1", []byte(doc))
	if err != nil {
		t.Fatalf("ImportMT940: %v", err)
	}
	if rep.Matched != 0 || rep.Mismatched != 1 || rep.Unmatched != 1 {
		t.Fatalf("match counts = %d/%d/%d, want 0/1/1", rep.Matched, rep.Mismatched, rep.Unmatched)
	}
	mm := rep.Lines[0]
	if mm.Status != CAMT053Mismatched || mm.EntryID != "mt940-e1" {
		t.Errorf("line 1 = %+v, want mismatched on mt940-e1", mm)
	}
	if mm.EntryAmountCents != 15000 || mm.Reason == "" {
		t.Errorf("line 1 entry amount/reason = %d/%q, want 15000/non-empty", mm.EntryAmountCents, mm.Reason)
	}
	if rep.Lines[1].Status != CAMT053Unmatched {
		t.Errorf("line 2 = %+v, want unmatched", rep.Lines[1])
	}

	// Direction alone can mismatch: the seeded inflow claimed as
	// a debit. The statement foots (0 − 150,00 = −150,00).
	dirDoc := mt940Doc("LINE-DIR", "acct1", "C261005USD0,00", "D261005USD150,00", "D261005USD150,00",
		mt940Line("261005", "1005", "D", "150,00", "mt940-e1"))
	rep, err = l.ImportMT940("acct1", []byte(dirDoc))
	if err != nil {
		t.Fatalf("ImportMT940(direction): %v", err)
	}
	if rep.Mismatched != 1 || rep.Lines[0].Status != CAMT053Mismatched {
		t.Errorf("direction line = %+v, want mismatched", rep.Lines[0])
	}
	if !rep.Lines[0].EntryCredit || rep.Lines[0].EntryAmountCents != 15000 {
		t.Errorf("direction line entry = %+v, want credit/15000", rep.Lines[0])
	}
}

// A statement for an account the ledger does not hold is refused
// fail-closed with the unknown-account error (the 422 class).
func TestImportMT940_RefusesUnknownAccount(t *testing.T) {
	l := New()
	doc := mt940Doc("GHOST-1", "ghost-acct", "C261005USD0,00", "C261005USD150,00", "C261005USD150,00",
		mt940Line("261005", "1005", "C", "150,00", "ghost-e1"))
	if _, err := l.ImportMT940("ghost-acct", []byte(doc)); !errors.Is(err, ErrMT940UnknownAccount) {
		t.Errorf("ImportMT940 unknown account: err = %v, want ErrMT940UnknownAccount", err)
	}
}

// Re-importing the same document returns the original report with
// Duplicate set — a re-uploaded statement never double-counts —
// and emits no second audit event: exactly one
// `mt940_import_received` event, with the specified details and
// actor, and the version unchanged by the receipt.
func TestImportMT940_ReplaySameDocumentNoop(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	mt940Seed(t, l)
	doc := mt940Doc("IDEM-1", "acct1", "C261005USD0,00", "C261005USD150,00", "C261005USD150,00",
		mt940Line("261005", "1005", "C", "150,00", "mt940-e1"))
	first, err := l.ImportMT940("acct1", []byte(doc))
	if err != nil {
		t.Fatalf("first ImportMT940: %v", err)
	}
	if first.Duplicate {
		t.Errorf("first import: duplicate = true, want false")
	}
	second, err := l.ImportMT940("acct1", []byte(doc))
	if err != nil {
		t.Fatalf("second ImportMT940: %v", err)
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
	if second.Matched != first.Matched {
		t.Errorf("replay matched = %d, want %d", second.Matched, first.Matched)
	}

	events := readAuditOps(t, al)
	if countOp(events, "mt940_import_received") != 1 {
		t.Fatalf("mt940_import_received count = %d, want 1", countOp(events, "mt940_import_received"))
	}
	received := findOp(events, "mt940_import_received")
	if received == nil {
		t.Fatalf("no mt940_import_received event in %d events", len(events))
	}
	if received.Actor != "ImportMT940" {
		t.Errorf("audit actor = %q, want ImportMT940", received.Actor)
	}
	if received.Details["document_sha256"] != first.DocumentSHA256 {
		t.Errorf("audit document_sha256 = %v, want %q", received.Details["document_sha256"], first.DocumentSHA256)
	}
	if received.Details["account"] != "acct1" {
		t.Errorf("audit account = %v, want acct1", received.Details["account"])
	}
	if received.Details["statement_number"] != "1" {
		t.Errorf("audit statement_number = %v, want 1", received.Details["statement_number"])
	}
	if received.VersionBefore != received.VersionAfter {
		t.Errorf("audit version moved %d -> %d, want unchanged (receipt only)", received.VersionBefore, received.VersionAfter)
	}
}
