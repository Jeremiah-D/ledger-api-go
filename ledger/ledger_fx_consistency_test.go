package ledger

import (
	"context"
	"errors"
	"testing"
	"time"
)

// mustSetRate lives in ledger_fx_test.go and is reused here.

func TestScanFXRateInconsistencyConsistent(t *testing.T) {
	l := New()
	// Exact-1 triangle: (1/2)*(1/3)*(6/1) = 1.
	mustSetRate(t, l, "USD", "EUR", 1, 2)
	mustSetRate(t, l, "EUR", "JPY", 1, 3)
	mustSetRate(t, l, "JPY", "USD", 6, 1)

	if alerts := l.ScanFXRateInconsistency(1); len(alerts) != 0 {
		t.Fatalf("got %d alerts, want 0: %+v", len(alerts), alerts)
	}
}

func TestScanFXRateInconsistencyAlert(t *testing.T) {
	l := New()
	mustSetRate(t, l, "USD", "EUR", 1, 1)
	mustSetRate(t, l, "EUR", "JPY", 1, 1)
	mustSetRate(t, l, "JPY", "USD", 99, 100)

	// Cycle (EUR,JPY,USD): EUR→JPY 1/1, JPY→USD 99/100, USD→EUR 1/1.
	// Product = 99/100, |P-1| = 1/100 = 100 bps. The reverse cycle
	// needs EUR→USD/USD→JPY/JPY→EUR legs, which are absent.
	alerts := l.ScanFXRateInconsistency(50)
	if len(alerts) != 1 {
		t.Fatalf("got %d alerts, want 1: %+v", len(alerts), alerts)
	}
	a := alerts[0]
	if a.Path != [3]string{"EUR", "JPY", "USD"} {
		t.Errorf("path = %v, want [EUR JPY USD]", a.Path)
	}
	if a.Product != "99/100" {
		t.Errorf("product = %q, want 99/100", a.Product)
	}
	if a.DeviationBps != "100" {
		t.Errorf("deviation_bps = %q, want 100", a.DeviationBps)
	}
	// Exact big.Rat math: no float involved.
	if a.Legs[0].Num != 1 || a.Legs[1].Num != 99 || a.Legs[1].Den != 100 || a.Legs[2].Num != 1 {
		t.Errorf("legs = %+v, want 1/1, 99/100, 1/1", a.Legs)
	}
}

func TestScanFXRateInconsistencyThresholdBoundary(t *testing.T) {
	l := New()
	mustSetRate(t, l, "USD", "EUR", 1, 1)
	mustSetRate(t, l, "EUR", "JPY", 1, 1)
	mustSetRate(t, l, "JPY", "USD", 99, 100)

	// Deviation is exactly 100 bps: strictly greater fires, equal does
	// not.
	if alerts := l.ScanFXRateInconsistency(100); len(alerts) != 0 {
		t.Fatalf("threshold == deviation: got %d alerts, want 0", len(alerts))
	}
	if alerts := l.ScanFXRateInconsistency(99); len(alerts) != 1 {
		t.Fatalf("threshold 99: got %d alerts, want 1", len(alerts))
	}
}

func TestScanFXRateInconsistencyDisabled(t *testing.T) {
	l := New()
	mustSetRate(t, l, "USD", "EUR", 1, 1)
	mustSetRate(t, l, "EUR", "JPY", 1, 1)
	mustSetRate(t, l, "JPY", "USD", 1, 2)

	if alerts := l.ScanFXRateInconsistency(0); len(alerts) != 0 {
		t.Fatalf("threshold 0: got %d alerts, want 0 (disabled)", len(alerts))
	}
	if alerts := l.ScanFXRateInconsistency(-5); len(alerts) != 0 {
		t.Fatalf("threshold -5: got %d alerts, want 0 (disabled)", len(alerts))
	}
}

func TestScanFXRateInconsistencyBothCycles(t *testing.T) {
	l := New()
	// All six legs present: both directed cycles are checkable.
	mustSetRate(t, l, "USD", "EUR", 1, 1)
	mustSetRate(t, l, "EUR", "USD", 1, 1)
	mustSetRate(t, l, "EUR", "JPY", 1, 1)
	mustSetRate(t, l, "JPY", "EUR", 1, 1)
	mustSetRate(t, l, "JPY", "USD", 99, 100)
	mustSetRate(t, l, "USD", "JPY", 99, 100)

	alerts := l.ScanFXRateInconsistency(50)
	if len(alerts) != 2 {
		t.Fatalf("got %d alerts, want 2: %+v", len(alerts), alerts)
	}
	// Deterministic order: canonical paths sorted.
	if alerts[0].Path != [3]string{"EUR", "JPY", "USD"} ||
		alerts[1].Path != [3]string{"EUR", "USD", "JPY"} {
		t.Errorf("paths = %v, %v, want [EUR JPY USD], [EUR USD JPY]", alerts[0].Path, alerts[1].Path)
	}
}

// ReportFXRateInconsistency is the dumb emitter by design (LG-68
// convention): it records every alert it is handed and bumps the
// counter. Edge-trigger dedup lives in FXConsistencyWatch.tick.
func TestReportFXRateInconsistencyEmits(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	mustSetRate(t, l, "USD", "EUR", 1, 1)
	mustSetRate(t, l, "EUR", "JPY", 1, 1)
	mustSetRate(t, l, "JPY", "USD", 99, 100)

	report := func() {
		l.ReportFXRateInconsistency(l.ScanFXRateInconsistency(50))
	}
	report()
	report()
	if got := l.FXRateInconsistencyAlertCount(); got != 2 {
		t.Fatalf("count = %d, want 2 (one per report call, no dedup here)", got)
	}

	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, corrupt, err := ReadAuditLog(al.Dir(), time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt=%d, want 0", corrupt)
	}
	var found int
	for _, e := range events {
		if e.Op != "fx_rate_inconsistent" {
			continue
		}
		found++
		if e.Actor != "FXConsistencyWatch" {
			t.Errorf("actor = %q, want FXConsistencyWatch", e.Actor)
		}
		if e.Details["path"] != "EUR->JPY->USD->EUR" {
			t.Errorf("path = %v, want EUR->JPY->USD->EUR", e.Details["path"])
		}
		if e.Details["product"] != "99/100" {
			t.Errorf("product = %v, want 99/100", e.Details["product"])
		}
		if e.Details["deviation_bps"] != "100" {
			t.Errorf("deviation_bps = %v, want 100", e.Details["deviation_bps"])
		}
	}
	if found != 2 {
		t.Errorf("fx_rate_inconsistent events = %d, want 2", found)
	}
}

func TestFXConsistencyWatchEdgeTrigger(t *testing.T) {
	l := New()
	mustSetRate(t, l, "USD", "EUR", 1, 1)
	mustSetRate(t, l, "EUR", "JPY", 1, 1)
	mustSetRate(t, l, "JPY", "USD", 99, 100)

	var fired [][]FXRateInconsistency
	w := StartFXConsistencyWatch(context.Background(), l, time.Hour, 50, func(as []FXRateInconsistency) {
		fired = append(fired, as)
	})
	if w == nil {
		t.Fatal("watch is nil")
	}
	defer w.Stop()

	w.tick()
	w.tick()
	if w.Total() != 1 {
		t.Fatalf("total = %d, want 1 (edge-triggered)", w.Total())
	}
	if len(fired) != 1 || len(fired[0]) != 1 {
		t.Fatalf("onAlert fired %d times, want 1", len(fired))
	}

	// Changing a leg's value re-arms the alert.
	mustSetRate(t, l, "JPY", "USD", 98, 100)
	w.tick()
	if w.Total() != 2 {
		t.Fatalf("after leg change: total = %d, want 2", w.Total())
	}

	// Removing a leg prunes the mark: re-adding the same values alerts
	// again.
	l.mu.Lock()
	delete(l.fxRates, fxPair{from: "JPY", to: "USD"})
	l.mu.Unlock()
	w.tick()
	if _, ok := w.seen[[3]string{"EUR", "JPY", "USD"}]; ok {
		t.Fatal("mark not pruned after leg removal")
	}
	mustSetRate(t, l, "JPY", "USD", 98, 100)
	w.tick()
	if w.Total() != 3 {
		t.Fatalf("after re-add: total = %d, want 3", w.Total())
	}
	if w.Ticks() != 5 {
		t.Errorf("ticks = %d, want 5", w.Ticks())
	}
}

func TestStartFXConsistencyWatchDisabled(t *testing.T) {
	l := New()
	if w := StartFXConsistencyWatch(context.Background(), l, 0, 50, nil); w != nil {
		t.Error("interval 0: want nil watch")
		w.Stop()
	}
	if w := StartFXConsistencyWatch(context.Background(), l, time.Minute, 0, nil); w != nil {
		t.Error("threshold 0: want nil watch (opt-in)")
		w.Stop()
	}
	if w := StartFXConsistencyWatch(context.Background(), l, -time.Minute, 50, nil); w != nil {
		t.Error("negative interval: want nil watch")
		w.Stop()
	}
}

// Drive-by (修真) — the FX path missed the "<id>/chunk-0" namespace
// check the plain transfer path has (transfer.go): a plain FX
// PostTransfer reusing a split transfer's ID would journal a second
// transfer under one identity.
func TestPlainFXTransferRejectsSplitTransferID(t *testing.T) {
	l := New(
		WithMaxTransferChunkCents(10000),
		WithFXAccount("fx-clearing"),
	)
	mustSetRate(t, l, "USD", "EUR", 9, 10)
	seedBalance(t, l, "alice", 100000)

	if _, err := l.PostTransferSplit(Transfer{
		ID: "t-dup", From: "alice", To: "bob", AmountCents: 25000,
		Currency: "USD", ToCurrency: "EUR",
	}); err != nil {
		t.Fatalf("PostTransferSplit FX: %v", err)
	}

	_, err := l.PostTransfer(Transfer{
		ID: "t-dup", From: "alice", To: "bob", AmountCents: 100,
		Currency: "USD", ToCurrency: "EUR",
	})
	if !errors.Is(err, ErrTransferIDConflict) {
		t.Fatalf("PostTransfer FX reusing split ID: err = %v, want ErrTransferIDConflict", err)
	}
}
