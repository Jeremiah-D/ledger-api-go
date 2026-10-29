package ledger

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestAmendTransferScheduleSkipFeeValidation(t *testing.T) {
	l := New()
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := l.AmendTransferScheduleSkipFee("sched-unknown", true); !errors.Is(err, ErrScheduleNotFound) {
		t.Errorf("unknown schedule: err = %v, want ErrScheduleNotFound", err)
	}
	orig := createFeeSchedule(t, l, "sched-1", now)

	// Cancelled.
	l2 := New()
	createFeeSchedule(t, l2, "sched-1", now)
	if _, err := l2.CancelTransferSchedule("sched-1"); err != nil {
		t.Fatalf("CancelTransferSchedule: %v", err)
	}
	if _, err := l2.AmendTransferScheduleSkipFee("sched-1", true); !errors.Is(err, ErrScheduleBadTransition) {
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
	if _, err := l3.AmendTransferScheduleSkipFee("sched-1", true); !errors.Is(err, ErrScheduleBadTransition) {
		t.Errorf("completed schedule: err = %v, want ErrScheduleBadTransition", err)
	}

	// Amending to the current switch value is a no-op, not a change.
	if got, err := l.AmendTransferScheduleSkipFee("sched-1", false); err != nil || !reflect.DeepEqual(got, orig) {
		t.Errorf("same-switch amendment = %+v, %v, want %+v, nil", got, err, orig)
	}

	// Active plan: only SkipFee changes — FeeCents and FeeAccount
	// in particular are untouched, so switching back off later
	// restores exactly the previous fee behaviour.
	amended, err := l.AmendTransferScheduleSkipFee("sched-1", true)
	if err != nil {
		t.Fatalf("AmendTransferScheduleSkipFee active: %v", err)
	}
	if !amended.SkipFee {
		t.Errorf("amended skip fee = %v, want true", amended.SkipFee)
	}
	if amended.FeeCents != 10 || amended.FeeAccount != "fees" {
		t.Errorf("amended fee/account = %d/%q, want 10/fees (unchanged)", amended.FeeCents, amended.FeeAccount)
	}
	amended.SkipFee = orig.SkipFee
	if !reflect.DeepEqual(amended, orig) {
		t.Errorf("amended schedule = %+v, want %+v except skip fee", amended, orig)
	}

	// A boolean is always legal: switching back off is legal too,
	// on a plan with a positive fee and a fee account.
	back, err := l.AmendTransferScheduleSkipFee("sched-1", false)
	if err != nil {
		t.Fatalf("AmendTransferScheduleSkipFee switch off: %v", err)
	}
	if back.SkipFee || !reflect.DeepEqual(back, orig) {
		t.Errorf("switched-off schedule = %+v, want %+v", back, orig)
	}

	// Paused plan: pausing is a hold, not a terminal state, so the
	// revision waits with the plan — and again only the switch moves.
	l4 := New()
	orig4 := createFeeSchedule(t, l4, "sched-1", now)
	if _, err := l4.PauseTransferSchedule("sched-1"); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	paused, err := l4.AmendTransferScheduleSkipFee("sched-1", true)
	if err != nil {
		t.Fatalf("AmendTransferScheduleSkipFee paused: %v", err)
	}
	if !paused.SkipFee || paused.Status != SchedulePaused {
		t.Errorf("amended paused = skip %v status %q, want true paused", paused.SkipFee, paused.Status)
	}
	paused.SkipFee = orig4.SkipFee
	paused.Status = orig4.Status
	if !reflect.DeepEqual(paused, orig4) {
		t.Errorf("amended paused schedule = %+v, want %+v except skip fee", paused, orig4)
	}
}

func TestAmendTransferScheduleSkipFeeNextRelease(t *testing.T) {
	// The new switch governs the next release with no firing-path
	// change, on a plan with no explicit fee: the first run books
	// the 1% policy fee (10 cents on 1000 to the revenue account),
	// the amendment itself posts nothing, the second run books no
	// fee leg at all (the revenue account is untouched), and
	// switching back off resumes policy fee booking on the third
	// run — the already-released runs' fee legs are never
	// rewritten.
	l := New(WithTransferFeePolicy(100, "revenue"))
	now := time.Now().UTC().Truncate(time.Second)
	fundAccount(t, l, "alice", 100000, "USD")
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
	if got := l.BalanceIn("revenue", "USD"); got != 10 {
		t.Fatalf("revenue after first run = %d, want 10 (policy fee)", got)
	}
	if _, err := l.AmendTransferScheduleSkipFee("sched-1", true); err != nil {
		t.Fatalf("AmendTransferScheduleSkipFee: %v", err)
	}
	if got := l.BalanceIn("revenue", "USD"); got != 10 {
		t.Fatalf("revenue after amendment = %d, want 10 (amendment posts nothing)", got)
	}
	live, err := l.GetTransferSchedule("sched-1")
	if err != nil {
		t.Fatalf("GetTransferSchedule: %v", err)
	}
	if runs := l.SweepDue(live.NextRunAt); len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("second sweep runs = %+v, want one fired run", runs)
	}
	if got := l.BalanceIn("revenue", "USD"); got != 10 {
		t.Errorf("revenue after second run = %d, want 10 (skipped run books no policy fee)", got)
	}
	if got := l.BalanceIn("alice", "USD"); got != 100000-2*1000-10 {
		t.Errorf("alice after two runs = %d, want %d", got, 100000-2*1000-10)
	}
	if _, err := l.AmendTransferScheduleSkipFee("sched-1", false); err != nil {
		t.Fatalf("AmendTransferScheduleSkipFee switch off: %v", err)
	}
	live, err = l.GetTransferSchedule("sched-1")
	if err != nil {
		t.Fatalf("GetTransferSchedule: %v", err)
	}
	if runs := l.SweepDue(live.NextRunAt); len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("third sweep runs = %+v, want one fired run", runs)
	}
	if got := l.BalanceIn("revenue", "USD"); got != 20 {
		t.Errorf("revenue after third run = %d, want 20 (policy fee booking resumed)", got)
	}
	if got := l.BalanceIn("alice", "USD"); got != 100000-3*1000-2*10 {
		t.Errorf("alice after three runs = %d, want %d", got, 100000-3*1000-2*10)
	}
	if runs, err := l.ListScheduleRuns("sched-1", 10); err != nil || len(runs) != 3 {
		t.Errorf("run history = %d runs, %v, want 3", len(runs), err)
	}
}

func TestAmendTransferScheduleSkipFeeLeavesExplicitFee(t *testing.T) {
	// SkipFee suppresses the ledger's default fee policy only —
	// an explicit FeeCents still applies, exactly as for a one-off
	// transfer (see Transfer.SkipFee). Switching the switch on a
	// plan with an explicit 10-cent fee therefore does NOT drop
	// that fee: both runs book it. This pins the real semantics
	// rather than the looser "no fee leg while switched on"
	// reading, which holds only for policy-fee plans.
	l := New()
	now := time.Now().UTC().Truncate(time.Second)
	fundAccount(t, l, "alice", 100000, "USD")
	createFeeSchedule(t, l, "sched-1", now)
	if runs := l.SweepDue(now); len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("first sweep runs = %+v, want one fired run", runs)
	}
	if _, err := l.AmendTransferScheduleSkipFee("sched-1", true); err != nil {
		t.Fatalf("AmendTransferScheduleSkipFee: %v", err)
	}
	live, err := l.GetTransferSchedule("sched-1")
	if err != nil {
		t.Fatalf("GetTransferSchedule: %v", err)
	}
	if runs := l.SweepDue(live.NextRunAt); len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("second sweep runs = %+v, want one fired run", runs)
	}
	if got := l.BalanceIn("fees", "USD"); got != 20 {
		t.Errorf("fees after skipped-switch run = %d, want 20 (explicit fee still applies)", got)
	}
	if got := l.BalanceIn("alice", "USD"); got != 100000-2*1000-2*10 {
		t.Errorf("alice after two runs = %d, want %d", got, 100000-2*1000-2*10)
	}
}

func TestAmendTransferScheduleSkipFeeAuditAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	now := time.Now().UTC().Truncate(time.Second)
	before := createFeeSchedule(t, l, "sched-1", now)
	// The no-op revision emits nothing.
	if _, err := l.AmendTransferScheduleSkipFee("sched-1", false); err != nil {
		t.Fatalf("no-op amend: %v", err)
	}
	ctx := ContextWithActor(context.Background(), "ops-desk")
	if _, err := l.AmendTransferScheduleSkipFeeCtx(ctx, "sched-1", true); err != nil {
		t.Fatalf("AmendTransferScheduleSkipFeeCtx: %v", err)
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
	if !after.SkipFee {
		t.Errorf("restored skip fee = %v, want true", after.SkipFee)
	}
	after.SkipFee = before.SkipFee
	if !reflect.DeepEqual(after, before) {
		t.Errorf("restored schedule = %+v, want %+v except skip fee", after, before)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["transfer_schedule_skip_fee_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1 (no-op must not audit)", len(events))
	}
	if events[0].Actor != "ops-desk" {
		t.Errorf("event actor = %q, want ops-desk", events[0].Actor)
	}
	d := events[0].Details
	if d["schedule_id"] != "sched-1" ||
		d["previous_skip_fee"] != false ||
		d["skip_fee"] != true ||
		d["fee_cents"] != float64(10) ||
		d["fee_account"] != "fees" ||
		d["currency"] != "USD" ||
		d["run_seq"] != float64(0) {
		t.Errorf("event details = %+v, want sched-1 false->true 10 fees USD run_seq 0", d)
	}
}

func TestAmendTransferScheduleSkipFeeOffRoundTrip(t *testing.T) {
	// A switch flipped back off round-trips through snapshots as
	// false, not as a resurrected skip: switch on, switch off,
	// then export and restore.
	l := New()
	now := time.Now().UTC().Truncate(time.Second)
	createFeeSchedule(t, l, "sched-1", now)
	if _, err := l.AmendTransferScheduleSkipFee("sched-1", true); err != nil {
		t.Fatalf("AmendTransferScheduleSkipFee on: %v", err)
	}
	if _, err := l.AmendTransferScheduleSkipFee("sched-1", false); err != nil {
		t.Fatalf("AmendTransferScheduleSkipFee off: %v", err)
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
	if after.SkipFee {
		t.Errorf("restored skip fee = %v, want false", after.SkipFee)
	}
	if after.FeeCents != 10 || after.FeeAccount != "fees" {
		t.Errorf("restored fee/account = %d/%q, want 10/fees (untouched)", after.FeeCents, after.FeeAccount)
	}
}
