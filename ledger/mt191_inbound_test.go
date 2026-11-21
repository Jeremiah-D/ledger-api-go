package ledger

import (
	"errors"
	"strings"
	"testing"
)

// mt191Doc renders one inbound SWIFT FIN MT191 request for
// payment of charges message: ref as :20:, payoutID as :21:,
// amount32B as the full :32B: body ("CCC<integer>,<ff>"), and
// details as the :71B: text (a text containing "\n" spans
// lines). Empty details omits :71B: entirely — which ParseMT191
// must refuse, since :71B: is mandatory in MT191.
func mt191Doc(ref, payoutID, amount32B, details string) string {
	var sb strings.Builder
	sb.WriteString("{1:F01BANKUS33XXX0000000000}\n")
	sb.WriteString("{2:I191BANKDEFFXXXXU3003}\n")
	sb.WriteString("{3:{108:" + ref + "}}\n")
	sb.WriteString("{4:\n")
	sb.WriteString(":20:" + ref + "\n")
	sb.WriteString(":21:" + payoutID + "\n")
	sb.WriteString(":32B:" + amount32B + "\n")
	if details != "" {
		sb.WriteString(":71B:" + details + "\n")
	}
	sb.WriteString("-}")
	return sb.String()
}

// mt191DocForPayout renders the MT191 requesting a 15,00 charge
// in the payout's currency: the :20: is ref, the :71B: names the
// fee in words.
func mt191DocForPayout(p PayoutInstruction, ref string) string {
	return mt191Doc(ref, p.ID, "USD15,00", "Correspondent handling fee\nFor payment processing")
}

// applyMT191Doc parses doc and applies it, failing the test on
// parse errors: the gates under test are the apply gates, not the
// parse gates.
func applyMT191Doc(t *testing.T, l *Ledger, doc string) (MT191ApplyResult, error) {
	t.Helper()
	m, err := ParseMT191(doc)
	if err != nil {
		t.Fatalf("ParseMT191: %v", err)
	}
	return l.ApplyMT191(m, "correspondent-bank")
}

// mt191ApplyLedger seeds a payout ledger with an audit log, the
// fixture for the MT191 apply tests.
func mt191ApplyLedger(t *testing.T, al *AuditLog) *Ledger {
	t.Helper()
	return seedPayoutLedger(t, WithAuditLog(al))
}

func TestParseMT191_RoundTrip(t *testing.T) {
	l := mt103TestLedger(t)
	p := mt103SubmittedPayout(t, l, "po-mt191-1", 250_000)

	doc := mt191DocForPayout(p, "MT191REQ00000001")
	m, err := ParseMT191(doc)
	if err != nil {
		t.Fatalf("ParseMT191: %v", err)
	}
	if m.SenderLT != "BANKUS33XXX" || m.ReceiverLT != "BANKDEFFXXXX" {
		t.Errorf("LTs = %q/%q, want BANKUS33XXX/BANKDEFFXXXX", m.SenderLT, m.ReceiverLT)
	}
	if m.Reference != "MT191REQ00000001" || m.RelatedReference != p.ID {
		t.Errorf("references = %q/%q, want MT191REQ00000001/%q", m.Reference, m.RelatedReference, p.ID)
	}
	if m.Currency != "USD" || m.AmountCents != 1500 {
		t.Errorf("currency/amount = %q/%d, want USD/1500", m.Currency, m.AmountCents)
	}
	if m.Details != "Correspondent handling fee\nFor payment processing" {
		t.Errorf("details = %q, want the :71B: text verbatim", m.Details)
	}
	// A request carries no value date: the :32B: has no day.
	if _, err := ParseMT191(mt191Doc("MT191REQ00000001", p.ID, "USD0,01", "One cent fee")); err != nil {
		t.Errorf("one cent: err = %v, want nil", err)
	}
}

func TestParseMT191_RejectsBadBlockStructure(t *testing.T) {
	l := mt103TestLedger(t)
	p := mt103SubmittedPayout(t, l, "po-mt191-1", 250_000)
	good := mt191DocForPayout(p, "MT191REQ00000001")

	cases := []struct {
		name string
		doc  string
	}{
		{"block 2 is MT190", strings.Replace(good, "{2:I191", "{2:I190", 1)},
		{"missing block 3", strings.Replace(good, "{3:{108:MT191REQ00000001}}\n", "", 1)},
		{"missing {4:", strings.Replace(good, "{4:\n", "", 1)},
		{"missing trailer", strings.TrimSuffix(good, "-}")},
		{"field order swapped", strings.Replace(good,
			":20:MT191REQ00000001\n:21:po-mt191-1\n",
			":21:po-mt191-1\n:20:MT191REQ00000001\n", 1)},
		{"empty :20:", strings.Replace(good, ":20:MT191REQ00000001\n", ":20:\n", 1)},
		{"empty :21:", strings.Replace(good, ":21:po-mt191-1\n", ":21:\n", 1)},
		{"empty :32B:", strings.Replace(good, ":32B:USD15,00\n", ":32B:\n", 1)},
		{":32B: too short", mt191Doc("MT191REQ00000001", p.ID, "USD", "Correspondent handling fee")},
		{":32B: lowercase currency", mt191Doc("MT191REQ00000001", p.ID, "usd15,00", "Correspondent handling fee")},
		{":32B: amount without comma", mt191Doc("MT191REQ00000001", p.ID, "USD1500", "Correspondent handling fee")},
		{":32B: amount one fraction digit", mt191Doc("MT191REQ00000001", p.ID, "USD15,0", "Correspondent handling fee")},
		{"missing :71B:", mt191Doc("MT191REQ00000001", p.ID, "USD15,00", "")},
		{"too few lines", "{1:F01BANKUS33XXX0000000000}"},
		{"empty document", "  "},
	}
	for _, tc := range cases {
		if _, err := ParseMT191(tc.doc); err == nil {
			t.Errorf("%s: parsed without error, want refusal", tc.name)
		}
	}
	// The currency failure carries its own error identity, not
	// the framing error; the missing :71B: is a field error.
	if _, err := ParseMT191(mt191Doc("MT191REQ00000001", p.ID, "usd15,00", "Correspondent handling fee")); !errors.Is(err, ErrMT191Currency) {
		t.Errorf("lowercase currency: err = %v, want ErrMT191Currency", err)
	}
	if _, err := ParseMT191(mt191Doc("MT191REQ00000001", p.ID, "USD15,00", "")); !errors.Is(err, ErrMT191Field) {
		t.Errorf("missing :71B:: err = %v, want ErrMT191Field", err)
	}
}

func TestParseMT191_RejectsUnknownField(t *testing.T) {
	l := mt103TestLedger(t)
	p := mt103SubmittedPayout(t, l, "po-mt191-1", 250_000)
	good := mt191DocForPayout(p, "MT191REQ00000001")

	// :32A: (value date, currency, amount) is outside the core
	// set: a request has no value date, and the dated option
	// must not be silently accepted.
	with32A := strings.Replace(good, ":32B:USD15,00\n", ":32A:261121USD15,00\n", 1)
	if _, err := ParseMT191(with32A); !errors.Is(err, ErrMT191Field) {
		t.Errorf(":32A: field: err = %v, want ErrMT191Field", err)
	}
	// An optional :25: account identification is out of the
	// core set: the ledger knows the payout's account.
	with25 := strings.Replace(good, ":32B:", ":25:ACC-123\n:32B:", 1)
	if _, err := ParseMT191(with25); !errors.Is(err, ErrMT191Field) {
		t.Errorf(":25: field: err = %v, want ErrMT191Field", err)
	}
	// An optional :52a: ordering institution is out of the
	// core set: routing, not evidence.
	with52 := strings.Replace(good, ":71B:", ":52A:BANKDEFFXXXX\n:71B:", 1)
	if _, err := ParseMT191(with52); !errors.Is(err, ErrMT191Field) {
		t.Errorf(":52a: field: err = %v, want ErrMT191Field", err)
	}
	// An optional :72: sender-to-receiver field is out of the
	// core set: refused, not parsed with the field quietly
	// discarded.
	with72 := strings.Replace(good, "-}", ":72:/BNK/Please note\n-}", 1)
	if _, err := ParseMT191(with72); !errors.Is(err, ErrMT191Field) {
		t.Errorf(":72: field: err = %v, want ErrMT191Field", err)
	}
	// A `:` line inside the :71B: details run is an unknown
	// field, not a continuation.
	withColon := strings.Replace(good, ":71B:Correspondent handling fee\n", ":71B:Correspondent handling fee\n:99:extra\n", 1)
	if _, err := ParseMT191(withColon); !errors.Is(err, ErrMT191Field) {
		t.Errorf(":99: in details: err = %v, want ErrMT191Field", err)
	}
	// :71B: is 6*35x: a seventh line is refused.
	if _, err := ParseMT191(mt191Doc("MT191REQ00000001", p.ID, "USD15,00", "L1\nL2\nL3\nL4\nL5\nL6\nL7")); !errors.Is(err, ErrMT191Field) {
		t.Errorf("7-line :71B:: err = %v, want ErrMT191Field", err)
	}
	// A :71B: line over 35 characters is refused.
	if _, err := ParseMT191(mt191Doc("MT191REQ00000001", p.ID, "USD15,00", "Correspondent handling fee, this line is far too long")); !errors.Is(err, ErrMT191Field) {
		t.Errorf("over-long :71B: line: err = %v, want ErrMT191Field", err)
	}
}

func TestParseMT191_RejectsNonPositiveAmount(t *testing.T) {
	l := mt103TestLedger(t)
	p := mt103SubmittedPayout(t, l, "po-mt191-1", 250_000)

	if _, err := ParseMT191(mt191Doc("MT191REQ00000001", p.ID, "USD0,00", "Correspondent handling fee")); !errors.Is(err, ErrMT191Amount) {
		t.Errorf("zero amount: err = %v, want ErrMT191Amount", err)
	}
	// A negative amount cannot be written in the 15d shape:
	// the minus sign fails the digit check.
	if _, err := ParseMT191(mt191Doc("MT191REQ00000001", p.ID, "USD-5,00", "Correspondent handling fee")); !errors.Is(err, ErrMT191Amount) {
		t.Errorf("negative amount: err = %v, want ErrMT191Amount", err)
	}
	// One cent parses: the floor is zero, not one unit.
	if _, err := ParseMT191(mt191Doc("MT191REQ00000001", p.ID, "USD0,01", "Correspondent handling fee")); err != nil {
		t.Errorf("one cent: err = %v, want nil", err)
	}
}

func TestParseMT191_AcceptsCRLF(t *testing.T) {
	l := mt103TestLedger(t)
	p := mt103SubmittedPayout(t, l, "po-mt191-1", 250_000)
	doc := mt191DocForPayout(p, "MT191REQ00000001")

	crlf := strings.ReplaceAll(doc, "\n", "\r\n")
	if _, err := ParseMT191(crlf); err != nil {
		t.Errorf("CRLF: %v", err)
	}
	// A trailing newline after the trailer is tolerated.
	if _, err := ParseMT191(doc + "\n"); err != nil {
		t.Errorf("trailing newline: %v", err)
	}
}

func TestApplyMT191_RecordsChargeRequestPending(t *testing.T) {
	al := mustAuditLog(t)
	l := mt191ApplyLedger(t, al)
	p := mt103SubmittedPayout(t, l, "po-mt191-1", 250_000)
	before := l.Version()
	entriesBefore := len(l.Entries())

	res, err := applyMT191Doc(t, l, mt191DocForPayout(p, "MT191REQ00000001"))
	if err != nil {
		t.Fatalf("ApplyMT191: %v", err)
	}
	if res.Duplicate {
		t.Error("duplicate = true on first apply")
	}
	if res.Reference != "MT191REQ00000001" || res.PayoutID != p.ID {
		t.Errorf("reference/payout = %q/%q, want MT191REQ00000001/%q", res.Reference, res.PayoutID, p.ID)
	}
	r := res.Request
	if r.MsgID != "MT191REQ00000001" || r.PayoutID != p.ID || r.Currency != "USD" ||
		r.AmountCents != 1500 ||
		r.Details != "Correspondent handling fee\nFor payment processing" {
		t.Errorf("request = %+v, want the parsed fields verbatim", r)
	}
	// The request waits for an operator: pending, never paid
	// on arrival.
	if r.Status != PayoutChargeRequestStatusPending {
		t.Errorf("status = %q, want pending", r.Status)
	}
	if r.Actor != "correspondent-bank" || r.AppliedAt.IsZero() {
		t.Errorf("actor/applied_at = %q/%v, want correspondent-bank/a time", r.Actor, r.AppliedAt)
	}
	// The per-payout list holds the request, in apply order; a
	// second request under a fresh :20: appends after it.
	list, ok := l.GetPayoutChargeRequests(p.ID)
	if !ok || len(list) != 1 || list[0].MsgID != "MT191REQ00000001" {
		t.Errorf("GetPayoutChargeRequests = %v/%v, want one request MT191REQ00000001", list, ok)
	}
	if _, err := applyMT191Doc(t, l, mt191Doc("MT191REQ00000002", p.ID, "USD2,50", "SWIFT messaging fee")); err != nil {
		t.Fatalf("ApplyMT191 second: %v", err)
	}
	list, ok = l.GetPayoutChargeRequests(p.ID)
	if !ok || len(list) != 2 || list[1].MsgID != "MT191REQ00000002" || list[1].AmountCents != 250 {
		t.Errorf("GetPayoutChargeRequests after second = %v, want two requests in order", list)
	}
	// Recording is evidence plus a pending decision: no version
	// bump, no journal entry, and the payout itself stays
	// submitted.
	if l.Version() != before {
		t.Errorf("version = %d, want %d (evidence-only, no bump)", l.Version(), before)
	}
	if len(l.Entries()) != entriesBefore {
		t.Errorf("journal entries = %d, want %d (no entry posted)", len(l.Entries()), entriesBefore)
	}
	if got, ok := l.GetPayout(p.ID); !ok || got.Status != PayoutSubmitted {
		t.Errorf("stored payout = %+v, want still submitted", got)
	}
	// Audit: exactly one inbound marker per request, with the
	// exact details — the :20: on the durable record.
	events := readAuditOps(t, al)
	if countOp(events, "mt191_charges_requested") != 2 {
		t.Errorf("mt191_charges_requested count = %d, want 2", countOp(events, "mt191_charges_requested"))
	}
	ev := findOp(events, "mt191_charges_requested")
	if ev == nil {
		t.Fatal("mt191_charges_requested event missing")
	}
	if ev.Actor != "correspondent-bank" {
		t.Errorf("actor = %q, want correspondent-bank (ApplyMT191)", ev.Actor)
	}
	if ev.TraceID != "MT191REQ00000001" {
		t.Errorf("trace = %q, want MT191REQ00000001", ev.TraceID)
	}
	if ev.Details["msg_id"] != "MT191REQ00000001" || ev.Details["payout_id"] != p.ID ||
		ev.Details["currency"] != "USD" || ev.Details["amount_cents"] != float64(1500) ||
		ev.Details["details"] != "Correspondent handling fee\nFor payment processing" ||
		ev.Details["status"] != PayoutChargeRequestStatusPending {
		t.Errorf("details = %v, want msg/payout/currency/amount/details/status", ev.Details)
	}
}

func TestApplyMT191_UnknownPayoutGoesToRepair(t *testing.T) {
	al := mustAuditLog(t)
	l := mt191ApplyLedger(t, al)
	p := mt103SubmittedPayout(t, l, "po-mt191-1", 250_000)
	_ = p

	res, err := applyMT191Doc(t, l, mt191Doc("MT191REQ00000001", "no-such", "USD15,00", "Correspondent handling fee"))
	if !errors.Is(err, ErrMT103PayoutNotFound) {
		t.Errorf("err = %v, want ErrMT103PayoutNotFound", err)
	}
	// The parked result carries the repair ID alongside the
	// error: the request is visible to an operator, not dropped.
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
		t.Errorf("repair instruction id = %q, want the charged :21:", rec.Instruction.ID)
	}
	if rec.Instruction.Destination != "mt191-inbound:MT191REQ00000001" {
		t.Errorf("repair destination = %q, want mt191-inbound:MT191REQ00000001", rec.Instruction.Destination)
	}
	// No request was recorded: nothing to replay, and no charge
	// marker event — the repair_enqueued event is the record.
	if _, ok := l.GetPayoutChargeRequests("no-such"); ok {
		t.Error("GetPayoutChargeRequests = true for a parked request, want false")
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt191_charges_requested") != 0 {
		t.Errorf("mt191_charges_requested count = %d, want 0", countOp(events, "mt191_charges_requested"))
	}
	if countOp(events, "repair_enqueued") != 1 {
		t.Errorf("repair_enqueued count = %d, want 1", countOp(events, "repair_enqueued"))
	}
}

func TestApplyMT191_CurrencyMismatchGoesToRepair(t *testing.T) {
	al := mustAuditLog(t)
	l := mt191ApplyLedger(t, al)
	p := mt103SubmittedPayout(t, l, "po-mt191-1", 250_000)

	// The payout is USD; the request charges EUR. Well-formed,
	// but it disagrees with the ledger's fact about the payout:
	// parked, never converted.
	res, err := applyMT191Doc(t, l, mt191Doc("MT191REQ00000001", p.ID, "EUR15,00", "Correspondent handling fee"))
	if !errors.Is(err, ErrMT191CurrencyMismatch) {
		t.Errorf("err = %v, want ErrMT191CurrencyMismatch", err)
	}
	if res.RepairID == "" {
		t.Error("repair_id empty, want the parked repair record")
	}
	rec, ok := l.GetRepair(res.RepairID)
	if !ok {
		t.Fatalf("GetRepair(%q) = false, want the parked record", res.RepairID)
	}
	if rec.ReasonCode != RepairReasonInvalidFormat {
		t.Errorf("repair reason = %q, want invalid_format", rec.ReasonCode)
	}
	if rec.Instruction.Destination != "mt191-inbound:MT191REQ00000001" {
		t.Errorf("repair destination = %q, want mt191-inbound:MT191REQ00000001", rec.Instruction.Destination)
	}
	// No request was recorded for the payout, and no marker
	// event: the EUR charge is not evidence about this USD
	// payout.
	if _, ok := l.GetPayoutChargeRequests(p.ID); ok {
		t.Error("GetPayoutChargeRequests = true after a currency mismatch, want false")
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt191_charges_requested") != 0 {
		t.Errorf("mt191_charges_requested count = %d, want 0", countOp(events, "mt191_charges_requested"))
	}
	// The same :20: with the payout's own currency records
	// fine: the park holds no replay claim on the reference.
	if _, err := applyMT191Doc(t, l, mt191DocForPayout(p, "MT191REQ00000001")); err != nil {
		t.Fatalf("ApplyMT191 after park: %v", err)
	}
	if list, ok := l.GetPayoutChargeRequests(p.ID); !ok || len(list) != 1 {
		t.Errorf("GetPayoutChargeRequests after re-apply = %v, want one request", list)
	}
}

func TestApplyMT191_ReplaySameReferenceNoop(t *testing.T) {
	al := mustAuditLog(t)
	l := mt191ApplyLedger(t, al)
	p := mt103SubmittedPayout(t, l, "po-mt191-1", 250_000)
	doc := mt191DocForPayout(p, "MT191REQ00000001")
	if _, err := applyMT191Doc(t, l, doc); err != nil {
		t.Fatalf("ApplyMT191: %v", err)
	}
	// Same :20:, same terms: an idempotent no-op returning the
	// stored request, with no second audit event and no second
	// list entry.
	res, err := applyMT191Doc(t, l, doc)
	if err != nil {
		t.Fatalf("ApplyMT191 replay: %v", err)
	}
	if !res.Duplicate {
		t.Error("duplicate = false on replay, want true")
	}
	if res.Request.MsgID != "MT191REQ00000001" || res.Request.AmountCents != 1500 {
		t.Errorf("replay request = %+v, want the stored request", res.Request)
	}
	if list, ok := l.GetPayoutChargeRequests(p.ID); !ok || len(list) != 1 {
		t.Errorf("GetPayoutChargeRequests after replay = %v, want one request", list)
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt191_charges_requested") != 1 {
		t.Errorf("mt191_charges_requested count = %d, want 1 after replay", countOp(events, "mt191_charges_requested"))
	}
}

func TestApplyMT191_ConflictingTermsRejected(t *testing.T) {
	al := mustAuditLog(t)
	l := mt191ApplyLedger(t, al)
	p := mt103SubmittedPayout(t, l, "po-mt191-1", 250_000)
	if _, err := applyMT191Doc(t, l, mt191DocForPayout(p, "MT191REQ00000001")); err != nil {
		t.Fatalf("ApplyMT191: %v", err)
	}
	// Same :20:, different amount: a conflict, never a silent
	// overwrite.
	conflict := mt191Doc("MT191REQ00000001", p.ID, "USD25,00", "Correspondent handling fee\nFor payment processing")
	if _, err := applyMT191Doc(t, l, conflict); !errors.Is(err, ErrMT191MsgIDConflict) {
		t.Errorf("conflict amount: err = %v, want ErrMT191MsgIDConflict", err)
	}
	// Same :20:, different details: a conflict too.
	conflict = mt191Doc("MT191REQ00000001", p.ID, "USD15,00", "A different fee entirely")
	if _, err := applyMT191Doc(t, l, conflict); !errors.Is(err, ErrMT191MsgIDConflict) {
		t.Errorf("conflict details: err = %v, want ErrMT191MsgIDConflict", err)
	}
	// Same :20:, different payout: a conflict too.
	conflict = mt191Doc("MT191REQ00000001", "po-other", "USD15,00", "Correspondent handling fee\nFor payment processing")
	if _, err := applyMT191Doc(t, l, conflict); !errors.Is(err, ErrMT191MsgIDConflict) {
		t.Errorf("conflict payout: err = %v, want ErrMT191MsgIDConflict", err)
	}
	// The stored request is untouched, and no second marker.
	if list, ok := l.GetPayoutChargeRequests(p.ID); !ok || len(list) != 1 || list[0].AmountCents != 1500 {
		t.Errorf("GetPayoutChargeRequests after conflicts = %v, want the one stored request", list)
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt191_charges_requested") != 1 {
		t.Errorf("mt191_charges_requested count = %d, want 1 after conflicts", countOp(events, "mt191_charges_requested"))
	}
}

func TestApplyMT191_DoesNotPostJournalEntries(t *testing.T) {
	al := mustAuditLog(t)
	l := mt191ApplyLedger(t, al)
	p := mt103SubmittedPayout(t, l, "po-mt191-1", 250_000)
	before := l.Version()
	entriesBefore := len(l.Entries())
	trackBefore, ok := l.GetGPITrack(p.UETR)
	if !ok {
		t.Fatal("gpi track missing for a submitted payout")
	}

	if _, err := applyMT191Doc(t, l, mt191DocForPayout(p, "MT191REQ00000001")); err != nil {
		t.Fatalf("ApplyMT191: %v", err)
	}
	// The payout itself stays submitted, the version is
	// untouched, no journal entry appears, and the gpi history
	// is exactly as it was: a request advances nothing and pays
	// nothing.
	if got, ok := l.GetPayout(p.ID); !ok || got.Status != PayoutSubmitted {
		t.Errorf("stored payout = %+v, want still submitted", got)
	}
	if l.Version() != before {
		t.Errorf("version = %d, want %d (evidence-only, no bump)", l.Version(), before)
	}
	if len(l.Entries()) != entriesBefore {
		t.Errorf("journal entries = %d, want %d (no entry posted)", len(l.Entries()), entriesBefore)
	}
	trackAfter, ok := l.GetGPITrack(p.UETR)
	if !ok {
		t.Fatal("gpi track missing after request")
	}
	if len(trackAfter.History) != len(trackBefore.History) || trackAfter.Status != trackBefore.Status {
		t.Errorf("gpi track moved: %+v -> %+v, want unchanged", trackBefore, trackAfter)
	}
}

func TestMT191Request_GetByPayout(t *testing.T) {
	al := mustAuditLog(t)
	l := mt191ApplyLedger(t, al)
	p := mt103SubmittedPayout(t, l, "po-mt191-1", 250_000)

	if _, ok := l.GetPayoutChargeRequests(p.ID); ok {
		t.Error("GetPayoutChargeRequests = true before any request, want false")
	}
	res, err := applyMT191Doc(t, l, mt191DocForPayout(p, "MT191REQ00000001"))
	if err != nil {
		t.Fatalf("ApplyMT191: %v", err)
	}
	got, ok := l.GetPayoutChargeRequests(p.ID)
	if !ok {
		t.Fatal("GetPayoutChargeRequests = false after recording, want true")
	}
	if len(got) != 1 || got[0] != res.Request {
		t.Errorf("GetPayoutChargeRequests = %+v, want [%+v]", got, res.Request)
	}
	// The returned slice is a copy: mutating it leaves the
	// ledger's list alone.
	got[0].AmountCents = 999999
	again, ok := l.GetPayoutChargeRequests(p.ID)
	if !ok || len(again) != 1 || again[0].AmountCents != 1500 {
		t.Errorf("GetPayoutChargeRequests after mutation = %+v, want the stored 1500", again)
	}
	if _, ok := l.GetPayoutChargeRequests("po-never-charged"); ok {
		t.Error("GetPayoutChargeRequests = true for an uncharged payout, want false")
	}
}
