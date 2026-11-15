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

// postPain013XML posts a raw pain.013 XML document to
// /exports/pain013/apply with an x-ledger-actor header.
func postPain013XML(t *testing.T, url, xmlDoc, actor string) (int, map[string]any) {
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

// pain013HTTPDoc renders a pain.013 document of the given schema
// version: msgID asks for endToEndID, debtor IBAN pays creditor
// IBAN amount (decimal) in currency, expiring at xpry (RFC3339)
// and requested for execution on reqd (YYYY-MM-DD).
func pain013HTTPDoc(version, msgID, endToEndID, dbtrIBAN, cdtrIBAN, amount, currency, reqd, xpry string) string {
	var sb strings.Builder
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pain.013.` + version + `">`)
	sb.WriteString(`<CdtrPmtActvtnReq><GrpHdr><MsgId>` + msgID + `</MsgId>`)
	sb.WriteString(`<CreDtTm>2026-11-14T10:00:00Z</CreDtTm>`)
	sb.WriteString(`<NbOfTxs>1</NbOfTxs><InitgPty><Nm>LEDGER</Nm></InitgPty></GrpHdr>`)
	sb.WriteString(`<PmtInf><PmtInfId>` + msgID + `</PmtInfId><PmtMtd>TRF</PmtMtd>`)
	sb.WriteString(`<ReqdExctnDt><Dt>` + reqd + `</Dt></ReqdExctnDt>`)
	sb.WriteString(`<XpryDt><DtTm>` + xpry + `</DtTm></XpryDt>`)
	sb.WriteString(`<Dbtr><Nm>customer</Nm></Dbtr>`)
	sb.WriteString(`<DbtrAcct><Id><IBAN>` + dbtrIBAN + `</IBAN></Id></DbtrAcct>`)
	sb.WriteString(`<DbtrAgt><FinInstnId><BIC>DEUTDEFF</BIC></FinInstnId></DbtrAgt>`)
	sb.WriteString(`<CdtTrfTx><PmtId><EndToEndId>` + endToEndID + `</EndToEndId></PmtId>`)
	sb.WriteString(`<Amt><InstdAmt Ccy="` + currency + `">` + amount + `</InstdAmt></Amt>`)
	sb.WriteString(`<ChrgBr>SLEV</ChrgBr>`)
	sb.WriteString(`<CdtrAgt><FinInstnId><BIC>NWBKGB2L</BIC></FinInstnId></CdtrAgt>`)
	sb.WriteString(`<Cdtr><Nm>merchant</Nm></Cdtr>`)
	sb.WriteString(`<CdtrAcct><Id><IBAN>` + cdtrIBAN + `</IBAN></Id></CdtrAcct>`)
	sb.WriteString(`</CdtTrfTx></PmtInf></CdtrPmtActvtnReq></Document>`)
	return sb.String()
}

// pain013HTTPSetup registers the fixture accounts (customer pays
// merchant) with IBANs and funds the customer, but registers no
// RfP — the inbound request registers it.
func pain013HTTPSetup(t *testing.T) *ledger.Ledger {
	t.Helper()
	l := ledger.New()
	if _, _, err := l.Post(ledger.JournalEntry{
		ID: "fund-cust", DebitAccount: "customer", CreditAccount: "equity",
		AmountCents: 1_000_000, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed Post: %v", err)
	}
	if err := l.SetAccountIBAN("customer", "DE89370400440532013000", "DEUTDEFF"); err != nil {
		t.Fatalf("SetAccountIBAN customer: %v", err)
	}
	if err := l.SetAccountIBAN("merchant", "GB29NWBK60161331926819", "NWBKGB2L"); err != nil {
		t.Fatalf("SetAccountIBAN merchant: %v", err)
	}
	return l
}

func pain013HTTPDocValid(msgID, endToEndID string) string {
	return pain013HTTPDoc("001.07", msgID, endToEndID,
		"DE89370400440532013000", "GB29NWBK60161331926819", "50.00", "USD",
		time.Now().UTC().Format("2006-01-02"),
		time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
}

// Full flow through HTTP: apply a counterparty's pain.013 request,
// replay the same MsgId, and the metrics counter moves. The GET
// export on the same server is unaffected: the registered RfP still
// exports as a pain.013 request.
func TestHTTPPain013Apply(t *testing.T) {
	l := pain013HTTPSetup(t)
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()
	applyURL := srv.URL + "/exports/pain013/apply"

	doc := pain013HTTPDocValid("msg-http-1", "rfp-http-1")
	code, body := postPain013XML(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/pain013/apply: status=%d body=%v", code, body)
	}
	rfp, ok := body["rfp"].(map[string]any)
	if !ok {
		t.Fatalf("rfp = %v, want an object", body["rfp"])
	}
	if rfp["id"] != "rfp-http-1" || rfp["creditor"] != "merchant" || rfp["debtor"] != "customer" {
		t.Errorf("rfp = %+v, want rfp-http-1 merchant/customer", rfp)
	}
	if rfp["amount_cents"] != float64(5000) || rfp["currency"] != "USD" {
		t.Errorf("rfp = %+v, want 5000 USD", rfp)
	}
	if rfp["status"] != "issued" {
		t.Errorf("rfp = %+v, want status issued (registration is not acceptance)", rfp)
	}
	if body["duplicate"] == true {
		t.Errorf("duplicate = true on first apply, want false")
	}
	// Replay the same MsgId with the same terms: idempotent,
	// duplicate set.
	code, body = postPain013XML(t, applyURL, doc, "ops")
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
	if !strings.Contains(metricsBody, "ledger_pain013_applies_total 2") {
		t.Errorf("metrics exposition missing ledger_pain013_applies_total 2:\n%s", metricsBody)
	}
	// The GET export on the same server is unaffected: the
	// registered RfP still exports as a pain.013 request.
	code, exportBody := getXML(t, srv.URL+"/exports/pain013?rfp_id=rfp-http-1")
	if code != http.StatusOK {
		t.Fatalf("GET /exports/pain013: status=%d body=%s", code, exportBody)
	}
	for _, want := range []string{
		"<MsgId>pain013-rfp-http-1</MsgId>",
		"<EndToEndId>rfp-http-1</EndToEndId>",
		`<InstdAmt Ccy="USD">50.00</InstdAmt>`,
	} {
		if !strings.Contains(exportBody, want) {
			t.Errorf("export body missing %q:\n%s", want, exportBody)
		}
	}
}

// Error mapping through HTTP: a wrong-schema document is a 400
// with a repair ID, an unknown account is a 404 with a repair ID,
// a same-party request, a non-positive amount, and a past expiry
// are 400s with repair IDs, and a MsgId reused for different
// terms is a 422.
func TestHTTPPain013ApplyErrors(t *testing.T) {
	l := pain013HTTPSetup(t)
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()
	applyURL := srv.URL + "/exports/pain013/apply"
	now := time.Now().UTC().Format("2006-01-02")
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)

	// Wrong schema version: unparsable, parked.
	bad := pain013HTTPDoc("001.05", "msg-http-bad", "rfp-http-bad",
		"DE89370400440532013000", "GB29NWBK60161331926819", "50.00", "USD", now, future)
	code, body := postPain013XML(t, applyURL, bad, "ops")
	if code != http.StatusBadRequest {
		t.Fatalf("wrong version: status=%d body=%v, want 400", code, body)
	}
	if body["repair_id"] == nil || body["repair_id"] == "" {
		t.Errorf("wrong version: repair_id = %v, want a repair record", body["repair_id"])
	}
	// Unknown debtor account: 404 with a repair ID.
	unknown := pain013HTTPDoc("001.07", "msg-http-unk", "rfp-http-unk",
		"FR1420041010050500013M02606", "GB29NWBK60161331926819", "50.00", "USD", now, future)
	code, body = postPain013XML(t, applyURL, unknown, "ops")
	if code != http.StatusNotFound {
		t.Fatalf("unknown account: status=%d body=%v, want 404", code, body)
	}
	if body["repair_id"] == nil || body["repair_id"] == "" {
		t.Errorf("unknown account: repair_id = %v, want a repair record", body["repair_id"])
	}
	// Same party: 400 with a repair ID.
	same := pain013HTTPDoc("001.07", "msg-http-same", "rfp-http-same",
		"DE89370400440532013000", "DE89370400440532013000", "50.00", "USD", now, future)
	code, body = postPain013XML(t, applyURL, same, "ops")
	if code != http.StatusBadRequest {
		t.Fatalf("same party: status=%d body=%v, want 400", code, body)
	}
	if body["repair_id"] == nil || body["repair_id"] == "" {
		t.Errorf("same party: repair_id = %v, want a repair record", body["repair_id"])
	}
	// Non-positive amount: 400 with a repair ID.
	zero := pain013HTTPDoc("001.07", "msg-http-zero", "rfp-http-zero",
		"DE89370400440532013000", "GB29NWBK60161331926819", "0.00", "USD", now, future)
	code, body = postPain013XML(t, applyURL, zero, "ops")
	if code != http.StatusBadRequest {
		t.Fatalf("zero amount: status=%d body=%v, want 400", code, body)
	}
	if body["repair_id"] == nil || body["repair_id"] == "" {
		t.Errorf("zero amount: repair_id = %v, want a repair record", body["repair_id"])
	}
	// Expiry not in the future: 400 with a repair ID.
	expired := pain013HTTPDoc("001.07", "msg-http-past", "rfp-http-past",
		"DE89370400440532013000", "GB29NWBK60161331926819", "50.00", "USD", now, past)
	code, body = postPain013XML(t, applyURL, expired, "ops")
	if code != http.StatusBadRequest {
		t.Fatalf("past expiry: status=%d body=%v, want 400", code, body)
	}
	if body["repair_id"] == nil || body["repair_id"] == "" {
		t.Errorf("past expiry: repair_id = %v, want a repair record", body["repair_id"])
	}
	// A MsgId reused for different terms is a conflict.
	first := pain013HTTPDocValid("msg-http-2", "rfp-http-2")
	code, body = postPain013XML(t, applyURL, first, "ops")
	if code != http.StatusOK {
		t.Fatalf("first apply: status=%d body=%v", code, body)
	}
	conflict := pain013HTTPDoc("001.07", "msg-http-2", "rfp-http-2",
		"DE89370400440532013000", "GB29NWBK60161331926819", "60.00", "USD", now, future)
	code, body = postPain013XML(t, applyURL, conflict, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("conflict: status=%d body=%v, want 422", code, body)
	}
	// Every apply — served or refused — counts in metrics.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_pain013_applies_total 7") {
		t.Errorf("metrics exposition missing ledger_pain013_applies_total 7:\n%s", metricsBody)
	}
}
