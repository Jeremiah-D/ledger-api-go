package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func seedHTTPServer(t *testing.T) *httptest.Server {
	t.Helper()
	l := ledger.New()
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		e := ledger.JournalEntry{
			ID:             "e-http-" + string(rune('0'+i)),
			DebitAccount:   "cash",
			CreditAccount:  "equity",
			AmountCents:    100,
			IdempotencyKey: "key-http-" + string(rune('0'+i)),
			CreatedAt:      base.Add(time.Duration(i) * 24 * time.Hour),
		}
		if _, dup, err := l.Post(e); err != nil || dup {
			t.Fatalf("seed Post %d = dup=%v err=%v", i, dup, err)
		}
	}
	return httptest.NewServer(newRouter(l))
}

func getJSON(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
	return resp.StatusCode, body
}

func TestHandleSnapshot(t *testing.T) {
	srv := seedHTTPServer(t)
	defer srv.Close()

	code, body := getJSON(t, srv.URL+"/accounts/cash/snapshot")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if body["account"] != "cash" {
		t.Errorf("account = %v, want cash", body["account"])
	}
	if body["balance_cents"] != float64(300) {
		t.Errorf("balance_cents = %v, want 300", body["balance_cents"])
	}
	if body["version"] != float64(3) {
		t.Errorf("version = %v, want 3", body["version"])
	}

	// Unknown account snapshots to zero at the same ledger version.
	_, body = getJSON(t, srv.URL+"/accounts/nobody/snapshot")
	if body["balance_cents"] != float64(0) || body["version"] != float64(3) {
		t.Errorf("unknown account snapshot = %v, want balance 0 version 3", body)
	}
}

func TestHandleListEntries(t *testing.T) {
	srv := seedHTTPServer(t)
	defer srv.Close()

	type listResp struct {
		Entries    []map[string]any `json:"entries"`
		NextCursor string           `json:"next_cursor"`
	}
	fetch := func(t *testing.T, url string) (int, listResp) {
		t.Helper()
		resp, err := http.Get(url)
		if err != nil {
			t.Fatalf("GET %s: %v", url, err)
		}
		defer resp.Body.Close()
		var lr listResp
		if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
			t.Fatalf("decode %s: %v", url, err)
		}
		return resp.StatusCode, lr
	}

	// Full export, no params.
	code, lr := fetch(t, srv.URL+"/entries")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(lr.Entries) != 3 || lr.NextCursor != "" {
		t.Fatalf("full export = %d entries cursor %q, want 3 entries empty cursor", len(lr.Entries), lr.NextCursor)
	}
	if lr.Entries[0]["id"] != "e-http-0" || lr.Entries[2]["id"] != "e-http-2" {
		t.Fatalf("entries not in time order: %v", lr.Entries)
	}

	// Two pages of limit=2 walk every entry exactly once.
	var ids []string
	cursor := ""
	for {
		_, lr = fetch(t, srv.URL+"/entries?limit=2&cursor="+cursor)
		for _, e := range lr.Entries {
			ids = append(ids, e["id"].(string))
		}
		if lr.NextCursor == "" {
			break
		}
		cursor = lr.NextCursor
	}
	if len(ids) != 3 || ids[0] != "e-http-0" || ids[1] != "e-http-1" || ids[2] != "e-http-2" {
		t.Fatalf("paginated walk = %v", ids)
	}

	// Time window filtering.
	_, lr = fetch(t, srv.URL+"/entries?since=2026-10-02T00:00:00Z&until=2026-10-03T00:00:00Z")
	if len(lr.Entries) != 1 || lr.Entries[0]["id"] != "e-http-1" {
		t.Fatalf("windowed export = %v, want [e-http-1]", lr.Entries)
	}

	// Malformed inputs are 400.
	for _, url := range []string{
		srv.URL + "/entries?since=not-a-time",
		srv.URL + "/entries?until=2026-13-99T00:00:00Z",
		srv.URL + "/entries?limit=banana",
		srv.URL + "/entries?limit=0",
		srv.URL + "/entries?cursor=!!!",
	} {
		if code, _ := getJSON(t, url); code != http.StatusBadRequest {
			t.Errorf("GET %s status = %d, want 400", url, code)
		}
	}
}
