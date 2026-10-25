package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func mt202Sample() MT202 {
	return MT202{
		SenderLT:                  "BANKUS33",
		ReceiverLT:                "BANKDEFFXXXX",
		TransactionReference:      "MT202-REF-1",
		RelatedReference:          "ORIG-REF-9",
		ValueDate:                 time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC),
		Currency:                  "USD",
		AmountCents:               250_000,
		OrderingInstitutionBIC:    "BANKUS33",
		BeneficiaryInstitutionBIC: "BANKDEFF",
	}
}

func TestExportMT202_HappyPath(t *testing.T) {
	msg, err := ExportMT202(mt202Sample())
	if err != nil {
		t.Fatalf("ExportMT202: %v", err)
	}
	want := "{1:F01BANKUS33XXX0000000000}\n" +
		"{2:I202BANKDEFFXXXXU3003}\n" +
		"{3:{108:MT202-REF-1}}\n" +
		"{4:\n" +
		":20:MT202-REF-1\n" +
		":21:ORIG-REF-9\n" +
		":32A:261022USD2500,00\n" +
		":52A:BANKUS33\n" +
		":58A:BANKDEFF\n" +
		"-}"
	if msg != want {
		t.Errorf("ExportMT202 =\n%q\nwant\n%q", msg, want)
	}
	// No block 5: CHK is network-computed, never faked.
	if strings.Contains(msg, "{5:") {
		t.Error("export must not carry a block 5 trailer")
	}
}

func TestParseMT202_RoundTrip(t *testing.T) {
	want := mt202Sample()
	msg, err := ExportMT202(want)
	if err != nil {
		t.Fatalf("ExportMT202: %v", err)
	}
	got, err := ParseMT202(msg)
	if err != nil {
		t.Fatalf("ParseMT202: %v", err)
	}
	// SenderLT normalizes BIC8 → 12-char LT on export.
	want.SenderLT = "BANKUS33XXX"
	if got != want {
		t.Errorf("round-trip got %+v, want %+v", got, want)
	}
}

func TestParseMT202_AcceptsCRLF(t *testing.T) {
	msg, err := ExportMT202(mt202Sample())
	if err != nil {
		t.Fatalf("ExportMT202: %v", err)
	}
	crlf := strings.ReplaceAll(msg, "\n", "\r\n")
	got, err := ParseMT202(crlf)
	if err != nil {
		t.Fatalf("ParseMT202 CRLF: %v", err)
	}
	if got.TransactionReference != "MT202-REF-1" || got.AmountCents != 250_000 {
		t.Errorf("CRLF parse lost fields: %+v", got)
	}
}

func TestExportMT202_Validation(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*MT202)
		want error
	}{
		{"bad sender LT", func(m *MT202) { m.SenderLT = "NOPE" }, ErrMT202LT},
		{"bad receiver LT", func(m *MT202) { m.ReceiverLT = "BANK!" }, ErrMT202LT},
		{"empty :20:", func(m *MT202) { m.TransactionReference = "" }, ErrMT202Reference},
		{"long :21:", func(m *MT202) { m.RelatedReference = "12345678901234567" }, ErrMT202Reference},
		{"brace :20:", func(m *MT202) { m.TransactionReference = "REF{1}" }, ErrMT202Reference},
		{"zero value date", func(m *MT202) { m.ValueDate = time.Time{} }, ErrMT202ValueDate},
		{"lowercase currency", func(m *MT202) { m.Currency = "usd" }, ErrMT202Currency},
		{"short currency", func(m *MT202) { m.Currency = "US" }, ErrMT202Currency},
		{"zero amount", func(m *MT202) { m.AmountCents = 0 }, ErrMT202Amount},
		{"negative amount", func(m *MT202) { m.AmountCents = -5 }, ErrMT202Amount},
		{"empty ordering BIC", func(m *MT202) { m.OrderingInstitutionBIC = "" }, ErrMT202BIC},
		{"bad beneficiary BIC", func(m *MT202) { m.BeneficiaryInstitutionBIC = "BANK1" }, ErrMT202BIC},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := mt202Sample()
			tc.mut(&m)
			if _, err := ExportMT202(m); !errors.Is(err, tc.want) {
				t.Errorf("ExportMT202 error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestParseMT202_FailClosed(t *testing.T) {
	base, err := ExportMT202(mt202Sample())
	if err != nil {
		t.Fatalf("ExportMT202: %v", err)
	}
	lines := strings.Split(base, "\n")
	mk := func(replace map[int]string, drop int) string {
		out := []string{}
		for i, ln := range lines {
			if i == drop {
				continue
			}
			if r, ok := replace[i]; ok {
				out = append(out, r)
			} else {
				out = append(out, ln)
			}
		}
		return strings.Join(out, "\n")
	}
	cases := []struct {
		name string
		text string
		want error
	}{
		{"empty input", "", ErrMT202Framing},
		{"missing :21:", mk(nil, 5), ErrMT202Framing},
		{"unknown :72: field", mk(map[int]string{8: ":72:/SENDER TO RECEIVER INFO"}, -1), ErrMT202Field},
		{"D-variant :52D:", mk(map[int]string{7: ":52D:BANKUS33"}, -1), ErrMT202Field},
		{"fields out of order", mk(map[int]string{4: ":21:ORIG-REF-9", 5: ":20:MT202-REF-1"}, -1), ErrMT202Field},
		{"duplicate :20:", mk(map[int]string{5: ":20:MT202-REF-1"}, -1), ErrMT202Field},
		{"bad :32A: date", mk(map[int]string{6: ":32A:261332USD2500,00"}, -1), ErrMT202ValueDate},
		{"bad :32A: currency", mk(map[int]string{6: ":32A:261022US12500,00"}, -1), ErrMT202Currency},
		{"dot amount separator", mk(map[int]string{6: ":32A:261022USD2500.00"}, -1), ErrMT202Amount},
		{"one-digit fraction", mk(map[int]string{6: ":32A:261022USD2500,0"}, -1), ErrMT202Amount},
		{"zero amount", mk(map[int]string{6: ":32A:261022USD0,00"}, -1), ErrMT202Amount},
		{"bad ordering BIC", mk(map[int]string{7: ":52A:NOTABIC!"}, -1), ErrMT202BIC},
		{"bad block 1", mk(map[int]string{0: "{1:F01BANKUS33XXX000000000}"}, -1), ErrMT202Framing},
		{"bad block 2 type", mk(map[int]string{1: "{2:I103BANKDEFFXXXXU3003}"}, -1), ErrMT202Framing},
		{"truncated trailer", mk(map[int]string{9: ""}, -1), ErrMT202Framing},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseMT202(tc.text); !errors.Is(err, tc.want) {
				t.Errorf("ParseMT202 error = %v, want %v", err, tc.want)
			}
		})
	}
}
