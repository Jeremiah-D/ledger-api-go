package ledger

import (
	"encoding/base64"
	"sort"
	"time"
)

// decodeListCursor validates the opaque keyset cursor (base64url of an entry
// ID) and returns the entry ID to resume after. An empty cursor resumes from
// the beginning.
func decodeListCursor(cursor string) (string, error) {
	if cursor == "" {
		return "", nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(decoded) == 0 {
		return "", ErrInvalidCursor
	}
	return string(decoded), nil
}

// paginateSortedEntries sorts entries by (CreatedAt, ID) and returns the
// page following afterID (empty = from the start), plus the cursor for the
// next page (empty when this is the last page). afterID must name an entry
// present in the slice; otherwise the resume position is undefined and
// ErrInvalidCursor is returned. The returned page is never nil, so it
// encodes as [] in JSON.
func paginateSortedEntries(filtered []JournalEntry, afterID string, limit int) (page []JournalEntry, nextCursor string, err error) {
	sort.Slice(filtered, func(i, j int) bool {
		if !filtered[i].CreatedAt.Equal(filtered[j].CreatedAt) {
			return filtered[i].CreatedAt.Before(filtered[j].CreatedAt)
		}
		return filtered[i].ID < filtered[j].ID
	})

	start := 0
	if afterID != "" {
		found := false
		for i, e := range filtered {
			if e.ID == afterID {
				start = i + 1
				found = true
				break
			}
		}
		if !found {
			return nil, "", ErrInvalidCursor
		}
	}

	end := start + limit
	if end > len(filtered) {
		end = len(filtered)
	}
	page = filtered[start:end]
	if end < len(filtered) {
		nextCursor = base64.RawURLEncoding.EncodeToString([]byte(page[len(page)-1].ID))
	}
	return page, nextCursor, nil
}

// ListAccountEntries returns up to limit journal entries that touched the
// given account — either as the debit or as the credit leg — whose CreatedAt
// falls in [since, until), sorted by (CreatedAt, ID). It carries the same
// cursor contract as ListEntries: opaque base64url cursor, never
// duplicated-or-skipped resume, ErrInvalidCursor for unknown cursors,
// ErrInvalidLimit for out-of-range limits. Unknown accounts yield an empty
// (non-nil) page.
//
// The per-account index maintained by Post makes the gather O(k) in the
// account's own entries instead of O(N) over the whole journal; the index
// itself is read under the read lock, so concurrent ListAccountEntries
// calls never block each other — only Posts take the write lock.
func (l *Ledger) ListAccountEntries(a AccountID, since, until time.Time, cursor string, limit int) (page []JournalEntry, nextCursor string, err error) {
	if limit < 1 || limit > maxPageSize {
		return nil, "", ErrInvalidLimit
	}
	afterID, err := decodeListCursor(cursor)
	if err != nil {
		return nil, "", err
	}

	l.mu.RLock()
	ids := l.byAccount[a] // nil for unknown accounts: the loop below yields nothing
	filtered := make([]JournalEntry, 0, len(ids))
	for _, id := range ids {
		e := l.entries[id]
		if e.CreatedAt.Before(since) {
			continue
		}
		if !e.CreatedAt.Before(until) {
			continue
		}
		filtered = append(filtered, e)
	}
	l.mu.RUnlock()

	return paginateSortedEntries(filtered, afterID, limit)
}
