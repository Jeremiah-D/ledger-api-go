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

// Full camt.052 intraday report flow through HTTP: seed entries, export
// the report, and verify the XML body, the error mapping, and the
// metrics counter.
func TestHTTPCAMT052Export(t *testing.T) {
	l := ledger.New()
	acct := ledger.AccountID("ops")
	base := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	seed := []ledger.JournalEntry{
		{ID: "c52-b1", DebitAccount: acct, CreditAccount: "equity", AmountCents: 15000, Currency: "USD", CreatedAt: base.Add(-2 * time.Hour)},
		{ID: "c52-w1", DebitAccount: acct, CreditAccount: "equity", AmountCents: 5000, Currency: "USD", CreatedAt: base.Add(30 * time.Minute)},
		{ID: "c52-w2", DebitAccount: "rent", CreditAccount: acct, AmountCents: 2000, Currency: "USD", CreatedAt: base.Add(time.Hour)},
	}
	for _, e := range seed {
		if _, dup, err := l.Post(e); err != nil || dup {
			t.Fatalf("seed Post(%s) = dup=%v err=%v", e.ID, dup, err)
		}
	}
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	getXML := func(t *testing.T, raw string) (int, http.Header, string) {
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
	q.Set("rpt_id", "RPT-HTTP-1")
	q.Set("since", base.Format(time.RFC3339))
	q.Set("until", base.Add(24*time.Hour).Format(time.RFC3339))

	// Missing rpt_id: 400.
	code, _, _ := getXML(t, srv.URL+"/exports/camt052?account=ops")
	if code != http.StatusBadRequest {
		t.Errorf("missing rpt_id: status=%d, want 400", code)
	}
	// Bad since: 400.
	bad := url.Values{}
	bad.Set("account", "ops")
	bad.Set("rpt_id", "R")
	bad.Set("since", "not-a-time")
	bad.Set("until", base.Add(time.Hour).Format(time.RFC3339))
	if code, _, _ := getXML(t, srv.URL+"/exports/camt052?"+bad.Encode()); code != http.StatusBadRequest {
		t.Errorf("bad since: status=%d, want 400", code)
	}

	code, hdr, body := getXML(t, srv.URL+"/exports/camt052?"+q.Encode())
	if code != http.StatusOK {
		t.Fatalf("export: status=%d, want 200", code)
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "application/xml") {
		t.Errorf("content-type = %q, want application/xml", ct)
	}
	for _, want := range []string{
		"camt.052.001.02",
		"<Id>RPT-HTTP-1</Id>",
		`<Cd>OPBD</Cd>`, `<Cd>CLBD</Cd>`,
		`<Amt Ccy="USD">150.00</Amt>`, `<Amt Ccy="USD">180.00</Amt>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("export missing %q", want)
		}
	}
	if n := strings.Count(body, "<Ntry>"); n != 2 {
		t.Errorf("got %d <Ntry> rows, want 2", n)
	}

	// The metrics counter counts the export, including the rejected
	// requests above (2 rejected + 1 ok = 3).
	code, _, mbody := getXML(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics: status=%d, want 200", code)
	}
	if !strings.Contains(mbody, "ledger_camt052_exports_total 3") {
		t.Errorf("metrics missing ledger_camt052_exports_total 3")
	}
}
