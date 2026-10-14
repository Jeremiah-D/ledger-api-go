package ledger

// LG-86: rate admission at request time and the opt-in slippage cap.
//
// PostTransfer admits the FX rate quote before taking the write lock
// (see admitFXRateQuote); the commit settles at the admitted price and
// measures the live rate's drift against it (see fxSlippageBps). These
// tests reproduce the admission→commit window deterministically via
// postWithAdmittedQuote: admit, mutate the table (what a concurrent
// SetFXRate would do under the same write lock), then post through the
// internal entry point.

import (
	"errors"
	"fmt"
	"math/big"
	"sync"
	"testing"
)

// admissionTestLedger builds a ledger with an FX account, a USD->EUR
// rate of 108/100, and a funded payer.
func admissionTestLedger(t *testing.T) *Ledger {
	t.Helper()
	l := New(WithFXAccount("fx-pnl"))
	mustSetRate(t, l, "USD", "EUR", 108, 100)
	if _, _, err := l.Post(JournalEntry{ID: "fund", DebitAccount: "payer", CreditAccount: "bank", AmountCents: 1000000, Currency: "USD"}); err != nil {
		t.Fatalf("fund: %v", err)
	}
	return l
}

// postWithAdmittedQuote reproduces the admission→commit window: it
// admits the quote, runs move (e.g. a concurrent SetFXRate) between
// admission and commit, then posts through the internal entry point
// under the write lock.
func postWithAdmittedQuote(l *Ledger, tr Transfer, move func()) (TransferReceipt, error) {
	quote := l.admitFXRateQuote(tr)
	if move != nil {
		move()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.postTransferLocked(tr, quote)
}

func TestFXSlippageBps(t *testing.T) {
	// |120/100 − 108/100| / (108/100) = 12/108 ≈ 11.111% → 1111 bps (floor).
	if got := fxSlippageBps(120, 100, 108, 100); got != 1111 {
		t.Errorf("moved up: got %d, want 1111", got)
	}
	// |100/100 − 108/100| / (108/100) = 8/108 ≈ 7.407% → 740 bps.
	if got := fxSlippageBps(100, 100, 108, 100); got != 740 {
		t.Errorf("moved down: got %d, want 740", got)
	}
	// No movement: zero slippage.
	if got := fxSlippageBps(108, 100, 108, 100); got != 0 {
		t.Errorf("unchanged: got %d, want 0", got)
	}
	// Reduced-fraction inputs measure the same drift: 12/10 vs 108/100
	// is the same 1/9 move as 120/100 vs 108/100.
	if got := fxSlippageBps(12, 10, 108, 100); got != 1111 {
		t.Errorf("unreduced: got %d, want 1111", got)
	}
}

func TestFXSlippageExceededRecordsNothing(t *testing.T) {
	l := admissionTestLedger(t)
	entriesBefore := len(l.entries)
	versionBefore := l.Version()
	tr := Transfer{ID: "fx-slip-1", From: "payer", To: "payee", AmountCents: 10000,
		Currency: "USD", ToCurrency: "EUR", MaxSlippageBps: 1000}
	_, err := postWithAdmittedQuote(l, tr, func() {
		mustSetRate(t, l, "USD", "EUR", 120, 100) // 1111 bps of drift
	})
	if !errors.Is(err, ErrFXSlippageExceeded) {
		t.Fatalf("err=%v, want ErrFXSlippageExceeded", err)
	}
	// A slippage rejection records nothing: no journal rows, no chain
	// links, no version bump.
	if len(l.entries) != entriesBefore {
		t.Errorf("journal grew on rejected transfer: %d -> %d", entriesBefore, len(l.entries))
	}
	if l.Version() != versionBefore {
		t.Errorf("version bumped on rejected transfer: %d -> %d", versionBefore, l.Version())
	}
	if _, exists := l.entries["fx-slip-1"]; exists {
		t.Error("rejected transfer left a journal row")
	}
}

func TestFXSlippageCapBoundary(t *testing.T) {
	// The gate is strict: slippage_bps strictly greater than the cap
	// rejects; equal to the cap settles.
	for _, tc := range []struct {
		name      string
		cap       int64
		wantError bool
	}{
		{"equal settles", 1111, false},
		{"one below rejects", 1110, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := admissionTestLedger(t)
			tr := Transfer{ID: "fx-slip-bound", From: "payer", To: "payee", AmountCents: 10000,
				Currency: "USD", ToCurrency: "EUR", MaxSlippageBps: tc.cap}
			_, err := postWithAdmittedQuote(l, tr, func() {
				mustSetRate(t, l, "USD", "EUR", 120, 100)
			})
			if tc.wantError && !errors.Is(err, ErrFXSlippageExceeded) {
				t.Fatalf("err=%v, want ErrFXSlippageExceeded", err)
			}
			if !tc.wantError && err != nil {
				t.Fatalf("err=%v, want nil", err)
			}
		})
	}
}

func TestFXSettlesAtAdmittedRate(t *testing.T) {
	l := admissionTestLedger(t)
	tr := Transfer{ID: "fx-adm-1", From: "payer", To: "payee", AmountCents: 10000,
		Currency: "USD", ToCurrency: "EUR"}
	receipt, err := postWithAdmittedQuote(l, tr, func() {
		mustSetRate(t, l, "USD", "EUR", 120, 100)
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	fx := receipt.FX
	if fx == nil {
		t.Fatal("receipt has no FX disclosure")
	}
	// Settled at the admitted 108/100 — the live 120/100 only shows up
	// in the disclosure.
	if fx.ConvertedCents != 10800 {
		t.Errorf("converted=%d, want 10800 (admitted 108/100)", fx.ConvertedCents)
	}
	if fx.RateNum != 108 || fx.RateDen != 100 {
		t.Errorf("rate=%d/%d, want 108/100", fx.RateNum, fx.RateDen)
	}
	if fx.AdmittedRateNum != 108 || fx.AdmittedRateDen != 100 {
		t.Errorf("admitted_rate=%d/%d, want 108/100", fx.AdmittedRateNum, fx.AdmittedRateDen)
	}
	if fx.SlippageBps != 1111 {
		t.Errorf("slippage_bps=%d, want 1111", fx.SlippageBps)
	}
	if fx.AdmittedAt.IsZero() {
		t.Error("admitted_at is zero")
	}
	// The booked principal leg carries the admitted conversion.
	if len(receipt.Entries) < 1 || receipt.Entries[0].AmountCents != 10800 || receipt.Entries[0].Currency != "EUR" {
		t.Errorf("principal leg = %+v, want 10800 EUR", receipt.Entries[0])
	}
	// No cap: the drift is disclosed, not gated.
	if fx.SlippageBps == 0 {
		t.Error("slippage_bps must be disclosed even without a cap")
	}
}

func TestFXSlippageCapNotExceeded(t *testing.T) {
	l := admissionTestLedger(t)
	tr := Transfer{ID: "fx-cap-ok", From: "payer", To: "payee", AmountCents: 10000,
		Currency: "USD", ToCurrency: "EUR", MaxSlippageBps: 2000}
	receipt, err := postWithAdmittedQuote(l, tr, func() {
		mustSetRate(t, l, "USD", "EUR", 120, 100) // 1111 bps < 2000 cap
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if receipt.FX == nil || receipt.FX.ConvertedCents != 10800 {
		t.Fatalf("want settlement at admitted 108/100, got %+v", receipt.FX)
	}
	if receipt.FX.SlippageBps != 1111 {
		t.Errorf("slippage_bps=%d, want 1111", receipt.FX.SlippageBps)
	}
}

func TestFXNegativeMaxSlippageRejected(t *testing.T) {
	l := admissionTestLedger(t)
	versionBefore := l.Version()
	// FX transfer with a negative cap.
	tr := Transfer{ID: "fx-neg-1", From: "payer", To: "payee", AmountCents: 100,
		Currency: "USD", ToCurrency: "EUR", MaxSlippageBps: -1}
	if _, err := l.PostTransfer(tr); !errors.Is(err, ErrInvalidMaxSlippage) {
		t.Errorf("FX transfer: err=%v, want ErrInvalidMaxSlippage", err)
	}
	// Same-currency transfer with a negative cap: validation runs
	// before the FX branch, so this rejects too.
	tr2 := Transfer{ID: "fx-neg-2", From: "payer", To: "payee", AmountCents: 100,
		Currency: "USD", MaxSlippageBps: -5}
	if _, err := l.PostTransfer(tr2); !errors.Is(err, ErrInvalidMaxSlippage) {
		t.Errorf("same-currency transfer: err=%v, want ErrInvalidMaxSlippage", err)
	}
	if l.Version() != versionBefore {
		t.Errorf("version moved on validation rejection: %d -> %d", versionBefore, l.Version())
	}
}

func TestFXSlippageCapIgnoredOnSameCurrency(t *testing.T) {
	l := admissionTestLedger(t)
	// A positive cap on a same-currency transfer is ignored, not
	// rejected: the transfer posts plainly with no FX disclosure.
	tr := Transfer{ID: "fx-ig-1", From: "payer", To: "payee", AmountCents: 100,
		Currency: "USD", MaxSlippageBps: 50}
	receipt, err := l.PostTransfer(tr)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if receipt.FX != nil {
		t.Errorf("same-currency receipt carries FX disclosure: %+v", receipt.FX)
	}
}

func TestFXDenylistBeatsMissingRate(t *testing.T) {
	l := New(WithFXAccount("fx-pnl"))
	l.AddDeniedAccount("payer")
	// No rate is configured for USD->EUR at all: screening must reject
	// with the denylist error, not ErrFXRateMissing — the screening
	// order (denylist before the FX branch) is pinned by this test.
	tr := Transfer{ID: "fx-deny-1", From: "payer", To: "payee", AmountCents: 100,
		Currency: "USD", ToCurrency: "EUR"}
	_, err := l.PostTransfer(tr)
	if !errors.Is(err, ErrAccountDenied) {
		t.Fatalf("err=%v, want ErrAccountDenied (not ErrFXRateMissing)", err)
	}
	// A denied transfer leaves no idempotency trace and no journal row.
	if l.Version() != 0 {
		t.Errorf("version=%d after denied transfer, want 0", l.Version())
	}
	if _, exists := l.entries["fx-deny-1"]; exists {
		t.Error("denied transfer left a journal row")
	}
}

func TestFXAdmissionIncludesOverride(t *testing.T) {
	l := admissionTestLedger(t)
	// An approved override at 110/100 (1.85% off market, inside the 10%
	// default cap): admission must resolve the full effective price,
	// override included.
	if err := l.SetRateOverride("USD", "EUR", big.NewRat(110, 100), 0, "feed stale", "alice"); err != nil {
		t.Fatalf("propose: %v", err)
	}
	if err := l.ApproveRateOverride("USD", "EUR", "bob"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	tr := Transfer{ID: "fx-ovr-1", From: "payer", To: "payee", AmountCents: 10000,
		Currency: "USD", ToCurrency: "EUR"}
	receipt, err := postWithAdmittedQuote(l, tr, nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	fx := receipt.FX
	if fx == nil {
		t.Fatal("receipt has no FX disclosure")
	}
	// Settled at the override price, disclosed as admitted.
	if fx.ConvertedCents != 11000 {
		t.Errorf("converted=%d, want 11000 (override 110/100)", fx.ConvertedCents)
	}
	if !fx.OverrideApplied || fx.OverrideApprovedBy != "bob" {
		t.Errorf("override disclosure = applied:%v by:%q, want true/bob", fx.OverrideApplied, fx.OverrideApprovedBy)
	}
	if fx.AdmittedRateNum != 11 || fx.AdmittedRateDen != 10 {
		// big.Rat always reduces: the proposed 110/100 is stored and
		// admitted as the identical ratio 11/10.
		t.Errorf("admitted_rate=%d/%d, want 11/10", fx.AdmittedRateNum, fx.AdmittedRateDen)
	}
	if fx.MarketRateNum != 108 || fx.MarketRateDen != 100 {
		t.Errorf("market_rate=%d/%d, want 108/100", fx.MarketRateNum, fx.MarketRateDen)
	}
}

func TestFXAdmissionAuditDetails(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al), WithFXAccount("fx-pnl"))
	mustSetRate(t, l, "USD", "EUR", 108, 100)
	if _, _, err := l.Post(JournalEntry{ID: "fund", DebitAccount: "payer", CreditAccount: "bank", AmountCents: 1000000, Currency: "USD"}); err != nil {
		t.Fatalf("fund: %v", err)
	}
	tr := Transfer{ID: "fx-audit-1", From: "payer", To: "payee", AmountCents: 10000,
		Currency: "USD", ToCurrency: "EUR", MaxSlippageBps: 2000}
	if _, err := postWithAdmittedQuote(l, tr, func() {
		mustSetRate(t, l, "USD", "EUR", 120, 100)
	}); err != nil {
		t.Fatalf("post: %v", err)
	}
	var found bool
	for _, ev := range providerAuditEvents(t, al) {
		if ev.Op != "transfer" || ev.TraceID != "fx-audit-1" {
			continue
		}
		found = true
		d := ev.Details
		admitted, ok := d["admitted_rate"].(map[string]any)
		if !ok {
			t.Fatalf("details has no admitted_rate map: %v", d)
		}
		if admitted["num"] != float64(108) || admitted["den"] != float64(100) {
			t.Errorf("admitted_rate = %v, want num:108 den:100", admitted)
		}
		if _, ok := admitted["effective_version"]; !ok {
			t.Errorf("admitted_rate missing effective_version: %v", admitted)
		}
		if _, ok := admitted["admitted_at"].(string); !ok {
			t.Errorf("admitted_rate missing admitted_at string: %v", admitted)
		}
		committed, ok := d["committed_rate"].(map[string]any)
		if !ok {
			t.Fatalf("details has no committed_rate map: %v", d)
		}
		if committed["num"] != float64(120) || committed["den"] != float64(100) {
			t.Errorf("committed_rate = %v, want num:120 den:100", committed)
		}
		if d["slippage_bps"] != float64(1111) {
			t.Errorf("slippage_bps = %v, want 1111", d["slippage_bps"])
		}
		if d["max_slippage_bps"] != float64(2000) {
			t.Errorf("max_slippage_bps = %v, want 2000", d["max_slippage_bps"])
		}
		// The pre-LG-86 provenance stays sealed alongside.
		if d["converted_cents"] != float64(10800) {
			t.Errorf("converted_cents = %v, want 10800 (admitted price)", d["converted_cents"])
		}
	}
	if !found {
		t.Fatal("no transfer audit event for fx-audit-1")
	}
}

func TestFXDryRunSlippageNoSideEffects(t *testing.T) {
	l := admissionTestLedger(t)
	versionBefore := l.Version()
	entriesBefore := len(l.entries)
	tr := Transfer{ID: "fx-dr-1", From: "payer", To: "payee", AmountCents: 10000,
		Currency: "USD", ToCurrency: "EUR", MaxSlippageBps: 1000}
	res, err := l.DryRunTransfer(tr)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !res.WouldSucceed {
		t.Error("dry run should succeed")
	}
	// Zero side effects on the live ledger.
	if l.Version() != versionBefore {
		t.Errorf("version moved on dry run: %d -> %d", versionBefore, l.Version())
	}
	if len(l.entries) != entriesBefore {
		t.Errorf("journal grew on dry run: %d -> %d", entriesBefore, len(l.entries))
	}
	// A negative cap is still validation-rejected on the dry-run path.
	trNeg := tr
	trNeg.ID = "fx-dr-2"
	trNeg.MaxSlippageBps = -1
	if _, err := l.DryRunTransfer(trNeg); !errors.Is(err, ErrInvalidMaxSlippage) {
		t.Errorf("dry run negative cap: err=%v, want ErrInvalidMaxSlippage", err)
	}
	// A missing rate is still a 422-class failure on the dry-run path.
	trMissing := tr
	trMissing.ID = "fx-dr-3"
	trMissing.ToCurrency = "JPY"
	if _, err := l.DryRunTransfer(trMissing); !errors.Is(err, ErrFXRateMissing) {
		t.Errorf("dry run missing rate: err=%v, want ErrFXRateMissing", err)
	}
}

func TestFXAdmissionConcurrent(t *testing.T) {
	l := admissionTestLedger(t)
	const n = 32
	var wg sync.WaitGroup
	receipts := make([]TransferReceipt, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tr := Transfer{
				ID:             fmt.Sprintf("fx-cc-%d", i),
				From:           "payer",
				To:             "payee",
				AmountCents:    100,
				Currency:       "USD",
				ToCurrency:     "EUR",
				MaxSlippageBps: 500,
			}
			receipts[i], errs[i] = l.PostTransfer(tr)
		}(i)
	}
	// Flip the rate table under the transfers: admission and commit
	// interleave for real here. Every transfer must settle at its own
	// admitted price or fail cleanly with ErrFXSlippageExceeded — never
	// panic, never mis-settle.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 100; j++ {
			if j%2 == 0 {
				_ = l.SetFXRate("USD", "EUR", 108, 100)
			} else {
				_ = l.SetFXRate("USD", "EUR", 120, 100)
			}
		}
	}()
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			if !errors.Is(errs[i], ErrFXSlippageExceeded) {
				t.Errorf("transfer %d: unexpected err %v", i, errs[i])
			}
			continue
		}
		fx := receipts[i].FX
		if fx == nil {
			t.Errorf("transfer %d: missing FX disclosure", i)
			continue
		}
		// The booked conversion must equal the admitted price's math:
		// no mis-settlement under the race.
		want, ok := fxConvertCents(100, fx.AdmittedRateNum, fx.AdmittedRateDen)
		if !ok || want != fx.ConvertedCents {
			t.Errorf("transfer %d: converted=%d, want floor(100*%d/%d)=%d",
				i, fx.ConvertedCents, fx.AdmittedRateNum, fx.AdmittedRateDen, want)
		}
		// Slippage is a non-negative disclosure (the commit-time rate
		// it was measured against is audit-only, not on the receipt).
		if fx.SlippageBps < 0 {
			t.Errorf("transfer %d: slippage_bps=%d, want >= 0", i, fx.SlippageBps)
		}
	}
}
