package ledger

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Audit-sink delivery-target amendment tests (LG-352). They reuse the
// audit_sink_test.go harness: sinkCapture, sinkTestConfig,
// startSinkTestLedger (audit_sink_delivery_amend_test.go), mustPostSink,
// and waitForSink.

// sinkURLRevisedEvents polls the audit log until at least one
// sink_url_revised event is visible, then returns all of them.
func sinkURLRevisedEvents(t *testing.T, auditDir string) []AuditEvent {
	t.Helper()
	day := time.Now().UTC().Format("2006-01-02")
	var out []AuditEvent
	waitForSink(t, 5*time.Second, "url-revised audit events", func() bool {
		events, _, err := ReadAuditLog(auditDir, day)
		if err != nil {
			return false
		}
		out = nil
		for _, ev := range events {
			if ev.Op == "sink_url_revised" {
				out = append(out, ev)
			}
		}
		return len(out) > 0
	})
	return out
}

// sinkURLRevisedCount counts sink_url_revised events in today's audit
// log, for no-op assertions.
func sinkURLRevisedCount(t *testing.T, auditDir string) int {
	t.Helper()
	day := time.Now().UTC().Format("2006-01-02")
	events, _, err := ReadAuditLog(auditDir, day)
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	n := 0
	for _, ev := range events {
		if ev.Op == "sink_url_revised" {
			n++
		}
	}
	return n
}

func TestSetSinkURLRejectsInvalidURLAndKeepsLive(t *testing.T) {
	cap := &sinkCapture{}
	_, sink, auditDir := startSinkTestLedger(t, cap, sinkTestConfig(""))
	before := sink.SinkURL()

	cases := map[string]string{
		"empty":         "",
		"ftp scheme":    "ftp://example.com/hook",
		"no host":       "http://",
		"relative path": "relative/path",
		"no scheme":     "example.com/hook",
		"malformed":     "http://[::1",
		"bare https":    "https://",
	}
	for name, u := range cases {
		if _, err := sink.SetSinkURL(u); !errors.Is(err, ErrInvalidSinkURL) {
			t.Errorf("%s: err = %v, want ErrInvalidSinkURL", name, err)
		}
		if got := sink.SinkURL(); got != before {
			t.Errorf("%s: live URL moved to %q, want unchanged %q", name, got, before)
		}
	}
	time.Sleep(300 * time.Millisecond) // let any stray event flush
	if n := sinkURLRevisedCount(t, auditDir); n != 0 {
		t.Fatalf("url-revised events = %d, want 0 after the rejected revisions", n)
	}
}

func TestSetSinkURLSameValueNoOpWritesNoAudit(t *testing.T) {
	cap := &sinkCapture{}
	_, sink, auditDir := startSinkTestLedger(t, cap, sinkTestConfig(""))

	// One real revision, as a positive control: it must audit.
	cap2 := &sinkCapture{}
	srv2 := httptest.NewServer(http.HandlerFunc(cap2.handler))
	t.Cleanup(srv2.Close)
	if _, err := sink.SetSinkURL(srv2.URL); err != nil {
		t.Fatalf("SetSinkURL: %v", err)
	}
	events := sinkURLRevisedEvents(t, auditDir)
	if len(events) != 1 {
		t.Fatalf("url-revised events = %d, want 1 after the real revision", len(events))
	}

	// Same-value revisions are no-ops: no new audit.
	if _, err := sink.SetSinkURL(srv2.URL); err != nil {
		t.Fatalf("same-value SetSinkURL: %v", err)
	}
	if _, err := sink.SetSinkURLCtx(context.Background(), srv2.URL); err != nil {
		t.Fatalf("same-value SetSinkURLCtx: %v", err)
	}
	if got := sink.SinkURL(); got != srv2.URL {
		t.Fatalf("SinkURL = %q, want %q", got, srv2.URL)
	}
	time.Sleep(300 * time.Millisecond) // let any stray event flush
	if n := sinkURLRevisedCount(t, auditDir); n != 1 {
		t.Fatalf("url-revised events = %d, want still 1 after the no-op revisions", n)
	}
}

func TestSetSinkURLReturnsPreviousURL(t *testing.T) {
	cap := &sinkCapture{}
	_, sink, _ := startSinkTestLedger(t, cap, sinkTestConfig(""))
	before := sink.SinkURL()

	cap2 := &sinkCapture{}
	srv2 := httptest.NewServer(http.HandlerFunc(cap2.handler))
	t.Cleanup(srv2.Close)
	prev, err := sink.SetSinkURL(srv2.URL)
	if err != nil {
		t.Fatalf("SetSinkURL: %v", err)
	}
	if prev != before {
		t.Fatalf("previous URL = %q, want %q", prev, before)
	}
	if got := sink.SinkURL(); got != srv2.URL {
		t.Fatalf("SinkURL = %q, want %q", got, srv2.URL)
	}
}

func TestSetSinkURLMovesNextDeliveryToNewURL(t *testing.T) {
	cap := &sinkCapture{}
	l, sink, _ := startSinkTestLedger(t, cap, sinkTestConfig(""))

	mustPostSink(t, l, "amend-sink-url-1")
	waitForSink(t, 10*time.Second, "first delivery at the old URL", func() bool { return cap.count() == 1 })

	cap2 := &sinkCapture{}
	srv2 := httptest.NewServer(http.HandlerFunc(cap2.handler))
	t.Cleanup(srv2.Close)
	if _, err := sink.SetSinkURL(srv2.URL); err != nil {
		t.Fatalf("SetSinkURL: %v", err)
	}

	mustPostSink(t, l, "amend-sink-url-2")
	waitForSink(t, 10*time.Second, "second delivery at the new URL", func() bool { return cap2.count() == 1 })
	if n := cap.count(); n != 1 {
		t.Fatalf("old server got %d deliveries, want 1: the revision must not replay old events", n)
	}
	if sent, failed, dead := sink.Stats(); sent != 2 || failed != 0 || dead != 0 {
		t.Fatalf("Stats = (%d, %d, %d), want (2, 0, 0)", sent, failed, dead)
	}
}

func TestSetSinkURLInFlightKeepsOldURL(t *testing.T) {
	cap := &sinkCapture{failFirst: 5}
	cfg := sinkTestConfig("")
	// Default MaxAttempts (10) covers the 5 failures plus the success.
	l, sink, _ := startSinkTestLedger(t, cap, cfg)

	cap2 := &sinkCapture{}
	srv2 := httptest.NewServer(http.HandlerFunc(cap2.handler))
	t.Cleanup(srv2.Close)

	mustPostSink(t, l, "amend-sink-url-inflight-1")
	// Wait until the first attempt has landed, then revise mid-flight:
	// the delivery must finish under the target it started with —
	// every retry still goes to the old URL.
	waitForSink(t, 10*time.Second, "first attempt to land", func() bool { return cap.count() >= 1 })
	if _, err := sink.SetSinkURL(srv2.URL); err != nil {
		t.Fatalf("SetSinkURL mid-flight: %v", err)
	}
	// The in-flight delivery keeps retrying the old URL and succeeds
	// on its 6th attempt.
	waitForSink(t, 10*time.Second, "in-flight delivery to succeed", func() bool {
		sent, _, _ := sink.Stats()
		return sent == 1
	})
	if n := cap.count(); n != 6 {
		t.Fatalf("old server got %d attempts, want 6: the in-flight delivery kept its old target", n)
	}
	if n := cap2.count(); n != 0 {
		t.Fatalf("new server got %d attempts, want 0: no retry leaked to the new target", n)
	}

	// The next delivery reads the new URL.
	mustPostSink(t, l, "amend-sink-url-inflight-2")
	waitForSink(t, 10*time.Second, "next delivery at the new URL", func() bool { return cap2.count() == 1 })
	if sent, failed, dead := sink.Stats(); sent != 2 || failed != 5 || dead != 0 {
		t.Fatalf("Stats = (%d, %d, %d), want (2, 5, 0)", sent, failed, dead)
	}
}

func TestSetSinkURLAuditEventFields(t *testing.T) {
	cap := &sinkCapture{}
	_, sink, auditDir := startSinkTestLedger(t, cap, sinkTestConfig(""))
	before := sink.SinkURL()

	cap2 := &sinkCapture{}
	srv2 := httptest.NewServer(http.HandlerFunc(cap2.handler))
	t.Cleanup(srv2.Close)
	if _, err := sink.SetSinkURL(srv2.URL); err != nil {
		t.Fatalf("SetSinkURL: %v", err)
	}
	events := sinkURLRevisedEvents(t, auditDir)
	if len(events) != 1 {
		t.Fatalf("url-revised events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Actor != "SetSinkURL" {
		t.Fatalf("Actor = %q, want SetSinkURL", ev.Actor)
	}
	if got := ev.Details["previous_url"]; got != before {
		t.Fatalf("previous_url = %v, want %q", got, before)
	}
	if got := ev.Details["current_url"]; got != srv2.URL {
		t.Fatalf("current_url = %v, want %q", got, srv2.URL)
	}
}

func TestSetSinkURLCtxActorAttribution(t *testing.T) {
	cap := &sinkCapture{}
	_, sink, auditDir := startSinkTestLedger(t, cap, sinkTestConfig(""))

	cap2 := &sinkCapture{}
	srv2 := httptest.NewServer(http.HandlerFunc(cap2.handler))
	t.Cleanup(srv2.Close)
	cap3 := &sinkCapture{}
	srv3 := httptest.NewServer(http.HandlerFunc(cap3.handler))
	t.Cleanup(srv3.Close)

	ctx := ContextWithActor(context.Background(), "ops-alan")
	if _, err := sink.SetSinkURLCtx(ctx, srv2.URL); err != nil {
		t.Fatalf("SetSinkURLCtx: %v", err)
	}
	// A plain call keeps the entrypoint name as the actor.
	if _, err := sink.SetSinkURL(srv3.URL); err != nil {
		t.Fatalf("SetSinkURL: %v", err)
	}
	var events []AuditEvent
	waitForSink(t, 5*time.Second, "2 url-revised events", func() bool {
		day := time.Now().UTC().Format("2006-01-02")
		evs, _, err := ReadAuditLog(auditDir, day)
		if err != nil {
			return false
		}
		events = nil
		for _, ev := range evs {
			if ev.Op == "sink_url_revised" {
				events = append(events, ev)
			}
		}
		return len(events) == 2
	})
	if events[0].Actor != "ops-alan" {
		t.Fatalf("first event Actor = %q, want ops-alan", events[0].Actor)
	}
	if got := events[0].Details["current_url"]; got != srv2.URL {
		t.Fatalf("first event current_url = %v, want %q", got, srv2.URL)
	}
	if events[1].Actor != "SetSinkURL" {
		t.Fatalf("second event Actor = %q, want SetSinkURL", events[1].Actor)
	}
	if got := events[1].Details["previous_url"]; got != srv2.URL {
		t.Fatalf("second event previous_url = %v, want %q", got, srv2.URL)
	}
	if got := events[1].Details["current_url"]; got != srv3.URL {
		t.Fatalf("second event current_url = %v, want %q", got, srv3.URL)
	}
}

func TestSetSinkURLLeavesClientAndPolicyUntouched(t *testing.T) {
	cap := &sinkCapture{}
	_, sink, _ := startSinkTestLedger(t, cap, sinkTestConfig(""))
	beforePolicy := sink.DeliveryPolicy()
	beforeClient := sink.client

	cap2 := &sinkCapture{}
	srv2 := httptest.NewServer(http.HandlerFunc(cap2.handler))
	t.Cleanup(srv2.Close)
	if _, err := sink.SetSinkURL(srv2.URL); err != nil {
		t.Fatalf("SetSinkURL: %v", err)
	}
	// The URL is not part of the http.Client: no rebuild, no pool
	// churn — and the four delivery knobs stay exactly as they were.
	if sink.client != beforeClient {
		t.Fatalf("client was rebuilt by a URL revision: the URL is not client state")
	}
	if got := sink.DeliveryPolicy(); !sinkPolicyEqual(got, beforePolicy) {
		t.Fatalf("delivery policy moved to %+v, want unchanged %+v", got, beforePolicy)
	}
}
