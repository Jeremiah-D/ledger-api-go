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

// camt058HTTPSetup reuses the camt.057 HTTP fixture: BICs on both
// legs (alice DEUTDEFF, bob NWBKGB2L) and one daily schedule,
// 10000 cents USD, next run 2026-11-12 09:00 UTC. Callers pause
// the schedule before exporting: camt.058 withdraws that notice,
// and only a schedule whose notification premise is dead exports.
func camt058HTTPSetup(t *testing.T) (*ledger.Ledger, ledger.TransferSchedule) {
	t.Helper()
	return camt057HTTPSetup(t)
}

// Note on repeat exports: GrpHdr/CreDtTm is a second-resolution wall
// clock, so two exports a second apart differ in that one line
// only — the document is byte-identical modulo CreDtTm (the
// camt.060 HTTP-test precedent, which strips CreDtTm and pins
// MsgId). Falsifying CreDtTm to force byte-identity would mislabel
// when the document was created.
var camt058CreDtTm = regexp.MustCompile(`<CreDtTm>[^<]*</CreDtTm>`)

// Full camt.058 cancellation advice flow through HTTP: pause the
// schedule, export the advice, and verify the XML body, the repeat
// export, and the metrics counter.
func TestHTTPCAMT058Export(t *testing.T) {
	l, sched := camt058HTTPSetup(t)
	if _, err := l.PauseTransferSchedule(sched.ID); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
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
	q.Set("schedule_id", sched.ID)
	q.Set("servicing_bic", "CITIUS33")
	q.Set("reason", "operator paused the plan")

	code, hdr, body := getText(t, srv.URL+"/exports/camt058?"+q.Encode())
	if code != http.StatusOK {
		t.Fatalf("export: status=%d, want 200", code)
	}
	if ct := hdr.Get("Content-Type"); ct != "application/xml; charset=utf-8" {
		t.Errorf("content-type = %q, want application/xml; charset=utf-8", ct)
	}
	for _, want := range []string{
		`xmlns="urn:iso:std:iso:20022:tech:xsd:camt.058.001.08"`,
		"<NtfctnToRcvCxlAdvc>",
		"<MsgId>camt058-sched-1-0</MsgId>",
		"<OrgnlMsgId>camt057-sched-1-0</OrgnlMsgId>",
		"<OrgnlNtfctnId>camt057-sched-1-0</OrgnlNtfctnId>",
		"<Prtry>operator paused the plan</Prtry>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("export missing %q, body:\n%s", want, body)
		}
	}
	// No amount is restated, and no servicing BIC is embedded in
	// the body: the BIC names the recipient, not a body element.
	for _, banned := range []string{"<TtlAmt>", "<Amt>", "<BICFI>"} {
		if strings.Contains(body, banned) {
			t.Errorf("export must not carry %s, body:\n%s", banned, body)
		}
	}

	// Repeat export is identical apart from the wall-clock CreDtTm line.
	_, _, body2 := getText(t, srv.URL+"/exports/camt058?"+q.Encode())
	if camt058CreDtTm.ReplaceAllString(body, "<CreDtTm/>") != camt058CreDtTm.ReplaceAllString(body2, "<CreDtTm/>") {
		t.Errorf("repeat export differs beyond CreDtTm:\nfirst:\n%s\nsecond:\n%s", body, body2)
	}

	// The metrics counter counts the export (1 export + 1 repeat
	// = 2).
	code, _, mbody := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics: status=%d, want 200", code)
	}
	if !strings.Contains(mbody, "ledger_camt058_exports_total 2") {
		t.Errorf("metrics missing ledger_camt058_exports_total 2")
	}
}

// Error mapping for GET /exports/camt058: a missing schedule_id or
// servicing_bic is a 400, an unknown schedule a 404, a still-active
// schedule a 422, an invalid servicing BIC a 400 — and rejected
// requests still count in the metrics. A paused schedule exports
// with 200, the deliberate opposite of camt.057's active-only gate.
func TestHTTPCAMT058ExportErrors(t *testing.T) {
	l, sched := camt058HTTPSetup(t)
	if _, err := l.PauseTransferSchedule(sched.ID); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	if _, _, err := l.CreateTransferSchedule(ledger.TransferSchedule{
		ID: "sched-fx", From: "alice", To: "bob",
		AmountCents: 10000, Currency: "USD", ToCurrency: "EUR",
		FXAccount: "desk-fx", Interval: 24 * time.Hour,
		NextRunAt: time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("CreateTransferSchedule FX: %v", err)
	}
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	getCode := func(t *testing.T, raw string) int {
		t.Helper()
		resp, err := http.Get(raw)
		if err != nil {
			t.Fatalf("GET %s: %v", raw, err)
		}
		defer resp.Body.Close()
		io.ReadAll(resp.Body)
		return resp.StatusCode
	}
	getBody := func(t *testing.T, raw string) (int, string) {
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
		return resp.StatusCode, string(body)
	}

	// Missing schedule_id.
	if code := getCode(t, srv.URL+"/exports/camt058?servicing_bic=CITIUS33"); code != http.StatusBadRequest {
		t.Errorf("missing schedule_id: status=%d, want 400", code)
	}
	// Missing servicing_bic.
	if code := getCode(t, srv.URL+"/exports/camt058?schedule_id="+sched.ID); code != http.StatusBadRequest {
		t.Errorf("missing servicing_bic: status=%d, want 400", code)
	}
	// Unknown schedule.
	q := url.Values{}
	q.Set("schedule_id", "sched-nope")
	q.Set("servicing_bic", "CITIUS33")
	if code := getCode(t, srv.URL+"/exports/camt058?"+q.Encode()); code != http.StatusNotFound {
		t.Errorf("unknown schedule: status=%d, want 404", code)
	}
	// Still-active schedule (the FX schedule is active: the
	// status gate refuses before any FX consideration).
	q.Set("schedule_id", "sched-fx")
	if code := getCode(t, srv.URL+"/exports/camt058?"+q.Encode()); code != http.StatusUnprocessableEntity {
		t.Errorf("active schedule: status=%d, want 422", code)
	}
	// Invalid servicing BIC.
	q.Set("schedule_id", sched.ID)
	q.Set("servicing_bic", "CITI")
	if code := getCode(t, srv.URL+"/exports/camt058?"+q.Encode()); code != http.StatusBadRequest {
		t.Errorf("bad servicing BIC: status=%d, want 400", code)
	}
	// Paused schedule exports with 200 and no CxlRsn when no
	// reason is given.
	q.Set("servicing_bic", "CITIUS33")
	code, body := getBody(t, srv.URL+"/exports/camt058?"+q.Encode())
	if code != http.StatusOK {
		t.Errorf("paused schedule: status=%d, want 200", code)
	}
	if strings.Contains(body, "<CxlRsn>") {
		t.Errorf("no-reason export must omit CxlRsn, body:\n%s", body)
	}

	// Rejected requests still count: 5 errors above + 1 success.
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	mbody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics: %v", err)
	}
	if !strings.Contains(string(mbody), "ledger_camt058_exports_total 6") {
		t.Errorf("metrics missing ledger_camt058_exports_total 6")
	}
}
