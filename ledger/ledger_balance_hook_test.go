package ledger

import (
	"sync"
	"testing"
	"time"
)

// hookRecorder collects BalanceChange events delivered to a
// BalanceChangeHook, safe for the concurrent async dispatch.
type hookRecorder struct {
	mu     sync.Mutex
	events []BalanceChange
	ch     chan BalanceChange
}

func newHookRecorder() *hookRecorder {
	return &hookRecorder{ch: make(chan BalanceChange, 64)}
}

func (r *hookRecorder) hook() BalanceChangeHook {
	return func(ev BalanceChange) {
		r.mu.Lock()
		r.events = append(r.events, ev)
		r.mu.Unlock()
		r.ch <- ev
	}
}

// collect waits for n events or fails the test.
func (r *hookRecorder) collect(t *testing.T, n int) []BalanceChange {
	t.Helper()
	out := make([]BalanceChange, 0, n)
	deadline := time.After(5 * time.Second)
	for len(out) < n {
		select {
		case ev := <-r.ch:
			out = append(out, ev)
		case <-deadline:
			t.Fatalf("timed out waiting for hook events: got %d of %d", len(out), n)
		}
	}
	return out
}

func (r *hookRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

func findHookEvent(events []BalanceChange, account AccountID, currency string) (BalanceChange, bool) {
	for _, ev := range events {
		if ev.Account == account && ev.Currency == currency {
			return ev, true
		}
	}
	return BalanceChange{}, false
}

func TestBalanceHookPost(t *testing.T) {
	rec := newHookRecorder()
	l := New(WithBalanceChangeHook(rec.hook()))
	if !l.BalanceChangeHookEnabled() {
		t.Fatal("hook should be enabled after WithBalanceChangeHook")
	}

	_, dup, err := l.Post(JournalEntry{ID: "e1", DebitAccount: "b", CreditAccount: "a", AmountCents: 1000})
	if err != nil || dup {
		t.Fatalf("Post: err=%v dup=%v", err, dup)
	}

	events := rec.collect(t, 2)
	if got := l.Version(); got != 1 {
		t.Fatalf("version = %d, want 1", got)
	}
	debit, ok := findHookEvent(events, "b", "USD")
	if !ok {
		t.Fatalf("no event for debit account b: %+v", events)
	}
	if debit.OldCents != 0 || debit.NewCents != 1000 || debit.DeltaCents != 1000 {
		t.Fatalf("debit event wrong: %+v", debit)
	}
	if debit.Version != 1 || debit.Trace != "e1" || debit.Currency != DefaultCurrency {
		t.Fatalf("debit event metadata wrong: %+v", debit)
	}
	credit, ok := findHookEvent(events, "a", "USD")
	if !ok {
		t.Fatalf("no event for credit account a: %+v", events)
	}
	if credit.OldCents != 0 || credit.NewCents != -1000 || credit.DeltaCents != -1000 {
		t.Fatalf("credit event wrong: %+v", credit)
	}
	if credit.Version != 1 || credit.Trace != "e1" {
		t.Fatalf("credit event metadata wrong: %+v", credit)
	}

	// A second posting moves the same accounts: old balances must be
	// the committed pre-post balances.
	if _, _, err := l.Post(JournalEntry{ID: "e2", DebitAccount: "b", CreditAccount: "a", AmountCents: 500}); err != nil {
		t.Fatalf("Post e2: %v", err)
	}
	events2 := rec.collect(t, 2)
	debit2, _ := findHookEvent(events2, "b", "USD")
	if debit2.OldCents != 1000 || debit2.NewCents != 1500 || debit2.DeltaCents != 500 || debit2.Version != 2 || debit2.Trace != "e2" {
		t.Fatalf("second debit event wrong: %+v", debit2)
	}
}

func TestBalanceHookTransferWithFee(t *testing.T) {
	rec := newHookRecorder()
	l := New(WithBalanceChangeHook(rec.hook()))

	// Seed the payer so it can cover amount + fee.
	if _, _, err := l.Post(JournalEntry{ID: "seed", DebitAccount: "payer", CreditAccount: "equity", AmountCents: 100000}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	seedEvents := rec.collect(t, 2)
	if _, ok := findHookEvent(seedEvents, "payer", "USD"); !ok {
		t.Fatalf("missing seed event for payer: %+v", seedEvents)
	}

	_, err := l.PostTransfer(Transfer{
		ID: "tx1", From: "payer", To: "payee",
		AmountCents: 10000, FeeCents: 250, FeeAccount: "fees",
	})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	events := rec.collect(t, 3)
	// One event per touched account, with the payer's delta netting
	// amount + fee.
	payer, ok := findHookEvent(events, "payer", "USD")
	if !ok {
		t.Fatalf("missing payer event: %+v", events)
	}
	if payer.OldCents != 100000 || payer.NewCents != 89750 || payer.DeltaCents != -10250 {
		t.Fatalf("payer event wrong: %+v", payer)
	}
	if payer.Version != 3 || payer.Trace != "tx1" {
		t.Fatalf("payer event metadata wrong: %+v", payer)
	}
	payee, ok := findHookEvent(events, "payee", "USD")
	if !ok || payee.DeltaCents != 10000 || payee.OldCents != 0 || payee.NewCents != 10000 {
		t.Fatalf("payee event wrong: %+v", payee)
	}
	fee, ok := findHookEvent(events, "fees", "USD")
	if !ok || fee.DeltaCents != 250 || fee.OldCents != 0 || fee.NewCents != 250 {
		t.Fatalf("fee event wrong: %+v", fee)
	}
}

func TestBalanceHookCapture(t *testing.T) {
	rec := newHookRecorder()
	l := New(WithBalanceChangeHook(rec.hook()))

	if _, _, err := l.Post(JournalEntry{ID: "seed", DebitAccount: "card", CreditAccount: "equity", AmountCents: 10000}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rec.collect(t, 2)

	if _, _, err := l.Hold(Hold{ID: "h1", Account: "card", AmountCents: 6000, ExpiresAt: futureExpiry()}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	// Holds are off-journal: no events.
	select {
	case ev := <-rec.ch:
		t.Fatalf("hold fired a hook event, want none: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}

	r, err := l.Capture(Capture{ID: "c1", HoldID: "h1", To: "merchant", AmountCents: 3200})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if r.CapturedCents != 3200 {
		t.Fatalf("captured = %d", r.CapturedCents)
	}
	events := rec.collect(t, 2)
	card, ok := findHookEvent(events, "card", "USD")
	if !ok || card.OldCents != 10000 || card.NewCents != 6800 || card.DeltaCents != -3200 {
		t.Fatalf("card event wrong: %+v", card)
	}
	if card.Trace != "h1" {
		t.Fatalf("capture trace = %q, want hold ID h1", card.Trace)
	}
	merchant, ok := findHookEvent(events, "merchant", "USD")
	if !ok || merchant.DeltaCents != 3200 || merchant.NewCents != 3200 {
		t.Fatalf("merchant event wrong: %+v", merchant)
	}
}

func TestBalanceHookSweep(t *testing.T) {
	rec := newHookRecorder()
	l := New(WithBalanceChangeHook(rec.hook()))

	for i, seed := range []struct {
		id      string
		account AccountID
		amount  int64
	}{
		{"s1", "sub-1", 10000},
		{"s2", "sub-2", 2500},
	} {
		if _, _, err := l.Post(JournalEntry{ID: seed.id, DebitAccount: seed.account, CreditAccount: "equity", AmountCents: seed.amount}); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		rec.collect(t, 2)
	}

	receipt, err := l.PostSweep(Sweep{ID: "sw1", From: []AccountID{"sub-1", "sub-2"}, To: "hub"})
	if err != nil {
		t.Fatalf("PostSweep: %v", err)
	}
	if receipt.Duplicate || len(receipt.Legs) != 2 {
		t.Fatalf("unexpected sweep receipt: %+v", receipt)
	}
	events := rec.collect(t, 3)
	sub1, ok := findHookEvent(events, "sub-1", "USD")
	if !ok || sub1.OldCents != 10000 || sub1.NewCents != 0 || sub1.DeltaCents != -10000 {
		t.Fatalf("sub-1 event wrong: %+v", sub1)
	}
	if sub1.Trace != "sw1" {
		t.Fatalf("sweep trace = %q, want sw1", sub1.Trace)
	}
	sub2, ok := findHookEvent(events, "sub-2", "USD")
	if !ok || sub2.DeltaCents != -2500 || sub2.NewCents != 0 {
		t.Fatalf("sub-2 event wrong: %+v", sub2)
	}
	hub, ok := findHookEvent(events, "hub", "USD")
	if !ok || hub.OldCents != 0 || hub.NewCents != 12500 || hub.DeltaCents != 12500 {
		t.Fatalf("hub event wrong: %+v", hub)
	}
	// Version: 2 seeds + 2 sweep legs = 4.
	if hub.Version != 4 {
		t.Fatalf("hub event version = %d, want 4", hub.Version)
	}
}

func TestBalanceHookMerge(t *testing.T) {
	rec := newHookRecorder()
	l := New(WithBalanceChangeHook(rec.hook()))

	if _, _, err := l.Post(JournalEntry{ID: "seed", DebitAccount: "old-corp", CreditAccount: "equity", AmountCents: 7500}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rec.collect(t, 2)

	receipt, err := l.PostMerge(Merge{ID: "m1", From: "old-corp", To: "new-corp"})
	if err != nil {
		t.Fatalf("PostMerge: %v", err)
	}
	if receipt.Duplicate || !receipt.SourceFrozen {
		t.Fatalf("unexpected merge receipt: %+v", receipt)
	}
	events := rec.collect(t, 2)
	src, ok := findHookEvent(events, "old-corp", "USD")
	if !ok || src.OldCents != 7500 || src.NewCents != 0 || src.DeltaCents != -7500 {
		t.Fatalf("source event wrong: %+v", src)
	}
	if src.Trace != "m1" {
		t.Fatalf("merge trace = %q, want m1", src.Trace)
	}
	dst, ok := findHookEvent(events, "new-corp", "USD")
	if !ok || dst.OldCents != 0 || dst.NewCents != 7500 || dst.DeltaCents != 7500 {
		t.Fatalf("target event wrong: %+v", dst)
	}
}

func TestBalanceHookReplaySilent(t *testing.T) {
	rec := newHookRecorder()
	l := New(WithBalanceChangeHook(rec.hook()))

	_, _, err := l.Post(JournalEntry{ID: "e1", DebitAccount: "b", CreditAccount: "a", AmountCents: 100, IdempotencyKey: "k1"})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	rec.collect(t, 2)

	_, dup, err := l.Post(JournalEntry{ID: "e1-retry", DebitAccount: "b", CreditAccount: "a", AmountCents: 100, IdempotencyKey: "k1"})
	if err != nil || !dup {
		t.Fatalf("replay: err=%v dup=%v", err, dup)
	}
	// A replay books nothing, so it must fire nothing.
	select {
	case ev := <-rec.ch:
		t.Fatalf("replay fired a hook event, want none: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestBalanceHookOptOutDefault(t *testing.T) {
	l := New()
	if l.BalanceChangeHookEnabled() {
		t.Fatal("hook must be disabled by default")
	}
	posted, _, err := l.Post(JournalEntry{ID: "e1", DebitAccount: "b", CreditAccount: "a", AmountCents: 100})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if posted.ID != "e1" || l.Balance("b") != 100 || l.Version() != 1 {
		t.Fatalf("opt-out posting changed behavior: %+v", posted)
	}
}

func TestBalanceHookPanicIsolation(t *testing.T) {
	l := New(WithBalanceChangeHook(func(BalanceChange) {
		panic("downstream webhook exploded")
	}))

	// The panic must not escape the commit path and must not affect the
	// committed entry.
	posted, _, err := l.Post(JournalEntry{ID: "e1", DebitAccount: "b", CreditAccount: "a", AmountCents: 100})
	if err != nil {
		t.Fatalf("Post with panicking hook: %v", err)
	}
	if posted.ID != "e1" {
		t.Fatalf("posted = %+v", posted)
	}
	if got := l.Balance("b"); got != 100 {
		t.Fatalf("balance b = %d, want 100", got)
	}
	if got := l.Version(); got != 1 {
		t.Fatalf("version = %d, want 1", got)
	}
	// The ledger keeps working after a hook panic.
	if _, _, err := l.Post(JournalEntry{ID: "e2", DebitAccount: "b", CreditAccount: "a", AmountCents: 50}); err != nil {
		t.Fatalf("second Post after hook panic: %v", err)
	}
	if got := l.Balance("b"); got != 150 {
		t.Fatalf("balance b = %d, want 150", got)
	}
}

func TestBalanceHookTimeout(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })

	rec := newHookRecorder()
	l := New(
		WithBalanceChangeHook(func(ev BalanceChange) {
			<-release // blocks until the test ends: simulates a wedged downstream
			rec.ch <- ev
		}),
		WithBalanceChangeHookTimeout(20*time.Millisecond),
	)

	start := time.Now()
	_, _, err := l.Post(JournalEntry{ID: "e1", DebitAccount: "b", CreditAccount: "a", AmountCents: 100})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Post with wedged hook: %v", err)
	}
	// The commit path must not wait out the wedged callback: 20ms
	// timeout vs a 2s bound leaves two orders of magnitude of slack.
	if elapsed > 2*time.Second {
		t.Fatalf("Post blocked on hook for %v, want < 2s", elapsed)
	}
	if got := l.Balance("b"); got != 100 {
		t.Fatalf("entry not committed: balance b = %d", got)
	}
	if got := l.Version(); got != 1 {
		t.Fatalf("version = %d, want 1", got)
	}
}

func TestBalanceHookTimeoutConfig(t *testing.T) {
	l := New()
	if l.balanceHookTimeout != 0 {
		t.Fatalf("default timeout field = %v, want 0 (unset)", l.balanceHookTimeout)
	}
	l.SetBalanceChangeHookTimeout(250 * time.Millisecond)
	if l.balanceHookTimeout != 250*time.Millisecond {
		t.Fatalf("timeout = %v, want 250ms", l.balanceHookTimeout)
	}
	// Non-positive durations are ignored.
	l.SetBalanceChangeHookTimeout(0)
	l.SetBalanceChangeHookTimeout(-time.Second)
	if l.balanceHookTimeout != 250*time.Millisecond {
		t.Fatalf("timeout changed on non-positive input: %v", l.balanceHookTimeout)
	}

	rec := newHookRecorder()
	l2 := New(WithBalanceChangeHook(rec.hook()), WithBalanceChangeHookTimeout(50*time.Millisecond))
	if l2.balanceHookTimeout != 50*time.Millisecond {
		t.Fatalf("option timeout = %v, want 50ms", l2.balanceHookTimeout)
	}
	if _, _, err := l2.Post(JournalEntry{ID: "e1", DebitAccount: "b", CreditAccount: "a", AmountCents: 1}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	rec.collect(t, 2)
}

func TestBalanceHookRuntimeRegistration(t *testing.T) {
	l := New()
	if l.BalanceChangeHookEnabled() {
		t.Fatal("hook enabled before registration")
	}
	rec := newHookRecorder()
	l.SetBalanceChangeHook(rec.hook())
	if !l.BalanceChangeHookEnabled() {
		t.Fatal("hook not enabled after SetBalanceChangeHook")
	}
	if _, _, err := l.Post(JournalEntry{ID: "e1", DebitAccount: "b", CreditAccount: "a", AmountCents: 10}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	events := rec.collect(t, 2)
	if ev, ok := findHookEvent(events, "b", "USD"); !ok || ev.DeltaCents != 10 {
		t.Fatalf("runtime hook event wrong: %+v", events)
	}
	// Clearing with nil disables again.
	l.SetBalanceChangeHook(nil)
	if l.BalanceChangeHookEnabled() {
		t.Fatal("hook still enabled after SetBalanceChangeHook(nil)")
	}
	if _, _, err := l.Post(JournalEntry{ID: "e2", DebitAccount: "b", CreditAccount: "a", AmountCents: 10}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	select {
	case ev := <-rec.ch:
		t.Fatalf("disabled hook fired, want none: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}
