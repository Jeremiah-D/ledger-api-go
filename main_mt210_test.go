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

// mt210HTTPSetup registers BICs on both legs (alice DEUTDEFF, bob
// NWBKGB2L) and creates one daily schedule: 10000 cents USD, next
// run 2026-11-12 09:00 UTC, a 250-cent per-run fee on its own leg.
// The schedule ID is short enough that the run transfer ID
// ("sched-1/run/0") stays within the 16x :21: limit.
func mt210HTTPSetup(t *testing.T) (*ledger.Ledger, ledger.TransferSchedule) {
	t.Helper()
	l := ledger.New()
	if err := l.SetAccountIBAN("alice", "DE89370400440532013000", "DEUTDEFF"); err != nil {
		t.Fatalf("SetAccountIBAN alice: %v", err)
	}
	if err := l.SetAccountIBAN("bob", "GB29NWBK60161331926819", "NWBKGB2L"); err != nil {
		t.Fatalf("SetAccountIBAN bob: %v", err)
	}
	sched, _, err := l.CreateTransferSchedule(ledger.TransferSchedule{
		ID: "sched-1", From: "alice", To: "bob",
		AmountCents: 10000, Currency: "USD",
		FeeCents: 250, FeeAccount: "fee-revenue",
		Interval:  24 * time.Hour,
		NextRunAt: time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("CreateTransferSchedule: %v", err)
	}
	return l, sched
}

// Full MT210 notice flow through HTTP: create the schedule,
// export the notice, and verify the FIN body, the byte-identical
// repeat, and the metrics counter.
func TestHTTPMT210Export(t *testing.T) {
	l, sched := mt210HTTPSetup(t)
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

	code, hdr, body := getText(t, srv.URL+"/exports/mt210?"+q.Encode())
	if code != http.StatusOK {
		t.Fatalf("export: status=%d, want 200", code)
	}
	if ct := hdr.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("content-type = %q, want text/plain; charset=utf-8", ct)
	}
	for _, want := range []string{
		"{1:F01NWBKGB2LXXX0000000000}",
		"{2:I210CITIUS33XXXU3003}",
		"{3:{108:sched-1}}",
		":20:sched-1",
		":25:GB29NWBK60161331926819",
		":30:261112",
		":21:sched-1/run/0",
		":32B:USD100,00",
		":52A:DEUTDEFF",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("export missing %q, body:\n%s", want, body)
		}
	}
	for _, banned := range []string{":50D:", ":56A:", ":56D:", ":72:"} {
		if strings.Contains(body, banned) {
			t.Errorf("export must not carry %s, body:\n%s", banned, body)
		}
	}

	// Repeat export is byte-identical: every field is pinned to
	// the stored schedule and its RunSeq, no clock value leaks
	// into the document.
	_, _, body2 := getText(t, srv.URL+"/exports/mt210?"+q.Encode())
	if body2 != body {
		t.Errorf("repeat export differs:\nfirst:\n%s\nsecond:\n%s", body, body2)
	}

	// The metrics counter counts the export (1 export + 1 repeat
	// = 2).
	code, _, mbody := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics: status=%d, want 200", code)
	}
	if !strings.Contains(mbody, "ledger_mt210_exports_total 2") {
		t.Errorf("metrics missing ledger_mt210_exports_total 2")
	}
}

// Error mapping for GET /exports/mt210: a missing schedule_id or
// servicing_bic is a 400, an unknown schedule a 404, a paused
// schedule a 422, an invalid servicing BIC a 400 — and rejected
// requests still count in the metrics.
func TestHTTPMT210ExportErrors(t *testing.T) {
	l, sched := mt210HTTPSetup(t)
	if _, err := l.PauseTransferSchedule(sched.ID); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
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

	// Missing schedule_id.
	if code := getCode(t, srv.URL+"/exports/mt210?servicing_bic=CITIUS33"); code != http.StatusBadRequest {
		t.Errorf("missing schedule_id: status=%d, want 400", code)
	}
	// Missing servicing_bic.
	if code := getCode(t, srv.URL+"/exports/mt210?schedule_id="+sched.ID); code != http.StatusBadRequest {
		t.Errorf("missing servicing_bic: status=%d, want 400", code)
	}
	// Unknown schedule.
	q := url.Values{}
	q.Set("schedule_id", "sched-nope")
	q.Set("servicing_bic", "CITIUS33")
	if code := getCode(t, srv.URL+"/exports/mt210?"+q.Encode()); code != http.StatusNotFound {
		t.Errorf("unknown schedule: status=%d, want 404", code)
	}
	// Paused schedule.
	q.Set("schedule_id", sched.ID)
	if code := getCode(t, srv.URL+"/exports/mt210?"+q.Encode()); code != http.StatusUnprocessableEntity {
		t.Errorf("paused schedule: status=%d, want 422", code)
	}
	// Invalid servicing BIC.
	q.Set("servicing_bic", "CITI")
	if code := getCode(t, srv.URL+"/exports/mt210?"+q.Encode()); code != http.StatusBadRequest {
		t.Errorf("bad servicing BIC: status=%d, want 400", code)
	}

	// Rejected requests still count: 5 errors above.
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	mbody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics: %v", err)
	}
	if !strings.Contains(string(mbody), "ledger_mt210_exports_total 5") {
		t.Errorf("metrics missing ledger_mt210_exports_total 5")
	}
}
