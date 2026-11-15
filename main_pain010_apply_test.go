package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// postPain010XML posts a raw pain.010 XML document to
// /exports/pain010/apply with an x-ledger-actor header.
func postPain010XML(t *testing.T, url, xmlDoc, actor string) (int, map[string]any) {
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

// pain010HTTPDoc renders a pain.010 amendment request of the given
// schema version: msgID amending mandate mndtID under schemeID,
// with the stated creditor/debtor IBANs, MaxAmt (amount +
// currency), frequency, and effective date.
func pain010HTTPDoc(version, msgID, mndtID, schemeID, cdtrIBAN, dbtrIBAN, maxAmt, maxCcy, frqcy, frDt string) string {
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

// pain010HTTPSetup registers the mandate fixture's accounts and
// creates one active mandate m-008 (payer cust, payee merchant,
// cap 5000 USD, per-month, valid one year from yesterday).
func pain010HTTPSetup(t *testing.T) *ledger.Ledger {
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
	return l
}

func pain010HTTPEffectiveDate() string {
	return time.Now().AddDate(0, 1, 0).UTC().Format("2006-01-02")
}

// Full flow through HTTP: apply a counterparty's pain.010
// amendment request, replay the same MsgId, and the metrics
// counter moves. The GET export on the same server is unaffected:
// the amended mandate still exports a new amendment request.
func TestHTTPPain010Apply(t *testing.T) {
	l := pain010HTTPSetup(t)
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()
	applyURL := srv.URL + "/exports/pain010/apply"
	effective := pain010HTTPEffectiveDate()

	doc := pain010HTTPDoc("001.02", "msg-http-1", "m-008", "DE98ZZZ09999999999",
		"", "", "90.00", "USD", "per-week", effective)
	code, body := postPain010XML(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/pain010/apply: status=%d body=%v", code, body)
	}
	mandate, ok := body["mandate"].(map[string]any)
	if !ok {
		t.Fatalf("mandate = %v, want an object", body["mandate"])
	}
	if mandate["id"] != "m-008" || mandate["max_amount_cents"] != float64(9000) || mandate["frequency"] != "per-week" {
		t.Errorf("mandate = %+v, want m-008/9000/per-week", mandate)
	}
	if mandate["status"] != "active" {
		t.Errorf("mandate = %+v, want status active", mandate)
	}
	if body["duplicate"] == true {
		t.Errorf("duplicate = true on first apply, want false")
	}
	// Replay the same MsgId with the same terms: idempotent,
	// duplicate set.
	code, body = postPain010XML(t, applyURL, doc, "ops")
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
	if !strings.Contains(metricsBody, "ledger_pain010_applies_total 2") {
		t.Errorf("metrics exposition missing ledger_pain010_applies_total 2:\n%s", metricsBody)
	}
	// The GET export on the same server is unaffected: the
	// amended mandate still exports a new amendment request.
	q := url.Values{}
	q.Set("mandate", "m-008")
	q.Set("creditor_scheme_id", "DE98ZZZ09999999999")
	q.Set("effective_date", pain010HTTPEffectiveDate())
	q.Set("new_frequency", "per-day")
	code, _, exportBody := getBody(t, srv.URL+"/exports/pain010?"+q.Encode())
	if code != http.StatusOK {
		t.Fatalf("GET /exports/pain010: status=%d", code)
	}
	if !strings.Contains(exportBody, "<Frqcy>per-day</Frqcy>") {
		t.Errorf("export body missing per-day frequency:\n%s", exportBody)
	}
	if !strings.Contains(exportBody, "<MndtId>m-008</MndtId>") {
		t.Errorf("export body missing MndtId m-008:\n%s", exportBody)
	}
}

// Error mapping through HTTP: a malformed document is a 400, a
// missing effective date is a 400, an amendment changing nothing
// is a 400, an unknown mandate is a 404, a MsgId reused for
// different terms is a 422, and a suspended mandate is a 422.
func TestHTTPPain010ApplyErrors(t *testing.T) {
	l := pain010HTTPSetup(t)
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()
	applyURL := srv.URL + "/exports/pain010/apply"
	effective := pain010HTTPEffectiveDate()

	// Wrong schema version: unparsable.
	bad := pain010HTTPDoc("001.01", "msg-http-bad", "m-008", "DE98ZZZ09999999999",
		"", "", "90.00", "USD", "", effective)
	code, body := postPain010XML(t, applyURL, bad, "ops")
	if code != http.StatusBadRequest {
		t.Fatalf("wrong version: status=%d body=%v, want 400", code, body)
	}
	// Missing effective date.
	noDate := pain010HTTPDoc("001.02", "msg-http-nodate", "m-008", "DE98ZZZ09999999999",
		"", "", "90.00", "USD", "", "")
	code, body = postPain010XML(t, applyURL, noDate, "ops")
	if code != http.StatusBadRequest {
		t.Fatalf("missing effective date: status=%d body=%v, want 400", code, body)
	}
	// No changed field.
	empty := pain010HTTPDoc("001.02", "msg-http-empty", "m-008", "DE98ZZZ09999999999",
		"", "", "", "", "", effective)
	code, body = postPain010XML(t, applyURL, empty, "ops")
	if code != http.StatusBadRequest {
		t.Fatalf("no changed field: status=%d body=%v, want 400", code, body)
	}
	// Unknown mandate.
	unknown := pain010HTTPDoc("001.02", "msg-http-unk", "no-such", "DE98ZZZ09999999999",
		"", "", "90.00", "USD", "", effective)
	code, body = postPain010XML(t, applyURL, unknown, "ops")
	if code != http.StatusNotFound {
		t.Fatalf("unknown mandate: status=%d body=%v, want 404", code, body)
	}
	// A MsgId reused for different terms is a conflict.
	first := pain010HTTPDoc("001.02", "msg-http-1", "m-008", "DE98ZZZ09999999999",
		"", "", "90.00", "USD", "", effective)
	code, body = postPain010XML(t, applyURL, first, "ops")
	if code != http.StatusOK {
		t.Fatalf("first apply: status=%d body=%v", code, body)
	}
	conflict := pain010HTTPDoc("001.02", "msg-http-1", "m-008", "DE98ZZZ09999999999",
		"", "", "80.00", "USD", "", effective)
	code, body = postPain010XML(t, applyURL, conflict, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("conflict: status=%d body=%v, want 422", code, body)
	}
	// Suspended mandate: the amendment gate refuses it.
	if _, err := l.SuspendMandate("m-008"); err != nil {
		t.Fatalf("SuspendMandate: %v", err)
	}
	suspended := pain010HTTPDoc("001.02", "msg-http-susp", "m-008", "DE98ZZZ09999999999",
		"", "", "70.00", "USD", "", effective)
	code, body = postPain010XML(t, applyURL, suspended, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("suspended: status=%d body=%v, want 422", code, body)
	}
	// Every apply — served or refused — counts in metrics.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_pain010_applies_total 7") {
		t.Errorf("metrics exposition missing ledger_pain010_applies_total 7:\n%s", metricsBody)
	}
}
