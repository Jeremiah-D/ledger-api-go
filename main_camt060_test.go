package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// Full camt.060 account reporting request flow through HTTP: seed an
// account with a registered IBAN, request a report, and verify the XML
// body, the error mapping, the deterministic MsgId, and the metrics
// counter.
//
// Note on repeat exports: GrpHdr/CreDtTm is a second-resolution wall
// clock (ledger.RenderCAMT060Request), so two exports a second apart
// differ in that one line — the test strips it before comparing, and
// pins the MsgId as the stable identifier instead (the camt.053
// precedent).
func TestHTTPCAMT060Export(t *testing.T) {
	l := ledger.New()
	base := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	if err := l.SetAccountIBAN("alice", "DE89370400440532013000", "DEUTDEFF"); err != nil {
		t.Fatalf("SetAccountIBAN alice: %v", err)
	}
	if err := l.SetAccountIBAN("bob", "GB29NWBK60161331926819", ""); err != nil {
		t.Fatalf("SetAccountIBAN bob: %v", err)
	}
	// "ops" is known (it has journal entries) but has no IBAN.
	if _, dup, err := l.Post(ledger.JournalEntry{ID: "m60-a1", DebitAccount: "ops", CreditAccount: "equity", AmountCents: 15000, Currency: "USD", CreatedAt: base}); err != nil || dup {
		t.Fatalf("seed Post(m60-a1) = dup=%v err=%v", dup, err)
	}
	// "alice" is known through her IBAN registration alone; one entry
	// also makes her entry-known.
	if _, dup, err := l.Post(ledger.JournalEntry{ID: "m60-a2", DebitAccount: "alice", CreditAccount: "equity", AmountCents: 5000, Currency: "USD", CreatedAt: base}); err != nil || dup {
		t.Fatalf("seed Post(m60-a2) = dup=%v err=%v", dup, err)
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
	q.Set("account", "alice")
	q.Set("since", base.Format(time.RFC3339))
	q.Set("until", base.Add(24*time.Hour).Format(time.RFC3339))

	// Missing account: 400.
	if code, _, _ := getText(t, srv.URL+"/exports/camt060"); code != http.StatusBadRequest {
		t.Errorf("missing account: status=%d, want 400", code)
	}
	// Bad since: 400.
	bad := url.Values{}
	bad.Set("account", "alice")
	bad.Set("since", "not-a-time")
	if code, _, _ := getText(t, srv.URL+"/exports/camt060?"+bad.Encode()); code != http.StatusBadRequest {
		t.Errorf("bad since: status=%d, want 400", code)
	}
	// since > until: 400.
	inverted := url.Values{}
	inverted.Set("account", "alice")
	inverted.Set("since", base.Add(24*time.Hour).Format(time.RFC3339))
	inverted.Set("until", base.Format(time.RFC3339))
	if code, _, _ := getText(t, srv.URL+"/exports/camt060?"+inverted.Encode()); code != http.StatusBadRequest {
		t.Errorf("since > until: status=%d, want 400", code)
	}
	// Lone until (no since): 400 — FrToDt cannot express an open start.
	lone := url.Values{}
	lone.Set("account", "alice")
	lone.Set("until", base.Add(24*time.Hour).Format(time.RFC3339))
	if code, _, _ := getText(t, srv.URL+"/exports/camt060?"+lone.Encode()); code != http.StatusBadRequest {
		t.Errorf("lone until: status=%d, want 400", code)
	}
	// Unknown account: 404 — a report request about an account the
	// ledger never saw is a client error, not an empty report.
	unknown := url.Values{}
	unknown.Set("account", "void")
	if code, _, _ := getText(t, srv.URL+"/exports/camt060?"+unknown.Encode()); code != http.StatusNotFound {
		t.Errorf("unknown account: status=%d, want 404", code)
	}
	// Known account with no registered IBAN: 422, fail-closed.
	noIBAN := url.Values{}
	noIBAN.Set("account", "ops")
	if code, _, _ := getText(t, srv.URL+"/exports/camt060?"+noIBAN.Encode()); code != http.StatusUnprocessableEntity {
		t.Errorf("no IBAN: status=%d, want 422", code)
	}

	code, hdr, body := getText(t, srv.URL+"/exports/camt060?"+q.Encode())
	if code != http.StatusOK {
		t.Fatalf("export: status=%d, want 200", code)
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "application/xml") {
		t.Errorf("content-type = %q, want application/xml", ct)
	}
	for _, want := range []string{
		`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.060.001.03">`,
		"<AcctRptgReq>",
		"<IBAN>DE89370400440532013000</IBAN>",
		"<FrDtTm>2026-10-22T09:00:00Z</FrDtTm>",
		"<ToDtTm>2026-10-23T09:00:00Z</ToDtTm>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("export missing %q, body:\n%s", want, body)
		}
	}
	// The MsgId is deterministic in (account, window); RptgReq/Id
	// carries the same identity.
	wantMsgID := "camt060-alice-" +
		strconv.FormatInt(base.Unix(), 10) + "-" +
		strconv.FormatInt(base.Add(24*time.Hour).Unix(), 10)
	if !strings.Contains(body, "<MsgId>"+wantMsgID+"</MsgId>") {
		t.Errorf("export missing deterministic MsgId %q, body:\n%s", wantMsgID, body)
	}
	if !strings.Contains(body, "<Id>"+wantMsgID+"</Id>") {
		t.Errorf("export missing RptgReq/Id %q, body:\n%s", wantMsgID, body)
	}

	// Re-export is identical apart from the wall-clock CreDtTm line.
	stripCreDtTm := regexp.MustCompile(`<CreDtTm>[^<]*</CreDtTm>`).ReplaceAllString(body, "<CreDtTm/>")
	_, _, body2 := getText(t, srv.URL+"/exports/camt060?"+q.Encode())
	if stripCreDtTm != regexp.MustCompile(`<CreDtTm>[^<]*</CreDtTm>`).ReplaceAllString(body2, "<CreDtTm/>") {
		t.Errorf("repeat export differs beyond CreDtTm:\nfirst:\n%s\nsecond:\n%s", body, body2)
	}

	// The metrics counter counts the export, including the rejected
	// requests above (6 rejected + 2 ok = 8).
	code, _, mbody := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics: status=%d, want 200", code)
	}
	if !strings.Contains(mbody, "ledger_camt060_exports_total 8") {
		t.Errorf("metrics missing ledger_camt060_exports_total 8")
	}
}
