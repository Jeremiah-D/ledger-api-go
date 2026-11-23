package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// mt999HTTPDoc renders an MT999 notification: ref as :20:,
// relatedRef as :21: (empty omits :21:), narrative as the :79:
// lines.
func mt999HTTPDoc(ref, relatedRef string, narrative []string) string {
	var sb strings.Builder
	sb.WriteString("{1:F01BANKDEFFXXX0000000000}\n")
	sb.WriteString("{2:I999BANKUS33XXXU3003}\n")
	sb.WriteString("{3:{108:" + ref + "}}\n")
	sb.WriteString("{4:\n")
	sb.WriteString(":20:" + ref + "\n")
	if relatedRef != "" {
		sb.WriteString(":21:" + relatedRef + "\n")
	}
	sb.WriteString(":79:" + narrative[0] + "\n")
	for _, ln := range narrative[1:] {
		sb.WriteString(ln + "\n")
	}
	sb.WriteString("-}")
	return sb.String()
}

// exportMT999OutboundHTTP exports our outbound MT999 with :20:
// ref and returns the rendered FIN text — the :21: an inbound
// notification answering it must name.
func exportMT999OutboundHTTP(t *testing.T, srvURL, ref string) string {
	t.Helper()
	body, err := json.Marshal(mt999ExportRequest{
		SenderLT:             "BANKUS33",
		ReceiverLT:           "BANKDEFF",
		TransactionReference: ref,
		Narrative:            []string{"PLEASE ADVISE CASH POSITION"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, srvURL+"/exports/mt999", strings.NewReader(string(body)))
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
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /exports/mt999: status=%d body=%s", resp.StatusCode, string(b))
	}
	return string(b)
}

// Full flow through HTTP: export an outbound MT999, register the
// correspondent's MT999 reply, replay the same :20:, and verify
// the error mapping and the metrics counter.
func TestMT999ApplyEndpoint_RegistersNotification(t *testing.T) {
	srv := mt195ExportTestServer(t)
	outboundRef := "F999-HTTP-OUT"
	exportMT999OutboundHTTP(t, srv.URL, outboundRef)
	applyURL := srv.URL + "/exports/mt999/apply"

	doc := mt999HTTPDoc("F999-HTTP-IN", outboundRef,
		[]string{"CASH POSITION CONFIRMED", "BALANCE AS EXPECTED"})
	code, body := postMT195Text(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/mt999/apply: status=%d body=%v", code, body)
	}
	if body["reference"] != "F999-HTTP-IN" {
		t.Errorf("reference = %v, want F999-HTTP-IN", body["reference"])
	}
	if body["related_reference"] != outboundRef || body["outbound_reference"] != outboundRef {
		t.Errorf("related/outbound = %v/%v, want %q", body["related_reference"], body["outbound_reference"], outboundRef)
	}
	if body["payout_id"] != nil {
		t.Errorf("payout_id = %v, want absent (outbound anchor)", body["payout_id"])
	}
	narrative, ok := body["narrative"].([]any)
	if !ok || len(narrative) != 2 || narrative[0] != "CASH POSITION CONFIRMED" ||
		narrative[1] != "BALANCE AS EXPECTED" {
		t.Errorf("narrative = %v, want the two verbatim lines", body["narrative"])
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
	// queue), and the error body carries the repair record ID.
	unknown := mt999HTTPDoc("F999-HTTP-IN-2", "no-such-anchor",
		[]string{"ANSWER TO NOTHING WE SENT"})
	code, body = postMT195Text(t, applyURL, unknown, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown :21:: status=%d body=%v, want 422", code, body)
	}
	if body["repair_id"] == nil || body["repair_id"] == "" {
		t.Errorf("repair_id = %v, want the parked repair record", body["repair_id"])
	}
	// Same :20:, different narrative: a 422 conflict, never a
	// silent overwrite.
	conflict := mt999HTTPDoc("F999-HTTP-IN", outboundRef,
		[]string{"A different note"})
	code, body = postMT195Text(t, applyURL, conflict, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("conflict: status=%d body=%v, want 422", code, body)
	}
	// The second anchor tier: a :21: naming a payout registers
	// against that payout.
	payoutID := "po-mt999-http-2"
	submittedPayoutHTTP(t, srv.URL, payoutID)
	payoutDoc := mt999HTTPDoc("F999-HTTP-IN-3", payoutID,
		[]string{"CHARGES FOR PAYMENT", "WILL BE ADVISED SEPARATELY"})
	code, body = postMT195Text(t, applyURL, payoutDoc, "ops")
	if code != http.StatusOK {
		t.Fatalf("payout anchor: status=%d body=%v", code, body)
	}
	if body["payout_id"] != payoutID {
		t.Errorf("payout_id = %v, want %q", body["payout_id"], payoutID)
	}
	if body["outbound_reference"] != nil {
		t.Errorf("outbound_reference = %v, want absent (payout anchor)", body["outbound_reference"])
	}
	// An empty :21: registers a standalone notification.
	standalone := mt999HTTPDoc("F999-HTTP-IN-4", "",
		[]string{"GENERAL CASH MANAGEMENT NOTE"})
	code, body = postMT195Text(t, applyURL, standalone, "ops")
	if code != http.StatusOK {
		t.Fatalf("standalone: status=%d body=%v", code, body)
	}
	if body["related_reference"] != nil || body["outbound_reference"] != nil || body["payout_id"] != nil {
		t.Errorf("standalone anchors = %v/%v/%v, want all absent", body["related_reference"], body["outbound_reference"], body["payout_id"])
	}
	// The metrics counter counts every apply served.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_mt999_applies_total 7") {
		t.Errorf("metrics exposition missing ledger_mt999_applies_total 7:\n%s", metricsBody)
	}
}
