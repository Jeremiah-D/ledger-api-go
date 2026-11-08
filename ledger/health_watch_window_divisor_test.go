package ledger

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// Balance-health-watch spot-check window divisor runtime revision
// tests (LG-356). They reuse mustAuditLog from ledger_audit_test.go
// and ReadAuditLog from the audit log reader.

// seedCorruptedKeys builds a ledger with n distinct (account,
// currency) keys whose balances are all corrupted by +1 cent, so
// every key the spot check covers reports an accounting_equation
// violation. The keys sort as k0..k(n-1), so a scan's window covers a
// predictable prefix of them from the rotation cursor.
func seedCorruptedKeys(t *testing.T, n int) *Ledger {
	t.Helper()
	l := New()
	l.mu.Lock()
	for i := 0; i < n; i++ {
		k := accountCurrency{account: AccountID(fmt.Sprintf("k%d", i)), currency: "USD"}
		l.balances[k] = 101
		l.debitTotals[k] = 100
		l.creditTotals[k] = 0
	}
	l.mu.Unlock()
	return l
}

// equationViolationAccounts returns the set of accounts named by
// accounting_equation violations in a scan result.
func equationViolationAccounts(vs []HealthViolation) map[AccountID]bool {
	out := make(map[AccountID]bool)
	for _, v := range vs {
		if v.Check == HealthCheckAccountingEquation {
			out[v.Account] = true
		}
	}
	return out
}

// retunedHealthWatchWindowDivisorEvents closes the audit log (which
// drains the async writer) and returns every
// health_watch_window_divisor_retuned event recorded in it. The
// ledger must not be used after this call.
func retunedHealthWatchWindowDivisorEvents(t *testing.T, al *AuditLog) []AuditEvent {
	t.Helper()
	if err := al.Close(); err != nil {
		t.Fatalf("Close audit log: %v", err)
	}
	events, corrupt, err := ReadAuditLog(al.Dir(), time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt events = %d, want 0", corrupt)
	}
	var out []AuditEvent
	for _, ev := range events {
		if ev.Op == "health_watch_window_divisor_retuned" {
			out = append(out, ev)
		}
	}
	return out
}

func TestSetHealthWatchWindowDivisorInvalidRejectedAndKeepsLive(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	defer func() { _ = al.Close() }()

	if got := l.HealthWatchWindowDivisor(); got != 4 {
		t.Fatalf("default divisor = %d, want 4", got)
	}
	for _, n := range []int{0, -1, -100} {
		prev, err := l.SetHealthWatchWindowDivisor(n)
		if !errors.Is(err, ErrInvalidHealthWatchWindowDivisor) {
			t.Fatalf("SetHealthWatchWindowDivisor(%d): err = %v, want ErrInvalidHealthWatchWindowDivisor", n, err)
		}
		if prev != 4 {
			t.Fatalf("SetHealthWatchWindowDivisor(%d) returned %d, want the live 4", n, prev)
		}
		if got := l.HealthWatchWindowDivisor(); got != 4 {
			t.Fatalf("divisor = %d after rejected %d, want still 4", got, n)
		}
	}
	// The Ctx variant rejects the same way.
	if prev, err := l.SetHealthWatchWindowDivisorCtx(context.Background(), 0); !errors.Is(err, ErrInvalidHealthWatchWindowDivisor) || prev != 4 {
		t.Fatalf("SetHealthWatchWindowDivisorCtx(0) = (%d, %v), want (4, ErrInvalidHealthWatchWindowDivisor)", prev, err)
	}
	// Zero is rejected here because it would divide by zero in the
	// window computation — a runtime revision never silently breaks
	// the scan.
	if events := retunedHealthWatchWindowDivisorEvents(t, al); len(events) != 0 {
		t.Fatalf("retuned events = %d, want 0 after only rejected revisions", len(events))
	}
}

func TestSetHealthWatchWindowDivisorSameValueNoOpNoAudit(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	defer func() { _ = al.Close() }()

	prev, err := l.SetHealthWatchWindowDivisor(4)
	if err != nil {
		t.Fatalf("same-value SetHealthWatchWindowDivisor: %v", err)
	}
	if prev != 4 {
		t.Fatalf("same-value revision returned %d, want the live 4", prev)
	}
	// One real revision, then the same value again: only the real
	// one audits.
	if _, err := l.SetHealthWatchWindowDivisor(2); err != nil {
		t.Fatalf("SetHealthWatchWindowDivisor(2): %v", err)
	}
	if prev, err := l.SetHealthWatchWindowDivisor(2); err != nil || prev != 2 {
		t.Fatalf("same-value SetHealthWatchWindowDivisor(2) = (%d, %v), want (2, nil)", prev, err)
	}
	if events := retunedHealthWatchWindowDivisorEvents(t, al); len(events) != 1 {
		t.Fatalf("retuned events = %d, want 1 after the real revision plus the no-ops", len(events))
	}
}

func TestSetHealthWatchWindowDivisorReturnsPreviousValue(t *testing.T) {
	l := New()
	prev, err := l.SetHealthWatchWindowDivisor(2)
	if err != nil {
		t.Fatalf("SetHealthWatchWindowDivisor: %v", err)
	}
	if prev != 4 {
		t.Fatalf("returned previous divisor = %d, want 4", prev)
	}
	if got := l.HealthWatchWindowDivisor(); got != 2 {
		t.Fatalf("divisor = %d after the revision, want 2", got)
	}
}

func TestSetHealthWatchWindowDivisorOneCoversAllInSingleScan(t *testing.T) {
	l := seedCorruptedKeys(t, 8)
	if _, err := l.SetHealthWatchWindowDivisor(1); err != nil {
		t.Fatalf("SetHealthWatchWindowDivisor(1): %v", err)
	}
	vs := l.ScanBalanceHealth()
	if got := len(equationViolationAccounts(vs)); got != 8 {
		t.Fatalf("equation violations in one scan = %d, want all 8 keys with divisor 1", got)
	}
}

func TestSetHealthWatchWindowDivisorExactPerScanCoverage(t *testing.T) {
	// 8 keys, divisor 2: window ceil(8/2) = 4 — exactly four keys
	// per scan, starting at the cursor.
	l := seedCorruptedKeys(t, 8)
	if _, err := l.SetHealthWatchWindowDivisor(2); err != nil {
		t.Fatalf("SetHealthWatchWindowDivisor(2): %v", err)
	}
	vs := l.ScanBalanceHealth()
	got := equationViolationAccounts(vs)
	if len(got) != 4 {
		t.Fatalf("equation violations in one scan = %d, want exactly 4 with divisor 2", len(got))
	}
	for _, a := range []AccountID{"k0", "k1", "k2", "k3"} {
		if !got[a] {
			t.Fatalf("first scan missed %s, want the window to cover k0..k3", a)
		}
	}

	// 8 keys, divisor 8: window ceil(8/8) = 1 — exactly one key per
	// scan.
	l2 := seedCorruptedKeys(t, 8)
	if _, err := l2.SetHealthWatchWindowDivisor(8); err != nil {
		t.Fatalf("SetHealthWatchWindowDivisor(8): %v", err)
	}
	vs2 := l2.ScanBalanceHealth()
	got2 := equationViolationAccounts(vs2)
	if len(got2) != 1 || !got2["k0"] {
		t.Fatalf("equation violations in one scan = %v, want exactly {k0} with divisor 8", got2)
	}
}

func TestSetHealthWatchWindowDivisorDoesNotResetCursor(t *testing.T) {
	l := seedCorruptedKeys(t, 8)

	// One scan at the default divisor 4 covers k0, k1 and advances
	// the cursor by 2.
	vs := l.ScanBalanceHealth()
	if got := equationViolationAccounts(vs); len(got) != 2 || !got["k0"] || !got["k1"] {
		t.Fatalf("first scan covered %v, want exactly {k0, k1}", got)
	}
	cursorBefore := l.healthCursor.Load()
	versionBefore := l.version

	if _, err := l.SetHealthWatchWindowDivisor(2); err != nil {
		t.Fatalf("SetHealthWatchWindowDivisor(2): %v", err)
	}
	// The revision itself moves nothing but the divisor.
	if got := l.healthCursor.Load(); got != cursorBefore {
		t.Fatalf("rotation cursor moved %d -> %d, want unchanged (revision must not reset the window)", cursorBefore, got)
	}
	if got := l.version; got != versionBefore {
		t.Fatalf("version moved %d -> %d, want unchanged", versionBefore, got)
	}

	// The next scan continues from the cursor on the new window:
	// keys k2..k5, not a restart from k0.
	vs2 := l.ScanBalanceHealth()
	got2 := equationViolationAccounts(vs2)
	if len(got2) != 4 {
		t.Fatalf("second scan covered %d keys, want exactly 4 on the new divisor 2", len(got2))
	}
	for _, a := range []AccountID{"k2", "k3", "k4", "k5"} {
		if !got2[a] {
			t.Fatalf("second scan missed %s, want the window to continue from the cursor (k2..k5)", a)
		}
	}
}

func TestSetHealthWatchWindowDivisorConcurrentRevisionAndScanRaceFree(t *testing.T) {
	l := seedCorruptedKeys(t, 8)

	var wg sync.WaitGroup
	// Concurrent revisions from several goroutines while scans run
	// underneath: -race must stay silent, and the divisor must stay
	// a valid live value throughout.
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				n := 1 + (g+i)%8
				_, _ = l.SetHealthWatchWindowDivisor(n)
				_ = l.HealthWatchWindowDivisor()
			}
		}(g)
	}
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				_ = l.ScanBalanceHealth()
			}
		}()
	}
	wg.Wait()
	if got := l.HealthWatchWindowDivisor(); got < 1 || got > 8 {
		t.Fatalf("divisor = %d after concurrent revisions, want a live value in [1, 8]", got)
	}
}

func TestSetHealthWatchWindowDivisorCtxActorAttribution(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	defer func() { _ = al.Close() }()

	ctx := ContextWithActor(context.Background(), "ops-alan")
	if _, err := l.SetHealthWatchWindowDivisorCtx(ctx, 2); err != nil {
		t.Fatalf("SetHealthWatchWindowDivisorCtx: %v", err)
	}
	// A plain call keeps the entrypoint name as the actor.
	if _, err := l.SetHealthWatchWindowDivisor(8); err != nil {
		t.Fatalf("SetHealthWatchWindowDivisor: %v", err)
	}
	events := retunedHealthWatchWindowDivisorEvents(t, al)
	if len(events) != 2 {
		t.Fatalf("retuned events = %d, want 2", len(events))
	}
	if events[0].Actor != "ops-alan" {
		t.Fatalf("first event Actor = %q, want ops-alan", events[0].Actor)
	}
	if got := events[0].Details["previous_window_divisor"]; got != float64(4) {
		t.Fatalf("first previous_window_divisor = %v, want 4", got)
	}
	if got := events[0].Details["current_window_divisor"]; got != float64(2) {
		t.Fatalf("first current_window_divisor = %v, want 2", got)
	}
	if events[1].Actor != "SetHealthWatchWindowDivisor" {
		t.Fatalf("second event Actor = %q, want SetHealthWatchWindowDivisor", events[1].Actor)
	}
	if got := events[1].Details["previous_window_divisor"]; got != float64(2) {
		t.Fatalf("second previous_window_divisor = %v, want 2", got)
	}
	if got := events[1].Details["current_window_divisor"]; got != float64(8) {
		t.Fatalf("second current_window_divisor = %v, want 8", got)
	}
}

func TestSetHealthWatchWindowDivisorAuditPreviousCurrentExact(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	defer func() { _ = al.Close() }()

	prev, err := l.SetHealthWatchWindowDivisor(2)
	if err != nil {
		t.Fatalf("SetHealthWatchWindowDivisor: %v", err)
	}
	if prev != 4 {
		t.Fatalf("returned previous divisor = %d, want 4", prev)
	}
	events := retunedHealthWatchWindowDivisorEvents(t, al)
	if len(events) != 1 {
		t.Fatalf("retuned events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Actor != "SetHealthWatchWindowDivisor" {
		t.Errorf("Actor = %q, want SetHealthWatchWindowDivisor", ev.Actor)
	}
	if ev.VersionBefore != ev.VersionAfter {
		t.Errorf("version moved %d -> %d, want no bump", ev.VersionBefore, ev.VersionAfter)
	}
	if got := ev.Details["previous_window_divisor"]; got != float64(4) {
		t.Errorf("previous_window_divisor = %v, want 4", got)
	}
	if got := ev.Details["current_window_divisor"]; got != float64(2) {
		t.Errorf("current_window_divisor = %v, want 2", got)
	}
	// Only the two integer fields ride in the event: no other keys.
	if len(ev.Details) != 2 {
		t.Errorf("details = %v, want exactly previous/current window divisor", ev.Details)
	}
}

func TestSetHealthWatchWindowDivisorGetterFollowsLiveValue(t *testing.T) {
	l := New()
	if got := l.HealthWatchWindowDivisor(); got != 4 {
		t.Fatalf("divisor = %d at start, want 4", got)
	}
	if prev, err := l.SetHealthWatchWindowDivisor(2); err != nil || prev != 4 {
		t.Fatalf("SetHealthWatchWindowDivisor(2) = (%d, %v), want (4, nil)", prev, err)
	}
	if got := l.HealthWatchWindowDivisor(); got != 2 {
		t.Fatalf("divisor = %d after tightening, want 2", got)
	}
	// A rejected revision leaves the getter in place.
	if _, err := l.SetHealthWatchWindowDivisor(0); !errors.Is(err, ErrInvalidHealthWatchWindowDivisor) {
		t.Fatalf("SetHealthWatchWindowDivisor(0): err = %v, want ErrInvalidHealthWatchWindowDivisor", err)
	}
	if got := l.HealthWatchWindowDivisor(); got != 2 {
		t.Fatalf("divisor = %d after rejected revision, want still 2", got)
	}
}

func TestSetHealthWatchWindowDivisorSnapshotRestoresDefault(t *testing.T) {
	l := New()
	if _, err := l.SetHealthWatchWindowDivisor(2); err != nil {
		t.Fatalf("SetHealthWatchWindowDivisor(2): %v", err)
	}
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	// Snapshots do not carry the divisor: a restore starts again at
	// the default 4, while the revision event stays in the audit
	// trail for traceability.
	if got := restored.HealthWatchWindowDivisor(); got != 4 {
		t.Fatalf("restored divisor = %d, want the default 4", got)
	}
}
