package ledger

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Snapshot version diff (LG-70).
//
// Backup snapshots (see snapshot.go / snapshot_backup.go) are addressed
// by ledger version: a full snapshot file is
// "snapshot-full-v<version>-<utc-timestamp>.jsonl" and an incremental
// one "snapshot-incr-v<base>-v<version>-<utc-timestamp>.jsonl", both in
// the backup directory configured with LEDGER_SNAPSHOT_BACKUP_DIR.
// LoadSnapshotAtVersion resolves a version to a ledger state by reading
// the newest full snapshot at or below the requested version and, when
// the full is below it, applying incremental deltas in continuity order
// until the version is reached — the same continuity invariant
// ImportIncrementalSnapshot enforces. DiffSnapshotVersions loads two
// versions and reports the difference in three dimensions:
//
//   - balances: per (account, currency) balance, in cents
//   - entries: journal rows by entry ID
//   - frozen: frozen-account risk-control state (incl. timed-freeze deadlines)
//
// Each dimension reports added / deleted / changed rows. The diff is
// purely read-side: the loaded ledgers are fresh in-memory copies, so a
// diff never writes to the live ledger, bumps no version, and emits no
// audit events — "diff 只读零写影响" is structural, not a convention.

// ErrSnapshotVersionNotFound is returned by LoadSnapshotAtVersion and
// DiffSnapshotVersions when no chain of backup files can construct the
// requested version: no full snapshot at or below it exists, or the
// incremental chain from the newest usable full has a gap.
var ErrSnapshotVersionNotFound = errors.New("ledger: snapshot version not found")

// BalanceDiffRow is one per-(account, currency) balance line of the
// diff. For added rows FromCents is 0, for deleted rows ToCents is 0;
// DeltaCents is always ToCents - FromCents.
type BalanceDiffRow struct {
	Account    AccountID `json:"account"`
	Currency   string    `json:"currency"`
	FromCents  int64     `json:"from_cents"`
	ToCents    int64     `json:"to_cents"`
	DeltaCents int64     `json:"delta_cents"`
}

// BalanceDiffSection is the balances dimension of a snapshot diff.
type BalanceDiffSection struct {
	Added   []BalanceDiffRow `json:"added"`
	Deleted []BalanceDiffRow `json:"deleted"`
	Changed []BalanceDiffRow `json:"changed"`
}

// EntryDiffRow is one changed journal row: the entry ID, the names of
// the fields that differ, and both versions of the row. Entries are
// append-only in a healthy ledger, so a changed row is always worth a
// second look — the diff reports it instead of pretending it cannot
// happen.
type EntryDiffRow struct {
	ID            string       `json:"id"`
	ChangedFields []string     `json:"changed_fields"`
	From          JournalEntry `json:"from"`
	To            JournalEntry `json:"to"`
}

// EntryDiffSection is the entries dimension of a snapshot diff.
type EntryDiffSection struct {
	Added   []JournalEntry `json:"added"`
	Deleted []JournalEntry `json:"deleted"`
	Changed []EntryDiffRow `json:"changed"`
}

// FrozenDiffRow is one frozen-account line of the diff. The
// auto-unfreeze deadline (see FreezeUntil) is nil for a manually frozen
// account; a frozen account whose deadline appeared, disappeared, or
// moved is reported under changed.
type FrozenDiffRow struct {
	Account            AccountID  `json:"account"`
	FromAutoUnfreezeAt *time.Time `json:"from_auto_unfreeze_at,omitempty"`
	ToAutoUnfreezeAt   *time.Time `json:"to_auto_unfreeze_at,omitempty"`
}

// FrozenDiffSection is the frozen-state dimension of a snapshot diff.
type FrozenDiffSection struct {
	Added   []FrozenDiffRow `json:"added"`
	Deleted []FrozenDiffRow `json:"deleted"`
	Changed []FrozenDiffRow `json:"changed"`
}

// SnapshotDiff is the full three-dimension diff between two snapshot
// versions of the same ledger.
type SnapshotDiff struct {
	FromVersion uint64             `json:"from_version"`
	ToVersion   uint64             `json:"to_version"`
	Balances    BalanceDiffSection `json:"balances"`
	Entries     EntryDiffSection   `json:"entries"`
	Frozen      FrozenDiffSection  `json:"frozen"`
}

// IsEmpty reports whether the diff contains no changes at all.
func (d *SnapshotDiff) IsEmpty() bool {
	return len(d.Balances.Added) == 0 && len(d.Balances.Deleted) == 0 && len(d.Balances.Changed) == 0 &&
		len(d.Entries.Added) == 0 && len(d.Entries.Deleted) == 0 && len(d.Entries.Changed) == 0 &&
		len(d.Frozen.Added) == 0 && len(d.Frozen.Deleted) == 0 && len(d.Frozen.Changed) == 0
}

// snapshotFileRef is one backup file parsed from its name.
type snapshotFileRef struct {
	name    string
	kind    string // "full" or "incr"
	version uint64
	base    uint64 // incr only
}

// parseSnapshotFilename parses a backup filename produced by
// snapshotBackupFilename. It returns ok=false for anything else,
// including temp files mid-rename (".snapshot-tmp-*").
func parseSnapshotFilename(name string) (ref snapshotFileRef, ok bool) {
	if !strings.HasSuffix(name, ".jsonl") {
		return ref, false
	}
	body := strings.TrimSuffix(name, ".jsonl")
	switch {
	case strings.HasPrefix(body, "snapshot-full-v"):
		rest := strings.TrimPrefix(body, "snapshot-full-v")
		verStr, _, found := strings.Cut(rest, "-")
		if !found {
			return ref, false
		}
		v, err := strconv.ParseUint(verStr, 10, 64)
		if err != nil {
			return ref, false
		}
		return snapshotFileRef{name: name, kind: "full", version: v}, true
	case strings.HasPrefix(body, "snapshot-incr-v"):
		rest := strings.TrimPrefix(body, "snapshot-incr-v")
		baseStr, rest2, found := strings.Cut(rest, "-v")
		if !found {
			return ref, false
		}
		verStr, _, found := strings.Cut(rest2, "-")
		if !found {
			return ref, false
		}
		b, err1 := strconv.ParseUint(baseStr, 10, 64)
		v, err2 := strconv.ParseUint(verStr, 10, 64)
		if err1 != nil || err2 != nil {
			return ref, false
		}
		return snapshotFileRef{name: name, kind: "incr", version: v, base: b}, true
	}
	return ref, false
}

// LoadSnapshotAtVersion resolves a ledger version to a ledger state by
// reading the backup directory: the newest full snapshot at or below
// the requested version, then incremental deltas applied in ascending
// order until the version is reached. When several files claim the
// same (kind, base, version), the lexically newest name wins — the
// filename timestamp is fixed-width, so lexical order is chronological.
//
// The returned ledger is a fresh in-memory ledger; loading never touches
// the live ledger. A tampered snapshot is rejected by the same
// VerifyChain the import always runs (ErrSnapshotInvalid), never
// half-loaded; a version no backup chain can construct returns
// ErrSnapshotVersionNotFound.
func LoadSnapshotAtVersion(dir string, version uint64) (*Ledger, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot read backup dir %q: %v", ErrSnapshotVersionNotFound, dir, err)
	}

	// Newest name wins per (kind, base, version): retention prunes the
	// tail, so the tail is also the authoritative copy.
	fulls := map[uint64]string{}
	incrs := map[[2]uint64]string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ref, ok := parseSnapshotFilename(e.Name())
		if !ok {
			continue
		}
		switch ref.kind {
		case "full":
			if cur, dup := fulls[ref.version]; !dup || ref.name > cur {
				fulls[ref.version] = ref.name
			}
		case "incr":
			key := [2]uint64{ref.base, ref.version}
			if cur, dup := incrs[key]; !dup || ref.name > cur {
				incrs[key] = ref.name
			}
		}
	}

	openFile := func(name string) (*os.File, error) {
		return os.Open(filepath.Join(dir, name))
	}

	// Exact full snapshot: the common case for versions the rotation
	// actually materialized.
	if name, ok := fulls[version]; ok {
		f, err := openFile(name)
		if err != nil {
			return nil, fmt.Errorf("ledger: open snapshot %q: %w", name, err)
		}
		defer f.Close()
		l, err := ImportSnapshot(f)
		if err != nil {
			return nil, err
		}
		return l, nil
	}

	// Otherwise build the version from the newest full at or below it
	// plus the incremental chain. The chain must be gapless: each delta
	// is applied only when its base equals the version we have so far,
	// exactly the continuity ImportIncrementalSnapshot requires.
	var base uint64
	var baseName string
	found := false
	for v, name := range fulls {
		if v < version && (!found || v > base) {
			base, baseName, found = v, name, true
		}
	}
	if !found {
		return nil, fmt.Errorf("%w: no full snapshot at or below version %d in %q",
			ErrSnapshotVersionNotFound, version, dir)
	}

	f, err := openFile(baseName)
	if err != nil {
		return nil, fmt.Errorf("ledger: open snapshot %q: %w", baseName, err)
	}
	l, err := ImportSnapshot(f)
	f.Close()
	if err != nil {
		return nil, err
	}

	for cur := base; cur < version; {
		var next uint64
		var nextName string
		for key, name := range incrs {
			if key[0] == cur && key[1] > cur && key[1] <= version && key[1] > next {
				next, nextName = key[1], name
			}
		}
		if nextName == "" {
			return nil, fmt.Errorf("%w: incremental chain from version %d cannot reach version %d in %q",
				ErrSnapshotVersionNotFound, base, version, dir)
		}
		f, err := openFile(nextName)
		if err != nil {
			return nil, fmt.Errorf("ledger: open snapshot %q: %w", nextName, err)
		}
		err = l.ImportIncrementalSnapshot(f)
		f.Close()
		if err != nil {
			return nil, err
		}
		cur = next
	}
	return l, nil
}

// DiffSnapshotVersions loads the two requested snapshot versions from
// the backup directory and diffs them. from and to are unordered
// endpoints: added means present in `to` but not `from`. The diff is
// purely read-side — neither the live ledger nor any file is written.
func DiffSnapshotVersions(dir string, from, to uint64) (*SnapshotDiff, error) {
	fromL, err := LoadSnapshotAtVersion(dir, from)
	if err != nil {
		return nil, err
	}
	toL, err := LoadSnapshotAtVersion(dir, to)
	if err != nil {
		return nil, err
	}
	d := DiffSnapshots(fromL, toL)
	d.FromVersion = from
	d.ToVersion = to
	return &d, nil
}

// ledgerDiffView is a consistent read-side copy of the fields the diff
// compares, taken under the ledger's read lock so the diff itself is
// pure and lock-free.
type ledgerDiffView struct {
	balances    map[accountCurrency]int64
	entries     map[string]JournalEntry
	frozen      map[AccountID]bool
	frozenUntil map[AccountID]time.Time
}

// diffViewOf copies the diff-relevant state under the read lock.
func (l *Ledger) diffViewOf() ledgerDiffView {
	l.mu.RLock()
	defer l.mu.RUnlock()
	v := ledgerDiffView{
		balances:    make(map[accountCurrency]int64, len(l.balances)),
		entries:     make(map[string]JournalEntry, len(l.entries)),
		frozen:      make(map[AccountID]bool, len(l.frozen)),
		frozenUntil: make(map[AccountID]time.Time, len(l.frozenUntil)),
	}
	for k, b := range l.balances {
		v.balances[k] = b
	}
	for id, e := range l.entries {
		v.entries[id] = e
	}
	for a := range l.frozen {
		v.frozen[a] = true
	}
	for a, until := range l.frozenUntil {
		v.frozenUntil[a] = until
	}
	return v
}

// DiffSnapshots computes the three-dimension diff between two ledgers
// (normally both produced by LoadSnapshotAtVersion). It copies each
// ledger's state under its own read lock and then diffs the copies, so
// the comparison itself takes no locks and writes nothing.
func DiffSnapshots(from, to *Ledger) SnapshotDiff {
	fv, tv := from.diffViewOf(), to.diffViewOf()
	return SnapshotDiff{
		Balances: diffBalances(fv.balances, tv.balances),
		Entries:  diffEntries(fv.entries, tv.entries),
		Frozen:   diffFrozen(fv.frozen, fv.frozenUntil, tv.frozen, tv.frozenUntil),
	}
}

func diffBalances(from, to map[accountCurrency]int64) BalanceDiffSection {
	var sec BalanceDiffSection
	for k, tb := range to {
		fb, ok := from[k]
		switch {
		case !ok:
			sec.Added = append(sec.Added, BalanceDiffRow{Account: k.account, Currency: k.currency, ToCents: tb, DeltaCents: tb})
		case fb != tb:
			sec.Changed = append(sec.Changed, BalanceDiffRow{Account: k.account, Currency: k.currency, FromCents: fb, ToCents: tb, DeltaCents: tb - fb})
		}
	}
	for k, fb := range from {
		if _, ok := to[k]; !ok {
			sec.Deleted = append(sec.Deleted, BalanceDiffRow{Account: k.account, Currency: k.currency, FromCents: fb, DeltaCents: -fb})
		}
	}
	sort.Slice(sec.Added, func(i, j int) bool {
		return balanceRowLess(sec.Added[i], sec.Added[j])
	})
	sort.Slice(sec.Deleted, func(i, j int) bool {
		return balanceRowLess(sec.Deleted[i], sec.Deleted[j])
	})
	sort.Slice(sec.Changed, func(i, j int) bool {
		return balanceRowLess(sec.Changed[i], sec.Changed[j])
	})
	return sec
}

func balanceRowLess(a, b BalanceDiffRow) bool {
	if a.Account != b.Account {
		return a.Account < b.Account
	}
	return a.Currency < b.Currency
}

// entryChangedFields names the JournalEntry fields that differ between
// two rows with the same ID, mirroring journalEntriesEqual's field set.
func entryChangedFields(a, b JournalEntry) []string {
	var fields []string
	if a.DebitAccount != b.DebitAccount {
		fields = append(fields, "debit_account")
	}
	if a.CreditAccount != b.CreditAccount {
		fields = append(fields, "credit_account")
	}
	if a.AmountCents != b.AmountCents {
		fields = append(fields, "amount_cents")
	}
	if a.Currency != b.Currency {
		fields = append(fields, "currency")
	}
	if a.IdempotencyKey != b.IdempotencyKey {
		fields = append(fields, "idempotency_key")
	}
	if a.BatchID != b.BatchID {
		fields = append(fields, "batch_id")
	}
	if a.Memo != b.Memo {
		fields = append(fields, "memo")
	}
	if a.ExternalRef != b.ExternalRef {
		fields = append(fields, "external_ref")
	}
	if a.PendingReview != b.PendingReview {
		fields = append(fields, "pending_review")
	}
	if a.CreatedAt.UnixNano() != b.CreatedAt.UnixNano() {
		fields = append(fields, "created_at")
	}
	return fields
}

func diffEntries(from, to map[string]JournalEntry) EntryDiffSection {
	var sec EntryDiffSection
	for id, te := range to {
		fe, ok := from[id]
		switch {
		case !ok:
			sec.Added = append(sec.Added, te)
		default:
			if fields := entryChangedFields(fe, te); len(fields) > 0 {
				sec.Changed = append(sec.Changed, EntryDiffRow{ID: id, ChangedFields: fields, From: fe, To: te})
			}
		}
	}
	for id, fe := range from {
		if _, ok := to[id]; !ok {
			sec.Deleted = append(sec.Deleted, fe)
		}
	}
	sort.Slice(sec.Added, func(i, j int) bool { return sec.Added[i].ID < sec.Added[j].ID })
	sort.Slice(sec.Deleted, func(i, j int) bool { return sec.Deleted[i].ID < sec.Deleted[j].ID })
	sort.Slice(sec.Changed, func(i, j int) bool { return sec.Changed[i].ID < sec.Changed[j].ID })
	return sec
}

func diffFrozen(from map[AccountID]bool, fromTimes map[AccountID]time.Time, to map[AccountID]bool, toTimes map[AccountID]time.Time) FrozenDiffSection {
	var sec FrozenDiffSection
	for a := range to {
		if !from[a] {
			row := FrozenDiffRow{Account: a}
			if t, ok := toTimes[a]; ok {
				tc := t
				row.ToAutoUnfreezeAt = &tc
			}
			sec.Added = append(sec.Added, row)
			continue
		}
		ft, fok := fromTimes[a]
		tt, tok := toTimes[a]
		if fok != tok || (fok && !ft.Equal(tt)) {
			var fr, tr *time.Time
			if fok {
				fc := ft
				fr = &fc
			}
			if tok {
				tc := tt
				tr = &tc
			}
			sec.Changed = append(sec.Changed, FrozenDiffRow{Account: a, FromAutoUnfreezeAt: fr, ToAutoUnfreezeAt: tr})
		}
	}
	for a := range from {
		if !to[a] {
			row := FrozenDiffRow{Account: a}
			if t, ok := fromTimes[a]; ok {
				fc := t
				row.FromAutoUnfreezeAt = &fc
			}
			sec.Deleted = append(sec.Deleted, row)
		}
	}
	sort.Slice(sec.Added, func(i, j int) bool { return sec.Added[i].Account < sec.Added[j].Account })
	sort.Slice(sec.Deleted, func(i, j int) bool { return sec.Deleted[i].Account < sec.Deleted[j].Account })
	sort.Slice(sec.Changed, func(i, j int) bool { return sec.Changed[i].Account < sec.Changed[j].Account })
	return sec
}
