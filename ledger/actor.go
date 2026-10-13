package ledger

import (
	"context"
	"strings"
)

// Audit actor attribution (LG-73).
//
// AuditEvent.Actor used to name only the in-process ledger entrypoint
// ("Post", "Freeze", ...): enough to say what code ran, but not who
// asked for it. Operator actions — freeze/unfreeze, transfer-review
// approve/reject, account close, period close/reopen — now accept a
// per-call actor through a context value, so the HTTP layer can stamp
// the real operator identity (from the x-ledger-actor header) onto the
// audit event before it is sealed.
//
// The convention is deliberately narrow:
//   - ContextWithActor/ActorFromContext carry the identity; the ledger
//     never invents one. Operator methods expose Ctx variants
//     (FreezeCtx, ApproveTransferReviewCtx, ...) whose plain twins
//     delegate with context.Background() — every existing caller and
//     test sees byte-identical behavior.
//   - emitAuditCtx overrides the event's Actor only when the context
//     carries a non-empty identity; otherwise the entrypoint name is
//     kept. Attribution happens before AuditLog.Log seals the event, so
//     the LG-32 hash-chain seal covers the actor like every other
//     field: tampering with the actor breaks chain verification.
//   - The actor is pure metadata: it never enters balance math, version
//     bumps, journal entries, or snapshots. Snapshot round-trips are
//     actor-agnostic — the actor is call-scoped, never ledger state.
//   - The HTTP layer defaults a missing x-ledger-actor header to
//     DefaultAuditActor ("system") and validates the value; the ledger
//     trusts whatever identity the context carries.

// actorContextKey is the unexported context key for the operator
// identity. Unexported so only this package's helpers can set it — a
// caller cannot smuggle a different key's value in.
type actorContextKey struct{}

// DefaultAuditActor is the actor the HTTP layer attributes when the
// client sends no x-ledger-actor header.
const DefaultAuditActor = "system"

// ContextWithActor returns ctx carrying the operator identity for audit
// attribution. A nil ctx is treated as context.Background().
func ContextWithActor(ctx context.Context, actor string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, actorContextKey{}, actor)
}

// ActorFromContext returns the operator identity carried by ctx, or ""
// when none was set.
func ActorFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if a, ok := ctx.Value(actorContextKey{}).(string); ok {
		return a
	}
	return ""
}

// NormalizeAuditActor trims the header value; an empty value becomes
// DefaultAuditActor. The HTTP layer applies this to x-ledger-actor so
// every operator request is attributed, even anonymous ones.
func NormalizeAuditActor(actor string) string {
	if a := strings.TrimSpace(actor); a != "" {
		return a
	}
	return DefaultAuditActor
}

// emitAuditCtx enqueues an audit event attributed to the operator carried
// by ctx (LG-73). When ctx carries no (non-blank) actor, the event's
// Actor field — the in-process entrypoint name — is kept unchanged.
// Callers must hold l.mu (either lock suffices), like emitAudit.
func (l *Ledger) emitAuditCtx(ctx context.Context, ev AuditEvent) {
	if actor := strings.TrimSpace(ActorFromContext(ctx)); actor != "" {
		ev.Actor = actor
	}
	l.emitAudit(ev)
}
