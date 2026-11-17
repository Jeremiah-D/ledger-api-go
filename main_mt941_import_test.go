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

// mt941ImportDoc renders a minimal MT941 balance report for the
// HTTP tests: :20: :25: :28C: :13D: :90D: :90C: :62F: :64: :65:.
// The report foots (0 + 150,00 − 0 = 150,00) and agrees with the
// seeded ledger balance, unless closing is overridden.
func mt941ImportDoc(ref, acct, dt13, closing string) string {
	return strings.Join([]string{
		":20:" + ref,
		":25:" + acct,
		":28C:1/1",
		":13D:" + dt13,
		":90D:00,00",
		":90C:1150,00",
		":62F:" + closing,
		":64:" + closing,
		":65:" + closing,
	}, "\n")
}

func TestMT941ImportEndpoint(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()
	acct := "acct1"

	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"`+acct+`","credit_account":"equity","amount_cents":15000,"currency":"USD","memo":"seed","idempotency_key":"mt941-imp-e1"}`)
	if code != http.StatusCreated {
		t.Fatalf("post e1: status=%d", code)
	}

	// The :13D: instant is tomorrow noon, after the seeded entry,
	// so the ledger balance at that instant is the seeded 150,00.
	tomorrow := time.Now().UTC().Add(24 * time.Hour)
	dt13 := tomorrow.Format("0601021504")
	vd := tomorrow.Format("060102")
	closing := "C" + vd + "USD150,00"
	importBody := func(doc string) string {
		raw, err := json.Marshal(map[string]string{"account": acct, "mt941": doc})
		if err != nil {
			t.Fatalf("marshal import body: %v", err)
		}
		return string(raw)
	}

	// First import: 200, no mismatches, not a duplicate.
	code, body := postJSON(t, srv.URL+"/mt941/import", importBody(mt941ImportDoc("HTTP-941-1", acct, dt13, closing)))
	if code != http.StatusOK {
		t.Fatalf("POST /mt941/import: status=%d body=%v", code, body)
	}
	if dup, _ := body["duplicate"].(bool); dup {
		t.Errorf("first import: duplicate = true, want false")
	}
	if _, ok := body["balance_mismatches"]; ok {
		t.Errorf("balance_mismatches present on an agreeing report: %v", body["balance_mismatches"])
	}
	if n, _ := body["closing_balance_cents"].(float64); n != 15000 {
		t.Errorf("closing_balance_cents = %v, want 15000", body["closing_balance_cents"])
	}
	if n, _ := body["credit_total_cents"].(float64); n != 15000 {
		t.Errorf("credit_total_cents = %v, want 15000", body["credit_total_cents"])
	}

	// Re-importing the same document: 200 with duplicate=true.
	code, body = postJSON(t, srv.URL+"/mt941/import", importBody(mt941ImportDoc("HTTP-941-1", acct, dt13, closing)))
	if code != http.StatusOK {
		t.Fatalf("POST /mt941/import duplicate: status=%d body=%v", code, body)
	}
	if dup, _ := body["duplicate"].(bool); !dup {
		t.Errorf("second import: duplicate = false, want true")
	}

	// 400s: missing account, empty document, unbalanced document.
	unbalanced := mt941ImportDoc("HTTP-941-2", acct, dt13, "C"+vd+"USD160,00")
	for name, payload := range map[string]string{
		"missing account": `{"mt941":"` + strings.ReplaceAll(mt941ImportDoc("HTTP-941-1", acct, dt13, closing), "\n", `\n`) + `"}`,
		"empty document":  `{"account":"` + acct + `","mt941":""}`,
		"unbalanced":      importBody(unbalanced),
	} {
		if code, body := postJSON(t, srv.URL+"/mt941/import", payload); code != http.StatusBadRequest {
			t.Errorf("%s: status=%d body=%v, want 400", name, code, body)
		}
	}

	// 422: a report for an unknown account.
	ghostDoc := mt941ImportDoc("HTTP-941-3", "ghost-acct", dt13, closing)
	raw, err := json.Marshal(map[string]string{"account": "ghost-acct", "mt941": ghostDoc})
	if err != nil {
		t.Fatalf("marshal ghost body: %v", err)
	}
	if code, body := postJSON(t, srv.URL+"/mt941/import", string(raw)); code != http.StatusUnprocessableEntity {
		t.Errorf("unknown account: status=%d body=%v, want 422", code, body)
	}

	// Metrics: 2 successful imports + 4 rejected payloads = 6 served.
	code, mbody, _ := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(mbody, "ledger_mt941_imports_total 6") {
		t.Errorf("metrics missing ledger_mt941_imports_total 6")
	}
}
