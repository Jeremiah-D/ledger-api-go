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

// mt292HTTPSetup reuses the MT210 HTTP fixture: BICs on both legs
// (alice DEUTDEFF, bob NWBKGB2L) and one daily schedule, 10000
// cents USD, next run 2026-11-12 09:00 UTC. Callers pause the
// schedule before exporting: MT292 withdraws that notice, and only
// a schedule whose notification premise is dead exports.
func mt292HTTPSetup(t *testing.T) (*ledger.Ledger, ledger.TransferSchedule) {
	t.Helper()
	return mt210HTTPSetup(t)
}

// Full MT292 cancellation flow through HTTP: pause the schedule,
// export the cancellation request, and verify the FIN body, the
// byte-identical repeat, and the metrics counter.
func TestHTTPMT292Export(t *testing.T) {
	l, sched := mt292HTTPSetup(t)
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
	q.Set("original_date", "2026-11-10")
	q.Set("reason", "Schedule paused by operator")

	code, hdr, body := getText(t, srv.URL+"/exports/mt292?"+q.Encode())
	if code != http.StatusOK {
		t.Fatalf("export: status=%d, want 200", code)
	}
	if ct := hdr.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("content-type = %q, want text/plain; charset=utf-8", ct)
	}
	for _, want := range []string{
		"{1:F01NWBKGB2LXXX0000000000}",
		"{2:I292CITIUS33XXXU3003}",
		":21:sched-1",
		":11S:210261110",
		":79:Schedule paused by operator",
		"-}",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("export missing %q, body:\n%s", want, body)
		}
	}
	// :20: is the deterministic derivation — exactly 16 chars —
	// and :21: is the schedule ID, the original MT210's :20:.
	lines := strings.Split(body, "\n")
	var ref string
	for _, ln := range lines {
		if strings.HasPrefix(ln, ":20:") {
			ref = strings.TrimPrefix(ln, ":20:")
		}
	}
	if len(ref) != 16 || !strings.HasPrefix(ref, "MT292-") {
		t.Errorf(":20: = %q, want a 16-character MT292- reference", ref)
	}
	if !strings.Contains(body, "{3:{108:"+ref+"}}") {
		t.Errorf("block 3 does not carry {108:%s}, body:\n%s", ref, body)
	}
	for _, banned := range []string{":32B:", ":30:", ":25:", "{5:"} {
		if strings.Contains(body, banned) {
			t.Errorf("export must not carry %s, body:\n%s", banned, body)
		}
	}

	// Repeat export is byte-identical: :20: is pinned to
	// schedule+RunSeq, :11S: to the caller-supplied OriginalDate,
	// no clock value leaks into the document.
	_, _, body2 := getText(t, srv.URL+"/exports/mt292?"+q.Encode())
	if body2 != body {
		t.Errorf("repeat export differs:\nfirst:\n%s\nsecond:\n%s", body, body2)
	}

	// The metrics counter counts the export (1 export + 1 repeat
	// = 2).
	code, _, mbody := getText(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics: status=%d, want 200", code)
	}
	if !strings.Contains(mbody, "ledger_mt292_exports_total 2") {
		t.Errorf("metrics missing ledger_mt292_exports_total 2")
	}
}

// Error mapping for GET /exports/mt292: a missing schedule_id,
// servicing_bic, or original_date is a 400, an unparseable
// original_date a 400, an unknown schedule a 404, a still-active
// schedule a 422, an invalid servicing BIC a 400 — and rejected
// requests still count in the metrics.
func TestHTTPMT292ExportErrors(t *testing.T) {
	l, sched := mt292HTTPSetup(t)
	if _, err := l.PauseTransferSchedule(sched.ID); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	// A second, still-active schedule for the 422 case: the
	// paused fixture schedule exports, so it cannot carry it.
	active, _, err := l.CreateTransferSchedule(ledger.TransferSchedule{
		ID: "sched-2", From: "alice", To: "bob",
		AmountCents: 10000, Currency: "USD",
		Interval:  24 * time.Hour,
		NextRunAt: time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("CreateTransferSchedule sched-2: %v", err)
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
	base := url.Values{}
	base.Set("schedule_id", sched.ID)
	base.Set("servicing_bic", "CITIUS33")
	base.Set("original_date", "2026-11-10")

	// with returns a fresh copy of base with the given overrides
	// applied: url.Values is a map, so assigning it would alias
	// base and let one case corrupt the next.
	with := func(overrides ...[2]string) url.Values {
		q := url.Values{}
		for k, vs := range base {
			q[k] = append([]string(nil), vs...)
		}
		for _, o := range overrides {
			q.Set(o[0], o[1])
		}
		return q
	}

	// Missing schedule_id.
	q := with()
	q.Del("schedule_id")
	if code := getCode(t, srv.URL+"/exports/mt292?"+q.Encode()); code != http.StatusBadRequest {
		t.Errorf("missing schedule_id: status=%d, want 400", code)
	}
	// Missing servicing_bic.
	q = with()
	q.Del("servicing_bic")
	if code := getCode(t, srv.URL+"/exports/mt292?"+q.Encode()); code != http.StatusBadRequest {
		t.Errorf("missing servicing_bic: status=%d, want 400", code)
	}
	// Missing original_date: the ledger records no MT210 issue
	// date, so the caller must supply it.
	q = with()
	q.Del("original_date")
	if code := getCode(t, srv.URL+"/exports/mt292?"+q.Encode()); code != http.StatusBadRequest {
		t.Errorf("missing original_date: status=%d, want 400", code)
	}
	// Unparseable original_date.
	q = with([2]string{"original_date", "10 Nov 2026"})
	if code := getCode(t, srv.URL+"/exports/mt292?"+q.Encode()); code != http.StatusBadRequest {
		t.Errorf("bad original_date: status=%d, want 400", code)
	}
	// Unknown schedule.
	q = with([2]string{"schedule_id", "sched-nope"})
	if code := getCode(t, srv.URL+"/exports/mt292?"+q.Encode()); code != http.StatusNotFound {
		t.Errorf("unknown schedule: status=%d, want 404", code)
	}
	// Still-active schedule: the money is still coming.
	q = with([2]string{"schedule_id", active.ID})
	if code := getCode(t, srv.URL+"/exports/mt292?"+q.Encode()); code != http.StatusUnprocessableEntity {
		t.Errorf("active schedule: status=%d, want 422", code)
	}
	// Invalid servicing BIC.
	q = with([2]string{"servicing_bic", "CITI"})
	if code := getCode(t, srv.URL+"/exports/mt292?"+q.Encode()); code != http.StatusBadRequest {
		t.Errorf("bad servicing BIC: status=%d, want 400", code)
	}

	// Rejected requests still count: 7 errors above.
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	mbody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics: %v", err)
	}
	if !strings.Contains(string(mbody), "ledger_mt292_exports_total 7") {
		t.Errorf("metrics missing ledger_mt292_exports_total 7")
	}
}
