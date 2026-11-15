package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// postCAMT057XML posts a raw camt.057 XML document to
// /exports/camt057/apply with an x-ledger-actor header.
func postCAMT057XML(t *testing.T, url, xmlDoc, actor string) (int, map[string]any) {
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

// camt057HTTPDoc renders a camt.057 notification of the given schema
// version, crediting acctXML (the <Acct> inner XML), amount/currency
// expected on valueDate, from debtorXML (the <Dbtr> inner XML).
func camt057HTTPDoc(version, msgID, ntfctnID, acctXML, amount, currency, valueDate, debtorXML string) string {
	var sb strings.Builder
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.057.` + version + `">`)
	sb.WriteString(`<NtfctnToRcv><GrpHdr><MsgId>` + msgID + `</MsgId>`)
	sb.WriteString(`<CreDtTm>2026-11-15T09:00:00Z</CreDtTm></GrpHdr>`)
	sb.WriteString(`<Ntfctn><Id>` + ntfctnID + `</Id>`)
	sb.WriteString(`<Acct><Id>` + acctXML + `</Id></Acct>`)
	sb.WriteString(`<AcctSvcr><FinInstnId><BICFI>CITIUS33</BICFI></FinInstnId></AcctSvcr>`)
	sb.WriteString(`<TtlAmt Ccy="` + currency + `">` + amount + `</TtlAmt>`)
	sb.WriteString(`<XpctdValDt>` + valueDate + `</XpctdValDt>`)
	sb.WriteString(`<Dbtr>` + debtorXML + `</Dbtr>`)
	sb.WriteString(`<Itm><Id>item-1</Id><Amt Ccy="` + currency + `">` + amount + `</Amt><XpctdValDt>` + valueDate + `</XpctdValDt></Itm>`)
	sb.WriteString(`</Ntfctn></NtfctnToRcv></Document>`)
	return sb.String()
}

// Full flow through HTTP: apply a counterparty's camt.057
// notification, replay the same MsgId, and the metrics counter
// moves. The server ledger is the camt.057 export fixture, so the
// GET export stays live on the same server.
func TestHTTPCAMT057Apply(t *testing.T) {
	l, sched := camt057HTTPSetup(t)
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()
	applyURL := srv.URL + "/exports/camt057/apply"

	doc := camt057HTTPDoc("001.07", "msg-http-1", "ntfctn-http-1",
		`<IBAN>GB29NWBK60161331926819</IBAN>`, "250.00", "USD", "2026-11-20",
		`<Pty><Nm>ACME Corp</Nm></Pty>`)
	code, body := postCAMT057XML(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/camt057/apply: status=%d body=%v", code, body)
	}
	receipt, ok := body["receipt"].(map[string]any)
	if !ok {
		t.Fatalf("receipt = %v, want an object", body["receipt"])
	}
	if receipt["msg_id"] != "msg-http-1" || receipt["account"] != "bob" {
		t.Errorf("receipt = %+v, want msg-1/bob", receipt)
	}
	if receipt["amount_cents"] != float64(25000) || receipt["currency"] != "USD" {
		t.Errorf("receipt = %+v, want 25000 USD", receipt)
	}
	if receipt["expected_value_date"] != "2026-11-20" {
		t.Errorf("receipt = %+v, want value date 2026-11-20", receipt)
	}
	// Replay the same MsgId: idempotent, duplicate set.
	code, body = postCAMT057XML(t, applyURL, doc, "ops")
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
	if !strings.Contains(metricsBody, "ledger_camt057_applies_total 2") {
		t.Errorf("metrics exposition missing ledger_camt057_applies_total 2:\n%s", metricsBody)
	}
	// The GET export on the same server is unaffected: the schedule
	// still exports its notice.
	q := url.Values{}
	q.Set("schedule_id", sched.ID)
	q.Set("servicing_bic", "CITIUS33")
	code, _, exportBody := getBody(t, srv.URL+"/exports/camt057?"+q.Encode())
	if code != http.StatusOK {
		t.Fatalf("GET /exports/camt057: status=%d", code)
	}
	if !strings.Contains(exportBody, "camt057-sched-1-0") {
		t.Errorf("export body missing camt057-sched-1-0:\n%s", exportBody)
	}
}

// A malformed document is a 400 with a repair-queue record; an
// unknown account is a 404 with a repair-queue record.
func TestHTTPCAMT057ApplyErrors(t *testing.T) {
	l, _ := camt057HTTPSetup(t)
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()
	applyURL := srv.URL + "/exports/camt057/apply"

	// Wrong schema version: unparsable, but the document still
	// names its MsgId, so it parks.
	bad := camt057HTTPDoc("001.06", "msg-http-bad", "ntfctn-http-bad",
		`<IBAN>GB29NWBK60161331926819</IBAN>`, "250.00", "USD", "2026-11-20",
		`<Pty><Nm>ACME Corp</Nm></Pty>`)
	code, body := postCAMT057XML(t, applyURL, bad, "ops")
	if code != http.StatusBadRequest {
		t.Fatalf("wrong version: status=%d body=%v, want 400", code, body)
	}
	if body["repair_id"] == nil || body["repair_id"] == "" {
		t.Errorf("body = %v, want a repair_id", body)
	}
	// Unknown account: a valid IBAN the ledger never registered
	// (alice's DE89 IBAN is registered on this fixture — the
	// unknown one must be a different valid IBAN).
	unknown := camt057HTTPDoc("001.07", "msg-http-unk", "ntfctn-http-unk",
		`<IBAN>FR1420041010050500013M02606</IBAN>`, "250.00", "USD", "2026-11-20",
		`<Pty><Nm>ACME Corp</Nm></Pty>`)
	code, body = postCAMT057XML(t, applyURL, unknown, "ops")
	if code != http.StatusNotFound {
		t.Fatalf("unknown account: status=%d body=%v, want 404", code, body)
	}
	if body["repair_id"] == nil || body["repair_id"] == "" {
		t.Errorf("body = %v, want a repair_id", body)
	}
	// The failed applies still count in metrics.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_camt057_applies_total 2") {
		t.Errorf("metrics exposition missing ledger_camt057_applies_total 2:\n%s", metricsBody)
	}
}

// The apply handler resolves Othr/Id accounts too: alice has a
// registered IBAN and no entries on this fixture, so she is known
// through the IBAN registry.
func TestHTTPCAMT057ApplyOthrAccount(t *testing.T) {
	l, _ := camt057HTTPSetup(t)
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()
	applyURL := srv.URL + "/exports/camt057/apply"

	doc := camt057HTTPDoc("001.07", "msg-http-othr", "ntfctn-http-othr",
		`<Othr><Id>alice</Id></Othr>`, "99.99", "USD", "2026-11-20",
		`<Agt><FinInstnId><BICFI>DEUTDEFF</BICFI></FinInstnId></Agt>`)
	code, body := postCAMT057XML(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/camt057/apply: status=%d body=%v", code, body)
	}
	receipt, ok := body["receipt"].(map[string]any)
	if !ok {
		t.Fatalf("receipt = %v, want an object", body["receipt"])
	}
	if receipt["account"] != "alice" || receipt["amount_cents"] != float64(9999) {
		t.Errorf("receipt = %+v, want alice/9999", receipt)
	}
}
