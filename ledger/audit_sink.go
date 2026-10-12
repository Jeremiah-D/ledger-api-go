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
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Audit-event webhook sink (LG-67): an opt-in forwarder that streams
// the outbox's domain events (see outbox.go) to an external HTTPS
// endpoint as signed JSON webhooks.
//
// Delivery contract, stated honestly:
//
//   - The sink never touches the posting path: it tails the outbox
//     files on its own goroutine, so a slow or dead webhook cannot
//     stall Post/Transfer. A crash or restart resumes from the
//     persisted cursor.
//   - At-least-once: an event is delivered (HTTP 2xx), then the cursor
//     advances. A crash between delivery and the cursor write replays
//     the event after restart — downstream must dedupe on
//     (type, seq), like any other outbox consumer.
//   - Gaps are possible by design: when an event exhausts MaxAttempts
//     it is dead-lettered — recorded as an audit_sink_dead_letter
//     audit event, counted, and skipped — and the cursor moves past it.
//     The stream then has a hole at that seq; the dead-letter audit
//     event is the record of it. Alert on the dead-letter counter.
//   - First start begins at the current outbox head: history is NOT
//     replayed. The sink forwards new domain facts; it is not a
//     backfill tool.
//
// Every delivery is signed: HMAC-SHA256(secret, body), hex-encoded,
// sent as X-Ledger-Signature: sha256=<hex>, alongside
// X-Ledger-Event-Type and X-Ledger-Event-Seq. The secret is mandatory
// (see SinkConfig): an unsigned webhook can be forged, so the sink
// refuses to start without one.

// Default sink tunables (see SinkConfig): 10 attempts with backoff
// from 1s doubling to 60s, a 1s idle poll, and a 10s per-attempt HTTP
// timeout.
const (
	DefaultSinkMaxAttempts    = 10
	DefaultSinkInitialBackoff = time.Second
	DefaultSinkMaxBackoff     = 60 * time.Second
	DefaultSinkPollInterval   = time.Second
	DefaultSinkHTTPTimeout    = 10 * time.Second
)

// sinkCursorFilename is the default cursor file name inside the outbox
// directory; sinkPollLimit caps one outbox poll page.
const (
	sinkCursorFilename = "audit-sink.cursor"
	sinkPollLimit      = 100
)

// SinkConfig configures the audit-sink webhook forwarder. Zero values
// select the documented defaults, so SinkConfig{URL, Secret} alone is a
// complete configuration.
type SinkConfig struct {
	// URL is the webhook endpoint receiving every outbox domain event
	// as a JSON POST. Required; must be an absolute http(s) URL.
	URL string
	// Secret signs every delivery: HMAC-SHA256(secret, body), sent as
	// X-Ledger-Signature: sha256=<hex>. Required — an unsigned webhook
	// can be forged, so the sink refuses to start without one.
	Secret []byte
	// MaxAttempts bounds the POST attempts per event, including the
	// first try. Default DefaultSinkMaxAttempts.
	MaxAttempts int
	// InitialBackoff is the wait before the first retry; it doubles
	// per retry up to MaxBackoff. Defaults DefaultSinkInitialBackoff
	// and DefaultSinkMaxBackoff.
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	// PollInterval is the idle wait between outbox polls when no new
	// events are waiting. Default DefaultSinkPollInterval.
	PollInterval time.Duration
	// HTTPTimeout bounds a single POST attempt. Default
	// DefaultSinkHTTPTimeout.
	HTTPTimeout time.Duration
	// CursorFile persists the last delivered event seq across restarts
	// (atomic write). Default <outboxdir>/audit-sink.cursor.
	CursorFile string
}

// Validate rejects an unusable sink config: a missing or non-http(s)
// URL, an empty secret (unsigned webhooks can be forged — fail fast,
// never silently forward unsigned), a MaxAttempts below 1, or a
// negative duration. Callers that want zero values to mean "use the
// default" must run withDefaults first — StartAuditSink does.
func (c SinkConfig) Validate() error {
	if c.URL == "" {
		return errors.New("ledger: audit sink: URL is required")
	}
	u, err := url.Parse(c.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("ledger: audit sink: invalid URL %q: want an absolute http(s) URL", c.URL)
	}
	if len(c.Secret) == 0 {
		return errors.New("ledger: audit sink: secret is required: unsigned webhooks can be forged")
	}
	if c.MaxAttempts < 1 {
		return fmt.Errorf("ledger: audit sink: MaxAttempts must be >= 1, got %d", c.MaxAttempts)
	}
	if c.InitialBackoff < 0 || c.MaxBackoff < 0 || c.PollInterval < 0 || c.HTTPTimeout < 0 {
		return errors.New("ledger: audit sink: durations must not be negative (zero selects the default)")
	}
	return nil
}

// withDefaults fills zero tunables with the documented defaults. The
// URL, secret, and cursor file are left untouched.
func (c SinkConfig) withDefaults() SinkConfig {
	if c.MaxAttempts < 1 {
		c.MaxAttempts = DefaultSinkMaxAttempts
	}
	if c.InitialBackoff <= 0 {
		c.InitialBackoff = DefaultSinkInitialBackoff
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = DefaultSinkMaxBackoff
	}
	if c.InitialBackoff > c.MaxBackoff {
		c.InitialBackoff = c.MaxBackoff
	}
	if c.PollInterval <= 0 {
		c.PollInterval = DefaultSinkPollInterval
	}
	if c.HTTPTimeout <= 0 {
		c.HTTPTimeout = DefaultSinkHTTPTimeout
	}
	return c
}

// AuditSink forwards outbox domain events to a webhook endpoint. It
// owns one background goroutine; Stop (or context cancellation) halts
// it. The zero value is unusable — build one with StartAuditSink.
type AuditSink struct {
	l          *Ledger
	dir        string
	cfg        SinkConfig
	client     *http.Client
	cursorPath string

	done     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	cursor         uint64 // last delivered seq; only the run goroutine touches it
	sent           atomic.Uint64
	failedAttempts atomic.Uint64
	deadLetters    atomic.Uint64
}

// StartAuditSink launches the background forwarder for l's outbox. It
// fails when the config is invalid, when the ledger has no outbox
// attached (WithOutbox was never used — there is nothing to forward),
// when the outbox directory is unavailable, or when the cursor file is
// unreadable. A missing cursor file starts the sink at the current
// outbox head: history is not replayed (see the package doc).
//
// The sink never blocks the posting path: it tails the outbox files on
// its own goroutine, and a slow webhook only delays the cursor, never
// a Post.
func StartAuditSink(ctx context.Context, l *Ledger, cfg SinkConfig) (*AuditSink, error) {
	if l == nil {
		return nil, errors.New("ledger: audit sink: nil ledger")
	}
	// Defaults resolve before validation: a zero SinkConfig is the
	// documented default setup, and Validate enforces the effective
	// config (MaxAttempts >= 1 after defaulting).
	cfg = cfg.withDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	dir, ok := l.OutboxDir()
	if !ok {
		return nil, errors.New("ledger: audit sink: ledger has no outbox attached (WithOutbox): nothing to forward")
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("ledger: audit sink: outbox dir %q unavailable: %v", dir, err)
	}
	cursorPath := cfg.CursorFile
	if cursorPath == "" {
		cursorPath = filepath.Join(dir, sinkCursorFilename)
	}
	cursor, found, err := readSinkCursor(cursorPath)
	if err != nil {
		return nil, err
	}
	if !found {
		head, _, err := scanOutboxMaxSeq(dir)
		if err != nil {
			return nil, fmt.Errorf("ledger: audit sink: scan outbox head: %w", err)
		}
		cursor = head
		if err := writeSinkCursor(cursorPath, cursor); err != nil {
			return nil, fmt.Errorf("ledger: audit sink: persist initial cursor: %w", err)
		}
	}
	s := &AuditSink{
		l:          l,
		dir:        dir,
		cfg:        cfg,
		client:     &http.Client{Timeout: cfg.HTTPTimeout},
		cursorPath: cursorPath,
		cursor:     cursor,
		done:       make(chan struct{}),
	}
	s.wg.Add(1)
	go s.run(ctx)
	return s, nil
}

// Stop halts the forwarder and waits for its goroutine to exit. It is
// idempotent and safe to call alongside context cancellation.
func (s *AuditSink) Stop() {
	s.stopOnce.Do(func() { close(s.done) })
	s.wg.Wait()
}

// Stats reports the sink's delivery counters: events delivered (HTTP
// 2xx), failed POST attempts (network errors and non-2xx responses,
// including the final attempts of dead-lettered events), and events
// dead-lettered after exhausting retries.
func (s *AuditSink) Stats() (sent, failedAttempts, deadLetters uint64) {
	return s.sent.Load(), s.failedAttempts.Load(), s.deadLetters.Load()
}

// Cursor reports the last delivered event seq (the persisted cursor
// lags at most one event behind it).
func (s *AuditSink) Cursor() uint64 { return s.cursor }

func (s *AuditSink) run(ctx context.Context) {
	defer s.wg.Done()
	for {
		if !sinkAlive(ctx, s.done) {
			return
		}
		events, _, _, err := ReadOutboxEvents(s.dir, OutboxQuery{AfterSeq: s.cursor, Limit: sinkPollLimit})
		if err != nil {
			// The outbox read is best-effort: a transient disk error
			// must not kill the forwarder — the next poll retries.
			fmt.Fprintf(os.Stderr, "ledger: audit sink: outbox read: %v\n", err)
			if !sinkSleep(ctx, s.done, s.cfg.PollInterval) {
				return
			}
			continue
		}
		if len(events) == 0 {
			if !sinkSleep(ctx, s.done, s.cfg.PollInterval) {
				return
			}
			continue
		}
		for _, ev := range events {
			if !s.deliver(ctx, ev) {
				return
			}
			s.cursor = ev.Seq
			if err := writeSinkCursor(s.cursorPath, s.cursor); err != nil {
				// The cursor still advances in memory; a restart
				// replays from the last persisted cursor and the
				// downstream dedupes (at-least-once). Losing the
				// process over a cursor write would be worse.
				fmt.Fprintf(os.Stderr, "ledger: audit sink: persist cursor: %v\n", err)
			}
		}
	}
}

// deliver POSTs one event with its HMAC signature, retrying with
// exponential backoff up to MaxAttempts. It returns false only when
// the sink is stopping mid-delivery; both a delivered event and a
// dead-lettered one advance the cursor — the cursor must never stick
// on poison.
func (s *AuditSink) deliver(ctx context.Context, ev OutboxEvent) bool {
	body, err := json.Marshal(ev)
	if err != nil {
		// OutboxEvent is JSON-safe by construction; a marshal failure
		// is a programming bug, not a retryable webhook fault.
		// Dead-letter it rather than spinning on poison.
		s.deadLetter(ev, 0, 0, "marshal: "+err.Error())
		return true
	}
	mac := hmac.New(sha256.New, s.cfg.Secret)
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	backoff := s.cfg.InitialBackoff
	for attempt := 1; ; attempt++ {
		status, perr := s.post(ctx, ev, body, sig)
		if perr == nil && status >= 200 && status < 300 {
			s.sent.Add(1)
			return true
		}
		s.failedAttempts.Add(1)
		lastErr := ""
		if perr != nil {
			lastErr = perr.Error()
		}
		if attempt >= s.cfg.MaxAttempts {
			s.deadLetter(ev, attempt, status, lastErr)
			return true
		}
		if !sinkSleep(ctx, s.done, backoff) {
			return false
		}
		backoff *= 2
		if backoff > s.cfg.MaxBackoff {
			backoff = s.cfg.MaxBackoff
		}
	}
}

// post sends one signed delivery attempt. It returns the HTTP status
// (0 on transport failure) and the transport error, if any.
func (s *AuditSink) post(ctx context.Context, ev OutboxEvent, body []byte, sig string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Ledger-Signature", sig)
	req.Header.Set("X-Ledger-Event-Type", ev.Type)
	req.Header.Set("X-Ledger-Event-Seq", strconv.FormatUint(ev.Seq, 10))
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, nil
}

// deadLetter records an undeliverable event as an audit_sink_dead_letter
// audit event (the compliance record of the gap), counts it, and lets
// the cursor move past it. emitAudit's contract wants l.mu held; the
// sink holds no other lock, so a brief read lock is deadlock-free and
// never stalls the posting path.
func (s *AuditSink) deadLetter(ev OutboxEvent, attempts, lastStatus int, lastErr string) {
	s.deadLetters.Add(1)
	details := map[string]any{
		"event_seq":   ev.Seq,
		"event_type":  ev.Type,
		"attempts":    attempts,
		"last_status": lastStatus,
	}
	if lastErr != "" {
		details["last_error"] = lastErr
	}
	s.l.mu.RLock()
	s.l.emitAudit(AuditEvent{Op: "audit_sink_dead_letter", Actor: "AuditSink", Details: details})
	s.l.mu.RUnlock()
}

// readSinkCursor loads the persisted cursor: (seq, true, nil) when the
// file exists and parses, (0, false, nil) when it is absent (first
// start), and an error when it is corrupt — a corrupt cursor fails the
// start loudly rather than silently replaying or skipping the stream.
func readSinkCursor(path string) (uint64, bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("ledger: audit sink: read cursor: %w", err)
	}
	s := strings.TrimSpace(string(raw))
	seq, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("ledger: audit sink: corrupt cursor file %q", path)
	}
	return seq, true, nil
}

// writeSinkCursor persists the cursor atomically: write to a temp file,
// fsync it, then rename over the cursor file, so a crash can never
// leave a torn cursor behind.
func writeSinkCursor(path string, seq uint64) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("ledger: audit sink: write cursor: %w", err)
	}
	_, werr := io.WriteString(f, strconv.FormatUint(seq, 10)+"\n")
	serr := f.Sync()
	cerr := f.Close()
	if werr != nil {
		return fmt.Errorf("ledger: audit sink: write cursor: %w", werr)
	}
	if serr != nil {
		return fmt.Errorf("ledger: audit sink: fsync cursor: %w", serr)
	}
	if cerr != nil {
		return fmt.Errorf("ledger: audit sink: close cursor: %w", cerr)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("ledger: audit sink: rename cursor: %w", err)
	}
	return nil
}

// sinkAlive reports whether the sink should keep running.
func sinkAlive(ctx context.Context, done <-chan struct{}) bool {
	select {
	case <-ctx.Done():
		return false
	case <-done:
		return false
	default:
		return true
	}
}

// sinkSleep waits d, returning false when the sink is stopping. A
// non-positive d returns the liveness immediately.
func sinkSleep(ctx context.Context, done <-chan struct{}, d time.Duration) bool {
	if d <= 0 {
		return sinkAlive(ctx, done)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-done:
		return false
	case <-t.C:
		return true
	}
}
