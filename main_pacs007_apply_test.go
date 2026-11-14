package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// postPacs007XML posts a raw pacs.007 XML document to
// /exports/pacs007/apply with an x-ledger-actor header.
func postPacs007XML(t *testing.T, url, xmlDoc, actor string) (int, map[string]any) {
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

// pacs007HTTPDoc renders a pacs.007.001.02 reversal answering msgID
// (a payout ID) for uetr, reversing amount in ccy for reason.
func pacs007HTTPDoc(msgID, uetr, amount, ccy, reason string) string {
	var sb strings.Builder
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pacs.007.001.02">`)
	sb.WriteString(`<FIToFIPmtRvsl><GrpHdr><MsgId>rvsl-http</MsgId>`)
	sb.WriteString(`<CreDtTm>2026-11-13T10:00:00Z</CreDtTm></GrpHdr>`)
	sb.WriteString(`<OrgnlGrpInf><OrgnlMsgId>` + msgID + `</OrgnlMsgId>`)
	sb.WriteString(`<OrgnlMsgNmId>pacs.008.001.02</OrgnlMsgNmId></OrgnlGrpInf>`)
	sb.WriteString(`<TxInf><RvslId>rvsl-http</RvslId>`)
	sb.WriteString(`<OrgnlEndToEndId>` + msgID + `</OrgnlEndToEndId>`)
	sb.WriteString(`<OrgnlUETR>` + uetr + `</OrgnlUETR>`)
	sb.WriteString(`<RvsdIntrBkSttlmAmt Ccy="` + ccy + `">` + amount + `</RvsdIntrBkSttlmAmt>`)
	sb.WriteString(`<RtrRsnInf><Rsn><Prtry>` + reason + `</Prtry></Rsn></RtrRsnInf>`)
	sb.WriteString(`<OrgnlTxRef><OrgnlIntrBkSttlmAmt Ccy="` + ccy + `">` + amount + `</OrgnlIntrBkSttlmAmt>`)
	sb.WriteString(`<OrgnlEndToEndId>` + msgID + `</OrgnlEndToEndId></OrgnlTxRef>`)
	sb.WriteString(`</TxInf></FIToFIPmtRvsl></Document>`)
	return sb.String()
}

// settlePayoutHTTP drives a drafted payout through approve, submit
// and settle against a matching bank statement line, the
// main_pacs007_test.go convention.
func settlePayoutHTTP(t *testing.T, srvURL, payoutID string) {
	t.Helper()
	code, _ := postJSON(t, srvURL+"/payouts/"+payoutID+"/approve", `{"approver":"alice"}`)
	if code != http.StatusOK {
		t.Fatalf("approve: status=%d, want 200", code)
	}
	code, _ = postJSON(t, srvURL+"/payouts/"+payoutID+"/submit", "")
	if code != http.StatusOK {
		t.Fatalf("submit: status=%d, want 200", code)
	}
	code, body := postJSON(t, srvURL+"/payouts/"+payoutID+"/settle",
		fmt.Sprintf(`{"statement":{"id":"stmt-p7a-1","account":"saver","currency":"USD","direction":"out",`+
			`"amount_cents":250000,"posted_at":%q}}`, time.Now().UTC().Format(time.RFC3339)))
	if code != http.StatusOK || body["status"] != "settled" {
		t.Fatalf("settle = status %d body %v, want 200 settled", code, body)
	}
}

// Full flow through HTTP: draft a payout, settle it, apply the
// bank's pacs.007 reversal, replay the same MsgId, and the metrics
// counter moves.
func TestHTTPPacs007Apply(t *testing.T) {
	srv := gpiTestServer(t)
	payoutID := "po-pacs007-apply-http-1"
	uetr := draftPayoutHTTP(t, srv.URL, payoutID)
	settlePayoutHTTP(t, srv.URL, payoutID)
	applyURL := srv.URL + "/exports/pacs007/apply"

	doc := pacs007HTTPDoc(payoutID, uetr, "2500.00", "USD", "DUPL")
	code, body := postPacs007XML(t, applyURL, doc, "bank-reversal")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/pacs007/apply: status=%d body=%v", code, body)
	}
	if body["applied"] != true {
		t.Errorf("applied = %v, want true", body["applied"])
	}
	if body["duplicate"] != false {
		t.Errorf("duplicate = %v, want false", body["duplicate"])
	}
	if body["payout_id"] != payoutID {
		t.Errorf("payout_id = %v, want %q", body["payout_id"], payoutID)
	}
	if body["reversal_entry_id"] != "payout/"+payoutID+"/reversal" {
		t.Errorf("reversal_entry_id = %v, want payout/%s/reversal", body["reversal_entry_id"], payoutID)
	}

	// The payout is reversed now: terminal, not settled.
	code, payout := getJSON(t, srv.URL+"/payouts/"+payoutID)
	if code != http.StatusOK || payout["status"] != "reversed" {
		t.Errorf("payout after apply = status %d body %v, want 200 reversed", code, payout)
	}
	if payout["reversal_entry_id"] != "payout/"+payoutID+"/reversal" {
		t.Errorf("payout reversal_entry_id = %v, want payout/%s/reversal", payout["reversal_entry_id"], payoutID)
	}

	// Replay the same MsgId: a no-op success, duplicate set.
	code, body = postPacs007XML(t, applyURL, doc, "bank-reversal")
	if code != http.StatusOK {
		t.Fatalf("replay POST /exports/pacs007/apply: status=%d body=%v", code, body)
	}
	if body["duplicate"] != true || body["applied"] != false {
		t.Errorf("replay body = %v, want duplicate true, applied false", body)
	}

	// Metrics: both applies were counted.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_pacs007_applies_total 2") {
		t.Errorf("metrics exposition missing ledger_pacs007_applies_total 2:\n%s", metricsBody)
	}
}

// Error mapping: malformed XML is a 400, an unknown UETR is a 404, a
// foreign MsgId or a wrong message name is a 400, an unsettled
// payout or an already-reversed payout is a 422, and an amount
// mismatch is a 422 carrying the repair-queue record ID.
func TestHTTPPacs007ApplyErrors(t *testing.T) {
	srv := gpiTestServer(t)
	payoutID := "po-pacs007-apply-http-2"
	uetr := draftPayoutHTTP(t, srv.URL, payoutID)
	applyURL := srv.URL + "/exports/pacs007/apply"

	if code, body := postPacs007XML(t, applyURL, "<Document><broken", "ops"); code != http.StatusBadRequest {
		t.Errorf("malformed XML: status=%d body=%v, want 400", code, body)
	}
	doc := pacs007HTTPDoc(payoutID, "no-such-uetr", "2500.00", "USD", "DUPL")
	if code, body := postPacs007XML(t, applyURL, doc, "ops"); code != http.StatusNotFound {
		t.Errorf("unknown UETR: status=%d body=%v, want 404", code, body)
	}
	doc = pacs007HTTPDoc("po-other-payout", uetr, "2500.00", "USD", "DUPL")
	if code, body := postPacs007XML(t, applyURL, doc, "ops"); code != http.StatusBadRequest {
		t.Errorf("foreign MsgId: status=%d body=%v, want 400", code, body)
	}
	doc = strings.Replace(pacs007HTTPDoc(payoutID, uetr, "2500.00", "USD", "DUPL"),
		"pacs.008.001.02", "pain.001.001.03", 1)
	if code, body := postPacs007XML(t, applyURL, doc, "ops"); code != http.StatusBadRequest {
		t.Errorf("wrong message name: status=%d body=%v, want 400", code, body)
	}
	doc = pacs007HTTPDoc(payoutID, uetr, "2500.00", "USD", "NOPE")
	if code, body := postPacs007XML(t, applyURL, doc, "ops"); code != http.StatusBadRequest {
		t.Errorf("unknown reason: status=%d body=%v, want 400", code, body)
	}
	// Draft payout: 422 — only settled money can be reversed.
	doc = pacs007HTTPDoc(payoutID, uetr, "2500.00", "USD", "DUPL")
	if code, body := postPacs007XML(t, applyURL, doc, "ops"); code != http.StatusUnprocessableEntity {
		t.Errorf("draft payout: status=%d body=%v, want 422", code, body)
	}

	settlePayoutHTTP(t, srv.URL, payoutID)

	// Partial reversal: 422, and the repair-queue record ID rides
	// the error body.
	doc = pacs007HTTPDoc(payoutID, uetr, "600.00", "USD", "AM09")
	if code, body := postPacs007XML(t, applyURL, doc, "ops"); code != http.StatusUnprocessableEntity {
		t.Errorf("amount mismatch: status=%d body=%v, want 422", code, body)
	} else if body["repair_id"] == nil || body["repair_id"] == "" {
		t.Errorf("amount mismatch body = %v, want a repair_id", body)
	}

	// Full reversal applies, then a second MsgId is refused.
	doc = pacs007HTTPDoc(payoutID, uetr, "2500.00", "USD", "DUPL")
	if code, body := postPacs007XML(t, applyURL, doc, "ops"); code != http.StatusOK {
		t.Fatalf("apply: status=%d body=%v, want 200", code, body)
	}
	doc = strings.Replace(doc, "rvsl-http", "rvsl-http-2", -1)
	if code, body := postPacs007XML(t, applyURL, doc, "ops"); code != http.StatusUnprocessableEntity {
		t.Errorf("already reversed: status=%d body=%v, want 422", code, body)
	}
}
