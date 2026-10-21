package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func velocityTestLedger(t *testing.T) *Ledger {
	t.Helper()
	return New()
}

func postAt(t *testing.T, l *Ledger, id, debit, credit string, cents int64, at time.Time) {
	t.Helper()
	e := JournalEntry{
		ID:            id,
		DebitAccount:  AccountID(debit),
		CreditAccount: AccountID(credit),
		AmountCents:   cents,
		Currency:      "USD",
		CreatedAt:     at,
	}
	if _, dup, err := l.Post(e); err != nil || dup {
		t.Fatalf("Post %s = dup=%v err=%v", id, dup, err)
	}
}

func TestSetVelocityLimitValidation(t *testing.T) {
	l := velocityTestLedger(t)
	if err := l.SetVelocityLimit("", "USD", time.Hour, 10, 1000); !errors.Is(err, ErrInvalidVelocityLimit) {
		t.Fatalf("empty account: %v", err)
	}
	if err := l.SetVelocityLimit("a", "USD", 0, 10, 1000); !errors.Is(err, ErrInvalidVelocityLimit) {
		t.Fatalf("zero window: %v", err)
	}
	if err := l.SetVelocityLimit("a", "USD", -time.Hour, 10, 1000); !errors.Is(err, ErrInvalidVelocityLimit) {
		t.Fatalf("negative window: %v", err)
	}
	if err := l.SetVelocityLimit("a", "USD", 31*24*time.Hour, 10, 1000); !errors.Is(err, ErrInvalidVelocityLimit) {
		t.Fatalf("window over cap: %v", err)
	}
	if err := l.SetVelocityLimit("a", "USD", time.Hour, 0, 0); !errors.Is(err, ErrInvalidVelocityLimit) {
		t.Fatalf("no positive dimension: %v", err)
	}
	if err := l.SetVelocityLimit("a", "USD", time.Hour, -1, -1); !errors.Is(err, ErrInvalidVelocityLimit) {
		t.Fatalf("both unlimited: %v", err)
	}
	if err := l.SetVelocityLimit("a", "US", time.Hour, 10, 1000); !errors.Is(err, ErrInvalidVelocityLimit) {
		t.Fatalf("bad currency: %v", err)
	}
	// Exactly one positive dimension is fine.
	if err := l.SetVelocityLimit("a", "USD", time.Hour, 10, 0); err != nil {
		t.Fatalf("count-only limit: %v", err)
	}
	if err := l.SetVelocityLimit("b", "USD", time.Hour, 0, 1000); err != nil {
		t.Fatalf("amount-only limit: %v", err)
	}
}

func TestVelocityLimitCountDimension(t *testing.T) {
	l := velocityTestLedger(t)
	base := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	if err := l.SetVelocityLimit("payer", "USD", time.Hour, 2, 0); err != nil {
		t.Fatal(err)
	}
	postAt(t, l, "v1", "payee1", "payer", 100, base)
	postAt(t, l, "v2", "payee2", "payer", 100, base.Add(10*time.Minute))
	// Third outflow posting inside the hour must be rejected.
	_, _, err := l.Post(JournalEntry{ID: "v3", DebitAccount: "payee3", CreditAccount: "payer", AmountCents: 100, Currency: "USD", CreatedAt: base.Add(20 * time.Minute)})
	if !errors.Is(err, ErrVelocityLimitExceeded) {
		t.Fatalf("third posting: want ErrVelocityLimitExceeded, got %v", err)
	}
	// A posting after the window slides past the first event is fine.
	postAt(t, l, "v4", "payee4", "payer", 100, base.Add(61*time.Minute))
	// Inflow to a different account is unaffected by payer's outflow cap.
	postAt(t, l, "v5", "payer", "other", 100, base.Add(62*time.Minute))
}

func TestVelocityLimitAmountDimension(t *testing.T) {
	l := velocityTestLedger(t)
	base := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	if err := l.SetVelocityLimit("payer", "USD", time.Hour, 0, 1000); err != nil {
		t.Fatal(err)
	}
	postAt(t, l, "a1", "payee", "payer", 600, base)
	postAt(t, l, "a2", "payee", "payer", 400, base.Add(time.Minute))
	// 600+400 = 1000 used; one more cent exceeds.
	_, _, err := l.Post(JournalEntry{ID: "a3", DebitAccount: "payee", CreditAccount: "payer", AmountCents: 1, Currency: "USD", CreatedAt: base.Add(2 * time.Minute)})
	if !errors.Is(err, ErrVelocityLimitExceeded) {
		t.Fatalf("over-amount posting: want ErrVelocityLimitExceeded, got %v", err)
	}
}

func TestVelocityLimitDirectionsIndependent(t *testing.T) {
	l := velocityTestLedger(t)
	base := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	// One posting per direction per hour: the inflow and outflow
	// windows are independent — filling one does not consume the
	// other.
	if err := l.SetVelocityLimit("acct", "USD", time.Hour, 1, 0); err != nil {
		t.Fatal(err)
	}
	postAt(t, l, "i1", "acct", "other", 5000, base)                    // inflow #1
	postAt(t, l, "o1", "other", "acct", 100, base.Add(time.Minute))    // outflow #1 (independent window)
	if _, _, err := l.Post(JournalEntry{ID: "o2", DebitAccount: "other", CreditAccount: "acct", AmountCents: 100, Currency: "USD", CreatedAt: base.Add(2 * time.Minute)}); !errors.Is(err, ErrVelocityLimitExceeded) {
		t.Fatalf("second outflow: want ErrVelocityLimitExceeded, got %v", err)
	}
	if _, _, err := l.Post(JournalEntry{ID: "i2", DebitAccount: "acct", CreditAccount: "other", AmountCents: 100, Currency: "USD", CreatedAt: base.Add(3 * time.Minute)}); !errors.Is(err, ErrVelocityLimitExceeded) {
		t.Fatalf("second inflow: want ErrVelocityLimitExceeded, got %v", err)
	}
}

func TestVelocityLimitIdempotentReplayNotCounted(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	l := New(WithVelocityClock(func() time.Time { return now }))
	base := now
	if err := l.SetVelocityLimit("payer", "USD", time.Hour, 1, 0); err != nil {
		t.Fatal(err)
	}
	e := JournalEntry{ID: "r1", DebitAccount: "payee", CreditAccount: "payer", AmountCents: 100, Currency: "USD", IdempotencyKey: "rk1", CreatedAt: base}
	if _, dup, err := l.Post(e); err != nil || dup {
		t.Fatalf("first post: dup=%v err=%v", dup, err)
	}
	// Replay the same key: must return the original entry, not count
	// toward the window, and not trip the limit.
	if _, dup, err := l.Post(e); err != nil || !dup {
		t.Fatalf("replay: dup=%v err=%v", dup, err)
	}
	// The window still holds exactly one outflow posting.
	usage, ok := l.VelocityUsageOf("payer", "USD", VelocityOutflow)
	if !ok {
		t.Fatal("usage missing")
	}
	if usage.Count != 1 {
		t.Fatalf("replay counted toward window: count=%d", usage.Count)
	}
	// One more distinct posting must be rejected (1 already counted).
	if _, _, err := l.Post(JournalEntry{ID: "r2", DebitAccount: "payee", CreditAccount: "payer", AmountCents: 100, Currency: "USD", CreatedAt: base.Add(time.Minute)}); !errors.Is(err, ErrVelocityLimitExceeded) {
		t.Fatalf("second distinct posting: want ErrVelocityLimitExceeded, got %v", err)
	}
}

func TestVelocityLimitTransferGates(t *testing.T) {
	l := velocityTestLedger(t)
	base := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	// Seed balances.
	postAt(t, l, "seed", "payer", "bank", 100000, base)
	if err := l.SetVelocityLimit("payer", "USD", time.Hour, 0, 1000); err != nil {
		t.Fatal(err)
	}
	// First transfer: 900 outflow OK.
	if _, err := l.PostTransfer(Transfer{ID: "t1", From: "payer", To: "payee", AmountCents: 900, Currency: "USD", CreatedAt: base.Add(time.Minute)}); err != nil {
		t.Fatalf("transfer t1: %v", err)
	}
	// Second transfer would take the window to 1700 > 1000.
	if _, err := l.PostTransfer(Transfer{ID: "t2", From: "payer", To: "payee", AmountCents: 800, Currency: "USD", CreatedAt: base.Add(2 * time.Minute)}); !errors.Is(err, ErrVelocityLimitExceeded) {
		t.Fatalf("transfer t2: want ErrVelocityLimitExceeded, got %v", err)
	}
}

func TestVelocityLimitTransferInflowGate(t *testing.T) {
	l := velocityTestLedger(t)
	base := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	postAt(t, l, "seed", "payer", "bank", 100000, base)
	// Payee inflow capped at 1 posting/hour.
	if err := l.SetVelocityLimit("payee", "USD", time.Hour, 1, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := l.PostTransfer(Transfer{ID: "t1", From: "payer", To: "payee", AmountCents: 100, Currency: "USD", CreatedAt: base.Add(time.Minute)}); err != nil {
		t.Fatalf("transfer t1: %v", err)
	}
	if _, err := l.PostTransfer(Transfer{ID: "t2", From: "payer", To: "payee", AmountCents: 100, Currency: "USD", CreatedAt: base.Add(2 * time.Minute)}); !errors.Is(err, ErrVelocityLimitExceeded) {
		t.Fatalf("transfer t2 (payee inflow): want ErrVelocityLimitExceeded, got %v", err)
	}
}

func TestVelocityLimitReversalGates(t *testing.T) {
	// ReverseEntry stamps the reversal with the real clock, so the
	// windows here are anchored at now, not at a fixed fixture date.
	newLimited := func(t *testing.T) (*Ledger, time.Time) {
		t.Helper()
		l := velocityTestLedger(t)
		base := time.Now().Add(-time.Minute).Truncate(time.Second)
		// Windows start when the limit is configured: set it first.
		if err := l.SetVelocityLimit("payer", "USD", time.Hour, 1, 0); err != nil {
			t.Fatal(err)
		}
		return l, base
	}
	// (a) The reversal is a new journal row: reversing orig2 flips the
	// legs to {debit: y, credit: payer}, booking payer outflow #2 on
	// top of orig1's outflow #1 — rejected.
	l, base := newLimited(t)
	postAt(t, l, "orig1", "x", "payer", 500, base)          // payer outflow #1
	postAt(t, l, "orig2", "payer", "y", 500, base.Add(time.Second)) // payer inflow (other window)
	if _, _, err := l.ReverseEntry("orig2", "rev1"); !errors.Is(err, ErrVelocityLimitExceeded) {
		t.Fatalf("(a) reversal: want ErrVelocityLimitExceeded, got %v", err)
	}
	// (b) Direction specificity: reversing orig1 books payer INFLOW —
	// the inflow window is empty (orig2's inflow belongs to scenario
	// (a)'s ledger), so it is admitted.
	l2, base2 := newLimited(t)
	postAt(t, l2, "o1", "payer", "y", 500, base2) // payer inflow #1
	if _, _, err := l2.ReverseEntry("o1", "rev"); err != nil {
		t.Fatalf("(b) inflow-direction reversal should be admitted: %v", err)
	}
}

func TestVelocityLimitBatchRecordsEvents(t *testing.T) {
	l := velocityTestLedger(t)
	base := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	// Batch has no velocity gate (mirroring the daily-limit scope),
	// but its committed rows feed the windows through the commit
	// funnel — a subsequent Post must see them. The window starts
	// when the limit is configured, so set it before the batch.
	if err := l.SetVelocityLimit("payer", "USD", time.Hour, 1, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := l.PostBatch(Batch{
		ID: "b1",
		Entries: []JournalEntry{
			{ID: "b1e1", DebitAccount: "x", CreditAccount: "payer", AmountCents: 100, Currency: "USD", CreatedAt: base},
		},
	}); err != nil {
		t.Fatalf("batch: %v", err)
	}
	if _, _, err := l.Post(JournalEntry{ID: "p1", DebitAccount: "x", CreditAccount: "payer", AmountCents: 100, Currency: "USD", CreatedAt: base.Add(time.Minute)}); !errors.Is(err, ErrVelocityLimitExceeded) {
		t.Fatalf("post after batch: want ErrVelocityLimitExceeded, got %v", err)
	}
}

func TestClearVelocityLimit(t *testing.T) {
	l := velocityTestLedger(t)
	base := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	if err := l.SetVelocityLimit("payer", "USD", time.Hour, 1, 0); err != nil {
		t.Fatal(err)
	}
	postAt(t, l, "c1", "payee", "payer", 100, base)
	l.ClearVelocityLimit("payer", "USD")
	if _, ok := l.VelocityLimit("payer", "USD"); ok {
		t.Fatal("limit should be cleared")
	}
	// Posting freely again, and the old window events are gone: a new
	// limit starts from a clean window.
	postAt(t, l, "c2", "payee", "payer", 100, base.Add(time.Minute))
	if err := l.SetVelocityLimit("payer", "USD", time.Hour, 2, 0); err != nil {
		t.Fatal(err)
	}
	postAt(t, l, "c3", "payee", "payer", 100, base.Add(2*time.Minute))
	postAt(t, l, "c4", "payee", "payer", 100, base.Add(3*time.Minute))
	// Clearing a missing limit is a no-op.
	l.ClearVelocityLimit("nobody", "USD")
}

func TestVelocityLimitPerCurrency(t *testing.T) {
	l := velocityTestLedger(t)
	base := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	if err := l.SetVelocityLimit("payer", "USD", time.Hour, 1, 0); err != nil {
		t.Fatal(err)
	}
	postAt(t, l, "m1", "payee", "payer", 100, base)
	// EUR has no limit configured: unlimited.
	e := JournalEntry{ID: "m2", DebitAccount: "payee", CreditAccount: "payer", AmountCents: 100, Currency: "EUR", CreatedAt: base.Add(time.Minute)}
	if _, dup, err := l.Post(e); err != nil || dup {
		t.Fatalf("EUR post: dup=%v err=%v", dup, err)
	}
}

func TestVelocityUsageOfInjectedClock(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	l := New(WithVelocityClock(func() time.Time { return now }))
	base := now.Add(-30 * time.Minute)
	if err := l.SetVelocityLimit("payer", "USD", time.Hour, 10, 5000); err != nil {
		t.Fatal(err)
	}
	postAt(t, l, "u1", "payee", "payer", 100, base)
	postAt(t, l, "u2", "payee", "payer", 200, base.Add(10*time.Minute))
	usage, ok := l.VelocityUsageOf("payer", "USD", VelocityOutflow)
	if !ok {
		t.Fatal("usage missing")
	}
	if usage.Count != 2 || usage.AmountCents != 300 {
		t.Fatalf("usage = %+v, want count=2 amount=300", usage)
	}
	if !usage.WindowStart.Equal(now.Add(-time.Hour)) || !usage.WindowEnd.Equal(now) {
		t.Fatalf("window = %v..%v", usage.WindowStart, usage.WindowEnd)
	}
	// Advance the clock past the window: usage drains to zero.
	now = now.Add(2 * time.Hour)
	usage, ok = l.VelocityUsageOf("payer", "USD", VelocityOutflow)
	if !ok {
		t.Fatal("usage missing after clock advance")
	}
	if usage.Count != 0 || usage.AmountCents != 0 {
		t.Fatalf("usage after window slide = %+v, want zeros", usage)
	}
	// No limit configured: false.
	if _, ok := l.VelocityUsageOf("ghost", "USD", VelocityOutflow); ok {
		t.Fatal("usage for unconfigured account must be false")
	}
}

func TestVelocityLimitOverflowSafe(t *testing.T) {
	l := velocityTestLedger(t)
	base := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	// A limit near MaxInt64 must not overflow the check arithmetic.
	if err := l.SetVelocityLimit("payer", "USD", time.Hour, 0, 1<<62); err != nil {
		t.Fatal(err)
	}
	postAt(t, l, "w1", "payee", "payer", 1<<61, base)
	// Another 1<<61 fits (2^62 total); 1<<62+1 does not.
	if _, _, err := l.Post(JournalEntry{ID: "w2", DebitAccount: "payee", CreditAccount: "payer", AmountCents: 1 << 61, Currency: "USD", CreatedAt: base.Add(time.Minute)}); err != nil {
		t.Fatalf("fitting post: %v", err)
	}
	if _, _, err := l.Post(JournalEntry{ID: "w3", DebitAccount: "payee", CreditAccount: "payer", AmountCents: 1, Currency: "USD", CreatedAt: base.Add(2 * time.Minute)}); !errors.Is(err, ErrVelocityLimitExceeded) {
		t.Fatalf("overflowing post: want ErrVelocityLimitExceeded, got %v", err)
	}
}

func TestParseVelocityLimits(t *testing.T) {
	limits, err := ParseVelocityLimits("cust-123:USD:10m:50:100000,cust-456:EUR:24h:500:5000000")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(limits) != 2 {
		t.Fatalf("got %d limits", len(limits))
	}
	if limits[0].Account != "cust-123" || limits[0].Currency != "USD" || limits[0].Window != 10*time.Minute || limits[0].MaxCount != 50 || limits[0].MaxAmountCents != 100000 {
		t.Fatalf("limit[0] = %+v", limits[0])
	}
	if limits[1].Window != 24*time.Hour {
		t.Fatalf("limit[1].Window = %v", limits[1].Window)
	}
	// Unlimited dimensions via 0 (count-only or amount-only limits).
	limits, err = ParseVelocityLimits("a:USD:1h:0:1000")
	if err != nil {
		t.Fatalf("parse count-unlimited: %v", err)
	}
	if limits[0].MaxCount != 0 || limits[0].MaxAmountCents != 1000 {
		t.Fatalf("count-unlimited = %+v", limits[0])
	}
	limits, err = ParseVelocityLimits("a:USD:1h:50:-1")
	if err != nil {
		t.Fatalf("parse amount-unlimited: %v", err)
	}
	if limits[0].MaxCount != 50 || limits[0].MaxAmountCents != -1 {
		t.Fatalf("amount-unlimited = %+v", limits[0])
	}
	for _, raw := range []string{
		"a:USD:10m:50",            // wrong arity
		":USD:10m:50:100",         // empty account
		"a:US:10m:50:100",         // bad currency
		"a:USD:ten-minutes:50:100", // bad window
		"a:USD:10m:x:100",         // bad count
		"a:USD:10m:50:x",          // bad amount
		"a:USD:0s:50:100",         // zero window
		"a:USD:1000h:50:100",      // window over cap
		"a:USD:10m:0:0",           // no positive dimension
	} {
		if _, err := ParseVelocityLimits(raw); err == nil {
			t.Fatalf("ParseVelocityLimits(%q) should fail", raw)
		}
	}
	if limits, err := ParseVelocityLimits(""); err != nil || len(limits) != 0 {
		t.Fatalf("empty = %v %v", limits, err)
	}
}

func TestVelocityLimitBreachAudit(t *testing.T) {
	al := mustAuditLog(t)
	defer al.Close()
	l := New(WithAuditLog(al))
	base := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	if err := l.SetVelocityLimit("payer", "USD", time.Hour, 1, 0); err != nil {
		t.Fatal(err)
	}
	postAt(t, l, "b1", "payee", "payer", 100, base)
	if _, _, err := l.Post(JournalEntry{ID: "b2", DebitAccount: "payee", CreditAccount: "payer", AmountCents: 100, Currency: "USD", CreatedAt: base.Add(time.Minute)}); !errors.Is(err, ErrVelocityLimitExceeded) {
		t.Fatalf("want ErrVelocityLimitExceeded, got %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// The breach event is stamped with the real clock (emitAudit fills
	// time.Now when the event carries no timestamp), so read today's
	// audit file, not the fixture date's.
	events, corrupt, err := ReadAuditLog(al.Dir(), time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt events = %d", corrupt)
	}
	found := false
	for _, ev := range events {
		if ev.Op == "velocity_limit_breach" {
			found = true
			if ev.Details["dimension"] != "count" || ev.Details["direction"] != "outflow" {
				t.Fatalf("breach event = %+v", ev.Details)
			}
		}
	}
	if !found {
		t.Fatal("no velocity_limit_breach audit event")
	}
}

func TestVelocityLimitConcurrentPosts(t *testing.T) {
	l := velocityTestLedger(t)
	base := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	// 20 postings allowed in the window; 40 goroutines race to post.
	if err := l.SetVelocityLimit("payer", "USD", time.Hour, 20, 0); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 40)
	for i := 0; i < 40; i++ {
		go func(i int) {
			_, _, err := l.Post(JournalEntry{
				ID:            strings.Repeat("x", 8) + string(rune('a'+i/26)) + string(rune('a'+i%26)),
				DebitAccount:  "payee",
				CreditAccount: "payer",
				AmountCents:   1,
				Currency:      "USD",
				CreatedAt:     base.Add(time.Duration(i) * time.Second),
			})
			done <- err
		}(i)
	}
	var rejected, ok int
	for i := 0; i < 40; i++ {
		if err := <-done; errors.Is(err, ErrVelocityLimitExceeded) {
			rejected++
		} else if err == nil {
			ok++
		} else {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	// Exactly 20 admitted, 20 rejected — the check-then-record is
	// atomic under the write lock.
	if ok != 20 || rejected != 20 {
		t.Fatalf("admitted=%d rejected=%d, want 20/20", ok, rejected)
	}
}

func TestVelocityLimitSnapshotRoundTrip(t *testing.T) {
	l := velocityTestLedger(t)
	base := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	if err := l.SetVelocityLimit("payer", "USD", time.Hour, 10, 5000); err != nil {
		t.Fatal(err)
	}
	if err := l.SetVelocityLimit("payer", "EUR", 24*time.Hour, 0, 999); err != nil {
		t.Fatal(err)
	}
	postAt(t, l, "s1", "payee", "payer", 100, base)
	var sb strings.Builder
	if err := l.ExportSnapshot(&sb); err != nil {
		t.Fatalf("export: %v", err)
	}
	l2, err := ImportSnapshot(strings.NewReader(sb.String()))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	got := l2.VelocityLimits()
	if len(got) != 2 {
		t.Fatalf("restored %d limits, want 2", len(got))
	}
	lim, ok := l2.VelocityLimit("payer", "USD")
	if !ok || lim.Window != time.Hour || lim.MaxCount != 10 || lim.MaxAmountCents != 5000 {
		t.Fatalf("restored USD limit = %+v ok=%v", lim, ok)
	}
	// The windows restart empty at the restore point: the pre-restore
	// posting does not count toward the fresh window.
	postAt(t, l2, "r1", "payee", "payer", 100, base.Add(time.Hour))
}
