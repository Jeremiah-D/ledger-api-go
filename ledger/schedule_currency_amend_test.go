package ledger

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// createCurrencySchedule registers an active schedule in the given
// base currency, optionally with an FX settlement target, and returns
// the recorded schedule.
func createCurrencySchedule(t *testing.T, l *Ledger, id string, now time.Time, currency, toCurrency string) TransferSchedule {
	t.Helper()
	s := testSchedule(now)
	s.ID = id
	s.Currency = currency
	s.ToCurrency = toCurrency
	created, _, err := l.CreateTransferSchedule(s)
	if err != nil {
		t.Fatalf("CreateTransferSchedule: %v", err)
	}
	return created
}

func TestAmendTransferScheduleCurrencyValidation(t *testing.T) {
	l := New()
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := l.AmendTransferScheduleCurrency("sched-unknown", "EUR"); !errors.Is(err, ErrScheduleNotFound) {
		t.Errorf("unknown schedule: err = %v, want ErrScheduleNotFound", err)
	}
	orig := createCurrencySchedule(t, l, "sched-1", now, "USD", "EUR")

	// Cancelled.
	l2 := New()
	createCurrencySchedule(t, l2, "sched-1", now, "USD", "EUR")
	if _, err := l2.CancelTransferSchedule("sched-1"); err != nil {
		t.Fatalf("CancelTransferSchedule: %v", err)
	}
	if _, err := l2.AmendTransferScheduleCurrency("sched-1", "GBP"); !errors.Is(err, ErrScheduleBadTransition) {
		t.Errorf("cancelled schedule: err = %v, want ErrScheduleBadTransition", err)
	}

	// Completed: one firing advances NextRunAt past EndsAt.
	l3 := New()
	fundAccount(t, l3, "alice", 100000, "USD")
	s := testSchedule(now)
	s.ID = "sched-1"
	s.Interval = time.Hour
	s.NextRunAt = now.Add(-time.Minute)
	s.EndsAt = s.NextRunAt.Add(30 * time.Minute)
	if _, _, err := l3.CreateTransferSchedule(s); err != nil {
		t.Fatalf("create: %v", err)
	}
	if runs := l3.SweepDue(now); len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("sweep runs = %+v, want one fired run", runs)
	}
	if got, _ := l3.GetTransferSchedule("sched-1"); got.Status != ScheduleCompleted {
		t.Fatalf("status after firing past ends_at = %q, want completed", got.Status)
	}
	if _, err := l3.AmendTransferScheduleCurrency("sched-1", "EUR"); !errors.Is(err, ErrScheduleBadTransition) {
		t.Errorf("completed schedule: err = %v, want ErrScheduleBadTransition", err)
	}

	// Amending to the current currency is a no-op, not a change —
	// including the empty code, which normalizes to the default USD.
	if got, err := l.AmendTransferScheduleCurrency("sched-1", "USD"); err != nil || !reflect.DeepEqual(got, orig) {
		t.Errorf("same-currency amendment = %+v, %v, want %+v, nil", got, err, orig)
	}
	if got, err := l.AmendTransferScheduleCurrency("sched-1", ""); err != nil || !reflect.DeepEqual(got, orig) {
		t.Errorf("empty-currency amendment = %+v, %v, want %+v, nil (empty normalizes to USD)", got, err, orig)
	}

	// Every rejection leaves the original currency exactly as it was.
	rejections := []struct {
		name     string
		currency string
		want     error
	}{
		{"malformed currency", "US", ErrInvalidCurrency},
		{"lowercase currency", "eur", ErrInvalidCurrency},
		{"four-letter currency", "USDD", ErrInvalidCurrency},
		{"new currency equals live settlement target", "EUR", ErrCrossCurrencyTransfer},
	}
	for _, tc := range rejections {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := l.AmendTransferScheduleCurrency("sched-1", tc.currency); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
			got, err := l.GetTransferSchedule("sched-1")
			if err != nil {
				t.Fatalf("GetTransferSchedule: %v", err)
			}
			if !reflect.DeepEqual(got, orig) {
				t.Errorf("schedule after rejection = %+v, want %+v (untouched)", got, orig)
			}
		})
	}

	// Active plan with no FX pair: only the currency changes — legs,
	// amount, pair, fees, memo, interval, next run time, end time,
	// run sequence, and run history are untouched.
	l5 := New()
	orig5 := createCurrencySchedule(t, l5, "sched-1", now, "USD", "")
	amended, err := l5.AmendTransferScheduleCurrency("sched-1", "GBP")
	if err != nil {
		t.Fatalf("AmendTransferScheduleCurrency active: %v", err)
	}
	if amended.Currency != "GBP" {
		t.Errorf("amended currency = %q, want GBP", amended.Currency)
	}
	amended.Currency = orig5.Currency
	if !reflect.DeepEqual(amended, orig5) {
		t.Errorf("amended schedule = %+v, want %+v except currency", amended, orig5)
	}

	// An empty code normalizes to the default currency: a EUR plan
	// amended with "" moves to USD, and the move is a real revision
	// (not a no-op) with the rest of the row untouched.
	l6 := New()
	orig6 := createCurrencySchedule(t, l6, "sched-1", now, "EUR", "")
	cleared, err := l6.AmendTransferScheduleCurrency("sched-1", "")
	if err != nil {
		t.Fatalf("AmendTransferScheduleCurrency empty: %v", err)
	}
	if cleared.Currency != "USD" {
		t.Errorf("empty-code currency = %q, want USD (default)", cleared.Currency)
	}
	cleared.Currency = orig6.Currency
	if !reflect.DeepEqual(cleared, orig6) {
		t.Errorf("empty-code schedule = %+v, want %+v except currency", cleared, orig6)
	}

	// Paused plan: pausing is a hold, not a terminal state, so the
	// revision waits with the plan — and again only the currency moves.
	l7 := New()
	orig7 := createCurrencySchedule(t, l7, "sched-1", now, "USD", "EUR")
	if _, err := l7.PauseTransferSchedule("sched-1"); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	paused, err := l7.AmendTransferScheduleCurrency("sched-1", "GBP")
	if err != nil {
		t.Fatalf("AmendTransferScheduleCurrency paused: %v", err)
	}
	if paused.Currency != "GBP" || paused.Status != SchedulePaused {
		t.Errorf("amended paused = %q status %q, want GBP paused", paused.Currency, paused.Status)
	}
	paused.Currency = orig7.Currency
	paused.Status = orig7.Status
	if !reflect.DeepEqual(paused, orig7) {
		t.Errorf("amended paused schedule = %+v, want %+v except currency", paused, orig7)
	}
}

func TestAmendTransferScheduleCurrencyNextRelease(t *testing.T) {
	// The new currency governs the next release with no firing-path
	// change: the first run books USD to the payee, the amendment
	// itself posts nothing, the second run books EUR to the payee
	// and the first run's USD leg is never rewritten.
	l := New()
	now := time.Now().UTC().Truncate(time.Second)
	fundAccount(t, l, "alice", 100000, "USD")
	fundAccount(t, l, "alice", 100000, "EUR")
	if _, _, err := l.CreateTransferSchedule(TransferSchedule{
		ID:          "sched-1",
		From:        "alice",
		To:          "bob",
		AmountCents: 1000,
		Currency:    "USD",
		Interval:    time.Hour,
		NextRunAt:   now,
		EndsAt:      now.Add(30 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("CreateTransferSchedule: %v", err)
	}
	if runs := l.SweepDue(now); len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("first sweep runs = %+v, want one fired run", runs)
	}
	if got := l.BalanceIn("bob", "USD"); got != 1000 {
		t.Fatalf("bob USD after first run = %d, want 1000", got)
	}
	if _, err := l.AmendTransferScheduleCurrency("sched-1", "EUR"); err != nil {
		t.Fatalf("AmendTransferScheduleCurrency: %v", err)
	}
	if got := l.BalanceIn("bob", "USD"); got != 1000 {
		t.Fatalf("bob USD after amendment = %d, want 1000 (amendment posts nothing)", got)
	}
	if got := l.BalanceIn("bob", "EUR"); got != 0 {
		t.Fatalf("bob EUR after amendment = %d, want 0", got)
	}
	live, err := l.GetTransferSchedule("sched-1")
	if err != nil {
		t.Fatalf("GetTransferSchedule: %v", err)
	}
	if runs := l.SweepDue(live.NextRunAt); len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("second sweep runs = %+v, want one fired run", runs)
	}
	if got := l.BalanceIn("bob", "USD"); got != 1000 {
		t.Errorf("bob USD after second run = %d, want 1000 (first run untouched)", got)
	}
	if got := l.BalanceIn("bob", "EUR"); got != 1000 {
		t.Errorf("bob EUR after second run = %d, want 1000 (second run booked in EUR)", got)
	}
	if got := l.BalanceIn("alice", "USD"); got != 100000-1000 {
		t.Errorf("alice USD after two runs = %d, want %d", got, 100000-1000)
	}
	if got := l.BalanceIn("alice", "EUR"); got != 100000-1000 {
		t.Errorf("alice EUR after two runs = %d, want %d", got, 100000-1000)
	}
	if runs, err := l.ListScheduleRuns("sched-1", 10); err != nil || len(runs) != 2 {
		t.Errorf("run history = %d runs, %v, want 2", len(runs), err)
	}
}

func TestAmendTransferScheduleCurrencyFailedRunRetryReadsNewCurrency(t *testing.T) {
	// A run that failed for a missing USD->EUR rate posts nothing,
	// so its retry under the same "<ID>/run/<seq>" key reads the
	// amended currency: with the base moved to GBP (rate set, and
	// an empty settlement account falling back to the ledger-level
	// FX account) the retry books exactly once, in EUR, funded from
	// the payer's GBP balance.
	l := New(WithFXAccount("fxdesk"))
	now := time.Now().UTC().Truncate(time.Second)
	fundAccount(t, l, "alice", 100000, "USD")
	fundAccount(t, l, "alice", 100000, "GBP")
	fundAccount(t, l, "fxdesk", 100000, "USD")
	fundAccount(t, l, "fxdesk", 100000, "GBP")
	s := testSchedule(now)
	s.ID = "sched-1"
	s.ToCurrency = "EUR"
	s.FXAccount = ""
	if _, _, err := l.CreateTransferSchedule(s); err != nil {
		t.Fatalf("CreateTransferSchedule: %v", err)
	}
	if runs := l.SweepDue(now); len(runs) != 1 || runs[0].Status != "failed" {
		t.Fatalf("first sweep runs = %+v, want one failed run (no USD->EUR rate)", runs)
	}
	if got := l.BalanceIn("bob", "EUR"); got != 0 {
		t.Fatalf("bob EUR after failed run = %d, want 0 (failed run posted nothing)", got)
	}
	if _, err := l.AmendTransferScheduleCurrency("sched-1", "GBP"); err != nil {
		t.Fatalf("AmendTransferScheduleCurrency: %v", err)
	}
	if err := l.SetFXRate("GBP", "EUR", 1, 1); err != nil {
		t.Fatalf("SetFXRate: %v", err)
	}
	live, err := l.GetTransferSchedule("sched-1")
	if err != nil {
		t.Fatalf("GetTransferSchedule: %v", err)
	}
	if live.RunSeq != 0 {
		t.Fatalf("run seq after failed run = %d, want 0 (failed run advances nothing)", live.RunSeq)
	}
	if runs := l.SweepDue(now); len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("retry sweep runs = %+v, want one fired run", runs)
	}
	if got := l.BalanceIn("bob", "EUR"); got != 1000 {
		t.Errorf("bob EUR after retry = %d, want 1000 (retry booked at the amended currency)", got)
	}
	if got := l.BalanceIn("bob", "USD"); got != 0 {
		t.Errorf("bob USD after retry = %d, want 0 (failed run posted nothing in USD either)", got)
	}
	if got := l.BalanceIn("alice", "GBP"); got != 100000-1000 {
		t.Errorf("alice GBP after retry = %d, want %d (retry debited the amended currency)", got, 100000-1000)
	}
	// ListScheduleRuns returns newest first: the fired retry, then
	// the failed attempt, both under the same run sequence.
	if runs, err := l.ListScheduleRuns("sched-1", 10); err != nil || len(runs) != 2 ||
		runs[0].Status != "fired" || runs[1].Status != "failed" || runs[0].RunSeq != runs[1].RunSeq {
		t.Errorf("run history = %+v, %v, want fired+failed under one seq", runs, err)
	}
}

func TestAmendTransferScheduleCurrencyAuditAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	now := time.Now().UTC().Truncate(time.Second)
	before := createCurrencySchedule(t, l, "sched-1", now, "USD", "EUR")
	// The no-op revision emits nothing.
	if _, err := l.AmendTransferScheduleCurrency("sched-1", "USD"); err != nil {
		t.Fatalf("no-op amend: %v", err)
	}
	ctx := ContextWithActor(context.Background(), "ops-desk")
	if _, err := l.AmendTransferScheduleCurrencyCtx(ctx, "sched-1", "GBP"); err != nil {
		t.Fatalf("AmendTransferScheduleCurrencyCtx: %v", err)
	}
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	after, err := restored.GetTransferSchedule("sched-1")
	if err != nil {
		t.Fatalf("restored schedule missing: %v", err)
	}
	if after.Currency != "GBP" || after.ToCurrency != "EUR" {
		t.Errorf("restored currency/pair = %q/%q, want GBP/EUR", after.Currency, after.ToCurrency)
	}
	after.Currency = before.Currency
	if !reflect.DeepEqual(after, before) {
		t.Errorf("restored schedule = %+v, want %+v except currency", after, before)
	}
	// A second revision rides the same row through snapshots.
	if _, err := l.AmendTransferScheduleCurrency("sched-1", "JPY"); err != nil {
		t.Fatalf("AmendTransferScheduleCurrency second: %v", err)
	}
	buf.Reset()
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err = ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	second, err := restored.GetTransferSchedule("sched-1")
	if err != nil {
		t.Fatalf("restored schedule missing: %v", err)
	}
	if second.Currency != "JPY" || second.ToCurrency != "EUR" {
		t.Errorf("restored second currency/pair = %q/%q, want JPY/EUR", second.Currency, second.ToCurrency)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["transfer_schedule_currency_amended"]
	if len(events) != 2 {
		t.Fatalf("amend events = %d, want 2 (two revisions; no-op must not audit)", len(events))
	}
	if events[0].Actor != "ops-desk" {
		t.Errorf("event actor = %q, want ops-desk", events[0].Actor)
	}
	d := events[0].Details
	if d["schedule_id"] != "sched-1" ||
		d["previous_currency"] != "USD" ||
		d["currency"] != "GBP" ||
		d["to_currency"] != "EUR" ||
		d["run_seq"] != float64(0) {
		t.Errorf("event details = %+v, want sched-1 USD->GBP to EUR run_seq 0", d)
	}
	d = events[1].Details
	if d["previous_currency"] != "GBP" ||
		d["currency"] != "JPY" ||
		d["to_currency"] != "EUR" {
		t.Errorf("second event details = %+v, want GBP->JPY to EUR", d)
	}
}
