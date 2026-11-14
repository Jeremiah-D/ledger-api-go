package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// seedPacs009ApplyServer creates a server with a funded creditor-agent
// account (bank-cdtr) and the pacs.009 settlement account configured,
// so a standalone receipt can book. It returns the server and the
// underlying ledger for direct setup.
func seedPacs009ApplyServer(t *testing.T) (*httptest.Server, *ledger.Ledger) {
	t.Helper()
	l := ledger.New()
	srv := httptest.NewServer(newRouter(l))
	t.Cleanup(srv.Close)
	l.SetPacs009SettlementAccount("acct-pacs009-settle")
	fundPacs009Account(t, srv.URL, "bank-cdtr", 100000)
	return srv, l
}

// fundPacs009Account funds account with cents USD through POST
// /entries, so the account is a known ledger account.
func fundPacs009Account(t *testing.T, url, account string, cents int64) {
	t.Helper()
	code, _ := postJSON(t, url+"/entries", fmt.Sprintf(`{"debit_account":%q,"credit_account":"equity","amount_cents":%d,"currency":"USD"}`, account, cents))
	if code != http.StatusCreated {
		t.Fatalf("fund %s: status = %d, want 201", account, code)
	}
}

// seedPacs009Transfer funds alice and posts transfer id alice -> bob
// for amountCents USD, so the inbound document can associate by
// EndToEndId.
func seedPacs009Transfer(t *testing.T, srv *httptest.Server, l *ledger.Ledger, id string, amountCents int64) {
	t.Helper()
	fundPacs009Account(t, srv.URL, "alice", 10000)
	if _, err := l.PostTransfer(ledger.Transfer{ID: id, From: "alice", To: "bob", AmountCents: amountCents}); err != nil {
		t.Fatalf("PostTransfer %s: %v", id, err)
	}
}

// postPacs009XML posts a raw pacs.009 XML document to
// /exports/pacs009/apply with an x-ledger-actor header.
func postPacs009XML(t *testing.T, url, xmlDoc, actor string) (int, map[string]any) {
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

// pacs009AgentXML renders one pacs.009 agent element (FinInstnId /
// Othr / Id = the account ID, the pacs009.go mapping).
func pacs009AgentXML(tag, id string) string {
	return `<` + tag + `><FinInstnId><Othr><Id>` + id + `</Id></Othr></FinInstnId></` + tag + `>`
}

// pacs009HTTPDoc renders a pacs.009.001.06 FI-to-FI credit transfer
// for msgID / endToEndID / instrID, moving amount in ccy from
// debtor agent dbtrAgt to creditor agent cdtrAgt.
func pacs009HTTPDoc(msgID, endToEndID, instrID, amount, ccy, sttlmMtd, chrgsBr, dbtrAgt, cdtrAgt string) string {
	var sb strings.Builder
	sb.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pacs.009.001.06">`)
	sb.WriteString(`<FICdtTrf><GrpHdr><MsgId>` + msgID + `</MsgId>`)
	sb.WriteString(`<CreDtTm>2026-11-14T07:00:00Z</CreDtTm><NbOfTxs>1</NbOfTxs>`)
	sb.WriteString(`<SttlmInf><SttlmMtd>` + sttlmMtd + `</SttlmMtd></SttlmInf></GrpHdr>`)
	sb.WriteString(`<CdtTrfTxInf><PmtId><InstrId>` + instrID + `</InstrId>`)
	sb.WriteString(`<EndToEndId>` + endToEndID + `</EndToEndId>`)
	sb.WriteString(`<TxId>` + endToEndID + `</TxId></PmtId>`)
	sb.WriteString(`<IntrBkSttlmAmt Ccy="` + ccy + `">` + amount + `</IntrBkSttlmAmt>`)
	sb.WriteString(`<IntrBkSttlmDt>2026-11-14</IntrBkSttlmDt>`)
	sb.WriteString(pacs009AgentXML("InstgAgt", dbtrAgt))
	sb.WriteString(pacs009AgentXML("InstdAgt", cdtrAgt))
	sb.WriteString(pacs009AgentXML("Dbtr", dbtrAgt))
	sb.WriteString(pacs009AgentXML("DbtrAgt", dbtrAgt))
	sb.WriteString(pacs009AgentXML("CdtrAgt", cdtrAgt))
	sb.WriteString(pacs009AgentXML("Cdtr", cdtrAgt))
	sb.WriteString(`<ChrgsBr>` + chrgsBr + `</ChrgsBr>`)
	sb.WriteString(`</CdtTrfTxInf></FICdtTrf></Document>`)
	return sb.String()
}

// requirePacs009Repair fails the test unless body carries a
// non-empty repair_id.
func requirePacs009Repair(t *testing.T, body map[string]any) {
	t.Helper()
	if body["repair_id"] == nil || body["repair_id"] == "" {
		t.Errorf("body = %v, want a repair_id", body)
	}
}

// Full flow through HTTP: apply the correspondent bank's pacs.009
// credit transfer, replay the same MsgId, and the metrics counter
// moves.
func TestHTTPPacs009Apply(t *testing.T) {
	srv, l := seedPacs009ApplyServer(t)
	applyURL := srv.URL + "/exports/pacs009/apply"

	doc := pacs009HTTPDoc("p9-http-1", "e2e-p9-1", "e2e-p9-1", "12.00", "USD", "INDA", "SLEV", "bank-dbtr", "bank-cdtr")
	code, body := postPacs009XML(t, applyURL, doc, "bank-corr")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/pacs009/apply: status=%d body=%v", code, body)
	}
	if body["applied"] != true {
		t.Errorf("applied = %v, want true", body["applied"])
	}
	if body["duplicate"] != false {
		t.Errorf("duplicate = %v, want false", body["duplicate"])
	}
	if body["entry_id"] != "pacs009/e2e-p9-1" {
		t.Errorf("entry_id = %v, want pacs009/e2e-p9-1", body["entry_id"])
	}
	assoc, _ := body["association"].(map[string]any)
	if assoc["kind"] != "none" {
		t.Errorf("association kind = %v, want none", assoc["kind"])
	}
	transfer, _ := body["transfer"].(map[string]any)
	if transfer["msg_id"] != "p9-http-1" {
		t.Errorf("transfer msg_id = %v, want p9-http-1", transfer["msg_id"])
	}
	if transfer["amount_cents"] != float64(1200) || transfer["currency"] != "USD" {
		t.Errorf("transfer amount = %v %v, want 1200 USD", transfer["amount_cents"], transfer["currency"])
	}
	if transfer["settlement_method"] != "INDA" || transfer["charges_bearer"] != "SLEV" {
		t.Errorf("transfer routing = %v/%v, want INDA/SLEV", transfer["settlement_method"], transfer["charges_bearer"])
	}
	if transfer["creditor_agent"] != "bank-cdtr" {
		t.Errorf("transfer creditor_agent = %v, want bank-cdtr", transfer["creditor_agent"])
	}

	// The creditor agent received the 12.00.
	if bal := l.BalanceIn("bank-cdtr", "USD"); bal != 100000+1200 {
		t.Errorf("bank-cdtr balance = %d, want %d", bal, 100000+1200)
	}

	// Replay the same MsgId: a no-op success, duplicate set.
	code, body = postPacs009XML(t, applyURL, doc, "bank-corr")
	if code != http.StatusOK {
		t.Fatalf("replay POST /exports/pacs009/apply: status=%d body=%v", code, body)
	}
	if body["duplicate"] != true || body["applied"] != false {
		t.Errorf("replay body = %v, want duplicate true, applied false", body)
	}
	if body["entry_id"] != "pacs009/e2e-p9-1" {
		t.Errorf("replay entry_id = %v, want pacs009/e2e-p9-1", body["entry_id"])
	}
	if bal := l.BalanceIn("bank-cdtr", "USD"); bal != 100000+1200 {
		t.Errorf("balance after replay = %d, want %d (unchanged)", bal, 100000+1200)
	}

	// Metrics: both applies were counted.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_pacs009_applies_total 2") {
		t.Errorf("metrics exposition missing ledger_pacs009_applies_total 2:\n%s", metricsBody)
	}
}

// Association: an EndToEndId naming a posted transfer books the
// receipt against that original, and an InstrId naming a transfer
// associates when the EndToEndId names nothing.
func TestHTTPPacs009ApplyAssociatedTransfer(t *testing.T) {
	srv, l := seedPacs009ApplyServer(t)
	applyURL := srv.URL + "/exports/pacs009/apply"
	seedPacs009Transfer(t, srv, l, "p9-tx-1", 2500)

	doc := pacs009HTTPDoc("p9-http-2", "p9-tx-1", "p9-tx-1", "25.00", "USD", "INDA", "SLEV", "alice", "bob")
	code, body := postPacs009XML(t, applyURL, doc, "bank-corr")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/pacs009/apply: status=%d body=%v", code, body)
	}
	assoc, _ := body["association"].(map[string]any)
	if assoc["kind"] != "transfer" || assoc["id"] != "p9-tx-1" {
		t.Errorf("association = %v, want transfer p9-tx-1", assoc)
	}
	if body["entry_id"] != "pacs009/p9-tx-1" {
		t.Errorf("entry_id = %v, want pacs009/p9-tx-1", body["entry_id"])
	}
	// bob holds the transfer (2500) plus the receipt (2500).
	if bal := l.BalanceIn("bob", "USD"); bal != 5000 {
		t.Errorf("bob balance = %d, want 5000", bal)
	}

	// InstrId association: the EndToEndId names nothing, the
	// InstrId names the second transfer.
	seedPacs009Transfer(t, srv, l, "p9-tx-2", 1000)
	doc = pacs009HTTPDoc("p9-http-3", "e2e-unknown-2", "p9-tx-2", "10.00", "USD", "COVE", "DEBT", "alice", "bob")
	code, body = postPacs009XML(t, applyURL, doc, "bank-corr")
	if code != http.StatusOK {
		t.Fatalf("InstrId apply: status=%d body=%v", code, body)
	}
	assoc, _ = body["association"].(map[string]any)
	if assoc["kind"] != "transfer" || assoc["id"] != "p9-tx-2" {
		t.Errorf("InstrId association = %v, want transfer p9-tx-2", assoc)
	}
	if body["entry_id"] != "pacs009/e2e-unknown-2" {
		t.Errorf("entry_id = %v, want pacs009/e2e-unknown-2", body["entry_id"])
	}
	// bob holds the first transfer (2500) and receipt (2500), plus
	// the second transfer (1000) and receipt (1000).
	if bal := l.BalanceIn("bob", "USD"); bal != 7000 {
		t.Errorf("bob balance = %d, want 7000", bal)
	}
}

// Parse refusals are parked in the repair queue, not silently
// dropped: malformed XML, a NbOfTxs other than 1, and a missing
// MsgId are all answered 400 with a repair_id.
func TestHTTPPacs009ApplyParseRefusalParked(t *testing.T) {
	srv, _ := seedPacs009ApplyServer(t)
	applyURL := srv.URL + "/exports/pacs009/apply"

	if code, body := postPacs009XML(t, applyURL, "<Document><broken", "ops"); code != http.StatusBadRequest {
		t.Errorf("malformed XML: status=%d body=%v, want 400", code, body)
	} else {
		requirePacs009Repair(t, body)
	}
	doc := pacs009HTTPDoc("p9-parse-2", "e2e-parse-2", "e2e-parse-2", "12.00", "USD", "INDA", "SLEV", "bank-dbtr", "bank-cdtr")
	doc = strings.Replace(doc, "<NbOfTxs>1</NbOfTxs>", "<NbOfTxs>2</NbOfTxs>", 1)
	if code, body := postPacs009XML(t, applyURL, doc, "ops"); code != http.StatusBadRequest {
		t.Errorf("NbOfTxs 2: status=%d body=%v, want 400", code, body)
	} else {
		requirePacs009Repair(t, body)
	}
	doc = pacs009HTTPDoc("p9-parse-3", "e2e-parse-3", "e2e-parse-3", "12.00", "USD", "INDA", "SLEV", "bank-dbtr", "bank-cdtr")
	doc = strings.Replace(doc, "<MsgId>p9-parse-3</MsgId>", "", 1)
	if code, body := postPacs009XML(t, applyURL, doc, "ops"); code != http.StatusBadRequest {
		t.Errorf("missing MsgId: status=%d body=%v, want 400", code, body)
	} else {
		requirePacs009Repair(t, body)
	}
}

// Vocabulary violations are parse refusals, so they are parked too:
// a SttlmMtd or ChrgsBr outside the admitted closed sets is
// answered 400 with a repair_id.
func TestHTTPPacs009ApplyVocabularyParked(t *testing.T) {
	srv, _ := seedPacs009ApplyServer(t)
	applyURL := srv.URL + "/exports/pacs009/apply"

	doc := pacs009HTTPDoc("p9-vocab-1", "e2e-vocab-1", "e2e-vocab-1", "12.00", "USD", "XXXX", "SLEV", "bank-dbtr", "bank-cdtr")
	if code, body := postPacs009XML(t, applyURL, doc, "ops"); code != http.StatusBadRequest {
		t.Errorf("unknown SttlmMtd: status=%d body=%v, want 400", code, body)
	} else {
		requirePacs009Repair(t, body)
	}
	doc = pacs009HTTPDoc("p9-vocab-2", "e2e-vocab-2", "e2e-vocab-2", "12.00", "USD", "INDA", "XXXX", "bank-dbtr", "bank-cdtr")
	if code, body := postPacs009XML(t, applyURL, doc, "ops"); code != http.StatusBadRequest {
		t.Errorf("unknown ChrgsBr: status=%d body=%v, want 400", code, body)
	} else {
		requirePacs009Repair(t, body)
	}
}

// An unassociated transfer whose CdtrAgt is unknown to the ledger
// is parked and answered 404 with a repair_id.
func TestHTTPPacs009ApplyUnknownCreditorParked(t *testing.T) {
	srv, _ := seedPacs009ApplyServer(t)
	applyURL := srv.URL + "/exports/pacs009/apply"

	doc := pacs009HTTPDoc("p9-ghost-1", "e2e-ghost-1", "e2e-ghost-1", "12.00", "USD", "INDA", "SLEV", "bank-dbtr", "bank-ghost")
	code, body := postPacs009XML(t, applyURL, doc, "ops")
	if code != http.StatusNotFound {
		t.Errorf("unknown CdtrAgt: status=%d body=%v, want 404", code, body)
	} else {
		requirePacs009Repair(t, body)
	}
}

// Association mismatches are parked and answered 422 with a
// repair_id: an amount that differs from the associated transfer.
func TestHTTPPacs009ApplyAmountMismatchParked(t *testing.T) {
	srv, l := seedPacs009ApplyServer(t)
	applyURL := srv.URL + "/exports/pacs009/apply"
	seedPacs009Transfer(t, srv, l, "p9-tx-1", 2500)

	doc := pacs009HTTPDoc("p9-mm-1", "p9-tx-1", "p9-tx-1", "26.00", "USD", "INDA", "SLEV", "alice", "bob")
	code, body := postPacs009XML(t, applyURL, doc, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Errorf("amount mismatch: status=%d body=%v, want 422", code, body)
	} else {
		requirePacs009Repair(t, body)
	}
	// Nothing booked on the mismatch.
	if bal := l.BalanceIn("bob", "USD"); bal != 2500 {
		t.Errorf("bob balance = %d, want 2500 (transfer only)", bal)
	}
}

// Association mismatches, continued: a currency that differs from
// the associated transfer.
func TestHTTPPacs009ApplyCurrencyMismatchParked(t *testing.T) {
	srv, l := seedPacs009ApplyServer(t)
	applyURL := srv.URL + "/exports/pacs009/apply"
	seedPacs009Transfer(t, srv, l, "p9-tx-1", 2500)

	doc := pacs009HTTPDoc("p9-mm-2", "p9-tx-1", "p9-tx-1", "25.00", "EUR", "INDA", "SLEV", "alice", "bob")
	code, body := postPacs009XML(t, applyURL, doc, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Errorf("currency mismatch: status=%d body=%v, want 422", code, body)
	} else {
		requirePacs009Repair(t, body)
	}
}

// Association mismatches, continued: a CdtrAgt that is a known
// ledger account but none of the associated transfer's accounts.
func TestHTTPPacs009ApplyCreditorMismatchParked(t *testing.T) {
	srv, l := seedPacs009ApplyServer(t)
	applyURL := srv.URL + "/exports/pacs009/apply"
	seedPacs009Transfer(t, srv, l, "p9-tx-1", 2500)

	doc := pacs009HTTPDoc("p9-mm-3", "p9-tx-1", "p9-tx-1", "25.00", "USD", "INDA", "SLEV", "alice", "bank-cdtr")
	code, body := postPacs009XML(t, applyURL, doc, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Errorf("creditor mismatch: status=%d body=%v, want 422", code, body)
	} else {
		requirePacs009Repair(t, body)
	}
}

// With no pacs.009 settlement account configured the transfer is
// refused 422 with no repair record: the counterparty leg has
// nowhere to go, and there is nothing for an operator to repair
// through the queue.
func TestHTTPPacs009ApplyUnconfiguredSettlement(t *testing.T) {
	l := ledger.New()
	srv := httptest.NewServer(newRouter(l))
	t.Cleanup(srv.Close)
	fundPacs009Account(t, srv.URL, "bank-cdtr", 100000)
	applyURL := srv.URL + "/exports/pacs009/apply"

	doc := pacs009HTTPDoc("p9-nosettle-1", "e2e-nosettle-1", "e2e-nosettle-1", "12.00", "USD", "INDA", "SLEV", "bank-dbtr", "bank-cdtr")
	code, body := postPacs009XML(t, applyURL, doc, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Errorf("unconfigured settlement: status=%d body=%v, want 422", code, body)
	} else if body["repair_id"] != nil && body["repair_id"] != "" {
		t.Errorf("unconfigured settlement body = %v, want no repair_id", body)
	}
}

// A MsgId reused for a different transfer is refused 422 with no
// repair record: one message ID names one transfer.
func TestHTTPPacs009ApplyMsgIDConflict(t *testing.T) {
	srv, _ := seedPacs009ApplyServer(t)
	applyURL := srv.URL + "/exports/pacs009/apply"

	doc := pacs009HTTPDoc("p9-conflict", "e2e-c1", "e2e-c1", "12.00", "USD", "INDA", "SLEV", "bank-dbtr", "bank-cdtr")
	if code, body := postPacs009XML(t, applyURL, doc, "bank-corr"); code != http.StatusOK {
		t.Fatalf("first apply: status=%d body=%v, want 200", code, body)
	}
	doc = pacs009HTTPDoc("p9-conflict", "e2e-c2", "e2e-c2", "13.00", "USD", "INDA", "SLEV", "bank-dbtr", "bank-cdtr")
	code, body := postPacs009XML(t, applyURL, doc, "bank-corr")
	if code != http.StatusUnprocessableEntity {
		t.Errorf("MsgId conflict: status=%d body=%v, want 422", code, body)
	} else if body["repair_id"] != nil && body["repair_id"] != "" {
		t.Errorf("MsgId conflict body = %v, want no repair_id", body)
	}
}

// The inbound half leaves the outbound half alone: after an apply,
// GET /exports/pacs009 still renders the posted transfer as a
// pacs.009 instruction, with the deterministic export MsgId — not
// the receipt entry.
func TestHTTPPacs009ApplyLeavesExportIntact(t *testing.T) {
	srv, l := seedPacs009ApplyServer(t)
	applyURL := srv.URL + "/exports/pacs009/apply"
	seedPacs009Transfer(t, srv, l, "p9-tx-exp", 2500)

	doc := pacs009HTTPDoc("p9-http-exp", "p9-tx-exp", "p9-tx-exp", "25.00", "USD", "INDA", "SLEV", "alice", "bob")
	if code, body := postPacs009XML(t, applyURL, doc, "bank-corr"); code != http.StatusOK {
		t.Fatalf("POST /exports/pacs009/apply: status=%d body=%v", code, body)
	}

	code, _, exportBody := getBody(t, srv.URL+"/exports/pacs009?transfer=p9-tx-exp")
	if code != http.StatusOK {
		t.Fatalf("GET /exports/pacs009: status=%d body=%s", code, exportBody)
	}
	for _, want := range []string{
		"<MsgId>p9-tx-exp/ficdt</MsgId>",
		`<IntrBkSttlmAmt Ccy="USD">25.00</IntrBkSttlmAmt>`,
		"<SttlmMtd>INDA</SttlmMtd>",
		"<ChrgsBr>SLEV</ChrgsBr>",
	} {
		if !strings.Contains(exportBody, want) {
			t.Errorf("export missing %q:\n%s", want, exportBody)
		}
	}
	// The export renders the transfer, never the inbound receipt
	// entry the apply booked.
	if strings.Contains(exportBody, "pacs009/p9-tx-exp") {
		t.Errorf("export carries the receipt entry id:\n%s", exportBody)
	}
}
