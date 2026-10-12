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
	// OFXExports counts GET /accounts/{id}/ofx requests served.
	OFXExports atomic.Uint64
	// VerifyRequests counts GET /entries/verify requests served.
	VerifyRequests atomic.Uint64
	// AnchorsTotal counts POST /entries/anchor requests that signed a
	// chain-head checkpoint.
	AnchorsTotal atomic.Uint64
	// ReconcileRuns counts POST /reconcile requests served.
	ReconcileRuns atomic.Uint64
	// ReconcileExportRuns counts POST /reconcile/export requests served
	// (signed reconcile exports, LG-46).
	ReconcileExportRuns atomic.Uint64
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
	// ScheduledTransfersTotal counts scheduled-transfer runs the sweeper
	// fired successfully (see StartScheduleSweeper): each run that posted
	// through PostTransfer, including duplicate replays after a crash.
	// Failed runs are recorded in the audit log, not here.
	ScheduledTransfersTotal atomic.Uint64
	// FrozenRejections counts POST /entries, POST /entries/batch,
	// POST /transfers, POST /sweeps, POST /merges, and hold requests
	// rejected with 403 because an account involved was frozen.
	FrozenRejections atomic.Uint64
	// FreezeAutoReleasesTotal counts automatic account unfreezes: timed
	// freezes (see ledger.FreezeUntil) whose deadline passed, released
	// by the write-path lazy check, the background freeze sweeper, or an
	// explicit ledger.ExpireFrozenAccounts call.
	FreezeAutoReleasesTotal atomic.Uint64
	// OverdraftRejections counts POST /entries, POST /entries/batch,
	// POST /transfers, POST /merges, and capture requests rejected with
	// 422 because they would have overdrawn an overdraft-protected
	// account.
	OverdraftRejections atomic.Uint64
	// ChartDirectionRejections counts POST /entries, POST /entries/batch,
	// and POST /entries/dry-run requests rejected with 422 because a
	// leg of the entry booked against the account's registered
	// chart-of-accounts type (see ledger.RegisterAccountType).
	ChartDirectionRejections atomic.Uint64
	// DailyLimitRejections counts POST /entries, POST /entries/batch,
	// and POST /transfers requests rejected with 422 because they would
	// have taken the account's cumulative outflow for the UTC day above
	// its configured daily outflow limit.
	DailyLimitRejections atomic.Uint64
	// CurrencyRejections counts POST /entries, POST /entries/batch, and
	// POST /transfers requests rejected for currency reasons: a malformed
	// currency code (400), a cross-currency transfer (422), or an expired
	// FX rate (422).
	CurrencyRejections atomic.Uint64
	// FXTransfersTotal counts POST /transfers requests that attempted a
	// cross-currency transfer (all attempts, including rejected ones).
	FXTransfersTotal atomic.Uint64
	// PeriodRejections counts journal-writing requests rejected with 422
	// because the entry's timestamp fell in a closed accounting period:
	// POST /entries, POST /entries/batch, POST /transfers, POST /sweeps,
	// POST /merges, and POST /holds/{id}/capture.
	PeriodRejections atomic.Uint64
	// DryRunRequests counts POST /entries/dry-run, POST /transfers/dry-run,
	// and POST /sweeps/dry-run requests received (all attempts, including
	// would-be rejections). A dry run records nothing by design, so there
	// is no separate "committed" counter.
	DryRunRequests atomic.Uint64
	// SnapshotBackupsTotal counts successful periodic snapshot backup
	// ticks (full and incremental).
	SnapshotBackupsTotal atomic.Uint64
	// SnapshotBackupsFailed counts periodic snapshot backup ticks that
	// failed after retries.
	SnapshotBackupsFailed atomic.Uint64
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
	// ReversalsTotal counts reversal journal entries committed by
	// POST /entries/{id}/reverse (see ledger.ReverseEntry). Only
	// first-time reversals are counted: idempotent replays return the
	// existing reversal and book nothing new, so they are not counted
	// twice. Rejected reversals are counted in the frozen/overdraft/
	// daily-limit/period rejection counters, like the other writers.
	ReversalsTotal atomic.Uint64
	// RevaluationsTotal counts revaluation journal entries committed by
	// POST /revaluations (see ledger.RevalueBalances): one per
	// (account, currency) whose base-currency carrying value moved. Runs
	// that book nothing (all unchanged/skipped/failed) move it by zero.
	RevaluationsTotal atomic.Uint64
	// BatchTotal counts POST /entries/batch requests received (all attempts,
	// including duplicates and rejected payloads).
	BatchTotal atomic.Uint64
	// BatchIdempotencyHits counts POST /entries/batch requests that replayed
	// an already-posted batch idempotency key and returned the original
	// receipt.
	BatchIdempotencyHits atomic.Uint64
	// AuditEventsTotal counts audit-log events written to disk. It is
	// synced from the ledger's audit log on every GET /metrics scrape,
	// so it stays meaningful even though the events are produced inside
	// the ledger package.
	AuditEventsTotal atomic.Uint64
	// AuditDroppedTotal counts audit-log events dropped because the async
	// queue was full or the log was closed. A growing value means the
	// disk cannot keep up — alert on it.
	AuditDroppedTotal atomic.Uint64
	// OutboxEventsTotal counts domain events durably written to the
	// outbox (see ledger.NewOutbox). It is synced from the ledger on
	// every GET /metrics scrape, like the audit-log counters, so it
	// stays meaningful even though the events are produced inside the
	// ledger package.
	OutboxEventsTotal atomic.Uint64
	// OutboxDroppedTotal counts outbox events dropped because the outbox
	// was closed or a disk write/fsync failed. Unlike the audit log's
	// drops, an outbox drop means a committed operation has no domain
	// event in the stream — a gap downstream consumers can detect via
	// seq. Any nonzero growth is an incident — alert on it.
	OutboxDroppedTotal atomic.Uint64
	// AuditVerifyTotal counts GET /audit/verify requests that executed
	// a full hash-chain verification of the structured audit log.
	AuditVerifyTotal atomic.Uint64
	// AuditVerifyBreaks counts audit-log hash-chain verifications that
	// found a broken chain (the first break is reported in the
	// response body). Any nonzero value is an integrity incident —
	// alert on it.
	AuditVerifyBreaks atomic.Uint64
	// BalanceWatchBreaches counts balance-threshold alert events emitted
	// by the ledger (see ledger.SetBalanceWatch). It is synced from the
	// ledger on every GET /metrics scrape, like the audit-log counters,
	// so it stays meaningful even though the events are produced inside
	// the ledger package. The alert is advisory — it never rejects a
	// posting — so this counter measures alert volume, not rejections.
	BalanceWatchBreaches atomic.Uint64
	// TenantsTotal reports how many tenant ledgers are provisioned in
	// the process (see ledger.TenantStore), synced on every GET /metrics
	// scrape. A single-tenant deployment reports 1 (the default tenant).
	TenantsTotal atomic.Uint64
	// ReviewPendingTotal counts POST /transfers requests that entered
	// dual-control pending review (see ledger.PostTransfer): first-time
	// freezes only, not idempotent replays of the review receipt.
	ReviewPendingTotal atomic.Uint64
	// ReviewApprovedTotal counts POST /transfers/{id}/approve requests
	// that settled a pending review. Idempotent re-approvals (which
	// return the original receipt and settle nothing new) are not
	// counted again.
	ReviewApprovedTotal atomic.Uint64
	// ReviewRejectedTotal counts review rejections: POST
	// /transfers/{id}/reject requests that rejected a pending review,
	// POST /transfers/reviews/expire runs, and background
	// review-sweeper ticks, by number of reviews rejected/expired (not
	// by request).
	ReviewRejectedTotal atomic.Uint64
	// SettlementRuns counts GET /settlement requests served (all
	// formats: json, csv, sql).
	SettlementRuns atomic.Uint64
	// SettlementAlerts counts settlement mismatch alerts across served
	// settlement reports: the day-end reconciliation breaks an operator
	// works through, not posting rejections.
	SettlementAlerts atomic.Uint64
	// AgingAlertsTotal counts AR aging overdue alerts across served
	// aging reports (LG-50): accounts whose 61-90 + 90+ day overdue
	// cents exceeded the report's alert threshold.
	AgingAlertsTotal atomic.Uint64
	// JournalRuleSetsTotal counts PUT /rules requests received (all
	// attempts, including rejected payloads and illegal rules).
	JournalRuleSetsTotal atomic.Uint64
	// JournalRuleDeletesTotal counts DELETE /rules requests received.
	JournalRuleDeletesTotal atomic.Uint64
	// JournalRuleLegsTotal counts derived journal legs committed by the
	// opt-in journal rules engine (see ledger.SetJournalRule). It is
	// synced from the ledger on every GET /metrics scrape, like the
	// audit-log counters, so it stays meaningful even though the legs
	// are committed inside the ledger package.
	JournalRuleLegsTotal atomic.Uint64
	// PeriodClosesTotal counts POST /periods/{id}/close requests
	// received (all attempts, including rejected ones and plain closes
	// without closing entries).
	PeriodClosesTotal atomic.Uint64
	// ClosingEntriesTotal counts period-closing journal entries
	// committed by POST /periods/{id}/close?closing=true (see
	// ledger.ClosePeriodWithClosing).
	ClosingEntriesTotal atomic.Uint64
	// BalanceHealthViolations counts balance-health violations found by
	// the background health watchdog (see ledger.StartHealthWatch). It
	// is synced from the ledger on every GET /metrics scrape, like the
	// low-balance counter, so it stays meaningful even though the
	// violations are detected inside the ledger package. Any nonzero
	// value is an integrity incident — alert on it.
	BalanceHealthViolations atomic.Uint64
	// BalanceHistorySnapshots counts balance-history snapshot ticks
	// completed by the background recorder (see
	// LEDGER_BALANCE_HISTORY_DIR): one per tick that wrote rows, not per
	// row — it measures that the time series is alive, not its volume.
	BalanceHistorySnapshots atomic.Uint64
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
	writeCounter(&sb, "ledger_ofx_exports_total",
		"Total GET /accounts/{id}/ofx OFX statement exports served.", m.OFXExports.Load())
	writeCounter(&sb, "ledger_verify_requests_total",
		"Total GET /entries/verify requests served.", m.VerifyRequests.Load())
	writeCounter(&sb, "ledger_anchors_total",
		"Total POST /entries/anchor requests that signed a chain-head checkpoint.", m.AnchorsTotal.Load())
	writeCounter(&sb, "ledger_reconcile_runs_total",
		"Total POST /reconcile requests served.", m.ReconcileRuns.Load())
	writeCounter(&sb, "ledger_reconcile_export_runs_total",
		"Total POST /reconcile/export requests served.", m.ReconcileExportRuns.Load())
	writeCounter(&sb, "ledger_transfers_total",
		"Total POST /transfers requests received.", m.TransfersTotal.Load())
	writeCounter(&sb, "ledger_transfer_idempotency_hits_total",
		"Total POST /transfers requests that replayed an existing idempotency key.",
		m.TransferIdempotencyHits.Load())
	writeCounter(&sb, "ledger_transfer_fee_cents_total",
		"Total fee cents booked by POST /transfers fee legs.",
		m.TransferFeeCentsTotal.Load())
	writeCounter(&sb, "ledger_scheduled_transfers_total",
		"Total scheduled-transfer runs fired by the sweeper.",
		m.ScheduledTransfersTotal.Load())
	writeCounter(&sb, "ledger_frozen_rejections_total",
		"Total POST /entries, POST /entries/batch, POST /transfers, and hold requests rejected because an account was frozen.",
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
	writeCounter(&sb, "ledger_reversals_total",
		"Total reversal journal entries committed by POST /entries/{id}/reverse (idempotent replays not counted twice).",
		m.ReversalsTotal.Load())
	writeCounter(&sb, "ledger_revaluations_total",
		"Total revaluation journal entries committed by POST /revaluations (one per account/currency whose carrying value moved).",
		m.RevaluationsTotal.Load())
	writeCounter(&sb, "ledger_batch_total",
		"Total POST /entries/batch requests received.", m.BatchTotal.Load())
	writeCounter(&sb, "ledger_batch_idempotency_hits_total",
		"Total POST /entries/batch requests that replayed an existing batch idempotency key.",
		m.BatchIdempotencyHits.Load())
	writeCounter(&sb, "ledger_audit_events_total",
		"Total audit-log events written to disk.", m.AuditEventsTotal.Load())
	writeCounter(&sb, "ledger_audit_dropped_total",
		"Total audit-log events dropped because the async queue was full or the log was closed.",
		m.AuditDroppedTotal.Load())
	writeCounter(&sb, "ledger_outbox_events_total",
		"Total domain events durably written to the outbox (LG-53).",
		m.OutboxEventsTotal.Load())
	writeCounter(&sb, "ledger_outbox_dropped_total",
		"Total outbox events dropped because the outbox was closed or a disk write/fsync failed (a stream gap — alert on growth).",
		m.OutboxDroppedTotal.Load())
	writeCounter(&sb, "ledger_audit_verify_total",
		"Total GET /audit/verify requests that executed a full hash-chain verification of the structured audit log.",
		m.AuditVerifyTotal.Load())
	writeCounter(&sb, "ledger_audit_verify_breaks_total",
		"Total audit-log hash-chain verifications that found a broken chain.",
		m.AuditVerifyBreaks.Load())
	writeCounter(&sb, "ledger_balance_watch_breaches_total",
		"Total balance-threshold alert events emitted (advisory; never a rejection).",
		m.BalanceWatchBreaches.Load())
	writeCounter(&sb, "ledger_tenants_total",
		"Number of provisioned tenant ledgers in the process (1 for a single-tenant deployment).",
		m.TenantsTotal.Load())
	writeCounter(&sb, "ledger_overdraft_rejections_total",
		"Total POST /entries, POST /entries/batch, POST /transfers, and capture requests rejected because they would have overdrawn an overdraft-protected account.",
		m.OverdraftRejections.Load())
	writeCounter(&sb, "ledger_chart_direction_rejections_total",
		"Total POST /entries, POST /entries/batch, and POST /entries/dry-run requests rejected because a leg of the entry booked against the account's registered chart-of-accounts type.",
		m.ChartDirectionRejections.Load())
	writeCounter(&sb, "ledger_daily_limit_rejections_total",
		"Total POST /entries, POST /entries/batch, and POST /transfers requests rejected because they would have taken the account's daily outflow above its configured limit.",
		m.DailyLimitRejections.Load())
	writeCounter(&sb, "ledger_currency_rejections_total",
		"Total POST /entries, POST /entries/batch, and POST /transfers requests rejected for currency reasons: malformed currency code, cross-currency transfer, or expired FX rate.",
		m.CurrencyRejections.Load())
	writeCounter(&sb, "ledger_fx_transfers_total",
		"Total POST /transfers requests that attempted a cross-currency transfer, including rejected ones.",
		m.FXTransfersTotal.Load())
	writeCounter(&sb, "ledger_period_rejections_total",
		"Total journal-writing requests rejected because the entry's timestamp fell in a closed accounting period: POST /entries, POST /entries/batch, POST /transfers, POST /sweeps, POST /merges, and POST /holds/{id}/capture.",
		m.PeriodRejections.Load())
	writeCounter(&sb, "ledger_period_closes_total",
		"Total POST /periods/{id}/close requests received, including rejected ones and plain closes without closing entries.",
		m.PeriodClosesTotal.Load())
	writeCounter(&sb, "ledger_closing_entries_total",
		"Total period-closing journal entries committed by POST /periods/{id}/close?closing=true.",
		m.ClosingEntriesTotal.Load())
	writeCounter(&sb, "ledger_balance_health_violations_total",
		"Total balance-health violations found by the background health watchdog (advisory integrity signal; any nonzero value is an incident).",
		m.BalanceHealthViolations.Load())
	writeCounter(&sb, "ledger_balance_history_snapshots_total",
		"Total balance-history snapshot ticks completed by the background recorder (one per tick, not per row).",
		m.BalanceHistorySnapshots.Load())
	writeCounter(&sb, "ledger_dry_runs_total",
		"Total what-if dry-run requests received (POST /entries/dry-run, POST /transfers/dry-run, POST /sweeps/dry-run), including would-be rejections.",
		m.DryRunRequests.Load())
	writeCounter(&sb, "ledger_snapshot_backups_total",
		"Total successful periodic snapshot backup ticks (full and incremental).",
		m.SnapshotBackupsTotal.Load())
	writeCounter(&sb, "ledger_snapshot_backups_failed_total",
		"Total periodic snapshot backup ticks that failed after retries.",
		m.SnapshotBackupsFailed.Load())
	writeCounter(&sb, "ledger_review_pending_total",
		"Total POST /transfers requests that entered dual-control pending review (first-time freezes only).",
		m.ReviewPendingTotal.Load())
	writeCounter(&sb, "ledger_review_approved_total",
		"Total POST /transfers/{id}/approve requests that settled a pending review.",
		m.ReviewApprovedTotal.Load())
	writeCounter(&sb, "ledger_review_rejected_total",
		"Total transfer reviews rejected or auto-expired: operator rejects, manual expire runs, and background sweeper ticks.",
		m.ReviewRejectedTotal.Load())
	writeCounter(&sb, "ledger_settlement_runs_total",
		"Total GET /settlement requests served (all formats).",
		m.SettlementRuns.Load())
	writeCounter(&sb, "ledger_settlement_alerts_total",
		"Total settlement mismatch alerts across served settlement reports.",
		m.SettlementAlerts.Load())
	writeCounter(&sb, "ledger_aging_alerts_total",
		"Total AR aging overdue alerts across served aging reports.",
		m.AgingAlertsTotal.Load())
	writeCounter(&sb, "ledger_journal_rule_sets_total",
		"Total PUT /rules requests received, including rejected payloads and illegal rules.",
		m.JournalRuleSetsTotal.Load())
	writeCounter(&sb, "ledger_journal_rule_deletes_total",
		"Total DELETE /rules requests received.", m.JournalRuleDeletesTotal.Load())
	writeCounter(&sb, "ledger_journal_rule_legs_total",
		"Total derived journal legs committed by the opt-in journal rules engine.",
		m.JournalRuleLegsTotal.Load())
	writeCounter(&sb, "ledger_freeze_auto_releases_total",
		"Total automatic account unfreezes: timed freezes whose deadline passed, released by the write-path lazy check, the background freeze sweeper, or an explicit ExpireFrozenAccounts call.",
		m.FreezeAutoReleasesTotal.Load())
	_, _ = w.Write([]byte(sb.String()))
}

func writeCounter(sb *strings.Builder, name, help string, value uint64) {
	sb.WriteString("# HELP " + name + " " + help + "\n")
	sb.WriteString("# TYPE " + name + " counter\n")
	sb.WriteString(name + " " + strconv.FormatUint(value, 10) + "\n")
}
