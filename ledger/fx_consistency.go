package ledger

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// FX rate triangular-consistency check (LG-76).
//
// Cross-currency transfers price each leg off the rate table
// independently (LG-27/LG-34). Nothing on the write path asks whether
// the table is *self-consistent*: if the rates USD→EUR, EUR→JPY and
// JPY→USD multiply to something far from 1, the book has an arbitrage
// leak — a payer routing through the table can mint or burn value
// across currencies while every single leg looks legitimate. The
// inconsistency is a data-quality problem in the operator's rate feed,
// not a booking bug, so the check deliberately never blocks a
// conversion: it is a pure read-only scan that raises an advisory
// alert, exactly like the LG-68 expiry watch.
//
// A rate table is triangle-consistent when every directed 3-cycle
// A→B→C→A multiplies to 1. The scan enumerates every unordered
// currency triple and checks both directed cycles (each needs all
// three legs present), computing the product in exact big.Rat arithmetic (the table stores integer num/den, so
// the product is exact; no float rounding can hide or invent a
// deviation). A cycle whose |product − 1| exceeds the opt-in threshold
// (basis points) is reported as an "fx_rate_inconsistent" structured
// audit event plus the ledger_fx_rate_inconsistency_total counter.
//
// The scan runs on the reference MID prices (ExchangeRate.Num/Den),
// not the LG-93 dealing prices, deliberately: bid ≤ mid ≤ ask by
// construction, so the customer always deals at a price weakly worse
// than mid — a mid-consistent table cannot offer the customer a
// triangular arbitrage (any ask-cycle product is ≥ the mid-cycle
// product), while the desk's own bid-side cycle is its inventory
// business, not a book leak. The dealing spread is P&L attribution, not
// a consistency property.
//
// Edge-triggered, LG-68 style: a cycle that stays inconsistent across
// ticks alerts once, when it *becomes* inconsistent. The dedup identity
// per canonical cycle is the three legs' (num, den) values — changing
// any leg's value re-arms the alert, setting the same values again
// does not, and a cycle that loses a leg drops its mark on the next
// tick. The scan itself writes nothing to the books: no journal rows,
// no version bumps; the only mutations are the audit events and the
// alert counter.

// FXRateInconsistency is one triangle the consistency scan found
// inconsistent: the exact product of the cycle's three legs deviates
// from 1 by more than the threshold.
type FXRateInconsistency struct {
	// Path is the canonical directed cycle, e.g. {"EUR","JPY","USD"}
	// meaning EUR→JPY→USD→EUR. The canonical form starts at the
	// lexicographically smallest currency, so each directed cycle
	// appears exactly once.
	Path [3]string `json:"path"`
	// Legs are the three table rates in cycle order.
	Legs [3]ExchangeRate `json:"legs"`
	// Product is the exact cycle product as "num/den".
	Product string `json:"product"`
	// DeviationBps is the exact |product − 1| in basis points,
	// as "num/den".
	DeviationBps string    `json:"deviation_bps"`
	DetectedAt   time.Time `json:"detected_at"`
}

// fxConsistencyLeg is the dedup identity of one cycle leg: the rate's
// value. A value change re-arms the alert; re-setting the same value
// does not.
type fxConsistencyLeg struct {
	num int64
	den int64
}

// ScanFXRateInconsistency runs one read-only rate-table scan and
// returns every directed 3-cycle whose exact product deviates from 1 by
// more than thresholdBps basis points, sorted by path for
// determinism. It writes nothing to the books, emits no audit events,
// and touches no counters — the pure inspection variant, safe to call
// from tests and ad-hoc tooling.
//
// A nil or non-positive threshold disables the check entirely (opt-in
// on purpose: without a threshold every microscopic feed wobble would
// page). Cycles missing a leg are skipped — consistency is only
// defined where the table has an opinion.
func (l *Ledger) ScanFXRateInconsistency(thresholdBps int64) []FXRateInconsistency {
	if thresholdBps <= 0 {
		return nil
	}
	l.mu.RLock()
	defer l.mu.RUnlock()

	// Collect the currency universe and index legs by ordered pair.
	currencies := map[string]bool{}
	legs := map[[2]string]ExchangeRate{}
	for p, r := range l.fxRates {
		currencies[p.from] = true
		currencies[p.to] = true
		legs[[2]string{p.from, p.to}] = r
	}
	var names []string
	for c := range currencies {
		names = append(names, c)
	}
	sort.Strings(names)

	threshold := big.NewRat(thresholdBps, 1)
	one := big.NewRat(1, 1)
	bps := big.NewRat(10000, 1)

	var out []FXRateInconsistency
	// Unordered triple (a,b,c), a<b<c, carries two directed cycles:
	// a→b→c→a and a→c→b→a. Both are canonical (start at the smallest).
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			for k := j + 1; k < len(names); k++ {
				a, b, c := names[i], names[j], names[k]
				for _, cycle := range [][3]string{{a, b, c}, {a, c, b}} {
					l0, ok0 := legs[[2]string{cycle[0], cycle[1]}]
					l1, ok1 := legs[[2]string{cycle[1], cycle[2]}]
					l2, ok2 := legs[[2]string{cycle[2], cycle[0]}]
					if !ok0 || !ok1 || !ok2 {
						continue
					}
					// Exact product: integer num/den legs, so no
					// float rounding can hide or invent a
					// deviation.
					prod := new(big.Rat).Mul(
						new(big.Rat).Mul(
							big.NewRat(l0.Num, l0.Den),
							big.NewRat(l1.Num, l1.Den)),
						big.NewRat(l2.Num, l2.Den))
					dev := new(big.Rat).Abs(new(big.Rat).Sub(prod, one))
					devBps := new(big.Rat).Mul(dev, bps)
					if devBps.Cmp(threshold) <= 0 {
						continue
					}
					out = append(out, FXRateInconsistency{
						Path:         cycle,
						Legs:         [3]ExchangeRate{l0, l1, l2},
						Product:      prod.RatString(),
						DeviationBps: devBps.RatString(),
						DetectedAt:   time.Now().UTC(),
					})
				}
			}
		}
	}
	return out
}

// ReportFXRateInconsistency records edge-triggered inconsistency
// alerts: one structured audit event per alert (op
// "fx_rate_inconsistent") plus the ledger's alert counter. The scan
// itself writes nothing to the books — the write lock is held only to
// serialize the audit emission and the counter bump with concurrent
// writers.
func (l *Ledger) ReportFXRateInconsistency(alerts []FXRateInconsistency) {
	l.mu.Lock()
	defer l.mu.Unlock()

	for _, a := range alerts {
		l.fxRateInconsistentAlerts++
		path := fmt.Sprintf("%s->%s->%s->%s", a.Path[0], a.Path[1], a.Path[2], a.Path[0])
		legs := make([]string, 3)
		for i, leg := range a.Legs {
			legs[i] = fmt.Sprintf("%d/%d", leg.Num, leg.Den)
		}
		l.emitAudit(AuditEvent{
			Op:            "fx_rate_inconsistent",
			Actor:         "FXConsistencyWatch",
			TraceID:       path,
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Details: map[string]any{
				"path":          path,
				"legs":          legs,
				"product":       a.Product,
				"deviation_bps": a.DeviationBps,
			},
		})
	}
}

// FXRateInconsistencyAlertCount reports how many fx_rate_inconsistent
// alerts have been recorded since process start. The HTTP layer syncs
// this into ledger_fx_rate_inconsistency_total on every /metrics
// scrape.
func (l *Ledger) FXRateInconsistencyAlertCount() uint64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.fxRateInconsistentAlerts
}

// FXConsistencyWatch is the background FX-rate triangular-consistency
// watcher: one goroutine that scans the rate table on a ticker until
// its context is cancelled or Stop is called. Like the expiry watcher
// (LG-68) and the other sweepers it shares the server context, fires
// its first tick after one full interval, and is disabled (nil) on a
// non-positive interval. A non-positive thresholdBps disables the
// check: the threshold is opt-in — microscopic feed wobble must not
// page by default.
type FXConsistencyWatch struct {
	l            *Ledger
	interval     time.Duration
	thresholdBps int64
	onAlert      func([]FXRateInconsistency)

	done     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	mu   sync.Mutex
	seen map[[3]string][3]fxConsistencyLeg

	ticks atomic.Uint64
	total atomic.Uint64
}

// StartFXConsistencyWatch launches a background goroutine that scans
// l.ScanFXRateInconsistency on every interval until ctx is cancelled
// or Stop is called. After each tick it invokes onAlert (when non-nil)
// with the newly fired alerts, so the caller can push notifications —
// the audit events and the ledger's own counter are recorded
// regardless, inside ReportFXRateInconsistency.
//
// A non-positive interval or thresholdBps disables the watcher: it
// returns nil and starts nothing, so callers can pass configured
// values straight through without branching. The first tick fires
// after one full interval, not immediately, so a freshly started
// process settles before it scans.
func StartFXConsistencyWatch(ctx context.Context, l *Ledger, interval time.Duration, thresholdBps int64, onAlert func([]FXRateInconsistency)) *FXConsistencyWatch {
	if interval <= 0 || thresholdBps <= 0 {
		return nil
	}
	w := &FXConsistencyWatch{
		l:            l,
		interval:     interval,
		thresholdBps: thresholdBps,
		onAlert:      onAlert,
		done:         make(chan struct{}),
		seen:         make(map[[3]string][3]fxConsistencyLeg),
	}
	w.wg.Add(1)
	go w.run(ctx)
	return w
}

func (w *FXConsistencyWatch) run(ctx context.Context) {
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

// tick runs one consistency scan, edge-triggers the newly inconsistent
// cycles, and prunes dedup marks for cycles that no longer have all
// three legs in the table.
func (w *FXConsistencyWatch) tick() {
	alerts := w.l.ScanFXRateInconsistency(w.thresholdBps)

	var fresh []FXRateInconsistency
	w.mu.Lock()
	present := make(map[[3]string]bool, len(alerts))
	for _, a := range alerts {
		present[a.Path] = true
	}
	for p := range w.seen {
		if !present[p] {
			delete(w.seen, p)
		}
	}
	for _, a := range alerts {
		identity := [3]fxConsistencyLeg{
			{num: a.Legs[0].Num, den: a.Legs[0].Den},
			{num: a.Legs[1].Num, den: a.Legs[1].Den},
			{num: a.Legs[2].Num, den: a.Legs[2].Den},
		}
		if m, ok := w.seen[a.Path]; ok && m == identity {
			continue
		}
		w.seen[a.Path] = identity
		fresh = append(fresh, a)
		w.total.Add(1)
	}
	w.mu.Unlock()

	w.ticks.Add(1)
	if len(fresh) > 0 {
		w.l.ReportFXRateInconsistency(fresh)
		if w.onAlert != nil {
			w.onAlert(fresh)
		}
	}
}

// Stop halts the watcher and waits for its goroutine to exit. It is
// idempotent and safe to call alongside context cancellation —
// whichever fires first wins, the other is a no-op.
func (w *FXConsistencyWatch) Stop() {
	w.stopOnce.Do(func() { close(w.done) })
	w.wg.Wait()
}

// Ticks reports how many consistency scans have completed. Total
// reports the total alerts edge-triggered across all ticks.
func (w *FXConsistencyWatch) Ticks() uint64 { return w.ticks.Load() }
func (w *FXConsistencyWatch) Total() uint64 { return w.total.Load() }
