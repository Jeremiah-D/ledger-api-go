package ledger

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Outbound signed webhooks (LG-102): an opt-in subscriber of the
// domain-event outbox (see outbox.go) that POSTs selected domain facts —
// transfer settled/rejected, pain.001 status moves, or any other outbox
// event type — to registered external endpoints as HMAC-SHA256-signed
// JSON webhooks.
//
// Delivery contract, stated honestly:
//
//   - The dispatcher is a subscriber, not a second outbox: it only
//     forwards events the outbox durably emitted (Emit runs first, the
//     webhook payload is the outbox event JSON including its seq), and
//     only when at least one endpoint is registered. With no endpoints
//     registered, Notify is a no-op and the posting path pays nothing
//     beyond a nil check.
//   - The posting path never blocks on webhooks: Notify fans out with
//     non-blocking sends into per-endpoint bounded queues. A full queue
//     drops the event (counted, never retried — the outbox stream
//     remains the durable record) and the commit returns. Delivery and
//     retries happen on per-endpoint background workers.
//   - At-least-once per delivery attempt, at-most-once per queue drop:
//     a crash loses whatever sat in the in-memory queues. The outbox
//     files are the durable stream; the webhooks are a live
//     notification fan-out, not a replay log.
//   - A delivery that exhausts its retries is recorded as failed and
//     counted (ledger_webhook_failures_total — alert on growth). The
//     failure never touches the posting path.
//   - Every delivery is signed: HMAC-SHA256 over the exact request
//     body, sent as X-Ledger-Signature: sha256=<hex> alongside
//     X-Ledger-Key-Id (which signing key), X-Ledger-Event-Type and
//     X-Ledger-Event-Seq. Endpoints refuse to register without keys —
//     unsigned webhooks can be forged.
//
// This is deliberately separate from the audit-sink forwarder (LG-67,
// see audit_sink.go): the sink tails the outbox files for one
// compliance endpoint with a persisted cursor; the dispatcher pushes
// live events to a registry of consumer endpoints with per-endpoint
// event filters, key rotation, and a delivery-status query surface.

// Delivery statuses for DeliveryRecord.
const (
	WebhookDeliveryPending   = "pending"
	WebhookDeliveryDelivered = "delivered"
	WebhookDeliveryFailed    = "failed"
)

// Default webhook tunables (see WebhookEndpointConfig): 8 attempts,
// 500ms initial backoff doubling to 30s with up to +25% jitter, a 10s
// per-attempt HTTP timeout, a 1024-event per-endpoint queue, and a
// 24h signing-key grace period.
const (
	DefaultWebhookMaxAttempts    = 8
	DefaultWebhookInitialBackoff = 500 * time.Millisecond
	DefaultWebhookMaxBackoff     = 30 * time.Second
	DefaultWebhookHTTPTimeout    = 10 * time.Second
	DefaultWebhookQueueSize      = 1024
	DefaultWebhookMaxRecords     = 1024
	DefaultWebhookKeyGracePeriod = 24 * time.Hour
)

// DeliveryRecord is one webhook delivery's observable lifecycle: the
// outbox event seq it carries, the endpoint it went to, how many POST
// attempts it took, its terminal state, the last transport/HTTP error,
// and when the record was created. Records live in a bounded ring
// (DefaultWebhookMaxRecords); the query surface is GET
// /webhooks/deliveries.
type DeliveryRecord struct {
	EventSeq  uint64    `json:"event_seq"`
	Endpoint  string    `json:"endpoint"`
	Attempts  int       `json:"attempts"`
	Status    string    `json:"status"`
	LastError string    `json:"last_error,omitempty"`
	At        time.Time `json:"at"`
}

// WebhookDeliveryQuery filters a Deliveries scan. Zero values mean
// "unbounded": Endpoint exact-matches a registered endpoint name,
// Status is one of pending/delivered/failed, and Limit caps the page at
// 1..100. Records come back latest-first.
type WebhookDeliveryQuery struct {
	Endpoint string
	Status   string
	Limit    int
}

// WebhookKey is one HMAC signing secret with a stable identity. ID is
// sent as X-Ledger-Key-Id so the receiver knows which secret signed the
// body. RetiredAt is zero for the active primary; a non-zero RetiredAt
// marks a rotated-out key that still verifies until
// RetiredAt+grace (the grace period).
type WebhookKey struct {
	ID        string
	Secret    []byte
	RetiredAt time.Time
}

// RotatingKey is a thread-safe signing-key ring for one webhook
// endpoint: at least one key, unique IDs, exactly one primary. Signing
// always uses the primary; verification accepts any key that has not
// passed its grace period. Rotate retires the current primary (it keeps
// verifying for the grace period, so in-flight and just-delivered
// payloads signed with it stay verifiable) and promotes the new key.
type RotatingKey struct {
	mu      sync.RWMutex
	keys    map[string]*WebhookKey
	primary string
	grace   time.Duration
}

// NewRotatingKey builds a key ring with a single primary key. A
// non-positive grace selects DefaultWebhookKeyGracePeriod. The secret
// is copied.
func NewRotatingKey(id string, secret []byte, grace time.Duration) (*RotatingKey, error) {
	if id == "" {
		return nil, errors.New("ledger: webhook: key id is required")
	}
	if len(secret) == 0 {
		return nil, errors.New("ledger: webhook: key secret is required: unsigned webhooks can be forged")
	}
	if grace < 0 {
		return nil, errors.New("ledger: webhook: key grace period must not be negative")
	}
	if grace == 0 {
		grace = DefaultWebhookKeyGracePeriod
	}
	cp := make([]byte, len(secret))
	copy(cp, secret)
	return &RotatingKey{
		keys:    map[string]*WebhookKey{id: {ID: id, Secret: cp}},
		primary: id,
		grace:   grace,
	}, nil
}

// Rotate retires the current primary key and promotes a new one: the
// new key signs from here on, the old key keeps verifying until its
// grace period lapses. The new ID must be unique in the ring and the
// secret non-empty; both violations fail fast — a rotation that
// silently reused an ID would make X-Ledger-Key-Id ambiguous.
func (k *RotatingKey) Rotate(newID string, newSecret []byte) error {
	if newID == "" {
		return errors.New("ledger: webhook: rotated key id is required")
	}
	if len(newSecret) == 0 {
		return errors.New("ledger: webhook: rotated key secret is required")
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, dup := k.keys[newID]; dup {
		return fmt.Errorf("ledger: webhook: duplicate key id %q", newID)
	}
	k.purgeExpiredLocked(time.Now())
	if old, ok := k.keys[k.primary]; ok {
		old.RetiredAt = time.Now().UTC()
	}
	cp := make([]byte, len(newSecret))
	copy(cp, newSecret)
	k.keys[newID] = &WebhookKey{ID: newID, Secret: cp}
	k.primary = newID
	return nil
}

// PrimaryID reports the ID of the key currently used for signing.
func (k *RotatingKey) PrimaryID() string {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.primary
}

// Keys snapshots the ring: the primary first, then retired keys newest
// first. Secrets are copied. Expired keys are purged before the
// snapshot.
func (k *RotatingKey) Keys() []WebhookKey {
	k.mu.Lock()
	defer k.mu.Unlock()
	now := time.Now()
	k.purgeExpiredLocked(now)
	out := make([]WebhookKey, 0, len(k.keys))
	if p, ok := k.keys[k.primary]; ok {
		out = append(out, p.copy())
	}
	var retired []*WebhookKey
	for id, key := range k.keys {
		if id != k.primary {
			retired = append(retired, key)
		}
	}
	for i := 0; i < len(retired); i++ {
		for j := i + 1; j < len(retired); j++ {
			if retired[j].RetiredAt.After(retired[i].RetiredAt) {
				retired[i], retired[j] = retired[j], retired[i]
			}
		}
	}
	for _, key := range retired {
		out = append(out, key.copy())
	}
	return out
}

func (k *WebhookKey) copy() WebhookKey {
	cp := make([]byte, len(k.Secret))
	copy(cp, k.Secret)
	return WebhookKey{ID: k.ID, Secret: cp, RetiredAt: k.RetiredAt}
}

// purgeExpiredLocked drops retired keys past their grace period.
// Callers must hold k.mu with the write lock.
func (k *RotatingKey) purgeExpiredLocked(now time.Time) {
	for id, key := range k.keys {
		if id == k.primary {
			continue
		}
		if !key.RetiredAt.IsZero() && !now.Before(key.RetiredAt.Add(k.grace)) {
			delete(k.keys, id)
		}
	}
}

// expiredLocked reports whether key may no longer verify. Callers must
// hold k.mu (either lock suffices).
func (k *RotatingKey) expiredLocked(key *WebhookKey, now time.Time) bool {
	return !key.RetiredAt.IsZero() && !now.Before(key.RetiredAt.Add(k.grace))
}

// Sign computes HMAC-SHA256(primary secret, body) and returns it in the
// X-Ledger-Signature wire form (sha256=<hex>) plus the signing key's
// ID for X-Ledger-Key-Id. It fails only when the ring has no primary,
// which cannot happen through the public constructors.
func (k *RotatingKey) Sign(body []byte) (signature, keyID string, err error) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	p, ok := k.keys[k.primary]
	if !ok {
		return "", "", errors.New("ledger: webhook: no primary signing key")
	}
	mac := hmac.New(sha256.New, p.Secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil)), p.ID, nil
}

// Verify reports whether signature is a valid HMAC-SHA256 of body
// under any unexpired key in the ring. It accepts the wire form
// (sha256=<hex>) or the bare hex.
func (k *RotatingKey) Verify(body []byte, signature string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	now := time.Now()
	k.purgeExpiredLocked(now)
	sig := strings.TrimPrefix(signature, "sha256=")
	want, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	for _, key := range k.keys {
		if k.expiredLocked(key, now) {
			continue
		}
		mac := hmac.New(sha256.New, key.Secret)
		mac.Write(body)
		if hmac.Equal(mac.Sum(nil), want) {
			return true
		}
	}
	return false
}

// VerifyWithKey verifies against one named key only — the receiver-side
// counterpart of X-Ledger-Key-Id. It returns false for unknown or
// expired key IDs.
func (k *RotatingKey) VerifyWithKey(body []byte, keyID, signature string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	now := time.Now()
	k.purgeExpiredLocked(now)
	key, ok := k.keys[keyID]
	if !ok || k.expiredLocked(key, now) {
		return false
	}
	sig := strings.TrimPrefix(signature, "sha256=")
	want, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, key.Secret)
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), want)
}

// WebhookEndpointConfig configures one outbound webhook endpoint. Zero
// values select the documented defaults via withDefaults, except Keys
// (required) and Enabled (defaults true).
type WebhookEndpointConfig struct {
	// Name identifies the endpoint in delivery records and the
	// registry. Required, unique per dispatcher.
	Name string
	// URL is the webhook receiver. Required; must be an absolute
	// http(s) URL.
	URL string
	// Events filters which outbox event types are forwarded (exact
	// match on the event type, e.g. transfer_posted, review_rejected,
	// pain001_status_changed). Empty means all types.
	Events []string
	// Keys is the endpoint's signing-key ring. Required — unsigned
	// webhooks can be forged, so an endpoint without keys refuses to
	// register.
	Keys *RotatingKey
	// Enabled gates delivery: a disabled endpoint's events are dropped
	// at fan-out (not queued). Defaults true.
	Enabled bool
	// MaxAttempts bounds the POST attempts per delivery, including the
	// first try. Default DefaultWebhookMaxAttempts.
	MaxAttempts int
	// InitialBackoff is the wait before the first retry; it doubles
	// per retry up to MaxBackoff, plus up to +25% jitter. Defaults
	// DefaultWebhookInitialBackoff/DefaultWebhookMaxBackoff.
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	// HTTPTimeout bounds a single POST attempt. Default
	// DefaultWebhookHTTPTimeout.
	HTTPTimeout time.Duration
	// QueueSize bounds the endpoint's in-memory event queue. A full
	// queue drops the event (counted) rather than blocking the posting
	// path. Default DefaultWebhookQueueSize.
	QueueSize int
}

// withDefaults fills zero tunables with the documented defaults. Keys
// and the event filter are left untouched.
func (c WebhookEndpointConfig) withDefaults() WebhookEndpointConfig {
	if c.MaxAttempts < 1 {
		c.MaxAttempts = DefaultWebhookMaxAttempts
	}
	if c.InitialBackoff <= 0 {
		c.InitialBackoff = DefaultWebhookInitialBackoff
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = DefaultWebhookMaxBackoff
	}
	if c.InitialBackoff > c.MaxBackoff {
		c.InitialBackoff = c.MaxBackoff
	}
	if c.HTTPTimeout <= 0 {
		c.HTTPTimeout = DefaultWebhookHTTPTimeout
	}
	if c.QueueSize < 1 {
		c.QueueSize = DefaultWebhookQueueSize
	}
	return c
}

// Validate rejects an unusable endpoint config: a missing name, a
// missing or non-http(s) URL, no signing keys, a MaxAttempts below 1,
// or a negative duration. Callers that want zero values to mean "use
// the default" must run withDefaults first — Register and
// NewWebhookDispatcher do.
func (c WebhookEndpointConfig) Validate() error {
	if c.Name == "" {
		return errors.New("ledger: webhook: endpoint name is required")
	}
	u, err := url.Parse(c.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("ledger: webhook: invalid URL %q: want an absolute http(s) URL", c.URL)
	}
	if c.Keys == nil {
		return fmt.Errorf("ledger: webhook: endpoint %q has no signing keys: unsigned webhooks can be forged", c.Name)
	}
	if c.MaxAttempts < 1 {
		return fmt.Errorf("ledger: webhook: endpoint %q MaxAttempts must be >= 1, got %d", c.Name, c.MaxAttempts)
	}
	if c.InitialBackoff < 0 || c.MaxBackoff < 0 || c.HTTPTimeout < 0 {
		return fmt.Errorf("ledger: webhook: endpoint %q durations must not be negative (zero selects the default)", c.Name)
	}
	if c.QueueSize < 1 {
		return fmt.Errorf("ledger: webhook: endpoint %q QueueSize must be >= 1, got %d", c.Name, c.QueueSize)
	}
	return nil
}

// webhookEndpointJSON is the LEDGER_WEBHOOKS wire form of one endpoint
// (see ParseWebhookEndpoints).
type webhookEndpointJSON struct {
	Name           string            `json:"name"`
	URL            string            `json:"url"`
	Events         []string          `json:"events"`
	Secret         string            `json:"secret"`
	KeyID          string            `json:"key_id"`
	Keys           []webhookKeyJSON  `json:"keys"`
	Enabled        *bool             `json:"enabled"`
	MaxAttempts    int               `json:"max_attempts"`
	InitialBackoff string            `json:"initial_backoff"`
	MaxBackoff     string            `json:"max_backoff"`
	HTTPTimeout    string            `json:"http_timeout"`
	QueueSize      int               `json:"queue_size"`
	KeyGracePeriod string            `json:"key_grace_period"`
}

type webhookKeyJSON struct {
	ID        string `json:"id"`
	Secret    string `json:"secret"`
	RetiredAt string `json:"retired_at"`
}

// ParseWebhookEndpoints parses LEDGER_WEBHOOKS: a JSON array of
// endpoint objects. Each endpoint needs a name, an absolute http(s)
// URL, and signing key material — either "secret" (single-key
// shorthand; "key_id" names it, default "primary") or "keys" (a list of
// {id, secret, retired_at?}; the first is the primary, the rest are
// retired keys still inside their grace period). "enabled" defaults to
// true; durations are Go duration strings ("500ms", "30s").
// An invalid value fails fast: a misconfigured webhook must never
// silently run unsigned or unfiltered.
func ParseWebhookEndpoints(raw string) ([]WebhookEndpointConfig, error) {
	var items []webhookEndpointJSON
	dec := json.NewDecoder(strings.NewReader(raw))
	if err := dec.Decode(&items); err != nil {
		return nil, fmt.Errorf("ledger: webhook: parse LEDGER_WEBHOOKS: %w", err)
	}
	if dec.More() {
		return nil, errors.New("ledger: webhook: parse LEDGER_WEBHOOKS: trailing data after JSON array")
	}
	out := make([]WebhookEndpointConfig, 0, len(items))
	seen := make(map[string]bool, len(items))
	for i, item := range items {
		cfg, err := parseWebhookEndpointJSON(item)
		if err != nil {
			return nil, fmt.Errorf("ledger: webhook: endpoint %d: %w", i, err)
		}
		if seen[cfg.Name] {
			return nil, fmt.Errorf("ledger: webhook: duplicate endpoint name %q", cfg.Name)
		}
		seen[cfg.Name] = true
		out = append(out, cfg)
	}
	return out, nil
}

func parseWebhookEndpointJSON(item webhookEndpointJSON) (WebhookEndpointConfig, error) {
	cfg := WebhookEndpointConfig{
		Name:        strings.TrimSpace(item.Name),
		URL:         strings.TrimSpace(item.URL),
		Events:      item.Events,
		Enabled:     true,
		MaxAttempts: item.MaxAttempts,
		QueueSize:   item.QueueSize,
	}
	if item.Enabled != nil {
		cfg.Enabled = *item.Enabled
	}
	parseDur := func(raw, field string) (time.Duration, error) {
		if raw == "" {
			return 0, nil
		}
		d, err := time.ParseDuration(raw)
		if err != nil {
			return 0, fmt.Errorf("invalid %s %q: %w", field, raw, err)
		}
		return d, nil
	}
	var err error
	if cfg.InitialBackoff, err = parseDur(item.InitialBackoff, "initial_backoff"); err != nil {
		return cfg, err
	}
	if cfg.MaxBackoff, err = parseDur(item.MaxBackoff, "max_backoff"); err != nil {
		return cfg, err
	}
	if cfg.HTTPTimeout, err = parseDur(item.HTTPTimeout, "http_timeout"); err != nil {
		return cfg, err
	}
	grace, err := parseDur(item.KeyGracePeriod, "key_grace_period")
	if err != nil {
		return cfg, err
	}
	switch {
	case len(item.Keys) > 0 && item.Secret != "":
		return cfg, errors.New(`"secret" and "keys" are mutually exclusive`)
	case len(item.Keys) > 0:
		ring, err := buildKeyRing(item.Keys, grace)
		if err != nil {
			return cfg, err
		}
		cfg.Keys = ring
	case item.Secret != "":
		id := item.KeyID
		if id == "" {
			id = "primary"
		}
		ring, err := NewRotatingKey(id, []byte(item.Secret), grace)
		if err != nil {
			return cfg, err
		}
		cfg.Keys = ring
	default:
		return cfg, errors.New("signing key material is required (\"secret\" or \"keys\"): unsigned webhooks can be forged")
	}
	// Defaults resolve before validation: a zero WebhookEndpointConfig
	// is the documented default setup, and Validate enforces the
	// effective config (MaxAttempts >= 1 after defaulting).
	cfg = cfg.withDefaults()
	if err := cfg.Validate(); err != nil {
		return WebhookEndpointConfig{}, err
	}
	return cfg, nil
}

// buildKeyRing builds a RotatingKey from the "keys" list: the first
// entry becomes the primary, later entries are retired keys (their
// retired_at marks when rotation happened; they verify until
// retired_at+grace).
func buildKeyRing(items []webhookKeyJSON, grace time.Duration) (*RotatingKey, error) {
	if grace < 0 {
		return nil, errors.New("negative key_grace_period")
	}
	if grace == 0 {
		grace = DefaultWebhookKeyGracePeriod
	}
	first := items[0]
	if first.ID == "" || first.Secret == "" {
		return nil, errors.New("keys[0]: id and secret are required")
	}
	ring, err := NewRotatingKey(first.ID, []byte(first.Secret), grace)
	if err != nil {
		return nil, err
	}
	ring.mu.Lock()
	defer ring.mu.Unlock()
	for i := 1; i < len(items); i++ {
		it := items[i]
		if it.ID == "" || it.Secret == "" {
			return nil, fmt.Errorf("keys[%d]: id and secret are required", i)
		}
		if _, dup := ring.keys[it.ID]; dup {
			return nil, fmt.Errorf("keys[%d]: duplicate key id %q", i, it.ID)
		}
		var retiredAt time.Time
		if it.RetiredAt != "" {
			retiredAt, err = time.Parse(time.RFC3339, it.RetiredAt)
			if err != nil {
				return nil, fmt.Errorf("keys[%d]: invalid retired_at %q: %w", i, it.RetiredAt, err)
			}
		} else {
			retiredAt = time.Now().UTC()
		}
		cp := make([]byte, len(it.Secret))
		copy(cp, it.Secret)
		ring.keys[it.ID] = &WebhookKey{ID: it.ID, Secret: cp, RetiredAt: retiredAt.UTC()}
	}
	ring.purgeExpiredLocked(time.Now())
	return ring, nil
}

// webhookEndpoint is a registered endpoint plus its delivery worker.
type webhookEndpoint struct {
	cfg    WebhookEndpointConfig
	queue  chan OutboxEvent
	client *http.Client
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	wg     sync.WaitGroup
	d      *WebhookDispatcher
}

func (e *webhookEndpoint) matches(eventType string) bool {
	if len(e.cfg.Events) == 0 {
		return true
	}
	for _, t := range e.cfg.Events {
		if t == eventType {
			return true
		}
	}
	return false
}

func (e *webhookEndpoint) start() {
	e.wg.Add(1)
	go e.run()
}

func (e *webhookEndpoint) stop() {
	close(e.done)
	e.cancel()
	e.wg.Wait()
}

// run drains the endpoint's queue serially: one event's retries fully
// complete (or exhaust) before the next event starts, so per-endpoint
// delivery order matches outbox seq order.
func (e *webhookEndpoint) run() {
	defer e.wg.Done()
	for {
		select {
		case <-e.done:
			// Prompt shutdown: queued-but-undelivered events are
			// dropped and counted — they never left the process, so
			// the outbox stream stays the durable record.
			n := 0
		drain:
			for {
				select {
				case <-e.queue:
					n++
				default:
					break drain
				}
			}
			if n > 0 {
				e.d.dropped.Add(uint64(n))
			}
			return
		case ev, ok := <-e.queue:
			if !ok {
				return
			}
			e.deliver(ev)
		}
	}
}

// deliver POSTs one event with its HMAC signature, retrying with
// exponential backoff plus jitter up to MaxAttempts. Network errors,
// 429s and 5xx retry; other 4xx are terminal (the receiver rejects the
// payload — retrying the same bytes will not help). Every terminal
// outcome updates the delivery record; an exhausted delivery counts
// one webhook failure. It never touches the ledger: the worker holds
// no ledger lock, so a slow endpoint cannot stall Post/Transfer.
func (e *webhookEndpoint) deliver(ev OutboxEvent) {
	body, err := json.Marshal(ev)
	if err != nil {
		// OutboxEvent is JSON-safe by construction; a marshal failure
		// is a programming bug, not a retryable webhook fault.
		e.d.appendRecord(DeliveryRecord{
			EventSeq: ev.Seq, Endpoint: e.cfg.Name,
			Attempts: 0, Status: WebhookDeliveryFailed,
			LastError: "marshal: " + err.Error(), At: time.Now().UTC(),
		})
		e.d.failures.Add(1)
		return
	}
	sig, keyID, err := e.cfg.Keys.Sign(body)
	if err != nil {
		e.d.appendRecord(DeliveryRecord{
			EventSeq: ev.Seq, Endpoint: e.cfg.Name,
			Attempts: 0, Status: WebhookDeliveryFailed,
			LastError: "sign: " + err.Error(), At: time.Now().UTC(),
		})
		e.d.failures.Add(1)
		return
	}
	rec := e.d.appendRecord(DeliveryRecord{
		EventSeq: ev.Seq, Endpoint: e.cfg.Name,
		Status: WebhookDeliveryPending, At: time.Now().UTC(),
	})
	backoff := e.cfg.InitialBackoff
	for attempt := 1; ; attempt++ {
		e.d.updateRecord(rec, func(r *DeliveryRecord) { r.Attempts = attempt })
		status, perr := e.post(ev, body, sig, keyID)
		if perr == nil && status >= 200 && status < 300 {
			e.d.updateRecord(rec, func(r *DeliveryRecord) {
				r.Status = WebhookDeliveryDelivered
				r.LastError = ""
			})
			e.d.delivered.Add(1)
			return
		}
		e.d.failedAttempts.Add(1)
		lastErr := describeDeliveryFailure(status, perr)
		retryable := perr != nil || status == http.StatusTooManyRequests || status >= 500
		if !retryable || attempt >= e.cfg.MaxAttempts {
			e.d.updateRecord(rec, func(r *DeliveryRecord) {
				r.Status = WebhookDeliveryFailed
				r.LastError = lastErr
			})
			e.d.failures.Add(1)
			return
		}
		e.d.updateRecord(rec, func(r *DeliveryRecord) { r.LastError = lastErr })
		if !e.sleepOrDone(webhookJitteredBackoff(backoff)) {
			e.d.updateRecord(rec, func(r *DeliveryRecord) {
				r.Status = WebhookDeliveryFailed
				r.LastError = "dispatcher stopping"
			})
			e.d.failures.Add(1)
			return
		}
		backoff *= 2
		if backoff > e.cfg.MaxBackoff {
			backoff = e.cfg.MaxBackoff
		}
	}
}

// post sends one signed delivery attempt. It returns the HTTP status
// (0 on transport failure) and the transport error, if any.
func (e *webhookEndpoint) post(ev OutboxEvent, body []byte, sig, keyID string) (int, error) {
	req, err := http.NewRequestWithContext(e.ctx, http.MethodPost, e.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Ledger-Signature", sig)
	req.Header.Set("X-Ledger-Key-Id", keyID)
	req.Header.Set("X-Ledger-Event-Type", ev.Type)
	req.Header.Set("X-Ledger-Event-Seq", fmt.Sprintf("%d", ev.Seq))
	resp, err := e.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, nil
}

// sleepOrDone waits d, returning false when the endpoint is stopping.
// A non-positive d returns immediately.
func (e *webhookEndpoint) sleepOrDone(d time.Duration) bool {
	if d <= 0 {
		select {
		case <-e.done:
			return false
		default:
			return true
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-e.done:
		return false
	case <-t.C:
		return true
	}
}

// describeDeliveryFailure renders one failed attempt for the delivery
// record: the transport error, or the HTTP status when the receiver
// answered.
func describeDeliveryFailure(status int, perr error) string {
	if perr != nil {
		return perr.Error()
	}
	return fmt.Sprintf("http %d", status)
}

// webhookJitteredBackoff adds up to +25% jitter to the backoff: enough
// to decorrelate endpoint retries after a shared outage, small enough
// to keep the backoff schedule recognizable.
func webhookJitteredBackoff(backoff time.Duration) time.Duration {
	if backoff <= 0 {
		return 0
	}
	return backoff + time.Duration(rand.Int63n(int64(backoff)/4+1))
}

// WebhookDispatcher is the outbox's webhook subscriber: an endpoint
// registry plus one background delivery worker per endpoint. The zero
// value is unusable — build one with NewWebhookDispatcher.
//
// Notify is safe to call from the commit critical section (see
// emitOutboxLocked): it only takes the registry read lock and performs
// non-blocking queue sends, so it can neither block nor deadlock the
// posting path.
type WebhookDispatcher struct {
	mu         sync.RWMutex
	endpoints  map[string]*webhookEndpoint
	order      []string
	records    []*DeliveryRecord
	maxRecords int

	delivered      atomic.Uint64
	failedAttempts atomic.Uint64
	failures       atomic.Uint64
	dropped        atomic.Uint64

	stopOnce sync.Once
	stopped  atomic.Bool
}

// NewWebhookDispatcher builds a dispatcher and registers every config.
// It fails fast on the first invalid or duplicate endpoint: a
// half-registered dispatcher would forward to some endpoints while
// silently missing others. Endpoints start their delivery workers on
// registration.
func NewWebhookDispatcher(cfgs ...WebhookEndpointConfig) (*WebhookDispatcher, error) {
	d := &WebhookDispatcher{
		endpoints:  make(map[string]*webhookEndpoint),
		maxRecords: DefaultWebhookMaxRecords,
	}
	for _, cfg := range cfgs {
		if err := d.Register(cfg); err != nil {
			d.Stop()
			return nil, err
		}
	}
	return d, nil
}

// Register adds an endpoint and starts its delivery worker. The name
// must be unique; re-registering a name fails — Unregister first.
func (d *WebhookDispatcher) Register(cfg WebhookEndpointConfig) error {
	cfg = cfg.withDefaults()
	if err := cfg.Validate(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped.Load() {
		return errors.New("ledger: webhook: dispatcher is stopped")
	}
	if _, dup := d.endpoints[cfg.Name]; dup {
		return fmt.Errorf("ledger: webhook: duplicate endpoint name %q", cfg.Name)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ep := &webhookEndpoint{
		cfg:    cfg,
		queue:  make(chan OutboxEvent, cfg.QueueSize),
		client: &http.Client{Timeout: cfg.HTTPTimeout},
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
		d:      d,
	}
	d.endpoints[cfg.Name] = ep
	d.order = append(d.order, cfg.Name)
	ep.start()
	return nil
}

// Unregister removes an endpoint and stops its worker after draining:
// events already queued are still delivered (their retries run to
// completion); new events stop matching immediately. It returns false
// when the name is unknown.
func (d *WebhookDispatcher) Unregister(name string) bool {
	d.mu.Lock()
	ep, ok := d.endpoints[name]
	if !ok {
		d.mu.Unlock()
		return false
	}
	delete(d.endpoints, name)
	for i, n := range d.order {
		if n == name {
			d.order = append(d.order[:i], d.order[i+1:]...)
			break
		}
	}
	d.mu.Unlock()
	close(ep.queue)
	ep.stop()
	return true
}

// SetEnabled flips an endpoint's delivery gate: disabling stops new
// events from queueing (in-flight deliveries finish); re-enabling
// resumes. It returns false when the name is unknown.
func (d *WebhookDispatcher) SetEnabled(name string, enabled bool) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	ep, ok := d.endpoints[name]
	if !ok {
		return false
	}
	ep.cfg.Enabled = enabled
	return true
}

// EndpointNames lists registered endpoint names in registration order.
func (d *WebhookDispatcher) EndpointNames() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]string, len(d.order))
	copy(out, d.order)
	return out
}

// RotateKey rotates one endpoint's signing-key ring (see
// RotatingKey.Rotate). It returns false when the endpoint is unknown.
func (d *WebhookDispatcher) RotateKey(endpoint, newID string, newSecret []byte) error {
	d.mu.RLock()
	ep, ok := d.endpoints[endpoint]
	d.mu.RUnlock()
	if !ok {
		return fmt.Errorf("ledger: webhook: unknown endpoint %q", endpoint)
	}
	return ep.cfg.Keys.Rotate(newID, newSecret)
}

// Notify fans one outbox event out to every enabled endpoint whose
// event filter matches. Sends are non-blocking: a full endpoint queue
// drops the event (counted) instead of stalling the commit. A stopped
// dispatcher ignores the event.
func (d *WebhookDispatcher) Notify(ev OutboxEvent) {
	if d.stopped.Load() {
		return
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, name := range d.order {
		ep := d.endpoints[name]
		if !ep.cfg.Enabled || !ep.matches(ev.Type) {
			continue
		}
		select {
		case ep.queue <- ev:
		default:
			// The queue is full: drop, don't block. The outbox
			// stream stays the durable record; the webhook is a
			// live fan-out and a slow consumer must not stall the
			// ledger's commit path.
			d.dropped.Add(1)
		}
	}
}

// appendRecord records a new delivery; the returned pointer is the
// handle for updateRecord. The ring keeps the newest maxRecords
// records. The pointer stays valid for the record's whole delivery:
// per endpoint, deliveries are serial, so an in-flight record is
// always among the newest and can never be evicted mid-delivery.
func (d *WebhookDispatcher) appendRecord(r DeliveryRecord) *DeliveryRecord {
	d.mu.Lock()
	defer d.mu.Unlock()
	rec := &DeliveryRecord{
		EventSeq: r.EventSeq, Endpoint: r.Endpoint, Status: r.Status, At: r.At,
	}
	d.records = append(d.records, rec)
	if len(d.records) > d.maxRecords {
		copy(d.records, d.records[1:])
		d.records = d.records[:len(d.records)-1]
	}
	return rec
}

// updateRecord mutates a delivery record obtained from appendRecord.
func (d *WebhookDispatcher) updateRecord(rec *DeliveryRecord, fn func(*DeliveryRecord)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	fn(rec)
}

// Deliveries returns delivery records latest-first, filtered by the
// query. The result is never nil.
func (d *WebhookDispatcher) Deliveries(q WebhookDeliveryQuery) []DeliveryRecord {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]DeliveryRecord, 0, len(d.records))
	for i := len(d.records) - 1; i >= 0; i-- {
		rec := d.records[i]
		if q.Endpoint != "" && rec.Endpoint != q.Endpoint {
			continue
		}
		if q.Status != "" && rec.Status != q.Status {
			continue
		}
		out = append(out, *rec)
		if q.Limit > 0 && len(out) >= q.Limit {
			break
		}
	}
	return out
}

// Stats reports the dispatcher's counters: events delivered (HTTP 2xx),
// failed POST attempts (network errors and non-2xx responses,
// including the final attempts of failed deliveries), deliveries that
// exhausted their retries, and events dropped on full endpoint queues.
func (d *WebhookDispatcher) Stats() (delivered, failedAttempts, failures, dropped uint64) {
	return d.delivered.Load(), d.failedAttempts.Load(), d.failures.Load(), d.dropped.Load()
}

// Stop halts every endpoint worker and waits for in-flight deliveries
// to settle. Queued-but-undelivered events are dropped and counted. It
// is idempotent.
func (d *WebhookDispatcher) Stop() {
	d.stopOnce.Do(func() {
		d.stopped.Store(true)
		d.mu.Lock()
		eps := make([]*webhookEndpoint, 0, len(d.order))
		for _, name := range d.order {
			eps = append(eps, d.endpoints[name])
		}
		d.mu.Unlock()
		for _, ep := range eps {
			ep.stop()
		}
	})
}

// WithWebhooks attaches the outbound webhook dispatcher (see the
// package doc): every durably emitted outbox event is fanned out to
// the dispatcher's registered endpoints. A nil dispatcher disables
// forwarding entirely. Like WithOutbox, the option propagates to
// lazily-provisioned tenant ledgers through the tenant store.
func WithWebhooks(d *WebhookDispatcher) Option {
	return func(l *Ledger) {
		l.webhooks = d
	}
}

// Webhooks reports the ledger's attached dispatcher, or nil when
// WithWebhooks was never used.
func (l *Ledger) Webhooks() *WebhookDispatcher {
	return l.webhooks
}

// emitOutboxPain001StatusLocked emits a pain001_status_changed domain
// event for one pain.001 status move — instruction-level (txID empty)
// or per-transfer (the bank's accepted/rejected verdict on one
// transfer). Callers must hold l.mu with the write lock.
func (l *Ledger) emitOutboxPain001StatusLocked(msgID, txID, from, to, reason, actor string) {
	if l.outbox == nil {
		return
	}
	details := map[string]any{
		"msg_id": msgID,
		"from":   from,
		"to":     to,
	}
	if txID != "" {
		details["tx_id"] = txID
	}
	if reason != "" {
		details["reason"] = reason
	}
	if actor != "" {
		details["actor"] = actor
	}
	l.emitOutboxLocked(OutboxEvent{Type: OutboxPain001StatusChanged, Details: details})
}
