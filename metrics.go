// Metrics exposed on GET /metrics, rendered in the Prometheus text
// exposition format using only the standard library. The counters live in
// the HTTP layer: they describe what the API served, not the ledger's
// internal bookkeeping.
package main

import (
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
)

// Metrics holds the process-wide counters served by GET /metrics.
// All fields are safe for concurrent use.
type Metrics struct {
	// PostsTotal counts POST /entries requests received (all attempts,
	// including duplicates and rejected payloads).
	PostsTotal atomic.Uint64
	// IdempotencyHits counts POST /entries requests that replayed an
	// already-posted idempotency key and returned the original entry.
	IdempotencyHits atomic.Uint64
	// BalanceQueries counts GET /accounts/{id}/balance requests served.
	BalanceQueries atomic.Uint64
	// BalanceAtQueries counts GET /accounts/{id}/balance-at requests served.
	BalanceAtQueries atomic.Uint64
	// VerifyRequests counts GET /entries/verify requests served.
	VerifyRequests atomic.Uint64
	// ReconcileRuns counts POST /reconcile requests served.
	ReconcileRuns atomic.Uint64
	// TransfersTotal counts POST /transfers requests received (all attempts,
	// including duplicates and rejected payloads).
	TransfersTotal atomic.Uint64
	// TransferIdempotencyHits counts POST /transfers requests that replayed
	// an already-posted idempotency key and returned the original receipt.
	TransferIdempotencyHits atomic.Uint64
	// TransferFeeCentsTotal counts the total fee cents booked by
	// POST /transfers fee legs (principal + fee entries land together, so
	// the fee is counted once the transfer commits).
	TransferFeeCentsTotal atomic.Uint64
	// FrozenRejections counts POST /entries, POST /transfers, POST /sweeps,
	// POST /merges, and hold requests rejected with 403 because an account
	// involved was frozen.
	FrozenRejections atomic.Uint64
	// OverdraftRejections counts POST /entries, POST /transfers,
	// POST /merges, and capture requests rejected with 422 because they
	// would have overdrawn an overdraft-protected account.
	OverdraftRejections atomic.Uint64
	// DailyLimitRejections counts POST /entries and POST /transfers
	// requests rejected with 422 because they would have taken the
	// account's cumulative outflow for the UTC day above its configured
	// daily outflow limit.
	DailyLimitRejections atomic.Uint64
	// CurrencyRejections counts POST /entries and POST /transfers requests
	// rejected for currency reasons: a malformed currency code (400) or a
	// cross-currency transfer (422).
	CurrencyRejections atomic.Uint64
	// FXTransfersTotal counts POST /transfers requests that attempted a
	// cross-currency transfer (all attempts, including rejected ones).
	FXTransfersTotal atomic.Uint64
	// HoldsTotal counts POST /holds requests received (all attempts,
	// including duplicates and rejected payloads).
	HoldsTotal atomic.Uint64
	// HoldIdempotencyHits counts POST /holds requests that replayed an
	// already-placed hold idempotency key and returned the original hold.
	HoldIdempotencyHits atomic.Uint64
	// HoldRejections counts hold-domain requests rejected with 422 for
	// semantic reasons: insufficient available funds on POST /holds, and
	// capture requests that exceed the held amount or target a hold that
	// is not active (already captured/released) or expired.
	HoldRejections atomic.Uint64
	// CapturesTotal counts POST /holds/{id}/capture requests received
	// (all attempts, including duplicates and rejected payloads).
	CapturesTotal atomic.Uint64
	// CaptureIdempotencyHits counts POST /holds/{id}/capture requests
	// that replayed an already-settled capture idempotency key and
	// returned the original receipt.
	CaptureIdempotencyHits atomic.Uint64
	// ReleasesTotal counts POST /holds/{id}/release requests received.
	ReleasesTotal atomic.Uint64
	// HoldSweeps counts hold-expiry sweeps: POST /holds/expire requests
	// received plus background hold-sweeper ticks (see
	// LEDGER_HOLD_SWEEP_INTERVAL). Both paths run the same ExpireHolds
	// sweep; the counter measures sweep executions, not expired holds.
	HoldSweeps atomic.Uint64
	// SweepsTotal counts POST /sweeps requests received (all attempts,
	// including duplicates and rejected payloads).
	SweepsTotal atomic.Uint64
	// SweepIdempotencyHits counts POST /sweeps requests that replayed an
	// already-posted sweep idempotency key and returned the original
	// receipt.
	SweepIdempotencyHits atomic.Uint64
	// MergesTotal counts POST /merges requests received (all attempts,
	// including duplicates and rejected payloads).
	MergesTotal atomic.Uint64
	// MergeIdempotencyHits counts POST /merges requests that replayed an
	// already-posted merge idempotency key and returned the original
	// receipt.
	MergeIdempotencyHits atomic.Uint64
	// AuditEventsTotal counts audit-log events written to disk. It is
	// synced from the ledger's audit log on every GET /metrics scrape,
	// so it stays meaningful even though the events are produced inside
	// the ledger package.
	AuditEventsTotal atomic.Uint64
	// AuditDroppedTotal counts audit-log events dropped because the async
	// queue was full or the log was closed. A growing value means the
	// disk cannot keep up — alert on it.
	AuditDroppedTotal atomic.Uint64
	// AuditVerifyTotal counts GET /audit/verify requests that executed
	// a full hash-chain verification of the structured audit log.
	AuditVerifyTotal atomic.Uint64
	// AuditVerifyBreaks counts audit-log hash-chain verifications that
	// found a broken chain (the first break is reported in the
	// response body). Any nonzero value is an integrity incident —
	// alert on it.
	AuditVerifyBreaks atomic.Uint64
}

// handleMetrics implements GET /metrics. It emits the counters in the
// Prometheus text exposition format:
//
//	# HELP <name> <help>
//	# TYPE <name> counter
//	<name> <value>
//
// Integer counters are always rendered without a decimal point, as the
// exposition format expects.
func (m *Metrics) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	var sb strings.Builder
	writeCounter(&sb, "ledger_posts_total",
		"Total POST /entries requests received.", m.PostsTotal.Load())
	writeCounter(&sb, "ledger_idempotency_hits_total",
		"Total POST /entries requests that replayed an existing idempotency key.",
		m.IdempotencyHits.Load())
	writeCounter(&sb, "ledger_balance_queries_total",
		"Total GET /accounts/{id}/balance requests served.", m.BalanceQueries.Load())
	writeCounter(&sb, "ledger_balance_at_queries_total",
		"Total GET /accounts/{id}/balance-at requests served.", m.BalanceAtQueries.Load())
	writeCounter(&sb, "ledger_verify_requests_total",
		"Total GET /entries/verify requests served.", m.VerifyRequests.Load())
	writeCounter(&sb, "ledger_reconcile_runs_total",
		"Total POST /reconcile requests served.", m.ReconcileRuns.Load())
	writeCounter(&sb, "ledger_transfers_total",
		"Total POST /transfers requests received.", m.TransfersTotal.Load())
	writeCounter(&sb, "ledger_transfer_idempotency_hits_total",
		"Total POST /transfers requests that replayed an existing idempotency key.",
		m.TransferIdempotencyHits.Load())
	writeCounter(&sb, "ledger_transfer_fee_cents_total",
		"Total fee cents booked by POST /transfers fee legs.",
		m.TransferFeeCentsTotal.Load())
	writeCounter(&sb, "ledger_frozen_rejections_total",
		"Total POST /entries, POST /transfers, and hold requests rejected because an account was frozen.",
		m.FrozenRejections.Load())
	writeCounter(&sb, "ledger_holds_total",
		"Total POST /holds requests received.", m.HoldsTotal.Load())
	writeCounter(&sb, "ledger_hold_idempotency_hits_total",
		"Total POST /holds requests that replayed an existing hold idempotency key.",
		m.HoldIdempotencyHits.Load())
	writeCounter(&sb, "ledger_hold_rejections_total",
		"Total hold-domain requests rejected with 422: insufficient available funds, capture exceeding the held amount, or capture on a non-active or expired hold.",
		m.HoldRejections.Load())
	writeCounter(&sb, "ledger_captures_total",
		"Total POST /holds/{id}/capture requests received.", m.CapturesTotal.Load())
	writeCounter(&sb, "ledger_capture_idempotency_hits_total",
		"Total POST /holds/{id}/capture requests that replayed an existing capture idempotency key.",
		m.CaptureIdempotencyHits.Load())
	writeCounter(&sb, "ledger_releases_total",
		"Total POST /holds/{id}/release requests received.", m.ReleasesTotal.Load())
	writeCounter(&sb, "ledger_hold_sweeps_total",
		"Total hold-expiry sweeps: POST /holds/expire requests plus background hold-sweeper ticks.", m.HoldSweeps.Load())
	writeCounter(&sb, "ledger_sweeps_total",
		"Total POST /sweeps requests received.", m.SweepsTotal.Load())
	writeCounter(&sb, "ledger_sweep_idempotency_hits_total",
		"Total POST /sweeps requests that replayed an existing sweep idempotency key.",
		m.SweepIdempotencyHits.Load())
	writeCounter(&sb, "ledger_merges_total",
		"Total POST /merges requests received.", m.MergesTotal.Load())
	writeCounter(&sb, "ledger_merge_idempotency_hits_total",
		"Total POST /merges requests that replayed an existing merge idempotency key.",
		m.MergeIdempotencyHits.Load())
	writeCounter(&sb, "ledger_audit_events_total",
		"Total audit-log events written to disk.", m.AuditEventsTotal.Load())
	writeCounter(&sb, "ledger_audit_dropped_total",
		"Total audit-log events dropped because the async queue was full or the log was closed.",
		m.AuditDroppedTotal.Load())
	writeCounter(&sb, "ledger_audit_verify_total",
		"Total GET /audit/verify requests that executed a full hash-chain verification of the structured audit log.",
		m.AuditVerifyTotal.Load())
	writeCounter(&sb, "ledger_audit_verify_breaks_total",
		"Total audit-log hash-chain verifications that found a broken chain.",
		m.AuditVerifyBreaks.Load())
	writeCounter(&sb, "ledger_overdraft_rejections_total",
		"Total POST /entries, POST /transfers, and capture requests rejected because they would have overdrawn an overdraft-protected account.",
		m.OverdraftRejections.Load())
	writeCounter(&sb, "ledger_daily_limit_rejections_total",
		"Total POST /entries and POST /transfers requests rejected because they would have taken the account's daily outflow above its configured limit.",
		m.DailyLimitRejections.Load())
	writeCounter(&sb, "ledger_currency_rejections_total",
		"Total POST /entries and POST /transfers requests rejected for currency reasons: malformed currency code or cross-currency transfer.",
		m.CurrencyRejections.Load())
	writeCounter(&sb, "ledger_fx_transfers_total",
		"Total POST /transfers requests that attempted a cross-currency transfer, including rejected ones.",
		m.FXTransfersTotal.Load())
	_, _ = w.Write([]byte(sb.String()))
}

func writeCounter(sb *strings.Builder, name, help string, value uint64) {
	sb.WriteString("# HELP " + name + " " + help + "\n")
	sb.WriteString("# TYPE " + name + " counter\n")
	sb.WriteString(name + " " + strconv.FormatUint(value, 10) + "\n")
}
