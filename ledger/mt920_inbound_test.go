package ledger

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// mt920InboundLedger seeds a payout ledger with an audit log, the
// fixture for the ApplyMT920 tests: the "saver" account carries
// journaled rows, so a :25: naming it resolves through
// mt940ResolveAccountLocked.
func mt920InboundLedger(t *testing.T, al *AuditLog) *Ledger {
	t.Helper()
	return seedPayoutLedger(t, WithAuditLog(al))
}

// mt920InboundDoc renders one inbound MT920 request through the
// outbound export — the Parse ∘ Export round-trip fixture.
func mt920InboundDoc(t *testing.T, m MT920) string {
	t.Helper()
	msg, err := ExportMT920(m)
	if err != nil {
		t.Fatalf("ExportMT920: %v", err)
	}
	return msg
}

// applyMT920Doc parses doc and applies it, failing the test on
// parse errors: the gates under test are the apply gates, not the
// parse gates.
func applyMT920Doc(t *testing.T, l *Ledger, doc string) (MT920ApplyResult, error) {
	t.Helper()
	m, err := ParseMT920(doc)
	if err != nil {
		t.Fatalf("ParseMT920: %v", err)
	}
	return l.ApplyMT920(m, "correspondent-bank")
}

// mt920InboundRequest builds a multi-sequence inbound request:
// a 940 for the seeded "saver" account, a 942 with both floor
// limits, and a 950 with no account at all.
func mt920InboundRequest() MT920 {
	return MT920{
		SenderLT:             "BANKUS33",
		ReceiverLT:           "BANKDEFF",
		TransactionReference: "REQ-920-IN-1",
		Sequences: []MT920Sequence{
			{RequestedType: "940", Account: "saver"},
			{RequestedType: "942", Account: "saver",
				FloorLimitDebit:  mt920Floor("USD", 100000),
				FloorLimitCredit: mt920Floor("USD", 50000)},
			{RequestedType: "950"},
		},
	}
}

func TestParseMT920_RoundTripExport(t *testing.T) {
	want := mt920InboundRequest()
	// Canonical 12-character LTs, so the round-trip is
	// byte-identical: the export normalizes a BIC8 LT to 11
	// characters, and re-exporting that 11-character form would
	// normalize it once more to 12 (the MT999 double-"X" note)
	// — pre-existing export semantics, not a parse defect.
	want.SenderLT = "BANKUS33XXXX"
	want.ReceiverLT = "BANKDEFFXXXX"
	doc := mt920InboundDoc(t, want)

	got, err := ParseMT920(doc)
	if err != nil {
		t.Fatalf("ParseMT920: %v", err)
	}
	// The parse returns the LTs exactly as rendered.
	if got.SenderLT != "BANKUS33XXXX" || got.ReceiverLT != "BANKDEFFXXXX" {
		t.Errorf("LTs = %q/%q, want BANKUS33XXXX/BANKDEFFXXXX", got.SenderLT, got.ReceiverLT)
	}
	if got.TransactionReference != want.TransactionReference {
		t.Errorf("reference = %q, want %q", got.TransactionReference, want.TransactionReference)
	}
	if !reflect.DeepEqual(got.Sequences, want.Sequences) {
		t.Errorf("sequences = %+v, want %+v", got.Sequences, want.Sequences)
	}
	// Re-rendering the parsed message reproduces the export
	// byte-for-byte: Parse is the strict inverse of Export.
	again, err := ExportMT920(got)
	if err != nil {
		t.Fatalf("ExportMT920(parsed): %v", err)
	}
	if again != doc {
		t.Errorf("re-render = %q, want %q", again, doc)
	}
}

func TestParseMT920_CRLFTolerant(t *testing.T) {
	doc := strings.ReplaceAll(mt920InboundDoc(t, mt920InboundRequest()), "\n", "\r\n")
	got, err := ParseMT920(doc)
	if err != nil {
		t.Fatalf("ParseMT920 CRLF: %v", err)
	}
	if len(got.Sequences) != 3 || got.Sequences[1].RequestedType != "942" {
		t.Errorf("sequences = %+v, want the three-sequence request", got.Sequences)
	}
}

func TestParseMT920_RejectsIllegalRequestedType(t *testing.T) {
	// Export refuses it too, so hand-build the document.
	doc := "{1:F01BANKUS33XXX0000000000}\n" +
		"{2:I920BANKDEFFXXXU3003}\n" +
		"{3:{108:REQ-920-BAD-12}}\n" +
		"{4:\n:20:REQ-920-BAD-12\n:12:943\n:25:saver\n-}"
	if _, err := ParseMT920(doc); !errors.Is(err, ErrInvalidMT920Export) {
		t.Errorf("err = %v, want ErrInvalidMT920Export", err)
	}
}

func TestParseMT920_Rejects942WithoutFloorLimit(t *testing.T) {
	doc := "{1:F01BANKUS33XXX0000000000}\n" +
		"{2:I920BANKDEFFXXXU3003}\n" +
		"{3:{108:REQ-920-BAD-942}}\n" +
		"{4:\n:20:REQ-920-BAD-942\n:12:942\n:25:saver\n-}"
	if _, err := ParseMT920(doc); !errors.Is(err, ErrInvalidMT920Export) {
		t.Errorf("err = %v, want ErrInvalidMT920Export", err)
	}
}

func TestParseMT920_RejectsOutOfOrderSequence(t *testing.T) {
	head := "{1:F01BANKUS33XXX0000000000}\n" +
		"{2:I920BANKDEFFXXXU3003}\n" +
		"{3:{108:REQ-920-BAD-ORD}}\n{4:\n:20:REQ-920-BAD-ORD\n"
	cases := map[string]string{
		// A :25: before any :12: starts no sequence.
		"account before type": head + ":25:saver\n:12:940\n-}",
		// A :25: after a :34F: belongs to no sequence start.
		"account after floor limit": head + ":12:940\n:34F:USD D100,00\n:25:saver\n-}",
		// Two :25: lines in one sequence: a repeated field.
		"duplicate account": head + ":12:940\n:25:saver\n:25:saver\n-}",
		// Two debit :34F: lines in one sequence.
		"duplicate debit floor limit": head + ":12:942\n:34F:USDD100,00\n:34F:USDD200,00\n-}",
		// An unknown tag: a request must not lose content quietly.
		"unknown tag": head + ":12:940\n:32A:260101USD100,00\n-}",
	}
	for name, doc := range cases {
		if _, err := ParseMT920(doc); !errors.Is(err, ErrInvalidMT920Export) {
			t.Errorf("%s: err = %v, want ErrInvalidMT920Export", name, err)
		}
	}
}

func TestParseMT920_RejectsMURMismatch(t *testing.T) {
	doc := "{1:F01BANKUS33XXX0000000000}\n" +
		"{2:I920BANKDEFFXXXU3003}\n" +
		"{3:{108:REQ-920-OTHER}}\n" +
		"{4:\n:20:REQ-920-BAD-MUR\n:12:940\n-}"
	if _, err := ParseMT920(doc); !errors.Is(err, ErrInvalidMT920Export) {
		t.Errorf("err = %v, want ErrInvalidMT920Export", err)
	}
}

func TestParseMT920_ZeroFloorLimitIsALimit(t *testing.T) {
	m := mt920InboundRequest()
	m.TransactionReference = "REQ-920-ZERO"
	m.Sequences = []MT920Sequence{
		{RequestedType: "942", Account: "saver", FloorLimitDebit: mt920Floor("USD", 0)},
	}
	got, err := ParseMT920(mt920InboundDoc(t, m))
	if err != nil {
		t.Fatalf("ParseMT920: %v", err)
	}
	fl := got.Sequences[0].FloorLimitDebit
	if fl == nil || fl.AmountCents != 0 {
		t.Errorf("floor limit = %+v, want USD 0 (a limit, not a missing amount)", fl)
	}
}

func TestApplyMT920_RegistersKnownAccount(t *testing.T) {
	al := mustAuditLog(t)
	l := mt920InboundLedger(t, al)
	before := l.Version()
	entriesBefore := len(l.Entries())

	res, err := applyMT920Doc(t, l, mt920InboundDoc(t, mt920InboundRequest()))
	if err != nil {
		t.Fatalf("ApplyMT920: %v", err)
	}
	if res.Duplicate {
		t.Error("duplicate = true on first apply")
	}
	if res.Reference != "REQ-920-IN-1" {
		t.Errorf("reference = %q, want REQ-920-IN-1", res.Reference)
	}
	if fmt.Sprint(res.Requested) != "[940 942 950]" {
		t.Errorf("requested = %v, want [940 942 950]", res.Requested)
	}
	if fmt.Sprint(res.Accounts) != "[saver saver]" {
		t.Errorf("accounts = %v, want [saver saver]", res.Accounts)
	}
	if len(res.Sequences) != 3 || res.Sequences[1].FloorLimitDebit == nil ||
		res.Sequences[1].FloorLimitDebit.AmountCents != 100000 {
		t.Errorf("sequences = %+v, want the three-sequence request", res.Sequences)
	}
	// Registering is read-only: no version bump, no journal rows.
	if l.Version() != before {
		t.Errorf("version = %d, want %d (read-only, no bump)", l.Version(), before)
	}
	if len(l.Entries()) != entriesBefore {
		t.Errorf("entries = %d, want %d (no journal rows)", len(l.Entries()), entriesBefore)
	}
	// The stored request round-trips through the getter.
	stored, ok := l.GetMT920Received("REQ-920-IN-1")
	if !ok {
		t.Fatal("GetMT920Received = false, want the stored request")
	}
	if !reflect.DeepEqual(stored.Sequences, res.Sequences) {
		t.Errorf("stored sequences = %+v, want %+v", stored.Sequences, res.Sequences)
	}
	if _, ok := l.GetMT920Received("no-such-request"); ok {
		t.Error("GetMT920Received unknown = true, want false")
	}
	// Audit: exactly one received marker, with the exact details
	// — the :20: on the durable record, and no accounts: the
	// :25: values ride in the event details instead, along the
	// mt920_request_exported shape.
	events := readAuditOps(t, al)
	if countOp(events, "mt920_request_received") != 1 {
		t.Errorf("mt920_request_received count = %d, want 1", countOp(events, "mt920_request_received"))
	}
	ev := findOp(events, "mt920_request_received")
	if ev == nil {
		t.Fatal("mt920_request_received event missing")
	}
	if ev.Actor != "correspondent-bank" {
		t.Errorf("actor = %q, want correspondent-bank (ApplyMT920)", ev.Actor)
	}
	if ev.TraceID != "REQ-920-IN-1" {
		t.Errorf("trace = %q, want REQ-920-IN-1", ev.TraceID)
	}
	if ev.Details["msg_id"] != "REQ-920-IN-1" ||
		fmt.Sprint(ev.Details["requested"]) != "[940 942 950]" ||
		fmt.Sprint(ev.Details["account"]) != "[saver saver]" {
		t.Errorf("details = %v, want msg/requested/account", ev.Details)
	}
	if ev.VersionBefore != ev.VersionAfter || ev.VersionAfter != before {
		t.Errorf("version %d -> %d, want %d -> %d (no bump)", ev.VersionBefore, ev.VersionAfter, before, before)
	}
	if len(ev.Accounts) != 0 {
		t.Errorf("accounts = %v, want none (evidence-only registration)", ev.Accounts)
	}
}

func TestApplyMT920_NoAccountSequenceRegisters(t *testing.T) {
	al := mustAuditLog(t)
	l := mt920InboundLedger(t, al)
	m := mt920InboundRequest()
	m.TransactionReference = "REQ-920-NO-ACCT"
	m.Sequences = []MT920Sequence{{RequestedType: "950"}}

	res, err := applyMT920Doc(t, l, mt920InboundDoc(t, m))
	if err != nil {
		t.Fatalf("ApplyMT920: %v", err)
	}
	if len(res.Accounts) != 0 {
		t.Errorf("accounts = %v, want none (account-less request needs no anchor)", res.Accounts)
	}
	if _, ok := l.GetMT920Received("REQ-920-NO-ACCT"); !ok {
		t.Error("GetMT920Received = false, want the account-less request registered")
	}
}

func TestApplyMT920_UnknownAccountGoesToRepair(t *testing.T) {
	al := mustAuditLog(t)
	l := mt920InboundLedger(t, al)
	m := mt920InboundRequest()
	m.TransactionReference = "REQ-920-UNKNOWN"
	m.Sequences = []MT920Sequence{{RequestedType: "940", Account: "NO-SUCH-ACCOUNT"}}

	res, err := applyMT920Doc(t, l, mt920InboundDoc(t, m))
	if !errors.Is(err, ErrMT920UnknownAccount) {
		t.Errorf("err = %v, want ErrMT920UnknownAccount", err)
	}
	// The parked result carries the repair ID alongside the
	// error: the request is visible to an operator, not dropped.
	if res.RepairID == "" {
		t.Error("repair_id empty, want the parked repair record")
	}
	if res.Reference != "REQ-920-UNKNOWN" || fmt.Sprint(res.Accounts) != "[NO-SUCH-ACCOUNT]" {
		t.Errorf("parked result = %+v, want reference + accounts", res)
	}
	rec, ok := l.GetRepair(res.RepairID)
	if !ok {
		t.Fatalf("GetRepair(%q) = false, want the parked record", res.RepairID)
	}
	if rec.ReasonCode != RepairReasonUnknownRoute {
		t.Errorf("repair reason = %q, want unknown_route", rec.ReasonCode)
	}
	if rec.Instruction.ID != "NO-SUCH-ACCOUNT" {
		t.Errorf("repair instruction id = %q, want the unknown :25:", rec.Instruction.ID)
	}
	if rec.Instruction.Destination != "mt920-inbound:REQ-920-UNKNOWN" {
		t.Errorf("repair destination = %q, want mt920-inbound:REQ-920-UNKNOWN", rec.Instruction.Destination)
	}
	// No request was recorded: nothing to replay, and no
	// received marker event — the repair_enqueued event is the
	// record.
	if _, ok := l.GetMT920Received("REQ-920-UNKNOWN"); ok {
		t.Error("GetMT920Received = true for a parked request, want false")
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt920_request_received") != 0 {
		t.Errorf("mt920_request_received count = %d, want 0", countOp(events, "mt920_request_received"))
	}
	if countOp(events, "repair_enqueued") != 1 {
		t.Errorf("repair_enqueued count = %d, want 1", countOp(events, "repair_enqueued"))
	}
}

func TestApplyMT920_ReplaysSameTerms(t *testing.T) {
	al := mustAuditLog(t)
	l := mt920InboundLedger(t, al)
	doc := mt920InboundDoc(t, mt920InboundRequest())

	first, err := applyMT920Doc(t, l, doc)
	if err != nil {
		t.Fatalf("ApplyMT920: %v", err)
	}
	dup, err := applyMT920Doc(t, l, doc)
	if err != nil {
		t.Fatalf("ApplyMT920 replay: %v", err)
	}
	if !dup.Duplicate {
		t.Error("duplicate = false on replay, want true")
	}
	if !reflect.DeepEqual(dup.Sequences, first.Sequences) {
		t.Error("replay outcome differs from the stored outcome")
	}
	// A replay under different LTs is still a replay: the LTs
	// are envelope, not terms.
	otherLT := mt920InboundRequest()
	otherLT.SenderLT = "BANKGB22"
	otherLT.ReceiverLT = "BANKFRPP"
	dupLT, err := applyMT920Doc(t, l, mt920InboundDoc(t, otherLT))
	if err != nil {
		t.Fatalf("ApplyMT920 replay under other LTs: %v", err)
	}
	if !dupLT.Duplicate {
		t.Error("duplicate = false on replay under other LTs, want true")
	}
	if countOp(readAuditOps(t, al), "mt920_request_received") != 1 {
		t.Error("replay emitted a second mt920_request_received event, want exactly 1")
	}
}

func TestApplyMT920_ConflictOnDifferentTerms(t *testing.T) {
	al := mustAuditLog(t)
	l := mt920InboundLedger(t, al)

	if _, err := applyMT920Doc(t, l, mt920InboundDoc(t, mt920InboundRequest())); err != nil {
		t.Fatalf("ApplyMT920: %v", err)
	}
	// Same :20:, different :12:: a conflict, never a silent
	// overwrite — and the stored request is untouched.
	other := mt920InboundRequest()
	other.Sequences = []MT920Sequence{{RequestedType: "941", Account: "saver"}}
	if _, err := applyMT920Doc(t, l, mt920InboundDoc(t, other)); !IsMT920MsgIDConflict(err) {
		t.Errorf("conflict err = %v, want MT920MsgIDConflictError", err)
	}
	stored, ok := l.GetMT920Received("REQ-920-IN-1")
	if !ok || fmt.Sprint(stored.Requested) != "[940 942 950]" {
		t.Errorf("stored = %+v, want the first request untouched", stored)
	}
	if countOp(readAuditOps(t, al), "mt920_request_received") != 1 {
		t.Error("conflicting apply emitted a second mt920_request_received event, want exactly 1")
	}
}

func TestApplyMT920_GetterReturnsOriginalSequences(t *testing.T) {
	al := mustAuditLog(t)
	l := mt920InboundLedger(t, al)
	want := mt920InboundRequest()

	if _, err := applyMT920Doc(t, l, mt920InboundDoc(t, want)); err != nil {
		t.Fatalf("ApplyMT920: %v", err)
	}
	stored, ok := l.GetMT920Received("REQ-920-IN-1")
	if !ok {
		t.Fatal("GetMT920Received = false, want the stored request")
	}
	if len(stored.Sequences) != 3 {
		t.Fatalf("sequences = %+v, want 3", stored.Sequences)
	}
	fl := stored.Sequences[1].FloorLimitCredit
	if fl == nil || fl.Currency != "USD" || fl.AmountCents != 50000 {
		t.Errorf("credit floor limit = %+v, want USD 50000", fl)
	}
	if stored.Sequences[2].Account != "" {
		t.Errorf("third sequence account = %q, want empty (no :25:)", stored.Sequences[2].Account)
	}
	// The returned sequences are copies: mutating them cannot
	// corrupt the registry.
	stored.Sequences[1].FloorLimitCredit.AmountCents = 1
	again, ok := l.GetMT920Received("REQ-920-IN-1")
	if !ok || again.Sequences[1].FloorLimitCredit.AmountCents != 50000 {
		t.Errorf("getter returned a live floor limit, want a copy: %+v", again.Sequences)
	}
}

func TestApplyMT920_RejectsInvalidMessage(t *testing.T) {
	al := mustAuditLog(t)
	l := mt920InboundLedger(t, al)

	// No sequences: a request asking for nothing is not a request.
	if _, err := l.ApplyMT920(MT920{
		SenderLT:             "BANKUS33",
		ReceiverLT:           "BANKDEFF",
		TransactionReference: "REQ-920-BAD-1",
	}, "correspondent-bank"); !errors.Is(err, ErrInvalidMT920Export) {
		t.Errorf("empty sequences err = %v, want ErrInvalidMT920Export", err)
	}
	// A :20: over 16 characters is refused at the apply gate,
	// so hand-built requests share the parse gate.
	if _, err := l.ApplyMT920(MT920{
		SenderLT:             "BANKUS33",
		ReceiverLT:           "BANKDEFF",
		TransactionReference: "REQ-920-REFERENCE-TOO-LONG",
		Sequences:            []MT920Sequence{{RequestedType: "940"}},
	}, "correspondent-bank"); !errors.Is(err, ErrInvalidMT920Export) {
		t.Errorf("long :20: err = %v, want ErrInvalidMT920Export", err)
	}
	// Nothing was recorded for either refusal.
	if _, ok := l.GetMT920Received("REQ-920-BAD-1"); ok {
		t.Error("invalid apply recorded a request, want none")
	}
	if countOp(readAuditOps(t, al), "mt920_request_received") != 0 {
		t.Error("invalid apply emitted mt920_request_received, want none")
	}
}
