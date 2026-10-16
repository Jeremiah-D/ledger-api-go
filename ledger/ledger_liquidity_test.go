package ledger

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func liquidityPos(t *testing.T, r IntradayLiquidityReport, a AccountID, currency string) LiquidityPosition {
	t.Helper()
	for _, p := range r.Positions {
		if p.Account == a && p.Currency == currency {
			return p
		}
	}
	t.Fatalf("no position for %s/%s in %+v", a, currency, r.Positions)
	return LiquidityPosition{}
}

func mustLiquidityPost(t *testing.T, l *Ledger, e JournalEntry) {
	t.Helper()
	if _, dup, err := l.Post(e); err != nil || dup {
		t.Fatalf("Post %s = dup=%v err=%v", e.ID, dup, err)
	}
}

// liquidityAlerts collects low_liquidity_alert audit events from the log,
// failing the test on read errors.
func liquidityAlerts(t *testing.T, al *AuditLog) []AuditEvent {
	t.Helper()
	now := time.Now().UTC().Format("2006-01-02")
	events, corrupt, err := ReadAuditLog(al.Dir(), now)
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt = %d, want 0", corrupt)
	}
	var out []AuditEvent
	for _, ev := range events {
		if ev.Op == "low_liquidity_alert" {
			out = append(out, ev)
		}
	}
	return out
}

func TestIntradayLiquidityBookedFlows(t *testing.T) {
	l := New()
	now := time.Now().UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC)
	mustLiquidityPost(t, l, JournalEntry{ID: "e1", DebitAccount: "cust", CreditAccount: "bank", AmountCents: 10000, Currency: "USD", CreatedAt: day})
	mustLiquidityPost(t, l, JournalEntry{ID: "e2", DebitAccount: "bank", CreditAccount: "cust", AmountCents: 3000, Currency: "USD", CreatedAt: day})
	// Yesterday's entry must not leak into the day.
	mustLiquidityPost(t, l, JournalEntry{ID: "e3", DebitAccount: "cust", CreditAccount: "bank", AmountCents: 50000, Currency: "USD", CreatedAt: day.Add(-24 * time.Hour)})
	// A different currency is a separate position.
	mustLiquidityPost(t, l, JournalEntry{ID: "e4", DebitAccount: "cust", CreditAccount: "bank", AmountCents: 200, Currency: "EUR", CreatedAt: day})

	r := l.IntradayLiquidity(day)
	if r.Date != utcDay(day) {
		t.Errorf("date = %q, want %q", r.Date, utcDay(day))
	}
	cust := liquidityPos(t, r, "cust", "USD")
	if cust.BookedInflowCents != 10000 {
		t.Errorf("booked inflow = %d, want 10000", cust.BookedInflowCents)
	}
	if cust.BookedOutflowCents != 3000 {
		t.Errorf("booked outflow = %d, want 3000", cust.BookedOutflowCents)
	}
	if cust.NetPositionCents != 7000 {
		t.Errorf("net = %d, want 7000", cust.NetPositionCents)
	}
	eur := liquidityPos(t, r, "cust", "EUR")
	if eur.BookedInflowCents != 200 || eur.NetPositionCents != 200 {
		t.Errorf("EUR position = %+v, want inflow 200 net 200", eur)
	}
}

func TestIntradayLiquidityHolds(t *testing.T) {
	l := New()
	now := time.Now().UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC)
	mustLiquidityPost(t, l, JournalEntry{ID: "seed", DebitAccount: "cust", CreditAccount: "bank", AmountCents: 100000, Currency: "USD", CreatedAt: day})

	// An active hold outstanding past the end of the day reserves outflow.
	if _, _, err := l.Hold(Hold{ID: "h1", Account: "cust", AmountCents: 20000, Currency: "USD", ExpiresAt: day.Add(48 * time.Hour), CreatedAt: day}); err != nil {
		t.Fatalf("Hold h1: %v", err)
	}
	// A hold expiring before the end of the day releases its funds back
	// and must not count as reserved.
	if _, _, err := l.Hold(Hold{ID: "h2", Account: "cust", AmountCents: 15000, Currency: "USD", ExpiresAt: day.Add(30 * time.Hour), CreatedAt: day}); err != nil {
		t.Fatalf("Hold h2: %v", err)
	}
	if n := l.ExpireHoldsAt(day.Add(30 * time.Hour)); n != 1 {
		t.Fatalf("ExpireHoldsAt = %d, want 1", n)
	}

	cust := liquidityPos(t, l.IntradayLiquidity(day), "cust", "USD")
	if cust.HoldReservedCents != 20000 {
		t.Errorf("hold reserved = %d, want 20000", cust.HoldReservedCents)
	}
	if cust.NetPositionCents != 100000-20000 {
		t.Errorf("net = %d, want 80000", cust.NetPositionCents)
	}
}

func TestIntradayLiquiditySchedules(t *testing.T) {
	l := New()
	now := time.Now().UTC()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	mustLiquidityPost(t, l, JournalEntry{ID: "seed", DebitAccount: "cust", CreditAccount: "bank", AmountCents: 1000000, Currency: "USD"})

	// One run firing today.
	if _, _, err := l.CreateTransferSchedule(TransferSchedule{
		ID: "s1", From: "cust", To: "bank", AmountCents: 5000, Currency: "USD",
		Interval: 24 * time.Hour, NextRunAt: dayStart.Add(10 * time.Hour),
	}); err != nil {
		t.Fatalf("CreateTransferSchedule s1: %v", err)
	}
	// Paused schedules do not fire.
	if _, _, err := l.CreateTransferSchedule(TransferSchedule{
		ID: "s2", From: "cust", To: "bank", AmountCents: 9000, Currency: "USD",
		Interval: 24 * time.Hour, NextRunAt: dayStart.Add(11 * time.Hour),
	}); err != nil {
		t.Fatalf("CreateTransferSchedule s2: %v", err)
	}
	if _, err := l.PauseTransferSchedule("s2"); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	// An overdue schedule contributes its one catch-up run.
	if _, _, err := l.CreateTransferSchedule(TransferSchedule{
		ID: "s3", From: "cust", To: "bank", AmountCents: 7000, Currency: "USD",
		Interval: 24 * time.Hour, NextRunAt: dayStart.Add(-48 * time.Hour),
	}); err != nil {
		t.Fatalf("CreateTransferSchedule s3: %v", err)
	}
	// A schedule whose next run is tomorrow does not count.
	if _, _, err := l.CreateTransferSchedule(TransferSchedule{
		ID: "s4", From: "cust", To: "bank", AmountCents: 11000, Currency: "USD",
		Interval: 24 * time.Hour, NextRunAt: dayStart.Add(25 * time.Hour),
	}); err != nil {
		t.Fatalf("CreateTransferSchedule s4: %v", err)
	}

	r := l.IntradayLiquidity(dayStart.Add(12 * time.Hour))
	cust := liquidityPos(t, r, "cust", "USD")
	if cust.ScheduledOutflowCents != 5000+7000 {
		t.Errorf("scheduled outflow = %d, want 12000", cust.ScheduledOutflowCents)
	}
	bank := liquidityPos(t, r, "bank", "USD")
	if bank.ScheduledInflowCents != 5000+7000 {
		t.Errorf("scheduled inflow = %d, want 12000", bank.ScheduledInflowCents)
	}
	if cust.NetPositionCents != 1000000-(5000+7000) {
		t.Errorf("net = %d, want 988000", cust.NetPositionCents)
	}
}

func TestIntradayLiquidityTermDeposits(t *testing.T) {
	l := New()
	now := time.Now().UTC()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	mustLiquidityPost(t, l, JournalEntry{ID: "seed", DebitAccount: "cust", CreditAccount: "bank", AmountCents: 1000000, Currency: "USD"})

	// Matures today: opened yesterday, 1-day tenor.
	if _, _, err := l.OpenTermDeposit(TermDeposit{
		ID: "td1", Account: "cust", AmountCents: 100000, Currency: "USD",
		RateBps: 500, TermDays: 1, OpenedAt: dayStart.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("OpenTermDeposit td1: %v", err)
	}
	// Matures tomorrow: excluded.
	if _, _, err := l.OpenTermDeposit(TermDeposit{
		ID: "td2", Account: "cust", AmountCents: 200000, Currency: "USD",
		RateBps: 500, TermDays: 2, OpenedAt: now,
	}); err != nil {
		t.Fatalf("OpenTermDeposit td2: %v", err)
	}

	cust := liquidityPos(t, l.IntradayLiquidity(dayStart.Add(12*time.Hour)), "cust", "USD")
	if cust.TermDepositInflowCents < 100000 {
		t.Errorf("term-deposit inflow = %d, want >= 100000 (principal plus accrued interest)", cust.TermDepositInflowCents)
	}
}

func TestLiquidityAlertThresholdFires(t *testing.T) {
	al := mustAuditLog(t)
	defer al.Close()
	l := New(WithAuditLog(al))
	now := time.Now().UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC)
	mustLiquidityPost(t, l, JournalEntry{ID: "seed", DebitAccount: "cust", CreditAccount: "bank", AmountCents: 10000, Currency: "USD", CreatedAt: day})

	if err := l.SetLiquidityAlertThreshold("", "USD", 50000); !errors.Is(err, ErrEmptyLiquidityAccount) {
		t.Fatalf("empty account = %v, want ErrEmptyLiquidityAccount", err)
	}
	if err := l.SetLiquidityAlertThreshold("cust", "US", 50000); !errors.Is(err, ErrInvalidCurrency) {
		t.Fatalf("bad currency = %v, want ErrInvalidCurrency", err)
	}
	if err := l.SetLiquidityAlertThreshold("cust", "USD", 50000); err != nil {
		t.Fatalf("SetLiquidityAlertThreshold: %v", err)
	}
	if got, ok := l.GetLiquidityAlertThreshold("cust", "USD"); !ok || got != 50000 {
		t.Fatalf("GetLiquidityAlertThreshold = %d, %v; want 50000, true", got, ok)
	}

	// Net 10000 < 50000: fires once.
	alerts := l.EvaluateLiquidityAlerts(day)
	if len(alerts) != 1 {
		t.Fatalf("first evaluation fired %d alerts, want 1", len(alerts))
	}
	a := alerts[0]
	if a.Account != "cust" || a.Currency != "USD" || a.ThresholdCents != 50000 || a.NetPositionCents != 10000 {
		t.Errorf("alert = %+v, want cust/USD threshold 50000 net 10000", a)
	}
	// Still below: edge-triggered, no second event.
	if alerts := l.EvaluateLiquidityAlerts(day); len(alerts) != 0 {
		t.Errorf("second evaluation fired %d alerts, want 0 (edge-triggered)", len(alerts))
	}
	// Recover above the floor: re-arms silently.
	mustLiquidityPost(t, l, JournalEntry{ID: "topup", DebitAccount: "cust", CreditAccount: "bank", AmountCents: 100000, Currency: "USD", CreatedAt: day})
	if alerts := l.EvaluateLiquidityAlerts(day); len(alerts) != 0 {
		t.Errorf("recovery evaluation fired %d alerts, want 0", len(alerts))
	}
	// Fall below again: fires again.
	mustLiquidityPost(t, l, JournalEntry{ID: "drain", DebitAccount: "bank", CreditAccount: "cust", AmountCents: 70000, Currency: "USD", CreatedAt: day})
	if alerts := l.EvaluateLiquidityAlerts(day); len(alerts) != 1 {
		t.Errorf("re-breach evaluation fired %d alerts, want 1", len(alerts))
	}
	if got := l.LiquidityAlertCount(); got != 2 {
		t.Errorf("LiquidityAlertCount = %d, want 2", got)
	}
	// The audit log carries the structured events (flushed on Close).
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	evs := liquidityAlerts(t, al)
	if len(evs) != 2 {
		t.Fatalf("low_liquidity_alert events = %d, want 2", len(evs))
	}
	if evs[0].Details["threshold_cents"] != float64(50000) {
		t.Errorf("event threshold = %v, want 50000", evs[0].Details["threshold_cents"])
	}
	if evs[0].Details["net_position_cents"] != float64(10000) {
		t.Errorf("event net = %v, want 10000", evs[0].Details["net_position_cents"])
	}

	// Clear: no more evaluation.
	l.ClearLiquidityAlertThreshold("cust", "USD")
	if _, ok := l.GetLiquidityAlertThreshold("cust", "USD"); ok {
		t.Error("threshold still present after ClearLiquidityAlertThreshold")
	}
	mustLiquidityPost(t, l, JournalEntry{ID: "drain2", DebitAccount: "bank", CreditAccount: "cust", AmountCents: 10000, Currency: "USD", CreatedAt: day})
	if alerts := l.EvaluateLiquidityAlerts(day); len(alerts) != 0 {
		t.Errorf("evaluation after clear fired %d alerts, want 0", len(alerts))
	}
}

func TestReconcileDisclosesIntradayLiquidity(t *testing.T) {
	l := New()
	now := time.Now().UTC()
	mustLiquidityPost(t, l, JournalEntry{ID: "e1", DebitAccount: "cust", CreditAccount: "bank", AmountCents: 10000, Currency: "USD"})
	if err := l.SetLiquidityAlertThreshold("cust", "USD", 50000); err != nil {
		t.Fatalf("SetLiquidityAlertThreshold: %v", err)
	}

	rep := l.Reconcile(now)
	found := false
	for _, p := range rep.IntradayLiquidity {
		if p.Account == "cust" && p.Currency == "USD" {
			found = true
			if p.BookedInflowCents != 10000 {
				t.Errorf("reconcile booked inflow = %d, want 10000", p.BookedInflowCents)
			}
			if p.NetPositionCents != 10000 {
				t.Errorf("reconcile net = %d, want 10000", p.NetPositionCents)
			}
			if p.ThresholdCents == nil || *p.ThresholdCents != 50000 {
				t.Errorf("reconcile threshold = %v, want 50000", p.ThresholdCents)
			}
			if !p.BelowThreshold {
				t.Error("reconcile BelowThreshold = false, want true (10000 < 50000)")
			}
		}
	}
	if !found {
		t.Fatalf("reconcile report has no intraday liquidity row for cust/USD: %+v", rep.IntradayLiquidity)
	}
}

func TestLiquidityThresholdSnapshotRoundTrip(t *testing.T) {
	l := New()
	if err := l.SetLiquidityAlertThreshold("cust", "USD", 1000); err != nil {
		t.Fatalf("SetLiquidityAlertThreshold: %v", err)
	}
	// cust has no flows today: net 0 < 1000.

	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	l2, err := ImportSnapshot(&buf)
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	got, ok := l2.GetLiquidityAlertThreshold("cust", "USD")
	if !ok || got != 1000 {
		t.Fatalf("restored threshold = %d, %v; want 1000, true", got, ok)
	}
	// The breach state re-derives from the restored ledger: the first
	// evaluation after the restore must not backfire a stale alert.
	if alerts := l2.EvaluateLiquidityAlerts(time.Now()); len(alerts) != 0 {
		t.Errorf("first evaluation after restore fired %d alerts, want 0 (no stale backfire)", len(alerts))
	}
	if got := l2.LiquidityAlertCount(); got != 0 {
		t.Errorf("LiquidityAlertCount after restore = %d, want 0", got)
	}
	// The threshold is still live: recovery then re-breach fires.
	mustLiquidityPost(t, l2, JournalEntry{ID: "topup", DebitAccount: "cust", CreditAccount: "bank", AmountCents: 5000, Currency: "USD"})
	if alerts := l2.EvaluateLiquidityAlerts(time.Now()); len(alerts) != 0 {
		t.Errorf("recovery evaluation fired %d alerts, want 0", len(alerts))
	}
	mustLiquidityPost(t, l2, JournalEntry{ID: "drain", DebitAccount: "bank", CreditAccount: "cust", AmountCents: 5000, Currency: "USD"})
	if alerts := l2.EvaluateLiquidityAlerts(time.Now()); len(alerts) != 1 {
		t.Errorf("re-breach evaluation fired %d alerts, want 1", len(alerts))
	}
}

func TestScheduleFiringsOnDay(t *testing.T) {
	dayStart := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	dayEnd := dayStart.Add(24 * time.Hour)

	// Two runs land on the day.
	s := &TransferSchedule{Interval: 12 * time.Hour, NextRunAt: dayStart.Add(6 * time.Hour)}
	if got := scheduleFiringsOnDay(s, dayStart, dayEnd); len(got) != 2 {
		t.Errorf("two-a-day firings = %d, want 2", len(got))
	}
	// Next run tomorrow: nothing.
	s = &TransferSchedule{Interval: 24 * time.Hour, NextRunAt: dayStart.Add(25 * time.Hour)}
	if got := scheduleFiringsOnDay(s, dayStart, dayEnd); len(got) != 0 {
		t.Errorf("tomorrow firings = %d, want 0", len(got))
	}
	// Overdue: exactly one catch-up run.
	s = &TransferSchedule{Interval: 24 * time.Hour, NextRunAt: dayStart.Add(-72 * time.Hour)}
	if got := scheduleFiringsOnDay(s, dayStart, dayEnd); len(got) != 1 {
		t.Errorf("overdue firings = %d, want 1", len(got))
	}
	// Expired before the run is due (NextRunAt past EndsAt): nothing fires.
	s = &TransferSchedule{Interval: 24 * time.Hour, NextRunAt: dayStart.Add(-48 * time.Hour), EndsAt: dayStart.Add(-72 * time.Hour)}
	if got := scheduleFiringsOnDay(s, dayStart, dayEnd); len(got) != 0 {
		t.Errorf("expired firings = %d, want 0", len(got))
	}
	// Zero interval: never (defensive).
	s = &TransferSchedule{Interval: 0, NextRunAt: dayStart}
	if got := scheduleFiringsOnDay(s, dayStart, dayEnd); len(got) != 0 {
		t.Errorf("zero-interval firings = %d, want 0", len(got))
	}
}
