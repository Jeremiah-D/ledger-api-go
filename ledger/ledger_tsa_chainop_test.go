package ledger

import (
	"context"
	"testing"
	"time"
)

// LG-100: every *new* TSA anchoring is recorded in the hash-chained audit
// log as a chain_anchored op carrying {chain_seq, head_hash, tsa_token},
// so deleting tsa_anchors.jsonl cannot erase all evidence that an anchor
// happened.

func pollChainAnchoredOps(t *testing.T, dir string, want int, what string) []AuditEvent {
	t.Helper()
	day := time.Now().UTC().Format("2006-01-02")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		events, _, err := ReadAuditLog(dir, day)
		if err != nil {
			t.Fatalf("ReadAuditLog: %v", err)
		}
		var ops []AuditEvent
		for _, e := range events {
			if e.Op == "chain_anchored" {
				ops = append(ops, e)
			}
		}
		if len(ops) >= want {
			return ops
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d chain_anchored ops (%s)", want, what)
	return nil
}

func TestChainAnchoredOpEmitted(t *testing.T) {
	ts := newTestTSA(t)
	l, dir := newTSATestLedger(t, ts)
	postAnchorEntries(t, l, "tsachainop", 2)

	a, err := l.AnchorTimestamp(context.Background(), &RFC3161Client{URL: ts.srv.URL})
	if err != nil {
		t.Fatalf("AnchorTimestamp: %v", err)
	}

	ops := pollChainAnchoredOps(t, dir, 1, "after AnchorTimestamp")
	op := ops[0]
	d := op.Details
	if d == nil {
		t.Fatal("chain_anchored op has no details")
	}
	if got := uint64(d["chain_seq"].(float64)); got != a.Seq {
		t.Fatalf("chain_seq = %d, want anchor seq %d", got, a.Seq)
	}
	if d["head_hash"] != a.HeadHash {
		t.Fatalf("head_hash = %v, want %s", d["head_hash"], a.HeadHash)
	}
	if d["tsa_token"] != a.Token {
		t.Fatal("tsa_token does not match the recorded anchor token")
	}
	if d["tsa_url"] != ts.srv.URL {
		t.Fatalf("tsa_url = %v, want %s", d["tsa_url"], ts.srv.URL)
	}
	if d["serial"] != a.Serial {
		t.Fatalf("serial = %v, want %s", d["serial"], a.Serial)
	}
	if _, ok := d["gen_time"].(string); !ok {
		t.Fatalf("gen_time missing or not a string: %v", d["gen_time"])
	}
	// Anchoring changes no journal state: the version bracket is flat.
	if op.VersionBefore != op.VersionAfter {
		t.Fatalf("version bracket not flat: %d -> %d", op.VersionBefore, op.VersionAfter)
	}
	if op.Actor != "TSAAnchorer" {
		t.Fatalf("actor = %q, want TSAAnchorer", op.Actor)
	}
}

func TestChainAnchoredOpIdempotent(t *testing.T) {
	ts := newTestTSA(t)
	l, dir := newTSATestLedger(t, ts)
	postAnchorEntries(t, l, "tsachainopidem", 1)

	if _, err := l.AnchorTimestamp(context.Background(), &RFC3161Client{URL: ts.srv.URL}); err != nil {
		t.Fatalf("first AnchorTimestamp: %v", err)
	}
	pollChainAnchoredOps(t, dir, 1, "after first anchor")

	// Re-anchoring an unchanged head is a no-op: no second op.
	if _, err := l.AnchorTimestamp(context.Background(), &RFC3161Client{URL: ts.srv.URL}); err != nil {
		t.Fatalf("second AnchorTimestamp: %v", err)
	}
	// Give the async audit flush a chance to (incorrectly) emit, then count.
	time.Sleep(300 * time.Millisecond)
	day := time.Now().UTC().Format("2006-01-02")
	events, _, err := ReadAuditLog(dir, day)
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	n := 0
	for _, e := range events {
		if e.Op == "chain_anchored" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("idempotent re-anchor emitted %d chain_anchored ops, want 1", n)
	}
}

func TestChainAnchoredOpAuditChainIntact(t *testing.T) {
	ts := newTestTSA(t)
	l, dir := newTSATestLedger(t, ts)
	postAnchorEntries(t, l, "tsachainopverify", 1)

	if _, err := l.AnchorTimestamp(context.Background(), &RFC3161Client{URL: ts.srv.URL}); err != nil {
		t.Fatalf("AnchorTimestamp: %v", err)
	}
	pollChainAnchoredOps(t, dir, 1, "before audit verify")

	// The new op is sealed into the LG-32 hash chain like any other entry.
	rep, err := VerifyAuditLog(dir)
	if err != nil {
		t.Fatalf("VerifyAuditLog: %v", err)
	}
	if !rep.Ok {
		t.Fatalf("audit chain broken after chain_anchored: %+v", rep.FirstBreak)
	}
	if rep.CheckedEntries == 0 {
		t.Fatal("VerifyAuditLog checked zero entries")
	}
}
