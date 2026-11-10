package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// Full camt.053 statement export flow through HTTP: seed entries,
// export the statement, and verify the XML body, the error mapping,
// the pending-review exclusion, the msg_id default, and the metrics
// counter.
//
// Note on repeat exports: GrpHdr/CreDtTm is a second-resolution wall
// clock (ledger.RenderCAMT053Statement), so two exports a second apart
// differ in that one line — the test strips it before comparing, and
// pins the MsgId as the stable identifier instead (the LG-375
// pacs.007 precedent, whose CreDtTm is likewise wall-clock).
func TestHTTPCAMT053Export(t *testing.T) {
	l := ledger.New()
	acct := ledger.AccountID("ops")
	base := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	seed := []ledger.JournalEntry{
		{ID: "m53-a1", DebitAccount: acct, CreditAccount: "equity", AmountCents: 15000, Currency: "USD", CreatedAt: base.Add(-2 * time.Hour)},
		{ID: "m53-b1", DebitAccount: acct, CreditAccount: "equity", AmountCents: 5000, Currency: "USD", CreatedAt: base.Add(30 * time.Minute), Memo: "salary"},
		{ID: "m53-c1", DebitAccount: "rent", CreditAccount: acct, AmountCents: 2000, Currency: "USD", CreatedAt: base.Add(time.Hour)},
		{ID: "m53-p1", DebitAccount: acct, CreditAccount: "equity", AmountCents: 99900, Currency: "USD", CreatedAt: base.Add(2 * time.Hour), PendingReview: true},
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
	q.Set("stmt_id", "2026-10")
	q.Set("since", base.Format(time.RFC3339))
	q.Set("until", base.Add(24*time.Hour).Format(time.RFC3339))

	// Missing account: 400.
	if code, _, _ := getText(t, srv.URL+"/exports/camt053?stmt_id=2026-10"); code != http.StatusBadRequest {
		t.Errorf("missing account: status=%d, want 400", code)
	}
	// Missing stmt_id: 400.
	if code, _, _ := getText(t, srv.URL+"/exports/camt053?account=ops"); code != http.StatusBadRequest {
		t.Errorf("missing stmt_id: status=%d, want 400", code)
	}
	// Invalid currency: 400.
	badCur := url.Values{}
	badCur.Set("account", "ops")
	badCur.Set("stmt_id", "2026-10")
	badCur.Set("currency", "US")
	if code, _, _ := getText(t, srv.URL+"/exports/camt053?"+badCur.Encode()); code != http.StatusBadRequest {
		t.Errorf("invalid currency: status=%d, want 400", code)
	}
	// since > until: 400.
	inverted := url.Values{}
	inverted.Set("account", "ops")
	inverted.Set("stmt_id", "2026-10")
	inverted.Set("since", base.Add(24*time.Hour).Format(time.RFC3339))
	inverted.Set("until", base.Format(time.RFC3339))
	if code, _, _ := getText(t, srv.URL+"/exports/camt053?"+inverted.Encode()); code != http.StatusBadRequest {
		t.Errorf("since > until: status=%d, want 400", code)
	}
	// Bad since: 400.
	bad := url.Values{}
	bad.Set("account", "ops")
	bad.Set("stmt_id", "2026-10")
	bad.Set("since", "not-a-time")
	bad.Set("until", base.Add(time.Hour).Format(time.RFC3339))
	if code, _, _ := getText(t, srv.URL+"/exports/camt053?"+bad.Encode()); code != http.StatusBadRequest {
		t.Errorf("bad since: status=%d, want 400", code)
	}

	// Unknown account: 200 with a valid empty statement — one Stmt,
	// zero Ntry, never a 404.
	emptyQ := url.Values{}
	emptyQ.Set("account", "void")
	emptyQ.Set("stmt_id", "2026-10")
	emptyQ.Set("since", base.Format(time.RFC3339))
	emptyQ.Set("until", base.Add(24*time.Hour).Format(time.RFC3339))
	code, _, ebody := getText(t, srv.URL+"/exports/camt053?"+emptyQ.Encode())
	if code != http.StatusOK {
		t.Fatalf("empty account: status=%d, want 200", code)
	}
	if !strings.Contains(ebody, "<Stmt>") || !strings.Contains(ebody, "<Id>2026-10</Id>") {
		t.Errorf("empty statement missing Stmt/Id, body:\n%s", ebody)
	}
	if n := strings.Count(ebody, "<Ntry>"); n != 0 {
		t.Errorf("empty statement has %d Ntry, want 0", n)
	}

	code, hdr, body := getText(t, srv.URL+"/exports/camt053?"+q.Encode())
	if code != http.StatusOK {
		t.Fatalf("export: status=%d, want 200", code)
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "application/xml") {
		t.Errorf("content-type = %q, want application/xml", ct)
	}
	// The account is the debit leg on b1 (money in) and the credit
	// leg on c1 (money out); amounts are absolute, the sign lives in
	// CdtDbtInd. msg_id defaults to stmt_id.
	for _, want := range []string{
		"<MsgId>2026-10</MsgId>",
		"<Id>2026-10</Id>",
		"<Id>ops</Id>",
		"<Amt Ccy=\"USD\">50.00</Amt>",
		"<CdtDbtInd>CRDT</CdtDbtInd>",
		"<DtTm>2026-10-22T09:30:00Z</DtTm>",
		"<EndToEndId>m53-b1</EndToEndId>",
		"<Ustrd>salary</Ustrd>",
		"<Amt Ccy=\"USD\">20.00</Amt>",
		"<CdtDbtInd>DBIT</CdtDbtInd>",
		"<DtTm>2026-10-22T10:00:00Z</DtTm>",
		"<EndToEndId>m53-c1</EndToEndId>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("export missing %q, body:\n%s", want, body)
		}
	}
	if n := strings.Count(body, "<Ntry>"); n != 2 {
		t.Errorf("got %d Ntry, want 2 (pre-window row excluded)", n)
	}
	// The pending-review row never touched balances: it must not
	// appear as an entry or in the amounts.
	if strings.Contains(body, "m53-p1") || strings.Contains(body, "999.00") {
		t.Errorf("pending-review row leaked into export, body:\n%s", body)
	}

	// An explicit msg_id overrides the stmt_id default.
	msgQ := url.Values{}
	msgQ.Set("account", "ops")
	msgQ.Set("stmt_id", "2026-10")
	msgQ.Set("msg_id", "MSG-9")
	msgQ.Set("since", base.Format(time.RFC3339))
	msgQ.Set("until", base.Add(24*time.Hour).Format(time.RFC3339))
	_, _, msgBody := getText(t, srv.URL+"/exports/camt053?"+msgQ.Encode())
	if !strings.Contains(msgBody, "<MsgId>MSG-9</MsgId>") {
		t.Errorf("explicit msg_id not honoured, body:\n%s", msgBody)
	}

	// Re-export is identical apart from the wall-clock CreDtTm line.
	stripCreDtTm := regexp.MustCompile(`<CreDtTm>[^<]*</CreDtTm>`).ReplaceAllString(body, "<CreDtTm/>")
	_, _, body2 := getText(t, srv.URL+"/exports/camt053?"+q.Encode())
	if stripCreDtTm != regexp.MustCompile(`<CreDtTm>[^<]*</CreDtTm>`).ReplaceAllString(body2, "<CreDtTm/>") {
		t.Errorf("repeat export differs beyond CreDtTm:\nfirst:\n%s\nsecond:\n%s", body, body2)
	}

	// The metrics counter counts the export, including the rejected
	// requests above (5 rejected + 4 ok = 9).
	code, _, mbody := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics: status=%d, want 200", code)
	}
	if !strings.Contains(mbody, "ledger_camt053_exports_total 9") {
		t.Errorf("metrics missing ledger_camt053_exports_total 9")
	}
}
