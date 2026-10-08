package ledger

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// buildRichLedger exercises every snapshotted subsystem: journaled posts
// (multi-currency, idempotency keys), transfers with fee legs, holds with
// a partial capture, sweeps, frozen accounts, overdraft protection, the
// sub-account hierarchy, and a non-default TTL.
func buildRichLedger(t *testing.T, now time.Time) *Ledger {
	t.Helper()
	l := New(
		WithIdempotencyTTL(time.Hour),
		WithOverdraftProtection("payer-od"),
		WithTransferFeePolicy(100, "fee-revenue"), // 1% fee
	)

	post := func(e JournalEntry) {
		t.Helper()
		if _, _, err := l.Post(e); err != nil {
			t.Fatalf("Post %s: %v", e.ID, err)
		}
	}
	post(JournalEntry{ID: "e1", DebitAccount: "alice", CreditAccount: "bank", AmountCents: 100000, Currency: "USD", IdempotencyKey: "k-e1"})
	post(JournalEntry{ID: "e2", DebitAccount: "bob", CreditAccount: "bank", AmountCents: 25000, Currency: "USD", IdempotencyKey: "k-e2"})
	post(JournalEntry{ID: "e3", DebitAccount: "alice", CreditAccount: "carol", AmountCents: 5000, Currency: "EUR"})

	if _, err := l.PostTransfer(Transfer{
		ID: "t1", From: "alice", To: "dave", AmountCents: 10000, Currency: "USD", IdempotencyKey: "k-t1",
	}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}

	l.Freeze("frozen-acct")
	if err := l.SetParent("child1", "parent1"); err != nil {
		t.Fatalf("SetParent: %v", err)
	}

	if _, _, err := l.Hold(Hold{
		ID: "h1", Account: "alice", AmountCents: 20000, Currency: "USD",
		ExpiresAt: now.Add(2 * time.Hour), IdempotencyKey: "k-h1",
	}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if _, err := l.Capture(Capture{
		ID: "c1", HoldID: "h1", To: "merchant", AmountCents: 12000, IdempotencyKey: "k-c1",
	}); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	// h2 stays active (no capture) so the snapshot carries live hold state.
	if _, _, err := l.Hold(Hold{
		ID: "h2", Account: "bob", AmountCents: 5000, Currency: "USD",
		ExpiresAt: now.Add(2 * time.Hour), IdempotencyKey: "k-h2",
	}); err != nil {
		t.Fatalf("Hold h2: %v", err)
	}

	post(JournalEntry{ID: "e4", DebitAccount: "s1", CreditAccount: "bank", AmountCents: 7000, Currency: "USD"})
	post(JournalEntry{ID: "e5", DebitAccount: "s2", CreditAccount: "bank", AmountCents: 3000, Currency: "EUR"})
	if _, err := l.PostSweep(Sweep{ID: "sw1", From: []AccountID{"s1", "s2"}, To: "treasury", IdempotencyKey: "k-sw1"}); err != nil {
		t.Fatalf("PostSweep: %v", err)
	}
	return l
}

// exportLines exports l and returns the snapshot as individual lines.
func exportLines(t *testing.T, l *Ledger) []string {
	t.Helper()
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) == 0 {
		t.Fatal("empty export")
	}
	var meta snapshotMetaLine
	if err := json.Unmarshal([]byte(lines[0]), &meta); err != nil || meta.Record != "meta" {
		t.Fatalf("first line is not a meta record: %v / %+v", err, meta)
	}
	return lines
}

func TestSnapshotRoundTripRichLedger(t *testing.T) {
	now := time.Date(2026, 10, 8, 23, 0, 0, 0, time.UTC)
	src := buildRichLedger(t, now)

	// Export determinism: identical state exports to identical bytes,
	// except the informational exported_at on the meta line.
	var b1, b2 bytes.Buffer
	if err := src.ExportSnapshot(&b1); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	if err := src.ExportSnapshot(&b2); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	l1 := strings.Split(strings.TrimRight(b1.String(), "\n"), "\n")
	l2 := strings.Split(strings.TrimRight(b2.String(), "\n"), "\n")
	if len(l1) != len(l2) {
		t.Fatalf("export line count changed: %d vs %d", len(l1), len(l2))
	}
	for i := 1; i < len(l1); i++ {
		if l1[i] != l2[i] {
			t.Fatalf("export not deterministic at line %d:\n%s\n%s", i, l1[i], l2[i])
		}
	}

	restored, err := ImportSnapshot(bytes.NewReader(b1.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if !snapshotLedgersEqual(src, restored) {
		t.Fatal("restored ledger state differs from source")
	}

	// Chain integrity on the restored copy.
	if err := restored.VerifyChain(); err != nil {
		t.Fatalf("restored VerifyChain: %v", err)
	}
	h1, n1 := src.ChainHead()
	h2, n2 := restored.ChainHead()
	if h1 != h2 || n1 != n2 {
		t.Fatalf("chain head differs: %s/%d vs %s/%d", h1, n1, h2, n2)
	}

	// Reconcile reruns cleanly against the snapshot source and matches
	// the original report exactly (the operator's pre-promotion check).
	r1 := src.Reconcile(now)
	r2 := restored.Reconcile(now)
	r1.GeneratedAt = time.Time{}
	r2.GeneratedAt = time.Time{}
	if !reflect.DeepEqual(r1, r2) {
		t.Fatalf("reconcile reports differ:\n%+v\n%+v", r1, r2)
	}
	if !r2.AccountingEquationOK || len(r2.Discrepancies) != 0 {
		t.Fatalf("restored reconcile not clean: %+v", r2)
	}
	if !r2.AuditChain.VerifyOK || !r2.AuditChain.HeadConsistent {
		t.Fatalf("restored audit chain not healthy: %+v", r2.AuditChain)
	}
	if len(r2.HeldTotals) == 0 {
		t.Fatal("restored reconcile lost the active hold totals")
	}
	if len(r2.FrozenAccounts) != 1 || r2.FrozenAccounts[0] != "frozen-acct" {
		t.Fatalf("frozen accounts not restored: %v", r2.FrozenAccounts)
	}
	if len(r2.OverdraftProtectedAccounts) != 1 {
		t.Fatalf("overdraft config not restored: %v", r2.OverdraftProtectedAccounts)
	}

	// Idempotent replays still work after the restore.
	got, ok := restored.GetByIdempotencyKey("k-e1")
	if !ok || got.ID != "e1" {
		t.Fatalf("replay index lost: %+v %v", got, ok)
	}
	replayed, dup, err := restored.Post(JournalEntry{
		ID: "e1-retry", DebitAccount: "alice", CreditAccount: "bank",
		AmountCents: 100000, Currency: "USD", IdempotencyKey: "k-e1",
	})
	if err != nil || !dup || replayed.ID != "e1" {
		t.Fatalf("post-restore replay broken: %+v %v %v", replayed, dup, err)
	}

	// Risk config survived: frozen rejects, overdraft rejects.
	if _, _, err := restored.Post(JournalEntry{ID: "fz", DebitAccount: "x", CreditAccount: "frozen-acct", AmountCents: 1}); err != ErrAccountFrozen {
		t.Fatalf("frozen account not enforced after restore: %v", err)
	}
	if _, _, err := restored.Post(JournalEntry{ID: "od", DebitAccount: "x", CreditAccount: "payer-od", AmountCents: 1}); err != ErrAccountOverdraft {
		t.Fatalf("overdraft protection not enforced after restore: %v", err)
	}

	// Balances and available funds match across the restore (checked
	// before the restored ledger accepts new posts below).
	for _, acct := range []AccountID{"alice", "bob", "treasury", "fee-revenue", "merchant"} {
		if a, b := src.Balance(acct), restored.Balance(acct); a != b {
			t.Fatalf("balance %s differs: %d vs %d", acct, a, b)
		}
	}
	if a, b := src.BalanceIn("alice", "EUR"), restored.BalanceIn("alice", "EUR"); a != b {
		t.Fatalf("EUR balance differs: %d vs %d", a, b)
	}
	if a, b := src.Available("alice"), restored.Available("alice"); a != b {
		t.Fatalf("available funds differ: %d vs %d", a, b)
	}

	// The restored ledger keeps working: new posts extend the chain.
	if _, _, err := restored.Post(JournalEntry{ID: "e-new", DebitAccount: "alice", CreditAccount: "bank", AmountCents: 10}); err != nil {
		t.Fatalf("post-restore Post: %v", err)
	}
	if _, v := restored.Snapshot("alice"); v != func() uint64 { _, sv := src.Snapshot("alice"); return sv }()+1 {
		t.Fatalf("version did not continue: %d", v)
	}
	if err := restored.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain after new post: %v", err)
	}
}

func TestSnapshotEmptyLedgerRoundTrip(t *testing.T) {
	src := New()
	restored, err := ImportSnapshot(bytes.NewReader(exportBytes(t, src)))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if !snapshotLedgersEqual(src, restored) {
		t.Fatal("empty ledgers differ")
	}
	if err := restored.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	r := restored.Reconcile(time.Now())
	if !r.AccountingEquationOK || !r.AuditChain.VerifyOK || r.AuditChain.Links != 0 {
		t.Fatalf("empty reconcile not clean: %+v", r)
	}
}

func exportBytes(t *testing.T, l *Ledger) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	return buf.Bytes()
}

// tamperLine rewrites one snapshot line (matched by predicate) with mutate
// applied to its decoded JSON map, and returns the rebuilt document.
func tamperLine(t *testing.T, raw []byte, match func(map[string]any) bool, mutate func(map[string]any)) []byte {
	t.Helper()
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	done := false
	for i, ln := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
		if match(m) && !done {
			mutate(m)
			enc, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			lines[i] = string(enc)
			done = true
		}
	}
	if !done {
		t.Fatal("no matching line to tamper")
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

func importMustFail(t *testing.T, raw []byte, wantSub string) {
	t.Helper()
	_, err := ImportSnapshot(bytes.NewReader(raw))
	if err == nil {
		t.Fatal("expected import to fail, it succeeded")
	}
	if !errors.Is(err, ErrSnapshotInvalid) {
		t.Fatalf("error does not wrap ErrSnapshotInvalid: %v", err)
	}
	if wantSub != "" && !strings.Contains(err.Error(), wantSub) {
		t.Fatalf("error %q does not mention %q", err, wantSub)
	}
}

func TestSnapshotImportRejectsTamperedAmount(t *testing.T) {
	now := time.Date(2026, 10, 8, 23, 0, 0, 0, time.UTC)
	raw := exportBytes(t, buildRichLedger(t, now))
	// Rewrite the amount consistently in both the journal and the
	// idempotency registry, so the tamper reaches the chain check: the
	// registry/journal cross-check passes, but the recomputed link hash
	// no longer matches the exported one.
	tampered := tamperLine(t, raw,
		func(m map[string]any) bool {
			return m["record"] == "entry" && m["entry"].(map[string]any)["id"] == "e1"
		},
		func(m map[string]any) {
			m["entry"].(map[string]any)["amount_cents"] = float64(999999)
		})
	tampered = tamperLine(t, tampered,
		func(m map[string]any) bool {
			return m["record"] == "idempotency" && m["namespace"] == "entry" && m["key"] == "k-e1"
		},
		func(m map[string]any) {
			m["entry"].(map[string]any)["amount_cents"] = float64(999999)
		})
	importMustFail(t, tampered, "audit chain verification failed")
}

func TestSnapshotImportRejectsTamperedLinkHash(t *testing.T) {
	now := time.Date(2026, 10, 8, 23, 0, 0, 0, time.UTC)
	raw := exportBytes(t, buildRichLedger(t, now))
	tampered := tamperLine(t, raw,
		func(m map[string]any) bool { return m["record"] == "link" && m["seq"] == float64(2) },
		func(m map[string]any) {
			h := m["hash"].(string)
			last := h[len(h)-1]
			if last == '0' {
				last = '1'
			} else {
				last = '0'
			}
			m["hash"] = h[:len(h)-1] + string(last)
		})
	importMustFail(t, tampered, "audit chain verification failed")
}

func TestSnapshotImportRejectsSplicedLink(t *testing.T) {
	now := time.Date(2026, 10, 8, 23, 0, 0, 0, time.UTC)
	raw := exportBytes(t, buildRichLedger(t, now))
	tampered := tamperLine(t, raw,
		func(m map[string]any) bool { return m["record"] == "link" && m["seq"] == float64(2) },
		func(m map[string]any) { m["prev_hash"] = strings.Repeat("0", 64) })
	importMustFail(t, tampered, "audit chain verification failed")
}

func TestSnapshotImportRejectsReorderedJournal(t *testing.T) {
	now := time.Date(2026, 10, 8, 23, 0, 0, 0, time.UTC)
	raw := exportBytes(t, buildRichLedger(t, now))
	tampered := tamperLine(t, raw,
		func(m map[string]any) bool { return m["record"] == "link" && m["seq"] == float64(3) },
		func(m map[string]any) { m["entry_id"] = "e1" }) // link 3 now claims e1's entry
	importMustFail(t, tampered, "")
}

func TestSnapshotImportRejectsStructuralCorruption(t *testing.T) {
	now := time.Date(2026, 10, 8, 23, 0, 0, 0, time.UTC)
	good := exportBytes(t, buildRichLedger(t, now))

	importMustFail(t, []byte("this is not json\n"), "")
	importMustFail(t, []byte(""), "")

	// meta not first: an entry leading the document is rejected, and so is
	// a meta record displaced by a leading blank line.
	lines := strings.Split(strings.TrimRight(string(good), "\n"), "\n")
	swapped := append([]string{lines[1], lines[0]}, lines[2:]...)
	importMustFail(t, []byte(strings.Join(swapped, "\n")+"\n"), "before meta")
	importMustFail(t, []byte("\n"+string(good)), "must be the first line")

	// version / chain length inconsistency
	verTampered := tamperLine(t, good,
		func(m map[string]any) bool { return m["record"] == "meta" },
		func(m map[string]any) { m["version"] = float64(999) })
	importMustFail(t, verTampered, "inconsistent with chain length")

	// dangling registry reference
	dangling := tamperLine(t, good,
		func(m map[string]any) bool { return m["record"] == "idempotency" && m["namespace"] == "transfer" },
		func(m map[string]any) { m["entry_ids"] = []any{"no-such-entry"} })
	importMustFail(t, dangling, "missing entry")

	// unknown record type
	withBogus := append([]string{}, lines...)
	withBogus = append(withBogus, `{"record":"mystery"}`)
	importMustFail(t, []byte(strings.Join(withBogus, "\n")+"\n"), "unknown record type")
}

func TestSnapshotSurvivesTTLPrunedKeys(t *testing.T) {
	// Keys pruned by the TTL before export simply stay absent: the import
	// must not resurrect them, and replays of pruned keys book anew.
	l := New(WithIdempotencyTTL(time.Nanosecond))
	if _, _, err := l.Post(JournalEntry{ID: "e1", DebitAccount: "a", CreditAccount: "b", AmountCents: 100, IdempotencyKey: "k1"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Nanosecond)
	if n := l.ExpireIdempotencyKeys(); n != 1 {
		t.Fatalf("expected 1 pruned key, got %d", n)
	}
	restored, err := ImportSnapshot(bytes.NewReader(exportBytes(t, l)))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if _, ok := restored.GetByIdempotencyKey("k1"); ok {
		t.Fatal("pruned key resurrected by import")
	}
	if _, dup, err := restored.Post(JournalEntry{ID: "e2", DebitAccount: "a", CreditAccount: "b", AmountCents: 100, IdempotencyKey: "k1"}); err != nil || dup {
		t.Fatalf("replay of pruned key should book anew: %v %v", dup, err)
	}
}
