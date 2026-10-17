package ledger

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
)

// Automatic bank-statement line matching (LG-107): the payment-fintech
// reconciliation counterpart of the statement imports (see ofx_import.go
// and camt053.go). A bank hands us a statement; the import parsers turn
// it into statement lines; MatchStatement matches each line against the
// account's journaled rows by amount (± tolerance), booking date (±
// window), and external_ref / memo keywords, with a per-line score.
//
// The outcome per line is matched (exactly one candidate), ambiguous
// (several candidates — the operator picks from the ranked list), or
// unmatched (no candidate). Unmatched lines are recorded and surfaced by
// Reconcile as TrialBalanceDiscrepancy rows through the same
// discrepancies mechanism the accounting-equation check uses — no second
// mechanism. Re-importing the same statement is idempotent: a line
// matched once stays matched to the same entry, and replays never
// double-count.
//
// Like the imports, matching is a draft operation, not a posting: it
// writes no journal rows, enters no hash chain, and bumps no version. It
// mutates only the statement-match registry, an operational in-memory
// index (like freezeEvents in statement.go): it does not round-trip
// through disaster-recovery snapshots, and the structured audit log
// (Op "statement_match") remains the durable record.

// StatementDirection is the money-movement direction of a statement
// line, account-centric like the imports: "in" means the account was
// the debit leg (money added to it), "out" means it was the credit leg
// (money left it).
type StatementDirection string

const (
	// StatementDirectionIn: the account is the debit leg — funds flowed
	// in.
	StatementDirectionIn StatementDirection = "in"
	// StatementDirectionOut: the account is the credit leg — funds
	// flowed out.
	StatementDirectionOut StatementDirection = "out"
)

// StatementLine is one normalized bank-statement line, as produced by
// the OFX and camt.053 import parsers (see NewStatementLineFromOFX and
// NewStatementLineFromCAMT053). ID is the stable line identity — the
// same ID DraftEntry carries ("ofx/<FITID>" or
// "camt053/<stmtID>/<seq>") — so re-imports of the same statement
// address the same lines. AmountCents is the absolute value in cents;
// the sign lives in Direction.
type StatementLine struct {
	ID             string             `json:"id"`
	Account        AccountID          `json:"account"`
	Currency       string             `json:"currency"`
	Direction      StatementDirection `json:"direction"`
	AmountCents    int64              `json:"amount_cents"`
	PostedAt       time.Time          `json:"posted_at"`
	Memo           string             `json:"memo,omitempty"`
	ExternalRef    string             `json:"external_ref,omitempty"`
	DocumentSHA256 string             `json:"document_sha256,omitempty"`
}

// NewStatementLineFromOFX normalizes an OFX statement line (see
// OFXTransaction in ofx_import.go) into a StatementLine.
func NewStatementLineFromOFX(t OFXTransaction, documentSHA256 string) StatementLine {
	dir := StatementDirectionIn
	if t.Direction == OFXDirectionOut {
		dir = StatementDirectionOut
	}
	return StatementLine{
		ID:             t.Draft().ID,
		Account:        t.Account,
		Currency:       t.Currency,
		Direction:      dir,
		AmountCents:    t.AmountCents,
		PostedAt:       t.PostedAt,
		Memo:           t.Memo,
		ExternalRef:    t.Ref,
		DocumentSHA256: documentSHA256,
	}
}

// NewStatementLineFromCAMT053 normalizes a camt.053 statement entry
// (see CAMT053Transaction in camt053.go) into a StatementLine.
func NewStatementLineFromCAMT053(t CAMT053Transaction, documentSHA256 string) StatementLine {
	dir := StatementDirectionIn
	if t.Direction == CAMT053DirectionOut {
		dir = StatementDirectionOut
	}
	return StatementLine{
		ID:             t.Draft().ID,
		Account:        t.Account,
		Currency:       t.Currency,
		Direction:      dir,
		AmountCents:    t.AmountCents,
		PostedAt:       t.PostedAt,
		Memo:           t.Memo,
		ExternalRef:    t.EndToEndID,
		DocumentSHA256: documentSHA256,
	}
}

// SignedAmountCents is the line's amount with the direction's sign:
// positive for inflows, negative for outflows — the delta the line
// claims on the account's balance.
func (l StatementLine) SignedAmountCents() int64 {
	if l.Direction == StatementDirectionOut {
		return -l.AmountCents
	}
	return l.AmountCents
}

// StatementMatchStatus is the matching outcome of one statement line
// against the journal.
type StatementMatchStatus string

const (
	// StatementMatched: exactly one journaled row is a candidate — the
	// statement line is already booked.
	StatementMatched StatementMatchStatus = "matched"
	// StatementAmbiguous: several journaled rows are candidates — the
	// operator disambiguates from the score-ranked list.
	StatementAmbiguous StatementMatchStatus = "ambiguous"
	// StatementUnmatched: no journaled row is a candidate — the line
	// enters the Reconcile discrepancies.
	StatementUnmatched StatementMatchStatus = "unmatched"
)

// StatementCandidate is one journaled row competing for a statement
// line, with its match score.
type StatementCandidate struct {
	EntryID string  `json:"entry_id"`
	Score   float64 `json:"score"`
}

// StatementMatchItem is one statement line with its match outcome. Score
// is the winning candidate's score when Status is matched, the best
// candidate's score when ambiguous, and 0 when unmatched. Candidates is
// the score-ranked candidate list, present only when ambiguous.
type StatementMatchItem struct {
	Line           StatementLine        `json:"line"`
	Status         StatementMatchStatus `json:"status"`
	Score          float64              `json:"score"`
	MatchedEntryID string               `json:"matched_entry_id,omitempty"`
	Candidates     []StatementCandidate `json:"candidates,omitempty"`
}

// StatementMatchReport is the full match report for one MatchStatement
// call.
type StatementMatchReport struct {
	Account   AccountID           `json:"account"`
	Items     []StatementMatchItem `json:"items"`
	Matched   int                 `json:"matched"`
	Ambiguous int                 `json:"ambiguous"`
	Unmatched int                 `json:"unmatched"`
}

// StatementMatchOptions tunes MatchStatement. AmountToleranceCents is
// the ± tolerance in cents around a line's |amount| inside which a
// journaled row counts as a candidate; negative means 0.
// MatchWindow is the ± window around a line's PostedAt inside which a
// journaled row's CreatedAt counts as a candidate; non-positive means
// DefaultOFXMatchWindow.
type StatementMatchOptions struct {
	AmountToleranceCents int64
	MatchWindow          time.Duration
}

// statementMatchRecord is one row of the statement-match registry (see
// statementMatches on Ledger): the line's last outcome, keyed by the
// line's stable ID. A matched line replays its recorded entry on
// re-import; an unmatched or ambiguous line is re-scanned because the
// journal only grows — a newly booked row can resolve it.
type statementMatchRecord struct {
	line           StatementLine
	status         StatementMatchStatus
	matchedEntryID string
	score          float64
}

// MatchStatement matches statement lines (as produced by the OFX and
// camt.053 import parsers) against the account's journaled rows and
// returns the per-line outcomes with scores.
//
// A journaled row is a candidate for a line when it is effective
// (pending-review rows never touched balances, so they cannot match a
// bank statement — the same rule the imports apply), touches the
// account, carries the same currency, has |amount| within ±
// AmountToleranceCents of the line's, and its CreatedAt falls within ±
// MatchWindow of the line's PostedAt. Exactly one candidate is matched;
// several is ambiguous; none is unmatched. The score (0..1) ranks how
// close a candidate is: amount closeness, date closeness, and an
// external_ref exact match (or memo keyword overlap) bonus.
//
// Matching is idempotent per line ID: a line recorded as matched replays
// its recorded entry — re-importing the same statement never
// double-matches. Unmatched and ambiguous lines are re-scanned on every
// call, so booking the missing row later resolves the line and clears
// its Reconcile discrepancy. Unmatched lines are recorded in the
// statement-match registry, which Reconcile surfaces as
// TrialBalanceDiscrepancy rows.
//
// A line naming a different account than the match target is an error:
// matching another account's statement lines into this one would corrupt
// the reconciliation.
func (l *Ledger) MatchStatement(account AccountID, lines []StatementLine, opts StatementMatchOptions) (*StatementMatchReport, error) {
	if account == "" {
		return nil, ErrEmptyAccountID
	}
	window := opts.MatchWindow
	if window <= 0 {
		window = DefaultOFXMatchWindow
	}
	tolerance := opts.AmountToleranceCents
	if tolerance < 0 {
		tolerance = 0
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.statementMatches == nil {
		l.statementMatches = make(map[string]*statementMatchRecord)
	}

	report := &StatementMatchReport{
		Account: account,
		Items:   []StatementMatchItem{},
	}
	for _, line := range lines {
		if line.Account != account {
			return nil, fmt.Errorf("ledger: statement line %q names account %q, match targets %q",
				line.ID, line.Account, account)
		}
		item := l.matchStatementLineLocked(line, tolerance, window)
		switch item.Status {
		case StatementMatched:
			report.Matched++
		case StatementAmbiguous:
			report.Ambiguous++
		default:
			report.Unmatched++
		}
		report.Items = append(report.Items, item)
	}

	// Matching is a draft operation: it writes no journal rows and bumps
	// no version — the event still records that matching ran, against
	// which ledger state, and what it found.
	l.emitAudit(AuditEvent{
		Op:            "statement_match",
		Actor:         "MatchStatement",
		TraceID:       fmt.Sprintf("statement_match@%d", l.version),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"account":   string(account),
			"lines":     len(lines),
			"matched":   report.Matched,
			"ambiguous": report.Ambiguous,
			"unmatched": report.Unmatched,
		},
	})
	return report, nil
}

// matchStatementLineLocked matches one line and records the outcome in
// the registry. Callers must hold l.mu (the write lock — the registry
// is mutated).
func (l *Ledger) matchStatementLineLocked(line StatementLine, tolerance int64, window time.Duration) StatementMatchItem {
	if rec, ok := l.statementMatches[line.ID]; ok &&
		rec.status == StatementMatched &&
		statementLineIdentityEqual(rec.line, line) {
		if _, exists := l.entries[rec.matchedEntryID]; exists {
			// A line matched once stays matched to the same entry:
			// re-imports replay the recorded outcome instead of
			// re-running the scan, so the same file can never
			// double-match.
			return StatementMatchItem{
				Line:           line,
				Status:         StatementMatched,
				Score:          rec.score,
				MatchedEntryID: rec.matchedEntryID,
			}
		}
		// The recorded entry is gone (only reachable through exotic
		// paths — the journal is append-only): fall through and
		// re-scan rather than replaying a dangling match.
	}

	cands := l.statementCandidatesLocked(line, tolerance, window)
	item := StatementMatchItem{Line: line}
	var rec *statementMatchRecord
	switch len(cands) {
	case 0:
		item.Status = StatementUnmatched
		rec = &statementMatchRecord{line: line, status: StatementUnmatched}
	case 1:
		item.Status = StatementMatched
		item.Score = cands[0].Score
		item.MatchedEntryID = cands[0].EntryID
		rec = &statementMatchRecord{
			line: line, status: StatementMatched,
			matchedEntryID: cands[0].EntryID, score: cands[0].Score,
		}
	default:
		item.Status = StatementAmbiguous
		item.Score = cands[0].Score
		item.Candidates = cands
		rec = &statementMatchRecord{line: line, status: StatementAmbiguous, score: cands[0].Score}
	}
	l.statementMatches[line.ID] = rec
	return item
}

// statementLineIdentityEqual reports whether two lines are the same
// statement line: the matching-relevant fields agree. A re-import whose
// stable ID survived but whose substance changed (a pathological
// document) re-scans instead of replaying a stale match.
func statementLineIdentityEqual(a, b StatementLine) bool {
	return a.ID == b.ID &&
		a.Account == b.Account &&
		a.Currency == b.Currency &&
		a.Direction == b.Direction &&
		a.AmountCents == b.AmountCents &&
		a.PostedAt.Equal(b.PostedAt)
}

// statementCandidatesLocked returns the score-ranked candidates for one
// line: effective journaled rows for (account, currency) whose |amount|
// is within ±tolerance of the line's and whose CreatedAt is within
// ±window of the line's PostedAt. Sorted by score descending, entry ID
// ascending for determinism. Callers must hold l.mu (either lock
// suffices).
func (l *Ledger) statementCandidatesLocked(line StatementLine, tolerance int64, window time.Duration) []StatementCandidate {
	var out []StatementCandidate
	for _, id := range l.byAccount[line.Account] {
		e := l.entries[id]
		if e.PendingReview || e.Currency != line.Currency {
			continue
		}
		amt := e.AmountCents
		if amt < 0 {
			amt = -amt
		}
		delta := amt - line.AmountCents
		if delta < 0 {
			delta = -delta
		}
		if delta > tolerance {
			continue
		}
		dt := e.CreatedAt.Sub(line.PostedAt)
		if dt < 0 {
			dt = -dt
		}
		if dt > window {
			continue
		}
		out = append(out, StatementCandidate{
			EntryID: id,
			Score:   statementMatchScore(e, line, delta, dt, tolerance, window),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].EntryID < out[j].EntryID
	})
	return out
}

// statementMatchScore scores one candidate in [0,1]: half amount
// closeness, three-tenths date closeness, two-tenths reference evidence.
// An exact external_ref match on both sides is full reference evidence;
// otherwise the memo keyword overlap decides; with no reference text on
// either side there is no reference evidence. delta and dt are already
// known to be within tolerance and window, so every component — and the
// total — stays in [0,1].
func statementMatchScore(e JournalEntry, line StatementLine, delta int64, dt time.Duration, tolerance int64, window time.Duration) float64 {
	amountScore := 1 - float64(delta)/float64(tolerance+1)
	dateScore := 1 - float64(dt)/float64(window)
	var refScore float64
	switch {
	case line.ExternalRef != "" && e.ExternalRef != "":
		if strings.EqualFold(line.ExternalRef, e.ExternalRef) {
			refScore = 1
		}
	default:
		refScore = memoKeywordOverlap(line.Memo, e.Memo)
	}
	return 0.5*amountScore + 0.3*dateScore + 0.2*refScore
}

// memoKeywordOverlap is the Jaccard similarity of the two memos'
// keyword sets (lower-cased alphanumeric runs): 1 for identical word
// sets, 0 for disjoint ones or when both memos are empty. A tolerant
// reader — banks rewrite memo text freely, so keywords rank candidates
// rather than gating them.
func memoKeywordOverlap(a, b string) float64 {
	sa, sb := memoKeywords(a), memoKeywords(b)
	if len(sa) == 0 && len(sb) == 0 {
		return 0
	}
	inter := 0
	for w := range sa {
		if _, ok := sb[w]; ok {
			inter++
		}
	}
	union := len(sa) + len(sb) - inter
	return float64(inter) / float64(union)
}

// memoKeywords splits s into its lower-cased alphanumeric keyword set.
func memoKeywords(s string) map[string]struct{} {
	out := make(map[string]struct{})
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			out[b.String()] = struct{}{}
			b.Reset()
		}
	}
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

// unmatchedStatementMatchesLocked returns the still-unmatched
// statement-match records, sorted by (account, currency, line ID) for a
// deterministic Reconcile. Callers must hold l.mu; the read lock
// suffices.
func (l *Ledger) unmatchedStatementMatchesLocked() []*statementMatchRecord {
	var out []*statementMatchRecord
	for _, rec := range l.statementMatches {
		if rec.status == StatementUnmatched {
			out = append(out, rec)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].line, out[j].line
		if a.Account != b.Account {
			return a.Account < b.Account
		}
		if a.Currency != b.Currency {
			return a.Currency < b.Currency
		}
		return a.ID < b.ID
	})
	return out
}
