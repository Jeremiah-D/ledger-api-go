package ledger

import (
	"testing"
	"time"
)

func clearingLegs() []ClearingLeg {
	return []ClearingLeg{
		{ID: "leg1", From: "alice", To: "bob", AmountCents: 100, Currency: "USD"},
		{ID: "leg2", From: "bob", To: "carol", AmountCents: 100, Currency: "USD"},
		{ID: "leg3", From: "alice", To: "carol", AmountCents: 50, Currency: "USD"},
	}
}

func mustOpenClearingBatch(t *testing.T, l *Ledger, id string, legs []ClearingLeg) ClearingBatch {
	t.Helper()
	batch, err := l.OpenClearingBatch(id, legs, "")
	if err != nil {
		t.Fatalf("OpenClearingBatch: %v", err)
	}
	return batch
}

func TestOpenClearingBatchValidation(t *testing.T) {
	good := clearingLegs()
	cases := []struct {
		name  string
		id    string
		legs  []ClearingLeg
		mutate func([]ClearingLeg) []ClearingLeg
		want  error
	}{
		{"empty ID", "", good, nil, ErrEmptyClearingBatchID},
		{"no legs", "cb-x", nil, nil, ErrEmptyClearingBatch},
		{"empty leg ID", "cb-x", good, func(ls []ClearingLeg) []ClearingLeg {
			ls[0].ID = ""
			return ls
		}, ErrEmptyClearingLegID},
		{"duplicate leg ID", "cb-x", good, func(ls []ClearingLeg) []ClearingLeg {
			ls[1].ID = ls[0].ID
			return ls
		}, ErrDuplicateClearingLegID},
		{"empty from", "cb-x", good, func(ls []ClearingLeg) []ClearingLeg {
			ls[0].From = ""
			return ls
		}, ErrEmptyClearingLegFrom},
		{"empty to", "cb-x", good, func(ls []ClearingLeg) []ClearingLeg {
			ls[0].To = ""
			return ls
		}, ErrEmptyClearingLegTo},
		{"same account", "cb-x", good, func(ls []ClearingLeg) []ClearingLeg {
			ls[0].To = ls[0].From
			return ls
		}, ErrClearingLegSameAccount},
		{"zero amount", "cb-x", good, func(ls []ClearingLeg) []ClearingLeg {
			ls[0].AmountCents = 0
			return ls
		}, ErrInvalidClearingLegAmount},
		{"bad currency", "cb-x", good, func(ls []ClearingLeg) []ClearingLeg {
			ls[0].Currency = "xx"
			return ls
		}, ErrInvalidCurrency},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := New()
			legs := append([]ClearingLeg(nil), tc.legs...)
			if tc.mutate != nil {
				legs = tc.mutate(legs)
			}
			if _, err := l.OpenClearingBatch(tc.id, legs, ""); err != tc.want {
				t.Errorf("OpenClearingBatch err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestOpenClearingBatchDuplicateIDAndKey(t *testing.T) {
	l := New()
	mustOpenClearingBatch(t, l, "cb-dup", clearingLegs())
	if _, err := l.OpenClearingBatch("cb-dup", clearingLegs(), ""); err != ErrClearingBatchIDTaken {
		t.Errorf("second open err = %v, want %v", err, ErrClearingBatchIDTaken)
	}

	// Idempotent open with a key.
	first, err := l.OpenClearingBatch("cb-key", clearingLegs(), "open-cb-key")
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	second, err := l.OpenClearingBatch("cb-key", clearingLegs(), "open-cb-key")
	if err != nil {
		t.Fatalf("replay open: %v", err)
	}
	if first.ID != second.ID || len(second.Legs) != 3 {
		t.Errorf("replay returned a different batch: %+v", second)
	}
	// The legs are untouched by the open: nothing journaled.
	if got := len(l.Entries()); got != 0 {
		t.Errorf("open journaled %d entries, want 0", got)
	}
}

func TestSettleClearingBatchNetsLegs(t *testing.T) {
	l := New()
	mustOpenClearingBatch(t, l, "cb-net", clearingLegs())
	now := time.Date(2026, 3, 31, 12, 0, 0, 0, time.UTC)

	batch, err := l.SettleClearingBatch("cb-net", now)
	if err != nil {
		t.Fatalf("SettleClearingBatch: %v", err)
	}
	if batch.Status != ClearingSettled {
		t.Errorf("status = %q, want %q", batch.Status, ClearingSettled)
	}
	if !batch.SettledAt.Equal(now) {
		t.Errorf("settledAt = %v, want %v", batch.SettledAt, now)
	}
	// Nets: alice -150, bob 0, carol +150. One row: debit carol (net
	// payee), credit alice (net payer), 150. Bob nets to zero and
	// drops out.
	if len(batch.EntryIDs) != 1 || batch.EntryIDs[0] != "clearing/cb-net/1" {
		t.Fatalf("entry IDs = %v, want [clearing/cb-net/1]", batch.EntryIDs)
	}
	entries := l.Entries()
	if len(entries) != 1 {
		t.Fatalf("journaled %d entries, want 1", len(entries))
	}
	e := entries[0]
	if e.DebitAccount != "carol" || e.CreditAccount != "alice" || e.AmountCents != 150 {
		t.Errorf("net entry = debit %s credit %s amount %d; want debit carol credit alice amount 150",
			e.DebitAccount, e.CreditAccount, e.AmountCents)
	}
	if e.BatchID != "clearing/cb-net" {
		t.Errorf("BatchID = %q, want clearing/cb-net", e.BatchID)
	}
	if e.Currency != "USD" {
		t.Errorf("currency = %q, want USD", e.Currency)
	}
	if got := l.BalanceIn("alice", "USD"); got != -150 {
		t.Errorf("alice balance = %d, want -150", got)
	}
	if got := l.BalanceIn("carol", "USD"); got != 150 {
		t.Errorf("carol balance = %d, want 150", got)
	}
	if got := l.BalanceIn("bob", "USD"); got != 0 {
		t.Errorf("bob balance = %d, want 0", got)
	}

	// Re-settling is idempotent.
	n := len(l.Entries())
	again, err := l.SettleClearingBatch("cb-net", now)
	if err != nil {
		t.Fatalf("second settle: %v", err)
	}
	if again.Status != ClearingSettled || len(again.EntryIDs) != 1 {
		t.Errorf("second settle changed the batch: %+v", again)
	}
	if got := len(l.Entries()); got != n {
		t.Errorf("second settle journaled %d new entries", got-n)
	}
}

func TestSettleClearingBatchMultiCurrency(t *testing.T) {
	l := New()
	legs := []ClearingLeg{
		{ID: "l1", From: "alice", To: "bob", AmountCents: 100, Currency: "USD"},
		{ID: "l2", From: "alice", To: "bob", AmountCents: 200, Currency: "EUR"},
	}
	mustOpenClearingBatch(t, l, "cb-fx", legs)
	batch, err := l.SettleClearingBatch("cb-fx", time.Now())
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if len(batch.EntryIDs) != 2 {
		t.Fatalf("entry IDs = %v, want 2 rows (one per currency)", batch.EntryIDs)
	}
	if got := l.BalanceIn("bob", "USD"); got != 100 {
		t.Errorf("bob USD = %d, want 100", got)
	}
	if got := l.BalanceIn("bob", "EUR"); got != 200 {
		t.Errorf("bob EUR = %d, want 200", got)
	}
}

func TestSettleClearingBatchAtomicRollback(t *testing.T) {
	l := New()
	mustOpenClearingBatch(t, l, "cb-rollback", clearingLegs())

	// Freeze the net debtor: the settle is rejected as a whole and the
	// batch stays open with zero partial rows.
	l.Freeze("alice")
	if _, err := l.SettleClearingBatch("cb-rollback", time.Now()); err == nil {
		t.Fatal("settle with frozen debtor: want error, got nil")
	}
	batch, ok := l.ClearingBatch("cb-rollback")
	if !ok {
		t.Fatal("batch missing after failed settle")
	}
	if batch.Status != ClearingOpen {
		t.Errorf("status = %q, want %q after failed settle", batch.Status, ClearingOpen)
	}
	if len(batch.EntryIDs) != 0 {
		t.Errorf("failed settle recorded entry IDs: %v", batch.EntryIDs)
	}
	if got := len(l.Entries()); got != 0 {
		t.Errorf("failed settle journaled %d entries, want 0", got)
	}

	// Fix the cause and settle again: the retry books the full net.
	l.Unfreeze("alice")
	batch, err := l.SettleClearingBatch("cb-rollback", time.Now())
	if err != nil {
		t.Fatalf("retry settle: %v", err)
	}
	if batch.Status != ClearingSettled || len(batch.EntryIDs) != 1 {
		t.Errorf("retry settle = %+v, want settled with 1 entry", batch)
	}
}

func TestSettleClearingBatchZeroNet(t *testing.T) {
	l := New()
	legs := []ClearingLeg{
		{ID: "l1", From: "alice", To: "bob", AmountCents: 100, Currency: "USD"},
		{ID: "l2", From: "bob", To: "alice", AmountCents: 100, Currency: "USD"},
	}
	mustOpenClearingBatch(t, l, "cb-zero", legs)
	batch, err := l.SettleClearingBatch("cb-zero", time.Now())
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if batch.Status != ClearingSettled {
		t.Errorf("status = %q, want %q", batch.Status, ClearingSettled)
	}
	if len(batch.EntryIDs) != 0 {
		t.Errorf("zero-net settle booked entries: %v", batch.EntryIDs)
	}
	if got := len(l.Entries()); got != 0 {
		t.Errorf("zero-net settle journaled %d entries, want 0", got)
	}
}

func TestCancelClearingBatch(t *testing.T) {
	l := New()
	mustOpenClearingBatch(t, l, "cb-cancel", clearingLegs())

	batch, err := l.CancelClearingBatch("cb-cancel")
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if batch.Status != ClearingCancelled {
		t.Errorf("status = %q, want %q", batch.Status, ClearingCancelled)
	}
	// Cancelling again is idempotent.
	if _, err := l.CancelClearingBatch("cb-cancel"); err != nil {
		t.Fatalf("second cancel: %v", err)
	}
	// Settling a cancelled batch is rejected; nothing was ever
	// journaled.
	if _, err := l.SettleClearingBatch("cb-cancel", time.Now()); err != ErrClearingBatchTerminal {
		t.Errorf("settle cancelled err = %v, want %v", err, ErrClearingBatchTerminal)
	}
	if got := len(l.Entries()); got != 0 {
		t.Errorf("cancelled batch journaled %d entries", got)
	}
	// Cancelling a settled batch is rejected: settled rows stay.
	mustOpenClearingBatch(t, l, "cb-settled", clearingLegs())
	if _, err := l.SettleClearingBatch("cb-settled", time.Now()); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if _, err := l.CancelClearingBatch("cb-settled"); err != ErrClearingBatchTerminal {
		t.Errorf("cancel settled err = %v, want %v", err, ErrClearingBatchTerminal)
	}
	if _, err := l.CancelClearingBatch("missing"); err != ErrClearingBatchNotFound {
		t.Errorf("cancel missing err = %v, want %v", err, ErrClearingBatchNotFound)
	}
}

func TestReconcileListsClearingBatches(t *testing.T) {
	l := New()
	mustOpenClearingBatch(t, l, "cb-b", clearingLegs())
	mustOpenClearingBatch(t, l, "cb-a", clearingLegs())
	mustOpenClearingBatch(t, l, "cb-c", clearingLegs())
	if _, err := l.SettleClearingBatch("cb-a", time.Now()); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if _, err := l.CancelClearingBatch("cb-c"); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	report := l.Reconcile(time.Now())
	if len(report.ClearingBatches) != 3 {
		t.Fatalf("reconcile listed %d batches, want 3", len(report.ClearingBatches))
	}
	// Sorted by ID, with the right statuses.
	want := []struct {
		id     string
		status string
	}{
		{"cb-a", ClearingSettled},
		{"cb-b", ClearingOpen},
		{"cb-c", ClearingCancelled},
	}
	for i, w := range want {
		got := report.ClearingBatches[i]
		if got.ID != w.id || got.Status != w.status {
			t.Errorf("row %d = (%s, %s), want (%s, %s)", i, got.ID, got.Status, w.id, w.status)
		}
		if got.Legs != 3 {
			t.Errorf("row %d legs = %d, want 3", i, got.Legs)
		}
		if len(got.Currencies) != 1 || got.Currencies[0] != "USD" {
			t.Errorf("row %d currencies = %v, want [USD]", i, got.Currencies)
		}
	}
	if report.ClearingBatches[0].SettledAt.IsZero() {
		t.Error("settled batch summary missing SettledAt")
	}
	if !report.ClearingBatches[1].SettledAt.IsZero() {
		t.Error("open batch summary carries SettledAt")
	}
}

func TestClearingBatchesSorted(t *testing.T) {
	l := New()
	mustOpenClearingBatch(t, l, "cb-z", clearingLegs())
	mustOpenClearingBatch(t, l, "cb-m", clearingLegs())
	batches := l.ClearingBatches()
	if len(batches) != 2 || batches[0].ID != "cb-m" || batches[1].ID != "cb-z" {
		t.Errorf("batches not sorted by ID: %+v", batches)
	}
	if _, ok := l.ClearingBatch("missing"); ok {
		t.Error("ClearingBatch(missing) returned ok")
	}
}
