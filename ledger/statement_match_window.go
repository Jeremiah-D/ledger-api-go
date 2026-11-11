package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Statement match window runtime revision (LG-394).
//
// The statement imports (OFX, camt.053, camt.054), the standalone
// MatchStatement matcher, and the nostro reconciliation all judge a
// journaled row a match candidate only when its creation falls within
// ±window of the statement line's booking date. Each of the five
// fallback read sites used to hard-code its own 24h default when the
// caller left MatchWindow unset — ImportOFX (ofx_import.go),
// ImportCAMT053 (camt053.go), ImportCAMT054 (camt054.go),
// matchStatementLocked (statement_match.go), matchNostroLocked
// (nostro.go) — so an operator who wanted to tighten the window to
// ±2h (against cross-day mis-matches) or widen it to ±72h (weekend
// reconciliation) had to change a constant and redeploy. This module
// is the orthogonal runtime path: an operator revises the live
// default in place, instead of restarting the server.
//
// Semantics, along LG-347 (the forward grace: the same duration-knob,
// (error) audit shape) and LG-344 (the check order — validate,
// compare, swap, then audit):
//
//   - The window is a live field on the Ledger, guarded by l.mu —
//     the same lock that guards the import registries and the
//     matchers, so no second lock is introduced and no lock order
//     is created. Every fallback read site snapshots it once under
//     the lock it already holds.
//   - The three per-format constants (DefaultOFXMatchWindow,
//     DefaultCAMT053MatchWindow, DefaultCAMT054MatchWindow) stay as
//     the default-value source: the live field seeds from
//     DefaultOFXMatchWindow (24h) at construction.
//   - An explicit opts.MatchWindow always wins: the live value only
//     fills in when the caller left the window unset (<= 0). A
//     revision never re-matches rows already matched — it governs
//     the next import, match, or nostro reconciliation, including
//     the next line of an HTTP import already in flight (the HTTP
//     importers call ImportOFX/ImportCAMT053/ImportCAMT054 with
//     empty options, so they always read the live default).
//   - A non-positive window is rejected with
//     ErrInvalidStatementMatchWindow and the live value is left
//     untouched. Zero is rejected, not a "close the window" state:
//     zero in the options already means "take the default", so a
//     setter that accepted zero would be ambiguous (along LG-336).
//     There is no upper bound: the operator owns the
//     mis-match/false-negative trade-off.
//   - Revising to the current window is a no-op with no audit
//     event. The revision is audited as
//     statement_match_window_revised, with previous/current as
//     duration strings.
//   - The window does not round-trip through snapshots: a restore
//     starts again at the 24h default, like the other live
//     duration knobs. The revision event stays in the audit trail.

var (
	// ErrInvalidStatementMatchWindow is returned by
	// SetStatementMatchWindow for a non-positive window: the live
	// window is left untouched.
	ErrInvalidStatementMatchWindow = errors.New("ledger: statement match window must be positive")
)

// SetStatementMatchWindow revises the live default statement-match
// window in place and returns an error for a non-positive window.
// Revising to the current window is a no-op with no audit event.
// The revision is audited as statement_match_window_revised.
func (l *Ledger) SetStatementMatchWindow(d time.Duration) error {
	return l.SetStatementMatchWindowCtx(context.Background(), d)
}

// SetStatementMatchWindowCtx is SetStatementMatchWindow with
// per-call audit actor attribution (LG-73).
func (l *Ledger) SetStatementMatchWindowCtx(ctx context.Context, d time.Duration) error {
	// Check order, along LG-344: validate first, then compare,
	// then swap, then audit — all under l.mu, which also guards
	// the field, so the decision and the swap are atomic with
	// respect to every fallback reader.
	l.mu.Lock()
	defer l.mu.Unlock()
	previous := l.statementMatchWindow
	if d <= 0 {
		return fmt.Errorf("%w: got %s", ErrInvalidStatementMatchWindow, d)
	}
	if d == previous {
		return nil
	}
	l.statementMatchWindow = d
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "statement_match_window_revised",
		Actor:         DefaultAuditActor,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"previous_window": previous.String(),
			"current_window":  d.String(),
		},
	})
	return nil
}

// StatementMatchWindow reports the live default statement-match
// window: how far a journaled row's creation may stray from a
// statement line's booking date when the caller left MatchWindow
// unset. The name is shared with the per-format default constants,
// which are the construction-time default (24h) and the seed — the
// method reads the live value.
func (l *Ledger) StatementMatchWindow() time.Duration {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.statementMatchWindow
}

// effectiveStatementMatchWindowLocked resolves the window an import,
// match, or nostro reconciliation actually judges against: an
// explicit caller window always wins, an unset one (<= 0) falls back
// to the live default. Callers must hold l.mu (either lock
// suffices).
func (l *Ledger) effectiveStatementMatchWindowLocked(window time.Duration) time.Duration {
	if window <= 0 {
		return l.statementMatchWindow
	}
	return window
}
