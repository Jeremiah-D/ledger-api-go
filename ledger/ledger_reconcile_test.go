package ledger

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func reconcileEntry(id string, debit, credit AccountID, amount int64, key string, at time.Time) JournalEntry {
	return JournalEntry{
		ID:             id,
		DebitAccount:   debit,
		CreditAccount:  credit,
		AmountCents:    amount,
		IdempotencyKey: key,
		CreatedAt:      at,
	}
}

func seedReconcileLedger(t *testing.T, l *Ledger, now time.Time) {
	t.Helper()
	posts := []JournalEntry{
		reconcileEntry("r-1", "cash", "equity", 1000, "rkey-1", now.Add(-26*time.Hour)),
		reconcileEntry("r-2", "cash", "equity", 500, "rkey-2", now.Add(-25*time.Hour)),
		reconcileEntry("r-3", "fees", "cash", 150, "rkey-3", now.Add(-24*time.Hour)),
	}
	for _, e := range posts {
		if _, dup, err := l.Post(e); err != nil || dup {
			t.Fatalf("Post(%s) = dup=%v err=%v", e.ID, dup, err)
		}
	}
}

// A healthy ledger reconciles clean: equation holds, no discrepancies,
// every active account appears exactly once in sorted order, the chain is
// intact and in step with the version, and the key index reflects reality.
func TestReconcileHealthyLedger(t *testing.T) {
	l := New()
	now := time.Date(2026, 10, 7, 18, 0, 0, 0, time.UTC)
	seedReconcileLedger(t, l, now)

	report := l.Reconcile(now)

	if report.Version != 3 {
		t.Errorf("version = %d, want 3", report.Version)
	}
	if !report.GeneratedAt.Equal(now) {
		t.Errorf("generated_at = %v, want caller-supplied %v", report.GeneratedAt, now)
	}
	if !report.AccountingEquationOK {
		t.Fatalf("accounting_equation_ok = false, accounting_error = %q", report.AccountingError)
	}
	if report.AccountingError != "" {
		t.Errorf("accounting_error = %q, want empty on healthy ledger", report.AccountingError)
	}
	if report.Discrepancies == nil {
		t.Errorf("discrepancies is nil; a clean run must encode as an empty list")
	}
	if len(report.Discrepancies) != 0 {
		t.Errorf("discrepancies = %v, want empty", report.Discrepancies)
	}
	// Global totals give the reconciler an at-a-glance balance check.
	if report.TotalDebitsCents != 1650 || report.TotalCreditsCents != 1650 {
		t.Errorf("totals = %d/%d, want 1650/1650", report.TotalDebitsCents, report.TotalCreditsCents)
	}

	// All active accounts are covered, sorted, with per-account equation
	// fields consistent.
	if report.TrialBalances == nil {
		t.Fatalf("trial_balances is nil; active accounts must be listed")
	}
	var accounts []string
	for _, tb := range report.TrialBalances {
		accounts = append(accounts, string(tb.Account))
		if tb.NetBalance != tb.TotalDebits-tb.TotalCredits {
			t.Errorf("account %q trial balance inconsistent: net %d != debits %d - credits %d",
				tb.Account, tb.NetBalance, tb.TotalDebits, tb.TotalCredits)
		}
		if tb.Version != 3 {
			t.Errorf("account %q version = %d, want 3", tb.Account, tb.Version)
		}
	}
	if len(accounts) != 3 ||
		accounts[0] != "cash" || accounts[1] != "equity" || accounts[2] != "fees" {
		t.Errorf("trial balance accounts = %v, want [cash equity fees] sorted", accounts)
	}

	// Chain: intact, and head consistent with the ledger version.
	if !report.AuditChain.VerifyOK {
		t.Errorf("audit chain verify_ok = false, error = %q", report.AuditChain.VerifyError)
	}
	if report.AuditChain.VerifyError != "" {
		t.Errorf("verify_error = %q, want empty on healthy chain", report.AuditChain.VerifyError)
	}
	if report.AuditChain.Links != 3 {
		t.Errorf("links = %d, want 3", report.AuditChain.Links)
	}
	if len(report.AuditChain.Head) != 64 {
		t.Errorf("head = %q, want 64 hex chars", report.AuditChain.Head)
	}
	if !report.AuditChain.HeadConsistent {
		t.Errorf("head_consistent = false, consistency_error = %q", report.AuditChain.ConsistencyError)
	}

	// Idempotency keys: no TTL configured, so none can be expired.
	if report.IdempotencyKeys.TTLConfigured {
		t.Errorf("ttl_configured = true, want false without WithIdempotencyTTL")
	}
	if report.IdempotencyKeys.TotalKeys != 3 {
		t.Errorf("total_keys = %d, want 3", report.IdempotencyKeys.TotalKeys)
	}
	if report.IdempotencyKeys.ExpiredEligible != 0 {
		t.Errorf("expired_eligible = %d, want 0 without TTL", report.IdempotencyKeys.ExpiredEligible)
	}
}

// An empty ledger reconciles to a fully healthy report with empty lists —
// "ran clean", not "did not run".
func TestReconcileEmptyLedger(t *testing.T) {
	l := New()
	now := time.Date(2026, 10, 7, 18, 0, 0, 0, time.UTC)
	report := l.Reconcile(now)

	if !report.AccountingEquationOK || !report.AuditChain.VerifyOK || !report.AuditChain.HeadConsistent {
		t.Fatalf("empty ledger report = %+v, want all checks green", report)
	}
	if len(report.TrialBalances) != 0 || len(report.Discrepancies) != 0 {
		t.Errorf("empty ledger lists: trial_balances=%v discrepancies=%v, want both empty",
			report.TrialBalances, report.Discrepancies)
	}
	if report.AuditChain.Links != 0 || report.AuditChain.Head != strings.Repeat("0", 64) {
		t.Errorf("empty ledger chain = head %q links %d, want genesis head 0 links",
			report.AuditChain.Head, report.AuditChain.Links)
	}

	var buf bytes.Buffer
	if err := report.WriteJSON(&buf); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	// The clean lists must serialize as [], not null — otherwise a
	// consumer cannot tell a healthy report from a malformed one.
	out := buf.String()
	if !strings.Contains(out, `"trial_balances": []`) {
		t.Errorf("report JSON lacks an empty trial_balances list:\n%s", out)
	}
	if !strings.Contains(out, `"discrepancies": []`) {
		t.Errorf("report JSON lacks an empty discrepancies list:\n%s", out)
	}
}

// A storage-layer corruption that skews a net balance away from its totals
// is caught as a discrepancy with the exact shortfall, and the accounting
// equation fails with the account named.
func TestReconcileDetectsBalanceCorruption(t *testing.T) {
	l := New()
	now := time.Date(2026, 10, 7, 18, 0, 0, 0, time.UTC)
	seedReconcileLedger(t, l, now)

	// Cash's true net after the seed: debits 1500, credits 150 → 1350.
	// A corruptor skims 50 cents off the net balance row only.
	l.balances["cash"] -= 50

	report := l.Reconcile(now)

	if report.AccountingEquationOK {
		t.Fatalf("accounting_equation_ok = true after balance corruption")
	}
	if !strings.Contains(report.AccountingError, `"cash"`) {
		t.Errorf("accounting_error %q does not name the corrupted account", report.AccountingError)
	}
	if len(report.Discrepancies) != 1 {
		t.Fatalf("discrepancies = %v, want exactly the cash row", report.Discrepancies)
	}
	d := report.Discrepancies[0]
	if d.Account != "cash" {
		t.Errorf("discrepancy account = %q, want cash", d.Account)
	}
	if d.TotalDebitsCents != 1500 || d.TotalCreditsCents != 150 {
		t.Errorf("discrepancy totals = %d/%d, want 1500/150",
			d.TotalDebitsCents, d.TotalCreditsCents)
	}
	if d.NetBalanceCents != 1300 {
		t.Errorf("net_balance_cents = %d, want corrupted 1300", d.NetBalanceCents)
	}
	if d.ExpectedNetCents != 1350 {
		t.Errorf("expected_net_cents = %d, want 1350", d.ExpectedNetCents)
	}
	if d.DifferenceCents != -50 {
		t.Errorf("difference_cents = %d, want -50", d.DifferenceCents)
	}

	// The chain was untouched, so it still verifies and stays consistent:
	// the report separates book-keeping corruption from tamper detection.
	if !report.AuditChain.VerifyOK || !report.AuditChain.HeadConsistent {
		t.Errorf("chain checks went red on a pure balance corruption: %+v", report.AuditChain)
	}
}

// Rewriting a journaled entry after posting breaks the audit chain, and the
// report surfaces the chain verify error without hiding the (still clean)
// equation result.
func TestReconcileDetectsChainTamper(t *testing.T) {
	l := New()
	now := time.Date(2026, 10, 7, 18, 0, 0, 0, time.UTC)
	seedReconcileLedger(t, l, now)

	// Same technique as the chain tamper tests: rewrite a journaled
	// amount in place, exactly like a storage-layer corruption would.
	corrupt := l.entries["r-2"]
	corrupt.AmountCents = 999999
	l.entries["r-2"] = corrupt

	report := l.Reconcile(now)

	if report.AuditChain.VerifyOK {
		t.Fatalf("audit chain verify_ok = true after entry rewrite")
	}
	if !strings.Contains(report.AuditChain.VerifyError, "r-2") {
		t.Errorf("verify_error %q does not name the tampered entry", report.AuditChain.VerifyError)
	}
	// Net balances and totals were not touched, so the equation still
	// holds; chain tamper and book imbalance are independent axes.
	if !report.AccountingEquationOK {
		t.Errorf("accounting_equation_ok = false on pure chain tamper, error = %q",
			report.AccountingError)
	}
	if len(report.Discrepancies) != 0 {
		t.Errorf("discrepancies = %v, want empty on pure chain tamper", report.Discrepancies)
	}
}

// A chain/ledger version desync (more links than version) is flagged as
// head-inconsistent even though each link verifies on its own.
func TestReconcileDetectsChainHeadInconsistency(t *testing.T) {
	l := New()
	now := time.Date(2026, 10, 7, 18, 0, 0, 0, time.UTC)
	seedReconcileLedger(t, l, now)

	// Duplicate the last link: the chain still recomputes (the splice
	// keeps PrevHash continuity and entry IDs hash correctly), but the
	// link count now exceeds the ledger version.
	l.chain = append(l.chain, l.chain[len(l.chain)-1])

	report := l.Reconcile(now)

	if report.AuditChain.HeadConsistent {
		t.Fatalf("head_consistent = true with %d links at version %d",
			report.AuditChain.Links, report.Version)
	}
	if !strings.Contains(report.AuditChain.ConsistencyError, "4 links") ||
		!strings.Contains(report.AuditChain.ConsistencyError, "version is 3") {
		t.Errorf("consistency_error %q does not describe the desync",
			report.AuditChain.ConsistencyError)
	}
}

// With a TTL configured, keys posted more than the TTL ago are counted as
// expired-eligible — the same set a key sweep would evict.
func TestReconcileIdempotencyTTLCountsExpired(t *testing.T) {
	l := New(WithIdempotencyTTL(time.Hour))
	now := time.Date(2026, 10, 7, 18, 0, 0, 0, time.UTC)
	posts := []JournalEntry{
		reconcileEntry("t-1", "cash", "equity", 100, "ttl-old-1", now.Add(-2*time.Hour)),
		reconcileEntry("t-2", "cash", "equity", 100, "ttl-old-2", now.Add(-61*time.Minute)),
		reconcileEntry("t-3", "cash", "equity", 100, "ttl-fresh", now.Add(-30*time.Minute)),
	}
	for _, e := range posts {
		if _, dup, err := l.Post(e); err != nil || dup {
			t.Fatalf("Post(%s) = dup=%v err=%v", e.ID, dup, err)
		}
	}

	report := l.Reconcile(now)

	if !report.IdempotencyKeys.TTLConfigured {
		t.Errorf("ttl_configured = false, want true")
	}
	if report.IdempotencyKeys.TTL != (time.Hour).String() {
		t.Errorf("ttl = %q, want %q", report.IdempotencyKeys.TTL, (time.Hour).String())
	}
	if report.IdempotencyKeys.TotalKeys != 3 {
		t.Errorf("total_keys = %d, want 3", report.IdempotencyKeys.TotalKeys)
	}
	if report.IdempotencyKeys.ExpiredEligible != 2 {
		t.Errorf("expired_eligible = %d, want 2 (the two keys older than 1h)",
			report.IdempotencyKeys.ExpiredEligible)
	}
}

// The same ledger shape, scanned at the same now, serializes byte-identical:
// operators can diff archived reports with plain diff.
func TestReconcileDeterministicOutput(t *testing.T) {
	l := New()
	now := time.Date(2026, 10, 7, 18, 0, 0, 0, time.UTC)
	seedReconcileLedger(t, l, now)

	var a, b bytes.Buffer
	if err := l.Reconcile(now).WriteJSON(&a); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	if err := l.Reconcile(now).WriteJSON(&b); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	if a.String() != b.String() {
		t.Errorf("two scans at the same now serialized differently:\n%s\n---\n%s", a.String(), b.String())
	}

	// The indented form round-trips through the standard decoder.
	var decoded ReconciliationReport
	if err := json.Unmarshal(a.Bytes(), &decoded); err != nil {
		t.Fatalf("round-trip decode: %v", err)
	}
	if decoded.Version != 3 || !decoded.AccountingEquationOK || !decoded.AuditChain.VerifyOK {
		t.Errorf("round-tripped report = %+v, want version 3 and green checks", decoded)
	}
}
