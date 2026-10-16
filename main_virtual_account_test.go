package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func TestVirtualAccountEndpoints(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	// Register.
	code, body := postJSON(t, srv.URL+"/virtual-accounts", `{"van":"van-9","parent":"parent-9"}`)
	if code != http.StatusOK {
		t.Fatalf("POST /virtual-accounts: status=%d body=%v", code, body)
	}
	if body["van"] != "van-9" || body["parent"] != "parent-9" {
		t.Fatalf("register response = %v", body)
	}
	if v, ok := body["version"].(float64); !ok || v != 1 {
		t.Fatalf("version = %v, want 1", body["version"])
	}

	// Idempotent re-register: same version.
	code, body = postJSON(t, srv.URL+"/virtual-accounts", `{"van":"van-9","parent":"parent-9"}`)
	if code != http.StatusOK || body["version"].(float64) != 1 {
		t.Fatalf("re-register: status=%d body=%v", code, body)
	}

	// Validation failures are 400.
	for _, payload := range []string{
		`{"van":"","parent":"p"}`,
		`{"van":"v","parent":""}`,
		`{"van":"v","parent":"v"}`,
	} {
		if code, _ := postJSON(t, srv.URL+"/virtual-accounts", payload); code != http.StatusBadRequest {
			t.Errorf("POST %s: status=%d, want 400", payload, code)
		}
	}
	// Strict JSON: unknown fields are 400.
	if code, _ := postJSON(t, srv.URL+"/virtual-accounts", `{"van":"v","parent":"p","bogus":1}`); code != http.StatusBadRequest {
		t.Errorf("unknown field: status=%d, want 400", code)
	}

	// List.
	resp, err := http.Get(srv.URL + "/virtual-accounts")
	if err != nil {
		t.Fatalf("GET /virtual-accounts: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /virtual-accounts: status=%d", resp.StatusCode)
	}

	// A transfer to the VAN attributes to the parent end to end.
	code, _ = postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"hv1","from_account":"payer","to_account":"van-9","amount_cents":4200,"currency":"USD"}`)
	if code != http.StatusCreated {
		t.Fatalf("post transfer to VAN: status=%d", code)
	}
	// Delete: unknown VAN is an idempotent no-op.
	code, body = deleteJSON(t, srv.URL+"/virtual-accounts/ghost")
	if code != http.StatusOK {
		t.Fatalf("DELETE unknown: status=%d", code)
	}
	if body["deleted"] != false {
		t.Fatalf("DELETE unknown: deleted = %v, want false", body["deleted"])
	}
	code, body = deleteJSON(t, srv.URL+"/virtual-accounts/van-9")
	if code != http.StatusOK || body["deleted"] != true {
		t.Fatalf("DELETE van-9: status=%d body=%v", code, body)
	}
	if v, ok := body["version"].(float64); !ok || v != 2 {
		t.Fatalf("version after delete = %v, want 2", body["version"])
	}
}

// A transfer to a VAN books to the parent: check via the balance
// endpoint.
func TestVirtualAccountTransferEndToEnd(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	if code, _ := postJSON(t, srv.URL+"/virtual-accounts", `{"van":"evan","parent":"eparent"}`); code != http.StatusOK {
		t.Fatalf("register: status=%d", code)
	}
	if code, _ := postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"ev1","from_account":"payer","to_account":"evan","amount_cents":777,"currency":"USD"}`); code != http.StatusCreated {
		t.Fatalf("transfer: status=%d", code)
	}
	code, ct, s := getBody(t, srv.URL+"/accounts/eparent/balance")
	if code != http.StatusOK {
		t.Fatalf("GET balance: status=%d", code)
	}
	_ = ct
	if !strings.Contains(s, "777") {
		t.Fatalf("parent balance body missing 777: %s", s)
	}
	code, _, s = getBody(t, srv.URL+"/accounts/evan/balance")
	if code != http.StatusOK {
		t.Fatalf("GET van balance: status=%d", code)
	}
	if !strings.Contains(s, `"balance_cents":0`) {
		t.Fatalf("VAN balance should be 0: %s", s)
	}
	// Paying from the VAN is rejected with 422.
	if code, _ := postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"ev2","from_account":"evan","to_account":"payer","amount_cents":10,"currency":"USD"}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("transfer from VAN: status=%d, want 422", code)
	}
}

func deleteJSON(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		t.Fatalf("DELETE %s: %v", url, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE %s: %v", url, err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
	return resp.StatusCode, body
}
