package main

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Full pacs.007 flow through HTTP: draft a payout, exercise the
// error mapping on the unsettled states, settle it, and export the
// FI-to-FI payment reversal.
func TestHTTPPacs007Export(t *testing.T) {
	srv := gpiTestServer(t)
	defer srv.Close()

	getXML := func(t *testing.T, url string) (int, string, string) {
		t.Helper()
		resp, err := http.Get(url)
		if err != nil {
			t.Fatalf("GET %s: %v", url, err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read %s: %v", url, err)
		}
		return resp.StatusCode, string(body), resp.Header.Get("Content-Type")
	}

	// Missing uetr: 400.
	if code, _, _ := getXML(t, srv.URL+"/exports/pacs007?reason=DUPL"); code != http.StatusBadRequest {
		t.Errorf("missing uetr: status=%d, want 400", code)
	}
	// Missing reason: 400.
	if code, _, _ := getXML(t, srv.URL+"/exports/pacs007?uetr=whatever"); code != http.StatusBadRequest {
		t.Errorf("missing reason: status=%d, want 400", code)
	}
	// Unknown UETR: 404.
	if code, _, _ := getXML(t, srv.URL+"/exports/pacs007?uetr=no-such-uetr&reason=DUPL"); code != http.StatusNotFound {
		t.Errorf("unknown UETR: status=%d, want 404", code)
	}
	// Reason outside the admitted vocabulary: 400 (the ledger
	// validates the reason before looking the UETR up).
	if code, _, _ := getXML(t, srv.URL+"/exports/pacs007?uetr=no-such-uetr&reason=AC03"); code != http.StatusBadRequest {
		t.Errorf("bad reason: status=%d, want 400", code)
	}

	// Draft a payout: its UETR is tracked from creation.
	uetr := draftPayoutHTTP(t, srv.URL, "po-pacs007-http-1")

	// Draft payout: 422 — only settled money can be reversed.
	if code, _, _ := getXML(t, srv.URL+"/exports/pacs007?uetr="+uetr+"&reason=DUPL"); code != http.StatusUnprocessableEntity {
		t.Errorf("draft payout: status=%d, want 422", code)
	}

	// Approve, then submit: still unsettled, still 422.
	code, _ := postJSON(t, srv.URL+"/payouts/po-pacs007-http-1/approve", `{"approver":"alice"}`)
	if code != http.StatusOK {
		t.Fatalf("approve: status=%d, want 200", code)
	}
	code, _ = postJSON(t, srv.URL+"/payouts/po-pacs007-http-1/submit", "")
	if code != http.StatusOK {
		t.Fatalf("submit: status=%d, want 200", code)
	}
	if code, _, _ := getXML(t, srv.URL+"/exports/pacs007?uetr="+uetr+"&reason=DUPL"); code != http.StatusUnprocessableEntity {
		t.Errorf("submitted payout: status=%d, want 422", code)
	}

	// Settle against a matching bank statement line.
	code, body := postJSON(t, srv.URL+"/payouts/po-pacs007-http-1/settle",
		fmt.Sprintf(`{"statement":{"id":"stmt-p7-1","account":"saver","currency":"USD","direction":"out",`+
			`"amount_cents":250000,"posted_at":%q}}`, time.Now().UTC().Format(time.RFC3339)))
	if code != http.StatusOK || body["status"] != "settled" {
		t.Fatalf("settle = status %d body %v, want 200 settled", code, body)
	}

	// Settled payout: 200, the reversal document.
	code, doc, ctype := getXML(t, srv.URL+"/exports/pacs007?uetr="+uetr+"&reason=FRAD")
	if code != http.StatusOK {
		t.Fatalf("export: status=%d, want 200", code)
	}
	if ctype != "application/xml; charset=utf-8" {
		t.Errorf("Content-Type = %q, want application/xml; charset=utf-8", ctype)
	}
	for _, want := range []string{
		"pacs.007.001.02",
		"<FIToFIPmtRvsl>",
		"<MsgId>" + uetr + "/rvsl</MsgId>",
		"<RvslId>" + uetr + "/rvsl</RvslId>",
		"<OrgnlUETR>" + uetr + "</OrgnlUETR>",
		"<Prtry>FRAD</Prtry>",
		`<RvsdIntrBkSttlmAmt Ccy="USD">2500.00</RvsdIntrBkSttlmAmt>`,
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("export missing %q", want)
		}
	}

	// Repeat export: the MsgId is deterministic, and the document is
	// byte-identical apart from the wall-clock CreDtTm (second
	// resolution) — the same contract the ledger-level test asserts.
	_, second, _ := getXML(t, srv.URL+"/exports/pacs007?uetr="+uetr+"&reason=FRAD")
	stripCreDtTm := func(s string) string {
		var kept []string
		for _, line := range strings.Split(s, "\n") {
			if strings.Contains(line, "<CreDtTm>") {
				continue
			}
			kept = append(kept, line)
		}
		return strings.Join(kept, "\n")
	}
	if stripCreDtTm(doc) != stripCreDtTm(second) {
		t.Error("repeat export differs beyond CreDtTm")
	}
	if !strings.Contains(second, "<MsgId>"+uetr+"/rvsl</MsgId>") {
		t.Error("repeat export carries a different MsgId")
	}

	// The export changed nothing: the payout is still settled.
	code, payout := getJSON(t, srv.URL+"/payouts/po-pacs007-http-1")
	if code != http.StatusOK || payout["status"] != "settled" {
		t.Errorf("payout after export = status %d body %v, want 200 settled", code, payout)
	}
}
