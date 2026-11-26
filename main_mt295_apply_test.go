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

// mt295HTTPDoc renders one inbound MT295 query as FIN text: ref
// as :20:, relatedRef as :21: (the queried message's :20:),
// queries as the :75: lines, narrative as the :77A: lines (empty
// omits :77A:), description as the :79: lines (empty omits
// :79:). There is no ExportMT295, so the fixture is hand-built
// FIN text, the MT292 inbound convention.
func mt295HTTPDoc(ref, relatedRef string, queries, narrative, description []string) string {
	var b strings.Builder
	b.WriteString("{1:F01CITIUS33XXX0000000000}\n")
	b.WriteString("{2:I295NWBKGB2LXXXU3003}\n")
	b.WriteString("{3:{108:" + ref + "}}\n")
	b.WriteString("{4:\n")
	b.WriteString(":20:" + ref + "\n")
	b.WriteString(":21:" + relatedRef + "\n")
	b.WriteString(":75:" + queries[0] + "\n")
	for _, ln := range queries[1:] {
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

// mt295SeedMT210Doc renders one inbound MT210 notification as FIN
// text, the anchor the HTTP test's MT295 queries ask about. The
// account is the test server's saver IBAN
// (DE89370400440532013000), so the notification resolves.
func mt295SeedMT210Doc(msgID string) string {
	return "{1:F01NWBKGB2LXXX0000000000}\n" +
		"{2:I210CITIUS33XXXU3003}\n" +
		"{3:{108:" + msgID + "}}\n" +
		"{4:\n" +
		":20:" + msgID + "\n" +
		":25:DE89370400440532013000\n" +
		":30:261124\n" +
		":21:sched-1/run/1\n" +
		":32B:USD100,00\n" +
		":52A:DEUTDEFF\n" +
		"-}"
}

// Full flow through HTTP: register a correspondent bank's MT295
// query about an MT210 notification, replay the same :20:, and
// verify the error mapping and the metrics counter.
func TestMT295ApplyEndpoint_RegistersQuery(t *testing.T) {
	srv := mt195ExportTestServer(t)
	applyURL := srv.URL + "/exports/mt295/apply"

	// Seed the anchor first: an MT210 notification whose :20:
	// the query's :21: will name.
	code, body := postMT195Text(t, srv.URL+"/exports/mt210/apply", mt295SeedMT210Doc("NTF-295-HTTP"), "ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/mt210/apply seed: status=%d body=%v", code, body)
	}

	doc := mt295HTTPDoc("QRY-295-HTTP-1", "NTF-295-HTTP",
		[]string{"STAT", "PLEASE ADVISE STATUS"},
		[]string{"OPERATOR NOTE"},
		[]string{"BANK ASKS FOR THE VALUE DATE"})
	code, body = postMT195Text(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/mt295/apply: status=%d body=%v", code, body)
	}
	if body["reference"] != "QRY-295-HTTP-1" {
		t.Errorf("reference = %v, want QRY-295-HTTP-1", body["reference"])
	}
	if body["related_reference"] != "NTF-295-HTTP" {
		t.Errorf("related_reference = %v, want NTF-295-HTTP", body["related_reference"])
	}
	if body["anchored_kind"] != "mt210" {
		t.Errorf("anchored_kind = %v, want mt210", body["anchored_kind"])
	}
	queries, ok := body["queries"].([]any)
	if !ok || len(queries) != 2 || queries[0] != "STAT" {
		t.Errorf("queries = %v, want [STAT ...]", body["queries"])
	}
	if body["duplicate"] == true {
		t.Errorf("duplicate = true on first apply, want false")
	}
	// A malformed FIN document is a 400.
	code, body = postMT195Text(t, applyURL, "{1:F01CITIUS33XXX0000000000}", "ops")
	if code != http.StatusBadRequest {
		t.Fatalf("malformed: status=%d body=%v, want 400", code, body)
	}
	// A query with no :75: is a 400 — the question is mandatory.
	noQuestion := "{1:F01CITIUS33XXX0000000000}\n" +
		"{2:I295NWBKGB2LXXXU3003}\n" +
		"{3:{108:QRY-295-HTTP-NQ}}\n" +
		"{4:\n" +
		":20:QRY-295-HTTP-NQ\n" +
		":21:NTF-295-HTTP\n" +
		":79:PLEASE ADVISE\n" +
		"-}"
	code, body = postMT195Text(t, applyURL, noQuestion, "ops")
	if code != http.StatusBadRequest {
		t.Fatalf("no :75:: status=%d body=%v, want 400", code, body)
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
	unknown := mt295HTTPDoc("QRY-295-HTTP-2", "NO-SUCH-MSG",
		[]string{"STAT"}, nil, nil)
	code, body = postMT195Text(t, applyURL, unknown, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown :21:: status=%d body=%v, want 422", code, body)
	}
	if body["repair_id"] == nil || body["repair_id"] == "" {
		t.Errorf("repair_id = %v, want the parked repair record", body["repair_id"])
	}
	// Same :20:, different terms: a 422 conflict, never a
	// silent overwrite.
	conflict := mt295HTTPDoc("QRY-295-HTTP-1", "NTF-295-HTTP",
		[]string{"DUPL"}, nil, []string{"BANK ASKS FOR THE VALUE DATE"})
	code, body = postMT195Text(t, applyURL, conflict, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("conflict: status=%d body=%v, want 422", code, body)
	}
	// A second query with no :77A: registers as-is: the
	// narrative is optional.
	second := mt295HTTPDoc("QRY-295-HTTP-3", "NTF-295-HTTP",
		[]string{"DUPL"}, nil, []string{"STILL PENDING AT THE BANK"})
	code, body = postMT195Text(t, applyURL, second, "ops")
	if code != http.StatusOK {
		t.Fatalf("no :77A:: status=%d body=%v", code, body)
	}
	if body["reference"] != "QRY-295-HTTP-3" {
		t.Errorf("reference = %v, want QRY-295-HTTP-3", body["reference"])
	}
	// The metrics counter counts every apply served: register,
	// malformed, no-:75:, replay, unknown, conflict, second.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_mt295_applies_total 7") {
		t.Errorf("metrics exposition missing ledger_mt295_applies_total 7:\n%s", metricsBody)
	}
}

// Full flow through HTTP for the outbound tiers (LG-474):
// register an outbound MT292 over GET /exports/mt292, then ask
// about it with POST /exports/mt295/apply — the query registers
// against the outbound cancellation request, kind
// outbound-mt292.
func TestMT295ApplyEndpoint_AnchorsOutboundMT292(t *testing.T) {
	l, sched := mt292HTTPSetup(t)
	if _, err := l.PauseTransferSchedule(sched.ID); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()
	applyURL := srv.URL + "/exports/mt295/apply"

	// Register the outbound MT292: the deterministic :20: for
	// sched-1 run 1.
	q := url.Values{}
	q.Set("schedule_id", sched.ID)
	q.Set("servicing_bic", "CITIUS33")
	q.Set("original_date", "2026-11-10")
	q.Set("reason", "Schedule paused by operator")
	code, doc292, _ := getText(t, srv.URL+"/exports/mt292?"+q.Encode())
	if code != http.StatusOK {
		t.Fatalf("export MT292: status=%d, want 200", code)
	}
	ref := mt292DocRef(t, doc292)

	doc := mt295HTTPDoc("QRY-295-HTTP-O29", ref,
		[]string{"STAT"}, []string{"OPERATOR NOTE"}, nil)
	code, body := postMT195Text(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/mt295/apply: status=%d body=%v", code, body)
	}
	if body["reference"] != "QRY-295-HTTP-O29" {
		t.Errorf("reference = %v, want QRY-295-HTTP-O29", body["reference"])
	}
	if body["related_reference"] != ref {
		t.Errorf("related_reference = %v, want %v", body["related_reference"], ref)
	}
	if body["anchored_kind"] != "outbound-mt292" {
		t.Errorf("anchored_kind = %v, want outbound-mt292", body["anchored_kind"])
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
}

// The outbound MT296 tier over HTTP (LG-478): register an
// outbound MT296 answer first, then ask about it — the :21: is
// the answer's own :20:, caller-supplied, and the query anchors
// to mt296Exports with kind outbound-mt296.
func TestMT295ApplyEndpoint_AnchorsOutboundMT296(t *testing.T) {
	srv := mt195ExportTestServer(t)
	applyURL := srv.URL + "/exports/mt295/apply"

	// Seed the anchor first: an MT210 notification, then the
	// inbound MT292 cancellation request the MT296 answers.
	code, body := postMT195Text(t, srv.URL+"/exports/mt210/apply", mt295SeedMT210Doc("NTF-296-HTTP-478"), "ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/mt210/apply seed: status=%d body=%v", code, body)
	}
	code, body = postMT292JSON(t, srv.URL+"/exports/mt292/apply",
		mt292ApplyDoc("CXL-296-HTTP-478", "NTF-296-HTTP-478", "210261124", "Client withdrew the order"), "ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/mt292/apply seed: status=%d body=%v", code, body)
	}
	// Register the outbound MT296 answer: the query anchors
	// against this registry, so nothing is queryable before
	// this export lands.
	mt296Body, err := json.Marshal(mt296ExportRequest{
		SenderLT:             "DEUTDEFF",
		ReceiverLT:           "CHASUS33",
		TransactionReference: "ANS-296-HTTP-478",
		RelatedReference:     "CXL-296-HTTP-478",
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
	if !strings.Contains(string(b), ":20:ANS-296-HTTP-478") {
		t.Fatalf("registered MT296 missing :20:ANS-296-HTTP-478, body:\n%s", string(b))
	}

	doc := mt295HTTPDoc("QRY-295-HTTP-O96", "ANS-296-HTTP-478",
		[]string{"STAT"}, []string{"OPERATOR NOTE"}, nil)
	code, body = postMT195Text(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/mt295/apply: status=%d body=%v", code, body)
	}
	if body["reference"] != "QRY-295-HTTP-O96" {
		t.Errorf("reference = %v, want QRY-295-HTTP-O96", body["reference"])
	}
	if body["related_reference"] != "ANS-296-HTTP-478" {
		t.Errorf("related_reference = %v, want ANS-296-HTTP-478", body["related_reference"])
	}
	if body["anchored_kind"] != "outbound-mt296" {
		t.Errorf("anchored_kind = %v, want outbound-mt296", body["anchored_kind"])
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
}

// The shape alone is not enough over HTTP either (LG-474): a
// :21: shaped like our outbound MT292's :20: that names no
// rendered MT292 is a 422, and the error body carries the repair
// record ID.
func TestMT295ApplyEndpoint_UnregisteredOutboundShapeIs422(t *testing.T) {
	srv := mt195ExportTestServer(t)
	applyURL := srv.URL + "/exports/mt295/apply"

	doc := mt295HTTPDoc("QRY-295-HTTP-422", "MT292-0123456789",
		[]string{"STAT"}, nil, nil)
	code, body := postMT195Text(t, applyURL, doc, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("unregistered outbound-shaped :21:: status=%d body=%v, want 422", code, body)
	}
	if body["repair_id"] == nil || body["repair_id"] == "" {
		t.Errorf("repair_id = %v, want the parked repair record", body["repair_id"])
	}
}
