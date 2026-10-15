package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var dividendDay = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

func dividendHolders() []DividendHolder {
	return []DividendHolder{
		{Account: "holder-a", SharesBps: 6000},
		{Account: "holder-b", SharesBps: 4000},
	}
}

func TestDeclareDividendHappyPath(t *testing.T) {
	l := New()
	r, err := l.DeclareDividend("retained-earnings", 10000, "USD", dividendHolders(), dividendDay)
	if err != nil {
		t.Fatalf("DeclareDividend: %v", err)
	}
	if r.Duplicate {
		t.Fatal("first declaration must not be a duplicate")
	}
	if r.DividendID != "dividend/retained-earnings/2026-10-15" {
		t.Fatalf("dividend ID = %q, want deterministic date-suffixed ID", r.DividendID)
	}
	if len(r.Payouts) != 2 {
		t.Fatalf("payouts = %d, want 2", len(r.Payouts))
	}
	// 6000/4000 bps of 10000 cents: exact, no dust.
	if r.Payouts[0].AmountCents != 6000 || r.Payouts[1].AmountCents != 4000 {
		t.Fatalf("amounts = %d/%d, want 6000/4000", r.Payouts[0].AmountCents, r.Payouts[1].AmountCents)
	}
	if r.Payouts[0].SharesBps != 6000 || r.Payouts[1].SharesBps != 4000 {
		t.Fatalf("shares = %d/%d, want 6000/4000", r.Payouts[0].SharesBps, r.Payouts[1].SharesBps)
	}
	for i, p := range r.Payouts {
		wantID := "dividend/retained-earnings/2026-10-15/" + string(rune('0'+i))
		if p.EntryID != wantID {
			t.Fatalf("payout %d entry ID = %q, want %q", i, p.EntryID, wantID)
		}
	}
	// The books moved like ordinary postings: debit the pool (balance
	// up), credit each holder (balance down).
	if b := l.Balance("retained-earnings"); b != 10000 {
		t.Fatalf("pool balance = %d, want 10000", b)
	}
	if b := l.Balance("holder-a"); b != -6000 {
		t.Fatalf("holder-a balance = %d, want -6000", b)
	}
	if b := l.Balance("holder-b"); b != -4000 {
		t.Fatalf("holder-b balance = %d, want -4000", b)
	}
	// The accounting equation holds: total debits == total credits.
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Fatalf("VerifyAccountingEquation: %v", err)
	}
}

func TestDeclareDividendDustToFirstHolder(t *testing.T) {
	l := New()
	holders := []DividendHolder{
		{Account: "h1", SharesBps: 3333},
		{Account: "h2", SharesBps: 3333},
		{Account: "h3", SharesBps: 3334},
	}
	r, err := l.DeclareDividend("pool", 10001, "", holders, dividendDay)
	if err != nil {
		t.Fatalf("DeclareDividend: %v", err)
	}
	// floor(10001*3333/10000) = 3333, floor(10001*3334/10000) = 3334:
	// distributed = 10000, dust = 1 -> first holder.
	if r.Payouts[0].AmountCents != 3334 || r.Payouts[1].AmountCents != 3333 || r.Payouts[2].AmountCents != 3334 {
		t.Fatalf("amounts = %d/%d/%d, want 3334/3333/3334",
			r.Payouts[0].AmountCents, r.Payouts[1].AmountCents, r.Payouts[2].AmountCents)
	}
	var sum int64
	for _, p := range r.Payouts {
		sum += p.AmountCents
	}
	if sum != 10001 {
		t.Fatalf("distributed total = %d, want 10001", sum)
	}
	// Empty currency normalized to the default.
	if r.Currency != DefaultCurrency {
		t.Fatalf("currency = %q, want %q", r.Currency, DefaultCurrency)
	}
}

func TestDeclareDividendIdempotent(t *testing.T) {
	l := New()
	first, err := l.DeclareDividend("pool", 10000, "USD", dividendHolders(), dividendDay)
	if err != nil || first.Duplicate {
		t.Fatalf("first = dup=%v err=%v", first.Duplicate, err)
	}
	ver := l.Version()
	second, err := l.DeclareDividend("pool", 10000, "USD", dividendHolders(), dividendDay)
	if err != nil || !second.Duplicate {
		t.Fatalf("replay = dup=%v err=%v, want dup=true", second.Duplicate, err)
	}
	if second.DividendID != first.DividendID {
		t.Fatalf("replay dividend ID = %q, want %q", second.DividendID, first.DividendID)
	}
	for i := range first.Payouts {
		if second.Payouts[i].EntryID != first.Payouts[i].EntryID ||
			second.Payouts[i].AmountCents != first.Payouts[i].AmountCents ||
			second.Payouts[i].SharesBps != first.Payouts[i].SharesBps {
			t.Fatalf("replay payout %d = %+v, want %+v", i, second.Payouts[i], first.Payouts[i])
		}
	}
	if l.Version() != ver {
		t.Fatalf("replay bumped version %d -> %d", ver, l.Version())
	}
	// A different record date is a different declaration.
	if _, err := l.DeclareDividend("pool", 10000, "USD", dividendHolders(), dividendDay.Add(24*time.Hour)); err != nil {
		t.Fatalf("new record date: %v", err)
	}
	if n := len(l.Dividends()); n != 2 {
		t.Fatalf("dividends = %d, want 2", n)
	}
}

func TestDeclareDividendValidation(t *testing.T) {
	l := New()
	cases := []struct {
		name     string
		pool     AccountID
		total    int64
		currency string
		holders  []DividendHolder
		wantErr  error
	}{
		{"shares do not sum to 10000", "p", 100, "USD",
			[]DividendHolder{{"a", 5000}, {"b", 4999}}, ErrInvalidDividendShares},
		{"shares exceed 10000", "p", 100, "USD",
			[]DividendHolder{{"a", 10001}}, ErrInvalidDividendHolderShares},
		{"zero holder shares", "p", 100, "USD",
			[]DividendHolder{{"a", 0}, {"b", 10000}}, ErrInvalidDividendHolderShares},
		{"no holders", "p", 100, "USD", nil, ErrNoDividendHolders},
		{"pool with slash", "p/o", 100, "USD", dividendHolders(), ErrDividendAccountSeparator},
		{"holder with slash", "p", 100, "USD",
			[]DividendHolder{{"a/b", 10000}}, ErrDividendAccountSeparator},
		{"empty pool", "", 100, "USD", dividendHolders(), ErrEmptyDebitAccount},
		{"zero total", "p", 0, "USD", dividendHolders(), ErrNonPositiveAmount},
		{"bad currency", "p", 100, "usd", dividendHolders(), ErrInvalidCurrency},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := l.DeclareDividend(c.pool, c.total, c.currency, c.holders, dividendDay); !errors.Is(err, c.wantErr) {
				t.Fatalf("err = %v, want %v", err, c.wantErr)
			}
		})
	}
	if n := len(l.Dividends()); n != 0 {
		t.Fatalf("failed declarations booked %d dividends", n)
	}
}

func TestDeclareDividendRunsPostGates(t *testing.T) {
	l := New()
	// A frozen pool rejects the whole declaration before anything books.
	l.Freeze("pool")
	if _, err := l.DeclareDividend("pool", 10000, "USD", dividendHolders(), dividendDay); !errors.Is(err, ErrAccountFrozen) {
		t.Fatalf("frozen pool err = %v, want ErrAccountFrozen", err)
	}
	if n := len(l.Dividends()); n != 0 {
		t.Fatalf("frozen declaration booked %d dividends", n)
	}
	l.Unfreeze("pool")
	// An overdraft-protected holder whose dividend would take it
	// negative rejects: the credit leg decreases the holder's balance.
	l.EnableOverdraftProtection("holder-a")
	if _, err := l.DeclareDividend("pool", 10000, "USD", dividendHolders(), dividendDay); !errors.Is(err, ErrAccountOverdraft) {
		t.Fatalf("overdraft holder err = %v, want ErrAccountOverdraft", err)
	}
	l.DisableOverdraftProtection("holder-a")
	if _, err := l.DeclareDividend("pool", 10000, "USD", dividendHolders(), dividendDay); err != nil {
		t.Fatalf("after clearing gates: %v", err)
	}
}

func TestDeclareDividendResumeAfterPartialFailure(t *testing.T) {
	l := New()
	holders := []DividendHolder{
		{Account: "ok-1", SharesBps: 5000},
		{Account: "blocked", SharesBps: 3000},
		{Account: "ok-2", SharesBps: 2000},
	}
	l.Freeze("blocked")
	partial, err := l.DeclareDividend("pool", 10000, "USD", holders, dividendDay)
	if !errors.Is(err, ErrAccountFrozen) {
		t.Fatalf("partial err = %v, want ErrAccountFrozen", err)
	}
	if len(partial.Payouts) != 1 || partial.Payouts[0].Account != "ok-1" {
		t.Fatalf("partial payouts = %+v, want the first leg only", partial.Payouts)
	}
	l.Unfreeze("blocked")
	// The retry resumes: the journaled leg is skipped, the rest books.
	resumed, err := l.DeclareDividend("pool", 10000, "USD", holders, dividendDay)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed.Duplicate {
		t.Fatal("resumed declaration booked new legs, must not be a duplicate")
	}
	if len(resumed.Payouts) != 3 {
		t.Fatalf("resumed payouts = %d, want 3", len(resumed.Payouts))
	}
	var sum int64
	for _, p := range resumed.Payouts {
		sum += p.AmountCents
	}
	if sum != 10000 {
		t.Fatalf("resumed total = %d, want 10000", sum)
	}
	// A further retry is a clean replay.
	final, err := l.DeclareDividend("pool", 10000, "USD", holders, dividendDay)
	if err != nil || !final.Duplicate {
		t.Fatalf("final = dup=%v err=%v, want dup=true", final.Duplicate, err)
	}
}

func TestDeclareDividendSkipsRulesAndReview(t *testing.T) {
	l := New()
	// A journal rule that would cascade a derived leg off every posting.
	rule := JournalRule{
		ID:                "tax",
		MatchDebitAccount: "pool",
		Legs: []RuleLeg{
			{DebitAccount: "tax-expense", CreditAccount: "tax-payable", RateBps: 1000, Label: "tax"},
		},
	}
	if err := l.SetJournalRule(rule); err != nil {
		t.Fatalf("SetJournalRule: %v", err)
	}
	if _, err := l.DeclareDividend("pool", 10000, "USD", dividendHolders(), dividendDay); err != nil {
		t.Fatalf("DeclareDividend: %v", err)
	}
	// No derived legs: exactly the two dividend rows journaled.
	var dividendRows int
	for id := range l.entries {
		if strings.HasPrefix(id, "dividend/") {
			dividendRows++
		}
	}
	if dividendRows != 2 {
		t.Fatalf("journaled dividend rows = %d, want 2 (no rule-derived legs)", dividendRows)
	}
}

func TestDividendsListingAndReconcile(t *testing.T) {
	l := New()
	if _, err := l.DeclareDividend("pool-b", 5000, "USD", []DividendHolder{{"x", 10000}}, dividendDay); err != nil {
		t.Fatalf("declare 1: %v", err)
	}
	if _, err := l.DeclareDividend("pool-a", 8000, "USD", dividendHolders(), dividendDay); err != nil {
		t.Fatalf("declare 2: %v", err)
	}
	list := l.Dividends()
	if len(list) != 2 {
		t.Fatalf("Dividends() = %d, want 2", len(list))
	}
	if list[0].DividendID >= list[1].DividendID {
		t.Fatalf("dividends not sorted: %q, %q", list[0].DividendID, list[1].DividendID)
	}
	if list[0].TotalCents != 8000 || list[1].TotalCents != 5000 {
		t.Fatalf("totals = %d/%d, want 8000/5000", list[0].TotalCents, list[1].TotalCents)
	}
	rep := l.Reconcile(time.Now())
	if len(rep.Dividends) != 2 {
		t.Fatalf("reconcile dividends = %d, want 2", len(rep.Dividends))
	}
	if rep.Dividends[0].Pool != "pool-a" || rep.Dividends[0].TotalCents != 8000 {
		t.Fatalf("reconcile dividend[0] = %+v, want pool-a/8000", rep.Dividends[0])
	}
}

func TestDividendSnapshotRoundTrip(t *testing.T) {
	l := New()
	want, err := l.DeclareDividend("pool", 10000, "USD", dividendHolders(), dividendDay)
	if err != nil {
		t.Fatalf("DeclareDividend: %v", err)
	}
	var buf strings.Builder
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	l2, err := ImportSnapshot(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	got := l2.Dividends()
	if len(got) != 1 {
		t.Fatalf("restored dividends = %d, want 1", len(got))
	}
	if got[0].DividendID != want.DividendID || got[0].TotalCents != want.TotalCents || len(got[0].Payouts) != 2 {
		t.Fatalf("restored = %+v, want %+v", got[0], want)
	}
	// The restored ledger replays the declaration idempotently.
	replay, err := l2.DeclareDividend("pool", 10000, "USD", dividendHolders(), dividendDay)
	if err != nil || !replay.Duplicate {
		t.Fatalf("post-restore replay = dup=%v err=%v, want dup=true", replay.Duplicate, err)
	}
}

func TestDeclareDividendConcurrent(t *testing.T) {
	l := New()
	// Concurrent declarations of the same dividend converge: exactly one
	// books the legs, the rest replay.
	done := make(chan DividendReceipt, 8)
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			r, err := l.DeclareDividend("pool", 10000, "USD", dividendHolders(), dividendDay)
			if err != nil {
				errs <- err
				return
			}
			done <- r
		}()
	}
	var booked int
	for i := 0; i < 8; i++ {
		select {
		case r := <-done:
			if !r.Duplicate {
				booked++
			}
		case err := <-errs:
			t.Fatalf("concurrent declare: %v", err)
		}
	}
	if booked != 1 {
		t.Fatalf("booked = %d declarations, want exactly 1", booked)
	}
	if n := len(l.Dividends()); n != 1 {
		t.Fatalf("dividends = %d, want 1", n)
	}
}
