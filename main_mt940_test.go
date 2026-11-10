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

// Full MT940 statement export flow through HTTP: seed entries, export
// the statement, and verify the FIN body, the error mapping, the
// pending-review exclusion, and the metrics counter.
func TestHTTPMT940Export(t *testing.T) {
	l := ledger.New()
	acct := ledger.AccountID("ops")
	base := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	seed := []ledger.JournalEntry{
		{ID: "m940-a1", DebitAccount: acct, CreditAccount: "equity", AmountCents: 15000, Currency: "USD", CreatedAt: base.Add(-2 * time.Hour)},
		{ID: "m940-b1", DebitAccount: acct, CreditAccount: "equity", AmountCents: 5000, Currency: "USD", CreatedAt: base.Add(30 * time.Minute), Memo: "salary"},
		{ID: "m940-c1", DebitAccount: "rent", CreditAccount: acct, AmountCents: 2000, Currency: "USD", CreatedAt: base.Add(time.Hour)},
		{ID: "m940-p1", DebitAccount: acct, CreditAccount: "equity", AmountCents: 99900, Currency: "USD", CreatedAt: base.Add(2 * time.Hour), PendingReview: true},
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

	// Missing account: 400.
	if code, _, _ := getText(t, srv.URL+"/exports/mt940?currency=USD"); code != http.StatusBadRequest {
		t.Errorf("missing account: status=%d, want 400", code)
	}
	// Missing currency: 400.
	if code, _, _ := getText(t, srv.URL+"/exports/mt940?account=ops"); code != http.StatusBadRequest {
		t.Errorf("missing currency: status=%d, want 400", code)
	}
	// Invalid currency: 400.
	badCur := url.Values{}
	badCur.Set("account", "ops")
	badCur.Set("currency", "US")
	if code, _, _ := getText(t, srv.URL+"/exports/mt940?"+badCur.Encode()); code != http.StatusBadRequest {
		t.Errorf("invalid currency: status=%d, want 400", code)
	}
	// since > until: 400.
	inverted := url.Values{}
	inverted.Set("account", "ops")
	inverted.Set("currency", "USD")
	inverted.Set("since", base.Add(24*time.Hour).Format(time.RFC3339))
	inverted.Set("until", base.Format(time.RFC3339))
	if code, _, _ := getText(t, srv.URL+"/exports/mt940?"+inverted.Encode()); code != http.StatusBadRequest {
		t.Errorf("since > until: status=%d, want 400", code)
	}
	// Bad since: 400.
	bad := url.Values{}
	bad.Set("account", "ops")
	bad.Set("currency", "USD")
	bad.Set("since", "not-a-time")
	bad.Set("until", base.Add(time.Hour).Format(time.RFC3339))
	if code, _, _ := getText(t, srv.URL+"/exports/mt940?"+bad.Encode()); code != http.StatusBadRequest {
		t.Errorf("bad since: status=%d, want 400", code)
	}

	// Unknown account: 200 with a valid empty statement — zero
	// balance, zero :61: lines, never a 404.
	emptyQ := url.Values{}
	emptyQ.Set("account", "void")
	emptyQ.Set("currency", "USD")
	emptyQ.Set("statement_id", "2026-10")
	emptyQ.Set("since", base.Format(time.RFC3339))
	emptyQ.Set("until", base.Add(24*time.Hour).Format(time.RFC3339))
	code, _, ebody := getText(t, srv.URL+"/exports/mt940?"+emptyQ.Encode())
	if code != http.StatusOK {
		t.Fatalf("empty account: status=%d, want 200", code)
	}
	if !strings.Contains(ebody, ":62F:C261023USD0,00") {
		t.Errorf("empty statement missing zero closing balance, body:\n%s", ebody)
	}
	if n := strings.Count(ebody, ":61:"); n != 0 {
		t.Errorf("empty statement has %d :61: lines, want 0", n)
	}

	code, hdr, body := getText(t, srv.URL+"/exports/mt940?"+q.Encode())
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
		":61:2610221022C50,00NTRF//m940-b1",
		":86:salary",
		":61:2610221022D20,00NTRF//m940-c1",
		":62F:C261023USD180,00",
		":64:C261023USD180,00",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("export missing %q, body:\n%s", want, body)
		}
	}
	if n := strings.Count(body, ":61:"); n != 2 {
		t.Errorf("got %d :61: lines, want 2", n)
	}
	// The pending-review row never touched balances: it must not
	// appear as a line or in the footing.
	if strings.Contains(body, "m940-p1") || strings.Contains(body, "999,00") {
		t.Errorf("pending-review row leaked into export, body:\n%s", body)
	}

	// Re-export is byte-identical: the window pins both balance
	// dates, so no clock value leaks into the document.
	_, _, body2 := getText(t, srv.URL+"/exports/mt940?"+q.Encode())
	if body2 != body {
		t.Errorf("repeat export differs:\nfirst:\n%s\nsecond:\n%s", body, body2)
	}

	// The metrics counter counts the export, including the rejected
	// requests above (5 rejected + 3 ok = 8).
	code, _, mbody := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics: status=%d, want 200", code)
	}
	if !strings.Contains(mbody, "ledger_mt940_exports_total 8") {
		t.Errorf("metrics missing ledger_mt940_exports_total 8")
	}
}
