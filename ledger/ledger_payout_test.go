package ledger

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

// seedPayoutLedger builds a ledger with a funded payee account and the
// payout clearing account configured.
func seedPayoutLedger(t *testing.T, opts ...Option) *Ledger {
	t.Helper()
	all := append([]Option{WithPayoutClearingAccount("payout-clearing")}, opts...)
	l := New(all...)
	seedBalance(t, l, "saver", 1_000_000)
	return l
}

func draftPayout(t *testing.T, l *Ledger, id string, amount int64) PayoutInstruction {
	t.Helper()
	p, dup, err := l.CreatePayout(PayoutInstruction{
		ID:           id,
		PayeeAccount: "saver",
		AmountCents:  amount,
		Destination:  "FPS:6222000012345678",
		Maker:        "alice",
	})
	if err != nil {
		t.Fatalf("CreatePayout: %v", err)
	}
	if dup {
		t.Fatal("duplicate = true on first create")
	}
	return p
}

func approveSubmitPayout(t *testing.T, l *Ledger, id, approver string) PayoutInstruction {
	t.Helper()
	p, err := l.ApprovePayout(id, approver)
	if err != nil {
		t.Fatalf("ApprovePayout: %v", err)
	}
	p, err = l.SubmitPayout(id)
	if err != nil {
		t.Fatalf("SubmitPayout: %v", err)
	}
	return p
}

func TestCreatePayout_HappyPath(t *testing.T) {
	l := seedPayoutLedger(t)
	p := draftPayout(t, l, "po-1", 250_000)
	if p.Status != PayoutDraft {
		t.Errorf("status = %q, want draft", p.Status)
	}
	if p.Currency != "USD" {
		t.Errorf("currency = %q, want USD default", p.Currency)
	}
	if p.Maker != "alice" || p.Approver != "" {
		t.Errorf("maker/approver = %q/%q, want alice/\"\"", p.Maker, p.Approver)
	}
	// Drafting is off-journal: balances untouched.
	if b := l.Balance("saver"); b != 1_000_000 {
		t.Errorf("balance = %d, want 1000000", b)
	}
}

func TestCreatePayout_Validation(t *testing.T) {
	l := seedPayoutLedger(t)
	cases := []struct {
		name string
		p    PayoutInstruction
		want error
	}{
		{"empty ID", PayoutInstruction{PayeeAccount: "saver", AmountCents: 100, Destination: "x", Maker: "a"}, ErrEmptyPayoutID},
		{"slash ID", PayoutInstruction{ID: "a/b", PayeeAccount: "saver", AmountCents: 100, Destination: "x", Maker: "a"}, ErrPayoutIDSeparator},
		{"empty account", PayoutInstruction{ID: "x", AmountCents: 100, Destination: "x", Maker: "a"}, ErrEmptyPayoutAccount},
		{"zero amount", PayoutInstruction{ID: "x", PayeeAccount: "saver", Destination: "x", Maker: "a"}, ErrPayoutNonPositiveAmount},
		{"empty destination", PayoutInstruction{ID: "x", PayeeAccount: "saver", AmountCents: 100, Maker: "a"}, ErrEmptyPayoutDestination},
		{"empty maker", PayoutInstruction{ID: "x", PayeeAccount: "saver", AmountCents: 100, Destination: "x"}, ErrEmptyPayoutMaker},
		{"bad currency", PayoutInstruction{ID: "x", PayeeAccount: "saver", AmountCents: 100, Destination: "x", Maker: "a", Currency: "XX"}, ErrInvalidCurrency},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := l.CreatePayout(c.p); !errors.Is(err, c.want) {
				t.Errorf("err = %v, want %v", err, c.want)
			}
		})
	}
}

func TestCreatePayout_IdempotencyAndConflict(t *testing.T) {
	l := seedPayoutLedger(t)
	p1, dup, err := l.CreatePayout(PayoutInstruction{
		ID: "po-key", PayeeAccount: "saver", AmountCents: 100,
		Destination: "x", Maker: "alice", IdempotencyKey: "k-po",
	})
	if err != nil || dup {
		t.Fatalf("create: %v dup=%v", err, dup)
	}
	p2, dup, err := l.CreatePayout(PayoutInstruction{
		ID: "po-key-other", PayeeAccount: "saver", AmountCents: 999,
		Destination: "y", Maker: "bob", IdempotencyKey: "k-po",
	})
	if err != nil || !dup {
		t.Fatalf("replay: %v dup=%v, want replay", err, dup)
	}
	if p2.ID != p1.ID || p2.AmountCents != p1.AmountCents {
		t.Errorf("replay returned %+v, want original %+v", p2, p1)
	}
	// Same ID, different key: conflict, not replay.
	if _, _, err := l.CreatePayout(PayoutInstruction{
		ID: "po-key", PayeeAccount: "saver", AmountCents: 100,
		Destination: "x", Maker: "alice", IdempotencyKey: "k-po-2",
	}); !errors.Is(err, ErrPayoutIDConflict) {
		t.Errorf("conflict err = %v, want ErrPayoutIDConflict", err)
	}
	if _, ok := l.GetPayout("missing"); ok {
		t.Error("GetPayout(missing) = true, want false")
	}
}

func TestCreatePayout_DestinationValidatedAgainstMapping(t *testing.T) {
	l := seedPayoutLedger(t)
	if err := l.SetExternalMapping("saver", "FPS", "6222000012345678", "saver fps"); err != nil {
		t.Fatalf("SetExternalMapping: %v", err)
	}
	// Matching the registered LG-122 mapping: OK.
	if _, _, err := l.CreatePayout(PayoutInstruction{
		ID: "po-map-ok", PayeeAccount: "saver", AmountCents: 100,
		Destination: "FPS:6222000012345678", Maker: "alice",
	}); err != nil {
		t.Errorf("mapped destination: %v", err)
	}
	// Same system, different external ID: rejected.
	if _, _, err := l.CreatePayout(PayoutInstruction{
		ID: "po-map-bad", PayeeAccount: "saver", AmountCents: 100,
		Destination: "FPS:9999999999999999", Maker: "alice",
	}); !errors.Is(err, ErrPayoutUnknownDestination) {
		t.Errorf("mismatched destination err = %v, want ErrPayoutUnknownDestination", err)
	}
	// Unregistered system: free-form, no validation.
	if _, _, err := l.CreatePayout(PayoutInstruction{
		ID: "po-map-free", PayeeAccount: "saver", AmountCents: 100,
		Destination: "SWIFT:DEUTDEFF", Maker: "alice",
	}); err != nil {
		t.Errorf("unregistered system destination: %v", err)
	}
}

func TestApprovePayout_MakerChecker(t *testing.T) {
	// No threshold configured: the maker may self-approve.
	l := seedPayoutLedger(t)
	draftPayout(t, l, "po-self", 50_000)
	if p, err := l.ApprovePayout("po-self", "alice"); err != nil || p.Approver != "alice" {
		t.Errorf("self-approve without threshold: %v %+v", err, p)
	}

	// At/above the LG-42 review threshold the checker must differ.
	l2 := seedPayoutLedger(t, WithReviewThreshold(100_000))
	draftPayout(t, l2, "po-mc", 100_000)
	if _, err := l2.ApprovePayout("po-mc", "alice"); !errors.Is(err, ErrPayoutMakerCheckerViolation) {
		t.Errorf("maker self-approve at threshold: %v, want violation", err)
	}
	p, err := l2.ApprovePayout("po-mc", "bob")
	if err != nil || p.Status != PayoutApproved || p.Approver != "bob" {
		t.Errorf("checker approve: %v %+v", err, p)
	}
	// Below the threshold the maker may still self-approve.
	draftPayout(t, l2, "po-small", 10_000)
	if _, err := l2.ApprovePayout("po-small", "alice"); err != nil {
		t.Errorf("self-approve below threshold: %v", err)
	}
	// Re-approving is idempotent; approving a non-draft is rejected.
	if _, err := l2.ApprovePayout("po-mc", "carol"); err != nil {
		t.Errorf("re-approve: %v", err)
	}
	if _, err := l2.ApprovePayout("missing", "bob"); !errors.Is(err, ErrPayoutNotFound) {
		t.Errorf("approve missing: %v, want not found", err)
	}
	if _, err := l2.ApprovePayout("po-mc", ""); !errors.Is(err, ErrEmptyPayoutApprover) {
		t.Errorf("empty approver: %v, want ErrEmptyPayoutApprover", err)
	}
}

func TestSubmitPayout_HappyPath(t *testing.T) {
	l := seedPayoutLedger(t)
	draftPayout(t, l, "po-sub", 250_000)
	p := approveSubmitPayout(t, l, "po-sub", "bob")
	if p.Status != PayoutSubmitted {
		t.Fatalf("status = %q, want submitted", p.Status)
	}
	if p.ClearingEntryID != "payout/po-sub/clearing" {
		t.Errorf("clearing entry = %q, want payout/po-sub/clearing", p.ClearingEntryID)
	}
	// Submit books debit clearing / credit payee: the payee's balance
	// drops, the clearing account holds the earmark.
	if b := l.Balance("saver"); b != 750_000 {
		t.Errorf("saver balance = %d, want 750000", b)
	}
	if b := l.Balance("payout-clearing"); b != 250_000 {
		t.Errorf("clearing balance = %d, want 250000", b)
	}
	// Re-submitting is a no-op success.
	if p2, err := l.SubmitPayout("po-sub"); err != nil || p2.ClearingEntryID != p.ClearingEntryID {
		t.Errorf("re-submit: %v %+v", err, p2)
	}
	// Submitting a draft (not approved) is rejected.
	draftPayout(t, l, "po-draft", 100)
	if _, err := l.SubmitPayout("po-draft"); !errors.Is(err, ErrPayoutInvalidTransition) {
		t.Errorf("submit draft: %v, want invalid transition", err)
	}
}

func TestSubmitPayout_ClearingNotConfigured(t *testing.T) {
	l := New()
	seedBalance(t, l, "saver", 1_000_000)
	draftPayout(t, l, "po-noclear", 100)
	if _, err := l.ApprovePayout("po-noclear", "bob"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := l.SubmitPayout("po-noclear"); !errors.Is(err, ErrPayoutClearingAccountNotConfigured) {
		t.Errorf("submit without clearing: %v, want not configured", err)
	}
}

func TestSubmitPayout_FrozenPayeeRejected(t *testing.T) {
	l := seedPayoutLedger(t)
	draftPayout(t, l, "po-frozen", 100_000)
	if _, err := l.ApprovePayout("po-frozen", "bob"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	l.Freeze("saver")
	if _, err := l.SubmitPayout("po-frozen"); !errors.Is(err, ErrAccountFrozen) {
		t.Errorf("submit on frozen: %v, want ErrAccountFrozen", err)
	}
	// The instruction stays approved — the operator can retry after
	// unfreezing.
	if p, _ := l.GetPayout("po-frozen"); p.Status != PayoutApproved {
		t.Errorf("status = %q, want approved", p.Status)
	}
}

func statementLineForPayout(p PayoutInstruction, at time.Time) StatementLine {
	return StatementLine{
		ID:          "stmt-" + p.ID,
		Account:     p.PayeeAccount,
		Currency:    p.Currency,
		Direction:   StatementDirectionOut,
		AmountCents: p.AmountCents,
		PostedAt:    at,
		Memo:        "payout:" + p.ID,
	}
}

func TestSettlePayout_HappyPath(t *testing.T) {
	l := seedPayoutLedger(t)
	draftPayout(t, l, "po-settle", 250_000)
	p := approveSubmitPayout(t, l, "po-settle", "bob")

	p, err := l.SettlePayout("po-settle", statementLineForPayout(p, p.SubmittedAt), StatementMatchOptions{})
	if err != nil {
		t.Fatalf("SettlePayout: %v", err)
	}
	if p.Status != PayoutSettled {
		t.Fatalf("status = %q, want settled", p.Status)
	}
	if p.SettlementEntryID != "payout/po-settle/settle" {
		t.Errorf("settlement entry = %q", p.SettlementEntryID)
	}
	if p.StatementLineID != "stmt-po-settle" {
		t.Errorf("statement line = %q", p.StatementLineID)
	}
	// The clearing suspense nets to zero; the payee's balance is back to
	// the pre-submit level (the outflow itself is evidenced by the
	// matched statement line, not by a second journal leg).
	if b := l.Balance("payout-clearing"); b != 0 {
		t.Errorf("clearing balance = %d, want 0", b)
	}
	if b := l.Balance("saver"); b != 1_000_000 {
		t.Errorf("saver balance = %d, want 1000000", b)
	}
	// Re-settling is a no-op success.
	if _, err := l.SettlePayout("po-settle", statementLineForPayout(p, p.SubmittedAt), StatementMatchOptions{}); err != nil {
		t.Errorf("re-settle: %v", err)
	}
}

func TestSettlePayout_StatementMismatch(t *testing.T) {
	l := seedPayoutLedger(t)
	draftPayout(t, l, "po-mm", 250_000)
	p := approveSubmitPayout(t, l, "po-mm", "bob")

	cases := []struct {
		name string
		line StatementLine
	}{
		{"wrong amount", StatementLine{ID: "s1", Account: "saver", Currency: "USD", Direction: StatementDirectionOut, AmountCents: 1, PostedAt: p.SubmittedAt}},
		{"inflow direction", StatementLine{ID: "s2", Account: "saver", Currency: "USD", Direction: StatementDirectionIn, AmountCents: 250_000, PostedAt: p.SubmittedAt}},
		{"outside window", StatementLine{ID: "s3", Account: "saver", Currency: "USD", Direction: StatementDirectionOut, AmountCents: 250_000, PostedAt: p.SubmittedAt.Add(30 * 24 * time.Hour)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := l.SettlePayout("po-mm", c.line, StatementMatchOptions{}); !errors.Is(err, ErrPayoutStatementMismatch) {
				t.Errorf("err = %v, want mismatch", err)
			}
		})
	}
	// The instruction stays submitted after failed settles.
	if p, _ := l.GetPayout("po-mm"); p.Status != PayoutSubmitted {
		t.Errorf("status = %q, want submitted", p.Status)
	}
	// A line that matches a DIFFERENT journal row is rejected even when
	// the amount is right: book a decoy posting first.
	if _, _, err := l.Post(JournalEntry{ID: "decoy", DebitAccount: "saver", CreditAccount: "ops", AmountCents: 250_000, Currency: "USD"}); err != nil {
		t.Fatalf("decoy post: %v", err)
	}
	ambiguous := statementLineForPayout(p, p.SubmittedAt)
	ambiguous.ID = "s-ambig"
	if _, err := l.SettlePayout("po-mm", ambiguous, StatementMatchOptions{}); !errors.Is(err, ErrPayoutStatementMismatch) {
		t.Errorf("ambiguous settle err = %v, want mismatch", err)
	}
	// Settling a draft is an invalid transition.
	draftPayout(t, l, "po-mm-draft", 100)
	if _, err := l.SettlePayout("po-mm-draft", statementLineForPayout(p, time.Now()), StatementMatchOptions{}); !errors.Is(err, ErrPayoutInvalidTransition) {
		t.Errorf("settle draft err = %v, want invalid transition", err)
	}
}

func TestFailPayout(t *testing.T) {
	l := seedPayoutLedger(t)
	draftPayout(t, l, "po-fail", 100_000)
	p := approveSubmitPayout(t, l, "po-fail", "bob")

	p, err := l.FailPayout("po-fail", "beneficiary bank rejected: account closed")
	if err != nil {
		t.Fatalf("FailPayout: %v", err)
	}
	if p.Status != PayoutFailed || p.FailReason == "" {
		t.Errorf("payout = %+v, want failed with reason", p)
	}
	if p.SettlementEntryID != "payout/po-fail/fail" {
		t.Errorf("settlement entry = %q", p.SettlementEntryID)
	}
	// The suspense reverses: clearing nets to zero, the payee is whole.
	if b := l.Balance("payout-clearing"); b != 0 {
		t.Errorf("clearing balance = %d, want 0", b)
	}
	if b := l.Balance("saver"); b != 1_000_000 {
		t.Errorf("saver balance = %d, want 1000000", b)
	}
	// A reason is required; re-failing is idempotent.
	if _, err := l.FailPayout("po-fail", ""); !errors.Is(err, ErrEmptyPayoutFailReason) {
		t.Errorf("empty reason: %v, want ErrEmptyPayoutFailReason", err)
	}
	if _, err := l.FailPayout("po-fail", "again"); err != nil {
		t.Errorf("re-fail: %v", err)
	}
	// Failing a draft is an invalid transition.
	draftPayout(t, l, "po-fail-draft", 100)
	if _, err := l.FailPayout("po-fail-draft", "x"); !errors.Is(err, ErrPayoutInvalidTransition) {
		t.Errorf("fail draft: %v, want invalid transition", err)
	}
	_ = p
}

func TestCancelPayout(t *testing.T) {
	l := seedPayoutLedger(t)
	// Cancel from draft.
	draftPayout(t, l, "po-cancel-draft", 100)
	p, err := l.CancelPayout("po-cancel-draft")
	if err != nil || p.Status != PayoutCancelled {
		t.Errorf("cancel draft: %v %+v", err, p)
	}
	// Cancel from approved.
	draftPayout(t, l, "po-cancel-appr", 100)
	if _, err := l.ApprovePayout("po-cancel-appr", "bob"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if p, err := l.CancelPayout("po-cancel-appr"); err != nil || p.Status != PayoutCancelled {
		t.Errorf("cancel approved: %v %+v", err, p)
	}
	// Cancelling books nothing.
	if b := l.Balance("saver"); b != 1_000_000 {
		t.Errorf("balance = %d, want 1000000", b)
	}
	// Cancel from submitted is rejected: settle or fail instead.
	draftPayout(t, l, "po-cancel-sub", 100)
	approveSubmitPayout(t, l, "po-cancel-sub", "bob")
	if _, err := l.CancelPayout("po-cancel-sub"); !errors.Is(err, ErrPayoutInvalidTransition) {
		t.Errorf("cancel submitted: %v, want invalid transition", err)
	}
	// Re-cancelling is idempotent.
	if _, err := l.CancelPayout("po-cancel-draft"); err != nil {
		t.Errorf("re-cancel: %v", err)
	}
}

func TestPayout_ReconcileDisclosure(t *testing.T) {
	l := seedPayoutLedger(t)
	draftPayout(t, l, "po-recon", 100_000)
	approveSubmitPayout(t, l, "po-recon", "bob")

	rep := l.Reconcile(time.Now())
	byID := map[string]PayoutReport{}
	for _, r := range rep.Payouts {
		byID[r.ID] = r
	}
	r, ok := byID["po-recon"]
	if !ok {
		t.Fatal("po-recon missing from reconcile")
	}
	if r.Status != PayoutSubmitted || r.ClearingEntryID != "payout/po-recon/clearing" {
		t.Errorf("report = %+v, want submitted with clearing entry", r)
	}
	if r.Maker != "alice" || r.Approver != "bob" || r.Destination != "FPS:6222000012345678" {
		t.Errorf("report = %+v, want maker/approver/destination", r)
	}
}

func TestPayout_ClearingAccountSnapshotRoundTrip(t *testing.T) {
	l := seedPayoutLedger(t)
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	l2, err := ImportSnapshot(&buf)
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	// The fail-closed clearing account survives; instructions do not
	// (in-memory only, like clearing batches).
	draftPayout(t, l2, "po-restored", 100)
	if _, err := l2.ApprovePayout("po-restored", "bob"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := l2.SubmitPayout("po-restored"); err != nil {
		t.Errorf("submit after restore: %v (clearing account lost)", err)
	}
}

func TestPayout_IdempotencyKeyExpiry(t *testing.T) {
	l := New(WithPayoutClearingAccount("payout-clearing"), WithIdempotencyTTL(time.Hour))
	seedBalance(t, l, "saver", 1_000_000)
	if _, _, err := l.CreatePayout(PayoutInstruction{
		ID: "po-ttl", PayeeAccount: "saver", AmountCents: 100,
		Destination: "x", Maker: "alice", IdempotencyKey: "k-ttl",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Prune with a clock past the TTL: the key index shrinks, the
	// instruction stays.
	l.prunePayoutKeysLocked(time.Now().Add(2 * time.Hour))
	if _, dup, err := l.CreatePayout(PayoutInstruction{
		ID: "po-ttl-2", PayeeAccount: "saver", AmountCents: 100,
		Destination: "x", Maker: "alice", IdempotencyKey: "k-ttl",
	}); err != nil || dup {
		t.Errorf("replay after TTL: err=%v dup=%v, want fresh draft", err, dup)
	}
	if _, ok := l.GetPayout("po-ttl"); !ok {
		t.Error("original instruction evicted with its key")
	}
}
