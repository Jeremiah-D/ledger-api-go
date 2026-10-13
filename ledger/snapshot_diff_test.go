package ledger

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeFullSnapshotFile exports l to dir under the backup worker's full
// filename convention so the diff tests exercise the real resolution
// path (LoadSnapshotAtVersion), not a hand-built fixture.
func writeFullSnapshotFile(t *testing.T, dir string, l *Ledger) {
	t.Helper()
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	name := fmt.Sprintf("snapshot-full-v%020d-20261012T000000Z.jsonl", l.Version())
	if err := os.WriteFile(filepath.Join(dir, name), buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func writeIncrSnapshotFile(t *testing.T, dir string, l *Ledger, base uint64) {
	t.Helper()
	var buf bytes.Buffer
	if err := l.ExportIncrementalSnapshot(&buf, base); err != nil {
		t.Fatalf("ExportIncrementalSnapshot: %v", err)
	}
	name := fmt.Sprintf("snapshot-incr-v%020d-v%020d-20261012T000000Z.jsonl", base, l.Version())
	if err := os.WriteFile(filepath.Join(dir, name), buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

var diffPostSeq int

func mustPostDiff(t *testing.T, l *Ledger, debit, credit AccountID, cents int64) {
	t.Helper()
	diffPostSeq++
	if _, _, err := l.Post(JournalEntry{
		ID:           fmt.Sprintf("diff-e%d", diffPostSeq),
		DebitAccount: debit, CreditAccount: credit, AmountCents: cents,
	}); err != nil {
		t.Fatalf("Post(%s->%s %d): %v", debit, credit, cents, err)
	}
}

func TestSnapshotDiffThreeDimensions(t *testing.T) {
	dir := t.TempDir()

	// v3: acct-a funds acct-b; acct-c frozen.
	l := New()
	mustPostDiff(t, l, "acct-a", "acct-b", 1000)
	mustPostDiff(t, l, "acct-a", "acct-b", 500)
	mustPostDiff(t, l, "acct-b", "acct-c", 200)
	l.Freeze("acct-c")
	writeFullSnapshotFile(t, dir, l)
	v3 := l.Version()

	// v5: more postings, acct-c unfrozen, new currency leg for acct-a.
	mustPostDiff(t, l, "acct-a", "acct-d", 300)
	l.Unfreeze("acct-c")
	writeFullSnapshotFile(t, dir, l)
	v5 := l.Version()

	d, err := DiffSnapshotVersions(dir, v3, v5)
	if err != nil {
		t.Fatalf("DiffSnapshotVersions: %v", err)
	}
	if d.FromVersion != v3 || d.ToVersion != v5 {
		t.Fatalf("versions = %d->%d, want %d->%d", d.FromVersion, d.ToVersion, v3, v5)
	}

	// Entries: one new row added, none deleted or changed.
	if len(d.Entries.Added) != 1 || len(d.Entries.Deleted) != 0 || len(d.Entries.Changed) != 0 {
		t.Fatalf("entries = +%d/-%d/~%d, want +1/-0/~0",
			len(d.Entries.Added), len(d.Entries.Deleted), len(d.Entries.Changed))
	}

	// Balances: acct-a lost 300 more (USD), acct-d is a new key.
	var changedA, addedD *BalanceDiffRow
	for i := range d.Balances.Changed {
		r := &d.Balances.Changed[i]
		if r.Account == "acct-a" && r.Currency == "USD" {
			changedA = r
		}
	}
	for i := range d.Balances.Added {
		r := &d.Balances.Added[i]
		if r.Account == "acct-d" && r.Currency == "USD" {
			addedD = r
		}
	}
	if changedA == nil {
		t.Fatalf("balances.changed missing acct-a/USD: %+v", d.Balances.Changed)
	}
	if changedA.FromCents != 1500 || changedA.ToCents != 1800 || changedA.DeltaCents != 300 {
		t.Fatalf("acct-a row = %+v, want from=1500 to=1800 delta=300", changedA)
	}
	if addedD == nil || addedD.ToCents != -300 || addedD.FromCents != 0 {
		t.Fatalf("balances.added missing acct-d/USD=-300: %+v", d.Balances.Added)
	}

	// Frozen: acct-c unfrozen -> deleted.
	if len(d.Frozen.Added) != 0 || len(d.Frozen.Changed) != 0 || len(d.Frozen.Deleted) != 1 {
		t.Fatalf("frozen = +%d/-%d/~%d, want +0/-1/~0",
			len(d.Frozen.Added), len(d.Frozen.Deleted), len(d.Frozen.Changed))
	}
	if d.Frozen.Deleted[0].Account != "acct-c" {
		t.Fatalf("frozen.deleted[0] = %s, want acct-c", d.Frozen.Deleted[0].Account)
	}
}

func TestSnapshotDiffFrozenDeadlineChanged(t *testing.T) {
	dir := t.TempDir()
	l := New()
	mustPostDiff(t, l, "acct-a", "acct-b", 1000)
	if err := l.FreezeUntil("acct-a", time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatalf("FreezeUntil: %v", err)
	}
	writeFullSnapshotFile(t, dir, l)
	v1 := l.Version()

	// Re-freeze with a later deadline: still frozen, deadline moved.
	// A posting in between bumps the version so the two snapshots land
	// in different files (freeze changes carry no version bump).
	mustPostDiff(t, l, "acct-b", "acct-d", 1)
	if err := l.FreezeUntil("acct-a", time.Now().UTC().Add(2*time.Hour)); err != nil {
		t.Fatalf("FreezeUntil: %v", err)
	}
	writeFullSnapshotFile(t, dir, l)
	v2 := l.Version()

	d, err := DiffSnapshotVersions(dir, v1, v2)
	if err != nil {
		t.Fatalf("DiffSnapshotVersions: %v", err)
	}
	if len(d.Frozen.Changed) != 1 || d.Frozen.Changed[0].Account != "acct-a" {
		t.Fatalf("frozen.changed = %+v, want 1 row for acct-a", d.Frozen.Changed)
	}
	row := d.Frozen.Changed[0]
	if row.FromAutoUnfreezeAt == nil || row.ToAutoUnfreezeAt == nil {
		t.Fatalf("changed row missing deadlines: %+v", row)
	}
	if !row.ToAutoUnfreezeAt.After(*row.FromAutoUnfreezeAt) {
		t.Fatalf("deadline did not move forward: %+v", row)
	}
	// The freeze change itself bumps nothing and touches no journal row:
	// the only entry in the diff is the intermediate version-bump post.
	if len(d.Entries.Added) != 1 || len(d.Frozen.Added) != 0 || len(d.Frozen.Deleted) != 0 {
		t.Fatalf("unexpected diff shape: %+v", d)
	}
}

func TestLoadSnapshotAtVersionIncrementalChain(t *testing.T) {
	dir := t.TempDir()
	l := New()
	mustPostDiff(t, l, "acct-a", "acct-b", 1000)
	mustPostDiff(t, l, "acct-a", "acct-b", 200)
	base := l.Version()
	writeFullSnapshotFile(t, dir, l)

	mustPostDiff(t, l, "acct-b", "acct-c", 150)
	mustPostDiff(t, l, "acct-a", "acct-c", 50)
	writeIncrSnapshotFile(t, dir, l, base)
	v5 := l.Version()

	loaded, err := LoadSnapshotAtVersion(dir, v5)
	if err != nil {
		t.Fatalf("LoadSnapshotAtVersion(%d): %v", v5, err)
	}
	for _, tc := range []struct {
		account AccountID
		want    int64
	}{
		{"acct-a", 1250}, {"acct-b", -1050}, {"acct-c", -200},
	} {
		if got := loaded.Balance(tc.account); got != tc.want {
			t.Fatalf("Balance(%s) = %d, want %d", tc.account, got, tc.want)
		}
	}
	if loaded.Version() != v5 {
		t.Fatalf("loaded version = %d, want %d", loaded.Version(), v5)
	}

	// The intermediate full version still resolves directly.
	if _, err := LoadSnapshotAtVersion(dir, base); err != nil {
		t.Fatalf("LoadSnapshotAtVersion(%d): %v", base, err)
	}
}

func TestLoadSnapshotAtVersionNotFound(t *testing.T) {
	// Empty directory.
	if _, err := LoadSnapshotAtVersion(t.TempDir(), 3); !errors.Is(err, ErrSnapshotVersionNotFound) {
		t.Fatalf("empty dir: err = %v, want ErrSnapshotVersionNotFound", err)
	}

	// Full at v2, asking for v5 with no incremental chain to bridge.
	dir := t.TempDir()
	l := New()
	mustPostDiff(t, l, "acct-a", "acct-b", 100)
	mustPostDiff(t, l, "acct-a", "acct-b", 100)
	writeFullSnapshotFile(t, dir, l)
	if _, err := LoadSnapshotAtVersion(dir, 99); !errors.Is(err, ErrSnapshotVersionNotFound) {
		t.Fatalf("gap: err = %v, want ErrSnapshotVersionNotFound", err)
	}

	// Version below every full.
	if _, err := LoadSnapshotAtVersion(dir, 1); !errors.Is(err, ErrSnapshotVersionNotFound) {
		t.Fatalf("below full: err = %v, want ErrSnapshotVersionNotFound", err)
	}
}

func TestSnapshotDiffSameVersionEmpty(t *testing.T) {
	dir := t.TempDir()
	l := New()
	mustPostDiff(t, l, "acct-a", "acct-b", 777)
	writeFullSnapshotFile(t, dir, l)
	v := l.Version()

	d, err := DiffSnapshotVersions(dir, v, v)
	if err != nil {
		t.Fatalf("DiffSnapshotVersions(%d,%d): %v", v, v, err)
	}
	if !d.IsEmpty() {
		t.Fatalf("same-version diff not empty: %+v", d)
	}
}

func TestSnapshotDiffReversedDirection(t *testing.T) {
	dir := t.TempDir()
	l := New()
	mustPostDiff(t, l, "acct-a", "acct-b", 100)
	writeFullSnapshotFile(t, dir, l)
	v1 := l.Version()
	mustPostDiff(t, l, "acct-a", "acct-b", 100)
	writeFullSnapshotFile(t, dir, l)
	v2 := l.Version()

	// Reverse direction: the newer balance row shows up as deleted.
	d, err := DiffSnapshotVersions(dir, v2, v1)
	if err != nil {
		t.Fatalf("DiffSnapshotVersions: %v", err)
	}
	if len(d.Entries.Deleted) != 1 || len(d.Entries.Added) != 0 {
		t.Fatalf("reversed entries = +%d/-%d, want +0/-1",
			len(d.Entries.Added), len(d.Entries.Deleted))
	}
	var changedA *BalanceDiffRow
	for i := range d.Balances.Changed {
		if d.Balances.Changed[i].Account == "acct-a" {
			changedA = &d.Balances.Changed[i]
		}
	}
	if changedA == nil || changedA.DeltaCents != -100 {
		t.Fatalf("reversed acct-a delta = %+v, want -100", changedA)
	}
}

func TestDiffEntriesChangedFields(t *testing.T) {
	mk := func(id, memo string, cents int64) JournalEntry {
		return JournalEntry{ID: id, DebitAccount: "a", CreditAccount: "b",
			AmountCents: cents, Currency: "USD", Memo: memo,
			CreatedAt: time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)}
	}
	from := map[string]JournalEntry{"e1": mk("e1", "one", 100), "e2": mk("e2", "two", 50)}
	to := map[string]JournalEntry{"e1": mk("e1", "one-changed", 100), "e3": mk("e3", "three", 70)}

	sec := diffEntries(from, to)
	if len(sec.Added) != 1 || sec.Added[0].ID != "e3" {
		t.Fatalf("added = %+v, want [e3]", sec.Added)
	}
	if len(sec.Deleted) != 1 || sec.Deleted[0].ID != "e2" {
		t.Fatalf("deleted = %+v, want [e2]", sec.Deleted)
	}
	if len(sec.Changed) != 1 || sec.Changed[0].ID != "e1" {
		t.Fatalf("changed = %+v, want [e1]", sec.Changed)
	}
	fields := sec.Changed[0].ChangedFields
	if len(fields) != 1 || fields[0] != "memo" {
		t.Fatalf("changed_fields = %v, want [memo]", fields)
	}
}
