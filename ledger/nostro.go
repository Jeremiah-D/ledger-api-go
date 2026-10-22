package ledger

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// Nostro/vostro correspondent-bank reconciliation (LG-175): the
// bank-to-bank counterpart of the bank-to-customer statement matching
// (see MatchStatement in statement_match.go).
//
// A nostro account is our money at a correspondent bank ("our account
// with them"); a vostro account is their money with us ("their
// account with us"). The correspondent reports what it sees on the
// account as statement lines — camt.053 import rows normalized with
// NewStatementLineFromCAMT053, or hand-built StatementLines (camt.052
// has no inbound parser in the ledger); the ledger holds what it
// booked as journal rows.
// MatchNostro runs the bilateral match: every expected (correspondent)
// line is claimed against an actual (journal) row, and every journal
// row is claimed against an expected line. The remainder is the break
// list:
//
//	expected_unreached  the correspondent shows a line the ledger
//	                    never booked (value-dating lag, a missing
//	                    posting)
//	actual_unreported   the ledger booked a row the correspondent
//	                    does not show (a late statement, an
//	                    unreleased payment)
//	amount_mismatch     the two sides agree on the identity (external
//	                    reference) but the amounts differ beyond the
//	                    tolerance
//
// Orthogonal to statement_match by design: that matcher answers "did
// my customer statement book?", this one answers "does my
// correspondent agree with my books?". They share the StatementLine
// type and nothing else — no registry, no audit coupling.
//
// Registration and expectation recording are operator calls that
// mutate an in-memory operational registry (the SetKyc convention):
// they bump no version and book no journal rows, but they emit one
// hash-chained audit event each. The match itself is a pure query —
// reconcileLocked calls it under the read lock, and the Reconcile
// report carries the aggregate break list as nostro_breaks.

// Nostro account kinds.
const (
	// NostroKindNostro: our account at the correspondent bank.
	NostroKindNostro = "nostro"
	// NostroKindVostro: the correspondent's account with us.
	NostroKindVostro = "vostro"
)

// Nostro errors.
var (
	// ErrNostroInvalidKind is returned when a nostro registration names
	// a kind outside the nostro/vostro vocabulary.
	ErrNostroInvalidKind = errors.New("ledger: invalid nostro kind: want nostro or vostro")
	// ErrNostroEmptyCorrespondent is returned when a nostro
	// registration names no correspondent: a correspondent account
	// without a correspondent is meaningless.
	ErrNostroEmptyCorrespondent = errors.New("ledger: nostro registration requires a correspondent")
	// ErrNostroNotRegistered is returned when expectations are
	// recorded for an account that was never registered as a nostro
	// account.
	ErrNostroNotRegistered = errors.New("ledger: account is not a registered nostro account")
	// ErrNostroLineAccountMismatch is returned when a recorded
	// expectation line names a different account than the registration
	// target: matching another account's correspondent lines into
	// this one would corrupt the reconciliation. Fail-closed, nothing
	// is stored.
	ErrNostroLineAccountMismatch = errors.New("ledger: nostro expectation line names a different account")
)

// NostroAccount is one registered correspondent account: the ledger
// account, the correspondent bank it sits with, and which side of the
// nostro/vostro relationship it is.
type NostroAccount struct {
	Account       AccountID `json:"account"`
	Correspondent string    `json:"correspondent"`
	Kind          string    `json:"kind"`
	RegisteredAt  time.Time `json:"registered_at"`
}

// NostroBreakKind is the taxonomy of bilateral breaks.
type NostroBreakKind string

const (
	// NostroExpectedUnreached: the correspondent shows a line the
	// ledger never booked.
	NostroExpectedUnreached NostroBreakKind = "expected_unreached"
	// NostroActualUnreported: the ledger booked a row the
	// correspondent does not show.
	NostroActualUnreported NostroBreakKind = "actual_unreported"
	// NostroAmountMismatch: both sides name the same identity
	// (external reference) but the amounts differ beyond tolerance.
	NostroAmountMismatch NostroBreakKind = "amount_mismatch"
)

// Nostro age buckets for break aging: how long the break has been
// open, as of the match time.
const (
	NostroAgeBucketCurrent = "0-1d"
	NostroAgeBucketWeek    = "2-7d"
	NostroAgeBucketMonth   = "8-30d"
	NostroAgeBucketStale   = ">30d"
)

// NostroBreak is one bilateral break: an expected line with no actual
// row, an actual row with no expected line, or an identity-agreed pair
// whose amounts disagree. ReferenceDate is the line's PostedAt (or
// the row's CreatedAt); AgeDays/AgeBucket age it as of the match.
type NostroBreak struct {
	Kind           NostroBreakKind    `json:"kind"`
	Account        AccountID          `json:"account"`
	Correspondent  string             `json:"correspondent"`
	ExpectedLineID string             `json:"expected_line_id,omitempty"`
	ActualEntryID  string             `json:"actual_entry_id,omitempty"`
	Currency       string             `json:"currency"`
	Direction      StatementDirection `json:"direction"`
	ExpectedCents  int64              `json:"expected_cents,omitempty"`
	ActualCents    int64              `json:"actual_cents,omitempty"`
	DifferenceCents int64             `json:"difference_cents,omitempty"`
	ReferenceDate  time.Time          `json:"reference_date"`
	AgeDays        int64              `json:"age_days"`
	AgeBucket      string             `json:"age_bucket"`
}

// NostroMatchReport is the full bilateral match report for one nostro
// account.
type NostroMatchReport struct {
	Account          AccountID      `json:"account"`
	Correspondent    string         `json:"correspondent"`
	Kind             string         `json:"kind"`
	GeneratedAt      time.Time      `json:"generated_at"`
	Expected         int            `json:"expected"`
	Actual           int            `json:"actual"`
	Matched          int            `json:"matched"`
	Ambiguous        int            `json:"ambiguous"`
	Breaks           []NostroBreak   `json:"breaks"`
	ExpectedUnreached int           `json:"expected_unreached"`
	ActualUnreported  int           `json:"actual_unreported"`
	AmountMismatches  int           `json:"amount_mismatches"`
}

// NostroMatchOptions tunes MatchNostro. AmountToleranceCents is the ±
// tolerance in cents around a line's |amount| inside which a journaled
// row counts as a candidate; negative means 0. MatchWindow is the ±
// window around a line's PostedAt inside which a journaled row's
// CreatedAt counts as a candidate; non-positive means
// DefaultOFXMatchWindow.
type NostroMatchOptions struct {
	AmountToleranceCents int64
	MatchWindow          time.Duration
}

// nostroActualRow is one effective journaled row on the actual side of
// the bilateral match, in Post order.
type nostroActualRow struct {
	id    string
	entry JournalEntry
}

// RegisterNostroAccount registers account as a nostro/vostro account
// held with correspondent. Re-registering overwrites the previous
// registration (the expectations already recorded stay — they name
// the same account). The registration emits one hash-chained
// nostro_account_registered audit event; it bumps no version and
// books nothing.
func (l *Ledger) RegisterNostroAccount(account AccountID, correspondent, kind string) (NostroAccount, error) {
	if account == "" {
		return NostroAccount{}, ErrEmptyAccountID
	}
	if correspondent == "" {
		return NostroAccount{}, ErrNostroEmptyCorrespondent
	}
	if kind != NostroKindNostro && kind != NostroKindVostro {
		return NostroAccount{}, fmt.Errorf("%w: %q", ErrNostroInvalidKind, kind)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.nostroAccounts == nil {
		l.nostroAccounts = make(map[AccountID]*NostroAccount)
	}
	rec := &NostroAccount{
		Account:       account,
		Correspondent: correspondent,
		Kind:          kind,
		RegisteredAt:  time.Now().UTC(),
	}
	l.nostroAccounts[account] = rec
	l.emitAudit(AuditEvent{
		Op:            "nostro_account_registered",
		Actor:         "RegisterNostroAccount",
		TraceID:       string(account),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{account},
		Details: map[string]any{
			"correspondent": correspondent,
			"kind":          kind,
		},
	})
	cp := *rec
	return cp, nil
}

// RecordNostroExpectations records the correspondent's expected
// statement lines for a registered nostro account — the "expected"
// side of the bilateral match (camt.053 import rows via
// NewStatementLineFromCAMT053, or hand-built StatementLines). Recording is append-by-line-ID: re-recording
// the same lines is an idempotent no-op (the statement-match
// re-import convention), and recording a line that names a different
// account is refused fail-closed with nothing stored. Returns the
// number of newly recorded lines. One hash-chained
// nostro_expectations_recorded audit event; no version bump, no
// journal rows.
func (l *Ledger) RecordNostroExpectations(account AccountID, lines []StatementLine) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.nostroAccounts == nil {
		return 0, fmt.Errorf("%w: %q", ErrNostroNotRegistered, account)
	}
	if _, ok := l.nostroAccounts[account]; !ok {
		return 0, fmt.Errorf("%w: %q", ErrNostroNotRegistered, account)
	}
	for _, line := range lines {
		if line.Account != account {
			return 0, fmt.Errorf("%w: line %q names %q", ErrNostroLineAccountMismatch, line.ID, line.Account)
		}
	}
	if l.nostroExpectations == nil {
		l.nostroExpectations = make(map[AccountID]map[string]StatementLine)
	}
	slot, ok := l.nostroExpectations[account]
	if !ok {
		slot = make(map[string]StatementLine)
		l.nostroExpectations[account] = slot
	}
	added := 0
	for _, line := range lines {
		if _, exists := slot[line.ID]; !exists {
			added++
		}
		slot[line.ID] = line
	}
	l.emitAudit(AuditEvent{
		Op:            "nostro_expectations_recorded",
		Actor:         "RecordNostroExpectations",
		TraceID:       string(account),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{account},
		Details: map[string]any{
			"lines": len(lines),
			"added": added,
		},
	})
	return added, nil
}

// GetNostroAccount returns the nostro registration for account, or
// false when the account was never registered. The returned record is
// a copy.
func (l *Ledger) GetNostroAccount(account AccountID) (NostroAccount, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	rec, ok := l.nostroAccounts[account]
	if !ok {
		return NostroAccount{}, false
	}
	cp := *rec
	return cp, true
}

// MatchNostro runs the bilateral match for one registered nostro
// account: every recorded expectation line is claimed against an
// effective journaled row touching the account, and every effective
// journaled row touching the account is claimed against an expectation
// line. The remainder is the break list with per-break aging as of
// now. A pure query: no version bump, no journal rows, no audit
// event — reconcileLocked calls it under the read lock.
func (l *Ledger) MatchNostro(account AccountID, now time.Time, opts NostroMatchOptions) (*NostroMatchReport, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.matchNostroLocked(account, now, opts)
}

// matchNostroLocked is MatchNostro's body for callers that already
// hold l.mu (either lock suffices — the scan mutates nothing).
func (l *Ledger) matchNostroLocked(account AccountID, now time.Time, opts NostroMatchOptions) (*NostroMatchReport, error) {
	reg, ok := l.nostroAccounts[account]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNostroNotRegistered, account)
	}
	tolerance := opts.AmountToleranceCents
	if tolerance < 0 {
		tolerance = 0
	}
	window := opts.MatchWindow
	if window <= 0 {
		window = DefaultOFXMatchWindow
	}

	// Expected side: the recorded correspondent lines, deterministic
	// order.
	var expected []StatementLine
	for _, line := range l.nostroExpectations[account] {
		expected = append(expected, line)
	}
	sort.Slice(expected, func(i, j int) bool { return expected[i].ID < expected[j].ID })

	// Actual side: effective journaled rows touching the account, in
	// Post order (byAccount is append-only).
	var actual []nostroActualRow
	for _, id := range l.byAccount[account] {
		e := l.entries[id]
		if e.PendingReview {
			continue
		}
		actual = append(actual, nostroActualRow{id: id, entry: e})
	}

	correspondent := reg.Correspondent
	report := &NostroMatchReport{
		Account:       account,
		Correspondent: correspondent,
		Kind:          reg.Kind,
		GeneratedAt:   now,
		Expected:      len(expected),
		Actual:        len(actual),
		Breaks:        []NostroBreak{},
	}
	claimed := make([]bool, len(actual))

	// Pass 1: claim each expected line. Identity (external reference)
	// wins over fuzzy candidates: a line whose ExternalRef names a
	// journaled row's external reference, idempotency key, or memo is
	// the same business event even when the amounts disagree — that
	// disagreement is the amount_mismatch break, not an unmatched
	// line.
	for _, line := range expected {
		if idx, found := nostroIdentityMatchLocked(line, account, actual, claimed); found {
			e := actual[idx].entry
			if nostroAbsDelta(e.AmountCents, line.AmountCents) <= tolerance {
				claimed[idx] = true
				report.Matched++
				continue
			}
			claimed[idx] = true
			report.AmountMismatches++
			report.Breaks = append(report.Breaks, nostroBreak(
				NostroAmountMismatch, account, correspondent, line, actual[idx].id,
				e.AmountCents, now))
			continue
		}
		cands := nostroFuzzyCandidatesLocked(line, account, actual, claimed, tolerance, window)
		switch len(cands) {
		case 0:
			report.ExpectedUnreached++
			report.Breaks = append(report.Breaks, nostroBreak(
				NostroExpectedUnreached, account, correspondent, line, "", 0, now))
		case 1:
			claimed[cands[0]] = true
			report.Matched++
		default:
			// Several journaled rows fit the line: the operator
			// disambiguates — claiming none rather than
			// misattributing. Not a break yet.
			report.Ambiguous++
		}
	}

	// Pass 2: every unclaimed actual row is a break the correspondent
	// does not show.
	for i, row := range actual {
		if claimed[i] {
			continue
		}
		report.ActualUnreported++
		b := NostroBreak{
			Kind:          NostroActualUnreported,
			Account:       account,
			Correspondent: correspondent,
			ActualEntryID: row.id,
			Currency:      row.entry.Currency,
			Direction:     nostroEntryDirection(row.entry, account),
			ActualCents:   row.entry.AmountCents,
			ReferenceDate: row.entry.CreatedAt,
		}
		b.AgeDays, b.AgeBucket = nostroAge(now, b.ReferenceDate)
		report.Breaks = append(report.Breaks, b)
	}

	sort.Slice(report.Breaks, func(i, j int) bool {
		a, b := report.Breaks[i], report.Breaks[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if !a.ReferenceDate.Equal(b.ReferenceDate) {
			return a.ReferenceDate.Before(b.ReferenceDate)
		}
		return a.ExpectedLineID+a.ActualEntryID < b.ExpectedLineID+b.ActualEntryID
	})
	return report, nil
}

// nostroIdentityMatchLocked finds the unclaimed actual row whose
// external reference, idempotency key, or memo exactly equals the
// line's external reference, with the direction agreeing. The
// journal's ExternalRef is unique per ledger, so an identity hit is
// the same business event.
func nostroIdentityMatchLocked(line StatementLine, account AccountID, actual []nostroActualRow, claimed []bool) (int, bool) {
	if line.ExternalRef == "" {
		return 0, false
	}
	for i, row := range actual {
		if claimed[i] {
			continue
		}
		e := row.entry
		if line.ExternalRef != e.ExternalRef &&
			line.ExternalRef != e.IdempotencyKey &&
			line.ExternalRef != e.Memo {
			continue
		}
		if nostroLineDirectionAgrees(line.Direction, e, account) {
			return i, true
		}
	}
	return 0, false
}

// nostroFuzzyCandidatesLocked returns the unclaimed actual rows that
// fit the line on currency, direction, |amount| ± tolerance, and
// CreatedAt ± window — the statementCandidatesLocked convention,
// without the score ranking: one candidate claims, several are
// ambiguous.
func nostroFuzzyCandidatesLocked(line StatementLine, account AccountID, actual []nostroActualRow, claimed []bool, tolerance int64, window time.Duration) []int {
	var out []int
	for i, row := range actual {
		if claimed[i] {
			continue
		}
		e := row.entry
		if e.Currency != line.Currency {
			continue
		}
		if !nostroLineDirectionAgrees(line.Direction, e, account) {
			continue
		}
		if nostroAbsDelta(e.AmountCents, line.AmountCents) > tolerance {
			continue
		}
		dt := e.CreatedAt.Sub(line.PostedAt)
		if dt < 0 {
			dt = -dt
		}
		if dt > window {
			continue
		}
		out = append(out, i)
	}
	return out
}

// nostroLineDirectionAgrees pins the account-centric direction: an
// "in" line means the account was the debit leg (money flowed in), an
// "out" line the credit leg — the StatementDirection convention.
func nostroLineDirectionAgrees(dir StatementDirection, e JournalEntry, account AccountID) bool {
	if dir == StatementDirectionIn {
		return e.DebitAccount == account
	}
	return e.CreditAccount == account
}

// nostroEntryDirection reports the account-centric direction of a
// journaled row for account.
func nostroEntryDirection(e JournalEntry, account AccountID) StatementDirection {
	if e.DebitAccount == account {
		return StatementDirectionIn
	}
	return StatementDirectionOut
}

// nostroAbsDelta is ||a| - |b||: the magnitude difference the
// tolerance gates.
func nostroAbsDelta(a, b int64) int64 {
	if a < 0 {
		a = -a
	}
	if b < 0 {
		b = -b
	}
	d := a - b
	if d < 0 {
		d = -d
	}
	return d
}

// nostroBreak builds one line-carrying break (expected_unreached or
// amount_mismatch) with aging as of now. The actual_unreported kind
// has no line and is built inline by the caller.
func nostroBreak(kind NostroBreakKind, account AccountID, correspondent string, line StatementLine, actualEntryID string, actualCents int64, now time.Time) NostroBreak {
	b := NostroBreak{
		Kind:           kind,
		Account:        account,
		Correspondent:  correspondent,
		ExpectedLineID: line.ID,
		ActualEntryID:  actualEntryID,
		Currency:       line.Currency,
		Direction:      line.Direction,
		ExpectedCents:  line.AmountCents,
		ReferenceDate:  line.PostedAt,
	}
	if kind == NostroAmountMismatch {
		b.ActualCents = actualCents
		b.DifferenceCents = line.AmountCents - actualCents
		if b.DifferenceCents < 0 {
			b.DifferenceCents = -b.DifferenceCents
		}
	}
	b.AgeDays, b.AgeBucket = nostroAge(now, b.ReferenceDate)
	return b
}

// nostroAge ages referenceDate as of now: whole days, never negative,
// bucketed into the NostroAgeBucket* bands.
func nostroAge(now, ref time.Time) (int64, string) {
	days := int64(now.Sub(ref).Hours() / 24)
	if days < 0 {
		days = 0
	}
	switch {
	case days <= 1:
		return days, NostroAgeBucketCurrent
	case days <= 7:
		return days, NostroAgeBucketWeek
	case days <= 30:
		return days, NostroAgeBucketMonth
	default:
		return days, NostroAgeBucketStale
	}
}

// nostroBreaksLocked aggregates the bilateral breaks across every
// registered nostro account, sorted by account: the Reconcile
// nostro_breaks section. Pure read — reconcileLocked calls it under
// the read lock.
func (l *Ledger) nostroBreaksLocked(now time.Time) []NostroBreak {
	var accounts []AccountID
	for a := range l.nostroAccounts {
		accounts = append(accounts, a)
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i] < accounts[j] })
	var out []NostroBreak
	for _, a := range accounts {
		rep, err := l.matchNostroLocked(a, now, NostroMatchOptions{})
		if err != nil {
			continue
		}
		out = append(out, rep.Breaks...)
	}
	return out
}
