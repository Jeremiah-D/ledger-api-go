package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestExportPain011_HappyPathWithReason(t *testing.T) {
	day := pain008Day()
	l, m := pain008Fixture(t, MandateFrequencyPerMonth, day, day.AddDate(1, 0, 0))

	doc, err := l.ExportPain011(m.ID, "DE98ZZZ09999999999", "MD16")
	if err != nil {
		t.Fatalf("ExportPain011: %v", err)
	}
	s := string(doc)
	for _, want := range []string{
		`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pain.011.001.03">`,
		"<MndtCxlReq>",
		"<MsgId>" + pain011MsgID(m.ID, "DE98ZZZ09999999999") + "</MsgId>",
		"<OrgnlMndt>",
		"<MndtId>m-008</MndtId>",
		"<Id>DE98ZZZ09999999999</Id>",
		"<CxlRsn><Rsn><Prtry>MD16</Prtry></Rsn></CxlRsn>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("document missing %q:\n%s", want, s)
		}
	}
	// A cancellation request carries the authorization, never a
	// collection and never an account: no instructed amount and no
	// IBAN elements ride the wire.
	if strings.Contains(s, "InstdAmt") {
		t.Error("pain.011 document carries an instructed amount")
	}
	if strings.Contains(s, "<IBAN>") {
		t.Error("pain.011 document carries an IBAN")
	}
}

func TestExportPain011_NoReasonOmitsCxlRsn(t *testing.T) {
	day := pain008Day()
	l, m := pain008Fixture(t, MandateFrequencyPerMonth, day, day.AddDate(1, 0, 0))

	doc, err := l.ExportPain011(m.ID, "DE98ZZZ09999999999", "")
	if err != nil {
		t.Fatalf("ExportPain011: %v", err)
	}
	if strings.Contains(string(doc), "<CxlRsn>") {
		t.Errorf("reasonless export carries a CxlRsn block:\n%s", doc)
	}
	if strings.Contains(string(doc), "<Prtry>") {
		t.Errorf("reasonless export carries a reason element:\n%s", doc)
	}
	// The original mandate still renders in full.
	if !strings.Contains(string(doc), "<MndtId>m-008</MndtId>") {
		t.Errorf("reasonless export lost the original mandate:\n%s", doc)
	}
}

func TestExportPain011_SuspendedCanCancel(t *testing.T) {
	// The deliberate divergence from pain.009/010: a suspended
	// mandate still exists at the bank, so it still needs a
	// revocation notice — the active-only gate would block exactly
	// the mandates most in need of cancelling.
	day := pain008Day()
	l, m := pain008Fixture(t, MandateFrequencyPerMonth, day, day.AddDate(1, 0, 0))
	if _, err := l.SuspendMandate(m.ID); err != nil {
		t.Fatalf("SuspendMandate: %v", err)
	}
	doc, err := l.ExportPain011(m.ID, "DE98ZZZ09999999999", "")
	if err != nil {
		t.Fatalf("ExportPain011 on suspended mandate: %v", err)
	}
	if !strings.Contains(string(doc), "<MndtId>m-008</MndtId>") {
		t.Errorf("suspended export missing mandate id:\n%s", doc)
	}
}

func TestExportPain011_FailClosed(t *testing.T) {
	day := pain008Day()
	l, m := pain008Fixture(t, MandateFrequencyPerMonth, day, day.AddDate(1, 0, 0))

	// A cancelled mandate is dead: the revocation request has
	// nothing left to revoke.
	if _, err := l.CancelMandate(m.ID); err != nil {
		t.Fatalf("CancelMandate: %v", err)
	}
	if _, err := l.ExportPain011(m.ID, "DE98ZZZ09999999999", ""); !errors.Is(err, ErrMandateNotActive) {
		t.Errorf("cancelled mandate: err = %v, want ErrMandateNotActive", err)
	}

	// A mandate whose validity already ended reads as expired.
	past := day.AddDate(0, -2, 0)
	l2, m2 := pain008Fixture(t, MandateFrequencyPerDay, past, past.Add(24*time.Hour))
	if _, err := l2.ExportPain011(m2.ID, "DE98ZZZ09999999999", ""); !errors.Is(err, ErrMandateExpired) {
		t.Errorf("expired mandate: err = %v, want ErrMandateExpired", err)
	}

	// Unknown mandate and missing scheme fail before any gate.
	if _, err := l.ExportPain011("m-ghost", "DE98ZZZ09999999999", ""); !errors.Is(err, ErrMandateNotFound) {
		t.Errorf("unknown mandate: err = %v, want ErrMandateNotFound", err)
	}
	if _, err := l.ExportPain011(m.ID, "  ", ""); !errors.Is(err, ErrMissingPain011CreditorScheme) {
		t.Errorf("empty scheme: err = %v, want ErrMissingPain011CreditorScheme", err)
	}
}

func TestExportPain011_NoIBANStillExports(t *testing.T) {
	// The second deliberate divergence from pain.009: the request
	// identifies only the mandate itself, so a mandate with no
	// registered IBANs on either side still cancels.
	day := pain008Day()
	l := New()
	seedBalance(t, l, "cust", 1_000_000)
	seedBalance(t, l, "merchant", 1_000_000)
	if _, err := l.CreateMandate(Mandate{
		ID:             "m-011-noiban",
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
	doc, err := l.ExportPain011("m-011-noiban", "DE98ZZZ09999999999", "MD17")
	if err != nil {
		t.Fatalf("ExportPain011 without IBANs: %v", err)
	}
	if !strings.Contains(string(doc), "<MndtId>m-011-noiban</MndtId>") {
		t.Errorf("document missing mandate id:\n%s", doc)
	}
}

func TestExportPain011_DeterministicExceptCreDtTm(t *testing.T) {
	day := pain008Day()
	l, m := pain008Fixture(t, MandateFrequencyPerMonth, day, day.AddDate(1, 0, 0))

	first, err := l.ExportPain011(m.ID, "DE98ZZZ09999999999", "MD16")
	if err != nil {
		t.Fatalf("first export: %v", err)
	}
	second, err := l.ExportPain011(m.ID, "DE98ZZZ09999999999", "MD16")
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
	if !strings.Contains(string(second), "<MsgId>"+pain011MsgID(m.ID, "DE98ZZZ09999999999")+"</MsgId>") {
		t.Error("repeat export carries a different MsgId")
	}
}

func TestExportPain011_ReadOnly(t *testing.T) {
	day := pain008Day()
	l, m := pain008Fixture(t, MandateFrequencyPerMonth, day, day.AddDate(1, 0, 0))

	before := l.Version()
	if _, err := l.ExportPain011(m.ID, "DE98ZZZ09999999999", "MD16"); err != nil {
		t.Fatalf("ExportPain011: %v", err)
	}
	if l.Version() != before {
		t.Errorf("version moved %d -> %d on a read-only export", before, l.Version())
	}
	got, ok := l.GetMandate(m.ID)
	if !ok {
		t.Fatal("GetMandate: mandate vanished")
	}
	if got.Status != MandateStatusActive {
		t.Errorf("mandate status = %q after export, want active (export is a request, not a cancellation)", got.Status)
	}
}

func TestExportPain011_AuditEvent(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	day := pain008Day()
	seedBalance(t, l, "cust", 1_000_000)
	seedBalance(t, l, "merchant", 1_000_000)
	mustSetIBAN(t, l, "cust", testIBANDE, "")
	mustSetIBAN(t, l, "merchant", testIBANGB, "")
	if _, err := l.CreateMandate(Mandate{
		ID:             "m-011-audit",
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

	if _, err := l.ExportPain011("m-011-audit", "DE98ZZZ09999999999", "MD16"); err != nil {
		t.Fatalf("ExportPain011: %v", err)
	}
	events := readAuditOps(t, al)
	ev := findOp(events, "pain011_exported")
	if ev == nil {
		t.Fatal("pain011_exported audit event missing")
	}
	if countOp(events, "pain011_exported") != 1 {
		t.Errorf("pain011_exported count = %d, want 1", countOp(events, "pain011_exported"))
	}
	if ev.TraceID != pain011MsgID("m-011-audit", "DE98ZZZ09999999999") {
		t.Errorf("trace_id = %q, want the deterministic MsgId", ev.TraceID)
	}
	if ev.Details["msg_id"] != pain011MsgID("m-011-audit", "DE98ZZZ09999999999") ||
		ev.Details["mandate_id"] != "m-011-audit" ||
		ev.Details["creditor_scheme_id"] != "DE98ZZZ09999999999" ||
		ev.Details["reason_code"] != "MD16" {
		t.Errorf("audit details = %v, want msg_id/mandate_id/creditor_scheme_id/reason_code", ev.Details)
	}
	if ev.VersionBefore != ev.VersionAfter {
		t.Errorf("audit version moved %d -> %d on a read-only export", ev.VersionBefore, ev.VersionAfter)
	}
}
