package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// mt296HTTPDoc renders one inbound MT296 answer as FIN text: ref
// as :20:, relatedRef as :21: (the answered MT292's :20:),
// answers as the :76: lines, narrative as the :77A: lines (empty
// omits :77A:), description as the :79: lines (empty omits
// :79:). The fixture is hand-built FIN text, the MT292 inbound
// convention: the parse side takes raw text, and the export side
// lives in ledger/mt296_export.go (LG-467).
func mt296HTTPDoc(ref, relatedRef string, answers, narrative, description []string) string {
	var b strings.Builder
	b.WriteString("{1:F01CITIUS33XXX0000000000}\n")
	b.WriteString("{2:I296NWBKGB2LXXXU3003}\n")
	b.WriteString("{3:{108:" + ref + "}}\n")
	b.WriteString("{4:\n")
	b.WriteString(":20:" + ref + "\n")
	b.WriteString(":21:" + relatedRef + "\n")
	b.WriteString(":76:" + answers[0] + "\n")
	for _, ln := range answers[1:] {
		b.WriteString(ln + "\n")
	}
	if len(narrative) > 0 {
		b.WriteString(":77A:" + narrative[0] + "\n")
		for _, ln := range narrative[1:] {
			b.WriteString(ln + "\n")
		}
	}
	if len(description) > 0 {
		b.WriteString(":79:" + description[0] + "\n")
		for _, ln := range description[1:] {
			b.WriteString(ln + "\n")
		}
	}
	b.WriteString("-}")
	return b.String()
}

// mt296HTTPAnsweredRef is a :21: with the shape of an outbound
// MT292 :20: — the deterministic reference ExportMT292 derives
// for schedule "sched-1", run 1 (SHA-256 of "sched-1/run/1",
// first 10 hex characters: 117c924981).
const mt296HTTPAnsweredRef = "MT292-117c924981"

// Full flow through HTTP: register a correspondent bank's MT296
// answer, replay the same :20:, and verify the error mapping and
// the metrics counter.
func TestMT296ApplyEndpoint_RegistersAnswer(t *testing.T) {
	srv := mt195ExportTestServer(t)
	applyURL := srv.URL + "/exports/mt296/apply"

	doc := mt296HTTPDoc("ANS-296-HTTP-1", mt296HTTPAnsweredRef,
		[]string{"/CNCL/", "CANCELLATION CONFIRMED 261124"},
		[]string{"OPERATOR NOTE"},
		[]string{"BANK CONFIRMS THE NOTIFICATION"})
	code, body := postMT195Text(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/mt296/apply: status=%d body=%v", code, body)
	}
	if body["reference"] != "ANS-296-HTTP-1" {
		t.Errorf("reference = %v, want ANS-296-HTTP-1", body["reference"])
	}
	if body["related_reference"] != mt296HTTPAnsweredRef {
		t.Errorf("related_reference = %v, want %s", body["related_reference"], mt296HTTPAnsweredRef)
	}
	answers, ok := body["answers"].([]any)
	if !ok || len(answers) != 2 || answers[0] != "/CNCL/" {
		t.Errorf("answers = %v, want [/CNCL/ ...]", body["answers"])
	}
	if body["duplicate"] == true {
		t.Errorf("duplicate = true on first apply, want false")
	}
	// A malformed FIN document is a 400.
	code, body = postMT195Text(t, applyURL, "{1:F01CITIUS33XXX0000000000}", "ops")
	if code != http.StatusBadRequest {
		t.Fatalf("malformed: status=%d body=%v, want 400", code, body)
	}
	// Replay the same :20: with the same terms: idempotent,
	// duplicate set.
	code, body = postMT195Text(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("replay: status=%d body=%v", code, body)
	}
	if body["duplicate"] != true {
		t.Errorf("duplicate = %v, want true", body["duplicate"])
	}
	// An unknown :21: is a 422 (parked fail-closed in the repair
	// queue), and the error body carries the repair record ID.
	unknown := mt296HTTPDoc("ANS-296-HTTP-2", "sched-1",
		[]string{"/CNCL/"}, nil, nil)
	code, body = postMT195Text(t, applyURL, unknown, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown :21:: status=%d body=%v, want 422", code, body)
	}
	if body["repair_id"] == nil || body["repair_id"] == "" {
		t.Errorf("repair_id = %v, want the parked repair record", body["repair_id"])
	}
	// Same :20:, different terms: a 422 conflict, never a
	// silent overwrite.
	conflict := mt296HTTPDoc("ANS-296-HTTP-1", mt296HTTPAnsweredRef,
		[]string{"/RJCR/"}, nil, []string{"BANK CONFIRMS THE NOTIFICATION"})
	code, body = postMT195Text(t, applyURL, conflict, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("conflict: status=%d body=%v, want 422", code, body)
	}
	// A second answer with no :77A: registers as-is: the
	// narrative is optional.
	second := mt296HTTPDoc("ANS-296-HTTP-3", mt296HTTPAnsweredRef,
		[]string{"/PDCR/"}, nil, []string{"STILL PENDING AT THE BANK"})
	code, body = postMT195Text(t, applyURL, second, "ops")
	if code != http.StatusOK {
		t.Fatalf("no :77A:: status=%d body=%v", code, body)
	}
	if body["reference"] != "ANS-296-HTTP-3" {
		t.Errorf("reference = %v, want ANS-296-HTTP-3", body["reference"])
	}
	// The metrics counter counts every apply served.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_mt296_applies_total 6") {
		t.Errorf("metrics exposition missing ledger_mt296_applies_total 6:\n%s", metricsBody)
	}
}

// The tier-(b) gate end to end (LG-469): export an MT295 query
// over HTTP, answer it with an inbound MT296 over HTTP, and verify
// the registry-backed registration, the replay, the unregistered
// shape's 422, and the metrics counter.
func TestMT296ApplyEndpoint_MT295TierRegistersAnswer(t *testing.T) {
	srv := mt195ExportTestServer(t)

	// Create a schedule (active) and pause it: only a schedule
	// whose notification premise is dead can be queried about
	// its MT292.
	code, body := postJSON(t, srv.URL+"/transfer-schedules", `{
		"schedule_id": "sched-296-http",
		"from_account": "saver",
		"to_account": "bob",
		"amount_cents": 5000,
		"currency": "USD",
		"interval": "24h",
		"next_run_at": "2026-11-12T09:00:00Z",
		"idempotency_key": "sched-296-http-key"
	}`)
	if code != http.StatusCreated {
		t.Fatalf("create schedule: status=%d, want 201 (body=%v)", code, body)
	}
	code, body = postJSON(t, srv.URL+"/transfer-schedules/sched-296-http/pause", `{}`)
	if code != http.StatusOK {
		t.Fatalf("pause schedule: status=%d, want 200 (body=%v)", code, body)
	}

	postMT295 := func(t *testing.T, reqBody string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/exports/mt295", strings.NewReader(reqBody))
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
	exportReq, err := json.Marshal(mt295ExportRequest{
		ScheduleID: "sched-296-http",
		RunSeq:     0,
		SenderLT:   "DEUTDEFF",
		ReceiverLT: "CHASUS33",
		Queries:    []string{"STAT"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	code, msg := postMT295(t, string(exportReq))
	if code != http.StatusOK {
		t.Fatalf("export MT295: status=%d, want 200 (body=%v)", code, msg)
	}
	// The export's :20: is the tier-(b) anchor: the inbound
	// MT296 answers it by naming it in :21:.
	mt295Ref := ""
	for _, ln := range strings.Split(msg, "\n") {
		if strings.HasPrefix(ln, ":20:") {
			mt295Ref = strings.TrimPrefix(ln, ":20:")
		}
	}
	if mt295Ref == "" || !strings.HasPrefix(mt295Ref, "MT295-") || len(mt295Ref) != 16 {
		t.Fatalf("export :20: = %q, want a 16-character MT295- reference", mt295Ref)
	}

	applyURL := srv.URL + "/exports/mt296/apply"
	doc := mt296HTTPDoc("ANS-296-HTTP-20", mt295Ref,
		[]string{"/CNCL/"}, nil, []string{"CONFIRMED"})
	code, body = postMT195Text(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("apply tier-(b): status=%d body=%v, want 200", code, body)
	}
	if body["reference"] != "ANS-296-HTTP-20" || body["related_reference"] != mt295Ref {
		t.Errorf("refs = %v/%v, want ANS-296-HTTP-20/%s", body["reference"], body["related_reference"], mt295Ref)
	}
	if body["duplicate"] == true {
		t.Errorf("duplicate = true on first apply, want false")
	}
	// Replay the same :20: with the same terms: idempotent,
	// duplicate set.
	code, body = postMT195Text(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("replay: status=%d body=%v", code, body)
	}
	if body["duplicate"] != true {
		t.Errorf("duplicate = %v, want true", body["duplicate"])
	}
	// A well-shaped MT295 :20: for a query never rendered is a
	// 422 (parked fail-closed in the repair queue), the error
	// body carrying the repair record ID — the shape alone is
	// not enough for tier (b).
	unregistered := mt296HTTPDoc("ANS-296-HTTP-21", "MT295-0123456789",
		[]string{"/CNCL/"}, nil, nil)
	code, body = postMT195Text(t, applyURL, unregistered, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("unregistered tier-(b) :21:: status=%d body=%v, want 422", code, body)
	}
	if body["repair_id"] == nil || body["repair_id"] == "" {
		t.Errorf("repair_id = %v, want the parked repair record", body["repair_id"])
	}
	// The metrics counter counts every apply served (register +
	// replay + unregistered = 3).
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_mt296_applies_total 3") {
		t.Errorf("metrics exposition missing ledger_mt296_applies_total 3:\n%s", metricsBody)
	}
}

// The apply handler's error for a parked answer names the unknown
// :21:, so an operator reading the 422 knows what is missing
// without opening the repair queue.
func TestMT296ApplyEndpoint_UnknownReferenceErrorNamesReference(t *testing.T) {
	srv := mt195ExportTestServer(t)
	doc := mt296HTTPDoc("ANS-296-HTTP-9", "NO-SUCH-MT292",
		[]string{"/CNCL/"}, nil, nil)
	code, body := postMT195Text(t, srv.URL+"/exports/mt296/apply", doc, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%v, want 422", code, body)
	}
	if errMsg, ok := body["error"].(string); !ok || !strings.Contains(errMsg, "NO-SUCH-MT292") {
		t.Errorf("error = %v, want it to name NO-SUCH-MT292", body["error"])
	}
	// The counter counts this rejected apply too.
	_, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if !strings.Contains(metricsBody, "ledger_mt296_applies_total 1") {
		t.Errorf("metrics exposition missing ledger_mt296_applies_total 1:\n%s", metricsBody)
	}
}
