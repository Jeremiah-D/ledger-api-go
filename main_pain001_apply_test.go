package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// pain001ApplyTestServer builds a server with a funded payer and
// IBANs on both sides, ready for pain.001 initiation through HTTP.
func pain001ApplyTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	l := ledger.New()
	if _, _, err := l.Post(ledger.JournalEntry{
		ID: "fund-cust", DebitAccount: "cust", CreditAccount: "equity",
		AmountCents: 1_000_000, Currency: "USD", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed Post: %v", err)
	}
	if err := l.SetAccountIBAN("cust", "DE89370400440532013000", "DEUTDEFF"); err != nil {
		t.Fatalf("SetAccountIBAN cust: %v", err)
	}
	if err := l.SetAccountIBAN("merchant", "GB29NWBK60161331926819", "NWBKGB2L"); err != nil {
		t.Fatalf("SetAccountIBAN merchant: %v", err)
	}
	srv := httptest.NewServer(newRouter(l))
	t.Cleanup(srv.Close)
	return srv
}

// postPain001XML posts a raw pain.001 XML document to
// /exports/pain001/apply with an x-ledger-actor header.
func postPain001XML(t *testing.T, url, xmlDoc, actor string) (int, map[string]any) {
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

// pain001ApplyTx is one CdtTrfTxInf of a test document.
type pain001ApplyTx struct {
	endToEnd string
	amount   string
	ccy      string
	cdtrIBAN string
}

// pain001ApplyDoc renders one inbound pain.001.001.03 document:
// msgID in GrpHdr (NbOfTxs and CtrlSum computed from the
// transactions), one PmtInf (PmtMtd TRF, ReqdExctnDt today, Dbtr
// cust), one CdtTrfTxInf per transaction. version selects the
// namespace (001.03 valid, anything else the wrong schema version).
func pain001ApplyDoc(t *testing.T, version, msgID string, txs []pain001ApplyTx) string {
	t.Helper()
	var sb strings.Builder
	today := time.Now().UTC().Format("2006-01-02")
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pain.001.` + version + `">` + "\n")
	sb.WriteString("<CstmrCdtTrfInitn>\n<GrpHdr>\n")
	sb.WriteString("<MsgId>" + msgID + "</MsgId>\n")
	sb.WriteString("<CreDtTm>2026-11-15T10:00:00Z</CreDtTm>\n")
	var sum float64
	for _, tx := range txs {
		v, err := strconv.ParseFloat(tx.amount, 64)
		if err != nil {
			t.Fatalf("test amount %q: %v", tx.amount, err)
		}
		sum += v
	}
	sb.WriteString(fmt.Sprintf("<NbOfTxs>%d</NbOfTxs>\n", len(txs)))
	sb.WriteString(fmt.Sprintf("<CtrlSum>%.2f</CtrlSum>\n", sum))
	sb.WriteString("<InitgPty><Nm>LEDGER</Nm></InitgPty>\n</GrpHdr>\n")
	sb.WriteString("<PmtInf>\n")
	sb.WriteString(fmt.Sprintf("<PmtInfId>%s-%s</PmtInfId>\n", msgID, today))
	sb.WriteString("<PmtMtd>TRF</PmtMtd>\n")
	sb.WriteString(fmt.Sprintf("<NbOfTxs>%d</NbOfTxs>\n", len(txs)))
	sb.WriteString(fmt.Sprintf("<CtrlSum>%.2f</CtrlSum>\n", sum))
	sb.WriteString(fmt.Sprintf("<ReqdExctnDt>%s</ReqdExctnDt>\n", today))
	sb.WriteString("<Dbtr><Nm>cust</Nm></Dbtr>\n")
	sb.WriteString("<DbtrAcct><Id><IBAN>DE89370400440532013000</IBAN></Id></DbtrAcct>\n")
	sb.WriteString("<DbtrAgt><FinInstnId><BIC>DEUTDEFF</BIC></FinInstnId></DbtrAgt>\n")
	for _, tx := range txs {
		cdtrIBAN := tx.cdtrIBAN
		if cdtrIBAN == "" {
			cdtrIBAN = "GB29NWBK60161331926819"
		}
		sb.WriteString("<CdtTrfTxInf>\n")
		sb.WriteString(fmt.Sprintf("<PmtId><EndToEndId>%s</EndToEndId></PmtId>\n", tx.endToEnd))
		sb.WriteString(fmt.Sprintf("<Amt><InstdAmt Ccy=\"%s\">%s</InstdAmt></Amt>\n", tx.ccy, tx.amount))
		sb.WriteString("<Cdtr><Nm>merchant</Nm></Cdtr>\n")
		sb.WriteString(fmt.Sprintf("<CdtrAcct><Id><IBAN>%s</IBAN></Id></CdtrAcct>\n", cdtrIBAN))
		sb.WriteString("<CdtrAgt><FinInstnId><BIC>NWBKGB2L</BIC></FinInstnId></CdtrAgt>\n")
		sb.WriteString("</CdtTrfTxInf>\n")
	}
	sb.WriteString("</PmtInf>\n</CstmrCdtTrfInitn>\n</Document>\n")
	return sb.String()
}

// pain001ApplyValidDoc is the happy-path document: msg-001-http-1
// pays merchant USD 100.00 (e-http-1) from cust, today.
func pain001ApplyValidDoc(t *testing.T) string {
	t.Helper()
	return pain001ApplyDoc(t, "001.03", "msg-001-http-1",
		[]pain001ApplyTx{{endToEnd: "e-http-1", amount: "100.00", ccy: "USD"}})
}

// pain001ApplyFirstResult extracts the first item from an apply
// response body.
func pain001ApplyFirstResult(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	results, ok := body["results"].([]any)
	if !ok || len(results) == 0 {
		t.Fatalf("response has no results: %v", body)
	}
	return results[0].(map[string]any)
}

// Full flow through HTTP: apply -> the entry books, replay is
// idempotent, the metrics counter moves, and the booked transfer
// flows through the existing export / receipts / settle lifecycle
// untouched.
func TestHTTPPain001Apply(t *testing.T) {
	srv := pain001ApplyTestServer(t)
	applyURL := srv.URL + "/exports/pain001/apply"

	code, body := postPain001XML(t, applyURL, pain001ApplyValidDoc(t), "ops")
	if code != http.StatusOK {
		t.Fatalf("apply: status=%d body=%v, want 200", code, body)
	}
	if body["msg_id"] != "msg-001-http-1" {
		t.Errorf("msg_id = %v, want msg-001-http-1", body["msg_id"])
	}
	first := pain001ApplyFirstResult(t, body)
	if first["entry_id"] != "e-http-1" {
		t.Errorf("entry_id = %v, want e-http-1", first["entry_id"])
	}
	if first["duplicate"] == true {
		t.Error("duplicate = true on first apply, want false")
	}
	if _, hasErr := first["error"]; hasErr {
		t.Errorf("first result has error: %v", first)
	}

	// Replay the same MsgId: the original receipt, duplicate set,
	// nothing booked twice.
	code, body = postPain001XML(t, applyURL, pain001ApplyValidDoc(t), "ops")
	if code != http.StatusOK {
		t.Fatalf("replay: status=%d body=%v, want 200", code, body)
	}
	if body["duplicate"] != true {
		t.Errorf("replay duplicate = %v, want true", body["duplicate"])
	}
	first = pain001ApplyFirstResult(t, body)
	if first["entry_id"] != "e-http-1" {
		t.Errorf("replay entry_id = %v, want e-http-1", first["entry_id"])
	}

	// The metrics counter counts both applies.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_pain001_applies_total 2") {
		t.Errorf("metrics exposition missing ledger_pain001_applies_total 2:\n%s", metricsBody)
	}

	// The booked transfer exports again through GET /exports/pain001.
	code, _, exportBody := getBody(t, srv.URL+"/exports/pain001?currency=USD")
	if code != http.StatusOK {
		t.Fatalf("GET /exports/pain001: status=%d", code)
	}
	if !strings.Contains(exportBody, "e-http-1") {
		t.Errorf("GET /exports/pain001 missing booked transfer e-http-1:\n%s", exportBody)
	}

	// The inbound file's instruction answers bank receipts and
	// settles exactly like an exported instruction.
	code, rec := postJSON(t, srv.URL+"/exports/pain001/msg-001-http-1/receipts",
		`{"receipts":[{"tx_id":"e-http-1","status":"accepted"}]}`)
	if code != http.StatusOK {
		t.Fatalf("POST receipts: status=%d body=%v", code, rec)
	}
	if rec["state"] != "accepted" {
		t.Errorf("receipts state = %v, want accepted", rec["state"])
	}
	code, rec = postJSONWithActor(t, srv.URL+"/exports/pain001/msg-001-http-1/settle", `{}`, "night-ops")
	if code != http.StatusOK {
		t.Fatalf("POST settle: status=%d body=%v", code, rec)
	}
	if rec["state"] != "settled" {
		t.Errorf("settle state = %v, want settled", rec["state"])
	}
	code, _, statusBody := getBody(t, srv.URL+"/exports/pain001/msg-001-http-1/status")
	if code != http.StatusOK {
		t.Fatalf("GET status: status=%d", code)
	}
	if !strings.Contains(statusBody, "settled") {
		t.Errorf("final status body missing settled:\n%s", statusBody)
	}
}

// Error mapping: a malformed document is a 400; a wrong-namespace
// document parks in the repair queue, garbage names nothing and
// does not; a MsgId reused for different terms is a 422.
func TestHTTPPain001ApplyErrors(t *testing.T) {
	srv := pain001ApplyTestServer(t)
	applyURL := srv.URL + "/exports/pain001/apply"

	bad := pain001ApplyDoc(t, "001.09", "msg-001-http-ns",
		[]pain001ApplyTx{{endToEnd: "e-http-ns", amount: "10.00", ccy: "USD"}})
	code, body := postPain001XML(t, applyURL, bad, "ops")
	if code != http.StatusBadRequest {
		t.Errorf("wrong namespace: status=%d body=%v, want 400", code, body)
	}
	if body["repair_id"] == nil || body["repair_id"] == "" {
		t.Errorf("wrong namespace: no repair_id in body %v", body)
	}
	code, body = postPain001XML(t, applyURL, "not xml at all", "ops")
	if code != http.StatusBadRequest {
		t.Errorf("garbage: status=%d body=%v, want 400", code, body)
	}
	if _, hasRepair := body["repair_id"]; hasRepair {
		t.Errorf("garbage: unexpected repair_id in body %v", body)
	}

	// Apply once, then reuse the MsgId for a different amount.
	code, body = postPain001XML(t, applyURL, pain001ApplyValidDoc(t), "ops")
	if code != http.StatusOK {
		t.Fatalf("apply: status=%d body=%v", code, body)
	}
	other := pain001ApplyDoc(t, "001.03", "msg-001-http-1",
		[]pain001ApplyTx{{endToEnd: "e-http-1", amount: "99.00", ccy: "USD"}})
	code, body = postPain001XML(t, applyURL, other, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Errorf("MsgId conflict: status=%d body=%v, want 422", code, body)
	}
}

// A per-transaction failure stays a 200 with the item's error and
// repair record: an unknown creditor IBAN parks, and the endpoint
// does not fail the whole file.
func TestHTTPPain001ApplyPerItemFailure(t *testing.T) {
	srv := pain001ApplyTestServer(t)
	applyURL := srv.URL + "/exports/pain001/apply"

	doc := pain001ApplyDoc(t, "001.03", "msg-001-http-partial",
		[]pain001ApplyTx{
			{endToEnd: "e-http-ok", amount: "10.00", ccy: "USD"},
			{endToEnd: "e-http-unk", amount: "20.00", ccy: "USD", cdtrIBAN: "FR1420041010050500013M02606"},
		})
	code, body := postPain001XML(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("apply: status=%d body=%v, want 200", code, body)
	}
	results, ok := body["results"].([]any)
	if !ok || len(results) != 2 {
		t.Fatalf("results = %v, want 2 items", body["results"])
	}
	good := results[0].(map[string]any)
	if good["entry_id"] != "e-http-ok" {
		t.Errorf("good entry_id = %v, want e-http-ok", good["entry_id"])
	}
	bad := results[1].(map[string]any)
	if bad["error"] == nil || bad["error"] == "" {
		t.Errorf("bad item has no error: %v", bad)
	}
	if bad["repair_id"] == nil || bad["repair_id"] == "" {
		t.Errorf("bad item has no repair_id: %v", bad)
	}
	if bad["entry_id"] != nil && bad["entry_id"] != "" {
		t.Errorf("bad item has entry_id %v, want none", bad["entry_id"])
	}
}
