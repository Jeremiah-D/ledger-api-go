package ledger

import (
	"errors"
	"math"
	"testing"
	"time"
)

func TestPostTransferExplicitFee(t *testing.T) {
	l := New()
	seedBalance(t, l, "alice", 10000)

	nBefore := len(l.Entries())
	_, vBefore := l.Snapshot("alice")

	receipt, err := l.PostTransfer(Transfer{
		ID: "tx-fee", From: "alice", To: "bob",
		AmountCents: 1000, FeeCents: 25, FeeAccount: "fees",
	})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if receipt.FeeCents != 25 {
		t.Errorf("receipt.FeeCents = %d, want 25", receipt.FeeCents)
	}
	if len(receipt.Entries) != 2 {
		t.Fatalf("receipt.Entries has %d entries, want 2 (principal + fee)", len(receipt.Entries))
	}
	principal, fee := receipt.Entries[0], receipt.Entries[1]
	if principal.ID != "tx-fee" || principal.DebitAccount != "bob" ||
		principal.CreditAccount != "alice" || principal.AmountCents != 1000 {
		t.Errorf("principal entry = %+v, want id tx-fee debit bob credit alice amount 1000", principal)
	}
	// The fee is charged on top of the amount: the payer funds the fee
	// account, the payee still receives the full amount.
	if fee.ID != "tx-fee/fee" || fee.DebitAccount != "fees" ||
		fee.CreditAccount != "alice" || fee.AmountCents != 25 {
		t.Errorf("fee entry = %+v, want id tx-fee/fee debit fees credit alice amount 25", fee)
	}

	// The payer's total outflow is amount + fee.
	if got := l.Balance("alice"); got != 10000-1025 {
		t.Errorf("alice balance = %d, want %d", got, 10000-1025)
	}
	if got := l.Balance("bob"); got != 1000 {
		t.Errorf("bob balance = %d, want 1000", got)
	}
	if got := l.Balance("fees"); got != 25 {
		t.Errorf("fees balance = %d, want 25", got)
	}
	if _, v := l.Snapshot("alice"); v != vBefore+2 {
		t.Errorf("version = %d, want %d (two bumps)", v, vBefore+2)
	}
	if n := len(l.Entries()); n != nBefore+2 {
		t.Errorf("journal entries = %d, want %d", n, nBefore+2)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Errorf("VerifyAccountingEquation: %v", err)
	}
	if err := l.VerifyChain(); err != nil {
		t.Errorf("VerifyChain: %v", err)
	}
	// Both legs are visible in the per-account journal export.
	page, _, err := l.ListAccountEntries("fees",
		time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC), "", 10)
	if err != nil {
		t.Fatalf("ListAccountEntries: %v", err)
	}
	if len(page) != 1 || page[0].ID != "tx-fee/fee" {
		t.Errorf("fees journal = %+v, want the fee entry", page)
	}
}

func TestPostTransferFeeValidation(t *testing.T) {
	cases := []struct {
		name string
		tr   Transfer
	}{
		{"negative fee", Transfer{ID: "t", From: "a", To: "b", AmountCents: 100, FeeCents: -1, FeeAccount: "f"}},
		{"fee without account", Transfer{ID: "t", From: "a", To: "b", AmountCents: 100, FeeCents: 5}},
		{"account without fee", Transfer{ID: "t", From: "a", To: "b", AmountCents: 100, FeeAccount: "f"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := New()
			_, err := l.PostTransfer(tc.tr)
			if !errors.Is(err, ErrInvalidFee) {
				t.Fatalf("PostTransfer err = %v, want ErrInvalidFee", err)
			}
			assertLedgerUntouched(t, l, 0, 0)
		})
	}
}

func TestPostTransferFeePolicy(t *testing.T) {
	l := New(WithTransferFeePolicy(250, "revenue")) // 2.5%
	seedBalance(t, l, "alice", 100000)

	receipt, err := l.PostTransfer(Transfer{ID: "tx-pol", From: "alice", To: "bob", AmountCents: 10000})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if receipt.FeeCents != 250 {
		t.Errorf("policy fee = %d, want 250 (2.5%% of 10000)", receipt.FeeCents)
	}
	if len(receipt.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(receipt.Entries))
	}
	if fee := receipt.Entries[1]; fee.DebitAccount != "revenue" || fee.CreditAccount != "alice" {
		t.Errorf("policy fee entry = %+v, want debit revenue credit alice", fee)
	}
	if got := l.Balance("revenue"); got != 250 {
		t.Errorf("revenue balance = %d, want 250", got)
	}

	// Policy fees round down: 101 * 250 / 10000 = 2.525 -> 2.
	receipt, err = l.PostTransfer(Transfer{ID: "tx-pol2", From: "alice", To: "bob", AmountCents: 101})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if receipt.FeeCents != 2 {
		t.Errorf("policy fee on 101 = %d, want 2 (floor)", receipt.FeeCents)
	}

	// A computed fee of zero posts no leg: the receipt stays single-entry.
	receipt, err = l.PostTransfer(Transfer{ID: "tx-pol3", From: "alice", To: "bob", AmountCents: 10})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if receipt.FeeCents != 0 || len(receipt.Entries) != 1 {
		t.Errorf("dust transfer receipt = %+v, want no fee leg", receipt)
	}
}

func TestPostTransferFeePolicyDisabled(t *testing.T) {
	// Empty revenue account disables the policy even with a rate set.
	l := New(WithTransferFeePolicy(250, ""))
	seedBalance(t, l, "alice", 1000)
	receipt, err := l.PostTransfer(Transfer{ID: "t1", From: "alice", To: "bob", AmountCents: 100})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if receipt.FeeCents != 0 || len(receipt.Entries) != 1 {
		t.Errorf("receipt = %+v, want no fee leg when policy has no revenue account", receipt)
	}

	// Negative rates normalize to 0: no fee leg.
	l2 := New(WithTransferFeePolicy(-100, "revenue"))
	seedBalance(t, l2, "alice", 1000)
	receipt, err = l2.PostTransfer(Transfer{ID: "t2", From: "alice", To: "bob", AmountCents: 100})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if receipt.FeeCents != 0 || len(receipt.Entries) != 1 {
		t.Errorf("receipt = %+v, want no fee leg for negative rate", receipt)
	}
}

func TestPostTransferSkipFee(t *testing.T) {
	l := New(WithTransferFeePolicy(250, "revenue"))
	seedBalance(t, l, "alice", 10000)

	receipt, err := l.PostTransfer(Transfer{
		ID: "tx-skip", From: "alice", To: "bob", AmountCents: 10000, SkipFee: true,
	})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if receipt.FeeCents != 0 || len(receipt.Entries) != 1 {
		t.Errorf("receipt = %+v, want SkipFee to suppress the policy", receipt)
	}
	if got := l.Balance("alice"); got != 0 {
		t.Errorf("alice balance = %d, want 0 (no fee taken)", got)
	}
}

func TestPostTransferExplicitFeeOverridesPolicy(t *testing.T) {
	l := New(WithTransferFeePolicy(250, "revenue"))
	seedBalance(t, l, "alice", 10000)

	receipt, err := l.PostTransfer(Transfer{
		ID: "tx-ovr", From: "alice", To: "bob",
		AmountCents: 10000, FeeCents: 99, FeeAccount: "custom-fees",
	})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if receipt.FeeCents != 99 {
		t.Errorf("fee = %d, want explicit 99 (not policy 250)", receipt.FeeCents)
	}
	if fee := receipt.Entries[1]; fee.DebitAccount != "custom-fees" {
		t.Errorf("fee account = %q, want custom-fees", fee.DebitAccount)
	}
	if got := l.Balance("revenue"); got != 0 {
		t.Errorf("revenue balance = %d, want 0 (policy suppressed by explicit fee)", got)
	}
}

func TestPostTransferFrozenFeeAccountRollsBack(t *testing.T) {
	l := New()
	seedBalance(t, l, "alice", 10000)
	l.Freeze("fees")

	_, vBefore := l.Snapshot("alice")
	nBefore := len(l.Entries())
	_, err := l.PostTransfer(Transfer{
		ID: "tx-ff", From: "alice", To: "bob",
		AmountCents: 1000, FeeCents: 25, FeeAccount: "fees",
	})
	if !errors.Is(err, ErrAccountFrozen) {
		t.Fatalf("PostTransfer err = %v, want ErrAccountFrozen", err)
	}
	// Atomicity across legs: the principal entry must NOT be recorded when
	// the fee leg fails — either every leg lands or nothing does.
	assertLedgerUntouched(t, l, vBefore, nBefore)
	if got := l.Balance("alice"); got != 10000 {
		t.Errorf("alice balance = %d, want 10000 (principal rolled back)", got)
	}
	if got := l.Balance("bob"); got != 0 {
		t.Errorf("bob balance = %d, want 0 (principal rolled back)", got)
	}
}

func TestPostTransferOverdraftCoversFee(t *testing.T) {
	l := New(WithOverdraftProtection("alice"))
	seedBalance(t, l, "alice", 1000)

	// 900 + 200 fee = 1100 outflow > 1000 balance: rejected.
	_, vBefore := l.Snapshot("alice")
	nBefore := len(l.Entries())
	_, err := l.PostTransfer(Transfer{
		ID: "tx-od", From: "alice", To: "bob",
		AmountCents: 900, FeeCents: 200, FeeAccount: "fees",
	})
	if !errors.Is(err, ErrAccountOverdraft) {
		t.Fatalf("PostTransfer err = %v, want ErrAccountOverdraft", err)
	}
	assertLedgerUntouched(t, l, vBefore, nBefore)

	// Exact total outflow (800 + 200 = 1000) is allowed: draining to zero.
	receipt, err := l.PostTransfer(Transfer{
		ID: "tx-od2", From: "alice", To: "bob",
		AmountCents: 800, FeeCents: 200, FeeAccount: "fees",
	})
	if err != nil {
		t.Fatalf("exact-outflow PostTransfer: %v", err)
	}
	if receipt.FeeCents != 200 || len(receipt.Entries) != 2 {
		t.Errorf("receipt = %+v, want fee leg booked", receipt)
	}
	if got := l.Balance("alice"); got != 0 {
		t.Errorf("alice balance = %d, want 0", got)
	}
}

func TestPostTransferFeeReplay(t *testing.T) {
	l := New()
	seedBalance(t, l, "alice", 10000)

	first, err := l.PostTransfer(Transfer{
		ID: "tx-fr", From: "alice", To: "bob",
		AmountCents: 1000, FeeCents: 25, FeeAccount: "fees",
		IdempotencyKey: "fee-key-1",
	})
	if err != nil {
		t.Fatalf("first PostTransfer: %v", err)
	}
	_, vAfterFirst := l.Snapshot("alice")

	second, err := l.PostTransfer(Transfer{
		ID: "tx-fr-retry", From: "alice", To: "bob",
		AmountCents: 1000, FeeCents: 25, FeeAccount: "fees",
		IdempotencyKey: "fee-key-1",
	})
	if err != nil {
		t.Fatalf("replay PostTransfer: %v", err)
	}
	if !second.Duplicate {
		t.Errorf("replay Duplicate = false, want true")
	}
	if len(second.Entries) != 2 {
		t.Fatalf("replay entries = %d, want the full receipt (principal + fee)", len(second.Entries))
	}
	for i := range first.Entries {
		if second.Entries[i].ID != first.Entries[i].ID {
			t.Errorf("replay entry %d = %q, want %q", i, second.Entries[i].ID, first.Entries[i].ID)
		}
	}
	if second.FeeCents != 25 {
		t.Errorf("replay FeeCents = %d, want 25", second.FeeCents)
	}
	if _, v := l.Snapshot("alice"); v != vAfterFirst {
		t.Errorf("version moved on replay: %d -> %d", vAfterFirst, v)
	}
	if got := l.Balance("alice"); got != 10000-1025 {
		t.Errorf("alice balance = %d, want %d (applied once)", got, 10000-1025)
	}
}

func TestPostTransferFeeAmountOverflow(t *testing.T) {
	l := New()
	_, err := l.PostTransfer(Transfer{
		ID: "tx-big", From: "alice", To: "bob",
		AmountCents: math.MaxInt64, FeeCents: 1, FeeAccount: "fees",
	})
	if !errors.Is(err, ErrAmountOverflow) {
		t.Fatalf("PostTransfer err = %v, want ErrAmountOverflow", err)
	}
	assertLedgerUntouched(t, l, 0, 0)
}

func TestPostTransferFeePolicyArithmetic(t *testing.T) {
	// The split multiplication must equal floor(amount*rate/10000) for a
	// spread of amounts, including values near MaxInt64/10000.
	for _, amount := range []int64{0, 1, 9999, 10000, 10001, 123456789, math.MaxInt64 / 10000, math.MaxInt64} {
		for _, rate := range []int64{0, 1, 250, 9999, 10000} {
			// Reference via big-free decomposition: amount = 10000*q + r.
			q, r := amount/10000, amount%10000
			want := q*rate + r*rate/10000
			if got := policyFeeCents(amount, rate); got != want {
				t.Errorf("policyFeeCents(%d, %d) = %d, want %d", amount, rate, got, want)
			}
		}
	}
}

func TestExpireIdempotencyKeysDropsTransferReceipts(t *testing.T) {
	l := New(WithIdempotencyTTL(time.Hour))
	now := time.Now()
	seedBalance(t, l, "alice", 10000)

	old := now.Add(-2 * time.Hour)
	if _, err := l.PostTransfer(Transfer{
		ID: "tx-old", From: "alice", To: "bob",
		AmountCents: 100, FeeCents: 5, FeeAccount: "fees",
		IdempotencyKey: "old-fee-key", CreatedAt: old,
	}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}

	if removed := l.ExpireIdempotencyKeys(); removed != 2 {
		t.Fatalf("ExpireIdempotencyKeys removed %d keys, want 2 (principal + fee leg)", removed)
	}
	// The transfer receipt index expired with the principal key: reposting
	// the key books brand-new entries (duplicate == false).
	receipt, err := l.PostTransfer(Transfer{
		ID: "tx-old-retry", From: "alice", To: "bob",
		AmountCents: 100, FeeCents: 5, FeeAccount: "fees",
		IdempotencyKey: "old-fee-key", CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("repost after expiry: %v", err)
	}
	if receipt.Duplicate {
		t.Errorf("repost after TTL expiry returned Duplicate = true, want a fresh booking")
	}
	if len(receipt.Entries) != 2 {
		t.Errorf("repost entries = %d, want 2", len(receipt.Entries))
	}
}
