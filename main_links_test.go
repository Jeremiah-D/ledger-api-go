package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// postLinkEntry seeds one journal entry and returns its server-assigned ID.
func postLinkEntry(t *testing.T, srvURL string) string {
	t.Helper()
	code, body := postJSON(t, srvURL+"/entries", `{"debit_account":"alice","credit_account":"bank","amount_cents":1000}`)
	if code != http.StatusCreated {
		t.Fatalf("seed POST /entries status = %d, want 201", code)
	}
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatalf("seed response missing id: %v", body)
	}
	return id
}

func TestHandleEntryLinks(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	entryID := postLinkEntry(t, srv.URL)

	// The reversal auto-declares a reversal_of link (LG-123).
	code, rev := postJSON(t, srv.URL+"/entries/"+entryID+"/reverse", `{}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /entries/{id}/reverse status = %d, want 201", code)
	}
	revID, _ := rev["id"].(string)

	// Outgoing view from the reversal entry. Reversal IDs contain a
	// slash ("<id>/reversal"), so the ID is URL-escaped for the path.
	code, body := getJSON(t, srv.URL+"/entries/"+url.PathEscape(revID)+"/links")
	if code != http.StatusOK {
		t.Fatalf("GET links (reversal) status = %d, want 200", code)
	}
	if body["entry_id"] != revID {
		t.Errorf("entry_id = %v, want %s", body["entry_id"], revID)
	}
	links, ok := body["links"].([]any)
	if !ok || len(links) != 1 {
		t.Fatalf("links = %v, want 1 link", body["links"])
	}
	row := links[0].(map[string]any)
	if row["from_entry_id"] != revID || row["to_entry_id"] != entryID {
		t.Errorf("link endpoints = %v/%v, want %s -> %s", row["from_entry_id"], row["to_entry_id"], revID, entryID)
	}
	if row["link_type"] != "reversal_of" || row["direction"] != "outgoing" {
		t.Errorf("link = %v, want reversal_of/outgoing", row)
	}
	if _, ok := row["declared_at"].(string); !ok {
		t.Errorf("link missing declared_at: %v", row)
	}

	// Incoming view from the original entry.
	code, body = getJSON(t, srv.URL+"/entries/"+entryID+"/links")
	if code != http.StatusOK {
		t.Fatalf("GET links (original) status = %d, want 200", code)
	}
	links, ok = body["links"].([]any)
	if !ok || len(links) != 1 {
		t.Fatalf("links = %v, want 1 link", body["links"])
	}
	if links[0].(map[string]any)["direction"] != "incoming" {
		t.Errorf("original's link direction = %v, want incoming", links[0].(map[string]any)["direction"])
	}

	// An entry with no declared links returns an empty list, not 404.
	lonely := postLinkEntry(t, srv.URL)
	code, body = getJSON(t, srv.URL+"/entries/"+lonely+"/links")
	if code != http.StatusOK {
		t.Fatalf("GET links (unlinked) status = %d, want 200", code)
	}
	if ls, ok := body["links"].([]any); !ok || len(ls) != 0 {
		t.Errorf("unlinked entry links = %v, want empty list", body["links"])
	}

	// An unknown entry ID returns 404.
	code, _ = getJSON(t, srv.URL+"/entries/no-such-entry/links")
	if code != http.StatusNotFound {
		t.Errorf("GET links (unknown) status = %d, want 404", code)
	}
}

// TestHandleEntryLinksMethodNotAllowed: the links endpoint is read-only —
// the mux rejects a POST with 405 before the handler runs.
func TestHandleEntryLinksMethodNotAllowed(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()
	entryID := postLinkEntry(t, srv.URL)
	resp, err := http.Post(srv.URL+"/entries/"+entryID+"/links", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST links: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST links status = %d, want 405", resp.StatusCode)
	}
}

// TestEntryLinksMetric: declared links surface on GET /metrics as
// ledger_entry_links_total, including the reversal paths' automatic
// declarations.
func TestEntryLinksMetric(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	entryID := postLinkEntry(t, srv.URL)
	if code, _ := postJSON(t, srv.URL+"/entries/"+entryID+"/reverse", `{}`); code != http.StatusCreated {
		t.Fatalf("reverse status = %d, want 201", code)
	}
	code, body, _ := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want 200", code)
	}
	var line string
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, "ledger_entry_links_total ") {
			line = l
		}
	}
	if line != "ledger_entry_links_total 1" {
		t.Errorf("metrics line = %q, want \"ledger_entry_links_total 1\"", line)
	}
}
