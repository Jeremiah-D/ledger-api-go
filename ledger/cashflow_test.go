package ledger

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"
)

// seedCashFlowLedger builds a ledger for the cash-flow statement tests:
//
//   - cash, cash2: cash-class accounts (marked, but deliberately NOT
//     chart-registered — chart.go's direction control rejects crediting
//     an asset-class account, so cash accounts that pay out stay
//     unregistered per chart.go's own guidance).
//   - sales (revenue), rent (expense), owner (equity), loan (liability),
//     equipment (asset): registered counterparties driving the activity
//     mapping. clearing is left unregistered to exercise the operating
//     fallback.
//
// All amounts in cents. Every posting respects the chart direction
// control: debits name asset/expense (or unregistered) accounts, credits
// name liability/equity/revenue (or unregistered) accounts.
func seedCashFlowLedger(t *testing.T) *Ledger {
	t.Helper()
	l := New()
	reg := func(a AccountID, typ AccountType) {
		t.Helper()
		if err := l.RegisterAccountType(a, typ); err != nil {
			t.Fatalf("RegisterAccountType(%s): %v", a, err)
		}
	}
	reg("sales", AccountTypeRevenue)
	reg("rent", AccountTypeExpense)
	reg("owner", AccountTypeEquity)
	reg("loan", AccountTypeLiability)
	reg("equipment", AccountTypeAsset)

	if err := l.MarkCashAccount("cash", ""); err != nil {
		t.Fatalf("MarkCashAccount(cash): %v", err)
	}
	if err := l.MarkCashAccount("cash2", ""); err != nil {
		t.Fatalf("MarkCashAccount(cash2): %v", err)
	}

	post := func(id string, debit, credit AccountID, cents int64, currency string, at time.Time) {
		t.Helper()
		if _, _, err := l.Post(JournalEntry{
			ID: id, DebitAccount: debit, CreditAccount: credit,
			AmountCents: cents, Currency: currency,
			IdempotencyKey: "cf-" + id, CreatedAt: at,
		}); err != nil {
			t.Fatalf("Post %s: %v", id, err)
		}
	}
	pre := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	day := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

	post("pre", "cash", "sales", 10000, "USD", pre)                         // opening balance
	post("op-in", "cash", "sales", 5000, "USD", day)                        // operating inflow
	post("op-out", "rent", "cash", 2000, "USD", day.Add(time.Hour))         // operating outflow
	post("inv-out", "equipment", "cash", 3000, "USD", day.Add(2*time.Hour)) // investing outflow
	post("fin-in1", "cash", "owner", 4000, "USD", day.Add(3*time.Hour))     // financing inflow
	post("fin-in2", "cash", "loan", 2500, "USD", day.Add(4*time.Hour))      // financing inflow (borrowing)
	post("unreg", "cash", "clearing", 6000, "USD", day.Add(5*time.Hour))    // unregistered counterparty -> operating
	post("eur", "cash", "sales", 9999, "EUR", day.Add(6*time.Hour))         // other currency
	post("internal", "cash", "cash2", 1000, "USD", day.Add(7*time.Hour))    // cash-to-cash transfer
	if _, _, err := l.Post(JournalEntry{
		ID: "pending", DebitAccount: "cash", CreditAccount: "sales",
		AmountCents: 7777, Currency: "USD",
		IdempotencyKey: "cf-pending", CreatedAt: day.Add(8 * time.Hour),
		PendingReview: true,
	}); err != nil {
		t.Fatalf("Post pending: %v", err)
	}
	post("post", "cash", "sales", 2500, "USD", day.Add(48*time.Hour)) // after the period
	return l
}

func cashActivity(acts []CashFlowActivity, a CashActivity) *CashFlowActivity {
	for i := range acts {
		if acts[i].Activity == a {
			return &acts[i]
		}
	}
	return nil
}

func cashRow(rows []CashFlowAccountRow, a AccountID) *CashFlowAccountRow {
	for i := range rows {
		if rows[i].Account == a {
			return &rows[i]
		}
	}
	return nil
}

func TestCashFlowAggregation(t *testing.T) {
	l := seedCashFlowLedger(t)
	now := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	report, err := l.CashFlow(now, CashFlowOptions{From: "2026-10-10", To: "2026-10-10", Currency: "USD"})
	if err != nil {
		t.Fatalf("CashFlow: %v", err)
	}

	cash := cashRow(report.Accounts, "cash")
	if cash == nil {
		t.Fatalf("no account row for cash")
	}
	if cash.OpeningCents != 10000 {
		t.Errorf("cash opening = %d, want 10000", cash.OpeningCents)
	}
	if cash.InflowCents != 18500 { // 5000 + 4000 + 2500 + 6000 + 1000
		t.Errorf("cash inflow = %d, want 18500", cash.InflowCents)
	}
	if cash.OutflowCents != 5000 { // 2000 + 3000
		t.Errorf("cash outflow = %d, want 5000", cash.OutflowCents)
	}
	if cash.NetCents != 13500 {
		t.Errorf("cash net = %d, want 13500", cash.NetCents)
	}
	if cash.ClosingCents != 23500 {
		t.Errorf("cash closing = %d, want 23500", cash.ClosingCents)
	}

	// Per-activity breakdown of the cash account.
	op := cashActivity(cash.Activities, CashActivityOperating)
	if op == nil || op.InflowCents != 12000 || op.OutflowCents != 2000 || op.NetCents != 10000 {
		t.Errorf("cash operating = %+v, want inflow 12000 outflow 2000 net 10000", op)
	}
	inv := cashActivity(cash.Activities, CashActivityInvesting)
	if inv == nil || inv.InflowCents != 0 || inv.OutflowCents != 3000 || inv.NetCents != -3000 {
		t.Errorf("cash investing = %+v, want inflow 0 outflow 3000 net -3000", inv)
	}
	fin := cashActivity(cash.Activities, CashActivityFinancing)
	if fin == nil || fin.InflowCents != 6500 || fin.OutflowCents != 0 || fin.NetCents != 6500 {
		t.Errorf("cash financing = %+v, want inflow 6500 outflow 0 net 6500", fin)
	}

	// The internal cash-to-cash transfer: cash2 carries the other leg —
	// operating outflow 1000 (unregistered counterparty fallback), net
	// -1000, and it nets to zero at the pool level.
	cash2 := cashRow(report.Accounts, "cash2")
	if cash2 == nil {
		t.Fatalf("no account row for cash2")
	}
	if cash2.InflowCents != 0 || cash2.OutflowCents != 1000 || cash2.NetCents != -1000 {
		t.Errorf("cash2 flows = in %d out %d net %d, want 0/1000/-1000",
			cash2.InflowCents, cash2.OutflowCents, cash2.NetCents)
	}
	if cash2.OpeningCents != 0 || cash2.ClosingCents != -1000 {
		t.Errorf("cash2 opening/closing = %d/%d, want 0/-1000", cash2.OpeningCents, cash2.ClosingCents)
	}

	// Report-level activities and totals.
	if got := cashActivity(report.Activities, CashActivityOperating).NetCents; got != 9000 {
		t.Errorf("report operating net = %d, want 9000", got)
	}
	if got := cashActivity(report.Activities, CashActivityInvesting).NetCents; got != -3000 {
		t.Errorf("report investing net = %d, want -3000", got)
	}
	if got := cashActivity(report.Activities, CashActivityFinancing).NetCents; got != 6500 {
		t.Errorf("report financing net = %d, want 6500", got)
	}
	if report.OpeningCents != 10000 || report.InflowCents != 18500 ||
		report.OutflowCents != 6000 || report.NetFlowCents != 12500 ||
		report.ClosingCents != 22500 {
		t.Errorf("report totals = open %d in %d out %d net %d close %d, want 10000/18500/6000/12500/22500",
			report.OpeningCents, report.InflowCents, report.OutflowCents,
			report.NetFlowCents, report.ClosingCents)
	}

	// Reconciliation identity: closing == opening + net flow everywhere.
	if len(report.Discrepancies) != 0 {
		t.Errorf("discrepancies = %+v, want empty", report.Discrepancies)
	}
	if report.ClosingCents != report.OpeningCents+report.NetFlowCents {
		t.Errorf("report identity broken: %d != %d + %d",
			report.ClosingCents, report.OpeningCents, report.NetFlowCents)
	}

	// Deterministic order: accounts sorted, activities fixed order.
	if len(report.Accounts) != 2 || report.Accounts[0].Account != "cash" || report.Accounts[1].Account != "cash2" {
		t.Errorf("accounts not sorted: %+v", report.Accounts)
	}
	for i, want := range cashActivityOrder {
		if report.Activities[i].Activity != want {
			t.Errorf("activities order = %v, want operating/investing/financing", report.Activities)
			break
		}
	}
	if len(report.CashAccounts) != 2 {
		t.Errorf("cash_accounts = %+v, want 2 marks", report.CashAccounts)
	}
}

func TestCashFlowMultiDayRange(t *testing.T) {
	l := seedCashFlowLedger(t)
	now := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	// 2026-10-08..2026-10-10: the pre-period entry becomes in-period.
	report, err := l.CashFlow(now, CashFlowOptions{From: "2026-10-08", To: "2026-10-10", Currency: "USD"})
	if err != nil {
		t.Fatalf("CashFlow: %v", err)
	}
	cash := cashRow(report.Accounts, "cash")
	if cash.OpeningCents != 0 {
		t.Errorf("cash opening = %d, want 0", cash.OpeningCents)
	}
	if cash.InflowCents != 28500 { // 18500 + the 10000 pre entry
		t.Errorf("cash inflow = %d, want 28500", cash.InflowCents)
	}
	if cash.ClosingCents != 23500 {
		t.Errorf("cash closing = %d, want 23500", cash.ClosingCents)
	}
	if len(report.Discrepancies) != 0 {
		t.Errorf("discrepancies = %+v, want empty", report.Discrepancies)
	}
}

func TestCashFlowCurrencyIsolation(t *testing.T) {
	l := seedCashFlowLedger(t)
	now := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	report, err := l.CashFlow(now, CashFlowOptions{From: "2026-10-10", To: "2026-10-10", Currency: "EUR"})
	if err != nil {
		t.Fatalf("CashFlow: %v", err)
	}
	cash := cashRow(report.Accounts, "cash")
	if cash == nil || cash.InflowCents != 9999 || cash.NetCents != 9999 {
		t.Errorf("EUR cash row = %+v, want inflow 9999", cash)
	}
	if len(report.Accounts) != 1 {
		t.Errorf("EUR accounts = %d rows, want 1", len(report.Accounts))
	}
}

func TestCashFlowPendingReviewExcluded(t *testing.T) {
	l := seedCashFlowLedger(t)
	now := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	report, err := l.CashFlow(now, CashFlowOptions{From: "2026-10-10", To: "2026-10-10", Currency: "USD"})
	if err != nil {
		t.Fatalf("CashFlow: %v", err)
	}
	// The 7777-cent pending-review entry must not appear anywhere: the
	// cash inflow would be 26277 with it.
	if report.InflowCents != 18500 {
		t.Errorf("inflow = %d, want 18500 (pending-review 7777 excluded)", report.InflowCents)
	}
}

func TestCashFlowNoMarksEmptyReport(t *testing.T) {
	l := New()
	now := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	report, err := l.CashFlow(now, CashFlowOptions{From: "2026-10-10", To: "2026-10-10", Currency: "USD"})
	if err != nil {
		t.Fatalf("CashFlow: %v", err)
	}
	if len(report.Accounts) != 0 || len(report.Discrepancies) != 0 {
		t.Errorf("accounts/discrepancies = %d/%d, want 0/0", len(report.Accounts), len(report.Discrepancies))
	}
	if report.OpeningCents != 0 || report.InflowCents != 0 || report.OutflowCents != 0 ||
		report.NetFlowCents != 0 || report.ClosingCents != 0 {
		t.Errorf("totals nonzero on empty report: %+v", report)
	}
	// The three activities are always present, even when empty.
	for i, want := range cashActivityOrder {
		if report.Activities[i].Activity != want || report.Activities[i].NetCents != 0 {
			t.Errorf("empty activity %d = %+v, want %s zeros", i, report.Activities[i], want)
		}
	}
}

func TestCashFlowRangeValidation(t *testing.T) {
	l := New()
	now := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		opts CashFlowOptions
		want error
	}{
		{"bad from", CashFlowOptions{From: "2026-1-5", To: "2026-10-10", Currency: "USD"}, ErrInvalidCashFlowRange},
		{"bad to", CashFlowOptions{From: "2026-10-10", To: "not-a-day", Currency: "USD"}, ErrInvalidCashFlowRange},
		{"empty from", CashFlowOptions{From: "", To: "2026-10-10", Currency: "USD"}, ErrInvalidCashFlowRange},
		{"from after to", CashFlowOptions{From: "2026-10-11", To: "2026-10-10", Currency: "USD"}, ErrInvalidCashFlowRange},
		{"empty currency", CashFlowOptions{From: "2026-10-10", To: "2026-10-10", Currency: ""}, ErrEmptyCashFlowCurrency},
		{"bad currency", CashFlowOptions{From: "2026-10-10", To: "2026-10-10", Currency: "usd"}, ErrInvalidCurrency},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := l.CashFlow(now, tc.opts); !strings.Contains(err.Error(), tc.want.Error()) {
				t.Errorf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestMarkCashAccountValidation(t *testing.T) {
	l := New()
	if err := l.MarkCashAccount("", "USD"); err != ErrEmptyCashAccount {
		t.Errorf("empty account err = %v, want ErrEmptyCashAccount", err)
	}
	if err := l.MarkCashAccount("cash", "usd"); err != ErrInvalidCurrency {
		t.Errorf("bad currency err = %v, want ErrInvalidCurrency", err)
	}
	if err := l.MarkCashAccount("cash", ""); err != nil {
		t.Fatalf("mark all-currency: %v", err)
	}
	if !l.IsCashAccount("cash", "USD") || !l.IsCashAccount("cash", "EUR") {
		t.Errorf("all-currency mark not visible in USD/EUR")
	}
	if err := l.MarkCashAccount("settle", "EUR"); err != nil {
		t.Fatalf("mark scoped: %v", err)
	}
	if !l.IsCashAccount("settle", "EUR") {
		t.Errorf("scoped mark not visible in EUR")
	}
	if l.IsCashAccount("settle", "USD") {
		t.Errorf("scoped mark visible in USD, want scoped to EUR")
	}
	// Sorted listing.
	marks := l.CashAccounts()
	want := []CashAccountMark{{Account: "cash"}, {Account: "settle", Currency: "EUR"}}
	if !reflect.DeepEqual(marks, want) {
		t.Errorf("marks = %+v, want %+v", marks, want)
	}
	// Unmarking is scoped too.
	if err := l.UnmarkCashAccount("settle", "EUR"); err != nil {
		t.Fatalf("unmark: %v", err)
	}
	if l.IsCashAccount("settle", "EUR") {
		t.Errorf("mark still visible after unmark")
	}
	if err := l.UnmarkCashAccount("cash", ""); err != nil {
		t.Fatalf("unmark all-currency: %v", err)
	}
	if len(l.CashAccounts()) != 0 {
		t.Errorf("marks = %+v, want empty", l.CashAccounts())
	}
	// Unmarking a mark that was never set is a no-op.
	if err := l.UnmarkCashAccount("nobody", ""); err != nil {
		t.Errorf("unmark unknown: %v", err)
	}
}

func TestCashFlowReadOnly(t *testing.T) {
	l := seedCashFlowLedger(t)
	v0 := l.Version()
	now := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	opts := CashFlowOptions{From: "2026-10-10", To: "2026-10-10", Currency: "USD"}
	r1, err := l.CashFlow(now, opts)
	if err != nil {
		t.Fatalf("CashFlow: %v", err)
	}
	r2, err := l.CashFlow(now, opts)
	if err != nil {
		t.Fatalf("CashFlow: %v", err)
	}
	if l.Version() != v0 {
		t.Errorf("version = %d, want %d (report must not bump)", l.Version(), v0)
	}
	if !reflect.DeepEqual(r1, r2) {
		t.Errorf("report not deterministic across runs")
	}
}

func TestCashFlowSnapshotRoundTrip(t *testing.T) {
	l := seedCashFlowLedger(t)
	if err := l.MarkCashAccount("settle", "EUR"); err != nil {
		t.Fatalf("MarkCashAccount: %v", err)
	}
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	l2, err := ImportSnapshot(&buf)
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if !reflect.DeepEqual(l.CashAccounts(), l2.CashAccounts()) {
		t.Errorf("marks = %+v, want %+v", l2.CashAccounts(), l.CashAccounts())
	}
	// The restored ledger's statement matches the original's.
	now := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	opts := CashFlowOptions{From: "2026-10-10", To: "2026-10-10", Currency: "USD"}
	r1, err := l.CashFlow(now, opts)
	if err != nil {
		t.Fatalf("CashFlow: %v", err)
	}
	r2, err := l2.CashFlow(now, opts)
	if err != nil {
		t.Fatalf("CashFlow on restored: %v", err)
	}
	r1.GeneratedAt, r2.GeneratedAt = time.Time{}, time.Time{}
	if !reflect.DeepEqual(r1, r2) {
		t.Errorf("restored report differs")
	}
}

func TestCashFlowSnapshotRejectsBadMark(t *testing.T) {
	l := New()
	if err := l.MarkCashAccount("cash", ""); err != nil {
		t.Fatalf("MarkCashAccount: %v", err)
	}
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	out := buf.String()
	// Tamper the exported mark: an empty account must fail the import
	// instead of installing a mark that could never have been created.
	bad := strings.Replace(out, `{"account":"cash"}`, `{"account":""}`, 1)
	if bad == out {
		t.Fatalf("exported snapshot has no cash mark row to tamper")
	}
	if _, err := ImportSnapshot(strings.NewReader(bad)); err == nil {
		t.Error("ImportSnapshot accepted an empty-account cash mark")
	}
	// And an invalid currency must fail too.
	bad = strings.Replace(out, `{"account":"cash"}`, `{"account":"cash","currency":"usd"}`, 1)
	if _, err := ImportSnapshot(strings.NewReader(bad)); err == nil {
		t.Error("ImportSnapshot accepted an invalid-currency cash mark")
	}
}

func TestWithCashAccountOption(t *testing.T) {
	l := New(WithCashAccount("cash", ""), WithCashAccount("settle", "EUR"))
	if !l.IsCashAccount("cash", "USD") || !l.IsCashAccount("settle", "EUR") {
		t.Errorf("option marks missing")
	}
	if l.IsCashAccount("settle", "USD") {
		t.Errorf("scoped option mark leaked to USD")
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Errorf("WithCashAccount(\"\") did not panic")
			}
		}()
		_ = WithCashAccount("", "")
	}()
}

func TestCashFlowCSVExport(t *testing.T) {
	l := seedCashFlowLedger(t)
	now := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	report, err := l.CashFlow(now, CashFlowOptions{From: "2026-10-10", To: "2026-10-10", Currency: "USD"})
	if err != nil {
		t.Fatalf("CashFlow: %v", err)
	}
	var buf bytes.Buffer
	if err := report.WriteCSV(&buf); err != nil {
		t.Fatalf("WriteCSV: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1+2*3 { // header + 2 accounts x 3 activities
		t.Fatalf("csv lines = %d, want 7", len(lines))
	}
	if lines[0] != "from,to,currency,account,activity,inflow_cents,outflow_cents,net_cents,opening_cents,closing_cents" {
		t.Errorf("csv header = %q", lines[0])
	}
	// First data row: cash / operating — deterministic (account, activity).
	if !strings.HasPrefix(lines[1], "2026-10-10,2026-10-10,USD,cash,operating,12000,2000,10000,10000,23500") {
		t.Errorf("csv first row = %q", lines[1])
	}
	// JSON round-trips too.
	var jbuf bytes.Buffer
	if err := report.WriteJSON(&jbuf); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	if !strings.Contains(jbuf.String(), `"activity": "operating"`) {
		t.Errorf("json missing operating activity")
	}
}
