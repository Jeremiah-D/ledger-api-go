package ledger

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestAmendTermDepositCurrencyReleasesOldLockAndLocksNew(t *testing.T) {
	l := seedTermDepositLedger(t)
	seedBalanceIn(t, l, "saver", 1_000_000, "EUR")
	orig := openFlatTermDeposit(t, l, "td-1")
	day1 := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	// Day 1 books at the old currency: floor(365000*500/10000/365) = 50 USD.
	e1, dup, err := l.AccrueTermInterest("td-1", day1)
	if err != nil || dup {
		t.Fatalf("day 1 accrual: entry=%+v dup=%v err=%v", e1, dup, err)
	}
	if e1.AmountCents != 50 || e1.Currency != "USD" {
		t.Errorf("day 1 interest = %+v, want 50 USD", e1)
	}
	versionBefore := l.Version()
	amended, err := l.AmendTermDepositCurrency("td-1", "EUR")
	if err != nil {
		t.Fatalf("AmendTermDepositCurrency: %v", err)
	}
	if amended.Currency != "EUR" {
		t.Errorf("amended currency = %q, want EUR", amended.Currency)
	}
	if l.Version() != versionBefore {
		t.Error("version moved: amendment is off-journal")
	}
	// The old currency's lock is released and the new currency's
	// lock is in force: USD available is the full balance (plus
	// the day-1 interest), EUR available is the balance minus the
	// principal.
	if got := l.AvailableIn("saver", "USD"); got != 1_000_050 {
		t.Errorf("USD available after amend = %d, want 1000050", got)
	}
	if got := l.AvailableIn("saver", "EUR"); got != 635_000 {
		t.Errorf("EUR available after amend = %d, want 635000", got)
	}
	// Day 2 books in the new currency: floor(365000*500/10000/365)
	// = 50 EUR. The day-1 row keeps its old currency.
	e2, dup, err := l.AccrueTermInterest("td-1", day2)
	if err != nil || dup {
		t.Fatalf("day 2 accrual: entry=%+v dup=%v err=%v", e2, dup, err)
	}
	if e2.AmountCents != 50 || e2.Currency != "EUR" {
		t.Errorf("day 2 interest = %+v, want 50 EUR", e2)
	}
	if got := l.AvailableIn("saver", "EUR"); got != 635_050 {
		t.Errorf("EUR available after day-2 accrual = %d, want 635050", got)
	}
	// Only the currency moved.
	amended.Currency = orig.Currency
	if !reflect.DeepEqual(amended, orig) {
		t.Errorf("amended deposit = %+v, want %+v except the currency", amended, orig)
	}
}

func TestAmendTermDepositCurrencyValidation(t *testing.T) {
	// Unknown deposit.
	l := seedTermDepositLedger(t)
	seedBalanceIn(t, l, "saver", 364_999, "EUR")
	if _, err := l.AmendTermDepositCurrency("td-unknown", "EUR"); !errors.Is(err, ErrTermDepositNotFound) {
		t.Errorf("unknown deposit: err = %v, want ErrTermDepositNotFound", err)
	}
	orig := openFlatTermDeposit(t, l, "td-1")
	// Empty and malformed codes are rejected, live untouched:
	// unlike creation, an empty code does not default to USD.
	for _, code := range []string{"", "usd", "eur", "US", "USDD", "U$D", "EURO"} {
		if _, err := l.AmendTermDepositCurrency("td-1", code); !errors.Is(err, ErrInvalidCurrency) {
			t.Errorf("currency %q: err = %v, want ErrInvalidCurrency", code, err)
		}
	}
	if got, ok := l.GetTermDeposit("td-1"); !ok || !reflect.DeepEqual(got, orig) {
		t.Errorf("deposit after rejected currencies = %+v ok=%v, want %+v", got, ok, orig)
	}
	// Insufficient funds in the new currency: the saver holds
	// 364,999 EUR against a 365,000-cent principal.
	if _, err := l.AmendTermDepositCurrency("td-1", "EUR"); !errors.Is(err, ErrInsufficientAvailableFunds) {
		t.Errorf("under-funded amend: err = %v, want ErrInsufficientAvailableFunds", err)
	}
	if got, ok := l.GetTermDeposit("td-1"); !ok || got.Currency != "USD" {
		t.Errorf("deposit after rejected amend = %+v ok=%v, want USD", got, ok)
	}
	// Boundary: a EUR balance of exactly the principal fits.
	l2 := seedTermDepositLedger(t)
	seedBalanceIn(t, l2, "saver", 365_000, "EUR")
	openFlatTermDeposit(t, l2, "td-1")
	if got, err := l2.AmendTermDepositCurrency("td-1", "EUR"); err != nil || got.Currency != "EUR" {
		t.Errorf("boundary amend to EUR = %+v, %v, want EUR, nil", got, err)
	}
}

func TestAmendTermDepositCurrencyTerminalStates(t *testing.T) {
	// Matured.
	l := seedTermDepositLedger(t)
	seedBalanceIn(t, l, "saver", 1_000_000, "EUR")
	openFlatTermDeposit(t, l, "td-1")
	if _, err := l.MatureTermDeposit("td-1", time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("MatureTermDeposit: %v", err)
	}
	if _, err := l.AmendTermDepositCurrency("td-1", "EUR"); !errors.Is(err, ErrTermDepositNotActive) {
		t.Errorf("matured deposit: err = %v, want ErrTermDepositNotActive", err)
	}
	// Early-withdrawn.
	l2 := seedTermDepositLedger(t)
	seedBalanceIn(t, l2, "saver", 1_000_000, "EUR")
	openFlatTermDeposit(t, l2, "td-1")
	if _, err := l2.WithdrawTermDeposit("td-1", 0, time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("WithdrawTermDeposit: %v", err)
	}
	if _, err := l2.AmendTermDepositCurrency("td-1", "EUR"); !errors.Is(err, ErrTermDepositNotActive) {
		t.Errorf("withdrawn deposit: err = %v, want ErrTermDepositNotActive", err)
	}
}

func TestAmendTermDepositCurrencyMaturityReleasesNewCurrencyLock(t *testing.T) {
	// Amending to EUR moves the lock; maturing afterwards releases
	// the EUR lock — the maturity payout follows the new currency,
	// and the USD side is untouched throughout.
	l := seedTermDepositLedger(t)
	seedBalanceIn(t, l, "saver", 1_000_000, "EUR")
	openFlatTermDeposit(t, l, "td-1")
	if _, err := l.AmendTermDepositCurrency("td-1", "EUR"); err != nil {
		t.Fatalf("AmendTermDepositCurrency: %v", err)
	}
	if got := l.AvailableIn("saver", "EUR"); got != 635_000 {
		t.Fatalf("EUR available before maturity = %d, want 635000", got)
	}
	matured, err := l.MatureTermDeposit("td-1", time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("MatureTermDeposit: %v", err)
	}
	if matured.Currency != "EUR" || matured.Status != TermDepositMatured {
		t.Errorf("matured deposit = %+v, want EUR matured", matured)
	}
	if got := l.AvailableIn("saver", "EUR"); got != 1_000_000 {
		t.Errorf("EUR available after maturity = %d, want 1000000", got)
	}
	if got := l.AvailableIn("saver", "USD"); got != 1_000_000 {
		t.Errorf("USD available after maturity = %d, want 1000000", got)
	}
}

func TestAmendTermDepositCurrencyAuditAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithTermDepositInterestExpense("td-interest-expense"))
	seedBalance(t, l, "saver", 1_000_000)
	seedBalanceIn(t, l, "saver", 1_000_000, "EUR")
	before := openFlatTermDeposit(t, l, "td-1")
	// The no-op revision emits nothing.
	if _, err := l.AmendTermDepositCurrency("td-1", "USD"); err != nil {
		t.Fatalf("no-op amend: %v", err)
	}
	ctx := ContextWithActor(context.Background(), "ops-desk")
	if _, err := l.AmendTermDepositCurrencyCtx(ctx, "td-1", "EUR"); err != nil {
		t.Fatalf("AmendTermDepositCurrencyCtx: %v", err)
	}
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	after, ok := restored.GetTermDeposit("td-1")
	if !ok {
		t.Fatal("restored deposit missing")
	}
	if after.Currency != "EUR" {
		t.Errorf("restored currency = %q, want EUR", after.Currency)
	}
	after.Currency = before.Currency
	if !reflect.DeepEqual(after, before) {
		t.Errorf("restored deposit = %+v, want %+v except the currency", after, before)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["term_deposit_currency_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1 (no-op must not audit)", len(events))
	}
	if events[0].Actor != "ops-desk" {
		t.Errorf("event actor = %q, want ops-desk", events[0].Actor)
	}
	d := events[0].Details
	if d["term_deposit_id"] != "td-1" || d["previous_currency"] != "USD" ||
		d["currency"] != "EUR" || d["amount_cents"] != float64(365000) {
		t.Errorf("event details = %+v, want td-1 USD->EUR 365000", d)
	}
}

func TestAmendTermDepositCurrencyRolloverInheritsNewCurrency(t *testing.T) {
	// A child generated after the amendment inherits the amended
	// currency: the rollover copies the live row at generation
	// time, and the amendment writes only the parent's row.
	l := seedTermDepositLedger(t)
	seedBalanceIn(t, l, "saver", 1_000_000, "EUR")
	if _, _, err := l.OpenTermDeposit(TermDeposit{
		ID:          "td-roll",
		Account:     "saver",
		AmountCents: 365_000,
		TermDays:    10,
		RateBps:     500,
		OpenedAt:    time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		Rollover:    &RolloverPolicy{Mode: RolloverPrincipal, TermDays: 30, MaxRollovers: 3},
	}); err != nil {
		t.Fatalf("open rollover parent: %v", err)
	}
	if _, err := l.AmendTermDepositCurrency("td-roll", "EUR"); err != nil {
		t.Fatalf("AmendTermDepositCurrency: %v", err)
	}
	l.SweepTermDeposits(time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC))
	child, ok := l.GetTermDeposit("td-roll/roll/1")
	if !ok {
		t.Fatal("rollover child missing after sweep")
	}
	if child.Currency != "EUR" {
		t.Errorf("child currency = %q, want EUR (amended parent)", child.Currency)
	}
	if child.AmountCents != 365_000 {
		t.Errorf("child principal = %d, want 365000", child.AmountCents)
	}
}
