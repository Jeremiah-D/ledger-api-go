package ledger

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sort"
	"time"
)

// PII erasure — the right to be forgotten (LG-131).
//
// A ledger is append-only by design: journal rows are sealed into a
// tamper-evident hash chain (see chain.go), so "deleting" a journaled
// business note would destroy the audit property the whole system is
// built on. Erasure therefore works in two layers, and the distinction
// is load-bearing:
//
//  1. Account holder names live in a mutable PII registry
//     (SetAccountHolderName). Erasing one overwrites the stored bytes
//     with cryptographic randomness and drops the record — true
//     deletion, with a tombstone left behind for the audit trail.
//  2. Entry memos live on sealed journal rows whose bytes the chain
//     hash covers (LG-38). Erasing a memo does NOT rewrite the row
//     (that would break VerifyChain); instead the erasure is recorded
//     in an erasure registry, and every read/export path redacts the
//     memo to "[erased]". The books stay verifiable, the PII stops
//     being disclosed. The erasure event itself is appended to the
//     structured audit chain, so the chain's hash continuity is
//     preserved — the erasure is proven, not hidden.
//
// Erasure is maker-checker: ErasePII files a request, and a DIFFERENT
// reviewer must approve it (the LG-126 dual-control convention).
// Self-approval is rejected.
//
// The erasable field set is a closed whitelist. Amounts, accounts,
// currencies, journal legs — the financial facts — can never be
// erased: that is the tamper floor, and ErasePII refuses them loudly
// instead of silently ignoring them.
//
// All entry points are safe for concurrent use; the request lifecycle
// and the registries are guarded by l.mu.

// Erasable PII fields.
const (
	// PIIFieldHolderName erases the account holder name registry entry.
	PIIFieldHolderName = "holder_name"
	// PIIFieldMemo erases memos on the account's journal entries
	// (redaction-registry layer, see above).
	PIIFieldMemo = "memo"
)

// ErasedMemoTombstone is what read/export paths return for an erased
// memo.
const ErasedMemoTombstone = "[erased]"

// Erasure request statuses.
const (
	ErasureStatusPending  = "pending"
	ErasureStatusApproved = "approved"
	ErasureStatusRejected = "rejected"
)

// piiErasableFields is the closed whitelist of erasable fields.
var piiErasableFields = map[string]bool{
	PIIFieldHolderName: true,
	PIIFieldMemo:       true,
}

// Validation and state errors returned by the PII erasure APIs.
var (
	// ErrPIIFieldNotErasable is returned when ErasePII is asked to
	// erase a field outside the whitelist — notably amounts, accounts,
	// currencies, and journal legs, which are financial facts and can
	// never be erased.
	ErrPIIFieldNotErasable = errors.New("ledger: PII field is not erasable")
	// ErrInvalidHolderName is returned for an empty or overlong holder
	// name.
	ErrInvalidHolderName = errors.New("ledger: holder name must be 1-128 characters")
	// ErrErasureNotFound is returned for an unknown erasure request ID.
	ErrErasureNotFound = errors.New("ledger: PII erasure request not found")
	// ErrErasureNotPending is returned when approving/rejecting a
	// request that is no longer pending.
	ErrErasureNotPending = errors.New("ledger: PII erasure request is not pending")
	// ErrErasureSelfApproval is returned when the reviewer is the same
	// operator who filed the request: erasure needs dual control (the
	// LG-126 maker-checker convention).
	ErrErasureSelfApproval = errors.New("ledger: PII erasure must be approved by a different reviewer")
	// ErrErasureInvalidReviewer is returned for an empty reviewer.
	ErrErasureInvalidReviewer = errors.New("ledger: PII erasure reviewer must not be empty")
)

// holderNameRecord is one account's holder name. The name is kept as
// bytes (not a string) so erasure can overwrite the backing store
// with randomness before the record is dropped.
type holderNameRecord struct {
	name      []byte
	updatedAt time.Time
}

// ErasureRequest is one filed PII erasure: pending until a different
// reviewer approves or rejects it.
type ErasureRequest struct {
	ID          string     `json:"id"`
	Account     AccountID  `json:"account"`
	Fields      []string   `json:"fields"`
	RequestedBy string     `json:"requested_by"`
	RequestedAt time.Time  `json:"requested_at"`
	Status      string     `json:"status"`
	Reviewer    string     `json:"reviewer,omitempty"`
	ReviewedAt  time.Time  `json:"reviewed_at,omitempty"`
	Reason      string     `json:"reason,omitempty"`
}

// ErasureRecord is the tombstone left by an executed erasure: what was
// erased, when, under which request. For memos, EntryID names the
// journal row whose memo is redacted on read; the sealed row itself is
// untouched.
type ErasureRecord struct {
	RequestID string    `json:"request_id"`
	Account   AccountID `json:"account"`
	Field     string    `json:"field"`
	EntryID   string    `json:"entry_id,omitempty"`
	ErasedAt  time.Time `json:"erased_at"`
}

// piiHolderNameSnapshot is the serializable form of an unerased holder
// name for snapshot round-trips.
type piiHolderNameSnapshot struct {
	Account   AccountID `json:"account"`
	Name      string    `json:"name"`
	UpdatedAt string    `json:"updated_at"`
}

// SetAccountHolderName records the account holder's name in the PII
// registry. The audit event carries the account only, never the name —
// PII values do not belong in the audit trail.
func (l *Ledger) SetAccountHolderName(account AccountID, name string) error {
	return l.SetAccountHolderNameCtx(context.Background(), account, name)
}

// SetAccountHolderNameCtx is SetAccountHolderName with actor
// attribution from ctx.
func (l *Ledger) SetAccountHolderNameCtx(ctx context.Context, account AccountID, name string) error {
	if len(name) == 0 || len([]rune(name)) > 128 {
		return ErrInvalidHolderName
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.holderNames[account] = &holderNameRecord{name: []byte(name), updatedAt: time.Now().UTC()}
	l.emitAuditCtx(ctx, AuditEvent{
		Op:       "account_holder_name_set",
		Actor:    "SetAccountHolderName",
		Accounts: []AccountID{account},
	})
	return nil
}

// AccountHolderName returns the account's holder name, or false when
// none is recorded (including after erasure).
func (l *Ledger) AccountHolderName(account AccountID) (string, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	rec, ok := l.holderNames[account]
	if !ok {
		return "", false
	}
	return string(rec.name), true
}

// ErasePII files a PII erasure request for the account. Fields must
// come from the erasable whitelist; anything else (amounts, legs,
// accounts, currencies) fails with ErrPIIFieldNotErasable. The request
// is pending until a different reviewer approves it — nothing is
// erased at filing time.
func (l *Ledger) ErasePII(account AccountID, fields []string, requestedBy string) (*ErasureRequest, error) {
	return l.ErasePIIWithCtx(context.Background(), account, fields, requestedBy)
}

// ErasePIIWithCtx is ErasePII with actor attribution from ctx.
func (l *Ledger) ErasePIIWithCtx(ctx context.Context, account AccountID, fields []string, requestedBy string) (*ErasureRequest, error) {
	if len(fields) == 0 {
		return nil, ErrPIIFieldNotErasable
	}
	seen := make(map[string]bool, len(fields))
	clean := make([]string, 0, len(fields))
	for _, f := range fields {
		if !piiErasableFields[f] {
			return nil, fmt.Errorf("%w: %q", ErrPIIFieldNotErasable, f)
		}
		if !seen[f] {
			seen[f] = true
			clean = append(clean, f)
		}
	}
	sort.Strings(clean)
	if requestedBy == "" {
		return nil, ErrErasureInvalidReviewer
	}
	now := time.Now().UTC()
	req := &ErasureRequest{
		ID:          fmt.Sprintf("erasure-%d", now.UnixNano()),
		Account:     account,
		Fields:      clean,
		RequestedBy: requestedBy,
		RequestedAt: now,
		Status:      ErasureStatusPending,
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, dup := l.erasureRequests[req.ID]; dup {
		// Nanosecond collision in the same process: disambiguate.
		req.ID += "-1"
	}
	l.erasureRequests[req.ID] = req
	out := *req
	l.emitAuditCtx(ctx, AuditEvent{
		Op:       "pii_erasure_requested",
		Actor:    "ErasePII",
		Accounts: []AccountID{account},
		Details:  map[string]any{"request_id": req.ID, "fields": clean, "requested_by": requestedBy},
	})
	return &out, nil
}

// ApproveErasure approves a pending erasure request and executes it.
// The reviewer must differ from the requester (dual control). On
// success the erasure is irreversible.
func (l *Ledger) ApproveErasure(requestID, reviewer string) (*ErasureRequest, error) {
	return l.ApproveErasureCtx(context.Background(), requestID, reviewer)
}

// ApproveErasureCtx is ApproveErasure with actor attribution from ctx.
func (l *Ledger) ApproveErasureCtx(ctx context.Context, requestID, reviewer string) (*ErasureRequest, error) {
	if reviewer == "" {
		return nil, ErrErasureInvalidReviewer
	}
	now := time.Now().UTC()
	l.mu.Lock()
	defer l.mu.Unlock()
	req, ok := l.erasureRequests[requestID]
	if !ok {
		return nil, ErrErasureNotFound
	}
	if req.Status != ErasureStatusPending {
		return nil, ErrErasureNotPending
	}
	if reviewer == req.RequestedBy {
		return nil, ErrErasureSelfApproval
	}
	req.Status = ErasureStatusApproved
	req.Reviewer = reviewer
	req.ReviewedAt = now
	for _, f := range req.Fields {
		switch f {
		case PIIFieldHolderName:
			l.eraseHolderNameLocked(req.Account, req.ID, now)
		case PIIFieldMemo:
			l.eraseAccountMemosLocked(req.Account, req.ID, now)
		}
	}
	l.erasureVersion++
	out := *req
	l.emitAuditCtx(ctx, AuditEvent{
		Op:       "pii_erasure_approved",
		Actor:    "ApproveErasure",
		Accounts: []AccountID{req.Account},
		Details:  map[string]any{"request_id": req.ID, "fields": req.Fields, "reviewer": reviewer},
	})
	// The erasure event itself enters the audit chain: the chain's hash
	// continuity is preserved — erasure is proven, not hidden.
	l.emitAuditCtx(ctx, AuditEvent{
		Op:       "pii_erasure_executed",
		Actor:    "ApproveErasure",
		Accounts: []AccountID{req.Account},
		Details: map[string]any{
			"request_id": req.ID, "fields": req.Fields,
			"reviewer": reviewer, "erasure_version": l.erasureVersion,
		},
	})
	return &out, nil
}

// eraseHolderNameLocked scrubs the holder name: the backing bytes are
// overwritten with cryptographic randomness before the record is
// dropped, and a tombstone is recorded. Callers must hold the write
// lock.
func (l *Ledger) eraseHolderNameLocked(account AccountID, requestID string, now time.Time) {
	rec, ok := l.holderNames[account]
	if !ok {
		return
	}
	if _, err := rand.Read(rec.name); err != nil {
		// Randomness failure must never leave recoverable PII: zero
		// the bytes instead.
		for i := range rec.name {
			rec.name[i] = 0
		}
	}
	delete(l.holderNames, account)
	l.erasureRecords = append(l.erasureRecords, ErasureRecord{
		RequestID: requestID, Account: account, Field: PIIFieldHolderName, ErasedAt: now,
	})
}

// eraseAccountMemosLocked records erasure marks for every journal row
// touching the account. The sealed rows are NOT rewritten — the chain
// hash covers their bytes (LG-38) — so read/export paths redact via
// the marks. Callers must hold the write lock.
func (l *Ledger) eraseAccountMemosLocked(account AccountID, requestID string, now time.Time) {
	for _, entryID := range l.byAccount[account] {
		if _, dup := l.erasedMemos[entryID]; dup {
			continue
		}
		rec := ErasureRecord{
			RequestID: requestID, Account: account, Field: PIIFieldMemo, EntryID: entryID, ErasedAt: now,
		}
		l.erasedMemos[entryID] = rec
		l.erasureRecords = append(l.erasureRecords, rec)
	}
}

// RejectErasure terminally rejects a pending erasure request. Nothing
// is erased.
func (l *Ledger) RejectErasure(requestID, reviewer, reason string) (*ErasureRequest, error) {
	return l.RejectErasureCtx(context.Background(), requestID, reviewer, reason)
}

// RejectErasureCtx is RejectErasure with actor attribution from ctx.
func (l *Ledger) RejectErasureCtx(ctx context.Context, requestID, reviewer, reason string) (*ErasureRequest, error) {
	if reviewer == "" {
		return nil, ErrErasureInvalidReviewer
	}
	now := time.Now().UTC()
	l.mu.Lock()
	defer l.mu.Unlock()
	req, ok := l.erasureRequests[requestID]
	if !ok {
		return nil, ErrErasureNotFound
	}
	if req.Status != ErasureStatusPending {
		return nil, ErrErasureNotPending
	}
	req.Status = ErasureStatusRejected
	req.Reviewer = reviewer
	req.ReviewedAt = now
	req.Reason = reason
	out := *req
	l.emitAuditCtx(ctx, AuditEvent{
		Op:       "pii_erasure_rejected",
		Actor:    "RejectErasure",
		Accounts: []AccountID{req.Account},
		Details:  map[string]any{"request_id": req.ID, "reviewer": reviewer, "reason": reason},
	})
	return &out, nil
}

// GetErasureRequest returns a copy of the erasure request.
func (l *Ledger) GetErasureRequest(id string) (*ErasureRequest, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	req, ok := l.erasureRequests[id]
	if !ok {
		return nil, false
	}
	out := *req
	return &out, true
}

// ErasureRecords returns the erasure tombstones for an account, in
// execution order.
func (l *Ledger) ErasureRecords(account AccountID) []ErasureRecord {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var out []ErasureRecord
	for _, r := range l.erasureRecords {
		if r.Account == account {
			out = append(out, r)
		}
	}
	return out
}

// ErasedMemo reports whether the entry's memo was erased.
func (l *Ledger) ErasedMemo(entryID string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	_, ok := l.erasedMemos[entryID]
	return ok
}

// RedactEntryMemo returns the entry with its memo replaced by the
// erasure tombstone when the memo was erased. Empty memos are left
// alone (there is nothing to disclose). The stored row is never
// mutated — the sealed journal bytes stay byte-identical so
// VerifyChain keeps passing.
func (l *Ledger) RedactEntryMemo(e JournalEntry) JournalEntry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.redactEntryMemoLocked(e)
}

// redactEntryMemoLocked is RedactEntryMemo for callers already holding
// l.mu (either lock suffices) — e.g. ExportOFX, which iterates rows
// under the read lock.
func (l *Ledger) redactEntryMemoLocked(e JournalEntry) JournalEntry {
	if e.Memo == "" {
		return e
	}
	if _, ok := l.erasedMemos[e.ID]; !ok {
		return e
	}
	e.Memo = ErasedMemoTombstone
	return e
}

// ErasureVersion is the monotonic erasure counter: every executed
// erasure bumps it. Export manifests (LG-128) annotate it so
// downstream consumers can tell whether their copy predates an
// erasure.
func (l *Ledger) ErasureVersion() uint64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.erasureVersion
}

// ErasureCount returns the number of executed erasure tombstones.
func (l *Ledger) ErasureCount() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.erasureRecords)
}

// piiSnapshotLocked exports the PII registries for the snapshot config
// line: unerased holder names (so a restore keeps working data),
// erasure marks (so a restore stays erased), and the version counter.
// Callers must hold l.mu (either lock suffices).
func (l *Ledger) piiSnapshotLocked() (names []piiHolderNameSnapshot, erasures []ErasureRecord, version uint64) {
	for a, rec := range l.holderNames {
		names = append(names, piiHolderNameSnapshot{
			Account:   a,
			Name:      string(rec.name),
			UpdatedAt: rec.updatedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	sort.Slice(names, func(i, j int) bool { return names[i].Account < names[j].Account })
	erasures = append([]ErasureRecord(nil), l.erasureRecords...)
	return names, erasures, l.erasureVersion
}

// restorePIILocked validates and installs the PII registries from a
// snapshot. Malformed rows fail the import instead of silently losing
// erasure state. Callers must hold the write lock.
func (l *Ledger) restorePIILocked(names []piiHolderNameSnapshot, erasures []ErasureRecord, version uint64) error {
	seenNames := make(map[AccountID]bool, len(names))
	for _, n := range names {
		if n.Account == "" || seenNames[n.Account] {
			return fmt.Errorf("duplicate or empty PII holder account %q", n.Account)
		}
		seenNames[n.Account] = true
		if len(n.Name) == 0 || len([]rune(n.Name)) > 128 {
			return fmt.Errorf("bad PII holder name for account %q", n.Account)
		}
		updatedAt, err := time.Parse(time.RFC3339Nano, n.UpdatedAt)
		if err != nil {
			return fmt.Errorf("bad PII holder updated_at for account %q: %w", n.Account, err)
		}
		l.holderNames[n.Account] = &holderNameRecord{name: []byte(n.Name), updatedAt: updatedAt.UTC()}
	}
	seenErasure := make(map[string]bool, len(erasures))
	for _, r := range erasures {
		key := r.RequestID + "\x00" + r.Field + "\x00" + r.EntryID
		if seenErasure[key] {
			return fmt.Errorf("duplicate PII erasure record %q", key)
		}
		seenErasure[key] = true
		if !piiErasableFields[r.Field] {
			return fmt.Errorf("PII erasure record with non-erasable field %q", r.Field)
		}
		l.erasureRecords = append(l.erasureRecords, r)
		if r.Field == PIIFieldMemo && r.EntryID != "" {
			l.erasedMemos[r.EntryID] = r
		}
	}
	if version < uint64(len(erasures)) {
		return fmt.Errorf("PII erasure version %d below record count %d", version, len(erasures))
	}
	l.erasureVersion = version
	return nil
}
