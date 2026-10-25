package ledger

import (
	"errors"
	"strings"
	"testing"
)

func mt199Base(t *testing.T) MT199 {
	t.Helper()
	return MT199{
		SenderLT:             "BANKUS33",
		ReceiverLT:           "BANKDEFF",
		TransactionReference: "FREE-199-1",
		RelatedReference:     "po-mt199-1",
		Narrative:            []string{"PLEASE ADVISE STATUS OF", "TRANSFER po-mt199-1"},
	}
}

func TestExportMT199_HappyPath(t *testing.T) {
	m := mt199Base(t)
	msg, err := ExportMT199(m)
	if err != nil {
		t.Fatalf("ExportMT199: %v", err)
	}
	want := "{1:F01BANKUS33XXX0000000000}\n" +
		"{2:I199BANKDEFFXXXU3003}\n" +
		"{3:{108:FREE-199-1}}\n" +
		"{4:\n" +
		":20:FREE-199-1\n" +
		":21:po-mt199-1\n" +
		":79:PLEASE ADVISE STATUS OF\n" +
		"TRANSFER po-mt199-1\n" +
		"-}"
	if msg != want {
		t.Errorf("message = %q, want %q", msg, want)
	}
	if strings.Contains(msg, "{5:") {
		t.Error("block 5 must be omitted: CHK is network-assigned")
	}
	if strings.Contains(msg, ":32A:") {
		t.Error("an MT199 must never carry a value field")
	}
	// Round-trip: parse returns the same business fields.
	got, err := ParseMT199(msg)
	if err != nil {
		t.Fatalf("ParseMT199: %v", err)
	}
	if got.TransactionReference != "FREE-199-1" || got.RelatedReference != "po-mt199-1" ||
		len(got.Narrative) != 2 || got.Narrative[1] != "TRANSFER po-mt199-1" {
		t.Errorf("round-trip = %+v", got)
	}
	// Repeat exports are byte-identical.
	again, err := ExportMT199(m)
	if err != nil {
		t.Fatalf("ExportMT199 repeat: %v", err)
	}
	if again != msg {
		t.Error("repeat export is not byte-identical")
	}
}

func TestExportMT199_NoRelatedReferenceAndCRLF(t *testing.T) {
	m := mt199Base(t)
	m.RelatedReference = ""
	msg, err := ExportMT199(m)
	if err != nil {
		t.Fatalf("ExportMT199: %v", err)
	}
	if strings.Contains(msg, ":21:") {
		t.Error(":21: must be omitted when empty")
	}
	got, err := ParseMT199(strings.ReplaceAll(msg, "\n", "\r\n") + "\n")
	if err != nil {
		t.Fatalf("ParseMT199 CRLF: %v", err)
	}
	if got.RelatedReference != "" || len(got.Narrative) != 2 {
		t.Errorf("CRLF parse = %+v", got)
	}
}

func TestExportMT199_Validation(t *testing.T) {
	base := mt199Base(t)
	cases := []struct {
		name string
		mut  func(*MT199)
		want error
	}{
		{"empty :20:", func(m *MT199) { m.TransactionReference = "" }, ErrMT199Reference},
		{"long :20:", func(m *MT199) { m.TransactionReference = "12345678901234567" }, ErrMT199Reference},
		{"long :21:", func(m *MT199) { m.RelatedReference = "12345678901234567" }, ErrMT199Reference},
		{"empty narrative", func(m *MT199) { m.Narrative = nil }, ErrMT199Narrative},
		{"empty narrative line", func(m *MT199) { m.Narrative = []string{""} }, ErrMT199Narrative},
		{"long narrative line", func(m *MT199) { m.Narrative = []string{strings.Repeat("x", 36)} }, ErrMT199Narrative},
		{"too many narrative lines", func(m *MT199) {
			m.Narrative = make([]string, 51)
			for i := range m.Narrative {
				m.Narrative[i] = "x"
			}
		}, ErrMT199Narrative},
		{"brace in narrative", func(m *MT199) { m.Narrative = []string{"BAD {BRACE}"} }, ErrMT199Narrative},
		{"bad sender LT", func(m *MT199) { m.SenderLT = "XX" }, ErrMT199LT},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := base
			c.mut(&m)
			if _, err := ExportMT199(m); !errors.Is(err, c.want) {
				t.Errorf("err = %v, want %v", err, c.want)
			}
		})
	}
}

func TestParseMT199_Rejections(t *testing.T) {
	base, err := ExportMT199(mt199Base(t))
	if err != nil {
		t.Fatalf("ExportMT199: %v", err)
	}
	cases := []struct {
		name string
		msg  string
		want error
	}{
		{"I900 is not free format", strings.Replace(base, "I199", "I900", 1), ErrMT199Framing},
		{"missing :79:", strings.Replace(base, ":79:PLEASE ADVISE STATUS OF\nTRANSFER po-mt199-1\n", "", 1), ErrMT199Field},
		{"valued field before :79:", strings.Replace(base, ":79:", ":32A:261024USD2500,00\n:79:", 1), ErrMT199Field},
		{"out of order :21: first", strings.Replace(base, ":20:FREE-199-1\n:21:po-mt199-1", ":21:po-mt199-1\n:20:FREE-199-1", 1), ErrMT199Field},
		{"empty :21:", strings.Replace(base, ":21:po-mt199-1", ":21:", 1), ErrMT199Field},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := ParseMT199(c.msg); !errors.Is(err, c.want) {
				t.Errorf("err = %v, want %v", err, c.want)
			}
		})
	}
	// A :79: continuation line starting with a colon is narrative
	// data, not a new field: it must round-trip (same rule as
	// MT900/MT910's :72:).
	m := mt199Base(t)
	m.Narrative = []string{"FIRST LINE", ":SECOND LOOKS LIKE A FIELD"}
	msg, err := ExportMT199(m)
	if err != nil {
		t.Fatalf("ExportMT199: %v", err)
	}
	got, err := ParseMT199(msg)
	if err != nil {
		t.Fatalf("ParseMT199 colon continuation: %v", err)
	}
	if len(got.Narrative) != 2 || got.Narrative[1] != ":SECOND LOOKS LIKE A FIELD" {
		t.Errorf(":79: = %v", got.Narrative)
	}
}

func TestWrapMT199Narrative(t *testing.T) {
	lines, err := WrapMT199Narrative("Please advise the status of transfer po-mt199-1 and confirm receipt")
	if err != nil {
		t.Fatalf("WrapMT199Narrative: %v", err)
	}
	for _, ln := range lines {
		if len(ln) > 35 {
			t.Errorf("wrapped line exceeds 35: %q", ln)
		}
	}
	if got := strings.Join(lines, " "); got != "Please advise the status of transfer po-mt199-1 and confirm receipt" {
		t.Errorf("wrapped join = %q", got)
	}
	// Wrapped lines export and round-trip.
	m := mt199Base(t)
	m.Narrative = lines
	msg, err := ExportMT199(m)
	if err != nil {
		t.Fatalf("ExportMT199 wrapped: %v", err)
	}
	got, err := ParseMT199(msg)
	if err != nil {
		t.Fatalf("ParseMT199 wrapped: %v", err)
	}
	if strings.Join(got.Narrative, " ") != strings.Join(lines, " ") {
		t.Errorf("wrapped round-trip = %v", got.Narrative)
	}
	// Fail-closed cases: empty text, an unsplittable long token
	// (a UETR must never be split mid-token), and >50 lines.
	if _, err := WrapMT199Narrative("   "); !errors.Is(err, ErrMT199Narrative) {
		t.Errorf("empty text err = %v, want ErrMT199Narrative", err)
	}
	if _, err := WrapMT199Narrative("see UETR 12345678-1234-4234-8234-123456789012 here"); !errors.Is(err, ErrMT199Narrative) {
		t.Errorf("long token err = %v, want ErrMT199Narrative", err)
	}
	long := strings.Repeat("word ", 400)
	if _, err := WrapMT199Narrative(long); !errors.Is(err, ErrMT199Narrative) {
		t.Errorf("over-long text err = %v, want ErrMT199Narrative", err)
	}
}
