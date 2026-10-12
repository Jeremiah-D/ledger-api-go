package ledger

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func mustSetWatch(t *testing.T, l *Ledger, a AccountID, currency string, cfg BalanceWatchConfig) {
	t.Helper()
	if err := l.SetBalanceWatch(a, currency, cfg); err != nil {
		t.Fatalf("SetBalanceWatch: %v", err)
	}
}

func mustWatchPost(t *testing.T, l *Ledger, e JournalEntry) {
	t.Helper()
	if _, dup, err := l.Post(e); err != nil || dup {
		t.Fatalf("Post %s = dup=%v err=%v", e.ID, dup, err)
	}
}

// breachEvents collects balance_threshold_breached audit events from the
// log, failing the test on read errors.
func breachEvents(t *testing.T, al *AuditLog) []AuditEvent {
	t.Helper()
	now := time.Now().UTC().Format("2006-01-02")
	events, corrupt, err := ReadAuditLog(al.Dir(), now)
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt = %d, want 0", corrupt)
	}
	var out []AuditEvent
	for _, ev := range events {
		if ev.Op == "balance_threshold_breached" {
			out = append(out, ev)
		}
	}
	return out
}

func TestSetBalanceWatchValidation(t *testing.T) {
	l := New()
	if err := l.SetBalanceWatch("", "USD", BalanceWatchConfig{LowCents: int64ptr(100)}); !errors.Is(err, ErrEmptyWatchAccount) {
		t.Errorf("empty account = %v, want ErrEmptyWatchAccount", err)
	}
	if err := l.SetBalanceWatch("a", "US", BalanceWatchConfig{LowCents: int64ptr(100)}); !errors.Is(err, ErrInvalidCurrency) {
		t.Errorf("bad currency = %v, want ErrInvalidCurrency", err)
	}
	if err := l.SetBalanceWatch("a", "USD", BalanceWatchConfig{}); !errors.Is(err, ErrBalanceWatchEmpty) {
		t.Errorf("empty config = %v, want ErrBalanceWatchEmpty", err)
	}
	if err := l.SetBalanceWatch("a", "USD", BalanceWatchConfig{LowCents: int64ptr(500), HighCents: int64ptr(500)}); !errors.Is(err, ErrInvalidBalanceWatch) {
		t.Errorf("low==high = %v, want ErrInvalidBalanceWatch", err)
	}
	if err := l.SetBalanceWatch("a", "USD", BalanceWatchConfig{LowCents: int64ptr(900), HighCents: int64ptr(500)}); !errors.Is(err, ErrInvalidBalanceWatch) {
		t.Errorf("low>high = %v, want ErrInvalidBalanceWatch", err)
	}
	// A failed validation records nothing.
	if _, ok := l.GetBalanceWatch("a", "USD"); ok {
		t.Error("failed SetBalanceWatch recorded a watch")
	}
}

func TestBalanceWatchLowBreachEdgeTriggered(t *testing.T) {
	al := mustAuditLog(t)
	defer al.Close()
	l := New(WithAuditLog(al))
	// Seed inside the band first: setting a watch while the balance is
	// already outside never backfills, so the seed must come first.
	mustWatchPost(t, l, JournalEntry{ID: "seed", DebitAccount: "cust-1", CreditAccount: "bank", AmountCents: 1000, Currency: "USD"})
	mustSetWatch(t, l, "cust-1", "USD", BalanceWatchConfig{LowCents: int64ptr(500)})

	// Cross the level.
	mustWatchPost(t, l, JournalEntry{ID: "e1", DebitAccount: "bank", CreditAccount: "cust-1", AmountCents: 600, Currency: "USD"})
	if got := l.BalanceWatchBreachCount(); got != 1 {
		t.Fatalf("BreachCount = %d, want 1", got)
	}

	// Further postings while below stay silent.
	mustWatchPost(t, l, JournalEntry{ID: "e2", DebitAccount: "bank", CreditAccount: "cust-1", AmountCents: 100, Currency: "USD"})
	if got := l.BalanceWatchBreachCount(); got != 1 {
		t.Errorf("BreachCount after silent posting = %d, want 1", got)
	}

	// Recover to at/above the level: re-arms silently.
	mustWatchPost(t, l, JournalEntry{ID: "e3", DebitAccount: "cust-1", CreditAccount: "bank", AmountCents: 200, Currency: "USD"})
	if got := l.BalanceWatchBreachCount(); got != 1 {
		t.Errorf("BreachCount after recovery = %d, want 1", got)
	}

	// Cross again: fires a second time.
	mustWatchPost(t, l, JournalEntry{ID: "e4", DebitAccount: "bank", CreditAccount: "cust-1", AmountCents: 100, Currency: "USD"})
	if got := l.BalanceWatchBreachCount(); got != 2 {
		t.Errorf("BreachCount after re-breach = %d, want 2", got)
	}

	// The audit log carries the structured events (flushed on Close).
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := breachEvents(t, al)
	if len(events) != 2 {
		t.Fatalf("breach events = %d, want 2", len(events))
	}
	ev := events[0]
	if ev.Details["direction"] != "low" {
		t.Errorf("direction = %v, want low", ev.Details["direction"])
	}
	if ev.Details["threshold_cents"] != float64(500) || ev.Details["balance_cents"] != float64(400) {
		t.Errorf("threshold/balance = %v/%v, want 500/400", ev.Details["threshold_cents"], ev.Details["balance_cents"])
	}
	if ev.Details["currency"] != "USD" || ev.Actor != "Post" || ev.TraceID != "e1" {
		t.Errorf("event = %+v, want currency USD actor Post trace e1", ev.Details)
	}
	// Advisory: the alert brackets the committed state with zero version
	// movement.
	if ev.VersionBefore != ev.VersionAfter {
		t.Errorf("version bracket = %d/%d, want equal", ev.VersionBefore, ev.VersionAfter)
	}
}

func TestBalanceWatchHighBreach(t *testing.T) {
	al := mustAuditLog(t)
	defer al.Close()
	l := New(WithAuditLog(al))
	mustWatchPost(t, l, JournalEntry{ID: "seed", DebitAccount: "treasury", CreditAccount: "bank", AmountCents: 9000, Currency: "USD"})
	mustSetWatch(t, l, "treasury", "USD", BalanceWatchConfig{HighCents: int64ptr(10000)})

	mustWatchPost(t, l, JournalEntry{ID: "e2", DebitAccount: "treasury", CreditAccount: "bank", AmountCents: 2000, Currency: "USD"})
	if got := l.BalanceWatchBreachCount(); got != 1 {
		t.Fatalf("BreachCount = %d, want 1", got)
	}

	// Stays above: silent. Recovers: re-arms. Crosses again: fires.
	mustWatchPost(t, l, JournalEntry{ID: "e3", DebitAccount: "treasury", CreditAccount: "bank", AmountCents: 500, Currency: "USD"})
	mustWatchPost(t, l, JournalEntry{ID: "e4", DebitAccount: "bank", CreditAccount: "treasury", AmountCents: 2000, Currency: "USD"})
	mustWatchPost(t, l, JournalEntry{ID: "e5", DebitAccount: "treasury", CreditAccount: "bank", AmountCents: 1000, Currency: "USD"})
	if got := l.BalanceWatchBreachCount(); got != 2 {
		t.Fatalf("BreachCount = %d, want 2", got)
	}

	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := breachEvents(t, al)
	if len(events) != 2 {
		t.Fatalf("breach events = %d, want 2", len(events))
	}
	if events[0].Details["direction"] != "high" {
		t.Errorf("direction = %v, want high", events[0].Details["direction"])
	}
	if events[0].Details["threshold_cents"] != float64(10000) || events[0].Details["balance_cents"] != float64(11000) {
		t.Errorf("threshold/balance = %v/%v, want 10000/11000", events[0].Details["threshold_cents"], events[0].Details["balance_cents"])
	}
}

func TestBalanceWatchBothSides(t *testing.T) {
	l := New()
	// Seed inside the band before setting the watch.
	mustWatchPost(t, l, JournalEntry{ID: "seed", DebitAccount: "ops", CreditAccount: "bank", AmountCents: 500, Currency: "USD"})
	mustSetWatch(t, l, "ops", "USD", BalanceWatchConfig{LowCents: int64ptr(100), HighCents: int64ptr(1000)})

	w, ok := l.GetBalanceWatch("ops", "USD")
	if !ok || w.LowCents == nil || *w.LowCents != 100 || w.HighCents == nil || *w.HighCents != 1000 {
		t.Fatalf("GetBalanceWatch = %+v, want low=100 high=1000", w)
	}

	// Cross low, then recover, then cross high: two events, one per side.
	mustWatchPost(t, l, JournalEntry{ID: "e1", DebitAccount: "bank", CreditAccount: "ops", AmountCents: 450, Currency: "USD"})
	if got := l.BalanceWatchBreachCount(); got != 1 {
		t.Fatalf("BreachCount after low = %d, want 1", got)
	}
	mustWatchPost(t, l, JournalEntry{ID: "e2", DebitAccount: "ops", CreditAccount: "bank", AmountCents: 50, Currency: "USD"})
	mustWatchPost(t, l, JournalEntry{ID: "e3", DebitAccount: "ops", CreditAccount: "bank", AmountCents: 1500, Currency: "USD"})
	if got := l.BalanceWatchBreachCount(); got != 2 {
		t.Fatalf("BreachCount after high = %d, want 2", got)
	}
}

func TestBalanceWatchTransferAndCapture(t *testing.T) {
	al := mustAuditLog(t)
	defer al.Close()
	l := New(WithAuditLog(al))
	// Seed inside both bands before setting the watches.
	mustWatchPost(t, l, JournalEntry{ID: "seed1", DebitAccount: "payer", CreditAccount: "bank", AmountCents: 2000, Currency: "USD"})
	mustWatchPost(t, l, JournalEntry{ID: "seed2", DebitAccount: "payee", CreditAccount: "bank", AmountCents: 500, Currency: "USD"})
	mustSetWatch(t, l, "payer", "USD", BalanceWatchConfig{LowCents: int64ptr(500)})
	mustSetWatch(t, l, "payee", "USD", BalanceWatchConfig{HighCents: int64ptr(1000)})

	// Transfer: payer drops below low, payee rises above high — both fire.
	if _, err := l.PostTransfer(Transfer{ID: "tr1", From: "payer", To: "payee", AmountCents: 1600, Currency: "USD"}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if got := l.BalanceWatchBreachCount(); got != 2 {
		t.Fatalf("BreachCount after transfer = %d, want 2", got)
	}

	// A hold capture's settlement leg crosses the payee's high again
	// after recovery: fires once more, attributed to Capture.
	mustWatchPost(t, l, JournalEntry{ID: "e2", DebitAccount: "bank", CreditAccount: "payee", AmountCents: 1300, Currency: "USD"})
	mustWatchPost(t, l, JournalEntry{ID: "seed3", DebitAccount: "payer2", CreditAccount: "bank", AmountCents: 1000, Currency: "USD"})
	if _, _, err := l.Hold(Hold{ID: "h1", Account: "payer2", AmountCents: 300, Currency: "USD", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if _, err := l.Capture(Capture{ID: "c1", HoldID: "h1", To: "payee", AmountCents: 300}); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if got := l.BalanceWatchBreachCount(); got != 3 {
		t.Fatalf("BreachCount after capture = %d, want 3", got)
	}

	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := breachEvents(t, al)
	if len(events) != 3 {
		t.Fatalf("breach events = %d, want 3", len(events))
	}
	dirs := map[string]string{}
	for _, ev := range events[:2] {
		dirs[ev.Details["account"].(string)] = ev.Details["direction"].(string)
		if ev.Actor != "PostTransfer" || ev.TraceID != "tr1" {
			t.Errorf("transfer breach actor/trace = %s/%s, want PostTransfer/tr1", ev.Actor, ev.TraceID)
		}
	}
	if dirs["payer"] != "low" || dirs["payee"] != "high" {
		t.Errorf("directions = %v, want payer=low payee=high", dirs)
	}
	last := events[2]
	if last.Actor != "Capture" || last.Details["direction"] != "high" {
		t.Errorf("capture breach = actor %s direction %v, want Capture/high", last.Actor, last.Details["direction"])
	}
}

func TestBalanceWatchRejectedOpsDontEvaluate(t *testing.T) {
	al := mustAuditLog(t)
	defer al.Close()
	l := New(WithAuditLog(al), WithOverdraftProtection("payer"))
	mustWatchPost(t, l, JournalEntry{ID: "seed", DebitAccount: "payer", CreditAccount: "bank", AmountCents: 1000, Currency: "USD"})
	mustSetWatch(t, l, "payer", "USD", BalanceWatchConfig{LowCents: int64ptr(500)})

	// Frozen rejection: books nothing, evaluates nothing.
	l.Freeze("payer")
	if _, _, err := l.Post(JournalEntry{ID: "rej1", DebitAccount: "bank", CreditAccount: "payer", AmountCents: 600, Currency: "USD"}); !errors.Is(err, ErrAccountFrozen) {
		t.Fatalf("frozen post = %v, want ErrAccountFrozen", err)
	}
	l.Unfreeze("payer")

	// Overdraft rejection: books nothing, evaluates nothing.
	if _, _, err := l.Post(JournalEntry{ID: "rej2", DebitAccount: "bank", CreditAccount: "payer", AmountCents: 1500, Currency: "USD"}); !errors.Is(err, ErrAccountOverdraft) {
		t.Fatalf("overdraft post = %v, want ErrAccountOverdraft", err)
	}

	// Validation rejection: books nothing, evaluates nothing.
	if _, _, err := l.Post(JournalEntry{ID: "rej3", DebitAccount: "bank", CreditAccount: "payer", AmountCents: -5, Currency: "USD"}); !errors.Is(err, ErrNonPositiveAmount) {
		t.Fatalf("invalid post = %v, want ErrNonPositiveAmount", err)
	}

	if got := l.BalanceWatchBreachCount(); got != 0 {
		t.Errorf("BreachCount after rejections = %d, want 0", got)
	}
	// Close flushes the async writer, so the empty read below is a real
	// assertion, not a race with the queue.
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if events := breachEvents(t, al); len(events) != 0 {
		t.Errorf("breach events after rejections = %d, want 0", len(events))
	}
}

func TestBalanceWatchCRUD(t *testing.T) {
	al := mustAuditLog(t)
	defer al.Close()
	l := New(WithAuditLog(al))

	mustSetWatch(t, l, "a", "USD", BalanceWatchConfig{LowCents: int64ptr(100), HighCents: int64ptr(900)})
	mustSetWatch(t, l, "b", "EUR", BalanceWatchConfig{HighCents: int64ptr(500)})

	watches := l.ListBalanceWatches()
	if len(watches) != 2 || watches[0].Account != "a" || watches[1].Account != "b" {
		t.Fatalf("ListBalanceWatches = %+v, want [a b]", watches)
	}
	if _, ok := l.GetBalanceWatch("missing", "USD"); ok {
		t.Error("GetBalanceWatch(missing) = true, want false")
	}

	// Clearing a watch that was never set is a silent no-op.
	l.ClearBalanceWatch("missing", "USD")

	l.ClearBalanceWatch("a", "USD")
	if _, ok := l.GetBalanceWatch("a", "USD"); ok {
		t.Error("GetBalanceWatch after clear = true, want false")
	}

	// CRUD is audited: two sets and one clear (flushed on Close).
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	now := time.Now().UTC().Format("2006-01-02")
	events, _, err := ReadAuditLog(al.Dir(), now)
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	var sets, clears int
	for _, ev := range events {
		switch ev.Op {
		case "balance_watch_set":
			sets++
			if ev.Details["account"] == nil || ev.Details["currency"] == nil {
				t.Errorf("balance_watch_set missing account/currency: %+v", ev.Details)
			}
		case "balance_watch_cleared":
			clears++
		}
	}
	if sets != 2 || clears != 1 {
		t.Errorf("CRUD audit: sets=%d clears=%d, want 2/1", sets, clears)
	}
}

func TestBalanceWatchLegacyLowAPIInterop(t *testing.T) {
	l := New()
	// Legacy low-only API writes the same store...
	if err := l.SetLowBalanceThreshold("a", "USD", 500); err != nil {
		t.Fatalf("SetLowBalanceThreshold: %v", err)
	}
	w, ok := l.GetBalanceWatch("a", "USD")
	if !ok || w.LowCents == nil || *w.LowCents != 500 || w.HighCents != nil {
		t.Fatalf("watch after legacy set = %+v, want low=500 high=nil", w)
	}
	// ...and preserves a high side set through the new API.
	mustSetWatch(t, l, "a", "USD", BalanceWatchConfig{LowCents: int64ptr(500), HighCents: int64ptr(5000)})
	if err := l.SetLowBalanceThreshold("a", "USD", 600); err != nil {
		t.Fatalf("SetLowBalanceThreshold replace: %v", err)
	}
	w, _ = l.GetBalanceWatch("a", "USD")
	if *w.LowCents != 600 || w.HighCents == nil || *w.HighCents != 5000 {
		t.Fatalf("watch after legacy replace = %+v, want low=600 high=5000", w)
	}
	// Legacy clear removes only the low side when a high side exists.
	l.ClearLowBalanceThreshold("a", "USD")
	w, ok = l.GetBalanceWatch("a", "USD")
	if !ok || w.LowCents != nil || *w.HighCents != 5000 {
		t.Fatalf("watch after legacy clear = %+v, want low=nil high=5000", w)
	}
	// ...and removes the watch entirely when the low side was the only side.
	mustSetWatch(t, l, "b", "USD", BalanceWatchConfig{LowCents: int64ptr(100)})
	l.ClearLowBalanceThreshold("b", "USD")
	if _, ok := l.GetBalanceWatch("b", "USD"); ok {
		t.Error("watch b survives legacy clear of its only side")
	}
	// A legacy low threshold still fires through the unified engine with
	// the new event shape.
	al := mustAuditLog(t)
	defer al.Close()
	l2 := New(WithAuditLog(al))
	if err := l2.SetLowBalanceThreshold("c", "USD", 500); err != nil {
		t.Fatalf("SetLowBalanceThreshold: %v", err)
	}
	mustWatchPost(t, l2, JournalEntry{ID: "seed", DebitAccount: "c", CreditAccount: "bank", AmountCents: 1000, Currency: "USD"})
	mustWatchPost(t, l2, JournalEntry{ID: "e1", DebitAccount: "bank", CreditAccount: "c", AmountCents: 600, Currency: "USD"})
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := breachEvents(t, al)
	if len(events) != 1 || events[0].Details["direction"] != "low" {
		t.Fatalf("legacy low breach events = %d, want 1 with direction low", len(events))
	}
}

func TestParseBalanceWatches(t *testing.T) {
	got, err := ParseBalanceWatches("cust-123:USD:50000:,ops:EUR::-10000,treasury:USD:1000:5000000")
	if err != nil {
		t.Fatalf("ParseBalanceWatches: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("parsed = %d, want 3", len(got))
	}
	if got[0].LowCents == nil || *got[0].LowCents != 50000 || got[0].HighCents != nil {
		t.Errorf("watches[0] = %+v, want low=50000 high=nil", got[0])
	}
	if got[1].HighCents == nil || *got[1].HighCents != -10000 || got[1].LowCents != nil {
		t.Errorf("watches[1] = %+v, want low=nil high=-10000", got[1])
	}
	if *got[2].LowCents != 1000 || *got[2].HighCents != 5000000 {
		t.Errorf("watches[2] = %+v, want low=1000 high=5000000", got[2])
	}
	if _, err := ParseBalanceWatches(""); err != nil {
		t.Errorf("empty = %v, want nil", err)
	}
	for _, bad := range []string{
		"a:USD:100",          // 3 segments
		"a:USD:100:200:300", // 5 segments
		":USD:100:",          // empty account
		"a:US:100:",          // bad currency
		"a:USD:x:",           // bad low
		"a:USD::y",           // bad high
		"a:USD::",            // both empty
		"a:USD:500:500",      // low == high
		"a:USD:900:100",      // low > high
	} {
		if _, err := ParseBalanceWatches(bad); err == nil {
			t.Errorf("ParseBalanceWatches(%q): want error, got nil", bad)
		}
	}
}

func TestBalanceWatchSnapshotRoundTrip(t *testing.T) {
	l := New()
	mustSetWatch(t, l, "a", "USD", BalanceWatchConfig{LowCents: int64ptr(100), HighCents: int64ptr(900)})
	mustSetWatch(t, l, "b", "EUR", BalanceWatchConfig{HighCents: int64ptr(500)})
	mustWatchPost(t, l, JournalEntry{ID: "e1", DebitAccount: "a", CreditAccount: "bank", AmountCents: 50, Currency: "USD"})

	snap := exportSnapshotString(t, l)

	l2, err := ImportSnapshot(strings.NewReader(snap))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	watches := l2.ListBalanceWatches()
	if len(watches) != 2 {
		t.Fatalf("restored watches = %d, want 2", len(watches))
	}
	w, ok := l2.GetBalanceWatch("a", "USD")
	if !ok || *w.LowCents != 100 || *w.HighCents != 900 {
		t.Errorf("restored watch a = %+v, want low=100 high=900", w)
	}
	// Breach state re-derived from restored balances: a is at 50, below
	// its low of 100, so the next posting below stays silent (no
	// backfired alert) rather than firing.
	mustWatchPost(t, l2, JournalEntry{ID: "e2", DebitAccount: "bank", CreditAccount: "a", AmountCents: 10, Currency: "USD"})
	if got := l2.BalanceWatchBreachCount(); got != 0 {
		t.Errorf("BreachCount after restore+post = %d, want 0 (no backfill)", got)
	}
	// Recovery then re-cross fires exactly once.
	mustWatchPost(t, l2, JournalEntry{ID: "e3", DebitAccount: "a", CreditAccount: "bank", AmountCents: 100, Currency: "USD"})
	mustWatchPost(t, l2, JournalEntry{ID: "e4", DebitAccount: "bank", CreditAccount: "a", AmountCents: 50, Currency: "USD"})
	if got := l2.BalanceWatchBreachCount(); got != 1 {
		t.Errorf("BreachCount after re-cross = %d, want 1", got)
	}
}

// exportSnapshotString exports the ledger's disaster-recovery snapshot to
// a string.
func exportSnapshotString(t *testing.T, l *Ledger) string {
	t.Helper()
	var buf strings.Builder
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	return buf.String()
}

func TestBalanceWatchLegacySnapshotImport(t *testing.T) {
	// A snapshot carrying the pre-LG-61 low_balance_thresholds list
	// imports its levels as low-only watches. The legacy field is
	// spliced into the config record of a real JSONL export so the
	// surrounding envelope stays valid.
	l := New()
	snap := exportSnapshotString(t, l)
	var out []string
	spliced := false
	for _, line := range strings.Split(snap, "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decode snapshot line: %v", err)
		}
		if rec["record"] == "config" {
			rec["low_balance_thresholds"] = []any{
				map[string]any{"account": "old", "currency": "USD", "threshold_cents": float64(250)},
			}
			raw, err := json.Marshal(rec)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			line = string(raw)
			spliced = true
		}
		out = append(out, line)
	}
	if !spliced {
		t.Fatal("no config record in snapshot export")
	}
	l2, err := ImportSnapshot(strings.NewReader(strings.Join(out, "\n") + "\n"))
	if err != nil {
		t.Fatalf("ImportSnapshot(legacy): %v", err)
	}
	w, ok := l2.GetBalanceWatch("old", "USD")
	if !ok || w.LowCents == nil || *w.LowCents != 250 || w.HighCents != nil {
		t.Fatalf("legacy-imported watch = %+v, want low=250 high=nil", w)
	}
}

func TestBalanceWatchConcurrent(t *testing.T) {
	l := New()
	mustSetWatch(t, l, "hot", "USD", BalanceWatchConfig{LowCents: int64ptr(-1000000), HighCents: int64ptr(1000000)})
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_, _, _ = l.Post(JournalEntry{
					ID:            fmt.Sprintf("cc-%d-%d", i, j),
					DebitAccount:  "hot",
					CreditAccount: "bank",
					AmountCents:   10,
					Currency:      "USD",
				})
			}
		}(i)
	}
	wg.Wait()
	// 16*50 postings of 10 cents = 8000 < 1000000: no high breach, and no
	// race (run with -race).
	if got := l.BalanceWatchBreachCount(); got != 0 {
		t.Errorf("BreachCount = %d, want 0", got)
	}
	if got := l.BalanceIn("hot", "USD"); got != 8000 {
		t.Errorf("hot balance = %d, want 8000", got)
	}
}
