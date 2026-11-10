package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// Full MT199 free-format export flow through HTTP: post the JSON body,
// export the message, and verify the FIN body, the error mapping, and
// the metrics counter.
func TestHTTPMT199Export(t *testing.T) {
	l := ledger.New()
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	post := func(t *testing.T, body string) (int, http.Header, string) {
		t.Helper()
		resp, err := http.Post(srv.URL+"/exports/mt199", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST /exports/mt199: %v", err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read /exports/mt199: %v", err)
		}
		return resp.StatusCode, resp.Header, string(b)
	}
	mustJSON := func(t *testing.T, req mt199ExportRequest) string {
		t.Helper()
		b, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(b)
	}
	valid := mt199ExportRequest{
		SenderLT:             "DEUTDEFF",
		ReceiverLT:           "CHASUS33",
		TransactionReference: "REF-2026-001",
		RelatedReference:     "PAY-99",
		Narrative:            []string{"Please confirm receipt", "of the free format notice."},
	}
	validBody := mustJSON(t, valid)

	// Malformed JSON: 400.
	if code, _, _ := post(t, `{"sender_lt": }`); code != http.StatusBadRequest {
		t.Errorf("malformed JSON: status=%d, want 400", code)
	}
	// Unknown field: 400.
	if code, _, _ := post(t, `{"sender_lt":"DEUTDEFF","receiver_lt":"CHASUS33","transaction_reference":"REF-2026-001","narrative":["hi"],"amount":100}`); code != http.StatusBadRequest {
		t.Errorf("unknown field: status=%d, want 400", code)
	}

	code, hdr, body := post(t, validBody)
	if code != http.StatusOK {
		t.Fatalf("export: status=%d, want 200", code)
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content-type = %q, want text/plain", ct)
	}
	for _, want := range []string{
		"{1:F01DEUTDEFFXXX0000000000}",
		"{2:I199CHASUS33XXXU3003}",
		"{3:{108:REF-2026-001}}",
		":20:REF-2026-001",
		":21:PAY-99",
		":79:Please confirm receipt",
		"of the free format notice.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("export missing %q, body:\n%s", want, body)
		}
	}
	if strings.Contains(body, "{5:") {
		t.Errorf("export must omit block 5, body:\n%s", body)
	}

	// Missing :20:: 400.
	missingRef := valid
	missingRef.TransactionReference = ""
	if code, _, _ := post(t, mustJSON(t, missingRef)); code != http.StatusBadRequest {
		t.Errorf("missing :20:: status=%d, want 400", code)
	}
	// Illegal :20: (over 16 characters): 400.
	longRef := valid
	longRef.TransactionReference = "REF-2026-001-EXTRA"
	if code, _, _ := post(t, mustJSON(t, longRef)); code != http.StatusBadRequest {
		t.Errorf("overlong :20:: status=%d, want 400", code)
	}
	// Empty narrative: 400.
	emptyNarr := valid
	emptyNarr.Narrative = []string{}
	if code, _, _ := post(t, mustJSON(t, emptyNarr)); code != http.StatusBadRequest {
		t.Errorf("empty narrative: status=%d, want 400", code)
	}
	// Narrative over 50 lines: 400.
	manyLines := valid
	manyLines.Narrative = make([]string, 51)
	for i := range manyLines.Narrative {
		manyLines.Narrative[i] = "line"
	}
	if code, _, _ := post(t, mustJSON(t, manyLines)); code != http.StatusBadRequest {
		t.Errorf("narrative over 50 lines: status=%d, want 400", code)
	}
	// One narrative line over 35 characters: 400.
	longLine := valid
	longLine.Narrative = []string{strings.Repeat("x", 36)}
	if code, _, _ := post(t, mustJSON(t, longLine)); code != http.StatusBadRequest {
		t.Errorf("narrative line over 35 chars: status=%d, want 400", code)
	}
	// Illegal LT (not a BIC): 400.
	badLT := valid
	badLT.SenderLT = "BAD"
	if code, _, _ := post(t, mustJSON(t, badLT)); code != http.StatusBadRequest {
		t.Errorf("illegal LT: status=%d, want 400", code)
	}

	// No related_reference: the message has no :21: line.
	noRel := valid
	noRel.RelatedReference = ""
	code, _, bodyNoRel := post(t, mustJSON(t, noRel))
	if code != http.StatusOK {
		t.Fatalf("export without :21:: status=%d, want 200", code)
	}
	if strings.Contains(bodyNoRel, ":21:") {
		t.Errorf("export without related_reference must have no :21: line, body:\n%s", bodyNoRel)
	}

	// Re-export is byte-identical: pure rendering, no clock value.
	_, _, body2 := post(t, validBody)
	if body2 != body {
		t.Errorf("repeat export differs:\nfirst:\n%s\nsecond:\n%s", body, body2)
	}

	// The metrics counter counts the export, including the rejected
	// requests above (2 malformed + 1 ok + 6 validation + 1 ok + 1
	// repeat = 11).
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	mbody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics: status=%d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(mbody), "ledger_mt199_exports_total 11") {
		t.Errorf("metrics missing ledger_mt199_exports_total 11")
	}
}
