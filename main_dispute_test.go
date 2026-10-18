package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// seedDisputeServer builds a server with a funded payer and one posted
// transfer t1 (payer -> payee, 10000 USD) for the dispute endpoints.
func seedDisputeServer(t *testing.T) *httptest.Server {
	t.Helper()
	l := ledger.New()
	if _, _, err := l.Post(ledger.JournalEntry{
		ID:            "fund-payer",
		DebitAccount:  "payer",
		CreditAccount: "equity",
		AmountCents:   20000,
		Currency:      "USD",
		CreatedAt:     time.Now(),
	}); err != nil {
		t.Fatalf("seed Post: %v", err)
	}
	if _, err := l.PostTransfer(ledger.Transfer{
		ID:          "t1",
		From:        "payer",
		To:          "payee",
		AmountCents: 10000,
		Currency:    "USD",
	}); err != nil {
		t.Fatalf("seed PostTransfer: %v", err)
	}
	return httptest.NewServer(newRouter(l))
}

func TestHTTPDisputeFlow(t *testing.T) {
	srv := seedDisputeServer(t)
	defer srv.Close()

	// Raise: 201 with the open dispute.
	code, body := postJSON(t, srv.URL+"/disputes",
		`{"dispute_id":"d1","transfer_id":"t1","reason_code":"unauthorized","amount_cents":6000}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /disputes status = %d, want 201 (body %v)", code, body)
	}
	if body["id"] != "d1" || body["status"] != "open" || body["hold_id"] != "dispute/d1" {
		t.Errorf("raise body = %v, want open dispute d1 with hold dispute/d1", body)
	}

	// Idempotent replay of the dispute ID: 200 with the original.
	code, dup := postJSON(t, srv.URL+"/disputes",
		`{"dispute_id":"d1","transfer_id":"t1","reason_code":"fraud","amount_cents":999}`)
	if code != http.StatusOK || dup["reason_code"] != "unauthorized" {
		t.Errorf("replay = status %d body %v, want 200 with the original", code, dup)
	}

	// Read: 200 with the open dispute.
	code, got := getJSON(t, srv.URL+"/disputes/d1")
	if code != http.StatusOK || got["status"] != "open" {
		t.Errorf("GET /disputes/d1 = status %d body %v, want 200 open", code, got)
	}

	// The payee's available balance is frozen by the disputed amount.
	code, bal := getJSON(t, srv.URL+"/accounts/payee/balance")
	if code != http.StatusOK {
		t.Fatalf("GET /accounts/payee/balance status = %d", code)
	}
	if bal["balance_cents"] != float64(10000) || bal["available_cents"] != float64(4000) {
		t.Errorf("payee balance = %v, want balance 10000 / available 4000", bal)
	}

	// Resolve upheld: 200, the payer is refunded.
	code, resolved := postJSON(t, srv.URL+"/disputes/d1/resolve", `{"uphold":true}`)
	if code != http.StatusOK {
		t.Fatalf("POST /disputes/d1/resolve status = %d, want 200 (body %v)", code, resolved)
	}
	if resolved["status"] != "resolved" || resolved["upheld"] != true {
		t.Errorf("resolve body = %v, want resolved + upheld", resolved)
	}

	// Resolution is terminal: a second resolve is 409.
	code, _ = postJSON(t, srv.URL+"/disputes/d1/resolve", `{"uphold":false}`)
	if code != http.StatusConflict {
		t.Errorf("second resolve status = %d, want 409", code)
	}

	// Balances reflect the refund: payer 16000, payee 4000.
	code, bal = getJSON(t, srv.URL+"/accounts/payer/balance")
	if bal["balance_cents"] != float64(16000) {
		t.Errorf("payer balance = %v, want 16000", bal)
	}
	code, bal = getJSON(t, srv.URL+"/accounts/payee/balance")
	if bal["balance_cents"] != float64(4000) || bal["available_cents"] != float64(4000) {
		t.Errorf("payee balance = %v, want 4000 / 4000 after resolution", bal)
	}

	// Metrics recorded the raise and the resolution.
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics: %v", err)
	}
	if !strings.Contains(string(raw), "ledger_disputes_raised_total 1\n") {
		t.Errorf("/metrics missing ledger_disputes_raised_total 1:\n%s", raw)
	}
	if !strings.Contains(string(raw), "ledger_disputes_resolved_total 1\n") {
		t.Errorf("/metrics missing ledger_disputes_resolved_total 1:\n%s", raw)
	}
}

func TestHTTPDisputeErrors(t *testing.T) {
	srv := seedDisputeServer(t)
	defer srv.Close()

	cases := []struct {
		name   string
		method string
		path   string
		body   string
		want   int
	}{
		{"unknown dispute", "GET", "/disputes/nope", "", http.StatusNotFound},
		{"unknown transfer", "POST", "/disputes", `{"dispute_id":"dx","transfer_id":"nope","reason_code":"fraud","amount_cents":100}`, http.StatusNotFound},
		{"bad reason", "POST", "/disputes", `{"dispute_id":"dx","transfer_id":"t1","reason_code":"typo","amount_cents":100}`, http.StatusBadRequest},
		{"zero amount", "POST", "/disputes", `{"dispute_id":"dx","transfer_id":"t1","reason_code":"fraud","amount_cents":0}`, http.StatusBadRequest},
		{"amount exceeds transfer", "POST", "/disputes", `{"dispute_id":"dx","transfer_id":"t1","reason_code":"fraud","amount_cents":10001}`, http.StatusBadRequest},
		{"currency mismatch", "POST", "/disputes", `{"dispute_id":"dx","transfer_id":"t1","reason_code":"fraud","amount_cents":100,"currency":"EUR"}`, http.StatusBadRequest},
		{"resolve unknown", "POST", "/disputes/nope/resolve", `{"uphold":true}`, http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var code int
			var body map[string]any
			switch c.method {
			case "GET":
				code, body = getJSON(t, srv.URL+c.path)
			default:
				code, body = postJSON(t, srv.URL+c.path, c.body)
			}
			if code != c.want {
				t.Errorf("%s %s status = %d, want %d (body %v)", c.method, c.path, code, c.want, body)
			}
		})
	}
}

func TestHTTPDisputeRejectFlow(t *testing.T) {
	srv := seedDisputeServer(t)
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/disputes",
		`{"dispute_id":"d2","transfer_id":"t1","reason_code":"duplicate","amount_cents":6000}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /disputes status = %d, want 201", code)
	}
	// Reject: 200, the payee keeps the funds, the freeze lifts.
	code, resolved := postJSON(t, srv.URL+"/disputes/d2/resolve", `{"uphold":false}`)
	if code != http.StatusOK {
		t.Fatalf("POST /disputes/d2/resolve status = %d, want 200 (body %v)", code, resolved)
	}
	if resolved["status"] != "resolved" || resolved["upheld"] != false {
		t.Errorf("resolve body = %v, want resolved + rejected", resolved)
	}
	_, bal := getJSON(t, srv.URL+"/accounts/payee/balance")
	if bal["balance_cents"] != float64(10000) || bal["available_cents"] != float64(10000) {
		t.Errorf("payee balance = %v, want 10000 / 10000 after rejection", bal)
	}
}
