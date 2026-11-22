package ledger

import (
	"errors"
	"strings"
	"testing"
)

func mt999Base(t *testing.T) MT999 {
	t.Helper()
	return MT999{
		SenderLT:             "BANKUS33",
		ReceiverLT:           "BANKDEFF",
		TransactionReference: "FREE-999-1",
		RelatedReference:     "po-mt999-1",
		Narrative:            []string{"PLEASE ADVISE CASH POSITION", "FOR ACCOUNT po-mt999-1"},
	}
}

func TestExportMT999_HappyPath(t *testing.T) {
	m := mt999Base(t)
	msg, err := ExportMT999(m)
	if err != nil {
		t.Fatalf("ExportMT999: %v", err)
	}
	want := "{1:F01BANKUS33XXX0000000000}\n" +
		"{2:I999BANKDEFFXXXU3003}\n" +
		"{3:{108:FREE-999-1}}\n" +
		"{4:\n" +
		":20:FREE-999-1\n" +
		":21:po-mt999-1\n" +
		":79:PLEASE ADVISE CASH POSITION\n" +
		"FOR ACCOUNT po-mt999-1\n" +
		"-}"
	if msg != want {
		t.Errorf("message = %q, want %q", msg, want)
	}
	if strings.Contains(msg, "{5:") {
		t.Error("block 5 must be omitted: CHK is network-assigned")
	}
	if strings.Contains(msg, ":32A:") {
		t.Error("an MT999 must never carry a value field")
	}
	// Round-trip: parse returns the same business fields.
	got, err := ParseMT999(msg)
	if err != nil {
		t.Fatalf("ParseMT999: %v", err)
	}
	if got.TransactionReference != "FREE-999-1" || got.RelatedReference != "po-mt999-1" ||
		len(got.Narrative) != 2 || got.Narrative[1] != "FOR ACCOUNT po-mt999-1" {
		t.Errorf("round-trip = %+v", got)
	}
	// Repeat exports are byte-identical.
	again, err := ExportMT999(m)
	if err != nil {
		t.Fatalf("ExportMT999 repeat: %v", err)
	}
	if again != msg {
		t.Error("repeat export is not byte-identical")
	}
}

func TestExportMT999_NoRelatedReferenceAndCRLF(t *testing.T) {
	m := mt999Base(t)
	m.RelatedReference = ""
	msg, err := ExportMT999(m)
	if err != nil {
		t.Fatalf("ExportMT999: %v", err)
	}
	if strings.Contains(msg, ":21:") {
		t.Error(":21: must be omitted when empty")
	}
	got, err := ParseMT999(strings.ReplaceAll(msg, "\n", "\r\n") + "\n")
	if err != nil {
		t.Fatalf("ParseMT999 CRLF: %v", err)
	}
	if got.RelatedReference != "" || len(got.Narrative) != 2 {
		t.Errorf("CRLF parse = %+v", got)
	}
}

func TestExportMT999_Validation(t *testing.T) {
	base := mt999Base(t)
	cases := []struct {
		name string
		mut  func(*MT999)
		want error
	}{
		{"empty :20:", func(m *MT999) { m.TransactionReference = "" }, ErrMT999Reference},
		{"long :20:", func(m *MT999) { m.TransactionReference = "12345678901234567" }, ErrMT999Reference},
		{"long :21:", func(m *MT999) { m.RelatedReference = "12345678901234567" }, ErrMT999Reference},
		{"empty narrative", func(m *MT999) { m.Narrative = nil }, ErrMT999Narrative},
		{"empty narrative line", func(m *MT999) { m.Narrative = []string{""} }, ErrMT999Narrative},
		{"long narrative line", func(m *MT999) { m.Narrative = []string{strings.Repeat("x", 36)} }, ErrMT999Narrative},
		{"too many narrative lines", func(m *MT999) {
			m.Narrative = make([]string, 51)
			for i := range m.Narrative {
				m.Narrative[i] = "x"
			}
		}, ErrMT999Narrative},
		{"brace in narrative", func(m *MT999) { m.Narrative = []string{"BAD {BRACE}"} }, ErrMT999Narrative},
		{"bad sender LT", func(m *MT999) { m.SenderLT = "XX" }, ErrMT999LT},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := base
			c.mut(&m)
			if _, err := ExportMT999(m); !errors.Is(err, c.want) {
				t.Errorf("err = %v, want %v", err, c.want)
			}
		})
	}
}

func TestParseMT999_Rejections(t *testing.T) {
	base, err := ExportMT999(mt999Base(t))
	if err != nil {
		t.Fatalf("ExportMT999: %v", err)
	}
	cases := []struct {
		name string
		msg  string
		want error
	}{
		{"I199 is not category 9", strings.Replace(base, "I999", "I199", 1), ErrMT999Framing},
		{"missing :79:", strings.Replace(base, ":79:PLEASE ADVISE CASH POSITION\nFOR ACCOUNT po-mt999-1\n", "", 1), ErrMT999Field},
		{"valued field before :79:", strings.Replace(base, ":79:", ":32A:261024USD2500,00\n:79:", 1), ErrMT999Field},
		{"out of order :21: first", strings.Replace(base, ":20:FREE-999-1\n:21:po-mt999-1", ":21:po-mt999-1\n:20:FREE-999-1", 1), ErrMT999Field},
		{"empty :21:", strings.Replace(base, ":21:po-mt999-1", ":21:", 1), ErrMT999Field},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := ParseMT999(c.msg); !errors.Is(err, c.want) {
				t.Errorf("err = %v, want %v", err, c.want)
			}
		})
	}
	// A :79: continuation line starting with a colon is narrative
	// data, not a new field: it must round-trip (same rule as
	// MT900/MT910's :72:).
	m := mt999Base(t)
	m.Narrative = []string{"FIRST LINE", ":SECOND LOOKS LIKE A FIELD"}
	msg, err := ExportMT999(m)
	if err != nil {
		t.Fatalf("ExportMT999: %v", err)
	}
	got, err := ParseMT999(msg)
	if err != nil {
		t.Fatalf("ParseMT999 colon continuation: %v", err)
	}
	if len(got.Narrative) != 2 || got.Narrative[1] != ":SECOND LOOKS LIKE A FIELD" {
		t.Errorf(":79: = %v", got.Narrative)
	}
}

func TestExportMT999_RecordsAndReplays(t *testing.T) {
	al := mustAuditLog(t)
	l := seedPayoutLedger(t, WithAuditLog(al))
	before := l.Version()
	m := mt999Base(t)

	res, err := l.ExportMT999(m, "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT999: %v", err)
	}
	if res.Duplicate {
		t.Error("duplicate = true on first export")
	}
	if res.Reference != "FREE-999-1" {
		t.Errorf("reference = %q, want FREE-999-1", res.Reference)
	}
	if res.RelatedReference != "po-mt999-1" {
		t.Errorf("related_reference = %q, want po-mt999-1", res.RelatedReference)
	}
	for _, want := range []string{
		":20:FREE-999-1",
		":21:po-mt999-1",
		":79:PLEASE ADVISE CASH POSITION",
		"{2:I999BANKDEFFXXXU3003}",
	} {
		if !strings.Contains(res.Message, want) {
			t.Errorf("message missing %q:\n%s", want, res.Message)
		}
	}
	// Recording is evidence-only: no version bump.
	if l.Version() != before {
		t.Errorf("version = %d, want %d (read-only, no bump)", l.Version(), before)
	}
	// The stored export round-trips through the registry.
	stored, ok := l.GetMT999Export("FREE-999-1")
	if !ok {
		t.Fatal("GetMT999Export = false, want the stored export")
	}
	if stored.Message != res.Message || stored.RelatedReference != "po-mt999-1" {
		t.Errorf("stored export = %+v, want the rendered message", stored)
	}
	if _, ok := l.GetMT999Export("no-such-export"); ok {
		t.Error("GetMT999Export unknown = true, want false")
	}
	// Audit: exactly one export marker, with the exact details —
	// the :20: on the durable record, and no accounts: a
	// free-format note touches no ledger account.
	events := readAuditOps(t, al)
	if countOp(events, "mt999_exported") != 1 {
		t.Errorf("mt999_exported count = %d, want 1", countOp(events, "mt999_exported"))
	}
	ev := findOp(events, "mt999_exported")
	if ev == nil {
		t.Fatal("mt999_exported event missing")
	}
	if ev.Actor != "ledger-operator" {
		t.Errorf("actor = %q, want ledger-operator (ExportMT999)", ev.Actor)
	}
	if ev.TraceID != "FREE-999-1" {
		t.Errorf("trace = %q, want FREE-999-1", ev.TraceID)
	}
	if ev.Details["msg_id"] != "FREE-999-1" || ev.Details["related_reference"] != "po-mt999-1" {
		t.Errorf("details = %v, want msg/related", ev.Details)
	}
	if len(ev.Accounts) != 0 {
		t.Errorf("accounts = %v, want none (evidence-only export)", ev.Accounts)
	}

	// Replay with the same terms returns the stored message with
	// Duplicate set and no second audit event.
	dup, err := l.ExportMT999(m, "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT999 replay: %v", err)
	}
	if !dup.Duplicate {
		t.Error("duplicate = false on replay, want true")
	}
	if dup.Message != res.Message {
		t.Error("replay message differs from the stored message")
	}
	if countOp(readAuditOps(t, al), "mt999_exported") != 1 {
		t.Error("replay emitted a second mt999_exported event, want exactly 1")
	}

	// Same :20:, different narrative: a conflict, never a silent
	// overwrite — and the stored message is untouched.
	conflict := m
	conflict.Narrative = []string{"A different note"}
	if _, err := l.ExportMT999(conflict, "ledger-operator"); !errors.Is(err, ErrMT999MsgIDConflict) {
		t.Errorf("conflict err = %v, want ErrMT999MsgIDConflict", err)
	}
	after, ok := l.GetMT999Export("FREE-999-1")
	if !ok || after.Message != res.Message {
		t.Error("conflicting export overwrote the stored message")
	}

	// Same :20:, different :21:: also a conflict.
	conflictRef := m
	conflictRef.RelatedReference = "another-anchor"
	if _, err := l.ExportMT999(conflictRef, "ledger-operator"); !errors.Is(err, ErrMT999MsgIDConflict) {
		t.Errorf("related-reference conflict err = %v, want ErrMT999MsgIDConflict", err)
	}

	// A replay under different envelope LTs still returns the
	// first rendering: LTs are envelope, not terms.
	envelope := m
	envelope.SenderLT = "CHASUS33"
	envelope.ReceiverLT = "DEUTDEFF"
	env, err := l.ExportMT999(envelope, "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT999 envelope replay: %v", err)
	}
	if !env.Duplicate || env.Message != res.Message {
		t.Errorf("envelope replay = %+v, want the stored message", env)
	}
}
