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

// seedPacs003ApplyServer creates a server with a funded payer and one
// mandate m-pacs003 (cust -> merchant, 5000 USD cap, per-day) whose
// window spans today by a wide margin, so a collection date of today
// — or a few days either way — is inside it unless the test moves it
// outside on purpose. It returns the server and the underlying ledger
// for direct setup.
func seedPacs003ApplyServer(t *testing.T) (*httptest.Server, *ledger.Ledger) {
	t.Helper()
	l := ledger.New()
	srv := httptest.NewServer(newRouter(l))
	t.Cleanup(srv.Close)
	code, _ := postJSON(t, srv.URL+"/entries", `{"debit_account":"cust","credit_account":"bank","amount_cents":100000,"currency":"USD"}`)
	if code != http.StatusCreated {
		t.Fatalf("seed fund status = %d, want 201", code)
	}
	now := time.Now().UTC()
	code, body := postJSON(t, srv.URL+"/mandates", fmt.Sprintf(`{"id":"m-pacs003","payer":"cust","payee":"merchant","max_amount_cents":5000,"currency":"USD","frequency":"per-day","valid_from":%q,"valid_until":%q}`,
		now.Add(-48*time.Hour).Format(time.RFC3339), now.Add(30*24*time.Hour).Format(time.RFC3339)))
	if code != http.StatusCreated {
		t.Fatalf("seed mandate status = %d, want 201", code)
	}
	if body["status"] != "active" {
		t.Fatalf("seed mandate status = %v, want active", body)
	}
	return srv, l
}

// postPacs003XML posts a raw pacs.003 XML document to
// /exports/pacs003/apply with an x-ledger-actor header.
func postPacs003XML(t *testing.T, url, xmlDoc, actor string) (int, map[string]any) {
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

// pacs003HTTPDoc renders a pacs.003.001.08 instruction for mandate
// mndtID, collecting amount in ccy on colltnDt (YYYY-MM-DD).
func pacs003HTTPDoc(msgID, endToEndID, mndtID, amount, ccy, colltnDt string) string {
	var sb strings.Builder
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pacs.003.001.08">`)
	sb.WriteString(`<FIToFICstmrDrctDbt><GrpHdr><MsgId>` + msgID + `</MsgId>`)
	sb.WriteString(`<CreDtTm>2026-11-13T10:00:00Z</CreDtTm><NbOfTxs>1</NbOfTxs>`)
	sb.WriteString(`<IntrBkSttlmDt>` + colltnDt + `</IntrBkSttlmDt>`)
	sb.WriteString(`<InitgPty><FinInstnId><Othr><Id>BANKCRED</Id></Othr></FinInstnId></InitgPty></GrpHdr>`)
	sb.WriteString(`<DrctDbtTxInf><PmtId><InstrId>` + endToEndID + `</InstrId>`)
	sb.WriteString(`<EndToEndId>` + endToEndID + `</EndToEndId>`)
	sb.WriteString(`<TxId>` + endToEndID + `</TxId></PmtId>`)
	sb.WriteString(`<InstdAmt Ccy="` + ccy + `">` + amount + `</InstdAmt>`)
	sb.WriteString(`<DrctDbtTx><MndtRltdInf><MndtId>` + mndtID + `</MndtId>`)
	sb.WriteString(`<DtOfSgntr>2026-01-15</DtOfSgntr></MndtRltdInf></DrctDbtTx>`)
	sb.WriteString(`<CdtrAgt><FinInstnId><Othr><Id>BANKCRED</Id></Othr></FinInstnId></CdtrAgt>`)
	sb.WriteString(`<Cdtr><Id><PrvtId><Othr><Id>merchant</Id></Othr></PrvtId></Id></Cdtr>`)
	sb.WriteString(`<CdtrAcct><Id><Othr><Id>merchant</Id></Othr></Id></CdtrAcct>`)
	sb.WriteString(`<DbtrAgt><FinInstnId><Othr><Id>BANKDEBB</Id></Othr></FinInstnId></DbtrAgt>`)
	sb.WriteString(`<Dbtr><Id><PrvtId><Othr><Id>cust</Id></Othr></PrvtId></Id></Dbtr>`)
	sb.WriteString(`<DbtrAcct><Id><Othr><Id>cust</Id></Othr></Id></DbtrAcct>`)
	sb.WriteString(`</DrctDbtTxInf></FIToFICstmrDrctDbt></Document>`)
	return sb.String()
}

// pacs003HTTPDate is the UTC date d days from today, YYYY-MM-DD.
func pacs003HTTPDate(days int) string {
	return time.Now().UTC().AddDate(0, 0, days).Format("2006-01-02")
}

// Full flow through HTTP: apply the creditor bank's pacs.003
// instruction, replay the same MsgId, and the metrics counter moves.
func TestHTTPPacs003Apply(t *testing.T) {
	srv, l := seedPacs003ApplyServer(t)
	applyURL := srv.URL + "/exports/pacs003/apply"
	today := pacs003HTTPDate(0)

	doc := pacs003HTTPDoc("pacs003-http-1", "e2e-http-1", "m-pacs003", "12.00", "USD", today)
	code, body := postPacs003XML(t, applyURL, doc, "bank-cred")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/pacs003/apply: status=%d body=%v", code, body)
	}
	if body["mandate_id"] != "m-pacs003" {
		t.Errorf("mandate_id = %v, want m-pacs003", body["mandate_id"])
	}
	if body["duplicate"] != false {
		t.Errorf("duplicate = %v, want false", body["duplicate"])
	}
	entry, _ := body["entry"].(map[string]any)
	if entry["id"] != "e2e-http-1" {
		t.Errorf("entry id = %v, want the EndToEndId e2e-http-1", entry["id"])
	}
	if entry["debit_account"] != "merchant" || entry["credit_account"] != "cust" {
		t.Errorf("entry legs = %v, want merchant/cust", entry)
	}
	if entry["amount_cents"] != float64(1200) {
		t.Errorf("entry amount = %v, want 1200", entry["amount_cents"])
	}
	if entry["memo"] != "pacs003:pacs003-http-1 mandate_of:m-pacs003" {
		t.Errorf("entry memo = %v, want pacs003:pacs003-http-1 mandate_of:m-pacs003", entry["memo"])
	}

	// The ledger booked exactly one debit under the mandate.
	if n := l.MandateDebitCount(); n != 1 {
		t.Errorf("MandateDebitCount = %d, want 1", n)
	}

	// Replay the same instruction: a no-op success, duplicate set.
	code, body = postPacs003XML(t, applyURL, doc, "bank-cred")
	if code != http.StatusOK {
		t.Fatalf("replay POST /exports/pacs003/apply: status=%d body=%v", code, body)
	}
	if body["duplicate"] != true {
		t.Errorf("replay body = %v, want duplicate true", body)
	}
	if n := l.MandateDebitCount(); n != 1 {
		t.Errorf("MandateDebitCount after replay = %d, want 1", n)
	}

	// Metrics: both applies were counted.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_pacs003_applies_total 2") {
		t.Errorf("metrics exposition missing ledger_pacs003_applies_total 2:\n%s", metricsBody)
	}
}

// Error mapping: malformed XML is a 400, an unknown mandate is a
// 404, and the mandate gates — over-cap amount, currency mismatch,
// collection date outside the mandate window — are 422s.
func TestHTTPPacs003ApplyErrors(t *testing.T) {
	srv, _ := seedPacs003ApplyServer(t)
	applyURL := srv.URL + "/exports/pacs003/apply"
	today := pacs003HTTPDate(0)

	if code, body := postPacs003XML(t, applyURL, "<Document><broken", "ops"); code != http.StatusBadRequest {
		t.Errorf("malformed XML: status=%d body=%v, want 400", code, body)
	}
	if code, body := postPacs003XML(t, applyURL, pacs003HTTPDoc("m-1", "e2e-err-1", "no-such-mandate", "12.00", "USD", today), "ops"); code != http.StatusNotFound {
		t.Errorf("unknown mandate: status=%d body=%v, want 404", code, body)
	}
	if code, body := postPacs003XML(t, applyURL, pacs003HTTPDoc("m-2", "e2e-err-2", "m-pacs003", "50.01", "USD", today), "ops"); code != http.StatusUnprocessableEntity {
		t.Errorf("over-cap amount: status=%d body=%v, want 422", code, body)
	}
	if code, body := postPacs003XML(t, applyURL, pacs003HTTPDoc("m-3", "e2e-err-3", "m-pacs003", "12.00", "EUR", today), "ops"); code != http.StatusUnprocessableEntity {
		t.Errorf("currency mismatch: status=%d body=%v, want 422", code, body)
	}
	if code, body := postPacs003XML(t, applyURL, pacs003HTTPDoc("m-4", "e2e-err-4", "m-pacs003", "12.00", "USD", pacs003HTTPDate(31)), "ops"); code != http.StatusUnprocessableEntity {
		t.Errorf("collection after ValidUntil: status=%d body=%v, want 422", code, body)
	}
}

// The inbound half leaves the outbound half alone: after an apply,
// GET /exports/pacs003 still renders the posted transfer as a
// pacs.003 instruction.
func TestHTTPPacs003ApplyLeavesExportIntact(t *testing.T) {
	srv, l := seedPacs003ApplyServer(t)
	applyURL := srv.URL + "/exports/pacs003/apply"

	doc := pacs003HTTPDoc("pacs003-http-2", "e2e-http-2", "m-pacs003", "12.00", "USD", pacs003HTTPDate(0))
	if code, body := postPacs003XML(t, applyURL, doc, "bank-cred"); code != http.StatusOK {
		t.Fatalf("POST /exports/pacs003/apply: status=%d body=%v", code, body)
	}

	// A posted transfer to export: alice -> bob 2500 USD.
	if _, _, err := l.Post(ledger.JournalEntry{ID: "fund-alice", DebitAccount: "alice", CreditAccount: "bank", AmountCents: 10000, Currency: "USD"}); err != nil {
		t.Fatalf("fund alice: %v", err)
	}
	if _, err := l.PostTransfer(ledger.Transfer{ID: "p3-tx-http", From: "alice", To: "bob", AmountCents: 2500}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	code, _, exportBody := getBody(t, srv.URL+"/exports/pacs003?transfer=p3-tx-http&dbtr_agt=BANKDEBB&cdtr_agt=BANKCRED&mndt_id=m-pacs003&mndt_dt=2026-01-15")
	if code != http.StatusOK {
		t.Fatalf("GET /exports/pacs003: status=%d body=%s", code, exportBody)
	}
	for _, want := range []string{
		"<MsgId>p3-tx-http/drctdbt</MsgId>",
		"<MndtId>m-pacs003</MndtId>",
		`<InstdAmt Ccy="USD">25.00</InstdAmt>`,
	} {
		if !strings.Contains(exportBody, want) {
			t.Errorf("export missing %q:\n%s", want, exportBody)
		}
	}
	// The export must not carry the inbound settlement date: the
	// outbound shape is unchanged by the new inbound field.
	if strings.Contains(exportBody, "IntrBkSttlmDt") {
		t.Errorf("export gained an IntrBkSttlmDt element:\n%s", exportBody)
	}
}
