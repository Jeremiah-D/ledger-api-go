package ledger

import (
	"context"
	"errors"
	"fmt"
)

// Report run history depth runtime revision (LG-395).
//
// Each report subscription keeps a bounded run history — the newest
// runs stay, the oldest are evicted (see recordReportRunLocked) —
// and the bound was a construction-time constant (maxReportRuns,
// 100). A compliance team that wants a full year of daily-report
// history (365 runs) or an operator under memory pressure who wants
// to tighten to 30 runs had to change the constant and redeploy.
// This module is the orthogonal runtime path: an operator revises
// the live depth in place, instead of restarting the server.
//
// Semantics, along LG-347 (the forward grace: the same knob,
// (previous, error) audit shape) and LG-344 (the check order —
// validate, compare, swap, then audit):
//
//   - The depth is a live field on the Ledger, guarded by l.mu —
//     the same lock that guards the run-history maps, so no second
//     lock is introduced and no lock order is created. The only
//     enforcement read site (recordReportRunLocked) reads it under
//     the write lock its caller already holds.
//   - The constant stays as the default-value source, renamed
//     defaultMaxReportRuns: the live field seeds from it (100) at
//     construction.
//   - A depth below 1 is rejected with
//     ErrInvalidReportRunHistoryDepth and the live value is left
//     untouched. Zero is rejected, not a "keep nothing" state: a
//     zero depth would make ListReportRuns return nothing forever,
//     which is switching the query surface off, not a meaningful
//     retention policy (along LG-336). There is no upper bound:
//     the memory trade-off belongs to the operator (along LG-356).
//   - Revising to the current depth is a no-op with no audit
//     event. The revision is audited as report_runs_retuned, with
//     previous/current as integers.
//   - Tightening trims every subscription's stored history to the
//     new depth immediately, oldest first, inside the same write
//     lock — the next ListReportRuns already sees the new depth
//     (along LG-336's tighten-trims-immediately rule). Widening
//     only affects later appends: history already evicted is never
//     revived. The revision never touches RunSeq or any
//     subscription field (ReviseReportSchedule, LG-315, owns those).
//   - Report subscriptions are process-lifetime state and do not
//     round-trip through snapshots (ledger.go): a restart restores
//     both the subscriptions and the 100-run default. The revision
//     event stays in the audit trail.

var (
	// ErrInvalidReportRunHistoryDepth is returned by
	// SetReportRunHistoryDepth for a depth below 1: the live depth
	// is left untouched.
	ErrInvalidReportRunHistoryDepth = errors.New("ledger: report run history depth must be at least 1")
)

// SetReportRunHistoryDepth revises the live per-schedule run
// history depth in place and returns the pre-revision depth (for
// the audit trail). Tightening trims every subscription's stored
// history to the new depth immediately; widening only affects
// later runs. A depth below 1 is rejected with
// ErrInvalidReportRunHistoryDepth and the live depth is left
// untouched (the returned value is the live depth in that case
// too). Revising to the current depth is a no-op with no audit
// event. The revision is audited as report_runs_retuned.
func (l *Ledger) SetReportRunHistoryDepth(n int) (int, error) {
	return l.SetReportRunHistoryDepthCtx(context.Background(), n)
}

// SetReportRunHistoryDepthCtx is SetReportRunHistoryDepth with
// per-call audit actor attribution (LG-73).
func (l *Ledger) SetReportRunHistoryDepthCtx(ctx context.Context, n int) (int, error) {
	// Check order, along LG-344: validate first, then compare,
	// then swap, trim, and audit — all under l.mu, which also
	// guards the field and the history maps, so the decision, the
	// swap, and the trim are atomic with respect to every sweep
	// and list reader.
	l.mu.Lock()
	defer l.mu.Unlock()
	previous := l.reportRunHistoryDepth
	if n < 1 {
		return previous, fmt.Errorf("%w: got %d", ErrInvalidReportRunHistoryDepth, n)
	}
	if n == previous {
		return previous, nil
	}
	l.reportRunHistoryDepth = n
	if n < previous {
		for id, runs := range l.reportScheduleRuns {
			l.reportScheduleRuns[id] = l.trimReportRunHistoryLocked(runs)
		}
	}
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "report_runs_retuned",
		Actor:         DefaultAuditActor,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"previous_depth": previous,
			"current_depth":  n,
		},
	})
	return previous, nil
}

// ReportRunHistoryDepth reports the live per-schedule run history
// depth: how many of the newest runs ListReportRuns keeps per
// subscription. The name is shared with the default constant,
// which is the construction-time default (100) and the seed — the
// method reads the live value.
func (l *Ledger) ReportRunHistoryDepth() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.reportRunHistoryDepth
}

// trimReportRunHistoryLocked trims one schedule's stored history to
// the live depth, oldest first. Callers must hold the write lock.
// (Kept as a named helper so the trim rule lives in one place: the
// setter's immediate trim and recordReportRunLocked's append-time
// eviction share it.)
func (l *Ledger) trimReportRunHistoryLocked(runs []ReportRun) []ReportRun {
	if len(runs) > l.reportRunHistoryDepth {
		return runs[len(runs)-l.reportRunHistoryDepth:]
	}
	return runs
}
