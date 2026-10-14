package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCheckAttachments(t *testing.T) {
	valid := []string{
		"https://files.example.com/receipts/inv-001.pdf",
		"https://storage.example.com:8443/a/b?sig=xyz",
	}
	if err := checkAttachments(valid); err != nil {
		t.Fatalf("checkAttachments(valid): %v", err)
	}
	if err := checkAttachments(nil); err != nil {
		t.Fatalf("checkAttachments(nil): %v", err)
	}
	cases := []struct {
		name string
		atts []string
	}{
		{"too many", []string{
			"https://x.example/1", "https://x.example/2", "https://x.example/3",
			"https://x.example/4", "https://x.example/5", "https://x.example/6",
			"https://x.example/7", "https://x.example/8", "https://x.example/9",
		}},
		{"http scheme", []string{"http://x.example/receipt.pdf"}},
		{"no scheme", []string{"x.example/receipt.pdf"}},
		{"empty host", []string{"https:///receipt.pdf"}},
		{"not a URI", []string{"just a note"}},
		{"too long", []string{"https://x.example/" + strings.Repeat("a", 512)}},
		{"one bad among good", []string{"https://x.example/ok.pdf", "http://x.example/bad.pdf"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := checkAttachments(c.atts); !errors.Is(err, ErrInvalidAttachment) {
				t.Fatalf("checkAttachments: got %v, want ErrInvalidAttachment", err)
			}
		})
	}
}

func TestPostWithAttachments(t *testing.T) {
	l := New()
	atts := []string{
		"https://files.example.com/receipts/inv-001.pdf",
		"https://files.example.com/receipts/inv-002.pdf",
	}
	posted, dup, err := l.Post(JournalEntry{
		ID: "e-att-1", DebitAccount: "expenses", CreditAccount: "cash",
		AmountCents: 2500, Currency: "USD", Attachments: atts,
	})
	if err != nil || dup {
		t.Fatalf("Post: %v %v", err, dup)
	}
	if len(posted.Attachments) != 2 || posted.Attachments[0] != atts[0] {
		t.Fatalf("posted attachments = %v", posted.Attachments)
	}
	// Echoed on read.
	entries := l.Entries()
	if len(entries) != 1 || len(entries[0].Attachments) != 2 {
		t.Fatalf("Entries() attachments = %v", entries)
	}
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
}

func TestPostInvalidAttachments(t *testing.T) {
	l := New()
	cases := [][]string{
		{"http://x.example/receipt.pdf"},
		{strings.Repeat("https://x.example/", 1) + strings.Repeat("a", 600)},
		{
			"https://x.example/1", "https://x.example/2", "https://x.example/3",
			"https://x.example/4", "https://x.example/5", "https://x.example/6",
			"https://x.example/7", "https://x.example/8", "https://x.example/9",
		},
	}
	for i, atts := range cases {
		_, _, err := l.Post(JournalEntry{
			ID: "e-bad-" + string(rune('0'+i)), DebitAccount: "a", CreditAccount: "b",
			AmountCents: 100, Currency: "USD", Attachments: atts,
		})
		if !errors.Is(err, ErrInvalidAttachment) {
			t.Fatalf("case %d: got %v, want ErrInvalidAttachment", i, err)
		}
	}
	if l.Version() != 0 {
		t.Fatal("rejected postings recorded entries")
	}
}

func TestTransferAttachments(t *testing.T) {
	l := New()
	atts := []string{"https://files.example.com/receipts/pay-009.pdf"}
	receipt, err := l.PostTransfer(Transfer{
		ID: "t-att-1", From: "payer", To: "payee", AmountCents: 5000,
		Currency: "USD", FeeCents: 100, FeeAccount: "fee-revenue",
		Attachments: atts,
	})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if len(receipt.Entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(receipt.Entries))
	}
	principal := receipt.Entries[0]
	if len(principal.Attachments) != 1 || principal.Attachments[0] != atts[0] {
		t.Fatalf("principal attachments = %v", principal.Attachments)
	}
	if len(receipt.Entries[1].Attachments) != 0 {
		t.Fatalf("fee leg carries attachments: %v", receipt.Entries[1].Attachments)
	}
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
}

func TestTransferInvalidAttachments(t *testing.T) {
	l := New()
	_, err := l.PostTransfer(Transfer{
		ID: "t-att-bad", From: "payer", To: "payee", AmountCents: 5000,
		Currency: "USD", Attachments: []string{"http://x.example/r.pdf"},
	})
	if !errors.Is(err, ErrInvalidAttachment) {
		t.Fatalf("got %v, want ErrInvalidAttachment", err)
	}
	if l.Version() != 0 {
		t.Fatal("rejected transfer recorded entries")
	}
}

func TestBatchAttachments(t *testing.T) {
	l := New()
	receipt, err := l.PostBatch(Batch{
		ID: "b-att-1",
		Entries: []JournalEntry{
			{ID: "be-1", DebitAccount: "a", CreditAccount: "b", AmountCents: 100, Currency: "USD",
				Attachments: []string{"https://x.example/r1.pdf"}},
			{ID: "be-2", DebitAccount: "c", CreditAccount: "d", AmountCents: 200, Currency: "USD"},
		},
	})
	if err != nil {
		t.Fatalf("PostBatch: %v", err)
	}
	if len(receipt.Entries[0].Attachments) != 1 {
		t.Fatalf("batch entry 0 attachments = %v", receipt.Entries[0].Attachments)
	}
	if len(receipt.Entries[1].Attachments) != 0 {
		t.Fatalf("batch entry 1 attachments = %v", receipt.Entries[1].Attachments)
	}
	// An invalid attachment on any leg rejects the whole batch.
	_, err = l.PostBatch(Batch{
		ID: "b-att-2",
		Entries: []JournalEntry{
			{ID: "be-3", DebitAccount: "a", CreditAccount: "b", AmountCents: 100, Currency: "USD",
				Attachments: []string{"ftp://x.example/r.pdf"}},
		},
	})
	if !errors.Is(err, ErrInvalidAttachment) {
		t.Fatalf("got %v, want ErrInvalidAttachment", err)
	}
}

func TestReversalInheritsAttachments(t *testing.T) {
	l := New()
	atts := []string{"https://files.example.com/receipts/orig.pdf"}
	posted, _, err := l.Post(JournalEntry{
		ID: "e-rev-src", DebitAccount: "a", CreditAccount: "b",
		AmountCents: 1000, Currency: "USD", Attachments: atts,
	})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	rev, _, err := l.ReverseEntry(posted.ID, "")
	if err != nil {
		t.Fatalf("ReverseEntry: %v", err)
	}
	if len(rev.Attachments) != 1 || rev.Attachments[0] != atts[0] {
		t.Fatalf("reversal attachments = %v, want inherited", rev.Attachments)
	}
	// The reversal's attachments are a copy: mutating the returned
	// slice must not corrupt the journaled row.
	rev.Attachments[0] = "https://evil.example/x.pdf"
	stored := l.Entries()
	for _, e := range stored {
		if e.ID == rev.ID && e.Attachments[0] != atts[0] {
			t.Fatal("reversal attachments alias the journaled row")
		}
	}
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
}

func TestCaptureAttachments(t *testing.T) {
	l := New()
	// Fund the card account: holds reserve available funds.
	if _, _, err := l.Post(JournalEntry{
		ID: "e-fund-card", DebitAccount: "card", CreditAccount: "equity",
		AmountCents: 100000, Currency: "USD",
	}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	held, _, err := l.Hold(Hold{
		ID: "h-att-1", Account: "card", AmountCents: 10000,
		Currency: "USD", ExpiresAt: time.Now().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	atts := []string{"https://files.example.com/auth/auth-1.pdf"}
	receipt, err := l.Capture(Capture{
		ID: "c-att-1", HoldID: held.ID, To: "merchant",
		AmountCents: 8000, Attachments: atts,
	})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if len(receipt.Entry.Attachments) != 1 || receipt.Entry.Attachments[0] != atts[0] {
		t.Fatalf("capture entry attachments = %v", receipt.Entry.Attachments)
	}
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	// Invalid attachments reject the capture before anything books.
	_, err = l.Capture(Capture{
		ID: "c-att-2", HoldID: held.ID, To: "merchant",
		AmountCents: 100, Attachments: []string{"http://x.example/r.pdf"},
	})
	if !errors.Is(err, ErrInvalidAttachment) {
		t.Fatalf("got %v, want ErrInvalidAttachment", err)
	}
}

func TestAttachmentIdempotentReplay(t *testing.T) {
	l := New()
	atts := []string{"https://files.example.com/receipts/replay.pdf"}
	first, dup, err := l.Post(JournalEntry{
		ID: "e-replay-att", DebitAccount: "a", CreditAccount: "b",
		AmountCents: 100, Currency: "USD", IdempotencyKey: "k-att",
		Attachments: atts,
	})
	if err != nil || dup {
		t.Fatalf("Post: %v %v", err, dup)
	}
	second, dup, err := l.Post(JournalEntry{
		ID: "e-replay-att-2", DebitAccount: "a", CreditAccount: "b",
		AmountCents: 100, Currency: "USD", IdempotencyKey: "k-att",
	})
	if err != nil || !dup {
		t.Fatalf("replay Post: %v %v", err, dup)
	}
	if len(second.Attachments) != 1 || second.Attachments[0] != atts[0] {
		t.Fatalf("replay attachments = %v", second.Attachments)
	}
	if first.ID != second.ID {
		t.Fatal("replay returned a different entry")
	}
}

func TestAttachmentChainTamperDetected(t *testing.T) {
	l := New()
	if _, _, err := l.Post(JournalEntry{
		ID: "e-tamper", DebitAccount: "a", CreditAccount: "b",
		AmountCents: 100, Currency: "USD",
		Attachments: []string{"https://x.example/real.pdf"},
	}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain before tamper: %v", err)
	}
	// Rewrite a journaled attachment in place: the chain must break.
	e := l.entries["e-tamper"]
	e.Attachments = []string{"https://x.example/forged.pdf"}
	l.entries["e-tamper"] = e
	if err := l.VerifyChain(); err == nil {
		t.Fatal("VerifyChain missed a rewritten attachment")
	}
}

func TestAttachmentsSnapshotRoundTrip(t *testing.T) {
	l := New()
	atts := []string{"https://files.example.com/receipts/snap.pdf"}
	if _, _, err := l.Post(JournalEntry{
		ID: "e-snap-att", DebitAccount: "a", CreditAccount: "b",
		AmountCents: 100, Currency: "USD", IdempotencyKey: "k-snap-att",
		Attachments: atts,
	}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	var buf strings.Builder
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	l2, err := ImportSnapshot(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	restored := l2.Entries()
	if len(restored) != 1 || len(restored[0].Attachments) != 1 || restored[0].Attachments[0] != atts[0] {
		t.Fatalf("attachments did not round-trip: %+v", restored)
	}
	if err := l2.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain after restore: %v", err)
	}
	// Idempotent replay still returns the attachments after restore.
	replay, dup, err := l2.Post(JournalEntry{
		ID: "e-snap-att-2", DebitAccount: "a", CreditAccount: "b",
		AmountCents: 100, Currency: "USD", IdempotencyKey: "k-snap-att",
	})
	if err != nil || !dup {
		t.Fatalf("replay after restore: %v %v", err, dup)
	}
	if len(replay.Attachments) != 1 {
		t.Fatalf("replay attachments after restore = %v", replay.Attachments)
	}
}
