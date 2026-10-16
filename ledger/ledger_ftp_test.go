package ledger

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

var ftpDay = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

func testFtpAccounts() FtpAccounts {
	return FtpAccounts{
		PoolAccount:    "treasury-pool",
		ExpenseAccount: "ftp-expense",
		RevenueAccount: "ftp-revenue",
	}
}

func TestSetFtpCurveValidation(t *testing.T) {
	l := New()
	bad := []struct {
		name   string
		cur    string
		points []FtpTenorPoint
	}{
		{"empty", "USD", nil},
		{"zero tenor", "USD", []FtpTenorPoint{{0, 100}}},
		{"tenor over cap", "USD", []FtpTenorPoint{{1201, 100}}},
		{"negative rate", "USD", []FtpTenorPoint{{12, -1}}},
		{"absurd rate", "USD", []FtpTenorPoint{{12, 100001}}},
		{"duplicate tenor", "USD", []FtpTenorPoint{{12, 100}, {12, 200}}},
		{"bad currency", "US", []FtpTenorPoint{{12, 100}}},
		{"lowercase currency", "usd", []FtpTenorPoint{{12, 100}}},
	}
	for _, tc := range bad {
		if _, err := l.SetFtpCurve(tc.cur, tc.points); !errors.Is(err, ErrInvalidFtpCurve) {
			t.Fatalf("%s: err = %v, want ErrInvalidFtpCurve", tc.name, err)
		}
	}
	// Unsorted input is stored sorted, not rejected.
	got, err := l.SetFtpCurve("USD", []FtpTenorPoint{{12, 500}, {1, 100}, {6, 300}})
	if err != nil {
		t.Fatalf("unsorted: %v", err)
	}
	if len(got.Points) != 3 || got.Points[0].TenorMonths != 1 || got.Points[2].TenorMonths != 12 {
		t.Fatalf("points not sorted: %+v", got.Points)
	}
	if got.Version != 1 {
		t.Fatalf("version = %d, want 1", got.Version)
	}
}

func TestSetFtpCurveVersioning(t *testing.T) {
	l := New()
	if _, err := l.SetFtpCurve("USD", []FtpTenorPoint{{12, 500}}); err != nil {
		t.Fatal(err)
	}
	c2, err := l.SetFtpCurve("USD", []FtpTenorPoint{{12, 600}})
	if err != nil {
		t.Fatal(err)
	}
	if c2.Version != 2 {
		t.Fatalf("version = %d, want 2", c2.Version)
	}
	got, ok := l.FtpCurveFor("USD")
	if !ok || got.Version != 2 || got.Points[0].RateBps != 600 {
		t.Fatalf("FtpCurveFor = %+v, %v; want version 2 at 600bps", got, ok)
	}
	if _, ok := l.FtpCurveFor("EUR"); ok {
		t.Fatal("FtpCurveFor(EUR) = true, want false")
	}
	// The returned curve is a copy: mutating it must not move the
	// stored curve.
	got.Points[0].RateBps = 1
	again, _ := l.FtpCurveFor("USD")
	if again.Points[0].RateBps != 600 {
		t.Fatal("FtpCurveFor returned a live reference")
	}
}

func TestFtpRateInterpolation(t *testing.T) {
	l := New()
	if _, err := l.SetFtpCurve("USD", []FtpTenorPoint{{1, 100}, {12, 500}}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		tenor int64
		want  int64
	}{
		{1, 100},    // exact bucket
		{12, 500},   // exact bucket
		{6, 281},    // 100 + 400*5/11 = 100 + 181 (integer truncation)
		{24, 500},   // clamped to the nearest endpoint
		{1200, 500}, // clamped
	}
	for _, tc := range cases {
		rate, ver, err := l.FtpRateBps("USD", tc.tenor)
		if err != nil || rate != tc.want || ver != 1 {
			t.Fatalf("tenor %d: rate=%d ver=%d err=%v; want %d/1", tc.tenor, rate, ver, err, tc.want)
		}
	}
	if _, _, err := l.FtpRateBps("USD", 0); !errors.Is(err, ErrFtpTenorOutOfRange) {
		t.Fatalf("tenor 0: err = %v, want ErrFtpTenorOutOfRange", err)
	}
	if _, _, err := l.FtpRateBps("USD", 1201); !errors.Is(err, ErrFtpTenorOutOfRange) {
		t.Fatalf("tenor 1201: err = %v, want ErrFtpTenorOutOfRange", err)
	}
	if _, _, err := l.FtpRateBps("EUR", 12); !errors.Is(err, ErrFtpCurveNotFound) {
		t.Fatalf("unknown currency: err = %v, want ErrFtpCurveNotFound", err)
	}
}

func TestFtpPreviewZeroSideEffect(t *testing.T) {
	l := New(WithFtpAccounts(testFtpAccounts()))
	if _, err := l.SetFtpCurve("USD", []FtpTenorPoint{{12, 500}}); err != nil {
		t.Fatal(err)
	}
	ver := l.Version()
	p, err := l.FtpPreview("bu-lending", "USD", 12, 1200000)
	if err != nil {
		t.Fatal(err)
	}
	// floor(1200000 * 500/10000 * 12/12) = 60000.
	if p.ChargeCents != 60000 || p.RateBps != 500 || p.CurveVersion != 1 || p.Direction != FtpDirectionCost {
		t.Fatalf("preview = %+v; want charge 60000 @500bps v1 cost", p)
	}
	if l.Version() != ver {
		t.Fatalf("preview bumped version %d -> %d", ver, l.Version())
	}
	if totals := l.Reconcile(time.Now()).FtpTotals; len(totals) != 0 {
		t.Fatalf("preview booked charges: %+v", totals)
	}
	// A surplus previews the revenue direction.
	p2, err := l.FtpPreview("bu-deposits", "USD", 12, -600000)
	if err != nil {
		t.Fatal(err)
	}
	if p2.Direction != FtpDirectionRevenue || p2.ChargeCents != 30000 {
		t.Fatalf("surplus preview = %+v; want revenue 30000", p2)
	}
}

func TestChargeFtpGap(t *testing.T) {
	l := New(WithFtpAccounts(testFtpAccounts()))
	if _, err := l.SetFtpCurve("USD", []FtpTenorPoint{{12, 500}}); err != nil {
		t.Fatal(err)
	}
	got, dup, err := l.ChargeFtp("bu-lending", "USD", 12, 1200000, ftpDay)
	if err != nil || dup {
		t.Fatalf("ChargeFtp = dup=%v err=%v", dup, err)
	}
	if got.AmountCents != 60000 {
		t.Fatalf("charge = %d, want 60000", got.AmountCents)
	}
	// Gap: the unit pays — debit FTP expense, credit the pool.
	if got.DebitAccount != "ftp-expense" || got.CreditAccount != "treasury-pool" {
		t.Fatalf("legs = %s/%s, want ftp-expense/treasury-pool", got.DebitAccount, got.CreditAccount)
	}
	if got.Memo != "ftp:charge" {
		t.Fatalf("memo = %q, want ftp:charge", got.Memo)
	}
	if got.ID != "ftp/USD/bu-lending/12/2026-11-03" {
		t.Fatalf("ID = %q, want deterministic ftp ID", got.ID)
	}
	if got.Currency != "USD" {
		t.Fatalf("currency = %q, want USD", got.Currency)
	}
	if b := l.Balance("ftp-expense"); b != 60000 {
		t.Fatalf("expense balance = %d, want 60000", b)
	}
}

func TestChargeFtpSurplus(t *testing.T) {
	l := New(WithFtpAccounts(testFtpAccounts()))
	if _, err := l.SetFtpCurve("USD", []FtpTenorPoint{{12, 500}}); err != nil {
		t.Fatal(err)
	}
	got, _, err := l.ChargeFtp("bu-deposits", "USD", 12, -600000, ftpDay)
	if err != nil {
		t.Fatal(err)
	}
	if got.AmountCents != 30000 {
		t.Fatalf("charge = %d, want 30000", got.AmountCents)
	}
	// Surplus: the pool pays the unit — debit pool, credit FTP revenue.
	if got.DebitAccount != "treasury-pool" || got.CreditAccount != "ftp-revenue" {
		t.Fatalf("legs = %s/%s, want treasury-pool/ftp-revenue", got.DebitAccount, got.CreditAccount)
	}
}

func TestChargeFtpIdempotentPerDay(t *testing.T) {
	l := New(WithFtpAccounts(testFtpAccounts()))
	if _, err := l.SetFtpCurve("USD", []FtpTenorPoint{{12, 500}}); err != nil {
		t.Fatal(err)
	}
	first, dup, err := l.ChargeFtp("bu-lending", "USD", 12, 1200000, ftpDay)
	if err != nil || dup {
		t.Fatalf("first = dup=%v err=%v", dup, err)
	}
	ver := l.Version()
	second, dup, err := l.ChargeFtp("bu-lending", "USD", 12, 999999, ftpDay)
	if err != nil || !dup {
		t.Fatalf("replay = dup=%v err=%v, want dup=true", dup, err)
	}
	if second.ID != first.ID || second.AmountCents != first.AmountCents {
		t.Fatal("replay did not return the original entry")
	}
	if l.Version() != ver {
		t.Fatalf("replay bumped version %d -> %d", ver, l.Version())
	}
}

func TestChargeFtpErrors(t *testing.T) {
	// No accounts configured.
	l := New()
	if _, err := l.SetFtpCurve("USD", []FtpTenorPoint{{12, 500}}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.FtpPreview("bu", "USD", 12, 1000); !errors.Is(err, ErrFtpAccountsNotConfigured) {
		t.Fatalf("preview without accounts: err = %v, want ErrFtpAccountsNotConfigured", err)
	}
	if _, _, err := l.ChargeFtp("bu", "USD", 12, 1000, ftpDay); !errors.Is(err, ErrFtpAccountsNotConfigured) {
		t.Fatalf("charge without accounts: err = %v, want ErrFtpAccountsNotConfigured", err)
	}
	// No curve for the currency.
	l2 := New(WithFtpAccounts(testFtpAccounts()))
	if _, _, err := l2.ChargeFtp("bu", "EUR", 12, 1000, ftpDay); !errors.Is(err, ErrFtpCurveNotFound) {
		t.Fatalf("charge without curve: err = %v, want ErrFtpCurveNotFound", err)
	}
	// Zero gap, bad tenor, slash account.
	l3 := New(WithFtpAccounts(testFtpAccounts()))
	if _, err := l3.SetFtpCurve("USD", []FtpTenorPoint{{12, 500}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l3.ChargeFtp("bu", "USD", 12, 0, ftpDay); !errors.Is(err, ErrFtpZeroGap) {
		t.Fatalf("zero gap: err = %v, want ErrFtpZeroGap", err)
	}
	if _, _, err := l3.ChargeFtp("bu", "USD", 0, 1000, ftpDay); !errors.Is(err, ErrFtpTenorOutOfRange) {
		t.Fatalf("tenor 0: err = %v, want ErrFtpTenorOutOfRange", err)
	}
	if _, _, err := l3.ChargeFtp("b/u", "USD", 12, 1000, ftpDay); err == nil {
		t.Fatal("slash account accepted")
	}
	if _, _, err := l3.ChargeFtp("bu", "usd", 12, 1000, ftpDay); !errors.Is(err, ErrInvalidCurrency) {
		t.Fatalf("lowercase currency: err = %v, want ErrInvalidCurrency", err)
	}
	// Dust: a zero-rate curve prices a zero charge — nothing is booked.
	l4 := New(WithFtpAccounts(testFtpAccounts()))
	if _, err := l4.SetFtpCurve("USD", []FtpTenorPoint{{12, 0}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l4.ChargeFtp("bu", "USD", 12, 100000, ftpDay); !errors.Is(err, ErrNonPositiveAmount) {
		t.Fatalf("dust charge: err = %v, want ErrNonPositiveAmount", err)
	}
}

func TestFtpReconcileTotals(t *testing.T) {
	l := New(WithFtpAccounts(testFtpAccounts()))
	if _, err := l.SetFtpCurve("USD", []FtpTenorPoint{{12, 500}}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.SetFtpCurve("EUR", []FtpTenorPoint{{12, 250}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.ChargeFtp("bu-lending", "USD", 12, 1200000, ftpDay); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.ChargeFtp("bu-deposits", "USD", 12, -600000, ftpDay); err != nil {
		t.Fatal(err)
	}
	// floor(400000 * 250/10000 * 12/12) = 10000.
	if _, _, err := l.ChargeFtp("bu-eu", "EUR", 12, 400000, ftpDay); err != nil {
		t.Fatal(err)
	}
	rep := l.Reconcile(ftpDay)
	if len(rep.FtpTotals) != 2 {
		t.Fatalf("FtpTotals = %+v, want 2 currencies", rep.FtpTotals)
	}
	byCur := map[string]FtpTotals{}
	for _, ft := range rep.FtpTotals {
		byCur[ft.Currency] = ft
	}
	usd := byCur["USD"]
	if usd.Currency != "USD" || usd.CostCents != 60000 || usd.RevenueCents != 30000 || usd.Charges != 2 {
		t.Fatalf("USD totals = %+v; want cost 60000 revenue 30000 charges 2", usd)
	}
	eur := byCur["EUR"]
	if eur.Currency != "EUR" || eur.CostCents != 10000 || eur.RevenueCents != 0 || eur.Charges != 1 {
		t.Fatalf("EUR totals = %+v; want cost 10000 revenue 0 charges 1", eur)
	}
	if len(rep.FtpCurveVersions) != 2 || rep.FtpCurveVersions[0].Version != 1 {
		t.Fatalf("FtpCurveVersions = %+v, want 2 curves at version 1", rep.FtpCurveVersions)
	}
	if !rep.AccountingEquationOK {
		t.Fatalf("accounting equation broken: %s", rep.AccountingError)
	}
}

func TestFtpSnapshotRoundTrip(t *testing.T) {
	l := New(WithFtpAccounts(testFtpAccounts()))
	if _, err := l.SetFtpCurve("USD", []FtpTenorPoint{{1, 100}, {12, 500}}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.SetFtpCurve("USD", []FtpTenorPoint{{1, 150}, {12, 550}}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	l2, err := ImportSnapshot(&buf)
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	c, ok := l2.FtpCurveFor("USD")
	if !ok {
		t.Fatal("curve lost in snapshot round-trip")
	}
	if c.Version != 2 || len(c.Points) != 2 || c.Points[1].RateBps != 550 {
		t.Fatalf("restored curve = %+v; want version 2 with 550bps at 12m", c)
	}
	// Charges work on the restored ledger: the accounts round-tripped.
	got, _, err := l2.ChargeFtp("bu-lending", "USD", 12, 1200000, ftpDay)
	if err != nil {
		t.Fatalf("ChargeFtp on restored ledger: %v", err)
	}
	// 550bps at 12m: floor(1200000 * 550/10000) = 66000.
	if got.AmountCents != 66000 {
		t.Fatalf("restored charge = %d, want 66000 (v2 curve)", got.AmountCents)
	}
}

func TestFtpSnapshotRejectsBadCurve(t *testing.T) {
	l := New(WithFtpAccounts(testFtpAccounts()))
	if _, err := l.SetFtpCurve("USD", []FtpTenorPoint{{12, 500}}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatal(err)
	}
	// Corrupt the curve row: a zero version could never have been set.
	var out []string
	spliced := false
	for _, line := range strings.Split(buf.String(), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decode snapshot line: %v", err)
		}
		if rec["record"] == "config" {
			curves, _ := rec["ftp_curves"].([]any)
			if len(curves) != 1 {
				t.Fatalf("ftp_curves = %v, want 1 row", rec["ftp_curves"])
			}
			row, _ := curves[0].(map[string]any)
			row["version"] = float64(0)
			spliced = true
		}
		b, _ := json.Marshal(rec)
		out = append(out, string(b))
	}
	if !spliced {
		t.Fatal("no config record found in snapshot")
	}
	if _, err := ImportSnapshot(strings.NewReader(strings.Join(out, "\n"))); err == nil {
		t.Fatal("ImportSnapshot accepted a zero-version FTP curve")
	}
}

func TestWithFtpAccountsPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("WithFtpAccounts did not panic on duplicate accounts")
		}
	}()
	WithFtpAccounts(FtpAccounts{PoolAccount: "p", ExpenseAccount: "p", RevenueAccount: "r"})
}

func TestChargeFtpEntersHashChain(t *testing.T) {
	l := New(WithFtpAccounts(testFtpAccounts()))
	if _, err := l.SetFtpCurve("USD", []FtpTenorPoint{{12, 500}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.ChargeFtp("bu-lending", "USD", 12, 1200000, ftpDay); err != nil {
		t.Fatal(err)
	}
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
}
