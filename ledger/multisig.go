package ledger

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// N-of-M Ed25519 multi-signature approval for large operations.
//
// LG-142. review_quorum.go (LG-124) hardened the dual-control review
// with named N-of-M votes, but those votes are bearer assertions: the
// ledger trusts the caller-supplied approver identity. Multisig closes
// that gap for the highest-value operations (transfers, payouts,
// reversals): every approval is an Ed25519 signature over the
// operation's canonical bytes, verified against a registered approver
// key. A stolen operator session is no longer enough — the attacker
// would need N distinct approver private keys.
//
// The module is complementary to both earlier controls, not a
// replacement:
//   - LG-42 dual-control review is a sequential maker→checker flow on
//     threshold-triggered transfers; multisig is a parallel quorum on
//     explicitly submitted large operations.
//   - LG-124 review quorum is named votes on reviews; multisig is
//     cryptographic signatures on operations.
//
// Lifecycle: SubmitMultisigOperation validates the operation against
// the per-type policy, freezes the funds with a hold (see hold.go —
// the pending amount leaves *available* while the op is undecided),
// and parks the op in the pending_multisig queue. Approvers vote with
// VoteMultisig, signing the canonical bytes; N distinct approve
// signatures execute the operation through the standard write paths
// (PostTransfer / CreatePayout / ReverseEntry) with an idempotency key
// derived from the op ID, so a retry can never double-execute. One
// reject signature rejects the whole op immediately (classic
// four-eyes veto). Expiry is lazy on votes plus an explicit
// ExpireMultisigOperations sweep; expired ops release their hold and
// enter the audit trail.
//
// Signature binding (anti-replay / anti-transplant): the signed bytes
// are domain || opType || opID || from || amount || currency ||
// expiry || type-specific payload fields || vote. A signature cannot
// move to another operation (different opID), another vote direction
// (different vote byte), or a re-submitted op with different economics
// (different payload). The anchor-domain convention (see anchor.go)
// keeps multisig signatures disjoint from anchor signatures even when
// an operator reuses one key for both.

// Multisig operation types.
const (
	MultisigOpTransfer = "transfer"
	MultisigOpPayout   = "payout"
	MultisigOpReversal = "reversal"
)

// Multisig operation statuses.
const (
	MultisigStatusPending       = "pending"
	MultisigStatusApproved      = "approved"
	MultisigStatusRejected      = "rejected"
	MultisigStatusExpired       = "expired"
	MultisigStatusExecuting     = "executing"
	MultisigStatusExecuted      = "executed"
	MultisigStatusExecuteFailed = "execute_failed"
)

// Validation and state errors returned by the multisig APIs.
var (
	// ErrMultisigInvalidPolicy is returned for a policy that could
	// never be installed: unknown op type, threshold below 1 or above
	// the approver count, blank or duplicated approver ID, or an
	// approver with no registered key.
	ErrMultisigInvalidPolicy = errors.New("ledger: invalid multisig policy")
	// ErrMultisigPolicyMissing is returned by SubmitMultisigOperation
	// when no policy is configured for the operation's type.
	ErrMultisigPolicyMissing = errors.New("ledger: no multisig policy for operation type")
	// ErrMultisigApproverUnknown is returned when an approver ID has
	// no registered Ed25519 public key.
	ErrMultisigApproverUnknown = errors.New("ledger: multisig approver has no registered key")
	// ErrMultisigNotMember is returned when a vote names an approver
	// who is not in the operation type's policy.
	ErrMultisigNotMember = errors.New("ledger: approver is not a multisig policy member")
	// ErrMultisigOpNotFound is returned for an unknown operation ID.
	ErrMultisigOpNotFound = errors.New("ledger: multisig operation not found")
	// ErrMultisigNotPending is returned when voting on or executing an
	// operation that is no longer pending (or retrying one that is not
	// in an executable state).
	ErrMultisigNotPending = errors.New("ledger: multisig operation is not pending")
	// ErrMultisigExpired is returned when voting on an operation whose
	// expiry has passed. The op is swept to expired as part of the
	// call, so a second vote sees ErrMultisigNotPending instead.
	ErrMultisigExpired = errors.New("ledger: multisig operation has expired")
	// ErrMultisigBadSignature is returned when the Ed25519 signature
	// does not verify against the approver's registered key over the
	// operation's canonical bytes.
	ErrMultisigBadSignature = errors.New("ledger: multisig signature verification failed")
	// ErrMultisigAlreadyVoted is returned when an approver who already
	// voted tries to vote the opposite way. The first vote stands;
	// re-voting the same way is idempotent.
	ErrMultisigAlreadyVoted = errors.New("ledger: approver already voted on this multisig operation")
	// ErrMultisigInvalidOp is returned for a structurally invalid
	// operation submission: blank ID, bad type, non-positive amount,
	// missing type-specific payload, or an expiry that is not in the
	// future.
	ErrMultisigInvalidOp = errors.New("ledger: invalid multisig operation")
)

// multisigDomain separates multisig signatures from every other
// signature an approver key might produce (anchor checkpoints use
// LEDGER-ANCHOR/v1:), so a signature can never be transplanted across
// protocols even when one key serves both.
var multisigDomain = []byte("LEDGER-MULTISIG/v1:")

// MultisigPolicy is the N-of-M approval policy for one operation
// type: ThresholdN distinct approve signatures from the ApproverIDs
// member set execute the operation. TTL bounds how long a submitted op
// waits for its quorum; it is the default expiry when the submission
// carries none.
type MultisigPolicy struct {
	ThresholdN  int           `json:"threshold_n"`
	ApproverIDs []string      `json:"approver_ids"`
	TTL         time.Duration `json:"ttl"`
}

// MultisigVote is one recorded vote on a multisig operation: the
// approver identity, the direction, and when it was recorded. The
// signature itself is verified at vote time and not stored — the vote
// record plus the audit trail is the durable evidence.
type MultisigVote struct {
	ApproverID string    `json:"approver_id"`
	Approve    bool      `json:"approve"`
	VotedAt    time.Time `json:"voted_at"`
}

// MultisigOperation is one large operation awaiting (or having passed)
// N-of-M approval. Exactly one of Transfer, Payout, ReversalEntryID
// carries the type-specific payload; From/AmountCents/Currency are the
// hold dimensions (for reversals From names the account the reversal
// will debit — the original entry's credit account — since the entry
// itself is only read at execution).
type MultisigOperation struct {
	ID              string             `json:"id"`
	Type            string             `json:"type"`
	From            AccountID          `json:"from_account"`
	AmountCents     int64              `json:"amount_cents"`
	Currency        string             `json:"currency"`
	Transfer        *Transfer          `json:"transfer,omitempty"`
	Payout          *PayoutInstruction `json:"payout,omitempty"`
	ReversalEntryID string             `json:"reversal_entry_id,omitempty"`
	ExpiresAt       time.Time          `json:"expires_at"`
	CreatedAt       time.Time          `json:"created_at"`
	Status          string             `json:"status"`
	Votes           []MultisigVote     `json:"votes"`
	ExecutedAt      time.Time          `json:"executed_at,omitempty"`
	ExecutionNote   string             `json:"execution_note,omitempty"`
	Duplicate       bool               `json:"duplicate,omitempty"`
}

// multisigExecParams is the copied execution payload handed from the
// locked vote path to the unlocked executor: everything the executor
// needs, without holding the ledger lock across the write paths.
type multisigExecParams struct {
	id              string
	typ             string
	transfer        Transfer
	payout          PayoutInstruction
	reversalEntryID string
}

// RegisterMultisigApprover registers an approver's Ed25519 public key
// under an approver ID. Policies may only name registered approvers.
// Re-registering an ID replaces its key (key rotation); the replaced
// key's past votes stay on their operations — history is never
// rewritten.
func (l *Ledger) RegisterMultisigApprover(id string, pub ed25519.PublicKey) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("%w: approver ID must not be blank", ErrMultisigInvalidPolicy)
	}
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: approver %q: want %d-byte ed25519 public key, got %d",
			ErrMultisigInvalidPolicy, id, ed25519.PublicKeySize, len(pub))
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.multisigApprovers == nil {
		l.multisigApprovers = make(map[string]ed25519.PublicKey)
	}
	l.multisigApprovers[id] = bytes.Clone(pub)
	return nil
}

// SetMultisigPolicy installs the N-of-M policy for one operation
// type ("transfer", "payout", "reversal"): thresholdN distinct approve
// signatures from approverIDs execute a submitted op, ttl bounds the
// wait (a submission may still set its own earlier expiry). Every
// approver ID must already be registered. Installing is structural —
// it does not bump the ledger version.
func (l *Ledger) SetMultisigPolicy(opType string, p MultisigPolicy) error {
	if err := validateMultisigPolicy(opType, p, l.multisigApprovers); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.multisigPolicies == nil {
		l.multisigPolicies = make(map[string]MultisigPolicy)
	}
	l.multisigPolicies[opType] = MultisigPolicy{
		ThresholdN:  p.ThresholdN,
		ApproverIDs: append([]string(nil), p.ApproverIDs...),
		TTL:         p.TTL,
	}
	return nil
}

// validateMultisigPolicy checks a policy structurally. approvers is the
// registered-key map (may be nil when called pre-registration in
// tests); every named approver must have a key.
func validateMultisigPolicy(opType string, p MultisigPolicy, approvers map[string]ed25519.PublicKey) error {
	switch opType {
	case MultisigOpTransfer, MultisigOpPayout, MultisigOpReversal:
	default:
		return fmt.Errorf("%w: unknown operation type %q", ErrMultisigInvalidPolicy, opType)
	}
	if p.ThresholdN < 1 {
		return fmt.Errorf("%w: threshold_n must be >= 1, got %d", ErrMultisigInvalidPolicy, p.ThresholdN)
	}
	if len(p.ApproverIDs) == 0 {
		return fmt.Errorf("%w: needs at least one approver", ErrMultisigInvalidPolicy)
	}
	if p.ThresholdN > len(p.ApproverIDs) {
		return fmt.Errorf("%w: threshold_n %d exceeds approver count %d",
			ErrMultisigInvalidPolicy, p.ThresholdN, len(p.ApproverIDs))
	}
	if p.TTL <= 0 {
		return fmt.Errorf("%w: ttl must be positive, got %v", ErrMultisigInvalidPolicy, p.TTL)
	}
	seen := make(map[string]bool, len(p.ApproverIDs))
	for _, id := range p.ApproverIDs {
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("%w: approver ID must not be blank", ErrMultisigInvalidPolicy)
		}
		if seen[id] {
			return fmt.Errorf("%w: duplicate approver %q", ErrMultisigInvalidPolicy, id)
		}
		seen[id] = true
		if _, ok := approvers[id]; !ok {
			return fmt.Errorf("%w: %q", ErrMultisigApproverUnknown, id)
		}
	}
	return nil
}

// MultisigPolicy reports the active policy for an operation type. The
// returned policy is a copy. The second return value is false when no
// policy is configured.
func (l *Ledger) MultisigPolicy(opType string) (MultisigPolicy, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	p, ok := l.multisigPolicies[opType]
	if !ok {
		return MultisigPolicy{}, false
	}
	out := p
	out.ApproverIDs = append([]string(nil), p.ApproverIDs...)
	return out, true
}

// multisigCanonical builds the exact bytes an approver signs:
// domain || opType || 0x00 || opID || 0x00 || from || 0x00 ||
// BE64(amount) || currency || 0x00 || BE64(expiry unixnano) ||
// type-payload || voteByte. Fixed-width numerics, 0x00 separators, no
// JSON ambiguity — the same construction the verifier rebuilds, so any
// divergence fails closed.
func multisigCanonical(opType, opID string, from AccountID, amountCents int64, currency string, expiresAt time.Time, payload []byte, approve bool) []byte {
	msg := make([]byte, 0, len(multisigDomain)+len(payload)+64)
	msg = append(msg, multisigDomain...)
	msg = append(msg, opType...)
	msg = append(msg, 0x00)
	msg = append(msg, opID...)
	msg = append(msg, 0x00)
	msg = append(msg, string(from)...)
	msg = append(msg, 0x00)
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(amountCents))
	msg = append(msg, b[:]...)
	msg = append(msg, currency...)
	msg = append(msg, 0x00)
	binary.BigEndian.PutUint64(b[:], uint64(expiresAt.UTC().UnixNano()))
	msg = append(msg, b[:]...)
	msg = append(msg, payload...)
	if approve {
		msg = append(msg, 0x01)
	} else {
		msg = append(msg, 0x00)
	}
	return msg
}

// multisigPayloadBytes serializes the type-specific payload for the
// canonical form: transfer commits to the payee and transfer ID,
// payout to the payee, destination and payout ID, reversal to the
// target entry ID. An approver signs the economics, not just the
// amount — swapping the payee after signing fails verification.
func multisigPayloadBytes(op *MultisigOperation) []byte {
	var sb strings.Builder
	switch op.Type {
	case MultisigOpTransfer:
		if op.Transfer != nil {
			sb.WriteString(string(op.Transfer.To))
			sb.WriteByte(0x00)
			sb.WriteString(op.Transfer.ID)
			sb.WriteByte(0x00)
		}
	case MultisigOpPayout:
		if op.Payout != nil {
			sb.WriteString(string(op.Payout.PayeeAccount))
			sb.WriteByte(0x00)
			sb.WriteString(op.Payout.Destination)
			sb.WriteByte(0x00)
			sb.WriteString(op.Payout.ID)
			sb.WriteByte(0x00)
		}
	case MultisigOpReversal:
		sb.WriteString(op.ReversalEntryID)
		sb.WriteByte(0x00)
	}
	return []byte(sb.String())
}

// multisigCanonicalFor builds the canonical bytes for one vote on an
// op. Callers must hold l.mu (reads only).
func multisigCanonicalFor(op *MultisigOperation, approve bool) []byte {
	return multisigCanonical(op.Type, op.ID, op.From, op.AmountCents, op.Currency,
		op.ExpiresAt, multisigPayloadBytes(op), approve)
}

// validateMultisigOp checks a submission structurally and normalizes
// the currency. It does not touch the policy or the approver registry.
func validateMultisigOp(op *MultisigOperation, now time.Time) error {
	if strings.TrimSpace(op.ID) == "" {
		return fmt.Errorf("%w: operation ID must not be empty", ErrMultisigInvalidOp)
	}
	switch op.Type {
	case MultisigOpTransfer, MultisigOpPayout, MultisigOpReversal:
	default:
		return fmt.Errorf("%w: unknown operation type %q", ErrMultisigInvalidOp, op.Type)
	}
	if op.AmountCents <= 0 {
		return fmt.Errorf("%w: amount must be greater than zero", ErrMultisigInvalidOp)
	}
	if strings.TrimSpace(string(op.From)) == "" {
		return fmt.Errorf("%w: from account must not be empty", ErrMultisigInvalidOp)
	}
	currency, err := normalizeCurrency(op.Currency)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrMultisigInvalidOp, err)
	}
	op.Currency = currency
	if op.ExpiresAt.IsZero() {
		return fmt.Errorf("%w: expiry must be set (or submit with the policy TTL)", ErrMultisigInvalidOp)
	}
	if !op.ExpiresAt.After(now) {
		return fmt.Errorf("%w: expiry must be in the future", ErrMultisigInvalidOp)
	}
	switch op.Type {
	case MultisigOpTransfer:
		if op.Transfer == nil {
			return fmt.Errorf("%w: transfer payload is required", ErrMultisigInvalidOp)
		}
		if strings.TrimSpace(op.Transfer.ID) == "" {
			return fmt.Errorf("%w: transfer ID must not be empty", ErrMultisigInvalidOp)
		}
		if op.Transfer.From != op.From {
			return fmt.Errorf("%w: transfer from %q != operation from %q",
				ErrMultisigInvalidOp, op.Transfer.From, op.From)
		}
	case MultisigOpPayout:
		if op.Payout == nil {
			return fmt.Errorf("%w: payout payload is required", ErrMultisigInvalidOp)
		}
		if strings.TrimSpace(op.Payout.ID) == "" {
			return fmt.Errorf("%w: payout ID must not be empty", ErrMultisigInvalidOp)
		}
		if op.Payout.PayeeAccount != op.From {
			return fmt.Errorf("%w: payout payee %q != operation from %q",
				ErrMultisigInvalidOp, op.Payout.PayeeAccount, op.From)
		}
	case MultisigOpReversal:
		if strings.TrimSpace(op.ReversalEntryID) == "" {
			return fmt.Errorf("%w: reversal entry ID must not be empty", ErrMultisigInvalidOp)
		}
	}
	return nil
}

// SubmitMultisigOperation parks a large operation in the
// pending_multisig queue: it validates the op against the type's
// policy, freezes the amount on the from account with a hold (the
// pending amount leaves *available* until the op settles or dies), and
// records the submission in the audit chain. A submission with a
// duplicate ID replays the stored op idempotently. The hold is placed
// with the op's expiry so a forgotten op can never strand funds past
// its own deadline.
func (l *Ledger) SubmitMultisigOperation(ctx context.Context, op MultisigOperation) (*MultisigOperation, bool, error) {
	now := time.Now().UTC()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.multisigOps == nil {
		l.multisigOps = make(map[string]*MultisigOperation)
	}
	if existing, ok := l.multisigOps[op.ID]; ok {
		out := *existing
		out.Duplicate = true
		return &out, true, nil
	}
	policy, ok := l.multisigPolicies[op.Type]
	if !ok {
		return nil, false, fmt.Errorf("%w: %q", ErrMultisigPolicyMissing, op.Type)
	}
	if op.ExpiresAt.IsZero() {
		op.ExpiresAt = now.Add(policy.TTL)
	}
	if err := validateMultisigOp(&op, now); err != nil {
		return nil, false, err
	}
	// Freeze the funds: the hold ID is derived from the op ID so a
	// retried submission replays the same hold idempotently.
	if _, _, err := l.holdLocked(Hold{
		ID:             "multisig/" + op.ID,
		Account:        op.From,
		AmountCents:    op.AmountCents,
		Currency:       op.Currency,
		ExpiresAt:      op.ExpiresAt,
		CreatedAt:      now,
		IdempotencyKey: "multisig/" + op.ID,
	}); err != nil {
		return nil, false, fmt.Errorf("ledger: multisig fund freeze failed: %w", err)
	}
	stored := &MultisigOperation{
		ID:              op.ID,
		Type:            op.Type,
		From:            op.From,
		AmountCents:     op.AmountCents,
		Currency:        op.Currency,
		Transfer:        op.Transfer,
		Payout:          op.Payout,
		ReversalEntryID: op.ReversalEntryID,
		ExpiresAt:       op.ExpiresAt,
		CreatedAt:       now,
		Status:          MultisigStatusPending,
	}
	l.multisigOps[op.ID] = stored
	l.multisigSubmitted.Add(1)
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "multisig_submitted",
		Actor:         "SubmitMultisigOperation",
		TraceID:       op.ID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{op.From},
		Details: map[string]any{
			"op_type":     op.Type,
			"amount_cents": op.AmountCents,
			"currency":    op.Currency,
			"threshold_n": policy.ThresholdN,
			"expires_at":  op.ExpiresAt.UTC().Format(time.RFC3339),
		},
	})
	out := *stored
	return &out, false, nil
}

// GetMultisigOperation returns a copy of a multisig operation.
func (l *Ledger) GetMultisigOperation(id string) (*MultisigOperation, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	op, ok := l.multisigOps[id]
	if !ok {
		return nil, false
	}
	out := *op
	out.Votes = append([]MultisigVote(nil), op.Votes...)
	return &out, true
}

// multisigVoteCountLocked counts approve votes on an op. Callers must
// hold l.mu.
func multisigVoteCountLocked(op *MultisigOperation) int {
	n := 0
	for _, v := range op.Votes {
		if v.Approve {
			n++
		}
	}
	return n
}

// multisigFindVoteLocked returns the vote index for an approver, or -1.
// Callers must hold l.mu.
func multisigFindVoteLocked(op *MultisigOperation, approverID string) int {
	for i, v := range op.Votes {
		if v.ApproverID == approverID {
			return i
		}
	}
	return -1
}

// expireMultisigLocked sweeps one expired pending op: releases its
// hold, marks it expired, audits it, and counts it. Returns true when
// the op was pending and expired. Callers must hold the write lock.
func (l *Ledger) expireMultisigLocked(ctx context.Context, op *MultisigOperation, now time.Time) bool {
	if op.Status != MultisigStatusPending || !now.After(op.ExpiresAt) && !now.Equal(op.ExpiresAt) {
		return false
	}
	if _, err := l.releaseHoldLocked("multisig/" + op.ID); err != nil {
		// The hold release is best-effort: a missing hold (e.g.
		// manually released) must not block the expiry itself.
		_ = err
	}
	op.Status = MultisigStatusExpired
	l.multisigExpired.Add(1)
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "multisig_expired",
		Actor:         "ExpireMultisigOperations",
		TraceID:       op.ID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{op.From},
		Details: map[string]any{
			"op_type":      op.Type,
			"amount_cents": op.AmountCents,
			"currency":     op.Currency,
			"votes":        len(op.Votes),
		},
	})
	return true
}

// releaseHoldLocked releases a hold by ID, tolerating a missing or
// already-terminal hold. Callers must hold the write lock.
func (l *Ledger) releaseHoldLocked(id string) (Hold, error) {
	h, ok := l.holds[id]
	if !ok {
		return Hold{}, ErrHoldNotFound
	}
	if h.Status != HoldStatusActive {
		return h, nil
	}
	return l.releaseLocked(id)
}

// VoteMultisig records one approver's signed vote on a pending
// operation: the Ed25519 signature must verify against the approver's
// registered key over the operation's canonical bytes for this vote
// direction. An approve vote that reaches the policy threshold
// executes the operation (the execution runs outside the ledger lock
// through the standard write paths, with an op-derived idempotency
// key). A reject vote rejects the op immediately and releases the
// fund freeze. Re-voting the same way is idempotent; voting the
// opposite way fails — the first vote stands.
func (l *Ledger) VoteMultisig(ctx context.Context, opID, approverID string, approve bool, sig []byte) (*MultisigOperation, error) {
	now := time.Now().UTC()
	l.mu.Lock()
	op, ok := l.multisigOps[opID]
	if !ok {
		l.mu.Unlock()
		return nil, fmt.Errorf("%w: %q", ErrMultisigOpNotFound, opID)
	}
	if op.Status != MultisigStatusPending {
		l.mu.Unlock()
		return nil, fmt.Errorf("%w: %q is %s", ErrMultisigNotPending, opID, op.Status)
	}
	if l.expireMultisigLocked(ctx, op, now) {
		l.mu.Unlock()
		return nil, fmt.Errorf("%w: %q", ErrMultisigExpired, opID)
	}
	policy, ok := l.multisigPolicies[op.Type]
	if !ok {
		l.mu.Unlock()
		return nil, fmt.Errorf("%w: %q", ErrMultisigPolicyMissing, op.Type)
	}
	if !multisigPolicyHasApprover(policy, approverID) {
		l.mu.Unlock()
		return nil, fmt.Errorf("%w: %q", ErrMultisigNotMember, approverID)
	}
	pub, ok := l.multisigApprovers[approverID]
	if !ok {
		l.mu.Unlock()
		return nil, fmt.Errorf("%w: %q", ErrMultisigApproverUnknown, approverID)
	}
	if !ed25519.Verify(pub, multisigCanonicalFor(op, approve), sig) {
		l.mu.Unlock()
		return nil, ErrMultisigBadSignature
	}
	if i := multisigFindVoteLocked(op, approverID); i >= 0 {
		existing := op.Votes[i]
		l.mu.Unlock()
		if existing.Approve != approve {
			return nil, fmt.Errorf("%w: %q", ErrMultisigAlreadyVoted, approverID)
		}
		out, _ := l.GetMultisigOperation(opID)
		return out, nil
	}
	op.Votes = append(op.Votes, MultisigVote{ApproverID: approverID, Approve: approve, VotedAt: now})
	voteWord := "approve"
	if !approve {
		voteWord = "reject"
	}
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "multisig_vote",
		Actor:         "VoteMultisig",
		TraceID:       op.ID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{op.From},
		Details: map[string]any{
			"voter":         approverID,
			"vote":          voteWord,
			"approve_votes": multisigVoteCountLocked(op),
			"threshold_n":   policy.ThresholdN,
		},
	})
	var execParams *multisigExecParams
	if !approve {
		// One reject is decisive: release the freeze and close the op.
		if _, err := l.releaseHoldLocked("multisig/" + op.ID); err != nil {
			_ = err // best-effort, like expiry
		}
		op.Status = MultisigStatusRejected
		l.multisigRejected.Add(1)
		l.emitAuditCtx(ctx, AuditEvent{
			Op:            "multisig_rejected",
			Actor:         "VoteMultisig",
			TraceID:       op.ID,
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Accounts:      []AccountID{op.From},
			Details: map[string]any{
				"rejecter": approverID,
				"op_type":  op.Type,
			},
		})
	} else if multisigVoteCountLocked(op) >= policy.ThresholdN {
		// Quorum reached: release the freeze and execute outside the
		// lock. The op moves to approved first so a concurrent vote
		// cannot trigger a second execution.
		if _, err := l.releaseHoldLocked("multisig/" + op.ID); err != nil {
			_ = err // best-effort, like expiry
		}
		op.Status = MultisigStatusApproved
		l.multisigApproved.Add(1)
		l.emitAuditCtx(ctx, AuditEvent{
			Op:            "multisig_approved",
			Actor:         "VoteMultisig",
			TraceID:       op.ID,
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Accounts:      []AccountID{op.From},
			Details: map[string]any{
				"approvers":     multisigApproveVoters(op),
				"threshold_n":   policy.ThresholdN,
				"op_type":       op.Type,
				"amount_cents":  op.AmountCents,
				"currency":      op.Currency,
			},
		})
		execParams = op.executionParams()
	}
	l.mu.Unlock()
	if execParams != nil {
		l.runMultisigExecution(ctx, execParams)
	}
	out, _ := l.GetMultisigOperation(opID)
	return out, nil
}

// multisigPolicyHasApprover reports policy membership.
func multisigPolicyHasApprover(p MultisigPolicy, approverID string) bool {
	for _, id := range p.ApproverIDs {
		if id == approverID {
			return true
		}
	}
	return false
}

// multisigApproveVoters lists the approver IDs that voted approve, in
// vote order.
func multisigApproveVoters(op *MultisigOperation) []string {
	var out []string
	for _, v := range op.Votes {
		if v.Approve {
			out = append(out, v.ApproverID)
		}
	}
	return out
}

// executionParams copies the execution payload out of the op.
func (op *MultisigOperation) executionParams() *multisigExecParams {
	p := &multisigExecParams{id: op.ID, typ: op.Type}
	if op.Transfer != nil {
		t := *op.Transfer
		p.transfer = t
	}
	if op.Payout != nil {
		po := *op.Payout
		p.payout = po
	}
	p.reversalEntryID = op.ReversalEntryID
	return p
}

// runMultisigExecution executes an approved operation through the
// standard write paths with an op-derived idempotency key, then
// records the outcome. It is safe to call concurrently: only the
// winner of the approved→executing transition runs the write paths.
func (l *Ledger) runMultisigExecution(ctx context.Context, params *multisigExecParams) {
	l.mu.Lock()
	op, ok := l.multisigOps[params.id]
	if !ok || (op.Status != MultisigStatusApproved && op.Status != MultisigStatusExecuteFailed) {
		l.mu.Unlock()
		return
	}
	op.Status = MultisigStatusExecuting
	l.mu.Unlock()

	note, execErr := l.executeMultisigParams(params)

	l.mu.Lock()
	defer l.mu.Unlock()
	op, ok = l.multisigOps[params.id]
	if !ok {
		return
	}
	now := time.Now().UTC()
	if execErr != nil {
		op.Status = MultisigStatusExecuteFailed
		op.ExecutionNote = execErr.Error()
		l.emitAuditCtx(ctx, AuditEvent{
			Op:            "multisig_execute_failed",
			Actor:         "VoteMultisig",
			TraceID:       params.id,
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Accounts:      []AccountID{op.From},
			Details: map[string]any{
				"error": execErr.Error(),
			},
		})
		return
	}
	op.Status = MultisigStatusExecuted
	op.ExecutedAt = now
	op.ExecutionNote = note
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "multisig_executed",
		Actor:         "VoteMultisig",
		TraceID:       params.id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{op.From},
		Details: map[string]any{
			"result": note,
		},
	})
}

// executeMultisigParams runs the operation through the standard write
// paths. It must not hold l.mu: the write paths take it themselves.
// Every path uses the op-derived idempotency key, so a retried
// execution replays the original result instead of double-booking.
func (l *Ledger) executeMultisigParams(params *multisigExecParams) (string, error) {
	key := "multisig/" + params.id
	switch params.typ {
	case MultisigOpTransfer:
		t := params.transfer
		t.IdempotencyKey = key
		receipt, err := l.PostTransfer(t)
		if err != nil {
			return "", fmt.Errorf("multisig execute transfer: %w", err)
		}
		return fmt.Sprintf("transfer %s review=%s", receipt.TransferID, receipt.ReviewStatus), nil
	case MultisigOpPayout:
		p := params.payout
		p.IdempotencyKey = key
		inst, _, err := l.CreatePayout(p)
		if err != nil {
			return "", fmt.Errorf("multisig execute payout: %w", err)
		}
		return fmt.Sprintf("payout %s status=%s", inst.ID, inst.Status), nil
	case MultisigOpReversal:
		reversal, _, err := l.ReverseEntry(params.reversalEntryID, key)
		if err != nil {
			return "", fmt.Errorf("multisig execute reversal: %w", err)
		}
		return fmt.Sprintf("reversal %s", reversal.ID), nil
	default:
		return "", fmt.Errorf("multisig execute: unknown operation type %q", params.typ)
	}
}

// ExecuteMultisigOperation retries the execution of an approved (or
// previously failed) operation: the quorum stands, only the write-path
// execution is re-attempted. It is a no-op returning the current op
// when the op already executed.
func (l *Ledger) ExecuteMultisigOperation(ctx context.Context, opID string) (*MultisigOperation, error) {
	l.mu.Lock()
	op, ok := l.multisigOps[opID]
	if !ok {
		l.mu.Unlock()
		return nil, fmt.Errorf("%w: %q", ErrMultisigOpNotFound, opID)
	}
	switch op.Status {
	case MultisigStatusExecuted:
		out := *op
		l.mu.Unlock()
		return &out, nil
	case MultisigStatusApproved, MultisigStatusExecuteFailed:
		// Proceed to execution below.
	default:
		l.mu.Unlock()
		return nil, fmt.Errorf("%w: %q is %s", ErrMultisigNotPending, opID, op.Status)
	}
	params := op.executionParams()
	l.mu.Unlock()
	l.runMultisigExecution(ctx, params)
	out, _ := l.GetMultisigOperation(opID)
	return out, nil
}

// ExpireMultisigOperations sweeps pending operations whose expiry has
// passed: each releases its fund freeze, enters the audit trail as
// expired, and counts toward ledger_multisig_expired_total. Returns
// the number of operations expired.
func (l *Ledger) ExpireMultisigOperations(ctx context.Context, now time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, op := range l.multisigOps {
		if l.expireMultisigLocked(ctx, op, now) {
			n++
		}
	}
	return n
}

// MultisigSubmitted reports how many operations entered the
// pending_multisig queue (first-time submissions only). Synced to
// /metrics as ledger_multisig_submitted_total.
func (l *Ledger) MultisigSubmitted() uint64 {
	return l.multisigSubmitted.Load()
}

// MultisigApproved reports how many operations reached quorum.
// Synced to /metrics as ledger_multisig_approved_total.
func (l *Ledger) MultisigApproved() uint64 {
	return l.multisigApproved.Load()
}

// MultisigRejected reports how many operations a reject vote killed.
// Synced to /metrics as ledger_multisig_rejected_total.
func (l *Ledger) MultisigRejected() uint64 {
	return l.multisigRejected.Load()
}

// MultisigExpired reports how many operations lapsed without quorum.
// Synced to /metrics as ledger_multisig_expired_total.
func (l *Ledger) MultisigExpired() uint64 {
	return l.multisigExpired.Load()
}

// multisigReportFingerprint returns the SHA-256 hex of the canonical
// bytes for diagnostics (never logged with the signature itself).
func multisigReportFingerprint(op *MultisigOperation, approve bool) string {
	sum := sha256.Sum256(multisigCanonicalFor(op, approve))
	return hex.EncodeToString(sum[:])
}
