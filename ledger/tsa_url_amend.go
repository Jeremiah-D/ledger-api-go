package ledger

import (
	"context"
	"errors"
	"net/url"
	"strings"
)

// TSA timestamp endpoint runtime re-sourcing (LG-303).
//
// WithTSAURL (tsa.go) configures the timestamp authority endpoint only
// at construction — the endpoint was frozen for the life of the
// process, so migrating to a new TSA service (or a moved endpoint)
// needed a restart. This module is the orthogonal runtime path: an
// operator re-sources the endpoint in place, instead of rebuilding
// the ledger and replaying its journal.
//
// The naive setter would only fix half the split: the manual path
// (POST /entries/tsa-anchor, main.go handleTSAAnchor) already reads
// the live Ledger.TSAURL() on every request, but the background
// worker was handed a frozen &RFC3161Client{URL: tsaURL} at startup
// (main.go) and reused it on every tick for the life of the process.
// A ledger-level setter alone would leave the worker stamping
// against the old TSA while the operator believed the migration had
// happened. The fix is therefore two halves: this setter, and the
// worker-side resolving authority in main.go (tsaMetricsAuthority
// now holds the ledger and constructs a fresh RFC3161Client from the
// live TSAURL() on every Timestamp call).
//
// Semantics, deliberately fail-closed throughout:
//
//   - Validation reuses the in-repo URL convention (ledger.go
//     checkAttachments, webhook.go): url.Parse, then the scheme must
//     be http or https and the host must be non-empty. An empty,
//     unparsable, non-http(s), or hostless URL is rejected with the
//     new error ErrInvalidTSAURL and the live endpoint is left
//     exactly as it was. The runtime path never panics, unlike
//     construction, which only stores the string unvalidated.
//     There is no "clear the endpoint" spelling at runtime: an empty
//     URL is invalid, not a disable.
//   - Re-sourcing to the current URL string is a no-op: it returns
//     nil and writes no audit event, so a retried or duplicated
//     amendment cannot spam the audit trail.
//   - The swap is atomic (lock order l.mu -> tsa.mu, per the note on
//     tsaAnchorState) and the endpoint itself lives in an
//     atomic.Pointer[string]: the worker's resolving authority reads
//     the live endpoint on every Timestamp call, and AnchorTimestamp
//     holds tsa.mu across that call, so a mutex-guarded read would
//     deadlock against the in-flight anchoring. An anchoring already
//     in flight when the setter runs finishes first and records the
//     old URL — the URL recorded on an anchor always names the TSA
//     that actually signed it, never a torn mix.
//   - Re-sourcing does not break history verification: every anchor
//     carries its own TSAURL (AnchorTimestamp records it per anchor),
//     and verifyTimestampAnchorsLocked only verifies token signatures
//     against the trust root (LG-302) — the URL never participates in
//     verification. Old anchors keep verifying, TimestampAnchorStatus
//     ().Verified and the Reconcile anchor health stay green across a
//     re-sourcing, and a restart recovers the construction-time / env
//     URL while the amendment events stay in the audit trail. One
//     verifier rule had to move for that to hold: the sequence check
//     used to demand strictly increasing seq, which a same-head
//     re-anchor against the new TSA violates by design, so both
//     verifiers (live and offline) now admit an equal sequence only
//     as a same-head re-anchor against a different TSA with a
//     different token (tsaAnchorSeqOK) — an exact duplicate line, or
//     one with an edited URL, still fails as a rewrite.
//   - The idempotency key of AnchorTimestamp includes the TSA URL, so
//     after a re-sourcing the same head is re-anchored against the
//     new TSA (which has not witnessed that head yet — correct), and
//     the old anchor stays in the journal.
//   - The amendment is endpoint governance, not ledger state: it
//     bumps neither the ledger version nor the audit chain, and no
//     journal entry or outbox event fires. It lands one
//     tsa_url_amended audit event carrying previous_url / new_url —
//     URLs only, never token material. The Ctx variant attributes
//     the operator (LG-73).
//   - Snapshots do not carry the URL (the TSA endpoint is
//     construction-time config, like the audit log): a restart
//     restores the env value.
//
// Orthogonality: WithTSAURL owns construction-time install, LG-84
// owns the anchoring mechanism itself, LG-100 owns the chain_anchored
// audit-chain evidence, LG-300 owns the operator's own Ed25519 anchor
// signing key, LG-302 owns the TSA X.509 trust root (which signers
// are trusted — this item owns which endpoint is asked, a separate
// dimension), and this module owns only the runtime re-sourcing of
// the TSA endpoint and the worker's per-tick follow. There is no HTTP
// endpoint for the amendment (per LG-241/242): the endpoint is
// in-process operator config. Explicitly out of scope: TSA client
// reconnect/retry and timeout tuning (RFC3161Client behavior), and
// the trust certificate (LG-302).
//
// Boundary (from the item spec): the worker only exists when
// LEDGER_TSA_URL was set at startup — this item does not cold-start
// a worker. A ledger that never configured a URL can still be
// re-sourced, and the manual anchor path then works against the new
// endpoint immediately; the periodic worker appears only after a
// restart with the env var set.

// ErrInvalidTSAURL is returned by SetTSAURL when the candidate
// endpoint is empty, unparsable, not an http(s) URL, or hostless.
// The live endpoint is left untouched.
var ErrInvalidTSAURL = errors.New("ledger: invalid TSA URL (must be a non-empty http(s) URL with a host)")

// validTSAURL validates a candidate TSA endpoint URL: url.Parse, then
// an http(s) scheme and a non-empty host, following the in-repo URL
// convention (ledger.go checkAttachments, webhook.go). Go's url.Parse
// lowercases the scheme, so the comparison is exact.
func validTSAURL(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return ErrInvalidTSAURL
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ErrInvalidTSAURL
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ErrInvalidTSAURL
	}
	if u.Host == "" {
		return ErrInvalidTSAURL
	}
	return nil
}

// SetTSAURL re-sources the timestamp authority endpoint at runtime.
// The new endpoint governs only later anchoring: anchors already
// taken keep their recorded URL and keep verifying, and the same head
// is re-anchored against the new TSA (which has not witnessed it
// yet). An invalid URL is rejected with ErrInvalidTSAURL and the live
// endpoint is left untouched; re-sourcing to the current URL is a
// no-op with no audit event. The amendment bumps neither the ledger
// version nor the audit chain, and fires no journal entry or outbox
// event; it lands one tsa_url_amended audit event carrying
// previous_url / new_url only.
func (l *Ledger) SetTSAURL(url string) error {
	return l.SetTSAURLCtx(context.Background(), url)
}

// SetTSAURLCtx is SetTSAURL with per-call audit actor attribution
// (LG-73).
func (l *Ledger) SetTSAURLCtx(ctx context.Context, url string) error {
	if err := validTSAURL(url); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	t := &l.tsa
	t.mu.Lock()
	defer t.mu.Unlock()
	previous := t.tsaURLValue()
	if previous == url {
		return nil
	}
	u := url
	t.tsaURL.Store(&u)
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "tsa_url_amended",
		Actor:         "SetTSAURL",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"previous_url": previous,
			"new_url":      url,
		},
	})
	return nil
}
