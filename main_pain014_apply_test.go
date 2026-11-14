package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// seedPain014ApplyServer creates a server with a funded debtor and
// one issued RfP (rfp-http, merchant asks customer for USD 50.00,
// expiring in an hour) — the full pain.014 inbound input.
func seedPain014ApplyServer(t *testing.T) (*httptest.Server, *ledger.Ledger) {
	t.Helper()
	l := ledger.New()
	srv := httptest.NewServer(newRouter(l))
	t.Cleanup(srv.Close)
	code, _ := postJSON(t, srv.URL+"/entries", `{"debit_account":"customer","credit_account":"bank","amount_cents":10000,"currency":"USD"}`)
	if code != http.StatusCreated {
		t.Fatalf("seed fund status = %d, want 201", code)
	}
	expires := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	code, _ = postJSON(t, srv.URL+"/rfp", fmt.Sprintf(`{"id":"rfp-http","creditor":"merchant","debtor":"customer","amount_cents":5000,"currency":"USD","expires_at":%q}`, expires))
	if code != http.StatusCreated {
		t.Fatalf("seed rfp status = %d, want 201", code)
	}
	return srv, l
}

// postPain014XML posts a raw pain.014 XML document to
// /exports/pain014/apply with an x-ledger-actor header.
func postPain014XML(t *testing.T, url, xmlDoc, actor string) (int, map[string]any) {
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

// pain014HTTPDoc renders a pain.014.001.07 debtor response: MsgId
// msgID answering the pain.013 message for endToEndID with TxSts
// txSts and, when amount is non-empty, the OrgnlTxRef amount.
func pain014HTTPDoc(msgID, endToEndID, txSts, amount, ccy string) string {
	var sb strings.Builder
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pain.014.001.07">`)
	sb.WriteString(`<CdtrPmtActvtnReqStsRpt><GrpHdr><MsgId>` + msgID + `</MsgId>`)
	sb.WriteString(`<CreDtTm>2026-11-14T10:00:00Z</CreDtTm><InitgPty><Nm>DEBTORBANK</Nm></InitgPty></GrpHdr>`)
	sb.WriteString(`<OrgnlGrpInfAndSts><OrgnlMsgId>pain013-` + endToEndID + `</OrgnlMsgId>`)
	sb.WriteString(`<OrgnlMsgNmId>pain.013.001.07</OrgnlMsgNmId>`)
	sb.WriteString(`<OrgnlCreDtTm>2026-11-14T09:00:00Z</OrgnlCreDtTm><OrgnlNbOfTxs>1</OrgnlNbOfTxs></OrgnlGrpInfAndSts>`)
	sb.WriteString(`<OrgnlPmtInfAndSts><OrgnlPmtInfId>pain013-` + endToEndID + `</OrgnlPmtInfId><TxInfAndSts>`)
	sb.WriteString(`<OrgnlEndToEndId>` + endToEndID + `</OrgnlEndToEndId>`)
	sb.WriteString(`<TxSts>` + txSts + `</TxSts>`)
	sb.WriteString(`<OrgnlTxRef>`)
	if amount != "" {
		sb.WriteString(`<Amt><InstdAmt Ccy="` + ccy + `">` + amount + `</InstdAmt></Amt>`)
	}
	sb.WriteString(`<ReqdExctnDt><Dt>2026-11-14</Dt></ReqdExctnDt>`)
	sb.WriteString(`<CdtrAgt><FinInstnId><BIC>NWBKGB2L</BIC></FinInstnId></CdtrAgt>`)
	sb.WriteString(`<Cdtr><Nm>merchant</Nm></Cdtr>`)
	sb.WriteString(`</OrgnlTxRef></TxInfAndSts></OrgnlPmtInfAndSts></CdtrPmtActvtnReqStsRpt></Document>`)
	return sb.String()
}

// Full flow through HTTP: apply the debtor bank's pain.014 ACCP
// response, replay the same MsgId, and the metrics counter moves.
func TestHTTPPain014Apply(t *testing.T) {
	srv, l := seedPain014ApplyServer(t)
	applyURL := srv.URL + "/exports/pain014/apply"

	doc := pain014HTTPDoc("pain014-http-1", "rfp-http", "ACCP", "50.00", "USD")
	code, body := postPain014XML(t, applyURL, doc, "debtor-bank")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/pain014/apply: status=%d body=%v", code, body)
	}
	if body["status"] != "paid" {
		t.Errorf("status = %v, want paid", body["status"])
	}
	if body["transfer_id"] != "rfp/rfp-http/payout" {
		t.Errorf("transfer_id = %v, want rfp/rfp-http/payout", body["transfer_id"])
	}
	if got := l.BalanceIn("merchant", "USD"); got != 5000 {
		t.Errorf("merchant balance = %d, want 5000", got)
	}

	// Replay the same response: a no-op success returning the
	// stored paid RfP.
	code, body = postPain014XML(t, applyURL, doc, "debtor-bank")
	if code != http.StatusOK {
		t.Fatalf("replay POST /exports/pain014/apply: status=%d body=%v", code, body)
	}
	if body["status"] != "paid" {
		t.Errorf("replay status = %v, want paid", body["status"])
	}
	if got := l.BalanceIn("merchant", "USD"); got != 5000 {
		t.Errorf("merchant balance after replay = %d, want 5000", got)
	}

	// Metrics: both applies were counted.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_pain014_applies_total 2") {
		t.Errorf("metrics exposition missing ledger_pain014_applies_total 2:\n%s", metricsBody)
	}
}

// Error mapping: a malformed report is a 400, an unknown EndToEndId
// is a 404, and the RfP gates — a paid RfP, an amount mismatch —
// are 422s.
func TestHTTPPain014ApplyErrors(t *testing.T) {
	srv, _ := seedPain014ApplyServer(t)
	applyURL := srv.URL + "/exports/pain014/apply"

	if code, body := postPain014XML(t, applyURL, "<Document><broken", "ops"); code != http.StatusBadRequest {
		t.Errorf("malformed XML: status=%d body=%v, want 400", code, body)
	}
	if code, body := postPain014XML(t, applyURL, pain014HTTPDoc("m-1", "rfp-nope", "ACCP", "50.00", "USD"), "ops"); code != http.StatusNotFound {
		t.Errorf("unknown EndToEndId: status=%d body=%v, want 404", code, body)
	}
	if code, body := postPain014XML(t, applyURL, pain014HTTPDoc("m-2", "rfp-http", "PDNG", "50.00", "USD"), "ops"); code != http.StatusBadRequest {
		t.Errorf("PDNG verdict: status=%d body=%v, want 400", code, body)
	}
	if code, body := postPain014XML(t, applyURL, pain014HTTPDoc("m-3", "rfp-http", "ACCP", "49.99", "USD"), "ops"); code != http.StatusUnprocessableEntity {
		t.Errorf("amount mismatch: status=%d body=%v, want 422", code, body)
	}
	// Pay the RfP, then a distinct response to it is a 422.
	if code, body := postPain014XML(t, applyURL, pain014HTTPDoc("m-4", "rfp-http", "ACCP", "50.00", "USD"), "ops"); code != http.StatusOK {
		t.Fatalf("accept: status=%d body=%v", code, body)
	}
	if code, body := postPain014XML(t, applyURL, pain014HTTPDoc("m-5", "rfp-http", "RJCT", "50.00", "USD"), "ops"); code != http.StatusUnprocessableEntity {
		t.Errorf("response to paid RfP: status=%d body=%v, want 422", code, body)
	}
}

// The inbound half leaves the outbound half alone: after an apply,
// GET /exports/pain013 still renders the request — and still
// refuses, because the RfP is now paid.
func TestHTTPPain014ApplyLeavesExportIntact(t *testing.T) {
	srv, _ := seedPain014ApplyServer(t)
	applyURL := srv.URL + "/exports/pain014/apply"

	doc := pain014HTTPDoc("pain014-http-2", "rfp-http", "RJCT", "50.00", "USD")
	if code, body := postPain014XML(t, applyURL, doc, "debtor-bank"); code != http.StatusOK {
		t.Fatalf("POST /exports/pain014/apply: status=%d body=%v", code, body)
	}
	code, _, exportBody := getBody(t, srv.URL+"/exports/pain013?rfp_id=rfp-http")
	if code != http.StatusUnprocessableEntity {
		t.Errorf("GET /exports/pain013 after reject: status=%d body=%s, want 422", code, exportBody)
	}
}
