package ledger

import (
	"errors"
	"testing"
)

// mt196ApplyLedger seeds a payout ledger with an audit log and the
// sender bank's BIC registered on the payee account, the fixture
// for the ApplyMT196 tests: the outbound MT195 query needs the
// sender BIC to render.
func mt196ApplyLedger(t *testing.T, al *AuditLog) *Ledger {
	t.Helper()
	return mt195ExportLedger(t, al)
}

// exportMT195Query exports the outbound MT195 query about payout
// p and returns its :20: reference — the :21: an inbound MT196
// answer must name.
func exportMT195Query(t *testing.T, l *Ledger, p PayoutInstruction) string {
	t.Helper()
	res, err := l.ExportMT195(p.ID, mt195ExportOptions(t), "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT195: %v", err)
	}
	return res.Reference
}

// mt196AnswerDoc renders one inbound SWIFT FIN MT196 answer: ref
// as :20:, queryRef as :21:, answers as the :76: answer set, and
// narrative as the :77A: note. An empty narrative omits :77A:
// entirely.
func mt196AnswerDoc(t *testing.T, ref, queryRef string, answers, narrative []string) string {
	t.Helper()
	msg, err := ExportMT196(MT196{
		SenderLT:             "BANKDEFF",
		ReceiverLT:           "BANKUS33",
		TransactionReference: ref,
		RelatedReference:     queryRef,
		Answers:              answers,
		Narrative:            narrative,
	})
	if err != nil {
		t.Fatalf("ExportMT196: %v", err)
	}
	return msg
}

// mt196AnswerDocForQuery renders the MT196 answering the outbound
// query queryRef about submitted payout p: the :20: is ref, the
// :76: carries the payout's status, value date, and gpi status,
// the :77A: carries the correspondent's note.
func mt196AnswerDocForQuery(t *testing.T, p PayoutInstruction, queryRef, ref string) string {
	t.Helper()
	return mt196AnswerDoc(t, ref, queryRef, []string{
		"PAYOUT STATUS: submitted",
		"VALUE DATE: " + mt103ValueDate(p.SubmittedAt),
		"GPI STATUS: tracked",
	}, []string{"Please see the status above"})
}

// applyMT196Doc parses doc and applies it, failing the test on
// parse errors: the gates under test are the apply gates, not the
// parse gates.
func applyMT196Doc(t *testing.T, l *Ledger, doc string) (MT196ApplyResult, error) {
	t.Helper()
	m, err := ParseMT196(doc)
	if err != nil {
		t.Fatalf("ParseMT196: %v", err)
	}
	return l.ApplyMT196(m, "correspondent-bank")
}

func TestApplyMT196_RegistersAnswerVerbatim(t *testing.T) {
	al := mustAuditLog(t)
	l := mt196ApplyLedger(t, al)
	p := mt103SubmittedPayout(t, l, "po-mt196-apply-1", 250_000)
	queryRef := exportMT195Query(t, l, p)
	before := l.Version()

	res, err := applyMT196Doc(t, l, mt196AnswerDocForQuery(t, p, queryRef, "ANS-196-0001"))
	if err != nil {
		t.Fatalf("ApplyMT196: %v", err)
	}
	if res.Duplicate {
		t.Error("duplicate = true on first apply")
	}
	if res.Reference != "ANS-196-0001" {
		t.Errorf("reference = %q, want ANS-196-0001", res.Reference)
	}
	if res.RelatedReference != queryRef || res.QueryRef != queryRef {
		t.Errorf("related/query = %q/%q, want %q", res.RelatedReference, res.QueryRef, queryRef)
	}
	if res.PayoutID != p.ID {
		t.Errorf("payout_id = %q, want %q", res.PayoutID, p.ID)
	}
	// The answer is the correspondent's words, verbatim — never
	// re-derived from live facts.
	wantAnswers := []string{
		"PAYOUT STATUS: submitted",
		"VALUE DATE: " + mt103ValueDate(p.SubmittedAt),
		"GPI STATUS: tracked",
	}
	if len(res.Answers) != len(wantAnswers) {
		t.Fatalf("answers = %v, want %v", res.Answers, wantAnswers)
	}
	for i, ln := range wantAnswers {
		if res.Answers[i] != ln {
			t.Errorf("answers[%d] = %q, want %q", i, res.Answers[i], ln)
		}
	}
	if len(res.Narrative) != 1 || res.Narrative[0] != "Please see the status above" {
		t.Errorf("narrative = %v, want the correspondent's note verbatim", res.Narrative)
	}
	// Registering is read-only: no version bump.
	if l.Version() != before {
		t.Errorf("version = %d, want %d (read-only, no bump)", l.Version(), before)
	}
	// The stored answer round-trips through both getters.
	stored, ok := l.GetMT196Answer(queryRef)
	if !ok {
		t.Fatal("GetMT196Answer = false, want the stored answer")
	}
	if stored.Reference != res.Reference || stored.PayoutID != p.ID ||
		len(stored.Answers) != 3 || stored.Answers[2] != "GPI STATUS: tracked" {
		t.Errorf("stored answer = %+v, want %+v", stored, res)
	}
	byRef, ok := l.GetMT196AnswerByReference("ANS-196-0001")
	if !ok {
		t.Fatal("GetMT196AnswerByReference = false, want the stored answer")
	}
	if byRef.QueryRef != queryRef {
		t.Errorf("by-reference query_ref = %q, want %q", byRef.QueryRef, queryRef)
	}
	// Audit: exactly one inbound marker, with the exact
	// details — the :20: on the durable record.
	events := readAuditOps(t, al)
	if countOp(events, "mt196_answer_applied") != 1 {
		t.Errorf("mt196_answer_applied count = %d, want 1", countOp(events, "mt196_answer_applied"))
	}
	ev := findOp(events, "mt196_answer_applied")
	if ev == nil {
		t.Fatal("mt196_answer_applied event missing")
	}
	if ev.Actor != "correspondent-bank" {
		t.Errorf("actor = %q, want correspondent-bank (ApplyMT196)", ev.Actor)
	}
	if ev.TraceID != "ANS-196-0001" {
		t.Errorf("trace = %q, want ANS-196-0001", ev.TraceID)
	}
	if ev.Details["msg_id"] != "ANS-196-0001" || ev.Details["related_reference"] != queryRef ||
		ev.Details["query_ref"] != queryRef || ev.Details["payout_id"] != p.ID {
		t.Errorf("details = %v, want msg/related/query/payout", ev.Details)
	}
}

func TestApplyMT196_UnknownQueryGoesToRepair(t *testing.T) {
	al := mustAuditLog(t)
	l := mt196ApplyLedger(t, al)
	p := mt103SubmittedPayout(t, l, "po-mt196-apply-1", 250_000)
	_ = p

	res, err := applyMT196Doc(t, l, mt196AnswerDoc(t, "ANS-196-0001", "no-such-query",
		[]string{"PAYOUT STATUS: submitted"}, nil))
	if !errors.Is(err, ErrMT196QueryNotFound) {
		t.Errorf("err = %v, want ErrMT196QueryNotFound", err)
	}
	// The parked result carries the repair ID alongside the
	// error: the answer is visible to an operator, not dropped.
	if res.RepairID == "" {
		t.Error("repair_id empty, want the parked repair record")
	}
	if res.Reference != "ANS-196-0001" || res.RelatedReference != "no-such-query" ||
		res.QueryRef != "no-such-query" {
		t.Errorf("parked result = %+v, want reference/related/query", res)
	}
	rec, ok := l.GetRepair(res.RepairID)
	if !ok {
		t.Fatalf("GetRepair(%q) = false, want the parked record", res.RepairID)
	}
	if rec.ReasonCode != RepairReasonUnknownRoute {
		t.Errorf("repair reason = %q, want unknown_route", rec.ReasonCode)
	}
	if rec.Instruction.ID != "no-such-query" {
		t.Errorf("repair instruction id = %q, want the answered :21:", rec.Instruction.ID)
	}
	if rec.Instruction.Destination != "mt196-inbound:ANS-196-0001" {
		t.Errorf("repair destination = %q, want mt196-inbound:ANS-196-0001", rec.Instruction.Destination)
	}
	// No answer was recorded: nothing to replay, and no inbound
	// marker event — the repair_enqueued event is the record.
	if _, ok := l.GetMT196Answer("no-such-query"); ok {
		t.Error("GetMT196Answer = true for a parked answer, want false")
	}
	if _, ok := l.GetMT196AnswerByReference("ANS-196-0001"); ok {
		t.Error("GetMT196AnswerByReference = true for a parked answer, want false")
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt196_answer_applied") != 0 {
		t.Errorf("mt196_answer_applied count = %d, want 0", countOp(events, "mt196_answer_applied"))
	}
	if countOp(events, "repair_enqueued") != 1 {
		t.Errorf("repair_enqueued count = %d, want 1", countOp(events, "repair_enqueued"))
	}
}

func TestApplyMT196_ReplaySameReferenceNoop(t *testing.T) {
	al := mustAuditLog(t)
	l := mt196ApplyLedger(t, al)
	p := mt103SubmittedPayout(t, l, "po-mt196-apply-1", 250_000)
	queryRef := exportMT195Query(t, l, p)
	doc := mt196AnswerDocForQuery(t, p, queryRef, "ANS-196-0001")
	if _, err := applyMT196Doc(t, l, doc); err != nil {
		t.Fatalf("ApplyMT196: %v", err)
	}
	// Same :20:, same terms: an idempotent no-op returning the
	// stored outcome, with no second audit event.
	res, err := applyMT196Doc(t, l, doc)
	if err != nil {
		t.Fatalf("ApplyMT196 replay: %v", err)
	}
	if !res.Duplicate {
		t.Error("duplicate = false on replay, want true")
	}
	if len(res.Answers) != 3 || res.Answers[0] != "PAYOUT STATUS: submitted" {
		t.Errorf("replay answers = %v, want the stored verbatim answers", res.Answers)
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt196_answer_applied") != 1 {
		t.Errorf("mt196_answer_applied count = %d, want 1 after replay", countOp(events, "mt196_answer_applied"))
	}
	// Same :20:, different answers: a conflict, never a silent
	// overwrite.
	conflict := mt196AnswerDoc(t, "ANS-196-0001", queryRef,
		[]string{"PAYOUT STATUS: settled"}, nil)
	if _, err := applyMT196Doc(t, l, conflict); !errors.Is(err, ErrMT196MsgIDConflict) {
		t.Errorf("conflict answers: err = %v, want ErrMT196MsgIDConflict", err)
	}
	// Same :20:, different narrative: a conflict too.
	conflict = mt196AnswerDoc(t, "ANS-196-0001", queryRef,
		[]string{"PAYOUT STATUS: submitted", "VALUE DATE: " + mt103ValueDate(p.SubmittedAt), "GPI STATUS: tracked"},
		[]string{"A different note"})
	if _, err := applyMT196Doc(t, l, conflict); !errors.Is(err, ErrMT196MsgIDConflict) {
		t.Errorf("conflict narrative: err = %v, want ErrMT196MsgIDConflict", err)
	}
	// A different :20: answering the same query: one query,
	// one answer — a conflict, not a second registration.
	second := mt196AnswerDocForQuery(t, p, queryRef, "ANS-196-0002")
	if _, err := applyMT196Doc(t, l, second); !errors.Is(err, ErrMT196MsgIDConflict) {
		t.Errorf("second answer to same query: err = %v, want ErrMT196MsgIDConflict", err)
	}
	events = readAuditOps(t, al)
	if countOp(events, "mt196_answer_applied") != 1 {
		t.Errorf("mt196_answer_applied count = %d, want 1 after conflicts", countOp(events, "mt196_answer_applied"))
	}
}

func TestApplyMT196_DoesNotMoveFunds(t *testing.T) {
	al := mustAuditLog(t)
	l := mt196ApplyLedger(t, al)
	p := mt103SubmittedPayout(t, l, "po-mt196-apply-1", 250_000)
	queryRef := exportMT195Query(t, l, p)
	before := l.Version()
	trackBefore, ok := l.GetGPITrack(p.UETR)
	if !ok {
		t.Fatal("gpi track missing for a submitted payout")
	}

	if _, err := applyMT196Doc(t, l, mt196AnswerDocForQuery(t, p, queryRef, "ANS-196-0001")); err != nil {
		t.Fatalf("ApplyMT196: %v", err)
	}
	// The payout itself stays submitted, the version is
	// untouched, and the gpi history is exactly as it was: an
	// answer advances nothing.
	if got, ok := l.GetPayout(p.ID); !ok || got.Status != PayoutSubmitted {
		t.Errorf("stored payout = %+v, want still submitted", got)
	}
	if l.Version() != before {
		t.Errorf("version = %d, want %d (read-only, no bump)", l.Version(), before)
	}
	trackAfter, ok := l.GetGPITrack(p.UETR)
	if !ok {
		t.Fatal("gpi track missing after answer")
	}
	if len(trackAfter.History) != len(trackBefore.History) || trackAfter.Status != trackBefore.Status {
		t.Errorf("gpi track moved: %+v -> %+v, want unchanged", trackBefore, trackAfter)
	}
}

func TestApplyMT196_AnswerIsVerbatimNotLive(t *testing.T) {
	al := mustAuditLog(t)
	l := mt196ApplyLedger(t, al)
	p := mt103SubmittedPayout(t, l, "po-mt196-apply-1", 250_000)
	queryRef := exportMT195Query(t, l, p)

	first, err := applyMT196Doc(t, l, mt196AnswerDocForQuery(t, p, queryRef, "ANS-196-0001"))
	if err != nil {
		t.Fatalf("ApplyMT196: %v", err)
	}
	if len(first.Answers) != 3 || first.Answers[2] != "GPI STATUS: tracked" {
		t.Fatalf("first answers = %v, want the submitted/track fact lines", first.Answers)
	}
	// The correspondent's verdict moves the track; the registered
	// answer is the correspondent's words, not a live view — it
	// still reads "tracked".
	if _, err := l.AdvanceGPI(p.UETR, GPIStatusACSP, "", "correspondent-bank"); err != nil {
		t.Fatalf("AdvanceGPI: %v", err)
	}
	stored, ok := l.GetMT196Answer(queryRef)
	if !ok {
		t.Fatal("GetMT196Answer = false after gpi advance, want the stored answer")
	}
	if len(stored.Answers) != 3 || stored.Answers[2] != "GPI STATUS: tracked" {
		t.Errorf("stored answers = %v, want the original verbatim answers", stored.Answers)
	}
	replay, err := applyMT196Doc(t, l, mt196AnswerDocForQuery(t, p, queryRef, "ANS-196-0001"))
	if err != nil {
		t.Fatalf("ApplyMT196 replay: %v", err)
	}
	if !replay.Duplicate || len(replay.Answers) != 3 || replay.Answers[2] != "GPI STATUS: tracked" {
		t.Errorf("replay = %+v, want duplicate with the original verbatim answers", replay)
	}
}

func TestApplyMT196_ValidationRejectsBadAnswers(t *testing.T) {
	al := mustAuditLog(t)
	l := mt196ApplyLedger(t, al)
	p := mt103SubmittedPayout(t, l, "po-mt196-apply-1", 250_000)
	queryRef := exportMT195Query(t, l, p)

	// No answers at all: :76: is mandatory.
	empty := MT196{
		SenderLT: "BANKDEFF", ReceiverLT: "BANKUS33",
		TransactionReference: "ANS-196-0001", RelatedReference: queryRef,
	}
	if _, err := l.ApplyMT196(empty, "correspondent-bank"); !errors.Is(err, ErrMT196Answers) {
		t.Errorf("empty answers: err = %v, want ErrMT196Answers", err)
	}
	// A :20: over 16 characters is refused.
	longRef := MT196{
		SenderLT: "BANKDEFF", ReceiverLT: "BANKUS33",
		TransactionReference: "ANS-196-0001-EXTRA", RelatedReference: queryRef,
		Answers: []string{"PAYOUT STATUS: submitted"},
	}
	if _, err := l.ApplyMT196(longRef, "correspondent-bank"); !errors.Is(err, ErrMT196Reference) {
		t.Errorf("overlong :20:: err = %v, want ErrMT196Reference", err)
	}
	// An empty :21: names no query.
	noQuery := MT196{
		SenderLT: "BANKDEFF", ReceiverLT: "BANKUS33",
		TransactionReference: "ANS-196-0001",
		Answers:              []string{"PAYOUT STATUS: submitted"},
	}
	if _, err := l.ApplyMT196(noQuery, "correspondent-bank"); !errors.Is(err, ErrMT196Reference) {
		t.Errorf("empty :21:: err = %v, want ErrMT196Reference", err)
	}
	// An invalid sender LT is refused.
	badLT := MT196{
		SenderLT: "NOTABIC", ReceiverLT: "BANKUS33",
		TransactionReference: "ANS-196-0001", RelatedReference: queryRef,
		Answers: []string{"PAYOUT STATUS: submitted"},
	}
	if _, err := l.ApplyMT196(badLT, "correspondent-bank"); !errors.Is(err, ErrMT196LT) {
		t.Errorf("bad sender LT: err = %v, want ErrMT196LT", err)
	}
	// Nothing was registered by the refused applies.
	if _, ok := l.GetMT196Answer(queryRef); ok {
		t.Error("GetMT196Answer = true after refused applies, want false")
	}
}

func TestMT196Answer_GetByReference(t *testing.T) {
	al := mustAuditLog(t)
	l := mt196ApplyLedger(t, al)
	p := mt103SubmittedPayout(t, l, "po-mt196-apply-1", 250_000)
	queryRef := exportMT195Query(t, l, p)

	if _, ok := l.GetMT196Answer(queryRef); ok {
		t.Error("GetMT196Answer = true before any answer, want false")
	}
	if _, ok := l.GetMT196AnswerByReference("ANS-196-0001"); ok {
		t.Error("GetMT196AnswerByReference = true before any answer, want false")
	}
	res, err := applyMT196Doc(t, l, mt196AnswerDocForQuery(t, p, queryRef, "ANS-196-0001"))
	if err != nil {
		t.Fatalf("ApplyMT196: %v", err)
	}
	got, ok := l.GetMT196Answer(queryRef)
	if !ok {
		t.Fatal("GetMT196Answer = false after registering, want true")
	}
	if got.Reference != res.Reference || got.PayoutID != res.PayoutID ||
		len(got.Answers) != len(res.Answers) || got.Answers[0] != res.Answers[0] {
		t.Errorf("GetMT196Answer = %+v, want %+v", got, res)
	}
	if _, ok := l.GetMT196Answer("no-such-query"); ok {
		t.Error("GetMT196Answer = true for an unknown query, want false")
	}
	if _, ok := l.GetMT196AnswerByReference("ANS-196-9999"); ok {
		t.Error("GetMT196AnswerByReference = true for an unknown :20:, want false")
	}
	// Mutating a returned result's slices must not mutate the
	// stored answer: getters return copies.
	got.Answers[0] = "PAYOUT STATUS: settled"
	again, ok := l.GetMT196Answer(queryRef)
	if !ok {
		t.Fatal("GetMT196Answer = false on re-read, want true")
	}
	if again.Answers[0] != "PAYOUT STATUS: submitted" {
		t.Errorf("stored answers mutated through a getter: %v", again.Answers)
	}
}

func TestApplyMT196_SecondQuerySeparateAnswer(t *testing.T) {
	al := mustAuditLog(t)
	l := mt196ApplyLedger(t, al)
	p := mt103SubmittedPayout(t, l, "po-mt196-apply-1", 250_000)
	// Two outbound queries about the same payout, different
	// questions, different :20:s — each gets its own answer.
	firstRef := exportMT195Query(t, l, p)
	secondRes, err := l.ExportMT195(p.ID, MT195ExportOptions{
		ReceiverLT: "BANKDEFF",
		Queries:    "DUPL",
		Narrative:  "Please confirm no duplicate",
	}, "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT195 second: %v", err)
	}
	secondRef := secondRes.Reference
	if secondRef == firstRef {
		t.Fatalf("second query :20: = %q, want a different reference", secondRef)
	}

	first, err := applyMT196Doc(t, l, mt196AnswerDocForQuery(t, p, firstRef, "ANS-196-0001"))
	if err != nil {
		t.Fatalf("ApplyMT196 first: %v", err)
	}
	second, err := applyMT196Doc(t, l, mt196AnswerDoc(t, "ANS-196-0002", secondRef,
		[]string{"PAYOUT STATUS: submitted", "VALUE DATE: " + mt103ValueDate(p.SubmittedAt)},
		[]string{"No duplicate found"}))
	if err != nil {
		t.Fatalf("ApplyMT196 second: %v", err)
	}
	if first.PayoutID != p.ID || second.PayoutID != p.ID {
		t.Errorf("payout ids = %q/%q, want %q", first.PayoutID, second.PayoutID, p.ID)
	}
	if len(second.Answers) != 2 || second.Answers[1] != "VALUE DATE: "+mt103ValueDate(p.SubmittedAt) {
		t.Errorf("second answers = %v, want two lines, no gpi line", second.Answers)
	}
	stored, ok := l.GetMT196Answer(secondRef)
	if !ok || stored.Reference != "ANS-196-0002" {
		t.Errorf("second stored = %+v, want ANS-196-0002", stored)
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt196_answer_applied") != 2 {
		t.Errorf("mt196_answer_applied count = %d, want 2", countOp(events, "mt196_answer_applied"))
	}
	// The first answer is untouched by the second registration.
	if first.Answers[0] != "PAYOUT STATUS: submitted" {
		t.Errorf("first answers = %v, want still submitted", first.Answers)
	}
}
