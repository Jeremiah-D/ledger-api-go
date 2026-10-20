package ledger

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// watchTestLedger builds a ledger with an outbox in a temp dir, and
// returns the ledger plus the outbox directory for event reads.
func watchTestLedger(t *testing.T) (*Ledger, string) {
	t.Helper()
	dir := t.TempDir()
	ob, err := NewOutbox(dir)
	if err != nil {
		t.Fatalf("NewOutbox: %v", err)
	}
	t.Cleanup(func() { _ = ob.Close() })
	return New(WithOutbox(ob)), dir
}

// fireWatchBreach posts a movement that takes acct's USD balance below
// the low watch level, returning the posting's trace ID.
func fireWatchBreach(t *testing.T, l *Ledger, acct AccountID) {
	t.Helper()
	seedBalanceIn(t, l, acct, 10000, "USD")
	if err := l.SetBalanceWatch(acct, "USD", BalanceWatchConfig{LowCents: int64ptr(5000)}); err != nil {
		t.Fatalf("SetBalanceWatch: %v", err)
	}
	if _, _, err := l.Post(JournalEntry{
		ID: "watch-breach-post", DebitAccount: "funding-out", CreditAccount: acct,
		AmountCents: 6000, Currency: "USD",
	}); err != nil {
		t.Fatalf("breach posting: %v", err)
	}
}

func breachOutboxEvents(t *testing.T, dir string) []OutboxEvent {
	t.Helper()
	evs, _, _, err := ReadOutboxEvents(dir, OutboxQuery{
		Types: []string{OutboxBalanceThresholdBreached}, Limit: 100,
	})
	if err != nil {
		t.Fatalf("ReadOutboxEvents: %v", err)
	}
	return evs
}

func TestBalanceWatchBreachEmitsOutboxEvent(t *testing.T) {
	l, dir := watchTestLedger(t)
	fireWatchBreach(t, l, "acct-low")

	evs := breachOutboxEvents(t, dir)
	if len(evs) != 1 {
		t.Fatalf("breach events = %d, want 1", len(evs))
	}
	ev := evs[0]
	if ev.Account != "acct-low" || ev.AmountCents != 4000 || ev.Currency != "USD" {
		t.Errorf("event = %+v, want account acct-low balance 4000 USD", ev)
	}
	if ev.Details["direction"] != "low" || ev.Details["threshold_cents"] != float64(5000) ||
		ev.Details["balance_cents"] != float64(4000) {
		t.Errorf("event details = %v, want low/5000/4000", ev.Details)
	}
	if ev.Details["trace_id"] != "watch-breach-post" || ev.Details["actor"] != "Post" {
		t.Errorf("event details = %v, want trace watch-breach-post actor Post", ev.Details)
	}
	if ev.Seq == 0 {
		t.Error("event.Seq = 0, want an assigned outbox seq")
	}
}

func TestBalanceWatchBreachNoOutboxIsSilent(t *testing.T) {
	// No outbox attached: the breach still records its audit event and
	// counter, and the posting path is untouched.
	l := New()
	fireWatchBreach(t, l, "acct-nooutbox")
	if got := l.BalanceWatchBreachCount(); got != 1 {
		t.Errorf("breach count = %d, want 1", got)
	}
	if got := l.BalanceIn("acct-nooutbox", "USD"); got != 4000 {
		t.Errorf("balance = %d, want 4000", got)
	}
}

func TestBalanceWatchBreachEdgeTriggeredOutbox(t *testing.T) {
	l, dir := watchTestLedger(t)
	fireWatchBreach(t, l, "acct-edge")

	// A second posting while the balance stays below the low level is
	// silent: exactly one breach event, no re-fire.
	if _, _, err := l.Post(JournalEntry{
		ID: "watch-second", DebitAccount: "funding-out", CreditAccount: "acct-edge",
		AmountCents: 1000, Currency: "USD",
	}); err != nil {
		t.Fatalf("second posting: %v", err)
	}
	if evs := breachOutboxEvents(t, dir); len(evs) != 1 {
		t.Fatalf("breach events after second posting = %d, want 1", len(evs))
	}

	// Recovery re-arms: a posting back inside the band, then another
	// fall below, fires a second event.
	if _, _, err := l.Post(JournalEntry{
		ID: "watch-recover", DebitAccount: "acct-edge", CreditAccount: "funding-in",
		AmountCents: 10000, Currency: "USD",
	}); err != nil {
		t.Fatalf("recovery posting: %v", err)
	}
	if _, _, err := l.Post(JournalEntry{
		ID: "watch-breach-2", DebitAccount: "funding-out", CreditAccount: "acct-edge",
		AmountCents: 10000, Currency: "USD",
	}); err != nil {
		t.Fatalf("second breach posting: %v", err)
	}
	if evs := breachOutboxEvents(t, dir); len(evs) != 2 {
		t.Fatalf("breach events after re-arm = %d, want 2", len(evs))
	}
}

func TestBalanceWatchHighBreachEmitsOutboxEvent(t *testing.T) {
	l, dir := watchTestLedger(t)
	seedBalanceIn(t, l, "acct-high", 10000, "USD")
	if err := l.SetBalanceWatch("acct-high", "USD", BalanceWatchConfig{HighCents: int64ptr(12000)}); err != nil {
		t.Fatalf("SetBalanceWatch: %v", err)
	}
	if _, _, err := l.Post(JournalEntry{
		ID: "watch-high-post", DebitAccount: "acct-high", CreditAccount: "funding-in",
		AmountCents: 5000, Currency: "USD",
	}); err != nil {
		t.Fatalf("high breach posting: %v", err)
	}
	evs := breachOutboxEvents(t, dir)
	if len(evs) != 1 {
		t.Fatalf("breach events = %d, want 1", len(evs))
	}
	if evs[0].Details["direction"] != "high" || evs[0].AmountCents != 15000 {
		t.Errorf("event = %+v, want high/15000", evs[0])
	}
}

func TestBalanceWatchBreachWebhookDispatch(t *testing.T) {
	cap := &webhookCapture{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	keys := mustRotatingKey(t, "k1", "watch-secret")
	cfg := webhookTestEndpoint(t, "risk", srv.URL, keys)
	cfg.Events = []string{OutboxBalanceThresholdBreached}
	d, err := NewWebhookDispatcher(cfg)
	if err != nil {
		t.Fatalf("NewWebhookDispatcher: %v", err)
	}
	defer d.Stop()
	l := webhookTestLedger(t, d)

	fireWatchBreach(t, l, "acct-webhook")

	waitForWebhook(t, 5*time.Second, func() bool {
		dl, _, _, _ := d.Stats()
		return dl == 1
	}, "one breach delivery")

	got := cap.attempts()
	if len(got) != 1 {
		t.Fatalf("attempts = %d, want 1", len(got))
	}
	a := got[0]
	if a.typ != OutboxBalanceThresholdBreached {
		t.Errorf("X-Ledger-Event-Type = %q, want %q", a.typ, OutboxBalanceThresholdBreached)
	}
	if a.keyID != "k1" {
		t.Errorf("X-Ledger-Key-Id = %q, want k1", a.keyID)
	}
	if !keys.Verify(a.body, a.sig) {
		t.Error("webhook signature does not verify under the endpoint's key ring")
	}
	var ev OutboxEvent
	if err := json.Unmarshal(a.body, &ev); err != nil {
		t.Fatalf("webhook body: %v", err)
	}
	if ev.Type != OutboxBalanceThresholdBreached || ev.Account != "acct-webhook" ||
		ev.Details["direction"] != "low" {
		t.Errorf("webhook body event = %+v", ev)
	}
}

func TestBalanceWatchBreachWebhookFilterExcludes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	// The endpoint only wants transfer_posted: the breach must not
	// reach it.
	keys := mustRotatingKey(t, "k1", "watch-secret-2")
	cfg := webhookTestEndpoint(t, "transfers-only", srv.URL, keys)
	cfg.Events = []string{OutboxTransferPosted}
	d, err := NewWebhookDispatcher(cfg)
	if err != nil {
		t.Fatalf("NewWebhookDispatcher: %v", err)
	}
	defer d.Stop()
	l := webhookTestLedger(t, d)

	fireWatchBreach(t, l, "acct-filtered")

	// Give the dispatcher a beat to mis-deliver, then assert silence.
	time.Sleep(200 * time.Millisecond)
	dl, _, _, _ := d.Stats()
	if dl != 0 {
		t.Errorf("delivered = %d, want 0 (breach filtered out)", dl)
	}
	// The event is still in the outbox stream: filtering is a webhook
	// concern, not a stream concern.
	if got := l.BalanceWatchBreachCount(); got != 1 {
		t.Errorf("breach count = %d, want 1", got)
	}
}
