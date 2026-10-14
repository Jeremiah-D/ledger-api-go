package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func TestHTTPEntryAttachments(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	// Valid https attachments are journaled and echoed.
	code, body := postEntry(t, srv.URL+"/entries", `{"debit_account":"expenses","credit_account":"cash","amount_cents":2500,"currency":"USD","attachments":["https://files.example.com/receipts/inv-001.pdf","https://files.example.com/receipts/inv-002.pdf"]}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /entries status = %d, want 201", code)
	}
	atts, ok := body["attachments"].([]any)
	if !ok || len(atts) != 2 || atts[0] != "https://files.example.com/receipts/inv-001.pdf" {
		t.Fatalf("POST response attachments = %v", body["attachments"])
	}

	// A non-https URI fails 400 with nothing recorded.
	code, _ = postEntry(t, srv.URL+"/entries", `{"debit_account":"a","credit_account":"b","amount_cents":1,"attachments":["http://files.example.com/r.pdf"]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("http-scheme attachment status = %d, want 400", code)
	}
	// Nine attachments fail 400.
	code, _ = postEntry(t, srv.URL+"/entries", `{"debit_account":"a","credit_account":"b","amount_cents":1,"attachments":["https://x.example/1","https://x.example/2","https://x.example/3","https://x.example/4","https://x.example/5","https://x.example/6","https://x.example/7","https://x.example/8","https://x.example/9"]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("nine attachments status = %d, want 400", code)
	}

	// GET /entries echoes the attachments.
	resp, err := http.Get(srv.URL + "/entries?limit=10")
	if err != nil {
		t.Fatalf("GET /entries: %v", err)
	}
	defer resp.Body.Close()
	var listing struct {
		Entries []ledger.JournalEntry `json:"entries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listing); err != nil {
		t.Fatalf("decode listing: %v", err)
	}
	if len(listing.Entries) != 1 || len(listing.Entries[0].Attachments) != 2 {
		t.Fatalf("GET /entries attachments = %+v", listing.Entries)
	}
}

func TestHTTPTransferAttachments(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	code, body := postEntry(t, srv.URL+"/transfers", `{"transfer_id":"t-http-att","from_account":"payer","to_account":"payee","amount_cents":5000,"currency":"USD","attachments":["https://files.example.com/receipts/pay-009.pdf"]}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /transfers status = %d, want 201", code)
	}
	entries, ok := body["entries"].([]any)
	if !ok || len(entries) != 1 {
		t.Fatalf("transfer entries = %v", body["entries"])
	}
	principal, ok := entries[0].(map[string]any)
	if !ok {
		t.Fatalf("principal entry shape = %v", entries[0])
	}
	atts, ok := principal["attachments"].([]any)
	if !ok || len(atts) != 1 {
		t.Fatalf("principal attachments = %v", principal["attachments"])
	}

	// Invalid attachments fail 400 at the transfer level.
	code, _ = postEntry(t, srv.URL+"/transfers", `{"transfer_id":"t-http-att-bad","from_account":"payer","to_account":"payee","amount_cents":5000,"attachments":["http://x.example/r.pdf"]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("bad transfer attachment status = %d, want 400", code)
	}
}

func TestHTTPCaptureAndReversalAttachments(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	// Fund the card account, then hold.
	if code, _ := postEntry(t, srv.URL+"/entries", `{"debit_account":"card","credit_account":"equity","amount_cents":100000,"currency":"USD"}`); code != http.StatusCreated {
		t.Fatalf("fund card status = %d", code)
	}
	code, holdBody := postEntry(t, srv.URL+"/holds", `{"hold_id":"h-http-att","account":"card","amount_cents":10000,"currency":"USD","expires_at":"2030-01-01T00:00:00Z"}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /holds status = %d, body %v", code, holdBody)
	}
	// Capture with attachments: the settlement entry carries them.
	code, capBody := postEntry(t, srv.URL+"/holds/h-http-att/capture", `{"capture_id":"c-http-att","to_account":"merchant","amount_cents":8000,"attachments":["https://files.example.com/auth/auth-1.pdf"]}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /holds/{id}/capture status = %d, body %v", code, capBody)
	}
	entry, ok := capBody["entry"].(map[string]any)
	if !ok {
		t.Fatalf("capture receipt entry = %v", capBody["entry"])
	}
	atts, ok := entry["attachments"].([]any)
	if !ok || len(atts) != 1 {
		t.Fatalf("capture entry attachments = %v", entry["attachments"])
	}

	// Reverse the funding entry: the reversal inherits its attachments.
	code, fundBody := postEntry(t, srv.URL+"/entries", `{"debit_account":"ops","credit_account":"cash","amount_cents":100,"attachments":["https://files.example.com/receipts/orig.pdf"]}`)
	if code != http.StatusCreated {
		t.Fatalf("fund entry status = %d", code)
	}
	fundID, _ := fundBody["id"].(string)
	code, revBody := postEntry(t, srv.URL+"/entries/"+fundID+"/reverse", `{}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /entries/{id}/reverse status = %d, body %v", code, revBody)
	}
	revAtts, ok := revBody["attachments"].([]any)
	if !ok || len(revAtts) != 1 || revAtts[0] != "https://files.example.com/receipts/orig.pdf" {
		t.Fatalf("reversal attachments = %v", revBody["attachments"])
	}
}

func TestHTTPBatchAttachments(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	code, body := postEntry(t, srv.URL+"/entries/batch", `{"batch_id":"b-http-att","entries":[{"entry_id":"be-http-1","debit_account":"a","credit_account":"b","amount_cents":100,"attachments":["https://x.example/r1.pdf"]}]}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /entries/batch status = %d, body %v", code, body)
	}
	entries, ok := body["entries"].([]any)
	if !ok || len(entries) != 1 {
		t.Fatalf("batch entries = %v", body["entries"])
	}
	e0, _ := entries[0].(map[string]any)
	atts, ok := e0["attachments"].([]any)
	if !ok || len(atts) != 1 {
		t.Fatalf("batch entry attachments = %v", e0["attachments"])
	}
	// An invalid attachment on any leg rejects the whole batch with 400.
	code, _ = postEntry(t, srv.URL+"/entries/batch", `{"batch_id":"b-http-att-bad","entries":[{"entry_id":"be-http-2","debit_account":"a","credit_account":"b","amount_cents":100,"attachments":["ftp://x.example/r.pdf"]}]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("bad batch attachment status = %d, want 400", code)
	}
	// The rejected batch recorded nothing.
	resp, err := http.Get(srv.URL + "/entries?limit=10")
	if err != nil {
		t.Fatalf("GET /entries: %v", err)
	}
	defer resp.Body.Close()
	var listing struct {
		Entries []ledger.JournalEntry `json:"entries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listing); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, e := range listing.Entries {
		if strings.Contains(e.ID, "be-http-2") {
			t.Fatal("rejected batch entry was recorded")
		}
	}
}
