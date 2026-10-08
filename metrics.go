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
	// FrozenRejections counts POST /entries, POST /transfers, and hold
	// requests rejected with 403 because an account involved was frozen.
	FrozenRejections atomic.Uint64
	// OverdraftRejections counts POST /entries, POST /transfers, and
	// capture requests rejected with 422 because they would have
	// overdrawn an overdraft-protected account.
	OverdraftRejections atomic.Uint64
	// CurrencyRejections counts POST /entries and POST /transfers requests
	// rejected for currency reasons: a malformed currency code (400) or a
	// cross-currency transfer (422).
	CurrencyRejections atomic.Uint64
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
	// HoldSweeps counts POST /holds/expire requests received.
	HoldSweeps atomic.Uint64
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
		"Total POST /holds/expire requests received.", m.HoldSweeps.Load())
	writeCounter(&sb, "ledger_overdraft_rejections_total",
		"Total POST /entries, POST /transfers, and capture requests rejected because they would have overdrawn an overdraft-protected account.",
		m.OverdraftRejections.Load())
	writeCounter(&sb, "ledger_currency_rejections_total",
		"Total POST /entries and POST /transfers requests rejected for currency reasons: malformed currency code or cross-currency transfer.",
		m.CurrencyRejections.Load())
	_, _ = w.Write([]byte(sb.String()))
}

func writeCounter(sb *strings.Builder, name, help string, value uint64) {
	sb.WriteString("# HELP " + name + " " + help + "\n")
	sb.WriteString("# TYPE " + name + " counter\n")
	sb.WriteString(name + " " + strconv.FormatUint(value, 10) + "\n")
}
