package ledger

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// reviewSeed funds payer and returns a ledger with a global review
// threshold armed.
func reviewSeed(t *testing.T, thresholdCents int64) *Ledger {
	t.Helper()
	l := New(WithReviewThreshold(thresholdCents))
	seedBalance(t, l, "payer", 100000)
	return l
}

func TestReviewThresholdGate(t *testing.T) {
	l := reviewSeed(t, 10000)

	// Below the threshold: settles immediately, no review.
	receipt, err := l.PostTransfer(Transfer{ID: "tx-small", From: "payer", To: "payee", AmountCents: 9999})
	if err != nil {
		t.Fatalf("PostTransfer small: %v", err)
	}
	if receipt.ReviewStatus != "" {
		t.Errorf("small transfer ReviewStatus = %q, want empty", receipt.ReviewStatus)
	}
	if _, ok := l.GetTransferReview("tx-small"); ok {
		t.Error("small transfer registered a review, want none")
	}
	if got := l.Balance("payer"); got != 100000-9999 {
		t.Errorf("payer balance = %d, want %d", got, 100000-9999)
	}

	// At the threshold: freezes into pending review.
	receipt, err = l.PostTransfer(Transfer{ID: "tx-large", From: "payer", To: "payee", AmountCents: 10000})
	if err != nil {
		t.Fatalf("PostTransfer large: %v", err)
	}
	if receipt.ReviewStatus != ReviewStatusPending {
		t.Errorf("large transfer ReviewStatus = %q, want %q", receipt.ReviewStatus, ReviewStatusPending)
	}
	if receipt.Duplicate {
		t.Error("fresh review receipt has Duplicate = true")
	}
	for _, e := range receipt.Entries {
		if !e.PendingReview {
			t.Errorf("review entry %q PendingReview = false, want true", e.ID)
		}
	}
	rev, ok := l.GetTransferReview("tx-large")
	if !ok {
		t.Fatal("large transfer has no review record")
	}
	if rev.Status != ReviewStatusPending {
		t.Errorf("review status = %q, want pending", rev.Status)
	}
	if rev.ThresholdCents != 10000 {
		t.Errorf("review ThresholdCents = %d, want 10000", rev.ThresholdCents)
	}
}

func TestReviewFreezesFunds(t *testing.T) {
	l := reviewSeed(t, 10000)
	versionBefore := l.Version()
	chainHeadBefore, linksBefore := l.ChainHead()

	receipt, err := l.PostTransfer(Transfer{ID: "tx-freeze", From: "payer", To: "payee", AmountCents: 30000})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	_ = receipt

	// Net balances untouched: the money has not moved.
	if got := l.Balance("payer"); got != 100000 {
		t.Errorf("payer balance = %d, want 100000 (frozen, not moved)", got)
	}
	if got := l.Balance("payee"); got != 0 {
		t.Errorf("payee balance = %d, want 0", got)
	}
	tb := l.TrialBalance("payer")
	if tb.NetBalance != 100000 || tb.TotalDebits != 100000 || tb.TotalCredits != 0 {
		t.Errorf("payer trial balance = %+v, want debits 100000 credits 0", tb)
	}
	// Available deducts the frozen outflow, like a hold.
	if got := l.Available("payer"); got != 100000-30000 {
		t.Errorf("payer available = %d, want %d", got, 100000-30000)
	}
	// Version and audit chain untouched: no settlement happened.
	if got := l.Version(); got != versionBefore {
		t.Errorf("version = %d, want unchanged %d", got, versionBefore)
	}
	if head, links := l.ChainHead(); head != chainHeadBefore || links != linksBefore {
		t.Errorf("chain head moved on review freeze: links %d -> %d", linksBefore, links)
	}
	// The rows are journaled and marked: visible in the journal.
	found := false
	for _, e := range l.Entries() {
		if e.ID == "tx-freeze" {
			found = true
			if !e.PendingReview {
				t.Error("journal row tx-freeze lost its pending marker")
			}
		}
	}
	if !found {
		t.Error("frozen transfer has no journal row")
	}
	if err := l.VerifyChain(); err != nil {
		t.Errorf("VerifyChain with pending review: %v", err)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Errorf("VerifyAccountingEquation with pending review: %v", err)
	}
}

func TestReviewApprove(t *testing.T) {
	l := reviewSeed(t, 10000)
	versionBefore := l.Version()

	receipt, err := l.PostTransfer(Transfer{ID: "tx-appr", From: "payer", To: "payee", AmountCents: 40000})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	_ = receipt

	approved, err := l.ApproveTransferReview("tx-appr", "op-alice")
	if err != nil {
		t.Fatalf("ApproveTransferReview: %v", err)
	}
	if approved.ReviewStatus != ReviewStatusApproved {
		t.Errorf("approval ReviewStatus = %q, want approved", approved.ReviewStatus)
	}
	if approved.Duplicate {
		t.Error("first approval has Duplicate = true")
	}
	// Balances settled.
	if got := l.Balance("payer"); got != 100000-40000 {
		t.Errorf("payer balance = %d, want %d", got, 100000-40000)
	}
	if got := l.Balance("payee"); got != 40000 {
		t.Errorf("payee balance = %d, want 40000", got)
	}
	// Available no longer deducts: the freeze became a real outflow.
	if got := l.Available("payer"); got != 100000-40000 {
		t.Errorf("payer available = %d, want %d", got, 100000-40000)
	}
	// Version bumped once per leg, marker flipped, chain intact.
	if got := l.Version(); got != versionBefore+1 {
		t.Errorf("version = %d, want %d", got, versionBefore+1)
	}
	for _, e := range approved.Entries {
		if e.PendingReview {
			t.Errorf("approved entry %q still marked pending", e.ID)
		}
	}
	if err := l.VerifyChain(); err != nil {
		t.Errorf("VerifyChain after approve: %v", err)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Errorf("VerifyAccountingEquation after approve: %v", err)
	}
	rev, _ := l.GetTransferReview("tx-appr")
	if rev.Status != ReviewStatusApproved || rev.ReviewedBy != "op-alice" {
		t.Errorf("review record = status %q by %q, want approved by op-alice", rev.Status, rev.ReviewedBy)
	}
	if rev.ApprovedReceipt == nil {
		t.Fatal("approved review carries no stored receipt")
	}

	// Idempotent re-approval: same receipt, nothing settles twice.
	versionAfter := l.Version()
	dup, err := l.ApproveTransferReview("tx-appr", "op-bob")
	if err != nil {
		t.Fatalf("re-approve: %v", err)
	}
	if !dup.Duplicate {
		t.Error("re-approve Duplicate = false, want true")
	}
	if dup.ReviewStatus != ReviewStatusApproved {
		t.Errorf("re-approve ReviewStatus = %q, want approved", dup.ReviewStatus)
	}
	if got := l.Version(); got != versionAfter {
		t.Errorf("version moved on re-approve: %d -> %d", versionAfter, got)
	}
	if got := l.Balance("payer"); got != 100000-40000 {
		t.Errorf("payer balance moved on re-approve: %d", got)
	}
	rev2, _ := l.GetTransferReview("tx-appr")
	if rev2.ReviewedBy != "op-alice" {
		t.Errorf("re-approve overwrote reviewer: %q", rev2.ReviewedBy)
	}
}

func TestReviewApproveWithFee(t *testing.T) {
	l := reviewSeed(t, 10000)
	receipt, err := l.PostTransfer(Transfer{
		ID: "tx-fee", From: "payer", To: "payee", AmountCents: 20000,
		FeeCents: 500, FeeAccount: "fee-revenue",
	})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if len(receipt.Entries) != 2 {
		t.Fatalf("review entries = %d, want 2 (principal + fee)", len(receipt.Entries))
	}
	// The freeze covers amount + fee.
	if got := l.Available("payer"); got != 100000-20500 {
		t.Errorf("payer available = %d, want %d", got, 100000-20500)
	}
	approved, err := l.ApproveTransferReview("tx-fee", "op-alice")
	if err != nil {
		t.Fatalf("ApproveTransferReview: %v", err)
	}
	if approved.FeeCents != 500 {
		t.Errorf("approved FeeCents = %d, want 500", approved.FeeCents)
	}
	if got := l.Balance("payer"); got != 100000-20500 {
		t.Errorf("payer balance = %d, want %d", got, 100000-20500)
	}
	if got := l.Balance("fee-revenue"); got != 500 {
		t.Errorf("fee-revenue balance = %d, want 500", got)
	}
}

func TestReviewReject(t *testing.T) {
	l := reviewSeed(t, 10000)
	versionBefore := l.Version()

	if _, err := l.PostTransfer(Transfer{ID: "tx-rej", From: "payer", To: "payee", AmountCents: 25000}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	rev, changed, err := l.RejectTransferReview("tx-rej", "op-carol", "sanctions screen")
	if err != nil {
		t.Fatalf("RejectTransferReview: %v", err)
	}
	if !changed {
		t.Error("first reject changed = false, want true")
	}
	if rev.Status != ReviewStatusRejected || rev.ReviewedBy != "op-carol" {
		t.Errorf("review = status %q by %q, want rejected by op-carol", rev.Status, rev.ReviewedBy)
	}
	// Funds released: available restored, balances never moved.
	if got := l.Available("payer"); got != 100000 {
		t.Errorf("payer available = %d, want 100000 after reject", got)
	}
	if got := l.Balance("payer"); got != 100000 {
		t.Errorf("payer balance = %d, want 100000", got)
	}
	if got := l.Version(); got != versionBefore {
		t.Errorf("version moved on reject: %d -> %d", versionBefore, got)
	}
	if err := l.VerifyChain(); err != nil {
		t.Errorf("VerifyChain after reject: %v", err)
	}

	// Reject is idempotent: no error, no double audit, same record.
	rev2, changed2, err := l.RejectTransferReview("tx-rej", "op-dave", "again")
	if err != nil {
		t.Fatalf("second reject: %v", err)
	}
	if changed2 {
		t.Error("second reject changed = true, want false")
	}
	if rev2.ReviewedBy != "op-carol" {
		t.Errorf("second reject overwrote reviewer: %q", rev2.ReviewedBy)
	}

	// A rejected review cannot be approved afterwards.
	if _, err := l.ApproveTransferReview("tx-rej", "op-alice"); !errors.Is(err, ErrReviewNotPending) {
		t.Errorf("approve after reject = %v, want ErrReviewNotPending", err)
	}
}

func TestReviewNotFound(t *testing.T) {
	l := reviewSeed(t, 10000)
	if _, err := l.ApproveTransferReview("nope", "op"); !errors.Is(err, ErrReviewNotFound) {
		t.Errorf("approve unknown = %v, want ErrReviewNotFound", err)
	}
	if _, _, err := l.RejectTransferReview("nope", "op", ""); !errors.Is(err, ErrReviewNotFound) {
		t.Errorf("reject unknown = %v, want ErrReviewNotFound", err)
	}
}

func TestReviewExpiry(t *testing.T) {
	l := New(WithReviewThreshold(5000), WithReviewExpiry(time.Hour))
	seedBalance(t, l, "payer", 100000)
	if _, err := l.PostTransfer(Transfer{ID: "tx-exp", From: "payer", To: "payee", AmountCents: 9000}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	rev, _ := l.GetTransferReview("tx-exp")
	if rev.ExpiresAt.IsZero() {
		t.Fatal("review has no expiry despite WithReviewExpiry")
	}

	// Not yet expired: sweep marks nothing.
	if n := l.ExpireReviewsAt(rev.CreatedAt.Add(30 * time.Minute)); n != 0 {
		t.Errorf("ExpireReviewsAt early = %d, want 0", n)
	}
	// Past expiry: auto-rejected.
	if n := l.ExpireReviewsAt(rev.CreatedAt.Add(2 * time.Hour)); n != 1 {
		t.Fatalf("ExpireReviewsAt late = %d, want 1", n)
	}
	rev2, _ := l.GetTransferReview("tx-exp")
	if rev2.Status != ReviewStatusExpired {
		t.Errorf("review status = %q, want expired", rev2.Status)
	}
	if rev2.ReviewedBy != "system" {
		t.Errorf("expired review ReviewedBy = %q, want system", rev2.ReviewedBy)
	}
	// Funds released by the sweep.
	if got := l.Available("payer"); got != 100000 {
		t.Errorf("payer available = %d, want 100000 after expiry", got)
	}
	// Approving a lapsed review is refused.
	if _, err := l.ApproveTransferReview("tx-exp", "op"); !errors.Is(err, ErrReviewNotPending) {
		t.Errorf("approve expired = %v, want ErrReviewNotPending", err)
	}
}

func TestReviewApproveAfterExpiryAutoRejects(t *testing.T) {
	l := New(WithReviewThreshold(5000), WithReviewExpiry(time.Hour))
	seedBalance(t, l, "payer", 100000)
	if _, err := l.PostTransfer(Transfer{ID: "tx-late", From: "payer", To: "payee", AmountCents: 9000}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	// Simulate the clock passing the expiry without running the sweep:
	// approve must auto-reject first and refuse with ErrReviewExpired.
	l.mu.Lock()
	l.reviews["tx-late"].ExpiresAt = time.Now().Add(-time.Minute)
	l.mu.Unlock()
	if _, err := l.ApproveTransferReview("tx-late", "op"); !errors.Is(err, ErrReviewExpired) {
		t.Errorf("approve lapsed = %v, want ErrReviewExpired", err)
	}
	rev2, _ := l.GetTransferReview("tx-late")
	if rev2.Status != ReviewStatusExpired {
		t.Errorf("lapsed review status = %q, want expired", rev2.Status)
	}
}

func TestReviewIdempotentReplay(t *testing.T) {
	l := reviewSeed(t, 10000)
	first, err := l.PostTransfer(Transfer{
		ID: "tx-key", From: "payer", To: "payee", AmountCents: 15000,
		IdempotencyKey: "key-1",
	})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	second, err := l.PostTransfer(Transfer{
		ID: "tx-key-retry", From: "payer", To: "payee", AmountCents: 15000,
		IdempotencyKey: "key-1",
	})
	if err != nil {
		t.Fatalf("replay PostTransfer: %v", err)
	}
	if !second.Duplicate {
		t.Error("replay Duplicate = false, want true")
	}
	if second.ReviewStatus != ReviewStatusPending {
		t.Errorf("replay ReviewStatus = %q, want pending_review", second.ReviewStatus)
	}
	if len(second.Entries) != len(first.Entries) {
		t.Fatalf("replay entries = %d, want %d", len(second.Entries), len(first.Entries))
	}
	for i := range second.Entries {
		if second.Entries[i].ID != first.Entries[i].ID {
			t.Errorf("replay entry %d = %q, want %q", i, second.Entries[i].ID, first.Entries[i].ID)
		}
		if !second.Entries[i].PendingReview {
			t.Errorf("replay entry %q lost its pending marker", second.Entries[i].ID)
		}
	}
	// No second review was created: still exactly one, funds frozen once.
	if n := len(l.PendingTransferReviews()); n != 1 {
		t.Errorf("pending reviews = %d, want 1", n)
	}
	if got := l.Available("payer"); got != 100000-15000 {
		t.Errorf("payer available = %d, want %d (frozen once)", got, 100000-15000)
	}
}

func TestReviewReplayAfterApprove(t *testing.T) {
	l := reviewSeed(t, 10000)
	if _, err := l.PostTransfer(Transfer{
		ID: "tx-ka", From: "payer", To: "payee", AmountCents: 15000,
		IdempotencyKey: "key-a",
	}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if _, err := l.ApproveTransferReview("tx-ka", "op"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	replay, err := l.PostTransfer(Transfer{
		ID: "tx-ka-retry", From: "payer", To: "payee", AmountCents: 15000,
		IdempotencyKey: "key-a",
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !replay.Duplicate || replay.ReviewStatus != ReviewStatusApproved {
		t.Errorf("replay = duplicate %v status %q, want duplicate true status approved",
			replay.Duplicate, replay.ReviewStatus)
	}
	if got := l.Balance("payer"); got != 100000-15000 {
		t.Errorf("payer balance = %d, want %d (settled once)", got, 100000-15000)
	}
}

func TestReviewPerAccountThreshold(t *testing.T) {
	l := New(WithReviewThreshold(50000))
	seedBalance(t, l, "payer", 100000)
	seedBalance(t, l, "vip", 100000)
	if err := l.SetReviewThreshold("vip", 5000); err != nil {
		t.Fatalf("SetReviewThreshold: %v", err)
	}
	// vip's per-account threshold (5000) wins over the global (50000).
	r1, err := l.PostTransfer(Transfer{ID: "tx-vip", From: "vip", To: "payee", AmountCents: 6000})
	if err != nil {
		t.Fatalf("PostTransfer vip: %v", err)
	}
	if r1.ReviewStatus != ReviewStatusPending {
		t.Errorf("vip transfer ReviewStatus = %q, want pending_review", r1.ReviewStatus)
	}
	// payer still uses the global threshold.
	r2, err := l.PostTransfer(Transfer{ID: "tx-p", From: "payer", To: "payee", AmountCents: 6000})
	if err != nil {
		t.Fatalf("PostTransfer payer: %v", err)
	}
	if r2.ReviewStatus != "" {
		t.Errorf("payer transfer ReviewStatus = %q, want empty", r2.ReviewStatus)
	}
	// Clearing falls back to the global threshold.
	l.ClearReviewThreshold("vip")
	r3, err := l.PostTransfer(Transfer{ID: "tx-vip2", From: "vip", To: "payee", AmountCents: 6000})
	if err != nil {
		t.Fatalf("PostTransfer vip2: %v", err)
	}
	if r3.ReviewStatus != "" {
		t.Errorf("vip transfer after clear ReviewStatus = %q, want empty", r3.ReviewStatus)
	}
	// Negative thresholds are rejected.
	if err := l.SetReviewThreshold("vip", -1); !errors.Is(err, ErrInvalidReviewThreshold) {
		t.Errorf("SetReviewThreshold(-1) = %v, want ErrInvalidReviewThreshold", err)
	}
	if got, ok := l.ReviewThreshold("vip"); !ok || got != 50000 {
		t.Errorf("ReviewThreshold(vip) = %d, %v; want 50000, true", got, ok)
	}
	// No per-account row: the global threshold applies.
	if got, ok := l.ReviewThreshold("nobody"); !ok || got != 50000 {
		t.Errorf("ReviewThreshold(nobody) = %d, %v; want 50000, true", got, ok)
	}
	// No threshold anywhere: review disabled.
	plain := New()
	if _, ok := plain.ReviewThreshold("nobody"); ok {
		t.Error("ReviewThreshold on unconfigured ledger ok = true, want false")
	}
}

func TestReviewThresholdZeroPerAccount(t *testing.T) {
	l := New() // no global threshold
	seedBalance(t, l, "payer", 100000)
	if err := l.SetReviewThreshold("payer", 0); err != nil {
		t.Fatalf("SetReviewThreshold(0): %v", err)
	}
	// An explicit zero per-account threshold reviews everything.
	r, err := l.PostTransfer(Transfer{ID: "tx-dust", From: "payer", To: "payee", AmountCents: 1})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if r.ReviewStatus != ReviewStatusPending {
		t.Errorf("dust transfer ReviewStatus = %q, want pending_review", r.ReviewStatus)
	}
}

func TestReviewProtectedPayerAvailableCheck(t *testing.T) {
	l := New(WithReviewThreshold(5000), WithOverdraftProtection("payer"))
	seedBalance(t, l, "payer", 100000)
	// An active hold eats most of the available balance.
	if _, _, err := l.Hold(Hold{ID: "h1", Account: "payer", AmountCents: 95000, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	// The transfer passes the overdraft check (100000 >= 9000) but the
	// review freeze cannot cover available: rejected, records nothing.
	versionBefore := l.Version()
	_, err := l.PostTransfer(Transfer{ID: "tx-av", From: "payer", To: "payee", AmountCents: 9000})
	if !errors.Is(err, ErrInsufficientAvailableFunds) {
		t.Errorf("PostTransfer = %v, want ErrInsufficientAvailableFunds", err)
	}
	assertLedgerUntouched(t, l, versionBefore, len(l.Entries()))
}

func TestReviewApproveFrozenPayer(t *testing.T) {
	l := reviewSeed(t, 10000)
	if _, err := l.PostTransfer(Transfer{ID: "tx-fz", From: "payer", To: "payee", AmountCents: 20000}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	l.Freeze("payer")
	if _, err := l.ApproveTransferReview("tx-fz", "op"); !errors.Is(err, ErrAccountFrozen) {
		t.Errorf("approve frozen payer = %v, want ErrAccountFrozen", err)
	}
	// The review stays pending: unfreeze and approve works.
	l.Unfreeze("payer")
	if _, err := l.ApproveTransferReview("tx-fz", "op"); err != nil {
		t.Errorf("approve after unfreeze: %v", err)
	}
}

func TestReviewApproveClosedPeriod(t *testing.T) {
	l := reviewSeed(t, 10000)
	// A backdated transfer in an open period, then close the period.
	ts := time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)
	tr := Transfer{ID: "tx-pd", From: "payer", To: "payee", AmountCents: 20000, CreatedAt: ts}
	if _, err := l.PostTransfer(tr); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if err := l.ClosePeriod("2026-03"); err != nil {
		t.Fatalf("ClosePeriod: %v", err)
	}
	// Approval into a closed period is refused; the review stays pending.
	if _, err := l.ApproveTransferReview("tx-pd", "op"); !errors.Is(err, ErrPeriodClosed) {
		t.Errorf("approve closed period = %v, want ErrPeriodClosed", err)
	}
	rev, _ := l.GetTransferReview("tx-pd")
	if rev.Status != ReviewStatusPending {
		t.Errorf("review status = %q, want still pending", rev.Status)
	}
	// Reopen: approval settles.
	if err := l.ReopenPeriod("2026-03"); err != nil {
		t.Fatalf("ReopenPeriod: %v", err)
	}
	if _, err := l.ApproveTransferReview("tx-pd", "op"); err != nil {
		t.Errorf("approve after reopen: %v", err)
	}
}

func TestReviewApproveOverdraftRecheck(t *testing.T) {
	l := New(WithReviewThreshold(5000), WithOverdraftProtection("payer"))
	seedBalance(t, l, "payer", 100000)
	if _, err := l.PostTransfer(Transfer{ID: "tx-od", From: "payer", To: "payee", AmountCents: 60000}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	// A concurrent direct posting drains the payer below the frozen
	// outflow before approval.
	if _, _, err := l.Post(JournalEntry{ID: "drain", DebitAccount: "escrow", CreditAccount: "payer", AmountCents: 50000}); err != nil {
		t.Fatalf("drain Post: %v", err)
	}
	if _, err := l.ApproveTransferReview("tx-od", "op"); !errors.Is(err, ErrAccountOverdraft) {
		t.Errorf("approve after drain = %v, want ErrAccountOverdraft", err)
	}
	rev, _ := l.GetTransferReview("tx-od")
	if rev.Status != ReviewStatusPending {
		t.Errorf("review status = %q, want still pending", rev.Status)
	}
}

func TestReviewFXTransfer(t *testing.T) {
	l := New(
		WithReviewThreshold(5000),
		WithFXAccount("fx-clearing"),
		WithFXRateOption("USD", "EUR", 108, 100),
	)
	seedBalance(t, l, "payer", 100000)
	receipt, err := l.PostTransfer(Transfer{
		ID: "tx-fx", From: "payer", To: "payee",
		AmountCents: 20000, Currency: "USD", ToCurrency: "EUR",
	})
	if err != nil {
		t.Fatalf("PostTransfer FX: %v", err)
	}
	if receipt.ReviewStatus != ReviewStatusPending {
		t.Fatalf("FX transfer ReviewStatus = %q, want pending_review", receipt.ReviewStatus)
	}
	if len(receipt.Entries) != 2 {
		t.Fatalf("FX review entries = %d, want 2", len(receipt.Entries))
	}
	if receipt.FX == nil || receipt.FX.ConvertedCents != 21600 {
		t.Errorf("FX disclosure = %+v, want converted 21600", receipt.FX)
	}
	// Frozen in the source currency: USD available drops, EUR untouched.
	if got := l.AvailableIn("payer", "USD"); got != 100000-20000 {
		t.Errorf("payer USD available = %d, want %d", got, 100000-20000)
	}
	if got := l.BalanceIn("payee", "EUR"); got != 0 {
		t.Errorf("payee EUR balance = %d, want 0 (not settled)", got)
	}
	approved, err := l.ApproveTransferReview("tx-fx", "op")
	if err != nil {
		t.Fatalf("approve FX: %v", err)
	}
	if approved.FX == nil || approved.FX.ConvertedCents != 21600 {
		t.Errorf("approved FX disclosure = %+v", approved.FX)
	}
	if got := l.BalanceIn("payee", "EUR"); got != 21600 {
		t.Errorf("payee EUR balance = %d, want 21600", got)
	}
	if err := l.VerifyChain(); err != nil {
		t.Errorf("VerifyChain after FX approve: %v", err)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Errorf("VerifyAccountingEquation after FX approve: %v", err)
	}
}

func TestReviewReconcilePendingList(t *testing.T) {
	l := reviewSeed(t, 10000)
	if _, err := l.PostTransfer(Transfer{ID: "tx-r1", From: "payer", To: "payee", AmountCents: 12000}); err != nil {
		t.Fatalf("PostTransfer r1: %v", err)
	}
	if _, err := l.PostTransfer(Transfer{ID: "tx-r2", From: "payer", To: "payee", AmountCents: 13000}); err != nil {
		t.Fatalf("PostTransfer r2: %v", err)
	}
	if _, err := l.ApproveTransferReview("tx-r1", "op"); err != nil {
		t.Fatalf("approve r1: %v", err)
	}
	report := l.Reconcile(time.Now())
	if len(report.PendingReviews) != 1 {
		t.Fatalf("PendingReviews = %d, want 1", len(report.PendingReviews))
	}
	pr := report.PendingReviews[0]
	if pr.TransferID != "tx-r2" || pr.Status != ReviewStatusPending {
		t.Errorf("pending review = %q %q, want tx-r2 pending_review", pr.TransferID, pr.Status)
	}
	if pr.TotalOutflowCents != 13000 {
		t.Errorf("pending TotalOutflowCents = %d, want 13000", pr.TotalOutflowCents)
	}
	if !report.AccountingEquationOK {
		t.Errorf("AccountingEquationOK = false: %s", report.AccountingError)
	}
}

func TestReviewSnapshotRoundTrip(t *testing.T) {
	l := New(WithReviewThreshold(5000), WithReviewExpiry(time.Hour))
	if err := l.SetReviewThreshold("vip", 1000); err != nil {
		t.Fatalf("SetReviewThreshold: %v", err)
	}
	seedBalance(t, l, "payer", 100000)
	seedBalance(t, l, "vip", 100000)
	// One pending, one approved, one rejected review.
	if _, err := l.PostTransfer(Transfer{ID: "tx-pend", From: "payer", To: "payee", AmountCents: 9000, IdempotencyKey: "k-pend"}); err != nil {
		t.Fatalf("PostTransfer pend: %v", err)
	}
	if _, err := l.PostTransfer(Transfer{ID: "tx-appr2", From: "payer", To: "payee", AmountCents: 8000, IdempotencyKey: "k-appr"}); err != nil {
		t.Fatalf("PostTransfer appr: %v", err)
	}
	if _, err := l.ApproveTransferReview("tx-appr2", "op-alice"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := l.PostTransfer(Transfer{ID: "tx-rej2", From: "vip", To: "payee", AmountCents: 2000}); err != nil {
		t.Fatalf("PostTransfer rej: %v", err)
	}
	if _, _, err := l.RejectTransferReview("tx-rej2", "op-bob", "test"); err != nil {
		t.Fatalf("reject: %v", err)
	}

	var buf strings.Builder
	// strings.Builder implements io.Writer.
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	r2, err := ImportSnapshot(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}

	// Registry round-trips with statuses, reviewers, and thresholds.
	for _, id := range []string{"tx-pend", "tx-appr2", "tx-rej2"} {
		orig, ok1 := l.GetTransferReview(id)
		rest, ok2 := r2.GetTransferReview(id)
		if !ok1 || !ok2 {
			t.Fatalf("review %q present: orig %v restored %v", id, ok1, ok2)
		}
		if !reviewsEqual(&orig, &rest) {
			t.Errorf("review %q differs after round-trip:\norig: %+v\nrest: %+v", id, orig, rest)
		}
	}
	// Pending freeze survives: available still deducts on the restore.
	if got := r2.Available("payer"); got != l.Available("payer") {
		t.Errorf("restored available = %d, want %d", got, l.Available("payer"))
	}
	// Balances, chain, and equation survive.
	if got := r2.Balance("payee"); got != l.Balance("payee") {
		t.Errorf("restored payee balance = %d, want %d", got, l.Balance("payee"))
	}
	if err := r2.VerifyChain(); err != nil {
		t.Errorf("restored VerifyChain: %v", err)
	}
	if err := r2.VerifyAccountingEquation(); err != nil {
		t.Errorf("restored VerifyAccountingEquation: %v", err)
	}
	// The restored pending review is still actionable.
	if _, err := r2.ApproveTransferReview("tx-pend", "op-restored"); err != nil {
		t.Errorf("approve on restored ledger: %v", err)
	}
	// Idempotent replay still works after restore.
	replay, err := r2.PostTransfer(Transfer{ID: "tx-appr2-x", From: "payer", To: "payee", AmountCents: 8000, IdempotencyKey: "k-appr"})
	if err != nil {
		t.Fatalf("replay after restore: %v", err)
	}
	if !replay.Duplicate || replay.ReviewStatus != ReviewStatusApproved {
		t.Errorf("replay = duplicate %v status %q, want true/approved", replay.Duplicate, replay.ReviewStatus)
	}
	// Per-account thresholds survive as config.
	if got, ok := r2.ReviewThreshold("vip"); !ok || got != 1000 {
		t.Errorf("restored ReviewThreshold(vip) = %d, %v; want 1000, true", got, ok)
	}
}

func TestReviewIncrementalSnapshotRoundTrip(t *testing.T) {
	l := reviewSeed(t, 10000)
	if _, err := l.PostTransfer(Transfer{ID: "tx-inc", From: "payer", To: "payee", AmountCents: 12000}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	base := l.Version()
	// A replica is a true copy of the primary at the base version: it
	// starts from a full snapshot, so its chain head matches the
	// primary's exactly (entry timestamps included).
	var full strings.Builder
	if err := l.ExportSnapshot(&full); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	replica, err := ImportSnapshot(strings.NewReader(full.String()))
	if err != nil {
		t.Fatalf("replica base import: %v", err)
	}

	if _, err := l.PostTransfer(Transfer{ID: "tx-inc2", From: "payer", To: "payee", AmountCents: 11000}); err != nil {
		t.Fatalf("PostTransfer inc2: %v", err)
	}
	if _, err := l.ApproveTransferReview("tx-inc2", "op"); err != nil {
		t.Fatalf("approve: %v", err)
	}

	var buf strings.Builder
	if err := l.ExportIncrementalSnapshot(&buf, base); err != nil {
		t.Fatalf("ExportIncrementalSnapshot: %v", err)
	}
	if err := replica.ImportIncrementalSnapshot(strings.NewReader(buf.String())); err != nil {
		t.Fatalf("ImportIncrementalSnapshot: %v", err)
	}
	if n := len(replica.PendingTransferReviews()); n != 1 {
		t.Errorf("replica pending reviews = %d, want 1", n)
	}
	rev, ok := replica.GetTransferReview("tx-inc2")
	if !ok || rev.Status != ReviewStatusApproved {
		t.Errorf("replica review tx-inc2 = %+v, %v; want approved", rev, ok)
	}
	if err := replica.VerifyChain(); err != nil {
		t.Errorf("replica VerifyChain: %v", err)
	}
	if got := replica.Available("payer"); got != l.Available("payer") {
		t.Errorf("replica available = %d, want %d", got, l.Available("payer"))
	}
}

func TestParseReviewThresholds(t *testing.T) {
	got, err := ParseReviewThresholds("treasury:1000000,ops:500000")
	if err != nil {
		t.Fatalf("ParseReviewThresholds: %v", err)
	}
	if len(got) != 2 || got[0].Account != "treasury" || got[0].ThresholdCents != 1000000 {
		t.Errorf("parsed = %+v", got)
	}
	for _, raw := range []string{
		"treasury", "treasury:abc", "treasury:-5", ":100", "a:1,a:2", "treasury:1:2",
	} {
		if _, err := ParseReviewThresholds(raw); err == nil {
			t.Errorf("ParseReviewThresholds(%q) = nil error, want error", raw)
		}
	}
	if got, err := ParseReviewThresholds(""); err != nil || got != nil {
		t.Errorf("ParseReviewThresholds(\"\") = %v, %v; want nil, nil", got, err)
	}
}

func TestReviewHoldsCombinedAvailable(t *testing.T) {
	l := reviewSeed(t, 10000)
	if _, _, err := l.Hold(Hold{ID: "h1", Account: "payer", AmountCents: 10000, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if _, err := l.PostTransfer(Transfer{ID: "tx-c", From: "payer", To: "payee", AmountCents: 15000}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	// Holds and review freezes deduct together.
	if got := l.Available("payer"); got != 100000-10000-15000 {
		t.Errorf("payer available = %d, want %d", got, 100000-10000-15000)
	}
	// Releasing the hold leaves only the review freeze.
	if _, err := l.Release("h1"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if got := l.Available("payer"); got != 100000-15000 {
		t.Errorf("payer available after release = %d, want %d", got, 100000-15000)
	}
}

func TestReviewSweeper(t *testing.T) {
	l := New(WithReviewThreshold(5000), WithReviewExpiry(50*time.Millisecond))
	seedBalance(t, l, "payer", 100000)
	if _, err := l.PostTransfer(Transfer{ID: "tx-sw", From: "payer", To: "payee", AmountCents: 9000}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	expiredCh := make(chan int, 4)
	sw := StartReviewSweeper(ctx, l, 20*time.Millisecond, func(expired int) { expiredCh <- expired })
	if sw == nil {
		t.Fatal("StartReviewSweeper returned nil for a positive interval")
	}
	defer sw.Stop()
	deadline := time.Now().Add(3 * time.Second)
	for {
		rev, _ := l.GetTransferReview("tx-sw")
		if rev.Status == ReviewStatusExpired {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("sweeper did not expire the review in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := sw.ExpiredTotal(); got < 1 {
		t.Errorf("sweeper ExpiredTotal = %d, want >= 1", got)
	}
	// A non-positive interval disables the sweeper.
	if sw := StartReviewSweeper(ctx, l, 0, nil); sw != nil {
		t.Error("StartReviewSweeper(0) != nil, want nil")
		sw.Stop()
	}
}

func TestReviewDailyLimitReservedAtCreation(t *testing.T) {
	l := New(WithReviewThreshold(5000), WithDailyLimit("payer", "USD", 15000))
	seedBalance(t, l, "payer", 100000)
	// The daily-limit check keys on the transfer's own timestamp, so pin
	// it (see ledger_daily_limit_test.go for the convention).
	now := time.Now()
	// First review reserves 9000 of the 15000 daily budget.
	if _, err := l.PostTransfer(Transfer{ID: "tx-d1", From: "payer", To: "payee", AmountCents: 9000, CreatedAt: now}); err != nil {
		t.Fatalf("PostTransfer d1: %v", err)
	}
	// A second transfer that would exceed the budget is rejected even
	// though nothing settled yet — the freeze reserved the budget.
	if _, err := l.PostTransfer(Transfer{ID: "tx-d2", From: "payer", To: "payee", AmountCents: 7000, CreatedAt: now}); !errors.Is(err, ErrDailyLimitExceeded) {
		t.Errorf("PostTransfer d2 = %v, want ErrDailyLimitExceeded", err)
	}
	// Approving the first review does not double-count the budget.
	if _, err := l.ApproveTransferReview("tx-d1", "op"); err != nil {
		t.Fatalf("approve d1: %v", err)
	}
	if _, err := l.PostTransfer(Transfer{ID: "tx-d3", From: "payer", To: "payee", AmountCents: 7000, CreatedAt: now}); !errors.Is(err, ErrDailyLimitExceeded) {
		t.Errorf("PostTransfer d3 = %v, want ErrDailyLimitExceeded (9000+7000 > 15000)", err)
	}
}
