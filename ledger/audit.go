package ledger

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// AuditEvent is one structured record in the ledger's compliance audit
// log. Every mutating ledger operation (Post, PostTransfer, PostSweep,
// PostMerge, holds, Freeze/Unfreeze) and every Reconcile run emits one
// after it commits, so the log is a complete, ordered history of what the
// ledger did — the "who did what, when, and what changed" trail a payment
// operation needs for compliance and incident review.
//
// The fields are deliberately small and stable:
//   - Op names the operation ("post", "transfer", "sweep", "merge",
//     "hold", "hold_capture", "hold_release", "hold_expire", "freeze",
//     "unfreeze", "reconcile").
//   - Actor names the in-process ledger entrypoint that performed it
//     ("Post", "PostTransfer", ...). HTTP handlers call through these
//     entrypoints; correlate with the HTTP access log by timestamp for
//     client attribution.
//   - TraceID correlates related records: the entry ID for a Post, the
//     transfer/sweep/merge ID for a compound operation (all of its legs
//     share it), the hold ID for hold operations, the account for a
//     freeze, the reconciled version for a reconcile run.
//   - VersionBefore/VersionAfter bracket the ledger version around the
//     operation. Read-only operations (freeze, reconcile, hold release)
//     report the same version twice — the field still proves the log
//     line was written against a known ledger state.
//   - EntryIDs lists the journal entries the operation committed, in
//     commit order; empty for operations that book nothing.
//   - Details carries op-specific facts (amounts, currencies, frozen
//     state, expired counts) as plain JSON values.
type AuditEvent struct {
	Timestamp     time.Time      `json:"ts"`
	Op            string         `json:"op"`
	Actor         string         `json:"actor"`
	TraceID       string         `json:"trace_id"`
	// Tenant names the tenant namespace the event belongs to (see
	// tenant.go). Empty for the default tenant — events from a
	// single-tenant deployment look exactly as before. The seal covers
	// it like every other field.
	Tenant        string         `json:"tenant,omitempty"`
	VersionBefore uint64         `json:"version_before"`
	VersionAfter  uint64         `json:"version_after"`
	EntryIDs      []string       `json:"entry_ids,omitempty"`
	Accounts      []AccountID    `json:"accounts,omitempty"`
	Details       map[string]any `json:"details,omitempty"`
	// PrevHash links this entry to the previous one: the hex SHA-256
	// seal of the entry sealed immediately before it (LG-32). The
	// first entry of the log carries AuditGenesisPrevHash. Because
	// the link is assigned at enqueue time, it survives daily and
	// size rotations — a new file's first entry points at the
	// previous file's last entry — and gzip archiving.
	PrevHash string `json:"prev_hash"`
	// Hash is the hex SHA-256 seal of this entry: SHA-256 over the
	// entry's canonical serialization (fixed field order, excluding
	// Hash itself) concatenated with PrevHash. Any rewrite of the
	// entry's content, or any deletion/reorder in the log, breaks
	// the recomputation — see VerifyAuditLog.
	Hash string `json:"hash"`
	// Sig carries the per-line HMAC-SHA256 signature when the log runs
	// with signing keys (LG-65, see audit_sign.go): the MAC covers the
	// line's canonical bytes with the sig field itself absent. Nil on
	// unsigned lines — old and unsigned lines parse unchanged, and the
	// hash chain (which seals over the canonical form without Sig) is
	// unaffected either way.
	Sig *AuditSig `json:"sig,omitempty"`
}

// DefaultAuditMaxBytes is the default per-file size cap for audit files:
// 100 MiB. A busy ledger rotates long before a file gets unwieldy, and
// the cap bounds how much a single file can grow between rotations.
const DefaultAuditMaxBytes = 100 * 1024 * 1024

// DefaultAuditQueueSize bounds the in-memory channel between ledger
// operations and the background writer. Log is non-blocking: when the
// queue is full the event is dropped and counted (see Dropped), so a
// slow disk can never stall Post.
const DefaultAuditQueueSize = 4096

// DefaultAuditFlushInterval is the default fsync cadence for audit
// files: 1s. The writer fsyncs at most once per interval, plus once
// every 128 written events (whichever comes first) — a longer interval
// widens the crash-loss window, a shorter one costs more fsyncs.
// Revisable at runtime via Ledger.SetAuditLogFlushInterval (LG-342).
const DefaultAuditFlushInterval = time.Second

// auditLogFilename lays out audit files as
// audit-2006-01-02.jsonl, with -NNN suffixes when a day rotates on size:
// audit-2006-01-02-001.jsonl. Days are UTC so rotation is deterministic
// regardless of the process timezone.
func auditLogFilename(day string, seq int) string {
	if seq == 0 {
		return fmt.Sprintf("audit-%s.jsonl", day)
	}
	return fmt.Sprintf("audit-%s-%03d.jsonl", day, seq)
}

// AuditLogOption configures an AuditLog.
type AuditLogOption func(*auditLogConfig)

type auditLogConfig struct {
	maxBytes  int64
	queueSize int
	// flushInterval is the fsync cadence for the background writer
	// (LG-342): how often the writer calls f.Sync() on the current
	// audit file, on top of the every-128-events sync in run. 0 is
	// never stored — NewAuditLog defaults it to
	// DefaultAuditFlushInterval, and the option below keeps the
	// default for non-positive values.
	flushInterval time.Duration
	// retentionDays is the audit-log retention window in whole days
	// (LG-137, see audit_retention.go): ApplyAuditRetention deletes
	// whole audit files older than the window. 0 (the default) keeps
	// everything; negative values fail NewAuditLog.
	retentionDays int
	// signingKeys enables per-line HMAC-SHA256 signatures (LG-65). Empty
	// means the log runs unsigned — degraded mode, with an
	// audit_signing_disabled alert recorded in the trail itself.
	signingKeys  []AuditSigningKey
	signingGrace time.Duration
}

// WithAuditLogMaxBytes caps a single audit file at n bytes; the writer
// rotates to a new file past the cap. Non-positive values keep the
// default.
func WithAuditLogMaxBytes(n int64) AuditLogOption {
	return func(c *auditLogConfig) {
		if n > 0 {
			c.maxBytes = n
		}
	}
}

// WithAuditLogQueueSize bounds the async event queue. Non-positive values
// keep the default.
func WithAuditLogQueueSize(n int) AuditLogOption {
	return func(c *auditLogConfig) {
		if n > 0 {
			c.queueSize = n
		}
	}
}

// WithAuditLogFlushInterval sets how often the background writer fsyncs
// the audit file (LG-342). A longer interval widens the crash-loss
// window; a shorter one costs more fsyncs. Non-positive values keep
// the default (DefaultAuditFlushInterval, 1s). Revisable at runtime
// via Ledger.SetAuditLogFlushInterval.
func WithAuditLogFlushInterval(d time.Duration) AuditLogOption {
	return func(c *auditLogConfig) {
		if d > 0 {
			c.flushInterval = d
		}
	}
}

// WithAuditLogSigning enables per-line HMAC-SHA256 signatures on the
// audit log (LG-65, see audit_sign.go): every sealed event the writer
// flushes carries {"kid","mac"}, the MAC covering the line's canonical
// bytes. keys is the rotation set — the first non-retired key signs new
// lines, retired keys still verify inside grace (see
// ParseAuditSigningKeys and VerifyAuditSignatures). A non-positive grace
// keeps DefaultAuditSigningGrace. An empty keys slice keeps the log
// unsigned. Revisable at runtime via Ledger.SetAuditSigningGrace.
func WithAuditLogSigning(keys []AuditSigningKey, grace time.Duration) AuditLogOption {
	return func(c *auditLogConfig) {
		c.signingKeys = keys
		c.signingGrace = grace
	}
}

// AuditLog appends AuditEvents as JSONL to a configured directory, one
// line per event, from a background goroutine. Ledger operations enqueue
// with Log (non-blocking); the writer owns all file state.
//
// Line signatures (LG-65): with WithAuditLogSigning the writer attaches
// a per-line HMAC-SHA256 (the sig field) covering the line's canonical
// bytes, keyed by the operator's signing keys with rotation support.
// Without keys the log runs unsigned and records an
// audit_signing_disabled alert in the trail itself, so a later offline
// verification can tell "never signed" from "signature stripped".
//
// Hash chain (LG-32): every event is sealed at enqueue time with the
// hex SHA-256 of the previously sealed event (PrevHash) and its own
// seal (Hash = SHA-256(canonical entry || PrevHash), standard library
// only). The chain is assigned under a mutex at the single ordered
// enqueue point, so the async writer's flush order can never break it;
// rotation (daily/size) and gzip archiving carry the chain across files
// unchanged, and a restart recovers the head from the newest sealed
// entry on disk. GET /audit/verify replays the whole chain and reports
// the first break.
//
// Rotation: a new file starts each UTC day, and whenever the current file
// reaches the size cap. Rotated files are gzipped in the background
// (audit-2006-01-02.jsonl.gz) so the directory stays compact; the writer
// never blocks on compression. Retention (LG-137): ApplyAuditRetention
// deletes whole files older than the LEDGER_AUDIT_RETENTION_DAYS window;
// inside the window files are append-only (WORM) and never modified.
//
// Failure semantics: a write error drops the event and counts it
// (Dropped) but never panics and never propagates to the ledger
// operation — the audit trail is best-effort by design, and a dead disk
// must not take down posting. Each event is a single Write syscall, so
// lines are never torn under normal operation. Note that a write-time
// drop leaves a gap the hash chain will surface: verification reports
// the first missing link, which is the honest tamper-evident signal for
// lost events.
type AuditLog struct {
	dir      string
	maxBytes int64
	// retentionDays is the retention window in whole days (LG-137):
	// ApplyAuditRetention deletes whole audit files older than the
	// window. 0 means unlimited retention.
	retentionDays int
	// cfgMu guards maxBytes, retentionDays, queueSize, flushInterval
	// and pending (LG-301, LG-309, LG-342):
	// maxBytes/retentionDays/queueSize/flushInterval are
	// runtime-revisable via Ledger.SetAuditLogMaxBytes /
	// SetAuditLogRetentionDays / SetAuditLogQueueSize /
	// SetAuditLogFlushInterval while the
	// background writer (run) and the retention sweep read them live.
	// It is deliberately a separate mutex from chainMu, which only
	// serializes hash-chain sealing at enqueue — config reads must
	// never contend with, or be ordered behind, chain sealing.
	cfgMu sync.RWMutex
	// retentionDeletions counts audit files deleted by retention
	// sweeps; synced into ledger_audit_retention_deletions_total on
	// every /metrics scrape.
	retentionDeletions atomic.Uint64
	// queue is the async event channel between ledger operations and
	// the background writer. Its capacity is queueSize (below): the
	// writer owns the swap — a revision installs a new channel and the
	// writer drains the old generation before reading the new one, so
	// a revision can neither strand buffered events nor reorder them
	// (LG-309).
	queue chan AuditEvent
	// queueSize is the live capacity of queue (LG-309): the
	// construction-time WithAuditLogQueueSize value, revisable at
	// runtime via Ledger.SetAuditLogQueueSize. Read it through
	// QueueSize, never directly: the field moves under cfgMu when the
	// writer installs a new generation.
	queueSize int
	// flushInterval is the live fsync cadence (LG-342): the
	// construction-time WithAuditLogFlushInterval value, revisable at
	// runtime via Ledger.SetAuditLogFlushInterval. Read it through
	// FlushInterval, never directly: the field moves under cfgMu when
	// a revision is applied.
	flushInterval time.Duration
	// pending holds events drained out of a previous queue generation
	// during a shrink (LG-309): when the capacity is revised down,
	// buffered events are moved here instead of being dropped, and
	// the writer flushes them — oldest first — before taking new
	// events from the channel. While pending is non-empty the total
	// depth (len(pending) + len(queue)) can exceed queueSize, and new
	// events are dropped by plain channel semantics whenever the
	// (smaller) new channel is full, until the writer works the
	// backlog down.
	pending []AuditEvent
	// resizeReq carries at most one pending queue-size revision to
	// the background writer (LG-309): the writer owns the swap, so a
	// revision is applied at a point where the writer holds no
	// received event and the old channel can be drained without
	// reordering or stranding anything. Buffered 1; resizeMu
	// serializes setters, so a send never blocks on a full channel.
	resizeReq chan queueResizeRequest
	// resizeMu serializes queue-size revisions and Close (LG-309): a
	// revision in flight must finish before Close can stop the
	// writer, and Close must finish before a revision can take the
	// closed path — otherwise a swap could race the writer's final
	// drain.
	resizeMu sync.Mutex
	// flushReq carries at most one pending flush-interval revision to
	// the background writer (LG-342): the writer owns the ticker, so
	// a revision is applied at a select point where the writer holds
	// no received event — it syncs once at the old cadence boundary,
	// then stops and rebuilds the ticker at the new interval.
	// Buffered 1; flushMu serializes setters, so a send never blocks
	// on a full channel.
	flushReq chan flushIntervalRequest
	// flushMu serializes flush-interval revisions and Close (LG-342):
	// a revision in flight must finish before Close can stop the
	// writer, and Close must finish before a revision can take the
	// closed path — otherwise a rebuild could race the writer's
	// final drain.
	flushMu sync.Mutex
	done     chan struct{}
	wg       sync.WaitGroup
	gzWg     sync.WaitGroup // in-flight background gzipFile calls
	closed   atomic.Bool
	written  atomic.Uint64
	dropped  atomic.Uint64
	// chainMu serializes hash-chain sealing: the single ordered point
	// where an event's PrevHash/Hash are assigned, before enqueue.
	// The mutex (not the async writer) owns the ordering, so flush
	// order can never break the chain. chainHead is the hex seal of
	// the last sealed-and-enqueued event; empty means the genesis
	// marker applies.
	chainMu   sync.Mutex
	chainHead string
	// signer seals each flushed line with HMAC-SHA256 (LG-65). Nil
	// means the log runs unsigned (degraded mode).
	signer *auditSigner
	// signMu guards signer, signingKeys and signingGrace (LG-305):
	// the background writer and Signed() snapshot the signer under
	// RLock, rotation swaps it under Lock. It is deliberately a
	// separate mutex from chainMu (hash-chain sealing) and cfgMu
	// (LG-301 maxBytes/retentionDays) — signing rotation is its own
	// concern, and the writer must never contend with, or be ordered
	// behind, chain sealing or config revision.
	signMu sync.RWMutex
	// signingKeys is the installed rotation set the live signer was
	// built from (LG-305): the first non-retired key signs new lines,
	// retired keys still verify inside grace. Key material lives only
	// in memory, never on disk. Empty means the log runs unsigned.
	signingKeys []AuditSigningKey
	// signingGrace is the retired-key verification window the live
	// signer was built with (LG-65, default
	// DefaultAuditSigningGrace); rotation rebuilds the signer with
	// this same grace.
	signingGrace time.Duration
}

// NewAuditLog creates the audit directory (if needed) and starts the
// background writer. The caller must Close the log on shutdown to flush
// queued events.
func NewAuditLog(dir string, opts ...AuditLogOption) (*AuditLog, error) {
	cfg := auditLogConfig{maxBytes: DefaultAuditMaxBytes, queueSize: DefaultAuditQueueSize, flushInterval: DefaultAuditFlushInterval}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.retentionDays < 0 {
		return nil, fmt.Errorf("ledger: audit log: negative retention days %d (want 0 for unlimited, or a positive number of days)", cfg.retentionDays)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("ledger: audit log: %w", err)
	}
	var signer *auditSigner
	grace := cfg.signingGrace
	if grace <= 0 {
		grace = DefaultAuditSigningGrace
	}
	if len(cfg.signingKeys) > 0 {
		s, err := newAuditSigner(cfg.signingKeys, grace)
		if err != nil {
			return nil, err
		}
		signer = s
	}
	a := &AuditLog{
		dir:           dir,
		maxBytes:      cfg.maxBytes,
		retentionDays: cfg.retentionDays,
		queue:         make(chan AuditEvent, cfg.queueSize),
		// LG-309: the live capacity starts at the construction-time
		// value and moves only when the writer installs a new queue
		// generation (setQueueSize / applyQueueResize).
		queueSize: cfg.queueSize,
		// LG-342: the live fsync cadence starts at the
		// construction-time value and moves only when the writer
		// rebuilds its ticker (setFlushInterval).
		flushInterval: cfg.flushInterval,
		resizeReq: make(chan queueResizeRequest, 1),
		flushReq:  make(chan flushIntervalRequest, 1),
		done:      make(chan struct{}),
		signer:    signer,
		// LG-305: the rotation set and grace live with the signer so
		// a runtime rotation can rebuild it; the stored copy is
		// deep-copied so the caller's slice can never alias the live
		// signer.
		signingKeys:  copyAuditSigningKeys(cfg.signingKeys),
		signingGrace: grace,
	}
	// Recover the hash-chain head from the newest sealed entry on
	// disk (bounded scan), so a restart continues the chain instead
	// of breaking it at the first new event.
	a.chainHead = recoverAuditChainHead(dir)
	a.wg.Add(1)
	go a.run()
	if signer == nil {
		// Degraded unsigned mode: record the fact in the trail itself.
		// An offline verification can then tell "never signed" (this
		// alert is present, lines carry no sig) from "signatures
		// stripped" (no alert, lines missing sig). The alert is a normal
		// chained event, so the hash chain stays contiguous.
		a.Log(AuditEvent{
			Op:      "audit_signing_disabled",
			Actor:   "NewAuditLog",
			TraceID: "audit-signing",
			Details: map[string]any{
				"reason": "no signing keys configured; audit lines are written without HMAC",
			},
		})
	}
	return a, nil
}

// Signed reports whether the log attaches per-line HMAC signatures
// (LG-65): false means degraded unsigned mode. The signer is read
// under signMu (LG-305): rotation swaps it live, so a lock-free read
// would race the writer's snapshot.
func (a *AuditLog) Signed() bool {
	a.signMu.RLock()
	defer a.signMu.RUnlock()
	return a.signer != nil
}

// Log enqueues an event for asynchronous writing. It never blocks: when
// the queue is full, or the log is closed, the event is dropped, counted,
// and Log returns false.
//
// The event is hash-chain sealed here, under chainMu, before enqueue:
// PrevHash is the previous sealed event's hash (or the genesis marker),
// and Hash seals this event's canonical serialization. Sealing at the
// single ordered enqueue point — not in the background writer — means
// async flush order can never reorder or break the chain. A dropped
// event does not advance the chain head, so the on-disk chain stays
// contiguous over exactly the events that were enqueued.
func (a *AuditLog) Log(ev AuditEvent) bool {
	if a.closed.Load() {
		a.dropped.Add(1)
		return false
	}
	a.chainMu.Lock()
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now().UTC()
	}
	ev.Timestamp = ev.Timestamp.UTC()
	if a.chainHead == "" {
		ev.PrevHash = AuditGenesisPrevHash
	} else {
		ev.PrevHash = a.chainHead
	}
	ev.Hash = sealAuditEvent(ev)
	// LG-309: the send holds cfgMu.RLock so a concurrent queue-size
	// revision (which swaps the channel under cfgMu.Lock) either waits
	// for this send to land in the old channel — where the writer's
	// drain picks it up — or this send reads the new channel after
	// the swap. Without the lock a send could land in an abandoned
	// generation and be lost. The lock is held only for the
	// non-blocking send attempt, so it can never stall a poster.
	a.cfgMu.RLock()
	select {
	case a.queue <- ev:
		a.chainHead = ev.Hash
		a.cfgMu.RUnlock()
		a.chainMu.Unlock()
		return true
	default:
		a.dropped.Add(1)
		a.cfgMu.RUnlock()
		a.chainMu.Unlock()
		return false
	}
}

// Stats reports how many events were written and how many were dropped.
// A growing dropped count means the disk or the writer cannot keep up —
// alert on it.
func (a *AuditLog) Stats() (written, dropped uint64) {
	return a.written.Load(), a.dropped.Load()
}

// Dir returns the audit directory.
func (a *AuditLog) Dir() string { return a.dir }

// queueResizeRequest is one pending queue-capacity revision for the
// background writer (LG-309). done is closed once the new channel is
// installed; drained events are already in pending by then and are
// flushed by the writer in order.
type queueResizeRequest struct {
	n    int
	done chan struct{}
}

// QueueSize reports the current async event-queue capacity, read live
// under cfgMu. It is the runtime counterpart of the construction-time
// WithAuditLogQueueSize value (default DefaultAuditQueueSize).
func (a *AuditLog) QueueSize() int {
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	return a.queueSize
}

// queueChan reads the current queue generation under cfgMu.RLock. The
// writer calls it once at startup; afterwards it tracks generations
// through applyQueueResize, never by re-reading mid-generation.
func (a *AuditLog) queueChan() chan AuditEvent {
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	return a.queue
}

// takePending pops the oldest event drained out of a previous queue
// generation during a shrink, if any. The writer flushes pending
// before taking new events from the channel, so shrink survivors keep
// their place in line.
func (a *AuditLog) takePending() (AuditEvent, bool) {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	if len(a.pending) == 0 {
		return AuditEvent{}, false
	}
	ev := a.pending[0]
	a.pending[0] = AuditEvent{} // avoid retaining the sealed entry
	a.pending = a.pending[1:]
	if len(a.pending) == 0 {
		a.pending = nil // release the backing array between revisions
	}
	return ev, true
}

// drainQueueLocked moves every event still buffered in ch into
// pending, preserving order. Callers must hold cfgMu.Lock: Log sends
// under cfgMu.RLock, so no event can enter the channel mid-drain, and
// the writer is the only other consumer — and it is the caller here
// (applyQueueResize) or has exited (the closed path of setQueueSize).
func (a *AuditLog) drainQueueLocked(ch chan AuditEvent) {
	for {
		select {
		case ev := <-ch:
			a.pending = append(a.pending, ev)
		default:
			return
		}
	}
}

// applyQueueResize installs a new queue channel of capacity req.n and
// reports it to the writer. It runs in the writer goroutine, at a
// select point where the writer holds no received event: the old
// generation is drained into pending under cfgMu (Log is excluded for
// the duration, so nothing can enter the old channel mid-drain and no
// drained event can be overtaken by a new one), then the new channel
// is installed and the setter is released. The drained events are
// flushed by the writer's takePending path, in order, before any event
// taken from the new channel — a shrink therefore loses nothing, and
// while the backlog drains, new events are dropped by plain channel
// semantics whenever the (smaller) new channel is full.
func (a *AuditLog) applyQueueResize(req queueResizeRequest, q chan AuditEvent) chan AuditEvent {
	a.cfgMu.Lock()
	a.drainQueueLocked(q)
	next := make(chan AuditEvent, req.n)
	a.queue = next
	a.queueSize = req.n
	a.cfgMu.Unlock()
	close(req.done)
	return next
}

// setQueueSize installs a new queue capacity and reports the previous
// value and whether it changed. Callers must validate n > 0 first; the
// swap itself is unconditional once past the same-value check, so a
// rejected value leaves the live capacity untouched. A closed log takes
// the inline path (Close holds resizeMu, so a closed log seen here
// means the writer has exited and the queue is drained; any residual
// buffered events move to pending the same way, though with no writer
// they are never flushed — Close drains the queue, so in practice
// there are none).
func (a *AuditLog) setQueueSize(n int) (previous int, changed bool) {
	a.resizeMu.Lock()
	defer a.resizeMu.Unlock()
	a.cfgMu.RLock()
	previous = a.queueSize
	closed := a.closed.Load()
	a.cfgMu.RUnlock()
	if previous == n {
		return previous, false
	}
	if closed {
		a.cfgMu.Lock()
		a.drainQueueLocked(a.queue)
		a.queue = make(chan AuditEvent, n)
		a.queueSize = n
		a.cfgMu.Unlock()
		return previous, true
	}
	req := queueResizeRequest{n: n, done: make(chan struct{})}
	a.resizeReq <- req
	<-req.done
	return previous, true
}

// AuditDir reports the audit-log directory when a log is attached
// (WithAuditLog was used). It lets the HTTP layer run hash-chain
// verification without reaching into the ledger's internals.
func (l *Ledger) AuditDir() (string, bool) {
	if l.audit == nil {
		return "", false
	}
	return l.audit.Dir(), true
}

// Close stops the writer after draining the queue, waits for in-flight
// background gzips to finish (so the file set is stable for readers
// afterwards), syncs the current file, and returns. It is idempotent.
func (a *AuditLog) Close() error {
	// LG-309: serialize with queue-size revisions — a revision in
	// flight must be applied (and its setter unblocked) before the
	// writer stops, and no revision may take the closed path while
	// the writer is still draining. The writer applies a pending
	// revision from its main loop, which is still running here, so
	// waiting on resizeMu cannot deadlock.
	a.resizeMu.Lock()
	defer a.resizeMu.Unlock()
	// LG-342: serialize with flush-interval revisions the same way —
	// a rebuild in flight must finish before the writer stops, and
	// no revision may take the closed path while the writer is still
	// draining.
	a.flushMu.Lock()
	defer a.flushMu.Unlock()
	if a.closed.Swap(true) {
		return nil
	}
	close(a.done)
	a.wg.Wait()
	a.gzWg.Wait()
	return nil
}

// run is the background writer: the only goroutine that touches the
// audit files.
func (a *AuditLog) run() {
	defer a.wg.Done()

	var (
		f       *os.File
		curDay  string
		curSize int64
		pending int // events since last sync
		seqs    = make(map[string]int)
	)

	syncFile := func() {
		if f != nil {
			_ = f.Sync() // best-effort; failures surface as dropped writes
		}
		pending = 0
	}

	// rotate closes the current file (gzipping it in the background) and
	// opens the file for day, picking the next sequence number when the
	// day's base file already hit the size cap (e.g. after a restart).
	rotate := func(day string) {
		if f != nil {
			name := f.Name()
			_ = f.Close()
			f = nil
			a.gzWg.Add(1)
			go func() {
				defer a.gzWg.Done()
				gzipFile(name)
			}()
		}
		// Filenames are never reused, within a process or across
		// restarts: the next sequence is one past the highest on disk
		// for the day. A reused name would make ReadAuditLog's
		// .jsonl/.gz dedupe ambiguous (same base name, different
		// content). A restart therefore starts a fresh sequence file
		// rather than appending to the previous run's tail.
		next, ok := seqs[day]
		if !ok {
			next = scanMaxSeq(a.dir, day) + 1
		}
		seqs[day] = next + 1
		path := filepath.Join(a.dir, auditLogFilename(day, next))
		fh, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			// The directory was validated at construction; a failure
			// here is unexpected. Without a file the writer cannot
			// proceed — drop everything until the next rotation attempt
			// rather than spinning.
			f = nil
			return
		}
		f = fh
		if st, err := fh.Stat(); err == nil {
			curSize = st.Size()
		} else {
			curSize = 0
		}
		curDay = day
		pending = 0
	}

	write := func(ev AuditEvent) {
		day := ev.Timestamp.UTC().Format("2006-01-02")
		// The cap is read live under cfgMu (LG-301): a revision takes
		// effect on the next write, with no forced immediate rotation
		// of a file that already exceeds the new cap.
		if f == nil || day != curDay || curSize >= a.MaxBytes() {
			rotate(day)
		}
		if f == nil {
			a.dropped.Add(1)
			return
		}
		line, err := json.Marshal(ev)
		if err != nil {
			a.dropped.Add(1)
			return
		}
		// LG-305: snapshot the live signer under signMu.RLock and
		// sign with the snapshot. A rotation installs a new signer
		// atomically under signMu.Lock; a line already being flushed
		// keeps the signer it started with, so rotation only affects
		// rows flushed afterwards (future-only).
		if signer := a.snapshotSigner(); signer != nil {
			// LG-65: seal the canonical line bytes (sig field absent)
			// under the primary signing key, then re-marshal with the
			// signature attached. The verifier strips sig and
			// re-marshals to the identical bytes.
			sig := signer.sign(line)
			ev.Sig = &sig
			if line, err = json.Marshal(ev); err != nil {
				a.dropped.Add(1)
				return
			}
		}
		line = append(line, '\n')
		if n, err := f.Write(line); err != nil {
			a.dropped.Add(1)
		} else {
			curSize += int64(n)
			a.written.Add(1)
			pending++
			if pending >= 128 {
				syncFile()
			}
		}
	}

	ticker := time.NewTicker(a.FlushInterval())
	// The ticker is rebuilt by flush-interval revisions (LG-342):
	// the deferred stop must read the current variable, not the
	// ticker value captured at this line.
	defer func() { ticker.Stop() }()
	// q is the queue generation the writer is draining (LG-309): the
	// channel is re-read only when the writer itself installs a new
	// one (applyQueueResize), never mid-generation, so a revision can
	// neither strand buffered events nor reorder them.
	q := a.queueChan()
	for {
		// Events drained out of a previous generation during a
		// shrink are older than anything in the channel: flush them
		// first, in order, before taking new events.
		if ev, ok := a.takePending(); ok {
			write(ev)
			continue
		}
		select {
		case ev := <-q:
			write(ev)
		case req := <-a.resizeReq:
			q = a.applyQueueResize(req, q)
		case req := <-a.flushReq:
			// LG-342: sync once at the old cadence boundary, then
			// rebuild the ticker at the new interval (retune-drain,
			// along LG-328): a revision never swallows a due sync,
			// and the new cadence governs from the next tick. The
			// live field moves here, under cfgMu, so FlushInterval
			// reports the new cadence the moment the ticker does.
			syncFile()
			ticker.Stop()
			ticker = time.NewTicker(req.d)
			a.cfgMu.Lock()
			a.flushInterval = req.d
			a.cfgMu.Unlock()
			close(req.done)
		case <-ticker.C:
			syncFile()
		case <-a.done:
			for {
				if ev, ok := a.takePending(); ok {
					write(ev)
					continue
				}
				select {
				case ev := <-q:
					write(ev)
				default:
					syncFile()
					if f != nil {
						_ = f.Close()
					}
					return
				}
			}
		}
	}
}

// scanMaxSeq returns the highest sequence number among this UTC day's
// audit files (plain and gzipped), or -1 when none exist. The writer
// starts one past it so filenames are never reused.
func scanMaxSeq(dir, day string) int {
	matches, _ := filepath.Glob(filepath.Join(dir, "audit-"+day+"*.jsonl*"))
	max := -1
	prefix := "audit-" + day
	for _, m := range matches {
		rest := strings.TrimPrefix(filepath.Base(m), prefix)
		rest = strings.TrimSuffix(rest, ".gz")
		rest = strings.TrimSuffix(rest, ".jsonl")
		var seq int
		switch {
		case rest == "":
			seq = 0
		case strings.HasPrefix(rest, "-"):
			n, err := strconv.Atoi(rest[1:])
			if err != nil {
				continue
			}
			seq = n
		default:
			continue
		}
		if seq > max {
			max = seq
		}
	}
	return max
}

// gzipFile compresses path to path+".gz" and removes the original. It
// runs in its own goroutine so rotation never blocks the writer. The
// compressed output is written to a temp file and renamed into place, so
// a .gz file that exists is always complete — readers never observe a
// half-written archive. Failures are silent: the uncompressed file
// simply stays.
func gzipFile(path string) {
	in, err := os.Open(path)
	if err != nil {
		return
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(path), ".audit-gzip-*")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	// Best-effort cleanup of the temp file on any failure path; the
	// deferred Remove is a no-op after a successful rename.
	defer os.Remove(tmpName)
	w := gzip.NewWriter(tmp)
	_, copyErr := io.Copy(w, in)
	closeErr := w.Close()
	syncErr := tmp.Sync()
	fileErr := tmp.Close()
	if copyErr != nil || closeErr != nil || syncErr != nil || fileErr != nil {
		return
	}
	if err := os.Rename(tmpName, path+".gz"); err != nil {
		return
	}
	_ = os.Remove(path)
}

// ReadAuditLog reads every audit file for the given UTC day ("2006-01-02")
// — plain .jsonl and gzipped .jsonl.gz, including size-rotation sequence
// files — in filename order, and returns the parsed events plus the
// number of corrupt lines skipped.
//
// A damaged audit file never fails the read: malformed lines are counted
// and skipped so operators still get every intact record. The audit log
// is append-only and each event is one line, so a torn write can only
// ever corrupt its own line.
func ReadAuditLog(dir, day string) (events []AuditEvent, corrupt int, err error) {
	if _, err := time.Parse("2006-01-02", day); err != nil {
		return nil, 0, fmt.Errorf("ledger: bad audit day %q: %w", day, err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "audit-"+day+"*.jsonl*"))
	if err != nil {
		return nil, 0, fmt.Errorf("ledger: audit glob: %w", err)
	}
	var files []string
	for _, m := range matches {
		if strings.HasSuffix(m, ".jsonl") || strings.HasSuffix(m, ".jsonl.gz") {
			files = append(files, m)
		}
	}
	sort.Strings(files)
	// Dedupe by logical file: a rotation caught mid-flight can leave
	// both audit-<day>.jsonl and audit-<day>.jsonl.gz on disk, and they
	// carry identical events — read it once. Sort order puts the plain
	// file first, so it wins.
	seen := make(map[string]bool)
	deduped := files[:0]
	for _, f := range files {
		base := strings.TrimSuffix(f, ".gz")
		if seen[base] {
			continue
		}
		seen[base] = true
		deduped = append(deduped, f)
	}
	for _, path := range deduped {
		n, c, err := readAuditFile(path)
		if err != nil {
			return nil, 0, err
		}
		events = append(events, n...)
		corrupt += c
	}
	return events, corrupt, nil
}

// readAuditFile parses one audit file, skipping blank and malformed
// lines. Corrupt lines are counted, not fatal. If the file vanished
// between the directory listing and the open (a rotation completed
// mid-read), the gzip twin — the same events — is read instead; the
// rename-then-remove order in gzipFile guarantees at least one copy is
// always visible.
func readAuditFile(path string) ([]AuditEvent, int, error) {
	fh, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			twin := path + ".gz"
			if strings.HasSuffix(path, ".gz") {
				twin = strings.TrimSuffix(path, ".gz")
			}
			if fh2, err2 := os.Open(twin); err2 == nil {
				fh2.Close()
				return readAuditFilePlain(twin)
			}
			// Neither copy visible: rotation is mid-flight. The events
			// are still on disk; a retry will see them.
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("ledger: audit read %s: %w", path, err)
	}
	defer fh.Close()
	return scanAuditReader(fh, path)
}

// readAuditFilePlain parses one audit file known to exist, without the
// rotation fallback (used for the twin retry above).
func readAuditFilePlain(path string) ([]AuditEvent, int, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, 0, fmt.Errorf("ledger: audit read %s: %w", path, err)
	}
	defer fh.Close()
	return scanAuditReader(fh, path)
}

// scanAuditReader parses audit events from r, skipping blank and
// malformed lines. Corrupt lines are counted, not fatal.
func scanAuditReader(r io.Reader, path string) ([]AuditEvent, int, error) {
	var rd io.Reader = r
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(r)
		if err != nil {
			return nil, 0, fmt.Errorf("ledger: audit gunzip %s: %w", path, err)
		}
		defer gz.Close()
		rd = gz
	}

	var (
		events  []AuditEvent
		corrupt int
	)
	sc := bufio.NewScanner(rd)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var ev AuditEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			corrupt++
			continue
		}
		events = append(events, ev)
	}
	if err := sc.Err(); err != nil {
		return nil, 0, fmt.Errorf("ledger: audit scan %s: %w", path, err)
	}
	return events, corrupt, nil
}
