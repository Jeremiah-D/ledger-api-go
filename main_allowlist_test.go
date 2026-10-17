package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func TestAllowlistHTTPSSetGetClear(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	// A payer with no list installed: disabled with an empty set.
	code, body := getJSON(t, srv.URL+"/accounts/alice/allowlist")
	if code != http.StatusOK {
		t.Fatalf("GET allowlist: status=%d, want 200", code)
	}
	if body["allowlist_enabled"] != false {
		t.Errorf("allowlist_enabled = %v, want false", body["allowlist_enabled"])
	}
	if beneficiaries, ok := body["beneficiaries"].([]any); !ok || len(beneficiaries) != 0 {
		t.Errorf("beneficiaries = %v, want empty array", body["beneficiaries"])
	}

	// Install a list: the response echoes the sorted set.
	code, body = putJSON(t, srv.URL+"/accounts/alice/allowlist",
		`{"beneficiaries":["mallory","bob","bob"]}`)
	if code != http.StatusOK {
		t.Fatalf("PUT allowlist: status=%d, want 200 (body=%v)", code, body)
	}
	if body["allowlist_enabled"] != true {
		t.Errorf("allowlist_enabled = %v, want true", body["allowlist_enabled"])
	}
	if got := beneficiariesOf(body); len(got) != 2 || got[0] != "bob" || got[1] != "mallory" {
		t.Errorf("beneficiaries = %v, want [bob mallory]", got)
	}

	// GET reads the list back.
	code, body = getJSON(t, srv.URL+"/accounts/alice/allowlist")
	if code != http.StatusOK || body["allowlist_enabled"] != true {
		t.Fatalf("GET allowlist: status=%d enabled=%v, want 200 true", code, body["allowlist_enabled"])
	}
	if got := beneficiariesOf(body); len(got) != 2 || got[0] != "bob" || got[1] != "mallory" {
		t.Errorf("beneficiaries = %v, want [bob mallory]", got)
	}

	// An empty set clears: the gate disables again.
	code, body = putJSON(t, srv.URL+"/accounts/alice/allowlist", `{"beneficiaries":[]}`)
	if code != http.StatusOK {
		t.Fatalf("PUT clear: status=%d, want 200 (body=%v)", code, body)
	}
	if body["allowlist_enabled"] != false {
		t.Errorf("allowlist_enabled after clear = %v, want false", body["allowlist_enabled"])
	}

	// An empty beneficiary entry is 400.
	code, _ = putJSON(t, srv.URL+"/accounts/alice/allowlist", `{"beneficiaries":[""]}`)
	if code != http.StatusBadRequest {
		t.Errorf("PUT empty beneficiary: status=%d, want 400", code)
	}
}

func beneficiariesOf(body map[string]any) []string {
	raw, ok := body["beneficiaries"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, _ := v.(string)
		out = append(out, s)
	}
	return out
}

func TestAllowlistHTTPTransfer403(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"alice","credit_account":"funding","amount_cents":10000}`)
	if code != http.StatusCreated {
		t.Fatalf("funding post: status=%d, want 201", code)
	}

	code, _ = putJSON(t, srv.URL+"/accounts/alice/allowlist", `{"beneficiaries":["bob"]}`)
	if code != http.StatusOK {
		t.Fatalf("PUT allowlist: status=%d, want 200", code)
	}

	// A transfer to an unlisted payee is 403.
	code, body := postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"al-http-1","from_account":"alice","to_account":"mallory","amount_cents":1000}`)
	if code != http.StatusForbidden {
		t.Fatalf("transfer to unlisted payee: status=%d, want 403 (body=%v)", code, body)
	}
	if _, ok := body["error"]; !ok {
		t.Errorf("403 body = %v, want an error field", body)
	}

	// A transfer to the listed payee still books: 201.
	code, _ = postJSON(t, srv.URL+"/transfers",
		`{"transfer_id":"al-http-2","from_account":"alice","to_account":"bob","amount_cents":1000}`)
	if code != http.StatusCreated {
		t.Fatalf("transfer to listed payee: status=%d, want 201 (body=%v)", code, body)
	}

	// The dry-run twin sees the gate too: the same rejection returns 403.
	code, body = postJSON(t, srv.URL+"/transfers/dry-run",
		`{"transfer_id":"al-http-3","from_account":"alice","to_account":"mallory","amount_cents":1000}`)
	if code != http.StatusForbidden {
		t.Fatalf("dry-run to unlisted payee: status=%d, want 403 (body=%v)", code, body)
	}

	// The rejection counter moved.
	code, mbody, _ := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200", code)
	}
	m := parseExposition(t, mbody)
	if m["ledger_allowlist_rejections_total"] != 2 {
		t.Errorf("ledger_allowlist_rejections_total = %d, want 2", m["ledger_allowlist_rejections_total"])
	}
}
