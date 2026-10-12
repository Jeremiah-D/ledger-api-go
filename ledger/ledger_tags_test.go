package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestSetAccountTagsValidation pins the charset and length rules: keys
// and values must be 1-64 characters of [A-Za-z0-9._-].
func TestSetAccountTagsValidation(t *testing.T) {
	l := New()
	if err := l.SetAccountTags("", map[string]string{"k": "v"}); err != ErrEmptyTagAccount {
		t.Fatalf("empty account: err = %v, want ErrEmptyTagAccount", err)
	}
	long := strings.Repeat("a", maxTagSegmentLen+1)
	cases := []struct {
		name string
		tags map[string]string
		want error
	}{
		{"empty key", map[string]string{"": "v"}, ErrInvalidTagKey},
		{"key too long", map[string]string{long: "v"}, ErrInvalidTagKey},
		{"key bad char", map[string]string{"line/1": "v"}, ErrInvalidTagKey},
		{"key with space", map[string]string{"line 1": "v"}, ErrInvalidTagKey},
		{"empty value", map[string]string{"k": ""}, ErrInvalidTagValue},
		{"value too long", map[string]string{"k": long}, ErrInvalidTagValue},
		{"value bad char", map[string]string{"k": "v:1"}, ErrInvalidTagValue},
		{"value unicode", map[string]string{"k": "零售"}, ErrInvalidTagValue},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := l.SetAccountTags("acct", tc.tags)
			if !errors.Is(err, tc.want) {
				t.Fatalf("SetAccountTags(%v): err = %v, want it to wrap %v", tc.tags, err, tc.want)
			}
		})
	}
	// A failed validation leaves no tags behind.
	if _, ok := l.AccountTagsOf("acct"); ok {
		t.Fatal("AccountTagsOf after failed validation: want false")
	}
}

// TestSetAccountTagsSemantics covers replace, clear, copy isolation, and
// the structural property: tagging never bumps the journal version.
func TestSetAccountTagsSemantics(t *testing.T) {
	l := New()
	if _, _, err := l.Post(JournalEntry{ID: "e1", DebitAccount: "a", CreditAccount: "bank", AmountCents: 100}); err != nil {
		t.Fatal(err)
	}
	v0 := l.Version()

	tags := map[string]string{"line": "retail", "segment": "cross_border"}
	if err := l.SetAccountTags("a", tags); err != nil {
		t.Fatal(err)
	}
	// Mutating the input map afterwards must not touch the ledger.
	tags["line"] = "wholesale"
	got, ok := l.AccountTagsOf("a")
	if !ok {
		t.Fatal("AccountTagsOf: want true")
	}
	if got["line"] != "retail" {
		t.Fatalf("tags[line] = %q, want %q (input map isolation)", got["line"], "retail")
	}
	// Mutating the returned map must not touch the ledger either.
	got["line"] = "wholesale"
	got2, _ := l.AccountTagsOf("a")
	if got2["line"] != "retail" {
		t.Fatalf("tags[line] = %q after mutating the copy, want %q", got2["line"], "retail")
	}
	// Replace semantics: the whole set is replaced, not merged.
	if err := l.SetAccountTags("a", map[string]string{"cost_center": "ops"}); err != nil {
		t.Fatal(err)
	}
	got, _ = l.AccountTagsOf("a")
	if len(got) != 1 || got["cost_center"] != "ops" {
		t.Fatalf("tags after replace = %v, want {cost_center:ops}", got)
	}
	// Structural: the version never moved.
	if v := l.Version(); v != v0 {
		t.Fatalf("version = %d, want %d (tagging is structural)", v, v0)
	}
	// Clearing removes the row entirely.
	if err := l.SetAccountTags("a", map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := l.AccountTagsOf("a"); ok {
		t.Fatal("AccountTagsOf after clear: want false")
	}
	if _, ok := l.AccountTagsOf("never-tagged"); ok {
		t.Fatal("AccountTagsOf on unknown account: want false")
	}
}

// TestTagRollupByTag aggregates balances per currency across the tag
// dimension, including tagged accounts with no postings and excluding
// untagged accounts.
func TestTagRollupByTag(t *testing.T) {
	l := New()
	post := func(id, acct string, cents int64, currency string) {
		t.Helper()
		_, _, err := l.Post(JournalEntry{ID: id, DebitAccount: AccountID(acct), CreditAccount: "bank", AmountCents: cents, Currency: currency})
		if err != nil {
			t.Fatal(err)
		}
	}
	post("e1", "retail-1", 1000, "USD")
	post("e2", "retail-2", 2000, "USD")
	post("e3", "retail-1", 500, "EUR")
	post("e4", "wholesale-1", 7000, "USD") // same key, different value: excluded
	post("e5", "untagged", 9000, "USD")    // no tags: excluded

	if err := l.SetAccountTags("retail-1", map[string]string{"line": "retail"}); err != nil {
		t.Fatal(err)
	}
	if err := l.SetAccountTags("retail-2", map[string]string{"line": "retail", "segment": "cross_border"}); err != nil {
		t.Fatal(err)
	}
	if err := l.SetAccountTags("wholesale-1", map[string]string{"line": "wholesale"}); err != nil {
		t.Fatal(err)
	}
	// Tagged but never posted: appears with zero contribution.
	if err := l.SetAccountTags("dormant", map[string]string{"line": "retail"}); err != nil {
		t.Fatal(err)
	}

	r := l.TagRollupByTag("line", "retail")
	if r.TagKey != "line" || r.TagValue != "retail" {
		t.Fatalf("tag key/value = %q/%q, want line/retail", r.TagKey, r.TagValue)
	}
	if r.Version != l.Version() {
		t.Fatalf("rollup version = %d, want %d", r.Version, l.Version())
	}
	wantAccounts := []AccountID{"dormant", "retail-1", "retail-2"}
	if len(r.Accounts) != len(wantAccounts) {
		t.Fatalf("accounts = %v, want %v", r.Accounts, wantAccounts)
	}
	for i, a := range wantAccounts {
		if r.Accounts[i] != a {
			t.Fatalf("accounts = %v, want %v", r.Accounts, wantAccounts)
		}
	}
	byCurrency := map[string]int64{}
	for _, row := range r.ByCurrency {
		byCurrency[row.Currency] = row.BalanceCents
	}
	if byCurrency["USD"] != 3000 {
		t.Fatalf("USD = %d, want 3000", byCurrency["USD"])
	}
	if byCurrency["EUR"] != 500 {
		t.Fatalf("EUR = %d, want 500", byCurrency["EUR"])
	}

	// A pair matching nothing is an empty table, not an error.
	empty := l.TagRollupByTag("line", "nonexistent")
	if len(empty.Accounts) != 0 || len(empty.ByCurrency) != 0 {
		t.Fatalf("empty rollup = %+v, want no accounts and no rows", empty)
	}

	// Balances from a different tag value are not mixed in.
	multi := l.TagRollupByTag("segment", "cross_border")
	if len(multi.Accounts) != 1 || multi.Accounts[0] != "retail-2" {
		t.Fatalf("segment=cross_border accounts = %v, want [retail-2]", multi.Accounts)
	}
}

// TestAccountTagsSnapshotRoundTrip pins the disaster-recovery guarantee:
// tags travel with the config record and import restores them exactly.
func TestAccountTagsSnapshotRoundTrip(t *testing.T) {
	l := New()
	if _, _, err := l.Post(JournalEntry{ID: "e1", DebitAccount: "a", CreditAccount: "bank", AmountCents: 100}); err != nil {
		t.Fatal(err)
	}
	if err := l.SetAccountTags("a", map[string]string{"line": "retail", "segment": "cross_border"}); err != nil {
		t.Fatal(err)
	}
	if err := l.SetAccountTags("b", map[string]string{"line": "wholesale"}); err != nil {
		t.Fatal(err)
	}

	var buf strings.Builder
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatal(err)
	}
	restored, err := ImportSnapshot(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatal(err)
	}
	gotA, ok := restored.AccountTagsOf("a")
	if !ok || gotA["line"] != "retail" || gotA["segment"] != "cross_border" {
		t.Fatalf("restored tags for a = %v (%v), want {line:retail segment:cross_border}", gotA, ok)
	}
	gotB, ok := restored.AccountTagsOf("b")
	if !ok || gotB["line"] != "wholesale" {
		t.Fatalf("restored tags for b = %v (%v), want {line:wholesale}", gotB, ok)
	}
	// The tag rollup must agree across the restore too.
	want := l.TagRollupByTag("line", "retail")
	got := restored.TagRollupByTag("line", "retail")
	if len(got.Accounts) != len(want.Accounts) || len(got.ByCurrency) != len(want.ByCurrency) {
		t.Fatalf("restored rollup = %+v, want %+v", got, want)
	}

	// A snapshot with a malformed tag row fails the import instead of
	// installing uncreatable tags.
	var tampered strings.Builder
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if strings.Contains(line, `"account_tags"`) {
			line = strings.Replace(line, `"line":"retail"`, `"line":"bad/key"`, 1)
		}
		tampered.WriteString(line + "\n")
	}
	if _, err := ImportSnapshot(strings.NewReader(tampered.String())); err == nil {
		t.Fatal("ImportSnapshot with malformed tag key: want error, got nil")
	}
}

// TestAccountTagsTrialBalanceAndReconcile covers the report surface: the
// trial balance carries the account's tags, and Reconcile lists every
// tagged account so downstream tooling can group rows by tag.
func TestAccountTagsTrialBalanceAndReconcile(t *testing.T) {
	l := New()
	if _, _, err := l.Post(JournalEntry{ID: "e1", DebitAccount: "a", CreditAccount: "bank", AmountCents: 100}); err != nil {
		t.Fatal(err)
	}
	if err := l.SetAccountTags("a", map[string]string{"line": "retail"}); err != nil {
		t.Fatal(err)
	}

	tb := l.TrialBalance("a")
	if tb.Tags["line"] != "retail" {
		t.Fatalf("trial balance tags = %v, want {line:retail}", tb.Tags)
	}
	tbUntagged := l.TrialBalance("bank")
	if len(tbUntagged.Tags) != 0 {
		t.Fatalf("untagged trial balance tags = %v, want empty", tbUntagged.Tags)
	}

	report := l.Reconcile(time.Now())
	if len(report.AccountTags) != 1 || report.AccountTags[0].Account != "a" {
		t.Fatalf("report account_tags = %+v, want [{a {line:retail}}]", report.AccountTags)
	}
	// Clearing the tags drops the row from the report.
	if err := l.SetAccountTags("a", nil); err != nil {
		t.Fatal(err)
	}
	if report := l.Reconcile(time.Now()); len(report.AccountTags) != 0 {
		t.Fatalf("report account_tags after clear = %+v, want empty", report.AccountTags)
	}
}

// TestSetAccountTagsAudited verifies the structured-audit trail: every
// tag change appends one account_tags_set event, and clears are marked.
func TestSetAccountTagsAudited(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	l := New(WithAuditLog(al))
	if err := l.SetAccountTags("a", map[string]string{"line": "retail"}); err != nil {
		t.Fatal(err)
	}
	if err := l.SetAccountTags("a", nil); err != nil {
		t.Fatal(err)
	}
	if err := al.Close(); err != nil {
		t.Fatal(err)
	}

	events, _, err := ReadAuditLog(dir, time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatal(err)
	}
	var sets []AuditEvent
	for _, ev := range events {
		if ev.Op == "account_tags_set" {
			sets = append(sets, ev)
		}
	}
	if len(sets) != 2 {
		t.Fatalf("account_tags_set events = %d, want 2", len(sets))
	}
	if sets[0].Details["tags"].(map[string]any)["line"] != "retail" {
		t.Fatalf("first set event details = %v, want tags.line=retail", sets[0].Details)
	}
	if sets[1].Details["tags_cleared"] != true {
		t.Fatalf("clear event details = %v, want tags_cleared=true", sets[1].Details)
	}
}
