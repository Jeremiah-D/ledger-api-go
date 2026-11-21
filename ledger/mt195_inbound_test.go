package ledger

import (
	"errors"
	"strings"
	"testing"
)

// mt195Doc renders one inbound SWIFT FIN MT195 query message: ref
// as :20:, payoutID as :21:, queries as the :75: coded question
// (a question containing "\n" spans lines), and narrative as the
// :79: description (likewise multi-line). An empty queries omits
// :75: entirely; an empty narrative omits :79: entirely.
func mt195Doc(ref, payoutID, queries, narrative string) string {
	var sb strings.Builder
	sb.WriteString("{1:F01BANKUS33XXX0000000000}\n")
	sb.WriteString("{2:I195BANKDEFFXXXXU3003}\n")
	sb.WriteString("{3:{108:" + ref + "}}\n")
	sb.WriteString("{4:\n")
	sb.WriteString(":20:" + ref + "\n")
	sb.WriteString(":21:" + payoutID + "\n")
	if queries != "" {
		sb.WriteString(":75:" + queries + "\n")
	}
	if narrative != "" {
		sb.WriteString(":79:" + narrative + "\n")
	}
	sb.WriteString("-}")
	return sb.String()
}

// mt195DocForPayout renders the MT195 querying submitted payout
// p: the :20: is ref, the :75: asks for the message status, the
// :79: carries the question in words.
func mt195DocForPayout(p PayoutInstruction, ref string) string {
	return mt195Doc(ref, p.ID, "STAT", "Please confirm status of payment")
}

// applyMT195Doc parses doc and applies it, failing the test on
// parse errors: the gates under test are the apply gates, not the
// parse gates.
func applyMT195Doc(t *testing.T, l *Ledger, doc string) (MT195ApplyResult, error) {
	t.Helper()
	m, err := ParseMT195(doc)
	if err != nil {
		t.Fatalf("ParseMT195: %v", err)
	}
	return l.ApplyMT195(m, "correspondent-bank")
}

// mt195ApplyLedger seeds a payout ledger with an audit log, the
// fixture for the MT195 apply tests.
func mt195ApplyLedger(t *testing.T, al *AuditLog) *Ledger {
	t.Helper()
	return seedPayoutLedger(t, WithAuditLog(al))
}

func TestParseMT195_RoundTrip(t *testing.T) {
	l := mt103TestLedger(t)
	p := mt103SubmittedPayout(t, l, "po-mt195-1", 250_000)

	doc := mt195DocForPayout(p, "MT195QUERY00001")
	m, err := ParseMT195(doc)
	if err != nil {
		t.Fatalf("ParseMT195: %v", err)
	}
	if m.SenderLT != "BANKUS33XXX" || m.ReceiverLT != "BANKDEFFXXXX" {
		t.Errorf("LTs = %q/%q, want BANKUS33XXX/BANKDEFFXXXX", m.SenderLT, m.ReceiverLT)
	}
	if m.Reference != "MT195QUERY00001" {
		t.Errorf(":20: = %q, want MT195QUERY00001", m.Reference)
	}
	if m.RelatedReference != p.ID {
		t.Errorf(":21: = %q, want %q", m.RelatedReference, p.ID)
	}
	if m.Queries != "STAT" {
		t.Errorf(":75: = %q, want STAT", m.Queries)
	}
	if m.Narrative != "Please confirm status of payment" {
		t.Errorf(":79: = %q, want the question verbatim", m.Narrative)
	}
	// A multi-line :75: and :79: round-trip line for line.
	doc = mt195Doc("MT195QUERY00001", p.ID, "STAT\nPlease confirm receipt", "Please confirm status\nof payment sent")
	m, err = ParseMT195(doc)
	if err != nil {
		t.Fatalf("ParseMT195(multi-line): %v", err)
	}
	if m.Queries != "STAT\nPlease confirm receipt" {
		t.Errorf(":75: = %q, want two lines verbatim", m.Queries)
	}
	if m.Narrative != "Please confirm status\nof payment sent" {
		t.Errorf(":79: = %q, want two lines verbatim", m.Narrative)
	}
	// :79: alone, without :75:, parses: the narrative carries
	// the question.
	doc = mt195Doc("MT195QUERY00001", p.ID, "", "Please confirm status of payment")
	m, err = ParseMT195(doc)
	if err != nil {
		t.Fatalf("ParseMT195(:79: only): %v", err)
	}
	if m.Queries != "" || m.Narrative != "Please confirm status of payment" {
		t.Errorf(":75:/:79: = %q/%q, want empty/narrative", m.Queries, m.Narrative)
	}
}

func TestParseMT195_RejectsBadBlockStructure(t *testing.T) {
	l := mt103TestLedger(t)
	p := mt103SubmittedPayout(t, l, "po-mt195-1", 250_000)
	good := mt195DocForPayout(p, "MT195QUERY00001")

	cases := []struct {
		name string
		doc  string
	}{
		{"block 2 is MT202", strings.Replace(good, "{2:I195", "{2:I202", 1)},
		{"missing block 3", strings.Replace(good, "{3:{108:MT195QUERY00001}}\n", "", 1)},
		{"missing {4:", strings.Replace(good, "{4:\n", "", 1)},
		{"missing trailer", strings.TrimSuffix(good, "-}")},
		{"field order swapped", strings.Replace(good,
			":20:MT195QUERY00001\n:21:po-mt195-1\n",
			":21:po-mt195-1\n:20:MT195QUERY00001\n", 1)},
		{"empty :20:", strings.Replace(good, ":20:MT195QUERY00001\n", ":20:\n", 1)},
		{"neither :75: nor :79:", mt195Doc("MT195QUERY00001", p.ID, "", "")},
		{":79: before :75:", strings.Replace(good,
			":75:STAT\n:79:Please confirm status of payment\n",
			":79:Please confirm status of payment\n:75:STAT\n", 1)},
		{"too few lines", "{1:F01BANKUS33XXX0000000000}"},
		{"empty document", "  "},
	}
	for _, tc := range cases {
		if _, err := ParseMT195(tc.doc); err == nil {
			t.Errorf("%s: parsed without error, want refusal", tc.name)
		}
	}
	// The no-question case is a field error, not a framing
	// error: the document is well-formed, it just asks nothing.
	if _, err := ParseMT195(mt195Doc("MT195QUERY00001", p.ID, "", "")); !errors.Is(err, ErrMT195Field) {
		t.Errorf("no question: err = %v, want ErrMT195Field", err)
	}
}

func TestParseMT195_RejectsUnknownField(t *testing.T) {
	l := mt103TestLedger(t)
	p := mt103SubmittedPayout(t, l, "po-mt195-1", 250_000)
	good := mt195DocForPayout(p, "MT195QUERY00001")

	// An optional :72: sender-to-receiver field is out of the
	// core set: refused, not parsed with the field quietly
	// discarded.
	with72 := strings.Replace(good, "-}", ":72:/BNK/Please hurry\n-}", 1)
	if _, err := ParseMT195(with72); !errors.Is(err, ErrMT195Field) {
		t.Errorf(":72: field: err = %v, want ErrMT195Field", err)
	}
	// A `:` line inside the :79: narrative run is an unknown
	// field, not a continuation.
	withColon := strings.Replace(good, ":79:Please confirm status of payment\n", ":79:Please confirm status of payment\n:99:extra\n", 1)
	if _, err := ParseMT195(withColon); !errors.Is(err, ErrMT195Field) {
		t.Errorf(":99: in narrative: err = %v, want ErrMT195Field", err)
	}
	// :77A: (new priority) is outside the core set: a priority
	// change is a different action from a status query.
	with77A := strings.Replace(good, ":75:STAT\n", ":75:STAT\n:77A:/PRTY/HIGH\n", 1)
	if _, err := ParseMT195(with77A); !errors.Is(err, ErrMT195Field) {
		t.Errorf(":77A: field: err = %v, want ErrMT195Field", err)
	}
	// :75: is 6*35x: a seventh line is refused.
	if _, err := ParseMT195(mt195Doc("MT195QUERY00001", p.ID, "L1\nL2\nL3\nL4\nL5\nL6\nL7", "")); !errors.Is(err, ErrMT195Field) {
		t.Errorf("7-line :75:: err = %v, want ErrMT195Field", err)
	}
	// :79: is 50*35x: a fifty-first line is refused.
	many := strings.Repeat("line\n", 50) + "last"
	if _, err := ParseMT195(mt195Doc("MT195QUERY00001", p.ID, "", many)); !errors.Is(err, ErrMT195Field) {
		t.Errorf("51-line :79:: err = %v, want ErrMT195Field", err)
	}
	// A :75: line over 35 characters is refused.
	if _, err := ParseMT195(mt195Doc("MT195QUERY00001", p.ID, "STAT plus a very long continuation line here", "")); !errors.Is(err, ErrMT195Field) {
		t.Errorf("over-long :75: line: err = %v, want ErrMT195Field", err)
	}
}

func TestParseMT195_AcceptsCRLF(t *testing.T) {
	l := mt103TestLedger(t)
	p := mt103SubmittedPayout(t, l, "po-mt195-1", 250_000)
	doc := mt195DocForPayout(p, "MT195QUERY00001")

	crlf := strings.ReplaceAll(doc, "\n", "\r\n")
	if _, err := ParseMT195(crlf); err != nil {
		t.Errorf("CRLF: %v", err)
	}
	// A trailing newline after the trailer is tolerated.
	if _, err := ParseMT195(doc + "\n"); err != nil {
		t.Errorf("trailing newline: %v", err)
	}
}

func TestApplyMT195_ResolvesPayoutStatus(t *testing.T) {
	al := mustAuditLog(t)
	l := mt195ApplyLedger(t, al)
	p := mt103SubmittedPayout(t, l, "po-mt195-1", 250_000)
	before := l.Version()

	res, err := applyMT195Doc(t, l, mt195DocForPayout(p, "MT195QUERY00001"))
	if err != nil {
		t.Fatalf("ApplyMT195: %v", err)
	}
	if res.Duplicate {
		t.Error("duplicate = true on first apply")
	}
	if res.Reference != "MT195QUERY00001" {
		t.Errorf("reference = %q, want MT195QUERY00001", res.Reference)
	}
	if res.PayoutID != p.ID {
		t.Errorf("payout_id = %q, want %q", res.PayoutID, p.ID)
	}
	// The answer is ledger facts: the payout is submitted, its
	// MT103 value date is the submit day, and its gpi track —
	// opened at draft time — is "tracked".
	if res.PayoutStatus != PayoutSubmitted {
		t.Errorf("payout_status = %q, want submitted", res.PayoutStatus)
	}
	if res.ValueDate != mt103ValueDate(p.SubmittedAt) {
		t.Errorf("value_date = %q, want %q", res.ValueDate, mt103ValueDate(p.SubmittedAt))
	}
	if res.GPIStatus != GPIStatusTracked {
		t.Errorf("gpi_status = %q, want tracked", res.GPIStatus)
	}
	// Answering is read-only: no version bump.
	if l.Version() != before {
		t.Errorf("version = %d, want %d (read-only, no bump)", l.Version(), before)
	}
	// Audit: exactly one inbound marker, with the exact
	// details — the :20: on the durable record.
	events := readAuditOps(t, al)
	if countOp(events, "mt195_query_received") != 1 {
		t.Errorf("mt195_query_received count = %d, want 1", countOp(events, "mt195_query_received"))
	}
	ev := findOp(events, "mt195_query_received")
	if ev == nil {
		t.Fatal("mt195_query_received event missing")
	}
	if ev.Actor != "correspondent-bank" {
		t.Errorf("actor = %q, want correspondent-bank (ApplyMT195)", ev.Actor)
	}
	if ev.TraceID != "MT195QUERY00001" {
		t.Errorf("trace = %q, want MT195QUERY00001", ev.TraceID)
	}
	if ev.Details["msg_id"] != "MT195QUERY00001" || ev.Details["related_reference"] != p.ID ||
		ev.Details["payout_id"] != p.ID || ev.Details["gpi_status"] != GPIStatusTracked ||
		ev.Details["value_date"] != mt103ValueDate(p.SubmittedAt) {
		t.Errorf("details = %v, want msg/related/payout/gpi/value-date", ev.Details)
	}
}

func TestApplyMT195_UnknownReferenceGoesToRepair(t *testing.T) {
	al := mustAuditLog(t)
	l := mt195ApplyLedger(t, al)
	p := mt103SubmittedPayout(t, l, "po-mt195-1", 250_000)
	_ = p

	res, err := applyMT195Doc(t, l, mt195Doc("MT195QUERY00001", "no-such", "STAT", "Please confirm status"))
	if !errors.Is(err, ErrMT103PayoutNotFound) {
		t.Errorf("err = %v, want ErrMT103PayoutNotFound", err)
	}
	// The parked result carries the repair ID alongside the
	// error: the query is visible to an operator, not dropped.
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
	if rec.Instruction.ID != "no-such" {
		t.Errorf("repair instruction id = %q, want the queried :21:", rec.Instruction.ID)
	}
	if rec.Instruction.Destination != "mt195-inbound:MT195QUERY00001" {
		t.Errorf("repair destination = %q, want mt195-inbound:MT195QUERY00001", rec.Instruction.Destination)
	}
	// No answer was recorded: nothing to replay, and no query
	// marker event — the repair_enqueued event is the record.
	if _, ok := l.GetMT195Query("MT195QUERY00001"); ok {
		t.Error("GetMT195Query = true for a parked query, want false")
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt195_query_received") != 0 {
		t.Errorf("mt195_query_received count = %d, want 0", countOp(events, "mt195_query_received"))
	}
	if countOp(events, "repair_enqueued") != 1 {
		t.Errorf("repair_enqueued count = %d, want 1", countOp(events, "repair_enqueued"))
	}
}

func TestApplyMT195_ReplaySameReferenceNoop(t *testing.T) {
	al := mustAuditLog(t)
	l := mt195ApplyLedger(t, al)
	p := mt103SubmittedPayout(t, l, "po-mt195-1", 250_000)
	doc := mt195DocForPayout(p, "MT195QUERY00001")
	if _, err := applyMT195Doc(t, l, doc); err != nil {
		t.Fatalf("ApplyMT195: %v", err)
	}
	// Same :20:, same terms: an idempotent no-op returning the
	// stored answer snapshot, with no second audit event.
	res, err := applyMT195Doc(t, l, doc)
	if err != nil {
		t.Fatalf("ApplyMT195 replay: %v", err)
	}
	if !res.Duplicate {
		t.Error("duplicate = false on replay, want true")
	}
	if res.PayoutStatus != PayoutSubmitted || res.ValueDate != mt103ValueDate(p.SubmittedAt) || res.GPIStatus != GPIStatusTracked {
		t.Errorf("replay answer = %+v, want the stored snapshot", res)
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt195_query_received") != 1 {
		t.Errorf("mt195_query_received count = %d, want 1 after replay", countOp(events, "mt195_query_received"))
	}
	// Same :20:, different question: a conflict, never a silent
	// overwrite.
	conflict := mt195Doc("MT195QUERY00001", p.ID, "DUPL", "Please confirm status of payment")
	if _, err := applyMT195Doc(t, l, conflict); !errors.Is(err, ErrMT195MsgIDConflict) {
		t.Errorf("conflict queries: err = %v, want ErrMT195MsgIDConflict", err)
	}
	// Same :20:, different payout: a conflict too.
	conflict = mt195Doc("MT195QUERY00001", "po-other", "STAT", "Please confirm status of payment")
	if _, err := applyMT195Doc(t, l, conflict); !errors.Is(err, ErrMT195MsgIDConflict) {
		t.Errorf("conflict payout: err = %v, want ErrMT195MsgIDConflict", err)
	}
	events = readAuditOps(t, al)
	if countOp(events, "mt195_query_received") != 1 {
		t.Errorf("mt195_query_received count = %d, want 1 after conflicts", countOp(events, "mt195_query_received"))
	}
}

func TestApplyMT195_DoesNotMoveFunds(t *testing.T) {
	al := mustAuditLog(t)
	l := mt195ApplyLedger(t, al)
	p := mt103SubmittedPayout(t, l, "po-mt195-1", 250_000)
	before := l.Version()
	trackBefore, ok := l.GetGPITrack(p.UETR)
	if !ok {
		t.Fatal("gpi track missing for a submitted payout")
	}

	if _, err := applyMT195Doc(t, l, mt195DocForPayout(p, "MT195QUERY00001")); err != nil {
		t.Fatalf("ApplyMT195: %v", err)
	}
	// The payout itself stays submitted, the version is
	// untouched, and the gpi history is exactly as it was: a
	// query advances nothing.
	if got, ok := l.GetPayout(p.ID); !ok || got.Status != PayoutSubmitted {
		t.Errorf("stored payout = %+v, want still submitted", got)
	}
	if l.Version() != before {
		t.Errorf("version = %d, want %d (read-only, no bump)", l.Version(), before)
	}
	trackAfter, ok := l.GetGPITrack(p.UETR)
	if !ok {
		t.Fatal("gpi track missing after query")
	}
	if len(trackAfter.History) != len(trackBefore.History) || trackAfter.Status != trackBefore.Status {
		t.Errorf("gpi track moved: %+v -> %+v, want unchanged", trackBefore, trackAfter)
	}
}

func TestApplyMT195_AnswerTracksLiveGPIStatus(t *testing.T) {
	al := mustAuditLog(t)
	l := mt195ApplyLedger(t, al)
	p := mt103SubmittedPayout(t, l, "po-mt195-1", 250_000)

	first, err := applyMT195Doc(t, l, mt195DocForPayout(p, "MT195QUERY00001"))
	if err != nil {
		t.Fatalf("ApplyMT195: %v", err)
	}
	if first.GPIStatus != GPIStatusTracked {
		t.Errorf("first gpi_status = %q, want tracked", first.GPIStatus)
	}
	// The bank's verdict moves the track; the next query under
	// a fresh :20: answers with the live status.
	if _, err := l.AdvanceGPI(p.UETR, GPIStatusACSP, "", "correspondent-bank"); err != nil {
		t.Fatalf("AdvanceGPI: %v", err)
	}
	second, err := applyMT195Doc(t, l, mt195DocForPayout(p, "MT195QUERY00002"))
	if err != nil {
		t.Fatalf("ApplyMT195 second: %v", err)
	}
	if second.GPIStatus != GPIStatusACSP {
		t.Errorf("second gpi_status = %q, want ACSP (live)", second.GPIStatus)
	}
	// The first :20: still replays its stored snapshot: the
	// answer is a snapshot at apply time, not a live view.
	replay, err := applyMT195Doc(t, l, mt195DocForPayout(p, "MT195QUERY00001"))
	if err != nil {
		t.Fatalf("ApplyMT195 replay: %v", err)
	}
	if !replay.Duplicate || replay.GPIStatus != GPIStatusTracked {
		t.Errorf("replay = %+v, want duplicate with stored tracked status", replay)
	}
}

func TestMT195Query_GetByReference(t *testing.T) {
	al := mustAuditLog(t)
	l := mt195ApplyLedger(t, al)
	p := mt103SubmittedPayout(t, l, "po-mt195-1", 250_000)

	if _, ok := l.GetMT195Query("MT195QUERY00001"); ok {
		t.Error("GetMT195Query = true before any query, want false")
	}
	res, err := applyMT195Doc(t, l, mt195DocForPayout(p, "MT195QUERY00001"))
	if err != nil {
		t.Fatalf("ApplyMT195: %v", err)
	}
	got, ok := l.GetMT195Query("MT195QUERY00001")
	if !ok {
		t.Fatal("GetMT195Query = false after answering, want true")
	}
	if got.Reference != res.Reference || got.PayoutID != res.PayoutID ||
		got.PayoutStatus != res.PayoutStatus || got.ValueDate != res.ValueDate ||
		got.GPIStatus != res.GPIStatus {
		t.Errorf("GetMT195Query = %+v, want %+v", got, res)
	}
	if _, ok := l.GetMT195Query("MT195QUERY99999"); ok {
		t.Error("GetMT195Query = true for an unknown :20:, want false")
	}
}
