package main

import (
	"encoding/json"
	"net/http"
	"testing"
)

func fetchAccountEntries(t *testing.T, url string) (int, []map[string]any, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	var lr struct {
		Entries    []map[string]any `json:"entries"`
		NextCursor string           `json:"next_cursor"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
	return resp.StatusCode, lr.Entries, lr.NextCursor
}

func TestHandleListAccountEntries(t *testing.T) {
	srv := seedHTTPServer(t)
	defer srv.Close()

	// The seed posts 3 entries, all debit cash / credit equity.
	code, entries, cursor := fetchAccountEntries(t, srv.URL+"/accounts/cash/entries")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(entries) != 3 || cursor != "" {
		t.Fatalf("cash entries = %d cursor %q, want 3 entries empty cursor", len(entries), cursor)
	}
	if entries[0]["id"] != "e-http-0" || entries[2]["id"] != "e-http-2" {
		t.Fatalf("entries not in time order: %v", entries)
	}

	// The credit leg is listed too: equity sees the same 3 entries.
	_, equityEntries, _ := fetchAccountEntries(t, srv.URL+"/accounts/equity/entries")
	if len(equityEntries) != 3 {
		t.Fatalf("equity entries = %d, want 3", len(equityEntries))
	}

	// Unknown accounts return an empty JSON array, not 404.
	code, entries, cursor = fetchAccountEntries(t, srv.URL+"/accounts/nobody/entries")
	if code != http.StatusOK {
		t.Fatalf("unknown account status = %d, want 200", code)
	}
	if entries == nil || len(entries) != 0 || cursor != "" {
		t.Fatalf("unknown account = %#v cursor %q, want empty array", entries, cursor)
	}

	// Paginated walk: limit=2 covers all 3 entries exactly once.
	var ids []string
	cursor = ""
	for {
		_, page, next := fetchAccountEntries(t, srv.URL+"/accounts/cash/entries?limit=2&cursor="+cursor)
		for _, e := range page {
			ids = append(ids, e["id"].(string))
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if len(ids) != 3 || ids[0] != "e-http-0" || ids[1] != "e-http-1" || ids[2] != "e-http-2" {
		t.Fatalf("paginated walk = %v", ids)
	}

	// Time window filtering mirrors /entries.
	_, entries, _ = fetchAccountEntries(t, srv.URL+"/accounts/cash/entries?since=2026-10-02T00:00:00Z&until=2026-10-03T00:00:00Z")
	if len(entries) != 1 || entries[0]["id"] != "e-http-1" {
		t.Fatalf("windowed export = %v, want [e-http-1]", entries)
	}

	// Malformed inputs are 400.
	for _, url := range []string{
		srv.URL + "/accounts/cash/entries?since=not-a-time",
		srv.URL + "/accounts/cash/entries?limit=banana",
		srv.URL + "/accounts/cash/entries?limit=0",
		srv.URL + "/accounts/cash/entries?cursor=!!!",
	} {
		if code, _ := getJSON(t, url); code != http.StatusBadRequest {
			t.Errorf("GET %s status = %d, want 400", url, code)
		}
	}
}
