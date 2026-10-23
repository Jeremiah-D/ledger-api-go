package ledger

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Settlement SLA monitoring (LG-185, fintech / backend engineering).
//
// A payment is only done when the money arrives: the ledger tracks the
// instruction (payout.go), the gpi state machine tracks the
// correspondent-banking journey (gpi.go, LG-165), and this module
// tracks the *time* — whether each payment settles inside the
// settlement SLA its rail promises. It is the compliance-monitoring
// layer, orthogonal to the gpi state tracking: gpi says *where* the
// payment is, the SLA clock says whether it is *late*.
//
// Rails and their default SLAs (all operator-configurable via
// SetSLAConfig):
//
//	fednow  FedNow instant payments: under 20s, per the FedNow
//	        service's instant-settlement promise.
//	rtp     RTP (The Clearing House) instant payments: under 20s.
//	sepa    SEPA credit transfer: same business day (24h).
//	swift   SWIFT correspondent banking without gpi tracking: 48h.
//	gpi     SWIFT gpi: 24h, the gpi rulebook's same-day value
//	        expectation, tracked per UETR.
//
// A rail's SLA is a deadline, not a stopwatch that progress extends:
// when a payout is submitted (SubmitPayout) or a rail-bound transfer
// posts (PostTransfer), an SLA watch opens with deadline = submit
// time + the rail's SLA. The gpi state machine feeds the watch as it
// advances (see applyGPITransitionLocked): every transition refreshes
// the watch's last-progress timestamp, and reaching a terminal state
// closes it — ACCP as settled, RJCT as rejected. SettlePayout and
// FailPayout close it the same way.
//
// Breaches fire on evaluation, not on a timer inside the ledger:
// CheckSLAClocks(now) marks every open watch past its deadline as
// breached, emits one hash-chained sla_breached audit event per
// breach, and bumps the per-rail breach counter (surfaced as
// ledger_sla_breaches_total{rail}). The explicit now makes the whole
// thing deterministic under test; in production the background SLA
// sweeper (StartSLASweeper) or POST /sla/check drives it. A breached
// watch keeps its breach flag even if it later settles — the breach
// happened, the audit says so, and un-setting it would rewrite
// history. Each watch fires at most once.
//
// Payouts carry an optional Rail (see PayoutInstruction): empty means
// "no rail stated", and the watch then uses the gpi rail — every
// payout is a cross-border payment initiation with a UETR and a gpi
// track (LG-165), so the gpi/UETR settlement expectation governs when
// the operator names no rail. Transfers carry an optional Rail too;
// a transfer with no rail opens no watch (domestic book transfers
// have no rail SLA to miss).
//
// Watches are in-memory only, like payout instructions and gpi
// tracks: the hash-chained sla_breached audit events are the durable
// record, the same convention as the gpi status events.

// Payment rails with a settlement SLA expectation.
const (
	// RailFedNow is the FedNow instant-payment rail.
	RailFedNow = "fednow"
	// RailRTP is the RTP (The Clearing House) instant-payment rail.
	RailRTP = "rtp"
	// RailSEPA is the SEPA credit-transfer rail.
	RailSEPA = "sepa"
	// RailSWIFT is the SWIFT correspondent-banking rail (non-gpi).
	RailSWIFT = "swift"
	// RailGPI is the SWIFT gpi rail, tracked per UETR.
	RailGPI = "gpi"
)

// slaRails is the closed rail vocabulary, in canonical order: it
// bounds the ledger_sla_breaches_total{rail} label cardinality and
// sizes the per-rail breach counter array.
var slaRails = [5]string{RailFedNow, RailRTP, RailSEPA, RailSWIFT, RailGPI}

// defaultSLADuration returns the rail's default settlement SLA, before
// any operator override (see SetSLAConfig).
func defaultSLADuration(rail string) time.Duration {
	switch rail {
	case RailFedNow, RailRTP:
		return 20 * time.Second
	case RailSEPA:
		return 24 * time.Hour
	case RailSWIFT:
		return 48 * time.Hour
	case RailGPI:
		return 24 * time.Hour
	}
	return 0
}

// slaRailIndex maps a canonical rail to its breach-counter slot.
func slaRailIndex(rail string) (int, bool) {
	for i, r := range slaRails {
		if r == rail {
			return i, true
		}
	}
	return 0, false
}

// ErrInvalidRail is returned when a rail name is outside the SLA
// vocabulary (fednow, rtp, sepa, swift, gpi): unknown rails are
// refused, not coerced — like the gpi status vocabulary in gpi.go.
var ErrInvalidRail = errors.New("ledger: invalid payment rail: want fednow, rtp, sepa, swift or gpi")

// ErrInvalidSLADuration is returned by SetSLAConfig for a non-positive
// duration: an SLA of zero or less would breach every watch at open,
// which is a misconfiguration, not a policy.
var ErrInvalidSLADuration = errors.New("ledger: SLA duration must be positive")

// normalizeRail canonicalizes a rail name: trimmed, lowercased. Empty
// stays empty ("no rail stated"). Anything else must be in the SLA
// vocabulary.
func normalizeRail(rail string) (string, error) {
	r := strings.ToLower(strings.TrimSpace(rail))
	if r == "" {
		return "", nil
	}
	if _, ok := slaRailIndex(r); !ok {
		return "", fmt.Errorf("%w: %q", ErrInvalidRail, rail)
	}
	return r, nil
}

// SLA watch kinds.
const (
	// SLAWatchKindPayout is a watch opened by SubmitPayout.
	SLAWatchKindPayout = "payout"
	// SLAWatchKindTransfer is a watch opened by PostTransfer.
	SLAWatchKindTransfer = "transfer"
)

// SLA watch close reasons.
const (
	// SLACloseSettled means the payment settled (payout settled, gpi
	// track reached ACCP).
	SLACloseSettled = "settled"
	// SLACloseFailed means the payout failed (FailPayout).
	SLACloseFailed = "failed"
	// SLACloseRejected means the gpi track reached RJCT.
	SLACloseRejected = "rejected"
)

// SLAWatch is the settlement-SLA watch for one payout or transfer:
// the rail, when it opened, its deadline, the last gpi progress, and
// — once resolved — when and how it closed. Breached is sticky: once
// CheckSLAClocks fires the breach it stays true even if the payment
// later settles, because the breach happened.
type SLAWatch struct {
	ID             string    `json:"id"`
	Kind           string    `json:"kind"`
	Rail           string    `json:"rail"`
	OpenedAt       time.Time `json:"opened_at"`
	Deadline       time.Time `json:"deadline"`
	LastProgressAt time.Time `json:"last_progress_at"`
	ClosedAt       time.Time `json:"closed_at,omitempty"`
	CloseReason    string    `json:"close_reason,omitempty"`
	Breached       bool      `json:"breached"`
}

// SLABreach is one fired SLA breach: the watch's identity, its
// timeline, and when the breach fired.
type SLABreach struct {
	ID       string    `json:"id"`
	Kind     string    `json:"kind"`
	Rail     string    `json:"rail"`
	OpenedAt time.Time `json:"opened_at"`
	Deadline time.Time `json:"deadline"`
	FiredAt  time.Time `json:"fired_at"`
}

// SetSLAConfig overrides a rail's settlement SLA: the new duration
// applies to watches opened after the call; watches already open keep
// the deadline they were born with — changing the SLA mid-flight
// would rewrite the commitment the watch was opened under. The rail
// must be in the vocabulary and the duration positive.
func (l *Ledger) SetSLAConfig(rail string, d time.Duration) error {
	r, err := normalizeRail(rail)
	if err != nil {
		return err
	}
	if r == "" {
		return fmt.Errorf("%w: %q", ErrInvalidRail, rail)
	}
	if d <= 0 {
		return fmt.Errorf("%w: %v", ErrInvalidSLADuration, d)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.slaDurations[r] = d
	return nil
}

// SLADuration reports a rail's effective settlement SLA: the
// operator's override when set, the default otherwise.
func (l *Ledger) SLADuration(rail string) (time.Duration, error) {
	r, err := normalizeRail(rail)
	if err != nil {
		return 0, err
	}
	if r == "" {
		return 0, fmt.Errorf("%w: %q", ErrInvalidRail, rail)
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.slaDurationLocked(r), nil
}

// slaDurationLocked returns the effective SLA for a canonical rail.
// Callers must hold at least the read lock.
func (l *Ledger) slaDurationLocked(rail string) time.Duration {
	if d, ok := l.slaDurations[rail]; ok {
		return d
	}
	return defaultSLADuration(rail)
}

// openSLAWatchLocked opens the SLA watch for id (a payout or transfer
// ID) on rail, with deadline = openedAt + the rail's effective SLA.
// Opening a watch for an ID that already has one is a no-op —
// re-submits and replays must not restart the clock. Callers must
// hold the write lock; rail must be canonical (normalize first).
func (l *Ledger) openSLAWatchLocked(id, kind, rail string, openedAt time.Time) {
	if _, exists := l.slaWatches[id]; exists {
		return
	}
	deadline := openedAt.Add(l.slaDurationLocked(rail))
	l.slaWatches[id] = &SLAWatch{
		ID:             id,
		Kind:           kind,
		Rail:           rail,
		OpenedAt:       openedAt,
		Deadline:       deadline,
		LastProgressAt: openedAt,
	}
}

// closeSLAWatchLocked closes the watch for id with reason at at. A
// missing or already-closed watch is a no-op; a breached watch keeps
// its breach flag. Callers must hold the write lock.
func (l *Ledger) closeSLAWatchLocked(id, reason string, at time.Time) {
	w, ok := l.slaWatches[id]
	if !ok || !w.ClosedAt.IsZero() {
		return
	}
	w.ClosedAt = at
	w.CloseReason = reason
}

// SLAWatch returns the watch for id, or false when no watch was ever
// opened for it. The returned record is a copy — callers cannot
// mutate ledger state through it.
func (l *Ledger) SLAWatch(id string) (SLAWatch, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	w, ok := l.slaWatches[id]
	if !ok {
		return SLAWatch{}, false
	}
	return *w, true
}

// SLAWatches returns every watch sorted by ID: the read side of the
// registry.
func (l *Ledger) SLAWatches() []SLAWatch {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]SLAWatch, 0, len(l.slaWatches))
	for _, w := range l.slaWatches {
		out = append(out, *w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// CheckSLAClocks evaluates every open SLA watch against now: a watch
// whose deadline has passed and which has not breached yet fires — it
// is marked breached (sticky), one hash-chained sla_breached audit
// event is emitted, and the rail's breach counter bumps. Watches that
// closed on time, watches still inside their SLA, and watches that
// already fired are left alone, so each watch breaches at most once.
//
// The explicit now is the seam that makes the clock deterministic:
// tests pass a synthetic time; production passes time.Now().UTC via
// the background sweeper or POST /sla/check.
func (l *Ledger) CheckSLAClocks(now time.Time) []SLABreach {
	l.mu.Lock()
	defer l.mu.Unlock()
	var fired []SLABreach
	// Deterministic order: iterate sorted IDs so the audit events and
	// the returned breaches read the same way every run.
	ids := make([]string, 0, len(l.slaWatches))
	for id := range l.slaWatches {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		w := l.slaWatches[id]
		if w.Breached || !w.ClosedAt.IsZero() {
			continue
		}
		if !now.After(w.Deadline) {
			continue
		}
		w.Breached = true
		overdue := now.Sub(w.Deadline)
		l.emitAudit(AuditEvent{
			Op:            "sla_breached",
			Actor:         "CheckSLAClocks",
			TraceID:       w.ID,
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Details: map[string]any{
				"id":          w.ID,
				"kind":        w.Kind,
				"rail":        w.Rail,
				"opened_at":   w.OpenedAt.UTC().Format(time.RFC3339),
				"deadline":    w.Deadline.UTC().Format(time.RFC3339),
				"overdue":     overdue.String(),
				"close_state": "open",
			},
		})
		if i, ok := slaRailIndex(w.Rail); ok {
			l.slaBreaches[i].Add(1)
		}
		fired = append(fired, SLABreach{
			ID:       w.ID,
			Kind:     w.Kind,
			Rail:     w.Rail,
			OpenedAt: w.OpenedAt,
			Deadline: w.Deadline,
			FiredAt:  now,
		})
	}
	return fired
}

// updateSLAWatchForGPILocked feeds a gpi transition into the payout's
// SLA watch: the transition refreshes the watch's last-progress
// timestamp — the SLA clock sees the payment moving — and a terminal
// transition closes the watch (ACCP as settled, RJCT as rejected).
// Non-terminal transitions never extend the deadline: the SLA is a
// deadline, and progress does not renegotiate it. Callers must hold
// the write lock; called from applyGPITransitionLocked, so both the
// operator advance and the pain.002 auto-advance feed the clock.
func (l *Ledger) updateSLAWatchForGPILocked(payoutID, status string, now time.Time) {
	if payoutID == "" {
		return
	}
	w, ok := l.slaWatches[payoutID]
	if !ok || !w.ClosedAt.IsZero() {
		return
	}
	w.LastProgressAt = now
	switch status {
	case GPIStatusACCP:
		l.closeSLAWatchLocked(payoutID, SLACloseSettled, now)
	case GPIStatusRJCT:
		l.closeSLAWatchLocked(payoutID, SLACloseRejected, now)
	}
}

// SLABreachCounts reports the per-rail SLA breach counts: the
// ledger-side source of the ledger_sla_breaches_total{rail} metric.
// Operational counts, not books: in-memory only, like the gpi
// lifecycle counters.
func (l *Ledger) SLABreachCounts() map[string]uint64 {
	out := make(map[string]uint64, len(slaRails))
	for i, rail := range slaRails {
		if n := l.slaBreaches[i].Load(); n > 0 {
			out[rail] = n
		}
	}
	return out
}

// SLASweeper is the background SLA clock evaluator: every tick it
// runs CheckSLAClocks against the ledger, so breaches fire without an
// operator polling POST /sla/check.
type SLASweeper struct {
	l        *Ledger
	interval time.Duration
	onSweep  func(breached int)
	done     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
	ticks    atomic.Uint64
	breached atomic.Uint64
}

// StartSLASweeper starts the background SLA clock evaluator, or
// returns nil when interval <= 0 (disabled).
func StartSLASweeper(ctx context.Context, l *Ledger, interval time.Duration, onSweep func(breached int)) *SLASweeper {
	if interval <= 0 {
		return nil
	}
	sw := &SLASweeper{
		l:        l,
		interval: interval,
		onSweep:  onSweep,
		done:     make(chan struct{}),
	}
	sw.wg.Add(1)
	go sw.run(ctx)
	return sw
}

func (sw *SLASweeper) run(ctx context.Context) {
	defer sw.wg.Done()
	ticker := time.NewTicker(sw.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sw.done:
			return
		case <-ticker.C:
			breached := sw.l.CheckSLAClocks(time.Now().UTC())
			sw.ticks.Add(1)
			sw.breached.Add(uint64(len(breached)))
			if sw.onSweep != nil {
				sw.onSweep(len(breached))
			}
		}
	}
}

// Stop halts the sweeper and waits for its goroutine to exit. It is
// idempotent and safe to call alongside context cancellation.
func (sw *SLASweeper) Stop() {
	sw.stopOnce.Do(func() { close(sw.done) })
	sw.wg.Wait()
}

// Ticks reports how many evaluation sweeps have completed.
// BreachedTotal reports the total breaches fired across all ticks.
func (sw *SLASweeper) Ticks() uint64         { return sw.ticks.Load() }
func (sw *SLASweeper) BreachedTotal() uint64 { return sw.breached.Load() }
