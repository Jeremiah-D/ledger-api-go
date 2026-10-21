package ledger

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func batchTransferFixture(t *testing.T, id, from, to string, cents int64) Transfer {
	t.Helper()
	return Transfer{
		ID: id, From: AccountID(from), To: AccountID(to),
		AmountCents: cents, Currency: "USD",
		CreatedAt:   time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC),
	}
}

// Happy path: three transfers, one with an explicit fee. Every
// transfer's legs land, the receipt discloses per-transfer entries,
// and the batch is atomic by construction.
func TestPostBatchTransferHappyPath(t *testing.T) {
	l := New()
	batch := TransferBatch{
		ID: "payroll-oct",
		Transfers: []Transfer{
			batchTransferFixture(t, "t1", "payroll", "alice", 500000),
			batchTransferFixture(t, "t2", "payroll", "bob", 450000),
			func() Transfer {
				tr := batchTransferFixture(t, "t3", "payroll", "carol", 100000)
				tr.FeeCents = 200
				tr.FeeAccount = "fees"
				return tr
			}(),
		},
		IdempotencyKey: "bt-1",
	}
	receipt, err := l.PostBatchTransfer(batch)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.BatchID != "payroll-oct" || receipt.Duplicate {
		t.Fatalf("receipt = %+v, want batch payroll-oct non-duplicate", receipt)
	}
	if len(receipt.Receipts) != 3 {
		t.Fatalf("receipts = %d, want 3", len(receipt.Receipts))
	}
	if receipt.Receipts[2].FeeCents != 200 {
		t.Errorf("t3 fee = %d, want 200", receipt.Receipts[2].FeeCents)
	}
	// Per-transfer leg detail: principal + fee leg on t3.
	if len(receipt.Receipts[2].Entries) != 2 {
		t.Errorf("t3 entries = %d, want 2 (principal + fee)", len(receipt.Receipts[2].Entries))
	}
	// Balances: payroll paid 500000+450000+100000+200 fee.
	if got := l.BalanceIn("payroll", "USD"); got != -1050200 {
		t.Errorf("payroll balance = %d, want -1050200", got)
	}
	if got := l.BalanceIn("alice", "USD"); got != 500000 {
		t.Errorf("alice balance = %d, want 500000", got)
	}
	if got := l.BalanceIn("fees", "USD"); got != 200 {
		t.Errorf("fees balance = %d, want 200", got)
	}
}

// Atomicity: the second transfer overdrafts a protected payer. The
// whole batch rejects and the first transfer's rows never land — no
// journal rows, no version bump, no idempotency registrations.
func TestPostBatchTransferAtomic(t *testing.T) {
	l := New()
	l.EnableOverdraftProtection("payroll")
	versionBefore := l.Version()
	batch := TransferBatch{
		ID: "atomic-1",
		Transfers: []Transfer{
			batchTransferFixture(t, "a1", "payroll", "alice", 100000),
			batchTransferFixture(t, "a2", "payroll", "bob", 900000),
		},
		IdempotencyKey: "bt-atomic",
	}
	_, err := l.PostBatchTransfer(batch)
	if !errors.Is(err, ErrAccountOverdraft) {
		t.Fatalf("want ErrAccountOverdraft, got %v", err)
	}
	if got := l.BalanceIn("alice", "USD"); got != 0 {
		t.Errorf("alice balance = %d after rejected batch, want 0", got)
	}
	if l.Version() != versionBefore {
		t.Errorf("version moved on rejected batch: %d -> %d", versionBefore, l.Version())
	}
	if _, ok := l.entries["a1"]; ok {
		t.Error("a1 journaled despite batch rejection")
	}
	if _, ok := l.batchTransferKeys["bt-atomic"]; ok {
		t.Error("batch key registered despite batch rejection")
	}
	if _, ok := l.byKey["bt-atomic"]; ok {
		t.Error("byKey polluted by rejected batch")
	}
}

// A rejected batch must not shadow a later valid batch with the same
// transfer IDs: the IDs were never journaled, so reposting succeeds.
func TestPostBatchTransferRetryAfterRejection(t *testing.T) {
	l := New()
	l.EnableOverdraftProtection("payroll")
	// Seed payroll so the retry succeeds.
	if _, _, err := l.Post(JournalEntry{ID: "seed", DebitAccount: "payroll", CreditAccount: "bank", AmountCents: 200000, Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	bad := TransferBatch{ID: "retry-1", Transfers: []Transfer{
		batchTransferFixture(t, "r1", "payroll", "alice", 100000),
		batchTransferFixture(t, "r2", "payroll", "bob", 900000),
	}}
	if _, err := l.PostBatchTransfer(bad); !errors.Is(err, ErrAccountOverdraft) {
		t.Fatalf("first attempt: want ErrAccountOverdraft, got %v", err)
	}
	good := TransferBatch{ID: "retry-2", Transfers: []Transfer{
		batchTransferFixture(t, "r1", "payroll", "alice", 100000),
		batchTransferFixture(t, "r2", "payroll", "bob", 50000),
	}}
	receipt, err := l.PostBatchTransfer(good)
	if err != nil {
		t.Fatalf("retry after rejection: %v", err)
	}
	if len(receipt.Receipts) != 2 {
		t.Fatalf("receipts = %d, want 2", len(receipt.Receipts))
	}
}

// Batch-level idempotency: reposting the batch key returns the whole
// original receipt with Duplicate == true and books nothing new.
func TestPostBatchTransferIdempotency(t *testing.T) {
	l := New()
	batch := TransferBatch{
		ID: "idem-1",
		Transfers: []Transfer{
			batchTransferFixture(t, "i1", "payroll", "alice", 100000),
			batchTransferFixture(t, "i2", "payroll", "bob", 200000),
		},
		IdempotencyKey: "bt-idem",
	}
	first, err := l.PostBatchTransfer(batch)
	if err != nil {
		t.Fatal(err)
	}
	versionAfterFirst := l.Version()
	second, err := l.PostBatchTransfer(TransferBatch{
		ID: "ignored", IdempotencyKey: "bt-idem",
		Transfers: []Transfer{batchTransferFixture(t, "i9", "x", "y", 1)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Duplicate || second.BatchID != "idem-1" {
		t.Fatalf("replay = %+v, want duplicate of idem-1", second)
	}
	if len(second.Receipts) != 2 {
		t.Fatalf("replay receipts = %d, want 2", len(second.Receipts))
	}
	for i, r := range second.Receipts {
		if r.TransferID != first.Receipts[i].TransferID {
			t.Errorf("replay receipt %d transfer = %s, want %s", i, r.TransferID, first.Receipts[i].TransferID)
		}
		if !r.Duplicate {
			t.Errorf("replay receipt %d not marked duplicate", i)
		}
	}
	if l.Version() != versionAfterFirst {
		t.Errorf("version moved on replay: %d -> %d", versionAfterFirst, l.Version())
	}
	if _, ok := l.entries["i9"]; ok {
		t.Error("replay batch's transfer journaled")
	}
}

// Per-transfer replay inside a batch: a transfer whose key was already
// posted resolves to the original receipt; the other transfers commit
// fresh.
func TestPostBatchTransferPerTransferReplay(t *testing.T) {
	l := New()
	orig, err := l.PostTransfer(batchTransferFixture(t, "orig-1", "payroll", "alice", 100000))
	if err != nil {
		t.Fatal(err)
	}
	_ = orig
	replay := batchTransferFixture(t, "new-id", "payroll", "alice", 100000)
	replay.IdempotencyKey = ""
	// Give the second transfer the SAME idempotency key as... no:
	// replay the original transfer's key on a differently-ID'd transfer.
	posted, err := l.PostTransfer(Transfer{ID: "keyed-1", From: "payroll", To: "bob", AmountCents: 50000, Currency: "USD", IdempotencyKey: "k-1", CreatedAt: time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	_ = posted

	batch := TransferBatch{
		ID: "mixed-1",
		Transfers: []Transfer{
			batchTransferFixture(t, "fresh-1", "payroll", "carol", 25000),
			{ID: "replay-as", From: "zz", To: "zz2", AmountCents: 1, Currency: "USD", IdempotencyKey: "k-1", CreatedAt: time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)},
		},
	}
	receipt, err := l.PostBatchTransfer(batch)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipt.Receipts) != 2 {
		t.Fatalf("receipts = %d, want 2", len(receipt.Receipts))
	}
	if receipt.Receipts[0].Duplicate {
		t.Error("fresh transfer marked duplicate")
	}
	if !receipt.Receipts[1].Duplicate {
		t.Error("replayed transfer not marked duplicate")
	}
	if receipt.Receipts[1].TransferID != "replay-as" {
		t.Errorf("replay TransferID = %s, want replay-as", receipt.Receipts[1].TransferID)
	}
	if len(receipt.Receipts[1].Entries) == 0 || receipt.Receipts[1].Entries[0].ID != "keyed-1" {
		t.Errorf("replay must resolve to the originally posted entry keyed-1, got %+v", receipt.Receipts[1].Entries)
	}
}

// Duplicate transfer IDs inside one batch are ambiguous: rejected
// before anything is recorded.
func TestPostBatchTransferDuplicateID(t *testing.T) {
	l := New()
	batch := TransferBatch{
		ID: "dup-id",
		Transfers: []Transfer{
			batchTransferFixture(t, "same", "payroll", "alice", 100),
			batchTransferFixture(t, "same", "payroll", "bob", 200),
		},
	}
	if _, err := l.PostBatchTransfer(batch); !errors.Is(err, ErrBatchTransferDuplicateID) {
		t.Fatalf("want ErrBatchTransferDuplicateID, got %v", err)
	}
}

// Two new transfers sharing a fresh idempotency key are ambiguous:
// rejected. A key that replays an already-posted transfer is fine.
func TestPostBatchTransferDuplicateKey(t *testing.T) {
	l := New()
	mk := func(id, key string) Transfer {
		tr := batchTransferFixture(t, id, "payroll", "alice", 100)
		tr.IdempotencyKey = key
		return tr
	}
	batch := TransferBatch{
		ID: "dup-key",
		Transfers: []Transfer{mk("d1", "fresh-key"), mk("d2", "fresh-key")},
	}
	if _, err := l.PostBatchTransfer(batch); !errors.Is(err, ErrBatchTransferDuplicateIdempotencyKey) {
		t.Fatalf("want ErrBatchTransferDuplicateIdempotencyKey, got %v", err)
	}
	// But two transfers replaying the same already-posted key resolve
	// to the same original — not a conflict.
	if _, err := l.PostTransfer(Transfer{ID: "posted-1", From: "payroll", To: "alice", AmountCents: 100, Currency: "USD", IdempotencyKey: "posted-key"}); err != nil {
		t.Fatal(err)
	}
	batch2 := TransferBatch{
		ID: "replay-key",
		Transfers: []Transfer{mk("r1", "posted-key"), mk("r2", "posted-key")},
	}
	receipt, err := l.PostBatchTransfer(batch2)
	if err != nil {
		t.Fatalf("replaying an already-posted key twice: %v", err)
	}
	if !receipt.Receipts[0].Duplicate || !receipt.Receipts[1].Duplicate {
		t.Error("both transfers must replay the posted original")
	}
}

// Frozen payer: the whole batch rejects, nothing books.
func TestPostBatchTransferFrozen(t *testing.T) {
	l := New()
	l.Freeze("payroll")
	batch := TransferBatch{
		ID: "frozen-1",
		Transfers: []Transfer{
			batchTransferFixture(t, "f1", "payroll", "alice", 100),
			batchTransferFixture(t, "f2", "alice", "bob", 100),
		},
	}
	if _, err := l.PostBatchTransfer(batch); !errors.Is(err, ErrAccountFrozen) {
		t.Fatalf("want ErrAccountFrozen, got %v", err)
	}
	if _, ok := l.entries["f1"]; ok {
		t.Error("f1 journaled despite frozen payer")
	}
}

// Field validation runs before anything books: one bad transfer
// rejects the whole batch.
func TestPostBatchTransferValidation(t *testing.T) {
	l := New()
	bad := batchTransferFixture(t, "v1", "payroll", "alice", 100)
	bad.AmountCents = -5
	batch := TransferBatch{
		ID: "valid-1",
		Transfers: []Transfer{
			batchTransferFixture(t, "v0", "payroll", "alice", 100),
			bad,
		},
	}
	if _, err := l.PostBatchTransfer(batch); !errors.Is(err, ErrTransferNonPositiveAmount) {
		t.Fatalf("want ErrTransferNonPositiveAmount, got %v", err)
	}
	if _, ok := l.entries["v0"]; ok {
		t.Error("v0 journaled despite sibling validation failure")
	}
	if _, err := l.PostBatchTransfer(TransferBatch{ID: "x"}); !errors.Is(err, ErrEmptyTransferBatch) {
		t.Fatalf("empty batch: want ErrEmptyTransferBatch, got %v", err)
	}
	if _, err := l.PostBatchTransfer(TransferBatch{Transfers: []Transfer{batchTransferFixture(t, "x", "a", "b", 1)}}); !errors.Is(err, ErrEmptyTransferBatchID) {
		t.Fatalf("empty batch ID: want ErrEmptyTransferBatchID, got %v", err)
	}
}

// Dual-control review: a transfer above the payer's threshold journals
// with the pending-review marker, atomically with the settled ones.
func TestPostBatchTransferReview(t *testing.T) {
	l := New(WithReviewThreshold(100000))
	batch := TransferBatch{
		ID: "review-1",
		Transfers: []Transfer{
			batchTransferFixture(t, "rv1", "payroll", "alice", 50000),
			batchTransferFixture(t, "rv2", "payroll", "bob", 150000),
		},
	}
	receipt, err := l.PostBatchTransfer(batch)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Receipts[0].ReviewStatus == ReviewStatusPending {
		t.Error("rv1 below threshold must not be pending review")
	}
	if receipt.Receipts[1].ReviewStatus != ReviewStatusPending {
		t.Errorf("rv2 review status = %q, want pending_review", receipt.Receipts[1].ReviewStatus)
	}
	// The pending leg never touched balances; the settled one did.
	if got := l.BalanceIn("alice", "USD"); got != 50000 {
		t.Errorf("alice = %d, want 50000", got)
	}
	if got := l.BalanceIn("bob", "USD"); got != 0 {
		t.Errorf("bob = %d, want 0 (pending review)", got)
	}
}

// Batch keys survive a disaster-recovery snapshot round-trip, and the
// replay still resolves after restore.
func TestPostBatchTransferSnapshotRoundTrip(t *testing.T) {
	l := New()
	batch := TransferBatch{
		ID: "snap-1",
		Transfers: []Transfer{batchTransferFixture(t, "s1", "payroll", "alice", 100000)},
		IdempotencyKey: "bt-snap",
	}
	if _, err := l.PostBatchTransfer(batch); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	if err := l.ExportSnapshot(&sb); err != nil {
		t.Fatal(err)
	}
	l2, err := ImportSnapshot(strings.NewReader(sb.String()))
	if err != nil {
		t.Fatal(err)
	}
	if !snapshotLedgersEqual(l, l2) {
		t.Fatal("ledgers differ after snapshot round-trip")
	}
	receipt, err := l2.PostBatchTransfer(TransferBatch{
		ID: "other", IdempotencyKey: "bt-snap",
		Transfers: []Transfer{batchTransferFixture(t, "ignored", "x", "y", 1)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.Duplicate || receipt.BatchID != "snap-1" {
		t.Fatalf("replay after restore = %+v, want duplicate of snap-1", receipt)
	}
}

// Concurrent batches under -race: no data race, both batches commit.
func TestPostBatchTransferConcurrent(t *testing.T) {
	l := New()
	if _, _, err := l.Post(JournalEntry{ID: "fund", DebitAccount: "payroll", CreditAccount: "bank", AmountCents: 10000000, Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := string(rune('a'+i)) + "1"
			batch := TransferBatch{
				ID: "conc-" + id,
				Transfers: []Transfer{
					batchTransferFixture(t, id+"-t1", "payroll", "alice", 1000),
					batchTransferFixture(t, id+"-t2", "payroll", "bob", 1000),
				},
			}
			_, errs[i] = l.PostBatchTransfer(batch)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("batch %d: %v", i, err)
		}
	}
	if got := l.BalanceIn("alice", "USD"); got != 8000 {
		t.Errorf("alice = %d, want 8000", got)
	}
}

// Denylist screening rejects the whole batch before the idempotency
// check: the attempt leaves no key trace.
func TestPostBatchTransferDenylist(t *testing.T) {
	l := New()
	l.AddDeniedAccount("evil")
	batch := TransferBatch{
		ID: "deny-1",
		Transfers: []Transfer{
			batchTransferFixture(t, "n1", "payroll", "alice", 100),
			batchTransferFixture(t, "n2", "payroll", "evil", 100),
		},
		IdempotencyKey: "bt-deny",
	}
	if _, err := l.PostBatchTransfer(batch); !errors.Is(err, ErrAccountDenied) {
		t.Fatalf("want ErrAccountDenied, got %v", err)
	}
	if _, ok := l.batchTransferKeys["bt-deny"]; ok {
		t.Error("batch key registered despite denylist rejection")
	}
}
