package ledger

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"
)

// Multisig approver revocation (LG-354): the departure / key-compromise
// counterpart to registration — fail-closed, history-preserving, and
// audited without key material.

// multisigRevokedAuditEvents returns the multisig_approver_revoked
// events in order, closing the audit log first (like the other amend
// tests).
func multisigRevokedAuditEvents(t *testing.T, al *AuditLog) []AuditEvent {
	t.Helper()
	var out []AuditEvent
	for _, e := range providerAuditEvents(t, al) {
		if e.Op == "multisig_approver_revoked" {
			out = append(out, e)
		}
	}
	return out
}

// newMultisigRevokeTestLedger builds a ledger with an audit log, like
// newAnchorKeyTestLedger. The caller must not use the audit log after
// multisigRevokedAuditEvents closes it.
func newMultisigRevokeTestLedger(t *testing.T) (*Ledger, *AuditLog) {
	t.Helper()
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { al.Close() }) // idempotent: safe after providerAuditEvents closes it
	return New(WithAuditLog(al)), al
}

func TestRevokeMultisigApproverUnknownIDKeepsRegistry(t *testing.T) {
	l := New()
	ids, keys := multisigTestApprovers(t, l, 3)
	if err := l.SetMultisigPolicy(MultisigOpTransfer, multisigPolicy2of3(ids)); err != nil {
		t.Fatalf("SetMultisigPolicy: %v", err)
	}
	if err := l.RevokeMultisigApprover("ghost"); !errors.Is(err, ErrMultisigApproverUnknown) {
		t.Fatalf("RevokeMultisigApprover(ghost) = %v, want ErrMultisigApproverUnknown", err)
	}
	// Every registered approver still votes: the failed revoke changed
	// nothing.
	op := multisigTransferOp(t, l, "revoke-unknown-op")
	if _, _, err := l.SubmitMultisigOperation(context.Background(), op); err != nil {
		t.Fatalf("SubmitMultisigOperation: %v", err)
	}
	if _, err := l.VoteMultisig(context.Background(), op.ID, ids[0], true, multisigSign(t, l, op.ID, keys[0], true)); err != nil {
		t.Fatalf("VoteMultisig after failed revoke = %v, want nil", err)
	}
}

func TestRevokeMultisigApproverRefusedWhileNamedByPolicy(t *testing.T) {
	l := New()
	ids, keys := multisigTestApprovers(t, l, 3)
	if err := l.SetMultisigPolicy(MultisigOpTransfer, multisigPolicy2of3(ids)); err != nil {
		t.Fatalf("SetMultisigPolicy: %v", err)
	}
	if err := l.RevokeMultisigApprover(ids[0]); !errors.Is(err, ErrMultisigInvalidPolicy) {
		t.Fatalf("RevokeMultisigApprover(policy member) = %v, want ErrMultisigInvalidPolicy", err)
	}
	// Still registered: the refused revoke changed nothing.
	op := multisigTransferOp(t, l, "revoke-policy-op")
	if _, _, err := l.SubmitMultisigOperation(context.Background(), op); err != nil {
		t.Fatalf("SubmitMultisigOperation: %v", err)
	}
	if _, err := l.VoteMultisig(context.Background(), op.ID, ids[0], true, multisigSign(t, l, op.ID, keys[0], true)); err != nil {
		t.Fatalf("VoteMultisig after refused revoke = %v, want nil", err)
	}
}

func TestRevokeMultisigApproverKeepsHistoryAndPendingVotes(t *testing.T) {
	l := New()
	ids, keys := multisigTestApprovers(t, l, 3)
	if err := l.SetMultisigPolicy(MultisigOpTransfer, multisigPolicy2of3(ids)); err != nil {
		t.Fatalf("SetMultisigPolicy: %v", err)
	}
	op := multisigTransferOp(t, l, "revoke-history-op")
	if _, _, err := l.SubmitMultisigOperation(context.Background(), op); err != nil {
		t.Fatalf("SubmitMultisigOperation: %v", err)
	}
	// One approve vote from ids[0]: the op stays pending (1 of 2).
	if _, err := l.VoteMultisig(context.Background(), op.ID, ids[0], true, multisigSign(t, l, op.ID, keys[0], true)); err != nil {
		t.Fatalf("VoteMultisig: %v", err)
	}
	// Remove ids[0] from the policy, then revoke: the op's recorded
	// vote must survive both steps.
	if err := l.SetMultisigPolicy(MultisigOpTransfer, MultisigPolicy{ThresholdN: 1, ApproverIDs: ids[1:], TTL: time.Hour}); err != nil {
		t.Fatalf("SetMultisigPolicy (drop approver): %v", err)
	}
	if err := l.RevokeMultisigApprover(ids[0]); err != nil {
		t.Fatalf("RevokeMultisigApprover: %v", err)
	}
	stored, ok := l.GetMultisigOperation(op.ID)
	if !ok {
		t.Fatalf("op %q vanished after revoke", op.ID)
	}
	if stored.Status != MultisigStatusPending {
		t.Fatalf("op status = %q, want pending", stored.Status)
	}
	if len(stored.Votes) != 1 || stored.Votes[0].ApproverID != ids[0] || !stored.Votes[0].Approve {
		t.Fatalf("op votes = %+v, want the single retained approve vote from %q", stored.Votes, ids[0])
	}
	// The retained vote still counts: one more approve reaches the
	// (revised) threshold and executes the op.
	if _, err := l.VoteMultisig(context.Background(), op.ID, ids[1], true, multisigSign(t, l, op.ID, keys[1], true)); err != nil {
		t.Fatalf("VoteMultisig by remaining approver: %v", err)
	}
	stored, _ = l.GetMultisigOperation(op.ID)
	if stored.Status != MultisigStatusExecuted {
		t.Fatalf("op status = %q, want executed (retained vote counted)", stored.Status)
	}
}

func TestRevokeMultisigApproverVoteRefusedAfterRevoke(t *testing.T) {
	l := New()
	ids, keys := multisigTestApprovers(t, l, 3)
	if err := l.SetMultisigPolicy(MultisigOpTransfer, multisigPolicy2of3(ids)); err != nil {
		t.Fatalf("SetMultisigPolicy: %v", err)
	}
	if err := l.SetMultisigPolicy(MultisigOpTransfer, MultisigPolicy{ThresholdN: 1, ApproverIDs: ids[1:], TTL: time.Hour}); err != nil {
		t.Fatalf("SetMultisigPolicy (drop approver): %v", err)
	}
	if err := l.RevokeMultisigApprover(ids[0]); err != nil {
		t.Fatalf("RevokeMultisigApprover: %v", err)
	}
	op := multisigTransferOp(t, l, "revoke-vote-op")
	if _, _, err := l.SubmitMultisigOperation(context.Background(), op); err != nil {
		t.Fatalf("SubmitMultisigOperation: %v", err)
	}
	// The revoked ID is no longer in the policy and no longer
	// registered: voting is refused (policy membership is checked
	// first, so the refusal surfaces as not-a-member).
	if _, err := l.VoteMultisig(context.Background(), op.ID, ids[0], true, multisigSign(t, l, op.ID, keys[0], true)); !errors.Is(err, ErrMultisigNotMember) {
		t.Fatalf("VoteMultisig by revoked approver = %v, want ErrMultisigNotMember", err)
	}
}

func TestRevokeMultisigApproverReregisterRestores(t *testing.T) {
	l := New()
	ids, keys := multisigTestApprovers(t, l, 3)
	if err := l.SetMultisigPolicy(MultisigOpTransfer, multisigPolicy2of3(ids)); err != nil {
		t.Fatalf("SetMultisigPolicy: %v", err)
	}
	if err := l.SetMultisigPolicy(MultisigOpTransfer, MultisigPolicy{ThresholdN: 1, ApproverIDs: ids[1:], TTL: time.Hour}); err != nil {
		t.Fatalf("SetMultisigPolicy (drop approver): %v", err)
	}
	if err := l.RevokeMultisigApprover(ids[0]); err != nil {
		t.Fatalf("RevokeMultisigApprover: %v", err)
	}
	// Re-register the same ID under a fresh key and put it back in
	// the policy: the approver votes again, under the new key.
	_, newPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.RegisterMultisigApprover(ids[0], newPriv.Public().(ed25519.PublicKey)); err != nil {
		t.Fatalf("RegisterMultisigApprover after revoke: %v", err)
	}
	if err := l.SetMultisigPolicy(MultisigOpTransfer, MultisigPolicy{ThresholdN: 1, ApproverIDs: ids[:1], TTL: time.Hour}); err != nil {
		t.Fatalf("SetMultisigPolicy (restore approver): %v", err)
	}
	op := multisigTransferOp(t, l, "revoke-reregister-op")
	if _, _, err := l.SubmitMultisigOperation(context.Background(), op); err != nil {
		t.Fatalf("SubmitMultisigOperation: %v", err)
	}
	if _, err := l.VoteMultisig(context.Background(), op.ID, ids[0], true, multisigSign(t, l, op.ID, newPriv, true)); err != nil {
		t.Fatalf("VoteMultisig with new key after re-register: %v", err)
	}
	// The old key no longer verifies for the restored approver.
	op2 := multisigTransferOp(t, l, "revoke-reregister-op2")
	if _, _, err := l.SubmitMultisigOperation(context.Background(), op2); err != nil {
		t.Fatalf("SubmitMultisigOperation: %v", err)
	}
	if _, err := l.VoteMultisig(context.Background(), op2.ID, ids[0], true, multisigSign(t, l, op2.ID, keys[0], true)); !errors.Is(err, ErrMultisigBadSignature) {
		t.Fatalf("VoteMultisig with old key = %v, want ErrMultisigBadSignature", err)
	}
}

func TestRevokeMultisigApproverAudit(t *testing.T) {
	l, al := newMultisigRevokeTestLedger(t)
	ids, _ := multisigTestApprovers(t, l, 2)
	// A failed revoke writes no audit event.
	if err := l.RevokeMultisigApprover("ghost"); err == nil {
		t.Fatalf("RevokeMultisigApprover(ghost) = nil, want error")
	}
	if err := l.RevokeMultisigApproverCtx(context.Background(), ids[0]); err != nil {
		t.Fatalf("RevokeMultisigApproverCtx: %v", err)
	}
	events := multisigRevokedAuditEvents(t, al)
	if len(events) != 1 {
		t.Fatalf("multisig_approver_revoked events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Details["approver"] != ids[0] {
		t.Fatalf("audit approver = %v, want %q", ev.Details["approver"], ids[0])
	}
	if len(ev.Details) != 1 {
		t.Fatalf("audit details = %v, want only the approver ID (never key material)", ev.Details)
	}
}

func TestRevokeMultisigApproverCtxAttribution(t *testing.T) {
	l, al := newMultisigRevokeTestLedger(t)
	ids, _ := multisigTestApprovers(t, l, 1)
	ctx := ContextWithActor(context.Background(), "ops-lead")
	if err := l.RevokeMultisigApproverCtx(ctx, ids[0]); err != nil {
		t.Fatalf("RevokeMultisigApproverCtx: %v", err)
	}
	events := multisigRevokedAuditEvents(t, al)
	if len(events) != 1 {
		t.Fatalf("multisig_approver_revoked events = %d, want 1", len(events))
	}
	if events[0].Actor != "ops-lead" {
		t.Fatalf("audit actor = %q, want %q (ctx attribution)", events[0].Actor, "ops-lead")
	}
}
