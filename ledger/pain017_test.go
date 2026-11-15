package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestExportPain017_HappyPath(t *testing.T) {
	day := pain008Day()
	l, m := pain008Fixture(t, MandateFrequencyPerMonth, day, day.AddDate(1, 0, 0))

	doc, err := l.ExportPain017(m.ID, "DE98ZZZ09999999999")
	if err != nil {
		t.Fatalf("ExportPain017: %v", err)
	}
	s := string(doc)
	for _, want := range []string{
		`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pain.017.001.03">`,
		"<MndtCpyReq>",
		"<MsgId>" + pain017MsgID(m.ID, "DE98ZZZ09999999999") + "</MsgId>",
		"<UndrlygCpyReqDtls>",
		"<OrgnlMndt>",
		"<MndtId>m-008</MndtId>",
		"<Id>DE98ZZZ09999999999</Id>",
		"<MndtSts><Cd>ACTV</Cd></MndtSts>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("document missing %q:\n%s", want, s)
		}
	}
	// A copy request carries the authorization, never a collection
	// and never an account: no instructed amount and no IBAN
	// elements ride the wire.
	if strings.Contains(s, "InstdAmt") {
		t.Error("pain.017 document carries an instructed amount")
	}
	if strings.Contains(s, "<IBAN>") {
		t.Error("pain.017 document carries an IBAN")
	}
}

func TestExportPain017_FourStatesAllExport(t *testing.T) {
	// The deliberate divergence from pain.009/010/011/018: a copy
	// request is a record request, not a lifecycle move, so there
	// is no state gate — active, suspended, cancelled, and expired
	// mandates are all exportable, and a cancelled mandate's copy
	// is precisely the evidence a dispute needs.
	day := pain008Day()
	cases := []struct {
		name       string
		prepare    func(t *testing.T, l *Ledger, id string)
		validFrom  time.Time
		validUntil time.Time
		wantCode   string
	}{
		{"active", nil, day, day.AddDate(1, 0, 0), "ACTV"},
		{"suspended", func(t *testing.T, l *Ledger, id string) {
			t.Helper()
			if _, err := l.SuspendMandate(id); err != nil {
				t.Fatalf("SuspendMandate: %v", err)
			}
		}, day, day.AddDate(1, 0, 0), "SUSP"},
		{"cancelled", func(t *testing.T, l *Ledger, id string) {
			t.Helper()
			if _, err := l.CancelMandate(id); err != nil {
				t.Fatalf("CancelMandate: %v", err)
			}
		}, day, day.AddDate(1, 0, 0), "CANC"},
		{"expired", nil, day.AddDate(0, -2, 0), day.AddDate(0, -2, 0).Add(24 * time.Hour), "EXPI"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, m := pain008Fixture(t, MandateFrequencyPerMonth, tc.validFrom, tc.validUntil)
			if tc.prepare != nil {
				tc.prepare(t, l, m.ID)
			}
			doc, err := l.ExportPain017(m.ID, "DE98ZZZ09999999999")
			if err != nil {
				t.Fatalf("ExportPain017 (%s): %v", tc.name, err)
			}
			if want := "<MndtSts><Cd>" + tc.wantCode + "</Cd></MndtSts>"; !strings.Contains(string(doc), want) {
				t.Errorf("document missing %q:\n%s", want, doc)
			}
		})
	}
}

func TestExportPain017_FailClosed(t *testing.T) {
	day := pain008Day()
	l, m := pain008Fixture(t, MandateFrequencyPerMonth, day, day.AddDate(1, 0, 0))

	// Existence is the only gate: an unknown mandate is refused.
	if _, err := l.ExportPain017("m-ghost", "DE98ZZZ09999999999"); !errors.Is(err, ErrMandateNotFound) {
		t.Errorf("unknown mandate: err = %v, want ErrMandateNotFound", err)
	}
	// A missing creditor scheme ID is refused, never defaulted:
	// the ledger stores no creditor identifier to fall back on.
	if _, err := l.ExportPain017(m.ID, ""); !errors.Is(err, ErrMissingPain017CreditorScheme) {
		t.Errorf("empty scheme: err = %v, want ErrMissingPain017CreditorScheme", err)
	}
	if _, err := l.ExportPain017(m.ID, "   "); !errors.Is(err, ErrMissingPain017CreditorScheme) {
		t.Errorf("blank scheme: err = %v, want ErrMissingPain017CreditorScheme", err)
	}
}

func TestExportPain017_NoIBANStillExports(t *testing.T) {
	// The request identifies only the mandate itself, so a mandate
	// with no registered IBANs on either side still exports.
	day := pain008Day()
	l := New()
	seedBalance(t, l, "cust", 1_000_000)
	seedBalance(t, l, "merchant", 1_000_000)
	if _, err := l.CreateMandate(Mandate{
		ID:             "m-017-noiban",
		Payer:          "cust",
		Payee:          "merchant",
		MaxAmountCents: 5000,
		Currency:       "USD",
		Frequency:      MandateFrequencyPerMonth,
		ValidFrom:      day,
		ValidUntil:     day.AddDate(1, 0, 0),
	}); err != nil {
		t.Fatalf("CreateMandate: %v", err)
	}
	doc, err := l.ExportPain017("m-017-noiban", "DE98ZZZ09999999999")
	if err != nil {
		t.Fatalf("ExportPain017 without IBANs: %v", err)
	}
	if !strings.Contains(string(doc), "<MndtId>m-017-noiban</MndtId>") {
		t.Errorf("document missing mandate id:\n%s", doc)
	}
}

func TestExportPain017_DeterministicExceptCreDtTm(t *testing.T) {
	day := pain008Day()
	l, m := pain008Fixture(t, MandateFrequencyPerMonth, day, day.AddDate(1, 0, 0))

	first, err := l.ExportPain017(m.ID, "DE98ZZZ09999999999")
	if err != nil {
		t.Fatalf("first export: %v", err)
	}
	second, err := l.ExportPain017(m.ID, "DE98ZZZ09999999999")
	if err != nil {
		t.Fatalf("second export: %v", err)
	}
	stripCreDtTm := func(doc []byte) string {
		var lines []string
		for _, line := range strings.Split(string(doc), "\n") {
			if !strings.HasPrefix(line, "<CreDtTm>") {
				lines = append(lines, line)
			}
		}
		return strings.Join(lines, "\n")
	}
	if stripCreDtTm(first) != stripCreDtTm(second) {
		t.Errorf("repeat exports differ apart from CreDtTm:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
	if !strings.Contains(string(second), "<MsgId>"+pain017MsgID(m.ID, "DE98ZZZ09999999999")+"</MsgId>") {
		t.Error("repeat export carries a different MsgId")
	}
}

func TestExportPain017_ReadOnly(t *testing.T) {
	day := pain008Day()
	l, m := pain008Fixture(t, MandateFrequencyPerMonth, day, day.AddDate(1, 0, 0))

	before := l.Version()
	if _, err := l.ExportPain017(m.ID, "DE98ZZZ09999999999"); err != nil {
		t.Fatalf("ExportPain017: %v", err)
	}
	if l.Version() != before {
		t.Errorf("version moved %d -> %d on a read-only export", before, l.Version())
	}
	got, ok := l.GetMandate(m.ID)
	if !ok {
		t.Fatal("GetMandate: mandate vanished")
	}
	if got.Status != MandateStatusActive {
		t.Errorf("mandate status = %q after export, want active (export is a request, not a state move)", got.Status)
	}
}

func TestExportPain017_AuditEvent(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	day := pain008Day()
	seedBalance(t, l, "cust", 1_000_000)
	seedBalance(t, l, "merchant", 1_000_000)
	mustSetIBAN(t, l, "cust", testIBANDE, "")
	mustSetIBAN(t, l, "merchant", testIBANGB, "")
	if _, err := l.CreateMandate(Mandate{
		ID:             "m-017-audit",
		Payer:          "cust",
		Payee:          "merchant",
		MaxAmountCents: 5000,
		Currency:       "USD",
		Frequency:      MandateFrequencyPerMonth,
		ValidFrom:      day,
		ValidUntil:     day.AddDate(1, 0, 0),
	}); err != nil {
		t.Fatalf("CreateMandate: %v", err)
	}

	if _, err := l.ExportPain017("m-017-audit", "DE98ZZZ09999999999"); err != nil {
		t.Fatalf("ExportPain017: %v", err)
	}
	events := readAuditOps(t, al)
	ev := findOp(events, "pain017_exported")
	if ev == nil {
		t.Fatal("pain017_exported audit event missing")
	}
	if countOp(events, "pain017_exported") != 1 {
		t.Errorf("pain017_exported count = %d, want 1", countOp(events, "pain017_exported"))
	}
	if ev.TraceID != pain017MsgID("m-017-audit", "DE98ZZZ09999999999") {
		t.Errorf("trace_id = %q, want the deterministic MsgId", ev.TraceID)
	}
	if ev.Details["msg_id"] != pain017MsgID("m-017-audit", "DE98ZZZ09999999999") ||
		ev.Details["mandate_id"] != "m-017-audit" ||
		ev.Details["creditor_scheme_id"] != "DE98ZZZ09999999999" {
		t.Errorf("audit details = %v, want msg_id/mandate_id/creditor_scheme_id", ev.Details)
	}
	if ev.VersionBefore != ev.VersionAfter {
		t.Errorf("audit version moved %d -> %d on a read-only export", ev.VersionBefore, ev.VersionAfter)
	}
}
