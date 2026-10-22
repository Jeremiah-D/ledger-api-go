package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// TestRepairFixEndpoint covers POST /repairs/{id}/fix (LG-177): the
// repair lifecycle through HTTP. The module-level tests
// (ledger_repair_test.go) own the queue semantics; this is the HTTP
// contract. Repair IDs are slash-free ("repair-1"), so a plain
// single-segment route wildcard carries them.
func TestRepairFixEndpoint(t *testing.T) {
	l := ledger.New(ledger.WithPayoutClearingAccount("payout-clearing"))
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	rec, err := l.EnqueueRepair(ledger.PayoutInstruction{
		ID:             "po-http-177",
		PayeeAccount:   "saver",
		AmountCents:    50000,
		Destination:    "", // the STP failure: missing destination
		Maker:          "alice",
		IdempotencyKey: "idem-po-http-177",
	}, ledger.RepairReasonMissingField, "ops")
	if err != nil {
		t.Fatalf("EnqueueRepair: %v", err)
	}
	fixURL := srv.URL + "/repairs/" + rec.ID + "/fix"

	// Unknown repair ID is a 404.
	code, _ := postJSONWithActor(t, srv.URL+"/repairs/repair-999/fix", `{"fixed_fields":{}}`, "ops")
	if code != http.StatusNotFound {
		t.Errorf("unknown repair: status = %d, want 404", code)
	}

	// An unknown fixed-field name fails closed with a 400, before
	// any state is touched.
	code, _ = postJSONWithActor(t, fixURL, `{"fixed_fields":{"bogus_field":"x"}}`, "ops")
	if code != http.StatusBadRequest {
		t.Errorf("unknown field: status = %d, want 400", code)
	}

	// A resubmit that still fails the STP gate is a 422, and the
	// record stays open for another attempt.
	code, body := postJSONWithActor(t, fixURL, `{"fixed_fields":{}}`, "ops")
	if code != http.StatusUnprocessableEntity {
		t.Errorf("still-broken resubmit: status = %d, want 422 (body: %v)", code, body)
	}

	// Completing the destination resubmits through the STP gate: 200
	// with the payout draft.
	code, body = postJSONWithActor(t, fixURL, `{"fixed_fields":{"destination":"FPS:6222000012345678"}}`, "ops")
	if code != http.StatusOK {
		t.Fatalf("successful fix: status = %d, want 200 (body: %v)", code, body)
	}
	payout, ok := body["payout"].(map[string]any)
	if !ok {
		t.Fatalf("response has no payout object: %v", body)
	}
	if id, _ := payout["id"].(string); id != "po-http-177" {
		t.Errorf("payout id = %q, want po-http-177", id)
	}
	if key, _ := payout["idempotency_key"].(string); key != "idem-po-http-177" {
		t.Errorf("payout idempotency key = %q, want the original instruction's key", key)
	}

	// A fix on the now-terminal record is a 409.
	code, _ = postJSONWithActor(t, fixURL, `{"fixed_fields":{}}`, "ops")
	if code != http.StatusConflict {
		t.Errorf("terminal record: status = %d, want 409", code)
	}
}
