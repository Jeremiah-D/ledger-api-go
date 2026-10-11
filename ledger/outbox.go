package ledger

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Domain event outbox (LG-53): an append-only JSONL stream of the
// ledger's domain state changes, for downstream consumers (indexers,
// notification fan-out, data-warehouse loaders).
//
// It is deliberately decoupled from the compliance audit log (LG-29,
// see audit.go): the audit log answers "who did what, when" — every
// operation, including reads of reports, config changes, and dry runs
// that commit nothing — while the outbox answers "what domain facts
// changed": journal entries committed, transfers settled, holds
// created/captured/released/expired, accounts frozen/unfrozen, reviews
// decided. A consumer rebuilding balances needs the outbox; a
// compliance officer needs the audit log. Neither replaces the other.
//
// Event envelope:
//
//	{seq, type, at, account?, entry_id?, amount_cents?, currency?, details?}
//
//   - seq is a ledger-process-global monotonic sequence number, assigned
//     at emit time inside the commit critical section, so seq order is
//     commit order. It is the pagination cursor for GET /events.
//   - type names the domain fact: entry_posted, transfer_posted,
//     hold_created, hold_captured, hold_released, hold_expired,
//     account_frozen, account_unfrozen, review_approved, review_rejected,
//     review_expired, entry_reversed.
//   - account is the primary account of the event (debit account for
//     entry_posted, payer for transfer_posted, hold account for hold
//     events). The ?account= query filter also matches either leg of an
//     entry_posted event via its details.
//   - details carries event-specific facts (the second leg, memos,
//     batch IDs, hold IDs, reviewer names, FX provenance) as plain JSON
//     values. The envelope stays stable; details may grow.
//
// Crash semantics (at-least-once, documented honestly):
//
//   - Emit is synchronous and happens inside the commit critical section
//     (the caller holds the ledger's write lock), before the operation
//     returns success. Each event is written and fsync'd before Emit
//     returns, so any operation the caller observed as successful has
//     its events durably on disk: a crash can never lose a committed
//     event.
//   - Duplicates are possible and must be tolerated: a crash between the
//     commit and the caller's receipt of the response makes the caller
//     retry; the idempotency-key replay suppresses a second event for
//     the same key within its TTL, but a retry with a different key, an
//     expired TTL, or a keyless repost commits anew and emits anew.
//     Consumers must treat the stream as at-least-once and dedupe on
//     (type, entry_id) or their own idempotency handling.
//   - A disk failure at emit time is counted (dropped) but never fails
//     the posting: availability of the ledger outranks completeness of
//     the stream. A rising dropped counter means the stream has gaps —
//     alert on ledger_outbox_dropped_total. Seq gaps on read are the
//     visible symptom.
//   - Restart recovery: NewOutbox scans the directory for the highest
//     seq on disk and continues from there, so seq never rewinds.
//   - Unlike the audit log (LG-32), the outbox carries no hash chain:
//     it is a consumer stream, not tamper evidence. Tamper detection
//     stays with the audit chain and the journal chain.
//
// The outbox is opt-in (WithOutbox): a nil outbox disables emission
// entirely — operations skip event construction and the read path is
// untouched, so the hot path pays nothing when the feature is off. The
// fsync-per-event cost applies only when the outbox is attached; the
// README benchmark numbers were measured without it.

// Outbox event types: the domain facts the outbox streams.
const (
	OutboxEntryPosted     = "entry_posted"
	OutboxTransferPosted  = "transfer_posted"
	OutboxHoldCreated     = "hold_created"
	OutboxHoldCaptured    = "hold_captured"
	OutboxHoldReleased    = "hold_released"
	OutboxHoldExpired     = "hold_expired"
	OutboxAccountFrozen   = "account_frozen"
	OutboxAccountUnfrozen = "account_unfrozen"
	OutboxReviewApproved  = "review_approved"
	OutboxReviewRejected  = "review_rejected"
	OutboxReviewExpired   = "review_expired"
	OutboxEntryReversed   = "entry_reversed"
)

// OutboxEvent is one domain fact in the outbox stream.
type OutboxEvent struct {
	Seq         uint64         `json:"seq"`
	Type        string         `json:"type"`
	At          time.Time      `json:"at"`
	Account     AccountID      `json:"account,omitempty"`
	EntryID     string         `json:"entry_id,omitempty"`
	AmountCents int64          `json:"amount_cents,omitempty"`
	Currency    string         `json:"currency,omitempty"`
	Details     map[string]any `json:"details,omitempty"`
}

// outboxFilename lays out outbox files as outbox-2006-01-02.jsonl, one
// file per UTC day. Days are UTC so file boundaries are deterministic
// regardless of the process timezone. There is no size rotation: the
// stream is read back in day order and seq is global, so a mid-day split
// would only complicate the reader for no consumer benefit.
func outboxFilename(day string) string {
	return fmt.Sprintf("outbox-%s.jsonl", day)
}

// Outbox appends OutboxEvents as JSONL to a configured directory, one
// line per event, synchronously: Emit writes and fsyncs before it
// returns, which is what makes the stream crash-safe (see the package
// doc). Callers must invoke Emit while holding the ledger's write lock
// — the ledger's emitOutboxLocked helper enforces this by convention —
// so emission order is commit order and seq assignment is the single
// ordered point.
//
// The Outbox owns its own mutex as a leaf lock: it never calls back
// into the ledger, so holding it under the ledger's write lock cannot
// deadlock.
type Outbox struct {
	dir     string
	mu      sync.Mutex
	seq     uint64 // last assigned seq; 0 when the directory is empty
	f       *os.File
	curDay  string
	written atomic.Uint64
	dropped atomic.Uint64
	closed  atomic.Bool
}

// NewOutbox creates the outbox directory (if needed), recovers the
// highest seq already on disk (so a restart continues the sequence
// instead of rewinding it), and opens today's file for append. The
// caller must Close the outbox on shutdown.
func NewOutbox(dir string) (*Outbox, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("ledger: outbox: %w", err)
	}
	o := &Outbox{dir: dir}
	maxSeq, corrupt, err := scanOutboxMaxSeq(dir)
	if err != nil {
		return nil, err
	}
	if corrupt > 0 {
		// Corrupt lines on recovery are skipped, not fatal: they were
		// already skipped by every reader, and refusing to start would
		// turn one bad line into a full outage. The count is logged so
		// the operator can investigate.
		fmt.Fprintf(os.Stderr, "ledger: outbox: skipped %d corrupt lines during seq recovery in %s\n", corrupt, dir)
	}
	o.seq = maxSeq
	if err := o.openDay(time.Now().UTC().Format("2006-01-02")); err != nil {
		return nil, err
	}
	return o, nil
}

// openDay closes the current file (if any) and opens the append handle
// for day. Callers must hold o.mu.
func (o *Outbox) openDay(day string) error {
	if o.f != nil {
		_ = o.f.Close()
		o.f = nil
	}
	path := filepath.Join(o.dir, outboxFilename(day))
	fh, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("ledger: outbox open %s: %w", path, err)
	}
	o.f = fh
	o.curDay = day
	return nil
}

// Emit appends one event to the outbox, synchronously: the seq is
// assigned, the line is written, and the file is fsync'd before Emit
// returns. Callers must hold the ledger's write lock (see
// emitOutboxLocked): emission inside the commit critical section is what
// guarantees a committed operation's events are durable before the
// operation reports success.
//
// Emit never blocks indefinitely and never panics: when the outbox is
// closed, or the write/fsync fails, the event is dropped, counted, and
// Emit returns false. The posting still commits — availability of the
// ledger outranks completeness of the stream (see the package doc).
func (o *Outbox) Emit(ev OutboxEvent) bool {
	if o.closed.Load() {
		o.dropped.Add(1)
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed.Load() {
		o.dropped.Add(1)
		return false
	}
	o.seq++
	ev.Seq = o.seq
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	ev.At = ev.At.UTC()
	if day := ev.At.Format("2006-01-02"); day != o.curDay {
		if err := o.openDay(day); err != nil {
			o.dropped.Add(1)
			return false
		}
	}
	line, err := json.Marshal(ev)
	if err != nil {
		o.dropped.Add(1)
		return false
	}
	line = append(line, '\n')
	if _, err := o.f.Write(line); err != nil {
		o.dropped.Add(1)
		return false
	}
	// The fsync is the crash-safety contract: without it, a crash could
	// lose the tail of committed events and the "at-least-once" claim
	// would be a lie. The cost is paid only when the outbox is
	// attached; the hot path without an outbox is untouched.
	if err := o.f.Sync(); err != nil {
		o.dropped.Add(1)
		return false
	}
	o.written.Add(1)
	return true
}

// Stats reports how many events were durably written and how many were
// dropped. A growing dropped count means the disk cannot keep up or is
// failing — alert on it, because the stream then has gaps.
func (o *Outbox) Stats() (written, dropped uint64) {
	return o.written.Load(), o.dropped.Load()
}

// Dir returns the outbox directory.
func (o *Outbox) Dir() string { return o.dir }

// Close flushes and closes the current file. It is idempotent.
func (o *Outbox) Close() error {
	if o.closed.Swap(true) {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.f != nil {
		err := o.f.Close()
		o.f = nil
		return err
	}
	return nil
}

// scanOutboxMaxSeq returns the highest event seq across all outbox files
// in dir, plus the number of corrupt lines skipped. A damaged file never
// fails the scan: malformed lines are counted and skipped.
func scanOutboxMaxSeq(dir string) (maxSeq uint64, corrupt int, err error) {
	matches, err := filepath.Glob(filepath.Join(dir, "outbox-*.jsonl"))
	if err != nil {
		return 0, 0, fmt.Errorf("ledger: outbox glob: %w", err)
	}
	sort.Strings(matches)
	for _, path := range matches {
		fh, err := os.Open(path)
		if err != nil {
			return 0, 0, fmt.Errorf("ledger: outbox scan %s: %w", path, err)
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Bytes()
			if len(strings.TrimSpace(string(line))) == 0 {
				continue
			}
			var ev OutboxEvent
			if err := json.Unmarshal(line, &ev); err != nil || ev.Seq == 0 || ev.Type == "" {
				corrupt++
				continue
			}
			if ev.Seq > maxSeq {
				maxSeq = ev.Seq
			}
		}
		scanErr := sc.Err()
		fh.Close()
		if scanErr != nil {
			return 0, 0, fmt.Errorf("ledger: outbox scan %s: %w", path, scanErr)
		}
	}
	return maxSeq, corrupt, nil
}

// OutboxQuery filters a ReadOutboxEvents scan. Zero values mean
// "unbounded": Since/Until bound the event time, Types exact-matches the
// event type (empty = all types), Account matches the event's account or
// either leg of an entry_posted event, AfterSeq resumes after a sequence
// number (the cursor contract), and Limit caps the page at 1..1000.
type OutboxQuery struct {
	Since     time.Time
	Until     time.Time
	Types     []string
	Account   AccountID
	AfterSeq  uint64
	Limit     int
}

// encodeOutboxCursor renders a seq as the opaque page cursor: base64url
// of the decimal seq, mirroring the journal's cursor convention (see
// decodeListCursor).
func encodeOutboxCursor(seq uint64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatUint(seq, 10)))
}

// DecodeOutboxCursor parses a GET /events page cursor back to a seq. An
// empty cursor resumes from the beginning (seq 0).
func DecodeOutboxCursor(cursor string) (uint64, error) {
	if cursor == "" {
		return 0, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(decoded) == 0 {
		return 0, ErrInvalidCursor
	}
	seq, err := strconv.ParseUint(string(decoded), 10, 64)
	if err != nil {
		return 0, ErrInvalidCursor
	}
	return seq, nil
}

// ReadOutboxEvents reads every outbox file in dir in day order (which is
// seq order: seq is assigned monotonically across days), applies the
// query filters, and returns one page plus the cursor for the next page
// (empty when this is the last page). A damaged file never fails the
// read: malformed lines are counted in corrupt and skipped, so operators
// still get every intact event.
func ReadOutboxEvents(dir string, q OutboxQuery) (events []OutboxEvent, nextCursor string, corrupt int, err error) {
	if q.Limit < 1 || q.Limit > maxPageSize {
		return nil, "", 0, ErrInvalidLimit
	}
	matches, err := filepath.Glob(filepath.Join(dir, "outbox-*.jsonl"))
	if err != nil {
		return nil, "", 0, fmt.Errorf("ledger: outbox glob: %w", err)
	}
	sort.Strings(matches)

	typeSet := make(map[string]bool, len(q.Types))
	for _, t := range q.Types {
		typeSet[t] = true
	}
	matchesAccount := func(ev OutboxEvent) bool {
		if q.Account == "" {
			return true
		}
		if ev.Account == q.Account {
			return true
		}
		// Either leg of an entry_posted event counts: a consumer asking
		// "everything touching account X" wants both sides of the money
		// movement, not just the debit side the event is filed under.
		if s, ok := ev.Details["debit_account"].(string); ok && AccountID(s) == q.Account {
			return true
		}
		if s, ok := ev.Details["credit_account"].(string); ok && AccountID(s) == q.Account {
			return true
		}
		return false
	}

	var all []OutboxEvent
	for _, path := range matches {
		fh, err := os.Open(path)
		if err != nil {
			return nil, "", 0, fmt.Errorf("ledger: outbox read %s: %w", path, err)
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Bytes()
			if len(strings.TrimSpace(string(line))) == 0 {
				continue
			}
			var ev OutboxEvent
			if err := json.Unmarshal(line, &ev); err != nil || ev.Seq == 0 || ev.Type == "" {
				corrupt++
				continue
			}
			if ev.Seq <= q.AfterSeq {
				continue
			}
			if !q.Since.IsZero() && ev.At.Before(q.Since) {
				continue
			}
			if !q.Until.IsZero() && !ev.At.Before(q.Until) {
				continue
			}
			if len(typeSet) > 0 && !typeSet[ev.Type] {
				continue
			}
			if !matchesAccount(ev) {
				continue
			}
			all = append(all, ev)
		}
		scanErr := sc.Err()
		fh.Close()
		if scanErr != nil {
			return nil, "", 0, fmt.Errorf("ledger: outbox scan %s: %w", path, scanErr)
		}
	}
	// Day order is seq order by construction, but sort defensively: a
	// hand-placed file must not scramble pagination.
	sort.Slice(all, func(i, j int) bool { return all[i].Seq < all[j].Seq })

	if len(all) > q.Limit {
		nextCursor = encodeOutboxCursor(all[q.Limit-1].Seq)
		all = all[:q.Limit]
	}
	if events == nil {
		events = []OutboxEvent{}
	}
	events = all
	return events, nextCursor, corrupt, nil
}

// WithOutbox attaches a domain-event outbox (see the package doc): every
// committed state change emits its domain events synchronously into the
// outbox. A nil outbox disables emission entirely.
func WithOutbox(o *Outbox) Option {
	return func(l *Ledger) {
		l.outbox = o
	}
}

// emitOutboxLocked appends one domain event when the outbox is enabled;
// it is a no-op when disabled. Callers must hold l.mu with the write
// lock: emission inside the commit critical section is the crash-safety
// contract (the event is fsync'd before the operation returns success).
// The timestamp is taken here so the recorded time matches the commit.
func (l *Ledger) emitOutboxLocked(ev OutboxEvent) {
	if l.outbox == nil {
		return
	}
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	l.outbox.Emit(ev)
}

// emitOutboxEntriesLocked emits one entry_posted event per committed
// journal entry, in commit order. Callers must hold l.mu with the write
// lock. Only freshly committed entries belong here: idempotent replays
// book nothing new, and their event was emitted by the original commit.
func (l *Ledger) emitOutboxEntriesLocked(entries []JournalEntry) {
	if l.outbox == nil {
		return
	}
	for _, e := range entries {
		details := map[string]any{
			"debit_account":  string(e.DebitAccount),
			"credit_account": string(e.CreditAccount),
		}
		if e.Memo != "" {
			details["memo"] = e.Memo
		}
		if e.BatchID != "" {
			details["batch_id"] = e.BatchID
		}
		l.emitOutboxLocked(OutboxEvent{
			Type:        OutboxEntryPosted,
			Account:     e.DebitAccount,
			EntryID:     e.ID,
			AmountCents: e.AmountCents,
			Currency:    e.Currency,
			Details:     details,
		})
	}
}

// emitOutboxTransferLocked emits the entry_posted events for a settled
// transfer's legs (principal, fee, FX legs) followed by the
// transfer_posted envelope, in commit order. Callers must hold l.mu with
// the write lock. extra carries path-specific details (FX provenance);
// it may be nil. Only the settle path calls this: transfers routed into
// dual-control review emit nothing here — their legs become effective at
// ApproveTransferReview, which emits entry_posted plus review_approved.
func (l *Ledger) emitOutboxTransferLocked(t Transfer, feeCents int64, entries []JournalEntry, extra map[string]any) {
	if l.outbox == nil || len(entries) == 0 {
		return
	}
	l.emitOutboxEntriesLocked(entries)
	details := map[string]any{
		"transfer_id": t.ID,
		"to":          string(t.To),
		"fee_cents":   feeCents,
	}
	if t.Memo != "" {
		details["memo"] = t.Memo
	}
	for k, v := range extra {
		details[k] = v
	}
	l.emitOutboxLocked(OutboxEvent{
		Type:        OutboxTransferPosted,
		Account:     t.From,
		EntryID:     entries[0].ID,
		AmountCents: t.AmountCents,
		Currency:    t.Currency,
		Details:     details,
	})
}

// OutboxStats reports the outbox's written/dropped event counters.
// Enabled is false when no outbox is attached (WithOutbox was never
// used): in that case the counters are zero and meaningless.
func (l *Ledger) OutboxStats() (written, dropped uint64, enabled bool) {
	if l.outbox == nil {
		return 0, 0, false
	}
	w, d := l.outbox.Stats()
	return w, d, true
}

// OutboxDir reports the outbox directory when an outbox is attached
// (WithOutbox was used). It lets the HTTP layer serve GET /events
// without reaching into the ledger's internals.
func (l *Ledger) OutboxDir() (string, bool) {
	if l.outbox == nil {
		return "", false
	}
	return l.outbox.Dir(), true
}
