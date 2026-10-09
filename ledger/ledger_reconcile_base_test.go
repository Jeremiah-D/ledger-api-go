package ledger

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// seedBaseFXLedger posts USD 1000 (debit cash / credit equity) and EUR 900
// (debit fees / credit cash), then installs EUR->USD 108/100. The rate is
// set after the posts so it records EffectiveVersion 2, giving the FX
// snapshot assertion a real version to check.
func seedBaseFXLedger(t *testing.T, l *Ledger, now time.Time) {
	t.Helper()
	posts := []JournalEntry{
		{ID: "b-1", DebitAccount: "cash", CreditAccount: "equity", AmountCents: 1000, Currency: "USD", IdempotencyKey: "bk-1", CreatedAt: now},
		{ID: "b-2", DebitAccount: "fees", CreditAccount: "cash", AmountCents: 900, Currency: "EUR", IdempotencyKey: "bk-2", CreatedAt: now},
	}
	for _, e := range posts {
		if _, dup, err := l.Post(e); err != nil || dup {
			t.Fatalf("Post(%s) = dup=%v err=%v", e.ID, dup, err)
		}
	}
	if err := l.SetFXRate("EUR", "USD", 108, 100); err != nil {
		t.Fatalf("SetFXRate(EUR->USD): %v", err)
	}
}

// A base-currency scan converts every totals row at the rate in effect at
// scan time with the floor integer convention (900 EUR * 108/100 = 972
// USD), lists the rates it used with their effective versions, and sets
// fx_applied. The base currency itself converts at identity.
func TestReconcileBaseCurrencyConvertsTotals(t *testing.T) {
	l := New()
	now := time.Date(2026, 10, 9, 18, 0, 0, 0, time.UTC)
	seedBaseFXLedger(t, l, now)

	report, err := l.ReconcileWithOptions(now, ReconcileOptions{BaseCurrency: "USD"})
	if err != nil {
		t.Fatalf("ReconcileWithOptions: %v", err)
	}
	if !report.FXApplied {
		t.Fatalf("fx_applied = false, want true with base_currency set")
	}
	sum := report.BaseCurrencySummary
	if sum == nil {
		t.Fatalf("base_currency_summary is nil with fx_applied=true")
	}
	if sum.BaseCurrency != "USD" {
		t.Errorf("base_currency = %q, want USD", sum.BaseCurrency)
	}
	if sum.FXIncomplete {
		t.Errorf("fx_incomplete = true, want false when every currency converts")
	}
	if len(sum.MissingRates) != 0 {
		t.Errorf("missing_rates = %v, want empty", sum.MissingRates)
	}
	if sum.MissingRates == nil {
		t.Errorf("missing_rates is nil; it must encode as an empty list, not null")
	}

	// floor(900 * 108 / 100) = 972 EUR-side; USD passes through.
	wantConversions := []CurrencyTotals{
		{Currency: "EUR", TotalDebits: 972, TotalCredits: 972},
		{Currency: "USD", TotalDebits: 1000, TotalCredits: 1000},
	}
	if len(sum.Conversions) != 2 {
		t.Fatalf("conversions = %v, want 2 rows", sum.Conversions)
	}
	for i, want := range wantConversions {
		if got := sum.Conversions[i]; got != want {
			t.Errorf("conversions[%d] = %+v, want %+v", i, got, want)
		}
	}
	if sum.TotalDebitsCents != 1972 || sum.TotalCreditsCents != 1972 {
		t.Errorf("base totals = %d/%d, want 1972/1972",
			sum.TotalDebitsCents, sum.TotalCreditsCents)
	}

	// The snapshot discloses exactly which rate converted each row.
	if len(sum.FXSnapshot) != 2 {
		t.Fatalf("fx_snapshot = %v, want 2 rows", sum.FXSnapshot)
	}
	eur := sum.FXSnapshot[0]
	if eur.Currency != "EUR" || eur.RateNum != 108 || eur.RateDen != 100 || eur.RateAsOfVersion != 2 {
		t.Errorf("fx_snapshot[0] = %+v, want EUR 108/100 at version 2", eur)
	}
	usd := sum.FXSnapshot[1]
	if usd.Currency != "USD" || usd.RateNum != 1 || usd.RateDen != 1 || usd.RateAsOfVersion != 0 {
		t.Errorf("fx_snapshot[1] = %+v, want USD identity 1/1 at version 0", usd)
	}

	if len(sum.Discrepancies) != 0 {
		t.Errorf("discrepancies = %v, want empty on healthy ledger", sum.Discrepancies)
	}
	if sum.Discrepancies == nil {
		t.Errorf("discrepancies is nil; a clean run must encode as an empty list")
	}

	// The plain report shape is untouched by the summary.
	if len(report.CurrencyTotals) != 2 || report.CurrencyTotals[0].Currency != "EUR" {
		t.Errorf("currency_totals = %v, want the unconverted [EUR USD] rows", report.CurrencyTotals)
	}
}

// A currency with no (currency -> base) rate is listed in missing_rates,
// excluded from the converted figures, and the summary says it is
// incomplete — never silently skipped.
func TestReconcileBaseCurrencyMissingRates(t *testing.T) {
	l := New()
	now := time.Date(2026, 10, 9, 18, 0, 0, 0, time.UTC)
	seedBaseFXLedger(t, l, now)
	if _, dup, err := l.Post(JournalEntry{
		ID: "b-3", DebitAccount: "wallet", CreditAccount: "cash",
		AmountCents: 5000, Currency: "JPY", IdempotencyKey: "bk-3", CreatedAt: now,
	}); err != nil || dup {
		t.Fatalf("Post(b-3) = dup=%v err=%v", dup, err)
	}

	report, err := l.ReconcileWithOptions(now, ReconcileOptions{BaseCurrency: "USD"})
	if err != nil {
		t.Fatalf("ReconcileWithOptions: %v", err)
	}
	sum := report.BaseCurrencySummary
	if sum == nil {
		t.Fatalf("base_currency_summary is nil")
	}
	if len(sum.MissingRates) != 1 || sum.MissingRates[0] != "JPY" {
		t.Errorf("missing_rates = %v, want [JPY]", sum.MissingRates)
	}
	if !sum.FXIncomplete {
		t.Errorf("fx_incomplete = false, want true with an unconverted currency")
	}
	// The converted figures cover only the convertible currencies.
	for _, row := range sum.Conversions {
		if row.Currency == "JPY" {
			t.Errorf("JPY appears in conversions despite the missing rate: %+v", row)
		}
	}
	if sum.TotalDebitsCents != 1972 || sum.TotalCreditsCents != 1972 {
		t.Errorf("base totals = %d/%d, want 1972/1972 (JPY excluded)",
			sum.TotalDebitsCents, sum.TotalCreditsCents)
	}
	for _, snap := range sum.FXSnapshot {
		if snap.Currency == "JPY" {
			t.Errorf("JPY appears in fx_snapshot despite the missing rate: %+v", snap)
		}
	}
}

// Without base_currency the report is byte-for-byte the legacy shape:
// fx_applied=false and no summary key at all.
func TestReconcileWithoutBaseCurrencyUnchanged(t *testing.T) {
	l := New()
	now := time.Date(2026, 10, 9, 18, 0, 0, 0, time.UTC)
	seedBaseFXLedger(t, l, now)

	for name, opts := range map[string]ReconcileOptions{
		"Reconcile":            {},
		"ReconcileWithOptions": {},
	} {
		var report ReconciliationReport
		var err error
		if name == "Reconcile" {
			report = l.Reconcile(now)
		} else {
			report, err = l.ReconcileWithOptions(now, opts)
		}
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if report.FXApplied {
			t.Errorf("%s: fx_applied = true, want false without base_currency", name)
		}
		if report.BaseCurrencySummary != nil {
			t.Errorf("%s: base_currency_summary = %+v, want nil", name, report.BaseCurrencySummary)
		}
	}

	// And the summary key serializes as absent, while fx_applied is an
	// explicit false.
	var buf bytes.Buffer
	if err := l.Reconcile(now).WriteJSON(&buf); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `"fx_applied": false`) {
		t.Errorf("report JSON lacks explicit \"fx_applied\": false:\n%s", out)
	}
	if strings.Contains(out, "base_currency_summary") {
		t.Errorf("report JSON carries base_currency_summary without base_currency:\n%s", out)
	}
}

// An invalid base currency fails the scan with ErrInvalidCurrency; the
// ledger itself is untouched and a plain scan still works.
func TestReconcileInvalidBaseCurrency(t *testing.T) {
	l := New()
	now := time.Date(2026, 10, 9, 18, 0, 0, 0, time.UTC)
	seedBaseFXLedger(t, l, now)

	for _, bad := range []string{"usd", "US", "USDD", "12", "US-D"} {
		_, err := l.ReconcileWithOptions(now, ReconcileOptions{BaseCurrency: bad})
		if !errors.Is(err, ErrInvalidCurrency) {
			t.Errorf("base_currency %q: err = %v, want ErrInvalidCurrency", bad, err)
		}
	}
	if report := l.Reconcile(now); !report.AccountingEquationOK {
		t.Errorf("plain Reconcile after invalid options: equation red, error = %q",
			report.AccountingError)
	}
}

// Discrepancies convert into the base currency too, annotated with the
// original currency. The corruption skims 50 EUR cents off cash's EUR
// balance row: debits 0, credits 900, net -950 (expected -900), diff -50.
func TestReconcileBaseCurrencyDiscrepancyConverted(t *testing.T) {
	l := New()
	now := time.Date(2026, 10, 9, 18, 0, 0, 0, time.UTC)
	seedBaseFXLedger(t, l, now)
	l.balances[accountCurrency{account: "cash", currency: "EUR"}] -= 50

	report, err := l.ReconcileWithOptions(now, ReconcileOptions{BaseCurrency: "USD"})
	if err != nil {
		t.Fatalf("ReconcileWithOptions: %v", err)
	}
	if len(report.Discrepancies) != 1 {
		t.Fatalf("report discrepancies = %v, want the single cash/EUR row", report.Discrepancies)
	}
	sum := report.BaseCurrencySummary
	if sum == nil {
		t.Fatalf("base_currency_summary is nil")
	}
	if len(sum.Discrepancies) != 1 {
		t.Fatalf("converted discrepancies = %v, want 1", sum.Discrepancies)
	}
	d := sum.Discrepancies[0]
	if d.Account != "cash" || d.Currency != "EUR" {
		t.Errorf("converted discrepancy = %q/%q, want cash/EUR (original currency annotated)",
			d.Account, d.Currency)
	}
	// floor(x * 108/100): credits 900 -> 972; net -950 -> -1026;
	// expected -900 -> -972; difference -50 -> -54.
	if d.TotalDebitsCents != 0 || d.TotalCreditsCents != 972 {
		t.Errorf("converted totals = %d/%d, want 0/972", d.TotalDebitsCents, d.TotalCreditsCents)
	}
	if d.NetBalanceCents != -1026 || d.ExpectedNetCents != -972 {
		t.Errorf("converted net/expected = %d/%d, want -1026/-972",
			d.NetBalanceCents, d.ExpectedNetCents)
	}
	if d.DifferenceCents != -54 {
		t.Errorf("converted difference = %d, want -54", d.DifferenceCents)
	}
}

// The base-currency scan runs concurrently with posts and other scans
// under -race: the conversion reads the rate table through the same read
// lock as the scan, never a nested lock.
func TestReconcileBaseCurrencyConcurrentRace(t *testing.T) {
	l := New()
	now := time.Date(2026, 10, 9, 18, 0, 0, 0, time.UTC)
	if err := l.SetFXRate("EUR", "USD", 108, 100); err != nil {
		t.Fatalf("SetFXRate: %v", err)
	}

	var wg sync.WaitGroup
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				cur := "EUR"
				if (w+i)%2 == 0 {
					cur = "USD"
				}
				_, _, err := l.Post(JournalEntry{
					ID:             fmt.Sprintf("c-%d-%d", w, i),
					DebitAccount:   "cash",
					CreditAccount:  "equity",
					AmountCents:    100,
					Currency:       cur,
					IdempotencyKey: fmt.Sprintf("ck-%d-%d", w, i),
					CreatedAt:      now,
				})
				if err != nil {
					t.Errorf("Post: %v", err)
					return
				}
			}
		}(w)
	}
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				report, err := l.ReconcileWithOptions(now, ReconcileOptions{BaseCurrency: "USD"})
				if err != nil {
					t.Errorf("ReconcileWithOptions: %v", err)
					return
				}
				if !report.FXApplied || report.BaseCurrencySummary == nil {
					t.Errorf("concurrent scan lost the base-currency summary")
					return
				}
			}
		}()
	}
	wg.Wait()
}
