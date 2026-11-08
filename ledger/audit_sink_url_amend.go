package ledger

import (
	"context"
	"errors"
	"fmt"
	"net/url"
)

// Audit-sink delivery-target amendment (LG-352).
//
// StartAuditSink fixes the sink's delivery target for the life of the
// forwarder: SinkConfig.URL is validated once at construction, and
// the run goroutine reads it on every delivery. The only way to move
// a live sink to a new SIEM / archive endpoint was Stop +
// StartAuditSink, which is a teardown, not a revision: Stop waits for
// the run goroutine to exit, a restart re-reads the cursor file (no
// progress is lost, but the stream stalls for the gap), a delivery in
// flight is cut short by the done channel, and a failed restart
// (outbox directory unavailable, corrupt cursor) stops forwarding
// entirely. This module is the orthogonal runtime path: an operator
// re-points the live sink in place, instead of rebuilding it.
//
// Semantics, deliberately fail-closed throughout, along LG-306's
// delivery-policy revision and LG-313's secret rotation (the same
// component, the same shape):
//
//   - The revision is in place: the cursor, the queue order, the
//     counters, the delivery policy, and the signing secret stay
//     exactly as they were. Only the target URL moves. The http.Client
//     is not rebuilt — the URL is not part of the client, so a
//     revision never disturbs the client's connection pool.
//   - The race is the real implementation point. post read s.cfg.URL
//     without any lock, while a setter writing s.cfg.URL directly
//     would race those reads. The URL therefore joins the delivery
//     snapshot (LG-306): deliver snapshots it under cfgMu.RLock at
//     entry, post takes the snapshotted URL as a parameter, and the
//     snapshot is thrown away after the delivery. A delivery in
//     flight keeps the target it started with — including all its
//     retries — and the revision governs only the next delivery
//     (future-only, along LG-288 / LG-301 / LG-304 / LG-306 /
//     LG-313).
//   - Validation reuses the construction-time gate
//     (SinkConfig.Validate): the URL must be an absolute http(s) URL
//     with a host. An illegal URL is ErrInvalidSinkURL and the live
//     URL stays exactly as it was. A runtime revision never silently
//     normalizes (along LG-290 / LG-304 / LG-306).
//   - A revision identical to the live URL is a no-op: it returns the
//     live URL with a nil error and writes no audit event, so a
//     retried or duplicated revision cannot spam the audit trail.
//   - The amendment is structural config, not ledger state: it bumps
//     neither the version nor the audit chain, writes no journal
//     entry, and emits no outbox event — so the sink never forwards
//     its own revision event and there is no self-loop. It lands one
//     sink_url_revised audit event (previous_url / current_url, plain
//     URLs — no key material). The Ctx variant attributes the
//     operator (LG-73). Lock order follows deadLetter and LG-306:
//     the setter releases cfgMu, then takes l.mu.RLock to emit —
//     cfgMu and l.mu are never held nested, so the lock order is
//     acyclic.
//   - There is no HTTP endpoint for the revision (along LG-241/242
//     and LG-306): delivery-target retargeting is in-process
//     structural config.
//   - Snapshots do not carry the URL: the sink config comes only from
//     the startup-time LEDGER_AUDIT_SINK_URL parse, so a restart
//     restores the env value while the amendment events themselves
//     stay in the audit trail for traceability. Update the env var
//     alongside the revision for it to persist (along LG-313).
//   - This module deliberately adds no Ledger-level setter: the
//     Ledger never holds the sink (AuditSink appears in ledger/*.go
//     only inside audit_sink.go; main.go's srv.auditSink is the sole
//     holder), so callers holding the *AuditSink revise it directly
//     — the one deliberate departure from LG-300–305's Ledger
//     delegate shape, for a reason verified in the code (along
//     LG-306 / LG-313).
//   - Explicitly out of scope: Secret (LG-313 owns it), the four
//     delivery knobs (LG-306 owns them), PollInterval (LG-314 owns
//     it), the poll page limit (LG-350 owns it), and CursorFile
//     (cursor identity, fixed at start).
//
// Orthogonality: LG-67 owns the sink mechanism itself (polling,
// cursor, signing, dead-lettering — configurable only at start; this
// module is its delivery target's runtime entry point), LG-306 owns
// the four delivery knobs (its entry explicitly deferred the URL to a
// later round — this module is that round), LG-313 owns the signing
// secret (key material; this module is the target address), and
// LG-304 owns the outbound webhook endpoints' delivery policy (a
// multi-endpoint registry; this module is a single sink with no
// registry — the webhook endpoints' own URL revision is LG-353).

var (
	// ErrInvalidSinkURL is returned by AuditSink.SetSinkURL when the
	// new URL fails the construction-time gate: it must be an absolute
	// http(s) URL with a host. The live URL is left untouched.
	ErrInvalidSinkURL = errors.New("ledger: audit sink: invalid sink URL: want an absolute http(s) URL")
)

// validSinkURL applies the construction-time URL gate
// (SinkConfig.Validate) to a candidate target, without the
// construction-time "secret is required" check: a runtime revision
// never silently normalizes.
func validSinkURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("%w: URL is required", ErrInvalidSinkURL)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%w: invalid URL %q", ErrInvalidSinkURL, raw)
	}
	return nil
}

// SetSinkURL re-points the sink's delivery target in place and
// reports the pre-revision URL (for the audit trail). The revision is
// future-only: a delivery already in flight keeps the target it
// snapshotted at entry, including all its retries; only the next
// delivery reads the new URL. An illegal URL returns
// ErrInvalidSinkURL with the live URL untouched; a revision identical
// to the live URL is a no-op that returns the live URL with a nil
// error and no side effect.
func (s *AuditSink) SetSinkURL(url string) (string, error) {
	return s.SetSinkURLCtx(context.Background(), url)
}

// SetSinkURLCtx is SetSinkURL with per-call audit actor attribution
// (LG-73).
func (s *AuditSink) SetSinkURLCtx(ctx context.Context, url string) (string, error) {
	s.cfgMu.Lock()
	if err := validSinkURL(url); err != nil {
		s.cfgMu.Unlock()
		return "", err
	}
	previous := s.cfg.URL
	if url == previous {
		s.cfgMu.Unlock()
		return previous, nil
	}
	s.cfg.URL = url
	s.cfgMu.Unlock()

	// Audit after the swap, under l.mu.RLock like deadLetter and
	// LG-306: cfgMu and l.mu are never held nested, so the lock order
	// is acyclic. emitAudit is a no-op when no audit log is attached,
	// and the event goes only to the audit log — never to the outbox,
	// so the sink cannot forward its own revision event.
	s.l.mu.RLock()
	s.l.emitAuditCtx(ctx, AuditEvent{
		Op:            "sink_url_revised",
		Actor:         "SetSinkURL",
		VersionBefore: s.l.version,
		VersionAfter:  s.l.version,
		Details: map[string]any{
			"previous_url": previous,
			"current_url":  url,
		},
	})
	s.l.mu.RUnlock()
	return previous, nil
}

// SinkURL reports the sink's live delivery target.
func (s *AuditSink) SinkURL() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.URL
}
