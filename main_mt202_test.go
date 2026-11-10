package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// mt202TestServer builds an HTTP test server for the MT202 export.
// The endpoint is pure rendering — no ledger state is consulted —
// so the ledger is empty.
func mt202TestServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(newRouter(ledger.New()))
	t.Cleanup(srv.Close)
	return srv
}

const mt202ValidBody = `{
	"sender_lt":"BANKUS33",
	"receiver_lt":"BANKDEFF",
	"transaction_reference":"MT202REF1",
	"related_reference":"RELREF202",
	"value_date":"2026-10-22",
	"currency":"USD",
	"amount_cents":250000,
	"ordering_institution_bic":"DEUTDEFF",
	"beneficiary_institution_bic":"BNPAFRPP"
}`

// Full MT202 flow through HTTP: render the message and exercise the
// error mapping. Every field is caller-supplied; there is no ledger
// lookup, so there is no 404 and no 422.
func TestHTTPMT202Export(t *testing.T) {
	srv := mt202TestServer(t)

	// Malformed JSON: 400.
	code, _, _ := postTextResp(t, srv.URL+"/exports/mt202", `{`)
	if code != http.StatusBadRequest {
		t.Errorf("malformed JSON: status=%d, want 400", code)
	}
	// Trailing garbage after the JSON value: 400.
	code, _, _ = postTextResp(t, srv.URL+"/exports/mt202", mt202ValidBody+` trailing`)
	if code != http.StatusBadRequest {
		t.Errorf("trailing garbage: status=%d, want 400", code)
	}
	// Unknown field: 400 (strict decode).
	code, _, _ = postTextResp(t, srv.URL+"/exports/mt202",
		strings.Replace(mt202ValidBody, `"currency":"USD"`, `"currency":"USD","swift_code":"BANKDEFF"`, 1))
	if code != http.StatusBadRequest {
		t.Errorf("unknown field: status=%d, want 400", code)
	}
	// Missing value_date: 400 (required).
	code, _, _ = postTextResp(t, srv.URL+"/exports/mt202",
		strings.Replace(mt202ValidBody, "\n\t\"value_date\":\"2026-10-22\",\n", "\n", 1))
	if code != http.StatusBadRequest {
		t.Errorf("missing value_date: status=%d, want 400", code)
	}
	// Invalid value_date: 400.
	code, _, _ = postTextResp(t, srv.URL+"/exports/mt202",
		strings.Replace(mt202ValidBody, `"value_date":"2026-10-22"`, `"value_date":"22/10/2026"`, 1))
	if code != http.StatusBadRequest {
		t.Errorf("invalid value_date: status=%d, want 400", code)
	}
	// Invalid :20: (over 16 characters): 400.
	code, _, body := postTextResp(t, srv.URL+"/exports/mt202",
		strings.Replace(mt202ValidBody, `"transaction_reference":"MT202REF1"`, `"transaction_reference":"MT202REF1-TOO-LONG"`, 1))
	if code != http.StatusBadRequest {
		t.Errorf("long :20:: status=%d, want 400", code)
	}
	if !strings.Contains(body, "invalid reference") {
		t.Errorf("long :20: error = %q, want the ledger sentinel text", body)
	}
	// Missing :21: (related reference is required for MT202): 400.
	code, _, _ = postTextResp(t, srv.URL+"/exports/mt202",
		strings.Replace(mt202ValidBody, "\n\t\"related_reference\":\"RELREF202\",\n", "\n", 1))
	if code != http.StatusBadRequest {
		t.Errorf("missing :21:: status=%d, want 400", code)
	}
	// Invalid currency (lowercase): 400.
	code, _, _ = postTextResp(t, srv.URL+"/exports/mt202",
		strings.Replace(mt202ValidBody, `"currency":"USD"`, `"currency":"usd"`, 1))
	if code != http.StatusBadRequest {
		t.Errorf("lowercase currency: status=%d, want 400", code)
	}
	// Non-positive amount: 400.
	code, _, _ = postTextResp(t, srv.URL+"/exports/mt202",
		strings.Replace(mt202ValidBody, `"amount_cents":250000`, `"amount_cents":0`, 1))
	if code != http.StatusBadRequest {
		t.Errorf("zero amount: status=%d, want 400", code)
	}
	code, _, _ = postTextResp(t, srv.URL+"/exports/mt202",
		strings.Replace(mt202ValidBody, `"amount_cents":250000`, `"amount_cents":-100`, 1))
	if code != http.StatusBadRequest {
		t.Errorf("negative amount: status=%d, want 400", code)
	}
	// Invalid BIC: 400.
	code, _, _ = postTextResp(t, srv.URL+"/exports/mt202",
		strings.Replace(mt202ValidBody, `"ordering_institution_bic":"DEUTDEFF"`, `"ordering_institution_bic":"NOTABIC"`, 1))
	if code != http.StatusBadRequest {
		t.Errorf("invalid BIC: status=%d, want 400", code)
	}
	// Invalid sender LT: 400.
	code, _, _ = postTextResp(t, srv.URL+"/exports/mt202",
		strings.Replace(mt202ValidBody, `"sender_lt":"BANKUS33"`, `"sender_lt":"BAD"`, 1))
	if code != http.StatusBadRequest {
		t.Errorf("invalid sender LT: status=%d, want 400", code)
	}

	// Happy path: Content-Type text/plain, exact FIN lines.
	code, ctype, msg := postTextResp(t, srv.URL+"/exports/mt202", mt202ValidBody)
	if code != http.StatusOK {
		t.Fatalf("export: status=%d body=%s, want 200", code, msg)
	}
	if ctype != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/plain; charset=utf-8", ctype)
	}
	for _, want := range []string{
		"{1:F01BANKUS33XXX0000000000}",
		"{2:I202BANKDEFFXXXU3003}",
		"{3:{108:MT202REF1}}",
		":20:MT202REF1",
		":21:RELREF202",
		":32A:261022USD2500,00",
		":52A:DEUTDEFF",
		":58A:BNPAFRPP",
		"-}",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message is missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, ":72:") || strings.Contains(msg, ":52D:") || strings.Contains(msg, ":58D:") {
		t.Errorf("message carries unexpected optional fields:\n%s", msg)
	}

	// Repeat export: byte-identical.
	code, _, again := postTextResp(t, srv.URL+"/exports/mt202", mt202ValidBody)
	if code != http.StatusOK {
		t.Fatalf("repeat export: status=%d, want 200", code)
	}
	if again != msg {
		t.Errorf("repeat export differs:\nfirst:\n%s\nsecond:\n%s", msg, again)
	}
}
