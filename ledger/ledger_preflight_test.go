package ledger

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"
)

// mustPostPreflight is the local Post helper for preflight tests.
func mustPostPreflight(t *testing.T, l *Ledger, id string, debit, credit AccountID, cents int64, at time.Time) {
	t.Helper()
	if _, _, err := l.Post(JournalEntry{ID: id, DebitAccount: debit, CreditAccount: credit,
		AmountCents: cents, Currency: "USD", IdempotencyKey: "k-" + id, CreatedAt: at}); err != nil {
		t.Fatalf("Post %s: %v", id, err)
	}
}

func preflightBlocker(blockers []PreflightBlocker, code string) *PreflightBlocker {
	for i := range blockers {
		if blockers[i].Code == code {
			return &blockers[i]
		}
	}
	return nil
}

// TestClosePeriodPreflightClean: a quiet ledger closes clean; with
// closing accounts configured there are no warnings either.
func TestClosePeriodPreflightClean(t *testing.T) {
	l := New()
	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	mustPostPreflight(t, l, "p1", "a", "b", 1000, at)
	if err := l.SetClosingAccounts([]AccountID{"pl-expense"}, "retained"); err != nil {
		t.Fatalf("SetClosingAccounts: %v", err)
	}
	rep, err := l.ClosePeriodPreflight(at, "2026-10")
	if err != nil {
		t.Fatalf("ClosePeriodPreflight: %v", err)
	}
	if !rep.Clean || len(rep.Blockers) != 0 {
		t.Fatalf("report = %+v, want clean with no blockers", rep)
	}
	if len(rep.Warnings) != 0 {
		t.Fatalf("warnings = %+v, want none (rows dated in period, closing configured)", rep.Warnings)
	}
	if rep.Period != "2026-10" {
		t.Fatalf("period = %q, want 2026-10", rep.Period)
	}
}

// TestClosePeriodPreflightBlockers: all four blocker kinds are detected
// with their stable codes and IDs.
func TestClosePeriodPreflightBlockers(t *testing.T) {
	l := New(WithReviewThreshold(1)) // every transfer routes to review
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

	// 1. Pending dual-control review.
	tr := Transfer{ID: "xfer-1", From: "ar-clearing", To: "cust", AmountCents: 10000,
		Currency: "USD", IdempotencyKey: "kx1", CreatedAt: now}
	if _, err := l.PostTransfer(tr); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}

	// 2. Unexpired authorization hold (needs available funds first:
	// the hold reserves the payer's balance, so ops takes the debit leg).
	mustPostPreflight(t, l, "fund", "ops", "treasury", 50000, now)
	if _, _, err := l.Hold(Hold{ID: "hold-1", Account: "ops", AmountCents: 10000,
		Currency: "USD", ExpiresAt: now.Add(24 * time.Hour), CreatedAt: now}); err != nil {
		t.Fatalf("Hold: %v", err)
	}

	// 3. Booked (unsettled) FX forward.
	if _, err := l.BookForward(ForwardBooking{ID: "fwd-1", From: "desk", To: "client",
		FromCurrency: "USD", ToCurrency: "EUR", Rate: big.NewRat(11, 10),
		NotionalCents: 5000, SettleAt: now.Add(30 * 24 * time.Hour)}); err != nil {
		t.Fatalf("BookForward: %v", err)
	}

	// 4. Dunning L3 receivable.
	mustPostPreflight(t, l, "old", "cust-old", "ar-clearing", 20000, now.Add(-100*24*time.Hour))
	if err := l.SetDunningConfig(DunningConfig{Account: "cust-old", Currency: "USD",
		Level1Days: 30, Level2Days: 60, Level3Days: 90}); err != nil {
		t.Fatalf("SetDunningConfig: %v", err)
	}
	l.EscalateDunning(now)

	rep, err := l.ClosePeriodPreflight(now, "2026-10")
	if err != nil {
		t.Fatalf("ClosePeriodPreflight: %v", err)
	}
	if rep.Clean {
		t.Fatal("report is clean, want 4 blockers")
	}
	if len(rep.Blockers) != 4 {
		t.Fatalf("blockers = %+v, want 4", rep.Blockers)
	}
	if b := preflightBlocker(rep.Blockers, PreflightBlockerPendingReviews); b == nil || b.Count != 1 || b.IDs[0] != "xfer-1" {
		t.Fatalf("pending_reviews blocker = %+v", b)
	}
	if b := preflightBlocker(rep.Blockers, PreflightBlockerActiveHolds); b == nil || b.Count != 1 || b.IDs[0] != "hold-1" {
		t.Fatalf("active_holds blocker = %+v", b)
	}
	if b := preflightBlocker(rep.Blockers, PreflightBlockerBookedForwards); b == nil || b.Count != 1 || b.IDs[0] != "fwd-1" {
		t.Fatalf("booked_forwards blocker = %+v", b)
	}
	if b := preflightBlocker(rep.Blockers, PreflightBlockerDunningL3); b == nil || b.Count != 1 || b.IDs[0] != "cust-old:USD" {
		t.Fatalf("dunning_level3 blocker = %+v", b)
	}
}

// TestClosePeriodPreflightWarnings: empty months and a missing closing
// mapping warn without dirtying the close.
func TestClosePeriodPreflightWarnings(t *testing.T) {
	l := New()
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	rep, err := l.ClosePeriodPreflight(now, "2026-10")
	if err != nil {
		t.Fatalf("ClosePeriodPreflight: %v", err)
	}
	if !rep.Clean {
		t.Fatalf("report = %+v, want clean (warnings are not blockers)", rep)
	}
	if len(rep.Warnings) != 2 {
		t.Fatalf("warnings = %+v, want empty_period + no_closing_accounts", rep.Warnings)
	}
	codes := map[string]bool{}
	for _, w := range rep.Warnings {
		codes[w.Code] = true
	}
	if !codes[PreflightWarningEmptyPeriod] || !codes[PreflightWarningNoClosingConfig] {
		t.Fatalf("warning codes = %v, want both", codes)
	}
}

// TestClosePeriodPreflightInvalidID: a malformed period ID fails 400-class
// without touching the ledger.
func TestClosePeriodPreflightInvalidID(t *testing.T) {
	l := New()
	if _, err := l.ClosePeriodPreflight(time.Now(), "2026-13"); !errors.Is(err, ErrInvalidPeriodID) {
		t.Fatalf("ClosePeriodPreflight(bad ID) = %v, want ErrInvalidPeriodID", err)
	}
	if _, err := l.ClosePeriodPreflight(time.Now(), "october"); !errors.Is(err, ErrInvalidPeriodID) {
		t.Fatalf("ClosePeriodPreflight(bad ID) = %v, want ErrInvalidPeriodID", err)
	}
}

// TestClosePeriodWithPreflightBlocked: with blockers the close is refused
// (422-class), the period stays open, the version is untouched, and the
// refusal is audit-logged.
func TestClosePeriodWithPreflightBlocked(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithReviewThreshold(1), WithAuditLog(al))
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	tr := Transfer{ID: "xfer-1", From: "ar-clearing", To: "cust", AmountCents: 10000,
		Currency: "USD", IdempotencyKey: "kx1", CreatedAt: now}
	if _, err := l.PostTransfer(tr); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	vBefore := l.version
	err := l.ClosePeriodWithPreflight(context.Background(), "2026-10", true)
	if !errors.Is(err, ErrPeriodPreflightBlocked) {
		t.Fatalf("ClosePeriodWithPreflight = %v, want ErrPeriodPreflightBlocked", err)
	}
	if l.IsPeriodClosed("2026-10") {
		t.Fatal("period closed despite blockers")
	}
	if l.version != vBefore {
		t.Fatalf("version moved %d -> %d on a refused close", vBefore, l.version)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, _, err := ReadAuditLog(al.Dir(), time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	found := false
	for _, ev := range events {
		if ev.Op == "period_preflight_blocked" {
			found = true
			if ev.Details["blockers"] != float64(1) {
				t.Fatalf("blocked event details = %v, want 1 blocker", ev.Details)
			}
		}
		if ev.Op == "period_close" {
			t.Fatal("period_close event emitted for a refused close")
		}
	}
	if !found {
		t.Fatal("no period_preflight_blocked event in audit log")
	}
}

// TestClosePeriodWithPreflightClean: a clean scan closes exactly like the
// plain close; requirePreflightClean=false skips the scan entirely.
func TestClosePeriodWithPreflightClean(t *testing.T) {
	l := New()
	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	mustPostPreflight(t, l, "p1", "a", "b", 1000, at)
	if err := l.ClosePeriodWithPreflight(context.Background(), "2026-10", true); err != nil {
		t.Fatalf("ClosePeriodWithPreflight(clean) = %v", err)
	}
	if !l.IsPeriodClosed("2026-10") {
		t.Fatal("clean period not closed")
	}
	// Gate off: closes even with blockers present.
	l2 := New(WithReviewThreshold(1))
	tr := Transfer{ID: "xfer-1", From: "ar-clearing", To: "cust", AmountCents: 10000,
		Currency: "USD", IdempotencyKey: "kx1", CreatedAt: at}
	if _, err := l2.PostTransfer(tr); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if err := l2.ClosePeriodWithPreflight(context.Background(), "2026-10", false); err != nil {
		t.Fatalf("ClosePeriodWithPreflight(gate off) = %v", err)
	}
	if !l2.IsPeriodClosed("2026-10") {
		t.Fatal("period not closed with gate off")
	}
}

// TestClosePeriodPreflightZeroSideEffect: the scan itself changes nothing
// — version, journal, and dunning state are all untouched.
func TestClosePeriodPreflightZeroSideEffect(t *testing.T) {
	l := New()
	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	mustPostPreflight(t, l, "p1", "a", "b", 1000, at)
	vBefore, nBefore := l.version, len(l.entries)
	if _, err := l.ClosePeriodPreflight(at, "2026-10"); err != nil {
		t.Fatalf("ClosePeriodPreflight: %v", err)
	}
	if l.version != vBefore || len(l.entries) != nBefore {
		t.Fatalf("scan moved version %d->%d or journal %d->%d", vBefore, l.version, nBefore, len(l.entries))
	}
}

// TestClosePeriodPreflightLapsedForwardNotBlocker: a forward past its
// settlement grace period has lapsed (expiry is cancellation) and must
// not block the close.
func TestClosePeriodPreflightLapsedForwardNotBlocker(t *testing.T) {
	l := New()
	// BookForward requires SettleAt in the future; book against the real
	// clock then scan past the grace period.
	if _, err := l.BookForward(ForwardBooking{ID: "fwd-old", From: "desk", To: "client",
		FromCurrency: "USD", ToCurrency: "EUR", Rate: big.NewRat(11, 10),
		NotionalCents: 5000, SettleAt: time.Now().Add(24 * time.Hour)}); err != nil {
		t.Fatalf("BookForward: %v", err)
	}
	// Far past the grace period: the contract has lapsed.
	scanAt := time.Now().Add(400 * 24 * time.Hour)
	rep, err := l.ClosePeriodPreflight(scanAt, "2026-10")
	if err != nil {
		t.Fatalf("ClosePeriodPreflight: %v", err)
	}
	if b := preflightBlocker(rep.Blockers, PreflightBlockerBookedForwards); b != nil {
		t.Fatalf("lapsed forward blocks the close: %+v", b)
	}
}
