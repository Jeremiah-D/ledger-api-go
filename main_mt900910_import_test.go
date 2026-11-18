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

// mt900910ImportDoc renders a minimal MT900 confirmation for the
// HTTP tests through the real export: :20: :21: :25: :32A:. The
// :21: cites the original MT103 :20: while the seeded entry
// carries "mt103-"+ref as its idempotency key, so the import hits
// through the prefix strip.
func mt900910ImportDoc(ref, related, acct string, cents int64) string {
	doc, err := ledger.ExportMT900910(ledger.MT900910{
		MessageType: "900", SenderLT: "BANKUS33", ReceiverLT: "BANKDEFF",
		TransactionReference: ref, RelatedReference: related, Account: acct,
		ValueDate: time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC),
		Currency: "USD", AmountCents: cents,
	})
	if err != nil {
		panic(err)
	}
	return doc
}

func TestMT900910ImportEndpoint(t *testing.T) {
	srv := httptest.NewServer(newRouter(ledger.New()))
	defer srv.Close()
	acct := "acct1"

	code, _ := postJSON(t, srv.URL+"/entries",
		`{"debit_account":"equity","credit_account":"`+acct+`","amount_cents":15000,"currency":"USD","memo":"seed","idempotency_key":"mt103-HTTP900"}`)
	if code != http.StatusCreated {
		t.Fatalf("post e1: status=%d", code)
	}

	importBody := func(doc string) string {
		raw, err := json.Marshal(map[string]string{"account": acct, "mt900910": doc})
		if err != nil {
			t.Fatalf("marshal import body: %v", err)
		}
		return string(raw)
	}

	// First import: 200, one matched confirmation, not a duplicate.
	doc := mt900910ImportDoc("HTTP-900-1", "HTTP900", acct, 15000)
	code, body := postJSON(t, srv.URL+"/mt900910/import", importBody(doc))
	if code != http.StatusOK {
		t.Fatalf("POST /mt900910/import: status=%d body=%v", code, body)
	}
	if dup, _ := body["duplicate"].(bool); dup {
		t.Errorf("first import: duplicate = true, want false")
	}
	match, _ := body["match"].(map[string]any)
	if match == nil {
		t.Fatalf("match missing from report: %v", body)
	}
	if s, _ := match["status"].(string); s != "matched" {
		t.Errorf("match status = %q, want matched: %v", s, body)
	}
	if n, _ := body["amount_cents"].(float64); n != 15000 {
		t.Errorf("amount_cents = %v, want 15000", body["amount_cents"])
	}
	if s, _ := body["message_type"].(string); s != "900" {
		t.Errorf("message_type = %q, want 900", s)
	}

	// Re-importing the same document: 200 with duplicate=true.
	code, body = postJSON(t, srv.URL+"/mt900910/import", importBody(doc))
	if code != http.StatusOK {
		t.Fatalf("POST /mt900910/import duplicate: status=%d body=%v", code, body)
	}
	if dup, _ := body["duplicate"].(bool); !dup {
		t.Errorf("second import: duplicate = false, want true")
	}

	// 400s: missing account, empty document.
	for name, payload := range map[string]string{
		"missing account": `{"mt900910":"` + strings.ReplaceAll(doc, "\n", `\n`) + `"}`,
		"empty document":  `{"account":"` + acct + `","mt900910":""}`,
	} {
		if code, body := postJSON(t, srv.URL+"/mt900910/import", payload); code != http.StatusBadRequest {
			t.Errorf("%s: status=%d body=%v, want 400", name, code, body)
		}
	}

	// 400: a confirmation naming acct1 imported into acct2 — the
	// document resolves, but the target differs.
	raw, err := json.Marshal(map[string]string{"account": "acct2", "mt900910": mt900910ImportDoc("HTTP-900-2", "HTTP900", acct, 15000)})
	if err != nil {
		t.Fatalf("marshal wrong-account body: %v", err)
	}
	if code, body := postJSON(t, srv.URL+"/mt900910/import", string(raw)); code != http.StatusBadRequest {
		t.Errorf("wrong account: status=%d body=%v, want 400", code, body)
	}

	// 422: a confirmation for an unknown account.
	raw, err = json.Marshal(map[string]string{"account": "ghost-acct", "mt900910": mt900910ImportDoc("HTTP-900-3", "HTTP900", "ghost-acct", 15000)})
	if err != nil {
		t.Fatalf("marshal ghost body: %v", err)
	}
	if code, body := postJSON(t, srv.URL+"/mt900910/import", string(raw)); code != http.StatusUnprocessableEntity {
		t.Errorf("unknown account: status=%d body=%v, want 422", code, body)
	}

	// Metrics: 2 successful imports + 4 rejected payloads = 6 served.
	code, mbody, _ := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics: status=%d", code)
	}
	if !strings.Contains(mbody, "ledger_mt900910_imports_total 6") {
		t.Errorf("metrics missing ledger_mt900910_imports_total 6")
	}
}
