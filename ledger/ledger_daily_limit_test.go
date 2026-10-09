package ledger

import (
	"errors"
	"math"
	"sync"
	"testing"
	"time"
)

func mustSetDailyLimit(t *testing.T, l *Ledger, a AccountID, currency string, limit int64) {
	t.Helper()
	if err := l.SetDailyLimit(a, currency, limit); err != nil {
		t.Fatalf("SetDailyLimit(%q, %q, %d): %v", a, currency, limit, err)
	}
}

func postOutflow(t *testing.T, l *Ledger, id string, credit AccountID, amount int64, at time.Time) (JournalEntry, bool, error) {
	t.Helper()
	return l.Post(JournalEntry{
		ID:            id,
		DebitAccount:  "merchant",
		CreditAccount: credit,
		AmountCents:   amount,
		Currency:      "USD",
		CreatedAt:     at,
	})
}

// The daily limit is opt-in: without a configured limit, outflow is
// unbounded.
func TestDailyLimitUnsetIsUnlimited(t *testing.T) {
	l := New()
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		if _, _, err := postOutflow(t, l, "e"+string(rune('a'+i)), "payer", math.MaxInt64/10, at); err != nil {
			t.Fatalf("post %d without a limit: %v", i, err)
		}
	}
}

// Posts accumulate against the day's budget; hitting the limit exactly is
// allowed, exceeding it is rejected with ErrDailyLimitExceeded — and the
// rejection records nothing (no version bump, no balance change).
func TestDailyLimitAccumulatesAndRejects(t *testing.T) {
	l := New()
	mustSetDailyLimit(t, l, "payer", "USD", 1000)
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

	if _, _, err := postOutflow(t, l, "e1", "payer", 600, at); err != nil {
		t.Fatalf("post 600: %v", err)
	}
	if _, _, err := postOutflow(t, l, "e2", "payer", 400, at); err != nil {
		t.Fatalf("post 400 (exactly at limit): %v", err)
	}
	vBefore := l.version
	if _, _, err := postOutflow(t, l, "e3", "payer", 1, at); !errors.Is(err, ErrDailyLimitExceeded) {
		t.Fatalf("post 1 over the limit: err=%v, want ErrDailyLimitExceeded", err)
	}
	if l.version != vBefore {
		t.Fatalf("rejected post bumped version: %d -> %d", vBefore, l.version)
	}
	if got := l.Balance("payer"); got != -1000 {
		t.Fatalf("payer balance = %d, want -1000 (rejected post must not book)", got)
	}
	if _, ok := l.entries["e3"]; ok {
		t.Fatal("rejected entry e3 was journaled")
	}
}

// The window is the UTC calendar day: the next UTC day starts a fresh
// budget, even for the same account and currency.
func TestDailyLimitRollsOverAtUTCMidnight(t *testing.T) {
	l := New()
	mustSetDailyLimit(t, l, "payer", "USD", 1000)
	day1 := time.Date(2026, 10, 8, 23, 59, 59, 0, time.UTC)
	day2 := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)

	if _, _, err := postOutflow(t, l, "e1", "payer", 1000, day1); err != nil {
		t.Fatalf("day1 post: %v", err)
	}
	if _, _, err := postOutflow(t, l, "e2", "payer", 1, day1); !errors.Is(err, ErrDailyLimitExceeded) {
		t.Fatalf("day1 over-limit post: err=%v, want ErrDailyLimitExceeded", err)
	}
	// Same instant in a non-UTC zone still belongs to day1's UTC budget.
	pacific := day1.In(time.FixedZone("PDT", -7*3600))
	if _, _, err := postOutflow(t, l, "e3", "payer", 1, pacific); !errors.Is(err, ErrDailyLimitExceeded) {
		t.Fatalf("day1 (PDT clock) over-limit post: err=%v, want ErrDailyLimitExceeded", err)
	}
	if _, _, err := postOutflow(t, l, "e4", "payer", 1000, day2); err != nil {
		t.Fatalf("day2 post after rollover: %v", err)
	}
}

// Limits are per (account, currency): a USD limit never constrains EUR
// outflow, and another account's outflow never counts.
func TestDailyLimitIsPerAccountPerCurrency(t *testing.T) {
	l := New()
	mustSetDailyLimit(t, l, "payer", "USD", 1000)
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

	eur := JournalEntry{ID: "eur1", DebitAccount: "merchant", CreditAccount: "payer",
		AmountCents: 5000, Currency: "EUR", CreatedAt: at}
	if _, _, err := l.Post(eur); err != nil {
		t.Fatalf("EUR post under a USD limit: %v", err)
	}
	if _, _, err := postOutflow(t, l, "usd1", "other", 5000, at); err != nil {
		t.Fatalf("other account's post: %v", err)
	}
	if _, _, err := postOutflow(t, l, "usd2", "payer", 1000, at); err != nil {
		t.Fatalf("USD post at limit: %v", err)
	}
}

// Only the outflow (credit/payer) side counts: receiving money (debit leg)
// never consumes the account's own daily budget.
func TestDailyLimitCountsOutflowSideOnly(t *testing.T) {
	l := New()
	mustSetDailyLimit(t, l, "acct", "USD", 100)
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

	// acct receives 10_000 as the debit leg — not outflow.
	in := JournalEntry{ID: "in1", DebitAccount: "acct", CreditAccount: "funder",
		AmountCents: 10000, Currency: "USD", CreatedAt: at}
	if _, _, err := l.Post(in); err != nil {
		t.Fatalf("inbound post: %v", err)
	}
	// acct's own outflow budget is untouched: a full 100 still fits.
	if _, _, err := postOutflow(t, l, "out1", "acct", 100, at); err != nil {
		t.Fatalf("outflow at limit after large inbound: %v", err)
	}
}

// A transfer counts the payer's total outflow — amount plus the fee leg —
// against the payer's daily limit.
func TestDailyLimitTransferCountsAmountPlusFee(t *testing.T) {
	l := New(WithTransferFeeSchedule(
		[]FeeTier{{MinAmountCents: 0, RateBps: 1000}}, // 10% fee
		"fee-revenue",
	))
	mustSetDailyLimit(t, l, "payer", "USD", 1000)
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

	// 900 + 90 fee = 990 <= 1000: fits.
	r, err := l.PostTransfer(Transfer{ID: "t1", From: "payer", To: "payee",
		AmountCents: 900, Currency: "USD", CreatedAt: at})
	if err != nil {
		t.Fatalf("transfer 900 + 90 fee: %v", err)
	}
	if r.FeeCents != 90 {
		t.Fatalf("fee = %d, want 90", r.FeeCents)
	}
	// 100 + 10 fee = 110; 990 + 110 > 1000: rejected.
	_, err = l.PostTransfer(Transfer{ID: "t2", From: "payer", To: "payee",
		AmountCents: 100, Currency: "USD", CreatedAt: at})
	if !errors.Is(err, ErrDailyLimitExceeded) {
		t.Fatalf("transfer over the limit: err=%v, want ErrDailyLimitExceeded", err)
	}
	// SkipFee: 100 with no fee fits exactly (990 + 100 = 1090 > 1000
	// still — use a fresh day to isolate the fee math).
	day2 := at.Add(24 * time.Hour)
	_, err = l.PostTransfer(Transfer{ID: "t3", From: "payer", To: "payee",
		AmountCents: 1000, Currency: "USD", SkipFee: true, CreatedAt: day2})
	if err != nil {
		t.Fatalf("skip-fee transfer at limit: %v", err)
	}
}

// The daily-limit check runs after the idempotency replay check: replaying
// a key books nothing new, so it neither fails on a limit set after the
// original post nor double-counts the outflow.
func TestDailyLimitIdempotentReplayDoesNotDoubleCount(t *testing.T) {
	l := New()
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

	e := JournalEntry{ID: "e1", DebitAccount: "merchant", CreditAccount: "payer",
		AmountCents: 600, Currency: "USD", IdempotencyKey: "k1", CreatedAt: at}
	if _, dup, err := l.Post(e); err != nil || dup {
		t.Fatalf("first post: dup=%v err=%v", dup, err)
	}
	mustSetDailyLimit(t, l, "payer", "USD", 1000)
	if _, dup, err := l.Post(e); err != nil || !dup {
		t.Fatalf("replay after limit set: dup=%v err=%v, want clean replay", dup, err)
	}
	// The replay consumed no budget: 400 more still fits exactly.
	if _, _, err := postOutflow(t, l, "e2", "payer", 400, at); err != nil {
		t.Fatalf("post after replay: %v", err)
	}
}

// Limit management: negative limits are rejected, limits are structural
// (no version bump), and clearing restores unlimited outflow.
func TestDailyLimitManagement(t *testing.T) {
	l := New()
	if err := l.SetDailyLimit("payer", "USD", -1); !errors.Is(err, ErrInvalidDailyLimit) {
		t.Fatalf("negative limit: err=%v, want ErrInvalidDailyLimit", err)
	}
	if err := l.SetDailyLimit("payer", "usd", 500); !errors.Is(err, ErrInvalidCurrency) {
		t.Fatalf("lowercase currency: err=%v, want currency error", err)
	}
	vBefore := l.version
	mustSetDailyLimit(t, l, "payer", "USD", 500)
	if l.version != vBefore {
		t.Fatal("SetDailyLimit bumped the version; limits are structural")
	}
	if got, ok := l.DailyLimit("payer", "USD"); !ok || got != 500 {
		t.Fatalf("DailyLimit = (%d, %v), want (500, true)", got, ok)
	}
	if _, ok := l.DailyLimit("payer", "EUR"); ok {
		t.Fatal("DailyLimit reported a limit for an unconfigured currency")
	}
	l.ClearDailyLimit("payer", "USD")
	if _, ok := l.DailyLimit("payer", "USD"); ok {
		t.Fatal("ClearDailyLimit did not remove the limit")
	}
	l.ClearDailyLimit("payer", "USD") // no-op, must not panic

	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	if _, _, err := postOutflow(t, l, "e1", "payer", 100000, at); err != nil {
		t.Fatalf("post after clear: %v", err)
	}
}

// Overflow safety: a single posting larger than the limit is rejected by
// the amount > limit fast path, and huge amounts near MaxInt64 can never
// wrap the accumulation arithmetic.
func TestDailyLimitOverflowSafety(t *testing.T) {
	l := New()
	mustSetDailyLimit(t, l, "payer", "USD", math.MaxInt64-1)
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

	if _, _, err := postOutflow(t, l, "huge", "payer", math.MaxInt64, at); !errors.Is(err, ErrDailyLimitExceeded) {
		t.Fatalf("MaxInt64 post: err=%v, want ErrDailyLimitExceeded", err)
	}
	// Unlimited accounts accumulate saturating, never wrapping negative.
	l2 := New()
	if _, _, err := postOutflow(t, l2, "h1", "whale", math.MaxInt64, at); err != nil {
		t.Fatalf("unlimited MaxInt64 post: %v", err)
	}
	mustSetDailyLimit(t, l2, "whale", "USD", math.MaxInt64)
	if _, _, err := postOutflow(t, l2, "h2", "whale", 1, at); !errors.Is(err, ErrDailyLimitExceeded) {
		t.Fatalf("post after saturated accumulation: err=%v, want ErrDailyLimitExceeded", err)
	}
}

// Concurrency: the check-then-add is atomic under the write lock, so the
// day's booked outflow can never exceed the limit, however the posts
// interleave.
func TestDailyLimitConcurrent(t *testing.T) {
	l := New()
	mustSetDailyLimit(t, l, "payer", "USD", 1000)
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

	const workers = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	succeeded, rejected := 0, 0
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, err := l.Post(JournalEntry{
				ID:            "c" + string(rune('a'+i)) + string(rune('0'+i%10)),
				DebitAccount:  "merchant",
				CreditAccount: "payer",
				AmountCents:   100,
				Currency:      "USD",
				CreatedAt:     at,
			})
			mu.Lock()
			defer mu.Unlock()
			if errors.Is(err, ErrDailyLimitExceeded) {
				rejected++
			} else if err != nil {
				t.Errorf("worker %d: unexpected err %v", i, err)
			} else {
				succeeded++
			}
		}(i)
	}
	wg.Wait()
	if succeeded != 10 || rejected != 10 {
		t.Fatalf("succeeded=%d rejected=%d, want 10/10 (limit 1000, posts of 100)", succeeded, rejected)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Fatalf("accounting equation after concurrent posts: %v", err)
	}
}

// ParseDailyLimits: the env-var syntax parses strictly and fails fast.
func TestParseDailyLimits(t *testing.T) {
	got, err := ParseDailyLimits("cust-123:USD:100000,cust-456:EUR:50000")
	if err != nil {
		t.Fatalf("valid spec: %v", err)
	}
	want := []DailyLimit{
		{Account: "cust-123", Currency: "USD", LimitCents: 100000},
		{Account: "cust-456", Currency: "EUR", LimitCents: 50000},
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %d limits, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("limit %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if got, err := ParseDailyLimits(""); err != nil || got != nil {
		t.Fatalf("empty spec = (%v, %v), want (nil, nil)", got, err)
	}
	for _, bad := range []string{
		"cust-123:USD",          // missing limit
		"cust-123:USD:-5",       // negative limit
		"cust-123:USD:abc",      // non-integer limit
		"cust-123:usd:100",      // lowercase currency
		":USD:100",              // empty account
		"cust-123:USD:100:extra", // too many segments
	} {
		if _, err := ParseDailyLimits(bad); err == nil {
			t.Fatalf("spec %q parsed without error", bad)
		}
	}
}

// Audit surface: TrialBalance reports the default-currency limit and
// Reconcile lists every configured limit.
func TestDailyLimitAuditSurface(t *testing.T) {
	l := New()
	mustSetDailyLimit(t, l, "payer", "USD", 1000)
	mustSetDailyLimit(t, l, "payer", "EUR", 2000)

	tb := l.TrialBalance("payer")
	if tb.DailyLimitCents != 1000 {
		t.Fatalf("TrialBalance.DailyLimitCents = %d, want 1000", tb.DailyLimitCents)
	}
	if l.TrialBalance("other").DailyLimitCents != 0 {
		t.Fatal("unconfigured account reports a nonzero daily limit")
	}

	report := l.Reconcile(time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC))
	want := []DailyLimit{
		{Account: "payer", Currency: "EUR", LimitCents: 2000},
		{Account: "payer", Currency: "USD", LimitCents: 1000},
	}
	if len(report.DailyLimits) != len(want) {
		t.Fatalf("reconcile daily_limits = %+v, want %+v", report.DailyLimits, want)
	}
	for i := range want {
		if report.DailyLimits[i] != want[i] {
			t.Fatalf("daily_limits[%d] = %+v, want %+v", i, report.DailyLimits[i], want[i])
		}
	}
}
