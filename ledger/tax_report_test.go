package ledger

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"
)

// taxTestLedger builds a ledger with a VAT output rule (19% on merchant
// transfers) and an input-tax rule (19% on supplier payouts via
// capture events... simpler: second transfer rule on supplier prefix).
func taxTestLedger(t *testing.T, opts ...Option) *Ledger {
	t.Helper()
	rules := []PostingRule{
		{
			Name:          "vat-output",
			EventKind:     PostingEventTransfer,
			AccountPrefix: "merchant-",
			Legs: []PostingRuleLeg{
				{AccountTemplate: "vat-payable", Side: "credit", AmountExpr: "bps:1900"},
			},
		},
		{
			Name:          "vat-input",
			EventKind:     PostingEventTransfer,
			AccountPrefix: "supplier-",
			Legs: []PostingRuleLeg{
				{AccountTemplate: "vat-receivable", Side: "debit", AmountExpr: "bps:1900"},
			},
		},
	}
	all := append([]Option{WithPostingRules(rules)}, opts...)
	return New(all...)
}

func taxTestTransfer(t *testing.T, l *Ledger, id, from, to string, cents int64) {
	t.Helper()
	if _, err := l.PostTransfer(Transfer{
		ID: id, From: AccountID(from), To: AccountID(to),
		AmountCents: cents,
	}); err != nil {
		t.Fatalf("transfer %s: %v", id, err)
	}
}

func TestTaxReportAggregates(t *testing.T) {
	l := taxTestLedger(t)
	ctx := context.Background()
	fundAccount(t, l, "merchant-a", 1000000, "")
	fundAccount(t, l, "supplier-a", 1000000, "")
	start := time.Now().Add(-time.Hour)

	taxTestTransfer(t, l, "tx-1", "merchant-a", "customer-1", 100000) // vat 19000
	taxTestTransfer(t, l, "tx-2", "merchant-a", "customer-2", 200000) // vat 38000
	taxTestTransfer(t, l, "tx-3", "supplier-a", "vendor-1", 50000)    // input vat 9500

	report, err := l.TaxReport(ctx, TaxReportOptions{
		From: start,
		To:   time.Now().Add(time.Hour),
		Rules: []TaxRuleSpec{
			{Name: "vat-output", Kind: TaxRuleOutput},
			{Name: "vat-input", Kind: TaxRuleInput},
		},
	})
	if err != nil {
		t.Fatalf("TaxReport: %v", err)
	}
	if len(report.Lines) != 2 {
		t.Fatalf("lines = %d, want 2", len(report.Lines))
	}
	// Deterministic order: rule name sort.
	if report.Lines[0].Rule != "vat-input" || report.Lines[1].Rule != "vat-output" {
		t.Fatalf("line order = %q,%q, want vat-input,vat-output",
			report.Lines[0].Rule, report.Lines[1].Rule)
	}
	if report.Lines[1].Cents != 57000 || report.Lines[1].EntryCount != 2 {
		t.Errorf("vat-output = %d cents x %d, want 57000 x 2",
			report.Lines[1].Cents, report.Lines[1].EntryCount)
	}
	if report.Lines[0].Cents != 9500 || report.Lines[0].EntryCount != 1 {
		t.Errorf("vat-input = %d cents x %d, want 9500 x 1",
			report.Lines[0].Cents, report.Lines[0].EntryCount)
	}
	if report.TotalOutputCents != 57000 || report.TotalInputCents != 9500 {
		t.Errorf("totals output=%d input=%d, want 57000/9500",
			report.TotalOutputCents, report.TotalInputCents)
	}
	if report.RateSnapshots["vat-output"] != 1900 || report.RateSnapshots["vat-input"] != 1900 {
		t.Errorf("rate snapshots = %v, want 1900/1900", report.RateSnapshots)
	}
	if report.Signature != "" {
		t.Errorf("unsigned report carries a signature")
	}
}

func TestTaxReportPeriodAndCurrencyFilter(t *testing.T) {
	l := taxTestLedger(t)
	ctx := context.Background()
	fundAccount(t, l, "merchant-a", 1000000, "")
	taxTestTransfer(t, l, "tx-1", "merchant-a", "customer-1", 100000)

	// Period before the transfer: empty.
	empty, err := l.TaxReport(ctx, TaxReportOptions{
		From:  time.Now().Add(-2 * time.Hour),
		To:    time.Now().Add(-time.Hour),
		Rules: []TaxRuleSpec{{Name: "vat-output", Kind: TaxRuleOutput}},
	})
	if err != nil {
		t.Fatalf("TaxReport: %v", err)
	}
	if len(empty.Lines) != 0 || empty.TotalOutputCents != 0 {
		t.Errorf("out-of-period report not empty: %+v", empty.Lines)
	}
	// Currency filter with no match: empty.
	nomatch, err := l.TaxReport(ctx, TaxReportOptions{
		From:     time.Now().Add(-time.Hour),
		To:       time.Now().Add(time.Hour),
		Currency: "EUR",
		Rules:    []TaxRuleSpec{{Name: "vat-output", Kind: TaxRuleOutput}},
	})
	if err != nil {
		t.Fatalf("TaxReport: %v", err)
	}
	if len(nomatch.Lines) != 0 {
		t.Errorf("currency-filtered report not empty: %+v", nomatch.Lines)
	}
}

func TestTaxReportExemptAccounts(t *testing.T) {
	l := taxTestLedger(t)
	ctx := context.Background()
	fundAccount(t, l, "merchant-a", 1000000, "")
	fundAccount(t, l, "merchant-exempt-1", 1000000, "")
	taxTestTransfer(t, l, "tx-1", "merchant-a", "customer-1", 100000)        // 19000
	taxTestTransfer(t, l, "tx-2", "merchant-exempt-1", "customer-2", 100000) // excluded

	report, err := l.TaxReport(ctx, TaxReportOptions{
		From:                  time.Now().Add(-time.Hour),
		To:                    time.Now().Add(time.Hour),
		Rules:                 []TaxRuleSpec{{Name: "vat-output", Kind: TaxRuleOutput}},
		ExemptAccountPrefixes: []string{"merchant-exempt"},
	})
	if err != nil {
		t.Fatalf("TaxReport: %v", err)
	}
	if len(report.Lines) != 1 || report.Lines[0].Cents != 19000 || report.Lines[0].EntryCount != 1 {
		t.Fatalf("exempt report = %+v, want single 19000 line", report.Lines)
	}
}

func TestTaxReportValidation(t *testing.T) {
	l := taxTestLedger(t)
	ctx := context.Background()
	now := time.Now()
	if _, err := l.TaxReport(ctx, TaxReportOptions{From: now, To: now.Add(-time.Hour)}); !errors.Is(err, ErrInvalidTaxReportOptions) {
		t.Errorf("inverted period err = %v, want ErrInvalidTaxReportOptions", err)
	}
	if _, err := l.TaxReport(ctx, TaxReportOptions{
		From: now.Add(-time.Hour), To: now,
		Rules: []TaxRuleSpec{{Name: "vat-output", Kind: "bogus"}},
	}); !errors.Is(err, ErrInvalidTaxReportOptions) {
		t.Errorf("bad kind err = %v, want ErrInvalidTaxReportOptions", err)
	}
	if _, err := l.TaxReport(ctx, TaxReportOptions{
		From: now.Add(-time.Hour), To: now,
		Rules: []TaxRuleSpec{{Name: "vat-output", Kind: TaxRuleOutput}, {Name: "vat-output", Kind: TaxRuleInput}},
	}); !errors.Is(err, ErrInvalidTaxReportOptions) {
		t.Errorf("duplicate rule err = %v, want ErrInvalidTaxReportOptions", err)
	}
	// No designated rules: empty report, not an error.
	empty, err := l.TaxReport(ctx, TaxReportOptions{From: now.Add(-time.Hour), To: now})
	if err != nil || len(empty.Lines) != 0 {
		t.Errorf("empty rules = lines %d err %v, want empty report", len(empty.Lines), err)
	}
}

func TestTaxReportSignature(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	l := taxTestLedger(t, WithAnchorKey(priv))
	ctx := context.Background()
	fundAccount(t, l, "merchant-a", 1000000, "")
	taxTestTransfer(t, l, "tx-1", "merchant-a", "customer-1", 100000)

	report, err := l.TaxReport(ctx, TaxReportOptions{
		From:             time.Now().Add(-time.Hour),
		To:               time.Now().Add(time.Hour),
		Rules:            []TaxRuleSpec{{Name: "vat-output", Kind: TaxRuleOutput}},
		IncludeSignature: true,
	})
	if err != nil {
		t.Fatalf("signed TaxReport: %v", err)
	}
	if report.KeyID == "" || report.Signature == "" {
		t.Fatalf("signed report missing key_id/signature")
	}
	if !VerifyTaxReportSignature(report, pub) {
		t.Errorf("signature does not verify")
	}
	// Tampering breaks verification.
	tampered := *report
	tampered.TotalOutputCents++
	if VerifyTaxReportSignature(&tampered, pub) {
		t.Errorf("tampered report verifies")
	}
	// A wrong key does not verify.
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	if VerifyTaxReportSignature(report, otherPub) {
		t.Errorf("wrong-key report verifies")
	}
}

func TestTaxReportSignatureRequiresKey(t *testing.T) {
	l := taxTestLedger(t)
	ctx := context.Background()
	_, err := l.TaxReport(ctx, TaxReportOptions{
		From:             time.Now().Add(-time.Hour),
		To:               time.Now().Add(time.Hour),
		Rules:            []TaxRuleSpec{{Name: "vat-output", Kind: TaxRuleOutput}},
		IncludeSignature: true,
	})
	if !errors.Is(err, ErrAnchorKeyNotConfigured) {
		t.Errorf("unsigned-key report err = %v, want ErrAnchorKeyNotConfigured", err)
	}
}

func TestTaxReportCSV(t *testing.T) {
	l := taxTestLedger(t)
	ctx := context.Background()
	fundAccount(t, l, "merchant-a", 1000000, "")
	taxTestTransfer(t, l, "tx-1", "merchant-a", "customer-1", 100000)

	report, err := l.TaxReport(ctx, TaxReportOptions{
		From:  time.Now().Add(-time.Hour),
		To:    time.Now().Add(time.Hour),
		Rules: []TaxRuleSpec{{Name: "vat-output", Kind: TaxRuleOutput}},
	})
	if err != nil {
		t.Fatalf("TaxReport: %v", err)
	}
	var buf bytes.Buffer
	if err := report.WriteCSV(&buf); err != nil {
		t.Fatalf("WriteCSV: %v", err)
	}
	out := buf.String()
	if !strings.HasPrefix(out, "rule,kind,currency,cents,entry_count,rate_bps\n") {
		t.Errorf("csv header = %q", out)
	}
	if !strings.Contains(out, "vat-output,output,USD,19000,1,1900") {
		t.Errorf("csv missing data row:\n%s", out)
	}
	var jbuf bytes.Buffer
	if err := report.WriteJSON(&jbuf); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	if !strings.Contains(jbuf.String(), `"total_output_cents":19000`) {
		t.Errorf("json missing total:\n%s", jbuf.String())
	}
}
