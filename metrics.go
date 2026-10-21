// Metrics exposed on GET /metrics, rendered in the Prometheus text
// exposition format using only the standard library. The counters live in
// the HTTP layer: they describe what the API served, not the ledger's
// internal bookkeeping.
package main

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	// StatementQueries counts GET /accounts/{id}/statement requests served.
	StatementQueries atomic.Uint64
	// OFXImports counts POST /ofx/import requests served (all attempts,
	// including duplicates and rejected payloads).
	OFXImports atomic.Uint64
	// CAMT053Imports counts POST /camt053/import requests served (all
	// attempts, including duplicates and rejected payloads; LG-103).
	CAMT053Imports atomic.Uint64
	// Pain001Exports counts GET /exports/pain001 requests served.
	Pain001Exports atomic.Uint64
	// Pain001StatusQueries counts GET /exports/pain001/{msgid}/status
	// requests served (LG-99).
	Pain001StatusQueries atomic.Uint64
	// Pain001Receipts counts POST /exports/pain001/{msgid}/receipts
	// requests served, including rejected batches (LG-99).
	Pain001Receipts atomic.Uint64
	// Pain001Settlements counts POST /exports/pain001/{msgid}/settle
	// reconcile-confirmation requests served (LG-99).
	Pain001Settlements atomic.Uint64
	// VerifyRequests counts GET /entries/verify requests served.
	VerifyRequests atomic.Uint64
	// AnchorsTotal counts POST /entries/anchor requests that signed a
	// chain-head checkpoint.
	AnchorsTotal atomic.Uint64
	// TSAAnchorsTotal counts successful RFC 3161 timestamp anchorings of
	// the audit-chain head (LG-84): manual POST /entries/tsa-anchor plus
	// background worker ticks.
	TSAAnchorsTotal atomic.Uint64
	// TSAAnchorFailuresTotal counts failed TSA anchoring attempts
	// (network, TSA rejection). Failures are degraded, never fatal:
	// each one is also recorded as a `tsa_anchor_failed` audit event.
	TSAAnchorFailuresTotal atomic.Uint64
	// ReconcileRuns counts POST /reconcile requests served.
	ReconcileRuns atomic.Uint64
	// ReconcileExportRuns counts POST /reconcile/export requests served
	// (signed reconcile exports, LG-46).
	ReconcileExportRuns atomic.Uint64
	// LargeTxReportsTotal counts large-transaction (CTR) reports generated
	// by POST /compliance/large-tx-reports (LG-94): only newly generated
	// reports count, regenerations that replay stored reports do not.
	LargeTxReportsTotal atomic.Uint64
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
	// ClosedRejections counts the same set of requests rejected with 403
	// because an account involved was closed (see ledger.CloseAccount):
	// the terminal lifecycle state rejects like the frozen stop, but is
	// counted separately so operators can tell lifecycle closes apart
	// from risk-control freezes.
	ClosedRejections atomic.Uint64
	// AccountClosesTotal counts POST /accounts/{id}/close requests that
	// closed an account (idempotent replays are not counted: they close
	// nothing new).
	AccountClosesTotal atomic.Uint64
	// AccountSuspendsTotal counts POST /accounts/{id}/suspend requests
	// that suspended an account (idempotent replays are not counted:
	// they suspend nothing new). Resumes are not counted here — the
	// counter is monotonic, like AccountClosesTotal.
	AccountSuspendsTotal atomic.Uint64
	// SuspendedRejections counts journal-writing requests rejected with
	// 403 because an account involved was suspended (see
	// ledger.SuspendAccount): the operational pause rejects like the
	// frozen stop, but is counted separately so operators can tell a
	// pause apart from a risk-control freeze.
	SuspendedRejections atomic.Uint64
	// FreezeAutoReleasesTotal counts automatic account unfreezes: timed
	// freezes (see ledger.FreezeUntil) whose deadline passed, released
	// by the write-path lazy check, the background freeze sweeper, or an
	// explicit ledger.ExpireFrozenAccounts call.
	FreezeAutoReleasesTotal atomic.Uint64
	// SettlementDeferredTotal counts transfers deferred to the next
	// settlement window since start (see ledger.SettlementDeferral):
	// transfers posted after their currency's cutoff. Mirrored from
	// the ledgers on every GET /metrics scrape, like the freeze
	// auto-release counter.
	SettlementDeferredTotal atomic.Uint64
	// SettlementNextWindowQueries counts GET /settlement/next-window
	// requests served.
	SettlementNextWindowQueries atomic.Uint64
	// AuditSinkSentTotal counts outbox domain events the audit-sink
	// webhook forwarder delivered (HTTP 2xx). Mirrored from the sink on
	// GET /metrics; zero when the sink is disabled.
	AuditSinkSentTotal atomic.Uint64
	// AuditSinkFailedAttemptsTotal counts failed audit-sink delivery
	// attempts (network errors and non-2xx responses), including the
	// final attempts of dead-lettered events.
	AuditSinkFailedAttemptsTotal atomic.Uint64
	// AuditSinkDeadLettersTotal counts events the audit sink gave up on
	// after exhausting its retries: each was recorded as an
	// audit_sink_dead_letter audit event and the cursor moved past it.
	AuditSinkDeadLettersTotal atomic.Uint64
	// WebhookFailuresTotal counts outbound webhook deliveries that
	// exhausted their retries (LG-102): one event x endpoint pair the
	// dispatcher gave up on. Mirrored from the dispatcher on every GET
	// /metrics scrape; zero when webhooks are disabled. A rising count
	// means a downstream consumer is down or rejecting — alert on it.
	// Delivery failures never block the posting path.
	WebhookFailuresTotal atomic.Uint64
	// APIKeyAuthFailuresTotal counts requests rejected by the API key
	// middleware (LG-130): missing/invalid/expired/revoked credentials
	// and rate-quota denials. Only meaningful when API key enforcement
	// is enabled.
	APIKeyAuthFailuresTotal atomic.Uint64
	// APIKeyCreationsTotal counts API keys issued through the HTTP API
	// (LG-130): creations and rotations both mint a fresh secret.
	APIKeyCreationsTotal atomic.Uint64
	// PIIErasureRequestsTotal counts PII erasure requests filed
	// (LG-131): POST /pii/erase requests that passed validation.
	PIIErasureRequestsTotal atomic.Uint64
	// PIIErasuresTotal counts PII erasures executed after dual-control
	// approval (LG-131).
	PIIErasuresTotal atomic.Uint64
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
	// VelocityLimitRejections counts POST /entries, POST /transfers,
	// and POST /entries/{id}/reverse requests rejected with 422
	// because they would have taken the account's rolling-window
	// count or amount above its configured velocity limit (see
	// ledger.SetVelocityLimit). Orthogonal to the daily outflow
	// limit (UTC-day buckets) and the API-key rate limiter.
	VelocityLimitRejections atomic.Uint64
	// BudgetRejections counts POST /entries, POST /entries/batch,
	// POST /transfers, POST /sweeps, and POST /holds/{id}/capture
	// requests rejected with 422 because they would have taken the
	// account's cumulative spend for the UTC calendar month above its
	// configured monthly spend budget (see ledger.SetAccountBudget).
	BudgetRejections atomic.Uint64
	// KycRejections counts POST /entries, POST /entries/batch, and
	// POST /transfers requests rejected with 422 by the KYC/尽调 gate
	// (see ledger.SetKyc): an expired profile blocks any amount, and an
	// unverified (or pending) account is rejected above the ledger's
	// opt-in KYC threshold. The gate runs after the idempotency replay
	// check, so replayed keys do not count here — a replay returns the
	// original entry instead of failing.
	KycRejections atomic.Uint64
	// DenylistRejections counts POST /entries, POST /entries/batch,
	// POST /transfers, and POST /holds/{id}/capture requests rejected
	// with 422 because a leg of the posting named an account on the
	// sanctions/denylist (see ledger.AddDeniedAccount). Screening runs
	// before the idempotency replay check, so denied replays count here
	// too — the compliance trail must show the attempt.
	DenylistRejections atomic.Uint64
	// AllowlistRejections counts POST /transfers requests rejected with
	// 403 because the payer has a transfer beneficiary allowlist
	// installed and the payee is not on it (see
	// ledger.SetTransferAllowlist). The allowlist gate runs after the
	// idempotency replay check, like the frozen gate, so replayed keys
	// do not count here — a replay returns the original receipt
	// instead of failing.
	AllowlistRejections atomic.Uint64
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
	// SnapshotDiffsTotal counts GET /snapshots/diff requests served
	// successfully (see ledger.DiffSnapshotVersions): read-only version
	// diffs between backup snapshots. Rejected requests (bad versions,
	// unknown versions, unconfigured backup dir) are not counted.
	SnapshotDiffsTotal atomic.Uint64
	// MerkleProofQueries counts GET /accounts/{id}/merkle-proof
	// requests served successfully (see ledger.MerkleProofFor).
	// Rejected requests (unknown account, bad currency) are not
	// counted.
	MerkleProofQueries atomic.Uint64
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
	// PartialCapturesTotal counts POST /holds/{id}/capture/partial
	// requests received (all attempts, including duplicates and
	// rejected payloads).
	PartialCapturesTotal atomic.Uint64
	// ReleasesTotal counts POST /holds/{id}/release requests received.
	ReleasesTotal atomic.Uint64
	// HoldSweeps counts hold-expiry sweeps: POST /holds/expire requests
	// received plus background hold-sweeper ticks (see
	// LEDGER_HOLD_SWEEP_INTERVAL). Both paths run the same ExpireHolds
	// sweep; the counter measures sweep executions, not expired holds.
	HoldSweeps atomic.Uint64
	// TermDepositsTotal counts POST /term-deposits requests received
	// (all attempts, including duplicates and rejected payloads).
	TermDepositsTotal atomic.Uint64
	// TermDepositIdempotencyHits counts POST /term-deposits requests
	// that replayed an already-opened deposit idempotency key and
	// returned the original deposit.
	TermDepositIdempotencyHits atomic.Uint64
	// TermDepositAccrualsTotal counts POST /term-deposits/{id}/accrue
	// requests received (all attempts, including replays and
	// rejections).
	TermDepositAccrualsTotal atomic.Uint64
	// TermDepositMaturitiesTotal counts POST /term-deposits/{id}/mature
	// requests received.
	TermDepositMaturitiesTotal atomic.Uint64
	// TermDepositWithdrawalsTotal counts POST
	// /term-deposits/{id}/withdraw requests received.
	TermDepositWithdrawalsTotal atomic.Uint64
	// TermDepositSweepsTotal counts POST /term-deposits/sweep requests
	// received.
	TermDepositSweepsTotal atomic.Uint64
	// TermDepositRejectionsTotal counts term-deposit-domain requests
	// rejected with 422 for semantic reasons: insufficient available
	// funds on open, accruing past maturity, or maturing/withdrawing a
	// deposit in the wrong state.
	TermDepositRejections atomic.Uint64
	// PayoutsTotal counts POST /payouts requests received (all attempts,
	// including duplicates and rejected payloads).
	PayoutsTotal atomic.Uint64
	// PayoutSettlementsTotal counts payout instructions that reached
	// settled, the terminal success state (see SettlePayout).
	PayoutSettlementsTotal atomic.Uint64
	// PayoutRejections counts payout-domain requests rejected for
	// semantic reasons: maker-checker violations, invalid state
	// transitions, statement mismatches on settle, or Post-gate
	// rejections (frozen/closed/overdraft/daily-limit/period) on the
	// submit and settlement legs.
	PayoutRejections atomic.Uint64
	// CreditLinesTotal counts POST /credit-lines requests received
	// (all attempts, including duplicates and rejected payloads).
	CreditLinesTotal atomic.Uint64
	// CreditInterestAccrualsTotal counts POST
	// /credit-lines/accrue-interest requests received.
	CreditInterestAccrualsTotal atomic.Uint64
	// CreditLimitRejectionsTotal counts postings rejected with 422
	// because they would breach a credit line's floor
	// (ErrCreditLimitExceeded).
	CreditLimitRejections atomic.Uint64
	// ECLExposureRequestsTotal counts POST /ecl/exposures requests
	// received (all attempts, including rejected payloads).
	ECLExposureRequestsTotal atomic.Uint64
	// ECLProvisionRequestsTotal counts POST /ecl/provisions requests
	// received (all attempts, including replays and rejections).
	ECLProvisionRequestsTotal atomic.Uint64
	// ECLProvisionReplaysTotal counts POST /ecl/provisions requests
	// that replayed an already-booked provision entry for the day and
	// returned the original row.
	ECLProvisionReplaysTotal atomic.Uint64
	// ECLProvisionRejectionsTotal counts POST /ecl/provisions requests
	// rejected with 400/404/500 for semantic reasons: no tracked
	// exposure, unconfigured booking accounts (fail-closed), or a dust
	// provision.
	ECLProvisionRejectionsTotal atomic.Uint64
	// ECLProvisionsTotal counts ECL provision journal entries committed
	// via POST /ecl/provisions. It is synced from the ledgers on every
	// GET /metrics scrape, like JournalRuleLegsTotal: idempotent
	// replays are not counted twice.
	ECLProvisionsTotal atomic.Uint64
	// ECLStageMigrationsTotal counts ECL stage migrations recorded. It
	// is synced from the ledgers on every GET /metrics scrape, like
	// ECLProvisionsTotal; the durable record is the
	// ecl_stage_migration audit events.
	ECLStageMigrationsTotal atomic.Uint64
	// SweepsTotal counts POST /sweeps requests received (all attempts,
	// including duplicates and rejected payloads).
	SweepsTotal atomic.Uint64
	// SweepIdempotencyHits counts POST /sweeps requests that replayed an
	// already-posted sweep idempotency key and returned the original
	// receipt.
	SweepIdempotencyHits atomic.Uint64
	// SweepsFXTotal counts POST /sweeps/fx requests received (all
	// attempts, including duplicates and rejected payloads).
	SweepsFXTotal atomic.Uint64
	// SweepFXIdempotencyHits counts POST /sweeps/fx requests that
	// replayed an already-posted cross-currency sweep idempotency key
	// and returned the original receipt.
	SweepFXIdempotencyHits atomic.Uint64
	// MergesTotal counts POST /merges requests received (all attempts,
	// including duplicates and rejected payloads).
	MergesTotal atomic.Uint64
	// MergeIdempotencyHits counts POST /merges requests that replayed an
	// already-posted merge idempotency key and returned the original
	// receipt.
	MergeIdempotencyHits atomic.Uint64
	// SplitsTotal counts POST /accounts/{id}/split requests received
	// (all attempts, including duplicates and rejected payloads).
	SplitsTotal atomic.Uint64
	// SplitIdempotencyHits counts POST /accounts/{id}/split requests
	// that replayed an already-posted split idempotency key and
	// returned the original receipt.
	SplitIdempotencyHits atomic.Uint64
	// ReversalsFullTotal counts full reversal journal entries committed
	// by POST /entries/{id}/reverse (see ledger.ReverseEntry). Only
	// first-time reversals are counted: idempotent replays return the
	// existing reversal and book nothing new, so they are not counted
	// twice. Rejected reversals are counted in the frozen/overdraft/
	// daily-limit/period rejection counters, like the other writers.
	// Rendered as ledger_reversals_total{kind="full"} (see
	// ReversalsPartialTotal for the partial series).
	ReversalsFullTotal atomic.Uint64
	// ReversalsPartialTotal counts partial reversal journal entries
	// committed by POST /entries/{id}/reverse-partial (see
	// ledger.ReverseEntryPartial): one per committed partial reversal,
	// idempotent replays not counted twice. Rendered as
	// ledger_reversals_total{kind="partial"}.
	ReversalsPartialTotal atomic.Uint64
	// DisputesRaisedTotal counts transfer disputes raised by
	// POST /disputes (see ledger.RaiseDispute): one per first-time
	// raise — idempotent replays return the original dispute and book
	// nothing new, so they are not counted twice. Rendered as
	// ledger_disputes_raised_total.
	DisputesRaisedTotal atomic.Uint64
	// DisputesResolvedTotal counts transfer disputes terminally
	// resolved by POST /disputes/{id}/resolve (see
	// ledger.ResolveDispute): upheld and rejected resolutions both
	// count — the uphold decision rides the dispute_resolved audit and
	// outbox events. Rendered as ledger_disputes_resolved_total.
	DisputesResolvedTotal atomic.Uint64
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
	// TransferBatchTotal counts POST /transfers/batch requests received
	// (all attempts, including duplicates and rejected payloads).
	TransferBatchTotal atomic.Uint64
	// TransferBatchIdempotencyHits counts POST /transfers/batch requests
	// that replayed an already-posted transfer-batch idempotency key and
	// returned the original receipt.
	TransferBatchIdempotencyHits atomic.Uint64
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
	// AuditExportTotal counts GET /audit/export requests that built an
	// audit-chain export snapshot (LG-128).
	AuditExportTotal atomic.Uint64
	// AuditExportBreaks counts audit-chain exports whose verification
	// report found a broken chain (reported in the manifest, not as an
	// HTTP error). Any nonzero value is an integrity incident — alert
	// on it.
	AuditExportBreaks atomic.Uint64
	// AuditRetentionDeletionsTotal counts audit-log files deleted by the
	// retention sweep (see LEDGER_AUDIT_RETENTION_DAYS, LG-137): one per
	// deleted file — the plain .jsonl and .gz copies of one logical
	// file count once. Every deletion is also recorded as an
	// audit_retention_deleted event in the trail itself.
	AuditRetentionDeletionsTotal atomic.Uint64
	// BalanceWatchBreaches counts balance-threshold alert events emitted
	// by the ledger (see ledger.SetBalanceWatch). It is synced from the
	// ledger on every GET /metrics scrape, like the audit-log counters,
	// so it stays meaningful even though the events are produced inside
	// the ledger package. The alert is advisory — it never rejects a
	// posting — so this counter measures alert volume, not rejections.
	BalanceWatchBreaches atomic.Uint64
	// BudgetPaceAlertsTotal counts budget pace alerts fired by the
	// ledger (see ledger.BudgetPace): a budget's projected month-end
	// spend exceeded the budget times the configured pace alert ratio.
	// It is synced from the ledger on every GET /metrics scrape, like
	// the audit-log counters, so it stays meaningful even though the
	// alerts are produced inside the ledger package. The alert is
	// advisory — it never rejects a posting — so this counter measures
	// alert volume, not rejections.
	BudgetPaceAlertsTotal atomic.Uint64
	// LowLiquidityAlerts counts low-liquidity alert events emitted by
	// the ledger (see ledger.EvaluateLiquidityAlerts). It is synced from
	// the ledger on every GET /metrics scrape, like the audit-log
	// counters. The alert is advisory — it never rejects a posting —
	// so this counter measures alert volume, not rejections.
	LowLiquidityAlerts atomic.Uint64
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
	// ReviewBatchRequestsTotal counts POST /transfers/reviews/approve
	// requests served (LG-149): the batch endpoint that approves many
	// pending reviews in one operator call. Counted by request; the
	// per-item outcomes are recorded separately (settled items count
	// in ReviewApprovedTotal, recorded quorum/chain votes in
	// QuorumVotesTotal).
	ReviewBatchRequestsTotal atomic.Uint64
	// QuorumVotesTotal counts review-quorum votes (LG-124): every named
	// approve/reject vote cast through the review decision endpoints
	// while a quorum policy is active, by approver identity, vote kind,
	// and outcome. The zero value is ready to use. Exported as
	// ledger_review_quorum_votes_total{approver,vote,outcome}.
	QuorumVotesTotal QuorumVoteCounter
	// ReviewDelegationsTotal counts review vote delegations granted
	// (LG-138): every DelegateVote grant, whether direct, extended, or
	// re-granted after a revoke. It is synced from the ledgers on
	// every GET /metrics scrape — grants happen inside the ledger,
	// not in a handler (see ledger/review_delegation.go). The
	// ledger is the source of truth, so the scrape overwrites (never
	// accumulates) the exposed series; the count is monotonic and
	// survives disaster-recovery snapshots. Exported as
	// ledger_review_delegations_total.
	ReviewDelegationsTotal atomic.Uint64
	// MultisigSubmittedTotal counts multisig operations submitted to
	// the pending_multisig queue (LG-142): first-time submissions
	// only, not idempotent replays. It is synced from the ledgers on
	// every GET /metrics scrape — submissions happen inside the
	// ledger, not in a handler (see ledger/multisig.go). Exported as
	// ledger_multisig_submitted_total.
	MultisigSubmittedTotal atomic.Uint64
	// MultisigApprovedTotal counts multisig operations that reached
	// quorum (LG-142). Synced from the ledgers on every scrape, like
	// MultisigSubmittedTotal. Exported as
	// ledger_multisig_approved_total.
	MultisigApprovedTotal atomic.Uint64
	// MultisigRejectedTotal counts multisig operations killed by a
	// reject vote (LG-142). Synced from the ledgers on every scrape.
	// Exported as ledger_multisig_rejected_total.
	MultisigRejectedTotal atomic.Uint64
	// MultisigExpiredTotal counts multisig operations that lapsed
	// without quorum (LG-142). Synced from the ledgers on every
	// scrape. Exported as ledger_multisig_expired_total.
	MultisigExpiredTotal atomic.Uint64
	// PostingRuleLegsTotal counts derived posting-rule legs committed
	// by the automatic posting rules (LG-132): every derived leg
	// appended to a transfer, capture, or sweep event, by rule name.
	// It is synced from the ledger on every GET /metrics scrape —
	// the legs commit inside the event write paths, not in a handler.
	// Rule names are operator-configured, so the label cardinality
	// is bounded by the rule set. Exported as
	// ledger_posting_rule_legs_total{rule}.
	PostingRuleLegsTotal PostingRuleLegCounter
	// OutboxStreamConnections counts the currently open
	// GET /events/stream live tails (LG-133): incremented when a
	// stream opens, decremented when it closes. Exported as the
	// ledger_outbox_stream_connections gauge.
	OutboxStreamConnections atomic.Int64
	// SettlementRuns counts GET /settlement requests served (all
	// formats: json, csv, sql).
	SettlementRuns atomic.Uint64
	// SettlementAlerts counts settlement mismatch alerts across served
	// settlement reports: the day-end reconciliation breaks an operator
	// works through, not posting rejections.
	SettlementAlerts atomic.Uint64
	// CashFlowRuns counts GET /cash-flow requests served (all formats:
	// json, csv).
	CashFlowRuns atomic.Uint64
	// AgingAlertsTotal counts AR aging overdue alerts across served
	// aging reports (LG-50): accounts whose 61-90 + 90+ day overdue
	// cents exceeded the report's alert threshold.
	AgingAlertsTotal atomic.Uint64
	// DunningEscalationsTotal counts collections escalations (LG-118):
	// every dunning_escalated audit event fired by EscalateDunning. It
	// is synced from the ledger on every GET /metrics scrape, like the
	// aging-alert counter's sibling: the escalations fire inside the
	// ledger package, not in a handler.
	DunningEscalationsTotal atomic.Uint64
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
	// VANAttributionsTotal counts virtual-account attributions applied
	// (see ledger.RegisterVirtualAccount). It is synced from the ledger
	// on every GET /metrics scrape, like JournalRuleLegsTotal: the
	// attributions happen inside Post/Transfer/..., not in a handler.
	VANAttributionsTotal atomic.Uint64
	// PeriodClosesTotal counts POST /periods/{id}/close requests
	// received (all attempts, including rejected ones and plain closes
	// without closing entries).
	PeriodClosesTotal atomic.Uint64
	// PeriodPreflightRunsTotal counts POST /periods/{id}/preflight
	// requests received (all attempts, including rejected ones).
	PeriodPreflightRunsTotal atomic.Uint64
	// PeriodAutoClosesTotal counts period auto-close plans the
	// background sweeper fired (LG-151): executed and failed closes.
	// Failed plans keep their period open; the count records the fire,
	// not the outcome.
	PeriodAutoClosesTotal atomic.Uint64
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
	// FXRateStaleTotal counts fx_rate_stale alerts edge-triggered by the
	// background FX-rate watch (see ledger.StartFXRateWatch): rates that
	// entered the opt-in warning window. It is synced from the ledger on
	// every GET /metrics scrape, like the health-violation counter.
	FXRateStaleTotal atomic.Uint64
	// FXRateExpiredTotal counts fx_rate_expired alerts edge-triggered by
	// the background FX-rate watch: rates whose expiry has passed, so
	// cross-currency transfers for the pair are now rejected with
	// ErrFXRateExpired until the operator installs a fresh rate.
	FXRateExpiredTotal atomic.Uint64
	// FXRateOverriddenTotal counts cross-currency transfers converted
	// at an approved manual rate override (see ledger.SetRateOverride):
	// fx_rate_overridden audit events. It is synced from the ledger on
	// every GET /metrics scrape, like the rate-watch counters.
	FXRateOverriddenTotal atomic.Uint64
	// FXRateRefreshesTotal counts market FX rate quotes installed by the
	// opt-in provider adapter (see ledger.WithFXRateProvider): provider
	// refreshes through the SetFXRateRat write path. It is synced from
	// the ledger on every GET /metrics scrape, like the rate-watch
	// counters.
	FXRateRefreshesTotal atomic.Uint64
	// FXRateMovedTotal counts provider refreshes that changed a pair's
	// price: fx_rate_moved audit events. It is synced from the ledger on
	// every GET /metrics scrape, like the rate-watch counters.
	FXRateMovedTotal atomic.Uint64
	// FXRateProviderFailedTotal counts provider fetch failures:
	// fx_rate_provider_failed audit events. It is synced from the ledger
	// on every GET /metrics scrape, like the rate-watch counters.
	FXRateProviderFailedTotal atomic.Uint64
	// FXRateInconsistencyTotal counts fx_rate_inconsistent alerts
	// edge-triggered by the background FX-consistency watch (see
	// ledger.StartFXConsistencyWatch): directed 3-cycles of the rate
	// table whose exact product deviates from 1 by more than the
	// opt-in basis-point threshold. It is synced from the ledger on
	// every GET /metrics scrape, like the rate-watch counters.
	FXRateInconsistencyTotal atomic.Uint64
	// FXSpreadBookingsTotal counts journaled FX spread legs (see
	// ledger/fx_spread.go, LG-93): the "<transfer ID>/spread" legs that
	// book the dealer's spread to the FX income account. It is synced
	// from the ledger on every GET /metrics scrape, like the
	// rate-watch counters.
	FXSpreadBookingsTotal atomic.Uint64
	// FXSpreadCentsTotal is the total spread cents journaled across all
	// settlement currencies since process start (see
	// ledger/fx_spread.go, LG-93): an operational counter, not an
	// accounting total — currencies are never summed on the books; the
	// per-currency P&L lives in POST /reconcile's fx_spread_pnl section.
	// It is synced from the ledger on every GET /metrics scrape, like
	// the rate-watch counters.
	FXSpreadCentsTotal atomic.Int64
	// BalanceHistorySnapshots counts balance-history snapshot ticks
	// completed by the background recorder (see
	// LEDGER_BALANCE_HISTORY_DIR): one per tick that wrote rows, not per
	// row — it measures that the time series is alive, not its volume.
	BalanceHistorySnapshots atomic.Uint64
	// AliasSetsTotal counts PUT /accounts/{id}/alias requests served
	// successfully (see ledger.SetAlias): bindings, rebinds, and clears
	// alike. Rejected requests (bad alias, conflicting alias) are not
	// counted.
	AliasSetsTotal atomic.Uint64
	// ExternalMappingChangesTotal counts external-account mapping
	// changes (see ledger.SetExternalMapping/RemoveExternalMapping,
	// LG-122): rows declared or updated plus rows removed, across all
	// tenants (see GET /metrics). Idempotent replays of an unchanged
	// row are not counted. It is synced from the ledger on every GET
	// /metrics scrape, like the VAN attribution counter.
	ExternalMappingChangesTotal atomic.Uint64
	// EntryLinkChangesTotal counts declared entry links (see
	// ledger.DeclareEntryLink, LG-123): new declarations only —
	// idempotent replays of an identical link are not counted, across
	// all tenants (see GET /metrics). It is synced from the ledger on
	// every GET /metrics scrape, like ExternalMappingChangesTotal.
	EntryLinkChangesTotal atomic.Uint64
	// MandateDebitsTotal counts posted direct-debit mandate debits (see
	// ledger.DebitWithMandate, LG-125): new debits only — idempotent
	// replays of a debit key are not counted, across all tenants (see
	// GET /metrics). It is synced from the ledger on every GET /metrics
	// scrape, like EntryLinkChangesTotal.
	MandateDebitsTotal atomic.Uint64
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
	writeCounter(&sb, "ledger_statement_queries_total",
		"Total GET /accounts/{id}/statement requests served.", m.StatementQueries.Load())
	writeCounter(&sb, "ledger_ofx_imports_total",
		"Total POST /ofx/import OFX statement pre-posting imports served, including duplicates and rejected payloads.", m.OFXImports.Load())
	writeCounter(&sb, "ledger_camt053_imports_total",
		"Total POST /camt053/import camt.053 statement pre-posting imports served, including duplicates and rejected payloads.", m.CAMT053Imports.Load())
	writeCounter(&sb, "ledger_pain001_exports_total",
		"Total GET /exports/pain001 ISO 20022 pain.001 payment instruction exports served.", m.Pain001Exports.Load())
	writeCounter(&sb, "ledger_pain001_status_queries_total",
		"Total GET /exports/pain001/{msgid}/status payment-instruction status queries served.", m.Pain001StatusQueries.Load())
	writeCounter(&sb, "ledger_pain001_receipts_total",
		"Total POST /exports/pain001/{msgid}/receipts bank-receipt batches served, including rejected batches.", m.Pain001Receipts.Load())
	writeCounter(&sb, "ledger_pain001_settlements_total",
		"Total POST /exports/pain001/{msgid}/settle reconcile-confirmation requests served.", m.Pain001Settlements.Load())
	writeCounter(&sb, "ledger_verify_requests_total",
		"Total GET /entries/verify requests served.", m.VerifyRequests.Load())
	writeCounter(&sb, "ledger_anchors_total",
		"Total POST /entries/anchor requests that signed a chain-head checkpoint.", m.AnchorsTotal.Load())
	writeCounter(&sb, "ledger_tsa_anchors_total",
		"Total successful RFC 3161 timestamp anchorings of the audit-chain head (manual plus background worker).", m.TSAAnchorsTotal.Load())
	writeCounter(&sb, "ledger_tsa_anchor_failures_total",
		"Total failed TSA anchoring attempts (network errors, TSA rejections); each is also a tsa_anchor_failed audit event.", m.TSAAnchorFailuresTotal.Load())
	writeCounter(&sb, "ledger_reconcile_runs_total",
		"Total POST /reconcile requests served.", m.ReconcileRuns.Load())
	writeCounter(&sb, "ledger_reconcile_export_runs_total",
		"Total POST /reconcile/export requests served.", m.ReconcileExportRuns.Load())
	writeCounter(&sb, "ledger_large_tx_reports_total",
		"Total large-transaction (CTR) reports generated by POST /compliance/large-tx-reports; only newly generated reports count, replays do not.",
		m.LargeTxReportsTotal.Load())
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
	writeCounter(&sb, "ledger_closed_rejections_total",
		"Total POST /entries, POST /entries/batch, POST /transfers, and hold requests rejected because an account was closed.",
		m.ClosedRejections.Load())
	writeCounter(&sb, "ledger_account_closes_total",
		"Total POST /accounts/{id}/close requests that closed an account.", m.AccountClosesTotal.Load())
	writeCounter(&sb, "ledger_account_suspends_total",
		"Total POST /accounts/{id}/suspend requests that suspended an account (idempotent replays not counted).", m.AccountSuspendsTotal.Load())
	writeCounter(&sb, "ledger_suspended_rejections_total",
		"Total journal-writing requests rejected with 403 because an account involved was suspended.",
		m.SuspendedRejections.Load())
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
	writeCounter(&sb, "ledger_partial_captures_total",
		"Total POST /holds/{id}/capture/partial requests received.",
		m.PartialCapturesTotal.Load())
	writeCounter(&sb, "ledger_releases_total",
		"Total POST /holds/{id}/release requests received.", m.ReleasesTotal.Load())
	writeCounter(&sb, "ledger_hold_sweeps_total",
		"Total hold-expiry sweeps: POST /holds/expire requests plus background hold-sweeper ticks.", m.HoldSweeps.Load())
	writeCounter(&sb, "ledger_term_deposits_total",
		"Total POST /term-deposits requests received.", m.TermDepositsTotal.Load())
	writeCounter(&sb, "ledger_term_deposit_idempotency_hits_total",
		"Total POST /term-deposits requests that replayed an existing deposit idempotency key.",
		m.TermDepositIdempotencyHits.Load())
	writeCounter(&sb, "ledger_term_deposit_accruals_total",
		"Total POST /term-deposits/{id}/accrue requests received.", m.TermDepositAccrualsTotal.Load())
	writeCounter(&sb, "ledger_term_deposit_maturities_total",
		"Total POST /term-deposits/{id}/mature requests received.", m.TermDepositMaturitiesTotal.Load())
	writeCounter(&sb, "ledger_term_deposit_withdrawals_total",
		"Total POST /term-deposits/{id}/withdraw requests received.", m.TermDepositWithdrawalsTotal.Load())
	writeCounter(&sb, "ledger_term_deposit_sweeps_total",
		"Total POST /term-deposits/sweep requests received.", m.TermDepositSweepsTotal.Load())
	writeCounter(&sb, "ledger_term_deposit_rejections_total",
		"Total term-deposit-domain requests rejected with 422: insufficient available funds, accruing past maturity, or wrong-state maturity/withdrawal.",
		m.TermDepositRejections.Load())
	writeCounter(&sb, "ledger_payouts_total",
		"Total POST /payouts requests received.", m.PayoutsTotal.Load())
	writeCounter(&sb, "ledger_payouts_settled_total",
		"Total payout instructions that reached settled.", m.PayoutSettlementsTotal.Load())
	writeCounter(&sb, "ledger_payouts_rejected_total",
		"Total payout-domain requests rejected: maker-checker violations, invalid transitions, statement mismatches, or Post-gate rejections on the submit/settlement legs.",
		m.PayoutRejections.Load())
	writeCounter(&sb, "ledger_credit_lines_total",
		"Total POST /credit-lines requests received.", m.CreditLinesTotal.Load())
	writeCounter(&sb, "ledger_credit_interest_accruals_total",
		"Total POST /credit-lines/accrue-interest requests received.", m.CreditInterestAccrualsTotal.Load())
	writeCounter(&sb, "ledger_credit_limit_rejections_total",
		"Total postings rejected with 422 because they would breach a credit line's floor.",
		m.CreditLimitRejections.Load())
	writeCounter(&sb, "ledger_ecl_exposure_requests_total",
		"Total POST /ecl/exposures requests received.", m.ECLExposureRequestsTotal.Load())
	writeCounter(&sb, "ledger_ecl_provision_requests_total",
		"Total POST /ecl/provisions requests received.", m.ECLProvisionRequestsTotal.Load())
	writeCounter(&sb, "ledger_ecl_provision_replays_total",
		"Total POST /ecl/provisions requests that replayed an already-booked provision entry for the day.",
		m.ECLProvisionReplaysTotal.Load())
	writeCounter(&sb, "ledger_ecl_provision_rejections_total",
		"Total POST /ecl/provisions requests rejected: no tracked exposure, unconfigured booking accounts, or dust provision.",
		m.ECLProvisionRejectionsTotal.Load())
	writeCounter(&sb, "ledger_ecl_provisions_total",
		"Total ECL provision journal entries committed via POST /ecl/provisions (idempotent replays not counted twice).",
		m.ECLProvisionsTotal.Load())
	writeCounter(&sb, "ledger_ecl_stage_migrations_total",
		"Total ECL stage migrations recorded (ecl_stage_migration audit events).",
		m.ECLStageMigrationsTotal.Load())
	writeCounter(&sb, "ledger_sweeps_total",
		"Total POST /sweeps requests received.", m.SweepsTotal.Load())
	writeCounter(&sb, "ledger_sweep_idempotency_hits_total",
		"Total POST /sweeps requests that replayed an existing sweep idempotency key.",
		m.SweepIdempotencyHits.Load())
	writeCounter(&sb, "ledger_sweep_fx_total",
		"Total POST /sweeps/fx requests received.", m.SweepsFXTotal.Load())
	writeCounter(&sb, "ledger_sweep_fx_idempotency_hits_total",
		"Total POST /sweeps/fx requests that replayed an existing cross-currency sweep idempotency key.",
		m.SweepFXIdempotencyHits.Load())
	writeCounter(&sb, "ledger_merges_total",
		"Total POST /merges requests received.", m.MergesTotal.Load())
	writeCounter(&sb, "ledger_merge_idempotency_hits_total",
		"Total POST /merges requests that replayed an existing merge idempotency key.",
		m.MergeIdempotencyHits.Load())
	writeCounter(&sb, "ledger_splits_total",
		"Total POST /accounts/{id}/split requests received.", m.SplitsTotal.Load())
	writeCounter(&sb, "ledger_split_idempotency_hits_total",
		"Total POST /accounts/{id}/split requests that replayed an existing split idempotency key.",
		m.SplitIdempotencyHits.Load())
	writeLabeledCounter(&sb, "ledger_reversals_total",
		"Total reversal journal entries committed (idempotent replays not counted twice), split by kind: full reversals via POST /entries/{id}/reverse and partial reversals via POST /entries/{id}/reverse-partial.",
		[]labeledCounterSeries{
			{labels: `kind="full"`, value: m.ReversalsFullTotal.Load()},
			{labels: `kind="partial"`, value: m.ReversalsPartialTotal.Load()},
		})
	writeCounter(&sb, "ledger_disputes_raised_total",
		"Total transfer disputes raised by POST /disputes (idempotent replays not counted twice).",
		m.DisputesRaisedTotal.Load())
	writeCounter(&sb, "ledger_disputes_resolved_total",
		"Total transfer disputes terminally resolved by POST /disputes/{id}/resolve (upheld or rejected).",
		m.DisputesResolvedTotal.Load())
	writeCounter(&sb, "ledger_revaluations_total",
		"Total revaluation journal entries committed by POST /revaluations (one per account/currency whose carrying value moved).",
		m.RevaluationsTotal.Load())
	writeCounter(&sb, "ledger_batch_total",
		"Total POST /entries/batch requests received.", m.BatchTotal.Load())
	writeCounter(&sb, "ledger_batch_idempotency_hits_total",
		"Total POST /entries/batch requests that replayed an existing batch idempotency key.",
		m.BatchIdempotencyHits.Load())
	writeCounter(&sb, "ledger_transfer_batch_total",
		"Total POST /transfers/batch requests received.", m.TransferBatchTotal.Load())
	writeCounter(&sb, "ledger_transfer_batch_idempotency_hits_total",
		"Total POST /transfers/batch requests that replayed an existing transfer-batch idempotency key.",
		m.TransferBatchIdempotencyHits.Load())
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
	writeCounter(&sb, "ledger_audit_export_total",
		"Total GET /audit/export requests that built an audit-chain export snapshot (LG-128).",
		m.AuditExportTotal.Load())
	writeCounter(&sb, "ledger_audit_export_breaks_total",
		"Total audit-chain exports whose verification report found a broken chain.",
		m.AuditExportBreaks.Load())
	writeCounter(&sb, "ledger_audit_retention_deletions_total",
		"Total audit-log files deleted by the retention sweep (see LEDGER_AUDIT_RETENTION_DAYS); each deletion is also an audit_retention_deleted event in the trail.",
		m.AuditRetentionDeletionsTotal.Load())
	writeCounter(&sb, "ledger_balance_watch_breaches_total",
		"Total balance-threshold alert events emitted (advisory; never a rejection).",
		m.BalanceWatchBreaches.Load())
	writeCounter(&sb, "ledger_low_liquidity_alerts_total",
		"Total low-liquidity alert events emitted (advisory; never a rejection).",
		m.LowLiquidityAlerts.Load())
	writeCounter(&sb, "ledger_tenants_total",
		"Number of provisioned tenant ledgers in the process (1 for a single-tenant deployment).",
		m.TenantsTotal.Load())
	writeCounter(&sb, "ledger_overdraft_rejections_total",
		"Total POST /entries, POST /entries/batch, POST /transfers, and capture requests rejected because they would have overdrawn an overdraft-protected account.",
		m.OverdraftRejections.Load())
	writeCounter(&sb, "ledger_kyc_rejections_total",
		"Total POST /entries, POST /entries/batch, and POST /transfers requests rejected by the KYC gate: an expired profile blocks any amount, an unverified account is rejected above the opt-in threshold.",
		m.KycRejections.Load())
	writeCounter(&sb, "ledger_chart_direction_rejections_total",
		"Total POST /entries, POST /entries/batch, and POST /entries/dry-run requests rejected because a leg of the entry booked against the account's registered chart-of-accounts type.",
		m.ChartDirectionRejections.Load())
	writeCounter(&sb, "ledger_daily_limit_rejections_total",
		"Total POST /entries, POST /entries/batch, and POST /transfers requests rejected because they would have taken the account's daily outflow above its configured limit.",
		m.DailyLimitRejections.Load())
	writeCounter(&sb, "ledger_velocity_limit_rejections_total",
		"Total POST /entries, POST /transfers, and POST /entries/{id}/reverse requests rejected because they would have taken the account's rolling-window posting count or moved amount above its configured velocity limit (LG-156).",
		m.VelocityLimitRejections.Load())
	writeCounter(&sb, "ledger_budget_rejections_total",
		"Total POST /entries, POST /entries/batch, POST /transfers, POST /sweeps, and POST /holds/{id}/capture requests rejected because they would have taken the account's UTC-calendar-month cumulative spend above its configured monthly spend budget.",
		m.BudgetRejections.Load())
	writeCounter(&sb, "ledger_budget_pace_alerts_total",
		"Total budget pace alerts fired by ledger.BudgetPace: a budget's projected month-end spend (booked month-to-date spend linearly extrapolated over the elapsed share of the UTC calendar month) exceeded the budget times the configured pace alert ratio. The alert is advisory — it never rejects a posting.",
		m.BudgetPaceAlertsTotal.Load())
	writeCounter(&sb, "ledger_denylist_rejections_total",
		"Total POST /entries, POST /entries/batch, POST /transfers, and POST /holds/{id}/capture requests rejected because a leg of the posting named an account on the sanctions/denylist.",
		m.DenylistRejections.Load())
	writeCounter(&sb, "ledger_allowlist_rejections_total",
		"Total POST /transfers requests rejected with 403 because the payer has a transfer beneficiary allowlist installed and the payee is not on it (see ledger.SetTransferAllowlist).",
		m.AllowlistRejections.Load())
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
	writeCounter(&sb, "ledger_period_preflight_runs_total",
		"Total POST /periods/{id}/preflight requests received, including rejected ones.",
		m.PeriodPreflightRunsTotal.Load())
	writeCounter(&sb, "ledger_period_autocloses_total",
		"Total period auto-close plans fired by the background sweeper (LG-151), executed and failed: failed plans keep their period open.",
		m.PeriodAutoClosesTotal.Load())
	writeCounter(&sb, "ledger_closing_entries_total",
		"Total period-closing journal entries committed by POST /periods/{id}/close?closing=true.",
		m.ClosingEntriesTotal.Load())
	writeCounter(&sb, "ledger_balance_health_violations_total",
		"Total balance-health violations found by the background health watchdog (advisory integrity signal; any nonzero value is an incident).",
		m.BalanceHealthViolations.Load())
	writeCounter(&sb, "ledger_fx_rate_stale_total",
		"Total fx_rate_stale alerts edge-triggered by the background FX-rate watch (rates that entered the opt-in warning window).",
		m.FXRateStaleTotal.Load())
	writeCounter(&sb, "ledger_fx_rate_expired_total",
		"Total fx_rate_expired alerts edge-triggered by the background FX-rate watch (rates whose expiry has passed; transfers for the pair are rejected until a fresh rate is installed).",
		m.FXRateExpiredTotal.Load())
	writeCounter(&sb, "ledger_fx_rate_overridden_total",
		"Total cross-currency transfers converted at an approved manual rate override instead of the market rate (fx_rate_overridden audit events).",
		m.FXRateOverriddenTotal.Load())
	writeCounter(&sb, "ledger_fx_rate_refreshes_total",
		"Total market FX rate quotes installed by the opt-in provider adapter (WithFXRateProvider) through the SetFXRateRat write path.",
		m.FXRateRefreshesTotal.Load())
	writeCounter(&sb, "ledger_fx_rate_moved_total",
		"Total provider refreshes that changed a pair's price (fx_rate_moved audit events).",
		m.FXRateMovedTotal.Load())
	writeCounter(&sb, "ledger_fx_rate_provider_failed_total",
		"Total market FX rate provider fetch failures (fx_rate_provider_failed audit events); the watch worker continues on the next tick.",
		m.FXRateProviderFailedTotal.Load())
	writeCounter(&sb, "ledger_fx_rate_inconsistency_total",
		"Total fx_rate_inconsistent alerts edge-triggered by the background FX-consistency watch (directed 3-cycles of the rate table whose exact product deviates from 1 beyond the opt-in basis-point threshold; advisory only, conversions are never blocked).",
		m.FXRateInconsistencyTotal.Load())
	writeCounter(&sb, "ledger_fx_spread_bookings_total",
		"Total journaled FX spread legs (LG-93): the \"<transfer ID>/spread\" legs that book the dealer's two-sided dealing spread to the FX income account.",
		m.FXSpreadBookingsTotal.Load())
	// The spread-cents counter is non-negative by construction (only
	// positive spreads book), so the uint64 conversion is exact.
	writeCounter(&sb, "ledger_fx_spread_cents_total",
		"Total spread cents journaled across all settlement currencies (LG-93): an operational counter, not an accounting total — the per-currency P&L lives in POST /reconcile's fx_spread_pnl section.",
		uint64(m.FXSpreadCentsTotal.Load()))
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
	writeCounter(&sb, "ledger_snapshot_diffs_total",
		"Total GET /snapshots/diff requests served successfully (read-only version diffs between backup snapshots).",
		m.SnapshotDiffsTotal.Load())
	writeCounter(&sb, "ledger_merkle_proof_queries_total",
		"Total GET /accounts/{id}/merkle-proof requests served successfully (balance Merkle inclusion proofs).",
		m.MerkleProofQueries.Load())
	writeCounter(&sb, "ledger_review_pending_total",
		"Total POST /transfers requests that entered dual-control pending review (first-time freezes only).",
		m.ReviewPendingTotal.Load())
	writeCounter(&sb, "ledger_review_approved_total",
		"Total POST /transfers/{id}/approve requests that settled a pending review.",
		m.ReviewApprovedTotal.Load())
	writeCounter(&sb, "ledger_review_rejected_total",
		"Total transfer reviews rejected or auto-expired: operator rejects, manual expire runs, and background sweeper ticks.",
		m.ReviewRejectedTotal.Load())
	writeCounter(&sb, "ledger_review_batch_requests_total",
		"Total POST /transfers/reviews/approve batch requests served (LG-149): one operator call approving many pending reviews. Settled items also count in ledger_review_approved_total; recorded quorum/chain votes in ledger_review_quorum_votes_total.",
		m.ReviewBatchRequestsTotal.Load())
	writeLabeledCounter(&sb, "ledger_review_quorum_votes_total",
		"Total review-quorum votes cast (LG-124): named approve/reject votes through the review decision endpoints while a quorum policy is active, by approver identity, vote kind (approve/reject), and outcome (recorded/settled/decisive/duplicate).",
		m.QuorumVotesTotal.series())
	writeCounter(&sb, "ledger_review_delegations_total",
		"Total review vote delegations granted (LG-138): every DelegateVote grant on any voting path (review/quorum/chain), including extensions and re-grants after revocation. Synced from the ledger on every scrape; monotonic and snapshot-durable.",
		m.ReviewDelegationsTotal.Load())
	writeCounter(&sb, "ledger_multisig_submitted_total",
		"Total multisig operations submitted to the pending_multisig queue (LG-142): first-time submissions only. Synced from the ledger on every scrape.",
		m.MultisigSubmittedTotal.Load())
	writeCounter(&sb, "ledger_multisig_approved_total",
		"Total multisig operations that reached quorum (LG-142). Synced from the ledger on every scrape.",
		m.MultisigApprovedTotal.Load())
	writeCounter(&sb, "ledger_multisig_rejected_total",
		"Total multisig operations killed by a reject vote (LG-142). Synced from the ledger on every scrape.",
		m.MultisigRejectedTotal.Load())
	writeCounter(&sb, "ledger_multisig_expired_total",
		"Total multisig operations that lapsed without quorum (LG-142). Synced from the ledger on every scrape.",
		m.MultisigExpiredTotal.Load())
	writeLabeledCounter(&sb, "ledger_posting_rule_legs_total",
		"Total derived posting-rule legs committed (LG-132): every derived journal leg an automatic posting rule appended to a transfer, capture, or sweep event, by rule name.",
		m.PostingRuleLegsTotal.series())
	writeGauge(&sb, "ledger_outbox_stream_connections",
		"Current open GET /events/stream live tails (LG-133): SSE connections receiving the domain-event outbox in real time.",
		m.OutboxStreamConnections.Load())
	writeCounter(&sb, "ledger_settlement_runs_total",
		"Total GET /settlement requests served (all formats).",
		m.SettlementRuns.Load())
	writeCounter(&sb, "ledger_settlement_alerts_total",
		"Total settlement mismatch alerts across served settlement reports.",
		m.SettlementAlerts.Load())
	writeCounter(&sb, "ledger_cash_flow_runs_total",
		"Total GET /cash-flow requests served (all formats).",
		m.CashFlowRuns.Load())
	writeCounter(&sb, "ledger_aging_alerts_total",
		"Total AR aging overdue alerts across served aging reports.",
		m.AgingAlertsTotal.Load())
	writeCounter(&sb, "ledger_dunning_escalations_total",
		"Total collections escalations (dunning_escalated audit events).",
		m.DunningEscalationsTotal.Load())
	writeCounter(&sb, "ledger_journal_rule_sets_total",
		"Total PUT /rules requests received, including rejected payloads and illegal rules.",
		m.JournalRuleSetsTotal.Load())
	writeCounter(&sb, "ledger_journal_rule_deletes_total",
		"Total DELETE /rules requests received.", m.JournalRuleDeletesTotal.Load())
	writeCounter(&sb, "ledger_journal_rule_legs_total",
		"Total derived journal legs committed by the opt-in journal rules engine.",
		m.JournalRuleLegsTotal.Load())
	writeCounter(&sb, "ledger_van_attributions_total",
		"Total virtual-account attributions applied: receipts naming a registered VAN booked to its parent.",
		m.VANAttributionsTotal.Load())
	writeCounter(&sb, "ledger_freeze_auto_releases_total",
		"Total automatic account unfreezes: timed freezes whose deadline passed, released by the write-path lazy check, the background freeze sweeper, or an explicit ExpireFrozenAccounts call.",
		m.FreezeAutoReleasesTotal.Load())
	writeCounter(&sb, "ledger_settlement_deferred_total",
		"Total transfers deferred to the next settlement window: transfers posted after their currency's settlement cutoff (see the per-currency cutoff config).",
		m.SettlementDeferredTotal.Load())
	writeCounter(&sb, "ledger_settlement_next_window_queries_total",
		"Total GET /settlement/next-window requests served.",
		m.SettlementNextWindowQueries.Load())
	writeCounter(&sb, "ledger_audit_sink_sent_total",
		"Total outbox domain events the audit-sink webhook forwarder delivered (HTTP 2xx).",
		m.AuditSinkSentTotal.Load())
	writeCounter(&sb, "ledger_audit_sink_failed_attempts_total",
		"Total failed audit-sink webhook delivery attempts (network errors and non-2xx responses), including the final attempts of dead-lettered events.",
		m.AuditSinkFailedAttemptsTotal.Load())
	writeCounter(&sb, "ledger_audit_sink_dead_letter_total",
		"Total events the audit sink gave up on after exhausting retries (each recorded as an audit_sink_dead_letter audit event).",
		m.AuditSinkDeadLettersTotal.Load())
	writeCounter(&sb, "ledger_webhook_failures_total",
		"Total outbound webhook deliveries that exhausted their retries (one event x endpoint pair the dispatcher gave up on). A rising count means a downstream webhook consumer is down or rejecting; delivery failures never block the posting path.",
		m.WebhookFailuresTotal.Load())
	writeCounter(&sb, "ledger_api_key_auth_failures_total",
		"Total requests rejected by the API key middleware (missing/invalid/expired/revoked credentials, insufficient scope, rate-quota denials).",
		m.APIKeyAuthFailuresTotal.Load())
	writeCounter(&sb, "ledger_api_key_creations_total",
		"Total API keys issued through the HTTP API (creations and rotations).",
		m.APIKeyCreationsTotal.Load())
	writeCounter(&sb, "ledger_pii_erasure_requests_total",
		"Total PII erasure requests filed (POST /pii/erase requests that passed validation).",
		m.PIIErasureRequestsTotal.Load())
	writeCounter(&sb, "ledger_pii_erasures_total",
		"Total PII erasures executed after dual-control approval.",
		m.PIIErasuresTotal.Load())
	writeCounter(&sb, "ledger_alias_sets_total",
		"Total PUT /accounts/{id}/alias requests served successfully (alias bindings, rebinds, and clears).",
		m.AliasSetsTotal.Load())
	writeCounter(&sb, "ledger_external_mapping_changes_total",
		"Total external-account mapping changes: rows declared or updated via POST /accounts/{id}/external-mappings and rows removed via DELETE /accounts/{id}/external-mappings. Idempotent replays of an unchanged row are not counted.",
		m.ExternalMappingChangesTotal.Load())
	writeCounter(&sb, "ledger_entry_links_total",
		"Total entry links declared (see ledger.DeclareEntryLink): explicit directed associations between journal entries (reversal_of, adjustment_of, fee_of, refund_of), including the reversal_of links ReverseEntry and ReverseEntryPartial declare automatically. Idempotent replays of an identical link are not counted.",
		m.EntryLinkChangesTotal.Load())
	writeCounter(&sb, "ledger_mandate_debits_total",
		"Total direct-debit mandate debits posted (see ledger.DebitWithMandate): pulls authorized by a mandate and booked through the full Post validation sequence. Idempotent replays of a debit key are not counted.",
		m.MandateDebitsTotal.Load())
	_, _ = w.Write([]byte(sb.String()))
}

func writeCounter(sb *strings.Builder, name, help string, value uint64) {
	sb.WriteString("# HELP " + name + " " + help + "\n")
	sb.WriteString("# TYPE " + name + " counter\n")
	sb.WriteString(name + " " + strconv.FormatUint(value, 10) + "\n")
}

// writeGauge renders one Prometheus gauge metric: a value that goes up
// and down, like the open SSE stream count.
func writeGauge(sb *strings.Builder, name, help string, value int64) {
	sb.WriteString("# HELP " + name + " " + help + "\n")
	sb.WriteString("# TYPE " + name + " gauge\n")
	sb.WriteString(name + " " + strconv.FormatInt(value, 10) + "\n")
}

// QuorumVoteCounter is the per-(approver, vote, outcome) breakdown of
// review-quorum votes (LG-124), rendered as
// ledger_review_quorum_votes_total{approver="..",vote="..",outcome=".."}:
// approver is the voting identity, vote is "approve" or "reject", and
// outcome is "recorded" (the vote advanced a still-pending review),
// "settled" (the approve vote reached the threshold), "decisive" (the
// reject vote closed the review), or "duplicate" (an idempotent replay
// of an earlier vote). Approver identities come from the configured
// quorum policy -- a bounded set -- so the label cardinality stays
// finite. The zero value is ready to use; Add is safe for concurrent
// use.
type QuorumVoteCounter struct {
	mu     sync.Mutex
	counts map[string]uint64
}

// Add records one quorum vote.
func (c *QuorumVoteCounter) Add(approver, vote, outcome string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.counts == nil {
		c.counts = make(map[string]uint64)
	}
	c.counts[approver+"\x00"+vote+"\x00"+outcome]++
}

// series renders the counter as labeled series, sorted by label set for
// a deterministic exposition.
func (c *QuorumVoteCounter) series() []labeledCounterSeries {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]labeledCounterSeries, 0, len(c.counts))
	for k, v := range c.counts {
		parts := strings.Split(k, "\x00")
		out = append(out, labeledCounterSeries{
			labels: fmt.Sprintf(`approver=%q,vote=%q,outcome=%q`, parts[0], parts[1], parts[2]),
			value:  v,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].labels < out[j].labels })
	return out
}

// labeledCounterSeries is one labeled series of a multi-dimensional
// counter: labels is the pre-rendered label set (e.g. `kind="full"` —
// callers pass literal, already-safe label text) and value its count.
type labeledCounterSeries struct {
	labels string
	value  uint64
}

// PostingRuleLegCounter is the per-rule breakdown of derived posting
// rule legs committed (LG-132), rendered as
// ledger_posting_rule_legs_total{rule=".."}: the ledger counts the legs
// at commit time and the HTTP layer mirrors the counts on scrape, so
// Set overwrites (never accumulates) — the ledger is the source of
// truth. The zero value is ready to use; Set is safe for concurrent
// use.
type PostingRuleLegCounter struct {
	mu     sync.Mutex
	counts map[string]uint64
}

// Set records the ledger's authoritative per-rule leg count. Rule
// names are operator config (no '/' allowed), rendered with %q so the
// label value stays valid exposition text.
func (c *PostingRuleLegCounter) Set(rule string, n uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.counts == nil {
		c.counts = make(map[string]uint64)
	}
	c.counts[rule] = n
}

// series renders the counter as labeled series, sorted by rule name
// for a deterministic exposition.
func (c *PostingRuleLegCounter) series() []labeledCounterSeries {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]labeledCounterSeries, 0, len(c.counts))
	for rule, v := range c.counts {
		out = append(out, labeledCounterSeries{
			labels: fmt.Sprintf(`rule=%q`, rule),
			value:  v,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].labels < out[j].labels })
	return out
}

// writeLabeledCounter renders one Prometheus counter metric with one
// series per label set, e.g.:
//
//	# HELP ledger_reversals_total ...
//	# TYPE ledger_reversals_total counter
//	ledger_reversals_total{kind="full"} 3
//	ledger_reversals_total{kind="partial"} 1
func writeLabeledCounter(sb *strings.Builder, name, help string, series []labeledCounterSeries) {
	sb.WriteString("# HELP " + name + " " + help + "\n")
	sb.WriteString("# TYPE " + name + " counter\n")
	for _, s := range series {
		sb.WriteString(name + "{" + s.labels + "} " + strconv.FormatUint(s.value, 10) + "\n")
	}
}
