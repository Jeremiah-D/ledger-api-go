package ledger

import (
	"context"
	"errors"
	"fmt"
)

// Max transfer chunk cap amendment (LG-290).
//
// WithMaxTransferChunkCents (LG-75) installs the opt-in single-transfer
// cap for PostTransferSplit at construction; this module is the
// orthogonal runtime path: an operator revises the cap in place,
// instead of rebuilding the ledger and replaying its journal.
// Revising in place matters because a rebuild is a new ledger — a gap
// in which the old cap (or no cap) governs large payouts, plus a fresh
// identity that breaks the audit story — while an amendment keeps the
// same ledger, the same journal, and the same audit trail from old cap
// to new.
//
// Semantics, deliberately fail-closed throughout, along the LG-241/242
// structural-revision paradigm:
//
//   - The new cap takes effect immediately for every later
//     PostTransferSplit call, because the split path reads the live
//     field, never a cached copy: splitChunkSizeLocked reads
//     l.maxTransferChunkCents at call time under the write lock. No
//     split-path code change was needed — the revision is clean
//     exactly because the reader was already live.
//   - A negative cap is rejected with
//     ErrInvalidMaxTransferChunkCents and the previous cap is left
//     exactly as it was — a runtime revision must never silently
//     normalize, unlike the construction option, which panics on a
//     negative cap. The operator fixes the value and retries; the
//     ledger never runs on a half-installed cap.
//   - Zero disables chunking, exactly as at construction: a transfer
//     posted after a 0 amendment delegates to PostTransfer and books
//     a single chunk, whatever its amount (subject to the payer's
//     daily outflow limit sizing, as before).
//   - Amending to the current cap is a no-op: no state change, no
//     audit event. The cap is already exactly what the operator
//     asked for.
//   - Only future splits are resized. A split already committed keeps
//     its journaled chunk entries exactly as booked — its receipt is
//     a historical record, never rewritten — and an in-flight split
//     cannot exist across the amendment: PostTransferSplit holds the
//     write lock from entry, and the amendment takes the same lock,
//     so a split either commits wholly under the old cap or wholly
//     under the new one. A replayed idempotency key likewise keeps
//     returning the original receipt, not a re-chunked one.
//   - The amendment is structural, like SetDailyLimit: it moves no
//     money, so it bumps neither the ledger version nor the audit
//     chain, and no outbox event fires. It lands one
//     max_transfer_chunk_cents_amended audit event carrying the
//     previous and new caps. The amended cap rides the config line
//     (max_transfer_chunk_cents) through snapshots exactly like a
//     construction-time cap — no snapshot change is needed.
//
// Orthogonality: WithMaxTransferChunkCents owns the
// construction-time cap, WithSplitFeePerChunk owns the per-chunk fee
// policy (a fee strategy, not a size cap), SetDailyLimit owns the
// payer's daily outflow limit (a different control that also sizes
// chunks through the tighter-of rule, see splitChunkSizeLocked),
// and this module owns only the runtime revision of the chunk cap
// itself. There is no HTTP endpoint for the revision (per LG-241/242).

// Max transfer chunk cap amendment errors.
var (
	// ErrInvalidMaxTransferChunkCents is returned by
	// SetMaxTransferChunkCents when the proposed cap is negative:
	// a runtime revision must never silently normalize a bad cap.
	// The previous cap is left unchanged.
	ErrInvalidMaxTransferChunkCents = errors.New("ledger: max transfer chunk must not be negative")
)

// SetMaxTransferChunkCents revises the opt-in single-transfer cap for
// PostTransferSplit in place. Only transfers posted after the call
// are chunked under the new cap; already-posted splits are untouched.
// A negative cap fails with ErrInvalidMaxTransferChunkCents and the
// previous cap is unchanged. Zero disables chunking (the construction
// default). Amending to the current cap is a no-op. The revision is
// audited as max_transfer_chunk_cents_amended.
func (l *Ledger) SetMaxTransferChunkCents(chunkCents int64) error {
	return l.SetMaxTransferChunkCentsCtx(context.Background(), chunkCents)
}

// SetMaxTransferChunkCentsCtx is SetMaxTransferChunkCents with
// per-call audit actor attribution (LG-73).
func (l *Ledger) SetMaxTransferChunkCentsCtx(ctx context.Context, chunkCents int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.setMaxTransferChunkCentsLocked(ctx, chunkCents)
}

// setMaxTransferChunkCentsLocked validates and installs a chunk cap.
// Callers must hold l.mu. Validation runs before the no-op check and
// before any mutation, so a rejected cap leaves the previous cap
// exactly as it was.
func (l *Ledger) setMaxTransferChunkCentsLocked(ctx context.Context, chunkCents int64) error {
	if chunkCents < 0 {
		return fmt.Errorf("%w: chunk cap %d", ErrInvalidMaxTransferChunkCents, chunkCents)
	}
	previous := l.maxTransferChunkCents
	if chunkCents == previous {
		return nil
	}
	l.maxTransferChunkCents = chunkCents
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "max_transfer_chunk_cents_amended",
		Actor:         "SetMaxTransferChunkCents",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"previous_chunk_cents": previous,
			"chunk_cents":          chunkCents,
		},
	})
	return nil
}
