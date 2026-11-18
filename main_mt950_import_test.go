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

// mt950ImportDoc renders a minimal MT950 interbank statement for
// the HTTP tests: :20: :25: :28C: :60F: :61: :62F: :64: :65:. The
// single :61: line foots the balances (0 opening, 150,00 closing)
// and matches the seeded entry by its idempotency key, unless
// closing is overridden.
func mt950ImportDoc(ref, acct, opening, line, closing string) string {
	return strings.Join([]string{
		":20:" + ref,
		":25:" + acct,
		":28C:1/1",
		":60F:" + opening,
		":61:" + line,
		":62F:" + closing,
		":64:" + closing,
		":65:" + closing,
	}, "\n")
}

func TestMT950ImportEndpoint(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()
	acct := "acct1"

	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"`+acct+`","credit_account":"equity","amount_cents":15000,"currency":"USD","memo":"seed","idempotency_key":"mt950-imp-e1"}`)
	if code != http.StatusCreated {
		t.Fatalf("post e1: status=%d", code)
	}

	// The opening balance is dated today (zero — the seeded entry
	// was posted after midnight); the closing balance is dated
	// tomorrow, so the fold at the end of the closing day covers
	// the seed. The :61: line is dated today.
	tomorrow := time.Now().UTC().Add(24 * time.Hour)
	today := time.Now().UTC()
	opening := "C" + today.Format("060102") + "USD0,00"
	closing := "C" + tomorrow.Format("060102") + "USD150,00"
	line := today.Format("060102") + today.Format("0102") + "C150,00NTRF//mt950-imp-e1"
	importBody := func(doc string) string {
		raw, err := json.Marshal(map[string]string{"account": acct, "mt950": doc})
		if err != nil {
			t.Fatalf("marshal import body: %v", err)
		}
		return string(raw)
	}

	// First import: 200, one matched line, no balance mismatches,
	// not a duplicate.
	code, body := postJSON(t, srv.URL+"/mt950/import", importBody(mt950ImportDoc("HTTP-950-1", acct, opening, line, closing)))
	if code != http.StatusOK {
		t.Fatalf("POST /mt950/import: status=%d body=%v", code, body)
	}
	if dup, _ := body["duplicate"].(bool); dup {
		t.Errorf("first import: duplicate = true, want false")
	}
	if n, _ := body["matched"].(float64); n != 1 {
		t.Errorf("matched = %v, want 1", body["matched"])
	}
	if n, _ := body["closing_balance_cents"].(float64); n != 15000 {
		t.Errorf("closing_balance_cents = %v, want 15000", body["closing_balance_cents"])
	}
	if _, ok := body["balance_mismatches"]; ok {
		t.Errorf("balance_mismatches present on a statement the journal agrees with: %v", body["balance_mismatches"])
	}

	// Re-importing the same document: 200 with duplicate=true.
	code, body = postJSON(t, srv.URL+"/mt950/import", importBody(mt950ImportDoc("HTTP-950-1", acct, opening, line, closing)))
	if code != http.StatusOK {
		t.Fatalf("POST /mt950/import duplicate: status=%d body=%v", code, body)
	}
	if dup, _ := body["duplicate"].(bool); !dup {
		t.Errorf("second import: duplicate = false, want true")
	}

	// 400s: missing account, empty document, closing balance that
	// does not foot the opening balance and the lines.
	badClosing := "C" + tomorrow.Format("060102") + "USD151,00"
	for name, payload := range map[string]string{
		"missing account": `{"mt950":"` + strings.ReplaceAll(mt950ImportDoc("HTTP-950-1", acct, opening, line, closing), "\n", `\n`) + `"}`,
		"empty document":  `{"account":"` + acct + `","mt950":""}`,
		"bad closing":     importBody(mt950ImportDoc("HTTP-950-2", acct, opening, line, badClosing)),
	} {
		if code, body := postJSON(t, srv.URL+"/mt950/import", payload); code != http.StatusBadRequest {
			t.Errorf("%s: status=%d body=%v, want 400", name, code, body)
		}
	}

	// 422: a statement for an unknown account.
	ghostDoc := mt950ImportDoc("HTTP-950-3", "ghost-acct", opening, line, closing)
	raw, err := json.Marshal(map[string]string{"account": "ghost-acct", "mt950": ghostDoc})
	if err != nil {
		t.Fatalf("marshal ghost body: %v", err)
	}
	if code, body := postJSON(t, srv.URL+"/mt950/import", string(raw)); code != http.StatusUnprocessableEntity {
		t.Errorf("unknown account: status=%d body=%v, want 422", code, body)
	}

	// Metrics: 2 successful imports + 4 rejected payloads = 6 served.
	code, mbody, _ := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(mbody, "ledger_mt950_imports_total 6") {
		t.Errorf("metrics missing ledger_mt950_imports_total 6")
	}
}
