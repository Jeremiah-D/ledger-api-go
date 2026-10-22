package ledger

import (
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// Payment repair queue (LG-177): pre-issuance repair for payout
// instructions that fail straight-through processing.
//
// Orthogonal to disputes (see dispute.go): disputes are post-hoc — a
// payer clawing back money that already moved, freezing the payee's
// funds through an authorization hold. The repair queue is
// pre-issuance: a payout instruction whose STP validation failed
// (fields missing, format invalid, route unknown) is parked here
// instead of being hard-rejected, so an operator can complete the
// fields and resubmit through the exact same CreatePayout gate that
// rejected it. No money moves through the repair queue — records are
// operational state, like payout instructions themselves (see
// payout.go): they do not round-trip through disaster-recovery
// snapshots, and enqueue/resubmit bump neither the journal version
// nor the audit chain head beyond the audit events they emit.
//
// Lifecycle: open -> resubmitted (the fixed instruction passed
// CreatePayout and the payout draft exists) is the success path;
// open -> expired is the TTL path. Both terminal states are
// read-only: a terminal record can never be resubmitted again.
//
// Storage note: this module is intentionally self-contained — the
// whole feature lives in repair.go (plus its test). The queue rides
// a package-level registry keyed by *Ledger rather than a Ledger
// struct field, so no other file is touched. A ledger's queue lives
// as long as the ledger; the production process runs one ledger, and
// the registry entry is tiny (an empty map) for short-lived test
// ledgers. Lock order is always l.mu first, then the registry mutex
// — never the reverse.

// Repair queue statuses.
const (
	// RepairStatusOpen is a parked instruction awaiting operator
	// repair and resubmission.
	RepairStatusOpen = "open"
	// RepairStatusResubmitted is terminal: the fixed instruction
	// passed CreatePayout and the payout draft was created (or
	// idempotently replayed). PayoutID names it.
	RepairStatusResubmitted = "resubmitted"
	// RepairStatusExpired is terminal: the record passed its TTL with
	// no operator activity. Read-only.
	RepairStatusExpired = "expired"
)

// Repair reason codes: the admitted vocabulary for why an
// instruction failed STP. Anything outside this set is rejected at
// enqueue time — the queue never holds an unclassifiable failure.
const (
	// RepairReasonMissingField: a required field was absent
	// (payout ID, payee account, destination, maker, amount).
	RepairReasonMissingField = "missing_field"
	// RepairReasonInvalidFormat: a field was present but malformed
	// (bad currency code, '/' in the payout ID, unparseable value).
	RepairReasonInvalidFormat = "invalid_format"
	// RepairReasonUnknownRoute: the destination named no known route
	// (a <system>:<externalID> destination that does not match the
	// registered external mapping for the payee).
	RepairReasonUnknownRoute = "unknown_route"
)

// repairReasonCodes is the accepted reason-code set for validation.
var repairReasonCodes = map[string]bool{
	RepairReasonMissingField:  true,
	RepairReasonInvalidFormat: true,
	RepairReasonUnknownRoute:  true,
}

// defaultRepairQueueCap bounds the repair queue: 1000 parked
// instructions. Enqueue on a full queue fails with
// ErrRepairQueueFull — fail-closed, never silently evicting the
// oldest record to make room.
const defaultRepairQueueCap = 1000

// defaultRepairTTL is how long an open repair record may sit without
// operator activity before it is marked expired: 7 days. Past that the
// instruction is stale — the operator should re-raise it from the
// source system rather than repair a week-old snapshot.
const defaultRepairTTL = 7 * 24 * time.Hour

// Repair errors.
var (
	// ErrRepairUnknownReasonCode is returned by EnqueueRepair when the
	// reason code is not one of the admitted codes
	// (missing_field, invalid_format, unknown_route).
	ErrRepairUnknownReasonCode = errors.New("ledger: unknown repair reason code")
	// ErrRepairEmptyActor is returned when the operator actor is empty:
	// every repair action needs an actor for the audit trail.
	ErrRepairEmptyActor = errors.New("ledger: repair actor must not be empty")
	// ErrRepairQueueFull is returned by EnqueueRepair when the queue
	// already holds cap records: fail-closed, never silently evict.
	ErrRepairQueueFull = errors.New("ledger: repair queue is full")
	// ErrRepairNotFound is returned by RepairAndResubmit and
	// GetRepair for an unknown repair ID. It is a 404-class error.
	ErrRepairNotFound = errors.New("ledger: repair record not found")
	// ErrRepairTerminal is returned by RepairAndResubmit on a record
	// that is already resubmitted or expired: terminal records are
	// read-only. It is a 409-class error.
	ErrRepairTerminal = errors.New("ledger: repair record is terminal (resubmitted or expired)")
	// ErrRepairUnknownField is returned by RepairAndResubmit when
	// fixedFields names a field outside the admitted set:
	// fail-closed, so a typo can never silently drop an operator fix.
	ErrRepairUnknownField = errors.New("ledger: unknown repair field name")
	// ErrRepairInvalidAmount is returned by RepairAndResubmit when the
	// fixed "amount_cents" value does not parse as an integer.
	ErrRepairInvalidAmount = errors.New("ledger: repair amount_cents is not a valid integer")
)

// RepairRecord is one parked payout instruction: the immutable
// snapshot of the instruction as STP saw it, the admitted reason it
// failed, and the operator repair state.
//
// Instruction is a defensive copy taken at enqueue time —
// PayoutInstruction carries only value fields (strings, int64,
// time.Time), so the struct copy is a full deep copy: later mutation
// of the caller's PayoutInstruction cannot change the record, and
// the record handed back to callers is a copy too.
//
// RepairCount counts resubmit attempts (every RepairAndResubmit
// call, success or failure). Status moves open -> resubmitted or
// open -> expired, exactly once each way. PayoutID is set on the
// success path.
type RepairRecord struct {
	ID          string            `json:"id"`
	Instruction PayoutInstruction `json:"instruction"`
	ReasonCode  string            `json:"reason_code"`
	RepairCount int               `json:"repair_count"`
	Status      string            `json:"status"`
	PayoutID    string            `json:"payout_id,omitempty"`
	EnqueuedAt  time.Time         `json:"enqueued_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
	TTL         time.Duration     `json:"ttl"`
}

// repairQueue is one ledger's bounded repair queue.
type repairQueue struct {
	records map[string]*RepairRecord
	seq     uint64 // repair-record sequence; IDs are "repair-<seq>"
	cap     int
}

// repairQueues is the per-ledger registry (see the package comment).
// Callers must hold l.mu before taking repairQueues.Mutex.
var repairQueues = struct {
	sync.Mutex
	m map[*Ledger]*repairQueue
}{m: make(map[*Ledger]*repairQueue)}

// repairQueueForLocked returns l's repair queue, creating it on first
// use. Callers must hold l.mu and repairQueues.Mutex.
func repairQueueForLocked(l *Ledger) *repairQueue {
	q, ok := repairQueues.m[l]
	if !ok {
		q = &repairQueue{records: make(map[string]*RepairRecord), cap: defaultRepairQueueCap}
		repairQueues.m[l] = q
	}
	return q
}

// WithRepairQueueCap overrides the repair queue's capacity for this
// ledger. Non-positive values keep the default (1000). Mainly a test
// hook — production ledgers run the default.
func WithRepairQueueCap(n int) Option {
	return func(l *Ledger) {
		if n <= 0 {
			return
		}
		repairQueues.Lock()
		defer repairQueues.Unlock()
		repairQueueForLocked(l).cap = n
	}
}

// repairAdmittedFields is the field vocabulary an operator may fix on
// a parked snapshot. Only fields that exist on PayoutInstruction are
// admitted — "payer_account" and "memo" are deliberately absent:
// PayoutInstruction has no such fields, and admitting them would let
// an operator "fix" silently into the void instead of failing
// closed. Anything outside this set fails the resubmit with
// ErrRepairUnknownField before any state is touched.
var repairAdmittedFields = map[string]bool{
	"payee_account":   true,
	"amount_cents":    true,
	"currency":        true,
	"destination":     true,
	"idempotency_key": true,
	"maker":           true,
}

// validateRepairFields fail-closes on unknown fixed-field names. It
// runs before any state is read or written, so a bad name can never
// half-apply an operator fix.
func validateRepairFields(fixed map[string]string) error {
	for name := range fixed {
		if !repairAdmittedFields[name] {
			return fmt.Errorf("%w: %q", ErrRepairUnknownField, name)
		}
	}
	return nil
}

// applyRepairFields overlays the admitted fixed fields onto dst. All
// names were pre-validated by validateRepairFields; a value that does
// not parse (amount_cents) fails closed. Semantic validation (empty
// destination, bad currency, non-positive amount) is intentionally
// left to CreatePayout on resubmit — this module never copies the
// STP validation logic.
func applyRepairFields(dst *PayoutInstruction, fixed map[string]string) error {
	for name, value := range fixed {
		switch name {
		case "payee_account":
			dst.PayeeAccount = AccountID(value)
		case "amount_cents":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return fmt.Errorf("%w: %q", ErrRepairInvalidAmount, value)
			}
			dst.AmountCents = n
		case "currency":
			dst.Currency = value
		case "destination":
			dst.Destination = value
		case "idempotency_key":
			dst.IdempotencyKey = value
		case "maker":
			dst.Maker = value
		}
	}
	return nil
}

// repairReasonFor maps a CreatePayout rejection back onto the
// admitted reason vocabulary for the re-enqueue. Unknown-route is
// checked first (it wraps), then the missing-field sentinels; anything
// else — bad currency, '/' in the ID, ID conflicts, transition
// errors — is an invalid format/value. The default is fail-closed
// towards keeping the record in the queue, never dropping it.
func repairReasonFor(err error) string {
	if errors.Is(err, ErrPayoutUnknownDestination) {
		return RepairReasonUnknownRoute
	}
	switch {
	case errors.Is(err, ErrEmptyPayoutID),
		errors.Is(err, ErrEmptyPayoutAccount),
		errors.Is(err, ErrEmptyPayoutDestination),
		errors.Is(err, ErrEmptyPayoutMaker),
		errors.Is(err, ErrEmptyPayoutApprover),
		errors.Is(err, ErrPayoutNonPositiveAmount):
		return RepairReasonMissingField
	default:
		return RepairReasonInvalidFormat
	}
}

// sameRepairInstruction reports whether got is the payout this repair
// record's instruction would create: same ID and the same core
// instruction content. Used to recognize an idempotent resubmission
// that raced another record for the same instruction ID.
func sameRepairInstruction(got, want PayoutInstruction) bool {
	return got.ID == want.ID &&
		got.PayeeAccount == want.PayeeAccount &&
		got.AmountCents == want.AmountCents &&
		got.Currency == want.Currency &&
		got.Destination == want.Destination &&
		got.Maker == want.Maker &&
		got.IdempotencyKey == want.IdempotencyKey
}

// EnqueueRepair parks a payout instruction that failed
// straight-through processing, instead of hard-rejecting it. The
// instruction is stored as an immutable snapshot copy — mutating the
// caller's PayoutInstruction afterwards cannot change the record.
//
// The reason code must be one of the admitted codes
// (missing_field, invalid_format, unknown_route); the queue is
// bounded — a full queue fails with ErrRepairQueueFull, never
// silently evicting. The record ID is "repair-<seq>", unique per
// ledger. One "repair_enqueued" audit event fires, carrying the
// snapshot as submitted.
func (l *Ledger) EnqueueRepair(instruction PayoutInstruction, reasonCode, actor string) (*RepairRecord, error) {
	if !repairReasonCodes[reasonCode] {
		return nil, ErrRepairUnknownReasonCode
	}
	if actor == "" {
		return nil, ErrRepairEmptyActor
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	repairQueues.Lock()
	defer repairQueues.Unlock()
	q := repairQueueForLocked(l)
	if len(q.records) >= q.cap {
		return nil, ErrRepairQueueFull
	}

	q.seq++
	now := time.Now()
	rec := &RepairRecord{
		ID:          fmt.Sprintf("repair-%d", q.seq),
		Instruction: instruction, // value copy: PayoutInstruction is all value fields
		ReasonCode:  reasonCode,
		Status:      RepairStatusOpen,
		EnqueuedAt:  now,
		UpdatedAt:   now,
		TTL:         defaultRepairTTL,
	}
	q.records[rec.ID] = rec

	l.emitAudit(AuditEvent{
		Op:            "repair_enqueued",
		Actor:         actor,
		TraceID:       rec.ID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{instruction.PayeeAccount},
		Details: map[string]any{
			"reason_code": reasonCode,
			"instruction": instruction,
			"ttl":         rec.TTL.String(),
		},
	})

	out := *rec
	return &out, nil
}

// GetRepair returns a copy of the repair record with the given ID, or
// false when no such record exists.
func (l *Ledger) GetRepair(id string) (RepairRecord, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	repairQueues.Lock()
	defer repairQueues.Unlock()
	rec, ok := repairQueueForLocked(l).records[id]
	if !ok {
		return RepairRecord{}, false
	}
	return *rec, true
}

// expireRepairLocked marks an open record expired: terminal and
// read-only. Callers must hold l.mu (write) and repairQueues.Mutex.
func expireRepairLocked(l *Ledger, rec *RepairRecord, now time.Time, actor string) {
	rec.Status = RepairStatusExpired
	rec.UpdatedAt = now
	l.emitAudit(AuditEvent{
		Op:            "repair_expired",
		Actor:         actor,
		TraceID:       rec.ID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{rec.Instruction.PayeeAccount},
		Details: map[string]any{
			"reason_code":  rec.ReasonCode,
			"repair_count": rec.RepairCount,
			"ttl":          rec.TTL.String(),
		},
	})
}

// SweepExpiredRepairs marks every open record older than its TTL with
// no activity as expired and returns how many were marked. A zero now
// means time.Now(). The sweep is the explicit expiry path; resubmit
// also expires lazily (see RepairAndResubmit), so the terminal
// invariant holds even when nobody sweeps.
func (l *Ledger) SweepExpiredRepairs(now time.Time) int {
	if now.IsZero() {
		now = time.Now()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	repairQueues.Lock()
	defer repairQueues.Unlock()

	marked := 0
	for _, rec := range repairQueueForLocked(l).records {
		if rec.Status == RepairStatusOpen && now.Sub(rec.UpdatedAt) > rec.TTL {
			expireRepairLocked(l, rec, now, "SweepExpiredRepairs")
			marked++
		}
	}
	return marked
}

// RepairAndResubmit lets an operator complete fields on a parked
// instruction snapshot and resubmit it.
//
// fixedFields names are fail-closed against the admitted set
// (payee_account, amount_cents, currency, destination,
// idempotency_key, maker) — an unknown name rejects the whole call
// before any state is touched, and an unparseable amount_cents does
// the same. The fixed snapshot is then run through the FULL
// CreatePayout gate — the same validation path a fresh STP
// submission takes, reused rather than copied — carrying the
// original instruction's ID and idempotency key, so a retried STP
// submission replays instead of double-creating.
//
//   - Success: the payout draft is created (or idempotently replayed:
//     when the instruction's payout already exists from a racing
//     resubmit, the call converges on it). The record goes terminal
//     (resubmitted), RepairCount counts this attempt, and the payout
//     continues through the normal ApprovePayout/SubmitPayout
//     lifecycle — resubmission only re-runs the STP gate, it does not
//     approve or submit the payout.
//   - Failure: the record re-enqueues — the operator's fixes are
//     kept on the snapshot, RepairCount is incremented, the reason
//     code is re-derived from the CreatePayout rejection, and the
//     record stays open for another attempt. The rejection is
//     returned to the caller.
//
// One "repair_resubmitted" audit event fires per attempt (success or
// failure), carrying the before and after record state.
//
// Terminal records (resubmitted or expired) are read-only: resubmits
// fail with ErrRepairTerminal. An open record past its TTL is expired
// lazily here even if SweepExpiredRepairs never ran. The CreatePayout
// call itself runs without l.mu held (it takes its own lock); the
// prepare and commit sections each hold l.mu and the registry mutex,
// in that order.
func (l *Ledger) RepairAndResubmit(id string, fixedFields map[string]string, actor string) (PayoutInstruction, error) {
	if actor == "" {
		return PayoutInstruction{}, ErrRepairEmptyActor
	}
	if err := validateRepairFields(fixedFields); err != nil {
		return PayoutInstruction{}, err
	}

	now := time.Now()

	// Prepare: fetch the record, enforce the terminal and TTL
	// invariants, and apply the operator's fixes to a working copy of
	// the snapshot.
	l.mu.Lock()
	repairQueues.Lock()
	rec, ok := repairQueueForLocked(l).records[id]
	if !ok {
		repairQueues.Unlock()
		l.mu.Unlock()
		return PayoutInstruction{}, ErrRepairNotFound
	}
	if rec.Status != RepairStatusOpen {
		repairQueues.Unlock()
		l.mu.Unlock()
		return PayoutInstruction{}, ErrRepairTerminal
	}
	if now.Sub(rec.UpdatedAt) > rec.TTL {
		expireRepairLocked(l, rec, now, actor)
		repairQueues.Unlock()
		l.mu.Unlock()
		return PayoutInstruction{}, ErrRepairTerminal
	}
	before := *rec
	fixed := rec.Instruction // value copy of the snapshot
	if err := applyRepairFields(&fixed, fixedFields); err != nil {
		repairQueues.Unlock()
		l.mu.Unlock()
		return PayoutInstruction{}, err
	}
	repairQueues.Unlock()
	l.mu.Unlock()

	// The full STP gate, reused — never a copy of its validation.
	created, _, err := l.CreatePayout(fixed)
	var payout PayoutInstruction
	switch {
	case err == nil:
		payout = created
	case errors.Is(err, ErrPayoutIDConflict):
		// The instruction's payout already exists — a racing
		// resubmit (or a retried STP submission) created it first.
		// Idempotent resubmission converges on the existing payout
		// rather than forking a second one, but only when it really
		// is this instruction: a foreign payout squatting on the ID
		// is a caller bug and re-enqueues as invalid_format.
		got, found := l.GetPayout(fixed.ID)
		if !found || !sameRepairInstruction(got, fixed) {
			err = fmt.Errorf("%w: payout ID %q is owned by a different instruction", ErrPayoutIDConflict, fixed.ID)
			break
		}
		// The payout for this instruction already exists: the
		// resubmission is an idempotent replay, not an error.
		payout = got
		err = nil
	}
	if err != nil {
		// Still broken: keep the operator's fixes, count the attempt,
		// re-derive the reason from the rejection, stay open.
		l.mu.Lock()
		repairQueues.Lock()
		if r, ok := repairQueueForLocked(l).records[id]; ok && r.Status == RepairStatusOpen {
			r.Instruction = fixed
			r.ReasonCode = repairReasonFor(err)
			r.RepairCount++
			r.UpdatedAt = time.Now()
			after := *r
			l.emitAudit(AuditEvent{
				Op:            "repair_resubmitted",
				Actor:         actor,
				TraceID:       id,
				VersionBefore: l.version,
				VersionAfter:  l.version,
				Accounts:      []AccountID{fixed.PayeeAccount},
				Details: map[string]any{
					"success":      false,
					"error":        err.Error(),
					"reason_code":  r.ReasonCode,
					"repair_count": r.RepairCount,
					"before":       before,
					"after":        after,
				},
			})
		}
		repairQueues.Unlock()
		l.mu.Unlock()
		return PayoutInstruction{}, err
	}

	// Success: terminal, pointing at the payout.
	l.mu.Lock()
	repairQueues.Lock()
	if r, ok := repairQueueForLocked(l).records[id]; ok && r.Status == RepairStatusOpen {
		r.Status = RepairStatusResubmitted
		r.PayoutID = payout.ID
		r.Instruction = fixed
		r.RepairCount++
		r.UpdatedAt = time.Now()
		after := *r
		l.emitAudit(AuditEvent{
			Op:            "repair_resubmitted",
			Actor:         actor,
			TraceID:       id,
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Accounts:      []AccountID{payout.PayeeAccount},
			Details: map[string]any{
				"success":      true,
				"payout_id":    payout.ID,
				"repair_count": r.RepairCount,
				"before":       before,
				"after":        after,
			},
		})
	}
	repairQueues.Unlock()
	l.mu.Unlock()
	return payout, nil
}
