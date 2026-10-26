package ledger

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func TestPartialFreezeExpirySetValidation(t *testing.T) {
	l := New()
	seedBalance(t, l, "alice", 100000)

	// No freeze to put a deadline on: rejected, nothing created.
	if err := l.SetPartialFreezeExpiry("alice", time.Now().Add(time.Hour)); !errors.Is(err, ErrPartialFreezeNoFreeze) {
		t.Errorf("set without freeze: err = %v, want ErrPartialFreezeNoFreeze", err)
	}
	if got := l.PartialFrozenAmount("alice"); got != 0 {
		t.Errorf("frozen after rejected set = %d, want 0", got)
	}
	if _, ok := l.PartialFreezeExpiry("alice"); ok {
		t.Error("expiry set after rejected set")
	}

	if err := l.FreezeAmount("alice", 30000); err != nil {
		t.Fatalf("FreezeAmount: %v", err)
	}
	// A past deadline is rejected and the freeze (and any prior
	// expiry) is untouched.
	if err := l.SetPartialFreezeExpiry("alice", time.Now().Add(-time.Hour)); !errors.Is(err, ErrPartialFreezeExpiryNotFuture) {
		t.Errorf("past expiry: err = %v, want ErrPartialFreezeExpiryNotFuture", err)
	}
	if _, ok := l.PartialFreezeExpiry("alice"); ok {
		t.Error("expiry set after past-deadline rejection")
	}
	if err := l.SetPartialFreezeExpiry("", time.Now().Add(time.Hour)); !errors.Is(err, ErrPartialFreezeEmptyAccount) {
		t.Errorf("empty account: err = %v, want ErrPartialFreezeEmptyAccount", err)
	}

	// A valid set changes nothing but the deadline: no money
	// moves, the version does not move, Available is unchanged.
	versionBefore := l.Version()
	expires := time.Now().Add(time.Hour).Truncate(time.Second)
	if err := l.SetPartialFreezeExpiry("alice", expires); err != nil {
		t.Fatalf("SetPartialFreezeExpiry: %v", err)
	}
	if got, ok := l.PartialFreezeExpiry("alice"); !ok || !got.Equal(expires) {
		t.Errorf("expiry = %v, %v, want %v, true", got, ok, expires)
	}
	if got := l.PartialFrozenAmount("alice"); got != 30000 {
		t.Errorf("frozen after set = %d, want 30000", got)
	}
	if got := l.Available("alice"); got != 70000 {
		t.Errorf("available after set = %d, want 70000", got)
	}
	if got := l.Version(); got != versionBefore {
		t.Errorf("version = %d, want %d (expiry bumps nothing)", got, versionBefore)
	}
}

func TestPartialFreezeExpiryBlocksNewFreezesPerCurrency(t *testing.T) {
	l := New()
	seedBalance(t, l, "alice", 100000)
	seedBalanceIn(t, l, "alice", 50000, "EUR")
	if err := l.FreezeAmount("alice", 30000); err != nil {
		t.Fatalf("FreezeAmount: %v", err)
	}
	if err := l.FreezeAmountIn("alice", "EUR", 10000); err != nil {
		t.Fatalf("FreezeAmountIn EUR: %v", err)
	}
	if err := l.SetPartialFreezeExpiry("alice", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("SetPartialFreezeExpiry: %v", err)
	}
	// The same (account, currency) refuses new freezes while the
	// expiry is pending — the new money must never be swept up
	// by the pending release.
	if err := l.FreezeAmount("alice", 1000); !errors.Is(err, ErrPartialFreezeExpirySet) {
		t.Errorf("freeze under expiry: err = %v, want ErrPartialFreezeExpirySet", err)
	}
	if got := l.PartialFrozenAmount("alice"); got != 30000 {
		t.Errorf("frozen after blocked freeze = %d, want 30000", got)
	}
	// Another currency is unaffected: the block is per
	// (account, currency), not per account.
	if err := l.FreezeAmountIn("alice", "EUR", 5000); err != nil {
		t.Fatalf("FreezeAmountIn EUR under USD expiry: %v", err)
	}
}

func TestPartialFreezeExpirySweepReleasesOnlyDue(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	defer al.Close()
	l := New(WithAuditLog(al))
	seedBalance(t, l, "alice", 100000)
	seedBalance(t, l, "bob", 100000)
	if err := l.FreezeAmount("alice", 30000); err != nil {
		t.Fatalf("FreezeAmount alice: %v", err)
	}
	if err := l.FreezeAmount("bob", 20000); err != nil {
		t.Fatalf("FreezeAmount bob: %v", err)
	}
	now := time.Now()
	if err := l.SetPartialFreezeExpiry("alice", now.Add(time.Hour)); err != nil {
		t.Fatalf("SetPartialFreezeExpiry alice: %v", err)
	}
	if err := l.SetPartialFreezeExpiry("bob", now.Add(2 * time.Hour)); err != nil {
		t.Fatalf("SetPartialFreezeExpiry bob: %v", err)
	}

	// Nothing due yet: a sweep releases nothing and audits nothing.
	if got := l.ExpirePartialFreezes(now.Add(30 * time.Minute)); got != 0 {
		t.Errorf("early sweep released = %d, want 0", got)
	}
	if got := l.PartialFrozenAmount("alice"); got != 30000 {
		t.Errorf("alice frozen after early sweep = %d, want 30000", got)
	}

	// At the deadline the freeze releases: Available restores in
	// full, the expiry is gone, and the version never moved.
	versionBefore := l.Version()
	if got := l.ExpirePartialFreezes(now.Add(time.Hour)); got != 1 {
		t.Errorf("due sweep released = %d, want 1 (alice only)", got)
	}
	if got := l.PartialFrozenAmount("alice"); got != 0 {
		t.Errorf("alice frozen after sweep = %d, want 0", got)
	}
	if got := l.Available("alice"); got != 100000 {
		t.Errorf("alice available after sweep = %d, want 100000", got)
	}
	if _, ok := l.PartialFreezeExpiry("alice"); ok {
		t.Error("alice expiry survives its own release")
	}
	if got := l.Version(); got != versionBefore {
		t.Errorf("version = %d, want %d (expiry release bumps nothing)", got, versionBefore)
	}
	// Bob is untouched: still frozen, still expiring later.
	if got := l.PartialFrozenAmount("bob"); got != 20000 {
		t.Errorf("bob frozen after alice sweep = %d, want 20000", got)
	}
	if _, ok := l.PartialFreezeExpiry("bob"); !ok {
		t.Error("bob expiry lost by alice sweep")
	}
	// The sweep is idempotent: a second sweep releases nothing.
	if got := l.ExpirePartialFreezes(now.Add(3 * time.Hour)); got != 1 {
		t.Errorf("second sweep released = %d, want 1 (bob only)", got)
	}

	byOp := auditEventsByOp(t, dir)
	if got := byOp["partial_freeze_expired"]; len(got) != 2 {
		t.Fatalf("partial_freeze_expired events = %d, want 2", len(got))
	}
	// Audit details round-trip through JSON, so the amount reads
	// back as float64, not int64.
	if got := byOp["partial_freeze_expired"][0].Details["released_cents"]; got != float64(30000) {
		t.Errorf("alice expired released_cents = %v, want 30000", got)
	}
	// An automatic release is never recorded as a manual one.
	if got := byOp["partial_freeze_released"]; len(got) != 0 {
		t.Errorf("partial_freeze_released events = %d, want 0 (sweep releases are expired, not released)", len(got))
	}
}

func TestPartialFreezeExpiryStaleDeadlineStillBlocksUntilSweep(t *testing.T) {
	l := New()
	seedBalance(t, l, "alice", 100000)
	if err := l.FreezeAmount("alice", 30000); err != nil {
		t.Fatalf("FreezeAmount: %v", err)
	}
	if err := l.SetPartialFreezeExpiry("alice", time.Now().Add(200*time.Millisecond)); err != nil {
		t.Fatalf("SetPartialFreezeExpiry: %v", err)
	}
	// Wait past the deadline without sweeping: the stale expiry
	// still blocks new freezes — fail-closed, no funds slip in
	// ahead of the release.
	time.Sleep(300 * time.Millisecond)
	if err := l.FreezeAmount("alice", 1000); !errors.Is(err, ErrPartialFreezeExpirySet) {
		t.Errorf("freeze under stale expiry: err = %v, want ErrPartialFreezeExpirySet", err)
	}
	if got := l.ExpirePartialFreezes(time.Now()); got != 1 {
		t.Errorf("stale sweep released = %d, want 1", got)
	}
	// After the release, freezing is admitted again.
	if err := l.FreezeAmount("alice", 1000); err != nil {
		t.Fatalf("FreezeAmount after release: %v", err)
	}
}

func TestPartialFreezeExpiryPartialReleaseKeepsFullReleaseClears(t *testing.T) {
	l := New()
	seedBalance(t, l, "alice", 100000)
	if err := l.FreezeAmount("alice", 30000); err != nil {
		t.Fatalf("FreezeAmount: %v", err)
	}
	if err := l.SetPartialFreezeExpiry("alice", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("SetPartialFreezeExpiry: %v", err)
	}
	// A partial release leaves the remaining amount expiring at
	// the same instant.
	if err := l.UnfreezeAmount("alice", 10000); err != nil {
		t.Fatalf("partial UnfreezeAmount: %v", err)
	}
	if _, ok := l.PartialFreezeExpiry("alice"); !ok {
		t.Error("expiry lost by partial release")
	}
	if got := l.PartialFrozenAmount("alice"); got != 20000 {
		t.Errorf("frozen after partial release = %d, want 20000", got)
	}
	// A full release clears the expiry with the freeze, so a
	// later freeze is admitted again.
	if err := l.UnfreezeAmount("alice", 20000); err != nil {
		t.Fatalf("full UnfreezeAmount: %v", err)
	}
	if _, ok := l.PartialFreezeExpiry("alice"); ok {
		t.Error("expiry survives full release")
	}
	if err := l.FreezeAmount("alice", 5000); err != nil {
		t.Fatalf("FreezeAmount after full release: %v", err)
	}
}

func TestPartialFreezeExpiryClearRestoresFreezing(t *testing.T) {
	l := New()
	seedBalance(t, l, "alice", 100000)
	if err := l.FreezeAmount("alice", 30000); err != nil {
		t.Fatalf("FreezeAmount: %v", err)
	}
	// Clearing with no expiry is a no-op, not an error.
	if err := l.ClearPartialFreezeExpiry("alice"); err != nil {
		t.Errorf("clear with no expiry: %v, want nil", err)
	}
	if err := l.SetPartialFreezeExpiry("alice", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("SetPartialFreezeExpiry: %v", err)
	}
	if err := l.ClearPartialFreezeExpiry("alice"); err != nil {
		t.Fatalf("ClearPartialFreezeExpiry: %v", err)
	}
	if _, ok := l.PartialFreezeExpiry("alice"); ok {
		t.Error("expiry survives clear")
	}
	// The frozen amount itself is untouched by the clear, and
	// freezing is admitted again.
	if got := l.PartialFrozenAmount("alice"); got != 30000 {
		t.Errorf("frozen after clear = %d, want 30000", got)
	}
	if err := l.FreezeAmount("alice", 1000); err != nil {
		t.Fatalf("FreezeAmount after clear: %v", err)
	}
}

func TestPartialFreezeExpiryWholeAccountUnfreezeClears(t *testing.T) {
	l := New()
	seedBalance(t, l, "alice", 100000)
	if err := l.FreezeAmount("alice", 30000); err != nil {
		t.Fatalf("FreezeAmount: %v", err)
	}
	if err := l.SetPartialFreezeExpiry("alice", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("SetPartialFreezeExpiry: %v", err)
	}
	l.Freeze("alice")
	l.Unfreeze("alice")
	if got := l.PartialFrozenAmount("alice"); got != 0 {
		t.Errorf("frozen after Unfreeze = %d, want 0", got)
	}
	if _, ok := l.PartialFreezeExpiry("alice"); ok {
		t.Error("expiry survives whole-account Unfreeze")
	}
}

func TestPartialFreezeExpirySnapshotRoundTrip(t *testing.T) {
	src := New()
	seedBalance(t, src, "alice", 100000)
	if err := src.FreezeAmount("alice", 30000); err != nil {
		t.Fatalf("FreezeAmount: %v", err)
	}
	expires := time.Now().Add(time.Hour).Truncate(time.Second)
	if err := src.SetPartialFreezeExpiry("alice", expires); err != nil {
		t.Fatalf("SetPartialFreezeExpiry: %v", err)
	}
	var buf bytes.Buffer
	if err := src.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if got := restored.PartialFrozenAmount("alice"); got != 30000 {
		t.Errorf("restored frozen = %d, want 30000", got)
	}
	if got, ok := restored.PartialFreezeExpiry("alice"); !ok || !got.Equal(expires) {
		t.Errorf("restored expiry = %v, %v, want %v, true", got, ok, expires)
	}
	// The restored deadline still blocks new freezes, and the
	// reconcile report discloses it with the frozen amount.
	if err := restored.FreezeAmount("alice", 1000); !errors.Is(err, ErrPartialFreezeExpirySet) {
		t.Errorf("freeze under restored expiry: err = %v, want ErrPartialFreezeExpirySet", err)
	}
	report := restored.Reconcile(time.Now())
	found := false
	for _, row := range report.PartialFrozen {
		if row.Account == "alice" && row.AmountCents == 30000 {
			found = true
			if row.ExpiresAt == nil || !row.ExpiresAt.Equal(expires) {
				t.Errorf("reconcile row expiry = %v, want %v", row.ExpiresAt, expires)
			}
		}
	}
	if !found {
		t.Errorf("reconcile PartialFrozen missing alice row: %+v", report.PartialFrozen)
	}
	// And the restored sweep still releases at the deadline.
	if got := restored.ExpirePartialFreezes(expires); got != 1 {
		t.Errorf("restored sweep released = %d, want 1", got)
	}
}
