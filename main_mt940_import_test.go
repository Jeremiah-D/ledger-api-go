package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// mt940ImportDoc renders a minimal MT940 customer statement for
// the HTTP tests: :20: :25: :28C: :60F: :61: :62F: :64:. The
// statement foots (0 + 150,00 = 150,00) and agrees with the seeded
// ledger balance, unless closing is overridden.
func mt940ImportDoc(ref, acct, vd, mmdd, closing string) string {
	return strings.Join([]string{
		":20:" + ref,
		":25:" + acct,
		":28C:1/1",
		":60F:C" + vd + "USD0,00",
		":61:" + vd + mmdd + "C150,00NTRF//mt940-imp-e1",
		":62F:" + closing,
		":64:" + closing,
	}, "\n")
}

func TestMT940ImportEndpoint_ImportsAndReturnsReport(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()
	acct := "acct1"

	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"`+acct+`","credit_account":"equity","amount_cents":15000,"currency":"USD","memo":"seed","idempotency_key":"mt940-imp-e1"}`)
	if code != http.StatusCreated {
		t.Fatalf("post e1: status=%d", code)
	}

	// The closing day is tomorrow, after the seeded entry, so the
	// ledger balance at the end of that day is the seeded 150,00.
	tomorrow := time.Now().UTC().Add(24 * time.Hour)
	vd := tomorrow.Format("060102")
	mmdd := tomorrow.Format("0102")
	closing := "C" + vd + "USD150,00"
	importBody := func(doc string) string {
		raw, err := json.Marshal(map[string]string{"account": acct, "mt940": doc})
		if err != nil {
			t.Fatalf("marshal import body: %v", err)
		}
		return string(raw)
	}

	// First import: 200, the :61: line matched, no mismatches, not
	// a duplicate.
	code, body := postJSON(t, srv.URL+"/mt940/import", importBody(mt940ImportDoc("HTTP-940-1", acct, vd, mmdd, closing)))
	if code != http.StatusOK {
		t.Fatalf("POST /mt940/import: status=%d body=%v", code, body)
	}
	if dup, _ := body["duplicate"].(bool); dup {
		t.Errorf("first import: duplicate = true, want false")
	}
	if _, ok := body["balance_mismatches"]; ok {
		t.Errorf("balance_mismatches present on an agreeing statement: %v", body["balance_mismatches"])
	}
	if n, _ := body["closing_balance_cents"].(float64); n != 15000 {
		t.Errorf("closing_balance_cents = %v, want 15000", body["closing_balance_cents"])
	}
	if n, _ := body["matched"].(float64); n != 1 {
		t.Errorf("matched = %v, want 1", body["matched"])
	}
	lines, _ := body["lines"].([]any)
	if len(lines) != 1 {
		t.Fatalf("lines = %v, want 1", body["lines"])
	}
	if line, _ := lines[0].(map[string]any); line["status"] != "matched" {
		t.Errorf("line status = %v, want matched", line["status"])
	}

	// Re-importing the same document: 200 with duplicate=true.
	code, body = postJSON(t, srv.URL+"/mt940/import", importBody(mt940ImportDoc("HTTP-940-1", acct, vd, mmdd, closing)))
	if code != http.StatusOK {
		t.Fatalf("POST /mt940/import duplicate: status=%d body=%v", code, body)
	}
	if dup, _ := body["duplicate"].(bool); !dup {
		t.Errorf("second import: duplicate = false, want true")
	}

	// 400s: missing account, empty document, unbalanced document.
	unbalanced := mt940ImportDoc("HTTP-940-2", acct, vd, mmdd, "C"+vd+"USD160,00")
	for name, payload := range map[string]string{
		"missing account": `{"mt940":"` + strings.ReplaceAll(mt940ImportDoc("HTTP-940-1", acct, vd, mmdd, closing), "\n", `\n`) + `"}`,
		"empty document":  `{"account":"` + acct + `","mt940":""}`,
		"unbalanced":      importBody(unbalanced),
	} {
		if code, body := postJSON(t, srv.URL+"/mt940/import", payload); code != http.StatusBadRequest {
			t.Errorf("%s: status=%d body=%v, want 400", name, code, body)
		}
	}

	// 422: a statement for an unknown account.
	ghostDoc := mt940ImportDoc("HTTP-940-3", "ghost-acct", vd, mmdd, closing)
	raw, err := json.Marshal(map[string]string{"account": "ghost-acct", "mt940": ghostDoc})
	if err != nil {
		t.Fatalf("marshal ghost body: %v", err)
	}
	if code, body := postJSON(t, srv.URL+"/mt940/import", string(raw)); code != http.StatusUnprocessableEntity {
		t.Errorf("unknown account: status=%d body=%v, want 422", code, body)
	}

	// Metrics: 2 successful imports + 4 rejected payloads = 6 served.
	code, mbody, _ := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(mbody, "ledger_mt940_imports_total 6") {
		t.Errorf("metrics missing ledger_mt940_imports_total 6")
	}
}
