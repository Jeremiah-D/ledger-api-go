package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// buildACHReturnFileHTTP renders a bank return file for the payout
// through HTTP-visible APIs only: the origination entry detail comes
// from ledger.GenerateNACHAFile, and one Addenda 98 record is spliced
// after each entry detail line. The trace number is the builder's
// documented contract: ODFI transit (8) + per-batch 7-digit sequence
// from 1.
func buildACHReturnFileHTTP(t *testing.T, payoutID string, amountCents int64, reasonCode string) string {
	t.Helper()
	const originatingDFI = "12104288"
	entry, err := ledger.PayoutToNACHAEntry(
		ledger.PayoutInstruction{ID: payoutID, AmountCents: amountCents},
		ledger.NACHAReceiver{Routing: "021000089", AccountNumber: "987654321", IndividualName: "ACME SUPPLIER"},
	)
	if err != nil {
		t.Fatalf("PayoutToNACHAEntry: %v", err)
	}
	file, err := ledger.GenerateNACHAFile(
		ledger.NACHAFileConfig{
			ImmediateDestination: "021000089",
			ImmediateOrigin:      "121042882",
			DestinationName:      "FEDERAL RESERVE",
			OriginName:           "FIRST NATIONAL",
			CreationTime:         time.Date(2026, 10, 21, 14, 30, 0, 0, time.UTC),
			FileIDModifier:       "A",
			ReferenceCode:        "RETURNS",
		},
		[]ledger.NACHABatch{{
			Config: ledger.NACHABatchConfig{
				ServiceClassCode:        "220",
				CompanyName:             "ACME CORP",
				CompanyID:               "1122334455",
				EntryClassCode:          "CCD",
				CompanyEntryDescription: "VENDOR PAY",
				EffectiveEntryDate:      time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC),
				OriginatingDFI:          originatingDFI,
				BatchNumber:             1,
			},
			Entries: []ledger.NACHAEntry{entry},
		}},
	)
	if err != nil {
		t.Fatalf("GenerateNACHAFile: %v", err)
	}
	trace := originatingDFI + "0000001"
	addenda := "7" + "98" + reasonCode + " " + trace +
		strings.Repeat(" ", 6) +
		"021000089"+strings.Repeat(" ", 6) +
		("RETURNED "+reasonCode)+strings.Repeat(" ", 37-len("RETURNED "+reasonCode)) +
		trace[:4] + strings.Repeat(" ", 10)
	if len(addenda) != 94 {
		t.Fatalf("addenda 98 length = %d, want 94", len(addenda))
	}
	var out []string
	for _, ln := range strings.Split(file, "\n") {
		out = append(out, ln)
		if strings.HasPrefix(ln, "6") {
			out = append(out, addenda)
		}
	}
	return strings.Join(out, "\n")
}

// submitPayoutHTTP drafts, approves, and submits one payout through
// the HTTP API, returning its ID.
func submitPayoutHTTP(t *testing.T, srvURL, id string, amountCents int64) {
	t.Helper()
	code, _ := postJSON(t, srvURL+"/payouts",
		fmt.Sprintf(`{"id":%q,"payee_account":"saver","amount_cents":%d,"destination":"FPS:6222000012345678","maker":"alice"}`, id, amountCents))
	if code != http.StatusCreated {
		t.Fatalf("create payout: status=%d", code)
	}
	code, _ = postJSON(t, srvURL+"/payouts/"+id+"/approve", `{"approver":"alice"}`)
	if code != http.StatusOK {
		t.Fatalf("approve payout: status=%d", code)
	}
	code, _ = postJSON(t, srvURL+"/payouts/"+id+"/submit", "")
	if code != http.StatusOK {
		t.Fatalf("submit payout: status=%d", code)
	}
}

func TestHTTPAChReturns(t *testing.T) {
	srv := seedPayoutServer(t)
	defer srv.Close()
	submitPayoutHTTP(t, srv.URL, "po-ach-1", 250_000)

	file := buildACHReturnFileHTTP(t, "po-ach-1", 250_000, "R01")
	payload, err := json.Marshal(map[string]string{"file": file})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	code, body := postJSONWithActor(t, srv.URL+"/ach/returns", string(payload), "ops")
	if code != http.StatusOK {
		t.Fatalf("POST /ach/returns: status=%d body=%v", code, body)
	}
	results, ok := body["results"].([]any)
	if !ok || len(results) != 1 {
		t.Fatalf("results = %v, want 1 item", body["results"])
	}
	item := results[0].(map[string]any)
	if item["payout_id"] != "po-ach-1" {
		t.Errorf("payout_id = %v", item["payout_id"])
	}
	if item["reason_code"] != "R01" {
		t.Errorf("reason_code = %v", item["reason_code"])
	}
	if item["reversal_id"] != "payout/po-ach-1/clearing/reversal" {
		t.Errorf("reversal_id = %v", item["reversal_id"])
	}
	if item["duplicate"] == true {
		t.Error("duplicate = true on first processing")
	}
	if _, hasErr := item["error"]; hasErr {
		t.Errorf("error = %v, want absent", item["error"])
	}
	// The payee is whole again.
	code, bal := getJSON(t, srv.URL+"/accounts/saver/balance")
	if code != http.StatusOK || bal["balance_cents"] != float64(1_000_000) {
		t.Errorf("saver balance = %v, want 1000000", bal)
	}

	// Replay: duplicate, no double booking.
	code, body = postJSONWithActor(t, srv.URL+"/ach/returns", string(payload), "ops")
	if code != http.StatusOK {
		t.Fatalf("replay: status=%d body=%v", code, body)
	}
	item = body["results"].([]any)[0].(map[string]any)
	if item["duplicate"] != true {
		t.Errorf("replay duplicate = %v, want true", item["duplicate"])
	}

	// A return file with an invalid reason code is refused whole.
	badFile := buildACHReturnFileHTTP(t, "po-ach-1", 250_000, "R99")
	badPayload, _ := json.Marshal(map[string]string{"file": badFile})
	code, body = postJSONWithActor(t, srv.URL+"/ach/returns", string(badPayload), "ops")
	if code != http.StatusBadRequest {
		t.Errorf("invalid reason code: status=%d body=%v, want 400", code, body)
	}

	// The metrics exposition carries the per-reason-code series.
	code, _, metricsBody := getBody(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(metricsBody, `ledger_ach_returns_total{reason_code="R01"} 1`) {
		t.Errorf("metrics missing ledger_ach_returns_total{reason_code=\"R01\"} 1")
	}
	if !strings.Contains(metricsBody, "ledger_ach_return_requests_total 3") {
		t.Errorf("metrics missing ledger_ach_return_requests_total 3")
	}
}
