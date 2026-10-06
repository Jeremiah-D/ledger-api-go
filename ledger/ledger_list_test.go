package ledger

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// seedWindowedEntries posts 5 entries with CreatedAt spread across two days.
func seedWindowedEntries(t *testing.T, l *Ledger) {
	t.Helper()
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		e := JournalEntry{
			ID:             fmt.Sprintf("e-win-%d", i),
			DebitAccount:   "cash",
			CreditAccount:  "equity",
			AmountCents:    100,
			IdempotencyKey: fmt.Sprintf("key-win-%d", i),
			CreatedAt:      base.Add(time.Duration(i*12) * time.Hour), // 12h apart
		}
		if _, dup, err := l.Post(e); err != nil || dup {
			t.Fatalf("Post(e-win-%d) = dup=%v err=%v", i, dup, err)
		}
	}
}

func entryIDs(es []JournalEntry) []string {
	ids := make([]string, len(es))
	for i, e := range es {
		ids[i] = e.ID
	}
	return ids
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestListEntriesTimeWindow(t *testing.T) {
	l := New()
	seedWindowedEntries(t, l)

	since := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)

	page, next, err := l.ListEntries(since, until, "", 100)
	if err != nil {
		t.Fatalf("ListEntries error: %v", err)
	}
	// Entries at 10-01 12:00, 10-02 00:00, 10-02 12:00, 10-03 00:00, 10-03 12:00.
	// Window [10-02 00:00, 10-03 00:00) contains e-win-1 and e-win-2.
	if got, want := entryIDs(page), []string{"e-win-1", "e-win-2"}; !equalStrings(got, want) {
		t.Fatalf("windowed page = %v, want %v", got, want)
	}
	if next != "" {
		t.Fatalf("next cursor = %q on the last page, want empty", next)
	}

	// Empty window yields an empty page, not nil, so JSON encodes as [].
	page, next, err = l.ListEntries(until, since, "", 100)
	if err != nil {
		t.Fatalf("ListEntries error: %v", err)
	}
	if page == nil || len(page) != 0 {
		t.Fatalf("inverted window page = %#v, want empty non-nil slice", page)
	}
	if next != "" {
		t.Fatalf("next cursor = %q on empty result, want empty", next)
	}
}

func TestListEntriesCursorPagination(t *testing.T) {
	l := New()
	seedWindowedEntries(t, l)

	var seen []string
	cursor := ""
	for {
		page, next, err := l.ListEntries(time.Time{}, time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), cursor, 2)
		if err != nil {
			t.Fatalf("ListEntries error: %v", err)
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

	want := []string{"e-win-0", "e-win-1", "e-win-2", "e-win-3", "e-win-4"}
	if !equalStrings(seen, want) {
		t.Fatalf("paginated walk = %v, want %v", seen, want)
	}
}

func TestListEntriesInsertBetweenPages(t *testing.T) {
	l := New()
	seedWindowedEntries(t, l)

	// Take the first page.
	first, cursor, err := l.ListEntries(time.Time{}, time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), "", 2)
	if err != nil {
		t.Fatalf("ListEntries error: %v", err)
	}
	if cursor == "" {
		t.Fatalf("expected a cursor after the first page")
	}

	// Insert a new entry that sorts between page 1 and page 2 (i.e. after
	// the cursor, like any entry posted later would).
	mid := JournalEntry{
		ID:             "e-win-mid",
		DebitAccount:   "cash",
		CreditAccount:  "equity",
		AmountCents:    100,
		IdempotencyKey: "key-win-mid",
		CreatedAt:      time.Date(2026, 10, 2, 6, 0, 0, 0, time.UTC), // between e-win-1 and e-win-2
	}
	if _, dup, err := l.Post(mid); err != nil || dup {
		t.Fatalf("Post(mid) = dup=%v err=%v", dup, err)
	}

	// Resume: the new entry must appear exactly once, nothing is skipped or
	// duplicated.
	var seen []string
	seen = append(seen, entryIDs(first)...)
	for cur := cursor; cur != ""; {
		page, next, err := l.ListEntries(time.Time{}, time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), cur, 2)
		if err != nil {
			t.Fatalf("ListEntries error: %v", err)
		}
		seen = append(seen, entryIDs(page)...)
		cur = next
	}
	want := []string{"e-win-0", "e-win-1", "e-win-mid", "e-win-2", "e-win-3", "e-win-4"}
	if !equalStrings(seen, want) {
		t.Fatalf("walk with interleaved insert = %v, want %v", seen, want)
	}
}

func TestListEntriesInvalidInput(t *testing.T) {
	l := New()
	seedWindowedEntries(t, l)
	epoch := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

	if _, _, err := l.ListEntries(time.Time{}, epoch, "", 0); !errors.Is(err, ErrInvalidLimit) {
		t.Fatalf("limit 0 error = %v, want ErrInvalidLimit", err)
	}
	if _, _, err := l.ListEntries(time.Time{}, epoch, "", 1001); !errors.Is(err, ErrInvalidLimit) {
		t.Fatalf("limit 1001 error = %v, want ErrInvalidLimit", err)
	}
	if _, _, err := l.ListEntries(time.Time{}, epoch, "!!!not-base64!!!", 10); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("garbage cursor error = %v, want ErrInvalidCursor", err)
	}

	// A cursor naming an entry outside the requested window is rejected,
	// not silently treated as the start.
	_, cursor, err := l.ListEntries(time.Time{}, epoch, "", 1)
	if err != nil {
		t.Fatalf("ListEntries error: %v", err)
	}
	since := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	if _, _, err := l.ListEntries(since, epoch, cursor, 10); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("out-of-window cursor error = %v, want ErrInvalidCursor", err)
	}
}
