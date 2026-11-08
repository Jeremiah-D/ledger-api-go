package ledger

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Balance-health watchdog (LG-49).
//
// The write path guards every booking with risk controls — frozen
// accounts, overdraft protection, daily limits, period locks — but a
// control that never fires is a control nobody verifies. The health
// watchdog is the in-process verifier: a background goroutine that
// periodically re-checks the invariants the write path promises, read
// only, and reports every break as a structured audit event
// ("balance_health_violation") plus the ledger_balance_health_violations_total
// counter. An unhealthy finding is defense-in-depth signal: it means a
// bug, a bad snapshot import, or operator config drift got past the
// write-path gates — the books themselves are never mutated by the
// watchdog.
//
// Three invariants, all cheap enough to run on a ticker:
//
//  1. negative_balance — an overdraft-protected account must never carry
//     a negative (account, currency) balance: the write path rejects
//     any posting that would take it below zero (see overdraft.go).
//     Note the check is scoped to protected accounts on purpose. This
//     ledger's sign convention (debit increases, credit decreases)
//     leaves revenue, liability, and payer accounts with legitimately
//     negative balances; flagging every negative balance would page on
//     normal books. The invariant that must never break is the one the
//     write path guarantees.
//  2. accounting_equation — spot check: for a rotating subset of
//     (account, currency) keys, net balance == total debits − total
//     credits. Each scan covers a deterministic slice of the keys
//     (sorted, windowed, cursor-advanced), so every key is re-checked
//     at least once every healthWatchWindowDivisor scans — a full
//     equation audit amortized over time. The divisor defaults to 4
//     and is revisable at runtime via SetHealthWatchWindowDivisor
//     (LG-356). Per-account balance implies the global per-currency
//     equality, so the rotation converges to the whole check.
//  3. version_continuity — len(chain) == version and the head link's
//     seq == version: the audit chain and the journal version counter
//     must stay in step (see the chain-health check in reconcile.go).
//     A mismatch means entries were booked without version bumps (or
//     vice versa) — exactly the corruption a bad restore leaves.
//
// The scan itself is read-only under the ledger's lock: it writes
// nothing to the books — no journal rows, no balance changes, no version
// bumps. The only mutations are the audit events and the violation
// counter, which is why RunHealthCheck takes the write lock (to
// serialize audit emission with the counter bump); ScanBalanceHealth is
// the pure read-only variant for tests and ad-hoc inspection. The
// rotation cursor is an atomic counter — observability state, not books.

// HealthCheck names a balance-health invariant checked by the watchdog.
const (
	// HealthCheckNegativeBalance flags an overdraft-protected account
	// carrying a negative (account, currency) balance.
	HealthCheckNegativeBalance = "negative_balance"
	// HealthCheckAccountingEquation flags an (account, currency) whose
	// net balance differs from total debits − total credits.
	HealthCheckAccountingEquation = "accounting_equation"
	// HealthCheckVersionContinuity flags the audit chain and the journal
	// version counter being out of step.
	HealthCheckVersionContinuity = "version_continuity"
)

// HealthViolation is one broken invariant found by a health scan.
type HealthViolation struct {
	// Check is one of the HealthCheck* constants above.
	Check string `json:"check"`
	// Account and Currency name the (account, currency) the check
	// failed on. Empty for ledger-wide checks (version_continuity).
	Account  AccountID `json:"account,omitempty"`
	Currency string     `json:"currency,omitempty"`
	// Details carries the check-specific numbers (balance, debits,
	// credits; version, chain links, head seq).
	Details map[string]any `json:"details,omitempty"`
	// DetectedAt is the scan time, UTC.
	DetectedAt time.Time `json:"detected_at"`
}

// defaultHealthWatchWindowDivisor is the default spot-check window:
// each scan checks ceil(n/divisor) of the (account, currency) keys,
// so every key is re-checked at least once every divisor scans. The
// live value is Ledger.healthWatchWindowDivisor, revisable at runtime
// via SetHealthWatchWindowDivisor (LG-356); this const is only the
// construction-time default, restored on restart.
const defaultHealthWatchWindowDivisor = 4

// ScanBalanceHealth runs one read-only health scan and returns every
// violation found, sorted by (check, account, currency) for
// determinism. It writes nothing to the books, emits no audit events,
// and touches no counters — the pure inspection variant, safe to call
// from tests and ad-hoc tooling. The rotation cursor advances (atomic,
// observability state only), so consecutive scans walk the key space.
func (l *Ledger) ScanBalanceHealth() []HealthViolation {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.scanBalanceHealthLocked()
}

// scanBalanceHealthLocked runs the three invariant checks. Callers must
// hold l.mu; the read lock suffices because the scan mutates nothing but
// the atomic rotation cursor.
func (l *Ledger) scanBalanceHealthLocked() []HealthViolation {
	var out []HealthViolation
	now := time.Now().UTC()

	// Union of (account, currency) keys, sorted — the same key space
	// VerifyAccountingEquation walks.
	seen := make(map[accountCurrency]bool)
	for k := range l.balances {
		seen[k] = true
	}
	for k := range l.debitTotals {
		seen[k] = true
	}
	for k := range l.creditTotals {
		seen[k] = true
	}
	keys := make([]accountCurrency, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].account != keys[j].account {
			return keys[i].account < keys[j].account
		}
		return keys[i].currency < keys[j].currency
	})

	// 1. Negative balances on overdraft-protected accounts: the hard
	// invariant the write path guarantees (see the package doc for why
	// the check is scoped this way). Full scan every time — it is a
	// single map walk.
	for _, k := range keys {
		if !l.noOverdraft[k.account] {
			continue
		}
		if b := l.balances[k]; b < 0 {
			out = append(out, HealthViolation{
				Check:      HealthCheckNegativeBalance,
				Account:    k.account,
				Currency:   k.currency,
				Details:    map[string]any{"balance_cents": b},
				DetectedAt: now,
			})
		}
	}

	// 2. Accounting-equation spot check on a rotating window: every key
	// is re-checked at least once every l.healthWatchWindowDivisor
	// scans (default 4, revisable at runtime via
	// SetHealthWatchWindowDivisor, LG-356).
	if n := len(keys); n > 0 {
		size := (n + l.healthWatchWindowDivisor - 1) / l.healthWatchWindowDivisor
		if size < 1 {
			size = 1
		}
		start := int(l.healthCursor.Load() % uint64(n))
		for i := 0; i < size; i++ {
			k := keys[(start+i)%n]
			d := l.debitTotals[k]
			c := l.creditTotals[k]
			if want := d - c; l.balances[k] != want {
				out = append(out, HealthViolation{
					Check:    HealthCheckAccountingEquation,
					Account:  k.account,
					Currency: k.currency,
					Details: map[string]any{
						"balance_cents":       l.balances[k],
						"total_debits_cents":  d,
						"total_credits_cents": c,
					},
					DetectedAt: now,
				})
			}
		}
		l.healthCursor.Add(uint64(size))
	}

	// 3. Version continuity: the audit chain and the journal version
	// counter stay in step — the same head-consistency check the
	// reconciliation report runs (see reconcile.go).
	if n := len(l.chain); n != int(l.version) || (n > 0 && l.chain[n-1].seq != l.version) {
		var headSeq uint64
		if n := len(l.chain); n > 0 {
			headSeq = l.chain[n-1].seq
		}
		out = append(out, HealthViolation{
			Check: HealthCheckVersionContinuity,
			Details: map[string]any{
				"version":     l.version,
				"chain_links": len(l.chain),
				"head_seq":    headSeq,
			},
			DetectedAt: now,
		})
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Check != out[j].Check {
			return out[i].Check < out[j].Check
		}
		if out[i].Account != out[j].Account {
			return out[i].Account < out[j].Account
		}
		return out[i].Currency < out[j].Currency
	})
	return out
}

// RunHealthCheck runs one health scan and reports every violation: one
// structured audit event per violation (op "balance_health_violation")
// plus the ledger's violation counter. The scan itself writes nothing to
// the books — the write lock is held only to serialize the audit
// emission and the counter bump with concurrent writers. It returns the
// violations found, like ScanBalanceHealth.
func (l *Ledger) RunHealthCheck() []HealthViolation {
	l.mu.Lock()
	defer l.mu.Unlock()

	violations := l.scanBalanceHealthLocked()
	for _, v := range violations {
		traceID := v.Check
		if v.Account != "" {
			traceID += ":" + string(v.Account)
			if v.Currency != "" {
				traceID += ":" + v.Currency
			}
		}
		details := make(map[string]any, len(v.Details)+2)
		for k, d := range v.Details {
			details[k] = d
		}
		details["check"] = v.Check
		var accounts []AccountID
		if v.Account != "" {
			accounts = []AccountID{v.Account}
			details["account"] = string(v.Account)
		}
		if v.Currency != "" {
			details["currency"] = v.Currency
		}
		l.emitAudit(AuditEvent{
			Op:            "balance_health_violation",
			Actor:         "HealthWatch",
			TraceID:       traceID,
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Accounts:      accounts,
			Details:       details,
		})
	}
	l.healthViolations += uint64(len(violations))
	return violations
}

// HealthViolationCount reports how many balance-health violations the
// watchdog has found since process start. The HTTP layer syncs this into
// ledger_balance_health_violations_total on every /metrics scrape.
func (l *Ledger) HealthViolationCount() uint64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.healthViolations
}

// HealthWatch is the background balance-health sweeper: one goroutine
// that calls RunHealthCheck on a ticker until its context is cancelled
// or Stop is called. Like the hold/review/schedule sweepers it shares
// the server context, fires its first tick after one full interval, and
// is disabled (nil) on a non-positive interval. The scan interval is
// revisable at runtime in place via SetInterval (LG-327): a revision
// wakes run() through retune, which stops the old ticker, runs a tick
// that fired while the revision was in flight (so the revision cannot
// swallow a due scan), and rebuilds the ticker on the new live
// interval.
type HealthWatch struct {
	l           *Ledger
	interval    time.Duration
	onViolation func([]HealthViolation)

	done     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	// mu guards interval: run() re-reads the live value under RLock
	// when it rebuilds its ticker after a SetInterval revision
	// (LG-327), so a runtime revision governs from the next tick
	// without restarting the watch.
	mu sync.RWMutex

	// retune wakes run() when SetInterval revises the live scan
	// interval. Buffered 1 and sent non-blocking, along the
	// ReportSweeper (LG-316) pattern: a revision never stalls on a
	// loop that is mid-scan, and a newer revision supersedes a
	// coalesced wakeup because run() re-reads the live value.
	retune chan struct{}

	ticks           atomic.Uint64
	violationsTotal atomic.Uint64
}

// StartHealthWatch launches a background goroutine that runs
// l.RunHealthCheck() every interval until ctx is cancelled or Stop is
// called. After each tick it invokes onViolation (when non-nil) with the
// violations that tick found, so the caller can push alerts — the audit
// events and the ledger's own counter are recorded regardless, inside
// RunHealthCheck.
//
// A non-positive interval disables the watchdog: it returns nil and
// starts nothing, so callers can pass a configured interval straight
// through without branching. The first tick fires after one full
// interval, not immediately, so a freshly started process settles before
// it scans.
func StartHealthWatch(ctx context.Context, l *Ledger, interval time.Duration, onViolation func([]HealthViolation)) *HealthWatch {
	if interval <= 0 {
		return nil
	}
	hw := &HealthWatch{
		l:           l,
		interval:    interval,
		onViolation: onViolation,
		done:        make(chan struct{}),
		retune:      make(chan struct{}, 1),
	}
	hw.wg.Add(1)
	go hw.run(ctx)
	return hw
}

func (hw *HealthWatch) run(ctx context.Context) {
	defer hw.wg.Done()
	ticker := time.NewTicker(hw.Interval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-hw.done:
			return
		case <-hw.retune:
			// A tick may have fired while the revision was in
			// flight: run it now so the revision cannot swallow
			// a due scan, then rebuild the ticker on the new
			// live interval. The old ticker is stopped before
			// the new one is built, so two tickers never run in
			// parallel; a scan already in progress is
			// unaffected — the loop only selects between scans,
			// so the revision waits until it finishes.
			ticker.Stop()
			select {
			case <-ticker.C:
				hw.tick()
			default:
			}
			ticker = time.NewTicker(hw.Interval())
		case <-ticker.C:
			hw.tick()
		}
	}
}

// tick runs one health scan, records the violations it found, and
// invokes onViolation (when non-nil) with them. It is the single
// scan path for both the ticker and the retune drain, so a revision
// can never swallow a due scan.
func (hw *HealthWatch) tick() {
	violations := hw.l.RunHealthCheck()
	hw.ticks.Add(1)
	hw.violationsTotal.Add(uint64(len(violations)))
	if len(violations) > 0 && hw.onViolation != nil {
		hw.onViolation(violations)
	}
}

// Stop halts the watchdog and waits for its goroutine to exit. It is
// idempotent and safe to call alongside context cancellation — whichever
// fires first wins, the other is a no-op.
func (hw *HealthWatch) Stop() {
	hw.stopOnce.Do(func() { close(hw.done) })
	hw.wg.Wait()
}

// Ticks reports how many health scans have completed. ViolationsTotal
// reports the total number of violations found across all ticks.
func (hw *HealthWatch) Ticks() uint64            { return hw.ticks.Load() }
func (hw *HealthWatch) ViolationsTotal() uint64  { return hw.violationsTotal.Load() }
