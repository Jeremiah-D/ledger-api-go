package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Exact-fixture test: one inflow, one outflow, a floor limit, and an
// IBAN mapping under the "mt942" system. Every tag byte is pinned —
// the bank's report processor parses positions, so drift is a bug.
// MT942 has no balance tags (:60F:/:62F:/:64:) and carries the
// :90D:/:90C: number-and-sum trailers after the :61: lines.
func TestExportMT942ExactFixture(t *testing.T) {
	l := New()
	iban := "DE89370400440532013000"
	if err := l.SetExternalMapping("acct1", ExternalSystemMT942, iban, "test"); err != nil {
		t.Fatal(err)
	}
	post := func(id, debit, credit string, cents int64, at time.Time, memo string) {
		if _, _, err := l.Post(JournalEntry{
			ID: id, DebitAccount: AccountID(debit), CreditAccount: AccountID(credit),
			AmountCents: cents, Currency: "USD", Memo: memo, CreatedAt: at,
		}); err != nil {
			t.Fatal(err)
		}
	}
	post("stmt-e1", "acct1", "bank", 100000, time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC), "salary oct")
	post("stmt-e2", "bank", "acct1", 20000, time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC), "")

	doc, err := l.ExportMT942(MT942ExportOptions{
		Account: "acct1", StatementID: "1", SequenceID: "1",
		Since:    time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		Until:    time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC),
		Currency: "USD",
		FloorLimit: &MT942FloorLimit{
			Credit: false, Currency: "USD", AmountCents: 50000,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		":20:1",
		":25:" + iban,
		":28C:1/1",
		":34F:USDD500,00",
		":13D:2610100000",
		":61:2610011001C1000,00NTRF//stmt-e1",
		":86:salary oct",
		":61:2610021002D200,00NTRF//stmt-e2",
		":90D:1200,00",
		":90C:11000,00",
	}, "\n")
	if doc != want {
		t.Fatalf("MT942 mismatch:\n got:\n%s\nwant:\n%s", doc, want)
	}
	for _, forbidden := range []string{":60F:", ":62F:", ":64:", ":65:", ":21:"} {
		if strings.Contains(doc, forbidden) {
			t.Fatalf("MT942 must not carry %s:\n%s", forbidden, doc)
		}
	}
}

// The :90D:/:90C: trailers foot the :61: lines: counts and totals
// per side, re-derived by the renderer — this test pins the exported
// numbers, not just the invariant.
func TestExportMT942SummariesFootLines(t *testing.T) {
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

	doc, err := l.ExportMT942(MT942ExportOptions{
		Account:  "acct1",
		Since:    time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		Until:    time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC),
		Currency: "USD",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Two credits totalling 500,00; three debits totalling 200,00.
	if !strings.Contains(doc, ":90C:2500,00") {
		t.Fatalf("credit trailer must foot 2 lines totalling 500,00:\n%s", doc)
	}
	if !strings.Contains(doc, ":90D:3200,00") {
		t.Fatalf("debit trailer must foot 3 lines totalling 200,00:\n%s", doc)
	}
	if n := strings.Count(doc, ":61:"); n != 5 {
		t.Fatalf("got %d :61: lines, want 5", n)
	}
}

// The :34F: floor limit renders when set (an empty currency renders
// as the report currency) and is omitted when nil.
func TestExportMT942FloorLimitOptional(t *testing.T) {
	l := New()
	if _, _, err := l.Post(JournalEntry{
		ID: "e1", DebitAccount: "acct1", CreditAccount: "bank",
		AmountCents: 10000, Currency: "USD",
		CreatedAt: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	doc, err := l.ExportMT942(MT942ExportOptions{
		Account: "acct1", Currency: "USD",
		FloorLimit: &MT942FloorLimit{Credit: true, AmountCents: 25000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc, ":34F:USDC250,00") {
		t.Fatalf("floor limit must render with the report currency:\n%s", doc)
	}
	doc, err = l.ExportMT942(MT942ExportOptions{Account: "acct1", Currency: "USD"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(doc, ":34F:") {
		t.Fatalf("floor limit must be omitted when nil:\n%s", doc)
	}
}

// Pre-window rows are dropped, never folded: unlike MT940, an MT942
// has no opening balance for them to fold into, so a pre-window
// inflow must not appear in the lines or in the :90C: trailer.
func TestExportMT942PreWindowRowsDroppedNotFolded(t *testing.T) {
	l := New()
	post := func(id, debit, credit string, cents int64, at time.Time) {
		if _, _, err := l.Post(JournalEntry{
			ID: id, DebitAccount: AccountID(debit), CreditAccount: AccountID(credit),
			AmountCents: cents, Currency: "USD", CreatedAt: at,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Pre-window: a large inflow that MT940 would fold into :60F:.
	post("pre-1", "acct1", "bank", 50000, time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC))
	// In window: one inflow, one outflow.
	post("w-1", "acct1", "bank", 20000, time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC))
	post("w-2", "bank", "acct1", 5000, time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC))

	doc, err := l.ExportMT942(MT942ExportOptions{
		Account:  "acct1",
		Since:    time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		Until:    time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC),
		Currency: "USD",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(doc, "pre-1") {
		t.Fatalf("pre-window rows must not leak into the lines:\n%s", doc)
	}
	// The trailers foot only the two window lines: 1 credit of
	// 200,00 and 1 debit of 50,00 — the pre-window 500,00 is nowhere.
	if !strings.Contains(doc, ":90C:1200,00") {
		t.Fatalf("credit trailer must foot only the window inflow:\n%s", doc)
	}
	if !strings.Contains(doc, ":90D:150,00") {
		t.Fatalf("debit trailer must foot only the window outflow:\n%s", doc)
	}
	if strings.Contains(doc, "500,00") {
		t.Fatalf("pre-window amount leaked into the report:\n%s", doc)
	}
}

// Pending-review rows never touched balances: excluded from the
// lines and from the trailers, exactly like the other statement
// exports.
func TestExportMT942ExcludesPendingReview(t *testing.T) {
	l := New(WithReviewThreshold(1))
	// The transfer is above the payer's review threshold, so it books
	// as a pending-review row (LG-42) — which never touched balances.
	if _, err := l.PostTransfer(Transfer{
		ID: "pend-1", From: "acct1", To: "bank",
		AmountCents: 100000, Currency: "USD",
		CreatedAt: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	doc, err := l.ExportMT942(MT942ExportOptions{
		Account: "acct1", Currency: "USD",
		Since: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		Until: time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(doc, "pend-1") {
		t.Fatalf("pending-review row leaked into the report:\n%s", doc)
	}
	if !strings.Contains(doc, ":90D:00,00") || !strings.Contains(doc, ":90C:00,00") {
		t.Fatalf("empty report must carry zero trailers:\n%s", doc)
	}
}

// Fail-loud cases: the exporter must never emit a report the bank
// cannot trust.
func TestExportMT942FailLoud(t *testing.T) {
	l := New()
	_, err := l.ExportMT942(MT942ExportOptions{Account: "", Currency: "USD"})
	if !errors.Is(err, ErrEmptyAccountID) {
		t.Fatalf("empty account: got %v", err)
	}
	_, err = l.ExportMT942(MT942ExportOptions{Account: "a"})
	if err != nil {
		t.Fatalf("empty currency normalizes to the default currency: got %v", err)
	}
	doc, err := l.ExportMT942(MT942ExportOptions{
		Account: "a", Currency: DefaultCurrency,
		FloorLimit: &MT942FloorLimit{Credit: true, AmountCents: 100},
	})
	if err != nil {
		t.Fatalf("default currency export: got %v", err)
	}
	// MT942 carries no balance tags, so the currency surfaces only
	// in the :34F: — and the floor limit inherits the report
	// currency when its own is empty.
	if !strings.Contains(doc, ":34F:"+DefaultCurrency+"C1,00") {
		t.Fatalf("floor limit must inherit the default currency:\n%s", doc)
	}
	_, err = l.ExportMT942(MT942ExportOptions{Account: "a", Currency: "us!"})
	if !errors.Is(err, ErrInvalidMT942Export) {
		t.Fatalf("bad currency: got %v", err)
	}
	_, err = l.ExportMT942(MT942ExportOptions{
		Account: "a", Currency: "USD",
		Since: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC),
		Until: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
	})
	if !errors.Is(err, ErrInvalidMT942Export) {
		t.Fatalf("inverted window: got %v", err)
	}
	_, err = l.ExportMT942(MT942ExportOptions{
		Account: "a", Currency: "USD",
		FloorLimit: &MT942FloorLimit{Credit: true, Currency: "us!", AmountCents: 100},
	})
	if !errors.Is(err, ErrInvalidMT942Export) {
		t.Fatalf("bad floor-limit currency: got %v", err)
	}
	_, err = l.ExportMT942(MT942ExportOptions{
		Account: "a", Currency: "USD",
		FloorLimit: &MT942FloorLimit{Credit: true, Currency: "USD", AmountCents: -100},
	})
	if !errors.Is(err, ErrInvalidMT942Export) {
		t.Fatalf("negative floor limit: got %v", err)
	}
}

// The renderer re-derives the trailers from the lines and fails loud
// on any drift — a report whose trailer does not foot its lines is
// never rendered.
func TestRenderMT942StatementTrailerMismatch(t *testing.T) {
	base := MT942Statement{
		TransactionReference: "1", AccountIdentification: "acct1",
		StatementNumber: "1", SequenceNumber: "1",
		ReportDateTime: time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC),
		Lines: []MT940Line{
			{ValueDate: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), EntryDate: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Credit: true, AmountCents: 100000, Reference: "e1"},
			{ValueDate: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC), EntryDate: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC), Credit: false, AmountCents: 20000, Reference: "e2"},
		},
		DebitSummary:  MT942Summary{Count: 1, AmountCents: 20000},
		CreditSummary: MT942Summary{Count: 1, AmountCents: 100000},
	}
	doc, err := RenderMT942Statement(base)
	if err != nil {
		t.Fatalf("valid report rejected: %v", err)
	}
	if !strings.Contains(doc, ":90D:1200,00") || !strings.Contains(doc, ":90C:11000,00") {
		t.Fatalf("trailers missing or wrong:\n%s", doc)
	}
	drift := base
	drift.DebitSummary.Count = 2
	if _, err := RenderMT942Statement(drift); !errors.Is(err, ErrInvalidMT942Export) {
		t.Fatalf("debit count drift: expected ErrInvalidMT942Export, got %v", err)
	}
	drift = base
	drift.CreditSummary.AmountCents = 99999
	if _, err := RenderMT942Statement(drift); !errors.Is(err, ErrInvalidMT942Export) {
		t.Fatalf("credit total drift: expected ErrInvalidMT942Export, got %v", err)
	}
	drift = base
	drift.FloorLimit = &MT942FloorLimit{Credit: true, Currency: "us!", AmountCents: 100}
	if _, err := RenderMT942Statement(drift); !errors.Is(err, ErrInvalidMT942Export) {
		t.Fatalf("bad floor-limit currency: expected ErrInvalidMT942Export, got %v", err)
	}
	drift = base
	drift.ReportDateTime = time.Time{}
	if _, err := RenderMT942Statement(drift); !errors.Is(err, ErrInvalidMT942Export) {
		t.Fatalf("zero report date/time: expected ErrInvalidMT942Export, got %v", err)
	}
	drift = base
	drift.AccountIdentification = ""
	if _, err := RenderMT942Statement(drift); !errors.Is(err, ErrInvalidMT942Export) {
		t.Fatalf("missing account: expected ErrInvalidMT942Export, got %v", err)
	}
}

// StatementID and SequenceID default to "1" — the :20: reference and
// the :28C: number/sequence both read "1".
func TestExportMT942Defaults(t *testing.T) {
	l := New()
	doc, err := l.ExportMT942(MT942ExportOptions{Account: "acct1", Currency: "USD"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc, ":20:1\n") {
		t.Fatalf("statement id must default to \"1\":\n%s", doc)
	}
	if !strings.Contains(doc, ":28C:1/1") {
		t.Fatalf("statement/sequence must default to \"1/1\":\n%s", doc)
	}
}

// The export is read-only: no version bump. An unknown account
// exports a valid empty report — zero :61: lines, zero trailers.
func TestExportMT942ReadOnlyAndUnknownAccount(t *testing.T) {
	l := New()
	before := l.Version()
	doc, err := l.ExportMT942(MT942ExportOptions{
		Account: "void", Currency: "USD",
		Since: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		Until: time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if l.Version() != before {
		t.Fatalf("export bumped version %d -> %d", before, l.Version())
	}
	if !strings.Contains(doc, ":90D:00,00") || !strings.Contains(doc, ":90C:00,00") {
		t.Fatalf("unknown account must export zero trailers:\n%s", doc)
	}
	if n := strings.Count(doc, ":61:"); n != 0 {
		t.Fatalf("unknown account has %d :61: lines, want 0", n)
	}
}

// An unmapped account exports under its internal account ID — and a
// mapping declared under the "mt940" system does not leak into the
// MT942 :25:: the two messages go to different recipients.
func TestExportMT942MappingIsolation(t *testing.T) {
	l := New()
	if err := l.SetExternalMapping("acct1", ExternalSystemMT940, "MT940-IBAN", "test"); err != nil {
		t.Fatal(err)
	}
	doc, err := l.ExportMT942(MT942ExportOptions{Account: "acct1", Currency: "USD"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc, ":25:acct1") {
		t.Fatalf("account with only an mt940 mapping must export under its internal ID:\n%s", doc)
	}
	if strings.Contains(doc, "MT940-IBAN") {
		t.Fatalf("mt940 mapping leaked into the MT942 report:\n%s", doc)
	}
	// Declaring the mt942 mapping renames :25: for MT942 only.
	if err := l.SetExternalMapping("acct1", ExternalSystemMT942, "NOSTRO-77", "test"); err != nil {
		t.Fatal(err)
	}
	doc, err = l.ExportMT942(MT942ExportOptions{Account: "acct1", Currency: "USD"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc, ":25:NOSTRO-77") {
		t.Fatalf("mt942 mapping must name :25::\n%s", doc)
	}
}

// Repeatability: with Until pinned, the :13D: is pinned too, so a
// repeat export is byte-identical. Without Until, :13D: reads the
// wall clock — the two exports agree once the :13D: line is stripped.
func TestExportMT942Repeatability(t *testing.T) {
	l := New()
	if _, _, err := l.Post(JournalEntry{
		ID: "e1", DebitAccount: "acct1", CreditAccount: "bank",
		AmountCents: 10000, Currency: "USD",
		CreatedAt: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	pinned := MT942ExportOptions{
		Account: "acct1", Currency: "USD",
		Since: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		Until: time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC),
	}
	doc1, err := l.ExportMT942(pinned)
	if err != nil {
		t.Fatal(err)
	}
	doc2, err := l.ExportMT942(pinned)
	if err != nil {
		t.Fatal(err)
	}
	if doc1 != doc2 {
		t.Fatalf("repeat export with pinned Until differs:\nfirst:\n%s\nsecond:\n%s", doc1, doc2)
	}
	if !strings.Contains(doc1, ":13D:2610100000") {
		t.Fatalf(":13D: must pin to Until:\n%s", doc1)
	}

	open := MT942ExportOptions{
		Account: "acct1", Currency: "USD",
		Since: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
	}
	docA, err := l.ExportMT942(open)
	if err != nil {
		t.Fatal(err)
	}
	docB, err := l.ExportMT942(open)
	if err != nil {
		t.Fatal(err)
	}
	strip13D := func(doc string) string {
		var kept []string
		for _, line := range strings.Split(doc, "\n") {
			if strings.HasPrefix(line, ":13D:") {
				continue
			}
			kept = append(kept, line)
		}
		return strings.Join(kept, "\n")
	}
	if strip13D(docA) != strip13D(docB) {
		t.Fatalf("open-ended repeat export differs past :13D::\nfirst:\n%s\nsecond:\n%s", docA, docB)
	}
}
