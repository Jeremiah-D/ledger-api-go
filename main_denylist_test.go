package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func TestDenylistManagementEndpoints(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	// Add an exact account with operator attribution.
	code, body := postJSONWithActor(t, srv.URL+"/denylist", `{"account":"bad-actor"}`, "ops-chen")
	if code != http.StatusCreated {
		t.Fatalf("POST /denylist: status=%d body=%v, want 201", code, body)
	}
	if body["value"] != "bad-actor" || body["prefix"] != false {
		t.Fatalf("POST /denylist body=%v, want the exact entry", body)
	}
	if body["added_by"] != "ops-chen" {
		t.Fatalf("added_by=%v, want ops-chen", body["added_by"])
	}

	// Add a prefix pattern.
	code, _ = postJSONWithActor(t, srv.URL+"/denylist", `{"prefix":"sdn-"}`, "")
	if code != http.StatusCreated {
		t.Fatalf("POST /denylist prefix: status=%d, want 201", code)
	}

	// Both or neither is 400.
	code, _ = postJSONWithActor(t, srv.URL+"/denylist", `{"account":"x","prefix":"y"}`, "")
	if code != http.StatusBadRequest {
		t.Fatalf("POST /denylist both set: status=%d, want 400", code)
	}
	code, _ = postJSONWithActor(t, srv.URL+"/denylist", `{}`, "")
	if code != http.StatusBadRequest {
		t.Fatalf("POST /denylist empty: status=%d, want 400", code)
	}

	// List shows exact first, then prefix.
	code, body = getJSON(t, srv.URL+"/denylist")
	if code != http.StatusOK {
		t.Fatalf("GET /denylist: status=%d, want 200", code)
	}
	entries := body["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("GET /denylist entries=%v, want 2", body["entries"])
	}

	// Remove the exact entry; second removal is 404.
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/denylist/bad-actor", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE /denylist: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE /denylist/bad-actor: status=%d, want 200", resp.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodDelete, srv.URL+"/denylist/bad-actor", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE /denylist: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("DELETE /denylist/bad-actor again: status=%d, want 404", resp.StatusCode)
	}

	// Remove the prefix with ?prefix=true.
	req, _ = http.NewRequest(http.MethodDelete, srv.URL+"/denylist/sdn-?prefix=true", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE /denylist prefix: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE /denylist/sdn-?prefix=true: status=%d, want 200", resp.StatusCode)
	}
	code, body = getJSON(t, srv.URL+"/denylist")
	if len(body["entries"].([]any)) != 0 {
		t.Fatalf("GET /denylist after removals = %v, want empty", body["entries"])
	}
}

func TestDenylistImportEndpoint(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	code, body := postJSON(t, srv.URL+"/denylist/import",
		`{"entries":[{"account":"a1"},{"prefix":"sdn-"},{"account":"a1"}]}`)
	if code != http.StatusOK {
		t.Fatalf("POST /denylist/import: status=%d body=%v, want 200", code, body)
	}
	if body["added"] != float64(2) || body["skipped"] != float64(1) {
		t.Fatalf("import result=%v, want added=2 skipped=1", body)
	}
	// A malformed row aborts the whole import with 400.
	code, _ = postJSON(t, srv.URL+"/denylist/import", `{"entries":[{"account":"a2"},{}]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("POST /denylist/import bad row: status=%d, want 400", code)
	}
	code, body = getJSON(t, srv.URL+"/denylist")
	if len(body["entries"].([]any)) != 2 {
		t.Fatalf("entries after failed import=%v, want the original 2", body["entries"])
	}
}

func TestDenylistRejectionHTTP(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	if code, _ := postJSON(t, srv.URL+"/denylist", `{"account":"bad"}`); code != http.StatusCreated {
		t.Fatalf("setup denylist: status=%d, want 201", code)
	}

	// POST /entries through the denied account is 422 and bumps the
	// counter.
	code, body := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"bad","credit_account":"equity","amount_cents":100}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("POST /entries denied: status=%d, want 422", code)
	}
	if !strings.Contains(body["error"].(string), "denylist") {
		t.Fatalf("422 body=%v, want a denylist error", body)
	}

	// POST /transfers with a denied payer is 422.
	code, _ = postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"t1","from_account":"bad","to_account":"merchant","amount_cents":100}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("POST /transfers denied: status=%d, want 422", code)
	}

	// A clean transfer still works, so the server is healthy.
	code, _ = postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"t2","from_account":"customer","to_account":"merchant","amount_cents":100}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /transfers clean: status=%d, want 201", code)
	}

	// The counter counts all three write paths.
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	if !strings.Contains(sb.String(), "ledger_denylist_rejections_total 2") {
		t.Fatalf("metrics missing ledger_denylist_rejections_total 2:\n%s", sb.String())
	}
}

func TestDenylistCaptureRejectionHTTP(t *testing.T) {
	l := ledger.New()
	if _, _, err := l.Post(ledger.JournalEntry{ID: "fund-card", DebitAccount: "card", CreditAccount: "equity", AmountCents: 10000}); err != nil {
		t.Fatalf("seed Post: %v", err)
	}
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/holds",
		`{"hold_id":"h1","account":"card","amount_cents":6000,"expires_at":"2099-01-01T00:00:00Z"}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /holds: status=%d, want 201", code)
	}
	if code, _ := postJSON(t, srv.URL+"/denylist", `{"account":"card"}`); code != http.StatusCreated {
		t.Fatalf("setup denylist: status=%d, want 201", code)
	}
	code, body := postJSON(t, srv.URL+"/holds/h1/capture",
		`{"capture_id":"c1","to_account":"merchant","amount_cents":1000}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("POST /holds/h1/capture denied: status=%d body=%v, want 422", code, body)
	}
}
