package ledger

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// TestNormalizeCurrency checks the ISO 4217 validation: empty normalizes
// to the default currency, exactly three ASCII uppercase letters pass,
// everything else is rejected.
func TestNormalizeCurrency(t *testing.T) {
	for _, code := range []string{"USD", "EUR", "CNY", "JPY", "GBP"} {
		if got, err := normalizeCurrency(code); err != nil || got != code {
			t.Errorf("normalizeCurrency(%q) = %q, %v; want %q, nil", code, got, err, code)
		}
	}
	if got, err := normalizeCurrency(""); err != nil || got != DefaultCurrency {
		t.Errorf("normalizeCurrency(\"\") = %q, %v; want %q, nil", got, err, DefaultCurrency)
	}
	for _, bad := range []string{"usd", "Usd", "US", "USDD", "U$D", "12A", "US ", " USD", "€UR", "...."} {
		if _, err := normalizeCurrency(bad); !errors.Is(err, ErrInvalidCurrency) {
			t.Errorf("normalizeCurrency(%q) err = %v; want ErrInvalidCurrency", bad, err)
		}
	}
}

// TestPostCurrencyNormalizedToDefault checks that an entry posted without
// a currency is journaled in the default currency, and that replays with
// an explicit default code still hit the idempotency index (normalization
// runs before the replay check).
func TestPostCurrencyNormalizedToDefault(t *testing.T) {
	l := New()
	posted, dup, err := l.Post(validEntry())
	if err != nil || dup {
		t.Fatalf("Post = dup=%v err=%v", err, dup)
	}
	if posted.Currency != DefaultCurrency {
		t.Fatalf("posted.Currency = %q, want %q", posted.Currency, DefaultCurrency)
	}
	if got := l.Balance("cash"); got != 1000 {
		t.Fatalf("Balance(cash) = %d, want 1000", got)
	}

	// Replay the same idempotency key with an explicit "USD": same
	// canonical currency, so it must still be a duplicate, not a new
	// posting.
	retry := validEntry()
	retry.ID = "e-cur-retry"
	retry.Currency = "USD"
	if _, dup, err := l.Post(retry); err != nil || !dup {
		t.Fatalf("replay Post = dup=%v err=%v, want dup=true", dup, err)
	}
	if _, v := l.Snapshot("cash"); v != 1 {
		t.Fatalf("version after replay = %d, want 1", v)
	}
}

// TestPostInvalidCurrencyRejected checks that malformed currency codes are
// rejected as field validation (400-class): nothing is recorded, the
// version does not move, and the journal stays empty.
func TestPostInvalidCurrencyRejected(t *testing.T) {
	for _, bad := range []string{"usd", "US", "USDD", "U$D"} {
		l := New()
		e := validEntry()
		e.Currency = bad
		if _, _, err := l.Post(e); !errors.Is(err, ErrInvalidCurrency) {
			t.Fatalf("Post currency %q err = %v, want ErrInvalidCurrency", bad, err)
		}
		if len(l.Entries()) != 0 {
			t.Fatalf("journal has %d entries after rejected post", len(l.Entries()))
		}
		if _, v := l.Snapshot("cash"); v != 0 {
			t.Fatalf("version = %d after rejected post, want 0", v)
		}
	}
}

// TestMultiCurrencyBalancesIsolated checks that one account can hold
// several currencies side by side: balances and debit/credit totals are
// tracked per (account, currency), Balance keeps its default-currency
// meaning, and BalanceIn reads a specific currency.
func TestMultiCurrencyBalancesIsolated(t *testing.T) {
	l := New()
	post := func(id, debit, credit, currency string, cents int64) {
		t.Helper()
		e := validEntry()
		e.ID, e.DebitAccount, e.CreditAccount = id, AccountID(debit), AccountID(credit)
		e.AmountCents, e.Currency = cents, currency
		e.IdempotencyKey = "key-" + id
		if _, dup, err := l.Post(e); err != nil || dup {
			t.Fatalf("Post %s = dup=%v err=%v", id, dup, err)
		}
	}

	post("e-cur-1", "cash", "equity", "USD", 1000)
	post("e-cur-2", "cash", "equity", "EUR", 2000)
	post("e-cur-3", "cash", "equity", "EUR", 500)

	if got := l.Balance("cash"); got != 1000 {
		t.Fatalf("Balance(cash) = %d, want 1000 (default-currency view)", got)
	}
	if got := l.BalanceIn("cash", "EUR"); got != 2500 {
		t.Fatalf("BalanceIn(cash, EUR) = %d, want 2500", got)
	}
	if got := l.BalanceIn("cash", ""); got != 1000 {
		t.Fatalf("BalanceIn(cash, \"\") = %d, want 1000 (empty = default)", got)
	}
	if got := l.BalanceIn("cash", "JPY"); got != 0 {
		t.Fatalf("BalanceIn(cash, JPY) = %d, want 0", got)
	}
	// Equity's credit side is isolated per currency too.
	if got := l.BalanceIn("equity", "EUR"); got != -2500 {
		t.Fatalf("BalanceIn(equity, EUR) = %d, want -2500", got)
	}

	if err := l.VerifyAccountingEquation(); err != nil {
		t.Fatalf("VerifyAccountingEquation = %v", err)
	}
}

// TestVerifyAccountingEquationPerCurrency checks the isolation property
// directly: each currency's books balance independently. A USD debit can
// never offset a EUR credit in the equation.
func TestVerifyAccountingEquationPerCurrency(t *testing.T) {
	l := New()
	mk := func(id, debit, credit, currency string, cents int64) JournalEntry {
		e := validEntry()
		e.ID, e.DebitAccount, e.CreditAccount = id, AccountID(debit), AccountID(credit)
		e.AmountCents, e.Currency = cents, currency
		e.IdempotencyKey = "key-" + id
		return e
	}
	for i, e := range []JournalEntry{
		mk("e-eq-1", "cash", "equity", "USD", 1000),
		mk("e-eq-2", "cash", "equity", "EUR", 2000),
		mk("e-eq-3", "expenses", "cash", "EUR", 500),
		mk("e-eq-4", "cash", "equity", "CNY", 700),
	} {
		if _, dup, err := l.Post(e); err != nil || dup {
			t.Fatalf("Post %d = dup=%v err=%v", i, dup, err)
		}
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Fatalf("VerifyAccountingEquation = %v", err)
	}

	// Corrupt one currency's balance row: the error must name the
	// currency, proving the check is per-currency and not a global sum.
	l.balances[accountCurrency{account: "cash", currency: "EUR"}] -= 100
	err := l.VerifyAccountingEquation()
	if err == nil || !strings.Contains(err.Error(), "EUR") {
		t.Fatalf("VerifyAccountingEquation after EUR corruption = %v, want error naming EUR", err)
	}
}

// TestTrialBalanceByCurrency checks the grouped output: the top-level
// totals are the default-currency view, and ByCurrency lists every
// currency the account holds, sorted by code.
func TestTrialBalanceByCurrency(t *testing.T) {
	l := New()
	post := func(id, debit, credit, currency string, cents int64) {
		t.Helper()
		e := validEntry()
		e.ID, e.DebitAccount, e.CreditAccount = id, AccountID(debit), AccountID(credit)
		e.AmountCents, e.Currency = cents, currency
		e.IdempotencyKey = "key-" + id
		if _, dup, err := l.Post(e); err != nil || dup {
			t.Fatalf("Post %s = dup=%v err=%v", id, dup, err)
		}
	}
	post("e-tbcur-1", "cash", "equity", "", 1000)    // default USD
	post("e-tbcur-2", "cash", "equity", "EUR", 2000) // explicit EUR

	tb := l.TrialBalance("cash")
	if tb.Currency != DefaultCurrency {
		t.Fatalf("TrialBalance.Currency = %q, want %q", tb.Currency, DefaultCurrency)
	}
	if tb.TotalDebits != 1000 || tb.TotalCredits != 0 || tb.NetBalance != 1000 {
		t.Fatalf("top-level totals = %+v, want USD debits 1000 net 1000", tb)
	}
	if len(tb.ByCurrency) != 2 {
		t.Fatalf("ByCurrency = %+v, want 2 rows", tb.ByCurrency)
	}
	// Sorted by code: EUR before USD.
	if tb.ByCurrency[0].Currency != "EUR" || tb.ByCurrency[1].Currency != "USD" {
		t.Fatalf("ByCurrency order = %q, %q; want EUR, USD",
			tb.ByCurrency[0].Currency, tb.ByCurrency[1].Currency)
	}
	eur := tb.ByCurrency[0]
	if eur.TotalDebits != 2000 || eur.NetBalance != 2000 {
		t.Fatalf("EUR row = %+v, want debits 2000 net 2000", eur)
	}
	if eur.NetBalance != eur.TotalDebits-eur.TotalCredits {
		t.Fatalf("EUR row net %d != debits %d - credits %d",
			eur.NetBalance, eur.TotalDebits, eur.TotalCredits)
	}
}

// TestTransferCurrency checks that a transfer books its principal and fee
// legs in the transfer's currency, and that the receipt entries carry it.
func TestTransferCurrency(t *testing.T) {
	l := New()
	receipt, err := l.PostTransfer(Transfer{
		ID: "t-cur-1", From: "alice", To: "bob", AmountCents: 1000,
		Currency: "EUR", IdempotencyKey: "tkey-cur-1",
	})
	if err != nil {
		t.Fatalf("PostTransfer = %v", err)
	}
	if len(receipt.Entries) != 1 {
		t.Fatalf("receipt has %d entries, want 1", len(receipt.Entries))
	}
	for _, e := range receipt.Entries {
		if e.Currency != "EUR" {
			t.Fatalf("receipt entry %s currency = %q, want EUR", e.ID, e.Currency)
		}
	}
	if got := l.BalanceIn("bob", "EUR"); got != 1000 {
		t.Fatalf("BalanceIn(bob, EUR) = %d, want 1000", got)
	}
	if got := l.BalanceIn("alice", "EUR"); got != -1000 {
		t.Fatalf("BalanceIn(alice, EUR) = %d, want -1000", got)
	}
	if got := l.Balance("bob"); got != 0 {
		t.Fatalf("Balance(bob) = %d, want 0 (no USD postings)", got)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Fatalf("VerifyAccountingEquation = %v", err)
	}
}

// TestTransferInvalidCurrencyRejected checks that a transfer with a
// malformed currency code fails field validation and records nothing.
func TestTransferInvalidCurrencyRejected(t *testing.T) {
	l := New()
	_, err := l.PostTransfer(Transfer{
		ID: "t-cur-bad", From: "alice", To: "bob", AmountCents: 100,
		Currency: "euro",
	})
	if !errors.Is(err, ErrInvalidCurrency) {
		t.Fatalf("PostTransfer err = %v, want ErrInvalidCurrency", err)
	}
	if len(l.Entries()) != 0 {
		t.Fatalf("journal has %d entries after rejected transfer", len(l.Entries()))
	}
}

// TestTransferLegsShareOneCurrency pins the no-FX invariant: every leg a
// transfer commits carries the transfer's single currency, so a transfer
// can never span currencies.
func TestTransferLegsShareOneCurrency(t *testing.T) {
	l := New(WithTransferFeePolicy(1000, "fee-revenue")) // 10% policy fee
	receipt, err := l.PostTransfer(Transfer{
		ID: "t-cur-fee", From: "alice", To: "bob", AmountCents: 1000,
		Currency: "CNY", IdempotencyKey: "tkey-cur-fee",
	})
	if err != nil {
		t.Fatalf("PostTransfer = %v", err)
	}
	if len(receipt.Entries) != 2 {
		t.Fatalf("receipt has %d entries, want 2 (principal + fee)", len(receipt.Entries))
	}
	for _, e := range receipt.Entries {
		if e.Currency != "CNY" {
			t.Fatalf("leg %s currency = %q, want CNY (no cross-currency legs)", e.ID, e.Currency)
		}
	}
	// The fee leg lands on the revenue account in the transfer's
	// currency, isolated from other currencies.
	if got := l.BalanceIn("fee-revenue", "CNY"); got != 100 {
		t.Fatalf("BalanceIn(fee-revenue, CNY) = %d, want 100", got)
	}
	if got := l.BalanceIn("fee-revenue", "USD"); got != 0 {
		t.Fatalf("BalanceIn(fee-revenue, USD) = %d, want 0", got)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Fatalf("VerifyAccountingEquation = %v", err)
	}
}

// TestOverdraftPerCurrency checks that overdraft protection compares
// against the payer's balance in the transfer/entry currency: a EUR
// balance cannot cover a USD outflow.
func TestOverdraftPerCurrency(t *testing.T) {
	l := New()
	l.EnableOverdraftProtection("alice")
	// Fund alice with EUR only.
	e := validEntry()
	e.ID, e.DebitAccount, e.CreditAccount = "e-odcur-1", "alice", "equity"
	e.AmountCents, e.Currency = 5000, "EUR"
	e.IdempotencyKey = "key-odcur-1"
	if _, _, err := l.Post(e); err != nil {
		t.Fatalf("fund Post = %v", err)
	}

	// A USD posting would overdraw alice's (zero) USD balance.
	usd := validEntry()
	usd.ID, usd.DebitAccount, usd.CreditAccount = "e-odcur-2", "bob", "alice"
	usd.AmountCents, usd.Currency = 100, "USD"
	usd.IdempotencyKey = "key-odcur-2"
	if _, _, err := l.Post(usd); !errors.Is(err, ErrAccountOverdraft) {
		t.Fatalf("USD Post err = %v, want ErrAccountOverdraft", err)
	}
	// The same amount in EUR is covered.
	eur := validEntry()
	eur.ID, eur.DebitAccount, eur.CreditAccount = "e-odcur-3", "bob", "alice"
	eur.AmountCents, eur.Currency = 100, "EUR"
	eur.IdempotencyKey = "key-odcur-3"
	if _, _, err := l.Post(eur); err != nil {
		t.Fatalf("EUR Post = %v", err)
	}
	if got := l.BalanceIn("alice", "EUR"); got != 4900 {
		t.Fatalf("BalanceIn(alice, EUR) = %d, want 4900", got)
	}
}

// TestChainCoversCurrency checks that the tamper-evident audit chain
// covers the currency field: rewriting it after posting is detected.
func TestChainCoversCurrency(t *testing.T) {
	l := New()
	e := validEntry()
	e.Currency = "EUR"
	if _, _, err := l.Post(e); err != nil {
		t.Fatalf("Post = %v", err)
	}
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain = %v", err)
	}
	// Rewrite the journaled currency behind the chain's back.
	stored := l.entries["e-1"]
	stored.Currency = "USD"
	l.entries["e-1"] = stored
	if err := l.VerifyChain(); err == nil {
		t.Fatal("VerifyChain = nil after currency rewrite, want hash mismatch")
	}
}

// TestCurrencyConcurrentPosts hammers the ledger with concurrent
// multi-currency posts under -race: per-currency balances must stay exact
// and the equation must hold afterwards.
func TestCurrencyConcurrentPosts(t *testing.T) {
	l := New()
	const goroutines = 8
	const perGoroutine = 250
	currencies := []string{"USD", "EUR", "CNY", "JPY"}

	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			cur := currencies[g%len(currencies)]
			for i := 0; i < perGoroutine; i++ {
				e := JournalEntry{
					ID:             fmt.Sprintf("e-curc-g%02d-%04d", g, i),
					DebitAccount:   "cash",
					CreditAccount:  "equity",
					AmountCents:    1,
					Currency:       cur,
					IdempotencyKey: fmt.Sprintf("key-curc-g%02d-%04d", g, i),
				}
				if _, dup, err := l.Post(e); err != nil || dup {
					errs <- fmt.Errorf("goroutine %d post %d: dup=%v err=%v", g, i, dup, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	// Each currency saw exactly 2 goroutines × 250 cents.
	for _, cur := range currencies {
		if got := l.BalanceIn("cash", cur); got != 500 {
			t.Fatalf("BalanceIn(cash, %s) = %d, want 500", cur, got)
		}
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Fatalf("VerifyAccountingEquation after concurrent multi-currency posts = %v", err)
	}
}
