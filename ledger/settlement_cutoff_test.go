package ledger

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// cutoffTestLedger builds a ledger with a USD 17:00 UTC cutoff and a
// funded payer.
func cutoffTestLedger(t *testing.T, hhmm, timezone string) *Ledger {
	t.Helper()
	l := New()
	if err := l.SetSettlementCutoff("USD", hhmm, timezone); err != nil {
		t.Fatalf("SetSettlementCutoff: %v", err)
	}
	seedBalance(t, l, "payer", 100_000_000)
	return l
}

func postCutoffTransfer(t *testing.T, l *Ledger, id string, at time.Time) TransferReceipt {
	t.Helper()
	receipt, err := l.PostTransfer(Transfer{
		ID:          id,
		From:        "payer",
		To:          "payee",
		AmountCents: 1000,
		Currency:    "USD",
		CreatedAt:   at,
	})
	if err != nil {
		t.Fatalf("PostTransfer %s: %v", id, err)
	}
	return receipt
}

func TestParseSettlementCutoff(t *testing.T) {
	c, err := ParseSettlementCutoff("USD", "17:00", "")
	if err != nil {
		t.Fatalf("ParseSettlementCutoff: %v", err)
	}
	if c.Currency != "USD" || c.Cutoff != "17:00" || c.Timezone != "UTC" {
		t.Fatalf("cutoff = %+v, want USD/17:00/UTC", c)
	}
	c, err = ParseSettlementCutoff("EUR", "09:30", "America/New_York")
	if err != nil {
		t.Fatalf("ParseSettlementCutoff tz: %v", err)
	}
	if c.Timezone != "America/New_York" {
		t.Fatalf("timezone = %q, want America/New_York", c.Timezone)
	}

	bad := []struct {
		name     string
		currency string
		hhmm     string
		tz       string
		wantErr  error
	}{
		{"hour out of range", "USD", "25:00", "", ErrInvalidSettlementCutoff},
		{"minute out of range", "USD", "17:60", "", ErrInvalidSettlementCutoff},
		{"single digit", "USD", "9:05", "", ErrInvalidSettlementCutoff},
		{"no colon", "USD", "1700", "", ErrInvalidSettlementCutoff},
		{"empty", "USD", "", "", ErrInvalidSettlementCutoff},
		{"trailing junk", "USD", "17:00:00", "", ErrInvalidSettlementCutoff},
		{"bad currency", "US", "17:00", "", ErrInvalidCurrency},
		{"bad timezone", "USD", "17:00", "Mars/Olympus", ErrInvalidSettlementTimezone},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseSettlementCutoff(tc.currency, tc.hhmm, tc.tz); !errors.Is(err, tc.wantErr) {
				t.Fatalf("ParseSettlementCutoff(%q,%q,%q) err = %v, want %v",
					tc.currency, tc.hhmm, tc.tz, err, tc.wantErr)
			}
		})
	}
}

func TestWithSettlementCutoffPanicsOnInvalid(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("WithSettlementCutoff with bad HH:MM did not panic")
		}
	}()
	_ = New(WithSettlementCutoff("USD", "99:99", ""))
}

func TestSettlementCutoffBeforeCutoffSettles(t *testing.T) {
	l := cutoffTestLedger(t, "17:00", "")
	// Friday 2026-10-16 16:59 UTC: before the cutoff, a business day.
	at := time.Date(2026, 10, 16, 16, 59, 0, 0, time.UTC)
	receipt := postCutoffTransfer(t, l, "t-before", at)
	if receipt.SettlementStatus != "settled" {
		t.Fatalf("SettlementStatus = %q, want settled", receipt.SettlementStatus)
	}
	wantWindow := time.Date(2026, 10, 16, 17, 0, 0, 0, time.UTC)
	if !receipt.SettlementWindow.Equal(wantWindow) {
		t.Fatalf("SettlementWindow = %v, want %v", receipt.SettlementWindow, wantWindow)
	}
	if got := len(l.DeferredSettlements()); got != 0 {
		t.Fatalf("deferrals = %d, want 0", got)
	}
	if got := l.SettlementDeferredCount(); got != 0 {
		t.Fatalf("SettlementDeferredCount = %d, want 0", got)
	}
	// The cutoff instant itself still belongs to the current window.
	receipt = postCutoffTransfer(t, l, "t-exact", time.Date(2026, 10, 16, 17, 0, 0, 0, time.UTC))
	if receipt.SettlementStatus != "settled" {
		t.Fatalf("exact-cutoff SettlementStatus = %q, want settled", receipt.SettlementStatus)
	}
}

func TestSettlementCutoffAfterCutoffDefers(t *testing.T) {
	l := cutoffTestLedger(t, "17:00", "")
	// Friday 2026-10-16 17:01 UTC: after the cutoff -> Monday's window
	// (weekend skipped via the LG-95 calendar).
	at := time.Date(2026, 10, 16, 17, 1, 0, 0, time.UTC)
	receipt := postCutoffTransfer(t, l, "t-after", at)
	if receipt.SettlementStatus != "scheduled" {
		t.Fatalf("SettlementStatus = %q, want scheduled", receipt.SettlementStatus)
	}
	wantWindow := time.Date(2026, 10, 19, 17, 0, 0, 0, time.UTC)
	if !receipt.SettlementWindow.Equal(wantWindow) {
		t.Fatalf("SettlementWindow = %v, want %v", receipt.SettlementWindow, wantWindow)
	}
	// The money still booked: the cutoff never blocks the journal.
	if got := l.BalanceIn("payee", "USD"); got != 1000 {
		t.Fatalf("payee balance = %d, want 1000 (booking must not be blocked)", got)
	}
	deferrals := l.DeferredSettlements()
	if len(deferrals) != 1 {
		t.Fatalf("deferrals = %d, want 1", len(deferrals))
	}
	d := deferrals[0]
	if d.TransferID != "t-after" || d.Currency != "USD" || d.Status != DeferralScheduled {
		t.Fatalf("deferral = %+v, want t-after/USD/scheduled", d)
	}
	if !d.Window.Equal(wantWindow) || d.Timezone != "UTC" {
		t.Fatalf("deferral window = %v (%s), want %v (UTC)", d.Window, d.Timezone, wantWindow)
	}
	if !d.SubmittedAt.Equal(at) {
		t.Fatalf("deferral submitted_at = %v, want %v", d.SubmittedAt, at)
	}
	if got := l.SettlementDeferredCount(); got != 1 {
		t.Fatalf("SettlementDeferredCount = %d, want 1", got)
	}
}

func TestNextSettlementWindow(t *testing.T) {
	l := cutoffTestLedger(t, "17:00", "")

	// Friday before cutoff: today's window, still open.
	w, err := l.NextSettlementWindow("USD", time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("NextSettlementWindow: %v", err)
	}
	if !w.Window.Equal(time.Date(2026, 10, 16, 17, 0, 0, 0, time.UTC)) || !w.IsToday {
		t.Fatalf("window = %+v, want today 17:00 UTC, is_today", w)
	}

	// Friday after cutoff: Monday's window (weekend skipped).
	w, err = l.NextSettlementWindow("USD", time.Date(2026, 10, 16, 18, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("NextSettlementWindow: %v", err)
	}
	if !w.Window.Equal(time.Date(2026, 10, 19, 17, 0, 0, 0, time.UTC)) || w.IsToday {
		t.Fatalf("window = %+v, want Monday 17:00 UTC, not today", w)
	}

	// Saturday morning: no window today even though the cutoff has not
	// passed — windows only land on business days.
	w, err = l.NextSettlementWindow("USD", time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("NextSettlementWindow: %v", err)
	}
	if !w.Window.Equal(time.Date(2026, 10, 19, 17, 0, 0, 0, time.UTC)) || w.IsToday {
		t.Fatalf("Saturday window = %+v, want Monday 17:00 UTC", w)
	}

	// A configured holiday (LG-95 table) pushes the window past it.
	if err := l.AddHoliday("2026-10-19"); err != nil {
		t.Fatalf("AddHoliday: %v", err)
	}
	w, err = l.NextSettlementWindow("USD", time.Date(2026, 10, 16, 18, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("NextSettlementWindow: %v", err)
	}
	if !w.Window.Equal(time.Date(2026, 10, 20, 17, 0, 0, 0, time.UTC)) {
		t.Fatalf("holiday window = %+v, want Tuesday 2026-10-20 17:00 UTC", w)
	}

	// Unknown currency code and unconfigured currency.
	if _, err := l.NextSettlementWindow("US", time.Now()); !errors.Is(err, ErrInvalidCurrency) {
		t.Fatalf("bad currency err = %v, want ErrInvalidCurrency", err)
	}
	if _, err := l.NextSettlementWindow("EUR", time.Now()); !errors.Is(err, ErrNoSettlementCutoff) {
		t.Fatalf("unconfigured err = %v, want ErrNoSettlementCutoff", err)
	}
}

func TestNextSettlementWindowTimezone(t *testing.T) {
	l := cutoffTestLedger(t, "17:00", "America/New_York")
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	// Friday 2026-10-16 20:00 UTC = 16:00 EDT: before the 17:00 ET
	// cutoff — today's window.
	w, err := l.NextSettlementWindow("USD", time.Date(2026, 10, 16, 20, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("NextSettlementWindow: %v", err)
	}
	want := time.Date(2026, 10, 16, 17, 0, 0, 0, ny)
	if !w.Window.Equal(want) || !w.IsToday || w.Timezone != "America/New_York" {
		t.Fatalf("window = %+v, want %v today in America/New_York", w, want)
	}
	// 21:30 UTC = 17:30 EDT: after the cutoff — Monday 17:00 ET
	// (weekend skipped).
	w, err = l.NextSettlementWindow("USD", time.Date(2026, 10, 16, 21, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("NextSettlementWindow: %v", err)
	}
	want = time.Date(2026, 10, 19, 17, 0, 0, 0, ny)
	if !w.Window.Equal(want) || w.IsToday {
		t.Fatalf("window = %+v, want %v (Monday), not today", w, want)
	}
}

func TestSettlementCutoffNoConfigZeroBehavior(t *testing.T) {
	l := New()
	seedBalance(t, l, "payer", 100_000_000)
	// EUR has no cutoff: the receipt discloses nothing, nothing defers.
	receipt, err := l.PostTransfer(Transfer{
		ID: "t-nocutoff", From: "payer", To: "payee",
		AmountCents: 500, Currency: "EUR",
		CreatedAt: time.Date(2026, 10, 16, 23, 59, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if receipt.SettlementStatus != "" || !receipt.SettlementWindow.IsZero() {
		t.Fatalf("receipt discloses %+v/%v, want empty (no cutoff configured)",
			receipt.SettlementStatus, receipt.SettlementWindow)
	}
	if got := len(l.DeferredSettlements()); got != 0 {
		t.Fatalf("deferrals = %d, want 0", got)
	}
}

func TestAdvanceAndSettleDeferral(t *testing.T) {
	l := cutoffTestLedger(t, "17:00", "")
	at := time.Date(2026, 10, 16, 17, 1, 0, 0, time.UTC)
	postCutoffTransfer(t, l, "t-adv", at)
	window := time.Date(2026, 10, 19, 17, 0, 0, 0, time.UTC)

	// Advancing before the window opens touches nothing (idempotent).
	if got := l.AdvanceSettlementWindows(window.Add(-time.Hour)); len(got) != 0 {
		t.Fatalf("early advance = %v, want empty", got)
	}
	// A scheduled deferral cannot jump straight to settled.
	if err := l.SettleDeferredTransfer("t-adv", "ops"); !errors.Is(err, ErrDeferralBadTransition) {
		t.Fatalf("settle scheduled err = %v, want ErrDeferralBadTransition", err)
	}
	if err := l.SettleDeferredTransfer("nope", "ops"); !errors.Is(err, ErrDeferralNotFound) {
		t.Fatalf("settle unknown err = %v, want ErrDeferralNotFound", err)
	}

	// The window opens: scheduled -> pending.
	advanced := l.AdvanceSettlementWindows(window)
	if len(advanced) != 1 || advanced[0] != "t-adv" {
		t.Fatalf("advanced = %v, want [t-adv]", advanced)
	}
	pending := l.DeferredSettlements(DeferralPending)
	if len(pending) != 1 || pending[0].TransferID != "t-adv" {
		t.Fatalf("pending = %+v, want [t-adv]", pending)
	}
	// Re-advancing is a no-op.
	if got := l.AdvanceSettlementWindows(window.Add(time.Hour)); len(got) != 0 {
		t.Fatalf("re-advance = %v, want empty", got)
	}

	// The operator confirms the run: pending -> settled.
	if err := l.SettleDeferredTransfer("t-adv", "ops-chen"); err != nil {
		t.Fatalf("SettleDeferredTransfer: %v", err)
	}
	settled := l.DeferredSettlements(DeferralSettled)
	if len(settled) != 1 || settled[0].TransferID != "t-adv" {
		t.Fatalf("settled = %+v, want [t-adv]", settled)
	}
	// Settling twice is a bad transition.
	if err := l.SettleDeferredTransfer("t-adv", "ops"); !errors.Is(err, ErrDeferralBadTransition) {
		t.Fatalf("double settle err = %v, want ErrDeferralBadTransition", err)
	}
}

func TestSettlementDeferralAuditEvent(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	if err := l.SetSettlementCutoff("USD", "17:00", ""); err != nil {
		t.Fatalf("SetSettlementCutoff: %v", err)
	}
	seedBalance(t, l, "payer", 100_000_000)
	postCutoffTransfer(t, l, "t-audit", time.Date(2026, 10, 16, 17, 1, 0, 0, time.UTC))
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	byOp := auditEventsByOp(t, dir)
	defers := byOp["transfer_settlement_deferred"]
	if len(defers) != 1 {
		t.Fatalf("defer events = %d, want 1", len(defers))
	}
	ev := defers[0]
	if ev.TraceID != "t-audit" || ev.Actor != "PostTransfer" {
		t.Fatalf("defer event = %+v, want trace t-audit actor PostTransfer", ev)
	}
	if ev.Details["reason"] != "settlement_cutoff" || ev.Details["status"] != "scheduled" {
		t.Fatalf("defer details = %v, want reason=settlement_cutoff status=scheduled", ev.Details)
	}
	if ev.Details["currency"] != "USD" || ev.Details["cutoff"] != "17:00" {
		t.Fatalf("defer details = %v, want currency=USD cutoff=17:00", ev.Details)
	}
	if !strings.HasPrefix(ev.Details["window"].(string), "2026-10-19T17:00:00") {
		t.Fatalf("defer window = %v, want Monday 17:00", ev.Details["window"])
	}
	// The config change itself is audited too.
	if len(byOp["settlement_cutoff_set"]) != 1 {
		t.Fatalf("cutoff_set events = %d, want 1", len(byOp["settlement_cutoff_set"]))
	}
}

func TestSetClearSettlementCutoff(t *testing.T) {
	l := New()
	if err := l.SetSettlementCutoff("USD", "17:00", ""); err != nil {
		t.Fatalf("SetSettlementCutoff: %v", err)
	}
	c, ok := l.SettlementCutoff("USD")
	if !ok || c.Cutoff != "17:00" || c.Timezone != "UTC" {
		t.Fatalf("SettlementCutoff = %+v,%v, want 17:00/UTC,true", c, ok)
	}
	if _, ok := l.SettlementCutoff("EUR"); ok {
		t.Fatalf("EUR cutoff reported configured")
	}
	if _, ok := l.SettlementCutoff("US"); ok {
		t.Fatalf("bad currency reported configured")
	}
	// Invalid input is rejected and installs nothing.
	if err := l.SetSettlementCutoff("EUR", "9:5", ""); !errors.Is(err, ErrInvalidSettlementCutoff) {
		t.Fatalf("bad HH:MM err = %v", err)
	}
	if _, ok := l.SettlementCutoff("EUR"); ok {
		t.Fatalf("failed Set installed a cutoff")
	}
	if err := l.ClearSettlementCutoff("USD"); err != nil {
		t.Fatalf("ClearSettlementCutoff: %v", err)
	}
	if _, ok := l.SettlementCutoff("USD"); ok {
		t.Fatalf("USD cutoff still configured after clear")
	}
	// Clearing an absent cutoff is a no-op success.
	if err := l.ClearSettlementCutoff("USD"); err != nil {
		t.Fatalf("ClearSettlementCutoff absent: %v", err)
	}
	if err := l.ClearSettlementCutoff("US"); !errors.Is(err, ErrInvalidCurrency) {
		t.Fatalf("ClearSettlementCutoff bad currency err = %v", err)
	}
	cutoffs := l.SettlementCutoffs()
	if len(cutoffs) != 0 {
		t.Fatalf("SettlementCutoffs = %v, want empty", cutoffs)
	}
}

func TestSettlementCutoffConcurrentDefer(t *testing.T) {
	l := cutoffTestLedger(t, "00:00", "")
	// Cutoff 00:00 UTC: every submission lands after the cutoff, so the
	// test is deterministic without waiting on the wall clock.
	const n = 16
	at := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = l.PostTransfer(Transfer{
				ID:             "t-race-" + strings.Repeat("0", 2) + string(rune('a'+i)),
				From:           "payer",
				To:             "payee",
				AmountCents:    100,
				Currency:       "USD",
				CreatedAt:      at,
				IdempotencyKey: "k-race-" + string(rune('a'+i)),
			})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	if got := l.SettlementDeferredCount(); got != n {
		t.Fatalf("SettlementDeferredCount = %d, want %d", got, n)
	}
	if got := len(l.DeferredSettlements(DeferralScheduled)); got != n {
		t.Fatalf("scheduled deferrals = %d, want %d", got, n)
	}
}

func TestReconcileDisclosesCutoffsAndDeferrals(t *testing.T) {
	l := cutoffTestLedger(t, "17:00", "")
	postCutoffTransfer(t, l, "t-recon", time.Date(2026, 10, 16, 17, 1, 0, 0, time.UTC))
	report := l.Reconcile(time.Date(2026, 10, 16, 18, 0, 0, 0, time.UTC))
	if len(report.SettlementCutoffs) != 1 || report.SettlementCutoffs[0].Currency != "USD" {
		t.Fatalf("report cutoffs = %+v, want [USD]", report.SettlementCutoffs)
	}
	if len(report.SettlementDeferrals) != 1 || report.SettlementDeferrals[0].TransferID != "t-recon" {
		t.Fatalf("report deferrals = %+v, want [t-recon]", report.SettlementDeferrals)
	}
}

func TestSettlementCutoffSnapshotRoundTrip(t *testing.T) {
	l := cutoffTestLedger(t, "17:00", "America/New_York")
	postCutoffTransfer(t, l, "t-snap", time.Date(2026, 10, 16, 21, 30, 0, 0, time.UTC)) // 17:30 ET

	snap, err := func() ([]byte, error) {
		var buf bytes.Buffer
		if err := l.ExportSnapshot(&buf); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}()
	if err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	r, err := ImportSnapshot(bytes.NewReader(snap))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	c, ok := r.SettlementCutoff("USD")
	if !ok || c.Cutoff != "17:00" || c.Timezone != "America/New_York" {
		t.Fatalf("restored cutoff = %+v,%v, want 17:00/America/New_York", c, ok)
	}
	deferrals := r.DeferredSettlements()
	if len(deferrals) != 1 || deferrals[0].TransferID != "t-snap" || deferrals[0].Status != DeferralScheduled {
		t.Fatalf("restored deferrals = %+v, want [t-snap/scheduled]", deferrals)
	}
	ny, _ := time.LoadLocation("America/New_York")
	wantWindow := time.Date(2026, 10, 19, 17, 0, 0, 0, ny)
	if !deferrals[0].Window.Equal(wantWindow) {
		t.Fatalf("restored window = %v, want %v", deferrals[0].Window, wantWindow)
	}
	// The restored ledger answers the window query with the restored
	// cutoff.
	w, err := r.NextSettlementWindow("USD", time.Date(2026, 10, 16, 20, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("NextSettlementWindow after restore: %v", err)
	}
	if !w.Window.Equal(time.Date(2026, 10, 16, 17, 0, 0, 0, ny)) || !w.IsToday {
		t.Fatalf("restored window query = %+v, want today 17:00 ET", w)
	}
}
