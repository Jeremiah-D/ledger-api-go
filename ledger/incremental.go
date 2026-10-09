package ledger

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"
	"time"
)

// Incremental snapshots: bandwidth-efficient backup and replica sync.
//
// A full snapshot (see snapshot.go) exports the entire journal. An
// incremental snapshot exports only what changed since a base version:
// the chain links (and their journal entries) with seq > base_version,
// plus the full holds section, the full idempotency registries, and the
// current config. The journal is the volume — that is what stays
// incremental; holds, registries, and config are compact state that is
// always cheap to carry whole, and carrying them whole keeps the merge
// rules simple and auditable.
//
// ExportIncrementalSnapshot(w, sinceVersion) exports the delta from
// sinceVersion to the current version. ImportIncrementalSnapshot(r)
// applies such a delta onto a live ledger under the write lock: it
// requires the delta's base_version to equal the ledger's current
// version (continuity — a gap or an overlap is rejected, never silently
// skipped or double-applied), folds the new entries in chain order,
// merges holds and idempotency registries (identical rows merge cleanly;
// conflicting rows reject the import), applies the latest config, and
// finishes with a full VerifyChain over the whole chain — the new links
// hash onto the old chain's head, so the verification proves the delta
// continues exactly where the ledger left off.

// incrementalMetaLine is the first line of every incremental snapshot.
// The "base_version" field is what distinguishes it from a full snapshot
// (ImportSnapshot refuses a meta carrying it).
type incrementalMetaLine struct {
	Record      string    `json:"record"`
	Format      int       `json:"format"`
	BaseVersion uint64    `json:"base_version"`
	Version     uint64    `json:"version"`
	ExportedAt  time.Time `json:"exported_at"`
	EntryCount  int       `json:"entry_count"`
	ChainLinks  int       `json:"chain_links"`
	HoldCount   int       `json:"hold_count"`
}

// ExportIncrementalSnapshot writes a JSONL incremental snapshot of the
// ledger to w: the journal delta from sinceVersion (exclusive) to the
// current version, plus full holds, idempotency registries, and config.
// The export holds the read lock for its whole duration, so the document
// describes one consistent point in time. sinceVersion must not exceed
// the current version; an empty delta (sinceVersion == current version)
// is legal and exports only the compact sections, which lets a replica
// sync config and registry state without new journal rows. Deterministic
// ordering means identical ledger state exports to identical bytes.
func (l *Ledger) ExportIncrementalSnapshot(w io.Writer, sinceVersion uint64) error {
	l.mu.RLock()
	defer l.mu.RUnlock()

	if sinceVersion > l.version {
		return fmt.Errorf("ledger: incremental snapshot: sinceVersion %d exceeds ledger version %d",
			sinceVersion, l.version)
	}

	enc := json.NewEncoder(w)
	meta := incrementalMetaLine{
		Record:      "meta",
		Format:      snapshotFormatVersion,
		BaseVersion: sinceVersion,
		Version:     l.version,
		ExportedAt:  time.Now().UTC(),
		EntryCount:  int(l.version - sinceVersion),
		ChainLinks:  int(l.version - sinceVersion),
		HoldCount:   len(l.holds),
	}
	if err := enc.Encode(meta); err != nil {
		return fmt.Errorf("ledger: incremental snapshot export: %w", err)
	}

	// The journal delta, in chain (Post) order: chain seq numbers are
	// 1-based and equal the ledger version at Post time, so links with
	// index >= sinceVersion (0-based) are exactly the new rows.
	for _, link := range l.chain[sinceVersion:] {
		e, ok := l.entries[link.entryID]
		if !ok {
			return fmt.Errorf("ledger: incremental snapshot export: chain references missing entry %q", link.entryID)
		}
		if err := enc.Encode(snapshotEntryLine{Record: "entry", Entry: e}); err != nil {
			return fmt.Errorf("ledger: incremental snapshot export: %w", err)
		}
		if err := enc.Encode(snapshotLinkLine{
			Record:   "link",
			Seq:      link.seq,
			EntryID:  link.entryID,
			PrevHash: hex.EncodeToString(link.prevHash[:]),
			Hash:     hex.EncodeToString(link.hash[:]),
		}); err != nil {
			return fmt.Errorf("ledger: incremental snapshot export: %w", err)
		}
	}

	if err := l.exportHoldsLocked(enc); err != nil {
		return err
	}
	if err := l.exportIdempotencyLocked(enc); err != nil {
		return err
	}
	if err := l.exportConfigLocked(enc); err != nil {
		return err
	}
	return nil
}

// ImportIncrementalSnapshot applies an incremental snapshot produced by
// ExportIncrementalSnapshot onto the ledger, under the write lock: the
// ledger is the replica, the snapshot is the delta.
//
// Continuity is the core invariant: the delta's base_version must equal
// the ledger's current version. A delta from an older version (overlap)
// or a newer version (gap) is rejected with an error wrapping
// ErrSnapshotInvalid — a replica that missed a delta must re-sync from
// the base it actually has, never skip. New journal entries must be
// absent from the ledger (a present entry is an overlap, rejected, not
// merged), and link seqs must continue the chain without a break.
//
// Holds and idempotency registries merge: a row identical to the
// ledger's (compared field by field, instants by UnixNano) is a clean
// merge; a row that differs under an existing ID or key is corruption
// and rejects the import. Registry references are checked like in a
// full import: a row pointing at a journal row or hold that does not
// exist after folding is rejected.
//
// The config section carries the latest operational config and is
// applied wholesale (see applySnapshotConfigLocked).
//
// The import finishes with a full audit-chain verification over the
// whole chain — old and new links: the new links' prevHash values chain
// onto the old head, so the verification proves the delta continues the
// exact chain the ledger had. The first problem found rejects the whole
// import with an error wrapping ErrSnapshotInvalid; the ledger is left
// untouched on failure.
func (l *Ledger) ImportIncrementalSnapshot(r io.Reader) error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrSnapshotInvalid, fmt.Sprintf(format, args...))
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)

	var meta *incrementalMetaLine
	entries := map[string]JournalEntry{}
	var linkOrder []snapshotLinkLine
	holds := map[string]Hold{}
	byKey := map[string]JournalEntry{}
	transferKeys := map[string][]string{}
	holdKeys := map[string]string{}
	captureKeys := map[string]CaptureReceipt{}
	sweepKeys := map[string]sweepRecord{}
	var cfg *snapshotConfigLine
	lineNo := 0

	for sc.Scan() {
		lineNo++
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var env struct {
			Record string `json:"record"`
		}
		if err := json.Unmarshal(line, &env); err != nil {
			return fail("line %d: malformed JSON: %v", lineNo, err)
		}
		switch env.Record {
		case "meta":
			if lineNo != 1 {
				return fail("line %d: meta record must be the first line", lineNo)
			}
			if meta != nil {
				return fail("line %d: duplicate meta record", lineNo)
			}
			var m incrementalMetaLine
			if err := json.Unmarshal(line, &m); err != nil {
				return fail("line %d: malformed meta: %v", lineNo, err)
			}
			if m.Format != snapshotFormatVersion {
				return fail("line %d: unsupported snapshot format %d (this build reads %d)",
					lineNo, m.Format, snapshotFormatVersion)
			}
			meta = &m
		case "entry":
			if meta == nil {
				return fail("line %d: entry before meta", lineNo)
			}
			var el snapshotEntryLine
			if err := json.Unmarshal(line, &el); err != nil {
				return fail("line %d: malformed entry: %v", lineNo, err)
			}
			if el.Entry.ID == "" {
				return fail("line %d: entry with empty ID", lineNo)
			}
			if _, dup := entries[el.Entry.ID]; dup {
				return fail("line %d: duplicate entry ID %q", lineNo, el.Entry.ID)
			}
			entries[el.Entry.ID] = el.Entry
		case "link":
			if meta == nil {
				return fail("line %d: link before meta", lineNo)
			}
			var ll snapshotLinkLine
			if err := json.Unmarshal(line, &ll); err != nil {
				return fail("line %d: malformed link: %v", lineNo, err)
			}
			// Link seqs must continue the ledger's chain without a
			// break: the first new link is version+1.
			if want := l.version + uint64(len(linkOrder)) + 1; ll.Seq != want {
				return fail("line %d: link seq %d breaks chain continuity, want %d", lineNo, ll.Seq, want)
			}
			if len(ll.PrevHash) != 64 || len(ll.Hash) != 64 {
				return fail("line %d: link %d has malformed hashes", lineNo, ll.Seq)
			}
			linkOrder = append(linkOrder, ll)
		case "hold":
			if meta == nil {
				return fail("line %d: hold before meta", lineNo)
			}
			var hl snapshotHoldLine
			if err := json.Unmarshal(line, &hl); err != nil {
				return fail("line %d: malformed hold: %v", lineNo, err)
			}
			if hl.Hold.ID == "" {
				return fail("line %d: hold with empty ID", lineNo)
			}
			if _, dup := holds[hl.Hold.ID]; dup {
				return fail("line %d: duplicate hold ID %q", lineNo, hl.Hold.ID)
			}
			holds[hl.Hold.ID] = hl.Hold
		case "idempotency":
			if meta == nil {
				return fail("line %d: idempotency record before meta", lineNo)
			}
			var il snapshotIdempotencyLine
			if err := json.Unmarshal(line, &il); err != nil {
				return fail("line %d: malformed idempotency record: %v", lineNo, err)
			}
			if il.Key == "" {
				return fail("line %d: idempotency record with empty key", lineNo)
			}
			switch il.Namespace {
			case snapshotNSEntry:
				if il.Entry == nil {
					return fail("line %d: entry-namespace record without entry", lineNo)
				}
				if _, dup := byKey[il.Key]; dup {
					return fail("line %d: duplicate idempotency key %q", lineNo, il.Key)
				}
				byKey[il.Key] = *il.Entry
			case snapshotNSTransfer:
				if _, dup := transferKeys[il.Key]; dup {
					return fail("line %d: duplicate idempotency key %q", lineNo, il.Key)
				}
				transferKeys[il.Key] = il.EntryIDs
			case snapshotNSHold:
				if il.HoldID == "" {
					return fail("line %d: hold-namespace record without hold ID", lineNo)
				}
				if _, dup := holdKeys[il.Key]; dup {
					return fail("line %d: duplicate idempotency key %q", lineNo, il.Key)
				}
				holdKeys[il.Key] = il.HoldID
			case snapshotNSCapture:
				if il.Receipt == nil {
					return fail("line %d: capture-namespace record without receipt", lineNo)
				}
				if _, dup := captureKeys[il.Key]; dup {
					return fail("line %d: duplicate idempotency key %q", lineNo, il.Key)
				}
				captureKeys[il.Key] = *il.Receipt
			case snapshotNSSweep:
				if il.Sweep == nil {
					return fail("line %d: sweep-namespace record without sweep", lineNo)
				}
				if _, dup := sweepKeys[il.Key]; dup {
					return fail("line %d: duplicate idempotency key %q", lineNo, il.Key)
				}
				sweepKeys[il.Key] = sweepRecord{
					sweepID:   il.Sweep.SweepID,
					entryIDs:  il.Sweep.EntryIDs,
					createdAt: il.Sweep.CreatedAt,
				}
			default:
				return fail("line %d: unknown idempotency namespace %q", lineNo, il.Namespace)
			}
		case "config":
			if meta == nil {
				return fail("line %d: config before meta", lineNo)
			}
			if cfg != nil {
				return fail("line %d: duplicate config record", lineNo)
			}
			var c snapshotConfigLine
			if err := json.Unmarshal(line, &c); err != nil {
				return fail("line %d: malformed config: %v", lineNo, err)
			}
			cfg = &c
		default:
			return fail("line %d: unknown record type %q", lineNo, env.Record)
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("ledger: incremental snapshot import: read: %w", err)
	}
	if meta == nil {
		return fail("empty snapshot: missing meta record")
	}

	// Continuity: the delta must start exactly where this ledger is.
	if meta.BaseVersion != l.version {
		return fail("base version %d does not match ledger version %d: re-sync from the ledger's actual version",
			meta.BaseVersion, l.version)
	}
	if meta.Version < meta.BaseVersion {
		return fail("meta version %d is older than base version %d", meta.Version, meta.BaseVersion)
	}
	if len(entries) != meta.EntryCount {
		return fail("entry count %d does not match meta entry_count %d", len(entries), meta.EntryCount)
	}
	if len(linkOrder) != meta.ChainLinks {
		return fail("link count %d does not match meta chain_links %d", len(linkOrder), meta.ChainLinks)
	}
	if meta.Version != meta.BaseVersion+uint64(len(linkOrder)) {
		return fail("meta version %d inconsistent with base %d + %d links",
			meta.Version, meta.BaseVersion, len(linkOrder))
	}
	if len(holds) != meta.HoldCount {
		return fail("hold count %d does not match meta hold_count %d", len(holds), meta.HoldCount)
	}
	if cfg == nil {
		return fail("missing config record")
	}

	// Validate everything before mutating anything: the ledger is left
	// untouched when the import is rejected. First the staged journal:
	// every new entry must be absent from the ledger (a present entry is
	// an overlap, rejected, not merged), and the staged links must hash
	// onto the ledger's current chain head — a splice, a reordered link,
	// or a tampered entry fails here, before the first mutation.
	staged := make([]JournalEntry, 0, len(linkOrder))
	stagedLinks := make([]chainLink, 0, len(linkOrder))
	stagedByID := make(map[string]JournalEntry, len(linkOrder))
	var head [32]byte
	if n := len(l.chain); n > 0 {
		head = l.chain[n-1].hash
	}
	for _, ll := range linkOrder {
		e, ok := entries[ll.EntryID]
		if !ok {
			return fail("link seq %d references missing entry %q", ll.Seq, ll.EntryID)
		}
		if _, exists := l.entries[e.ID]; exists {
			return fail("entry %q is already journaled: delta overlaps the ledger; re-sync from version %d",
				e.ID, l.version)
		}
		prevRaw, err := hex.DecodeString(ll.PrevHash)
		if err != nil {
			return fail("link seq %d: bad prev_hash hex: %v", ll.Seq, err)
		}
		hashRaw, err := hex.DecodeString(ll.Hash)
		if err != nil {
			return fail("link seq %d: bad hash hex: %v", ll.Seq, err)
		}
		var ph, h [32]byte
		copy(ph[:], prevRaw)
		copy(h[:], hashRaw)
		if ph != head {
			return fail("link seq %d does not extend the ledger's chain head (spliced delta)", ll.Seq)
		}
		if got := hashChainLink(head, e); got != h {
			return fail("journal entry %q was modified after posting (hash mismatch)", e.ID)
		}
		head = h
		staged = append(staged, e)
		stagedLinks = append(stagedLinks, chainLink{seq: ll.Seq, entryID: ll.EntryID, prevHash: ph, hash: h})
		stagedByID[e.ID] = e
	}

	// Then the merges, still without mutating: holds and idempotency
	// rows. Identical rows merge cleanly; rows that differ under an
	// existing ID or key are corruption and reject the import.
	// Referential checks run against the journal as it will be after the
	// fold (existing entries plus staged ones).
	holdList := make([]Hold, 0, len(holds))
	for _, h := range holds {
		holdList = append(holdList, h)
	}
	sort.Slice(holdList, func(i, j int) bool {
		if !holdList[i].CreatedAt.Equal(holdList[j].CreatedAt) {
			return holdList[i].CreatedAt.Before(holdList[j].CreatedAt)
		}
		return holdList[i].ID < holdList[j].ID
	})
	stagedHolds := make(map[string]Hold, len(holdList))
	for _, h := range holdList {
		if existing, ok := l.holds[h.ID]; ok && !holdsEqual(existing, h) {
			return fail("hold %q conflicts with the ledger's hold: refusing merge", h.ID)
		}
		stagedHolds[h.ID] = h
	}
	entryVisible := func(id string) (JournalEntry, bool) {
		if e, ok := l.entries[id]; ok {
			return e, true
		}
		e, ok := stagedByID[id]
		return e, ok
	}
	holdVisible := func(id string) bool {
		if _, ok := l.holds[id]; ok {
			return true
		}
		_, ok := stagedHolds[id]
		return ok
	}
	for k, e := range byKey {
		j, ok := entryVisible(e.ID)
		if !ok {
			return fail("idempotency key %q references missing journal entry %q", k, e.ID)
		}
		if !journalEntriesEqual(j, e) {
			return fail("idempotency key %q registry entry differs from journal entry %q", k, e.ID)
		}
		if existing, ok := l.byKey[k]; ok && !journalEntriesEqual(existing, e) {
			return fail("idempotency key %q conflicts with the ledger's registry: refusing merge", k)
		}
	}
	for k, ids := range transferKeys {
		for _, id := range ids {
			if _, ok := entryVisible(id); !ok {
				return fail("transfer idempotency key %q references missing entry %q", k, id)
			}
		}
		if existing, ok := l.transferKeys[k]; ok && !reflect.DeepEqual(existing, ids) {
			return fail("transfer idempotency key %q conflicts with the ledger's registry: refusing merge", k)
		}
	}
	for k, id := range holdKeys {
		if !holdVisible(id) {
			return fail("hold idempotency key %q references missing hold %q", k, id)
		}
		if existing, ok := l.holdKeys[k]; ok && existing != id {
			return fail("hold idempotency key %q conflicts with the ledger's registry: refusing merge", k)
		}
	}
	for k, rc := range captureKeys {
		if !holdVisible(rc.HoldID) {
			return fail("capture idempotency key %q references missing hold %q", k, rc.HoldID)
		}
		if _, ok := entryVisible(rc.Entry.ID); !ok {
			return fail("capture idempotency key %q references missing entry %q", k, rc.Entry.ID)
		}
		if existing, ok := l.captureKeys[k]; ok {
			if existing.CaptureID != rc.CaptureID || existing.HoldID != rc.HoldID ||
				existing.CapturedCents != rc.CapturedCents || existing.ReleasedCents != rc.ReleasedCents ||
				existing.Duplicate != rc.Duplicate || !journalEntriesEqual(existing.Entry, rc.Entry) {
				return fail("capture idempotency key %q conflicts with the ledger's registry: refusing merge", k)
			}
		}
	}
	for k, rec := range sweepKeys {
		for _, id := range rec.entryIDs {
			if _, ok := entryVisible(id); !ok {
				return fail("sweep idempotency key %q references missing entry %q", k, id)
			}
		}
		if existing, ok := l.sweepKeys[k]; ok {
			if existing.sweepID != rec.sweepID || !reflect.DeepEqual(existing.entryIDs, rec.entryIDs) ||
				existing.createdAt.UnixNano() != rec.createdAt.UnixNano() {
				return fail("sweep idempotency key %q conflicts with the ledger's registry: refusing merge", k)
			}
		}
	}

	// Commit: everything above checked out, so nothing below can fail.
	// The journal fold applies the same effects commitEntryLocked does,
	// in chain order.
	for i, e := range staged {
		l.entries[e.ID] = e
		l.byAccount[e.DebitAccount] = append(l.byAccount[e.DebitAccount], e.ID)
		l.byAccount[e.CreditAccount] = append(l.byAccount[e.CreditAccount], e.ID)
		debitKey := accountCurrency{account: e.DebitAccount, currency: e.Currency}
		creditKey := accountCurrency{account: e.CreditAccount, currency: e.Currency}
		l.balances[debitKey] += e.AmountCents
		l.balances[creditKey] -= e.AmountCents
		l.debitTotals[debitKey] += e.AmountCents
		l.creditTotals[creditKey] += e.AmountCents
		l.chain = append(l.chain, stagedLinks[i])
	}
	l.version = meta.Version
	for _, h := range holdList {
		if _, ok := l.holds[h.ID]; !ok {
			l.holds[h.ID] = h
			l.holdsByAccount[h.Account] = append(l.holdsByAccount[h.Account], h.ID)
		}
	}
	for k, e := range byKey {
		if _, ok := l.byKey[k]; !ok {
			l.byKey[k] = e
		}
	}
	for k, ids := range transferKeys {
		if _, ok := l.transferKeys[k]; !ok {
			l.transferKeys[k] = ids
		}
	}
	for k, id := range holdKeys {
		if _, ok := l.holdKeys[k]; !ok {
			l.holdKeys[k] = id
		}
	}
	for k, rc := range captureKeys {
		if _, ok := l.captureKeys[k]; !ok {
			l.captureKeys[k] = rc
		}
	}
	for k, rec := range sweepKeys {
		if _, ok := l.sweepKeys[k]; !ok {
			l.sweepKeys[k] = rec
		}
	}
	// The config section carries the latest operational config and
	// applies wholesale; it was written by the same export, and
	// applySnapshotConfigLocked validates before applying, so a failure
	// here is unreachable after the validations above.
	if err := l.applySnapshotConfigLocked(cfg); err != nil {
		return fail("config apply failed after validation: %v", err)
	}

	// The whole point: every staged link was already proven to hash onto
	// the old head during validation, so this full verification is the
	// belt to that suspender — a final proof that the folded chain is
	// intact, old and new links alike.
	if err := l.verifyChainLocked(); err != nil {
		return fmt.Errorf("%w: audit chain verification failed: %v", ErrSnapshotInvalid, err)
	}
	return nil
}
