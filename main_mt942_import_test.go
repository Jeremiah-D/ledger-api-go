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

// mt942ImportDoc renders a minimal MT942 interim transaction
// report for the HTTP tests: :20: :25: :28C: :13D: :61: :90D:
// :90C:. The single :61: line foots the trailers (0 debit,
// 150,00 credit) and matches the seeded entry by its idempotency
// key, unless credit is overridden.
func mt942ImportDoc(ref, acct, dt13, line, credit string) string {
	return strings.Join([]string{
		":20:" + ref,
		":25:" + acct,
		":28C:1/1",
		":13D:" + dt13,
		":61:" + line,
		":90D:00,00",
		":90C:" + credit,
	}, "\n")
}

func TestMT942ImportEndpoint(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()
	acct := "acct1"

	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"`+acct+`","credit_account":"equity","amount_cents":15000,"currency":"USD","memo":"seed","idempotency_key":"mt942-imp-e1"}`)
	if code != http.StatusCreated {
		t.Fatalf("post e1: status=%d", code)
	}

	// The :13D: instant is tomorrow noon, after the seeded entry;
	// the :61: line is dated today, so the report window covers
	// the seed. No :34F: — the document names no currency, so the
	// line matches currency-agnostically and no trailer fold is
	// computed.
	tomorrow := time.Now().UTC().Add(24 * time.Hour)
	today := time.Now().UTC()
	dt13 := tomorrow.Format("0601021504")
	line := today.Format("060102") + today.Format("0102") + "C150,00NTRF//mt942-imp-e1"
	importBody := func(doc string) string {
		raw, err := json.Marshal(map[string]string{"account": acct, "mt942": doc})
		if err != nil {
			t.Fatalf("marshal import body: %v", err)
		}
		return string(raw)
	}

	// First import: 200, one matched line, not a duplicate.
	code, body := postJSON(t, srv.URL+"/mt942/import", importBody(mt942ImportDoc("HTTP-942-1", acct, dt13, line, "1150,00")))
	if code != http.StatusOK {
		t.Fatalf("POST /mt942/import: status=%d body=%v", code, body)
	}
	if dup, _ := body["duplicate"].(bool); dup {
		t.Errorf("first import: duplicate = true, want false")
	}
	if n, _ := body["matched"].(float64); n != 1 {
		t.Errorf("matched = %v, want 1", body["matched"])
	}
	if n, _ := body["credit_total_cents"].(float64); n != 15000 {
		t.Errorf("credit_total_cents = %v, want 15000", body["credit_total_cents"])
	}
	if _, ok := body["summary_mismatches"]; ok {
		t.Errorf("summary_mismatches present on a report with no :34F: currency: %v", body["summary_mismatches"])
	}

	// Re-importing the same document: 200 with duplicate=true.
	code, body = postJSON(t, srv.URL+"/mt942/import", importBody(mt942ImportDoc("HTTP-942-1", acct, dt13, line, "1150,00")))
	if code != http.StatusOK {
		t.Fatalf("POST /mt942/import duplicate: status=%d body=%v", code, body)
	}
	if dup, _ := body["duplicate"].(bool); !dup {
		t.Errorf("second import: duplicate = false, want true")
	}

	// 400s: missing account, empty document, trailer that does not
	// foot the :61: lines.
	badTrailer := mt942ImportDoc("HTTP-942-2", acct, dt13, line, "1151,00")
	for name, payload := range map[string]string{
		"missing account": `{"mt942":"` + strings.ReplaceAll(mt942ImportDoc("HTTP-942-1", acct, dt13, line, "1150,00"), "\n", `\n`) + `"}`,
		"empty document":  `{"account":"` + acct + `","mt942":""}`,
		"bad trailer":     importBody(badTrailer),
	} {
		if code, body := postJSON(t, srv.URL+"/mt942/import", payload); code != http.StatusBadRequest {
			t.Errorf("%s: status=%d body=%v, want 400", name, code, body)
		}
	}

	// 422: a report for an unknown account.
	ghostDoc := mt942ImportDoc("HTTP-942-3", "ghost-acct", dt13, line, "1150,00")
	raw, err := json.Marshal(map[string]string{"account": "ghost-acct", "mt942": ghostDoc})
	if err != nil {
		t.Fatalf("marshal ghost body: %v", err)
	}
	if code, body := postJSON(t, srv.URL+"/mt942/import", string(raw)); code != http.StatusUnprocessableEntity {
		t.Errorf("unknown account: status=%d body=%v, want 422", code, body)
	}

	// Metrics: 2 successful imports + 4 rejected payloads = 6 served.
	code, mbody, _ := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(mbody, "ledger_mt942_imports_total 6") {
		t.Errorf("metrics missing ledger_mt942_imports_total 6")
	}
}
