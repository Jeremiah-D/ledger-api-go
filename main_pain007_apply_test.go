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

// postPain007XML posts a raw pain.007 XML document to
// /exports/pain007/apply with an x-ledger-actor header.
func postPain007XML(t *testing.T, url, xmlDoc, actor string) (int, map[string]any) {
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

// pain007HTTPDoc renders a pain.007 cancellation request of the
// given schema version: msgID cancelling the transfer named by
// endToEnd with the stated reason, original amount, currency, and
// OrgnlTxRef IBANs.
func pain007HTTPDoc(version, msgID, endToEnd, reason, amount, ccy, debtorIBAN, creditorIBAN string) string {
	var sb strings.Builder
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pain.007.` + version + `">`)
	sb.WriteString(`<CstmrPmtCxlReq><GrpHdr><MsgId>` + msgID + `</MsgId>`)
	sb.WriteString(`<CreDtTm>2026-11-15T09:00:00Z</CreDtTm>`)
	sb.WriteString(`<InitgPty><Nm>LEDGER</Nm></InitgPty></GrpHdr>`)
	sb.WriteString(`<OrgnlPmtInfAndSts><TxInfAndSts>`)
	sb.WriteString(`<OrgnlEndToEndId>` + endToEnd + `</OrgnlEndToEndId>`)
	if reason != "" {
		sb.WriteString(`<CxlStsRsnInf><Rsn><Prtry>` + reason + `</Prtry></Rsn></CxlStsRsnInf>`)
	}
	sb.WriteString(`<OrgnlTxRef><Amt><InstdAmt Ccy="` + ccy + `">` + amount + `</InstdAmt></Amt>`)
	sb.WriteString(`<DbtrAcct><Id><IBAN>` + debtorIBAN + `</IBAN></Id></DbtrAcct>`)
	sb.WriteString(`<CdtrAcct><Id><IBAN>` + creditorIBAN + `</IBAN></Id></CdtrAcct>`)
	sb.WriteString(`</OrgnlTxRef></TxInfAndSts></OrgnlPmtInfAndSts>`)
	sb.WriteString(`</CstmrPmtCxlReq></Document>`)
	return sb.String()
}

// pain007HTTPSetup registers the camt.057 fixture accounts and
// posts one transfer the tests cancel, plus a second transfer the
// GET export still serves after the first is reversed.
func pain007HTTPSetup(t *testing.T) *ledger.Ledger {
	t.Helper()
	l, _ := camt057HTTPSetup(t)
	base := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	for _, tr := range []ledger.Transfer{
		{ID: "pay-1", From: "alice", To: "bob", AmountCents: 15000, Currency: "USD", CreatedAt: base},
		{ID: "pay-2", From: "alice", To: "bob", AmountCents: 25050, Currency: "USD", CreatedAt: base.Add(time.Hour)},
	} {
		if _, err := l.PostTransfer(tr); err != nil {
			t.Fatalf("PostTransfer(%s): %v", tr.ID, err)
		}
	}
	return l
}

// Full flow through HTTP: apply a counterparty's pain.007
// cancellation request, replay the same MsgId, and the metrics
// counter moves. The GET export on the same server is unaffected:
// the unreversed transfer still exports its cancellation request.
func TestHTTPPain007Apply(t *testing.T) {
	l := pain007HTTPSetup(t)
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()
	applyURL := srv.URL + "/exports/pain007/apply"

	doc := pain007HTTPDoc("001.02", "msg-http-1", "pay-1", "CUST", "150.00", "USD",
		"DE89370400440532013000", "GB29NWBK60161331926819")
	code, body := postPain007XML(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/pain007/apply: status=%d body=%v", code, body)
	}
	reversal, ok := body["reversal"].(map[string]any)
	if !ok {
		t.Fatalf("reversal = %v, want an object", body["reversal"])
	}
	if reversal["id"] != "pay-1/reversal" || reversal["amount_cents"] != float64(15000) {
		t.Errorf("reversal = %+v, want pay-1/reversal/15000", reversal)
	}
	if body["duplicate"] == true {
		t.Errorf("duplicate = true on first apply, want false")
	}
	// Replay the same MsgId: idempotent, duplicate set.
	code, body = postPain007XML(t, applyURL, doc, "ops")
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
	if !strings.Contains(metricsBody, "ledger_pain007_applies_total 2") {
		t.Errorf("metrics exposition missing ledger_pain007_applies_total 2:\n%s", metricsBody)
	}
	// The GET export on the same server is unaffected: the
	// unreversed transfer still exports its cancellation request.
	q := url.Values{}
	q.Set("transfer_id", "pay-2")
	q.Set("reason", "DUPL")
	code, _, exportBody := getBody(t, srv.URL+"/exports/pain007?"+q.Encode())
	if code != http.StatusOK {
		t.Fatalf("GET /exports/pain007: status=%d", code)
	}
	if !strings.Contains(exportBody, "<MsgId>pay-2/cxl</MsgId>") {
		t.Errorf("export body missing pay-2/cxl MsgId:\n%s", exportBody)
	}
	if !strings.Contains(exportBody, "<OrgnlEndToEndId>pay-2</OrgnlEndToEndId>") {
		t.Errorf("export body missing OrgnlEndToEndId pay-2:\n%s", exportBody)
	}
}

// Error mapping through HTTP: a malformed document is a 400 with a
// repair-queue record, an unknown original transfer is a 404 with a
// repair-queue record, an amount mismatch is a 422 with a
// repair-queue record, and an illegal reason code is a 400 with a
// repair-queue record.
func TestHTTPPain007ApplyErrors(t *testing.T) {
	l := pain007HTTPSetup(t)
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()
	applyURL := srv.URL + "/exports/pain007/apply"

	// Wrong schema version: unparsable, but the document still
	// names its MsgId, so it parks.
	bad := pain007HTTPDoc("001.01", "msg-http-bad", "pay-1", "CUST", "150.00", "USD",
		"DE89370400440532013000", "GB29NWBK60161331926819")
	code, body := postPain007XML(t, applyURL, bad, "ops")
	if code != http.StatusBadRequest {
		t.Fatalf("wrong version: status=%d body=%v, want 400", code, body)
	}
	if body["repair_id"] == nil || body["repair_id"] == "" {
		t.Errorf("body = %v, want a repair_id", body)
	}
	// Unknown original transfer: the ledger never registered it.
	unknown := pain007HTTPDoc("001.02", "msg-http-unk", "no-such", "CUST", "150.00", "USD",
		"DE89370400440532013000", "GB29NWBK60161331926819")
	code, body = postPain007XML(t, applyURL, unknown, "ops")
	if code != http.StatusNotFound {
		t.Fatalf("unknown transfer: status=%d body=%v, want 404", code, body)
	}
	if body["repair_id"] == nil || body["repair_id"] == "" {
		t.Errorf("body = %v, want a repair_id", body)
	}
	// Amount mismatch: the request points at a different payment.
	mismatch := pain007HTTPDoc("001.02", "msg-http-mm", "pay-1", "AM09", "150.01", "USD",
		"DE89370400440532013000", "GB29NWBK60161331926819")
	code, body = postPain007XML(t, applyURL, mismatch, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("amount mismatch: status=%d body=%v, want 422", code, body)
	}
	if body["repair_id"] == nil || body["repair_id"] == "" {
		t.Errorf("body = %v, want a repair_id", body)
	}
	// Illegal reason code: the vocabulary refuses it.
	illegal := pain007HTTPDoc("001.02", "msg-http-rsn", "pay-1", "NOPE", "150.00", "USD",
		"DE89370400440532013000", "GB29NWBK60161331926819")
	code, body = postPain007XML(t, applyURL, illegal, "ops")
	if code != http.StatusBadRequest {
		t.Fatalf("illegal reason: status=%d body=%v, want 400", code, body)
	}
	if body["repair_id"] == nil || body["repair_id"] == "" {
		t.Errorf("body = %v, want a repair_id", body)
	}
	// Every apply — served or refused — counts in metrics.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_pain007_applies_total 4") {
		t.Errorf("metrics exposition missing ledger_pain007_applies_total 4:\n%s", metricsBody)
	}
}
