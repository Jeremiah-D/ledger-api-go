package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func TestHTTPExternalRefEntryAndQuery(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	// POST /entries carries external_ref onto the journal row.
	code, body := postEntry(t, srv.URL+"/entries", `{"debit_account":"a","credit_account":"b","amount_cents":100,"currency":"USD","memo":"order","external_ref":"psp-txn-1"}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /entries status = %d, want 201", code)
	}
	if body["external_ref"] != "psp-txn-1" {
		t.Errorf("POST response external_ref = %v, want psp-txn-1", body["external_ref"])
	}
	postEntry(t, srv.URL+"/entries", `{"debit_account":"a","credit_account":"b","amount_cents":50,"currency":"USD","memo":"order","external_ref":"psp-txn-2"}`)
	postEntry(t, srv.URL+"/entries", `{"debit_account":"a","credit_account":"b","amount_cents":10,"currency":"USD","memo":"order"}`)

	getEntries := func(q string) []ledger.JournalEntry {
		t.Helper()
		resp, err := http.Get(srv.URL + "/entries" + q)
		if err != nil {
			t.Fatalf("GET /entries%s: %v", q, err)
		}
		defer resp.Body.Close()
		var listing struct {
			Entries []ledger.JournalEntry `json:"entries"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&listing); err != nil {
			t.Fatalf("decode listing: %v", err)
		}
		return listing.Entries
	}

	// ?ref= is an exact match on the reference.
	entries := getEntries("?ref=psp-txn-1")
	if len(entries) != 1 || entries[0].ExternalRef != "psp-txn-1" {
		t.Fatalf("?ref=psp-txn-1 = %d entries, want 1", len(entries))
	}
	// A substring is not an exact match.
	if entries := getEntries("?ref=psp-txn"); len(entries) != 0 {
		t.Fatalf("?ref=psp-txn = %d entries, want 0 (exact match only)", len(entries))
	}
	// ?ref= composes with ?memo=.
	entries = getEntries("?memo=order&ref=psp-txn-2")
	if len(entries) != 1 || entries[0].ExternalRef != "psp-txn-2" {
		t.Fatalf("?memo=order&ref=psp-txn-2 = %d entries, want 1", len(entries))
	}
	// ?ref= composes with cursor pagination: one per page.
	entries = getEntries("?ref=psp-txn-2&limit=1")
	if len(entries) != 1 {
		t.Fatalf("first ref page = %d entries, want 1", len(entries))
	}
	// An overlong external_ref fails 400 with nothing recorded.
	code, _ = postEntry(t, srv.URL+"/entries", `{"debit_account":"a","credit_account":"b","amount_cents":1,"external_ref":"`+strings.Repeat("x", 65)+`"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("overlong external_ref status = %d, want 400", code)
	}
	// A duplicated external_ref fails 400 with nothing recorded.
	code, _ = postEntry(t, srv.URL+"/entries", `{"debit_account":"a","credit_account":"b","amount_cents":1,"external_ref":"psp-txn-1"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("duplicate external_ref status = %d, want 400", code)
	}
	if entries := getEntries(""); len(entries) != 3 {
		t.Fatalf("journal = %d entries, want 3 (rejections recorded nothing)", len(entries))
	}
}

func TestHTTPExternalRefTransferPassthrough(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	// POST /transfers stamps external_ref on the principal entry.
	code, body := postEntry(t, srv.URL+"/transfers", `{"transfer_id":"tx-1","from_account":"payer","to_account":"payee","amount_cents":100,"external_ref":"order-42"}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /transfers status = %d, want 201", code)
	}
	entries, ok := body["entries"].([]any)
	if !ok || len(entries) != 1 {
		t.Fatalf("transfer entries = %v, want 1", body["entries"])
	}
	if entries[0].(map[string]any)["external_ref"] != "order-42" {
		t.Errorf("transfer principal external_ref = %v, want order-42", entries[0].(map[string]any)["external_ref"])
	}

	// Reusing the reference on another transfer is a 400.
	code, _ = postEntry(t, srv.URL+"/transfers", `{"transfer_id":"tx-2","from_account":"payer","to_account":"payee","amount_cents":100,"external_ref":"order-42"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("duplicate transfer external_ref status = %d, want 400", code)
	}

	// POST /entries/batch entries carry external_ref, and the batch is
	// atomic on duplicates.
	code, body = postEntry(t, srv.URL+"/entries/batch", `{"batch_id":"batch-1","entries":[{"entry_id":"be-1","debit_account":"a","credit_account":"b","amount_cents":10,"external_ref":"batch-ref-1"},{"entry_id":"be-2","debit_account":"a","credit_account":"b","amount_cents":10,"external_ref":"batch-ref-1"}]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("batch-internal duplicate external_ref status = %d, want 400", code)
	}
	code, _ = postEntry(t, srv.URL+"/entries/batch", `{"batch_id":"batch-2","entries":[{"entry_id":"be-3","debit_account":"a","credit_account":"b","amount_cents":10,"external_ref":"batch-ref-2"},{"entry_id":"be-4","debit_account":"a","credit_account":"b","amount_cents":10,"external_ref":"order-42"}]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("batch vs ledger duplicate external_ref status = %d, want 400", code)
	}
}

func TestHTTPExternalRefCapturePassthrough(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	postEntry(t, srv.URL+"/entries", `{"debit_account":"card","credit_account":"bank","amount_cents":10000}`)
	code, _ := postEntry(t, srv.URL+"/holds", `{"hold_id":"h-1","account":"card","amount_cents":5000,"expires_at":"2099-01-01T00:00:00Z"}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /holds status = %d, want 201", code)
	}

	// POST /holds/{id}/capture stamps external_ref on the settlement
	// entry.
	code, body := postEntry(t, srv.URL+"/holds/h-1/capture", `{"capture_id":"c-1","to_account":"merchant","amount_cents":3000,"external_ref":"auth-777"}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /holds/h-1/capture status = %d, want 201", code)
	}
	entry, ok := body["entry"].(map[string]any)
	if !ok {
		t.Fatalf("capture receipt has no entry: %v", body)
	}
	if entry["external_ref"] != "auth-777" {
		t.Errorf("capture entry external_ref = %v, want auth-777", entry["external_ref"])
	}

	// A second capture reusing the reference is a 400.
	postEntry(t, srv.URL+"/holds", `{"hold_id":"h-2","account":"card","amount_cents":2000,"expires_at":"2099-01-01T00:00:00Z"}`)
	code, _ = postEntry(t, srv.URL+"/holds/h-2/capture", `{"capture_id":"c-2","to_account":"merchant","amount_cents":1000,"external_ref":"auth-777"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("duplicate capture external_ref status = %d, want 400", code)
	}
}

func TestHTTPExternalRefReversalInheritance(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	code, body := postEntry(t, srv.URL+"/entries", `{"debit_account":"a","credit_account":"b","amount_cents":100,"external_ref":"psp-txn-9"}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /entries status = %d, want 201", code)
	}
	entryID, _ := body["id"].(string)
	if entryID == "" {
		t.Fatalf("response has no id: %v", body)
	}

	// POST /entries/{id}/reverse inherits "reversal_of:<ref>".
	code, body = postEntry(t, srv.URL+"/entries/"+entryID+"/reverse", `{}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /entries/{id}/reverse status = %d, want 201", code)
	}
	if body["external_ref"] != "reversal_of:psp-txn-9" {
		t.Errorf("reversal external_ref = %v, want reversal_of:psp-txn-9", body["external_ref"])
	}
}
