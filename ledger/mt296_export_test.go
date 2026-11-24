package ledger

import (
	"errors"
	"strings"
	"testing"
)

func mt296Base(t *testing.T) MT296 {
	t.Helper()
	return MT296{
		SenderLT:             "BANKUS33",
		ReceiverLT:           "BANKDEFF",
		TransactionReference: "ANS-296-1",
		RelatedReference:     "CXL-296-292",
		Answers:              []string{"/CNCL/", "CANCELLATION CONFIRMED 261124"},
		Narrative:            []string{"OPERATOR NOTE"},
		Description:          []string{"BANK CONFIRMS THE NOTIFICATION", "IS WITHDRAWN"},
	}
}

// mt296ExportLedger seeds a ledger with both :21: anchors the
// export can answer: an inbound MT292 cancellation request
// ("CXL-296-292", cancelling MT210 notification "sched-1") and
// an inbound MT295 query ("QRY-296-295", asking about MT210
// notification "NTF-296-295"). It returns the ledger, the audit
// log, and the two anchors.
func mt296ExportLedger(t *testing.T) (*Ledger, *AuditLog, string, string) {
	t.Helper()
	l, al := mt210InboundFixture(t)
	registerInboundMT210ForCancel(t, l, "sched-1", "sched-1/run/0", "261112")
	applyInboundMT292(t, l, mt292InboundDoc("CXL-296-292", "sched-1", "210261110",
		"Client withdrew the order"), "ops")
	registerInboundMT210ForQuery(t, l, "NTF-296-295")
	applyInboundMT295(t, l, mt295InboundDoc("QRY-296-295", "NTF-296-295",
		[]string{"STAT"}, nil, nil), "ops")
	return l, al, "CXL-296-292", "QRY-296-295"
}

func mt296ExportOptions(t *testing.T) MT296ExportOptions {
	t.Helper()
	return MT296ExportOptions{
		SenderLT:    "BANKUS33",
		ReceiverLT:  "BANKDEFF",
		Reference:   "ANS-296-0001",
		Answers:     []string{"/CNCL/", "CANCELLATION CONFIRMED 261124"},
		Narrative:   []string{"OPERATOR NOTE"},
		Description: []string{"BANK CONFIRMS THE NOTIFICATION"},
	}
}

func TestExportMT296_HappyPath(t *testing.T) {
	m := mt296Base(t)
	msg, err := ExportMT296(m)
	if err != nil {
		t.Fatalf("ExportMT296: %v", err)
	}
	want := "{1:F01BANKUS33XXX0000000000}\n" +
		"{2:I296BANKDEFFXXXU3003}\n" +
		"{3:{108:ANS-296-1}}\n" +
		"{4:\n" +
		":20:ANS-296-1\n" +
		":21:CXL-296-292\n" +
		":76:/CNCL/\n" +
		"CANCELLATION CONFIRMED 261124\n" +
		":77A:OPERATOR NOTE\n" +
		":79:BANK CONFIRMS THE NOTIFICATION\n" +
		"IS WITHDRAWN\n" +
		"-}"
	if msg != want {
		t.Errorf("message = %q, want %q", msg, want)
	}
	if strings.Contains(msg, "{5:") {
		t.Error("block 5 must be omitted: CHK is network-assigned")
	}
	if strings.Contains(msg, ":32A:") {
		t.Error("an MT296 must never carry a value field")
	}
	// Round-trip: parse returns the same business fields.
	got, err := ParseMT296(msg)
	if err != nil {
		t.Fatalf("ParseMT296: %v", err)
	}
	if got.TransactionReference != "ANS-296-1" || got.RelatedReference != "CXL-296-292" ||
		len(got.Answers) != 2 || got.Answers[0] != "/CNCL/" || got.Answers[1] != "CANCELLATION CONFIRMED 261124" ||
		len(got.Narrative) != 1 || got.Narrative[0] != "OPERATOR NOTE" ||
		len(got.Description) != 2 || got.Description[1] != "IS WITHDRAWN" {
		t.Errorf("round-trip = %+v", got)
	}
	// Repeat exports are byte-identical.
	again, err := ExportMT296(m)
	if err != nil {
		t.Fatalf("ExportMT296 repeat: %v", err)
	}
	if again != msg {
		t.Error("repeat export is not byte-identical")
	}
}

func TestExportMT296_NoNarrativeNoDescription(t *testing.T) {
	m := mt296Base(t)
	m.Narrative = nil
	m.Description = nil
	msg, err := ExportMT296(m)
	if err != nil {
		t.Fatalf("ExportMT296: %v", err)
	}
	if strings.Contains(msg, ":77A:") {
		t.Error(":77A: must be omitted when the narrative is empty")
	}
	if strings.Contains(msg, ":79:") {
		t.Error(":79: must be omitted when the description is empty")
	}
	got, err := ParseMT296(strings.ReplaceAll(msg, "\n", "\r\n") + "\n")
	if err != nil {
		t.Fatalf("ParseMT296 CRLF: %v", err)
	}
	if len(got.Narrative) != 0 || len(got.Description) != 0 || len(got.Answers) != 2 {
		t.Errorf("CRLF parse = %+v", got)
	}
}

func TestExportMT296ForAnswered_AnswersMT292(t *testing.T) {
	l, al, mt292Ref, _ := mt296ExportLedger(t)
	before := l.Version()
	entriesBefore := len(l.Entries())

	opts := mt296ExportOptions(t)
	res, err := l.ExportMT296ForAnswered(mt292Ref, opts, "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT296ForAnswered: %v", err)
	}
	if res.Duplicate {
		t.Error("duplicate = true on first export")
	}
	if res.Reference != "ANS-296-0001" {
		t.Errorf("reference = %q, want ANS-296-0001", res.Reference)
	}
	if res.RelatedReference != mt292Ref {
		t.Errorf("related_reference = %q, want %q", res.RelatedReference, mt292Ref)
	}
	if res.AnsweredKind != MT296AnsweredMT292 {
		t.Errorf("answered_kind = %q, want mt292", res.AnsweredKind)
	}
	// The answers are the operator's lines, verbatim — the
	// published /CNCL/ code is text, not a ledger decision.
	for _, want := range []string{
		":20:ANS-296-0001",
		":21:" + mt292Ref,
		":76:/CNCL/",
		"CANCELLATION CONFIRMED 261124",
		":77A:OPERATOR NOTE",
		":79:BANK CONFIRMS THE NOTIFICATION",
		"{2:I296BANKDEFFXXXU3003}",
	} {
		if !strings.Contains(res.Message, want) {
			t.Errorf("message missing %q:\n%s", want, res.Message)
		}
	}
	// Rendering is read-only: no version bump, no journal rows,
	// and the cancelled receipt is not flipped by the answer —
	// the answer reports the operator's decision, it is not the
	// cancellation itself.
	if l.Version() != before {
		t.Errorf("version = %d, want %d (read-only, no bump)", l.Version(), before)
	}
	if len(l.Entries()) != entriesBefore {
		t.Errorf("entries = %d, want %d (no journal rows)", len(l.Entries()), entriesBefore)
	}
	receipt, ok := l.GetExpectedReceipt("sched-1")
	if !ok || !receipt.Cancelled || receipt.CancelMsgID != "CXL-296-292" ||
		receipt.CancelReason != "Client withdrew the order" {
		t.Errorf("receipt = %+v/%v, want the cancelled receipt untouched by the export", receipt, ok)
	}
	// The stored export round-trips through the registry.
	stored, ok := l.GetMT296Export("ANS-296-0001")
	if !ok {
		t.Fatal("GetMT296Export = false, want the stored export")
	}
	if stored.Message != res.Message || stored.AnsweredKind != MT296AnsweredMT292 {
		t.Errorf("stored export = %+v, want the rendered message, kind mt292", stored)
	}
	if _, ok := l.GetMT296Export("no-such-export"); ok {
		t.Error("GetMT296Export unknown = true, want false")
	}
	// Audit: exactly one export marker, with the exact details —
	// the :20: on the durable record, and no accounts: an answer
	// touches no ledger account.
	events := readAuditOps(t, al)
	if countOp(events, "mt296_answer_exported") != 1 {
		t.Errorf("mt296_answer_exported count = %d, want 1", countOp(events, "mt296_answer_exported"))
	}
	ev := findOp(events, "mt296_answer_exported")
	if ev == nil {
		t.Fatal("mt296_answer_exported event missing")
	}
	if ev.Actor != "ledger-operator" {
		t.Errorf("actor = %q, want ledger-operator (ExportMT296ForAnswered)", ev.Actor)
	}
	if ev.TraceID != "ANS-296-0001" {
		t.Errorf("trace = %q, want ANS-296-0001", ev.TraceID)
	}
	if ev.Details["msg_id"] != "ANS-296-0001" || ev.Details["related_reference"] != mt292Ref ||
		ev.Details["answered_kind"] != "mt292" || ev.Details["sender_lt"] != "BANKUS33XXX" {
		t.Errorf("details = %v, want msg/related/kind/sender", ev.Details)
	}
	if len(ev.Accounts) != 0 {
		t.Errorf("accounts = %v, want none (evidence-only export)", ev.Accounts)
	}
	// The answer's :76: lines parse back as the operator's
	// verbatim lines.
	parsed, err := ParseMT296(res.Message)
	if err != nil {
		t.Fatalf("ParseMT296(rendered): %v", err)
	}
	if len(parsed.Answers) != 2 || parsed.Answers[0] != "/CNCL/" ||
		parsed.Answers[1] != "CANCELLATION CONFIRMED 261124" {
		t.Errorf("parsed answers = %v, want the operator's lines verbatim", parsed.Answers)
	}
	if len(parsed.Description) != 1 || parsed.Description[0] != "BANK CONFIRMS THE NOTIFICATION" {
		t.Errorf("parsed description = %v, want the :79: line verbatim", parsed.Description)
	}
}

func TestExportMT296ForAnswered_AnswersMT295(t *testing.T) {
	l, _, _, mt295Ref := mt296ExportLedger(t)
	opts := mt296ExportOptions(t)
	opts.Reference = "ANS-296-0002"
	opts.Answers = []string{"/RJCR/", "CANCELLATION REFUSED 261124"}
	opts.Narrative = nil
	opts.Description = []string{"QUERY ANSWERED IN FULL"}
	res, err := l.ExportMT296ForAnswered(mt295Ref, opts, "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT296ForAnswered: %v", err)
	}
	if res.AnsweredKind != MT296AnsweredMT295 {
		t.Errorf("answered_kind = %q, want mt295", res.AnsweredKind)
	}
	for _, want := range []string{
		":20:ANS-296-0002",
		":21:" + mt295Ref,
		":76:/RJCR/",
		"CANCELLATION REFUSED 261124",
		":79:QUERY ANSWERED IN FULL",
	} {
		if !strings.Contains(res.Message, want) {
			t.Errorf("message missing %q:\n%s", want, res.Message)
		}
	}
	if strings.Contains(res.Message, ":77A:") {
		t.Error(":77A: must be omitted when the narrative is empty")
	}
	stored, ok := l.GetMT296Export("ANS-296-0002")
	if !ok || stored.AnsweredKind != MT296AnsweredMT295 || stored.Message != res.Message {
		t.Errorf("stored export = %+v/%v, want the rendered message, kind mt295", stored, ok)
	}
}

func TestExportMT296ForAnswered_UnknownReferenceParks(t *testing.T) {
	l, al, _, _ := mt296ExportLedger(t)
	opts := mt296ExportOptions(t)
	res, err := l.ExportMT296ForAnswered("NO-SUCH-REF", opts, "ledger-operator")
	if !errors.Is(err, ErrMT296AnsweredNotFound) {
		t.Errorf("err = %v, want ErrMT296AnsweredNotFound", err)
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
	if rec.Instruction.ID != "NO-SUCH-REF" {
		t.Errorf("repair instruction id = %q, want the answered :21:", rec.Instruction.ID)
	}
	if rec.Instruction.Destination != "mt296-export:ANS-296-0001" {
		t.Errorf("repair destination = %q, want mt296-export:ANS-296-0001", rec.Instruction.Destination)
	}
	// No export was recorded: nothing to replay, and no answer
	// marker event — the repair_enqueued event is the record.
	if _, ok := l.GetMT296Export("ANS-296-0001"); ok {
		t.Error("GetMT296Export = true for a parked export, want false")
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt296_answer_exported") != 0 {
		t.Errorf("mt296_answer_exported count = %d, want 0", countOp(events, "mt296_answer_exported"))
	}
	if countOp(events, "repair_enqueued") != 1 {
		t.Errorf("repair_enqueued count = %d, want 1", countOp(events, "repair_enqueued"))
	}
}

func TestExportMT296ForAnswered_OutboundMT292ShapeAlsoParks(t *testing.T) {
	// A :21: with the shape of our outbound MT292's :20: names
	// no inbound record — the ledger holds no state for outbound
	// messages — so it parks like any other unknown reference.
	l, _, _, _ := mt296ExportLedger(t)
	opts := mt296ExportOptions(t)
	res, err := l.ExportMT296ForAnswered(mt296AnsweredRef(), opts, "ledger-operator")
	if !errors.Is(err, ErrMT296AnsweredNotFound) {
		t.Errorf("err = %v, want ErrMT296AnsweredNotFound", err)
	}
	if res.RepairID == "" {
		t.Error("repair_id empty, want the parked repair record")
	}
}

func TestExportMT296ForAnswered_ReplaySameReferenceNoop(t *testing.T) {
	l, al, mt292Ref, mt295Ref := mt296ExportLedger(t)
	opts := mt296ExportOptions(t)
	first, err := l.ExportMT296ForAnswered(mt292Ref, opts, "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT296ForAnswered: %v", err)
	}
	// Same :20:, same terms: an idempotent no-op returning the
	// stored message, with no second audit event.
	res, err := l.ExportMT296ForAnswered(mt292Ref, opts, "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT296ForAnswered replay: %v", err)
	}
	if !res.Duplicate {
		t.Error("duplicate = false on replay, want true")
	}
	if res.Message != first.Message {
		t.Errorf("replay message differs:\nfirst:\n%s\nsecond:\n%s", first.Message, res.Message)
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt296_answer_exported") != 1 {
		t.Errorf("mt296_answer_exported count = %d, want 1 after replay", countOp(events, "mt296_answer_exported"))
	}
	// Same :20:, different LTs: envelope, not terms — the stored
	// message is still returned, duplicate set.
	otherLT := opts
	otherLT.SenderLT = "CHASUS33"
	res, err = l.ExportMT296ForAnswered(mt292Ref, otherLT, "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT296ForAnswered replay (other LT): %v", err)
	}
	if !res.Duplicate || res.Message != first.Message {
		t.Errorf("replay under other LTs = %+v, want the stored message, duplicate", res)
	}
	// Same :20:, different answers: a conflict, never a silent
	// overwrite.
	otherAnswers := opts
	otherAnswers.Answers = []string{"/PDCR/", "CANCELLATION PENDING 261124"}
	if _, err := l.ExportMT296ForAnswered(mt292Ref, otherAnswers, "ledger-operator"); !errors.Is(err, ErrMT296MsgIDConflict) {
		t.Errorf("different answers: err = %v, want ErrMT296MsgIDConflict", err)
	}
	// Same :20:, different answered reference: a conflict, never
	// a silent overwrite.
	if _, err := l.ExportMT296ForAnswered(mt295Ref, opts, "ledger-operator"); !errors.Is(err, ErrMT296MsgIDConflict) {
		t.Errorf("different answered reference: err = %v, want ErrMT296MsgIDConflict", err)
	}
	// An invalid :20: is refused before any lookup: the reference
	// gate runs first.
	badRef := opts
	badRef.Reference = "ANS-296-0001-EXTRA"
	if _, err := l.ExportMT296ForAnswered(mt292Ref, badRef, "ledger-operator"); !errors.Is(err, ErrMT296Reference) {
		t.Errorf("overlong :20:: err = %v, want ErrMT296Reference", err)
	}
}

func TestExportMT296ForAnswered_AnswersValidation(t *testing.T) {
	l, _, mt292Ref, _ := mt296ExportLedger(t)
	// Empty :76: is a malformed export: the answer is the
	// operator's statement, and an answer with no answer lines
	// is no answer.
	empty := mt296ExportOptions(t)
	empty.Answers = nil
	if _, err := l.ExportMT296ForAnswered(mt292Ref, empty, "ledger-operator"); !errors.Is(err, ErrMT296Answers) {
		t.Errorf("empty :76:: err = %v, want ErrMT296Answers", err)
	}
	// Over 6 answer lines: refused.
	seven := mt296ExportOptions(t)
	seven.Answers = []string{"A1", "A2", "A3", "A4", "A5", "A6", "A7"}
	if _, err := l.ExportMT296ForAnswered(mt292Ref, seven, "ledger-operator"); !errors.Is(err, ErrMT296Answers) {
		t.Errorf("7 :76: lines: err = %v, want ErrMT296Answers", err)
	}
	// One answer line over 35 characters: refused.
	longLine := mt296ExportOptions(t)
	longLine.Answers = []string{strings.Repeat("x", 36)}
	if _, err := l.ExportMT296ForAnswered(mt292Ref, longLine, "ledger-operator"); !errors.Is(err, ErrMT296Answers) {
		t.Errorf("36-char :76: line: err = %v, want ErrMT296Answers", err)
	}
	// Over 20 narrative lines: refused.
	longNarr := mt296ExportOptions(t)
	longNarr.Narrative = make([]string, 21)
	for i := range longNarr.Narrative {
		longNarr.Narrative[i] = "NOTE"
	}
	if _, err := l.ExportMT296ForAnswered(mt292Ref, longNarr, "ledger-operator"); !errors.Is(err, ErrMT296Narrative) {
		t.Errorf("21 :77A: lines: err = %v, want ErrMT296Narrative", err)
	}
	// Over 50 description lines: refused.
	longDesc := mt296ExportOptions(t)
	longDesc.Description = make([]string, 51)
	for i := range longDesc.Description {
		longDesc.Description[i] = "DESC"
	}
	if _, err := l.ExportMT296ForAnswered(mt292Ref, longDesc, "ledger-operator"); !errors.Is(err, ErrMT296Description) {
		t.Errorf("51 :79: lines: err = %v, want ErrMT296Description", err)
	}
}
