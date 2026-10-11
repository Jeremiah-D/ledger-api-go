package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// fundReval posts a EUR balance for the revaluation tests: debit acct /
// credit bank in EUR.
func fundReval(t *testing.T, l *Ledger, id, acct string, cents int64) {
	t.Helper()
	if _, _, err := l.Post(JournalEntry{
		ID: id, DebitAccount: AccountID(acct), CreditAccount: "bank",
		AmountCents: cents, Currency: "EUR",
	}); err != nil {
		t.Fatalf("fund %s: %v", id, err)
	}
}

func mustSetFXRate(t *testing.T, l *Ledger, from, to string, num, den int64) {
	t.Helper()
	if err := l.SetFXRate(from, to, num, den); err != nil {
		t.Fatalf("SetFXRate %s->%s: %v", from, to, err)
	}
}

// findRevalItem returns the report item for the given account, or fails
// the test. fundReval always creates two EUR legs (acct and bank), so
// reports routinely carry both.
func findRevalItem(t *testing.T, report RevaluationReport, account string) RevaluationItem {
	t.Helper()
	for _, it := range report.Items {
		if string(it.Account) == account {
			return it
		}
	}
	t.Fatalf("no item for account %q in %+v", account, report.Items)
	return RevaluationItem{}
}

func TestRevalueBalancesNotConfigured(t *testing.T) {
	l := New()
	if _, err := l.RevalueBalances(time.Now(), "USD"); !errors.Is(err, ErrRevaluationAccountNotConfigured) {
		t.Fatalf("err = %v, want ErrRevaluationAccountNotConfigured", err)
	}
}

func TestRevalueBalancesBadBaseCurrency(t *testing.T) {
	l := New(WithRevaluationAccount("fx-pnl"))
	if _, err := l.RevalueBalances(time.Now(), "US"); !errors.Is(err, ErrInvalidCurrency) {
		t.Fatalf("err = %v, want ErrInvalidCurrency", err)
	}
}

func TestRevalueBalancesFirstRunBooksFullValue(t *testing.T) {
	l := New(WithRevaluationAccount("fx-pnl"))
	fundReval(t, l, "f1", "trader", 10000) // EUR 100
	mustSetFXRate(t, l, "EUR", "USD", 11, 10)

	asOf := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	report, err := l.RevalueBalances(asOf, "USD")
	if err != nil {
		t.Fatalf("RevalueBalances: %v", err)
	}
	// Two legs were funded (trader + bank), so two items; the trader's is
	// the initial recognition of EUR 100 at 1.10.
	it := findRevalItem(t, report, "trader")
	if it.Status != RevalStatusRevalued {
		t.Fatalf("status = %q, want revalued", it.Status)
	}
	if it.PriorBaseValue != 0 || it.NewBaseValue != 11000 || it.DeltaCents != 11000 {
		t.Fatalf("item = %+v, want prior 0 / new 11000 / delta 11000", it)
	}
	if len(report.Entries) != 2 {
		t.Fatalf("entries = %d, want 2 (trader + bank legs)", len(report.Entries))
	}
	// The booked carrying value equals the baseline from now on.
	if got := l.BalanceIn("trader", "USD"); got != 11000 {
		t.Fatalf("trader USD carrying = %d, want 11000", got)
	}
	// The EUR balance is untouched.
	if got := l.BalanceIn("trader", "EUR"); got != 10000 {
		t.Fatalf("EUR balance = %d, want 10000", got)
	}
	// The funding legs offset in P&L: no phantom profit from funding.
	if got := l.BalanceIn("fx-pnl", "USD"); got != 0 {
		t.Fatalf("fx-pnl = %d, want 0 (offsetting legs)", got)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Fatalf("VerifyAccountingEquation: %v", err)
	}
}

func TestRevalueBalancesBooksGainOnRateMove(t *testing.T) {
	l := New(WithRevaluationAccount("fx-pnl"))
	fundReval(t, l, "f1", "trader", 10000)
	mustSetFXRate(t, l, "EUR", "USD", 11, 10)
	asOf := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	if _, err := l.RevalueBalances(asOf, "USD"); err != nil {
		t.Fatalf("baseline run: %v", err)
	}

	// Rate moves 1.10 -> 1.20: the EUR 100 is now worth USD 120.
	mustSetFXRate(t, l, "EUR", "USD", 12, 10)
	report, err := l.RevalueBalances(asOf.Add(time.Hour), "USD")
	if err != nil {
		t.Fatalf("RevalueBalances: %v", err)
	}
	if len(report.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(report.Entries))
	}
	var traderEntry *JournalEntry
	for i := range report.Entries {
		if report.Entries[i].DebitAccount == "trader" || report.Entries[i].CreditAccount == "trader" {
			traderEntry = &report.Entries[i]
		}
	}
	if traderEntry == nil {
		t.Fatalf("no trader entry in %+v", report.Entries)
	}
	e := *traderEntry
	if e.AmountCents != 1000 || e.Currency != "USD" {
		t.Fatalf("entry = %d %s, want 1000 USD", e.AmountCents, e.Currency)
	}
	if e.DebitAccount != "trader" || e.CreditAccount != "fx-pnl" {
		t.Fatalf("gain legs = %s/%s, want trader/fx-pnl", e.DebitAccount, e.CreditAccount)
	}
	if !strings.HasPrefix(e.Memo, "reval:EUR->USD:asof=") {
		t.Fatalf("memo = %q", e.Memo)
	}
	it := findRevalItem(t, report, "trader")
	if it.Status != RevalStatusRevalued || it.DeltaCents != 1000 {
		t.Fatalf("item = %+v, want revalued delta 1000", it)
	}
	// Carrying value tracks the baseline.
	if got := l.BalanceIn("trader", "USD"); got != 12000 {
		t.Fatalf("trader USD carrying = %d, want 12000", got)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Fatalf("VerifyAccountingEquation: %v", err)
	}
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
}

func TestRevalueBalancesBooksLoss(t *testing.T) {
	l := New(WithRevaluationAccount("fx-pnl"))
	fundReval(t, l, "f1", "trader", 10000)
	mustSetFXRate(t, l, "EUR", "USD", 12, 10)
	asOf := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	if _, err := l.RevalueBalances(asOf, "USD"); err != nil {
		t.Fatalf("baseline run: %v", err)
	}
	// Rate drops 1.20 -> 1.10.
	mustSetFXRate(t, l, "EUR", "USD", 11, 10)
	report, err := l.RevalueBalances(asOf.Add(time.Hour), "USD")
	if err != nil {
		t.Fatalf("RevalueBalances: %v", err)
	}
	if len(report.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(report.Entries))
	}
	var traderEntry *JournalEntry
	for i := range report.Entries {
		if report.Entries[i].DebitAccount == "trader" || report.Entries[i].CreditAccount == "trader" {
			traderEntry = &report.Entries[i]
		}
	}
	if traderEntry == nil {
		t.Fatalf("no trader entry in %+v", report.Entries)
	}
	e := *traderEntry
	if e.AmountCents != 1000 {
		t.Fatalf("amount = %d, want 1000", e.AmountCents)
	}
	if e.DebitAccount != "fx-pnl" || e.CreditAccount != "trader" {
		t.Fatalf("loss legs = %s/%s, want fx-pnl/trader", e.DebitAccount, e.CreditAccount)
	}
	if it := findRevalItem(t, report, "trader"); it.DeltaCents != -1000 {
		t.Fatalf("delta = %d, want -1000", it.DeltaCents)
	}
}

func TestRevalueBalancesIdempotentSameAsOf(t *testing.T) {
	l := New(WithRevaluationAccount("fx-pnl"))
	fundReval(t, l, "f1", "trader", 10000)
	mustSetFXRate(t, l, "EUR", "USD", 11, 10)
	asOf := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	if _, err := l.RevalueBalances(asOf, "USD"); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	mustSetFXRate(t, l, "EUR", "USD", 12, 10)
	asOf2 := asOf.Add(time.Hour)
	r1, err := l.RevalueBalances(asOf2, "USD")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if len(r1.Entries) != 2 {
		t.Fatalf("first entries = %d, want 2", len(r1.Entries))
	}
	// Same asOf again: the deterministic ID makes it a no-op.
	r2, err := l.RevalueBalances(asOf2, "USD")
	if err != nil {
		t.Fatalf("repeat: %v", err)
	}
	if len(r2.Entries) != 0 {
		t.Fatalf("repeat entries = %d, want 0", len(r2.Entries))
	}
	if it := findRevalItem(t, r2, "trader"); it.Status != RevalStatusSkippedDuplicate {
		t.Fatalf("repeat status = %q, want skipped_duplicate", it.Status)
	}
	// Unchanged book, new asOf: delta is zero, nothing booked.
	r3, err := l.RevalueBalances(asOf2.Add(time.Hour), "USD")
	if err != nil {
		t.Fatalf("third: %v", err)
	}
	if len(r3.Entries) != 0 {
		t.Fatalf("third entries = %d, want 0", len(r3.Entries))
	}
	if it := findRevalItem(t, r3, "trader"); it.Status != RevalStatusUnchanged {
		t.Fatalf("third status = %q, want unchanged", it.Status)
	}
}

func TestRevalueBalancesSkipsMissingRate(t *testing.T) {
	l := New(WithRevaluationAccount("fx-pnl"))
	fundReval(t, l, "f1", "trader", 10000) // EUR, but no EUR->USD rate
	report, err := l.RevalueBalances(time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC), "USD")
	if err != nil {
		t.Fatalf("RevalueBalances: %v", err)
	}
	if report.Items[0].Status != RevalStatusSkippedNoRate {
		t.Fatalf("status = %q, want skipped_no_rate", report.Items[0].Status)
	}
	if len(report.Entries) != 0 {
		t.Fatalf("entries = %d, want 0", len(report.Entries))
	}
	// Installing the rate later measures from scratch (no baseline was set).
	mustSetFXRate(t, l, "EUR", "USD", 11, 10)
	report, err = l.RevalueBalances(time.Date(2026, 10, 11, 13, 0, 0, 0, time.UTC), "USD")
	if err != nil {
		t.Fatalf("RevalueBalances: %v", err)
	}
	if it := findRevalItem(t, report, "trader"); it.Status != RevalStatusRevalued || it.DeltaCents != 11000 {
		t.Fatalf("item = %+v, want revalued delta 11000", it)
	}
}

func TestRevalueBalancesSkipsBaseCurrencyBalances(t *testing.T) {
	l := New(WithRevaluationAccount("fx-pnl"))
	if _, _, err := l.Post(JournalEntry{ID: "u1", DebitAccount: "trader", CreditAccount: "bank", AmountCents: 500, Currency: "USD"}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	mustSetFXRate(t, l, "EUR", "USD", 11, 10)
	report, err := l.RevalueBalances(time.Now(), "USD")
	if err != nil {
		t.Fatalf("RevalueBalances: %v", err)
	}
	if len(report.Items) != 0 {
		t.Fatalf("items = %d, want 0 (USD balances are not revalued into USD)", len(report.Items))
	}
}

func TestRevalueBalancesZeroBalanceReversesCarryingValue(t *testing.T) {
	l := New(WithRevaluationAccount("fx-pnl"))
	fundReval(t, l, "f1", "trader", 10000)
	mustSetFXRate(t, l, "EUR", "USD", 11, 10)
	asOf := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	if _, err := l.RevalueBalances(asOf, "USD"); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	mustSetFXRate(t, l, "EUR", "USD", 12, 10)
	if _, err := l.RevalueBalances(asOf.Add(time.Hour), "USD"); err != nil {
		t.Fatalf("gain: %v", err)
	}
	if got := l.BalanceIn("trader", "USD"); got != 12000 {
		t.Fatalf("carrying = %d, want 12000", got)
	}
	// Drain the EUR balance to zero: the next revaluation reverses the
	// full carrying value to zero so no stale value lingers.
	if _, _, err := l.Post(JournalEntry{ID: "d1", DebitAccount: "bank", CreditAccount: "trader", AmountCents: 10000, Currency: "EUR"}); err != nil {
		t.Fatalf("drain: %v", err)
	}
	report, err := l.RevalueBalances(asOf.Add(2*time.Hour), "USD")
	if err != nil {
		t.Fatalf("RevalueBalances: %v", err)
	}
	it := findRevalItem(t, report, "trader")
	if it.Status != RevalStatusRevalued || it.DeltaCents != -12000 {
		t.Fatalf("trader item = %+v, want revalued delta -12000", it)
	}
	if got := l.BalanceIn("trader", "USD"); got != 0 {
		t.Fatalf("carrying = %d, want 0", got)
	}
	if got := l.BalanceIn("bank", "USD"); got != 0 {
		t.Fatalf("bank carrying = %d, want 0", got)
	}
}

func TestRevalueBalancesDoesNotTriggerJournalRules(t *testing.T) {
	l := New(WithRevaluationAccount("fx-pnl"))
	fundReval(t, l, "f1", "trader", 10000)
	mustSetFXRate(t, l, "EUR", "USD", 11, 10)
	// A rule matching every USD debit would fire on the revaluation entry
	// if rules applied — it must not.
	if err := l.SetJournalRule(JournalRule{
		ID:            "r1",
		MatchCurrency: "USD",
		Legs:          []RuleLeg{{DebitAccount: "tax", CreditAccount: "trader", RateBps: 10000}},
	}); err != nil {
		t.Fatalf("SetJournalRule: %v", err)
	}
	asOf := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	if _, err := l.RevalueBalances(asOf, "USD"); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	mustSetFXRate(t, l, "EUR", "USD", 12, 10)
	report, err := l.RevalueBalances(asOf.Add(time.Hour), "USD")
	if err != nil {
		t.Fatalf("RevalueBalances: %v", err)
	}
	// Two entries (trader + bank legs), and crucially no third entry from
	// the journal rule.
	if len(report.Entries) != 2 {
		t.Fatalf("entries = %d, want exactly 2 (no rule legs)", len(report.Entries))
	}
	for _, e := range report.Entries {
		if strings.HasPrefix(e.ID, "r1") || e.Memo == "rule:r1" {
			t.Fatalf("rule leg booked: %+v", e)
		}
	}
	if got := l.BalanceIn("tax", "USD"); got != 0 {
		t.Fatalf("tax balance = %d, want 0 (rule must not fire)", got)
	}
}

func TestRevalueBalancesFrozenAccountFailsItem(t *testing.T) {
	l := New(WithRevaluationAccount("fx-pnl"))
	fundReval(t, l, "f1", "trader", 10000)
	fundReval(t, l, "f2", "other", 5000)
	mustSetFXRate(t, l, "EUR", "USD", 11, 10)
	asOf := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	if _, err := l.RevalueBalances(asOf, "USD"); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	l.Freeze("trader")
	mustSetFXRate(t, l, "EUR", "USD", 12, 10)
	report, err := l.RevalueBalances(asOf.Add(time.Hour), "USD")
	if err != nil {
		t.Fatalf("RevalueBalances: %v", err)
	}
	// Best-effort: the frozen account fails, the healthy one revalues.
	byAcct := map[AccountID]RevaluationItem{}
	for _, it := range report.Items {
		byAcct[it.Account] = it
	}
	if byAcct["trader"].Status != RevalStatusFailed || !strings.Contains(byAcct["trader"].Error, "frozen") {
		t.Fatalf("trader item = %+v, want failed/frozen", byAcct["trader"])
	}
	if byAcct["other"].Status != RevalStatusRevalued {
		t.Fatalf("other item = %+v, want revalued", byAcct["other"])
	}
}

func TestRevalueBalancesPeriodClosedFailsItem(t *testing.T) {
	l := New(WithRevaluationAccount("fx-pnl"))
	fundReval(t, l, "f1", "trader", 10000)
	mustSetFXRate(t, l, "EUR", "USD", 11, 10)
	asOf := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	if _, err := l.RevalueBalances(asOf, "USD"); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	if err := l.ClosePeriod("2026-10"); err != nil {
		t.Fatalf("ClosePeriod: %v", err)
	}
	mustSetFXRate(t, l, "EUR", "USD", 12, 10)
	// The revaluation is timestamped at asOf (in the closed period).
	report, err := l.RevalueBalances(asOf.Add(time.Hour), "USD")
	if err != nil {
		t.Fatalf("RevalueBalances: %v", err)
	}
	if report.Items[0].Status != RevalStatusFailed || !strings.Contains(report.Items[0].Error, "closed") {
		t.Fatalf("item = %+v, want failed/closed period", report.Items[0])
	}
}

func TestRevalueBalancesSnapshotRoundTrip(t *testing.T) {
	l := New(WithRevaluationAccount("fx-pnl"))
	fundReval(t, l, "f1", "trader", 10000)
	mustSetFXRate(t, l, "EUR", "USD", 11, 10)
	asOf := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	if _, err := l.RevalueBalances(asOf, "USD"); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	mustSetFXRate(t, l, "EUR", "USD", 12, 10)
	if _, err := l.RevalueBalances(asOf.Add(time.Hour), "USD"); err != nil {
		t.Fatalf("gain: %v", err)
	}

	var sb strings.Builder
	if err := l.ExportSnapshot(&sb); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(strings.NewReader(sb.String()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if !snapshotLedgersEqual(l, restored) {
		t.Fatal("restored ledger differs (reval account/baselines did not round-trip)")
	}
	if restored.RevaluationAccount() != "fx-pnl" {
		t.Fatalf("restored reval account = %q", restored.RevaluationAccount())
	}
	// The restored ledger continues from the baseline: no phantom gain.
	report, err := restored.RevalueBalances(asOf.Add(2*time.Hour), "USD")
	if err != nil {
		t.Fatalf("RevalueBalances on restored: %v", err)
	}
	if len(report.Entries) != 0 || report.Items[0].Status != RevalStatusUnchanged {
		t.Fatalf("restored revaluation = %+v, want unchanged (no re-booked gain)", report.Items)
	}
}

func TestRevalueBalancesReconcileImpact(t *testing.T) {
	l := New(WithRevaluationAccount("fx-pnl"))
	fundReval(t, l, "f1", "trader", 10000)
	mustSetFXRate(t, l, "EUR", "USD", 11, 10)
	asOf := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	if _, err := l.RevalueBalances(asOf, "USD"); err != nil {
		t.Fatalf("RevalueBalances: %v", err)
	}
	report := l.Reconcile(time.Now())
	if report.RevaluationAccount != "fx-pnl" {
		t.Fatalf("revaluation_account = %q", report.RevaluationAccount)
	}
	if len(report.RevaluationImpact) != 2 {
		t.Fatalf("revaluation_impact rows = %d, want 2 (trader + bank)", len(report.RevaluationImpact))
	}
	pos := findRevalPosition(report.RevaluationImpact, "trader")
	if pos.Currency != "EUR" || pos.BaseCurrency != "USD" {
		t.Fatalf("position = %+v", pos)
	}
	if pos.ForeignBalance != 10000 || pos.BaseValue != 11000 {
		t.Fatalf("position balances = %d/%d, want 10000/11000", pos.ForeignBalance, pos.BaseValue)
	}
	if pos.RateNum != 11 || pos.RateDen != 10 || pos.RateVersion == 0 {
		t.Fatalf("position rate = %+v", pos)
	}
}

func findRevalPosition(positions []RevaluationPosition, account string) RevaluationPosition {
	for _, p := range positions {
		if string(p.Account) == account {
			return p
		}
	}
	return RevaluationPosition{}
}

func TestRevalueBalancesExpiredRateSkipped(t *testing.T) {
	l := New(WithRevaluationAccount("fx-pnl"))
	fundReval(t, l, "f1", "trader", 10000)
	// Install a rate that expired before the measurement time.
	rate, err := ParseFXRateDecimal("1.10")
	if err != nil {
		t.Fatalf("ParseFXRateDecimal: %v", err)
	}
	if err := l.SetFXRateRat("EUR", "USD", rate, time.Hour); err != nil {
		t.Fatalf("SetFXRateRat: %v", err)
	}
	asOf := time.Now().Add(2 * time.Hour)
	report, err := l.RevalueBalances(asOf, "USD")
	if err != nil {
		t.Fatalf("RevalueBalances: %v", err)
	}
	if report.Items[0].Status != RevalStatusSkippedRateExpiry {
		t.Fatalf("status = %q, want skipped_rate_expired", report.Items[0].Status)
	}
}
