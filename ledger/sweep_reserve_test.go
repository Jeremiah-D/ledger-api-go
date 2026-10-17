package ledger

import (
	"bytes"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

var sweepReserveFundSeq atomic.Int64

// fundSweepAccount posts a one-leg funding entry: debit account, credit
// the funding source, in the default currency. Entry IDs are sequence
// numbers so repeated fundings of the same account never collide.
func fundSweepAccount(t *testing.T, l *Ledger, account string, cents int64) {
	t.Helper()
	id := fmt.Sprintf("sweep-fund-%d", sweepReserveFundSeq.Add(1))
	_, _, err := l.Post(JournalEntry{
		ID:            id,
		DebitAccount:  AccountID(account),
		CreditAccount: "funding",
		AmountCents:   cents,
		Currency:      "USD",
	})
	if err != nil {
		t.Fatalf("fund %s: %v", account, err)
	}
}

func TestSetSweepReserveValidation(t *testing.T) {
	l := New()
	if err := l.SetSweepReserve("sub-a", "USD", 0); !errors.Is(err, ErrInvalidSweepReserve) {
		t.Errorf("zero floor: err=%v, want ErrInvalidSweepReserve", err)
	}
	if err := l.SetSweepReserve("sub-a", "USD", -100); !errors.Is(err, ErrInvalidSweepReserve) {
		t.Errorf("negative floor: err=%v, want ErrInvalidSweepReserve", err)
	}
	if err := l.SetSweepReserve("sub-a", "usd", 100); err == nil {
		t.Errorf("lowercase currency: want an error, got nil")
	}
	if err := l.SetSweepReserve("sub-a", "USD", 100); err != nil {
		t.Fatalf("valid floor: %v", err)
	}
	floor, ok := l.SweepReserve("sub-a", "USD")
	if !ok || floor != 100 {
		t.Errorf("SweepReserve = (%d, %v), want (100, true)", floor, ok)
	}
	// Currency defaults to USD on the read path, mirroring the write.
	if _, ok := l.SweepReserve("sub-a", ""); !ok {
		t.Errorf("SweepReserve with empty currency: want the USD reserve")
	}
	if _, ok := l.SweepReserve("sub-a", "EUR"); ok {
		t.Errorf("SweepReserve EUR: want no reserve configured")
	}
}

func TestClearSweepReserve(t *testing.T) {
	l := New()
	// Clearing an unset reserve is a silent no-op.
	if err := l.ClearSweepReserve("sub-a", "USD"); err != nil {
		t.Fatalf("clear unset: %v", err)
	}
	if err := l.SetSweepReserve("sub-a", "USD", 500); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := l.ClearSweepReserve("sub-a", "USD"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, ok := l.SweepReserve("sub-a", "USD"); ok {
		t.Errorf("reserve still configured after clear")
	}
}

func TestSweepReserveExactSweep(t *testing.T) {
	l := New()
	fundSweepAccount(t, l, "sub-a", 10000)
	fundSweepAccount(t, l, "sub-b", 5000)
	if err := l.SetSweepReserve("sub-a", "USD", 3000); err != nil {
		t.Fatalf("set reserve: %v", err)
	}

	receipt, err := l.PostSweep(Sweep{ID: "sweep-res-1", From: []AccountID{"sub-a", "sub-b"}, To: "treasury"})
	if err != nil {
		t.Fatalf("PostSweep: %v", err)
	}
	if len(receipt.Legs) != 2 {
		t.Fatalf("legs = %d, want 2", len(receipt.Legs))
	}
	// sub-a keeps its 3000 floor: only 7000 moves.
	if receipt.Legs[0].From != "sub-a" || receipt.Legs[0].AmountCents != 7000 {
		t.Errorf("sub-a leg = %+v, want 7000 cents", receipt.Legs[0])
	}
	// sub-b has no reserve: full 5000 moves.
	if receipt.Legs[1].From != "sub-b" || receipt.Legs[1].AmountCents != 5000 {
		t.Errorf("sub-b leg = %+v, want 5000 cents", receipt.Legs[1])
	}
	if bal := l.BalanceIn("sub-a", "USD"); bal != 3000 {
		t.Errorf("sub-a balance = %d, want 3000", bal)
	}
	if bal := l.BalanceIn("sub-b", "USD"); bal != 0 {
		t.Errorf("sub-b balance = %d, want 0", bal)
	}
	if bal := l.BalanceIn("treasury", "USD"); bal != 12000 {
		t.Errorf("treasury balance = %d, want 12000", bal)
	}
}

func TestSweepReserveFloorAboveBalance(t *testing.T) {
	l := New()
	fundSweepAccount(t, l, "sub-a", 1000)
	if err := l.SetSweepReserve("sub-a", "USD", 2000); err != nil {
		t.Fatalf("set reserve: %v", err)
	}

	// The floor consumes the whole balance: the leg does not land, but
	// the sweep still succeeds with an empty receipt — and it still
	// registers its idempotency key, so a replay returns the same empty
	// receipt instead of sweeping funds that arrive later.
	receipt, err := l.PostSweep(Sweep{ID: "sweep-res-2", From: []AccountID{"sub-a"}, To: "treasury", IdempotencyKey: "res-key-1"})
	if err != nil {
		t.Fatalf("PostSweep: %v", err)
	}
	if len(receipt.Legs) != 0 || len(receipt.Entries) != 0 {
		t.Errorf("legs/entries = %d/%d, want empty", len(receipt.Legs), len(receipt.Entries))
	}
	if receipt.Duplicate {
		t.Errorf("duplicate = true on first sweep, want false")
	}
	if bal := l.BalanceIn("sub-a", "USD"); bal != 1000 {
		t.Errorf("sub-a balance = %d, want 1000", bal)
	}

	// Fund more so the balance exceeds the floor, then replay the key:
	// the replay must return the original empty receipt, not sweep the
	// newly arrived funds.
	fundSweepAccount(t, l, "sub-a", 5000)
	replay, err := l.PostSweep(Sweep{ID: "sweep-res-2-retry", From: []AccountID{"sub-a"}, To: "treasury", IdempotencyKey: "res-key-1"})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !replay.Duplicate || len(replay.Legs) != 0 {
		t.Errorf("replay = duplicate=%v legs=%d, want duplicate=true empty", replay.Duplicate, len(replay.Legs))
	}
	if bal := l.BalanceIn("sub-a", "USD"); bal != 6000 {
		t.Errorf("sub-a balance after replay = %d, want 6000 (untouched)", bal)
	}
}

func TestSweepReserveFrozenBeforeReserve(t *testing.T) {
	l := New()
	fundSweepAccount(t, l, "sub-a", 10000)
	if err := l.SetSweepReserve("sub-a", "USD", 3000); err != nil {
		t.Fatalf("set reserve: %v", err)
	}
	l.Freeze("sub-a")
	// Frozen rejects first: a frozen source never silently sweeps a
	// reserve-reduced amount.
	if _, err := l.PostSweep(Sweep{ID: "sweep-res-3", From: []AccountID{"sub-a"}, To: "treasury"}); !errors.Is(err, ErrAccountFrozen) {
		t.Errorf("PostSweep frozen source: err=%v, want ErrAccountFrozen", err)
	}
}

func TestSweepReserveIdempotentReplay(t *testing.T) {
	l := New()
	fundSweepAccount(t, l, "sub-a", 10000)
	if err := l.SetSweepReserve("sub-a", "USD", 3000); err != nil {
		t.Fatalf("set reserve: %v", err)
	}
	first, err := l.PostSweep(Sweep{ID: "sweep-res-4", From: []AccountID{"sub-a"}, To: "treasury", IdempotencyKey: "res-key-2"})
	if err != nil {
		t.Fatalf("PostSweep: %v", err)
	}
	second, err := l.PostSweep(Sweep{ID: "sweep-res-4-retry", From: []AccountID{"sub-a"}, To: "treasury", IdempotencyKey: "res-key-2"})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !second.Duplicate {
		t.Errorf("duplicate = false, want true")
	}
	if len(second.Legs) != 1 || second.Legs[0].AmountCents != 7000 {
		t.Errorf("replay legs = %+v, want the original 7000-cent leg", second.Legs)
	}
	if first.Legs[0].EntryID != second.Legs[0].EntryID {
		t.Errorf("replay entry ID = %s, want %s", second.Legs[0].EntryID, first.Legs[0].EntryID)
	}
	// The replay books nothing new: the floor still holds 3000.
	if bal := l.BalanceIn("sub-a", "USD"); bal != 3000 {
		t.Errorf("sub-a balance = %d, want 3000", bal)
	}
}

func TestSweepReserveSnapshotRoundTrip(t *testing.T) {
	l := New()
	if err := l.SetSweepReserve("sub-a", "USD", 3000); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := l.SetSweepReserve("sub-a", "EUR", 1500); err != nil {
		t.Fatalf("set: %v", err)
	}
	fundSweepAccount(t, l, "sub-a", 10000)

	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("export: %v", err)
	}
	restored, err := ImportSnapshot(&buf)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if floor, ok := restored.SweepReserve("sub-a", "USD"); !ok || floor != 3000 {
		t.Errorf("restored USD reserve = (%d, %v), want (3000, true)", floor, ok)
	}
	if floor, ok := restored.SweepReserve("sub-a", "EUR"); !ok || floor != 1500 {
		t.Errorf("restored EUR reserve = (%d, %v), want (1500, true)", floor, ok)
	}
	// The reserve applies on the restored ledger too.
	receipt, err := restored.PostSweep(Sweep{ID: "sweep-res-5", From: []AccountID{"sub-a"}, To: "treasury"})
	if err != nil {
		t.Fatalf("PostSweep on restored: %v", err)
	}
	if len(receipt.Legs) != 1 || receipt.Legs[0].AmountCents != 7000 {
		t.Errorf("restored sweep legs = %+v, want one 7000-cent leg", receipt.Legs)
	}
}

func TestSweepReserveIncrementalSnapshotRoundTrip(t *testing.T) {
	l := New()
	if err := l.SetSweepReserve("sub-a", "USD", 3000); err != nil {
		t.Fatalf("set: %v", err)
	}
	fundSweepAccount(t, l, "sub-a", 10000)
	base := l.Version()

	// Change the reserve after the base version: the incremental config
	// section carries the latest config wholesale.
	if err := l.SetSweepReserve("sub-a", "USD", 4000); err != nil {
		t.Fatalf("set: %v", err)
	}
	var buf bytes.Buffer
	if err := l.ExportIncrementalSnapshot(&buf, base); err != nil {
		t.Fatalf("export: %v", err)
	}

	// A replica at the base version: the same funding posting, no
	// reserves.
	replica := New()
	replicaFundID := fmt.Sprintf("sweep-replica-fund-%d", sweepReserveFundSeq.Add(1))
	_, _, err := replica.Post(JournalEntry{
		ID:            replicaFundID,
		DebitAccount:  "sub-a",
		CreditAccount: "funding",
		AmountCents:   10000,
		Currency:      "USD",
	})
	if err != nil {
		t.Fatalf("replica fund: %v", err)
	}
	if replica.Version() != base {
		t.Fatalf("replica version = %d, want base %d", replica.Version(), base)
	}
	if err := replica.ImportIncrementalSnapshot(&buf); err != nil {
		t.Fatalf("incremental import: %v", err)
	}
	if floor, ok := replica.SweepReserve("sub-a", "USD"); !ok || floor != 4000 {
		t.Errorf("replica reserve = (%d, %v), want (4000, true)", floor, ok)
	}
}

func TestSweepReserveReconcileDisclosure(t *testing.T) {
	l := New()
	if err := l.SetSweepReserve("sub-b", "USD", 3000); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := l.SetSweepReserve("sub-a", "EUR", 1500); err != nil {
		t.Fatalf("set: %v", err)
	}
	report := l.Reconcile(time.Now())
	if len(report.SweepReserves) != 2 {
		t.Fatalf("sweep_reserves = %d rows, want 2", len(report.SweepReserves))
	}
	// Sorted by (account, currency).
	if report.SweepReserves[0].Account != "sub-a" || report.SweepReserves[0].Currency != "EUR" || report.SweepReserves[0].FloorCents != 1500 {
		t.Errorf("row 0 = %+v, want sub-a/EUR/1500", report.SweepReserves[0])
	}
	if report.SweepReserves[1].Account != "sub-b" || report.SweepReserves[1].Currency != "USD" || report.SweepReserves[1].FloorCents != 3000 {
		t.Errorf("row 1 = %+v, want sub-b/USD/3000", report.SweepReserves[1])
	}
}

func TestSweepReserveAuditTrail(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	l := New(WithAuditLog(al))
	if err := l.SetSweepReserve("sub-a", "USD", 3000); err != nil {
		t.Fatalf("set: %v", err)
	}
	// Repeating the identical floor is a silent no-op: no error, and no
	// second audit event.
	if err := l.SetSweepReserve("sub-a", "USD", 3000); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if err := l.SetSweepReserve("sub-a", "USD", 4000); err != nil {
		t.Fatalf("change: %v", err)
	}
	if err := l.ClearSweepReserve("sub-a", "USD"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatal(err)
	}
	events, corrupt, err := ReadAuditLog(dir, time.Now().UTC().Format("2006-01-02"))
	if err != nil || corrupt != 0 {
		t.Fatalf("ReadAuditLog: %v corrupt=%d", err, corrupt)
	}
	found := map[string]int{}
	for _, e := range events {
		found[e.Op]++
	}
	if found["sweep_reserve_set"] != 1 {
		t.Errorf("sweep_reserve_set events = %d, want 1", found["sweep_reserve_set"])
	}
	if found["sweep_reserve_changed"] != 1 {
		t.Errorf("sweep_reserve_changed events = %d, want 1", found["sweep_reserve_changed"])
	}
	if found["sweep_reserve_cleared"] != 1 {
		t.Errorf("sweep_reserve_cleared events = %d, want 1", found["sweep_reserve_cleared"])
	}
	// Structural changes never bump the journal version.
	if l.Version() != 0 {
		t.Errorf("version = %d after reserve changes, want 0", l.Version())
	}
}
