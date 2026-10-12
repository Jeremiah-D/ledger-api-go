package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func newTenantTestServer() *httptest.Server {
	return httptest.NewServer(newRouter(ledger.New()))
}

func postEntryTenant(t *testing.T, srv *httptest.Server, path string, body map[string]any, headers map[string]string) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", srv.URL+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode POST %s: %v", path, err)
	}
	return resp.StatusCode, out
}

func getBalanceTenant(t *testing.T, srv *httptest.Server, path string, headers map[string]string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest("GET", srv.URL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode GET %s: %v", path, err)
	}
	return resp.StatusCode, out
}

func TestTenantHeaderRouting(t *testing.T) {
	srv := newTenantTestServer()
	defer srv.Close()
	hdr := map[string]string{"X-Tenant": "acme"}

	body := map[string]any{"debit_account": "cash", "credit_account": "rev", "amount_cents": 1000, "idempotency_key": "order-1"}
	if code, _ := postEntryTenant(t, srv, "/entries", body, hdr); code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("POST /entries (acme) = %d, want 2xx", code)
	}
	// Same idempotency key in another tenant books a second entry: the
	// namespaces are isolated.
	body2 := map[string]any{"debit_account": "cash", "credit_account": "rev", "amount_cents": 2000, "idempotency_key": "order-1"}
	if code, _ := postEntryTenant(t, srv, "/entries", body2, map[string]string{"X-Tenant": "globex"}); code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("POST /entries (globex) = %d, want 2xx", code)
	}
	// Same key replayed in acme is a duplicate (no new booking).
	if code, out := postEntryTenant(t, srv, "/entries", body, hdr); code != http.StatusOK {
		t.Fatalf("replay POST /entries (acme) = %d (%v), want 200 duplicate", code, out)
	}

	if code, out := getBalanceTenant(t, srv, "/accounts/cash/balance", hdr); code != http.StatusOK || out["balance_cents"].(float64) != 1000 {
		t.Fatalf("acme cash balance = %d (%v), want 200/1000", code, out)
	}
	if code, out := getBalanceTenant(t, srv, "/accounts/cash/balance", map[string]string{"X-Tenant": "globex"}); code != http.StatusOK || out["balance_cents"].(float64) != 2000 {
		t.Fatalf("globex cash balance = %d (%v), want 200/2000", code, out)
	}
	// The default tenant (no header) never saw these postings:
	// backward compatible.
	if code, out := getBalanceTenant(t, srv, "/accounts/cash/balance", nil); code != http.StatusOK || out["balance_cents"].(float64) != 0 {
		t.Fatalf("default cash balance = %d (%v), want 200/0", code, out)
	}
}

func TestTenantPathRouting(t *testing.T) {
	srv := newTenantTestServer()
	defer srv.Close()

	body := map[string]any{"debit_account": "cash", "credit_account": "rev", "amount_cents": 500}
	if code, _ := postEntryTenant(t, srv, "/tenants/acme/entries", body, nil); code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("POST /tenants/acme/entries = %d, want 2xx", code)
	}
	if code, out := getBalanceTenant(t, srv, "/tenants/acme/accounts/cash/balance", nil); code != http.StatusOK || out["balance_cents"].(float64) != 500 {
		t.Fatalf("acme cash = %d (%v), want 200/500", code, out)
	}
	// Header and path agree on the same tenant.
	if code, out := getBalanceTenant(t, srv, "/accounts/cash/balance", map[string]string{"X-Tenant": "acme"}); code != http.StatusOK || out["balance_cents"].(float64) != 500 {
		t.Fatalf("acme cash via header = %d (%v), want 200/500", code, out)
	}
	// An unknown tenant is a fresh, empty ledger — not an error.
	if code, out := getBalanceTenant(t, srv, "/tenants/brand-new/accounts/cash/balance", nil); code != http.StatusOK || out["balance_cents"].(float64) != 0 {
		t.Fatalf("brand-new tenant cash = %d (%v), want 200/0", code, out)
	}
}

func TestTenantInvalidID(t *testing.T) {
	srv := newTenantTestServer()
	defer srv.Close()
	// A space in a tenant ID is rejected with 400 before any ledger is
	// touched. The raw ID is smuggled past the client URL parser via
	// req.URL.Path; the transport escapes it on the wire and the server
	// decodes it back before the middleware validates.
	req, _ := http.NewRequest("POST", srv.URL+"/entries", strings.NewReader("{}"))
	req.URL.Path = "/tenants/bad id/entries"
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("raw request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST /tenants/<bad>/entries = %d, want 400", resp.StatusCode)
	}
	// Invalid x-tenant header is also 400.
	req2, _ := http.NewRequest("GET", srv.URL+"/accounts/cash/balance", nil)
	req2.Header.Set("X-Tenant", "no/slash")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("raw request: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("GET with bad x-tenant = %d, want 400", resp2.StatusCode)
	}
}

func TestTenantListTenants(t *testing.T) {
	srv := newTenantTestServer()
	defer srv.Close()
	body := map[string]any{"debit_account": "cash", "credit_account": "rev", "amount_cents": 100}
	postEntryTenant(t, srv, "/entries", body, map[string]string{"X-Tenant": "acme"})
	postEntryTenant(t, srv, "/entries", body, map[string]string{"X-Tenant": "globex"})
	postEntryTenant(t, srv, "/entries", body, nil) // default tenant

	code, out := getJSON(t, srv.URL+"/tenants")
	if code != http.StatusOK {
		t.Fatalf("GET /tenants = %d, want 200", code)
	}
	raw, _ := json.Marshal(out["tenants"])
	var tenants []string
	if err := json.Unmarshal(raw, &tenants); err != nil {
		t.Fatalf("decode tenants: %v", err)
	}
	want := []string{"", "acme", "globex"}
	if len(tenants) != len(want) {
		t.Fatalf("tenants = %v, want %v", tenants, want)
	}
	for i := range want {
		if tenants[i] != want[i] {
			t.Fatalf("tenants = %v, want %v", tenants, want)
		}
	}
}

func TestTenantReconcilePerTenant(t *testing.T) {
	srv := newTenantTestServer()
	defer srv.Close()
	body := map[string]any{"debit_account": "cash", "credit_account": "rev", "amount_cents": 100}
	postEntryTenant(t, srv, "/entries", body, map[string]string{"X-Tenant": "acme"})

	code, out := postEntryTenant(t, srv, "/tenants/acme/reconcile", map[string]any{}, nil)
	if code != http.StatusOK {
		t.Fatalf("POST /tenants/acme/reconcile = %d (%v), want 200", code, out)
	}
	code, out = postEntryTenant(t, srv, "/tenants/globex/reconcile", map[string]any{}, nil)
	if code != http.StatusOK {
		t.Fatalf("POST /tenants/globex/reconcile = %d (%v), want 200", code, out)
	}
	// globex is empty: its report must not contain acme's journal.
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), `"e1"`) {
		t.Errorf("globex reconcile report leaks acme's entry: %s", raw)
	}
}

func TestTenantMetricsTenantsTotal(t *testing.T) {
	srv := newTenantTestServer()
	defer srv.Close()
	body := map[string]any{"debit_account": "cash", "credit_account": "rev", "amount_cents": 100}
	postEntryTenant(t, srv, "/entries", body, map[string]string{"X-Tenant": "acme"})
	postEntryTenant(t, srv, "/entries", body, map[string]string{"X-Tenant": "globex"})

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	buf.ReadFrom(resp.Body)
	text := buf.String()
	// default + acme + globex = 3 provisioned tenants.
	if !strings.Contains(text, "ledger_tenants_total 3\n") {
		t.Errorf("metrics missing ledger_tenants_total 3:\n%s", text)
	}
}
