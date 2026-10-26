package ledger

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func TestPartialFreezeBasicAndAvailable(t *testing.T) {
	l := New()
	seedBalance(t, l, "alice", 100000)

	// Validation first: nothing freezes on a bad request.
	if err := l.FreezeAmount("", 1000); !errors.Is(err, ErrPartialFreezeEmptyAccount) {
		t.Errorf("empty account: err = %v, want ErrPartialFreezeEmptyAccount", err)
	}
	if err := l.FreezeAmount("alice", 0); !errors.Is(err, ErrPartialFreezeNonPositiveAmount) {
		t.Errorf("zero amount: err = %v, want ErrPartialFreezeNonPositiveAmount", err)
	}
	if err := l.FreezeAmount("alice", -500); !errors.Is(err, ErrPartialFreezeNonPositiveAmount) {
		t.Errorf("negative amount: err = %v, want ErrPartialFreezeNonPositiveAmount", err)
	}
	// Fail-closed on coverage: more than available freezes nothing.
	if err := l.FreezeAmount("alice", 100001); !errors.Is(err, ErrPartialFreezeExceedsAvailable) {
		t.Errorf("over-available: err = %v, want ErrPartialFreezeExceedsAvailable", err)
	}
	if got := l.PartialFrozenAmount("alice"); got != 0 {
		t.Errorf("frozen after rejected freeze = %d, want 0", got)
	}

	versionBefore := l.Version()
	if err := l.FreezeAmount("alice", 30000); err != nil {
		t.Fatalf("FreezeAmount: %v", err)
	}
	// Off-journal: net balance untouched, version untouched, but
	// available drops by exactly the frozen amount.
	if got := l.Balance("alice"); got != 100000 {
		t.Errorf("balance = %d, want 100000 (freeze moves no money)", got)
	}
	if got := l.Version(); got != versionBefore {
		t.Errorf("version = %d, want %d (freeze bumps nothing)", got, versionBefore)
	}
	if got := l.Available("alice"); got != 70000 {
		t.Errorf("available = %d, want 70000", got)
	}
	if got := l.PartialFrozenAmount("alice"); got != 30000 {
		t.Errorf("frozen = %d, want 30000", got)
	}
	// Stacking: a second freeze adds, still bounded by available.
	if err := l.FreezeAmount("alice", 70001); !errors.Is(err, ErrPartialFreezeExceedsAvailable) {
		t.Errorf("stack over available: err = %v, want ErrPartialFreezeExceedsAvailable", err)
	}
	if err := l.FreezeAmount("alice", 10000); err != nil {
		t.Fatalf("second FreezeAmount: %v", err)
	}
	if got := l.Available("alice"); got != 60000 {
		t.Errorf("available after stack = %d, want 60000", got)
	}
}

func TestPartialFreezeUnfreezePartialAndOverRelease(t *testing.T) {
	l := New()
	seedBalance(t, l, "alice", 100000)
	if err := l.FreezeAmount("alice", 30000); err != nil {
		t.Fatalf("FreezeAmount: %v", err)
	}
	if err := l.UnfreezeAmount("alice", 10000); err != nil {
		t.Fatalf("partial UnfreezeAmount: %v", err)
	}
	if got := l.PartialFrozenAmount("alice"); got != 20000 {
		t.Errorf("frozen = %d, want 20000", got)
	}
	if got := l.Available("alice"); got != 80000 {
		t.Errorf("available = %d, want 80000", got)
	}
	// Over-release is rejected and releases nothing.
	if err := l.UnfreezeAmount("alice", 20001); !errors.Is(err, ErrPartialFreezeExceedsFrozen) {
		t.Errorf("over-release: err = %v, want ErrPartialFreezeExceedsFrozen", err)
	}
	if got := l.PartialFrozenAmount("alice"); got != 20000 {
		t.Errorf("frozen after over-release = %d, want 20000", got)
	}
	if err := l.UnfreezeAmount("alice", 20000); err != nil {
		t.Fatalf("full UnfreezeAmount: %v", err)
	}
	if got := l.PartialFrozenAmount("alice"); got != 0 {
		t.Errorf("frozen after full release = %d, want 0", got)
	}
	if got := l.Available("alice"); got != 100000 {
		t.Errorf("available after full release = %d, want 100000", got)
	}
	// Releasing from nothing fails the same way.
	if err := l.UnfreezeAmount("alice", 1); !errors.Is(err, ErrPartialFreezeExceedsFrozen) {
		t.Errorf("release from zero: err = %v, want ErrPartialFreezeExceedsFrozen", err)
	}
}

func TestPartialFreezeProtectedPostingGate(t *testing.T) {
	l := New()
	seedBalance(t, l, "alice", 100000)
	l.EnableOverdraftProtection("alice")
	if err := l.FreezeAmount("alice", 30000); err != nil {
		t.Fatalf("FreezeAmount: %v", err)
	}
	// Spending exactly down to the frozen amount is allowed: the
	// remaining available balance still posts.
	if _, _, err := l.Post(JournalEntry{ID: "spend-ok", DebitAccount: "bob", CreditAccount: "alice", AmountCents: 70000}); err != nil {
		t.Fatalf("post to the floor: %v", err)
	}
	if got := l.Balance("alice"); got != 30000 {
		t.Errorf("balance after floor post = %d, want 30000", got)
	}
	// One cent into the frozen amount is blocked, and books nothing.
	versionBefore := l.Version()
	if _, _, err := l.Post(JournalEntry{ID: "spend-frozen", DebitAccount: "bob", CreditAccount: "alice", AmountCents: 1}); !errors.Is(err, ErrPartialFreezeBlockedPosting) {
		t.Errorf("post into frozen funds: err = %v, want ErrPartialFreezeBlockedPosting", err)
	}
	if got := l.Version(); got != versionBefore {
		t.Errorf("version after blocked post = %d, want %d", got, versionBefore)
	}
	// Inflows into the partially frozen account still post.
	if _, _, err := l.Post(JournalEntry{ID: "inflow-ok", DebitAccount: "alice", CreditAccount: "carol", AmountCents: 5000}); err != nil {
		t.Fatalf("inflow post: %v", err)
	}
	if got := l.Balance("alice"); got != 35000 {
		t.Errorf("balance after inflow = %d, want 35000", got)
	}
}

func TestPartialFreezeGateDoesNotApplyUnprotected(t *testing.T) {
	// Unprotected accounts keep the sign convention: the freeze
	// deducts from Available (advisory, like a hold) but does not
	// raise a posting floor, because no floor exists to raise.
	l := New()
	seedBalance(t, l, "alice", 100000)
	if err := l.FreezeAmount("alice", 30000); err != nil {
		t.Fatalf("FreezeAmount: %v", err)
	}
	if _, _, err := l.Post(JournalEntry{ID: "spend-unprotected", DebitAccount: "bob", CreditAccount: "alice", AmountCents: 80000}); err != nil {
		t.Fatalf("unprotected post: %v", err)
	}
	if got := l.Balance("alice"); got != 20000 {
		t.Errorf("balance = %d, want 20000", got)
	}
}

func TestPartialFreezeWithHoldNoDoubleCount(t *testing.T) {
	l := New()
	seedBalance(t, l, "alice", 100000)
	if _, _, err := l.Hold(Hold{ID: "h1", Account: "alice", AmountCents: 20000, Currency: "USD", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if err := l.FreezeAmount("alice", 30000); err != nil {
		t.Fatalf("FreezeAmount: %v", err)
	}
	// Available deducts each reservation once: 100000 - 20000 hold
	// - 30000 partial freeze = 50000. A freeze that fits only if
	// one of the two were ignored must fail.
	if got := l.Available("alice"); got != 50000 {
		t.Errorf("available = %d, want 50000 (hold + partial freeze, each once)", got)
	}
	if err := l.FreezeAmount("alice", 50001); !errors.Is(err, ErrPartialFreezeExceedsAvailable) {
		t.Errorf("freeze over joint available: err = %v, want ErrPartialFreezeExceedsAvailable", err)
	}
	if err := l.FreezeAmount("alice", 50000); err != nil {
		t.Fatalf("freeze exactly the joint available: %v", err)
	}
	if got := l.Available("alice"); got != 0 {
		t.Errorf("available after full reservation = %d, want 0", got)
	}
}

func TestPartialFreezeWholeFreezeInteraction(t *testing.T) {
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
	// A new partial freeze on a fully frozen account is refused:
	// the whole-account stop already covers the account.
	l.Freeze("alice")
	if err := l.FreezeAmount("alice", 1000); !errors.Is(err, ErrAccountFrozen) {
		t.Errorf("freeze on frozen account: err = %v, want ErrAccountFrozen", err)
	}
	// Releasing is exempt from the stop, like Hold release: freeing
	// funds is never blocked.
	if err := l.UnfreezeAmount("alice", 10000); err != nil {
		t.Fatalf("UnfreezeAmount on frozen account: %v", err)
	}
	if got := l.PartialFrozenAmount("alice"); got != 20000 {
		t.Errorf("frozen after exempt release = %d, want 20000", got)
	}
	// A full unfreeze clears the remaining partial freeze too.
	l.Unfreeze("alice")
	if l.IsFrozen("alice") {
		t.Error("account still frozen after Unfreeze")
	}
	if got := l.PartialFrozenAmount("alice"); got != 0 {
		t.Errorf("frozen after Unfreeze = %d, want 0 (full unfreeze clears partial)", got)
	}
	if got := l.Available("alice"); got != 100000 {
		t.Errorf("available after Unfreeze = %d, want 100000", got)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	byOp := auditEventsByOp(t, dir)
	if got := byOp["partial_freeze"]; len(got) != 1 {
		t.Errorf("partial_freeze events = %d, want 1", len(got))
	}
	released := byOp["partial_freeze_released"]
	if len(released) != 2 {
		t.Fatalf("partial_freeze_released events = %d, want 2 (manual + account_unfrozen)", len(released))
	}
	if released[1].Details["reason"] != "account_unfrozen" {
		t.Errorf("unfreeze release event details = %+v, want reason account_unfrozen", released[1].Details)
	}
}

func TestPartialFreezeMultiCurrencyIsolation(t *testing.T) {
	l := New()
	seedBalance(t, l, "alice", 100000)
	seedBalanceIn(t, l, "alice", 50000, "EUR")
	if err := l.FreezeAmountIn("alice", "EUR", 10000); err != nil {
		t.Fatalf("FreezeAmountIn EUR: %v", err)
	}
	if got := l.AvailableIn("alice", "EUR"); got != 40000 {
		t.Errorf("EUR available = %d, want 40000", got)
	}
	if got := l.Available("alice"); got != 100000 {
		t.Errorf("USD available = %d, want 100000 (EUR freeze isolated)", got)
	}
	if got := l.PartialFrozenAmountIn("alice", "EUR"); got != 10000 {
		t.Errorf("EUR frozen = %d, want 10000", got)
	}
	if got := l.PartialFrozenAmount("alice"); got != 0 {
		t.Errorf("USD frozen = %d, want 0", got)
	}
}

func TestPartialFreezeSnapshotAndReconcile(t *testing.T) {
	l := New()
	seedBalance(t, l, "alice", 100000)
	seedBalanceIn(t, l, "alice", 50000, "EUR")
	if err := l.FreezeAmount("alice", 30000); err != nil {
		t.Fatalf("FreezeAmount USD: %v", err)
	}
	if err := l.FreezeAmountIn("alice", "EUR", 10000); err != nil {
		t.Fatalf("FreezeAmountIn EUR: %v", err)
	}

	// Reconcile discloses both rows, sorted by (account, currency).
	report := l.Reconcile(time.Now())
	if len(report.PartialFrozen) != 2 {
		t.Fatalf("reconcile partial rows = %+v, want 2", report.PartialFrozen)
	}
	if report.PartialFrozen[0].Currency != "EUR" || report.PartialFrozen[0].AmountCents != 10000 ||
		report.PartialFrozen[1].Currency != "USD" || report.PartialFrozen[1].AmountCents != 30000 {
		t.Errorf("reconcile partial rows = %+v, want EUR 10000 then USD 30000", report.PartialFrozen)
	}
	if len(report.FrozenAccounts) != 0 {
		t.Errorf("reconcile frozen accounts = %v, want none (partial is not a whole-account stop)", report.FrozenAccounts)
	}

	// Snapshot round-trip preserves the freezes and the available
	// figures they imply.
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if got := restored.PartialFrozenAmount("alice"); got != 30000 {
		t.Errorf("restored USD frozen = %d, want 30000", got)
	}
	if got := restored.PartialFrozenAmountIn("alice", "EUR"); got != 10000 {
		t.Errorf("restored EUR frozen = %d, want 10000", got)
	}
	if got := restored.Available("alice"); got != 70000 {
		t.Errorf("restored USD available = %d, want 70000", got)
	}
	if got := restored.AvailableIn("alice", "EUR"); got != 40000 {
		t.Errorf("restored EUR available = %d, want 40000", got)
	}

}
