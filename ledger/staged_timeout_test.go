package ledger

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

// backdateStagedStage moves a staged review's current-stage clock
// back by d, the same test-only manipulation the chain timeout test
// uses for ChainProgress.StageStartedAt: it pins "the stage has
// already waited this long" deterministically, without sleeping.
func backdateStagedStage(t *testing.T, l *Ledger, id string, d time.Duration) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.reviews[id]
	if !ok || r.StagedApproval == nil {
		t.Fatalf("review %q has no staged approval to backdate", id)
	}
	r.StagedApproval.StageStartedAt = time.Now().Add(-d)
}

func TestStagedTimeoutValidation(t *testing.T) {
	l := stagedSeed(t, "tx-t1")
	if err := l.SetStageTimeouts("tx-unknown", []time.Duration{time.Hour}); !errors.Is(err, ErrReviewNotFound) {
		t.Errorf("unknown transfer: err = %v, want ErrReviewNotFound", err)
	}
	// No stages installed yet: timeouts have nothing to attach to.
	if err := l.SetStageTimeouts("tx-t1", []time.Duration{time.Hour}); !errors.Is(err, ErrStagedApprovalNotConfigured) {
		t.Errorf("unstaged: err = %v, want ErrStagedApprovalNotConfigured", err)
	}
	if err := l.SetApprovalStages("tx-t1", stagedRoles); err != nil {
		t.Fatalf("SetApprovalStages: %v", err)
	}
	if err := l.SetStageTimeouts("tx-t1", []time.Duration{time.Hour}); !errors.Is(err, ErrInvalidStageTimeouts) {
		t.Errorf("short list: err = %v, want ErrInvalidStageTimeouts", err)
	}
	if err := l.SetStageTimeouts("tx-t1", []time.Duration{time.Hour, -time.Minute, time.Hour}); !errors.Is(err, ErrInvalidStageTimeouts) {
		t.Errorf("negative lifetime: err = %v, want ErrInvalidStageTimeouts", err)
	}
	if err := l.SetStageTimeouts("tx-t1", []time.Duration{time.Hour, 30 * time.Minute, time.Hour}); err != nil {
		t.Fatalf("valid install: %v", err)
	}
	// The accessor returns a copy: mutating it must not affect the ledger.
	got, ok := l.StagedTimeouts("tx-t1")
	if !ok || len(got) != 3 || got[1] != 30*time.Minute {
		t.Fatalf("StagedTimeouts = %v, ok=%v, want [1h 30m 1h]", got, ok)
	}
	got[1] = 0
	again, _ := l.StagedTimeouts("tx-t1")
	if again[1] != 30*time.Minute {
		t.Errorf("accessor copy leaked mutation: %v", again)
	}
	if err := l.SetStageTimeouts("tx-t1", []time.Duration{time.Hour, time.Hour, time.Hour}); !errors.Is(err, ErrStagedTimeoutsAlreadySet) {
		t.Errorf("second install: err = %v, want ErrStagedTimeoutsAlreadySet", err)
	}
	if _, ok := l.StagedTimeouts("tx-unknown"); ok {
		t.Error("StagedTimeouts on unknown transfer returned true")
	}
	// A review that is no longer pending refuses installation.
	l2 := stagedSeed(t, "tx-t2")
	if err := l2.SetApprovalStages("tx-t2", stagedRoles); err != nil {
		t.Fatalf("SetApprovalStages: %v", err)
	}
	if _, err := l2.ApproveTransferReview("tx-t2", "alice"); err != nil {
		t.Fatalf("stage 0 approve: %v", err)
	}
	if _, err := l2.ApproveTransferReview("tx-t2", "bob"); err != nil {
		t.Fatalf("stage 1 approve: %v", err)
	}
	if _, err := l2.ApproveTransferReview("tx-t2", "carol"); err != nil {
		t.Fatalf("stage 2 approve: %v", err)
	}
	if err := l2.SetStageTimeouts("tx-t2", []time.Duration{time.Hour, time.Hour, time.Hour}); !errors.Is(err, ErrReviewNotPending) {
		t.Errorf("settled review: err = %v, want ErrReviewNotPending", err)
	}
}

func TestStagedTimeoutLazyApproveRejects(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithReviewThreshold(10000))
	seedBalance(t, l, "payer", 100000)
	if _, err := l.PostTransfer(Transfer{ID: "tx-t3", From: "payer", To: "payee", AmountCents: 30000}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if err := l.SetApprovalStages("tx-t3", stagedRoles[:2]); err != nil {
		t.Fatalf("SetApprovalStages: %v", err)
	}
	if err := l.SetStageTimeouts("tx-t3", []time.Duration{time.Hour, time.Hour}); err != nil {
		t.Fatalf("SetStageTimeouts: %v", err)
	}
	backdateStagedStage(t, l, "tx-t3", 2*time.Hour)

	// Approving past a lapsed stage is refused, and the review is
	// rejected first — fail-closed, never advanced or settled.
	ctx := ContextWithActor(context.Background(), "ops-desk")
	if _, err := l.ApproveStagedReviewCtx(ctx, "tx-t3", "alice"); !errors.Is(err, ErrReviewExpired) {
		t.Fatalf("approve on lapsed stage: err = %v, want ErrReviewExpired", err)
	}
	rev, _ := l.GetTransferReview("tx-t3")
	if rev.Status != ReviewStatusRejected {
		t.Errorf("status = %q, want rejected after stage timeout", rev.Status)
	}
	if rev.ReviewedBy != "system" {
		t.Errorf("ReviewedBy = %q, want system", rev.ReviewedBy)
	}
	// The staged progress survives the timeout rejection: the trail
	// shows which stages and timeouts were in force.
	if rev.StagedApproval == nil || len(rev.StagedApproval.Timeouts) != 2 {
		t.Errorf("staged progress after timeout = %+v, want timeouts preserved", rev.StagedApproval)
	}
	if got := l.Available("payer"); got != 100000 {
		t.Errorf("payer available = %d, want 100000 (freeze released)", got)
	}
	if got := l.Balance("payee"); got != 0 {
		t.Errorf("payee balance = %d, want 0 (never settled)", got)
	}
	// A rejected review stays rejected: no second decision.
	if _, err := l.ApproveTransferReview("tx-t3", "alice"); !errors.Is(err, ErrReviewNotPending) {
		t.Errorf("approve after timeout rejection: err = %v, want ErrReviewNotPending", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	byOp := auditEventsByOp(t, dir)
	events := byOp["transfer_review_staged_stage_timeout"]
	if len(events) != 1 {
		t.Fatalf("staged timeout events = %d, want 1", len(events))
	}
	d := events[0].Details
	if d["stage"] != float64(0) || d["role"] != "manager" || d["reason"] != "staged stage timeout" {
		t.Errorf("timeout event details = %+v, want stage 0 manager with timeout reason", d)
	}
	if events[0].Actor != "ops-desk" {
		t.Errorf("timeout event actor = %q, want ops-desk", events[0].Actor)
	}
}

func TestStagedTimeoutSweepAndLazyAvailable(t *testing.T) {
	l := stagedSeed(t, "tx-t4")
	if err := l.SetApprovalStages("tx-t4", stagedRoles[:2]); err != nil {
		t.Fatalf("SetApprovalStages: %v", err)
	}
	if err := l.SetStageTimeouts("tx-t4", []time.Duration{time.Hour, time.Hour}); err != nil {
		t.Fatalf("SetStageTimeouts: %v", err)
	}
	backdateStagedStage(t, l, "tx-t4", 2*time.Hour)

	// Lazy by predicate: the freeze already released for
	// available-balance purposes and the pending list, before any
	// sweep runs.
	if got := l.Available("payer"); got != 100000 {
		t.Errorf("lazy available = %d, want 100000", got)
	}
	if pending := l.PendingTransferReviews(); len(pending) != 0 {
		t.Errorf("pending reviews = %d, want 0 (timed-out stage excluded)", len(pending))
	}
	// The sweep marks it rejected, and the sweep count keeps its
	// LG-42 meaning: stage timeouts are rejections, not expiries.
	if n := l.ExpireReviewsAt(time.Now().Add(time.Hour)); n != 0 {
		t.Errorf("ExpireReviewsAt returned %d, want 0 (stage timeout is a reject, not an expiry)", n)
	}
	rev, _ := l.GetTransferReview("tx-t4")
	if rev.Status != ReviewStatusRejected || rev.ReviewedBy != "system" {
		t.Errorf("swept review = status %q reviewed_by %q, want rejected/system", rev.Status, rev.ReviewedBy)
	}
	// The explicit staged sweep is idempotent once rejected.
	if n := l.ExpireStagedReviewsAt(time.Now()); n != 0 {
		t.Errorf("second staged sweep = %d, want 0", n)
	}
}

func TestStagedTimeoutPerStageClock(t *testing.T) {
	// The advance resets the next stage's clock: backdate the stage
	// clock to a known past instant, advance, and the new stage
	// must start from the advance instant, not the backdated one.
	l := stagedSeed(t, "tx-t5")
	if err := l.SetApprovalStages("tx-t5", stagedRoles); err != nil {
		t.Fatalf("SetApprovalStages: %v", err)
	}
	if err := l.SetStageTimeouts("tx-t5", []time.Duration{24 * time.Hour, 24 * time.Hour, 24 * time.Hour}); err != nil {
		t.Fatalf("SetStageTimeouts: %v", err)
	}
	backdateStagedStage(t, l, "tx-t5", 2*time.Hour)
	if _, err := l.ApproveStagedReview("tx-t5", "alice"); err != nil {
		t.Fatalf("stage 0 approve: %v", err)
	}
	rev, _ := l.GetTransferReview("tx-t5")
	if rev.StagedApproval == nil || rev.StagedApproval.Stage != 1 {
		t.Fatalf("after stage 0: staged = %+v, want stage 1", rev.StagedApproval)
	}
	if rev.StagedApproval.StageStartedAt.Before(time.Now().Add(-time.Minute)) {
		t.Errorf("stage 1 clock = %v, want ~now (reset at advance, not backdated)", rev.StagedApproval.StageStartedAt)
	}

	// The current stage's own timeout governs: stage 1 waited 20
	// minutes — past its 10-minute lifetime, inside stage 0's
	// 1-hour lifetime — so it times out.
	l2 := stagedSeed(t, "tx-t6")
	if err := l2.SetApprovalStages("tx-t6", stagedRoles[:2]); err != nil {
		t.Fatalf("SetApprovalStages: %v", err)
	}
	if err := l2.SetStageTimeouts("tx-t6", []time.Duration{time.Hour, 10 * time.Minute}); err != nil {
		t.Fatalf("SetStageTimeouts: %v", err)
	}
	if _, err := l2.ApproveStagedReview("tx-t6", "alice"); err != nil {
		t.Fatalf("stage 0 approve: %v", err)
	}
	backdateStagedStage(t, l2, "tx-t6", 20*time.Minute)
	if n := l2.ExpireStagedReviewsAt(time.Now()); n != 1 {
		t.Errorf("staged sweep = %d, want 1 (stage 1 lapsed)", n)
	}
	rev2, _ := l2.GetTransferReview("tx-t6")
	if rev2.Status != ReviewStatusRejected {
		t.Errorf("status = %q, want rejected", rev2.Status)
	}

	// Control: same 20-minute wait, but stage 1's lifetime is
	// 1 hour — the review stays pending.
	l3 := stagedSeed(t, "tx-t7")
	if err := l3.SetApprovalStages("tx-t7", stagedRoles[:2]); err != nil {
		t.Fatalf("SetApprovalStages: %v", err)
	}
	if err := l3.SetStageTimeouts("tx-t7", []time.Duration{10 * time.Minute, time.Hour}); err != nil {
		t.Fatalf("SetStageTimeouts: %v", err)
	}
	if _, err := l3.ApproveStagedReview("tx-t7", "alice"); err != nil {
		t.Fatalf("stage 0 approve: %v", err)
	}
	backdateStagedStage(t, l3, "tx-t7", 20*time.Minute)
	if n := l3.ExpireStagedReviewsAt(time.Now()); n != 0 {
		t.Errorf("staged sweep = %d, want 0 (stage 1 still within its hour)", n)
	}
	rev3, _ := l3.GetTransferReview("tx-t7")
	if rev3.Status != ReviewStatusPending {
		t.Errorf("status = %q, want pending", rev3.Status)
	}
}

func TestStagedTimeoutZeroNeverTimesOut(t *testing.T) {
	l := stagedSeed(t, "tx-t8")
	if err := l.SetApprovalStages("tx-t8", stagedRoles[:2]); err != nil {
		t.Fatalf("SetApprovalStages: %v", err)
	}
	if err := l.SetStageTimeouts("tx-t8", []time.Duration{0, 0}); err != nil {
		t.Fatalf("SetStageTimeouts: %v", err)
	}
	if n := l.ExpireReviewsAt(time.Now().Add(100 * 24 * time.Hour)); n != 0 {
		t.Errorf("ExpireReviewsAt = %d, want 0", n)
	}
	rev, _ := l.GetTransferReview("tx-t8")
	if rev.Status != ReviewStatusPending {
		t.Errorf("status = %q, want pending (zero timeout never lapses)", rev.Status)
	}
	if got := l.Available("payer"); got != 70000 {
		t.Errorf("payer available = %d, want 70000 (freeze still held)", got)
	}
}

func TestStagedTimeoutSnapshotRoundTrip(t *testing.T) {
	l := stagedSeed(t, "tx-t9")
	if err := l.SetApprovalStages("tx-t9", stagedRoles); err != nil {
		t.Fatalf("SetApprovalStages: %v", err)
	}
	if err := l.SetStageTimeouts("tx-t9", []time.Duration{time.Hour, 30 * time.Minute, time.Hour}); err != nil {
		t.Fatalf("SetStageTimeouts: %v", err)
	}
	if _, err := l.ApproveStagedReview("tx-t9", "alice"); err != nil {
		t.Fatalf("stage 0 approve: %v", err)
	}
	before, _ := l.GetTransferReview("tx-t9")

	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	after, ok := restored.GetTransferReview("tx-t9")
	if !ok {
		t.Fatal("restored review missing")
	}
	if !stagedApprovalEqual(before.StagedApproval, after.StagedApproval) {
		t.Errorf("restored staged progress = %+v, want %+v", after.StagedApproval, before.StagedApproval)
	}
	timeouts, ok := restored.StagedTimeouts("tx-t9")
	if !ok || len(timeouts) != 3 || timeouts[1] != 30*time.Minute {
		t.Errorf("restored timeouts = %v, ok=%v, want [1h 30m 1h]", timeouts, ok)
	}
	if after.Status != ReviewStatusPending {
		t.Errorf("restored status = %q, want pending", after.Status)
	}
}
