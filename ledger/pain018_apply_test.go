package ledger

import (
	"errors"
	"strings"
	"testing"
)

// pain018Doc renders one inbound pain.018.001.03 document of the
// given schema version: msgID suspending mandate mndtID under
// schemeID, with the reason — the shape ExportPain018 renders,
// so a document this ledger's export renders parses back. An
// empty reason omits the SspnsnRsn block entirely; an empty
// mandate ID omits the MndtId element entirely.
func pain018Doc(version, msgID, mndtID, schemeID, reason string) string {
	var sb strings.Builder
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pain.018.` + version + `">`)
	sb.WriteString(`<MndtSspnsnReq><GrpHdr><MsgId>` + msgID + `</MsgId>`)
	sb.WriteString(`<CreDtTm>2026-11-15T09:00:00Z</CreDtTm></GrpHdr>`)
	sb.WriteString(`<SspnsnReqId>` + msgID + `</SspnsnReqId>`)
	sb.WriteString(`<OrgnlMndt>`)
	if mndtID != "" {
		sb.WriteString(`<MndtId>` + mndtID + `</MndtId>`)
	}
	sb.WriteString(`<CdtrSchmeId><Id><PrtryId><Id>` + schemeID + `</Id></PrtryId></Id></CdtrSchmeId>`)
	sb.WriteString(`</OrgnlMndt>`)
	if reason != "" {
		sb.WriteString(`<SspnsnRsn><Rsn><Prtry>` + reason + `</Prtry></Rsn></SspnsnRsn>`)
	}
	sb.WriteString(`</MndtSspnsnReq></Document>`)
	return sb.String()
}

// applyPain018Doc parses doc and applies it, failing the test on
// parse errors: the gates under test are the apply gates, not the
// parse gates.
func applyPain018Doc(t *testing.T, l *Ledger, doc string) (Pain018ApplyResult, error) {
	t.Helper()
	req, err := ParsePain018([]byte(doc))
	if err != nil {
		t.Fatalf("ParsePain018: %v", err)
	}
	return l.ApplyPain018(req, "creditor-bank")
}

func TestParsePain018_RoundTrip(t *testing.T) {
	day := pain008Day()
	l, m := pain008Fixture(t, MandateFrequencyPerMonth, day, day.AddDate(1, 0, 0))
	doc, err := l.ExportPain018(m.ID, "DE98ZZZ09999999999", "Debtor requested pause")
	if err != nil {
		t.Fatalf("ExportPain018: %v", err)
	}
	req, err := ParsePain018(doc)
	if err != nil {
		t.Fatalf("ParsePain018(export): %v", err)
	}
	if req.MsgID != pain018MsgID(m.ID, "DE98ZZZ09999999999") || req.MndtID != "m-008" {
		t.Errorf("req = %+v, want msg %q / m-008", req, pain018MsgID(m.ID, "DE98ZZZ09999999999"))
	}
	if req.CreditorSchemeID != "DE98ZZZ09999999999" || req.Reason != "Debtor requested pause" {
		t.Errorf("scheme/reason = %q/%q, want DE98ZZZ09999999999/Debtor requested pause", req.CreditorSchemeID, req.Reason)
	}
}

func TestParsePain018_RejectsMissingReason(t *testing.T) {
	// No SspnsnRsn block at all: SspnsnRsn is [1..1], so the
	// request is refused, never defaulted.
	doc := pain018Doc("001.03", "msg-1", "m-008", "DE98ZZZ09999999999", "")
	if _, err := ParsePain018([]byte(doc)); !errors.Is(err, ErrInvalidPain018) {
		t.Errorf("missing reason: err = %v, want ErrInvalidPain018", err)
	}
	// An empty reason block is the same refusal.
	doc = pain018Doc("001.03", "msg-1", "m-008", "DE98ZZZ09999999999", "  ")
	if _, err := ParsePain018([]byte(doc)); !errors.Is(err, ErrInvalidPain018) {
		t.Errorf("blank reason: err = %v, want ErrInvalidPain018", err)
	}
	// Wrong schema version: the namespace gate refuses it.
	doc = pain018Doc("001.01", "msg-1", "m-008", "DE98ZZZ09999999999", "Debtor requested pause")
	if _, err := ParsePain018([]byte(doc)); !errors.Is(err, ErrInvalidPain018) {
		t.Errorf("wrong version: err = %v, want ErrInvalidPain018", err)
	}
	// Malformed XML and an empty document are refusals too.
	if _, err := ParsePain018([]byte("<Document><MndtSspnsnReq>")); !errors.Is(err, ErrInvalidPain018) {
		t.Errorf("malformed: err = %v, want ErrInvalidPain018", err)
	}
	if _, err := ParsePain018([]byte("  ")); !errors.Is(err, ErrInvalidPain018) {
		t.Errorf("empty: err = %v, want ErrInvalidPain018", err)
	}
}

func TestParsePain018_RejectsMissingMandateID(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
	}{
		{"missing MsgId", pain018Doc("001.03", "", "m-008", "DE98ZZZ09999999999", "Debtor requested pause")},
		{"missing MndtId", pain018Doc("001.03", "msg-1", "", "DE98ZZZ09999999999", "Debtor requested pause")},
		{"missing scheme", pain018Doc("001.03", "msg-1", "m-008", "", "Debtor requested pause")},
	} {
		if _, err := ParsePain018([]byte(tc.doc)); !errors.Is(err, ErrInvalidPain018) {
			t.Errorf("%s: err = %v, want ErrInvalidPain018", tc.name, err)
		}
	}
}

func TestApplyPain018_SuspendsActiveMandate(t *testing.T) {
	al := mustAuditLog(t)
	l, _ := pain010InboundFixture(t, al)
	before := l.Version()
	reason := "Debtor requested pause: account under review"

	res, err := applyPain018Doc(t, l, pain018Doc("001.03", "msg-1", "m-008", "DE98ZZZ09999999999", reason))
	if err != nil {
		t.Fatalf("ApplyPain018: %v", err)
	}
	if res.Duplicate {
		t.Error("duplicate = true on first apply")
	}
	if res.Mandate.Status != MandateStatusSuspended {
		t.Errorf("status = %q, want suspended", res.Mandate.Status)
	}
	if m, ok := l.GetMandate("m-008"); !ok || m.Status != MandateStatusSuspended {
		t.Errorf("stored mandate = %+v, want suspended", m)
	}
	// Suspending is structural: no version bump.
	if l.Version() != before {
		t.Errorf("version = %d, want %d (structural, no bump)", l.Version(), before)
	}
	// Audit: the entry's event plus the inbound marker, each
	// exactly once, with the exact details — the reason verbatim.
	events := readAuditOps(t, al)
	if countOp(events, "mandate_suspended") != 1 {
		t.Errorf("mandate_suspended count = %d, want 1", countOp(events, "mandate_suspended"))
	}
	if countOp(events, "pain018_suspension_applied") != 1 {
		t.Errorf("pain018_suspension_applied count = %d, want 1", countOp(events, "pain018_suspension_applied"))
	}
	ev := findOp(events, "pain018_suspension_applied")
	if ev == nil {
		t.Fatal("pain018_suspension_applied event missing")
	}
	if ev.Actor != "creditor-bank" {
		t.Errorf("actor = %q, want creditor-bank (ApplyPain018)", ev.Actor)
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

func TestApplyPain018_RefusesUnknownMandate(t *testing.T) {
	al := mustAuditLog(t)
	l, _ := pain010InboundFixture(t, al)
	_, err := applyPain018Doc(t, l, pain018Doc("001.03", "msg-1", "no-such", "DE98ZZZ09999999999", "Debtor requested pause"))
	if !errors.Is(err, ErrMandateNotFound) {
		t.Errorf("err = %v, want ErrMandateNotFound", err)
	}
	// No suspension is recorded for a mandate that does not
	// exist (the fixture's own setup events are untouched).
	events := readAuditOps(t, al)
	if countOp(events, "pain018_suspension_applied") != 0 || countOp(events, "mandate_suspended") != 0 {
		t.Errorf("suspension events = %d/%d, want 0/0",
			countOp(events, "pain018_suspension_applied"), countOp(events, "mandate_suspended"))
	}
}

func TestApplyPain018_RefusesSuspendedMandate(t *testing.T) {
	al := mustAuditLog(t)
	l, m := pain010InboundFixture(t, al)
	if _, err := l.SuspendMandate(m.ID); err != nil {
		t.Fatalf("SuspendMandate: %v", err)
	}
	// A new MsgId on the already-suspended mandate is refused
	// by SuspendMandate's own gate — the registry never sees it.
	_, err := applyPain018Doc(t, l, pain018Doc("001.03", "msg-2", "m-008", "DE98ZZZ09999999999", "Debtor requested pause"))
	if !errors.Is(err, ErrMandateNotActive) {
		t.Errorf("err = %v, want ErrMandateNotActive", err)
	}
	events := readAuditOps(t, al)
	if countOp(events, "pain018_suspension_applied") != 0 {
		t.Errorf("pain018_suspension_applied count = %d, want 0", countOp(events, "pain018_suspension_applied"))
	}
}

func TestApplyPain018_RefusesCancelledMandate(t *testing.T) {
	al := mustAuditLog(t)
	l, m := pain010InboundFixture(t, al)
	if _, err := l.CancelMandate(m.ID); err != nil {
		t.Fatalf("CancelMandate: %v", err)
	}
	_, err := applyPain018Doc(t, l, pain018Doc("001.03", "msg-1", "m-008", "DE98ZZZ09999999999", "Debtor requested pause"))
	if !errors.Is(err, ErrMandateNotActive) {
		t.Errorf("err = %v, want ErrMandateNotActive", err)
	}
	events := readAuditOps(t, al)
	if countOp(events, "pain018_suspension_applied") != 0 {
		t.Errorf("pain018_suspension_applied count = %d, want 0", countOp(events, "pain018_suspension_applied"))
	}
}

func TestApplyPain018_RefusesExpiredMandate(t *testing.T) {
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
	_, err := applyPain018Doc(t, l, pain018Doc("001.03", "msg-1", "m-old", "DE98ZZZ09999999999", "Debtor requested pause"))
	if !errors.Is(err, ErrMandateExpired) {
		t.Errorf("err = %v, want ErrMandateExpired", err)
	}
}

func TestApplyPain018_ReplaySameMsgIdNoop(t *testing.T) {
	al := mustAuditLog(t)
	l, _ := pain010InboundFixture(t, al)
	doc := pain018Doc("001.03", "msg-1", "m-008", "DE98ZZZ09999999999", "Debtor requested pause")
	if _, err := applyPain018Doc(t, l, doc); err != nil {
		t.Fatalf("ApplyPain018: %v", err)
	}
	// Same MsgId, same terms: an idempotent no-op returning the
	// current (suspended) mandate, with no second audit event.
	res, err := applyPain018Doc(t, l, doc)
	if err != nil {
		t.Fatalf("ApplyPain018 replay: %v", err)
	}
	if !res.Duplicate {
		t.Error("duplicate = false on replay, want true")
	}
	if res.Mandate.Status != MandateStatusSuspended {
		t.Errorf("status = %q, want suspended", res.Mandate.Status)
	}
	events := readAuditOps(t, al)
	if countOp(events, "mandate_suspended") != 1 || countOp(events, "pain018_suspension_applied") != 1 {
		t.Errorf("suspended/applied events = %d/%d, want 1/1 after replay",
			countOp(events, "mandate_suspended"), countOp(events, "pain018_suspension_applied"))
	}
}

func TestApplyPain018_SameMsgIdDifferentTermsRefused(t *testing.T) {
	al := mustAuditLog(t)
	l, _ := pain010InboundFixture(t, al)
	doc := pain018Doc("001.03", "msg-1", "m-008", "DE98ZZZ09999999999", "Debtor requested pause")
	if _, err := applyPain018Doc(t, l, doc); err != nil {
		t.Fatalf("ApplyPain018: %v", err)
	}
	// Same MsgId, different reason: a conflict, never a silent
	// overwrite.
	conflict := pain018Doc("001.03", "msg-1", "m-008", "DE98ZZZ09999999999", "A different reason")
	if _, err := applyPain018Doc(t, l, conflict); !errors.Is(err, ErrPain018MsgIDConflict) {
		t.Errorf("conflict reason: err = %v, want ErrPain018MsgIDConflict", err)
	}
	// Same MsgId, different scheme: a conflict too.
	conflict = pain018Doc("001.03", "msg-1", "m-008", "FR12ZZZ12345678901", "Debtor requested pause")
	if _, err := applyPain018Doc(t, l, conflict); !errors.Is(err, ErrPain018MsgIDConflict) {
		t.Errorf("conflict scheme: err = %v, want ErrPain018MsgIDConflict", err)
	}
	// A hand-built request with no reason is refused before
	// anything moves: the reason is required at the apply gate
	// too, the parse gate and the apply gate sharing one check.
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
	if _, err := l2.ApplyPain018(Pain018Request{
		MsgID: "msg-hand", MndtID: "m-009", CreditorSchemeID: "DE98ZZZ09999999999",
	}, "creditor-bank"); !errors.Is(err, ErrInvalidPain018) {
		t.Errorf("missing reason: err = %v, want ErrInvalidPain018", err)
	}
	if m, ok := l2.GetMandate("m-009"); !ok || m.Status != MandateStatusActive {
		t.Errorf("stored mandate = %+v, want still active", m)
	}
}
