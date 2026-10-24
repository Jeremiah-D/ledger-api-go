package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestExportPain009_HappyPath(t *testing.T) {
	day := pain008Day()
	l, m := pain008Fixture(t, MandateFrequencyPerMonth, day, day.AddDate(1, 0, 0))

	doc, err := l.ExportPain009(m.ID, "DE98ZZZ09999999999")
	if err != nil {
		t.Fatalf("ExportPain009: %v", err)
	}
	s := string(doc)
	for _, want := range []string{
		"<MndtInitnReq>",
		"<MndtId>m-008</MndtId>",
		"<Id>DE98ZZZ09999999999</Id>",
		"<IBAN>" + testIBANDE + "</IBAN>",
		"<IBAN>" + testIBANGB + "</IBAN>",
		"<FrDt>" + day.Format("2006-01-02") + "</FrDt>",
		"<ToDt>" + day.AddDate(1, 0, 0).Format("2006-01-02") + "</ToDt>",
		"<MsgId>" + pain009MsgID(m.ID, "DE98ZZZ09999999999") + "</MsgId>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("document missing %q", want)
		}
	}
	// An initiation request carries the authorization, never a
	// collection: no instructed amount rides the wire.
	if strings.Contains(s, "InstdAmt") {
		t.Error("pain.009 document carries an instructed amount")
	}
	// Repeat exports carry the same deterministic MsgId.
	second, err := l.ExportPain009(m.ID, "DE98ZZZ09999999999")
	if err != nil {
		t.Fatalf("ExportPain009 repeat: %v", err)
	}
	if !strings.Contains(string(second), "<MsgId>"+pain009MsgID(m.ID, "DE98ZZZ09999999999")+"</MsgId>") {
		t.Error("repeat export carries a different MsgId")
	}
	// A different creditor scheme changes the MsgId: the ID is
	// deterministic in (mandate, scheme), not constant.
	other, err := l.ExportPain009(m.ID, "FR12ZZZ12345678901")
	if err != nil {
		t.Fatalf("ExportPain009 other scheme: %v", err)
	}
	if strings.Contains(string(other), "<MsgId>"+pain009MsgID(m.ID, "DE98ZZZ09999999999")+"</MsgId>") {
		t.Error("different scheme reuses the same MsgId")
	}
}

func TestExportPain009_FailClosed(t *testing.T) {
	day := pain008Day()
	l, m := pain008Fixture(t, MandateFrequencyPerMonth, day, day.AddDate(1, 0, 0))

	if _, err := l.ExportPain009("no-such-mandate", "DE98ZZZ09999999999"); !errors.Is(err, ErrMandateNotFound) {
		t.Errorf("unknown mandate: err = %v, want ErrMandateNotFound", err)
	}
	if _, err := l.ExportPain009(m.ID, "  "); !errors.Is(err, ErrMissingPain009CreditorScheme) {
		t.Errorf("empty scheme: err = %v, want ErrMissingPain009CreditorScheme", err)
	}

	// A suspended mandate authorizes nothing — including its own
	// initiation request.
	if _, err := l.SuspendMandate(m.ID); err != nil {
		t.Fatalf("SuspendMandate: %v", err)
	}
	if _, err := l.ExportPain009(m.ID, "DE98ZZZ09999999999"); !errors.Is(err, ErrMandateNotActive) {
		t.Errorf("suspended mandate: err = %v, want ErrMandateNotActive", err)
	}
}

func TestExportPain009_ExpiredAndMissingIBAN(t *testing.T) {
	// A mandate whose validity already ended is expired on every
	// read: an initiation request for it is refused.
	past := pain008Day().AddDate(0, -2, 0)
	l, m := pain008Fixture(t, MandateFrequencyPerDay, past, past.Add(24*time.Hour))
	if _, err := l.ExportPain009(m.ID, "DE98ZZZ09999999999"); !errors.Is(err, ErrMandateExpired) {
		t.Errorf("expired mandate: err = %v, want ErrMandateExpired", err)
	}

	// No registered IBANs: the initiation request cannot name the
	// accounts, so it is refused rather than rendered with forged
	// identifiers.
	day := pain008Day()
	l2 := New()
	seedBalance(t, l2, "cust", 1_000_000)
	seedBalance(t, l2, "merchant", 1_000_000)
	if _, err := l2.CreateMandate(Mandate{
		ID:             "m-009-noiban",
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
	if _, err := l2.ExportPain009("m-009-noiban", "DE98ZZZ09999999999"); !errors.Is(err, ErrMissingAccountIBAN) {
		t.Errorf("missing IBAN: err = %v, want ErrMissingAccountIBAN", err)
	}
}

func TestExportPain009_DebtorBIC(t *testing.T) {
	// A registered debtor BIC rides DbtrAgt; a missing one simply
	// omits the element — never an empty BIC.
	day := pain008Day()
	l := New()
	seedBalance(t, l, "cust", 1_000_000)
	seedBalance(t, l, "merchant", 1_000_000)
	mustSetIBAN(t, l, "cust", testIBANDE, "DEUTDEFF")
	mustSetIBAN(t, l, "merchant", testIBANGB, "")
	if _, err := l.CreateMandate(Mandate{
		ID:             "m-009-bic",
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
	doc, err := l.ExportPain009("m-009-bic", "DE98ZZZ09999999999")
	if err != nil {
		t.Fatalf("ExportPain009: %v", err)
	}
	if !strings.Contains(string(doc), "<BIC>DEUTDEFF</BIC>") {
		t.Error("document missing debtor BIC")
	}
}
