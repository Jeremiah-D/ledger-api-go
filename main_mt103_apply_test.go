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

// mt103HTTPDoc renders one inbound MT103 test document: cust pays
// merchant USD 100.00, value date today (UTC), CRED/SHA, with both
// institution BICs. The ref names the instruction.
func mt103HTTPDoc(ref string) string {
	var sb strings.Builder
	sb.WriteString("{1:F01DEUTDEFFXXX0000000000}\n")
	sb.WriteString("{2:I103NWBKGB2LXXXU3003}\n")
	sb.WriteString("{3:{108:" + ref + "}}\n")
	sb.WriteString("{4:\n")
	sb.WriteString(":20:" + ref + "\n")
	sb.WriteString(":23B:CRED\n")
	sb.WriteString(":32A:" + time.Now().UTC().Format("060102") + "USD100,00\n")
	sb.WriteString(":50K:/DE89370400440532013000\n")
	sb.WriteString("Alice Customer\n")
	sb.WriteString(":52A:DEUTDEFF\n")
	sb.WriteString(":57A:NWBKGB2L\n")
	sb.WriteString(":59:/GB29NWBK60161331926819\n")
	sb.WriteString("Bob Merchant\n")
	sb.WriteString(":70:invoice 42\n")
	sb.WriteString(":71A:SHA\n")
	sb.WriteString("-}")
	return sb.String()
}

// mt103HTTPSetup builds a ledger with a funded payer and IBANs on
// both sides, ready for POST /exports/mt103/apply: cust holds USD
// 10,000.00, merchant holds nothing yet.
func mt103HTTPSetup(t *testing.T) *ledger.Ledger {
	t.Helper()
	l := ledger.New()
	if _, _, err := l.Post(ledger.JournalEntry{ID: "fund-cust", DebitAccount: "cust", CreditAccount: "bank", AmountCents: 1000000, Currency: "USD"}); err != nil {
		t.Fatalf("fund Post: %v", err)
	}
	if err := l.SetAccountIBAN("cust", "DE89370400440532013000", "DEUTDEFF"); err != nil {
		t.Fatalf("SetAccountIBAN cust: %v", err)
	}
	if err := l.SetAccountIBAN("merchant", "GB29NWBK60161331926819", "NWBKGB2L"); err != nil {
		t.Fatalf("SetAccountIBAN merchant: %v", err)
	}
	return l
}

// postMT103Text posts a raw MT103 FIN text document to
// /exports/mt103/apply with an x-ledger-actor header.
func postMT103Text(t *testing.T, url, finDoc, actor string) (int, map[string]any) {
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

// Full flow through HTTP: apply a correspondent's MT103, replay
// the same :20:, and the metrics counter moves.
func TestMT103ApplyEndpoint_AppliesAndReturnsReceipt(t *testing.T) {
	l := mt103HTTPSetup(t)
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()
	applyURL := srv.URL + "/exports/mt103/apply"

	doc := mt103HTTPDoc("MT103-HTTP-01")
	code, body := postMT103Text(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/mt103/apply: status=%d body=%v", code, body)
	}
	if body["msg_id"] != "MT103-HTTP-01" {
		t.Errorf("msg_id = %v, want MT103-HTTP-01", body["msg_id"])
	}
	if body["from_account"] != "cust" || body["to_account"] != "merchant" {
		t.Errorf("legs = %v->%v, want cust->merchant", body["from_account"], body["to_account"])
	}
	if body["entry_id"] != "MT103-HTTP-01" {
		t.Errorf("entry_id = %v, want MT103-HTTP-01", body["entry_id"])
	}
	receipt, ok := body["receipt"].(map[string]any)
	if !ok {
		t.Fatalf("receipt = %v, want an object", body["receipt"])
	}
	if receipt["transfer_id"] != "MT103-HTTP-01" {
		t.Errorf("receipt transfer_id = %v, want MT103-HTTP-01", receipt["transfer_id"])
	}
	if body["duplicate"] == true {
		t.Errorf("duplicate = true on first apply, want false")
	}
	if body["repair_id"] != nil {
		t.Errorf("repair_id = %v, want absent", body["repair_id"])
	}
	// The money moved.
	if got := l.BalanceIn("merchant", "USD"); got != 10000 {
		t.Errorf("merchant balance = %d, want 10000", got)
	}
	// Replay the same :20: with the same terms: idempotent,
	// duplicate set, nothing booked twice.
	code, body = postMT103Text(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("replay: status=%d body=%v", code, body)
	}
	if body["duplicate"] != true {
		t.Errorf("duplicate = %v, want true", body["duplicate"])
	}
	if got := l.BalanceIn("merchant", "USD"); got != 10000 {
		t.Errorf("merchant balance after replay = %d, want 10000", got)
	}
	// Same :20:, different terms: a 422 conflict.
	conflict := strings.Replace(doc, ":32A:"+time.Now().UTC().Format("060102")+"USD100,00", ":32A:"+time.Now().UTC().Format("060102")+"USD200,00", 1)
	code, body = postMT103Text(t, applyURL, conflict, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("conflict: status=%d body=%v, want 422", code, body)
	}
	// A malformed FIN document is a 400.
	code, body = postMT103Text(t, applyURL, "{1:F01DEUTDEFFXXX0000000000}", "ops")
	if code != http.StatusBadRequest {
		t.Fatalf("malformed: status=%d body=%v, want 400", code, body)
	}
	// The metrics counter counts every apply served.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_mt103_applies_total 4") {
		t.Errorf("metrics exposition missing ledger_mt103_applies_total 4:\n%s", metricsBody)
	}
}
