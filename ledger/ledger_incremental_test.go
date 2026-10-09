package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// buildDivergedPair returns a source ledger A with a rich state (posts,
// transfer with idempotency key, hold, freeze, FX rate) and a replica B
// that matches A exactly at baseVersion (built by applying a full
// snapshot, like a real replica would).
func buildDivergedPair(t *testing.T) (a, b *Ledger, baseVersion uint64) {
	t.Helper()
	a = New(WithFXAccount("fx-pnl"), WithIdempotencyTTL(time.Hour))
	if err := a.SetFXRate("USD", "EUR", 108, 100); err != nil {
		t.Fatal(err)
	}
	post := func(e JournalEntry) {
		t.Helper()
		if _, _, err := a.Post(e); err != nil {
			t.Fatalf("post %s: %v", e.ID, err)
		}
	}
	post(JournalEntry{ID: "e1", DebitAccount: "alice", CreditAccount: "bank", AmountCents: 100000, Currency: "USD"})
	post(JournalEntry{ID: "e2", DebitAccount: "bob", CreditAccount: "bank", AmountCents: 50000, Currency: "EUR", IdempotencyKey: "k-e2"})
	if _, err := a.PostTransfer(Transfer{ID: "t1", From: "alice", To: "carol", AmountCents: 10000, Currency: "USD", IdempotencyKey: "k-t1"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Hold(Hold{ID: "h1", Account: "alice", AmountCents: 5000, Currency: "USD", ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now(), IdempotencyKey: "k-h1"}); err != nil {
		t.Fatal(err)
	}
	a.Freeze("mallory")

	var full strings.Builder
	if err := a.ExportSnapshot(&full); err != nil {
		t.Fatal(err)
	}
	b, err := ImportSnapshot(strings.NewReader(full.String()))
	if err != nil {
		t.Fatalf("replica full import: %v", err)
	}
	baseVersion = a.versionOf()
	if !snapshotLedgersEqual(a, b) {
		t.Fatal("replica diverged from source before the delta")
	}
	return a, b, baseVersion
}

func (l *Ledger) versionOf() uint64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.version
}

func TestIncrementalHappyPath(t *testing.T) {
	a, b, base := buildDivergedPair(t)

	// New activity on the source: a post, an FX transfer, a config
	// change (new FX rate), and another post.
	if _, _, err := a.Post(JournalEntry{ID: "e3", DebitAccount: "dave", CreditAccount: "bank", AmountCents: 7000, Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.PostTransfer(Transfer{ID: "fx1", From: "alice", To: "erin", AmountCents: 20000, Currency: "USD", ToCurrency: "EUR"}); err != nil {
		t.Fatal(err)
	}
	if err := a.SetFXRate("USD", "JPY", 150, 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Post(JournalEntry{ID: "e4", DebitAccount: "bank", CreditAccount: "dave", AmountCents: 1000, Currency: "USD"}); err != nil {
		t.Fatal(err)
	}

	var delta strings.Builder
	if err := a.ExportIncrementalSnapshot(&delta, base); err != nil {
		t.Fatalf("export delta: %v", err)
	}
	if err := b.ImportIncrementalSnapshot(strings.NewReader(delta.String())); err != nil {
		t.Fatalf("import delta: %v", err)
	}

	if !snapshotLedgersEqual(a, b) {
		t.Error("replica diverged from source after delta apply")
	}
	if err := b.VerifyChain(); err != nil {
		t.Errorf("replica chain broken: %v", err)
	}
	if err := b.VerifyAccountingEquation(); err != nil {
		t.Errorf("replica books do not balance: %v", err)
	}
	if got := b.BalanceIn("erin", "EUR"); got != 21600 {
		t.Errorf("erin EUR = %d, want 21600", got)
	}
	// The config change rode the delta: the new rate is live on the replica.
	if r, ok := b.FXRate("USD", "JPY"); !ok || r.Num != 150 {
		t.Errorf("replica USD->JPY rate: %+v, %v", r, ok)
	}
	// The replica keeps working: posts and replays.
	if _, _, err := b.Post(JournalEntry{ID: "e5", DebitAccount: "zoe", CreditAccount: "bank", AmountCents: 1}); err != nil {
		t.Errorf("post on replica: %v", err)
	}
	if _, dup, err := b.Post(JournalEntry{ID: "e5b", DebitAccount: "zoe", CreditAccount: "bank", AmountCents: 1, IdempotencyKey: "k-e2"}); err != nil || !dup {
		t.Errorf("replay on replica: dup=%v err=%v", dup, err)
	}
}

func TestIncrementalEmptyDelta(t *testing.T) {
	a, b, base := buildDivergedPair(t)
	var delta strings.Builder
	if err := a.ExportIncrementalSnapshot(&delta, base); err != nil {
		t.Fatal(err)
	}
	if err := b.ImportIncrementalSnapshot(strings.NewReader(delta.String())); err != nil {
		t.Fatalf("empty delta import: %v", err)
	}
	if !snapshotLedgersEqual(a, b) {
		t.Error("empty delta changed the replica")
	}
	if b.versionOf() != base {
		t.Errorf("version = %d, want %d", b.versionOf(), base)
	}
}

func TestIncrementalContinuity(t *testing.T) {
	a, b, base := buildDivergedPair(t)

	// The source moves twice; the replica only applies the second delta:
	// a gap. It must be rejected, and the replica untouched.
	if _, _, err := a.Post(JournalEntry{ID: "e3", DebitAccount: "x", CreditAccount: "bank", AmountCents: 1}); err != nil {
		t.Fatal(err)
	}
	mid := a.versionOf()
	if _, _, err := a.Post(JournalEntry{ID: "e4", DebitAccount: "y", CreditAccount: "bank", AmountCents: 2}); err != nil {
		t.Fatal(err)
	}
	var delta strings.Builder
	if err := a.ExportIncrementalSnapshot(&delta, mid); err != nil {
		t.Fatal(err)
	}
	before := b.versionOf()
	if err := b.ImportIncrementalSnapshot(strings.NewReader(delta.String())); !errors.Is(err, ErrSnapshotInvalid) {
		t.Fatalf("gap delta: want ErrSnapshotInvalid, got %v", err)
	}
	if b.versionOf() != before {
		t.Error("rejected gap delta mutated the replica")
	}

	// The correct delta (from base) still applies afterwards.
	var good strings.Builder
	if err := a.ExportIncrementalSnapshot(&good, base); err != nil {
		t.Fatal(err)
	}
	if err := b.ImportIncrementalSnapshot(strings.NewReader(good.String())); err != nil {
		t.Fatalf("good delta after gap rejection: %v", err)
	}
	if !snapshotLedgersEqual(a, b) {
		t.Error("replica diverged after good delta")
	}
}

func TestIncrementalTamperedEntry(t *testing.T) {
	a, b, base := buildDivergedPair(t)
	if _, _, err := a.Post(JournalEntry{ID: "e3", DebitAccount: "victim", CreditAccount: "bank", AmountCents: 9000}); err != nil {
		t.Fatal(err)
	}
	var delta strings.Builder
	if err := a.ExportIncrementalSnapshot(&delta, base); err != nil {
		t.Fatal(err)
	}
	// Tamper with the entry amount in the delta (the hash no longer matches).
	tampered := strings.Replace(delta.String(), `"amount_cents":9000`, `"amount_cents":9001`, 1)
	if tampered == delta.String() {
		t.Fatal("tamper did not take effect")
	}
	before := b.versionOf()
	if err := b.ImportIncrementalSnapshot(strings.NewReader(tampered)); !errors.Is(err, ErrSnapshotInvalid) {
		t.Fatalf("tampered delta: want ErrSnapshotInvalid, got %v", err)
	}
	if b.versionOf() != before || len(b.Entries()) != len(a.Entries())-1 {
		t.Error("rejected tampered delta mutated the replica")
	}
	if err := b.VerifyChain(); err != nil {
		t.Errorf("replica chain broken after rejection: %v", err)
	}
}

func TestIncrementalSplicedLink(t *testing.T) {
	a, b, base := buildDivergedPair(t)
	if _, _, err := a.Post(JournalEntry{ID: "e3", DebitAccount: "x", CreditAccount: "bank", AmountCents: 1}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Post(JournalEntry{ID: "e4", DebitAccount: "y", CreditAccount: "bank", AmountCents: 2}); err != nil {
		t.Fatal(err)
	}
	var delta strings.Builder
	if err := a.ExportIncrementalSnapshot(&delta, base); err != nil {
		t.Fatal(err)
	}
	// Splice: drop the first entry/link pair so the second link's
	// prev_hash no longer extends the replica's head.
	lines := strings.Split(strings.TrimRight(delta.String(), "\n"), "\n")
	var kept []string
	skipped := 0
	for _, ln := range lines {
		if skipped < 2 && (strings.Contains(ln, `"id":"e3"`) || (strings.Contains(ln, `"record":"link"`) && strings.Contains(ln, `"entry_id":"e3"`))) {
			skipped++
			continue
		}
		kept = append(kept, ln)
	}
	if skipped != 2 {
		t.Fatalf("splice setup dropped %d lines, want 2", skipped)
	}
	if err := b.ImportIncrementalSnapshot(strings.NewReader(strings.Join(kept, "\n") + "\n")); !errors.Is(err, ErrSnapshotInvalid) {
		t.Fatalf("spliced delta: want ErrSnapshotInvalid, got nil")
	}
}

func TestIncrementalOverlap(t *testing.T) {
	a, b, base := buildDivergedPair(t)
	if _, _, err := a.Post(JournalEntry{ID: "e3", DebitAccount: "x", CreditAccount: "bank", AmountCents: 1}); err != nil {
		t.Fatal(err)
	}
	// The replica independently posts an entry with the same ID (same
	// content — still an overlap, still rejected).
	if _, _, err := b.Post(JournalEntry{ID: "e3", DebitAccount: "x", CreditAccount: "bank", AmountCents: 1}); err != nil {
		t.Fatal(err)
	}
	var delta strings.Builder
	if err := a.ExportIncrementalSnapshot(&delta, base); err != nil {
		t.Fatal(err)
	}
	if err := b.ImportIncrementalSnapshot(strings.NewReader(delta.String())); !errors.Is(err, ErrSnapshotInvalid) {
		t.Fatalf("overlapping delta: want ErrSnapshotInvalid, got %v", err)
	}
}

func TestIncrementalRegistryConflict(t *testing.T) {
	a, b, base := buildDivergedPair(t)
	if _, _, err := a.Post(JournalEntry{ID: "e3", DebitAccount: "x", CreditAccount: "bank", AmountCents: 1, IdempotencyKey: "k-new"}); err != nil {
		t.Fatal(err)
	}
	var delta strings.Builder
	if err := a.ExportIncrementalSnapshot(&delta, base); err != nil {
		t.Fatal(err)
	}
	// Corrupt the delta's idempotency record for k-new so it conflicts
	// with the journal row it references.
	lines := strings.Split(strings.TrimRight(delta.String(), "\n"), "\n")
	for i, ln := range lines {
		if strings.Contains(ln, `"namespace":"entry"`) && strings.Contains(ln, `"key":"k-new"`) {
			lines[i] = strings.Replace(ln, `"amount_cents":1`, `"amount_cents":2`, 1)
		}
	}
	corrupt := strings.Join(lines, "\n") + "\n"
	if err := b.ImportIncrementalSnapshot(strings.NewReader(corrupt)); !errors.Is(err, ErrSnapshotInvalid) {
		t.Fatalf("conflicting registry: want ErrSnapshotInvalid, got %v", err)
	}
}

func TestIncrementalExportFromFuture(t *testing.T) {
	a, _, base := buildDivergedPair(t)
	var delta strings.Builder
	if err := a.ExportIncrementalSnapshot(&delta, base+100); err == nil {
		t.Error("export from a future version: want error, got nil")
	}
}

func TestIncrementalDeltaDeterminism(t *testing.T) {
	a, _, base := buildDivergedPair(t)
	if _, _, err := a.Post(JournalEntry{ID: "e3", DebitAccount: "x", CreditAccount: "bank", AmountCents: 1}); err != nil {
		t.Fatal(err)
	}
	var d1, d2 strings.Builder
	if err := a.ExportIncrementalSnapshot(&d1, base); err != nil {
		t.Fatal(err)
	}
	if err := a.ExportIncrementalSnapshot(&d2, base); err != nil {
		t.Fatal(err)
	}
	// Same state exports to identical bytes, modulo the exported_at meta
	// timestamp: strip the first line and compare the rest.
	stripMeta := func(s string) string {
		_, rest, _ := strings.Cut(s, "\n")
		return rest
	}
	if stripMeta(d1.String()) != stripMeta(d2.String()) {
		t.Error("incremental export is not deterministic")
	}
}

func TestFullImportRejectsIncremental(t *testing.T) {
	a, _, base := buildDivergedPair(t)
	if _, _, err := a.Post(JournalEntry{ID: "e3", DebitAccount: "x", CreditAccount: "bank", AmountCents: 1}); err != nil {
		t.Fatal(err)
	}
	var delta strings.Builder
	if err := a.ExportIncrementalSnapshot(&delta, base); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportSnapshot(strings.NewReader(delta.String())); !errors.Is(err, ErrSnapshotInvalid) {
		t.Fatalf("full import of incremental snapshot: want ErrSnapshotInvalid, got %v", err)
	}
}
