package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// mt196HTTPDoc renders an MT196 answer: ref as :20:, queryRef as
// :21:, answers as the :76: answer set, and narrative as the
// :77A: note. An empty narrative omits :77A: entirely.
func mt196HTTPDoc(ref, queryRef string, answers, narrative []string) string {
	var sb strings.Builder
	sb.WriteString("{1:F01BANKDEFFXXX0000000000}\n")
	sb.WriteString("{2:I196BANKUS33XXXU3003}\n")
	sb.WriteString("{3:{108:" + ref + "}}\n")
	sb.WriteString("{4:\n")
	sb.WriteString(":20:" + ref + "\n")
	sb.WriteString(":21:" + queryRef + "\n")
	sb.WriteString(":76:" + answers[0] + "\n")
	for _, ln := range answers[1:] {
		sb.WriteString(ln + "\n")
	}
	if len(narrative) > 0 {
		sb.WriteString(":77A:" + narrative[0] + "\n")
		for _, ln := range narrative[1:] {
			sb.WriteString(ln + "\n")
		}
	}
	sb.WriteString("-}")
	return sb.String()
}

// exportMT195QueryHTTP exports the outbound MT195 query about
// payoutID and returns its derived :20: reference — the :21: an
// inbound MT196 answer must name.
func exportMT195QueryHTTP(t *testing.T, srvURL, payoutID string) string {
	t.Helper()
	body, err := json.Marshal(mt195ExportRequest{
		PayoutID:   payoutID,
		ReceiverLT: "BANKDEFF",
		Queries:    []string{"STAT"},
		Narrative:  []string{"Please confirm status of payment"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, srvURL+"/exports/mt195", strings.NewReader(string(body)))
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
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /exports/mt195: status=%d body=%s", resp.StatusCode, string(b))
	}
	for _, ln := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(ln, ":20:") {
			return strings.TrimPrefix(ln, ":20:")
		}
	}
	t.Fatalf("exported MT195 has no :20: line:\n%s", string(b))
	return ""
}

// Full flow through HTTP: submit a payout, ask the correspondent
// bank about it with an outbound MT195 query, register the
// correspondent's MT196 answer, replay the same :20:, and verify
// the error mapping and the metrics counter.
func TestMT196ApplyEndpoint_RegistersAnswer(t *testing.T) {
	srv := mt195ExportTestServer(t)
	payoutID := "po-mt196-app-1"
	submittedPayoutHTTP(t, srv.URL, payoutID)
	queryRef := exportMT195QueryHTTP(t, srv.URL, payoutID)
	applyURL := srv.URL + "/exports/mt196/apply"

	// The answer is the correspondent's words, verbatim: the
	// payout is submitted, its MT103 value date is the submit
	// day, and its gpi track is "tracked".
	valueDate := time.Now().UTC().Format("060102")
	doc := mt196HTTPDoc("ANS-196-HTTP-1", queryRef,
		[]string{"PAYOUT STATUS: submitted", "VALUE DATE: " + valueDate, "GPI STATUS: tracked"},
		[]string{"Please see the status above"})
	code, body := postMT195Text(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/mt196/apply: status=%d body=%v", code, body)
	}
	if body["reference"] != "ANS-196-HTTP-1" {
		t.Errorf("reference = %v, want ANS-196-HTTP-1", body["reference"])
	}
	if body["related_reference"] != queryRef || body["query_ref"] != queryRef {
		t.Errorf("related/query = %v/%v, want %q", body["related_reference"], body["query_ref"], queryRef)
	}
	if body["payout_id"] != payoutID {
		t.Errorf("payout_id = %v, want %q", body["payout_id"], payoutID)
	}
	answers, ok := body["answers"].([]any)
	if !ok || len(answers) != 3 || answers[0] != "PAYOUT STATUS: submitted" ||
		answers[1] != "VALUE DATE: "+valueDate || answers[2] != "GPI STATUS: tracked" {
		t.Errorf("answers = %v, want the three verbatim lines", body["answers"])
	}
	if body["duplicate"] == true {
		t.Errorf("duplicate = true on first apply, want false")
	}
	// A malformed FIN document is a 400.
	code, body = postMT195Text(t, applyURL, "{1:F01BANKDEFFXXX0000000000}", "ops")
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
	// queue).
	unknown := mt196HTTPDoc("ANS-196-HTTP-2", "no-such-query",
		[]string{"PAYOUT STATUS: submitted"}, nil)
	code, body = postMT195Text(t, applyURL, unknown, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown :21:: status=%d body=%v, want 422", code, body)
	}
	// Same :20:, different answers: a 422 conflict, never a
	// silent overwrite.
	conflict := mt196HTTPDoc("ANS-196-HTTP-1", queryRef,
		[]string{"PAYOUT STATUS: settled"}, nil)
	code, body = postMT195Text(t, applyURL, conflict, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("conflict: status=%d body=%v, want 422", code, body)
	}
	// The metrics counter counts every apply served.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_mt196_applies_total 5") {
		t.Errorf("metrics exposition missing ledger_mt196_applies_total 5:\n%s", metricsBody)
	}
}
