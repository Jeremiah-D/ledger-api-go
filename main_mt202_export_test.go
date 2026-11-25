package main

import (
	"net/http"
	"strings"
	"testing"
)

// Full MT202 export flow through HTTP with registration (LG-470):
// the first export renders and registers, a repeat returns the
// stored message, and a :20: reused for different terms is a 422.
func TestHTTPMT202ExportRegistered(t *testing.T) {
	srv := mt202TestServer(t)

	code, ctype, msg := postTextResp(t, srv.URL+"/exports/mt202", mt202ValidBody)
	if code != http.StatusOK {
		t.Fatalf("export: status=%d body=%s, want 200", code, msg)
	}
	if ctype != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/plain; charset=utf-8", ctype)
	}
	for _, want := range []string{
		":20:MT202REF1",
		":21:RELREF202",
		":32A:261022USD2500,00",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message is missing %q:\n%s", want, msg)
		}
	}

	// Repeat export: the stored message, byte-identical.
	code, _, again := postTextResp(t, srv.URL+"/exports/mt202", mt202ValidBody)
	if code != http.StatusOK {
		t.Fatalf("repeat export: status=%d, want 200", code)
	}
	if again != msg {
		t.Errorf("repeat export differs:\nfirst:\n%s\nsecond:\n%s", msg, again)
	}
}

func TestHTTPMT202ExportConflict(t *testing.T) {
	srv := mt202TestServer(t)

	code, _, _ := postTextResp(t, srv.URL+"/exports/mt202", mt202ValidBody)
	if code != http.StatusOK {
		t.Fatalf("first export: status=%d, want 200", code)
	}
	// Same :20:, different amount: 422, the ledger sentinel text
	// passed through verbatim.
	code, _, body := postTextResp(t, srv.URL+"/exports/mt202",
		strings.Replace(mt202ValidBody, `"amount_cents":250000`, `"amount_cents":250001`, 1))
	if code != http.StatusUnprocessableEntity {
		t.Errorf("conflicting :20:: status=%d, want 422", code)
	}
	if !strings.Contains(body, "reused for different terms") {
		t.Errorf("conflict error = %q, want the ledger sentinel text", body)
	}
	// A field error is still a 400, unchanged by registration.
	code, _, _ = postTextResp(t, srv.URL+"/exports/mt202",
		strings.Replace(mt202ValidBody, `"currency":"USD"`, `"currency":"usd"`, 1))
	if code != http.StatusBadRequest {
		t.Errorf("lowercase currency: status=%d, want 400", code)
	}
	// The conflict overwrote nothing: the original export still
	// replays byte-identical.
	code, _, msg := postTextResp(t, srv.URL+"/exports/mt202", mt202ValidBody)
	if code != http.StatusOK {
		t.Fatalf("replay after conflict: status=%d, want 200", code)
	}
	if !strings.Contains(msg, ":32A:261022USD2500,00") {
		t.Errorf("replay after conflict lost the first message:\n%s", msg)
	}
}
