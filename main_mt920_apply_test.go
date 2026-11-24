package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// mt920HTTPDoc renders an inbound MT920 request through the
// outbound export — the POST /exports/mt920/apply body fixture.
func mt920HTTPDoc(t *testing.T, m ledger.MT920) string {
	t.Helper()
	msg, err := ledger.ExportMT920(m)
	if err != nil {
		t.Fatalf("ExportMT920: %v", err)
	}
	return msg
}

// mt920HTTPRequest builds a two-sequence inbound request for the
// HTTP tests: a 940 for the test server's seeded "saver" account
// and a 942 for its registered IBAN, with a debit floor limit.
func mt920HTTPRequest(ref string) ledger.MT920 {
	return ledger.MT920{
		SenderLT:             "BANKUS33",
		ReceiverLT:           "BANKDEFF",
		TransactionReference: ref,
		Sequences: []ledger.MT920Sequence{
			{RequestedType: "940", Account: "saver"},
			{RequestedType: "942", Account: "DE89370400440532013000",
				FloorLimitDebit: &ledger.MT920FloorLimit{Currency: "USD", AmountCents: 100000}},
		},
	}
}

// Full flow through HTTP: register a counterparty bank's MT920
// report request, replay the same :20:, and verify the error
// mapping and the metrics counter.
func TestMT920ApplyEndpoint_RegistersRequest(t *testing.T) {
	srv := mt195ExportTestServer(t)
	applyURL := srv.URL + "/exports/mt920/apply"

	doc := mt920HTTPDoc(t, mt920HTTPRequest("R920-HTTP-IN-1"))
	code, body := postMT195Text(t, applyURL, doc, "ops")
	if code != http.StatusOK {
		t.Fatalf("POST /exports/mt920/apply: status=%d body=%v", code, body)
	}
	if body["reference"] != "R920-HTTP-IN-1" {
		t.Errorf("reference = %v, want R920-HTTP-IN-1", body["reference"])
	}
	requested, ok := body["requested"].([]any)
	if !ok || len(requested) != 2 || requested[0] != "940" || requested[1] != "942" {
		t.Errorf("requested = %v, want [940 942]", body["requested"])
	}
	accounts, ok := body["accounts"].([]any)
	if !ok || len(accounts) != 2 || accounts[0] != "saver" ||
		accounts[1] != "DE89370400440532013000" {
		t.Errorf("accounts = %v, want [saver DE89370400440532013000]", body["accounts"])
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
	// An unknown :25: is a 422 (parked fail-closed in the repair
	// queue), and the error body carries the repair record ID.
	unknown := mt920HTTPDoc(t, ledger.MT920{
		SenderLT:             "BANKUS33",
		ReceiverLT:           "BANKDEFF",
		TransactionReference: "R920-HTTP-IN-2",
		Sequences: []ledger.MT920Sequence{
			{RequestedType: "940", Account: "NO-SUCH-ACCOUNT"},
		},
	})
	code, body = postMT195Text(t, applyURL, unknown, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown :25:: status=%d body=%v, want 422", code, body)
	}
	if body["repair_id"] == nil || body["repair_id"] == "" {
		t.Errorf("repair_id = %v, want the parked repair record", body["repair_id"])
	}
	// Same :20:, different terms: a 422 conflict, never a
	// silent overwrite.
	conflict := mt920HTTPDoc(t, ledger.MT920{
		SenderLT:             "BANKUS33",
		ReceiverLT:           "BANKDEFF",
		TransactionReference: "R920-HTTP-IN-1",
		Sequences: []ledger.MT920Sequence{
			{RequestedType: "941", Account: "saver"},
		},
	})
	code, body = postMT195Text(t, applyURL, conflict, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("conflict: status=%d body=%v, want 422", code, body)
	}
	// A sequence with no :25: registers as-is: an account-less
	// request for a whole report needs no anchor.
	noAccount := mt920HTTPDoc(t, ledger.MT920{
		SenderLT:             "BANKUS33",
		ReceiverLT:           "BANKDEFF",
		TransactionReference: "R920-HTTP-IN-3",
		Sequences: []ledger.MT920Sequence{
			{RequestedType: "950"},
		},
	})
	code, body = postMT195Text(t, applyURL, noAccount, "ops")
	if code != http.StatusOK {
		t.Fatalf("no :25:: status=%d body=%v", code, body)
	}
	if body["reference"] != "R920-HTTP-IN-3" {
		t.Errorf("reference = %v, want R920-HTTP-IN-3", body["reference"])
	}
	// The metrics counter counts every apply served.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, "ledger_mt920_applies_total 6") {
		t.Errorf("metrics exposition missing ledger_mt920_applies_total 6:\n%s", metricsBody)
	}
}

// The apply handler's error for a parked request names the
// unknown account, so an operator reading the 422 knows what is
// missing without opening the repair queue.
func TestMT920ApplyEndpoint_UnknownAccountErrorNamesAccount(t *testing.T) {
	srv := mt195ExportTestServer(t)
	doc := mt920HTTPDoc(t, ledger.MT920{
		SenderLT:             "BANKUS33",
		ReceiverLT:           "BANKDEFF",
		TransactionReference: "R920-HTTP-IN-9",
		Sequences: []ledger.MT920Sequence{
			{RequestedType: "941", Account: "MISSING-ACCT-7"},
		},
	})
	code, body := postMT195Text(t, srv.URL+"/exports/mt920/apply", doc, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%v, want 422", code, body)
	}
	if errMsg, ok := body["error"].(string); !ok || !strings.Contains(errMsg, "MISSING-ACCT-7") {
		t.Errorf("error = %v, want it to name MISSING-ACCT-7", body["error"])
	}
	// The counter counts this rejected apply too.
	_, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if !strings.Contains(metricsBody, "ledger_mt920_applies_total 1") {
		t.Errorf("metrics exposition missing ledger_mt920_applies_total 1:\n%s", metricsBody)
	}
}
