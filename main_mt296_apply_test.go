package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
// first 10 hex characters: 117c924981) — that this ledger never
// rendered: no export is ever registered for it, so since
// LG-473 it is the tier-(a) shape that parks, not the tier-(a)
// anchor.
const mt296HTTPAnsweredRef = "MT292-117c924981"

// Full flow through HTTP: register a correspondent bank's MT296
// answer, replay the same :20:, and verify the error mapping and
// the metrics counter. Since LG-473 the tier-(a) :21: must name
// an MT292 this ledger rendered: the schedule is paused, its
// MT292 exported over HTTP, and the export's :20: is the anchor.
func TestMT296ApplyEndpoint_RegistersAnswer(t *testing.T) {
	l, sched := mt292HTTPSetup(t)
	if _, err := l.PauseTransferSchedule(sched.ID); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	// Register the outbound MT292 over HTTP; its :20: is the
	// tier-(a) anchor for the answers below.
	q := url.Values{}
	q.Set("schedule_id", sched.ID)
	q.Set("servicing_bic", "CITIUS33")
	q.Set("original_date", "2026-11-10")
	q.Set("reason", "Schedule paused by operator")
	code, _, msg := mt292ExportHTTPGet(t, srv.URL+"/exports/mt292?"+q.Encode(), "ledger-operator")
	if code != http.StatusOK {
		t.Fatalf("export MT292: status=%d, want 200", code)
	}
	mt292Ref := ""
	for _, ln := range strings.Split(msg, "\n") {
		if strings.HasPrefix(ln, ":20:") {
			mt292Ref = strings.TrimPrefix(ln, ":20:")
		}
	}
	if mt292Ref == "" || !strings.HasPrefix(mt292Ref, "MT292-") || len(mt292Ref) != 16 {
		t.Fatalf("export :20: = %q, want a 16-character MT292- reference", mt292Ref)
	}

	applyURL := srv.URL + "/exports/mt296/apply"

	doc := mt296HTTPDoc("ANS-296-HTTP-1", mt292Ref,
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
	if body["related_reference"] != mt292Ref {
		t.Errorf("related_reference = %v, want %s", body["related_reference"], mt292Ref)
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
	conflict := mt296HTTPDoc("ANS-296-HTTP-1", mt292Ref,
		[]string{"/RJCR/"}, nil, []string{"BANK CONFIRMS THE NOTIFICATION"})
	code, body = postMT195Text(t, applyURL, conflict, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("conflict: status=%d body=%v, want 422", code, body)
	}
	// A second answer with no :77A: registers as-is: the
	// narrative is optional.
	second := mt296HTTPDoc("ANS-296-HTTP-3", mt292Ref,
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

// The tier-(a) gate end to end (LG-473): a well-shaped MT292
// :20: for a cancellation request this ledger never rendered is
// a 422 (parked fail-closed in the repair queue), the error body
// carrying the repair record ID — the shape alone is not enough.
func TestMT296ApplyEndpoint_MT292TierUnregisteredGoesToRepair(t *testing.T) {
	srv := mt195ExportTestServer(t)
	doc := mt296HTTPDoc("ANS-296-HTTP-4", mt296HTTPAnsweredRef,
		[]string{"/CNCL/"}, nil, nil)
	code, body := postMT195Text(t, srv.URL+"/exports/mt296/apply", doc, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("unregistered tier-(a) :21:: status=%d body=%v, want 422", code, body)
	}
	if body["repair_id"] == nil || body["repair_id"] == "" {
		t.Errorf("repair_id = %v, want the parked repair record", body["repair_id"])
	}
	// The counter counts this rejected apply too.
	_, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if !strings.Contains(metricsBody, "ledger_mt296_applies_total 1") {
		t.Errorf("metrics exposition missing ledger_mt296_applies_total 1:\n%s", metricsBody)
	}
}

// The tier-(b) gate end to end (LG-469): export an MT295 query
// over HTTP, answer it with an inbound MT296 over HTTP, and verify
// the registry-backed registration, the replay, the unregistered
// shape's 422, and the metrics counter.
func TestMT296ApplyEndpoint_MT295TierRegistersAnswer(t *testing.T) {
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
		"schedule_id": "sched-296-http",
		"from_account": "alice",
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

	// Register the MT292 first: the MT292 tier of the MT295
	// export is registry-anchored (LG-476), so nothing is
	// queryable before this export lands.
	code, _, mt292Msg := getBody(t, srv.URL+"/exports/mt292?schedule_id=sched-296-http&servicing_bic=CITIUS33&original_date=2026-11-10&reason=Schedule+paused+by+operator")
	if code != http.StatusOK {
		t.Fatalf("register MT292: status=%d, want 200 (body=%v)", code, mt292Msg)
	}
	if !strings.Contains(mt292Msg, ":20:"+mt295HTTPExpectedMT292Ref("sched-296-http", 0)) {
		t.Fatalf("registered MT292 missing :20:%s, body:\n%s", mt295HTTPExpectedMT292Ref("sched-296-http", 0), mt292Msg)
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

// The tier-(c) gate end to end (LG-472): export an MT202 transfer
// over HTTP, answer it with an inbound MT296 over HTTP, and verify
// the registry-backed registration, the replay, and the metrics
// counter. The MT202's :20: ("MT202REF1") matches neither the
// tier-(a) nor the tier-(b) shape: only tier (c) can anchor it.
func TestMT296ApplyEndpoint_MT202TierRegistersAnswer(t *testing.T) {
	srv := mt195ExportTestServer(t)

	code, _, _ := postTextResp(t, srv.URL+"/exports/mt202", mt202ValidBody)
	if code != http.StatusOK {
		t.Fatalf("export MT202: status=%d, want 200", code)
	}

	applyURL := srv.URL + "/exports/mt296/apply"
	doc := mt296HTTPDoc("ANS-296-HTTP-30", "MT202REF1",
		[]string{"/CNCL/"}, nil, []string{"CONFIRMED"})
	code, body := postMT195Text(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("apply tier-(c): status=%d body=%v, want 200", code, body)
	}
	if body["reference"] != "ANS-296-HTTP-30" || body["related_reference"] != "MT202REF1" {
		t.Errorf("refs = %v/%v, want ANS-296-HTTP-30/MT202REF1", body["reference"], body["related_reference"])
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
	// The metrics counter counts every apply served (register +
	// replay = 2; the MT202 export counts under its own counter).
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_mt296_applies_total 2") {
		t.Errorf("metrics exposition missing ledger_mt296_applies_total 2:\n%s", metricsBody)
	}
}

// Tier (c) errors end to end (LG-472): a :21: naming a transfer
// this ledger never rendered is a 422, a :20: reused for different
// terms is a 422, and a replay after the conflict still returns
// the first registration.
func TestMT296ApplyEndpoint_MT202TierUnknownAndConflict(t *testing.T) {
	srv := mt195ExportTestServer(t)

	code, _, _ := postTextResp(t, srv.URL+"/exports/mt202", mt202ValidBody)
	if code != http.StatusOK {
		t.Fatalf("export MT202: status=%d, want 200", code)
	}

	applyURL := srv.URL + "/exports/mt296/apply"
	// A :21: naming no rendered transfer is a 422 (parked
	// fail-closed in the repair queue), the error body carrying
	// the repair record ID — the lookup is the whole tier.
	unknown := mt296HTTPDoc("ANS-296-HTTP-31", "MT202REF9",
		[]string{"/CNCL/"}, nil, nil)
	code, body := postMT195Text(t, applyURL, unknown, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("unregistered tier-(c) :21:: status=%d body=%v, want 422", code, body)
	}
	if body["repair_id"] == nil || body["repair_id"] == "" {
		t.Errorf("repair_id = %v, want the parked repair record", body["repair_id"])
	}
	// Register the answer against the exported transfer.
	doc := mt296HTTPDoc("ANS-296-HTTP-32", "MT202REF1",
		[]string{"/CNCL/"}, nil, []string{"CONFIRMED"})
	code, body = postMT195Text(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("apply tier-(c): status=%d body=%v, want 200", code, body)
	}
	// Same :20:, different terms: a 422 conflict, never a
	// silent overwrite.
	conflict := mt296HTTPDoc("ANS-296-HTTP-32", "MT202REF1",
		[]string{"/RJCR/"}, nil, []string{"CONFIRMED"})
	code, body = postMT195Text(t, applyURL, conflict, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("conflict: status=%d body=%v, want 422", code, body)
	}
	// The conflict overwrote nothing: the original answer still
	// replays with duplicate set.
	code, body = postMT195Text(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("replay after conflict: status=%d body=%v", code, body)
	}
	if body["duplicate"] != true {
		t.Errorf("duplicate = %v, want true", body["duplicate"])
	}
	answers, ok := body["answers"].([]any)
	if !ok || len(answers) != 1 || answers[0] != "/CNCL/" {
		t.Errorf("answers = %v, want [/CNCL/] (the first registration)", body["answers"])
	}
	// The metrics counter counts every apply served (unknown +
	// register + conflict + replay = 4).
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_mt296_applies_total 4") {
		t.Errorf("metrics exposition missing ledger_mt296_applies_total 4:\n%s", metricsBody)
	}
}
