package ledger

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestReplaceFutureStageRolesValidation(t *testing.T) {
	l := stagedSeed(t, "tx-r1")
	if err := l.ReplaceFutureStageRoles("tx-unknown", []ApprovalStage{{Role: "audit"}}); !errors.Is(err, ErrReviewNotFound) {
		t.Errorf("unknown transfer: err = %v, want ErrReviewNotFound", err)
	}
	// No stages installed yet: there is nothing future to revise.
	if err := l.ReplaceFutureStageRoles("tx-r1", []ApprovalStage{{Role: "audit"}}); !errors.Is(err, ErrStagedApprovalNotConfigured) {
		t.Errorf("unstaged: err = %v, want ErrStagedApprovalNotConfigured", err)
	}
	if err := l.SetApprovalStages("tx-r1", stagedRoles); err != nil {
		t.Fatalf("SetApprovalStages: %v", err)
	}
	// At stage 0 of 3, exactly 2 future stages exist.
	if err := l.ReplaceFutureStageRoles("tx-r1", nil); !errors.Is(err, ErrInvalidFutureStageRoles) {
		t.Errorf("empty list: err = %v, want ErrInvalidFutureStageRoles", err)
	}
	if err := l.ReplaceFutureStageRoles("tx-r1", []ApprovalStage{{Role: "audit"}}); !errors.Is(err, ErrInvalidFutureStageRoles) {
		t.Errorf("short list: err = %v, want ErrInvalidFutureStageRoles", err)
	}
	if err := l.ReplaceFutureStageRoles("tx-r1", []ApprovalStage{{Role: "a"}, {Role: "b"}, {Role: "c"}}); !errors.Is(err, ErrInvalidFutureStageRoles) {
		t.Errorf("long list: err = %v, want ErrInvalidFutureStageRoles", err)
	}
	if err := l.ReplaceFutureStageRoles("tx-r1", []ApprovalStage{{Role: "audit"}, {Role: "  "}}); !errors.Is(err, ErrInvalidFutureStageRoles) {
		t.Errorf("blank role: err = %v, want ErrInvalidFutureStageRoles", err)
	}
	// Every rejected revision leaves the installed stages untouched.
	p, _ := l.StagedApproval("tx-r1")
	if len(p.Stages) != 3 || p.Stages[0].Role != "manager" || p.Stages[1].Role != "finance" || p.Stages[2].Role != "treasury" {
		t.Errorf("stages after rejected revisions = %+v, want installed roles intact", p.Stages)
	}
	// A settled review refuses revision.
	l2 := stagedSeed(t, "tx-r2")
	if _, err := l2.ApproveTransferReview("tx-r2", "alice"); err != nil {
		t.Fatalf("flat approve: %v", err)
	}
	if err := l2.ReplaceFutureStageRoles("tx-r2", []ApprovalStage{{Role: "audit"}}); !errors.Is(err, ErrReviewNotPending) {
		t.Errorf("settled review: err = %v, want ErrReviewNotPending", err)
	}
}

func TestReplaceFutureStageRolesMidFlight(t *testing.T) {
	l := stagedSeed(t, "tx-r3")
	if err := l.SetApprovalStages("tx-r3", stagedRoles); err != nil {
		t.Fatalf("SetApprovalStages: %v", err)
	}
	if _, err := l.ApproveStagedReview("tx-r3", "alice"); err != nil {
		t.Fatalf("stage 0 approve: %v", err)
	}
	// Sitting on stage 1 (finance), only stage 2 is future.
	if err := l.ReplaceFutureStageRoles("tx-r3", []ApprovalStage{{Role: "audit"}}); err != nil {
		t.Fatalf("ReplaceFutureStageRoles: %v", err)
	}
	p, ok := l.StagedApproval("tx-r3")
	if !ok {
		t.Fatal("StagedApproval: not found after revision")
	}
	if p.Stage != 1 || len(p.Stages) != 3 {
		t.Errorf("progress = stage %d of %d, want stage 1 of 3", p.Stage, len(p.Stages))
	}
	if p.Stages[0].Role != "manager" || p.Stages[1].Role != "finance" || p.Stages[2].Role != "audit" {
		t.Errorf("stages = %+v, want manager/finance/audit", p.Stages)
	}
	if len(p.Approvers) != 1 || p.Approvers[0] != "alice" {
		t.Errorf("approvers = %v, want [alice] (decided approvers never move)", p.Approvers)
	}
	// The revised chain still walks to settlement: stage 1 by bob
	// (not alice, so no adjacent repeat), then the revised stage 2.
	if _, err := l.ApproveStagedReview("tx-r3", "bob"); err != nil {
		t.Fatalf("stage 1 approve: %v", err)
	}
	rec, err := l.ApproveStagedReview("tx-r3", "carol")
	if err != nil {
		t.Fatalf("revised stage approve: %v", err)
	}
	if rec.ReviewStatus != ReviewStatusApproved {
		t.Errorf("final receipt status = %q, want approved", rec.ReviewStatus)
	}
}

func TestReplaceFutureStageRolesInvariants(t *testing.T) {
	l := stagedSeed(t, "tx-r4")
	if err := l.SetApprovalStages("tx-r4", stagedRoles); err != nil {
		t.Fatalf("SetApprovalStages: %v", err)
	}
	if err := l.SetStageTimeouts("tx-r4", []time.Duration{time.Hour, time.Hour, time.Hour}); err != nil {
		t.Fatalf("SetStageTimeouts: %v", err)
	}
	deadline := time.Now().Add(24 * time.Hour)
	if err := l.SetStagedReviewDeadline("tx-r4", deadline); err != nil {
		t.Fatalf("SetStagedReviewDeadline: %v", err)
	}
	if _, err := l.ApproveStagedReview("tx-r4", "alice"); err != nil {
		t.Fatalf("stage 0 approve: %v", err)
	}
	before, _ := l.StagedApproval("tx-r4")
	if err := l.ReplaceFutureStageRoles("tx-r4", []ApprovalStage{{Role: "audit"}}); err != nil {
		t.Fatalf("ReplaceFutureStageRoles: %v", err)
	}
	after, _ := l.StagedApproval("tx-r4")
	// Everything but the one future role is identical.
	if after.Stage != before.Stage || len(after.Stages) != len(before.Stages) {
		t.Errorf("stage/count changed: before stage %d of %d, after stage %d of %d",
			before.Stage, len(before.Stages), after.Stage, len(after.Stages))
	}
	if after.Stages[0] != before.Stages[0] || after.Stages[1] != before.Stages[1] {
		t.Errorf("current/decided stages moved: before %+v, after %+v", before.Stages, after.Stages)
	}
	if len(after.Timeouts) != 3 || after.Timeouts[0] != time.Hour || after.Timeouts[1] != time.Hour || after.Timeouts[2] != time.Hour {
		t.Errorf("timeouts = %v, want 3x1h (LG-223 config untouched)", after.Timeouts)
	}
	if after.Deadline.UnixNano() != deadline.UnixNano() {
		t.Errorf("deadline = %v, want %v (LG-226 config untouched)", after.Deadline, deadline)
	}
	if after.StageStartedAt.UnixNano() != before.StageStartedAt.UnixNano() {
		t.Errorf("stage clock moved: before %v, after %v", before.StageStartedAt, after.StageStartedAt)
	}
	if len(after.Approvers) != 1 || after.Approvers[0] != "alice" {
		t.Errorf("approvers = %v, want [alice]", after.Approvers)
	}
}

func TestReplaceFutureStageRolesNoFutureStages(t *testing.T) {
	// A single-stage chain has no future stage from the start.
	l := stagedSeed(t, "tx-r5")
	if err := l.SetApprovalStages("tx-r5", stagedRoles[:1]); err != nil {
		t.Fatalf("SetApprovalStages: %v", err)
	}
	if err := l.ReplaceFutureStageRoles("tx-r5", nil); !errors.Is(err, ErrInvalidFutureStageRoles) {
		t.Errorf("no future stages (empty): err = %v, want ErrInvalidFutureStageRoles", err)
	}
	if err := l.ReplaceFutureStageRoles("tx-r5", []ApprovalStage{{Role: "audit"}}); !errors.Is(err, ErrInvalidFutureStageRoles) {
		t.Errorf("no future stages (one role): err = %v, want ErrInvalidFutureStageRoles", err)
	}
	// A 3-stage chain sitting on its last stage likewise has none.
	l2 := stagedSeed(t, "tx-r6")
	if err := l2.SetApprovalStages("tx-r6", stagedRoles); err != nil {
		t.Fatalf("SetApprovalStages: %v", err)
	}
	if _, err := l2.ApproveStagedReview("tx-r6", "alice"); err != nil {
		t.Fatalf("stage 0 approve: %v", err)
	}
	if _, err := l2.ApproveStagedReview("tx-r6", "bob"); err != nil {
		t.Fatalf("stage 1 approve: %v", err)
	}
	if err := l2.ReplaceFutureStageRoles("tx-r6", nil); !errors.Is(err, ErrInvalidFutureStageRoles) {
		t.Errorf("last stage: err = %v, want ErrInvalidFutureStageRoles", err)
	}
}

func TestReplaceFutureStageRolesAuditAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithReviewThreshold(10000))
	seedBalance(t, l, "payer", 100000)
	if _, err := l.PostTransfer(Transfer{ID: "tx-r7", From: "payer", To: "payee", AmountCents: 30000}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if err := l.SetApprovalStages("tx-r7", stagedRoles); err != nil {
		t.Fatalf("SetApprovalStages: %v", err)
	}
	ctx := ContextWithActor(context.Background(), "ops-desk")
	if err := l.ReplaceFutureStageRolesCtx(ctx, "tx-r7", []ApprovalStage{{Role: "ops"}, {Role: "risk"}}); err != nil {
		t.Fatalf("ReplaceFutureStageRoles: %v", err)
	}
	before, _ := l.GetTransferReview("tx-r7")
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	after, ok := restored.GetTransferReview("tx-r7")
	if !ok {
		t.Fatal("restored review missing")
	}
	if !stagedApprovalEqual(before.StagedApproval, after.StagedApproval) {
		t.Errorf("restored staged progress = %+v, want %+v", after.StagedApproval, before.StagedApproval)
	}
	if after.StagedApproval.Stages[1].Role != "ops" || after.StagedApproval.Stages[2].Role != "risk" {
		t.Errorf("restored stages = %+v, want revised ops/risk future roles", after.StagedApproval.Stages)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["staged_future_roles_replaced"]
	if len(events) != 1 {
		t.Fatalf("replace events = %d, want 1", len(events))
	}
	if events[0].Actor != "ops-desk" {
		t.Errorf("event actor = %q, want ops-desk", events[0].Actor)
	}
	if events[0].Details["stage"] != float64(0) || events[0].Details["stage_count"] != float64(3) {
		t.Errorf("event details = %+v, want stage 0 of 3", events[0].Details)
	}
}
