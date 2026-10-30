package ledger

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// createFXSchedule registers an active USD->EUR schedule settling
// through "fxdesk" and returns the recorded schedule.
func createFXSchedule(t *testing.T, l *Ledger, id string, now time.Time) TransferSchedule {
	t.Helper()
	s := testSchedule(now)
	s.ID = id
	s.ToCurrency = "EUR"
	s.FXAccount = "fxdesk"
	created, _, err := l.CreateTransferSchedule(s)
	if err != nil {
		t.Fatalf("CreateTransferSchedule: %v", err)
	}
	return created
}

func TestAmendTransferScheduleFxPairValidation(t *testing.T) {
	l := New()
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := l.AmendTransferScheduleFxPair("sched-unknown", "EUR", "fxdesk"); !errors.Is(err, ErrScheduleNotFound) {
		t.Errorf("unknown schedule: err = %v, want ErrScheduleNotFound", err)
	}
	orig := createFXSchedule(t, l, "sched-1", now)

	// Cancelled.
	l2 := New()
	createFXSchedule(t, l2, "sched-1", now)
	if _, err := l2.CancelTransferSchedule("sched-1"); err != nil {
		t.Fatalf("CancelTransferSchedule: %v", err)
	}
	if _, err := l2.AmendTransferScheduleFxPair("sched-1", "GBP", "fxdesk"); !errors.Is(err, ErrScheduleBadTransition) {
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
	if _, err := l3.AmendTransferScheduleFxPair("sched-1", "EUR", "fxdesk"); !errors.Is(err, ErrScheduleBadTransition) {
		t.Errorf("completed schedule: err = %v, want ErrScheduleBadTransition", err)
	}

	// Amending to the current pair is a no-op, not a change.
	if got, err := l.AmendTransferScheduleFxPair("sched-1", "EUR", "fxdesk"); err != nil || !reflect.DeepEqual(got, orig) {
		t.Errorf("same-pair amendment = %+v, %v, want %+v, nil", got, err, orig)
	}

	// Every rejection leaves the original pair exactly as it was.
	rejections := []struct {
		name       string
		toCurrency string
		fxAccount  AccountID
		want       error
	}{
		{"orphan settlement account", "", "fxdesk", ErrScheduleBadFXPair},
		{"target equals schedule currency", "USD", "fxdesk", ErrCrossCurrencyTransfer},
		{"malformed target currency", "US", "fxdesk", ErrInvalidCurrency},
		{"fx account equals payer", "GBP", "alice", ErrInvalidFXAccount},
		{"fx account equals payee", "GBP", "bob", ErrInvalidFXAccount},
	}
	for _, tc := range rejections {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := l.AmendTransferScheduleFxPair("sched-1", tc.toCurrency, tc.fxAccount); !errors.Is(err, tc.want) {
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

	// Active plan: only the pair changes — legs, amount, currency,
	// fees, memo, interval, next run time, end time, run sequence,
	// and run history are untouched.
	amended, err := l.AmendTransferScheduleFxPair("sched-1", "GBP", "fxdesk2")
	if err != nil {
		t.Fatalf("AmendTransferScheduleFxPair active: %v", err)
	}
	if amended.ToCurrency != "GBP" || amended.FXAccount != "fxdesk2" {
		t.Errorf("amended pair = %q/%q, want GBP/fxdesk2", amended.ToCurrency, amended.FXAccount)
	}
	amended.ToCurrency = orig.ToCurrency
	amended.FXAccount = orig.FXAccount
	if !reflect.DeepEqual(amended, orig) {
		t.Errorf("amended schedule = %+v, want %+v except pair", amended, orig)
	}

	// Setting with an empty settlement account is legal: the pair
	// records no account and fire time falls back to the
	// ledger-level FX account.
	fallback, err := l.AmendTransferScheduleFxPair("sched-1", "JPY", "")
	if err != nil {
		t.Fatalf("AmendTransferScheduleFxPair empty fx account: %v", err)
	}
	if fallback.ToCurrency != "JPY" || fallback.FXAccount != "" {
		t.Errorf("fallback pair = %q/%q, want JPY/empty", fallback.ToCurrency, fallback.FXAccount)
	}

	// Clearing back to (empty, empty) restores same-currency
	// settlement, and clearing again is a no-op.
	cleared, err := l.AmendTransferScheduleFxPair("sched-1", "", "")
	if err != nil {
		t.Fatalf("AmendTransferScheduleFxPair clear: %v", err)
	}
	if cleared.ToCurrency != "" || cleared.FXAccount != "" {
		t.Errorf("cleared pair = %q/%q, want empty/empty", cleared.ToCurrency, cleared.FXAccount)
	}
	if got, err := l.AmendTransferScheduleFxPair("sched-1", "", ""); err != nil || !reflect.DeepEqual(got, cleared) {
		t.Errorf("re-clear = %+v, %v, want %+v, nil", got, err, cleared)
	}

	// Paused plan: pausing is a hold, not a terminal state, so the
	// revision waits with the plan — and again only the pair moves.
	l4 := New()
	orig4 := createFXSchedule(t, l4, "sched-1", now)
	if _, err := l4.PauseTransferSchedule("sched-1"); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	paused, err := l4.AmendTransferScheduleFxPair("sched-1", "GBP", "fxdesk2")
	if err != nil {
		t.Fatalf("AmendTransferScheduleFxPair paused: %v", err)
	}
	if paused.ToCurrency != "GBP" || paused.FXAccount != "fxdesk2" || paused.Status != SchedulePaused {
		t.Errorf("amended paused = %q/%q status %q, want GBP/fxdesk2 paused", paused.ToCurrency, paused.FXAccount, paused.Status)
	}
	paused.ToCurrency = orig4.ToCurrency
	paused.FXAccount = orig4.FXAccount
	paused.Status = orig4.Status
	if !reflect.DeepEqual(paused, orig4) {
		t.Errorf("amended paused schedule = %+v, want %+v except pair", paused, orig4)
	}
}

func TestAmendTransferScheduleFxPairNextRelease(t *testing.T) {
	// The new pair governs the next release with no firing-path
	// change, on a plan that starts same-currency: the first run
	// books USD to the payee, the amendment itself posts nothing,
	// the second run books EUR at the 9/10 rate (900 on 1000) and
	// the first run's USD leg is never rewritten.
	l := New(WithFXAccount("fxdesk"))
	now := time.Now().UTC().Truncate(time.Second)
	fundAccount(t, l, "alice", 100000, "USD")
	fundAccount(t, l, "fxdesk", 100000, "USD")
	if err := l.SetFXRate("USD", "EUR", 9, 10); err != nil {
		t.Fatalf("SetFXRate: %v", err)
	}
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
	if _, err := l.AmendTransferScheduleFxPair("sched-1", "EUR", "fxdesk"); err != nil {
		t.Fatalf("AmendTransferScheduleFxPair: %v", err)
	}
	if got := l.BalanceIn("bob", "USD"); got != 1000 {
		t.Fatalf("bob USD after amendment = %d, want 1000 (amendment posts nothing)", got)
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
	if got := l.BalanceIn("bob", "EUR"); got != 900 {
		t.Errorf("bob EUR after second run = %d, want 900 (floor(1000*9/10))", got)
	}
	if got := l.BalanceIn("alice", "USD"); got != 100000-2*1000 {
		t.Errorf("alice after two runs = %d, want %d", got, 100000-2*1000)
	}
	if runs, err := l.ListScheduleRuns("sched-1", 10); err != nil || len(runs) != 2 {
		t.Errorf("run history = %d runs, %v, want 2", len(runs), err)
	}
}

func TestAmendTransferScheduleFxPairFailedRunRetryReadsNewPair(t *testing.T) {
	// A run that failed for a missing GBP rate posts nothing, so
	// its retry under the same "<ID>/run/<seq>" key reads the
	// amended pair: with the pair moved to EUR (rate set, and an
	// empty settlement account falling back to the ledger-level FX
	// account) the retry books exactly once, in EUR.
	l := New(WithFXAccount("fxdesk"))
	now := time.Now().UTC().Truncate(time.Second)
	fundAccount(t, l, "alice", 100000, "USD")
	fundAccount(t, l, "fxdesk", 100000, "USD")
	if err := l.SetFXRate("USD", "EUR", 1, 1); err != nil {
		t.Fatalf("SetFXRate: %v", err)
	}
	s := testSchedule(now)
	s.ID = "sched-1"
	s.ToCurrency = "GBP"
	s.FXAccount = ""
	if _, _, err := l.CreateTransferSchedule(s); err != nil {
		t.Fatalf("CreateTransferSchedule: %v", err)
	}
	if runs := l.SweepDue(now); len(runs) != 1 || runs[0].Status != "failed" {
		t.Fatalf("first sweep runs = %+v, want one failed run (no GBP rate)", runs)
	}
	if got := l.BalanceIn("bob", "GBP"); got != 0 {
		t.Fatalf("bob GBP after failed run = %d, want 0 (failed run posted nothing)", got)
	}
	if _, err := l.AmendTransferScheduleFxPair("sched-1", "EUR", ""); err != nil {
		t.Fatalf("AmendTransferScheduleFxPair: %v", err)
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
		t.Errorf("bob EUR after retry = %d, want 1000 (retry booked at the amended pair)", got)
	}
	// ListScheduleRuns returns newest first: the fired retry, then
	// the failed attempt, both under the same run sequence.
	if runs, err := l.ListScheduleRuns("sched-1", 10); err != nil || len(runs) != 2 ||
		runs[0].Status != "fired" || runs[1].Status != "failed" || runs[0].RunSeq != runs[1].RunSeq {
		t.Errorf("run history = %+v, %v, want fired+failed under one seq", runs, err)
	}
}

func TestAmendTransferScheduleFxPairAuditAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	now := time.Now().UTC().Truncate(time.Second)
	before := createFXSchedule(t, l, "sched-1", now)
	// The no-op revision emits nothing.
	if _, err := l.AmendTransferScheduleFxPair("sched-1", "EUR", "fxdesk"); err != nil {
		t.Fatalf("no-op amend: %v", err)
	}
	ctx := ContextWithActor(context.Background(), "ops-desk")
	if _, err := l.AmendTransferScheduleFxPairCtx(ctx, "sched-1", "GBP", "fxdesk2"); err != nil {
		t.Fatalf("AmendTransferScheduleFxPairCtx: %v", err)
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
	if after.ToCurrency != "GBP" || after.FXAccount != "fxdesk2" {
		t.Errorf("restored pair = %q/%q, want GBP/fxdesk2", after.ToCurrency, after.FXAccount)
	}
	after.ToCurrency = before.ToCurrency
	after.FXAccount = before.FXAccount
	if !reflect.DeepEqual(after, before) {
		t.Errorf("restored schedule = %+v, want %+v except pair", after, before)
	}
	// A pair cleared back to empty round-trips as empty, not as a
	// resurrected settlement pair.
	if _, err := l.AmendTransferScheduleFxPair("sched-1", "", ""); err != nil {
		t.Fatalf("AmendTransferScheduleFxPair clear: %v", err)
	}
	buf.Reset()
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err = ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	cleared, err := restored.GetTransferSchedule("sched-1")
	if err != nil {
		t.Fatalf("restored schedule missing: %v", err)
	}
	if cleared.ToCurrency != "" || cleared.FXAccount != "" {
		t.Errorf("restored cleared pair = %q/%q, want empty/empty", cleared.ToCurrency, cleared.FXAccount)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["transfer_schedule_fx_pair_amended"]
	if len(events) != 2 {
		t.Fatalf("amend events = %d, want 2 (set + clear; no-op must not audit)", len(events))
	}
	if events[0].Actor != "ops-desk" {
		t.Errorf("event actor = %q, want ops-desk", events[0].Actor)
	}
	d := events[0].Details
	if d["schedule_id"] != "sched-1" ||
		d["previous_to_currency"] != "EUR" ||
		d["to_currency"] != "GBP" ||
		d["previous_fx_account"] != "fxdesk" ||
		d["fx_account"] != "fxdesk2" ||
		d["currency"] != "USD" ||
		d["run_seq"] != float64(0) {
		t.Errorf("event details = %+v, want sched-1 EUR/fxdesk->GBP/fxdesk2 USD run_seq 0", d)
	}
	d = events[1].Details
	if d["previous_to_currency"] != "GBP" ||
		d["to_currency"] != "" ||
		d["previous_fx_account"] != "fxdesk2" ||
		d["fx_account"] != "" {
		t.Errorf("clear event details = %+v, want GBP/fxdesk2->empty/empty", d)
	}
}
