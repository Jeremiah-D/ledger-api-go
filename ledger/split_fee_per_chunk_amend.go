package ledger

import (
	"context"
)

// Split fee-per-chunk amendment (LG-291).
//
// WithSplitFeePerChunk installs the per-chunk fee mode at construction;
// this module is the orthogonal runtime path: an operator revises the
// mode in place, instead of rebuilding the ledger and replaying its
// journal. Revising in place matters because a rebuild is a new
// ledger — a gap in which the old fee mode (or no per-chunk mode)
// governs large payouts, plus a fresh identity that breaks the audit
// story — while an amendment keeps the same ledger, the same journal,
// and the same audit trail from old mode to new.
//
// Semantics, deliberately minimal throughout, along the LG-241/242
// structural-revision paradigm:
//
//   - The new mode takes effect immediately for every later
//     PostTransferSplit call, because the split path reads the live
//     field, never a cached copy: splitFeeForLocked reads
//     l.splitFeePerChunk at call time under the write lock
//     (transfer_split.go). No split-path code change was needed —
//     the revision is clean exactly because the reader was already
//     live.
//   - A bool has no invalid value — every value installs — so the
//     setter returns nothing and cannot fail (the LG-288
//     SetReviewExpiry shape: any value installs, no error return).
//   - Amending to the current mode is a no-op: no state change, no
//     audit event. The mode is already exactly what the operator
//     asked for.
//   - Only future splits change mode. A split already committed keeps
//     its journaled fee legs exactly as booked — its receipt is a
//     historical record, never rewritten — and an in-flight split
//     cannot exist across the amendment: PostTransferSplit holds the
//     write lock from entry, and the amendment takes the same lock,
//     so a split either commits wholly under the old mode or wholly
//     under the new one. A replayed idempotency key likewise keeps
//     returning the original receipt, not a re-fee'd one.
//   - An explicit FeeCents is always charged once per transfer,
//     whatever the mode: the amendment never changes that rule, and
//     neither do SkipFee transfers or a disabled fee policy.
//   - The amendment is structural, like SetDailyLimit: it moves no
//     money, so it bumps neither the ledger version nor the audit
//     chain, and no outbox event fires. It lands one
//     split_fee_per_chunk_amended audit event carrying the previous
//     and new modes. The amended mode rides the config line
//     (split_fee_per_chunk) through snapshots exactly like a
//     construction-time mode — no snapshot change is needed.
//
// Orthogonality: WithSplitFeePerChunk owns the construction-time
// mode, SetMaxTransferChunkCents (LG-290) owns the runtime revision
// of the chunk-size cap (a size control, not a fee mode),
// WithTransferFeePolicy owns the fee rate table itself, and this
// module owns only the runtime revision of the per-chunk fee mode.
// There is no HTTP endpoint for the revision (per LG-241/242).

// SetSplitFeePerChunk revises the per-chunk fee mode for
// PostTransferSplit in place. Only transfers posted after the call
// are fee'd under the new mode; already-posted splits keep their
// journaled fee legs. Amending to the current mode is a no-op. The
// revision is audited as split_fee_per_chunk_amended.
func (l *Ledger) SetSplitFeePerChunk(enabled bool) {
	l.SetSplitFeePerChunkCtx(context.Background(), enabled)
}

// SetSplitFeePerChunkCtx is SetSplitFeePerChunk with per-call audit
// actor attribution (LG-73).
func (l *Ledger) SetSplitFeePerChunkCtx(ctx context.Context, enabled bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	previous := l.splitFeePerChunk
	if previous == enabled {
		return
	}
	l.splitFeePerChunk = enabled
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "split_fee_per_chunk_amended",
		Actor:         "SetSplitFeePerChunk",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"previous_enabled": previous,
			"enabled":          enabled,
		},
	})
}
