package ledger

import (
	"errors"
	"testing"
	"time"
)

func TestExtendPreAuthExpirySuccess(t *testing.T) {
	l, _ := preauthFixture(t)
	before := time.Now().Add(time.Hour).Truncate(time.Second)
	p, _, err := l.AuthorizePreAuth(PreAuth{
		ID:          "pa-1",
		MandateID:   "m-sub",
		AmountCents: 2000,
		Currency:    "USD",
		ExpiresAt:   before,
	})
	if err != nil {
		t.Fatalf("AuthorizePreAuth: %v", err)
	}
	versionBefore := l.Version()
	extended := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	got, err := l.ExtendPreAuthExpiry("m-sub", "pa-1", extended)
	if err != nil {
		t.Fatalf("ExtendPreAuthExpiry: %v", err)
	}
	// The extension moves the deadline and nothing else.
	if !got.ExpiresAt.Equal(extended) {
		t.Errorf("expires_at = %v, want %v", got.ExpiresAt, extended)
	}
	if got.AmountCents != 2000 || got.Currency != "USD" || got.Status != PreAuthStatusActive {
		t.Errorf("extended preauth = %+v, want amount 2000 USD active", got)
	}
	if got.ID != "pa-1" || got.MandateID != "m-sub" {
		t.Errorf("extended preauth = %+v, want pa-1/m-sub", got)
	}
	if !p.CreatedAt.Equal(got.CreatedAt) {
		t.Errorf("created_at moved: %v -> %v", p.CreatedAt, got.CreatedAt)
	}
	if l.Version() != versionBefore {
		t.Error("version moved: extension is off-journal")
	}
	// The stored row carries the new deadline.
	stored, ok := l.GetPreAuth("pa-1")
	if !ok || !stored.ExpiresAt.Equal(extended) || stored.Status != PreAuthStatusActive {
		t.Errorf("stored preauth = %+v,%v, want active with expiry %v", stored, ok, extended)
	}
	// The lazy predicate and the sweep read the live deadline: a
	// sweep at the old deadline expires nothing, and one at the
	// new deadline expires the pre-authorization.
	if got := l.ExpirePreAuthsAt(before); got != 0 {
		t.Errorf("sweep at old expiry expired = %d, want 0", got)
	}
	if got := l.ExpirePreAuthsAt(extended); got != 1 {
		t.Errorf("sweep at new expiry expired = %d, want 1", got)
	}
}

func TestExtendPreAuthExpiryRejections(t *testing.T) {
	now := time.Now()
	before := now.Add(time.Hour).Truncate(time.Second)
	newer := now.Add(3 * time.Hour).Truncate(time.Second)

	// Unknown pre-authorization, and a pre-authorization under a
	// different mandate than the route's.
	l, _ := preauthFixture(t)
	authorizeOne(t, l, "pa-1")
	if _, err := l.ExtendPreAuthExpiry("m-sub", "nope", newer); !errors.Is(err, ErrPreAuthNotFound) {
		t.Errorf("unknown preauth: err = %v, want ErrPreAuthNotFound", err)
	}
	if _, err := l.ExtendPreAuthExpiry("other-mandate", "pa-1", newer); !errors.Is(err, ErrPreAuthNotFound) {
		t.Errorf("wrong mandate: err = %v, want ErrPreAuthNotFound", err)
	}

	// Equal, earlier, and past new deadlines all fail, and the
	// original deadline is untouched.
	for _, tc := range []struct {
		name string
		next time.Time
	}{
		{"equal", before},
		{"earlier", now.Add(30 * time.Minute)},
		{"past", now.Add(-time.Hour)},
	} {
		ll, _ := preauthFixture(t)
		orig := authorizeOne(t, ll, "pa-1")
		next := tc.next
		if tc.name == "equal" {
			next = orig.ExpiresAt
		}
		if _, err := ll.ExtendPreAuthExpiry("m-sub", "pa-1", next); !errors.Is(err, ErrPreAuthExpiryExtensionNotLater) {
			t.Errorf("%s: err = %v, want ErrPreAuthExpiryExtensionNotLater", tc.name, err)
		}
		stored, _ := ll.GetPreAuth("pa-1")
		if !stored.ExpiresAt.Equal(orig.ExpiresAt) {
			t.Errorf("%s: expiry moved to %v after rejected extension, want %v", tc.name, stored.ExpiresAt, orig.ExpiresAt)
		}
	}

	// Captured and released pre-authorizations are terminal.
	lc, _ := preauthFixture(t)
	authorizeOne(t, lc, "pa-1")
	if _, err := lc.CapturePreAuth("m-sub", PreAuthCapture{ID: "cap-1", PreAuthID: "pa-1"}); err != nil {
		t.Fatalf("CapturePreAuth: %v", err)
	}
	if _, err := lc.ExtendPreAuthExpiry("m-sub", "pa-1", newer); !errors.Is(err, ErrPreAuthNotActive) {
		t.Errorf("captured: err = %v, want ErrPreAuthNotActive", err)
	}
	lr, _ := preauthFixture(t)
	authorizeOne(t, lr, "pa-1")
	if _, err := lr.ReleasePreAuth("m-sub", "pa-1"); err != nil {
		t.Fatalf("ReleasePreAuth: %v", err)
	}
	if _, err := lr.ExtendPreAuthExpiry("m-sub", "pa-1", newer); !errors.Is(err, ErrPreAuthNotActive) {
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
	if _, err := le.ExtendPreAuthExpiry("m-sub", "pa-born-expired", newer); !errors.Is(err, ErrPreAuthExpired) {
		t.Errorf("born expired: err = %v, want ErrPreAuthExpired", err)
	}
	ls, _ := preauthFixture(t)
	authorizeOne(t, ls, "pa-1")
	if got := ls.ExpirePreAuthsAt(now.Add(2 * time.Hour)); got != 1 {
		t.Fatalf("ExpirePreAuthsAt: expired = %d, want 1", got)
	}
	if _, err := ls.ExtendPreAuthExpiry("m-sub", "pa-1", newer); !errors.Is(err, ErrPreAuthExpired) {
		t.Errorf("swept expired: err = %v, want ErrPreAuthExpired", err)
	}
	// Note: the born-expired case above already covers the lazy
	// predicate — its stored status is still active, and only
	// preauthExpiredLocked marks it expired — so no separate
	// unswept-lapse case is needed.
}

func TestExtendPreAuthExpiryAudit(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	// Rebuild the mandate fixture on the audited ledger.
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
	before := now.Add(time.Hour).Truncate(time.Second)
	if _, _, err := l.AuthorizePreAuth(PreAuth{
		ID: "pa-1", MandateID: "m-sub",
		AmountCents: 2000, Currency: "USD", ExpiresAt: before,
	}); err != nil {
		t.Fatalf("AuthorizePreAuth: %v", err)
	}
	extended := now.Add(3 * time.Hour).Truncate(time.Second)
	if _, err := l.ExtendPreAuthExpiry("m-sub", "pa-1", extended); err != nil {
		t.Fatalf("ExtendPreAuthExpiry: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["preauth_expiry_extended"]
	if len(events) != 1 {
		t.Fatalf("extend events = %d, want 1", len(events))
	}
	d := events[0].Details
	if d["mandate_id"] != "m-sub" || d["amount_cents"] != float64(2000) {
		t.Errorf("event details = %+v, want m-sub, 2000 cents", d)
	}
	if d["previous_expires_at"] != before.UTC().Format(time.RFC3339) {
		t.Errorf("previous_expires_at = %v, want %v", d["previous_expires_at"], before.UTC().Format(time.RFC3339))
	}
	if d["expires_at"] != extended.UTC().Format(time.RFC3339) {
		t.Errorf("expires_at = %v, want %v", d["expires_at"], extended.UTC().Format(time.RFC3339))
	}
}
