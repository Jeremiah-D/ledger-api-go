package ledger

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestECLStageConfigValidation(t *testing.T) {
	valid := DefaultECLStageConfig()
	if err := valid.Validate(); err != nil {
		t.Fatalf("default config: %v", err)
	}
	cases := []struct {
		name string
		cfg  ECLStageConfig
	}{
		{"negative stage2 dpd", ECLStageConfig{Stage2DPD: -1, Stage3DPD: 90, RateStage1Bps: 100, RateStage2Bps: 1000, RateStage3Bps: 5000}},
		{"negative stage3 dpd", ECLStageConfig{Stage2DPD: 30, Stage3DPD: -1, RateStage1Bps: 100, RateStage2Bps: 1000, RateStage3Bps: 5000}},
		{"equal boundaries", ECLStageConfig{Stage2DPD: 30, Stage3DPD: 30, RateStage1Bps: 100, RateStage2Bps: 1000, RateStage3Bps: 5000}},
		{"inverted boundaries", ECLStageConfig{Stage2DPD: 90, Stage3DPD: 30, RateStage1Bps: 100, RateStage2Bps: 1000, RateStage3Bps: 5000}},
		{"negative rate", ECLStageConfig{Stage2DPD: 30, Stage3DPD: 90, RateStage1Bps: -1, RateStage2Bps: 1000, RateStage3Bps: 5000}},
		{"rate over 100%", ECLStageConfig{Stage2DPD: 30, Stage3DPD: 90, RateStage1Bps: 100, RateStage2Bps: 1000, RateStage3Bps: 10001}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.cfg.Validate(); !errors.Is(err, ErrInvalidECLStageConfig) {
				t.Errorf("err = %v, want ErrInvalidECLStageConfig", err)
			}
			l := New()
			if err := l.SetECLStageConfig(tc.cfg); !errors.Is(err, ErrInvalidECLStageConfig) {
				t.Errorf("SetECLStageConfig err = %v, want ErrInvalidECLStageConfig", err)
			}
			// A rejected config leaves the current one untouched.
			if got := l.ECLStageConfig(); got != DefaultECLStageConfig() {
				t.Errorf("config changed on rejected set: %+v", got)
			}
		})
	}
	// A zero stage-1 rate is legal config (dust then refuses the booking).
	zero := ECLStageConfig{Stage2DPD: 10, Stage3DPD: 20, RateStage1Bps: 0, RateStage2Bps: 5000, RateStage3Bps: 10000}
	if err := zero.Validate(); err != nil {
		t.Errorf("zero stage-1 rate: %v", err)
	}
}

func TestECLStageBoundaries(t *testing.T) {
	cfg := DefaultECLStageConfig()
	cases := []struct {
		dpd   int64
		stage int
	}{
		{0, 1}, {29, 1}, {30, 2}, {31, 2}, {89, 2}, {90, 3}, {91, 3}, {1000, 3},
	}
	for _, tc := range cases {
		if got := cfg.StageForDPD(tc.dpd); got != tc.stage {
			t.Errorf("StageForDPD(%d) = %d, want %d", tc.dpd, got, tc.stage)
		}
	}
	if got := cfg.RateForStage(1); got != 100 {
		t.Errorf("RateForStage(1) = %d, want 100", got)
	}
	if got := cfg.RateForStage(2); got != 1000 {
		t.Errorf("RateForStage(2) = %d, want 1000", got)
	}
	if got := cfg.RateForStage(3); got != 5000 {
		t.Errorf("RateForStage(3) = %d, want 5000", got)
	}
}

func TestECLRegisterExposure_HappyPath(t *testing.T) {
	l := New()
	exp, err := l.RegisterECLExposure("borrower", "USD", 100_000, 10)
	if err != nil {
		t.Fatalf("RegisterECLExposure: %v", err)
	}
	if exp.Account != "borrower" || exp.Currency != "USD" || exp.ExposureCents != 100_000 {
		t.Errorf("exposure = %+v", exp)
	}
	if exp.Source != ECLObligorManual {
		t.Errorf("source = %q, want manual", exp.Source)
	}
	if exp.Stage != 1 || exp.DaysPastDue != 10 {
		t.Errorf("stage/dpd = %d/%d, want 1/10", exp.Stage, exp.DaysPastDue)
	}
	// Empty currency normalizes to the default.
	exp2, err := l.RegisterECLExposure("borrower2", "", 50_000, 45)
	if err != nil || exp2.Currency != "USD" || exp2.Stage != 2 {
		t.Errorf("empty currency: %+v err=%v, want USD/stage 2", exp2, err)
	}
}

func TestECLRegisterExposure_Validation(t *testing.T) {
	l := New()
	cases := []struct {
		name                    string
		account                 AccountID
		exposure, dpd           int64
		currency                string
		want                    error
	}{
		{"empty account", "", 1000, 0, "USD", ErrEmptyECLAccount},
		{"slash account", "a/b", 1000, 0, "USD", ErrECLAccountSeparator},
		{"zero exposure", "x", 0, 0, "USD", ErrECLNonPositiveExposure},
		{"negative exposure", "x", -5, 0, "USD", ErrECLNonPositiveExposure},
		{"negative dpd", "x", 1000, -1, "USD", ErrECLNegativeDPD},
		{"bad currency", "x", 1000, 0, "usd", ErrInvalidCurrency},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := l.RegisterECLExposure(tc.account, tc.currency, tc.exposure, tc.dpd); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestECLRegisterExposure_ReRegister(t *testing.T) {
	l := New()
	if _, err := l.RegisterECLExposure("b", "USD", 100_000, 10); err != nil {
		t.Fatalf("first register: %v", err)
	}
	exp, err := l.RegisterECLExposure("b", "USD", 200_000, 95)
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if exp.ExposureCents != 200_000 || exp.DaysPastDue != 95 || exp.Stage != 3 {
		t.Errorf("re-registered = %+v, want 200000/95/stage 3", exp)
	}
}

func TestECLSetDaysPastDue_NoExposure(t *testing.T) {
	l := New()
	if err := l.SetECLDaysPastDue("ghost", "USD", 10); !errors.Is(err, ErrECLExposureNotFound) {
		t.Errorf("err = %v, want ErrECLExposureNotFound", err)
	}
	// With a credit line granted, the first DPD update creates the row.
	if _, err := l.GrantCreditLine("trader", 50_000, "USD", 800); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if err := l.SetECLDaysPastDue("trader", "USD", 45); err != nil {
		t.Fatalf("SetECLDaysPastDue: %v", err)
	}
	if err := l.SetECLDaysPastDue("trader", "USD", -1); !errors.Is(err, ErrECLNegativeDPD) {
		t.Errorf("negative dpd err = %v, want ErrECLNegativeDPD", err)
	}
	if err := l.SetECLDaysPastDue("a/b", "USD", 1); !errors.Is(err, ErrECLAccountSeparator) {
		t.Errorf("slash account err = %v, want ErrECLAccountSeparator", err)
	}
}

func eclTestLedger() *Ledger {
	return New(WithECLLossExpense("ecl-expense"), WithECLAllowance("ecl-allowance"))
}

func TestECLBookProvision_ManualHappyPath(t *testing.T) {
	l := eclTestLedger()
	if _, err := l.RegisterECLExposure("borrower", "USD", 100_000, 10); err != nil {
		t.Fatalf("register: %v", err)
	}
	asof := time.Date(2026, 4, 2, 9, 0, 0, 0, time.UTC)
	v0 := l.version
	e, dup, err := l.BookECLProvision("borrower", "USD", asof)
	if err != nil {
		t.Fatalf("BookECLProvision: %v", err)
	}
	if dup {
		t.Error("duplicate = true, want false")
	}
	// floor(100_000 * 100 / 10000) = 1000.
	if e.AmountCents != 1000 {
		t.Errorf("provision = %d, want 1000", e.AmountCents)
	}
	if e.ID != "ecl/borrower/USD/2026-04-02" {
		t.Errorf("entry ID = %q, want deterministic ecl/<account>/<currency>/<date>", e.ID)
	}
	if e.DebitAccount != "ecl-expense" || e.CreditAccount != "ecl-allowance" {
		t.Errorf("legs = debit %q credit %q, want ecl-expense / ecl-allowance", e.DebitAccount, e.CreditAccount)
	}
	if e.Memo != "ecl:provision:borrower" {
		t.Errorf("memo = %q", e.Memo)
	}
	if b := l.Balance("ecl-expense"); b != 1000 {
		t.Errorf("expense balance = %d, want 1000", b)
	}
	if b := l.Balance("ecl-allowance"); b != -1000 {
		t.Errorf("allowance balance = %d, want -1000", b)
	}
	if l.version != v0+1 {
		t.Errorf("version = %d, want %d (one journal row)", l.version, v0+1)
	}
	if n := l.ECLProvisionCount(); n != 1 {
		t.Errorf("ECLProvisionCount = %d, want 1", n)
	}
}

func TestECLBookProvision_IdempotentReplay(t *testing.T) {
	l := eclTestLedger()
	if _, err := l.RegisterECLExposure("borrower", "USD", 100_000, 10); err != nil {
		t.Fatalf("register: %v", err)
	}
	asof := time.Date(2026, 4, 2, 9, 0, 0, 0, time.UTC)
	e1, _, err := l.BookECLProvision("borrower", "USD", asof)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	v1 := l.version
	e2, dup, err := l.BookECLProvision("borrower", "USD", asof)
	if err != nil || !dup {
		t.Fatalf("replay: err=%v dup=%v, want dup=true", err, dup)
	}
	if e2.ID != e1.ID || e2.AmountCents != e1.AmountCents {
		t.Errorf("replay returned %+v, want original %+v", e2, e1)
	}
	if l.version != v1 {
		t.Errorf("version moved on replay: %d -> %d", v1, l.version)
	}
	if b := l.Balance("ecl-expense"); b != 1000 {
		t.Errorf("expense balance after replay = %d, want 1000", b)
	}
	if n := l.ECLProvisionCount(); n != 1 {
		t.Errorf("ECLProvisionCount = %d, want 1 (replays not counted)", n)
	}
	// A new day books a fresh row.
	asof2 := asof.Add(24 * time.Hour)
	e3, dup, err := l.BookECLProvision("borrower", "USD", asof2)
	if err != nil || dup {
		t.Fatalf("next day: err=%v dup=%v", err, dup)
	}
	if e3.ID == e1.ID {
		t.Errorf("next-day entry ID %q collides with %q", e3.ID, e1.ID)
	}
}

func TestECLBookProvision_FailClosed(t *testing.T) {
	// Neither account configured.
	l := New()
	if _, err := l.RegisterECLExposure("b", "USD", 100_000, 0); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, _, err := l.BookECLProvision("b", "USD", time.Now()); !errors.Is(err, ErrECLLossExpenseNotConfigured) {
		t.Errorf("err = %v, want ErrECLLossExpenseNotConfigured", err)
	}
	// Only the expense account configured.
	l2 := New(WithECLLossExpense("ecl-expense"))
	if _, err := l2.RegisterECLExposure("b", "USD", 100_000, 0); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, _, err := l2.BookECLProvision("b", "USD", time.Now()); !errors.Is(err, ErrECLAllowanceNotConfigured) {
		t.Errorf("err = %v, want ErrECLAllowanceNotConfigured", err)
	}
	// Clearing an account re-arms the fail-closed gate.
	l2.SetECLAllowance("ecl-allowance")
	l2.SetECLLossExpense("")
	if _, _, err := l2.BookECLProvision("b", "USD", time.Now()); !errors.Is(err, ErrECLLossExpenseNotConfigured) {
		t.Errorf("after clear: err = %v, want ErrECLLossExpenseNotConfigured", err)
	}
}

func TestECLBookProvision_Dust(t *testing.T) {
	l := eclTestLedger()
	// floor(50 * 100 / 10000) = 0: dust books nothing.
	if _, err := l.RegisterECLExposure("dust", "USD", 50, 0); err != nil {
		t.Fatalf("register: %v", err)
	}
	v0 := l.version
	if _, _, err := l.BookECLProvision("dust", "USD", time.Now()); !errors.Is(err, ErrNonPositiveAmount) {
		t.Errorf("err = %v, want ErrNonPositiveAmount", err)
	}
	if l.version != v0 {
		t.Errorf("version moved on dust: %d -> %d", v0, l.version)
	}
	// A credit-line exposure with nothing drawn is dust too.
	l2 := New(WithECLLossExpense("ecl-expense"), WithECLAllowance("ecl-allowance"), WithOverdraftProtection("t"))
	if _, err := l2.GrantCreditLine("t", 50_000, "USD", 800); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if err := l2.SetECLDaysPastDue("t", "USD", 100); err != nil {
		t.Fatalf("dpd: %v", err)
	}
	if _, _, err := l2.BookECLProvision("t", "USD", time.Now()); !errors.Is(err, ErrNonPositiveAmount) {
		t.Errorf("undrawn line err = %v, want ErrNonPositiveAmount", err)
	}
}

func TestECLBookProvision_Errors(t *testing.T) {
	l := eclTestLedger()
	if _, _, err := l.BookECLProvision("", "USD", time.Now()); !errors.Is(err, ErrEmptyECLAccount) {
		t.Errorf("empty account err = %v", err)
	}
	if _, _, err := l.BookECLProvision("a/b", "USD", time.Now()); !errors.Is(err, ErrECLAccountSeparator) {
		t.Errorf("slash account err = %v", err)
	}
	if _, _, err := l.BookECLProvision("ghost", "USD", time.Now()); !errors.Is(err, ErrECLExposureNotFound) {
		t.Errorf("unknown obligor err = %v", err)
	}
	if _, _, err := l.BookECLProvision("ghost", "usd", time.Now()); !errors.Is(err, ErrInvalidCurrency) {
		t.Errorf("bad currency err = %v", err)
	}
}

func TestECLBookProvision_CreditLineSource(t *testing.T) {
	l := New(WithECLLossExpense("ecl-expense"), WithECLAllowance("ecl-allowance"), WithOverdraftProtection("trader"))
	seedBalance(t, l, "trader", 100_000)
	if _, err := l.GrantCreditLine("trader", 50_000, "USD", 800); err != nil {
		t.Fatalf("grant: %v", err)
	}
	// Draw 40_000: balance -40_000, utilization 40_000.
	if _, _, err := l.Post(JournalEntry{
		ID: "draw", DebitAccount: "vendor", CreditAccount: "trader", AmountCents: 140_000,
	}); err != nil {
		t.Fatalf("draw: %v", err)
	}
	if err := l.SetECLDaysPastDue("trader", "USD", 95); err != nil {
		t.Fatalf("dpd: %v", err)
	}
	asof := time.Date(2026, 4, 2, 9, 0, 0, 0, time.UTC)
	e, dup, err := l.BookECLProvision("trader", "USD", asof)
	if err != nil {
		t.Fatalf("BookECLProvision: %v", err)
	}
	if dup {
		t.Error("duplicate = true, want false")
	}
	// Stage 3 at 95 DPD: floor(40_000 * 5000 / 10000) = 20_000.
	if e.AmountCents != 20_000 {
		t.Errorf("provision = %d, want 20000", e.AmountCents)
	}
	if e.ID != "ecl/trader/USD/2026-04-02" {
		t.Errorf("entry ID = %q", e.ID)
	}
	exps := l.ECLExposures()
	if len(exps) != 1 || exps[0].Source != ECLObligorCreditLine {
		t.Fatalf("exposures = %+v, want one credit_line row", exps)
	}
	if exps[0].ExposureCents != 40_000 || exps[0].Stage != 3 || exps[0].ProvisionedCents != 20_000 {
		t.Errorf("exposure row = %+v, want 40000/stage 3/provisioned 20000", exps[0])
	}
	// The provision books expense vs allowance — the obligor's own
	// balance only moved by the original draw.
	if b := l.Balance("trader"); b != -40_000 {
		t.Errorf("trader balance = %d, want -40000", b)
	}
}

func TestECLBookProvision_ManualWinsOverCreditLine(t *testing.T) {
	l := New(WithECLLossExpense("ecl-expense"), WithECLAllowance("ecl-allowance"), WithOverdraftProtection("trader"))
	seedBalance(t, l, "trader", 100_000)
	if _, err := l.GrantCreditLine("trader", 50_000, "USD", 800); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, _, err := l.Post(JournalEntry{
		ID: "draw", DebitAccount: "vendor", CreditAccount: "trader", AmountCents: 140_000,
	}); err != nil {
		t.Fatalf("draw: %v", err)
	}
	// A manual exposure on the same obligor takes precedence over the
	// line's utilization.
	if _, err := l.RegisterECLExposure("trader", "USD", 10_000, 0); err != nil {
		t.Fatalf("register: %v", err)
	}
	e, _, err := l.BookECLProvision("trader", "USD", time.Date(2026, 4, 2, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("BookECLProvision: %v", err)
	}
	// Stage 1: floor(10_000 * 100 / 10000) = 100, not the 40_000
	// utilization's 400.
	if e.AmountCents != 100 {
		t.Errorf("provision = %d, want 100 (manual exposure wins)", e.AmountCents)
	}
}

func TestECLStageMigration_AuditEvent(t *testing.T) {
	al := mustAuditLog(t)
	defer al.Close()
	l := New(WithAuditLog(al), WithECLLossExpense("ecl-expense"), WithECLAllowance("ecl-allowance"))

	if _, err := l.RegisterECLExposure("b", "USD", 100_000, 10); err != nil {
		t.Fatalf("register: %v", err)
	}
	// First staging is silent: no migration from the zero stage.
	if n := l.ECLStageMigrationCount(); n != 0 {
		t.Fatalf("migrations after first staging = %d, want 0", n)
	}
	// 10 -> 45 DPD: stage 1 -> 2 fires one migration.
	if err := l.SetECLDaysPastDue("b", "USD", 45); err != nil {
		t.Fatalf("dpd: %v", err)
	}
	// Same stage again: no second event.
	if err := l.SetECLDaysPastDue("b", "USD", 50); err != nil {
		t.Fatalf("dpd: %v", err)
	}
	// 50 -> 120 DPD: stage 2 -> 3 fires another.
	if _, _, err := l.BookECLProvision("b", "USD", time.Date(2026, 4, 2, 9, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("provision: %v", err)
	}
	_ = l.SetECLDaysPastDue("b", "USD", 120)

	if n := l.ECLStageMigrationCount(); n != 2 {
		t.Fatalf("ECLStageMigrationCount = %d, want 2", n)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, corrupt, err := ReadAuditLog(al.Dir(), time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt = %d, want 0", corrupt)
	}
	var migrations []AuditEvent
	for _, ev := range events {
		if ev.Op == "ecl_stage_migration" {
			migrations = append(migrations, ev)
		}
	}
	if len(migrations) != 2 {
		t.Fatalf("ecl_stage_migration events = %d, want 2", len(migrations))
	}
	m1 := migrations[0]
	if m1.Details["from_stage"] != float64(1) || m1.Details["to_stage"] != float64(2) {
		t.Errorf("migration[0] details = %v, want from 1 to 2", m1.Details)
	}
	if m1.Details["days_past_due"] != float64(45) {
		t.Errorf("migration[0] dpd = %v, want 45", m1.Details["days_past_due"])
	}
	m2 := migrations[1]
	if m2.Details["from_stage"] != float64(2) || m2.Details["to_stage"] != float64(3) {
		t.Errorf("migration[1] details = %v, want from 2 to 3", m2.Details)
	}
}

func TestECLReconcileCoverage(t *testing.T) {
	l := eclTestLedger()
	if _, err := l.RegisterECLExposure("b1", "USD", 100_000, 10); err != nil {
		t.Fatalf("register b1: %v", err)
	}
	if _, err := l.RegisterECLExposure("b2", "USD", 200_000, 45); err != nil {
		t.Fatalf("register b2: %v", err)
	}
	asof := time.Date(2026, 4, 2, 9, 0, 0, 0, time.UTC)
	if _, _, err := l.BookECLProvision("b1", "USD", asof); err != nil {
		t.Fatalf("provision b1: %v", err)
	}
	if _, _, err := l.BookECLProvision("b2", "USD", asof); err != nil {
		t.Fatalf("provision b2: %v", err)
	}
	// b1 stage 1: floor(100_000 * 100 / 10000) = 1000.
	// b2 stage 2: floor(200_000 * 1000 / 10000) = 20_000.
	report := l.Reconcile(time.Now())
	if len(report.ECLExposures) != 2 {
		t.Fatalf("ECLExposures = %d rows, want 2", len(report.ECLExposures))
	}
	r1, r2 := report.ECLExposures[0], report.ECLExposures[1]
	if r1.Account != "b1" || r2.Account != "b2" {
		t.Fatalf("rows not sorted by account: %+v / %+v", r1, r2)
	}
	if r1.Stage != 1 || r1.ExposureCents != 100_000 || r1.ProvisionedCents != 1000 {
		t.Errorf("b1 row = %+v", r1)
	}
	if r2.Stage != 2 || r2.ExposureCents != 200_000 || r2.ProvisionedCents != 20_000 {
		t.Errorf("b2 row = %+v", r2)
	}
	if len(report.ECLCoverage) != 1 {
		t.Fatalf("ECLCoverage = %d rows, want 1", len(report.ECLCoverage))
	}
	cov := report.ECLCoverage[0]
	if cov.Currency != "USD" || cov.ExposureCents != 300_000 || cov.ProvisionedCents != 21_000 {
		t.Errorf("coverage = %+v, want USD/300000/21000", cov)
	}
	if want := 21_000.0 / 300_000.0; cov.CoverageRatio != want {
		t.Errorf("coverage ratio = %v, want %v", cov.CoverageRatio, want)
	}
	// A zero-exposure tracking row stays out of the disclosure.
	l2 := eclTestLedger()
	if _, err := l2.GrantCreditLine("t", 50_000, "USD", 800); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if err := l2.SetECLDaysPastDue("t", "USD", 10); err != nil {
		t.Fatalf("dpd: %v", err)
	}
	report2 := l2.Reconcile(time.Now())
	if len(report2.ECLExposures) != 0 || len(report2.ECLCoverage) != 0 {
		t.Errorf("zero-exposure disclosure = %+v / %+v, want empty",
			report2.ECLExposures, report2.ECLCoverage)
	}
}

func TestECLSnapshotRoundTrip(t *testing.T) {
	src := New(WithECLLossExpense("ecl-expense"), WithECLAllowance("ecl-allowance"))
	if err := src.SetECLStageConfig(ECLStageConfig{
		Stage2DPD: 20, Stage3DPD: 60,
		RateStage1Bps: 50, RateStage2Bps: 2000, RateStage3Bps: 8000,
	}); err != nil {
		t.Fatalf("config: %v", err)
	}
	if _, err := src.RegisterECLExposure("b", "USD", 100_000, 25); err != nil {
		t.Fatalf("register: %v", err)
	}
	asof := time.Date(2026, 4, 2, 9, 0, 0, 0, time.UTC)
	if _, _, err := src.BookECLProvision("b", "USD", asof); err != nil {
		t.Fatalf("provision: %v", err)
	}

	var buf bytes.Buffer
	if err := src.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if !snapshotLedgersEqual(src, restored) {
		t.Fatal("restored ledger state differs from source")
	}
	if got := restored.ECLStageConfig(); got.Stage2DPD != 20 || got.RateStage3Bps != 8000 {
		t.Errorf("restored config = %+v", got)
	}
	exps := restored.ECLExposures()
	if len(exps) != 1 || exps[0].ProvisionedCents != 20_000 || exps[0].Stage != 2 {
		t.Errorf("restored exposures = %+v, want one row stage 2 provisioned 20000", exps)
	}
	// The restored ledger keeps provisioning idempotently on the same day.
	if _, dup, err := restored.BookECLProvision("b", "USD", asof); err != nil || !dup {
		t.Errorf("restored replay: err=%v dup=%v, want dup=true", err, dup)
	}
}

func TestECLConcurrent(t *testing.T) {
	l := eclTestLedger()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			acct := AccountID("conc")
			if _, err := l.RegisterECLExposure(acct, "USD", int64(100_000+i), int64(i*10)); err != nil {
				t.Errorf("register: %v", err)
				return
			}
			if err := l.SetECLDaysPastDue(acct, "USD", int64(i*10)); err != nil {
				t.Errorf("dpd: %v", err)
				return
			}
			asof := time.Date(2026, 4, 2, 9, 0, 0, 0, time.UTC).Add(time.Duration(i) * 24 * time.Hour)
			if _, _, err := l.BookECLProvision(acct, "USD", asof); err != nil {
				t.Errorf("provision: %v", err)
				return
			}
			_ = l.ECLExposures()
			_ = l.Reconcile(time.Now())
			_ = l.ECLProvisionCount()
			_ = l.ECLStageMigrationCount()
		}(i)
	}
	wg.Wait()
}
