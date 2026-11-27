package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
	// The MT292 tier is registry-anchored (LG-476): the server
	// needs BICs on both legs to render one, so this test uses
	// the MT292 fixture (alice DEUTDEFF, bob NWBKGB2L) rather
	// than the MT195 one (no BIC on bob).
	l, _ := mt292HTTPSetup(t)
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	// Create a schedule (active) and pause it: only a schedule
	// whose notification premise is dead can be queried about
	// its MT292.
	code, body := postJSON(t, srv.URL+"/transfer-schedules", `{
		"schedule_id": "sched-295-http",
		"from_account": "alice",
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

	// Register the MT292 first: the MT292 tier is
	// registry-anchored (LG-476), so nothing is queryable
	// before this export lands.
	code, _, mt292Msg := getBody(t, srv.URL+"/exports/mt292?schedule_id=sched-295-http&servicing_bic=CITIUS33&original_date=2026-11-10&reason=Schedule+paused+by+operator")
	if code != http.StatusOK {
		t.Fatalf("register MT292: status=%d, want 200 (body=%v)", code, mt292Msg)
	}
	if !strings.Contains(mt292Msg, ":20:"+mt295HTTPExpectedMT292Ref("sched-295-http", 0)) {
		t.Fatalf("registered MT292 missing :20:%s, body:\n%s", mt295HTTPExpectedMT292Ref("sched-295-http", 0), mt292Msg)
	}

	// A second schedule, left active: its MT292 cannot have
	// been sent, so asking about it parks fail-closed.
	code, body = postJSON(t, srv.URL+"/transfer-schedules", `{
		"schedule_id": "sched-295-http-active",
		"from_account": "alice",
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

// MT202-tier MT295 query export through HTTP: register an
// outbound MT202, ask the correspondent bank about it by its
// :20:, replay the same question, and verify the FIN body, the
// anchor errors, and the metrics counter.
func TestHTTPMT295ExportForMT202(t *testing.T) {
	srv := mt195ExportTestServer(t)

	post := func(t *testing.T, path, body string) (int, http.Header, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-ledger-actor", "ops")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
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

	// Register the outbound MT202 first: the query anchors
	// against the registry, so nothing is queryable before
	// this export lands.
	code, _, mt202Msg := post(t, "/exports/mt202", `{
		"sender_lt": "BANKUS33",
		"receiver_lt": "BANKDEFFXXXX",
		"transaction_reference": "MT202-REF-1",
		"related_reference": "ORIG-REF-9",
		"value_date": "2026-10-22",
		"currency": "USD",
		"amount_cents": 250000,
		"ordering_institution_bic": "BANKUS33",
		"beneficiary_institution_bic": "BANKDEFF"
	}`)
	if code != http.StatusOK {
		t.Fatalf("register MT202: status=%d, want 200 (body=%v)", code, mt202Msg)
	}
	if !strings.Contains(mt202Msg, ":20:MT202-REF-1") {
		t.Fatalf("registered MT202 missing :20:MT202-REF-1, body:\n%s", mt202Msg)
	}

	valid := mt295ExportRequest{
		MT202Reference: "MT202-REF-1",
		SenderLT:       "DEUTDEFF",
		ReceiverLT:     "CHASUS33",
		Queries:        []string{"STAT"},
	}
	validBody := mustJSON(t, valid)

	// The two anchors are mutually exclusive: a body naming
	// both the MT202 reference and a schedule is a 400.
	both := valid
	both.ScheduleID = "sched-295-http"
	if code, _, _ := post(t, "/exports/mt295", mustJSON(t, both)); code != http.StatusBadRequest {
		t.Errorf("schedule_id + mt202_reference: status=%d, want 400", code)
	}
	// A non-zero run_seq counts as the MT292 anchor too.
	bothRun := valid
	bothRun.RunSeq = 5
	if code, _, _ := post(t, "/exports/mt295", mustJSON(t, bothRun)); code != http.StatusBadRequest {
		t.Errorf("run_seq + mt202_reference: status=%d, want 400", code)
	}

	// An unregistered MT202 reference is a 422 (parked
	// fail-closed in the repair queue, the error body carrying
	// the repair record ID).
	unknown := valid
	unknown.MT202Reference = "MT202-REF-9"
	code, _, errBody := post(t, "/exports/mt295", mustJSON(t, unknown))
	if code != http.StatusUnprocessableEntity {
		t.Errorf("unknown MT202: status=%d, want 422", code)
	}
	var errDoc map[string]any
	if err := json.Unmarshal([]byte(errBody), &errDoc); err != nil {
		t.Fatalf("unknown MT202 error body is not JSON: %v", err)
	}
	if errDoc["repair_id"] == nil || errDoc["repair_id"] == "" {
		t.Errorf("unknown MT202 error body = %v, want a repair_id", errDoc)
	}

	// The export renders the question: the :21: is the MT202's
	// own :20:, caller-supplied — no derivation, no shape.
	code, hdr, msg := post(t, "/exports/mt295", validBody)
	if code != http.StatusOK {
		t.Fatalf("export: status=%d, want 200", code)
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content-type = %q, want text/plain", ct)
	}
	for _, want := range []string{
		"{1:F01DEUTDEFFXXX0000000000}",
		"{2:I295CHASUS33XXXU3003}",
		":20:MT295-",
		":21:MT202-REF-1",
		":75:STAT",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("export missing %q, body:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "{5:") {
		t.Errorf("export must omit block 5, body:\n%s", msg)
	}

	// Re-export is byte-identical: the same MT202 reference
	// asked the same question returns the stored message.
	_, _, msg2 := post(t, "/exports/mt295", validBody)
	if msg2 != msg {
		t.Errorf("repeat export differs:\nfirst:\n%s\nsecond:\n%s", msg, msg2)
	}

	// The metrics counter counts the export, including the
	// rejected requests above (2 dual-anchor 400s + 1 parked
	// 422 + 1 ok + 1 repeat = 5). The MT202 registration is
	// counted separately, under ledger_mt202_exports_total.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_mt295_exports_total 5") {
		t.Errorf("metrics exposition missing ledger_mt295_exports_total 5:\n%s", metricsBody)
	}
}

// MT292-tier MT295 query export without a registered MT292:
// a paused schedule whose cancellation request was never
// rendered parks fail-closed with a 422 and a repair ID.
func TestHTTPMT295ExportUnregisteredMT292(t *testing.T) {
	// The MT292 tier is registry-anchored (LG-476): the server
	// needs BICs on both legs to render one, so this test uses
	// the MT292 fixture (alice DEUTDEFF, bob NWBKGB2L) rather
	// than the MT195 one (no BIC on bob).
	l, _ := mt292HTTPSetup(t)
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	code, body := postJSON(t, srv.URL+"/transfer-schedules", `{
		"schedule_id": "sched-295-unreg",
		"from_account": "alice",
		"to_account": "bob",
		"amount_cents": 5000,
		"currency": "USD",
		"interval": "24h",
		"next_run_at": "2026-11-12T09:00:00Z",
		"idempotency_key": "sched-295-unreg-key"
	}`)
	if code != http.StatusCreated {
		t.Fatalf("create schedule: status=%d, want 201 (body=%v)", code, body)
	}
	code, body = postJSON(t, srv.URL+"/transfer-schedules/sched-295-unreg/pause", `{}`)
	if code != http.StatusOK {
		t.Fatalf("pause schedule: status=%d, want 200 (body=%v)", code, body)
	}

	post := func(t *testing.T, body string) (int, string) {
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
		return resp.StatusCode, string(b)
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
		ScheduleID: "sched-295-unreg",
		RunSeq:     0,
		SenderLT:   "DEUTDEFF",
		ReceiverLT: "CHASUS33",
		Queries:    []string{"STAT"},
	}

	// Paused but never rendered: the query parks fail-closed,
	// the error body carrying the repair record ID and naming
	// the unregistered MT292's :20:.
	code, errBody := post(t, mustJSON(t, valid))
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("unregistered MT292: status=%d, want 422 (body=%v)", code, errBody)
	}
	var errDoc map[string]any
	if err := json.Unmarshal([]byte(errBody), &errDoc); err != nil {
		t.Fatalf("unregistered MT292 error body is not JSON: %v", err)
	}
	if errDoc["repair_id"] == nil || errDoc["repair_id"] == "" {
		t.Errorf("unregistered MT292 error body = %v, want a repair_id", errDoc)
	}
	if !strings.Contains(errBody, mt295HTTPExpectedMT292Ref("sched-295-unreg", 0)) {
		t.Errorf("unregistered MT292 error body = %v, want the MT292 :20: %s", errDoc, mt295HTTPExpectedMT292Ref("sched-295-unreg", 0))
	}
}

// MT292-tier MT295 query export after registration: the same
// question parks before the MT292 is rendered, then renders
// 200 once GET /exports/mt292 has registered it — the
// restart-recovery path (LG-476): re-export first, then re-ask.
func TestHTTPMT295ExportAfterMT292Registration(t *testing.T) {
	// The MT292 tier is registry-anchored (LG-476): the server
	// needs BICs on both legs to render one, so this test uses
	// the MT292 fixture (alice DEUTDEFF, bob NWBKGB2L) rather
	// than the MT195 one (no BIC on bob).
	l, _ := mt292HTTPSetup(t)
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	code, body := postJSON(t, srv.URL+"/transfer-schedules", `{
		"schedule_id": "sched-295-reg",
		"from_account": "alice",
		"to_account": "bob",
		"amount_cents": 5000,
		"currency": "USD",
		"interval": "24h",
		"next_run_at": "2026-11-12T09:00:00Z",
		"idempotency_key": "sched-295-reg-key"
	}`)
	if code != http.StatusCreated {
		t.Fatalf("create schedule: status=%d, want 201 (body=%v)", code, body)
	}
	code, body = postJSON(t, srv.URL+"/transfer-schedules/sched-295-reg/pause", `{}`)
	if code != http.StatusOK {
		t.Fatalf("pause schedule: status=%d, want 200 (body=%v)", code, body)
	}

	post := func(t *testing.T, body string) (int, string) {
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
		return resp.StatusCode, string(b)
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
		ScheduleID: "sched-295-reg",
		RunSeq:     0,
		SenderLT:   "DEUTDEFF",
		ReceiverLT: "CHASUS33",
		Queries:    []string{"STAT"},
	}
	validBody := mustJSON(t, valid)

	// Before registration the same question parks.
	if code, errBody := post(t, validBody); code != http.StatusUnprocessableEntity {
		t.Fatalf("before registration: status=%d, want 422 (body=%v)", code, errBody)
	}

	// Register the MT292: the same schedule and run derive the
	// same :20: the query's :21: will name.
	code, _, mt292Msg := getBody(t, srv.URL+"/exports/mt292?schedule_id=sched-295-reg&servicing_bic=CITIUS33&original_date=2026-11-10")
	if code != http.StatusOK {
		t.Fatalf("register MT292: status=%d, want 200 (body=%v)", code, mt292Msg)
	}

	// Now the same question renders: the :21: is the
	// registered MT292's :20:.
	code, msg := post(t, validBody)
	if code != http.StatusOK {
		t.Fatalf("export: status=%d, want 200 (body=%v)", code, msg)
	}
	for _, want := range []string{
		":20:MT295-",
		":21:" + mt295HTTPExpectedMT292Ref("sched-295-reg", 0),
		":75:STAT",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("export missing %q, body:\n%s", want, msg)
		}
	}
}

// MT296-tier MT295 query export through HTTP: register an
// outbound MT296 answer first, then ask about it — the :21: is
// the answer's own :20:, caller-supplied, and the three anchors
// (schedule/run, mt202_reference, mt296_reference) are mutually
// exclusive.
func TestHTTPMT295ExportForMT296(t *testing.T) {
	srv := mt195ExportTestServer(t)

	// Seed the anchor first: an MT210 notification, then the
	// inbound MT292 cancellation request the MT296 answers.
	code, body := postMT195Text(t, srv.URL+"/exports/mt210/apply", mt295SeedMT210Doc("NTF-296-HTTP"), "ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/mt210/apply seed: status=%d body=%v", code, body)
	}
	code, body = postMT292JSON(t, srv.URL+"/exports/mt292/apply",
		mt292ApplyDoc("CXL-296-HTTP", "NTF-296-HTTP", "210261124", "Client withdrew the order"), "ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/mt292/apply seed: status=%d body=%v", code, body)
	}
	// Register the outbound MT296 answer: the query anchors
	// against this registry, so nothing is queryable before
	// this export lands.
	mt296Body, err := json.Marshal(mt296ExportRequest{
		SenderLT:             "DEUTDEFF",
		ReceiverLT:           "CHASUS33",
		TransactionReference: "ANS-296-HTTP-1",
		RelatedReference:     "CXL-296-HTTP",
		Answers:              []string{"/CNCL/", "CANCELLATION CONFIRMED 261124"},
	})
	if err != nil {
		t.Fatalf("marshal mt296 request: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/exports/mt296", strings.NewReader(string(mt296Body)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-ledger-actor", "ops")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /exports/mt296: %v", err)
	}
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read /exports/mt296: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register MT296: status=%d, want 200 (body=%v)", resp.StatusCode, string(b))
	}
	if !strings.Contains(string(b), ":20:ANS-296-HTTP-1") {
		t.Fatalf("registered MT296 missing :20:ANS-296-HTTP-1, body:\n%s", string(b))
	}

	post := func(t *testing.T, path, body string) (int, http.Header, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-ledger-actor", "ops")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
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
		MT296Reference: "ANS-296-HTTP-1",
		SenderLT:       "DEUTDEFF",
		ReceiverLT:     "CHASUS33",
		Queries:        []string{"STAT"},
	}
	validBody := mustJSON(t, valid)

	// The three anchors are mutually exclusive: a body naming
	// the MT296 reference plus any other anchor is a 400.
	bothSched := valid
	bothSched.ScheduleID = "sched-295-http"
	if code, _, _ := post(t, "/exports/mt295", mustJSON(t, bothSched)); code != http.StatusBadRequest {
		t.Errorf("schedule_id + mt296_reference: status=%d, want 400", code)
	}
	bothRun := valid
	bothRun.RunSeq = 5
	if code, _, _ := post(t, "/exports/mt295", mustJSON(t, bothRun)); code != http.StatusBadRequest {
		t.Errorf("run_seq + mt296_reference: status=%d, want 400", code)
	}
	bothMT202 := valid
	bothMT202.MT202Reference = "MT202-REF-1"
	if code, _, _ := post(t, "/exports/mt295", mustJSON(t, bothMT202)); code != http.StatusBadRequest {
		t.Errorf("mt202_reference + mt296_reference: status=%d, want 400", code)
	}

	// The export renders the question: the :21: is the MT296's
	// own :20:, caller-supplied — no derivation, no shape.
	code, hdr, msg := post(t, "/exports/mt295", validBody)
	if code != http.StatusOK {
		t.Fatalf("export: status=%d, want 200", code)
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content-type = %q, want text/plain", ct)
	}
	for _, want := range []string{
		"{1:F01DEUTDEFFXXX0000000000}",
		"{2:I295CHASUS33XXXU3003}",
		":20:MT295-",
		":21:ANS-296-HTTP-1",
		":75:STAT",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("export missing %q, body:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "{5:") {
		t.Errorf("export must omit block 5, body:\n%s", msg)
	}

	// Re-export is byte-identical: the same MT296 reference
	// asked the same question returns the stored message.
	_, _, msg2 := post(t, "/exports/mt295", validBody)
	if msg2 != msg {
		t.Errorf("repeat export differs:\nfirst:\n%s\nsecond:\n%s", msg, msg2)
	}

	// The metrics counter counts the export, including the
	// rejected requests above (3 dual-anchor 400s + 1 ok +
	// 1 repeat = 5). The MT296 registration is counted
	// separately, under ledger_mt296_exports_total.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_mt295_exports_total 5") {
		t.Errorf("metrics exposition missing ledger_mt295_exports_total 5:\n%s", metricsBody)
	}
}

// MT296-tier MT295 query export without a registered MT296:
// the answer was never rendered, so the query parks
// fail-closed with a 422 and a repair ID.
func TestHTTPMT295ExportUnregisteredMT296(t *testing.T) {
	srv := mt195ExportTestServer(t)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/exports/mt295", strings.NewReader(`{
		"mt296_reference": "ANS-296-9999",
		"sender_lt": "DEUTDEFF",
		"receiver_lt": "CHASUS33",
		"queries": ["STAT"]
	}`))
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
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("unknown MT296: status=%d, want 422 (body=%v)", resp.StatusCode, string(b))
	}
	var errDoc map[string]any
	if err := json.Unmarshal(b, &errDoc); err != nil {
		t.Fatalf("unknown MT296 error body is not JSON: %v", err)
	}
	if errDoc["repair_id"] == nil || errDoc["repair_id"] == "" {
		t.Errorf("unknown MT296 error body = %v, want a repair_id", errDoc)
	}
}

func TestHTTPMT295ExportForMT210(t *testing.T) {
	l, _ := mt292HTTPSetup(t)
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	// Create a schedule and keep it active: MT210 refuses a
	// paused schedule (LG-479), the opposite of the MT292
	// fixture, which pauses first.
	code, body := postJSON(t, srv.URL+"/transfer-schedules", `{
		"schedule_id": "sched-210",
		"from_account": "alice",
		"to_account": "bob",
		"amount_cents": 5000,
		"currency": "USD",
		"interval": "24h",
		"next_run_at": "2026-11-12T09:00:00Z",
		"idempotency_key": "sched-210-key"
	}`)
	if code != http.StatusCreated {
		t.Fatalf("create schedule: status=%d, want 201 (body=%v)", code, body)
	}

	// Register the outbound MT210 first: the query anchors
	// against the registry, so nothing is queryable before
	// this export lands.
	code, _, mt210Msg := getBody(t, srv.URL+"/exports/mt210?schedule_id=sched-210&servicing_bic=CITIUS33")
	if code != http.StatusOK {
		t.Fatalf("register MT210: status=%d, want 200 (body=%v)", code, mt210Msg)
	}
	if !strings.Contains(mt210Msg, ":20:sched-210") {
		t.Fatalf("registered MT210 missing :20:sched-210, body:\n%s", mt210Msg)
	}

	post := func(t *testing.T, path, body string) (int, http.Header, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-ledger-actor", "ops")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
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
		MT210Reference: "sched-210",
		SenderLT:       "DEUTDEFF",
		ReceiverLT:     "CHASUS33",
		Queries:        []string{"STAT"},
	}
	validBody := mustJSON(t, valid)

	// The four anchors are mutually exclusive: a body naming
	// the MT210 reference plus any other anchor is a 400.
	bothSched := valid
	bothSched.ScheduleID = "sched-210"
	if code, _, _ := post(t, "/exports/mt295", mustJSON(t, bothSched)); code != http.StatusBadRequest {
		t.Errorf("schedule_id + mt210_reference: status=%d, want 400", code)
	}
	bothRun := valid
	bothRun.RunSeq = 5
	if code, _, _ := post(t, "/exports/mt295", mustJSON(t, bothRun)); code != http.StatusBadRequest {
		t.Errorf("run_seq + mt210_reference: status=%d, want 400", code)
	}
	bothMT202 := valid
	bothMT202.MT202Reference = "MT202-REF-1"
	if code, _, _ := post(t, "/exports/mt295", mustJSON(t, bothMT202)); code != http.StatusBadRequest {
		t.Errorf("mt202_reference + mt210_reference: status=%d, want 400", code)
	}
	bothMT296 := valid
	bothMT296.MT296Reference = "ANS-296-HTTP-1"
	if code, _, _ := post(t, "/exports/mt295", mustJSON(t, bothMT296)); code != http.StatusBadRequest {
		t.Errorf("mt296_reference + mt210_reference: status=%d, want 400", code)
	}

	// An unregistered MT210 reference is a 422 (parked
	// fail-closed in the repair queue, the error body carrying
	// the repair record ID).
	unknown := valid
	unknown.MT210Reference = "sched-unknown"
	code, _, errBody := post(t, "/exports/mt295", mustJSON(t, unknown))
	if code != http.StatusUnprocessableEntity {
		t.Errorf("unknown MT210: status=%d, want 422", code)
	}
	var errDoc map[string]any
	if err := json.Unmarshal([]byte(errBody), &errDoc); err != nil {
		t.Fatalf("unknown MT210 error body is not JSON: %v", err)
	}
	if errDoc["repair_id"] == nil || errDoc["repair_id"] == "" {
		t.Errorf("unknown MT210 error body = %v, want a repair_id", errDoc)
	}

	// The export renders the question: the :21: is the MT210's
	// own :20: — the schedule ID itself, stable across runs.
	code, hdr, msg := post(t, "/exports/mt295", validBody)
	if code != http.StatusOK {
		t.Fatalf("export: status=%d, want 200", code)
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content-type = %q, want text/plain", ct)
	}
	for _, want := range []string{
		"{1:F01DEUTDEFFXXX0000000000}",
		"{2:I295CHASUS33XXXU3003}",
		":20:MT295-",
		":21:sched-210",
		":75:STAT",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("export missing %q, body:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "{5:") {
		t.Errorf("export must omit block 5, body:\n%s", msg)
	}

	// Re-export is byte-identical: the same MT210 reference
	// asked the same question returns the stored message.
	_, _, msg2 := post(t, "/exports/mt295", validBody)
	if msg2 != msg {
		t.Errorf("repeat export differs:\nfirst:\n%s\nsecond:\n%s", msg, msg2)
	}

	// The metrics counter counts the export, including the
	// rejected requests above (4 dual-anchor 400s + 1 parked
	// 422 + 1 ok + 1 repeat = 7). The MT210 registration is
	// counted separately, under ledger_mt210_exports_total.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_mt295_exports_total 7") {
		t.Errorf("metrics exposition missing ledger_mt295_exports_total 7:\n%s", metricsBody)
	}
}
