package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestMemoLengthValidation(t *testing.T) {
	l := New()
	// Exactly 255 UTF-8 characters is legal; 256 is not. Counted in
	// runes, not bytes: multibyte characters count once.
	if _, _, err := l.Post(JournalEntry{ID: "m-ok", DebitAccount: "a", CreditAccount: "b", AmountCents: 1, Memo: strings.Repeat("中", 255)}); err != nil {
		t.Fatalf("255-rune memo rejected: %v", err)
	}
	if _, _, err := l.Post(JournalEntry{ID: "m-bad", DebitAccount: "a", CreditAccount: "b", AmountCents: 1, Memo: strings.Repeat("x", 256)}); !errors.Is(err, ErrMemoTooLong) {
		t.Fatalf("256-char memo: %v, want ErrMemoTooLong", err)
	}
	// A rejected memo records nothing.
	if _, ok := l.entries["m-bad"]; ok {
		t.Fatal("rejected entry was journaled")
	}
}

func TestMemoChainHashCoverage(t *testing.T) {
	l := New()
	mustPostThreshold(t, l, JournalEntry{ID: "e1", DebitAccount: "a", CreditAccount: "b", AmountCents: 100, Currency: "USD", Memo: "order-123"})
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	// Rewriting the memo breaks the chain exactly like rewriting an
	// amount: the memo is a journaled field.
	e := l.entries["e1"]
	e.Memo = "order-999"
	l.entries["e1"] = e
	if err := l.VerifyChain(); err == nil {
		t.Fatal("VerifyChain passed after memo rewrite, want failure")
	}
}

func TestMemoListEntriesFilter(t *testing.T) {
	l := New()
	epoch := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mustPost := func(id, memo string) {
		t.Helper()
		e := JournalEntry{ID: id, DebitAccount: "a", CreditAccount: "b", AmountCents: 1, Memo: memo, CreatedAt: epoch}
		if _, _, err := l.Post(e); err != nil {
			t.Fatalf("Post %s: %v", id, err)
		}
	}
	mustPost("e1", "order-123 paid")
	mustPost("e2", "order-456 paid")
	mustPost("e3", "")
	mustPost("e4", "ORDER-123 refund")

	// Keyword filter matches the substring, case-sensitively.
	page, _, err := l.ListEntriesFiltered(epoch, epoch.Add(time.Hour), "", 100, "order-123")
	if err != nil {
		t.Fatalf("ListEntriesFiltered: %v", err)
	}
	if len(page) != 1 || page[0].ID != "e1" {
		t.Fatalf("memo filter = %v, want [e1]", idsOf(page))
	}
	// "paid" matches two entries.
	page, _, err = l.ListEntriesFiltered(epoch, epoch.Add(time.Hour), "", 100, "paid")
	if err != nil {
		t.Fatalf("ListEntriesFiltered: %v", err)
	}
	if len(page) != 2 {
		t.Fatalf("memo filter 'paid' = %v, want 2 entries", idsOf(page))
	}
	// Empty memo means no filter: all four entries.
	page, _, err = l.ListEntriesFiltered(epoch, epoch.Add(time.Hour), "", 100, "")
	if err != nil {
		t.Fatalf("ListEntriesFiltered: %v", err)
	}
	if len(page) != 4 {
		t.Fatalf("empty memo filter = %d entries, want 4", len(page))
	}
	// The filter composes with cursor pagination: page through a
	// two-result filter one entry at a time.
	page, next, err := l.ListEntriesFiltered(epoch, epoch.Add(time.Hour), "", 1, "paid")
	if err != nil || next == "" || len(page) != 1 {
		t.Fatalf("first filtered page = %v next=%q err=%v, want 1 entry + cursor", idsOf(page), next, err)
	}
	page, next, err = l.ListEntriesFiltered(epoch, epoch.Add(time.Hour), next, 1, "paid")
	if err != nil || len(page) != 1 || next != "" {
		t.Fatalf("second filtered page = %v next=%q err=%v, want last entry", idsOf(page), next, err)
	}
	// The filter composes with the time window: entries outside it are
	// excluded even when their memo matches.
	mustPostLate := func() {
		e := JournalEntry{ID: "e5", DebitAccount: "a", CreditAccount: "b", AmountCents: 1, Memo: "paid late", CreatedAt: epoch.Add(48 * time.Hour)}
		if _, _, err := l.Post(e); err != nil {
			t.Fatalf("Post e5: %v", err)
		}
	}
	mustPostLate()
	page, _, err = l.ListEntriesFiltered(epoch, epoch.Add(time.Hour), "", 100, "paid")
	if err != nil {
		t.Fatalf("ListEntriesFiltered: %v", err)
	}
	if len(page) != 2 {
		t.Fatalf("time-bounded memo filter = %v, want 2 entries", idsOf(page))
	}
}

func idsOf(entries []JournalEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.ID)
	}
	return out
}

func TestTransferMemo(t *testing.T) {
	l := New()
	rcpt, err := l.PostTransfer(Transfer{ID: "tr1", From: "payer", To: "payee", AmountCents: 100, Memo: "invoice-7"})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if len(rcpt.Entries) != 1 || rcpt.Entries[0].Memo != "invoice-7" {
		t.Fatalf("principal memo = %v, want invoice-7", rcpt.Entries)
	}
	// The memo round-trips through the journal.
	got, ok := l.entries["tr1"]
	if !ok || got.Memo != "invoice-7" {
		t.Fatalf("journal memo = %q, want invoice-7", got.Memo)
	}
	// An overlong transfer memo is rejected before anything lands.
	if _, err := l.PostTransfer(Transfer{ID: "tr2", From: "payer", To: "payee", AmountCents: 1, Memo: strings.Repeat("y", 256)}); !errors.Is(err, ErrMemoTooLong) {
		t.Fatalf("overlong transfer memo: %v, want ErrMemoTooLong", err)
	}
	if _, ok := l.entries["tr2"]; ok {
		t.Fatal("rejected transfer was journaled")
	}
}

func TestTransferMemoWithFeeLeg(t *testing.T) {
	l := New(WithTransferFeePolicy(250, "fees"))
	rcpt, err := l.PostTransfer(Transfer{ID: "trf", From: "payer", To: "payee", AmountCents: 10000, Memo: "order-42"})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if len(rcpt.Entries) != 2 {
		t.Fatalf("entries = %d, want 2 (principal + fee)", len(rcpt.Entries))
	}
	if rcpt.Entries[0].Memo != "order-42" {
		t.Errorf("principal memo = %q, want order-42", rcpt.Entries[0].Memo)
	}
	// The fee leg is a system leg: it carries no memo.
	if rcpt.Entries[1].Memo != "" {
		t.Errorf("fee leg memo = %q, want empty", rcpt.Entries[1].Memo)
	}
}

func TestBatchMemo(t *testing.T) {
	l := New()
	rcpt, err := l.PostBatch(Batch{ID: "b1", Entries: []JournalEntry{
		{ID: "be1", DebitAccount: "a", CreditAccount: "b", AmountCents: 10, Memo: "payroll run"},
		{ID: "be2", DebitAccount: "a", CreditAccount: "b", AmountCents: 20},
	}})
	if err != nil {
		t.Fatalf("PostBatch: %v", err)
	}
	if rcpt.Entries[0].Memo != "payroll run" || rcpt.Entries[1].Memo != "" {
		t.Fatalf("batch memos = %q/%q, want payroll run/\"\"", rcpt.Entries[0].Memo, rcpt.Entries[1].Memo)
	}
	// One overlong memo rejects the whole batch, atomically.
	if _, err := l.PostBatch(Batch{ID: "b2", Entries: []JournalEntry{
		{ID: "be3", DebitAccount: "a", CreditAccount: "b", AmountCents: 10, Memo: strings.Repeat("z", 300)},
	}}); !errors.Is(err, ErrMemoTooLong) {
		t.Fatalf("overlong batch memo: %v, want ErrMemoTooLong", err)
	}
	if _, ok := l.entries["be3"]; ok {
		t.Fatal("rejected batch entry was journaled")
	}
}

func TestDryRunEchoesMemo(t *testing.T) {
	l := New()
	result, err := l.DryRunPost(JournalEntry{ID: "dr1", DebitAccount: "a", CreditAccount: "b", AmountCents: 50, Memo: "what-if note"})
	if err != nil {
		t.Fatalf("DryRunPost: %v", err)
	}
	if !result.WouldSucceed || len(result.Legs) != 1 {
		t.Fatalf("dry run result = %+v, want one leg", result)
	}
	if result.Legs[0].Memo != "what-if note" {
		t.Errorf("dry-run leg memo = %q, want what-if note", result.Legs[0].Memo)
	}
	// The dry run recorded nothing.
	if _, ok := l.entries["dr1"]; ok {
		t.Fatal("dry run journaled the entry")
	}
}

func TestReconcileKeepsMemoRetrievable(t *testing.T) {
	l := New()
	epoch := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e := JournalEntry{ID: "m1", DebitAccount: "a", CreditAccount: "b", AmountCents: 100, Memo: "order-555", CreatedAt: epoch}
	if _, _, err := l.Post(e); err != nil {
		t.Fatalf("Post: %v", err)
	}
	report := l.Reconcile(time.Now())
	if !report.AccountingEquationOK {
		t.Fatalf("reconcile: %s", report.AccountingError)
	}
	// The memo survives the end-of-day scan and stays queryable.
	page, _, err := l.ListEntriesFiltered(time.Time{}, time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), "", 100, "order-555")
	if err != nil {
		t.Fatalf("ListEntriesFiltered: %v", err)
	}
	if len(page) != 1 || page[0].Memo != "order-555" {
		t.Fatalf("memo after reconcile = %v, want order-555", page)
	}
}

func TestMemoSnapshotRoundTrip(t *testing.T) {
	l := New()
	if _, _, err := l.Post(JournalEntry{ID: "s1", DebitAccount: "a", CreditAccount: "b", AmountCents: 100, Memo: "snap note"}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	var buf strings.Builder
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if !snapshotLedgersEqual(l, restored) {
		t.Fatal("restored ledger differs from original")
	}
	got, ok := restored.entries["s1"]
	if !ok || got.Memo != "snap note" {
		t.Fatalf("restored memo = %q, want snap note", got.Memo)
	}
	if err := restored.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain on restored: %v", err)
	}
}

func TestMemoAuditEventCarriesMemo(t *testing.T) {
	al := mustAuditLog(t)
	defer al.Close()
	l := New(WithAuditLog(al))
	now := time.Now()
	if _, _, err := l.Post(JournalEntry{ID: "a1", DebitAccount: "a", CreditAccount: "b", AmountCents: 100, Memo: "audited note", CreatedAt: now}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, corrupt, err := ReadAuditLog(al.Dir(), now.UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt = %d, want 0", corrupt)
	}
	var post *AuditEvent
	for i, ev := range events {
		if ev.Op == "post" {
			post = &events[i]
		}
	}
	if post == nil {
		t.Fatal("no post event in audit log")
	}
	if post.Details["memo"] != "audited note" {
		t.Errorf("post audit memo = %v, want audited note", post.Details["memo"])
	}
}
