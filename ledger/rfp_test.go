package ledger

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func testRfP(ctx context.Context, t *testing.T, l *Ledger, id string) RfP {
	t.Helper()
	r, dup, err := l.CreateRfP(ctx, RfP{
		ID:          id,
		Creditor:    "merchant",
		Debtor:      "customer",
		AmountCents: 5000,
		Currency:    "USD",
		ExpiresAt:   time.Now().Add(time.Hour),
	})
	if err != nil || dup {
		t.Fatalf("CreateRfP(%s): %v dup=%v", id, err, dup)
	}
	return r
}

// TestRfPCreateAndAccept is the core LG-171 test: the creditor issues
// an RfP (zero journal rows, zero chain), the debtor accepts, and the
// payout posts debtor -> creditor atomically.
func TestRfPCreateAndAccept(t *testing.T) {
	ctx := context.Background()
	l := New()
	mustPostOFX(t, l, "fund", "customer", "equity", 10000, "USD", time.Now().Add(-time.Hour))
	v0 := l.version

	r := testRfP(ctx, t, l, "rfp-1")
	if r.Status != RfPStatusIssued {
		t.Fatalf("status = %q, want issued", r.Status)
	}
	if l.version != v0 {
		t.Fatal("CreateRfP must not bump the version")
	}
	if n := len(l.entries); n != 1 {
		t.Fatalf("CreateRfP booked %d entries, want 1 (the funding row only)", n)
	}

	receipt, err := l.AcceptRfP(ctx, "rfp-1", "customer")
	if err != nil {
		t.Fatalf("AcceptRfP: %v", err)
	}
	if receipt.TransferID != "rfp/rfp-1/payout" {
		t.Fatalf("transfer ID = %q, want rfp/rfp-1/payout", receipt.TransferID)
	}
	if l.BalanceIn("customer", "USD") != 5000 {
		t.Fatalf("customer balance = %d, want 5000", l.BalanceIn("customer", "USD"))
	}
	if l.BalanceIn("merchant", "USD") != 5000 {
		t.Fatalf("merchant balance = %d, want 5000", l.BalanceIn("merchant", "USD"))
	}
	// The payout entry carries the RfP reference memo.
	e := l.entries["rfp/rfp-1/payout"]
	if e.Memo != "rfp_of:rfp-1" {
		t.Fatalf("payout memo = %q, want rfp_of:rfp-1", e.Memo)
	}
	got, ok := l.GetRfP("rfp-1")
	if !ok || got.Status != RfPStatusPaid || got.TransferID != "rfp/rfp-1/payout" {
		t.Fatalf("GetRfP after accept: %+v ok=%v", got, ok)
	}
	// Re-accept is idempotent: the original receipt replays.
	dup, err := l.AcceptRfP(ctx, "rfp-1", "customer")
	if err != nil {
		t.Fatalf("re-accept: %v", err)
	}
	if !dup.Duplicate || dup.TransferID != receipt.TransferID {
		t.Fatalf("re-accept did not replay the original receipt: %+v", dup)
	}
	if n := len(l.entries); n != 2 {
		t.Fatalf("re-accept booked a second payout: %d entries", n)
	}
}

func TestRfPCreateValidation(t *testing.T) {
	ctx := context.Background()
	l := New()
	future := time.Now().Add(time.Hour)
	good := RfP{ID: "r1", Creditor: "m", Debtor: "c", AmountCents: 100, Currency: "USD", ExpiresAt: future}
	for name, mutate := range map[string]func(*RfP){
		"empty id":       func(r *RfP) { r.ID = "" },
		"whitespace id":  func(r *RfP) { r.ID = "a b" },
		"empty creditor": func(r *RfP) { r.Creditor = "" },
		"empty debtor":   func(r *RfP) { r.Debtor = "" },
		"same party":     func(r *RfP) { r.Creditor = r.Debtor },
		"zero amount":    func(r *RfP) { r.AmountCents = 0 },
		"negative amount": func(r *RfP) { r.AmountCents = -5 },
		"zero expiry":    func(r *RfP) { r.ExpiresAt = time.Time{} },
		"past expiry":    func(r *RfP) { r.ExpiresAt = time.Now().Add(-time.Second) },
		"bad currency":   func(r *RfP) { r.Currency = "xx" },
	} {
		r := good
		mutate(&r)
		if _, _, err := l.CreateRfP(ctx, r); err == nil {
			t.Fatalf("%s: expected error, got nil", name)
		}
	}
}

func TestRfPCreateIdempotent(t *testing.T) {
	ctx := context.Background()
	l := New()
	first := testRfP(ctx, t, l, "rfp-dup")
	second, dup, err := l.CreateRfP(ctx, RfP{
		ID: "rfp-dup", Creditor: "other", Debtor: "someone-else",
		AmountCents: 99999, Currency: "EUR", ExpiresAt: time.Now().Add(2 * time.Hour),
	})
	if err != nil || !dup {
		t.Fatalf("re-create: %v dup=%v", err, dup)
	}
	// The replay returns the ORIGINAL record, not the new arguments.
	if second.AmountCents != first.AmountCents || second.Currency != first.Currency {
		t.Fatalf("replay did not return the original record: %+v", second)
	}
}

func TestRfPReject(t *testing.T) {
	ctx := context.Background()
	l := New()
	testRfP(ctx, t, l, "rfp-r")
	r, err := l.RejectRfP(ctx, "rfp-r", "customer")
	if err != nil {
		t.Fatalf("RejectRfP: %v", err)
	}
	if r.Status != RfPStatusRejected {
		t.Fatalf("status = %q, want rejected", r.Status)
	}
	if _, err := l.RejectRfP(ctx, "rfp-r", "customer"); !errors.Is(err, ErrRfPNotIssued) {
		t.Fatalf("double reject: %v, want ErrRfPNotIssued", err)
	}
	if _, err := l.AcceptRfP(ctx, "rfp-r", "customer"); !errors.Is(err, ErrRfPNotIssued) {
		t.Fatalf("accept after reject: %v, want ErrRfPNotIssued", err)
	}
	if _, err := l.RejectRfP(ctx, "nope", "customer"); !errors.Is(err, ErrRfPNotFound) {
		t.Fatalf("reject unknown: %v, want ErrRfPNotFound", err)
	}
	if _, err := l.AcceptRfP(ctx, "nope", "customer"); !errors.Is(err, ErrRfPNotFound) {
		t.Fatalf("accept unknown: %v, want ErrRfPNotFound", err)
	}
}

// TestRfPExpiry covers both the lazy predicate (accept sees the
// expiry) and the explicit sweep.
func TestRfPExpiry(t *testing.T) {
	ctx := context.Background()
	l := New()
	mustPostOFX(t, l, "fund", "customer", "equity", 10000, "USD", time.Now().Add(-time.Hour))
	testRfP(ctx, t, l, "rfp-e1")
	testRfP(ctx, t, l, "rfp-e2")

	if n := l.ExpireRfPs(time.Now().Add(2 * time.Hour)); n != 2 {
		t.Fatalf("ExpireRfPs = %d, want 2", n)
	}
	if n := l.ExpireRfPs(time.Now().Add(2 * time.Hour)); n != 0 {
		t.Fatalf("second sweep = %d, want 0 (terminal rows untouched)", n)
	}
	got, ok := l.GetRfP("rfp-e1")
	if !ok || got.Status != RfPStatusExpired {
		t.Fatalf("GetRfP after sweep: %+v ok=%v", got, ok)
	}
	if _, err := l.AcceptRfP(ctx, "rfp-e1", "customer"); !errors.Is(err, ErrRfPNotIssued) {
		t.Fatalf("accept swept-expired: %v, want ErrRfPNotIssued", err)
	}

	// Lazy expiry: no sweep — the accept itself observes the deadline
	// via the lazy predicate (rfpExpiredLocked), marks the record
	// expired, and audits rfp_expired.
	l2 := New()
	mustPostOFX(t, l2, "fund", "customer", "equity", 10000, "USD", time.Now().Add(-time.Hour))
	if _, _, err := l2.CreateRfP(ctx, RfP{
		ID: "rfp-lazy", Creditor: "merchant", Debtor: "customer",
		AmountCents: 100, Currency: "USD", ExpiresAt: time.Now().Add(50 * time.Millisecond),
	}); err != nil {
		t.Fatalf("CreateRfP: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	if _, err := l2.AcceptRfP(ctx, "rfp-lazy", "customer"); !errors.Is(err, ErrRfPExpired) {
		t.Fatalf("lazy accept past expiry: %v, want ErrRfPExpired", err)
	}
	got2, ok := l2.GetRfP("rfp-lazy")
	if !ok || got2.Status != RfPStatusExpired {
		t.Fatalf("lazy expiry must persist the expired mark: %+v", got2)
	}
	if _, err := l2.RejectRfP(ctx, "rfp-lazy", "customer"); !errors.Is(err, ErrRfPNotIssued) {
		t.Fatalf("reject expired: %v, want ErrRfPNotIssued", err)
	}
}

// TestRfPAcceptFailureKeepsIssued pins the atomicity contract: when
// the payout cannot post (frozen debtor), the RfP stays issued and no
// money moves.
func TestRfPAcceptFailureKeepsIssued(t *testing.T) {
	ctx := context.Background()
	l := New()
	mustPostOFX(t, l, "fund", "customer", "equity", 10000, "USD", time.Now().Add(-time.Hour))
	l.Freeze("customer")
	testRfP(ctx, t, l, "rfp-f")
	if _, err := l.AcceptRfP(ctx, "rfp-f", "customer"); !errors.Is(err, ErrAccountFrozen) {
		t.Fatalf("accept on frozen debtor: %v, want ErrAccountFrozen", err)
	}
	got, ok := l.GetRfP("rfp-f")
	if !ok || got.Status != RfPStatusIssued {
		t.Fatalf("failed accept must leave the RfP issued: %+v", got)
	}
	if l.BalanceIn("customer", "USD") != 10000 || l.BalanceIn("merchant", "USD") != 0 {
		t.Fatal("failed accept moved money")
	}
	// After unfreezing the same RfP pays normally.
	l.Unfreeze("customer")
	if _, err := l.AcceptRfP(ctx, "rfp-f", "customer"); err != nil {
		t.Fatalf("accept after unfreeze: %v", err)
	}
}

// TestRfPMemoSearchable verifies the payout carries the rfp_of memo
// convention so operators can find payouts by their RfP.
func TestRfPMemoSearchable(t *testing.T) {
	ctx := context.Background()
	l := New()
	mustPostOFX(t, l, "fund", "customer", "equity", 10000, "USD", time.Now().Add(-time.Hour))
	testRfP(ctx, t, l, "rfp-m")
	if _, err := l.AcceptRfP(ctx, "rfp-m", "customer"); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, id := range l.byAccount["merchant"] {
		if strings.Contains(l.entries[id].Memo, "rfp_of:rfp-m") {
			found = true
		}
	}
	if !found {
		t.Fatal("payout entry not findable via the rfp_of memo")
	}
}
