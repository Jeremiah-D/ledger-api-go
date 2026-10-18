package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func TestHandleReverseEntryLifecycle(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	// Seed a posting; the server assigns the entry ID.
	code, seed := postJSON(t, srv.URL+"/entries", `{"debit_account":"alice","credit_account":"bank","amount_cents":1000}`)
	if code != http.StatusCreated {
		t.Fatalf("seed POST /entries status = %d, want 201", code)
	}
	entryID, _ := seed["id"].(string)
	if entryID == "" {
		t.Fatalf("seed response missing id: %v", seed)
	}
	revPath := srv.URL + "/entries/" + entryID + "/reverse"

	// First reversal: 201 with the swapped-legs entry.
	code, body := postJSON(t, revPath, `{}`)
	if code != http.StatusCreated {
		t.Fatalf("reverse status = %d, want 201 (body=%v)", code, body)
	}
	if body["id"] != entryID+"/reversal" {
		t.Fatalf("id = %v, want %s/reversal", body["id"], entryID)
	}
	if body["debit_account"] != "bank" || body["credit_account"] != "alice" {
		t.Fatalf("legs = %v/%v, want bank/alice", body["debit_account"], body["credit_account"])
	}
	if body["memo"] != "reversal_of:"+entryID {
		t.Fatalf("memo = %v, want reversal_of:%s", body["memo"], entryID)
	}

	// Balances net to zero.
	code, body = getJSON(t, srv.URL+"/accounts/alice/balance")
	if code != http.StatusOK || body["balance_cents"] != float64(0) {
		t.Fatalf("alice balance = %v (status %d), want 0", body["balance_cents"], code)
	}

	// Keyless repeat: 200 with the same entry (deterministic idempotency).
	code, body2 := postJSON(t, revPath, `{}`)
	if code != http.StatusOK {
		t.Fatalf("repeat reverse status = %d, want 200", code)
	}
	if body2["id"] != entryID+"/reversal" {
		t.Fatalf("repeat id = %v, want %s/reversal", body2["id"], entryID)
	}

	// New idempotency key on an already-reversed entry: 409.
	code, _ = postJSON(t, revPath, `{"idempotency_key":"another-try"}`)
	if code != http.StatusConflict {
		t.Fatalf("re-reverse with new key status = %d, want 409", code)
	}
}

func TestHandleReverseEntryNotFound(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/entries/ghost/reverse", `{}`)
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
}

func TestHandleReverseEntryFrozen(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	code, seed := postJSON(t, srv.URL+"/entries", `{"debit_account":"a","credit_account":"b","amount_cents":100}`)
	if code != http.StatusCreated {
		t.Fatalf("seed status = %d", code)
	}
	entryID := seed["id"].(string)
	code, _ = postJSON(t, srv.URL+"/accounts/a/freeze", ``)
	if code != http.StatusOK {
		t.Fatalf("freeze status = %d", code)
	}
	code, _ = postJSON(t, srv.URL+"/entries/"+entryID+"/reverse", `{}`)
	if code != http.StatusForbidden {
		t.Fatalf("reverse frozen status = %d, want 403", code)
	}
}

func TestHandleReverseEntryIdempotencyKey(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	code, seed := postJSON(t, srv.URL+"/entries", `{"debit_account":"a","credit_account":"b","amount_cents":100}`)
	if code != http.StatusCreated {
		t.Fatalf("seed status = %d", code)
	}
	entryID := seed["id"].(string)
	revPath := srv.URL + "/entries/" + entryID + "/reverse"
	code, _ = postJSON(t, revPath, `{"idempotency_key":"rev-k1"}`)
	if code != http.StatusCreated {
		t.Fatalf("first reverse status = %d, want 201", code)
	}
	// Same key replays the reversal.
	code, body := postJSON(t, revPath, `{"idempotency_key":"rev-k1"}`)
	if code != http.StatusOK {
		t.Fatalf("replay status = %d, want 200", code)
	}
	if body["id"] != entryID+"/reversal" {
		t.Fatalf("replay id = %v", body["id"])
	}
}

func TestHandleReverseEntryMetrics(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	code, seed := postJSON(t, srv.URL+"/entries", `{"debit_account":"a","credit_account":"b","amount_cents":100}`)
	if code != http.StatusCreated {
		t.Fatalf("seed status = %d", code)
	}
	entryID := seed["id"].(string)
	revPath := srv.URL + "/entries/" + entryID + "/reverse"
	// One committed reversal, one idempotent replay: the counter moves once.
	if code, _ := postJSON(t, revPath, `{}`); code != http.StatusCreated {
		t.Fatalf("reverse status = %d", code)
	}
	if code, _ := postJSON(t, revPath, `{}`); code != http.StatusOK {
		t.Fatalf("replay status = %d", code)
	}
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics: %v", err)
	}
	if !strings.Contains(string(raw), "ledger_reversals_total{kind=\"full\"} 1\n") {
		t.Fatalf("/metrics missing ledger_reversals_total 1:\n%s", raw)
	}
	if !strings.Contains(string(raw), "ledger_reversals_total{kind=\"full\"} 1\n") {
		t.Fatalf("/metrics missing ledger_reversals_total 1:\n%s", raw)
	}
}
