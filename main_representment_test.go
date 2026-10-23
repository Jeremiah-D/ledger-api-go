package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// seedRepresentmentServer builds a server with one open dispute d1
// (payer -> payee transfer t1, 10000 USD, disputed 6000) for the
// representment endpoints.
func seedRepresentmentServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := seedDisputeServer(t)
	if code, body := postJSON(t, srv.URL+"/disputes",
		`{"dispute_id":"d1","transfer_id":"t1","reason_code":"unauthorized","amount_cents":6000}`); code != http.StatusCreated {
		t.Fatalf("seed raise dispute: status %d (body %v)", code, body)
	}
	return srv
}

func TestHTTPRepresentmentFlow(t *testing.T) {
	srv := seedRepresentmentServer(t)
	defer srv.Close()

	// Submit: 201 with the representment.
	code, body := postJSON(t, srv.URL+"/disputes/d1/representment",
		`{"document_hashes":["sha256:receipt-001"],"statement":"authorized in writing"}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /disputes/d1/representment status = %d, want 201 (body %v)", code, body)
	}
	if body["dispute_id"] != "d1" || body["adjudicated"] != false {
		t.Errorf("submit body = %v, want d1 unadjudicated", body)
	}

	// Idempotent replay of the same evidence: 200 with the original.
	code, dup := postJSON(t, srv.URL+"/disputes/d1/representment",
		`{"document_hashes":["sha256:receipt-001"],"statement":"authorized in writing"}`)
	if code != http.StatusOK {
		t.Errorf("replay status = %d, want 200 (body %v)", code, dup)
	}

	// A second, different package: 409.
	code, _ = postJSON(t, srv.URL+"/disputes/d1/representment",
		`{"document_hashes":["sha256:other"],"statement":"other"}`)
	if code != http.StatusConflict {
		t.Errorf("second package status = %d, want 409", code)
	}

	// Adjudicate uphold: 200, the dispute resolves upheld and the
	// payer is refunded.
	code, resolved := postJSON(t, srv.URL+"/disputes/d1/adjudicate",
		`{"decision":"uphold","adjudicator":"arbiter-7"}`)
	if code != http.StatusOK {
		t.Fatalf("POST /disputes/d1/adjudicate status = %d, want 200 (body %v)", code, resolved)
	}
	if resolved["status"] != "resolved" || resolved["upheld"] != true {
		t.Errorf("adjudicate body = %v, want resolved/upheld", resolved)
	}

	// Adjudicating again: 409 — resolution is terminal.
	code, _ = postJSON(t, srv.URL+"/disputes/d1/adjudicate",
		`{"decision":"reverse","adjudicator":"arbiter-7"}`)
	if code != http.StatusConflict {
		t.Errorf("second adjudication status = %d, want 409", code)
	}
}

func TestHTTPRepresentmentErrors(t *testing.T) {
	srv := seedRepresentmentServer(t)
	defer srv.Close()

	// Unknown dispute: 404.
	if code, _ := postJSON(t, srv.URL+"/disputes/nope/representment",
		`{"document_hashes":["sha256:x"],"statement":"s"}`); code != http.StatusNotFound {
		t.Errorf("unknown dispute submit = %d, want 404", code)
	}
	if code, _ := postJSON(t, srv.URL+"/disputes/nope/adjudicate",
		`{"decision":"uphold","adjudicator":"a"}`); code != http.StatusNotFound {
		t.Errorf("unknown dispute adjudicate = %d, want 404", code)
	}

	// Empty evidence: 400.
	if code, _ := postJSON(t, srv.URL+"/disputes/d1/representment",
		`{"document_hashes":[],"statement":""}`); code != http.StatusBadRequest {
		t.Errorf("empty evidence = %d, want 400", code)
	}

	// Adjudicate with no evidence submitted: 409.
	if code, _ := postJSON(t, srv.URL+"/disputes/d1/adjudicate",
		`{"decision":"uphold","adjudicator":"a"}`); code != http.StatusConflict {
		t.Errorf("adjudicate without evidence = %d, want 409", code)
	}

	// Unknown decision: 400. Empty adjudicator: 400.
	if code, _ := postJSON(t, srv.URL+"/disputes/d1/adjudicate",
		`{"decision":"maybe","adjudicator":"a"}`); code != http.StatusBadRequest {
		t.Errorf("bad decision = %d, want 400", code)
	}
	if code, _ := postJSON(t, srv.URL+"/disputes/d1/adjudicate",
		`{"decision":"uphold","adjudicator":""}`); code != http.StatusBadRequest {
		t.Errorf("empty adjudicator = %d, want 400", code)
	}
}

func TestHTTPAdjudicateReverse(t *testing.T) {
	srv := seedRepresentmentServer(t)
	defer srv.Close()

	if code, _ := postJSON(t, srv.URL+"/disputes/d1/representment",
		`{"document_hashes":["sha256:x"],"statement":"s"}`); code != http.StatusCreated {
		t.Fatalf("submit status = %d, want 201", code)
	}
	// Reverse: the claim is rejected, the payee keeps the funds — the
	// payer's balance stays at 10000 and the payee's available is whole.
	code, resolved := postJSON(t, srv.URL+"/disputes/d1/adjudicate",
		`{"decision":"reverse","adjudicator":"ops-alice"}`)
	if code != http.StatusOK || resolved["upheld"] != false {
		t.Fatalf("adjudicate reverse = status %d body %v, want 200 rejected", code, resolved)
	}
	if code, bal := getJSON(t, srv.URL+"/accounts/payer/balance"); code != http.StatusOK ||
		bal["balance_cents"] != float64(10000) {
		t.Errorf("payer balance = %v, want 10000 (no refund)", bal)
	}
	if code, bal := getJSON(t, srv.URL+"/accounts/payee/balance"); code != http.StatusOK ||
		bal["available_cents"] != float64(10000) {
		t.Errorf("payee available = %v, want 10000 (hold released)", bal)
	}
}
