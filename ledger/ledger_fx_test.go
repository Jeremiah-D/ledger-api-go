package ledger

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

func mustSetRate(t *testing.T, l *Ledger, from, to string, num, den int64) {
	t.Helper()
	if err := l.SetFXRate(from, to, num, den); err != nil {
		t.Fatalf("SetFXRate(%s,%s,%d/%d): %v", from, to, num, den, err)
	}
}

func TestFXRateValidation(t *testing.T) {
	l := New()
	for _, tc := range []struct {
		name      string
		from, to  string
		num, den  int64
	}{
		{"bad from code", "us", "EUR", 1, 1},
		{"bad to code", "USD", "EURO", 1, 1},
		{"empty from", "", "EUR", 1, 1},
		{"same currency", "USD", "USD", 1, 1},
		{"zero num", "USD", "EUR", 0, 1},
		{"zero den", "USD", "EUR", 1, 0},
		{"negative num", "USD", "EUR", -1, 1},
	} {
		if err := l.SetFXRate(tc.from, tc.to, tc.num, tc.den); !errors.Is(err, ErrInvalidFXRate) {
			t.Errorf("%s: want ErrInvalidFXRate, got %v", tc.name, err)
		}
	}
	// A rejected rate is not installed.
	if _, ok := l.FXRate("USD", "EUR"); ok {
		t.Error("rejected rate must not be installed")
	}
}

func TestParseFXRates(t *testing.T) {
	rates, err := ParseFXRates("USD:EUR=108/100,USD:CNY=720/100")
	if err != nil {
		t.Fatalf("ParseFXRates: %v", err)
	}
	if len(rates) != 2 || rates[0].Num != 108 || rates[0].Den != 100 || rates[1].ToCurrency != "CNY" {
		t.Fatalf("unexpected parsed rates: %+v", rates)
	}
	for _, raw := range []string{
		"",
		"USD:EUR=108",          // missing den
		"USDEUR=108:100",       // missing colon
		"USD:EUR:108:100",      // missing =
		"USD:EUR=0:100",        // zero num
		"USD:EUR=108:0",        // zero den
		"USD:USD=108:100",      // same currency
		"usd:EUR=108:100",      // lowercase
		"USD:EUR=108/100,USD:EUR=109/100", // duplicate pair
	} {
		if _, err := ParseFXRates(raw); err == nil {
			t.Errorf("ParseFXRates(%q): want error, got nil", raw)
		}
	}
}

func TestFXConvertCents(t *testing.T) {
	if got, ok := fxConvertCents(10000, 108, 100); !ok || got != 10800 {
		t.Errorf("10000 @108/100 = %d,%v; want 10800,true", got, ok)
	}
	// Flooring: 1 cent @ 1/3 floors to 0.
	if got, ok := fxConvertCents(1, 1, 3); !ok || got != 0 {
		t.Errorf("1 @1/3 = %d,%v; want 0,true", got, ok)
	}
	// Exact 128-bit math: MaxInt64 * 2 / 2 must not overflow.
	if got, ok := fxConvertCents(math.MaxInt64, 2, 2); !ok || got != math.MaxInt64 {
		t.Errorf("MaxInt64 @2/2 = %d,%v; want MaxInt64,true", got, ok)
	}
	// True overflow: MaxInt64 * 2 / 1 does not fit.
	if _, ok := fxConvertCents(math.MaxInt64, 2, 1); ok {
		t.Error("MaxInt64 @2/1: want overflow=false")
	}
	// Huge ratio that would overflow naive float64 multiplication: exact.
	if got, ok := fxConvertCents(1_000_000_000, 1_000_000_000, 1); !ok || got != 1_000_000_000_000_000_000 {
		t.Errorf("1e9 @1e9/1 = %d,%v", got, ok)
	}
	if _, ok := fxConvertCents(1_000_000_000, 1_000_000_000, 0); ok {
		t.Error("den 0: want false")
	}
}

func TestFXTransferHappyPath(t *testing.T) {
	l := New(WithFXAccount("fx-pnl"))

	// Seed the payer with USD.
	if _, _, err := l.Post(JournalEntry{ID: "seed", DebitAccount: "payer", CreditAccount: "bank", AmountCents: 100000, Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	mustSetRate(t, l, "USD", "EUR", 108, 100) // 1 USD = 1.08 EUR, takes effect at version 1

	rcpt, err := l.PostTransfer(Transfer{
		ID: "fx1", From: "payer", To: "payee", AmountCents: 10000,
		Currency: "USD", ToCurrency: "EUR",
	})
	if err != nil {
		t.Fatalf("PostTransfer FX: %v", err)
	}
	if rcpt.Duplicate {
		t.Error("first post must not be a duplicate")
	}
	if rcpt.FX == nil {
		t.Fatal("receipt must disclose FX conversion")
	}
	fx := rcpt.FX
	if fx.FromCurrency != "USD" || fx.ToCurrency != "EUR" || fx.ConvertedCents != 10800 ||
		fx.RateNum != 108 || fx.RateDen != 100 {
		t.Errorf("bad FX disclosure: %+v", fx)
	}
	if fx.EffectiveVersion != 1 { // rate set when version was 1 (after seed post)
		t.Errorf("EffectiveVersion = %d, want 1", fx.EffectiveVersion)
	}

	// Two journal entries, each single-currency, legs as designed.
	if len(rcpt.Entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(rcpt.Entries))
	}
	principal, fxLeg := rcpt.Entries[0], rcpt.Entries[1]
	if principal.ID != "fx1" || principal.DebitAccount != "payee" || principal.CreditAccount != "fx-pnl" ||
		principal.AmountCents != 10800 || principal.Currency != "EUR" {
		t.Errorf("bad principal leg: %+v", principal)
	}
	if fxLeg.ID != "fx1/fx" || fxLeg.DebitAccount != "fx-pnl" || fxLeg.CreditAccount != "payer" ||
		fxLeg.AmountCents != 10000 || fxLeg.Currency != "USD" {
		t.Errorf("bad FX leg: %+v", fxLeg)
	}

	// Balances: payee +10800 EUR, payer -10000 USD, fx-pnl nets both.
	if got := l.BalanceIn("payee", "EUR"); got != 10800 {
		t.Errorf("payee EUR = %d, want 10800", got)
	}
	if got := l.BalanceIn("payer", "USD"); got != 90000 {
		t.Errorf("payer USD = %d, want 90000", got)
	}
	if got := l.BalanceIn("fx-pnl", "USD"); got != 10000 {
		t.Errorf("fx-pnl USD = %d, want 10000", got)
	}
	if got := l.BalanceIn("fx-pnl", "EUR"); got != -10800 {
		t.Errorf("fx-pnl EUR = %d, want -10800", got)
	}

	// The accounting equation still holds, per currency.
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Errorf("accounting equation broken by FX transfer: %v", err)
	}

	// Audit chain covers both legs.
	if err := l.VerifyChain(); err != nil {
		t.Errorf("chain broken: %v", err)
	}
}

func TestFXTransferMissingRate(t *testing.T) {
	l := New(WithFXAccount("fx-pnl"))
	v0 := l.Version()
	_, err := l.PostTransfer(Transfer{
		ID: "fx-miss", From: "p", To: "q", AmountCents: 100,
		Currency: "USD", ToCurrency: "JPY",
	})
	if !errors.Is(err, ErrFXRateMissing) {
		t.Fatalf("want ErrFXRateMissing, got %v", err)
	}
	// Rejected transfers record nothing.
	if l.Version() != v0 {
		t.Error("rejected FX transfer bumped the version")
	}
	if len(l.Entries()) != 0 {
		t.Error("rejected FX transfer journaled entries")
	}
}

func TestFXAccountConfig(t *testing.T) {
	l := New()
	mustSetRate(t, l, "USD", "EUR", 108, 100)
	// No ledger FX account and no per-transfer override.
	if _, err := l.PostTransfer(Transfer{ID: "a", From: "p", To: "q", AmountCents: 100, Currency: "USD", ToCurrency: "EUR"}); !errors.Is(err, ErrFXAccountNotConfigured) {
		t.Errorf("want ErrFXAccountNotConfigured, got %v", err)
	}
	// Per-transfer override works.
	rcpt, err := l.PostTransfer(Transfer{ID: "b", From: "p", To: "q", AmountCents: 100, Currency: "USD", ToCurrency: "EUR", FXAccount: "my-fx"})
	if err != nil {
		t.Fatalf("per-transfer FXAccount: %v", err)
	}
	if rcpt.Entries[1].DebitAccount != "my-fx" {
		t.Errorf("FX leg used %q, want my-fx", rcpt.Entries[1].DebitAccount)
	}
	// FX account equal to the payer or payee is rejected: it would
	// debit and credit the same account.
	for _, fx := range []AccountID{"p", "q"} {
		if _, err := l.PostTransfer(Transfer{ID: "c" + string(fx), From: "p", To: "q", AmountCents: 100, Currency: "USD", ToCurrency: "EUR", FXAccount: fx}); !errors.Is(err, ErrInvalidFXAccount) {
			t.Errorf("FXAccount=%q: want ErrInvalidFXAccount, got %v", fx, err)
		}
	}
}

func TestFXTransferFrozenFXAccount(t *testing.T) {
	l := New(WithFXAccount("fx-pnl"))
	mustSetRate(t, l, "USD", "EUR", 108, 100)
	l.Freeze("fx-pnl")
	_, err := l.PostTransfer(Transfer{ID: "f", From: "p", To: "q", AmountCents: 100, Currency: "USD", ToCurrency: "EUR"})
	if !errors.Is(err, ErrAccountFrozen) {
		t.Fatalf("frozen FX account: want ErrAccountFrozen, got %v", err)
	}
}

func TestFXTransferOverdraftAndDailyLimitInSourceCurrency(t *testing.T) {
	l := New(WithFXAccount("fx-pnl"), WithOverdraftProtection("payer"))
	mustSetRate(t, l, "USD", "EUR", 108, 100)
	if err := l.SetDailyLimit("payer", "USD", 5000); err != nil {
		t.Fatal(err)
	}
	// Payer holds 1000 USD: an overdraft-protected payer cannot fund
	// 1000 USD + 100 USD fee.
	if _, _, err := l.Post(JournalEntry{ID: "seed", DebitAccount: "payer", CreditAccount: "bank", AmountCents: 1000, Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	_, err := l.PostTransfer(Transfer{ID: "od", From: "payer", To: "q", AmountCents: 1000, FeeCents: 100, FeeAccount: "fees", Currency: "USD", ToCurrency: "EUR"})
	if !errors.Is(err, ErrAccountOverdraft) {
		t.Errorf("want ErrAccountOverdraft, got %v", err)
	}
	// EUR balance cannot cover a USD outflow: payee EUR is irrelevant.
	l2 := New(WithFXAccount("fx-pnl"))
	mustSetRate(t, l2, "USD", "EUR", 108, 100)
	l2.EnableOverdraftProtection("payer")
	if _, _, err := l2.Post(JournalEntry{ID: "s1", DebitAccount: "payee", CreditAccount: "bank", AmountCents: 999999, Currency: "EUR"}); err != nil {
		t.Fatal(err)
	}
	if _, err := l2.PostTransfer(Transfer{ID: "od2", From: "payer", To: "q", AmountCents: 100, Currency: "USD", ToCurrency: "EUR"}); !errors.Is(err, ErrAccountOverdraft) {
		t.Errorf("want ErrAccountOverdraft (zero USD payer), got %v", err)
	}
	// Daily limit: 6000 USD outflow on a 5000 limit rejects.
	l3 := New(WithFXAccount("fx-pnl"))
	mustSetRate(t, l3, "USD", "EUR", 108, 100)
	if err := l3.SetDailyLimit("payer", "USD", 5000); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l3.Post(JournalEntry{ID: "s2", DebitAccount: "payer", CreditAccount: "bank", AmountCents: 100000, Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	if _, err := l3.PostTransfer(Transfer{ID: "dl", From: "payer", To: "q", AmountCents: 6000, Currency: "USD", ToCurrency: "EUR"}); !errors.Is(err, ErrDailyLimitExceeded) {
		t.Errorf("want ErrDailyLimitExceeded, got %v", err)
	}
}

func TestFXTransferIdempotentReplay(t *testing.T) {
	l := New(WithFXAccount("fx-pnl"), WithTransferFeePolicy(250, "fee-rev"))
	mustSetRate(t, l, "USD", "EUR", 108, 100)
	if _, _, err := l.Post(JournalEntry{ID: "seed", DebitAccount: "payer", CreditAccount: "bank", AmountCents: 1000000, Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	orig, err := l.PostTransfer(Transfer{
		ID: "fxr", From: "payer", To: "payee", AmountCents: 10000,
		Currency: "USD", ToCurrency: "EUR", IdempotencyKey: "k1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(orig.Entries) != 3 { // principal + fx leg + fee leg (2.5% of 10000 = 250)
		t.Fatalf("want 3 entries, got %d", len(orig.Entries))
	}
	if orig.FeeCents != 250 {
		t.Errorf("fee = %d, want 250", orig.FeeCents)
	}
	// The fee leg is in the source currency, charged on top.
	fee := orig.Entries[2]
	if fee.ID != "fxr/fee" || fee.Currency != "USD" || fee.AmountCents != 250 || fee.DebitAccount != "fee-rev" || fee.CreditAccount != "payer" {
		t.Errorf("bad fee leg: %+v", fee)
	}
	if got := l.BalanceIn("payer", "USD"); got != 1000000-10000-250 {
		t.Errorf("payer USD = %d", got)
	}

	v := l.Version()
	replay, err := l.PostTransfer(Transfer{
		ID: "fxr2", From: "payer", To: "payee", AmountCents: 10000,
		Currency: "USD", ToCurrency: "EUR", IdempotencyKey: "k1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Duplicate {
		t.Error("second post with same key must be a duplicate")
	}
	if l.Version() != v {
		t.Error("replay bumped the version")
	}
	if len(replay.Entries) != 3 || replay.FeeCents != 250 {
		t.Errorf("replay receipt incomplete: %d entries, fee %d", len(replay.Entries), replay.FeeCents)
	}
	if replay.FX != nil {
		t.Error("replay receipt must omit the FX disclosure")
	}
	// Tier re-derivation must use the source-currency amount (the FX
	// leg), not the converted principal: with 250 bps policy on 10000
	// USD the tier index is 0.
	if replay.FeeTierIndex != orig.FeeTierIndex || replay.FeeRateBps != 250 {
		t.Errorf("replay tier disclosure = (%d,%d), want (%d,250)",
			replay.FeeTierIndex, replay.FeeRateBps, orig.FeeTierIndex)
	}

	// Replay works even after the rate is removed: the replay books
	// nothing new, so the missing rate must not fail it.
	if _, err := l.RemoveFXRate("USD", "EUR"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.PostTransfer(Transfer{
		ID: "fxr3", From: "payer", To: "payee", AmountCents: 10000,
		Currency: "USD", ToCurrency: "EUR", IdempotencyKey: "k1",
	}); err != nil {
		t.Errorf("replay after rate removal: %v", err)
	}
}

func TestFXRatesInReconcileAndSnapshot(t *testing.T) {
	l := New(WithFXAccount("fx-pnl"))
	if _, _, err := l.Post(JournalEntry{ID: "seed", DebitAccount: "payer", CreditAccount: "bank", AmountCents: 100000, Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	mustSetRate(t, l, "USD", "EUR", 108, 100)
	mustSetRate(t, l, "EUR", "USD", 100, 108)
	if _, err := l.PostTransfer(Transfer{ID: "fx1", From: "payer", To: "payee", AmountCents: 10000, Currency: "USD", ToCurrency: "EUR"}); err != nil {
		t.Fatal(err)
	}

	rep := l.Reconcile(time.Now())
	if len(rep.FXRates) != 2 {
		t.Fatalf("reconcile FXRates = %d, want 2", len(rep.FXRates))
	}
	if rep.FXRates[0].FromCurrency != "EUR" || rep.FXRates[1].FromCurrency != "USD" {
		t.Errorf("rates not sorted: %+v", rep.FXRates)
	}
	if rep.FXRates[1].EffectiveVersion != 1 {
		t.Errorf("EffectiveVersion = %d, want 1", rep.FXRates[1].EffectiveVersion)
	}

	// Snapshot round-trip preserves rates, the FX account, and the FX
	// journal entries (chain-verified).
	var buf strings.Builder
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatal(err)
	}
	l2, err := ImportSnapshot(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if !snapshotLedgersEqual(l, l2) {
		t.Error("restored ledger differs from original")
	}
	if r, ok := l2.FXRate("USD", "EUR"); !ok || r.Num != 108 || r.EffectiveVersion != 1 {
		t.Errorf("restored rate: %+v, %v", r, ok)
	}
	if err := l2.VerifyAccountingEquation(); err != nil {
		t.Errorf("restored books do not balance: %v", err)
	}
	// The restored ledger converts with the restored rate.
	rcpt, err := l2.PostTransfer(Transfer{ID: "fx2", From: "payer", To: "payee2", AmountCents: 5000, Currency: "USD", ToCurrency: "EUR"})
	if err != nil {
		t.Fatalf("FX on restored ledger: %v", err)
	}
	if rcpt.Entries[0].AmountCents != 5400 {
		t.Errorf("converted = %d, want 5400", rcpt.Entries[0].AmountCents)
	}
}

func TestFXTransferInvalidTargetCurrency(t *testing.T) {
	l := New(WithFXAccount("fx-pnl"))
	mustSetRate(t, l, "USD", "EUR", 108, 100)
	if _, err := l.PostTransfer(Transfer{ID: "x", From: "p", To: "q", AmountCents: 100, Currency: "USD", ToCurrency: "usd"}); !errors.Is(err, ErrInvalidCurrency) {
		t.Errorf("want ErrInvalidCurrency, got %v", err)
	}
	// ToCurrency == Currency is the plain path: no rate needed, one entry.
	if _, _, err := l.Post(JournalEntry{ID: "seed", DebitAccount: "p", CreditAccount: "bank", AmountCents: 1000}); err != nil {
		t.Fatal(err)
	}
	rcpt, err := l.PostTransfer(Transfer{ID: "y", From: "p", To: "q", AmountCents: 100, Currency: "USD", ToCurrency: "USD"})
	if err != nil {
		t.Fatalf("same-currency ToCurrency: %v", err)
	}
	if len(rcpt.Entries) != 1 || rcpt.FX != nil {
		t.Errorf("same-currency transfer must stay single-entry without FX disclosure: %+v", rcpt)
	}
}

func (l *Ledger) Version() uint64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.version
}
