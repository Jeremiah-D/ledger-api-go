package ledger

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"
)

// multisigTestApprovers registers n approvers and returns their IDs and
// private keys. IDs are "ms-approver-0".."ms-approver-(n-1)".
func multisigTestApprovers(t *testing.T, l *Ledger, n int) ([]string, []ed25519.PrivateKey) {
	t.Helper()
	ids := make([]string, n)
	keys := make([]ed25519.PrivateKey, n)
	for i := 0; i < n; i++ {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatalf("generate key: %v", err)
		}
		ids[i] = "ms-approver-" + string(rune('0'+i))
		if err := l.RegisterMultisigApprover(ids[i], pub); err != nil {
			t.Fatalf("register approver: %v", err)
		}
		keys[i] = priv
	}
	return ids, keys
}

// multisigSign signs the canonical bytes for one vote on the stored op.
func multisigSign(t *testing.T, l *Ledger, opID string, key ed25519.PrivateKey, approve bool) []byte {
	t.Helper()
	op, ok := l.GetMultisigOperation(opID)
	if !ok {
		t.Fatalf("op %q not found for signing", opID)
	}
	return ed25519.Sign(key, multisigCanonicalFor(op, approve))
}

// multisigTransferOp builds a funded transfer op fixture.
func multisigTransferOp(t *testing.T, l *Ledger, id string) MultisigOperation {
	t.Helper()
	fundAccount(t, l, "ms-payer", 100000, "")
	return MultisigOperation{
		ID:          id,
		Type:        MultisigOpTransfer,
		From:        "ms-payer",
		AmountCents: 60000,
		Currency:    "",
		Transfer: &Transfer{
			ID:     "ms-xfer-" + id,
			From:   "ms-payer",
			To:     "ms-payee",
			AmountCents: 60000,
		},
	}
}

func multisigPolicy2of3(ids []string) MultisigPolicy {
	return MultisigPolicy{ThresholdN: 2, ApproverIDs: ids, TTL: time.Hour}
}

func TestMultisigPolicyValidation(t *testing.T) {
	l := New()
	ids, _ := multisigTestApprovers(t, l, 3)

	cases := []struct {
		name   string
		opType string
		pol    MultisigPolicy
	}{
		{"bad type", "wire", multisigPolicy2of3(ids)},
		{"zero threshold", MultisigOpTransfer, MultisigPolicy{ThresholdN: 0, ApproverIDs: ids, TTL: time.Hour}},
		{"threshold exceeds members", MultisigOpTransfer, MultisigPolicy{ThresholdN: 4, ApproverIDs: ids, TTL: time.Hour}},
		{"no approvers", MultisigOpTransfer, MultisigPolicy{ThresholdN: 1, ApproverIDs: nil, TTL: time.Hour}},
		{"duplicate approver", MultisigOpTransfer, MultisigPolicy{ThresholdN: 1, ApproverIDs: []string{ids[0], ids[0]}, TTL: time.Hour}},
		{"unregistered approver", MultisigOpTransfer, MultisigPolicy{ThresholdN: 1, ApproverIDs: []string{"ghost"}, TTL: time.Hour}},
		{"zero ttl", MultisigOpTransfer, MultisigPolicy{ThresholdN: 1, ApproverIDs: ids[:1], TTL: 0}},
	}
	for _, c := range cases {
		if err := l.SetMultisigPolicy(c.opType, c.pol); err == nil {
			t.Errorf("%s: SetMultisigPolicy succeeded, want error", c.name)
		} else if !errors.Is(err, ErrMultisigInvalidPolicy) && !errors.Is(err, ErrMultisigApproverUnknown) {
			t.Errorf("%s: err = %v, want ErrMultisigInvalidPolicy/ErrMultisigApproverUnknown", c.name, err)
		}
	}
	if err := l.SetMultisigPolicy(MultisigOpTransfer, multisigPolicy2of3(ids)); err != nil {
		t.Fatalf("valid policy: %v", err)
	}
	if _, _, err := l.SubmitMultisigOperation(context.Background(), multisigTransferOp(t, l, "t1")); err != nil {
		t.Fatalf("submit after valid policy: %v", err)
	}
}

func TestMultisigTransferQuorumExecutes(t *testing.T) {
	l := New()
	ids, keys := multisigTestApprovers(t, l, 3)
	if err := l.SetMultisigPolicy(MultisigOpTransfer, multisigPolicy2of3(ids)); err != nil {
		t.Fatalf("policy: %v", err)
	}
	ctx := context.Background()
	op, dup, err := l.SubmitMultisigOperation(ctx, multisigTransferOp(t, l, "q1"))
	if err != nil || dup {
		t.Fatalf("submit = dup=%v err=%v", dup, err)
	}
	if op.Status != MultisigStatusPending {
		t.Fatalf("status = %q, want pending", op.Status)
	}
	// The pending op freezes available funds via a hold.
	if got := l.Available("ms-payer"); got != 40000 {
		t.Fatalf("Available after submit = %d, want 40000", got)
	}
	// First approve: recorded, still pending.
	if _, err := l.VoteMultisig(ctx, "q1", ids[0], true, multisigSign(t, l, "q1", keys[0], true)); err != nil {
		t.Fatalf("vote 1: %v", err)
	}
	op, _ = l.GetMultisigOperation("q1")
	if op.Status != MultisigStatusPending {
		t.Fatalf("status after 1 vote = %q, want pending", op.Status)
	}
	// Second approve: quorum reached, transfer executes.
	if _, err := l.VoteMultisig(ctx, "q1", ids[1], true, multisigSign(t, l, "q1", keys[1], true)); err != nil {
		t.Fatalf("vote 2: %v", err)
	}
	op, _ = l.GetMultisigOperation("q1")
	if op.Status != MultisigStatusExecuted {
		t.Fatalf("status = %q, want executed", op.Status)
	}
	if got := l.Balance("ms-payee"); got != 60000 {
		t.Errorf("payee balance = %d, want 60000", got)
	}
	if got := l.Balance("ms-payer"); got != 40000 {
		t.Errorf("payer balance = %d, want 40000", got)
	}
	// The freeze hold is released: available equals balance again.
	if got := l.Available("ms-payer"); got != 40000 {
		t.Errorf("Available after execution = %d, want 40000", got)
	}
	if l.MultisigSubmitted() != 1 || l.MultisigApproved() != 1 {
		t.Errorf("counters submitted=%d approved=%d, want 1/1", l.MultisigSubmitted(), l.MultisigApproved())
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Errorf("accounting equation: %v", err)
	}
}

func TestMultisigBadSignature(t *testing.T) {
	l := New()
	ids, _ := multisigTestApprovers(t, l, 3)
	if err := l.SetMultisigPolicy(MultisigOpTransfer, multisigPolicy2of3(ids)); err != nil {
		t.Fatalf("policy: %v", err)
	}
	ctx := context.Background()
	if _, _, err := l.SubmitMultisigOperation(ctx, multisigTransferOp(t, l, "b1")); err != nil {
		t.Fatalf("submit: %v", err)
	}
	// Signed by a different key than the claimed approver.
	_, otherKey, _ := ed25519.GenerateKey(rand.Reader)
	sig := multisigSign(t, l, "b1", otherKey, true)
	if _, err := l.VoteMultisig(ctx, "b1", ids[0], true, sig); !errors.Is(err, ErrMultisigBadSignature) {
		t.Errorf("wrong-key vote err = %v, want ErrMultisigBadSignature", err)
	}
}

func TestMultisigSignatureTransplantRejected(t *testing.T) {
	l := New()
	ids, keys := multisigTestApprovers(t, l, 3)
	if err := l.SetMultisigPolicy(MultisigOpTransfer, multisigPolicy2of3(ids)); err != nil {
		t.Fatalf("policy: %v", err)
	}
	ctx := context.Background()
	if _, _, err := l.SubmitMultisigOperation(ctx, multisigTransferOp(t, l, "x1")); err != nil {
		t.Fatalf("submit x1: %v", err)
	}
	if _, _, err := l.SubmitMultisigOperation(ctx, multisigTransferOp(t, l, "x2")); err != nil {
		t.Fatalf("submit x2: %v", err)
	}
	// A signature minted for x1 must not verify on x2.
	sig := multisigSign(t, l, "x1", keys[0], true)
	if _, err := l.VoteMultisig(ctx, "x2", ids[0], true, sig); !errors.Is(err, ErrMultisigBadSignature) {
		t.Errorf("transplanted vote err = %v, want ErrMultisigBadSignature", err)
	}
}

func TestMultisigVoteDirectionBound(t *testing.T) {
	l := New()
	ids, keys := multisigTestApprovers(t, l, 3)
	if err := l.SetMultisigPolicy(MultisigOpTransfer, multisigPolicy2of3(ids)); err != nil {
		t.Fatalf("policy: %v", err)
	}
	ctx := context.Background()
	if _, _, err := l.SubmitMultisigOperation(ctx, multisigTransferOp(t, l, "d1")); err != nil {
		t.Fatalf("submit: %v", err)
	}
	// Approve bytes replayed as a reject vote must fail verification.
	sig := multisigSign(t, l, "d1", keys[0], true)
	if _, err := l.VoteMultisig(ctx, "d1", ids[0], false, sig); !errors.Is(err, ErrMultisigBadSignature) {
		t.Errorf("direction-swapped vote err = %v, want ErrMultisigBadSignature", err)
	}
}

func TestMultisigRejectReleasesFreeze(t *testing.T) {
	l := New()
	ids, keys := multisigTestApprovers(t, l, 3)
	if err := l.SetMultisigPolicy(MultisigOpTransfer, multisigPolicy2of3(ids)); err != nil {
		t.Fatalf("policy: %v", err)
	}
	ctx := context.Background()
	if _, _, err := l.SubmitMultisigOperation(ctx, multisigTransferOp(t, l, "r1")); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := l.VoteMultisig(ctx, "r1", ids[0], true, multisigSign(t, l, "r1", keys[0], true)); err != nil {
		t.Fatalf("approve vote: %v", err)
	}
	if _, err := l.VoteMultisig(ctx, "r1", ids[1], false, multisigSign(t, l, "r1", keys[1], false)); err != nil {
		t.Fatalf("reject vote: %v", err)
	}
	op, _ := l.GetMultisigOperation("r1")
	if op.Status != MultisigStatusRejected {
		t.Fatalf("status = %q, want rejected", op.Status)
	}
	if got := l.Available("ms-payer"); got != 100000 {
		t.Errorf("Available after reject = %d, want 100000", got)
	}
	if l.MultisigRejected() != 1 {
		t.Errorf("rejected counter = %d, want 1", l.MultisigRejected())
	}
	// Voting on a rejected op fails.
	if _, err := l.VoteMultisig(ctx, "r1", ids[2], true, multisigSign(t, l, "r1", keys[2], true)); !errors.Is(err, ErrMultisigNotPending) {
		t.Errorf("vote on rejected op err = %v, want ErrMultisigNotPending", err)
	}
}

func TestMultisigExpirySweep(t *testing.T) {
	l := New()
	ids, _ := multisigTestApprovers(t, l, 3)
	if err := l.SetMultisigPolicy(MultisigOpTransfer, multisigPolicy2of3(ids)); err != nil {
		t.Fatalf("policy: %v", err)
	}
	ctx := context.Background()
	if _, _, err := l.SubmitMultisigOperation(ctx, multisigTransferOp(t, l, "e1")); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if n := l.ExpireMultisigOperations(ctx, time.Now().Add(2*time.Hour)); n != 1 {
		t.Fatalf("expired = %d, want 1", n)
	}
	op, _ := l.GetMultisigOperation("e1")
	if op.Status != MultisigStatusExpired {
		t.Fatalf("status = %q, want expired", op.Status)
	}
	if got := l.Available("ms-payer"); got != 100000 {
		t.Errorf("Available after expiry = %d, want 100000", got)
	}
	if l.MultisigExpired() != 1 {
		t.Errorf("expired counter = %d, want 1", l.MultisigExpired())
	}
	// A vote on an op whose expiry passed reports expiry (lazy path).
	pub2, priv2, _ := ed25519.GenerateKey(rand.Reader)
	if err := l.RegisterMultisigApprover("late", pub2); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := l.SetMultisigPolicy(MultisigOpTransfer, MultisigPolicy{ThresholdN: 1, ApproverIDs: []string{"late"}, TTL: time.Hour}); err != nil {
		t.Fatalf("policy: %v", err)
	}
	opE2 := multisigTransferOp(t, l, "e2")
	opE2.ExpiresAt = time.Now().Add(time.Millisecond)
	if _, _, err := l.SubmitMultisigOperation(ctx, opE2); err != nil {
		t.Fatalf("submit e2: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	op2, _ := l.GetMultisigOperation("e2")
	sig := ed25519.Sign(priv2, multisigCanonicalFor(op2, true))
	if _, err := l.VoteMultisig(ctx, "e2", "late", true, sig); !errors.Is(err, ErrMultisigExpired) {
		t.Errorf("vote on expired op err = %v, want ErrMultisigExpired", err)
	}
}

func TestMultisigVoteIdempotencyAndFlip(t *testing.T) {
	l := New()
	ids, keys := multisigTestApprovers(t, l, 3)
	if err := l.SetMultisigPolicy(MultisigOpTransfer, multisigPolicy2of3(ids)); err != nil {
		t.Fatalf("policy: %v", err)
	}
	ctx := context.Background()
	if _, _, err := l.SubmitMultisigOperation(ctx, multisigTransferOp(t, l, "i1")); err != nil {
		t.Fatalf("submit: %v", err)
	}
	sig := multisigSign(t, l, "i1", keys[0], true)
	if _, err := l.VoteMultisig(ctx, "i1", ids[0], true, sig); err != nil {
		t.Fatalf("vote 1: %v", err)
	}
	// Same vote again: idempotent, still one vote.
	if _, err := l.VoteMultisig(ctx, "i1", ids[0], true, sig); err != nil {
		t.Fatalf("duplicate vote: %v", err)
	}
	op, _ := l.GetMultisigOperation("i1")
	if len(op.Votes) != 1 {
		t.Fatalf("votes = %d, want 1", len(op.Votes))
	}
	// Opposite vote: the first vote stands.
	if _, err := l.VoteMultisig(ctx, "i1", ids[0], false, multisigSign(t, l, "i1", keys[0], false)); !errors.Is(err, ErrMultisigAlreadyVoted) {
		t.Errorf("flipped vote err = %v, want ErrMultisigAlreadyVoted", err)
	}
}

func TestMultisigNonMemberAndUnknownOp(t *testing.T) {
	l := New()
	ids, keys := multisigTestApprovers(t, l, 3)
	if err := l.SetMultisigPolicy(MultisigOpTransfer, MultisigPolicy{ThresholdN: 1, ApproverIDs: ids[:2], TTL: time.Hour}); err != nil {
		t.Fatalf("policy: %v", err)
	}
	ctx := context.Background()
	if _, _, err := l.SubmitMultisigOperation(ctx, multisigTransferOp(t, l, "n1")); err != nil {
		t.Fatalf("submit: %v", err)
	}
	// Registered but not in this policy.
	if _, err := l.VoteMultisig(ctx, "n1", ids[2], true, multisigSign(t, l, "n1", keys[2], true)); !errors.Is(err, ErrMultisigNotMember) {
		t.Errorf("non-member vote err = %v, want ErrMultisigNotMember", err)
	}
	if _, err := l.VoteMultisig(ctx, "nope", ids[0], true, []byte("x")); !errors.Is(err, ErrMultisigOpNotFound) {
		t.Errorf("unknown op err = %v, want ErrMultisigOpNotFound", err)
	}
}

func TestMultisigReversalQuorum(t *testing.T) {
	l := New()
	ids, keys := multisigTestApprovers(t, l, 3)
	if err := l.SetMultisigPolicy(MultisigOpReversal, MultisigPolicy{ThresholdN: 1, ApproverIDs: ids[:1], TTL: time.Hour}); err != nil {
		t.Fatalf("policy: %v", err)
	}
	ctx := context.Background()
	fundAccount(t, l, "rv-a", 50000, "")
	// rv-a pays rv-b 20000: debit rv-b / credit rv-a (debit increases).
	orig, _, err := l.Post(JournalEntry{
		ID: "rv-orig", DebitAccount: "rv-b", CreditAccount: "rv-a",
		AmountCents: 20000, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	// The reversal debits rv-a (the original credit account): the
	// multisig freeze lands there.
	op := MultisigOperation{
		ID: "rev1", Type: MultisigOpReversal, From: "rv-a",
		AmountCents: 20000, Currency: "", ReversalEntryID: orig.ID,
	}
	if _, _, err := l.SubmitMultisigOperation(ctx, op); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := l.VoteMultisig(ctx, "rev1", ids[0], true, multisigSign(t, l, "rev1", keys[0], true)); err != nil {
		t.Fatalf("vote: %v", err)
	}
	stored, _ := l.GetMultisigOperation("rev1")
	if stored.Status != MultisigStatusExecuted {
		t.Fatalf("status = %q, want executed", stored.Status)
	}
	// The reversal fully unwinds the payment: rv-a back to 50000,
	// rv-b back to 0.
	if got := l.Balance("rv-a"); got != 50000 {
		t.Errorf("rv-a balance = %d, want 50000 after reversal", got)
	}
	if got := l.Balance("rv-b"); got != 0 {
		t.Errorf("rv-b balance = %d, want 0 after reversal", got)
	}
}

func TestMultisigDuplicateSubmitAndRetry(t *testing.T) {
	l := New()
	ids, keys := multisigTestApprovers(t, l, 3)
	if err := l.SetMultisigPolicy(MultisigOpTransfer, MultisigPolicy{ThresholdN: 1, ApproverIDs: ids[:1], TTL: time.Hour}); err != nil {
		t.Fatalf("policy: %v", err)
	}
	ctx := context.Background()
	if _, dup, err := l.SubmitMultisigOperation(ctx, multisigTransferOp(t, l, "s1")); err != nil || dup {
		t.Fatalf("submit 1 = dup=%v err=%v", dup, err)
	}
	op, dup, err := l.SubmitMultisigOperation(ctx, multisigTransferOp(t, l, "s1"))
	if err != nil || !dup {
		t.Fatalf("submit 2 = dup=%v err=%v, want duplicate replay", dup, err)
	}
	if !op.Duplicate {
		t.Errorf("replayed op missing Duplicate flag")
	}
	// Execute via quorum, then retry the execution entrypoint: no-op.
	if _, err := l.VoteMultisig(ctx, "s1", ids[0], true, multisigSign(t, l, "s1", keys[0], true)); err != nil {
		t.Fatalf("vote: %v", err)
	}
	op, err = l.ExecuteMultisigOperation(ctx, "s1")
	if err != nil {
		t.Fatalf("retry execute: %v", err)
	}
	if op.Status != MultisigStatusExecuted {
		t.Errorf("status = %q, want executed", op.Status)
	}
	if got := l.Balance("ms-payee"); got != 60000 {
		t.Errorf("payee balance = %d, want 60000 (no double-execution)", got)
	}
}

func TestMultisigMissingPolicy(t *testing.T) {
	l := New()
	if _, _, err := l.SubmitMultisigOperation(context.Background(), multisigTransferOp(t, l, "p1")); !errors.Is(err, ErrMultisigPolicyMissing) {
		t.Errorf("submit without policy err = %v, want ErrMultisigPolicyMissing", err)
	}
}
