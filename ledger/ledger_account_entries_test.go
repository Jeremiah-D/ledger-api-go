package ledger

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// seedMixedAccounts posts 6 entries across three accounts: cash↔equity,
// cash↔revenue, and equity↔revenue, with CreatedAt 6h apart.
func seedMixedAccounts(t *testing.T, l *Ledger) {
	t.Helper()
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	legs := [][2]AccountID{
		{"cash", "equity"},
		{"cash", "revenue"},
		{"equity", "cash"},
		{"revenue", "equity"},
		{"cash", "equity"},
		{"revenue", "cash"},
	}
	for i, leg := range legs {
		e := JournalEntry{
			ID:             fmt.Sprintf("e-acct-%d", i),
			DebitAccount:   leg[0],
			CreditAccount:  leg[1],
			AmountCents:    100,
			IdempotencyKey: fmt.Sprintf("key-acct-%d", i),
			CreatedAt:      base.Add(time.Duration(i*6) * time.Hour),
		}
		if _, dup, err := l.Post(e); err != nil || dup {
			t.Fatalf("Post(e-acct-%d) = dup=%v err=%v", i, dup, err)
		}
	}
}

func TestListAccountEntriesFiltersByAccount(t *testing.T) {
	l := New()
	seedMixedAccounts(t, l)
	epoch := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

	// cash touches e-acct-0 (debit), e-acct-1 (debit), e-acct-2 (credit),
	// e-acct-4 (debit), e-acct-5 (credit).
	page, next, err := l.ListAccountEntries("cash", time.Time{}, epoch, "", 100)
	if err != nil {
		t.Fatalf("ListAccountEntries(cash) error: %v", err)
	}
	want := []string{"e-acct-0", "e-acct-1", "e-acct-2", "e-acct-4", "e-acct-5"}
	if got := entryIDs(page); !equalStrings(got, want) {
		t.Fatalf("cash entries = %v, want %v", got, want)
	}
	if next != "" {
		t.Fatalf("next cursor = %q on the last page, want empty", next)
	}

	// revenue touches e-acct-1 (credit), e-acct-3 (debit), e-acct-5 (debit).
	page, _, err = l.ListAccountEntries("revenue", time.Time{}, epoch, "", 100)
	if err != nil {
		t.Fatalf("ListAccountEntries(revenue) error: %v", err)
	}
	if got, want := entryIDs(page), []string{"e-acct-1", "e-acct-3", "e-acct-5"}; !equalStrings(got, want) {
		t.Fatalf("revenue entries = %v, want %v", got, want)
	}

	// Unknown accounts yield an empty, non-nil page — not 404, not nil.
	page, next, err = l.ListAccountEntries("nobody", time.Time{}, epoch, "", 100)
	if err != nil {
		t.Fatalf("ListAccountEntries(nobody) error: %v", err)
	}
	if page == nil || len(page) != 0 {
		t.Fatalf("unknown account page = %#v, want empty non-nil slice", page)
	}
	if next != "" {
		t.Fatalf("next cursor = %q on empty result, want empty", next)
	}
}

func TestListAccountEntriesCursorPagination(t *testing.T) {
	l := New()
	seedMixedAccounts(t, l)
	epoch := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

	// Walk cash's 5 entries two at a time.
	var seen []string
	cursor := ""
	for {
		page, next, err := l.ListAccountEntries("cash", time.Time{}, epoch, cursor, 2)
		if err != nil {
			t.Fatalf("ListAccountEntries error: %v", err)
		}
		if len(page) > 2 {
			t.Fatalf("page larger than limit: %d", len(page))
		}
		seen = append(seen, entryIDs(page)...)
		if next == "" {
			break
		}
		cursor = next
	}
	want := []string{"e-acct-0", "e-acct-1", "e-acct-2", "e-acct-4", "e-acct-5"}
	if !equalStrings(seen, want) {
		t.Fatalf("paginated walk = %v, want %v", seen, want)
	}
}

func TestListAccountEntriesTimeWindow(t *testing.T) {
	l := New()
	seedMixedAccounts(t, l)

	// Entries at 10-01 12:00, 18:00, 10-02 00:00, 06:00, 12:00, 18:00.
	// Window [10-02 00:00, 10-02 12:00) holds e-acct-2 and e-acct-3; only
	// e-acct-2 touches cash.
	since := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	page, _, err := l.ListAccountEntries("cash", since, until, "", 100)
	if err != nil {
		t.Fatalf("ListAccountEntries error: %v", err)
	}
	if got, want := entryIDs(page), []string{"e-acct-2"}; !equalStrings(got, want) {
		t.Fatalf("windowed page = %v, want %v", got, want)
	}
}

func TestListAccountEntriesInvalidInput(t *testing.T) {
	l := New()
	seedMixedAccounts(t, l)
	epoch := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

	if _, _, err := l.ListAccountEntries("cash", time.Time{}, epoch, "", 0); !errors.Is(err, ErrInvalidLimit) {
		t.Fatalf("limit 0 error = %v, want ErrInvalidLimit", err)
	}
	if _, _, err := l.ListAccountEntries("cash", time.Time{}, epoch, "", 1001); !errors.Is(err, ErrInvalidLimit) {
		t.Fatalf("limit 1001 error = %v, want ErrInvalidLimit", err)
	}
	if _, _, err := l.ListAccountEntries("cash", time.Time{}, epoch, "!!!not-base64!!!", 10); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("garbage cursor error = %v, want ErrInvalidCursor", err)
	}

	// A cursor naming an entry that never touched this account is rejected:
	// e-acct-3 is equity↔revenue, not cash. The first revenue page of
	// limit=2 ends at e-acct-3, so its cursor names that entry.
	_, cursorOfThird, err := l.ListAccountEntries("revenue", time.Time{}, epoch, "", 2)
	if err != nil {
		t.Fatalf("ListAccountEntries error: %v", err)
	}
	// cursorOfThird points at e-acct-3 (the second revenue entry); presenting
	// it to cash must fail.
	if _, _, err := l.ListAccountEntries("cash", time.Time{}, epoch, cursorOfThird, 10); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("foreign cursor error = %v, want ErrInvalidCursor", err)
	}
}

func TestConcurrentReadsAndWrites(t *testing.T) {
	// Readers (Balance, Snapshot, ListEntries, ListAccountEntries,
	// VerifyChain, ChainHead) race writers (Post) under -race: the RWMutex
	// must keep every interleaving safe, and the final state must verify.
	l := New()
	const writers = 8
	const perWriter = 50

	var wg sync.WaitGroup
	errs := make(chan error, writers*2)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				e := JournalEntry{
					ID:             fmt.Sprintf("e-rw-%d-%d", w, i),
					DebitAccount:   "cash",
					CreditAccount:  "equity",
					AmountCents:    1,
					IdempotencyKey: fmt.Sprintf("key-rw-%d-%d", w, i),
					CreatedAt:      time.Now(),
				}
				if _, _, err := l.Post(e); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		epoch := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
		for i := 0; i < writers*perWriter; i++ {
			_ = l.Balance("cash")
			_, _ = l.Snapshot("equity")
			_, _, _ = l.ListEntries(time.Time{}, epoch, "", 10)
			_, _, _ = l.ListAccountEntries("cash", time.Time{}, epoch, "", 10)
			_ = l.VerifyChain() // may legitimately be mid-write? No: Post holds the write lock for the whole commit.
			_, _ = l.ChainHead()
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Post error: %v", err)
	}

	total := int64(writers * perWriter)
	if got := l.Balance("cash"); got != total {
		t.Errorf("Balance(cash) = %d, want %d", got, total)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Errorf("VerifyAccountingEquation = %v, want nil", err)
	}
	if err := l.VerifyChain(); err != nil {
		t.Errorf("VerifyChain = %v, want nil", err)
	}
	if _, links := l.ChainHead(); links != uint64(total) {
		t.Errorf("chain links = %d, want %d", links, total)
	}
	if page, _, err := l.ListAccountEntries("cash", time.Time{}, time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), "", 1000); err != nil || len(page) != int(total) {
		t.Errorf("ListAccountEntries(cash) = %d entries err=%v, want %d", len(page), err, total)
	}
}
