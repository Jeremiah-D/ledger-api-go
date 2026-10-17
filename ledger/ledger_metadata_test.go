package ledger

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

// The pre-metadata chain encoding must stay byte-identical for entries
// without metadata. These hashes were pinned before the metadata field
// existed (see the LG-112 design): any change to the canonical encoding
// of metadata-less entries fails this test loudly.
func TestChainHashBackwardCompatibleWithoutMetadata(t *testing.T) {
	var zero [32]byte
	e := JournalEntry{
		ID:            "pin-fixture-1",
		DebitAccount:  "acct-debit",
		CreditAccount: "acct-credit",
		AmountCents:   12345,
		Currency:      "USD",
		IdempotencyKey: "key-1",
		Memo:          "test memo",
		ExternalRef:   "ref-1",
		Attachments:   []string{"https://example.com/r.pdf"},
		CreatedAt:     time.Unix(1760000000, 0).UTC(),
	}
	h := hashChainLink(zero, e)
	want, err := hex.DecodeString("d5da92a2f174acf2b5e6a9d41604da48ec398e8f88e71bc77208e4c1b3cf3e6b")
	if err != nil {
		t.Fatal(err)
	}
	var hx [32]byte
	copy(hx[:], want)
	if h != hx {
		t.Errorf("chain hash of metadata-less entry changed: backward compatibility broken")
	}

	e2 := JournalEntry{
		ID: "pin-fixture-2", DebitAccount: "a", CreditAccount: "b",
		AmountCents: 100, Currency: "USD", CreatedAt: time.Unix(1760000000, 0).UTC(),
	}
	h2 := hashChainLink(zero, e2)
	want2, err := hex.DecodeString("50883f5d1275a7b3c360bd968af6be9b035c1a80d3c839a524dad08fe32167f2")
	if err != nil {
		t.Fatal(err)
	}
	var hx2 [32]byte
	copy(hx2[:], want2)
	if h2 != hx2 {
		t.Errorf("chain hash of minimal metadata-less entry changed: backward compatibility broken")
	}
}

func TestMetadataValidation(t *testing.T) {
	cases := []struct {
		name string
		meta map[string]string
	}{
		{"too many pairs", map[string]string{
			"k1": "v", "k2": "v", "k3": "v", "k4": "v", "k5": "v", "k6": "v",
			"k7": "v", "k8": "v", "k9": "v", "k10": "v", "k11": "v", "k12": "v",
			"k13": "v", "k14": "v", "k15": "v", "k16": "v", "k17": "v",
		}},
		{"empty key", map[string]string{"": "v"}},
		{"key with space", map[string]string{"bad key": "v"}},
		{"key with slash", map[string]string{"bad/key": "v"}},
		{"key too long", map[string]string{strings.Repeat("k", 65): "v"}},
		{"value too long", map[string]string{"k": strings.Repeat("v", 257)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkMetadata(tc.meta); !errors.Is(err, ErrInvalidMetadata) {
				t.Errorf("checkMetadata: err=%v, want ErrInvalidMetadata", err)
			}
			l := New()
			_, _, err := l.Post(JournalEntry{
				ID:            "meta-bad-" + strings.ReplaceAll(tc.name, " ", "-"),
				DebitAccount:  "a",
				CreditAccount: "b",
				AmountCents:   100,
				Currency:      "USD",
				Metadata:      tc.meta,
			})
			if !errors.Is(err, ErrInvalidMetadata) {
				t.Errorf("Post: err=%v, want ErrInvalidMetadata", err)
			}
			// The rejection records nothing.
			if l.Version() != 0 {
				t.Errorf("version = %d after rejected post, want 0", l.Version())
			}
		})
	}
	// Boundary values are accepted: exactly 16 pairs, 64-char keys,
	// 256-char values.
	edge := map[string]string{}
	for i := 0; i < 16; i++ {
		edge["k"+strings.Repeat("0", 62)+string(rune('a'+i))] = strings.Repeat("v", 256)
	}
	if err := checkMetadata(edge); err != nil {
		t.Errorf("boundary metadata: %v", err)
	}
	l := New()
	if _, _, err := l.Post(JournalEntry{
		ID: "meta-edge", DebitAccount: "a", CreditAccount: "b",
		AmountCents: 100, Currency: "USD", Metadata: edge,
	}); err != nil {
		t.Errorf("Post boundary metadata: %v", err)
	}
}

func TestMetadataChainCoverage(t *testing.T) {
	l := New()
	posted, _, err := l.Post(JournalEntry{
		ID: "meta-chain-1", DebitAccount: "a", CreditAccount: "b",
		AmountCents: 100, Currency: "USD",
		Metadata: map[string]string{"channel": "alipay", "risk_score": "12"},
	})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if posted.Metadata["channel"] != "alipay" {
		t.Errorf("posted metadata = %v, want channel=alipay", posted.Metadata)
	}
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain on intact chain: %v", err)
	}
	// Rewriting the metadata after posting breaks the chain exactly
	// like rewriting an amount.
	l.mu.Lock()
	e := l.entries["meta-chain-1"]
	e.Metadata = map[string]string{"channel": "wechat"}
	l.entries["meta-chain-1"] = e
	l.mu.Unlock()
	if err := l.VerifyChain(); err == nil {
		t.Errorf("VerifyChain after metadata rewrite: want an error, got nil")
	}
}

func TestMetadataTransferPassThrough(t *testing.T) {
	l := New()
	receipt, err := l.PostTransfer(Transfer{
		ID: "meta-xfer-1", From: "a", To: "b", AmountCents: 1000, Currency: "USD",
		FeeCents: 50, FeeAccount: "fees",
		Metadata: map[string]string{"channel": "alipay"},
	})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if len(receipt.Entries) != 2 {
		t.Fatalf("entries = %d, want 2 (principal + fee)", len(receipt.Entries))
	}
	if receipt.Entries[0].Metadata["channel"] != "alipay" {
		t.Errorf("principal metadata = %v, want channel=alipay", receipt.Entries[0].Metadata)
	}
	// The fee leg never inherits metadata.
	if len(receipt.Entries[1].Metadata) != 0 {
		t.Errorf("fee leg metadata = %v, want empty", receipt.Entries[1].Metadata)
	}
	// Transfer-level validation rejects bad metadata before anything books.
	if _, err := l.PostTransfer(Transfer{
		ID: "meta-xfer-2", From: "a", To: "b", AmountCents: 100, Currency: "USD",
		Metadata: map[string]string{"bad key": "v"},
	}); !errors.Is(err, ErrInvalidMetadata) {
		t.Errorf("PostTransfer bad metadata: err=%v, want ErrInvalidMetadata", err)
	}
}

func TestMetadataFXTransferPassThrough(t *testing.T) {
	l := New()
	if err := l.SetFXRate("USD", "EUR", 9, 10); err != nil {
		t.Fatalf("SetFXRate: %v", err)
	}
	receipt, err := l.PostTransfer(Transfer{
		ID: "meta-xfer-fx", From: "a", To: "b", AmountCents: 1000,
		Currency: "USD", ToCurrency: "EUR", SkipFee: true, FXAccount: "fx",
		Metadata: map[string]string{"channel": "alipay"},
	})
	if err != nil {
		t.Fatalf("PostTransfer FX: %v", err)
	}
	var fxLeg *JournalEntry
	for i := range receipt.Entries {
		if receipt.Entries[i].ID == "meta-xfer-fx/fx" {
			fxLeg = &receipt.Entries[i]
		}
	}
	if fxLeg == nil {
		t.Fatalf("no /fx leg in receipt: %+v", receipt.Entries)
	}
	if fxLeg.Metadata["channel"] != "alipay" {
		t.Errorf("/fx leg metadata = %v, want channel=alipay", fxLeg.Metadata)
	}
	// The settlement-currency leg carries none.
	for i := range receipt.Entries {
		if receipt.Entries[i].ID == "meta-xfer-fx" && len(receipt.Entries[i].Metadata) != 0 {
			t.Errorf("settlement leg metadata = %v, want empty", receipt.Entries[i].Metadata)
		}
	}
}

func TestMetadataCapturePassThrough(t *testing.T) {
	l := New()
	if _, _, err := l.Post(JournalEntry{
		ID: "meta-cap-fund", DebitAccount: "card", CreditAccount: "funding",
		AmountCents: 6000, Currency: "USD",
	}); err != nil {
		t.Fatalf("fund: %v", err)
	}
	hold, _, err := l.Hold(Hold{
		ID: "meta-hold-1", Account: "card", AmountCents: 6000, Currency: "USD",
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	receipt, err := l.Capture(Capture{
		ID: "meta-cap-1", HoldID: hold.ID, To: "merchant", AmountCents: 4000,
		Metadata: map[string]string{"channel": "alipay"},
	})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if receipt.Entry.Metadata["channel"] != "alipay" {
		t.Errorf("settlement metadata = %v, want channel=alipay", receipt.Entry.Metadata)
	}
	// Capture-level validation rejects bad metadata.
	if _, err := l.Capture(Capture{
		ID: "meta-cap-2", HoldID: hold.ID, To: "merchant", AmountCents: 100,
		Metadata: map[string]string{"bad key": "v"},
	}); !errors.Is(err, ErrInvalidMetadata) {
		t.Errorf("Capture bad metadata: err=%v, want ErrInvalidMetadata", err)
	}
}

func TestMetadataReversalPassThrough(t *testing.T) {
	l := New()
	if _, _, err := l.Post(JournalEntry{
		ID: "meta-rev-orig", DebitAccount: "a", CreditAccount: "b",
		AmountCents: 500, Currency: "USD",
		Metadata: map[string]string{"channel": "alipay"},
	}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	// The original's metadata is not inherited: only the caller's is.
	rev, _, err := l.ReverseEntryWithMetadata("meta-rev-orig", "", map[string]string{"reason": "fraud"})
	if err != nil {
		t.Fatalf("ReverseEntryWithMetadata: %v", err)
	}
	if rev.Metadata["reason"] != "fraud" || len(rev.Metadata) != 1 {
		t.Errorf("reversal metadata = %v, want only reason=fraud", rev.Metadata)
	}
	// Plain ReverseEntry carries none.
	if _, _, err := l.Post(JournalEntry{
		ID: "meta-rev-orig2", DebitAccount: "a", CreditAccount: "b",
		AmountCents: 100, Currency: "USD",
	}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	rev2, _, err := l.ReverseEntry("meta-rev-orig2", "")
	if err != nil {
		t.Fatalf("ReverseEntry: %v", err)
	}
	if len(rev2.Metadata) != 0 {
		t.Errorf("plain reversal metadata = %v, want empty", rev2.Metadata)
	}
	// Reversal-level validation rejects bad metadata.
	if _, _, err := l.ReverseEntryWithMetadata("meta-rev-orig2", "", map[string]string{"bad key": "v"}); !errors.Is(err, ErrInvalidMetadata) {
		t.Errorf("ReverseEntryWithMetadata bad metadata: want ErrInvalidMetadata, got %v", err)
	}
}

func TestMetadataBatchAndReplayPassThrough(t *testing.T) {
	l := New()
	receipt, err := l.PostBatch(Batch{
		ID: "meta-batch-1",
		Entries: []JournalEntry{
			{ID: "meta-b-1", DebitAccount: "a", CreditAccount: "b", AmountCents: 100, Currency: "USD",
				Metadata: map[string]string{"channel": "alipay"}},
			{ID: "meta-b-2", DebitAccount: "b", CreditAccount: "c", AmountCents: 100, Currency: "USD"},
		},
	})
	if err != nil {
		t.Fatalf("PostBatch: %v", err)
	}
	if receipt.Entries[0].Metadata["channel"] != "alipay" {
		t.Errorf("batch entry metadata = %v, want channel=alipay", receipt.Entries[0].Metadata)
	}
	// Idempotent replays return the original entry with its metadata.
	posted, dup, err := l.Post(JournalEntry{
		ID: "meta-replay-1", DebitAccount: "a", CreditAccount: "b",
		AmountCents: 100, Currency: "USD", IdempotencyKey: "meta-k-1",
		Metadata: map[string]string{"channel": "alipay"},
	})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	replayed, dup, err := l.Post(JournalEntry{
		ID: "meta-replay-2", DebitAccount: "a", CreditAccount: "b",
		AmountCents: 100, Currency: "USD", IdempotencyKey: "meta-k-1",
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !dup {
		t.Errorf("duplicate = false, want true")
	}
	if replayed.ID != posted.ID || replayed.Metadata["channel"] != "alipay" {
		t.Errorf("replay = (%s, %v), want original entry with channel=alipay", replayed.ID, replayed.Metadata)
	}
}

func TestMetadataSnapshotRoundTrip(t *testing.T) {
	l := New()
	if _, _, err := l.Post(JournalEntry{
		ID: "meta-snap-1", DebitAccount: "a", CreditAccount: "b",
		AmountCents: 100, Currency: "USD",
		Metadata: map[string]string{"channel": "alipay", "risk_score": "12"},
	}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("export: %v", err)
	}
	// Copy the snapshot bytes: the first import consumes the buffer.
	snapBytes := append([]byte(nil), buf.Bytes()...)
	restored, err := ImportSnapshot(&buf)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	e, ok := restored.entries["meta-snap-1"]
	if !ok {
		t.Fatalf("entry missing after restore")
	}
	if e.Metadata["channel"] != "alipay" || e.Metadata["risk_score"] != "12" {
		t.Errorf("restored metadata = %v, want both pairs", e.Metadata)
	}
	if err := restored.VerifyChain(); err != nil {
		t.Errorf("VerifyChain on restored: %v", err)
	}
	// Incremental snapshots carry metadata the same way.
	base := l.Version()
	if _, _, err := l.Post(JournalEntry{
		ID: "meta-snap-2", DebitAccount: "a", CreditAccount: "b",
		AmountCents: 50, Currency: "USD",
		Metadata: map[string]string{"channel": "wechat"},
	}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	var ibuf bytes.Buffer
	if err := l.ExportIncrementalSnapshot(&ibuf, base); err != nil {
		t.Fatalf("incremental export: %v", err)
	}
	replica, err := ImportSnapshot(bytes.NewReader(snapBytes))
	if err != nil {
		t.Fatalf("replica base import: %v", err)
	}
	if err := replica.ImportIncrementalSnapshot(&ibuf); err != nil {
		t.Fatalf("incremental import: %v", err)
	}
	e2, ok := replica.entries["meta-snap-2"]
	if !ok || e2.Metadata["channel"] != "wechat" {
		t.Errorf("replica metadata = %v, want channel=wechat", e2.Metadata)
	}
	if err := replica.VerifyChain(); err != nil {
		t.Errorf("VerifyChain on replica: %v", err)
	}
}

func TestMetadataFilter(t *testing.T) {
	l := New()
	post := func(id, memo string, meta map[string]string) {
		t.Helper()
		_, _, err := l.Post(JournalEntry{
			ID: id, DebitAccount: "a", CreditAccount: "b",
			AmountCents: 100, Currency: "USD", Memo: memo, Metadata: meta,
		})
		if err != nil {
			t.Fatalf("Post %s: %v", id, err)
		}
	}
	post("mf-1", "order-1", map[string]string{"channel": "alipay", "risk_score": "12"})
	post("mf-2", "order-2", map[string]string{"channel": "wechat"})
	post("mf-3", "order-3", map[string]string{"channel": "alipay"})

	page, _, err := l.ListEntriesMetadataFiltered(time.Time{}, time.Now().Add(time.Hour), "", 100, "", "", map[string]string{"channel": "alipay"})
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if len(page) != 2 {
		t.Fatalf("filtered = %d entries, want 2", len(page))
	}
	// Composes with the memo filter.
	page, _, err = l.ListEntriesMetadataFiltered(time.Time{}, time.Now().Add(time.Hour), "", 100, "order-1", "", map[string]string{"channel": "alipay"})
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if len(page) != 1 || page[0].ID != "mf-1" {
		t.Errorf("memo+meta filter = %+v, want only mf-1", page)
	}
	// Two pairs must both match.
	page, _, err = l.ListEntriesMetadataFiltered(time.Time{}, time.Now().Add(time.Hour), "", 100, "", "", map[string]string{"channel": "alipay", "risk_score": "12"})
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if len(page) != 1 || page[0].ID != "mf-1" {
		t.Errorf("two-pair filter = %+v, want only mf-1", page)
	}
	// A pair no entry carries matches nothing.
	page, _, err = l.ListEntriesMetadataFiltered(time.Time{}, time.Now().Add(time.Hour), "", 100, "", "", map[string]string{"channel": "unionpay"})
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if len(page) != 0 {
		t.Errorf("unmatched filter = %d entries, want 0", len(page))
	}
}

func TestMetadataDryRunEcho(t *testing.T) {
	l := New()
	if _, _, err := l.Post(JournalEntry{
		ID: "meta-dr-fund", DebitAccount: "a", CreditAccount: "funding",
		AmountCents: 10000, Currency: "USD",
	}); err != nil {
		t.Fatalf("fund: %v", err)
	}
	result, err := l.DryRunTransfer(Transfer{
		ID: "meta-dr-1", From: "a", To: "b", AmountCents: 1000, Currency: "USD",
		SkipFee: true, Metadata: map[string]string{"channel": "alipay"},
	})
	if err != nil {
		t.Fatalf("DryRunTransfer: %v", err)
	}
	if !result.WouldSucceed {
		t.Fatalf("WouldSucceed = false")
	}
	if len(result.Legs) == 0 {
		t.Fatalf("no legs in dry-run result")
	}
	if result.Legs[0].Metadata["channel"] != "alipay" {
		t.Errorf("dry-run leg metadata = %v, want channel=alipay", result.Legs[0].Metadata)
	}
}
