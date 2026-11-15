package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestExportPain018_HappyPathWithReason(t *testing.T) {
	day := pain008Day()
	l, m := pain008Fixture(t, MandateFrequencyPerMonth, day, day.AddDate(1, 0, 0))

	doc, err := l.ExportPain018(m.ID, "DE98ZZZ09999999999", "SUSP")
	if err != nil {
		t.Fatalf("ExportPain018: %v", err)
	}
	s := string(doc)
	for _, want := range []string{
		`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pain.018.001.03">`,
		"<MndtSspnsnReq>",
		"<MsgId>" + pain018MsgID(m.ID, "DE98ZZZ09999999999") + "</MsgId>",
		"<SspnsnReqId>" + pain018MsgID(m.ID, "DE98ZZZ09999999999") + "</SspnsnReqId>",
		"<OrgnlMndt>",
		"<MndtId>m-008</MndtId>",
		"<Id>DE98ZZZ09999999999</Id>",
		"<SspnsnRsn><Rsn><Prtry>SUSP</Prtry></Rsn></SspnsnRsn>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("document missing %q:\n%s", want, s)
		}
	}
	// A suspension request carries the authorization, never a
	// collection and never an account: no instructed amount and no
	// IBAN elements ride the wire.
	if strings.Contains(s, "InstdAmt") {
		t.Error("pain.018 document carries an instructed amount")
	}
	if strings.Contains(s, "<IBAN>") {
		t.Error("pain.018 document carries an IBAN")
	}
}

func TestExportPain018_MissingReasonRefused(t *testing.T) {
	// SspnsnRsn is [1..1] in the full V03 definition
	// (MandateSuspension3.sspnsn_rsn is a plain, non-Option field),
	// so a reasonless request is refused — no default code is
	// invented and no reasonless document is ever rendered.
	day := pain008Day()
	l, m := pain008Fixture(t, MandateFrequencyPerMonth, day, day.AddDate(1, 0, 0))

	if _, err := l.ExportPain018(m.ID, "DE98ZZZ09999999999", ""); !errors.Is(err, ErrMissingPain018Reason) {
		t.Errorf("empty reason: err = %v, want ErrMissingPain018Reason", err)
	}
	if _, err := l.ExportPain018(m.ID, "DE98ZZZ09999999999", "   "); !errors.Is(err, ErrMissingPain018Reason) {
		t.Errorf("blank reason: err = %v, want ErrMissingPain018Reason", err)
	}
}

func TestExportPain018_SuspendedRefused(t *testing.T) {
	// The deliberate divergence from pain.011: a suspension request
	// targets a live authorization — asking the bank to pause an
	// already suspended mandate is empty talk, so the active-only
	// gate refuses it.
	day := pain008Day()
	l, m := pain008Fixture(t, MandateFrequencyPerMonth, day, day.AddDate(1, 0, 0))
	if _, err := l.SuspendMandate(m.ID); err != nil {
		t.Fatalf("SuspendMandate: %v", err)
	}
	if _, err := l.ExportPain018(m.ID, "DE98ZZZ09999999999", "SUSP"); !errors.Is(err, ErrMandateNotActive) {
		t.Errorf("suspended mandate: err = %v, want ErrMandateNotActive", err)
	}
}

func TestExportPain018_FailClosed(t *testing.T) {
	day := pain008Day()
	l, m := pain008Fixture(t, MandateFrequencyPerMonth, day, day.AddDate(1, 0, 0))

	// A cancelled mandate is dead: the suspension request has
	// nothing left to pause.
	if _, err := l.CancelMandate(m.ID); err != nil {
		t.Fatalf("CancelMandate: %v", err)
	}
	if _, err := l.ExportPain018(m.ID, "DE98ZZZ09999999999", "SUSP"); !errors.Is(err, ErrMandateNotActive) {
		t.Errorf("cancelled mandate: err = %v, want ErrMandateNotActive", err)
	}

	// A mandate whose validity already ended reads as expired.
	past := day.AddDate(0, -2, 0)
	l2, m2 := pain008Fixture(t, MandateFrequencyPerDay, past, past.Add(24*time.Hour))
	if _, err := l2.ExportPain018(m2.ID, "DE98ZZZ09999999999", "SUSP"); !errors.Is(err, ErrMandateExpired) {
		t.Errorf("expired mandate: err = %v, want ErrMandateExpired", err)
	}

	// Unknown mandate and missing scheme fail before any gate.
	if _, err := l.ExportPain018("m-ghost", "DE98ZZZ09999999999", "SUSP"); !errors.Is(err, ErrMandateNotFound) {
		t.Errorf("unknown mandate: err = %v, want ErrMandateNotFound", err)
	}
	if _, err := l.ExportPain018(m.ID, "  ", "SUSP"); !errors.Is(err, ErrMissingPain018CreditorScheme) {
		t.Errorf("empty scheme: err = %v, want ErrMissingPain018CreditorScheme", err)
	}
}

func TestExportPain018_NoIBANStillExports(t *testing.T) {
	// The request identifies only the mandate itself, so a mandate
	// with no registered IBANs on either side still suspends.
	day := pain008Day()
	l := New()
	seedBalance(t, l, "cust", 1_000_000)
	seedBalance(t, l, "merchant", 1_000_000)
	if _, err := l.CreateMandate(Mandate{
		ID:             "m-018-noiban",
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
	doc, err := l.ExportPain018("m-018-noiban", "DE98ZZZ09999999999", "SUSP")
	if err != nil {
		t.Fatalf("ExportPain018 without IBANs: %v", err)
	}
	if !strings.Contains(string(doc), "<MndtId>m-018-noiban</MndtId>") {
		t.Errorf("document missing mandate id:\n%s", doc)
	}
}

func TestExportPain018_DeterministicExceptCreDtTm(t *testing.T) {
	day := pain008Day()
	l, m := pain008Fixture(t, MandateFrequencyPerMonth, day, day.AddDate(1, 0, 0))

	first, err := l.ExportPain018(m.ID, "DE98ZZZ09999999999", "SUSP")
	if err != nil {
		t.Fatalf("first export: %v", err)
	}
	second, err := l.ExportPain018(m.ID, "DE98ZZZ09999999999", "SUSP")
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
	if !strings.Contains(string(second), "<MsgId>"+pain018MsgID(m.ID, "DE98ZZZ09999999999")+"</MsgId>") {
		t.Error("repeat export carries a different MsgId")
	}
}

func TestExportPain018_ReadOnly(t *testing.T) {
	day := pain008Day()
	l, m := pain008Fixture(t, MandateFrequencyPerMonth, day, day.AddDate(1, 0, 0))

	before := l.Version()
	if _, err := l.ExportPain018(m.ID, "DE98ZZZ09999999999", "SUSP"); err != nil {
		t.Fatalf("ExportPain018: %v", err)
	}
	if l.Version() != before {
		t.Errorf("version moved %d -> %d on a read-only export", before, l.Version())
	}
	got, ok := l.GetMandate(m.ID)
	if !ok {
		t.Fatal("GetMandate: mandate vanished")
	}
	if got.Status != MandateStatusActive {
		t.Errorf("mandate status = %q after export, want active (export is a request, not a suspension)", got.Status)
	}
}

func TestExportPain018_AuditEvent(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	day := pain008Day()
	seedBalance(t, l, "cust", 1_000_000)
	seedBalance(t, l, "merchant", 1_000_000)
	mustSetIBAN(t, l, "cust", testIBANDE, "")
	mustSetIBAN(t, l, "merchant", testIBANGB, "")
	if _, err := l.CreateMandate(Mandate{
		ID:             "m-018-audit",
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

	if _, err := l.ExportPain018("m-018-audit", "DE98ZZZ09999999999", "SUSP"); err != nil {
		t.Fatalf("ExportPain018: %v", err)
	}
	events := readAuditOps(t, al)
	ev := findOp(events, "pain018_exported")
	if ev == nil {
		t.Fatal("pain018_exported audit event missing")
	}
	if countOp(events, "pain018_exported") != 1 {
		t.Errorf("pain018_exported count = %d, want 1", countOp(events, "pain018_exported"))
	}
	if ev.TraceID != pain018MsgID("m-018-audit", "DE98ZZZ09999999999") {
		t.Errorf("trace_id = %q, want the deterministic MsgId", ev.TraceID)
	}
	if ev.Details["msg_id"] != pain018MsgID("m-018-audit", "DE98ZZZ09999999999") ||
		ev.Details["mandate_id"] != "m-018-audit" ||
		ev.Details["creditor_scheme_id"] != "DE98ZZZ09999999999" ||
		ev.Details["reason_code"] != "SUSP" {
		t.Errorf("audit details = %v, want msg_id/mandate_id/creditor_scheme_id/reason_code", ev.Details)
	}
	if ev.VersionBefore != ev.VersionAfter {
		t.Errorf("audit version moved %d -> %d on a read-only export", ev.VersionBefore, ev.VersionAfter)
	}
}
