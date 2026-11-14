package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// postCAMT055XML posts a raw camt.055 XML document to
// /exports/camt055/apply with an x-ledger-actor header.
func postCAMT055XML(t *testing.T, url, xmlDoc, actor string) (int, map[string]any) {
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

// camt055HTTPDoc renders a camt.055 cancellation request of the
// given schema version, from assignor (the customer), recalling
// uetr (payoutID) for reason. An empty uetr omits OrgnlUETR (the
// OrgnlInstrId-only shape); an empty payoutID omits OrgnlInstrId
// and leaves OrgnlGrpInf/OrgnlMsgId empty.
func camt055HTTPDoc(version, cxlID, assignor, uetr, payoutID, reason string) string {
	var sb strings.Builder
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.055.` + version + `">`)
	sb.WriteString(`<CstmrPmtCxlReq><Assgnmt><Id>` + cxlID + `</Id>`)
	sb.WriteString(`<Assgnr><Nm>` + assignor + `</Nm></Assgnr>`)
	sb.WriteString(`<Assgne><Nm>our-bank</Nm></Assgne>`)
	sb.WriteString(`<CreDtTm>2026-11-14T10:00:00Z</CreDtTm></Assgnmt>`)
	sb.WriteString(`<Undrlyg><TxInf><CxlId>` + cxlID + `</CxlId>`)
	if uetr != "" {
		sb.WriteString(`<OrgnlUETR>` + uetr + `</OrgnlUETR>`)
	}
	sb.WriteString(`<OrgnlGrpInf><OrgnlMsgId>` + payoutID + `</OrgnlMsgId>`)
	sb.WriteString(`<OrgnlMsgNmId>pacs.008.001.02</OrgnlMsgNmId></OrgnlGrpInf>`)
	if payoutID != "" {
		sb.WriteString(`<OrgnlInstrId>` + payoutID + `</OrgnlInstrId>`)
	}
	sb.WriteString(`<CxlRsnInf><Rsn><Prtry>` + reason + `</Prtry></Rsn></CxlRsnInf>`)
	sb.WriteString(`</TxInf></Undrlyg></CstmrPmtCxlReq></Document>`)
	return sb.String()
}

// Full flow through HTTP: submit a payout, apply the customer's
// camt.055 cancellation request, replay the same CxlId, export the
// open cancellation, resolve it with the counterparty's camt.029,
// and the metrics counter moves.
func TestHTTPCAMT055Apply(t *testing.T) {
	srv := gpiTestServer(t)
	payoutID := "po-camt055-apply-http-1"
	uetr := submittedPayoutHTTP(t, srv.URL, payoutID)
	applyURL := srv.URL + "/exports/camt055/apply"

	doc := camt055HTTPDoc("001.01", "cxl-in-http-1", "ACME Corp", uetr, payoutID, "DUPL")
	code, body := postCAMT055XML(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/camt055/apply: status=%d body=%v", code, body)
	}
	if body["status"] != "requested" {
		t.Errorf("status = %v, want requested", body["status"])
	}
	if body["msg_id"] != uetr+"/cxl/1" {
		t.Errorf("msg_id = %v, want %s/cxl/1", body["msg_id"], uetr)
	}
	if body["seq"] != float64(1) {
		t.Errorf("seq = %v, want 1", body["seq"])
	}
	if body["reason_code"] != "DUPL" {
		t.Errorf("reason_code = %v, want DUPL", body["reason_code"])
	}
	if body["payout_id"] != payoutID {
		t.Errorf("payout_id = %v, want %q", body["payout_id"], payoutID)
	}
	// RequestedBy names the customer from the document, not the
	// operator who applied it.
	if body["requested_by"] != "ACME Corp" {
		t.Errorf("requested_by = %v, want ACME Corp", body["requested_by"])
	}

	// The GET export is unaffected: the open cancellation still
	// renders as the outbound camt.055 — one record, two shapes.
	code, xmlBody := getXMLBody(t, srv.URL+"/exports/camt055?uetr="+uetr)
	if code != http.StatusOK {
		t.Fatalf("GET /exports/camt055: status=%d, want 200", code)
	}
	for _, want := range []string{"CstmrPmtCxlReq", uetr, "DUPL"} {
		if !strings.Contains(xmlBody, want) {
			t.Errorf("export missing %q:\n%s", want, xmlBody)
		}
	}

	// Replay the same CxlId: a no-op success with the same record.
	code, body = postCAMT055XML(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("replay POST /exports/camt055/apply: status=%d body=%v", code, body)
	}
	if body["msg_id"] != uetr+"/cxl/1" || body["seq"] != float64(1) {
		t.Errorf("replay body = %v, want the same record", body)
	}

	// Metrics: both applies were counted.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_camt055_applies_total 2") {
		t.Errorf("metrics exposition missing ledger_camt055_applies_total 2:\n%s", metricsBody)
	}

	// The counterparty's camt.029 resolves the cancellation the
	// customer opened: the payout is cancelled. The inbound
	// request only opened the record — the resolution does the
	// settling.
	if code := postCAMT029XML(t, srv.URL+"/exports/camt029/apply", camt029HTTPDoc(uetr, payoutID, "ACCR")); code != http.StatusOK {
		t.Fatalf("camt029 apply: status=%d, want 200", code)
	}
	code, payout := getJSON(t, srv.URL+"/payouts/"+payoutID)
	if code != http.StatusOK || payout["status"] != "cancelled" {
		t.Errorf("payout after resolve = status %d body %v, want 200 cancelled", code, payout)
	}
}

// Parse refusals park in the repair queue when the document still
// names an instruction; garbage that names nothing gets a plain
// 400 with no repair record.
func TestHTTPCAMT055ApplyParseRejectionParksRepair(t *testing.T) {
	srv := gpiTestServer(t)
	payoutID := "po-camt055-apply-http-2"
	uetr := submittedPayoutHTTP(t, srv.URL, payoutID)
	applyURL := srv.URL + "/exports/camt055/apply"

	// Missing CxlId, identifiers intact: 400 with a repair_id.
	doc := strings.Replace(camt055HTTPDoc("001.01", "cxl-in-http-2", "ACME Corp", uetr, payoutID, "DUPL"), "<CxlId>cxl-in-http-2</CxlId>", "<CxlId></CxlId>", 1)
	if code, body := postCAMT055XML(t, applyURL, doc, "ops"); code != http.StatusBadRequest {
		t.Errorf("missing CxlId: status=%d body=%v, want 400", code, body)
	} else if body["repair_id"] == nil || body["repair_id"] == "" {
		t.Errorf("missing CxlId: body=%v, want a repair_id", body)
	}
	// Wrong schema version, identifiers intact: 400 with a
	// repair_id.
	doc = camt055HTTPDoc("001.02", "cxl-in-http-2", "ACME Corp", uetr, payoutID, "DUPL")
	if code, body := postCAMT055XML(t, applyURL, doc, "ops"); code != http.StatusBadRequest {
		t.Errorf("wrong version: status=%d body=%v, want 400", code, body)
	} else if body["repair_id"] == nil || body["repair_id"] == "" {
		t.Errorf("wrong version: body=%v, want a repair_id", body)
	}
	// Malformed XML names nothing: plain 400, no repair record.
	if code, body := postCAMT055XML(t, applyURL, "<Document><broken", "ops"); code != http.StatusBadRequest {
		t.Errorf("malformed XML: status=%d body=%v, want 400", code, body)
	} else if body["repair_id"] != nil {
		t.Errorf("malformed XML: body=%v, want no repair_id", body)
	}
	// No reference at all: 400 (parse refuses; nothing to apply).
	doc = camt055HTTPDoc("001.01", "cxl-in-http-2", "ACME Corp", "", "", "DUPL")
	if code, body := postCAMT055XML(t, applyURL, doc, "ops"); code != http.StatusBadRequest {
		t.Errorf("no reference: status=%d body=%v, want 400", code, body)
	}
}

// Error mapping: an unknown UETR is a 404, an empty reason code is
// a 422, an unissued or settled payout is a 422, and an
// already-cancelled payout is a 422 — and the fail-closed refusals
// carry their repair-queue record ID in the error body.
func TestHTTPCAMT055ApplyErrors(t *testing.T) {
	srv := gpiTestServer(t)
	payoutID := "po-camt055-apply-http-3"
	uetr := draftPayoutHTTP(t, srv.URL, payoutID)
	applyURL := srv.URL + "/exports/camt055/apply"

	// Unknown UETR: 404 with a repair_id.
	doc := camt055HTTPDoc("001.01", "cxl-in-http-3", "ACME Corp", "no-such-uetr", payoutID, "DUPL")
	if code, body := postCAMT055XML(t, applyURL, doc, "ops"); code != http.StatusNotFound {
		t.Errorf("unknown UETR: status=%d body=%v, want 404", code, body)
	} else if body["repair_id"] == nil || body["repair_id"] == "" {
		t.Errorf("unknown UETR: body=%v, want a repair_id", body)
	}
	// Empty reason: 422 with a repair_id.
	doc = camt055HTTPDoc("001.01", "cxl-in-http-3", "ACME Corp", uetr, payoutID, "")
	if code, body := postCAMT055XML(t, applyURL, doc, "ops"); code != http.StatusUnprocessableEntity {
		t.Errorf("empty reason: status=%d body=%v, want 422", code, body)
	} else if body["repair_id"] == nil || body["repair_id"] == "" {
		t.Errorf("empty reason: body=%v, want a repair_id", body)
	}
	// Draft payout: 422 — only issued money can be recalled.
	doc = camt055HTTPDoc("001.01", "cxl-in-http-3", "ACME Corp", uetr, payoutID, "DUPL")
	if code, body := postCAMT055XML(t, applyURL, doc, "ops"); code != http.StatusUnprocessableEntity {
		t.Errorf("draft payout: status=%d body=%v, want 422", code, body)
	} else if body["repair_id"] == nil || body["repair_id"] == "" {
		t.Errorf("draft payout: body=%v, want a repair_id", body)
	}

	// Settled payout: 422 with a repair_id.
	settlePayoutHTTP(t, srv.URL, payoutID)
	if code, body := postCAMT055XML(t, applyURL, doc, "ops"); code != http.StatusUnprocessableEntity {
		t.Errorf("settled payout: status=%d body=%v, want 422", code, body)
	} else if body["repair_id"] == nil || body["repair_id"] == "" {
		t.Errorf("settled payout: body=%v, want a repair_id", body)
	}

	// Already-cancelled payout: 422.
	payoutID2 := "po-camt055-apply-http-4"
	uetr2 := submittedPayoutHTTP(t, srv.URL, payoutID2)
	doc2 := camt055HTTPDoc("001.01", "cxl-in-http-4", "ACME Corp", uetr2, payoutID2, "DUPL")
	if code, body := postCAMT055XML(t, applyURL, doc2, "ops"); code != http.StatusOK {
		t.Fatalf("apply: status=%d body=%v, want 200", code, body)
	}
	if code := postCAMT029XML(t, srv.URL+"/exports/camt029/apply", camt029HTTPDoc(uetr2, payoutID2, "ACCR")); code != http.StatusOK {
		t.Fatalf("camt029 apply: status=%d, want 200", code)
	}
	doc2 = strings.ReplaceAll(doc2, "cxl-in-http-4", "cxl-in-http-5")
	if code, body := postCAMT055XML(t, applyURL, doc2, "ops"); code != http.StatusUnprocessableEntity {
		t.Errorf("already cancelled: status=%d body=%v, want 422", code, body)
	}
}
