package ledger

import (
	"errors"
	"testing"
	"time"
)

// postStmtEntry is a small helper for statement tests: it posts one
// journal entry with an explicit CreatedAt and fails the test on error.
func postStmtEntry(t *testing.T, l *Ledger, id string, debit, credit AccountID, cents int64, currency string, at time.Time) {
	t.Helper()
	e := JournalEntry{
		ID:            id,
		DebitAccount:  debit,
		CreditAccount: credit,
		AmountCents:   cents,
		Currency:      currency,
		CreatedAt:     at,
	}
	if _, dup, err := l.Post(e); err != nil || dup {
		t.Fatalf("Post %s = dup=%v err=%v", id, dup, err)
	}
}

func findStmtBalance(rows []StatementCurrencyBalance, currency string) (int64, bool) {
	for _, r := range rows {
		if r.Currency == currency {
			return r.BalanceCents, true
		}
	}
	return 0, false
}

func TestGenerateStatementMultiCurrencyOpeningClosing(t *testing.T) {
	l := New()
	base := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	// e1: +10000 USD to cash on 09-10; e2: +5000 EUR to cash on 09-10
	// midday; e3: -3000 USD from cash on 09-11 midday.
	postStmtEntry(t, l, "s-e1", "cash", "equity", 10000, "USD", base)
	postStmtEntry(t, l, "s-e2", "cash", "equity", 5000, "EUR", base.Add(12*time.Hour))
	postStmtEntry(t, l, "s-e3", "equity", "cash", 3000, "USD", base.Add(36*time.Hour))

	from := base.Add(24 * time.Hour) // 2026-09-11T00:00Z
	to := base.Add(72 * time.Hour)   // 2026-09-13T00:00Z
	stmt, err := l.GenerateStatement("cash", from, to)
	if err != nil {
		t.Fatalf("GenerateStatement = %v", err)
	}
	if !stmt.Period.From.Equal(from) || !stmt.Period.To.Equal(to) {
		t.Errorf("period = %v..%v, want %v..%v", stmt.Period.From, stmt.Period.To, from, to)
	}
	// Opening: e1 and e2 are before from; e3 is in the period.
	if got, ok := findStmtBalance(stmt.OpeningBalances, "USD"); !ok || got != 10000 {
		t.Errorf("opening USD = %d,%v; want 10000,true", got, ok)
	}
	if got, ok := findStmtBalance(stmt.OpeningBalances, "EUR"); !ok || got != 5000 {
		t.Errorf("opening EUR = %d,%v; want 5000,true", got, ok)
	}
	// Closing: e3's 3000 USD outflow applies.
	if got, ok := findStmtBalance(stmt.ClosingBalances, "USD"); !ok || got != 7000 {
		t.Errorf("closing USD = %d,%v; want 7000,true", got, ok)
	}
	if got, ok := findStmtBalance(stmt.ClosingBalances, "EUR"); !ok || got != 5000 {
		t.Errorf("closing EUR = %d,%v; want 5000,true", got, ok)
	}
	// Currencies sorted by code.
	if len(stmt.OpeningBalances) != 2 || stmt.OpeningBalances[0].Currency != "EUR" || stmt.OpeningBalances[1].Currency != "USD" {
		t.Errorf("opening balances order = %+v, want EUR then USD", stmt.OpeningBalances)
	}
	// Only e3 falls in [from, to).
	if len(stmt.Entries) != 1 || stmt.Entries[0].ID != "s-e3" {
		t.Errorf("period entries = %v, want [s-e3]", stmt.Entries)
	}
	// No fees, no events on this ledger.
	if len(stmt.FeeSummary) != 0 {
		t.Errorf("fee summary = %+v, want empty", stmt.FeeSummary)
	}
	if len(stmt.EventSummary.FreezeEvents) != 0 || len(stmt.EventSummary.TransferReviews) != 0 {
		t.Errorf("event summary = %+v, want empty", stmt.EventSummary)
	}
	// Opening + period net == closing, per currency (statement arithmetic).
	openUSD, _ := findStmtBalance(stmt.OpeningBalances, "USD")
	closeUSD, _ := findStmtBalance(stmt.ClosingBalances, "USD")
	if openUSD-3000 != closeUSD {
		t.Errorf("statement arithmetic broken: opening %d - 3000 != closing %d", openUSD, closeUSD)
	}
}

func TestGenerateStatementEmptyPeriod(t *testing.T) {
	l := New()
	base := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	postStmtEntry(t, l, "s-empty-1", "cash", "equity", 4200, "USD", base)

	// A period long after the only posting: no entries, and opening ==
	// closing == the balance carried into the window.
	from := base.Add(30 * 24 * time.Hour)
	to := base.Add(31 * 24 * time.Hour)
	stmt, err := l.GenerateStatement("cash", from, to)
	if err != nil {
		t.Fatalf("GenerateStatement = %v", err)
	}
	if len(stmt.Entries) != 0 {
		t.Errorf("entries = %d, want 0", len(stmt.Entries))
	}
	if got, _ := findStmtBalance(stmt.OpeningBalances, "USD"); got != 4200 {
		t.Errorf("opening USD = %d, want 4200", got)
	}
	if got, _ := findStmtBalance(stmt.ClosingBalances, "USD"); got != 4200 {
		t.Errorf("closing USD = %d, want 4200", got)
	}
	if len(stmt.FeeSummary) != 0 {
		t.Errorf("fee summary = %+v, want empty", stmt.FeeSummary)
	}
}

func TestGenerateStatementUnknownAccount(t *testing.T) {
	l := New()
	stmt, err := l.GenerateStatement("nobody", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("GenerateStatement = %v", err)
	}
	if stmt.Account != "nobody" {
		t.Errorf("account = %q, want nobody", stmt.Account)
	}
	if len(stmt.Entries) != 0 || len(stmt.OpeningBalances) != 0 || len(stmt.ClosingBalances) != 0 {
		t.Errorf("unknown account statement not empty: %+v", stmt)
	}
	if len(stmt.FeeSummary) != 0 || len(stmt.EventSummary.FreezeEvents) != 0 {
		t.Errorf("unknown account event/fee sections not empty: %+v", stmt)
	}
}

func TestGenerateStatementDefaultMonth(t *testing.T) {
	l := New()
	now := time.Now().UTC()
	postStmtEntry(t, l, "s-month-1", "cash", "equity", 900, "USD", now.Add(-time.Hour))

	stmt, err := l.GenerateStatement("cash", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("GenerateStatement = %v", err)
	}
	wantFrom := statementMonthStart(now)
	if !stmt.Period.From.Equal(wantFrom) {
		t.Errorf("default from = %v, want month start %v", stmt.Period.From, wantFrom)
	}
	if stmt.Period.To.Before(now) || stmt.Period.To.After(time.Now().UTC().Add(time.Minute)) {
		t.Errorf("default to = %v, want ~now", stmt.Period.To)
	}
	if len(stmt.Entries) != 1 || stmt.Entries[0].ID != "s-month-1" {
		t.Errorf("entries = %v, want the one posted this month", stmt.Entries)
	}
}

func TestGenerateStatementInvalidRange(t *testing.T) {
	l := New()
	from := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if _, err := l.GenerateStatement("cash", from, to); !errors.Is(err, ErrInvalidStatementRange) {
		t.Errorf("err = %v, want ErrInvalidStatementRange", err)
	}
}

func TestGenerateStatementFeeSummary(t *testing.T) {
	l := New(WithTransferFeePolicy(100, "fee-revenue")) // 1%
	base := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)

	// t1: alice -> bob 10000 USD, fee 100 to fee-revenue.
	if _, err := l.PostTransfer(Transfer{
		ID: "s-fee-t1", From: "alice", To: "bob",
		AmountCents: 10000, Currency: "USD", CreatedAt: base,
	}); err != nil {
		t.Fatalf("PostTransfer t1 = %v", err)
	}
	// t2: same but SkipFee — no fee leg.
	if _, err := l.PostTransfer(Transfer{
		ID: "s-fee-t2", From: "alice", To: "bob",
		AmountCents: 5000, Currency: "USD", CreatedAt: base.Add(time.Hour), SkipFee: true,
	}); err != nil {
		t.Fatalf("PostTransfer t2 = %v", err)
	}

	from := base.Add(-time.Hour)
	to := base.Add(2 * time.Hour)

	// Payer's view: paid 100 USD in one fee leg.
	stmt, err := l.GenerateStatement("alice", from, to)
	if err != nil {
		t.Fatalf("GenerateStatement = %v", err)
	}
	if len(stmt.FeeSummary) != 1 {
		t.Fatalf("fee summary = %+v, want one USD row", stmt.FeeSummary)
	}
	row := stmt.FeeSummary[0]
	if row.Currency != "USD" || row.PaidCents != 100 || row.ReceivedCents != 0 || row.EntryCount != 1 {
		t.Errorf("fee row = %+v, want {USD 100 0 1}", row)
	}
	// The fee leg is one of the period entries (ID s-fee-t1/fee).
	found := false
	for _, e := range stmt.Entries {
		if e.ID == "s-fee-t1/fee" {
			found = true
		}
		if e.ID == "s-fee-t2/fee" {
			t.Errorf("SkipFee transfer produced a fee leg in entries")
		}
	}
	if !found {
		t.Errorf("fee leg s-fee-t1/fee missing from period entries")
	}

	// Fee account's view: received 100 USD.
	rev, err := l.GenerateStatement("fee-revenue", from, to)
	if err != nil {
		t.Fatalf("GenerateStatement = %v", err)
	}
	if len(rev.FeeSummary) != 1 {
		t.Fatalf("fee-revenue summary = %+v, want one USD row", rev.FeeSummary)
	}
	rrow := rev.FeeSummary[0]
	if rrow.PaidCents != 0 || rrow.ReceivedCents != 100 || rrow.EntryCount != 1 {
		t.Errorf("fee-revenue row = %+v, want {USD paid 0 received 100 count 1}", rrow)
	}
}

func TestGenerateStatementEventSummaryReviews(t *testing.T) {
	l := New(WithReviewThreshold(5000))
	base := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)

	// t1 enters dual-control review (10000 >= 5000): pending.
	if _, err := l.PostTransfer(Transfer{
		ID: "s-rev-t1", From: "alice", To: "bob",
		AmountCents: 10000, Currency: "USD", CreatedAt: base,
	}); err != nil {
		t.Fatalf("PostTransfer t1 = %v", err)
	}
	// t2: below threshold — settles immediately, never a review.
	if _, err := l.PostTransfer(Transfer{
		ID: "s-rev-t2", From: "alice", To: "bob",
		AmountCents: 100, Currency: "USD", CreatedAt: base.Add(time.Hour),
	}); err != nil {
		t.Fatalf("PostTransfer t2 = %v", err)
	}

	from := base.Add(-time.Hour)
	to := base.Add(2 * time.Hour)
	// NOTE: the review's CreatedAt is the wall-clock creation time (see
	// maybeReviewTransferLocked), not the transfer's backdated CreatedAt,
	// so the review window below is anchored at now.
	revFrom := time.Now().UTC().Add(-time.Hour)
	revTo := time.Now().UTC().Add(time.Hour)
	stmt, err := l.GenerateStatement("alice", from, to)
	if err != nil {
		t.Fatalf("GenerateStatement = %v", err)
	}
	// Pending rows are not effective: only t2's principal shows.
	if len(stmt.Entries) != 1 || stmt.Entries[0].ID != "s-rev-t2" {
		t.Errorf("entries = %v, want only settled [s-rev-t2]", stmt.Entries)
	}
	revStmt, err := l.GenerateStatement("alice", revFrom, revTo)
	if err != nil {
		t.Fatalf("GenerateStatement = %v", err)
	}
	ev := revStmt.EventSummary
	if len(ev.TransferReviews) != 1 {
		t.Fatalf("transfer reviews = %+v, want one", ev.TransferReviews)
	}
	rv := ev.TransferReviews[0]
	if rv.TransferID != "s-rev-t1" || rv.Status != ReviewStatusPending || rv.AmountCents != 10000 || rv.Currency != "USD" {
		t.Errorf("review = %+v, want pending s-rev-t1 10000 USD", rv)
	}
	if ev.ReviewCounts[ReviewStatusPending] != 1 {
		t.Errorf("review counts = %v, want {pending_review:1}", ev.ReviewCounts)
	}

	// Approve t1: the review flips to approved and its legs settle.
	if _, err := l.ApproveTransferReview("s-rev-t1", "ops"); err != nil {
		t.Fatalf("ApproveTransferReview = %v", err)
	}
	revStmt2, err := l.GenerateStatement("alice", revFrom, revTo)
	if err != nil {
		t.Fatalf("GenerateStatement = %v", err)
	}
	ev2 := revStmt2.EventSummary
	if len(ev2.TransferReviews) != 1 || ev2.TransferReviews[0].Status != ReviewStatusApproved {
		t.Fatalf("reviews after approval = %+v, want one approved", ev2.TransferReviews)
	}
	if ev2.TransferReviews[0].ReviewedBy != "ops" {
		t.Errorf("reviewed_by = %q, want ops", ev2.TransferReviews[0].ReviewedBy)
	}
	if ev2.ReviewCounts[ReviewStatusApproved] != 1 {
		t.Errorf("review counts = %v, want {approved:1}", ev2.ReviewCounts)
	}
	// Approved legs are effective now: t1's principal joins the entries
	// of the journal window (the legs keep the transfer's CreatedAt).
	stmt2, err := l.GenerateStatement("alice", from, to)
	if err != nil {
		t.Fatalf("GenerateStatement = %v", err)
	}
	if len(stmt2.Entries) != 2 {
		t.Errorf("entries after approval = %d, want 2", len(stmt2.Entries))
	}

	// Reject a second large transfer: status rejected.
	if _, err := l.PostTransfer(Transfer{
		ID: "s-rev-t3", From: "alice", To: "bob",
		AmountCents: 6000, Currency: "USD", CreatedAt: base.Add(90 * time.Minute),
	}); err != nil {
		t.Fatalf("PostTransfer t3 = %v", err)
	}
	if _, _, err := l.RejectTransferReview("s-rev-t3", "ops", "suspicious"); err != nil {
		t.Fatalf("RejectTransferReview = %v", err)
	}
	stmt3, err := l.GenerateStatement("alice", revFrom, revTo.Add(time.Hour))
	if err != nil {
		t.Fatalf("GenerateStatement = %v", err)
	}
	if got := stmt3.EventSummary.ReviewCounts[ReviewStatusRejected]; got != 1 {
		t.Errorf("rejected count = %d, want 1 (counts=%v)", got, stmt3.EventSummary.ReviewCounts)
	}
	if got := stmt3.EventSummary.ReviewCounts[ReviewStatusApproved]; got != 1 {
		t.Errorf("approved count = %d, want 1 (counts=%v)", got, stmt3.EventSummary.ReviewCounts)
	}
}

func TestGenerateStatementFreezeEvents(t *testing.T) {
	l := New()
	base := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	postStmtEntry(t, l, "s-frz-1", "cash", "equity", 1000, "USD", base)

	l.Freeze("cash")
	l.Unfreeze("cash")

	// Wide window around now: both transitions are in the period.
	stmt, err := l.GenerateStatement("cash", time.Now().UTC().Add(-time.Hour), time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("GenerateStatement = %v", err)
	}
	fe := stmt.EventSummary.FreezeEvents
	if len(fe) != 2 {
		t.Fatalf("freeze events = %+v, want 2", fe)
	}
	if fe[0].Kind != FreezeEventFrozen || fe[0].Account != "cash" {
		t.Errorf("first event = %+v, want frozen/cash", fe[0])
	}
	if fe[1].Kind != FreezeEventUnfrozen || fe[1].Account != "cash" {
		t.Errorf("second event = %+v, want unfrozen/cash", fe[1])
	}
	if !fe[1].At.After(fe[0].At) && !fe[1].At.Equal(fe[0].At) {
		t.Errorf("freeze events out of order: %v then %v", fe[0].At, fe[1].At)
	}
	// cash was frozen and then unfrozen again: must read false.
	if stmt.EventSummary.CurrentlyFrozen {
		t.Errorf("currently_frozen = true, want false after Unfreeze")
	}

	// A window in the past, before the freeze happened: no events.
	past, err := l.GenerateStatement("cash", base.Add(-2*time.Hour), base.Add(-time.Hour))
	if err != nil {
		t.Fatalf("GenerateStatement = %v", err)
	}
	if len(past.EventSummary.FreezeEvents) != 0 {
		t.Errorf("past-window freeze events = %+v, want empty", past.EventSummary.FreezeEvents)
	}
}

func TestGenerateStatementReadOnly(t *testing.T) {
	l := New(WithReviewThreshold(5000))
	base := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	postStmtEntry(t, l, "s-ro-1", "cash", "equity", 1000, "USD", base)
	l.Freeze("cash")
	l.Unfreeze("cash")
	l.EnableOverdraftProtection("cash")
	_, versionBefore := l.Snapshot("cash")
	freezeEventsBefore := len(l.freezeEvents)
	balBefore := l.Balance("cash")

	stmt, err := l.GenerateStatement("cash", base.Add(-time.Hour), time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("GenerateStatement = %v", err)
	}
	// Overdraft protection is structured current state: reported, not
	// invented events.
	if !stmt.EventSummary.OverdraftProtected {
		t.Errorf("overdraft_protected = false, want true")
	}

	if _, got := l.Snapshot("cash"); got != versionBefore {
		t.Errorf("version changed by statement: %d -> %d", versionBefore, got)
	}
	if got := len(l.freezeEvents); got != freezeEventsBefore {
		t.Errorf("freeze history changed by statement: %d -> %d", freezeEventsBefore, got)
	}
	if got := l.Balance("cash"); got != balBefore {
		t.Errorf("balance changed by statement: %d -> %d", balBefore, got)
	}
	// Generating a statement twice yields identical output: no hidden
	// write effects between reads.
	stmt2, err := l.GenerateStatement("cash", base.Add(-time.Hour), stmt.Period.To)
	if err != nil {
		t.Fatalf("GenerateStatement = %v", err)
	}
	if len(stmt2.Entries) != len(stmt.Entries) ||
		len(stmt2.OpeningBalances) != len(stmt.OpeningBalances) ||
		len(stmt2.FeeSummary) != len(stmt.FeeSummary) {
		t.Errorf("statement not stable across reads")
	}
}
