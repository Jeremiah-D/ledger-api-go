package ledger

import (
	"errors"
	"fmt"
	"sort"
	"unicode/utf8"
)

// Account tags (LG-62).
//
// A tag is a small (key, value) label on an account: a merchant's
// business line ("line":"retail"), a regulatory segment ("segment":"cross_border"),
// a cost center ("cost_center":"ops"). Tags are classification, not
// bookkeeping — assigning or clearing them is structural, like SetParent
// or RegisterAccountType: it does not bump the ledger version, so a
// reporting change never looks like money movement to version-based
// change detectors. Tag changes are recorded in the structured audit
// chain ("account_tags_set"), so classification changes stay attributable
// for compliance.
//
// One account holds at most one value per key: SetAccountTags replaces the
// account's whole tag set atomically, and an empty map clears it. Tags
// round-trip through the disaster-recovery snapshot config record (see
// snapshot.go).
//
// Tags unlock the tag-dimension rollup: GET /rollup?tag=k:v aggregates the
// balances of every account carrying that (key, value) pair, per
// currency, without requiring a hierarchy link between them. Where the
// hierarchy rollup (see hierarchy.go) follows the sub-merchant tree, the
// tag rollup cuts across the tree — a report by business line or by
// segment over an arbitrary account set. Trial balances carry the
// account's tags and the Reconcile report lists every tagged account, so
// downstream reporting can group the same rows by tag without a second
// scan.

// AccountTag is one row of the account-tag classification: the tags
// assigned to an account. It is the unit the snapshot config record
// serializes (see snapshot.go).
type AccountTag struct {
	Account AccountID         `json:"account"`
	Tags    map[string]string `json:"tags"`
}

// Validation errors for account tags.
var (
	// ErrEmptyTagAccount is returned by SetAccountTags when the account
	// ID is empty.
	ErrEmptyTagAccount = errors.New("ledger: account tag account must not be empty")
	// ErrInvalidTagKey is returned by SetAccountTags when a tag key is
	// empty, too long, or carries characters outside [A-Za-z0-9._-].
	ErrInvalidTagKey = errors.New("ledger: account tag key must be 1-64 characters of [A-Za-z0-9._-]")
	// ErrInvalidTagValue is returned by SetAccountTags when a tag value
	// is empty, too long, or carries characters outside [A-Za-z0-9._-].
	ErrInvalidTagValue = errors.New("ledger: account tag value must be 1-64 characters of [A-Za-z0-9._-]")
)

// maxTagSegmentLen caps tag keys and values at 64 UTF-8 characters, the
// same bound as the external business reference (see ledger.go): long
// enough for "cost_center=apac_processing_01", short enough to keep the
// tag index compact.
const maxTagSegmentLen = 64

// CheckTagSegment is the exported form of checkTagSegment, for the HTTP
// layer's query validation: a tag key or value must be non-empty, at most
// 64 UTF-8 characters, and carry only [A-Za-z0-9._-] characters. The
// sentinel is ErrInvalidTagKey; the caller disambiguates key from value.
func CheckTagSegment(s string) error { return checkTagSegment(s, ErrInvalidTagKey) }

// checkTagSegment validates one tag key or value segment: non-empty, at
// most maxTagSegmentLen runes, and only the [A-Za-z0-9._-] characters the
// tenant-ID validator already allows (see tenant.go). The shared charset
// keeps tag values safe in URLs and JSON without escaping rules. base is
// the sentinel to wrap on failure (ErrInvalidTagKey or
// ErrInvalidTagValue).
func checkTagSegment(s string, base error) error {
	if s == "" || utf8.RuneCountInString(s) > maxTagSegmentLen {
		return fmt.Errorf("%w: %q", base, s)
	}
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '.' || r == '_' || r == '-' {
			continue
		}
		return fmt.Errorf("%w: invalid character %q in %q", base, r, s)
	}
	return nil
}

// checkAccountTags validates a tag set before it is installed: every key
// and value passes checkTagSegment. Keys and values have distinct error
// families so a caller can tell a malformed key from a malformed value.
func checkAccountTags(tags map[string]string) error {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := checkTagSegment(k, ErrInvalidTagKey); err != nil {
			return err
		}
		if err := checkTagSegment(tags[k], ErrInvalidTagValue); err != nil {
			return fmt.Errorf("%w: for key %q: value %q invalid", ErrInvalidTagValue, k, tags[k])
		}
	}
	return nil
}

// SetAccountTags replaces the tag set of an account, atomically. An
// empty map clears the account's tags. Setting tags is structural: it
// does not bump the journal version, and it survives disaster-recovery
// snapshots. The operation is audited as "account_tags_set" with the new
// set (an empty set records tags_cleared=true), so classification changes
// stay attributable. Tags apply to the account itself only — no tag
// inheritance through the sub-account hierarchy: inheritance would make
// "which accounts are tagged X" a transitive query and hide the
// classification in code, whereas the explicit set keeps the report
// auditable.
func (l *Ledger) SetAccountTags(account AccountID, tags map[string]string) error {
	if account == "" {
		return ErrEmptyTagAccount
	}
	if err := checkAccountTags(tags); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.setAccountTagsLocked(account, tags)
}

// setAccountTagsLocked is SetAccountTags's body. Callers must hold the
// write lock; the tag set must already be validated. It returns the error
// from the audit enqueue path (never in practice, since emitAudit cannot
// block or fail — the signature keeps the convention of the other
// structural setters).
func (l *Ledger) setAccountTagsLocked(account AccountID, tags map[string]string) error {
	if len(tags) == 0 {
		delete(l.tags, account)
	} else {
		cp := make(map[string]string, len(tags))
		for k, v := range tags {
			cp[k] = v
		}
		l.tags[account] = cp
	}
	details := map[string]any{
		"account": string(account),
		"tags":    l.tags[account],
	}
	if len(tags) == 0 {
		details["tags_cleared"] = true
	}
	l.emitAudit(AuditEvent{
		Op:       "account_tags_set",
		Actor:    "SetAccountTags",
		Accounts: []AccountID{account},
		Details:  details,
	})
	return nil
}

// AccountTagsOf reports the tag set assigned to an account. The second
// return value is false when the account carries no tags — either never
// tagged or cleared. The returned map is a copy: mutating it does not
// touch the ledger.
func (l *Ledger) AccountTagsOf(account AccountID) (map[string]string, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	t, ok := l.tags[account]
	if !ok {
		return nil, false
	}
	cp := make(map[string]string, len(t))
	for k, v := range t {
		cp[k] = v
	}
	return cp, true
}

// accountTagsLocked lists every account's tag set, sorted by account.
// Callers must hold l.mu; the read lock suffices.
func (l *Ledger) accountTagsLocked() []AccountTag {
	out := make([]AccountTag, 0, len(l.tags))
	for a, t := range l.tags {
		cp := make(map[string]string, len(t))
		for k, v := range t {
			cp[k] = v
		}
		out = append(out, AccountTag{Account: a, Tags: cp})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Account < out[j].Account })
	return out
}

// TagRollup aggregates the balances of every account carrying the (key,
// value) tag pair, per currency: the tag dimension across the whole
// ledger, unlike the hierarchy rollup (see hierarchy.go) which aggregates
// a subtree. The rows describe one consistent ledger version — the scan
// holds the read lock — and currencies are never summed together: each
// ByCurrency row stands on its own.
type TagRollup struct {
	// TagKey and TagValue name the pair this rollup aggregates: every
	// account whose tag set contains TagKey=TagValue contributed.
	TagKey   string `json:"tag_key"`
	TagValue string `json:"tag_value"`
	// Accounts are the matching accounts, sorted. An account with no
	// postings still appears when it is tagged, like the hierarchy
	// rollup's pre-settlement sub-merchants.
	Accounts []AccountID `json:"accounts"`
	// ByCurrency is the per-currency aggregated net balance of the
	// matching accounts, sorted by currency code.
	ByCurrency []RolledCurrencyBalance `json:"by_currency"`
	// Version is the ledger version the rollup was read at — the same
	// sequence Snapshot reports, so a rollup and a snapshot taken at one
	// version describe the same books.
	Version uint64 `json:"version"`
}

// tagMatches reports whether the account's tag set contains the pair.
func tagMatches(tags map[string]string, key, value string) bool {
	if tags == nil {
		return false
	}
	v, ok := tags[key]
	return ok && v == value
}

// TagRollupByTag returns the balance rollup of every account carrying the
// (key, value) tag pair. A key or value that matches no account rolls up
// to an empty account list with zero balances — like Rollup's unknown
// account, never an error, because an operator's report should show an
// empty table, not a 404, when nothing is tagged yet.
func (l *Ledger) TagRollupByTag(key, value string) TagRollup {
	l.mu.RLock()
	defer l.mu.RUnlock()
	matched := make([]AccountID, 0)
	for a, t := range l.tags {
		if tagMatches(t, key, value) {
			matched = append(matched, a)
		}
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i] < matched[j] })
	return l.tagRollupLocked(key, value, matched)
}

// tagRollupLocked builds the rollup for the given matching accounts.
// Callers must hold l.mu; the read lock suffices because nothing is
// mutated.
func (l *Ledger) tagRollupLocked(key, value string, matched []AccountID) TagRollup {
	seen := make(map[AccountID]bool, len(matched))
	for _, a := range matched {
		seen[a] = true
	}
	currencies := make(map[string]bool)
	for k := range l.balances {
		if seen[k.account] {
			currencies[k.currency] = true
		}
	}
	for k := range l.debitTotals {
		if seen[k.account] {
			currencies[k.currency] = true
		}
	}
	for k := range l.creditTotals {
		if seen[k.account] {
			currencies[k.currency] = true
		}
	}
	codes := make([]string, 0, len(currencies))
	for c := range currencies {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	byCurrency := make([]RolledCurrencyBalance, 0, len(codes))
	for _, c := range codes {
		var total int64
		for _, acct := range matched {
			total += l.balances[accountCurrency{account: acct, currency: c}]
		}
		byCurrency = append(byCurrency, RolledCurrencyBalance{
			Currency:     c,
			BalanceCents: total,
		})
	}
	return TagRollup{
		TagKey:     key,
		TagValue:   value,
		Accounts:   matched,
		ByCurrency: byCurrency,
		Version:    l.version,
	}
}
