package ledger

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

// TestSetRepairTTLNewRecordsStampedWithNewTTL pins the live
// revision: a record enqueued after the TTL is revised carries the
// new TTL, and the getter follows the live value.
func TestSetRepairTTLNewRecordsStampedWithNewTTL(t *testing.T) {
	l := New()
	if err := l.SetRepairTTL(24 * time.Hour); err != nil {
		t.Fatalf("SetRepairTTL(24h): %v", err)
	}
	if got := l.RepairTTL(); got != 24*time.Hour {
		t.Fatalf("RepairTTL() = %v, want 24h", got)
	}
	rec, err := l.EnqueueRepair(repairTestInstruction("po-ttl-1"), RepairReasonMissingField, "ops")
	if err != nil {
		t.Fatalf("EnqueueRepair: %v", err)
	}
	if rec.TTL != 24*time.Hour {
		t.Fatalf("enqueued record TTL = %v, want 24h", rec.TTL)
	}
	stored, ok := l.GetRepair(rec.ID)
	if !ok {
		t.Fatalf("GetRepair(%s) missing", rec.ID)
	}
	if stored.TTL != 24*time.Hour {
		t.Fatalf("stored record TTL = %v, want 24h", stored.TTL)
	}
}

// TestSetRepairTTLPreRevisionRecordKeepsOldTTL pins future-only
// semantics: a record enqueued before the revision keeps the TTL it
// was stamped with, while a record enqueued after carries the new
// one.
func TestSetRepairTTLPreRevisionRecordKeepsOldTTL(t *testing.T) {
	l := New()
	before, err := l.EnqueueRepair(repairTestInstruction("po-ttl-old"), RepairReasonMissingField, "ops")
	if err != nil {
		t.Fatalf("EnqueueRepair: %v", err)
	}
	if before.TTL != defaultRepairTTL {
		t.Fatalf("pre-revision record TTL = %v, want %v", before.TTL, defaultRepairTTL)
	}
	if err := l.SetRepairTTL(24 * time.Hour); err != nil {
		t.Fatalf("SetRepairTTL(24h): %v", err)
	}
	after, err := l.EnqueueRepair(repairTestInstruction("po-ttl-new"), RepairReasonMissingField, "ops")
	if err != nil {
		t.Fatalf("EnqueueRepair: %v", err)
	}
	if after.TTL != 24*time.Hour {
		t.Fatalf("post-revision record TTL = %v, want 24h", after.TTL)
	}
	kept, ok := l.GetRepair(before.ID)
	if !ok {
		t.Fatalf("GetRepair(%s) missing", before.ID)
	}
	if kept.TTL != defaultRepairTTL {
		t.Fatalf("pre-revision record TTL after revision = %v, want %v", kept.TTL, defaultRepairTTL)
	}
}

// TestSetRepairTTLShortenDoesNotExpireParkedEarly pins the
// fail-closed shorten: after the TTL is cut from 7d to 1h, the
// pre-revision record still expires on its own 7d clock, not the
// new 1h one — a sweep 2h after enqueue expires only the
// post-revision record, and the old record expires only once its
// own 7d have passed.
func TestSetRepairTTLShortenDoesNotExpireParkedEarly(t *testing.T) {
	l := New()
	old, err := l.EnqueueRepair(repairTestInstruction("po-ttl-short-old"), RepairReasonMissingField, "ops")
	if err != nil {
		t.Fatalf("EnqueueRepair: %v", err)
	}
	if err := l.SetRepairTTL(time.Hour); err != nil {
		t.Fatalf("SetRepairTTL(1h): %v", err)
	}
	fresh, err := l.EnqueueRepair(repairTestInstruction("po-ttl-short-new"), RepairReasonMissingField, "ops")
	if err != nil {
		t.Fatalf("EnqueueRepair: %v", err)
	}
	base := old.UpdatedAt
	if marked := l.SweepExpiredRepairs(base.Add(2 * time.Hour)); marked != 1 {
		t.Fatalf("sweep at +2h marked %d, want 1 (only the 1h record)", marked)
	}
	if rec, _ := l.GetRepair(old.ID); rec.Status != RepairStatusOpen {
		t.Fatalf("old record status = %q after +2h sweep, want open (its TTL is still 7d)", rec.Status)
	}
	if rec, _ := l.GetRepair(fresh.ID); rec.Status != RepairStatusExpired {
		t.Fatalf("new record status = %q after +2h sweep, want expired (its TTL is 1h)", rec.Status)
	}
	if marked := l.SweepExpiredRepairs(base.Add(8 * 24 * time.Hour)); marked != 1 {
		t.Fatalf("sweep at +8d marked %d, want 1 (the old record, on its own 7d clock)", marked)
	}
	if rec, _ := l.GetRepair(old.ID); rec.Status != RepairStatusExpired {
		t.Fatalf("old record status = %q after +8d sweep, want expired", rec.Status)
	}
}

// TestSetRepairTTLLengthenDoesNotReviveExpired pins the terminal
// invariant: a record expired under the old (short) TTL stays
// expired after the TTL is lengthened — lengthening never revives
// it, and a resubmit attempt fails with ErrRepairTerminal.
func TestSetRepairTTLLengthenDoesNotReviveExpired(t *testing.T) {
	l := New()
	rec, err := l.EnqueueRepair(repairTestInstruction("po-ttl-long"), RepairReasonMissingField, "ops")
	if err != nil {
		t.Fatalf("EnqueueRepair: %v", err)
	}
	if marked := l.SweepExpiredRepairs(rec.UpdatedAt.Add(8 * 24 * time.Hour)); marked != 1 {
		t.Fatalf("sweep at +8d marked %d, want 1", marked)
	}
	if err := l.SetRepairTTL(30 * 24 * time.Hour); err != nil {
		t.Fatalf("SetRepairTTL(30d): %v", err)
	}
	if _, err := l.RepairAndResubmit(rec.ID, map[string]string{"destination": "FPS:6222000012345678"}, "ops"); !errors.Is(err, ErrRepairTerminal) {
		t.Fatalf("resubmit of expired record = %v, want ErrRepairTerminal", err)
	}
	if kept, _ := l.GetRepair(rec.ID); kept.Status != RepairStatusExpired {
		t.Fatalf("record status after lengthen + resubmit attempt = %q, want expired", kept.Status)
	}
}

// TestSetRepairTTLInvalidRejected pins the fail-closed gate: a
// zero or negative TTL is rejected with ErrInvalidRepairTTL, the
// live value is left untouched, and no amendment event is written
// — a non-positive TTL would make new records immediately expired,
// so it is an operator error, never a silent normalization.
func TestSetRepairTTLInvalidRejected(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	for _, ttl := range []time.Duration{0, -time.Hour, -24 * time.Hour} {
		if err := l.SetRepairTTL(ttl); !errors.Is(err, ErrInvalidRepairTTL) {
			t.Fatalf("SetRepairTTL(%v) = %v, want ErrInvalidRepairTTL", ttl, err)
		}
	}
	if got := l.RepairTTL(); got != defaultRepairTTL {
		t.Fatalf("RepairTTL() after rejected revisions = %v, want %v", got, defaultRepairTTL)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := len(auditEventsByOp(t, dir)["repair_ttl_amended"]); n != 0 {
		t.Fatalf("rejected revisions wrote %d amendment events, want 0", n)
	}
}

// TestSetRepairTTLSameValueNoop pins the no-op: revising to the
// current TTL returns nil and writes no audit event.
func TestSetRepairTTLSameValueNoop(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	if err := l.SetRepairTTL(defaultRepairTTL); err != nil {
		t.Fatalf("SetRepairTTL(default) same value: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := len(auditEventsByOp(t, dir)["repair_ttl_amended"]); n != 0 {
		t.Fatalf("same-value revision wrote %d amendment events, want 0", n)
	}
}

// TestSetRepairTTLCtxActorAttribution pins LG-73 attribution on
// the Ctx variant: the operator identity replaces the entrypoint
// name, while the plain setter keeps it. The amendment details
// carry the previous and new TTLs as duration strings.
func TestSetRepairTTLCtxActorAttribution(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	ctx := ContextWithActor(context.Background(), "ops-ttl")
	if err := l.SetRepairTTLCtx(ctx, 24*time.Hour); err != nil {
		t.Fatalf("SetRepairTTLCtx: %v", err)
	}
	if err := l.SetRepairTTL(48 * time.Hour); err != nil {
		t.Fatalf("SetRepairTTL: %v", err)
	}
	waitAuditWritten(t, al, 2)
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	amended := auditEventsByOp(t, dir)["repair_ttl_amended"]
	if len(amended) != 2 {
		t.Fatalf("amendment events = %d, want 2", len(amended))
	}
	if amended[0].Actor != "ops-ttl" {
		t.Errorf("Ctx amendment actor = %q, want ops-ttl", amended[0].Actor)
	}
	if amended[0].Details["previous_ttl"] != defaultRepairTTL.String() {
		t.Errorf("Ctx amendment previous_ttl = %v, want %q", amended[0].Details["previous_ttl"], defaultRepairTTL.String())
	}
	if amended[0].Details["ttl"] != (24 * time.Hour).String() {
		t.Errorf("Ctx amendment ttl = %v, want %q", amended[0].Details["ttl"], (24 * time.Hour).String())
	}
	if amended[1].Actor != "SetRepairTTL" {
		t.Errorf("plain amendment actor = %q, want SetRepairTTL", amended[1].Actor)
	}
	if amended[1].Details["previous_ttl"] != (24 * time.Hour).String() {
		t.Errorf("plain amendment previous_ttl = %v, want %q", amended[1].Details["previous_ttl"], (24 * time.Hour).String())
	}
	if amended[1].Details["ttl"] != (48 * time.Hour).String() {
		t.Errorf("plain amendment ttl = %v, want %q", amended[1].Details["ttl"], (48 * time.Hour).String())
	}
}

// TestRepairTTLGetterFollowsLiveValue pins the live getter: the
// default TTL is 7 days, and a runtime revision moves the getter.
func TestRepairTTLGetterFollowsLiveValue(t *testing.T) {
	if got := New().RepairTTL(); got != defaultRepairTTL {
		t.Fatalf("default RepairTTL() = %v, want %v", got, defaultRepairTTL)
	}
	l := New()
	if err := l.SetRepairTTL(48 * time.Hour); err != nil {
		t.Fatalf("SetRepairTTL(48h): %v", err)
	}
	if got := l.RepairTTL(); got != 48*time.Hour {
		t.Fatalf("RepairTTL() after revision = %v, want 48h", got)
	}
}

// TestSetRepairTTLSnapshotRestoresDefault pins the disaster-
// recovery guard: snapshots carry no repair queue, so a restored
// ledger reports the default TTL (7d) — the amendment must be
// re-applied — and the parked records are not in the snapshot.
func TestSetRepairTTLSnapshotRestoresDefault(t *testing.T) {
	src := New()
	rec, err := src.EnqueueRepair(repairTestInstruction("po-ttl-snap"), RepairReasonMissingField, "ops")
	if err != nil {
		t.Fatalf("EnqueueRepair: %v", err)
	}
	if err := src.SetRepairTTL(24 * time.Hour); err != nil {
		t.Fatalf("SetRepairTTL(24h): %v", err)
	}
	var buf bytes.Buffer
	if err := src.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if got := restored.RepairTTL(); got != defaultRepairTTL {
		t.Fatalf("restored RepairTTL() = %v, want default %v", got, defaultRepairTTL)
	}
	if _, ok := restored.GetRepair(rec.ID); ok {
		t.Fatalf("restored ledger still holds repair record %s", rec.ID)
	}
}
