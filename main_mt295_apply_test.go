package main

import (
	"net/http"
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
