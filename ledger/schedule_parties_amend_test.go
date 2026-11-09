package ledger

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// createPartiesSchedule registers one active plan for the parties
// amendment tests: alice pays bob 1000 cents hourly, no fee, with an
// end time far in the future, so the amendment tests can prove every
// other field survives untouched.
func createPartiesSchedule(t *testing.T, l *Ledger, id string, now time.Time) TransferSchedule {
	t.Helper()
	sched, _, err := l.CreateTransferSchedule(TransferSchedule{
		ID:          id,
		From:        "alice",
		To:          "bob",
		AmountCents: 1000,
		Currency:    "USD",
		Interval:    24 * time.Hour,
		NextRunAt:   now,
		EndsAt:      now.Add(30 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateTransferSchedule %s: %v", id, err)
	}
	return sched
}

func TestAmendTransferSchedulePartiesValidation(t *testing.T) {
	l := New()
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := l.AmendTransferSchedulePayer("sched-unknown", "carol"); !errors.Is(err, ErrScheduleNotFound) {
		t.Errorf("unknown schedule payer: err = %v, want ErrScheduleNotFound", err)
	}
	if _, err := l.AmendTransferSchedulePayee("sched-unknown", "dave"); !errors.Is(err, ErrScheduleNotFound) {
		t.Errorf("unknown schedule payee: err = %v, want ErrScheduleNotFound", err)
	}
	orig := createPartiesSchedule(t, l, "sched-1", now)

	// Cancelled.
	l2 := New()
	createPartiesSchedule(t, l2, "sched-1", now)
	if _, err := l2.CancelTransferSchedule("sched-1"); err != nil {
		t.Fatalf("CancelTransferSchedule: %v", err)
	}
	if _, err := l2.AmendTransferSchedulePayer("sched-1", "carol"); !errors.Is(err, ErrScheduleBadTransition) {
		t.Errorf("cancelled schedule payer: err = %v, want ErrScheduleBadTransition", err)
	}
	if _, err := l2.AmendTransferSchedulePayee("sched-1", "dave"); !errors.Is(err, ErrScheduleBadTransition) {
		t.Errorf("cancelled schedule payee: err = %v, want ErrScheduleBadTransition", err)
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
	if _, err := l3.AmendTransferSchedulePayer("sched-1", "carol"); !errors.Is(err, ErrScheduleBadTransition) {
		t.Errorf("completed schedule payer: err = %v, want ErrScheduleBadTransition", err)
	}

	// Fired but still active: RunSeq != 0 refuses the amendment even
	// though the plan is not terminal — the fired run's journaled
	// legs must not be split from the plan's future legs.
	l4 := New()
	fundAccount(t, l4, "alice", 100000, "USD")
	s4 := testSchedule(now)
	s4.ID = "sched-1"
	s4.Interval = time.Hour
	s4.NextRunAt = now.Add(-time.Minute)
	s4.EndsAt = now.Add(30 * 24 * time.Hour)
	if _, _, err := l4.CreateTransferSchedule(s4); err != nil {
		t.Fatalf("create: %v", err)
	}
	if runs := l4.SweepDue(now); len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("sweep runs = %+v, want one fired run", runs)
	}
	if got, _ := l4.GetTransferSchedule("sched-1"); got.RunSeq != 1 {
		t.Fatalf("run_seq after one firing = %d, want 1", got.RunSeq)
	}
	if _, err := l4.AmendTransferSchedulePayer("sched-1", "carol"); !errors.Is(err, ErrScheduleBadTransition) {
		t.Errorf("fired schedule payer: err = %v, want ErrScheduleBadTransition", err)
	}
	if _, err := l4.AmendTransferSchedulePayee("sched-1", "dave"); !errors.Is(err, ErrScheduleBadTransition) {
		t.Errorf("fired schedule payee: err = %v, want ErrScheduleBadTransition", err)
	}

	// Empty parties and same-account parties reuse the creation
	// gate, and every rejected amendment leaves the plan exactly
	// as it was.
	if _, err := l.AmendTransferSchedulePayer("sched-1", ""); !errors.Is(err, ErrScheduleEmptyAccount) {
		t.Errorf("empty payer: err = %v, want ErrScheduleEmptyAccount", err)
	}
	if _, err := l.AmendTransferSchedulePayee("sched-1", ""); !errors.Is(err, ErrScheduleEmptyAccount) {
		t.Errorf("empty payee: err = %v, want ErrScheduleEmptyAccount", err)
	}
	// Cross-leg: the new payer must not be the live payee, and the
	// new payee must not be the live payer.
	if _, err := l.AmendTransferSchedulePayer("sched-1", "bob"); !errors.Is(err, ErrScheduleSameAccount) {
		t.Errorf("payer == live payee: err = %v, want ErrScheduleSameAccount", err)
	}
	if _, err := l.AmendTransferSchedulePayee("sched-1", "alice"); !errors.Is(err, ErrScheduleSameAccount) {
		t.Errorf("payee == live payer: err = %v, want ErrScheduleSameAccount", err)
	}
	if got, err := l.GetTransferSchedule("sched-1"); err != nil || !reflect.DeepEqual(got, orig) {
		t.Errorf("schedule after rejected amendments = %+v, %v, want %+v", got, err, orig)
	}

	// Amending to the current party is a no-op, not a change.
	if got, err := l.AmendTransferSchedulePayer("sched-1", "alice"); err != nil || !reflect.DeepEqual(got, orig) {
		t.Errorf("same-payer amendment = %+v, %v, want %+v, nil", got, err, orig)
	}
	if got, err := l.AmendTransferSchedulePayee("sched-1", "bob"); err != nil || !reflect.DeepEqual(got, orig) {
		t.Errorf("same-payee amendment = %+v, %v, want %+v, nil", got, err, orig)
	}

	// Active plan: only From moves.
	amended, err := l.AmendTransferSchedulePayer("sched-1", "carol")
	if err != nil {
		t.Fatalf("AmendTransferSchedulePayer active: %v", err)
	}
	if amended.From != "carol" {
		t.Errorf("amended payer = %q, want carol", amended.From)
	}
	amended.From = orig.From
	if !reflect.DeepEqual(amended, orig) {
		t.Errorf("amended schedule = %+v, want %+v except payer", amended, orig)
	}

	// Active plan: only To moves (the payer is already carol from
	// the amendment above, so compare against the live row).
	current, _ := l.GetTransferSchedule("sched-1")
	amended, err = l.AmendTransferSchedulePayee("sched-1", "dave")
	if err != nil {
		t.Fatalf("AmendTransferSchedulePayee active: %v", err)
	}
	if amended.To != "dave" {
		t.Errorf("amended payee = %q, want dave", amended.To)
	}
	amended.To = current.To
	if !reflect.DeepEqual(amended, current) {
		t.Errorf("amended schedule = %+v, want %+v except payee", amended, current)
	}

	// Paused plan: pausing is a hold, not a terminal state, so the
	// revision waits with the plan and governs the first run after
	// resume — and again only the amended leg moves.
	l5 := New()
	orig5 := createPartiesSchedule(t, l5, "sched-1", now)
	if _, err := l5.PauseTransferSchedule("sched-1"); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	paused, err := l5.AmendTransferSchedulePayer("sched-1", "carol")
	if err != nil {
		t.Fatalf("AmendTransferSchedulePayer paused: %v", err)
	}
	if paused.From != "carol" || paused.Status != SchedulePaused {
		t.Errorf("amended paused = payer %q status %q, want carol paused", paused.From, paused.Status)
	}
	paused.From = orig5.From
	paused.Status = orig5.Status
	if !reflect.DeepEqual(paused, orig5) {
		t.Errorf("amended paused schedule = %+v, want %+v except payer", paused, orig5)
	}
	pausedPayee, err := l5.AmendTransferSchedulePayee("sched-1", "dave")
	if err != nil {
		t.Fatalf("AmendTransferSchedulePayee paused: %v", err)
	}
	if pausedPayee.To != "dave" || pausedPayee.Status != SchedulePaused {
		t.Errorf("amended paused = payee %q status %q, want dave paused", pausedPayee.To, pausedPayee.Status)
	}
}

func TestAmendTransferSchedulePayerNextFire(t *testing.T) {
	// The next successful firing debits the new payer: the sweep
	// snapshot is taken at fire time, so no firing-path change is
	// needed, and the already-fired lineage is untouched.
	l := New()
	now := time.Now().UTC().Truncate(time.Second)
	fundAccount(t, l, "carol", 100000, "USD")
	createPartiesSchedule(t, l, "sched-1", now.Add(-time.Minute))
	if _, err := l.AmendTransferSchedulePayer("sched-1", "carol"); err != nil {
		t.Fatalf("AmendTransferSchedulePayer: %v", err)
	}
	runs := l.SweepDue(now)
	if len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("runs = %+v, want one fired run", runs)
	}
	if got := l.BalanceIn("carol", "USD"); got != 99000 {
		t.Errorf("carol = %d, want 99000 (new payer debited 1000)", got)
	}
	if got := l.BalanceIn("bob", "USD"); got != 1000 {
		t.Errorf("bob = %d, want 1000", got)
	}
	if got, _ := l.GetTransferSchedule("sched-1"); got.RunSeq != 1 {
		t.Errorf("run_seq = %d, want 1", got.RunSeq)
	}
}

func TestAmendTransferSchedulePayeeNextFire(t *testing.T) {
	// The next successful firing credits the new payee.
	l := New()
	now := time.Now().UTC().Truncate(time.Second)
	fundAccount(t, l, "alice", 100000, "USD")
	createPartiesSchedule(t, l, "sched-1", now.Add(-time.Minute))
	if _, err := l.AmendTransferSchedulePayee("sched-1", "dave"); err != nil {
		t.Fatalf("AmendTransferSchedulePayee: %v", err)
	}
	runs := l.SweepDue(now)
	if len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("runs = %+v, want one fired run", runs)
	}
	if got := l.BalanceIn("dave", "USD"); got != 1000 {
		t.Errorf("dave = %d, want 1000 (new payee credited)", got)
	}
	if got := l.BalanceIn("bob", "USD"); got != 0 {
		t.Errorf("bob = %d, want 0 (old payee untouched)", got)
	}
}

func TestAmendTransferSchedulePartiesFailedRunRetry(t *testing.T) {
	// A failed run posts nothing, so its retry reads the amended
	// legs and books exactly once — no double-posting risk, and the
	// same "<ID>/run/<seq>" key is reused.
	l := New()
	now := time.Now().UTC()
	fundAccount(t, l, "carol", 100000, "USD")
	createPartiesSchedule(t, l, "sched-1", now.Add(-time.Minute))
	l.Freeze("alice")
	runs := l.SweepDue(now)
	if len(runs) != 1 || runs[0].Status != "failed" {
		t.Fatalf("runs = %+v, want one failed run", runs)
	}
	if _, err := l.AmendTransferSchedulePayer("sched-1", "carol"); err != nil {
		t.Fatalf("AmendTransferSchedulePayer after failure: %v", err)
	}
	l.Unfreeze("alice")
	runs = l.SweepDue(now)
	if len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("retry runs = %+v, want one fired run", runs)
	}
	if runs[0].RunSeq != 1 {
		t.Errorf("retried run_seq = %d, want 1 (same run retried)", runs[0].RunSeq)
	}
	if got := l.BalanceIn("carol", "USD"); got != 99000 {
		t.Errorf("carol = %d, want 99000 (amended payer, booked exactly once)", got)
	}
	if got := l.BalanceIn("bob", "USD"); got != 1000 {
		t.Errorf("bob = %d, want 1000", got)
	}
}

func TestAmendTransferSchedulePartiesAuditAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	now := time.Now().UTC().Truncate(time.Second)
	before := createPartiesSchedule(t, l, "sched-1", now)
	// The no-op revisions emit nothing.
	if _, err := l.AmendTransferSchedulePayer("sched-1", "alice"); err != nil {
		t.Fatalf("no-op payer amend: %v", err)
	}
	if _, err := l.AmendTransferSchedulePayee("sched-1", "bob"); err != nil {
		t.Fatalf("no-op payee amend: %v", err)
	}
	ctx := ContextWithActor(context.Background(), "ops-desk")
	if _, err := l.AmendTransferSchedulePayerCtx(ctx, "sched-1", "carol"); err != nil {
		t.Fatalf("AmendTransferSchedulePayerCtx: %v", err)
	}
	if _, err := l.AmendTransferSchedulePayeeCtx(ctx, "sched-1", "dave"); err != nil {
		t.Fatalf("AmendTransferSchedulePayeeCtx: %v", err)
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
	if after.From != "carol" || after.To != "dave" {
		t.Errorf("restored legs = %q -> %q, want carol -> dave", after.From, after.To)
	}
	after.From, after.To = before.From, before.To
	if !reflect.DeepEqual(after, before) {
		t.Errorf("restored schedule = %+v, want %+v except legs", after, before)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	byOp := auditEventsByOp(t, dir)
	payerEvents := byOp["transfer_schedule_payer_amended"]
	if len(payerEvents) != 1 {
		t.Fatalf("payer amend events = %d, want 1 (no-op must not audit)", len(payerEvents))
	}
	if payerEvents[0].Actor != "ops-desk" {
		t.Errorf("payer event actor = %q, want ops-desk", payerEvents[0].Actor)
	}
	d := payerEvents[0].Details
	if d["schedule_id"] != "sched-1" ||
		d["previous_payer"] != "alice" ||
		d["payer"] != "carol" ||
		d["currency"] != "USD" ||
		d["run_seq"] != float64(0) {
		t.Errorf("payer event details = %+v, want sched-1 alice->carol USD run_seq 0", d)
	}
	payeeEvents := byOp["transfer_schedule_payee_amended"]
	if len(payeeEvents) != 1 {
		t.Fatalf("payee amend events = %d, want 1 (no-op must not audit)", len(payeeEvents))
	}
	d = payeeEvents[0].Details
	if d["schedule_id"] != "sched-1" ||
		d["previous_payee"] != "bob" ||
		d["payee"] != "dave" ||
		d["currency"] != "USD" ||
		d["run_seq"] != float64(0) {
		t.Errorf("payee event details = %+v, want sched-1 bob->dave USD run_seq 0", d)
	}
}
