package ledger

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func mt920Base(t *testing.T) MT920 {
	t.Helper()
	return MT920{
		SenderLT:             "BANKUS33",
		ReceiverLT:           "BANKDEFF",
		TransactionReference: "REQ-920-1",
		Sequences: []MT920Sequence{
			{RequestedType: "940", Account: "DE89370400440532013000"},
		},
	}
}

func mt920Floor(currency string, cents int64) *MT920FloorLimit {
	return &MT920FloorLimit{Currency: currency, AmountCents: cents}
}

func TestExportMT920_HappyPath(t *testing.T) {
	m := mt920Base(t)
	msg, err := ExportMT920(m)
	if err != nil {
		t.Fatalf("ExportMT920: %v", err)
	}
	want := "{1:F01BANKUS33XXX0000000000}\n" +
		"{2:I920BANKDEFFXXXU3003}\n" +
		"{3:{108:REQ-920-1}}\n" +
		"{4:\n" +
		":20:REQ-920-1\n" +
		":12:940\n" +
		":25:DE89370400440532013000\n" +
		"-}"
	if msg != want {
		t.Errorf("message = %q, want %q", msg, want)
	}
	if strings.Contains(msg, "{5:") {
		t.Error("block 5 must be omitted: CHK is network-assigned")
	}
	// Repeat exports are byte-identical.
	again, err := ExportMT920(m)
	if err != nil {
		t.Fatalf("ExportMT920 repeat: %v", err)
	}
	if again != msg {
		t.Error("repeat export is not byte-identical")
	}
}

func TestExportMT920_MultiSequenceCrossAccount(t *testing.T) {
	m := mt920Base(t)
	m.Sequences = []MT920Sequence{
		{RequestedType: "941", Account: "ACCT-ONE"},
		{RequestedType: "942", Account: "ACCT-TWO", FloorLimitDebit: mt920Floor("USD", 100000), FloorLimitCredit: mt920Floor("USD", 50000)},
		{RequestedType: "950"},
	}
	msg, err := ExportMT920(m)
	if err != nil {
		t.Fatalf("ExportMT920: %v", err)
	}
	want := "{1:F01BANKUS33XXX0000000000}\n" +
		"{2:I920BANKDEFFXXXU3003}\n" +
		"{3:{108:REQ-920-1}}\n" +
		"{4:\n" +
		":20:REQ-920-1\n" +
		":12:941\n" +
		":25:ACCT-ONE\n" +
		":12:942\n" +
		":25:ACCT-TWO\n" +
		":34F:USDD1000,00\n" +
		":34F:USDC500,00\n" +
		":12:950\n" +
		"-}"
	if msg != want {
		t.Errorf("message = %q, want %q", msg, want)
	}
}

func TestExportMT920_RejectsBadRequestedType(t *testing.T) {
	for _, bad := range []string{"", "94", "9400", "943", "95O", "940 "} {
		m := mt920Base(t)
		m.Sequences[0].RequestedType = bad
		if _, err := ExportMT920(m); !errors.Is(err, ErrInvalidMT920Export) {
			t.Errorf(":12: %q: err = %v, want ErrInvalidMT920Export", bad, err)
		}
	}
}

func TestExportMT920_Rejects942WithoutFloorLimit(t *testing.T) {
	m := mt920Base(t)
	m.Sequences[0].RequestedType = "942"
	if _, err := ExportMT920(m); !errors.Is(err, ErrInvalidMT920Export) {
		t.Errorf("942 without :34F:: err = %v, want ErrInvalidMT920Export", err)
	}
	// One floor limit satisfies the 942 rule — debit alone, or
	// credit alone.
	for _, seq := range []MT920Sequence{
		{RequestedType: "942", Account: "ACCT-TWO", FloorLimitDebit: mt920Floor("EUR", 25000)},
		{RequestedType: "942", Account: "ACCT-TWO", FloorLimitCredit: mt920Floor("EUR", 25000)},
	} {
		m.Sequences[0] = seq
		if _, err := ExportMT920(m); err != nil {
			t.Errorf("942 with one :34F:: err = %v, want nil", err)
		}
	}
	// A non-942 sequence needs no floor limit (and may carry one).
	m.Sequences[0] = MT920Sequence{RequestedType: "940", Account: "ACCT-ONE", FloorLimitDebit: mt920Floor("USD", 100)}
	if _, err := ExportMT920(m); err != nil {
		t.Errorf("940 with a floor limit: err = %v, want nil", err)
	}
}

func TestExportMT920_RejectsBadReferenceAndAccount(t *testing.T) {
	m := mt920Base(t)
	m.TransactionReference = ""
	if _, err := ExportMT920(m); !errors.Is(err, ErrInvalidMT920Export) {
		t.Errorf("empty :20:: err = %v, want ErrInvalidMT920Export", err)
	}
	m = mt920Base(t)
	m.TransactionReference = "REQ-920-1-TOO-LONG"
	if _, err := ExportMT920(m); !errors.Is(err, ErrInvalidMT920Export) {
		t.Errorf("overlong :20:: err = %v, want ErrInvalidMT920Export", err)
	}
	m = mt920Base(t)
	m.TransactionReference = "REQ/920//1"
	if _, err := ExportMT920(m); !errors.Is(err, ErrInvalidMT920Export) {
		t.Errorf("double-slash :20:: err = %v, want ErrInvalidMT920Export", err)
	}
	// :25: over 35 characters is refused, never truncated.
	m = mt920Base(t)
	m.Sequences[0].Account = strings.Repeat("A", 36)
	if _, err := ExportMT920(m); !errors.Is(err, ErrInvalidMT920Export) {
		t.Errorf("overlong :25:: err = %v, want ErrInvalidMT920Export", err)
	}
	// :25: with a slash is legal — account numbers carry them.
	m = mt920Base(t)
	m.Sequences[0].Account = "DE89/37040044"
	if _, err := ExportMT920(m); err != nil {
		t.Errorf(":25: with slash: err = %v, want nil", err)
	}
	// No sequences at all is refused.
	m = mt920Base(t)
	m.Sequences = nil
	if _, err := ExportMT920(m); !errors.Is(err, ErrInvalidMT920Export) {
		t.Errorf("no sequences: err = %v, want ErrInvalidMT920Export", err)
	}
	// A bad LT is refused.
	m = mt920Base(t)
	m.SenderLT = "NOT-A-BIC"
	if _, err := ExportMT920(m); !errors.Is(err, ErrInvalidMT920Export) {
		t.Errorf("bad sender LT: err = %v, want ErrInvalidMT920Export", err)
	}
}

func TestExportMT920_RejectsBadFloorLimit(t *testing.T) {
	cases := []struct {
		name string
		fl   *MT920FloorLimit
	}{
		{"missing currency", &MT920FloorLimit{Currency: "", AmountCents: 100}},
		{"lowercase currency", &MT920FloorLimit{Currency: "usd", AmountCents: 100}},
		{"short currency", &MT920FloorLimit{Currency: "US", AmountCents: 100}},
		{"negative amount", &MT920FloorLimit{Currency: "USD", AmountCents: -1}},
		{"amount over 15d", &MT920FloorLimit{Currency: "USD", AmountCents: mt920MaxFloorLimitCents}},
	}
	for _, tc := range cases {
		m := mt920Base(t)
		m.Sequences[0].RequestedType = "942"
		m.Sequences[0].FloorLimitDebit = tc.fl
		if _, err := ExportMT920(m); !errors.Is(err, ErrInvalidMT920Export) {
			t.Errorf("%s: err = %v, want ErrInvalidMT920Export", tc.name, err)
		}
	}
	// A zero floor limit is a limit, not a missing amount.
	m := mt920Base(t)
	m.Sequences[0].RequestedType = "942"
	m.Sequences[0].FloorLimitDebit = mt920Floor("USD", 0)
	msg, err := ExportMT920(m)
	if err != nil {
		t.Fatalf("zero floor limit: %v", err)
	}
	if !strings.Contains(msg, ":34F:USDD0,00") {
		t.Errorf("zero floor limit not rendered, message:\n%s", msg)
	}
}

func TestExportMT920_RecordsAndReplays(t *testing.T) {
	al := mustAuditLog(t)
	l := seedPayoutLedger(t, WithAuditLog(al))
	before := l.Version()
	m := mt920Base(t)
	m.Sequences = []MT920Sequence{
		{RequestedType: "940", Account: "ACCT-ONE"},
		{RequestedType: "942", Account: "ACCT-TWO", FloorLimitDebit: mt920Floor("USD", 100000)},
	}

	res, err := l.ExportMT920(m, "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT920: %v", err)
	}
	if res.Duplicate {
		t.Error("duplicate = true on first export")
	}
	if res.Reference != "REQ-920-1" {
		t.Errorf("reference = %q, want REQ-920-1", res.Reference)
	}
	if fmt.Sprint(res.Requested) != "[940 942]" {
		t.Errorf("requested = %v, want [940 942]", res.Requested)
	}
	if fmt.Sprint(res.Accounts) != "[ACCT-ONE ACCT-TWO]" {
		t.Errorf("accounts = %v, want [ACCT-ONE ACCT-TWO]", res.Accounts)
	}
	for _, want := range []string{
		":20:REQ-920-1",
		":12:940",
		":25:ACCT-ONE",
		":12:942",
		":34F:USDD1000,00",
		"{2:I920BANKDEFFXXXU3003}",
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
	stored, ok := l.GetMT920Export("REQ-920-1")
	if !ok {
		t.Fatal("GetMT920Export = false, want the stored export")
	}
	if stored.Message != res.Message || fmt.Sprint(stored.Requested) != "[940 942]" {
		t.Errorf("stored export = %+v, want the rendered message", stored)
	}
	if _, ok := l.GetMT920Export("no-such-export"); ok {
		t.Error("GetMT920Export unknown = true, want false")
	}
	// Audit: exactly one request marker, with the exact details —
	// the :20: on the durable record, and no accounts: the :25:
	// values name accounts at the servicing bank, not ledger
	// accounts.
	events := readAuditOps(t, al)
	if countOp(events, "mt920_request_exported") != 1 {
		t.Errorf("mt920_request_exported count = %d, want 1", countOp(events, "mt920_request_exported"))
	}
	ev := findOp(events, "mt920_request_exported")
	if ev == nil {
		t.Fatal("mt920_request_exported event missing")
	}
	if ev.Actor != "ledger-operator" {
		t.Errorf("actor = %q, want ledger-operator (ExportMT920)", ev.Actor)
	}
	if ev.TraceID != "REQ-920-1" {
		t.Errorf("trace = %q, want REQ-920-1", ev.TraceID)
	}
	if ev.Details["msg_id"] != "REQ-920-1" {
		t.Errorf("details msg_id = %v, want REQ-920-1", ev.Details["msg_id"])
	}
	// Details round-trip through JSON, so the slices come back as
	// []any — compare their printed form.
	if fmt.Sprint(ev.Details["requested"]) != "[940 942]" {
		t.Errorf("details requested = %v, want [940 942]", ev.Details["requested"])
	}
	if fmt.Sprint(ev.Details["account"]) != "[ACCT-ONE ACCT-TWO]" {
		t.Errorf("details account = %v, want [ACCT-ONE ACCT-TWO]", ev.Details["account"])
	}
	if len(ev.Accounts) != 0 {
		t.Errorf("accounts = %v, want none (evidence-only export)", ev.Accounts)
	}

	// Replay with the same terms returns the stored message with
	// Duplicate set and no second audit event.
	dup, err := l.ExportMT920(m, "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT920 replay: %v", err)
	}
	if !dup.Duplicate {
		t.Error("duplicate = false on replay, want true")
	}
	if dup.Message != res.Message {
		t.Error("replay message differs from the stored message")
	}
	if countOp(readAuditOps(t, al), "mt920_request_exported") != 1 {
		t.Error("replay emitted a second mt920_request_exported event, want exactly 1")
	}

	// Same :20:, different sequences: a conflict, never a silent
	// overwrite — and the stored message is untouched. The
	// conflict wraps the module sentinel (single-sentinel shape)
	// and is detectable as a conflict for the 422 mapping.
	conflict := m
	conflict.Sequences = []MT920Sequence{{RequestedType: "941", Account: "ACCT-ONE"}}
	_, err = l.ExportMT920(conflict, "ledger-operator")
	if !IsMT920MsgIDConflict(err) {
		t.Errorf("conflict err = %v, want MT920MsgIDConflictError", err)
	}
	if !errors.Is(err, ErrInvalidMT920Export) {
		t.Errorf("conflict err = %v, want it to wrap ErrInvalidMT920Export", err)
	}
	after, ok := l.GetMT920Export("REQ-920-1")
	if !ok || after.Message != res.Message {
		t.Error("conflicting export overwrote the stored message")
	}

	// A replay under different envelope LTs still returns the
	// first rendering: LTs are envelope, not terms.
	otherLT := m
	otherLT.SenderLT = "BANKFRPP"
	otherLT.ReceiverLT = "BANKJPJT"
	dupLT, err := l.ExportMT920(otherLT, "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT920 other-LT replay: %v", err)
	}
	if !dupLT.Duplicate || dupLT.Message != res.Message {
		t.Error("other-LT replay must return the first rendering with Duplicate set")
	}
}
