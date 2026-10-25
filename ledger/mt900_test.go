package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func mt900910Base(t *testing.T) MT900910 {
	t.Helper()
	return MT900910{
		MessageType:          "900",
		SenderLT:             "BANKUS33",
		ReceiverLT:           "BANKDEFF",
		TransactionReference: "CONF-900-1",
		RelatedReference:     "ORIG-202",
		Account:              "12345678",
		ValueDate:            time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC),
		Currency:             "USD",
		AmountCents:          250_000,
	}
}

func TestExportMT900910_HappyPath(t *testing.T) {
	m := mt900910Base(t)
	m.OrderingInstitutionBIC = "BANKUS33"
	m.SenderToReceiverInfo = []string{"DEBIT CONFIRMED", "VALUE 2026-10-24"}

	msg, err := ExportMT900910(m)
	if err != nil {
		t.Fatalf("ExportMT900910: %v", err)
	}
	want := "{1:F01BANKUS33XXX0000000000}\n" +
		"{2:I900BANKDEFFXXXU3003}\n" +
		"{3:{108:CONF-900-1}}\n" +
		"{4:\n" +
		":20:CONF-900-1\n" +
		":21:ORIG-202\n" +
		":25:12345678\n" +
		":32A:261024USD2500,00\n" +
		":52A:BANKUS33\n" +
		":72:DEBIT CONFIRMED\n" +
		"VALUE 2026-10-24\n" +
		"-}"
	if msg != want {
		t.Errorf("message = %q, want %q", msg, want)
	}
	if strings.Contains(msg, "{5:") {
		t.Error("block 5 must be omitted: CHK is network-assigned")
	}
	// Round-trip: parse returns the same business fields.
	got, err := ParseMT900910(msg)
	if err != nil {
		t.Fatalf("ParseMT900910: %v", err)
	}
	if got.MessageType != "900" || got.TransactionReference != "CONF-900-1" ||
		got.RelatedReference != "ORIG-202" || got.Account != "12345678" ||
		got.Currency != "USD" || got.AmountCents != 250_000 ||
		got.OrderingInstitutionBIC != "BANKUS33" ||
		len(got.SenderToReceiverInfo) != 2 || got.SenderToReceiverInfo[1] != "VALUE 2026-10-24" {
		t.Errorf("round-trip = %+v", got)
	}
	if !got.ValueDate.Equal(m.ValueDate) {
		t.Errorf("value date = %v, want %v", got.ValueDate, m.ValueDate)
	}
	// Repeat exports are byte-identical.
	again, err := ExportMT900910(m)
	if err != nil {
		t.Fatalf("ExportMT900910 repeat: %v", err)
	}
	if again != msg {
		t.Error("repeat export is not byte-identical")
	}
}

func TestExportMT900910_CreditVariantAndOptionalOmission(t *testing.T) {
	m := mt900910Base(t)
	m.MessageType = "910"
	msg, err := ExportMT900910(m)
	if err != nil {
		t.Fatalf("ExportMT900910: %v", err)
	}
	if !strings.Contains(msg, "{2:I910BANKDEFFXXXU3003}\n") {
		t.Error("block 2 is not I910")
	}
	if strings.Contains(msg, ":52A:") || strings.Contains(msg, ":72:") {
		t.Error("optional :52A:/:72: must be omitted when empty")
	}
	got, err := ParseMT900910(msg)
	if err != nil {
		t.Fatalf("ParseMT900910: %v", err)
	}
	if got.MessageType != "910" {
		t.Errorf("message type = %q, want 910", got.MessageType)
	}
	// CRLF input parses identically.
	gotCRLF, err := ParseMT900910(strings.ReplaceAll(msg, "\n", "\r\n") + "\n")
	if err != nil {
		t.Fatalf("ParseMT900910 CRLF: %v", err)
	}
	if gotCRLF.TransactionReference != m.TransactionReference {
		t.Errorf("CRLF parse reference = %q", gotCRLF.TransactionReference)
	}
}

func TestExportMT900910_Validation(t *testing.T) {
	base := mt900910Base(t)
	cases := []struct {
		name string
		mut  func(*MT900910)
		want error
	}{
		{"bad type", func(m *MT900910) { m.MessageType = "901" }, ErrMT900910Type},
		{"empty type", func(m *MT900910) { m.MessageType = "" }, ErrMT900910Type},
		{"empty :20:", func(m *MT900910) { m.TransactionReference = "" }, ErrMT900910Reference},
		{"long :21:", func(m *MT900910) { m.RelatedReference = "12345678901234567" }, ErrMT900910Reference},
		{"slash :20:", func(m *MT900910) { m.TransactionReference = "/CONF" }, ErrMT900910Reference},
		{"empty :25:", func(m *MT900910) { m.Account = "" }, ErrMT900910Account},
		{"long :25:", func(m *MT900910) { m.Account = strings.Repeat("1", 36) }, ErrMT900910Account},
		{"zero value date", func(m *MT900910) { m.ValueDate = time.Time{} }, ErrMT900910ValueDate},
		{"lowercase currency", func(m *MT900910) { m.Currency = "usd" }, ErrMT900910Currency},
		{"zero amount", func(m *MT900910) { m.AmountCents = 0 }, ErrMT900910Amount},
		{"bad BIC", func(m *MT900910) { m.OrderingInstitutionBIC = "NOTABIC" }, ErrMT900910BIC},
		{"long :72: line", func(m *MT900910) { m.SenderToReceiverInfo = []string{strings.Repeat("x", 36)} }, ErrMT900910Info},
		{"too many :72: lines", func(m *MT900910) {
			m.SenderToReceiverInfo = []string{"a", "b", "c", "d", "e", "f", "g"}
		}, ErrMT900910Info},
		{"bad sender LT", func(m *MT900910) { m.SenderLT = "XX" }, ErrMT900910LT},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := base
			c.mut(&m)
			if _, err := ExportMT900910(m); !errors.Is(err, c.want) {
				t.Errorf("err = %v, want %v", err, c.want)
			}
		})
	}
}

func TestParseMT900910_Rejections(t *testing.T) {
	base, err := ExportMT900910(mt900910Base(t))
	if err != nil {
		t.Fatalf("ExportMT900910: %v", err)
	}
	cases := []struct {
		name string
		msg  string
		want error
	}{
		{"I202 is not a confirmation", strings.Replace(base, "I900", "I202", 1), ErrMT900910Framing},
		{"D variant refused", strings.Replace(base, "-}", ":52D:BANK NAME\n-}", 1), ErrMT900910Field},
		{"out of order", strings.Replace(base, ":21:ORIG-202\n:25:12345678", ":25:12345678\n:21:ORIG-202", 1), ErrMT900910Field},
		{"missing :25:", strings.Replace(base, ":25:12345678\n", "", 1), ErrMT900910Framing},
		{"bad :32A: date", strings.Replace(base, ":32A:261024USD2500,00", ":32A:261332USD2500,00", 1), ErrMT900910ValueDate},
		{"zero :32A: amount", strings.Replace(base, ":32A:261024USD2500,00", ":32A:261024USD0,00", 1), ErrMT900910Amount},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := ParseMT900910(c.msg); !errors.Is(err, c.want) {
				t.Errorf("err = %v, want %v", err, c.want)
			}
		})
	}
	// A :72: continuation line starting with a colon is data, not a
	// new field: it must round-trip.
	m := mt900910Base(t)
	m.SenderToReceiverInfo = []string{"FIRST", ":SECOND LOOKS LIKE A FIELD"}
	msg, err := ExportMT900910(m)
	if err != nil {
		t.Fatalf("ExportMT900910: %v", err)
	}
	got, err := ParseMT900910(msg)
	if err != nil {
		t.Fatalf("ParseMT900910 colon continuation: %v", err)
	}
	if len(got.SenderToReceiverInfo) != 2 || got.SenderToReceiverInfo[1] != ":SECOND LOOKS LIKE A FIELD" {
		t.Errorf(":72: = %v", got.SenderToReceiverInfo)
	}
}

func mt900910SettledPayout(t *testing.T, l *Ledger, id string) PayoutInstruction {
	t.Helper()
	draftPayout(t, l, id, 250_000)
	p := approveSubmitPayout(t, l, id, "bob")
	p, err := l.SettlePayout(id, statementLineForPayout(p, p.SubmittedAt), StatementMatchOptions{})
	if err != nil {
		t.Fatalf("SettlePayout: %v", err)
	}
	if p.Status != PayoutSettled {
		t.Fatalf("status = %q, want settled", p.Status)
	}
	return p
}

func TestExportMT900910ForPayout_SettledOnly(t *testing.T) {
	l := mt103TestLedger(t)
	p := mt900910SettledPayout(t, l, "po-mt900-1")

	msg, err := l.ExportMT900910ForPayout(p.ID, MT900910PayoutOptions{ReceiverLT: "BANKDEFFXXXX"})
	if err != nil {
		t.Fatalf("ExportMT900910ForPayout: %v", err)
	}
	if !strings.HasPrefix(msg, "{1:F01BANKUS33XXX0000000000}\n") {
		t.Errorf("block 1 = %q, want sender LT BANKUS33XXX", strings.SplitN(msg, "\n", 2)[0])
	}
	if !strings.Contains(msg, "{2:I900BANKDEFFXXXXU3003}\n") {
		t.Error("block 2 is not I900")
	}
	if !strings.Contains(msg, ":20:po-mt900-1\n") {
		t.Error(":20: must be the payout ID")
	}
	if !strings.Contains(msg, ":21:NONREF\n") {
		t.Error(":21: must default to NONREF")
	}
	if !strings.Contains(msg, ":25:saver\n") {
		t.Error(":25: must default to the payee account debited on settle")
	}
	want32A := p.SettledAt.UTC().Format("060102") + "USD2500,00"
	if !strings.Contains(msg, ":32A:"+want32A+"\n") {
		t.Errorf(":32A: missing %s in %q", want32A, msg)
	}
	// Overrides: explicit related reference, account, and credit type.
	msg2, err := l.ExportMT900910ForPayout(p.ID, MT900910PayoutOptions{
		MessageType: "910", ReceiverLT: "BANKDEFFXXXX",
		RelatedReference: "ORIG-1", Account: "EXT-ACC-9",
	})
	if err != nil {
		t.Fatalf("ExportMT900910ForPayout override: %v", err)
	}
	if !strings.Contains(msg2, "{2:I910BANKDEFFXXXXU3003}\n") ||
		!strings.Contains(msg2, ":21:ORIG-1\n") || !strings.Contains(msg2, ":25:EXT-ACC-9\n") {
		t.Errorf("override message missing fields: %q", msg2)
	}
	// Repeat export is byte-identical and the payout is untouched.
	again, err := l.ExportMT900910ForPayout(p.ID, MT900910PayoutOptions{ReceiverLT: "BANKDEFFXXXX"})
	if err != nil {
		t.Fatalf("ExportMT900910ForPayout repeat: %v", err)
	}
	if again != msg {
		t.Error("repeat export is not byte-identical")
	}
	if got, _ := l.GetPayout(p.ID); got.Status != PayoutSettled {
		t.Errorf("export mutated payout status to %q", got.Status)
	}
}

func TestExportMT900910ForPayout_NotSettledFailClosed(t *testing.T) {
	l := mt103TestLedger(t)
	// Submitted but not settled: a confirmation would be a lie.
	p := mt103SubmittedPayout(t, l, "po-mt900-sub", 100_000)
	if _, err := l.ExportMT900910ForPayout(p.ID, MT900910PayoutOptions{ReceiverLT: "BANKDEFFXXXX"}); !errors.Is(err, ErrMT900910BadStatus) {
		t.Errorf("submitted payout err = %v, want ErrMT900910BadStatus", err)
	}
	// Draft: even further from settled.
	draftPayout(t, l, "po-mt900-draft", 100_000)
	if _, err := l.ExportMT900910ForPayout("po-mt900-draft", MT900910PayoutOptions{ReceiverLT: "BANKDEFFXXXX"}); !errors.Is(err, ErrMT900910BadStatus) {
		t.Errorf("draft payout err = %v, want ErrMT900910BadStatus", err)
	}
	// Unknown payout.
	if _, err := l.ExportMT900910ForPayout("po-missing", MT900910PayoutOptions{ReceiverLT: "BANKDEFFXXXX"}); !errors.Is(err, ErrMT900910PayoutNotFound) {
		t.Errorf("unknown payout err = %v, want ErrMT900910PayoutNotFound", err)
	}
}
