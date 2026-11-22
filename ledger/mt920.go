package ledger

import (
	"errors"
	"fmt"
	"strings"
)

// SWIFT FIN MT920 request message export (LG-459, fintech: SWIFT
// 报表索要请求).
//
// An MT920 is the request half of the statement lifecycle: the
// ledger has imported MT940/MT941/MT942/MT950 reports for years,
// but it never asked the account-servicing bank for one — the MT920
// is that ask. It names which report is wanted (:12:) and for which
// account (:25:), optionally with a floor limit (:34F:) that leaves
// small entries out of the report that comes back. It moves no
// money: a request is a question, and the answer is another message
// (MT940/MT941/MT942/MT950 import, see mt940_import.go and its
// siblings).
//
// Field set, verified against two independent sources before this
// module was written (2026-11-22):
//
//   - Payments Signal, "MT920: asking the bank for a report now"
//     and "MT920 — message reference": only four reports can be
//     asked for — MT940, MT941, MT942, MT950 — named in :12:; :25:
//     is the account the report is wanted for; :20: is the
//     request's own reference, quoted back in :21: of the report
//     (MT950 has no :21: field; retain the request context in
//     other records); a request for a 942 must carry a :34F: in the
//     same sequence, and a request for a balance report needs no
//     floor limit.
//   - docs.rs, swift-mt-message, MT920Sequence: a repetitive
//     sequence of {field_12, field_25, floor_limit_debit:
//     Option<Field34F>, floor_limit_credit: Option<Field34F>} —
//     one request can ask for several reports across several
//     accounts, with a debit and a credit floor limit each.
//
// The :34F: floor limit indicator renders as currency + D/C mark +
// amount (3!a15d), the in-repo MT942 precedent
// (mt942FloorLimitLine / mt942ParseFloorLimit): the mark carries
// the side the floor applies to, the amount is absolute with a
// comma decimal separator (mt940Amount). A floor limit of zero is
// a limit, not a missing amount (the MT942 import precedent); a
// negative amount is refused, and the amount is capped so the
// rendered 15d field never exceeds 15 digits.
//
// The 942 rule is enforced per sequence: a sequence asking for a
// 942 must carry at least one :34F: (debit or credit — one
// satisfies the rule; both are allowed). A floor limit on a
// non-942 sequence is permitted, not refused: the field is
// optional in the sequence, and the rule mandates it for 942
// without forbidding it elsewhere.
//
// Framing follows the MT103/MT199 convention: block 1 (basic
// header, sender LT, session/sequence zero-filled and
// interface-assigned), block 2 (application header, receiver LT,
// `I920`), block 3 (`{108:MUR}` carrying the :20: reference),
// block 4 (the :20: reference, then one :12: / :25: / :34F:
// sequence per requested report, in canonical order), no block 5
// (CHK is computed by the SWIFT network on receipt). Lines are
// joined with "\n", the nacha.go convention.
//
// The pure value object is separate from the ledger-bound export
// below. The value is pure: no ledger lock, no version bump, no
// journal rows, no audit event, and export is deterministic per
// message (repeat exports render byte-identical output). Sending
// an MT920 changes nothing on the ledger — that is the point of a
// request, and the reason it must never carry an amount of its
// own: the :34F: amounts are filters on the report that comes
// back, not instructions to move value. The ledger-bound method
// adds only evidence: a :20: registry entry (the idempotency
// guard) and one `mt920_request_exported` audit event, the durable
// record. No journal entry, no balance or status change, no
// version bump.
//
// The :20: is the MsgId / idempotency key (the pain.014
// convention, along MT196/MT999): re-exporting the same :20: with
// the same sequences returns the stored rendered message with
// Duplicate set, rendering nothing twice and emitting no second
// audit event. A :20: reused for different sequences is refused
// with MT920MsgIDConflictError — never silently overwritten. The
// sender/receiver LTs are envelope, not terms, along the MT196
// precedent: a replay under different LTs still returns the first
// rendering, because the :20: names one export.
//
// The request/reply pairing: the report that comes back quotes
// this request's :20: in its :21: (MT940/MT941/MT942). MT950 has
// no :21: field, so the pairing for a 950 request lives only in
// the `mt920_request_exported` audit event (details msg_id /
// requested / account) and the operator's own records — the
// ledger never fabricates a :21: into an MT950.
//
// Error discipline: this module keeps a single sentinel,
// ErrInvalidMT920Export (the MT940/MT942 single-sentinel shape,
// per the backlog item — no new sentinels). The :20: conflict is
// a typed error, MT920MsgIDConflictError, wrapping that sentinel
// so the HTTP layer can map it to 422 with
// IsMT920MsgIDConflict while every other failure stays a 400.

// ErrInvalidMT920Export is returned when an MT920 request cannot
// be rendered: an invalid LT address, :20: reference, :12:
// requested type, :25: account, or :34F: floor limit, an empty
// sequence list, or a 942 sequence without a floor limit. It is
// the module's only sentinel; the :20: conflict is the typed
// MT920MsgIDConflictError below, which wraps this sentinel.
var ErrInvalidMT920Export = errors.New("ledger: invalid MT920 export")

// MT920MsgIDConflictError is returned by the ledger-bound
// ExportMT920 when a :20: reference already exported carries
// different terms: the reference names one export, and a second,
// different export under it is refused rather than silently
// overwritten (the pain.014 convention, along MT196/MT999). It
// wraps ErrInvalidMT920Export so errors.Is still reports the
// module sentinel; callers that need the 422 mapping use
// IsMT920MsgIDConflict.
type MT920MsgIDConflictError struct {
	// Reference is the :20: already exported with different terms.
	Reference string
}

func (e *MT920MsgIDConflictError) Error() string {
	return fmt.Sprintf("ledger: invalid MT920 export: :20: %q already exported with different terms", e.Reference)
}

func (e *MT920MsgIDConflictError) Unwrap() error { return ErrInvalidMT920Export }

// IsMT920MsgIDConflict reports whether err is an
// MT920MsgIDConflictError.
func IsMT920MsgIDConflict(err error) bool {
	var conflict *MT920MsgIDConflictError
	return errors.As(err, &conflict)
}

// MT920 requested report types: the only four reports an MT920
// may ask for.
const (
	mt920RequestMT940 = "940"
	mt920RequestMT941 = "941"
	mt920RequestMT942 = "942"
	mt920RequestMT950 = "950"
)

// mt920MaxFloorLimitCents caps a :34F: amount so the rendered
// 15d field never exceeds 15 digits: cents below 10^15 render an
// integer part of at most 13 digits plus two fraction digits.
const mt920MaxFloorLimitCents = int64(1_000_000_000_000_000)

// MT920FloorLimit is one :34F: floor limit indicator: the
// currency (3!a, mandatory — unlike the MT942 export, an MT920
// carries no report currency to fall back on) and the limit
// amount, in cents. The D/C side is not a field here: a debit
// floor limit and a credit floor limit are separate :34F:
// occurrences in the sequence (see MT920Sequence), each rendered
// with its own mark.
type MT920FloorLimit struct {
	Currency    string
	AmountCents int64
}

// MT920Sequence is one requested report: the :12: message type,
// the optional :25: account, and the optional :34F: floor limits
// — at most one debit and one credit limit per sequence. A
// sequence asking for a 942 must carry at least one floor limit.
type MT920Sequence struct {
	RequestedType    string           // :12:, "940" | "941" | "942" | "950".
	Account          string           // :25:, 35x, optional.
	FloorLimitDebit  *MT920FloorLimit // :34F: with a D mark, optional.
	FloorLimitCredit *MT920FloorLimit // :34F: with a C mark, optional.
}

// MT920 is one SWIFT FIN MT920 request message: the block-4 field
// set plus the block 1/2 LT addresses. Sequences is the repeating
// :12: / :25: / :34F: sequence, one entry per requested report —
// at least one. The struct deliberately has no amount, currency,
// or value-date field of its own: an MT920 asks for a report, and
// the :34F: amounts are filters on the report that comes back,
// not value the ledger moves.
type MT920 struct {
	SenderLT             string
	ReceiverLT           string
	TransactionReference string
	Sequences            []MT920Sequence
}

// mt920Reference validates a :20: reference: 1-16 SWIFT `x`
// characters, no braces, no line breaks (a single-line field must
// not smuggle a second line into block 4), no leading/trailing
// colon or slash, no double slash. The MT196 rule, under the
// MT920 error identity.
func mt920Reference(ref string) error {
	if ref == "" || len(ref) > mt103MaxSenderReference {
		return fmt.Errorf("%w: :20:: want 1-%d characters, got %q", ErrInvalidMT920Export, mt103MaxSenderReference, ref)
	}
	if strings.ContainsAny(ref, "\r\n") {
		return fmt.Errorf("%w: :20:: line break in a single-line field: %q", ErrInvalidMT920Export, ref)
	}
	if err := mt103CheckX(ref); err != nil {
		return fmt.Errorf("%w: :20:: %w", ErrInvalidMT920Export, err)
	}
	if strings.HasPrefix(ref, ":") || strings.HasPrefix(ref, "/") ||
		strings.HasSuffix(ref, ":") || strings.HasSuffix(ref, "/") ||
		strings.Contains(ref, "//") {
		return fmt.Errorf("%w: :20:: leading/trailing colon or slash, or double slash: %q", ErrInvalidMT920Export, ref)
	}
	return nil
}

// mt920NormalizeLT normalizes a sender/receiver LT address with
// the MT103 rule (BIC8 -> BIC8+"XXX", BIC11 -> BIC11+"X", BIC12
// as-is), reported under the MT920 error identity.
func mt920NormalizeLT(raw string) (string, error) {
	lt, err := mt103NormalizeLT(raw)
	if err != nil {
		return "", fmt.Errorf("%w: %q", ErrInvalidMT920Export, raw)
	}
	return lt, nil
}

// mt920Account validates one :25: account identification: empty
// is allowed (the field is optional), otherwise 1-35 SWIFT `x`
// characters with no line breaks. A slash is legal here —
// account numbers carry them — so only the charset and the
// length are enforced, never the MT196 reference punctuation
// rules.
func mt920Account(account string) error {
	if account == "" {
		return nil
	}
	if len(account) > 35 {
		return fmt.Errorf("%w: :25:: want at most 35 characters, got %d", ErrInvalidMT920Export, len(account))
	}
	if strings.ContainsAny(account, "\r\n") {
		return fmt.Errorf("%w: :25:: line break in a single-line field: %q", ErrInvalidMT920Export, account)
	}
	if err := mt103CheckX(account); err != nil {
		return fmt.Errorf("%w: :25:: %w", ErrInvalidMT920Export, err)
	}
	return nil
}

// mt920FloorLimit validates one :34F: floor limit: an explicit
// 3-letter currency (no default — an MT920 carries no report
// currency to fall back on, unlike the MT942 export) and a
// non-negative amount that fits the 15d field. A zero amount is a
// limit, not a missing amount (the MT942 import precedent).
func mt920FloorLimit(fl *MT920FloorLimit, tag string) error {
	if fl == nil {
		return nil
	}
	if fl.Currency == "" {
		return fmt.Errorf("%w: %s: currency is required", ErrInvalidMT920Export, tag)
	}
	if _, err := normalizeCurrency(fl.Currency); err != nil {
		return fmt.Errorf("%w: %s: bad currency %q", ErrInvalidMT920Export, tag, fl.Currency)
	}
	if fl.AmountCents < 0 {
		return fmt.Errorf("%w: %s: negative amount", ErrInvalidMT920Export, tag)
	}
	if fl.AmountCents >= mt920MaxFloorLimitCents {
		return fmt.Errorf("%w: %s: amount exceeds the 15d field", ErrInvalidMT920Export, tag)
	}
	return nil
}

// mt920FloorLimitLine renders one :34F: floor limit indicator:
// currency + D/C mark + amount, no separators — the same compact
// form as the MT942 :34F: (mt942FloorLimitLine).
func mt920FloorLimitLine(fl *MT920FloorLimit, credit bool) string {
	return fmt.Sprintf(":34F:%s%s%s", fl.Currency, mt940DC(credit), mt940Amount(fl.AmountCents))
}

// mt920Validate checks every field of m, in field order, and
// returns the normalized sender/receiver LT addresses for
// rendering.
func mt920Validate(m MT920) (senderLT, receiverLT string, err error) {
	senderLT, err = mt920NormalizeLT(m.SenderLT)
	if err != nil {
		return "", "", fmt.Errorf("sender: %w", err)
	}
	receiverLT, err = mt920NormalizeLT(m.ReceiverLT)
	if err != nil {
		return "", "", fmt.Errorf("receiver: %w", err)
	}
	if err := mt920Reference(m.TransactionReference); err != nil {
		return "", "", err
	}
	if len(m.Sequences) == 0 {
		return "", "", fmt.Errorf("%w: at least one :12: sequence is required", ErrInvalidMT920Export)
	}
	for i, seq := range m.Sequences {
		tag := fmt.Sprintf("sequence %d", i+1)
		switch seq.RequestedType {
		case mt920RequestMT940, mt920RequestMT941, mt920RequestMT942, mt920RequestMT950:
		default:
			return "", "", fmt.Errorf("%w: %s: :12:: want 940, 941, 942, or 950, got %q", ErrInvalidMT920Export, tag, seq.RequestedType)
		}
		if err := mt920Account(seq.Account); err != nil {
			return "", "", fmt.Errorf("%s: %w", tag, err)
		}
		if err := mt920FloorLimit(seq.FloorLimitDebit, tag+" :34F: debit"); err != nil {
			return "", "", err
		}
		if err := mt920FloorLimit(seq.FloorLimitCredit, tag+" :34F: credit"); err != nil {
			return "", "", err
		}
		// The 942 rule: a request for an interim report must
		// carry a floor limit in the same sequence — one
		// satisfies it, both are allowed, none is refused.
		if seq.RequestedType == mt920RequestMT942 && seq.FloorLimitDebit == nil && seq.FloorLimitCredit == nil {
			return "", "", fmt.Errorf("%w: %s: :12: 942 requires a :34F: floor limit in the same sequence", ErrInvalidMT920Export, tag)
		}
	}
	return senderLT, receiverLT, nil
}

// ExportMT920 renders m as a SWIFT FIN MT920 message: blocks 1-4,
// lines joined with "\n", no trailing newline (the nacha.go
// convention). The export is deterministic per message: repeat
// exports render byte-identical output.
func ExportMT920(m MT920) (string, error) {
	senderLT, receiverLT, err := mt920Validate(m)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	// Block 1: session/sequence zero-filled — assigned by the
	// sender's SWIFT interface on transmission (MT103 convention).
	fmt.Fprintf(&b, "{1:F01%s0000000000}\n", senderLT)
	// Block 2: application header, input to SWIFT, I920. Priority
	// U, delivery monitoring 3, obsolescence period 003 — the MT103
	// application header with the message type swapped.
	fmt.Fprintf(&b, "{2:I920%sU3003}\n", receiverLT)
	// Block 3: the Message User Reference carries the :20:
	// reference, the MT103 convention.
	fmt.Fprintf(&b, "{3:{108:%s}}\n", m.TransactionReference)
	// Block 4: the :20: reference, then one :12: / :25: / :34F:
	// sequence per requested report, in canonical order.
	b.WriteString("{4:\n")
	fmt.Fprintf(&b, ":20:%s\n", m.TransactionReference)
	for _, seq := range m.Sequences {
		fmt.Fprintf(&b, ":12:%s\n", seq.RequestedType)
		if seq.Account != "" {
			fmt.Fprintf(&b, ":25:%s\n", seq.Account)
		}
		if seq.FloorLimitDebit != nil {
			fmt.Fprintf(&b, "%s\n", mt920FloorLimitLine(seq.FloorLimitDebit, false))
		}
		if seq.FloorLimitCredit != nil {
			fmt.Fprintf(&b, "%s\n", mt920FloorLimitLine(seq.FloorLimitCredit, true))
		}
	}
	b.WriteString("-}")
	return b.String(), nil
}

// MT920ExportResult is the recorded outcome of one MT920 export:
// the rendered message and its references. Duplicate reports
// whether the export was already rendered under the same :20: with
// the same terms: a no-op success, not an error.
type MT920ExportResult struct {
	// Reference is this export's own :20: transaction reference.
	Reference string `json:"reference"`
	// Requested is the :12: requested report type of each
	// sequence, in order.
	Requested []string `json:"requested"`
	// Accounts is the :25: account of each sequence that names
	// one, in order.
	Accounts []string `json:"accounts,omitempty"`
	// Message is the rendered SWIFT FIN MT920 text.
	Message string `json:"message"`
	// Duplicate reports whether the export was already rendered
	// under the same :20: with the same terms: a no-op success,
	// not an error.
	Duplicate bool `json:"duplicate,omitempty"`
}

// mt920ExportRecord is the registry record for one rendered MT920
// request, keyed by :20: in mt920Exports: the message it was
// rendered from and the result, so a replay returns the same
// message without rendering twice.
type mt920ExportRecord struct {
	Message MT920
	Result  MT920ExportResult
}

// mt920FloorLimitEqual reports whether two :34F: floor limits
// carry the same terms, treating two nil limits as equal.
func mt920FloorLimitEqual(a, b *MT920FloorLimit) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Currency == b.Currency && a.AmountCents == b.AmountCents
}

// mt920TermsEqual reports whether a stored export record carries
// exactly the terms an export request would render: the same
// sequences in the same order — same :12:, same :25:, same
// :34F: limits. The sender/receiver LTs are envelope, not terms,
// along the MT196 precedent — a replay under different LTs still
// returns the first rendering, because the :20: names one export.
func mt920TermsEqual(rec mt920ExportRecord, m MT920) bool {
	if len(rec.Message.Sequences) != len(m.Sequences) {
		return false
	}
	for i, want := range m.Sequences {
		got := rec.Message.Sequences[i]
		if got.RequestedType != want.RequestedType || got.Account != want.Account ||
			!mt920FloorLimitEqual(got.FloorLimitDebit, want.FloorLimitDebit) ||
			!mt920FloorLimitEqual(got.FloorLimitCredit, want.FloorLimitCredit) {
			return false
		}
	}
	return true
}

// ExportMT920 renders m as a SWIFT FIN MT920 request message and
// records the export as evidence: the :20: registry entry and one
// `mt920_request_exported` audit event (details msg_id /
// requested / account, trace :20:). Nothing moves: no journal
// entry, no balance or status change, no version bump — asking
// for a report is evidence-only, and the audit event is the
// durable record, not a state change. The audit event carries no
// accounts: the :25: values name accounts at the servicing bank,
// not ledger accounts, and attributing the evidence to a ledger
// account would misstate it; the requested accounts ride in the
// event details instead.
//
// Idempotency is the :20: with its terms (the pain.014
// convention, along MT196/MT999): a replay with the same
// sequences returns the stored rendered message with Duplicate
// set, rendering nothing twice and emitting no second audit
// event; a :20: reused for different terms is refused with
// MT920MsgIDConflictError.
//
// Locking: the validation runs on the parsed values alone; the
// replay read runs under the read lock; the :20: record and the
// export marker event run under a short write lock of their own,
// re-checking the registry first so a concurrent double-export
// still records once.
//
// (This is the Ledger method of the same name as the package-level
// ExportMT920 above: the unqualified call below is the pure
// renderer, not recursion — methods are not bare identifiers.)
func (l *Ledger) ExportMT920(m MT920, actor string) (MT920ExportResult, error) {
	if actor == "" {
		actor = "ExportMT920"
	}
	if _, _, err := mt920Validate(m); err != nil {
		return MT920ExportResult{}, err
	}

	// :20: replay: the request was already rendered under this
	// reference. Same terms is a no-op returning the stored
	// message; different terms is a conflict.
	l.mu.RLock()
	if rec, ok := l.mt920Exports[m.TransactionReference]; ok {
		l.mu.RUnlock()
		if !mt920TermsEqual(rec, m) {
			return MT920ExportResult{}, &MT920MsgIDConflictError{Reference: m.TransactionReference}
		}
		dup := rec.Result
		dup.Duplicate = true
		return dup, nil
	}
	l.mu.RUnlock()

	msg, err := ExportMT920(m)
	if err != nil {
		return MT920ExportResult{}, err
	}
	requested := make([]string, 0, len(m.Sequences))
	accounts := make([]string, 0, len(m.Sequences))
	for _, seq := range m.Sequences {
		requested = append(requested, seq.RequestedType)
		if seq.Account != "" {
			accounts = append(accounts, seq.Account)
		}
	}
	res := MT920ExportResult{
		Reference: m.TransactionReference,
		Requested: requested,
		Accounts:  accounts,
		Message:   msg,
	}

	// The :20: record and the export marker event, under one short
	// write lock: the message is already rendered when the marker
	// lands. A concurrent double-export that landed first returns
	// the stored outcome instead of a second marker. The details
	// carry the requested report types and accounts verbatim, in
	// sequence order — the only structured link to the report
	// that comes back, which quotes this :20: in its :21: (except
	// MT950, which has no :21: field).
	l.mu.Lock()
	if rec, ok := l.mt920Exports[m.TransactionReference]; ok {
		l.mu.Unlock()
		if !mt920TermsEqual(rec, m) {
			return MT920ExportResult{}, &MT920MsgIDConflictError{Reference: m.TransactionReference}
		}
		dup := rec.Result
		dup.Duplicate = true
		return dup, nil
	}
	if l.mt920Exports == nil {
		l.mt920Exports = make(map[string]mt920ExportRecord)
	}
	l.mt920Exports[m.TransactionReference] = mt920ExportRecord{Message: m, Result: res}
	l.emitAudit(AuditEvent{
		Op:            "mt920_request_exported",
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

// GetMT920Export returns the recorded export for msgID, or false
// when no request was ever rendered under it. The returned record
// is a copy — callers cannot mutate ledger state through it.
func (l *Ledger) GetMT920Export(msgID string) (MT920ExportResult, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	rec, ok := l.mt920Exports[msgID]
	if !ok {
		return MT920ExportResult{}, false
	}
	return rec.Result, true
}
