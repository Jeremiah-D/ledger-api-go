package ledger

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func amendPartialFreezeSeed(t *testing.T, account AccountID, balance, frozen int64) *Ledger {
	t.Helper()
	l := New()
	seedBalance(t, l, account, balance)
	if err := l.FreezeAmount(account, frozen); err != nil {
		t.Fatalf("FreezeAmount: %v", err)
	}
	return l
}

func TestAmendPartialFreezeAmountSuccess(t *testing.T) {
	l := amendPartialFreezeSeed(t, "alice", 100000, 30000)
	versionBefore := l.Version()
	// Growing the reservation tightens Available by the same
	// amount; shrinking it loosens Available again. The
	// amendment moves the frozen amount and nothing else.
	if err := l.AmendPartialFreezeAmount("alice", "", 50000); err != nil {
		t.Fatalf("AmendPartialFreezeAmount up: %v", err)
	}
	if got := l.PartialFrozenAmount("alice"); got != 50000 {
		t.Errorf("frozen after grow = %d, want 50000", got)
	}
	if got := l.Available("alice"); got != 50000 {
		t.Errorf("available after grow = %d, want 50000", got)
	}
	if err := l.AmendPartialFreezeAmount("alice", "", 20000); err != nil {
		t.Fatalf("AmendPartialFreezeAmount down: %v", err)
	}
	if got := l.PartialFrozenAmount("alice"); got != 20000 {
		t.Errorf("frozen after shrink = %d, want 20000", got)
	}
	if got := l.Available("alice"); got != 80000 {
		t.Errorf("available after shrink = %d, want 80000", got)
	}
	if l.Version() != versionBefore {
		t.Error("version moved: amendment is off-journal")
	}
}

func TestAmendPartialFreezeAmountZeroClears(t *testing.T) {
	l := amendPartialFreezeSeed(t, "alice", 100000, 30000)
	if err := l.AmendPartialFreezeAmount("alice", "", 0); err != nil {
		t.Fatalf("AmendPartialFreezeAmount zero: %v", err)
	}
	if got := l.PartialFrozenAmount("alice"); got != 0 {
		t.Errorf("frozen after zero amend = %d, want 0", got)
	}
	if got := l.Available("alice"); got != 100000 {
		t.Errorf("available after zero amend = %d, want 100000", got)
	}
	// The record is gone, not dangling: a fresh freeze works,
	// and a further amend finds nothing to amend.
	if err := l.FreezeAmount("alice", 10000); err != nil {
		t.Fatalf("re-freeze after zero amend: %v", err)
	}
	if err := l.UnfreezeAmount("alice", 10000); err != nil {
		t.Fatalf("release re-frozen amount: %v", err)
	}
	if err := l.AmendPartialFreezeAmount("alice", "", 5000); !errors.Is(err, ErrPartialFreezeNotFound) {
		t.Errorf("amend after clear: err = %v, want ErrPartialFreezeNotFound", err)
	}
}

func TestAmendPartialFreezeAmountRejections(t *testing.T) {
	// No freeze record at all.
	l := New()
	seedBalance(t, l, "alice", 100000)
	if err := l.AmendPartialFreezeAmount("alice", "", 5000); !errors.Is(err, ErrPartialFreezeNotFound) {
		t.Errorf("never frozen: err = %v, want ErrPartialFreezeNotFound", err)
	}

	// Negative amounts fail, and the original amount is
	// untouched.
	ln := amendPartialFreezeSeed(t, "alice", 100000, 30000)
	if err := ln.AmendPartialFreezeAmount("alice", "", -1); !errors.Is(err, ErrPartialFreezeAmountNegative) {
		t.Errorf("negative: err = %v, want ErrPartialFreezeAmountNegative", err)
	}
	if got := ln.PartialFrozenAmount("alice"); got != 30000 {
		t.Errorf("negative: frozen moved to %d, want 30000", got)
	}

	// An increase past the available-before-partial figure
	// fails, and the original amount is untouched; exactly
	// the whole available figure succeeds (boundary).
	lo := amendPartialFreezeSeed(t, "alice", 100000, 30000)
	if err := lo.AmendPartialFreezeAmount("alice", "", 100001); !errors.Is(err, ErrPartialFreezeExceedsAvailable) {
		t.Errorf("over-available: err = %v, want ErrPartialFreezeExceedsAvailable", err)
	}
	if got := lo.PartialFrozenAmount("alice"); got != 30000 {
		t.Errorf("over-available: frozen moved to %d, want 30000", got)
	}
	if err := lo.AmendPartialFreezeAmount("alice", "", 100000); err != nil {
		t.Errorf("at available boundary: err = %v, want nil", err)
	}
	if got := lo.Available("alice"); got != 0 {
		t.Errorf("at available boundary: available = %d, want 0", got)
	}

	// A pair carrying an expiry refuses amendment: the
	// deadline releases the whole amount, so revising the
	// amount under it would make the release ambiguous.
	le := amendPartialFreezeSeed(t, "alice", 100000, 30000)
	if err := le.SetPartialFreezeExpiry("alice", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("SetPartialFreezeExpiry: %v", err)
	}
	if err := le.AmendPartialFreezeAmount("alice", "", 50000); !errors.Is(err, ErrPartialFreezeExpirySet) {
		t.Errorf("expiry set: err = %v, want ErrPartialFreezeExpirySet", err)
	}
	if got := le.PartialFrozenAmount("alice"); got != 30000 {
		t.Errorf("expiry set: frozen moved to %d, want 30000", got)
	}
	// Clearing the deadline re-opens amendment.
	if err := le.ClearPartialFreezeExpiry("alice"); err != nil {
		t.Fatalf("ClearPartialFreezeExpiry: %v", err)
	}
	if err := le.AmendPartialFreezeAmount("alice", "", 50000); err != nil {
		t.Errorf("after expiry cleared: err = %v, want nil", err)
	}
}

func TestAmendPartialFreezeAmountPostingGateReadsLiveAmount(t *testing.T) {
	l := New()
	seedBalance(t, l, "alice", 100000)
	l.EnableOverdraftProtection("alice")
	if err := l.FreezeAmount("alice", 30000); err != nil {
		t.Fatalf("FreezeAmount: %v", err)
	}
	// Tightening the freeze to 90000 moves the posting floor
	// with it: spending down to 90000 is allowed, one cent
	// further is blocked — the gate reads the amended amount,
	// not the amount frozen at freeze time.
	if err := l.AmendPartialFreezeAmount("alice", "", 90000); err != nil {
		t.Fatalf("AmendPartialFreezeAmount: %v", err)
	}
	if _, _, err := l.Post(JournalEntry{ID: "spend-ok", DebitAccount: "bob", CreditAccount: "alice", AmountCents: 10000}); err != nil {
		t.Fatalf("post to the amended floor: %v", err)
	}
	if got := l.Balance("alice"); got != 90000 {
		t.Fatalf("balance after floor post = %d, want 90000", got)
	}
	if _, _, err := l.Post(JournalEntry{ID: "spend-frozen", DebitAccount: "bob", CreditAccount: "alice", AmountCents: 1}); !errors.Is(err, ErrPartialFreezeBlockedPosting) {
		t.Errorf("post into amended frozen funds: err = %v, want ErrPartialFreezeBlockedPosting", err)
	}
}

func TestAmendPartialFreezeAmountAudit(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	seedBalance(t, l, "alice", 100000)
	if err := l.FreezeAmount("alice", 30000); err != nil {
		t.Fatalf("FreezeAmount: %v", err)
	}
	ctx := ContextWithActor(context.Background(), "ops-bob")
	if err := l.AmendPartialFreezeAmountCtx(ctx, "alice", "", 50000); err != nil {
		t.Fatalf("AmendPartialFreezeAmountCtx: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["partial_freeze_amount_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
	if events[0].Actor != "ops-bob" {
		t.Errorf("event actor = %q, want ops-bob", events[0].Actor)
	}
	d := events[0].Details
	if d["currency"] != "USD" || d["previous_amount_cents"] != float64(30000) ||
		d["amount_cents"] != float64(50000) {
		t.Errorf("event details = %+v, want USD 30000 -> 50000", d)
	}
}

func TestAmendPartialFreezeAmountSnapshotRoundTrip(t *testing.T) {
	l := amendPartialFreezeSeed(t, "alice", 100000, 30000)
	if err := l.AmendPartialFreezeAmount("alice", "", 45000); err != nil {
		t.Fatalf("AmendPartialFreezeAmount: %v", err)
	}
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	l2, err := ImportSnapshot(&buf)
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if got := l2.PartialFrozenAmount("alice"); got != 45000 {
		t.Errorf("restored frozen = %d, want amended 45000", got)
	}
}
