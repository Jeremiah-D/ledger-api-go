package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// UBO endpoints: declare, read, resolve, and set the threshold.
func TestUboEndpoints(t *testing.T) {
	l := ledger.New()
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	body := `{"owners":[{"name":"Alice","kind":"person","share_bp":6000},{"name":"HoldCo","kind":"entity","share_bp":4000}]}`
	code, resp := postJSON(t, srv.URL+"/accounts/corp-http/ubo", body)
	if code != http.StatusOK {
		t.Fatalf("POST /accounts/{id}/ubo: status=%d body=%v", code, resp)
	}
	if _, err := json.Marshal(resp); err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// GET the declaration back.
	r2, err := http.Get(srv.URL + "/accounts/corp-http/ubo")
	if err != nil {
		t.Fatalf("GET ubo: %v", err)
	}
	defer r2.Body.Close()
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("GET /accounts/{id}/ubo: status=%d", r2.StatusCode)
	}
	var decl map[string]any
	if err := json.NewDecoder(r2.Body).Decode(&decl); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decl["account"] != "corp-http" {
		t.Fatalf("account = %v, want corp-http", decl["account"])
	}

	// Resolve: HoldCo has no declaration, so it reports unresolved.
	r3, err := http.Get(srv.URL + "/accounts/corp-http/ubo/resolved")
	if err != nil {
		t.Fatalf("GET resolved: %v", err)
	}
	defer r3.Body.Close()
	var res map[string]any
	if err := json.NewDecoder(r3.Body).Decode(&res); err != nil {
		t.Fatalf("decode resolved: %v", err)
	}
	owners, _ := res["owners"].([]any)
	if len(owners) != 2 {
		t.Fatalf("owners = %v, want 2", owners)
	}

	// A bad declaration is a 400.
	code, _ = postJSON(t, srv.URL+"/accounts/corp-http/ubo", `{"owners":[{"name":"A","kind":"person","share_bp":99999}]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("bad declaration: status=%d, want 400", code)
	}

	// Threshold round-trip.
	code, resp = postJSON(t, srv.URL+"/ubo/threshold", `{"threshold_bp":5000}`)
	if code != http.StatusOK {
		t.Fatalf("POST /ubo/threshold: status=%d body=%v", code, resp)
	}
	if n, _ := resp["threshold_bp"].(float64); n != 5000 {
		t.Fatalf("threshold_bp = %v, want 5000", resp["threshold_bp"])
	}
	if got := l.UboControlThreshold(); got != 5000 {
		t.Fatalf("ledger threshold = %d, want 5000", got)
	}

	// Unknown account: 404 on both reads.
	r4, err := http.Get(srv.URL + "/accounts/nobody/ubo")
	if err != nil {
		t.Fatalf("GET ubo: %v", err)
	}
	defer r4.Body.Close()
	if r4.StatusCode != http.StatusNotFound {
		t.Fatalf("GET unknown ubo: status=%d, want 404", r4.StatusCode)
	}
	r5, err := http.Get(srv.URL + "/accounts/nobody/ubo/resolved")
	if err != nil {
		t.Fatalf("GET resolved: %v", err)
	}
	defer r5.Body.Close()
	if r5.StatusCode != http.StatusNotFound {
		t.Fatalf("GET unknown resolved: status=%d, want 404", r5.StatusCode)
	}
}

// A cyclic ownership chain resolves to 422, not 500.
func TestUboResolveCycleEndpoint(t *testing.T) {
	l := ledger.New()
	if err := l.SetUbo("ca", []ledger.UboOwner{{Name: "cb", Kind: ledger.UboOwnerEntity, ShareBP: 10000}}); err != nil {
		t.Fatalf("SetUbo ca: %v", err)
	}
	if err := l.SetUbo("cb", []ledger.UboOwner{{Name: "ca", Kind: ledger.UboOwnerEntity, ShareBP: 10000}}); err != nil {
		t.Fatalf("SetUbo cb: %v", err)
	}
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()
	r, err := http.Get(srv.URL + "/accounts/ca/ubo/resolved")
	if err != nil {
		t.Fatalf("GET resolved: %v", err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("cyclic resolve: status=%d, want 422", r.StatusCode)
	}
}
