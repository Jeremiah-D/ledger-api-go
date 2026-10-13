package ledger

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
	"testing"
)

func TestPostTransferSplitNoChunkingDelegates(t *testing.T) {
	l := New()
	seedBalance(t, l, "alice", 10000)

	// No cap configured: the transfer posts exactly as PostTransfer
	// would — the bare transfer ID, not a chunk namespace.
	receipt, err := l.PostTransferSplit(Transfer{
		ID: "t-delegate", From: "alice", To: "bob", AmountCents: 5000,
	})
	if err != nil {
		t.Fatalf("PostTransferSplit: %v", err)
	}
	if len(receipt.Chunks) != 1 {
		t.Fatalf("chunks = %d, want 1", len(receipt.Chunks))
	}
	c := receipt.Chunks[0]
	if c.ChunkIndex != 0 || c.ChunkCount != 1 || c.AmountCents != 5000 {
		t.Errorf("chunk = %+v, want index 0 / count 1 / amount 5000", c)
	}
	if len(c.EntryIDs) != 1 || c.EntryIDs[0] != "t-delegate" {
		t.Errorf("chunk entry IDs = %v, want [t-delegate]", c.EntryIDs)
	}
	if l.Balance("bob") != 5000 || l.Balance("alice") != 5000 {
		t.Errorf("balances: bob=%d alice=%d, want 5000/5000", l.Balance("bob"), l.Balance("alice"))
	}
}

func TestPostTransferSplitBasicChunking(t *testing.T) {
	l := New(WithMaxTransferChunkCents(10000))
	seedBalance(t, l, "alice", 100000)

	receipt, err := l.PostTransferSplit(Transfer{
		ID: "t-split", From: "alice", To: "bob", AmountCents: 25000,
		Memo: "order-42", ExternalRef: "ext-1",
	})
	if err != nil {
		t.Fatalf("PostTransferSplit: %v", err)
	}
	if len(receipt.Chunks) != 3 {
		t.Fatalf("chunks = %d, want 3", len(receipt.Chunks))
	}
	wantAmounts := []int64{10000, 10000, 5000}
	for i, c := range receipt.Chunks {
		if c.ChunkIndex != i || c.ChunkCount != 3 {
			t.Errorf("chunk %d: index/count = %d/%d, want %d/3", i, c.ChunkIndex, c.ChunkCount, i)
		}
		if c.AmountCents != wantAmounts[i] {
			t.Errorf("chunk %d amount = %d, want %d", i, c.AmountCents, wantAmounts[i])
		}
		wantID := "t-split/chunk-" + itoa(i)
		if len(c.EntryIDs) != 1 || c.EntryIDs[0] != wantID {
			t.Errorf("chunk %d entry IDs = %v, want [%s]", i, c.EntryIDs, wantID)
		}
	}
	// Memo and external reference ride chunk 0 only.
	e0 := findEntry(l, "t-split/chunk-0")
	if e0.Memo != "order-42" || e0.ExternalRef != "ext-1" {
		t.Errorf("chunk-0 memo/ref = %q/%q, want order-42/ext-1", e0.Memo, e0.ExternalRef)
	}
	if e1 := findEntry(l, "t-split/chunk-1"); e1.Memo != "" || e1.ExternalRef != "" {
		t.Errorf("chunk-1 memo/ref = %q/%q, want empty", e1.Memo, e1.ExternalRef)
	}
	if l.Balance("bob") != 25000 || l.Balance("alice") != 75000 {
		t.Errorf("balances: bob=%d alice=%d, want 25000/75000", l.Balance("bob"), l.Balance("alice"))
	}
	if err := l.VerifyChain(); err != nil {
		t.Errorf("VerifyChain: %v", err)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Errorf("VerifyAccountingEquation: %v", err)
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

func findEntry(l *Ledger, id string) JournalEntry {
	for _, e := range l.Entries() {
		if e.ID == id {
			return e
		}
	}
	return JournalEntry{}
}

func TestPostTransferSplitIdempotentReplay(t *testing.T) {
	l := New(WithMaxTransferChunkCents(10000))
	seedBalance(t, l, "alice", 100000)

	first, err := l.PostTransferSplit(Transfer{
		ID: "t-replay", From: "alice", To: "bob", AmountCents: 25000,
		IdempotencyKey: "key-1",
	})
	if err != nil {
		t.Fatalf("first PostTransferSplit: %v", err)
	}
	_, versionAfterFirst := l.Snapshot("alice")
	entriesAfterFirst := len(l.Entries())

	second, err := l.PostTransferSplit(Transfer{
		ID: "t-replay", From: "alice", To: "bob", AmountCents: 25000,
		IdempotencyKey: "key-1",
	})
	if err != nil {
		t.Fatalf("replay PostTransferSplit: %v", err)
	}
	if !second.Duplicate {
		t.Error("replay Duplicate = false, want true")
	}
	if len(second.Chunks) != 3 {
		t.Fatalf("replay chunks = %d, want 3", len(second.Chunks))
	}
	for i := range first.Chunks {
		if second.Chunks[i].AmountCents != first.Chunks[i].AmountCents ||
			len(second.Chunks[i].EntryIDs) != len(first.Chunks[i].EntryIDs) ||
			second.Chunks[i].EntryIDs[0] != first.Chunks[i].EntryIDs[0] {
			t.Errorf("replay chunk %d = %+v, want %+v", i, second.Chunks[i], first.Chunks[i])
		}
	}
	// The replay books nothing new.
	if _, v := l.Snapshot("alice"); v != versionAfterFirst {
		t.Errorf("version moved on replay: %d -> %d", versionAfterFirst, v)
	}
	if n := len(l.Entries()); n != entriesAfterFirst {
		t.Errorf("entries moved on replay: %d -> %d", entriesAfterFirst, n)
	}
	// Chunk entries carry per-(transfer, chunk) idempotency keys.
	e0 := findEntry(l, "t-replay/chunk-0")
	if e0.IdempotencyKey != "key-1/chunk-0" {
		t.Errorf("chunk-0 idempotency key = %q, want key-1/chunk-0", e0.IdempotencyKey)
	}
}

func TestPostTransferSplitFeeOncePerTransfer(t *testing.T) {
	l := New(
		WithMaxTransferChunkCents(10000),
		WithTransferFeePolicy(250, "fee-rev"),
	)
	seedBalance(t, l, "alice", 100000)

	receipt, err := l.PostTransferSplit(Transfer{
		ID: "t-fee", From: "alice", To: "bob", AmountCents: 25000,
		IdempotencyKey: "fee-key",
	})
	if err != nil {
		t.Fatalf("PostTransferSplit: %v", err)
	}
	// LG-17 convention: the fee is computed once on the whole amount.
	if receipt.FeeCents != 625 { // floor(25000 * 250 / 10000)
		t.Errorf("FeeCents = %d, want 625", receipt.FeeCents)
	}
	if receipt.FeePerChunk {
		t.Error("FeePerChunk = true, want false")
	}
	if receipt.FeeTierIndex != 0 || receipt.FeeRateBps != 250 {
		t.Errorf("tier = %d/%d bps, want 0/250", receipt.FeeTierIndex, receipt.FeeRateBps)
	}
	feeLeg := findEntry(l, "t-fee/fee")
	if feeLeg.AmountCents != 625 || feeLeg.DebitAccount != "fee-rev" || feeLeg.CreditAccount != "alice" {
		t.Errorf("fee leg = %+v, want 625 debit fee-rev credit alice", feeLeg)
	}
	if feeLeg.IdempotencyKey != "fee-key/fee" {
		t.Errorf("fee leg idempotency key = %q, want fee-key/fee", feeLeg.IdempotencyKey)
	}
	// One fee leg total — no per-chunk fee legs.
	for _, e := range l.Entries() {
		if strings.HasSuffix(e.ID, "/fee") && e.ID != "t-fee/fee" {
			t.Errorf("unexpected per-chunk fee leg %q", e.ID)
		}
	}
	if l.Balance("alice") != 100000-25625 || l.Balance("fee-rev") != 625 {
		t.Errorf("balances: alice=%d fee-rev=%d, want 74375/625", l.Balance("alice"), l.Balance("fee-rev"))
	}
}

func TestPostTransferSplitFeePerChunk(t *testing.T) {
	l := New(
		WithMaxTransferChunkCents(10000),
		WithTransferFeePolicy(250, "fee-rev"),
		WithSplitFeePerChunk(),
	)
	seedBalance(t, l, "alice", 100000)

	receipt, err := l.PostTransferSplit(Transfer{
		ID: "t-perchunk", From: "alice", To: "bob", AmountCents: 25000,
	})
	if err != nil {
		t.Fatalf("PostTransferSplit: %v", err)
	}
	if !receipt.FeePerChunk {
		t.Error("FeePerChunk = false, want true")
	}
	// Per-chunk policy fees: 250 + 250 + 125.
	wantFees := []int64{250, 250, 125}
	var total int64
	for i, c := range receipt.Chunks {
		if c.FeeCents != wantFees[i] {
			t.Errorf("chunk %d fee = %d, want %d", i, c.FeeCents, wantFees[i])
		}
		if c.FeeTierIndex != 0 || c.FeeRateBps != 250 {
			t.Errorf("chunk %d tier = %d/%d, want 0/250", i, c.FeeTierIndex, c.FeeRateBps)
		}
		total += c.FeeCents
		feeID := "t-perchunk/chunk-" + itoa(i) + "/fee"
		if fe := findEntry(l, feeID); fe.AmountCents != wantFees[i] {
			t.Errorf("fee leg %s amount = %d, want %d", feeID, fe.AmountCents, wantFees[i])
		}
		if len(c.EntryIDs) != 2 {
			t.Errorf("chunk %d entry IDs = %v, want [chunk, fee]", i, c.EntryIDs)
		}
	}
	if receipt.FeeCents != total || total != 625 {
		t.Errorf("total fee = %d (receipt %d), want 625", total, receipt.FeeCents)
	}
	// Top-level tier is undisclosed in per-chunk mode (tiers vary per chunk).
	if receipt.FeeTierIndex != -1 || receipt.FeeRateBps != 0 {
		t.Errorf("top-level tier = %d/%d, want -1/0", receipt.FeeTierIndex, receipt.FeeRateBps)
	}
	// No once-per-transfer fee leg.
	if e := findEntry(l, "t-perchunk/fee"); e.ID != "" {
		t.Errorf("unexpected once-per-transfer fee leg: %+v", e)
	}
}

func TestPostTransferSplitAtomicOnDailyLimit(t *testing.T) {
	l := New()
	seedBalance(t, l, "alice", 100000)
	if err := l.SetDailyLimit("alice", "USD", 10000); err != nil {
		t.Fatalf("SetDailyLimit: %v", err)
	}

	// No opt-in cap: the chunk size derives from the daily limit
	// (10000), so the transfer splits into 3 chunks — but the day's
	// cumulative budget cannot cover them, and the whole split fails
	// atomically with zero records.
	_, versionBefore := l.Snapshot("alice")
	entriesBefore := len(l.Entries())
	_, err := l.PostTransferSplit(Transfer{
		ID: "t-daily", From: "alice", To: "bob", AmountCents: 25000,
	})
	if !errors.Is(err, ErrDailyLimitExceeded) {
		t.Fatalf("err = %v, want ErrDailyLimitExceeded", err)
	}
	assertLedgerUntouched(t, l, versionBefore, entriesBefore)
	if l.Balance("alice") != 100000 || l.Balance("bob") != 0 {
		t.Errorf("balances moved on rejected split: alice=%d bob=%d", l.Balance("alice"), l.Balance("bob"))
	}
}

func TestPostTransferSplitDailyLimitCumulativeAccounting(t *testing.T) {
	l := New(WithMaxTransferChunkCents(10000))
	seedBalance(t, l, "alice", 100000)
	if err := l.SetDailyLimit("alice", "USD", 100000); err != nil {
		t.Fatalf("SetDailyLimit: %v", err)
	}

	// Chunk size is min(cap, limit) = 10000; the 25000 transfer posts in
	// 3 chunks against the 100000 day budget.
	if _, err := l.PostTransferSplit(Transfer{
		ID: "t-dl-ok", From: "alice", To: "bob", AmountCents: 25000,
	}); err != nil {
		t.Fatalf("PostTransferSplit: %v", err)
	}
	// The day's remaining budget is 75000: a further 80000 posting is
	// rejected, proving the chunks were accounted per chunk.
	if _, _, err := l.Post(JournalEntry{
		ID: "dl-probe", DebitAccount: "bob", CreditAccount: "alice", AmountCents: 80000,
	}); !errors.Is(err, ErrDailyLimitExceeded) {
		t.Fatalf("probe err = %v, want ErrDailyLimitExceeded", err)
	}
	// ...while 75000 still fits.
	if _, _, err := l.Post(JournalEntry{
		ID: "dl-probe2", DebitAccount: "bob", CreditAccount: "alice", AmountCents: 75000,
	}); err != nil {
		t.Fatalf("probe2 err = %v, want nil", err)
	}
}

func TestPostTransferSplitOverdraft(t *testing.T) {
	l := New(WithMaxTransferChunkCents(10000), WithOverdraftProtection("alice"))
	seedBalance(t, l, "alice", 20000)

	// The payer's TOTAL outflow (25000) exceeds the balance: rejected,
	// even though each chunk alone would fit.
	_, versionBefore := l.Snapshot("alice")
	entriesBefore := len(l.Entries())
	_, err := l.PostTransferSplit(Transfer{
		ID: "t-od", From: "alice", To: "bob", AmountCents: 25000,
	})
	if !errors.Is(err, ErrAccountOverdraft) {
		t.Fatalf("err = %v, want ErrAccountOverdraft", err)
	}
	assertLedgerUntouched(t, l, versionBefore, entriesBefore)
}

func TestPostTransferSplitFrozenAtomic(t *testing.T) {
	l := New(WithMaxTransferChunkCents(10000))
	seedBalance(t, l, "alice", 100000)
	l.Freeze("bob")

	_, versionBefore := l.Snapshot("alice")
	entriesBefore := len(l.Entries())
	_, err := l.PostTransferSplit(Transfer{
		ID: "t-frozen", From: "alice", To: "bob", AmountCents: 25000,
	})
	if !errors.Is(err, ErrAccountFrozen) {
		t.Fatalf("err = %v, want ErrAccountFrozen", err)
	}
	assertLedgerUntouched(t, l, versionBefore, entriesBefore)
}

func TestPostTransferSplitIDConflict(t *testing.T) {
	l := New(WithMaxTransferChunkCents(10000))
	seedBalance(t, l, "alice", 100000)

	if _, err := l.PostTransferSplit(Transfer{
		ID: "t-conflict", From: "alice", To: "bob", AmountCents: 25000,
		IdempotencyKey: "conflict-key-1",
	}); err != nil {
		t.Fatalf("first split: %v", err)
	}
	_, versionBefore := l.Snapshot("alice")
	entriesBefore := len(l.Entries())
	// Same transfer ID, different idempotency key: the chunk entry IDs
	// collide.
	_, err := l.PostTransferSplit(Transfer{
		ID: "t-conflict", From: "alice", To: "bob", AmountCents: 25000,
		IdempotencyKey: "conflict-key-2",
	})
	if !errors.Is(err, ErrTransferIDConflict) {
		t.Fatalf("err = %v, want ErrTransferIDConflict", err)
	}
	assertLedgerUntouched(t, l, versionBefore, entriesBefore)
}

func TestPostTransferSplitReverseIDConflict(t *testing.T) {
	l := New(WithMaxTransferChunkCents(10000))
	seedBalance(t, l, "alice", 100000)

	if _, err := l.PostTransferSplit(Transfer{
		ID: "t-rconf", From: "alice", To: "bob", AmountCents: 25000,
	}); err != nil {
		t.Fatalf("split: %v", err)
	}
	// A plain transfer reusing a split transfer's ID is rejected: one
	// identity, one transfer.
	_, err := l.PostTransfer(Transfer{
		ID: "t-rconf", From: "alice", To: "bob", AmountCents: 100,
	})
	if !errors.Is(err, ErrTransferIDConflict) {
		t.Fatalf("err = %v, want ErrTransferIDConflict", err)
	}
}

func TestPostTransferSplitReviewThresholdAntiEvasion(t *testing.T) {
	l := New(WithMaxTransferChunkCents(10000), WithReviewThreshold(20000))
	seedBalance(t, l, "alice", 100000)

	// The whole transfer meets the review threshold: splitting must not
	// smuggle it past dual control.
	_, versionBefore := l.Snapshot("alice")
	entriesBefore := len(l.Entries())
	_, err := l.PostTransferSplit(Transfer{
		ID: "t-review", From: "alice", To: "bob", AmountCents: 25000,
	})
	if !errors.Is(err, ErrSplitTransferNeedsReview) {
		t.Fatalf("err = %v, want ErrSplitTransferNeedsReview", err)
	}
	assertLedgerUntouched(t, l, versionBefore, entriesBefore)

	// Below the threshold, the same chunking works fine.
	receipt, err := l.PostTransferSplit(Transfer{
		ID: "t-review-ok", From: "alice", To: "bob", AmountCents: 15000,
	})
	if err != nil {
		t.Fatalf("below-threshold split: %v", err)
	}
	if len(receipt.Chunks) != 2 {
		t.Errorf("chunks = %d, want 2", len(receipt.Chunks))
	}
}

func TestPostTransferSplitFX(t *testing.T) {
	l := New(
		WithMaxTransferChunkCents(10000),
		WithFXAccount("fx-clearing"),
	)
	if err := l.SetFXRate("USD", "EUR", 9, 10); err != nil {
		t.Fatalf("SetFXRate: %v", err)
	}
	seedBalance(t, l, "alice", 100000)

	receipt, err := l.PostTransferSplit(Transfer{
		ID: "t-fx", From: "alice", To: "bob", AmountCents: 25000,
		Currency: "USD", ToCurrency: "EUR",
	})
	if err != nil {
		t.Fatalf("PostTransferSplit FX: %v", err)
	}
	if receipt.FX == nil {
		t.Fatal("receipt FX is nil")
	}
	if receipt.FX.SourceCents != 25000 || receipt.FX.ConvertedCents != 22500 {
		t.Errorf("FX = %+v, want source 25000 / converted 22500", receipt.FX)
	}
	if receipt.FX.FromCurrency != "USD" || receipt.FX.ToCurrency != "EUR" {
		t.Errorf("FX currencies = %s->%s, want USD->EUR", receipt.FX.FromCurrency, receipt.FX.ToCurrency)
	}
	// Per-chunk legs keep their single currency: the payee leg in EUR,
	// the payer leg in USD.
	wantConverted := []int64{9000, 9000, 4500}
	for i, c := range receipt.Chunks {
		if len(c.EntryIDs) != 2 {
			t.Fatalf("chunk %d entry IDs = %v, want [chunk, fx]", i, c.EntryIDs)
		}
		toLeg := findEntry(l, "t-fx/chunk-"+itoa(i))
		if toLeg.Currency != "EUR" || toLeg.AmountCents != wantConverted[i] {
			t.Errorf("chunk %d payee leg = %d %s, want %d EUR", i, toLeg.AmountCents, toLeg.Currency, wantConverted[i])
		}
		if toLeg.DebitAccount != "bob" || toLeg.CreditAccount != "fx-clearing" {
			t.Errorf("chunk %d payee leg accounts = %s/%s", i, toLeg.DebitAccount, toLeg.CreditAccount)
		}
		fxLeg := findEntry(l, "t-fx/chunk-"+itoa(i)+"/fx")
		if fxLeg.Currency != "USD" || fxLeg.AmountCents != c.AmountCents {
			t.Errorf("chunk %d fx leg = %d %s, want %d USD", i, fxLeg.AmountCents, fxLeg.Currency, c.AmountCents)
		}
	}
	if l.BalanceIn("bob", "EUR") != 22500 {
		t.Errorf("bob EUR balance = %d, want 22500", l.BalanceIn("bob", "EUR"))
	}
	if l.BalanceIn("alice", "USD") != 75000 {
		t.Errorf("alice USD balance = %d, want 75000", l.BalanceIn("alice", "USD"))
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Errorf("VerifyAccountingEquation: %v", err)
	}
}

func TestPostTransferSplitSnapshotRoundTrip(t *testing.T) {
	src := New(
		WithMaxTransferChunkCents(10000),
		WithSplitFeePerChunk(),
		WithTransferFeePolicy(250, "fee-rev"),
	)
	var buf bytes.Buffer
	if err := src.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if !snapshotLedgersEqual(src, restored) {
		t.Fatal("restored ledger state differs from source")
	}
	// The restored ledger still chunks: config round-tripped.
	seedBalance(t, restored, "alice", 100000)
	receipt, err := restored.PostTransferSplit(Transfer{
		ID: "t-after-restore", From: "alice", To: "bob", AmountCents: 25000,
	})
	if err != nil {
		t.Fatalf("PostTransferSplit after restore: %v", err)
	}
	if len(receipt.Chunks) != 3 {
		t.Errorf("chunks after restore = %d, want 3", len(receipt.Chunks))
	}
	if !receipt.FeePerChunk {
		t.Error("FeePerChunk after restore = false, want true")
	}
}

func TestPostTransferSplitValidation(t *testing.T) {
	l := New(WithMaxTransferChunkCents(10000))
	for _, tc := range []struct {
		name string
		t    Transfer
		err  error
	}{
		{"empty ID", Transfer{From: "a", To: "b", AmountCents: 100}, ErrEmptyTransferID},
		{"same account", Transfer{ID: "x", From: "a", To: "a", AmountCents: 100}, ErrTransferSameAccount},
		{"non-positive amount", Transfer{ID: "x", From: "a", To: "b", AmountCents: 0}, ErrTransferNonPositiveAmount},
		{"bad currency", Transfer{ID: "x", From: "a", To: "b", AmountCents: 100, Currency: "US"}, ErrInvalidCurrency},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := l.PostTransferSplit(tc.t); !errors.Is(err, tc.err) {
				t.Errorf("err = %v, want %v", err, tc.err)
			}
		})
	}
}

func TestPostTransferSplitReplayAfterDelegate(t *testing.T) {
	l := New(WithMaxTransferChunkCents(10000))
	seedBalance(t, l, "alice", 100000)

	// A transfer that needed no split delegates to PostTransfer; a
	// later PostTransferSplit replay with the same key returns the
	// single-chunk receipt.
	first, err := l.PostTransferSplit(Transfer{
		ID: "t-delegate-replay", From: "alice", To: "bob", AmountCents: 5000,
		IdempotencyKey: "delegate-key",
	})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := l.PostTransferSplit(Transfer{
		ID: "t-delegate-replay", From: "alice", To: "bob", AmountCents: 5000,
		IdempotencyKey: "delegate-key",
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !second.Duplicate || len(second.Chunks) != 1 {
		t.Errorf("replay = duplicate:%v chunks:%d, want true/1", second.Duplicate, len(second.Chunks))
	}
	if len(first.Entries) != len(second.Entries) || first.Entries[0].ID != second.Entries[0].ID {
		t.Errorf("replay entries differ from original")
	}
}
