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

// A :61: line citing an outbound MT202 this ledger rendered
// (LG-487) matches through HTTP: POST /exports/mt202 registers
// the transfer, POST /mt940/import matches the statement line
// against the export record, with export_reference in the report
// and no entry_id — the export posts no journal row. The :62F:
// residual is expected (the balance fold reads the journal only)
// and is not asserted here.
func TestMT940ImportEndpoint_MatchesOutboundMT202Export(t *testing.T) {
	l := ledger.New()
	if err := l.SetExternalMapping("acct1", ledger.ExternalSystemMT940, "DE89370400440532013000", "test"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	exportBody := `{"sender_lt":"BANKUS33","receiver_lt":"BANKDEFFXXXX","transaction_reference":"MT202-REF-1","related_reference":"ORIG-REF-9","value_date":"2026-10-22","currency":"USD","amount_cents":250000,"ordering_institution_bic":"BANKUS33","beneficiary_institution_bic":"BANKDEFF"}`
	code, _, msg := postTextResp(t, srv.URL+"/exports/mt202", exportBody)
	if code != http.StatusOK {
		t.Fatalf("POST /exports/mt202: status=%d body=%s, want 200", code, msg)
	}

	doc := strings.Join([]string{
		":20:HTTP-940-EX",
		":25:DE89370400440532013000",
		":28C:1/1",
		":60F:C261005USD0,00",
		":61:2610051005D2500,00NTRF//MT202-REF-1",
		":62F:D261005USD2500,00",
		":64:D261005USD2500,00",
	}, "\n")
	raw, err := json.Marshal(map[string]string{"account": "acct1", "mt940": doc})
	if err != nil {
		t.Fatalf("marshal import body: %v", err)
	}
	code, body := postJSON(t, srv.URL+"/mt940/import", string(raw))
	if code != http.StatusOK {
		t.Fatalf("POST /mt940/import: status=%d body=%v, want 200", code, body)
	}
	if n, _ := body["matched"].(float64); n != 1 {
		t.Errorf("matched = %v, want 1", body["matched"])
	}
	if n, _ := body["unmatched"].(float64); n != 0 {
		t.Errorf("unmatched = %v, want 0", body["unmatched"])
	}
	lines, _ := body["lines"].([]any)
	if len(lines) != 1 {
		t.Fatalf("lines = %v, want 1", body["lines"])
	}
	line, _ := lines[0].(map[string]any)
	if line["status"] != "matched" {
		t.Errorf("line status = %v, want matched", line["status"])
	}
	if line["export_reference"] != "MT202-REF-1" {
		t.Errorf("export_reference = %v, want MT202-REF-1", line["export_reference"])
	}
	if _, hasEntry := line["entry_id"]; hasEntry {
		t.Errorf("entry_id present = %v, want absent (the export posts no journal row)", line["entry_id"])
	}
}
