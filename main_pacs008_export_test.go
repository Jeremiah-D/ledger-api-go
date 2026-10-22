package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// TestPacs008ExportEndpoint covers GET /exports/pacs008 (LG-176): the
// happy-path render plus the 400/404 error mapping. The module-level
// tests (ledger_pacs008_export_test.go) own the wire-format and
// fail-closed coverage; this is the HTTP contract.
func TestPacs008ExportEndpoint(t *testing.T) {
	l := ledger.New(ledger.WithPayoutClearingAccount("payout-clearing"))
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	if _, _, err := l.CreatePayout(ledger.PayoutInstruction{
		ID:             "po-http-176",
		PayeeAccount:   "saver",
		AmountCents:    12500,
		Currency:       "USD",
		Destination:    "FPS:6222000012345678",
		Maker:          "alice",
		IdempotencyKey: "idem-po-http-176",
	}); err != nil {
		t.Fatalf("CreatePayout: %v", err)
	}

	get := func(query string) (int, string, string) {
		resp, err := http.Get(srv.URL + "/exports/pacs008" + query)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		return resp.StatusCode, resp.Header.Get("Content-Type"), string(body)
	}

	// Happy path: deterministic MsgId, E2E Id from the idempotency
	// key, and the gpi UETR all render on the wire.
	code, ctype, body := get("?payout=po-http-176")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", code, body)
	}
	if !strings.HasPrefix(ctype, "application/xml") {
		t.Errorf("content-type = %q, want application/xml", ctype)
	}
	for _, want := range []string{
		"FIToFICstmrCdtTrf",
		"<MsgId>po-http-176/pacs008</MsgId>",
		"<EndToEndId>idem-po-http-176</EndToEndId>",
		"<UETR>",
		"Ccy=\"USD\">125.00<",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("export missing %q:\n%s", want, body)
		}
	}

	// Missing payout parameter is a 400.
	if code, _, _ := get(""); code != http.StatusBadRequest {
		t.Errorf("missing payout: status = %d, want 400", code)
	}

	// Unknown payout ID is a 404.
	if code, _, body := get("?payout=nope"); code != http.StatusNotFound {
		t.Errorf("unknown payout: status = %d, want 404 (body: %s)", code, body)
	}
}
