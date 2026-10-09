package ledger

import (
	"bytes"
	"errors"
	"math/big"
	"testing"
	"time"
)

func mustParseDecimalRate(t *testing.T, s string) *big.Rat {
	t.Helper()
	r, err := ParseFXRateDecimal(s)
	if err != nil {
		t.Fatalf("ParseFXRateDecimal(%q): %v", s, err)
	}
	return r
}

func TestParseFXRateDecimal(t *testing.T) {
	for _, tc := range []struct {
		in      string
		wantNum string
		wantDen string
	}{
		{"1.10", "11", "10"},
		{"7.2015", "14403", "2000"},
		{"0.85", "17", "20"},
		{"2", "2", "1"},
		{"  1.5  ", "3", "2"}, // surrounding whitespace is trimmed
	} {
		r, err := ParseFXRateDecimal(tc.in)
		if err != nil {
			t.Errorf("ParseFXRateDecimal(%q): %v", tc.in, err)
			continue
		}
		if r.Num().String() != tc.wantNum || r.Denom().String() != tc.wantDen {
			t.Errorf("ParseFXRateDecimal(%q) = %s/%s, want %s/%s",
				tc.in, r.Num(), r.Denom(), tc.wantNum, tc.wantDen)
		}
	}
	for _, in := range []string{"", "  ", "abc", "1.2.3", "0", "-1.5", "0.00"} {
		if _, err := ParseFXRateDecimal(in); err == nil {
			t.Errorf("ParseFXRateDecimal(%q): want error, got nil", in)
		} else if !errors.Is(err, ErrInvalidFXRate) {
			t.Errorf("ParseFXRateDecimal(%q): err = %v, want ErrInvalidFXRate", in, err)
		}
	}
}

func TestSetFXRateRatValidation(t *testing.T) {
	l := New()
	huge := new(big.Rat).SetInt(new(big.Int).Lsh(big.NewInt(1), 100)) // 2^100: num does not fit int64
	for _, tc := range []struct {
		name     string
		from, to string
		rate     *big.Rat
		ttl      time.Duration
	}{
		{"nil rate", "USD", "EUR", nil, 0},
		{"zero rate", "USD", "EUR", new(big.Rat), 0},
		{"negative rate", "USD", "EUR", big.NewRat(-3, 2), 0},
		{"same currency", "USD", "USD", big.NewRat(1, 1), 0},
		{"bad from code", "us", "EUR", big.NewRat(1, 1), 0},
		{"empty from", "", "EUR", big.NewRat(1, 1), 0},
		{"num overflows int64", "USD", "EUR", huge, 0},
		{"negative ttl", "USD", "EUR", big.NewRat(1, 1), -time.Second},
	} {
		if err := l.SetFXRateRat(tc.from, tc.to, tc.rate, tc.ttl); !errors.Is(err, ErrInvalidFXRate) {
			t.Errorf("%s: want ErrInvalidFXRate, got %v", tc.name, err)
		}
	}
	if _, ok := l.FXRate("USD", "EUR"); ok {
		t.Error("rejected rate must not be installed")
	}
}

func TestSetFXRateRatExactConversion(t *testing.T) {
	l := New(WithFXAccount("fx-pnl"))
	// 1.10 as a decimal is exactly 11/10 — the point of the big.Rat path:
	// no float64 rounding anywhere in the pipeline.
	if err := l.SetFXRateRat("USD", "EUR", mustParseDecimalRate(t, "1.10"), 0); err != nil {
		t.Fatalf("SetFXRateRat: %v", err)
	}
	rate, ok := l.FXRate("USD", "EUR")
	if !ok {
		t.Fatal("rate not found")
	}
	if rate.Num != 11 || rate.Den != 10 {
		t.Fatalf("stored rate = %d/%d, want 11/10", rate.Num, rate.Den)
	}
	if !rate.ExpiresAt.IsZero() {
		t.Errorf("ExpiresAt = %v, want zero (no ttl)", rate.ExpiresAt)
	}

	if _, _, err := l.Post(JournalEntry{ID: "seed", DebitAccount: "payer", CreditAccount: "bank", AmountCents: 100000, Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	rcpt, err := l.PostTransfer(Transfer{
		ID: "fx1", From: "payer", To: "payee", AmountCents: 10000,
		Currency: "USD", ToCurrency: "EUR",
	})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if rcpt.FX == nil {
		t.Fatal("receipt must disclose FX conversion")
	}
	// Before/after disclosure on the receipt: 10000 USD in, 11000 EUR out.
	if rcpt.FX.SourceCents != 10000 {
		t.Errorf("FX.SourceCents = %d, want 10000", rcpt.FX.SourceCents)
	}
	if rcpt.FX.ConvertedCents != 11000 {
		t.Errorf("FX.ConvertedCents = %d, want 11000", rcpt.FX.ConvertedCents)
	}
	if rcpt.FX.RateNum != 11 || rcpt.FX.RateDen != 10 {
		t.Errorf("FX rate = %d/%d, want 11/10", rcpt.FX.RateNum, rcpt.FX.RateDen)
	}
}

func TestFXRateExpiry(t *testing.T) {
	l := New(WithFXAccount("fx-pnl"))
	if err := l.SetFXRateRat("USD", "EUR", mustParseDecimalRate(t, "1.10"), 60*time.Millisecond); err != nil {
		t.Fatalf("SetFXRateRat: %v", err)
	}
	rate, _ := l.FXRate("USD", "EUR")
	if rate.ExpiresAt.IsZero() {
		t.Fatal("ExpiresAt not set for ttl rate")
	}

	if _, _, err := l.Post(JournalEntry{ID: "seed", DebitAccount: "payer", CreditAccount: "bank", AmountCents: 100000, Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	// While the rate is live the transfer converts.
	if _, err := l.PostTransfer(Transfer{ID: "fx-live", From: "payer", To: "payee", AmountCents: 1000, Currency: "USD", ToCurrency: "EUR"}); err != nil {
		t.Fatalf("live-rate transfer: %v", err)
	}
	v1 := l.Version()

	time.Sleep(120 * time.Millisecond)
	// After expiry the transfer is rejected — never converted at the
	// stale rate — and records nothing.
	_, err := l.PostTransfer(Transfer{ID: "fx-stale", From: "payer", To: "payee", AmountCents: 1000, Currency: "USD", ToCurrency: "EUR"})
	if !errors.Is(err, ErrFXRateExpired) {
		t.Fatalf("expired-rate transfer err = %v, want ErrFXRateExpired", err)
	}
	if v := l.Version(); v != v1 {
		t.Errorf("version = %d, want %d (rejection records nothing)", v, v1)
	}
	if _, ok := l.entries["fx-stale"]; ok {
		t.Error("rejected transfer journaled an entry")
	}
	if _, ok := l.entries["fx-stale/fx"]; ok {
		t.Error("rejected transfer journaled an FX leg")
	}
	if got := l.BalanceIn("payee", "EUR"); got != 1100 {
		t.Errorf("payee EUR = %d, want 1100 (only the live transfer)", got)
	}
	// Installing a fresh rate unblocks the pair.
	if err := l.SetFXRate("USD", "EUR", 11, 10); err != nil {
		t.Fatalf("SetFXRate: %v", err)
	}
	if _, err := l.PostTransfer(Transfer{ID: "fx-fresh", From: "payer", To: "payee", AmountCents: 1000, Currency: "USD", ToCurrency: "EUR"}); err != nil {
		t.Fatalf("fresh-rate transfer: %v", err)
	}
}

func TestFXRateExpiryReplayStillWorks(t *testing.T) {
	l := New(WithFXAccount("fx-pnl"))
	if err := l.SetFXRateRat("USD", "EUR", mustParseDecimalRate(t, "1.10"), 60*time.Millisecond); err != nil {
		t.Fatalf("SetFXRateRat: %v", err)
	}
	if _, _, err := l.Post(JournalEntry{ID: "seed", DebitAccount: "payer", CreditAccount: "bank", AmountCents: 100000, Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	r1, err := l.PostTransfer(Transfer{
		ID: "fx1", From: "payer", To: "payee", AmountCents: 1000,
		Currency: "USD", ToCurrency: "EUR", IdempotencyKey: "fx-key",
	})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	time.Sleep(120 * time.Millisecond)

	// The replay check runs before the rate lookup: replaying a key
	// posted while the rate was live returns the original receipt even
	// though the rate has since expired — the replay books nothing new.
	r2, err := l.PostTransfer(Transfer{
		ID: "fx1-retry", From: "payer", To: "payee", AmountCents: 1000,
		Currency: "USD", ToCurrency: "EUR", IdempotencyKey: "fx-key",
	})
	if err != nil {
		t.Fatalf("replay after expiry: %v", err)
	}
	if !r2.Duplicate {
		t.Error("replay: Duplicate = false, want true")
	}
	if len(r2.Entries) != len(r1.Entries) {
		t.Errorf("replay entries = %d, want %d", len(r2.Entries), len(r1.Entries))
	}
}

func TestFXReceiptCarriesRateExpiry(t *testing.T) {
	l := New(WithFXAccount("fx-pnl"))
	before := time.Now()
	if err := l.SetFXRateRat("USD", "EUR", mustParseDecimalRate(t, "2"), time.Hour); err != nil {
		t.Fatalf("SetFXRateRat: %v", err)
	}
	if _, _, err := l.Post(JournalEntry{ID: "seed", DebitAccount: "payer", CreditAccount: "bank", AmountCents: 100000, Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	rcpt, err := l.PostTransfer(Transfer{ID: "fx1", From: "payer", To: "payee", AmountCents: 1000, Currency: "USD", ToCurrency: "EUR"})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if rcpt.FX.RateExpiresAt.Before(before.Add(time.Hour)) || rcpt.FX.RateExpiresAt.After(time.Now().Add(time.Hour)) {
		t.Errorf("FX.RateExpiresAt = %v, want ~now+1h", rcpt.FX.RateExpiresAt)
	}
}

func TestFXConversionAuditEventCarriesProvenance(t *testing.T) {
	al := mustAuditLog(t)
	defer al.Close()
	l := New(WithFXAccount("fx-pnl"), WithAuditLog(al))
	if err := l.SetFXRateRat("USD", "EUR", mustParseDecimalRate(t, "1.10"), 0); err != nil {
		t.Fatalf("SetFXRateRat: %v", err)
	}
	if _, _, err := l.Post(JournalEntry{ID: "seed", DebitAccount: "payer", CreditAccount: "bank", AmountCents: 100000, Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.PostTransfer(Transfer{ID: "fx1", From: "payer", To: "payee", AmountCents: 10000, Currency: "USD", ToCurrency: "EUR"}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, _, err := ReadAuditLog(al.Dir(), time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	var fx *AuditEvent
	for i := range events {
		if events[i].Op == "transfer" && events[i].TraceID == "fx1" {
			fx = &events[i]
		}
	}
	if fx == nil {
		t.Fatal("no transfer audit event for fx1")
	}
	// Before/after amounts and the exact rate, sealed by the audit log's
	// hash chain: the conversion's full provenance.
	for k, want := range map[string]float64{
		"source_cents":    10000,
		"converted_cents": 11000,
		"rate_num":        11,
		"rate_den":        10,
	} {
		if got := fx.Details[k]; got != want {
			t.Errorf("details[%q] = %v, want %v", k, got, want)
		}
	}
	if _, ok := fx.Details["rate_effective_version"]; !ok {
		t.Error("details missing rate_effective_version")
	}
}

func TestFXRateExpirySnapshotRoundTrip(t *testing.T) {
	l := New(WithFXAccount("fx-pnl"))
	if err := l.SetFXRateRat("USD", "EUR", mustParseDecimalRate(t, "1.10"), time.Hour); err != nil {
		t.Fatalf("SetFXRateRat: %v", err)
	}
	want, _ := l.FXRate("USD", "EUR")

	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	l2, err := ImportSnapshot(&buf)
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	got, ok := l2.FXRate("USD", "EUR")
	if !ok {
		t.Fatal("rate missing after restore")
	}
	if got.Num != want.Num || got.Den != want.Den {
		t.Errorf("restored rate = %d/%d, want %d/%d", got.Num, got.Den, want.Num, want.Den)
	}
	if got.ExpiresAt.UnixNano() != want.ExpiresAt.UnixNano() {
		t.Errorf("restored ExpiresAt = %v, want %v", got.ExpiresAt, want.ExpiresAt)
	}
	if got.EffectiveVersion != want.EffectiveVersion {
		t.Errorf("restored EffectiveVersion = %d, want %d", got.EffectiveVersion, want.EffectiveVersion)
	}
}
