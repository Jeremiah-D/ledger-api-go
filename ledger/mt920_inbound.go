package ledger

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// SWIFT FIN MT920 report-request inbound registration (LG-464):
// the inbound half of the MT920 lifecycle, the mirror of the
// LG-459 export.
//
// ExportMT920 asks the account-servicing bank for a report now.
// This module consumes the report request a counterparty bank
// sends to this ledger, acting as the account bank: it registers
// the request against the accounts its :25: names, and nothing
// else. The two directions are orthogonal by construction:
//
//   - LG-459 exports: the ledger tells its bank to send a report
//     (outbound, evidence-only).
//   - LG-464 imports: a counterparty tells the ledger to send a
//     report (inbound, evidence-only).
//
// ParseMT920 is the strict inverse of ExportMT920 (the camt.054
// Parse/Render round-trip precedent): the block structure, the
// field order, and every field value are validated, and anything
// outside the :20: / :12: / :25: / :34F: field set is refused —
// a request must not lose content quietly. CRLF line endings are
// accepted; a trailing newline after the `-}` trailer is
// tolerated. The :34F: floor limit parses through
// mt942ParseFloorLimit (a zero limit is a limit, not a missing
// amount, the MT942 import precedent). Parsing ends with the same
// mt920Validate the export runs, so the parse gate and the apply
// gate share one check (the ApplyMT196 mt196Validate reuse).
//
// The block-3 Message User Reference must name the :20: the
// block 4 carries: the export renders `{108:}` as the :20:, so a
// document whose MUR disagrees with its own :20: is not the
// export's inverse and is refused. (The MT103/MT199 parsers only
// shape-check block 3; this module enforces the equality the
// spec pins down, because the :20: is the registration key and
// a MUR that names a different request would mis-key it.)
//
// Association is the ledger account, not a transfer: a sequence
// naming a :25: must resolve through mt940ResolveAccountLocked
// (internal ID, mt940-system external mapping, registered IBAN)
// to an account this ledger holds. An unknown account is refused
// fail-closed and parked in the repair queue (unknown_route),
// never invented. A sequence with no :25: registers as-is: an
// account-less request for a whole report needs no anchor.
//
// Registering a request moves nothing: no journal rows, no
// balance or payout change, no version bump — a request is
// evidence, not an instruction, and the audit event is the
// durable record, not a state change. No report is ever sent
// back automatically: the reply travels the existing GET
// /exports/mt940/941/942/950 routes, and the operator matches
// the request's :20: against the registry and the audit event.
// The request/reply pairing lives only in the registry and the
// audit events — the ledger never fabricates a :21: into a
// report, following the MT950 precedent (mt950.go).
//
// Idempotency is the :20: with its terms (the pain.014
// convention, along MT196/MT999): a replay with the same
// sequences in the same order returns the stored outcome with
// Duplicate set, registering nothing twice and emitting no
// second audit event. A :20: reused for different terms is
// refused with MT920MsgIDConflictError — never silently
// overwritten. The sender/receiver LTs are envelope, not terms,
// along the MT196 precedent: a replay under different LTs still
// returns the first registration, because the :20: names one
// request.

// ErrMT920UnknownAccount is returned by ApplyMT920 when a
// sequence's :25: names no ledger account: no internal account,
// no mt940-system external mapping, and no registered IBAN. The
// request is parked in the repair queue, not dropped. It is a
// 422-class error, along ErrMT940UnknownAccount.
var ErrMT920UnknownAccount = errors.New("ledger: mt920 inbound: account is unknown")

// MT920ApplyResult is the outcome of ApplyMT920: the registered
// request, verbatim. A parked request (unknown :25:) carries
// only Reference, Requested, Accounts, and RepairID, alongside
// the ErrMT920UnknownAccount error.
type MT920ApplyResult struct {
	// Reference echoes the request's :20: reference.
	Reference string `json:"reference"`
	// Requested is the :12: requested report type of each
	// sequence, in order.
	Requested []string `json:"requested"`
	// Accounts is the :25: account of each sequence that names
	// one, in order.
	Accounts []string `json:"accounts,omitempty"`
	// Sequences is the request's :12: / :25: / :34F: sequences,
	// verbatim, in order — the getter's view of what was
	// registered.
	Sequences []MT920Sequence `json:"sequences,omitempty"`
	// RepairID is the repair queue record for a parked request
	// (unknown :25:). Empty on a registered request.
	RepairID string `json:"repair_id,omitempty"`
	// Duplicate reports whether the request was already
	// registered under the same :20: with the same terms: a
	// no-op success, not an error.
	Duplicate bool `json:"duplicate,omitempty"`
}

// mt920ReceivedRecord is the registry record for one registered
// MT920 request, keyed by :20: in mt920Received: the message as
// received, when it was registered, and who registered it, so a
// replay returns the same outcome without registering twice.
type mt920ReceivedRecord struct {
	Message    MT920
	ReceivedAt time.Time
	Actor      string
	Result     MT920ApplyResult
}

// mt920ReceivedTermsEqual reports whether a stored request
// record carries exactly the terms an inbound request would
// apply: the same sequences in the same order — same :12:,
// same :25:, same :34F: limits. The sender/receiver LTs are
// envelope, not terms, along the MT196 precedent — a replay
// under different LTs still returns the first registration,
// because the :20: names one request.
func mt920ReceivedTermsEqual(rec mt920ReceivedRecord, m MT920) bool {
	return mt920TermsEqual(mt920ExportRecord{Message: rec.Message}, m)
}

// mt920SequencesCopy returns a deep copy of seqs, so a
// registered request's sequences can never be mutated through
// a returned result — the getter invariant, along the other
// registries.
func mt920SequencesCopy(seqs []MT920Sequence) []MT920Sequence {
	if seqs == nil {
		return nil
	}
	out := make([]MT920Sequence, len(seqs))
	for i, sq := range seqs {
		out[i] = sq
		if sq.FloorLimitDebit != nil {
			fl := *sq.FloorLimitDebit
			out[i].FloorLimitDebit = &fl
		}
		if sq.FloorLimitCredit != nil {
			fl := *sq.FloorLimitCredit
			out[i].FloorLimitCredit = &fl
		}
	}
	return out
}

// mt920ReceivedResultCopy returns a copy of res with its own
// requested, accounts, and sequences slices.
func mt920ReceivedResultCopy(res MT920ApplyResult) MT920ApplyResult {
	if res.Requested != nil {
		requested := make([]string, len(res.Requested))
		copy(requested, res.Requested)
		res.Requested = requested
	}
	if res.Accounts != nil {
		accounts := make([]string, len(res.Accounts))
		copy(accounts, res.Accounts)
		res.Accounts = accounts
	}
	res.Sequences = mt920SequencesCopy(res.Sequences)
	return res
}

// mt920ParseLT extracts the LT address from a block 1 or block 2
// header line and returns it exactly as written. Block 1 has the
// form `{1:F01<LT><10 session/sequence digits>}`; block 2 has the
// form `{2:I920<LT><5 header characters>}` (priority U, delivery
// monitoring 3, obsolescence period 003). The MT292 rule, under
// the MT920 error identity.
func mt920ParseLT(line, prefix string, tailLen int) (string, error) {
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, "}") {
		return "", fmt.Errorf("%w: %q", ErrInvalidMT920Export, line)
	}
	inner := line[len(prefix) : len(line)-1]
	if len(inner) <= tailLen {
		return "", fmt.Errorf("%w: %q", ErrInvalidMT920Export, line)
	}
	lt, tail := inner[:len(inner)-tailLen], inner[len(inner)-tailLen:]
	// Block 1's tail is the session/sequence digits (digit-checked);
	// block 2's tail is header flags (any 5 characters: U3003).
	if tailLen == 10 {
		for _, c := range tail {
			if c < '0' || c > '9' {
				return "", fmt.Errorf("%w: %q", ErrInvalidMT920Export, line)
			}
		}
	}
	if !mt202ValidLT(lt) {
		return "", fmt.Errorf("%w: %q", ErrInvalidMT920Export, lt)
	}
	return lt, nil
}

// mt920ParseFloorLimit parses one :34F: body through the MT942
// import's floor-limit parser and converts it to the MT920
// floor-limit shape: the D/C mark decides which side of the
// sequence the limit lands on. A zero limit is a limit, not a
// missing amount. The error is re-homed under the MT920
// sentinel: a bad :34F: in an MT920 document is an MT920 parse
// failure, never an MT942 one.
func mt920ParseFloorLimit(body string) (*MT942FloorLimit, error) {
	fl, err := mt942ParseFloorLimit(body)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidMT920Export, err)
	}
	return fl, nil
}

// ParseMT920 parses a SWIFT FIN MT920 message produced by
// ExportMT920 (or an equivalent counterparty message) into its
// business fields. Parsing is strict: the block structure, the
// field order, and every field value are validated, and anything
// outside the :20: / :12: / :25: / :34F: field set is refused —
// a request must not lose content quietly. CRLF line endings
// are accepted; a trailing newline after the `-}` trailer is
// tolerated. Sequences run in strict order :12: → :25: → :34F::
// a :25: may appear at most once per sequence and only before
// any :34F:, and each side's :34F: at most once per sequence —
// an unknown, repeated, or out-of-order field is a framing
// error. A :12: outside the four report types is refused, and
// a 942 sequence without a :34F: is refused; both are re-checked
// by the mt920Validate call at the end, so the parse gate and
// the apply gate share one check.
func ParseMT920(text string) (MT920, error) {
	var zero MT920
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	lines := strings.Split(text, "\n")
	if len(lines) < 7 {
		return zero, fmt.Errorf("%w: want at least 7 lines (3 headers + {4: + :20: + :12: + -}), got %d", ErrInvalidMT920Export, len(lines))
	}
	senderLT, err := mt920ParseLT(lines[0], "{1:F01", 10)
	if err != nil {
		return zero, err
	}
	receiverLT, err := mt920ParseLT(lines[1], "{2:I920", 5)
	if err != nil {
		return zero, err
	}
	// Block 3: the Message User Reference must name the :20:
	// the block 4 carries — the export renders {108:} as the
	// :20:, so a MUR that disagrees with it is not the
	// export's inverse.
	mur := lines[2]
	if !strings.HasPrefix(mur, "{3:{108:") || !strings.HasSuffix(mur, "}}") {
		return zero, fmt.Errorf("%w: malformed block 3: %q", ErrInvalidMT920Export, mur)
	}
	murRef := mur[len("{3:{108:") : len(mur)-2]
	if err := mt920Reference(murRef); err != nil {
		return zero, err
	}
	if lines[3] != "{4:" {
		return zero, fmt.Errorf("%w: want {4: on line 4, got %q", ErrInvalidMT920Export, lines[3])
	}
	if lines[len(lines)-1] != "-}" {
		return zero, fmt.Errorf("%w: want -} trailer, got %q", ErrInvalidMT920Export, lines[len(lines)-1])
	}
	body := lines[4 : len(lines)-1]
	if len(body) < 2 {
		return zero, fmt.Errorf("%w: want at least :20: and :12:, got %d block-4 lines", ErrInvalidMT920Export, len(body))
	}
	var m MT920
	m.SenderLT, m.ReceiverLT = senderLT, receiverLT
	if !strings.HasPrefix(body[0], ":20:") {
		return zero, fmt.Errorf("%w: block-4 line 1: want :20: field, got %q", ErrInvalidMT920Export, body[0])
	}
	ref := body[0][len(":20:"):]
	if ref == "" {
		return zero, fmt.Errorf("%w: :20: field is empty", ErrInvalidMT920Export)
	}
	if err := mt920Reference(ref); err != nil {
		return zero, err
	}
	if murRef != ref {
		return zero, fmt.Errorf("%w: {108:} %q does not match :20: %q", ErrInvalidMT920Export, murRef, ref)
	}
	m.TransactionReference = ref

	// Sequences: each starts with a :12:, followed by an
	// optional :25:, followed by zero or more :34F: lines —
	// at most one per side. A new :12: starts the next
	// sequence; anything else is a framing error.
	idx := 1
	for idx < len(body) {
		line := body[idx]
		if !strings.HasPrefix(line, ":12:") {
			return zero, fmt.Errorf("%w: block-4 line %d: want :12: field, got %q", ErrInvalidMT920Export, idx+5, line)
		}
		seq := MT920Sequence{RequestedType: line[len(":12:"):]}
		switch seq.RequestedType {
		case mt920RequestMT940, mt920RequestMT941, mt920RequestMT942, mt920RequestMT950:
		default:
			return zero, fmt.Errorf("%w: :12:: want 940, 941, 942, or 950, got %q", ErrInvalidMT920Export, seq.RequestedType)
		}
		idx++
		if idx < len(body) && strings.HasPrefix(body[idx], ":25:") {
			seq.Account = body[idx][len(":25:"):]
			idx++
		}
		for idx < len(body) && strings.HasPrefix(body[idx], ":34F:") {
			fl, err := mt920ParseFloorLimit(body[idx][len(":34F:"):])
			if err != nil {
				return zero, err
			}
			limit := &MT920FloorLimit{Currency: fl.Currency, AmountCents: fl.AmountCents}
			if fl.Credit {
				if seq.FloorLimitCredit != nil {
					return zero, fmt.Errorf("%w: duplicate :34F: credit floor limit in one sequence", ErrInvalidMT920Export)
				}
				seq.FloorLimitCredit = limit
			} else {
				if seq.FloorLimitDebit != nil {
					return zero, fmt.Errorf("%w: duplicate :34F: debit floor limit in one sequence", ErrInvalidMT920Export)
				}
				seq.FloorLimitDebit = limit
			}
			idx++
		}
		// A 942 sequence must carry a floor limit in the same
		// sequence — checked again by mt920Validate below; the
		// parse-time check names the sequence for the error.
		if seq.RequestedType == mt920RequestMT942 && seq.FloorLimitDebit == nil && seq.FloorLimitCredit == nil {
			return zero, fmt.Errorf("%w: :12: 942 requires a :34F: floor limit in the same sequence", ErrInvalidMT920Export)
		}
		m.Sequences = append(m.Sequences, seq)
	}
	// The parse gate and the apply gate share one check: every
	// field is re-validated through mt920Validate, so a
	// hand-built MT920 and a parsed one face the same rules.
	if _, _, err := mt920Validate(m); err != nil {
		return zero, err
	}
	return m, nil
}

// parkMT920Request parks a parsed MT920 request that failed
// closed, and returns the repair record ID. The snapshot names
// the unknown :25: account as the instruction ID and the
// request (:20:) as the destination, so the operator can find
// what is missing. The snapshot's Maker is deliberately empty
// (see the MT999 package comment): the record is operator
// visibility, never a resubmittable instruction. A parking
// failure (a full queue) is returned so the caller can fail loud
// rather than silently drop the bank's request.
func (l *Ledger) parkMT920Request(m MT920, unknownAccount, actor string) (string, error) {
	rec, err := l.EnqueueRepair(PayoutInstruction{
		ID:          unknownAccount,
		Destination: "mt920-inbound:" + m.TransactionReference,
		// Maker intentionally empty: see the MT999 package comment.
	}, RepairReasonUnknownRoute, actor)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// ApplyMT920 applies one parsed inbound SWIFT FIN MT920 report
// request: it registers the counterparty bank's request against
// the accounts its :25: names, and nothing else.
//
// A sequence naming a :25: must resolve through
// mt940ResolveAccountLocked to an account this ledger holds —
// otherwise the request is refused fail-closed and parked in
// the repair queue (unknown_route). The ledger never invents an
// account in order to register a request. A sequence with no
// :25: registers as-is: an account-less request for a whole
// report needs no anchor.
//
// Only evidence is recorded: no journal rows, no version bump,
// no money moved, no payout state changed. The audit event
// `mt920_request_received` (details msg_id / requested /
// account, trace :20:) is the durable record.
//
// Idempotency is the :20: with its terms (the pain.014
// convention): a replay with the same sequences returns the
// stored outcome with Duplicate set, registering nothing twice
// and emitting no second audit event; a :20: reused for
// different terms is refused with MT920MsgIDConflictError.
//
// Locking: the validation runs on the parsed values alone; the
// replay read and the :25: anchor reads run under the read lock;
// the :20: record and the inbound marker event run under a
// short write lock of their own, re-checking the registry first
// so a concurrent double-apply still records once; the repair
// parking runs outside any held lock because EnqueueRepair takes
// l.mu itself.
func (l *Ledger) ApplyMT920(m MT920, actor string) (MT920ApplyResult, error) {
	if actor == "" {
		actor = "ApplyMT920"
	}
	// The parse gate and the apply gate share one check.
	if _, _, err := mt920Validate(m); err != nil {
		return MT920ApplyResult{}, err
	}

	// :20: replay: the request was already registered under
	// this reference. Same terms is a no-op returning the
	// stored outcome; different terms is a conflict.
	l.mu.RLock()
	if rec, ok := l.mt920Received[m.TransactionReference]; ok {
		l.mu.RUnlock()
		if !mt920ReceivedTermsEqual(rec, m) {
			return MT920ApplyResult{}, &MT920MsgIDConflictError{Reference: m.TransactionReference}
		}
		dup := mt920ReceivedResultCopy(rec.Result)
		dup.Duplicate = true
		return dup, nil
	}
	l.mu.RUnlock()

	// The :25: anchor: every named account must resolve to a
	// ledger account. The first unknown account parks the
	// whole request fail-closed — a request naming an account
	// this ledger does not hold is not half-registered.
	var unknownAccount string
	l.mu.RLock()
	for _, seq := range m.Sequences {
		if seq.Account == "" {
			continue
		}
		if _, err := l.mt940ResolveAccountLocked(seq.Account); err != nil {
			unknownAccount = seq.Account
			break
		}
	}
	l.mu.RUnlock()
	if unknownAccount != "" {
		repairID, parkErr := l.parkMT920Request(m, unknownAccount, actor)
		if parkErr != nil {
			return MT920ApplyResult{}, fmt.Errorf("%w: %q (repair parking failed: %v)", ErrMT920UnknownAccount, unknownAccount, parkErr)
		}
		requested, accounts := mt920ApplySummaries(m)
		return MT920ApplyResult{
			Reference: m.TransactionReference,
			Requested: requested,
			Accounts:  accounts,
			RepairID:  repairID,
		}, fmt.Errorf("%w: %q", ErrMT920UnknownAccount, unknownAccount)
	}

	requested, accounts := mt920ApplySummaries(m)
	res := MT920ApplyResult{
		Reference: m.TransactionReference,
		Requested: requested,
		Accounts:  accounts,
		Sequences: mt920SequencesCopy(m.Sequences),
	}

	// The :20: record and the inbound marker event, under one
	// short write lock: the request is already built when the
	// marker lands. A concurrent double-apply that landed
	// first returns the stored outcome instead of a second
	// marker. The audit event carries no accounts: the :25:
	// values name the accounts the report is wanted for, and
	// they ride in the event details instead, along the
	// mt920_request_exported shape.
	l.mu.Lock()
	if rec, ok := l.mt920Received[m.TransactionReference]; ok {
		l.mu.Unlock()
		if !mt920ReceivedTermsEqual(rec, m) {
			return MT920ApplyResult{}, &MT920MsgIDConflictError{Reference: m.TransactionReference}
		}
		dup := mt920ReceivedResultCopy(rec.Result)
		dup.Duplicate = true
		return dup, nil
	}
	if l.mt920Received == nil {
		l.mt920Received = make(map[string]mt920ReceivedRecord)
	}
	l.mt920Received[m.TransactionReference] = mt920ReceivedRecord{
		Message: MT920{
			SenderLT:             m.SenderLT,
			ReceiverLT:           m.ReceiverLT,
			TransactionReference: m.TransactionReference,
			Sequences:            mt920SequencesCopy(m.Sequences),
		},
		ReceivedAt: time.Now().UTC(),
		Actor:      actor,
		Result:     mt920ReceivedResultCopy(res),
	}
	l.emitAudit(AuditEvent{
		Op:            "mt920_request_received",
		Actor:         actor,
		TraceID:       m.TransactionReference,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"msg_id":    m.TransactionReference,
			"requested": requested,
			"account":   accounts,
		},
	})
	l.mu.Unlock()
	return res, nil
}

// mt920ApplySummaries builds the :12: and :25: summaries of m,
// in sequence order, for the apply result and the audit event.
func mt920ApplySummaries(m MT920) (requested, accounts []string) {
	requested = make([]string, 0, len(m.Sequences))
	accounts = make([]string, 0, len(m.Sequences))
	for _, seq := range m.Sequences {
		requested = append(requested, seq.RequestedType)
		if seq.Account != "" {
			accounts = append(accounts, seq.Account)
		}
	}
	return requested, accounts
}

// GetMT920Received returns the registered request for msgID (its
// :20:), or false when no request was ever registered under it.
// The returned result is a copy — callers cannot mutate ledger
// state through it.
func (l *Ledger) GetMT920Received(msgID string) (MT920ApplyResult, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	rec, ok := l.mt920Received[msgID]
	if !ok {
		return MT920ApplyResult{}, false
	}
	return mt920ReceivedResultCopy(rec.Result), true
}
