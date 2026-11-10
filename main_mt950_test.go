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

// Full MT950 statement export flow through HTTP: seed entries, export
// the statement, and verify the FIN body, the pending-review
// exclusion, the byte-identical repeat, and the metrics counter.
// MT950 has no :21: tag, and the balances carry both :64: and :65:.
func TestHTTPMT950Export(t *testing.T) {
	l := ledger.New()
	acct := ledger.AccountID("ops")
	base := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	seed := []ledger.JournalEntry{
		{ID: "m950-a1", DebitAccount: acct, CreditAccount: "equity", AmountCents: 15000, Currency: "USD", CreatedAt: base.Add(-2 * time.Hour)},
		{ID: "m950-b1", DebitAccount: acct, CreditAccount: "equity", AmountCents: 5000, Currency: "USD", CreatedAt: base.Add(30 * time.Minute), Memo: "salary"},
		{ID: "m950-c1", DebitAccount: "rent", CreditAccount: acct, AmountCents: 2000, Currency: "USD", CreatedAt: base.Add(time.Hour)},
		{ID: "m950-p1", DebitAccount: acct, CreditAccount: "equity", AmountCents: 99900, Currency: "USD", CreatedAt: base.Add(2 * time.Hour), PendingReview: true},
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

	// Unknown account: 200 with a valid empty statement — zero
	// balance, zero :61: lines, never a 404.
	emptyQ := url.Values{}
	emptyQ.Set("account", "void")
	emptyQ.Set("currency", "USD")
	emptyQ.Set("statement_id", "2026-10")
	emptyQ.Set("since", base.Format(time.RFC3339))
	emptyQ.Set("until", base.Add(24*time.Hour).Format(time.RFC3339))
	code, _, ebody := getText(t, srv.URL+"/exports/mt950?"+emptyQ.Encode())
	if code != http.StatusOK {
		t.Fatalf("empty account: status=%d, want 200", code)
	}
	if !strings.Contains(ebody, ":62F:C261023USD0,00") {
		t.Errorf("empty statement missing zero closing balance, body:\n%s", ebody)
	}
	if n := strings.Count(ebody, ":61:"); n != 0 {
		t.Errorf("empty statement has %d :61: lines, want 0", n)
	}
	if strings.Contains(ebody, ":21:") {
		t.Errorf("empty statement must not carry a :21: tag, body:\n%s", ebody)
	}

	code, hdr, body := getText(t, srv.URL+"/exports/mt950?"+q.Encode())
	if code != http.StatusOK {
		t.Fatalf("export: status=%d, want 200", code)
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content-type = %q, want text/plain", ct)
	}
	// Opening folds the pre-window row (15000); the in-window rows
	// foot to 15000 + 5000 - 2000 = 18000.
	for _, want := range []string{
		":20:2026-10",
		":25:ops",
		":28C:2026-10/1",
		":60F:C261022USD150,00",
		":61:2610221022C50,00NTRF//m950-b1",
		":86:salary",
		":61:2610221022D20,00NTRF//m950-c1",
		":62F:C261023USD180,00",
		":64:C261023USD180,00",
		":65:C261023USD180,00",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("export missing %q, body:\n%s", want, body)
		}
	}
	if n := strings.Count(body, ":61:"); n != 2 {
		t.Errorf("got %d :61: lines, want 2", n)
	}
	if strings.Contains(body, ":21:") {
		t.Errorf("MT950 must not carry a :21: related reference, body:\n%s", body)
	}
	// The pending-review row never touched balances: it must not
	// appear as a line or in the footing.
	if strings.Contains(body, "m950-p1") || strings.Contains(body, "999,00") {
		t.Errorf("pending-review row leaked into export, body:\n%s", body)
	}
	// statement_id/sequence_id default to "1" when omitted.
	defQ := url.Values{}
	defQ.Set("account", "ops")
	defQ.Set("currency", "USD")
	defQ.Set("since", base.Format(time.RFC3339))
	defQ.Set("until", base.Add(24*time.Hour).Format(time.RFC3339))
	_, _, defBody := getText(t, srv.URL+"/exports/mt950?"+defQ.Encode())
	if !strings.Contains(defBody, ":20:1\n") || !strings.Contains(defBody, ":28C:1/1") {
		t.Errorf("statement/sequence must default to \"1\":\n%s", defBody)
	}

	// Re-export is byte-identical: the window pins both balance
	// dates, so no clock value leaks into the document.
	_, _, body2 := getText(t, srv.URL+"/exports/mt950?"+q.Encode())
	if body2 != body {
		t.Errorf("repeat export differs:\nfirst:\n%s\nsecond:\n%s", body, body2)
	}

	// The metrics counter counts the export (1 empty + 1 export +
	// 1 default + 1 repeat = 4).
	code, _, mbody := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics: status=%d, want 200", code)
	}
	if !strings.Contains(mbody, "ledger_mt950_exports_total 4") {
		t.Errorf("metrics missing ledger_mt950_exports_total 4")
	}
}

// Error mapping for GET /exports/mt950: missing account/currency, an
// invalid currency, an inverted window, or a malformed since/until
// is a 400 — and rejected requests still count in the metrics.
func TestHTTPMT950ExportErrors(t *testing.T) {
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
	if code := getCode(t, srv.URL+"/exports/mt950?currency=USD"); code != http.StatusBadRequest {
		t.Errorf("missing account: status=%d, want 400", code)
	}
	// Missing currency: 400.
	if code := getCode(t, srv.URL+"/exports/mt950?account=ops"); code != http.StatusBadRequest {
		t.Errorf("missing currency: status=%d, want 400", code)
	}
	// Invalid currency: 400.
	badCur := url.Values{}
	badCur.Set("account", "ops")
	badCur.Set("currency", "US")
	if code := getCode(t, srv.URL+"/exports/mt950?"+badCur.Encode()); code != http.StatusBadRequest {
		t.Errorf("invalid currency: status=%d, want 400", code)
	}
	// since > until: 400.
	inverted := url.Values{}
	inverted.Set("account", "ops")
	inverted.Set("currency", "USD")
	inverted.Set("since", base.Add(24*time.Hour).Format(time.RFC3339))
	inverted.Set("until", base.Format(time.RFC3339))
	if code := getCode(t, srv.URL+"/exports/mt950?"+inverted.Encode()); code != http.StatusBadRequest {
		t.Errorf("since > until: status=%d, want 400", code)
	}
	// Bad since: 400.
	bad := url.Values{}
	bad.Set("account", "ops")
	bad.Set("currency", "USD")
	bad.Set("since", "not-a-time")
	bad.Set("until", base.Add(time.Hour).Format(time.RFC3339))
	if code := getCode(t, srv.URL+"/exports/mt950?"+bad.Encode()); code != http.StatusBadRequest {
		t.Errorf("bad since: status=%d, want 400", code)
	}

	// The metrics counter counts the rejected requests too (5).
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	mbody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics: %v", err)
	}
	if !strings.Contains(string(mbody), "ledger_mt950_exports_total 5") {
		t.Errorf("metrics missing ledger_mt950_exports_total 5")
	}
}
