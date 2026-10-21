package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// LG-155: API version negotiation end-to-end.

func getRaw(t *testing.T, url, accept string) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if rerr != nil {
			break
		}
	}
	return resp.StatusCode, resp.Header, []byte(sb.String())
}

func TestVersionedPathPrefixEnvelopesJSON(t *testing.T) {
	srv := seedHTTPServer(t)
	defer srv.Close()

	code, hdr, body := getRaw(t, srv.URL+"/v1/entries?limit=2", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", code, body)
	}
	if hdr.Get("X-API-Version") != "v1" {
		t.Fatalf("X-API-Version = %q, want v1", hdr.Get("X-API-Version"))
	}
	var env struct {
		APIVersion string `json:"api_version"`
		Data       struct {
			Entries []any `json:"entries"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode envelope: %v (body: %s)", err, body)
	}
	if env.APIVersion != "v1" {
		t.Fatalf("envelope api_version = %q, want v1", env.APIVersion)
	}
	if len(env.Data.Entries) != 2 {
		t.Fatalf("data.entries = %d entries, want 2", len(env.Data.Entries))
	}
}

func TestUnversionedKeepsLegacyBareShape(t *testing.T) {
	srv := seedHTTPServer(t)
	defer srv.Close()

	code, hdr, body := getRaw(t, srv.URL+"/entries?limit=2", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if hdr.Get("X-API-Version") != "v1" {
		t.Fatalf("X-API-Version = %q, want v1", hdr.Get("X-API-Version"))
	}
	var legacy struct {
		Entries []any `json:"entries"`
	}
	if err := json.Unmarshal(body, &legacy); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(legacy.Entries) != 2 {
		t.Fatalf("legacy entries = %d, want 2", len(legacy.Entries))
	}
	// No envelope on the legacy shape: api_version must be absent.
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := raw["api_version"]; ok {
		t.Fatal("unversioned request must keep the bare legacy shape, no api_version key")
	}
}

func TestVersionedAcceptHeaderEnvelopesJSON(t *testing.T) {
	srv := seedHTTPServer(t)
	defer srv.Close()

	code, _, body := getRaw(t, srv.URL+"/entries?limit=1", "application/vnd.ledger.v1+json")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	var env struct {
		APIVersion string         `json:"api_version"`
		Data       map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode envelope: %v (body: %s)", err, body)
	}
	if env.APIVersion != "v1" || env.Data["entries"] == nil {
		t.Fatalf("envelope = %+v, want api_version=v1 with entries", env)
	}
}

func TestUnknownVersionIs406(t *testing.T) {
	srv := seedHTTPServer(t)
	defer srv.Close()

	code, _, body := getRaw(t, srv.URL+"/v2/entries", "")
	if code != http.StatusNotAcceptable {
		t.Fatalf("path /v2: status = %d, want 406 (body: %s)", code, body)
	}
	var errBody map[string]any
	if err := json.Unmarshal(body, &errBody); err != nil {
		t.Fatalf("406 body must be JSON: %v", err)
	}
	if errBody["supported_versions"] == nil || errBody["latest"] != "v1" {
		t.Fatalf("406 body must list supported versions: %s", body)
	}

	code, _, _ = getRaw(t, srv.URL+"/entries", "application/vnd.ledger.v9+json")
	if code != http.StatusNotAcceptable {
		t.Fatalf("Accept v9: status = %d, want 406", code)
	}
}

func TestConflictingVersionSignalsAre406(t *testing.T) {
	srv := seedHTTPServer(t)
	defer srv.Close()

	code, _, _ := getRaw(t, srv.URL+"/v1/entries", "application/vnd.ledger.v9+json")
	if code != http.StatusNotAcceptable {
		t.Fatalf("path v1 + Accept v9: status = %d, want 406", code)
	}
}

func postEmpty(t *testing.T, url string) (int, http.Header) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, resp.Header
}

func TestDeprecatedEndpointHeaders(t *testing.T) {
	srv := seedHTTPServer(t)
	defer srv.Close()

	// POST /entries is deprecated in favor of POST /entries/batch.
	// The endpoint still works (201 on a valid post); the headers are
	// the announcement.
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/entries",
		strings.NewReader(`{"debit_account":"cash","credit_account":"equity","amount_cents":10,"idempotency_key":"lg155-dep-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("deprecated endpoint must stay functional: status = %d", resp.StatusCode)
	}
	if resp.Header.Get("Deprecation") != "true" {
		t.Fatal("POST /entries must carry Deprecation: true")
	}
	if resp.Header.Get("Sunset") == "" {
		t.Fatal("POST /entries must carry a Sunset date")
	}
	link := resp.Header.Get("Link")
	if !strings.Contains(link, "successor-version") || !strings.Contains(link, "/entries/batch") {
		t.Fatalf("Link header must point at the successor: %q", link)
	}
	if resp.Header.Get("X-API-Version") != "v1" {
		t.Fatalf("X-API-Version = %q, want v1", resp.Header.Get("X-API-Version"))
	}

	// The successor itself is not deprecated.
	code, hdr := postEmpty(t, srv.URL+"/entries/batch")
	if code != http.StatusCreated && code != http.StatusBadRequest {
		t.Fatalf("batch post status = %d", code)
	}
	if hdr.Get("Deprecation") != "" {
		t.Fatal("POST /entries/batch must not be deprecated")
	}

	// Unversioned deprecated endpoint also carries the headers.
	_, hdr = postEmpty(t, srv.URL+"/entries")
	if hdr.Get("Deprecation") != "true" || hdr.Get("Sunset") == "" {
		t.Fatal("unversioned POST /entries must also carry deprecation headers")
	}
}

func TestMetricsPassthroughUnwrapped(t *testing.T) {
	srv := seedHTTPServer(t)
	defer srv.Close()

	// Non-JSON surfaces keep their content type and are never
	// enveloped; the version rides in X-API-Version only.
	code, hdr, body := getRaw(t, srv.URL+"/v1/metrics", "application/vnd.ledger.v1+json")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if !strings.HasPrefix(hdr.Get("Content-Type"), "text/plain") {
		t.Fatalf("Content-Type = %q, want text/plain", hdr.Get("Content-Type"))
	}
	if hdr.Get("X-API-Version") != "v1" {
		t.Fatalf("X-API-Version = %q, want v1", hdr.Get("X-API-Version"))
	}
	if strings.Contains(string(body), `"api_version"`) {
		t.Fatal("/metrics must not be wrapped in the JSON envelope")
	}
}

func TestVersionedErrorResponsesEnveloped(t *testing.T) {
	srv := seedHTTPServer(t)
	defer srv.Close()

	// A JSON error from a registered handler gets the envelope on a
	// versioned request (unknown entry ID -> 404 JSON).
	code, hdr, body := getRaw(t, srv.URL+"/v1/entries/no-such-id/links", "application/vnd.ledger.v1+json")
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
	var env struct {
		APIVersion string         `json:"api_version"`
		Data       map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("versioned 404 must be enveloped JSON: %v (body: %s)", err, body)
	}
	if env.APIVersion != "v1" {
		t.Fatalf("envelope api_version = %q", env.APIVersion)
	}
	if hdr.Get("X-API-Version") != "v1" {
		t.Fatalf("X-API-Version = %q", hdr.Get("X-API-Version"))
	}
}
