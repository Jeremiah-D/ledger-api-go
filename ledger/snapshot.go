package ledger

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"time"
)

// Disaster-recovery snapshots.
//
// A snapshot is a JSONL document: one JSON object per line, each carrying a
// "record" discriminator. The layout is append-ordered and deterministic so
// two exports of the same ledger state are byte-identical:
//
//	{"record":"meta", ...}            exactly one, always the first line
//	{"record":"entry", ...}           journal entries, in audit-chain (Post) order
//	{"record":"link", ...}            audit-chain links, seq 1..N, matching entry order
//	{"record":"hold", ...}            authorization holds, by (CreatedAt, ID)
//	{"record":"idempotency", ...}     the seven idempotency-key namespaces
//	{"record":"config", ...}          operational config (frozen, overdraft, hierarchy, fees, TTL, daily limits)
//
// The export carries only source-of-truth rows: journal entries, chain
// links, holds, idempotency registries, and operational config. Derived
// state (balances, debit/credit totals, per-account indexes) is recomputed
// on import by folding the journal in chain order, which also cross-checks
// the folding logic against live Post behavior. Import finishes with a
// full VerifyChain: any tampered amount, rewritten entry, spliced link, or
// reordered journal rejects the import with an error — a snapshot that
// cannot prove its own integrity is refused, never half-restored.
//
// A ledger restored by ImportSnapshot is a fully working ledger: Post,
// Reconcile, BalanceAt, and every other read keep working, and Reconcile
// can be rerun against the restored copy (the end-of-day report is how
// operators confirm a restored backup before promoting it).
//
// Timestamps round-trip through RFC 3339 with nanosecond precision, which
// preserves CreatedAt.UnixNano() exactly — the value the audit-chain hash
// is computed over. A snapshot must be exported and imported with the same
// chain-hash semantics; snapshotFormatVersion guards that.

// snapshotFormatVersion pins the export layout and the chain-hash input
// encoding it is verified against. Bump it if either ever changes.
const snapshotFormatVersion = 1

// ErrSnapshotInvalid is returned by ImportSnapshot when the snapshot cannot
// be trusted: malformed JSONL, a missing or duplicated section, dangling
// registry references, or an audit chain that fails verification. The
// error wraps details about the first problem found; no ledger is
// returned on failure.
var ErrSnapshotInvalid = errors.New("ledger: invalid snapshot")

// snapshotMetaLine is the first line of every snapshot.
type snapshotMetaLine struct {
	Record     string    `json:"record"`
	Format     int       `json:"format"`
	ExportedAt time.Time `json:"exported_at"`
	Version    uint64    `json:"version"`
	EntryCount int       `json:"entry_count"`
	ChainLinks int       `json:"chain_links"`
	HoldCount  int       `json:"hold_count"`
}

// snapshotEntryLine carries one journal entry, in chain order.
type snapshotEntryLine struct {
	Record string       `json:"record"`
	Entry  JournalEntry `json:"entry"`
}

// snapshotLinkLine carries one audit-chain link, seq 1..N.
type snapshotLinkLine struct {
	Record   string `json:"record"`
	Seq      uint64 `json:"seq"`
	EntryID  string `json:"entry_id"`
	PrevHash string `json:"prev_hash"`
	Hash     string `json:"hash"`
}

// snapshotHoldLine carries one authorization hold.
type snapshotHoldLine struct {
	Record string `json:"record"`
	Hold   Hold   `json:"hold"`
}

// snapshotSweep is the exportable form of the unexported sweepRecord.
type snapshotSweep struct {
	SweepID   string    `json:"sweep_id"`
	EntryIDs  []string  `json:"entry_ids"`
	CreatedAt time.Time `json:"created_at"`
}

// snapshotBatch is the exportable form of the unexported batchRecord.
type snapshotBatch struct {
	BatchID   string    `json:"batch_id"`
	EntryIDs  []string  `json:"entry_ids"`
	CreatedAt time.Time `json:"created_at"`
}

// snapshotMerge is the exportable form of the unexported mergeRecord:
// one committed account merge (see merge.go).
type snapshotMerge struct {
	MergeID   string     `json:"merge_id"`
	From      AccountID  `json:"from_account"`
	To        AccountID  `json:"to_account"`
	Legs      []MergeLeg `json:"legs"`
	EntryIDs  []string   `json:"entry_ids"`
	CreatedAt time.Time  `json:"created_at"`
}

// snapshotMergeLine carries one committed account merge, by merge ID.
type snapshotMergeLine struct {
	Record string        `json:"record"`
	Merge  snapshotMerge `json:"merge"`
}

// Idempotency-key namespaces in a snapshot. One record per key, so every
// namespace stays independently addressable on import.
const (
	snapshotNSEntry    = "entry"     // key -> JournalEntry (byKey)
	snapshotNSTransfer = "transfer"  // key -> []entry IDs (transferKeys)
	snapshotNSHold     = "hold"      // key -> hold ID (holdKeys)
	snapshotNSCapture  = "capture"   // key -> CaptureReceipt (captureKeys)
	snapshotNSSweep    = "sweep"     // key -> snapshotSweep (sweepKeys)
	snapshotNSMergeKey = "merge_key" // key -> merge ID (mergeKeys)
	snapshotNSBatch    = "batch"     // key -> snapshotBatch (batchKeys)
)

// snapshotIdempotencyLine carries one idempotency-key registration. Only
// the payload field matching Namespace is populated.
type snapshotIdempotencyLine struct {
	Record    string          `json:"record"`
	Namespace string          `json:"namespace"`
	Key       string          `json:"key"`
	Entry     *JournalEntry   `json:"entry,omitempty"`
	EntryIDs  []string        `json:"entry_ids,omitempty"`
	HoldID    string          `json:"hold_id,omitempty"`
	Receipt   *CaptureReceipt `json:"receipt,omitempty"`
	Sweep     *snapshotSweep  `json:"sweep,omitempty"`
	MergeID   string          `json:"merge_id,omitempty"`
	Batch     *snapshotBatch  `json:"batch,omitempty"`
}

// snapshotConfigLine carries operational config that a faithful restore
// needs: frozen and overdraft-protected accounts (replay checks order
// against them), the sub-account hierarchy, the transfer fee policy, and
// the idempotency TTL (key-expiry behavior must survive a restore).
// FeeTiers carries the tiered schedule; FeeRateBps is kept for snapshots
// written before the tiered schedule existed (a flat single-tier policy)
// and is only read when FeeTiers is absent.
type snapshotConfigLine struct {
	Record             string                  `json:"record"`
	Frozen             []AccountID             `json:"frozen"`
	OverdraftProtected []AccountID             `json:"overdraft_protected"`
	Parents            map[AccountID]AccountID `json:"parents"`
	FeeRateBps         int64                   `json:"fee_rate_bps"`
	FeeTiers           []FeeTier               `json:"fee_tiers,omitempty"`
	FeeRevenueAccount  AccountID               `json:"fee_revenue_account"`
	IdempotencyTTL     string                  `json:"idempotency_ttl"`
	// DailyLimits carries the daily outflow limits (see SetDailyLimit);
	// absent in snapshots written before daily limits existed. The
	// accumulated per-day outflow counters are deliberately NOT restored:
	// a disaster-recovery restore resets the current day's counters to
	// zero, which fail-opens for the remainder of that day — operators
	// should treat a restore as a risk-control reset event.
	DailyLimits []DailyLimit `json:"daily_limits,omitempty"`
	// FXRates carries the FX rate table (see SetFXRate) and FXAccount the
	// ledger-wide FX clearing account (see WithFXAccount); absent in
	// snapshots written before FX support existed. Restored rates keep
	// their EffectiveVersion, so restored ledgers disclose the same
	// conversion provenance as the original.
	FXRates   []ExchangeRate `json:"fx_rates,omitempty"`
	FXAccount AccountID      `json:"fx_account,omitempty"`
}

// flatFeeRateBps reports the fee rate for snapshots read by legacy
// consumers that only understand fee_rate_bps: it is the rate when the
// policy is a single flat tier starting at 0, and 0 otherwise (a tiered
// schedule has no single rate; legacy readers see the fee_tiers field).
func flatFeeRateBps(tiers []FeeTier) int64 {
	if len(tiers) == 1 && tiers[0].MinAmountCents == 0 {
		return tiers[0].RateBps
	}
	return 0
}

// ExportSnapshot writes a JSONL disaster-recovery snapshot of the ledger to
// w. The export holds the read lock for its whole duration, so the
// document describes one consistent point in time: a concurrent Post can
// never slip between the journal scan and the chain scan. Deterministic
// section and key ordering means identical ledger state exports to
// identical bytes.
func (l *Ledger) ExportSnapshot(w io.Writer) error {
	l.mu.RLock()
	defer l.mu.RUnlock()

	enc := json.NewEncoder(w)

	meta := snapshotMetaLine{
		Record:     "meta",
		Format:     snapshotFormatVersion,
		ExportedAt: time.Now().UTC(),
		Version:    l.version,
		EntryCount: len(l.chain),
		ChainLinks: len(l.chain),
		HoldCount:  len(l.holds),
	}
	if err := enc.Encode(meta); err != nil {
		return fmt.Errorf("ledger: snapshot export: %w", err)
	}

	// Entries and links in chain (Post) order. The link order is the
	// canonical journal order; import folds entries in this order.
	for _, link := range l.chain {
		e, ok := l.entries[link.entryID]
		if !ok {
			return fmt.Errorf("ledger: snapshot export: chain references missing entry %q", link.entryID)
		}
		if err := enc.Encode(snapshotEntryLine{Record: "entry", Entry: e}); err != nil {
			return fmt.Errorf("ledger: snapshot export: %w", err)
		}
		if err := enc.Encode(snapshotLinkLine{
			Record:   "link",
			Seq:      link.seq,
			EntryID:  link.entryID,
			PrevHash: hex.EncodeToString(link.prevHash[:]),
			Hash:     hex.EncodeToString(link.hash[:]),
		}); err != nil {
			return fmt.Errorf("ledger: snapshot export: %w", err)
		}
	}

	if err := l.exportHoldsLocked(enc); err != nil {
		return err
	}
	if err := l.exportMergesLocked(enc); err != nil {
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

// exportHoldsLocked writes the hold records, sorted by (CreatedAt, ID)
// for determinism. Callers must hold l.mu; the read lock suffices.
func (l *Ledger) exportHoldsLocked(enc *json.Encoder) error {
	holds := make([]Hold, 0, len(l.holds))
	for _, h := range l.holds {
		holds = append(holds, h)
	}
	sort.Slice(holds, func(i, j int) bool {
		if !holds[i].CreatedAt.Equal(holds[j].CreatedAt) {
			return holds[i].CreatedAt.Before(holds[j].CreatedAt)
		}
		return holds[i].ID < holds[j].ID
	})
	for _, h := range holds {
		if err := enc.Encode(snapshotHoldLine{Record: "hold", Hold: h}); err != nil {
			return fmt.Errorf("ledger: snapshot export: %w", err)
		}
	}
	return nil
}

// exportMergesLocked writes the merge records, sorted by merge ID for
// determinism. Callers must hold l.mu; the read lock suffices.
func (l *Ledger) exportMergesLocked(enc *json.Encoder) error {
	ids := make([]string, 0, len(l.merges))
	for id := range l.merges {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		rec := l.merges[id]
		if err := enc.Encode(snapshotMergeLine{Record: "merge", Merge: snapshotMerge{
			MergeID:   rec.mergeID,
			From:      rec.from,
			To:        rec.to,
			Legs:      rec.legs,
			EntryIDs:  rec.entryIDs,
			CreatedAt: rec.createdAt,
		}}); err != nil {
			return fmt.Errorf("ledger: snapshot export: %w", err)
		}
	}
	return nil
}

// exportIdempotencyLocked writes the seven idempotency-key namespaces, one
// record per key, sorted for determinism. Callers must hold l.mu; the
// read lock suffices.
func (l *Ledger) exportIdempotencyLocked(enc *json.Encoder) error {
	sortedKeys := func(keys []string) []string {
		out := append([]string(nil), keys...)
		sort.Strings(out)
		return out
	}
	byKeyList := make([]string, 0, len(l.byKey))
	for k := range l.byKey {
		byKeyList = append(byKeyList, k)
	}
	for _, k := range sortedKeys(byKeyList) {
		e := l.byKey[k]
		if err := enc.Encode(snapshotIdempotencyLine{
			Record: "idempotency", Namespace: snapshotNSEntry, Key: k, Entry: &e,
		}); err != nil {
			return fmt.Errorf("ledger: snapshot export: %w", err)
		}
	}
	transferKeyList := make([]string, 0, len(l.transferKeys))
	for k := range l.transferKeys {
		transferKeyList = append(transferKeyList, k)
	}
	for _, k := range sortedKeys(transferKeyList) {
		if err := enc.Encode(snapshotIdempotencyLine{
			Record: "idempotency", Namespace: snapshotNSTransfer, Key: k, EntryIDs: l.transferKeys[k],
		}); err != nil {
			return fmt.Errorf("ledger: snapshot export: %w", err)
		}
	}
	holdKeyList := make([]string, 0, len(l.holdKeys))
	for k := range l.holdKeys {
		holdKeyList = append(holdKeyList, k)
	}
	for _, k := range sortedKeys(holdKeyList) {
		if err := enc.Encode(snapshotIdempotencyLine{
			Record: "idempotency", Namespace: snapshotNSHold, Key: k, HoldID: l.holdKeys[k],
		}); err != nil {
			return fmt.Errorf("ledger: snapshot export: %w", err)
		}
	}
	captureKeyList := make([]string, 0, len(l.captureKeys))
	for k := range l.captureKeys {
		captureKeyList = append(captureKeyList, k)
	}
	for _, k := range sortedKeys(captureKeyList) {
		r := l.captureKeys[k]
		if err := enc.Encode(snapshotIdempotencyLine{
			Record: "idempotency", Namespace: snapshotNSCapture, Key: k, Receipt: &r,
		}); err != nil {
			return fmt.Errorf("ledger: snapshot export: %w", err)
		}
	}
	sweepKeyList := make([]string, 0, len(l.sweepKeys))
	for k := range l.sweepKeys {
		sweepKeyList = append(sweepKeyList, k)
	}
	for _, k := range sortedKeys(sweepKeyList) {
		rec := l.sweepKeys[k]
		if err := enc.Encode(snapshotIdempotencyLine{
			Record: "idempotency", Namespace: snapshotNSSweep, Key: k,
			Sweep: &snapshotSweep{SweepID: rec.sweepID, EntryIDs: rec.entryIDs, CreatedAt: rec.createdAt},
		}); err != nil {
			return fmt.Errorf("ledger: snapshot export: %w", err)
		}
	}
	mergeKeyList := make([]string, 0, len(l.mergeKeys))
	for k := range l.mergeKeys {
		mergeKeyList = append(mergeKeyList, k)
	}
	for _, k := range sortedKeys(mergeKeyList) {
		if err := enc.Encode(snapshotIdempotencyLine{
			Record: "idempotency", Namespace: snapshotNSMergeKey, Key: k,
			MergeID: l.mergeKeys[k],
		}); err != nil {
			return fmt.Errorf("ledger: snapshot export: %w", err)
		}
	}
	batchKeyList := make([]string, 0, len(l.batchKeys))
	for k := range l.batchKeys {
		batchKeyList = append(batchKeyList, k)
	}
	for _, k := range sortedKeys(batchKeyList) {
		rec := l.batchKeys[k]
		if err := enc.Encode(snapshotIdempotencyLine{
			Record: "idempotency", Namespace: snapshotNSBatch, Key: k,
			Batch: &snapshotBatch{BatchID: rec.batchID, EntryIDs: rec.entryIDs, CreatedAt: rec.createdAt},
		}); err != nil {
			return fmt.Errorf("ledger: snapshot export: %w", err)
		}
	}
	return nil
}

// exportConfigLocked writes the operational config record. Callers must
// hold l.mu; the read lock suffices.
func (l *Ledger) exportConfigLocked(enc *json.Encoder) error {
	frozen := make([]AccountID, 0, len(l.frozen))
	for a := range l.frozen {
		frozen = append(frozen, a)
	}
	sort.Slice(frozen, func(i, j int) bool { return frozen[i] < frozen[j] })
	overdraft := make([]AccountID, 0, len(l.noOverdraft))
	for a := range l.noOverdraft {
		overdraft = append(overdraft, a)
	}
	sort.Slice(overdraft, func(i, j int) bool { return overdraft[i] < overdraft[j] })
	parents := make(map[AccountID]AccountID, len(l.parents))
	for c, p := range l.parents {
		parents[c] = p
	}
	cfg := snapshotConfigLine{
		Record:             "config",
		Frozen:             frozen,
		OverdraftProtected: overdraft,
		Parents:            parents,
		FeeRateBps:         flatFeeRateBps(l.feeTiers),
		FeeTiers:           l.feeTiers,
		FeeRevenueAccount:  l.feeRevenueAccount,
		IdempotencyTTL:     l.idempotencyTTL.String(),
		DailyLimits:        l.dailyLimitsLocked(),
		FXRates:            l.fxRatesLocked(),
		FXAccount:          l.fxAccount,
	}
	if err := enc.Encode(cfg); err != nil {
		return fmt.Errorf("ledger: snapshot export: %w", err)
	}
	return nil
}

// journalEntriesEqual compares two journal entries field by field,
// ignoring time.Location representation: JSON round-trips preserve the
// instant (and thus UnixNano, which is what the chain hash covers) but not
// the exact Location pointer.
func journalEntriesEqual(a, b JournalEntry) bool {
	return a.ID == b.ID &&
		a.DebitAccount == b.DebitAccount &&
		a.CreditAccount == b.CreditAccount &&
		a.AmountCents == b.AmountCents &&
		a.Currency == b.Currency &&
		a.IdempotencyKey == b.IdempotencyKey &&
		a.BatchID == b.BatchID &&
		a.CreatedAt.UnixNano() == b.CreatedAt.UnixNano()
}

// ImportSnapshot reads a disaster-recovery snapshot produced by
// ExportSnapshot and rebuilds a working Ledger from it: journal rows,
// derived balances/totals/indexes (refolded in chain order), holds,
// idempotency registries, and operational config.
//
// Every structural invariant is checked on the way in — section order and
// counts, link seq continuity, entry/link correspondence, registry
// references — and the import finishes with a full audit-chain
// verification. The first problem found rejects the whole import with an
// error wrapping ErrSnapshotInvalid; no ledger is returned, so callers
// can never observe a half-restored ledger.
func ImportSnapshot(r io.Reader) (*Ledger, error) {
	fail := func(format string, args ...any) (*Ledger, error) {
		return nil, fmt.Errorf("%w: %s", ErrSnapshotInvalid, fmt.Sprintf(format, args...))
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)

	var meta *snapshotMetaLine
	entries := map[string]JournalEntry{}
	var linkOrder []snapshotLinkLine
	holds := map[string]Hold{}
	byKey := map[string]JournalEntry{}
	transferKeys := map[string][]string{}
	holdKeys := map[string]string{}
	captureKeys := map[string]CaptureReceipt{}
	sweepKeys := map[string]sweepRecord{}
	merges := map[string]mergeRecord{}
	mergeKeys := map[string]string{}
	batchKeys := map[string]batchRecord{}
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
			var m snapshotMetaLine
			if err := json.Unmarshal(line, &m); err != nil {
				return fail("line %d: malformed meta: %v", lineNo, err)
			}
			if m.Format != snapshotFormatVersion {
				return fail("line %d: unsupported snapshot format %d (this build reads %d)",
					lineNo, m.Format, snapshotFormatVersion)
			}
			// An incremental snapshot carries base_version; a full
			// import must refuse it explicitly instead of silently
			// treating it as a complete journal.
			var probe struct {
				BaseVersion *uint64 `json:"base_version"`
			}
			if err := json.Unmarshal(line, &probe); err == nil && probe.BaseVersion != nil {
				return fail("line %d: incremental snapshot (base_version %d) cannot be fully imported; apply it with ImportIncrementalSnapshot",
					lineNo, *probe.BaseVersion)
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
			if want := uint64(len(linkOrder) + 1); ll.Seq != want {
				return fail("line %d: link seq %d out of order, want %d", lineNo, ll.Seq, want)
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
		case "merge":
			if meta == nil {
				return fail("line %d: merge before meta", lineNo)
			}
			var ml snapshotMergeLine
			if err := json.Unmarshal(line, &ml); err != nil {
				return fail("line %d: malformed merge: %v", lineNo, err)
			}
			if ml.Merge.MergeID == "" {
				return fail("line %d: merge with empty ID", lineNo)
			}
			if _, dup := merges[ml.Merge.MergeID]; dup {
				return fail("line %d: duplicate merge ID %q", lineNo, ml.Merge.MergeID)
			}
			merges[ml.Merge.MergeID] = mergeRecord{
				mergeID:   ml.Merge.MergeID,
				from:      ml.Merge.From,
				to:        ml.Merge.To,
				legs:      ml.Merge.Legs,
				entryIDs:  ml.Merge.EntryIDs,
				createdAt: ml.Merge.CreatedAt,
			}
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
			case snapshotNSMergeKey:
				if il.MergeID == "" {
					return fail("line %d: merge_key-namespace record without merge_id", lineNo)
				}
				if _, dup := mergeKeys[il.Key]; dup {
					return fail("line %d: duplicate idempotency key %q", lineNo, il.Key)
				}
				mergeKeys[il.Key] = il.MergeID
			case snapshotNSBatch:
				if il.Batch == nil {
					return fail("line %d: batch-namespace record without batch", lineNo)
				}
				if _, dup := batchKeys[il.Key]; dup {
					return fail("line %d: duplicate idempotency key %q", lineNo, il.Key)
				}
				batchKeys[il.Key] = batchRecord{
					batchID:   il.Batch.BatchID,
					entryIDs:  il.Batch.EntryIDs,
					createdAt: il.Batch.CreatedAt,
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
		return nil, fmt.Errorf("ledger: snapshot import: read: %w", err)
	}
	if meta == nil {
		return fail("empty snapshot: missing meta record")
	}

	// Structural invariants before any state is built.
	if len(entries) != meta.EntryCount {
		return fail("entry count %d does not match meta entry_count %d", len(entries), meta.EntryCount)
	}
	if len(linkOrder) != meta.ChainLinks {
		return fail("link count %d does not match meta chain_links %d", len(linkOrder), meta.ChainLinks)
	}
	if len(holds) != meta.HoldCount {
		return fail("hold count %d does not match meta hold_count %d", len(holds), meta.HoldCount)
	}
	// In a healthy ledger every Post bumps the version and appends exactly
	// one link, so version == links; a snapshot claiming otherwise was not
	// produced by a consistent export.
	if meta.Version != uint64(meta.ChainLinks) {
		return fail("meta version %d inconsistent with chain length %d", meta.Version, meta.ChainLinks)
	}
	if cfg == nil {
		return fail("missing config record")
	}

	l := New()

	// Fold the journal in chain order, rebuilding every derived index the
	// same way commitEntryLocked does. The chain is rebuilt from the
	// exported link hashes (not recomputed) so the final VerifyChain also
	// proves the exported hashes match the recomputed ones.
	for _, ll := range linkOrder {
		e, ok := entries[ll.EntryID]
		if !ok {
			return fail("link seq %d references missing entry %q", ll.Seq, ll.EntryID)
		}
		l.entries[e.ID] = e
		l.byAccount[e.DebitAccount] = append(l.byAccount[e.DebitAccount], e.ID)
		l.byAccount[e.CreditAccount] = append(l.byAccount[e.CreditAccount], e.ID)
		debitKey := accountCurrency{account: e.DebitAccount, currency: e.Currency}
		creditKey := accountCurrency{account: e.CreditAccount, currency: e.Currency}
		l.balances[debitKey] += e.AmountCents
		l.balances[creditKey] -= e.AmountCents
		l.debitTotals[debitKey] += e.AmountCents
		l.creditTotals[creditKey] += e.AmountCents

		prevRaw, err := hex.DecodeString(ll.PrevHash)
		if err != nil {
			return fail("link seq %d: bad prev_hash hex: %v", ll.Seq, err)
		}
		hashRaw, err := hex.DecodeString(ll.Hash)
		if err != nil {
			return fail("link seq %d: bad hash hex: %v", ll.Seq, err)
		}
		var prev, h [32]byte
		copy(prev[:], prevRaw)
		copy(h[:], hashRaw)
		l.chain = append(l.chain, chainLink{seq: ll.Seq, entryID: ll.EntryID, prevHash: prev, hash: h})
	}
	l.version = meta.Version

	// Holds, rebuilt in export order.
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
	for _, h := range holdList {
		l.holds[h.ID] = h
		l.holdsByAccount[h.Account] = append(l.holdsByAccount[h.Account], h.ID)
	}

	// Idempotency registries, with referential checks: a registry row that
	// points at a journal row that does not exist (or at a journal row it
	// no longer matches) is corruption, not a warning.
	for k, e := range byKey {
		j, ok := l.entries[e.ID]
		if !ok {
			return fail("idempotency key %q references missing journal entry %q", k, e.ID)
		}
		if !journalEntriesEqual(j, e) {
			return fail("idempotency key %q registry entry differs from journal entry %q", k, e.ID)
		}
		l.byKey[k] = e
	}
	for k, ids := range transferKeys {
		for _, id := range ids {
			if _, ok := l.entries[id]; !ok {
				return fail("transfer idempotency key %q references missing entry %q", k, id)
			}
		}
		l.transferKeys[k] = ids
	}
	for k, id := range holdKeys {
		if _, ok := l.holds[id]; !ok {
			return fail("hold idempotency key %q references missing hold %q", k, id)
		}
		l.holdKeys[k] = id
	}
	for k, rc := range captureKeys {
		if _, ok := l.holds[rc.HoldID]; !ok {
			return fail("capture idempotency key %q references missing hold %q", k, rc.HoldID)
		}
		if _, ok := l.entries[rc.Entry.ID]; !ok {
			return fail("capture idempotency key %q references missing entry %q", k, rc.Entry.ID)
		}
		l.captureKeys[k] = rc
	}
	for k, rec := range sweepKeys {
		for _, id := range rec.entryIDs {
			if _, ok := l.entries[id]; !ok {
				return fail("sweep idempotency key %q references missing entry %q", k, id)
			}
		}
		l.sweepKeys[k] = rec
	}
	for id, rec := range merges {
		for _, eid := range rec.entryIDs {
			if _, ok := l.entries[eid]; !ok {
				return fail("merge %q references missing entry %q", id, eid)
			}
		}
		l.merges[id] = rec
	}
	for k, mergeID := range mergeKeys {
		if _, ok := merges[mergeID]; !ok {
			return fail("merge idempotency key %q references missing merge %q", k, mergeID)
		}
		l.mergeKeys[k] = mergeID
	}
	for k, rec := range batchKeys {
		for _, id := range rec.entryIDs {
			if _, ok := l.entries[id]; !ok {
				return fail("batch idempotency key %q references missing entry %q", k, id)
			}
		}
		l.batchKeys[k] = rec
	}

	// Operational config.
	if err := l.applySnapshotConfigLocked(cfg); err != nil {
		return fail("%v", err)
	}

	// The whole point: the rebuilt ledger must prove its own integrity.
	// A rewritten amount, a deleted or reordered entry, or a spliced link
	// fails here and the import is rejected — no half-restored ledger is
	// ever returned.
	if err := l.verifyChainLocked(); err != nil {
		return nil, fmt.Errorf("%w: audit chain verification failed: %v", ErrSnapshotInvalid, err)
	}
	return l, nil
}

// applySnapshotConfigLocked installs a snapshot's operational config on
// the ledger: frozen and overdraft-protected accounts, the sub-account
// hierarchy, the transfer fee policy, the idempotency TTL, daily outflow
// limits, and the FX rate table. It validates every row before applying
// anything: invalid config is an error and leaves the ledger untouched —
// a restore must never silently drop a risk control or convert at a bad
// rate. Callers must hold the write lock. For a full restore the ledger
// starts empty, so installation is a plain overwrite; for an incremental
// import the same config section carries the latest config, applied the
// same way.
func (l *Ledger) applySnapshotConfigLocked(cfg *snapshotConfigLine) error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf(format, args...)
	}
	// Validate first: nothing is applied until every row checks out.
	type validatedLimit struct {
		key   dailyLimitKey
		limit int64
	}
	var limits []validatedLimit
	for _, dl := range cfg.DailyLimits {
		if dl.Account == "" {
			return bad("daily limit with empty account")
		}
		cur, err := normalizeCurrency(dl.Currency)
		if err != nil {
			return bad("bad daily limit currency %q: %v", dl.Currency, err)
		}
		if dl.LimitCents < 0 {
			return bad("negative daily limit for account %q (%s)", dl.Account, dl.Currency)
		}
		limits = append(limits, validatedLimit{dailyLimitKey{account: dl.Account, currency: cur}, dl.LimitCents})
	}
	var ttl time.Duration
	ttlSet := false
	if cfg.IdempotencyTTL != "" {
		var err error
		ttl, err = time.ParseDuration(cfg.IdempotencyTTL)
		if err != nil {
			return bad("bad idempotency_ttl %q: %v", cfg.IdempotencyTTL, err)
		}
		ttlSet = true
	}
	fx := make(map[fxPair]ExchangeRate, len(cfg.FXRates))
	for _, r := range cfg.FXRates {
		f, t, err := normalizeFXRate(r.FromCurrency, r.ToCurrency, r.Num, r.Den)
		if err != nil {
			return bad("bad FX rate %s->%s: %v", r.FromCurrency, r.ToCurrency, err)
		}
		fx[fxPair{from: f, to: t}] = ExchangeRate{
			FromCurrency:     f,
			ToCurrency:       t,
			Num:              r.Num,
			Den:              r.Den,
			EffectiveVersion: r.EffectiveVersion,
		}
	}

	// Apply: every row above checked out.
	for _, a := range cfg.Frozen {
		l.frozen[a] = true
	}
	for _, a := range cfg.OverdraftProtected {
		l.noOverdraft[a] = true
	}
	for c, p := range cfg.Parents {
		l.parents[c] = p
	}
	for _, vl := range limits {
		l.dailyLimits[vl.key] = vl.limit
	}
	// The transfer fee policy: prefer the tiered schedule when present;
	// otherwise rebuild the flat single-tier policy from the legacy
	// fee_rate_bps field (snapshots predating tiered fees). An empty
	// revenue account means the policy was disabled.
	if len(cfg.FeeTiers) > 0 {
		l.feeTiers = cfg.FeeTiers
	} else if cfg.FeeRevenueAccount != "" {
		l.feeTiers = []FeeTier{{MinAmountCents: 0, RateBps: cfg.FeeRateBps}}
	}
	l.feeRevenueAccount = cfg.FeeRevenueAccount
	if ttlSet {
		l.idempotencyTTL = ttl
	}
	l.fxRates = fx
	l.fxAccount = cfg.FXAccount
	return nil
}

// holdsEqual compares two holds field by field, ignoring time.Location
// representation (see journalEntriesEqual).
func holdsEqual(a, b Hold) bool {
	return a.ID == b.ID &&
		a.Account == b.Account &&
		a.AmountCents == b.AmountCents &&
		a.Currency == b.Currency &&
		a.ExpiresAt.UnixNano() == b.ExpiresAt.UnixNano() &&
		a.CreatedAt.UnixNano() == b.CreatedAt.UnixNano() &&
		a.IdempotencyKey == b.IdempotencyKey &&
		a.Status == b.Status
}

// mergeRecordsEqual compares two merge registry entries field by field,
// ignoring time.Location representation (see journalEntriesEqual).
func mergeRecordsEqual(a, b mergeRecord) bool {
	return a.mergeID == b.mergeID &&
		a.from == b.from &&
		a.to == b.to &&
		reflect.DeepEqual(a.legs, b.legs) &&
		reflect.DeepEqual(a.entryIDs, b.entryIDs) &&
		a.createdAt.UnixNano() == b.createdAt.UnixNano()
}

// snapshotLedgersEqual is a test helper: it reports whether two ledgers
// carry the same journaled and operational state, for snapshot
// round-trip tests. Time fields are compared by instant (not by
// time.Time's internal representation, which JSON round-trips drop the
// monotonic reading from), and derived maps (balances, totals,
// per-account indexes) are intentionally not compared — they are
// recomputed from the journal on import, so comparing them would only
// re-test the folding code against itself.
func snapshotLedgersEqual(a, b *Ledger) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	b.mu.RLock()
	defer b.mu.RUnlock()

	if a.version != b.version || len(a.chain) != len(b.chain) || len(a.entries) != len(b.entries) {
		return false
	}
	for i, la := range a.chain {
		if la != b.chain[i] {
			return false
		}
	}
	if len(a.byKey) != len(b.byKey) {
		return false
	}
	for id, ea := range a.entries {
		eb, ok := b.entries[id]
		if !ok || !journalEntriesEqual(ea, eb) {
			return false
		}
	}
	for k, ea := range a.byKey {
		eb, ok := b.byKey[k]
		if !ok || !journalEntriesEqual(ea, eb) {
			return false
		}
	}
	if len(a.holds) != len(b.holds) {
		return false
	}
	for id, ha := range a.holds {
		hb, ok := b.holds[id]
		if !ok || !holdsEqual(ha, hb) {
			return false
		}
	}
	if !reflect.DeepEqual(a.holdKeys, b.holdKeys) {
		return false
	}
	if len(a.captureKeys) != len(b.captureKeys) {
		return false
	}
	for k, ra := range a.captureKeys {
		rb, ok := b.captureKeys[k]
		if !ok || ra.CaptureID != rb.CaptureID || ra.HoldID != rb.HoldID ||
			ra.CapturedCents != rb.CapturedCents || ra.ReleasedCents != rb.ReleasedCents ||
			ra.Duplicate != rb.Duplicate || !journalEntriesEqual(ra.Entry, rb.Entry) {
			return false
		}
	}
	if !reflect.DeepEqual(a.transferKeys, b.transferKeys) {
		return false
	}
	if len(a.sweepKeys) != len(b.sweepKeys) {
		return false
	}
	for k, sa := range a.sweepKeys {
		sb, ok := b.sweepKeys[k]
		if !ok || sa.sweepID != sb.sweepID ||
			!reflect.DeepEqual(sa.entryIDs, sb.entryIDs) ||
			sa.createdAt.UnixNano() != sb.createdAt.UnixNano() {
			return false
		}
	}
	if len(a.merges) != len(b.merges) {
		return false
	}
	for id, ma := range a.merges {
		mb, ok := b.merges[id]
		if !ok || ma.mergeID != mb.mergeID || ma.from != mb.from || ma.to != mb.to ||
			!reflect.DeepEqual(ma.legs, mb.legs) ||
			!reflect.DeepEqual(ma.entryIDs, mb.entryIDs) ||
			ma.createdAt.UnixNano() != mb.createdAt.UnixNano() {
			return false
		}
	}
	if !reflect.DeepEqual(a.mergeKeys, b.mergeKeys) {
		return false
	}
	if !reflect.DeepEqual(a.frozen, b.frozen) {
		return false
	}
	if !reflect.DeepEqual(a.noOverdraft, b.noOverdraft) {
		return false
	}
	if !reflect.DeepEqual(a.parents, b.parents) {
		return false
	}
	if !reflect.DeepEqual(a.dailyLimits, b.dailyLimits) {
		return false
	}
	if !reflect.DeepEqual(a.feeTiers, b.feeTiers) || a.feeRevenueAccount != b.feeRevenueAccount {
		return false
	}
	if a.fxAccount != b.fxAccount {
		return false
	}
	if !reflect.DeepEqual(a.fxRates, b.fxRates) {
		return false
	}
	if a.idempotencyTTL != b.idempotencyTTL {
		return false
	}
	return true
}
