package ledger

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TSA timestamp endpoint runtime re-sourcing (LG-303): the setter's
// fail-closed gates, the no-op, re-anchoring the same head against
// the new TSA, old anchors keeping their URL and verification,
// concurrent re-sourcing against in-flight anchoring, operator
// attribution, and the audit trail's URL-only boundary.

// newTSAURLTestLedger builds a ledger with an audit log and a TSA URL
// (no trust certificate pinned unless the caller adds one), returning
// the audit log too so tests can read the audit trail.
func newTSAURLTestLedger(t *testing.T, tsaURL string) (*Ledger, string, *AuditLog) {
	t.Helper()
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { al.Close() })
	l := New(WithAuditLog(al), WithTSAURL(tsaURL))
	return l, dir, al
}

// tsaURLAmendedAuditEvents returns the tsa_url_amended events in
// order, closing the audit log first (like the other amend tests).
func tsaURLAmendedAuditEvents(t *testing.T, al *AuditLog) []AuditEvent {
	t.Helper()
	var out []AuditEvent
	for _, e := range providerAuditEvents(t, al) {
		if e.Op == "tsa_url_amended" {
			out = append(out, e)
		}
	}
	return out
}

// secondTestTSAEndpoint serves the same test TSA (same signing key
// and certificate) on a second httptest server, so a re-sourcing can
// move the endpoint while the trust root stays valid for anchors
// from both endpoints.
func secondTestTSAEndpoint(t *testing.T, ts *testTSA) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(ts.handle))
	t.Cleanup(srv.Close)
	return srv.URL
}

// readTSAAnchors reads every stored anchor from tsa_anchors.jsonl in
// order.
func readTSAAnchors(t *testing.T, dir string) []TimestampAnchor {
	t.Helper()
	fh, err := os.Open(filepath.Join(dir, tsaAnchorsFileName))
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	var out []TimestampAnchor
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 64*1024), 64*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var a TimestampAnchor
		if err := json.Unmarshal([]byte(line), &a); err != nil {
			t.Fatalf("anchor line unmarshal: %v", err)
		}
		out = append(out, a)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSetTSAURLRejectsInvalidURLs(t *testing.T) {
	ts := newTestTSA(t)
	l, _, _ := newTSAURLTestLedger(t, ts.srv.URL)
	anchorWith(t, l, ts, "tsa", 3)
	v := l.Version()

	invalid := []string{
		"",
		"   ",
		"://bad",
		"ftp://tsa.example.com/tsa",
		"gopher://tsa.example.com/tsa",
		"http://",
		"https://",
		"tsa.example.com/tsa",
		"http://exa mple.com/tsa",
		"/just/a/path",
	}
	for _, raw := range invalid {
		if err := l.SetTSAURL(raw); !errors.Is(err, ErrInvalidTSAURL) {
			t.Fatalf("SetTSAURL(%q) = %v, want ErrInvalidTSAURL", raw, err)
		}
		if err := l.SetTSAURLCtx(context.Background(), raw); !errors.Is(err, ErrInvalidTSAURL) {
			t.Fatalf("SetTSAURLCtx(%q) = %v, want ErrInvalidTSAURL", raw, err)
		}
	}
	// The live endpoint is untouched, the version unmoved, and no
	// amendment was audited.
	if got := l.TSAURL(); got != ts.srv.URL {
		t.Fatalf("TSAURL() after rejected re-sourcing = %q, want %q", got, ts.srv.URL)
	}
	if got := l.Version(); got != v {
		t.Fatalf("Version() = %d, want %d (rejected re-sourcing bumps nothing)", got, v)
	}
	ts.checkNoHandleError(t)
}

func TestSetTSAURLSameValueIsNoOp(t *testing.T) {
	ts := newTestTSA(t)
	l, _, al := newTSAURLTestLedger(t, ts.srv.URL)
	anchorWith(t, l, ts, "tsa", 3)
	v := l.Version()

	if err := l.SetTSAURL(ts.srv.URL); err != nil {
		t.Fatalf("SetTSAURL(same URL) = %v, want nil", err)
	}
	// Re-sourcing to the current URL changes nothing: same endpoint,
	// no version bump, no audit event.
	if got := l.TSAURL(); got != ts.srv.URL {
		t.Fatalf("TSAURL() = %q, want %q", got, ts.srv.URL)
	}
	if got := l.Version(); got != v {
		t.Fatalf("Version() = %d, want %d (no bump on no-op)", got, v)
	}
	if events := tsaURLAmendedAuditEvents(t, al); len(events) != 0 {
		t.Fatalf("tsa_url_amended events = %d, want 0 (no-op writes no audit)", len(events))
	}
	ts.checkNoHandleError(t)
}

func TestSetTSAURLAmendsAndReanchorsSameHead(t *testing.T) {
	ts := newTestTSA(t)
	url2 := secondTestTSAEndpoint(t, ts)
	l, dir, al := newTSAURLTestLedger(t, ts.srv.URL)
	// Pin the shared trust root so anchors from both endpoints
	// verify: the re-sourcing moves the endpoint, not the trust.
	if err := l.SetTSATrustCert(ts.cert); err != nil {
		t.Fatalf("SetTSATrustCert = %v", err)
	}
	a1 := anchorWith(t, l, ts, "tsa", 3)
	if a1.TSAURL != ts.srv.URL {
		t.Fatalf("first anchor TSAURL = %q, want %q", a1.TSAURL, ts.srv.URL)
	}
	v := l.Version()

	if err := l.SetTSAURL(url2); err != nil {
		t.Fatalf("SetTSAURL = %v", err)
	}
	if got := l.TSAURL(); got != url2 {
		t.Fatalf("TSAURL() = %q, want %q", got, url2)
	}
	if got := l.Version(); got != v {
		t.Fatalf("Version() = %d, want %d (amendment bumps no version)", got, v)
	}

	// The same head, no new entries: the idempotency key includes
	// the TSA URL, so the head is re-anchored against the new TSA
	// instead of replaying the old anchor.
	a2, err := l.AnchorTimestamp(context.Background(), &RFC3161Client{URL: url2})
	if err != nil {
		t.Fatalf("AnchorTimestamp after re-sourcing = %v", err)
	}
	if a2.Seq != a1.Seq || a2.HeadHash != a1.HeadHash {
		t.Fatalf("re-anchor = %+v, want same seq/head as %+v", a2, a1)
	}
	if a2.TSAURL != url2 {
		t.Fatalf("re-anchor TSAURL = %q, want %q", a2.TSAURL, url2)
	}

	// Both anchors are stored: the old one keeps its URL, and the
	// whole history still verifies against the live chain and the
	// trust root.
	stored := readTSAAnchors(t, dir)
	if len(stored) != 2 {
		t.Fatalf("stored anchors = %d, want 2", len(stored))
	}
	if stored[0].TSAURL != ts.srv.URL || stored[1].TSAURL != url2 {
		t.Fatalf("stored anchor URLs = %q, %q, want %q then %q",
			stored[0].TSAURL, stored[1].TSAURL, ts.srv.URL, url2)
	}
	st := l.TimestampAnchorStatus()
	if st.Anchors != 2 || st.Verified == nil || !*st.Verified || st.Error != "" {
		t.Fatalf("TimestampAnchorStatus() after re-sourcing = %+v, want 2 anchors verified", st)
	}
	if st.Latest == nil || st.Latest.TSAURL != url2 {
		t.Fatalf("status Latest = %+v, want the new-endpoint anchor", st.Latest)
	}
	rep := l.Reconcile(time.Now())
	if !rep.AuditChain.TSAAnchorOK || rep.AuditChain.TSAAnchorError != "" {
		t.Fatalf("Reconcile TSA anchor health = %+v, want ok", rep.AuditChain)
	}
	// The offline verifier accepts the journal too: two anchors, one
	// head, two endpoints.
	if n, err := VerifyTimestampAnchors(dir, ts.cert); err != nil || n != 2 {
		t.Fatalf("VerifyTimestampAnchors = %d, %v, want 2, nil", n, err)
	}

	// One amendment event, carrying the two URLs and nothing else.
	events := tsaURLAmendedAuditEvents(t, al)
	if len(events) != 1 {
		t.Fatalf("tsa_url_amended events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Details["previous_url"] != ts.srv.URL || ev.Details["new_url"] != url2 {
		t.Fatalf("audit urls = %v, want previous %q new %q", ev.Details, ts.srv.URL, url2)
	}
	if len(ev.Details) != 2 {
		t.Fatalf("audit details = %v, want exactly previous_url/new_url", ev.Details)
	}
	ts.checkNoHandleError(t)
}

func TestSetTSAURLConcurrentAnchor(t *testing.T) {
	ts := newTestTSA(t)
	url2 := secondTestTSAEndpoint(t, ts)
	l, dir, _ := newTSAURLTestLedger(t, ts.srv.URL)
	if err := l.SetTSATrustCert(ts.cert); err != nil {
		t.Fatalf("SetTSATrustCert = %v", err)
	}
	postAnchorEntries(t, l, "tsa", 4)
	if _, err := l.AnchorTimestamp(context.Background(), &RFC3161Client{URL: ts.srv.URL}); err != nil {
		t.Fatalf("AnchorTimestamp = %v", err)
	}
	allowed := map[string]bool{ts.srv.URL: true, url2: true}

	var wg sync.WaitGroup
	errs := make(chan error, 18)
	amend := func(url string) {
		defer wg.Done()
		if err := l.SetTSAURL(url); err != nil {
			errs <- err
		}
	}
	anchor := func() {
		defer wg.Done()
		// Resolve the endpoint the way the manual path does, then
		// anchor against it: an anchoring in flight when the
		// setter runs records the URL it actually used.
		if _, err := l.AnchorTimestamp(context.Background(), &RFC3161Client{URL: l.TSAURL()}); err != nil {
			errs <- err
		}
	}
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go anchor()
		wg.Add(1)
		go amend(url2)
		wg.Add(1)
		go amend(ts.srv.URL)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent AnchorTimestamp/SetTSAURL = %v", err)
	}
	// Every stored anchor names an endpoint that was live at some
	// point (never a torn mix), the live endpoint is one of the two,
	// and the whole history still verifies.
	for _, a := range readTSAAnchors(t, dir) {
		if !allowed[a.TSAURL] {
			t.Fatalf("stored anchor TSAURL = %q, want one of the two endpoints", a.TSAURL)
		}
	}
	if got := l.TSAURL(); !allowed[got] {
		t.Fatalf("TSAURL() = %q, want one of the two endpoints", got)
	}
	st := l.TimestampAnchorStatus()
	if st.Verified == nil || !*st.Verified || st.Error != "" {
		t.Fatalf("TimestampAnchorStatus() after concurrent re-sourcing = %+v, want verified", st)
	}
	ts.checkNoHandleError(t)
}

func TestSetTSAURLCtxOperatorAttribution(t *testing.T) {
	ts := newTestTSA(t)
	url2 := secondTestTSAEndpoint(t, ts)
	l, _, al := newTSAURLTestLedger(t, ts.srv.URL)
	anchorWith(t, l, ts, "tsa", 3)

	ctx := ContextWithActor(context.Background(), "op-tsa-url")
	if err := l.SetTSAURLCtx(ctx, url2); err != nil {
		t.Fatalf("SetTSAURLCtx = %v", err)
	}
	events := tsaURLAmendedAuditEvents(t, al)
	if len(events) != 1 {
		t.Fatalf("tsa_url_amended events = %d, want 1", len(events))
	}
	if events[0].Actor != "op-tsa-url" {
		t.Fatalf("audit Actor = %q, want %q", events[0].Actor, "op-tsa-url")
	}
	ts.checkNoHandleError(t)
}

func TestSetTSAURLAuditDetailsOnlyURL(t *testing.T) {
	ts := newTestTSA(t)
	url2 := secondTestTSAEndpoint(t, ts)
	l, _, al := newTSAURLTestLedger(t, ts.srv.URL)
	anchorWith(t, l, ts, "tsa", 3)

	if err := l.SetTSAURL(url2); err != nil {
		t.Fatalf("SetTSAURL = %v", err)
	}
	events := tsaURLAmendedAuditEvents(t, al)
	if len(events) != 1 {
		t.Fatalf("tsa_url_amended events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Op != "tsa_url_amended" || ev.Actor != "SetTSAURL" {
		t.Fatalf("audit op/actor = %q/%q, want tsa_url_amended/SetTSAURL", ev.Op, ev.Actor)
	}
	detailsJSON, err := json.Marshal(ev.Details)
	if err != nil {
		t.Fatal(err)
	}
	// URLs only: no token material may ever reach the audit trail.
	if strings.Contains(string(detailsJSON), "token") {
		t.Fatalf("audit details mention a token: %s", detailsJSON)
	}
	if strings.Contains(string(detailsJSON), "MIIN") || strings.Contains(string(detailsJSON), "BEGIN") {
		t.Fatalf("audit details contain token material: %s", detailsJSON)
	}
	ts.checkNoHandleError(t)
}

func TestSetTSAURLOnUnconfiguredLedgerEnablesManualAnchor(t *testing.T) {
	ts := newTestTSA(t)
	l, _, _ := newTSAURLTestLedger(t, "")
	if got := l.TSAURL(); got != "" {
		t.Fatalf("TSAURL() = %q, want empty on an unconfigured ledger", got)
	}
	// No worker exists for this ledger (cold start is out of scope),
	// but re-sourcing installs the endpoint and the manual path works
	// against it immediately.
	if err := l.SetTSAURL(ts.srv.URL); err != nil {
		t.Fatalf("SetTSAURL = %v", err)
	}
	if got := l.TSAURL(); got != ts.srv.URL {
		t.Fatalf("TSAURL() = %q, want %q", got, ts.srv.URL)
	}
	a := anchorWith(t, l, ts, "tsa", 3)
	if a.TSAURL != ts.srv.URL {
		t.Fatalf("manual anchor TSAURL = %q, want %q", a.TSAURL, ts.srv.URL)
	}
	ts.checkNoHandleError(t)
}

// TestVerifyTimestampAnchorsRejectsDuplicatedSeqLine (LG-303): the
// relaxed sequence rule admits a genuine re-anchor (same head,
// different TSA, different token) but still rejects a duplicated
// journal line — exact copy or copy with an edited URL — and a
// sequence that goes backwards.
func TestVerifyTimestampAnchorsRejectsDuplicatedSeqLine(t *testing.T) {
	ts := newTestTSA(t)
	l, dir, _ := newTSAURLTestLedger(t, ts.srv.URL)
	anchorWith(t, l, ts, "tsa", 3)
	path := filepath.Join(dir, tsaAnchorsFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 journal line, got %d", len(lines))
	}
	var a TimestampAnchor
	if err := json.Unmarshal([]byte(lines[0]), &a); err != nil {
		t.Fatal(err)
	}

	// Exact duplicate line: same seq, same head, same URL, same
	// token — a replay, not a re-anchor.
	dup := lines[0]
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"+dup+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyTimestampAnchors(dir, ts.cert); err == nil {
		t.Fatal("exact duplicate journal line accepted")
	} else if !strings.Contains(err.Error(), "repeats without a new TSA endpoint") {
		t.Fatalf("duplicate-line error = %v, want the re-anchor rejection", err)
	}

	// Same token, edited URL: still a replay — a different TSA
	// would have produced a different token.
	a.TSAURL = "https://other-tsa.example.com/tsa"
	edited, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(lines[0]+"\n"+string(edited)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyTimestampAnchors(dir, ts.cert); err == nil {
		t.Fatal("duplicate journal line with edited URL accepted")
	} else if !strings.Contains(err.Error(), "repeats without a new TSA endpoint") {
		t.Fatalf("edited-URL error = %v, want the re-anchor rejection", err)
	}

	// A sequence that goes backwards still fails with the original
	// error. Restore the clean single-line journal first so the
	// failure can only come from the tampered line.
	a.TSAURL = ts.srv.URL
	a.Seq = 0
	backwards, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(lines[0]+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A second, higher-seq anchor first so the tampered line goes
	// backwards relative to it.
	postAnchorEntries(t, l, "tsa2", 1)
	if _, err := l.AnchorTimestamp(context.Background(), &RFC3161Client{URL: ts.srv.URL}); err != nil {
		t.Fatalf("second AnchorTimestamp = %v", err)
	}
	raw, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines = strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 journal lines, got %d", len(lines))
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"+string(backwards)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyTimestampAnchors(dir, ts.cert); err == nil {
		t.Fatal("backwards sequence accepted")
	} else if !strings.Contains(err.Error(), "not strictly increasing") {
		t.Fatalf("backwards-seq error = %v, want the strictly-increasing rejection", err)
	}
	ts.checkNoHandleError(t)
}
