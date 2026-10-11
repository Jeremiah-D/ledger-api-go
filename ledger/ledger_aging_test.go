package ledger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mustPostAging is the local Post helper for aging tests.
func mustPostAging(t *testing.T, l *Ledger, e JournalEntry) {
	t.Helper()
	if _, _, err := l.Post(e); err != nil {
		t.Fatalf("Post %s: %v", e.ID, err)
	}
}

func agingRow(accounts []AccountAging, account AccountID, currency string) *AccountAging {
	for i := range accounts {
		if accounts[i].Account == account && accounts[i].Currency == currency {
			return &accounts[i]
		}
	}
	return nil
}

// TestAgingFIFOAttribution: a payment retires the oldest lots first, so a
// 90-day-old invoice paid last week does not read as overdue.
func TestAgingFIFOAttribution(t *testing.T) {
	l := New()
	asof := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	post := func(id string, debit, credit AccountID, cents int64, at time.Time) {
		mustPostAging(t, l, JournalEntry{ID: id, DebitAccount: debit, CreditAccount: credit,
			AmountCents: cents, Currency: "USD", IdempotencyKey: "k-" + id, CreatedAt: at})
	}
	post("a1", "cust-a", "ar-clearing", 10000, asof.Add(-100*24*time.Hour)) // 90+ bucket
	post("a2", "cust-a", "ar-clearing", 5000, asof.Add(-10*24*time.Hour))   // 0-30 bucket
	post("a3", "ar-clearing", "cust-a", 8000, asof.Add(-24*time.Hour))      // payment

	report := l.Aging(asof, AgingOptions{})
	row := agingRow(report.Accounts, "cust-a", "USD")
	if row == nil {
		t.Fatal("cust-a/USD missing from aging report")
	}
	// 8000 payment consumes the oldest lot: 10000@90+ -> 2000@90+.
	if row.Buckets.Days90Plus != 2000 {
		t.Fatalf("90+ bucket = %d, want 2000 (oldest lot retired first)", row.Buckets.Days90Plus)
	}
	if row.Buckets.Days0_30 != 5000 {
		t.Fatalf("0-30 bucket = %d, want 5000", row.Buckets.Days0_30)
	}
	if row.Buckets.Days31_60 != 0 || row.Buckets.Days61_90 != 0 {
		t.Fatalf("middle buckets = %d/%d, want 0/0", row.Buckets.Days31_60, row.Buckets.Days61_90)
	}
	if row.TotalCents != 7000 {
		t.Fatalf("total = %d, want 7000", row.TotalCents)
	}
	if row.OverdueCents != 2000 {
		t.Fatalf("overdue = %d, want 2000", row.OverdueCents)
	}
	// ar-clearing is net negative: not a receivable, excluded.
	if agingRow(report.Accounts, "ar-clearing", "USD") != nil {
		t.Fatal("ar-clearing (negative balance) must not appear as a receivable")
	}
}

// TestAgingBucketBoundaries pins the 0-30/31-60/61-90/90+ edges.
func TestAgingBucketBoundaries(t *testing.T) {
	l := New()
	asof := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	// ageDays -> expected bucket: 30->0-30, 31->31-60, 60->31-60,
	// 61->61-90, 90->61-90, 91->90+.
	cases := []struct {
		ageDays int64
		bucket  int
	}{
		{30, agingBucket0_30}, {31, agingBucket31_60}, {60, agingBucket31_60},
		{61, agingBucket61_90}, {90, agingBucket61_90}, {91, agingBucket90Plus},
	}
	for i, c := range cases {
		id := "b" + string(rune('a'+i))
		mustPostAging(t, l, JournalEntry{ID: id, DebitAccount: "cust-b", CreditAccount: "ar-clearing",
			AmountCents: 100, Currency: "USD", IdempotencyKey: "k-" + id,
			CreatedAt: asof.Add(-time.Duration(c.ageDays) * 24 * time.Hour)})
	}
	report := l.Aging(asof, AgingOptions{})
	row := agingRow(report.Accounts, "cust-b", "USD")
	if row == nil {
		t.Fatal("cust-b/USD missing")
	}
	arr := row.Buckets.bucketsArray()
	var want [numAgingBuckets]int64
	for _, c := range cases {
		want[c.bucket] += 100
	}
	for b := 0; b < numAgingBuckets; b++ {
		if arr[b] != want[b] {
			t.Fatalf("bucket %s = %d, want %d", agingBucketLabels[b], arr[b], want[b])
		}
	}
}

// TestAgingAsOfBackdating: age follows the entry's business timestamp,
// not wall-clock posting order.
func TestAgingAsOfBackdating(t *testing.T) {
	l := New()
	posted := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	mustPostAging(t, l, JournalEntry{ID: "d1", DebitAccount: "cust-d", CreditAccount: "ar-clearing",
		AmountCents: 3000, Currency: "USD", IdempotencyKey: "k-d1", CreatedAt: posted})

	early := l.Aging(posted.Add(10*24*time.Hour), AgingOptions{})
	if r := agingRow(early.Accounts, "cust-d", "USD"); r == nil || r.Buckets.Days0_30 != 3000 {
		t.Fatalf("asof+10d: want 3000 in 0-30 bucket, got %+v", r)
	}
	late := l.Aging(posted.Add(40*24*time.Hour), AgingOptions{})
	if r := agingRow(late.Accounts, "cust-d", "USD"); r == nil || r.Buckets.Days31_60 != 3000 {
		t.Fatalf("asof+40d: want 3000 in 31-60 bucket, got %+v", r)
	}
}

// TestAgingFutureEntryClamped: a future-dated entry lands in the youngest
// bucket instead of a negative-age phantom bucket.
func TestAgingFutureEntryClamped(t *testing.T) {
	l := New()
	asof := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	mustPostAging(t, l, JournalEntry{ID: "f1", DebitAccount: "cust-f", CreditAccount: "ar-clearing",
		AmountCents: 1500, Currency: "USD", IdempotencyKey: "k-f1", CreatedAt: asof.Add(24*time.Hour)})
	row := agingRow(l.Aging(asof, AgingOptions{}).Accounts, "cust-f", "USD")
	if row == nil || row.Buckets.Days0_30 != 1500 {
		t.Fatalf("future entry: want 1500 in 0-30 bucket, got %+v", row)
	}
}

// TestAgingOverpaymentCarry: a payment larger than everything owed parks a
// negative carry at its own age; the account is not a receivable while
// net negative.
func TestAgingOverpaymentCarry(t *testing.T) {
	l := New()
	asof := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	mustPostAging(t, l, JournalEntry{ID: "o1", DebitAccount: "cust-o", CreditAccount: "ar-clearing",
		AmountCents: 6000, Currency: "USD", IdempotencyKey: "k-o1", CreatedAt: asof.Add(-10 * 24 * time.Hour)})
	mustPostAging(t, l, JournalEntry{ID: "o2", DebitAccount: "ar-clearing", CreditAccount: "cust-o",
		AmountCents: 10000, Currency: "USD", IdempotencyKey: "k-o2", CreatedAt: asof.Add(-100 * 24 * time.Hour)})

	// Net -4000: not a receivable, excluded by default.
	if agingRow(l.Aging(asof, AgingOptions{}).Accounts, "cust-o", "USD") != nil {
		t.Fatal("net-negative cust-o must be excluded from the default report")
	}
	// Forced via filter: the -4000 carry sits at the payment's age.
	filtered := l.Aging(asof, AgingOptions{Account: "cust-o"})
	row := agingRow(filtered.Accounts, "cust-o", "USD")
	if row == nil {
		t.Fatal("account filter must force the row to appear")
	}
	if row.Buckets.Days90Plus != -4000 || row.TotalCents != -4000 {
		t.Fatalf("overpayment: want -4000 in 90+ bucket, got %+v", row.Buckets)
	}
}

// TestAgingPendingReviewExcluded: booked-but-not-effective rows must not
// age, or bucket sums would stop adding up to the balance.
func TestAgingPendingReviewExcluded(t *testing.T) {
	l := New(WithReviewThreshold(1)) // every transfer goes to review
	asof := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	// Seed the payer so the transfer passes overdraft/validation.
	mustPostAging(t, l, JournalEntry{ID: "p0", DebitAccount: "payer", CreditAccount: "ar-clearing",
		AmountCents: 50000, Currency: "USD", IdempotencyKey: "k-p0", CreatedAt: asof.Add(-100 * 24 * time.Hour)})
	if _, err := l.PostTransfer(Transfer{ID: "pt1", From: "payer", To: "cust-p",
		AmountCents: 20000, Currency: "USD", IdempotencyKey: "k-pt1"}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	report := l.Aging(asof, AgingOptions{})
	// cust-p's pending credit must not appear as a receivable.
	if agingRow(report.Accounts, "cust-p", "USD") != nil {
		t.Fatal("pending-review row leaked into the aging report")
	}
	// payer's row must equal its effective balance (50000), not 30000.
	row := agingRow(report.Accounts, "payer", "USD")
	if row == nil || row.TotalCents != 50000 {
		t.Fatalf("payer row = %+v, want total 50000", row)
	}
}

// TestAgingBucketsSumToBalance: the core invariant — bucket sums always
// equal the current balance — across accounts and currencies.
func TestAgingBucketsSumToBalance(t *testing.T) {
	l := New()
	asof := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	type leg struct {
		id            string
		debit, credit AccountID
		cents         int64
		currency      string
		ageDays       int64
	}
	legs := []leg{
		{"m1", "a", "x", 1000, "USD", 5},
		{"m2", "a", "x", 2000, "USD", 45},
		{"m3", "x", "a", 500, "USD", 70},
		{"m4", "a", "x", 300, "EUR", 200},
		{"m5", "b", "x", 700, "USD", 10},
		{"m6", "x", "b", 900, "USD", 2}, // b net negative
		{"m7", "c", "x", 100, "USD", 400},
	}
	for _, e := range legs {
		mustPostAging(t, l, JournalEntry{ID: e.id, DebitAccount: e.debit, CreditAccount: e.credit,
			AmountCents: e.cents, Currency: e.currency, IdempotencyKey: "k-" + e.id,
			CreatedAt: asof.Add(-time.Duration(e.ageDays) * 24 * time.Hour)})
	}
	report := l.Aging(asof, AgingOptions{})
	for _, row := range report.Accounts {
		arr := row.Buckets.bucketsArray()
		var sum int64
		for _, v := range arr {
			sum += v
		}
		if sum != row.TotalCents {
			t.Fatalf("%s/%s: bucket sum %d != total %d", row.Account, row.Currency, sum, row.TotalCents)
		}
		if bal := l.BalanceIn(row.Account, row.Currency); bal != row.TotalCents {
			t.Fatalf("%s/%s: total %d != BalanceIn %d", row.Account, row.Currency, row.TotalCents, bal)
		}
	}
	// a/USD, a/EUR, c/USD are receivables; b/USD is net negative, x/* net negative.
	if len(report.Accounts) != 3 {
		t.Fatalf("len(Accounts) = %d, want 3 (a/USD, a/EUR, c/USD)", len(report.Accounts))
	}
	// Deterministic order: (a,EUR) < (a,USD) < (c,USD).
	if report.Accounts[0].Account != "a" || report.Accounts[0].Currency != "EUR" ||
		report.Accounts[1].Account != "a" || report.Accounts[1].Currency != "USD" ||
		report.Accounts[2].Account != "c" {
		t.Fatalf("accounts not sorted deterministically: %+v", report.Accounts)
	}
}

// TestAgingAlertThreshold: overdue past the threshold fires one
// aging_alert audit event per breaching account and lists it in Alerts.
func TestAgingAlertThreshold(t *testing.T) {
	al := mustAuditLog(t)
	defer al.Close()
	l := New(WithAuditLog(al))
	asof := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	mustPostAging(t, l, JournalEntry{ID: "g1", DebitAccount: "cust-g", CreditAccount: "ar-clearing",
		AmountCents: 9000, Currency: "USD", IdempotencyKey: "k-g1", CreatedAt: asof.Add(-100 * 24 * time.Hour)})
	mustPostAging(t, l, JournalEntry{ID: "g2", DebitAccount: "cust-h", CreditAccount: "ar-clearing",
		AmountCents: 500, Currency: "USD", IdempotencyKey: "k-g2", CreatedAt: asof.Add(-100 * 24 * time.Hour)})

	report := l.Aging(asof, AgingOptions{OverdueAlertThresholdCents: 1000})
	if len(report.Alerts) != 1 {
		t.Fatalf("len(Alerts) = %d, want 1 (cust-g overdue 9000 > 1000; cust-h 500 below)", len(report.Alerts))
	}
	a := report.Alerts[0]
	if a.Account != "cust-g" || a.OverdueCents != 9000 || a.ThresholdCents != 1000 {
		t.Fatalf("alert = %+v, want cust-g overdue 9000 threshold 1000", a)
	}

	// The audit event must be on disk: flush and read the JSONL.
	if err := al.Close(); err != nil {
		t.Fatalf("audit Close: %v", err)
	}
	var found bool
	entries, err := os.ReadDir(al.Dir())
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, de := range entries {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".jsonl") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(al.Dir(), de.Name()))
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.Contains(line, `"op":"aging_alert"`) && strings.Contains(line, "cust-g") {
				found = true
			}
			if strings.Contains(line, `"op":"aging_alert"`) && strings.Contains(line, "cust-h") {
				t.Fatal("cust-h (below threshold) must not emit an aging_alert")
			}
		}
	}
	if !found {
		t.Fatal("aging_alert audit event for cust-g not found in audit log")
	}
}

// TestAgingThresholdDisabled: no threshold means no alerts and no alert
// audit events, but the overdue numbers are still computed.
func TestAgingThresholdDisabled(t *testing.T) {
	l := New()
	asof := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	mustPostAging(t, l, JournalEntry{ID: "n1", DebitAccount: "cust-n", CreditAccount: "ar-clearing",
		AmountCents: 9000, Currency: "USD", IdempotencyKey: "k-n1", CreatedAt: asof.Add(-100 * 24 * time.Hour)})
	report := l.Aging(asof, AgingOptions{})
	if len(report.Alerts) != 0 {
		t.Fatalf("len(Alerts) = %d, want 0 (threshold disabled)", len(report.Alerts))
	}
	row := agingRow(report.Accounts, "cust-n", "USD")
	if row == nil || row.OverdueCents != 9000 {
		t.Fatalf("overdue still computed: got %+v", row)
	}
}

// TestAgingZeroAsOfUsesNow: the zero AsOf measures age against now.
func TestAgingZeroAsOfUsesNow(t *testing.T) {
	l := New()
	now := time.Now().UTC()
	mustPostAging(t, l, JournalEntry{ID: "z1", DebitAccount: "cust-z", CreditAccount: "ar-clearing",
		AmountCents: 1000, Currency: "USD", IdempotencyKey: "k-z1", CreatedAt: now.Add(-time.Hour)})
	report := l.Aging(now, AgingOptions{})
	if report.AsOf.IsZero() {
		t.Fatal("AsOf must default to now, not stay zero")
	}
	row := agingRow(report.Accounts, "cust-z", "USD")
	if row == nil || row.Buckets.Days0_30 != 1000 {
		t.Fatalf("zero AsOf: want 1000 in 0-30 bucket, got %+v", row)
	}
}
