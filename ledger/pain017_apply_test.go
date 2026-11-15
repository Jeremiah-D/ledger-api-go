package ledger

import (
	"errors"
	"strings"
	"testing"
)

// pain017Doc renders one inbound pain.017.001.03 document of the
// given schema version: msgID requesting the copy of mandate
// mndtID under schemeID, with the requester's status view — the
// shape ExportPain017 renders, so a document this ledger's
// export renders parses back. An empty status omits the MndtSts
// block entirely; an empty mandate ID omits the MndtId element
// entirely.
func pain017Doc(version, msgID, mndtID, schemeID, statusCode string) string {
	var sb strings.Builder
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pain.017.` + version + `">`)
	sb.WriteString(`<MndtCpyReq><GrpHdr><MsgId>` + msgID + `</MsgId>`)
	sb.WriteString(`<CreDtTm>2026-11-15T09:00:00Z</CreDtTm></GrpHdr>`)
	sb.WriteString(`<UndrlygCpyReqDtls><OrgnlMndt>`)
	if mndtID != "" {
		sb.WriteString(`<MndtId>` + mndtID + `</MndtId>`)
	}
	sb.WriteString(`<CdtrSchmeId><Id><PrtryId><Id>` + schemeID + `</Id></PrtryId></Id></CdtrSchmeId>`)
	sb.WriteString(`</OrgnlMndt>`)
	if statusCode != "" {
		sb.WriteString(`<MndtSts><Cd>` + statusCode + `</Cd></MndtSts>`)
	}
	sb.WriteString(`</UndrlygCpyReqDtls></MndtCpyReq></Document>`)
	return sb.String()
}

// applyPain017Doc parses doc and applies it, failing the test on
// parse errors: the gates under test are the apply gates, not the
// parse gates.
func applyPain017Doc(t *testing.T, l *Ledger, doc string) (Pain017ApplyResult, error) {
	t.Helper()
	req, err := ParsePain017([]byte(doc))
	if err != nil {
		t.Fatalf("ParsePain017: %v", err)
	}
	return l.ApplyPain017(req, "creditor-bank")
}

func TestParsePain017_RoundTrip(t *testing.T) {
	day := pain008Day()
	l, m := pain008Fixture(t, MandateFrequencyPerMonth, day, day.AddDate(1, 0, 0))
	doc, err := l.ExportPain017(m.ID, "DE98ZZZ09999999999")
	if err != nil {
		t.Fatalf("ExportPain017: %v", err)
	}
	req, err := ParsePain017(doc)
	if err != nil {
		t.Fatalf("ParsePain017(export): %v", err)
	}
	if req.MsgID != pain017MsgID(m.ID, "DE98ZZZ09999999999") || req.MndtID != "m-008" {
		t.Errorf("req = %+v, want msg %q / m-008", req, pain017MsgID(m.ID, "DE98ZZZ09999999999"))
	}
	if req.CreditorSchemeID != "DE98ZZZ09999999999" || req.StatusCode != "ACTV" {
		t.Errorf("scheme/status = %q/%q, want DE98ZZZ09999999999/ACTV", req.CreditorSchemeID, req.StatusCode)
	}
}

func TestParsePain017_AcceptsMissingStatus(t *testing.T) {
	// MndtSts is [0..1]: a request with no status view parses
	// and applies against whatever the ledger holds.
	req, err := ParsePain017([]byte(pain017Doc("001.03", "msg-1", "m-008", "DE98ZZZ09999999999", "")))
	if err != nil {
		t.Fatalf("ParsePain017: %v", err)
	}
	if req.StatusCode != "" {
		t.Errorf("status = %q, want empty", req.StatusCode)
	}
}

func TestParsePain017_RejectsMissingMandateID(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
	}{
		{"missing MsgId", pain017Doc("001.03", "", "m-008", "DE98ZZZ09999999999", "")},
		{"missing MndtId", pain017Doc("001.03", "msg-1", "", "DE98ZZZ09999999999", "")},
		{"missing scheme", pain017Doc("001.03", "msg-1", "m-008", "", "")},
	} {
		if _, err := ParsePain017([]byte(tc.doc)); !errors.Is(err, ErrInvalidPain017) {
			t.Errorf("%s: err = %v, want ErrInvalidPain017", tc.name, err)
		}
	}
	// Wrong schema version: the namespace gate refuses it.
	doc := pain017Doc("001.01", "msg-1", "m-008", "DE98ZZZ09999999999", "")
	if _, err := ParsePain017([]byte(doc)); !errors.Is(err, ErrInvalidPain017) {
		t.Errorf("wrong version: err = %v, want ErrInvalidPain017", err)
	}
	// Malformed XML and an empty document are refusals too.
	if _, err := ParsePain017([]byte("<Document><MndtCpyReq>")); !errors.Is(err, ErrInvalidPain017) {
		t.Errorf("malformed: err = %v, want ErrInvalidPain017", err)
	}
	if _, err := ParsePain017([]byte("  ")); !errors.Is(err, ErrInvalidPain017) {
		t.Errorf("empty: err = %v, want ErrInvalidPain017", err)
	}
}

func TestApplyPain017_ServesActiveMandateCopy(t *testing.T) {
	al := mustAuditLog(t)
	l, _ := pain010InboundFixture(t, al)
	before := l.Version()

	res, err := applyPain017Doc(t, l, pain017Doc("001.03", "msg-1", "m-008", "DE98ZZZ09999999999", "ACTV"))
	if err != nil {
		t.Fatalf("ApplyPain017: %v", err)
	}
	if res.Duplicate {
		t.Error("duplicate = true on first apply")
	}
	if res.Mandate.ID != "m-008" || res.Mandate.Status != MandateStatusActive {
		t.Errorf("mandate = %+v, want m-008/active", res.Mandate)
	}
	if res.CreditorSchemeID != "DE98ZZZ09999999999" {
		t.Errorf("scheme = %q, want DE98ZZZ09999999999", res.CreditorSchemeID)
	}
	// Serving a copy is read-only: the stored mandate and the
	// version are untouched.
	if m, ok := l.GetMandate("m-008"); !ok || m.Status != MandateStatusActive {
		t.Errorf("stored mandate = %+v, want still active", m)
	}
	if l.Version() != before {
		t.Errorf("version = %d, want %d (read-only, no bump)", l.Version(), before)
	}
	// Audit: exactly one pain017_copy_served, with the exact
	// details — the copy served, not the mandate changed.
	events := readAuditOps(t, al)
	if countOp(events, "pain017_copy_served") != 1 {
		t.Errorf("pain017_copy_served count = %d, want 1", countOp(events, "pain017_copy_served"))
	}
	ev := findOp(events, "pain017_copy_served")
	if ev == nil {
		t.Fatal("pain017_copy_served event missing")
	}
	if ev.Actor != "creditor-bank" {
		t.Errorf("actor = %q, want creditor-bank (ApplyPain017)", ev.Actor)
	}
	if ev.TraceID != "msg-1" {
		t.Errorf("trace = %q, want msg-1", ev.TraceID)
	}
	if ev.Details["msg_id"] != "msg-1" || ev.Details["mandate_id"] != "m-008" ||
		ev.Details["creditor_scheme_id"] != "DE98ZZZ09999999999" || ev.Details["status"] != "active" {
		t.Errorf("details = %v, want msg-1/m-008/DE98ZZZ09999999999/active", ev.Details)
	}
}

func TestApplyPain017_ServesSuspendedMandateCopy(t *testing.T) {
	al := mustAuditLog(t)
	l, m := pain010InboundFixture(t, al)
	if _, err := l.SuspendMandate(m.ID); err != nil {
		t.Fatalf("SuspendMandate: %v", err)
	}
	// No state gate: a suspended mandate's copy is served, and
	// the requester's SUSP view agrees with the ledger.
	res, err := applyPain017Doc(t, l, pain017Doc("001.03", "msg-1", "m-008", "DE98ZZZ09999999999", "SUSP"))
	if err != nil {
		t.Fatalf("ApplyPain017: %v", err)
	}
	if res.Mandate.Status != MandateStatusSuspended {
		t.Errorf("status = %q, want suspended", res.Mandate.Status)
	}
	events := readAuditOps(t, al)
	if countOp(events, "pain017_copy_served") != 1 {
		t.Errorf("pain017_copy_served count = %d, want 1", countOp(events, "pain017_copy_served"))
	}
	if ev := findOp(events, "pain017_copy_served"); ev == nil || ev.Details["status"] != "suspended" {
		t.Errorf("event = %+v, want status suspended", ev)
	}
}

func TestApplyPain017_ServesCancelledMandateCopy(t *testing.T) {
	al := mustAuditLog(t)
	l, m := pain010InboundFixture(t, al)
	if _, err := l.CancelMandate(m.ID); err != nil {
		t.Fatalf("CancelMandate: %v", err)
	}
	// A cancelled mandate's copy is precisely the evidence a
	// dispute needs — served, with the CANC view agreeing.
	res, err := applyPain017Doc(t, l, pain017Doc("001.03", "msg-1", "m-008", "DE98ZZZ09999999999", "CANC"))
	if err != nil {
		t.Fatalf("ApplyPain017: %v", err)
	}
	if res.Mandate.Status != MandateStatusCancelled {
		t.Errorf("status = %q, want cancelled", res.Mandate.Status)
	}
	events := readAuditOps(t, al)
	if countOp(events, "pain017_copy_served") != 1 {
		t.Errorf("pain017_copy_served count = %d, want 1", countOp(events, "pain017_copy_served"))
	}
}

func TestApplyPain017_ServesExpiredMandateCopy(t *testing.T) {
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
	// The effective status reads expired even before the sweep
	// marks it, and the EXPI view agrees.
	res, err := applyPain017Doc(t, l, pain017Doc("001.03", "msg-1", "m-old", "DE98ZZZ09999999999", "EXPI"))
	if err != nil {
		t.Fatalf("ApplyPain017: %v", err)
	}
	if res.Mandate.Status != MandateStatusExpired {
		t.Errorf("status = %q, want expired", res.Mandate.Status)
	}
}

func TestApplyPain017_RefusesUnknownMandate(t *testing.T) {
	al := mustAuditLog(t)
	l, _ := pain010InboundFixture(t, al)
	_, err := applyPain017Doc(t, l, pain017Doc("001.03", "msg-1", "no-such", "DE98ZZZ09999999999", ""))
	if !errors.Is(err, ErrMandateNotFound) {
		t.Errorf("err = %v, want ErrMandateNotFound", err)
	}
	events := readAuditOps(t, al)
	if countOp(events, "pain017_copy_served") != 0 {
		t.Errorf("pain017_copy_served count = %d, want 0", countOp(events, "pain017_copy_served"))
	}
	// A hand-built request with no MsgId is refused before
	// anything moves: the parse gate and the apply gate share
	// one check.
	if _, err := l.ApplyPain017(Pain017Request{
		MndtID: "m-008", CreditorSchemeID: "DE98ZZZ09999999999",
	}, "creditor-bank"); !errors.Is(err, ErrInvalidPain017) {
		t.Errorf("missing MsgId: err = %v, want ErrInvalidPain017", err)
	}
}

func TestApplyPain017_RefusesStaleStatusView(t *testing.T) {
	al := mustAuditLog(t)
	l, _ := pain010InboundFixture(t, al)
	// The mandate is active; the requester believes it
	// cancelled. Serving under a disputed status would be
	// dishonest — refused, no audit event.
	_, err := applyPain017Doc(t, l, pain017Doc("001.03", "msg-1", "m-008", "DE98ZZZ09999999999", "CANC"))
	if !errors.Is(err, ErrPain017StaleStatusView) {
		t.Errorf("stale CANC: err = %v, want ErrPain017StaleStatusView", err)
	}
	// A code outside the four-code set can never agree with an
	// effective status: refused the same way.
	_, err = applyPain017Doc(t, l, pain017Doc("001.03", "msg-2", "m-008", "DE98ZZZ09999999999", "XXXX"))
	if !errors.Is(err, ErrPain017StaleStatusView) {
		t.Errorf("unknown code: err = %v, want ErrPain017StaleStatusView", err)
	}
	events := readAuditOps(t, al)
	if countOp(events, "pain017_copy_served") != 0 {
		t.Errorf("pain017_copy_served count = %d, want 0", countOp(events, "pain017_copy_served"))
	}
	if m, ok := l.GetMandate("m-008"); !ok || m.Status != MandateStatusActive {
		t.Errorf("stored mandate = %+v, want still active", m)
	}
}

func TestApplyPain017_ReplaySameMsgIdNoop(t *testing.T) {
	al := mustAuditLog(t)
	l, _ := pain010InboundFixture(t, al)
	doc := pain017Doc("001.03", "msg-1", "m-008", "DE98ZZZ09999999999", "ACTV")
	if _, err := applyPain017Doc(t, l, doc); err != nil {
		t.Fatalf("ApplyPain017: %v", err)
	}
	// Same MsgId, same terms: an idempotent no-op returning the
	// current copy, with no second audit event.
	res, err := applyPain017Doc(t, l, doc)
	if err != nil {
		t.Fatalf("ApplyPain017 replay: %v", err)
	}
	if !res.Duplicate {
		t.Error("duplicate = false on replay, want true")
	}
	if res.Mandate.Status != MandateStatusActive {
		t.Errorf("status = %q, want active", res.Mandate.Status)
	}
	events := readAuditOps(t, al)
	if countOp(events, "pain017_copy_served") != 1 {
		t.Errorf("pain017_copy_served count = %d, want 1 after replay", countOp(events, "pain017_copy_served"))
	}
	// Same MsgId, different scheme: a conflict, never a silent
	// overwrite.
	conflict := pain017Doc("001.03", "msg-1", "m-008", "FR12ZZZ12345678901", "ACTV")
	if _, err := applyPain017Doc(t, l, conflict); !errors.Is(err, ErrPain017MsgIDConflict) {
		t.Errorf("conflict scheme: err = %v, want ErrPain017MsgIDConflict", err)
	}
	// Same MsgId, different status view: a conflict too.
	conflict = pain017Doc("001.03", "msg-1", "m-008", "DE98ZZZ09999999999", "SUSP")
	if _, err := applyPain017Doc(t, l, conflict); !errors.Is(err, ErrPain017MsgIDConflict) {
		t.Errorf("conflict status view: err = %v, want ErrPain017MsgIDConflict", err)
	}
	events = readAuditOps(t, al)
	if countOp(events, "pain017_copy_served") != 1 {
		t.Errorf("pain017_copy_served count = %d, want 1 after conflicts", countOp(events, "pain017_copy_served"))
	}
}
