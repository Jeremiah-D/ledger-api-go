package ledger

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"
)

// fundSplitSource posts starting balances for the split tests: debit
// legs fund the source with positive balances.
func fundSplitSource(t *testing.T, l *Ledger, legs ...JournalEntry) {
	t.Helper()
	for _, e := range legs {
		if _, _, err := l.Post(e); err != nil {
			t.Fatalf("fund %s: %v", e.ID, err)
		}
	}
}

func TestSplitAccount_HappyPath(t *testing.T) {
	l := New()
	fundSplitSource(t, l,
		JournalEntry{ID: "f1", DebitAccount: "parent", CreditAccount: "bank", AmountCents: 1000, Currency: "USD"},
		JournalEntry{ID: "f2", DebitAccount: "parent", CreditAccount: "bank", AmountCents: 500, Currency: "EUR"},
	)
	vb := l.versionOf()

	rcpt, err := l.SplitAccount(Split{
		ID: "sp1", From: "parent", To: "child", IdempotencyKey: "k-sp1",
		Legs: []SplitLeg{
			{Currency: "USD", AmountCents: 400},
			{Currency: "EUR", AmountCents: 500},
		},
	})
	if err != nil {
		t.Fatalf("SplitAccount: %v", err)
	}
	if rcpt.SplitID != "sp1" || rcpt.From != "parent" || rcpt.To != "child" {
		t.Fatalf("receipt identity = %+v", rcpt)
	}
	if rcpt.Duplicate {
		t.Fatal("Duplicate = true on first split")
	}
	// One leg per requested currency, currencies sorted.
	if len(rcpt.Legs) != 2 || len(rcpt.Entries) != 2 {
		t.Fatalf("legs = %d, entries = %d; want 2/2", len(rcpt.Legs), len(rcpt.Entries))
	}
	if rcpt.Legs[0].Currency != "EUR" || rcpt.Legs[1].Currency != "USD" {
		t.Fatalf("leg currencies = %v, want [EUR USD]", rcpt.Legs)
	}
	for _, leg := range rcpt.Legs {
		if want := "sp1/" + leg.Currency; leg.EntryID != want {
			t.Errorf("leg entry ID = %q, want %q", leg.EntryID, want)
		}
	}
	if rcpt.Legs[0].AmountCents != 500 || rcpt.Legs[1].AmountCents != 400 {
		t.Errorf("leg amounts = %v, want [500 400]", rcpt.Legs)
	}
	// Source retains the remainder; target accumulated.
	if got := l.BalanceIn("parent", "USD"); got != 600 {
		t.Errorf("parent USD = %d, want 600", got)
	}
	if got := l.BalanceIn("parent", "EUR"); got != 0 {
		t.Errorf("parent EUR = %d, want 0", got)
	}
	if got := l.BalanceIn("child", "USD"); got != 400 {
		t.Errorf("child USD = %d, want 400", got)
	}
	if got := l.BalanceIn("child", "EUR"); got != 500 {
		t.Errorf("child EUR = %d, want 500", got)
	}
	// The version bumped once per leg.
	if l.versionOf() != vb+2 {
		t.Errorf("version = %d, want %d", l.versionOf(), vb+2)
	}
	// The source is NOT frozen: splitting is a reorganization, not a
	// decommission — a second split still works.
	rcpt2, err := l.SplitAccount(Split{
		ID: "sp2", From: "parent", To: "child2",
		Legs: []SplitLeg{{Currency: "USD", AmountCents: 100}},
	})
	if err != nil {
		t.Fatalf("second split: %v", err)
	}
	if rcpt2.Legs[0].AmountCents != 100 || l.BalanceIn("parent", "USD") != 500 {
		t.Errorf("second split: leg=%+v parent USD=%d", rcpt2.Legs[0], l.BalanceIn("parent", "USD"))
	}
}

func TestSplitAccount_RatioLegs(t *testing.T) {
	l := New()
	fundSplitSource(t, l,
		JournalEntry{ID: "f1", DebitAccount: "parent", CreditAccount: "bank", AmountCents: 1000, Currency: "USD"},
	)
	rcpt, err := l.SplitAccount(Split{
		ID: "sp-ratio", From: "parent", To: "child",
		Legs: []SplitLeg{{Currency: "USD", RatioBps: 2500}},
	})
	if err != nil {
		t.Fatalf("SplitAccount: %v", err)
	}
	// floor(1000 * 2500 / 10000) = 250.
	if len(rcpt.Legs) != 1 || rcpt.Legs[0].AmountCents != 250 {
		t.Fatalf("legs = %+v, want one 250 leg", rcpt.Legs)
	}
	if got := l.BalanceIn("parent", "USD"); got != 750 {
		t.Errorf("parent USD = %d, want 750", got)
	}
	// A full-ratio split moves the whole balance.
	rcpt2, err := l.SplitAccount(Split{
		ID: "sp-ratio-full", From: "parent", To: "child",
		Legs: []SplitLeg{{Currency: "USD", RatioBps: 10000}},
	})
	if err != nil {
		t.Fatalf("full-ratio split: %v", err)
	}
	if rcpt2.Legs[0].AmountCents != 750 || l.BalanceIn("parent", "USD") != 0 {
		t.Errorf("full-ratio: leg=%+v parent USD=%d", rcpt2.Legs[0], l.BalanceIn("parent", "USD"))
	}
}

func TestSplitAccount_Validation(t *testing.T) {
	l := New()
	fundSplitSource(t, l,
		JournalEntry{ID: "f1", DebitAccount: "parent", CreditAccount: "bank", AmountCents: 1000, Currency: "USD"},
	)
	good := []SplitLeg{{Currency: "USD", AmountCents: 100}}
	cases := []struct {
		name string
		s    Split
		want error
	}{
		{"empty ID", Split{From: "parent", To: "child", Legs: good}, ErrEmptySplitID},
		{"empty source", Split{ID: "x", To: "child", Legs: good}, ErrEmptySplitSource},
		{"empty target", Split{ID: "x", From: "parent", Legs: good}, ErrEmptySplitTarget},
		{"same account", Split{ID: "x", From: "parent", To: "parent", Legs: good}, ErrSplitSameAccount},
		{"no legs", Split{ID: "x", From: "parent", To: "child"}, ErrSplitNoLegs},
		{"amount and ratio", Split{ID: "x", From: "parent", To: "child", Legs: []SplitLeg{{Currency: "USD", AmountCents: 100, RatioBps: 100}}}, ErrSplitLegAmountAndRatio},
		{"neither amount nor ratio", Split{ID: "x", From: "parent", To: "child", Legs: []SplitLeg{{Currency: "USD"}}}, ErrSplitLegAmountAndRatio},
		{"negative amount", Split{ID: "x", From: "parent", To: "child", Legs: []SplitLeg{{Currency: "USD", AmountCents: -5}}}, ErrSplitNonPositiveAmount},
		{"zero ratio", Split{ID: "x", From: "parent", To: "child", Legs: []SplitLeg{{Currency: "USD", RatioBps: 0}}}, ErrSplitLegAmountAndRatio},
		{"ratio over 10000", Split{ID: "x", From: "parent", To: "child", Legs: []SplitLeg{{Currency: "USD", RatioBps: 10001}}}, ErrSplitInvalidRatio},
		{"duplicate currency", Split{ID: "x", From: "parent", To: "child", Legs: []SplitLeg{{Currency: "USD", AmountCents: 10}, {Currency: "USD", AmountCents: 10}}}, ErrSplitDuplicateCurrency},
		{"bad currency", Split{ID: "x", From: "parent", To: "child", Legs: []SplitLeg{{Currency: "USDD", AmountCents: 10}}}, ErrInvalidCurrency},
		{"lowercase currency", Split{ID: "x", From: "parent", To: "child", Legs: []SplitLeg{{Currency: "usd", AmountCents: 10}}}, ErrInvalidCurrency},
		{"amount exceeds balance", Split{ID: "x", From: "parent", To: "child", Legs: []SplitLeg{{Currency: "USD", AmountCents: 1001}}}, ErrSplitInsufficientBalance},
		{"unknown currency balance", Split{ID: "x", From: "parent", To: "child", Legs: []SplitLeg{{Currency: "EUR", AmountCents: 10}}}, ErrSplitInsufficientBalance},
		{"dust ratio", Split{ID: "x", From: "poor", To: "child", Legs: []SplitLeg{{Currency: "USD", RatioBps: 1}}}, ErrSplitInsufficientBalance},
	}
	// "poor" holds 50 cents: 1bps floors to 0 -> dust rejection needs a
	// positive-but-tiny balance; use the funded parent with a 1bps ratio
	// on a 50-cent account instead.
	l2 := New()
	fundSplitSource(t, l2,
		JournalEntry{ID: "f1", DebitAccount: "poor", CreditAccount: "bank", AmountCents: 50, Currency: "USD"},
	)
	for _, c := range cases {
		target := l
		if c.name == "dust ratio" {
			c.s.From = "poor"
			target = l2
			c.want = ErrSplitNonPositiveAmount
		}
		if _, err := target.SplitAccount(c.s); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}

	// A lowercase currency is rejected (currencies are strict
	// 3-letter uppercase ISO 4217, like every other writer): the
	// duplicate-currency gate above already covers the case where both
	// legs pass validation.
	_, err := l.SplitAccount(Split{
		ID: "sp-norm", From: "parent", To: "child",
		Legs: []SplitLeg{{Currency: "usd", AmountCents: 10}},
	})
	if !errors.Is(err, ErrInvalidCurrency) {
		t.Errorf("lowercase currency: err = %v, want ErrInvalidCurrency", err)
	}
}

func TestSplitAccount_Idempotency(t *testing.T) {
	l := New()
	fundSplitSource(t, l,
		JournalEntry{ID: "f1", DebitAccount: "parent", CreditAccount: "bank", AmountCents: 1000, Currency: "USD"},
	)
	first, err := l.SplitAccount(Split{
		ID: "sp-idem", From: "parent", To: "child", IdempotencyKey: "k-sp",
		Legs: []SplitLeg{{Currency: "USD", AmountCents: 300}},
	})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	vb := l.versionOf()
	replay, err := l.SplitAccount(Split{
		ID: "sp-idem-retry", From: "parent", To: "child", IdempotencyKey: "k-sp",
		Legs: []SplitLeg{{Currency: "USD", AmountCents: 300}},
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !replay.Duplicate || replay.SplitID != "sp-idem" {
		t.Errorf("replay = %+v, want duplicate of sp-idem", replay)
	}
	if len(replay.Entries) != 1 || replay.Entries[0].ID != first.Entries[0].ID {
		t.Errorf("replay entries = %+v, want the original", replay.Entries)
	}
	if l.versionOf() != vb {
		t.Errorf("version moved on replay: %d -> %d", vb, l.versionOf())
	}
	if got := l.BalanceIn("parent", "USD"); got != 700 {
		t.Errorf("parent USD = %d, want 700 (no double split)", got)
	}
}

func TestSplitAccount_IDConflict(t *testing.T) {
	l := New()
	fundSplitSource(t, l,
		JournalEntry{ID: "sp-conflict/USD", DebitAccount: "x", CreditAccount: "bank", AmountCents: 1, Currency: "USD"},
		JournalEntry{ID: "f1", DebitAccount: "parent", CreditAccount: "bank", AmountCents: 1000, Currency: "USD"},
	)
	if _, err := l.SplitAccount(Split{
		ID: "sp-conflict", From: "parent", To: "child",
		Legs: []SplitLeg{{Currency: "USD", AmountCents: 10}},
	}); !errors.Is(err, ErrSplitIDConflict) {
		t.Errorf("leg ID conflict: err = %v, want ErrSplitIDConflict", err)
	}
}

func TestSplitAccount_Gates(t *testing.T) {
	l := New()
	fundSplitSource(t, l,
		JournalEntry{ID: "f1", DebitAccount: "parent", CreditAccount: "bank", AmountCents: 1000, Currency: "USD"},
		JournalEntry{ID: "f2", DebitAccount: "target", CreditAccount: "bank", AmountCents: 100, Currency: "USD"},
	)
	legs := []SplitLeg{{Currency: "USD", AmountCents: 10}}

	l.Freeze("parent")
	if _, err := l.SplitAccount(Split{ID: "sp-fz", From: "parent", To: "target", Legs: legs}); !errors.Is(err, ErrAccountFrozen) {
		t.Errorf("frozen source: err = %v, want ErrAccountFrozen", err)
	}
	l.Unfreeze("parent")
	l.Freeze("target")
	if _, err := l.SplitAccount(Split{ID: "sp-fz2", From: "parent", To: "target", Legs: legs}); !errors.Is(err, ErrAccountFrozen) {
		t.Errorf("frozen target: err = %v, want ErrAccountFrozen", err)
	}
	l.Unfreeze("target")
	if _, _, err := l.CloseAccount("parent", false); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := l.SplitAccount(Split{ID: "sp-cl", From: "parent", To: "target", Legs: legs}); !errors.Is(err, ErrAccountClosed) {
		t.Errorf("closed source: err = %v, want ErrAccountClosed", err)
	}
	// Rejected splits record nothing.
	if got := l.BalanceIn("parent", "USD"); got != 1000 {
		t.Errorf("parent USD = %d, want 1000", got)
	}
}

func TestSplitAccount_SplitKeysTTL(t *testing.T) {
	l := New(WithIdempotencyTTL(time.Hour))
	fundSplitSource(t, l,
		JournalEntry{ID: "f1", DebitAccount: "parent", CreditAccount: "bank", AmountCents: 1000, Currency: "USD"},
	)
	if _, err := l.SplitAccount(Split{
		ID: "sp-ttl", From: "parent", To: "child", IdempotencyKey: "k-ttl",
		Legs: []SplitLeg{{Currency: "USD", AmountCents: 100}},
	}); err != nil {
		t.Fatalf("split: %v", err)
	}
	if _, ok := l.splitKeys["k-ttl"]; !ok {
		t.Fatal("split key not registered")
	}
	if n := l.pruneSplitKeysLocked(time.Now().Add(2 * time.Hour)); n != 1 {
		t.Errorf("pruned = %d, want 1", n)
	}
	if _, ok := l.splitKeys["k-ttl"]; ok {
		t.Error("split key survived TTL prune")
	}
	// The registry itself survives: it is audit history, not a replay
	// index.
	if _, ok := l.splits["sp-ttl"]; !ok {
		t.Error("split registry row pruned with the key")
	}
}

func TestSplitAccount_SnapshotRoundTrip(t *testing.T) {
	l := New()
	fundSplitSource(t, l,
		JournalEntry{ID: "f1", DebitAccount: "parent", CreditAccount: "bank", AmountCents: 1000, Currency: "USD"},
	)
	if _, err := l.SplitAccount(Split{
		ID: "sp-snap", From: "parent", To: "child", IdempotencyKey: "k-snap",
		Legs: []SplitLeg{{Currency: "USD", AmountCents: 250}},
	}); err != nil {
		t.Fatalf("split: %v", err)
	}
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("export: %v", err)
	}
	l2, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(l2.splits) != 1 {
		t.Fatalf("restored splits = %d, want 1", len(l2.splits))
	}
	rec := l2.splits["sp-snap"]
	if rec.from != "parent" || rec.to != "child" || len(rec.legs) != 1 || rec.legs[0].AmountCents != 250 {
		t.Errorf("restored split = %+v", rec)
	}
	// The idempotency key index survives: a replay returns the original
	// receipt instead of splitting again.
	replay, err := l2.SplitAccount(Split{
		ID: "sp-snap-retry", From: "parent", To: "child", IdempotencyKey: "k-snap",
		Legs: []SplitLeg{{Currency: "USD", AmountCents: 250}},
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !replay.Duplicate || replay.SplitID != "sp-snap" {
		t.Errorf("replay = %+v, want duplicate of sp-snap", replay)
	}
	if got := l2.BalanceIn("parent", "USD"); got != 750 {
		t.Errorf("parent USD = %d, want 750", got)
	}
}

func TestSplitAccount_Reconcile(t *testing.T) {
	l := New()
	fundSplitSource(t, l,
		JournalEntry{ID: "f1", DebitAccount: "parent", CreditAccount: "bank", AmountCents: 1000, Currency: "USD"},
	)
	if _, err := l.SplitAccount(Split{
		ID: "sp-rep", From: "parent", To: "child",
		Legs: []SplitLeg{{Currency: "USD", AmountCents: 300}},
	}); err != nil {
		t.Fatalf("split: %v", err)
	}
	report := l.Reconcile(time.Now())
	if len(report.Splits) != 1 {
		t.Fatalf("report.Splits = %d, want 1", len(report.Splits))
	}
	sp := report.Splits[0]
	if sp.SplitID != "sp-rep" || sp.FromAccount != "parent" || sp.ToAccount != "child" {
		t.Errorf("split = %+v", sp)
	}
	if len(sp.Legs) != 1 || sp.Legs[0].AmountCents != 300 || sp.Legs[0].EntryID != "sp-rep/USD" {
		t.Errorf("legs = %+v", sp.Legs)
	}
}

func TestSplitAccount_ConcurrentNoDoubleSplit(t *testing.T) {
	l := New()
	fundSplitSource(t, l,
		JournalEntry{ID: "f1", DebitAccount: "parent", CreditAccount: "bank", AmountCents: 1000, Currency: "USD"},
	)
	var wg sync.WaitGroup
	errs := make([]error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = l.SplitAccount(Split{
				ID:   "sp-conc-" + string(rune('a'+i)),
				From: "parent", To: "child",
				Legs: []SplitLeg{{Currency: "USD", AmountCents: 300}},
			})
		}(i)
	}
	wg.Wait()
	succeeded := 0
	for _, err := range errs {
		if err == nil {
			succeeded++
		} else if !errors.Is(err, ErrSplitInsufficientBalance) {
			t.Errorf("unexpected error: %v", err)
		}
	}
	// 1000 / 300 = 3 full splits; the rest see the drained balance.
	if succeeded != 3 {
		t.Errorf("succeeded = %d, want 3", succeeded)
	}
	if got := l.BalanceIn("parent", "USD"); got != 100 {
		t.Errorf("parent USD = %d, want 100", got)
	}
	if got := l.BalanceIn("child", "USD"); got != 900 {
		t.Errorf("child USD = %d, want 900", got)
	}
}

func TestSplitAccount_AuditAndOutbox(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	fundSplitSource(t, l,
		JournalEntry{ID: "f1", DebitAccount: "parent", CreditAccount: "bank", AmountCents: 1000, Currency: "USD"},
	)
	if _, err := l.SplitAccount(Split{
		ID: "sp-audit", From: "parent", To: "child",
		Legs: []SplitLeg{{Currency: "USD", AmountCents: 200}},
	}); err != nil {
		t.Fatalf("split: %v", err)
	}
	// The audit log buffers writes: close flushes before reading.
	if err := al.Close(); err != nil {
		t.Fatalf("audit log close: %v", err)
	}
	events, corrupt, err := ReadAuditLog(al.Dir(), time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt = %d, want 0", corrupt)
	}
	found := false
	for _, ev := range events {
		if ev.Op == "split" && ev.TraceID == "sp-audit" {
			found = true
			if len(ev.EntryIDs) != 1 || ev.EntryIDs[0] != "sp-audit/USD" {
				t.Errorf("audit entry IDs = %v", ev.EntryIDs)
			}
		}
	}
	if !found {
		t.Error("split audit event not found")
	}
}
