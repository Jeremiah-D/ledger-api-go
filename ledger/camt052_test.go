package ledger

import (
	"strings"
	"testing"
	"time"
)

// TestCAMT052ExportIntraday is the core LG-170 test: an intraday
// camt.052 report carries the [Since, Until) entries as Ntry rows plus
// per-currency OPBD/CLBD booked-balance snapshots, with the identity
// CLBD == OPBD + window net pinned per currency.
func TestCAMT052ExportIntraday(t *testing.T) {
	l := New()
	acct := AccountID("ops")
	base := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	// Before the window: 150.00 USD inflow.
	mustPostOFX(t, l, "b1", acct, "equity", 15000, "USD", base.Add(-2*time.Hour))
	// Inside the window: +50.00 inflow, -20.00 outflow.
	mustPostOFX(t, l, "w1", acct, "equity", 5000, "USD", base.Add(30*time.Minute))
	mustPostOFX(t, l, "w2", "rent", acct, 2000, "USD", base.Add(time.Hour))
	// After the window: must not appear anywhere.
	mustPostOFX(t, l, "a1", acct, "equity", 99999, "USD", base.Add(25*time.Hour))
	// Another currency inside the window.
	mustPostOFX(t, l, "e1", acct, "equity", 7000, "EUR", base.Add(90*time.Minute))

	doc, err := l.ExportCAMT052(CAMT052ReportOptions{
		Account: acct, RptID: "RPT-1", Since: base, Until: base.Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(doc)
	if !strings.Contains(s, `xmlns="urn:iso:std:iso:20022:tech:xsd:camt.052.001.02"`) {
		t.Fatal("export does not declare the camt.052.001.02 namespace")
	}
	if !strings.HasPrefix(s, "<?xml") {
		t.Fatal("export does not start with the XML declaration")
	}
	if !strings.Contains(s, "<MsgId>RPT-1</MsgId>") {
		t.Fatal("default MsgId not applied")
	}
	// USD: OPBD 150.00 (b1), window net +30.00, CLBD 180.00.
	for _, want := range []string{
		`<Cd>OPBD</Cd>`, `<Cd>CLBD</Cd>`,
		`<Amt Ccy="USD">150.00</Amt>`, `<Amt Ccy="USD">180.00</Amt>`,
		`<Amt Ccy="EUR">0.00</Amt>`, `<Amt Ccy="EUR">70.00</Amt>`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("report missing %q:\n%s", want, s)
		}
	}
	// a1 is outside the window and must not leak into entries or
	// balances: exactly 3 Ntry rows (w1, w2, e1).
	if n := strings.Count(s, "<Ntry>"); n != 3 {
		t.Fatalf("got %d <Ntry> rows, want 3", n)
	}
	if strings.Count(s, "<Bal>") != 4 {
		t.Fatalf("got %d <Bal> rows, want 4 (OPBD+CLBD x 2 currencies)", strings.Count(s, "<Bal>"))
	}
	// Closing == opening + window net is the report's core identity:
	// USD 150.00 + (50.00 - 20.00) = 180.00; EUR 0.00 + 70.00 = 70.00.
	bal := l.BalanceIn(acct, "USD")
	if bal != 18000+99999 { // all-time balance, window-external row included
		t.Fatalf("sanity: BalanceIn(USD) = %d", bal)
	}
}

func TestCAMT052ExportCurrencyFilter(t *testing.T) {
	l := New()
	acct := AccountID("ops")
	base := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	mustPostOFX(t, l, "u1", acct, "equity", 10000, "USD", base.Add(time.Hour))
	mustPostOFX(t, l, "e1", acct, "equity", 9000, "EUR", base.Add(2*time.Hour))

	doc, err := l.ExportCAMT052(CAMT052ReportOptions{
		Account: acct, RptID: "R", Since: base, Until: base.Add(24 * time.Hour), Currency: "USD",
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(doc)
	if strings.Count(s, "<Bal>") != 2 {
		t.Fatalf("filtered report: got %d <Bal>, want 2", strings.Count(s, "<Bal>"))
	}
	if strings.Contains(s, `Ccy="EUR"`) {
		t.Fatal("filtered report leaks EUR balances")
	}
	if strings.Count(s, "<Ntry>") != 1 {
		t.Fatalf("filtered report: got %d <Ntry>, want 1", strings.Count(s, "<Ntry>"))
	}
}

func TestCAMT052ExportEmptyAccount(t *testing.T) {
	l := New()
	base := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	doc, err := l.ExportCAMT052(CAMT052ReportOptions{
		Account: "ghost", RptID: "R-0", Since: base, Until: base.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(doc)
	if strings.Contains(s, "<Bal>") || strings.Contains(s, "<Ntry>") {
		t.Fatal("empty account must export an empty report")
	}
}

func TestCAMT052ExportValidation(t *testing.T) {
	l := New()
	base := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	good := CAMT052ReportOptions{Account: "a", RptID: "R", Since: base, Until: base.Add(time.Hour)}
	for name, mutate := range map[string]func(*CAMT052ReportOptions){
		"empty account":   func(o *CAMT052ReportOptions) { o.Account = "" },
		"empty rpt id":    func(o *CAMT052ReportOptions) { o.RptID = "  " },
		"zero since":      func(o *CAMT052ReportOptions) { o.Since = time.Time{} },
		"zero until":      func(o *CAMT052ReportOptions) { o.Until = time.Time{} },
		"since after until": func(o *CAMT052ReportOptions) { o.Since, o.Until = o.Until, o.Since },
		"since == until":  func(o *CAMT052ReportOptions) { o.Since = o.Until },
		"bad currency":    func(o *CAMT052ReportOptions) { o.Currency = "xx" },
	} {
		o := good
		mutate(&o)
		if _, err := l.ExportCAMT052(o); err == nil {
			t.Fatalf("%s: expected error, got nil", name)
		}
	}
}

// TestCAMT052ExportExcludesPendingReview pins the pending-review rule:
// a row that never touched balances must not appear in the snapshots
// or the entry list (same rule as ExportCAMT053).
func TestCAMT052ExportExcludesPendingReview(t *testing.T) {
	l := New()
	if err := l.SetReviewThreshold(AccountID("payer"), 1); err != nil { // every transfer needs review
		t.Fatalf("SetReviewThreshold: %v", err)
	}
	base := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	mustPostOFX(t, l, "b1", "payer", "equity", 100000, "USD", base.Add(-time.Hour))
	rcpt, err := l.PostTransfer(Transfer{ID: "t1", From: "payer", To: "payee", AmountCents: 5000, Currency: "USD"})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if rcpt.ReviewStatus != ReviewStatusPending {
		t.Fatal("expected the transfer to land in pending review")
	}
	doc, err := l.ExportCAMT052(CAMT052ReportOptions{
		Account: "payer", RptID: "R", Since: base, Until: base.Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(doc)
	// OPBD 1000.00 (b1 only); the pending 50.00 outflow is invisible.
	if !strings.Contains(s, `<Amt Ccy="USD">1000.00</Amt>`) {
		t.Fatalf("OPBD must exclude the pending-review row:\n%s", s)
	}
	if strings.Count(s, "<Ntry>") != 0 {
		t.Fatalf("pending-review row leaked into Ntry list:\n%s", s)
	}
}

func TestCAMT052RenderValidation(t *testing.T) {
	if _, err := RenderCAMT052Report("", "R", "M", nil, nil); err == nil {
		t.Fatal("empty account: expected error")
	}
	if _, err := RenderCAMT052Report("a", "", "M", nil, nil); err == nil {
		t.Fatal("empty rpt id: expected error")
	}
	badBal := []CAMT052Balance{{Type: "XXXX", Currency: "USD", AmountCents: 1, At: time.Now()}}
	if _, err := RenderCAMT052Report("a", "R", "M", badBal, nil); err == nil {
		t.Fatal("bad balance type: expected error")
	}
	noDate := []CAMT052Balance{{Type: CAMT052BalanceOPBD, Currency: "USD", AmountCents: 1}}
	if _, err := RenderCAMT052Report("a", "R", "M", noDate, nil); err == nil {
		t.Fatal("zero balance date: expected error")
	}
	neg := []CAMT052Balance{{Type: CAMT052BalanceCLBD, Currency: "USD", AmountCents: -500, At: time.Now()}}
	doc, err := RenderCAMT052Report("a", "R", "M", neg, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A negative balance renders DBIT with the absolute amount.
	if s := string(doc); !strings.Contains(s, "<CdtDbtInd>DBIT</CdtDbtInd>") || !strings.Contains(s, `<Amt Ccy="USD">5.00</Amt>`) {
		t.Fatalf("negative balance not rendered as DBIT/absolute:\n%s", s)
	}
}
