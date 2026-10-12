package ledger

import (
	"context"
	"math/big"
	"testing"
	"time"
)

func mustFXWatchLedger(t *testing.T) *Ledger {
	t.Helper()
	al := mustAuditLog(t)
	return New(WithAuditLog(al))
}

func mustSetRateTTL(t *testing.T, l *Ledger, from, to string, ttl time.Duration) {
	t.Helper()
	if err := l.SetFXRateRat(from, to, big.NewRat(11, 10), ttl); err != nil {
		t.Fatalf("SetFXRateRat: %v", err)
	}
}

// setRateExpiry reaches into the table to pin an exact expiry: the public
// API only sets expiries relative to now, but the watcher must also handle
// rates that crossed expiry while the process was down (restored from a
// snapshot) or whose clock moved.
func setRateExpiry(t *testing.T, l *Ledger, from, to string, expiresAt time.Time) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	p := fxPair{from: from, to: to}
	r, ok := l.fxRates[p]
	if !ok {
		t.Fatalf("no rate for %s->%s", from, to)
	}
	r.ExpiresAt = expiresAt
	l.fxRates[p] = r
}

func TestScanFXRateAlertsClassification(t *testing.T) {
	l := mustFXWatchLedger(t)
	now := time.Now()
	// No expiry: never alerted.
	if err := l.SetFXRateRat("USD", "EUR", big.NewRat(11, 10), 0); err != nil {
		t.Fatalf("SetFXRateRat: %v", err)
	}
	// Expires in 1h: stale inside a 2h window.
	mustSetRateTTL(t, l, "USD", "JPY", time.Hour)
	// Already expired.
	mustSetRateTTL(t, l, "EUR", "USD", time.Hour)
	setRateExpiry(t, l, "EUR", "USD", now.Add(-time.Minute))

	alerts := l.ScanFXRateAlerts(now, 2*time.Hour)
	if len(alerts) != 2 {
		t.Fatalf("got %d alerts, want 2: %+v", len(alerts), alerts)
	}
	// Sorted by (from, to).
	if alerts[0].FromCurrency != "EUR" || alerts[0].Kind != FXRateAlertExpired {
		t.Errorf("alerts[0] = %+v, want EUR->USD expired", alerts[0])
	}
	if alerts[1].FromCurrency != "USD" || alerts[1].ToCurrency != "JPY" || alerts[1].Kind != FXRateAlertStale {
		t.Errorf("alerts[1] = %+v, want USD->JPY stale", alerts[1])
	}
	if alerts[1].ExpiresAt.IsZero() || alerts[1].EffectiveVersion != 0 {
		t.Errorf("alert missing expiry metadata: %+v", alerts[1])
	}
}

func TestScanFXRateAlertsThresholdOptIn(t *testing.T) {
	l := mustFXWatchLedger(t)
	mustSetRateTTL(t, l, "USD", "JPY", time.Hour)
	// Capture now *after* installing the rate, so the rate expires
	// exactly 1h after this instant and the boundary is exact.
	now := time.Now()
	// Zero threshold: stale warnings disabled, and the rate has not
	// expired yet, so nothing fires.
	if alerts := l.ScanFXRateAlerts(now, 0); len(alerts) != 0 {
		t.Fatalf("got %d alerts with disabled threshold, want 0", len(alerts))
	}
	// Negative threshold: same, disabled.
	if alerts := l.ScanFXRateAlerts(now, -time.Hour); len(alerts) != 0 {
		t.Fatalf("got %d alerts with negative threshold, want 0", len(alerts))
	}
	// A rate expiring exactly at the window edge counts as stale
	// (now+threshold >= expiresAt, inclusive).
	if alerts := l.ScanFXRateAlerts(now, time.Hour); len(alerts) != 1 || alerts[0].Kind != FXRateAlertStale {
		t.Fatalf("boundary alerts = %+v, want 1 stale", alerts)
	}
}

func TestScanFXRateAlertsExpiredBoundary(t *testing.T) {
	l := mustFXWatchLedger(t)
	mustSetRateTTL(t, l, "USD", "JPY", time.Hour)
	// now == ExpiresAt is expired ("posted at or after ExpiresAt is
	// rejected", same boundary as the transfer gate).
	exp := l.fxRates[fxPair{from: "USD", to: "JPY"}].ExpiresAt
	if alerts := l.ScanFXRateAlerts(exp, 24*time.Hour); len(alerts) != 1 || alerts[0].Kind != FXRateAlertExpired {
		t.Fatalf("boundary alerts = %+v, want 1 expired", alerts)
	}
}

func TestFXRateWatchEdgeTriggered(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	now := time.Now()
	mustSetRateTTL(t, l, "USD", "JPY", 2*time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var seen [][]FXRateAlert
	w := StartFXRateWatch(ctx, l, time.Hour, 24*time.Hour, func(as []FXRateAlert) {
		seen = append(seen, as)
	})
	if w == nil {
		t.Fatal("StartFXRateWatch returned nil for positive interval")
	}
	defer w.Stop()

	// First tick: stale fires.
	w.tick()
	if w.StaleTotal() != 1 || w.ExpiredTotal() != 0 {
		t.Fatalf("after tick 1: stale=%d expired=%d, want 1/0", w.StaleTotal(), w.ExpiredTotal())
	}
	// Second tick: same identity, no re-fire.
	w.tick()
	if w.StaleTotal() != 1 || len(seen) != 1 {
		t.Fatalf("after tick 2: stale=%d callbacks=%d, want 1/1", w.StaleTotal(), len(seen))
	}
	// The rate crosses expiry: the expired transition fires even though
	// stale already fired for this identity.
	setRateExpiry(t, l, "USD", "JPY", now.Add(-time.Minute))
	w.tick()
	if w.ExpiredTotal() != 1 || len(seen) != 2 || seen[1][0].Kind != FXRateAlertExpired {
		t.Fatalf("after expiry tick: expired=%d callbacks=%d, want 1/2", w.ExpiredTotal(), len(seen))
	}
	// Fourth tick: expired already reported, silent.
	w.tick()
	if w.ExpiredTotal() != 1 || len(seen) != 2 {
		t.Fatalf("after tick 4: expired=%d callbacks=%d, want 1/2", w.ExpiredTotal(), len(seen))
	}
	if got := l.FXRateStaleAlertCount(); got != 1 {
		t.Errorf("ledger stale counter = %d, want 1", got)
	}
	if got := l.FXRateExpiredAlertCount(); got != 1 {
		t.Errorf("ledger expired counter = %d, want 1", got)
	}
}

func TestFXRateWatchRenewalRearms(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	mustSetRateTTL(t, l, "USD", "JPY", 2*time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := StartFXRateWatch(ctx, l, time.Hour, 24*time.Hour, nil)
	if w == nil {
		t.Fatal("StartFXRateWatch returned nil")
	}
	defer w.Stop()

	w.tick()
	if w.StaleTotal() != 1 {
		t.Fatalf("stale=%d, want 1", w.StaleTotal())
	}
	// Renewing the rate (new expiry, new identity) re-arms the warning.
	mustSetRateTTL(t, l, "USD", "JPY", 3*time.Hour)
	w.tick()
	if w.StaleTotal() != 2 {
		t.Fatalf("after renewal: stale=%d, want 2", w.StaleTotal())
	}
}

func TestFXRateWatchRemovalPrunes(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	mustSetRateTTL(t, l, "USD", "JPY", 2*time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := StartFXRateWatch(ctx, l, time.Hour, 24*time.Hour, nil)
	defer w.Stop()

	w.tick()
	if w.StaleTotal() != 1 {
		t.Fatalf("stale=%d, want 1", w.StaleTotal())
	}
	if _, err := l.RemoveFXRate("USD", "JPY"); err != nil {
		t.Fatalf("RemoveFXRate: %v", err)
	}
	// Re-add the identical rate. EffectiveVersion is unchanged (no
	// postings happened), so without pruning the stale mark the warning
	// would stay suppressed — the operator re-installed the rate and
	// must be warned again.
	mustSetRateTTL(t, l, "USD", "JPY", 2*time.Hour)
	w.tick()
	if w.StaleTotal() != 2 {
		t.Fatalf("after remove+re-add: stale=%d, want 2 (mark pruned)", w.StaleTotal())
	}
}

func TestFXRateWatchAuditEvents(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	now := time.Now()
	mustSetRateTTL(t, l, "USD", "JPY", 2*time.Hour)
	mustSetRateTTL(t, l, "EUR", "USD", time.Hour)
	setRateExpiry(t, l, "EUR", "USD", now.Add(-time.Minute))

	l.ReportFXRateAlerts(l.ScanFXRateAlerts(now, 24*time.Hour))

	// Close flushes the async audit queue before the file is read.
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
	var ops []string
	for _, e := range events {
		if e.Op == "fx_rate_stale" || e.Op == "fx_rate_expired" {
			ops = append(ops, e.Op)
			if e.Actor != "FXRateWatch" {
				t.Errorf("event %s actor = %q, want FXRateWatch", e.Op, e.Actor)
			}
			if e.Details["from_currency"] == nil || e.Details["expires_at"] == nil {
				t.Errorf("event %s missing details: %+v", e.Op, e.Details)
			}
		}
	}
	if len(ops) != 2 || ops[0] != "fx_rate_expired" || ops[1] != "fx_rate_stale" {
		t.Errorf("watch audit ops = %v, want [fx_rate_expired fx_rate_stale]", ops)
	}
}

func TestStartFXRateWatchDisabled(t *testing.T) {
	l := New()
	ctx := context.Background()
	if w := StartFXRateWatch(ctx, l, 0, time.Hour, nil); w != nil {
		t.Error("zero interval: want nil watcher")
		w.Stop()
	}
	if w := StartFXRateWatch(ctx, l, -time.Second, time.Hour, nil); w != nil {
		t.Error("negative interval: want nil watcher")
		w.Stop()
	}
}

func TestFXRateWatchTicksAndStop(t *testing.T) {
	l := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := StartFXRateWatch(ctx, l, 10*time.Millisecond, 0, nil)
	if w == nil {
		t.Fatal("StartFXRateWatch returned nil")
	}
	deadline := time.Now().Add(2 * time.Second)
	for w.Ticks() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if w.Ticks() < 2 {
		t.Fatalf("ticks=%d, want >=2", w.Ticks())
	}
	// Stop is idempotent and safe alongside context cancellation.
	w.Stop()
	w.Stop()
	cancel()
}

func TestFXRateWatchPureScanNoSideEffects(t *testing.T) {
	l := mustFXWatchLedger(t)
	now := time.Now()
	mustSetRateTTL(t, l, "USD", "JPY", 2*time.Hour)
	v0 := l.version
	l.ScanFXRateAlerts(now, 24*time.Hour)
	if l.version != v0 {
		t.Errorf("scan bumped version: %d -> %d", v0, l.version)
	}
	if got := l.FXRateStaleAlertCount(); got != 0 {
		t.Errorf("scan touched stale counter: %d", got)
	}
}
