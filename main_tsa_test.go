package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// newTSAHTTPServer builds a server whose ledger has an audit log (the
// durable home of tsa_anchors.jsonl) and a configured TSA URL, seeded
// with 2 journal entries. The TSA URL points at deadURL by default;
// pass a live test TSA URL for the happy path.
func newTSAHTTPServer(t *testing.T, tsaURL string) *httptest.Server {
	t.Helper()
	srv, _ := newTSAHTTPServerWithLedger(t, tsaURL)
	return srv
}

// newTSAHTTPServerWithLedger is newTSAHTTPServer plus the ledger
// handle, so tests can re-source the TSA endpoint in place
// (LG-303) and watch the HTTP path follow.
func newTSAHTTPServerWithLedger(t *testing.T, tsaURL string) (*httptest.Server, *ledger.Ledger) {
	t.Helper()
	dir := t.TempDir()
	al, err := ledger.NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	t.Cleanup(func() { al.Close() })
	l := ledger.New(ledger.WithAuditLog(al), ledger.WithTSAURL(tsaURL))
	base := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		e := ledger.JournalEntry{
			ID:             "e-tsa-http-" + string(rune('0'+i)),
			DebitAccount:   "cash",
			CreditAccount:  "equity",
			AmountCents:    100,
			IdempotencyKey: "key-tsa-http-" + string(rune('0'+i)),
			CreatedAt:      base.Add(time.Duration(i) * time.Hour),
		}
		if _, dup, err := l.Post(e); err != nil || dup {
			t.Fatalf("seed Post %d = dup=%v err=%v", i, dup, err)
		}
	}
	srv := httptest.NewServer(newRouter(l))
	t.Cleanup(srv.Close)
	return srv, l
}

// countingTSAStub is a TSA endpoint that counts requests and always
// fails: enough to prove which endpoint a call resolved to, without
// needing a real signed TimeStampResp.
type countingTSAStub struct {
	hits atomic.Int64
	srv  *httptest.Server
}

func newCountingTSAStub(t *testing.T) *countingTSAStub {
	t.Helper()
	stub := &countingTSAStub{}
	stub.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(stub.srv.Close)
	return stub
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", timeout)
}

func TestHandleTSAAnchorUnconfigured(t *testing.T) {
	// No TSA URL: the operator misconfigured the deployment → 500, and
	// the tsa section of /entries/verify reports configured:false.
	srv := newTSAHTTPServer(t, "")

	code, body := postJSON(t, srv.URL+"/entries/tsa-anchor", "")
	if code != http.StatusInternalServerError {
		t.Fatalf("tsa-anchor status = %d body=%v, want 500", code, body)
	}

	code, body = getJSON(t, srv.URL+"/entries/verify")
	if code != http.StatusOK {
		t.Fatalf("verify status = %d, want 200", code)
	}
	tsa, ok := body["tsa"].(map[string]any)
	if !ok {
		t.Fatalf("verify has no tsa section: %v", body)
	}
	if tsa["configured"] != false {
		t.Errorf("tsa.configured = %v, want false", tsa["configured"])
	}
}

func TestHandleTSAAnchorTSADown(t *testing.T) {
	// TSA URL configured but the authority is unreachable: 502, and the
	// ledger records no anchor.
	srv := newTSAHTTPServer(t, "http://127.0.0.1:1/tsa")

	code, body := postJSON(t, srv.URL+"/entries/tsa-anchor", "")
	if code != http.StatusBadGateway {
		t.Fatalf("tsa-anchor status = %d body=%v, want 502", code, body)
	}

	code, body = getJSON(t, srv.URL+"/entries/verify")
	if code != http.StatusOK {
		t.Fatalf("verify status = %d, want 200", code)
	}
	tsa := body["tsa"].(map[string]any)
	if tsa["configured"] != true {
		t.Errorf("tsa.configured = %v, want true", tsa["configured"])
	}
	if n, _ := tsa["anchors"].(float64); n != 0 {
		t.Errorf("tsa.anchors = %v, want 0 after failed anchor", n)
	}
}

func TestMetricsExposeTSASeries(t *testing.T) {
	srv := newTSAHTTPServer(t, "http://127.0.0.1:1/tsa")
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics: %v", err)
	}
	metrics := string(raw)
	for _, name := range []string{"ledger_tsa_anchors_total", "ledger_tsa_anchor_failures_total"} {
		if !strings.Contains(metrics, name) {
			t.Errorf("/metrics missing series %s", name)
		}
	}
}

// TestTSAMetricsAuthorityResolvesURLPerTick (LG-303): the worker's
// authority resolves the live endpoint on every call — successive
// Timestamp calls hit two different httptest TSAs after a
// re-sourcing, and an empty URL fails closed with
// ErrTSAURLNotConfigured instead of dialing nowhere.
func TestTSAMetricsAuthorityResolvesURLPerTick(t *testing.T) {
	stub1 := newCountingTSAStub(t)
	stub2 := newCountingTSAStub(t)
	dir := t.TempDir()
	al, err := ledger.NewAuditLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { al.Close() })
	l := ledger.New(ledger.WithAuditLog(al), ledger.WithTSAURL(stub1.srv.URL))
	m := &Metrics{}
	a := &tsaMetricsAuthority{ledger: l, metrics: m}
	var digest [32]byte
	digest[0] = 0x42
	ctx := context.Background()

	if _, err := a.Timestamp(ctx, digest); err == nil {
		t.Fatal("Timestamp against stub TSA = nil error, want a failure")
	}
	if got := stub1.hits.Load(); got != 1 {
		t.Fatalf("stub1 hits = %d, want 1", got)
	}
	if got := stub2.hits.Load(); got != 0 {
		t.Fatalf("stub2 hits = %d, want 0 before re-sourcing", got)
	}

	if err := l.SetTSAURL(stub2.srv.URL); err != nil {
		t.Fatalf("SetTSAURL = %v", err)
	}
	if _, err := a.Timestamp(ctx, digest); err == nil {
		t.Fatal("Timestamp against stub TSA = nil error, want a failure")
	}
	if got := stub1.hits.Load(); got != 1 {
		t.Fatalf("stub1 hits = %d, want 1 after re-sourcing (no frozen client)", got)
	}
	if got := stub2.hits.Load(); got != 1 {
		t.Fatalf("stub2 hits = %d, want 1 after re-sourcing", got)
	}
	if got := m.TSAAnchorFailuresTotal.Load(); got != 2 {
		t.Fatalf("TSAAnchorFailuresTotal = %d, want 2", got)
	}

	// No endpoint at all: fail closed, no dial, no panic.
	unconfigured := ledger.New()
	a2 := &tsaMetricsAuthority{ledger: unconfigured, metrics: &Metrics{}}
	if _, err := a2.Timestamp(ctx, digest); !errors.Is(err, ledger.ErrTSAURLNotConfigured) {
		t.Fatalf("Timestamp with no URL = %v, want ErrTSAURLNotConfigured", err)
	}
}

// TestTSAWorkerFollowsURLAmendment (LG-303): the background worker
// follows a runtime re-sourcing — after SetTSAURL, the next tick
// dials the new endpoint, not the startup one. The stubs always
// fail, so no anchor is ever stored and every tick calls the
// authority, which is exactly what makes the follow observable.
func TestTSAWorkerFollowsURLAmendment(t *testing.T) {
	stub1 := newCountingTSAStub(t)
	stub2 := newCountingTSAStub(t)
	dir := t.TempDir()
	al, err := ledger.NewAuditLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { al.Close() })
	l := ledger.New(ledger.WithAuditLog(al), ledger.WithTSAURL(stub1.srv.URL))
	base := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	e := ledger.JournalEntry{
		ID: "e-tsa-worker-0", DebitAccount: "cash", CreditAccount: "equity",
		AmountCents: 100, IdempotencyKey: "key-tsa-worker-0", CreatedAt: base,
	}
	if _, dup, err := l.Post(e); err != nil || dup {
		t.Fatalf("seed Post = dup=%v err=%v", dup, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := &Metrics{}
	authority := &tsaMetricsAuthority{ledger: l, metrics: m}
	anchorer := ledger.StartTimestampAnchoring(ctx, l, 20*time.Millisecond, authority, nil)
	if anchorer == nil {
		t.Fatal("StartTimestampAnchoring returned nil")
	}
	defer anchorer.Stop()

	waitFor(t, 5*time.Second, func() bool { return stub1.hits.Load() >= 1 })
	if got := stub2.hits.Load(); got != 0 {
		t.Fatalf("stub2 hits = %d, want 0 before re-sourcing", got)
	}
	if err := l.SetTSAURL(stub2.srv.URL); err != nil {
		t.Fatalf("SetTSAURL = %v", err)
	}
	waitFor(t, 5*time.Second, func() bool { return stub2.hits.Load() >= 1 })
	if got := stub1.hits.Load(); got < 1 {
		t.Fatalf("stub1 hits = %d, want >= 1", got)
	}
	if got := m.TSAAnchorFailuresTotal.Load(); got < 2 {
		t.Fatalf("TSAAnchorFailuresTotal = %d, want >= 2 (both endpoints failed)", got)
	}
}

// TestHandleTSAAnchorFollowsSetTSAURL (LG-303): the manual anchor
// path resolves the amended endpoint too — successive POSTs hit two
// different httptest TSAs after a re-sourcing.
func TestHandleTSAAnchorFollowsSetTSAURL(t *testing.T) {
	stub1 := newCountingTSAStub(t)
	stub2 := newCountingTSAStub(t)
	srv, l := newTSAHTTPServerWithLedger(t, stub1.srv.URL)

	code, _ := postJSON(t, srv.URL+"/entries/tsa-anchor", "")
	if code != http.StatusBadGateway {
		t.Fatalf("first tsa-anchor status = %d, want 502 (stub TSA fails)", code)
	}
	if got := stub1.hits.Load(); got != 1 {
		t.Fatalf("stub1 hits = %d, want 1", got)
	}

	if err := l.SetTSAURL(stub2.srv.URL); err != nil {
		t.Fatalf("SetTSAURL = %v", err)
	}
	code, _ = postJSON(t, srv.URL+"/entries/tsa-anchor", "")
	if code != http.StatusBadGateway {
		t.Fatalf("second tsa-anchor status = %d, want 502 (stub TSA fails)", code)
	}
	if got := stub1.hits.Load(); got != 1 {
		t.Fatalf("stub1 hits = %d, want 1 after re-sourcing", got)
	}
	if got := stub2.hits.Load(); got != 1 {
		t.Fatalf("stub2 hits = %d, want 1 after re-sourcing", got)
	}
}
