package ledger

import (
	"errors"
	"strings"
	"testing"
)

func TestExportPain010_HappyPathAllFields(t *testing.T) {
	day := pain008Day()
	l, m := pain008Fixture(t, MandateFrequencyPerMonth, day, day.AddDate(1, 0, 0))
	effective := day.AddDate(0, 1, 0)

	doc, err := l.ExportPain010(m.ID, "DE98ZZZ09999999999", MandateAmendment{
		CreditorIBAN:   "DE89370400440532013000",
		DebtorIBAN:     "GB29NWBK60161331926819",
		MaxAmountCents: 9000,
		Frequency:      MandateFrequencyPerWeek,
		EffectiveDate:  effective,
	})
	if err != nil {
		t.Fatalf("ExportPain010: %v", err)
	}
	s := string(doc)
	for _, want := range []string{
		`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pain.010.001.02">`,
		"<MndtAmdmntReq>",
		"<MsgId>" + pain010MsgID(m.ID, effective) + "</MsgId>",
		"<OrgnlMndt>",
		"<MndtId>m-008</MndtId>",
		"<Id>DE98ZZZ09999999999</Id>",
		"<CdtrAcct><Id><IBAN>DE89370400440532013000</IBAN></Id></CdtrAcct>",
		"<DbtrAcct><Id><IBAN>GB29NWBK60161331926819</IBAN></Id></DbtrAcct>",
		`<MaxAmt Ccy="USD">90.00</MaxAmt>`,
		"<Frqcy>per-week</Frqcy>",
		"<FrDt>" + effective.Format("2006-01-02") + "</FrDt>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("document missing %q:\n%s", want, s)
		}
	}
	// An amendment request carries the authorization change, never a
	// collection: no instructed amount rides the wire.
	if strings.Contains(s, "InstdAmt") {
		t.Error("pain.010 document carries an instructed amount")
	}
}

func TestExportPain010_OnlyChangedFieldsRender(t *testing.T) {
	day := pain008Day()
	l, m := pain008Fixture(t, MandateFrequencyPerMonth, day, day.AddDate(1, 0, 0))
	effective := day.AddDate(0, 2, 0)

	// Amend only the cap: no account elements, no frequency element.
	doc, err := l.ExportPain010(m.ID, "DE98ZZZ09999999999", MandateAmendment{
		MaxAmountCents: 7500,
		EffectiveDate:  effective,
	})
	if err != nil {
		t.Fatalf("ExportPain010: %v", err)
	}
	s := string(doc)
	for _, want := range []string{
		`<MaxAmt Ccy="USD">75.00</MaxAmt>`,
		"<FrDt>" + effective.Format("2006-01-02") + "</FrDt>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("document missing %q:\n%s", want, s)
		}
	}
	for _, absent := range []string{"<CdtrAcct>", "<DbtrAcct>", "<Frqcy>"} {
		if strings.Contains(s, absent) {
			t.Errorf("document restates unchanged field %q:\n%s", absent, s)
		}
	}
}

func TestExportPain010_DeterministicMsgID(t *testing.T) {
	day := pain008Day()
	l, m := pain008Fixture(t, MandateFrequencyPerMonth, day, day.AddDate(1, 0, 0))
	effective := day.AddDate(0, 1, 0)

	first, err := l.ExportPain010(m.ID, "DE98ZZZ09999999999", MandateAmendment{
		Frequency:     MandateFrequencyPerWeek,
		EffectiveDate: effective,
	})
	if err != nil {
		t.Fatalf("first export: %v", err)
	}
	second, err := l.ExportPain010(m.ID, "DE98ZZZ09999999999", MandateAmendment{
		Frequency:     MandateFrequencyPerWeek,
		EffectiveDate: effective,
	})
	if err != nil {
		t.Fatalf("second export: %v", err)
	}
	if !strings.Contains(string(second), "<MsgId>"+pain010MsgID(m.ID, effective)+"</MsgId>") {
		t.Error("repeat export carries a different MsgId")
	}
	// A different effective date changes the MsgId: two amendment
	// rounds for one mandate never share an ID.
	other, err := l.ExportPain010(m.ID, "DE98ZZZ09999999999", MandateAmendment{
		Frequency:     MandateFrequencyPerWeek,
		EffectiveDate: day.AddDate(0, 3, 0),
	})
	if err != nil {
		t.Fatalf("other-date export: %v", err)
	}
	if strings.Contains(string(other), "<MsgId>"+pain010MsgID(m.ID, effective)+"</MsgId>") {
		t.Error("different effective date reuses the MsgId")
	}
	_ = first
}

func TestExportPain010_ReadOnly(t *testing.T) {
	day := pain008Day()
	l, m := pain008Fixture(t, MandateFrequencyPerMonth, day, day.AddDate(1, 0, 0))

	before := l.Version()
	if _, err := l.ExportPain010(m.ID, "DE98ZZZ09999999999", MandateAmendment{
		Frequency:     MandateFrequencyPerWeek,
		EffectiveDate: day.AddDate(0, 1, 0),
	}); err != nil {
		t.Fatalf("ExportPain010: %v", err)
	}
	if l.Version() != before {
		t.Errorf("version moved %d -> %d on a read-only export", before, l.Version())
	}
	got, ok := l.GetMandate(m.ID)
	if !ok {
		t.Fatal("GetMandate: mandate vanished")
	}
	if got.Frequency != MandateFrequencyPerMonth || got.MaxAmountCents != 5000 {
		t.Errorf("mandate mutated by export: %+v", got)
	}
}

func TestExportPain010_FailClosed(t *testing.T) {
	day := pain008Day()
	l, m := pain008Fixture(t, MandateFrequencyPerMonth, day, day.AddDate(1, 0, 0))
	effective := day.AddDate(0, 1, 0)

	cases := []struct {
		name string
		call func() error
		want error
	}{
		{"missing scheme", func() error {
			_, err := l.ExportPain010(m.ID, "", MandateAmendment{Frequency: MandateFrequencyPerWeek, EffectiveDate: effective})
			return err
		}, ErrMissingPain010CreditorScheme},
		{"no amendment", func() error {
			_, err := l.ExportPain010(m.ID, "DE98ZZZ09999999999", MandateAmendment{EffectiveDate: effective})
			return err
		}, ErrPain010NoAmendment},
		{"missing effective date", func() error {
			_, err := l.ExportPain010(m.ID, "DE98ZZZ09999999999", MandateAmendment{Frequency: MandateFrequencyPerWeek})
			return err
		}, ErrPain010EffectiveDateRequired},
		{"negative cap", func() error {
			_, err := l.ExportPain010(m.ID, "DE98ZZZ09999999999", MandateAmendment{MaxAmountCents: -5, EffectiveDate: effective})
			return err
		}, ErrMandateNonPositiveMaxAmount},
		{"bad creditor IBAN", func() error {
			_, err := l.ExportPain010(m.ID, "DE98ZZZ09999999999", MandateAmendment{CreditorIBAN: "NOTANIBAN", EffectiveDate: effective})
			return err
		}, ErrInvalidIBAN},
		{"bad frequency", func() error {
			_, err := l.ExportPain010(m.ID, "DE98ZZZ09999999999", MandateAmendment{Frequency: "per-fortnight", EffectiveDate: effective})
			return err
		}, ErrInvalidMandateFrequency},
		{"unknown mandate", func() error {
			_, err := l.ExportPain010("m-ghost", "DE98ZZZ09999999999", MandateAmendment{Frequency: MandateFrequencyPerWeek, EffectiveDate: effective})
			return err
		}, ErrMandateNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want wrapped %v", err, tc.want)
			}
		})
	}
}

func TestExportPain010_InactiveMandateRefused(t *testing.T) {
	day := pain008Day()
	l, m := pain008Fixture(t, MandateFrequencyPerMonth, day, day.AddDate(1, 0, 0))
	effective := day.AddDate(0, 1, 0)

	// Suspended: the amendment would change an authorization that
	// is not currently in force.
	if _, err := l.SuspendMandate(m.ID); err != nil {
		t.Fatalf("SuspendMandate: %v", err)
	}
	_, err := l.ExportPain010(m.ID, "DE98ZZZ09999999999", MandateAmendment{Frequency: MandateFrequencyPerWeek, EffectiveDate: effective})
	if !errors.Is(err, ErrMandateNotActive) {
		t.Errorf("suspended error = %v, want ErrMandateNotActive", err)
	}

	// Expired: a mandate past ValidUntil reads as expired even
	// before ExpireMandates persists the transition.
	l2, m2 := pain008Fixture(t, MandateFrequencyPerMonth, day.AddDate(-2, 0, 0), day.AddDate(-1, 0, 0))
	_, err = l2.ExportPain010(m2.ID, "DE98ZZZ09999999999", MandateAmendment{Frequency: MandateFrequencyPerWeek, EffectiveDate: effective})
	if !errors.Is(err, ErrMandateExpired) {
		t.Errorf("expired error = %v, want ErrMandateExpired", err)
	}
}

func TestExportPain010_IBANNormalization(t *testing.T) {
	// Lowercase + spaced IBANs normalize to canonical form on the
	// wire, the SetAccountIBAN convention.
	day := pain008Day()
	l, m := pain008Fixture(t, MandateFrequencyPerMonth, day, day.AddDate(1, 0, 0))

	doc, err := l.ExportPain010(m.ID, "DE98ZZZ09999999999", MandateAmendment{
		CreditorIBAN:  "de89 3704 0044 0532 0130 00",
		EffectiveDate: day.AddDate(0, 1, 0),
	})
	if err != nil {
		t.Fatalf("ExportPain010: %v", err)
	}
	if !strings.Contains(string(doc), "<IBAN>DE89370400440532013000</IBAN>") {
		t.Errorf("document does not carry normalized IBAN:\n%s", doc)
	}
}
