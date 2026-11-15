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

// seedPacs010ApplyServer creates a server with a funded payer and two
// mandates m-pacs010 / m-pacs010b (cust -> merchant, 5000 USD cap,
// per-day) whose windows span today by a wide margin, so a collection
// date of today — or a few days either way — is inside them unless
// the test moves it outside on purpose. Two mandates let a two-item
// batch book both items without tripping either mandate's per-day
// frequency window. It returns the server and the underlying ledger
// for direct setup.
func seedPacs010ApplyServer(t *testing.T) (*httptest.Server, *ledger.Ledger) {
	t.Helper()
	l := ledger.New()
	srv := httptest.NewServer(newRouter(l))
	t.Cleanup(srv.Close)
	code, _ := postJSON(t, srv.URL+"/entries", `{"debit_account":"cust","credit_account":"bank","amount_cents":100000,"currency":"USD"}`)
	if code != http.StatusCreated {
		t.Fatalf("seed fund status = %d, want 201", code)
	}
	now := time.Now().UTC()
	for _, id := range []string{"m-pacs010", "m-pacs010b"} {
		code, body := postJSON(t, srv.URL+"/mandates", fmt.Sprintf(`{"id":%q,"payer":"cust","payee":"merchant","max_amount_cents":5000,"currency":"USD","frequency":"per-day","valid_from":%q,"valid_until":%q}`,
			id, now.Add(-48*time.Hour).Format(time.RFC3339), now.Add(30*24*time.Hour).Format(time.RFC3339)))
		if code != http.StatusCreated {
			t.Fatalf("seed mandate %s status = %d, want 201", id, code)
		}
		if body["status"] != "active" {
			t.Fatalf("seed mandate %s status = %v, want active", id, body)
		}
	}
	return srv, l
}

// postPacs010XML posts a raw pacs.010 XML document to
// /exports/pacs010/apply with an x-ledger-actor header.
func postPacs010XML(t *testing.T, url, xmlDoc, actor string) (int, map[string]any) {
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

// pacs010HTTPItem is one CdtInstr of a test document.
type pacs010HTTPItem struct {
	endToEnd string
	amount   string
	ccy      string
	mandate  string
	colltnDt string
}

// pacs010HTTPDoc renders a pacs.010.001.02 document for msgID
// (SttlmMtd sttlmMtd, NbOfTxs and CtrlSum computed from the items),
// one CdtInstr per item.
func pacs010HTTPDoc(msgID, sttlmMtd string, items ...pacs010HTTPItem) string {
	return pacs010HTTPDocCtrl(msgID, sttlmMtd, "", items...)
}

// pacs010HTTPDocCtrl is pacs010HTTPDoc with an explicit CtrlSum
// (empty means computed).
func pacs010HTTPDocCtrl(msgID, sttlmMtd, ctrlSum string, items ...pacs010HTTPItem) string {
	var sb strings.Builder
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pacs.010.001.02">`)
	sb.WriteString(`<FIDrctDbt><GrpHdr><MsgId>` + msgID + `</MsgId>`)
	sb.WriteString(`<CreDtTm>2026-11-14T07:00:00Z</CreDtTm>`)
	sb.WriteString(fmt.Sprintf(`<NbOfTxs>%d</NbOfTxs>`, len(items)))
	if ctrlSum == "" {
		var sum int64
		for _, it := range items {
			var whole, frac int64
			fmt.Sscanf(it.amount, "%d.%d", &whole, &frac)
			sum += whole*100 + frac
		}
		ctrlSum = fmt.Sprintf("%d.%02d", sum/100, sum%100)
	}
	sb.WriteString(`<CtrlSum>` + ctrlSum + `</CtrlSum>`)
	sb.WriteString(`<SttlmInf><SttlmMtd>` + sttlmMtd + `</SttlmMtd></SttlmInf>`)
	sb.WriteString(`</GrpHdr>`)
	for _, it := range items {
		sb.WriteString(`<CdtInstr><PmtId><InstrId>instr-` + it.endToEnd + `</InstrId><EndToEndId>` + it.endToEnd + `</EndToEndId></PmtId>`)
		sb.WriteString(`<InstdAmt Ccy="` + it.ccy + `">` + it.amount + `</InstdAmt>`)
		sb.WriteString(`<ReqdColltnDt>` + it.colltnDt + `</ReqdColltnDt>`)
		sb.WriteString(`<Cdtr><Nm>Merchant Bank</Nm></Cdtr>`)
		sb.WriteString(`<CdtrAcct><Id><IBAN>DE89370400440532013000</IBAN></Id></CdtrAcct>`)
		sb.WriteString(`<CdtrAgt><FinInstnId><BIC>BANKCRED</BIC></FinInstnId></CdtrAgt>`)
		sb.WriteString(`<Dbtr><Nm>Customer Bank</Nm></Dbtr>`)
		sb.WriteString(`<DbtrAcct><Id><IBAN>GB29NWBK60161331926819</IBAN></Id></DbtrAcct>`)
		sb.WriteString(`<DbtrAgt><FinInstnId><BIC>BANKDEBB</BIC></FinInstnId></DbtrAgt>`)
		sb.WriteString(`<DrctDbtTx><MndtRltdInf><MndtId>` + it.mandate + `</MndtId>`)
		sb.WriteString(`<DtOfSgntr>2026-01-15</DtOfSgntr></MndtRltdInf></DrctDbtTx>`)
		sb.WriteString(`</CdtInstr>`)
	}
	sb.WriteString(`</FIDrctDbt></Document>`)
	return sb.String()
}

// pacs010HTTPDate is the UTC date d days from today, YYYY-MM-DD.
func pacs010HTTPDate(days int) string {
	return time.Now().UTC().AddDate(0, 0, days).Format("2006-01-02")
}

// Full flow through HTTP: apply the creditor bank's pacs.010
// document, replay the same MsgId, and the metrics counter moves.
func TestHTTPPacs010Apply(t *testing.T) {
	srv, l := seedPacs010ApplyServer(t)
	applyURL := srv.URL + "/exports/pacs010/apply"
	today := pacs010HTTPDate(0)

	doc := pacs010HTTPDoc("pacs010-http-1", "INDA",
		pacs010HTTPItem{endToEnd: "e2e-http-1", amount: "12.00", ccy: "USD", mandate: "m-pacs010", colltnDt: today},
		pacs010HTTPItem{endToEnd: "e2e-http-2", amount: "25.00", ccy: "USD", mandate: "m-pacs010b", colltnDt: today},
	)
	code, body := postPacs010XML(t, applyURL, doc, "bank-cred")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/pacs010/apply: status=%d body=%v", code, body)
	}
	if body["msg_id"] != "pacs010-http-1" {
		t.Errorf("msg_id = %v, want pacs010-http-1", body["msg_id"])
	}
	results, _ := body["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("results = %v, want 2 items", body["results"])
	}
	first, _ := results[0].(map[string]any)
	if first["end_to_end_id"] != "e2e-http-1" || first["entry_id"] != "e2e-http-1" {
		t.Errorf("first result = %v, want e2e-http-1 booked", first)
	}
	if first["duplicate"] != false {
		t.Errorf("first duplicate = %v, want false", first["duplicate"])
	}
	if first["amount_cents"] != float64(1200) || first["mandate_id"] != "m-pacs010" {
		t.Errorf("first result = %v, want 1200 under m-pacs010", first)
	}
	second, _ := results[1].(map[string]any)
	if second["entry_id"] != "e2e-http-2" || second["amount_cents"] != float64(2500) {
		t.Errorf("second result = %v, want e2e-http-2/2500", second)
	}

	// The ledger booked exactly two debits under the mandates.
	if n := l.MandateDebitCount(); n != 2 {
		t.Errorf("MandateDebitCount = %d, want 2", n)
	}
	if got := l.BalanceIn("merchant", "USD"); got != 3700 {
		t.Errorf("merchant balance = %d, want 3700", got)
	}

	// Replay the same document: a no-op success, duplicates set.
	code, body = postPacs010XML(t, applyURL, doc, "bank-cred")
	if code != http.StatusOK {
		t.Fatalf("replay POST /exports/pacs010/apply: status=%d body=%v", code, body)
	}
	results, _ = body["results"].([]any)
	for i, r := range results {
		if item, _ := r.(map[string]any); item["duplicate"] != true {
			t.Errorf("replay result %d = %v, want duplicate true", i, item)
		}
	}
	if n := l.MandateDebitCount(); n != 2 {
		t.Errorf("MandateDebitCount after replay = %d, want 2", n)
	}

	// Metrics: both applies were counted.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_pacs010_applies_total 2") {
		t.Errorf("metrics exposition missing ledger_pacs010_applies_total 2:\n%s", metricsBody)
	}
}

// Error mapping: a malformed document is a 400, a settlement method
// outside the vocabulary is a 400, a control sum that disagrees with
// the instructions is a 400 — and a mixed batch still answers 200,
// with the uncovered instruction's failure carried in its item.
func TestHTTPPacs010ApplyErrors(t *testing.T) {
	srv, l := seedPacs010ApplyServer(t)
	applyURL := srv.URL + "/exports/pacs010/apply"
	today := pacs010HTTPDate(0)
	good := pacs010HTTPItem{endToEnd: "e2e-err-1", amount: "12.00", ccy: "USD", mandate: "m-pacs010", colltnDt: today}

	if code, body := postPacs010XML(t, applyURL, "<Document><broken", "ops"); code != http.StatusBadRequest {
		t.Errorf("malformed XML: status=%d body=%v, want 400", code, body)
	}
	if code, body := postPacs010XML(t, applyURL, pacs010HTTPDoc("m-2", "XXXX", good), "ops"); code != http.StatusBadRequest {
		t.Errorf("settlement method outside vocabulary: status=%d body=%v, want 400", code, body)
	}
	if code, body := postPacs010XML(t, applyURL, pacs010HTTPDocCtrl("m-3", "INDA", "999.99", good), "ops"); code != http.StatusBadRequest {
		t.Errorf("control sum mismatch: status=%d body=%v, want 400", code, body)
	}

	// Mixed batch: one covered instruction books, the unknown
	// mandate is a per-item error in the 200 body.
	doc := pacs010HTTPDoc("m-4", "COVE",
		good,
		pacs010HTTPItem{endToEnd: "e2e-err-2", amount: "12.00", ccy: "USD", mandate: "no-such-mandate", colltnDt: today},
	)
	code, body := postPacs010XML(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("mixed batch: status=%d body=%v, want 200", code, body)
	}
	results, _ := body["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("mixed batch results = %v, want 2 items", body["results"])
	}
	if first, _ := results[0].(map[string]any); first["entry_id"] != "e2e-err-1" {
		t.Errorf("mixed batch first result = %v, want booked e2e-err-1", first)
	}
	if second, _ := results[1].(map[string]any); second["error"] == nil || second["error"] == "" {
		t.Errorf("mixed batch second result = %v, want a per-item error", second)
	}
	if n := l.MandateDebitCount(); n != 1 {
		t.Errorf("MandateDebitCount = %d, want 1", n)
	}
}

// The inbound half leaves the outbound half alone: after an apply,
// GET /exports/pacs010 still renders the booked collections and GET
// /mandates/{id}/pacs010/reconcile still reports them pending.
func TestHTTPPacs010ApplyLeavesExportIntact(t *testing.T) {
	srv, l := seedPacs010ApplyServer(t)
	applyURL := srv.URL + "/exports/pacs010/apply"
	today := pacs010HTTPDate(0)

	doc := pacs010HTTPDoc("pacs010-http-2", "INDA",
		pacs010HTTPItem{endToEnd: "e2e-http-3", amount: "12.00", ccy: "USD", mandate: "m-pacs010", colltnDt: today},
	)
	if code, body := postPacs010XML(t, applyURL, doc, "bank-cred"); code != http.StatusOK {
		t.Fatalf("POST /exports/pacs010/apply: status=%d body=%v", code, body)
	}
	// The export needs registered IBANs for both mandate parties.
	if err := l.SetAccountIBAN("cust", "GB29NWBK60161331926819", "NWBKGB2L"); err != nil {
		t.Fatalf("SetAccountIBAN cust: %v", err)
	}
	if err := l.SetAccountIBAN("merchant", "DE89370400440532013000", "DEUTDEFF"); err != nil {
		t.Fatalf("SetAccountIBAN merchant: %v", err)
	}

	code, _, exportBody := getBody(t, srv.URL+"/exports/pacs010?mandate=m-pacs010")
	if code != http.StatusOK {
		t.Fatalf("GET /exports/pacs010: status=%d body=%s", code, exportBody)
	}
	for _, want := range []string{
		"<MndtId>m-pacs010</MndtId>",
		"<EndToEndId>e2e-http-3</EndToEndId>",
		`<InstdAmt Ccy="USD">12.00</InstdAmt>`,
	} {
		if !strings.Contains(exportBody, want) {
			t.Errorf("export missing %q:\n%s", want, exportBody)
		}
	}

	code, _, reconJSON := getBody(t, srv.URL+"/mandates/m-pacs010/pacs010/reconcile")
	if code != http.StatusOK {
		t.Fatalf("GET /mandates/m-pacs010/pacs010/reconcile: status=%d body=%s", code, reconJSON)
	}
	var recon map[string]any
	if err := json.Unmarshal([]byte(reconJSON), &recon); err != nil {
		t.Fatalf("decode reconcile body: %v", err)
	}
	if recon["pending"] != float64(1) {
		t.Errorf("reconcile body = %v, want pending 1", recon)
	}
}
