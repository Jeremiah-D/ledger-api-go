package ledger

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ECL: IFRS 9-style expected credit loss provisions (LG-92, fintech/bank
// compliance).
//
// Every credit exposure the ledger tracks — a manually registered
// exposure, or a granted credit line's drawn utilization (see
// creditline.go) — sits in one of three IFRS 9 stages by days past due
// (DPD): Stage 1 (performing), Stage 2 (significant increase in credit
// risk), Stage 3 (credit-impaired). Each stage carries a configurable
// provision rate in basis points, and BookECLProvision books the day's
// expected loss: floor(exposure * rateBps / 10000), debit the
// credit-loss expense account, credit the loss-allowance (contra-asset)
// account.
//
// The staging configuration is structural, like credit lines: changing
// it never bumps the journal version, and it round-trips through
// disaster-recovery snapshots. The P&L/allowance accounts are
// fail-closed, like the term-deposit interest-expense account and the
// credit-line interest-income account: with either unset,
// BookECLProvision refuses rather than booking against a zero account.
//
// Provisioning follows the accrual convention (see accrual.go): the entry
// ID is deterministic — "ecl/<account>/<YYYY-MM-DD>" (UTC day) — so
// re-running a day's provisioning replays the original row instead of
// double-booking, and the entry posts through the system path (no journal
// rules engine, no dual-control review, the dormant-account stop does not
// apply). A dust provision (zero cents after flooring) books nothing and
// returns ErrNonPositiveAmount.

// ECLStageConfig is the IFRS 9 staging configuration: the DPD buckets
// that delimit the stages and the per-stage provision rates in basis
// points. Stage 1 covers DPD < Stage2DPD, Stage 2 covers
// Stage2DPD <= DPD < Stage3DPD, Stage 3 covers DPD >= Stage3DPD.
type ECLStageConfig struct {
	Stage2DPD      int64 `json:"stage2_dpd"`
	Stage3DPD      int64 `json:"stage3_dpd"`
	RateStage1Bps  int64 `json:"rate_stage1_bps"`
	RateStage2Bps  int64 `json:"rate_stage2_bps"`
	RateStage3Bps  int64 `json:"rate_stage3_bps"`
}

// DefaultECLStageConfig is the ledger's staging configuration at
// construction: Stage 2 at 30 days past due, Stage 3 at 90 days, with
// 1% / 10% / 50% provision rates — a conventional bank starting point,
// adjustable per portfolio with SetECLStageConfig.
func DefaultECLStageConfig() ECLStageConfig {
	return ECLStageConfig{
		Stage2DPD:     30,
		Stage3DPD:     90,
		RateStage1Bps: 100,
		RateStage2Bps: 1000,
		RateStage3Bps: 5000,
	}
}

// Validation and state errors returned by the ECL APIs.
var (
	// ErrEmptyECLAccount is returned when the ECL account ID is empty.
	ErrEmptyECLAccount = errors.New("ledger: ECL account must not be empty")
	// ErrECLAccountSeparator is returned when the account ID contains
	// '/': the deterministic provision entry IDs join their components
	// with '/', so a '/' inside the account would let two different
	// obligors share one provision row (see the accrual convention in
	// accrual.go).
	ErrECLAccountSeparator = errors.New("ledger: ECL account ID must not contain '/'")
	// ErrECLNonPositiveExposure is returned when a manually registered
	// exposure is not greater than zero.
	ErrECLNonPositiveExposure = errors.New("ledger: ECL exposure must be greater than zero")
	// ErrECLNegativeDPD is returned when days past due is negative.
	ErrECLNegativeDPD = errors.New("ledger: ECL days past due must not be negative")
	// ErrECLExposureNotFound is returned when no exposure is tracked
	// for (account, currency): no manual exposure was registered and no
	// credit line is granted there.
	ErrECLExposureNotFound = errors.New("ledger: no ECL exposure tracked for this account and currency")
	// ErrECLLossExpenseNotConfigured is returned by BookECLProvision
	// when no credit-loss expense account is configured (see
	// SetECLLossExpense): fail-closed, so provisions are never booked
	// against a zero account.
	ErrECLLossExpenseNotConfigured = errors.New("ledger: ECL loss expense account not configured")
	// ErrECLAllowanceNotConfigured is returned by BookECLProvision
	// when no loss-allowance account is configured (see
	// SetECLAllowance): fail-closed, for the same reason.
	ErrECLAllowanceNotConfigured = errors.New("ledger: ECL allowance account not configured")
	// ErrInvalidECLStageConfig is returned when a staging
	// configuration is structurally invalid: a negative DPD bucket, an
	// inverted stage boundary, or a provision rate outside 0..10000
	// basis points. The detail rides in the wrapped message.
	ErrInvalidECLStageConfig = errors.New("ledger: invalid ECL stage config")
)

// Validate reports whether the staging configuration is structurally
// sound: non-negative DPD buckets, a strictly increasing stage
// boundary, and every provision rate within 0..10000 basis points. A
// rate of zero is legal (a stage the portfolio provisions nothing
// for); a rate above 100% is a configuration bug, not a clampable rate
// (same bound as the accrual and credit-line rates).
func (c ECLStageConfig) Validate() error {
	if c.Stage2DPD < 0 {
		return fmt.Errorf("%w: stage2_dpd %d is negative", ErrInvalidECLStageConfig, c.Stage2DPD)
	}
	if c.Stage3DPD < 0 {
		return fmt.Errorf("%w: stage3_dpd %d is negative", ErrInvalidECLStageConfig, c.Stage3DPD)
	}
	if c.Stage2DPD >= c.Stage3DPD {
		return fmt.Errorf("%w: stage2_dpd %d must be below stage3_dpd %d",
			ErrInvalidECLStageConfig, c.Stage2DPD, c.Stage3DPD)
	}
	for _, r := range [][2]any{{"rate_stage1_bps", c.RateStage1Bps}, {"rate_stage2_bps", c.RateStage2Bps}, {"rate_stage3_bps", c.RateStage3Bps}} {
		if r[1].(int64) < 0 || r[1].(int64) > 10000 {
			return fmt.Errorf("%w: %s %d is outside 0..10000 basis points",
				ErrInvalidECLStageConfig, r[0], r[1])
		}
	}
	return nil
}

// StageForDPD maps days past due to the IFRS 9 stage under this
// configuration: 1 below Stage2DPD, 2 below Stage3DPD, 3 at or above it.
// The bucket boundaries are inclusive at the lower end, so the default
// config stages DPD 30 as Stage 2 and DPD 90 as Stage 3.
func (c ECLStageConfig) StageForDPD(dpd int64) int {
	if dpd >= c.Stage3DPD {
		return 3
	}
	if dpd >= c.Stage2DPD {
		return 2
	}
	return 1
}

// RateForStage returns the provision rate in basis points for a stage
// (1, 2, or 3). Any other stage is a programming error and yields the
// Stage 1 rate rather than guessing — callers only ever pass stages from
// StageForDPD.
func (c ECLStageConfig) RateForStage(stage int) int64 {
	switch stage {
	case 2:
		return c.RateStage2Bps
	case 3:
		return c.RateStage3Bps
	default:
		return c.RateStage1Bps
	}
}

// ECL exposure sources.
const (
	// ECLObligorManual is a manually registered exposure (see
	// RegisterECLExposure): for obligors without a credit line.
	ECLObligorManual = "manual"
	// ECLObligorCreditLine is the drawn utilization of a granted
	// credit line (see GrantCreditLine), the LG-91 exposure source.
	ECLObligorCreditLine = "credit_line"
)

// eclExposureKey identifies one tracked exposure: an obligor carries at
// most one exposure row per currency.
type eclExposureKey struct {
	account  AccountID
	currency string
}

// eclExposureRow is the ledger's internal per-(account, currency)
// exposure state. hasManual marks a manually registered exposure;
// without one, the row only tracks days past due for a credit-line
// sourced exposure. stage is the last IFRS 9 stage recorded, so stage
// migrations are edge-triggered. provisionedCents accumulates every
// provision BookECLProvision committed for the row, backing the
// Reconcile coverage disclosure.
type eclExposureRow struct {
	manualCents      int64
	hasManual        bool
	daysPastDue      int64
	stage            int
	provisionedCents int64
	updatedAt        time.Time
}

// ECLExposure is the read-side view of one tracked exposure: the
// resolved exposure in cents (the manual amount, or the credit line's
// drawn utilization), its DPD and stage, and the provisioned amount so
// far. Rows whose exposure resolves to zero carry ExposureCents 0 —
// they track DPD only.
type ECLExposure struct {
	Account          AccountID `json:"account"`
	Currency         string    `json:"currency"`
	Source           string    `json:"source"`
	DaysPastDue      int64     `json:"days_past_due"`
	Stage            int       `json:"stage"`
	ExposureCents    int64     `json:"exposure_cents"`
	ProvisionedCents int64     `json:"provisioned_cents"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// ECLExposureRecord is the snapshot record form of one exposure row.
type ECLExposureRecord struct {
	Account          AccountID `json:"account"`
	Currency         string    `json:"currency"`
	ManualCents      int64     `json:"manual_cents,omitempty"`
	DaysPastDue      int64     `json:"days_past_due"`
	Stage            int       `json:"stage"`
	ProvisionedCents int64     `json:"provisioned_cents"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// eclEntryID builds the deterministic provision entry ID for one
// (account, currency, UTC day): the accrual convention (see accrual.go).
// The account ID cannot contain '/', so the namespace never collides
// across obligors. One currency rides per entry — an obligor with two
// tracked currencies books two rows a day, each idempotent on its own.
func eclEntryID(account AccountID, currency string, asof time.Time) string {
	return "ecl/" + string(account) + "/" + currency + "/" + asof.UTC().Format("2006-01-02")
}

// checkECLAccount rejects empty account IDs and ones that would break
// the deterministic ID scheme, mirroring checkAccrualAccounts.
func checkECLAccount(account AccountID) error {
	if account == "" {
		return ErrEmptyECLAccount
	}
	if strings.Contains(string(account), "/") {
		return ErrECLAccountSeparator
	}
	return nil
}

// SetECLStageConfig replaces the ledger's IFRS 9 staging configuration
// after validating it. Changing the stage rates or buckets is
// structural config, like granting a credit line: it never bumps the
// journal version, it is audited as ecl_stage_config_set, and it
// survives disaster-recovery snapshots. Already-tracked exposures keep
// their recorded stage until the next DPD update or provision run
// re-stages them — the config change itself does not migrate anyone,
// so no migration events fire here.
func (l *Ledger) SetECLStageConfig(cfg ECLStageConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.eclStageConfig = cfg
	l.emitAudit(AuditEvent{
		Op:      "ecl_stage_config_set",
		Actor:   "SetECLStageConfig",
		TraceID: "ecl-stage-config",
		Details: map[string]any{
			"stage2_dpd":       cfg.Stage2DPD,
			"stage3_dpd":       cfg.Stage3DPD,
			"rate_stage1_bps":  cfg.RateStage1Bps,
			"rate_stage2_bps":  cfg.RateStage2Bps,
			"rate_stage3_bps":  cfg.RateStage3Bps,
		},
	})
	return nil
}

// ECLStageConfig returns the ledger's current IFRS 9 staging
// configuration.
func (l *Ledger) ECLStageConfig() ECLStageConfig {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.eclStageConfig
}

// RegisterECLExposure registers (or re-registers) a manual credit
// exposure for an obligor without a credit line — the LG-92 manual
// source, independent of LG-91. The exposure is exposureCents in
// currency at daysPastDue days past due; the row's stage is computed
// under the current staging configuration. Re-registering updates the
// amount and DPD in place (create-or-replace); a stage change fires the
// ecl_stage_migration audit event, like any other stage transition.
// Registration is structural config: it never bumps the journal
// version, and it is audited as ecl_exposure_registered.
func (l *Ledger) RegisterECLExposure(account AccountID, currency string, exposureCents, daysPastDue int64) (ECLExposure, error) {
	if err := checkECLAccount(account); err != nil {
		return ECLExposure{}, err
	}
	cur, err := normalizeCurrency(currency)
	if err != nil {
		return ECLExposure{}, err
	}
	if exposureCents <= 0 {
		return ECLExposure{}, ErrECLNonPositiveExposure
	}
	if daysPastDue < 0 {
		return ECLExposure{}, ErrECLNegativeDPD
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	k := eclExposureKey{account: account, currency: cur}
	row := l.eclExposures[k]
	_, existed := l.eclExposures[k]
	row.manualCents = exposureCents
	row.hasManual = true
	row.daysPastDue = daysPastDue
	row.updatedAt = time.Now()
	l.updateECLStageLocked(k, &row, daysPastDue, exposureCents)
	l.eclExposures[k] = row
	l.emitAudit(AuditEvent{
		Op:            "ecl_exposure_registered",
		Actor:         "RegisterECLExposure",
		TraceID:       string(account) + "/" + cur,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{account},
		Details: map[string]any{
			"created":        !existed,
			"currency":       cur,
			"exposure_cents": exposureCents,
			"days_past_due":  daysPastDue,
			"stage":          row.stage,
		},
	})
	return l.eclExposureViewLocked(k, row), nil
}

// SetECLDaysPastDue updates the days-past-due tracking for an
// (account, currency) exposure — the LG-92 DPD update API. It works on
// both exposure sources: a manually registered exposure, or a
// credit-line-sourced exposure (the row is created on first update when
// a credit line is granted there). With neither a manual exposure nor a
// credit line, there is nothing to age and the call fails with
// ErrECLExposureNotFound. A stage change fires the ecl_stage_migration
// audit event. DPD tracking is structural config: it never bumps the
// journal version.
func (l *Ledger) SetECLDaysPastDue(account AccountID, currency string, daysPastDue int64) error {
	if err := checkECLAccount(account); err != nil {
		return err
	}
	cur, err := normalizeCurrency(currency)
	if err != nil {
		return err
	}
	if daysPastDue < 0 {
		return ErrECLNegativeDPD
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	k := eclExposureKey{account: account, currency: cur}
	row, tracked := l.eclExposures[k]
	if !tracked {
		if _, exists := l.creditLines[creditLineKey{account: account, currency: cur}]; !exists {
			return ErrECLExposureNotFound
		}
	}
	fromStage := row.stage
	row.daysPastDue = daysPastDue
	row.updatedAt = time.Now()
	exposure, _, _, _ := l.eclExposureLocked(k, row)
	l.updateECLStageLocked(k, &row, daysPastDue, exposure)
	l.eclExposures[k] = row
	l.emitAudit(AuditEvent{
		Op:            "ecl_dpd_updated",
		Actor:         "SetECLDaysPastDue",
		TraceID:       string(account) + "/" + cur,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{account},
		Details: map[string]any{
			"currency":      cur,
			"days_past_due": daysPastDue,
			"from_stage":    fromStage,
			"stage":         row.stage,
		},
	})
	return nil
}

// eclExposureLocked resolves the provisionable exposure for an
// (account, currency) row: the manually registered amount wins; without
// one, the granted credit line's drawn utilization is the exposure
// (zero when the line exists but nothing is drawn, or when the balance
// is non-negative). The source names which it was. Callers must hold
// l.mu; the read lock suffices.
func (l *Ledger) eclExposureLocked(k eclExposureKey, row eclExposureRow) (exposureCents, dpd int64, source string, ok bool) {
	if row.hasManual {
		return row.manualCents, row.daysPastDue, ECLObligorManual, true
	}
	if _, exists := l.creditLines[creditLineKey{account: k.account, currency: k.currency}]; exists {
		used := l.creditLineUsedLocked(creditLineKey{account: k.account, currency: k.currency})
		return used, row.daysPastDue, ECLObligorCreditLine, true
	}
	return 0, 0, "", false
}

// updateECLStageLocked re-stages a row under the current configuration
// and, when the stage changed, records the new stage, emits the
// ecl_stage_migration audit event, and bumps the stage-migration
// metric. The first staging (from the zero stage, before the row ever
// had a stage) is not a migration — it records silently. Callers must
// hold the write lock.
func (l *Ledger) updateECLStageLocked(k eclExposureKey, row *eclExposureRow, dpd, exposure int64) {
	to := l.eclStageConfig.StageForDPD(dpd)
	if to == row.stage {
		return
	}
	from := row.stage
	row.stage = to
	if from == 0 {
		return
	}
	l.eclStageMigrations.Add(1)
	l.emitAudit(AuditEvent{
		Op:            "ecl_stage_migration",
		Actor:         "ECLStaging",
		TraceID:       string(k.account) + "/" + k.currency,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{k.account},
		Details: map[string]any{
			"currency":       k.currency,
			"from_stage":     from,
			"to_stage":       to,
			"days_past_due":  dpd,
			"exposure_cents": exposure,
		},
	})
}

// eclExposureViewLocked builds the read-side view of one exposure row.
// Callers must hold l.mu; the read lock suffices.
func (l *Ledger) eclExposureViewLocked(k eclExposureKey, row eclExposureRow) ECLExposure {
	exposure, dpd, source, _ := l.eclExposureLocked(k, row)
	return ECLExposure{
		Account:          k.account,
		Currency:         k.currency,
		Source:           source,
		DaysPastDue:      dpd,
		Stage:            row.stage,
		ExposureCents:    exposure,
		ProvisionedCents: row.provisionedCents,
		UpdatedAt:        row.updatedAt,
	}
}

// eclExposureRecordsLocked is the sorted exposure-registry listing used
// by the snapshot config export. Callers must hold l.mu; the read lock
// suffices.
func (l *Ledger) eclExposureRecordsLocked() []ECLExposureRecord {
	out := make([]ECLExposureRecord, 0, len(l.eclExposures))
	for k, row := range l.eclExposures {
		out = append(out, ECLExposureRecord{
			Account:          k.account,
			Currency:         k.currency,
			ManualCents:      row.manualCents,
			DaysPastDue:      row.daysPastDue,
			Stage:            row.stage,
			ProvisionedCents: row.provisionedCents,
			UpdatedAt:        row.updatedAt,
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

// ECLExposures lists every tracked exposure row, sorted by (account,
// currency): the read side of the registry, backing GET /ecl/exposures
// and the Reconcile disclosure. Rows whose exposure resolves to zero
// (DPD tracking only, or a repaid credit line) are listed with
// exposure_cents 0.
func (l *Ledger) ECLExposures() []ECLExposure {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]ECLExposure, 0, len(l.eclExposures))
	for k, row := range l.eclExposures {
		out = append(out, l.eclExposureViewLocked(k, row))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Account != out[j].Account {
			return out[i].Account < out[j].Account
		}
		return out[i].Currency < out[j].Currency
	})
	return out
}

// BookECLProvision books one day's expected credit loss provision for
// an obligor: debit the configured credit-loss expense account, credit
// the loss-allowance account, for floor(exposure * stageRateBps /
// 10000) cents — the same overflow-safe basis-point math the transfer
// fee policy uses (see policyFeeCents). asof sets both the entry's
// timestamp and its calendar day (UTC); a zero asof means now.
//
// The exposure resolves per eclExposureLocked: a manually registered
// exposure wins, otherwise the granted credit line's drawn utilization.
// The stage follows the row's DPD under the current staging
// configuration, and a stage change is audited as ecl_stage_migration
// before anything books.
//
// The call is idempotent per (account, currency, calendar day): the
// entry ID is the deterministic "ecl/<account>/<currency>/<YYYY-MM-DD>",
// so re-running a day's provisioning returns the original entry with
// duplicate == true and books nothing new. A dust provision (zero cents
// after flooring, or zero exposure) books nothing and returns
// ErrNonPositiveAmount, like the accruals.
//
// Both booking accounts are fail-closed (see SetECLLossExpense and
// SetECLAllowance): with either unconfigured the call refuses, so a
// provision is never booked against a zero account.
//
// The entry posts through the system path: it never fires the journal
// rules engine, never enters dual-control review, and the
// dormant-account stop does not apply (see the accrual convention in
// accrual.go).
func (l *Ledger) BookECLProvision(account AccountID, currency string, asof time.Time) (JournalEntry, bool, error) {
	if err := checkECLAccount(account); err != nil {
		return JournalEntry{}, false, err
	}
	cur, err := normalizeCurrency(currency)
	if err != nil {
		return JournalEntry{}, false, err
	}
	if asof.IsZero() {
		asof = time.Now()
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	k := eclExposureKey{account: account, currency: cur}
	row := l.eclExposures[k]
	exposure, dpd, _, ok := l.eclExposureLocked(k, row)
	if !ok {
		return JournalEntry{}, false, ErrECLExposureNotFound
	}
	// Re-stage on every provision run: the DPD may have been set when a
	// different configuration was in force, and the migration event is
	// the record of when the books recognized the move.
	l.updateECLStageLocked(k, &row, dpd, exposure)
	l.eclExposures[k] = row
	stage := l.eclStageConfig.StageForDPD(dpd)
	if l.eclExpenseAccount == "" {
		return JournalEntry{}, false, ErrECLLossExpenseNotConfigured
	}
	if l.eclAllowanceAccount == "" {
		return JournalEntry{}, false, ErrECLAllowanceNotConfigured
	}
	if exposure <= 0 {
		return JournalEntry{}, false, ErrNonPositiveAmount
	}
	amount := policyFeeCents(exposure, l.eclStageConfig.RateForStage(stage))
	if amount <= 0 {
		return JournalEntry{}, false, ErrNonPositiveAmount
	}
	entryID := eclEntryID(account, cur, asof)
	e := JournalEntry{
		ID:             entryID,
		DebitAccount:   l.eclExpenseAccount,
		CreditAccount:  l.eclAllowanceAccount,
		AmountCents:    amount,
		Currency:       cur,
		IdempotencyKey: entryID,
		Memo:           "ecl:provision:" + string(account),
		CreatedAt:      asof,
	}
	posted, duplicate, err := l.postSystemLocked(e, false)
	if err != nil {
		return JournalEntry{}, false, err
	}
	if !duplicate {
		row.provisionedCents += amount
		row.updatedAt = asof
		l.eclExposures[k] = row
		l.eclProvisionsCommitted.Add(1)
	}
	return posted, duplicate, nil
}

// ECLProvisionCount reports how many ECL provision journal entries this
// ledger has committed. Idempotent replays are not counted.
func (l *Ledger) ECLProvisionCount() uint64 {
	return l.eclProvisionsCommitted.Load()
}

// ECLStageMigrationCount reports how many ECL stage migrations this
// ledger has recorded (the durable record is the ecl_stage_migration
// audit events).
func (l *Ledger) ECLStageMigrationCount() uint64 {
	return l.eclStageMigrations.Load()
}

// WithECLLossExpense configures the ledger-wide credit-loss expense
// account that ECL provisions debit (see BookECLProvision).
func WithECLLossExpense(a AccountID) Option {
	return func(l *Ledger) {
		l.eclExpenseAccount = a
	}
}

// WithECLAllowance configures the ledger-wide loss-allowance account
// that ECL provisions credit (see BookECLProvision).
func WithECLAllowance(a AccountID) Option {
	return func(l *Ledger) {
		l.eclAllowanceAccount = a
	}
}

// SetECLLossExpense configures (or clears, with the empty account) the
// ledger-wide credit-loss expense account that ECL provisions debit.
// Empty means unconfigured: BookECLProvision is fail-closed until the
// operator configures it. Structural config, like feeRevenueAccount —
// it does not bump the journal version and it survives
// disaster-recovery snapshots.
func (l *Ledger) SetECLLossExpense(a AccountID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.eclExpenseAccount = a
	l.emitAudit(AuditEvent{
		Op:      "ecl_loss_expense_set",
		Actor:   "SetECLLossExpense",
		TraceID: string(a),
		Details: map[string]any{"ecl_loss_expense_account": string(a)},
	})
}

// SetECLAllowance configures (or clears, with the empty account) the
// ledger-wide loss-allowance account that ECL provisions credit.
// Empty means unconfigured: BookECLProvision is fail-closed until the
// operator configures it. Structural config — it does not bump the
// journal version and it survives disaster-recovery snapshots.
func (l *Ledger) SetECLAllowance(a AccountID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.eclAllowanceAccount = a
	l.emitAudit(AuditEvent{
		Op:      "ecl_allowance_set",
		Actor:   "SetECLAllowance",
		TraceID: string(a),
		Details: map[string]any{"ecl_allowance_account": string(a)},
	})
}
