package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Full MT196 answer export flow through HTTP: submit a payout, ask
// the correspondent bank's MT195 query, export the MT196 answer,
// replay the same :20:, and verify the FIN body, the error mapping,
// and the metrics counter.
func TestHTTPMT196Export(t *testing.T) {
	srv := gpiTestServer(t)
	payoutID := "po-mt196-http-1"
	submittedPayoutHTTP(t, srv.URL, payoutID)

	// Answer the payout's MT195 query first: the export renders
	// from the registered answer snapshot.
	applyURL := srv.URL + "/exports/mt195/apply"
	doc := mt195HTTPDoc("MT195HTTP000001", payoutID, "STAT", "Please confirm status of payment")
	code, body := postMT195Text(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/mt195/apply: status=%d body=%v", code, body)
	}

	post := func(t *testing.T, body string) (int, http.Header, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/exports/mt196", strings.NewReader(body))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-ledger-actor", "ops")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST /exports/mt196: %v", err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read /exports/mt196: %v", err)
		}
		return resp.StatusCode, resp.Header, string(b)
	}
	mustJSON := func(t *testing.T, req mt196ExportRequest) string {
		t.Helper()
		b, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(b)
	}
	valid := mt196ExportRequest{
		SenderLT:             "DEUTDEFF",
		ReceiverLT:           "CHASUS33",
		TransactionReference: "ANS-196-HTTP-1",
		RelatedReference:     "MT195HTTP000001",
		Narrative:            []string{"Please see the status above"},
	}
	validBody := mustJSON(t, valid)

	// Malformed JSON: 400.
	if code, _, _ := post(t, `{"sender_lt": }`); code != http.StatusBadRequest {
		t.Errorf("malformed JSON: status=%d, want 400", code)
	}
	// Unknown field: 400.
	if code, _, _ := post(t, `{"sender_lt":"DEUTDEFF","receiver_lt":"CHASUS33","transaction_reference":"ANS-196-HTTP-1","related_reference":"MT195HTTP000001","amount":100}`); code != http.StatusBadRequest {
		t.Errorf("unknown field: status=%d, want 400", code)
	}

	// The answers are the snapshot's ledger facts: the payout is
	// submitted, its MT103 value date is the submit day, and its
	// gpi track is "tracked".
	valueDate := time.Now().UTC().Format("060102")
	code, hdr, msg := post(t, validBody)
	if code != http.StatusOK {
		t.Fatalf("export: status=%d, want 200", code)
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content-type = %q, want text/plain", ct)
	}
	for _, want := range []string{
		"{1:F01DEUTDEFFXXX0000000000}",
		"{2:I196CHASUS33XXXU3003}",
		"{3:{108:ANS-196-HTTP-1}}",
		":20:ANS-196-HTTP-1",
		":21:MT195HTTP000001",
		":76:PAYOUT STATUS: submitted",
		"VALUE DATE: " + valueDate,
		"GPI STATUS: tracked",
		":77A:Please see the status above",
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
	longRef.TransactionReference = "ANS-196-HTTP-1-EXTRA"
	if code, _, _ := post(t, mustJSON(t, longRef)); code != http.StatusBadRequest {
		t.Errorf("overlong :20:: status=%d, want 400", code)
	}
	// One narrative line over 35 characters: 400.
	longLine := valid
	longLine.Narrative = []string{strings.Repeat("x", 36)}
	if code, _, _ := post(t, mustJSON(t, longLine)); code != http.StatusBadRequest {
		t.Errorf("narrative line over 35 chars: status=%d, want 400", code)
	}

	// An unknown :21: is a 422 (parked fail-closed in the repair
	// queue).
	unknown := valid
	unknown.TransactionReference = "ANS-196-HTTP-2"
	unknown.RelatedReference = "no-such-query"
	if code, _, _ := post(t, mustJSON(t, unknown)); code != http.StatusUnprocessableEntity {
		t.Errorf("unknown :21:: status=%d, want 422", code)
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
	// requests above (2 malformed + 1 ok + 3 validation + 1 unknown
	// + 1 repeat + 1 conflict = 9).
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_mt196_exports_total 9") {
		t.Errorf("metrics exposition missing ledger_mt196_exports_total 9:\n%s", metricsBody)
	}
}
