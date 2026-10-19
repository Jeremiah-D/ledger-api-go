package ledger

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// channelFeeRule is the canonical test rule: a 2.5% channel fee on
// merchant settlement transfers, debited to the channel-fee revenue
// account and credited by the payer.
func channelFeeRule() PostingRule {
	return PostingRule{
		Name:           "channel-fee",
		EventKind:      PostingEventTransfer,
		AccountPrefix:  "merchant-",
		MinAmountCents: 1000,
		Legs: []PostingRuleLeg{
			{AccountTemplate: "channel-fee-revenue", Side: "debit", AmountExpr: "bps:250"},
		},
	}
}

func mustValidatePostingRule(t *testing.T, r PostingRule) PostingRule {
	t.Helper()
	nr, err := ValidatePostingRule(r)
	if err != nil {
		t.Fatalf("ValidatePostingRule(%q): %v", r.Name, err)
	}
	return nr
}

func TestValidatePostingRule(t *testing.T) {
	base := channelFeeRule()
	cases := []struct {
		name   string
		mutate func(*PostingRule)
		err    error
	}{
		{"empty name", func(r *PostingRule) { r.Name = "" }, ErrPostingRuleEmptyName},
		{"name too long", func(r *PostingRule) { r.Name = strings.Repeat("x", 65) }, ErrPostingRuleNameTooLong},
		{"name with slash", func(r *PostingRule) { r.Name = "a/b" }, ErrPostingRuleNameSeparator},
		{"bad kind", func(r *PostingRule) { r.EventKind = "post" }, ErrPostingRuleBadKind},
		{"no legs", func(r *PostingRule) { r.Legs = nil }, ErrPostingRuleNoLegs},
		{"empty template", func(r *PostingRule) { r.Legs[0].AccountTemplate = "" }, ErrPostingRuleEmptyTemplate},
		{"template too long", func(r *PostingRule) { r.Legs[0].AccountTemplate = strings.Repeat("x", 129) }, ErrPostingRuleTemplateTooLong},
		{"bad placeholder", func(r *PostingRule) { r.Legs[0].AccountTemplate = "{merchant}" }, ErrPostingRuleBadPlaceholder},
		{"unclosed brace", func(r *PostingRule) { r.Legs[0].AccountTemplate = "fee-{payer" }, ErrPostingRuleBadPlaceholder},
		{"bad side", func(r *PostingRule) { r.Legs[0].Side = "middle" }, ErrPostingRuleBadSide},
		{"bad expr", func(r *PostingRule) { r.Legs[0].AmountExpr = "percent:25" }, ErrPostingRuleBadAmountExpr},
		{"bps zero", func(r *PostingRule) { r.Legs[0].AmountExpr = "bps:0" }, ErrPostingRuleBadAmountExpr},
		{"bps over 10000", func(r *PostingRule) { r.Legs[0].AmountExpr = "bps:10001" }, ErrPostingRuleBadAmountExpr},
		{"fixed zero", func(r *PostingRule) { r.Legs[0].AmountExpr = "fixed:0" }, ErrPostingRuleBadAmountExpr},
		{"negative min", func(r *PostingRule) { r.MinAmountCents = -1 }, ErrPostingRuleMatchBounds},
		{"bad currency", func(r *PostingRule) { r.Currency = "usd" }, ErrInvalidCurrency},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Deep-copy: the table mutations must not leak across
			// subtests through the shared Legs slice.
			r := base
			r.Legs = append([]PostingRuleLeg(nil), base.Legs...)
			tc.mutate(&r)
			if _, err := ValidatePostingRule(r); !errors.Is(err, tc.err) {
				t.Fatalf("ValidatePostingRule: got %v, want %v", err, tc.err)
			}
		})
	}
}

func TestValidatePostingRuleOK(t *testing.T) {
	r := mustValidatePostingRule(t, channelFeeRule())
	if r.Name != "channel-fee" {
		t.Fatalf("name = %q", r.Name)
	}
	// Lowercase currency normalizes to the canonical code.
	r2 := channelFeeRule()
	r2.Currency = "USD"
	nr, err := ValidatePostingRule(r2)
	if err != nil {
		t.Fatalf("ValidatePostingRule: %v", err)
	}
	if nr.Currency != "USD" {
		t.Fatalf("currency = %q, want USD", nr.Currency)
	}
}

func TestWithPostingRulesFailFast(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("WithPostingRules with duplicate names: expected panic")
		}
	}()
	New(WithPostingRules([]PostingRule{channelFeeRule(), channelFeeRule()}))
}

func TestWithPostingRulesFailFastInvalid(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("WithPostingRules with invalid rule: expected panic")
		}
	}()
	bad := channelFeeRule()
	bad.EventKind = "nope"
	New(WithPostingRules([]PostingRule{bad}))
}

func TestPostingRuleTransferFires(t *testing.T) {
	l := New(WithPostingRules([]PostingRule{channelFeeRule()}))
	seedBalance(t, l, "merchant-alice", 100000)

	receipt, err := l.PostTransfer(Transfer{
		ID:          "settle-1",
		From:        "merchant-alice",
		To:          "platform",
		AmountCents: 20000,
	})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if len(receipt.RuleLegs) != 1 {
		t.Fatalf("RuleLegs = %d, want 1", len(receipt.RuleLegs))
	}
	info := receipt.RuleLegs[0]
	if info.Rule != "channel-fee" {
		t.Errorf("rule = %q, want channel-fee", info.Rule)
	}
	if info.EntryID != "settle-1/rule:channel-fee" {
		t.Errorf("entry ID = %q, want settle-1/rule:channel-fee", info.EntryID)
	}
	// floor(20000 * 250 / 10000) = 500.
	if info.AmountCents != 500 {
		t.Errorf("amount = %d, want 500", info.AmountCents)
	}
	if info.DebitAccount != "channel-fee-revenue" || info.CreditAccount != "merchant-alice" {
		t.Errorf("legs = %s/%s, want channel-fee-revenue/merchant-alice",
			info.DebitAccount, info.CreditAccount)
	}
	if info.Currency != DefaultCurrency {
		t.Errorf("currency = %q, want %q", info.Currency, DefaultCurrency)
	}
	// The rule leg is a real journal entry: BatchID and memo tag it.
	e, ok := l.entries["settle-1/rule:channel-fee"]
	if !ok {
		t.Fatal("rule leg not journaled")
	}
	if e.BatchID != "settle-1" {
		t.Errorf("batch = %q, want settle-1", e.BatchID)
	}
	if e.Memo != "rule:channel-fee" {
		t.Errorf("memo = %q, want rule:channel-fee", e.Memo)
	}
	if e.AmountCents != 500 {
		t.Errorf("journal amount = %d, want 500", e.AmountCents)
	}
	// Balances: alice paid 20000 + 500, platform got 20000.
	if got := l.Balance("merchant-alice"); got != 100000-20500 {
		t.Errorf("alice = %d, want %d", got, 100000-20500)
	}
	if got := l.Balance("channel-fee-revenue"); got != 500 {
		t.Errorf("fee revenue = %d, want 500", got)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Errorf("VerifyAccountingEquation: %v", err)
	}
	if err := l.VerifyChain(); err != nil {
		t.Errorf("VerifyChain: %v", err)
	}
	// Metrics: the per-rule counter moved.
	if got := l.PostingRuleLegCounts()["channel-fee"]; got != 1 {
		t.Errorf("PostingRuleLegCounts = %d, want 1", got)
	}
}

func TestPostingRuleTransferNoMatch(t *testing.T) {
	l := New(WithPostingRules([]PostingRule{channelFeeRule()}))
	seedBalance(t, l, "merchant-alice", 100000)
	seedBalance(t, l, "retail-bob", 100000)

	cases := []struct {
		name string
		tx   Transfer
	}{
		{"below min amount", Transfer{ID: "t1", From: "merchant-alice", To: "platform", AmountCents: 999}},
		{"prefix mismatch", Transfer{ID: "t2", From: "retail-bob", To: "platform", AmountCents: 20000}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			receipt, err := l.PostTransfer(tc.tx)
			if err != nil {
				t.Fatalf("PostTransfer: %v", err)
			}
			if len(receipt.RuleLegs) != 0 {
				t.Fatalf("RuleLegs = %v, want none", receipt.RuleLegs)
			}
			if got := l.PostingRuleLegCounts()["channel-fee"]; got != 0 {
				t.Fatalf("counter moved: %d", got)
			}
		})
	}
}

func TestPostingRuleCurrencyMatch(t *testing.T) {
	rule := channelFeeRule()
	rule.Name = "eur-fee"
	rule.Currency = "EUR"
	rule.AccountPrefix = ""
	rule.MinAmountCents = 0
	l := New(WithPostingRules([]PostingRule{rule}))
	seedBalance(t, l, "merchant-alice", 100000)

	// USD transfer: no fire.
	r1, err := l.PostTransfer(Transfer{ID: "u1", From: "merchant-alice", To: "platform", AmountCents: 20000, Currency: "USD"})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if len(r1.RuleLegs) != 0 {
		t.Fatalf("USD fired EUR rule: %v", r1.RuleLegs)
	}
	// EUR transfer: fires.
	r2, err := l.PostTransfer(Transfer{ID: "e1", From: "merchant-alice", To: "platform", AmountCents: 20000, Currency: "EUR"})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if len(r2.RuleLegs) != 1 || r2.RuleLegs[0].Currency != "EUR" {
		t.Fatalf("EUR rule legs = %v", r2.RuleLegs)
	}
}

func TestPostingRuleMultiLegAndPlaceholders(t *testing.T) {
	rule := PostingRule{
		Name:      "tax-split",
		EventKind: PostingEventTransfer,
		Legs: []PostingRuleLeg{
			{AccountTemplate: "tax-payable", Side: "credit", AmountExpr: "fixed:100"},
			{AccountTemplate: "{payee}-withholding", Side: "debit", AmountExpr: "bps:100"},
		},
	}
	l := New(WithPostingRules([]PostingRule{rule}))
	seedBalance(t, l, "merchant-alice", 100000)

	receipt, err := l.PostTransfer(Transfer{
		ID:          "payout-1",
		From:        "merchant-alice",
		To:          "supplier",
		AmountCents: 10000,
	})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if len(receipt.RuleLegs) != 2 {
		t.Fatalf("RuleLegs = %d, want 2", len(receipt.RuleLegs))
	}
	// Leg 0: credit side — debit payer, credit tax-payable, fixed 100.
	l0 := receipt.RuleLegs[0]
	if l0.EntryID != "payout-1/rule:tax-split/0" {
		t.Errorf("leg0 ID = %q", l0.EntryID)
	}
	if l0.DebitAccount != "merchant-alice" || l0.CreditAccount != "tax-payable" || l0.AmountCents != 100 {
		t.Errorf("leg0 = %+v", l0)
	}
	// Leg 1: {payee} placeholder resolves, debit side, floor(10000*100/10000)=100.
	l1 := receipt.RuleLegs[1]
	if l1.EntryID != "payout-1/rule:tax-split/1" {
		t.Errorf("leg1 ID = %q", l1.EntryID)
	}
	if l1.DebitAccount != "supplier-withholding" || l1.CreditAccount != "merchant-alice" || l1.AmountCents != 100 {
		t.Errorf("leg1 = %+v", l1)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Errorf("VerifyAccountingEquation: %v", err)
	}
}

func TestPostingRuleDustSkips(t *testing.T) {
	// 1 bps on 500 cents = floor(500*1/10000) = 0: dust books nothing.
	rule := PostingRule{
		Name:      "dust",
		EventKind: PostingEventTransfer,
		Legs: []PostingRuleLeg{
			{AccountTemplate: "dust-revenue", Side: "debit", AmountExpr: "bps:1"},
		},
	}
	l := New(WithPostingRules([]PostingRule{rule}))
	seedBalance(t, l, "alice", 100000)
	receipt, err := l.PostTransfer(Transfer{ID: "d1", From: "alice", To: "bob", AmountCents: 500})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if len(receipt.RuleLegs) != 0 {
		t.Fatalf("dust booked: %v", receipt.RuleLegs)
	}
	if got := l.PostingRuleLegCounts()["dust"]; got != 0 {
		t.Fatalf("dust counter = %d", got)
	}
}

func TestPostingRuleTransferFrozenLegRejects(t *testing.T) {
	l := New(WithPostingRules([]PostingRule{channelFeeRule()}))
	seedBalance(t, l, "merchant-alice", 100000)
	l.Freeze("channel-fee-revenue")
	_, versionBefore := l.Snapshot("merchant-alice")
	entriesBefore := len(l.Entries())
	_, err := l.PostTransfer(Transfer{
		ID:          "settle-frozen",
		From:        "merchant-alice",
		To:          "platform",
		AmountCents: 20000,
	})
	if !errors.Is(err, ErrAccountFrozen) {
		t.Fatalf("PostTransfer: got %v, want ErrAccountFrozen", err)
	}
	// The whole transfer rejected: the principal recorded nothing.
	assertLedgerUntouched(t, l, versionBefore, entriesBefore)
	if _, exists := l.entries["settle-frozen"]; exists {
		t.Fatal("principal entry survived the rejection")
	}
}

func TestPostingRuleTransferIdempotentReplay(t *testing.T) {
	l := New(WithPostingRules([]PostingRule{channelFeeRule()}))
	seedBalance(t, l, "merchant-alice", 100000)
	r1, err := l.PostTransfer(Transfer{
		ID: "settle-replay", From: "merchant-alice", To: "platform",
		AmountCents: 20000, IdempotencyKey: "rule-key-1",
	})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if len(r1.RuleLegs) != 1 {
		t.Fatalf("first post RuleLegs = %d", len(r1.RuleLegs))
	}
	r2, err := l.PostTransfer(Transfer{
		ID: "settle-replay-2", From: "merchant-alice", To: "platform",
		AmountCents: 20000, IdempotencyKey: "rule-key-1",
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !r2.Duplicate {
		t.Error("replay not marked duplicate")
	}
	if len(r2.RuleLegs) != 1 || r2.RuleLegs[0].Rule != "channel-fee" {
		t.Fatalf("replay RuleLegs = %v", r2.RuleLegs)
	}
	// The replay booked nothing new: the per-rule counter moved once.
	if got := l.PostingRuleLegCounts()["channel-fee"]; got != 1 {
		t.Fatalf("counter = %d, want 1", got)
	}
	if len(r2.Entries) != 2 {
		t.Fatalf("replay entries = %d, want 2 (principal + rule leg)", len(r2.Entries))
	}
}

func TestPostingRuleCaptureFires(t *testing.T) {
	rule := PostingRule{
		Name:      "payout-tax",
		EventKind: PostingEventCapture,
		Legs: []PostingRuleLeg{
			{AccountTemplate: "tax-payable", Side: "debit", AmountExpr: "bps:800"},
		},
	}
	l := New(WithPostingRules([]PostingRule{rule}))
	seedBalance(t, l, "payroll", 100000)
	h, _, err := l.Hold(Hold{
		ID: "hold-1", Account: "payroll", AmountCents: 10000,
		Currency: DefaultCurrency, ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	_ = h
	receipt, err := l.Capture(Capture{
		ID: "cap-1", HoldID: "hold-1", To: "employee", AmountCents: 10000,
		IdempotencyKey: "cap-key-1",
	})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if len(receipt.RuleLegs) != 1 {
		t.Fatalf("RuleLegs = %d, want 1", len(receipt.RuleLegs))
	}
	info := receipt.RuleLegs[0]
	if info.EntryID != "cap-1/rule:payout-tax" {
		t.Errorf("entry ID = %q", info.EntryID)
	}
	// floor(10000 * 800 / 10000) = 800, debit side: debit tax-payable, credit payroll.
	if info.DebitAccount != "tax-payable" || info.CreditAccount != "payroll" || info.AmountCents != 800 {
		t.Errorf("leg = %+v", info)
	}
	if got := l.Balance("tax-payable"); got != 800 {
		t.Errorf("tax-payable = %d, want 800", got)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Errorf("VerifyAccountingEquation: %v", err)
	}
	// Idempotent replay of the capture returns the same disclosure.
	r3, err := l.Capture(Capture{
		ID: "cap-1c", HoldID: "hold-1", To: "employee", AmountCents: 10000,
		IdempotencyKey: "cap-key-1",
	})
	if err != nil {
		t.Fatalf("capture replay: %v", err)
	}
	if !r3.Duplicate || len(r3.RuleLegs) != 1 {
		t.Fatalf("replay = %+v", r3)
	}
}

func TestPostingRuleSweepFiresPerLeg(t *testing.T) {
	rule := PostingRule{
		Name:      "sweep-reserve",
		EventKind: PostingEventSweep,
		Legs: []PostingRuleLeg{
			{AccountTemplate: "sweep-reserve", Side: "debit", AmountExpr: "bps:1000"},
		},
	}
	l := New(WithPostingRules([]PostingRule{rule}))
	seedBalance(t, l, "sub-a", 10000)
	seedBalance(t, l, "sub-b", 20000)

	receipt, err := l.PostSweep(Sweep{ID: "sweep-1", From: []AccountID{"sub-a", "sub-b"}, To: "master"})
	if err != nil {
		t.Fatalf("PostSweep: %v", err)
	}
	if len(receipt.Legs) != 2 {
		t.Fatalf("sweep legs = %d, want 2", len(receipt.Legs))
	}
	if len(receipt.RuleLegs) != 2 {
		t.Fatalf("RuleLegs = %d, want 2 (one per sweep leg)", len(receipt.RuleLegs))
	}
	// floor(10000*1000/10000)=1000 on sub-a, floor(20000*1000/10000)=2000 on sub-b.
	if receipt.RuleLegs[0].AmountCents != 1000 || receipt.RuleLegs[1].AmountCents != 2000 {
		t.Fatalf("rule amounts = %v", receipt.RuleLegs)
	}
	if !strings.HasPrefix(receipt.RuleLegs[0].EntryID, "sweep-1/sub-a/") {
		t.Errorf("leg0 trigger = %q", receipt.RuleLegs[0].EntryID)
	}
	if !strings.HasSuffix(receipt.RuleLegs[0].EntryID, "/rule:sweep-reserve") {
		t.Errorf("leg0 suffix = %q", receipt.RuleLegs[0].EntryID)
	}
	if got := l.Balance("sweep-reserve"); got != 3000 {
		t.Errorf("reserve = %d, want 3000", got)
	}
	if got := l.Balance("master"); got != 30000 {
		t.Errorf("master = %d, want 30000", got)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Errorf("VerifyAccountingEquation: %v", err)
	}
	// Replay: sweep legs exclude rule legs, RuleLegs disclosed.
	r2, err := l.PostSweep(Sweep{
		ID: "sweep-2", From: []AccountID{"sub-a"}, To: "master",
		IdempotencyKey: "sweep-key-1",
	})
	if err != nil {
		t.Fatalf("PostSweep: %v", err)
	}
	_ = r2
	r3, err := l.PostSweep(Sweep{
		ID: "sweep-3", From: []AccountID{"sub-a"}, To: "master",
		IdempotencyKey: "sweep-key-1",
	})
	if err != nil {
		t.Fatalf("sweep replay: %v", err)
	}
	if !r3.Duplicate {
		t.Error("replay not marked duplicate")
	}
	if len(r3.Legs) != 0 {
		t.Errorf("replay legs = %d, want 0 (sweep-2 swept nothing new)", len(r3.Legs))
	}
}

func TestPostingRuleSweepReplayExcludesRuleLegs(t *testing.T) {
	rule := PostingRule{
		Name:      "sweep-fee",
		EventKind: PostingEventSweep,
		Legs: []PostingRuleLeg{
			{AccountTemplate: "sweep-fee-revenue", Side: "debit", AmountExpr: "fixed:50"},
		},
	}
	l := New(WithPostingRules([]PostingRule{rule}))
	seedBalance(t, l, "sub-a", 10000)
	r1, err := l.PostSweep(Sweep{
		ID: "sw-1", From: []AccountID{"sub-a"}, To: "master",
		IdempotencyKey: "sw-key-1",
	})
	if err != nil {
		t.Fatalf("PostSweep: %v", err)
	}
	if len(r1.Legs) != 1 || len(r1.RuleLegs) != 1 {
		t.Fatalf("legs=%d ruleLegs=%d", len(r1.Legs), len(r1.RuleLegs))
	}
	r2, err := l.PostSweep(Sweep{
		ID: "sw-2", From: []AccountID{"sub-a"}, To: "master",
		IdempotencyKey: "sw-key-1",
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	// The replay's Legs must not contain the rule leg (the sweepLegsOf
	// filter): exactly the one sweep leg.
	if len(r2.Legs) != 1 {
		t.Fatalf("replay legs = %d, want 1 (rule legs excluded)", len(r2.Legs))
	}
	if r2.Legs[0].EntryID != "sw-1/sub-a/"+DefaultCurrency {
		t.Errorf("leg entry = %q", r2.Legs[0].EntryID)
	}
	if len(r2.RuleLegs) != 1 || r2.RuleLegs[0].Rule != "sweep-fee" {
		t.Fatalf("replay RuleLegs = %v", r2.RuleLegs)
	}
}

func TestPostingRuleReconcileListsRules(t *testing.T) {
	l := New(WithPostingRules([]PostingRule{channelFeeRule()}))
	report := l.Reconcile(time.Now())
	if len(report.PostingRules) != 1 {
		t.Fatalf("PostingRules = %d, want 1", len(report.PostingRules))
	}
	if report.PostingRules[0].Name != "channel-fee" {
		t.Errorf("rule = %q", report.PostingRules[0].Name)
	}
}

func TestPostingRuleSnapshotRoundTrip(t *testing.T) {
	l := New(WithPostingRules([]PostingRule{channelFeeRule()}))
	seedBalance(t, l, "merchant-alice", 100000)
	if _, err := l.PostTransfer(Transfer{ID: "t1", From: "merchant-alice", To: "platform", AmountCents: 20000}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	l2, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	rules := l2.PostingRules()
	if len(rules) != 1 || rules[0].Name != "channel-fee" {
		t.Fatalf("restored rules = %v", rules)
	}
	// The restored ledger fires the rule: config round-tripped.
	if _, err := l2.PostTransfer(Transfer{ID: "t2", From: "merchant-alice", To: "platform", AmountCents: 20000}); err != nil {
		t.Fatalf("PostTransfer on restored: %v", err)
	}
	if _, exists := l2.entries["t2/rule:channel-fee"]; !exists {
		t.Fatal("rule did not fire on restored ledger")
	}
}

func TestPostingRuleConcurrent(t *testing.T) {
	l := New(WithPostingRules([]PostingRule{channelFeeRule()}))
	seedBalance(t, l, "merchant-alice", 10000000)
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := l.PostTransfer(Transfer{
				ID:          strings.ReplaceAll("conc-NNN", "NNN", string(rune('a'+i))),
				From:        "merchant-alice",
				To:          "platform",
				AmountCents: 10000,
			})
			if err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent PostTransfer: %v", err)
	}
	if got := l.PostingRuleLegCounts()["channel-fee"]; got != 20 {
		t.Fatalf("counter = %d, want 20", got)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Errorf("VerifyAccountingEquation: %v", err)
	}
}

func TestPostingRuleNoRulesNoOp(t *testing.T) {
	l := New()
	seedBalance(t, l, "alice", 10000)
	receipt, err := l.PostTransfer(Transfer{ID: "n1", From: "alice", To: "bob", AmountCents: 1000})
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if len(receipt.RuleLegs) != 0 {
		t.Fatalf("RuleLegs = %v, want none without rules", receipt.RuleLegs)
	}
	if len(l.PostingRuleLegCounts()) != 0 {
		t.Fatalf("counts = %v", l.PostingRuleLegCounts())
	}
}
