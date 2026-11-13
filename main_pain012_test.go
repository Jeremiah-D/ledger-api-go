package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// postPain012XML posts a raw pain.012 XML document to
// /exports/pain012/apply with an x-ledger-actor header.
func postPain012XML(t *testing.T, url, xmlDoc, actor string) (int, map[string]any) {
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

// pain012HTTPDoc renders a pain.012.001.03 report answering msgNmID /
// orgnlMsgID for mandateID. accepted=true renders <Accptd>true</Accptd>;
// accepted=false renders <Accptd>false</Accptd> with the bank's
// rejection reason. Pass accepted as nil to omit Accptd entirely.
func pain012HTTPDoc(msgNmID, orgnlMsgID, mandateID string, accepted *bool, reasonCode, reasonInfo string) string {
	var sb strings.Builder
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pain.012.001.03">`)
	sb.WriteString(`<MndtAccptncRpt><GrpHdr><MsgId>RPT-012-HTTP</MsgId>`)
	sb.WriteString(`<CreDtTm>2026-11-13T09:00:00Z</CreDtTm></GrpHdr>`)
	sb.WriteString(`<UndrlygAccptncDtls><OrgnlMsgInf><MsgId>` + orgnlMsgID + `</MsgId>`)
	sb.WriteString(`<MsgNmId>` + msgNmID + `</MsgNmId></OrgnlMsgInf>`)
	sb.WriteString(`<AccptncRslt>`)
	if accepted != nil {
		if *accepted {
			sb.WriteString(`<Accptd>true</Accptd>`)
		} else {
			sb.WriteString(`<Accptd>false</Accptd>`)
		}
	}
	if reasonCode != "" {
		sb.WriteString(`<RjctRsn><Cd>` + reasonCode + `</Cd></RjctRsn>`)
	}
	if reasonInfo != "" {
		sb.WriteString(`<AddtlRjctRsnInf>` + reasonInfo + `</AddtlRjctRsnInf>`)
	}
	sb.WriteString(`</AccptncRslt>`)
	sb.WriteString(`<OrgnlMndt><OrgnlMndtId>` + mandateID + `</OrgnlMndtId></OrgnlMndt>`)
	sb.WriteString(`</UndrlygAccptncDtls></MndtAccptncRpt></Document>`)
	return sb.String()
}

func pain012HTTPBool(b bool) *bool { return &b }

// Full flow through HTTP: a pain.012 acceptance report applies, the
// receipt carries the verdict, and the metrics counter moves.
func TestHTTPPain012Apply(t *testing.T) {
	srv := pain011TestServer(t)

	doc := pain012HTTPDoc("pain.009.001.02", "pain009-m-active-DE98ZZZ09999999999", "m-active", pain012HTTPBool(true), "", "")
	code, body := postPain012XML(t, srv.URL+"/exports/pain012/apply", doc, "night-ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/pain012/apply: status=%d body=%v", code, body)
	}
	if body["msg_id"] != "RPT-012-HTTP" {
		t.Errorf("receipt msg_id = %v, want RPT-012-HTTP", body["msg_id"])
	}
	if body["replayed"] != false {
		t.Errorf("receipt replayed = %v, want false", body["replayed"])
	}
	verdicts, ok := body["verdicts"].([]any)
	if !ok || len(verdicts) != 1 {
		t.Fatalf("receipt verdicts = %v, want one verdict", body["verdicts"])
	}
	v := verdicts[0].(map[string]any)
	if v["mandate_id"] != "m-active" || v["verdict"] != "accepted" || v["orgnl_msg_nm_id"] != "pain.009.001.02" {
		t.Errorf("receipt verdict = %v, want m-active accepted for pain.009.001.02", v)
	}

	// Metrics: the apply was counted.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_pain012_applies_total 1") {
		t.Errorf("metrics exposition missing ledger_pain012_applies_total 1:\n%s", metricsBody)
	}
}

// Error mapping: malformed XML is a 400, an unknown mandate is a
// 404, a detail with no Accptd is a 422.
func TestHTTPPain012ApplyErrors(t *testing.T) {
	srv := pain011TestServer(t)

	if code, body := postPain012XML(t, srv.URL+"/exports/pain012/apply", "<Document><broken", "ops"); code != http.StatusBadRequest {
		t.Errorf("malformed XML: status=%d body=%v, want 400", code, body)
	}
	doc := pain012HTTPDoc("pain.009.001.02", "pain009-m-ghost-DE98ZZZ09999999999", "m-ghost", pain012HTTPBool(true), "", "")
	if code, body := postPain012XML(t, srv.URL+"/exports/pain012/apply", doc, "ops"); code != http.StatusNotFound {
		t.Errorf("unknown mandate: status=%d body=%v, want 404", code, body)
	}
	doc = pain012HTTPDoc("pain.009.001.02", "pain009-m-active-DE98ZZZ09999999999", "m-active", nil, "MD01", "No mandate")
	if code, body := postPain012XML(t, srv.URL+"/exports/pain012/apply", doc, "ops"); code != http.StatusUnprocessableEntity {
		t.Errorf("empty verdict: status=%d body=%v, want 422", code, body)
	}
}
