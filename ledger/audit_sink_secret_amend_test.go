package ledger

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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

// Audit-sink signing-secret rotation tests (LG-313). They reuse the
// audit_sink_test.go harness (sinkCapture, sinkTestConfig, mustPostSink,
// waitForSink, testSinkKey) and the LG-306 helper startSinkTestLedger.
// Note: no struct literal in this file names the signing-key field —
// configs come from sinkTestConfig and are tuned by field assignment.

// rotatedSinkKey is the replacement signing key under test; a second
// key covers the concurrent-rotation test.
const rotatedSinkKey = "rotated-sink-signing-key"
const rotatedSinkKeyB = "rotated-sink-signing-key-b"

// sinkSigFor recomputes the X-Ledger-Signature a delivery body must
// carry when signed with key, along the audit_sink_test.go :168
// verification mode.
func sinkSigFor(body []byte, key string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// sinkKeySHA256 returns the full SHA-256 hex of a key — the form
// SinkSecretFingerprint returns.
func sinkKeySHA256(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// sinkKeyPrefix returns the first 12 hex characters of a key's
// SHA-256 — the form the sink_secret_rotated audit event carries.
func sinkKeyPrefix(key string) string {
	return sinkKeySHA256(key)[:12]
}

// sinkRotatedEvents polls the audit log until at least one
// sink_secret_rotated event is visible, then returns all of them.
func sinkRotatedEvents(t *testing.T, auditDir string) []AuditEvent {
	t.Helper()
	day := time.Now().UTC().Format("2006-01-02")
	var out []AuditEvent
	waitForSink(t, 5*time.Second, "rotated audit events", func() bool {
		events, _, err := ReadAuditLog(auditDir, day)
		if err != nil {
			return false
		}
		out = nil
		for _, ev := range events {
			if ev.Op == "sink_secret_rotated" {
				out = append(out, ev)
			}
		}
		return len(out) > 0
	})
	return out
}

// sinkRotatedCount reads the audit log once and counts the
// sink_secret_rotated events.
func sinkRotatedCount(t *testing.T, auditDir string) int {
	t.Helper()
	day := time.Now().UTC().Format("2006-01-02")
	events, _, err := ReadAuditLog(auditDir, day)
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	n := 0
	for _, ev := range events {
		if ev.Op == "sink_secret_rotated" {
			n++
		}
	}
	return n
}

// sinkAuditRawLines reads every raw audit-log line for today, so a
// test can assert no key material ever reached the file.
func sinkAuditRawLines(t *testing.T, auditDir string) []string {
	t.Helper()
	day := time.Now().UTC().Format("2006-01-02")
	matches, err := filepath.Glob(filepath.Join(auditDir, "audit-"+day+"*.jsonl"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("audit glob: %v (matches %d)", err, len(matches))
	}
	var lines []string
	for _, m := range matches {
		raw, err := os.ReadFile(m)
		if err != nil {
			t.Fatalf("read audit file: %v", err)
		}
		lines = append(lines, strings.Split(string(raw), "\n")...)
	}
	return lines
}

func TestRotateSinkSecretSignsNewDeliveriesWithNewKey(t *testing.T) {
	cap := &sinkCapture{}
	l, sink, auditDir := startSinkTestLedger(t, cap, sinkTestConfig(""))

	before := sink.SinkSecretFingerprint()
	if before != sinkKeySHA256(testSinkKey) {
		t.Fatalf("fingerprint = %q, want sha256 of the construction key", before)
	}
	if before == testSinkKey {
		t.Fatalf("fingerprint must never equal the key itself")
	}

	if err := sink.RotateSinkSecret([]byte(rotatedSinkKey)); err != nil {
		t.Fatalf("RotateSinkSecret: %v", err)
	}
	after := sink.SinkSecretFingerprint()
	if after != sinkKeySHA256(rotatedSinkKey) {
		t.Fatalf("fingerprint after rotation = %q, want sha256 of the new key", after)
	}

	mustPostSink(t, l, "rotate-1")
	mustPostSink(t, l, "rotate-2")
	waitForSink(t, 5*time.Second, "2 deliveries", func() bool { return cap.count() == 2 })
	bodies, sigs, _, _ := cap.snapshot()
	for i, body := range bodies {
		if sigs[i] != sinkSigFor(body, rotatedSinkKey) {
			t.Errorf("delivery %d signature does not verify with the new key: %q", i, sigs[i])
		}
		if sigs[i] == sinkSigFor(body, testSinkKey) {
			t.Errorf("delivery %d signature still verifies with the old key", i)
		}
	}
	if sent, failed, dead := sink.Stats(); sent != 2 || failed != 0 || dead != 0 {
		t.Errorf("Stats = (%d, %d, %d), want (2, 0, 0)", sent, failed, dead)
	}

	events := sinkRotatedEvents(t, auditDir)
	if len(events) != 1 {
		t.Fatalf("rotated events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Actor != "RotateSinkSecret" {
		t.Errorf("Actor = %q, want RotateSinkSecret", ev.Actor)
	}
	if got := ev.Details["previous_secret_sha256_prefix"]; got != sinkKeyPrefix(testSinkKey) {
		t.Errorf("previous prefix = %v, want %q", got, sinkKeyPrefix(testSinkKey))
	}
	if got := ev.Details["secret_sha256_prefix"]; got != sinkKeyPrefix(rotatedSinkKey) {
		t.Errorf("current prefix = %v, want %q", got, sinkKeyPrefix(rotatedSinkKey))
	}
}

func TestRotateSinkSecretInFlightDeliveryKeepsOldSignature(t *testing.T) {
	// failFirst:2 with a 200ms backoff leaves a wide window to rotate
	// between the first failed attempt and the retries.
	cap := &sinkCapture{failFirst: 2}
	cfg := sinkTestConfig("")
	cfg.InitialBackoff = 200 * time.Millisecond
	cfg.MaxBackoff = 200 * time.Millisecond
	l, sink, _ := startSinkTestLedger(t, cap, cfg)

	mustPostSink(t, l, "rotate-inflight-1")
	waitForSink(t, 5*time.Second, "first attempt", func() bool { return cap.count() >= 1 })

	// Rotate mid-delivery: attempts 2 and 3 must still carry the old
	// signature, because deliver snapshotted it at entry.
	if err := sink.RotateSinkSecret([]byte(rotatedSinkKey)); err != nil {
		t.Fatalf("RotateSinkSecret: %v", err)
	}
	waitForSink(t, 10*time.Second, "delivery completes", func() bool {
		sent, _, _ := sink.Stats()
		return sent == 1
	})
	bodies, sigs, _, _ := cap.snapshot()
	if len(sigs) != 3 {
		t.Fatalf("attempts = %d, want 3 (fail, fail, success)", len(sigs))
	}
	for i, body := range bodies {
		if sigs[i] != sinkSigFor(body, testSinkKey) {
			t.Errorf("attempt %d signature = %q, want the old-key signature", i+1, sigs[i])
		}
		if sigs[i] == sinkSigFor(body, rotatedSinkKey) {
			t.Errorf("attempt %d signature verifies with the new key — rotation leaked into an in-flight delivery", i+1)
		}
	}
	if sent, failed, dead := sink.Stats(); sent != 1 || failed != 2 || dead != 0 {
		t.Errorf("Stats = (%d, %d, %d), want (1, 2, 0)", sent, failed, dead)
	}
}

func TestRotateSinkSecretRejectsEmpty(t *testing.T) {
	cap := &sinkCapture{}
	l, sink, auditDir := startSinkTestLedger(t, cap, sinkTestConfig(""))

	if err := sink.RotateSinkSecret([]byte{}); !errors.Is(err, ErrInvalidSinkSecret) {
		t.Fatalf("RotateSinkSecret(empty) = %v, want ErrInvalidSinkSecret", err)
	}
	if got := sink.SinkSecretFingerprint(); got != sinkKeySHA256(testSinkKey) {
		t.Fatalf("fingerprint moved after a rejected rotation: %q", got)
	}

	// The old key still signs: the rejection left the live key intact.
	mustPostSink(t, l, "rotate-empty-1")
	waitForSink(t, 5*time.Second, "1 delivery", func() bool { return cap.count() == 1 })
	bodies, sigs, _, _ := cap.snapshot()
	if sigs[0] != sinkSigFor(bodies[0], testSinkKey) {
		t.Errorf("delivery after rejected rotation does not verify with the old key")
	}
	time.Sleep(300 * time.Millisecond)
	if n := sinkRotatedCount(t, auditDir); n != 0 {
		t.Errorf("rotated events = %d, want 0 after a rejected rotation", n)
	}
}

func TestRotateSinkSecretRejectsNil(t *testing.T) {
	cap := &sinkCapture{}
	l, sink, auditDir := startSinkTestLedger(t, cap, sinkTestConfig(""))

	if err := sink.RotateSinkSecret(nil); !errors.Is(err, ErrInvalidSinkSecret) {
		t.Fatalf("RotateSinkSecret(nil) = %v, want ErrInvalidSinkSecret", err)
	}
	if got := sink.SinkSecretFingerprint(); got != sinkKeySHA256(testSinkKey) {
		t.Fatalf("fingerprint moved after a rejected rotation: %q", got)
	}

	mustPostSink(t, l, "rotate-nil-1")
	waitForSink(t, 5*time.Second, "1 delivery", func() bool { return cap.count() == 1 })
	bodies, sigs, _, _ := cap.snapshot()
	if sigs[0] != sinkSigFor(bodies[0], testSinkKey) {
		t.Errorf("delivery after rejected rotation does not verify with the old key")
	}
	time.Sleep(300 * time.Millisecond)
	if n := sinkRotatedCount(t, auditDir); n != 0 {
		t.Errorf("rotated events = %d, want 0 after a rejected rotation", n)
	}
}

func TestRotateSinkSecretSameKeyIsNoop(t *testing.T) {
	cap := &sinkCapture{}
	_, sink, auditDir := startSinkTestLedger(t, cap, sinkTestConfig(""))

	// Byte-identical to the live key: no error, no audit event.
	if err := sink.RotateSinkSecret([]byte(testSinkKey)); err != nil {
		t.Fatalf("RotateSinkSecret(same key) = %v, want nil", err)
	}
	if got := sink.SinkSecretFingerprint(); got != sinkKeySHA256(testSinkKey) {
		t.Fatalf("fingerprint moved on a no-op rotation: %q", got)
	}
	// The audit writer is async; give a wrongly-emitted event time to
	// land, then assert none did.
	time.Sleep(300 * time.Millisecond)
	if n := sinkRotatedCount(t, auditDir); n != 0 {
		t.Errorf("rotated events = %d, want 0 for a no-op rotation", n)
	}
}

func TestRotateSinkSecretCtxActorAttribution(t *testing.T) {
	cap := &sinkCapture{}
	_, sink, auditDir := startSinkTestLedger(t, cap, sinkTestConfig(""))

	ctx := ContextWithActor(context.Background(), "ops-alan")
	if err := sink.RotateSinkSecretCtx(ctx, []byte(rotatedSinkKey)); err != nil {
		t.Fatalf("RotateSinkSecretCtx: %v", err)
	}
	// A plain call keeps the entrypoint name as the actor.
	if err := sink.RotateSinkSecret([]byte(rotatedSinkKeyB)); err != nil {
		t.Fatalf("RotateSinkSecret: %v", err)
	}
	var events []AuditEvent
	waitForSink(t, 5*time.Second, "2 rotated events", func() bool {
		events = sinkRotatedEvents(t, auditDir)
		return len(events) == 2
	})
	if events[0].Actor != "ops-alan" {
		t.Errorf("first event Actor = %q, want ops-alan", events[0].Actor)
	}
	if events[1].Actor != "RotateSinkSecret" {
		t.Errorf("second event Actor = %q, want RotateSinkSecret", events[1].Actor)
	}
	if got := events[0].Details["previous_secret_sha256_prefix"]; got != sinkKeyPrefix(testSinkKey) {
		t.Errorf("first previous prefix = %v, want %q", got, sinkKeyPrefix(testSinkKey))
	}
	if got := events[0].Details["secret_sha256_prefix"]; got != sinkKeyPrefix(rotatedSinkKey) {
		t.Errorf("first current prefix = %v, want %q", got, sinkKeyPrefix(rotatedSinkKey))
	}
	if got := events[1].Details["previous_secret_sha256_prefix"]; got != sinkKeyPrefix(rotatedSinkKey) {
		t.Errorf("second previous prefix = %v, want %q", got, sinkKeyPrefix(rotatedSinkKey))
	}
	if got := events[1].Details["secret_sha256_prefix"]; got != sinkKeyPrefix(rotatedSinkKeyB) {
		t.Errorf("second current prefix = %v, want %q", got, sinkKeyPrefix(rotatedSinkKeyB))
	}
}

func TestRotateSinkSecretAuditEventHasNoKeyMaterial(t *testing.T) {
	cap := &sinkCapture{}
	_, sink, auditDir := startSinkTestLedger(t, cap, sinkTestConfig(""))

	if err := sink.RotateSinkSecret([]byte(rotatedSinkKey)); err != nil {
		t.Fatalf("RotateSinkSecret: %v", err)
	}
	events := sinkRotatedEvents(t, auditDir)
	if len(events) != 1 {
		t.Fatalf("rotated events = %d, want 1", len(events))
	}
	for k, v := range events[0].Details {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("detail %q is %T, want string", k, v)
		}
		if len(s) != 12 {
			t.Errorf("detail %q = %q (%d chars), want a 12-char prefix", k, s, len(s))
		}
	}
	// Scan the raw audit lines: neither the old nor the new key may
	// appear anywhere in the file.
	for _, line := range sinkAuditRawLines(t, auditDir) {
		if strings.Contains(line, testSinkKey) {
			t.Errorf("audit line contains the old key material: %s", line)
		}
		if strings.Contains(line, rotatedSinkKey) {
			t.Errorf("audit line contains the new key material: %s", line)
		}
	}
}

func TestRotateSinkSecretPreservesCursorStatsAndOrder(t *testing.T) {
	cap := &sinkCapture{}
	l, sink, auditDir := startSinkTestLedger(t, cap, sinkTestConfig(""))

	mustPostSink(t, l, "rotate-order-1")
	waitForSink(t, 5*time.Second, "1 delivery", func() bool { return cap.count() == 1 })
	if err := sink.RotateSinkSecret([]byte(rotatedSinkKey)); err != nil {
		t.Fatalf("RotateSinkSecret: %v", err)
	}
	mustPostSink(t, l, "rotate-order-2")
	mustPostSink(t, l, "rotate-order-3")
	waitForSink(t, 5*time.Second, "3 deliveries", func() bool { return cap.count() == 3 })

	bodies, sigs, _, seqs := cap.snapshot()
	if len(seqs) != 3 || seqs[0] != "1" || seqs[1] != "2" || seqs[2] != "3" {
		t.Errorf("seqs = %v, want [1 2 3] in posting order", seqs)
	}
	if sigs[0] != sinkSigFor(bodies[0], testSinkKey) {
		t.Errorf("first delivery does not verify with the pre-rotation key")
	}
	for i := 1; i < 3; i++ {
		if sigs[i] != sinkSigFor(bodies[i], rotatedSinkKey) {
			t.Errorf("delivery %d does not verify with the post-rotation key", i+1)
		}
	}
	if sent, failed, dead := sink.Stats(); sent != 3 || failed != 0 || dead != 0 {
		t.Errorf("Stats = (%d, %d, %d), want (3, 0, 0)", sent, failed, dead)
	}
	dir := filepath.Dir(auditDir)
	cursorPath := filepath.Join(dir, "outbox", sinkCursorFilename)
	waitForSink(t, 5*time.Second, "cursor at 3", func() bool {
		return readCursorFile(t, cursorPath) == "3"
	})
}

func TestRotateSinkSecretConcurrentPostAndRotate(t *testing.T) {
	cap := &sinkCapture{}
	l, sink, _ := startSinkTestLedger(t, cap, sinkTestConfig(""))

	var wg sync.WaitGroup
	// Errors from worker goroutines go back on a channel: FailNow
	// and friends are test-goroutine-only.
	errs := make(chan error, 100)
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			key := rotatedSinkKey
			if r%2 == 1 {
				key = rotatedSinkKeyB
			}
			for i := 0; i < 20; i++ {
				// Alternate keys so the live value actually moves.
				if i%2 == 1 {
					if key == rotatedSinkKey {
						key = rotatedSinkKeyB
					} else {
						key = rotatedSinkKey
					}
				}
				if err := sink.RotateSinkSecret([]byte(key)); err != nil {
					errs <- err
					return
				}
			}
		}(r)
	}
	for p := 0; p < 2; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				_, _, err := l.Post(JournalEntry{
					ID:           "rotate-race-" + string(rune('a'+p)) + string(rune('0'+i)),
					DebitAccount: "cash", CreditAccount: "equity",
					AmountCents: 1000, Currency: "USD",
				})
				if err != nil {
					errs <- err
					return
				}
			}
		}(p)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Post/Rotate: %v", err)
	}

	// All 20 posts deliver; every signature verifies under one of the
	// two keys that were live during the race — never a torn mix.
	waitForSink(t, 10*time.Second, "20 deliveries", func() bool { return cap.count() == 20 })
	bodies, sigs, _, _ := cap.snapshot()
	if len(sigs) != 20 {
		t.Fatalf("deliveries = %d, want 20", len(sigs))
	}
	for i, body := range bodies {
		if sigs[i] != sinkSigFor(body, rotatedSinkKey) && sigs[i] != sinkSigFor(body, rotatedSinkKeyB) && sigs[i] != sinkSigFor(body, testSinkKey) {
			t.Errorf("delivery %d signature verifies under none of the live keys", i)
		}
	}
	fp := sink.SinkSecretFingerprint()
	if fp != sinkKeySHA256(rotatedSinkKey) && fp != sinkKeySHA256(rotatedSinkKeyB) {
		t.Errorf("live fingerprint = %q, want one of the two rotated keys", fp)
	}
	if sent, failed, dead := sink.Stats(); sent != 20 || failed != 0 || dead != 0 {
		t.Errorf("Stats = (%d, %d, %d), want (20, 0, 0)", sent, failed, dead)
	}
}

func TestRotateSinkSecretDoesNotPersistAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	auditDir := filepath.Join(dir, "audit")
	al, err := NewAuditLog(auditDir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	t.Cleanup(func() { _ = al.Close() })
	l, _ := newSinkTestLedger(t, dir, WithAuditLog(al))

	cap := &sinkCapture{}
	srv := httptest.NewServer(http.HandlerFunc(cap.handler))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sink, err := StartAuditSink(ctx, l, sinkTestConfig(srv.URL))
	if err != nil {
		t.Fatalf("StartAuditSink: %v", err)
	}
	mustPostSink(t, l, "rotate-restart-1")
	waitForSink(t, 5*time.Second, "1 delivery", func() bool { return cap.count() == 1 })

	if err := sink.RotateSinkSecret([]byte(rotatedSinkKey)); err != nil {
		t.Fatalf("RotateSinkSecret: %v", err)
	}
	mustPostSink(t, l, "rotate-restart-2")
	waitForSink(t, 5*time.Second, "2 deliveries", func() bool { return cap.count() == 2 })
	bodies, sigs, _, _ := cap.snapshot()
	if sigs[1] != sinkSigFor(bodies[1], rotatedSinkKey) {
		t.Errorf("second delivery does not verify with the rotated key")
	}
	sink.Stop()

	// A fresh sink restarts from the persisted cursor but with the
	// construction-time key: rotation does not survive a restart.
	sink2, err := StartAuditSink(ctx, l, sinkTestConfig(srv.URL))
	if err != nil {
		t.Fatalf("StartAuditSink (restart): %v", err)
	}
	defer sink2.Stop()
	if got := sink2.SinkSecretFingerprint(); got != sinkKeySHA256(testSinkKey) {
		t.Errorf("restarted sink fingerprint = %q, want the construction key", got)
	}
	mustPostSink(t, l, "rotate-restart-3")
	waitForSink(t, 5*time.Second, "3 deliveries", func() bool { return cap.count() == 3 })
	bodies, sigs, _, _ = cap.snapshot()
	if sigs[2] != sinkSigFor(bodies[2], testSinkKey) {
		t.Errorf("post-restart delivery does not verify with the construction key")
	}
}

func TestRotateSinkSecretInputIsCloned(t *testing.T) {
	cap := &sinkCapture{}
	l, sink, _ := startSinkTestLedger(t, cap, sinkTestConfig(""))

	// Mutating the caller's slice after rotation must not rewrite the
	// live key.
	key := []byte(rotatedSinkKey)
	if err := sink.RotateSinkSecret(key); err != nil {
		t.Fatalf("RotateSinkSecret: %v", err)
	}
	for i := range key {
		key[i] = 'x'
	}
	if got := sink.SinkSecretFingerprint(); got != sinkKeySHA256(rotatedSinkKey) {
		t.Fatalf("fingerprint moved after the caller mutated its slice: %q", got)
	}
	mustPostSink(t, l, "rotate-clone-1")
	waitForSink(t, 5*time.Second, "1 delivery", func() bool { return cap.count() == 1 })
	bodies, sigs, _, _ := cap.snapshot()
	if sigs[0] != sinkSigFor(bodies[0], rotatedSinkKey) {
		t.Errorf("delivery does not verify with the rotated key after caller mutation")
	}
}
