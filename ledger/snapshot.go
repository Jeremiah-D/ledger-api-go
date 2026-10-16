package ledger

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"time"
)

// Disaster-recovery snapshots.
//
// A snapshot is a JSONL document: one JSON object per line, each carrying a
// "record" discriminator. The layout is append-ordered and deterministic so
// two exports of the same ledger state are byte-identical:
//
//	{"record":"meta", ...}            exactly one, always the first line
//	{"record":"entry", ...}           journal entries, in audit-chain (Post) order
//	{"record":"link", ...}            audit-chain links, seq 1..N, matching entry order
//	{"record":"hold", ...}            authorization holds, by (CreatedAt, ID)
//	{"record":"review", ...}          transfer dual-control reviews, by transfer ID
//	{"record":"idempotency", ...}     the seven idempotency-key namespaces
//	{"record":"config", ...}          operational config (frozen, overdraft, hierarchy, fees, TTL, daily limits)
//
// The export carries only source-of-truth rows: journal entries, chain
// links, holds, idempotency registries, and operational config. Derived
// state (balances, debit/credit totals, per-account indexes) is recomputed
// on import by folding the journal in chain order, which also cross-checks
// the folding logic against live Post behavior. Import finishes with a
// full VerifyChain: any tampered amount, rewritten entry, spliced link, or
// reordered journal rejects the import with an error — a snapshot that
// cannot prove its own integrity is refused, never half-restored.
//
// A ledger restored by ImportSnapshot is a fully working ledger: Post,
// Reconcile, BalanceAt, and every other read keep working, and Reconcile
// can be rerun against the restored copy (the end-of-day report is how
// operators confirm a restored backup before promoting it).
//
// Timestamps round-trip through RFC 3339 with nanosecond precision, which
// preserves CreatedAt.UnixNano() exactly — the value the audit-chain hash
// is computed over. A snapshot must be exported and imported with the same
// chain-hash semantics; snapshotFormatVersion guards that.

// snapshotFormatVersion pins the export layout and the chain-hash input
// encoding it is verified against. Bump it if either ever changes.
const snapshotFormatVersion = 1

// ErrSnapshotInvalid is returned by ImportSnapshot when the snapshot cannot
// be trusted: malformed JSONL, a missing or duplicated section, dangling
// registry references, or an audit chain that fails verification. The
// error wraps details about the first problem found; no ledger is
// returned on failure.
var ErrSnapshotInvalid = errors.New("ledger: invalid snapshot")

// snapshotMetaLine is the first line of every snapshot.
type snapshotMetaLine struct {
	Record     string    `json:"record"`
	Format     int       `json:"format"`
	ExportedAt time.Time `json:"exported_at"`
	Version    uint64    `json:"version"`
	EntryCount int       `json:"entry_count"`
	ChainLinks int       `json:"chain_links"`
	HoldCount  int       `json:"hold_count"`
	// ScheduleCount is the number of transfer-schedule records that
	// follow. Absent (zero) in snapshots written before LG-41.
	ScheduleCount int `json:"schedule_count,omitempty"`
	// ReviewCount is the number of transfer dual-control review records
	// that follow (see review.go). Absent (zero) in snapshots written
	// before LG-42.
	ReviewCount int `json:"review_count,omitempty"`
	// MerkleRoot is the hex balance-Merkle root (see ledger/merkle.go,
	// LG-78): the tree commits to every (account, currency) -> balance
	// leaf at export time. Absent in snapshots written before LG-78;
	// ImportSnapshot skips the root check for those, but every export
	// this build writes carries it, and a present root that does not
	// match the refolded balances rejects the import.
	MerkleRoot string `json:"merkle_root"`
}

// snapshotEntryLine carries one journal entry, in chain order.
type snapshotEntryLine struct {
	Record string       `json:"record"`
	Entry  JournalEntry `json:"entry"`
}

// snapshotLinkLine carries one audit-chain link, seq 1..N.
type snapshotLinkLine struct {
	Record   string `json:"record"`
	Seq      uint64 `json:"seq"`
	EntryID  string `json:"entry_id"`
	PrevHash string `json:"prev_hash"`
	Hash     string `json:"hash"`
}

// snapshotHoldLine carries one authorization hold.
type snapshotHoldLine struct {
	Record string `json:"record"`
	Hold   Hold   `json:"hold"`
}

// snapshotTermDepositLine carries one term deposit.
type snapshotTermDepositLine struct {
	Record      string      `json:"record"`
	TermDeposit TermDeposit `json:"term_deposit"`
}

// snapshotSweep is the exportable form of the unexported sweepRecord.
type snapshotSweep struct {
	SweepID   string    `json:"sweep_id"`
	EntryIDs  []string  `json:"entry_ids"`
	CreatedAt time.Time `json:"created_at"`
}

// snapshotBatch is the exportable form of the unexported batchRecord.
type snapshotBatch struct {
	BatchID   string    `json:"batch_id"`
	EntryIDs  []string  `json:"entry_ids"`
	CreatedAt time.Time `json:"created_at"`
}

// snapshotAllocation is the exportable form of the unexported
// allocRecord: one committed cost allocation (see allocation.go).
type snapshotAllocation struct {
	RuleID    string    `json:"rule_id"`
	Month     string    `json:"month"`
	EntryIDs  []string  `json:"entry_ids"`
	CreatedAt time.Time `json:"created_at"`
}

// snapshotMerge is the exportable form of the unexported mergeRecord:
// one committed account merge (see merge.go).
type snapshotMerge struct {
	MergeID   string     `json:"merge_id"`
	From      AccountID  `json:"from_account"`
	To        AccountID  `json:"to_account"`
	Legs      []MergeLeg `json:"legs"`
	EntryIDs  []string   `json:"entry_ids"`
	CreatedAt time.Time  `json:"created_at"`
}

// snapshotMergeLine carries one committed account merge, by merge ID.
type snapshotMergeLine struct {
	Record string        `json:"record"`
	Merge  snapshotMerge `json:"merge"`
}

// Idempotency-key namespaces in a snapshot. One record per key, so every
// namespace stays independently addressable on import.
const (
	snapshotNSEntry       = "entry"        // key -> JournalEntry (byKey)
	snapshotNSTransfer    = "transfer"     // key -> []entry IDs (transferKeys)
	snapshotNSHold        = "hold"         // key -> hold ID (holdKeys)
	snapshotNSCapture     = "capture"      // key -> CaptureReceipt (captureKeys)
	snapshotNSSweep       = "sweep"        // key -> snapshotSweep (sweepKeys)
	snapshotNSMergeKey    = "merge_key"    // key -> merge ID (mergeKeys)
	snapshotNSBatch       = "batch"        // key -> snapshotBatch (batchKeys)
	snapshotNSAlloc       = "allocation"   // key -> snapshotAllocation (allocKeys)
	snapshotNSTermDeposit = "term_deposit" // key -> deposit ID (termDepositKeys)
)

// snapshotIdempotencyLine carries one idempotency-key registration. Only
// the payload field matching Namespace is populated.
type snapshotIdempotencyLine struct {
	Record    string          `json:"record"`
	Namespace string          `json:"namespace"`
	Key       string          `json:"key"`
	Entry     *JournalEntry   `json:"entry,omitempty"`
	EntryIDs  []string        `json:"entry_ids,omitempty"`
	HoldID    string          `json:"hold_id,omitempty"`
	Receipt   *CaptureReceipt `json:"receipt,omitempty"`
	Sweep     *snapshotSweep  `json:"sweep,omitempty"`
	MergeID   string          `json:"merge_id,omitempty"`
	Batch     *snapshotBatch  `json:"batch,omitempty"`
	// Allocation carries the allocation receipt row for the
	// "allocation" namespace (see allocation.go).
	Allocation *snapshotAllocation `json:"allocation,omitempty"`
	// TermDepositID carries the deposit ID for the "term_deposit"
	// namespace (see termdeposit.go).
	TermDepositID string `json:"term_deposit_id,omitempty"`
}

// snapshotConfigLine carries operational config that a faithful restore
// needs: frozen and overdraft-protected accounts (replay checks order
// against them), the sub-account hierarchy, the transfer fee policy, and
// the idempotency TTL (key-expiry behavior must survive a restore).
// FeeTiers carries the tiered schedule; FeeRateBps is kept for snapshots
// written before the tiered schedule existed (a flat single-tier policy)
// and is only read when FeeTiers is absent.
type snapshotConfigLine struct {
	Record             string                  `json:"record"`
	Frozen             []AccountID             `json:"frozen"`
	OverdraftProtected []AccountID             `json:"overdraft_protected"`
	Parents            map[AccountID]AccountID `json:"parents"`
	// Dormant carries the dormancy markings (see dormancy.go): an empty
	// account fails the import instead of installing a mark that could
	// never have been created. The record is restored verbatim,
	// including the original MarkedAt — a restore must not move an
	// account's dormancy timestamp.
	Dormant           []DormancyRecord `json:"dormant,omitempty"`
	FeeRateBps        int64            `json:"fee_rate_bps"`
	FeeTiers          []FeeTier        `json:"fee_tiers,omitempty"`
	FeeRevenueAccount AccountID        `json:"fee_revenue_account"`
	// MaxTransferChunkCents carries the opt-in per-transfer chunk cap
	// for PostTransferSplit (see WithMaxTransferChunkCents); absent
	// (zero) in snapshots written before chunking existed or when
	// chunking is disabled. SplitFeePerChunk carries the per-chunk fee
	// mode (see WithSplitFeePerChunk).
	MaxTransferChunkCents int64  `json:"max_transfer_chunk_cents,omitempty"`
	SplitFeePerChunk      bool   `json:"split_fee_per_chunk,omitempty"`
	IdempotencyTTL        string `json:"idempotency_ttl"`
	// DailyLimits carries the daily outflow limits (see SetDailyLimit);
	// absent in snapshots written before daily limits existed. The
	// accumulated per-day outflow counters are deliberately NOT restored:
	// a disaster-recovery restore resets the current day's counters to
	// zero, which fail-opens for the remainder of that day — operators
	// should treat a restore as a risk-control reset event.
	DailyLimits []DailyLimit `json:"daily_limits,omitempty"`
	// LargeTxThresholds carries the large-transaction reporting
	// thresholds (see SetLargeTxThreshold); absent in snapshots written
	// before large-transaction reporting existed. The generated report
	// rows are deliberately NOT restored: the audit chain already carries
	// every generated report, and a restore must not replay them into
	// the chain — regenerating after a restore is explicit and
	// idempotent, like the daily outflow counters.
	LargeTxThresholds []LargeTxThreshold `json:"large_tx_thresholds,omitempty"`
	// LargeTxReportCurrency carries the reporting currency (see
	// SetLargeTxReportCurrency); absent in snapshots written before it
	// existed, which import as the default (USD).
	LargeTxReportCurrency string `json:"large_tx_report_currency,omitempty"`
	// BusinessDayCalendar carries the holiday calendar and its version
	// (see SetBusinessDayCalendar); absent in snapshots written before
	// business-day support existed, which import as the default
	// Saturday/Sunday calendar.
	BusinessDayCalendar *BusinessDayCalendar `json:"business_day_calendar,omitempty"`
	// AccrualBusinessDaysOnly carries the business-day-only accrual flag
	// (see SetAccrualBusinessDaysOnly); absent in snapshots written
	// before business-day support existed (defaults to false).
	AccrualBusinessDaysOnly bool `json:"accrual_business_days_only,omitempty"`
	// Budgets carries the monthly spend budgets (see SetAccountBudget);
	// absent in snapshots written before budgets existed. Like the daily
	// limits, the accumulated per-month spend counters are deliberately
	// NOT restored: a restore resets the current month's spend to zero,
	// which fail-opens for the remainder of the month — operators should
	// treat a restore as a risk-control reset event.
	Budgets []AccountBudget `json:"budgets,omitempty"`
	// Denylist carries the sanctions/denylist rows (see AddDeniedAccount,
	// AddDeniedPrefix); absent in snapshots written before the denylist
	// existed. A restore that dropped the list would silently lift
	// sanctions screening, so the list round-trips like the
	// frozen-account list.
	Denylist []DenylistEntry `json:"denylist,omitempty"`
	// FXRates carries the FX rate table (see SetFXRate) and FXAccount the
	// ledger-wide FX clearing account (see WithFXAccount); absent in
	// snapshots written before FX support existed. Restored rates keep
	// their EffectiveVersion, so restored ledgers disclose the same
	// conversion provenance as the original.
	FXRates   []ExchangeRate `json:"fx_rates,omitempty"`
	FXAccount AccountID      `json:"fx_account,omitempty"`
	// FXIncomeAccount is the ledger-wide FX spread-income account (see
	// WithFXIncomeAccount, LG-93); absent in snapshots written before
	// spread support existed.
	FXIncomeAccount AccountID `json:"fx_income_account,omitempty"`
	// FXSnapshotVersion and FXRateTableSnapshots carry the FX rate-table
	// snapshot history (LG-45, see fx_snapshot.go): the current version
	// counter and the per-version table copies. Absent in snapshots
	// written before snapshot versioning existed — the import rebuilds
	// version 1 from the imported table when it is non-empty, so a
	// restored ledger's history starts honest instead of claiming
	// version 0 with a populated table.
	FXSnapshotVersion    uint64                `json:"fx_snapshot_version,omitempty"`
	FXRateTableSnapshots []FXRateTableSnapshot `json:"fx_rate_table_snapshots,omitempty"`
	// ClosedPeriods carries the locked accounting periods ("YYYY-MM",
	// see ClosePeriod); absent in snapshots written before period locks
	// existed. A restore that dropped the locks would reopen closed
	// months, so periods are part of the operational config like the
	// frozen-account list.
	ClosedPeriods []string `json:"closed_periods,omitempty"`
	// AccountTypes carries the chart-of-accounts registrations (see
	// RegisterAccountType), sorted by account; absent in snapshots
	// written before the chart existed. A restore that dropped the
	// chart would silently lift the posting-direction control, so
	// registrations are part of the operational config like the
	// frozen-account list.
	AccountTypes []AccountTypeRegistration `json:"account_types,omitempty"`
	// AccountTags carries the account tag sets (see SetAccountTags),
	// sorted by account; absent in snapshots written before account
	// tags existed. A restore that dropped them would silently
	// reclassify every tagged account, so tags are part of the
	// operational config like the chart-of-accounts registrations.
	AccountTags []AccountTag `json:"account_tags,omitempty"`
	// BalanceWatches carries the balance threshold watches (see
	// SetBalanceWatch); absent in snapshots written before balance
	// watches existed. The alert state (breach flags) is re-derived from
	// the restored balances on import — a restore must not backfire
	// stale alerts.
	BalanceWatches []BalanceWatch `json:"balance_watches,omitempty"`
	// LowBalanceThresholds is the pre-LG-61 form of BalanceWatches
	// (see SetLowBalanceThreshold); it is still honored on import so
	// older snapshots restore their low-side alerts, but exports never
	// write it anymore.
	LowBalanceThresholds []LowBalanceThreshold `json:"low_balance_thresholds,omitempty"`
	// LiquidityAlertThresholds carries the low-liquidity alert floors
	// (see SetLiquidityAlertThreshold); absent in snapshots written
	// before LG-98. The alert state (breach flags) is re-derived from
	// the restored ledger on import — a restore must not backfire
	// stale alerts.
	LiquidityAlertThresholds []LiquidityAlertThreshold `json:"liquidity_alert_thresholds,omitempty"`
	// ReviewThresholds carries the per-account review thresholds (see
	// SetReviewThreshold); absent in snapshots written before LG-42.
	ReviewThresholds []ReviewThreshold `json:"review_thresholds,omitempty"`
	// ReviewThresholdCents carries the ledger-wide review threshold in
	// cents (see WithReviewThreshold); absent (zero) in snapshots
	// written before LG-42 or when the gate is disabled.
	ReviewThresholdCents int64 `json:"review_threshold_cents,omitempty"`
	// ReviewExpiry carries the pending-review lifetime (see
	// WithReviewExpiry) as a duration string; absent when auto-expiry
	// is disabled.
	ReviewExpiry string `json:"review_expiry,omitempty"`
	// SettlementChannels carries the account->channel mappings for the
	// settlement view (see SetSettlementChannel); absent in snapshots
	// written before LG-43. The mappings are structural config like
	// daily limits, so a disaster-recovery restore keeps the channel
	// dimension instead of silently flattening every row to "".
	SettlementChannels []SettlementChannel `json:"settlement_channels,omitempty"`
	// JournalRules carries the opt-in automatic journaling rules (see
	// SetJournalRule); absent in snapshots written before journal rules
	// existed. JournalRuleVersion is the rule-set mutation counter: every
	// rule set/delete bumps it, and the import restores the counter, so a
	// restored ledger carries the same rule-set generation as the source.
	JournalRules       []JournalRule `json:"journal_rules,omitempty"`
	JournalRuleVersion uint64        `json:"journal_rule_version,omitempty"`
	// AllocationRules carries the opt-in cost allocation rules (see
	// SetAllocationRule); absent in snapshots written before cost
	// allocation existed. AllocationRuleVersion is the rule-set
	// mutation counter, restored like the journal rules' counter.
	AllocationRules       []AllocationRule `json:"allocation_rules,omitempty"`
	AllocationRuleVersion uint64           `json:"allocation_rule_version,omitempty"`
	// ClosingPLAccounts and ClosingRetainedEarnings carry the opt-in
	// period-closing account mapping (see SetClosingAccounts); absent in
	// snapshots written before period-closing entries existed. A restore
	// that dropped the mapping would leave closing unconfigured, so the
	// mapping round-trips like the other structural config.
	ClosingPLAccounts       []AccountID `json:"closing_pl_accounts,omitempty"`
	ClosingRetainedEarnings AccountID   `json:"closing_retained_earnings,omitempty"`
	// RevalAccount carries the configured revaluation P&L account (see
	// SetRevaluationAccount); absent in snapshots written before FX
	// revaluation existed. RevalBaselines carries every revaluation
	// baseline (see RevalueBalances); absent when nothing was ever
	// revalued. Both must round-trip: a restore that dropped the
	// baselines would re-book full carrying values as gains on the next
	// revaluation.
	RevalAccount   AccountID             `json:"reval_account,omitempty"`
	RevalBaselines []RevaluationPosition `json:"reval_baselines,omitempty"`
	// FrozenAutoUnfreeze carries the timed-freeze deadlines (see
	// FreezeUntil in freeze.go); absent in snapshots written before
	// timed freezes existed. A restore that dropped them would turn a
	// timed freeze into a manual one, so deadlines round-trip like the
	// frozen-account list itself.
	FrozenAutoUnfreeze []FrozenAutoUnfreeze `json:"frozen_auto_unfreeze,omitempty"`
	// ClosedAccounts carries the terminal account-lifecycle records
	// (see CloseAccount in close.go), sorted by account; absent in
	// snapshots written before account closure existed. A restore that
	// dropped them would silently reopen closed accounts to new
	// postings, so closures round-trip like the frozen-account list.
	ClosedAccounts []AccountClosure `json:"closed_accounts,omitempty"`
	// OFXImportRegistry carries the OFX statement-import registry (see
	// ImportOFX in ofx_import.go): document SHA-256 -> import report,
	// sorted by SHA-256; absent in snapshots written before OFX import
	// existed. A restore that dropped it would re-import an already
	// imported statement as new, so the registry round-trips like the
	// idempotency indexes.
	OFXImportRegistry []OFXImportRecord `json:"ofx_import_registry,omitempty"`
	// TdInterestExpense carries the ledger-wide term-deposit
	// interest-expense account (see SetTermDepositInterestExpense);
	// absent in snapshots written before term deposits existed. A
	// restore that dropped it would turn fail-closed accruals into
	// fail-open ones, so it round-trips like the fee-revenue account.
	TdInterestExpense AccountID `json:"td_interest_expense,omitempty"`
	// CreditInterestIncome carries the ledger-wide credit-line
	// interest-income account (see SetCreditInterestIncome); absent in
	// snapshots written before credit lines existed, for the same
	// fail-closed reason as TdInterestExpense.
	CreditInterestIncome AccountID `json:"credit_interest_income,omitempty"`
	// CreditLines carries the granted credit lines (see
	// GrantCreditLine), sorted by (account, currency); absent in
	// snapshots written before credit lines existed. A restore that
	// dropped them would silently lift the -limit floor back to zero,
	// so they round-trip like the overdraft-protection list.
	CreditLines []CreditLine `json:"credit_lines,omitempty"`
	// ECLStageConfig carries the IFRS 9 staging configuration (see
	// SetECLStageConfig); absent in snapshots written before ECL
	// existed, in which case the restore keeps the ledger's default
	// config. A restore that dropped it would re-stage every exposure
	// under the wrong buckets, so it round-trips like the credit-line
	// registry.
	ECLStageConfig *ECLStageConfig `json:"ecl_stage_config,omitempty"`
	// ECLExpenseAccount and ECLAllowanceAccount carry the ledger-wide
	// fail-closed ECL booking accounts (see SetECLLossExpense and
	// SetECLAllowance); absent in snapshots written before ECL
	// existed. A restore that dropped them would turn fail-closed
	// provisioning into fail-open provisioning, so they round-trip
	// like the fee-revenue account.
	ECLExpenseAccount   AccountID `json:"ecl_expense_account,omitempty"`
	ECLAllowanceAccount AccountID `json:"ecl_allowance_account,omitempty"`
	// ECLExposures carries the ECL exposure registry (see
	// RegisterECLExposure), sorted by (account, currency); absent in
	// snapshots written before ECL existed. A restore that dropped it
	// would lose the provisioned-so-far coverage figures and the DPD
	// staging, so it round-trips like the credit-line registry.
	ECLExposures []ECLExposureRecord `json:"ecl_exposures,omitempty"`
	// PainTransfers carries the pain.001 export registry (see
	// registerPainTransferLocked in pain001.go), sorted by transfer ID;
	// absent in snapshots written before pain.001 export existed. A
	// restore that dropped it would silently un-export pre-restore
	// transfers, so the registry round-trips like the idempotency
	// indexes.
	PainTransfers []PainTransfer `json:"pain_transfers,omitempty"`
	// AccountIBANs carries the opt-in IBAN registry (see SetAccountIBAN
	// in pain001.go), sorted by account; absent in snapshots written
	// before pain.001 export existed. A restore that dropped it would
	// turn previously exportable payments into fail-closed refusals
	// with no explanation, so it round-trips like the account tags.
	AccountIBANs []AccountIBAN `json:"account_ibans,omitempty"`
	// VirtualAccounts carries the van -> parent mappings (see
	// RegisterVirtualAccount in virtual_account.go), sorted by VAN, and
	// VirtualAccountVersion the mapping mutation counter; absent in
	// snapshots written before virtual accounts existed. A restore that
	// dropped them would silently stop attributing VAN receipts to
	// parents, so they round-trip like the frozen-account list — and the
	// counter round-trips like the journal-rule version, so a restored
	// ledger carries the same mapping generation as the source.
	VirtualAccounts       []VirtualAccount `json:"virtual_accounts,omitempty"`
	VirtualAccountVersion uint64           `json:"virtual_account_version,omitempty"`
}

// FrozenAutoUnfreeze pairs a frozen account with its scheduled
// automatic-unfreeze time (see FreezeUntil). It is the snapshot record
// form of the Ledger.frozenUntil index.
type FrozenAutoUnfreeze struct {
	Account        AccountID `json:"account"`
	AutoUnfreezeAt time.Time `json:"auto_unfreeze_at"`
}

// flatFeeRateBps reports the fee rate for snapshots read by legacy
// consumers that only understand fee_rate_bps: it is the rate when the
// policy is a single flat tier starting at 0, and 0 otherwise (a tiered
// schedule has no single rate; legacy readers see the fee_tiers field).
func flatFeeRateBps(tiers []FeeTier) int64 {
	if len(tiers) == 1 && tiers[0].MinAmountCents == 0 {
		return tiers[0].RateBps
	}
	return 0
}

// ExportSnapshot writes a JSONL disaster-recovery snapshot of the ledger to
// w. The export holds the read lock for its whole duration, so the
// document describes one consistent point in time: a concurrent Post can
// never slip between the journal scan and the chain scan. Deterministic
// section and key ordering means identical ledger state exports to
// identical bytes.
func (l *Ledger) ExportSnapshot(w io.Writer) error {
	l.mu.RLock()
	defer l.mu.RUnlock()

	enc := json.NewEncoder(w)

	meta := snapshotMetaLine{
		Record:        "meta",
		Format:        snapshotFormatVersion,
		ExportedAt:    time.Now().UTC(),
		Version:       l.version,
		EntryCount:    len(l.chain),
		ChainLinks:    len(l.chain),
		HoldCount:     len(l.holds),
		ScheduleCount: len(l.schedules),
		ReviewCount:   len(l.reviews),
		// LG-78: the balance Merkle root commits to every
		// (account, currency) -> balance at export time, under the
		// same read lock as the rest of the document.
		MerkleRoot: l.merkleTreeLocked().RootHex(),
	}
	if err := enc.Encode(meta); err != nil {
		return fmt.Errorf("ledger: snapshot export: %w", err)
	}

	// Entries and links in chain (Post) order. The link order is the
	// canonical journal order; import folds entries in this order.
	for _, link := range l.chain {
		e, ok := l.entries[link.entryID]
		if !ok {
			return fmt.Errorf("ledger: snapshot export: chain references missing entry %q", link.entryID)
		}
		if err := enc.Encode(snapshotEntryLine{Record: "entry", Entry: e}); err != nil {
			return fmt.Errorf("ledger: snapshot export: %w", err)
		}
		if err := enc.Encode(snapshotLinkLine{
			Record:   "link",
			Seq:      link.seq,
			EntryID:  link.entryID,
			PrevHash: hex.EncodeToString(link.prevHash[:]),
			Hash:     hex.EncodeToString(link.hash[:]),
		}); err != nil {
			return fmt.Errorf("ledger: snapshot export: %w", err)
		}
	}

	if err := l.exportHoldsLocked(enc); err != nil {
		return err
	}
	if err := l.exportTermDepositsLocked(enc); err != nil {
		return err
	}
	if err := l.exportMergesLocked(enc); err != nil {
		return err
	}
	if err := l.exportSchedulesLocked(enc); err != nil {
		return err
	}
	if err := l.exportReviewsLocked(enc); err != nil {
		return err
	}
	if err := l.exportIdempotencyLocked(enc); err != nil {
		return err
	}
	if err := l.exportConfigLocked(enc); err != nil {
		return err
	}
	return nil
}

// validateReviewRecord checks one imported review record structurally:
// a non-empty transfer ID, a known status, legs that match the status
// (pending-marked while frozen, effective once approved), and an
// approved receipt only on approved reviews.
func validateReviewRecord(r *TransferReview) error {
	if r.TransferID == "" {
		return fmt.Errorf("review with empty transfer ID")
	}
	switch r.Status {
	case ReviewStatusPending, ReviewStatusApproved, ReviewStatusRejected, ReviewStatusExpired:
	default:
		return fmt.Errorf("review %q has unknown status %q", r.TransferID, r.Status)
	}
	if len(r.Entries) == 0 {
		return fmt.Errorf("review %q has no entries", r.TransferID)
	}
	for _, e := range r.Entries {
		if e.ID == "" {
			return fmt.Errorf("review %q has an entry with empty ID", r.TransferID)
		}
		// Frozen rows (pending, rejected, expired) must still carry
		// the marker; approved rows must have flipped to effective —
		// the chain hash covers the marker, so a mismatch is
		// corruption, not a warning.
		if want := r.Status != ReviewStatusApproved; e.PendingReview != want {
			return fmt.Errorf("review %q entry %q pending_review=%v, want %v for status %q",
				r.TransferID, e.ID, e.PendingReview, want, r.Status)
		}
	}
	if r.Status == ReviewStatusApproved && r.ApprovedReceipt == nil {
		return fmt.Errorf("review %q is approved but carries no approval receipt", r.TransferID)
	}
	if r.Status != ReviewStatusApproved && r.ApprovedReceipt != nil {
		return fmt.Errorf("review %q is %s but carries an approval receipt", r.TransferID, r.Status)
	}
	return nil
}

// reviewsEqual compares two review records field by field, ignoring
// time.Location representation: JSON round-trips preserve the instant
// but not the exact Location pointer.
func reviewsEqual(a, b *TransferReview) bool {
	if a.TransferID != b.TransferID || a.Status != b.Status ||
		a.FeeCents != b.FeeCents || a.FeeTierIndex != b.FeeTierIndex ||
		a.FeeRateBps != b.FeeRateBps || a.TotalOutflowCents != b.TotalOutflowCents ||
		a.ThresholdCents != b.ThresholdCents ||
		a.ReviewedBy != b.ReviewedBy ||
		a.CreatedAt.UnixNano() != b.CreatedAt.UnixNano() ||
		a.ExpiresAt.UnixNano() != b.ExpiresAt.UnixNano() ||
		a.ReviewedAt.UnixNano() != b.ReviewedAt.UnixNano() ||
		len(a.Entries) != len(b.Entries) {
		return false
	}
	// The transfer request normalizes the same way on both sides; a
	// field-by-field compare keeps the check honest without
	// reflect.DeepEqual's location sensitivity.
	ta, tb := a.Transfer, b.Transfer
	if ta.ID != tb.ID || ta.From != tb.From || ta.To != tb.To ||
		ta.AmountCents != tb.AmountCents || ta.Currency != tb.Currency ||
		ta.ToCurrency != tb.ToCurrency || ta.FXAccount != tb.FXAccount ||
		ta.FeeCents != tb.FeeCents || ta.FeeAccount != tb.FeeAccount ||
		ta.SkipFee != tb.SkipFee || ta.IdempotencyKey != tb.IdempotencyKey ||
		ta.Memo != tb.Memo || ta.ExternalRef != tb.ExternalRef || ta.CreatedAt.UnixNano() != tb.CreatedAt.UnixNano() {
		return false
	}
	for i := range a.Entries {
		if !journalEntriesEqual(a.Entries[i], b.Entries[i]) {
			return false
		}
	}
	return true
}

// exportHoldsLocked writes the hold records, sorted by (CreatedAt, ID)
// for determinism. Callers must hold l.mu; the read lock suffices.
func (l *Ledger) exportHoldsLocked(enc *json.Encoder) error {
	holds := make([]Hold, 0, len(l.holds))
	for _, h := range l.holds {
		holds = append(holds, h)
	}
	sort.Slice(holds, func(i, j int) bool {
		if !holds[i].CreatedAt.Equal(holds[j].CreatedAt) {
			return holds[i].CreatedAt.Before(holds[j].CreatedAt)
		}
		return holds[i].ID < holds[j].ID
	})
	for _, h := range holds {
		if err := enc.Encode(snapshotHoldLine{Record: "hold", Hold: h}); err != nil {
			return fmt.Errorf("ledger: snapshot export: %w", err)
		}
	}
	return nil
}

// exportTermDepositsLocked writes the term-deposit records, sorted by
// deposit ID for determinism. Callers must hold l.mu; the read lock
// suffices.
func (l *Ledger) exportTermDepositsLocked(enc *json.Encoder) error {
	deposits := make([]TermDeposit, 0, len(l.termDeposits))
	for _, td := range l.termDeposits {
		deposits = append(deposits, td)
	}
	sort.Slice(deposits, func(i, j int) bool { return deposits[i].ID < deposits[j].ID })
	for _, td := range deposits {
		if err := enc.Encode(snapshotTermDepositLine{Record: "term_deposit", TermDeposit: td}); err != nil {
			return fmt.Errorf("ledger: snapshot export: %w", err)
		}
	}
	return nil
}

// exportMergesLocked writes the merge records, sorted by merge ID for
// determinism. Callers must hold l.mu; the read lock suffices.
func (l *Ledger) exportMergesLocked(enc *json.Encoder) error {
	ids := make([]string, 0, len(l.merges))
	for id := range l.merges {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		rec := l.merges[id]
		if err := enc.Encode(snapshotMergeLine{Record: "merge", Merge: snapshotMerge{
			MergeID:   rec.mergeID,
			From:      rec.from,
			To:        rec.to,
			Legs:      rec.legs,
			EntryIDs:  rec.entryIDs,
			CreatedAt: rec.createdAt,
		}}); err != nil {
			return fmt.Errorf("ledger: snapshot export: %w", err)
		}
	}
	return nil
}

// snapshotReviewLine carries one transfer dual-control review (see
// review.go), in transfer-ID order. The review carries its legs: for a
// pending, rejected, or expired review the legs are the frozen,
// pending-marked journal rows (they have no chain links, so the review
// record is their only export); for an approved review the legs are the
// settled, effective rows, already exported as entry/link pairs — the
// record then restores the registry (status, reviewer, decision time)
// without re-adding the rows.
type snapshotReviewLine struct {
	Record string         `json:"record"`
	Review TransferReview `json:"review"`
}

// exportReviewsLocked writes the transfer dual-control review records,
// sorted by transfer ID for determinism. Callers must hold l.mu; the
// read lock suffices.
func (l *Ledger) exportReviewsLocked(enc *json.Encoder) error {
	ids := make([]string, 0, len(l.reviews))
	for id := range l.reviews {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := enc.Encode(snapshotReviewLine{Record: "review", Review: *l.reviews[id]}); err != nil {
			return fmt.Errorf("ledger: snapshot export: %w", err)
		}
	}
	return nil
}

// snapshotScheduleLine carries one transfer schedule (see schedule.go),
// in schedule-ID order. The run history is operational and is not
// exported — only the schedule definition plus its RunSeq/NextRunAt/
// Status, which is what a restore needs to keep firing.
type snapshotScheduleLine struct {
	Record   string           `json:"record"`
	Schedule TransferSchedule `json:"schedule"`
}

// exportSchedulesLocked writes the transfer-schedule records, sorted by
// schedule ID for determinism. Callers must hold l.mu; the read lock
// suffices.
func (l *Ledger) exportSchedulesLocked(enc *json.Encoder) error {
	ids := make([]string, 0, len(l.schedules))
	for id := range l.schedules {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := enc.Encode(snapshotScheduleLine{Record: "schedule", Schedule: *l.schedules[id]}); err != nil {
			return fmt.Errorf("ledger: snapshot export: %w", err)
		}
	}
	return nil
}

// exportIdempotencyLocked writes the seven idempotency-key namespaces, one
// record per key, sorted for determinism. Callers must hold l.mu; the
// read lock suffices.
func (l *Ledger) exportIdempotencyLocked(enc *json.Encoder) error {
	sortedKeys := func(keys []string) []string {
		out := append([]string(nil), keys...)
		sort.Strings(out)
		return out
	}
	byKeyList := make([]string, 0, len(l.byKey))
	for k := range l.byKey {
		byKeyList = append(byKeyList, k)
	}
	for _, k := range sortedKeys(byKeyList) {
		e := l.byKey[k]
		if err := enc.Encode(snapshotIdempotencyLine{
			Record: "idempotency", Namespace: snapshotNSEntry, Key: k, Entry: &e,
		}); err != nil {
			return fmt.Errorf("ledger: snapshot export: %w", err)
		}
	}
	transferKeyList := make([]string, 0, len(l.transferKeys))
	for k := range l.transferKeys {
		transferKeyList = append(transferKeyList, k)
	}
	for _, k := range sortedKeys(transferKeyList) {
		if err := enc.Encode(snapshotIdempotencyLine{
			Record: "idempotency", Namespace: snapshotNSTransfer, Key: k, EntryIDs: l.transferKeys[k],
		}); err != nil {
			return fmt.Errorf("ledger: snapshot export: %w", err)
		}
	}
	holdKeyList := make([]string, 0, len(l.holdKeys))
	for k := range l.holdKeys {
		holdKeyList = append(holdKeyList, k)
	}
	for _, k := range sortedKeys(holdKeyList) {
		if err := enc.Encode(snapshotIdempotencyLine{
			Record: "idempotency", Namespace: snapshotNSHold, Key: k, HoldID: l.holdKeys[k],
		}); err != nil {
			return fmt.Errorf("ledger: snapshot export: %w", err)
		}
	}
	captureKeyList := make([]string, 0, len(l.captureKeys))
	for k := range l.captureKeys {
		captureKeyList = append(captureKeyList, k)
	}
	for _, k := range sortedKeys(captureKeyList) {
		r := l.captureKeys[k]
		if err := enc.Encode(snapshotIdempotencyLine{
			Record: "idempotency", Namespace: snapshotNSCapture, Key: k, Receipt: &r,
		}); err != nil {
			return fmt.Errorf("ledger: snapshot export: %w", err)
		}
	}
	sweepKeyList := make([]string, 0, len(l.sweepKeys))
	for k := range l.sweepKeys {
		sweepKeyList = append(sweepKeyList, k)
	}
	for _, k := range sortedKeys(sweepKeyList) {
		rec := l.sweepKeys[k]
		if err := enc.Encode(snapshotIdempotencyLine{
			Record: "idempotency", Namespace: snapshotNSSweep, Key: k,
			Sweep: &snapshotSweep{SweepID: rec.sweepID, EntryIDs: rec.entryIDs, CreatedAt: rec.createdAt},
		}); err != nil {
			return fmt.Errorf("ledger: snapshot export: %w", err)
		}
	}
	mergeKeyList := make([]string, 0, len(l.mergeKeys))
	for k := range l.mergeKeys {
		mergeKeyList = append(mergeKeyList, k)
	}
	for _, k := range sortedKeys(mergeKeyList) {
		if err := enc.Encode(snapshotIdempotencyLine{
			Record: "idempotency", Namespace: snapshotNSMergeKey, Key: k,
			MergeID: l.mergeKeys[k],
		}); err != nil {
			return fmt.Errorf("ledger: snapshot export: %w", err)
		}
	}
	batchKeyList := make([]string, 0, len(l.batchKeys))
	for k := range l.batchKeys {
		batchKeyList = append(batchKeyList, k)
	}
	for _, k := range sortedKeys(batchKeyList) {
		rec := l.batchKeys[k]
		if err := enc.Encode(snapshotIdempotencyLine{
			Record: "idempotency", Namespace: snapshotNSBatch, Key: k,
			Batch: &snapshotBatch{BatchID: rec.batchID, EntryIDs: rec.entryIDs, CreatedAt: rec.createdAt},
		}); err != nil {
			return fmt.Errorf("ledger: snapshot export: %w", err)
		}
	}
	allocKeyList := make([]string, 0, len(l.allocKeys))
	for k := range l.allocKeys {
		allocKeyList = append(allocKeyList, k)
	}
	for _, k := range sortedKeys(allocKeyList) {
		rec := l.allocKeys[k]
		if err := enc.Encode(snapshotIdempotencyLine{
			Record: "idempotency", Namespace: snapshotNSAlloc, Key: k,
			Allocation: &snapshotAllocation{RuleID: rec.ruleID, Month: rec.month, EntryIDs: rec.entryIDs, CreatedAt: rec.createdAt},
		}); err != nil {
			return fmt.Errorf("ledger: snapshot export: %w", err)
		}
	}
	termDepositKeyList := make([]string, 0, len(l.termDepositKeys))
	for k := range l.termDepositKeys {
		termDepositKeyList = append(termDepositKeyList, k)
	}
	for _, k := range sortedKeys(termDepositKeyList) {
		if err := enc.Encode(snapshotIdempotencyLine{
			Record: "idempotency", Namespace: snapshotNSTermDeposit, Key: k,
			TermDepositID: l.termDepositKeys[k],
		}); err != nil {
			return fmt.Errorf("ledger: snapshot export: %w", err)
		}
	}
	return nil
}

// exportConfigLocked writes the operational config record. Callers must
// hold l.mu; the read lock suffices.
func (l *Ledger) exportConfigLocked(enc *json.Encoder) error {
	frozen := make([]AccountID, 0, len(l.frozen))
	for a := range l.frozen {
		frozen = append(frozen, a)
	}
	sort.Slice(frozen, func(i, j int) bool { return frozen[i] < frozen[j] })
	overdraft := make([]AccountID, 0, len(l.noOverdraft))
	for a := range l.noOverdraft {
		overdraft = append(overdraft, a)
	}
	sort.Slice(overdraft, func(i, j int) bool { return overdraft[i] < overdraft[j] })
	parents := make(map[AccountID]AccountID, len(l.parents))
	for c, p := range l.parents {
		parents[c] = p
	}
	cfg := snapshotConfigLine{
		Record:                "config",
		Frozen:                frozen,
		OverdraftProtected:    overdraft,
		Parents:               parents,
		Dormant:               l.dormantCopyLocked(),
		FeeRateBps:            flatFeeRateBps(l.feeTiers),
		FeeTiers:              l.feeTiers,
		FeeRevenueAccount:     l.feeRevenueAccount,
		MaxTransferChunkCents: l.maxTransferChunkCents,
		SplitFeePerChunk:      l.splitFeePerChunk,
		IdempotencyTTL:        l.idempotencyTTL.String(),
		DailyLimits:           l.dailyLimitsLocked(),
		LargeTxThresholds:     l.largeTxThresholdsLocked(),
		LargeTxReportCurrency: l.largeTxReportCurrency,
		BusinessDayCalendar:   l.businessCalendarSnapshotLocked(),
		AccrualBusinessDaysOnly: l.accrualBusinessDaysOnly,
		Budgets:               l.budgetsLocked(),
		Denylist:              l.denylistLocked(),
		FXRates:               l.fxRatesLocked(),
		FXAccount:             l.fxAccount,
		FXIncomeAccount:       l.fxIncomeAccount,
		FXSnapshotVersion:     l.fxSnapshotVersion,
		FXRateTableSnapshots:  l.fxRateTableSnapshotsLocked(),
		ClosedPeriods:         l.closedPeriodsLocked(),
		BalanceWatches:        l.balanceWatchesLocked(),
		LiquidityAlertThresholds: l.liquidityThresholdsLocked(),
		ReviewThresholds:      l.reviewThresholdsLocked(),
		ReviewThresholdCents:  l.reviewThreshold,
		SettlementChannels:    l.settlementChannelsLocked(),
		JournalRules:          l.journalRulesLocked(),
		JournalRuleVersion:    l.journalRuleVersion,
		AllocationRules:       l.allocationRulesLocked(),
		AllocationRuleVersion: l.allocRuleVersion,
		RevalAccount:          l.revalAccount,
		RevalBaselines:        l.revaluationPositionsLocked(),
		AccountTypes:          l.accountTypesLocked(),
		AccountTags:           l.accountTagsLocked(),
		ClosedAccounts:        l.closedCopyLocked(),
		OFXImportRegistry:     l.ofxImportsLocked(),
		TdInterestExpense:     l.tdInterestExpense,
		CreditInterestIncome:  l.creditInterestIncome,
		CreditLines:           l.creditLinesLocked(),
		ECLExpenseAccount:     l.eclExpenseAccount,
		ECLAllowanceAccount:   l.eclAllowanceAccount,
		ECLExposures:          l.eclExposureRecordsLocked(),
		PainTransfers:         l.painTransfersLocked(),
		AccountIBANs:          l.accountIBANsLocked(),
		VirtualAccounts:       l.virtualAccountsLocked(),
		VirtualAccountVersion: l.virtualAccountVersion,
	}
	eclCfg := l.eclStageConfig
	cfg.ECLStageConfig = &eclCfg
	plAccounts, retainedEarnings := l.closingAccountsLocked()
	if retainedEarnings != "" {
		cfg.ClosingPLAccounts = plAccounts
		cfg.ClosingRetainedEarnings = retainedEarnings
	}
	if l.reviewExpiry > 0 {
		cfg.ReviewExpiry = l.reviewExpiry.String()
	}
	cfg.FrozenAutoUnfreeze = l.frozenAutoUnfreezeLocked()
	if err := enc.Encode(cfg); err != nil {
		return fmt.Errorf("ledger: snapshot export: %w", err)
	}
	return nil
}

// frozenAutoUnfreezeLocked returns the timed-freeze deadlines sorted by
// account, the snapshot record form of the frozenUntil index. Callers
// must hold l.mu (either lock suffices; export holds the read lock).
func (l *Ledger) frozenAutoUnfreezeLocked() []FrozenAutoUnfreeze {
	out := make([]FrozenAutoUnfreeze, 0, len(l.frozenUntil))
	for a, until := range l.frozenUntil {
		out = append(out, FrozenAutoUnfreeze{Account: a, AutoUnfreezeAt: until})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Account < out[j].Account })
	return out
}

// frozenUntilEqual compares two freeze-deadline indexes by instant, not by
// time.Location representation: JSON round-trips preserve the instant but
// not the exact Location pointer, so reflect.DeepEqual would report a
// restored index as different.
func frozenUntilEqual(a, b map[AccountID]time.Time) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok || !va.Equal(vb) {
			return false
		}
	}
	return true
}

// journalEntriesEqual compares two journal entries field by field,
// ignoring time.Location representation: JSON round-trips preserve the
// instant (and thus UnixNano, which is what the chain hash covers) but not
// the exact Location pointer.
func journalEntriesEqual(a, b JournalEntry) bool {
	return a.ID == b.ID &&
		a.DebitAccount == b.DebitAccount &&
		a.CreditAccount == b.CreditAccount &&
		a.AmountCents == b.AmountCents &&
		a.Currency == b.Currency &&
		a.IdempotencyKey == b.IdempotencyKey &&
		a.BatchID == b.BatchID &&
		a.Memo == b.Memo &&
		a.ExternalRef == b.ExternalRef &&
		attachmentsEqual(a.Attachments, b.Attachments) &&
		a.PendingReview == b.PendingReview &&
		a.CreatedAt.UnixNano() == b.CreatedAt.UnixNano()
}

// attachmentsEqual compares two attachment lists element-wise: a nil list
// and an empty list are equal (JSON round-trips preserve neither
// distinction — omitempty drops both), so snapshot fidelity checks must
// not distinguish them either.
func attachmentsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ImportSnapshot reads a disaster-recovery snapshot produced by
// ExportSnapshot and rebuilds a working Ledger from it: journal rows,
// derived balances/totals/indexes (refolded in chain order), holds,
// idempotency registries, and operational config.
//
// Every structural invariant is checked on the way in — section order and
// counts, link seq continuity, entry/link correspondence, registry
// references — and the import finishes with a full audit-chain
// verification. The first problem found rejects the whole import with an
// error wrapping ErrSnapshotInvalid; no ledger is returned, so callers
// can never observe a half-restored ledger.
func ImportSnapshot(r io.Reader) (*Ledger, error) {
	fail := func(format string, args ...any) (*Ledger, error) {
		return nil, fmt.Errorf("%w: %s", ErrSnapshotInvalid, fmt.Sprintf(format, args...))
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)

	var meta *snapshotMetaLine
	entries := map[string]JournalEntry{}
	var linkOrder []snapshotLinkLine
	holds := map[string]Hold{}
	termDeposits := map[string]TermDeposit{}
	byKey := map[string]JournalEntry{}
	transferKeys := map[string][]string{}
	holdKeys := map[string]string{}
	termDepositKeys := map[string]string{}
	captureKeys := map[string]CaptureReceipt{}
	sweepKeys := map[string]sweepRecord{}
	merges := map[string]mergeRecord{}
	mergeKeys := map[string]string{}
	batchKeys := map[string]batchRecord{}
	allocKeys := map[string]allocRecord{}
	schedules := map[string]TransferSchedule{}
	reviews := map[string]*TransferReview{}
	var cfg *snapshotConfigLine
	lineNo := 0

	for sc.Scan() {
		lineNo++
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var env struct {
			Record string `json:"record"`
		}
		if err := json.Unmarshal(line, &env); err != nil {
			return fail("line %d: malformed JSON: %v", lineNo, err)
		}
		switch env.Record {
		case "meta":
			if lineNo != 1 {
				return fail("line %d: meta record must be the first line", lineNo)
			}
			if meta != nil {
				return fail("line %d: duplicate meta record", lineNo)
			}
			var m snapshotMetaLine
			if err := json.Unmarshal(line, &m); err != nil {
				return fail("line %d: malformed meta: %v", lineNo, err)
			}
			if m.Format != snapshotFormatVersion {
				return fail("line %d: unsupported snapshot format %d (this build reads %d)",
					lineNo, m.Format, snapshotFormatVersion)
			}
			// An incremental snapshot carries base_version; a full
			// import must refuse it explicitly instead of silently
			// treating it as a complete journal.
			var probe struct {
				BaseVersion *uint64 `json:"base_version"`
			}
			if err := json.Unmarshal(line, &probe); err == nil && probe.BaseVersion != nil {
				return fail("line %d: incremental snapshot (base_version %d) cannot be fully imported; apply it with ImportIncrementalSnapshot",
					lineNo, *probe.BaseVersion)
			}
			meta = &m
		case "entry":
			if meta == nil {
				return fail("line %d: entry before meta", lineNo)
			}
			var el snapshotEntryLine
			if err := json.Unmarshal(line, &el); err != nil {
				return fail("line %d: malformed entry: %v", lineNo, err)
			}
			if el.Entry.ID == "" {
				return fail("line %d: entry with empty ID", lineNo)
			}
			if _, dup := entries[el.Entry.ID]; dup {
				return fail("line %d: duplicate entry ID %q", lineNo, el.Entry.ID)
			}
			entries[el.Entry.ID] = el.Entry
		case "link":
			if meta == nil {
				return fail("line %d: link before meta", lineNo)
			}
			var ll snapshotLinkLine
			if err := json.Unmarshal(line, &ll); err != nil {
				return fail("line %d: malformed link: %v", lineNo, err)
			}
			if want := uint64(len(linkOrder) + 1); ll.Seq != want {
				return fail("line %d: link seq %d out of order, want %d", lineNo, ll.Seq, want)
			}
			if len(ll.PrevHash) != 64 || len(ll.Hash) != 64 {
				return fail("line %d: link %d has malformed hashes", lineNo, ll.Seq)
			}
			linkOrder = append(linkOrder, ll)
		case "hold":
			if meta == nil {
				return fail("line %d: hold before meta", lineNo)
			}
			var hl snapshotHoldLine
			if err := json.Unmarshal(line, &hl); err != nil {
				return fail("line %d: malformed hold: %v", lineNo, err)
			}
			if hl.Hold.ID == "" {
				return fail("line %d: hold with empty ID", lineNo)
			}
			if _, dup := holds[hl.Hold.ID]; dup {
				return fail("line %d: duplicate hold ID %q", lineNo, hl.Hold.ID)
			}
			holds[hl.Hold.ID] = hl.Hold
		case "term_deposit":
			if meta == nil {
				return fail("line %d: term_deposit before meta", lineNo)
			}
			var tl snapshotTermDepositLine
			if err := json.Unmarshal(line, &tl); err != nil {
				return fail("line %d: malformed term_deposit: %v", lineNo, err)
			}
			if tl.TermDeposit.ID == "" {
				return fail("line %d: term_deposit with empty ID", lineNo)
			}
			if _, dup := termDeposits[tl.TermDeposit.ID]; dup {
				return fail("line %d: duplicate term deposit ID %q", lineNo, tl.TermDeposit.ID)
			}
			termDeposits[tl.TermDeposit.ID] = tl.TermDeposit
		case "merge":
			if meta == nil {
				return fail("line %d: merge before meta", lineNo)
			}
			var ml snapshotMergeLine
			if err := json.Unmarshal(line, &ml); err != nil {
				return fail("line %d: malformed merge: %v", lineNo, err)
			}
			if ml.Merge.MergeID == "" {
				return fail("line %d: merge with empty ID", lineNo)
			}
			if _, dup := merges[ml.Merge.MergeID]; dup {
				return fail("line %d: duplicate merge ID %q", lineNo, ml.Merge.MergeID)
			}
			merges[ml.Merge.MergeID] = mergeRecord{
				mergeID:   ml.Merge.MergeID,
				from:      ml.Merge.From,
				to:        ml.Merge.To,
				legs:      ml.Merge.Legs,
				entryIDs:  ml.Merge.EntryIDs,
				createdAt: ml.Merge.CreatedAt,
			}
		case "idempotency":
			if meta == nil {
				return fail("line %d: idempotency record before meta", lineNo)
			}
			var il snapshotIdempotencyLine
			if err := json.Unmarshal(line, &il); err != nil {
				return fail("line %d: malformed idempotency record: %v", lineNo, err)
			}
			if il.Key == "" {
				return fail("line %d: idempotency record with empty key", lineNo)
			}
			switch il.Namespace {
			case snapshotNSEntry:
				if il.Entry == nil {
					return fail("line %d: entry-namespace record without entry", lineNo)
				}
				if _, dup := byKey[il.Key]; dup {
					return fail("line %d: duplicate idempotency key %q", lineNo, il.Key)
				}
				byKey[il.Key] = *il.Entry
			case snapshotNSTransfer:
				if _, dup := transferKeys[il.Key]; dup {
					return fail("line %d: duplicate idempotency key %q", lineNo, il.Key)
				}
				transferKeys[il.Key] = il.EntryIDs
			case snapshotNSHold:
				if il.HoldID == "" {
					return fail("line %d: hold-namespace record without hold ID", lineNo)
				}
				if _, dup := holdKeys[il.Key]; dup {
					return fail("line %d: duplicate idempotency key %q", lineNo, il.Key)
				}
				holdKeys[il.Key] = il.HoldID
			case snapshotNSTermDeposit:
				if il.TermDepositID == "" {
					return fail("line %d: term_deposit-namespace record without deposit ID", lineNo)
				}
				if _, dup := termDepositKeys[il.Key]; dup {
					return fail("line %d: duplicate idempotency key %q", lineNo, il.Key)
				}
				termDepositKeys[il.Key] = il.TermDepositID
			case snapshotNSCapture:
				if il.Receipt == nil {
					return fail("line %d: capture-namespace record without receipt", lineNo)
				}
				if _, dup := captureKeys[il.Key]; dup {
					return fail("line %d: duplicate idempotency key %q", lineNo, il.Key)
				}
				captureKeys[il.Key] = *il.Receipt
			case snapshotNSSweep:
				if il.Sweep == nil {
					return fail("line %d: sweep-namespace record without sweep", lineNo)
				}
				if _, dup := sweepKeys[il.Key]; dup {
					return fail("line %d: duplicate idempotency key %q", lineNo, il.Key)
				}
				sweepKeys[il.Key] = sweepRecord{
					sweepID:   il.Sweep.SweepID,
					entryIDs:  il.Sweep.EntryIDs,
					createdAt: il.Sweep.CreatedAt,
				}
			case snapshotNSMergeKey:
				if il.MergeID == "" {
					return fail("line %d: merge_key-namespace record without merge_id", lineNo)
				}
				if _, dup := mergeKeys[il.Key]; dup {
					return fail("line %d: duplicate idempotency key %q", lineNo, il.Key)
				}
				mergeKeys[il.Key] = il.MergeID
			case snapshotNSBatch:
				if il.Batch == nil {
					return fail("line %d: batch-namespace record without batch", lineNo)
				}
				if _, dup := batchKeys[il.Key]; dup {
					return fail("line %d: duplicate idempotency key %q", lineNo, il.Key)
				}
				batchKeys[il.Key] = batchRecord{
					batchID:   il.Batch.BatchID,
					entryIDs:  il.Batch.EntryIDs,
					createdAt: il.Batch.CreatedAt,
				}
			case snapshotNSAlloc:
				if il.Allocation == nil {
					return fail("line %d: allocation-namespace record without allocation", lineNo)
				}
				if _, dup := allocKeys[il.Key]; dup {
					return fail("line %d: duplicate idempotency key %q", lineNo, il.Key)
				}
				allocKeys[il.Key] = allocRecord{
					ruleID:    il.Allocation.RuleID,
					month:     il.Allocation.Month,
					entryIDs:  il.Allocation.EntryIDs,
					createdAt: il.Allocation.CreatedAt,
				}
			default:
				return fail("line %d: unknown idempotency namespace %q", lineNo, il.Namespace)
			}
		case "config":
			if meta == nil {
				return fail("line %d: config before meta", lineNo)
			}
			if cfg != nil {
				return fail("line %d: duplicate config record", lineNo)
			}
			var c snapshotConfigLine
			if err := json.Unmarshal(line, &c); err != nil {
				return fail("line %d: malformed config: %v", lineNo, err)
			}
			cfg = &c
		case "schedule":
			if meta == nil {
				return fail("line %d: schedule before meta", lineNo)
			}
			var sl snapshotScheduleLine
			if err := json.Unmarshal(line, &sl); err != nil {
				return fail("line %d: malformed schedule: %v", lineNo, err)
			}
			if _, dup := schedules[sl.Schedule.ID]; dup {
				return fail("line %d: duplicate schedule ID %q", lineNo, sl.Schedule.ID)
			}
			if err := validateScheduleRecord(&sl.Schedule); err != nil {
				return fail("line %d: invalid schedule %q: %v", lineNo, sl.Schedule.ID, err)
			}
			schedules[sl.Schedule.ID] = sl.Schedule
		case "review":
			if meta == nil {
				return fail("line %d: review before meta", lineNo)
			}
			var rl snapshotReviewLine
			if err := json.Unmarshal(line, &rl); err != nil {
				return fail("line %d: malformed review: %v", lineNo, err)
			}
			if _, dup := reviews[rl.Review.TransferID]; dup {
				return fail("line %d: duplicate review transfer ID %q", lineNo, rl.Review.TransferID)
			}
			if err := validateReviewRecord(&rl.Review); err != nil {
				return fail("line %d: invalid review: %v", lineNo, err)
			}
			rec := rl.Review
			reviews[rec.TransferID] = &rec
		default:
			return fail("line %d: unknown record type %q", lineNo, env.Record)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("ledger: snapshot import: read: %w", err)
	}
	if meta == nil {
		return fail("empty snapshot: missing meta record")
	}

	// Structural invariants before any state is built.
	if len(entries) != meta.EntryCount {
		return fail("entry count %d does not match meta entry_count %d", len(entries), meta.EntryCount)
	}
	if len(linkOrder) != meta.ChainLinks {
		return fail("link count %d does not match meta chain_links %d", len(linkOrder), meta.ChainLinks)
	}
	if len(holds) != meta.HoldCount {
		return fail("hold count %d does not match meta hold_count %d", len(holds), meta.HoldCount)
	}
	if len(schedules) != meta.ScheduleCount {
		return fail("schedule count %d does not match meta schedule_count %d", len(schedules), meta.ScheduleCount)
	}
	if len(reviews) != meta.ReviewCount {
		return fail("review count %d does not match meta review_count %d", len(reviews), meta.ReviewCount)
	}
	// In a healthy ledger every Post bumps the version and appends exactly
	// one link, so version == links; a snapshot claiming otherwise was not
	// produced by a consistent export.
	if meta.Version != uint64(meta.ChainLinks) {
		return fail("meta version %d inconsistent with chain length %d", meta.Version, meta.ChainLinks)
	}
	if cfg == nil {
		return fail("missing config record")
	}

	l := New()

	// Fold the journal in chain order, rebuilding every derived index the
	// same way commitEntryLocked does. The chain is rebuilt from the
	// exported link hashes (not recomputed) so the final VerifyChain also
	// proves the exported hashes match the recomputed ones.
	for _, ll := range linkOrder {
		e, ok := entries[ll.EntryID]
		if !ok {
			return fail("link seq %d references missing entry %q", ll.Seq, ll.EntryID)
		}
		l.entries[e.ID] = e
		l.byAccount[e.DebitAccount] = append(l.byAccount[e.DebitAccount], e.ID)
		l.byAccount[e.CreditAccount] = append(l.byAccount[e.CreditAccount], e.ID)
		// The external-reference index is rebuilt the same way
		// commitEntryLocked maintains it: the journaled rows are the
		// source of truth, so the index is derived from them rather
		// than exported separately.
		if e.ExternalRef != "" {
			l.byRef[e.ExternalRef] = e.ID
		}
		debitKey := accountCurrency{account: e.DebitAccount, currency: e.Currency}
		creditKey := accountCurrency{account: e.CreditAccount, currency: e.Currency}
		l.balances[debitKey] += e.AmountCents
		l.balances[creditKey] -= e.AmountCents
		l.debitTotals[debitKey] += e.AmountCents
		l.creditTotals[creditKey] += e.AmountCents

		prevRaw, err := hex.DecodeString(ll.PrevHash)
		if err != nil {
			return fail("link seq %d: bad prev_hash hex: %v", ll.Seq, err)
		}
		hashRaw, err := hex.DecodeString(ll.Hash)
		if err != nil {
			return fail("link seq %d: bad hash hex: %v", ll.Seq, err)
		}
		var prev, h [32]byte
		copy(prev[:], prevRaw)
		copy(h[:], hashRaw)
		l.chain = append(l.chain, chainLink{seq: ll.Seq, entryID: ll.EntryID, prevHash: prev, hash: h})
	}
	l.version = meta.Version

	// Holds, rebuilt in export order.
	holdList := make([]Hold, 0, len(holds))
	for _, h := range holds {
		holdList = append(holdList, h)
	}
	sort.Slice(holdList, func(i, j int) bool {
		if !holdList[i].CreatedAt.Equal(holdList[j].CreatedAt) {
			return holdList[i].CreatedAt.Before(holdList[j].CreatedAt)
		}
		return holdList[i].ID < holdList[j].ID
	})
	for _, h := range holdList {
		l.holds[h.ID] = h
		l.holdsByAccount[h.Account] = append(l.holdsByAccount[h.Account], h.ID)
	}

	// Term deposits, rebuilt in export (ID) order. The interest rows are
	// ordinary journal entries and were already folded above; the
	// deposit records and their idempotency index restore here.
	tdIDs := make([]string, 0, len(termDeposits))
	for id := range termDeposits {
		tdIDs = append(tdIDs, id)
	}
	sort.Strings(tdIDs)
	for _, id := range tdIDs {
		l.termDeposits[id] = termDeposits[id]
	}
	for k, id := range termDepositKeys {
		l.termDepositKeys[k] = id
	}

	// Transfer schedules, rebuilt in export (ID) order. The run history
	// is operational and intentionally not restored — the definition plus
	// RunSeq/NextRunAt/Status is what keeps the plans firing. The
	// scheduleKeys index is rebuilt from the idempotency keys so creation
	// idempotency survives the restore.
	scheduleIDs := make([]string, 0, len(schedules))
	for id := range schedules {
		scheduleIDs = append(scheduleIDs, id)
	}
	sort.Strings(scheduleIDs)
	for _, id := range scheduleIDs {
		s := schedules[id]
		rec := s
		l.schedules[id] = &rec
		if s.IdempotencyKey != "" {
			if _, dup := l.scheduleKeys[s.IdempotencyKey]; dup {
				return fail("duplicate schedule idempotency key %q", s.IdempotencyKey)
			}
			l.scheduleKeys[s.IdempotencyKey] = id
		}
	}

	// Transfer dual-control reviews, rebuilt in export (transfer-ID)
	// order. Approved reviews' legs already folded in with the chain
	// above — the record only restores the registry (status, reviewer,
	// decision time); their entry IDs are checked referentially.
	// Pending, rejected, and expired reviews' legs have no chain links,
	// so the review record is their only export: their rows are
	// restored into the journal indexes (marked, never into
	// balances/totals — they were never effective).
	reviewIDs := make([]string, 0, len(reviews))
	for id := range reviews {
		reviewIDs = append(reviewIDs, id)
	}
	sort.Strings(reviewIDs)
	for _, id := range reviewIDs {
		r := reviews[id]
		for _, e := range r.Entries {
			if _, ok := l.entries[e.ID]; ok {
				if r.Status == ReviewStatusApproved {
					continue
				}
				return fail("review %q entry %q collides with an existing journal entry", id, e.ID)
			}
			if r.Status == ReviewStatusApproved {
				return fail("review %q entry %q missing from the journal fold", id, e.ID)
			}
			l.entries[e.ID] = e
			l.byAccount[e.DebitAccount] = append(l.byAccount[e.DebitAccount], e.ID)
			l.byAccount[e.CreditAccount] = append(l.byAccount[e.CreditAccount], e.ID)
			// Review-frozen rows claim their references at freeze
			// time (see commitReviewEntryLocked), so the restore
			// rebuilds them here too.
			if e.ExternalRef != "" {
				l.byRef[e.ExternalRef] = e.ID
			}
		}
		l.reviews[id] = r
	}

	// Idempotency registries, with referential checks: a registry row that
	// points at a journal row that does not exist (or at a journal row it
	// no longer matches) is corruption, not a warning.
	for k, e := range byKey {
		j, ok := l.entries[e.ID]
		if !ok {
			return fail("idempotency key %q references missing journal entry %q", k, e.ID)
		}
		if !journalEntriesEqual(j, e) {
			return fail("idempotency key %q registry entry differs from journal entry %q", k, e.ID)
		}
		l.byKey[k] = e
	}
	for k, ids := range transferKeys {
		for _, id := range ids {
			if _, ok := l.entries[id]; !ok {
				return fail("transfer idempotency key %q references missing entry %q", k, id)
			}
		}
		l.transferKeys[k] = ids
	}
	for k, id := range holdKeys {
		if _, ok := l.holds[id]; !ok {
			return fail("hold idempotency key %q references missing hold %q", k, id)
		}
		l.holdKeys[k] = id
	}
	for k, rc := range captureKeys {
		if _, ok := l.holds[rc.HoldID]; !ok {
			return fail("capture idempotency key %q references missing hold %q", k, rc.HoldID)
		}
		if _, ok := l.entries[rc.Entry.ID]; !ok {
			return fail("capture idempotency key %q references missing entry %q", k, rc.Entry.ID)
		}
		l.captureKeys[k] = rc
	}
	for k, rec := range sweepKeys {
		for _, id := range rec.entryIDs {
			if _, ok := l.entries[id]; !ok {
				return fail("sweep idempotency key %q references missing entry %q", k, id)
			}
		}
		l.sweepKeys[k] = rec
	}
	for id, rec := range merges {
		for _, eid := range rec.entryIDs {
			if _, ok := l.entries[eid]; !ok {
				return fail("merge %q references missing entry %q", id, eid)
			}
		}
		l.merges[id] = rec
	}
	for k, mergeID := range mergeKeys {
		if _, ok := merges[mergeID]; !ok {
			return fail("merge idempotency key %q references missing merge %q", k, mergeID)
		}
		l.mergeKeys[k] = mergeID
	}
	for k, rec := range batchKeys {
		for _, id := range rec.entryIDs {
			if _, ok := l.entries[id]; !ok {
				return fail("batch idempotency key %q references missing entry %q", k, id)
			}
		}
		l.batchKeys[k] = rec
	}
	for k, rec := range allocKeys {
		for _, id := range rec.entryIDs {
			if _, ok := l.entries[id]; !ok {
				return fail("allocation idempotency key %q references missing entry %q", k, id)
			}
		}
		l.allocKeys[k] = rec
	}

	// Operational config.
	if err := l.applySnapshotConfigLocked(cfg); err != nil {
		return fail("%v", err)
	}

	// The whole point: the rebuilt ledger must prove its own integrity.
	// A rewritten amount, a deleted or reordered entry, or a spliced link
	// fails here and the import is rejected — no half-restored ledger is
	// ever returned.
	if err := l.verifyChainLocked(); err != nil {
		return nil, fmt.Errorf("%w: audit chain verification failed: %v", ErrSnapshotInvalid, err)
	}

	// LG-78: when the snapshot header carries a balance Merkle root,
	// the refolded balances must recompute to exactly that root. The
	// journal fold and chain verification above already proved the
	// journal is intact; this proves the committed balance sheet
	// matches the header's commitment too — a tampered merkle_root
	// rejects the import. Snapshots written before LG-78 carry no root
	// and skip this check.
	if meta.MerkleRoot != "" {
		if got := l.merkleTreeLocked().RootHex(); got != meta.MerkleRoot {
			return fail("snapshot balance merkle root mismatch: header commits to %s, refolded balances recompute to %s",
				meta.MerkleRoot, got)
		}
	}
	return l, nil
}

// applySnapshotConfigLocked installs a snapshot's operational config on
// the ledger: frozen and overdraft-protected accounts, the sub-account
// hierarchy, the transfer fee policy, the idempotency TTL, daily outflow
// limits, monthly spend budgets, and the FX rate table. It validates every row before applying
// anything: invalid config is an error and leaves the ledger untouched —
// a restore must never silently drop a risk control or convert at a bad
// rate. Callers must hold the write lock. For a full restore the ledger
// starts empty, so installation is a plain overwrite; for an incremental
// import the same config section carries the latest config, applied the
// same way.
func (l *Ledger) applySnapshotConfigLocked(cfg *snapshotConfigLine) error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf(format, args...)
	}
	// Validate first: nothing is applied until every row checks out.
	type validatedLimit struct {
		key   dailyLimitKey
		limit int64
	}
	var limits []validatedLimit
	for _, dl := range cfg.DailyLimits {
		if dl.Account == "" {
			return bad("daily limit with empty account")
		}
		cur, err := normalizeCurrency(dl.Currency)
		if err != nil {
			return bad("bad daily limit currency %q: %v", dl.Currency, err)
		}
		if dl.LimitCents < 0 {
			return bad("negative daily limit for account %q (%s)", dl.Account, dl.Currency)
		}
		limits = append(limits, validatedLimit{dailyLimitKey{account: dl.Account, currency: cur}, dl.LimitCents})
	}
	type validatedLargeTxThreshold struct {
		currency  string
		threshold int64
	}
	var largeTxThresholds []validatedLargeTxThreshold
	for _, t := range cfg.LargeTxThresholds {
		cur, err := normalizeCurrency(t.Currency)
		if err != nil {
			return bad("bad large-transaction threshold currency %q: %v", t.Currency, err)
		}
		if t.ThresholdCents <= 0 {
			return bad("non-positive large-transaction threshold for %s", cur)
		}
		largeTxThresholds = append(largeTxThresholds, validatedLargeTxThreshold{cur, t.ThresholdCents})
	}
	reportCurrency := DefaultCurrency
	if cfg.LargeTxReportCurrency != "" {
		cur, err := normalizeCurrency(cfg.LargeTxReportCurrency)
		if err != nil {
			return bad("bad large-transaction report currency %q: %v", cfg.LargeTxReportCurrency, err)
		}
		reportCurrency = cur
	}
	type validatedBudget struct {
		key    budgetKey
		budget int64
	}
	var budgetRows []validatedBudget
	for _, b := range cfg.Budgets {
		if b.Account == "" {
			return bad("monthly budget with empty account")
		}
		cur, err := normalizeCurrency(b.Currency)
		if err != nil {
			return bad("bad monthly budget currency %q: %v", b.Currency, err)
		}
		if b.BudgetCents < 0 {
			return bad("negative monthly budget for account %q (%s)", b.Account, b.Currency)
		}
		budgetRows = append(budgetRows, validatedBudget{budgetKey{account: b.Account, currency: cur}, b.BudgetCents})
	}
	var ttl time.Duration
	ttlSet := false
	if cfg.IdempotencyTTL != "" {
		var err error
		ttl, err = time.ParseDuration(cfg.IdempotencyTTL)
		if err != nil {
			return bad("bad idempotency_ttl %q: %v", cfg.IdempotencyTTL, err)
		}
		ttlSet = true
	}
	fx := make(map[fxPair]ExchangeRate, len(cfg.FXRates))
	for _, r := range cfg.FXRates {
		f, t, err := normalizeFXRate(r.FromCurrency, r.ToCurrency, r.Num, r.Den)
		if err != nil {
			return bad("bad FX rate %s->%s: %v", r.FromCurrency, r.ToCurrency, err)
		}
		fx[fxPair{from: f, to: t}] = ExchangeRate{
			FromCurrency:     f,
			ToCurrency:       t,
			Num:              r.Num,
			Den:              r.Den,
			EffectiveVersion: r.EffectiveVersion,
			ExpiresAt:        r.ExpiresAt,
			// LG-93: the two-sided dealing prices round-trip like the
			// mid — a restored ledger deals at the same bid/ask the
			// original did.
			BidNum: r.BidNum,
			BidDen: r.BidDen,
			AskNum: r.AskNum,
			AskDen: r.AskDen,
		}
	}
	// FX snapshot history (LG-45): snapshots present require a nonzero
	// declared version; each snapshot's version must be unique and
	// within 1..FXSnapshotVersion, and its rates must validate like the
	// table itself. Gaps are deliberately NOT rejected — disaster
	// recovery fail-opens, and Reconcile's continuity check reports the
	// missing versions instead of blocking the restore.
	if len(cfg.FXRateTableSnapshots) > 0 && cfg.FXSnapshotVersion == 0 {
		return bad("fx_rate_table_snapshots present with fx_snapshot_version 0")
	}
	seenFXVersions := make(map[uint64]bool, len(cfg.FXRateTableSnapshots))
	for _, snap := range cfg.FXRateTableSnapshots {
		if snap.Version == 0 || snap.Version > cfg.FXSnapshotVersion {
			return bad("fx snapshot version %d out of range 1..%d", snap.Version, cfg.FXSnapshotVersion)
		}
		if seenFXVersions[snap.Version] {
			return bad("duplicate fx snapshot version %d", snap.Version)
		}
		seenFXVersions[snap.Version] = true
		for _, r := range snap.Rates {
			if _, _, err := normalizeFXRate(r.FromCurrency, r.ToCurrency, r.Num, r.Den); err != nil {
				return bad("bad FX rate %s->%s in snapshot version %d: %v",
					r.FromCurrency, r.ToCurrency, snap.Version, err)
			}
		}
	}
	// Closed periods are validated like every other config row: a
	// malformed period ID in the snapshot fails the import instead of
	// silently unlocking a month.
	for _, id := range cfg.ClosedPeriods {
		if err := validatePeriodID(id); err != nil {
			return bad("bad closed period %q: %v", id, err)
		}
	}
	// Review thresholds are validated like daily limits: a negative
	// threshold or an empty account fails the import instead of
	// silently dropping a risk control.
	for _, rt := range cfg.ReviewThresholds {
		if rt.Account == "" {
			return bad("review threshold with empty account")
		}
		if rt.ThresholdCents < 0 {
			return bad("negative review threshold for account %q", rt.Account)
		}
	}
	// Split-transfer chunking config (LG-75): a negative chunk cap is a
	// corrupt snapshot — fail the import rather than silently running
	// with a different cap.
	if cfg.MaxTransferChunkCents < 0 {
		return bad("negative max_transfer_chunk_cents %d", cfg.MaxTransferChunkCents)
	}
	if cfg.ReviewThresholdCents < 0 {
		return bad("negative global review threshold %d", cfg.ReviewThresholdCents)
	}
	// OFX import registry (LG-66): every record must carry its document
	// SHA-256, and the report inside must agree with it — the registry
	// is the duplicate-import idempotency, and a restore must not
	// silently re-key it. Reports are validated structurally only: the
	// SHA-256 is the identity, and the report is opaque history.
	seenOFXShas := make(map[string]bool, len(cfg.OFXImportRegistry))
	for _, rec := range cfg.OFXImportRegistry {
		if rec.DocumentSHA256 == "" {
			return bad("ofx import record with empty document_sha256")
		}
		if seenOFXShas[rec.DocumentSHA256] {
			return bad("duplicate ofx import record for sha %q", rec.DocumentSHA256)
		}
		seenOFXShas[rec.DocumentSHA256] = true
		if rec.Report.DocumentSHA256 != rec.DocumentSHA256 {
			return bad("ofx import record sha mismatch for %q", rec.DocumentSHA256)
		}
	}
	// Pain.001 export registry (see pain001.go): every record must name
	// a non-empty transfer ID, non-empty payer/payee, a positive
	// amount, and a valid currency — the registry is the payment
	// instruction source, and a restore must not silently re-key it.
	seenPainIDs := make(map[string]bool, len(cfg.PainTransfers))
	for _, pt := range cfg.PainTransfers {
		if pt.TransferID == "" {
			return bad("pain transfer record with empty transfer_id")
		}
		if seenPainIDs[pt.TransferID] {
			return bad("duplicate pain transfer record for %q", pt.TransferID)
		}
		seenPainIDs[pt.TransferID] = true
		if pt.From == "" || pt.To == "" {
			return bad("pain transfer %q with empty from/to", pt.TransferID)
		}
		if pt.AmountCents <= 0 {
			return bad("pain transfer %q with non-positive amount", pt.TransferID)
		}
		if _, err := normalizeCurrency(pt.Currency); err != nil {
			return bad("pain transfer %q with invalid currency %q", pt.TransferID, pt.Currency)
		}
	}
	// IBAN registry (see SetAccountIBAN in pain001.go): every record
	// must name an account and carry a valid IBAN and BIC — a corrupt
	// snapshot must not install an identifier the export would then
	// emit to a bank.
	seenIBANAccounts := make(map[AccountID]bool, len(cfg.AccountIBANs))
	for _, reg := range cfg.AccountIBANs {
		if reg.Account == "" {
			return bad("account iban record with empty account")
		}
		if seenIBANAccounts[reg.Account] {
			return bad("duplicate account iban record for %q", reg.Account)
		}
		seenIBANAccounts[reg.Account] = true
		if !validIBAN(normalizeIBAN(reg.IBAN)) {
			return bad("account iban record for %q with invalid iban", reg.Account)
		}
		if !validBIC(reg.BIC) {
			return bad("account iban record for %q with invalid bic", reg.Account)
		}
	}
	// Virtual-account mappings (see RegisterVirtualAccount in
	// virtual_account.go): validated like every other config row — an
	// empty VAN or parent, a self-mapping, or a chained mapping fails
	// the import instead of installing a mapping that could never have
	// been created.
	seenVANs := make(map[AccountID]bool, len(cfg.VirtualAccounts))
	vanParents := make(map[AccountID]AccountID, len(cfg.VirtualAccounts))
	for _, va := range cfg.VirtualAccounts {
		if va.VAN == "" {
			return bad("virtual account record with empty van")
		}
		if va.Parent == "" {
			return bad("virtual account %q with empty parent", va.VAN)
		}
		if va.VAN == va.Parent {
			return bad("virtual account %q mapped to itself", va.VAN)
		}
		if seenVANs[va.VAN] {
			return bad("duplicate virtual account record for %q", va.VAN)
		}
		seenVANs[va.VAN] = true
		vanParents[va.VAN] = va.Parent
	}
	for van, parent := range vanParents {
		if vanParents[parent] != "" {
			return bad("virtual account %q chains through VAN parent %q", van, parent)
		}
		for v2, p2 := range vanParents {
			if p2 == van {
				return bad("virtual account %q already parents VAN %q", van, v2)
			}
		}
	}
	var reviewExpiry time.Duration
	if cfg.ReviewExpiry != "" {
		var err error
		reviewExpiry, err = time.ParseDuration(cfg.ReviewExpiry)
		if err != nil || reviewExpiry < 0 {
			return bad("bad review_expiry %q: %v", cfg.ReviewExpiry, err)
		}
	}

	// Settlement channel mappings are validated like every other config
	// row: an empty account or an over-long channel label fails the
	// import instead of silently dropping the channel dimension.
	for _, sc := range cfg.SettlementChannels {
		if sc.Account == "" {
			return bad("settlement channel with empty account")
		}
		if len(sc.Channel) > maxSettlementChannelLen {
			return bad("settlement channel label too long for account %q", sc.Account)
		}
	}

	// Journal rules are validated like every other config row: an illegal
	// rule in the snapshot fails the import instead of silently installing
	// a rule that could never have been created through SetJournalRule.
	// Match currencies are normalized to their canonical form, like
	// SetJournalRule does.
	validatedRules := make([]JournalRule, 0, len(cfg.JournalRules))
	for _, r := range cfg.JournalRules {
		if err := ValidateJournalRule(r); err != nil {
			return bad("bad journal rule %q: %v", r.ID, err)
		}
		legs := make([]RuleLeg, len(r.Legs))
		copy(legs, r.Legs)
		r.Legs = legs
		if r.MatchCurrency != "" {
			cur, err := normalizeCurrency(r.MatchCurrency)
			if err != nil {
				return bad("bad journal rule %q currency %q: %v", r.ID, r.MatchCurrency, err)
			}
			r.MatchCurrency = cur
		}
		validatedRules = append(validatedRules, r)
	}

	// Cost allocation rules are validated like every other config row:
	// an illegal rule in the snapshot fails the import instead of
	// silently installing a rule that could never have been created
	// through SetAllocationRule.
	validatedAllocRules := make([]AllocationRule, 0, len(cfg.AllocationRules))
	seenAllocRuleIDs := make(map[string]bool, len(cfg.AllocationRules))
	for _, r := range cfg.AllocationRules {
		if err := ValidateAllocationRule(r); err != nil {
			return bad("bad allocation rule %q: %v", r.ID, err)
		}
		if seenAllocRuleIDs[r.ID] {
			return bad("duplicate allocation rule %q", r.ID)
		}
		seenAllocRuleIDs[r.ID] = true
		targets := make([]AllocationTarget, len(r.Targets))
		copy(targets, r.Targets)
		r.Targets = targets
		validatedAllocRules = append(validatedAllocRules, r)
	}

	// Revaluation baselines are validated like every other config row: an
	// empty account, a bad currency, or a malformed rate fails the import
	// instead of silently installing a baseline the next revaluation
	// would misread. Currencies are normalized to canonical form.
	validatedReval := make([]RevaluationPosition, 0, len(cfg.RevalBaselines))
	seenRevalKeys := make(map[revalKey]bool, len(cfg.RevalBaselines))
	for _, rp := range cfg.RevalBaselines {
		if rp.Account == "" {
			return bad("revaluation baseline with empty account")
		}
		cur, err := normalizeCurrency(rp.Currency)
		if err != nil {
			return bad("bad revaluation baseline currency %q: %v", rp.Currency, err)
		}
		base, err := normalizeCurrency(rp.BaseCurrency)
		if err != nil {
			return bad("bad revaluation baseline base currency %q: %v", rp.BaseCurrency, err)
		}
		if _, _, err := normalizeFXRate(cur, base, rp.RateNum, rp.RateDen); err != nil {
			return bad("bad revaluation baseline rate %s->%s: %v", cur, base, err)
		}
		k := revalKey{account: rp.Account, currency: cur, base: base}
		if seenRevalKeys[k] {
			return bad("duplicate revaluation baseline for account %q (%s->%s)", rp.Account, cur, base)
		}
		seenRevalKeys[k] = true
		rp.Currency = cur
		rp.BaseCurrency = base
		validatedReval = append(validatedReval, rp)
	}

	// Apply: every row above checked out.
	for _, a := range cfg.Frozen {
		l.frozen[a] = true
	}
	// Dormancy markings restore like the frozen list: an empty account
	// fails the import instead of installing a mark that could never
	// have been created. The record is restored verbatim, including the
	// original MarkedAt and LastActivity — a restore must not move an
	// account's dormancy timestamps.
	for _, d := range cfg.Dormant {
		if d.Account == "" {
			return bad("dormancy record with empty account")
		}
		l.dormant[d.Account] = d
	}
	// Account closures restore like the frozen list: an empty account
	// fails the import instead of installing a closure that could never
	// have been created. The closure record is restored verbatim,
	// including the original ClosedAt — a restore must not move an
	// account's terminal timestamp.
	for _, c := range cfg.ClosedAccounts {
		if c.Account == "" {
			return bad("account closure with empty account")
		}
		l.closed[c.Account] = c
	}
	// Timed-freeze deadlines restore like the frozen list: an empty
	// account fails the import instead of installing a deadline that
	// could never have been created. The account is marked frozen as
	// well, in case a deadline row names an account the Frozen list
	// missed (a restore must not leave a deadline without its freeze).
	for _, fu := range cfg.FrozenAutoUnfreeze {
		if fu.Account == "" {
			return bad("frozen auto-unfreeze deadline with empty account")
		}
		l.frozen[fu.Account] = true
		l.frozenUntil[fu.Account] = fu.AutoUnfreezeAt
	}
	for _, a := range cfg.OverdraftProtected {
		l.noOverdraft[a] = true
	}
	// Chart-of-accounts registrations restore like the frozen list: an
	// empty account or an invalid type fails the import instead of
	// installing a registration that could never have been created.
	for _, reg := range cfg.AccountTypes {
		if reg.Account == "" {
			return bad("chart-of-accounts registration with empty account")
		}
		if !validAccountType(reg.Type) {
			return bad("chart-of-accounts registration with invalid type %q", reg.Type)
		}
		l.chart[reg.Account] = reg.Type
	}
	// Account tags restore like the chart-of-accounts registrations: an
	// empty account or a malformed key/value fails the import instead of
	// installing tags that could never have been created. The nil-map
	// case (an account with an empty tag set) is skipped — an empty
	// set clears, so it has no representation in the map index.
	for _, row := range cfg.AccountTags {
		if row.Account == "" {
			return bad("account tags with empty account")
		}
		if row.Tags == nil {
			row.Tags = map[string]string{}
		}
		if err := checkAccountTags(row.Tags); err != nil {
			return bad("account tags for %s: %v", row.Account, err)
		}
		if len(row.Tags) == 0 {
			continue
		}
		l.tags[row.Account] = row.Tags
	}
	for c, p := range cfg.Parents {
		l.parents[c] = p
	}
	for _, vl := range limits {
		l.dailyLimits[vl.key] = vl.limit
	}
	// Large-transaction reporting thresholds restore like the daily
	// outflow limits. Generated report rows are deliberately not
	// restored: the audit chain already carries every generated report,
	// and a restore must not replay them into the chain.
	for _, vt := range largeTxThresholds {
		l.largeTxThresholds[vt.currency] = vt.threshold
	}
	l.largeTxReportCurrency = reportCurrency
	// The business-day calendar restores verbatim — table and version —
	// so a restored ledger runs the exact holidays the original did.
	// Absent in snapshots written before business-day support existed,
	// which keep the default Saturday/Sunday calendar.
	if cfg.BusinessDayCalendar != nil {
		if err := validateBusinessDayCalendarSnapshot(cfg.BusinessDayCalendar); err != nil {
			return bad("invalid business-day calendar: %v", err)
		}
		l.businessCalendar = *cfg.BusinessDayCalendar
		l.accrualBusinessDaysOnly = cfg.AccrualBusinessDaysOnly
	}
	// Monthly spend budgets restore like the daily outflow limits: the
	// accumulated per-month spend counters are deliberately not
	// restored (fail-open for the remainder of the month), only the
	// configured budgets.
	for _, vb := range budgetRows {
		l.budgets[vb.key] = vb.budget
	}
	// Sanctions/denylist rows restore like the frozen-account list: a
	// restore that dropped them would silently lift screening. Rows are
	// validated — an empty value fails the import.
	for i, de := range cfg.Denylist {
		if de.Value == "" {
			return bad("denylist entry %d has an empty value", i)
		}
		if de.Prefix {
			l.deniedPrefixes[de.Value] = de
		} else {
			l.deniedExact[AccountID(de.Value)] = de
		}
	}
	// Settlement channel mappings: restore the channel dimension.
	for _, sc := range cfg.SettlementChannels {
		l.settlementChannels[sc.Account] = sc.Channel
	}
	// Journal rules restore like the other structural config: the rule
	// set and its mutation counter round-trip together, so the restored
	// ledger carries the same rule-set generation as the source.
	l.journalRules = make(map[string]JournalRule, len(validatedRules))
	for _, r := range validatedRules {
		l.journalRules[r.ID] = r
	}
	l.journalRuleVersion = cfg.JournalRuleVersion
	// Cost allocation rules restore like the other structural config:
	// the rule set and its mutation counter round-trip together.
	l.allocRules = make(map[string]AllocationRule, len(validatedAllocRules))
	for _, r := range validatedAllocRules {
		l.allocRules[r.ID] = r
	}
	l.allocRuleVersion = cfg.AllocationRuleVersion
	// Balance threshold watches are validated like daily limits: an
	// empty account or a bad currency fails the import instead of
	// silently dropping an alert band. Negative levels are legal (a
	// balance level, not an outflow); low must be below high when both
	// are set. Snapshots written before LG-61 carry the legacy
	// low_balance_thresholds list instead: those import as low-only
	// watches, so old snapshots keep their alerts.
	for _, w := range cfg.BalanceWatches {
		if w.Account == "" {
			return bad("balance watch with empty account")
		}
		cur, err := normalizeCurrency(w.Currency)
		if err != nil {
			return bad("bad balance watch currency %q: %v", w.Currency, err)
		}
		if err := checkBalanceWatchConfig(BalanceWatchConfig{LowCents: w.LowCents, HighCents: w.HighCents}); err != nil {
			return bad("bad balance watch for %s/%s: %v", w.Account, cur, err)
		}
		l.balanceWatches[balanceWatchKey{account: w.Account, currency: cur}] = BalanceWatch{
			Account:   w.Account,
			Currency:  cur,
			LowCents:  w.LowCents,
			HighCents: w.HighCents,
		}
	}
	for _, lt := range cfg.LowBalanceThresholds {
		if lt.Account == "" {
			return bad("low-balance threshold with empty account")
		}
		cur, err := normalizeCurrency(lt.Currency)
		if err != nil {
			return bad("bad low-balance threshold currency %q: %v", lt.Currency, err)
		}
		key := balanceWatchKey{account: lt.Account, currency: cur}
		if _, ok := l.balanceWatches[key]; ok {
			continue // the new-form watch wins on conflict
		}
		l.balanceWatches[key] = BalanceWatch{
			Account:   lt.Account,
			Currency:  cur,
			LowCents:  int64ptr(lt.ThresholdCents),
			HighCents: nil,
		}
	}
	// The transfer fee policy: prefer the tiered schedule when present;
	// otherwise rebuild the flat single-tier policy from the legacy
	// fee_rate_bps field (snapshots predating tiered fees). An empty
	// revenue account means the policy was disabled.
	if len(cfg.FeeTiers) > 0 {
		l.feeTiers = cfg.FeeTiers
	} else if cfg.FeeRevenueAccount != "" {
		l.feeTiers = []FeeTier{{MinAmountCents: 0, RateBps: cfg.FeeRateBps}}
	}
	l.feeRevenueAccount = cfg.FeeRevenueAccount
	// Term-deposit and credit-line config restores like the other
	// structural config: the fail-closed interest accounts (an absent
	// account keeps accruals rejected) and the credit-line registry. A
	// restore that dropped the credit lines would silently lift the
	// -limit floor back to zero, so they round-trip like the
	// overdraft-protection list.
	l.tdInterestExpense = cfg.TdInterestExpense
	l.creditInterestIncome = cfg.CreditInterestIncome
	l.creditLines = make(map[creditLineKey]CreditLine, len(cfg.CreditLines))
	for _, cl := range cfg.CreditLines {
		l.creditLines[creditLineKey{account: cl.Account, currency: cl.Currency}] = cl
	}
	// ECL config restores like the other structural config: the staging
	// configuration (validated — a corrupt snapshot must not install an
	// inverted boundary), the fail-closed booking accounts (an absent
	// account keeps provisioning rejected), and the exposure registry
	// with its provisioned-so-far coverage figures. hasManual is
	// recovered from the manual amount: the registry only ever stores
	// positive manual exposures.
	if cfg.ECLStageConfig != nil {
		if err := cfg.ECLStageConfig.Validate(); err != nil {
			return bad("bad ECL stage config: %v", err)
		}
		l.eclStageConfig = *cfg.ECLStageConfig
	}
	l.eclExpenseAccount = cfg.ECLExpenseAccount
	l.eclAllowanceAccount = cfg.ECLAllowanceAccount
	l.eclExposures = make(map[eclExposureKey]eclExposureRow, len(cfg.ECLExposures))
	for _, rec := range cfg.ECLExposures {
		l.eclExposures[eclExposureKey{account: rec.Account, currency: rec.Currency}] = eclExposureRow{
			manualCents:      rec.ManualCents,
			hasManual:        rec.ManualCents > 0,
			daysPastDue:      rec.DaysPastDue,
			stage:            rec.Stage,
			provisionedCents: rec.ProvisionedCents,
			updatedAt:        rec.UpdatedAt,
		}
	}
	// Split-transfer chunking config (LG-75), validated above.
	l.maxTransferChunkCents = cfg.MaxTransferChunkCents
	l.splitFeePerChunk = cfg.SplitFeePerChunk
	if ttlSet {
		l.idempotencyTTL = ttl
	}
	l.fxRates = fx
	l.fxAccount = cfg.FXAccount
	l.fxIncomeAccount = cfg.FXIncomeAccount
	// FX snapshot history (LG-45): restore the version counter and the
	// per-version tables. Legacy snapshots predate versioning — rebuild
	// version 1 from the imported table when it is non-empty, so the
	// restored ledger's history starts honest (a populated table at
	// version 0 would be a lie) and transfers keep a reproducible table.
	l.fxSnapshotVersion = cfg.FXSnapshotVersion
	l.fxRateSnapshots = make(map[uint64]FXRateTableSnapshot, len(cfg.FXRateTableSnapshots))
	for _, snap := range cfg.FXRateTableSnapshots {
		rates := make([]ExchangeRate, len(snap.Rates))
		copy(rates, snap.Rates)
		snap.Rates = rates
		l.fxRateSnapshots[snap.Version] = snap
	}
	if len(cfg.FXRateTableSnapshots) == 0 && len(fx) > 0 {
		rates := make([]ExchangeRate, 0, len(fx))
		for _, r := range fx {
			rates = append(rates, r)
		}
		sort.Slice(rates, func(i, j int) bool {
			if rates[i].FromCurrency != rates[j].FromCurrency {
				return rates[i].FromCurrency < rates[j].FromCurrency
			}
			return rates[i].ToCurrency < rates[j].ToCurrency
		})
		l.fxSnapshotVersion = 1
		l.fxRateSnapshots[1] = FXRateTableSnapshot{
			Version: 1,
			TakenAt: time.Now().UTC(),
			Change:  "imported",
			Rates:   rates,
		}
	}
	// Closed periods are additive like the frozen-account list: a period
	// locked in the snapshot must stay locked after the restore.
	for _, id := range cfg.ClosedPeriods {
		l.closedPeriods[id] = true
	}
	// The period-closing account mapping restores like the other
	// structural config: a restore that dropped it would silently
	// disable period-closing entries. Snapshots written before closing
	// support carry neither field, which restores as "not configured".
	if cfg.ClosingRetainedEarnings != "" {
		for _, a := range cfg.ClosingPLAccounts {
			l.closingPL[a] = true
		}
		l.closingRetained = cfg.ClosingRetainedEarnings
	}
	// Review thresholds restore like the other risk controls: a restore
	// that dropped them would silently stop reviewing large transfers.
	for _, rt := range cfg.ReviewThresholds {
		l.reviewThresholds[rt.Account] = rt.ThresholdCents
	}
	l.reviewThreshold = cfg.ReviewThresholdCents
	l.reviewExpiry = reviewExpiry
	// The revaluation P&L account and baselines restore like the other
	// structural config: a restore that dropped the baselines would
	// re-book full carrying values as gains on the next revaluation.
	l.revalAccount = cfg.RevalAccount
	l.revalBaselines = make(map[revalKey]revalBaseline, len(validatedReval))
	for _, rp := range validatedReval {
		l.revalBaselines[revalKey{account: rp.Account, currency: rp.Currency, base: rp.BaseCurrency}] = revalBaseline{
			foreignBalance: rp.ForeignBalance,
			baseValue:      rp.BaseValue,
			rateNum:        rp.RateNum,
			rateDen:        rp.RateDen,
			rateVersion:    rp.RateVersion,
			revaluedAt:     rp.RevaluedAt,
		}
	}
	// The OFX import registry restores like the idempotency indexes: a
	// restore that dropped it would re-import an already imported
	// statement as new. Snapshots written before OFX import existed
	// carry no records, which restores as an empty registry.
	l.ofxImports = make(map[string]*OFXImportReport, len(cfg.OFXImportRegistry))
	for _, rec := range cfg.OFXImportRegistry {
		rep := rec.Report
		l.ofxImports[rec.DocumentSHA256] = &rep
	}
	// The pain.001 export registry restores like the idempotency
	// indexes: a restore that dropped it would silently un-export
	// pre-restore transfers. Validated above; installed verbatim.
	l.painTransfers = make(map[string]PainTransfer, len(cfg.PainTransfers))
	for _, pt := range cfg.PainTransfers {
		l.painTransfers[pt.TransferID] = pt
	}
	// The IBAN registry restores like the account tags: validated
	// above, installed in canonical form so the export names exactly
	// the identifiers the snapshot carried.
	l.accountIBANs = make(map[AccountID]AccountIBAN, len(cfg.AccountIBANs))
	for _, reg := range cfg.AccountIBANs {
		reg.IBAN = normalizeIBAN(reg.IBAN)
		reg.BIC = strings.ToUpper(reg.BIC)
		l.accountIBANs[reg.Account] = reg
	}
	// The VAN mappings restore like the frozen-account list: validated
	// above, installed verbatim, with the mapping version counter so a
	// restored ledger carries the same mapping generation as the
	// source. Snapshots written before virtual accounts existed carry
	// no records, which restores as an empty mapping at version 0.
	l.virtualAccounts = make(map[AccountID]AccountID, len(cfg.VirtualAccounts))
	for _, va := range cfg.VirtualAccounts {
		l.virtualAccounts[va.VAN] = va.Parent
	}
	l.virtualAccountVersion = cfg.VirtualAccountVersion
	// Re-derive the balance-watch breach state from the restored
	// balances: an account already outside its band counts as breached,
	// so the first posting after the restore does not backfire a stale
	// alert. Silent — a restore emits no events and bumps no counters.
	l.recomputeBalanceWatchBreachesLocked()
	// Re-derive the low-liquidity breach state the same way: an
	// (account, currency) whose restored net position for today is
	// already below its floor counts as breached, so the first
	// evaluation after the restore does not backfire a stale alert.
	// Thresholds written before LG-98 are absent and restore as empty.
	for _, th := range cfg.LiquidityAlertThresholds {
		if th.Account == "" {
			return bad("liquidity alert threshold with empty account")
		}
		cur, err := normalizeCurrency(th.Currency)
		if err != nil {
			return bad("bad liquidity alert threshold currency %q: %v", th.Currency, err)
		}
		l.liquidityThresholds[liquidityThresholdKey{account: th.Account, currency: cur}] = LiquidityAlertThreshold{
			Account:        th.Account,
			Currency:       cur,
			ThresholdCents: th.ThresholdCents,
		}
	}
	l.recomputeLiquidityBreachesLocked()
	return nil
}

// revalPositionsEqual compares two sorted revaluation position lists,
// ignoring time.Location representation (see journalEntriesEqual).
func revalPositionsEqual(a, b []RevaluationPosition) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Account != b[i].Account ||
			a[i].Currency != b[i].Currency ||
			a[i].BaseCurrency != b[i].BaseCurrency ||
			a[i].ForeignBalance != b[i].ForeignBalance ||
			a[i].BaseValue != b[i].BaseValue ||
			a[i].RateNum != b[i].RateNum ||
			a[i].RateDen != b[i].RateDen ||
			a[i].RateVersion != b[i].RateVersion ||
			a[i].RevaluedAt.UnixNano() != b[i].RevaluedAt.UnixNano() {
			return false
		}
	}
	return true
}

// holdsEqual compares two holds field by field, ignoring time.Location
// representation (see journalEntriesEqual).
func holdsEqual(a, b Hold) bool {
	return a.ID == b.ID &&
		a.Account == b.Account &&
		a.AmountCents == b.AmountCents &&
		a.Currency == b.Currency &&
		a.ExpiresAt.UnixNano() == b.ExpiresAt.UnixNano() &&
		a.CreatedAt.UnixNano() == b.CreatedAt.UnixNano() &&
		a.IdempotencyKey == b.IdempotencyKey &&
		a.Status == b.Status
}

// termDepositsEqual compares two term-deposit records field by field,
// ignoring time.Location representation (see holdsEqual).
func termDepositsEqual(a, b TermDeposit) bool {
	return a.ID == b.ID &&
		a.Account == b.Account &&
		a.AmountCents == b.AmountCents &&
		a.Currency == b.Currency &&
		a.RateBps == b.RateBps &&
		a.TermDays == b.TermDays &&
		a.OpenedAt.UnixNano() == b.OpenedAt.UnixNano() &&
		a.MaturesAt.UnixNano() == b.MaturesAt.UnixNano() &&
		a.IdempotencyKey == b.IdempotencyKey &&
		a.Status == b.Status
}

// creditLinesEqual compares two credit-line records field by field,
// ignoring time.Location representation (see holdsEqual).
func creditLinesEqual(a, b CreditLine) bool {
	return a.Account == b.Account &&
		a.Currency == b.Currency &&
		a.LimitCents == b.LimitCents &&
		a.RateBps == b.RateBps &&
		a.GrantedAt.UnixNano() == b.GrantedAt.UnixNano()
}

// eclExposureRowsEqual compares two ECL exposure rows field by field,
// ignoring time.Location representation (see holdsEqual).
func eclExposureRowsEqual(a, b eclExposureRow) bool {
	return a.manualCents == b.manualCents &&
		a.hasManual == b.hasManual &&
		a.daysPastDue == b.daysPastDue &&
		a.stage == b.stage &&
		a.provisionedCents == b.provisionedCents &&
		a.updatedAt.UnixNano() == b.updatedAt.UnixNano()
}

// mergeRecordsEqual compares two merge registry entries field by field,
// ignoring time.Location representation (see journalEntriesEqual).
func mergeRecordsEqual(a, b mergeRecord) bool {
	return a.mergeID == b.mergeID &&
		a.from == b.from &&
		a.to == b.to &&
		reflect.DeepEqual(a.legs, b.legs) &&
		reflect.DeepEqual(a.entryIDs, b.entryIDs) &&
		a.createdAt.UnixNano() == b.createdAt.UnixNano()
}

// schedulesEqual compares two transfer schedules field by field,
// ignoring time.Location representation (see journalEntriesEqual).
func schedulesEqual(a, b *TransferSchedule) bool {
	return a.ID == b.ID &&
		a.From == b.From &&
		a.To == b.To &&
		a.AmountCents == b.AmountCents &&
		a.Currency == b.Currency &&
		a.ToCurrency == b.ToCurrency &&
		a.FXAccount == b.FXAccount &&
		a.FeeCents == b.FeeCents &&
		a.FeeAccount == b.FeeAccount &&
		a.SkipFee == b.SkipFee &&
		a.Memo == b.Memo &&
		a.Interval == b.Interval &&
		a.NextRunAt.UnixNano() == b.NextRunAt.UnixNano() &&
		a.EndsAt.UnixNano() == b.EndsAt.UnixNano() &&
		a.IdempotencyKey == b.IdempotencyKey &&
		a.Status == b.Status &&
		a.RunSeq == b.RunSeq &&
		a.CreatedAt.UnixNano() == b.CreatedAt.UnixNano()
}

// snapshotLedgersEqual is a test helper: it reports whether two ledgers
// carry the same journaled and operational state, for snapshot
// round-trip tests. Time fields are compared by instant (not by
// time.Time's internal representation, which JSON round-trips drop the
// monotonic reading from), and derived maps (balances, totals,
// per-account indexes) are intentionally not compared — they are
// recomputed from the journal on import, so comparing them would only
// re-test the folding code against itself.
func snapshotLedgersEqual(a, b *Ledger) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	b.mu.RLock()
	defer b.mu.RUnlock()

	if a.version != b.version || len(a.chain) != len(b.chain) || len(a.entries) != len(b.entries) {
		return false
	}
	for i, la := range a.chain {
		if la != b.chain[i] {
			return false
		}
	}
	if len(a.byKey) != len(b.byKey) {
		return false
	}
	for id, ea := range a.entries {
		eb, ok := b.entries[id]
		if !ok || !journalEntriesEqual(ea, eb) {
			return false
		}
	}
	for k, ea := range a.byKey {
		eb, ok := b.byKey[k]
		if !ok || !journalEntriesEqual(ea, eb) {
			return false
		}
	}
	if len(a.holds) != len(b.holds) {
		return false
	}
	for id, ha := range a.holds {
		hb, ok := b.holds[id]
		if !ok || !holdsEqual(ha, hb) {
			return false
		}
	}
	if !reflect.DeepEqual(a.holdKeys, b.holdKeys) {
		return false
	}
	if len(a.captureKeys) != len(b.captureKeys) {
		return false
	}
	for k, ra := range a.captureKeys {
		rb, ok := b.captureKeys[k]
		if !ok || ra.CaptureID != rb.CaptureID || ra.HoldID != rb.HoldID ||
			ra.CapturedCents != rb.CapturedCents || ra.ReleasedCents != rb.ReleasedCents ||
			ra.Duplicate != rb.Duplicate || !journalEntriesEqual(ra.Entry, rb.Entry) {
			return false
		}
	}
	if !reflect.DeepEqual(a.transferKeys, b.transferKeys) {
		return false
	}
	if len(a.sweepKeys) != len(b.sweepKeys) {
		return false
	}
	for k, sa := range a.sweepKeys {
		sb, ok := b.sweepKeys[k]
		if !ok || sa.sweepID != sb.sweepID ||
			!reflect.DeepEqual(sa.entryIDs, sb.entryIDs) ||
			sa.createdAt.UnixNano() != sb.createdAt.UnixNano() {
			return false
		}
	}
	if len(a.merges) != len(b.merges) {
		return false
	}
	for id, ma := range a.merges {
		mb, ok := b.merges[id]
		if !ok || ma.mergeID != mb.mergeID || ma.from != mb.from || ma.to != mb.to ||
			!reflect.DeepEqual(ma.legs, mb.legs) ||
			!reflect.DeepEqual(ma.entryIDs, mb.entryIDs) ||
			ma.createdAt.UnixNano() != mb.createdAt.UnixNano() {
			return false
		}
	}
	if !reflect.DeepEqual(a.mergeKeys, b.mergeKeys) {
		return false
	}
	if len(a.schedules) != len(b.schedules) {
		return false
	}
	for id, sa := range a.schedules {
		sb, ok := b.schedules[id]
		if !ok || !schedulesEqual(sa, sb) {
			return false
		}
	}
	if !reflect.DeepEqual(a.scheduleKeys, b.scheduleKeys) {
		return false
	}
	if !reflect.DeepEqual(a.frozen, b.frozen) {
		return false
	}
	if !frozenUntilEqual(a.frozenUntil, b.frozenUntil) {
		return false
	}
	if !reflect.DeepEqual(a.noOverdraft, b.noOverdraft) {
		return false
	}
	// Chart-of-accounts registrations must round-trip like the other
	// structural config: a restore that dropped them would silently
	// lift the posting-direction control.
	if !reflect.DeepEqual(a.chart, b.chart) {
		return false
	}
	// Account tags must round-trip like the chart registrations: a
	// restore that dropped them would silently reclassify every tagged
	// account.
	if !reflect.DeepEqual(a.tags, b.tags) {
		return false
	}
	if !reflect.DeepEqual(a.parents, b.parents) {
		return false
	}
	if !reflect.DeepEqual(a.dailyLimits, b.dailyLimits) {
		return false
	}
	if !reflect.DeepEqual(a.budgets, b.budgets) {
		return false
	}
	// Balance threshold watches must round-trip; the breach flags are
	// deliberately excluded, like the per-day outflow counters: a
	// restore re-derives breach state from the restored balances (see
	// recomputeBalanceWatchBreachesLocked), and the live alert state may
	// have legitimately moved on since the export.
	if !reflect.DeepEqual(a.balanceWatches, b.balanceWatches) {
		return false
	}
	if !reflect.DeepEqual(a.feeTiers, b.feeTiers) || a.feeRevenueAccount != b.feeRevenueAccount {
		return false
	}
	// Term deposits must round-trip: the deposit records, their
	// idempotency index, and the fail-closed interest-expense account.
	// A restore that dropped a deposit would unlock its principal early.
	// Times compare by UnixNano (see holdsEqual): the JSON round trip
	// strips the monotonic reading and normalizes the location.
	if len(a.termDeposits) != len(b.termDeposits) {
		return false
	}
	for id, td := range a.termDeposits {
		other, ok := b.termDeposits[id]
		if !ok || !termDepositsEqual(td, other) {
			return false
		}
	}
	if !reflect.DeepEqual(a.termDepositKeys, b.termDepositKeys) {
		return false
	}
	if a.tdInterestExpense != b.tdInterestExpense {
		return false
	}
	// Credit lines must round-trip with their fail-closed income
	// account: a restore that dropped the lines would silently lift the
	// -limit floor back to zero.
	if len(a.creditLines) != len(b.creditLines) {
		return false
	}
	for k, cl := range a.creditLines {
		other, ok := b.creditLines[k]
		if !ok || !creditLinesEqual(cl, other) {
			return false
		}
	}
	if a.creditInterestIncome != b.creditInterestIncome {
		return false
	}
	// ECL config must round-trip: the staging configuration, the
	// fail-closed booking accounts, and the exposure registry with
	// its provisioned-so-far coverage figures. A restore that dropped
	// the staging config would re-stage every exposure under the wrong
	// buckets. Times compare by UnixNano (see holdsEqual).
	if a.eclStageConfig != b.eclStageConfig {
		return false
	}
	if a.eclExpenseAccount != b.eclExpenseAccount || a.eclAllowanceAccount != b.eclAllowanceAccount {
		return false
	}
	if len(a.eclExposures) != len(b.eclExposures) {
		return false
	}
	for k, row := range a.eclExposures {
		other, ok := b.eclExposures[k]
		if !ok || !eclExposureRowsEqual(row, other) {
			return false
		}
	}
	// Split-transfer chunking config must round-trip (LG-75).
	if a.maxTransferChunkCents != b.maxTransferChunkCents || a.splitFeePerChunk != b.splitFeePerChunk {
		return false
	}
	// Journal rules must round-trip: the same rule set and the same
	// rule-set mutation counter (see snapshotConfigLine).
	if a.journalRuleVersion != b.journalRuleVersion {
		return false
	}
	if !reflect.DeepEqual(a.journalRules, b.journalRules) {
		return false
	}
	// The period-closing account mapping must round-trip too.
	if a.closingRetained != b.closingRetained {
		return false
	}
	if !reflect.DeepEqual(a.closingPL, b.closingPL) {
		return false
	}
	if a.fxAccount != b.fxAccount {
		return false
	}
	if a.fxIncomeAccount != b.fxIncomeAccount {
		return false
	}
	if !reflect.DeepEqual(a.fxRates, b.fxRates) {
		return false
	}
	// The revaluation account and baselines must round-trip: a restore
	// that dropped them would misprice the next revaluation. Times are
	// compared by instant, like journal entries (see
	// journalEntriesEqual).
	if a.revalAccount != b.revalAccount {
		return false
	}
	if !revalPositionsEqual(a.revaluationPositionsLocked(), b.revaluationPositionsLocked()) {
		return false
	}
	// FX snapshot history must round-trip too (LG-45): same version
	// counter, same per-version tables.
	if !fxSnapshotsEqualLocked(a, b) {
		return false
	}
	// The OFX import registry must round-trip (LG-66): it is the
	// duplicate-import idempotency, and a restore that dropped it would
	// re-import an already imported statement as new.
	if !reflect.DeepEqual(a.ofxImports, b.ofxImports) {
		return false
	}
	if a.idempotencyTTL != b.idempotencyTTL {
		return false
	}
	return true
}
