package ledger

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

// backdateStagedDeadline moves a staged review's overall deadline back
// by d, the same test-only manipulation backdateStagedStage uses for
// the stage clock: it pins "the deadline has already lapsed" without
// sleeping, and without going through the install-time validation
// that would reject a past deadline outright.
func backdateStagedDeadline(t *testing.T, l *Ledger, id string, d time.Duration) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.reviews[id]
	if !ok || r.StagedApproval == nil {
		t.Fatalf("review %q has no staged approval to backdate", id)
	}
	r.StagedApproval.Deadline = time.Now().Add(-d)
}

// deadlineEventIDs extracts the transfer_ids of a batched sweep
// audit event, decoded from the audit log as []any.
func deadlineEventIDs(t *testing.T, d map[string]any) []string {
	t.Helper()
	raw, ok := d["transfer_ids"].([]any)
	if !ok {
		t.Fatalf("transfer_ids = %#v, want a list", d["transfer_ids"])
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("transfer_ids entry = %#v, want a string", v)
		}
		out = append(out, s)
	}
	return out
}

func TestStagedDeadlineValidation(t *testing.T) {
	l := stagedSeed(t, "tx-d1")
	if err := l.SetStagedReviewDeadline("tx-unknown", time.Now().Add(time.Hour)); !errors.Is(err, ErrReviewNotFound) {
		t.Errorf("unknown transfer: err = %v, want ErrReviewNotFound", err)
	}
	// No stages installed yet: the deadline has nothing to attach to.
	if err := l.SetStagedReviewDeadline("tx-d1", time.Now().Add(time.Hour)); !errors.Is(err, ErrStagedApprovalNotConfigured) {
		t.Errorf("unstaged: err = %v, want ErrStagedApprovalNotConfigured", err)
	}
	if err := l.SetApprovalStages("tx-d1", stagedRoles); err != nil {
		t.Fatalf("SetApprovalStages: %v", err)
	}
	// A past deadline is rejected and the original state is untouched.
	past := time.Now().Add(-time.Hour)
	if err := l.SetStagedReviewDeadline("tx-d1", past); !errors.Is(err, ErrInvalidStagedReviewDeadline) {
		t.Errorf("past deadline: err = %v, want ErrInvalidStagedReviewDeadline", err)
	}
	if _, ok := l.StagedReviewDeadline("tx-d1"); ok {
		t.Error("past deadline install left a deadline behind")
	}
	rev, _ := l.GetTransferReview("tx-d1")
	if rev.StagedApproval == nil || len(rev.StagedApproval.Stages) != 3 || rev.StagedApproval.Stage != 0 {
		t.Errorf("stages after rejected install = %+v, want 3 stages at stage 0", rev.StagedApproval)
	}
	if err := l.SetStagedReviewDeadline("tx-d1", time.Time{}); !errors.Is(err, ErrInvalidStagedReviewDeadline) {
		t.Errorf("zero deadline: err = %v, want ErrInvalidStagedReviewDeadline", err)
	}
	deadline := time.Now().Add(time.Hour)
	if err := l.SetStagedReviewDeadline("tx-d1", deadline); err != nil {
		t.Fatalf("valid install: %v", err)
	}
	got, ok := l.StagedReviewDeadline("tx-d1")
	if !ok || got.UnixNano() != deadline.UnixNano() {
		t.Fatalf("StagedReviewDeadline = %v, ok=%v, want %v", got, ok, deadline)
	}
	// Re-setting replaces the deadline: an operator may extend the
	// bound before it lapses.
	later := time.Now().Add(2 * time.Hour)
	if err := l.SetStagedReviewDeadline("tx-d1", later); err != nil {
		t.Fatalf("re-set: %v", err)
	}
	got, _ = l.StagedReviewDeadline("tx-d1")
	if got.UnixNano() != later.UnixNano() {
		t.Errorf("after re-set: deadline = %v, want %v", got, later)
	}
	if _, ok := l.StagedReviewDeadline("tx-unknown"); ok {
		t.Error("StagedReviewDeadline on unknown transfer returned true")
	}
	// A review that is no longer pending refuses installation.
	l2 := stagedSeed(t, "tx-d2")
	if err := l2.SetApprovalStages("tx-d2", stagedRoles); err != nil {
		t.Fatalf("SetApprovalStages: %v", err)
	}
	for _, approver := range []string{"alice", "bob", "carol"} {
		if _, err := l2.ApproveTransferReview("tx-d2", approver); err != nil {
			t.Fatalf("stage approve: %v", err)
		}
	}
	if err := l2.SetStagedReviewDeadline("tx-d2", time.Now().Add(time.Hour)); !errors.Is(err, ErrReviewNotPending) {
		t.Errorf("settled review: err = %v, want ErrReviewNotPending", err)
	}
}

func TestStagedDeadlineLazyApproveRejects(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithReviewThreshold(10000))
	seedBalance(t, l, "payer", 100000)
	if _, err := l.PostTransfer(Transfer{ID: "tx-d3", From: "payer", To: "payee", AmountCents: 30000}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if err := l.SetApprovalStages("tx-d3", stagedRoles[:2]); err != nil {
		t.Fatalf("SetApprovalStages: %v", err)
	}
	if err := l.SetStagedReviewDeadline("tx-d3", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("SetStagedReviewDeadline: %v", err)
	}
	backdateStagedDeadline(t, l, "tx-d3", 2*time.Hour)

	// Approving past a lapsed deadline is refused, and the review is
	// rejected first — fail-closed, never advanced or settled.
	ctx := ContextWithActor(context.Background(), "ops-desk")
	if _, err := l.ApproveStagedReviewCtx(ctx, "tx-d3", "alice"); !errors.Is(err, ErrReviewExpired) {
		t.Fatalf("approve past deadline: err = %v, want ErrReviewExpired", err)
	}
	rev, _ := l.GetTransferReview("tx-d3")
	if rev.Status != ReviewStatusRejected {
		t.Errorf("status = %q, want rejected after deadline lapse", rev.Status)
	}
	if rev.ReviewedBy != "system" {
		t.Errorf("ReviewedBy = %q, want system", rev.ReviewedBy)
	}
	// The staged progress survives the deadline rejection: the trail
	// shows which stages and deadline were in force.
	if rev.StagedApproval == nil || len(rev.StagedApproval.Stages) != 2 || rev.StagedApproval.Deadline.IsZero() {
		t.Errorf("staged progress after deadline = %+v, want stages and deadline preserved", rev.StagedApproval)
	}
	if got := l.Available("payer"); got != 100000 {
		t.Errorf("payer available = %d, want 100000 (freeze released)", got)
	}
	if got := l.Balance("payee"); got != 0 {
		t.Errorf("payee balance = %d, want 0 (never settled)", got)
	}
	// A rejected review stays rejected: no second decision.
	if _, err := l.ApproveTransferReview("tx-d3", "alice"); !errors.Is(err, ErrReviewNotPending) {
		t.Errorf("approve after deadline rejection: err = %v, want ErrReviewNotPending", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	byOp := auditEventsByOp(t, dir)
	events := byOp["transfer_review_staged_deadline_expired"]
	if len(events) != 1 {
		t.Fatalf("deadline events = %d, want 1", len(events))
	}
	d := events[0].Details
	if d["stage"] != float64(0) || d["reason"] != "staged approval deadline expired" {
		t.Errorf("deadline event details = %+v, want stage 0 with deadline reason", d)
	}
	if events[0].Actor != "ops-desk" {
		t.Errorf("deadline event actor = %q, want ops-desk", events[0].Actor)
	}
}

func TestStagedDeadlineSweepRejectsRegardlessOfStage(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithReviewThreshold(10000))
	seedBalance(t, l, "payer", 100000)
	if _, err := l.PostTransfer(Transfer{ID: "tx-d4", From: "payer", To: "payee", AmountCents: 30000}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if err := l.SetApprovalStages("tx-d4", stagedRoles[:2]); err != nil {
		t.Fatalf("SetApprovalStages: %v", err)
	}
	// Advance to stage 1: the deadline must fire there too, not just
	// on the stage the review sat on when it was installed.
	if _, err := l.ApproveStagedReview("tx-d4", "alice"); err != nil {
		t.Fatalf("stage 0 approve: %v", err)
	}
	deadline := time.Now().Add(time.Hour)
	if err := l.SetStagedReviewDeadline("tx-d4", deadline); err != nil {
		t.Fatalf("SetStagedReviewDeadline: %v", err)
	}
	if n := l.ExpireStagedReviewsAt(deadline.Add(time.Hour)); n != 1 {
		t.Errorf("staged sweep past deadline = %d, want 1", n)
	}
	rev, _ := l.GetTransferReview("tx-d4")
	if rev.Status != ReviewStatusRejected || rev.ReviewedBy != "system" {
		t.Errorf("swept review = status %q reviewed_by %q, want rejected/system", rev.Status, rev.ReviewedBy)
	}
	if rev.StagedApproval == nil || rev.StagedApproval.Stage != 1 {
		t.Errorf("swept review stage = %+v, want stage 1 preserved", rev.StagedApproval)
	}
	if got := l.Available("payer"); got != 100000 {
		t.Errorf("payer available = %d, want 100000 (freeze released)", got)
	}
	// The explicit staged sweep is idempotent once rejected.
	if n := l.ExpireStagedReviewsAt(time.Now()); n != 0 {
		t.Errorf("second staged sweep = %d, want 0", n)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	byOp := auditEventsByOp(t, dir)
	events := byOp["transfer_review_staged_deadline_expired"]
	if len(events) != 1 {
		t.Fatalf("deadline events = %d, want 1", len(events))
	}
	ids := deadlineEventIDs(t, events[0].Details)
	if len(ids) != 1 || ids[0] != "tx-d4" {
		t.Errorf("deadline event ids = %v, want [tx-d4]", ids)
	}
	if events[0].Details["reason"] != "staged approval deadline expired" {
		t.Errorf("deadline event reason = %v, want deadline reason", events[0].Details["reason"])
	}
	// No stage timeout fired: no timeouts were installed.
	if len(byOp["transfer_review_staged_stage_timeout"]) != 0 {
		t.Error("stage timeout event fired without installed timeouts")
	}
}

func TestStagedDeadlineSweepAndLazyAvailable(t *testing.T) {
	l := stagedSeed(t, "tx-d5")
	if err := l.SetApprovalStages("tx-d5", stagedRoles[:2]); err != nil {
		t.Fatalf("SetApprovalStages: %v", err)
	}
	if err := l.SetStagedReviewDeadline("tx-d5", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("SetStagedReviewDeadline: %v", err)
	}
	backdateStagedDeadline(t, l, "tx-d5", 2*time.Hour)

	// Lazy by predicate: the freeze already released for
	// available-balance purposes and the pending list, before any
	// sweep runs.
	if got := l.Available("payer"); got != 100000 {
		t.Errorf("lazy available = %d, want 100000", got)
	}
	if pending := l.PendingTransferReviews(); len(pending) != 0 {
		t.Errorf("pending reviews = %d, want 0 (lapsed deadline excluded)", len(pending))
	}
	// The general sweep rejects it too, and the sweep count keeps
	// its LG-42 meaning: deadline rejections are rejections, not
	// expiries.
	if n := l.ExpireReviewsAt(time.Now().Add(time.Hour)); n != 0 {
		t.Errorf("ExpireReviewsAt returned %d, want 0 (deadline is a reject, not an expiry)", n)
	}
	rev, _ := l.GetTransferReview("tx-d5")
	if rev.Status != ReviewStatusRejected || rev.ReviewedBy != "system" {
		t.Errorf("swept review = status %q reviewed_by %q, want rejected/system", rev.Status, rev.ReviewedBy)
	}
	// The explicit staged sweep is idempotent once rejected.
	if n := l.ExpireStagedReviewsAt(time.Now()); n != 0 {
		t.Errorf("second staged sweep = %d, want 0", n)
	}

	// Control: same setup, but the sweep runs before the deadline —
	// the review stays pending and the freeze stays held.
	l2 := stagedSeed(t, "tx-d6")
	if err := l2.SetApprovalStages("tx-d6", stagedRoles[:2]); err != nil {
		t.Fatalf("SetApprovalStages: %v", err)
	}
	if err := l2.SetStagedReviewDeadline("tx-d6", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("SetStagedReviewDeadline: %v", err)
	}
	if n := l2.ExpireStagedReviewsAt(time.Now()); n != 0 {
		t.Errorf("early staged sweep = %d, want 0", n)
	}
	rev2, _ := l2.GetTransferReview("tx-d6")
	if rev2.Status != ReviewStatusPending {
		t.Errorf("status = %q, want pending (deadline not reached)", rev2.Status)
	}
	if got := l2.Available("payer"); got != 70000 {
		t.Errorf("payer available = %d, want 70000 (freeze still held)", got)
	}
}

func TestStagedDeadlineOrthogonalToStageTimeouts(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithReviewThreshold(10000))
	seedBalance(t, l, "payer", 100000)
	for _, id := range []string{"tx-d7", "tx-d8"} {
		if _, err := l.PostTransfer(Transfer{ID: id, From: "payer", To: "payee", AmountCents: 30000}); err != nil {
			t.Fatalf("PostTransfer %q: %v", id, err)
		}
	}
	// tx-d7: stage timeout lapsed, no deadline — the stage bound fires.
	if err := l.SetApprovalStages("tx-d7", stagedRoles[:2]); err != nil {
		t.Fatalf("SetApprovalStages tx-d7: %v", err)
	}
	if err := l.SetStageTimeouts("tx-d7", []time.Duration{time.Hour, time.Hour}); err != nil {
		t.Fatalf("SetStageTimeouts tx-d7: %v", err)
	}
	backdateStagedStage(t, l, "tx-d7", 2*time.Hour)
	// tx-d8: stage timeouts generous, overall deadline lapsed — the
	// deadline bound fires, on a stage whose own clock is fine.
	if err := l.SetApprovalStages("tx-d8", stagedRoles[:2]); err != nil {
		t.Fatalf("SetApprovalStages tx-d8: %v", err)
	}
	if err := l.SetStageTimeouts("tx-d8", []time.Duration{24 * time.Hour, 24 * time.Hour}); err != nil {
		t.Fatalf("SetStageTimeouts tx-d8: %v", err)
	}
	deadline := time.Now().Add(time.Hour)
	if err := l.SetStagedReviewDeadline("tx-d8", deadline); err != nil {
		t.Fatalf("SetStagedReviewDeadline tx-d8: %v", err)
	}
	if n := l.ExpireStagedReviewsAt(deadline.Add(time.Hour)); n != 2 {
		t.Errorf("staged sweep = %d, want 2 (one per bound)", n)
	}
	for _, id := range []string{"tx-d7", "tx-d8"} {
		rev, _ := l.GetTransferReview(id)
		if rev.Status != ReviewStatusRejected {
			t.Errorf("%s status = %q, want rejected", id, rev.Status)
		}
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	byOp := auditEventsByOp(t, dir)
	// Each bound emits its own batched event, naming only its own review.
	stageEvents := byOp["transfer_review_staged_stage_timeout"]
	if len(stageEvents) != 1 {
		t.Fatalf("stage timeout events = %d, want 1", len(stageEvents))
	}
	ids := deadlineEventIDs(t, stageEvents[0].Details)
	if len(ids) != 1 || ids[0] != "tx-d7" {
		t.Errorf("stage timeout ids = %v, want [tx-d7]", ids)
	}
	deadlineEvents := byOp["transfer_review_staged_deadline_expired"]
	if len(deadlineEvents) != 1 {
		t.Fatalf("deadline events = %d, want 1", len(deadlineEvents))
	}
	ids = deadlineEventIDs(t, deadlineEvents[0].Details)
	if len(ids) != 1 || ids[0] != "tx-d8" {
		t.Errorf("deadline ids = %v, want [tx-d8]", ids)
	}
}

func TestStagedDeadlineSnapshotRoundTrip(t *testing.T) {
	l := stagedSeed(t, "tx-d9")
	if err := l.SetApprovalStages("tx-d9", stagedRoles); err != nil {
		t.Fatalf("SetApprovalStages: %v", err)
	}
	deadline := time.Now().Add(24 * time.Hour)
	if err := l.SetStagedReviewDeadline("tx-d9", deadline); err != nil {
		t.Fatalf("SetStagedReviewDeadline: %v", err)
	}
	if _, err := l.ApproveStagedReview("tx-d9", "alice"); err != nil {
		t.Fatalf("stage 0 approve: %v", err)
	}
	before, _ := l.GetTransferReview("tx-d9")

	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	after, ok := restored.GetTransferReview("tx-d9")
	if !ok {
		t.Fatal("restored review missing")
	}
	if !stagedApprovalEqual(before.StagedApproval, after.StagedApproval) {
		t.Errorf("restored staged progress = %+v, want %+v", after.StagedApproval, before.StagedApproval)
	}
	got, ok := restored.StagedReviewDeadline("tx-d9")
	if !ok || got.UnixNano() != deadline.UnixNano() {
		t.Errorf("restored deadline = %v, ok=%v, want %v", got, ok, deadline)
	}
	if after.Status != ReviewStatusPending {
		t.Errorf("restored status = %q, want pending", after.Status)
	}
}
