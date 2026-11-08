package ledger

import (
	"context"
	"errors"
	"fmt"
)

// Dispute representment evidence-package cap runtime revision (LG-357).
//
// One representment's evidence package is capped at a number of
// document hashes: enough for a receipt, a delivery proof, and a
// contract, few enough to keep the record bounded. The cap was a
// construction-time constant (32): a complex dispute whose evidence
// chain needs more than 32 document hashes in one submission could
// only be filed after a code change and redeploy, and tightening the
// cap against evidence-package abuse was equally a redeploy. This
// module is the runtime path: an operator revises the live cap in
// place, instead of restarting the server.
//
// Semantics, deliberately fail-closed throughout, along the LG-327
// HealthWatch.SetInterval retune shape and the LG-314/LG-316..LG-326
// family:
//
//   - The revision is in place: disputes, submitted representments,
//     adjudications, and the ledger's counters are untouched. Only
//     the cap on future submissions moves. No restart.
//   - The revision is future-only: it governs the next submission.
//     Already-submitted representments are unaffected — a package
//     filed under a wider cap stays valid after a tightening, and a
//     tightening never retroactively invalidates it; the replay check
//     (same evidence returns the original) and adjudication keep
//     working on the stored package as filed.
//   - Validation is fail-closed with no silent normalization: the new
//     cap must be >= 1, else ErrInvalidRepresentmentMaxDocuments and
//     the live cap stays exactly as it was. Zero is rejected — a zero
//     cap would make any representment carrying a document fail —
//     and negatives are rejected. There is no upper bound: the
//     record-size trade-off is the operator's (along the LG-346
//     outbox buffer, whose cap is likewise unbounded).
//   - A revision identical to the live cap is a no-op: it returns the
//     live value with a nil error and writes no audit event, so a
//     retried or duplicated revision cannot spam the audit trail.
//   - The amendment is structural config, not ledger state: it bumps
//     neither the version nor the audit chain, writes no journal
//     entry, and emits no outbox event. It lands one
//     representment_max_documents_retuned audit event (previous /
//     current cap as plain integers). The Ctx variant attributes the
//     operator (LG-73). The enforcement path
//     (checkRepresentmentEvidence) reads the live value under the
//     write lock SubmitRepresentment already holds; the setter holds
//     the write lock, along the representmentWindow morphology.
//   - There is no HTTP endpoint for the revision (along LG-241/242
//     and the LG-314/LG-316..LG-326 family): dispute governance
//     tuning is in-process structural config.
//   - Snapshots do not carry the cap: a restore starts again at the
//     default 32, while the revision events themselves stay in the
//     audit trail for traceability.
//   - The cap is NOT the evidence window: how long the payee has to
//     file stays with SetRepresentmentWindow (a duration), while
//     this module decides how large one package may be — time and
//     size are orthogonal knobs. The other two caps (one hash at
//     most 128 characters, the statement at most 4000) are untouched,
//     as is the non-empty-evidence gate.
//
// Orthogonality: LG-179 owns the dispute representment mechanism
// itself (this module is its missing runtime revision entry point
// for the package cap), LG-206 owns the pacs.004 bank return (a
// different object: a bank-initiated return, not the payee's
// evidence package), and SetRepresentmentWindow owns the evidence
// window duration (time, this module package size).

var (
	// ErrInvalidRepresentmentMaxDocuments is returned by
	// SetRepresentmentMaxDocuments when the requested cap is less
	// than 1. The live cap is left untouched.
	ErrInvalidRepresentmentMaxDocuments = errors.New("ledger: representment max documents must be at least 1")
)

// SetRepresentmentMaxDocuments revises the live evidence-package cap
// in place and reports the pre-revision cap (for the audit trail). A
// wider cap lets a complex dispute file more document hashes in one
// submission; a tighter cap guards against evidence-package abuse.
// The revision governs only future submissions — already-submitted
// representments are unaffected. A cap less than 1 returns
// ErrInvalidRepresentmentMaxDocuments with the live cap untouched
// (the returned value is the live cap in that case too). A revision
// identical to the live cap is a no-op that returns the live value
// with a nil error and no side effect.
func (l *Ledger) SetRepresentmentMaxDocuments(n int) (int, error) {
	return l.SetRepresentmentMaxDocumentsCtx(context.Background(), n)
}

// SetRepresentmentMaxDocumentsCtx is SetRepresentmentMaxDocuments
// with per-call audit actor attribution (LG-73).
func (l *Ledger) SetRepresentmentMaxDocumentsCtx(ctx context.Context, n int) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	previous := l.representmentMaxDocuments
	if n < 1 {
		return previous, fmt.Errorf("%w: %d", ErrInvalidRepresentmentMaxDocuments, n)
	}
	if n == previous {
		return previous, nil
	}
	l.representmentMaxDocuments = n
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "representment_max_documents_retuned",
		Actor:         "SetRepresentmentMaxDocuments",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"previous_max_documents": previous,
			"current_max_documents":  n,
		},
	})
	return previous, nil
}

// RepresentmentMaxDocuments reports the live evidence-package cap:
// the most document hashes one representment may carry.
func (l *Ledger) RepresentmentMaxDocuments() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.representmentMaxDocuments
}
