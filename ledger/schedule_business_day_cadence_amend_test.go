package ledger

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestAmendTransferScheduleBusinessDayCadenceValidation(t *testing.T) {
	l := New()
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := l.AmendTransferScheduleBusinessDayCadence("sched-unknown", true); !errors.Is(err, ErrScheduleNotFound) {
		t.Errorf("unknown schedule: err = %v, want ErrScheduleNotFound", err)
	}
	orig := createFeeSchedule(t, l, "sched-1", now)

	// Cancelled.
	l2 := New()
	createFeeSchedule(t, l2, "sched-1", now)
	if _, err := l2.CancelTransferSchedule("sched-1"); err != nil {
		t.Fatalf("CancelTransferSchedule: %v", err)
	}
	if _, err := l2.AmendTransferScheduleBusinessDayCadence("sched-1", true); !errors.Is(err, ErrScheduleBadTransition) {
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
	if _, err := l3.AmendTransferScheduleBusinessDayCadence("sched-1", true); !errors.Is(err, ErrScheduleBadTransition) {
		t.Errorf("completed schedule: err = %v, want ErrScheduleBadTransition", err)
	}

	// Amending to the current rhythm value is a no-op, not a change.
	if got, err := l.AmendTransferScheduleBusinessDayCadence("sched-1", false); err != nil || !reflect.DeepEqual(got, orig) {
		t.Errorf("same-rhythm amendment = %+v, %v, want %+v, nil", got, err, orig)
	}

	// Active plan: only BusinessDayCadence changes — Interval and
	// NextRunAt in particular are untouched, so there is no
	// recomputation at amend time; the new rhythm only decides
	// how the next firing advances.
	amended, err := l.AmendTransferScheduleBusinessDayCadence("sched-1", true)
	if err != nil {
		t.Fatalf("AmendTransferScheduleBusinessDayCadence active: %v", err)
	}
	if !amended.BusinessDayCadence {
		t.Errorf("amended cadence = %v, want true", amended.BusinessDayCadence)
	}
	if amended.Interval != time.Hour || !amended.NextRunAt.Equal(now) {
		t.Errorf("amended interval/next = %v/%v, want 1h/%v (unchanged)", amended.Interval, amended.NextRunAt, now)
	}
	amended.BusinessDayCadence = orig.BusinessDayCadence
	if !reflect.DeepEqual(amended, orig) {
		t.Errorf("amended schedule = %+v, want %+v except cadence", amended, orig)
	}

	// A boolean is always legal: switching back off is legal too,
	// and restores exactly the fixed-duration rhythm the plan had.
	back, err := l.AmendTransferScheduleBusinessDayCadence("sched-1", false)
	if err != nil {
		t.Fatalf("AmendTransferScheduleBusinessDayCadence switch off: %v", err)
	}
	if back.BusinessDayCadence || !reflect.DeepEqual(back, orig) {
		t.Errorf("switched-off schedule = %+v, want %+v", back, orig)
	}

	// Paused plan: pausing is a hold, not a terminal state, so the
	// revision waits with the plan — and again only the rhythm moves.
	l4 := New()
	orig4 := createFeeSchedule(t, l4, "sched-1", now)
	if _, err := l4.PauseTransferSchedule("sched-1"); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	paused, err := l4.AmendTransferScheduleBusinessDayCadence("sched-1", true)
	if err != nil {
		t.Fatalf("AmendTransferScheduleBusinessDayCadence paused: %v", err)
	}
	if !paused.BusinessDayCadence || paused.Status != SchedulePaused {
		t.Errorf("amended paused = cadence %v status %q, want true paused", paused.BusinessDayCadence, paused.Status)
	}
	paused.BusinessDayCadence = orig4.BusinessDayCadence
	paused.Status = orig4.Status
	if !reflect.DeepEqual(paused, orig4) {
		t.Errorf("amended paused schedule = %+v, want %+v except cadence", paused, orig4)
	}
}

func TestAmendTransferScheduleBusinessDayCadenceNextRelease(t *testing.T) {
	// The new rhythm governs the next firing's advance with no
	// firing-path change: a daily plan firing on Friday advances
	// to Saturday under the fixed rhythm, to Monday once switched
	// to business days, and back to a fixed Tuesday once switched
	// off — already-fired advances are never rewritten. The
	// amendment itself posts nothing and books no run.
	l := New()
	seedBalance(t, l, "payer", 10_000_000)
	if _, _, err := l.CreateTransferSchedule(TransferSchedule{
		ID:          "sched-bd",
		From:        "payer",
		To:          "payee",
		AmountCents: 1000,
		Currency:    "USD",
		Interval:    24 * time.Hour,
		NextRunAt:   bdFriday,
	}); err != nil {
		t.Fatalf("CreateTransferSchedule: %v", err)
	}
	if runs := l.SweepDue(bdFriday.Add(2 * time.Hour)); len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("first sweep runs = %+v, want one fired run", runs)
	}
	after, err := l.GetTransferSchedule("sched-bd")
	if err != nil {
		t.Fatalf("GetTransferSchedule: %v", err)
	}
	bdSaturday := time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC)
	if !after.NextRunAt.Equal(bdSaturday) {
		t.Fatalf("NextRunAt after fixed advance = %v, want %v (Saturday)", after.NextRunAt, bdSaturday)
	}
	if _, err := l.AmendTransferScheduleBusinessDayCadence("sched-bd", true); err != nil {
		t.Fatalf("AmendTransferScheduleBusinessDayCadence: %v", err)
	}
	if got := l.BalanceIn("payer", "USD"); got != 10_000_000-1000 {
		t.Fatalf("payer after amendment = %d, want %d (amendment posts nothing)", got, 10_000_000-1000)
	}
	if runs := l.SweepDue(bdSaturday.Add(2 * time.Hour)); len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("second sweep runs = %+v, want one fired run", runs)
	}
	after, err = l.GetTransferSchedule("sched-bd")
	if err != nil {
		t.Fatalf("GetTransferSchedule: %v", err)
	}
	if !after.NextRunAt.Equal(bdMonday) {
		t.Fatalf("NextRunAt after business-day advance = %v, want %v (Monday)", after.NextRunAt, bdMonday)
	}
	if _, err := l.AmendTransferScheduleBusinessDayCadence("sched-bd", false); err != nil {
		t.Fatalf("AmendTransferScheduleBusinessDayCadence switch off: %v", err)
	}
	if runs := l.SweepDue(bdMonday.Add(2 * time.Hour)); len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("third sweep runs = %+v, want one fired run", runs)
	}
	after, err = l.GetTransferSchedule("sched-bd")
	if err != nil {
		t.Fatalf("GetTransferSchedule: %v", err)
	}
	if !after.NextRunAt.Equal(bdTuesday) {
		t.Fatalf("NextRunAt after fixed advance again = %v, want %v (Tuesday)", after.NextRunAt, bdTuesday)
	}
	if got := l.BalanceIn("payer", "USD"); got != 10_000_000-3*1000 {
		t.Errorf("payer after three runs = %d, want %d", got, 10_000_000-3*1000)
	}
	if runs, err := l.ListScheduleRuns("sched-bd", 10); err != nil || len(runs) != 3 {
		t.Errorf("run history = %d runs, %v, want 3", len(runs), err)
	}

	// Paused revision waits with the plan: amend while paused,
	// resume, and the first firing after resume advances on
	// business days.
	l2 := New()
	seedBalance(t, l2, "payer", 10_000_000)
	if _, _, err := l2.CreateTransferSchedule(TransferSchedule{
		ID:          "sched-paused",
		From:        "payer",
		To:          "payee",
		AmountCents: 1000,
		Currency:    "USD",
		Interval:    24 * time.Hour,
		NextRunAt:   bdFriday,
	}); err != nil {
		t.Fatalf("CreateTransferSchedule paused: %v", err)
	}
	if _, err := l2.PauseTransferSchedule("sched-paused"); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	if runs := l2.SweepDue(bdFriday.Add(2 * time.Hour)); len(runs) != 0 {
		t.Fatalf("paused sweep runs = %+v, want none", runs)
	}
	if _, err := l2.AmendTransferScheduleBusinessDayCadence("sched-paused", true); err != nil {
		t.Fatalf("AmendTransferScheduleBusinessDayCadence paused: %v", err)
	}
	if _, err := l2.ResumeTransferSchedule("sched-paused"); err != nil {
		t.Fatalf("ResumeTransferSchedule: %v", err)
	}
	if runs := l2.SweepDue(bdFriday.Add(2 * time.Hour)); len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("resumed sweep runs = %+v, want one fired run", runs)
	}
	after2, err := l2.GetTransferSchedule("sched-paused")
	if err != nil {
		t.Fatalf("GetTransferSchedule paused: %v", err)
	}
	if !after2.NextRunAt.Equal(bdMonday) {
		t.Fatalf("NextRunAt after resumed business-day advance = %v, want %v (Monday)", after2.NextRunAt, bdMonday)
	}
}

func TestAmendTransferScheduleBusinessDayCadenceAuditAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	now := time.Now().UTC().Truncate(time.Second)
	before := createFeeSchedule(t, l, "sched-1", now)
	// The no-op revision emits nothing.
	if _, err := l.AmendTransferScheduleBusinessDayCadence("sched-1", false); err != nil {
		t.Fatalf("no-op amend: %v", err)
	}
	ctx := ContextWithActor(context.Background(), "ops-desk")
	if _, err := l.AmendTransferScheduleBusinessDayCadenceCtx(ctx, "sched-1", true); err != nil {
		t.Fatalf("AmendTransferScheduleBusinessDayCadenceCtx: %v", err)
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
	if !after.BusinessDayCadence {
		t.Errorf("restored cadence = %v, want true", after.BusinessDayCadence)
	}
	after.BusinessDayCadence = before.BusinessDayCadence
	if !reflect.DeepEqual(after, before) {
		t.Errorf("restored schedule = %+v, want %+v except cadence", after, before)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["transfer_schedule_business_day_cadence_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1 (no-op must not audit)", len(events))
	}
	if events[0].Actor != "ops-desk" {
		t.Errorf("event actor = %q, want ops-desk", events[0].Actor)
	}
	d := events[0].Details
	if d["schedule_id"] != "sched-1" ||
		d["previous_business_day_cadence"] != false ||
		d["business_day_cadence"] != true ||
		d["interval"] != "1h0m0s" ||
		d["currency"] != "USD" ||
		d["run_seq"] != float64(0) {
		t.Errorf("event details = %+v, want sched-1 false->true 1h USD run_seq 0", d)
	}
}

func TestAmendTransferScheduleBusinessDayCadenceOffRoundTrip(t *testing.T) {
	// A rhythm flipped back off round-trips through snapshots as
	// false, not as a resurrected business-day cadence: switch on,
	// switch off, then export and restore.
	l := New()
	now := time.Now().UTC().Truncate(time.Second)
	createFeeSchedule(t, l, "sched-1", now)
	if _, err := l.AmendTransferScheduleBusinessDayCadence("sched-1", true); err != nil {
		t.Fatalf("AmendTransferScheduleBusinessDayCadence on: %v", err)
	}
	if _, err := l.AmendTransferScheduleBusinessDayCadence("sched-1", false); err != nil {
		t.Fatalf("AmendTransferScheduleBusinessDayCadence off: %v", err)
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
	if after.BusinessDayCadence {
		t.Errorf("restored cadence = %v, want false", after.BusinessDayCadence)
	}
	if after.Interval != time.Hour || after.FeeCents != 10 || after.FeeAccount != "fees" {
		t.Errorf("restored interval/fee/account = %v/%d/%q, want 1h/10/fees (untouched)", after.Interval, after.FeeCents, after.FeeAccount)
	}
}
