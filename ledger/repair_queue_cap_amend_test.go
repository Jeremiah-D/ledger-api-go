package ledger

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strconv"
	"testing"
	"time"
)

// TestSetRepairQueueCapExpandAdmitsBeyondOldCap pins the live
// expansion: a queue full at the old cap admits new records as soon
// as the cap is raised, and the getter follows the live value.
func TestSetRepairQueueCapExpandAdmitsBeyondOldCap(t *testing.T) {
	l := New(WithRepairQueueCap(2))
	for _, id := range []string{"po-cap-1", "po-cap-2"} {
		if _, err := l.EnqueueRepair(repairTestInstruction(id), RepairReasonMissingField, "ops"); err != nil {
			t.Fatalf("EnqueueRepair(%s): %v", id, err)
		}
	}
	if _, err := l.EnqueueRepair(repairTestInstruction("po-cap-3"), RepairReasonMissingField, "ops"); !errors.Is(err, ErrRepairQueueFull) {
		t.Fatalf("enqueue over cap 2 = %v, want ErrRepairQueueFull", err)
	}
	if err := l.SetRepairQueueCap(3); err != nil {
		t.Fatalf("SetRepairQueueCap(3): %v", err)
	}
	if got := l.RepairQueueCap(); got != 3 {
		t.Fatalf("RepairQueueCap() = %d, want 3", got)
	}
	if _, err := l.EnqueueRepair(repairTestInstruction("po-cap-3"), RepairReasonMissingField, "ops"); err != nil {
		t.Fatalf("enqueue after expansion: %v", err)
	}
}

// TestSetRepairQueueCapShrinkRejectsNewEnqueuesAndKeepsParked pins
// the fail-closed shrink: shrinking below the open count rejects new
// enqueues with ErrRepairQueueFull, and every parked record stays
// open and retrievable.
func TestSetRepairQueueCapShrinkRejectsNewEnqueuesAndKeepsParked(t *testing.T) {
	l := New(WithRepairQueueCap(3))
	ids := []string{"po-shr-1", "po-shr-2", "po-shr-3"}
	for _, id := range ids {
		if _, err := l.EnqueueRepair(repairTestInstruction(id), RepairReasonMissingField, "ops"); err != nil {
			t.Fatalf("EnqueueRepair(%s): %v", id, err)
		}
	}
	if err := l.SetRepairQueueCap(2); err != nil {
		t.Fatalf("SetRepairQueueCap(2): %v", err)
	}
	if _, err := l.EnqueueRepair(repairTestInstruction("po-shr-4"), RepairReasonMissingField, "ops"); !errors.Is(err, ErrRepairQueueFull) {
		t.Fatalf("enqueue after shrink below open count = %v, want ErrRepairQueueFull", err)
	}
	for i, id := range ids {
		rec, ok := l.GetRepair("repair-" + strconv.Itoa(i+1))
		if !ok {
			t.Fatalf("GetRepair(%s) missing after shrink", id)
		}
		if rec.Status != RepairStatusOpen {
			t.Errorf("record %s status = %q, want open", id, rec.Status)
		}
		if rec.Instruction.ID != id {
			t.Errorf("record %s instruction = %q, want %q", id, rec.Instruction.ID, id)
		}
	}
}

// TestSetRepairQueueCapShrinkKeepsRecordContents pins that a shrink
// moves only the cap: the parked records are byte-identical before
// and after (instruction snapshot, reason, TTL, timestamps).
func TestSetRepairQueueCapShrinkKeepsRecordContents(t *testing.T) {
	l := New(WithRepairQueueCap(3))
	if _, err := l.EnqueueRepair(repairTestInstruction("po-cnt-1"), RepairReasonInvalidFormat, "ops"); err != nil {
		t.Fatalf("EnqueueRepair: %v", err)
	}
	before, ok := l.GetRepair("repair-1")
	if !ok {
		t.Fatal("record repair-1 missing before shrink")
	}
	if err := l.SetRepairQueueCap(1); err != nil {
		t.Fatalf("SetRepairQueueCap(1): %v", err)
	}
	after, ok := l.GetRepair("repair-1")
	if !ok {
		t.Fatal("record repair-1 missing after shrink")
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("shrink moved the record:\nbefore: %+v\nafter:  %+v", before, after)
	}
}

// TestSetRepairQueueCapSameValueNoop pins the no-op: revising to the
// current capacity returns nil and writes no audit event.
func TestSetRepairQueueCapSameValueNoop(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithRepairQueueCap(8))
	if err := l.SetRepairQueueCap(8); err != nil {
		t.Fatalf("SetRepairQueueCap(8) same value: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := len(auditEventsByOp(t, dir)["repair_queue_cap_amended"]); n != 0 {
		t.Fatalf("same-value revision wrote %d amendment events, want 0", n)
	}
}

// TestSetRepairQueueCapInvalidRejected pins the fail-closed gate: a
// non-positive capacity is rejected and the live capacity is left
// untouched, with no amendment event in the trail.
func TestSetRepairQueueCapInvalidRejected(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithRepairQueueCap(8))
	for _, n := range []int{0, -1, -100} {
		if err := l.SetRepairQueueCap(n); !errors.Is(err, ErrInvalidRepairQueueCap) {
			t.Fatalf("SetRepairQueueCap(%d) = %v, want ErrInvalidRepairQueueCap", n, err)
		}
	}
	if got := l.RepairQueueCap(); got != 8 {
		t.Fatalf("RepairQueueCap() after rejected revisions = %d, want 8", got)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := len(auditEventsByOp(t, dir)["repair_queue_cap_amended"]); n != 0 {
		t.Fatalf("rejected revisions wrote %d amendment events, want 0", n)
	}
}

// TestSetRepairQueueCapCtxActorAttribution pins LG-73 attribution on
// the Ctx variant: the operator identity replaces the entrypoint
// name, while the plain setter keeps it. The amendment details carry
// the previous and new capacities.
func TestSetRepairQueueCapCtxActorAttribution(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithRepairQueueCap(8))
	ctx := ContextWithActor(context.Background(), "ops-cap")
	if err := l.SetRepairQueueCapCtx(ctx, 32); err != nil {
		t.Fatalf("SetRepairQueueCapCtx: %v", err)
	}
	if err := l.SetRepairQueueCap(64); err != nil {
		t.Fatalf("SetRepairQueueCap: %v", err)
	}
	waitAuditWritten(t, al, 2)
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	amended := auditEventsByOp(t, dir)["repair_queue_cap_amended"]
	if len(amended) != 2 {
		t.Fatalf("amendment events = %d, want 2", len(amended))
	}
	if amended[0].Actor != "ops-cap" {
		t.Errorf("Ctx amendment actor = %q, want ops-cap", amended[0].Actor)
	}
	if amended[0].Details["previous_cap"] != float64(8) {
		t.Errorf("Ctx amendment previous_cap = %v, want 8", amended[0].Details["previous_cap"])
	}
	if amended[0].Details["cap"] != float64(32) {
		t.Errorf("Ctx amendment cap = %v, want 32", amended[0].Details["cap"])
	}
	if amended[1].Actor != "SetRepairQueueCap" {
		t.Errorf("plain amendment actor = %q, want SetRepairQueueCap", amended[1].Actor)
	}
	if amended[1].Details["previous_cap"] != float64(32) {
		t.Errorf("plain amendment previous_cap = %v, want 32", amended[1].Details["previous_cap"])
	}
	if amended[1].Details["cap"] != float64(64) {
		t.Errorf("plain amendment cap = %v, want 64", amended[1].Details["cap"])
	}
}

// TestRepairQueueCapGetterFollowsLiveValue pins the live getter: the
// default capacity is 1000, the construction-time
// WithRepairQueueCap value is reported, and a runtime revision moves
// the getter.
func TestRepairQueueCapGetterFollowsLiveValue(t *testing.T) {
	if got := New().RepairQueueCap(); got != defaultRepairQueueCap {
		t.Fatalf("default RepairQueueCap() = %d, want %d", got, defaultRepairQueueCap)
	}
	l := New(WithRepairQueueCap(2))
	if got := l.RepairQueueCap(); got != 2 {
		t.Fatalf("RepairQueueCap() = %d, want 2", got)
	}
	if err := l.SetRepairQueueCap(5); err != nil {
		t.Fatalf("SetRepairQueueCap(5): %v", err)
	}
	if got := l.RepairQueueCap(); got != 5 {
		t.Fatalf("RepairQueueCap() after revision = %d, want 5", got)
	}
}

// TestSetRepairQueueCapDrainedRecordsStillOccupySlots pins the
// append-only queue map: expiring every open record does NOT free
// admission slots — terminal records stay in q.records (repair.go
// has no delete path), so a shrink below the record count keeps new
// enqueues failing until the cap is raised again. This is the honest
// form of the backlog's "drain then re-admit" case: the drain is the
// lifecycle (open -> expired), and the recovery is re-expansion, not
// the expiry itself.
func TestSetRepairQueueCapDrainedRecordsStillOccupySlots(t *testing.T) {
	l := New(WithRepairQueueCap(3))
	for _, id := range []string{"po-drn-1", "po-drn-2", "po-drn-3"} {
		if _, err := l.EnqueueRepair(repairTestInstruction(id), RepairReasonMissingField, "ops"); err != nil {
			t.Fatalf("EnqueueRepair(%s): %v", id, err)
		}
	}
	if err := l.SetRepairQueueCap(2); err != nil {
		t.Fatalf("SetRepairQueueCap(2): %v", err)
	}
	// Drain the lifecycle: every open record expires.
	if marked := l.SweepExpiredRepairs(time.Now().Add(8 * 24 * time.Hour)); marked != 3 {
		t.Fatalf("SweepExpiredRepairs marked %d, want 3", marked)
	}
	for i := 1; i <= 3; i++ {
		rec, ok := l.GetRepair("repair-" + strconv.Itoa(i))
		if !ok || rec.Status != RepairStatusExpired {
			t.Fatalf("record repair-%d = %+v, %v; want expired", i, rec, ok)
		}
	}
	// Expired records still occupy slots: admission stays closed.
	if _, err := l.EnqueueRepair(repairTestInstruction("po-drn-4"), RepairReasonMissingField, "ops"); !errors.Is(err, ErrRepairQueueFull) {
		t.Fatalf("enqueue after drain = %v, want ErrRepairQueueFull (terminal records occupy slots)", err)
	}
	// Recovery is re-expansion.
	if err := l.SetRepairQueueCap(4); err != nil {
		t.Fatalf("SetRepairQueueCap(4): %v", err)
	}
	if _, err := l.EnqueueRepair(repairTestInstruction("po-drn-4"), RepairReasonMissingField, "ops"); err != nil {
		t.Fatalf("enqueue after re-expansion: %v", err)
	}
}

// TestSetRepairQueueCapSnapshotRestoresDefault pins the disaster-
// recovery guard: snapshots carry no repair queue, so a restored
// ledger reports the default cap (1000) — the amendment must be
// re-applied — and the parked records are not in the snapshot.
func TestSetRepairQueueCapSnapshotRestoresDefault(t *testing.T) {
	src := New(WithRepairQueueCap(2))
	rec, err := src.EnqueueRepair(repairTestInstruction("po-snap-1"), RepairReasonMissingField, "ops")
	if err != nil {
		t.Fatalf("EnqueueRepair: %v", err)
	}
	if err := src.SetRepairQueueCap(5); err != nil {
		t.Fatalf("SetRepairQueueCap(5): %v", err)
	}
	var buf bytes.Buffer
	if err := src.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if got := restored.RepairQueueCap(); got != defaultRepairQueueCap {
		t.Fatalf("restored RepairQueueCap() = %d, want default %d", got, defaultRepairQueueCap)
	}
	if _, ok := restored.GetRepair(rec.ID); ok {
		t.Fatalf("restored ledger still holds repair record %s", rec.ID)
	}
}
