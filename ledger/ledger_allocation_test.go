package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func allocTestRule() AllocationRule {
	return AllocationRule{
		ID:     "platform-fee",
		Source: "fee-clearing",
		Targets: []AllocationTarget{
			{Account: "merchant-a", PctBps: 5000},
			{Account: "merchant-b", PctBps: 3000},
			{Account: "merchant-c", PctBps: 2000},
		},
		Description: "monthly platform fee split",
	}
}

func mustSetAllocRule(t *testing.T, l *Ledger, r AllocationRule) {
	t.Helper()
	if err := l.SetAllocationRule(r); err != nil {
		t.Fatalf("SetAllocationRule: %v", err)
	}
}

func TestValidateAllocationRule(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*AllocationRule)
		want error
	}{
		{"valid", func(r *AllocationRule) {}, nil},
		{"empty id", func(r *AllocationRule) { r.ID = "" }, ErrAllocationRuleEmptyID},
		{"id too long", func(r *AllocationRule) { r.ID = strings.Repeat("x", 65) }, ErrAllocationRuleIDTooLong},
		{"id separator", func(r *AllocationRule) { r.ID = "a/b" }, ErrAllocationRuleIDSeparator},
		{"empty source", func(r *AllocationRule) { r.Source = "" }, ErrAllocationRuleSourceEmpty},
		{"no targets", func(r *AllocationRule) { r.Targets = nil }, ErrAllocationRuleNoTargets},
		{"empty target account", func(r *AllocationRule) { r.Targets[0].Account = "" }, ErrAllocationRuleTargetAccounts},
		{"duplicate target", func(r *AllocationRule) { r.Targets[1].Account = r.Targets[0].Account }, ErrAllocationRuleTargetAccounts},
		{"target is source", func(r *AllocationRule) { r.Targets[2].Account = r.Source }, ErrAllocationRuleTargetAccounts},
		{"zero weight", func(r *AllocationRule) { r.Targets[0].PctBps = 0 }, ErrAllocationRuleTargetWeight},
		{"weight over 10000", func(r *AllocationRule) { r.Targets[0].PctBps = 10001 }, ErrAllocationRuleTargetWeight},
		{"weights under", func(r *AllocationRule) { r.Targets[2].PctBps = 1999 }, ErrAllocationRuleWeightsSum},
		{"weights over", func(r *AllocationRule) { r.Targets[2].PctBps = 2001 }, ErrAllocationRuleWeightsSum},
		{"description too long", func(r *AllocationRule) { r.Description = strings.Repeat("x", 256) }, ErrAllocationRuleDescriptionTooLong},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := allocTestRule()
			c.mut(&r)
			err := ValidateAllocationRule(r)
			if c.want == nil {
				if err != nil {
					t.Fatalf("ValidateAllocationRule: unexpected error %v", err)
				}
				return
			}
			if !errors.Is(err, c.want) {
				t.Fatalf("ValidateAllocationRule: got %v, want %v", err, c.want)
			}
			// SetAllocationRule must reject the same way and store nothing.
			l := New()
			if serr := l.SetAllocationRule(r); !errors.Is(serr, c.want) {
				t.Fatalf("SetAllocationRule: got %v, want %v", serr, c.want)
			}
			if _, ok := l.GetAllocationRule(r.ID); ok && r.ID != "" {
				t.Fatal("SetAllocationRule stored an illegal rule")
			}
		})
	}
}

func TestAllocationRuleRegistry(t *testing.T) {
	l := New()
	if _, ok := l.GetAllocationRule("platform-fee"); ok {
		t.Fatal("GetAllocationRule: unexpected hit")
	}
	mustSetAllocRule(t, l, allocTestRule())
	if v := l.AllocationRuleVersion(); v != 1 {
		t.Fatalf("AllocationRuleVersion = %d, want 1", v)
	}
	got, ok := l.GetAllocationRule("platform-fee")
	if !ok {
		t.Fatal("GetAllocationRule: missing")
	}
	if got.Source != "fee-clearing" || len(got.Targets) != 3 {
		t.Fatalf("GetAllocationRule: wrong rule %+v", got)
	}
	// Registry returns a copy: mutating the returned slice must not
	// corrupt the stored rule.
	got.Targets[0].PctBps = 1
	again, _ := l.GetAllocationRule("platform-fee")
	if again.Targets[0].PctBps != 5000 {
		t.Fatal("GetAllocationRule: registry not defensively copied")
	}
	// Replace bumps the version; delete removes and bumps.
	r := allocTestRule()
	r.Description = "v2"
	mustSetAllocRule(t, l, r)
	if v := l.AllocationRuleVersion(); v != 2 {
		t.Fatalf("AllocationRuleVersion = %d, want 2", v)
	}
	if !l.DeleteAllocationRule("platform-fee") {
		t.Fatal("DeleteAllocationRule: reported missing")
	}
	if l.DeleteAllocationRule("platform-fee") {
		t.Fatal("DeleteAllocationRule: double delete reported present")
	}
	if v := l.AllocationRuleVersion(); v != 3 {
		t.Fatalf("AllocationRuleVersion = %d, want 3", v)
	}
	if _, ok := l.GetAllocationRule("platform-fee"); ok {
		t.Fatal("GetAllocationRule: rule survived delete")
	}
	// Sorted listing.
	mustSetAllocRule(t, l, AllocationRule{ID: "b", Source: "s", Targets: []AllocationTarget{{Account: "t", PctBps: 10000}}})
	mustSetAllocRule(t, l, AllocationRule{ID: "a", Source: "s", Targets: []AllocationTarget{{Account: "t", PctBps: 10000}}})
	rules := l.AllocationRules()
	if len(rules) != 2 || rules[0].ID != "a" || rules[1].ID != "b" {
		t.Fatalf("AllocationRules not sorted: %+v", rules)
	}
}

func TestAllocateCostHappyPath(t *testing.T) {
	l := New()
	mustSetAllocRule(t, l, allocTestRule())
	before := l.Version()

	receipt, dup, err := l.AllocateCost("platform-fee", 10000, "USD")
	if err != nil {
		t.Fatalf("AllocateCost: %v", err)
	}
	if dup {
		t.Fatal("AllocateCost: unexpected duplicate")
	}
	if receipt.RuleID != "platform-fee" || receipt.TotalCents != 10000 || receipt.Currency != "USD" {
		t.Fatalf("bad receipt: %+v", receipt)
	}
	if receipt.Month != time.Now().UTC().Format("2006-01") {
		t.Fatalf("bad receipt month: %q", receipt.Month)
	}
	if len(receipt.EntryIDs) != 3 || len(receipt.Entries) != 3 {
		t.Fatalf("want 3 legs, got %+v", receipt)
	}
	// Shares: 5000/3000/2000 exactly, legs sum to the full amount.
	wantShares := []int64{5000, 3000, 2000}
	var sum int64
	for i, e := range receipt.Entries {
		if e.AmountCents != wantShares[i] {
			t.Fatalf("leg %d = %d cents, want %d", i, e.AmountCents, wantShares[i])
		}
		sum += e.AmountCents
		if e.DebitAccount != AccountID([]string{"merchant-a", "merchant-b", "merchant-c"}[i]) {
			t.Fatalf("leg %d debits %q", i, e.DebitAccount)
		}
		if e.CreditAccount != "fee-clearing" {
			t.Fatalf("leg %d credits %q, want fee-clearing", i, e.CreditAccount)
		}
		if e.Memo != "alloc:platform-fee" {
			t.Fatalf("leg %d memo = %q", i, e.Memo)
		}
		wantID := "alloc/platform-fee/" + receipt.Month + "/leg/" + string(rune('0'+i))
		if e.ID != wantID {
			t.Fatalf("leg %d ID = %q, want %q", i, e.ID, wantID)
		}
	}
	if sum != 10000 {
		t.Fatalf("legs sum to %d, want 10000", sum)
	}
	// Balances: source credited the full amount, targets debited shares.
	if got := l.BalanceIn("fee-clearing", "USD"); got != -10000 {
		t.Fatalf("fee-clearing balance = %d, want -10000", got)
	}
	for i, acct := range []string{"merchant-a", "merchant-b", "merchant-c"} {
		if got := l.BalanceIn(AccountID(acct), "USD"); got != wantShares[i] {
			t.Fatalf("%s balance = %d, want %d", acct, got, wantShares[i])
		}
	}
	// Accounting equation: every leg balances by construction, and the
	// full journal still balances.
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Fatalf("VerifyAccountingEquation: %v", err)
	}
	if got := l.Version(); got != before+3 {
		t.Fatalf("version = %d, want %d", got, before+3)
	}
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	if n := l.AllocationsCommitted(); n != 1 {
		t.Fatalf("AllocationsCommitted = %d, want 1", n)
	}
	// Discoverable through the ?memo= filter like rule legs.
	page, _, err := l.ListEntriesFiltered(time.Time{}, time.Now().Add(time.Hour), "", 100, "alloc:platform-fee", "")
	if err != nil {
		t.Fatalf("ListEntriesFiltered: %v", err)
	}
	if len(page) != 3 {
		t.Fatalf("memo query found %d entries, want 3", len(page))
	}
}

func TestAllocateCostDustRemainder(t *testing.T) {
	l := New()
	mustSetAllocRule(t, l, allocTestRule())
	// 10001 * 50% = 5000 (floor), remainder 1 lands on the first target.
	receipt, _, err := l.AllocateCost("platform-fee", 10001, "USD")
	if err != nil {
		t.Fatalf("AllocateCost: %v", err)
	}
	want := []int64{5001, 3000, 2000}
	var sum int64
	for i, e := range receipt.Entries {
		if e.AmountCents != want[i] {
			t.Fatalf("leg %d = %d, want %d", i, e.AmountCents, want[i])
		}
		sum += e.AmountCents
	}
	if sum != 10001 {
		t.Fatalf("legs sum to %d, want 10001", sum)
	}
}

func TestAllocateCostIdempotentReplay(t *testing.T) {
	l := New(WithIdempotencyTTL(time.Hour))
	mustSetAllocRule(t, l, allocTestRule())
	first, dup, err := l.AllocateCost("platform-fee", 10000, "USD")
	if err != nil || dup {
		t.Fatalf("AllocateCost: %v %v", err, dup)
	}
	versionAfterFirst := l.Version()
	second, dup, err := l.AllocateCost("platform-fee", 99999, "USD")
	if err != nil {
		t.Fatalf("replay AllocateCost: %v", err)
	}
	if !dup {
		t.Fatal("replay AllocateCost: duplicate = false")
	}
	if len(second.EntryIDs) != len(first.EntryIDs) {
		t.Fatal("replay receipt has different legs")
	}
	for i := range second.EntryIDs {
		if second.EntryIDs[i] != first.EntryIDs[i] {
			t.Fatal("replay receipt entry IDs differ")
		}
	}
	if second.TotalCents != 10000 {
		t.Fatalf("replay receipt total = %d, want original 10000", second.TotalCents)
	}
	if l.Version() != versionAfterFirst {
		t.Fatal("replay booked new entries")
	}
	if n := l.AllocationsCommitted(); n != 1 {
		t.Fatalf("AllocationsCommitted = %d, want 1 (replay not counted)", n)
	}
	// A different UTC month is a new allocation, not a replay.
	now := time.Now()
	nextMonth := now.AddDate(0, 1, 0)
	third, dup, err := l.allocateCostLocked("platform-fee", 10000, "USD", nextMonth)
	if err != nil || dup {
		t.Fatalf("next-month AllocateCost: %v %v", err, dup)
	}
	if third.Month == first.Month {
		t.Fatal("next-month allocation reused the same month key")
	}
	if third.EntryIDs[0] == first.EntryIDs[0] {
		t.Fatal("next-month allocation reused leg IDs")
	}
}

func TestAllocateCostNotFound(t *testing.T) {
	l := New()
	if _, _, err := l.AllocateCost("nope", 100, "USD"); !errors.Is(err, ErrAllocationNotFound) {
		t.Fatalf("AllocateCost: got %v, want ErrAllocationNotFound", err)
	}
	if _, _, err := l.AllocateCost("platform-fee", 0, "USD"); !errors.Is(err, ErrAllocationNotFound) {
		t.Fatalf("missing rule must fail before amount validation: %v", err)
	}
}

func TestAllocateCostValidation(t *testing.T) {
	l := New()
	mustSetAllocRule(t, l, allocTestRule())
	if _, _, err := l.AllocateCost("platform-fee", 0, "USD"); !errors.Is(err, ErrAllocationNonPositiveAmount) {
		t.Fatalf("zero amount: got %v", err)
	}
	if _, _, err := l.AllocateCost("platform-fee", -5, "USD"); !errors.Is(err, ErrAllocationNonPositiveAmount) {
		t.Fatalf("negative amount: got %v", err)
	}
	if _, _, err := l.AllocateCost("platform-fee", 100, "US"); !errors.Is(err, ErrInvalidCurrency) {
		t.Fatalf("bad currency: got %v", err)
	}
	// Dust: 2 cents across 3 targets — a share computes to zero.
	if _, _, err := l.AllocateCost("platform-fee", 2, "USD"); !errors.Is(err, ErrAllocationDustLeg) {
		t.Fatalf("dust amount: got %v, want ErrAllocationDustLeg", err)
	}
	if l.Version() != 0 {
		t.Fatal("rejected allocation recorded entries")
	}
}

func TestAllocateCostAtomicOnGateFailure(t *testing.T) {
	newRuleLedger := func() *Ledger {
		l := New()
		mustSetAllocRule(t, l, allocTestRule())
		return l
	}
	t.Run("frozen source", func(t *testing.T) {
		l := newRuleLedger()
		l.Freeze("fee-clearing")
		_, _, err := l.AllocateCost("platform-fee", 10000, "USD")
		if !errors.Is(err, ErrAccountFrozen) {
			t.Fatalf("got %v, want ErrAccountFrozen", err)
		}
		if l.Version() != 0 || len(l.Entries()) != 0 {
			t.Fatal("frozen allocation recorded partial legs")
		}
	})
	t.Run("frozen target", func(t *testing.T) {
		l := newRuleLedger()
		l.Freeze("merchant-b")
		_, _, err := l.AllocateCost("platform-fee", 10000, "USD")
		if !errors.Is(err, ErrAccountFrozen) {
			t.Fatalf("got %v, want ErrAccountFrozen", err)
		}
		if l.Version() != 0 {
			t.Fatal("frozen-target allocation recorded partial legs")
		}
	})
	t.Run("overdraft protected source", func(t *testing.T) {
		l := newRuleLedger()
		l.EnableOverdraftProtection("fee-clearing")
		// fee-clearing has a zero balance: any allocation overdraws it.
		_, _, err := l.AllocateCost("platform-fee", 10000, "USD")
		if !errors.Is(err, ErrAccountOverdraft) {
			t.Fatalf("got %v, want ErrAccountOverdraft", err)
		}
		if l.Version() != 0 {
			t.Fatal("overdraft allocation recorded partial legs")
		}
	})
	t.Run("daily limit on source", func(t *testing.T) {
		l := newRuleLedger()
		if err := l.SetDailyLimit("fee-clearing", "USD", 9999); err != nil {
			t.Fatalf("SetDailyLimit: %v", err)
		}
		_, _, err := l.AllocateCost("platform-fee", 10000, "USD")
		if !errors.Is(err, ErrDailyLimitExceeded) {
			t.Fatalf("got %v, want ErrDailyLimitExceeded", err)
		}
		if l.Version() != 0 {
			t.Fatal("limited allocation recorded partial legs")
		}
	})
	t.Run("monthly budget on source", func(t *testing.T) {
		l := newRuleLedger()
		if err := l.SetAccountBudget("fee-clearing", "USD", 9999); err != nil {
			t.Fatalf("SetAccountBudget: %v", err)
		}
		_, _, err := l.AllocateCost("platform-fee", 10000, "USD")
		if !errors.Is(err, ErrBudgetExceeded) {
			t.Fatalf("got %v, want ErrBudgetExceeded", err)
		}
		if l.Version() != 0 {
			t.Fatal("budgeted allocation recorded partial legs")
		}
	})
	t.Run("closed period", func(t *testing.T) {
		l := newRuleLedger()
		if err := l.ClosePeriod("2026-10"); err != nil {
			t.Fatalf("ClosePeriod: %v", err)
		}
		at := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
		_, _, err := l.allocateCostLocked("platform-fee", 10000, "USD", at)
		if !errors.Is(err, ErrPeriodClosed) {
			t.Fatalf("got %v, want ErrPeriodClosed", err)
		}
		if l.Version() != 0 {
			t.Fatal("closed-period allocation recorded partial legs")
		}
	})
	t.Run("leg ID conflict", func(t *testing.T) {
		l := newRuleLedger()
		now := time.Now()
		month := now.UTC().Format("2006-01")
		conflictID := "alloc/platform-fee/" + month + "/leg/1"
		if _, _, err := l.Post(JournalEntry{ID: conflictID, DebitAccount: "x", CreditAccount: "y", AmountCents: 1, Currency: "USD"}); err != nil {
			t.Fatalf("Post: %v", err)
		}
		_, _, err := l.AllocateCost("platform-fee", 10000, "USD")
		if !errors.Is(err, ErrAllocationLegConflict) {
			t.Fatalf("got %v, want ErrAllocationLegConflict", err)
		}
	})
}

func TestAllocateCostSkipsJournalRules(t *testing.T) {
	l := New()
	mustSetAllocRule(t, l, allocTestRule())
	// A rule that would fire on any posting debiting merchant-a: on a
	// raw Post it appends a derived leg.
	if err := l.SetJournalRule(JournalRule{
		ID:                "tax-withholding",
		Enabled:           true,
		MatchDebitAccount: "merchant-a",
		Legs: []RuleLeg{
			{DebitAccount: "tax-payable", CreditAccount: "merchant-a", RateBps: 1000, Label: "withholding"},
		},
	}); err != nil {
		t.Fatalf("SetJournalRule: %v", err)
	}
	before := l.RuleLegsCommitted()
	receipt, _, err := l.AllocateCost("platform-fee", 10000, "USD")
	if err != nil {
		t.Fatalf("AllocateCost: %v", err)
	}
	if got := l.RuleLegsCommitted(); got != before {
		t.Fatalf("RuleLegsCommitted moved %d -> %d: journal rules fired on allocation legs", before, got)
	}
	if len(receipt.Entries) != 3 {
		t.Fatalf("want exactly 3 legs, got %d (rule legs leaked in)", len(receipt.Entries))
	}
	for _, e := range l.Entries() {
		if strings.Contains(e.ID, "/rule/") {
			t.Fatalf("derived rule leg journaled: %q", e.ID)
		}
	}
	// Sanity: the same posting through Post DOES fire the rule.
	if _, _, err := l.Post(JournalEntry{ID: "raw-1", DebitAccount: "merchant-a", CreditAccount: "cash", AmountCents: 10000, Currency: "USD"}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if got := l.RuleLegsCommitted(); got != before+1 {
		t.Fatalf("RuleLegsCommitted = %d, want %d: rule stopped firing on Post", got, before+1)
	}
}

func TestAllocateCostSkipsDualReview(t *testing.T) {
	// A review threshold that would park any large transfer in pending
	// review must not touch allocations: they settle immediately.
	l := New(WithReviewThreshold(100))
	mustSetAllocRule(t, l, allocTestRule())
	receipt, _, err := l.AllocateCost("platform-fee", 1000000, "USD")
	if err != nil {
		t.Fatalf("AllocateCost: %v", err)
	}
	for _, e := range receipt.Entries {
		if e.PendingReview {
			t.Fatalf("leg %q parked in pending review", e.ID)
		}
	}
	if n := len(l.PendingTransferReviews()); n != 0 {
		t.Fatalf("PendingTransferReviews = %d, want 0", n)
	}
	// Sanity: a transfer at the threshold still parks.
	if _, err := l.PostTransfer(Transfer{ID: "t-review", From: "payer", To: "payee", AmountCents: 100, Currency: "USD"}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if n := len(l.PendingTransferReviews()); n != 1 {
		t.Fatalf("PendingTransferReviews = %d, want 1: review stopped engaging", n)
	}
}

func TestAllocateCostSnapshotRoundTrip(t *testing.T) {
	l := New(WithIdempotencyTTL(time.Hour))
	mustSetAllocRule(t, l, allocTestRule())
	receipt, _, err := l.AllocateCost("platform-fee", 10000, "USD")
	if err != nil {
		t.Fatalf("AllocateCost: %v", err)
	}
	versionBefore := l.AllocationRuleVersion()

	var buf strings.Builder
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	l2, err := ImportSnapshot(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	// Rules and the mutation counter round-trip.
	r, ok := l2.GetAllocationRule("platform-fee")
	if !ok || r.Source != "fee-clearing" || len(r.Targets) != 3 {
		t.Fatalf("rule did not round-trip: %+v", r)
	}
	if v := l2.AllocationRuleVersion(); v != versionBefore {
		t.Fatalf("AllocationRuleVersion = %d, want %d", v, versionBefore)
	}
	// The receipt registry round-trips: a replay returns the original
	// receipt with duplicate == true and books nothing.
	replay, dup, err := l2.AllocateCost("platform-fee", 10000, "USD")
	if err != nil {
		t.Fatalf("replay after restore: %v", err)
	}
	if !dup {
		t.Fatal("replay after restore: duplicate = false")
	}
	for i := range replay.EntryIDs {
		if replay.EntryIDs[i] != receipt.EntryIDs[i] {
			t.Fatal("replay after restore: entry IDs differ")
		}
	}
	if l2.BalanceIn("fee-clearing", "USD") != -10000 {
		t.Fatal("balances did not round-trip")
	}
	if err := l2.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain after restore: %v", err)
	}
}

func TestAllocateCostSnapshotRejectsIllegalRule(t *testing.T) {
	l := New()
	mustSetAllocRule(t, l, allocTestRule())
	var buf strings.Builder
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	// Corrupt the snapshot's rule: weights that do not sum to 10000.
	corrupted := strings.Replace(buf.String(),
		`"pct_bps":2000`, `"pct_bps":2001`, 1)
	if corrupted == buf.String() {
		t.Fatal("test setup: rule JSON not found in snapshot")
	}
	if _, err := ImportSnapshot(strings.NewReader(corrupted)); err == nil {
		t.Fatal("ImportSnapshot accepted a snapshot with an illegal allocation rule")
	}
}

func TestAllocateCostTTLExpiry(t *testing.T) {
	l := New(WithIdempotencyTTL(time.Nanosecond))
	mustSetAllocRule(t, l, allocTestRule())
	now := time.Now()
	if _, _, err := l.allocateCostLocked("platform-fee", 10000, "USD", now); err != nil {
		t.Fatalf("AllocateCost: %v", err)
	}
	time.Sleep(2 * time.Nanosecond)
	removed := l.ExpireIdempotencyKeys()
	if removed == 0 {
		t.Fatal("ExpireIdempotencyKeys removed nothing")
	}
	// After expiry the same month allocates again — but the deterministic
	// leg IDs are journaled, so it fails with a leg conflict instead of
	// double-booking. That is the honest outcome: the key expiry only
	// drops the replay shortcut.
	_, _, err := l.allocateCostLocked("platform-fee", 10000, "USD", now)
	if !errors.Is(err, ErrAllocationLegConflict) {
		t.Fatalf("post-expiry re-allocation: got %v, want ErrAllocationLegConflict", err)
	}
}
