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

// pain008InitiateTestServer builds a server with IBANs and two live
// per-day mandates (m-init, m-init2) with no booked debits — ready
// for pain.008 initiation through HTTP.
func pain008InitiateTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	l := ledger.New()
	if _, _, err := l.Post(ledger.JournalEntry{
		ID: "fund-cust", DebitAccount: "cust", CreditAccount: "equity",
		AmountCents: 1_000_000, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed Post: %v", err)
	}
	if err := l.SetAccountIBAN("cust", "DE89370400440532013000", "DEUTDEFF"); err != nil {
		t.Fatalf("SetAccountIBAN cust: %v", err)
	}
	if err := l.SetAccountIBAN("merchant", "GB29NWBK60161331926819", "NWBKGB2L"); err != nil {
		t.Fatalf("SetAccountIBAN merchant: %v", err)
	}
	now := time.Now()
	for _, id := range []string{"m-init", "m-init2"} {
		if _, err := l.CreateMandate(ledger.Mandate{
			ID: id, Payer: "cust", Payee: "merchant",
			MaxAmountCents: 500000, Currency: "USD",
			Frequency:  ledger.MandateFrequencyPerDay,
			ValidFrom:  now.Add(-48 * time.Hour),
			ValidUntil: now.Add(30 * 24 * time.Hour),
		}); err != nil {
			t.Fatalf("CreateMandate %s: %v", id, err)
		}
	}
	srv := httptest.NewServer(newRouter(l))
	t.Cleanup(srv.Close)
	return srv
}

// postPain008XML posts a raw pain.008 XML document to
// /exports/pain008/initiate with an x-ledger-actor header.
func postPain008XML(t *testing.T, url, xmlDoc, actor string) (int, map[string]any) {
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

// pain008InitiateTx is one DrctDbtTxInf of a test document.
type pain008InitiateTx struct {
	endToEnd string
	amount   string
	ccy      string
	mandate  string
}

// pain008InitiateDoc renders one inbound pain.008.001.02 document:
// msgID in GrpHdr, one PmtInf per (seqTp, mandate) group — each
// group carries its own SeqTp and scheme — one DrctDbtTxInf per
// transaction. version selects the namespace (001.02 valid,
// anything else the wrong schema version).
func pain008InitiateDoc(t *testing.T, version, msgID string, groups map[string][]pain008InitiateTx) string {
	t.Helper()
	var sb strings.Builder
	today := time.Now().UTC().Format("2006-01-02")
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pain.008.` + version + `">` + "\n")
	sb.WriteString("<CstmrDrctDbtInitn>\n<GrpHdr>\n")
	sb.WriteString("<MsgId>" + msgID + "</MsgId>\n")
	sb.WriteString("<CreDtTm>2026-11-14T10:00:00Z</CreDtTm>\n")
	total := 0
	var sum float64
	for _, txs := range groups {
		for _, tx := range txs {
			total++
			v, err := strconv.ParseFloat(tx.amount, 64)
			if err != nil {
				t.Fatalf("test amount %q: %v", tx.amount, err)
			}
			sum += v
		}
	}
	sb.WriteString(fmt.Sprintf("<NbOfTxs>%d</NbOfTxs>\n", total))
	sb.WriteString(fmt.Sprintf("<CtrlSum>%.2f</CtrlSum>\n", sum))
	sb.WriteString("<InitgPty><Nm>LEDGER</Nm></InitgPty>\n</GrpHdr>\n")
	// Deterministic group order: sort the keys (seqTp/mandate).
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	for _, key := range keys {
		parts := strings.SplitN(key, "/", 2)
		seqTp, mandate := parts[0], parts[1]
		txs := groups[key]
		var gsum float64
		for _, tx := range txs {
			v, err := strconv.ParseFloat(tx.amount, 64)
			if err != nil {
				t.Fatalf("test amount %q: %v", tx.amount, err)
			}
			gsum += v
		}
		sb.WriteString("<PmtInf>\n")
		sb.WriteString(fmt.Sprintf("<PmtInfId>%s-%s-%s</PmtInfId>\n", msgID, seqTp, today))
		sb.WriteString("<PmtMtd>DD</PmtMtd>\n")
		sb.WriteString(fmt.Sprintf("<NbOfTxs>%d</NbOfTxs>\n", len(txs)))
		sb.WriteString(fmt.Sprintf("<CtrlSum>%.2f</CtrlSum>\n", gsum))
		sb.WriteString("<PmtTpInf>\n")
		sb.WriteString("<SvcLvl><Cd>SEPA</Cd></SvcLvl>\n")
		sb.WriteString("<LclInstrm><Cd>CORE</Cd></LclInstrm>\n")
		sb.WriteString(fmt.Sprintf("<SeqTp>%s</SeqTp>\n", seqTp))
		sb.WriteString("</PmtTpInf>\n")
		sb.WriteString(fmt.Sprintf("<ReqdColltnDt>%s</ReqdColltnDt>\n", today))
		sb.WriteString("<Cdtr><Nm>merchant</Nm></Cdtr>\n")
		sb.WriteString("<CdtrAcct><Id><IBAN>GB29NWBK60161331926819</IBAN></Id></CdtrAcct>\n")
		sb.WriteString("<CdtrAgt><FinInstnId><BIC>NWBKGB2L</BIC></FinInstnId></CdtrAgt>\n")
		sb.WriteString("<ChrgBr>SLEV</ChrgBr>\n")
		sb.WriteString("<CdtrSchmeId><Id><PrtryId><Id>DE98ZZZ09999999999</Id></PrtryId></Id></CdtrSchmeId>\n")
		for _, tx := range txs {
			sb.WriteString("<DrctDbtTxInf>\n")
			sb.WriteString(fmt.Sprintf("<PmtId><InstrId>%s</InstrId><EndToEndId>%s</EndToEndId></PmtId>\n", tx.endToEnd, tx.endToEnd))
			sb.WriteString(fmt.Sprintf("<InstdAmt Ccy=\"%s\">%s</InstdAmt>\n", tx.ccy, tx.amount))
			sb.WriteString("<DrctDbtTx>\n<MndtRltdInf>\n")
			sb.WriteString(fmt.Sprintf("<MndtId>%s</MndtId>\n", mandate))
			sb.WriteString("<DtOfSgntr>2026-01-01</DtOfSgntr>\n")
			sb.WriteString("<AmdmntInd>false</AmdmntInd>\n")
			sb.WriteString("</MndtRltdInf>\n</DrctDbtTx>\n")
			sb.WriteString("<DbtrAgt><FinInstnId><BIC>DEUTDEFF</BIC></FinInstnId></DbtrAgt>\n")
			sb.WriteString("<Dbtr><Nm>cust</Nm></Dbtr>\n")
			sb.WriteString("<DbtrAcct><Id><IBAN>DE89370400440532013000</IBAN></Id></DbtrAcct>\n")
			sb.WriteString("</DrctDbtTxInf>\n")
		}
		sb.WriteString("</PmtInf>\n")
	}
	sb.WriteString("</CstmrDrctDbtInitn>\n</Document>\n")
	return sb.String()
}

// pain008InitiateValidDoc is the happy-path document:
// pain008-in-http-1 collects USD 10.00 under m-init, first
// collection, today. The MsgId carries the "pain008-" prefix so
// the file is also answerable through the full pain.002 parse
// path (LG-406 pairs the pain.008 name with that namespace).
func pain008InitiateValidDoc(t *testing.T) string {
	t.Helper()
	return pain008InitiateDoc(t, "001.02", "pain008-in-http-1", map[string][]pain008InitiateTx{
		"FRST/m-init": {{endToEnd: "e-http-1", amount: "10.00", ccy: "USD", mandate: "m-init"}},
	})
}

// pain008InitiateFirstResult extracts the first item from an
// initiate response body.
func pain008InitiateFirstResult(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	results, ok := body["results"].([]any)
	if !ok || len(results) == 0 {
		t.Fatalf("response has no results: %v", body)
	}
	return results[0].(map[string]any)
}

// Full flow through HTTP: initiate -> the entry books, replay is
// idempotent, and the metrics counter moves.
func TestHTTPPain008Initiate(t *testing.T) {
	srv := pain008InitiateTestServer(t)

	code, body := postPain008XML(t, srv.URL+"/exports/pain008/initiate", pain008InitiateValidDoc(t), "ops")
	if code != http.StatusOK {
		t.Fatalf("initiate: status=%d body=%v, want 200", code, body)
	}
	if body["msg_id"] != "pain008-in-http-1" {
		t.Errorf("msg_id = %v, want pain008-in-http-1", body["msg_id"])
	}
	first := pain008InitiateFirstResult(t, body)
	if first["entry_id"] != "e-http-1" {
		t.Errorf("entry_id = %v, want e-http-1", first["entry_id"])
	}
	if first["duplicate"] == true {
		t.Error("duplicate = true on first initiate, want false")
	}
	if _, hasErr := first["error"]; hasErr {
		t.Errorf("first result has error: %v", first)
	}

	// Replay the same MsgId: the original receipt, duplicate set,
	// nothing booked twice.
	code, body = postPain008XML(t, srv.URL+"/exports/pain008/initiate", pain008InitiateValidDoc(t), "ops")
	if code != http.StatusOK {
		t.Fatalf("replay: status=%d body=%v, want 200", code, body)
	}
	if body["duplicate"] != true {
		t.Errorf("replay duplicate = %v, want true", body["duplicate"])
	}
	first = pain008InitiateFirstResult(t, body)
	if first["entry_id"] != "e-http-1" {
		t.Errorf("replay entry_id = %v, want e-http-1", first["entry_id"])
	}

	// The metrics counter counts both initiates.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_pain008_initiates_total 2") {
		t.Errorf("metrics exposition missing ledger_pain008_initiates_total 2:\n%s", metricsBody)
	}

	// The booked entry is visible through the entries API, and the
	// instruction answers a pain.002 report: the inbound file is
	// registered under its wire MsgId.
	code, body = postPain002XML(t, srv.URL+"/exports/pain008/apply", pain008HTTPDoc("pain008-in-http-1", [3]string{"e-http-1", "ACSC", ""}), "bank")
	if code != http.StatusOK {
		t.Fatalf("pain.002 apply: status=%d body=%v, want 200", code, body)
	}
	if body["state"] != "accepted" {
		t.Errorf("pain.002 state = %v, want accepted", body["state"])
	}
}

// A malformed document is a 400; a wrong-namespace document parks
// in the repair queue, garbage names nothing and does not.
func TestHTTPPain008InitiateErrors(t *testing.T) {
	srv := pain008InitiateTestServer(t)

	bad := pain008InitiateDoc(t, "001.01", "msg-008-http-ns", map[string][]pain008InitiateTx{
		"FRST/m-init": {{endToEnd: "e-http-ns", amount: "10.00", ccy: "USD", mandate: "m-init"}},
	})
	code, body := postPain008XML(t, srv.URL+"/exports/pain008/initiate", bad, "ops")
	if code != http.StatusBadRequest {
		t.Errorf("wrong namespace: status=%d body=%v, want 400", code, body)
	}
	if body["repair_id"] == nil || body["repair_id"] == "" {
		t.Errorf("wrong namespace: no repair_id in body %v", body)
	}
	code, body = postPain008XML(t, srv.URL+"/exports/pain008/initiate", "not xml at all", "ops")
	if code != http.StatusBadRequest {
		t.Errorf("garbage: status=%d body=%v, want 400", code, body)
	}
	if _, hasRepair := body["repair_id"]; hasRepair {
		t.Errorf("garbage: unexpected repair_id in body %v", body)
	}
}

// A file naming two mandates is a 422 with a repair record, and
// books nothing.
func TestHTTPPain008InitiateMultiMandate(t *testing.T) {
	srv := pain008InitiateTestServer(t)

	doc := pain008InitiateDoc(t, "001.02", "msg-008-http-multi", map[string][]pain008InitiateTx{
		"FRST/m-init":  {{endToEnd: "e-http-m1", amount: "10.00", ccy: "USD", mandate: "m-init"}},
		"FRST/m-init2": {{endToEnd: "e-http-m2", amount: "20.00", ccy: "USD", mandate: "m-init2"}},
	})
	code, body := postPain008XML(t, srv.URL+"/exports/pain008/initiate", doc, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Errorf("multi-mandate: status=%d body=%v, want 422", code, body)
	}
	if body["repair_id"] == nil || body["repair_id"] == "" {
		t.Errorf("multi-mandate: no repair_id in body %v", body)
	}
}

// A per-transaction failure stays a 200 with the item's error and
// repair record: an unknown mandate parks, and the endpoint does
// not fail the whole file.
func TestHTTPPain008InitiatePerItemFailure(t *testing.T) {
	srv := pain008InitiateTestServer(t)

	doc := pain008InitiateDoc(t, "001.02", "msg-008-http-unk", map[string][]pain008InitiateTx{
		"FRST/m-nope": {{endToEnd: "e-http-unk", amount: "10.00", ccy: "USD", mandate: "m-nope"}},
	})
	code, body := postPain008XML(t, srv.URL+"/exports/pain008/initiate", doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("unknown mandate: status=%d body=%v, want 200 with a per-item error", code, body)
	}
	first := pain008InitiateFirstResult(t, body)
	if first["error"] == nil || first["error"] == "" {
		t.Errorf("first result has no error: %v", first)
	}
	if first["repair_id"] == nil || first["repair_id"] == "" {
		t.Errorf("first result has no repair_id: %v", first)
	}
	if _, hasEntry := first["entry_id"]; hasEntry {
		t.Errorf("first result booked an entry: %v", first)
	}
}

// GET /exports/pain008 and POST /exports/pain008/apply are
// unaffected by the new initiate endpoint: the export still
// renders and the pain.002 answer path still answers it.
func TestHTTPPain008Initiate_ExportAndApplyUnaffected(t *testing.T) {
	srv := pain008TestServer(t)

	code, doc := getXML(t, srv.URL+"/exports/pain008?mandate=m-http&creditor_scheme_id=DE98ZZZ09999999999")
	if code != http.StatusOK {
		t.Fatalf("export: status=%d body=%s, want 200", code, doc)
	}
	if !strings.Contains(doc, "<EndToEndId>d-http</EndToEndId>") {
		t.Errorf("export missing d-http:\\n%s", doc)
	}
	msgID := pain008StatusTestSetup(t, srv.URL)
	code, body := postPain002XML(t, srv.URL+"/exports/pain008/apply", pain008HTTPDoc(msgID, [3]string{"d-http", "ACSC", ""}), "bank")
	if code != http.StatusOK {
		t.Fatalf("pain.002 apply: status=%d body=%v, want 200", code, body)
	}
	if body["state"] != "accepted" {
		t.Errorf("pain.002 state = %v, want accepted", body["state"])
	}
}
