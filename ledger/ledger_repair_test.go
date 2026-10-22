package ledger

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// repairTestInstruction builds a payout instruction that fails STP:
// the destination is missing, so CreatePayout would reject it with
// ErrEmptyPayoutDestination and the instruction belongs in the repair
// queue instead.
func repairTestInstruction(id string) PayoutInstruction {
	return PayoutInstruction{
		ID:             id,
		PayeeAccount:   "saver",
		AmountCents:    50_000,
		Destination:    "", // missing — the STP failure
		Maker:          "alice",
		IdempotencyKey: "idem-" + id,
	}
}

// backdateRepairRecord moves a repair record's last-activity timestamp
// back by age, simulating a record the operator ignored past its TTL.
func backdateRepairRecord(t *testing.T, l *Ledger, id string, age time.Duration) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	repairQueues.Lock()
	defer repairQueues.Unlock()
	rec, ok := repairQueueForLocked(l).records[id]
	if !ok {
		t.Fatalf("repair record %q not found", id)
	}
	rec.UpdatedAt = time.Now().Add(-age)
}

func TestEnqueueRepair_HappyPath(t *testing.T) {
	l := New()
	for i, code := range []string{
		RepairReasonMissingField,
		RepairReasonInvalidFormat,
		RepairReasonUnknownRoute,
	} {
		rec, err := l.EnqueueRepair(repairTestInstruction("po-rq"), code, "ops")
		if err != nil {
			t.Fatalf("EnqueueRepair(%s): %v", code, err)
		}
		wantID := fmt.Sprintf("repair-%d", i+1)
		if rec.ID != wantID {
			t.Errorf("ID = %q, want %q", rec.ID, wantID)
		}
		if rec.Status != RepairStatusOpen {
			t.Errorf("status = %q, want open", rec.Status)
		}
		if rec.RepairCount != 0 {
			t.Errorf("repair count = %d, want 0", rec.RepairCount)
		}
		if rec.ReasonCode != code {
			t.Errorf("reason = %q, want %q", rec.ReasonCode, code)
		}
		if rec.Instruction.ID != "po-rq" || rec.Instruction.Maker != "alice" {
			t.Errorf("snapshot = %+v, want the submitted instruction", rec.Instruction)
		}
		if rec.EnqueuedAt.IsZero() || rec.UpdatedAt.IsZero() {
			t.Error("enqueued/updated timestamps not set")
		}
		if rec.TTL != defaultRepairTTL {
			t.Errorf("TTL = %v, want %v", rec.TTL, defaultRepairTTL)
		}
	}
}

func TestEnqueueRepair_UnknownReasonCode(t *testing.T) {
	l := New()
	if _, err := l.EnqueueRepair(repairTestInstruction("po-rq"), "typo_code", "ops"); !errors.Is(err, ErrRepairUnknownReasonCode) {
		t.Fatalf("err = %v, want ErrRepairUnknownReasonCode", err)
	}
}

func TestEnqueueRepair_EmptyActor(t *testing.T) {
	l := New()
	if _, err := l.EnqueueRepair(repairTestInstruction("po-rq"), RepairReasonMissingField, ""); !errors.Is(err, ErrRepairEmptyActor) {
		t.Fatalf("err = %v, want ErrRepairEmptyActor", err)
	}
}

func TestEnqueueRepair_SnapshotImmutability(t *testing.T) {
	l := New()
	inst := repairTestInstruction("po-rq")
	rec, err := l.EnqueueRepair(inst, RepairReasonMissingField, "ops")
	if err != nil {
		t.Fatal(err)
	}
	// Mutate the caller's struct after enqueue: the record must not
	// follow.
	inst.Destination = "FPS:mutated"
	inst.AmountCents = 1
	got, ok := l.GetRepair(rec.ID)
	if !ok {
		t.Fatal("record vanished")
	}
	if got.Instruction.Destination != "" || got.Instruction.AmountCents != 50_000 {
		t.Errorf("snapshot mutated: %+v", got.Instruction)
	}
	// Mutating the returned record must not touch the queue either.
	got.Instruction.Destination = "FPS:mutated-again"
	again, _ := l.GetRepair(rec.ID)
	if again.Instruction.Destination != "" {
		t.Errorf("queue record mutated through returned copy: %+v", again.Instruction)
	}
}

func TestRepairAndResubmit_Success(t *testing.T) {
	l := seedPayoutLedger(t)
	rec, err := l.EnqueueRepair(repairTestInstruction("po-repair-1"), RepairReasonMissingField, "ops")
	if err != nil {
		t.Fatal(err)
	}
	p, err := l.RepairAndResubmit(rec.ID, map[string]string{
		"destination": "FPS:6222000012345678",
	}, "ops")
	if err != nil {
		t.Fatalf("RepairAndResubmit: %v", err)
	}
	// The payout was actually created, with the original instruction's
	// ID and idempotency key — an idempotent resubmission.
	if p.ID != "po-repair-1" {
		t.Errorf("payout ID = %q, want po-repair-1", p.ID)
	}
	if p.IdempotencyKey != "idem-po-repair-1" {
		t.Errorf("idempotency key = %q, want the original", p.IdempotencyKey)
	}
	if p.Status != PayoutDraft {
		t.Errorf("payout status = %q, want draft (resubmit only re-runs the STP gate)", p.Status)
	}
	stored, ok := l.GetPayout("po-repair-1")
	if !ok || stored.Destination != "FPS:6222000012345678" {
		t.Errorf("payout not stored with the fixed destination: %+v", stored)
	}
	got, ok := l.GetRepair(rec.ID)
	if !ok {
		t.Fatal("record vanished")
	}
	if got.Status != RepairStatusResubmitted {
		t.Errorf("record status = %q, want resubmitted", got.Status)
	}
	if got.PayoutID != "po-repair-1" {
		t.Errorf("record payout ID = %q, want po-repair-1", got.PayoutID)
	}
	if got.RepairCount != 1 {
		t.Errorf("repair count = %d, want 1 (one attempt)", got.RepairCount)
	}
}

func TestRepairAndResubmit_FailureReenqueues(t *testing.T) {
	l := seedPayoutLedger(t)
	rec, err := l.EnqueueRepair(repairTestInstruction("po-repair-2"), RepairReasonMissingField, "ops")
	if err != nil {
		t.Fatal(err)
	}
	// Resubmit without fixing anything: the STP gate rejects again.
	if _, err := l.RepairAndResubmit(rec.ID, map[string]string{}, "ops"); !errors.Is(err, ErrEmptyPayoutDestination) {
		t.Fatalf("err = %v, want the CreatePayout rejection", err)
	}
	got, _ := l.GetRepair(rec.ID)
	if got.Status != RepairStatusOpen {
		t.Errorf("status = %q, want open (re-enqueued)", got.Status)
	}
	if got.RepairCount != 1 {
		t.Errorf("repair count = %d, want 1", got.RepairCount)
	}
	if got.ReasonCode != RepairReasonMissingField {
		t.Errorf("reason = %q, want missing_field re-derived", got.ReasonCode)
	}
	// A partial fix is retained on the record for the next attempt.
	if _, err := l.RepairAndResubmit(rec.ID, map[string]string{"maker": "bob"}, "ops"); !errors.Is(err, ErrEmptyPayoutDestination) {
		t.Fatalf("second attempt err = %v, want ErrEmptyPayoutDestination", err)
	}
	got, _ = l.GetRepair(rec.ID)
	if got.Instruction.Maker != "bob" {
		t.Errorf("operator fix not retained: maker = %q", got.Instruction.Maker)
	}
	if got.RepairCount != 2 {
		t.Errorf("repair count = %d, want 2", got.RepairCount)
	}
	// Now complete the repair: it succeeds on the third attempt.
	if _, err := l.RepairAndResubmit(rec.ID, map[string]string{"destination": "FPS:6222000012345678"}, "ops"); err != nil {
		t.Fatalf("final resubmit: %v", err)
	}
	got, _ = l.GetRepair(rec.ID)
	if got.Status != RepairStatusResubmitted || got.RepairCount != 3 {
		t.Errorf("after success: status = %q count = %d, want resubmitted/3", got.Status, got.RepairCount)
	}
	if p, ok := l.GetPayout("po-repair-2"); !ok || p.Maker != "bob" {
		t.Errorf("payout missing or lost the retained fix: %+v", p)
	}
}

func TestRepairAndResubmit_UnknownFieldFailClosed(t *testing.T) {
	l := seedPayoutLedger(t)
	rec, err := l.EnqueueRepair(repairTestInstruction("po-repair-3"), RepairReasonMissingField, "ops")
	if err != nil {
		t.Fatal(err)
	}
	_, err = l.RepairAndResubmit(rec.ID, map[string]string{
		"destination": "FPS:6222000012345678",
		"memo":        "sneaky", // not an admitted field
	}, "ops")
	if !errors.Is(err, ErrRepairUnknownField) {
		t.Fatalf("err = %v, want ErrRepairUnknownField", err)
	}
	// Fail-closed: the record is untouched — no attempt counted, no
	// partial fix applied.
	got, _ := l.GetRepair(rec.ID)
	if got.RepairCount != 0 || got.Instruction.Destination != "" || got.Status != RepairStatusOpen {
		t.Errorf("record changed on rejected resubmit: %+v", got)
	}
	if _, ok := l.GetPayout("po-repair-3"); ok {
		t.Error("payout created despite the rejected resubmit")
	}
}

func TestRepairAndResubmit_InvalidAmount(t *testing.T) {
	l := seedPayoutLedger(t)
	rec, err := l.EnqueueRepair(repairTestInstruction("po-repair-4"), RepairReasonMissingField, "ops")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.RepairAndResubmit(rec.ID, map[string]string{"amount_cents": "not-a-number"}, "ops"); !errors.Is(err, ErrRepairInvalidAmount) {
		t.Fatalf("err = %v, want ErrRepairInvalidAmount", err)
	}
}

func TestRepairAndResubmit_NotFound(t *testing.T) {
	l := seedPayoutLedger(t)
	if _, err := l.RepairAndResubmit("repair-999", map[string]string{}, "ops"); !errors.Is(err, ErrRepairNotFound) {
		t.Fatalf("err = %v, want ErrRepairNotFound", err)
	}
}

func TestRepairTerminalReadOnly(t *testing.T) {
	l := seedPayoutLedger(t)
	rec, err := l.EnqueueRepair(repairTestInstruction("po-repair-5"), RepairReasonMissingField, "ops")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.RepairAndResubmit(rec.ID, map[string]string{"destination": "FPS:6222000012345678"}, "ops"); err != nil {
		t.Fatal(err)
	}
	// A resubmitted record is terminal: no second resubmit.
	if _, err := l.RepairAndResubmit(rec.ID, map[string]string{"maker": "mallory"}, "ops"); !errors.Is(err, ErrRepairTerminal) {
		t.Fatalf("err = %v, want ErrRepairTerminal", err)
	}
}

func TestEnqueueRepair_QueueFull(t *testing.T) {
	l := New(WithRepairQueueCap(2))
	for i := 0; i < 2; i++ {
		if _, err := l.EnqueueRepair(repairTestInstruction("po-full"), RepairReasonMissingField, "ops"); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	// Fail-closed: no silent eviction, a plain error.
	if _, err := l.EnqueueRepair(repairTestInstruction("po-full"), RepairReasonMissingField, "ops"); !errors.Is(err, ErrRepairQueueFull) {
		t.Fatalf("err = %v, want ErrRepairQueueFull", err)
	}
}

func TestSweepExpiredRepairs(t *testing.T) {
	l := New()
	fresh, err := l.EnqueueRepair(repairTestInstruction("po-ttl-1"), RepairReasonMissingField, "ops")
	if err != nil {
		t.Fatal(err)
	}
	stale, err := l.EnqueueRepair(repairTestInstruction("po-ttl-2"), RepairReasonInvalidFormat, "ops")
	if err != nil {
		t.Fatal(err)
	}
	backdateRepairRecord(t, l, stale.ID, 8*24*time.Hour)

	if n := l.SweepExpiredRepairs(time.Now()); n != 1 {
		t.Fatalf("swept = %d, want 1", n)
	}
	got, _ := l.GetRepair(stale.ID)
	if got.Status != RepairStatusExpired {
		t.Errorf("stale status = %q, want expired", got.Status)
	}
	if got, _ := l.GetRepair(fresh.ID); got.Status != RepairStatusOpen {
		t.Errorf("fresh status = %q, want open", got.Status)
	}
	// Terminal records are read-only: no resubmit after expiry.
	if _, err := l.RepairAndResubmit(stale.ID, map[string]string{"destination": "FPS:1"}, "ops"); !errors.Is(err, ErrRepairTerminal) {
		t.Fatalf("err = %v, want ErrRepairTerminal", err)
	}
}

func TestRepairAndResubmit_LazyExpiry(t *testing.T) {
	l := seedPayoutLedger(t)
	rec, err := l.EnqueueRepair(repairTestInstruction("po-ttl-3"), RepairReasonMissingField, "ops")
	if err != nil {
		t.Fatal(err)
	}
	// No sweep ran, but the resubmit still expires the record lazily.
	backdateRepairRecord(t, l, rec.ID, 8*24*time.Hour)
	if _, err := l.RepairAndResubmit(rec.ID, map[string]string{"destination": "FPS:6222000012345678"}, "ops"); !errors.Is(err, ErrRepairTerminal) {
		t.Fatalf("err = %v, want ErrRepairTerminal", err)
	}
	got, _ := l.GetRepair(rec.ID)
	if got.Status != RepairStatusExpired {
		t.Errorf("status = %q, want expired", got.Status)
	}
	if _, ok := l.GetPayout("po-ttl-3"); ok {
		t.Error("payout created from an expired record")
	}
}

func TestRepairAuditTrail(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	l := New(WithAuditLog(al), WithPayoutClearingAccount("payout-clearing"))
	seedBalance(t, l, "saver", 1_000_000)

	rec, err := l.EnqueueRepair(repairTestInstruction("po-audit-1"), RepairReasonMissingField, "ops")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.RepairAndResubmit(rec.ID, map[string]string{"destination": "FPS:6222000012345678"}, "ops"); err != nil {
		t.Fatal(err)
	}
	if err := al.Close(); err != nil {
		t.Fatal(err)
	}
	events, corrupt, err := ReadAuditLog(dir, time.Now().UTC().Format("2006-01-02"))
	if err != nil || corrupt != 0 {
		t.Fatalf("ReadAuditLog: %v corrupt=%d", err, corrupt)
	}
	var enqueued, resubmitted bool
	for _, e := range events {
		switch e.Op {
		case "repair_enqueued":
			enqueued = true
			if e.TraceID != rec.ID {
				t.Errorf("enqueued trace = %q, want %q", e.TraceID, rec.ID)
			}
			if e.Details["reason_code"] != RepairReasonMissingField {
				t.Errorf("enqueued reason = %v", e.Details["reason_code"])
			}
			if _, ok := e.Details["instruction"]; !ok {
				t.Error("enqueued event missing the before snapshot")
			}
		case "repair_resubmitted":
			resubmitted = true
			if e.Details["success"] != true {
				t.Errorf("resubmitted success = %v, want true", e.Details["success"])
			}
			if e.Details["payout_id"] != "po-audit-1" {
				t.Errorf("resubmitted payout_id = %v", e.Details["payout_id"])
			}
			if _, ok := e.Details["before"]; !ok {
				t.Error("resubmitted event missing before state")
			}
			if _, ok := e.Details["after"]; !ok {
				t.Error("resubmitted event missing after state")
			}
		}
	}
	if !enqueued {
		t.Error("no repair_enqueued audit event")
	}
	if !resubmitted {
		t.Error("no repair_resubmitted audit event")
	}
}
