package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// mt295HTTPExpectedMT292Ref is the deterministic :20: of the MT292
// for a schedule and run: "MT292-" plus the first 10 lowercase hex
// characters of the SHA-256 of the run transfer ID
// ("<ID>/run/<seq>", the MT210's :21:) — the same derivation the
// ledger uses (see ledger.mt292MessageReference).
func mt295HTTPExpectedMT292Ref(scheduleID string, runSeq uint64) string {
	runID := fmt.Sprintf("%s/run/%d", scheduleID, runSeq)
	sum := sha256.Sum256([]byte(runID))
	return "MT292-" + hex.EncodeToString(sum[:])[:10]
}

// Full MT295 query export flow through HTTP: create and pause a
// transfer schedule, ask the correspondent bank about its MT292,
// replay the same question, and verify the FIN body, the error
// mapping, and the metrics counter.
func TestHTTPMT295Export(t *testing.T) {
	srv := mt195ExportTestServer(t)

	// Create a schedule (active) and pause it: only a schedule
	// whose notification premise is dead can be queried about
	// its MT292.
	code, body := postJSON(t, srv.URL+"/transfer-schedules", `{
		"schedule_id": "sched-295-http",
		"from_account": "saver",
		"to_account": "bob",
		"amount_cents": 5000,
		"currency": "USD",
		"interval": "24h",
		"next_run_at": "2026-11-12T09:00:00Z",
		"idempotency_key": "sched-295-http-key"
	}`)
	if code != http.StatusCreated {
		t.Fatalf("create schedule: status=%d, want 201 (body=%v)", code, body)
	}
	code, body = postJSON(t, srv.URL+"/transfer-schedules/sched-295-http/pause", `{}`)
	if code != http.StatusOK {
		t.Fatalf("pause schedule: status=%d, want 200 (body=%v)", code, body)
	}

	// A second schedule, left active: its MT292 cannot have
	// been sent, so asking about it parks fail-closed.
	code, body = postJSON(t, srv.URL+"/transfer-schedules", `{
		"schedule_id": "sched-295-http-active",
		"from_account": "saver",
		"to_account": "bob",
		"amount_cents": 5000,
		"currency": "USD",
		"interval": "24h",
		"next_run_at": "2026-11-12T09:00:00Z",
		"idempotency_key": "sched-295-http-active-key"
	}`)
	if code != http.StatusCreated {
		t.Fatalf("create active schedule: status=%d, want 201 (body=%v)", code, body)
	}

	post := func(t *testing.T, body string) (int, http.Header, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/exports/mt295", strings.NewReader(body))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-ledger-actor", "ops")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST /exports/mt295: %v", err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read /exports/mt295: %v", err)
		}
		return resp.StatusCode, resp.Header, string(b)
	}
	mustJSON := func(t *testing.T, req mt295ExportRequest) string {
		t.Helper()
		b, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(b)
	}
	valid := mt295ExportRequest{
		ScheduleID: "sched-295-http",
		RunSeq:     0,
		SenderLT:   "DEUTDEFF",
		ReceiverLT: "CHASUS33",
		Queries:    []string{"STAT"},
	}
	validBody := mustJSON(t, valid)

	// Malformed JSON: 400.
	if code, _, _ := post(t, `{"schedule_id": }`); code != http.StatusBadRequest {
		t.Errorf("malformed JSON: status=%d, want 400", code)
	}
	// Unknown field: 400.
	if code, _, _ := post(t, `{"schedule_id":"sched-295-http","run_seq":0,"sender_lt":"DEUTDEFF","receiver_lt":"CHASUS33","queries":["STAT"],"amount":100}`); code != http.StatusBadRequest {
		t.Errorf("unknown field: status=%d, want 400", code)
	}
	// :11S: is not in the body: strict decode refuses it, 400.
	if code, _, _ := post(t, `{"schedule_id":"sched-295-http","run_seq":0,"sender_lt":"DEUTDEFF","receiver_lt":"CHASUS33","queries":["STAT"],"11s":"210261110"}`); code != http.StatusBadRequest {
		t.Errorf(":11S: in body: status=%d, want 400", code)
	}
	// No question at all: 400.
	noQuestion := valid
	noQuestion.Queries = nil
	if code, _, _ := post(t, mustJSON(t, noQuestion)); code != http.StatusBadRequest {
		t.Errorf("no question: status=%d, want 400", code)
	}
	// One :75: line over 35 characters: 400.
	longLine := valid
	longLine.Queries = []string{strings.Repeat("x", 36)}
	if code, _, _ := post(t, mustJSON(t, longLine)); code != http.StatusBadRequest {
		t.Errorf(":75: line over 35 chars: status=%d, want 400", code)
	}

	// An unknown schedule is a 422 (parked fail-closed in the
	// repair queue, the error body carrying the repair record
	// ID).
	unknown := valid
	unknown.ScheduleID = "no-such-schedule"
	code, _, errBody := post(t, mustJSON(t, unknown))
	if code != http.StatusUnprocessableEntity {
		t.Errorf("unknown schedule: status=%d, want 422", code)
	}
	var errDoc map[string]any
	if err := json.Unmarshal([]byte(errBody), &errDoc); err != nil {
		t.Fatalf("unknown schedule error body is not JSON: %v", err)
	}
	if errDoc["repair_id"] == nil || errDoc["repair_id"] == "" {
		t.Errorf("unknown schedule error body = %v, want a repair_id", errDoc)
	}

	// A still-active schedule is a 422 (parked the same way).
	stillActive := valid
	stillActive.ScheduleID = "sched-295-http-active"
	if code, _, _ := post(t, mustJSON(t, stillActive)); code != http.StatusUnprocessableEntity {
		t.Errorf("still-active schedule: status=%d, want 422", code)
	}

	// The export renders the question: the sender LT is the
	// operator-supplied BIC, the :20: is the derived reference,
	// the :21: is the queried MT292's :20:.
	code, hdr, msg := post(t, validBody)
	if code != http.StatusOK {
		t.Fatalf("export: status=%d, want 200", code)
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content-type = %q, want text/plain", ct)
	}
	wantRelated := mt295HTTPExpectedMT292Ref("sched-295-http", 0)
	for _, want := range []string{
		"{1:F01DEUTDEFFXXX0000000000}",
		"{2:I295CHASUS33XXXU3003}",
		":20:MT295-",
		":21:" + wantRelated,
		":75:STAT",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("export missing %q, body:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "{5:") {
		t.Errorf("export must omit block 5, body:\n%s", msg)
	}
	// The derived :20: is 16 characters: "MT295-" plus 10 hex.
	for _, ln := range strings.Split(msg, "\n") {
		if strings.HasPrefix(ln, ":20:") && len(ln) != len(":20:")+16 {
			t.Errorf(":20: line = %q, want 16 characters after the tag", ln)
		}
	}

	// Re-export is byte-identical: the same schedule and run
	// asked the same question returns the stored message.
	_, _, msg2 := post(t, validBody)
	if msg2 != msg {
		t.Errorf("repeat export differs:\nfirst:\n%s\nsecond:\n%s", msg, msg2)
	}

	// A different question about the same cancellation request
	// is a different export under a different :20:.
	other := valid
	other.Queries = []string{"DUPL"}
	code, _, msg3 := post(t, mustJSON(t, other))
	if code != http.StatusOK {
		t.Fatalf("other question: status=%d, want 200", code)
	}
	if msg3 == msg {
		t.Error("different question rendered the same message, want a different :20:")
	}

	// The metrics counter counts the export, including the
	// rejected requests above (3 malformed + 2 validation + 2
	// parked + 1 ok + 1 repeat + 1 other question = 10).
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_mt295_exports_total 10") {
		t.Errorf("metrics exposition missing ledger_mt295_exports_total 10:\n%s", metricsBody)
	}
}
