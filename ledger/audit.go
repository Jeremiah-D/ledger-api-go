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

// AuditLog appends AuditEvents as JSONL to a configured directory, one
// line per event, from a background goroutine. Ledger operations enqueue
// with Log (non-blocking); the writer owns all file state.
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
// never blocks on compression.
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
	queue    chan AuditEvent
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
}

// NewAuditLog creates the audit directory (if needed) and starts the
// background writer. The caller must Close the log on shutdown to flush
// queued events.
func NewAuditLog(dir string, opts ...AuditLogOption) (*AuditLog, error) {
	cfg := auditLogConfig{maxBytes: DefaultAuditMaxBytes, queueSize: DefaultAuditQueueSize}
	for _, opt := range opts {
		opt(&cfg)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("ledger: audit log: %w", err)
	}
	a := &AuditLog{
		dir:      dir,
		maxBytes: cfg.maxBytes,
		queue:    make(chan AuditEvent, cfg.queueSize),
		done:     make(chan struct{}),
	}
	// Recover the hash-chain head from the newest sealed entry on
	// disk (bounded scan), so a restart continues the chain instead
	// of breaking it at the first new event.
	a.chainHead = recoverAuditChainHead(dir)
	a.wg.Add(1)
	go a.run()
	return a, nil
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
	select {
	case a.queue <- ev:
		a.chainHead = ev.Hash
		a.chainMu.Unlock()
		return true
	default:
		a.dropped.Add(1)
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
		if f == nil || day != curDay || curSize >= a.maxBytes {
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

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case ev := <-a.queue:
			write(ev)
		case <-ticker.C:
			syncFile()
		case <-a.done:
			for {
				select {
				case ev := <-a.queue:
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
