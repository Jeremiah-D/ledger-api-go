package ledger

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAmendPreAuthExpiryShortenThenSweepExpiresEarly(t *testing.T) {
	l, _ := preauthFixture(t)
	p := authorizeOne(t, l, "pa-1")
	now := time.Now()
	// Shorten the deadline from +1h to +10m: the sweep at +30m
	// must now expire it, where the original deadline would have
	// survived.
	shortened := now.Add(10 * time.Minute)
	got, err := l.AmendPreAuthExpiry("m-sub", "pa-1", shortened)
	if err != nil {
		t.Fatalf("AmendPreAuthExpiry: %v", err)
	}
	if !got.ExpiresAt.Equal(shortened) {
		t.Errorf("expires_at = %v, want %v", got.ExpiresAt, shortened)
	}
	if got.AmountCents != p.AmountCents || got.Currency != p.Currency ||
		got.MandateID != p.MandateID || got.Status != PreAuthStatusActive ||
		got.IdempotencyKey != p.IdempotencyKey || !got.CreatedAt.Equal(p.CreatedAt) {
		t.Errorf("amended preauth = %+v, want only the expiry moved from %+v", got, p)
	}
	if expired := l.ExpirePreAuthsAt(now.Add(30 * time.Minute)); expired != 1 {
		t.Fatalf("ExpirePreAuthsAt(+30m): expired = %d, want 1", expired)
	}
	stored, ok := l.GetPreAuth("pa-1")
	if !ok || stored.Status != PreAuthStatusExpired {
		t.Errorf("stored preauth = %+v,%v, want expired", stored, ok)
	}
}

func TestAmendPreAuthExpiryLengthenSurvivesOldDeadline(t *testing.T) {
	l, _ := preauthFixture(t)
	authorizeOne(t, l, "pa-1")
	now := time.Now()
	lengthened := now.Add(3 * time.Hour)
	got, err := l.AmendPreAuthExpiry("m-sub", "pa-1", lengthened)
	if err != nil {
		t.Fatalf("AmendPreAuthExpiry: %v", err)
	}
	_ = got
	// The sweep at +2h — past the original +1h deadline — must
	// leave the lengthened reservation alone.
	if expired := l.ExpirePreAuthsAt(now.Add(2 * time.Hour)); expired != 0 {
		t.Fatalf("ExpirePreAuthsAt(+2h): expired = %d, want 0", expired)
	}
	stored, ok := l.GetPreAuth("pa-1")
	if !ok || stored.Status != PreAuthStatusActive || !stored.ExpiresAt.Equal(lengthened) {
		t.Errorf("stored preauth = %+v,%v, want active with expiry %v", stored, ok, lengthened)
	}
}

func TestAmendPreAuthExpiryRejections(t *testing.T) {
	// Unknown pre-authorization, and a pre-authorization under a
	// different mandate than the route's.
	l, _ := preauthFixture(t)
	authorizeOne(t, l, "pa-1")
	now := time.Now()
	if _, err := l.AmendPreAuthExpiry("m-sub", "nope", now.Add(time.Hour)); !errors.Is(err, ErrPreAuthNotFound) {
		t.Errorf("unknown preauth: err = %v, want ErrPreAuthNotFound", err)
	}
	if _, err := l.AmendPreAuthExpiry("other-mandate", "pa-1", now.Add(time.Hour)); !errors.Is(err, ErrPreAuthNotFound) {
		t.Errorf("wrong mandate: err = %v, want ErrPreAuthNotFound", err)
	}

	// A past or present deadline is refused, and the live
	// deadline is untouched.
	for _, tc := range []struct {
		name string
		when time.Time
	}{
		{"past", now.Add(-time.Hour)},
		{"now", now},
	} {
		ll, _ := preauthFixture(t)
		authorizeOne(t, ll, "pa-1")
		if _, err := ll.AmendPreAuthExpiry("m-sub", "pa-1", tc.when); !errors.Is(err, ErrPreAuthExpiryAmendInvalid) {
			t.Errorf("%s: err = %v, want ErrPreAuthExpiryAmendInvalid", tc.name, err)
		}
		stored, _ := ll.GetPreAuth("pa-1")
		if !stored.ExpiresAt.After(now) {
			t.Errorf("%s: live expiry moved to %v after rejected amendment", tc.name, stored.ExpiresAt)
		}
	}

	// Captured and released pre-authorizations are terminal.
	lcap, _ := preauthFixture(t)
	authorizeOne(t, lcap, "pa-1")
	if _, err := lcap.CapturePreAuth("m-sub", PreAuthCapture{ID: "cap-1", PreAuthID: "pa-1"}); err != nil {
		t.Fatalf("CapturePreAuth: %v", err)
	}
	if _, err := lcap.AmendPreAuthExpiry("m-sub", "pa-1", now.Add(2*time.Hour)); !errors.Is(err, ErrPreAuthNotActive) {
		t.Errorf("captured: err = %v, want ErrPreAuthNotActive", err)
	}
	lrel, _ := preauthFixture(t)
	authorizeOne(t, lrel, "pa-1")
	if _, err := lrel.ReleasePreAuth("m-sub", "pa-1"); err != nil {
		t.Fatalf("ReleasePreAuth: %v", err)
	}
	if _, err := lrel.AmendPreAuthExpiry("m-sub", "pa-1", now.Add(2*time.Hour)); !errors.Is(err, ErrPreAuthNotActive) {
		t.Errorf("released: err = %v, want ErrPreAuthNotActive", err)
	}

	// Expired pre-authorizations: born expired, and swept expired.
	le, _ := preauthFixture(t)
	if _, _, err := le.AuthorizePreAuth(PreAuth{
		ID: "pa-born-expired", MandateID: "m-sub",
		AmountCents: 2000, Currency: "USD",
		ExpiresAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("AuthorizePreAuth born expired: %v", err)
	}
	if _, err := le.AmendPreAuthExpiry("m-sub", "pa-born-expired", now.Add(time.Hour)); !errors.Is(err, ErrPreAuthExpired) {
		t.Errorf("born expired: err = %v, want ErrPreAuthExpired", err)
	}
	ls, _ := preauthFixture(t)
	authorizeOne(t, ls, "pa-1")
	if got := ls.ExpirePreAuthsAt(now.Add(2 * time.Hour)); got != 1 {
		t.Fatalf("ExpirePreAuthsAt: expired = %d, want 1", got)
	}
	if _, err := ls.AmendPreAuthExpiry("m-sub", "pa-1", now.Add(3*time.Hour)); !errors.Is(err, ErrPreAuthExpired) {
		t.Errorf("swept expired: err = %v, want ErrPreAuthExpired", err)
	}
}

func TestAmendPreAuthExpiryNoop(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	if _, _, err := l.Post(JournalEntry{ID: "fund-cust", DebitAccount: "cust", CreditAccount: "bank", AmountCents: 100000, Currency: "USD"}); err != nil {
		t.Fatalf("fund Post: %v", err)
	}
	now := time.Now()
	if _, err := l.CreateMandate(Mandate{
		ID: "m-sub", Payer: "cust", Payee: "merchant",
		MaxAmountCents: 5000, Currency: "USD",
		Frequency: MandateFrequencyPerDay,
		ValidFrom: now.Add(-time.Hour), ValidUntil: now.Add(30 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("CreateMandate: %v", err)
	}
	p, _, err := l.AuthorizePreAuth(PreAuth{
		ID: "pa-1", MandateID: "m-sub",
		AmountCents: 2000, Currency: "USD", ExpiresAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("AuthorizePreAuth: %v", err)
	}
	versionBefore := l.Version()
	got, err := l.AmendPreAuthExpiry("m-sub", "pa-1", p.ExpiresAt)
	if err != nil {
		t.Fatalf("AmendPreAuthExpiry same value: %v", err)
	}
	if !got.ExpiresAt.Equal(p.ExpiresAt) || got.Status != PreAuthStatusActive {
		t.Errorf("no-op preauth = %+v, want original expiry active", got)
	}
	if l.Version() != versionBefore {
		t.Error("version moved: amendment is off-journal")
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if events := auditEventsByOp(t, dir)["preauth_expiry_amended"]; len(events) != 0 {
		t.Errorf("amend events = %d, want 0 for a no-op", len(events))
	}
}

func TestAmendPreAuthExpiryAudit(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	if _, _, err := l.Post(JournalEntry{ID: "fund-cust", DebitAccount: "cust", CreditAccount: "bank", AmountCents: 100000, Currency: "USD"}); err != nil {
		t.Fatalf("fund Post: %v", err)
	}
	now := time.Now()
	if _, err := l.CreateMandate(Mandate{
		ID: "m-sub", Payer: "cust", Payee: "merchant",
		MaxAmountCents: 5000, Currency: "USD",
		Frequency: MandateFrequencyPerDay,
		ValidFrom: now.Add(-time.Hour), ValidUntil: now.Add(30 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("CreateMandate: %v", err)
	}
	p, _, err := l.AuthorizePreAuth(PreAuth{
		ID: "pa-1", MandateID: "m-sub",
		AmountCents: 2000, Currency: "USD", ExpiresAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("AuthorizePreAuth: %v", err)
	}
	versionBefore := l.Version()
	newExpiry := now.Add(2 * time.Hour)
	ctx := ContextWithActor(context.Background(), "ops-alice")
	got, err := l.AmendPreAuthExpiryCtx(ctx, "m-sub", "pa-1", newExpiry)
	if err != nil {
		t.Fatalf("AmendPreAuthExpiryCtx: %v", err)
	}
	if !got.ExpiresAt.Equal(newExpiry) {
		t.Errorf("expires_at = %v, want %v", got.ExpiresAt, newExpiry)
	}
	if l.Version() != versionBefore {
		t.Error("version moved: amendment is off-journal")
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["preauth_expiry_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
	if events[0].Actor != "ops-alice" {
		t.Errorf("event actor = %q, want ops-alice", events[0].Actor)
	}
	d := events[0].Details
	if d["mandate_id"] != "m-sub" ||
		d["previous_expires_at"] != p.ExpiresAt.UTC().Format(time.RFC3339) ||
		d["expires_at"] != newExpiry.UTC().Format(time.RFC3339) ||
		d["amount_cents"] != float64(2000) || d["currency"] != "USD" {
		t.Errorf("event details = %+v, want m-sub %v->%v 2000 USD", d,
			p.ExpiresAt.UTC().Format(time.RFC3339), newExpiry.UTC().Format(time.RFC3339))
	}
}
