package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Full MT999 free-format export flow through HTTP: export the
// message, replay the same :20:, and verify the FIN body, the error
// mapping, and the metrics counter.
func TestHTTPMT999Export(t *testing.T) {
	srv := gpiTestServer(t)

	post := func(t *testing.T, body string) (int, http.Header, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/exports/mt999", strings.NewReader(body))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-ledger-actor", "ops")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST /exports/mt999: %v", err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read /exports/mt999: %v", err)
		}
		return resp.StatusCode, resp.Header, string(b)
	}
	mustJSON := func(t *testing.T, req mt999ExportRequest) string {
		t.Helper()
		b, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(b)
	}
	valid := mt999ExportRequest{
		SenderLT:             "BANKUS33",
		ReceiverLT:           "BANKDEFF",
		TransactionReference: "FREE-999-HTTP-1",
		RelatedReference:     "po-mt999-http-1",
		Narrative:            []string{"PLEASE ADVISE CASH POSITION", "FOR ACCOUNT po-mt999-http-1"},
	}
	validBody := mustJSON(t, valid)

	// Malformed JSON: 400.
	if code, _, _ := post(t, `{"sender_lt": }`); code != http.StatusBadRequest {
		t.Errorf("malformed JSON: status=%d, want 400", code)
	}
	// Unknown field: 400.
	if code, _, _ := post(t, `{"sender_lt":"BANKUS33","receiver_lt":"BANKDEFF","transaction_reference":"FREE-999-HTTP-1","related_reference":"po-mt999-http-1","narrative":["x"],"amount":100}`); code != http.StatusBadRequest {
		t.Errorf("unknown field: status=%d, want 400", code)
	}

	code, hdr, msg := post(t, validBody)
	if code != http.StatusOK {
		t.Fatalf("export: status=%d, want 200", code)
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content-type = %q, want text/plain", ct)
	}
	for _, want := range []string{
		"{1:F01BANKUS33XXX0000000000}",
		"{2:I999BANKDEFFXXXU3003}",
		"{3:{108:FREE-999-HTTP-1}}",
		":20:FREE-999-HTTP-1",
		":21:po-mt999-http-1",
		":79:PLEASE ADVISE CASH POSITION",
		"FOR ACCOUNT po-mt999-http-1",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("export missing %q, body:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "{5:") {
		t.Errorf("export must omit block 5, body:\n%s", msg)
	}

	// Missing :20:: 400.
	missingRef := valid
	missingRef.TransactionReference = ""
	if code, _, _ := post(t, mustJSON(t, missingRef)); code != http.StatusBadRequest {
		t.Errorf("missing :20:: status=%d, want 400", code)
	}
	// Illegal :20: (over 16 characters): 400.
	longRef := valid
	longRef.TransactionReference = "FREE-999-HTTP-1-EXTRA"
	if code, _, _ := post(t, mustJSON(t, longRef)); code != http.StatusBadRequest {
		t.Errorf("overlong :20:: status=%d, want 400", code)
	}
	// One narrative line over 35 characters: 400.
	longLine := valid
	longLine.Narrative = []string{strings.Repeat("x", 36)}
	if code, _, _ := post(t, mustJSON(t, longLine)); code != http.StatusBadRequest {
		t.Errorf("narrative line over 35 chars: status=%d, want 400", code)
	}

	// Re-export is byte-identical: the same :20: with the same
	// terms returns the stored message.
	_, _, msg2 := post(t, validBody)
	if msg2 != msg {
		t.Errorf("repeat export differs:\nfirst:\n%s\nsecond:\n%s", msg, msg2)
	}

	// Same :20:, different narrative: a 422 conflict, never a
	// silent overwrite.
	conflict := valid
	conflict.Narrative = []string{"A different note"}
	if code, _, _ := post(t, mustJSON(t, conflict)); code != http.StatusUnprocessableEntity {
		t.Errorf("conflicting :20:: status=%d, want 422", code)
	}

	// The metrics counter counts the export, including the rejected
	// requests above (2 malformed + 1 ok + 3 validation + 1 repeat
	// + 1 conflict = 8).
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_mt999_exports_total 8") {
		t.Errorf("metrics exposition missing ledger_mt999_exports_total 8:\n%s", metricsBody)
	}
}
