package ledger

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestAmendFutureStageTimeoutsValidation(t *testing.T) {
	l := stagedSeed(t, "tx-f1")
	if err := l.AmendFutureStageTimeouts("tx-unknown", []time.Duration{time.Hour}); !errors.Is(err, ErrReviewNotFound) {
		t.Errorf("unknown transfer: err = %v, want ErrReviewNotFound", err)
	}
	// No stages installed yet: there is nothing timed to revise.
	if err := l.AmendFutureStageTimeouts("tx-f1", []time.Duration{time.Hour}); !errors.Is(err, ErrStagedApprovalNotConfigured) {
		t.Errorf("unstaged: err = %v, want ErrStagedApprovalNotConfigured", err)
	}
	if err := l.SetApprovalStages("tx-f1", stagedRoles); err != nil {
		t.Fatalf("SetApprovalStages: %v", err)
	}
	// Stages installed but no timeouts: revision never invents them.
	if err := l.AmendFutureStageTimeouts("tx-f1", []time.Duration{time.Hour, time.Hour}); !errors.Is(err, ErrStagedTimeoutsNotSet) {
		t.Errorf("timeouts unset: err = %v, want ErrStagedTimeoutsNotSet", err)
	}
	if err := l.SetStageTimeouts("tx-f1", []time.Duration{time.Hour, time.Hour, time.Hour}); err != nil {
		t.Fatalf("SetStageTimeouts: %v", err)
	}
	// At stage 0 of 3, exactly 2 future stages exist.
	if err := l.AmendFutureStageTimeouts("tx-f1", nil); !errors.Is(err, ErrInvalidFutureStageTimeouts) {
		t.Errorf("empty list: err = %v, want ErrInvalidFutureStageTimeouts", err)
	}
	if err := l.AmendFutureStageTimeouts("tx-f1", []time.Duration{2 * time.Hour}); !errors.Is(err, ErrInvalidFutureStageTimeouts) {
		t.Errorf("short list: err = %v, want ErrInvalidFutureStageTimeouts", err)
	}
	if err := l.AmendFutureStageTimeouts("tx-f1", []time.Duration{2 * time.Hour, 2 * time.Hour, 2 * time.Hour}); !errors.Is(err, ErrInvalidFutureStageTimeouts) {
		t.Errorf("long list: err = %v, want ErrInvalidFutureStageTimeouts", err)
	}
	if err := l.AmendFutureStageTimeouts("tx-f1", []time.Duration{2 * time.Hour, -time.Minute}); !errors.Is(err, ErrInvalidFutureStageTimeouts) {
		t.Errorf("negative lifetime: err = %v, want ErrInvalidFutureStageTimeouts", err)
	}
	// Every rejected revision leaves the installed timeouts untouched.
	got, _ := l.StagedTimeouts("tx-f1")
	if len(got) != 3 || got[0] != time.Hour || got[1] != time.Hour || got[2] != time.Hour {
		t.Errorf("timeouts after rejected revisions = %v, want 3x1h", got)
	}
	// A settled review refuses revision.
	l2 := stagedSeed(t, "tx-f2")
	if _, err := l2.ApproveTransferReview("tx-f2", "alice"); err != nil {
		t.Fatalf("flat approve: %v", err)
	}
	if err := l2.AmendFutureStageTimeouts("tx-f2", []time.Duration{time.Hour}); !errors.Is(err, ErrReviewNotPending) {
		t.Errorf("settled review: err = %v, want ErrReviewNotPending", err)
	}
}

func TestAmendFutureStageTimeoutsMidFlight(t *testing.T) {
	l := stagedSeed(t, "tx-f3")
	if err := l.SetApprovalStages("tx-f3", stagedRoles); err != nil {
		t.Fatalf("SetApprovalStages: %v", err)
	}
	if err := l.SetStageTimeouts("tx-f3", []time.Duration{time.Hour, time.Hour, time.Hour}); err != nil {
		t.Fatalf("SetStageTimeouts: %v", err)
	}
	deadline := time.Now().Add(24 * time.Hour)
	if err := l.SetStagedReviewDeadline("tx-f3", deadline); err != nil {
		t.Fatalf("SetStagedReviewDeadline: %v", err)
	}
	if _, err := l.ApproveStagedReview("tx-f3", "alice"); err != nil {
		t.Fatalf("stage 0 approve: %v", err)
	}
	// Sitting on stage 1 (finance), stage 2 is the only future
	// stage, so the future list has exactly one entry.
	before, _ := l.StagedApproval("tx-f3")
	versionBefore := l.Version()
	if err := l.AmendFutureStageTimeouts("tx-f3", []time.Duration{2 * time.Hour}); err != nil {
		t.Fatalf("AmendFutureStageTimeouts: %v", err)
	}
	after, _ := l.StagedApproval("tx-f3")
	if after.Stage != 1 || len(after.Stages) != 3 {
		t.Errorf("progress = stage %d of %d, want stage 1 of 3", after.Stage, len(after.Stages))
	}
	// The decided stage's timeout and the current stage's timeout
	// are immutable; only the future stage moved.
	if len(after.Timeouts) != 3 || after.Timeouts[0] != time.Hour || after.Timeouts[1] != time.Hour || after.Timeouts[2] != 2*time.Hour {
		t.Errorf("timeouts = %v, want [1h 1h 2h]", after.Timeouts)
	}
	// The current stage's clock keeps running from its advance
	// instant — the revision moves no clock.
	if after.StageStartedAt.UnixNano() != before.StageStartedAt.UnixNano() {
		t.Errorf("stage clock moved: before %v, after %v", before.StageStartedAt, after.StageStartedAt)
	}
	if after.Deadline.UnixNano() != deadline.UnixNano() {
		t.Errorf("deadline = %v, want %v (LG-226 config untouched)", after.Deadline, deadline)
	}
	if len(after.Approvers) != 1 || after.Approvers[0] != "alice" {
		t.Errorf("approvers = %v, want [alice]", after.Approvers)
	}
	if l.Version() != versionBefore {
		t.Errorf("version moved: revision is structural")
	}
	// The revised chain still walks to settlement.
	if _, err := l.ApproveStagedReview("tx-f3", "bob"); err != nil {
		t.Fatalf("stage 1 approve: %v", err)
	}
	rec, err := l.ApproveStagedReview("tx-f3", "carol")
	if err != nil {
		t.Fatalf("revised stage approve: %v", err)
	}
	if rec.ReviewStatus != ReviewStatusApproved {
		t.Errorf("final receipt status = %q, want approved", rec.ReviewStatus)
	}
}

func TestAmendFutureStageTimeoutsZeroNeverTimesOut(t *testing.T) {
	l := stagedSeed(t, "tx-f4")
	if err := l.SetApprovalStages("tx-f4", stagedRoles); err != nil {
		t.Fatalf("SetApprovalStages: %v", err)
	}
	if err := l.SetStageTimeouts("tx-f4", []time.Duration{time.Hour, time.Hour, time.Hour}); err != nil {
		t.Fatalf("SetStageTimeouts: %v", err)
	}
	// Re-time both future stages from stage 0: stage 1 to 30m,
	// stage 2 to 0 — never times out on its own.
	if err := l.AmendFutureStageTimeouts("tx-f4", []time.Duration{30 * time.Minute, 0}); err != nil {
		t.Fatalf("AmendFutureStageTimeouts: %v", err)
	}
	if _, err := l.ApproveStagedReview("tx-f4", "alice"); err != nil {
		t.Fatalf("stage 0 approve: %v", err)
	}
	if _, err := l.ApproveStagedReview("tx-f4", "bob"); err != nil {
		t.Fatalf("stage 1 approve: %v", err)
	}
	// Sitting on stage 2 with a zero timeout: even a sweep far in
	// the future must not time it out.
	if got := l.ExpireStagedReviewsAt(time.Now().Add(1000 * time.Hour)); got != 0 {
		t.Errorf("sweep expired = %d, want 0 (zero timeout never lapses)", got)
	}
	rev, ok := l.GetTransferReview("tx-f4")
	if !ok || rev.Status != ReviewStatusPending {
		t.Errorf("review = %+v, want still pending", rev)
	}
	if got, _ := l.StagedTimeouts("tx-f4"); len(got) != 3 || got[1] != 30*time.Minute || got[2] != 0 {
		t.Errorf("timeouts = %v, want [1h 30m 0]", got)
	}
}

func TestAmendFutureStageTimeoutsNoFutureStages(t *testing.T) {
	// A single-stage chain has no future stage from the start.
	l := stagedSeed(t, "tx-f5")
	if err := l.SetApprovalStages("tx-f5", stagedRoles[:1]); err != nil {
		t.Fatalf("SetApprovalStages: %v", err)
	}
	if err := l.SetStageTimeouts("tx-f5", []time.Duration{time.Hour}); err != nil {
		t.Fatalf("SetStageTimeouts: %v", err)
	}
	if err := l.AmendFutureStageTimeouts("tx-f5", nil); !errors.Is(err, ErrInvalidFutureStageTimeouts) {
		t.Errorf("no future stages (empty): err = %v, want ErrInvalidFutureStageTimeouts", err)
	}
	if err := l.AmendFutureStageTimeouts("tx-f5", []time.Duration{time.Hour}); !errors.Is(err, ErrInvalidFutureStageTimeouts) {
		t.Errorf("no future stages (one timeout): err = %v, want ErrInvalidFutureStageTimeouts", err)
	}
	// A 3-stage chain sitting on its last stage likewise has none.
	l2 := stagedSeed(t, "tx-f6")
	if err := l2.SetApprovalStages("tx-f6", stagedRoles); err != nil {
		t.Fatalf("SetApprovalStages: %v", err)
	}
	if err := l2.SetStageTimeouts("tx-f6", []time.Duration{time.Hour, time.Hour, time.Hour}); err != nil {
		t.Fatalf("SetStageTimeouts: %v", err)
	}
	if _, err := l2.ApproveStagedReview("tx-f6", "alice"); err != nil {
		t.Fatalf("stage 0 approve: %v", err)
	}
	if _, err := l2.ApproveStagedReview("tx-f6", "bob"); err != nil {
		t.Fatalf("stage 1 approve: %v", err)
	}
	if err := l2.AmendFutureStageTimeouts("tx-f6", nil); !errors.Is(err, ErrInvalidFutureStageTimeouts) {
		t.Errorf("last stage: err = %v, want ErrInvalidFutureStageTimeouts", err)
	}
}

func TestAmendFutureStageTimeoutsAuditAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithReviewThreshold(10000))
	seedBalance(t, l, "payer", 100000)
	if _, err := l.PostTransfer(Transfer{ID: "tx-f7", From: "payer", To: "payee", AmountCents: 30000}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if err := l.SetApprovalStages("tx-f7", stagedRoles); err != nil {
		t.Fatalf("SetApprovalStages: %v", err)
	}
	if err := l.SetStageTimeouts("tx-f7", []time.Duration{time.Hour, time.Hour, time.Hour}); err != nil {
		t.Fatalf("SetStageTimeouts: %v", err)
	}
	ctx := ContextWithActor(context.Background(), "ops-desk")
	if err := l.AmendFutureStageTimeoutsCtx(ctx, "tx-f7", []time.Duration{2 * time.Hour, 30 * time.Minute}); err != nil {
		t.Fatalf("AmendFutureStageTimeouts: %v", err)
	}
	before, _ := l.GetTransferReview("tx-f7")
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	after, ok := restored.GetTransferReview("tx-f7")
	if !ok {
		t.Fatal("restored review missing")
	}
	if !stagedApprovalEqual(before.StagedApproval, after.StagedApproval) {
		t.Errorf("restored staged progress = %+v, want %+v", after.StagedApproval, before.StagedApproval)
	}
	if got, _ := restored.StagedTimeouts("tx-f7"); len(got) != 3 || got[1] != 2*time.Hour || got[2] != 30*time.Minute {
		t.Errorf("restored timeouts = %v, want [1h 2h 30m]", got)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["staged_future_timeouts_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
	if events[0].Actor != "ops-desk" {
		t.Errorf("event actor = %q, want ops-desk", events[0].Actor)
	}
	if events[0].Details["stage"] != float64(0) || events[0].Details["stage_count"] != float64(3) {
		t.Errorf("event details = %+v, want stage 0 of 3", events[0].Details)
	}
	prev, _ := events[0].Details["previous_timeouts"].([]any)
	next, _ := events[0].Details["timeouts"].([]any)
	if len(prev) != 2 || prev[0] != "1h0m0s" || prev[1] != "1h0m0s" {
		t.Errorf("previous_timeouts = %v, want [1h0m0s 1h0m0s]", prev)
	}
	if len(next) != 2 || next[0] != "2h0m0s" || next[1] != "30m0s" {
		t.Errorf("timeouts = %v, want [2h0m0s 30m0s]", next)
	}
}
