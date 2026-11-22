package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// mt195ExportTestServer builds a server with a funded payer and the
// sender bank's BIC registered on the payee account, so the MT195
// export can derive its sender LT: the gpiTestServer shape plus the
// SetAccountIBAN line.
func mt195ExportTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	l := ledger.New(ledger.WithPayoutClearingAccount("payout-clearing"))
	if _, _, err := l.Post(ledger.JournalEntry{
		ID: "fund-saver", DebitAccount: "saver", CreditAccount: "equity",
		AmountCents: 1_000_000, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed Post: %v", err)
	}
	if err := l.SetAccountIBAN("saver", "DE89370400440532013000", "BANKUS33"); err != nil {
		t.Fatalf("SetAccountIBAN saver: %v", err)
	}
	srv := httptest.NewServer(newRouter(l))
	t.Cleanup(srv.Close)
	return srv
}

// Full MT195 query export flow through HTTP: submit a payout, ask
// the correspondent bank about it, replay the same question, and
// verify the FIN body, the error mapping, and the metrics counter.
func TestHTTPMT195Export(t *testing.T) {
	srv := mt195ExportTestServer(t)
	payoutID := "po-mt195-http-1"
	submittedPayoutHTTP(t, srv.URL, payoutID)

	post := func(t *testing.T, body string) (int, http.Header, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/exports/mt195", strings.NewReader(body))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-ledger-actor", "ops")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST /exports/mt195: %v", err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read /exports/mt195: %v", err)
		}
		return resp.StatusCode, resp.Header, string(b)
	}
	mustJSON := func(t *testing.T, req mt195ExportRequest) string {
		t.Helper()
		b, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(b)
	}
	valid := mt195ExportRequest{
		PayoutID:   payoutID,
		ReceiverLT: "BANKDEFF",
		Queries:    []string{"STAT"},
		Narrative:  []string{"Please confirm status of payment"},
	}
	validBody := mustJSON(t, valid)

	// Malformed JSON: 400.
	if code, _, _ := post(t, `{"payout_id": }`); code != http.StatusBadRequest {
		t.Errorf("malformed JSON: status=%d, want 400", code)
	}
	// Unknown field: 400.
	if code, _, _ := post(t, `{"payout_id":"`+payoutID+`","receiver_lt":"BANKDEFF","queries":["STAT"],"amount":100}`); code != http.StatusBadRequest {
		t.Errorf("unknown field: status=%d, want 400", code)
	}
	// Missing payout_id: 400.
	missingPayout := valid
	missingPayout.PayoutID = ""
	if code, _, _ := post(t, mustJSON(t, missingPayout)); code != http.StatusBadRequest {
		t.Errorf("missing payout_id: status=%d, want 400", code)
	}
	// Missing receiver_lt: 400.
	missingReceiver := valid
	missingReceiver.ReceiverLT = ""
	if code, _, _ := post(t, mustJSON(t, missingReceiver)); code != http.StatusBadRequest {
		t.Errorf("missing receiver_lt: status=%d, want 400", code)
	}
	// No question at all: 400.
	noQuestion := valid
	noQuestion.Queries = nil
	noQuestion.Narrative = nil
	if code, _, _ := post(t, mustJSON(t, noQuestion)); code != http.StatusBadRequest {
		t.Errorf("no question: status=%d, want 400", code)
	}
	// One :75: line over 35 characters: 400.
	longLine := valid
	longLine.Queries = []string{strings.Repeat("x", 36)}
	if code, _, _ := post(t, mustJSON(t, longLine)); code != http.StatusBadRequest {
		t.Errorf(":75: line over 35 chars: status=%d, want 400", code)
	}

	// An unknown payout is a 422 (parked fail-closed in the repair
	// queue).
	unknown := valid
	unknown.PayoutID = "no-such-payout"
	if code, _, _ := post(t, mustJSON(t, unknown)); code != http.StatusUnprocessableEntity {
		t.Errorf("unknown payout: status=%d, want 422", code)
	}

	// The export renders the question: the sender LT is the payee
	// account's registered BIC, the :20: is the derived reference,
	// the :21: is the payout ID.
	code, hdr, msg := post(t, validBody)
	if code != http.StatusOK {
		t.Fatalf("export: status=%d, want 200", code)
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content-type = %q, want text/plain", ct)
	}
	for _, want := range []string{
		"{1:F01BANKUS33XXX0000000000}",
		"{2:I195BANKDEFFXXXU3003}",
		":20:MT195-",
		":21:" + payoutID,
		":75:STAT",
		":79:Please confirm status of payment",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("export missing %q, body:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "{5:") {
		t.Errorf("export must omit block 5, body:\n%s", msg)
	}
	// The derived :20: is 16 characters: "MT195-" plus 10 hex.
	for _, ln := range strings.Split(msg, "\n") {
		if strings.HasPrefix(ln, ":20:") && len(ln) != len(":20:")+16 {
			t.Errorf(":20: line = %q, want 16 characters after the tag", ln)
		}
	}

	// Re-export is byte-identical: the same payout asked the same
	// question returns the stored message.
	_, _, msg2 := post(t, validBody)
	if msg2 != msg {
		t.Errorf("repeat export differs:\nfirst:\n%s\nsecond:\n%s", msg, msg2)
	}

	// A different question about the same payout is a different
	// export under a different :20:.
	other := valid
	other.Narrative = []string{"Please confirm value date"}
	code, _, msg3 := post(t, mustJSON(t, other))
	if code != http.StatusOK {
		t.Fatalf("other question: status=%d, want 200", code)
	}
	if msg3 == msg {
		t.Error("different question rendered the same message, want a different :20:")
	}

	// The metrics counter counts the export, including the rejected
	// requests above (2 malformed + 4 validation + 1 unknown + 1
	// ok + 1 repeat + 1 other question = 10).
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_mt195_exports_total 10") {
		t.Errorf("metrics exposition missing ledger_mt195_exports_total 10:\n%s", metricsBody)
	}
}
