package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// newAPIKeyTestServer builds a server with API key enforcement on.
func newAPIKeyTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	l := ledger.New(ledger.WithAPIKeyAuth())
	srv := httptest.NewServer(newServer(l).handler())
	t.Cleanup(srv.Close)
	return srv
}

func doKeyed(t *testing.T, method, url, key, payload string) (int, map[string]any) {
	t.Helper()
	var rdr *strings.Reader
	if payload == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(payload)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode %s %s: %v", method, url, err)
	}
	return resp.StatusCode, body
}

// bootstrapKey mints the first key through the open bootstrap endpoint.
func bootstrapKey(t *testing.T, srv *httptest.Server, scopes ...string) string {
	t.Helper()
	payload := `{"name":"op","scopes":` + `["` + strings.Join(scopes, `","`) + `"]}`
	code, body := doKeyed(t, http.MethodPost, srv.URL+"/api-keys", "", payload)
	if code != http.StatusCreated {
		t.Fatalf("bootstrap POST /api-keys = %d: %v", code, body)
	}
	secret, ok := body["secret"].(string)
	if !ok || secret == "" {
		t.Fatalf("bootstrap did not return a secret: %v", body)
	}
	return secret
}

func TestAPIKeyBootstrapAndEnforcement(t *testing.T) {
	srv := newAPIKeyTestServer(t)

	// No key yet: reads are 401, bootstrap create is open.
	if code, _ := doKeyed(t, http.MethodGet, srv.URL+"/accounts/cash/balance", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("GET without key = %d, want 401", code)
	}
	secret := bootstrapKey(t, srv, "admin:keys")

	// After bootstrap the create endpoint is locked down too.
	if code, _ := doKeyed(t, http.MethodPost, srv.URL+"/api-keys", "", `{"name":"x","scopes":["read:ledger"]}`); code != http.StatusUnauthorized {
		t.Fatalf("second unauthenticated create = %d, want 401", code)
	}
	// Bogus key is 401.
	if code, _ := doKeyed(t, http.MethodGet, srv.URL+"/accounts/cash/balance", "lak_bogus", ""); code != http.StatusUnauthorized {
		t.Fatalf("GET with bogus key = %d, want 401", code)
	}
	// Real key works.
	if code, _ := doKeyed(t, http.MethodGet, srv.URL+"/accounts/cash/balance", secret, ""); code != http.StatusOK {
		t.Fatalf("GET with valid key = %d, want 200", code)
	}
}

func TestAPIKeyScopeEnforcement(t *testing.T) {
	srv := newAPIKeyTestServer(t)
	root := bootstrapKey(t, srv, "admin:keys")

	// Mint a read-only key via the root key.
	code, body := doKeyed(t, http.MethodPost, srv.URL+"/api-keys", root, `{"name":"reader","scopes":["read:ledger"]}`)
	if code != http.StatusCreated {
		t.Fatalf("create reader key = %d: %v", code, body)
	}
	reader := body["secret"].(string)

	if code, _ := doKeyed(t, http.MethodGet, srv.URL+"/accounts/cash/balance", reader, ""); code != http.StatusOK {
		t.Fatalf("reader GET = %d, want 200", code)
	}
	entry := `{"debit_account":"cash","credit_account":"rev","amount_cents":100,"idempotency_key":"k1"}`
	if code, _ := doKeyed(t, http.MethodPost, srv.URL+"/entries", reader, entry); code != http.StatusForbidden {
		t.Fatalf("reader POST /entries = %d, want 403", code)
	}
	// Root (admin:keys implies everything) can book.
	if code, _ := doKeyed(t, http.MethodPost, srv.URL+"/entries", root, entry); code != http.StatusCreated {
		t.Fatalf("root POST /entries = %d, want 201", code)
	}
}

func TestAPIKeyRotateRevokeHTTP(t *testing.T) {
	srv := newAPIKeyTestServer(t)
	root := bootstrapKey(t, srv, "admin:keys")

	// Mint a writer key.
	code, body := doKeyed(t, http.MethodPost, srv.URL+"/api-keys", root, `{"name":"writer","scopes":["write:entries"]}`)
	if code != http.StatusCreated {
		t.Fatalf("create = %d: %v", code, body)
	}
	writer := body["secret"].(string)
	keyID := body["key"].(map[string]any)["id"].(string)

	// Rotate with zero grace: old secret dies at once.
	code, body = doKeyed(t, http.MethodPost, srv.URL+"/api-keys/"+keyID+"/rotate", root, `{"grace_seconds":0}`)
	if code != http.StatusOK {
		t.Fatalf("rotate = %d: %v", code, body)
	}
	fresh := body["secret"].(string)
	if code, _ := doKeyed(t, http.MethodGet, srv.URL+"/accounts/cash/balance", writer, ""); code != http.StatusUnauthorized {
		t.Fatalf("old secret after zero-grace rotate = %d, want 401", code)
	}
	// The rotated key keeps its scopes: still no read scope.
	if code, _ := doKeyed(t, http.MethodGet, srv.URL+"/accounts/cash/balance", fresh, ""); code != http.StatusForbidden {
		t.Fatalf("rotated writer GET = %d, want 403", code)
	}

	// Revoke: immediate and idempotent.
	newID := body["key"].(map[string]any)["id"].(string)
	if code, _ := doKeyed(t, http.MethodPost, srv.URL+"/api-keys/"+newID+"/revoke", root, ""); code != http.StatusOK {
		t.Fatalf("revoke = %d, want 200", code)
	}
	if code, _ := doKeyed(t, http.MethodPost, srv.URL+"/api-keys/"+newID+"/revoke", root, ""); code != http.StatusOK {
		t.Fatalf("re-revoke = %d, want 200 (idempotent)", code)
	}
	if code, _ := doKeyed(t, http.MethodGet, srv.URL+"/accounts/cash/balance", fresh, ""); code != http.StatusUnauthorized {
		t.Fatalf("revoked key = %d, want 401", code)
	}
	// Unknown key is 404.
	if code, _ := doKeyed(t, http.MethodGet, srv.URL+"/api-keys/key_nope", root, ""); code != http.StatusNotFound {
		t.Fatalf("GET unknown key = %d, want 404", code)
	}
}

func TestAPIKeyRateQuotaHTTP(t *testing.T) {
	srv := newAPIKeyTestServer(t)
	root := bootstrapKey(t, srv, "admin:keys")

	code, body := doKeyed(t, http.MethodPost, srv.URL+"/api-keys", root, `{"name":"limited","scopes":["read:ledger"],"rate_limit_per_min":2}`)
	if code != http.StatusCreated {
		t.Fatalf("create = %d: %v", code, body)
	}
	limited := body["secret"].(string)
	for i := 0; i < 2; i++ {
		if code, _ := doKeyed(t, http.MethodGet, srv.URL+"/accounts/cash/balance", limited, ""); code != http.StatusOK {
			t.Fatalf("request %d = %d, want 200", i, code)
		}
	}
	if code, body := doKeyed(t, http.MethodGet, srv.URL+"/accounts/cash/balance", limited, ""); code != http.StatusTooManyRequests {
		t.Fatalf("over-quota request = %d (%v), want 429", code, body)
	}
}

func TestAPIKeyDisabledByDefault(t *testing.T) {
	// Without WithAPIKeyAuth the middleware is a pass-through: the
	// anonymous-operator flow is unchanged.
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()
	if code, _ := doKeyed(t, http.MethodGet, srv.URL+"/accounts/cash/balance", "", ""); code != http.StatusOK {
		t.Fatalf("unauthenticated GET with auth disabled = %d, want 200", code)
	}
}

func TestAPIKeyListHidesSecrets(t *testing.T) {
	srv := newAPIKeyTestServer(t)
	root := bootstrapKey(t, srv, "admin:keys")
	code, body := doKeyed(t, http.MethodGet, srv.URL+"/api-keys", root, "")
	if code != http.StatusOK {
		t.Fatalf("list = %d: %v", code, body)
	}
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), "lak_") {
		t.Fatal("key listing leaks secret material")
	}
}

func TestAPIKeyValidationHTTP(t *testing.T) {
	srv := newAPIKeyTestServer(t)
	root := bootstrapKey(t, srv, "admin:keys")
	// Bad scope -> 400.
	if code, _ := doKeyed(t, http.MethodPost, srv.URL+"/api-keys", root, `{"name":"x","scopes":["root:all"]}`); code != http.StatusBadRequest {
		t.Fatalf("bad scope = %d, want 400", code)
	}
	// Past expiry -> 400.
	past := time.Now().Add(-time.Hour).Format(time.RFC3339)
	if code, _ := doKeyed(t, http.MethodPost, srv.URL+"/api-keys", root, `{"name":"x","scopes":["read:ledger"],"expires_at":`+strconv.Quote(past)+`}`); code != http.StatusBadRequest {
		t.Fatalf("past expiry = %d, want 400", code)
	}
}
