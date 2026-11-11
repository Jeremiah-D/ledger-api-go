package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// Full MT941 balance report flow through HTTP: seed entries, export
// the report, and verify the FIN body, the pending-review exclusion,
// the byte-identical repeat, and the metrics counter. MT941 carries
// balances and summaries but no :61: lines; the pre-window row folds
// into :60F:, the window rows into the :90D:/:90C: trailers.
func TestHTTPMT941Export(t *testing.T) {
	l := ledger.New()
	acct := ledger.AccountID("ops")
	base := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	seed := []ledger.JournalEntry{
		{ID: "m941-a1", DebitAccount: acct, CreditAccount: "equity", AmountCents: 15000, Currency: "USD", CreatedAt: base.Add(-2 * time.Hour)},
		{ID: "m941-b1", DebitAccount: acct, CreditAccount: "equity", AmountCents: 5000, Currency: "USD", CreatedAt: base.Add(30 * time.Minute)},
		{ID: "m941-c1", DebitAccount: "rent", CreditAccount: acct, AmountCents: 2000, Currency: "USD", CreatedAt: base.Add(time.Hour)},
		{ID: "m941-p1", DebitAccount: acct, CreditAccount: "equity", AmountCents: 99900, Currency: "USD", CreatedAt: base.Add(2 * time.Hour), PendingReview: true},
	}
	for _, e := range seed {
		if _, dup, err := l.Post(e); err != nil || dup {
			t.Fatalf("seed Post(%s) = dup=%v err=%v", e.ID, dup, err)
		}
	}
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	getText := func(t *testing.T, raw string) (int, http.Header, string) {
		t.Helper()
		resp, err := http.Get(raw)
		if err != nil {
			t.Fatalf("GET %s: %v", raw, err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read %s: %v", raw, err)
		}
		return resp.StatusCode, resp.Header, string(body)
	}
	q := url.Values{}
	q.Set("account", "ops")
	q.Set("currency", "USD")
	q.Set("statement_id", "2026-10")
	q.Set("since", base.Format(time.RFC3339))
	q.Set("until", base.Add(24*time.Hour).Format(time.RFC3339))

	// Unknown account: 200 with a valid empty report — zero
	// balances, zero trailers, no :60F:, never a 404.
	emptyQ := url.Values{}
	emptyQ.Set("account", "void")
	emptyQ.Set("currency", "USD")
	emptyQ.Set("statement_id", "2026-10")
	emptyQ.Set("since", base.Format(time.RFC3339))
	emptyQ.Set("until", base.Add(24*time.Hour).Format(time.RFC3339))
	code, _, ebody := getText(t, srv.URL+"/exports/mt941?"+emptyQ.Encode())
	if code != http.StatusOK {
		t.Fatalf("empty account: status=%d, want 200", code)
	}
	for _, want := range []string{":62F:C261023USD0,00", ":90D:00,00", ":90C:00,00"} {
		if !strings.Contains(ebody, want) {
			t.Errorf("empty report missing %q, body:\n%s", want, ebody)
		}
	}
	for _, forbidden := range []string{":60F:", ":61:"} {
		if strings.Contains(ebody, forbidden) {
			t.Errorf("empty report must not carry %s, body:\n%s", forbidden, ebody)
		}
	}

	code, hdr, body := getText(t, srv.URL+"/exports/mt941?"+q.Encode())
	if code != http.StatusOK {
		t.Fatalf("export: status=%d, want 200", code)
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content-type = %q, want text/plain", ct)
	}
	// Opening 150,00 + credits 50,00 − debits 20,00 = closing 180,00.
	for _, want := range []string{
		":20:2026-10",
		":25:ops",
		":28C:2026-10/1",
		":13D:2610230900",
		":60F:C261022USD150,00",
		":90D:120,00",
		":90C:150,00",
		":62F:C261023USD180,00",
		":64:C261023USD180,00",
		":65:C261023USD180,00",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("export missing %q, body:\n%s", want, body)
		}
	}
	for _, forbidden := range []string{":61:", "m941-a1", "m941-b1", "m941-c1"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("export must not contain %q, body:\n%s", forbidden, body)
		}
	}
	// The pending-review row never touched balances: it must not
	// appear in the trailers or the closing balance.
	if strings.Contains(body, "999,00") {
		t.Errorf("pending-review row leaked into export, body:\n%s", body)
	}
	// statement_id/sequence_id default to "1" when omitted.
	defQ := url.Values{}
	defQ.Set("account", "ops")
	defQ.Set("currency", "USD")
	defQ.Set("since", base.Format(time.RFC3339))
	defQ.Set("until", base.Add(24*time.Hour).Format(time.RFC3339))
	_, _, defBody := getText(t, srv.URL+"/exports/mt941?"+defQ.Encode())
	if !strings.Contains(defBody, ":20:1\n") || !strings.Contains(defBody, ":28C:1/1") {
		t.Errorf("statement/sequence must default to \"1\":\n%s", defBody)
	}

	// Re-export is byte-identical: Until pins the :13D:, so no clock
	// value leaks into the document.
	_, _, body2 := getText(t, srv.URL+"/exports/mt941?"+q.Encode())
	if body2 != body {
		t.Errorf("repeat export differs:\nfirst:\n%s\nsecond:\n%s", body, body2)
	}

	// The metrics counter counts the export (1 empty + 1 export +
	// 1 default + 1 repeat = 4).
	code, _, mbody := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics: status=%d, want 200", code)
	}
	if !strings.Contains(mbody, "ledger_mt941_exports_total 4") {
		t.Errorf("metrics missing ledger_mt941_exports_total 4")
	}
}

// Error mapping for GET /exports/mt941: missing account/currency, an
// invalid currency, an inverted window, or a malformed since/until
// is a 400 — and rejected requests still count in the metrics.
func TestHTTPMT941ExportErrors(t *testing.T) {
	l := ledger.New()
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	getCode := func(t *testing.T, raw string) int {
		t.Helper()
		resp, err := http.Get(raw)
		if err != nil {
			t.Fatalf("GET %s: %v", raw, err)
		}
		defer resp.Body.Close()
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			t.Fatalf("read %s: %v", raw, err)
		}
		return resp.StatusCode
	}
	base := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)

	// Missing account: 400.
	if code := getCode(t, srv.URL+"/exports/mt941?currency=USD"); code != http.StatusBadRequest {
		t.Errorf("missing account: status=%d, want 400", code)
	}
	// Missing currency: 400.
	if code := getCode(t, srv.URL+"/exports/mt941?account=ops"); code != http.StatusBadRequest {
		t.Errorf("missing currency: status=%d, want 400", code)
	}
	// Invalid currency: 400.
	badCur := url.Values{}
	badCur.Set("account", "ops")
	badCur.Set("currency", "US")
	if code := getCode(t, srv.URL+"/exports/mt941?"+badCur.Encode()); code != http.StatusBadRequest {
		t.Errorf("invalid currency: status=%d, want 400", code)
	}
	// since > until: 400.
	inverted := url.Values{}
	inverted.Set("account", "ops")
	inverted.Set("currency", "USD")
	inverted.Set("since", base.Add(24*time.Hour).Format(time.RFC3339))
	inverted.Set("until", base.Format(time.RFC3339))
	if code := getCode(t, srv.URL+"/exports/mt941?"+inverted.Encode()); code != http.StatusBadRequest {
		t.Errorf("since > until: status=%d, want 400", code)
	}
	// Bad since: 400.
	bad := url.Values{}
	bad.Set("account", "ops")
	bad.Set("currency", "USD")
	bad.Set("since", "not-a-time")
	bad.Set("until", base.Add(time.Hour).Format(time.RFC3339))
	if code := getCode(t, srv.URL+"/exports/mt941?"+bad.Encode()); code != http.StatusBadRequest {
		t.Errorf("bad since: status=%d, want 400", code)
	}

	// Rejected requests still count in the metrics (5 above).
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	mbody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics: %v", err)
	}
	if !strings.Contains(string(mbody), "ledger_mt941_exports_total 5") {
		t.Errorf("metrics missing ledger_mt941_exports_total 5")
	}
}
