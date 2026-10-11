package ledger

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// mustFreezeUntil freezes the account with a deadline, failing the test
// on error.
func mustFreezeUntil(t *testing.T, l *Ledger, a AccountID, until time.Time) {
	t.Helper()
	if err := l.FreezeUntil(a, until); err != nil {
		t.Fatalf("FreezeUntil(%q): %v", a, err)
	}
}

func TestFreezeUntilSetsDeadline(t *testing.T) {
	l := New()
	until := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	mustFreezeUntil(t, l, "cash", until)

	if !l.IsFrozen("cash") {
		t.Fatal("IsFrozen(cash) = false after FreezeUntil")
	}
	got, ok := l.FreezeAutoUnfreezeAt("cash")
	if !ok {
		t.Fatal("FreezeAutoUnfreezeAt(cash) = false, want a deadline")
	}
	if !got.Equal(until) {
		t.Fatalf("deadline = %v, want %v", got, until)
	}
	// A timed freeze rejects like a manual one before its deadline.
	if _, _, err := l.Post(validEntry()); !errors.Is(err, ErrAccountFrozen) {
		t.Fatalf("Post through timed-frozen account: got %v, want ErrAccountFrozen", err)
	}
}

func TestFreezeUntilRejectsPastDeadline(t *testing.T) {
	l := New()
	if err := l.FreezeUntil("cash", time.Now().Add(-time.Second)); !errors.Is(err, ErrFreezeUntilNotFuture) {
		t.Fatalf("FreezeUntil(past): got %v, want ErrFreezeUntilNotFuture", err)
	}
	if err := l.FreezeUntil("cash", time.Now()); !errors.Is(err, ErrFreezeUntilNotFuture) {
		t.Fatalf("FreezeUntil(now): got %v, want ErrFreezeUntilNotFuture", err)
	}
	if l.IsFrozen("cash") {
		t.Fatal("IsFrozen(cash) = true after rejected FreezeUntil")
	}
}

func TestFreezeUntilZeroIsManual(t *testing.T) {
	l := New()
	mustFreezeUntil(t, l, "cash", time.Time{})
	if !l.IsFrozen("cash") {
		t.Fatal("IsFrozen(cash) = false after zero-Until freeze")
	}
	if _, ok := l.FreezeAutoUnfreezeAt("cash"); ok {
		t.Fatal("FreezeAutoUnfreezeAt(cash) = true for a manual freeze")
	}
	if n := l.ExpireFrozenAccounts(); n != 0 {
		t.Fatalf("ExpireFrozenAccounts = %d on a manual freeze, want 0", n)
	}
	if _, _, err := l.Post(validEntry()); !errors.Is(err, ErrAccountFrozen) {
		t.Fatalf("Post through manual freeze: got %v, want ErrAccountFrozen", err)
	}
}

func TestManualFreezeClearsDeadline(t *testing.T) {
	l := New()
	mustFreezeUntil(t, l, "cash", time.Now().Add(time.Hour))
	l.Freeze("cash") // manual re-freeze converts the timed freeze to manual
	if !l.IsFrozen("cash") {
		t.Fatal("IsFrozen(cash) = false after manual Freeze")
	}
	if _, ok := l.FreezeAutoUnfreezeAt("cash"); ok {
		t.Fatal("deadline survived a manual Freeze: a manual freeze must never auto-release")
	}
	if n := l.ExpireFrozenAccounts(); n != 0 {
		t.Fatalf("ExpireFrozenAccounts = %d after manual re-freeze, want 0", n)
	}
}

func TestUnfreezeClearsDeadline(t *testing.T) {
	l := New()
	mustFreezeUntil(t, l, "cash", time.Now().Add(time.Hour))
	l.Unfreeze("cash")
	if l.IsFrozen("cash") {
		t.Fatal("IsFrozen(cash) = true after Unfreeze")
	}
	if _, ok := l.FreezeAutoUnfreezeAt("cash"); ok {
		t.Fatal("deadline survived Unfreeze")
	}
}

func TestLazyUnfreezeOnPost(t *testing.T) {
	al := mustAuditLog(t)
	defer al.Close()
	l := New(WithAuditLog(al))

	mustFreezeUntil(t, l, "cash", time.Now().Add(50*time.Millisecond))
	time.Sleep(200 * time.Millisecond)

	// The write path releases the expired freeze before the frozen gate,
	// so the post succeeds.
	if _, _, err := l.Post(validEntry()); err != nil {
		t.Fatalf("Post after deadline passed: %v, want success (lazy unfreeze)", err)
	}
	if l.IsFrozen("cash") {
		t.Fatal("IsFrozen(cash) = true after the deadline passed and a write ran")
	}
	if got := l.FrozenAutoReleases(); got != 1 {
		t.Fatalf("FrozenAutoReleases = %d, want 1", got)
	}

	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, corrupt, err := ReadAuditLog(al.Dir(), time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt = %d, want 0", corrupt)
	}
	var releases []AuditEvent
	for _, ev := range events {
		if ev.Op == "frozen_auto_released" {
			releases = append(releases, ev)
		}
	}
	if len(releases) != 1 {
		t.Fatalf("frozen_auto_released events = %d, want 1", len(releases))
	}
	rel := releases[0]
	if rel.TraceID != "cash" {
		t.Errorf("release trace = %q, want cash", rel.TraceID)
	}
	if rel.VersionBefore != rel.VersionAfter {
		t.Errorf("release version bracket = %d->%d, want flat (unfreezing moves no money)",
			rel.VersionBefore, rel.VersionAfter)
	}
	if rel.Details["trigger"] != "write_path" {
		t.Errorf("release details.trigger = %v, want write_path", rel.Details["trigger"])
	}
}

func TestExpireFrozenAccounts(t *testing.T) {
	l := New()
	mustFreezeUntil(t, l, "a", time.Now().Add(50*time.Millisecond))
	mustFreezeUntil(t, l, "b", time.Now().Add(time.Hour))
	l.Freeze("c") // manual: never expires

	time.Sleep(200 * time.Millisecond)
	if n := l.ExpireFrozenAccounts(); n != 1 {
		t.Fatalf("ExpireFrozenAccounts = %d, want 1 (only the expired timed freeze)", n)
	}
	if l.IsFrozen("a") {
		t.Error("IsFrozen(a) = true after ExpireFrozenAccounts")
	}
	if !l.IsFrozen("b") || !l.IsFrozen("c") {
		t.Error("ExpireFrozenAccounts released a freeze whose deadline had not passed")
	}
	if got := l.FrozenAutoReleases(); got != 1 {
		t.Fatalf("FrozenAutoReleases = %d, want 1", got)
	}
	// Second call releases nothing: the deadline row is gone.
	if n := l.ExpireFrozenAccounts(); n != 0 {
		t.Fatalf("second ExpireFrozenAccounts = %d, want 0", n)
	}
}

func TestFreezeSweeperReleasesExpired(t *testing.T) {
	l := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var cbMu sync.Mutex
	var callbacks []int
	sw := StartFreezeSweeper(ctx, l, 20*time.Millisecond, func(released int) {
		cbMu.Lock()
		callbacks = append(callbacks, released)
		cbMu.Unlock()
	})
	if sw == nil {
		t.Fatal("StartFreezeSweeper returned nil for a positive interval")
	}
	defer sw.Stop()

	mustFreezeUntil(t, l, "cash", time.Now().Add(60*time.Millisecond))

	deadline := time.Now().Add(5 * time.Second)
	for l.FrozenAutoReleases() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if l.IsFrozen("cash") {
		t.Fatal("sweeper did not release the expired timed freeze")
	}
	if got := sw.ReleasedTotal(); got != 1 {
		t.Fatalf("sweeper ReleasedTotal = %d, want 1", got)
	}
	if got := l.FrozenAutoReleases(); got != 1 {
		t.Fatalf("FrozenAutoReleases = %d, want 1", got)
	}
	if sw.Ticks() == 0 {
		t.Fatal("sweeper Ticks = 0, want at least one completed tick")
	}
	foundRelease := false
	cbMu.Lock()
	for _, c := range callbacks {
		if c == 1 {
			foundRelease = true
		}
	}
	cbMu.Unlock()
	if !foundRelease {
		t.Fatal("onSweep never reported a tick with 1 release")
	}
	// Stop is idempotent.
	sw.Stop()
	sw.Stop()
}

func TestStartFreezeSweeperDisabled(t *testing.T) {
	l := New()
	ctx := context.Background()
	if sw := StartFreezeSweeper(ctx, l, 0, nil); sw != nil {
		t.Fatal("StartFreezeSweeper(0) != nil, want disabled")
	}
	if sw := StartFreezeSweeper(ctx, l, -time.Second, nil); sw != nil {
		t.Fatal("StartFreezeSweeper(-1s) != nil, want disabled")
	}
}

func TestSnapshotRoundTripCarriesFreezeDeadline(t *testing.T) {
	l := New()
	until := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	mustFreezeUntil(t, l, "cash", until)
	l.Freeze("ops") // manual freeze alongside, for contrast

	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(&buf)
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	got, ok := restored.FreezeAutoUnfreezeAt("cash")
	if !ok {
		t.Fatal("restored ledger lost the freeze deadline")
	}
	if !got.Equal(until) {
		t.Fatalf("restored deadline = %v, want %v", got, until)
	}
	if !restored.IsFrozen("cash") {
		t.Fatal("restored ledger lost the freeze itself")
	}
	if _, ok := restored.FreezeAutoUnfreezeAt("ops"); ok {
		t.Fatal("restored manual freeze gained a deadline")
	}
	if !restored.IsFrozen("ops") {
		t.Fatal("restored ledger lost the manual freeze")
	}
}

func TestReconcileAnnotatesAutoUnfreeze(t *testing.T) {
	l := New()
	until := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	mustFreezeUntil(t, l, "cash", until)
	l.Freeze("ops")

	report := l.Reconcile(time.Now())
	if len(report.FrozenAccounts) != 2 {
		t.Fatalf("frozen_accounts = %v, want 2 accounts", report.FrozenAccounts)
	}
	got, ok := report.FrozenAccountAutoUnfreeze["cash"]
	if !ok {
		t.Fatal("reconcile missing auto_unfreeze_at for the timed freeze")
	}
	if !got.Equal(until) {
		t.Fatalf("reconcile auto_unfreeze_at = %v, want %v", got, until)
	}
	if _, ok := report.FrozenAccountAutoUnfreeze["ops"]; ok {
		t.Fatal("reconcile annotated a manual freeze with auto_unfreeze_at")
	}
}
