package ledger

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Outbound webhook signing-key grace-period amendment (LG-335):
// SetWebhookKeyGracePeriod revises one registered endpoint's retired-key
// grace window in place — no Unregister/Register teardown, no delivery
// window in which events are lost.

// graceRing builds a key ring with the given grace period.
func graceRing(t *testing.T, grace time.Duration) *RotatingKey {
	t.Helper()
	k, err := NewRotatingKey("k1", []byte("s3cr3t-1"), grace)
	if err != nil {
		t.Fatalf("NewRotatingKey: %v", err)
	}
	return k
}

// graceSign signs body with the ring's primary and returns the wire
// signature.
func graceSign(t *testing.T, k *RotatingKey, body []byte) string {
	t.Helper()
	sig, _, err := k.Sign(body)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	return sig
}

func TestRotatingKeySetGraceRejectsNegative(t *testing.T) {
	k := graceRing(t, 24*time.Hour)
	prev, err := k.SetGrace(-time.Second)
	if !errors.Is(err, ErrInvalidWebhookKeyGracePeriod) {
		t.Fatalf("SetGrace(-1s) err = %v, want ErrInvalidWebhookKeyGracePeriod", err)
	}
	if prev != 24*time.Hour {
		t.Fatalf("SetGrace(-1s) prev = %s, want 24h", prev)
	}
	if got := k.Grace(); got != 24*time.Hour {
		t.Fatalf("after rejected SetGrace, Grace() = %s, want 24h", got)
	}
}

func TestRotatingKeySetGraceZeroStopsVerifyingImmediately(t *testing.T) {
	k := graceRing(t, 24*time.Hour)
	body := []byte(`{"seq":1,"type":"entry_posted"}`)
	sig := graceSign(t, k, body)
	if err := k.Rotate("k2", []byte("s3cr3t-2")); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if !k.Verify(body, sig) {
		t.Fatalf("retired key must verify inside its grace period")
	}
	prev, err := k.SetGrace(0)
	if err != nil || prev != 24*time.Hour {
		t.Fatalf("SetGrace(0) = (%s, %v), want (24h, nil)", prev, err)
	}
	if got := k.Grace(); got != 0 {
		t.Fatalf("Grace() = %s, want 0", got)
	}
	if k.Verify(body, sig) {
		t.Fatalf("retired key must stop verifying once the grace is 0")
	}
	if k.VerifyWithKey(body, "k1", sig) {
		t.Fatalf("VerifyWithKey must refuse the retired key once the grace is 0")
	}
	// Sign is unaffected: it always uses the primary.
	if sig2 := graceSign(t, k, body); !k.Verify(body, sig2) {
		t.Fatalf("primary signature must still verify after the grace is 0")
	}
}

func TestRotatingKeySetGraceSameValueNoOp(t *testing.T) {
	k := graceRing(t, 24*time.Hour)
	prev, err := k.SetGrace(24 * time.Hour)
	if err != nil || prev != 24*time.Hour {
		t.Fatalf("SetGrace(24h) = (%s, %v), want (24h, nil)", prev, err)
	}
	if got := k.Grace(); got != 24*time.Hour {
		t.Fatalf("Grace() = %s, want 24h", got)
	}
}

func TestSetKeyGracePeriodTightensVerification(t *testing.T) {
	ring := graceRing(t, 24*time.Hour)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	d, err := NewWebhookDispatcher(webhookTestEndpoint(t, "risk", srv.URL, ring))
	if err != nil {
		t.Fatalf("NewWebhookDispatcher: %v", err)
	}
	t.Cleanup(func() { d.Stop() })

	body := []byte(`{"seq":1,"type":"entry_posted"}`)
	sig := graceSign(t, ring, body)
	if err := d.RotateKey("risk", "k2", []byte("s3cr3t-2")); err != nil {
		t.Fatalf("RotateKey: %v", err)
	}
	if !ring.Verify(body, sig) {
		t.Fatalf("retired key must verify inside its grace period")
	}
	prev, err := d.SetKeyGracePeriod("risk", time.Millisecond)
	if err != nil || prev != 24*time.Hour {
		t.Fatalf("SetKeyGracePeriod(1ms) = (%s, %v), want (24h, nil)", prev, err)
	}
	if got, ok := d.KeyGracePeriod("risk"); !ok || got != time.Millisecond {
		t.Fatalf("KeyGracePeriod() = (%s, %v), want (1ms, true)", got, ok)
	}
	time.Sleep(5 * time.Millisecond)
	if ring.Verify(body, sig) {
		t.Fatalf("retired key must stop verifying once the tightened grace lapses")
	}
}

func TestSetKeyGracePeriodWidensVerification(t *testing.T) {
	ring := graceRing(t, 50*time.Millisecond)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	d, err := NewWebhookDispatcher(webhookTestEndpoint(t, "risk", srv.URL, ring))
	if err != nil {
		t.Fatalf("NewWebhookDispatcher: %v", err)
	}
	t.Cleanup(func() { d.Stop() })

	body := []byte(`{"seq":1,"type":"entry_posted"}`)
	sig := graceSign(t, ring, body)
	if err := d.RotateKey("risk", "k2", []byte("s3cr3t-2")); err != nil {
		t.Fatalf("RotateKey: %v", err)
	}
	// Let the 50ms grace lapse without calling Verify/Keys, so the
	// retired key is logically expired but not yet purged.
	time.Sleep(120 * time.Millisecond)
	prev, err := d.SetKeyGracePeriod("risk", 24*time.Hour)
	if err != nil || prev != 50*time.Millisecond {
		t.Fatalf("SetKeyGracePeriod(24h) = (%s, %v), want (50ms, nil)", prev, err)
	}
	if !ring.Verify(body, sig) {
		t.Fatalf("widening the grace must make the un-purged retired key verify again")
	}
}

func TestSetKeyGracePeriodDoesNotRevivePurgedKey(t *testing.T) {
	ring := graceRing(t, 50*time.Millisecond)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	d, err := NewWebhookDispatcher(webhookTestEndpoint(t, "risk", srv.URL, ring))
	if err != nil {
		t.Fatalf("NewWebhookDispatcher: %v", err)
	}
	t.Cleanup(func() { d.Stop() })

	body := []byte(`{"seq":1,"type":"entry_posted"}`)
	sig := graceSign(t, ring, body)
	if err := d.RotateKey("risk", "k2", []byte("s3cr3t-2")); err != nil {
		t.Fatalf("RotateKey: %v", err)
	}
	time.Sleep(120 * time.Millisecond)
	// This Verify purges the expired retired key.
	if ring.Verify(body, sig) {
		t.Fatalf("retired key must not verify after its grace lapses")
	}
	if _, err := d.SetKeyGracePeriod("risk", 24*time.Hour); err != nil {
		t.Fatalf("SetKeyGracePeriod(24h): %v", err)
	}
	if ring.Verify(body, sig) {
		t.Fatalf("widening the grace must not revive a purged key")
	}
}

func TestSetKeyGracePeriodGovernsNewlyRotatedKeys(t *testing.T) {
	ring := graceRing(t, 24*time.Hour)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	d, err := NewWebhookDispatcher(webhookTestEndpoint(t, "risk", srv.URL, ring))
	if err != nil {
		t.Fatalf("NewWebhookDispatcher: %v", err)
	}
	t.Cleanup(func() { d.Stop() })

	if _, err := d.SetKeyGracePeriod("risk", 50*time.Millisecond); err != nil {
		t.Fatalf("SetKeyGracePeriod(50ms): %v", err)
	}
	body := []byte(`{"seq":1,"type":"entry_posted"}`)
	sig := graceSign(t, ring, body)
	if err := d.RotateKey("risk", "k2", []byte("s3cr3t-2")); err != nil {
		t.Fatalf("RotateKey: %v", err)
	}
	time.Sleep(120 * time.Millisecond)
	if ring.Verify(body, sig) {
		t.Fatalf("key retired after the tightening must follow the new 50ms grace")
	}
	// The new primary still signs and verifies.
	if sig2 := graceSign(t, ring, body); !ring.Verify(body, sig2) {
		t.Fatalf("new primary signature must verify")
	}
}

func TestSetKeyGracePeriodUnknownEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	d, err := NewWebhookDispatcher(webhookTestEndpoint(t, "risk", srv.URL, mustRotatingKey(t, "primary", "s3cr3t")))
	if err != nil {
		t.Fatalf("NewWebhookDispatcher: %v", err)
	}
	t.Cleanup(func() { d.Stop() })
	prev, err := d.SetKeyGracePeriod("nope", time.Hour)
	if !errors.Is(err, ErrWebhookEndpointNotFound) {
		t.Fatalf("SetKeyGracePeriod(unknown) err = %v, want ErrWebhookEndpointNotFound", err)
	}
	if prev != 0 {
		t.Fatalf("SetKeyGracePeriod(unknown) prev = %s, want 0", prev)
	}
	if _, ok := d.KeyGracePeriod("nope"); ok {
		t.Fatalf("KeyGracePeriod(unknown) ok = true, want false")
	}
}

func TestSetKeyGracePeriodStoppedDispatcher(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	d, err := NewWebhookDispatcher(webhookTestEndpoint(t, "risk", srv.URL, mustRotatingKey(t, "primary", "s3cr3t")))
	if err != nil {
		t.Fatalf("NewWebhookDispatcher: %v", err)
	}
	d.Stop()
	if _, err := d.SetKeyGracePeriod("risk", time.Hour); err == nil {
		t.Fatalf("SetKeyGracePeriod on a stopped dispatcher must fail")
	}
}

func TestSetWebhookKeyGracePeriodUnknownEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	d, err := NewWebhookDispatcher(webhookTestEndpoint(t, "risk", srv.URL, mustRotatingKey(t, "primary", "s3cr3t")))
	if err != nil {
		t.Fatalf("NewWebhookDispatcher: %v", err)
	}
	t.Cleanup(func() { d.Stop() })
	l, _, _ := deliveryAmendTestLedger(t, d)
	prev, err := l.SetWebhookKeyGracePeriod("nope", time.Hour)
	if !errors.Is(err, ErrWebhookEndpointNotFound) {
		t.Fatalf("SetWebhookKeyGracePeriod(unknown) err = %v, want ErrWebhookEndpointNotFound", err)
	}
	if prev != 0 {
		t.Fatalf("SetWebhookKeyGracePeriod(unknown) prev = %s, want 0", prev)
	}
	if _, ok := l.WebhookKeyGracePeriod("nope"); ok {
		t.Fatalf("WebhookKeyGracePeriod(unknown) ok = true, want false")
	}
}

func TestSetWebhookKeyGracePeriodWithoutDispatcher(t *testing.T) {
	base := t.TempDir()
	auditDir := filepath.Join(base, "audit")
	outboxDir := filepath.Join(base, "outbox")
	if err := os.MkdirAll(auditDir, 0o755); err != nil {
		t.Fatalf("MkdirAll audit: %v", err)
	}
	if err := os.MkdirAll(outboxDir, 0o755); err != nil {
		t.Fatalf("MkdirAll outbox: %v", err)
	}
	al, err := NewAuditLog(auditDir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	ob, err := NewOutbox(outboxDir)
	if err != nil {
		t.Fatalf("NewOutbox: %v", err)
	}
	t.Cleanup(func() { _ = ob.Close() })
	l := New(WithAuditLog(al), WithOutbox(ob))
	prev, err := l.SetWebhookKeyGracePeriod("risk", time.Hour)
	if !errors.Is(err, ErrWebhooksNotConfigured) {
		t.Fatalf("SetWebhookKeyGracePeriod without dispatcher err = %v, want ErrWebhooksNotConfigured", err)
	}
	if prev != 0 {
		t.Fatalf("SetWebhookKeyGracePeriod without dispatcher prev = %s, want 0", prev)
	}
	if _, ok := l.WebhookKeyGracePeriod("risk"); ok {
		t.Fatalf("WebhookKeyGracePeriod without dispatcher ok = true, want false")
	}
}

func TestSetWebhookKeyGracePeriodAuditsPreviousAndCurrent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	d, err := NewWebhookDispatcher(webhookTestEndpoint(t, "risk", srv.URL, mustRotatingKey(t, "primary", "s3cr3t")))
	if err != nil {
		t.Fatalf("NewWebhookDispatcher: %v", err)
	}
	t.Cleanup(func() { d.Stop() })
	l, al, dir := deliveryAmendTestLedger(t, d)

	prev, err := l.SetWebhookKeyGracePeriod("risk", 30*time.Minute)
	if err != nil || prev != time.Hour {
		t.Fatalf("SetWebhookKeyGracePeriod(30m) = (%s, %v), want (1h, nil)", prev, err)
	}
	if got, ok := l.WebhookKeyGracePeriod("risk"); !ok || got != 30*time.Minute {
		t.Fatalf("WebhookKeyGracePeriod() = (%s, %v), want (30m, true)", got, ok)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close audit log: %v", err)
	}
	events := auditEventsByOp(t, dir)["webhook_key_grace_period_retuned"]
	if len(events) != 1 {
		t.Fatalf("retuned events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Actor != "SetWebhookKeyGracePeriod" {
		t.Fatalf("event Actor = %q, want SetWebhookKeyGracePeriod", ev.Actor)
	}
	if ev.Details["endpoint"] != "risk" {
		t.Fatalf("event endpoint = %v, want risk", ev.Details["endpoint"])
	}
	if got := auditDetailFloat(t, ev, "previous_grace_period_ns"); got != float64(time.Hour.Nanoseconds()) {
		t.Fatalf("previous_grace_period_ns = %v, want %v", got, time.Hour.Nanoseconds())
	}
	if got := auditDetailFloat(t, ev, "current_grace_period_ns"); got != float64((30 * time.Minute).Nanoseconds()) {
		t.Fatalf("current_grace_period_ns = %v, want %v", got, (30 * time.Minute).Nanoseconds())
	}
	if ev.VersionBefore != ev.VersionAfter {
		t.Fatalf("version moved %d -> %d, want no bump", ev.VersionBefore, ev.VersionAfter)
	}
}

func TestSetWebhookKeyGracePeriodCtxRecordsOperator(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	d, err := NewWebhookDispatcher(webhookTestEndpoint(t, "risk", srv.URL, mustRotatingKey(t, "primary", "s3cr3t")))
	if err != nil {
		t.Fatalf("NewWebhookDispatcher: %v", err)
	}
	t.Cleanup(func() { d.Stop() })
	l, al, dir := deliveryAmendTestLedger(t, d)

	ctx := ContextWithActor(context.Background(), "op-9")
	prev, err := l.SetWebhookKeyGracePeriodCtx(ctx, "risk", 2*time.Hour)
	if err != nil || prev != time.Hour {
		t.Fatalf("SetWebhookKeyGracePeriodCtx = (%s, %v), want (1h, nil)", prev, err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close audit log: %v", err)
	}
	events := auditEventsByOp(t, dir)["webhook_key_grace_period_retuned"]
	if len(events) != 1 {
		t.Fatalf("retuned events = %d, want 1", len(events))
	}
	if events[0].Actor != "op-9" {
		t.Fatalf("event Actor = %q, want op-9", events[0].Actor)
	}
}

func TestSetWebhookKeyGracePeriodSameValueNoAudit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	d, err := NewWebhookDispatcher(webhookTestEndpoint(t, "risk", srv.URL, mustRotatingKey(t, "primary", "s3cr3t")))
	if err != nil {
		t.Fatalf("NewWebhookDispatcher: %v", err)
	}
	t.Cleanup(func() { d.Stop() })
	l, al, dir := deliveryAmendTestLedger(t, d)

	prev, err := l.SetWebhookKeyGracePeriod("risk", time.Hour)
	if err != nil || prev != time.Hour {
		t.Fatalf("SetWebhookKeyGracePeriod(1h) = (%s, %v), want (1h, nil)", prev, err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close audit log: %v", err)
	}
	if events := auditEventsByOp(t, dir)["webhook_key_grace_period_retuned"]; len(events) != 0 {
		t.Fatalf("no-op revision audited %d events, want 0", len(events))
	}
}

func TestSetWebhookKeyGracePeriodRejectsNegative(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	d, err := NewWebhookDispatcher(webhookTestEndpoint(t, "risk", srv.URL, mustRotatingKey(t, "primary", "s3cr3t")))
	if err != nil {
		t.Fatalf("NewWebhookDispatcher: %v", err)
	}
	t.Cleanup(func() { d.Stop() })
	l, al, dir := deliveryAmendTestLedger(t, d)

	prev, err := l.SetWebhookKeyGracePeriod("risk", -time.Second)
	if !errors.Is(err, ErrInvalidWebhookKeyGracePeriod) {
		t.Fatalf("SetWebhookKeyGracePeriod(-1s) err = %v, want ErrInvalidWebhookKeyGracePeriod", err)
	}
	if prev != time.Hour {
		t.Fatalf("SetWebhookKeyGracePeriod(-1s) prev = %s, want 1h (the live grace)", prev)
	}
	if got, ok := l.WebhookKeyGracePeriod("risk"); !ok || got != time.Hour {
		t.Fatalf("after rejected revision, WebhookKeyGracePeriod() = (%s, %v), want (1h, true)", got, ok)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close audit log: %v", err)
	}
	if events := auditEventsByOp(t, dir)["webhook_key_grace_period_retuned"]; len(events) != 0 {
		t.Fatalf("rejected revision audited %d events, want 0", len(events))
	}
}

func TestSetWebhookKeyGracePeriodConcurrentVerifyAndSign(t *testing.T) {
	ring := graceRing(t, 24*time.Hour)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	d, err := NewWebhookDispatcher(webhookTestEndpoint(t, "risk", srv.URL, ring))
	if err != nil {
		t.Fatalf("NewWebhookDispatcher: %v", err)
	}
	t.Cleanup(func() { d.Stop() })
	body := []byte(`{"seq":1,"type":"entry_posted"}`)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				grace := time.Hour
				if (i+j)%2 == 0 {
					grace = 2 * time.Hour
				}
				if _, err := d.SetKeyGracePeriod("risk", grace); err != nil {
					t.Errorf("SetKeyGracePeriod: %v", err)
					return
				}
			}
		}(i)
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				sig, _, err := ring.Sign(body)
				if err != nil {
					t.Errorf("Sign: %v", err)
					return
				}
				ring.Verify(body, sig)
				ring.VerifyWithKey(body, "k1", sig)
				ring.Grace()
			}
		}()
	}
	wg.Wait()
}

// waitForAttempts polls the capture server until it has recorded n
// attempts, or fails the test.
func waitForAttempts(t *testing.T, cap *timedWebhookCapture, n int, msg string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(cap.times()) >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d delivery attempts: %s (got %d)", n, msg, len(cap.times()))
}

func TestSetKeyGracePeriodDoesNotRestartEndpointWorker(t *testing.T) {
	cap := &timedWebhookCapture{slow: 300 * time.Millisecond}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	cfg := webhookTestEndpoint(t, "risk", srv.URL, mustRotatingKey(t, "primary", "s3cr3t"))
	d, err := NewWebhookDispatcher(cfg)
	if err != nil {
		t.Fatalf("NewWebhookDispatcher: %v", err)
	}
	t.Cleanup(func() { d.Stop() })

	// Two events: the first is in flight (slow receiver) while the
	// second waits in the queue.
	d.Notify(OutboxEvent{Seq: 1, Type: "entry_posted", At: time.Now()})
	d.Notify(OutboxEvent{Seq: 2, Type: "entry_posted", At: time.Now()})
	// Revise the grace mid-delivery: the worker must keep going.
	if _, err := d.SetKeyGracePeriod("risk", 2*time.Hour); err != nil {
		t.Fatalf("SetKeyGracePeriod: %v", err)
	}
	waitForAttempts(t, cap, 2, "both events after the revision")
	if got, ok := d.KeyGracePeriod("risk"); !ok || got != 2*time.Hour {
		t.Fatalf("KeyGracePeriod() = (%s, %v), want (2h, true)", got, ok)
	}

	// Control group: Unregister + Register loses the window — an event
	// fanned out while no endpoint is registered is gone for good.
	if !d.Unregister("risk") {
		t.Fatalf("Unregister(risk) = false, want true")
	}
	d.Notify(OutboxEvent{Seq: 3, Type: "entry_posted", At: time.Now()})
	time.Sleep(100 * time.Millisecond)
	if got := len(cap.times()); got != 2 {
		t.Fatalf("attempts after the unregistered window = %d, want 2 (event 3 lost)", got)
	}
	if err := d.Register(cfg); err != nil {
		t.Fatalf("Register after Unregister: %v", err)
	}
	d.Notify(OutboxEvent{Seq: 4, Type: "entry_posted", At: time.Now()})
	waitForAttempts(t, cap, 3, "event after re-registration")
}
