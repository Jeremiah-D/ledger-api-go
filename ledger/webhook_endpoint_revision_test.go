package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Outbound webhook endpoint revision tests (LG-353): ReviseWebhookEndpoint
// revises one registered endpoint's delivery target URL and event
// subscription in place — no Unregister/Register teardown, no delivery
// window — with a delivery already in flight keeping the target it
// snapshotted at entry and an event already queued keeping the
// subscription it was matched under.

func ptrStringForTest(v string) *string { return &v }

func ptrEventsForTest(v []string) *[]string { return &v }

// auditEventsStringList reads one audit detail as a list of strings:
// details round-trip through JSON, so a []string comes back as []any
// of strings and a nil list comes back as a JSON null.
func auditEventsStringList(t *testing.T, ev AuditEvent, key string) []string {
	t.Helper()
	v, ok := ev.Details[key]
	if !ok {
		t.Fatalf("audit event %q has no detail %q (details: %v)", ev.Op, key, ev.Details)
	}
	if v == nil {
		return nil
	}
	raw, ok := v.([]any)
	if !ok {
		t.Fatalf("audit detail %q = %T %v, want a JSON list", key, v, v)
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		s, ok := item.(string)
		if !ok {
			t.Fatalf("audit detail %q item = %T %v, want a string", key, item, item)
		}
		out = append(out, s)
	}
	return out
}

func auditEventsStringListEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// notifyWebhookEvent fans one fabricated outbox event at the
// dispatcher directly, bypassing the ledger posting path: the
// dispatcher only needs the seq and the type to match and deliver.
func notifyWebhookEvent(d *WebhookDispatcher, seq uint64, typ string) {
	d.Notify(OutboxEvent{Seq: seq, Type: typ, At: time.Now().UTC()})
}

// reviseWebhookTestDispatcher builds a dispatcher with one endpoint
// ("risk") against the stub, returning the dispatcher and the capture.
func reviseWebhookTestDispatcher(t *testing.T, cap *webhookCapture, events []string) (*WebhookDispatcher, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(cap.handler())
	t.Cleanup(srv.Close)
	cfg := webhookTestEndpoint(t, "risk", srv.URL, mustRotatingKey(t, "primary", "s3cr3t"))
	cfg.Events = events
	d, err := NewWebhookDispatcher(cfg)
	if err != nil {
		t.Fatalf("NewWebhookDispatcher: %v", err)
	}
	t.Cleanup(func() { d.Stop() })
	return d, srv
}

func TestReviseWebhookEndpointRejectsInvalidURLAndKeepsLive(t *testing.T) {
	cap := &webhookCapture{behavior: func(int) int { return 200 }}
	d, srv := reviseWebhookTestDispatcher(t, cap, []string{"transfer_posted"})
	l := webhookTestLedger(t, d)

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
		// An illegal URL must not install anything — not even the
		// valid Events half of the same patch.
		patch := WebhookEndpointRevision{
			URL:    ptrStringForTest(u),
			Events: ptrEventsForTest([]string{"review_rejected"}),
		}
		if err := l.ReviseWebhookEndpoint("risk", patch); !errors.Is(err, ErrInvalidWebhookEndpointRevision) {
			t.Errorf("%s: err = %v, want ErrInvalidWebhookEndpointRevision", name, err)
		}
		cfg, ok := l.WebhookEndpoint("risk")
		if !ok {
			t.Fatalf("%s: endpoint vanished", name)
		}
		if cfg.URL != srv.URL {
			t.Errorf("%s: live URL moved to %q, want unchanged %q", name, cfg.URL, srv.URL)
		}
		if !auditEventsStringListEqual(cfg.Events, []string{"transfer_posted"}) {
			t.Errorf("%s: live Events moved to %v, want unchanged [transfer_posted]", name, cfg.Events)
		}
	}

	// Unknown endpoint, at both layers.
	if _, err := d.ReviseWebhookEndpoint("ghost", WebhookEndpointRevision{URL: ptrStringForTest("https://ghost.example.com/hook")}); !errors.Is(err, ErrWebhookEndpointNotFound) {
		t.Errorf("dispatcher unknown endpoint: err = %v, want ErrWebhookEndpointNotFound", err)
	}
	if err := l.ReviseWebhookEndpoint("ghost", WebhookEndpointRevision{URL: ptrStringForTest("https://ghost.example.com/hook")}); !errors.Is(err, ErrWebhookEndpointNotFound) {
		t.Errorf("ledger unknown endpoint: err = %v, want ErrWebhookEndpointNotFound", err)
	}

	// No dispatcher attached.
	bare := New()
	if err := bare.ReviseWebhookEndpoint("risk", WebhookEndpointRevision{URL: ptrStringForTest("https://example.com/hook")}); !errors.Is(err, ErrWebhooksNotConfigured) {
		t.Errorf("no dispatcher: err = %v, want ErrWebhooksNotConfigured", err)
	}
	if _, ok := bare.WebhookEndpoint("risk"); ok {
		t.Errorf("no dispatcher: WebhookEndpoint returned a config, want none")
	}
}

func TestReviseWebhookEndpointReturnsPreviousConfig(t *testing.T) {
	cap := &webhookCapture{behavior: func(int) int { return 200 }}
	d, srv := reviseWebhookTestDispatcher(t, cap, []string{"transfer_posted"})

	cap2 := &webhookCapture{behavior: func(int) int { return 200 }}
	srv2 := httptest.NewServer(cap2.handler())
	t.Cleanup(srv2.Close)
	prev, err := d.ReviseWebhookEndpoint("risk", WebhookEndpointRevision{
		URL:    ptrStringForTest(srv2.URL),
		Events: ptrEventsForTest([]string{"review_rejected"}),
	})
	if err != nil {
		t.Fatalf("ReviseWebhookEndpoint: %v", err)
	}
	if prev.URL != srv.URL {
		t.Fatalf("previous URL = %q, want %q", prev.URL, srv.URL)
	}
	if !auditEventsStringListEqual(prev.Events, []string{"transfer_posted"}) {
		t.Fatalf("previous Events = %v, want [transfer_posted]", prev.Events)
	}
	live, ok := d.EndpointConfig("risk")
	if !ok {
		t.Fatalf("endpoint vanished after revision")
	}
	if live.URL != srv2.URL {
		t.Fatalf("live URL = %q, want %q", live.URL, srv2.URL)
	}
	if !auditEventsStringListEqual(live.Events, []string{"review_rejected"}) {
		t.Fatalf("live Events = %v, want [review_rejected]", live.Events)
	}
	if _, ok := d.EndpointConfig("ghost"); ok {
		t.Fatalf("EndpointConfig(ghost) returned a config, want none")
	}
}

func TestReviseWebhookEndpointNoOpWritesNoAudit(t *testing.T) {
	cap := &webhookCapture{behavior: func(int) int { return 200 }}
	d, _ := reviseWebhookTestDispatcher(t, cap, []string{"transfer_posted"})
	l, al, dir := deliveryAmendTestLedger(t, d)

	// One real revision, as a positive control: it must audit.
	cap2 := &webhookCapture{behavior: func(int) int { return 200 }}
	srv2 := httptest.NewServer(cap2.handler())
	t.Cleanup(srv2.Close)
	if err := l.ReviseWebhookEndpoint("risk", WebhookEndpointRevision{URL: ptrStringForTest(srv2.URL)}); err != nil {
		t.Fatalf("ReviseWebhookEndpoint: %v", err)
	}
	waitForWebhook(t, 5*time.Second, func() bool {
		return len(auditEventsByOp(t, dir)["webhook_endpoint_revised"]) == 1
	}, "1 revised event")

	// All-nil and same-value revisions are no-ops: no new audit.
	if err := l.ReviseWebhookEndpoint("risk", WebhookEndpointRevision{}); err != nil {
		t.Fatalf("all-nil revision: %v", err)
	}
	if err := l.ReviseWebhookEndpoint("risk", WebhookEndpointRevision{URL: ptrStringForTest(srv2.URL)}); err != nil {
		t.Fatalf("same-value URL revision: %v", err)
	}
	if err := l.ReviseWebhookEndpoint("risk", WebhookEndpointRevision{Events: ptrEventsForTest([]string{"transfer_posted"})}); err != nil {
		t.Fatalf("same-value Events revision: %v", err)
	}
	if err := l.ReviseWebhookEndpointCtx(context.Background(), "risk", WebhookEndpointRevision{}); err != nil {
		t.Fatalf("all-nil Ctx revision: %v", err)
	}
	time.Sleep(300 * time.Millisecond) // let any stray event flush
	if err := al.Close(); err != nil {
		t.Fatalf("Close audit log: %v", err)
	}
	if events := auditEventsByOp(t, dir)["webhook_endpoint_revised"]; len(events) != 1 {
		t.Fatalf("revised events = %d, want still 1 after the no-op revisions", len(events))
	}
}

func TestReviseWebhookEndpointMovesNextDeliveryToNewURL(t *testing.T) {
	cap := &webhookCapture{behavior: func(int) int { return 200 }}
	d, _ := reviseWebhookTestDispatcher(t, cap, nil)
	l := webhookTestLedger(t, d)

	mustPostWebhookEntry(t, l, "amend-url-1")
	waitForWebhook(t, 10*time.Second, func() bool { return len(cap.attempts()) == 1 }, "first delivery at the old URL")

	cap2 := &webhookCapture{behavior: func(int) int { return 200 }}
	srv2 := httptest.NewServer(cap2.handler())
	t.Cleanup(srv2.Close)
	if err := l.ReviseWebhookEndpoint("risk", WebhookEndpointRevision{URL: ptrStringForTest(srv2.URL)}); err != nil {
		t.Fatalf("ReviseWebhookEndpoint: %v", err)
	}

	mustPostWebhookEntry(t, l, "amend-url-2")
	waitForWebhook(t, 10*time.Second, func() bool { return len(cap2.attempts()) == 1 }, "second delivery at the new URL")
	if n := len(cap.attempts()); n != 1 {
		t.Fatalf("old server got %d deliveries, want 1: the revision must not replay old events", n)
	}
	if delivered, failedAttempts, failures, dropped := d.Stats(); delivered != 2 || failedAttempts != 0 || failures != 0 || dropped != 0 {
		t.Fatalf("Stats() = %d/%d/%d/%d, want 2/0/0/0", delivered, failedAttempts, failures, dropped)
	}
}

func TestReviseWebhookEndpointInFlightKeepsOldURL(t *testing.T) {
	cap := &webhookCapture{behavior: func(int) int { return 500 }}
	srv := httptest.NewServer(cap.handler())
	t.Cleanup(srv.Close)
	cfg := webhookTestEndpoint(t, "risk", srv.URL, mustRotatingKey(t, "primary", "s3cr3t"))
	cfg.MaxAttempts = 3
	d, err := NewWebhookDispatcher(cfg)
	if err != nil {
		t.Fatalf("NewWebhookDispatcher: %v", err)
	}
	t.Cleanup(func() { d.Stop() })
	l := webhookTestLedger(t, d)

	cap2 := &webhookCapture{behavior: func(int) int { return 200 }}
	srv2 := httptest.NewServer(cap2.handler())
	t.Cleanup(srv2.Close)

	mustPostWebhookEntry(t, l, "amend-url-inflight-1")
	// Wait until the first attempt has landed, then revise mid-flight:
	// the delivery must finish under the target it started with —
	// every retry still goes to the old URL.
	deliveryRecordFor(t, d, "risk", func(r DeliveryRecord) bool { return r.Attempts >= 1 }, "first attempt to land")
	if err := l.ReviseWebhookEndpoint("risk", WebhookEndpointRevision{URL: ptrStringForTest(srv2.URL)}); err != nil {
		t.Fatalf("ReviseWebhookEndpoint mid-flight: %v", err)
	}
	rec := deliveryRecordFor(t, d, "risk", func(r DeliveryRecord) bool { return r.Status == WebhookDeliveryFailed }, "in-flight delivery to exhaust")
	if rec.Attempts != 3 {
		t.Fatalf("in-flight delivery attempts = %d, want 3 (the pre-revision budget)", rec.Attempts)
	}
	if n := len(cap.attempts()); n != 3 {
		t.Fatalf("old server got %d attempts, want 3: every retry kept the old target", n)
	}
	if n := len(cap2.attempts()); n != 0 {
		t.Fatalf("new server got %d attempts, want 0: no retry leaked to the new target", n)
	}

	// The next delivery reads the new URL.
	mustPostWebhookEntry(t, l, "amend-url-inflight-2")
	waitForWebhook(t, 10*time.Second, func() bool { return len(cap2.attempts()) == 1 }, "next delivery at the new URL")
}

func TestReviseWebhookEndpointChangesSubscription(t *testing.T) {
	cap := &webhookCapture{behavior: func(int) int { return 200 }}
	d, _ := reviseWebhookTestDispatcher(t, cap, []string{"transfer_posted"})

	notifyWebhookEvent(d, 1, "transfer_posted")
	waitForWebhook(t, 10*time.Second, func() bool { return len(cap.attempts()) == 1 }, "transfer_posted to deliver")
	notifyWebhookEvent(d, 2, "review_rejected")
	time.Sleep(300 * time.Millisecond) // let a stray delivery land
	if n := len(cap.attempts()); n != 1 {
		t.Fatalf("attempts = %d, want 1: review_rejected must not match yet", n)
	}

	if _, err := d.ReviseWebhookEndpoint("risk", WebhookEndpointRevision{Events: ptrEventsForTest([]string{"review_rejected"})}); err != nil {
		t.Fatalf("ReviseWebhookEndpoint: %v", err)
	}
	notifyWebhookEvent(d, 3, "transfer_posted")
	time.Sleep(300 * time.Millisecond)
	if n := len(cap.attempts()); n != 1 {
		t.Fatalf("attempts = %d, want 1: transfer_posted must stop after the revision", n)
	}
	notifyWebhookEvent(d, 4, "review_rejected")
	waitForWebhook(t, 10*time.Second, func() bool { return len(cap.attempts()) == 2 }, "review_rejected to deliver")

	// An empty list re-opens the endpoint to every type.
	if _, err := d.ReviseWebhookEndpoint("risk", WebhookEndpointRevision{Events: ptrEventsForTest([]string{})}); err != nil {
		t.Fatalf("ReviseWebhookEndpoint to empty: %v", err)
	}
	notifyWebhookEvent(d, 5, "transfer_posted")
	waitForWebhook(t, 10*time.Second, func() bool { return len(cap.attempts()) == 3 }, "transfer_posted to deliver again")
	notifyWebhookEvent(d, 6, "review_rejected")
	waitForWebhook(t, 10*time.Second, func() bool { return len(cap.attempts()) == 4 }, "review_rejected to deliver again")

	attempts := cap.attempts()
	want := []string{"transfer_posted", "review_rejected", "transfer_posted", "review_rejected"}
	for i, typ := range want {
		if attempts[i].typ != typ {
			t.Fatalf("attempt %d type = %q, want %q", i+1, attempts[i].typ, typ)
		}
	}
}

func TestReviseWebhookEndpointQueuedEventsKeepOldSubscription(t *testing.T) {
	cap := &webhookCapture{behavior: func(attempt int) int {
		time.Sleep(300 * time.Millisecond) // keep the worker busy
		return 200
	}}
	d, _ := reviseWebhookTestDispatcher(t, cap, []string{"transfer_posted"})

	cap2 := &webhookCapture{behavior: func(int) int { return 200 }}
	srv2 := httptest.NewServer(cap2.handler())
	t.Cleanup(srv2.Close)

	notifyWebhookEvent(d, 1, "transfer_posted")
	waitForWebhook(t, 10*time.Second, func() bool { return len(cap.attempts()) == 1 }, "first attempt to land")
	// The second event is matched and queued under the old filter
	// while the first delivery is still in flight.
	notifyWebhookEvent(d, 2, "transfer_posted")
	if _, err := d.ReviseWebhookEndpoint("risk", WebhookEndpointRevision{
		URL:    ptrStringForTest(srv2.URL),
		Events: ptrEventsForTest([]string{"review_rejected"}),
	}); err != nil {
		t.Fatalf("ReviseWebhookEndpoint: %v", err)
	}
	// A transfer_posted fanned out after the revision matches nothing.
	notifyWebhookEvent(d, 3, "transfer_posted")

	// The queued event still delivers — to the new URL, since its own
	// delivery snapshots the target at entry.
	waitForWebhook(t, 10*time.Second, func() bool { return len(cap2.attempts()) == 1 }, "queued event to deliver at the new URL")
	time.Sleep(300 * time.Millisecond) // let a stray delivery land
	if n := len(cap.attempts()); n != 1 {
		t.Fatalf("old server got %d attempts, want 1: only the in-flight event went there", n)
	}
	if attempts := cap2.attempts(); len(attempts) != 1 || attempts[0].seq != "2" {
		t.Fatalf("new server attempts = %+v, want exactly seq 2", attempts)
	}
	for _, rec := range d.Deliveries(WebhookDeliveryQuery{Endpoint: "risk"}) {
		if rec.Status != WebhookDeliveryDelivered || rec.Attempts != 1 {
			t.Fatalf("record = %+v, want delivered in 1 attempt", rec)
		}
	}
	if delivered, _, failures, dropped := d.Stats(); delivered != 2 || failures != 0 || dropped != 0 {
		t.Fatalf("Stats() delivered/failures/dropped = %d/%d/%d, want 2/0/0", delivered, failures, dropped)
	}
}

func TestReviseWebhookEndpointAuditEventFields(t *testing.T) {
	cap := &webhookCapture{behavior: func(int) int { return 200 }}
	d, srv := reviseWebhookTestDispatcher(t, cap, []string{"transfer_posted"})
	l, al, dir := deliveryAmendTestLedger(t, d)

	cap2 := &webhookCapture{behavior: func(int) int { return 200 }}
	srv2 := httptest.NewServer(cap2.handler())
	t.Cleanup(srv2.Close)
	if err := l.ReviseWebhookEndpoint("risk", WebhookEndpointRevision{
		URL:    ptrStringForTest(srv2.URL),
		Events: ptrEventsForTest([]string{"review_rejected", "transfer_posted"}),
	}); err != nil {
		t.Fatalf("ReviseWebhookEndpoint: %v", err)
	}
	var events []AuditEvent
	waitForWebhook(t, 5*time.Second, func() bool {
		events = auditEventsByOp(t, dir)["webhook_endpoint_revised"]
		return len(events) == 1
	}, "1 revised event")
	ev := events[0]
	if ev.Actor != "ReviseWebhookEndpoint" {
		t.Fatalf("Actor = %q, want ReviseWebhookEndpoint", ev.Actor)
	}
	if ev.Details["endpoint"] != "risk" {
		t.Fatalf("endpoint = %v, want risk", ev.Details["endpoint"])
	}
	if got := ev.Details["previous_url"]; got != srv.URL {
		t.Fatalf("previous_url = %v, want %q", got, srv.URL)
	}
	if got := ev.Details["current_url"]; got != srv2.URL {
		t.Fatalf("current_url = %v, want %q", got, srv2.URL)
	}
	if got := auditEventsStringList(t, ev, "previous_events"); !auditEventsStringListEqual(got, []string{"transfer_posted"}) {
		t.Fatalf("previous_events = %v, want [transfer_posted]", got)
	}
	if got := auditEventsStringList(t, ev, "current_events"); !auditEventsStringListEqual(got, []string{"review_rejected", "transfer_posted"}) {
		t.Fatalf("current_events = %v, want [review_rejected transfer_posted]", got)
	}
	if ev.VersionBefore != ev.VersionAfter {
		t.Fatalf("version moved %d -> %d, want no bump", ev.VersionBefore, ev.VersionAfter)
	}
	// Only the endpoint plus the two changed fields ride in the
	// event: no key material, and no other keys at all.
	if len(ev.Details) != 5 {
		t.Fatalf("details = %v, want exactly endpoint + previous/current url/events", ev.Details)
	}
	blob, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	for _, bad := range []string{"s3cr3t", "secret", "key"} {
		if strings.Contains(string(blob), bad) {
			t.Fatalf("audit event contains %q — key material must never ride in a revision event", bad)
		}
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close audit log: %v", err)
	}
}

func TestReviseWebhookEndpointCtxActorAttribution(t *testing.T) {
	cap := &webhookCapture{behavior: func(int) int { return 200 }}
	d, _ := reviseWebhookTestDispatcher(t, cap, nil)
	l, al, dir := deliveryAmendTestLedger(t, d)

	cap2 := &webhookCapture{behavior: func(int) int { return 200 }}
	srv2 := httptest.NewServer(cap2.handler())
	t.Cleanup(srv2.Close)
	cap3 := &webhookCapture{behavior: func(int) int { return 200 }}
	srv3 := httptest.NewServer(cap3.handler())
	t.Cleanup(srv3.Close)

	ctx := ContextWithActor(context.Background(), "ops-alan")
	if err := l.ReviseWebhookEndpointCtx(ctx, "risk", WebhookEndpointRevision{URL: ptrStringForTest(srv2.URL)}); err != nil {
		t.Fatalf("ReviseWebhookEndpointCtx: %v", err)
	}
	// A plain call keeps the entrypoint name as the actor.
	if err := l.ReviseWebhookEndpoint("risk", WebhookEndpointRevision{URL: ptrStringForTest(srv3.URL)}); err != nil {
		t.Fatalf("ReviseWebhookEndpoint: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close audit log: %v", err)
	}
	events := auditEventsByOp(t, dir)["webhook_endpoint_revised"]
	if len(events) != 2 {
		t.Fatalf("revised events = %d, want 2", len(events))
	}
	if events[0].Actor != "ops-alan" {
		t.Fatalf("first event Actor = %q, want ops-alan", events[0].Actor)
	}
	if got := events[0].Details["current_url"]; got != srv2.URL {
		t.Fatalf("first event current_url = %v, want %q", got, srv2.URL)
	}
	if events[1].Actor != "ReviseWebhookEndpoint" {
		t.Fatalf("second event Actor = %q, want ReviseWebhookEndpoint", events[1].Actor)
	}
	if got := events[1].Details["previous_url"]; got != srv2.URL {
		t.Fatalf("second event previous_url = %v, want %q", got, srv2.URL)
	}
	if got := events[1].Details["current_url"]; got != srv3.URL {
		t.Fatalf("second event current_url = %v, want %q", got, srv3.URL)
	}
}

func TestReviseWebhookEndpointDoesNotAliasCallerSlice(t *testing.T) {
	cap := &webhookCapture{behavior: func(int) int { return 200 }}
	d, _ := reviseWebhookTestDispatcher(t, cap, []string{"transfer_posted"})

	events := []string{"review_rejected", "transfer_posted"}
	if _, err := d.ReviseWebhookEndpoint("risk", WebhookEndpointRevision{Events: ptrEventsForTest(events)}); err != nil {
		t.Fatalf("ReviseWebhookEndpoint: %v", err)
	}
	// Mutating the caller's slice after the call must not move the
	// live filter.
	events[0] = "tampered"
	live, _ := d.EndpointConfig("risk")
	if !auditEventsStringListEqual(live.Events, []string{"review_rejected", "transfer_posted"}) {
		t.Fatalf("live Events = %v, want [review_rejected transfer_posted]: the revision aliased the caller's slice", live.Events)
	}
	// Mutating a returned config must not move the live filter either.
	live.Events[0] = "tampered"
	again, _ := d.EndpointConfig("risk")
	if !auditEventsStringListEqual(again.Events, []string{"review_rejected", "transfer_posted"}) {
		t.Fatalf("live Events = %v after mutating a getter copy, want [review_rejected transfer_posted]", again.Events)
	}
}
