package ledger

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fundMergeSource posts starting balances for the merge tests: credit
// legs fund the source, debit legs create negative balances (debt).
func fundMergeSource(t *testing.T, l *Ledger, legs ...JournalEntry) {
	t.Helper()
	for _, e := range legs {
		if _, _, err := l.Post(e); err != nil {
			t.Fatalf("fund %s: %v", e.ID, err)
		}
	}
}

func TestPostMergeBasic(t *testing.T) {
	l := New()
	fundMergeSource(t, l,
		JournalEntry{ID: "f1", DebitAccount: "src", CreditAccount: "bank", AmountCents: 1000, Currency: "USD"},
		JournalEntry{ID: "f2", DebitAccount: "src", CreditAccount: "bank", AmountCents: 500, Currency: "EUR"},
	)
	vb := l.versionOf()

	rcpt, err := l.PostMerge(Merge{ID: "mg1", From: "src", To: "dst", IdempotencyKey: "k-mg1"})
	if err != nil {
		t.Fatalf("PostMerge: %v", err)
	}
	if rcpt.MergeID != "mg1" || rcpt.From != "src" || rcpt.To != "dst" {
		t.Fatalf("receipt identity = %+v", rcpt)
	}
	if rcpt.Duplicate {
		t.Fatal("Duplicate = true on first merge")
	}
	if !rcpt.SourceFrozen {
		t.Fatal("SourceFrozen = false, want true")
	}
	// One leg per currency, currencies sorted.
	if len(rcpt.Legs) != 2 || len(rcpt.Entries) != 2 {
		t.Fatalf("legs = %d, entries = %d; want 2/2", len(rcpt.Legs), len(rcpt.Entries))
	}
	if rcpt.Legs[0].Currency != "EUR" || rcpt.Legs[1].Currency != "USD" {
		t.Fatalf("leg currencies = %v, want [EUR USD]", rcpt.Legs)
	}
	for _, leg := range rcpt.Legs {
		if leg.DebtAbsorbed {
			t.Errorf("leg %+v: DebtAbsorbed = true for a positive balance", leg)
		}
		if want := "mg1/" + leg.Currency; leg.EntryID != want {
			t.Errorf("leg entry ID = %q, want %q", leg.EntryID, want)
		}
	}
	// Source zeroed in every currency, target accumulated.
	if got := l.BalanceIn("src", "USD"); got != 0 {
		t.Errorf("src USD = %d, want 0", got)
	}
	if got := l.BalanceIn("src", "EUR"); got != 0 {
		t.Errorf("src EUR = %d, want 0", got)
	}
	if got := l.BalanceIn("dst", "USD"); got != 1000 {
		t.Errorf("dst USD = %d, want 1000", got)
	}
	if got := l.BalanceIn("dst", "EUR"); got != 500 {
		t.Errorf("dst EUR = %d, want 500", got)
	}
	// Source frozen: new postings through it are rejected, reads work.
	if !l.IsFrozen("src") {
		t.Fatal("src not frozen after merge")
	}
	if _, _, err := l.Post(JournalEntry{ID: "x1", DebitAccount: "src", CreditAccount: "bank", AmountCents: 1}); !errors.Is(err, ErrAccountFrozen) {
		t.Fatalf("post-merge Post through src: err = %v, want ErrAccountFrozen", err)
	}
	if got := l.BalanceIn("src", "USD"); got != 0 {
		t.Fatalf("Balance on frozen src = %d, want 0 (reads unaffected)", got)
	}
	// Two legs committed: version += 2. The freeze does not bump.
	if got := l.versionOf(); got != vb+2 {
		t.Fatalf("version = %d, want %d", got, vb+2)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Fatalf("accounting equation: %v", err)
	}
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("chain: %v", err)
	}
}

func TestPostMergeValidation(t *testing.T) {
	l := New()
	fundMergeSource(t, l,
		JournalEntry{ID: "f1", DebitAccount: "src", CreditAccount: "bank", AmountCents: 100, Currency: "USD"})

	cases := []struct {
		name string
		m    Merge
		want error
	}{
		{"empty ID", Merge{From: "src", To: "dst"}, ErrEmptyMergeID},
		{"empty source", Merge{ID: "m", To: "dst"}, ErrEmptyMergeSource},
		{"empty target", Merge{ID: "m", From: "src"}, ErrEmptyMergeTarget},
		{"same account", Merge{ID: "m", From: "src", To: "src"}, ErrMergeSameAccount},
		{"merge ID collides with entry", Merge{ID: "f1", From: "src", To: "dst"}, ErrMergeIDConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := l.PostMerge(tc.m); !errors.Is(err, tc.want) {
				t.Fatalf("PostMerge(%+v) = %v, want %v", tc.m, err, tc.want)
			}
		})
	}

	// A leg entry ID collision is also an ID conflict: pre-book the ID
	// the merge would use for its USD leg.
	if _, _, err := l.Post(JournalEntry{ID: "mgx/USD", DebitAccount: "a", CreditAccount: "b", AmountCents: 1}); err != nil {
		t.Fatalf("setup post: %v", err)
	}
	if _, err := l.PostMerge(Merge{ID: "mgx", From: "src", To: "dst"}); !errors.Is(err, ErrMergeIDConflict) {
		t.Fatalf("leg ID collision: err = %v, want ErrMergeIDConflict", err)
	}

	// Rejected merges record nothing: version unchanged, source not
	// frozen, no merge registered.
	vb := l.versionOf()
	if _, err := l.PostMerge(Merge{ID: "bad", From: "src", To: "src"}); err == nil {
		t.Fatal("want error")
	}
	if got := l.versionOf(); got != vb {
		t.Fatalf("version moved on rejection: %d -> %d", vb, got)
	}
	if l.IsFrozen("src") {
		t.Fatal("src frozen by a rejected merge")
	}
}

func TestPostMergeIdempotent(t *testing.T) {
	l := New()
	fundMergeSource(t, l,
		JournalEntry{ID: "f1", DebitAccount: "src", CreditAccount: "bank", AmountCents: 100, Currency: "USD"})

	first, err := l.PostMerge(Merge{ID: "mg1", From: "src", To: "dst", IdempotencyKey: "k1"})
	if err != nil {
		t.Fatalf("PostMerge: %v", err)
	}
	vb := l.versionOf()

	second, err := l.PostMerge(Merge{ID: "mg1-retry", From: "src", To: "dst", IdempotencyKey: "k1"})
	if err != nil {
		t.Fatalf("replay PostMerge: %v", err)
	}
	if !second.Duplicate {
		t.Fatal("replay: Duplicate = false, want true")
	}
	if second.MergeID != first.MergeID || len(second.Entries) != len(first.Entries) {
		t.Fatalf("replay receipt = %+v, want original %+v", second, first)
	}
	if got := l.versionOf(); got != vb {
		t.Fatalf("replay bumped version: %d -> %d", vb, got)
	}

	// A NEW merge from the now-frozen source fails — the freeze sticks —
	// but the keyed replay above still succeeded: replay precedes the
	// frozen check in the check order.
	if _, err := l.PostMerge(Merge{ID: "mg2", From: "src", To: "dst", IdempotencyKey: "k2"}); !errors.Is(err, ErrAccountFrozen) {
		t.Fatalf("second merge from frozen src: err = %v, want ErrAccountFrozen", err)
	}
}

func TestPostMergeFrozenChecks(t *testing.T) {
	l := New()
	fundMergeSource(t, l,
		JournalEntry{ID: "f1", DebitAccount: "src", CreditAccount: "bank", AmountCents: 100, Currency: "USD"})

	l.Freeze("src")
	if _, err := l.PostMerge(Merge{ID: "m1", From: "src", To: "dst"}); !errors.Is(err, ErrAccountFrozen) {
		t.Fatalf("frozen source: err = %v, want ErrAccountFrozen", err)
	}
	l.Unfreeze("src")

	l.Freeze("dst")
	if _, err := l.PostMerge(Merge{ID: "m2", From: "src", To: "dst"}); !errors.Is(err, ErrAccountFrozen) {
		t.Fatalf("frozen target: err = %v, want ErrAccountFrozen", err)
	}
}

func TestPostMergeOverdraft(t *testing.T) {
	l := New(WithOverdraftProtection("dst"))
	// src is overdrawn by 300 USD (debit bank / credit src).
	fundMergeSource(t, l,
		JournalEntry{ID: "f1", DebitAccount: "bank", CreditAccount: "src", AmountCents: 300, Currency: "USD"})
	// dst holds 100 USD: absorbing 300 of debt would take it to -200.
	fundMergeSource(t, l,
		JournalEntry{ID: "f2", DebitAccount: "dst", CreditAccount: "bank", AmountCents: 100, Currency: "USD"})

	if _, err := l.PostMerge(Merge{ID: "m1", From: "src", To: "dst"}); !errors.Is(err, ErrAccountOverdraft) {
		t.Fatalf("overdrawn merge: err = %v, want ErrAccountOverdraft", err)
	}
	// Rejected: nothing booked, nothing frozen.
	if got := l.BalanceIn("src", "USD"); got != -300 {
		t.Fatalf("src USD = %d after rejection, want -300", got)
	}
	if l.IsFrozen("src") {
		t.Fatal("src frozen by a rejected merge")
	}

	// With enough balance the debt absorption succeeds: dst 100 - 300 =
	// -200 is still rejected, so fund dst to 500 first... dst already has
	// 100; add 400 more via a fresh ledger to keep the arithmetic clear.
	l2 := New(WithOverdraftProtection("dst"))
	fundMergeSource(t, l2,
		JournalEntry{ID: "f1", DebitAccount: "bank", CreditAccount: "src", AmountCents: 300, Currency: "USD"})
	fundMergeSource(t, l2,
		JournalEntry{ID: "f2", DebitAccount: "dst", CreditAccount: "bank", AmountCents: 500, Currency: "USD"})
	rcpt, err := l2.PostMerge(Merge{ID: "m1", From: "src", To: "dst"})
	if err != nil {
		t.Fatalf("debt-absorbing merge: %v", err)
	}
	if len(rcpt.Legs) != 1 || !rcpt.Legs[0].DebtAbsorbed {
		t.Fatalf("legs = %+v, want one debt-absorbing leg", rcpt.Legs)
	}
	if rcpt.Legs[0].AmountCents != 300 {
		t.Fatalf("leg amount = %d, want 300", rcpt.Legs[0].AmountCents)
	}
	// The leg zeroes the source by debiting it and crediting the target.
	e := rcpt.Entries[0]
	if e.DebitAccount != "src" || e.CreditAccount != "dst" {
		t.Fatalf("debt leg direction = %s/%s, want debit src / credit dst", e.DebitAccount, e.CreditAccount)
	}
	if got := l2.BalanceIn("src", "USD"); got != 0 {
		t.Fatalf("src USD = %d, want 0", got)
	}
	if got := l2.BalanceIn("dst", "USD"); got != 200 {
		t.Fatalf("dst USD = %d, want 200", got)
	}
	if err := l2.VerifyAccountingEquation(); err != nil {
		t.Fatalf("accounting equation: %v", err)
	}
}

func TestPostMergeEmptySource(t *testing.T) {
	l := New()
	// src never touched: no legs, but the merge still succeeds and
	// freezes the source — decommissioning an empty account is the
	// point, not an error.
	rcpt, err := l.PostMerge(Merge{ID: "mg-empty", From: "src", To: "dst"})
	if err != nil {
		t.Fatalf("PostMerge: %v", err)
	}
	if len(rcpt.Legs) != 0 || len(rcpt.Entries) != 0 {
		t.Fatalf("legs/entries = %d/%d, want 0/0", len(rcpt.Legs), len(rcpt.Entries))
	}
	if !rcpt.SourceFrozen || !l.IsFrozen("src") {
		t.Fatal("empty merge did not freeze the source")
	}
}

func TestPostMergeKeyExpiry(t *testing.T) {
	l := New(WithIdempotencyTTL(time.Hour))
	old := time.Now().Add(-2 * time.Hour)
	fundMergeSource(t, l,
		JournalEntry{ID: "f1", DebitAccount: "src", CreditAccount: "bank", AmountCents: 100, Currency: "USD", CreatedAt: old})
	if _, err := l.PostMerge(Merge{ID: "mg1", From: "src", To: "dst", IdempotencyKey: "k1", CreatedAt: old}); err != nil {
		t.Fatalf("PostMerge: %v", err)
	}
	if n := l.ExpireIdempotencyKeys(); n == 0 {
		t.Fatal("ExpireIdempotencyKeys pruned nothing")
	}
	// The key is gone: reposting attempts a brand-new merge, which fails
	// on the frozen source. The merge registry (Reconcile history) is
	// untouched by key expiry.
	if _, err := l.PostMerge(Merge{ID: "mg2", From: "src", To: "dst", IdempotencyKey: "k1"}); !errors.Is(err, ErrAccountFrozen) {
		t.Fatalf("repost after key expiry: err = %v, want ErrAccountFrozen", err)
	}
	report := l.Reconcile(time.Now())
	if len(report.Merges) != 1 || report.Merges[0].MergeID != "mg1" {
		t.Fatalf("reconcile merges = %+v, want [mg1]", report.Merges)
	}
}

func TestPostMergeConcurrent(t *testing.T) {
	l := New()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		src := AccountID("src-" + string(rune('a'+g)))
		fundMergeSource(t, l,
			JournalEntry{ID: "fund-" + string(rune('a'+g)), DebitAccount: src, CreditAccount: "bank", AmountCents: 100, Currency: "USD"})
	}
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			src := AccountID("src-" + string(rune('a'+g)))
			if _, err := l.PostMerge(Merge{ID: "mg-" + string(rune('a'+g)), From: src, To: "dst"}); err != nil {
				t.Errorf("PostMerge %s: %v", src, err)
			}
		}(g)
	}
	wg.Wait()
	if got := l.BalanceIn("dst", "USD"); got != 800 {
		t.Fatalf("dst USD = %d, want 800", got)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Fatalf("accounting equation: %v", err)
	}
}

func TestReconcileReportsMerges(t *testing.T) {
	l := New()
	fundMergeSource(t, l,
		JournalEntry{ID: "f1", DebitAccount: "src", CreditAccount: "bank", AmountCents: 100, Currency: "USD"})
	if _, err := l.PostMerge(Merge{ID: "mg1", From: "src", To: "dst", IdempotencyKey: "k1"}); err != nil {
		t.Fatalf("PostMerge: %v", err)
	}
	report := l.Reconcile(time.Now())
	if len(report.Merges) != 1 {
		t.Fatalf("merges = %d, want 1", len(report.Merges))
	}
	m := report.Merges[0]
	if m.MergeID != "mg1" || m.FromAccount != "src" || m.ToAccount != "dst" {
		t.Fatalf("merge = %+v", m)
	}
	if len(m.Legs) != 1 || m.Legs[0].AmountCents != 100 {
		t.Fatalf("merge legs = %+v", m.Legs)
	}
	// The merged source shows up in frozen_accounts: cross-checkable.
	found := false
	for _, a := range report.FrozenAccounts {
		if a == "src" {
			found = true
		}
	}
	if !found {
		t.Fatalf("frozen_accounts = %v, want src listed", report.FrozenAccounts)
	}
}

func TestMergeSnapshotRoundTrip(t *testing.T) {
	src := New(WithIdempotencyTTL(time.Hour))
	fundMergeSource(t, src,
		JournalEntry{ID: "f1", DebitAccount: "src", CreditAccount: "bank", AmountCents: 1000, Currency: "USD"},
		JournalEntry{ID: "f2", DebitAccount: "bank", CreditAccount: "src", AmountCents: 200, Currency: "EUR"})
	if _, err := src.PostMerge(Merge{ID: "mg1", From: "src", To: "dst", IdempotencyKey: "k1"}); err != nil {
		t.Fatalf("PostMerge: %v", err)
	}

	var buf strings.Builder
	if err := src.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if !snapshotLedgersEqual(src, restored) {
		t.Fatal("restored ledger differs from source")
	}
	// Idempotency survives the restore: the keyed replay still returns
	// the original receipt instead of failing on the frozen source.
	rcpt, err := restored.PostMerge(Merge{ID: "mg-retry", From: "src", To: "dst", IdempotencyKey: "k1"})
	if err != nil {
		t.Fatalf("replay after restore: %v", err)
	}
	if !rcpt.Duplicate || rcpt.MergeID != "mg1" {
		t.Fatalf("replay receipt = %+v, want duplicate of mg1", rcpt)
	}
	if !restored.IsFrozen("src") {
		t.Fatal("src not frozen after restore")
	}
}

func TestMergeSnapshotTamperRejected(t *testing.T) {
	src := New()
	fundMergeSource(t, src,
		JournalEntry{ID: "f1", DebitAccount: "src", CreditAccount: "bank", AmountCents: 100, Currency: "USD"})
	if _, err := src.PostMerge(Merge{ID: "mg1", From: "src", To: "dst"}); err != nil {
		t.Fatalf("PostMerge: %v", err)
	}
	var buf strings.Builder
	if err := src.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	// Corrupt the merge record's entry reference: import must refuse.
	tampered := strings.Replace(buf.String(), `"entry_ids":["mg1/USD"]`, `"entry_ids":["nope"]`, 1)
	if tampered == buf.String() {
		t.Fatal("tamper did not change the snapshot")
	}
	if _, err := ImportSnapshot(strings.NewReader(tampered)); err == nil {
		t.Fatal("tampered merge snapshot imported without error")
	}
}

func TestMergeIncrementalSnapshot(t *testing.T) {
	a, b, base := buildDivergedPair(t)

	fundMergeSource(t, a,
		JournalEntry{ID: "fc1", DebitAccount: "old-corp", CreditAccount: "bank", AmountCents: 9000, Currency: "USD"})
	if _, err := a.PostMerge(Merge{ID: "mg-corp", From: "old-corp", To: "new-corp", IdempotencyKey: "k-mg"}); err != nil {
		t.Fatalf("PostMerge: %v", err)
	}

	var delta strings.Builder
	if err := a.ExportIncrementalSnapshot(&delta, base); err != nil {
		t.Fatalf("export delta: %v", err)
	}
	if err := b.ImportIncrementalSnapshot(strings.NewReader(delta.String())); err != nil {
		t.Fatalf("import delta: %v", err)
	}
	if !snapshotLedgersEqual(a, b) {
		t.Fatal("replica diverged after merge delta")
	}
	if !b.IsFrozen("old-corp") {
		t.Fatal("replica: old-corp not frozen")
	}
	// The replay works on the replica too.
	rcpt, err := b.PostMerge(Merge{ID: "mg-retry", From: "old-corp", To: "new-corp", IdempotencyKey: "k-mg"})
	if err != nil {
		t.Fatalf("replica replay: %v", err)
	}
	if !rcpt.Duplicate {
		t.Fatal("replica replay: Duplicate = false")
	}
}
