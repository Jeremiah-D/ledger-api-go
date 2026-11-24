package ledger

import (
	"errors"
	"strings"
	"testing"
)

// mt296InboundLedger seeds a payout ledger with an audit log, the
// fixture for the ApplyMT296 tests: registering an answer moves
// nothing, so no funded account is needed.
func mt296InboundLedger(t *testing.T, al *AuditLog) *Ledger {
	t.Helper()
	return seedPayoutLedger(t, WithAuditLog(al))
}

// mt296InboundDoc renders one inbound MT296 answer: ref as :20:,
// relatedRef as :21: (the answered MT292's :20:), answers as the
// :76: lines, narrative as the :77A: lines (empty omits :77A:),
// description as the :79: lines (empty omits :79:). There is no
// ExportMT296 (LG-465 explicitly excludes an MT296 export —
// this ledger never answers another bank's MT292), so the
// fixture is hand-built FIN text, the MT292 inbound convention.
func mt296InboundDoc(ref, relatedRef string, answers, narrative, description []string) string {
	var b strings.Builder
	b.WriteString("{1:F01CITIUS33XXX0000000000}\n")
	b.WriteString("{2:I296NWBKGB2LXXXU3003}\n")
	b.WriteString("{3:{108:" + ref + "}}\n")
	b.WriteString("{4:\n")
	b.WriteString(":20:" + ref + "\n")
	b.WriteString(":21:" + relatedRef + "\n")
	b.WriteString(":76:" + answers[0] + "\n")
	for _, ln := range answers[1:] {
		b.WriteString(ln + "\n")
	}
	if len(narrative) > 0 {
		b.WriteString(":77A:" + narrative[0] + "\n")
		for _, ln := range narrative[1:] {
			b.WriteString(ln + "\n")
		}
	}
	if len(description) > 0 {
		b.WriteString(":79:" + description[0] + "\n")
		for _, ln := range description[1:] {
			b.WriteString(ln + "\n")
		}
	}
	b.WriteString("-}")
	return b.String()
}

// mt296AnsweredRef is a :21: with the shape of an outbound MT292
// :20: — the deterministic reference ExportMT292 derives for
// schedule "sched-1", run 1 (mt292MessageReference).
func mt296AnsweredRef() string {
	return mt292MessageReference("sched-1", 1)
}

// applyInboundMT296 parses and applies one inbound MT296 for the
// fixture, the inbound-apply test convention.
func applyInboundMT296(t *testing.T, l *Ledger, doc, actor string) MT296ApplyResult {
	t.Helper()
	m, err := ParseMT296(doc)
	if err != nil {
		t.Fatalf("ParseMT296: %v", err)
	}
	res, err := l.ApplyMT296(m, actor)
	if err != nil {
		t.Fatalf("ApplyMT296: %v", err)
	}
	return res
}

func TestParseMT296_FullDocument(t *testing.T) {
	doc := mt296InboundDoc("ANS-296-1", mt296AnsweredRef(),
		[]string{"/CNCL/", "CANCELLATION CONFIRMED 261124"},
		[]string{"OPERATOR NOTE"},
		[]string{"BANK CONFIRMS THE NOTIFICATION", "IS WITHDRAWN"})
	m, err := ParseMT296(doc)
	if err != nil {
		t.Fatalf("ParseMT296: %v", err)
	}
	if m.TransactionReference != "ANS-296-1" || m.RelatedReference != mt296AnsweredRef() {
		t.Errorf("refs = %q/%q, want ANS-296-1/%s", m.TransactionReference, m.RelatedReference, mt296AnsweredRef())
	}
	if len(m.Answers) != 2 || m.Answers[0] != "/CNCL/" || m.Answers[1] != "CANCELLATION CONFIRMED 261124" {
		t.Errorf("answers = %v, want the :76: lines verbatim", m.Answers)
	}
	if len(m.Narrative) != 1 || m.Narrative[0] != "OPERATOR NOTE" {
		t.Errorf("narrative = %v, want [OPERATOR NOTE]", m.Narrative)
	}
	if len(m.Description) != 2 || m.Description[0] != "BANK CONFIRMS THE NOTIFICATION" ||
		m.Description[1] != "IS WITHDRAWN" {
		t.Errorf("description = %v, want the :79: lines verbatim", m.Description)
	}
	if m.SenderLT != "CITIUS33XXX" || m.ReceiverLT != "NWBKGB2LXXX" {
		t.Errorf("LTs = %q/%q, want CITIUS33XXX/NWBKGB2LXXX", m.SenderLT, m.ReceiverLT)
	}
	// CRLF endings and a trailing newline after the trailer are
	// tolerated, like the MT196 parse.
	crlf := strings.ReplaceAll(doc, "\n", "\r\n") + "\n"
	if _, err := ParseMT296(crlf); err != nil {
		t.Errorf("ParseMT296 CRLF: %v", err)
	}
}

func TestParseMT296_MinimalDocument(t *testing.T) {
	m, err := ParseMT296(mt296InboundDoc("ANS-296-2", mt296AnsweredRef(),
		[]string{"/PDCR/"}, nil, nil))
	if err != nil {
		t.Fatalf("ParseMT296: %v", err)
	}
	if len(m.Answers) != 1 || m.Answers[0] != "/PDCR/" {
		t.Errorf("answers = %v, want [/PDCR/]", m.Answers)
	}
	if m.Narrative != nil || m.Description != nil {
		t.Errorf("narrative/description = %v/%v, want nil (both optional)", m.Narrative, m.Description)
	}
}

func TestParseMT296_RejectsMissingFields(t *testing.T) {
	cases := map[string]string{
		"missing :21:": "{1:F01CITIUS33XXX0000000000}\n{2:I296NWBKGB2LXXXU3003}\n{3:{108:ANS-296-3}}\n{4:\n:20:ANS-296-3\n:76:/CNCL/\n-}",
		"missing :76:": "{1:F01CITIUS33XXX0000000000}\n{2:I296NWBKGB2LXXXU3003}\n{3:{108:ANS-296-3}}\n{4:\n:20:ANS-296-3\n:21:" + mt296AnsweredRef() + "\n-}",
		"empty :20:": "{1:F01CITIUS33XXX0000000000}\n{2:I296NWBKGB2LXXXU3003}\n{3:{108:ANS-296-3}}\n{4:\n:20:\n:21:" + mt296AnsweredRef() + "\n:76:/CNCL/\n-}",
		"empty :21:": "{1:F01CITIUS33XXX0000000000}\n{2:I296NWBKGB2LXXXU3003}\n{3:{108:ANS-296-3}}\n{4:\n:20:ANS-296-3\n:21:\n:76:/CNCL/\n-}",
	}
	for name, doc := range cases {
		if _, err := ParseMT296(doc); err == nil {
			t.Errorf("%s: ParseMT296 succeeded, want an error", name)
		}
	}
}

func TestParseMT296_RejectsUnknownField(t *testing.T) {
	// :11S: is outside the core set — refused, never silently
	// dropped (the MT196 :11a: precedent).
	with11S := mt296InboundDoc("ANS-296-4", mt296AnsweredRef(),
		[]string{"/CNCL/"}, []string{"NOTE"}, nil)
	with11S = strings.Replace(with11S, "-}", ":11S:292261124\n-}", 1)
	if _, err := ParseMT296(with11S); err == nil {
		t.Error(":11S: accepted, want it refused as an unknown field")
	}
	// :77E: is not the narrative tag — the MT196 :77E: precedent.
	badTag := strings.Replace(mt296InboundDoc("ANS-296-4", mt296AnsweredRef(),
		[]string{"/CNCL/"}, []string{"NOTE"}, nil), ":77A:", ":77E:", 1)
	if _, err := ParseMT296(badTag); err == nil {
		t.Error(":77E: accepted, want it refused as an unknown field")
	}
	// :79: before :76: is out of order.
	outOfOrder := "{1:F01CITIUS33XXX0000000000}\n{2:I296NWBKGB2LXXXU3003}\n{3:{108:ANS-296-4}}\n{4:\n:20:ANS-296-4\n:21:" + mt296AnsweredRef() + "\n:79:TOO EARLY\n:76:/CNCL/\n-}"
	if _, err := ParseMT296(outOfOrder); err == nil {
		t.Error("out-of-order :79: accepted, want a framing error")
	}
}

func TestParseMT296_RejectsMismatchedMUR(t *testing.T) {
	doc := mt296InboundDoc("ANS-296-5", mt296AnsweredRef(), []string{"/CNCL/"}, nil, nil)
	doc = strings.Replace(doc, "{108:ANS-296-5}", "{108:OTHER-REF}", 1)
	if _, err := ParseMT296(doc); err == nil {
		t.Error("mismatched {108:} accepted, want a framing error")
	}
}

func TestParseMT296_AnswersAreVerbatimText(t *testing.T) {
	// No code table: any 1-35 `x` line parses as a :76: answer,
	// so a code the ledger has never seen is evidence, not an
	// error — the ledger invents no accept/reject code table.
	m, err := ParseMT296(mt296InboundDoc("ANS-296-6", mt296AnsweredRef(),
		[]string{"/WIBBLE/", "FREE TEXT ANSWER"}, nil, nil))
	if err != nil {
		t.Fatalf("ParseMT296: %v", err)
	}
	if len(m.Answers) != 2 || m.Answers[0] != "/WIBBLE/" {
		t.Errorf("answers = %v, want the lines verbatim", m.Answers)
	}
	// Seven :76: lines exceed the 6*35x cap.
	tooMany := mt296InboundDoc("ANS-296-6", mt296AnsweredRef(),
		[]string{"L1", "L2", "L3", "L4", "L5", "L6", "L7"}, nil, nil)
	if _, err := ParseMT296(tooMany); err == nil {
		t.Error("7 :76: lines accepted, want an error")
	}
	// An empty :76: first line is a framing error.
	empty := strings.Replace(mt296InboundDoc("ANS-296-6", mt296AnsweredRef(),
		[]string{"/CNCL/"}, nil, nil), ":76:/CNCL/", ":76:", 1)
	if _, err := ParseMT296(empty); err == nil {
		t.Error("empty :76: accepted, want an error")
	}
}

func TestParseMT296_DescriptionColonContinuation(t *testing.T) {
	// :79: is the last field: a continuation line starting with
	// a colon is content, not a new field (the MT199 :79: rule).
	doc := mt296InboundDoc("ANS-296-7", mt296AnsweredRef(),
		[]string{"/CNCL/"}, nil,
		[]string{"SEE :21: OF OUR MT292", ":11S: QUOTED IN PROSE"})
	m, err := ParseMT296(doc)
	if err != nil {
		t.Fatalf("ParseMT296: %v", err)
	}
	if len(m.Description) != 2 || m.Description[1] != ":11S: QUOTED IN PROSE" {
		t.Errorf("description = %v, want the colon line kept as content", m.Description)
	}
}

func TestApplyMT296_RegistersAnswer(t *testing.T) {
	al := mustAuditLog(t)
	l := mt296InboundLedger(t, al)
	before := l.Version()
	entriesBefore := len(l.Entries())

	res := applyInboundMT296(t, l, mt296InboundDoc("ANS-296-10", mt296AnsweredRef(),
		[]string{"/CNCL/", "CANCELLATION CONFIRMED 261124"},
		[]string{"OPERATOR NOTE"},
		[]string{"BANK CONFIRMS THE NOTIFICATION", "IS WITHDRAWN"}), "correspondent-bank")
	if res.Duplicate {
		t.Error("duplicate = true on first apply")
	}
	if res.Reference != "ANS-296-10" || res.RelatedReference != mt296AnsweredRef() {
		t.Errorf("refs = %q/%q, want ANS-296-10/%s", res.Reference, res.RelatedReference, mt296AnsweredRef())
	}
	if len(res.Answers) != 2 || res.Answers[0] != "/CNCL/" {
		t.Errorf("answers = %v, want the :76: lines verbatim", res.Answers)
	}
	if len(res.Narrative) != 1 || res.Narrative[0] != "OPERATOR NOTE" {
		t.Errorf("narrative = %v, want the :77A: lines verbatim", res.Narrative)
	}
	if len(res.Description) != 2 || res.Description[1] != "IS WITHDRAWN" {
		t.Errorf("description = %v, want the :79: lines verbatim", res.Description)
	}
	// Registering is read-only: no version bump, no journal rows.
	if l.Version() != before {
		t.Errorf("version = %d, want %d (read-only, no bump)", l.Version(), before)
	}
	if len(l.Entries()) != entriesBefore {
		t.Errorf("entries = %d, want %d (no journal rows)", len(l.Entries()), entriesBefore)
	}
	// The stored answer round-trips through the getter.
	stored, ok := l.GetMT296Received("ANS-296-10")
	if !ok {
		t.Fatal("GetMT296Received = false, want the stored answer")
	}
	if stored.RelatedReference != mt296AnsweredRef() || len(stored.Answers) != 2 ||
		len(stored.Narrative) != 1 || len(stored.Description) != 2 {
		t.Errorf("stored = %+v, want the registered answer", stored)
	}
	if _, ok := l.GetMT296Received("no-such-answer"); ok {
		t.Error("GetMT296Received unknown = true, want false")
	}
	// Audit: exactly one received marker, with the exact details
	// — the :20: on the durable record, and no accounts: an
	// answer touches no ledger account.
	events := readAuditOps(t, al)
	if countOp(events, "mt296_answer_received") != 1 {
		t.Errorf("mt296_answer_received count = %d, want 1", countOp(events, "mt296_answer_received"))
	}
	ev := findOp(events, "mt296_answer_received")
	if ev == nil {
		t.Fatal("mt296_answer_received event missing")
	}
	if ev.Actor != "correspondent-bank" {
		t.Errorf("actor = %q, want correspondent-bank (ApplyMT296)", ev.Actor)
	}
	if ev.TraceID != "ANS-296-10" {
		t.Errorf("trace = %q, want ANS-296-10", ev.TraceID)
	}
	if ev.Details["msg_id"] != "ANS-296-10" || ev.Details["related_reference"] != mt296AnsweredRef() ||
		ev.Details["sender_lt"] != "CITIUS33XXX" {
		t.Errorf("details = %v, want msg/related/sender", ev.Details)
	}
	if len(ev.Accounts) != 0 {
		t.Errorf("accounts = %v, want none (evidence-only registration)", ev.Accounts)
	}
}

func TestApplyMT296_UnknownReferenceGoesToRepair(t *testing.T) {
	al := mustAuditLog(t)
	l := mt296InboundLedger(t, al)

	// Two shapes that are valid :21: references but name no
	// MT292 this ledger could have sent: a schedule ID, and an
	// MT292-shaped prefix with non-hex characters.
	for _, badRef := range []string{"sched-1", "MT292-ABCDEF0123"} {
		m, err := ParseMT296(mt296InboundDoc("ANS-296-11", badRef,
			[]string{"/CNCL/"}, nil, []string{"ANSWER TO NOTHING WE SENT"}))
		if err != nil {
			t.Fatalf("ParseMT296: %v", err)
		}
		res, err := l.ApplyMT296(m, "correspondent-bank")
		if !errors.Is(err, ErrMT296UnknownReference) {
			t.Errorf(":21: %q: err = %v, want ErrMT296UnknownReference", badRef, err)
		}
		if res.RepairID == "" {
			t.Errorf(":21: %q: repair_id empty, want the parked repair record", badRef)
		}
		if res.Reference != "ANS-296-11" || res.RelatedReference != badRef {
			t.Errorf(":21: %q: parked result = %+v, want reference + related_reference", badRef, res)
		}
		rec, ok := l.GetRepair(res.RepairID)
		if !ok {
			t.Fatalf("GetRepair(%q) = false, want the parked record", res.RepairID)
		}
		if rec.ReasonCode != RepairReasonUnknownRoute {
			t.Errorf("repair reason = %q, want unknown_route", rec.ReasonCode)
		}
		if rec.Instruction.ID != badRef {
			t.Errorf("repair instruction id = %q, want the unanchored :21:", rec.Instruction.ID)
		}
		if rec.Instruction.Destination != "mt296-inbound:ANS-296-11" {
			t.Errorf("repair destination = %q, want mt296-inbound:ANS-296-11", rec.Instruction.Destination)
		}
	}
	// No answer was recorded: nothing to replay, and no received
	// marker event — the repair_enqueued event is the record.
	// (Two parks: one per bad :21: shape.)
	if _, ok := l.GetMT296Received("ANS-296-11"); ok {
		t.Error("GetMT296Received = true for a parked answer, want false")
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt296_answer_received") != 0 {
		t.Errorf("mt296_answer_received count = %d, want 0", countOp(events, "mt296_answer_received"))
	}
	if countOp(events, "repair_enqueued") != 2 {
		t.Errorf("repair_enqueued count = %d, want 2", countOp(events, "repair_enqueued"))
	}
}

func TestApplyMT296_ReplaysSameTerms(t *testing.T) {
	al := mustAuditLog(t)
	l := mt296InboundLedger(t, al)

	doc := mt296InboundDoc("ANS-296-12", mt296AnsweredRef(),
		[]string{"/CNCL/"}, []string{"NOTE"}, []string{"CONFIRMED"})
	first := applyInboundMT296(t, l, doc, "correspondent-bank")
	if first.Duplicate {
		t.Error("duplicate = true on first apply")
	}
	// A replay under different LTs is still the same answer: LTs
	// are envelope, not terms.
	replayDoc := strings.Replace(doc, "CITIUS33XXX", "DEUTDEFFXXX", 1)
	replayDoc = strings.Replace(replayDoc, "NWBKGB2LXXX", "CITIUS33XXX", 1)
	res, err := func() (MT296ApplyResult, error) {
		m, err := ParseMT296(replayDoc)
		if err != nil {
			t.Fatalf("ParseMT296: %v", err)
		}
		return l.ApplyMT296(m, "correspondent-bank")
	}()
	if err != nil {
		t.Fatalf("ApplyMT296 replay: %v", err)
	}
	if !res.Duplicate {
		t.Error("duplicate = false on replay, want true")
	}
	if res.Reference != first.Reference || res.RelatedReference != first.RelatedReference ||
		len(res.Answers) != len(first.Answers) || res.Answers[0] != first.Answers[0] {
		t.Errorf("replay = %+v, want the stored outcome", res)
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt296_answer_received") != 1 {
		t.Errorf("mt296_answer_received count = %d, want 1 (no second marker)", countOp(events, "mt296_answer_received"))
	}
}

func TestApplyMT296_ConflictOnDifferentTerms(t *testing.T) {
	l := mt296InboundLedger(t, mustAuditLog(t))

	applyInboundMT296(t, l, mt296InboundDoc("ANS-296-13", mt296AnsweredRef(),
		[]string{"/CNCL/"}, nil, []string{"CONFIRMED"}), "correspondent-bank")
	// Same :20:, different :79: — refused, never silently
	// overwritten.
	m, err := ParseMT296(mt296InboundDoc("ANS-296-13", mt296AnsweredRef(),
		[]string{"/CNCL/"}, nil, []string{"REJECTED INSTEAD"}))
	if err != nil {
		t.Fatalf("ParseMT296: %v", err)
	}
	if _, err := l.ApplyMT296(m, "correspondent-bank"); !errors.Is(err, ErrMT296MsgIDConflict) {
		t.Errorf("different :79:: err = %v, want ErrMT296MsgIDConflict", err)
	}
	// Same :20:, different :76: — the same conflict.
	m, err = ParseMT296(mt296InboundDoc("ANS-296-13", mt296AnsweredRef(),
		[]string{"/RJCR/"}, nil, []string{"CONFIRMED"}))
	if err != nil {
		t.Fatalf("ParseMT296: %v", err)
	}
	if _, err := l.ApplyMT296(m, "correspondent-bank"); !errors.Is(err, ErrMT296MsgIDConflict) {
		t.Errorf("different :76:: err = %v, want ErrMT296MsgIDConflict", err)
	}
}

func TestApplyMT296_GetterReturnsAnswerVerbatim(t *testing.T) {
	l := mt296InboundLedger(t, mustAuditLog(t))

	applyInboundMT296(t, l, mt296InboundDoc("ANS-296-14", mt296AnsweredRef(),
		[]string{"/CNCL/", "SECOND LINE"},
		[]string{"NOTE ONE", "NOTE TWO"},
		[]string{"DESC ONE"}), "correspondent-bank")
	stored, ok := l.GetMT296Received("ANS-296-14")
	if !ok {
		t.Fatal("GetMT296Received = false, want the stored answer")
	}
	if len(stored.Answers) != 2 || stored.Answers[1] != "SECOND LINE" ||
		len(stored.Narrative) != 2 || stored.Narrative[0] != "NOTE ONE" ||
		len(stored.Description) != 1 || stored.Description[0] != "DESC ONE" {
		t.Errorf("stored = %+v, want the answer verbatim", stored)
	}
	// The getter returns copies: mutating them must not rewrite
	// the registered answer.
	stored.Answers[0] = "MUTATED"
	stored.Narrative[0] = "MUTATED"
	stored.Description[0] = "MUTATED"
	again, _ := l.GetMT296Received("ANS-296-14")
	if again.Answers[0] != "/CNCL/" || again.Narrative[0] != "NOTE ONE" || again.Description[0] != "DESC ONE" {
		t.Errorf("re-read = %+v, want the unmutated answer", again)
	}
}

func TestApplyMT296_RejectsInvalidMessage(t *testing.T) {
	l := mt296InboundLedger(t, mustAuditLog(t))

	// A hand-built MT296 with no :76: answers fails the shared
	// validate gate — the parse gate and the apply gate share
	// one check.
	m := MT296{
		SenderLT:             "CITIUS33",
		ReceiverLT:           "NWBKGB2L",
		TransactionReference: "ANS-296-15",
		RelatedReference:     mt296AnsweredRef(),
	}
	if _, err := l.ApplyMT296(m, "correspondent-bank"); err == nil {
		t.Error("ApplyMT296 with no :76: succeeded, want an error")
	}
	if _, ok := l.GetMT296Received("ANS-296-15"); ok {
		t.Error("GetMT296Received = true for a refused answer, want false")
	}
}
