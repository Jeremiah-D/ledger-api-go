package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Exact-fixture test: a pre-window row folds into :60F:, two window
// credits and one window debit fold into the :90D:/:90C: trailers,
// and a pending-review row is excluded. Every tag byte is pinned —
// the bank's report processor parses positions, so drift is a bug.
// MT941 carries balances and summaries but no :61: lines.
func TestExportMT941ExactFixture(t *testing.T) {
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
	// Opening 400,00 + credits 1500,00 − debits 200,00 = closing 1700,00.
	want := strings.Join([]string{
		":20:1",
		":25:" + iban,
		":28C:1/1",
		":13D:2610100000",
		":60F:C261001USD400,00",
		":90D:1200,00",
		":90C:21500,00",
		":62F:C261010USD1700,00",
		":64:C261010USD1700,00",
		":65:C261010USD1700,00",
	}, "\n")
	if doc != want {
		t.Fatalf("MT941 mismatch:\n got:\n%s\nwant:\n%s", doc, want)
	}
	for _, forbidden := range []string{":61:", ":34F:", ":21:"} {
		if strings.Contains(doc, forbidden) {
			t.Fatalf("MT941 must not carry %s:\n%s", forbidden, doc)
		}
	}
}

// The :90D:/:90C: trailers foot the window rows: counts and totals
// per side — this test pins the exported numbers, not just the
// invariant. With no pre-window rows :60F: is omitted and the
// closing balance is credits − debits.
func TestExportMT941SummariesFootWindow(t *testing.T) {
	l := New()
	post := func(id, debit, credit string, cents int64, at time.Time) {
		if _, _, err := l.Post(JournalEntry{
			ID: id, DebitAccount: AccountID(debit), CreditAccount: AccountID(credit),
			AmountCents: cents, Currency: "USD", CreatedAt: at,
		}); err != nil {
			t.Fatal(err)
		}
	}
	post("w-1", "acct1", "bank", 20000, time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC))
	post("w-2", "acct1", "bank", 30000, time.Date(2026, 10, 5, 11, 0, 0, 0, time.UTC))
	post("w-3", "bank", "acct1", 5000, time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC))
	post("w-4", "bank", "acct1", 7000, time.Date(2026, 10, 6, 11, 0, 0, 0, time.UTC))
	post("w-5", "bank", "acct1", 8000, time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC))

	doc, err := l.ExportMT941(MT941ExportOptions{
		Account:  "acct1",
		Since:    time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		Until:    time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC),
		Currency: "USD",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Two credits totalling 500,00; three debits totalling 200,00;
	// closing 300,00 with no opening balance.
	if !strings.Contains(doc, ":90C:2500,00") {
		t.Fatalf("credit trailer must foot 2 lines totalling 500,00:\n%s", doc)
	}
	if !strings.Contains(doc, ":90D:3200,00") {
		t.Fatalf("debit trailer must foot 3 lines totalling 200,00:\n%s", doc)
	}
	if !strings.Contains(doc, ":62F:C261010USD300,00") {
		t.Fatalf("closing must be credits − debits with no opening:\n%s", doc)
	}
	if strings.Contains(doc, ":60F:") {
		t.Fatalf("no pre-window rows: :60F: must be omitted:\n%s", doc)
	}
	if n := strings.Count(doc, ":61:"); n != 0 {
		t.Fatalf("got %d :61: lines, want 0", n)
	}
}

// :64: and :65: repeat :62F: exactly — the ledger has no intraday
// credit-line or forward-balance concept.
func TestExportMT941AvailableAndForwardRepeatClosing(t *testing.T) {
	l := New()
	if _, _, err := l.Post(JournalEntry{
		ID: "a1", DebitAccount: "acct1", CreditAccount: "bank",
		AmountCents: 75000, Currency: "USD",
		CreatedAt: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	doc, err := l.ExportMT941(MT941ExportOptions{
		Account:  "acct1",
		Since:    time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		Until:    time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC),
		Currency: "USD",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range []string{
		":62F:C261010USD750,00",
		":64:C261010USD750,00",
		":65:C261010USD750,00",
	} {
		if !strings.Contains(doc, tag) {
			t.Fatalf("missing %s:\n%s", tag, doc)
		}
	}
}

// StatementID/SequenceID default to "1" — :20: and :28C: pin them.
func TestExportMT941StatementIDDefaults(t *testing.T) {
	l := New()
	doc, err := l.ExportMT941(MT941ExportOptions{
		Account:  "acct1",
		Since:    time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		Until:    time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC),
		Currency: "USD",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc, ":20:1\n") {
		t.Fatalf("transaction reference must default to 1:\n%s", doc)
	}
	if !strings.Contains(doc, ":28C:1/1\n") {
		t.Fatalf("statement/sequence must default to 1/1:\n%s", doc)
	}
}

// A repeat export over a pinned Until is byte-identical (:13D: pins
// Until); with no Until the report stamps the wall clock, so the
// comparison strips the :13D: line — the balance dates are date-only
// and stable within the day.
func TestExportMT941RepeatExportByteIdentical(t *testing.T) {
	l := New()
	if _, _, err := l.Post(JournalEntry{
		ID: "r1", DebitAccount: "acct1", CreditAccount: "bank",
		AmountCents: 10000, Currency: "USD",
		CreatedAt: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	opts := MT941ExportOptions{
		Account:  "acct1",
		Since:    time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		Until:    time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC),
		Currency: "USD",
	}
	first, err := l.ExportMT941(opts)
	if err != nil {
		t.Fatal(err)
	}
	second, err := l.ExportMT941(opts)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("pinned-Until repeat export differs:\n%s\n---\n%s", first, second)
	}

	stripReportDate := func(doc string) string {
		var kept []string
		for _, line := range strings.Split(doc, "\n") {
			if strings.HasPrefix(line, ":13D:") {
				continue
			}
			kept = append(kept, line)
		}
		return strings.Join(kept, "\n")
	}
	open := opts
	open.Until = time.Time{}
	a, err := l.ExportMT941(open)
	if err != nil {
		t.Fatal(err)
	}
	b, err := l.ExportMT941(open)
	if err != nil {
		t.Fatal(err)
	}
	if stripReportDate(a) != stripReportDate(b) {
		t.Fatalf("open-ended repeat export differs after stripping :13D::\n%s\n---\n%s", a, b)
	}
}

// An unknown account exports a valid empty report — zero balances,
// zero trailers, no :60F:, never an error.
func TestExportMT941UnknownAccountEmptyReport(t *testing.T) {
	l := New()
	doc, err := l.ExportMT941(MT941ExportOptions{
		Account:  "void",
		Since:    time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		Until:    time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC),
		Currency: "USD",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		":62F:C261010USD0,00",
		":64:C261010USD0,00",
		":65:C261010USD0,00",
		":90D:00,00",
		":90C:00,00",
	} {
		if !strings.Contains(doc, want) {
			t.Fatalf("empty report missing %s:\n%s", want, doc)
		}
	}
	if strings.Contains(doc, ":60F:") {
		t.Fatalf("empty report must not carry :60F::\n%s", doc)
	}
	if strings.Contains(doc, ":61:") {
		t.Fatalf("empty report must not carry :61::\n%s", doc)
	}
}

// Invalid options fail loud in the MT941 error domain.
func TestExportMT941InvalidOptions(t *testing.T) {
	l := New()
	if _, err := l.ExportMT941(MT941ExportOptions{Currency: "USD"}); !errors.Is(err, ErrEmptyAccountID) {
		t.Errorf("empty account: err=%v, want ErrEmptyAccountID", err)
	}
	if _, err := l.ExportMT941(MT941ExportOptions{Account: "acct1", Currency: "US"}); !errors.Is(err, ErrInvalidMT941Export) {
		t.Errorf("invalid currency: err=%v, want ErrInvalidMT941Export", err)
	}
	since := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if _, err := l.ExportMT941(MT941ExportOptions{Account: "acct1", Currency: "USD", Since: since, Until: until}); !errors.Is(err, ErrInvalidMT941Export) {
		t.Errorf("since > until: err=%v, want ErrInvalidMT941Export", err)
	}
}

// A pending-review row never touches the report: it appears neither
// in the trailers nor in the closing balance.
func TestExportMT941PendingReviewExcluded(t *testing.T) {
	l := New()
	post := func(id string, cents int64, pending bool) {
		if _, _, err := l.Post(JournalEntry{
			ID: id, DebitAccount: "acct1", CreditAccount: "bank",
			AmountCents: cents, Currency: "USD",
			CreatedAt:     time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC),
			PendingReview: pending,
		}); err != nil {
			t.Fatal(err)
		}
	}
	post("p-ok", 10000, false)
	post("p-pending", 99900, true)
	doc, err := l.ExportMT941(MT941ExportOptions{
		Account:  "acct1",
		Since:    time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		Until:    time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC),
		Currency: "USD",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc, ":90C:1100,00") {
		t.Fatalf("trailer must foot only the approved row:\n%s", doc)
	}
	if !strings.Contains(doc, ":62F:C261010USD100,00") {
		t.Fatalf("closing must exclude the pending row:\n%s", doc)
	}
}

// :25: names the "mt941" mapping when one exists; a mapping declared
// under "mt940" does not leak into the balance report.
func TestExportMT941ExternalMapping(t *testing.T) {
	l := New()
	if err := l.SetExternalMapping("acct1", ExternalSystemMT940, "MT940-IBAN", "test"); err != nil {
		t.Fatal(err)
	}
	doc, err := l.ExportMT941(MT941ExportOptions{
		Account:  "acct1",
		Since:    time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		Until:    time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC),
		Currency: "USD",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc, ":25:acct1\n") {
		t.Fatalf(":25: must name the internal ID when only an mt940 mapping exists:\n%s", doc)
	}
	if err := l.SetExternalMapping("acct1", ExternalSystemMT941, "DE89370400440532013000", "test"); err != nil {
		t.Fatal(err)
	}
	doc, err = l.ExportMT941(MT941ExportOptions{
		Account:  "acct1",
		Since:    time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		Until:    time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC),
		Currency: "USD",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc, ":25:DE89370400440532013000\n") {
		t.Fatalf(":25: must name the mt941 mapping when one exists:\n%s", doc)
	}
}

// The renderer re-derives the reconciliation and fails loud: a
// closing balance that does not foot, and a :64:/:65: that does not
// repeat the closing balance, are errors — never rendered reports.
func TestRenderMT941StatementReconciliationFailLoud(t *testing.T) {
	day := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	valid := func() MT941Statement {
		return MT941Statement{
			TransactionReference:  "1",
			AccountIdentification: "acct1",
			StatementNumber:       "1",
			SequenceNumber:        "1",
			ReportDateTime:        day,
			OpeningBalance:        &MT940Balance{Credit: true, ValueDate: day, Currency: "USD", AmountCents: 40000},
			DebitSummary:          MT942Summary{Count: 1, AmountCents: 20000},
			CreditSummary:         MT942Summary{Count: 2, AmountCents: 150000},
			ClosingBalance:        MT940Balance{Credit: true, ValueDate: day, Currency: "USD", AmountCents: 170000},
			AvailableBalance:      MT940Balance{Credit: true, ValueDate: day, Currency: "USD", AmountCents: 170000},
			ForwardBalance:        MT940Balance{Credit: true, ValueDate: day, Currency: "USD", AmountCents: 170000},
		}
	}
	if _, err := RenderMT941Statement(valid()); err != nil {
		t.Fatalf("valid statement must render: %v", err)
	}
	badClosing := valid()
	badClosing.ClosingBalance.AmountCents = 169900
	if _, err := RenderMT941Statement(badClosing); !errors.Is(err, ErrInvalidMT941Export) {
		t.Errorf("closing mismatch: err=%v, want ErrInvalidMT941Export", err)
	}
	badAvailable := valid()
	badAvailable.AvailableBalance.AmountCents = 170100
	if _, err := RenderMT941Statement(badAvailable); !errors.Is(err, ErrInvalidMT941Export) {
		t.Errorf(":64: mismatch: err=%v, want ErrInvalidMT941Export", err)
	}
	badForward := valid()
	badForward.ForwardBalance.AmountCents = 169900
	if _, err := RenderMT941Statement(badForward); !errors.Is(err, ErrInvalidMT941Export) {
		t.Errorf(":65: mismatch: err=%v, want ErrInvalidMT941Export", err)
	}
	badTrailer := valid()
	badTrailer.CreditSummary.AmountCents = 149900
	if _, err := RenderMT941Statement(badTrailer); !errors.Is(err, ErrInvalidMT941Export) {
		t.Errorf("trailer mismatch: err=%v, want ErrInvalidMT941Export", err)
	}
}

// The optional :86: renders last, truncated to the 65x field width,
// and is omitted when empty.
func TestRenderMT941StatementSupplementary(t *testing.T) {
	day := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	stmt := MT941Statement{
		TransactionReference:  "1",
		AccountIdentification: "acct1",
		StatementNumber:       "1",
		SequenceNumber:        "1",
		ReportDateTime:        day,
		CreditSummary:         MT942Summary{Count: 1, AmountCents: 10000},
		ClosingBalance:        MT940Balance{Credit: true, ValueDate: day, Currency: "USD", AmountCents: 10000},
		AvailableBalance:      MT940Balance{Credit: true, ValueDate: day, Currency: "USD", AmountCents: 10000},
		ForwardBalance:        MT940Balance{Credit: true, ValueDate: day, Currency: "USD", AmountCents: 10000},
		Supplementary:         "balance as of report",
	}
	doc, err := RenderMT941Statement(stmt)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(doc, ":86:balance as of report") {
		t.Fatalf(":86: must render last:\n%s", doc)
	}
	stmt.Supplementary = ""
	doc, err = RenderMT941Statement(stmt)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(doc, ":86:") {
		t.Fatalf("empty supplementary must omit :86::\n%s", doc)
	}
}
