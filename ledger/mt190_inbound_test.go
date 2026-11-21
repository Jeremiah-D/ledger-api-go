package ledger

import (
	"errors"
	"strings"
	"testing"
)

// mt190Doc renders one inbound SWIFT FIN MT190 advice of charges
// message: ref as :20:, payoutID as :21:, amount32A as the full
// :32A: body ("YYMMDDCCC<integer>,<ff>"), and details as the :71B:
// text (a text containing "\n" spans lines). Empty details omits
// :71B: entirely.
func mt190Doc(ref, payoutID, amount32A, details string) string {
	var sb strings.Builder
	sb.WriteString("{1:F01BANKUS33XXX0000000000}\n")
	sb.WriteString("{2:I190BANKDEFFXXXXU3003}\n")
	sb.WriteString("{3:{108:" + ref + "}}\n")
	sb.WriteString("{4:\n")
	sb.WriteString(":20:" + ref + "\n")
	sb.WriteString(":21:" + payoutID + "\n")
	sb.WriteString(":32A:" + amount32A + "\n")
	if details != "" {
		sb.WriteString(":71B:" + details + "\n")
	}
	sb.WriteString("-}")
	return sb.String()
}

// mt190DocForPayout renders the MT190 advising a 15,00 charge in
// the payout's currency on the payout's MT103 value date: the :20:
// is ref, the :71B: names the fee in words.
func mt190DocForPayout(p PayoutInstruction, ref string) string {
	return mt190Doc(ref, p.ID, mt103ValueDate(p.SubmittedAt)+"USD15,00", "Correspondent handling fee\nFor payment processing")
}

// applyMT190Doc parses doc and applies it, failing the test on
// parse errors: the gates under test are the apply gates, not the
// parse gates.
func applyMT190Doc(t *testing.T, l *Ledger, doc string) (MT190ApplyResult, error) {
	t.Helper()
	m, err := ParseMT190(doc)
	if err != nil {
		t.Fatalf("ParseMT190: %v", err)
	}
	return l.ApplyMT190(m, "correspondent-bank")
}

// mt190ApplyLedger seeds a payout ledger with an audit log, the
// fixture for the MT190 apply tests.
func mt190ApplyLedger(t *testing.T, al *AuditLog) *Ledger {
	t.Helper()
	return seedPayoutLedger(t, WithAuditLog(al))
}

func TestParseMT190_RoundTrip(t *testing.T) {
	l := mt103TestLedger(t)
	p := mt103SubmittedPayout(t, l, "po-mt190-1", 250_000)

	doc := mt190DocForPayout(p, "MT190ADVICE00001")
	m, err := ParseMT190(doc)
	if err != nil {
		t.Fatalf("ParseMT190: %v", err)
	}
	if m.SenderLT != "BANKUS33XXX" || m.ReceiverLT != "BANKDEFFXXXX" {
		t.Errorf("LTs = %q/%q, want BANKUS33XXX/BANKDEFFXXXX", m.SenderLT, m.ReceiverLT)
	}
	if m.Reference != "MT190ADVICE00001" {
		t.Errorf(":20: = %q, want MT190ADVICE00001", m.Reference)
	}
	if m.RelatedReference != p.ID {
		t.Errorf(":21: = %q, want %q", m.RelatedReference, p.ID)
	}
	if mt103ValueDate(m.ValueDate) != mt103ValueDate(p.SubmittedAt) {
		t.Errorf(":32A: value date = %q, want %q", mt103ValueDate(m.ValueDate), mt103ValueDate(p.SubmittedAt))
	}
	if m.Currency != "USD" {
		t.Errorf(":32A: currency = %q, want USD", m.Currency)
	}
	if m.AmountCents != 1500 {
		t.Errorf(":32A: amount = %d, want 1500", m.AmountCents)
	}
	if m.Details != "Correspondent handling fee\nFor payment processing" {
		t.Errorf(":71B: = %q, want two lines verbatim", m.Details)
	}
	// :71B: alone carries the detail; a message without it
	// parses with empty details.
	doc = mt190Doc("MT190ADVICE00001", p.ID, mt103ValueDate(p.SubmittedAt)+"USD15,00", "")
	m, err = ParseMT190(doc)
	if err != nil {
		t.Fatalf("ParseMT190(no :71B:): %v", err)
	}
	if m.Details != "" {
		t.Errorf(":71B: = %q, want empty", m.Details)
	}
	// A six-line :71B: is the cap, and it parses.
	six := "L1\nL2\nL3\nL4\nL5\nL6"
	doc = mt190Doc("MT190ADVICE00001", p.ID, mt103ValueDate(p.SubmittedAt)+"USD15,00", six)
	m, err = ParseMT190(doc)
	if err != nil {
		t.Fatalf("ParseMT190(6-line :71B:): %v", err)
	}
	if m.Details != six {
		t.Errorf(":71B: = %q, want six lines verbatim", m.Details)
	}
}

func TestParseMT190_RejectsBadBlockStructure(t *testing.T) {
	l := mt103TestLedger(t)
	p := mt103SubmittedPayout(t, l, "po-mt190-1", 250_000)
	good := mt190DocForPayout(p, "MT190ADVICE00001")
	valueDate := mt103ValueDate(p.SubmittedAt)

	cases := []struct {
		name string
		doc  string
	}{
		{"block 2 is MT202", strings.Replace(good, "{2:I190", "{2:I202", 1)},
		{"missing block 3", strings.Replace(good, "{3:{108:MT190ADVICE00001}}\n", "", 1)},
		{"missing {4:", strings.Replace(good, "{4:\n", "", 1)},
		{"missing trailer", strings.TrimSuffix(good, "-}")},
		{"field order swapped", strings.Replace(good,
			":20:MT190ADVICE00001\n:21:po-mt190-1\n",
			":21:po-mt190-1\n:20:MT190ADVICE00001\n", 1)},
		{"empty :20:", strings.Replace(good, ":20:MT190ADVICE00001\n", ":20:\n", 1)},
		{"empty :21:", strings.Replace(good, ":21:po-mt190-1\n", ":21:\n", 1)},
		{"empty :32A:", strings.Replace(good, ":32A:"+valueDate+"USD15,00\n", ":32A:\n", 1)},
		{":32A: too short", mt190Doc("MT190ADVICE00001", p.ID, "261121USD", "Correspondent handling fee")},
		{":32A: bad value date", mt190Doc("MT190ADVICE00001", p.ID, "991332USD15,00", "Correspondent handling fee")},
		{":32A: lowercase currency", mt190Doc("MT190ADVICE00001", p.ID, valueDate+"usd15,00", "Correspondent handling fee")},
		{":32A: amount without comma", mt190Doc("MT190ADVICE00001", p.ID, valueDate+"USD1500", "Correspondent handling fee")},
		{":32A: amount one fraction digit", mt190Doc("MT190ADVICE00001", p.ID, valueDate+"USD15,0", "Correspondent handling fee")},
		{"too few lines", "{1:F01BANKUS33XXX0000000000}"},
		{"empty document", "  "},
	}
	for _, tc := range cases {
		if _, err := ParseMT190(tc.doc); err == nil {
			t.Errorf("%s: parsed without error, want refusal", tc.name)
		}
	}
	// The value-date and currency failures carry their own
	// error identities, not the framing error.
	if _, err := ParseMT190(mt190Doc("MT190ADVICE00001", p.ID, "991332USD15,00", "Correspondent handling fee")); !errors.Is(err, ErrMT190ValueDate) {
		t.Errorf("bad value date: err = %v, want ErrMT190ValueDate", err)
	}
	if _, err := ParseMT190(mt190Doc("MT190ADVICE00001", p.ID, valueDate+"usd15,00", "Correspondent handling fee")); !errors.Is(err, ErrMT190Currency) {
		t.Errorf("lowercase currency: err = %v, want ErrMT190Currency", err)
	}
}

func TestParseMT190_RejectsUnknownField(t *testing.T) {
	l := mt103TestLedger(t)
	p := mt103SubmittedPayout(t, l, "po-mt190-1", 250_000)
	good := mt190DocForPayout(p, "MT190ADVICE00001")
	valueDate := mt103ValueDate(p.SubmittedAt)

	// :32B: (currency and amount, no value date) is outside the
	// core set: the ledger's evidence record needs the date.
	with32B := strings.Replace(good, ":32A:"+valueDate+"USD15,00\n", ":32B:USD15,00\n", 1)
	if _, err := ParseMT190(with32B); !errors.Is(err, ErrMT190Field) {
		t.Errorf(":32B: field: err = %v, want ErrMT190Field", err)
	}
	// An optional :25: account identification is out of the
	// core set: the ledger knows the payout's account.
	with25 := strings.Replace(good, ":32A:", ":25:ACC-123\n:32A:", 1)
	if _, err := ParseMT190(with25); !errors.Is(err, ErrMT190Field) {
		t.Errorf(":25: field: err = %v, want ErrMT190Field", err)
	}
	// An optional :72: sender-to-receiver field is out of the
	// core set: refused, not parsed with the field quietly
	// discarded.
	with72 := strings.Replace(good, "-}", ":72:/BNK/Please note\n-}", 1)
	if _, err := ParseMT190(with72); !errors.Is(err, ErrMT190Field) {
		t.Errorf(":72: field: err = %v, want ErrMT190Field", err)
	}
	// A `:` line inside the :71B: details run is an unknown
	// field, not a continuation.
	withColon := strings.Replace(good, ":71B:Correspondent handling fee\n", ":71B:Correspondent handling fee\n:99:extra\n", 1)
	if _, err := ParseMT190(withColon); !errors.Is(err, ErrMT190Field) {
		t.Errorf(":99: in details: err = %v, want ErrMT190Field", err)
	}
	// :71B: is 6*35x: a seventh line is refused.
	if _, err := ParseMT190(mt190Doc("MT190ADVICE00001", p.ID, valueDate+"USD15,00", "L1\nL2\nL3\nL4\nL5\nL6\nL7")); !errors.Is(err, ErrMT190Field) {
		t.Errorf("7-line :71B:: err = %v, want ErrMT190Field", err)
	}
	// A :71B: line over 35 characters is refused.
	if _, err := ParseMT190(mt190Doc("MT190ADVICE00001", p.ID, valueDate+"USD15,00", "Correspondent handling fee, this line is far too long")); !errors.Is(err, ErrMT190Field) {
		t.Errorf("over-long :71B: line: err = %v, want ErrMT190Field", err)
	}
}

func TestParseMT190_RejectsNonPositiveAmount(t *testing.T) {
	l := mt103TestLedger(t)
	p := mt103SubmittedPayout(t, l, "po-mt190-1", 250_000)
	valueDate := mt103ValueDate(p.SubmittedAt)

	if _, err := ParseMT190(mt190Doc("MT190ADVICE00001", p.ID, valueDate+"USD0,00", "Correspondent handling fee")); !errors.Is(err, ErrMT190Amount) {
		t.Errorf("zero amount: err = %v, want ErrMT190Amount", err)
	}
	// A negative amount cannot be written in the 15d shape:
	// the minus sign fails the digit check.
	if _, err := ParseMT190(mt190Doc("MT190ADVICE00001", p.ID, valueDate+"USD-5,00", "Correspondent handling fee")); !errors.Is(err, ErrMT190Amount) {
		t.Errorf("negative amount: err = %v, want ErrMT190Amount", err)
	}
	// One cent parses: the floor is zero, not one unit.
	if _, err := ParseMT190(mt190Doc("MT190ADVICE00001", p.ID, valueDate+"USD0,01", "Correspondent handling fee")); err != nil {
		t.Errorf("one cent: err = %v, want nil", err)
	}
}

func TestParseMT190_AcceptsCRLF(t *testing.T) {
	l := mt103TestLedger(t)
	p := mt103SubmittedPayout(t, l, "po-mt190-1", 250_000)
	doc := mt190DocForPayout(p, "MT190ADVICE00001")

	crlf := strings.ReplaceAll(doc, "\n", "\r\n")
	if _, err := ParseMT190(crlf); err != nil {
		t.Errorf("CRLF: %v", err)
	}
	// A trailing newline after the trailer is tolerated.
	if _, err := ParseMT190(doc + "\n"); err != nil {
		t.Errorf("trailing newline: %v", err)
	}
}

func TestApplyMT190_RecordsChargeAdvice(t *testing.T) {
	al := mustAuditLog(t)
	l := mt190ApplyLedger(t, al)
	p := mt103SubmittedPayout(t, l, "po-mt190-1", 250_000)
	before := l.Version()
	entriesBefore := len(l.Entries())

	res, err := applyMT190Doc(t, l, mt190DocForPayout(p, "MT190ADVICE00001"))
	if err != nil {
		t.Fatalf("ApplyMT190: %v", err)
	}
	if res.Duplicate {
		t.Error("duplicate = true on first apply")
	}
	if res.Reference != "MT190ADVICE00001" || res.PayoutID != p.ID {
		t.Errorf("reference/payout = %q/%q, want MT190ADVICE00001/%q", res.Reference, res.PayoutID, p.ID)
	}
	a := res.Advice
	if a.MsgID != "MT190ADVICE00001" || a.PayoutID != p.ID || a.Currency != "USD" ||
		a.AmountCents != 1500 || a.ValueDate != mt103ValueDate(p.SubmittedAt) ||
		a.Details != "Correspondent handling fee\nFor payment processing" {
		t.Errorf("advice = %+v, want the parsed fields verbatim", a)
	}
	if a.Actor != "correspondent-bank" || a.AppliedAt.IsZero() {
		t.Errorf("actor/applied_at = %q/%v, want correspondent-bank/a time", a.Actor, a.AppliedAt)
	}
	// The per-payout list holds the advice, in apply order; a
	// second advice under a fresh :20: appends after it.
	list, ok := l.GetPayoutCharges(p.ID)
	if !ok || len(list) != 1 || list[0].MsgID != "MT190ADVICE00001" {
		t.Errorf("GetPayoutCharges = %v/%v, want one advice MT190ADVICE00001", list, ok)
	}
	if _, err := applyMT190Doc(t, l, mt190Doc("MT190ADVICE00002", p.ID, mt103ValueDate(p.SubmittedAt)+"USD2,50", "SWIFT messaging fee")); err != nil {
		t.Fatalf("ApplyMT190 second: %v", err)
	}
	list, ok = l.GetPayoutCharges(p.ID)
	if !ok || len(list) != 2 || list[1].MsgID != "MT190ADVICE00002" || list[1].AmountCents != 250 {
		t.Errorf("GetPayoutCharges after second = %v, want two advices in order", list)
	}
	// Recording is evidence-only: no version bump, no journal
	// entry, and the payout itself stays submitted.
	if l.Version() != before {
		t.Errorf("version = %d, want %d (evidence-only, no bump)", l.Version(), before)
	}
	if len(l.Entries()) != entriesBefore {
		t.Errorf("journal entries = %d, want %d (no entry posted)", len(l.Entries()), entriesBefore)
	}
	if got, ok := l.GetPayout(p.ID); !ok || got.Status != PayoutSubmitted {
		t.Errorf("stored payout = %+v, want still submitted", got)
	}
	// Audit: exactly one inbound marker per advice, with the
	// exact details — the :20: on the durable record.
	events := readAuditOps(t, al)
	if countOp(events, "mt190_charges_advised") != 2 {
		t.Errorf("mt190_charges_advised count = %d, want 2", countOp(events, "mt190_charges_advised"))
	}
	ev := findOp(events, "mt190_charges_advised")
	if ev == nil {
		t.Fatal("mt190_charges_advised event missing")
	}
	if ev.Actor != "correspondent-bank" {
		t.Errorf("actor = %q, want correspondent-bank (ApplyMT190)", ev.Actor)
	}
	if ev.TraceID != "MT190ADVICE00001" {
		t.Errorf("trace = %q, want MT190ADVICE00001", ev.TraceID)
	}
	if ev.Details["msg_id"] != "MT190ADVICE00001" || ev.Details["payout_id"] != p.ID ||
		ev.Details["currency"] != "USD" || ev.Details["amount_cents"] != float64(1500) ||
		ev.Details["value_date"] != mt103ValueDate(p.SubmittedAt) ||
		ev.Details["details"] != "Correspondent handling fee\nFor payment processing" {
		t.Errorf("details = %v, want msg/payout/currency/amount/value-date/details", ev.Details)
	}
}

func TestApplyMT190_UnknownPayoutGoesToRepair(t *testing.T) {
	al := mustAuditLog(t)
	l := mt190ApplyLedger(t, al)
	p := mt103SubmittedPayout(t, l, "po-mt190-1", 250_000)
	_ = p

	res, err := applyMT190Doc(t, l, mt190Doc("MT190ADVICE00001", "no-such", mt103ValueDate(p.SubmittedAt)+"USD15,00", "Correspondent handling fee"))
	if !errors.Is(err, ErrMT103PayoutNotFound) {
		t.Errorf("err = %v, want ErrMT103PayoutNotFound", err)
	}
	// The parked result carries the repair ID alongside the
	// error: the advice is visible to an operator, not dropped.
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
	if rec.Instruction.Destination != "mt190-inbound:MT190ADVICE00001" {
		t.Errorf("repair destination = %q, want mt190-inbound:MT190ADVICE00001", rec.Instruction.Destination)
	}
	// No advice was recorded: nothing to replay, and no charge
	// marker event — the repair_enqueued event is the record.
	if _, ok := l.GetPayoutCharges("no-such"); ok {
		t.Error("GetPayoutCharges = true for a parked advice, want false")
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt190_charges_advised") != 0 {
		t.Errorf("mt190_charges_advised count = %d, want 0", countOp(events, "mt190_charges_advised"))
	}
	if countOp(events, "repair_enqueued") != 1 {
		t.Errorf("repair_enqueued count = %d, want 1", countOp(events, "repair_enqueued"))
	}
}

func TestApplyMT190_CurrencyMismatchGoesToRepair(t *testing.T) {
	al := mustAuditLog(t)
	l := mt190ApplyLedger(t, al)
	p := mt103SubmittedPayout(t, l, "po-mt190-1", 250_000)

	// The payout is USD; the advice charges EUR. Well-formed,
	// but it disagrees with the ledger's fact about the payout:
	// parked, never converted.
	res, err := applyMT190Doc(t, l, mt190Doc("MT190ADVICE00001", p.ID, mt103ValueDate(p.SubmittedAt)+"EUR15,00", "Correspondent handling fee"))
	if !errors.Is(err, ErrMT190CurrencyMismatch) {
		t.Errorf("err = %v, want ErrMT190CurrencyMismatch", err)
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
	if rec.Instruction.Destination != "mt190-inbound:MT190ADVICE00001" {
		t.Errorf("repair destination = %q, want mt190-inbound:MT190ADVICE00001", rec.Instruction.Destination)
	}
	// No advice was recorded for the payout, and no marker
	// event: the EUR charge is not evidence about this USD
	// payout.
	if _, ok := l.GetPayoutCharges(p.ID); ok {
		t.Error("GetPayoutCharges = true after a currency mismatch, want false")
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt190_charges_advised") != 0 {
		t.Errorf("mt190_charges_advised count = %d, want 0", countOp(events, "mt190_charges_advised"))
	}
	// The same :20: with the payout's own currency records
	// fine: the park holds no replay claim on the reference.
	if _, err := applyMT190Doc(t, l, mt190DocForPayout(p, "MT190ADVICE00001")); err != nil {
		t.Fatalf("ApplyMT190 after park: %v", err)
	}
	if list, ok := l.GetPayoutCharges(p.ID); !ok || len(list) != 1 {
		t.Errorf("GetPayoutCharges after re-apply = %v, want one advice", list)
	}
}

func TestApplyMT190_ReplaySameReferenceNoop(t *testing.T) {
	al := mustAuditLog(t)
	l := mt190ApplyLedger(t, al)
	p := mt103SubmittedPayout(t, l, "po-mt190-1", 250_000)
	doc := mt190DocForPayout(p, "MT190ADVICE00001")
	if _, err := applyMT190Doc(t, l, doc); err != nil {
		t.Fatalf("ApplyMT190: %v", err)
	}
	// Same :20:, same terms: an idempotent no-op returning the
	// stored advice, with no second audit event and no second
	// list entry.
	res, err := applyMT190Doc(t, l, doc)
	if err != nil {
		t.Fatalf("ApplyMT190 replay: %v", err)
	}
	if !res.Duplicate {
		t.Error("duplicate = false on replay, want true")
	}
	if res.Advice.MsgID != "MT190ADVICE00001" || res.Advice.AmountCents != 1500 {
		t.Errorf("replay advice = %+v, want the stored advice", res.Advice)
	}
	if list, ok := l.GetPayoutCharges(p.ID); !ok || len(list) != 1 {
		t.Errorf("GetPayoutCharges after replay = %v, want one advice", list)
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt190_charges_advised") != 1 {
		t.Errorf("mt190_charges_advised count = %d, want 1 after replay", countOp(events, "mt190_charges_advised"))
	}
}

func TestApplyMT190_ConflictingTermsRejected(t *testing.T) {
	al := mustAuditLog(t)
	l := mt190ApplyLedger(t, al)
	p := mt103SubmittedPayout(t, l, "po-mt190-1", 250_000)
	valueDate := mt103ValueDate(p.SubmittedAt)
	if _, err := applyMT190Doc(t, l, mt190DocForPayout(p, "MT190ADVICE00001")); err != nil {
		t.Fatalf("ApplyMT190: %v", err)
	}
	// Same :20:, different amount: a conflict, never a silent
	// overwrite.
	conflict := mt190Doc("MT190ADVICE00001", p.ID, valueDate+"USD25,00", "Correspondent handling fee\nFor payment processing")
	if _, err := applyMT190Doc(t, l, conflict); !errors.Is(err, ErrMT190MsgIDConflict) {
		t.Errorf("conflict amount: err = %v, want ErrMT190MsgIDConflict", err)
	}
	// Same :20:, different details: a conflict too.
	conflict = mt190Doc("MT190ADVICE00001", p.ID, valueDate+"USD15,00", "A different fee entirely")
	if _, err := applyMT190Doc(t, l, conflict); !errors.Is(err, ErrMT190MsgIDConflict) {
		t.Errorf("conflict details: err = %v, want ErrMT190MsgIDConflict", err)
	}
	// Same :20:, different payout: a conflict too.
	conflict = mt190Doc("MT190ADVICE00001", "po-other", valueDate+"USD15,00", "Correspondent handling fee\nFor payment processing")
	if _, err := applyMT190Doc(t, l, conflict); !errors.Is(err, ErrMT190MsgIDConflict) {
		t.Errorf("conflict payout: err = %v, want ErrMT190MsgIDConflict", err)
	}
	// The stored advice is untouched, and no second marker.
	if list, ok := l.GetPayoutCharges(p.ID); !ok || len(list) != 1 || list[0].AmountCents != 1500 {
		t.Errorf("GetPayoutCharges after conflicts = %v, want the one stored advice", list)
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt190_charges_advised") != 1 {
		t.Errorf("mt190_charges_advised count = %d, want 1 after conflicts", countOp(events, "mt190_charges_advised"))
	}
}

func TestApplyMT190_DoesNotPostJournalEntries(t *testing.T) {
	al := mustAuditLog(t)
	l := mt190ApplyLedger(t, al)
	p := mt103SubmittedPayout(t, l, "po-mt190-1", 250_000)
	before := l.Version()
	entriesBefore := len(l.Entries())
	trackBefore, ok := l.GetGPITrack(p.UETR)
	if !ok {
		t.Fatal("gpi track missing for a submitted payout")
	}

	if _, err := applyMT190Doc(t, l, mt190DocForPayout(p, "MT190ADVICE00001")); err != nil {
		t.Fatalf("ApplyMT190: %v", err)
	}
	// The payout itself stays submitted, the version is
	// untouched, no journal entry appears, and the gpi history
	// is exactly as it was: an advice advances nothing.
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
		t.Fatal("gpi track missing after advice")
	}
	if len(trackAfter.History) != len(trackBefore.History) || trackAfter.Status != trackBefore.Status {
		t.Errorf("gpi track moved: %+v -> %+v, want unchanged", trackBefore, trackAfter)
	}
}

func TestMT190Advice_GetByPayout(t *testing.T) {
	al := mustAuditLog(t)
	l := mt190ApplyLedger(t, al)
	p := mt103SubmittedPayout(t, l, "po-mt190-1", 250_000)

	if _, ok := l.GetPayoutCharges(p.ID); ok {
		t.Error("GetPayoutCharges = true before any advice, want false")
	}
	res, err := applyMT190Doc(t, l, mt190DocForPayout(p, "MT190ADVICE00001"))
	if err != nil {
		t.Fatalf("ApplyMT190: %v", err)
	}
	got, ok := l.GetPayoutCharges(p.ID)
	if !ok {
		t.Fatal("GetPayoutCharges = false after recording, want true")
	}
	if len(got) != 1 || got[0] != res.Advice {
		t.Errorf("GetPayoutCharges = %+v, want [%+v]", got, res.Advice)
	}
	// The returned slice is a copy: mutating it leaves the
	// ledger's list alone.
	got[0].AmountCents = 999999
	again, ok := l.GetPayoutCharges(p.ID)
	if !ok || len(again) != 1 || again[0].AmountCents != 1500 {
		t.Errorf("GetPayoutCharges after mutation = %+v, want the stored 1500", again)
	}
	if _, ok := l.GetPayoutCharges("po-never-charged"); ok {
		t.Error("GetPayoutCharges = true for an uncharged payout, want false")
	}
}
