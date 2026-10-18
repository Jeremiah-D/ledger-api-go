package ledger

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

// mustPostDunning is the local Post helper for dunning tests.
func mustPostDunning(t *testing.T, l *Ledger, id string, debit, credit AccountID, cents int64, at time.Time) {
	t.Helper()
	if _, _, err := l.Post(JournalEntry{ID: id, DebitAccount: debit, CreditAccount: credit,
		AmountCents: cents, Currency: "USD", IdempotencyKey: "k-" + id, CreatedAt: at}); err != nil {
		t.Fatalf("Post %s: %v", id, err)
	}
}

func mustSetDunning(t *testing.T, l *Ledger, account AccountID, currency string, l1, l2, l3 int64) {
	t.Helper()
	if err := l.SetDunningConfig(DunningConfig{Account: account, Currency: currency,
		Level1Days: l1, Level2Days: l2, Level3Days: l3}); err != nil {
		t.Fatalf("SetDunningConfig: %v", err)
	}
}

// TestSetDunningConfigValidation: malformed policies are rejected, and an
// empty currency normalizes to DefaultCurrency like the journal does.
func TestSetDunningConfigValidation(t *testing.T) {
	l := New()
	bad := []DunningConfig{
		{Account: "", Currency: "USD", Level1Days: 30, Level2Days: 60, Level3Days: 90},  // empty account
		{Account: "a", Currency: "US", Level1Days: 30, Level2Days: 60, Level3Days: 90},  // bad currency
		{Account: "a", Currency: "USD", Level1Days: 0, Level2Days: 60, Level3Days: 90},  // L1 <= 0
		{Account: "a", Currency: "USD", Level1Days: 60, Level2Days: 30, Level3Days: 90}, // L2 < L1
		{Account: "a", Currency: "USD", Level1Days: 30, Level2Days: 90, Level3Days: 60}, // L3 < L2
		{Account: "a", Currency: "USD", Level1Days: -5, Level2Days: 60, Level3Days: 90}, // negative L1
	}
	for i, cfg := range bad {
		if err := l.SetDunningConfig(cfg); !errors.Is(err, ErrDunningInvalidConfig) {
			t.Fatalf("config %d: SetDunningConfig = %v, want ErrDunningInvalidConfig", i, err)
		}
	}
	// Equal thresholds are allowed (L1 == L2 == L3: everything is L3).
	mustSetDunning(t, l, "flat", "USD", 30, 30, 30)
	// Empty currency normalizes to DefaultCurrency.
	mustSetDunning(t, l, "norm", "", 30, 60, 90)
	cfg, ok := l.DunningConfigOf("norm", DefaultCurrency)
	if !ok || cfg.Currency != DefaultCurrency {
		t.Fatalf("DunningConfigOf = %+v, %v: want normalized %s", cfg, ok, DefaultCurrency)
	}
	if _, ok := l.DunningConfigOf("nope", "USD"); ok {
		t.Fatal("DunningConfigOf(nope): got config, want none (opt-in)")
	}
}

// TestEscalateDunningProgression: a receivable escalates L1 -> L2 -> L3 as
// the oldest outstanding lot's age crosses the thresholds; a repeat scan
// at the same age escalates nothing.
func TestEscalateDunningProgression(t *testing.T) {
	l := New()
	t0 := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	mustPostDunning(t, l, "d1", "cust", "ar-clearing", 10000, t0.Add(-40*24*time.Hour))
	mustSetDunning(t, l, "cust", "USD", 30, 60, 90)

	esc := l.EscalateDunning(t0)
	if len(esc) != 1 || esc[0].Level != DunningLevelL1 || esc[0].PreviousLevel != DunningLevelNone {
		t.Fatalf("scan 1 = %+v, want single 0->L1 escalation", esc)
	}
	if esc[0].OverdueCents != 10000 || esc[0].DaysPastDue != 40 {
		t.Fatalf("scan 1 = %+v, want 40 days past due on 10000c", esc)
	}
	if got := l.DunningEscalationCount(); got != 1 {
		t.Fatalf("counter = %d, want 1", got)
	}

	// Repeat scan at the same age: no new escalation (edge-triggered).
	if esc := l.EscalateDunning(t0); len(esc) != 0 {
		t.Fatalf("repeat scan = %+v, want empty (already at L1)", esc)
	}
	if got := l.DunningEscalationCount(); got != 1 {
		t.Fatalf("counter = %d, want 1 (no double count)", got)
	}

	// Age to 70 days -> L2.
	esc = l.EscalateDunning(t0.Add(30 * 24 * time.Hour))
	if len(esc) != 1 || esc[0].Level != DunningLevelL2 || esc[0].PreviousLevel != DunningLevelL1 {
		t.Fatalf("scan 2 = %+v, want single L1->L2 escalation", esc)
	}
	st, ok := l.DunningStatusOf("cust", "USD")
	if !ok || st.Level != DunningLevelL2 || st.Escalations != 2 {
		t.Fatalf("state = %+v, %v: want L2 with 2 escalations", st, ok)
	}
	if got := l.DunningEscalationCount(); got != 2 {
		t.Fatalf("counter = %d, want 2", got)
	}
}

// TestEscalateDunningSkipLevels: a long-overdue receivable caught up late
// escalates straight to L3 in one event (not L1 then L2 then L3).
func TestEscalateDunningSkipLevels(t *testing.T) {
	l := New()
	t0 := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	mustPostDunning(t, l, "d1", "cust", "ar-clearing", 5000, t0.Add(-100*24*time.Hour))
	mustSetDunning(t, l, "cust", "USD", 30, 60, 90)

	esc := l.EscalateDunning(t0)
	if len(esc) != 1 {
		t.Fatalf("scan = %+v, want exactly one escalation", esc)
	}
	e := esc[0]
	if e.PreviousLevel != DunningLevelNone || e.Level != DunningLevelL3 {
		t.Fatalf("escalation = %+v, want 0->L3", e)
	}
	if got := l.DunningEscalationCount(); got != 1 {
		t.Fatalf("counter = %d, want 1 (one event for the skip)", got)
	}
	st, _ := l.DunningStatusOf("cust", "USD")
	if st.Escalations != 1 || st.DaysPastDue != 100 || st.OverdueCents != 5000 {
		t.Fatalf("state = %+v, want level 3, 100 days, 5000c", st)
	}
}

// TestEscalateDunningOneWay: paying the receivable off in full does NOT
// de-escalate — levels only move up, cleared only by ResetDunning.
func TestEscalateDunningOneWay(t *testing.T) {
	l := New()
	t0 := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	mustPostDunning(t, l, "d1", "cust", "ar-clearing", 10000, t0.Add(-70*24*time.Hour))
	mustSetDunning(t, l, "cust", "USD", 30, 60, 90)
	if esc := l.EscalateDunning(t0); len(esc) != 1 || esc[0].Level != DunningLevelL2 {
		t.Fatalf("scan 1 = %+v, want 0->L2", esc)
	}
	// Pay it all back: the receivable is gone.
	mustPostDunning(t, l, "d2", "ar-clearing", "cust", 10000, t0.Add(-1*24*time.Hour))
	if esc := l.EscalateDunning(t0); len(esc) != 0 {
		t.Fatalf("scan after payoff = %+v, want empty", esc)
	}
	st, ok := l.DunningStatusOf("cust", "USD")
	if !ok || st.Level != DunningLevelL2 {
		t.Fatalf("state = %+v, %v: want level stays L2 after payoff", st, ok)
	}
	if got := l.DunningEscalationCount(); got != 1 {
		t.Fatalf("counter = %d, want 1", got)
	}
	// A partial payment that retires the oldest lot drops the age: still
	// no de-escalation.
	mustPostDunning(t, l, "d3", "cust", "ar-clearing", 5000, t0.Add(-10*24*time.Hour))
	if esc := l.EscalateDunning(t0); len(esc) != 0 {
		t.Fatalf("scan after new young invoice = %+v, want empty", esc)
	}
	if st, _ := l.DunningStatusOf("cust", "USD"); st.Level != DunningLevelL2 {
		t.Fatalf("state level = %d, want still L2", st.Level)
	}
}

// TestResetDunning: the operator's explicit close of the file; resetting
// an unconfigured pair fails; the reset is audit-logged.
func TestResetDunning(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	t0 := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	mustPostDunning(t, l, "d1", "cust", "ar-clearing", 10000, t0.Add(-40*24*time.Hour))
	mustSetDunning(t, l, "cust", "USD", 30, 60, 90)
	l.EscalateDunning(t0)

	if err := l.ResetDunning("cust", "USD"); err != nil {
		t.Fatalf("ResetDunning: %v", err)
	}
	if _, ok := l.DunningStatusOf("cust", "USD"); ok {
		t.Fatal("state still present after ResetDunning")
	}
	if err := l.ResetDunning("nobody", "USD"); !errors.Is(err, ErrDunningNotConfigured) {
		t.Fatalf("ResetDunning(unconfigured) = %v, want ErrDunningNotConfigured", err)
	}
	// Re-escalating after a reset starts over at L1: the new money is new.
	mustPostDunning(t, l, "d2", "cust", "ar-clearing", 3000, t0.Add(-35*24*time.Hour))
	esc := l.EscalateDunning(t0)
	if len(esc) != 1 || esc[0].PreviousLevel != DunningLevelNone || esc[0].Level != DunningLevelL1 {
		t.Fatalf("post-reset scan = %+v, want 0->L1", esc)
	}

	now := time.Now()
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, corrupt, err := ReadAuditLog(al.Dir(), now.UTC().Format("2006-01-02"))
	if err != nil || corrupt != 0 {
		t.Fatalf("ReadAuditLog: %v, corrupt=%d", err, corrupt)
	}
	var escalated, reset *AuditEvent
	for i := range events {
		switch events[i].Op {
		case "dunning_escalated":
			c := events[i]
			escalated = &c
		case "dunning_reset":
			c := events[i]
			reset = &c
		}
	}
	if escalated == nil {
		t.Fatal("no dunning_escalated event in audit log")
	}
	d := escalated.Details
	for _, k := range []string{"account", "currency", "level", "overdue_cents"} {
		// account travels in Accounts, currency in Details.
		if k == "account" {
			continue
		}
		if _, ok := d[k]; !ok {
			t.Fatalf("dunning_escalated details missing %q: %v", k, d)
		}
	}
	if escalated.Accounts[0] != "cust" {
		t.Fatalf("escalated accounts = %v, want [cust]", escalated.Accounts)
	}
	if d["level"] != float64(DunningLevelL1) || d["level_label"] != "L1" {
		t.Fatalf("escalated details = %v, want level 1 / L1", d)
	}
	if reset == nil {
		t.Fatal("no dunning_reset event in audit log")
	}
	if reset.Details["cleared_level"] != float64(DunningLevelL1) {
		t.Fatalf("reset details = %v, want cleared_level 1", reset.Details)
	}
}

// TestEscalateDunningIgnoresUnconfigured: pairs without a policy never
// escalate, even when deeply overdue — dunning is strictly opt-in.
func TestEscalateDunningIgnoresUnconfigured(t *testing.T) {
	l := New()
	t0 := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	mustPostDunning(t, l, "d1", "cust", "ar-clearing", 10000, t0.Add(-200*24*time.Hour))
	if esc := l.EscalateDunning(t0); len(esc) != 0 {
		t.Fatalf("scan = %+v, want empty (no policy)", esc)
	}
	if got := l.DunningEscalationCount(); got != 0 {
		t.Fatalf("counter = %d, want 0", got)
	}
	if _, ok := l.DunningStatusOf("cust", "USD"); ok {
		t.Fatal("state created without a policy")
	}
}

// TestEscalateDunningPendingReviewExcluded: a transfer parked in
// dual-control review is booked but not effective — it must not age a
// receivable (exactly like the Aging view).
func TestEscalateDunningPendingReviewExcluded(t *testing.T) {
	l := New(WithReviewThreshold(1)) // every transfer routes to review
	mustSetDunning(t, l, "cust", "USD", 30, 60, 90)
	t0 := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	// A 100-day-old transfer that is still pending review: its cents are
	// booked but not effective, so they must not age the receivable.
	tr := Transfer{ID: "xfer-1", From: "ar-clearing", To: "cust", AmountCents: 10000,
		Currency: "USD", IdempotencyKey: "kx1", CreatedAt: t0.Add(-100 * 24 * time.Hour)}
	receipt, err := l.PostTransfer(tr)
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if receipt.ReviewStatus != ReviewStatusPending {
		t.Fatalf("receipt review status = %q, want pending_review", receipt.ReviewStatus)
	}
	if esc := l.EscalateDunning(t0); len(esc) != 0 {
		t.Fatalf("scan = %+v, want empty (pending review not effective)", esc)
	}
	if _, ok := l.DunningStatusOf("cust", "USD"); ok {
		t.Fatal("state created from pending-review cents")
	}
}

// TestDunningReconcileDisclosure: escalated positions are disclosed in
// the end-of-day report.
func TestDunningReconcileDisclosure(t *testing.T) {
	l := New()
	t0 := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	mustPostDunning(t, l, "d1", "cust-a", "ar-clearing", 10000, t0.Add(-70*24*time.Hour))
	mustPostDunning(t, l, "d2", "cust-b", "ar-clearing", 5000, t0.Add(-100*24*time.Hour))
	mustSetDunning(t, l, "cust-a", "USD", 30, 60, 90)
	mustSetDunning(t, l, "cust-b", "USD", 30, 60, 90)
	l.EscalateDunning(t0)

	report := l.Reconcile(t0)
	if len(report.DunningStatuses) != 2 {
		t.Fatalf("dunning_statuses = %+v, want 2 rows", report.DunningStatuses)
	}
	first, second := report.DunningStatuses[0], report.DunningStatuses[1]
	if first.Account != "cust-a" || first.Level != DunningLevelL2 || first.LevelLabel != "L2" {
		t.Fatalf("first row = %+v, want cust-a at L2", first)
	}
	if first.OverdueCents != 10000 || first.DaysPastDue != 70 {
		t.Fatalf("first row = %+v, want 70 days on 10000c", first)
	}
	if second.Account != "cust-b" || second.Level != DunningLevelL3 || second.LevelLabel != "L3" {
		t.Fatalf("second row = %+v, want cust-b at L3", second)
	}
}

// TestDunningSnapshotRoundTrip: policies and escalation states survive
// export/import; the restored ledger escalates from the restored state.
func TestDunningSnapshotRoundTrip(t *testing.T) {
	src := New()
	t0 := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	mustPostDunning(t, src, "d1", "cust", "ar-clearing", 10000, t0.Add(-70*24*time.Hour))
	mustSetDunning(t, src, "cust", "USD", 30, 60, 90)
	src.EscalateDunning(t0)

	var buf bytes.Buffer
	if err := src.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	cfg, ok := restored.DunningConfigOf("cust", "USD")
	if !ok || cfg.Level2Days != 60 {
		t.Fatalf("restored config = %+v, %v: want L2=60", cfg, ok)
	}
	st, ok := restored.DunningStatusOf("cust", "USD")
	if !ok || st.Level != DunningLevelL2 || st.OverdueCents != 10000 {
		t.Fatalf("restored state = %+v, %v: want L2 on 10000c", st, ok)
	}
	// The restored ledger escalates from the restored state, not from
	// scratch: 120 days -> L3, one event, previous level L2.
	esc := restored.EscalateDunning(t0.Add(50 * 24 * time.Hour))
	if len(esc) != 1 || esc[0].PreviousLevel != DunningLevelL2 || esc[0].Level != DunningLevelL3 {
		t.Fatalf("restored scan = %+v, want L2->L3", esc)
	}
}

// TestDunningClearConfig: clearing the policy removes policy and state;
// clearing an unconfigured pair is a no-op.
func TestDunningClearConfig(t *testing.T) {
	l := New()
	t0 := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	mustPostDunning(t, l, "d1", "cust", "ar-clearing", 10000, t0.Add(-70*24*time.Hour))
	mustSetDunning(t, l, "cust", "USD", 30, 60, 90)
	l.EscalateDunning(t0)
	if err := l.ClearDunningConfig("cust", "USD"); err != nil {
		t.Fatalf("ClearDunningConfig: %v", err)
	}
	if _, ok := l.DunningConfigOf("cust", "USD"); ok {
		t.Fatal("config still present after clear")
	}
	if _, ok := l.DunningStatusOf("cust", "USD"); ok {
		t.Fatal("state still present after clear")
	}
	if err := l.ClearDunningConfig("cust", "USD"); err != nil {
		t.Fatalf("ClearDunningConfig again: %v, want no-op", err)
	}
}
