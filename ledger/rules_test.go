package ledger

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

// validJournalRule returns a legal tax-withholding rule: 8% of any USD
// posting credited to "revenue" is withheld to "tax-payable".
func validJournalRule() JournalRule {
	return JournalRule{
		ID:                 "tax-withholding",
		Enabled:            true,
		Description:        "8% tax withholding on revenue postings",
		MatchCreditAccount: "revenue",
		MatchCurrency:      "USD",
		Legs: []RuleLeg{
			{DebitAccount: "revenue", CreditAccount: "tax-payable", RateBps: 800, Label: "withholding"},
		},
	}
}

func TestValidateJournalRule(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*JournalRule)
		wantErr error
	}{
		{"valid", func(r *JournalRule) {}, nil},
		{"empty id", func(r *JournalRule) { r.ID = "" }, ErrJournalRuleEmptyID},
		{"id too long", func(r *JournalRule) { r.ID = strings.Repeat("x", 65) }, ErrJournalRuleIDTooLong},
		{"id with separator", func(r *JournalRule) { r.ID = "a/b" }, ErrJournalRuleIDSeparator},
		{"description too long", func(r *JournalRule) { r.Description = strings.Repeat("x", 256) }, ErrJournalRuleDescriptionTooLong},
		{"no legs", func(r *JournalRule) { r.Legs = nil }, ErrJournalRuleNoLegs},
		{"leg empty debit", func(r *JournalRule) { r.Legs[0].DebitAccount = "" }, ErrJournalRuleLegAccounts},
		{"leg empty credit", func(r *JournalRule) { r.Legs[0].CreditAccount = "" }, ErrJournalRuleLegAccounts},
		{"leg same accounts", func(r *JournalRule) { r.Legs[0].CreditAccount = "revenue" }, ErrJournalRuleLegAccounts},
		{"leg no amount", func(r *JournalRule) { r.Legs[0].RateBps = 0 }, ErrJournalRuleLegAmount},
		{"leg both amounts", func(r *JournalRule) { r.Legs[0].FixedCents = 50 }, ErrJournalRuleLegAmount},
		{"leg negative fixed", func(r *JournalRule) {
			r.Legs[0].RateBps = 0
			r.Legs[0].FixedCents = -5
		}, ErrJournalRuleLegAmount},
		{"leg rate over 100%", func(r *JournalRule) { r.Legs[0].RateBps = 10001 }, ErrJournalRuleLegAmount},
		{"leg label too long", func(r *JournalRule) { r.Legs[0].Label = strings.Repeat("x", 65) }, ErrJournalRuleLabelTooLong},
		{"negative min", func(r *JournalRule) { r.MinAmountCents = -1 }, ErrJournalRuleMatchBounds},
		{"negative max", func(r *JournalRule) { r.MaxAmountCents = -1 }, ErrJournalRuleMatchBounds},
		{"min over max", func(r *JournalRule) { r.MinAmountCents, r.MaxAmountCents = 200, 100 }, ErrJournalRuleMatchBounds},
		{"bad currency", func(r *JournalRule) { r.MatchCurrency = "US" }, ErrInvalidCurrency},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := validJournalRule()
			tc.mutate(&r)
			err := ValidateJournalRule(r)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("ValidateJournalRule = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ValidateJournalRule = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestJournalRuleCRUD(t *testing.T) {
	l := New()

	if got := l.JournalRuleVersion(); got != 0 {
		t.Fatalf("JournalRuleVersion = %d, want 0", got)
	}
	if rules := l.JournalRules(); len(rules) != 0 {
		t.Fatalf("JournalRules = %d rules, want 0", len(rules))
	}

	r := validJournalRule()
	if err := l.SetJournalRule(r); err != nil {
		t.Fatalf("SetJournalRule: %v", err)
	}
	if got := l.JournalRuleVersion(); got != 1 {
		t.Fatalf("JournalRuleVersion = %d, want 1", got)
	}
	// Illegal rule: rejected, version untouched, nothing stored.
	bad := validJournalRule()
	bad.ID = "bad-rule"
	bad.Legs[0].RateBps = 10001
	if err := l.SetJournalRule(bad); !errors.Is(err, ErrJournalRuleLegAmount) {
		t.Fatalf("SetJournalRule illegal = %v, want ErrJournalRuleLegAmount", err)
	}
	if got := l.JournalRuleVersion(); got != 1 {
		t.Fatalf("JournalRuleVersion after rejected set = %d, want 1", got)
	}
	if _, ok := l.GetJournalRule("bad-rule"); ok {
		t.Fatal("illegal rule was stored")
	}

	// Stored rules are defensive copies: mutating the caller's slice
	// afterwards must not change the ledger's rule.
	r.Legs[0].RateBps = 1
	got, ok := l.GetJournalRule("tax-withholding")
	if !ok {
		t.Fatal("GetJournalRule: rule missing")
	}
	if got.Legs[0].RateBps != 800 {
		t.Fatalf("stored leg rate = %d, want 800 (defensive copy broken)", got.Legs[0].RateBps)
	}
	// Mutating a returned rule must not affect the stored one either.
	got.Legs[0].RateBps = 2
	again, _ := l.GetJournalRule("tax-withholding")
	if again.Legs[0].RateBps != 800 {
		t.Fatalf("stored leg rate after mutating returned copy = %d, want 800", again.Legs[0].RateBps)
	}

	// Replace: same ID overwrites, version still bumps.
	r2 := validJournalRule()
	r2.Description = "replaced"
	if err := l.SetJournalRule(r2); err != nil {
		t.Fatalf("SetJournalRule replace: %v", err)
	}
	if got := l.JournalRuleVersion(); got != 2 {
		t.Fatalf("JournalRuleVersion after replace = %d, want 2", got)
	}
	if stored, _ := l.GetJournalRule("tax-withholding"); stored.Description != "replaced" {
		t.Fatalf("replaced rule description = %q", stored.Description)
	}

	// A second rule sorts before the first in listings.
	other := validJournalRule()
	other.ID = "aaa-first"
	other.MatchCreditAccount = "other-rev"
	if err := l.SetJournalRule(other); err != nil {
		t.Fatalf("SetJournalRule second: %v", err)
	}
	rules := l.JournalRules()
	if len(rules) != 2 || rules[0].ID != "aaa-first" || rules[1].ID != "tax-withholding" {
		t.Fatalf("JournalRules order = %v, want [aaa-first tax-withholding]", rules)
	}

	if !l.DeleteJournalRule("aaa-first") {
		t.Fatal("DeleteJournalRule existing = false, want true")
	}
	if got := l.JournalRuleVersion(); got != 4 {
		t.Fatalf("JournalRuleVersion after delete = %d, want 4", got)
	}
	if l.DeleteJournalRule("aaa-first") {
		t.Fatal("DeleteJournalRule missing = true, want false")
	}
	if got := l.JournalRuleVersion(); got != 4 {
		t.Fatalf("JournalRuleVersion after no-op delete = %d, want 4", got)
	}
}

func TestJournalRuleAuditEvents(t *testing.T) {
	al := mustAuditLog(t)
	defer al.Close()
	l := New(WithAuditLog(al))

	if err := l.SetJournalRule(validJournalRule()); err != nil {
		t.Fatalf("SetJournalRule: %v", err)
	}
	if !l.DeleteJournalRule("tax-withholding") {
		t.Fatal("DeleteJournalRule: false")
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	now := time.Now()
	events, corrupt, err := ReadAuditLog(al.Dir(), now.UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt = %d, want 0", corrupt)
	}
	var set, del *AuditEvent
	for i := range events {
		switch events[i].Op {
		case "rule_set":
			set = &events[i]
		case "rule_delete":
			del = &events[i]
		}
	}
	if set == nil || set.TraceID != "tax-withholding" || set.Actor != "SetJournalRule" {
		t.Fatalf("rule_set event missing or wrong: %+v", set)
	}
	if set.Details["rule_version"] != float64(1) {
		t.Fatalf("rule_set rule_version = %v, want 1", set.Details["rule_version"])
	}
	if del == nil || del.TraceID != "tax-withholding" || del.Actor != "DeleteJournalRule" {
		t.Fatalf("rule_delete event missing or wrong: %+v", del)
	}
	if del.Details["rule_version"] != float64(2) {
		t.Fatalf("rule_delete rule_version = %v, want 2", del.Details["rule_version"])
	}
}

// postWithRule posts a 10000-cent USD principal (debit cash, credit
// revenue) on a ledger carrying the tax-withholding rule, returning the
// principal entry.
func postWithRule(t *testing.T, l *Ledger, id string) JournalEntry {
	t.Helper()
	e := JournalEntry{
		ID:            id,
		DebitAccount:  "cash",
		CreditAccount: "revenue",
		AmountCents:   10000,
		Currency:      "USD",
		CreatedAt:     time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC),
	}
	posted, dup, err := l.Post(e)
	if err != nil || dup {
		t.Fatalf("Post = dup=%v err=%v", dup, err)
	}
	return posted
}

func TestPostFiresMatchingRule(t *testing.T) {
	l := New()
	if err := l.SetJournalRule(validJournalRule()); err != nil {
		t.Fatalf("SetJournalRule: %v", err)
	}
	posted := postWithRule(t, l, "e-rule-1")

	// 8% of 10000 = 800 withheld: revenue nets -9200 (credited 10000,
	// debited 800 back), tax-payable owes 800.
	if got := l.BalanceIn("cash", "USD"); got != 10000 {
		t.Fatalf("cash = %d, want 10000", got)
	}
	if got := l.BalanceIn("revenue", "USD"); got != -9200 {
		t.Fatalf("revenue = %d, want -9200", got)
	}
	if got := l.BalanceIn("tax-payable", "USD"); got != -800 {
		t.Fatalf("tax-payable = %d, want -800", got)
	}
	// Principal + one derived leg: two chain links, version 2.
	if v := l.Version(); v != 2 {
		t.Fatalf("version = %d, want 2", v)
	}
	if _, n := l.ChainHead(); n != 2 {
		t.Fatalf("chain links = %d, want 2", n)
	}
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	if got := l.RuleLegsCommitted(); got != 1 {
		t.Fatalf("RuleLegsCommitted = %d, want 1", got)
	}

	// The derived leg is a first-class journal row with a deterministic
	// ID and a rule memo.
	leg, ok := l.entries["e-rule-1/rule/tax-withholding/0"]
	if !ok {
		t.Fatal("derived leg e-rule-1/rule/tax-withholding/0 missing from journal")
	}
	if leg.DebitAccount != "revenue" || leg.CreditAccount != "tax-payable" ||
		leg.AmountCents != 800 || leg.Currency != "USD" {
		t.Fatalf("derived leg = %+v", leg)
	}
	if leg.Memo != "rule:tax-withholding:withholding" {
		t.Fatalf("derived leg memo = %q", leg.Memo)
	}
	if !leg.CreatedAt.Equal(posted.CreatedAt) {
		t.Fatalf("derived leg timestamp %v != principal %v", leg.CreatedAt, posted.CreatedAt)
	}
	// The principal itself is untouched by the rule.
	if posted.ID != "e-rule-1" || posted.AmountCents != 10000 {
		t.Fatalf("principal = %+v", posted)
	}
}

func TestPostRuleScoping(t *testing.T) {
	newLedgerWithRule := func(t *testing.T, mutate func(*JournalRule)) *Ledger {
		t.Helper()
		l := New()
		r := validJournalRule()
		mutate(&r)
		if err := l.SetJournalRule(r); err != nil {
			t.Fatalf("SetJournalRule: %v", err)
		}
		return l
	}
	post := func(t *testing.T, l *Ledger, mutate func(*JournalEntry)) {
		t.Helper()
		e := JournalEntry{
			ID:            "e-scope",
			DebitAccount:  "cash",
			CreditAccount: "revenue",
			AmountCents:   10000,
			Currency:      "USD",
			CreatedAt:     time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC),
		}
		mutate(&e)
		if _, _, err := l.Post(e); err != nil {
			t.Fatalf("Post: %v", err)
		}
	}
	assertNoLegs := func(t *testing.T, l *Ledger) {
		t.Helper()
		if v := l.Version(); v != 1 {
			t.Fatalf("version = %d, want 1 (no derived legs)", v)
		}
		if got := l.RuleLegsCommitted(); got != 0 {
			t.Fatalf("RuleLegsCommitted = %d, want 0", got)
		}
	}

	t.Run("disabled rule does not fire", func(t *testing.T) {
		l := newLedgerWithRule(t, func(r *JournalRule) { r.Enabled = false })
		post(t, l, func(e *JournalEntry) {})
		assertNoLegs(t, l)
	})
	t.Run("non-matching debit account", func(t *testing.T) {
		l := newLedgerWithRule(t, func(r *JournalRule) { r.MatchDebitAccount = "bank" })
		post(t, l, func(e *JournalEntry) {})
		assertNoLegs(t, l)
	})
	t.Run("non-matching credit account", func(t *testing.T) {
		l := newLedgerWithRule(t, func(r *JournalRule) { r.MatchCreditAccount = "other" })
		post(t, l, func(e *JournalEntry) {})
		assertNoLegs(t, l)
	})
	t.Run("non-matching currency", func(t *testing.T) {
		l := newLedgerWithRule(t, func(r *JournalRule) { r.MatchCurrency = "EUR" })
		post(t, l, func(e *JournalEntry) {})
		assertNoLegs(t, l)
	})
	t.Run("below min amount", func(t *testing.T) {
		l := newLedgerWithRule(t, func(r *JournalRule) { r.MinAmountCents = 20000 })
		post(t, l, func(e *JournalEntry) {})
		assertNoLegs(t, l)
	})
	t.Run("above max amount", func(t *testing.T) {
		l := newLedgerWithRule(t, func(r *JournalRule) { r.MaxAmountCents = 5000 })
		post(t, l, func(e *JournalEntry) {})
		assertNoLegs(t, l)
	})
	t.Run("within amount range fires", func(t *testing.T) {
		l := newLedgerWithRule(t, func(r *JournalRule) {
			r.MinAmountCents = 5000
			r.MaxAmountCents = 20000
		})
		post(t, l, func(e *JournalEntry) {})
		if v := l.Version(); v != 2 {
			t.Fatalf("version = %d, want 2", v)
		}
	})
	t.Run("no rules configured posts normally", func(t *testing.T) {
		l := New()
		post(t, l, func(e *JournalEntry) {})
		assertNoLegs(t, l)
	})
}

func TestPostRuleFixedCentsLeg(t *testing.T) {
	l := New()
	// Platform service fee: a flat 30 cents on every USD posting out of
	// the merchant account.
	err := l.SetJournalRule(JournalRule{
		ID:                 "platform-fee",
		Enabled:            true,
		MatchCreditAccount: "merchant",
		MatchCurrency:      "USD",
		Legs: []RuleLeg{
			{DebitAccount: "platform-revenue", CreditAccount: "merchant", FixedCents: 30, Label: "svc-fee"},
		},
	})
	if err != nil {
		t.Fatalf("SetJournalRule: %v", err)
	}
	e := JournalEntry{
		ID: "e-fee", DebitAccount: "settlement", CreditAccount: "merchant",
		AmountCents: 5000, Currency: "USD",
		CreatedAt: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC),
	}
	if _, _, err := l.Post(e); err != nil {
		t.Fatalf("Post: %v", err)
	}
	leg, ok := l.entries["e-fee/rule/platform-fee/0"]
	if !ok {
		t.Fatal("derived fee leg missing")
	}
	if leg.AmountCents != 30 || leg.Memo != "rule:platform-fee:svc-fee" {
		t.Fatalf("fee leg = %+v", leg)
	}
	// Merchant outflow: 5000 principal + 30 fee.
	if got := l.BalanceIn("merchant", "USD"); got != -5030 {
		t.Fatalf("merchant = %d, want -5030", got)
	}
	if got := l.BalanceIn("platform-revenue", "USD"); got != 30 {
		t.Fatalf("platform-revenue = %d, want 30", got)
	}
}

func TestPostRuleMultiLegRule(t *testing.T) {
	l := New()
	err := l.SetJournalRule(JournalRule{
		ID:                 "split",
		Enabled:            true,
		MatchCreditAccount: "revenue",
		Legs: []RuleLeg{
			{DebitAccount: "revenue", CreditAccount: "tax-payable", RateBps: 800},
			{DebitAccount: "revenue", CreditAccount: "reserve", FixedCents: 100},
		},
	})
	if err != nil {
		t.Fatalf("SetJournalRule: %v", err)
	}
	postWithRule(t, l, "e-multi")
	if v := l.Version(); v != 3 {
		t.Fatalf("version = %d, want 3 (principal + 2 legs)", v)
	}
	for i, wantID := range []string{"e-multi/rule/split/0", "e-multi/rule/split/1"} {
		if _, ok := l.entries[wantID]; !ok {
			t.Fatalf("derived leg %d (%s) missing", i, wantID)
		}
	}
	if got := l.BalanceIn("revenue", "USD"); got != -9100 {
		t.Fatalf("revenue = %d, want -9100", got)
	}
}

func TestPostRuleZeroComputedLegSkipped(t *testing.T) {
	l := New()
	// 1 bps on 50 cents computes to 0: the leg books nothing, but the
	// principal still commits — like the fee policy's dust tiers.
	err := l.SetJournalRule(JournalRule{
		ID:                 "dust",
		Enabled:            true,
		MatchCreditAccount: "revenue",
		MinAmountCents:     1,
		MaxAmountCents:     100,
		Legs: []RuleLeg{
			{DebitAccount: "revenue", CreditAccount: "tax-payable", RateBps: 1},
		},
	})
	if err != nil {
		t.Fatalf("SetJournalRule: %v", err)
	}
	e := JournalEntry{
		ID: "e-dust", DebitAccount: "cash", CreditAccount: "revenue",
		AmountCents: 50, Currency: "USD",
		CreatedAt: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC),
	}
	if _, _, err := l.Post(e); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if v := l.Version(); v != 1 {
		t.Fatalf("version = %d, want 1 (zero leg skipped)", v)
	}
	if got := l.BalanceIn("revenue", "USD"); got != -50 {
		t.Fatalf("revenue = %d, want -50", got)
	}
}

func TestPostRuleDerivedLegFrozenRejects(t *testing.T) {
	l := New()
	if err := l.SetJournalRule(validJournalRule()); err != nil {
		t.Fatalf("SetJournalRule: %v", err)
	}
	// The principal's accounts are fine; the derived leg touches the
	// frozen tax-payable account.
	l.Freeze("tax-payable")

	e := JournalEntry{
		ID: "e-frozen-leg", DebitAccount: "cash", CreditAccount: "revenue",
		AmountCents: 10000, Currency: "USD",
		CreatedAt: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC),
	}
	if _, _, err := l.Post(e); !errors.Is(err, ErrAccountFrozen) {
		t.Fatalf("Post = %v, want ErrAccountFrozen", err)
	}
	// Atomic: nothing recorded — no journal row, no chain link, no
	// version bump.
	if v := l.Version(); v != 0 {
		t.Fatalf("version = %d, want 0", v)
	}
	if len(l.entries) != 0 || len(l.chain) != 0 {
		t.Fatalf("entries=%d chain=%d, want 0/0", len(l.entries), len(l.chain))
	}
	if got := l.RuleLegsCommitted(); got != 0 {
		t.Fatalf("RuleLegsCommitted = %d, want 0", got)
	}
}

func TestPostRuleOverdraftAggregateRejects(t *testing.T) {
	l := New(WithOverdraftProtection("payer"))
	// Fund the payer with exactly the principal amount.
	if _, _, err := l.Post(JournalEntry{
		ID: "e-fund", DebitAccount: "payer", CreditAccount: "bank",
		AmountCents: 10000, Currency: "USD",
		CreatedAt: time.Date(2026, 10, 6, 11, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("fund Post: %v", err)
	}
	// Rule: every posting out of the payer withholds a 1000-cent flat
	// charge to "collector". The principal alone (10000) fits the 10000
	// balance; with the derived leg the aggregate outflow is 11000.
	err := l.SetJournalRule(JournalRule{
		ID:                 "surcharge",
		Enabled:            true,
		MatchCreditAccount: "payer",
		Legs: []RuleLeg{
			{DebitAccount: "collector", CreditAccount: "payer", FixedCents: 1000},
		},
	})
	if err != nil {
		t.Fatalf("SetJournalRule: %v", err)
	}
	e := JournalEntry{
		ID: "e-od", DebitAccount: "vendor", CreditAccount: "payer",
		AmountCents: 10000, Currency: "USD",
		CreatedAt: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC),
	}
	if _, _, err := l.Post(e); !errors.Is(err, ErrAccountOverdraft) {
		t.Fatalf("Post = %v, want ErrAccountOverdraft", err)
	}
	if v := l.Version(); v != 1 {
		t.Fatalf("version = %d, want 1 (only the funding post)", v)
	}
}

func TestPostRuleDailyLimitAggregateRejects(t *testing.T) {
	l := New()
	// Payer's daily USD outflow limit is 10000; the principal alone (9000)
	// fits, the derived 2000-cent leg pushes the aggregate to 11000.
	if err := l.SetDailyLimit("payer", "USD", 10000); err != nil {
		t.Fatalf("SetDailyLimit: %v", err)
	}
	err := l.SetJournalRule(JournalRule{
		ID:                 "surcharge",
		Enabled:            true,
		MatchCreditAccount: "payer",
		Legs: []RuleLeg{
			{DebitAccount: "collector", CreditAccount: "payer", FixedCents: 2000},
		},
	})
	if err != nil {
		t.Fatalf("SetJournalRule: %v", err)
	}
	e := JournalEntry{
		ID: "e-dl", DebitAccount: "vendor", CreditAccount: "payer",
		AmountCents: 9000, Currency: "USD",
		CreatedAt: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC),
	}
	if _, _, err := l.Post(e); !errors.Is(err, ErrDailyLimitExceeded) {
		t.Fatalf("Post = %v, want ErrDailyLimitExceeded", err)
	}
	if v := l.Version(); v != 0 {
		t.Fatalf("version = %d, want 0", v)
	}
}

func TestPostRuleIdempotencyReplay(t *testing.T) {
	l := New()
	if err := l.SetJournalRule(validJournalRule()); err != nil {
		t.Fatalf("SetJournalRule: %v", err)
	}
	e := JournalEntry{
		ID: "e-replay", DebitAccount: "cash", CreditAccount: "revenue",
		AmountCents: 10000, Currency: "USD", IdempotencyKey: "rule-key-1",
		CreatedAt: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC),
	}
	if _, dup, err := l.Post(e); err != nil || dup {
		t.Fatalf("first Post = dup=%v err=%v", dup, err)
	}
	if v := l.Version(); v != 2 {
		t.Fatalf("version after first post = %d, want 2", v)
	}
	// Replay: returns the original principal, books nothing — the rule
	// does not fire again.
	retry := e
	retry.ID = "e-replay-2"
	posted, dup, err := l.Post(retry)
	if err != nil || !dup {
		t.Fatalf("replay Post = dup=%v err=%v, want dup=true", dup, err)
	}
	if posted.ID != "e-replay" {
		t.Fatalf("replay returned %q, want the original entry", posted.ID)
	}
	if v := l.Version(); v != 2 {
		t.Fatalf("version after replay = %d, want 2", v)
	}
	if got := l.RuleLegsCommitted(); got != 1 {
		t.Fatalf("RuleLegsCommitted after replay = %d, want 1", got)
	}
}

func TestPostRuleValidationOrder(t *testing.T) {
	l := New()
	if err := l.SetJournalRule(validJournalRule()); err != nil {
		t.Fatalf("SetJournalRule: %v", err)
	}
	// A principal that fails field validation never reaches rule
	// evaluation: the rule cannot rescue or alter the rejection.
	e := JournalEntry{
		ID: "e-bad", DebitAccount: "cash", CreditAccount: "revenue",
		AmountCents: 0, Currency: "USD",
		CreatedAt: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC),
	}
	if _, _, err := l.Post(e); !errors.Is(err, ErrNonPositiveAmount) {
		t.Fatalf("Post = %v, want ErrNonPositiveAmount", err)
	}
	// A frozen principal fails before rules are evaluated.
	l.Freeze("cash")
	e2 := JournalEntry{
		ID: "e-frozen", DebitAccount: "cash", CreditAccount: "revenue",
		AmountCents: 10000, Currency: "USD",
		CreatedAt: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC),
	}
	if _, _, err := l.Post(e2); !errors.Is(err, ErrAccountFrozen) {
		t.Fatalf("Post = %v, want ErrAccountFrozen", err)
	}
	if v := l.Version(); v != 0 {
		t.Fatalf("version = %d, want 0", v)
	}
}

func TestJournalRuleSnapshotRoundTrip(t *testing.T) {
	src := New()
	if err := src.SetJournalRule(validJournalRule()); err != nil {
		t.Fatalf("SetJournalRule: %v", err)
	}
	fee := validJournalRule()
	fee.ID = "platform-fee"
	fee.MatchCreditAccount = "merchant"
	fee.Legs = []RuleLeg{{DebitAccount: "platform-revenue", CreditAccount: "merchant", FixedCents: 30}}
	if err := src.SetJournalRule(fee); err != nil {
		t.Fatalf("SetJournalRule fee: %v", err)
	}
	if got := src.JournalRuleVersion(); got != 2 {
		t.Fatalf("JournalRuleVersion = %d, want 2", got)
	}
	postWithRule(t, src, "e-snap-rule")

	var buf bytes.Buffer
	if err := src.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if !snapshotLedgersEqual(src, restored) {
		t.Fatal("restored ledger state differs from source")
	}
	if got := restored.JournalRuleVersion(); got != 2 {
		t.Fatalf("restored JournalRuleVersion = %d, want 2", got)
	}
	rules := restored.JournalRules()
	if len(rules) != 2 || rules[0].ID != "platform-fee" || rules[1].ID != "tax-withholding" {
		t.Fatalf("restored rules = %v", rules)
	}
	if rules[1].Legs[0].RateBps != 800 || rules[1].MatchCurrency != "USD" {
		t.Fatalf("restored rule = %+v", rules[1])
	}
	// The restored ledger still fires its rules on new postings.
	postWithRule(t, restored, "e-after-restore")
	if got := restored.BalanceIn("tax-payable", "USD"); got != -1600 {
		t.Fatalf("tax-payable after restore = %d, want -1600", got)
	}
	if err := restored.VerifyChain(); err != nil {
		t.Fatalf("restored VerifyChain: %v", err)
	}
}

func TestSnapshotImportRejectsIllegalRule(t *testing.T) {
	l := New()
	bad := validJournalRule()
	bad.Legs[0].RateBps = 10001 // illegal: over 100% of principal
	cfg := snapshotConfigLine{JournalRules: []JournalRule{bad}}
	if err := l.applySnapshotConfigLocked(&cfg); err == nil {
		t.Fatal("applySnapshotConfigLocked accepted an illegal rule")
	}
	if len(l.journalRules) != 0 || l.journalRuleVersion != 0 {
		t.Fatal("failed import mutated the ledger")
	}
}

func TestPostRuleDerivedLegsDoNotRefire(t *testing.T) {
	l := New()
	// A rule matching the derived leg's own accounts must not cascade:
	// rules evaluate the principal only.
	err := l.SetJournalRule(JournalRule{
		ID:                 "cascade",
		Enabled:            true,
		MatchCreditAccount: "tax-payable",
		Legs: []RuleLeg{
			{DebitAccount: "x", CreditAccount: "y", FixedCents: 1},
		},
	})
	if err != nil {
		t.Fatalf("SetJournalRule: %v", err)
	}
	if err := l.SetJournalRule(validJournalRule()); err != nil {
		t.Fatalf("SetJournalRule tax: %v", err)
	}
	postWithRule(t, l, "e-nocascade")
	// Principal + tax leg only: the cascade rule's match on the derived
	// leg's credit account must not fire.
	if v := l.Version(); v != 2 {
		t.Fatalf("version = %d, want 2 (no cascading)", v)
	}
	if _, ok := l.entries["e-nocascade/rule/cascade/0"]; ok {
		t.Fatal("rule fired on its own derived leg")
	}
}
