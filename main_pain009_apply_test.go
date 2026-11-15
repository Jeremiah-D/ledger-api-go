package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// postPain009XML posts a raw pain.009 XML document to
// /exports/pain009/apply (the URL carries the operator policy as
// query parameters) with an x-ledger-actor header.
func postPain009XML(t *testing.T, url, xmlDoc, actor string) (int, map[string]any) {
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

// pain009HTTPDoc renders a pain.009 initiation request of the given
// schema version: msgID for mandate mndtID under schemeID, creditor
// cdtrName/cdtrIBAN, debtor dbtrName/dbtrIBAN (agent BIC dbtrBIC
// when non-empty), valid FrDt..ToDt.
func pain009HTTPDoc(version, msgID, mndtID, schemeID, cdtrName, cdtrIBAN, dbtrName, dbtrIBAN, dbtrBIC, frDt, toDt string) string {
	var sb strings.Builder
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pain.009.` + version + `">`)
	sb.WriteString(`<MndtInitnReq><GrpHdr><MsgId>` + msgID + `</MsgId>`)
	sb.WriteString(`<CreDtTm>2026-11-15T09:00:00Z</CreDtTm></GrpHdr>`)
	sb.WriteString(`<Mndt><MndtId>` + mndtID + `</MndtId>`)
	sb.WriteString(`<CdtrSchmeId><Id><PrtryId><Id>` + schemeID + `</Id></PrtryId></Id></CdtrSchmeId>`)
	sb.WriteString(`<Cdtr><Nm>` + cdtrName + `</Nm></Cdtr>`)
	sb.WriteString(`<CdtrAcct><Id><IBAN>` + cdtrIBAN + `</IBAN></Id></CdtrAcct>`)
	sb.WriteString(`<Dbtr><Nm>` + dbtrName + `</Nm></Dbtr>`)
	sb.WriteString(`<DbtrAcct><Id><IBAN>` + dbtrIBAN + `</IBAN></Id></DbtrAcct>`)
	if dbtrBIC != "" {
		sb.WriteString(`<DbtrAgt><FinInstnId><BIC>` + dbtrBIC + `</BIC></FinInstnId></DbtrAgt>`)
	}
	sb.WriteString(`<FrDt>` + frDt + `</FrDt><ToDt>` + toDt + `</ToDt>`)
	sb.WriteString(`</Mndt></MndtInitnReq></Document>`)
	return sb.String()
}

// pain009ApplyURL is the apply endpoint with the operator policy:
// 50000 cents per debit, at most one debit per month.
func pain009ApplyURL(base string) string {
	return base + "/exports/pain009/apply?max_amount_cents=50000&frequency=per-month"
}

// Full flow through HTTP: apply a counterparty's pain.009
// initiation request, replay the same MndtId, and the metrics
// counter moves. The server ledger is the camt.057 export fixture,
// so the GET export stays live on the same server.
func TestHTTPPain009Apply(t *testing.T) {
	l, _ := camt057HTTPSetup(t)
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()
	applyURL := pain009ApplyURL(srv.URL)

	doc := pain009HTTPDoc("001.02", "msg-http-1", "mndt-http-1", "SCHEME-1",
		"Bob Merchant", "GB29NWBK60161331926819",
		"Alice Customer", "DE89370400440532013000", "DEUTDEFF",
		"2026-11-01", "2027-11-01")
	code, body := postPain009XML(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/pain009/apply: status=%d body=%v", code, body)
	}
	mandate, ok := body["mandate"].(map[string]any)
	if !ok {
		t.Fatalf("mandate = %v, want an object", body["mandate"])
	}
	if mandate["id"] != "mndt-http-1" || mandate["payer"] != "alice" || mandate["payee"] != "bob" {
		t.Errorf("mandate = %+v, want mndt-http-1 alice->bob", mandate)
	}
	if mandate["max_amount_cents"] != float64(50000) || mandate["frequency"] != "per-month" {
		t.Errorf("mandate = %+v, want 50000/per-month", mandate)
	}
	if mandate["status"] != "active" {
		t.Errorf("mandate = %+v, want status active", mandate)
	}
	if body["duplicate"] == true {
		t.Errorf("duplicate = true on first apply, want false")
	}
	// Replay the same MndtId with the same terms: idempotent,
	// duplicate set.
	code, body = postPain009XML(t, applyURL, doc, "ops")
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
	if !strings.Contains(metricsBody, "ledger_pain009_applies_total 2") {
		t.Errorf("metrics exposition missing ledger_pain009_applies_total 2:\n%s", metricsBody)
	}
	// The GET export on the same server is unaffected: the
	// registered mandate exports its initiation request.
	q := url.Values{}
	q.Set("mandate", "mndt-http-1")
	q.Set("creditor_scheme_id", "SCHEME-1")
	code, _, exportBody := getBody(t, srv.URL+"/exports/pain009?"+q.Encode())
	if code != http.StatusOK {
		t.Fatalf("GET /exports/pain009: status=%d", code)
	}
	if !strings.Contains(exportBody, "pain009-mndt-http-1-SCHEME-1") {
		t.Errorf("export body missing pain009-mndt-http-1-SCHEME-1:\n%s", exportBody)
	}
	if !strings.Contains(exportBody, "<MndtId>mndt-http-1</MndtId>") {
		t.Errorf("export body missing MndtId mndt-http-1:\n%s", exportBody)
	}
}

// Error mapping through HTTP: a malformed document is a 400 with a
// repair-queue record, an unknown account is a 404 with a
// repair-queue record, a missing or illegal policy is a 400, and a
// MndtId reused for different terms is a 422.
func TestHTTPPain009ApplyErrors(t *testing.T) {
	l, _ := camt057HTTPSetup(t)
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()
	applyURL := pain009ApplyURL(srv.URL)

	// Wrong schema version: unparsable, but the document still
	// names its MsgId, so it parks.
	bad := pain009HTTPDoc("001.01", "msg-http-bad", "mndt-http-bad", "SCHEME-1",
		"Bob Merchant", "GB29NWBK60161331926819",
		"Alice Customer", "DE89370400440532013000", "DEUTDEFF",
		"2026-11-01", "2027-11-01")
	code, body := postPain009XML(t, applyURL, bad, "ops")
	if code != http.StatusBadRequest {
		t.Fatalf("wrong version: status=%d body=%v, want 400", code, body)
	}
	if body["repair_id"] == nil || body["repair_id"] == "" {
		t.Errorf("body = %v, want a repair_id", body)
	}
	// Unknown debtor account: a valid IBAN the ledger never
	// registered.
	unknown := pain009HTTPDoc("001.02", "msg-http-unk", "mndt-http-unk", "SCHEME-1",
		"Bob Merchant", "GB29NWBK60161331926819",
		"Alice Customer", "FR1420041010050500013M02606", "DEUTDEFF",
		"2026-11-01", "2027-11-01")
	code, body = postPain009XML(t, applyURL, unknown, "ops")
	if code != http.StatusNotFound {
		t.Fatalf("unknown account: status=%d body=%v, want 404", code, body)
	}
	if body["repair_id"] == nil || body["repair_id"] == "" {
		t.Errorf("body = %v, want a repair_id", body)
	}
	// Missing policy: the operator's cap and frequency are
	// required, and the wire cannot supply them.
	doc := pain009HTTPDoc("001.02", "msg-http-pol", "mndt-http-pol", "SCHEME-1",
		"Bob Merchant", "GB29NWBK60161331926819",
		"Alice Customer", "DE89370400440532013000", "DEUTDEFF",
		"2026-11-01", "2027-11-01")
	code, body = postPain009XML(t, srv.URL+"/exports/pain009/apply", doc, "ops")
	if code != http.StatusBadRequest {
		t.Fatalf("missing policy: status=%d body=%v, want 400", code, body)
	}
	// Illegal frequency: the policy gate refuses it.
	code, body = postPain009XML(t, srv.URL+"/exports/pain009/apply?max_amount_cents=50000&frequency=per-hour", doc, "ops")
	if code != http.StatusBadRequest {
		t.Fatalf("bad frequency: status=%d body=%v, want 400", code, body)
	}
	// A MndtId reused for different terms is a conflict.
	code, body = postPain009XML(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("first apply: status=%d body=%v", code, body)
	}
	conflictURL := srv.URL + "/exports/pain009/apply?max_amount_cents=60000&frequency=per-month"
	code, body = postPain009XML(t, conflictURL, doc, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("conflict: status=%d body=%v, want 422", code, body)
	}
	// Every apply — served or refused — counts in metrics.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_pain009_applies_total 6") {
		t.Errorf("metrics exposition missing ledger_pain009_applies_total 6:\n%s", metricsBody)
	}
}
