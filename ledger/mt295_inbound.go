package ledger

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// SWIFT FIN MT295 query inbound registration (LG-466): the
// inbound half of the Category 2 query leg. A correspondent bank
// asks this ledger about an earlier Category 2 message — an MT210
// notification it sent, an MT292 cancellation request it sent, or
// an MT202 transfer it sent — and ApplyMT295 registers the query
// against what its :21: names. The answer is a later MT296 export
// (LG-467): this module only registers the query, so the
// question-answer chain can be matched when the answer comes.
//
// Field set, verified against the ISO 15022 MT n95 (common
// query) table before this module was written:
//
//   - Swift Standards MT, November 2023 message reference guide,
//     Category n, MT n95 Queries — read from the ISO 15022 User
//     Handbook copy on iso20022.org (re-confirmed 2026-10-11, as
//     reported by Payments Signal's MT195/MT295 topic): the
//     seven-row format — fields 20, 21 and 75 mandatory; 77A,
//     11a and 79 optional; the untagged copy conditional under
//     network-validated rule C1 (field 79 or the copy of the
//     original's mandatory fields, but not both). The Category 2
//     volume carries no field list for MT 295 and points here.
//   - Prowide MT295.java at SRU 2025 (open source, Apache 2.0):
//     the six tagged body fields 20 (M), 21 (M), 75 (M), 77A
//     (O), 11 as R or S (O), 79 (O); the untagged copy cannot be
//     modelled by a field class.
//
// The :75: mandatory/optional question this module had to settle
// — the backlog draft flagged it — is answered by both sources
// in the same direction: :75: is mandatory. The in-repo MT195
// precedent (mt195_inbound.go) treats :75: as optional and
// requires at least one of :75: / :79:; that call is not followed
// here, because the handbook is explicit for the n95 table and
// a query that carries no question text is not a query. A
// correspondent that sends only :79: has sent a description
// with no question, and it is refused.
//
// The MT295 core set here is :20: / :21: / :75: / :77A: / :79:,
// in that canonical order:
//
//   - :20: Transaction Reference Number 16x M — the query's own
//     reference, the replay key.
//   - :21: Related Reference 16x M — the queried message's :20:,
//     the anchor (see below). Mandatory: a query with no :21:
//     names nothing to ask about, and is refused.
//   - :75: Queries 6*35x M — the coded question. The codewords
//     ride verbatim and are never interpreted — STAT, DUPL,
//     PRTY and any other text are the querier's words, read by
//     an operator, not a code table the ledger acts on (the
//     MT195 precedent: codewords ride verbatim, never
//     interpreted).
//   - :77A: Narrative 20*35x O — the operator note, registered
//     verbatim (the MT296 :77A: precedent; the MT195 module
//     refuses :77A: because there it would carry a priority
//     change, a different action from a status query — no such
//     reading applies to a Category 2 query, where :77A: is a
//     plain narrative).
//   - :79: Narrative 50*35x O — the free-format description of
//     the query, registered verbatim. Rule C1's other branch —
//     an untagged copy of the original message's mandatory
//     fields — is refused, never silently dropped: copy-form
//     content surfaces as out-of-order or unknown tagged lines
//     and fails the parse, along the MT195/MT196 rule. Field
//     79's /REJT/ and /RETN/ markers, where present, ride as
//     text inside the narrative and are never interpreted.
//
// :11S: / :11R: (MT and date of the original message) is refused
// as an unknown field, along the MT195 :11S: and MT196/MT296
// :11a: precedents — even though the handbook lists 11a as
// optional. The queried message is already named by the :21:,
// and a second, possibly disagreeing copy of its type and date
// would be a different claim — refused, never silently dropped.
// The ledger's core set is deliberately narrower than the
// handbook's here, the same call the MT195, MT196 and MT296
// modules made.
//
// The :21: anchors in six tiers, in order — the query asks
// about a message this ledger holds, so the anchor is a lookup
// in the ledger's own registries (the MT999 two-tier shape,
// extended to five; the MT195 "look the :21: up in the ledger"
// shape, not the MT296 "shape of an outbound :20:" shape,
// because a query names what we hold and an answer names what
// we sent). The three inbound tiers come first, so a :21: that
// names both an inbound record and an outbound export anchors
// to the inbound one — the order is fixed so the kind is
// deterministic (along the MT296 export's "MT292 tier
// consulted first" precedent, mt296_export.go):
//
//  1. An MT210 notification: the :21: is an expectedReceipts
//     MsgID (the notification's :20:, itself a schedule ID —
//     mt210_inbound.go).
//  2. An inbound MT292 cancellation request: the :21: is an
//     mt292Applied :20: (mt292_inbound.go, LG-452).
//  3. An inbound MT202 transfer: the :21: is an mt202Applied
//     :20: (mt202_inbound.go, LG-442).
//  4. An outbound MT292 cancellation request (LG-474): the
//     :21: has the outbound MT292 :20: shape (`MT292-` + 10
//     lowercase hex, mt292MessageReference) and is registered
//     in mt292Exports (mt292_export.go, LG-471). The shape
//     alone is not enough — a shape naming no rendered MT292
//     still parks, because a cancellation request this ledger
//     never rendered cannot be asked about.
//  5. An outbound MT202 transfer (LG-474): the :21: is
//     registered in mt202Exports (mt202_export.go, LG-470).
//     An outbound MT202 :20: is caller-supplied, 1-16 `x`
//     characters with no shape to check, so the lookup alone
//     anchors it (along the MT296 third tier, LG-472).
//  6. An outbound MT296 answer (LG-478): the :21: is
//     registered in mt296Exports (mt296_export.go, LG-467).
//     An outbound MT296 :20: is operator-supplied, 1-16 `x`
//     characters with no shape to check, so the lookup alone
//     anchors it (along the MT202 tier, LG-474, and the MT296
//     third tier, LG-472).
//
// A :21: that matches no tier is fail-closed into the repair
// queue (RepairReasonUnknownRoute, the camt.055 shape, along
// ApplyMT999 and ApplyMT292: the returned result carries the
// repair ID alongside the ErrMT295UnknownReference error), and
// reported as a 422-class error. The outbound tiers close the
// asymmetry the MT296 module once had: a :21: naming our
// outbound MT292's :20:, our outbound MT202's :20:, or our
// outbound MT296's :20: now anchors against the outbound
// registries (the answer side still answers registered
// inbound queries, not our own exports, mt296_export.go). There is no
// :21:→schedule reverse lookup: the SHA-256 behind the
// outbound MT292 :20: is irreversible, so the operator matches
// the query to the export from the :21: and the audit event.
// The registries are in-memory only, like the exports they
// record: a restart clears them, and a late query arriving
// after a restart parks exactly as it did before the outbound
// tiers existed — recovery is re-export (GET /exports/mt292
// deterministically re-derives the same :20:; POST
// /exports/mt202 with the same :20: and terms replays
// Duplicate) and then re-apply the same query (parking holds
// no :20:, so a re-apply is not a replay).
//
// The query is registered verbatim: the :75: question, the :77A:
// narrative, and the :79: description ride into the record
// exactly as received, never re-derived from live facts, because
// the query is the correspondent's question, not the ledger's
// own view of it. A later change to the queried object's status
// never rewrites a registered query; a replay returns the stored
// verbatim query. The :75: / :77A: / :79: texts are retrievable
// through the getter — they live in the registry record, not in
// the audit event details.
//
// No answer snapshot is built here, unlike MT195: a Category 1
// query asks about one payout, whose status is a single fact
// worth snapshotting; a Category 2 query asks about a
// notification, a cancellation request, or a transfer — three
// kinds of object, whose answer is organised per queried object
// by the later MT296 export (LG-467). This module registers the
// query itself, and nothing else.
//
// Nothing moves: no journal entry, no schedule status change, no
// ExpectedReceipt cancellation, no version bump — registering a
// query is read-only, and the audit event is the durable record,
// not a state change. The query's audit event,
// `mt295_query_received` (details msg_id / related_reference /
// sender_lt, trace :20:), puts the :20: on the durable record,
// along the mt999_received shape: no accounts, because a query
// touches no ledger account.
//
// Idempotency is the :20: with its terms (the pain.014
// convention, LG-416): a replay with the same :21:, the same
// :75: queries, the same :77A: narrative, and the same :79:
// description returns the stored outcome with Duplicate set,
// registering nothing twice and emitting no second audit event.
// A :20: reused for different terms is refused with
// ErrMT295MsgIDConflict — never silently overwritten. The
// registry check runs again under the write lock after the
// anchor lookup, so two concurrent applies of the same query
// still record once. A parked query records nothing: a re-sent
// query parks again — or registers, once the queried message
// exists (the MT195 parked-query rule).

// MT295 parse/apply errors.
var (
	// ErrMT295Framing is returned when the FIN block structure
	// is broken: a missing block, a malformed block 1/2/3
	// header, a missing or unterminated block 4, a block-3
	// MUR that does not name the :20:, or a field out of the
	// canonical :20: -> :21: -> :75: -> :77A: -> :79: order.
	ErrMT295Framing = errors.New("ledger: mt295: malformed FIN framing")
	// ErrMT295Field is returned for a block-4 field problem
	// that is not a value problem: a missing required field
	// (:20:, :21:, or :75:), a duplicate field, or a field
	// outside the five-field core set.
	ErrMT295Field = errors.New("ledger: mt295: unexpected or missing field")
	// ErrMT295Reference is returned for an invalid :20: or :21:
	// reference: empty, over 16 characters, outside the SWIFT
	// `x` character set, carrying a brace or a line break, or
	// with a leading/trailing colon or slash, or a double slash.
	ErrMT295Reference = errors.New("ledger: mt295: invalid reference")
	// ErrMT295LT is returned for an invalid sender or receiver
	// LT address (not a valid BIC8/BIC11/BIC12 form).
	ErrMT295LT = errors.New("ledger: mt295: invalid SWIFT LT address")
	// ErrMT295Queries is returned for an invalid :75: query
	// set: empty, over 6 lines, or a line that is empty, over
	// 35 characters, or outside the SWIFT `x` character set
	// (braces and line breaks refused).
	ErrMT295Queries = errors.New("ledger: mt295: invalid :75: queries")
	// ErrMT295Narrative is returned for an invalid :77A:
	// narrative: over 20 lines, or a line that is empty, over
	// 35 characters, or outside the SWIFT `x` character set
	// (braces and line breaks refused).
	ErrMT295Narrative = errors.New("ledger: mt295: invalid :77A: narrative")
	// ErrMT295Description is returned for an invalid :79:
	// description: over 50 lines, or a line that is empty,
	// over 35 characters, or outside the SWIFT `x` character
	// set (braces and line breaks refused).
	ErrMT295Description = errors.New("ledger: mt295: invalid :79: description")
	// ErrMT295UnknownReference is returned by ApplyMT295 when
	// the inbound query's :21: names no message this ledger
	// holds — neither an MT210 notification, nor an inbound
	// MT292 cancellation request, nor an inbound MT202
	// transfer, nor an outbound MT292 cancellation request
	// registered in mt292Exports (LG-474, shape + registry),
	// nor an outbound MT202 transfer registered in
	// mt202Exports (LG-474, registry alone), nor an outbound
	// MT296 answer registered in mt296Exports (LG-478,
	// registry alone) — so it is parked
	// in the repair queue fail-closed rather than dropped. It
	// is a 422-class error.
	ErrMT295UnknownReference = errors.New("ledger: mt295 inbound: unknown :21: related reference")
	// ErrMT295UnknownMT202 is returned by ExportMT295ForMT202
	// when the outbound MT202 :20: the query names is not
	// registered in mt202Exports (LG-475) — the transfer was
	// never rendered by this ledger, so the query parks in the
	// repair queue fail-closed rather than asking about a
	// transfer this ledger could not have sent. It is a
	// 422-class error.
	ErrMT295UnknownMT202 = errors.New("ledger: mt295 export: unknown MT202 reference")
	// ErrMT295UnknownMT296 is returned by ExportMT295ForMT296
	// when the outbound MT296 :20: the query names is not
	// registered in mt296Exports (LG-477) — the answer was
	// never rendered by this ledger, so the query parks in the
	// repair queue fail-closed rather than asking about an
	// answer this ledger could not have sent. It is a
	// 422-class error.
	ErrMT295UnknownMT296 = errors.New("ledger: mt295 export: unknown MT296 reference")
	// ErrMT295UnknownMT210 is returned by ExportMT295ForMT210
	// when the outbound MT210 :20: the query names is not
	// registered in mt210Exports (LG-481) — the notification
	// was never rendered by this ledger, so the query parks in
	// the repair queue fail-closed rather than asking about a
	// notification this ledger could not have sent. It is a
	// 422-class error.
	ErrMT295UnknownMT210 = errors.New("ledger: mt295 export: unknown MT210 reference")
	// ErrMT295UnknownMT292 is returned by ExportMT295ForCancelled
	// when the outbound MT292 :20: the query names is not
	// registered in mt292Exports (LG-476) — the cancellation
	// request was never rendered by this ledger, so the query
	// parks in the repair queue fail-closed rather than asking
	// about a cancellation request this ledger could not have
	// sent. It is a 422-class error.
	ErrMT295UnknownMT292 = errors.New("ledger: mt295 export: unknown MT292 reference")
	// ErrMT295MsgIDConflict is returned when a :20: reference
	// already applied carries different terms: the reference
	// names one query, and a second, different query under
	// it is refused rather than silently overwritten (the
	// pain.014 ErrPain014MsgIDConflict convention). It is a
	// 422-class error.
	ErrMT295MsgIDConflict = errors.New("ledger: mt295: :20: reused for different terms")
)

// MT295 line caps: :75: is 6*35x, :77A: is 20*35x, :79: is
// 50*35x, each line 1-35 SWIFT `x` characters.
const (
	mt295MaxQueriesLines     = 6
	mt295MaxNarrativeLines   = 20
	mt295MaxDescriptionLines = 50
	mt295MaxTextLine         = 35
)

// MT295AnchorKind names which ledger registry a query's :21:
// anchored to: "mt210" for an MT210 notification, "mt292" for
// an inbound MT292 cancellation request, "mt202" for an inbound
// MT202 transfer, "outbound-mt292" for an outbound MT292
// cancellation request registered in mt292Exports (LG-474),
// "outbound-mt202" for an outbound MT202 transfer registered
// in mt202Exports (LG-474), "outbound-mt296" for an outbound
// MT296 answer registered in mt296Exports (LG-478).
type MT295AnchorKind string

const (
	// MT295AnchorMT210 is the :21: tier for an MT210
	// notification (expectedReceipts).
	MT295AnchorMT210 MT295AnchorKind = "mt210"
	// MT295AnchorMT292 is the :21: tier for an inbound MT292
	// cancellation request (mt292Applied).
	MT295AnchorMT292 MT295AnchorKind = "mt292"
	// MT295AnchorMT202 is the :21: tier for an inbound MT202
	// transfer (mt202Applied).
	MT295AnchorMT202 MT295AnchorKind = "mt202"
	// MT295AnchorOutboundMT292 is the :21: tier for an outbound
	// MT292 cancellation request: the :21: has the outbound
	// MT292 :20: shape and is registered in mt292Exports.
	MT295AnchorOutboundMT292 MT295AnchorKind = "outbound-mt292"
	// MT295AnchorOutboundMT202 is the :21: tier for an outbound
	// MT202 transfer: the :21: is registered in mt202Exports.
	// No shape is checked — an outbound MT202 :20: is
	// caller-supplied, 1-16 `x` characters.
	MT295AnchorOutboundMT202 MT295AnchorKind = "outbound-mt202"
	// MT295AnchorOutboundMT296 is the :21: tier for an outbound
	// MT296 answer: the :21: is registered in mt296Exports.
	// No shape is checked — an outbound MT296 :20: is
	// operator-supplied, 1-16 `x` characters.
	MT295AnchorOutboundMT296 MT295AnchorKind = "outbound-mt296"
)

// MT295 is one SWIFT FIN MT295 query message: the block-4 field
// set plus the block 1/2 LT addresses. Queries is the :75:
// coded question (mandatory, 1-6 lines); Narrative is the :77A:
// operator note (optional, 0-20 lines, empty omits :77A:);
// Description is the :79: free-format description (optional,
// 0-50 lines, empty omits :79:). The struct deliberately has no
// amount, currency, value-date, or decision field of its own: an
// MT295 asks about a message, never about value — and the
// codewords inside :75: are prose the operator reads, not a
// status the ledger acts on.
type MT295 struct {
	SenderLT             string
	ReceiverLT           string
	TransactionReference string
	RelatedReference     string
	Queries              []string
	Narrative            []string
	Description          []string
}

// mt295Reference validates a :20:/:21: reference: 1-16 SWIFT
// `x` characters, no braces, no line breaks (a single-line field
// must not smuggle a second line into block 4), no
// leading/trailing colon or slash, no double slash.
func mt295Reference(ref, field string) error {
	if ref == "" || len(ref) > mt103MaxSenderReference {
		return fmt.Errorf("%w: %s: want 1-%d characters, got %q", ErrMT295Reference, field, mt103MaxSenderReference, ref)
	}
	if strings.ContainsAny(ref, "\r\n") {
		return fmt.Errorf("%w: %s: line break in a single-line field: %q", ErrMT295Reference, field, ref)
	}
	if err := mt103CheckX(ref); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrMT295Reference, field, err)
	}
	if strings.HasPrefix(ref, ":") || strings.HasPrefix(ref, "/") ||
		strings.HasSuffix(ref, ":") || strings.HasSuffix(ref, "/") ||
		strings.Contains(ref, "//") {
		return fmt.Errorf("%w: %s: leading/trailing colon or slash, or double slash: %q", ErrMT295Reference, field, ref)
	}
	return nil
}

// mt295TextLines validates one multi-line text field: at most
// maxLines lines of 1-35 SWIFT `x` characters each (braces
// refused, the block-4 framing rule). The MT199 narrative
// shape, under the MT295 field error identities.
func mt295TextLines(lines []string, tag string, maxLines int, errID error) error {
	if len(lines) == 0 || len(lines) > maxLines {
		return fmt.Errorf("%w: %s: want 1-%d lines, got %d", errID, tag, maxLines, len(lines))
	}
	for _, ln := range lines {
		if ln == "" || len(ln) > mt295MaxTextLine {
			return fmt.Errorf("%w: %s: line wants 1-%d characters, got %q", errID, tag, mt295MaxTextLine, ln)
		}
		if strings.ContainsAny(ln, "\r\n") {
			return fmt.Errorf("%w: %s: line break inside a line: %q", errID, tag, ln)
		}
		if err := mt103CheckX(ln); err != nil {
			return fmt.Errorf("%w: %s: %w", errID, tag, err)
		}
	}
	return nil
}

// mt295NormalizeLT normalizes a sender/receiver LT address with
// the MT103 rule (BIC8 -> BIC8+"XXX", BIC11 -> BIC11+"X", BIC12
// as-is), reported under the MT295 error identity.
func mt295NormalizeLT(raw string) (string, error) {
	lt, err := mt103NormalizeLT(raw)
	if err != nil {
		return "", fmt.Errorf("%w: %q", ErrMT295LT, raw)
	}
	return lt, nil
}

// mt295Validate checks every field of m, in field order, and
// returns the normalized sender/receiver LT addresses.
func mt295Validate(m MT295) (senderLT, receiverLT string, err error) {
	senderLT, err = mt295NormalizeLT(m.SenderLT)
	if err != nil {
		return "", "", fmt.Errorf("sender: %w", err)
	}
	receiverLT, err = mt295NormalizeLT(m.ReceiverLT)
	if err != nil {
		return "", "", fmt.Errorf("receiver: %w", err)
	}
	if err := mt295Reference(m.TransactionReference, ":20:"); err != nil {
		return "", "", err
	}
	if err := mt295Reference(m.RelatedReference, ":21:"); err != nil {
		return "", "", err
	}
	if err := mt295TextLines(m.Queries, ":75:", mt295MaxQueriesLines, ErrMT295Queries); err != nil {
		return "", "", err
	}
	if len(m.Narrative) > 0 {
		if err := mt295TextLines(m.Narrative, ":77A:", mt295MaxNarrativeLines, ErrMT295Narrative); err != nil {
			return "", "", err
		}
	}
	if len(m.Description) > 0 {
		if err := mt295TextLines(m.Description, ":79:", mt295MaxDescriptionLines, ErrMT295Description); err != nil {
			return "", "", err
		}
	}
	return senderLT, receiverLT, nil
}

// mt295ParseLT extracts the LT address from a block 1 or block 2
// header line and returns it exactly as written: Parse returns
// the normalized form verbatim rather than re-normalizing it a
// second time (which would append another "X"). Block 1 has the
// form `{1:F01<LT><10 session/sequence digits>}`; block 2 has the
// form `{2:I295<LT><5 header characters>}` (priority U,
// delivery monitoring 3, obsolescence 003).
func mt295ParseLT(line, prefix string, tailLen int) (string, error) {
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, "}") {
		return "", fmt.Errorf("%w: %q", ErrMT295Framing, line)
	}
	inner := line[len(prefix) : len(line)-1]
	if len(inner) <= tailLen {
		return "", fmt.Errorf("%w: %q", ErrMT295Framing, line)
	}
	lt, tail := inner[:len(inner)-tailLen], inner[len(inner)-tailLen:]
	if tailLen == 10 {
		for _, c := range tail {
			if c < '0' || c > '9' {
				return "", fmt.Errorf("%w: %q", ErrMT295Framing, line)
			}
		}
	}
	if len(lt) != 8 && len(lt) != 11 && len(lt) != 12 {
		return "", fmt.Errorf("%w: %q", ErrMT295LT, lt)
	}
	for i := 0; i < len(lt); i++ {
		c := lt[i]
		isLetter := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
		isDigit := c >= '0' && c <= '9'
		if !isLetter && !isDigit {
			return "", fmt.Errorf("%w: %q", ErrMT295LT, lt)
		}
		if i < 6 && !isLetter {
			return "", fmt.Errorf("%w: %q", ErrMT295LT, lt)
		}
	}
	return lt, nil
}

// mt295ParseTextRun consumes one multi-line text field starting
// at lines[i] (the first line carries the tag) and returns the
// lines and the index past the run. A bare line continues the
// field; a line starting with `:` ends it — the caller decides
// whether that tag is the legal next field or an unknown one.
// The caller passes the block-4 body with the trailer already
// stripped, so the run stops at the end of the slice.
func mt295ParseTextRun(lines []string, i int, tag string) ([]string, int, error) {
	run := []string{lines[i][len(tag):]}
	i++
	for ; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], ":") {
			break
		}
		run = append(run, lines[i])
	}
	if run[0] == "" {
		return nil, 0, fmt.Errorf("%w: %s field is empty", ErrMT295Field, tag)
	}
	return run, i, nil
}

// ParseMT295 parses a SWIFT FIN MT295 message (a
// correspondent-bank query about an earlier Category 2 message)
// into its business fields. Parsing is strict: the block
// structure, the field order, and every field value are
// validated, and anything outside the five-field core set (:20:
// / :21: / :75: / :77A: / :79:) is refused — a query must not
// lose content quietly. CRLF line endings are accepted; a
// trailing newline after the `-}` trailer is tolerated. The
// block-3 Message User Reference must name the :20: the block 4
// carries (the MT920 precedent): the :20: is the registration
// key, and a MUR that names a different query would mis-key it.
// A :75: field, once started, ends at the next `:` line — only
// `:77A:` is the legal next tag. A :77A: field, once started,
// ends at the next `:` line — only `:79:` is the legal next tag.
// A :79: field, once started, consumes every remaining block-4
// line as a continuation (it is the last field, so a
// continuation line may itself start with a colon without being
// mistaken for a new field — the same rule as MT199's :79:). A
// bare line starting with `:` inside a :75: run is an unknown
// field, not a continuation — except that `:77A:` ends a :75:
// run, the only legal next tag before :79:.
func ParseMT295(text string) (MT295, error) {
	var zero MT295
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	lines := strings.Split(text, "\n")
	if len(lines) < 8 {
		return zero, fmt.Errorf("%w: want at least 8 lines (3 headers + {4: + :20: + :21: + :75: + -}), got %d", ErrMT295Framing, len(lines))
	}
	senderLT, err := mt295ParseLT(lines[0], "{1:F01", 10)
	if err != nil {
		return zero, err
	}
	receiverLT, err := mt295ParseLT(lines[1], "{2:I295", 5)
	if err != nil {
		return zero, err
	}
	// Block 3: the Message User Reference must name the :20:
	// the block 4 carries — the MT920 precedent, because the
	// :20: is the registration key and a MUR that names a
	// different query would mis-key it.
	mur := lines[2]
	if !strings.HasPrefix(mur, "{3:{108:") || !strings.HasSuffix(mur, "}}") {
		return zero, fmt.Errorf("%w: malformed block 3: %q", ErrMT295Framing, mur)
	}
	murRef := mur[len("{3:{108:") : len(mur)-2]
	if err := mt295Reference(murRef, "{108:}"); err != nil {
		return zero, err
	}
	if lines[3] != "{4:" {
		return zero, fmt.Errorf("%w: want {4: on line 4, got %q", ErrMT295Framing, lines[3])
	}
	if lines[len(lines)-1] != "-}" {
		return zero, fmt.Errorf("%w: want -} trailer, got %q", ErrMT295Framing, lines[len(lines)-1])
	}
	body := lines[4 : len(lines)-1]
	if len(body) < 3 {
		return zero, fmt.Errorf("%w: want at least :20:, :21:, and :75:, got %d block-4 lines", ErrMT295Field, len(body))
	}
	var m MT295
	m.SenderLT, m.ReceiverLT = senderLT, receiverLT
	if !strings.HasPrefix(body[0], ":20:") {
		return zero, fmt.Errorf("%w: block-4 line 1: want :20: field, got %q", ErrMT295Field, body[0])
	}
	ref := body[0][len(":20:"):]
	if ref == "" {
		return zero, fmt.Errorf("%w: :20: field is empty", ErrMT295Field)
	}
	if err := mt295Reference(ref, ":20:"); err != nil {
		return zero, err
	}
	if murRef != ref {
		return zero, fmt.Errorf("%w: {108:} %q does not match :20: %q", ErrMT295Framing, murRef, ref)
	}
	m.TransactionReference = ref
	if !strings.HasPrefix(body[1], ":21:") {
		return zero, fmt.Errorf("%w: block-4 line 2: want :21: field, got %q", ErrMT295Field, body[1])
	}
	related := body[1][len(":21:"):]
	if related == "" {
		return zero, fmt.Errorf("%w: :21: field is empty", ErrMT295Field)
	}
	if err := mt295Reference(related, ":21:"); err != nil {
		return zero, err
	}
	m.RelatedReference = related
	// :75:, when present: the first line carries the tag, the
	// following bare lines continue the question until the
	// trailer or the :77A: tag. Any other `:` line there is an
	// unknown field, not a continuation. :75: is mandatory —
	// the handbook's M, not the MT195 module's optional call —
	// so its absence here is a field error, not an empty query.
	if !strings.HasPrefix(body[2], ":75:") {
		return zero, fmt.Errorf("%w: block-4 line 3: want :75: field, got %q", ErrMT295Field, body[2])
	}
	run, next, err := mt295ParseTextRun(body, 2, ":75:")
	if err != nil {
		return zero, err
	}
	if err := mt295TextLines(run, ":75:", mt295MaxQueriesLines, ErrMT295Queries); err != nil {
		return zero, err
	}
	m.Queries = run
	// :77A:, when present: the first line carries the tag, the
	// following bare lines continue the narrative until the
	// trailer or the :79: tag. Any other `:` line there is an
	// unknown field, not a continuation.
	if next < len(body) && strings.HasPrefix(body[next], ":77A:") {
		run, next, err = mt295ParseTextRun(body, next, ":77A:")
		if err != nil {
			return zero, err
		}
		if err := mt295TextLines(run, ":77A:", mt295MaxNarrativeLines, ErrMT295Narrative); err != nil {
			return zero, err
		}
		m.Narrative = run
	}
	// :79:, when present: the first line carries the tag, and
	// every remaining block-4 line is a continuation — it is
	// the last field, so a continuation line may itself start
	// with a colon (the MT199 :79: rule).
	if next < len(body) {
		if !strings.HasPrefix(body[next], ":79:") {
			return zero, fmt.Errorf("%w: line %d: unexpected field %q", ErrMT295Field, next+5, body[next])
		}
		description := []string{body[next][len(":79:"):]}
		description = append(description, body[next+1:]...)
		if description[0] == "" {
			return zero, fmt.Errorf("%w: :79: field is empty", ErrMT295Field)
		}
		if err := mt295TextLines(description, ":79:", mt295MaxDescriptionLines, ErrMT295Description); err != nil {
			return zero, err
		}
		m.Description = description
		next = len(body)
	}
	// Nothing but the trailer may follow.
	if next != len(body) {
		return zero, fmt.Errorf("%w: line %d: unexpected field %q", ErrMT295Field, next+5, body[next])
	}
	// The parse gate and the apply gate share one check: every
	// field is re-validated through mt295Validate, so a
	// hand-built MT295 and a parsed one face the same rules.
	if _, _, err := mt295Validate(m); err != nil {
		return zero, err
	}
	return m, nil
}

// MT295ApplyResult is the outcome of ApplyMT295: the registered
// query, verbatim, and the registry tier its :21: anchored to. A
// parked query (unknown :21:) carries only Reference,
// RelatedReference, Queries, Narrative, Description, and
// RepairID, alongside the ErrMT295UnknownReference error.
type MT295ApplyResult struct {
	// Reference echoes the query's :20: reference.
	Reference string `json:"reference"`
	// RelatedReference is the queried message's :20: (the :21:).
	RelatedReference string `json:"related_reference"`
	// AnchoredKind names the registry the :21: anchored to —
	// "mt210", "mt292", "mt202", "outbound-mt292",
	// "outbound-mt202", or "outbound-mt296". Empty on a parked query.
	AnchoredKind MT295AnchorKind `json:"anchored_kind,omitempty"`
	// Queries is the :75: question, verbatim.
	Queries []string `json:"queries,omitempty"`
	// Narrative is the :77A: narrative, verbatim.
	Narrative []string `json:"narrative,omitempty"`
	// Description is the :79: description, verbatim.
	Description []string `json:"description,omitempty"`
	// RepairID is the repair queue record for a parked query
	// (unknown :21:). Empty on a registered query.
	RepairID string `json:"repair_id,omitempty"`
	// Duplicate reports whether the query was already
	// registered under the same :20: with the same terms: a
	// no-op success, not an error.
	Duplicate bool `json:"duplicate,omitempty"`
}

// mt295ReceivedRecord is the registry record for one registered
// MT295 query, keyed by :20: in mt295Received: the message as
// received, its :21: anchor, when it was registered, and who
// registered it, so a replay returns the same outcome without
// registering twice.
type mt295ReceivedRecord struct {
	Message          MT295
	RelatedReference string
	ReceivedAt       time.Time
	Actor            string
	Result           MT295ApplyResult
}

// mt295ReceivedTermsEqual reports whether a stored query record
// carries exactly the terms an inbound query would apply: the
// same :21: related reference, the same :75: queries, the same
// :77A: narrative, and the same :79: description, verbatim. The
// sender/receiver LTs are envelope, not terms, along the MT296
// precedent — a replay under different LTs still returns the
// first registration, because the :20: names one query.
func mt295ReceivedTermsEqual(rec mt295ReceivedRecord, m MT295) bool {
	if rec.Message.RelatedReference != m.RelatedReference {
		return false
	}
	if len(rec.Message.Queries) != len(m.Queries) {
		return false
	}
	for i, ln := range rec.Message.Queries {
		if ln != m.Queries[i] {
			return false
		}
	}
	if len(rec.Message.Narrative) != len(m.Narrative) {
		return false
	}
	for i, ln := range rec.Message.Narrative {
		if ln != m.Narrative[i] {
			return false
		}
	}
	if len(rec.Message.Description) != len(m.Description) {
		return false
	}
	for i, ln := range rec.Message.Description {
		if ln != m.Description[i] {
			return false
		}
	}
	return true
}

// mt295LinesCopy returns a copy of lines, so a registered
// query's slices can never be mutated through a returned result
// — the getter invariant, along the other registries.
func mt295LinesCopy(lines []string) []string {
	if lines == nil {
		return nil
	}
	out := make([]string, len(lines))
	copy(out, lines)
	return out
}

// mt295ReceivedResultCopy returns a copy of res with its own
// queries, narrative, and description slices.
func mt295ReceivedResultCopy(res MT295ApplyResult) MT295ApplyResult {
	res.Queries = mt295LinesCopy(res.Queries)
	res.Narrative = mt295LinesCopy(res.Narrative)
	res.Description = mt295LinesCopy(res.Description)
	return res
}

// parkMT295Query parks a fail-closed inbound query in the repair
// queue and returns the record ID. The snapshot names the anchor
// the query tried to reach (:21:) as the instruction ID and the
// query (:20:) as the destination, so an operator can find the
// query from the record. The snapshot's Maker is deliberately
// empty (see the MT195 package comment): the record is operator
// visibility, never a resubmittable instruction. A parking
// failure (a full queue) is returned so the caller can fail loud
// rather than silently drop the bank's query.
func (l *Ledger) parkMT295Query(m MT295, actor string) (string, error) {
	rec, err := l.EnqueueRepair(PayoutInstruction{
		ID:          m.RelatedReference,
		Destination: "mt295-inbound:" + m.TransactionReference,
		// Maker intentionally empty: see the MT195 package comment.
	}, RepairReasonUnknownRoute, actor)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// ApplyMT295 applies one parsed inbound SWIFT FIN MT295 query:
// it registers the correspondent bank's query against the
// Category 2 message its :21: names — verbatim, with the :75:
// question, the :77A: narrative, and the :79: description as
// received, never re-derived from live facts.
//
// The :21: anchors in six tiers, in order: an MT210
// notification (expectedReceipts), an inbound MT292
// cancellation request (mt292Applied), an inbound MT202
// transfer (mt202Applied), an outbound MT292 cancellation
// request (the outbound MT292 :20: shape registered in
// mt292Exports), an outbound MT202 transfer (registered in
// mt202Exports), an outbound MT296 answer (registered in
// mt296Exports). A :21: matching no tier is
// ErrMT295UnknownReference, parked in the repair queue
// fail-closed in the camt.055 shape: the returned result
// carries the repair ID alongside the error. A parked query
// records nothing: a re-sent query parks again — or registers,
// once the queried message exists. No journal entry is ever
// posted, no schedule state is ever changed, no ExpectedReceipt
// is ever cancelled, and no version is ever bumped: a query is
// evidence, not an instruction. No answer snapshot is built: the
// answer is organised per queried object by the later MT296
// export (LG-467).
//
// Idempotency is the :20: with its terms (the pain.014
// convention): a replay with the same :21:, the same :75:
// queries, the same :77A: narrative, and the same :79:
// description returns the stored outcome with Duplicate set,
// registering nothing twice and emitting no second audit event;
// a :20: reused for different terms is refused with
// ErrMT295MsgIDConflict.
//
// Locking: the validation runs on the parsed values alone; the
// replay read and the anchor reads run under the read lock; the
// :20: record and the inbound marker event run under a short
// write lock of their own, re-checking the registry first so a
// concurrent double-apply still records once; the repair parking
// runs outside any held lock because EnqueueRepair takes l.mu
// itself.
func (l *Ledger) ApplyMT295(m MT295, actor string) (MT295ApplyResult, error) {
	if actor == "" {
		actor = "ApplyMT295"
	}
	// The parse gate and the apply gate share one check.
	if _, _, err := mt295Validate(m); err != nil {
		return MT295ApplyResult{}, err
	}

	// :20: replay: the query was already registered under this
	// reference. Same terms is a no-op returning the stored
	// outcome; different terms is a conflict.
	l.mu.RLock()
	if rec, ok := l.mt295Received[m.TransactionReference]; ok {
		l.mu.RUnlock()
		if !mt295ReceivedTermsEqual(rec, m) {
			return MT295ApplyResult{}, fmt.Errorf("%w: :20: %q", ErrMT295MsgIDConflict, m.TransactionReference)
		}
		dup := mt295ReceivedResultCopy(rec.Result)
		dup.Duplicate = true
		return dup, nil
	}
	l.mu.RUnlock()

	// The :21: anchor, six tiers in order: the MT210
	// notification, the inbound MT292 cancellation request, the
	// inbound MT202 transfer, the outbound MT292 cancellation
	// request (shape + mt292Exports registration, LG-474), the
	// outbound MT202 transfer (mt202Exports registration alone,
	// LG-474 — no shape to check), the outbound MT296 answer
	// (mt296Exports registration alone, LG-478 — no shape to
	// check). The five registries are
	// read under one read lock, and the tier order is fixed so
	// a :21: that names both an inbound record and an outbound
	// export anchors to the inbound one. A :21: with the
	// outbound MT292 shape but no registration matches none of
	// them, and parks like any other unknown reference.
	res := MT295ApplyResult{
		Reference:        m.TransactionReference,
		RelatedReference: m.RelatedReference,
		Queries:          mt295LinesCopy(m.Queries),
		Narrative:        mt295LinesCopy(m.Narrative),
		Description:      mt295LinesCopy(m.Description),
	}
	l.mu.RLock()
	_, inMT210 := l.expectedReceipts[m.RelatedReference]
	_, inMT292 := l.mt292Applied[m.RelatedReference]
	_, inMT202 := l.mt202Applied[m.RelatedReference]
	_, outMT292 := l.mt292Exports[m.RelatedReference]
	_, outMT202 := l.mt202Exports[m.RelatedReference]
	_, outMT296 := l.mt296Exports[m.RelatedReference]
	l.mu.RUnlock()
	switch {
	case inMT210:
		res.AnchoredKind = MT295AnchorMT210
	case inMT292:
		res.AnchoredKind = MT295AnchorMT292
	case inMT202:
		res.AnchoredKind = MT295AnchorMT202
	case mt296IsMT292Reference(m.RelatedReference) && outMT292:
		res.AnchoredKind = MT295AnchorOutboundMT292
	case outMT202:
		res.AnchoredKind = MT295AnchorOutboundMT202
	case outMT296:
		res.AnchoredKind = MT295AnchorOutboundMT296
	default:
		repairID, parkErr := l.parkMT295Query(m, actor)
		if parkErr != nil {
			return MT295ApplyResult{}, fmt.Errorf("%w: %q (repair parking failed: %v)", ErrMT295UnknownReference, m.RelatedReference, parkErr)
		}
		return MT295ApplyResult{
			Reference:        m.TransactionReference,
			RelatedReference: m.RelatedReference,
			Queries:          mt295LinesCopy(m.Queries),
			Narrative:        mt295LinesCopy(m.Narrative),
			Description:      mt295LinesCopy(m.Description),
			RepairID:         repairID,
		}, fmt.Errorf("%w: %q", ErrMT295UnknownReference, m.RelatedReference)
	}

	// The :20: record and the inbound marker event, under one
	// short write lock: the query is already built when the
	// marker lands. A concurrent double-apply that landed first
	// returns the stored outcome instead of a second marker.
	// The audit event carries no accounts: a query touches no
	// ledger account, and inventing one would misattribute the
	// evidence.
	l.mu.Lock()
	if rec, ok := l.mt295Received[m.TransactionReference]; ok {
		l.mu.Unlock()
		if !mt295ReceivedTermsEqual(rec, m) {
			return MT295ApplyResult{}, fmt.Errorf("%w: :20: %q", ErrMT295MsgIDConflict, m.TransactionReference)
		}
		dup := mt295ReceivedResultCopy(rec.Result)
		dup.Duplicate = true
		return dup, nil
	}
	if l.mt295Received == nil {
		l.mt295Received = make(map[string]mt295ReceivedRecord)
	}
	l.mt295Received[m.TransactionReference] = mt295ReceivedRecord{
		Message: MT295{
			SenderLT:             m.SenderLT,
			ReceiverLT:           m.ReceiverLT,
			TransactionReference: m.TransactionReference,
			RelatedReference:     m.RelatedReference,
			Queries:              mt295LinesCopy(m.Queries),
			Narrative:            mt295LinesCopy(m.Narrative),
			Description:          mt295LinesCopy(m.Description),
		},
		RelatedReference: m.RelatedReference,
		ReceivedAt:       time.Now().UTC(),
		Actor:            actor,
		Result:           mt295ReceivedResultCopy(res),
	}
	l.emitAudit(AuditEvent{
		Op:            "mt295_query_received",
		Actor:         actor,
		TraceID:       m.TransactionReference,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"msg_id":            m.TransactionReference,
			"related_reference": m.RelatedReference,
			"sender_lt":         m.SenderLT,
		},
	})
	l.mu.Unlock()
	return res, nil
}

// GetMT295Received returns the registered query for msgID (its
// :20:), or false when no query was ever registered under it.
// The returned result is a copy — callers cannot mutate ledger
// state through it.
func (l *Ledger) GetMT295Received(msgID string) (MT295ApplyResult, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	rec, ok := l.mt295Received[msgID]
	if !ok {
		return MT295ApplyResult{}, false
	}
	return mt295ReceivedResultCopy(rec.Result), true
}
