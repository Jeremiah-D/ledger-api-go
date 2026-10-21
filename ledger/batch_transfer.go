package ledger

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// Atomic batch transfer posting (LG-160, fintech: 批量工资/批量结算).
//
// PostBatchTransfer commits several transfers in one atomic step: the
// payroll / bulk-settlement view of the transfer API. Every transfer is
// an ordinary PostTransfer (see postTransferLocked): field validation,
// fee resolution, FX conversion, idempotency replay, frozen/overdraft/
// daily-limit/period risk controls, and the dual-control review gate.
// The batch is the unit of atomicity — either every transfer lands or
// none do.
//
// Atomicity is two-phase, reusing the dry-run twin pattern (see
// cloneForDryRunLocked, LG-36):
//
//  1. The write lock is taken once for the whole batch.
//  2. Every transfer runs postTransferLocked against a private deep
//     copy of the ledger, in batch order. The twin executes the exact
//     production code path — validation, denylist, replay, frozen,
//     overdraft, daily-limit, budget, period, review — so any rejection
//     the live commit would hit is caught here first, with zero records
//     on the live ledger.
//  3. Only if every transfer commits on the twin does the live ledger
//     commit them, in the same order, through the same function.
//
// The twin pass is a faithful predictor of the live commit: between
// the two passes nothing can interleave (the write lock is held the
// whole time), and every time-dependent check is either pinned to the
// transfers' finalized CreatedAt (filled once before the twin pass, so
// both passes see identical timestamps) or only grows more permissive
// with time (expired freezes release lazily; FX rates cannot move
// under the lock). A transfer the twin accepts is a transfer the live
// commit accepts — the live pass cannot fail after the twin pass
// succeeded, so there is never a half-committed batch to roll back.
//
// Idempotency is two-level, mirroring PostBatch (see batch.go). The
// batch-level key (TransferBatch.IdempotencyKey) lives in its own
// namespace (batchTransferKeys): reposting it returns the whole
// original receipt with Duplicate == true and books nothing new. Each
// transfer's own IdempotencyKey shares the transfer key namespace with
// PostTransfer: a key already posted — by PostTransfer or by an
// earlier batch — replays the original receipt, which is returned in
// the batch receipt without being booked again. A batch may freely mix
// fresh transfers and replays. Two batch transfers sharing a key that
// is not yet journaled are ambiguous and reject the whole batch
// (ErrBatchTransferDuplicateIdempotencyKey) — the same rule PostBatch
// applies to entry keys.
//
// Dual-control review transfers participate like any other: a transfer
// at or above its payer's review threshold journals its legs with the
// pending-review marker (LG-42), exactly as PostTransfer would. The
// batch commits them atomically with the settled ones.
//
// A rejected batch records nothing: no journal rows, no idempotency
// registrations, no chain links, no version bump. Replays — at either
// level — record nothing either.

// Validation errors returned by PostBatchTransfer.
var (
	// ErrEmptyTransferBatchID is returned when the batch carries no ID.
	ErrEmptyTransferBatchID = errors.New("ledger: transfer batch ID must not be empty")
	// ErrEmptyTransferBatch is returned when the batch names no transfers.
	ErrEmptyTransferBatch = errors.New("ledger: transfer batch must contain at least one transfer")
	// ErrBatchTransferDuplicateID is returned when two transfers in one
	// batch share a transfer ID: the second would collide with the
	// first's journaled entry IDs ("<id>", "<id>/fee"), so the batch is
	// ambiguous and rejected.
	ErrBatchTransferDuplicateID = errors.New("ledger: transfer batch contains duplicate transfer ID")
	// ErrBatchTransferDuplicateIdempotencyKey is returned when two
	// transfers in one batch share an idempotency key that is not yet
	// journaled: the key could only ever resolve to one of them, so the
	// batch is ambiguous and rejected. (Two transfers whose key replays
	// an already-posted transfer both resolve to that original — that
	// is not a conflict.)
	ErrBatchTransferDuplicateIdempotencyKey = errors.New("ledger: transfer batch contains duplicate transfer idempotency key")
)

// TransferBatch is one atomic multi-transfer posting: the payroll /
// bulk-settlement request. Transfers commit in slice order; a zero
// CreatedAt on the batch or on a transfer is filled once with the batch
// post time, before the twin validation pass, so the twin and the live
// commit see identical timestamps (daily-limit day buckets, monthly
// budgets, and FX effective-version lookups are deterministic across
// the two phases).
type TransferBatch struct {
	ID             string     `json:"batch_id"`
	Transfers      []Transfer `json:"transfers"`
	IdempotencyKey string     `json:"idempotency_key,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
}

// BatchTransferReceipt reports what PostBatchTransfer committed.
//
// Receipts holds every transfer's receipt in batch order: freshly
// posted transfers carry their journal entries (per-transfer leg
// detail, principal + fee + FX legs), and replayed transfers carry
// the originally posted receipt. Duplicate replays return the
// originally posted receipts with Duplicate == true and book nothing
// new.
type BatchTransferReceipt struct {
	BatchID    string            `json:"batch_id"`
	Receipts   []TransferReceipt `json:"receipts"`
	Duplicate  bool              `json:"duplicate"`
}

// batchTransferRecord remembers what a batch posted so idempotent
// replays can rebuild the receipt. Batch keys live in their own
// namespace (batchTransferKeys), independent of the entry/transfer/
// batch key namespaces, and expire with the TTL like every other key
// index (see pruneBatchTransferKeysLocked).
type batchTransferRecord struct {
	batchID     string
	transferIDs []string
	createdAt   time.Time
}

// PostBatchTransfer atomically posts every transfer of tb. See the
// package comment for the two-phase atomicity contract and the
// two-level idempotency semantics. A zero tb.CreatedAt is filled with
// the current time; transfers with a zero CreatedAt inherit the batch's
// post time.
func (l *Ledger) PostBatchTransfer(tb TransferBatch) (BatchTransferReceipt, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.postBatchTransferLocked(tb)
}

// postBatchTransferLocked is PostBatchTransfer's body. Callers must
// hold the write lock.
func (l *Ledger) postBatchTransferLocked(tb TransferBatch) (BatchTransferReceipt, error) {
	if tb.ID == "" {
		return BatchTransferReceipt{}, ErrEmptyTransferBatchID
	}
	if len(tb.Transfers) == 0 {
		return BatchTransferReceipt{}, ErrEmptyTransferBatch
	}

	now := time.Now()
	transfers := make([]Transfer, len(tb.Transfers))
	copy(transfers, tb.Transfers)
	for i := range transfers {
		if transfers[i].CreatedAt.IsZero() {
			transfers[i].CreatedAt = now
		}
	}

	// 1. Duplicate transfer IDs inside the batch: the second would
	// collide with the first's "<id>"/"<id>/fee" journal entries, so
	// the batch is ambiguous and rejected before anything is recorded.
	// (A transfer ID colliding with an already-journaled entry is
	// caught by the twin pass as ErrTransferIDConflict.)
	seenIDs := make(map[string]bool, len(transfers))
	for i := range transfers {
		if seenIDs[transfers[i].ID] {
			return BatchTransferReceipt{}, ErrBatchTransferDuplicateID
		}
		seenIDs[transfers[i].ID] = true
	}

	// 2. Duplicate fresh idempotency keys inside the batch: two new
	// transfers sharing a key that is not yet journaled could only
	// ever resolve to one of them. Keys that replay an already-posted
	// transfer resolve to that original and are not conflicts — the
	// same rule PostBatch applies to entry keys.
	freshKeys := make(map[string]bool, len(transfers))
	for i := range transfers {
		key := transfers[i].IdempotencyKey
		if key == "" {
			continue
		}
		if _, posted := l.byKey[key]; posted {
			continue
		}
		if _, posted := l.transferKeys[key]; posted {
			continue
		}
		if freshKeys[key] {
			return BatchTransferReceipt{}, ErrBatchTransferDuplicateIdempotencyKey
		}
		freshKeys[key] = true
	}

	// 3. Sanctions/denylist screening (see denylist.go): every
	// transfer's payer and payee are screened before the batch-level
	// idempotency check below — a hit rejects the whole batch and
	// leaves no idempotency trace, and the attempt is audited as
	// "denylist_rejected". The fee leg's revenue account is deliberately
	// not screened, the same exemption PostTransfer applies.
	for i := range transfers {
		if acct, entry, denied := l.denylistRejectedLocked(transfers[i].From, transfers[i].To); denied {
			return BatchTransferReceipt{}, l.deniedRejectionLocked("PostBatchTransfer", tb.ID, acct, entry, []AccountID{transfers[i].From, transfers[i].To})
		}
	}

	// 4. Batch-level idempotency: a replayed key returns the whole
	// original receipt, before any transfer-level work — the replay
	// books nothing new, so it must not fail on state that changed
	// since the original posting.
	if tb.IdempotencyKey != "" {
		if rec, ok := l.batchTransferKeys[tb.IdempotencyKey]; ok {
			receipts := make([]TransferReceipt, 0, len(rec.transferIDs))
			for _, id := range rec.transferIDs {
				receipts = append(receipts, l.replayBatchTransferEntryLocked(id))
			}
			return BatchTransferReceipt{BatchID: rec.batchID, Receipts: receipts, Duplicate: true}, nil
		}
	}

	// 5. Twin validation pass: run every transfer through the exact
	// production code path on a private deep copy of the ledger, in
	// batch order. Any rejection — validation, denylist, ID conflict,
	// frozen, overdraft, daily-limit, budget, period, FX — aborts the
	// whole batch here, with zero records on the live ledger. The
	// twin is disposable: its commits advance only its own copy.
	// Audit and outbox are nil on the twin (see cloneForDryRunLocked),
	// so the pass emits no audit events and no domain events; balance
	// hooks are unset on the twin, so no notifications fire.
	twin := l.cloneForDryRunLocked()
	for i := range transfers {
		if _, err := twin.postTransferLocked(transfers[i], FXRateQuote{}); err != nil {
			return BatchTransferReceipt{}, err
		}
	}

	// 6. Atomic commit: every transfer commits through the same
	// function the twin validated, in the same order, under the same
	// write lock. The twin pass is a faithful predictor (see the
	// package comment): the live pass cannot fail after it succeeded,
	// so there is nothing to roll back.
	versionBefore := l.version
	receipts := make([]TransferReceipt, 0, len(transfers))
	committedIDs := make([]string, 0, len(transfers))
	for i := range transfers {
		receipt, err := l.postTransferLocked(transfers[i], FXRateQuote{})
		if err != nil {
			// Unreachable by construction (see the package comment):
			// the twin pass already proved every transfer commits.
			// Reported, never silently partial.
			return BatchTransferReceipt{}, fmt.Errorf("ledger: batch transfer commit failed after twin validation: %w", err)
		}
		receipts = append(receipts, receipt)
		committedIDs = append(committedIDs, transfers[i].ID)
	}
	if tb.IdempotencyKey != "" {
		l.batchTransferKeys[tb.IdempotencyKey] = batchTransferRecord{
			batchID:     tb.ID,
			transferIDs: committedIDs,
			createdAt:   now,
		}
	}
	l.emitAudit(AuditEvent{
		Op:            "post_transfer_batch",
		Actor:         "PostBatchTransfer",
		TraceID:       tb.ID,
		VersionBefore: versionBefore,
		VersionAfter:  l.version,
		EntryIDs:      committedIDs,
		Details: map[string]any{
			"transfers": len(committedIDs),
		},
	})
	return BatchTransferReceipt{BatchID: tb.ID, Receipts: receipts, Duplicate: false}, nil
}

// replayBatchTransferEntryLocked rebuilds one transfer's receipt for a
// batch-level idempotent replay: the receipt is re-derived from the
// journaled legs, exactly like replayTransferLocked, so the replay
// books nothing new. Callers must hold l.mu.
func (l *Ledger) replayBatchTransferEntryLocked(transferID string) TransferReceipt {
	entries := []JournalEntry{}
	if e, ok := l.entries[transferID]; ok {
		entries = append(entries, e)
	}
	// Fee legs are journaled as "<transfer ID>/fee" (and fee-split
	// "<transfer ID>/fee/<i>" legs, FX "<transfer ID>/fx" legs); the
	// replay collects them by prefix so the receipt discloses exactly
	// what the original posted. Leg IDs sort for a deterministic
	// receipt — map iteration order is not.
	prefix := transferID + "/"
	var legIDs []string
	for id := range l.entries {
		if len(id) > len(prefix) && id[:len(prefix)] == prefix {
			legIDs = append(legIDs, id)
		}
	}
	sort.Strings(legIDs)
	for _, id := range legIDs {
		entries = append(entries, l.entries[id])
	}
	feeCents := feeCentsOf(transferID, entries)
	return TransferReceipt{
		TransferID: transferID,
		Entries:    entries,
		FeeCents:   feeCents,
		Duplicate:  true,
		ReviewStatus: l.reviewStatusLocked(transferID),
	}
}

// pruneBatchTransferKeysLocked evicts transfer-batch idempotency keys
// older than the TTL, mirroring the entry-level key expiry: after the
// TTL, reposting a batch key books a brand-new batch. The journaled
// batch transfers themselves are untouched — only the replay-detection
// index shrinks. A non-positive TTL disables pruning. Callers must
// hold l.mu (the write lock; the batch mutates the index).
func (l *Ledger) pruneBatchTransferKeysLocked(now time.Time) int {
	if l.idempotencyTTL <= 0 {
		return 0
	}
	cutoff := now.Add(-l.idempotencyTTL)
	removed := 0
	for key, rec := range l.batchTransferKeys {
		if rec.createdAt.Before(cutoff) {
			delete(l.batchTransferKeys, key)
			removed++
		}
	}
	return removed
}
