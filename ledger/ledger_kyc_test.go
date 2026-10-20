package ledger

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestSetKycRoundTrip(t *testing.T) {
	l := New()
	verifiedAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if err := l.SetKyc("acme", KycProfile{
		Status:     KycVerified,
		RiskTier:   "low",
		VerifiedAt: verifiedAt,
	}); err != nil {
		t.Fatalf("SetKyc: %v", err)
	}
	p, ok := l.GetKyc("acme")
	if !ok {
		t.Fatal("GetKyc(acme) = not found after SetKyc")
	}
	if p.Status != KycVerified || p.RiskTier != "low" || !p.VerifiedAt.Equal(verifiedAt) {
		t.Fatalf("GetKyc(acme) = %+v, want verified/low/%v", p, verifiedAt)
	}
	// Replacing the profile overwrites it.
	if err := l.SetKyc("acme", KycProfile{Status: KycPending}); err != nil {
		t.Fatalf("SetKyc replace: %v", err)
	}
	p, _ = l.GetKyc("acme")
	if p.Status != KycPending {
		t.Fatalf("GetKyc(acme) after replace = %q, want pending", p.Status)
	}
	if _, ok := l.GetKyc("unknown"); ok {
		t.Fatal("GetKyc(unknown) = found, want not found")
	}
}

func TestSetKycValidation(t *testing.T) {
	l := New()
	if err := l.SetKyc("", KycProfile{Status: KycVerified}); !errors.Is(err, ErrKycInvalidStatus) {
		t.Fatalf("SetKyc empty account: got %v, want ErrKycInvalidStatus", err)
	}
	for _, s := range []KycStatus{"expired", "gold", ""} {
		if err := l.SetKyc("a", KycProfile{Status: s}); !errors.Is(err, ErrKycInvalidStatus) {
			t.Fatalf("SetKyc status %q: got %v, want ErrKycInvalidStatus", s, err)
		}
	}
	// The expired state is derived-only: it cannot be installed by hand.
	if err := l.SetKyc("a", KycProfile{Status: KycExpired}); !errors.Is(err, ErrKycInvalidStatus) {
		t.Fatalf("SetKyc expired: got %v, want ErrKycInvalidStatus", err)
	}
}

func TestSetKycStampsVerifiedAt(t *testing.T) {
	l := New()
	before := time.Now().UTC()
	if err := l.SetKyc("a", KycProfile{Status: KycVerified}); err != nil {
		t.Fatalf("SetKyc: %v", err)
	}
	p, _ := l.GetKyc("a")
	if p.VerifiedAt.IsZero() || p.VerifiedAt.Before(before) {
		t.Fatalf("VerifiedAt = %v, want stamped at/after %v", p.VerifiedAt, before)
	}
}

func TestSetKycThresholdValidation(t *testing.T) {
	l := New()
	if err := l.SetKycThreshold(-1); !errors.Is(err, ErrKycInvalidThreshold) {
		t.Fatalf("SetKycThreshold(-1): got %v, want ErrKycInvalidThreshold", err)
	}
	if err := l.SetKycThreshold(10000); err != nil {
		t.Fatalf("SetKycThreshold(10000): %v", err)
	}
	if got := l.KycThreshold(); got != 10000 {
		t.Fatalf("KycThreshold() = %d, want 10000", got)
	}
}

func TestKycThresholdGateOnPost(t *testing.T) {
	l := New()
	if err := l.SetKycThreshold(10000); err != nil {
		t.Fatalf("SetKycThreshold: %v", err)
	}
	if err := l.SetKyc("verified-co", KycProfile{Status: KycVerified}); err != nil {
		t.Fatalf("SetKyc: %v", err)
	}
	if err := l.SetKyc("pending-co", KycProfile{Status: KycPending}); err != nil {
		t.Fatalf("SetKyc: %v", err)
	}

	post := func(debit, credit AccountID, amount int64) error {
		e := validEntry()
		e.ID = fmt.Sprintf("e-%s-%s-%d", debit, credit, amount)
		e.IdempotencyKey = fmt.Sprintf("k-%s-%s-%d", debit, credit, amount)
		e.DebitAccount = debit
		e.CreditAccount = credit
		e.AmountCents = amount
		_, _, err := l.Post(e)
		return err
	}

	// Under the threshold, unverified accounts post fine.
	if err := post("cash", "equity", 5000); err != nil {
		t.Fatalf("Post 5000 under threshold: %v", err)
	}
	// Over the threshold, an unverified debit leg is rejected.
	if err := post("cash", "equity", 15000); !errors.Is(err, ErrKycUnverified) {
		t.Fatalf("Post 15000 unverified debit: got %v, want ErrKycUnverified", err)
	}
	// The gate covers the credit leg too.
	if err := post("verified-co", "equity", 15000); !errors.Is(err, ErrKycUnverified) {
		t.Fatalf("Post 15000 unverified credit: got %v, want ErrKycUnverified", err)
	}
	// Pending counts as not-verified for the threshold.
	if err := post("pending-co", "equity", 15000); !errors.Is(err, ErrKycUnverified) {
		t.Fatalf("Post 15000 pending debit: got %v, want ErrKycUnverified", err)
	}
	// A verified counterparty posts at any amount — but verified-co2
	// has no profile yet, so it is still rejected first.
	if err := post("verified-co", "verified-co2", 15000); !errors.Is(err, ErrKycUnverified) {
		t.Fatalf("Post 15000 to profile-less payee: got %v, want ErrKycUnverified", err)
	}
	if err := l.SetKyc("verified-co2", KycProfile{Status: KycVerified}); err != nil {
		t.Fatalf("SetKyc: %v", err)
	}
	if err := post("verified-co", "verified-co2", 15000); err != nil {
		t.Fatalf("Post 15000 between verified accounts: %v", err)
	}
	// Exactly at the threshold is allowed (the gate is strictly above).
	if err := post("cash", "equity", 10000); err != nil {
		t.Fatalf("Post exactly at threshold: %v", err)
	}
	// Rejected posts record nothing: only the two successful posts
	// (5000 + 10000) moved the balance.
	if got := l.Balance("cash"); got != 15000 {
		t.Fatalf("Balance(cash) = %d after rejections, want 15000", got)
	}
}

func TestKycThresholdDisabledByDefault(t *testing.T) {
	l := New()
	if got := l.KycThreshold(); got != 0 {
		t.Fatalf("KycThreshold() = %d, want 0 (disabled by default)", got)
	}
	// No threshold: unverified accounts post at any amount; only expiry
	// stops them.
	if _, _, err := l.Post(validEntry()); err != nil {
		t.Fatalf("Post with no KYC config: %v", err)
	}
}

func TestKycExpiredBlocksAnyAmount(t *testing.T) {
	l := New()
	// No threshold at all: expiry still blocks everything. The profile
	// is on the entry's debit leg (validEntry posts cash -> equity).
	past := time.Now().UTC().Add(-time.Hour)
	if err := l.SetKyc("cash", KycProfile{Status: KycVerified, ExpiresAt: past}); err != nil {
		t.Fatalf("SetKyc: %v", err)
	}
	// The lazy write-path sweep expires the profile before the gate.
	e := validEntry()
	e.AmountCents = 1
	_, _, err := l.Post(e)
	if !errors.Is(err, ErrKycExpired) {
		t.Fatalf("Post 1 cent through expired account: got %v, want ErrKycExpired", err)
	}
	p, ok := l.GetKyc("cash")
	if !ok || p.Status != KycExpired {
		t.Fatalf("GetKyc(cash) = %+v, %v; want expired profile", p, ok)
	}
}

func TestKycGateOnTransfer(t *testing.T) {
	l := New()
	if err := l.SetKycThreshold(10000); err != nil {
		t.Fatalf("SetKycThreshold: %v", err)
	}
	if err := l.SetKyc("payer", KycProfile{Status: KycVerified}); err != nil {
		t.Fatalf("SetKyc: %v", err)
	}
	if err := l.SetKyc("payee", KycProfile{Status: KycVerified}); err != nil {
		t.Fatalf("SetKyc: %v", err)
	}
	transfer := func(from, to AccountID, amount int64) error {
		_, err := l.PostTransfer(Transfer{ID: fmt.Sprintf("t-%s-%s-%d", from, to, amount), From: from, To: to, AmountCents: amount})
		return err
	}
	// Unverified payee over threshold: rejected.
	if err := transfer("payer", "stranger", 15000); !errors.Is(err, ErrKycUnverified) {
		t.Fatalf("Transfer to unverified payee: got %v, want ErrKycUnverified", err)
	}
	// Verified pair over threshold: clears.
	if err := transfer("payer", "payee", 15000); err != nil {
		t.Fatalf("Transfer between verified accounts: %v", err)
	}
	// Expired payer blocks even small transfers.
	past := time.Now().UTC().Add(-time.Hour)
	if err := l.SetKyc("oldpayer", KycProfile{Status: KycVerified, ExpiresAt: past}); err != nil {
		t.Fatalf("SetKyc: %v", err)
	}
	if err := transfer("oldpayer", "payee", 100); !errors.Is(err, ErrKycExpired) {
		t.Fatalf("Transfer from expired payer: got %v, want ErrKycExpired", err)
	}
}

func TestKycReplayBeatsGate(t *testing.T) {
	l := New()
	if err := l.SetKycThreshold(10000); err != nil {
		t.Fatalf("SetKycThreshold: %v", err)
	}
	e := validEntry()
	e.AmountCents = 5000
	posted, dup, err := l.Post(e)
	if err != nil || dup {
		t.Fatalf("first Post: err=%v dup=%v", err, dup)
	}
	// The account's KYC lapses after the posting.
	past := time.Now().UTC().Add(-time.Hour)
	if err := l.SetKyc("cash", KycProfile{Status: KycVerified, ExpiresAt: past}); err != nil {
		t.Fatalf("SetKyc: %v", err)
	}
	// Replaying the same key returns the original entry instead of
	// failing: the replay books nothing new.
	replayed, dup, err := l.Post(e)
	if err != nil {
		t.Fatalf("replay after KYC expiry: %v", err)
	}
	if !dup {
		t.Fatal("replay after KYC expiry: dup = false, want true")
	}
	if replayed.ID != posted.ID {
		t.Fatalf("replay returned entry %q, want %q", replayed.ID, posted.ID)
	}
}

func TestExpireKycSweep(t *testing.T) {
	l := New()
	now := time.Now().UTC()
	if err := l.SetKyc("a", KycProfile{Status: KycVerified, ExpiresAt: now.Add(-time.Hour)}); err != nil {
		t.Fatalf("SetKyc: %v", err)
	}
	if err := l.SetKyc("b", KycProfile{Status: KycVerified, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatalf("SetKyc: %v", err)
	}
	if err := l.SetKyc("c", KycProfile{Status: KycPending, ExpiresAt: now.Add(-time.Minute)}); err != nil {
		t.Fatalf("SetKyc: %v", err)
	}
	if err := l.SetKyc("d", KycProfile{Status: KycVerified}); err != nil {
		t.Fatalf("SetKyc: %v", err)
	}
	if got := l.ExpireKyc(now); got != 2 {
		t.Fatalf("ExpireKyc = %d, want 2 (a and c)", got)
	}
	for _, a := range []AccountID{"a", "c"} {
		if p, _ := l.GetKyc(a); p.Status != KycExpired {
			t.Fatalf("GetKyc(%q) = %q, want expired", a, p.Status)
		}
	}
	for _, a := range []AccountID{"b", "d"} {
		if p, _ := l.GetKyc(a); p.Status == KycExpired {
			t.Fatalf("GetKyc(%q) = expired, want untouched", a)
		}
	}
	// The sweep is idempotent: a second run expires nothing new.
	if got := l.ExpireKyc(now); got != 0 {
		t.Fatalf("ExpireKyc second run = %d, want 0", got)
	}
}

func TestKycReconcileDisclosure(t *testing.T) {
	l := New()
	if err := l.SetKycThreshold(5000); err != nil {
		t.Fatalf("SetKycThreshold: %v", err)
	}
	if err := l.SetKyc("v1", KycProfile{Status: KycVerified}); err != nil {
		t.Fatalf("SetKyc: %v", err)
	}
	if err := l.SetKyc("p1", KycProfile{Status: KycPending}); err != nil {
		t.Fatalf("SetKyc: %v", err)
	}
	if err := l.SetKyc("u1", KycProfile{Status: KycUnverified}); err != nil {
		t.Fatalf("SetKyc: %v", err)
	}
	past := time.Now().UTC().Add(-time.Hour)
	if err := l.SetKyc("x1", KycProfile{Status: KycVerified, ExpiresAt: past}); err != nil {
		t.Fatalf("SetKyc: %v", err)
	}
	l.ExpireKyc(time.Now().UTC())
	rep := l.Reconcile(time.Now().UTC())
	if rep.Kyc.ThresholdCents != 5000 {
		t.Fatalf("Kyc.ThresholdCents = %d, want 5000", rep.Kyc.ThresholdCents)
	}
	if rep.Kyc.Verified != 1 || rep.Kyc.Pending != 1 || rep.Kyc.Unverified != 1 || rep.Kyc.Expired != 1 {
		t.Fatalf("Kyc counts = %+v, want 1/1/1/1", rep.Kyc)
	}
	if len(rep.Kyc.ExpiredAccounts) != 1 || rep.Kyc.ExpiredAccounts[0] != "x1" {
		t.Fatalf("Kyc.ExpiredAccounts = %v, want [x1]", rep.Kyc.ExpiredAccounts)
	}
}

func TestKycSnapshotRoundTrip(t *testing.T) {
	l := New()
	if err := l.SetKycThreshold(7000); err != nil {
		t.Fatalf("SetKycThreshold: %v", err)
	}
	if err := l.SetKyc("a", KycProfile{Status: KycVerified, RiskTier: "low"}); err != nil {
		t.Fatalf("SetKyc: %v", err)
	}
	if err := l.SetKyc("b", KycProfile{Status: KycPending}); err != nil {
		t.Fatalf("SetKyc: %v", err)
	}
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(&buf)
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if got := restored.KycThreshold(); got != 7000 {
		t.Fatalf("restored KycThreshold() = %d, want 7000", got)
	}
	p, ok := restored.GetKyc("a")
	if !ok || p.Status != KycVerified || p.RiskTier != "low" {
		t.Fatalf("restored GetKyc(a) = %+v, %v", p, ok)
	}
	if _, ok := restored.GetKyc("b"); !ok {
		t.Fatal("restored GetKyc(b) = not found")
	}
}
