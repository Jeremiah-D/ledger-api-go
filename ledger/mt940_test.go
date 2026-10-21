package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Exact-fixture test: one inflow, one outflow, an empty pre-window, and
// an IBAN mapping. Every tag byte is pinned — the MT940 consumer (the
// bank's statement processor) parses positions, so drift is a bug.
func TestExportMT940ExactFixture(t *testing.T) {
	l := New()
	iban := "DE89370400440532013000"
	if err := l.SetExternalMapping("acct1", ExternalSystemMT940, iban, "test"); err != nil {
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

	doc, err := l.ExportMT940(MT940ExportOptions{
		Account: "acct1", StatementID: "1", SequenceID: "1",
		Since: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		Until: time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC),
		Currency: "USD",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		":20:1",
		":25:" + iban,
		":28C:1/1",
		":60F:C261001USD0,00",
		":61:2610011001C1000,00NTRF//stmt-e1",
		":86:salary oct",
		":61:2610021002D200,00NTRF//stmt-e2",
		":62F:C261010USD800,00",
		":64:C261010USD800,00",
	}, "\n")
	if doc != want {
		t.Fatalf("MT940 mismatch:\n got:\n%s\nwant:\n%s", doc, want)
	}
}

// Reconciliation: a pre-window inflow folds into the opening balance,
// and closing = opening + signed lines. The renderer re-derives it —
// this test pins the exported numbers, not just the invariant.
func TestExportMT940OpeningReconciliation(t *testing.T) {
	l := New()
	post := func(id, debit, credit string, cents int64, at time.Time) {
		if _, _, err := l.Post(JournalEntry{
			ID: id, DebitAccount: AccountID(debit), CreditAccount: AccountID(credit),
			AmountCents: cents, Currency: "USD", CreatedAt: at,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Pre-window: two inflows, one outflow.
	post("pre-1", "acct1", "bank", 50000, time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC))
	post("pre-2", "acct1", "bank", 30000, time.Date(2026, 9, 30, 11, 0, 0, 0, time.UTC))
	post("pre-3", "bank", "acct1", 10000, time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC))
	// In window: one inflow, one outflow.
	post("w-1", "acct1", "bank", 20000, time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC))
	post("w-2", "bank", "acct1", 5000, time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC))

	doc, err := l.ExportMT940(MT940ExportOptions{
		Account: "acct1",
		Since:   time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		Until:   time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC),
		Currency: "USD",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Opening 500+300-100 = 700,00; lines +200-50; closing 850,00.
	if !strings.Contains(doc, ":60F:C261001USD700,00") {
		t.Fatalf("opening balance not folded from pre-window rows:\n%s", doc)
	}
	if !strings.Contains(doc, ":61:2610051005C200,00NTRF//w-1") {
		t.Fatalf("window inflow line missing:\n%s", doc)
	}
	if !strings.Contains(doc, ":61:2610061006D50,00NTRF//w-2") {
		t.Fatalf("window outflow line missing:\n%s", doc)
	}
	if !strings.Contains(doc, ":62F:C261010USD850,00") {
		t.Fatalf("closing balance does not reconcile:\n%s", doc)
	}
	if !strings.Contains(doc, ":64:C261010USD850,00") {
		t.Fatalf("available balance must equal closing:\n%s", doc)
	}
	// The :61: reference is the journal entry ID (stable across
	// exports, so the bank dedupes on it).
	if strings.Contains(doc, "//pre-1") {
		t.Fatalf("pre-window rows must not leak into the lines:\n%s", doc)
	}
}

// Pending-review rows never touched balances: excluded from the lines
// and from the opening fold, exactly like the other statement exports.
func TestExportMT940ExcludesPendingReview(t *testing.T) {
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
	doc, err := l.ExportMT940(MT940ExportOptions{
		Account: "acct1", Currency: "USD",
		Since: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		Until: time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(doc, "pend-1") {
		t.Fatalf("pending-review row leaked into the statement:\n%s", doc)
	}
	if !strings.Contains(doc, ":60F:C261001USD0,00") || !strings.Contains(doc, ":62F:C261010USD0,00") {
		t.Fatalf("empty statement must foot at zero:\n%s", doc)
	}
}

// Debit balances render with the D mark and a positive amount — the
// sign lives in the mark, never in the number.
func TestExportMT940DebitBalance(t *testing.T) {
	l := New()
	if _, _, err := l.Post(JournalEntry{
		ID: "out-1", DebitAccount: "bank", CreditAccount: "acct1",
		AmountCents: 25000, Currency: "USD",
		CreatedAt: time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	doc, err := l.ExportMT940(MT940ExportOptions{
		Account: "acct1", Currency: "USD",
		Since: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc, ":60F:D261001USD250,00") {
		t.Fatalf("debit opening balance must use the D mark:\n%s", doc)
	}
}

// Fail-loud cases: the exporter must never emit a statement the bank
// cannot trust.
func TestExportMT940FailLoud(t *testing.T) {
	l := New()
	_, err := l.ExportMT940(MT940ExportOptions{Account: "", Currency: "USD"})
	if !errors.Is(err, ErrEmptyAccountID) {
		t.Fatalf("empty account: got %v", err)
	}
	_, err = l.ExportMT940(MT940ExportOptions{Account: "a"})
	if err != nil {
		t.Fatalf("empty currency normalizes to the default currency: got %v", err)
	}
	doc, err := l.ExportMT940(MT940ExportOptions{Account: "a", Currency: DefaultCurrency})
	if err != nil {
		t.Fatalf("default currency export: got %v", err)
	}
	if !strings.Contains(doc, ":60F:C") || !strings.Contains(doc, DefaultCurrency) {
		t.Fatalf("default currency must name the currency tag:\n%s", doc)
	}
	_, err = l.ExportMT940(MT940ExportOptions{Account: "a", Currency: "us!"})
	if !errors.Is(err, ErrInvalidMT940Export) {
		t.Fatalf("bad currency: got %v", err)
	}
	_, err = l.ExportMT940(MT940ExportOptions{
		Account: "a", Currency: "USD",
		Since: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC),
		Until: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
	})
	if !errors.Is(err, ErrInvalidMT940Export) {
		t.Fatalf("inverted window: got %v", err)
	}
}

// The renderer guards its own contract: a line without a value date,
// amount, or reference, and a statement that does not foot, are
// rejected before a single tag ships.
func TestRenderMT940StatementFailLoud(t *testing.T) {
	base := MT940Statement{
		TransactionReference: "1", AccountIdentification: "acct1",
		StatementNumber: "1", SequenceNumber: "1",
		OpeningBalance:   MT940Balance{Credit: true, ValueDate: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Currency: "USD", AmountCents: 10000},
		ClosingBalance:   MT940Balance{Credit: true, ValueDate: time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC), Currency: "USD", AmountCents: 10000},
		AvailableBalance: MT940Balance{Credit: true, ValueDate: time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC), Currency: "USD", AmountCents: 10000},
	}
	line := MT940Line{
		ValueDate: time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC),
		Credit: true, AmountCents: 5000, Reference: "ref-1",
	}
	good := base
	good.Lines = []MT940Line{line}
	good.ClosingBalance.AmountCents = 15000
	good.AvailableBalance.AmountCents = 15000
	if _, err := RenderMT940Statement(good); err != nil {
		t.Fatalf("valid statement rejected: %v", err)
	}

	cases := map[string]func(*MT940Statement){
		"missing value date": func(s *MT940Statement) { s.Lines[0].ValueDate = time.Time{} },
		"missing amount":     func(s *MT940Statement) { s.Lines[0].AmountCents = 0 },
		"missing reference":  func(s *MT940Statement) { s.Lines[0].Reference = " " },
		"closing mismatch":   func(s *MT940Statement) { s.ClosingBalance.AmountCents = 99999 },
		"available mismatch": func(s *MT940Statement) { s.AvailableBalance.AmountCents = 99999 },
	}
	for name, mutate := range cases {
		s := good
		s.Lines = []MT940Line{line}
		mutate(&s)
		if _, err := RenderMT940Statement(s); !errors.Is(err, ErrInvalidMT940Export) {
			t.Fatalf("%s: expected ErrInvalidMT940Export, got %v", name, err)
		}
	}
}

// The idempotency key rides the :61: reference (the entry ID when the
// row carries no key), and references longer than 16x truncate — the
// bank field is max-width.
func TestExportMT940ReferenceRules(t *testing.T) {
	l := New()
	if _, _, err := l.Post(JournalEntry{
		ID: "short-id", DebitAccount: "acct1", CreditAccount: "bank",
		AmountCents: 100, Currency: "USD", IdempotencyKey: "payroll-oct-1",
		CreatedAt: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.Post(JournalEntry{
		ID: "averylongjournalentryidthatexceedssixteenchars", DebitAccount: "acct1", CreditAccount: "bank",
		AmountCents: 100, Currency: "USD",
		CreatedAt: time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	doc, err := l.ExportMT940(MT940ExportOptions{Account: "acct1", Currency: "USD"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc, "//payroll-oct-1") {
		t.Fatalf("idempotency key must ride the reference:\n%s", doc)
	}
	if !strings.Contains(doc, "//averylongjournal") {
		t.Fatalf("long reference must truncate to 16 chars:\n%s", doc)
	}
	if strings.Contains(doc, "averylongjournalentryidthatexceedssixteenchars") {
		t.Fatalf("full 16+ char reference must not leak into :61::\n%s", doc)
	}
}

// An unmapped account exports under its internal account ID.
func TestExportMT940UnmappedAccount(t *testing.T) {
	l := New()
	doc, err := l.ExportMT940(MT940ExportOptions{Account: "internal-acct", Currency: "USD"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc, ":25:internal-acct") {
		t.Fatalf("unmapped account must export under its internal ID:\n%s", doc)
	}
}

// Currency isolation: EUR rows never appear in a USD statement.
func TestExportMT940CurrencyIsolation(t *testing.T) {
	l := New()
	post := func(id, currency string, cents int64) {
		if _, _, err := l.Post(JournalEntry{
			ID: id, DebitAccount: "acct1", CreditAccount: "bank",
			AmountCents: cents, Currency: currency,
			CreatedAt: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC),
		}); err != nil {
			t.Fatal(err)
		}
	}
	post("usd-1", "USD", 10000)
	post("eur-1", "EUR", 10000)
	doc, err := l.ExportMT940(MT940ExportOptions{
		Account: "acct1", Currency: "USD",
		Until: time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(doc, "eur-1") || strings.Contains(doc, "EUR") {
		t.Fatalf("EUR row leaked into the USD statement:\n%s", doc)
	}
	// Opening folds nothing (no pre-window rows); the single USD line
	// foots to the closing balance.
	if !strings.Contains(doc, ":60F:C261005USD0,00") || !strings.Contains(doc, ":62F:C261010USD100,00") {
		t.Fatalf("USD statement must foot on USD rows only:\n%s", doc)
	}
}
