package ledger

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Automatic posting rules for domain events (LG-132).
//
// Journal rules (see rules.go) fire on raw Post journal entries; they do
// not see the payment-domain operations. Posting rules fill the gap: they
// evaluate on transfer, capture, and sweep events and append derived
// journal legs — the classic payment-fintech automations: a channel fee
// on a merchant settlement transfer, a tax withholding on a payout
// capture, a reserve on a treasury sweep. The trigger event and the rule
// legs commit atomically: a rule leg that fails any gate rejects the
// whole event, and readers never observe a transfer without its rule
// legs. Rejected events record nothing: no journal rows, no chain
// links, no version bump.
//
// A posting rule is deployment config, like the transfer fee policy
// (see transfer.go): rules are installed at construction with
// WithPostingRules, validated fail-fast, and cannot be added or removed
// at runtime. Structural config, like Freeze or SetDailyLimit: rule
// installation does not bump the journal version, and the rule set
// round-trips through the disaster-recovery snapshot config record
// (see snapshot.go).
//
// A PostingRule names an event kind — "transfer", "capture", or
// "sweep" — and an optional match predicate: MinAmountCents (the event
// amount must be at least this), Currency (the event currency must
// equal this), and AccountPrefix (the payer account must start with
// this). Every leg of a matching event books a derived entry:
//
//	Leg entry ID:  "<triggerID>/rule:<ruleName>"      ("/<i>" appended
//	               when the rule defines several legs)
//	BatchID:       <triggerID>  (the transfer/capture/sweep the rule fired on)
//	Memo:          "rule:<ruleName>"
//	Currency:      the event's currency
//
// The trigger ID is the transfer ID, the capture entry ID, or the sweep
// leg entry ID ("<sweepID>/<source>/<currency>"): sweep rules evaluate
// per sweep leg, so each sub-merchant settlement books its own legs.
// A derived leg that computes to zero cents (a tiny amount under a low
// basis-point rate) books nothing, mirroring the transfer fee policy's
// dust tiers and the journal rules' zero-leg rule; a rule whose legs
// could never book anything is rejected at construction instead.
//
// Each leg resolves its counterparty from the event: the account template
// may embed the {payer} and {payee} placeholders — e.g. "channel-fee"
// or "{payer}-tax" — and the Side selects which side of the derived entry
// the resolved account takes: "debit" debits the resolved account and
// credits the payer (the classic charge-on-top fee), "credit" does the
// reverse. A template that resolves to the payer on the wrong side (a
// debit==credit leg) fails the event fail-closed.
//
// Gate order mirrors the fee-leg treatment in PostTransfer: each derived
// leg re-runs frozen, closed, suspended, and overdraft on the aggregate
// credit-side outflow before anything books. Rule legs are deliberately
// excluded from the daily-outflow and monthly-spend accounting: the
// transfer's own gates already evaluated the payer's principal outflow,
// and micro-charges are internal settlement cost, not operator spend.
// Derived legs never re-trigger posting rules — evaluation is one level
// deep by construction, so a rule set cannot cascade.

// Posting rule event kinds: the closed set a rule may name.
const (
	// PostingEventTransfer fires on PostTransfer (both the plain and
	// the cross-currency paths): payer = the transfer's From account,
	// payee = To, amount = AmountCents, currency = the transfer's
	// currency, trigger = the transfer ID.
	PostingEventTransfer = "transfer"
	// PostingEventCapture fires on Capture: payer = the hold's account,
	// payee = the capture target, amount = the captured cents,
	// currency = the hold's currency, trigger = the capture entry ID.
	PostingEventCapture = "capture"
	// PostingEventSweep fires per sweep leg on PostSweep: payer = the
	// source account, payee = the sweep target, amount = the leg's
	// cents, currency = the leg's currency, trigger = the leg entry
	// ID "<sweepID>/<source>/<currency>".
	PostingEventSweep = "sweep"
)

// Validation errors for posting rules.
var (
	// ErrPostingRuleEmptyName is returned when the rule has no name.
	ErrPostingRuleEmptyName = errors.New("ledger: posting rule name must not be empty")
	// ErrPostingRuleNameTooLong is returned when the rule name exceeds
	// maxPostingRuleNameLen characters: the bound keeps the generated
	// derived-leg IDs ("<triggerID>/rule:<name>") comfortably short.
	ErrPostingRuleNameTooLong = errors.New("ledger: posting rule name must be at most 64 characters")
	// ErrPostingRuleNameSeparator is returned when the rule name
	// contains '/': the separator is reserved for derived-leg IDs, so a
	// name containing it would produce ambiguous journal entry IDs.
	ErrPostingRuleNameSeparator = errors.New("ledger: posting rule name must not contain '/'")
	// ErrPostingRuleBadKind is returned when the rule names an event
	// kind outside the closed set (transfer/capture/sweep).
	ErrPostingRuleBadKind = errors.New("ledger: posting rule event kind must be transfer, capture, or sweep")
	// ErrPostingRuleNoLegs is returned when the rule defines no legs: a
	// rule that appends nothing is a caller bug, not a no-op.
	ErrPostingRuleNoLegs = errors.New("ledger: posting rule must define at least one leg")
	// ErrPostingRuleDuplicateName is returned by WithPostingRules when
	// two rules share a name: names identify the derived legs, so they
	// must be unique.
	ErrPostingRuleDuplicateName = errors.New("ledger: posting rule names must be unique")
	// ErrPostingRuleEmptyTemplate is returned when a leg's account
	// template is empty.
	ErrPostingRuleEmptyTemplate = errors.New("ledger: posting rule leg account template must not be empty")
	// ErrPostingRuleTemplateTooLong is returned when a leg's account
	// template exceeds maxPostingRuleTemplateLen characters.
	ErrPostingRuleTemplateTooLong = errors.New("ledger: posting rule leg account template must be at most 128 characters")
	// ErrPostingRuleBadPlaceholder is returned when a leg's account
	// template contains a placeholder outside the {payer}/{payee}
	// whitelist: only the event's two parties may be interpolated, so a
	// typo cannot silently resolve to a wrong account.
	ErrPostingRuleBadPlaceholder = errors.New("ledger: posting rule leg account template may only use the {payer} and {payee} placeholders")
	// ErrPostingRuleBadSide is returned when a leg's side is not
	// "debit" or "credit".
	ErrPostingRuleBadSide = errors.New("ledger: posting rule leg side must be \"debit\" or \"credit\"")
	// ErrPostingRuleBadAmountExpr is returned when a leg's amount
	// expression is not "bps:<1..10000>" or "fixed:<positive>".
	ErrPostingRuleBadAmountExpr = errors.New("ledger: posting rule leg amount expression must be \"bps:<1..10000>\" or \"fixed:<positive cents>\"")
	// ErrPostingRuleZeroLeg is returned when a leg's amount expression
	// can never book anything: a bps rate above 100% of the event is a
	// configuration bug, not a clampable fee (mirrors the journal
	// rules' ErrJournalRuleLegAmount).
	ErrPostingRuleZeroLeg = errors.New("ledger: posting rule leg amount expression is never positive")
	// ErrPostingRuleMatchBounds is returned when MinAmountCents is
	// negative: a rule that can never match is a caller bug.
	ErrPostingRuleMatchBounds = errors.New("ledger: posting rule min amount must not be negative")
	// ErrPostingRuleLegConflict is returned when a derived rule leg's
	// deterministic ID collides with an existing journal entry. The
	// whole event is rejected and records nothing — silently
	// overwriting a journaled entry is never an option.
	ErrPostingRuleLegConflict = errors.New("ledger: derived posting rule leg ID collides with an existing journal entry")
	// ErrPostingRuleSelfLeg is returned when a leg's resolved accounts
	// are identical: a debit==credit leg is a no-op that would corrupt
	// the accounting equation, so the event fails closed.
	ErrPostingRuleSelfLeg = errors.New("ledger: posting rule leg resolved to identical debit and credit accounts")
)

// Bounds for posting rule fields.
const (
	maxPostingRuleNameLen     = 64
	maxPostingRuleTemplateLen = 128
)

// PostingRuleLeg is one derived journal leg a posting rule appends to a
// matching event. The amount is either a proportion of the event amount
// (AmountExpr "bps:<X>", X in 1..10000, computed as floor(event *
// X / 10000) overflow-safe like the transfer fee policy) or a fixed cent
// value (AmountExpr "fixed:<N>", N positive).
type PostingRuleLeg struct {
	// AccountTemplate names the derived entry's non-payer account,
	// with {payer} and {payee} interpolated from the event. Side
	// "debit" books the resolved account as the debit leg (credited by
	// the payer); side "credit" books it as the credit leg (debited by
	// the payer).
	AccountTemplate string `json:"account_template"`
	Side            string `json:"side"`
	AmountExpr      string `json:"amount_expr"`
}

// PostingRule is one automatic posting rule: when an event of EventKind
// matches the optional predicate, each Leg books a derived journal leg
// (see the package doc).
type PostingRule struct {
	// Name identifies the rule and names the derived legs
	// ("<triggerID>/rule:<name>"): unique per ledger, at most 64
	// characters, no '/'.
	Name string `json:"name"`
	// EventKind is one of "transfer", "capture", "sweep".
	EventKind string `json:"event_kind"`
	// MinAmountCents, when positive, requires the event amount to be
	// at least this many cents for the rule to fire.
	MinAmountCents int64 `json:"min_amount_cents,omitempty"`
	// Currency, when set, requires the event currency to equal it.
	Currency string `json:"currency,omitempty"`
	// AccountPrefix, when set, requires the payer account to start
	// with it (the merchant-prefix filter for settlement rules).
	AccountPrefix string `json:"account_prefix,omitempty"`
	Legs          []PostingRuleLeg `json:"legs"`
}

// PostingRuleLegInfo discloses one booked rule leg on a receipt: the
// rule that fired, the journal entry it booked, and the accounts it
// touched.
type PostingRuleLegInfo struct {
	Rule          string    `json:"rule"`
	EntryID       string    `json:"entry_id"`
	DebitAccount  AccountID `json:"debit_account"`
	CreditAccount AccountID `json:"credit_account"`
	AmountCents   int64     `json:"amount_cents"`
	Currency      string    `json:"currency"`
}

// postingRuleAmount holds a parsed leg amount expression.
type postingRuleAmount struct {
	bps   int64 // >0 when the expression is bps:<X>
	fixed int64 // >0 when the expression is fixed:<N>
}

// parsePostingRuleAmountExpr parses "bps:<X>" / "fixed:<N>".
func parsePostingRuleAmountExpr(expr string) (postingRuleAmount, error) {
	kind, val, ok := strings.Cut(expr, ":")
	if !ok {
		return postingRuleAmount{}, ErrPostingRuleBadAmountExpr
	}
	switch kind {
	case "bps":
		n, err := parsePostingRuleInt(val)
		if err != nil || n < 1 || n > 10000 {
			return postingRuleAmount{}, ErrPostingRuleBadAmountExpr
		}
		return postingRuleAmount{bps: n}, nil
	case "fixed":
		n, err := parsePostingRuleInt(val)
		if err != nil || n < 1 {
			return postingRuleAmount{}, ErrPostingRuleBadAmountExpr
		}
		return postingRuleAmount{fixed: n}, nil
	}
	return postingRuleAmount{}, ErrPostingRuleBadAmountExpr
}

func parsePostingRuleInt(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty")
	}
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errors.New("not a number")
		}
		n = n*10 + int64(c-'0')
	}
	return n, nil
}

// validatePostingRuleTemplate checks the {payer}/{payee} placeholder
// whitelist: any other {...} is rejected, and a template that is only
// placeholders is fine (it resolves from the event).
func validatePostingRuleTemplate(tmpl string) error {
	if tmpl == "" {
		return ErrPostingRuleEmptyTemplate
	}
	if len(tmpl) > maxPostingRuleTemplateLen {
		return ErrPostingRuleTemplateTooLong
	}
	for i := 0; i < len(tmpl); i++ {
		if tmpl[i] != '{' {
			continue
		}
		end := strings.IndexByte(tmpl[i:], '}')
		if end < 0 {
			return ErrPostingRuleBadPlaceholder
		}
		ph := tmpl[i : i+end+1]
		if ph != "{payer}" && ph != "{payee}" {
			return ErrPostingRuleBadPlaceholder
		}
		i += end
	}
	return nil
}

// ValidatePostingRule checks a posting rule and normalizes the currency
// code, returning the normalized copy. It is the fail-fast gate shared
// by WithPostingRules and the snapshot import.
func ValidatePostingRule(r PostingRule) (PostingRule, error) {
	if r.Name == "" {
		return r, ErrPostingRuleEmptyName
	}
	if len(r.Name) > maxPostingRuleNameLen {
		return r, ErrPostingRuleNameTooLong
	}
	if strings.Contains(r.Name, "/") {
		return r, ErrPostingRuleNameSeparator
	}
	switch r.EventKind {
	case PostingEventTransfer, PostingEventCapture, PostingEventSweep:
	default:
		return r, fmt.Errorf("%w: %q", ErrPostingRuleBadKind, r.EventKind)
	}
	if r.MinAmountCents < 0 {
		return r, ErrPostingRuleMatchBounds
	}
	if r.Currency != "" {
		cur, err := normalizeCurrency(r.Currency)
		if err != nil {
			return r, err
		}
		r.Currency = cur
	}
	if len(r.Legs) == 0 {
		return r, ErrPostingRuleNoLegs
	}
	for i := range r.Legs {
		leg := r.Legs[i]
		if err := validatePostingRuleTemplate(leg.AccountTemplate); err != nil {
			return r, err
		}
		switch leg.Side {
		case "debit", "credit":
		default:
			return r, fmt.Errorf("%w: %q", ErrPostingRuleBadSide, leg.Side)
		}
		if _, err := parsePostingRuleAmountExpr(leg.AmountExpr); err != nil {
			return r, fmt.Errorf("%w: %q", err, leg.AmountExpr)
		}
	}
	return r, nil
}

// WithPostingRules installs the ledger-wide automatic posting rules
// (see the package doc): rules evaluate on transfer, capture, and sweep
// events and append derived journal legs that commit atomically with the
// event. Every rule is validated fail-fast at construction — a bad rule
// panics, like the fee schedule's fail-fast (see normalizeFeeSchedule),
// so a misconfigured rule set can never silently mis-book. An empty
// list installs no rules: the ledger posts exactly as before.
func WithPostingRules(rules []PostingRule) Option {
	return func(l *Ledger) {
		seen := make(map[string]bool, len(rules))
		out := make([]PostingRule, 0, len(rules))
		for _, r := range rules {
			nr, err := ValidatePostingRule(r)
			if err != nil {
				panic(fmt.Sprintf("ledger: invalid posting rule %q: %v", r.Name, err))
			}
			if seen[nr.Name] {
				panic(fmt.Sprintf("ledger: %v: %q", ErrPostingRuleDuplicateName, nr.Name))
			}
			seen[nr.Name] = true
			out = append(out, nr)
		}
		l.postingRules = out
	}
}

// PostingRules returns the installed posting rules in registration
// order.
func (l *Ledger) PostingRules() []PostingRule {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]PostingRule, len(l.postingRules))
	copy(out, l.postingRules)
	return out
}

// postingRuleContext is one event a posting rule may fire on.
type postingRuleContext struct {
	kind        string // transfer | capture | sweep
	payer       AccountID
	payee       AccountID
	amountCents int64
	currency    string
	triggerID   string // transfer ID, capture entry ID, or sweep leg entry ID
}

// postingRuleMatches reports whether the rule fires on the event.
func postingRuleMatches(r PostingRule, ctx postingRuleContext) bool {
	if r.EventKind != ctx.kind {
		return false
	}
	if r.MinAmountCents > 0 && ctx.amountCents < r.MinAmountCents {
		return false
	}
	if r.Currency != "" && r.Currency != ctx.currency {
		return false
	}
	if r.AccountPrefix != "" && !strings.HasPrefix(string(ctx.payer), r.AccountPrefix) {
		return false
	}
	return true
}

// resolvePostingRuleTemplate interpolates {payer}/{payee} from the event.
func resolvePostingRuleTemplate(tmpl string, ctx postingRuleContext) AccountID {
	s := strings.ReplaceAll(tmpl, "{payer}", string(ctx.payer))
	s = strings.ReplaceAll(s, "{payee}", string(ctx.payee))
	return AccountID(s)
}

// resolvePostingRuleLegsLocked evaluates every installed rule against
// the event and builds the derived journal legs: one entry per leg of
// every matching rule, in registration order. Each leg re-runs the
// frozen, closed, suspended, and overdraft gates before anything books,
// and a leg whose deterministic ID collides with an existing entry
// rejects the whole event — the caller must treat any error as fatal
// for the event (zero partial booking). Callers must hold l.mu with the
// write lock; createdAt is the event's commit timestamp.
func (l *Ledger) resolvePostingRuleLegsLocked(ctx postingRuleContext, createdAt time.Time) ([]JournalEntry, []PostingRuleLegInfo, error) {
	var entries []JournalEntry
	var infos []PostingRuleLegInfo
	for _, r := range l.postingRules {
		if !postingRuleMatches(r, ctx) {
			continue
		}
		multi := len(r.Legs) > 1
		for i, leg := range r.Legs {
			amt, err := parsePostingRuleAmountExpr(leg.AmountExpr)
			if err != nil {
				return nil, nil, fmt.Errorf("ledger: posting rule %q leg %d: %w", r.Name, i, err)
			}
			var cents int64
			switch {
			case amt.bps > 0:
				cents = policyFeeCents(ctx.amountCents, amt.bps)
			default:
				cents = amt.fixed
			}
			if cents <= 0 {
				// Dust: a zero-cent derived leg books nothing,
				// mirroring the transfer fee policy and the journal
				// rules.
				continue
			}
			resolved := resolvePostingRuleTemplate(leg.AccountTemplate, ctx)
			if resolved == "" {
				return nil, nil, fmt.Errorf("ledger: posting rule %q leg %d: template resolved to an empty account", r.Name, i)
			}
			e := JournalEntry{
				BatchID:     ctx.triggerID,
				Memo:        "rule:" + r.Name,
				AmountCents: cents,
				Currency:    ctx.currency,
				CreatedAt:   createdAt,
			}
			if leg.Side == "debit" {
				e.DebitAccount = resolved
				e.CreditAccount = ctx.payer
			} else {
				e.DebitAccount = ctx.payer
				e.CreditAccount = resolved
			}
			if e.DebitAccount == e.CreditAccount {
				return nil, nil, fmt.Errorf("%w: rule %q", ErrPostingRuleSelfLeg, r.Name)
			}
			e.ID = ctx.triggerID + "/rule:" + r.Name
			if multi {
				e.ID += fmt.Sprintf("/%d", i)
			}
			if _, exists := l.entries[e.ID]; exists {
				return nil, nil, fmt.Errorf("%w: %q", ErrPostingRuleLegConflict, e.ID)
			}
			// Risk gates, like the transfer fee legs: a rule leg never
			// books through a frozen, closed, or suspended account, and
			// never overdraws a protected payer. A failing leg rejects
			// the whole event — the caller returned before any commit,
			// so nothing needs rolling back.
			if l.closedLocked(e.DebitAccount) || l.closedLocked(e.CreditAccount) {
				return nil, nil, ErrAccountClosed
			}
			if l.frozenLocked(e.DebitAccount) || l.frozenLocked(e.CreditAccount) {
				return nil, nil, ErrAccountFrozen
			}
			if l.suspendedLocked(e.DebitAccount) || l.suspendedLocked(e.CreditAccount) {
				return nil, nil, ErrAccountSuspended
			}
			if err := l.overdraftRejectedLocked(e); err != nil {
				return nil, nil, err
			}
			entries = append(entries, e)
			infos = append(infos, PostingRuleLegInfo{
				Rule:          r.Name,
				EntryID:       e.ID,
				DebitAccount:  e.DebitAccount,
				CreditAccount: e.CreditAccount,
				AmountCents:   cents,
				Currency:      ctx.currency,
			})
		}
	}
	return entries, infos, nil
}

// countPostingRuleLegsLocked records the per-rule committed leg counts
// that feed ledger_posting_rule_legs_total{rule}. Callers must hold
// l.mu (the write lock); call it strictly after the atomic commit.
func (l *Ledger) countPostingRuleLegsLocked(infos []PostingRuleLegInfo) {
	if len(infos) == 0 {
		return
	}
	if l.postingRuleLegs == nil {
		l.postingRuleLegs = make(map[string]uint64)
	}
	for _, info := range infos {
		l.postingRuleLegs[info.Rule]++
	}
}

// PostingRuleLegCounts returns the per-rule committed derived-leg
// counts, for the /metrics exposition (see
// ledger_posting_rule_legs_total). The ledger is the source of truth:
// the HTTP layer mirrors these counts on scrape.
func (l *Ledger) PostingRuleLegCounts() map[string]uint64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make(map[string]uint64, len(l.postingRuleLegs))
	for k, v := range l.postingRuleLegs {
		out[k] = v
	}
	return out
}

// postingRuleLegInfosFromEntries rebuilds the receipt disclosure for
// idempotent replays: rule legs are the entries whose ID carries the
// "/rule:" marker. The rule name is the segment after "/rule:", up to
// the next '/'.
func postingRuleLegInfosFromEntries(entries []JournalEntry) []PostingRuleLegInfo {
	var infos []PostingRuleLegInfo
	for _, e := range entries {
		idx := strings.Index(e.ID, "/rule:")
		if idx < 0 {
			continue
		}
		name := e.ID[idx+len("/rule:"):]
		if j := strings.IndexByte(name, '/'); j >= 0 {
			name = name[:j]
		}
		infos = append(infos, PostingRuleLegInfo{
			Rule:          name,
			EntryID:       e.ID,
			DebitAccount:  e.DebitAccount,
			CreditAccount: e.CreditAccount,
			AmountCents:   e.AmountCents,
			Currency:      e.Currency,
		})
	}
	return infos
}

// postingRuleDetails renders the audit-log disclosure of booked rule
// legs: one row per leg, mirroring the fee-split audit details.
func postingRuleDetails(infos []PostingRuleLegInfo) []map[string]any {
	rows := make([]map[string]any, 0, len(infos))
	for _, info := range infos {
		rows = append(rows, map[string]any{
			"rule":           info.Rule,
			"entry_id":       info.EntryID,
			"debit_account":  string(info.DebitAccount),
			"credit_account": string(info.CreditAccount),
			"amount_cents":   info.AmountCents,
			"currency":       info.Currency,
		})
	}
	return rows
}

// postingRulesSortedLocked returns the installed rules sorted by name
// for deterministic report and snapshot output. Callers must hold l.mu
// (either lock suffices).
func (l *Ledger) postingRulesSortedLocked() []PostingRule {
	out := make([]PostingRule, len(l.postingRules))
	copy(out, l.postingRules)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
