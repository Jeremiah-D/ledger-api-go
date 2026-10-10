package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func TestHTTPMemoEntryAndQuery(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	code, body := postEntry(t, srv.URL+"/entries", `{"debit_account":"a","credit_account":"b","amount_cents":100,"currency":"USD","memo":"order-123"}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /entries status = %d, want 201", code)
	}
	if body["memo"] != "order-123" {
		t.Errorf("POST response memo = %v, want order-123", body["memo"])
	}
	postEntry(t, srv.URL+"/entries", `{"debit_account":"a","credit_account":"b","amount_cents":50,"currency":"USD","memo":"order-456"}`)
	postEntry(t, srv.URL+"/entries", `{"debit_account":"a","credit_account":"b","amount_cents":10,"currency":"USD"}`)

	// ?memo= filters to matching entries.
	resp, err := http.Get(srv.URL + "/entries?memo=order-123")
	if err != nil {
		t.Fatalf("GET /entries?memo=: %v", err)
	}
	defer resp.Body.Close()
	var listing struct {
		Entries []ledger.JournalEntry `json:"entries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listing); err != nil {
		t.Fatalf("decode listing: %v", err)
	}
	if len(listing.Entries) != 1 || listing.Entries[0].Memo != "order-123" {
		t.Fatalf("?memo=order-123 = %d entries, want 1", len(listing.Entries))
	}

	// An overlong memo fails 400 with nothing recorded.
	code, _ = postEntry(t, srv.URL+"/entries", `{"debit_account":"a","credit_account":"b","amount_cents":1,"memo":"`+strings.Repeat("x", 256)+`"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("overlong memo status = %d, want 400", code)
	}
	resp2, _ := http.Get(srv.URL + "/entries?memo=xxxxxxxx")
	defer resp2.Body.Close()
	body2, _ := io.ReadAll(resp2.Body)
	if strings.Contains(string(body2), "xxxxxxxxxxxxxxxx") {
		t.Fatal("overlong memo entry was recorded")
	}
}

func TestHTTPMemoTransferAndDryRun(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	// POST /transfers carries the memo onto the principal entry.
	code, body := postEntry(t, srv.URL+"/transfers", `{"transfer_id":"tr-m","from_account":"payer","to_account":"payee","amount_cents":100,"memo":"invoice-9"}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /transfers status = %d, want 201", code)
	}
	entries, ok := body["entries"].([]any)
	if !ok || len(entries) != 1 {
		t.Fatalf("transfer entries = %v, want 1", body["entries"])
	}
	if entries[0].(map[string]any)["memo"] != "invoice-9" {
		t.Errorf("transfer principal memo = %v, want invoice-9", entries[0].(map[string]any)["memo"])
	}

	// POST /entries/dry-run echoes the memo on its legs.
	code, body = postEntry(t, srv.URL+"/entries/dry-run", `{"debit_account":"a","credit_account":"b","amount_cents":100,"memo":"what-if"}`)
	if code != http.StatusOK {
		t.Fatalf("POST /entries/dry-run status = %d, want 200", code)
	}
	legs, ok := body["legs"].([]any)
	if !ok || len(legs) != 1 {
		t.Fatalf("dry-run legs = %v, want 1", body["legs"])
	}
	if legs[0].(map[string]any)["memo"] != "what-if" {
		t.Errorf("dry-run leg memo = %v, want what-if", legs[0].(map[string]any)["memo"])
	}
}
