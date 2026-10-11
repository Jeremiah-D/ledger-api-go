package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// postDated posts a single entry with an explicit timestamp, for tests
// that need postings inside a specific accounting period.
func postDated(t *testing.T, l *Ledger, id, debit, credit string, cents int64, currency string, at time.Time) {
	t.Helper()
	if _, _, err := l.Post(JournalEntry{
		ID:           id,
		DebitAccount: AccountID(debit),
		CreditAccount: AccountID(credit),
		AmountCents:  cents,
		Currency:     currency,
		CreatedAt:    at,
	}); err != nil {
		t.Fatalf("Post %s: %v", id, err)
	}
}

func sept2026() time.Time { return time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC) }

func TestSetClosingAccountsValidation(t *testing.T) {
	l := New()

	// Clearing when nothing is configured is a no-op success.
	if err := l.SetClosingAccounts(nil, ""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	pl, retained := l.ClosingAccounts()
	if len(pl) != 0 || retained != "" {
		t.Fatalf("clear: got %v/%q, want empty", pl, retained)
	}

	// P&L accounts without a retained-earnings account: misconfigured.
	if err := l.SetClosingAccounts([]AccountID{"revenue"}, ""); !errors.Is(err, ErrClosingNotConfigured) {
		t.Fatalf("pl without retained: got %v, want ErrClosingNotConfigured", err)
	}

	// Retained earnings listed among the P&L accounts: rejected.
	if err := l.SetClosingAccounts([]AccountID{"revenue", "retained"}, "retained"); !errors.Is(err, ErrClosingRetainedInPLAccounts) {
		t.Fatalf("retained in pl: got %v, want ErrClosingRetainedInPLAccounts", err)
	}

	// Empty account name: rejected.
	if err := l.SetClosingAccounts([]AccountID{""}, "retained"); !errors.Is(err, ErrClosingEmptyAccount) {
		t.Fatalf("empty pl account: got %v, want ErrClosingEmptyAccount", err)
	}

	// Happy path: duplicates collapse, getter sorts.
	if err := l.SetClosingAccounts([]AccountID{"expenses", "revenue", "expenses"}, "retained"); err != nil {
		t.Fatalf("set: %v", err)
	}
	pl, retained = l.ClosingAccounts()
	if len(pl) != 2 || pl[0] != "expenses" || pl[1] != "revenue" || retained != "retained" {
		t.Fatalf("getter: got %v/%q", pl, retained)
	}

	// A failed validation leaves the previous configuration in place.
	if err := l.SetClosingAccounts([]AccountID{"revenue", "retained"}, "retained"); err == nil {
		t.Fatal("expected validation error")
	}
	pl, retained = l.ClosingAccounts()
	if len(pl) != 2 || retained != "retained" {
		t.Fatalf("failed set changed config: %v/%q", pl, retained)
	}
}

func TestClosePeriodWithClosing(t *testing.T) {
	l := New()
	if err := l.SetClosingAccounts([]AccountID{"revenue", "expenses"}, "retained"); err != nil {
		t.Fatalf("SetClosingAccounts: %v", err)
	}

	// Revenue: credit balance -500. Expenses: debit balance +200.
	postDated(t, l, "rev-1", "cash", "revenue", 500, "USD", sept2026())
	postDated(t, l, "exp-1", "expenses", "cash", 200, "USD", sept2026())
	versionBefore := l.Version()

	receipt, err := l.ClosePeriodWithClosing("2026-09")
	if err != nil {
		t.Fatalf("ClosePeriodWithClosing: %v", err)
	}
	if !receipt.Closed || receipt.AlreadyClosed {
		t.Fatalf("receipt = %+v, want Closed=true AlreadyClosed=false", receipt)
	}
	if len(receipt.Entries) != 2 {
		t.Fatalf("closing entries = %d, want 2", len(receipt.Entries))
	}

	// P&L accounts are zeroed; retained earnings holds the net (-500 + 200 = -300).
	if b := l.BalanceIn("revenue", "USD"); b != 0 {
		t.Fatalf("revenue balance = %d, want 0", b)
	}
	if b := l.BalanceIn("expenses", "USD"); b != 0 {
		t.Fatalf("expenses balance = %d, want 0", b)
	}
	if b := l.BalanceIn("retained", "USD"); b != -300 {
		t.Fatalf("retained balance = %d, want -300", b)
	}

	// Closing entries land on the next month's first instant, carry the
	// deterministic ID prefix and the closing memo.
	wantAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	seen := map[string]bool{}
	for _, e := range receipt.Entries {
		if !e.CreatedAt.Equal(wantAt) {
			t.Fatalf("entry %s at %v, want %v", e.ID, e.CreatedAt, wantAt)
		}
		if !strings.HasPrefix(e.ID, "2026-09/closing/") {
			t.Fatalf("entry ID %q lacks the closing prefix", e.ID)
		}
		if e.Memo != "closing:2026-09" {
			t.Fatalf("entry %s memo = %q, want closing:2026-09", e.ID, e.Memo)
		}
		if seen[e.ID] {
			t.Fatalf("duplicate closing entry ID %q", e.ID)
		}
		seen[e.ID] = true
	}
	// Commit order: account sorted, then currency sorted.
	if receipt.Entries[0].ID != "2026-09/closing/expenses/USD" ||
		receipt.Entries[1].ID != "2026-09/closing/revenue/USD" {
		t.Fatalf("commit order = %q, %q", receipt.Entries[0].ID, receipt.Entries[1].ID)
	}

	// Two closing entries bumped the version by two; the chain stayed in
	// step; the accounting equation holds over the closing entries.
	if got := l.Version(); got != versionBefore+2 {
		t.Fatalf("version = %d, want %d", got, versionBefore+2)
	}
	if _, links := l.ChainHead(); links != l.Version() {
		t.Fatalf("chain links = %d, version = %d", links, l.Version())
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Fatalf("VerifyAccountingEquation: %v", err)
	}
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}

	// The period is locked: a backdated posting into September is
	// rejected, and the reconcile report covers the closing entries.
	if !l.IsPeriodClosed("2026-09") {
		t.Fatal("2026-09 should be closed")
	}
	report := l.Reconcile(time.Now())
	if !report.AccountingEquationOK {
		t.Fatalf("reconcile equation not OK: %s", report.AccountingError)
	}
	if len(report.Discrepancies) != 0 {
		t.Fatalf("reconcile discrepancies = %v", len(report.Discrepancies))
	}
	for _, tb := range report.TrialBalances {
		if (tb.Account == "revenue" || tb.Account == "expenses") && tb.NetBalance != 0 {
			t.Fatalf("reconcile: P&L account %s net = %d, want 0", tb.Account, tb.NetBalance)
		}
	}

	// Memo query finds the closing entries (LG-38 integration).
	page, _, err := l.ListEntriesFiltered(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), "", 100, "closing:2026-09", "")
	if err != nil {
		t.Fatalf("ListEntriesFiltered: %v", err)
	}
	if len(page) != 2 {
		t.Fatalf("memo query found %d entries, want 2", len(page))
	}
}

func TestClosePeriodWithClosingIdempotent(t *testing.T) {
	l := New()
	if err := l.SetClosingAccounts([]AccountID{"revenue"}, "retained"); err != nil {
		t.Fatal(err)
	}
	postDated(t, l, "rev-1", "cash", "revenue", 500, "USD", sept2026())

	first, err := l.ClosePeriodWithClosing("2026-09")
	if err != nil {
		t.Fatalf("first close: %v", err)
	}
	versionAfterFirst := l.Version()

	// Re-closing is a no-op success: no new entries, no version bump.
	second, err := l.ClosePeriodWithClosing("2026-09")
	if err != nil {
		t.Fatalf("second close: %v", err)
	}
	if !second.AlreadyClosed || len(second.Entries) != 0 {
		t.Fatalf("second close = %+v, want AlreadyClosed with no entries", second)
	}
	if got := l.Version(); got != versionAfterFirst {
		t.Fatalf("version moved on re-close: %d -> %d", versionAfterFirst, got)
	}
	_ = first
}

func TestClosePeriodWithClosingNotConfigured(t *testing.T) {
	l := New()
	postDated(t, l, "rev-1", "cash", "revenue", 500, "USD", sept2026())

	_, err := l.ClosePeriodWithClosing("2026-09")
	if !errors.Is(err, ErrClosingNotConfigured) {
		t.Fatalf("got %v, want ErrClosingNotConfigured", err)
	}
	if l.IsPeriodClosed("2026-09") {
		t.Fatal("period must stay open when closing is not configured")
	}
	// The failed close recorded nothing.
	if got := l.Version(); got != 1 {
		t.Fatalf("version = %d, want 1", got)
	}
}

func TestClosePeriodWithClosingFrozenRetained(t *testing.T) {
	l := New()
	if err := l.SetClosingAccounts([]AccountID{"expenses"}, "retained"); err != nil {
		t.Fatal(err)
	}
	postDated(t, l, "exp-1", "expenses", "cash", 200, "USD", sept2026())

	l.Freeze("retained")
	_, err := l.ClosePeriodWithClosing("2026-09")
	if !errors.Is(err, ErrAccountFrozen) {
		t.Fatalf("got %v, want ErrAccountFrozen", err)
	}
	// The failed close keeps the period OPEN: never a silent half-close.
	if l.IsPeriodClosed("2026-09") {
		t.Fatal("period must stay open after a failed close")
	}
	if b := l.BalanceIn("expenses", "USD"); b != 200 {
		t.Fatalf("expenses balance = %d, want 200 (nothing committed)", b)
	}
	if got := l.Version(); got != 1 {
		t.Fatalf("version = %d, want 1", got)
	}

	// After unfreezing, the close succeeds.
	l.Unfreeze("retained")
	receipt, err := l.ClosePeriodWithClosing("2026-09")
	if err != nil {
		t.Fatalf("close after unfreeze: %v", err)
	}
	if len(receipt.Entries) != 1 || !receipt.Closed {
		t.Fatalf("receipt = %+v", receipt)
	}
}

func TestClosePeriodWithClosingNextPeriodClosed(t *testing.T) {
	l := New()
	if err := l.SetClosingAccounts([]AccountID{"revenue"}, "retained"); err != nil {
		t.Fatal(err)
	}
	postDated(t, l, "rev-1", "cash", "revenue", 500, "USD", sept2026())

	// Lock the next month first: the closing entries would land in a
	// closed period, so the close must fail and September stays open.
	if err := l.ClosePeriod("2026-10"); err != nil {
		t.Fatalf("ClosePeriod 2026-10: %v", err)
	}
	_, err := l.ClosePeriodWithClosing("2026-09")
	if !errors.Is(err, ErrPeriodClosed) {
		t.Fatalf("got %v, want ErrPeriodClosed", err)
	}
	if l.IsPeriodClosed("2026-09") {
		t.Fatal("2026-09 must stay open when the next month is closed")
	}
}

func TestClosePeriodWithClosingMultiCurrency(t *testing.T) {
	l := New()
	if err := l.SetClosingAccounts([]AccountID{"revenue"}, "retained"); err != nil {
		t.Fatal(err)
	}
	postDated(t, l, "rev-usd", "cash", "revenue", 500, "USD", sept2026())
	postDated(t, l, "rev-eur", "cash", "revenue", 300, "EUR", sept2026())

	receipt, err := l.ClosePeriodWithClosing("2026-09")
	if err != nil {
		t.Fatalf("ClosePeriodWithClosing: %v", err)
	}
	if len(receipt.Entries) != 2 {
		t.Fatalf("closing entries = %d, want 2 (one per currency)", len(receipt.Entries))
	}
	if receipt.Entries[0].ID != "2026-09/closing/revenue/EUR" ||
		receipt.Entries[1].ID != "2026-09/closing/revenue/USD" {
		t.Fatalf("currency order = %q, %q", receipt.Entries[0].ID, receipt.Entries[1].ID)
	}
	if b := l.BalanceIn("revenue", "USD"); b != 0 {
		t.Fatalf("revenue USD = %d, want 0", b)
	}
	if b := l.BalanceIn("revenue", "EUR"); b != 0 {
		t.Fatalf("revenue EUR = %d, want 0", b)
	}
	if b := l.BalanceIn("retained", "USD"); b != -500 {
		t.Fatalf("retained USD = %d, want -500", b)
	}
	if b := l.BalanceIn("retained", "EUR"); b != -300 {
		t.Fatalf("retained EUR = %d, want -300", b)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Fatalf("VerifyAccountingEquation: %v", err)
	}
}

func TestClosePeriodWithClosingSkipsZeroBalances(t *testing.T) {
	l := New()
	if err := l.SetClosingAccounts([]AccountID{"revenue", "dormant"}, "retained"); err != nil {
		t.Fatal(err)
	}
	postDated(t, l, "rev-1", "cash", "revenue", 500, "USD", sept2026())

	receipt, err := l.ClosePeriodWithClosing("2026-09")
	if err != nil {
		t.Fatalf("ClosePeriodWithClosing: %v", err)
	}
	// Only revenue carried a balance; the dormant P&L account books no
	// entry, but the period still closes.
	if len(receipt.Entries) != 1 {
		t.Fatalf("closing entries = %d, want 1", len(receipt.Entries))
	}
	if !receipt.Closed || !l.IsPeriodClosed("2026-09") {
		t.Fatal("period should be closed")
	}
}

func TestClosePeriodWithClosingIDConflict(t *testing.T) {
	l := New()
	if err := l.SetClosingAccounts([]AccountID{"revenue"}, "retained"); err != nil {
		t.Fatal(err)
	}
	postDated(t, l, "rev-1", "cash", "revenue", 500, "USD", sept2026())
	// Forge a manual entry on the deterministic closing ID: the close
	// must fail closed instead of double-booking.
	postDated(t, l, "2026-09/closing/revenue/USD", "cash", "revenue", 1, "USD", sept2026())

	_, err := l.ClosePeriodWithClosing("2026-09")
	if !errors.Is(err, ErrClosingIDConflict) {
		t.Fatalf("got %v, want ErrClosingIDConflict", err)
	}
	if l.IsPeriodClosed("2026-09") {
		t.Fatal("period must stay open on ID conflict")
	}
}

func TestClosePeriodWithClosingOverdraftRetained(t *testing.T) {
	l := New()
	if err := l.SetClosingAccounts([]AccountID{"revenue"}, "retained"); err != nil {
		t.Fatal(err)
	}
	l.EnableOverdraftProtection("retained")
	postDated(t, l, "rev-1", "cash", "revenue", 500, "USD", sept2026())

	// Closing the revenue credits retained 500, taking it to -500: an
	// overdraft-protected retained account rejects the close.
	_, err := l.ClosePeriodWithClosing("2026-09")
	if !errors.Is(err, ErrAccountOverdraft) {
		t.Fatalf("got %v, want ErrAccountOverdraft", err)
	}
	if l.IsPeriodClosed("2026-09") {
		t.Fatal("period must stay open on overdraft rejection")
	}
}

func TestClosingConfigSnapshotRoundTrip(t *testing.T) {
	l := New()
	if err := l.SetClosingAccounts([]AccountID{"revenue", "expenses"}, "retained"); err != nil {
		t.Fatal(err)
	}
	postDated(t, l, "rev-1", "cash", "revenue", 500, "USD", sept2026())
	if _, err := l.ClosePeriodWithClosing("2026-09"); err != nil {
		t.Fatalf("close: %v", err)
	}

	var buf strings.Builder
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	pl, retained := restored.ClosingAccounts()
	if len(pl) != 2 || pl[0] != "expenses" || pl[1] != "revenue" || retained != "retained" {
		t.Fatalf("restored config = %v/%q", pl, retained)
	}
	if !restored.IsPeriodClosed("2026-09") {
		t.Fatal("restored ledger lost the period lock")
	}
	if !snapshotLedgersEqual(l, restored) {
		t.Fatal("snapshot round-trip diverged")
	}
}
