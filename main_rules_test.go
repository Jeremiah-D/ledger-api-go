package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func putRule(t *testing.T, srvURL, id, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, srvURL+"/rules/"+id, strings.NewReader(body))
	if err != nil {
		t.Fatalf("PUT request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT /rules/%s: %v", id, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read PUT /rules/%s: %v", id, err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode PUT /rules/%s: %v (body %q)", id, err, b)
	}
	return resp.StatusCode, out
}

func deleteRule(t *testing.T, srvURL, id string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, srvURL+"/rules/"+id, nil)
	if err != nil {
		t.Fatalf("DELETE request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE /rules/%s: %v", id, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode DELETE /rules/%s: %v (body %q)", id, err, b)
	}
	return resp.StatusCode, out
}

func TestRulesHTTPWorkflow(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	ruleBody := `{"enabled":true,"description":"8% withholding","match_credit_account":"revenue",` +
		`"match_currency":"USD","legs":[{"debit_account":"revenue","credit_account":"tax-payable",` +
		`"rate_bps":800,"label":"withholding"}]}`

	// Create the rule.
	code, out := putRule(t, srv.URL, "tax-withholding", ruleBody)
	if code != http.StatusOK {
		t.Fatalf("PUT /rules = %d, want 200 (%v)", code, out)
	}
	if out["rule_version"] != float64(1) {
		t.Fatalf("rule_version = %v, want 1", out["rule_version"])
	}

	// List shows it.
	code, body, _ := getText(t, srv.URL+"/rules")
	if code != http.StatusOK {
		t.Fatalf("GET /rules = %d, want 200", code)
	}
	var listed struct {
		Rules       []ledger.JournalRule `json:"rules"`
		RuleVersion uint64               `json:"rule_version"`
	}
	if err := json.Unmarshal([]byte(body), &listed); err != nil {
		t.Fatalf("decode GET /rules: %v", err)
	}
	if len(listed.Rules) != 1 || listed.Rules[0].ID != "tax-withholding" || listed.RuleVersion != 1 {
		t.Fatalf("GET /rules = %+v", listed)
	}

	// Post a matching entry: the rule fires server-side.
	postBody := `{"debit_account":"cash","credit_account":"revenue","amount_cents":10000,"currency":"USD"}`
	resp, err := http.Post(srv.URL+"/entries", "application/json", strings.NewReader(postBody))
	if err != nil {
		t.Fatalf("POST /entries: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /entries = %d, want 201", resp.StatusCode)
	}

	// The derived leg is visible on the tax-payable account.
	code, body, _ = getText(t, srv.URL+"/accounts/tax-payable/entries")
	if code != http.StatusOK {
		t.Fatalf("GET /accounts/tax-payable/entries = %d, want 200", code)
	}
	if !strings.Contains(body, "rule:tax-withholding:withholding") {
		t.Fatalf("derived leg memo missing from account entries: %s", body)
	}

	// Metrics: one rule-set request, one committed rule leg.
	_, metricsBody, _ := getText(t, srv.URL+"/metrics")
	m := parseExposition(t, metricsBody)
	if m["ledger_journal_rule_sets_total"] != 1 {
		t.Errorf("ledger_journal_rule_sets_total = %d, want 1", m["ledger_journal_rule_sets_total"])
	}
	if m["ledger_journal_rule_legs_total"] != 1 {
		t.Errorf("ledger_journal_rule_legs_total = %d, want 1", m["ledger_journal_rule_legs_total"])
	}

	// Delete the rule; a second delete is a no-op.
	code, out = deleteRule(t, srv.URL, "tax-withholding")
	if code != http.StatusOK || out["deleted"] != true || out["rule_version"] != float64(2) {
		t.Fatalf("DELETE /rules = %d %v, want 200 deleted=true rule_version=2", code, out)
	}
	code, out = deleteRule(t, srv.URL, "tax-withholding")
	if code != http.StatusOK || out["deleted"] != false {
		t.Fatalf("DELETE /rules again = %d %v, want 200 deleted=false", code, out)
	}

	// After deletion, postings no longer fire the rule.
	resp, err = http.Post(srv.URL+"/entries", "application/json", strings.NewReader(postBody))
	if err != nil {
		t.Fatalf("POST /entries: %v", err)
	}
	resp.Body.Close()
	_, metricsBody, _ = getText(t, srv.URL+"/metrics")
	m = parseExposition(t, metricsBody)
	if m["ledger_journal_rule_legs_total"] != 1 {
		t.Errorf("ledger_journal_rule_legs_total after delete = %d, want 1", m["ledger_journal_rule_legs_total"])
	}
	if m["ledger_journal_rule_deletes_total"] != 2 {
		t.Errorf("ledger_journal_rule_deletes_total = %d, want 2", m["ledger_journal_rule_deletes_total"])
	}
}

func TestRulesHTTPRejectsIllegal(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()

	// A rule with no legs is illegal: 400, nothing stored.
	code, out := putRule(t, srv.URL, "empty", `{"enabled":true}`)
	if code != http.StatusBadRequest {
		t.Fatalf("PUT illegal rule = %d, want 400", code)
	}
	if !strings.Contains(out["error"].(string), "at least one derived leg") {
		t.Fatalf("error = %v, want leg complaint", out["error"])
	}

	// Unknown fields fail fast like the other POST endpoints.
	code, _ = putRule(t, srv.URL, "typo", `{"enabled":true,"legz":[]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("PUT unknown field = %d, want 400", code)
	}

	// Trailing garbage is rejected.
	code, _ = putRule(t, srv.URL, "garbage", `{"enabled":true} x`)
	if code != http.StatusBadRequest {
		t.Fatalf("PUT trailing garbage = %d, want 400", code)
	}

	// A rate above 100% of the principal is rejected, not clamped.
	bad := `{"enabled":true,"legs":[{"debit_account":"a","credit_account":"b","rate_bps":10001}]}`
	code, _ = putRule(t, srv.URL, "over", bad)
	if code != http.StatusBadRequest {
		t.Fatalf("PUT over-100%% rate = %d, want 400", code)
	}

	// Nothing was stored.
	_, body, _ := getText(t, srv.URL+"/rules")
	var listed struct {
		Rules []ledger.JournalRule `json:"rules"`
	}
	if err := json.Unmarshal([]byte(body), &listed); err != nil {
		t.Fatalf("decode GET /rules: %v", err)
	}
	if len(listed.Rules) != 0 {
		t.Fatalf("GET /rules = %d rules, want 0", len(listed.Rules))
	}
}
