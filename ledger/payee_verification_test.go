package ledger

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// seedPayeeLedger returns a ledger with funded payer "payer" and a
// payee "payee" whose holder name is registered.
func seedPayeeLedger(t *testing.T, holderName string) *Ledger {
	t.Helper()
	l := New()
	if _, _, err := l.Post(JournalEntry{
		ID: "seed-1", DebitAccount: "payer", CreditAccount: "funding",
		AmountCents: 100000, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Post seed: %v", err)
	}
	if holderName != "" {
		if err := l.SetAccountHolderName("payee", holderName); err != nil {
			t.Fatalf("SetAccountHolderName: %v", err)
		}
	}
	return l
}

func postPayeeTransfer(t *testing.T, l *Ledger, id, expected, key string, confirmed bool) (TransferReceipt, error) {
	t.Helper()
	return l.PostTransfer(Transfer{
		ID:                 id,
		From:               "payer",
		To:                 "payee",
		AmountCents:        1000,
		ExpectedPayeeName:  expected,
		PayeeNameConfirmed: confirmed,
		IdempotencyKey:     key,
		CreatedAt:          time.Now(),
	})
}

func TestMatchPayeeNameTiers(t *testing.T) {
	const threshold = DefaultPayeeCloseMatchThreshold
	cases := []struct {
		name     string
		actual   string
		expected string
		want     string
	}{
		{"identical", "Alice Smith", "Alice Smith", PayeeMatchExact},
		{"case-insensitive", "Alice Smith", "alice smith", PayeeMatchExact},
		{"whitespace-normalized", "  Alice   Smith ", "alice smith", PayeeMatchExact},
		{"single-typo-close", "Alice Smith", "Alice Smyth", PayeeMatchClose},
		{"transliteration-close", "Alexander Petrov", "Aleksander Petrov", PayeeMatchClose},
		{"different-name", "Alice Smith", "Bob Jones", PayeeMatchNoMatch},
		{"empty-actual", "", "Alice Smith", PayeeMatchUnverifiable},
	}
	for _, c := range cases {
		if got := MatchPayeeName(c.actual, c.expected, threshold); got != c.want {
			t.Errorf("%s: MatchPayeeName(%q, %q) = %q, want %q", c.name, c.actual, c.expected, got, c.want)
		}
	}
}

func TestPayeeVerificationPolicyValidation(t *testing.T) {
	good := DefaultPayeeVerificationPolicy()
	if err := good.validate(); err != nil {
		t.Fatalf("default policy invalid: %v", err)
	}
	for _, p := range []PayeeVerificationPolicy{
		{CloseMatchThreshold: 0, OnNoMatch: PayeeMismatchReject, OnUnverifiable: PayeeMismatchAudit},
		{CloseMatchThreshold: 1.5, OnNoMatch: PayeeMismatchReject, OnUnverifiable: PayeeMismatchAudit},
		{CloseMatchThreshold: 0.8, OnNoMatch: "block", OnUnverifiable: PayeeMismatchAudit},
		{CloseMatchThreshold: 0.8, OnNoMatch: PayeeMismatchReject, OnUnverifiable: "warn"},
	} {
		if err := p.validate(); !errors.Is(err, ErrInvalidPayeeVerificationPolicy) {
			t.Errorf("policy %+v: err = %v, want ErrInvalidPayeeVerificationPolicy", p, err)
		}
	}
	l := New()
	if err := l.SetPayeeVerificationPolicy(PayeeVerificationPolicy{CloseMatchThreshold: 0}); !errors.Is(err, ErrInvalidPayeeVerificationPolicy) {
		t.Fatalf("SetPayeeVerificationPolicy(0): err = %v", err)
	}
	custom := PayeeVerificationPolicy{CloseMatchThreshold: 0.9, OnNoMatch: PayeeMismatchAudit, OnUnverifiable: PayeeMismatchReject}
	if err := l.SetPayeeVerificationPolicy(custom); err != nil {
		t.Fatalf("SetPayeeVerificationPolicy: %v", err)
	}
	if got := l.PayeeVerificationPolicy(); got != custom {
		t.Fatalf("policy = %+v, want %+v", got, custom)
	}
}

func TestPostTransferPayeeExactMatch(t *testing.T) {
	l := seedPayeeLedger(t, "Alice Smith")
	receipt, err := postPayeeTransfer(t, l, "t-exact", "alice smith", "k-exact", false)
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if receipt.PayeeMatch != PayeeMatchExact {
		t.Errorf("PayeeMatch = %q, want %q", receipt.PayeeMatch, PayeeMatchExact)
	}
	if bal := l.Balance("payee"); bal != 1000 {
		t.Errorf("payee balance = %d, want 1000", bal)
	}
}

func TestPostTransferPayeeCloseMatch(t *testing.T) {
	l := seedPayeeLedger(t, "Alice Smith")
	receipt, err := postPayeeTransfer(t, l, "t-close", "Alice Smyth", "k-close", false)
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if receipt.PayeeMatch != PayeeMatchClose {
		t.Errorf("PayeeMatch = %q, want %q", receipt.PayeeMatch, PayeeMatchClose)
	}
}

func TestPostTransferPayeeNoMatchRejects(t *testing.T) {
	l := seedPayeeLedger(t, "Alice Smith")
	before := l.Version()
	_, err := postPayeeTransfer(t, l, "t-nomatch", "Bob Jones", "k-nomatch", false)
	if !errors.Is(err, ErrPayeeNameMismatch) {
		t.Fatalf("err = %v, want ErrPayeeNameMismatch", err)
	}
	// A rejection records nothing: no journal rows, no version bump.
	if bal := l.Balance("payee"); bal != 0 {
		t.Errorf("payee balance = %d, want 0", bal)
	}
	if l.Version() != before {
		t.Errorf("version = %d, want %d (no bump on rejection)", l.Version(), before)
	}
}

func TestPostTransferPayeeNoMatchConfirmed(t *testing.T) {
	l := seedPayeeLedger(t, "Alice Smith")
	// Secondary confirmation: the requester vouches out-of-band.
	receipt, err := postPayeeTransfer(t, l, "t-confirmed", "Bob Jones", "k-confirmed", true)
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if receipt.PayeeMatch != PayeeMatchNoMatch {
		t.Errorf("PayeeMatch = %q, want %q", receipt.PayeeMatch, PayeeMatchNoMatch)
	}
	if bal := l.Balance("payee"); bal != 1000 {
		t.Errorf("payee balance = %d, want 1000", bal)
	}
}

func TestPostTransferPayeeNoMatchAuditPolicy(t *testing.T) {
	l := seedPayeeLedger(t, "Alice Smith")
	p := DefaultPayeeVerificationPolicy()
	p.OnNoMatch = PayeeMismatchAudit
	if err := l.SetPayeeVerificationPolicy(p); err != nil {
		t.Fatalf("SetPayeeVerificationPolicy: %v", err)
	}
	receipt, err := postPayeeTransfer(t, l, "t-audit", "Bob Jones", "k-audit", false)
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if receipt.PayeeMatch != PayeeMatchNoMatch {
		t.Errorf("PayeeMatch = %q, want %q", receipt.PayeeMatch, PayeeMatchNoMatch)
	}
}

func TestPostTransferPayeeUnverifiable(t *testing.T) {
	l := seedPayeeLedger(t, "") // no holder name registered
	receipt, err := postPayeeTransfer(t, l, "t-unver", "Alice Smith", "k-unver", false)
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if receipt.PayeeMatch != PayeeMatchUnverifiable {
		t.Errorf("PayeeMatch = %q, want %q", receipt.PayeeMatch, PayeeMatchUnverifiable)
	}
}

func TestPostTransferPayeeUnverifiableRejectPolicy(t *testing.T) {
	l := seedPayeeLedger(t, "")
	p := DefaultPayeeVerificationPolicy()
	p.OnUnverifiable = PayeeMismatchReject
	if err := l.SetPayeeVerificationPolicy(p); err != nil {
		t.Fatalf("SetPayeeVerificationPolicy: %v", err)
	}
	_, err := postPayeeTransfer(t, l, "t-unver-reject", "Alice Smith", "k-unver-reject", false)
	if !errors.Is(err, ErrPayeeNameMismatch) {
		t.Fatalf("err = %v, want ErrPayeeNameMismatch", err)
	}
}

func TestPostTransferPayeeCheckSkipped(t *testing.T) {
	l := seedPayeeLedger(t, "Alice Smith")
	receipt, err := postPayeeTransfer(t, l, "t-skip", "", "k-skip", false)
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if receipt.PayeeMatch != "" {
		t.Errorf("PayeeMatch = %q, want empty (check skipped)", receipt.PayeeMatch)
	}
}

func TestPostTransferPayeeIdempotentReplay(t *testing.T) {
	l := seedPayeeLedger(t, "Alice Smith")
	first, err := postPayeeTransfer(t, l, "t-replay-1", "Alice Smith", "k-replay", false)
	if err != nil {
		t.Fatalf("first PostTransfer: %v", err)
	}
	if first.Duplicate {
		t.Fatalf("first receipt is a duplicate")
	}
	// The holder name changes to a mismatch after the original posting:
	// the replay must return the original receipt, not re-check.
	if err := l.SetAccountHolderName("payee", "Mallory"); err != nil {
		t.Fatalf("SetAccountHolderName: %v", err)
	}
	second, err := postPayeeTransfer(t, l, "t-replay-2", "Bob Jones", "k-replay", false)
	if err != nil {
		t.Fatalf("replay PostTransfer: %v", err)
	}
	if !second.Duplicate {
		t.Errorf("replay receipt Duplicate = false, want true")
	}
	if bal := l.Balance("payee"); bal != 1000 {
		t.Errorf("payee balance = %d, want 1000 (replay books nothing new)", bal)
	}
}

func TestPostTransferExpectedPayeeNameTooLong(t *testing.T) {
	l := seedPayeeLedger(t, "Alice Smith")
	_, err := postPayeeTransfer(t, l, "t-long", strings.Repeat("x", 129), "k-long", false)
	if !errors.Is(err, ErrInvalidExpectedPayeeName) {
		t.Fatalf("err = %v, want ErrInvalidExpectedPayeeName", err)
	}
}

func TestVerifyPayeeNameStandalone(t *testing.T) {
	l := seedPayeeLedger(t, "Alice Smith")
	for expected, want := range map[string]string{
		"Alice Smith": PayeeMatchExact,
		"Alice Smyth": PayeeMatchClose,
		"Bob Jones":   PayeeMatchNoMatch,
	} {
		if got, err := l.VerifyPayeeName("payee", expected); err != nil || got != want {
			t.Errorf("VerifyPayeeName(%q) = %q, %v; want %q", expected, got, err, want)
		}
	}
	if got, err := l.VerifyPayeeName("ghost", "Alice Smith"); err != nil || got != PayeeMatchUnverifiable {
		t.Errorf("VerifyPayeeName(ghost) = %q, %v; want unverifiable", got, err)
	}
	if got, err := l.VerifyPayeeName("payee", ""); err != nil || got != "" {
		t.Errorf("VerifyPayeeName(empty) = %q, %v; want empty (opt-in)", got, err)
	}
}

func TestPayeeVerificationAuditEvents(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	if _, _, err := l.Post(JournalEntry{
		ID: "seed-1", DebitAccount: "payer", CreditAccount: "funding",
		AmountCents: 100000, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Post seed: %v", err)
	}
	if err := l.SetAccountHolderName("payee", "Alice Smith"); err != nil {
		t.Fatalf("SetAccountHolderName: %v", err)
	}
	if _, err := postPayeeTransfer(t, l, "t-a1", "Alice Smith", "k-a1", false); err != nil {
		t.Fatalf("exact: %v", err)
	}
	if _, err := postPayeeTransfer(t, l, "t-a2", "Bob Jones", "k-a2", false); err == nil {
		t.Fatalf("no-match: expected rejection")
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	byOp := auditEventsByOp(t, dir)
	verified := byOp["payee_name_verified"]
	if len(verified) != 1 {
		t.Fatalf("payee_name_verified events = %d, want 1", len(verified))
	}
	if verified[0].Details["match"] != PayeeMatchExact {
		t.Errorf("match = %v, want exact", verified[0].Details["match"])
	}
	rejected := byOp["payee_name_mismatch_rejected"]
	if len(rejected) != 1 {
		t.Fatalf("payee_name_mismatch_rejected events = %d, want 1", len(rejected))
	}
	// PII discipline: the names never appear in the audit trail.
	raw, _ := json.Marshal(byOp)
	if strings.Contains(string(raw), "Alice Smith") || strings.Contains(string(raw), "Bob Jones") {
		t.Errorf("audit trail leaks holder names")
	}
}

func TestDryRunTransferPayeeVerification(t *testing.T) {
	l := seedPayeeLedger(t, "Alice Smith")
	// The dry-run twin carries the holder-name registry: a mismatch
	// fails the dry run exactly like the real PostTransfer.
	res, err := l.DryRunTransfer(Transfer{
		ID: "dry-cop", From: "payer", To: "payee", AmountCents: 1000,
		ExpectedPayeeName: "Bob Jones", CreatedAt: time.Now(),
	})
	if !errors.Is(err, ErrPayeeNameMismatch) {
		t.Fatalf("dry run err = %v (wouldSucceed=%v), want ErrPayeeNameMismatch", err, res.WouldSucceed)
	}
	res, err = l.DryRunTransfer(Transfer{
		ID: "dry-cop-ok", From: "payer", To: "payee", AmountCents: 1000,
		ExpectedPayeeName: "Alice Smith", CreatedAt: time.Now(),
	})
	if err != nil || !res.WouldSucceed {
		t.Fatalf("dry run err = %v (wouldSucceed=%v), want success", err, res.WouldSucceed)
	}
}

func TestPostTransferPayeeConcurrent(t *testing.T) {
	l := seedPayeeLedger(t, "Alice Smith")
	var wg sync.WaitGroup
	errs := make([]error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = l.PostTransfer(Transfer{
				ID:                fmt.Sprintf("t-conc-%d", i),
				From:              "payer",
				To:                "payee",
				AmountCents:       100,
				ExpectedPayeeName: "Alice Smith",
				CreatedAt:         time.Now(),
			})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	if bal := l.Balance("payee"); bal != 2000 {
		t.Errorf("payee balance = %d, want 2000", bal)
	}
}
