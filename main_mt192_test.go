package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Full MT192 flow through HTTP: draft, approve, submit, then export
// the SWIFT FIN cancellation request, exercising the error mapping
// and the metrics counter.
func TestHTTPMT192Export(t *testing.T) {
	srv := mt103TestServer(t)
	submittedPayoutHTTP(t, srv.URL, "po-mt192-http-1")

	// Happy path with an explicit original value date and reason.
	code, msg := postText(t, srv.URL+"/exports/mt192",
		`{"payout_id":"po-mt192-http-1","receiver_lt":"BANKDEFFXXXX","original_value_date":"2026-11-10","reason":"Duplicate payment, please cancel"}`)
	if code != http.StatusOK {
		t.Fatalf("export: status=%d, want 200, body=%s", code, msg)
	}
	// :20: is the deterministic derivation, exactly 16 characters.
	sum := sha256.Sum256([]byte("po-mt192-http-1"))
	ref := "MT192-" + hex.EncodeToString(sum[:])[:10]
	if !strings.Contains(msg, "{3:{108:"+ref+"}}\n") {
		t.Errorf("block 3 does not carry {108:%s}:\n%s", ref, msg)
	}
	if !strings.Contains(msg, "{2:I192BANKDEFFXXXXU3003}\n") {
		t.Errorf("block 2 is not {2:I192BANKDEFFXXXXU3003}:\n%s", msg)
	}
	if !strings.Contains(msg, ":20:"+ref+"\n") {
		t.Errorf("missing :20:%s:\n%s", ref, msg)
	}
	if !strings.Contains(msg, ":21:po-mt192-http-1\n") {
		t.Errorf("missing :21:po-mt192-http-1:\n%s", msg)
	}
	// :11S: is "103" plus the explicit original value date.
	if !strings.Contains(msg, ":11S:103261110\n") {
		t.Errorf("missing :11S:103261110:\n%s", msg)
	}
	if !strings.Contains(msg, ":79:Duplicate payment, please cancel\n") {
		t.Errorf("missing :79: reason:\n%s", msg)
	}

	// Repeat export is byte-identical.
	code, msg2 := postText(t, srv.URL+"/exports/mt192",
		`{"payout_id":"po-mt192-http-1","receiver_lt":"BANKDEFFXXXX","original_value_date":"2026-11-10","reason":"Duplicate payment, please cancel"}`)
	if code != http.StatusOK {
		t.Fatalf("repeat export: status=%d, want 200", code)
	}
	if msg2 != msg {
		t.Errorf("repeat export differs:\nfirst:\n%s\nsecond:\n%s", msg, msg2)
	}

	// The metrics counter counts both exports (2).
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	mbody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics: %v", err)
	}
	if !strings.Contains(string(mbody), "ledger_mt192_exports_total 2") {
		t.Errorf("metrics missing ledger_mt192_exports_total 2:\n%s", mbody)
	}
}

// Error mapping for POST /exports/mt192: an unknown payout is a 404,
// a missing or invalid receiver_lt is a 400, a draft payout is a
// 422, an invalid date is a 400, an unknown field is a 400 — and
// rejected requests still count in the metrics.
func TestHTTPMT192ExportErrors(t *testing.T) {
	srv := mt103TestServer(t)
	submittedPayoutHTTP(t, srv.URL, "po-mt192-http-2")
	draftPayoutHTTP(t, srv.URL, "po-mt192-http-draft")

	// Unknown payout: 404.
	code, _ := postText(t, srv.URL+"/exports/mt192",
		`{"payout_id":"no-such-payout","receiver_lt":"BANKDEFFXXXX"}`)
	if code != http.StatusNotFound {
		t.Errorf("unknown payout: status=%d, want 404", code)
	}
	// Missing receiver_lt: 400.
	code, _ = postText(t, srv.URL+"/exports/mt192",
		`{"payout_id":"po-mt192-http-2"}`)
	if code != http.StatusBadRequest {
		t.Errorf("missing receiver_lt: status=%d, want 400", code)
	}
	// Invalid receiver_lt: 400.
	code, _ = postText(t, srv.URL+"/exports/mt192",
		`{"payout_id":"po-mt192-http-2","receiver_lt":"BANK US33"}`)
	if code != http.StatusBadRequest {
		t.Errorf("invalid receiver_lt: status=%d, want 400", code)
	}
	// Draft payout: 422 — nothing was ever sent to cancel.
	code, _ = postText(t, srv.URL+"/exports/mt192",
		`{"payout_id":"po-mt192-http-draft","receiver_lt":"BANKDEFFXXXX"}`)
	if code != http.StatusUnprocessableEntity {
		t.Errorf("draft payout: status=%d, want 422", code)
	}
	// Invalid original_value_date: 400.
	code, _ = postText(t, srv.URL+"/exports/mt192",
		`{"payout_id":"po-mt192-http-2","receiver_lt":"BANKDEFFXXXX","original_value_date":"10/11/2026"}`)
	if code != http.StatusBadRequest {
		t.Errorf("invalid original_value_date: status=%d, want 400", code)
	}
	// Unknown field: 400 (strict decode).
	code, _ = postText(t, srv.URL+"/exports/mt192",
		`{"payout_id":"po-mt192-http-2","receiver_lt":"BANKDEFFXXXX","swift_code":"BANKDEFF"}`)
	if code != http.StatusBadRequest {
		t.Errorf("unknown field: status=%d, want 400", code)
	}
	// Trailing garbage after the JSON value: 400.
	code, _ = postText(t, srv.URL+"/exports/mt192",
		`{"payout_id":"po-mt192-http-2","receiver_lt":"BANKDEFFXXXX"} trailing`)
	if code != http.StatusBadRequest {
		t.Errorf("trailing garbage: status=%d, want 400", code)
	}

	// Rejected requests still count: 7 requests above.
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	mbody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics: %v", err)
	}
	if !strings.Contains(string(mbody), "ledger_mt192_exports_total 7") {
		t.Errorf("metrics missing ledger_mt192_exports_total 7:\n%s", mbody)
	}
}
