package ledger

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// pain010InboundFixture creates a ledger with an audit log and the
// pain.008 mandate m-008 (payer cust, payee merchant, cap 5000 USD,
// per-month, valid one year from today), plus a third account carol
// and a fourth account dave with registered IBANs for payer/payee
// amendment tests.
func pain010InboundFixture(t *testing.T, al *AuditLog) (*Ledger, Mandate) {
	t.Helper()
	l := New(WithAuditLog(al))
	seedBalance(t, l, "cust", 1_000_000)
	mustSetIBAN(t, l, "cust", testIBANDE, "")
	mustSetIBAN(t, l, "merchant", testIBANGB, "")
	mustSetIBAN(t, l, "carol", testIBANFR, "")
	mustSetIBAN(t, l, "dave", testIBANNL, "")
	day := pain008Day()
	m, err := l.CreateMandate(Mandate{
		ID:             "m-008",
		Payer:          "cust",
		Payee:          "merchant",
		MaxAmountCents: 5000,
		Currency:       "USD",
		Frequency:      MandateFrequencyPerMonth,
		ValidFrom:      day,
		ValidUntil:     day.AddDate(1, 0, 0),
	})
	if err != nil {
		t.Fatalf("CreateMandate: %v", err)
	}
	return l, m
}

// pain010Doc renders one inbound pain.010.001.02 document of the
// given schema version: msgID amending mandate mndtID under
// schemeID, with the stated creditor/debtor IBANs, MaxAmt (amount
// + currency), frequency, and effective date — the shape
// ExportPain010 renders, so a document this ledger's export
// renders parses back. Empty optional fields are omitted.
func pain010Doc(version, msgID, mndtID, schemeID, cdtrIBAN, dbtrIBAN, maxAmt, maxCcy, frqcy, frDt string) string {
	var sb strings.Builder
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pain.010.` + version + `">`)
	sb.WriteString(`<MndtAmdmntReq><GrpHdr><MsgId>` + msgID + `</MsgId>`)
	sb.WriteString(`<CreDtTm>2026-11-15T09:00:00Z</CreDtTm></GrpHdr>`)
	sb.WriteString(`<Mndt><OrgnlMndt><MndtId>` + mndtID + `</MndtId>`)
	sb.WriteString(`<CdtrSchmeId><Id><PrtryId><Id>` + schemeID + `</Id></PrtryId></Id></CdtrSchmeId>`)
	sb.WriteString(`</OrgnlMndt><AmdmntInf>`)
	if cdtrIBAN != "" {
		sb.WriteString(`<CdtrAcct><Id><IBAN>` + cdtrIBAN + `</IBAN></Id></CdtrAcct>`)
	}
	if dbtrIBAN != "" {
		sb.WriteString(`<DbtrAcct><Id><IBAN>` + dbtrIBAN + `</IBAN></Id></DbtrAcct>`)
	}
	if maxAmt != "" {
		sb.WriteString(`<MaxAmt Ccy="` + maxCcy + `">` + maxAmt + `</MaxAmt>`)
	}
	if frqcy != "" {
		sb.WriteString(`<Frqcy>` + frqcy + `</Frqcy>`)
	}
	if frDt != "" {
		sb.WriteString(`<FrDt>` + frDt + `</FrDt>`)
	}
	sb.WriteString(`</AmdmntInf></Mndt></MndtAmdmntReq></Document>`)
	return sb.String()
}

// parsePain010Doc parses doc and applies it, failing the test on
// parse errors: the gates under test are the apply gates, not the
// parse gates.
func applyPain010Doc(t *testing.T, l *Ledger, doc string) (Pain010ApplyResult, error) {
	t.Helper()
	req, err := ParsePain010([]byte(doc))
	if err != nil {
		t.Fatalf("ParsePain010: %v", err)
	}
	return l.ApplyPain010(req, "creditor-bank")
}

// pain010ChangedFieldsOf reads the changed_fields detail of an
// audit event back out of its JSON-decoded form.
func pain010ChangedFieldsOf(ev *AuditEvent) []string {
	raw, ok := ev.Details["changed_fields"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func TestParsePain010ValidDocument(t *testing.T) {
	day := pain008Day()
	effective := day.AddDate(0, 1, 0).Format("2006-01-02")
	req, err := ParsePain010([]byte(pain010Doc("001.02", "msg-1", "m-008", "DE98ZZZ09999999999",
		testIBANDE, testIBANGB, "90.00", "USD", "per-week", effective)))
	if err != nil {
		t.Fatalf("ParsePain010: %v", err)
	}
	if req.MsgID != "msg-1" || req.MndtID != "m-008" || req.CreditorSchemeID != "DE98ZZZ09999999999" {
		t.Errorf("req = %+v, want msg-1/m-008/DE98ZZZ09999999999", req)
	}
	if req.Amendment.CreditorIBAN != testIBANDE || req.Amendment.DebtorIBAN != testIBANGB {
		t.Errorf("IBANs = %q/%q, want %q/%q", req.Amendment.CreditorIBAN, req.Amendment.DebtorIBAN, testIBANDE, testIBANGB)
	}
	if req.Amendment.MaxAmountCents != 9000 || req.Currency != "USD" || req.Amendment.Frequency != "per-week" {
		t.Errorf("amendment = %+v currency %q, want 9000/USD/per-week", req.Amendment, req.Currency)
	}
	if got := req.Amendment.EffectiveDate.Format("2006-01-02"); got != effective {
		t.Errorf("effective date = %q, want %q", got, effective)
	}
}

func TestParsePain010RoundTripsExport(t *testing.T) {
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
	req, err := ParsePain010(doc)
	if err != nil {
		t.Fatalf("ParsePain010(export): %v", err)
	}
	if req.MsgID != pain010MsgID(m.ID, effective) || req.MndtID != "m-008" {
		t.Errorf("req = %+v, want msg %q / m-008", req, pain010MsgID(m.ID, effective))
	}
	if req.Amendment.MaxAmountCents != 9000 || req.Amendment.Frequency != "per-week" {
		t.Errorf("amendment = %+v, want 9000/per-week", req.Amendment)
	}
	if !req.Amendment.EffectiveDate.Equal(effective) {
		t.Errorf("effective date = %v, want %v", req.Amendment.EffectiveDate, effective)
	}
}

func TestParsePain010WrongNamespace(t *testing.T) {
	day := pain008Day()
	effective := day.AddDate(0, 1, 0).Format("2006-01-02")
	doc := pain010Doc("001.01", "msg-1", "m-008", "DE98ZZZ09999999999", "", "", "90.00", "USD", "", effective)
	if _, err := ParsePain010([]byte(doc)); !errors.Is(err, ErrInvalidPain010) {
		t.Errorf("err = %v, want ErrInvalidPain010", err)
	}
}

func TestParsePain010MissingEffectiveDate(t *testing.T) {
	doc := pain010Doc("001.02", "msg-1", "m-008", "DE98ZZZ09999999999", "", "", "90.00", "USD", "", "")
	if _, err := ParsePain010([]byte(doc)); !errors.Is(err, ErrPain010EffectiveDateRequired) {
		t.Errorf("err = %v, want ErrPain010EffectiveDateRequired", err)
	}
	// A malformed effective date is a parse refusal too.
	doc = pain010Doc("001.02", "msg-1", "m-008", "DE98ZZZ09999999999", "", "", "90.00", "USD", "", "not-a-date")
	if _, err := ParsePain010([]byte(doc)); !errors.Is(err, ErrInvalidPain010) {
		t.Errorf("malformed FrDt: err = %v, want ErrInvalidPain010", err)
	}
}

func TestParsePain010NoChangedFields(t *testing.T) {
	day := pain008Day()
	effective := day.AddDate(0, 1, 0).Format("2006-01-02")
	doc := pain010Doc("001.02", "msg-1", "m-008", "DE98ZZZ09999999999", "", "", "", "", "", effective)
	if _, err := ParsePain010([]byte(doc)); !errors.Is(err, ErrPain010NoAmendment) {
		t.Errorf("err = %v, want ErrPain010NoAmendment", err)
	}
	// An invalid frequency is refused at parse, never applied.
	doc = pain010Doc("001.02", "msg-1", "m-008", "DE98ZZZ09999999999", "", "", "", "", "per-hour", effective)
	if _, err := ParsePain010([]byte(doc)); !errors.Is(err, ErrInvalidMandateFrequency) {
		t.Errorf("bad frequency: err = %v, want ErrInvalidMandateFrequency", err)
	}
}

func TestApplyPain010UnknownMandate(t *testing.T) {
	al := mustAuditLog(t)
	l, _ := pain010InboundFixture(t, al)
	day := pain008Day()
	effective := day.AddDate(0, 1, 0).Format("2006-01-02")
	_, err := applyPain010Doc(t, l, pain010Doc("001.02", "msg-1", "no-such", "DE98ZZZ09999999999",
		"", "", "90.00", "USD", "", effective))
	if !errors.Is(err, ErrMandateNotFound) {
		t.Errorf("err = %v, want ErrMandateNotFound", err)
	}
}

func TestApplyPain010NonActiveMandate(t *testing.T) {
	al := mustAuditLog(t)
	l, m := pain010InboundFixture(t, al)
	day := pain008Day()
	effective := day.AddDate(0, 1, 0).Format("2006-01-02")
	doc := pain010Doc("001.02", "msg-1", "m-008", "DE98ZZZ09999999999", "", "", "90.00", "USD", "", effective)

	// Suspended: the amendment gate refuses it.
	if _, err := l.SuspendMandate(m.ID); err != nil {
		t.Fatalf("SuspendMandate: %v", err)
	}
	if _, err := applyPain010Doc(t, l, doc); !errors.Is(err, ErrMandateNotActive) {
		t.Errorf("suspended: err = %v, want ErrMandateNotActive", err)
	}

	// Expired: a window already past refuses it the same way.
	l2 := New()
	seedBalance(t, l2, "cust", 1_000_000)
	mustSetIBAN(t, l2, "cust", testIBANDE, "")
	mustSetIBAN(t, l2, "merchant", testIBANGB, "")
	if _, err := l2.CreateMandate(Mandate{
		ID: "m-old", Payer: "cust", Payee: "merchant",
		MaxAmountCents: 5000, Currency: "USD", Frequency: MandateFrequencyPerMonth,
		ValidFrom: day.AddDate(-1, 0, 0), ValidUntil: day.AddDate(0, -1, 0),
	}); err != nil {
		t.Fatalf("CreateMandate expired: %v", err)
	}
	doc2 := pain010Doc("001.02", "msg-2", "m-old", "DE98ZZZ09999999999", "", "", "90.00", "USD", "", effective)
	if _, err := applyPain010Doc(t, l2, doc2); !errors.Is(err, ErrMandateExpired) {
		t.Errorf("expired: err = %v, want ErrMandateExpired", err)
	}
}

func TestApplyPain010UnknownAccount(t *testing.T) {
	al := mustAuditLog(t)
	l, _ := pain010InboundFixture(t, al)
	day := pain008Day()
	effective := day.AddDate(0, 1, 0).Format("2006-01-02")
	// A well-formed IBAN the ledger never registered: the ledger
	// never opens an account to receive an amendment. dave's
	// IBAN is testIBANNL, registered above — use an unregistered
	// one instead.
	_, err := applyPain010Doc(t, l, pain010Doc("001.02", "msg-1", "m-008", "DE98ZZZ09999999999",
		"CH9300762011623852957", "", "", "", "", effective))
	if !errors.Is(err, ErrPain010UnknownAccount) {
		t.Errorf("err = %v, want ErrPain010UnknownAccount", err)
	}
	// The mandate is untouched.
	if m, ok := l.GetMandate("m-008"); !ok || m.Payee != "merchant" {
		t.Errorf("mandate = %+v, want payee merchant untouched", m)
	}
}

func TestApplyPain010MaxAmountAmendment(t *testing.T) {
	al := mustAuditLog(t)
	l, _ := pain010InboundFixture(t, al)
	day := pain008Day()
	effective := day.AddDate(0, 1, 0).Format("2006-01-02")
	before := l.Version()

	res, err := applyPain010Doc(t, l, pain010Doc("001.02", "msg-1", "m-008", "DE98ZZZ09999999999",
		"", "", "90.00", "USD", "", effective))
	if err != nil {
		t.Fatalf("ApplyPain010: %v", err)
	}
	if res.Duplicate {
		t.Error("duplicate = true on first apply")
	}
	if res.Mandate.MaxAmountCents != 9000 {
		t.Errorf("cap = %d, want 9000", res.Mandate.MaxAmountCents)
	}
	if res.Mandate.Currency != "USD" || res.Mandate.Frequency != "per-month" || res.Mandate.Payee != "merchant" {
		t.Errorf("mandate = %+v, want only the cap moved", res.Mandate)
	}
	// Amending is structural: no version bump.
	if l.Version() != before {
		t.Errorf("version = %d, want %d (structural, no bump)", l.Version(), before)
	}
	// Audit: the per-field event plus the inbound marker, each
	// exactly once, with the exact details.
	events := readAuditOps(t, al)
	if countOp(events, "mandate_max_amount_amended") != 1 {
		t.Errorf("mandate_max_amount_amended count = %d, want 1", countOp(events, "mandate_max_amount_amended"))
	}
	if countOp(events, "pain010_amendment_applied") != 1 {
		t.Errorf("pain010_amendment_applied count = %d, want 1", countOp(events, "pain010_amendment_applied"))
	}
	ev := findOp(events, "pain010_amendment_applied")
	if ev == nil {
		t.Fatal("pain010_amendment_applied event missing")
	}
	if ev.TraceID != "msg-1" {
		t.Errorf("trace = %q, want msg-1", ev.TraceID)
	}
	if ev.Details["msg_id"] != "msg-1" || ev.Details["mandate_id"] != "m-008" {
		t.Errorf("details = %v, want msg-1/m-008", ev.Details)
	}
	if got := fmt.Sprintf("%v", pain010ChangedFieldsOf(ev)); got != "[max_amount_cents]" {
		t.Errorf("changed_fields = %s, want [max_amount_cents]", got)
	}
	amt := findOp(events, "mandate_max_amount_amended")
	if amt == nil {
		t.Fatal("mandate_max_amount_amended event missing")
	}
	if amt.Details["previous_max_amount_cents"] != float64(5000) || amt.Details["max_amount_cents"] != float64(9000) {
		t.Errorf("cap event details = %v, want 5000 -> 9000", amt.Details)
	}
}

func TestApplyPain010FrequencyAmendment(t *testing.T) {
	al := mustAuditLog(t)
	l, _ := pain010InboundFixture(t, al)
	day := pain008Day()
	effective := day.AddDate(0, 1, 0).Format("2006-01-02")

	res, err := applyPain010Doc(t, l, pain010Doc("001.02", "msg-1", "m-008", "DE98ZZZ09999999999",
		"", "", "", "", "per-week", effective))
	if err != nil {
		t.Fatalf("ApplyPain010: %v", err)
	}
	if res.Mandate.Frequency != "per-week" {
		t.Errorf("frequency = %q, want per-week", res.Mandate.Frequency)
	}
	if res.Mandate.MaxAmountCents != 5000 {
		t.Errorf("cap = %d, want 5000 untouched", res.Mandate.MaxAmountCents)
	}
	events := readAuditOps(t, al)
	if countOp(events, "mandate_frequency_amended") != 1 {
		t.Errorf("mandate_frequency_amended count = %d, want 1", countOp(events, "mandate_frequency_amended"))
	}
	if countOp(events, "pain010_amendment_applied") != 1 {
		t.Errorf("pain010_amendment_applied count = %d, want 1", countOp(events, "pain010_amendment_applied"))
	}
	if got := fmt.Sprintf("%v", pain010ChangedFieldsOf(findOp(events, "pain010_amendment_applied"))); got != "[frequency]" {
		t.Errorf("changed_fields = %s, want [frequency]", got)
	}
}

func TestApplyPain010PayerPayeeAmendment(t *testing.T) {
	al := mustAuditLog(t)
	l, _ := pain010InboundFixture(t, al)
	day := pain008Day()
	effective := day.AddDate(0, 1, 0).Format("2006-01-02")

	// Payee merchant -> carol (creditor IBAN), payer cust -> dave
	// (debtor IBAN): both legs move in one amendment.
	res, err := applyPain010Doc(t, l, pain010Doc("001.02", "msg-1", "m-008", "DE98ZZZ09999999999",
		testIBANFR, testIBANNL, "", "", "", effective))
	if err != nil {
		t.Fatalf("ApplyPain010: %v", err)
	}
	if res.Mandate.Payee != "carol" || res.Mandate.Payer != "dave" {
		t.Errorf("mandate = %+v, want payer dave / payee carol", res.Mandate)
	}
	if res.Mandate.MaxAmountCents != 5000 || res.Mandate.Frequency != "per-month" {
		t.Errorf("mandate = %+v, want cap/frequency untouched", res.Mandate)
	}
	events := readAuditOps(t, al)
	if countOp(events, "mandate_payee_amended") != 1 || countOp(events, "mandate_payer_amended") != 1 {
		t.Errorf("payee/payer events = %d/%d, want 1/1", countOp(events, "mandate_payee_amended"), countOp(events, "mandate_payer_amended"))
	}
	if got := fmt.Sprintf("%v", pain010ChangedFieldsOf(findOp(events, "pain010_amendment_applied"))); got != "[creditor_iban debtor_iban]" {
		t.Errorf("changed_fields = %s, want [creditor_iban debtor_iban]", got)
	}
}

func TestApplyPain010CurrencyAmendment(t *testing.T) {
	al := mustAuditLog(t)
	l, _ := pain010InboundFixture(t, al)
	day := pain008Day()
	effective := day.AddDate(0, 1, 0).Format("2006-01-02")

	// MaxAmt in EUR: the cap moves and the currency with it.
	res, err := applyPain010Doc(t, l, pain010Doc("001.02", "msg-1", "m-008", "DE98ZZZ09999999999",
		"", "", "90.00", "EUR", "", effective))
	if err != nil {
		t.Fatalf("ApplyPain010: %v", err)
	}
	if res.Mandate.MaxAmountCents != 9000 || res.Mandate.Currency != "EUR" {
		t.Errorf("mandate = %+v, want cap 9000 EUR", res.Mandate)
	}
	events := readAuditOps(t, al)
	if countOp(events, "mandate_currency_amended") != 1 {
		t.Errorf("mandate_currency_amended count = %d, want 1", countOp(events, "mandate_currency_amended"))
	}
	if got := fmt.Sprintf("%v", pain010ChangedFieldsOf(findOp(events, "pain010_amendment_applied"))); got != "[max_amount_cents currency]" {
		t.Errorf("changed_fields = %s, want [max_amount_cents currency]", got)
	}
}

func TestApplyPain010SamePartyRefused(t *testing.T) {
	al := mustAuditLog(t)
	l, _ := pain010InboundFixture(t, al)
	day := pain008Day()
	effective := day.AddDate(0, 1, 0).Format("2006-01-02")

	// New payee = the payer's IBAN: the combined outcome would
	// put one account on both sides, so nothing moves.
	_, err := applyPain010Doc(t, l, pain010Doc("001.02", "msg-1", "m-008", "DE98ZZZ09999999999",
		testIBANDE, "", "", "", "", effective))
	if !errors.Is(err, ErrMandateSameParty) {
		t.Fatalf("err = %v, want ErrMandateSameParty", err)
	}
	if m, ok := l.GetMandate("m-008"); !ok || m.Payee != "merchant" || m.Payer != "cust" {
		t.Errorf("mandate = %+v, want cust/merchant untouched", m)
	}
	events := readAuditOps(t, al)
	if countOp(events, "pain010_amendment_applied") != 0 {
		t.Errorf("pain010_amendment_applied count = %d, want 0", countOp(events, "pain010_amendment_applied"))
	}
}

func TestApplyPain010ReplaySameTerms(t *testing.T) {
	al := mustAuditLog(t)
	l, _ := pain010InboundFixture(t, al)
	day := pain008Day()
	effective := day.AddDate(0, 1, 0).Format("2006-01-02")
	doc := pain010Doc("001.02", "msg-1", "m-008", "DE98ZZZ09999999999", "", "", "90.00", "USD", "per-week", effective)

	first, err := applyPain010Doc(t, l, doc)
	if err != nil {
		t.Fatalf("first ApplyPain010: %v", err)
	}
	if first.Mandate.MaxAmountCents != 9000 || first.Mandate.Frequency != "per-week" {
		t.Fatalf("first mandate = %+v, want 9000/per-week", first.Mandate)
	}
	// Replaying the same MsgId with the same terms: the current
	// mandate comes back, nothing re-amends, no second audit.
	second, err := applyPain010Doc(t, l, doc)
	if err != nil {
		t.Fatalf("replay ApplyPain010: %v", err)
	}
	if !second.Duplicate {
		t.Error("duplicate = false on replay, want true")
	}
	if second.Mandate.MaxAmountCents != 9000 || second.Mandate.Frequency != "per-week" {
		t.Errorf("replay mandate = %+v, want 9000/per-week", second.Mandate)
	}
	events := readAuditOps(t, al)
	if countOp(events, "pain010_amendment_applied") != 1 {
		t.Errorf("pain010_amendment_applied count = %d, want 1 (no second event on replay)", countOp(events, "pain010_amendment_applied"))
	}
	if countOp(events, "mandate_max_amount_amended") != 1 || countOp(events, "mandate_frequency_amended") != 1 {
		t.Errorf("field events = %d/%d, want 1/1 (no second events on replay)",
			countOp(events, "mandate_max_amount_amended"), countOp(events, "mandate_frequency_amended"))
	}
}

func TestApplyPain010MsgIDConflict(t *testing.T) {
	al := mustAuditLog(t)
	l, _ := pain010InboundFixture(t, al)
	day := pain008Day()
	effective := day.AddDate(0, 1, 0).Format("2006-01-02")

	first := pain010Doc("001.02", "msg-1", "m-008", "DE98ZZZ09999999999", "", "", "90.00", "USD", "", effective)
	if _, err := applyPain010Doc(t, l, first); err != nil {
		t.Fatalf("first ApplyPain010: %v", err)
	}
	// The same MsgId carrying different terms is refused, never
	// silently overwritten.
	second := pain010Doc("001.02", "msg-1", "m-008", "DE98ZZZ09999999999", "", "", "80.00", "USD", "", effective)
	if _, err := applyPain010Doc(t, l, second); !errors.Is(err, ErrPain010MsgIDConflict) {
		t.Fatalf("err = %v, want ErrPain010MsgIDConflict", err)
	}
	// The first amendment stands.
	if m, ok := l.GetMandate("m-008"); !ok || m.MaxAmountCents != 9000 {
		t.Errorf("mandate = %+v, want cap 9000 from the first amendment", m)
	}
}

func TestApplyPain010SameValueAmendment(t *testing.T) {
	al := mustAuditLog(t)
	l, _ := pain010InboundFixture(t, al)
	day := pain008Day()
	effective := day.AddDate(0, 1, 0).Format("2006-01-02")

	// Amend the cap to the value it already has, under a fresh
	// MsgId: the cap entry records the amendment even at an
	// unchanged value (its established semantics — only the
	// payer/payee/currency entries no-op on a same value), so
	// one field event fires; the mandate itself is unchanged.
	// Audit-free replays come from the MsgId registry, covered
	// by TestApplyPain010ReplaySameTerms.
	doc := pain010Doc("001.02", "msg-1", "m-008", "DE98ZZZ09999999999", "", "", "50.00", "USD", "", effective)
	res, err := applyPain010Doc(t, l, doc)
	if err != nil {
		t.Fatalf("ApplyPain010: %v", err)
	}
	if res.Mandate.MaxAmountCents != 5000 {
		t.Errorf("cap = %d, want 5000 unchanged", res.Mandate.MaxAmountCents)
	}
	events := readAuditOps(t, al)
	if countOp(events, "mandate_max_amount_amended") != 1 {
		t.Errorf("mandate_max_amount_amended count = %d, want 1 (the entry records the amendment)", countOp(events, "mandate_max_amount_amended"))
	}
	if countOp(events, "pain010_amendment_applied") != 1 {
		t.Errorf("pain010_amendment_applied count = %d, want 1", countOp(events, "pain010_amendment_applied"))
	}
}
