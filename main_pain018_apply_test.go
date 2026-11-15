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

// postPain018XML posts a raw pain.018 XML document to
// /exports/pain018/apply with an x-ledger-actor header.
func postPain018XML(t *testing.T, url, xmlDoc, actor string) (int, map[string]any) {
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

// pain018HTTPDoc renders a pain.018 suspension request of the
// given schema version: msgID suspending mandate mndtID under
// schemeID, with the reason — the shape ExportPain018 renders.
func pain018HTTPDoc(version, msgID, mndtID, schemeID, reason string) string {
	var sb strings.Builder
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pain.018.` + version + `">`)
	sb.WriteString(`<MndtSspnsnReq><GrpHdr><MsgId>` + msgID + `</MsgId>`)
	sb.WriteString(`<CreDtTm>2026-11-15T09:00:00Z</CreDtTm></GrpHdr>`)
	sb.WriteString(`<SspnsnReqId>` + msgID + `</SspnsnReqId>`)
	sb.WriteString(`<OrgnlMndt><MndtId>` + mndtID + `</MndtId>`)
	sb.WriteString(`<CdtrSchmeId><Id><PrtryId><Id>` + schemeID + `</Id></PrtryId></Id></CdtrSchmeId>`)
	sb.WriteString(`</OrgnlMndt>`)
	if reason != "" {
		sb.WriteString(`<SspnsnRsn><Rsn><Prtry>` + reason + `</Prtry></Rsn></SspnsnRsn>`)
	}
	sb.WriteString(`</MndtSspnsnReq></Document>`)
	return sb.String()
}

// pain018HTTPSetup registers the mandate fixture's accounts and
// creates one active mandate m-008 (payer cust, payee merchant,
// cap 5000 USD, per-month, valid one year from yesterday) plus
// one expired mandate m-old for the error paths.
func pain018HTTPSetup(t *testing.T) *ledger.Ledger {
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

// Full flow through HTTP: apply a counterparty's pain.018
// suspension request, replay the same MsgId, and the metrics
// counter moves.
func TestPain018ApplyEndpoint_SuspendsAndReturnsMandate(t *testing.T) {
	l := pain018HTTPSetup(t)
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()
	applyURL := srv.URL + "/exports/pain018/apply"

	doc := pain018HTTPDoc("001.03", "msg-http-1", "m-008", "DE98ZZZ09999999999", "Debtor requested pause")
	code, body := postPain018XML(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/pain018/apply: status=%d body=%v", code, body)
	}
	mandate, ok := body["mandate"].(map[string]any)
	if !ok {
		t.Fatalf("mandate = %v, want an object", body["mandate"])
	}
	if mandate["id"] != "m-008" || mandate["status"] != "suspended" {
		t.Errorf("mandate = %+v, want m-008/suspended", mandate)
	}
	if body["duplicate"] == true {
		t.Errorf("duplicate = true on first apply, want false")
	}
	// A reasonless request is a 400: SspnsnRsn is [1..1].
	reasonless := pain018HTTPDoc("001.03", "msg-http-noreason", "m-008", "DE98ZZZ09999999999", "")
	code, body = postPain018XML(t, applyURL, reasonless, "ops")
	if code != http.StatusBadRequest {
		t.Fatalf("reasonless: status=%d body=%v, want 400", code, body)
	}
	// Replay the same MsgId with the same terms: idempotent,
	// duplicate set.
	code, body = postPain018XML(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("replay: status=%d body=%v", code, body)
	}
	if body["duplicate"] != true {
		t.Errorf("duplicate = %v, want true", body["duplicate"])
	}
	// A new MsgId on the already-suspended mandate is refused.
	fresh := pain018HTTPDoc("001.03", "msg-http-2", "m-008", "DE98ZZZ09999999999", "Debtor requested pause")
	code, body = postPain018XML(t, applyURL, fresh, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("fresh MsgId on suspended: status=%d body=%v, want 422", code, body)
	}
	// An unknown mandate is a 404.
	unknown := pain018HTTPDoc("001.03", "msg-http-unk", "no-such", "DE98ZZZ09999999999", "Debtor requested pause")
	code, body = postPain018XML(t, applyURL, unknown, "ops")
	if code != http.StatusNotFound {
		t.Fatalf("unknown mandate: status=%d body=%v, want 404", code, body)
	}
	// The metrics counter counts every apply served.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_pain018_applies_total 5") {
		t.Errorf("metrics exposition missing ledger_pain018_applies_total 5:\n%s", metricsBody)
	}
}
