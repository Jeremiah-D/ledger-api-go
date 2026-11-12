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

// Full pain.007 customer payment cancellation request flow through
// HTTP: seed transfers with registered IBANs, cancel one, and verify
// the XML body, the error mapping, the recovered original MsgId, and
// the metrics counter.
//
// Note on repeat exports: GrpHdr/CreDtTm is a second-resolution wall
// clock (ledger.RenderPain007Request), so two exports a second apart
// differ in that one line — the test strips it before comparing, and
// pins the MsgId as the stable identifier instead (the camt.053
// precedent).
func TestHTTPPain007Export(t *testing.T) {
	l := ledger.New()
	base := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	if err := l.SetAccountIBAN("alice", "DE89370400440532013000", "DEUTDEFF"); err != nil {
		t.Fatalf("SetAccountIBAN alice: %v", err)
	}
	if err := l.SetAccountIBAN("bob", "GB29NWBK60161331926819", ""); err != nil {
		t.Fatalf("SetAccountIBAN bob: %v", err)
	}
	post := func(id, from, to string, cents int64, at time.Time) {
		t.Helper()
		if _, err := l.PostTransfer(ledger.Transfer{ID: id, From: ledger.AccountID(from), To: ledger.AccountID(to), AmountCents: cents, Currency: "USD", CreatedAt: at}); err != nil {
			t.Fatalf("PostTransfer(%s): %v", id, err)
		}
	}
	post("pay-1", "alice", "bob", 15000, base)
	post("rev-1", "alice", "bob", 25000, base.Add(time.Hour))
	if _, _, err := l.ReverseEntry("rev-1", "rev-key-1"); err != nil {
		t.Fatalf("ReverseEntry(rev-1): %v", err)
	}
	// A pending-review transfer: the review threshold freezes it.
	if err := l.SetReviewThreshold("alice", 100000); err != nil {
		t.Fatalf("SetReviewThreshold: %v", err)
	}
	post("frozen-1", "alice", "bob", 200000, base.Add(3*time.Hour))

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

	// Missing transfer_id: 400.
	if code, _, _ := getText(t, srv.URL+"/exports/pain007"); code != http.StatusBadRequest {
		t.Errorf("missing transfer_id: status=%d, want 400", code)
	}
	// Unknown transfer: 404.
	if code, _, _ := getText(t, srv.URL+"/exports/pain007?transfer_id=no-such"); code != http.StatusNotFound {
		t.Errorf("unknown transfer: status=%d, want 404", code)
	}
	// Invalid reason: 400.
	badReason := url.Values{}
	badReason.Set("transfer_id", "pay-1")
	badReason.Set("reason", "NOPE")
	if code, _, _ := getText(t, srv.URL+"/exports/pain007?"+badReason.Encode()); code != http.StatusBadRequest {
		t.Errorf("bad reason: status=%d, want 400", code)
	}
	// Pending-review transfer: 422.
	if code, _, _ := getText(t, srv.URL+"/exports/pain007?transfer_id=frozen-1"); code != http.StatusUnprocessableEntity {
		t.Errorf("pending-review transfer: status=%d, want 422", code)
	}
	// Already-reversed transfer: 409.
	if code, _, _ := getText(t, srv.URL+"/exports/pain007?transfer_id=rev-1"); code != http.StatusConflict {
		t.Errorf("already-reversed transfer: status=%d, want 409", code)
	}

	// Export the original pain.001 instruction first, so the
	// cancellation can recover its MsgId from the registry. The
	// export covers pay-1 and rev-1 (frozen-1 is pending-review and
	// excluded); its deterministic MsgId is
	// pain001-0-0-ALL-ALL-USD-pay-1-rev-1.
	if code, _, _ := getText(t, srv.URL+"/exports/pain001?currency=USD"); code != http.StatusOK {
		t.Fatalf("pain001 export: status=%d, want 200", code)
	}

	// A transfer with no IBANs on either side, posted after the
	// pain.001 export so it cannot fail-closed that export.
	post("noiban-1", "dave", "erin", 1000, base.Add(2*time.Hour))
	// No IBANs: 422, fail-closed.
	if code, _, _ := getText(t, srv.URL+"/exports/pain007?transfer_id=noiban-1"); code != http.StatusUnprocessableEntity {
		t.Errorf("no IBAN: status=%d, want 422", code)
	}

	q := url.Values{}
	q.Set("transfer_id", "pay-1")
	q.Set("reason", "CUST")
	code, hdr, body := getText(t, srv.URL+"/exports/pain007?"+q.Encode())
	if code != http.StatusOK {
		t.Fatalf("export: status=%d, want 200", code)
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "application/xml") {
		t.Errorf("content-type = %q, want application/xml", ct)
	}
	for _, want := range []string{
		`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pain.007.001.02">`,
		"<CstmrPmtCxlReq>",
		"<MsgId>pay-1/cxl</MsgId>",
		"<OrgnlEndToEndId>pay-1</OrgnlEndToEndId>",
		`<InstdAmt Ccy="USD">150.00</InstdAmt>`,
		"<IBAN>DE89370400440532013000</IBAN>",
		"<IBAN>GB29NWBK60161331926819</IBAN>",
		"<Prtry>CUST</Prtry>",
		"<OrgnlMsgNmId>pain.001.001.03</OrgnlMsgNmId>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("export missing %q, body:\n%s", want, body)
		}
	}
	// The original MsgId was recovered from the pain.001 registry:
	// the deterministic pain.001 MsgId for this window and content.
	if !strings.Contains(body, "<OrgnlMsgId>pain001-0-0-ALL-ALL-USD-pay-1-rev-1</OrgnlMsgId>") {
		t.Errorf("export missing recovered OrgnlMsgId, body:\n%s", body)
	}
	if !strings.Contains(body, "<OrgnlPmtInfId>pain001-0-0-ALL-ALL-USD-pay-1-rev-1-alice-USD</OrgnlPmtInfId>") {
		t.Errorf("export missing derived OrgnlPmtInfId, body:\n%s", body)
	}

	// Re-export is identical apart from the wall-clock CreDtTm line.
	stripCreDtTm := regexp.MustCompile(`<CreDtTm>[^<]*</CreDtTm>`).ReplaceAllString(body, "<CreDtTm/>")
	_, _, body2 := getText(t, srv.URL+"/exports/pain007?"+q.Encode())
	if stripCreDtTm != regexp.MustCompile(`<CreDtTm>[^<]*</CreDtTm>`).ReplaceAllString(body2, "<CreDtTm/>") {
		t.Errorf("repeat export differs beyond CreDtTm:\nfirst:\n%s\nsecond:\n%s", body, body2)
	}

	// The metrics counter counts the export, including the rejected
	// requests above (6 rejected + 2 ok = 8).
	code, _, mbody := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics: status=%d, want 200", code)
	}
	if !strings.Contains(mbody, "ledger_pain007_exports_total 8") {
		t.Errorf("metrics missing ledger_pain007_exports_total 8")
	}
}
