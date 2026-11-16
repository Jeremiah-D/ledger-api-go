package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// mt101ApplyTestServer builds a server with a funded payer and
// IBANs on both sides, ready for MT101 application through HTTP.
func mt101ApplyTestServer(t *testing.T) *httptest.Server {
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
	if err := l.SetAccountIBAN("merchant2", "FR1420041010050500013M02606", "DEUTDEFF"); err != nil {
		t.Fatalf("SetAccountIBAN merchant2: %v", err)
	}
	srv := httptest.NewServer(newRouter(l))
	t.Cleanup(srv.Close)
	return srv
}

// postMT101Text posts a raw MT101 FIN text document to
// /exports/mt101/apply with an x-ledger-actor header.
func postMT101Text(t *testing.T, url, finDoc, actor string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(finDoc))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "text/plain")
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

// mt101HTTPDoc renders an MT101 request for transfer: msgID as
// :20:, debtorIBAN as :50H:, valueDate as the :30: body
// ("261115"), and one sequence B per item (transferID, amount
// "100,00", ccy, creditorBIC, creditorIBAN, chargeBearer).
func mt101HTTPDoc(msgID, debtorIBAN, valueDate string, items ...mt101Item) string {
	var sb strings.Builder
	sb.WriteString("{1:F01BANKUS33XXX0000000000}\n")
	sb.WriteString("{2:I101BANKDEFFXXXXU3003}\n")
	sb.WriteString("{3:{108:" + msgID + "}}\n")
	sb.WriteString("{4:\n")
	sb.WriteString(":15A:\n")
	sb.WriteString(":20:" + msgID + "\n")
	sb.WriteString(":28D:1/1\n")
	sb.WriteString(":50H:/" + debtorIBAN + "\n")
	sb.WriteString(":52A:DEUTDEFF\n")
	sb.WriteString(":30:" + valueDate + "\n")
	for _, it := range items {
		sb.WriteString(":15B:\n")
		sb.WriteString(":21:" + it.transferID + "\n")
		sb.WriteString(":32B:" + it.ccy + it.amount + "\n")
		sb.WriteString(":57A:" + it.creditorBIC + "\n")
		sb.WriteString(":59:/" + it.creditorIBAN + "\n")
		sb.WriteString(":71A:" + it.chargeBearer + "\n")
	}
	sb.WriteString("-}")
	return sb.String()
}

// mt101Item is one sequence B payment of a test MT101 document,
// shared with the ledger package's test renderer by field name.
type mt101Item struct {
	transferID   string
	amount       string
	ccy          string
	creditorBIC  string
	creditorIBAN string
	chargeBearer string
}

// Full flow through HTTP: apply a customer's MT101 request for
// transfer, replay the same :20:, and the metrics counter moves.
func TestMT101ApplyEndpoint_BooksAndReturnsReceipts(t *testing.T) {
	srv := mt101ApplyTestServer(t)
	applyURL := srv.URL + "/exports/mt101/apply"

	doc := mt101HTTPDoc("MT101HTTP00001", "DE89370400440532013000", "261115",
		mt101Item{transferID: "tx-http-1", amount: "100,00", ccy: "USD", creditorBIC: "NWBKGB2L", creditorIBAN: "GB29NWBK60161331926819", chargeBearer: "SHA"},
		mt101Item{transferID: "tx-http-2", amount: "50,00", ccy: "USD", creditorBIC: "DEUTDEFF", creditorIBAN: "FR1420041010050500013M02606", chargeBearer: "OUR"},
	)
	code, body := postMT101Text(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/mt101/apply: status=%d body=%v", code, body)
	}
	if body["msg_id"] != "MT101HTTP00001" {
		t.Errorf("msg_id = %v, want MT101HTTP00001", body["msg_id"])
	}
	if body["duplicate"] == true {
		t.Errorf("duplicate = true on first apply, want false")
	}
	items, ok := body["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("items = %v, want 2 outcomes", body["items"])
	}
	first, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("items[0] = %v, want an object", items[0])
	}
	if first["entry_id"] != "tx-http-1" {
		t.Errorf("items[0] entry_id = %v, want tx-http-1", first["entry_id"])
	}
	if first["from_account"] != "cust" || first["to_account"] != "merchant" {
		t.Errorf("items[0] legs = %v->%v, want cust->merchant", first["from_account"], first["to_account"])
	}
	second, ok := items[1].(map[string]any)
	if !ok {
		t.Fatalf("items[1] = %v, want an object", items[1])
	}
	if second["entry_id"] != "tx-http-2" {
		t.Errorf("items[1] entry_id = %v, want tx-http-2", second["entry_id"])
	}
	// A malformed FIN document is a 400.
	code, body = postMT101Text(t, applyURL, "{1:F01BANKUS33XXX0000000000}", "ops")
	if code != http.StatusBadRequest {
		t.Fatalf("malformed: status=%d body=%v, want 400", code, body)
	}
	// An unknown debtor IBAN is a 422: the ledger never debits
	// an unknown account.
	unknown := mt101HTTPDoc("MT101HTTP00002", "NL91ABNA0417164300", "261115",
		mt101Item{transferID: "tx-http-3", amount: "100,00", ccy: "USD", creditorBIC: "NWBKGB2L", creditorIBAN: "GB29NWBK60161331926819", chargeBearer: "SHA"},
	)
	code, body = postMT101Text(t, applyURL, unknown, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown debtor: status=%d body=%v, want 422", code, body)
	}
	// Replay the same :20: with the same terms: idempotent,
	// duplicate set.
	code, body = postMT101Text(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("replay: status=%d body=%v", code, body)
	}
	if body["duplicate"] != true {
		t.Errorf("duplicate = %v, want true", body["duplicate"])
	}
	// Same :20:+:21:, different amount: a 422 conflict.
	conflict := mt101HTTPDoc("MT101HTTP00001", "DE89370400440532013000", "261115",
		mt101Item{transferID: "tx-http-1", amount: "200,00", ccy: "USD", creditorBIC: "NWBKGB2L", creditorIBAN: "GB29NWBK60161331926819", chargeBearer: "SHA"},
		mt101Item{transferID: "tx-http-2", amount: "50,00", ccy: "USD", creditorBIC: "DEUTDEFF", creditorIBAN: "FR1420041010050500013M02606", chargeBearer: "OUR"},
	)
	code, body = postMT101Text(t, applyURL, conflict, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("conflict: status=%d body=%v, want 422", code, body)
	}
	// The metrics counter counts every apply served.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_mt101_applies_total 5") {
		t.Errorf("metrics exposition missing ledger_mt101_applies_total 5:\n%s", metricsBody)
	}
}
