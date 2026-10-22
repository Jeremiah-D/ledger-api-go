package ledger

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// registerNostroTestAccount registers a nostro account for tests and
// fails the test on error.
func registerNostroTestAccount(t *testing.T, l *Ledger, account AccountID, correspondent, kind string) NostroAccount {
	t.Helper()
	rec, err := l.RegisterNostroAccount(account, correspondent, kind)
	if err != nil {
		t.Fatalf("RegisterNostroAccount: %v", err)
	}
	return rec
}

// postNostroEntry posts one journal row touching the nostro account
// and fails the test on error. in=true debits the nostro account
// (money in), in=false credits it (money out).
func postNostroEntry(t *testing.T, l *Ledger, id string, nostro AccountID, in bool, cents int64, ref string) JournalEntry {
	t.Helper()
	e := JournalEntry{ID: id, AmountCents: cents, Currency: "USD", ExternalRef: ref}
	if in {
		e.DebitAccount, e.CreditAccount = nostro, "operating"
	} else {
		e.DebitAccount, e.CreditAccount = "operating", nostro
	}
	posted, _, err := l.Post(e)
	if err != nil {
		t.Fatalf("Post(%s): %v", id, err)
	}
	return posted
}

// nostroTestLine builds one expected correspondent line.
func nostroTestLine(id string, account AccountID, dir StatementDirection, cents int64, postedAt time.Time, ref string) StatementLine {
	return StatementLine{
		ID:          id,
		Account:     account,
		Currency:    "USD",
		Direction:   dir,
		AmountCents: cents,
		PostedAt:    postedAt,
		ExternalRef: ref,
	}
}

func TestRegisterNostroAccount_Validation(t *testing.T) {
	l := New()
	if _, err := l.RegisterNostroAccount("", "CHASE", NostroKindNostro); !errors.Is(err, ErrEmptyAccountID) {
		t.Errorf("empty account: err = %v, want ErrEmptyAccountID", err)
	}
	if _, err := l.RegisterNostroAccount("nostro-usd", "", NostroKindNostro); !errors.Is(err, ErrNostroEmptyCorrespondent) {
		t.Errorf("empty correspondent: err = %v, want ErrNostroEmptyCorrespondent", err)
	}
	if _, err := l.RegisterNostroAccount("nostro-usd", "CHASE", "loro"); !errors.Is(err, ErrNostroInvalidKind) {
		t.Errorf("bad kind: err = %v, want ErrNostroInvalidKind", err)
	}

	rec := registerNostroTestAccount(t, l, "nostro-usd", "CHASE", NostroKindNostro)
	if rec.Account != "nostro-usd" || rec.Correspondent != "CHASE" || rec.Kind != NostroKindNostro {
		t.Errorf("registration = %+v", rec)
	}
	got, ok := l.GetNostroAccount("nostro-usd")
	if !ok || got.Correspondent != "CHASE" {
		t.Errorf("GetNostroAccount = %+v, %v", got, ok)
	}
	if _, ok := l.GetNostroAccount("nope"); ok {
		t.Errorf("GetNostroAccount(nope) = true, want false")
	}
	// Re-registering overwrites the correspondent.
	registerNostroTestAccount(t, l, "nostro-usd", "CITI", NostroKindVostro)
	got, _ = l.GetNostroAccount("nostro-usd")
	if got.Correspondent != "CITI" || got.Kind != NostroKindVostro {
		t.Errorf("re-register = %+v", got)
	}
}

func TestRecordNostroExpectations_Validation(t *testing.T) {
	l := New()
	now := time.Now().UTC()
	lines := []StatementLine{nostroTestLine("ln-1", "nostro-usd", StatementDirectionIn, 1000, now, "ref-1")}

	// Unregistered account is refused.
	if _, err := l.RecordNostroExpectations("nostro-usd", lines); !errors.Is(err, ErrNostroNotRegistered) {
		t.Errorf("unregistered: err = %v, want ErrNostroNotRegistered", err)
	}
	registerNostroTestAccount(t, l, "nostro-usd", "CHASE", NostroKindNostro)

	// A line naming a different account is refused fail-closed: nothing
	// is stored.
	bad := []StatementLine{nostroTestLine("ln-2", "nostro-usd", StatementDirectionIn, 1000, now, "ref-2"),
		nostroTestLine("ln-3", "other-acct", StatementDirectionIn, 1000, now, "ref-3")}
	if _, err := l.RecordNostroExpectations("nostro-usd", bad); !errors.Is(err, ErrNostroLineAccountMismatch) {
		t.Errorf("account mismatch: err = %v, want ErrNostroLineAccountMismatch", err)
	}
	rep, err := l.MatchNostro("nostro-usd", now, NostroMatchOptions{})
	if err != nil {
		t.Fatalf("MatchNostro: %v", err)
	}
	if rep.Expected != 0 {
		t.Errorf("expected = %d after refused record, want 0", rep.Expected)
	}

	// Happy path: 2 new lines; re-recording the same lines adds 0.
	added, err := l.RecordNostroExpectations("nostro-usd", lines[:1])
	if err != nil || added != 1 {
		t.Fatalf("record: added=%d err=%v", added, err)
	}
	added, err = l.RecordNostroExpectations("nostro-usd", lines[:1])
	if err != nil || added != 0 {
		t.Fatalf("re-record: added=%d err=%v, want 0/nil", added, err)
	}
}

func TestMatchNostro_AllMatched(t *testing.T) {
	l := New()
	now := time.Now().UTC()
	registerNostroTestAccount(t, l, "nostro-usd", "CHASE", NostroKindNostro)

	postNostroEntry(t, l, "e-in-1", "nostro-usd", true, 50_000, "chase-in-1")
	postNostroEntry(t, l, "e-out-1", "nostro-usd", false, 12_000, "chase-out-1")

	lines := []StatementLine{
		nostroTestLine("ln-in-1", "nostro-usd", StatementDirectionIn, 50_000, now.Add(-time.Hour), "chase-in-1"),
		nostroTestLine("ln-out-1", "nostro-usd", StatementDirectionOut, 12_000, now.Add(-2*time.Hour), "chase-out-1"),
	}
	if _, err := l.RecordNostroExpectations("nostro-usd", lines); err != nil {
		t.Fatalf("record: %v", err)
	}
	rep, err := l.MatchNostro("nostro-usd", now, NostroMatchOptions{})
	if err != nil {
		t.Fatalf("MatchNostro: %v", err)
	}
	if rep.Matched != 2 || rep.Expected != 2 || rep.Actual != 2 {
		t.Errorf("matched/expected/actual = %d/%d/%d, want 2/2/2", rep.Matched, rep.Expected, rep.Actual)
	}
	if len(rep.Breaks) != 0 {
		t.Errorf("breaks = %+v, want none", rep.Breaks)
	}
	if rep.Correspondent != "CHASE" || rep.Kind != NostroKindNostro {
		t.Errorf("report header = %+v", rep)
	}
}

func TestMatchNostro_ExpectedUnreached(t *testing.T) {
	l := New()
	now := time.Now().UTC()
	registerNostroTestAccount(t, l, "nostro-usd", "CHASE", NostroKindNostro)

	postedAt := now.Add(-72 * time.Hour) // 3 days old -> 2-7d bucket
	lines := []StatementLine{nostroTestLine("ln-missing", "nostro-usd", StatementDirectionIn, 77_000, postedAt, "chase-ghost")}
	if _, err := l.RecordNostroExpectations("nostro-usd", lines); err != nil {
		t.Fatalf("record: %v", err)
	}
	rep, err := l.MatchNostro("nostro-usd", now, NostroMatchOptions{})
	if err != nil {
		t.Fatalf("MatchNostro: %v", err)
	}
	if rep.ExpectedUnreached != 1 || len(rep.Breaks) != 1 {
		t.Fatalf("breaks = %+v", rep.Breaks)
	}
	b := rep.Breaks[0]
	if b.Kind != NostroExpectedUnreached {
		t.Errorf("kind = %q, want expected_unreached", b.Kind)
	}
	if b.ExpectedLineID != "ln-missing" || b.ExpectedCents != 77_000 {
		t.Errorf("break = %+v", b)
	}
	if b.AgeDays != 3 {
		t.Errorf("age_days = %d, want 3", b.AgeDays)
	}
	if b.AgeBucket != NostroAgeBucketWeek {
		t.Errorf("age_bucket = %q, want %q", b.AgeBucket, NostroAgeBucketWeek)
	}
	if !b.ReferenceDate.Equal(postedAt) {
		t.Errorf("reference_date = %v, want %v", b.ReferenceDate, postedAt)
	}
}

func TestMatchNostro_ActualUnreported(t *testing.T) {
	l := New()
	now := time.Now().UTC()
	registerNostroTestAccount(t, l, "nostro-usd", "CHASE", NostroKindNostro)

	postNostroEntry(t, l, "e-late", "nostro-usd", false, 9_500, "chase-late")
	rep, err := l.MatchNostro("nostro-usd", now, NostroMatchOptions{})
	if err != nil {
		t.Fatalf("MatchNostro: %v", err)
	}
	if rep.ActualUnreported != 1 || len(rep.Breaks) != 1 {
		t.Fatalf("breaks = %+v", rep.Breaks)
	}
	b := rep.Breaks[0]
	if b.Kind != NostroActualUnreported {
		t.Errorf("kind = %q, want actual_unreported", b.Kind)
	}
	if b.ActualEntryID != "e-late" || b.ActualCents != 9_500 {
		t.Errorf("break = %+v", b)
	}
	if b.Direction != StatementDirectionOut {
		t.Errorf("direction = %q, want out (nostro was the credit leg)", b.Direction)
	}
	if b.AgeBucket != NostroAgeBucketCurrent {
		t.Errorf("age_bucket = %q, want %q", b.AgeBucket, NostroAgeBucketCurrent)
	}
}

func TestMatchNostro_AmountMismatch(t *testing.T) {
	l := New()
	now := time.Now().UTC()
	registerNostroTestAccount(t, l, "nostro-usd", "CHASE", NostroKindNostro)

	// Same identity (external reference), different amounts.
	postNostroEntry(t, l, "e-fx", "nostro-usd", true, 100_000, "chase-fx-1")
	lines := []StatementLine{nostroTestLine("ln-fx", "nostro-usd", StatementDirectionIn, 99_000, now.Add(-time.Hour), "chase-fx-1")}
	if _, err := l.RecordNostroExpectations("nostro-usd", lines); err != nil {
		t.Fatalf("record: %v", err)
	}
	rep, err := l.MatchNostro("nostro-usd", now, NostroMatchOptions{})
	if err != nil {
		t.Fatalf("MatchNostro: %v", err)
	}
	if rep.AmountMismatches != 1 || len(rep.Breaks) != 1 {
		t.Fatalf("breaks = %+v", rep.Breaks)
	}
	b := rep.Breaks[0]
	if b.Kind != NostroAmountMismatch {
		t.Errorf("kind = %q, want amount_mismatch", b.Kind)
	}
	if b.ExpectedLineID != "ln-fx" || b.ActualEntryID != "e-fx" {
		t.Errorf("identity = %q/%q", b.ExpectedLineID, b.ActualEntryID)
	}
	if b.DifferenceCents != 1_000 {
		t.Errorf("difference_cents = %d, want 1000", b.DifferenceCents)
	}
}

func TestMatchNostro_DirectionPin(t *testing.T) {
	l := New()
	now := time.Now().UTC()
	registerNostroTestAccount(t, l, "nostro-usd", "CHASE", NostroKindNostro)

	// The journal row credits the nostro account (money out), but the
	// correspondent line claims money in: no match on either side.
	postNostroEntry(t, l, "e-dir", "nostro-usd", false, 20_000, "")
	lines := []StatementLine{nostroTestLine("ln-dir", "nostro-usd", StatementDirectionIn, 20_000, now.Add(-time.Hour), "")}
	if _, err := l.RecordNostroExpectations("nostro-usd", lines); err != nil {
		t.Fatalf("record: %v", err)
	}
	rep, err := l.MatchNostro("nostro-usd", now, NostroMatchOptions{})
	if err != nil {
		t.Fatalf("MatchNostro: %v", err)
	}
	if rep.Matched != 0 {
		t.Errorf("matched = %d, want 0 (direction disagrees)", rep.Matched)
	}
	if rep.ExpectedUnreached != 1 || rep.ActualUnreported != 1 {
		t.Errorf("expected_unreached/actual_unreported = %d/%d, want 1/1",
			rep.ExpectedUnreached, rep.ActualUnreported)
	}
}

func TestMatchNostro_Ambiguous(t *testing.T) {
	l := New()
	now := time.Now().UTC()
	registerNostroTestAccount(t, l, "nostro-usd", "CHASE", NostroKindNostro)

	// Two identical journal rows fit one line: ambiguous, and the rows
	// stay unclaimed rather than misattributed.
	postNostroEntry(t, l, "e-amb-1", "nostro-usd", true, 30_000, "")
	postNostroEntry(t, l, "e-amb-2", "nostro-usd", true, 30_000, "")
	lines := []StatementLine{nostroTestLine("ln-amb", "nostro-usd", StatementDirectionIn, 30_000, now.Add(-time.Hour), "")}
	if _, err := l.RecordNostroExpectations("nostro-usd", lines); err != nil {
		t.Fatalf("record: %v", err)
	}
	rep, err := l.MatchNostro("nostro-usd", now, NostroMatchOptions{})
	if err != nil {
		t.Fatalf("MatchNostro: %v", err)
	}
	if rep.Ambiguous != 1 {
		t.Errorf("ambiguous = %d, want 1", rep.Ambiguous)
	}
	if rep.Matched != 0 {
		t.Errorf("matched = %d, want 0 (ambiguous claims nothing)", rep.Matched)
	}
	if rep.ActualUnreported != 2 {
		t.Errorf("actual_unreported = %d, want 2 (unclaimed rows surface)", rep.ActualUnreported)
	}
}

func TestMatchNostro_IdentityIgnoresDateWindow(t *testing.T) {
	l := New()
	now := time.Now().UTC()
	registerNostroTestAccount(t, l, "nostro-usd", "CHASE", NostroKindNostro)

	// Identity wins over the date window: the same business event
	// matches even when the booking dates drift apart.
	postNostroEntry(t, l, "e-old", "nostro-usd", true, 40_000, "chase-old")
	lines := []StatementLine{nostroTestLine("ln-old", "nostro-usd", StatementDirectionIn, 40_000, now.Add(-30*24*time.Hour), "chase-old")}
	if _, err := l.RecordNostroExpectations("nostro-usd", lines); err != nil {
		t.Fatalf("record: %v", err)
	}
	rep, err := l.MatchNostro("nostro-usd", now, NostroMatchOptions{MatchWindow: time.Hour})
	if err != nil {
		t.Fatalf("MatchNostro: %v", err)
	}
	if rep.Matched != 1 {
		t.Errorf("matched = %d, want 1 (identity beats the window)", rep.Matched)
	}
}

func TestMatchNostro_Tolerance(t *testing.T) {
	l := New()
	now := time.Now().UTC()
	registerNostroTestAccount(t, l, "nostro-usd", "CHASE", NostroKindNostro)

	postNostroEntry(t, l, "e-tol", "nostro-usd", true, 50_000, "")
	// 50 cents off: matches with tolerance 100, misses with 0.
	lines := []StatementLine{nostroTestLine("ln-tol", "nostro-usd", StatementDirectionIn, 50_050, now.Add(-time.Hour), "")}
	if _, err := l.RecordNostroExpectations("nostro-usd", lines); err != nil {
		t.Fatalf("record: %v", err)
	}
	rep, err := l.MatchNostro("nostro-usd", now, NostroMatchOptions{AmountToleranceCents: 100})
	if err != nil {
		t.Fatalf("MatchNostro: %v", err)
	}
	if rep.Matched != 1 {
		t.Errorf("matched = %d with tolerance 100, want 1", rep.Matched)
	}
	rep, err = l.MatchNostro("nostro-usd", now, NostroMatchOptions{})
	if err != nil {
		t.Fatalf("MatchNostro: %v", err)
	}
	if rep.Matched != 0 || rep.ExpectedUnreached != 1 {
		t.Errorf("matched/expected_unreached = %d/%d with tolerance 0, want 0/1", rep.Matched, rep.ExpectedUnreached)
	}
}

func TestMatchNostro_Unregistered(t *testing.T) {
	l := New()
	if _, err := l.MatchNostro("nostro-usd", time.Now().UTC(), NostroMatchOptions{}); !errors.Is(err, ErrNostroNotRegistered) {
		t.Errorf("err = %v, want ErrNostroNotRegistered", err)
	}
}

func TestMatchNostro_ReadOnly(t *testing.T) {
	l := New()
	now := time.Now().UTC()
	registerNostroTestAccount(t, l, "nostro-usd", "CHASE", NostroKindNostro)
	postNostroEntry(t, l, "e-ro", "nostro-usd", true, 10_000, "")
	lines := []StatementLine{nostroTestLine("ln-ro", "nostro-usd", StatementDirectionIn, 10_000, now.Add(-time.Hour), "")}
	if _, err := l.RecordNostroExpectations("nostro-usd", lines); err != nil {
		t.Fatalf("record: %v", err)
	}
	versionBefore := l.version
	for i := 0; i < 3; i++ {
		if _, err := l.MatchNostro("nostro-usd", now, NostroMatchOptions{}); err != nil {
			t.Fatalf("MatchNostro: %v", err)
		}
	}
	if l.version != versionBefore {
		t.Errorf("version = %d, want %d (match must not bump)", l.version, versionBefore)
	}
}

func TestNostroBreaks_InReconcile(t *testing.T) {
	l := New()
	now := time.Now().UTC()
	registerNostroTestAccount(t, l, "nostro-usd", "CHASE", NostroKindNostro)
	registerNostroTestAccount(t, l, "nostro-eur", "DEUT", NostroKindVostro)

	postNostroEntry(t, l, "e-rc", "nostro-usd", true, 60_000, "")
	lines := []StatementLine{nostroTestLine("ln-rc", "nostro-usd", StatementDirectionIn, 61_000, now.Add(-time.Hour), "")}
	if _, err := l.RecordNostroExpectations("nostro-usd", lines); err != nil {
		t.Fatalf("record: %v", err)
	}

	rep := l.Reconcile(now)
	if len(rep.NostroBreaks) != 2 {
		t.Fatalf("nostro_breaks = %+v, want 2", rep.NostroBreaks)
	}
	// Sorted by account: nostro-eur has no activity, nostro-usd carries
	// the expected_unreached + actual_unreported pair.
	if rep.NostroBreaks[0].Account != "nostro-usd" {
		t.Errorf("first break account = %q, want nostro-usd", rep.NostroBreaks[0].Account)
	}
	kinds := map[NostroBreakKind]int{}
	for _, b := range rep.NostroBreaks {
		kinds[b.Kind]++
		if b.Correspondent != "CHASE" {
			t.Errorf("break correspondent = %q, want CHASE", b.Correspondent)
		}
	}
	if kinds[NostroExpectedUnreached] != 1 || kinds[NostroActualUnreported] != 1 {
		t.Errorf("kinds = %v", kinds)
	}

	// A ledger with no nostro accounts reports no nostro_breaks key at
	// all (omitempty).
	plain := New().Reconcile(now)
	if plain.NostroBreaks != nil {
		t.Errorf("plain reconcile nostro_breaks = %+v, want nil", plain.NostroBreaks)
	}
}

func TestMatchNostro_Concurrent(t *testing.T) {
	l := New()
	now := time.Now().UTC()
	registerNostroTestAccount(t, l, "nostro-usd", "CHASE", NostroKindNostro)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				postNostroEntry(t, l, fmt.Sprintf("e-cc-%d-%d", i, j), "nostro-usd", true, int64(1000+i*10+j), "")
			}
		}(i)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				if _, err := l.MatchNostro("nostro-usd", now, NostroMatchOptions{}); err != nil {
					t.Errorf("MatchNostro: %v", err)
				}
			}
		}()
	}
	wg.Wait()
}
