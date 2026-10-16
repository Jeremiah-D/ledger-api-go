package ledger

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func mustSetLargeTxThreshold(t *testing.T, l *Ledger, currency string, threshold int64) {
	t.Helper()
	if err := l.SetLargeTxThreshold(currency, threshold); err != nil {
		t.Fatalf("SetLargeTxThreshold(%s, %d): %v", currency, threshold, err)
	}
}

func mustPostCTR(t *testing.T, l *Ledger, id, debit, credit string, amount int64, currency string, at time.Time) {
	t.Helper()
	if _, _, err := l.Post(JournalEntry{
		ID:            id,
		DebitAccount:  AccountID(debit),
		CreditAccount: AccountID(credit),
		AmountCents:   amount,
		Currency:      currency,
		CreatedAt:     at,
	}); err != nil {
		t.Fatalf("Post %s: %v", id, err)
	}
}

// waitForLargeTxReportEvents polls the audit dir until want
// large_tx_report events are visible (the audit writer is async).
func waitForLargeTxReportEvents(t *testing.T, al *AuditLog, day string, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		events, corrupt, err := ReadAuditLog(al.Dir(), day)
		if err != nil {
			t.Fatalf("ReadAuditLog: %v", err)
		}
		if corrupt != 0 {
			t.Fatalf("corrupt = %d, want 0", corrupt)
		}
		n := 0
		for _, ev := range events {
			if ev.Op == "large_tx_report" {
				n++
			}
		}
		if n == want || time.Now().After(deadline) {
			if n != want {
				t.Fatalf("large_tx_report events = %d, want %d", n, want)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestParseLargeTxThresholds(t *testing.T) {
	got, err := ParseLargeTxThresholds("USD:1000000,EUR:900000")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got) != 2 || got[0] != (LargeTxThreshold{Currency: "USD", ThresholdCents: 1000000}) ||
		got[1] != (LargeTxThreshold{Currency: "EUR", ThresholdCents: 900000}) {
		t.Fatalf("got = %+v", got)
	}
	if got, err := ParseLargeTxThresholds(""); err != nil || got != nil {
		t.Fatalf("empty: got=%v err=%v", got, err)
	}
	for _, raw := range []string{
		"USD",            // missing threshold
		"USD:0",          // zero threshold
		"USD:-5",         // negative threshold
		"USD:abc",        // non-numeric
		"XX:100",         // bad currency
		"USD:100,USD",    // second segment malformed
		"usd:100:extra",  // three parts
	} {
		if _, err := ParseLargeTxThresholds(raw); err == nil {
			t.Fatalf("ParseLargeTxThresholds(%q): want error, got nil", raw)
		}
	}
}

func TestLargeTxThresholdValidation(t *testing.T) {
	l := New()
	if err := l.SetLargeTxThreshold("USD", 0); !errors.Is(err, ErrInvalidLargeTxThreshold) {
		t.Fatalf("zero threshold: err = %v", err)
	}
	if err := l.SetLargeTxThreshold("USD", -1); !errors.Is(err, ErrInvalidLargeTxThreshold) {
		t.Fatalf("negative threshold: err = %v", err)
	}
	if err := l.SetLargeTxThreshold("XX", 100); err == nil {
		t.Fatal("bad currency: want error, got nil")
	}
	if err := l.SetLargeTxReportCurrency("XX"); err == nil {
		t.Fatal("bad report currency: want error, got nil")
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("WithLargeTxThresholds with zero threshold: want panic, got none")
			}
		}()
		_ = New(WithLargeTxThresholds(map[string]int64{"USD": 0}))
	}()
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("WithLargeTxReportCurrency with bad code: want panic, got none")
			}
		}()
		_ = New(WithLargeTxReportCurrency("XX"))
	}()
	// Clearing an unset threshold is a no-op, not an error.
	New().ClearLargeTxThreshold("USD")
	if got := New().LargeTxReportCurrency(); got != DefaultCurrency {
		t.Fatalf("default report currency = %q, want %q", got, DefaultCurrency)
	}
}

func TestGenerateLargeTxReportsInflowOutflow(t *testing.T) {
	al, err := NewAuditLog(t.TempDir())
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	defer al.Close()
	l := New(WithAuditLog(al))
	mustSetLargeTxThreshold(t, l, "USD", 100000)

	day := time.Now().UTC()
	// A receives 150000 (inflow), B pays 150000 (outflow); C's 50000
	// stays under the threshold on both sides.
	mustPostCTR(t, l, "e1", "A", "B", 150000, "USD", day)
	mustPostCTR(t, l, "e2", "B", "C", 50000, "USD", day)

	reports, err := l.GenerateLargeTxReports("")
	if err != nil {
		t.Fatalf("GenerateLargeTxReports: %v", err)
	}
	if len(reports) != 2 {
		t.Fatalf("reports = %+v, want 2", reports)
	}
	// Sorted by (account, currency, direction): A/inflow, B/outflow.
	if reports[0].Account != "A" || reports[0].Direction != LargeTxInflow || reports[0].AmountCents != 150000 {
		t.Fatalf("reports[0] = %+v", reports[0])
	}
	if reports[1].Account != "B" || reports[1].Direction != LargeTxOutflow || reports[1].AmountCents != 150000 {
		t.Fatalf("reports[1] = %+v", reports[1])
	}
	for _, r := range reports {
		if r.Currency != "USD" || r.ThresholdCents != 100000 || r.ReportCurrency != "USD" {
			t.Fatalf("report = %+v", r)
		}
		if !r.ConversionOK || r.ConvertedCents != r.AmountCents {
			t.Fatalf("same-currency report must convert 1:1: %+v", r)
		}
		if r.Duplicate {
			t.Fatalf("first generation must not be a duplicate: %+v", r)
		}
		if r.Day != day.UTC().Format("2006-01-02") {
			t.Fatalf("day = %q", r.Day)
		}
	}
	waitForLargeTxReportEvents(t, al, day.UTC().Format("2006-01-02"), 2)

	// Regeneration replays instead of double-reporting.
	again, err := l.GenerateLargeTxReports(day.UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("regenerate: %v", err)
	}
	if len(again) != 2 {
		t.Fatalf("regenerate reports = %d, want 2", len(again))
	}
	for _, r := range again {
		if !r.Duplicate {
			t.Fatalf("regenerated report must be marked duplicate: %+v", r)
		}
	}
	waitForLargeTxReportEvents(t, al, day.UTC().Format("2006-01-02"), 2)
}

func TestGenerateLargeTxReportsNoThreshold(t *testing.T) {
	l := New()
	now := time.Now().UTC()
	mustPostCTR(t, l, "e1", "A", "B", 999999999, "USD", now)
	reports, err := l.GenerateLargeTxReports("")
	if err != nil {
		t.Fatalf("GenerateLargeTxReports: %v", err)
	}
	if len(reports) != 0 {
		t.Fatalf("reports = %+v, want none without thresholds", reports)
	}
}

func TestGenerateLargeTxReportsBadDay(t *testing.T) {
	l := New()
	if _, err := l.GenerateLargeTxReports("15-10-2026"); err == nil {
		t.Fatal("malformed day: want error, got nil")
	}
	if _, err := l.GenerateLargeTxReports("2026-13-99"); err == nil {
		t.Fatal("impossible day: want error, got nil")
	}
}

func TestGenerateLargeTxReportsFXConversion(t *testing.T) {
	l := New()
	mustSetLargeTxThreshold(t, l, "USD", 100000)
	if err := l.SetLargeTxReportCurrency("EUR"); err != nil {
		t.Fatalf("SetLargeTxReportCurrency: %v", err)
	}
	// 1 USD = 0.9 EUR.
	if err := l.SetFXRate("USD", "EUR", 9, 10); err != nil {
		t.Fatalf("SetFXRate: %v", err)
	}
	rate, ok := l.FXRate("USD", "EUR")
	if !ok {
		t.Fatal("rate not found")
	}
	now := time.Now().UTC()
	mustPostCTR(t, l, "e1", "A", "B", 100000, "USD", now)

	reports, err := l.GenerateLargeTxReports("")
	if err != nil {
		t.Fatalf("GenerateLargeTxReports: %v", err)
	}
	if len(reports) != 2 {
		t.Fatalf("reports = %+v, want 2", reports)
	}
	for _, r := range reports {
		if !r.ConversionOK {
			t.Fatalf("ConversionOK = false: %+v", r)
		}
		if r.ConvertedCents != 90000 {
			t.Fatalf("ConvertedCents = %d, want 90000 (floor(100000*9/10))", r.ConvertedCents)
		}
		if r.ReportCurrency != "EUR" {
			t.Fatalf("ReportCurrency = %q, want EUR", r.ReportCurrency)
		}
		if r.RateVersion != rate.EffectiveVersion {
			t.Fatalf("RateVersion = %d, want %d", r.RateVersion, rate.EffectiveVersion)
		}
	}
}

func TestGenerateLargeTxReportsMissingRate(t *testing.T) {
	l := New()
	mustSetLargeTxThreshold(t, l, "JPY", 100000)
	// No JPY->USD rate configured: the flow is reported anyway with the
	// conversion marked missing, never silently dropped.
	now := time.Now().UTC()
	mustPostCTR(t, l, "e1", "A", "B", 200000, "JPY", now)
	reports, err := l.GenerateLargeTxReports("")
	if err != nil {
		t.Fatalf("GenerateLargeTxReports: %v", err)
	}
	if len(reports) != 2 {
		t.Fatalf("reports = %+v, want 2", reports)
	}
	for _, r := range reports {
		if r.ConversionOK {
			t.Fatalf("ConversionOK must be false without a rate: %+v", r)
		}
		if r.ConvertedCents != 0 || r.RateVersion != 0 {
			t.Fatalf("missing conversion must zero the fields: %+v", r)
		}
	}
}

func TestGenerateLargeTxReportsSkipsPendingReview(t *testing.T) {
	l := New(WithReviewThreshold(5000))
	mustSetLargeTxThreshold(t, l, "USD", 100000)
	// 100000 exceeds the review threshold: the transfer books a pending
	// row but moves no money.
	if _, err := l.PostTransfer(Transfer{ID: "t1", From: "A", To: "B", AmountCents: 100000, Currency: "USD"}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	reports, err := l.GenerateLargeTxReports("")
	if err != nil {
		t.Fatalf("GenerateLargeTxReports: %v", err)
	}
	if len(reports) != 0 {
		t.Fatalf("reports = %+v, want none for pending-review rows", reports)
	}
}

func TestGenerateLargeTxReportsAccumulatesAcrossEntries(t *testing.T) {
	l := New()
	mustSetLargeTxThreshold(t, l, "USD", 100000)
	now := time.Now().UTC()
	// Three 40000 inflows to A: individually under, jointly over.
	mustPostCTR(t, l, "e1", "A", "B", 40000, "USD", now)
	mustPostCTR(t, l, "e2", "A", "C", 40000, "USD", now)
	mustPostCTR(t, l, "e3", "A", "D", 40000, "USD", now)
	reports, err := l.GenerateLargeTxReports("")
	if err != nil {
		t.Fatalf("GenerateLargeTxReports: %v", err)
	}
	if len(reports) != 1 || reports[0].Account != "A" || reports[0].Direction != LargeTxInflow || reports[0].AmountCents != 120000 {
		t.Fatalf("reports = %+v", reports)
	}
}

func TestGenerateLargeTxReportsBackdatedDay(t *testing.T) {
	l := New()
	mustSetLargeTxThreshold(t, l, "USD", 100000)
	backdated := time.Now().UTC().AddDate(0, 0, -5)
	mustPostCTR(t, l, "e1", "A", "B", 150000, "USD", backdated)
	// Today's generation sees nothing: the flow belongs to its own day.
	reports, err := l.GenerateLargeTxReports("")
	if err != nil {
		t.Fatalf("GenerateLargeTxReports: %v", err)
	}
	if len(reports) != 0 {
		t.Fatalf("reports = %+v, want none for today", reports)
	}
	day := backdated.Format("2006-01-02")
	reports, err = l.GenerateLargeTxReports(day)
	if err != nil {
		t.Fatalf("GenerateLargeTxReports(%s): %v", day, err)
	}
	if len(reports) != 2 {
		t.Fatalf("reports = %+v, want 2 for the backdated day", reports)
	}
}

func TestLargeTxReportsInReconcile(t *testing.T) {
	l := New()
	mustSetLargeTxThreshold(t, l, "USD", 100000)
	now := time.Now().UTC()
	mustPostCTR(t, l, "e1", "A", "B", 150000, "USD", now)
	if _, err := l.GenerateLargeTxReports(""); err != nil {
		t.Fatalf("GenerateLargeTxReports: %v", err)
	}
	report := l.Reconcile(now)
	if len(report.LargeTxReports) != 2 {
		t.Fatalf("reconcile LargeTxReports = %+v, want 2", report.LargeTxReports)
	}
	// Without generation the section is empty: Reconcile discloses, it
	// never generates.
	l2 := New()
	mustSetLargeTxThreshold(t, l2, "USD", 100000)
	mustPostCTR(t, l2, "e1", "A", "B", 150000, "USD", now)
	if got := l2.Reconcile(now).LargeTxReports; len(got) != 0 {
		t.Fatalf("reconcile without generation = %+v, want empty", got)
	}
}

func TestLargeTxThresholdSnapshotRoundTrip(t *testing.T) {
	src := New(WithLargeTxThresholds(map[string]int64{"USD": 1000000, "EUR": 900000}),
		WithLargeTxReportCurrency("EUR"))
	var buf bytes.Buffer
	if err := src.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	dst, err := ImportSnapshot(&buf)
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	for _, want := range []LargeTxThreshold{{"USD", 1000000}, {"EUR", 900000}} {
		got, ok := dst.LargeTxThreshold(want.Currency)
		if !ok || got != want.ThresholdCents {
			t.Fatalf("restored threshold %s = %d,%v; want %d,true", want.Currency, got, ok, want.ThresholdCents)
		}
	}
	if got := dst.LargeTxReportCurrency(); got != "EUR" {
		t.Fatalf("restored report currency = %q, want EUR", got)
	}
	// Generated report rows are not restored: the audit chain is their
	// durable record, and a restore must not replay them.
	if got := dst.Reconcile(time.Now()).LargeTxReports; len(got) != 0 {
		t.Fatalf("restored reconcile reports = %+v, want empty", got)
	}
}

func TestLargeTxThresholdSnapshotRejectsBadConfig(t *testing.T) {
	src := New()
	mustSetLargeTxThreshold(t, src, "USD", 100000)
	var buf bytes.Buffer
	if err := src.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	// Corrupt the threshold to a non-positive value and to a bad
	// currency; both imports must fail.
	for _, corrupt := range []struct {
		name string
		edit func(string) string
	}{
		{"zero threshold", func(s string) string {
			return strings.Replace(s, `"threshold_cents":100000`, `"threshold_cents":0`, 1)
		}},
		{"bad currency", func(s string) string {
			return strings.Replace(s, `"currency":"USD","threshold_cents":100000`, `"currency":"XX","threshold_cents":100000`, 1)
		}},
	} {
		if _, err := ImportSnapshot(strings.NewReader(corrupt.edit(buf.String()))); err == nil {
			t.Fatalf("%s: want import error, got nil", corrupt.name)
		}
	}
}

func TestGenerateLargeTxReportsPrune(t *testing.T) {
	l := New()
	mustSetLargeTxThreshold(t, l, "USD", 100000)
	old := time.Now().UTC().AddDate(0, 0, -5)
	mustPostCTR(t, l, "e1", "A", "B", 150000, "USD", old)
	day := old.Format("2006-01-02")
	if _, err := l.GenerateLargeTxReports(day); err != nil {
		t.Fatalf("GenerateLargeTxReports(%s): %v", day, err)
	}
	if got := l.largeTxReportsForDayLocked(day); len(got) != 2 {
		t.Fatalf("stored = %d, want 2", len(got))
	}
	// Generating today prunes rows older than yesterday. Force the prune
	// past the amortized interval.
	l.pruneInterval = 0
	if _, err := l.GenerateLargeTxReports(""); err != nil {
		t.Fatalf("GenerateLargeTxReports: %v", err)
	}
	if got := l.largeTxReportsForDayLocked(day); len(got) != 0 {
		t.Fatalf("after prune stored = %+v, want empty", got)
	}
}

func TestGenerateLargeTxReportsRace(t *testing.T) {
	l := New()
	mustSetLargeTxThreshold(t, l, "USD", 100000)
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				id := strings.Join([]string{"e", string(rune('a' + w)), strings.Repeat("x", i)}, "-")
				_, _, _ = l.Post(JournalEntry{
					ID:            id,
					DebitAccount:  "A",
					CreditAccount: "B",
					AmountCents:   1000,
					Currency:      "USD",
					CreatedAt:     time.Now().UTC(),
				})
				_, _ = l.GenerateLargeTxReports("")
			}
		}(w)
	}
	wg.Wait()
	reports, err := l.GenerateLargeTxReports("")
	if err != nil {
		t.Fatalf("GenerateLargeTxReports: %v", err)
	}
	// Generations raced the posts, so the stored rows may hold a
	// mid-run partial total (first generation wins, see
	// GenerateLargeTxReports) — what matters here is exactly one
	// inflow and one outflow row with no duplicates and no data race
	// (the -race detector covers the rest).
	if len(reports) != 2 {
		t.Fatalf("reports = %+v, want 2", reports)
	}
	seen := map[LargeTxDirection]bool{}
	for _, r := range reports {
		if seen[r.Direction] {
			t.Fatalf("duplicate direction row: %+v", reports)
		}
		seen[r.Direction] = true
	}
}
