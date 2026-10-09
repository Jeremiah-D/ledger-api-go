package ledger

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func mustPostBatch(t *testing.T, l *Ledger, b Batch) BatchReceipt {
	t.Helper()
	receipt, err := l.PostBatch(b)
	if err != nil {
		t.Fatalf("PostBatch: %v", err)
	}
	if receipt.Duplicate {
		t.Fatal("PostBatch: unexpected duplicate receipt")
	}
	return receipt
}

func batchEntry(id string, debit, credit AccountID, cents int64) JournalEntry {
	return JournalEntry{
		ID:            id,
		DebitAccount:  debit,
		CreditAccount: credit,
		AmountCents:   cents,
		Currency:      "USD",
	}
}

func TestPostBatchAtomicSuccess(t *testing.T) {
	l := New()
	receipt := mustPostBatch(t, l, Batch{
		ID: "payroll-2026-10",
		Entries: []JournalEntry{
			batchEntry("e1", "alice", "payroll", 500000),
			batchEntry("e2", "bob", "payroll", 450000),
			batchEntry("e3", "carol", "payroll", 480000),
		},
	})

	if receipt.BatchID != "payroll-2026-10" {
		t.Errorf("receipt.BatchID = %q, want payroll-2026-10", receipt.BatchID)
	}
	if len(receipt.Entries) != 3 {
		t.Fatalf("receipt entries = %d, want 3", len(receipt.Entries))
	}
	for i, want := range []string{"e1", "e2", "e3"} {
		if receipt.Entries[i].ID != want {
			t.Errorf("receipt.Entries[%d].ID = %q, want %q", i, receipt.Entries[i].ID, want)
		}
		if receipt.Entries[i].BatchID != "payroll-2026-10" {
			t.Errorf("receipt.Entries[%d].BatchID = %q, want payroll-2026-10", i, receipt.Entries[i].BatchID)
		}
	}
	// One version bump per entry, in batch order.
	if v := l.version; v != 3 {
		t.Errorf("version = %d, want 3", v)
	}
	if n := len(l.chain); n != 3 {
		t.Errorf("chain links = %d, want 3", n)
	}
	if got := l.Balance("alice"); got != 500000 {
		t.Errorf("alice balance = %d, want 500000", got)
	}
	if got := l.Balance("payroll"); got != -1430000 {
		t.Errorf("payroll balance = %d, want -1430000", got)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Errorf("VerifyAccountingEquation: %v", err)
	}
	if err := l.VerifyChain(); err != nil {
		t.Errorf("VerifyChain: %v", err)
	}
}

func TestPostBatchValidationRejectsWholeBatch(t *testing.T) {
	l := New()
	_, err := l.PostBatch(Batch{
		ID: "bad",
		Entries: []JournalEntry{
			batchEntry("e1", "alice", "payroll", 500000),
			// Zero amount: one bad leg rejects the whole batch.
			batchEntry("e2", "bob", "payroll", 0),
			batchEntry("e3", "carol", "payroll", 480000),
		},
	})
	if !errors.Is(err, ErrNonPositiveAmount) {
		t.Fatalf("err = %v, want ErrNonPositiveAmount", err)
	}
	// Zero落盘: no journal rows, no chain links, no version bump, no balances.
	if v := l.version; v != 0 {
		t.Errorf("version = %d, want 0", v)
	}
	if n := len(l.chain); n != 0 {
		t.Errorf("chain links = %d, want 0", n)
	}
	if n := len(l.entries); n != 0 {
		t.Errorf("journal entries = %d, want 0", n)
	}
	if got := l.Balance("alice"); got != 0 {
		t.Errorf("alice balance = %d, want 0", got)
	}
}

func TestPostBatchEmptyAndDuplicateIDs(t *testing.T) {
	l := New()
	if _, err := l.PostBatch(Batch{ID: "x"}); !errors.Is(err, ErrEmptyBatch) {
		t.Errorf("empty batch err = %v, want ErrEmptyBatch", err)
	}
	if _, err := l.PostBatch(Batch{Entries: []JournalEntry{batchEntry("e1", "a", "b", 100)}}); !errors.Is(err, ErrEmptyBatchID) {
		t.Errorf("empty batch ID err = %v, want ErrEmptyBatchID", err)
	}
	if _, err := l.PostBatch(Batch{ID: "dup", Entries: []JournalEntry{
		batchEntry("e1", "a", "b", 100),
		batchEntry("e1", "c", "d", 100),
	}}); !errors.Is(err, ErrBatchDuplicateEntryID) {
		t.Errorf("duplicate entry ID err = %v, want ErrBatchDuplicateEntryID", err)
	}
	// Entry ID already journaled by an earlier Post.
	if _, _, err := l.Post(batchEntry("taken", "a", "b", 100)); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if _, err := l.PostBatch(Batch{ID: "conflict", Entries: []JournalEntry{
		batchEntry("taken", "c", "d", 100),
	}}); !errors.Is(err, ErrBatchEntryIDConflict) {
		t.Errorf("ID conflict err = %v, want ErrBatchEntryIDConflict", err)
	}
	if v := l.version; v != 1 {
		t.Errorf("version = %d, want 1 (only the earlier Post)", v)
	}
}

func TestPostBatchFrozenRejectsWholeBatch(t *testing.T) {
	l := New()
	l.Freeze("frozen-acct")
	_, err := l.PostBatch(Batch{ID: "b", Entries: []JournalEntry{
		batchEntry("e1", "alice", "payroll", 100),
		batchEntry("e2", "frozen-acct", "payroll", 100),
	}})
	if !errors.Is(err, ErrAccountFrozen) {
		t.Fatalf("err = %v, want ErrAccountFrozen", err)
	}
	if v := l.version; v != 0 {
		t.Errorf("version = %d, want 0", v)
	}
	if n := len(l.entries); n != 0 {
		t.Errorf("journal entries = %d, want 0", n)
	}
}

func TestPostBatchOverdraftSequential(t *testing.T) {
	l := New()
	// Fund the payer with $100.
	if _, _, err := l.Post(batchEntry("fund", "payer", "bank", 10000)); err != nil {
		t.Fatalf("Post: %v", err)
	}
	l.EnableOverdraftProtection("payer")

	// Two legs that are each fine alone ($60, then $50) but overdraw
	// together ($110 > $100): the batch must fail as a unit.
	_, err := l.PostBatch(Batch{ID: "b", Entries: []JournalEntry{
		batchEntry("e1", "alice", "payer", 6000),
		batchEntry("e2", "bob", "payer", 5000),
	}})
	if !errors.Is(err, ErrAccountOverdraft) {
		t.Fatalf("err = %v, want ErrAccountOverdraft", err)
	}
	if got := l.Balance("payer"); got != 10000 {
		t.Errorf("payer balance = %d, want 10000 (untouched)", got)
	}
	if v := l.version; v != 1 {
		t.Errorf("version = %d, want 1 (only the funding Post)", v)
	}

	// A batch that stays within the balance commits.
	mustPostBatch(t, l, Batch{ID: "ok", Entries: []JournalEntry{
		batchEntry("e3", "alice", "payer", 6000),
		batchEntry("e4", "bob", "payer", 4000),
	}})
	if got := l.Balance("payer"); got != 0 {
		t.Errorf("payer balance = %d, want 0", got)
	}
}

func TestPostBatchOverdraftSimulationSeesInflows(t *testing.T) {
	l := New()
	// The payer starts at $0 but the batch's first entry funds it; the
	// second entry's overdraft check must see that inflow.
	l.EnableOverdraftProtection("payer")
	mustPostBatch(t, l, Batch{ID: "b", Entries: []JournalEntry{
		batchEntry("e1", "payer", "bank", 10000),
		batchEntry("e2", "alice", "payer", 9000),
	}})
	if got := l.Balance("payer"); got != 1000 {
		t.Errorf("payer balance = %d, want 1000", got)
	}
}

func TestPostBatchDailyLimitSequential(t *testing.T) {
	l := New()
	if err := l.SetDailyLimit("payer", "USD", 10000); err != nil {
		t.Fatalf("SetDailyLimit: %v", err)
	}
	// $60 + $60 on the same UTC day breaches the $100 daily limit on the
	// second leg: the whole batch is rejected.
	_, err := l.PostBatch(Batch{ID: "b", Entries: []JournalEntry{
		batchEntry("e1", "alice", "payer", 6000),
		batchEntry("e2", "bob", "payer", 6000),
	}})
	if !errors.Is(err, ErrDailyLimitExceeded) {
		t.Fatalf("err = %v, want ErrDailyLimitExceeded", err)
	}
	if v := l.version; v != 0 {
		t.Errorf("version = %d, want 0", v)
	}
	// Two legs inside the limit commit and consume the day's budget.
	mustPostBatch(t, l, Batch{ID: "ok", Entries: []JournalEntry{
		batchEntry("e3", "alice", "payer", 6000),
		batchEntry("e4", "bob", "payer", 4000),
	}})
	if _, err := l.PostBatch(Batch{ID: "over", Entries: []JournalEntry{
		batchEntry("e5", "carol", "payer", 1),
	}}); !errors.Is(err, ErrDailyLimitExceeded) {
		t.Errorf("post-batch outflow err = %v, want ErrDailyLimitExceeded", err)
	}
}

func TestPostBatchIdempotency(t *testing.T) {
	l := New()

	// Batch-level key: reposting returns the whole original receipt.
	r1 := mustPostBatch(t, l, Batch{
		ID:             "payroll-1",
		IdempotencyKey: "batch-key-1",
		Entries: []JournalEntry{
			{ID: "e1", DebitAccount: "alice", CreditAccount: "payroll", AmountCents: 100, Currency: "USD", IdempotencyKey: "entry-key-1"},
			batchEntry("e2", "bob", "payroll", 200),
		},
	})
	r2, err := l.PostBatch(Batch{
		ID:             "payroll-1-retry",
		IdempotencyKey: "batch-key-1",
		Entries:        []JournalEntry{batchEntry("other", "x", "y", 999)},
	})
	if err != nil {
		t.Fatalf("replay PostBatch: %v", err)
	}
	if !r2.Duplicate {
		t.Error("replay: Duplicate = false, want true")
	}
	if r2.BatchID != "payroll-1" {
		t.Errorf("replay BatchID = %q, want payroll-1", r2.BatchID)
	}
	if len(r2.Entries) != len(r1.Entries) {
		t.Fatalf("replay entries = %d, want %d", len(r2.Entries), len(r1.Entries))
	}
	for i := range r1.Entries {
		if r2.Entries[i].ID != r1.Entries[i].ID {
			t.Errorf("replay entry %d = %q, want %q", i, r2.Entries[i].ID, r1.Entries[i].ID)
		}
	}
	if v := l.version; v != 2 {
		t.Errorf("version = %d, want 2 (replay books nothing)", v)
	}

	// Per-entry key replay: an entry whose key was posted by Post
	// resolves to the original entry; the batch commits only the rest.
	if _, _, err := l.Post(JournalEntry{ID: "orig", DebitAccount: "d", CreditAccount: "c", AmountCents: 50, Currency: "USD", IdempotencyKey: "shared-key"}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	r3 := mustPostBatch(t, l, Batch{
		ID: "mixed",
		Entries: []JournalEntry{
			// Same key as the earlier Post: resolves to "orig".
			{ID: "ignored-id", DebitAccount: "x", CreditAccount: "y", AmountCents: 75, Currency: "USD", IdempotencyKey: "shared-key"},
			batchEntry("fresh", "p", "q", 25),
		},
	})
	if r3.Entries[0].ID != "orig" {
		t.Errorf("replayed entry ID = %q, want orig", r3.Entries[0].ID)
	}
	if r3.Entries[0].AmountCents != 50 {
		t.Errorf("replayed entry amount = %d, want 50 (the original)", r3.Entries[0].AmountCents)
	}
	if r3.Entries[1].BatchID != "mixed" {
		t.Errorf("fresh entry BatchID = %q, want mixed", r3.Entries[1].BatchID)
	}
	// Version: 2 (first batch) + 1 (the original Post) + 1 (fresh) = 4.
	if v := l.version; v != 4 {
		t.Errorf("version = %d, want 4", v)
	}
}

func TestPostBatchDuplicateKeyInBatch(t *testing.T) {
	l := New()
	_, err := l.PostBatch(Batch{ID: "b", Entries: []JournalEntry{
		{ID: "e1", DebitAccount: "a", CreditAccount: "b", AmountCents: 100, Currency: "USD", IdempotencyKey: "k"},
		{ID: "e2", DebitAccount: "c", CreditAccount: "d", AmountCents: 100, Currency: "USD", IdempotencyKey: "k"},
	}})
	if !errors.Is(err, ErrBatchDuplicateIdempotencyKey) {
		t.Errorf("err = %v, want ErrBatchDuplicateIdempotencyKey", err)
	}
	if v := l.version; v != 0 {
		t.Errorf("version = %d, want 0", v)
	}
}

func TestPostBatchChainAndEquationCoverBatchEntries(t *testing.T) {
	l := New()
	mustPostBatch(t, l, Batch{ID: "b1", Entries: []JournalEntry{
		batchEntry("e1", "alice", "payroll", 1000),
		batchEntry("e2", "bob", "payroll", 2000),
	}})
	mustPostBatch(t, l, Batch{ID: "b2", Entries: []JournalEntry{
		batchEntry("e3", "carol", "payroll", 3000),
	}})

	// Reconcile covers batch entries like ordinary postings.
	report := l.Reconcile(time.Now())
	if !report.AccountingEquationOK {
		t.Errorf("AccountingEquationOK = false: %s", report.AccountingError)
	}
	if len(report.Discrepancies) != 0 {
		t.Errorf("discrepancies = %v, want none", report.Discrepancies)
	}
	if report.AuditChain.Links != 3 {
		t.Errorf("audit chain links = %d, want 3", report.AuditChain.Links)
	}

	// Tampering with a batch entry breaks the chain: batch entries are
	// covered, not just counted.
	e := l.entries["e2"]
	e.AmountCents = 999999
	l.entries["e2"] = e
	if err := l.VerifyChain(); err == nil {
		t.Error("VerifyChain = nil after tampering with a batch entry, want error")
	}
}

func TestPostBatchConcurrent(t *testing.T) {
	l := New()
	const workers = 8
	const batchesPerWorker = 25
	var wg sync.WaitGroup
	errs := make([]error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < batchesPerWorker; i++ {
				bid := fmt.Sprintf("w%d-b%d", w, i)
				entries := []JournalEntry{
					{ID: bid + "-e1", DebitAccount: "a", CreditAccount: "b", AmountCents: 10, Currency: "USD"},
					{ID: bid + "-e2", DebitAccount: "c", CreditAccount: "d", AmountCents: 20, Currency: "USD"},
				}
				if _, err := l.PostBatch(Batch{ID: bid, Entries: entries, IdempotencyKey: "key-" + bid}); err != nil {
					errs[w] = err
					return
				}
				// Replay the same batch key: must be a clean duplicate.
				r, err := l.PostBatch(Batch{ID: bid, Entries: entries, IdempotencyKey: "key-" + bid})
				if err != nil {
					errs[w] = err
					return
				}
				if !r.Duplicate {
					errs[w] = fmt.Errorf("replay of %s not marked duplicate", bid)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	for w, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", w, err)
		}
	}
	wantVersion := uint64(workers * batchesPerWorker * 2)
	if v := l.version; v != wantVersion {
		t.Errorf("version = %d, want %d", v, wantVersion)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Errorf("VerifyAccountingEquation: %v", err)
	}
	if err := l.VerifyChain(); err != nil {
		t.Errorf("VerifyChain: %v", err)
	}
}

func TestPostBatchAuditEvent(t *testing.T) {
	al := mustAuditLog(t)
	defer al.Close()
	l := New(WithAuditLog(al))

	mustPostBatch(t, l, Batch{
		ID: "payroll-1",
		Entries: []JournalEntry{
			batchEntry("e1", "alice", "payroll", 100),
			batchEntry("e2", "bob", "payroll", 200),
		},
	})
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, corrupt, err := ReadAuditLog(al.Dir(), time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt = %d, want 0", corrupt)
	}
	var found *AuditEvent
	for i := range events {
		if events[i].Op == "post_batch" {
			found = &events[i]
		}
	}
	if found == nil {
		t.Fatalf("no post_batch audit event; ops: %v", opsOf(events))
	}
	if found.TraceID != "payroll-1" {
		t.Errorf("trace = %q, want payroll-1", found.TraceID)
	}
	if got := found.Details["entries"]; got != float64(2) {
		t.Errorf("details.entries = %v, want 2", got)
	}
	if got := found.Details["new_entries"]; got != float64(2) {
		t.Errorf("details.new_entries = %v, want 2", got)
	}
	if found.VersionAfter != found.VersionBefore+2 {
		t.Errorf("version bracket = %d->%d, want +2", found.VersionBefore, found.VersionAfter)
	}
}

func opsOf(events []AuditEvent) []string {
	ops := make([]string, 0, len(events))
	for _, e := range events {
		ops = append(ops, e.Op)
	}
	return ops
}

func TestPostBatchSnapshotRoundTrip(t *testing.T) {
	l := New()
	mustPostBatch(t, l, Batch{
		ID:             "payroll-1",
		IdempotencyKey: "batch-key-1",
		Entries: []JournalEntry{
			batchEntry("e1", "alice", "payroll", 100),
			batchEntry("e2", "bob", "payroll", 200),
		},
	})

	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	l2, err := ImportSnapshot(&buf)
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}

	// The batch key registry survived: replaying after restore is a
	// duplicate, not a second booking.
	r, err := l2.PostBatch(Batch{
		ID:             "payroll-1",
		IdempotencyKey: "batch-key-1",
		Entries:        []JournalEntry{batchEntry("e1", "alice", "payroll", 100)},
	})
	if err != nil {
		t.Fatalf("replay after restore: %v", err)
	}
	if !r.Duplicate {
		t.Error("replay after restore: Duplicate = false, want true")
	}
	if v := l2.version; v != 2 {
		t.Errorf("restored version = %d, want 2", v)
	}
	// BatchID metadata survived the round trip.
	if e := l2.entries["e1"]; e.BatchID != "payroll-1" {
		t.Errorf("restored e1.BatchID = %q, want payroll-1", e.BatchID)
	}
	if err := l2.VerifyChain(); err != nil {
		t.Errorf("restored VerifyChain: %v", err)
	}
}

func TestPostBatchIncrementalSnapshotRoundTrip(t *testing.T) {
	l := New()
	if _, _, err := l.Post(batchEntry("pre", "a", "b", 10)); err != nil {
		t.Fatalf("Post: %v", err)
	}
	// The replica starts from a full snapshot at the base version, so its
	// chain head matches the source's.
	var full bytes.Buffer
	if err := l.ExportSnapshot(&full); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	l2, err := ImportSnapshot(&full)
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}

	mustPostBatch(t, l, Batch{
		ID:             "payroll-1",
		IdempotencyKey: "batch-key-1",
		Entries:        []JournalEntry{batchEntry("e1", "alice", "payroll", 100)},
	})

	var delta bytes.Buffer
	if err := l.ExportIncrementalSnapshot(&delta, 1); err != nil {
		t.Fatalf("ExportIncrementalSnapshot: %v", err)
	}
	if err := l2.ImportIncrementalSnapshot(&delta); err != nil {
		t.Fatalf("ImportIncrementalSnapshot: %v", err)
	}
	r, err := l2.PostBatch(Batch{
		ID:             "payroll-1",
		IdempotencyKey: "batch-key-1",
		Entries:        []JournalEntry{batchEntry("e1", "alice", "payroll", 100)},
	})
	if err != nil {
		t.Fatalf("replay after incremental restore: %v", err)
	}
	if !r.Duplicate {
		t.Error("replay after incremental restore: Duplicate = false, want true")
	}
	if err := l2.VerifyChain(); err != nil {
		t.Errorf("replica VerifyChain: %v", err)
	}
}

func TestPostBatchTTLExpiry(t *testing.T) {
	l := New(WithIdempotencyTTL(time.Millisecond))
	r1 := mustPostBatch(t, l, Batch{
		ID:             "payroll-1",
		IdempotencyKey: "batch-key-1",
		Entries:        []JournalEntry{batchEntry("e1", "alice", "payroll", 100)},
	})
	if r1.Duplicate {
		t.Fatal("first post marked duplicate")
	}
	time.Sleep(5 * time.Millisecond)
	if n := l.ExpireIdempotencyKeys(); n < 1 {
		t.Fatalf("ExpireIdempotencyKeys = %d, want >= 1", n)
	}
	// After the TTL the key is forgotten: reposting books a brand-new
	// batch (the new entries need fresh IDs — the old IDs are journaled).
	r2, err := l.PostBatch(Batch{
		ID:             "payroll-2",
		IdempotencyKey: "batch-key-1",
		Entries:        []JournalEntry{batchEntry("e2", "alice", "payroll", 100)},
	})
	if err != nil {
		t.Fatalf("repost after TTL: %v", err)
	}
	if r2.Duplicate {
		t.Error("repost after TTL: Duplicate = true, want false (key expired)")
	}
	if v := l.version; v != 2 {
		t.Errorf("version = %d, want 2", v)
	}
}

func TestPostBatchCurrencyNormalization(t *testing.T) {
	l := New()
	// Empty currency normalizes to the default, like Post.
	r := mustPostBatch(t, l, Batch{ID: "b", Entries: []JournalEntry{
		{ID: "e1", DebitAccount: "a", CreditAccount: "b", AmountCents: 100},
	}})
	if r.Entries[0].Currency != DefaultCurrency {
		t.Errorf("currency = %q, want %q", r.Entries[0].Currency, DefaultCurrency)
	}
	if _, err := l.PostBatch(Batch{ID: "bad", Entries: []JournalEntry{
		{ID: "e2", DebitAccount: "a", CreditAccount: "b", AmountCents: 100, Currency: "US"},
	}}); !errors.Is(err, ErrInvalidCurrency) {
		t.Errorf("bad currency err = %v, want ErrInvalidCurrency", err)
	}
}
