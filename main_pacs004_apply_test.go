package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// postPacs004XML posts a raw pacs.004 XML document to
// /exports/pacs004/apply with an x-ledger-actor header.
func postPacs004XML(t *testing.T, url, xmlDoc, actor string) (int, map[string]any) {
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

// pacs004HTTPDoc renders a pacs.004.001.10 return answering msgID
// (a payout ID) for uetr, returning amount in ccy for reason.
func pacs004HTTPDoc(msgID, uetr, amount, ccy, reason string) string {
	var sb strings.Builder
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pacs.004.001.10">`)
	sb.WriteString(`<PmtRtr><GrpHdr><MsgId>rtr-http</MsgId>`)
	sb.WriteString(`<CreDtTm>2026-11-13T10:00:00Z</CreDtTm></GrpHdr>`)
	sb.WriteString(`<OrgnlGrpInf><OrgnlMsgId>` + msgID + `</OrgnlMsgId>`)
	sb.WriteString(`<OrgnlMsgNmId>pacs.008.001.02</OrgnlMsgNmId></OrgnlGrpInf>`)
	sb.WriteString(`<TxInf><RtrdIntrBkSttlmAmt Ccy="` + ccy + `">` + amount + `</RtrdIntrBkSttlmAmt>`)
	sb.WriteString(`<OrgnlEndToEndId>` + msgID + `</OrgnlEndToEndId>`)
	sb.WriteString(`<OrgnlUETR>` + uetr + `</OrgnlUETR>`)
	sb.WriteString(`<RtrRsnInf><Rsn><Prtry>` + reason + `</Prtry></Rsn></RtrRsnInf>`)
	sb.WriteString(`</TxInf></PmtRtr></Document>`)
	return sb.String()
}

// Full flow through HTTP: draft a payout, settle it, apply the
// bank's pacs.004 return, replay the same MsgId, and the metrics
// counter moves.
func TestHTTPPacs004Apply(t *testing.T) {
	srv := gpiTestServer(t)
	payoutID := "po-pacs004-apply-http-1"
	uetr := draftPayoutHTTP(t, srv.URL, payoutID)
	settlePayoutHTTP(t, srv.URL, payoutID)
	applyURL := srv.URL + "/exports/pacs004/apply"

	doc := pacs004HTTPDoc(payoutID, uetr, "2500.00", "USD", "DUPL")
	code, body := postPacs004XML(t, applyURL, doc, "bank-return")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/pacs004/apply: status=%d body=%v", code, body)
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
	if body["reversal_entry_id"] != "payout/"+payoutID+"/return" {
		t.Errorf("reversal_entry_id = %v, want payout/%s/return", body["reversal_entry_id"], payoutID)
	}

	// The payout is returned now: terminal, not settled.
	code, payout := getJSON(t, srv.URL+"/payouts/"+payoutID)
	if code != http.StatusOK || payout["status"] != "returned" {
		t.Errorf("payout after apply = status %d body %v, want 200 returned", code, payout)
	}
	if payout["return_entry_id"] != "payout/"+payoutID+"/return" {
		t.Errorf("payout return_entry_id = %v, want payout/%s/return", payout["return_entry_id"], payoutID)
	}

	// Replay the same MsgId: a no-op success, duplicate set.
	code, body = postPacs004XML(t, applyURL, doc, "bank-return")
	if code != http.StatusOK {
		t.Fatalf("replay POST /exports/pacs004/apply: status=%d body=%v", code, body)
	}
	if body["duplicate"] != true || body["applied"] != false {
		t.Errorf("replay body = %v, want duplicate true, applied false", body)
	}

	// Metrics: both applies were counted.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_pacs004_applies_total 2") {
		t.Errorf("metrics exposition missing ledger_pacs004_applies_total 2:\n%s", metricsBody)
	}
}

// Error mapping: malformed XML is a 400, an unknown reason is a
// 400, an unknown original is a 404 carrying the repair-queue
// record ID, an unsettled payout or an already-returned payout is
// a 422, and an amount mismatch is a 422 carrying the repair-queue
// record ID.
func TestHTTPPacs004ApplyErrors(t *testing.T) {
	srv := gpiTestServer(t)
	payoutID := "po-pacs004-apply-http-2"
	uetr := draftPayoutHTTP(t, srv.URL, payoutID)
	applyURL := srv.URL + "/exports/pacs004/apply"

	if code, body := postPacs004XML(t, applyURL, "<Document><broken", "ops"); code != http.StatusBadRequest {
		t.Errorf("malformed XML: status=%d body=%v, want 400", code, body)
	}
	doc := pacs004HTTPDoc(payoutID, uetr, "2500.00", "USD", "NOPE")
	if code, body := postPacs004XML(t, applyURL, doc, "ops"); code != http.StatusBadRequest {
		t.Errorf("unknown reason: status=%d body=%v, want 400", code, body)
	}
	// Draft payout: 422 — only settled money can be returned.
	doc = pacs004HTTPDoc(payoutID, uetr, "2500.00", "USD", "DUPL")
	if code, body := postPacs004XML(t, applyURL, doc, "ops"); code != http.StatusUnprocessableEntity {
		t.Errorf("draft payout: status=%d body=%v, want 422", code, body)
	}
	// Unknown original: 404, and the repair-queue record ID rides
	// the error body.
	doc = pacs004HTTPDoc("po-no-such-payout", uetr, "2500.00", "USD", "DUPL")
	if code, body := postPacs004XML(t, applyURL, doc, "ops"); code != http.StatusNotFound {
		t.Errorf("unknown original: status=%d body=%v, want 404", code, body)
	} else if body["repair_id"] == nil || body["repair_id"] == "" {
		t.Errorf("unknown original body = %v, want a repair_id", body)
	}

	settlePayoutHTTP(t, srv.URL, payoutID)

	// Partial return: 422, and the repair-queue record ID rides
	// the error body.
	doc = pacs004HTTPDoc(payoutID, uetr, "600.00", "USD", "AM09")
	if code, body := postPacs004XML(t, applyURL, doc, "ops"); code != http.StatusUnprocessableEntity {
		t.Errorf("amount mismatch: status=%d body=%v, want 422", code, body)
	} else if body["repair_id"] == nil || body["repair_id"] == "" {
		t.Errorf("amount mismatch body = %v, want a repair_id", body)
	}

	// Full return applies, then a second MsgId is refused.
	doc = pacs004HTTPDoc(payoutID, uetr, "2500.00", "USD", "DUPL")
	if code, body := postPacs004XML(t, applyURL, doc, "ops"); code != http.StatusOK {
		t.Fatalf("apply: status=%d body=%v, want 200", code, body)
	}
	doc = strings.Replace(doc, "rtr-http", "rtr-http-2", -1)
	if code, body := postPacs004XML(t, applyURL, doc, "ops"); code != http.StatusUnprocessableEntity {
		t.Errorf("already returned: status=%d body=%v, want 422", code, body)
	}
}
