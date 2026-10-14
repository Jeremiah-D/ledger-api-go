package ledger

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

func mustSetAccountBudget(t *testing.T, l *Ledger, a AccountID, currency string, budget int64) {
	t.Helper()
	if err := l.SetAccountBudget(a, currency, budget); err != nil {
		t.Fatalf("SetAccountBudget(%q, %q, %d): %v", a, currency, budget, err)
	}
}

func postSpend(t *testing.T, l *Ledger, id string, credit AccountID, amount int64, at time.Time) (JournalEntry, bool, error) {
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

// The monthly budget is opt-in: without a configured budget, spend is
// unbounded.
func TestBudgetUnsetIsUnlimited(t *testing.T) {
	l := New()
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		if _, _, err := postSpend(t, l, "e-unl-"+string(rune('a'+i)), "payer", math.MaxInt64/10, at); err != nil {
			t.Fatalf("post %d without a budget: %v", i, err)
		}
	}
}

// Posts accumulate against the month's budget; hitting the budget exactly
// is allowed, exceeding it is rejected with ErrBudgetExceeded — and the
// rejection records nothing (no version bump, no balance change).
func TestBudgetAccumulatesAndRejects(t *testing.T) {
	l := New()
	mustSetAccountBudget(t, l, "payer", "USD", 1000)
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

	if _, _, err := postSpend(t, l, "e1", "payer", 600, at); err != nil {
		t.Fatalf("post 600: %v", err)
	}
	if _, _, err := postSpend(t, l, "e2", "payer", 400, at); err != nil {
		t.Fatalf("post 400 (exactly at budget): %v", err)
	}
	vBefore := l.version
	if _, _, err := postSpend(t, l, "e3", "payer", 1, at); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("post 1 over budget: err=%v, want ErrBudgetExceeded", err)
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
	// A single posting larger than the whole budget is rejected without
	// adding: the comparison never computes used + amount, so it cannot
	// overflow even on absurd amounts.
	if _, _, err := postSpend(t, l, "e4", "payer", math.MaxInt64, at); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("post MaxInt64: err=%v, want ErrBudgetExceeded", err)
	}
}

// The window is the UTC calendar month: the next UTC month starts a fresh
// budget, even for the same account and currency. Spend is keyed by the
// entry's own timestamp, so backdated entries count toward their own
// month.
func TestBudgetResetsEachUTCMonth(t *testing.T) {
	l := New()
	mustSetAccountBudget(t, l, "payer", "USD", 1000)
	oct := time.Date(2026, 10, 31, 23, 59, 59, 0, time.UTC)
	nov := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

	if _, _, err := postSpend(t, l, "e-oct", "payer", 1000, oct); err != nil {
		t.Fatalf("october post: %v", err)
	}
	if _, _, err := postSpend(t, l, "e-nov", "payer", 1000, nov); err != nil {
		t.Fatalf("november post (fresh month): %v", err)
	}
	if _, _, err := postSpend(t, l, "e-nov2", "payer", 1, nov); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("second november post: err=%v, want ErrBudgetExceeded", err)
	}
	// Backdated into October still counts toward October's budget.
	if _, _, err := postSpend(t, l, "e-oct2", "payer", 1, oct); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("backdated october post: err=%v, want ErrBudgetExceeded", err)
	}
	if got := l.MonthlySpend("payer", "USD", "2026-10"); got != 1000 {
		t.Fatalf("MonthlySpend(2026-10) = %d, want 1000", got)
	}
	if got := l.MonthlySpend("payer", "USD", "2026-11"); got != 1000 {
		t.Fatalf("MonthlySpend(2026-11) = %d, want 1000", got)
	}
}

// Budgets are per (account, currency): a USD budget does not constrain
// EUR spend, and one account's budget does not constrain another's.
func TestBudgetIsPerAccountPerCurrency(t *testing.T) {
	l := New()
	mustSetAccountBudget(t, l, "payer", "USD", 100)
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

	if _, _, err := l.Post(JournalEntry{ID: "e-eur", DebitAccount: "merchant", CreditAccount: "payer", AmountCents: 10000, Currency: "EUR", CreatedAt: at}); err != nil {
		t.Fatalf("EUR post with only a USD budget: %v", err)
	}
	if _, _, err := postSpend(t, l, "e-other", "other", 10000, at); err != nil {
		t.Fatalf("other account post: %v", err)
	}
}

// A transfer's budget spend is its total outflow (amount + fee leg): the
// payer's cumulative spend for the month may not exceed the budget.
func TestBudgetTransferCountsAmountPlusFee(t *testing.T) {
	l := New()
	seedBalance(t, l, "alice", 100000)
	mustSetAccountBudget(t, l, "alice", "USD", 1025)

	rcpt, err := l.PostTransfer(Transfer{
		ID: "tx1", From: "alice", To: "bob",
		AmountCents: 1000, FeeCents: 25, FeeAccount: "fees",
		CreatedAt:   time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("transfer 1000+25 at budget 1025: %v", err)
	}
	if rcpt.Duplicate {
		t.Fatal("first transfer must not be a duplicate")
	}
	if _, err := l.PostTransfer(Transfer{
		ID: "tx2", From: "alice", To: "bob", AmountCents: 1,
		CreatedAt: time.Date(2026, 10, 8, 13, 0, 0, 0, time.UTC),
	}); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("transfer 1 over budget: err=%v, want ErrBudgetExceeded", err)
	}
	if got := l.MonthlySpend("alice", "USD", "2026-10"); got != 1025 {
		t.Fatalf("MonthlySpend = %d, want 1025 (amount + fee)", got)
	}
}

// A cross-currency transfer's budget spend is the source-currency total
// outflow (amount + fee): the converted leg books through the FX
// account and never counts as the payer's spend.
func TestBudgetFXTransferCountsSourceCurrencyOutflow(t *testing.T) {
	l := New(WithFXAccount("fx-pnl"))
	if _, _, err := l.Post(JournalEntry{ID: "seed", DebitAccount: "payer", CreditAccount: "bank", AmountCents: 100000, Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	mustSetRate(t, l, "USD", "EUR", 108, 100)
	mustSetAccountBudget(t, l, "payer", "USD", 10000)

	if _, err := l.PostTransfer(Transfer{
		ID: "fx1", From: "payer", To: "payee", AmountCents: 10000,
		Currency: "USD", ToCurrency: "EUR",
		CreatedAt: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("FX transfer 10000 at budget 10000: %v", err)
	}
	// The fx-pnl account has no budget: its 10800 EUR leg booked fine,
	// and the payer's USD spend is exactly the 10000 source outflow.
	if got := l.MonthlySpend("payer", "USD", "2026-10"); got != 10000 {
		t.Fatalf("payer MonthlySpend = %d, want 10000", got)
	}
	if _, err := l.PostTransfer(Transfer{
		ID: "fx2", From: "payer", To: "payee", AmountCents: 1,
		Currency: "USD", ToCurrency: "EUR",
		CreatedAt: time.Date(2026, 10, 8, 13, 0, 0, 0, time.UTC),
	}); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("FX transfer over budget: err=%v, want ErrBudgetExceeded", err)
	}
}

// Idempotent replays book nothing new, so they consume no budget —
// replaying a key posted before the budget was set (or exhausted)
// returns the original entry.
func TestBudgetIdempotentReplayConsumesNothing(t *testing.T) {
	l := New()
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	if _, _, err := l.Post(JournalEntry{ID: "e1", DebitAccount: "merchant", CreditAccount: "payer", AmountCents: 900, Currency: "USD", IdempotencyKey: "k1", CreatedAt: at}); err != nil {
		t.Fatalf("post 900: %v", err)
	}
	// Install an exhausted budget after the fact: the replay must still
	// succeed because it books nothing new. (SetAccountBudget rejects a
	// budget below current usage, so clear is not needed — the replay
	// predates the budget.)
	mustSetAccountBudget(t, l, "payer", "USD", 900)
	retry := JournalEntry{ID: "e1-retry", DebitAccount: "merchant", CreditAccount: "payer", AmountCents: 900, Currency: "USD", IdempotencyKey: "k1", CreatedAt: at}
	got, dup, err := l.Post(retry)
	if err != nil || !dup {
		t.Fatalf("replay = dup=%v err=%v, want dup=true err=nil", dup, err)
	}
	if got.ID != "e1" {
		t.Fatalf("replay returned entry %q, want e1", got.ID)
	}
	if spent := l.MonthlySpend("payer", "USD", "2026-10"); spent != 900 {
		t.Fatalf("MonthlySpend after replay = %d, want 900", spent)
	}
	// A fresh posting with the same key shape is still gated.
	if _, _, err := postSpend(t, l, "e2", "payer", 1, at); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("post over exhausted budget: err=%v, want ErrBudgetExceeded", err)
	}
}

// Pending dual-control reviews (LG-42) reserve the monthly budget at
// creation: the reserved outflow counts in the month's used figure from
// creation, and approval settles against the already-reserved budget
// without consuming it twice.
func TestBudgetPendingReviewReserves(t *testing.T) {
	l := New(WithReviewThreshold(1000))
	seedBalance(t, l, "alice", 100000)
	mustSetAccountBudget(t, l, "alice", "USD", 1500)
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

	rcpt, err := l.PostTransfer(Transfer{
		ID: "tx-rev", From: "alice", To: "bob", AmountCents: 1000,
		CreatedAt: at,
	})
	if err != nil {
		t.Fatalf("PostTransfer into review: %v", err)
	}
	if rcpt.ReviewStatus != ReviewStatusPending {
		t.Fatalf("ReviewStatus = %q, want pending_review", rcpt.ReviewStatus)
	}
	if got := l.MonthlySpend("alice", "USD", "2026-10"); got != 1000 {
		t.Fatalf("MonthlySpend after review creation = %d, want 1000 (reserved)", got)
	}
	// The reservation holds the budget: a further 501 would exceed 1500.
	if _, err := l.PostTransfer(Transfer{ID: "tx2", From: "alice", To: "bob", AmountCents: 501, CreatedAt: at}); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("transfer over reserved budget: err=%v, want ErrBudgetExceeded", err)
	}
	// Approval settles against the reservation — no double count.
	if _, err := l.ApproveTransferReview("tx-rev", "op-alice"); err != nil {
		t.Fatalf("ApproveTransferReview: %v", err)
	}
	if got := l.MonthlySpend("alice", "USD", "2026-10"); got != 1000 {
		t.Fatalf("MonthlySpend after approval = %d, want 1000 (no double count)", got)
	}
}

// Batch entries consume the month's budget sequentially: an earlier
// entry's spend counts against later entries in the same batch, and a
// batch that would breach the budget rejects atomically.
func TestBudgetBatchSequential(t *testing.T) {
	l := New()
	mustSetAccountBudget(t, l, "payer", "USD", 1000)
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

	if _, err := l.PostBatch(Batch{ID: "b-ok", Entries: []JournalEntry{
		{ID: "b-ok-1", DebitAccount: "m", CreditAccount: "payer", AmountCents: 600, Currency: "USD", CreatedAt: at},
		{ID: "b-ok-2", DebitAccount: "m", CreditAccount: "payer", AmountCents: 400, Currency: "USD", CreatedAt: at},
	}}); err != nil {
		t.Fatalf("batch 600+400 at budget 1000: %v", err)
	}
	vBefore := l.version
	if _, err := l.PostBatch(Batch{ID: "b-bad", Entries: []JournalEntry{
		{ID: "b-bad-1", DebitAccount: "m", CreditAccount: "payer", AmountCents: 1, Currency: "USD", CreatedAt: at},
	}}); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("batch over budget: err=%v, want ErrBudgetExceeded", err)
	}
	if l.version != vBefore {
		t.Fatalf("rejected batch bumped version: %d -> %d", vBefore, l.version)
	}
	if got := l.MonthlySpend("payer", "USD", "2026-10"); got != 1000 {
		t.Fatalf("MonthlySpend = %d, want 1000 (rejected batch booked nothing)", got)
	}
}

// Sweep source legs are spend: a sweep whose source leg would breach the
// source account's monthly budget is rejected with ErrBudgetExceeded,
// and a sweep within budget commits and consumes it.
func TestBudgetSweepRejects(t *testing.T) {
	l := New()
	seedBalanceIn(t, l, "src", 5000, "USD")
	mustSetAccountBudget(t, l, "src", "USD", 3000)
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

	if _, err := l.PostSweep(Sweep{ID: "sw1", From: []AccountID{"src"}, To: "dst", CreatedAt: at}); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("sweep 5000 over budget 3000: err=%v, want ErrBudgetExceeded", err)
	}
	if got := l.Balance("src"); got != 5000 {
		t.Fatalf("src balance = %d, want 5000 (rejected sweep booked nothing)", got)
	}
	// A sweep within budget commits and consumes it.
	mustSetAccountBudget(t, l, "src", "USD", 6000)
	if _, err := l.PostSweep(Sweep{ID: "sw2", From: []AccountID{"src"}, To: "dst", CreatedAt: at}); err != nil {
		t.Fatalf("sweep 5000 at budget 6000: %v", err)
	}
	if got := l.MonthlySpend("src", "USD", "2026-10"); got != 5000 {
		t.Fatalf("MonthlySpend after sweep = %d, want 5000", got)
	}
}

// Hold captures settle spend from the hold account: a capture that would
// breach the hold account's monthly budget is rejected.
func TestBudgetCapture(t *testing.T) {
	l := New()
	seedBalance(t, l, "card", 10000)
	if _, _, err := l.Hold(Hold{ID: "h1", Account: "card", AmountCents: 6000, ExpiresAt: futureExpiry()}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	mustSetAccountBudget(t, l, "card", "USD", 5000)
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

	if _, err := l.Capture(Capture{ID: "c1", HoldID: "h1", To: "merchant", AmountCents: 6000, CreatedAt: at}); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("capture 6000 over budget 5000: err=%v, want ErrBudgetExceeded", err)
	}
	if _, err := l.Capture(Capture{ID: "c2", HoldID: "h1", To: "merchant", AmountCents: 4000, CreatedAt: at}); err != nil {
		t.Fatalf("capture 4000 at budget 5000: %v", err)
	}
	if got := l.MonthlySpend("card", "USD", "2026-10"); got != 4000 {
		t.Fatalf("MonthlySpend after capture = %d, want 4000", got)
	}
}

// The daily outflow limit (LG-26) and the monthly budget (LG-81) are
// orthogonal: both are checked, and each rejects independently.
func TestBudgetOrthogonalToDailyLimit(t *testing.T) {
	l := New()
	mustSetDailyLimit(t, l, "payer", "USD", 100) // tight daily window
	mustSetAccountBudget(t, l, "payer", "USD", 100000)
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

	// The daily limit binds first.
	if _, _, err := postSpend(t, l, "e1", "payer", 101, at); !errors.Is(err, ErrDailyLimitExceeded) {
		t.Fatalf("post 101 with daily limit 100: err=%v, want ErrDailyLimitExceeded", err)
	}
	// Loosen the day; the month still binds.
	mustSetDailyLimit(t, l, "payer", "USD", 100000)
	mustSetAccountBudget(t, l, "payer", "USD", 50)
	if _, _, err := postSpend(t, l, "e2", "payer", 51, at); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("post 51 with budget 50: err=%v, want ErrBudgetExceeded", err)
	}
}

// Lowering a budget below the month's already-booked spend is rejected —
// the operator learns the current usage instead of installing a budget
// that rejects every subsequent posting (mirrors SetDailyLimit).
func TestBudgetLowerBelowUsageRejected(t *testing.T) {
	l := New()
	mustSetAccountBudget(t, l, "payer", "USD", 1000)
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	if _, _, err := postSpend(t, l, "e1", "payer", 600, at); err != nil {
		t.Fatalf("post 600: %v", err)
	}
	if err := l.SetAccountBudget("payer", "USD", 599); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("lower budget below usage: err=%v, want ErrBudgetExceeded", err)
	}
	if err := l.SetAccountBudget("payer", "USD", 600); err != nil {
		t.Fatalf("lower budget to exactly usage: %v", err)
	}
}

// Setting and clearing a budget lands in the structured audit log's
// hash chain (ops "budget_set" / "budget_cleared"), with the ledger
// version unchanged across the structural change.
func TestBudgetConfigEntersAuditChain(t *testing.T) {
	al := mustAuditLog(t)
	defer al.Close()
	l := New(WithAuditLog(al))

	vBefore := l.version
	mustSetAccountBudget(t, l, "payer", "USD", 1000)
	l.ClearAccountBudget("payer", "USD")
	if l.version != vBefore {
		t.Fatalf("budget config bumped version: %d -> %d", vBefore, l.version)
	}

	now := time.Now()
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, corrupt, err := ReadAuditLog(al.Dir(), now.UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt events = %d, want 0", corrupt)
	}
	var ops []string
	for _, ev := range events {
		ops = append(ops, ev.Op)
		if ev.VersionBefore != ev.VersionAfter {
			t.Fatalf("op %q moved version %d -> %d, want structural (no bump)", ev.Op, ev.VersionBefore, ev.VersionAfter)
		}
	}
	foundSet, foundClear := false, false
	for _, op := range ops {
		if op == "budget_set" {
			foundSet = true
		}
		if op == "budget_cleared" {
			foundClear = true
		}
	}
	if !foundSet || !foundClear {
		t.Fatalf("audit ops = %v, want budget_set and budget_cleared", ops)
	}
}

// Setting or clearing a budget is structural: no version bump, but the
// change lands in the structured audit log.
func TestBudgetConfigIsStructural(t *testing.T) {
	l := New()
	if _, _, err := postSpend(t, l, "seed", "payer", 100, time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	vBefore := l.version
	mustSetAccountBudget(t, l, "payer", "USD", 1000)
	if l.version != vBefore {
		t.Fatalf("SetAccountBudget bumped version: %d -> %d", vBefore, l.version)
	}
	l.ClearAccountBudget("payer", "USD")
	if l.version != vBefore {
		t.Fatalf("ClearAccountBudget bumped version: %d -> %d", vBefore, l.version)
	}
	if _, ok := l.AccountBudget("payer", "USD"); ok {
		t.Fatal("budget still configured after ClearAccountBudget")
	}
	// A no-op set (same value) records nothing and stays silent.
	mustSetAccountBudget(t, l, "payer", "USD", 500)
	vBefore = l.version
	mustSetAccountBudget(t, l, "payer", "USD", 500)
	if l.version != vBefore {
		t.Fatalf("no-op SetAccountBudget bumped version: %d -> %d", vBefore, l.version)
	}
}

// Budgets round-trip through disaster-recovery snapshots; the accumulated
// per-month spend counters deliberately do not (fail-open for the
// remainder of the month, like the daily outflow counters).
func TestBudgetSnapshotRoundtrip(t *testing.T) {
	l := New()
	mustSetAccountBudget(t, l, "payer", "USD", 1000) // strict uppercase ISO 4217, like every currency in the ledger
	mustSetAccountBudget(t, l, "other", "EUR", 2000)
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	if _, _, err := postSpend(t, l, "e1", "payer", 600, at); err != nil {
		t.Fatalf("post 600: %v", err)
	}

	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	l2, err := ImportSnapshot(&buf)
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if got, ok := l2.AccountBudget("payer", "USD"); !ok || got != 1000 {
		t.Fatalf("restored payer budget = (%d, %v), want (1000, true)", got, ok)
	}
	if got, ok := l2.AccountBudget("other", "EUR"); !ok || got != 2000 {
		t.Fatalf("restored other budget = (%d, %v), want (2000, true)", got, ok)
	}
	// The spend counters reset: the restored ledger fail-opens for the
	// rest of the month.
	if got := l2.MonthlySpend("payer", "USD", "2026-10"); got != 0 {
		t.Fatalf("restored MonthlySpend = %d, want 0 (counters reset)", got)
	}
	// And the budget still enforces from zero.
	if _, _, err := l2.Post(JournalEntry{ID: "e2", DebitAccount: "m", CreditAccount: "payer", AmountCents: 1001, Currency: "USD", CreatedAt: at}); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("post 1001 on restored ledger: err=%v, want ErrBudgetExceeded", err)
	}
}

// A snapshot carrying a malformed budget row fails the import instead of
// silently dropping the risk control.
func TestBudgetSnapshotRejectsBadRow(t *testing.T) {
	l := New()
	snap := exportSnapshotString(t, l)
	var out []string
	spliced := false
	for _, line := range strings.Split(snap, "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decode snapshot line: %v", err)
		}
		if rec["record"] == "config" {
			rec["budgets"] = []any{
				map[string]any{"account": "payer", "currency": "USD", "budget_cents": float64(-5)},
			}
			spliced = true
		}
		b, _ := json.Marshal(rec)
		out = append(out, string(b))
	}
	if !spliced {
		t.Fatal("no config record found in snapshot")
	}
	if _, err := ImportSnapshot(strings.NewReader(strings.Join(out, "\n"))); err == nil {
		t.Fatal("ImportSnapshot accepted a negative budget row")
	}
}

// Reconcile reports every configured budget's utilization for the current
// UTC month: limit, used, and the utilization fraction.
func TestBudgetReconcileUtilization(t *testing.T) {
	l := New()
	mustSetAccountBudget(t, l, "payer", "USD", 1000)
	mustSetAccountBudget(t, l, "payer", "EUR", 2000)
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	if _, _, err := postSpend(t, l, "e1", "payer", 250, now); err != nil {
		t.Fatalf("post 250: %v", err)
	}

	report := l.Reconcile(now)
	rows := report.BudgetUtilization
	if len(rows) != 2 {
		t.Fatalf("budget_utilization rows = %d, want 2", len(rows))
	}
	// Rows sort by (account, currency): EUR before USD.
	eur, usd := rows[0], rows[1]
	if eur.Account != "payer" || eur.Currency != "EUR" {
		t.Fatalf("row 0 = %+v, want payer/EUR", eur)
	}
	if eur.UsedCents != 0 || eur.Utilization != 0 {
		t.Fatalf("EUR row = %+v, want zero usage", eur)
	}
	if usd.Account != "payer" || usd.Currency != "USD" || usd.Month != "2026-10" {
		t.Fatalf("row 1 = %+v, want payer/USD/2026-10", usd)
	}
	if usd.BudgetCents != 1000 || usd.UsedCents != 250 {
		t.Fatalf("USD row = budget %d used %d, want 1000/250", usd.BudgetCents, usd.UsedCents)
	}
	if usd.Utilization != 0.25 {
		t.Fatalf("USD utilization = %v, want 0.25", usd.Utilization)
	}
	if usd.Exceeded {
		t.Fatal("USD row marked exceeded at 25% utilization")
	}
}

// DryRunPost previews the budget impact: the month's used spend before
// and after the simulated posting, for every budgeted (account,
// currency) the simulation moved. A simulation that would breach the
// budget fails with the same ErrBudgetExceeded the real call returns.
func TestBudgetDryRunPreview(t *testing.T) {
	l := New()
	mustSetAccountBudget(t, l, "payer", "USD", 1000)
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	if _, _, err := postSpend(t, l, "e1", "payer", 600, at); err != nil {
		t.Fatalf("post 600: %v", err)
	}

	res, err := l.DryRunPost(JournalEntry{ID: "dry1", DebitAccount: "m", CreditAccount: "payer", AmountCents: 300, Currency: "USD", CreatedAt: at})
	if err != nil {
		t.Fatalf("DryRunPost: %v", err)
	}
	if !res.WouldSucceed {
		t.Fatal("DryRunPost: WouldSucceed = false, want true")
	}
	if len(res.BudgetImpact) != 1 {
		t.Fatalf("BudgetImpact rows = %d, want 1", len(res.BudgetImpact))
	}
	imp := res.BudgetImpact[0]
	if imp.Account != "payer" || imp.Currency != "USD" || imp.Month != "2026-10" {
		t.Fatalf("impact = %+v, want payer/USD/2026-10", imp)
	}
	if imp.BudgetCents != 1000 || imp.UsedBeforeCents != 600 || imp.UsedAfterCents != 900 {
		t.Fatalf("impact = %+v, want budget 1000 before 600 after 900", imp)
	}
	// The dry run moved nothing on the live ledger.
	if got := l.MonthlySpend("payer", "USD", "2026-10"); got != 600 {
		t.Fatalf("live MonthlySpend after dry run = %d, want 600", got)
	}

	// A simulation that would breach the budget fails like the real call.
	if _, err := l.DryRunPost(JournalEntry{ID: "dry2", DebitAccount: "m", CreditAccount: "payer", AmountCents: 401, Currency: "USD", CreatedAt: at}); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("DryRunPost over budget: err=%v, want ErrBudgetExceeded", err)
	}
}

// Dry-run transfer previews the fee-inclusive budget impact.
func TestBudgetDryRunTransferPreview(t *testing.T) {
	l := New()
	seedBalance(t, l, "alice", 100000)
	mustSetAccountBudget(t, l, "alice", "USD", 2000)
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

	res, err := l.DryRunTransfer(Transfer{ID: "dry-tx", From: "alice", To: "bob", AmountCents: 1000, FeeCents: 25, FeeAccount: "fees", CreatedAt: at})
	if err != nil {
		t.Fatalf("DryRunTransfer: %v", err)
	}
	if len(res.BudgetImpact) != 1 {
		t.Fatalf("BudgetImpact rows = %d, want 1", len(res.BudgetImpact))
	}
	if imp := res.BudgetImpact[0]; imp.UsedBeforeCents != 0 || imp.UsedAfterCents != 1025 {
		t.Fatalf("impact = %+v, want before 0 after 1025 (amount + fee)", imp)
	}
}

// The env-var syntax mirrors LEDGER_DAILY_OUTFLOW_LIMITS: strict parsing
// fails fast on malformed segments.
func TestParseAccountBudgets(t *testing.T) {
	got, err := ParseAccountBudgets("cust-123:USD:100000,cust-456:EUR:50000")
	if err != nil {
		t.Fatalf("ParseAccountBudgets: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("parsed %d budgets, want 2", len(got))
	}
	if got[0].Account != "cust-123" || got[0].Currency != "USD" || got[0].BudgetCents != 100000 {
		t.Fatalf("budget 0 = %+v", got[0])
	}
	if got[1].Account != "cust-456" || got[1].Currency != "EUR" || got[1].BudgetCents != 50000 {
		t.Fatalf("budget 1 = %+v", got[1])
	}
	if _, err := ParseAccountBudgets(""); err != nil {
		t.Fatalf("empty string: %v", err)
	}
	for _, bad := range []string{
		"cust-123:USD",          // missing segment
		":USD:100",             // empty account
		"cust:US:100",          // short currency
		"cust:usd:100",         // lowercase currency (strict uppercase ISO 4217)
		"cust:USD:-1",          // negative budget
		"cust:USD:abc",         // non-integer budget
		"cust:USD:100:extra",   // too many segments
	} {
		if _, err := ParseAccountBudgets(bad); err == nil {
			t.Fatalf("ParseAccountBudgets(%q): want error, got nil", bad)
		}
	}
}

// Construction-time budgets fail fast on misconfiguration, like the fee
// schedule.
func TestWithAccountBudgetPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("WithAccountBudget with negative budget did not panic")
		}
	}()
	_ = New(WithAccountBudget("payer", "USD", -1))
}

// Zero is a legal budget: it stops all spend (a risk-control
// freeze-out), mirroring the zero daily limit.
func TestBudgetZeroStopsAllSpend(t *testing.T) {
	l := New()
	mustSetAccountBudget(t, l, "payer", "USD", 0)
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	if _, _, err := postSpend(t, l, "e1", "payer", 1, at); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("post 1 with zero budget: err=%v, want ErrBudgetExceeded", err)
	}
}

// Old spend buckets are pruned: months older than last month drop out of
// the accumulation map, so a long-lived ledger does not grow its bucket
// map forever.
func TestBudgetPrunesOldMonths(t *testing.T) {
	l := New()
	l.pruneInterval = 0 // prune on every posting, like the idempotency TTL test
	mustSetAccountBudget(t, l, "payer", "USD", math.MaxInt64)
	old := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	if _, _, err := postSpend(t, l, "e-old", "payer", 100, old); err != nil {
		t.Fatalf("post in July: %v", err)
	}
	// A fresh posting triggers the prune sweep, which drops the July
	// bucket (older than last month relative to October).
	if _, _, err := postSpend(t, l, "e-new", "payer", 100, time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("post in October: %v", err)
	}
	if got := l.MonthlySpend("payer", "USD", "2026-07"); got != 0 {
		t.Fatalf("MonthlySpend(2026-07) after prune = %d, want 0", got)
	}
}
