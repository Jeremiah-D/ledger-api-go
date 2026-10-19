package ledger

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func postPIITestEntry(t *testing.T, l *Ledger, id, debit, credit string, memo string) JournalEntry {
	t.Helper()
	e, dup, err := l.Post(JournalEntry{
		ID: id, DebitAccount: AccountID(debit), CreditAccount: AccountID(credit),
		AmountCents: 100, Currency: "USD", IdempotencyKey: "pii-" + id, Memo: memo,
	})
	if err != nil || dup {
		t.Fatalf("Post: dup=%v err=%v", dup, err)
	}
	return e
}

func TestErasePIIRejectsNonErasableFields(t *testing.T) {
	l := New()
	for _, f := range []string{"amount_cents", "debit_account", "credit_account", "currency", "idempotency_key", ""} {
		_, err := l.ErasePII("cash", []string{f}, "alice")
		if !errors.Is(err, ErrPIIFieldNotErasable) {
			t.Fatalf("field %q: got %v, want ErrPIIFieldNotErasable", f, err)
		}
	}
	if _, err := l.ErasePII("cash", nil, "alice"); !errors.Is(err, ErrPIIFieldNotErasable) {
		t.Fatalf("empty fields: got %v", err)
	}
	if _, err := l.ErasePII("cash", []string{PIIFieldMemo}, ""); !errors.Is(err, ErrErasureInvalidReviewer) {
		t.Fatalf("empty requester: got %v", err)
	}
}

func TestErasePIIHolderNameFlow(t *testing.T) {
	l := New()
	if err := l.SetAccountHolderName("cash", "Ada Lovelace"); err != nil {
		t.Fatal(err)
	}
	if got, ok := l.AccountHolderName("cash"); !ok || got != "Ada Lovelace" {
		t.Fatalf("holder name = %q, %v", got, ok)
	}
	if err := l.SetAccountHolderName("cash", ""); !errors.Is(err, ErrInvalidHolderName) {
		t.Fatalf("empty name: got %v", err)
	}

	req, err := l.ErasePII("cash", []string{PIIFieldHolderName}, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if req.Status != ErasureStatusPending {
		t.Fatalf("status = %q", req.Status)
	}
	// Nothing erased yet: filing is not executing.
	if _, ok := l.AccountHolderName("cash"); !ok {
		t.Fatal("filing must not erase")
	}
	// Dual control: the requester cannot approve their own request.
	if _, err := l.ApproveErasure(req.ID, "alice"); !errors.Is(err, ErrErasureSelfApproval) {
		t.Fatalf("self-approval: got %v, want ErrErasureSelfApproval", err)
	}
	done, err := l.ApproveErasure(req.ID, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != ErasureStatusApproved || done.Reviewer != "bob" {
		t.Fatalf("request = %+v", done)
	}
	if _, ok := l.AccountHolderName("cash"); ok {
		t.Fatal("holder name should be erased")
	}
	recs := l.ErasureRecords("cash")
	if len(recs) != 1 || recs[0].Field != PIIFieldHolderName {
		t.Fatalf("tombstones = %+v", recs)
	}
	if v := l.ErasureVersion(); v != 1 {
		t.Fatalf("erasure version = %d, want 1", v)
	}
	// Terminal: no double-approve, no reject-after-approve.
	if _, err := l.ApproveErasure(req.ID, "carol"); !errors.Is(err, ErrErasureNotPending) {
		t.Fatalf("re-approve: got %v", err)
	}
}

func TestErasePIIMemoRedactionKeepsChain(t *testing.T) {
	l := New()
	e1 := postPIITestEntry(t, l, "e1", "cash", "rev", "invoice ACME-42 for Ada")
	postPIITestEntry(t, l, "e2", "cash", "rev", "")
	headBefore, _ := l.ChainHead()

	req, err := l.ErasePII("cash", []string{PIIFieldMemo}, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.ApproveErasure(req.ID, "bob"); err != nil {
		t.Fatal(err)
	}
	// The sealed rows are untouched: the chain still verifies, with the
	// same head.
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain after memo erasure: %v", err)
	}
	if got, _ := l.ChainHead(); got != headBefore {
		t.Fatal("chain head moved under a memo erasure")
	}
	// Read paths redact.
	if got := l.RedactEntryMemo(e1); got.Memo != ErasedMemoTombstone {
		t.Fatalf("redacted memo = %q", got.Memo)
	}
	if got := l.RedactEntryMemo(e1); got.ID != "e1" {
		t.Fatalf("redaction must keep the entry identity: %+v", got)
	}
	// Empty memos are not fabricated.
	l.mu.RLock()
	e2 := l.entries["e2"]
	stored := l.entries["e1"]
	l.mu.RUnlock()
	if got := l.RedactEntryMemo(e2); got.Memo != "" {
		t.Fatalf("empty memo redacted to %q", got.Memo)
	}
	// The stored row still carries the original memo (chain input).
	if stored.Memo != "invoice ACME-42 for Ada" {
		t.Fatalf("sealed row was rewritten: %q", stored.Memo)
	}
	if n := l.ErasureCount(); n != 2 {
		t.Fatalf("erasure count = %d, want 2 (one per entry)", n)
	}
}

func TestErasePIIReject(t *testing.T) {
	l := New()
	if err := l.SetAccountHolderName("cash", "Grace Hopper"); err != nil {
		t.Fatal(err)
	}
	req, err := l.ErasePII("cash", []string{PIIFieldHolderName}, "alice")
	if err != nil {
		t.Fatal(err)
	}
	done, err := l.RejectErasure(req.ID, "bob", "customer dispute open")
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != ErasureStatusRejected {
		t.Fatalf("status = %q", done.Status)
	}
	if _, ok := l.AccountHolderName("cash"); !ok {
		t.Fatal("rejected erasure must not erase")
	}
	if _, err := l.RejectErasure("nope", "bob", "x"); !errors.Is(err, ErrErasureNotFound) {
		t.Fatalf("unknown request: got %v", err)
	}
}

func TestErasePIIFieldDedupAndSort(t *testing.T) {
	l := New()
	req, err := l.ErasePII("cash", []string{PIIFieldMemo, PIIFieldHolderName, PIIFieldMemo}, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Fields) != 2 || req.Fields[0] != PIIFieldHolderName || req.Fields[1] != PIIFieldMemo {
		t.Fatalf("fields = %v", req.Fields)
	}
}

func TestPIISnapshotRoundTrip(t *testing.T) {
	src := New()
	if err := src.SetAccountHolderName("cash", "Alan Turing"); err != nil {
		t.Fatal(err)
	}
	postPIITestEntry(t, src, "e1", "cash", "rev", "secret memo")
	req, err := src.ErasePII("cash", []string{PIIFieldMemo}, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.ApproveErasure(req.ID, "bob"); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := src.ExportSnapshot(&buf); err != nil {
		t.Fatal(err)
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	// Unerased holder name survives; erasure marks survive; version survives.
	if got, ok := restored.AccountHolderName("cash"); !ok || got != "Alan Turing" {
		t.Fatalf("holder name after restore = %q, %v", got, ok)
	}
	if !restored.ErasedMemo("e1") {
		t.Fatal("memo erasure mark lost in restore")
	}
	if v := restored.ErasureVersion(); v != 1 {
		t.Fatalf("erasure version after restore = %d", v)
	}
	if err := restored.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain after restore: %v", err)
	}
	// Tampered erasure rows fail the import.
	corrupt := strings.Replace(buf.String(), `"field":"memo"`, `"field":"amount_cents"`, 1)
	if _, err := ImportSnapshot(strings.NewReader(corrupt)); err == nil {
		t.Fatal("import with non-erasable field should fail")
	}
}

func TestErasePIIErasureVersionMonotonic(t *testing.T) {
	l := New()
	for i, acct := range []string{"a", "b"} {
		if err := l.SetAccountHolderName(AccountID(acct), "Name"); err != nil {
			t.Fatal(err)
		}
		req, err := l.ErasePII(AccountID(acct), []string{PIIFieldHolderName}, "alice")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := l.ApproveErasure(req.ID, "bob"); err != nil {
			t.Fatal(err)
		}
		if v := l.ErasureVersion(); v != uint64(i+1) {
			t.Fatalf("version = %d, want %d", v, i+1)
		}
	}
	_ = time.Now
}

func TestExportManifestAnnotatesErasure(t *testing.T) {
	al := mustChainAuditLog(t)
	logTestEvents(t, al, 2)
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	out := t.TempDir() + "/export"
	m, err := ExportAuditChain(al.Dir(), AuditChainExportOptions{
		ExportDir:      out,
		ErasureVersion: 3,
		ErasureCount:   7,
	})
	if err != nil {
		t.Fatalf("ExportAuditChain: %v", err)
	}
	if m.ErasureVersion != 3 || m.ErasureCount != 7 {
		t.Fatalf("manifest erasure = (%d, %d), want (3, 7)", m.ErasureVersion, m.ErasureCount)
	}
	// Zero values are omitted: exports without erasures look as before.
	out2 := t.TempDir() + "/export2"
	m2, err := ExportAuditChain(al.Dir(), AuditChainExportOptions{ExportDir: out2})
	if err != nil {
		t.Fatalf("ExportAuditChain: %v", err)
	}
	if m2.ErasureVersion != 0 || m2.ErasureCount != 0 {
		t.Fatalf("zero erasure should stay zero: %+v", m2)
	}
	raw, err := json.Marshal(m2)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "erasure_") {
		t.Fatalf("zero erasure fields should be omitted: %s", raw)
	}
}
