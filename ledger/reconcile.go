package ledger

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"time"
)

// TrialBalanceDiscrepancy describes one (account, currency) whose net
// balance does not equal its debit totals minus credit totals (the
// per-account, per-currency accounting equation). On a healthy ledger the
// report's discrepancy list is empty; the shape exists so reconciliation
// tooling can distinguish "ran clean" from "did not run".
type TrialBalanceDiscrepancy struct {
	Account           AccountID `json:"account"`
	Currency          string    `json:"currency"`
	TotalDebitsCents  int64     `json:"total_debits_cents"`
	TotalCreditsCents int64     `json:"total_credits_cents"`
	NetBalanceCents   int64     `json:"net_balance_cents"`
	ExpectedNetCents  int64     `json:"expected_net_cents"`
	DifferenceCents   int64     `json:"difference_cents"`
}

// IdempotencyKeyHealth reports the state of the idempotency-key index at
// report time: whether a TTL is configured, and how many keys are older
// than the TTL and thus eligible for a sweep. An unexpired key count that
// keeps growing on a TTL-configured ledger is an operational smell worth
// an operator's attention at end of day.
type IdempotencyKeyHealth struct {
	TTLConfigured   bool   `json:"ttl_configured"`
	TTL             string `json:"ttl"`
	TotalKeys       int    `json:"total_keys"`
	ExpiredEligible int    `json:"expired_eligible"`
}

// AuditChainHealth reports the tamper-evident audit chain at report time:
// whether it recomputes cleanly, the current head, and whether the head is
// consistent with the ledger's own version counter (links == version and
// the newest link's seq == version, which holds under every normal Post:
// idempotent replays append no link and bump no version).
type AuditChainHealth struct {
	VerifyOK         bool   `json:"verify_ok"`
	VerifyError      string `json:"verify_error,omitempty"`
	Head             string `json:"head"`
	Links            uint64 `json:"links"`
	HeadConsistent   bool   `json:"head_consistent"`
	ConsistencyError string `json:"consistency_error,omitempty"`
	// AnchorCheckpoints is the number of external chain-head anchors
	// (see AnchorCheckpoint, LG-44) loaded from checkpoints.jsonl.
	AnchorCheckpoints int `json:"anchor_checkpoints"`
	// AnchorOK reports the external anchor continuity check: every
	// checkpoint's seq strictly increases, its head hash matches the
	// chain link at its seq, and (with a configured key) its signature
	// verifies. Vacuously true with zero checkpoints — no anchor taken
	// yet is not a break. A missing anchor cadence is an operator
	// policy decision, not something the ledger can infer.
	AnchorOK bool `json:"anchor_ok"`
	// AnchorError describes the first anchor verification failure: a
	// reordered/duplicated checkpoint file, a head hash that no longer
	// matches the chain (journal rewritten after anchoring), a forged
	// signature, or an anchor past the chain end (truncated chain).
	AnchorError string `json:"anchor_error,omitempty"`
	// TSAAnchors is the number of RFC 3161 timestamp anchors (LG-84)
	// loaded from tsa_anchors.jsonl.
	TSAAnchors int `json:"tsa_anchors"`
	// TSAAnchorOK reports the TSA anchor check: every stored anchor's
	// token cryptographically verifies and its (seq, headHash) still
	// matches the live chain. Vacuously true with zero anchors. A
	// token whose linkage no longer matches means the journal was
	// rewritten after the TSA countersigned it — the wholesale-rewrite
	// attack this feature exists to detect.
	TSAAnchorOK bool `json:"tsa_anchor_ok"`
	// TSAAnchorError describes the first TSA anchor verification
	// failure: a tampered token, a signer outside the trust anchor, or
	// a (seq, headHash) the live chain no longer attests.
	TSAAnchorError string `json:"tsa_anchor_error,omitempty"`
}

// AccountMerge is one committed account merge (see PostMerge) as reported
// by Reconcile: which source account was consolidated into which target,
// the per-currency legs that moved, and when. Merged sources stay frozen;
// cross-check FromAccount against FrozenAccounts in the same report.
type AccountMerge struct {
	MergeID     string     `json:"merge_id"`
	FromAccount AccountID  `json:"from_account"`
	ToAccount   AccountID  `json:"to_account"`
	Legs        []MergeLeg `json:"legs"`
	CreatedAt   time.Time  `json:"created_at"`
}

// CurrencyHoldTotals is the per-currency rollup of active authorization
// holds (see hold.go) at report time: how many cents are currently
// reserved from available balances, and in how many holds. Currencies are
// never summed together — each currency's reserved funds are reported on
// their own row, sorted by currency code. Expired holds count as inactive
// (expiry is lazy); the rows only cover holds that still reserve funds.
type CurrencyHoldTotals struct {
	Currency    string `json:"currency"`
	HeldCents   int64  `json:"held_cents"`
	ActiveHolds int    `json:"active_holds"`
}

// ReconciliationReport is the end-of-day reconciliation: one consistent,
// read-only snapshot of the whole ledger's health. It answers, in one
// pass, the questions a settlement operator asks at day end: do the books
// balance (accounting equation, per-account and global), is any account
// off from its own trial balance, are idempotency keys accumulating past
// their TTL, and is the audit chain intact and in step with the ledger
// version.
type ReconciliationReport struct {
	GeneratedAt       time.Time `json:"generated_at"`
	Version           uint64    `json:"version"`
	TotalDebitsCents  int64     `json:"total_debits_cents"`
	TotalCreditsCents int64     `json:"total_credits_cents"`
	// CurrencyTotals is the per-currency rollup of the ledger's debit and
	// credit totals, sorted by currency code. The accounting equation
	// (total debits == total credits) holds independently inside each
	// row — currencies are never summed together. TotalDebitsCents and
	// TotalCreditsCents above are the DefaultCurrency row, kept for
	// backward compatibility.
	CurrencyTotals       []CurrencyTotals          `json:"currency_totals"`
	AccountingEquationOK bool                      `json:"accounting_equation_ok"`
	AccountingError      string                    `json:"accounting_error,omitempty"`
	TrialBalances        []TrialBalance            `json:"trial_balances"`
	Discrepancies        []TrialBalanceDiscrepancy `json:"discrepancies"`
	FrozenAccounts       []AccountID               `json:"frozen_accounts"`
	// ClosedAccounts lists the accounts in their terminal lifecycle
	// state (see CloseAccount), sorted: no journal entry may book
	// through any of them anymore, so the report's figures for those
	// accounts are final. Balances, totals, and the audit chain still
	// cover them — closing stops new activity, it never rewrites
	// history.
	ClosedAccounts []AccountID `json:"closed_accounts"`
	// FrozenAccountAutoUnfreeze annotates every timed-frozen account
	// with its scheduled automatic-unfreeze time (see FreezeUntil in
	// freeze.go); manually frozen accounts are absent from the map.
	// Empty when no timed freezes exist.
	FrozenAccountAutoUnfreeze map[AccountID]time.Time `json:"frozen_account_auto_unfreeze_at,omitempty"`
	// OverdraftProtectedAccounts lists the accounts currently guarded
	// against overdrafts (see EnableOverdraftProtection). Risk tooling
	// reads this to know which accounts cannot go negative.
	OverdraftProtectedAccounts []AccountID `json:"overdraft_protected_accounts"`
	// DailyLimits lists every configured daily outflow limit (see
	// SetDailyLimit), sorted by (account, currency). Risk tooling reads
	// this to know which accounts are capped on daily outflow and at
	// what level.
	DailyLimits []DailyLimit `json:"daily_limits"`
	// SubtreeLimitUtilization lists every configured daily outflow
	// limit with its subtree's already-booked outflow for the report's
	// UTC day (LG-80), sorted by (account, currency). A parent's limit
	// constrains its whole subtree's cumulative outflow, so this is the
	// section risk tooling watches for velocity-limit pressure; for a
	// leaf account the subtree is the account itself.
	SubtreeLimitUtilization []SubtreeLimitUsage `json:"subtree_limit_utilization"`
	// LargeTxReports lists the large-transaction (CTR) reports generated
	// for the report's UTC day (see GenerateLargeTxReports), sorted by
	// (account, currency, direction). Empty when no thresholds are
	// configured or no report was generated yet — generation is an
	// explicit operator call (POST /compliance/large-tx-reports); the
	// report discloses what was generated, it never generates.
	LargeTxReports []LargeTxReport `json:"large_tx_reports"`
	// BudgetUtilization lists every configured monthly spend budget
	// (see SetAccountBudget) with the current UTC month's booked spend
	// and its utilization fraction (LG-81), sorted by (account,
	// currency). Booked spend includes spend reserved by pending
	// dual-control reviews. This is the section risk tooling watches
	// for accounting-period spend pressure; the daily limit is the
	// rolling outflow window, the budget is the calendar-month control.
	BudgetUtilization []AccountBudgetUsage `json:"budget_utilization"`
	// BalanceWatches lists every configured balance threshold watch
	// (see SetBalanceWatch), sorted by (account, currency). Operations
	// tooling reads this to know which accounts alert on balance
	// levels and at what band.
	BalanceWatches []BalanceWatch `json:"balance_watches"`
	// AccountTypeTotals aggregates the chart-of-accounts categories
	// (see RegisterAccountType), sorted by (type, currency): the
	// per-category books across every registered account. Risk and
	// compliance tooling reads this for statement-level rollups —
	// total assets, total liabilities, and so on. Empty when no
	// account has a registered type.
	AccountTypeTotals []AccountTypeTotals `json:"account_type_totals"`
	// HeldTotals is the per-currency rollup of active authorization
	// holds (see hold.go) at report time, sorted by currency code: the
	// cents currently reserved from available balances. Expired holds
	// count as inactive even before the ExpireHolds sweep.
	HeldTotals      []CurrencyHoldTotals `json:"held_totals"`
	IdempotencyKeys IdempotencyKeyHealth `json:"idempotency_keys"`
	AuditChain      AuditChainHealth     `json:"audit_chain"`
	// FXRates lists the configured FX conversion rates (see SetFXRate),
	// sorted by (from, to): the conversion table a settlement operator
	// audited against. The FX clearing account's own balances are part
	// of the per-account trial balances above, like any other account.
	FXRates []ExchangeRate `json:"fx_rates"`
	// FXRateVersions reports the FX rate-table snapshot history (LG-45,
	// see fx_snapshot.go): the current snapshot version and whether
	// versions 1..current are all present. A broken continuity means
	// snapshot history was lost — the current table still converts, but
	// some historical transfer's table is no longer reproducible.
	FXRateVersions FXRateVersionReport `json:"fx_rate_versions"`
	// ForwardBook reports the FX forward-contract book (LG-110, see
	// forward.go): how many contracts are booked, settled, expired, or
	// cancelled, with a per-pair breakdown. Expiry is lazy: a booked
	// contract past its settlement grace period counts as expired even
	// before the ExpireForwards sweep persists the transition — the
	// same lazy-expiry convention as the hold totals.
	ForwardBook FXForwardBookReport `json:"forward_book"`
	// Merges lists every committed account merge (see PostMerge), sorted
	// by merge ID: which source account was consolidated into which
	// target, the per-currency legs, and when. Merged sources stay
	// frozen — the frozen_accounts list in this report shows the
	// resulting stops.
	Merges []AccountMerge `json:"merges"`
	// FXSpreadPNL lists the dealer's realized FX spread income (LG-93,
	// see fx_spread.go), summed per settlement currency and sorted by
	// currency: the booked "<transfer ID>/spread" legs, net of their
	// direct reversals. Pending-review spread legs are excluded — the
	// P&L is realized when the transfer settles, not when the
	// dual-control gate freezes it. Empty when no spread ever booked.
	FXSpreadPNL []FXSpreadPNLTotals `json:"fx_spread_pnl"`
	// Dividends lists every declared dividend (see DeclareDividend),
	// sorted by dividend ID: the retained-earnings pool, the record
	// date, the total, and the per-holder payouts recovered from the
	// journaled rows. The amounts are the books' own record — the sum
	// of a dividend's legs always equals its declared total.
	Dividends []DividendReceipt `json:"dividends"`
	// Escheats lists every escheat sweep (see EscheatDormant), sorted by
	// (account, currency, entry): the dormant account, the currency,
	// the amount swept to the escheat account, and whether the sweep was
	// later reversed. Recovered from the journaled rows — the journal is
	// the truth, so the disclosure can never disagree with the books.
	Escheats []EscheatRecord `json:"escheats"`
	// VirtualAccounts lists the van -> parent mappings (see
	// RegisterVirtualAccount), sorted by VAN, and VirtualAccountVersion
	// is the mapping mutation counter. Risk tooling reads this to know
	// which identifiers attribute to which real accounts.
	VirtualAccounts       []VirtualAccount `json:"virtual_accounts"`
	VirtualAccountVersion uint64           `json:"virtual_account_version"`
	// ClosedPeriods lists the currently locked accounting periods
	// ("YYYY-MM", UTC months; see ClosePeriod), sorted: no journal entry
	// may be booked with a timestamp in one of these months, so the
	// report's figures for those months are final. Risk and compliance
	// tooling reads this to know which periods are immutable.
	ClosedPeriods []string `json:"closed_periods"`
	// FXApplied is true when the report carries the opt-in base-currency
	// summary (see ReconcileOptions.BaseCurrency); false on a plain scan.
	FXApplied bool `json:"fx_applied"`
	// BaseCurrencySummary converts the report's per-currency totals and
	// discrepancies into one reporting currency through the FX rate
	// table. Nil unless FXApplied.
	BaseCurrencySummary *BaseCurrencySummary `json:"base_currency_summary,omitempty"`
	// ActiveTransferSchedules counts the recurring transfer plans (see
	// schedule.go) currently firing on their cadence. Paused, cancelled,
	// and completed plans are not counted. Operations tooling reads this
	// to know how many schedules the next sweep may fire.
	ActiveTransferSchedules int `json:"active_transfer_schedules"`
	// PendingReviews lists every large transfer currently awaiting an
	// operator decision (see review.go), sorted by (CreatedAt,
	// TransferID): the frozen legs, the reserved outflow, and each
	// review's expiry. Reviews whose expiry passed but which
	// ExpireReviews has not swept yet are excluded — they are already
	// inactive. Compliance tooling reads this to know which funds are
	// frozen in dual-control limbo at report time.
	PendingReviews []TransferReview `json:"pending_reviews"`
	// ReviewThresholds lists every configured per-account review
	// threshold (see SetReviewThreshold), sorted by account. The
	// ledger-wide threshold is not listed here — it applies to every
	// account without a per-account row.
	ReviewThresholds []ReviewThreshold `json:"review_thresholds"`
	// SettlementCutoffs lists every configured per-currency settlement
	// cutoff (see settlement_cutoff.go), sorted by currency. Treasury
	// tooling reads this to know which currencies settle on windows and
	// at what wall-clock time.
	SettlementCutoffs []SettlementCutoff `json:"settlement_cutoffs,omitempty"`
	// SettlementDeferrals lists every transfer currently deferred to a
	// settlement window (see settlement_cutoff.go), sorted by (window,
	// transfer ID), including settled history: the full settlement
	// trail the operator's window runs leave behind.
	SettlementDeferrals []SettlementDeferral `json:"settlement_deferrals,omitempty"`
	// RevaluationAccount is the configured P&L account for unrealized FX
	// gains/losses (see SetRevaluationAccount); empty when FX
	// revaluation is not configured.
	RevaluationAccount AccountID `json:"revaluation_account,omitempty"`
	// RevaluationImpact lists every (account, currency) revaluation
	// baseline (see RevalueBalances), sorted by (account, currency,
	// base): the last remeasured carrying value in the base currency and
	// the rate that produced it. This is the unrealized FX position the
	// revaluation entries have recognized so far — the P&L account above
	// accumulates the gains/losses, and each row here shows the carrying
	// value behind them.
	RevaluationImpact []RevaluationPosition `json:"revaluation_impact"`
	// AccountTags lists every account carrying a tag set (see
	// SetAccountTags), sorted by account: the classification labels
	// downstream reporting groups the per-account trial balances by.
	// Operations tooling reads this for tag-dimension reporting — a
	// rollup by business line or segment over the trial balances in this
	// same report. Empty when no account is tagged.
	AccountTags []AccountTag `json:"account_tags,omitempty"`
	// TermDeposits lists every term deposit (see OpenTermDeposit),
	// sorted by deposit ID: the locked principal, the rate and tenor,
	// the lifecycle status, and the interest journaled so far (recovered
	// from the deterministic "term-interest/<id>/" rows, like the
	// dividend disclosure). Risk tooling reads this to know which funds
	// are locked and when they release.
	TermDeposits []TermDepositReport `json:"term_deposits"`
	// CreditLines lists every granted credit line (see
	// GrantCreditLine), sorted by (account, currency): the limit, the
	// drawn utilization, and the remaining availability. Risk tooling
	// reads this for the ledger's credit exposure at report time.
	CreditLines []CreditLineReport `json:"credit_lines"`
	// ECLExposures lists every ECL-tracked exposure with positive
	// exposure at report time (see ecl.go), sorted by (account,
	// currency): the obligor's IFRS 9 stage, the exposure in cents,
	// and the provisioned amount so far. DPD-only tracking rows and
	// fully repaid credit lines (zero resolved exposure) are absent.
	ECLExposures []ECLExposureReport `json:"ecl_exposures"`
	// ECLCoverage rolls the exposures above up per currency, sorted by
	// currency code: coverage_ratio is provisioned/exposure, the IFRS 9
	// provision-coverage metric risk tooling watches. Currencies are
	// never summed together — each currency's coverage is its own row.
	ECLCoverage []ECLCoverageRow `json:"ecl_coverage"`
	// IntradayLiquidity lists every (account, currency) intraday
	// liquidity position for the report's UTC day (see
	// IntradayLiquidity), sorted by (account, currency): the day's
	// booked flows, the outflow reserved by outstanding holds, the
	// nominal transfer-schedule firings, the term-deposit maturities,
	// the net position, and the low-liquidity threshold state. Risk
	// tooling reads this to know which accounts the day's committed
	// and forecast flows leave thin on cash.
	IntradayLiquidity []LiquidityPosition `json:"intraday_liquidity"`
	// Pain001Pending lists every pain.001 payment instruction that has
	// not reached the terminal settled state (see
	// Pain001PendingInstruction in pain001_status.go), sorted by
	// msg_id: the operator's reconcile-confirmation worklist. A
	// rejected instruction is disclosed, not hidden — its transfers
	// never settled, and the operator needs to see it to fix the
	// cause and re-export.
	Pain001Pending []Pain001PendingInstruction `json:"pain001_pending"`
	// FtpTotals lists the per-currency booked FTP charge totals (see
	// ChargeFtp in ftp.go), derived from the journal's "ftp:charge"
	// rows and sorted by currency: the internal funding P&L the
	// treasury charged the business units. FtpCurveVersions discloses
	// each configured currency's curve version and points — the
	// generation that priced the booked charges, so the reconciliation
	// record reproduces the pricing basis.
	FtpTotals        []FtpTotals        `json:"ftp_totals"`
	FtpCurveVersions []FtpCurveSnapshot `json:"ftp_curve_versions"`
}

// TermDepositReport is one term deposit's Reconcile disclosure: the
// locked principal and the interest the journal has booked for it.
type TermDepositReport struct {
	ID                   string    `json:"id"`
	Account              AccountID `json:"account"`
	PrincipalCents       int64     `json:"principal_cents"`
	Currency             string    `json:"currency"`
	RateBps              int64     `json:"rate_bps"`
	TermDays             int64     `json:"term_days"`
	Status               string    `json:"status"`
	OpenedAt             time.Time `json:"opened_at"`
	MaturesAt            time.Time `json:"matures_at"`
	AccruedInterestCents int64     `json:"accrued_interest_cents"`
}

// ECLExposureReport is one exposure's Reconcile disclosure: the
// obligor's IFRS 9 stage and the exposure/provision figures behind the
// coverage ratio.
type ECLExposureReport struct {
	Account          AccountID `json:"account"`
	Currency         string    `json:"currency"`
	Source           string    `json:"source"`
	DaysPastDue      int64     `json:"days_past_due"`
	Stage            int       `json:"stage"`
	ExposureCents    int64     `json:"exposure_cents"`
	ProvisionedCents int64     `json:"provisioned_cents"`
}

// ECLCoverageRow is one currency's ECL provision-coverage rollup:
// coverage_ratio = provisioned_cents / exposure_cents, 0 when the
// exposure is 0.
type ECLCoverageRow struct {
	Currency         string  `json:"currency"`
	ExposureCents    int64   `json:"exposure_cents"`
	ProvisionedCents int64   `json:"provisioned_cents"`
	CoverageRatio    float64 `json:"coverage_ratio"`
}

// eclReportsLocked builds the Reconcile ECL disclosure: every tracked
// exposure whose resolved exposure is positive, sorted by (account,
// currency), plus the per-currency provision-coverage rollup, sorted by
// currency code. Callers must hold l.mu; the read lock suffices.
func (l *Ledger) eclReportsLocked() ([]ECLExposureReport, []ECLCoverageRow) {
	exposures := make([]ECLExposureReport, 0, len(l.eclExposures))
	byCurrency := make(map[string]*ECLCoverageRow)
	for k, row := range l.eclExposures {
		exposure, dpd, source, ok := l.eclExposureLocked(k, row)
		if !ok || exposure <= 0 {
			continue
		}
		stage := l.eclStageConfig.StageForDPD(dpd)
		exposures = append(exposures, ECLExposureReport{
			Account:          k.account,
			Currency:         k.currency,
			Source:           source,
			DaysPastDue:      dpd,
			Stage:            stage,
			ExposureCents:    exposure,
			ProvisionedCents: row.provisionedCents,
		})
		agg, exists := byCurrency[k.currency]
		if !exists {
			agg = &ECLCoverageRow{Currency: k.currency}
			byCurrency[k.currency] = agg
		}
		agg.ExposureCents += exposure
		agg.ProvisionedCents += row.provisionedCents
	}
	sort.Slice(exposures, func(i, j int) bool {
		if exposures[i].Account != exposures[j].Account {
			return exposures[i].Account < exposures[j].Account
		}
		return exposures[i].Currency < exposures[j].Currency
	})
	currencies := make([]string, 0, len(byCurrency))
	for c := range byCurrency {
		currencies = append(currencies, c)
	}
	sort.Strings(currencies)
	coverage := make([]ECLCoverageRow, 0, len(currencies))
	for _, c := range currencies {
		row := byCurrency[c]
		if row.ExposureCents > 0 {
			row.CoverageRatio = float64(row.ProvisionedCents) / float64(row.ExposureCents)
		}
		coverage = append(coverage, *row)
	}
	return exposures, coverage
}

// CreditLineReport is one credit line's Reconcile disclosure: the limit
// and the exposure drawn against it.
type CreditLineReport struct {
	Account        AccountID `json:"account"`
	Currency       string    `json:"currency"`
	LimitCents     int64     `json:"limit_cents"`
	UsedCents      int64     `json:"used_cents"`
	AvailableCents int64     `json:"available_cents"`
	RateBps        int64     `json:"rate_bps"`
	GrantedAt      time.Time `json:"granted_at"`
}

// ReconcileOptions tunes Reconcile. BaseCurrency opts into the
// base-currency summary (see BaseCurrencySummary): every per-currency
// totals row and every discrepancy is converted into the reporting
// currency through the FX rate table, the report lists the rates it used,
// and FXApplied is set. Empty (the default) leaves the report exactly as
// Reconcile produces it. BaseCurrency must be a 3-letter uppercase ISO
// 4217 code; anything else fails the scan with ErrInvalidCurrency.
type ReconcileOptions struct {
	BaseCurrency string
}

// ErrBaseCurrencyOverflow is returned by ReconcileWithOptions when one
// conversion or the converted grand totals do not fit in an int64. Money
// never silently wraps: with realistic amounts and rates this is
// unreachable (fxConvertCents already rejects per-amount overflow), but
// the report refuses to print a wrapped number rather than guessing.
var ErrBaseCurrencyOverflow = errors.New("ledger: base-currency reconciliation summary overflows int64")

// FXRateSnapshot records which conversion rate produced one row of the
// base-currency summary: the rate ratio plus the ledger version at which
// the rate took effect (ExchangeRate.EffectiveVersion), so the report is
// auditable against later rate changes. The base currency itself converts
// at the identity rate 1/1 with rate_asof_version 0 — no lookup was
// needed for it.
type FXRateSnapshot struct {
	Currency        string `json:"currency"`
	RateNum         int64  `json:"rate_num"`
	RateDen         int64  `json:"rate_den"`
	RateAsOfVersion uint64 `json:"rate_asof_version"`
}

// ConvertedDiscrepancy is one trial-balance discrepancy converted into
// the report's base currency. Currency keeps the original currency the
// books were kept in; every amount field is in the base currency.
type ConvertedDiscrepancy struct {
	Account           AccountID `json:"account"`
	Currency          string    `json:"currency"`
	TotalDebitsCents  int64     `json:"total_debits_cents"`
	TotalCreditsCents int64     `json:"total_credits_cents"`
	NetBalanceCents   int64     `json:"net_balance_cents"`
	ExpectedNetCents  int64     `json:"expected_net_cents"`
	DifferenceCents   int64     `json:"difference_cents"`
}

// BaseCurrencySummary is the opt-in end-of-day rollup of a reconciliation
// report into one reporting currency. Each currency's totals convert at
// the rate in effect at scan time — floor(amount * num / den), the same
// integer convention as FX transfers, computed with 128-bit intermediates
// so no float64 ever touches money. Currencies without a (currency ->
// base) rate are never silently skipped: they are listed in MissingRates,
// the converted figures cover only the convertible currencies, and
// FXIncomplete says so explicitly.
type BaseCurrencySummary struct {
	BaseCurrency string `json:"base_currency"`
	// TotalDebitsCents and TotalCreditsCents are the grand totals in the
	// base currency, summed over Conversions only.
	TotalDebitsCents  int64 `json:"total_debits_cents"`
	TotalCreditsCents int64 `json:"total_credits_cents"`
	// Conversions is one row per converted currency, sorted by currency
	// code, in the same shape as CurrencyTotals — amounts in the base
	// currency.
	Conversions []CurrencyTotals `json:"conversions"`
	// Discrepancies mirrors the report's discrepancies converted into
	// the base currency, in the same order; empty on a healthy ledger.
	Discrepancies []ConvertedDiscrepancy `json:"discrepancies"`
	// FXSnapshot lists the rate that converted each row of Conversions,
	// in the same order: the rate ratio plus its effective version.
	FXSnapshot []FXRateSnapshot `json:"fx_snapshot"`
	// MissingRates lists the currencies that could not be converted
	// because no (currency -> base) rate is configured, sorted by
	// currency code. They are excluded from the converted figures.
	MissingRates []string `json:"missing_rates"`
	// FXIncomplete is true exactly when MissingRates is non-empty: the
	// converted figures are a partial summary, not the whole ledger.
	FXIncomplete bool `json:"fx_incomplete"`
}

// SubtreeLimitUsage is one row of the reconcile report's
// subtree_limit_utilization section (LG-80): a configured daily outflow
// limit with its subtree's already-booked outflow for the report's UTC
// day. For a parent account the subtree spans every descendant; for a
// leaf account the subtree is the account itself.
type SubtreeLimitUsage struct {
	Account    AccountID `json:"account"`
	Currency   string    `json:"currency"`
	LimitCents int64     `json:"limit_cents"`
	// SubtreeUsedCents is the subtree's booked outflow for the report's
	// UTC day, in cents.
	SubtreeUsedCents int64 `json:"subtree_used_cents"`
	// Utilization is SubtreeUsedCents / LimitCents as a fraction. It is
	// 0 when the limit is 0: a zero limit admits no outflow, so used is
	// necessarily 0 and the ratio would be 0/0.
	Utilization float64 `json:"utilization"`
	// SubtreeAccounts counts the accounts in the subtree, including the
	// account itself.
	SubtreeAccounts int `json:"subtree_accounts"`
	// Exceeded is true when the booked outflow is above the limit. Limit
	// changes and parent links that would create this state are
	// rejected (see SetDailyLimit and SetParent), so true here is a
	// canary — always worth an operator's look.
	Exceeded bool `json:"exceeded"`
}

// subtreeLimitUtilizationLocked builds the reconcile report's
// subtree_limit_utilization section: every configured daily outflow
// limit with its subtree's booked outflow for the report's UTC day and
// the utilization fraction, sorted by (account, currency). Callers must
// hold l.mu; the read lock suffices because nothing is mutated.
func (l *Ledger) subtreeLimitUtilizationLocked(now time.Time) []SubtreeLimitUsage {
	day := utcDay(now.UTC())
	out := make([]SubtreeLimitUsage, 0, len(l.dailyLimits))
	for k, limit := range l.dailyLimits {
		members := l.subtreeAccountsLocked(k.account)
		used := l.subtreeOutflowLocked(members, k.currency, day, nil)
		var utilization float64
		if limit > 0 {
			utilization = float64(used) / float64(limit)
		}
		out = append(out, SubtreeLimitUsage{
			Account:          k.account,
			Currency:         k.currency,
			LimitCents:       limit,
			SubtreeUsedCents: used,
			Utilization:      utilization,
			SubtreeAccounts:  len(members),
			Exceeded:         used > limit,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Account != out[j].Account {
			return out[i].Account < out[j].Account
		}
		return out[i].Currency < out[j].Currency
	})
	return out
}

// Reconcile runs a full read-only scan of the ledger and returns the
// reconciliation report. The scan holds the ledger's read lock from start
// to finish, so the whole report describes one consistent point in time:
// a concurrent Post can never slip between the equation scan and the
// chain check. The caller supplies now (the server passes time.Now()) so
// tests can pin the timestamp and TTL arithmetic stays deterministic.
func (l *Ledger) Reconcile(now time.Time) ReconciliationReport {
	report, _ := l.ReconcileWithOptions(now, ReconcileOptions{})
	return report
}

// ReconcileWithOptions runs the same scan as Reconcile and, when
// opts.BaseCurrency is set, additionally folds every per-currency totals
// row and every discrepancy through the FX rate table into a
// base-currency summary. The conversion runs inside the same read lock as
// the scan, reading the rate table directly (no nested locking — see the
// LG-13 note below), so the summary describes exactly the ledger state
// the report was built from. An invalid base currency returns
// ErrInvalidCurrency; a conversion that does not fit in an int64 returns
// ErrBaseCurrencyOverflow.
func (l *Ledger) ReconcileWithOptions(now time.Time, opts ReconcileOptions) (ReconciliationReport, error) {
	base := ""
	if opts.BaseCurrency != "" {
		var err error
		if base, err = normalizeCurrency(opts.BaseCurrency); err != nil {
			return ReconciliationReport{}, fmt.Errorf("%w: base currency %q", ErrInvalidCurrency, opts.BaseCurrency)
		}
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	report := l.reconcileLocked(now)
	if base != "" {
		summary, err := l.baseCurrencySummaryLocked(base, report.CurrencyTotals, report.Discrepancies)
		if err != nil {
			return ReconciliationReport{}, err
		}
		report.FXApplied = true
		report.BaseCurrencySummary = summary
	}
	// Reconcile is read-only, so the version brackets are identical — the
	// event still records that reconciliation ran, against which ledger
	// state, and what it found. Emitted under the read lock; the enqueue
	// never blocks.
	l.emitAudit(AuditEvent{
		Op:            "reconcile",
		Actor:         "Reconcile",
		TraceID:       fmt.Sprintf("reconcile@%d", report.Version),
		VersionBefore: report.Version,
		VersionAfter:  report.Version,
		Details: map[string]any{
			"accounting_equation_ok": report.AccountingEquationOK,
			"discrepancies":          len(report.Discrepancies),
			"merges":                 len(report.Merges),
			"dividends":              len(report.Dividends),
			"escheats":               len(report.Escheats),
			"fx_applied":             report.FXApplied,
			"pain001_pending":        len(report.Pain001Pending),
			"ftp_totals":             len(report.FtpTotals),
			"fx_forward_contracts":   report.ForwardBook.Total,
		},
	})
	return report, nil
}

// reconcileLocked performs the scan. Callers must hold l.mu; the read lock
// suffices because the scan mutates nothing.
func (l *Ledger) reconcileLocked(now time.Time) ReconciliationReport {
	report := ReconciliationReport{
		GeneratedAt:                now,
		Version:                    l.version,
		TrialBalances:              make([]TrialBalance, 0, len(l.balances)),
		Discrepancies:              make([]TrialBalanceDiscrepancy, 0),
		FrozenAccounts:             l.frozenAccountsLocked(),
		ClosedAccounts:             l.closedAccountsLocked(),
		FrozenAccountAutoUnfreeze:  l.frozenUntilCopyLocked(),
		OverdraftProtectedAccounts: l.overdraftProtectedAccountsLocked(),
		DailyLimits:                l.dailyLimitsLocked(),
		SubtreeLimitUtilization:    l.subtreeLimitUtilizationLocked(now),
		LargeTxReports:             l.largeTxReportsForDayLocked(utcDay(now)),
		BudgetUtilization:          l.budgetUtilizationLocked(now),
		BalanceWatches:             l.balanceWatchesLocked(),
		AccountTypeTotals:          l.accountTypeTotalsLocked(),
		HeldTotals:                 l.heldTotalsLocked(now),
		FXRates:                    l.fxRatesLocked(),
		FXRateVersions:             l.fxRateVersionReportLocked(),
		ForwardBook:                l.forwardBookReportLocked(now),
		Merges:                     l.mergesLocked(),
		// LG-93: the spread P&L is derived from the journal's
		// "<transfer ID>/spread" legs (see fxSpreadPNLLocked) — the
		// same legs the audit chain and trial balances already
		// carry, so the section can never disagree with the books.
		FXSpreadPNL:             l.fxSpreadPNLLocked(),
		Dividends:               l.dividendsLocked(),
		Escheats:                l.escheatsLocked(),
		VirtualAccounts:         l.virtualAccountsLocked(),
		VirtualAccountVersion:   l.virtualAccountVersion,
		ClosedPeriods:           l.closedPeriodsLocked(),
		ActiveTransferSchedules: l.activeScheduleCountLocked(),
		PendingReviews:          l.pendingTransferReviewsLocked(now),
		ReviewThresholds:        l.reviewThresholdsLocked(),
		// LG-101: the report discloses the settlement-window config and
		// every deferred transfer, so the reconciliation record shows
		// which money settles in which window.
		SettlementCutoffs:   l.settlementCutoffsLocked(),
		SettlementDeferrals: l.deferredSettlementsLocked(),
		RevaluationAccount:  l.revalAccount,
		RevaluationImpact:   l.revaluationPositionsLocked(),
		AccountTags:         l.accountTagsLocked(),
		TermDeposits:        l.termDepositReportsLocked(),
		CreditLines:         l.creditLineReportsLocked(),
		// LG-98: the report discloses the day's intraday liquidity
		// position table — the same read-only forecast the
		// /liquidity/intraday endpoint serves, so the reconciliation
		// record carries the day's cash-position view.
		IntradayLiquidity: l.intradayLiquidityLocked(now).Positions,
		// LG-99: the report discloses every unsettled pain.001
		// payment instruction — the operator's
		// reconcile-confirmation worklist (see
		// SettlePain001Instruction in pain001_status.go).
		Pain001Pending: l.pain001PendingLocked(),
		// LG-105: the report discloses the booked FTP charge totals
		// and the curve versions that priced them — the internal
		// funding P&L plus its pricing basis, both derived from the
		// journal so the section can never disagree with the books.
		FtpTotals:        l.ftpTotalsLocked(),
		FtpCurveVersions: l.ftpCurveVersionsLocked(),
	}
	report.ECLExposures, report.ECLCoverage = l.eclReportsLocked()

	// Every account that has ever been touched. Net balances, debit
	// totals, and credit totals each cover a different (account, currency)
	// set in principle (a storage-layer corruption could strand a totals
	// row without a balance row, or vice versa), so union all three on
	// the account dimension. Sorting the accounts makes the report
	// deterministic across runs.
	seen := make(map[AccountID]bool)
	for k := range l.balances {
		seen[k.account] = true
	}
	for k := range l.debitTotals {
		seen[k.account] = true
	}
	for k := range l.creditTotals {
		seen[k.account] = true
	}
	accounts := make([]AccountID, 0, len(seen))
	for a := range seen {
		accounts = append(accounts, a)
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i] < accounts[j] })

	// One pass: per-account trial balances, per-account equation checks,
	// and the per-currency debit/credit sums. Currencies are never added
	// together: each currency's books must balance on their own.
	debits := make(map[string]int64)
	credits := make(map[string]int64)
	for _, a := range accounts {
		tb := l.trialBalanceLocked(a)
		report.TrialBalances = append(report.TrialBalances, tb)
		for _, row := range tb.ByCurrency {
			debits[row.Currency] += row.TotalDebits
			credits[row.Currency] += row.TotalCredits
			if expected := row.TotalDebits - row.TotalCredits; row.NetBalance != expected {
				report.Discrepancies = append(report.Discrepancies, TrialBalanceDiscrepancy{
					Account:           a,
					Currency:          row.Currency,
					TotalDebitsCents:  row.TotalDebits,
					TotalCreditsCents: row.TotalCredits,
					NetBalanceCents:   row.NetBalance,
					ExpectedNetCents:  expected,
					DifferenceCents:   row.NetBalance - expected,
				})
				if report.AccountingError == "" {
					report.AccountingError = fmt.Sprintf(
						"ledger: account %q (%s) out of balance: net %d != debits %d - credits %d",
						a, row.Currency, row.NetBalance, row.TotalDebits, row.TotalCredits)
				}
			}
		}
	}
	currencies := make([]string, 0, len(debits))
	for c := range debits {
		currencies = append(currencies, c)
	}
	sort.Strings(currencies)
	for _, c := range currencies {
		d, cr := debits[c], credits[c]
		report.CurrencyTotals = append(report.CurrencyTotals, CurrencyTotals{
			Currency:     c,
			TotalDebits:  d,
			TotalCredits: cr,
		})
		if c == DefaultCurrency {
			report.TotalDebitsCents = d
			report.TotalCreditsCents = cr
		}
		if report.AccountingError == "" && d != cr {
			report.AccountingError = fmt.Sprintf(
				"ledger: books do not balance in %s: total debits %d != total credits %d", c, d, cr)
		}
	}
	report.AccountingEquationOK = report.AccountingError == ""

	// LG-107: bank-statement lines that never matched a journaled row
	// surface as the same TrialBalanceDiscrepancy rows the equation
	// check above produces — one mechanism for every reconciliation
	// break, and the base-currency summary converts them like any other
	// row. A statement row is recognizable: the books balance internally
	// (NetBalance == TotalDebits - TotalCredits), while DifferenceCents
	// is the statement line's signed amount — the bank's claim on the
	// account's balance that the books do not carry — and
	// ExpectedNetCents is the balance the books would show if the line
	// were real and booked. Statement rows never move the accounting
	// equation flag: the equation is about the books' internal
	// consistency, and the books are consistent here — the bank and the
	// books disagree.
	for _, rec := range l.unmatchedStatementMatchesLocked() {
		signed := rec.line.SignedAmountCents()
		var debits, credits, net int64
		for _, row := range l.trialBalanceLocked(rec.line.Account).ByCurrency {
			if row.Currency == rec.line.Currency {
				debits, credits, net = row.TotalDebits, row.TotalCredits, row.NetBalance
			}
		}
		report.Discrepancies = append(report.Discrepancies, TrialBalanceDiscrepancy{
			Account:           rec.line.Account,
			Currency:          rec.line.Currency,
			TotalDebitsCents:  debits,
			TotalCreditsCents: credits,
			NetBalanceCents:   net,
			ExpectedNetCents:  net + signed,
			DifferenceCents:   signed,
		})
	}

	// Idempotency-key index health. Expired-eligible means the entry was
	// posted more than the TTL ago — exactly the set a key sweep would
	// evict, mirroring pruneIdempotencyKeysLocked's cutoff.
	report.IdempotencyKeys.TTLConfigured = l.idempotencyTTL > 0
	report.IdempotencyKeys.TTL = l.idempotencyTTL.String()
	report.IdempotencyKeys.TotalKeys = len(l.byKey)
	if report.IdempotencyKeys.TTLConfigured {
		cutoff := now.Add(-l.idempotencyTTL)
		for _, e := range l.byKey {
			if e.CreatedAt.Before(cutoff) {
				report.IdempotencyKeys.ExpiredEligible++
			}
		}
	}

	report.AuditChain = l.auditChainHealthLocked()

	return report
}

// heldTotalsLocked rolls active authorization holds up per currency at
// now, sorted by currency code. Expiry is lazy: holds whose ExpiresAt has
// passed count as inactive even before the ExpireHolds sweep. Callers must
// hold l.mu; the read lock suffices because the scan mutates nothing.
func (l *Ledger) heldTotalsLocked(now time.Time) []CurrencyHoldTotals {
	byCurrency := make(map[string]*CurrencyHoldTotals)
	for _, h := range l.holds {
		if !activeHoldLocked(h, now) {
			continue
		}
		row, ok := byCurrency[h.Currency]
		if !ok {
			row = &CurrencyHoldTotals{Currency: h.Currency}
			byCurrency[h.Currency] = row
		}
		row.HeldCents += h.AmountCents
		row.ActiveHolds++
	}
	currencies := make([]string, 0, len(byCurrency))
	for c := range byCurrency {
		currencies = append(currencies, c)
	}
	sort.Strings(currencies)
	out := make([]CurrencyHoldTotals, 0, len(currencies))
	for _, c := range currencies {
		out = append(out, *byCurrency[c])
	}
	return out
}

// baseCurrencySummaryLocked converts the report's per-currency totals and
// discrepancies into base through the FX rate table. Callers must hold
// l.mu; the read lock suffices. It reads l.fxRates directly — never the
// locking FXRate accessor — because the reconciliation scan already holds
// the read lock; a nested lock call here would be the LG-13 deadlock
// class of bug. Totals and discrepancies arrive in the report's own
// deterministic order (currencies sorted, discrepancies account-sorted),
// so every list the summary emits is sorted without extra work. A
// discrepancy's currency always appears in the totals pass, so a currency
// that misses its rate is already in MissingRates by the time the
// discrepancy pass would look it up.
func (l *Ledger) baseCurrencySummaryLocked(base string, totals []CurrencyTotals, discrepancies []TrialBalanceDiscrepancy) (*BaseCurrencySummary, error) {
	summary := &BaseCurrencySummary{
		BaseCurrency:  base,
		Conversions:   make([]CurrencyTotals, 0, len(totals)),
		Discrepancies: make([]ConvertedDiscrepancy, 0, len(discrepancies)),
		FXSnapshot:    make([]FXRateSnapshot, 0, len(totals)),
		MissingRates:  make([]string, 0),
	}
	var totalDebits, totalCredits int64
	accumulate := func(d, c int64) bool {
		var ok bool
		if totalDebits, ok = addCents(totalDebits, d); !ok {
			return false
		}
		if totalCredits, ok = addCents(totalCredits, c); !ok {
			return false
		}
		return true
	}
	for _, row := range totals {
		if row.Currency == base {
			// The base currency converts at identity; no rate lookup.
			summary.Conversions = append(summary.Conversions, row)
			summary.FXSnapshot = append(summary.FXSnapshot, FXRateSnapshot{
				Currency: base, RateNum: 1, RateDen: 1, RateAsOfVersion: 0,
			})
			if !accumulate(row.TotalDebits, row.TotalCredits) {
				return nil, ErrBaseCurrencyOverflow
			}
			continue
		}
		rate, ok := l.fxRates[fxPair{from: row.Currency, to: base}]
		if !ok {
			// No rate: listed, excluded, never silently skipped.
			summary.MissingRates = append(summary.MissingRates, row.Currency)
			continue
		}
		d, ok := fxConvertSignedCents(row.TotalDebits, rate.Num, rate.Den)
		if !ok {
			return nil, ErrBaseCurrencyOverflow
		}
		c, ok := fxConvertSignedCents(row.TotalCredits, rate.Num, rate.Den)
		if !ok {
			return nil, ErrBaseCurrencyOverflow
		}
		summary.Conversions = append(summary.Conversions, CurrencyTotals{
			Currency: row.Currency, TotalDebits: d, TotalCredits: c,
		})
		summary.FXSnapshot = append(summary.FXSnapshot, FXRateSnapshot{
			Currency: row.Currency, RateNum: rate.Num, RateDen: rate.Den,
			RateAsOfVersion: rate.EffectiveVersion,
		})
		if !accumulate(d, c) {
			return nil, ErrBaseCurrencyOverflow
		}
	}
	for _, disc := range discrepancies {
		conv := ConvertedDiscrepancy{Account: disc.Account, Currency: disc.Currency}
		if disc.Currency != base {
			rate, ok := l.fxRates[fxPair{from: disc.Currency, to: base}]
			if !ok {
				// Already listed in MissingRates by the totals pass;
				// its discrepancy converts with nothing, like its totals.
				continue
			}
			fields := [5]int64{
				disc.TotalDebitsCents, disc.TotalCreditsCents,
				disc.NetBalanceCents, disc.ExpectedNetCents, disc.DifferenceCents,
			}
			for i, v := range fields {
				c, ok := fxConvertSignedCents(v, rate.Num, rate.Den)
				if !ok {
					return nil, ErrBaseCurrencyOverflow
				}
				fields[i] = c
			}
			conv.TotalDebitsCents, conv.TotalCreditsCents = fields[0], fields[1]
			conv.NetBalanceCents, conv.ExpectedNetCents = fields[2], fields[3]
			conv.DifferenceCents = fields[4]
		} else {
			conv.TotalDebitsCents = disc.TotalDebitsCents
			conv.TotalCreditsCents = disc.TotalCreditsCents
			conv.NetBalanceCents = disc.NetBalanceCents
			conv.ExpectedNetCents = disc.ExpectedNetCents
			conv.DifferenceCents = disc.DifferenceCents
		}
		summary.Discrepancies = append(summary.Discrepancies, conv)
	}
	summary.TotalDebitsCents = totalDebits
	summary.TotalCreditsCents = totalCredits
	summary.FXIncomplete = len(summary.MissingRates) > 0
	return summary, nil
}

// fxConvertSignedCents converts a possibly-negative amount at the ratio
// num/den with the floor convention: non-negative amounts go through
// fxConvertCents; negative amounts convert by magnitude and re-apply the
// sign. Report totals are non-negative, but discrepancy fields (net,
// expected, difference) can dip below zero.
func fxConvertSignedCents(amount, num, den int64) (int64, bool) {
	if amount >= 0 {
		return fxConvertCents(amount, num, den)
	}
	if amount == math.MinInt64 {
		return 0, false
	}
	c, ok := fxConvertCents(-amount, num, den)
	if !ok {
		return 0, false
	}
	return -c, true
}

// mergesLocked lists every committed account merge, sorted by merge ID,
// for the reconciliation report. Callers must hold l.mu; the read lock
// suffices because the scan mutates nothing.
func (l *Ledger) mergesLocked() []AccountMerge {
	ids := make([]string, 0, len(l.merges))
	for id := range l.merges {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]AccountMerge, 0, len(ids))
	for _, id := range ids {
		rec := l.merges[id]
		out = append(out, AccountMerge{
			MergeID:     rec.mergeID,
			FromAccount: rec.from,
			ToAccount:   rec.to,
			Legs:        rec.legs,
			CreatedAt:   rec.createdAt,
		})
	}
	return out
}

// auditChainHealthLocked recomputes the audit chain and checks the head
// against the ledger version. Callers must hold l.mu; the read lock
// suffices because nothing is mutated.
func (l *Ledger) auditChainHealthLocked() AuditChainHealth {
	h := AuditChainHealth{}
	if err := l.verifyChainLocked(); err != nil {
		h.VerifyError = err.Error()
	} else {
		h.VerifyOK = true
	}

	n := uint64(len(l.chain))
	h.Links = n
	if n > 0 {
		h.Head = hex.EncodeToString(l.chain[n-1].hash[:])
	} else {
		var genesis [32]byte
		h.Head = hex.EncodeToString(genesis[:])
	}

	// Head consistency: one link per successful Post means the link count
	// must equal the ledger version, and the newest link's seq must be
	// that version. A mismatch means the chain and the journal counters
	// desynced — something VerifyChain alone cannot see.
	h.HeadConsistent = n == l.version && (n == 0 || l.chain[n-1].seq == l.version)
	if !h.HeadConsistent {
		if n != l.version {
			h.ConsistencyError = fmt.Sprintf(
				"ledger: audit chain has %d links but ledger version is %d", n, l.version)
		} else {
			h.ConsistencyError = fmt.Sprintf(
				"ledger: audit chain head link seq is %d, want ledger version %d",
				l.chain[n-1].seq, l.version)
		}
	}

	// External anchors (LG-44): replay every checkpoint against the live
	// chain. The checkpoint list is loaded lazily and guarded by its own
	// mutex (see anchor.go); reading the count takes that mutex, while
	// verifyAnchorsLocked runs under this function's l.mu.
	l.ensureCheckpointsLoaded()
	l.anchor.mu.Lock()
	h.AnchorCheckpoints = len(l.anchor.checkpoints)
	l.anchor.mu.Unlock()
	if err := l.verifyAnchorsLocked(); err != nil {
		h.AnchorError = err.Error()
	} else {
		h.AnchorOK = true
	}

	// RFC 3161 timestamp anchors (LG-84): replay every stored anchor
	// against the live chain and the configured trust anchor. The
	// anchor list is loaded lazily and guarded by its own mutex (see
	// tsa.go); reading the count takes that mutex, while
	// verifyTimestampAnchorsLocked runs under this function's l.mu.
	l.ensureTSAAnchorsLoaded()
	l.tsa.mu.Lock()
	h.TSAAnchors = len(l.tsa.anchors)
	l.tsa.mu.Unlock()
	if err := l.verifyTimestampAnchorsLocked(); err != nil {
		h.TSAAnchorError = err.Error()
	} else {
		h.TSAAnchorOK = true
	}
	return h
}

// WriteJSON encodes the report as indented JSON to w. The indented form is
// meant for humans: it is what the operator archives from POST /reconcile.
func (r ReconciliationReport) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}
