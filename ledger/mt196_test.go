package ledger

import (
	"errors"
	"strings"
	"testing"
)

func mt196Base(t *testing.T) MT196 {
	t.Helper()
	return MT196{
		SenderLT:             "BANKUS33",
		ReceiverLT:           "BANKDEFF",
		TransactionReference: "ANS-196-1",
		RelatedReference:     "MT195QUERY00001",
		Answers:              []string{"PAYOUT STATUS: submitted", "VALUE DATE: 261122", "GPI STATUS: tracked"},
		Narrative:            []string{"Please see the status above"},
	}
}

// mt196AnswerLedger seeds a payout, answers it with an MT195 query,
// and returns the ledger, the payout, and the query's :20: — the
// fixture for the ExportMT196ForQuery tests.
func mt196AnswerLedger(t *testing.T, al *AuditLog) (*Ledger, PayoutInstruction, string) {
	t.Helper()
	l := mt195ApplyLedger(t, al)
	p := mt103SubmittedPayout(t, l, "po-mt196-1", 250_000)
	queryRef := "MT195QUERY00001"
	if _, err := applyMT195Doc(t, l, mt195DocForPayout(p, queryRef)); err != nil {
		t.Fatalf("ApplyMT195: %v", err)
	}
	return l, p, queryRef
}

func mt196ExportOptions(t *testing.T) MT196ExportOptions {
	t.Helper()
	return MT196ExportOptions{
		SenderLT:   "BANKUS33",
		ReceiverLT: "BANKDEFF",
		Reference:  "ANS-196-0001",
		Narrative:  []string{"Please see the status above"},
	}
}

func TestExportMT196_HappyPath(t *testing.T) {
	m := mt196Base(t)
	msg, err := ExportMT196(m)
	if err != nil {
		t.Fatalf("ExportMT196: %v", err)
	}
	want := "{1:F01BANKUS33XXX0000000000}\n" +
		"{2:I196BANKDEFFXXXU3003}\n" +
		"{3:{108:ANS-196-1}}\n" +
		"{4:\n" +
		":20:ANS-196-1\n" +
		":21:MT195QUERY00001\n" +
		":76:PAYOUT STATUS: submitted\n" +
		"VALUE DATE: 261122\n" +
		"GPI STATUS: tracked\n" +
		":77A:Please see the status above\n" +
		"-}"
	if msg != want {
		t.Errorf("message = %q, want %q", msg, want)
	}
	if strings.Contains(msg, "{5:") {
		t.Error("block 5 must be omitted: CHK is network-assigned")
	}
	if strings.Contains(msg, ":32A:") {
		t.Error("an MT196 must never carry a value field")
	}
	// Round-trip: parse returns the same business fields.
	got, err := ParseMT196(msg)
	if err != nil {
		t.Fatalf("ParseMT196: %v", err)
	}
	if got.TransactionReference != "ANS-196-1" || got.RelatedReference != "MT195QUERY00001" ||
		len(got.Answers) != 3 || got.Answers[2] != "GPI STATUS: tracked" ||
		len(got.Narrative) != 1 || got.Narrative[0] != "Please see the status above" {
		t.Errorf("round-trip = %+v", got)
	}
	// Repeat exports are byte-identical.
	again, err := ExportMT196(m)
	if err != nil {
		t.Fatalf("ExportMT196 repeat: %v", err)
	}
	if again != msg {
		t.Error("repeat export is not byte-identical")
	}
}

func TestExportMT196_NoNarrativeAndCRLF(t *testing.T) {
	m := mt196Base(t)
	m.Narrative = nil
	msg, err := ExportMT196(m)
	if err != nil {
		t.Fatalf("ExportMT196: %v", err)
	}
	if strings.Contains(msg, ":77A:") {
		t.Error(":77A: must be omitted when the narrative is empty")
	}
	got, err := ParseMT196(strings.ReplaceAll(msg, "\n", "\r\n") + "\n")
	if err != nil {
		t.Fatalf("ParseMT196 CRLF: %v", err)
	}
	if len(got.Narrative) != 0 || len(got.Answers) != 3 {
		t.Errorf("CRLF parse = %+v", got)
	}
}

func TestParseMT196_RejectsBadBlockStructure(t *testing.T) {
	m := mt196Base(t)
	good, err := ExportMT196(m)
	if err != nil {
		t.Fatalf("ExportMT196: %v", err)
	}

	cases := []struct {
		name string
		doc  string
	}{
		{"block 2 is MT199", strings.Replace(good, "{2:I196", "{2:I199", 1)},
		{"missing block 3", strings.Replace(good, "{3:{108:ANS-196-1}}\n", "", 1)},
		{"missing {4:", strings.Replace(good, "{4:\n", "", 1)},
		{"missing trailer", strings.TrimSuffix(good, "-}")},
		{"field order swapped", strings.Replace(good,
			":20:ANS-196-1\n:21:MT195QUERY00001\n",
			":21:MT195QUERY00001\n:20:ANS-196-1\n", 1)},
		{"empty :20:", strings.Replace(good, ":20:ANS-196-1\n", ":20:\n", 1)},
		{"missing :76:", strings.Replace(good,
			":76:PAYOUT STATUS: submitted\nVALUE DATE: 261122\nGPI STATUS: tracked\n",
			"", 1)},
		{"too few lines", "{1:F01BANKUS33XXX0000000000}"},
		{"empty document", "  "},
	}
	for _, tc := range cases {
		if _, err := ParseMT196(tc.doc); err == nil {
			t.Errorf("%s: parsed without error, want refusal", tc.name)
		}
	}
	// A missing :76: is a field error, not a framing error: the
	// document is well-formed, it just carries no answer.
	no76 := strings.Replace(good,
		":76:PAYOUT STATUS: submitted\nVALUE DATE: 261122\nGPI STATUS: tracked\n",
		"", 1)
	if _, err := ParseMT196(no76); !errors.Is(err, ErrMT196Field) {
		t.Errorf("missing :76:: err = %v, want ErrMT196Field", err)
	}
}

func TestParseMT196_RejectsUnknownField(t *testing.T) {
	m := mt196Base(t)
	good, err := ExportMT196(m)
	if err != nil {
		t.Fatalf("ExportMT196: %v", err)
	}

	// An optional :72: sender-to-receiver field is out of the
	// core set: refused, not parsed with the field quietly
	// discarded.
	with72 := strings.Replace(good, "-}", ":72:/BNK/Please hurry\n-}", 1)
	if _, err := ParseMT196(with72); !errors.Is(err, ErrMT196Field) {
		t.Errorf(":72: field: err = %v, want ErrMT196Field", err)
	}
	// :77E: is not an MT196 field: the narrative tag is :77A:
	// (KELER RCMS User Guide v3.0 section 1.3; Oracle Answers
	// Table 6-5). The backlog's :77E: was corrected to :77A: in
	// LG-457, and this case locks the correction in.
	with77E := strings.Replace(good, ":77A:Please see the status above\n", ":77E:Please see the status above\n", 1)
	if _, err := ParseMT196(with77E); !errors.Is(err, ErrMT196Field) {
		t.Errorf(":77E: field: err = %v, want ErrMT196Field", err)
	}
	// :11S: (MT and date of the original message) is outside the
	// core set: the original message is already named by :21:.
	with11S := strings.Replace(good, ":76:PAYOUT STATUS: submitted\n", ":76:PAYOUT STATUS: submitted\n:11S:195261122\n", 1)
	if _, err := ParseMT196(with11S); !errors.Is(err, ErrMT196Field) {
		t.Errorf(":11S: field: err = %v, want ErrMT196Field", err)
	}
	// A `:` line inside the :76: run is an unknown field, not a
	// continuation.
	withColon := strings.Replace(good, "GPI STATUS: tracked\n", "GPI STATUS: tracked\n:99:extra\n", 1)
	if _, err := ParseMT196(withColon); !errors.Is(err, ErrMT196Field) {
		t.Errorf(":99: in answers: err = %v, want ErrMT196Field", err)
	}
	// :76: is 6*35x: a seventh line is refused.
	seven := mt196Base(t)
	seven.Answers = []string{"L1", "L2", "L3", "L4", "L5", "L6", "L7"}
	if _, err := ExportMT196(seven); !errors.Is(err, ErrMT196Answers) {
		t.Errorf("7-line :76:: err = %v, want ErrMT196Answers", err)
	}
	// :77A: is 20*35x: a twenty-first line is refused.
	many := mt196Base(t)
	many.Narrative = make([]string, 21)
	for i := range many.Narrative {
		many.Narrative[i] = "line"
	}
	if _, err := ExportMT196(many); !errors.Is(err, ErrMT196Narrative) {
		t.Errorf("21-line :77A:: err = %v, want ErrMT196Narrative", err)
	}
	// A :76: line over 35 characters is refused.
	long := mt196Base(t)
	long.Answers = []string{"PAYOUT STATUS: " + strings.Repeat("x", 30)}
	if _, err := ExportMT196(long); !errors.Is(err, ErrMT196Answers) {
		t.Errorf("over-long :76: line: err = %v, want ErrMT196Answers", err)
	}
}

func TestMT196AnswersFromSnapshot_OmitsMissingGPI(t *testing.T) {
	full := MT195ApplyResult{PayoutStatus: "submitted", ValueDate: "261122", GPIStatus: "tracked"}
	lines := mt196AnswersFromSnapshot(full)
	want := []string{"PAYOUT STATUS: submitted", "VALUE DATE: 261122", "GPI STATUS: tracked"}
	if len(lines) != len(want) {
		t.Fatalf("answers = %v, want %v", lines, want)
	}
	for i, ln := range want {
		if lines[i] != ln {
			t.Errorf("answers[%d] = %q, want %q", i, lines[i], ln)
		}
	}
	// No gpi track: the line is omitted, not invented.
	bare := MT195ApplyResult{PayoutStatus: "settled", ValueDate: "261120"}
	lines = mt196AnswersFromSnapshot(bare)
	if len(lines) != 2 || lines[0] != "PAYOUT STATUS: settled" || lines[1] != "VALUE DATE: 261120" {
		t.Errorf("answers without gpi = %v, want two lines, no GPI STATUS", lines)
	}
}

func TestExportMT196ForQuery_RendersSnapshotAnswers(t *testing.T) {
	al := mustAuditLog(t)
	l, p, queryRef := mt196AnswerLedger(t, al)
	before := l.Version()

	opts := mt196ExportOptions(t)
	res, err := l.ExportMT196ForQuery(queryRef, opts, "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT196ForQuery: %v", err)
	}
	if res.Duplicate {
		t.Error("duplicate = true on first export")
	}
	if res.Reference != "ANS-196-0001" {
		t.Errorf("reference = %q, want ANS-196-0001", res.Reference)
	}
	if res.RelatedReference != queryRef {
		t.Errorf("related_reference = %q, want %q", res.RelatedReference, queryRef)
	}
	if res.PayoutID != p.ID {
		t.Errorf("payout_id = %q, want %q", res.PayoutID, p.ID)
	}
	// The answers are the snapshot's ledger facts: the payout is
	// submitted, its MT103 value date is the submit day, and its
	// gpi track — opened at draft time — is "tracked".
	for _, want := range []string{
		":20:ANS-196-0001",
		":21:" + queryRef,
		":76:PAYOUT STATUS: submitted",
		"VALUE DATE: " + mt103ValueDate(p.SubmittedAt),
		"GPI STATUS: tracked",
		":77A:Please see the status above",
		"{2:I196BANKDEFFXXXU3003}",
	} {
		if !strings.Contains(res.Message, want) {
			t.Errorf("message missing %q:\n%s", want, res.Message)
		}
	}
	// Rendering is read-only: no version bump.
	if l.Version() != before {
		t.Errorf("version = %d, want %d (read-only, no bump)", l.Version(), before)
	}
	// The stored export round-trips through the registry.
	stored, ok := l.GetMT196Export("ANS-196-0001")
	if !ok {
		t.Fatal("GetMT196Export = false, want the stored export")
	}
	if stored.Message != res.Message || stored.PayoutID != p.ID {
		t.Errorf("stored export = %+v, want the rendered message", stored)
	}
	// Audit: exactly one export marker, with the exact details —
	// the :20: on the durable record.
	events := readAuditOps(t, al)
	if countOp(events, "mt196_answer_exported") != 1 {
		t.Errorf("mt196_answer_exported count = %d, want 1", countOp(events, "mt196_answer_exported"))
	}
	ev := findOp(events, "mt196_answer_exported")
	if ev == nil {
		t.Fatal("mt196_answer_exported event missing")
	}
	if ev.Actor != "ledger-operator" {
		t.Errorf("actor = %q, want ledger-operator (ExportMT196ForQuery)", ev.Actor)
	}
	if ev.TraceID != "ANS-196-0001" {
		t.Errorf("trace = %q, want ANS-196-0001", ev.TraceID)
	}
	if ev.Details["msg_id"] != "ANS-196-0001" || ev.Details["related_reference"] != queryRef ||
		ev.Details["payout_id"] != p.ID {
		t.Errorf("details = %v, want msg/related/payout", ev.Details)
	}
	// The answer's :76: lines parse back as the snapshot facts.
	parsed, err := ParseMT196(res.Message)
	if err != nil {
		t.Fatalf("ParseMT196(rendered): %v", err)
	}
	if len(parsed.Answers) != 3 || parsed.Answers[0] != "PAYOUT STATUS: submitted" ||
		parsed.Answers[1] != "VALUE DATE: "+mt103ValueDate(p.SubmittedAt) ||
		parsed.Answers[2] != "GPI STATUS: tracked" {
		t.Errorf("parsed answers = %v, want the snapshot facts", parsed.Answers)
	}
}

func TestExportMT196ForQuery_UnknownQueryGoesToRepair(t *testing.T) {
	al := mustAuditLog(t)
	l := mt195ApplyLedger(t, al)
	p := mt103SubmittedPayout(t, l, "po-mt196-1", 250_000)
	_ = p

	opts := mt196ExportOptions(t)
	res, err := l.ExportMT196ForQuery("no-such-query", opts, "ledger-operator")
	if !errors.Is(err, ErrMT196QueryNotFound) {
		t.Errorf("err = %v, want ErrMT196QueryNotFound", err)
	}
	// The parked result carries the repair ID alongside the
	// error: the export is visible to an operator, not dropped.
	if res.RepairID == "" {
		t.Error("repair_id empty, want the parked repair record")
	}
	rec, ok := l.GetRepair(res.RepairID)
	if !ok {
		t.Fatalf("GetRepair(%q) = false, want the parked record", res.RepairID)
	}
	if rec.ReasonCode != RepairReasonUnknownRoute {
		t.Errorf("repair reason = %q, want unknown_route", rec.ReasonCode)
	}
	if rec.Instruction.ID != "no-such-query" {
		t.Errorf("repair instruction id = %q, want the queried :21:", rec.Instruction.ID)
	}
	if rec.Instruction.Destination != "mt196-export:ANS-196-0001" {
		t.Errorf("repair destination = %q, want mt196-export:ANS-196-0001", rec.Instruction.Destination)
	}
	// No export was recorded: nothing to replay, and no answer
	// marker event — the repair_enqueued event is the record.
	if _, ok := l.GetMT196Export("ANS-196-0001"); ok {
		t.Error("GetMT196Export = true for a parked export, want false")
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt196_answer_exported") != 0 {
		t.Errorf("mt196_answer_exported count = %d, want 0", countOp(events, "mt196_answer_exported"))
	}
	if countOp(events, "repair_enqueued") != 1 {
		t.Errorf("repair_enqueued count = %d, want 1", countOp(events, "repair_enqueued"))
	}
}

func TestExportMT196ForQuery_ReplaySameReferenceNoop(t *testing.T) {
	al := mustAuditLog(t)
	l, _, queryRef := mt196AnswerLedger(t, al)
	opts := mt196ExportOptions(t)
	first, err := l.ExportMT196ForQuery(queryRef, opts, "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT196ForQuery: %v", err)
	}
	// Same :20:, same terms: an idempotent no-op returning the
	// stored message, with no second audit event.
	res, err := l.ExportMT196ForQuery(queryRef, opts, "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT196ForQuery replay: %v", err)
	}
	if !res.Duplicate {
		t.Error("duplicate = false on replay, want true")
	}
	if res.Message != first.Message {
		t.Errorf("replay message differs:\nfirst:\n%s\nsecond:\n%s", first.Message, res.Message)
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt196_answer_exported") != 1 {
		t.Errorf("mt196_answer_exported count = %d, want 1 after replay", countOp(events, "mt196_answer_exported"))
	}
	// Same :20:, different LTs: envelope, not terms — the stored
	// message is still returned, duplicate set.
	otherLT := opts
	otherLT.SenderLT = "CHASUS33"
	res, err = l.ExportMT196ForQuery(queryRef, otherLT, "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT196ForQuery replay (other LT): %v", err)
	}
	if !res.Duplicate || res.Message != first.Message {
		t.Errorf("replay under other LTs = %+v, want the stored message, duplicate", res)
	}
	// Same :20:, different narrative: a conflict, never a silent
	// overwrite.
	otherNarr := opts
	otherNarr.Narrative = []string{"A different note"}
	if _, err := l.ExportMT196ForQuery(queryRef, otherNarr, "ledger-operator"); !errors.Is(err, ErrMT196MsgIDConflict) {
		t.Errorf("different narrative: err = %v, want ErrMT196MsgIDConflict", err)
	}
	// Same :20:, different query: a conflict, never a silent
	// overwrite.
	if _, err := l.ExportMT196ForQuery("MT195QUERY00002", opts, "ledger-operator"); !errors.Is(err, ErrMT196MsgIDConflict) {
		t.Errorf("different query: err = %v, want ErrMT196MsgIDConflict", err)
	}
	// An invalid :20: is refused before any lookup: the reference
	// gate runs first.
	badRef := opts
	badRef.Reference = "ANS-196-0001-EXTRA"
	if _, err := l.ExportMT196ForQuery(queryRef, badRef, "ledger-operator"); !errors.Is(err, ErrMT196Reference) {
		t.Errorf("overlong :20:: err = %v, want ErrMT196Reference", err)
	}
}
