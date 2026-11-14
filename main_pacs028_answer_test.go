package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// postPacs028XML posts a raw pacs.028 XML document to
// /exports/pacs028/answer and returns the status, raw body, and
// response headers.
func postPacs028XML(t *testing.T, url, xmlDoc string) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(xmlDoc))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/xml")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return resp.StatusCode, string(raw), resp.Header
}

// pacs028AnswerDoc renders a pacs.028.001.03 status request asking
// about orgnlMsgID (a payout ID) for uetr, from requester. An empty
// requester omits the Reqstr elements entirely.
func pacs028AnswerDoc(msgID, orgnlMsgID, orgnlMsgNmID, uetr, requester string) string {
	var sb strings.Builder
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pacs.028.001.03">`)
	sb.WriteString(`<FIToFIPmtStsReq><GrpHdr><MsgId>` + msgID + `</MsgId>`)
	sb.WriteString(`<CreDtTm>2026-11-13T10:00:00Z</CreDtTm>`)
	if requester != "" {
		sb.WriteString(`<Reqstr><Id>` + requester + `</Id></Reqstr>`)
	}
	sb.WriteString(`</GrpHdr><OrgnlGrpInfAndSts><OrgnlMsgId>` + orgnlMsgID + `</OrgnlMsgId>`)
	sb.WriteString(`<OrgnlMsgNmId>` + orgnlMsgNmID + `</OrgnlMsgNmId>`)
	sb.WriteString(`<OrgnlUETR>` + uetr + `</OrgnlUETR>`)
	if requester != "" {
		sb.WriteString(`<Reqstr><Id>` + requester + `</Id></Reqstr>`)
	}
	sb.WriteString(`</OrgnlGrpInfAndSts></FIToFIPmtStsReq></Document>`)
	return sb.String()
}

// Full flow through HTTP: draft a payout, advance it to ACSC, answer
// a pacs.028 request about it, and the reply is the pacs.002 report
// for the track — same UETR, same TxSts — and the metrics counter
// moves.
func TestHTTPPacs028Answer(t *testing.T) {
	srv := gpiTestServer(t)
	payoutID := "po-pacs028-ans-http-1"
	uetr := draftPayoutHTTP(t, srv.URL, payoutID)
	if code, body := postJSONWithActor(t, srv.URL+"/gpi/track/"+uetr+"/advance",
		`{"status":"ACSC"}`, "ops"); code != http.StatusOK {
		t.Fatalf("advance: status=%d body=%v, want 200", code, body)
	}

	doc := pacs028AnswerDoc("REQ-HTTP-1", payoutID, "pacs.008.001.02", uetr, "bank-ops")
	code, body, header := postPacs028XML(t, srv.URL+"/exports/pacs028/answer", doc)
	if code != http.StatusOK {
		t.Fatalf("POST /exports/pacs028/answer: status=%d body=%v", code, body)
	}
	if ct := header.Get("Content-Type"); ct != "application/xml; charset=utf-8" {
		t.Errorf("Content-Type = %q, want application/xml; charset=utf-8", ct)
	}
	for _, want := range []string{
		"FIToFIPmtStsRpt",
		"<OrgnlUETR>" + uetr + "</OrgnlUETR>",
		"<TxSts>ACSC</TxSts>",
		"<OrgnlMsgId>" + payoutID + "</OrgnlMsgId>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("answer missing %q:\n%s", want, body)
		}
	}

	// Metrics: the answer was counted.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_pacs028_answers_total 1") {
		t.Errorf("metrics exposition missing ledger_pacs028_answers_total 1:\n%s", metricsBody)
	}
}

// Error mapping: malformed XML is a 400, an unknown UETR is a 404, a
// foreign MsgId is a 400, a wrong OrgnlMsgNmId is a 400, and a
// request without an identified requester is a 400.
func TestHTTPPacs028AnswerErrors(t *testing.T) {
	srv := gpiTestServer(t)
	payoutID := "po-pacs028-ans-http-2"
	uetr := draftPayoutHTTP(t, srv.URL, payoutID)
	answerURL := srv.URL + "/exports/pacs028/answer"

	if code, body, _ := postPacs028XML(t, answerURL, "<Document><broken"); code != http.StatusBadRequest {
		t.Errorf("malformed XML: status=%d body=%v, want 400", code, body)
	}
	doc := pacs028AnswerDoc("REQ-1", "po-nope", "pacs.008.001.02", "no-such-uetr", "bank-ops")
	if code, body, _ := postPacs028XML(t, answerURL, doc); code != http.StatusNotFound {
		t.Errorf("unknown UETR: status=%d body=%v, want 404", code, body)
	}
	doc = pacs028AnswerDoc("REQ-1", "po-other-payout", "pacs.008.001.02", uetr, "bank-ops")
	if code, body, _ := postPacs028XML(t, answerURL, doc); code != http.StatusBadRequest {
		t.Errorf("foreign MsgId: status=%d body=%v, want 400", code, body)
	}
	doc = pacs028AnswerDoc("REQ-1", payoutID, "pacs.008.001.08", uetr, "bank-ops")
	if code, body, _ := postPacs028XML(t, answerURL, doc); code != http.StatusBadRequest {
		t.Errorf("wrong OrgnlMsgNmId: status=%d body=%v, want 400", code, body)
	}
	doc = pacs028AnswerDoc("REQ-1", payoutID, "pacs.008.001.02", uetr, "")
	if code, body, _ := postPacs028XML(t, answerURL, doc); code != http.StatusBadRequest {
		t.Errorf("empty requester: status=%d body=%v, want 400", code, body)
	}
}
