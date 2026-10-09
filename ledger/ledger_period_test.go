package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// backdatedEntry returns a valid entry stamped in the given period.
func backdatedEntry(id string, at time.Time) JournalEntry {
	return JournalEntry{
		ID:            id,
		DebitAccount:  "cash",
		CreditAccount: "equity",
		AmountCents:   100,
		Currency:      "USD",
		CreatedAt:     at,
	}
}

func TestPeriodIDValidation(t *testing.T) {
	for _, bad := range []string{"", "2026", "2026-13", "2026-1", "26-10", "2026/10", "october"} {
		if err := validatePeriodID(bad); !errors.Is(err, ErrInvalidPeriodID) {
			t.Errorf("validatePeriodID(%q) = %v, want ErrInvalidPeriodID", bad, err)
		}
		if err := New().ClosePeriod(bad); !errors.Is(err, ErrInvalidPeriodID) {
			t.Errorf("ClosePeriod(%q) = %v, want ErrInvalidPeriodID", bad, err)
		}
		if err := New().ReopenPeriod(bad); !errors.Is(err, ErrInvalidPeriodID) {
			t.Errorf("ReopenPeriod(%q) = %v, want ErrInvalidPeriodID", bad, err)
		}
	}
	l := New()
	if err := l.ClosePeriod("2025-05"); err != nil {
		t.Fatalf("ClosePeriod = %v", err)
	}
	if !l.IsPeriodClosed("2025-05") || l.IsPeriodClosed("2025-06") {
		t.Fatal("IsPeriodClosed reports wrong state")
	}
}

func TestPeriodCloseReopenIdempotent(t *testing.T) {
	l := New()
	if err := l.ClosePeriod("2025-05"); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := l.ClosePeriod("2025-05"); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if err := l.ReopenPeriod("2025-05"); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if l.IsPeriodClosed("2025-05") {
		t.Fatal("period still closed after reopen")
	}
	// Reopening a period that was never closed is a no-op.
	if err := l.ReopenPeriod("2025-05"); err != nil {
		t.Fatalf("reopen of open period: %v", err)
	}
	got := l.ClosedPeriods()
	if len(got) != 0 {
		t.Fatalf("ClosedPeriods = %v, want empty", got)
	}
}

func TestPostIntoClosedPeriodRejected(t *testing.T) {
	l := New()
	may := time.Date(2025, 5, 17, 12, 0, 0, 0, time.UTC)
	if err := l.ClosePeriod("2025-05"); err != nil {
		t.Fatalf("close: %v", err)
	}

	_, _, err := l.Post(backdatedEntry("e1", may))
	if !errors.Is(err, ErrPeriodClosed) {
		t.Fatalf("Post into closed period = %v, want ErrPeriodClosed", err)
	}
	// Rejected postings record nothing.
	if n := len(l.Entries()); n != 0 {
		t.Fatalf("entries after rejection = %d, want 0", n)
	}
	if _, v := l.Snapshot("cash"); v != 0 {
		t.Fatalf("version after rejection = %d, want 0", v)
	}

	// A posting in an open period still works, and the month boundary is
	// exact: the first second of the next month is a different period.
	jun := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	if _, _, err := l.Post(backdatedEntry("e2", jun)); err != nil {
		t.Fatalf("Post into open period = %v", err)
	}
	// Timestamps carry the location; the boundary is evaluated in UTC:
	// May 31 16:59:59 PDT is May 31 23:59:59 UTC, still inside the closed
	// month.
	lastOfMay := time.Date(2025, 5, 31, 16, 59, 59, 0, time.FixedZone("PDT", -7*3600))
	if _, _, err := l.Post(backdatedEntry("e3", lastOfMay)); !errors.Is(err, ErrPeriodClosed) {
		t.Fatalf("Post at 2025-05-31 16:59:59 PDT (= May 31 23:59:59 UTC) = %v, want ErrPeriodClosed", err)
	}
}

func TestPeriodReplayExempt(t *testing.T) {
	l := New()
	may := time.Date(2025, 5, 17, 12, 0, 0, 0, time.UTC)
	e := backdatedEntry("e1", may)
	e.IdempotencyKey = "k1"
	if _, _, err := l.Post(e); err != nil {
		t.Fatalf("initial post: %v", err)
	}
	if err := l.ClosePeriod("2025-05"); err != nil {
		t.Fatalf("close: %v", err)
	}
	// The replay books nothing new, so it must not fail on the period
	// that closed after the original posting.
	posted, dup, err := l.Post(e)
	if err != nil || !dup {
		t.Fatalf("replay after close = (%v, dup=%v), want (nil, true)", err, dup)
	}
	if posted.ID != "e1" {
		t.Fatalf("replay returned entry %q, want e1", posted.ID)
	}
}

func TestTransferIntoClosedPeriodRejected(t *testing.T) {
	l := New()
	may := time.Date(2025, 5, 17, 12, 0, 0, 0, time.UTC)
	if err := l.ClosePeriod("2025-05"); err != nil {
		t.Fatalf("close: %v", err)
	}
	tr := Transfer{ID: "t1", From: "payer", To: "payee", AmountCents: 500, Currency: "USD", CreatedAt: may}
	if _, err := l.PostTransfer(tr); !errors.Is(err, ErrPeriodClosed) {
		t.Fatalf("PostTransfer into closed period = %v, want ErrPeriodClosed", err)
	}
	if _, v := l.Snapshot("payer"); v != 0 {
		t.Fatalf("version after rejection = %d, want 0", v)
	}
}

func TestBatchIntoClosedPeriodRejected(t *testing.T) {
	l := New()
	may := time.Date(2025, 5, 17, 12, 0, 0, 0, time.UTC)
	if err := l.ClosePeriod("2025-05"); err != nil {
		t.Fatalf("close: %v", err)
	}
	b := Batch{
		ID: "b1",
		Entries: []JournalEntry{
			{ID: "be1", DebitAccount: "a", CreditAccount: "b", AmountCents: 10, Currency: "USD", CreatedAt: time.Now()},
			{ID: "be2", DebitAccount: "a", CreditAccount: "b", AmountCents: 10, Currency: "USD", CreatedAt: may},
		},
	}
	if _, err := l.PostBatch(b); !errors.Is(err, ErrPeriodClosed) {
		t.Fatalf("PostBatch with closed-period leg = %v, want ErrPeriodClosed", err)
	}
	if n := len(l.Entries()); n != 0 {
		t.Fatalf("entries after rejection = %d, want 0 (atomic)", n)
	}
}

func TestSweepIntoClosedPeriodRejected(t *testing.T) {
	l := New()
	if _, _, err := l.Post(JournalEntry{ID: "seed", DebitAccount: "sub", CreditAccount: "equity", AmountCents: 1000, Currency: "USD"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	may := time.Date(2025, 5, 17, 12, 0, 0, 0, time.UTC)
	if err := l.ClosePeriod("2025-05"); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := l.PostSweep(Sweep{ID: "sw1", From: []AccountID{"sub"}, To: "treasury", CreatedAt: may}); !errors.Is(err, ErrPeriodClosed) {
		t.Fatalf("PostSweep into closed period = %v, want ErrPeriodClosed", err)
	}
	if got := l.Balance("sub"); got != 1000 {
		t.Fatalf("sub balance after rejection = %d, want 1000", got)
	}
}

func TestMergeIntoClosedPeriodRejected(t *testing.T) {
	l := New()
	if _, _, err := l.Post(JournalEntry{ID: "seed", DebitAccount: "src", CreditAccount: "equity", AmountCents: 1000, Currency: "USD"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	may := time.Date(2025, 5, 17, 12, 0, 0, 0, time.UTC)
	if err := l.ClosePeriod("2025-05"); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := l.PostMerge(Merge{ID: "m1", From: "src", To: "dst", CreatedAt: may}); !errors.Is(err, ErrPeriodClosed) {
		t.Fatalf("PostMerge into closed period = %v, want ErrPeriodClosed", err)
	}
	if l.IsFrozen("src") {
		t.Fatal("source frozen by a rejected merge")
	}
}

func TestCaptureIntoClosedPeriodRejected(t *testing.T) {
	l := New()
	if _, _, err := l.Post(JournalEntry{ID: "seed", DebitAccount: "payer", CreditAccount: "equity", AmountCents: 1000, Currency: "USD"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, _, err := l.Hold(Hold{ID: "h1", Account: "payer", AmountCents: 1000, Currency: "USD", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("hold: %v", err)
	}
	// Capture entries are stamped now, so close the current month.
	now := time.Now().UTC()
	if err := l.ClosePeriod(now.Format("2006-01")); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := l.Capture(Capture{ID: "c1", HoldID: "h1", To: "payee", AmountCents: 400}); !errors.Is(err, ErrPeriodClosed) {
		t.Fatalf("Capture into closed period = %v, want ErrPeriodClosed", err)
	}
}

func TestPeriodReopenAllowsPostingAgain(t *testing.T) {
	l := New()
	may := time.Date(2025, 5, 17, 12, 0, 0, 0, time.UTC)
	if err := l.ClosePeriod("2025-05"); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := l.ReopenPeriod("2025-05"); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, _, err := l.Post(backdatedEntry("e1", may)); err != nil {
		t.Fatalf("Post after reopen = %v, want nil", err)
	}
}

func TestReconcileListsClosedPeriods(t *testing.T) {
	l := New()
	for _, id := range []string{"2025-07", "2025-05"} {
		if err := l.ClosePeriod(id); err != nil {
			t.Fatalf("close %s: %v", id, err)
		}
	}
	report := l.Reconcile(time.Now())
	if len(report.ClosedPeriods) != 2 || report.ClosedPeriods[0] != "2025-05" || report.ClosedPeriods[1] != "2025-07" {
		t.Fatalf("ClosedPeriods = %v, want [2025-05 2025-07] sorted", report.ClosedPeriods)
	}
	if err := l.ReopenPeriod("2025-05"); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	report = l.Reconcile(time.Now())
	if len(report.ClosedPeriods) != 1 || report.ClosedPeriods[0] != "2025-07" {
		t.Fatalf("ClosedPeriods after reopen = %v, want [2025-07]", report.ClosedPeriods)
	}
}

func TestPeriodSurvivesSnapshotRoundTrip(t *testing.T) {
	l := New()
	may := time.Date(2025, 5, 17, 12, 0, 0, 0, time.UTC)
	if _, _, err := l.Post(backdatedEntry("e1", time.Date(2025, 4, 2, 0, 0, 0, 0, time.UTC))); err != nil {
		t.Fatalf("post: %v", err)
	}
	if err := l.ClosePeriod("2025-05"); err != nil {
		t.Fatalf("close: %v", err)
	}
	var buf strings.Builder
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("export: %v", err)
	}
	l2, err := ImportSnapshot(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if !l2.IsPeriodClosed("2025-05") {
		t.Fatal("closed period lost in snapshot round trip")
	}
	if _, _, err := l2.Post(backdatedEntry("e2", may)); !errors.Is(err, ErrPeriodClosed) {
		t.Fatalf("Post on restored ledger into closed period = %v, want ErrPeriodClosed", err)
	}
	// The restored period lock does not break the rest of the books.
	if err := l2.VerifyChain(); err != nil {
		t.Fatalf("chain on restored ledger: %v", err)
	}
}
