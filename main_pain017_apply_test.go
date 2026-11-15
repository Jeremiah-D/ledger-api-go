package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// postPain017XML posts a raw pain.017 XML document to
// /exports/pain017/apply with an x-ledger-actor header.
func postPain017XML(t *testing.T, url, xmlDoc, actor string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(xmlDoc))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/xml")
	if actor != "" {
		req.Header.Set("x-ledger-actor", actor)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
	return resp.StatusCode, body
}

// pain017HTTPDoc renders a pain.017 copy request of the given
// schema version: msgID requesting the copy of mandate mndtID
// under schemeID, with the requester's status view — the shape
// ExportPain017 renders. An empty status omits MndtSts.
func pain017HTTPDoc(version, msgID, mndtID, schemeID, statusCode string) string {
	var sb strings.Builder
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pain.017.` + version + `">`)
	sb.WriteString(`<MndtCpyReq><GrpHdr><MsgId>` + msgID + `</MsgId>`)
	sb.WriteString(`<CreDtTm>2026-11-15T09:00:00Z</CreDtTm></GrpHdr>`)
	sb.WriteString(`<UndrlygCpyReqDtls><OrgnlMndt><MndtId>` + mndtID + `</MndtId>`)
	sb.WriteString(`<CdtrSchmeId><Id><PrtryId><Id>` + schemeID + `</Id></PrtryId></Id></CdtrSchmeId>`)
	sb.WriteString(`</OrgnlMndt>`)
	if statusCode != "" {
		sb.WriteString(`<MndtSts><Cd>` + statusCode + `</Cd></MndtSts>`)
	}
	sb.WriteString(`</UndrlygCpyReqDtls></MndtCpyReq></Document>`)
	return sb.String()
}

// pain017HTTPSetup registers the mandate fixture's accounts and
// creates one active mandate m-008 (payer cust, payee merchant,
// cap 5000 USD, per-month, valid one year from yesterday) plus
// one expired mandate m-old for the error paths.
func pain017HTTPSetup(t *testing.T) *ledger.Ledger {
	t.Helper()
	l := ledger.New()
	if err := l.SetAccountIBAN("cust", "DE89370400440532013000", ""); err != nil {
		t.Fatalf("SetAccountIBAN cust: %v", err)
	}
	if err := l.SetAccountIBAN("merchant", "GB29NWBK60161331926819", ""); err != nil {
		t.Fatalf("SetAccountIBAN merchant: %v", err)
	}
	now := time.Now()
	if _, err := l.CreateMandate(ledger.Mandate{
		ID: "m-008", Payer: "cust", Payee: "merchant",
		MaxAmountCents: 5000, Currency: "USD", Frequency: "per-month",
		ValidFrom:  now.AddDate(0, 0, -1),
		ValidUntil: now.AddDate(1, 0, 0),
	}); err != nil {
		t.Fatalf("CreateMandate: %v", err)
	}
	if _, err := l.CreateMandate(ledger.Mandate{
		ID: "m-old", Payer: "cust", Payee: "merchant",
		MaxAmountCents: 5000, Currency: "USD", Frequency: "per-month",
		ValidFrom:  now.AddDate(-1, 0, 0),
		ValidUntil: now.AddDate(0, -1, 0),
	}); err != nil {
		t.Fatalf("CreateMandate expired: %v", err)
	}
	return l
}

// Full flow through HTTP: answer a counterparty's pain.017 copy
// request, refuse a stale status view, replay the same MsgId,
// and the metrics counter moves.
func TestPain017ApplyEndpoint_ReturnsMandateCopy(t *testing.T) {
	l := pain017HTTPSetup(t)
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()
	applyURL := srv.URL + "/exports/pain017/apply"

	doc := pain017HTTPDoc("001.03", "msg-http-1", "m-008", "DE98ZZZ09999999999", "ACTV")
	code, body := postPain017XML(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/pain017/apply: status=%d body=%v", code, body)
	}
	mandate, ok := body["mandate"].(map[string]any)
	if !ok {
		t.Fatalf("mandate = %v, want an object", body["mandate"])
	}
	if mandate["id"] != "m-008" || mandate["status"] != "active" {
		t.Errorf("mandate = %+v, want m-008/active", mandate)
	}
	if body["creditor_scheme_id"] != "DE98ZZZ09999999999" {
		t.Errorf("creditor_scheme_id = %v, want DE98ZZZ09999999999", body["creditor_scheme_id"])
	}
	if body["duplicate"] == true {
		t.Errorf("duplicate = true on first apply, want false")
	}
	// A stale status view is a 422: the mandate is active, not
	// cancelled.
	stale := pain017HTTPDoc("001.03", "msg-http-stale", "m-008", "DE98ZZZ09999999999", "CANC")
	code, body = postPain017XML(t, applyURL, stale, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("stale view: status=%d body=%v, want 422", code, body)
	}
	// A malformed document is a 400.
	code, body = postPain017XML(t, applyURL, "<Document><MndtCpyReq>", "ops")
	if code != http.StatusBadRequest {
		t.Fatalf("malformed: status=%d body=%v, want 400", code, body)
	}
	// Replay the same MsgId with the same terms: idempotent,
	// duplicate set.
	code, body = postPain017XML(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("replay: status=%d body=%v", code, body)
	}
	if body["duplicate"] != true {
		t.Errorf("duplicate = %v, want true", body["duplicate"])
	}
	// The same MsgId with a different scheme is a conflict.
	conflict := pain017HTTPDoc("001.03", "msg-http-1", "m-008", "FR12ZZZ12345678901", "ACTV")
	code, body = postPain017XML(t, applyURL, conflict, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("conflict: status=%d body=%v, want 422", code, body)
	}
	// An unknown mandate is a 404.
	unknown := pain017HTTPDoc("001.03", "msg-http-unk", "no-such", "DE98ZZZ09999999999", "")
	code, body = postPain017XML(t, applyURL, unknown, "ops")
	if code != http.StatusNotFound {
		t.Fatalf("unknown mandate: status=%d body=%v, want 404", code, body)
	}
	// The metrics counter counts every apply served.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_pain017_applies_total 6") {
		t.Errorf("metrics exposition missing ledger_pain017_applies_total 6:\n%s", metricsBody)
	}
}
