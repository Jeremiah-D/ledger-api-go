package ledger

import (
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

func chainEntry(id string, amount int64, at time.Time) JournalEntry {
	return JournalEntry{
		ID:             id,
		DebitAccount:   "cash",
		CreditAccount:  "equity",
		AmountCents:    amount,
		IdempotencyKey: "chain-" + id,
		CreatedAt:      at,
	}
}

func seedChain(t *testing.T, l *Ledger, n int) {
	t.Helper()
	base := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		e := chainEntry("e-chain-"+string(rune('0'+i)), 100, base.Add(time.Duration(i)*time.Hour))
		if _, dup, err := l.Post(e); err != nil || dup {
			t.Fatalf("Post(%s) = dup=%v err=%v", e.ID, dup, err)
		}
	}
}

func TestChainHeadEmptyLedger(t *testing.T) {
	l := New()
	head, links := l.ChainHead()
	if links != 0 {
		t.Fatalf("links = %d on empty ledger, want 0", links)
	}
	// An empty chain reports the genesis hash: 32 zero bytes.
	if want := strings.Repeat("0", 64); head != want {
		t.Fatalf("head = %q on empty ledger, want %q", head, want)
	}
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain on empty ledger = %v, want nil", err)
	}
}

func TestChainGrowsOneLinkPerPost(t *testing.T) {
	l := New()
	seedChain(t, l, 3)

	head, links := l.ChainHead()
	if links != 3 {
		t.Fatalf("links = %d, want 3", links)
	}
	if len(head) != 64 {
		t.Fatalf("head = %q, want 64 hex chars", head)
	}
	if _, err := hex.DecodeString(head); err != nil {
		t.Fatalf("head is not valid hex: %v", err)
	}

	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain = %v, want nil", err)
	}

	// Chain links are 1-based and contiguous with Post order.
	for i, want := range []string{"e-chain-0", "e-chain-1", "e-chain-2"} {
		if l.chain[i].entryID != want {
			t.Errorf("chain[%d].entryID = %q, want %q", i, l.chain[i].entryID, want)
		}
		if l.chain[i].seq != uint64(i+1) {
			t.Errorf("chain[%d].seq = %d, want %d", i, l.chain[i].seq, i+1)
		}
	}
	// Continuity: every link's PrevHash is the previous link's Hash.
	for i := 1; i < len(l.chain); i++ {
		if l.chain[i].prevHash != l.chain[i-1].hash {
			t.Fatalf("chain[%d].prevHash does not extend chain[%d].hash", i, i-1)
		}
	}
}

func TestChainHeadDeterministic(t *testing.T) {
	// Two ledgers journaling the same entries in the same order must
	// converge on the same head; different post order must not.
	a, b := New(), New()
	base := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	post := func(l *Ledger, id string, at time.Time) {
		t.Helper()
		if _, dup, err := l.Post(chainEntry(id, 100, at)); err != nil || dup {
			t.Fatalf("Post(%s) = dup=%v err=%v", id, dup, err)
		}
	}
	post(a, "e-1", base)
	post(a, "e-2", base.Add(time.Hour))
	post(b, "e-1", base)
	post(b, "e-2", base.Add(time.Hour))

	ha, la := a.ChainHead()
	hb, lb := b.ChainHead()
	if ha != hb || la != lb {
		t.Fatalf("same entries same order: heads differ (%q/%d vs %q/%d)", ha, la, hb, lb)
	}

	post(a, "e-3", base.Add(2*time.Hour))
	post(a, "e-4", base.Add(3*time.Hour))
	post(b, "e-4", base.Add(3*time.Hour))
	post(b, "e-3", base.Add(2*time.Hour))
	if ha, _ := a.ChainHead(); ha == hb {
		t.Fatalf("different post order converged on the same head %q", ha)
	}
}

func TestChainReplayAddsNoLink(t *testing.T) {
	l := New()
	e := chainEntry("e-replay", 100, time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC))
	if _, dup, err := l.Post(e); err != nil || dup {
		t.Fatalf("first Post = dup=%v err=%v", dup, err)
	}
	headBefore, linksBefore := l.ChainHead()

	retry := e
	retry.ID = "e-replay-retry"
	if _, dup, err := l.Post(retry); err != nil || !dup {
		t.Fatalf("replay Post = dup=%v err=%v, want duplicate", dup, err)
	}
	if head, links := l.ChainHead(); head != headBefore || links != linksBefore {
		t.Fatalf("replay moved the chain: head %q->%q links %d->%d", headBefore, head, linksBefore, links)
	}
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain after replay = %v, want nil", err)
	}
}

func TestVerifyChainDetectsRewrittenEntry(t *testing.T) {
	l := New()
	seedChain(t, l, 3)

	// An attacker rewrites a journaled amount in place (same package, so
	// the test can reach the private map exactly like a storage-layer
	// corruption would).
	corrupt := l.entries["e-chain-1"]
	corrupt.AmountCents = 999999
	l.entries["e-chain-1"] = corrupt

	err := l.VerifyChain()
	if err == nil {
		t.Fatalf("VerifyChain passed after an entry was rewritten")
	}
	if !strings.Contains(err.Error(), "e-chain-1") {
		t.Fatalf("VerifyChain error %q does not name the tampered entry", err)
	}
}

func TestVerifyChainDetectsSplicedLink(t *testing.T) {
	l := New()
	seedChain(t, l, 3)

	// Corrupt a link's PrevHash so the chain no longer extends the head.
	var zero [32]byte
	l.chain[2].prevHash = zero

	if err := l.VerifyChain(); err == nil {
		t.Fatalf("VerifyChain passed after a link was spliced")
	} else if !strings.Contains(err.Error(), "spliced") {
		t.Fatalf("VerifyChain error %q does not describe a splice", err)
	}
}

func TestVerifyChainDetectsRemovedLink(t *testing.T) {
	l := New()
	seedChain(t, l, 3)

	// Delete the middle link: seq continuity breaks at index 1.
	l.chain = append(l.chain[:1], l.chain[2:]...)

	if err := l.VerifyChain(); err == nil {
		t.Fatalf("VerifyChain passed after a link was removed")
	} else if !strings.Contains(err.Error(), "reordered") {
		t.Fatalf("VerifyChain error %q does not describe the break", err)
	}
}
