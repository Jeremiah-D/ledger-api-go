package ledger

import (
	"errors"
	"strings"
	"testing"
)

// pain011Doc renders one inbound pain.011.001.03 document of the
// given schema version: msgID cancelling mandate mndtID under
// schemeID, with the optional reason — the shape ExportPain011
// renders, so a document this ledger's export renders parses
// back. An empty reason omits the CxlRsn block entirely.
func pain011Doc(version, msgID, mndtID, schemeID, reason string) string {
	var sb strings.Builder
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pain.011.` + version + `">`)
	sb.WriteString(`<MndtCxlReq><GrpHdr><MsgId>` + msgID + `</MsgId>`)
	sb.WriteString(`<CreDtTm>2026-11-15T09:00:00Z</CreDtTm></GrpHdr>`)
	sb.WriteString(`<Mndt><OrgnlMndt><MndtId>` + mndtID + `</MndtId>`)
	sb.WriteString(`<CdtrSchmeId><Id><PrtryId><Id>` + schemeID + `</Id></PrtryId></Id></CdtrSchmeId>`)
	sb.WriteString(`</OrgnlMndt>`)
	if reason != "" {
		sb.WriteString(`<CxlRsn><Rsn><Prtry>` + reason + `</Prtry></Rsn></CxlRsn>`)
	}
	sb.WriteString(`</Mndt></MndtCxlReq></Document>`)
	return sb.String()
}

// applyPain011Doc parses doc and applies it, failing the test on
// parse errors: the gates under test are the apply gates, not the
// parse gates.
func applyPain011Doc(t *testing.T, l *Ledger, doc string) (Pain011ApplyResult, error) {
	t.Helper()
	req, err := ParsePain011([]byte(doc))
	if err != nil {
		t.Fatalf("ParsePain011: %v", err)
	}
	return l.ApplyPain011(req, "creditor-bank")
}

func TestParsePain011ValidDocument(t *testing.T) {
	req, err := ParsePain011([]byte(pain011Doc("001.03", "msg-1", "m-008", "DE98ZZZ09999999999",
		"Debtor requested closure: account closed at debtor request")))
	if err != nil {
		t.Fatalf("ParsePain011: %v", err)
	}
	if req.MsgID != "msg-1" || req.MndtID != "m-008" || req.CreditorSchemeID != "DE98ZZZ09999999999" {
		t.Errorf("req = %+v, want msg-1/m-008/DE98ZZZ09999999999", req)
	}
	if req.Reason != "Debtor requested closure: account closed at debtor request" {
		t.Errorf("reason = %q, want the counterparty text verbatim", req.Reason)
	}
	// No reason: the request carries none, and that is not a
	// refusal.
	req, err = ParsePain011([]byte(pain011Doc("001.03", "msg-2", "m-008", "DE98ZZZ09999999999", "")))
	if err != nil {
		t.Fatalf("ParsePain011 no-reason: %v", err)
	}
	if req.Reason != "" {
		t.Errorf("reason = %q, want empty", req.Reason)
	}
}

func TestParsePain011RoundTripsExport(t *testing.T) {
	day := pain008Day()
	l, m := pain008Fixture(t, MandateFrequencyPerMonth, day, day.AddDate(1, 0, 0))
	doc, err := l.ExportPain011(m.ID, "DE98ZZZ09999999999", "Debtor requested closure")
	if err != nil {
		t.Fatalf("ExportPain011: %v", err)
	}
	req, err := ParsePain011(doc)
	if err != nil {
		t.Fatalf("ParsePain011(export): %v", err)
	}
	if req.MsgID != pain011MsgID(m.ID, "DE98ZZZ09999999999") || req.MndtID != "m-008" {
		t.Errorf("req = %+v, want msg %q / m-008", req, pain011MsgID(m.ID, "DE98ZZZ09999999999"))
	}
	if req.CreditorSchemeID != "DE98ZZZ09999999999" || req.Reason != "Debtor requested closure" {
		t.Errorf("scheme/reason = %q/%q, want DE98ZZZ09999999999/Debtor requested closure", req.CreditorSchemeID, req.Reason)
	}
	// A reasonless export parses back reasonless too.
	doc, err = l.ExportPain011(m.ID, "DE98ZZZ09999999999", "")
	if err != nil {
		t.Fatalf("ExportPain011 no-reason: %v", err)
	}
	req, err = ParsePain011(doc)
	if err != nil {
		t.Fatalf("ParsePain011(export, no-reason): %v", err)
	}
	if req.Reason != "" {
		t.Errorf("reason = %q, want empty", req.Reason)
	}
}

func TestParsePain011RejectsBadDocuments(t *testing.T) {
	// Wrong schema version: the namespace gate refuses it.
	doc := pain011Doc("001.01", "msg-1", "m-008", "DE98ZZZ09999999999", "")
	if _, err := ParsePain011([]byte(doc)); !errors.Is(err, ErrInvalidPain011) {
		t.Errorf("wrong version: err = %v, want ErrInvalidPain011", err)
	}
	// Missing MsgId, MndtId, scheme: each is a parse refusal.
	for _, tc := range []struct {
		name string
		doc  string
	}{
		{"missing MsgId", pain011Doc("001.03", "", "m-008", "DE98ZZZ09999999999", "")},
		{"missing MndtId", pain011Doc("001.03", "msg-1", "", "DE98ZZZ09999999999", "")},
		{"missing scheme", pain011Doc("001.03", "msg-1", "m-008", "", "")},
	} {
		if _, err := ParsePain011([]byte(tc.doc)); !errors.Is(err, ErrInvalidPain011) {
			t.Errorf("%s: err = %v, want ErrInvalidPain011", tc.name, err)
		}
	}
	// Malformed XML and an empty document are refusals too.
	if _, err := ParsePain011([]byte("<Document><MndtCxlReq>")); !errors.Is(err, ErrInvalidPain011) {
		t.Errorf("malformed: err = %v, want ErrInvalidPain011", err)
	}
	if _, err := ParsePain011([]byte("  ")); !errors.Is(err, ErrInvalidPain011) {
		t.Errorf("empty: err = %v, want ErrInvalidPain011", err)
	}
}

func TestApplyPain011UnknownMandate(t *testing.T) {
	al := mustAuditLog(t)
	l, _ := pain010InboundFixture(t, al)
	_, err := applyPain011Doc(t, l, pain011Doc("001.03", "msg-1", "no-such", "DE98ZZZ09999999999", ""))
	if !errors.Is(err, ErrMandateNotFound) {
		t.Errorf("err = %v, want ErrMandateNotFound", err)
	}
	// No cancellation is recorded for a mandate that does not
	// exist (the fixture's own setup events are untouched).
	events := readAuditOps(t, al)
	if countOp(events, "pain011_cancellation_applied") != 0 || countOp(events, "mandate_cancelled") != 0 {
		t.Errorf("cancellation events = %d/%d, want 0/0",
			countOp(events, "pain011_cancellation_applied"), countOp(events, "mandate_cancelled"))
	}
}

func TestApplyPain011ActiveCancel(t *testing.T) {
	al := mustAuditLog(t)
	l, _ := pain010InboundFixture(t, al)
	before := l.Version()
	reason := "Debtor requested closure: account closed at debtor request"

	res, err := applyPain011Doc(t, l, pain011Doc("001.03", "msg-1", "m-008", "DE98ZZZ09999999999", reason))
	if err != nil {
		t.Fatalf("ApplyPain011: %v", err)
	}
	if res.Duplicate {
		t.Error("duplicate = true on first apply")
	}
	if res.Mandate.Status != MandateStatusCancelled {
		t.Errorf("status = %q, want cancelled", res.Mandate.Status)
	}
	if m, ok := l.GetMandate("m-008"); !ok || m.Status != MandateStatusCancelled {
		t.Errorf("stored mandate = %+v, want cancelled", m)
	}
	// Cancelling is structural: no version bump.
	if l.Version() != before {
		t.Errorf("version = %d, want %d (structural, no bump)", l.Version(), before)
	}
	// Audit: the entry's event plus the inbound marker, each
	// exactly once, with the exact details — the reason verbatim.
	events := readAuditOps(t, al)
	if countOp(events, "mandate_cancelled") != 1 {
		t.Errorf("mandate_cancelled count = %d, want 1", countOp(events, "mandate_cancelled"))
	}
	if countOp(events, "pain011_cancellation_applied") != 1 {
		t.Errorf("pain011_cancellation_applied count = %d, want 1", countOp(events, "pain011_cancellation_applied"))
	}
	ev := findOp(events, "pain011_cancellation_applied")
	if ev == nil {
		t.Fatal("pain011_cancellation_applied event missing")
	}
	if ev.TraceID != "msg-1" {
		t.Errorf("trace = %q, want msg-1", ev.TraceID)
	}
	if ev.Details["msg_id"] != "msg-1" || ev.Details["mandate_id"] != "m-008" ||
		ev.Details["creditor_scheme_id"] != "DE98ZZZ09999999999" {
		t.Errorf("details = %v, want msg-1/m-008/DE98ZZZ09999999999", ev.Details)
	}
	if ev.Details["reason_code"] != reason {
		t.Errorf("reason_code = %v, want %q verbatim", ev.Details["reason_code"], reason)
	}
}

func TestApplyPain011SuspendedCancellable(t *testing.T) {
	al := mustAuditLog(t)
	l, m := pain010InboundFixture(t, al)
	// Suspended: the cancellation gate admits it — a suspended
	// mandate still exists at the bank. This is the distinction
	// from the pain.010 amendment gate, which refuses suspended.
	if _, err := l.SuspendMandate(m.ID); err != nil {
		t.Fatalf("SuspendMandate: %v", err)
	}
	res, err := applyPain011Doc(t, l, pain011Doc("001.03", "msg-1", "m-008", "DE98ZZZ09999999999", ""))
	if err != nil {
		t.Fatalf("ApplyPain011 suspended: %v", err)
	}
	if res.Mandate.Status != MandateStatusCancelled {
		t.Errorf("status = %q, want cancelled", res.Mandate.Status)
	}
	events := readAuditOps(t, al)
	if countOp(events, "mandate_cancelled") != 1 || countOp(events, "pain011_cancellation_applied") != 1 {
		t.Errorf("cancelled/applied events = %d/%d, want 1/1",
			countOp(events, "mandate_cancelled"), countOp(events, "pain011_cancellation_applied"))
	}
}

func TestApplyPain011ReplayIdempotent(t *testing.T) {
	al := mustAuditLog(t)
	l, _ := pain010InboundFixture(t, al)
	doc := pain011Doc("001.03", "msg-1", "m-008", "DE98ZZZ09999999999", "Debtor requested closure")
	if _, err := applyPain011Doc(t, l, doc); err != nil {
		t.Fatalf("ApplyPain011: %v", err)
	}
	// Same MsgId, same terms: an idempotent no-op returning the
	// current (cancelled) mandate, with no second audit event.
	res, err := applyPain011Doc(t, l, doc)
	if err != nil {
		t.Fatalf("ApplyPain011 replay: %v", err)
	}
	if !res.Duplicate {
		t.Error("duplicate = false on replay, want true")
	}
	if res.Mandate.Status != MandateStatusCancelled {
		t.Errorf("status = %q, want cancelled", res.Mandate.Status)
	}
	events := readAuditOps(t, al)
	if countOp(events, "mandate_cancelled") != 1 || countOp(events, "pain011_cancellation_applied") != 1 {
		t.Errorf("cancelled/applied events = %d/%d, want 1/1 after replay",
			countOp(events, "mandate_cancelled"), countOp(events, "pain011_cancellation_applied"))
	}
	// Same MsgId, different terms: a conflict, never a silent
	// overwrite.
	conflict := pain011Doc("001.03", "msg-1", "m-008", "DE98ZZZ09999999999", "A different reason")
	if _, err := applyPain011Doc(t, l, conflict); !errors.Is(err, ErrPain011MsgIDConflict) {
		t.Errorf("conflict: err = %v, want ErrPain011MsgIDConflict", err)
	}
	// A new MsgId on the already-cancelled mandate is refused
	// by CancelMandate's own gate.
	fresh := pain011Doc("001.03", "msg-2", "m-008", "DE98ZZZ09999999999", "")
	if _, err := applyPain011Doc(t, l, fresh); !errors.Is(err, ErrMandateNotActive) {
		t.Errorf("fresh MsgId on cancelled: err = %v, want ErrMandateNotActive", err)
	}
}

func TestApplyPain011ExpiredRejected(t *testing.T) {
	day := pain008Day()
	l := New()
	seedBalance(t, l, "cust", 1_000_000)
	mustSetIBAN(t, l, "cust", testIBANDE, "")
	mustSetIBAN(t, l, "merchant", testIBANGB, "")
	if _, err := l.CreateMandate(Mandate{
		ID: "m-old", Payer: "cust", Payee: "merchant",
		MaxAmountCents: 5000, Currency: "USD", Frequency: MandateFrequencyPerMonth,
		ValidFrom: day.AddDate(-1, 0, 0), ValidUntil: day.AddDate(0, -1, 0),
	}); err != nil {
		t.Fatalf("CreateMandate expired: %v", err)
	}
	_, err := applyPain011Doc(t, l, pain011Doc("001.03", "msg-1", "m-old", "DE98ZZZ09999999999", ""))
	if !errors.Is(err, ErrMandateExpired) {
		t.Errorf("err = %v, want ErrMandateExpired", err)
	}
}

func TestApplyPain011NoReasonHasNoReasonField(t *testing.T) {
	al := mustAuditLog(t)
	l, _ := pain010InboundFixture(t, al)
	res, err := applyPain011Doc(t, l, pain011Doc("001.03", "msg-1", "m-008", "DE98ZZZ09999999999", ""))
	if err != nil {
		t.Fatalf("ApplyPain011: %v", err)
	}
	if res.Mandate.Status != MandateStatusCancelled {
		t.Errorf("status = %q, want cancelled", res.Mandate.Status)
	}
	ev := findOp(readAuditOps(t, al), "pain011_cancellation_applied")
	if ev == nil {
		t.Fatal("pain011_cancellation_applied event missing")
	}
	if _, ok := ev.Details["reason_code"]; ok {
		t.Errorf("reason_code present = %v, want no reason field when no reason was given", ev.Details["reason_code"])
	}
	if ev.Details["msg_id"] != "msg-1" || ev.Details["mandate_id"] != "m-008" ||
		ev.Details["creditor_scheme_id"] != "DE98ZZZ09999999999" {
		t.Errorf("details = %v, want msg-1/m-008/DE98ZZZ09999999999", ev.Details)
	}
	// A hand-built request with no reason applies the same way:
	// the reason is optional at the apply gate too.
	l2 := New()
	seedBalance(t, l2, "cust", 1_000_000)
	mustSetIBAN(t, l2, "cust", testIBANDE, "")
	mustSetIBAN(t, l2, "merchant", testIBANGB, "")
	day := pain008Day()
	if _, err := l2.CreateMandate(Mandate{
		ID: "m-009", Payer: "cust", Payee: "merchant",
		MaxAmountCents: 5000, Currency: "USD", Frequency: MandateFrequencyPerMonth,
		ValidFrom: day, ValidUntil: day.AddDate(1, 0, 0),
	}); err != nil {
		t.Fatalf("CreateMandate: %v", err)
	}
	res, err = l2.ApplyPain011(Pain011Request{
		MsgID: "msg-hand", MndtID: "m-009", CreditorSchemeID: "DE98ZZZ09999999999",
	}, "creditor-bank")
	if err != nil {
		t.Fatalf("ApplyPain011 hand-built: %v", err)
	}
	if res.Mandate.Status != MandateStatusCancelled {
		t.Errorf("hand-built status = %q, want cancelled", res.Mandate.Status)
	}
	// ... but a hand-built request missing the mandate ID is
	// refused before anything moves.
	if _, err := l2.ApplyPain011(Pain011Request{
		MsgID: "msg-hand-2", CreditorSchemeID: "DE98ZZZ09999999999",
	}, "creditor-bank"); !errors.Is(err, ErrInvalidPain011) {
		t.Errorf("missing MndtId: err = %v, want ErrInvalidPain011", err)
	}
}
