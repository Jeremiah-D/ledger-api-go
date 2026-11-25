package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// mt292ExportHTTPGet issues a GET /exports/mt292 with the operator
// actor header set, like the other operator actions.
func mt292ExportHTTPGet(t *testing.T, raw, actor string) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		t.Fatalf("NewRequest %s: %v", raw, err)
	}
	if actor != "" {
		req.Header.Set("x-ledger-actor", actor)
	}
	resp, err := http.DefaultClient.Do(req)
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

// Full registered MT292 export flow through HTTP: pause the
// schedule, export the cancellation request with the operator
// actor header, and verify the FIN body, the byte-identical
// registered repeat, and the metrics counter.
func TestHTTPMT292ExportRegistered(t *testing.T) {
	l, sched := mt292HTTPSetup(t)
	if _, err := l.PauseTransferSchedule(sched.ID); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	q := url.Values{}
	q.Set("schedule_id", sched.ID)
	q.Set("servicing_bic", "CITIUS33")
	q.Set("original_date", "2026-11-10")
	q.Set("reason", "Schedule paused by operator")

	code, hdr, body := mt292ExportHTTPGet(t, srv.URL+"/exports/mt292?"+q.Encode(), "ledger-operator")
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

	// Repeat export is byte-identical: the registered message is
	// returned, not a second render.
	_, _, body2 := mt292ExportHTTPGet(t, srv.URL+"/exports/mt292?"+q.Encode(), "ledger-operator")
	if body2 != body {
		t.Errorf("repeat export differs:\nfirst:\n%s\nsecond:\n%s", body, body2)
	}

	// The metrics counter counts the export (1 export + 1 repeat
	// = 2).
	code, _, mbody := mt292ExportHTTPGet(t, srv.URL+"/metrics", "")
	if code != http.StatusOK {
		t.Fatalf("metrics: status=%d, want 200", code)
	}
	if !strings.Contains(mbody, "ledger_mt292_exports_total 2") {
		t.Errorf("metrics missing ledger_mt292_exports_total 2")
	}
}

// A :20: reused for different terms is a 422 through HTTP, and
// the stored first rendering survives: a repeat with the original
// terms still returns it byte-identical. The 400/404 mapping is
// unchanged alongside.
func TestHTTPMT292ExportConflict(t *testing.T) {
	l, sched := mt292HTTPSetup(t)
	if _, err := l.PauseTransferSchedule(sched.ID); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	base := url.Values{}
	base.Set("schedule_id", sched.ID)
	base.Set("servicing_bic", "CITIUS33")
	base.Set("original_date", "2026-11-10")
	base.Set("reason", "Schedule paused by operator")

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

	code, _, first := mt292ExportHTTPGet(t, srv.URL+"/exports/mt292?"+with().Encode(), "ledger-operator")
	if code != http.StatusOK {
		t.Fatalf("first export: status=%d, want 200", code)
	}
	// Same :20:, different reason: the :20: names the first
	// export, so the reuse is a conflict.
	code, _, body := mt292ExportHTTPGet(t, srv.URL+"/exports/mt292?"+with([2]string{"reason", "Operator changed their mind"}).Encode(), "ledger-operator")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("conflicting export: status=%d, want 422, body:\n%s", code, body)
	}
	if !strings.Contains(body, "already exported with different terms") {
		t.Errorf("conflict body missing the sentinel wording, body:\n%s", body)
	}
	// The conflict overwrote nothing: the original terms still
	// return the first rendering, byte-identical.
	_, _, again := mt292ExportHTTPGet(t, srv.URL+"/exports/mt292?"+with().Encode(), "ledger-operator")
	if again != first {
		t.Errorf("repeat after conflict differs:\nfirst:\n%s\nagain:\n%s", first, again)
	}
	// 400/404 mapping unchanged: unknown schedule is a 404,
	// missing servicing_bic is a 400.
	code, _, _ = mt292ExportHTTPGet(t, srv.URL+"/exports/mt292?"+with([2]string{"schedule_id", "sched-nope"}).Encode(), "ledger-operator")
	if code != http.StatusNotFound {
		t.Errorf("unknown schedule: status=%d, want 404", code)
	}
	q := with()
	q.Del("servicing_bic")
	code, _, _ = mt292ExportHTTPGet(t, srv.URL+"/exports/mt292?"+q.Encode(), "ledger-operator")
	if code != http.StatusBadRequest {
		t.Errorf("missing servicing_bic: status=%d, want 400", code)
	}
}
