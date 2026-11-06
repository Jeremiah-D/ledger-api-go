package ledger

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Outbound webhook delivery-record ring capacity runtime revision
// (LG-336): SetWebhookMaxRecords revises the ring capacity in place —
// tightening trims the oldest records immediately, widening takes
// effect from the next append — with no restart, no endpoint
// teardown, and no audit spam for no-op revisions. Tests reuse
// deliveryAmendTestLedger / auditEventsByOp / auditDetailFloat from
// the LG-304 / LG-335 suites and webhookTestEndpoint /
// webhookTestLedger / deliveryRecordFor from ledger_webhook_test.go.

// maxRecordsTestDispatcher builds a dispatcher with one registered
// endpoint and no delivery traffic; tests drive appendRecord
// directly for determinism.
func maxRecordsTestDispatcher(t *testing.T) *WebhookDispatcher {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	t.Cleanup(srv.Close)
	d, err := NewWebhookDispatcher(webhookTestEndpoint(t, "risk", srv.URL, mustRotatingKey(t, "primary", "s3cr3t")))
	if err != nil {
		t.Fatalf("NewWebhookDispatcher: %v", err)
	}
	t.Cleanup(func() { d.Stop() })
	return d
}

// appendMaxRecordsTestRecord appends one delivery record for the
// "risk" endpoint and returns its handle.
func appendMaxRecordsTestRecord(d *WebhookDispatcher, seq uint64) *DeliveryRecord {
	return d.appendRecord(DeliveryRecord{
		EventSeq: seq, Endpoint: "risk", Status: "pending", At: time.Now(),
	})
}

// maxRecordsSeqs returns the event seqs of the dispatcher's records,
// latest first.
func maxRecordsSeqs(d *WebhookDispatcher) []uint64 {
	recs := d.Deliveries(WebhookDeliveryQuery{Limit: 100})
	seqs := make([]uint64, len(recs))
	for i, r := range recs {
		seqs[i] = r.EventSeq
	}
	return seqs
}

func TestSetWebhookMaxRecordsRejectsNonPositiveAndKeepsLive(t *testing.T) {
	d := maxRecordsTestDispatcher(t)
	for _, n := range []int{0, -1, -1024} {
		if got, err := d.SetMaxRecords(n); !errors.Is(err, ErrInvalidWebhookMaxRecords) || got != DefaultWebhookMaxRecords {
			t.Fatalf("SetMaxRecords(%d) = (%d, %v), want (%d, ErrInvalidWebhookMaxRecords)", n, got, err, DefaultWebhookMaxRecords)
		}
	}
	if got := d.MaxRecords(); got != DefaultWebhookMaxRecords {
		t.Fatalf("MaxRecords() = %d after rejected revisions, want %d", got, DefaultWebhookMaxRecords)
	}

	l, _, _ := deliveryAmendTestLedger(t, d)
	for _, n := range []int{0, -5} {
		if got, err := l.SetWebhookMaxRecords(n); !errors.Is(err, ErrInvalidWebhookMaxRecords) || got != DefaultWebhookMaxRecords {
			t.Fatalf("Ledger.SetWebhookMaxRecords(%d) = (%d, %v), want (%d, ErrInvalidWebhookMaxRecords)", n, got, err, DefaultWebhookMaxRecords)
		}
	}
	if got, ok := l.WebhookMaxRecords(); !ok || got != DefaultWebhookMaxRecords {
		t.Fatalf("Ledger.WebhookMaxRecords() = (%d, %v) after rejected revisions, want (%d, true)", got, ok, DefaultWebhookMaxRecords)
	}
}

func TestSetWebhookMaxRecordsSameValueNoOpNoAudit(t *testing.T) {
	d := maxRecordsTestDispatcher(t)
	l, al, dir := deliveryAmendTestLedger(t, d)
	prev, err := l.SetWebhookMaxRecords(DefaultWebhookMaxRecords)
	if err != nil || prev != DefaultWebhookMaxRecords {
		t.Fatalf("SetWebhookMaxRecords(%d) = (%d, %v), want (%d, nil)", DefaultWebhookMaxRecords, prev, err, DefaultWebhookMaxRecords)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close audit log: %v", err)
	}
	if events := auditEventsByOp(t, dir)["webhook_max_records_retuned"]; len(events) != 0 {
		t.Fatalf("retuned events = %d for a same-value revision, want 0", len(events))
	}
}

func TestSetWebhookMaxRecordsWidenKeepsRecordsPastOldCapacity(t *testing.T) {
	d := maxRecordsTestDispatcher(t)
	if prev, err := d.SetMaxRecords(3); err != nil || prev != DefaultWebhookMaxRecords {
		t.Fatalf("SetMaxRecords(3) = (%d, %v), want (%d, nil)", prev, err, DefaultWebhookMaxRecords)
	}
	appendMaxRecordsTestRecord(d, 1)
	appendMaxRecordsTestRecord(d, 2)
	appendMaxRecordsTestRecord(d, 3)
	appendMaxRecordsTestRecord(d, 4) // rolls seq 1 off at capacity 3
	if got := maxRecordsSeqs(d); len(got) != 3 || got[0] != 4 || got[2] != 2 {
		t.Fatalf("seqs at capacity 3 = %v, want [4 3 2]", got)
	}
	// Widen: the next appends stop trimming until the ring reaches
	// the new capacity — records already rolled off stay gone.
	if prev, err := d.SetMaxRecords(5); err != nil || prev != 3 {
		t.Fatalf("SetMaxRecords(5) = (%d, %v), want (3, nil)", prev, err)
	}
	appendMaxRecordsTestRecord(d, 5)
	appendMaxRecordsTestRecord(d, 6)
	if got := maxRecordsSeqs(d); len(got) != 5 || got[0] != 6 || got[4] != 2 {
		t.Fatalf("seqs after widening to 5 = %v, want [6 5 4 3 2]", got)
	}
}

func TestSetWebhookMaxRecordsTightenTrimsOldestImmediately(t *testing.T) {
	d := maxRecordsTestDispatcher(t)
	if _, err := d.SetMaxRecords(5); err != nil {
		t.Fatalf("SetMaxRecords(5): %v", err)
	}
	for seq := uint64(1); seq <= 5; seq++ {
		appendMaxRecordsTestRecord(d, seq)
	}
	if prev, err := d.SetMaxRecords(2); err != nil || prev != 5 {
		t.Fatalf("SetMaxRecords(2) = (%d, %v), want (5, nil)", prev, err)
	}
	// The next query already sees the new capacity: only the two
	// newest records survive, oldest-first trimming.
	if got := maxRecordsSeqs(d); len(got) != 2 || got[0] != 5 || got[1] != 4 {
		t.Fatalf("seqs after tightening to 2 = %v, want [5 4]", got)
	}
	if got := d.MaxRecords(); got != 2 {
		t.Fatalf("MaxRecords() = %d after tightening, want 2", got)
	}
}

func TestSetWebhookMaxRecordsTightenToOneKeepsNewest(t *testing.T) {
	d := maxRecordsTestDispatcher(t)
	if _, err := d.SetMaxRecords(3); err != nil {
		t.Fatalf("SetMaxRecords(3): %v", err)
	}
	appendMaxRecordsTestRecord(d, 1)
	appendMaxRecordsTestRecord(d, 2)
	appendMaxRecordsTestRecord(d, 3)
	if _, err := d.SetMaxRecords(1); err != nil {
		t.Fatalf("SetMaxRecords(1): %v", err)
	}
	if got := maxRecordsSeqs(d); len(got) != 1 || got[0] != 3 {
		t.Fatalf("seqs after tightening to 1 = %v, want [3]", got)
	}
}

func TestSetWebhookMaxRecordsInFlightRecordNotEvicted(t *testing.T) {
	d := maxRecordsTestDispatcher(t)
	if _, err := d.SetMaxRecords(2); err != nil {
		t.Fatalf("SetMaxRecords(2): %v", err)
	}
	appendMaxRecordsTestRecord(d, 1)
	// The in-flight record is the newest: tightening to 1 must keep
	// it, and its appendRecord handle must stay usable for a later
	// status update.
	inFlight := appendMaxRecordsTestRecord(d, 2)
	if _, err := d.SetMaxRecords(1); err != nil {
		t.Fatalf("SetMaxRecords(1): %v", err)
	}
	d.updateRecord(inFlight, func(r *DeliveryRecord) {
		r.Status = WebhookDeliveryDelivered
		r.Attempts = 1
	})
	recs := d.Deliveries(WebhookDeliveryQuery{Limit: 100})
	if len(recs) != 1 || recs[0].EventSeq != 2 || recs[0].Status != WebhookDeliveryDelivered || recs[0].Attempts != 1 {
		t.Fatalf("records after tightening = %+v, want the in-flight seq 2 delivered", recs)
	}
}

func TestSetWebhookMaxRecordsNoDispatcher(t *testing.T) {
	l := New()
	if got, err := l.SetWebhookMaxRecords(10); !errors.Is(err, ErrWebhooksNotConfigured) || got != 0 {
		t.Fatalf("SetWebhookMaxRecords without dispatcher = (%d, %v), want (0, ErrWebhooksNotConfigured)", got, err)
	}
	if got, ok := l.WebhookMaxRecords(); ok || got != 0 {
		t.Fatalf("WebhookMaxRecords() without dispatcher = (%d, %v), want (0, false)", got, ok)
	}
}

func TestSetWebhookMaxRecordsAuditsPreviousAndCurrent(t *testing.T) {
	d := maxRecordsTestDispatcher(t)
	l, al, dir := deliveryAmendTestLedger(t, d)
	prev, err := l.SetWebhookMaxRecords(2048)
	if err != nil || prev != DefaultWebhookMaxRecords {
		t.Fatalf("SetWebhookMaxRecords(2048) = (%d, %v), want (%d, nil)", prev, err, DefaultWebhookMaxRecords)
	}
	if got, ok := l.WebhookMaxRecords(); !ok || got != 2048 {
		t.Fatalf("WebhookMaxRecords() = (%d, %v), want (2048, true)", got, ok)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close audit log: %v", err)
	}
	events := auditEventsByOp(t, dir)["webhook_max_records_retuned"]
	if len(events) != 1 {
		t.Fatalf("retuned events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Actor != "SetWebhookMaxRecords" {
		t.Fatalf("event Actor = %q, want SetWebhookMaxRecords", ev.Actor)
	}
	if got := auditDetailFloat(t, ev, "previous_max_records"); got != float64(DefaultWebhookMaxRecords) {
		t.Fatalf("previous_max_records = %v, want %d", got, DefaultWebhookMaxRecords)
	}
	if got := auditDetailFloat(t, ev, "current_max_records"); got != 2048 {
		t.Fatalf("current_max_records = %v, want 2048", got)
	}
	if ev.VersionBefore != ev.VersionAfter {
		t.Fatalf("version moved %d -> %d, want no bump", ev.VersionBefore, ev.VersionAfter)
	}
}

func TestSetWebhookMaxRecordsCtxRecordsOperator(t *testing.T) {
	d := maxRecordsTestDispatcher(t)
	l, al, dir := deliveryAmendTestLedger(t, d)
	ctx := ContextWithActor(context.Background(), "ops-alan")
	if _, err := l.SetWebhookMaxRecordsCtx(ctx, 512); err != nil {
		t.Fatalf("SetWebhookMaxRecordsCtx: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close audit log: %v", err)
	}
	events := auditEventsByOp(t, dir)["webhook_max_records_retuned"]
	if len(events) != 1 {
		t.Fatalf("retuned events = %d, want 1", len(events))
	}
	if events[0].Actor != "ops-alan" {
		t.Fatalf("event Actor = %q, want ops-alan", events[0].Actor)
	}
}

func TestSetWebhookMaxRecordsConcurrentRevisionAndAccessRaceFree(t *testing.T) {
	d := maxRecordsTestDispatcher(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if _, err := d.SetMaxRecords(1 + (n+j)%64); err != nil {
					t.Errorf("SetMaxRecords: %v", err)
					return
				}
			}
		}(i)
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				appendMaxRecordsTestRecord(d, uint64(n*100+j))
				_ = d.Deliveries(WebhookDeliveryQuery{Limit: 10})
			}
		}(i)
	}
	wg.Wait()
	live := d.MaxRecords()
	if live < 1 || live > 64 {
		t.Fatalf("MaxRecords() = %d after concurrent revisions, want 1..64", live)
	}
	if n := len(d.Deliveries(WebhookDeliveryQuery{Limit: 100})); n > live {
		t.Fatalf("ring holds %d records at capacity %d, want <= capacity", n, live)
	}
}

func TestSetWebhookMaxRecordsGetterFollowsLiveValue(t *testing.T) {
	d := maxRecordsTestDispatcher(t)
	if got := d.MaxRecords(); got != DefaultWebhookMaxRecords {
		t.Fatalf("MaxRecords() on a fresh dispatcher = %d, want %d (restart restores the default)", got, DefaultWebhookMaxRecords)
	}
	if _, err := d.SetMaxRecords(512); err != nil {
		t.Fatalf("SetMaxRecords(512): %v", err)
	}
	if got := d.MaxRecords(); got != 512 {
		t.Fatalf("MaxRecords() = %d after revision, want 512", got)
	}
	l := webhookTestLedger(t, d)
	if got, ok := l.WebhookMaxRecords(); !ok || got != 512 {
		t.Fatalf("Ledger.WebhookMaxRecords() = (%d, %v), want (512, true)", got, ok)
	}
	if _, err := l.SetWebhookMaxRecords(2048); err != nil {
		t.Fatalf("SetWebhookMaxRecords(2048): %v", err)
	}
	if got, ok := l.WebhookMaxRecords(); !ok || got != 2048 {
		t.Fatalf("Ledger.WebhookMaxRecords() = (%d, %v) after revision, want (2048, true)", got, ok)
	}
}

func TestSetWebhookMaxRecordsDoesNotRestartEndpointWorker(t *testing.T) {
	cap := &timedWebhookCapture{slow: 100 * time.Millisecond}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	cfg := webhookTestEndpoint(t, "risk", srv.URL, mustRotatingKey(t, "primary", "s3cr3t"))
	d, err := NewWebhookDispatcher(cfg)
	if err != nil {
		t.Fatalf("NewWebhookDispatcher: %v", err)
	}
	t.Cleanup(func() { d.Stop() })
	l := webhookTestLedger(t, d)

	// Two events: the first is in flight (slow receiver) while the
	// second waits in the queue.
	mustPostWebhookEntry(t, l, "max-records-1")
	mustPostWebhookEntry(t, l, "max-records-2")
	// Revise the ring capacity mid-delivery: the worker must keep
	// going — no restart, no dropped event, no counter reset.
	if prev, err := l.SetWebhookMaxRecords(3); err != nil || prev != DefaultWebhookMaxRecords {
		t.Fatalf("SetWebhookMaxRecords(3) = (%d, %v), want (%d, nil)", prev, err, DefaultWebhookMaxRecords)
	}
	waitForAttempts(t, cap, 2, "both deliveries after the revision")
	delivered, _, _, _ := d.Stats()
	if delivered != 2 {
		t.Fatalf("Stats delivered = %d after the revision, want 2 (counters keep accumulating)", delivered)
	}
	recs := d.Deliveries(WebhookDeliveryQuery{Limit: 100})
	if len(recs) != 2 {
		t.Fatalf("records after the revision = %d, want 2 (both deliveries kept in the tightened ring)", len(recs))
	}
}
