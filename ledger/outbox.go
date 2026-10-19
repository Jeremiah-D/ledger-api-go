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
	OutboxAccountClosed   = "account_closed"
	// Account suspension lifecycle (see suspend.go): the operational
	// pause is reversible, unlike the terminal close above.
	OutboxAccountSuspended = "account_suspended"
	OutboxAccountResumed   = "account_resumed"
	OutboxReviewApproved   = "review_approved"
	OutboxReviewRejected   = "review_rejected"
	OutboxReviewExpired    = "review_expired"
	OutboxEntryReversed    = "entry_reversed"
	// Transfer dispute lifecycle (see dispute.go, LG-129): raising
	// freezes the disputed amount on the payee's available balance via
	// an authorization hold; resolving upholds (the payer is refunded)
	// or rejects (the payee keeps the funds) and releases the hold.
	OutboxDisputeRaised   = "dispute_raised"
	OutboxDisputeResolved = "dispute_resolved"
	// Direct-debit mandate lifecycle (see mandate.go, LG-125): the
	// mandate is structural config, but its lifecycle moves are domain
	// facts downstream consumers track — creation, suspension,
	// cancellation, expiry, and every authorized debit (the debit's
	// journal row emits the usual entry_posted event too).
	OutboxMandateCreated   = "mandate_created"
	OutboxMandateSuspended = "mandate_suspended"
	OutboxMandateCancelled = "mandate_cancelled"
	OutboxMandateExpired   = "mandate_expired"
	OutboxMandateDebited   = "mandate_debited"
	// OutboxMandateRetryExhausted fires when a mandate's automatic
	// retry runs out of attempts (see mandate_retry.go, LG-127): the
	// autopay missed a cycle. Downstream consumers can wire it into
	// dunning escalation (see EscalateDunning) to chase the payer.
	OutboxMandateRetryExhausted = "mandate_retry_exhausted"
	// Term-deposit lifecycle (see termdeposit.go): opening locks the
	// principal off-journal, maturing releases it at tenor end, and an
	// early withdrawal releases it with a penalty. The interest accrual
	// rows emit the usual entry_posted events.
	OutboxTermDepositOpened    = "term_deposit_opened"
	OutboxTermDepositMatured   = "term_deposit_matured"
	OutboxTermDepositWithdrawn = "term_deposit_withdrawn"
	// pain.001 instruction lifecycle (see pain001_status.go, LG-102):
	// every instruction-level or per-transfer status move emits one
	// event, so downstream consumers can subscribe to bank-side
	// settlement progress without polling.
	OutboxPain001StatusChanged = "pain001_status_changed"
)

// OutboxEvent is one domain fact in the outbox stream.
type OutboxEvent struct {
	Seq  uint64    `json:"seq"`
	Type string    `json:"type"`
	At   time.Time `json:"at"`
	// Tenant names the tenant namespace the event belongs to (see
	// tenant.go). Empty for the default tenant — events from a
	// single-tenant deployment look exactly as before.
	Tenant      string         `json:"tenant,omitempty"`
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
	// Live-tail subscribers (see Subscribe, LG-133): guarded by mu,
	// the same leaf lock Emit holds, so fan-out never deadlocks.
	subs      map[uint64]chan OutboxEvent
	nextSub   uint64
	slowDrops atomic.Uint64
}

// NewOutbox creates the outbox directory (if needed), recovers the
// highest seq already on disk (so a restart continues the sequence
// instead of rewinding it), and opens today's file for append. The
// caller must Close the outbox on shutdown.
func NewOutbox(dir string) (*Outbox, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("ledger: outbox: %w", err)
	}
	o := &Outbox{dir: dir, subs: make(map[uint64]chan OutboxEvent)}
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
	// Fan out to live-tail subscribers (see Subscribe): delivery is
	// non-blocking — a subscriber whose queue is full is dropped
	// rather than stalling the commit.
	o.deliverLocked(ev)
	return true
}

// outboxStreamBuffer is the per-subscriber live-event queue depth. A
// subscriber that falls behind past this is dropped (see deliverLocked):
// the Go equivalent of a writableLength guard — the commit path never
// blocks on a slow consumer and memory stays bounded.
const outboxStreamBuffer = 512

// Subscribe registers a live tail on the outbox (LG-133, see
// GET /events/stream): every event Emit commits from here on is also
// delivered on the returned channel, in seq order. The channel is
// buffered to outboxStreamBuffer events; when a subscriber falls behind
// past that, Emit drops the subscription and closes the channel — the
// consumer must reconnect with its last cursor (Last-Event-ID) to catch
// up from disk. Call the returned function to unsubscribe; it is
// idempotent. ok is false when the outbox is closed: there is no live
// tail to subscribe to.
func (o *Outbox) Subscribe() (ch <-chan OutboxEvent, unsubscribe func(), ok bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed.Load() {
		return nil, nil, false
	}
	sub := make(chan OutboxEvent, outboxStreamBuffer)
	id := o.nextSub
	o.nextSub++
	o.subs[id] = sub
	var once sync.Once
	unsubscribe = func() {
		once.Do(func() {
			o.mu.Lock()
			defer o.mu.Unlock()
			if c, found := o.subs[id]; found {
				delete(o.subs, id)
				close(c)
			}
		})
	}
	return sub, unsubscribe, true
}

// deliverLocked fans one committed event out to every live-tail
// subscriber. Callers must hold o.mu (Emit does). Delivery is strictly
// non-blocking: a full subscriber is dropped and its channel closed,
// counted as a slow-consumer drop — the commit path never waits on a
// consumer, and the dropped consumer reconnects with its cursor.
func (o *Outbox) deliverLocked(ev OutboxEvent) {
	for id, sub := range o.subs {
		select {
		case sub <- ev:
		default:
			delete(o.subs, id)
			close(sub)
			o.slowDrops.Add(1)
		}
	}
}

// Stats reports how many events were durably written and how many were
// dropped. A growing dropped count means the disk cannot keep up or is
// failing — alert on it, because the stream then has gaps.
func (o *Outbox) Stats() (written, dropped uint64) {
	return o.written.Load(), o.dropped.Load()
}

// LastSeq reports the most recently assigned event seq. The webhook
// dispatcher (see webhook.go) uses it to stamp the outbox seq onto the
// event it fans out: Emit assigns the seq to its own copy, and every
// Emit funnels through emitOutboxLocked under the ledger's write lock,
// so no other emit can interleave between Emit and this call.
func (o *Outbox) LastSeq() uint64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.seq
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
	Since    time.Time
	Until    time.Time
	Types    []string
	Account  AccountID
	AfterSeq uint64
	Limit    int
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
// The ledger's tenant is stamped onto the event, so the process-wide
// outbox stream stays attributable per tenant (see tenant.go); the
// default tenant's events carry no tenant field, exactly as before.
func (l *Ledger) emitOutboxLocked(ev OutboxEvent) {
	if l.outbox == nil {
		return
	}
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	if l.tenantID != DefaultTenantID {
		ev.Tenant = string(l.tenantID)
	}
	if !l.outbox.Emit(ev) {
		// The event was dropped (disk failure, closed outbox): it
		// never entered the stream, so no subscriber sees it. The
		// dropped counter is the record of the gap.
		return
	}
	if l.webhooks == nil {
		return
	}
	// Fan out to the webhook dispatcher (LG-102): the event is
	// durable now, so stamp the assigned seq (see LastSeq) and notify.
	// Notify never blocks — a full endpoint queue drops the event
	// rather than stalling this commit.
	ev.Seq = l.outbox.LastSeq()
	ev.At = ev.At.UTC()
	l.webhooks.Notify(ev)
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

// SlowDrops reports how many live-tail subscribers were dropped for
// falling behind (see Subscribe): each one must reconnect with its
// cursor. A rising count means consumers cannot keep up with the event
// rate — add consumer capacity or shard the stream.
func (o *Outbox) SlowDrops() uint64 {
	return o.slowDrops.Load()
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

// OutboxSubscribe registers a live tail on the domain-event outbox
// (see Outbox.Subscribe, LG-133): the HTTP layer's GET /events/stream
// endpoint. ok is false when no outbox is attached.
func (l *Ledger) OutboxSubscribe() (ch <-chan OutboxEvent, unsubscribe func(), ok bool) {
	if l.outbox == nil {
		return nil, nil, false
	}
	return l.outbox.Subscribe()
}

// OutboxSlowDrops reports the outbox's live-tail slow-consumer drop
// count (see Outbox.SlowDrops). ok is false when no outbox is attached.
func (l *Ledger) OutboxSlowDrops() (uint64, bool) {
	if l.outbox == nil {
		return 0, false
	}
	return l.outbox.SlowDrops(), true
}
