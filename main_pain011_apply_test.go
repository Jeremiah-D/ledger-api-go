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

// postPain011XML posts a raw pain.011 XML document to
// /exports/pain011/apply with an x-ledger-actor header.
func postPain011XML(t *testing.T, url, xmlDoc, actor string) (int, map[string]any) {
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

// pain011HTTPDoc renders a pain.011 cancellation request of the
// given schema version: msgID cancelling mandate mndtID under
// schemeID, with the optional reason.
func pain011HTTPDoc(version, msgID, mndtID, schemeID, reason string) string {
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

// pain011HTTPSetup registers the mandate fixture's accounts and
// creates one active mandate m-008 (payer cust, payee merchant,
// cap 5000 USD, per-month, valid one year from yesterday) plus
// one expired mandate m-old for the error paths.
func pain011HTTPSetup(t *testing.T) *ledger.Ledger {
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

// Full flow through HTTP: apply a counterparty's pain.011
// cancellation request, replay the same MsgId, and the metrics
// counter moves.
func TestHTTPPain011Apply(t *testing.T) {
	l := pain011HTTPSetup(t)
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()
	applyURL := srv.URL + "/exports/pain011/apply"

	doc := pain011HTTPDoc("001.03", "msg-http-1", "m-008", "DE98ZZZ09999999999", "Debtor requested closure")
	code, body := postPain011XML(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/pain011/apply: status=%d body=%v", code, body)
	}
	mandate, ok := body["mandate"].(map[string]any)
	if !ok {
		t.Fatalf("mandate = %v, want an object", body["mandate"])
	}
	if mandate["id"] != "m-008" || mandate["status"] != "cancelled" {
		t.Errorf("mandate = %+v, want m-008/cancelled", mandate)
	}
	if body["duplicate"] == true {
		t.Errorf("duplicate = true on first apply, want false")
	}
	// Replay the same MsgId with the same terms: idempotent,
	// duplicate set.
	code, body = postPain011XML(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("replay: status=%d body=%v", code, body)
	}
	if body["duplicate"] != true {
		t.Errorf("duplicate = %v, want true", body["duplicate"])
	}
	// The metrics counter counts both applies.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_pain011_applies_total 2") {
		t.Errorf("metrics exposition missing ledger_pain011_applies_total 2:\n%s", metricsBody)
	}
}

// Error mapping through HTTP: a malformed document is a 400, an
// unknown mandate is a 404, an expired mandate is a 422, and a
// new MsgId on an already-cancelled mandate is a 422.
func TestHTTPPain011ApplyErrors(t *testing.T) {
	l := pain011HTTPSetup(t)
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()
	applyURL := srv.URL + "/exports/pain011/apply"

	// Wrong schema version: unparsable.
	bad := pain011HTTPDoc("001.01", "msg-http-bad", "m-008", "DE98ZZZ09999999999", "")
	code, body := postPain011XML(t, applyURL, bad, "ops")
	if code != http.StatusBadRequest {
		t.Fatalf("wrong version: status=%d body=%v, want 400", code, body)
	}
	// Unknown mandate.
	unknown := pain011HTTPDoc("001.03", "msg-http-unk", "no-such", "DE98ZZZ09999999999", "")
	code, body = postPain011XML(t, applyURL, unknown, "ops")
	if code != http.StatusNotFound {
		t.Fatalf("unknown mandate: status=%d body=%v, want 404", code, body)
	}
	// Expired mandate.
	expired := pain011HTTPDoc("001.03", "msg-http-exp", "m-old", "DE98ZZZ09999999999", "")
	code, body = postPain011XML(t, applyURL, expired, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("expired: status=%d body=%v, want 422", code, body)
	}
	// Cancel m-008, then a new MsgId on it is refused.
	first := pain011HTTPDoc("001.03", "msg-http-1", "m-008", "DE98ZZZ09999999999", "")
	code, body = postPain011XML(t, applyURL, first, "ops")
	if code != http.StatusOK {
		t.Fatalf("first apply: status=%d body=%v", code, body)
	}
	fresh := pain011HTTPDoc("001.03", "msg-http-2", "m-008", "DE98ZZZ09999999999", "")
	code, body = postPain011XML(t, applyURL, fresh, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("fresh MsgId on cancelled: status=%d body=%v, want 422", code, body)
	}
	// Every apply — served or refused — counts in metrics.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_pain011_applies_total 5") {
		t.Errorf("metrics exposition missing ledger_pain011_applies_total 5:\n%s", metricsBody)
	}
}
