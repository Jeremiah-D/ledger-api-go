package ledger

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// mustAuditLog builds an AuditLog in a temp dir, failing the test on
// error. The caller must Close it.
func mustAuditLog(t *testing.T, opts ...AuditLogOption) *AuditLog {
	t.Helper()
	al, err := NewAuditLog(t.TempDir(), opts...)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	return al
}

func TestAuditLogDisabledByDefault(t *testing.T) {
	l := New()
	if _, _, enabled := l.AuditStats(); enabled {
		t.Fatal("AuditStats: enabled without WithAuditLog")
	}
	if _, _, err := l.Post(JournalEntry{ID: "e1", DebitAccount: "a", CreditAccount: "b", AmountCents: 100}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	// No-op emit must not panic or block when disabled.
	l.emitAudit(AuditEvent{Op: "test"})
}

func TestAuditLogRecordsOps(t *testing.T) {
	al := mustAuditLog(t)
	defer al.Close()
	l := New(WithAuditLog(al))

	mustPost := func(e JournalEntry) {
		t.Helper()
		if _, _, err := l.Post(e); err != nil {
			t.Fatalf("Post %s: %v", e.ID, err)
		}
	}
	mustPost(JournalEntry{ID: "e1", DebitAccount: "cash", CreditAccount: "rev", AmountCents: 1000, Currency: "USD"})
	mustPost(JournalEntry{ID: "e2", DebitAccount: "cash", CreditAccount: "rev", AmountCents: 500, Currency: "USD"})

	if _, err := l.PostTransfer(Transfer{ID: "tr1", From: "cash", To: "ops", AmountCents: 200}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if _, err := l.PostSweep(Sweep{ID: "sw1", From: []AccountID{"ops"}, To: "treasury"}); err != nil {
		t.Fatalf("PostSweep: %v", err)
	}
	now := time.Now()
	if _, _, err := l.Hold(Hold{ID: "h1", Account: "cash", AmountCents: 100, Currency: "USD", ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if _, err := l.Release("h1"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, _, err := l.Hold(Hold{ID: "h2", Account: "cash", AmountCents: 50, Currency: "USD", ExpiresAt: now.Add(-time.Hour)}); err != nil {
		t.Fatalf("Hold h2: %v", err)
	}
	if n := l.ExpireHoldsAt(now); n != 1 {
		t.Fatalf("ExpireHoldsAt = %d, want 1", n)
	}
	l.Freeze("frozen-acct")
	l.Unfreeze("frozen-acct")
	report := l.Reconcile(now)

	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, corrupt, err := ReadAuditLog(al.Dir(), now.UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt = %d, want 0", corrupt)
	}

	byOp := map[string][]AuditEvent{}
	for _, ev := range events {
		byOp[ev.Op] = append(byOp[ev.Op], ev)
	}
	// e1, e2, tr1, sw1, h1 hold, h1 release, h2 hold, expire, freeze,
	// unfreeze, reconcile = 11 events.
	if len(events) != 11 {
		t.Fatalf("got %d events, want 11", len(events))
	}

	// Post events carry the entry trace and a version bracket of one.
	posts := byOp["post"]
	if len(posts) != 2 {
		t.Fatalf("post events = %d, want 2", len(posts))
	}
	if posts[0].TraceID != "e1" || posts[0].Actor != "Post" {
		t.Errorf("post[0] = %+v, want trace e1 actor Post", posts[0])
	}
	if posts[0].VersionAfter != posts[0].VersionBefore+1 {
		t.Errorf("post version bracket = %d->%d, want +1", posts[0].VersionBefore, posts[0].VersionAfter)
	}
	if len(posts[0].EntryIDs) != 1 || posts[0].EntryIDs[0] != "e1" {
		t.Errorf("post entry_ids = %v, want [e1]", posts[0].EntryIDs)
	}
	if got := posts[0].Details["amount_cents"]; got != float64(1000) {
		t.Errorf("post amount_cents = %v, want 1000", got)
	}

	// Transfer: one event, trace = transfer ID, all legs listed.
	trs := byOp["transfer"]
	if len(trs) != 1 {
		t.Fatalf("transfer events = %d, want 1", len(trs))
	}
	if trs[0].TraceID != "tr1" || len(trs[0].EntryIDs) == 0 {
		t.Errorf("transfer event = %+v, want trace tr1 with legs", trs[0])
	}
	if trs[0].VersionAfter <= trs[0].VersionBefore {
		t.Errorf("transfer version bracket = %d->%d, want increase",
			trs[0].VersionBefore, trs[0].VersionAfter)
	}

	// Sweep legs share the sweep ID as trace.
	sw := byOp["sweep"]
	if len(sw) != 1 || sw[0].TraceID != "sw1" {
		t.Fatalf("sweep events = %+v, want one with trace sw1", sw)
	}

	// Freeze/unfreeze/reconcile/hold_expire book nothing: the bracket is
	// flat, proving the event was written against a known state.
	for _, op := range []string{"freeze", "unfreeze", "reconcile", "hold_expire"} {
		evs := byOp[op]
		if len(evs) != 1 {
			t.Fatalf("%s events = %d, want 1", op, len(evs))
		}
		if evs[0].VersionBefore != evs[0].VersionAfter {
			t.Errorf("%s version bracket = %d->%d, want flat",
				op, evs[0].VersionBefore, evs[0].VersionAfter)
		}
		if len(evs[0].EntryIDs) != 0 {
			t.Errorf("%s entry_ids = %v, want empty", op, evs[0].EntryIDs)
		}
	}
	if got := byOp["hold_expire"][0].Details["expired"]; got != float64(1) {
		t.Errorf("hold_expire details.expired = %v, want 1", got)
	}
	if byOp["reconcile"][0].VersionBefore != report.Version {
		t.Errorf("reconcile event version = %d, report version = %d",
			byOp["reconcile"][0].VersionBefore, report.Version)
	}

	// Events are written in commit order: timestamps non-decreasing and
	// versions monotonic.
	var lastVersion uint64
	for _, ev := range events {
		if ev.VersionBefore < lastVersion {
			t.Fatalf("event versions went backwards at op %s", ev.Op)
		}
		lastVersion = ev.VersionBefore
	}
}

func TestAuditLogReadsDoNotEmit(t *testing.T) {
	al := mustAuditLog(t)
	defer al.Close()
	l := New(WithAuditLog(al))
	if _, _, err := l.Post(JournalEntry{ID: "e1", DebitAccount: "a", CreditAccount: "b", AmountCents: 100}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	// Reads must not touch the audit log: balance, snapshot, trial
	// balance, listings, chain verification, time travel.
	_ = l.Balance("a")
	_, _ = l.Snapshot("a")
	_ = l.TrialBalance("a")
	_, _, _ = l.ListEntries(time.Now().Add(-time.Hour), time.Now().Add(time.Hour), "", 10)
	_ = l.VerifyChain()
	_, _ = l.BalanceAt("a", "", 1)
	_ = l.VerifyAccountingEquation()
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, _, err := ReadAuditLog(al.Dir(), time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if len(events) != 1 || events[0].Op != "post" {
		t.Fatalf("events = %v, want exactly the one post", events)
	}
}

func TestAuditLogRotationAndGzip(t *testing.T) {
	al := mustAuditLog(t, WithAuditLogMaxBytes(512))
	l := New(WithAuditLog(al))
	for i := 0; i < 30; i++ {
		l.Freeze(AccountID("acct-freeze-rotation"))
		l.Unfreeze(AccountID("acct-freeze-rotation"))
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	day := time.Now().UTC().Format("2006-01-02")
	matches, err := filepath.Glob(filepath.Join(al.Dir(), "audit-"+day+"*.jsonl*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) < 2 {
		t.Fatalf("files = %v, want at least 2 after size rotation", matches)
	}

	// The rotated files are gzipped in the background; wait for it.
	deadline := time.Now().Add(10 * time.Second)
	for {
		matches, _ = filepath.Glob(filepath.Join(al.Dir(), "audit-"+day+"*.jsonl.gz"))
		if len(matches) > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(matches) == 0 {
		t.Fatal("no .gz file appeared after rotation within 10s")
	}

	// ReadAuditLog transparently reads plain + gzipped files.
	events, corrupt, err := ReadAuditLog(al.Dir(), day)
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt = %d, want 0", corrupt)
	}
	if len(events) != 60 {
		t.Fatalf("events = %d, want 60 across rotated files", len(events))
	}
}

func TestAuditLogCorruptLinesSkipped(t *testing.T) {
	dir := t.TempDir()
	day := "2026-10-09"
	content := "{\"ts\":\"2026-10-09T00:00:00Z\",\"op\":\"post\",\"actor\":\"Post\",\"trace_id\":\"e1\",\"version_before\":0,\"version_after\":1}\n" +
		"this is not json\n" +
		"\n" +
		"{\"ts\":\"2026-10-09T00:00:01Z\",\"op\":\"freeze\",\"actor\":\"Freeze\",\"trace_id\":\"a\",\"version_before\":1,\"version_after\":1}\n" +
		"{\"truncated\": true\n"
	if err := os.WriteFile(filepath.Join(dir, "audit-2026-10-09.jsonl"), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	events, corrupt, err := ReadAuditLog(dir, day)
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2 intact records", len(events))
	}
	if corrupt != 2 {
		t.Fatalf("corrupt = %d, want 2", corrupt)
	}
	if events[0].Op != "post" || events[1].Op != "freeze" {
		t.Fatalf("events = %+v, want post then freeze", events)
	}
}

func TestAuditLogBadDayRejected(t *testing.T) {
	if _, _, err := ReadAuditLog(t.TempDir(), "not-a-day"); err == nil {
		t.Fatal("ReadAuditLog with bad day: want error")
	}
	if _, _, err := ReadAuditLog(t.TempDir(), "2026-10-09"); err != nil {
		t.Fatalf("ReadAuditLog with no files: %v", err)
	}
}

func TestAuditLogDropAfterClose(t *testing.T) {
	al := mustAuditLog(t)
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Logging after Close drops the event and counts it; Close is
	// idempotent.
	if al.Log(AuditEvent{Op: "post"}) {
		t.Fatal("Log after Close: want false")
	}
	if err := al.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	_, dropped := al.Stats()
	if dropped != 1 {
		t.Fatalf("dropped = %d, want 1", dropped)
	}
}

func TestAuditLogConcurrentPosts(t *testing.T) {
	al := mustAuditLog(t)
	defer al.Close()
	l := New(WithAuditLog(al))
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				id := "e-" + string(rune('a'+g)) + "-" + string(rune('0'+i/10)) + string(rune('0'+i%10))
				if _, _, err := l.Post(JournalEntry{
					ID: id, DebitAccount: "a", CreditAccount: "b", AmountCents: 1,
				}); err != nil {
					t.Errorf("Post %s: %v", id, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	written, dropped := al.Stats()
	if dropped != 0 {
		t.Fatalf("dropped = %d, want 0", dropped)
	}
	if written != 200 {
		t.Fatalf("written = %d, want 200", written)
	}
	events, corrupt, err := ReadAuditLog(al.Dir(), time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 || len(events) != 200 {
		t.Fatalf("events = %d, corrupt = %d; want 200/0", len(events), corrupt)
	}
}
