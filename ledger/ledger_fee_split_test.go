package ledger

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
	"time"
)

// splitTransfer builds a transfer with an explicit fee and two
// recipients, the canonical happy-path shape for these tests.
func splitTransfer(id string) Transfer {
	return Transfer{
		ID: "tx-split-" + id, From: "alice", To: "bob",
		AmountCents: 10000, FeeCents: 101,
		FeeSplits: []FeeSplit{{Account: "rev-a", Bps: 5000}, {Account: "rev-b", Bps: 5000}},
	}
}

func TestFeeSplitValidation(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Transfer)
		wantErr error
	}{
		{"bps sum under 10000", func(tr *Transfer) {
			tr.FeeSplits = []FeeSplit{{Account: "a", Bps: 5000}, {Account: "b", Bps: 4000}}
		}, ErrFeeSplitSum},
		{"bps sum over 10000", func(tr *Transfer) {
			tr.FeeSplits = []FeeSplit{{Account: "a", Bps: 6000}, {Account: "b", Bps: 5000}}
		}, ErrFeeSplitSum},
		{"empty account", func(tr *Transfer) {
			tr.FeeSplits = []FeeSplit{{Account: "", Bps: 10000}}
		}, ErrInvalidFeeSplit},
		{"zero bps", func(tr *Transfer) {
			tr.FeeSplits = []FeeSplit{{Account: "a", Bps: 0}, {Account: "b", Bps: 10000}}
		}, ErrInvalidFeeSplit},
		{"negative bps", func(tr *Transfer) {
			tr.FeeSplits = []FeeSplit{{Account: "a", Bps: -1}, {Account: "b", Bps: 10001}}
		}, ErrInvalidFeeSplit},
		{"bps over 10000", func(tr *Transfer) {
			tr.FeeSplits = []FeeSplit{{Account: "a", Bps: 10001}}
		}, ErrInvalidFeeSplit},
		{"fee account conflict", func(tr *Transfer) {
			tr.FeeAccount = "fees"
		}, ErrFeeSplitFeeAccountConflict},
		{"skip fee conflict", func(tr *Transfer) {
			tr.SkipFee = true
		}, ErrFeeSplitSkipFeeConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := New()
			seedBalance(t, l, "alice", 1_000_000)
			tr := splitTransfer(tc.name)
			tc.mutate(&tr)
			nBefore := len(l.Entries())
			if _, err := l.PostTransfer(tr); !errors.Is(err, tc.wantErr) {
				t.Fatalf("PostTransfer = %v, want %v", err, tc.wantErr)
			}
			// Rejections record nothing.
			if n := len(l.Entries()); n != nBefore {
				t.Fatalf("journal entries = %d, want %d (rejection recorded nothing)", n, nBefore)
			}
		})
	}
}

func TestFeeSplitExplicitFeeEndToEnd(t *testing.T) {
	l := New()
	seedBalance(t, l, "alice", 1_000_000)

	receipt, err := l.PostTransfer(splitTransfer("explicit"))
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if receipt.FeeCents != 101 {
		t.Errorf("receipt.FeeCents = %d, want 101", receipt.FeeCents)
	}
	if len(receipt.Entries) != 3 {
		t.Fatalf("receipt.Entries has %d entries, want 3 (principal + 2 split legs)", len(receipt.Entries))
	}
	// Dust: floor(101*5000/10000) = 50 each, remainder 1 lands on the
	// first recipient.
	wantLegs := []struct {
		id, memo string
		account  AccountID
		amount   int64
	}{
		{"tx-split-explicit/fee/0", "fee-split:0/2:rev-a:5000", "rev-a", 51},
		{"tx-split-explicit/fee/1", "fee-split:1/2:rev-b:5000", "rev-b", 50},
	}
	for i, w := range wantLegs {
		leg := receipt.Entries[i+1]
		if leg.ID != w.id || leg.DebitAccount != w.account || leg.CreditAccount != "alice" || leg.AmountCents != w.amount {
			t.Errorf("leg %d = %+v, want id %s debit %s credit alice amount %d", i, leg, w.id, w.account, w.amount)
		}
		if leg.Memo != w.memo {
			t.Errorf("leg %d memo = %q, want %q", i, leg.Memo, w.memo)
		}
		if leg.Currency != "USD" {
			t.Errorf("leg %d currency = %q, want USD (fee stays in source currency)", i, leg.Currency)
		}
	}
	// The receipt discloses the per-recipient breakdown.
	wantSplits := []FeeSplitShare{
		{Account: "rev-a", Bps: 5000, FeeCents: 51},
		{Account: "rev-b", Bps: 5000, FeeCents: 50},
	}
	if !reflect.DeepEqual(receipt.FeeSplits, wantSplits) {
		t.Errorf("receipt.FeeSplits = %+v, want %+v", receipt.FeeSplits, wantSplits)
	}
	// Balances: the payer funds amount + fee, the payee gets the full
	// amount, each recipient its share.
	if got := l.Balance("alice"); got != 1_000_000-10101 {
		t.Errorf("alice balance = %d, want %d", got, 1_000_000-10101)
	}
	if got := l.Balance("bob"); got != 10000 {
		t.Errorf("bob balance = %d, want 10000", got)
	}
	if got := l.Balance("rev-a"); got != 51 {
		t.Errorf("rev-a balance = %d, want 51", got)
	}
	if got := l.Balance("rev-b"); got != 50 {
		t.Errorf("rev-b balance = %d, want 50", got)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Errorf("VerifyAccountingEquation: %v", err)
	}
}

func TestFeeSplitDustConservation(t *testing.T) {
	// Unit-level: shares always sum to the fee exactly, including the
	// degenerate case where every floor share is zero.
	cases := []struct {
		fee    int64
		splits []FeeSplit
	}{
		{101, []FeeSplit{{Account: "a", Bps: 5000}, {Account: "b", Bps: 5000}}},
		{1, []FeeSplit{{Account: "a", Bps: 3333}, {Account: "b", Bps: 3333}, {Account: "c", Bps: 3334}}},
		{999999, []FeeSplit{{Account: "a", Bps: 1}, {Account: "b", Bps: 9999}}},
		{7, []FeeSplit{{Account: "a", Bps: 10000}}},
	}
	for _, tc := range cases {
		shares := computeFeeSplitShares(tc.fee, tc.splits)
		var sum int64
		for _, s := range shares {
			if s.FeeCents < 0 {
				t.Fatalf("fee %d: negative share %+v", tc.fee, s)
			}
			sum += s.FeeCents
		}
		if sum != tc.fee {
			t.Errorf("fee %d: shares sum to %d, want exact conservation", tc.fee, sum)
		}
	}
	// The 1-cent / 3-recipient case: all floors are zero, the whole
	// cent lands on the first recipient.
	shares := computeFeeSplitShares(1, []FeeSplit{{Account: "a", Bps: 3333}, {Account: "b", Bps: 3333}, {Account: "c", Bps: 3334}})
	if shares[0].FeeCents != 1 || shares[1].FeeCents != 0 || shares[2].FeeCents != 0 {
		t.Errorf("dust shares = %+v, want [1 0 0]", shares)
	}
}

func TestFeeSplitPolicyFeeEndToEnd(t *testing.T) {
	l := tieredLedger()
	seedBalance(t, l, "alice", 200_000_000)

	// 10000 cents falls in the 2.5% tier: fee = floor(10000*250/10000) = 250.
	receipt, err := l.PostTransfer(Transfer{
		ID: "tx-split-policy", From: "alice", To: "bob", AmountCents: 10000,
		FeeSplits: []FeeSplit{{Account: "rev-a", Bps: 7000}, {Account: "rev-b", Bps: 3000}},
	})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if receipt.FeeCents != 250 {
		t.Errorf("receipt.FeeCents = %d, want 250", receipt.FeeCents)
	}
	if receipt.FeeTierIndex != 1 || receipt.FeeRateBps != 250 {
		t.Errorf("tier disclosure = (%d, %d), want (1, 250)", receipt.FeeTierIndex, receipt.FeeRateBps)
	}
	// floor(250*7000/10000) = 175, floor(250*3000/10000) = 75, no dust.
	wantSplits := []FeeSplitShare{
		{Account: "rev-a", Bps: 7000, FeeCents: 175},
		{Account: "rev-b", Bps: 3000, FeeCents: 75},
	}
	if !reflect.DeepEqual(receipt.FeeSplits, wantSplits) {
		t.Errorf("receipt.FeeSplits = %+v, want %+v", receipt.FeeSplits, wantSplits)
	}
	if got := l.Balance("rev-a"); got != 175 {
		t.Errorf("rev-a balance = %d, want 175", got)
	}
	if got := l.Balance("rev-b"); got != 75 {
		t.Errorf("rev-b balance = %d, want 75", got)
	}
	if got := l.Balance("alice"); got != 200_000_000-10250 {
		t.Errorf("alice balance = %d, want %d", got, 200_000_000-10250)
	}
}

func TestFeeSplitFrozenRecipient(t *testing.T) {
	l := New()
	seedBalance(t, l, "alice", 1_000_000)
	l.Freeze("rev-b")

	nBefore := len(l.Entries())
	if _, err := l.PostTransfer(splitTransfer("frozen")); !errors.Is(err, ErrAccountFrozen) {
		t.Fatalf("PostTransfer with frozen recipient = %v, want ErrAccountFrozen", err)
	}
	if n := len(l.Entries()); n != nBefore {
		t.Fatalf("journal entries = %d, want %d (rejection recorded nothing)", n, nBefore)
	}
}

func TestFeeSplitZeroFeeRejected(t *testing.T) {
	// No policy, no explicit fee: a split of nothing is rejected, not
	// silently dropped.
	l := New()
	seedBalance(t, l, "alice", 1_000_000)
	tr := splitTransfer("zerofee")
	tr.FeeCents = 0
	if _, err := l.PostTransfer(tr); !errors.Is(err, ErrFeeSplitZeroFee) {
		t.Fatalf("PostTransfer zero fee = %v, want ErrFeeSplitZeroFee", err)
	}

	// A fee-free dust tier also resolves to zero: rejected too.
	l2 := tieredLedger()
	seedBalance(t, l2, "alice", 1_000_000)
	if _, err := l2.PostTransfer(Transfer{
		ID: "tx-split-dust", From: "alice", To: "bob", AmountCents: 100,
		FeeSplits: []FeeSplit{{Account: "rev-a", Bps: 10000}},
	}); !errors.Is(err, ErrFeeSplitZeroFee) {
		t.Fatalf("PostTransfer dust-tier zero fee = %v, want ErrFeeSplitZeroFee", err)
	}
}

func TestFeeSplitIdempotentReplay(t *testing.T) {
	l := New()
	seedBalance(t, l, "alice", 1_000_000)

	tr := splitTransfer("replay")
	tr.IdempotencyKey = "split-key-1"
	orig, err := l.PostTransfer(tr)
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	nAfterPost := len(l.Entries())

	// Replay under a different transfer ID: the original receipt comes
	// back, nothing books again.
	retry := tr
	retry.ID = "tx-split-replay-retry"
	dup, err := l.PostTransfer(retry)
	if err != nil {
		t.Fatalf("replay PostTransfer: %v", err)
	}
	if !dup.Duplicate {
		t.Error("replay duplicate = false, want true")
	}
	if dup.FeeCents != orig.FeeCents {
		t.Errorf("replay FeeCents = %d, want %d", dup.FeeCents, orig.FeeCents)
	}
	if !reflect.DeepEqual(dup.FeeSplits, orig.FeeSplits) {
		t.Errorf("replay FeeSplits = %+v, want %+v", dup.FeeSplits, orig.FeeSplits)
	}
	if !reflect.DeepEqual(dup.Entries, orig.Entries) {
		t.Error("replay entries differ from the original receipt's entries")
	}
	if n := len(l.Entries()); n != nAfterPost {
		t.Errorf("journal entries = %d, want %d (replay booked nothing new)", n, nAfterPost)
	}
	if got := l.Balance("rev-a"); got != 51 {
		t.Errorf("rev-a balance after replay = %d, want 51 (no double booking)", got)
	}
}

func TestFeeSplitPolicyReplayRedisclosesTier(t *testing.T) {
	l := tieredLedger()
	seedBalance(t, l, "alice", 200_000_000)

	tr := Transfer{
		ID: "tx-split-tier-replay", From: "alice", To: "bob", AmountCents: 10000,
		FeeSplits:      []FeeSplit{{Account: "rev-a", Bps: 7000}, {Account: "rev-b", Bps: 3000}},
		IdempotencyKey: "split-tier-key",
	}
	orig, err := l.PostTransfer(tr)
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	retry := tr
	retry.ID = "tx-split-tier-replay-retry"
	dup, err := l.PostTransfer(retry)
	if err != nil {
		t.Fatalf("replay PostTransfer: %v", err)
	}
	if dup.FeeTierIndex != orig.FeeTierIndex || dup.FeeRateBps != orig.FeeRateBps {
		t.Errorf("replay tier = (%d, %d), want (%d, %d) like the original",
			dup.FeeTierIndex, dup.FeeRateBps, orig.FeeTierIndex, orig.FeeRateBps)
	}
}

func TestTransferFeeSplitQuery(t *testing.T) {
	l := New()
	seedBalance(t, l, "alice", 1_000_000)
	if _, err := l.PostTransfer(splitTransfer("query")); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	// A plain single-fee-account transfer: no split to disclose.
	if _, err := l.PostTransfer(Transfer{
		ID: "tx-single-fee", From: "alice", To: "bob",
		AmountCents: 1000, FeeCents: 25, FeeAccount: "fees",
	}); err != nil {
		t.Fatalf("PostTransfer single fee: %v", err)
	}
	// A fee-free transfer.
	if _, err := l.PostTransfer(Transfer{
		ID: "tx-no-fee", From: "alice", To: "bob", AmountCents: 1000, SkipFee: true,
	}); err != nil {
		t.Fatalf("PostTransfer no fee: %v", err)
	}

	detail, err := l.TransferFeeSplit("tx-split-query")
	if err != nil {
		t.Fatalf("TransferFeeSplit: %v", err)
	}
	if detail.TransferID != "tx-split-query" || detail.FeeCents != 101 {
		t.Errorf("detail = %+v, want transfer tx-split-query fee 101", detail)
	}
	wantSplits := []FeeSplitShare{
		{Account: "rev-a", Bps: 5000, FeeCents: 51},
		{Account: "rev-b", Bps: 5000, FeeCents: 50},
	}
	if !reflect.DeepEqual(detail.Splits, wantSplits) {
		t.Errorf("detail.Splits = %+v, want %+v", detail.Splits, wantSplits)
	}

	if _, err := l.TransferFeeSplit("tx-missing"); !errors.Is(err, ErrTransferNotFound) {
		t.Errorf("TransferFeeSplit unknown = %v, want ErrTransferNotFound", err)
	}
	if _, err := l.TransferFeeSplit("tx-single-fee"); !errors.Is(err, ErrNoFeeSplit) {
		t.Errorf("TransferFeeSplit single fee = %v, want ErrNoFeeSplit", err)
	}
	if _, err := l.TransferFeeSplit("tx-no-fee"); !errors.Is(err, ErrNoFeeSplit) {
		t.Errorf("TransferFeeSplit no fee = %v, want ErrNoFeeSplit", err)
	}
}

func TestFeeSplitAuditDetails(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	defer al.Close()
	l := New(WithAuditLog(al))
	seedBalance(t, l, "alice", 1_000_000)
	if _, err := l.PostTransfer(splitTransfer("audit")); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, corrupt, err := ReadAuditLog(dir, time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt = %d, want 0", corrupt)
	}
	var found bool
	for _, ev := range events {
		if ev.Op != "transfer" || ev.TraceID != "tx-split-audit" {
			continue
		}
		found = true
		raw, ok := ev.Details["fee_splits"]
		if !ok {
			t.Fatal("transfer audit event has no fee_splits details")
		}
		rows, ok := raw.([]any)
		if !ok || len(rows) != 2 {
			t.Fatalf("fee_splits = %v, want 2 rows", raw)
		}
		first, ok := rows[0].(map[string]any)
		if !ok {
			t.Fatalf("fee_splits[0] = %v, want a detail map", rows[0])
		}
		if first["account"] != "rev-a" || first["bps"] != float64(5000) || first["fee_cents"] != float64(51) {
			t.Errorf("fee_splits[0] = %v, want account rev-a bps 5000 fee_cents 51", first)
		}
	}
	if !found {
		t.Fatal("no transfer audit event for tx-split-audit")
	}
}

func TestFeeSplitSnapshotRoundTrip(t *testing.T) {
	l := New()
	seedBalance(t, l, "alice", 1_000_000)
	tr := splitTransfer("snap")
	tr.IdempotencyKey = "split-snap-key"
	orig, err := l.PostTransfer(tr)
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}

	// Transfer receipts are not snapshot records (only the journal,
	// the idempotency index, and review records round-trip), so the
	// breakdown must rebuild from the journaled legs after a restore.
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(&buf)
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	detail, err := restored.TransferFeeSplit("tx-split-snap")
	if err != nil {
		t.Fatalf("TransferFeeSplit after restore: %v", err)
	}
	if !reflect.DeepEqual(detail.Splits, orig.FeeSplits) {
		t.Errorf("restored splits = %+v, want %+v", detail.Splits, orig.FeeSplits)
	}
	// The idempotency index round-trips too: a replay on the restored
	// ledger returns the original receipt and books nothing new.
	retry := tr
	retry.ID = "tx-split-snap-retry"
	dup, err := restored.PostTransfer(retry)
	if err != nil {
		t.Fatalf("replay on restored ledger: %v", err)
	}
	if !dup.Duplicate || !reflect.DeepEqual(dup.FeeSplits, orig.FeeSplits) {
		t.Errorf("restored replay = duplicate=%v splits=%+v, want duplicate=true splits=%+v",
			dup.Duplicate, dup.FeeSplits, orig.FeeSplits)
	}
}

func TestFeeSplitBackwardCompat(t *testing.T) {
	l := New()
	seedBalance(t, l, "alice", 10000)
	receipt, err := l.PostTransfer(Transfer{
		ID: "tx-compat", From: "alice", To: "bob",
		AmountCents: 1000, FeeCents: 25, FeeAccount: "fees",
	})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if len(receipt.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(receipt.Entries))
	}
	fee := receipt.Entries[1]
	// Byte-identical to the pre-split fee leg: "<id>/fee", no memo.
	if fee.ID != "tx-compat/fee" || fee.Memo != "" {
		t.Errorf("fee leg = %+v, want id tx-compat/fee with no memo", fee)
	}
	if receipt.FeeSplits != nil {
		t.Errorf("receipt.FeeSplits = %+v, want nil for a single fee account", receipt.FeeSplits)
	}
	if got := l.Balance("fees"); got != 25 {
		t.Errorf("fees balance = %d, want 25", got)
	}
}

func TestFeeSplitUnsupportedOnSplitTransfer(t *testing.T) {
	l := New(WithMaxTransferChunkCents(1000))
	seedBalance(t, l, "alice", 1_000_000)
	// The amount exceeds the chunk cap, so this would chunk — but fee
	// splits fail fast instead of misbooking.
	tr := splitTransfer("chunked")
	tr.AmountCents = 5000
	if _, err := l.PostTransferSplit(tr); !errors.Is(err, ErrFeeSplitUnsupported) {
		t.Fatalf("PostTransferSplit with FeeSplits = %v, want ErrFeeSplitUnsupported", err)
	}
}

// TestFeeLegIDAnchoring pins the transfer-ID-anchored fee-leg matcher: a
// user-chosen transfer ID that merely ends in "/fee/<digits>" must not be
// misread as a fee leg on replay (the old fuzzy suffix match did).
func TestFeeLegIDAnchoring(t *testing.T) {
	cases := []struct {
		transferID, id string
		want           bool
	}{
		{"tx-1", "tx-1/fee", true},
		{"tx-1", "tx-1/fee/0", true},
		{"tx-1", "tx-1/fee/12", true},
		{"tx-1", "tx-1/fee/abc", false},
		{"tx-1", "tx-1/fee/", false},
		{"tx-1", "tx-1", false},
		{"tx-1", "other/fee", false},
		// The regression shape: the principal of a transfer whose ID ends
		// in "/fee/<digits>" is not itself a fee leg.
		{"invoice/fee/3", "invoice/fee/3", false},
		{"invoice/fee/3", "invoice/fee/3/fee", true},
		{"invoice/fee/3", "invoice/fee/3/fee/0", true},
		// A transfer ID ending in bare "/fee" keeps working too.
		{"invoice/fee", "invoice/fee", false},
		{"invoice/fee", "invoice/fee/fee", true},
	}
	for _, tc := range cases {
		if got := isFeeLegID(tc.transferID, tc.id); got != tc.want {
			t.Errorf("isFeeLegID(%q, %q) = %v, want %v", tc.transferID, tc.id, got, tc.want)
		}
	}
}

// TestFeeReplayAnchoredTransferID is the end-to-end regression for the
// matcher above: replaying a transfer whose ID ends in "/fee/<digits>"
// must disclose only the real fee legs, not the principal.
func TestFeeReplayAnchoredTransferID(t *testing.T) {
	l := New()
	seedBalance(t, l, "alice", 1_000_000)

	// Single fee account path.
	single := Transfer{
		ID: "weird/fee/7", From: "alice", To: "bob",
		AmountCents: 10000, FeeCents: 101, FeeAccount: "fees",
		IdempotencyKey: "anchored-single",
	}
	orig, err := l.PostTransfer(single)
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if orig.FeeCents != 101 {
		t.Fatalf("orig FeeCents = %d, want 101", orig.FeeCents)
	}
	retry := single
	retry.ID = "weird/fee/7-retry"
	dup, err := l.PostTransfer(retry)
	if err != nil {
		t.Fatalf("replay PostTransfer: %v", err)
	}
	if dup.FeeCents != 101 {
		t.Errorf("replay FeeCents = %d, want 101 (principal misread as fee leg)", dup.FeeCents)
	}

	// Fee-split path on the same adversarial ID shape.
	split := Transfer{
		ID: "weird/fee/9", From: "alice", To: "bob",
		AmountCents: 10000, FeeCents: 101,
		FeeSplits:      []FeeSplit{{Account: "rev-a", Bps: 5000}, {Account: "rev-b", Bps: 5000}},
		IdempotencyKey: "anchored-split",
	}
	sorig, err := l.PostTransfer(split)
	if err != nil {
		t.Fatalf("PostTransfer split: %v", err)
	}
	sretry := split
	sretry.ID = "weird/fee/9-retry"
	sdup, err := l.PostTransfer(sretry)
	if err != nil {
		t.Fatalf("replay PostTransfer split: %v", err)
	}
	if sdup.FeeCents != sorig.FeeCents {
		t.Errorf("replay split FeeCents = %d, want %d", sdup.FeeCents, sorig.FeeCents)
	}
	if !reflect.DeepEqual(sdup.FeeSplits, sorig.FeeSplits) {
		t.Errorf("replay split FeeSplits = %+v, want %+v", sdup.FeeSplits, sorig.FeeSplits)
	}
}
