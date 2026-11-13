package main

import (
	"net/http"
	"strings"
	"testing"
)

// pain008HTTPDoc renders a pain.002.001.03 report answering a
// pain.008.001.02 message for the given entry verdicts.
func pain008HTTPDoc(msgID string, verdicts ...[3]string) string {
	var sb strings.Builder
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pain.002.001.03">`)
	sb.WriteString(`<CstmrPmtStsRpt><GrpHdr><MsgId>RPT-HTTP-008</MsgId>`)
	sb.WriteString(`<CreDtTm>2026-10-21T10:00:00Z</CreDtTm></GrpHdr>`)
	sb.WriteString(`<OrgnlGrpInfAndSts><OrgnlMsgId>` + msgID + `</OrgnlMsgId>`)
	sb.WriteString(`<OrgnlMsgNmId>pain.008.001.02</OrgnlMsgNmId>`)
	sb.WriteString(`<GrpSts>ACSC</GrpSts></OrgnlGrpInfAndSts>`)
	sb.WriteString(`<OrgnlPmtInfAndSts><OrgnlPmtInfId>PMTINF-1</OrgnlPmtInfId>`)
	for _, v := range verdicts {
		e2e, txSts, reason := v[0], v[1], v[2]
		sb.WriteString(`<TxInfAndSts><OrgnlEndToEndId>` + e2e + `</OrgnlEndToEndId>`)
		sb.WriteString(`<TxSts>` + txSts + `</TxSts>`)
		if reason != "" {
			sb.WriteString(`<StsRsnInf><Rsn><Cd>` + reason + `</Cd></Rsn></StsRsnInf>`)
		}
		sb.WriteString(`</TxInfAndSts>`)
	}
	sb.WriteString(`</OrgnlPmtInfAndSts></CstmrPmtStsRpt></Document>`)
	return sb.String()
}

// pain008TxStateOf extracts an entry's state from the apply response.
func pain008TxStateOf(t *testing.T, body map[string]any, entryID string) string {
	t.Helper()
	txs, ok := body["txs"].([]any)
	if !ok {
		t.Fatalf("response has no txs: %v", body)
	}
	for _, item := range txs {
		m := item.(map[string]any)
		if m["entry_id"] == entryID {
			return m["state"].(string)
		}
	}
	t.Fatalf("entry %q not in response", entryID)
	return ""
}

// pain008StatusTestSetup exports the fixture mandate's collections
// through HTTP and returns the deterministic MsgId from the rendered
// document.
func pain008StatusTestSetup(t *testing.T, srvURL string) string {
	t.Helper()
	code, doc := getXML(t, srvURL+"/exports/pain008?mandate=m-http&creditor_scheme_id=DE98ZZZ09999999999")
	if code != http.StatusOK {
		t.Fatalf("GET /exports/pain008: status=%d body=%s", code, doc)
	}
	const open, close = "<MsgId>", "</MsgId>"
	i, j := strings.Index(doc, open), strings.Index(doc, close)
	if i < 0 || j < 0 || j <= i {
		t.Fatalf("exported document has no MsgId:\n%s", doc)
	}
	return doc[i+len(open) : j]
}

// Full flow through HTTP: export -> pain.002 apply -> the entry
// advances, and the metrics counter moves.
func TestHTTPPain008Apply(t *testing.T) {
	srv := pain008TestServer(t)
	msgID := pain008StatusTestSetup(t, srv.URL)

	doc := pain008HTTPDoc(msgID, [3]string{"d-http", "ACSC", ""})
	code, body := postPain002XML(t, srv.URL+"/exports/pain008/apply", doc, "night-ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/pain008/apply: status=%d body=%v", code, body)
	}
	if got := pain008TxStateOf(t, body, "d-http"); got != "accepted" {
		t.Errorf("d-http state = %q, want accepted", got)
	}
	if body["state"] != "accepted" {
		t.Errorf("instruction state = %v, want accepted", body["state"])
	}

	// Metrics: the apply was counted.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_pain008_applies_total 1") {
		t.Errorf("metrics exposition missing ledger_pain008_applies_total 1:\n%s", metricsBody)
	}
}

// Error mapping: malformed XML is a 400, an unknown OrgnlMsgId is a
// 404, an unknown entry is a 422, and a report that does not answer a
// pain.008 message is a 400 on this endpoint.
func TestHTTPPain008ApplyErrors(t *testing.T) {
	srv := pain008TestServer(t)
	msgID := pain008StatusTestSetup(t, srv.URL)

	if code, body := postPain002XML(t, srv.URL+"/exports/pain008/apply", "<Document><broken", "ops"); code != http.StatusBadRequest {
		t.Errorf("malformed XML: status=%d body=%v, want 400", code, body)
	}
	doc := pain008HTTPDoc("pain008-0-0-never-exported", [3]string{"d-http", "ACSC", ""})
	if code, body := postPain002XML(t, srv.URL+"/exports/pain008/apply", doc, "ops"); code != http.StatusNotFound {
		t.Errorf("unknown OrgnlMsgId: status=%d body=%v, want 404", code, body)
	}
	doc = pain008HTTPDoc(msgID, [3]string{"d-nope", "ACSC", ""})
	if code, body := postPain002XML(t, srv.URL+"/exports/pain008/apply", doc, "ops"); code != http.StatusUnprocessableEntity {
		t.Errorf("unknown entry: status=%d body=%v, want 422", code, body)
	}
	// A genuine pain.001 report belongs on the pain.002 endpoint, not
	// here.
	doc = pain002HTTPDoc("pain001-0-0-x", [3]string{"st1", "ACSC", ""})
	if code, body := postPain002XML(t, srv.URL+"/exports/pain008/apply", doc, "ops"); code != http.StatusBadRequest {
		t.Errorf("pain.001 report on pain.008 endpoint: status=%d body=%v, want 400", code, body)
	}
}
