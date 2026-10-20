package ledger

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

// seedSweepFXLedger builds a ledger with an FX clearing account and two
// source accounts: sub-a holds USD, sub-b holds EUR.
func seedSweepFXLedger(t *testing.T) *Ledger {
	t.Helper()
	l := New(
		WithFXAccount("fx-clearing"),
		WithFXRateOption("USD", "EUR", 108, 100), // 1 USD = 1.08 EUR
		WithFXRateOption("EUR", "USD", 92, 100),  // 1 EUR = 0.92 USD
	)
	seedBalanceIn(t, l, "sub-a", 10000, "USD")
	seedBalanceIn(t, l, "sub-b", 5000, "EUR")
	return l
}

func TestPostSweepFXHappyPath(t *testing.T) {
	l := seedSweepFXLedger(t)

	receipt, err := l.PostSweepFX(SweepFX{
		ID:   "fxsweep-1",
		From: []AccountID{"sub-a", "sub-b"},
		To:   "treasury",
		// FXAccount empty: the ledger-wide account applies.
		ToCurrency: "EUR",
	})
	if err != nil {
		t.Fatalf("PostSweepFX: %v", err)
	}
	if receipt.Duplicate {
		t.Error("receipt.Duplicate = true, want false")
	}
	if receipt.ToCurrency != "EUR" || receipt.FXAccount != "fx-clearing" {
		t.Errorf("receipt to/fx = %q/%q, want EUR/fx-clearing", receipt.ToCurrency, receipt.FXAccount)
	}
	// Two legs: sub-a/USD converted, sub-b/EUR same-currency.
	if len(receipt.Legs) != 2 || len(receipt.FX) != 2 {
		t.Fatalf("legs = %d, fx = %d, want 2 and 2", len(receipt.Legs), len(receipt.FX))
	}
	usd := receipt.Legs[0]
	if usd.From != "sub-a" || usd.SourceCurrency != "USD" || usd.AmountCents != 10000 {
		t.Errorf("leg 0 = %+v, want sub-a/USD/10000", usd)
	}
	if usd.PrincipalEntryID != "fxsweep-1/sub-a/USD" || usd.FXEntryID != "fxsweep-1/sub-a/USD/fx" {
		t.Errorf("leg 0 entry IDs = %q/%q", usd.PrincipalEntryID, usd.FXEntryID)
	}
	// floor(10000 * 108 / 100) = 10800 EUR.
	if usd.ConvertedCents != 10800 {
		t.Errorf("leg 0 converted = %d, want 10800", usd.ConvertedCents)
	}
	fx := receipt.FX[0]
	if fx.FromCurrency != "USD" || fx.ToCurrency != "EUR" || fx.SourceCents != 10000 ||
		fx.ConvertedCents != 10800 || fx.RateNum != 108 || fx.RateDen != 100 ||
		fx.Direction != FXSideSell || fx.RateSnapshotVersion == 0 {
		t.Errorf("fx disclosure leg 0 = %+v", fx)
	}
	eur := receipt.Legs[1]
	if eur.From != "sub-b" || eur.SourceCurrency != "EUR" || eur.AmountCents != 5000 ||
		eur.ConvertedCents != 5000 || eur.FXEntryID != "" {
		t.Errorf("leg 1 = %+v, want same-currency sub-b/EUR/5000", eur)
	}
	if receipt.FX[1].Direction != sweepFXSameCurrency || receipt.FX[1].RateNum != 1 {
		t.Errorf("fx disclosure leg 1 = %+v, want same_currency identity", receipt.FX[1])
	}
	// Four journal rows: principal + fx leg, then the same-currency leg.
	if len(receipt.Entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(receipt.Entries))
	}
	// Sources are zeroed, the target collected the conversions.
	if got := l.BalanceIn("sub-a", "USD"); got != 0 {
		t.Errorf("BalanceIn(sub-a, USD) = %d, want 0", got)
	}
	if got := l.BalanceIn("sub-b", "EUR"); got != 0 {
		t.Errorf("BalanceIn(sub-b, EUR) = %d, want 0", got)
	}
	if got := l.BalanceIn("treasury", "EUR"); got != 15800 {
		t.Errorf("BalanceIn(treasury, EUR) = %d, want 15800", got)
	}
	// The FX clearing account is the counterparty of both legs: it sold
	// EUR (credit) and bought USD (debit), like the transfer-FX path.
	if got := l.BalanceIn("fx-clearing", "USD"); got != 10000 {
		t.Errorf("BalanceIn(fx-clearing, USD) = %d, want 10000", got)
	}
	if got := l.BalanceIn("fx-clearing", "EUR"); got != -10800 {
		t.Errorf("BalanceIn(fx-clearing, EUR) = %d, want -10800", got)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Errorf("VerifyAccountingEquation: %v", err)
	}
	if err := l.VerifyChain(); err != nil {
		t.Errorf("VerifyChain: %v", err)
	}
}

func TestPostSweepFXMissingRate(t *testing.T) {
	l := New(WithFXAccount("fx-clearing")) // no rates at all
	seedBalanceIn(t, l, "sub-a", 10000, "USD")
	versionBefore := l.Version()

	_, err := l.PostSweepFX(SweepFX{ID: "fxsweep-x", From: []AccountID{"sub-a"}, To: "treasury", ToCurrency: "EUR"})
	if !errors.Is(err, ErrFXRateMissing) {
		t.Fatalf("PostSweepFX = %v, want ErrFXRateMissing", err)
	}
	// Nothing recorded: no journal rows, no chain links, no version bump.
	if l.Version() != versionBefore {
		t.Errorf("version = %d, want %d (unchanged)", l.Version(), versionBefore)
	}
	if got := l.BalanceIn("sub-a", "USD"); got != 10000 {
		t.Errorf("BalanceIn(sub-a, USD) = %d, want 10000", got)
	}
}

func TestPostSweepFXExpiredRate(t *testing.T) {
	l := New(WithFXAccount("fx-clearing"))
	rate, err := ParseFXRateDecimal("1.08")
	if err != nil {
		t.Fatalf("ParseFXRateDecimal: %v", err)
	}
	if err := l.SetFXRateRat("USD", "EUR", rate, time.Hour); err != nil {
		t.Fatalf("SetFXRateRat: %v", err)
	}
	seedBalanceIn(t, l, "sub-a", 10000, "USD")

	// The sweep's effective post time is two hours out: the rate expired
	// an hour ago.
	_, err = l.PostSweepFX(SweepFX{
		ID:         "fxsweep-exp",
		From:       []AccountID{"sub-a"},
		To:         "treasury",
		ToCurrency: "EUR",
		CreatedAt:  time.Now().Add(2 * time.Hour),
	})
	if !errors.Is(err, ErrFXRateExpired) {
		t.Fatalf("PostSweepFX = %v, want ErrFXRateExpired", err)
	}
	if got := l.BalanceIn("sub-a", "USD"); got != 10000 {
		t.Errorf("BalanceIn(sub-a, USD) = %d, want 10000", got)
	}
}

func TestPostSweepFXIdempotentReplay(t *testing.T) {
	l := seedSweepFXLedger(t)

	first, err := l.PostSweepFX(SweepFX{
		ID:             "fxsweep-r",
		From:           []AccountID{"sub-a"},
		To:             "treasury",
		ToCurrency:     "EUR",
		IdempotencyKey: "fxkey-1",
	})
	if err != nil {
		t.Fatalf("PostSweepFX: %v", err)
	}
	versionAfter := l.Version()

	// A replay with a different sweep ID returns the original receipt.
	replay, err := l.PostSweepFX(SweepFX{
		ID:             "fxsweep-r-retry",
		From:           []AccountID{"sub-b"},
		To:             "treasury",
		ToCurrency:     "EUR",
		IdempotencyKey: "fxkey-1",
	})
	if err != nil {
		t.Fatalf("replay PostSweepFX: %v", err)
	}
	if !replay.Duplicate {
		t.Error("replay.Duplicate = false, want true")
	}
	if replay.SweepID != "fxsweep-r" || len(replay.Legs) != 1 {
		t.Errorf("replay = %+v, want original sweep fxsweep-r with 1 leg", replay)
	}
	if replay.FX != nil {
		t.Errorf("replay.FX = %+v, want nil (transfer-FX convention)", replay.FX)
	}
	// Nothing booked twice: sub-b's EUR is untouched, version unchanged.
	if got := l.BalanceIn("sub-b", "EUR"); got != 5000 {
		t.Errorf("BalanceIn(sub-b, EUR) = %d, want 5000", got)
	}
	if l.Version() != versionAfter {
		t.Errorf("version = %d, want %d", l.Version(), versionAfter)
	}
	_ = first
}

func TestPostSweepFXFrozenSource(t *testing.T) {
	l := seedSweepFXLedger(t)
	l.Freeze("sub-a")
	versionBefore := l.Version()

	_, err := l.PostSweepFX(SweepFX{ID: "fxsweep-f", From: []AccountID{"sub-a"}, To: "treasury", ToCurrency: "EUR"})
	if !errors.Is(err, ErrAccountFrozen) {
		t.Fatalf("PostSweepFX = %v, want ErrAccountFrozen", err)
	}
	if l.Version() != versionBefore {
		t.Errorf("version = %d, want %d (unchanged)", l.Version(), versionBefore)
	}
}

func TestPostSweepFXAccountValidation(t *testing.T) {
	l := seedSweepFXLedger(t)

	// No FX account configured anywhere.
	bare := New()
	seedBalanceIn(t, bare, "sub-a", 10000, "USD")
	if _, err := bare.PostSweepFX(SweepFX{ID: "x", From: []AccountID{"sub-a"}, To: "treasury", ToCurrency: "EUR"}); !errors.Is(err, ErrFXAccountNotConfigured) {
		t.Errorf("no fx account: err = %v, want ErrFXAccountNotConfigured", err)
	}
	// FX account must differ from the target and the sources.
	for _, bad := range []struct {
		name string
		fx   AccountID
	}{
		{"target", "treasury"},
		{"source", "sub-a"},
	} {
		if _, err := l.PostSweepFX(SweepFX{ID: "x-" + bad.name, From: []AccountID{"sub-a"}, To: "treasury", ToCurrency: "EUR", FXAccount: bad.fx}); !errors.Is(err, ErrInvalidFXAccount) {
			t.Errorf("fx=%s: err = %v, want ErrInvalidFXAccount", bad.fx, err)
		}
	}
	// Explicit FX account override wins over the ledger-wide one.
	receipt, err := l.PostSweepFX(SweepFX{
		ID: "fxsweep-ovr", From: []AccountID{"sub-a"}, To: "treasury",
		ToCurrency: "EUR", FXAccount: "fx-alt",
	})
	if err != nil {
		t.Fatalf("PostSweepFX with override: %v", err)
	}
	if receipt.FXAccount != "fx-alt" {
		t.Errorf("receipt.FXAccount = %q, want fx-alt", receipt.FXAccount)
	}
	if got := l.BalanceIn("fx-alt", "USD"); got != 10000 {
		t.Errorf("BalanceIn(fx-alt, USD) = %d, want 10000", got)
	}
}

func TestPostSweepFXEmptyToCurrency(t *testing.T) {
	l := seedSweepFXLedger(t)
	if _, err := l.PostSweepFX(SweepFX{ID: "x", From: []AccountID{"sub-a"}, To: "treasury"}); !errors.Is(err, ErrEmptySweepFXCurrency) {
		t.Errorf("err = %v, want ErrEmptySweepFXCurrency", err)
	}
}

func TestPostSweepFXSweepReserve(t *testing.T) {
	l := seedSweepFXLedger(t)
	// Reserve 4000 USD on sub-a: only the excess 6000 converts.
	if err := l.SetSweepReserve("sub-a", "USD", 4000); err != nil {
		t.Fatalf("SetSweepReserve: %v", err)
	}
	receipt, err := l.PostSweepFX(SweepFX{ID: "fxsweep-res", From: []AccountID{"sub-a"}, To: "treasury", ToCurrency: "EUR"})
	if err != nil {
		t.Fatalf("PostSweepFX: %v", err)
	}
	// floor(6000 * 108 / 100) = 6480 EUR.
	if receipt.Legs[0].ConvertedCents != 6480 {
		t.Errorf("converted = %d, want 6480", receipt.Legs[0].ConvertedCents)
	}
	if got := l.BalanceIn("sub-a", "USD"); got != 4000 {
		t.Errorf("BalanceIn(sub-a, USD) = %d, want 4000 (reserve left behind)", got)
	}
}

func TestPostSweepFXSnapshotRoundTrip(t *testing.T) {
	l := seedSweepFXLedger(t)
	if _, err := l.PostSweepFX(SweepFX{
		ID: "fxsweep-snap", From: []AccountID{"sub-a"}, To: "treasury",
		ToCurrency: "EUR", IdempotencyKey: "fxsnap-key",
	}); err != nil {
		t.Fatalf("PostSweepFX: %v", err)
	}

	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	// The FX-sweep idempotency registration survived: a replay on the
	// restored ledger returns the original receipt and books nothing.
	replay, err := restored.PostSweepFX(SweepFX{
		ID: "fxsweep-snap-retry", From: []AccountID{"sub-a"}, To: "treasury",
		ToCurrency: "EUR", IdempotencyKey: "fxsnap-key",
	})
	if err != nil {
		t.Fatalf("replay on restored ledger: %v", err)
	}
	if !replay.Duplicate || replay.SweepID != "fxsweep-snap" {
		t.Errorf("replay = %+v, want duplicate of fxsweep-snap", replay)
	}
	if replay.ToCurrency != "EUR" || replay.FXAccount != "fx-clearing" {
		t.Errorf("replay to/fx = %q/%q, want EUR/fx-clearing", replay.ToCurrency, replay.FXAccount)
	}
	if got := restored.BalanceIn("treasury", "EUR"); got != 10800 {
		t.Errorf("restored treasury EUR = %d, want 10800", got)
	}
}

func TestPostSweepFXEmptySweep(t *testing.T) {
	l := seedSweepFXLedger(t)
	// sub-a's USD is positive only; sweeping a zero-balance account
	// succeeds with an empty receipt.
	receipt, err := l.PostSweepFX(SweepFX{
		ID: "fxsweep-empty", From: []AccountID{"ghost"}, To: "treasury", ToCurrency: "EUR",
	})
	if err != nil {
		t.Fatalf("PostSweepFX: %v", err)
	}
	if len(receipt.Legs) != 0 || len(receipt.Entries) != 0 {
		t.Errorf("receipt = %+v, want empty", receipt)
	}
	if receipt.FXAccount != "fx-clearing" {
		t.Errorf("receipt.FXAccount = %q, want fx-clearing", receipt.FXAccount)
	}
}
