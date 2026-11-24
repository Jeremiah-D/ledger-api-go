package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Full MT296 answer export flow through HTTP: register a
// correspondent bank's MT295 query and its MT292 cancellation
// request about one MT210 notification, export the MT296 answer
// to each, replay the same :20:, and verify the FIN body, the
// error mapping, and the metrics counter.
func TestHTTPMT296Export(t *testing.T) {
	srv := mt195ExportTestServer(t)

	// Seed the anchor first: an MT210 notification whose :20:
	// the query's and the cancellation request's :21: will name.
	code, body := postMT195Text(t, srv.URL+"/exports/mt210/apply", mt295SeedMT210Doc("NTF-296-HTTP"), "ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/mt210/apply seed: status=%d body=%v", code, body)
	}

	// The inbound MT295 query asks about that notification; its
	// :20: is the second anchor tier.
	doc := mt295HTTPDoc("QRY-296-HTTP", "NTF-296-HTTP",
		[]string{"STAT"}, nil, nil)
	code, body = postMT195Text(t, srv.URL+"/exports/mt295/apply", doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/mt295/apply seed: status=%d body=%v", code, body)
	}

	// The inbound MT292 cancels the same notification; its :20:
	// is the first anchor tier.
	code, body = postMT292JSON(t, srv.URL+"/exports/mt292/apply",
		mt292ApplyDoc("CXL-296-HTTP", "NTF-296-HTTP", "210261124", "Client withdrew the order"), "ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/mt292/apply seed: status=%d body=%v", code, body)
	}

	post := func(t *testing.T, body string) (int, http.Header, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/exports/mt296", strings.NewReader(body))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-ledger-actor", "ops")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST /exports/mt296: %v", err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read /exports/mt296: %v", err)
		}
		return resp.StatusCode, resp.Header, string(b)
	}
	mustJSON := func(t *testing.T, req mt296ExportRequest) string {
		t.Helper()
		b, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(b)
	}
	validA := mt296ExportRequest{
		SenderLT:             "DEUTDEFF",
		ReceiverLT:           "CHASUS33",
		TransactionReference: "ANS-296-HTTP-1",
		RelatedReference:     "CXL-296-HTTP",
		Answers:              []string{"/CNCL/", "CANCELLATION CONFIRMED 261124"},
		Narrative:            []string{"OPERATOR NOTE"},
		Description:          []string{"BANK CONFIRMS THE NOTIFICATION"},
	}
	validB := mt296ExportRequest{
		SenderLT:             "DEUTDEFF",
		ReceiverLT:           "CHASUS33",
		TransactionReference: "ANS-296-HTTP-2",
		RelatedReference:     "QRY-296-HTTP",
		Answers:              []string{"/RJCR/"},
	}
	validABody := mustJSON(t, validA)

	// Malformed JSON: 400.
	if code, _, _ := post(t, `{"sender_lt": }`); code != http.StatusBadRequest {
		t.Errorf("malformed JSON: status=%d, want 400", code)
	}
	// Unknown field (:11S: is not in the export body): 400.
	if code, _, _ := post(t, `{"sender_lt":"DEUTDEFF","receiver_lt":"CHASUS33","transaction_reference":"ANS-296-HTTP-1","related_reference":"CXL-296-HTTP","answers":["/CNCL/"],"11s":"210261124"}`); code != http.StatusBadRequest {
		t.Errorf("unknown field: status=%d, want 400", code)
	}

	// The answers are the operator's lines, verbatim.
	code, hdr, msg := post(t, validABody)
	if code != http.StatusOK {
		t.Fatalf("export (a): status=%d, want 200", code)
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content-type = %q, want text/plain", ct)
	}
	for _, want := range []string{
		"{1:F01DEUTDEFFXXX0000000000}",
		"{2:I296CHASUS33XXXU3003}",
		"{3:{108:ANS-296-HTTP-1}}",
		":20:ANS-296-HTTP-1",
		":21:CXL-296-HTTP",
		":76:/CNCL/",
		"CANCELLATION CONFIRMED 261124",
		":77A:OPERATOR NOTE",
		":79:BANK CONFIRMS THE NOTIFICATION",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("export (a) missing %q, body:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "{5:") {
		t.Errorf("export must omit block 5, body:\n%s", msg)
	}

	// The MT295 tier answers the query with the same shape.
	code, _, msgB := post(t, mustJSON(t, validB))
	if code != http.StatusOK {
		t.Fatalf("export (b): status=%d, want 200", code)
	}
	for _, want := range []string{
		":20:ANS-296-HTTP-2",
		":21:QRY-296-HTTP",
		":76:/RJCR/",
	} {
		if !strings.Contains(msgB, want) {
			t.Errorf("export (b) missing %q, body:\n%s", want, msgB)
		}
	}

	// Empty :76: is a 400: the answer is the operator's
	// statement, and an answer with no answer lines is no answer.
	emptyAnswers := validA
	emptyAnswers.TransactionReference = "ANS-296-HTTP-3"
	emptyAnswers.Answers = nil
	if code, _, _ := post(t, mustJSON(t, emptyAnswers)); code != http.StatusBadRequest {
		t.Errorf("empty :76:: status=%d, want 400", code)
	}
	// Over 6 answer lines: 400.
	seven := validA
	seven.TransactionReference = "ANS-296-HTTP-4"
	seven.Answers = []string{"A1", "A2", "A3", "A4", "A5", "A6", "A7"}
	if code, _, _ := post(t, mustJSON(t, seven)); code != http.StatusBadRequest {
		t.Errorf("7 :76: lines: status=%d, want 400", code)
	}
	// One answer line over 35 characters: 400.
	longLine := validA
	longLine.TransactionReference = "ANS-296-HTTP-5"
	longLine.Answers = []string{strings.Repeat("x", 36)}
	if code, _, _ := post(t, mustJSON(t, longLine)); code != http.StatusBadRequest {
		t.Errorf("36-char :76: line: status=%d, want 400", code)
	}

	// An unknown :21: is a 422 (parked fail-closed in the repair
	// queue).
	unknown := validA
	unknown.TransactionReference = "ANS-296-HTTP-6"
	unknown.RelatedReference = "no-such-request"
	if code, _, _ := post(t, mustJSON(t, unknown)); code != http.StatusUnprocessableEntity {
		t.Errorf("unknown :21:: status=%d, want 422", code)
	}
	// A :21: shaped like our own outbound MT292's :20: is a 422
	// too: the ledger holds no outbound MT292 registry.
	outboundShape := validA
	outboundShape.TransactionReference = "ANS-296-HTTP-7"
	outboundShape.RelatedReference = mt296HTTPAnsweredRef
	if code, _, _ := post(t, mustJSON(t, outboundShape)); code != http.StatusUnprocessableEntity {
		t.Errorf("outbound-shaped :21:: status=%d, want 422", code)
	}

	// Re-export is byte-identical: the same :20: with the same
	// terms returns the stored message.
	_, _, msg2 := post(t, validABody)
	if msg2 != msg {
		t.Errorf("repeat export differs:\nfirst:\n%s\nsecond:\n%s", msg, msg2)
	}

	// Same :20:, different answers: a 422 conflict, never a
	// silent overwrite.
	conflict := validA
	conflict.Answers = []string{"/PDCR/"}
	if code, _, _ := post(t, mustJSON(t, conflict)); code != http.StatusUnprocessableEntity {
		t.Errorf("conflicting :20:: status=%d, want 422", code)
	}

	// The metrics counter counts the export, including the rejected
	// requests above (2 malformed + 2 ok + 3 validation + 2 unknown
	// + 1 repeat + 1 conflict = 11).
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_mt296_exports_total 11") {
		t.Errorf("metrics exposition missing ledger_mt296_exports_total 11:\n%s", metricsBody)
	}
}
