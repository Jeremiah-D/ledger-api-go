package ledger

import (
	"errors"
	"strings"
	"testing"
)

// mt290Doc renders one inbound SWIFT FIN MT290 advice of charges
// message: ref as :20:, mt202Ref as :21:, amount32B as the full
// :32B: body ("CCC<integer>,<ff>"), and details as the :71B:
// text (a text containing "\n" spans lines). Empty details omits
// :71B: entirely.
func mt290Doc(ref, mt202Ref, amount32B, details string) string {
	var sb strings.Builder
	sb.WriteString("{1:F01BANKUS33XXX0000000000}\n")
	sb.WriteString("{2:I290BANKDEFFXXXXU3003}\n")
	sb.WriteString("{3:{108:" + ref + "}}\n")
	sb.WriteString("{4:\n")
	sb.WriteString(":20:" + ref + "\n")
	sb.WriteString(":21:" + mt202Ref + "\n")
	sb.WriteString(":32B:" + amount32B + "\n")
	if details != "" {
		sb.WriteString(":71B:" + details + "\n")
	}
	sb.WriteString("-}")
	return sb.String()
}

// mt290DocForMT202 renders the MT290 advising a 15,00 charge in
// the MT202's currency on the applied MT202: the :20: is ref,
// the :71B: names the fee in words.
func mt290DocForMT202(ref string) string {
	return mt290Doc(ref, "MT202-REF-01", "USD15,00", "Correspondent handling fee\nFor payment processing")
}

// applyMT290Doc parses doc and applies it, failing the test on
// parse errors: the gates under test are the apply gates, not the
// parse gates.
func applyMT290Doc(t *testing.T, l *Ledger, doc string) (MT290ApplyResult, error) {
	t.Helper()
	m, err := ParseMT290(doc)
	if err != nil {
		t.Fatalf("ParseMT290: %v", err)
	}
	return l.ApplyMT290(m, "correspondent-bank")
}

// mt290ApplyLedger builds a ledger with a funded nostro account,
// both BICs registered, and one applied MT202 (MT202-REF-01,
// USD 100.00, DEUTDEFF nostro -> BNPAFRPP vostro, value date
// today): the fixture for the MT290 apply tests.
func mt290ApplyLedger(t *testing.T, al *AuditLog) *Ledger {
	t.Helper()
	l := mt202ApplyLedger(t, al)
	res, err := l.ApplyMT202(mt202ApplyMsg("MT202-REF-01"), "")
	if err != nil {
		t.Fatalf("ApplyMT202 setup: %v", err)
	}
	if res.Err != nil {
		t.Fatalf("ApplyMT202 setup result Err = %v", res.Err)
	}
	return l
}

func TestParseMT290_RoundTrip(t *testing.T) {
	doc := mt290DocForMT202("MT290ADVICE00001")
	m, err := ParseMT290(doc)
	if err != nil {
		t.Fatalf("ParseMT290: %v", err)
	}
	if m.SenderLT != "BANKUS33XXX" || m.ReceiverLT != "BANKDEFFXXXX" {
		t.Errorf("LTs = %q/%q, want BANKUS33XXX/BANKDEFFXXXX", m.SenderLT, m.ReceiverLT)
	}
	if m.Reference != "MT290ADVICE00001" {
		t.Errorf(":20: = %q, want MT290ADVICE00001", m.Reference)
	}
	if m.RelatedReference != "MT202-REF-01" {
		t.Errorf(":21: = %q, want MT202-REF-01", m.RelatedReference)
	}
	if m.Currency != "USD" {
		t.Errorf(":32B: currency = %q, want USD", m.Currency)
	}
	if m.AmountCents != 1500 {
		t.Errorf(":32B: amount = %d, want 1500", m.AmountCents)
	}
	if m.Details != "Correspondent handling fee\nFor payment processing" {
		t.Errorf(":71B: = %q, want two lines verbatim", m.Details)
	}
	// A six-line :71B: is the cap, and it parses.
	six := "L1\nL2\nL3\nL4\nL5\nL6"
	doc = mt290Doc("MT290ADVICE00001", "MT202-REF-01", "USD15,00", six)
	m, err = ParseMT290(doc)
	if err != nil {
		t.Fatalf("ParseMT290(6-line :71B:): %v", err)
	}
	if m.Details != six {
		t.Errorf(":71B: = %q, want six lines verbatim", m.Details)
	}
}

func TestParseMT290_RejectsBadBlockStructure(t *testing.T) {
	good := mt290DocForMT202("MT290ADVICE00001")

	cases := []struct {
		name string
		doc  string
	}{
		{"block 2 is MT190", strings.Replace(good, "{2:I290", "{2:I190", 1)},
		{"missing block 3", strings.Replace(good, "{3:{108:MT290ADVICE00001}}\n", "", 1)},
		{"missing {4:", strings.Replace(good, "{4:\n", "", 1)},
		{"missing trailer", strings.TrimSuffix(good, "-}")},
		{"field order swapped", strings.Replace(good,
			":20:MT290ADVICE00001\n:21:MT202-REF-01\n",
			":21:MT202-REF-01\n:20:MT290ADVICE00001\n", 1)},
		{"empty :20:", strings.Replace(good, ":20:MT290ADVICE00001\n", ":20:\n", 1)},
		{"empty :21:", strings.Replace(good, ":21:MT202-REF-01\n", ":21:\n", 1)},
		{"empty :32B:", strings.Replace(good, ":32B:USD15,00\n", ":32B:\n", 1)},
		{":32B: too short", mt290Doc("MT290ADVICE00001", "MT202-REF-01", "USD", "Correspondent handling fee")},
		{":32B: lowercase currency", mt290Doc("MT290ADVICE00001", "MT202-REF-01", "usd15,00", "Correspondent handling fee")},
		{":32B: amount without comma", mt290Doc("MT290ADVICE00001", "MT202-REF-01", "USD1500", "Correspondent handling fee")},
		{":32B: amount one fraction digit", mt290Doc("MT290ADVICE00001", "MT202-REF-01", "USD15,0", "Correspondent handling fee")},
		{"too few lines", "{1:F01BANKUS33XXX0000000000}"},
		{"empty document", "  "},
	}
	for _, tc := range cases {
		if _, err := ParseMT290(tc.doc); err == nil {
			t.Errorf("%s: parsed without error, want refusal", tc.name)
		}
	}
	// The currency failure carries its own error identity, not
	// the framing error.
	if _, err := ParseMT290(mt290Doc("MT290ADVICE00001", "MT202-REF-01", "usd15,00", "Correspondent handling fee")); !errors.Is(err, ErrMT290Currency) {
		t.Errorf("lowercase currency: err = %v, want ErrMT290Currency", err)
	}
}

func TestParseMT290_RejectsUnknownField(t *testing.T) {
	good := mt290DocForMT202("MT290ADVICE00001")

	// :32A: (value date, currency, amount) is outside the core
	// set: an MT290 carries :32B: — the value date lives on the
	// MT202, and a dated charge has no field to land in.
	with32A := strings.Replace(good, ":32B:USD15,00\n", ":32A:261121USD15,00\n", 1)
	if _, err := ParseMT290(with32A); !errors.Is(err, ErrMT290Field) {
		t.Errorf(":32A: field: err = %v, want ErrMT290Field", err)
	}
	// An optional :25: account identification is out of the
	// core set: the ledger knows the MT202's accounts.
	with25 := strings.Replace(good, ":32B:", ":25:ACC-123\n:32B:", 1)
	if _, err := ParseMT290(with25); !errors.Is(err, ErrMT290Field) {
		t.Errorf(":25: field: err = %v, want ErrMT290Field", err)
	}
	// An optional :72: sender-to-receiver field is out of the
	// core set: refused, not parsed with the field quietly
	// discarded.
	with72 := strings.Replace(good, "-}", ":72:/BNK/Please note\n-}", 1)
	if _, err := ParseMT290(with72); !errors.Is(err, ErrMT290Field) {
		t.Errorf(":72: field: err = %v, want ErrMT290Field", err)
	}
	// A `:` line inside the :71B: details run is an unknown
	// field, not a continuation.
	withColon := strings.Replace(good, ":71B:Correspondent handling fee\n", ":71B:Correspondent handling fee\n:99:extra\n", 1)
	if _, err := ParseMT290(withColon); !errors.Is(err, ErrMT290Field) {
		t.Errorf(":99: in details: err = %v, want ErrMT290Field", err)
	}
	// :71B: is 6*35x: a seventh line is refused.
	if _, err := ParseMT290(mt290Doc("MT290ADVICE00001", "MT202-REF-01", "USD15,00", "L1\nL2\nL3\nL4\nL5\nL6\nL7")); !errors.Is(err, ErrMT290Field) {
		t.Errorf("7-line :71B:: err = %v, want ErrMT290Field", err)
	}
	// A :71B: line over 35 characters is refused.
	if _, err := ParseMT290(mt290Doc("MT290ADVICE00001", "MT202-REF-01", "USD15,00", "Correspondent handling fee, this line is far too long")); !errors.Is(err, ErrMT290Field) {
		t.Errorf("over-long :71B: line: err = %v, want ErrMT290Field", err)
	}
	// :71B: is mandatory in MT290 (the SWIFT n90 field table
	// marks it M): a message without it is refused as a
	// missing required field, unlike the MT190 advice where it
	// is optional.
	if _, err := ParseMT290(mt290Doc("MT290ADVICE00001", "MT202-REF-01", "USD15,00", "")); !errors.Is(err, ErrMT290Field) {
		t.Errorf("missing :71B:: err = %v, want ErrMT290Field", err)
	}
}

func TestParseMT290_RejectsNonPositiveAmount(t *testing.T) {
	if _, err := ParseMT290(mt290Doc("MT290ADVICE00001", "MT202-REF-01", "USD0,00", "Correspondent handling fee")); !errors.Is(err, ErrMT290Amount) {
		t.Errorf("zero amount: err = %v, want ErrMT290Amount", err)
	}
	// A negative amount cannot be written in the 15d shape:
	// the minus sign fails the digit check.
	if _, err := ParseMT290(mt290Doc("MT290ADVICE00001", "MT202-REF-01", "USD-5,00", "Correspondent handling fee")); !errors.Is(err, ErrMT290Amount) {
		t.Errorf("negative amount: err = %v, want ErrMT290Amount", err)
	}
	// One cent parses: the floor is zero, not one unit.
	if _, err := ParseMT290(mt290Doc("MT290ADVICE00001", "MT202-REF-01", "USD0,01", "Correspondent handling fee")); err != nil {
		t.Errorf("one cent: err = %v, want nil", err)
	}
}

func TestParseMT290_AcceptsCRLF(t *testing.T) {
	doc := mt290DocForMT202("MT290ADVICE00001")

	crlf := strings.ReplaceAll(doc, "\n", "\r\n")
	if _, err := ParseMT290(crlf); err != nil {
		t.Errorf("CRLF: %v", err)
	}
	// A trailing newline after the trailer is tolerated.
	if _, err := ParseMT290(doc + "\n"); err != nil {
		t.Errorf("trailing newline: %v", err)
	}
}

func TestApplyMT290_RecordsChargeAdvice(t *testing.T) {
	al := mustAuditLog(t)
	l := mt290ApplyLedger(t, al)
	before := l.Version()
	entriesBefore := len(l.Entries())

	res, err := applyMT290Doc(t, l, mt290DocForMT202("MT290ADVICE00001"))
	if err != nil {
		t.Fatalf("ApplyMT290: %v", err)
	}
	if res.Duplicate {
		t.Error("duplicate = true on first apply")
	}
	if res.Reference != "MT290ADVICE00001" || res.MT202Ref != "MT202-REF-01" {
		t.Errorf("reference/mt202 = %q/%q, want MT290ADVICE00001/MT202-REF-01", res.Reference, res.MT202Ref)
	}
	a := res.Advice
	if a.MsgID != "MT290ADVICE00001" || a.MT202Ref != "MT202-REF-01" || a.Currency != "USD" ||
		a.AmountCents != 1500 ||
		a.Details != "Correspondent handling fee\nFor payment processing" {
		t.Errorf("advice = %+v, want the parsed fields verbatim", a)
	}
	if a.Actor != "correspondent-bank" || a.AppliedAt.IsZero() {
		t.Errorf("actor/applied_at = %q/%v, want correspondent-bank/a time", a.Actor, a.AppliedAt)
	}
	// The per-MT202 list holds the advice, in apply order; a
	// second advice under a fresh :20: appends after it.
	list, ok := l.GetMT202Charges("MT202-REF-01")
	if !ok || len(list) != 1 || list[0].MsgID != "MT290ADVICE00001" {
		t.Errorf("GetMT202Charges = %v/%v, want one advice MT290ADVICE00001", list, ok)
	}
	if _, err := applyMT290Doc(t, l, mt290Doc("MT290ADVICE00002", "MT202-REF-01", "USD2,50", "SWIFT messaging fee")); err != nil {
		t.Fatalf("ApplyMT290 second: %v", err)
	}
	list, ok = l.GetMT202Charges("MT202-REF-01")
	if !ok || len(list) != 2 || list[1].MsgID != "MT290ADVICE00002" || list[1].AmountCents != 250 {
		t.Errorf("GetMT202Charges after second = %v, want two advices in order", list)
	}
	// Recording is evidence-only: no version bump, no journal
	// entry, and the MT202's transfer stays exactly as booked.
	if l.Version() != before {
		t.Errorf("version = %d, want %d (evidence-only, no bump)", l.Version(), before)
	}
	if len(l.Entries()) != entriesBefore {
		t.Errorf("journal entries = %d, want %d (no entry posted)", len(l.Entries()), entriesBefore)
	}
	if got := l.BalanceIn("nostro-acct", "USD"); got != 1000000-10000 {
		t.Errorf("nostro-acct balance = %d, want %d (advice moves nothing)", got, 1000000-10000)
	}
	if got := l.BalanceIn("vostro-acct", "USD"); got != 10000 {
		t.Errorf("vostro-acct balance = %d, want 10000 (advice moves nothing)", got)
	}
	// Audit: exactly one inbound marker per advice, with the
	// exact details — the :20: on the durable record. There
	// is no value date: :32B: carries none.
	events := readAuditOps(t, al)
	if countOp(events, "mt290_charges_advised") != 2 {
		t.Errorf("mt290_charges_advised count = %d, want 2", countOp(events, "mt290_charges_advised"))
	}
	ev := findOp(events, "mt290_charges_advised")
	if ev == nil {
		t.Fatal("mt290_charges_advised event missing")
	}
	if ev.Actor != "correspondent-bank" {
		t.Errorf("actor = %q, want correspondent-bank (ApplyMT290)", ev.Actor)
	}
	if ev.TraceID != "MT290ADVICE00001" {
		t.Errorf("trace = %q, want MT290ADVICE00001", ev.TraceID)
	}
	if ev.Details["msg_id"] != "MT290ADVICE00001" || ev.Details["mt202_ref"] != "MT202-REF-01" ||
		ev.Details["currency"] != "USD" || ev.Details["amount_cents"] != float64(1500) ||
		ev.Details["details"] != "Correspondent handling fee\nFor payment processing" {
		t.Errorf("details = %v, want msg/mt202/currency/amount/details", ev.Details)
	}
	if _, hasValueDate := ev.Details["value_date"]; hasValueDate {
		t.Errorf("details carry value_date = %v, want none (:32B: has no date)", ev.Details["value_date"])
	}
}

func TestApplyMT290_UnknownMT202GoesToRepair(t *testing.T) {
	al := mustAuditLog(t)
	l := mt290ApplyLedger(t, al)

	res, err := applyMT290Doc(t, l, mt290Doc("MT290ADVICE00001", "no-such", "USD15,00", "Correspondent handling fee"))
	if !errors.Is(err, ErrMT290MT202NotFound) {
		t.Errorf("err = %v, want ErrMT290MT202NotFound", err)
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
	if rec.Instruction.Destination != "mt290-inbound:MT290ADVICE00001" {
		t.Errorf("repair destination = %q, want mt290-inbound:MT290ADVICE00001", rec.Instruction.Destination)
	}
	// No advice was recorded: nothing to replay, and no charge
	// marker event — the repair_enqueued event is the record.
	if _, ok := l.GetMT202Charges("no-such"); ok {
		t.Error("GetMT202Charges = true for a parked advice, want false")
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt290_charges_advised") != 0 {
		t.Errorf("mt290_charges_advised count = %d, want 0", countOp(events, "mt290_charges_advised"))
	}
	if countOp(events, "repair_enqueued") != 1 {
		t.Errorf("repair_enqueued count = %d, want 1", countOp(events, "repair_enqueued"))
	}
}

func TestApplyMT290_CurrencyMismatchGoesToRepair(t *testing.T) {
	al := mustAuditLog(t)
	l := mt290ApplyLedger(t, al)

	// The MT202 is USD; the advice charges EUR. Well-formed,
	// but it disagrees with the ledger's fact about the
	// instruction: parked, never converted.
	res, err := applyMT290Doc(t, l, mt290Doc("MT290ADVICE00001", "MT202-REF-01", "EUR15,00", "Correspondent handling fee"))
	if !errors.Is(err, ErrMT290CurrencyMismatch) {
		t.Errorf("err = %v, want ErrMT290CurrencyMismatch", err)
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
	if rec.Instruction.Destination != "mt290-inbound:MT290ADVICE00001" {
		t.Errorf("repair destination = %q, want mt290-inbound:MT290ADVICE00001", rec.Instruction.Destination)
	}
	// No advice was recorded for the MT202, and no marker
	// event: the EUR charge is not evidence about this USD
	// instruction.
	if _, ok := l.GetMT202Charges("MT202-REF-01"); ok {
		t.Error("GetMT202Charges = true after a currency mismatch, want false")
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt290_charges_advised") != 0 {
		t.Errorf("mt290_charges_advised count = %d, want 0", countOp(events, "mt290_charges_advised"))
	}
	// The same :20: with the instruction's own currency
	// records fine: the park holds no replay claim on the
	// reference.
	if _, err := applyMT290Doc(t, l, mt290DocForMT202("MT290ADVICE00001")); err != nil {
		t.Fatalf("ApplyMT290 after park: %v", err)
	}
	if list, ok := l.GetMT202Charges("MT202-REF-01"); !ok || len(list) != 1 {
		t.Errorf("GetMT202Charges after re-apply = %v, want one advice", list)
	}
}

func TestApplyMT290_ReplaySameReferenceNoop(t *testing.T) {
	al := mustAuditLog(t)
	l := mt290ApplyLedger(t, al)
	doc := mt290DocForMT202("MT290ADVICE00001")
	if _, err := applyMT290Doc(t, l, doc); err != nil {
		t.Fatalf("ApplyMT290: %v", err)
	}
	// Same :20:, same terms: an idempotent no-op returning the
	// stored advice, with no second audit event and no second
	// list entry.
	res, err := applyMT290Doc(t, l, doc)
	if err != nil {
		t.Fatalf("ApplyMT290 replay: %v", err)
	}
	if !res.Duplicate {
		t.Error("duplicate = false on replay, want true")
	}
	if res.Advice.MsgID != "MT290ADVICE00001" || res.Advice.AmountCents != 1500 {
		t.Errorf("replay advice = %+v, want the stored advice", res.Advice)
	}
	if list, ok := l.GetMT202Charges("MT202-REF-01"); !ok || len(list) != 1 {
		t.Errorf("GetMT202Charges after replay = %v, want one advice", list)
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt290_charges_advised") != 1 {
		t.Errorf("mt290_charges_advised count = %d, want 1 after replay", countOp(events, "mt290_charges_advised"))
	}
}

func TestApplyMT290_ConflictingTermsRejected(t *testing.T) {
	al := mustAuditLog(t)
	l := mt290ApplyLedger(t, al)
	if _, err := applyMT290Doc(t, l, mt290DocForMT202("MT290ADVICE00001")); err != nil {
		t.Fatalf("ApplyMT290: %v", err)
	}
	// Same :20:, different amount: a conflict, never a silent
	// overwrite.
	conflict := mt290Doc("MT290ADVICE00001", "MT202-REF-01", "USD25,00", "Correspondent handling fee\nFor payment processing")
	if _, err := applyMT290Doc(t, l, conflict); !errors.Is(err, ErrMT290MsgIDConflict) {
		t.Errorf("conflict amount: err = %v, want ErrMT290MsgIDConflict", err)
	}
	// Same :20:, different details: a conflict too.
	conflict = mt290Doc("MT290ADVICE00001", "MT202-REF-01", "USD15,00", "A different fee entirely")
	if _, err := applyMT290Doc(t, l, conflict); !errors.Is(err, ErrMT290MsgIDConflict) {
		t.Errorf("conflict details: err = %v, want ErrMT290MsgIDConflict", err)
	}
	// Same :20:, different MT202: a conflict too.
	conflict = mt290Doc("MT290ADVICE00001", "MT202-OTHER-01", "USD15,00", "Correspondent handling fee\nFor payment processing")
	if _, err := applyMT290Doc(t, l, conflict); !errors.Is(err, ErrMT290MsgIDConflict) {
		t.Errorf("conflict MT202: err = %v, want ErrMT290MsgIDConflict", err)
	}
	// The stored advice is untouched, and no second marker.
	if list, ok := l.GetMT202Charges("MT202-REF-01"); !ok || len(list) != 1 || list[0].AmountCents != 1500 {
		t.Errorf("GetMT202Charges after conflicts = %v, want the one stored advice", list)
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt290_charges_advised") != 1 {
		t.Errorf("mt290_charges_advised count = %d, want 1 after conflicts", countOp(events, "mt290_charges_advised"))
	}
}

func TestApplyMT290_DoesNotPostJournalEntries(t *testing.T) {
	al := mustAuditLog(t)
	l := mt290ApplyLedger(t, al)
	before := l.Version()
	entriesBefore := len(l.Entries())

	if _, err := applyMT290Doc(t, l, mt290DocForMT202("MT290ADVICE00001")); err != nil {
		t.Fatalf("ApplyMT290: %v", err)
	}
	// The MT202's transfer stays exactly as booked: the
	// version is untouched, no journal entry appears, and both
	// legs hold their booked balances — an advice advances
	// nothing.
	if l.Version() != before {
		t.Errorf("version = %d, want %d (evidence-only, no bump)", l.Version(), before)
	}
	if len(l.Entries()) != entriesBefore {
		t.Errorf("journal entries = %d, want %d (no entry posted)", len(l.Entries()), entriesBefore)
	}
	if got := l.BalanceIn("nostro-acct", "USD"); got != 1000000-10000 {
		t.Errorf("nostro-acct balance = %d, want %d", got, 1000000-10000)
	}
	if got := l.BalanceIn("vostro-acct", "USD"); got != 10000 {
		t.Errorf("vostro-acct balance = %d, want 10000", got)
	}
}

func TestMT290Advice_GetByMT202(t *testing.T) {
	al := mustAuditLog(t)
	l := mt290ApplyLedger(t, al)

	if _, ok := l.GetMT202Charges("MT202-REF-01"); ok {
		t.Error("GetMT202Charges = true before any advice, want false")
	}
	res, err := applyMT290Doc(t, l, mt290DocForMT202("MT290ADVICE00001"))
	if err != nil {
		t.Fatalf("ApplyMT290: %v", err)
	}
	got, ok := l.GetMT202Charges("MT202-REF-01")
	if !ok {
		t.Fatal("GetMT202Charges = false after recording, want true")
	}
	if len(got) != 1 || got[0] != res.Advice {
		t.Errorf("GetMT202Charges = %+v, want [%+v]", got, res.Advice)
	}
	// The returned slice is a copy: mutating it leaves the
	// ledger's list alone.
	got[0].AmountCents = 999999
	again, ok := l.GetMT202Charges("MT202-REF-01")
	if !ok || len(again) != 1 || again[0].AmountCents != 1500 {
		t.Errorf("GetMT202Charges after mutation = %+v, want the stored 1500", again)
	}
	if _, ok := l.GetMT202Charges("MT202-NEVER-CHARGED"); ok {
		t.Error("GetMT202Charges = true for an uncharged MT202, want false")
	}
}
