package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// pain008Fixture builds a ledger with a funded payer "cust", payee
// "merchant", registered IBANs, and one mandate with the given
// frequency and validity window.
func pain008Fixture(t *testing.T, frequency string, validFrom, validUntil time.Time) (*Ledger, Mandate) {
	t.Helper()
	l := New()
	seedBalance(t, l, "cust", 1_000_000)
	mustSetIBAN(t, l, "cust", testIBANDE, "")
	mustSetIBAN(t, l, "merchant", testIBANGB, "")
	m, err := l.CreateMandate(Mandate{
		ID:             "m-008",
		Payer:          "cust",
		Payee:          "merchant",
		MaxAmountCents: 5000,
		Currency:       "USD",
		Frequency:      frequency,
		ValidFrom:      validFrom,
		ValidUntil:     validUntil,
	})
	if err != nil {
		t.Fatalf("CreateMandate: %v", err)
	}
	return l, m
}

// pain008Debit books one mandate debit at the given timestamp.
func pain008Debit(t *testing.T, l *Ledger, id string, at time.Time) {
	t.Helper()
	if _, err := l.DebitWithMandate("m-008", MandateDebit{ID: id, AmountCents: 1000, CreatedAt: at}); err != nil {
		t.Fatalf("DebitWithMandate %s: %v", id, err)
	}
}

func pain008Opts() Pain008ExportOptions {
	return Pain008ExportOptions{CreditorSchemeID: "DE98ZZZ09999999999"}
}

// pain008SeqTpOf extracts the SeqTp values of an export's PmtInf
// blocks in document order.
func pain008SeqTpOf(t *testing.T, doc []byte) []string {
	t.Helper()
	var out []string
	s := string(doc)
	for {
		i := strings.Index(s, "<SeqTp>")
		if i < 0 {
			break
		}
		s = s[i+len("<SeqTp>"):]
		j := strings.Index(s, "</SeqTp>")
		if j < 0 {
			t.Fatal("unterminated <SeqTp>")
		}
		out = append(out, s[:j])
		s = s[j:]
	}
	return out
}

// pain008Day returns today's UTC midnight: the anchor for
// window-relative test dates (mandates must be live at export time).
func pain008Day() time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}

// A mandate whose whole validity fits in one frequency window can
// only ever see one collection: OOFF.
func TestExportPain008_OneOff(t *testing.T) {
	day := pain008Day()
	l, _ := pain008Fixture(t, MandateFrequencyPerDay, day, day.Add(24*time.Hour))
	pain008Debit(t, l, "d-oo", day.Add(10*time.Hour))

	doc, err := l.ExportPain008("m-008", pain008Opts())
	if err != nil {
		t.Fatalf("ExportPain008: %v", err)
	}
	if got := pain008SeqTpOf(t, doc); len(got) != 1 || got[0] != Pain008SeqOOFF {
		t.Errorf("SeqTp = %v, want [OOFF]", got)
	}
	if !strings.Contains(string(doc), "<NbOfTxs>1</NbOfTxs>") {
		t.Error("export missing NbOfTxs=1")
	}
}

// FRST for the first collection, RCUR for the middle, FNAL for the
// collection in the mandate's last authorizable window.
func TestExportPain008_FirstRecurringFinal(t *testing.T) {
	day := pain008Day()
	monthStart := time.Date(day.Year(), day.Month(), 1, 0, 0, 0, 0, time.UTC)
	l, _ := pain008Fixture(t, MandateFrequencyPerMonth, monthStart, monthStart.AddDate(0, 3, 0))
	pain008Debit(t, l, "d-m0", monthStart.AddDate(0, 0, 9))
	pain008Debit(t, l, "d-m1", monthStart.AddDate(0, 1, 9))
	pain008Debit(t, l, "d-m2", monthStart.AddDate(0, 2, 9))

	doc, err := l.ExportPain008("m-008", pain008Opts())
	if err != nil {
		t.Fatalf("ExportPain008: %v", err)
	}
	// Document order is SeqTp-major: FNAL < FRST < RCUR alphabetically.
	got := pain008SeqTpOf(t, doc)
	want := []string{Pain008SeqFNAL, Pain008SeqFRST, Pain008SeqRCUR}
	if len(got) != len(want) {
		t.Fatalf("SeqTp = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("SeqTp[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// The unit-level sequence-type rules, including the exact window
// boundaries.
func TestPain008SeqTp_Rules(t *testing.T) {
	day := MandateFrequencyPerDay
	mk := func(from, until time.Time) Mandate {
		return Mandate{Frequency: day, ValidFrom: from, ValidUntil: until}
	}
	jan1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		m     Mandate
		prior int
		ts    time.Time
		want  string
	}{
		{"single window is OOFF", mk(jan1, jan1.Add(24*time.Hour)), 0, jan1.Add(time.Hour), Pain008SeqOOFF},
		{"first of many is FRST", mk(jan1, jan1.Add(72*time.Hour)), 0, jan1.Add(time.Hour), Pain008SeqFRST},
		{"middle is RCUR", mk(jan1, jan1.Add(72*time.Hour)), 1, jan1.Add(25 * time.Hour), Pain008SeqRCUR},
		{"last window is FNAL", mk(jan1, jan1.Add(72*time.Hour)), 2, jan1.Add(49 * time.Hour), Pain008SeqFNAL},
		{"FNAL wins over FRST on a two-window mandate", mk(jan1, jan1.Add(48*time.Hour)), 0, jan1.Add(25 * time.Hour), Pain008SeqFNAL},
	}
	for _, c := range cases {
		if got := pain008SeqTp(c.m, c.prior, c.ts); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// The export names the mandate, the scheme, the IBANs, the mandate
// signature date, and the control totals — the document a bank can
// trust.
func TestExportPain008_DocumentShape(t *testing.T) {
	day := pain008Day()
	l, _ := pain008Fixture(t, MandateFrequencyPerDay, day, day.Add(24*time.Hour))
	pain008Debit(t, l, "d-shape", day.Add(10*time.Hour))

	doc, err := l.ExportPain008("m-008", pain008Opts())
	if err != nil {
		t.Fatalf("ExportPain008: %v", err)
	}
	s := string(doc)
	dateStr := day.Format("2006-01-02")
	for _, want := range []string{
		`xmlns="urn:iso:std:iso:20022:tech:xsd:pain.008.001.02"`,
		"<CstmrDrctDbtInitn>",
		"<PmtMtd>DD</PmtMtd>",
		"<MndtId>m-008</MndtId>",
		"<DtOfSgntr>" + dateStr + "</DtOfSgntr>",
		"<AmdmntInd>false</AmdmntInd>",
		"<CdtrSchmeId><Id><PrtryId><Id>DE98ZZZ09999999999</Id></PrtryId></Id></CdtrSchmeId>",
		"<IBAN>" + testIBANDE + "</IBAN>", // debtor = payer
		"<IBAN>" + testIBANGB + "</IBAN>", // creditor = payee
		"<EndToEndId>d-shape</EndToEndId>",
		"<ReqdColltnDt>" + dateStr + "</ReqdColltnDt>",
		"<CtrlSum>10.00</CtrlSum>",
		"<SvcLvl><Cd>SEPA</Cd></SvcLvl>",
		"<LclInstrm><Cd>CORE</Cd></LclInstrm>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("document missing %q", want)
		}
	}
	// The debtor must be the payer (customer), not the merchant: the
	// .008 pulls from the debtor, the inverse of the .001.
	dbtrIdx := strings.Index(s, "<Dbtr><Nm>cust</Nm></Dbtr>")
	cdtrIdx := strings.Index(s, "<Cdtr><Nm>merchant</Nm></Cdtr>")
	if dbtrIdx < 0 || cdtrIdx < 0 {
		t.Fatal("document missing Dbtr=cust or Cdtr=merchant")
	}
}

// The MsgId is deterministic: the same export re-renders
// byte-identical, so re-exports are safe.
func TestExportPain008_Deterministic(t *testing.T) {
	day := pain008Day()
	l, _ := pain008Fixture(t, MandateFrequencyPerDay, day, day.Add(72*time.Hour))
	pain008Debit(t, l, "d-det", day.Add(10*time.Hour))

	a, err := l.ExportPain008("m-008", pain008Opts())
	if err != nil {
		t.Fatalf("first export: %v", err)
	}
	b, err := l.ExportPain008("m-008", pain008Opts())
	if err != nil {
		t.Fatalf("second export: %v", err)
	}
	if string(a) != string(b) {
		t.Error("same export rendered different bytes")
	}
	if !strings.Contains(string(a), "<MsgId>pain008-") {
		t.Error("MsgId missing pain008 prefix")
	}
}

// A mandate with no booked collections still renders a valid,
// empty instruction — like the pain.001 export.
func TestExportPain008_EmptyMandate(t *testing.T) {
	day := pain008Day()
	l, _ := pain008Fixture(t, MandateFrequencyPerDay, day, day.Add(72*time.Hour))
	doc, err := l.ExportPain008("m-008", pain008Opts())
	if err != nil {
		t.Fatalf("ExportPain008: %v", err)
	}
	s := string(doc)
	if !strings.Contains(s, "<NbOfTxs>0</NbOfTxs>") {
		t.Error("empty export missing NbOfTxs=0")
	}
	if strings.Contains(s, "<PmtInf>") {
		t.Error("empty export must not contain PmtInf")
	}
}

// Fail-closed: unknown, suspended, cancelled, or expired mandates
// refuse the export, as do a missing creditor scheme ID and missing
// IBANs. A collection instruction must never be generated without a
// live authorization behind it.
func TestExportPain008_FailClosed(t *testing.T) {
	day := pain008Day()
	l, _ := pain008Fixture(t, MandateFrequencyPerDay, day, day.Add(72*time.Hour))

	if _, err := l.ExportPain008("no-such", pain008Opts()); !errors.Is(err, ErrMandateNotFound) {
		t.Errorf("unknown mandate: err = %v, want ErrMandateNotFound", err)
	}
	if _, err := l.ExportPain008("m-008", Pain008ExportOptions{}); !errors.Is(err, ErrMissingPain008CreditorScheme) {
		t.Errorf("missing scheme: err = %v, want ErrMissingPain008CreditorScheme", err)
	}
	if _, err := l.ExportPain008("m-008", pain008Opts()); err != nil {
		t.Fatalf("control export: %v", err)
	}

	// Suspended mandates authorize nothing.
	if _, err := l.SuspendMandate("m-008"); err != nil {
		t.Fatalf("SuspendMandate: %v", err)
	}
	if _, err := l.ExportPain008("m-008", pain008Opts()); !errors.Is(err, ErrMandateNotActive) {
		t.Errorf("suspended mandate: err = %v, want ErrMandateNotActive", err)
	}

	// Expired mandates authorize nothing either.
	l2, _ := pain008Fixture(t, MandateFrequencyPerDay,
		time.Now().Add(-48*time.Hour), time.Now().Add(-24*time.Hour))
	if _, err := l2.ExportPain008("m-008", pain008Opts()); !errors.Is(err, ErrMandateExpired) {
		t.Errorf("expired mandate: err = %v, want ErrMandateExpired", err)
	}

	// Missing debtor IBAN refuses the whole export, not just the
	// debtor leg — a forged or empty identifier is worse than no
	// instruction.
	l3, _ := pain008Fixture(t, MandateFrequencyPerDay, day, day.Add(72*time.Hour))
	pain008Debit(t, l3, "d-iban", day.Add(10*time.Hour))
	l3.mu.Lock()
	delete(l3.accountIBANs, "cust")
	l3.mu.Unlock()
	if _, err := l3.ExportPain008("m-008", pain008Opts()); !errors.Is(err, ErrMissingAccountIBAN) {
		t.Errorf("missing debtor IBAN: err = %v, want ErrMissingAccountIBAN", err)
	}
}

// The time window filters collections half-open [Since, Until), the
// OFX export convention.
func TestExportPain008_TimeWindow(t *testing.T) {
	day := pain008Day()
	l, _ := pain008Fixture(t, MandateFrequencyPerDay, day, day.Add(72*time.Hour))
	pain008Debit(t, l, "d-w1", day.Add(10*time.Hour))
	pain008Debit(t, l, "d-w2", day.Add(34*time.Hour))

	opts := pain008Opts()
	opts.Since = day.Add(24 * time.Hour)
	opts.Until = day.Add(48 * time.Hour)
	doc, err := l.ExportPain008("m-008", opts)
	if err != nil {
		t.Fatalf("ExportPain008: %v", err)
	}
	s := string(doc)
	if strings.Contains(s, "d-w1") {
		t.Error("windowed export includes d-w1")
	}
	if !strings.Contains(s, "d-w2") {
		t.Error("windowed export missing d-w2")
	}
	if _, err := l.ExportPain008("m-008", Pain008ExportOptions{
		CreditorSchemeID: "X", Since: day.Add(time.Hour), Until: day,
	}); !errors.Is(err, ErrInvalidPain008Range) {
		t.Errorf("inverted range: err = %v, want ErrInvalidPain008Range", err)
	}
}

// The control sum is overflow-checked: a wrapped total is worse than
// no export.
func TestRenderPain008_Overflow(t *testing.T) {
	m := Mandate{ID: "m", Payer: "cust", Payee: "merchant", Currency: "USD"}
	big := pain008Collection{Entry: JournalEntry{ID: "big", AmountCents: 1 << 62, Currency: "USD"}}
	if _, _, _, _, err := renderPain008(m, pain008Opts(), []pain008Collection{big, big}, []string{"big", "big2"}); !errors.Is(err, ErrPain008AmountOverflow) {
		t.Errorf("overflow: err = %v, want ErrPain008AmountOverflow", err)
	}
}
