package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// pain009InboundDoc renders a minimal pain.009 inbound document for
// the given schema version: the counterparty creditor's initiation
// request msgID for mandate mndtID under schemeID, creditor
// cdtrName/cdtrIBAN, debtor dbtrName/dbtrIBAN (agent BIC dbtrBIC
// when non-empty), valid FrDt..ToDt. Empty fields omit their
// elements, so callers can build the missing-field shapes.
func pain009InboundDoc(t *testing.T, version, msgID, mndtID, schemeID, cdtrName, cdtrIBAN, dbtrName, dbtrIBAN, dbtrBIC, frDt, toDt string) []byte {
	t.Helper()
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?>`)
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pain.009.` + version + `">`)
	sb.WriteString(`<MndtInitnReq><GrpHdr>`)
	if msgID != "" {
		sb.WriteString(`<MsgId>` + msgID + `</MsgId>`)
	}
	sb.WriteString(`<CreDtTm>2026-11-15T09:00:00Z</CreDtTm></GrpHdr>`)
	sb.WriteString(`<Mndt>`)
	if mndtID != "" {
		sb.WriteString(`<MndtId>` + mndtID + `</MndtId>`)
	}
	if schemeID != "" {
		sb.WriteString(`<CdtrSchmeId><Id><PrtryId><Id>` + schemeID + `</Id></PrtryId></Id></CdtrSchmeId>`)
	}
	if cdtrName != "" {
		sb.WriteString(`<Cdtr><Nm>` + cdtrName + `</Nm></Cdtr>`)
	}
	if cdtrIBAN != "" {
		sb.WriteString(`<CdtrAcct><Id><IBAN>` + cdtrIBAN + `</IBAN></Id></CdtrAcct>`)
	}
	if dbtrName != "" {
		sb.WriteString(`<Dbtr><Nm>` + dbtrName + `</Nm></Dbtr>`)
	}
	if dbtrIBAN != "" {
		sb.WriteString(`<DbtrAcct><Id><IBAN>` + dbtrIBAN + `</IBAN></Id></DbtrAcct>`)
	}
	if dbtrBIC != "" {
		sb.WriteString(`<DbtrAgt><FinInstnId><BIC>` + dbtrBIC + `</BIC></FinInstnId></DbtrAgt>`)
	}
	if frDt != "" {
		sb.WriteString(`<FrDt>` + frDt + `</FrDt>`)
	}
	if toDt != "" {
		sb.WriteString(`<ToDt>` + toDt + `</ToDt>`)
	}
	sb.WriteString(`</Mndt></MndtInitnReq></Document>`)
	return []byte(sb.String())
}

// pain009Fixture builds a ledger with an audit log and registered
// IBANs for alice (debtor, DE89370400440532013000) and bob
// (creditor, GB29NWBK60161331926819).
func pain009Fixture(t *testing.T) (*Ledger, *AuditLog) {
	t.Helper()
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	if err := l.SetAccountIBAN("alice", "DE89370400440532013000", "DEUTDEFF"); err != nil {
		t.Fatalf("SetAccountIBAN alice: %v", err)
	}
	if err := l.SetAccountIBAN("bob", "GB29NWBK60161331926819", "NWBKGB2L"); err != nil {
		t.Fatalf("SetAccountIBAN bob: %v", err)
	}
	return l, al
}

// pain009Doc is the standard valid inbound request: msg-1 for
// mandate mndt-1 under scheme SCHEME-1, bob (creditor) pulling
// from alice (debtor), 2026-11-01..2027-11-01.
func pain009Doc(t *testing.T) []byte {
	t.Helper()
	return pain009InboundDoc(t, "001.02", "msg-1", "mndt-1", "SCHEME-1",
		"Bob Merchant", "GB29NWBK60161331926819",
		"Alice Customer", "DE89370400440532013000", "DEUTDEFF",
		"2026-11-01", "2027-11-01")
}

// applyInboundPain009 parses and applies one inbound pain.009
// for the fixture with the given policy, the inbound-apply test
// convention.
func applyInboundPain009(t *testing.T, l *Ledger, doc []byte, policy Pain009Policy, actor string) Pain009ApplyResult {
	t.Helper()
	req, err := ParsePain009(doc)
	if err != nil {
		t.Fatalf("ParsePain009: %v", err)
	}
	res, err := l.ApplyPain009(req, policy, actor)
	if err != nil {
		t.Fatalf("ApplyPain009: %v", err)
	}
	return res
}

// pain009Policy is the standard operator policy for the fixture:
// 50000 cents per debit, at most one debit per month.
func pain009Policy() Pain009Policy {
	return Pain009Policy{MaxAmountCents: 50000, Frequency: MandateFrequencyPerMonth}
}

func TestParsePain009(t *testing.T) {
	req, err := ParsePain009(pain009Doc(t))
	if err != nil {
		t.Fatalf("ParsePain009: %v", err)
	}
	if req.MsgID != "msg-1" || req.MndtID != "mndt-1" {
		t.Errorf("ids = %q/%q, want msg-1/mndt-1", req.MsgID, req.MndtID)
	}
	if req.CreditorSchemeID != "SCHEME-1" {
		t.Errorf("scheme = %q, want SCHEME-1", req.CreditorSchemeID)
	}
	if req.DebtorIBAN != "DE89370400440532013000" || req.CreditorIBAN != "GB29NWBK60161331926819" {
		t.Errorf("ibans = %q/%q, want DE89…/GB29…", req.DebtorIBAN, req.CreditorIBAN)
	}
	if req.DebtorName != "Alice Customer" || req.CreditorName != "Bob Merchant" {
		t.Errorf("names = %q/%q, want Alice Customer/Bob Merchant", req.DebtorName, req.CreditorName)
	}
	if req.DebtorAgentBIC != "DEUTDEFF" {
		t.Errorf("debtor agent BIC = %q, want DEUTDEFF", req.DebtorAgentBIC)
	}
	wantFrom := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	wantUntil := time.Date(2027, 11, 1, 0, 0, 0, 0, time.UTC)
	if !req.ValidFrom.Equal(wantFrom) || !req.ValidUntil.Equal(wantUntil) {
		t.Errorf("window = %v..%v, want %v..%v", req.ValidFrom, req.ValidUntil, wantFrom, wantUntil)
	}
}

func TestParsePain009OrgIDScheme(t *testing.T) {
	// CdtrSchmeId/Id/OrgId/Othr/Id is the other SEPA shape for the
	// creditor scheme identifier: it parses to the same scheme.
	doc := []byte(`<?xml version="1.0" encoding="UTF-8"?>` +
		`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pain.009.001.02">` +
		`<MndtInitnReq><GrpHdr><MsgId>msg-org</MsgId><CreDtTm>2026-11-15T09:00:00Z</CreDtTm></GrpHdr>` +
		`<Mndt><MndtId>mndt-org</MndtId>` +
		`<CdtrSchmeId><Id><OrgId><Othr><Id>SCHEME-ORG</Id></Othr></OrgId></Id></CdtrSchmeId>` +
		`<Cdtr><Nm>Bob Merchant</Nm></Cdtr>` +
		`<CdtrAcct><Id><IBAN>GB29NWBK60161331926819</IBAN></Id></CdtrAcct>` +
		`<Dbtr><Nm>Alice Customer</Nm></Dbtr>` +
		`<DbtrAcct><Id><IBAN>DE89370400440532013000</IBAN></Id></DbtrAcct>` +
		`<ToDt>2027-11-01</ToDt></Mndt></MndtInitnReq></Document>`)
	req, err := ParsePain009(doc)
	if err != nil {
		t.Fatalf("ParsePain009: %v", err)
	}
	if req.CreditorSchemeID != "SCHEME-ORG" {
		t.Errorf("scheme = %q, want SCHEME-ORG", req.CreditorSchemeID)
	}
	if !req.ValidFrom.IsZero() {
		t.Errorf("valid_from = %v, want zero (FrDt omitted)", req.ValidFrom)
	}
}

func TestParsePain009Rejects(t *testing.T) {
	cases := []struct {
		name string
		doc  []byte
	}{
		{"empty document", []byte("  ")},
		{"malformed XML", []byte("<Document><MndtInitnReq>")},
		{"wrong version", pain009InboundDoc(t, "001.01", "msg-r", "mndt-r", "SCHEME-1",
			"Bob Merchant", "GB29NWBK60161331926819",
			"Alice Customer", "DE89370400440532013000", "DEUTDEFF",
			"2026-11-01", "2027-11-01")},
		{"missing MsgId", pain009InboundDoc(t, "001.02", "", "mndt-r", "SCHEME-1",
			"Bob Merchant", "GB29NWBK60161331926819",
			"Alice Customer", "DE89370400440532013000", "DEUTDEFF",
			"2026-11-01", "2027-11-01")},
		{"missing MndtId", pain009InboundDoc(t, "001.02", "msg-r", "", "SCHEME-1",
			"Bob Merchant", "GB29NWBK60161331926819",
			"Alice Customer", "DE89370400440532013000", "DEUTDEFF",
			"2026-11-01", "2027-11-01")},
		{"missing CdtrSchmeId", pain009InboundDoc(t, "001.02", "msg-r", "mndt-r", "",
			"Bob Merchant", "GB29NWBK60161331926819",
			"Alice Customer", "DE89370400440532013000", "DEUTDEFF",
			"2026-11-01", "2027-11-01")},
		{"missing ToDt", pain009InboundDoc(t, "001.02", "msg-r", "mndt-r", "SCHEME-1",
			"Bob Merchant", "GB29NWBK60161331926819",
			"Alice Customer", "DE89370400440532013000", "DEUTDEFF",
			"2026-11-01", "")},
		{"malformed ToDt", pain009InboundDoc(t, "001.02", "msg-r", "mndt-r", "SCHEME-1",
			"Bob Merchant", "GB29NWBK60161331926819",
			"Alice Customer", "DE89370400440532013000", "DEUTDEFF",
			"2026-11-01", "01/11/2027")},
		{"malformed FrDt", pain009InboundDoc(t, "001.02", "msg-r", "mndt-r", "SCHEME-1",
			"Bob Merchant", "GB29NWBK60161331926819",
			"Alice Customer", "DE89370400440532013000", "DEUTDEFF",
			"Nov 2026", "2027-11-01")},
		{"missing debtor IBAN", pain009InboundDoc(t, "001.02", "msg-r", "mndt-r", "SCHEME-1",
			"Bob Merchant", "GB29NWBK60161331926819",
			"Alice Customer", "", "DEUTDEFF",
			"2026-11-01", "2027-11-01")},
		{"missing creditor IBAN", pain009InboundDoc(t, "001.02", "msg-r", "mndt-r", "SCHEME-1",
			"Bob Merchant", "",
			"Alice Customer", "DE89370400440532013000", "DEUTDEFF",
			"2026-11-01", "2027-11-01")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParsePain009(tc.doc); err == nil {
				t.Fatal("ParsePain009: got nil error, want a refusal")
			} else if !errors.Is(err, ErrInvalidPain009) {
				t.Errorf("error = %v, want ErrInvalidPain009", err)
			}
		})
	}
}

func TestApplyPain009(t *testing.T) {
	l, al := pain009Fixture(t)
	versionBefore := l.Version()
	res := applyInboundPain009(t, l, pain009Doc(t), pain009Policy(), "ops")
	if res.Duplicate {
		t.Error("duplicate = true on first apply, want false")
	}
	m := res.Mandate
	if m.ID != "mndt-1" || m.Payer != "alice" || m.Payee != "bob" {
		t.Errorf("mandate = %+v, want mndt-1 alice->bob", m)
	}
	if m.MaxAmountCents != 50000 || m.Frequency != MandateFrequencyPerMonth {
		t.Errorf("mandate terms = %d/%s, want 50000/per-month", m.MaxAmountCents, m.Frequency)
	}
	// The wire carries no currency: the mandate is booked in the
	// ledger's default currency.
	if m.Currency != DefaultCurrency {
		t.Errorf("currency = %q, want default %q", m.Currency, DefaultCurrency)
	}
	if m.Status != MandateStatusActive {
		t.Errorf("status = %q, want active", m.Status)
	}
	wantFrom := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	wantUntil := time.Date(2027, 11, 1, 0, 0, 0, 0, time.UTC)
	if !m.ValidFrom.Equal(wantFrom) || !m.ValidUntil.Equal(wantUntil) {
		t.Errorf("window = %v..%v, want %v..%v", m.ValidFrom, m.ValidUntil, wantFrom, wantUntil)
	}
	// Registration only: no journal rows, no version bump.
	if l.Version() != versionBefore {
		t.Errorf("version = %d, want %d (no bump)", l.Version(), versionBefore)
	}
	if len(l.Entries()) != 0 {
		t.Errorf("entries = %d, want 0", len(l.Entries()))
	}
	// The mandate is readable back through the normal read path.
	got, ok := l.GetMandate("mndt-1")
	if !ok || got.Status != MandateStatusActive || got.MaxAmountCents != 50000 {
		t.Errorf("GetMandate = %+v/%v, want active/50000", got, ok)
	}
	// Both audit events are precise: CreateMandate's own, plus
	// the inbound registration's.
	events := readAuditOps(t, al)
	created := findOp(events, "mandate_created")
	if created == nil {
		t.Fatal("no mandate_created audit event")
	}
	if created.Details["max_amount_cents"] != float64(50000) || created.Details["frequency"] != "per-month" {
		t.Errorf("mandate_created details = %+v, want 50000/per-month", created.Details)
	}
	registered := findOp(events, "pain009_mandate_registered")
	if registered == nil {
		t.Fatal("no pain009_mandate_registered audit event")
	}
	if registered.Actor != "ops" || registered.TraceID != "msg-1" {
		t.Errorf("registered event = actor %q trace %q, want ops/msg-1", registered.Actor, registered.TraceID)
	}
	if registered.Details["msg_id"] != "msg-1" || registered.Details["mandate_id"] != "mndt-1" || registered.Details["creditor_scheme_id"] != "SCHEME-1" {
		t.Errorf("registered details = %+v, want msg-1/mndt-1/SCHEME-1", registered.Details)
	}
}

func TestApplyPain009DuplicateAndConflict(t *testing.T) {
	l, al := pain009Fixture(t)
	first := applyInboundPain009(t, l, pain009Doc(t), pain009Policy(), "ops")
	second := applyInboundPain009(t, l, pain009Doc(t), pain009Policy(), "ops")
	if !second.Duplicate {
		t.Error("duplicate = false on replay, want true")
	}
	if second.Mandate.ID != first.Mandate.ID || !second.Mandate.CreatedAt.Equal(first.Mandate.CreatedAt) {
		t.Errorf("replay mandate = %+v, want %+v", second.Mandate, first.Mandate)
	}
	if countOp(readAuditOps(t, al), "mandate_created") != 1 {
		t.Error("replay emitted a second mandate_created event")
	}
	if countOp(readAuditOps(t, al), "pain009_mandate_registered") != 1 {
		t.Error("replay emitted a second pain009_mandate_registered event")
	}
	// The same MndtId with a different cap is a conflict, never a
	// silent overwrite.
	req, err := ParsePain009(pain009Doc(t))
	if err != nil {
		t.Fatalf("ParsePain009: %v", err)
	}
	if _, err := l.ApplyPain009(req, Pain009Policy{MaxAmountCents: 60000, Frequency: MandateFrequencyPerMonth}, "ops"); !errors.Is(err, ErrMandateIDConflict) {
		t.Errorf("ApplyPain009 different cap = %v, want ErrMandateIDConflict", err)
	}
	// A different window is a conflict too.
	otherDoc := pain009InboundDoc(t, "001.02", "msg-2", "mndt-1", "SCHEME-1",
		"Bob Merchant", "GB29NWBK60161331926819",
		"Alice Customer", "DE89370400440532013000", "DEUTDEFF",
		"2026-12-01", "2027-12-01")
	other, err := ParsePain009(otherDoc)
	if err != nil {
		t.Fatalf("ParsePain009: %v", err)
	}
	if _, err := l.ApplyPain009(other, pain009Policy(), "ops"); !errors.Is(err, ErrMandateIDConflict) {
		t.Errorf("ApplyPain009 different window = %v, want ErrMandateIDConflict", err)
	}
	// The stored mandate is untouched by both conflicts.
	got, ok := l.GetMandate("mndt-1")
	if !ok || got.MaxAmountCents != 50000 {
		t.Errorf("GetMandate after conflicts = %+v/%v, want 50000", got, ok)
	}
}

func TestApplyPain009UnknownAccount(t *testing.T) {
	l, _ := pain009Fixture(t)
	// An unknown debtor IBAN: a valid IBAN the ledger never
	// registered.
	doc := pain009InboundDoc(t, "001.02", "msg-unk-1", "mndt-unk-1", "SCHEME-1",
		"Bob Merchant", "GB29NWBK60161331926819",
		"Alice Customer", "FR1420041010050500013M02606", "DEUTDEFF",
		"2026-11-01", "2027-11-01")
	req, err := ParsePain009(doc)
	if err != nil {
		t.Fatalf("ParsePain009: %v", err)
	}
	res, err := l.ApplyPain009(req, pain009Policy(), "ops")
	if !errors.Is(err, ErrPain009UnknownAccount) {
		t.Fatalf("ApplyPain009 = %v, want ErrPain009UnknownAccount", err)
	}
	if res.RepairID == "" {
		t.Error("repair_id is empty on the parked unknown-debtor path")
	}
	if _, ok := l.GetMandate("mndt-unk-1"); ok {
		t.Error("unknown-debtor request registered a mandate")
	}
	// An unknown creditor IBAN, the same way.
	doc2 := pain009InboundDoc(t, "001.02", "msg-unk-2", "mndt-unk-2", "SCHEME-1",
		"Bob Merchant", "FR1420041010050500013M02606",
		"Alice Customer", "DE89370400440532013000", "DEUTDEFF",
		"2026-11-01", "2027-11-01")
	req2, err := ParsePain009(doc2)
	if err != nil {
		t.Fatalf("ParsePain009: %v", err)
	}
	res2, err := l.ApplyPain009(req2, pain009Policy(), "ops")
	if !errors.Is(err, ErrPain009UnknownAccount) || res2.RepairID == "" {
		t.Errorf("ApplyPain009 unknown creditor = res %+v err %v, want unknown-account + repair_id", res2, err)
	}
}

func TestApplyPain009PolicyRejects(t *testing.T) {
	l, _ := pain009Fixture(t)
	req, err := ParsePain009(pain009Doc(t))
	if err != nil {
		t.Fatalf("ParsePain009: %v", err)
	}
	cases := []struct {
		name   string
		policy Pain009Policy
		want   error
	}{
		{"zero cap", Pain009Policy{MaxAmountCents: 0, Frequency: MandateFrequencyPerMonth}, ErrMandateNonPositiveMaxAmount},
		{"negative cap", Pain009Policy{MaxAmountCents: -100, Frequency: MandateFrequencyPerMonth}, ErrMandateNonPositiveMaxAmount},
		{"bad frequency", Pain009Policy{MaxAmountCents: 50000, Frequency: "per-hour"}, ErrInvalidMandateFrequency},
		{"empty frequency", Pain009Policy{MaxAmountCents: 50000, Frequency: ""}, ErrInvalidMandateFrequency},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := l.ApplyPain009(req, tc.policy, "ops"); !errors.Is(err, tc.want) {
				t.Errorf("ApplyPain009 = %v, want %v", err, tc.want)
			}
		})
	}
	if _, ok := l.GetMandate("mndt-1"); ok {
		t.Error("a policy refusal registered a mandate")
	}
}

func TestApplyPain009WindowAndSamePartyRejects(t *testing.T) {
	l, _ := pain009Fixture(t)
	// FrDt at or after ToDt cannot register: an authorization
	// without a forward window.
	closedDoc := pain009InboundDoc(t, "001.02", "msg-win", "mndt-win", "SCHEME-1",
		"Bob Merchant", "GB29NWBK60161331926819",
		"Alice Customer", "DE89370400440532013000", "DEUTDEFF",
		"2027-11-01", "2027-11-01")
	closed, err := ParsePain009(closedDoc)
	if err != nil {
		t.Fatalf("ParsePain009: %v", err)
	}
	if _, err := l.ApplyPain009(closed, pain009Policy(), "ops"); !errors.Is(err, ErrMandateInvalidWindow) {
		t.Errorf("ApplyPain009 closed window = %v, want ErrMandateInvalidWindow", err)
	}
	// Debtor and creditor IBANs naming the same account cannot
	// register: a direct debit pulls from someone else.
	sameDoc := pain009InboundDoc(t, "001.02", "msg-same", "mndt-same", "SCHEME-1",
		"Alice Customer", "DE89370400440532013000",
		"Alice Customer", "DE89370400440532013000", "DEUTDEFF",
		"2026-11-01", "2027-11-01")
	same, err := ParsePain009(sameDoc)
	if err != nil {
		t.Fatalf("ParsePain009: %v", err)
	}
	if _, err := l.ApplyPain009(same, pain009Policy(), "ops"); !errors.Is(err, ErrMandateSameParty) {
		t.Errorf("ApplyPain009 same party = %v, want ErrMandateSameParty", err)
	}
}

func TestParkPain009Unparsed(t *testing.T) {
	l, _ := pain009Fixture(t)
	// A wrong-version document that still names its MsgId parks.
	doc := pain009InboundDoc(t, "001.01", "msg-park-1", "mndt-park-1", "SCHEME-1",
		"Bob Merchant", "GB29NWBK60161331926819",
		"Alice Customer", "DE89370400440532013000", "DEUTDEFF",
		"2026-11-01", "2027-11-01")
	if _, err := ParsePain009(doc); err == nil {
		t.Fatal("ParsePain009 accepted a 001.01 document")
	}
	id, err := l.ParkPain009Unparsed(doc, "ops")
	if err != nil {
		t.Fatalf("ParkPain009Unparsed: %v", err)
	}
	if id == "" {
		t.Error("repair_id is empty for a named unparsed document")
	}
	// A document naming only its MndtId parks under it.
	noMsgDoc := []byte(`<?xml version="1.0" encoding="UTF-8"?>` +
		`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pain.009.001.01">` +
		`<MndtInitnReq><Mndt><MndtId>mndt-park-2</MndtId></Mndt></MndtInitnReq></Document>`)
	id, err = l.ParkPain009Unparsed(noMsgDoc, "ops")
	if err != nil {
		t.Fatalf("ParkPain009Unparsed mndt-only: %v", err)
	}
	if id == "" {
		t.Error("repair_id is empty for a MndtId-only document")
	}
	// Garbage names nothing: nothing to park.
	id, err = l.ParkPain009Unparsed([]byte("not xml at all"), "ops")
	if err != nil {
		t.Fatalf("ParkPain009Unparsed garbage: %v", err)
	}
	if id != "" {
		t.Errorf("repair_id = %q for garbage, want empty", id)
	}
}
