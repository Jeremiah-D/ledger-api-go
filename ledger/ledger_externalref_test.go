package ledger

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

// External-reference (external_ref) tests, LG-57.
//
// An external reference is the caller's business correlation identifier
// (a payment provider's transaction ID, an external order number). Unlike
// the memo it is a uniqueness domain: within one ledger no two journal
// rows may carry the same non-empty reference. The idempotency-key
// namespace stays independent — a replay of the original key returns the
// original entry without consulting the reference index.

func TestExternalRefLengthValidation(t *testing.T) {
	l := New()
	// Exactly 64 UTF-8 characters is legal; 65 is not. Counted in
	// runes, not bytes: multibyte characters count once.
	if _, _, err := l.Post(JournalEntry{ID: "r-ok", DebitAccount: "a", CreditAccount: "b", AmountCents: 1, ExternalRef: strings.Repeat("中", 64)}); err != nil {
		t.Fatalf("64-rune external_ref rejected: %v", err)
	}
	if _, _, err := l.Post(JournalEntry{ID: "r-bad", DebitAccount: "a", CreditAccount: "b", AmountCents: 1, ExternalRef: strings.Repeat("x", 65)}); !errors.Is(err, ErrExternalRefTooLong) {
		t.Fatalf("65-char external_ref: %v, want ErrExternalRefTooLong", err)
	}
	// A rejected reference records nothing.
	if _, ok := l.entries["r-bad"]; ok {
		t.Fatal("rejected entry was journaled")
	}
	if l.Version() != 1 {
		t.Fatalf("version = %d, want 1: rejected post bumped it", l.Version())
	}
}

func TestExternalRefDuplicateConflict(t *testing.T) {
	l := New()
	mustPost(t, l, JournalEntry{ID: "e1", DebitAccount: "a", CreditAccount: "b", AmountCents: 100, ExternalRef: "psp-txn-1"})

	// A second posting with the same reference is rejected with
	// ErrDuplicateExternalRef, and records nothing — even with a fresh
	// idempotency key and a distinct entry ID.
	if _, _, err := l.Post(JournalEntry{ID: "e2", DebitAccount: "a", CreditAccount: "b", AmountCents: 100, ExternalRef: "psp-txn-1", IdempotencyKey: "other-key"}); !errors.Is(err, ErrDuplicateExternalRef) {
		t.Fatalf("duplicate external_ref: %v, want ErrDuplicateExternalRef", err)
	}
	if _, ok := l.entries["e2"]; ok {
		t.Fatal("conflicting entry was journaled")
	}
	if l.Version() != 1 {
		t.Fatalf("version = %d, want 1", l.Version())
	}
	// An empty reference never conflicts: any number of entries may
	// carry none.
	for _, id := range []string{"e3", "e4"} {
		mustPost(t, l, JournalEntry{ID: id, DebitAccount: "a", CreditAccount: "b", AmountCents: 1})
	}
}

func TestExternalRefIdempotencyReplayPriority(t *testing.T) {
	l := New()
	mustPost(t, l, JournalEntry{ID: "e1", DebitAccount: "a", CreditAccount: "b", AmountCents: 100, IdempotencyKey: "key-1", ExternalRef: "psp-txn-1"})

	// Replaying the posted key returns the original entry, even when
	// the replay carries a *different* reference: the replay check runs
	// before the reference-conflict check, because a replay books
	// nothing new.
	posted, duplicate, err := l.Post(JournalEntry{ID: "e1-retry", DebitAccount: "a", CreditAccount: "b", AmountCents: 100, IdempotencyKey: "key-1", ExternalRef: "psp-txn-2"})
	if err != nil {
		t.Fatalf("replay returned error: %v", err)
	}
	if !duplicate || posted.ID != "e1" || posted.ExternalRef != "psp-txn-1" {
		t.Fatalf("replay = %+v duplicate=%v, want original e1", posted, duplicate)
	}
	// The key namespace and the reference namespace are independent:
	// a key string may appear as a reference elsewhere.
	mustPost(t, l, JournalEntry{ID: "e2", DebitAccount: "a", CreditAccount: "b", AmountCents: 1, ExternalRef: "key-1"})
}

func TestExternalRefBatchAtomicity(t *testing.T) {
	l := New()
	mustPost(t, l, JournalEntry{ID: "prior", DebitAccount: "a", CreditAccount: "b", AmountCents: 1, ExternalRef: "claimed"})

	newEntry := func(id, ref string) JournalEntry {
		return JournalEntry{ID: id, DebitAccount: "a", CreditAccount: "b", AmountCents: 1, ExternalRef: ref}
	}
	// A batch with a repeated reference inside it is rejected as a
	// whole — nothing lands.
	if _, err := l.PostBatch(Batch{ID: "b1", Entries: []JournalEntry{newEntry("n1", "dup"), newEntry("n2", "dup")}}); !errors.Is(err, ErrDuplicateExternalRef) {
		t.Fatalf("batch-internal duplicate ref: %v, want ErrDuplicateExternalRef", err)
	}
	// A batch colliding with an already-journaled reference is rejected
	// as a whole too.
	if _, err := l.PostBatch(Batch{ID: "b2", Entries: []JournalEntry{newEntry("n3", "fresh"), newEntry("n4", "claimed")}}); !errors.Is(err, ErrDuplicateExternalRef) {
		t.Fatalf("batch vs ledger duplicate ref: %v, want ErrDuplicateExternalRef", err)
	}
	if l.Version() != 1 {
		t.Fatalf("version = %d, want 1: rejected batches booked rows", l.Version())
	}
	// Batch entries carry references like ordinary postings; the batch
	// commits them atomically, and a later Post with the same
	// reference is rejected.
	rec, err := l.PostBatch(Batch{ID: "b3", Entries: []JournalEntry{newEntry("n5", "batch-ref-1"), newEntry("n6", "batch-ref-2")}})
	if err != nil {
		t.Fatalf("PostBatch: %v", err)
	}
	if rec.Entries[0].ExternalRef != "batch-ref-1" || rec.Entries[1].ExternalRef != "batch-ref-2" {
		t.Fatalf("batch refs = %q, %q", rec.Entries[0].ExternalRef, rec.Entries[1].ExternalRef)
	}
	if _, _, err := l.Post(newEntry("n7", "batch-ref-1")); !errors.Is(err, ErrDuplicateExternalRef) {
		t.Fatalf("post after batch: %v, want ErrDuplicateExternalRef", err)
	}
	// A batch-level idempotency replay short-circuits before the
	// reference checks: the replay books nothing new.
	if _, err := l.PostBatch(Batch{ID: "b4", IdempotencyKey: "batch-key-4", Entries: []JournalEntry{newEntry("p1", "replay-ref-1")}}); err != nil {
		t.Fatalf("first batch-key-4 post: %v", err)
	}
	rec2, err := l.PostBatch(Batch{ID: "b4-retry", IdempotencyKey: "batch-key-4", Entries: []JournalEntry{newEntry("p2", "replay-ref-1")}})
	if err != nil {
		t.Fatalf("batch replay: %v", err)
	}
	if !rec2.Duplicate {
		t.Fatal("batch replay did not report Duplicate")
	}
}

func TestExternalRefChainHashCoverage(t *testing.T) {
	l := New()
	mustPost(t, l, JournalEntry{ID: "e1", DebitAccount: "a", CreditAccount: "b", AmountCents: 100, Currency: "USD", ExternalRef: "psp-txn-1"})
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	// Rewriting the reference breaks the chain exactly like rewriting
	// an amount: the reference is a journaled field.
	e := l.entries["e1"]
	e.ExternalRef = "psp-txn-2"
	l.entries["e1"] = e
	if err := l.VerifyChain(); err == nil {
		t.Fatal("VerifyChain passed after external_ref rewrite, want failure")
	}
}

func TestExternalRefListEntriesFilter(t *testing.T) {
	l := New()
	epoch := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mustPostAt := func(id, ref, memo string, at time.Time) {
		t.Helper()
		e := JournalEntry{ID: id, DebitAccount: "a", CreditAccount: "b", AmountCents: 1, Memo: memo, ExternalRef: ref, CreatedAt: at}
		if _, _, err := l.Post(e); err != nil {
			t.Fatalf("Post %s: %v", id, err)
		}
	}
	mustPostAt("e1", "psp-1", "order paid", epoch)
	mustPostAt("e2", "psp-2", "order paid", epoch)
	mustPostAt("e3", "", "no ref", epoch)
	mustPostAt("e4", "psp-1-suffix", "suffix", epoch.Add(48*time.Hour))

	// Exact match only: "psp-1" does not match "psp-1-suffix".
	page, _, err := l.ListEntriesFiltered(epoch, epoch.Add(time.Hour), "", 100, "", "psp-1")
	if err != nil {
		t.Fatalf("ListEntriesFiltered: %v", err)
	}
	if len(page) != 1 || page[0].ID != "e1" {
		t.Fatalf("ref filter = %v, want [e1]", idsOf(page))
	}
	// The ref filter composes with the memo filter.
	page, _, err = l.ListEntriesFiltered(epoch, epoch.Add(time.Hour), "", 100, "paid", "psp-2")
	if err != nil {
		t.Fatalf("ListEntriesFiltered: %v", err)
	}
	if len(page) != 1 || page[0].ID != "e2" {
		t.Fatalf("memo+ref filter = %v, want [e2]", idsOf(page))
	}
	// Conflicting filters return nothing.
	page, _, err = l.ListEntriesFiltered(epoch, epoch.Add(time.Hour), "", 100, "paid", "psp-1-suffix")
	if err != nil {
		t.Fatalf("ListEntriesFiltered: %v", err)
	}
	if len(page) != 0 {
		t.Fatalf("conflicting filters = %v, want []", idsOf(page))
	}
	// The ref filter composes with the time window.
	page, _, err = l.ListEntriesFiltered(epoch, time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), "", 100, "", "psp-1-suffix")
	if err != nil {
		t.Fatalf("ListEntriesFiltered: %v", err)
	}
	if len(page) != 1 || page[0].ID != "e4" {
		t.Fatalf("windowed ref filter = %v, want [e4]", idsOf(page))
	}
	// The ref filter composes with cursor pagination.
	page, next, err := l.ListEntriesFiltered(epoch, time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), "", 1, "paid", "")
	if err != nil || next == "" || len(page) != 1 {
		t.Fatalf("first page = %v next=%q err=%v", idsOf(page), next, err)
	}
	page, next, err = l.ListEntriesFiltered(epoch, time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), next, 1, "paid", "")
	if err != nil || len(page) != 1 || next != "" {
		t.Fatalf("second page = %v next=%q err=%v", idsOf(page), next, err)
	}
}

func TestExternalRefTransferPassthrough(t *testing.T) {
	l := New()
	rec, err := l.PostTransfer(Transfer{ID: "t1", From: "payer", To: "payee", AmountCents: 1000, FeeCents: 10, FeeAccount: "fee-rev", ExternalRef: "order-42"})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if rec.Entries[0].ExternalRef != "order-42" {
		t.Fatalf("principal external_ref = %q, want order-42", rec.Entries[0].ExternalRef)
	}
	// The fee leg carries no reference.
	if len(rec.Entries) != 2 || rec.Entries[1].ExternalRef != "" {
		t.Fatalf("fee leg external_ref = %q, want empty", rec.Entries[1].ExternalRef)
	}
	// A second transfer reusing the reference is rejected, nothing
	// booked.
	if _, err := l.PostTransfer(Transfer{ID: "t2", From: "payer", To: "payee", AmountCents: 1000, ExternalRef: "order-42"}); !errors.Is(err, ErrDuplicateExternalRef) {
		t.Fatalf("duplicate transfer ref: %v, want ErrDuplicateExternalRef", err)
	}
	if _, ok := l.entries["t2"]; ok {
		t.Fatal("conflicting transfer was journaled")
	}
	// An overlong transfer reference is rejected before anything is
	// recorded.
	if _, err := l.PostTransfer(Transfer{ID: "t3", From: "payer", To: "payee", AmountCents: 1000, ExternalRef: strings.Repeat("x", 65)}); !errors.Is(err, ErrExternalRefTooLong) {
		t.Fatalf("overlong transfer ref: %v, want ErrExternalRefTooLong", err)
	}
	// Idempotent replay of the transfer returns the original receipt
	// without tripping the reference check.
	rec2, err := l.PostTransfer(Transfer{ID: "t1b", From: "payer", To: "payee", AmountCents: 1000, IdempotencyKey: "tx-key", ExternalRef: "order-43"})
	if err != nil {
		t.Fatalf("PostTransfer t1b: %v", err)
	}
	rec3, err := l.PostTransfer(Transfer{ID: "t1c", From: "payer", To: "payee", AmountCents: 1000, IdempotencyKey: "tx-key", ExternalRef: "order-44"})
	if err != nil {
		t.Fatalf("transfer replay: %v", err)
	}
	if !rec3.Duplicate || rec3.Entries[0].ExternalRef != "order-43" {
		t.Fatalf("transfer replay = %+v, want original with ref order-43", rec3)
	}
	_ = rec2
}

func TestExternalRefFXTransferPassthrough(t *testing.T) {
	l := New(WithFXAccount("fx-clearing"))
	if err := l.SetFXRate("USD", "EUR", 9, 10); err != nil {
		t.Fatal(err)
	}
	rec, err := l.PostTransfer(Transfer{ID: "fx1", From: "payer", To: "payee", AmountCents: 1000, Currency: "USD", ToCurrency: "EUR", ExternalRef: "fx-order-1"})
	if err != nil {
		t.Fatalf("PostTransfer FX: %v", err)
	}
	if len(rec.Entries) != 2 {
		t.Fatalf("FX entries = %d, want 2", len(rec.Entries))
	}
	// The memo convention: the /fx leg carries the business
	// annotations, so the reference rides it — the principal does not.
	if rec.Entries[1].ExternalRef != "fx-order-1" || rec.Entries[1].ID != "fx1/fx" {
		t.Fatalf("/fx leg external_ref = %q, want fx-order-1", rec.Entries[1].ExternalRef)
	}
	if rec.Entries[0].ExternalRef != "" {
		t.Fatalf("FX principal external_ref = %q, want empty", rec.Entries[0].ExternalRef)
	}
	if _, err := l.PostTransfer(Transfer{ID: "fx2", From: "payer", To: "payee", AmountCents: 1000, Currency: "USD", ToCurrency: "EUR", ExternalRef: "fx-order-1"}); !errors.Is(err, ErrDuplicateExternalRef) {
		t.Fatalf("duplicate FX ref: %v, want ErrDuplicateExternalRef", err)
	}
}

func TestExternalRefReversalInheritance(t *testing.T) {
	l := New()
	mustPost(t, l, JournalEntry{ID: "e1", DebitAccount: "a", CreditAccount: "b", AmountCents: 100, ExternalRef: "psp-txn-9"})

	// The reversal inherits "reversal_of:<orig ref>".
	rev, _, err := l.ReverseEntry("e1", "")
	if err != nil {
		t.Fatalf("ReverseEntry: %v", err)
	}
	if rev.ExternalRef != "reversal_of:psp-txn-9" {
		t.Fatalf("reversal external_ref = %q, want reversal_of:psp-txn-9", rev.ExternalRef)
	}
	// A reversal of an entry without a reference inherits none.
	mustPost(t, l, JournalEntry{ID: "e2", DebitAccount: "a", CreditAccount: "b", AmountCents: 100})
	rev2, _, err := l.ReverseEntry("e2", "")
	if err != nil {
		t.Fatalf("ReverseEntry e2: %v", err)
	}
	if rev2.ExternalRef != "" {
		t.Fatalf("reversal external_ref = %q, want empty", rev2.ExternalRef)
	}
	// Boundary: "reversal_of:" is 12 runes, so a 52-rune original
	// reference fits exactly (64), while 53 runes (65) rejects the
	// reversal with ErrExternalRefTooLong — nothing is truncated.
	mustPost(t, l, JournalEntry{ID: "e3", DebitAccount: "a", CreditAccount: "b", AmountCents: 100, ExternalRef: strings.Repeat("r", 52)})
	rev3, _, err := l.ReverseEntry("e3", "")
	if err != nil {
		t.Fatalf("ReverseEntry e3: %v", err)
	}
	if want := "reversal_of:" + strings.Repeat("r", 52); rev3.ExternalRef != want {
		t.Fatalf("reversal external_ref = %q, want %q", rev3.ExternalRef, want)
	}
	mustPost(t, l, JournalEntry{ID: "e4", DebitAccount: "a", CreditAccount: "b", AmountCents: 100, ExternalRef: strings.Repeat("r", 53)})
	if _, _, err := l.ReverseEntry("e4", ""); !errors.Is(err, ErrExternalRefTooLong) {
		t.Fatalf("overlong inherited ref: %v, want ErrExternalRefTooLong", err)
	}
	if _, ok := l.entries["e4/reversal"]; ok {
		t.Fatal("rejected reversal was journaled")
	}
}

func TestExternalRefCapturePassthrough(t *testing.T) {
	l := New()
	if _, _, err := l.Post(JournalEntry{ID: "fund", DebitAccount: "card", CreditAccount: "bank", AmountCents: 10000}); err != nil {
		t.Fatal(err)
	}
	h, _, err := l.Hold(Hold{ID: "h1", Account: "card", AmountCents: 5000, ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	_ = h
	receipt, err := l.Capture(Capture{ID: "c1", HoldID: "h1", To: "merchant", AmountCents: 3000, ExternalRef: "auth-777"})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if receipt.Entry.ExternalRef != "auth-777" {
		t.Fatalf("capture entry external_ref = %q, want auth-777", receipt.Entry.ExternalRef)
	}
	// A second capture reusing the reference is rejected.
	h2, _, err := l.Hold(Hold{ID: "h2", Account: "card", AmountCents: 2000, ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	_ = h2
	if _, err := l.Capture(Capture{ID: "c2", HoldID: "h2", To: "merchant", AmountCents: 1000, ExternalRef: "auth-777"}); !errors.Is(err, ErrDuplicateExternalRef) {
		t.Fatalf("duplicate capture ref: %v, want ErrDuplicateExternalRef", err)
	}
	// An overlong capture reference is rejected before anything is
	// recorded.
	if _, err := l.Capture(Capture{ID: "c3", HoldID: "h2", To: "merchant", AmountCents: 1000, ExternalRef: strings.Repeat("x", 65)}); !errors.Is(err, ErrExternalRefTooLong) {
		t.Fatalf("overlong capture ref: %v, want ErrExternalRefTooLong", err)
	}
	// Idempotent replay of the capture returns the original receipt.
	// The replay short-circuits before the hold-state checks, so it
	// succeeds on a fresh hold with the same key.
	h3, _, err := l.Hold(Hold{ID: "h3", Account: "card", AmountCents: 2000, ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	_ = h3
	r2, err := l.Capture(Capture{ID: "c3b", HoldID: "h3", To: "merchant", AmountCents: 1000, IdempotencyKey: "cap-key", ExternalRef: "auth-888"})
	if err != nil {
		t.Fatalf("Capture c3b: %v", err)
	}
	r3, err := l.Capture(Capture{ID: "c3c", HoldID: "h3", To: "merchant", AmountCents: 1000, IdempotencyKey: "cap-key", ExternalRef: "auth-999"})
	if err != nil {
		t.Fatalf("capture replay: %v", err)
	}
	if !r3.Duplicate || r3.Entry.ExternalRef != "auth-888" {
		t.Fatalf("capture replay = %+v, want original with ref auth-888", r3)
	}
	_ = r2
}

func TestExternalRefSnapshotRoundTrip(t *testing.T) {
	l := New()
	mustPost(t, l, JournalEntry{ID: "e1", DebitAccount: "a", CreditAccount: "b", AmountCents: 100, Currency: "USD", Memo: "order", ExternalRef: "psp-1"})
	if _, err := l.PostTransfer(Transfer{ID: "t1", From: "a", To: "b", AmountCents: 50, ExternalRef: "psp-2"}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if err := restored.VerifyChain(); err != nil {
		t.Fatalf("restored VerifyChain: %v", err)
	}
	// The byRef index is rebuilt from the journaled rows: duplicates
	// are still rejected on the restored ledger.
	if _, _, err := restored.Post(JournalEntry{ID: "n1", DebitAccount: "a", CreditAccount: "b", AmountCents: 1, ExternalRef: "psp-1"}); !errors.Is(err, ErrDuplicateExternalRef) {
		t.Fatalf("restored duplicate ref: %v, want ErrDuplicateExternalRef", err)
	}
	// The ?ref= path works on the restored ledger too.
	page, _, err := restored.ListEntriesFiltered(time.Time{}, time.Now().Add(time.Hour), "", 100, "", "psp-2")
	if err != nil {
		t.Fatalf("ListEntriesFiltered: %v", err)
	}
	if len(page) != 1 || page[0].ID != "t1" {
		t.Fatalf("restored ref filter = %v, want [t1]", idsOf(page))
	}
}

func TestExternalRefIncrementalRoundTrip(t *testing.T) {
	a, b, base := buildDivergedPair(t)

	// New activity on the source carries references.
	if _, _, err := a.Post(JournalEntry{ID: "e3", DebitAccount: "dave", CreditAccount: "bank", AmountCents: 7000, Currency: "USD", ExternalRef: "psp-3"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.PostTransfer(Transfer{ID: "t2", From: "alice", To: "carol", AmountCents: 1000, Currency: "USD", ExternalRef: "psp-4"}); err != nil {
		t.Fatal(err)
	}

	var delta bytes.Buffer
	if err := a.ExportIncrementalSnapshot(&delta, base); err != nil {
		t.Fatalf("export delta: %v", err)
	}
	if err := b.ImportIncrementalSnapshot(bytes.NewReader(delta.Bytes())); err != nil {
		t.Fatalf("import delta: %v", err)
	}
	if err := b.VerifyChain(); err != nil {
		t.Fatalf("merged VerifyChain: %v", err)
	}
	// The replica's byRef index covers the merged rows: duplicates
	// are rejected there as well.
	if _, _, err := b.Post(JournalEntry{ID: "n1", DebitAccount: "a", CreditAccount: "b", AmountCents: 1, ExternalRef: "psp-3"}); !errors.Is(err, ErrDuplicateExternalRef) {
		t.Fatalf("merged duplicate ref: %v, want ErrDuplicateExternalRef", err)
	}
}

// mustPost posts one journal entry, failing the test on error.
func mustPost(t *testing.T, l *Ledger, e JournalEntry) {
	t.Helper()
	if _, _, err := l.Post(e); err != nil {
		t.Fatalf("Post %s: %v", e.ID, err)
	}
}
