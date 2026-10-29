package ledger

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestAmendTransferScheduleFeeAccountValidation(t *testing.T) {
	l := New()
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := l.AmendTransferScheduleFeeAccount("sched-unknown", "fees2"); !errors.Is(err, ErrScheduleNotFound) {
		t.Errorf("unknown schedule: err = %v, want ErrScheduleNotFound", err)
	}
	orig := createFeeSchedule(t, l, "sched-1", now)

	// Cancelled.
	l2 := New()
	createFeeSchedule(t, l2, "sched-1", now)
	if _, err := l2.CancelTransferSchedule("sched-1"); err != nil {
		t.Fatalf("CancelTransferSchedule: %v", err)
	}
	if _, err := l2.AmendTransferScheduleFeeAccount("sched-1", "fees2"); !errors.Is(err, ErrScheduleBadTransition) {
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
	if _, err := l3.AmendTransferScheduleFeeAccount("sched-1", "fees2"); !errors.Is(err, ErrScheduleBadTransition) {
		t.Errorf("completed schedule: err = %v, want ErrScheduleBadTransition", err)
	}

	// The creation fee gate is reused: clearing the account while
	// the fee is positive is rejected (a positive fee needs an
	// account to book to), and every rejected amendment leaves the
	// plan exactly as it was.
	if _, err := l.AmendTransferScheduleFeeAccount("sched-1", ""); !errors.Is(err, ErrScheduleBadFee) {
		t.Errorf("clear account with positive fee: err = %v, want ErrScheduleBadFee", err)
	}
	if got, err := l.GetTransferSchedule("sched-1"); err != nil || !reflect.DeepEqual(got, orig) {
		t.Errorf("schedule after rejected amendment = %+v, %v, want %+v", got, err, orig)
	}

	// Amending to the current account is a no-op, not a change.
	if got, err := l.AmendTransferScheduleFeeAccount("sched-1", "fees"); err != nil || !reflect.DeepEqual(got, orig) {
		t.Errorf("same-account amendment = %+v, %v, want %+v, nil", got, err, orig)
	}

	// Active plan: only FeeAccount changes — FeeCents and SkipFee
	// in particular are untouched.
	amended, err := l.AmendTransferScheduleFeeAccount("sched-1", "fees2")
	if err != nil {
		t.Fatalf("AmendTransferScheduleFeeAccount active: %v", err)
	}
	if amended.FeeAccount != "fees2" {
		t.Errorf("amended fee account = %q, want fees2", amended.FeeAccount)
	}
	if amended.FeeCents != 10 || amended.SkipFee {
		t.Errorf("amended fee/skip = %d/%v, want 10/false (unchanged)", amended.FeeCents, amended.SkipFee)
	}
	amended.FeeAccount = orig.FeeAccount
	if !reflect.DeepEqual(amended, orig) {
		t.Errorf("amended schedule = %+v, want %+v except fee account", amended, orig)
	}

	// Zero-fee plan: clearing the account is legal, restating the
	// cleared account is a no-op, and a new account can be set.
	lZero := New()
	if _, _, err := lZero.CreateTransferSchedule(TransferSchedule{
		ID:          "sched-1",
		From:        "alice",
		To:          "bob",
		AmountCents: 1000,
		Currency:    "USD",
		FeeAccount:  "fees",
		Interval:    time.Hour,
		NextRunAt:   now,
	}); err != nil {
		t.Fatalf("create zero-fee schedule: %v", err)
	}
	cleared, err := lZero.AmendTransferScheduleFeeAccount("sched-1", "")
	if err != nil {
		t.Fatalf("AmendTransferScheduleFeeAccount clear: %v", err)
	}
	if cleared.FeeAccount != "" || cleared.FeeCents != 0 {
		t.Errorf("cleared account/fee = %q/%d, want empty/0", cleared.FeeAccount, cleared.FeeCents)
	}
	if got, err := lZero.AmendTransferScheduleFeeAccount("sched-1", ""); err != nil || got.FeeAccount != "" {
		t.Errorf("restate cleared account = %+v, %v, want empty, nil", got, err)
	}
	if got, err := lZero.AmendTransferScheduleFeeAccount("sched-1", "vault-fees"); err != nil || got.FeeAccount != "vault-fees" {
		t.Errorf("set account after clear = %+v, %v, want vault-fees, nil", got, err)
	}

	// Paused plan: pausing is a hold, not a terminal state, so the
	// revision waits with the plan — and again only the account moves.
	l4 := New()
	orig4 := createFeeSchedule(t, l4, "sched-1", now)
	if _, err := l4.PauseTransferSchedule("sched-1"); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	paused, err := l4.AmendTransferScheduleFeeAccount("sched-1", "fees2")
	if err != nil {
		t.Fatalf("AmendTransferScheduleFeeAccount paused: %v", err)
	}
	if paused.FeeAccount != "fees2" || paused.Status != SchedulePaused {
		t.Errorf("amended paused = account %q status %q, want fees2 paused", paused.FeeAccount, paused.Status)
	}
	paused.FeeAccount = orig4.FeeAccount
	paused.Status = orig4.Status
	if !reflect.DeepEqual(paused, orig4) {
		t.Errorf("amended paused schedule = %+v, want %+v except fee account", paused, orig4)
	}
}

func TestAmendTransferScheduleFeeAccountNextRelease(t *testing.T) {
	// The new account governs the next release with no firing-path
	// change: the first run books its 10-cent fee to the original
	// account, the amendment itself posts nothing, and the second
	// run books the same 10-cent fee to the amended account — the
	// already-released run's fee leg is never rewritten.
	l := New()
	now := time.Now().UTC().Truncate(time.Second)
	fundAccount(t, l, "alice", 100000, "USD")
	createFeeSchedule(t, l, "sched-1", now)
	if runs := l.SweepDue(now); len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("first sweep runs = %+v, want one fired run", runs)
	}
	if got := l.BalanceIn("fees", "USD"); got != 10 {
		t.Fatalf("fees after first run = %d, want 10", got)
	}
	if _, err := l.AmendTransferScheduleFeeAccount("sched-1", "fees2"); err != nil {
		t.Fatalf("AmendTransferScheduleFeeAccount: %v", err)
	}
	if got := l.BalanceIn("fees", "USD"); got != 10 {
		t.Fatalf("fees after amendment = %d, want 10 (amendment posts nothing)", got)
	}
	if got := l.BalanceIn("fees2", "USD"); got != 0 {
		t.Fatalf("fees2 after amendment = %d, want 0 (amendment posts nothing)", got)
	}
	live, err := l.GetTransferSchedule("sched-1")
	if err != nil {
		t.Fatalf("GetTransferSchedule: %v", err)
	}
	if runs := l.SweepDue(live.NextRunAt); len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("second sweep runs = %+v, want one fired run", runs)
	}
	if got := l.BalanceIn("fees", "USD"); got != 10 {
		t.Errorf("fees after second run = %d, want 10 (first run's leg untouched)", got)
	}
	if got := l.BalanceIn("fees2", "USD"); got != 10 {
		t.Errorf("fees2 after second run = %d, want 10 (amended account)", got)
	}
	if got := l.BalanceIn("alice", "USD"); got != 100000-2*1000-2*10 {
		t.Errorf("alice after two runs = %d, want %d", got, 100000-2*1000-2*10)
	}
	if runs, err := l.ListScheduleRuns("sched-1", 10); err != nil || len(runs) != 2 {
		t.Errorf("run history = %d runs, %v, want 2", len(runs), err)
	}
}

func TestAmendTransferScheduleFeeAccountAuditAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	now := time.Now().UTC().Truncate(time.Second)
	before := createFeeSchedule(t, l, "sched-1", now)
	// The no-op revision emits nothing.
	if _, err := l.AmendTransferScheduleFeeAccount("sched-1", "fees"); err != nil {
		t.Fatalf("no-op amend: %v", err)
	}
	ctx := ContextWithActor(context.Background(), "ops-desk")
	if _, err := l.AmendTransferScheduleFeeAccountCtx(ctx, "sched-1", "fees2"); err != nil {
		t.Fatalf("AmendTransferScheduleFeeAccountCtx: %v", err)
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
	if after.FeeAccount != "fees2" {
		t.Errorf("restored fee account = %q, want fees2", after.FeeAccount)
	}
	after.FeeAccount = before.FeeAccount
	if !reflect.DeepEqual(after, before) {
		t.Errorf("restored schedule = %+v, want %+v except fee account", after, before)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["transfer_schedule_fee_account_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1 (no-op must not audit)", len(events))
	}
	if events[0].Actor != "ops-desk" {
		t.Errorf("event actor = %q, want ops-desk", events[0].Actor)
	}
	d := events[0].Details
	if d["schedule_id"] != "sched-1" ||
		d["previous_fee_account"] != "fees" ||
		d["fee_account"] != "fees2" ||
		d["fee_cents"] != float64(10) ||
		d["currency"] != "USD" ||
		d["run_seq"] != float64(0) {
		t.Errorf("event details = %+v, want sched-1 fees->fees2 10 USD run_seq 0", d)
	}
}

func TestAmendTransferScheduleFeeAccountClearedSnapshotRoundTrip(t *testing.T) {
	// An account cleared to empty round-trips through snapshots as
	// empty, not as a resurrected charge account: clear the fee
	// first (LG-256 keeps the account), then clear the account
	// (legal once the fee is zero).
	l := New()
	now := time.Now().UTC().Truncate(time.Second)
	createFeeSchedule(t, l, "sched-1", now)
	if _, err := l.AmendTransferScheduleFee("sched-1", 0); err != nil {
		t.Fatalf("AmendTransferScheduleFee clear: %v", err)
	}
	if _, err := l.AmendTransferScheduleFeeAccount("sched-1", ""); err != nil {
		t.Fatalf("AmendTransferScheduleFeeAccount clear: %v", err)
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
	if after.FeeAccount != "" {
		t.Errorf("restored fee account = %q, want empty", after.FeeAccount)
	}
	if after.FeeCents != 0 {
		t.Errorf("restored fee = %d, want zero", after.FeeCents)
	}
}

func TestAmendTransferScheduleFeeAccountClearedBlocksPositiveFee(t *testing.T) {
	// The two amendments interlock: once the account is cleared
	// (fee already zero), a positive fee has nowhere to book and
	// is rejected until an account is restored — proving the
	// cleared state is real, not just a display value.
	l := New()
	now := time.Now().UTC().Truncate(time.Second)
	createFeeSchedule(t, l, "sched-1", now)
	if _, err := l.AmendTransferScheduleFee("sched-1", 0); err != nil {
		t.Fatalf("AmendTransferScheduleFee clear: %v", err)
	}
	if _, err := l.AmendTransferScheduleFeeAccount("sched-1", ""); err != nil {
		t.Fatalf("AmendTransferScheduleFeeAccount clear: %v", err)
	}
	if _, err := l.AmendTransferScheduleFee("sched-1", 10); !errors.Is(err, ErrScheduleBadFee) {
		t.Errorf("positive fee without account: err = %v, want ErrScheduleBadFee", err)
	}
	if _, err := l.AmendTransferScheduleFeeAccount("sched-1", "fees"); err != nil {
		t.Fatalf("AmendTransferScheduleFeeAccount restore: %v", err)
	}
	if back, err := l.AmendTransferScheduleFee("sched-1", 10); err != nil || back.FeeCents != 10 || back.FeeAccount != "fees" {
		t.Errorf("fee after restore = %+v, %v, want 10/fees", back, err)
	}
}
