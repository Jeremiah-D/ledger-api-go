package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// mt103TestServer builds an HTTP test server whose ledger has a funded
// payee account, the payout clearing account, and the sender bank's
// BIC registered on the payee account.
func mt103TestServer(t *testing.T) *httptest.Server {
	t.Helper()
	l := ledger.New(ledger.WithPayoutClearingAccount("payout-clearing"))
	if _, _, err := l.Post(ledger.JournalEntry{
		ID: "fund-saver", DebitAccount: "saver", CreditAccount: "equity",
		AmountCents: 1_000_000, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed Post: %v", err)
	}
	if err := l.SetAccountIBAN("saver", "DE89370400440532013000", "BANKUS33"); err != nil {
		t.Fatalf("SetAccountIBAN: %v", err)
	}
	srv := httptest.NewServer(newRouter(l))
	t.Cleanup(srv.Close)
	return srv
}

func postText(t *testing.T, url, payload string) (int, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return resp.StatusCode, string(body)
}

// Full MT103 flow through HTTP: draft, approve, submit, then export
// the SWIFT FIN instruction, exercising the error mapping.
func TestHTTPMT103Export(t *testing.T) {
	srv := mt103TestServer(t)
	submittedPayoutHTTP(t, srv.URL, "po-mt103-http-1")

	// Unknown payout: 404.
	code, _ := postText(t, srv.URL+"/exports/mt103",
		`{"payout_id":"no-such-payout","receiver_lt":"BANKDEFFXXXX"}`)
	if code != http.StatusNotFound {
		t.Errorf("unknown payout: status=%d, want 404", code)
	}
	// Bad option vocabulary: 400.
	code, _ = postText(t, srv.URL+"/exports/mt103",
		`{"payout_id":"po-mt103-http-1","receiver_lt":"BANKDEFFXXXX","details_of_charges":"SPLIT"}`)
	if code != http.StatusBadRequest {
		t.Errorf("bad charges: status=%d, want 400", code)
	}
	// Trailing garbage after the JSON value: 400.
	code, _ = postText(t, srv.URL+"/exports/mt103",
		`{"payout_id":"po-mt103-http-1","receiver_lt":"BANKDEFFXXXX"} trailing`)
	if code != http.StatusBadRequest {
		t.Errorf("trailing garbage: status=%d, want 400", code)
	}
	// Unknown field: 400 (strict decode).
	code, _ = postText(t, srv.URL+"/exports/mt103",
		`{"payout_id":"po-mt103-http-1","receiver_lt":"BANKDEFFXXXX","swift_code":"BANKDEFF"}`)
	if code != http.StatusBadRequest {
		t.Errorf("unknown field: status=%d, want 400", code)
	}

	// Happy path with full options.
	code, msg := postText(t, srv.URL+"/exports/mt103", `{
		"payout_id":"po-mt103-http-1",
		"receiver_lt":"BANKDEFF",
		"bank_operation_code":"SPAY",
		"beneficiary_institution_bic":"CREDAGRA",
		"beneficiary_account":"DE75512108001245126199",
		"beneficiary_name":["ACME Corp"],
		"remittance_info":"invoice 42",
		"details_of_charges":"OUR",
		"value_date":"2026-10-22"
	}`)
	if code != http.StatusOK {
		t.Fatalf("export: status=%d body=%s, want 200", code, msg)
	}
	for _, want := range []string{
		"{1:F01BANKUS33XXX0000000000}",
		"{2:I103BANKDEFFXXXU3003}",
		"{3:{108:po-mt103-http-1}}",
		":20:po-mt103-http-1",
		":23B:SPAY",
		":32A:261022USD2500,00",
		":50K:/saver",
		":57A:CREDAGRA",
		":59:/DE75512108001245126199",
		"ACME Corp",
		":70:invoice 42",
		":71A:OUR",
		"-}",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message is missing %q", want)
		}
	}

	// Draft payout: 422, the instruction is not yet authorized.
	draftPayoutHTTP(t, srv.URL, "po-mt103-http-2")
	code, _ = postText(t, srv.URL+"/exports/mt103",
		`{"payout_id":"po-mt103-http-2","receiver_lt":"BANKDEFFXXXX"}`)
	if code != http.StatusUnprocessableEntity {
		t.Errorf("draft: status=%d, want 422", code)
	}
}
