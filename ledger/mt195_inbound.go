package ledger

import (
	"errors"
	"fmt"
	"strings"
)

// SWIFT FIN MT195 queries inbound processing (LG-453): the inbound
// half of the MT195 lifecycle. There is no outbound half in this
// ledger — the ledger never asks a correspondent bank about a
// payment it sent; it answers the queries correspondent banks send
// it. The query names an earlier message and asks where it stands;
// the answer is a snapshot of ledger facts, never a new instruction.
//
// Field set, verified against the SWIFT MT195 (Queries) definition
// before this module was written (2026-11-21):
//
//   - :20: Transaction Reference Number (16x, mandatory) — the
//     query's own reference, the MsgId and the replay key.
//   - :21: Related Reference (16x, mandatory) — the original
//     message's :20:, the payout ID, the anchor ParseMT192 uses
//     for :21:.
//   - :75: Queries (6*35x, optional) — the coded question. The
//     National Bank of Serbia SWIFT format guidelines (citing the
//     SWIFT User Handbook) list the request codewords STAT
//     (message status), DUPL (message duplicate) and PRTY
//     (priority); the codewords ride verbatim and are never
//     interpreted — every query is answered with the same
//     ledger-fact snapshot (payout status, MT103 value date,
//     current gpi status), so no codeword allowlist is invented.
//   - :79: Narrative (50*35x, optional) — the free-format
//     description of the query, the MT199 :79: shape (1–50 lines
//     of 1–35 `x` characters each).
//
// Rule C1 (handbook, as read by Payments Signal 2026-10-11): either
// :79: or a copy of the original message's mandatory fields must be
// present, but not both. This parser does not parse the copy form —
// a copy block is refused as an unknown field, like every other
// field outside the core set — and requires at least one of :75: /
// :79: to be present, so a query with no question text at all is
// malformed. :77A: (new priority, for a PRTY change) and :11S:
// (MT and date of the original message) are likewise outside the
// core set: a priority change is a different action from a status
// query, and the value date is taken from the ledger, not the
// wire — both are refused, never silently dropped.
//
// Parsing is strict, along ParseMT192 (LG-438): the block
// structure, the field order (:20: → :21: → :75: → :79:), and
// every field value are validated, and anything outside the
// four-field core set is refused — a query must not lose content
// quietly. CRLF line endings are accepted; a trailing newline
// after the `-}` trailer is tolerated. A bare line starting with
// `:` inside a :75: or :79: run is an unknown field, not a
// continuation — except that `:79:` ends a :75: run, the only
// legal next tag.
//
// ApplyMT195 resolves the :21: against the payouts, builds the
// answer from ledger facts only — the payout's status, its MT103
// value date (mt103ValueDate of the export's default rule:
// submit time, then approval time, then creation time), and the
// current gpi status when a track exists (GetGPITrack) — and
// records the query with its answer snapshot under the :20: in
// mt195Queries. Nothing moves: no journal entry, no payout
// status change, no gpi advance (AdvanceGPI is never called —
// a query is read-only). An unknown :21: is fail-closed into the
// repair queue (RepairReasonUnknownRoute, the camt.055 shape:
// the parked result carries the repair ID alongside the
// ErrMT103PayoutNotFound error); a parked query records no
// answer, so a re-sent query parks again — or answers, once the
// payout exists. The query's own audit event,
// `mt195_query_received` (details msg_id / related_reference /
// payout_id / gpi_status / value_date), puts the :20: on the
// durable record.
//
// Idempotency is the :20: with its terms (the pain.014
// convention, LG-416): re-applying the same :20: with the same
// payout, queries, and narrative returns the stored answer
// snapshot with Duplicate set, answering nothing twice and
// emitting no second audit event. A :20: reused for different
// terms is refused with ErrMT195MsgIDConflict — never silently
// overwritten. The registry check runs again under the write
// lock after the answer is built, so two concurrent applies of
// the same :20: still record once.

// MT195 inbound errors.
var (
	// ErrMT195Framing is returned when the FIN block structure is
	// broken: a missing block, a malformed block 1/2/3 header, a
	// missing or unterminated block 4, or a field out of the
	// canonical :20: → :21: → :75: → :79: order.
	ErrMT195Framing = errors.New("ledger: mt195: malformed FIN framing")
	// ErrMT195Field is returned for a block-4 field problem that
	// is not a value problem: a missing required field, a query
	// carrying neither :75: nor :79:, or a field outside the
	// four-field core set.
	ErrMT195Field = errors.New("ledger: mt195: unexpected or missing field")
	// ErrMT195Reference is returned for an invalid :20: or :21:
	// reference: empty, over 16 characters, outside the SWIFT
	// `x` character set, carrying a brace, or with a
	// leading/trailing colon or slash, or a double slash.
	ErrMT195Reference = errors.New("ledger: mt195: invalid reference")
	// ErrMT195LT is returned for an invalid sender or receiver LT
	// address (not a valid BIC8/BIC11/BIC12 form).
	ErrMT195LT = errors.New("ledger: mt195: invalid SWIFT LT address")
	// ErrMT195MsgIDConflict is returned by ApplyMT195 when a
	// :20: reference already applied carries different terms: the
	// reference names one query, and a second, different query
	// under it is refused rather than silently overwritten (the
	// pain.014 ErrPain014MsgIDConflict convention). It is a
	// 422-class error.
	ErrMT195MsgIDConflict = errors.New("ledger: mt195 inbound: message reference already applied with different terms")
)

// MT195 line caps: :75: is 6*35x, :79: is 50*35x (the MT199
// narrative shape), each line 1–35 SWIFT `x` characters.
const (
	mt195MaxQueriesLines   = 6
	mt195MaxNarrativeLines = 50
	mt195MaxTextLine       = 35
)

// MT195 is one SWIFT FIN MT195 query: the block-4 core fields
// plus the block 1/2 LT addresses. Queries is the :75: coded
// question and Narrative the :79: description, each joined with
// "\n", empty when the message carries no such field.
type MT195 struct {
	SenderLT         string
	ReceiverLT       string
	Reference        string // :20:, this message's own reference, the replay key.
	RelatedReference string // :21:, the original MT103's :20: — the payout ID.
	Queries          string // :75:, the coded question.
	Narrative        string // :79:, the free-format description.
}

// MT195ApplyResult is the outcome of ApplyMT195: the answer
// snapshot for the queried payout, taken from ledger facts at
// apply time. A parked query (unknown :21:) carries only
// Reference, PayoutID, and RepairID, alongside the
// ErrMT103PayoutNotFound error.
type MT195ApplyResult struct {
	// Reference echoes the query's :20: reference.
	Reference string `json:"reference"`
	// PayoutID is the queried payout (the :21:).
	PayoutID string `json:"payout_id"`
	// PayoutStatus is the payout's status at apply time
	// ("submitted", "settled", ...). Empty on a parked query.
	PayoutStatus string `json:"payout_status,omitempty"`
	// ValueDate is the payout's MT103 value date (YYMMDD).
	// Empty on a parked query.
	ValueDate string `json:"value_date,omitempty"`
	// GPIStatus is the payout's current gpi track status when
	// a track exists ("tracked", "ACSP", ...). Empty when the
	// payout was never tracked — a missing track is not an
	// error, it is just no gpi fact to report.
	GPIStatus string `json:"gpi_status,omitempty"`
	// RepairID is the repair queue record for a parked query
	// (unknown :21:). Empty on an answered query.
	RepairID string `json:"repair_id,omitempty"`
	// Duplicate reports whether the query was already answered
	// under the same :20: with the same terms: a no-op success,
	// not an error.
	Duplicate bool `json:"duplicate,omitempty"`
}

// mt195QueryRecord is the registry record for one answered MT195
// query, keyed by :20: in mt195Queries: the query as parsed and
// the answer snapshot its first application recorded, so a replay
// returns the same outcome without answering twice.
type mt195QueryRecord struct {
	Query  MT195
	Answer MT195ApplyResult
}

// mt195TermsEqual reports whether a stored query record carries
// exactly the terms an inbound query would apply: the same
// payout, the same :75: question, and the same :79: narrative,
// verbatim.
func mt195TermsEqual(rec mt195QueryRecord, req MT195) bool {
	return rec.Query.RelatedReference == req.RelatedReference &&
		rec.Query.Queries == req.Queries &&
		rec.Query.Narrative == req.Narrative
}

// mt195Reference validates a :20:/:21: reference: 1–16 SWIFT `x`
// characters, no braces in block-4 content (see mt103CheckX), no
// leading/trailing colon or slash, no double slash. The MT202
// rule, under the MT195 error identity.
func mt195Reference(ref, field string) error {
	if ref == "" || len(ref) > mt103MaxSenderReference {
		return fmt.Errorf("%w: %s: want 1-%d characters, got %q", ErrMT195Reference, field, mt103MaxSenderReference, ref)
	}
	if err := mt103CheckX(ref); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrMT195Reference, field, err)
	}
	if strings.HasPrefix(ref, ":") || strings.HasPrefix(ref, "/") ||
		strings.HasSuffix(ref, ":") || strings.HasSuffix(ref, "/") || strings.Contains(ref, "//") {
		return fmt.Errorf("%w: %s: leading/trailing colon or slash, or double slash: %q", ErrMT195Reference, field, ref)
	}
	return nil
}

// mt195ParseLT extracts the LT address from a block 1 or block 2
// header line and returns it exactly as written. Block 1 has the
// form `{1:F01<LT><10 session/sequence digits>}`; block 2 has the
// form `{2:I195<LT><5 header characters>}` (priority U, delivery
// monitoring 3, obsolescence period 003). The MT202 rule, under
// the MT195 error identity.
func mt195ParseLT(line, prefix string, tailLen int) (string, error) {
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, "}") {
		return "", fmt.Errorf("%w: %q", ErrMT195Framing, line)
	}
	inner := line[len(prefix) : len(line)-1]
	if len(inner) <= tailLen {
		return "", fmt.Errorf("%w: %q", ErrMT195Framing, line)
	}
	lt, tail := inner[:len(inner)-tailLen], inner[len(inner)-tailLen:]
	// Block 1's tail is the session/sequence digits (digit-checked);
	// block 2's tail is header flags (any 5 characters: U3003).
	if tailLen == 10 {
		for _, c := range tail {
			if c < '0' || c > '9' {
				return "", fmt.Errorf("%w: %q", ErrMT195Framing, line)
			}
		}
	}
	if !mt202ValidLT(lt) {
		return "", fmt.Errorf("%w: %q", ErrMT195LT, lt)
	}
	return lt, nil
}

// mt195ParseTextLines validates one multi-line text field: at
// most maxLines lines of 1–35 SWIFT `x` characters each (braces
// refused, the block-4 framing rule). The MT199 narrative shape,
// under the MT195 field error identity.
func mt195ParseTextLines(lines []string, tag string, maxLines int) error {
	if len(lines) > maxLines {
		return fmt.Errorf("%w: %s: at most %d lines", ErrMT195Field, tag, maxLines)
	}
	for _, ln := range lines {
		if len(ln) == 0 || len(ln) > mt195MaxTextLine {
			return fmt.Errorf("%w: %s: line wants 1-%d characters, got %q", ErrMT195Field, tag, mt195MaxTextLine, ln)
		}
		if err := mt103CheckX(ln); err != nil {
			return fmt.Errorf("%w: %s: line: %v", ErrMT195Field, tag, err)
		}
	}
	return nil
}

// mt195ParseTextRun consumes one multi-line text field starting
// at lines[i] (the first line carries the tag) and returns the
// lines and the index past the run. A bare line continues the
// field; a line starting with `:` ends it — the caller decides
// whether that tag is the legal next field or an unknown one.
// The run stops at the trailer.
func mt195ParseTextRun(lines []string, i int, tag string) ([]string, int, error) {
	run := []string{lines[i][len(tag):]}
	i++
	for ; i < len(lines)-1; i++ {
		if strings.HasPrefix(lines[i], ":") {
			break
		}
		run = append(run, lines[i])
	}
	if run[0] == "" {
		return nil, 0, fmt.Errorf("%w: %s field is empty", ErrMT195Field, tag)
	}
	return run, i, nil
}

// ParseMT195 parses a SWIFT FIN MT195 query message into its
// business fields. Parsing is strict: the block structure, the
// field order, and every field value are validated, and anything
// outside the four-field core set (:20: / :21: / :75: / :79:) is
// refused — a query must not lose content quietly. CRLF line
// endings are accepted; a trailing newline after the `-}`
// trailer is tolerated.
func ParseMT195(text string) (MT195, error) {
	var zero MT195
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	lines := strings.Split(text, "\n")
	if len(lines) < 7 {
		return zero, fmt.Errorf("%w: want at least 7 lines (3 headers + {4: + 2 fields + -}), got %d", ErrMT195Framing, len(lines))
	}
	senderLT, err := mt195ParseLT(lines[0], "{1:F01", 10)
	if err != nil {
		return zero, err
	}
	receiverLT, err := mt195ParseLT(lines[1], "{2:I195", 5)
	if err != nil {
		return zero, err
	}
	// Block 3: shape-checked only (see the MT202 package doc).
	mur := lines[2]
	if !strings.HasPrefix(mur, "{3:{108:") || !strings.HasSuffix(mur, "}}") {
		return zero, fmt.Errorf("%w: malformed block 3: %q", ErrMT195Framing, mur)
	}
	if err := mt195Reference(mur[len("{3:{108:"):len(mur)-2], "{108:}"); err != nil {
		return zero, err
	}
	if lines[3] != "{4:" {
		return zero, fmt.Errorf("%w: want {4: on line 4, got %q", ErrMT195Framing, lines[3])
	}
	if lines[len(lines)-1] != "-}" {
		return zero, fmt.Errorf("%w: want -} trailer, got %q", ErrMT195Framing, lines[len(lines)-1])
	}
	// Block 4, canonical order, single-line fields for :20: /
	// :21:: a field body never spans lines in this core set, so
	// any line not starting with its expected tag is a framing
	// error, not a continuation.
	fields := []struct {
		tag, body string
	}{
		{":20:", ""}, {":21:", ""},
	}
	for i, want := range fields {
		line := lines[4+i]
		if !strings.HasPrefix(line, want.tag) {
			return zero, fmt.Errorf("%w: line %d: want %s field, got %q", ErrMT195Field, 5+i, want.tag, line)
		}
		fields[i].body = line[len(want.tag):]
		if fields[i].body == "" {
			return zero, fmt.Errorf("%w: %s field is empty", ErrMT195Field, want.tag)
		}
	}
	var m MT195
	m.SenderLT, m.ReceiverLT = senderLT, receiverLT
	if err := mt195Reference(fields[0].body, ":20:"); err != nil {
		return zero, err
	}
	m.Reference = fields[0].body
	if err := mt195Reference(fields[1].body, ":21:"); err != nil {
		return zero, err
	}
	m.RelatedReference = fields[1].body
	// :75:, when present: the first line carries the tag, the
	// following bare lines continue the question until the
	// trailer or the :79: tag. Any other `:` line there is an
	// unknown field, not a continuation.
	i := 6
	if i < len(lines)-1 && strings.HasPrefix(lines[i], ":75:") {
		run, next, err := mt195ParseTextRun(lines, i, ":75:")
		if err != nil {
			return zero, err
		}
		if err := mt195ParseTextLines(run, ":75:", mt195MaxQueriesLines); err != nil {
			return zero, err
		}
		m.Queries = strings.Join(run, "\n")
		i = next
	}
	// :79:, when present: the first line carries the tag, the
	// following bare lines continue the narrative until the
	// trailer. A line starting with `:` there is an unknown
	// field, not a continuation.
	if i < len(lines)-1 && strings.HasPrefix(lines[i], ":79:") {
		run, next, err := mt195ParseTextRun(lines, i, ":79:")
		if err != nil {
			return zero, err
		}
		if err := mt195ParseTextLines(run, ":79:", mt195MaxNarrativeLines); err != nil {
			return zero, err
		}
		m.Narrative = strings.Join(run, "\n")
		i = next
	}
	// Nothing but the trailer may follow.
	if i != len(lines)-1 {
		return zero, fmt.Errorf("%w: line %d: unexpected field %q", ErrMT195Field, i+1, lines[i])
	}
	// A query must ask something: at least one of :75: / :79:.
	if m.Queries == "" && m.Narrative == "" {
		return zero, fmt.Errorf("%w: a query must carry :75: or :79:", ErrMT195Field)
	}
	return m, nil
}

// validateMT195Request fail-closes on a query ParseMT195 would
// refuse, so ApplyMT195 stays safe on hand-built requests too:
// the parse gate and the apply gate share one check, never a
// copy. A query with no question text cannot be answered.
func validateMT195Request(req MT195) error {
	if err := mt195Reference(req.Reference, ":20:"); err != nil {
		return err
	}
	if err := mt195Reference(req.RelatedReference, ":21:"); err != nil {
		return err
	}
	if strings.TrimSpace(req.Queries) == "" && strings.TrimSpace(req.Narrative) == "" {
		return fmt.Errorf("%w: a query must carry :75: or :79:", ErrMT195Field)
	}
	return nil
}

// parkMT195Query parks a fail-closed parsed query in the repair
// queue and returns the record ID. The snapshot names the queried
// payout (:21:) as the instruction ID and the query (:20:) as
// the destination, so an operator can find the query from the
// record. The snapshot's Maker is deliberately empty (see the
// package comment): the record is operator visibility, never a
// resubmittable instruction. A parking failure (a full queue)
// is returned so the caller can fail loud rather than silently
// drop the bank's message.
func (l *Ledger) parkMT195Query(req MT195, actor string) (string, error) {
	rec, err := l.EnqueueRepair(PayoutInstruction{
		ID:          req.RelatedReference,
		Destination: "mt195-inbound:" + req.Reference,
		// Maker intentionally empty: see the package comment.
	}, RepairReasonUnknownRoute, actor)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// ApplyMT195 applies one parsed inbound SWIFT FIN MT195 query:
// it answers the query from ledger facts — the payout's status,
// its MT103 value date, and its current gpi status when a track
// exists — and records the answer under the query's :20:.
//
// The payout must exist (:21: unknown is
// ErrMT103PayoutNotFound, the export's error), parked in the
// repair queue fail-closed (the camt.055 shape: the returned
// result carries the repair ID alongside the error). No payout
// state is ever changed, no gpi track is ever advanced, and no
// journal entry is ever posted: a query is read-only, and the
// answer is a snapshot, not an instruction.
//
// Idempotency is the :20: with its terms (the pain.014
// convention): a replay with the same payout, queries, and
// narrative returns the stored answer snapshot with Duplicate
// set, answering nothing twice and emitting no second audit
// event; a :20: reused for different terms is refused with
// ErrMT195MsgIDConflict.
//
// Locking: the validation runs on the parsed values alone; the
// replay read and the payout read run under the read lock; the
// :20: record and the inbound marker event run under a short
// write lock of their own, re-checking the registry first so a
// concurrent double-apply still records once.
func (l *Ledger) ApplyMT195(req MT195, actor string) (MT195ApplyResult, error) {
	if actor == "" {
		actor = "ApplyMT195"
	}
	if err := validateMT195Request(req); err != nil {
		return MT195ApplyResult{}, err
	}

	// :20: replay: the query was already answered under this
	// reference. Same terms is a no-op returning the stored
	// answer snapshot; different terms is a conflict.
	l.mu.RLock()
	if rec, ok := l.mt195Queries[req.Reference]; ok {
		l.mu.RUnlock()
		if !mt195TermsEqual(rec, req) {
			return MT195ApplyResult{}, fmt.Errorf("%w: :20: %q", ErrMT195MsgIDConflict, req.Reference)
		}
		dup := rec.Answer
		dup.Duplicate = true
		return dup, nil
	}
	l.mu.RUnlock()

	// The payout itself: :21: names it, the original MT103's
	// :20:. Unknown is parked fail-closed in the repair queue
	// (the camt.055 shape) and reported as the export's error.
	l.mu.RLock()
	p, ok := l.payouts[req.RelatedReference]
	l.mu.RUnlock()
	if !ok {
		repairID, parkErr := l.parkMT195Query(req, actor)
		if parkErr != nil {
			return MT195ApplyResult{}, fmt.Errorf("%w: %q (repair parking failed: %v)", ErrMT103PayoutNotFound, req.RelatedReference, parkErr)
		}
		return MT195ApplyResult{Reference: req.Reference, PayoutID: req.RelatedReference, RepairID: repairID},
			fmt.Errorf("%w: %q", ErrMT103PayoutNotFound, req.RelatedReference)
	}

	// The answer, from ledger facts only: the payout's status,
	// its MT103 value date, and the current gpi status when a
	// track exists. A missing track is not an error — it is just
	// no gpi fact to report.
	valueDate := mt103ValueDate(mt192PayoutValueDate(p))
	gpiStatus := ""
	if track, ok := l.GetGPITrack(p.UETR); ok {
		gpiStatus = track.Status
	}
	ans := MT195ApplyResult{
		Reference:    req.Reference,
		PayoutID:     p.ID,
		PayoutStatus: p.Status,
		ValueDate:    valueDate,
		GPIStatus:    gpiStatus,
	}

	// The :20: record and the inbound marker event, under one
	// short write lock: the answer is already built when the
	// marker lands. A concurrent double-apply that landed first
	// returns the stored outcome instead of a second marker.
	l.mu.Lock()
	if rec, ok := l.mt195Queries[req.Reference]; ok {
		l.mu.Unlock()
		if !mt195TermsEqual(rec, req) {
			return MT195ApplyResult{}, fmt.Errorf("%w: :20: %q", ErrMT195MsgIDConflict, req.Reference)
		}
		dup := rec.Answer
		dup.Duplicate = true
		return dup, nil
	}
	if l.mt195Queries == nil {
		l.mt195Queries = make(map[string]mt195QueryRecord)
	}
	l.mt195Queries[req.Reference] = mt195QueryRecord{Query: req, Answer: ans}
	l.emitAudit(AuditEvent{
		Op:            "mt195_query_received",
		Actor:         actor,
		TraceID:       req.Reference,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{p.PayeeAccount},
		Details: map[string]any{
			"msg_id":            req.Reference,
			"related_reference": req.RelatedReference,
			"payout_id":         p.ID,
			"gpi_status":        gpiStatus,
			"value_date":        valueDate,
		},
	})
	l.mu.Unlock()
	return ans, nil
}

// GetMT195Query returns the recorded answer for msgID, or false
// when no query was ever answered under it. The returned record
// is a copy — callers cannot mutate ledger state through it.
func (l *Ledger) GetMT195Query(msgID string) (MT195ApplyResult, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	rec, ok := l.mt195Queries[msgID]
	if !ok {
		return MT195ApplyResult{}, false
	}
	return rec.Answer, true
}
