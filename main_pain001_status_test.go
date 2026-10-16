package main

import (
	"net/http"
	"strings"
	"testing"
)

// pain001StatusTestSetup posts one transfer and exports it, returning
// the deterministic MsgId from the rendered document.
func pain001StatusTestSetup(t *testing.T, srvURL string) string {
	t.Helper()
	if code, _ := postJSON(t, srvURL+"/transfers",
		`{"transfer_id":"st1","from_account":"alice","to_account":"bob","amount_cents":9000,"currency":"USD"}`); code != http.StatusCreated {
		t.Fatalf("post transfer st1: status=%d", code)
	}
	if code, _ := postJSON(t, srvURL+"/transfers",
		`{"transfer_id":"st2","from_account":"alice","to_account":"bob","amount_cents":1000,"currency":"USD"}`); code != http.StatusCreated {
		t.Fatalf("post transfer st2: status=%d", code)
	}
	code, _, body := getBody(t, srvURL+"/exports/pain001?currency=USD")
	if code != http.StatusOK {
		t.Fatalf("GET /exports/pain001: status=%d body=%s", code, body)
	}
	const open, close = "<MsgId>", "</MsgId>"
	i, j := strings.Index(body, open), strings.Index(body, close)
	if i < 0 || j < 0 || j <= i {
		t.Fatalf("exported document has no MsgId:\n%s", body)
	}
	return body[i+len(open) : j]
}

func pain001StatusOf(t *testing.T, srvURL, msgID string) (int, map[string]any) {
	t.Helper()
	return getJSON(t, srvURL+"/exports/pain001/"+msgID+"/status")
}

// The full lifecycle through HTTP: export -> status -> receipts ->
// status -> settle -> status, with the state machine visible at every
// step.
func TestPain001StatusLifecycleHTTP(t *testing.T) {
	srv := pain001TestServer(t)
	msgID := pain001StatusTestSetup(t, srv.URL)

	code, rec := pain001StatusOf(t, srv.URL, msgID)
	if code != http.StatusOK {
		t.Fatalf("GET status: status=%d body=%v", code, rec)
	}
	if rec["state"] != "exported" {
		t.Errorf("state = %v, want exported", rec["state"])
	}
	txs, ok := rec["txs"].([]any)
	if !ok || len(txs) != 2 {
		t.Fatalf("txs = %v, want 2 transfers", rec["txs"])
	}

	// Bank receipts: accept both transfers.
	code, rec = postJSON(t, srv.URL+"/exports/pain001/"+msgID+"/receipts",
		`{"receipts":[{"tx_id":"st1","status":"accepted"},{"tx_id":"st2","status":"accepted","reason":""}]}`)
	if code != http.StatusOK {
		t.Fatalf("POST receipts: status=%d body=%v", code, rec)
	}
	if rec["state"] != "accepted" {
		t.Errorf("state = %v, want accepted", rec["state"])
	}

	// Operator reconcile confirmation settles the accepted transfers.
	code, rec = postJSONWithActor(t, srv.URL+"/exports/pain001/"+msgID+"/settle", `{}`, "night-ops")
	if code != http.StatusOK {
		t.Fatalf("POST settle: status=%d body=%v", code, rec)
	}
	if rec["state"] != "settled" {
		t.Errorf("state = %v, want settled", rec["state"])
	}

	code, rec = pain001StatusOf(t, srv.URL, msgID)
	if code != http.StatusOK || rec["state"] != "settled" {
		t.Errorf("final status: code=%d state=%v, want 200/settled", code, rec["state"])
	}
}

// Unknown instructions are 404 on all three endpoints.
func TestPain001StatusUnknown404(t *testing.T) {
	srv := pain001TestServer(t)
	if code, _ := pain001StatusOf(t, srv.URL, "pain001-nope"); code != http.StatusNotFound {
		t.Errorf("GET status unknown: status=%d, want 404", code)
	}
	if code, _ := postJSON(t, srv.URL+"/exports/pain001/pain001-nope/receipts",
		`{"receipts":[{"tx_id":"x","status":"accepted"}]}`); code != http.StatusNotFound {
		t.Errorf("POST receipts unknown: status=%d, want 404", code)
	}
	if code, _ := postJSON(t, srv.URL+"/exports/pain001/pain001-nope/settle", `{}`); code != http.StatusNotFound {
		t.Errorf("POST settle unknown: status=%d, want 404", code)
	}
}

// Illegal transitions are 422: rejecting an accepted transfer,
// receipting a transfer outside the instruction, and unknown statuses.
func TestPain001ReceiptsIllegal422(t *testing.T) {
	srv := pain001TestServer(t)
	msgID := pain001StatusTestSetup(t, srv.URL)
	url := srv.URL + "/exports/pain001/" + msgID + "/receipts"

	if code, _ := postJSON(t, url, `{"receipts":[{"tx_id":"st1","status":"accepted"}]}`); code != http.StatusOK {
		t.Fatalf("accept st1: status=%d", code)
	}
	for _, payload := range []string{
		`{"receipts":[{"tx_id":"st1","status":"rejected","reason":"AC03"}]}`, // accepted -> rejected: illegal
		`{"receipts":[{"tx_id":"ghost","status":"accepted"}]}`,               // unknown transfer
		`{"receipts":[{"tx_id":"st2","status":"pending"}]}`,                  // unknown status
	} {
		if code, body := postJSON(t, url, payload); code != http.StatusUnprocessableEntity {
			t.Errorf("POST %s: status=%d, want 422 (body=%v)", payload, code, body)
		}
	}
	// The refused batches left the record untouched.
	if code, rec := pain001StatusOf(t, srv.URL, msgID); code != http.StatusOK || rec["state"] != "exported" {
		t.Errorf("after refused batches: code=%d state=%v, want 200/exported", code, rec["state"])
	}
}

// Duplicate receipt delivery is a no-op: 200, same state, no duplicate
// audit events.
func TestPain001ReceiptsDuplicateNoOpHTTP(t *testing.T) {
	srv := pain001TestServer(t)
	msgID := pain001StatusTestSetup(t, srv.URL)
	url := srv.URL + "/exports/pain001/" + msgID + "/receipts"
	payload := `{"receipts":[{"tx_id":"st1","status":"accepted"},{"tx_id":"st2","status":"accepted"}]}`

	if code, _ := postJSON(t, url, payload); code != http.StatusOK {
		t.Fatalf("first receipts: status=%d", code)
	}
	code, rec := postJSON(t, url, payload)
	if code != http.StatusOK {
		t.Fatalf("duplicate receipts: status=%d body=%v", code, rec)
	}
	if rec["state"] != "accepted" {
		t.Errorf("state = %v, want accepted", rec["state"])
	}
}

// An empty batch is a 400: it would record nothing, so it is a client
// error rather than a no-op success.
func TestPain001ReceiptsEmpty400(t *testing.T) {
	srv := pain001TestServer(t)
	msgID := pain001StatusTestSetup(t, srv.URL)
	if code, _ := postJSON(t, srv.URL+"/exports/pain001/"+msgID+"/receipts", `{"receipts":[]}`); code != http.StatusBadRequest {
		t.Errorf("empty receipts: status=%d, want 400", code)
	}
	if code, _ := postJSON(t, srv.URL+"/exports/pain001/"+msgID+"/receipts", `{}`); code != http.StatusBadRequest {
		t.Errorf("missing receipts: status=%d, want 400", code)
	}
}

// The reconcile report discloses unsettled instructions in
// pain001_pending and drops them once settled.
func TestPain001ReconcilePendingDisclosureHTTP(t *testing.T) {
	srv := pain001TestServer(t)
	msgID := pain001StatusTestSetup(t, srv.URL)

	code, report := postJSON(t, srv.URL+"/reconcile", `{}`)
	if code != http.StatusOK {
		t.Fatalf("POST /reconcile: status=%d", code)
	}
	pending, ok := report["pain001_pending"].([]any)
	if !ok || len(pending) != 1 {
		t.Fatalf("pain001_pending = %v, want 1 row", report["pain001_pending"])
	}
	row := pending[0].(map[string]any)
	if row["msg_id"] != msgID {
		t.Errorf("pending msg_id = %v, want %q", row["msg_id"], msgID)
	}
	unsettled, ok := row["unsettled_tx_ids"].([]any)
	if !ok || len(unsettled) != 2 {
		t.Errorf("unsettled_tx_ids = %v, want [st1 st2]", row["unsettled_tx_ids"])
	}

	// Receipt + settle the instruction: the worklist clears.
	if code, _ := postJSON(t, srv.URL+"/exports/pain001/"+msgID+"/receipts",
		`{"receipts":[{"tx_id":"st1","status":"accepted"},{"tx_id":"st2","status":"accepted"}]}`); code != http.StatusOK {
		t.Fatalf("receipts: status=%d", code)
	}
	if code, _ := postJSON(t, srv.URL+"/exports/pain001/"+msgID+"/settle", `{}`); code != http.StatusOK {
		t.Fatalf("settle: status=%d", code)
	}
	code, report = postJSON(t, srv.URL+"/reconcile", `{}`)
	if code != http.StatusOK {
		t.Fatalf("POST /reconcile: status=%d", code)
	}
	if pending, _ := report["pain001_pending"].([]any); len(pending) != 0 {
		t.Errorf("pain001_pending after settle = %v, want empty", pending)
	}
}

// A rejected instruction stays visible in the pending disclosure: its
// transfers never settled, and the operator needs to see it.
func TestPain001ReconcilePendingShowsRejectedHTTP(t *testing.T) {
	srv := pain001TestServer(t)
	msgID := pain001StatusTestSetup(t, srv.URL)

	if code, _ := postJSON(t, srv.URL+"/exports/pain001/"+msgID+"/receipts",
		`{"receipts":[{"tx_id":"st1","status":"rejected","reason":"AC03"},{"tx_id":"st2","status":"rejected","reason":"AC03"}]}`); code != http.StatusOK {
		t.Fatalf("receipts: status=%d", code)
	}
	code, report := postJSON(t, srv.URL+"/reconcile", `{}`)
	if code != http.StatusOK {
		t.Fatalf("POST /reconcile: status=%d", code)
	}
	pending, ok := report["pain001_pending"].([]any)
	if !ok || len(pending) != 1 {
		t.Fatalf("pain001_pending = %v, want the rejected instruction disclosed", report["pain001_pending"])
	}
	if row := pending[0].(map[string]any); row["state"] != "rejected" {
		t.Errorf("pending state = %v, want rejected", row["state"])
	}
}
