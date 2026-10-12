package ledger

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// FX rate expiry watch (LG-68).
//
// Rates can carry an expiry (see ExchangeRate.ExpiresAt and
// SetFXRateRat): a cross-currency transfer posted at or after the expiry
// is rejected with ErrFXRateExpired instead of converting at a stale
// rate. The write path enforces that gate, but nothing told the operator
// a rate was *about* to expire — the first signal was a rejected
// transfer. This watcher is the in-process early-warning: a background
// goroutine that periodically scans the rate table, read only, and
// reports two edge-triggered conditions as structured audit events:
//
//   - "stale":   the rate expires within the opt-in warning threshold
//     (fx_rate_stale, plus ledger_fx_rate_stale_total);
//   - "expired": now is at or past the expiry (fx_rate_expired, plus
//     ledger_fx_rate_expired_total).
//
// Edge-triggered, not per-tick: a rate that stays stale across ticks is
// reported once, when it *enters* the warning window, and once more when
// it *crosses* expiry. The dedup identity is (ExpiresAt,
// EffectiveVersion) per pair, so renewing a rate (new expiry) re-arms
// both warnings, and the stale→expired transition still fires for a rate
// that was already warned stale. A removed pair's mark is dropped on the
// next tick. Like the health watchdog, the scan itself writes nothing to
// the books — no journal rows, no version bumps; the only mutations are
// the audit events and the alert counters.

// FXRateAlertKind names the condition an FX rate watch alert reports.
type FXRateAlertKind string

const (
	// FXRateAlertStale means the rate expires within the warning
	// threshold but has not expired yet.
	FXRateAlertStale FXRateAlertKind = "stale"
	// FXRateAlertExpired means now is at or past the rate's expiry.
	FXRateAlertExpired FXRateAlertKind = "expired"
)

// FXRateAlert is one rate-table condition found by a watch scan.
type FXRateAlert struct {
	FromCurrency     string         `json:"from_currency"`
	ToCurrency       string         `json:"to_currency"`
	Num              int64          `json:"num"`
	Den              int64          `json:"den"`
	EffectiveVersion uint64         `json:"effective_version"`
	ExpiresAt        time.Time      `json:"expires_at"`
	Kind             FXRateAlertKind `json:"kind"`
	DetectedAt       time.Time      `json:"detected_at"`
}

// ScanFXRateAlerts runs one read-only rate-table scan and returns every
// alert, sorted by (from, to) for determinism. It writes nothing to the
// books, emits no audit events, and touches no counters — the pure
// inspection variant, safe to call from tests and ad-hoc tooling.
//
// A rate with a zero ExpiresAt never expires and is never alerted. A
// non-positive warningThreshold disables stale warnings (expiry alerts
// are always active): the threshold is opt-in on purpose — without it a
// rate that legitimately expires far in the future would page on every
// scan of its final window.
func (l *Ledger) ScanFXRateAlerts(now time.Time, warningThreshold time.Duration) []FXRateAlert {
	l.mu.RLock()
	defer l.mu.RUnlock()

	var out []FXRateAlert
	for _, r := range l.fxRatesLocked() {
		if r.ExpiresAt.IsZero() {
			continue
		}
		switch {
		case !now.Before(r.ExpiresAt):
			out = append(out, FXRateAlert{
				FromCurrency: r.FromCurrency, ToCurrency: r.ToCurrency,
				Num: r.Num, Den: r.Den,
				EffectiveVersion: r.EffectiveVersion, ExpiresAt: r.ExpiresAt,
				Kind:       FXRateAlertExpired,
				DetectedAt: now.UTC(),
			})
		case warningThreshold > 0 && !now.Add(warningThreshold).Before(r.ExpiresAt):
			out = append(out, FXRateAlert{
				FromCurrency: r.FromCurrency, ToCurrency: r.ToCurrency,
				Num: r.Num, Den: r.Den,
				EffectiveVersion: r.EffectiveVersion, ExpiresAt: r.ExpiresAt,
				Kind:       FXRateAlertStale,
				DetectedAt: now.UTC(),
			})
		}
	}
	return out
}

// ReportFXRateAlerts records edge-triggered alerts: one structured audit
// event per alert (op "fx_rate_stale" / "fx_rate_expired") plus the
// ledger's alert counters. The scan itself writes nothing to the books —
// the write lock is held only to serialize the audit emission and the
// counter bump with concurrent writers.
func (l *Ledger) ReportFXRateAlerts(alerts []FXRateAlert) {
	l.mu.Lock()
	defer l.mu.Unlock()

	for _, a := range alerts {
		op := "fx_rate_stale"
		if a.Kind == FXRateAlertExpired {
			op = "fx_rate_expired"
			l.fxRateExpiredAlerts++
		} else {
			l.fxRateStaleAlerts++
		}
		l.emitAudit(AuditEvent{
			Op:            op,
			Actor:         "FXRateWatch",
			TraceID:       fmt.Sprintf("%s->%s", a.FromCurrency, a.ToCurrency),
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Details: map[string]any{
				"from_currency":     a.FromCurrency,
				"to_currency":       a.ToCurrency,
				"rate":              fmt.Sprintf("%d/%d", a.Num, a.Den),
				"effective_version": a.EffectiveVersion,
				"expires_at":        a.ExpiresAt.UTC().Format(time.RFC3339),
				"kind":              string(a.Kind),
			},
		})
	}
}

// FXRateStaleAlertCount reports how many fx_rate_stale alerts have been
// recorded since process start. The HTTP layer syncs this into
// ledger_fx_rate_stale_total on every /metrics scrape.
func (l *Ledger) FXRateStaleAlertCount() uint64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.fxRateStaleAlerts
}

// FXRateExpiredAlertCount reports how many fx_rate_expired alerts have
// been recorded since process start. The HTTP layer syncs this into
// ledger_fx_rate_expired_total on every /metrics scrape.
func (l *Ledger) FXRateExpiredAlertCount() uint64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.fxRateExpiredAlerts
}

// fxWatchMark is the edge-trigger dedup state for one rate pair: the
// identity of the rate the warnings were reported for, and which
// warnings already fired for that identity.
type fxWatchMark struct {
	expiresAt        time.Time
	effectiveVersion uint64
	staleWarned      bool
	expiredReported  bool
}

// FXRateWatch is the background FX-rate expiry watcher: one goroutine
// that scans the rate table on a ticker until its context is cancelled
// or Stop is called. Like the hold/health sweepers it shares the server
// context, fires its first tick after one full interval, and is disabled
// (nil) on a non-positive interval. The warning threshold is the
// opt-in stale window from ScanFXRateAlerts: non-positive disables
// stale warnings while expiry alerts stay active.
type FXRateWatch struct {
	l                *Ledger
	interval         time.Duration
	warningThreshold time.Duration
	onAlert          func([]FXRateAlert)

	done     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	mu   sync.Mutex
	seen map[fxPair]fxWatchMark

	ticks        atomic.Uint64
	staleTotal   atomic.Uint64
	expiredTotal atomic.Uint64
}

// StartFXRateWatch launches a background goroutine that scans
// l.ScanFXRateAlerts on every interval until ctx is cancelled or Stop is
// called. After each tick it invokes onAlert (when non-nil) with the
// newly fired alerts, so the caller can push notifications — the audit
// events and the ledger's own counters are recorded regardless, inside
// ReportFXRateAlerts.
//
// A non-positive interval disables the watcher: it returns nil and
// starts nothing, so callers can pass a configured interval straight
// through without branching. The first tick fires after one full
// interval, not immediately, so a freshly started process settles before
// it scans.
func StartFXRateWatch(ctx context.Context, l *Ledger, interval, warningThreshold time.Duration, onAlert func([]FXRateAlert)) *FXRateWatch {
	if interval <= 0 {
		return nil
	}
	w := &FXRateWatch{
		l:                l,
		interval:         interval,
		warningThreshold: warningThreshold,
		onAlert:          onAlert,
		done:             make(chan struct{}),
		seen:             make(map[fxPair]fxWatchMark),
	}
	w.wg.Add(1)
	go w.run(ctx)
	return w
}

func (w *FXRateWatch) run(ctx context.Context) {
	defer w.wg.Done()
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.done:
			return
		case <-ticker.C:
			w.tick()
		}
	}
}

// tick runs one watch scan, edge-triggers the new alerts, and prunes
// dedup marks for pairs no longer in the table.
func (w *FXRateWatch) tick() {
	now := time.Now()
	alerts := w.l.ScanFXRateAlerts(now, w.warningThreshold)

	pairs := make(map[fxPair]bool, len(alerts))
	w.l.mu.RLock()
	for p := range w.l.fxRates {
		pairs[p] = true
	}
	w.l.mu.RUnlock()

	var fresh []FXRateAlert
	w.mu.Lock()
	for p := range w.seen {
		if !pairs[p] {
			delete(w.seen, p)
		}
	}
	for _, a := range alerts {
		p := fxPair{from: a.FromCurrency, to: a.ToCurrency}
		m := w.seen[p]
		sameIdentity := m.expiresAt.Equal(a.ExpiresAt) && m.effectiveVersion == a.EffectiveVersion
		if sameIdentity {
			if a.Kind == FXRateAlertStale && m.staleWarned {
				continue
			}
			if a.Kind == FXRateAlertExpired && m.expiredReported {
				continue
			}
		} else {
			m = fxWatchMark{expiresAt: a.ExpiresAt, effectiveVersion: a.EffectiveVersion}
		}
		if a.Kind == FXRateAlertStale {
			m.staleWarned = true
			w.staleTotal.Add(1)
		} else {
			m.expiredReported = true
			w.expiredTotal.Add(1)
		}
		w.seen[p] = m
		fresh = append(fresh, a)
	}
	w.mu.Unlock()

	w.ticks.Add(1)
	if len(fresh) > 0 {
		w.l.ReportFXRateAlerts(fresh)
		if w.onAlert != nil {
			w.onAlert(fresh)
		}
	}
}

// Stop halts the watcher and waits for its goroutine to exit. It is
// idempotent and safe to call alongside context cancellation — whichever
// fires first wins, the other is a no-op.
func (w *FXRateWatch) Stop() {
	w.stopOnce.Do(func() { close(w.done) })
	w.wg.Wait()
}

// Ticks reports how many watch scans have completed. StaleTotal and
// ExpiredTotal report the total alerts edge-triggered across all ticks.
func (w *FXRateWatch) Ticks() uint64        { return w.ticks.Load() }
func (w *FXRateWatch) StaleTotal() uint64   { return w.staleTotal.Load() }
func (w *FXRateWatch) ExpiredTotal() uint64 { return w.expiredTotal.Load() }
