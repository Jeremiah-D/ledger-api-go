package ledger

import (
	"errors"
	"testing"
	"time"
)

// preauthFixture authorizes nothing yet: it returns the standard
// mandate fixture (cust holds 100000 USD, mandate m-sub, cap 5000
// USD/debit, per-day, valid for 30 days).
func preauthFixture(t *testing.T) (*Ledger, Mandate) {
	t.Helper()
	l, m := mandateFixture(t)
	return l, m
}

// authorizeOne is the happy-path authorize used across tests:
// pre-authorization id on m-sub, 2000 USD, 1-hour expiry.
func authorizeOne(t *testing.T, l *Ledger, id string) PreAuth {
	t.Helper()
	p, dup, err := l.AuthorizePreAuth(PreAuth{
		ID:          id,
		MandateID:   "m-sub",
		AmountCents: 2000,
		Currency:    "USD",
		ExpiresAt:   time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("AuthorizePreAuth: %v", err)
	}
	if dup {
		t.Fatalf("AuthorizePreAuth returned duplicate on first authorize")
	}
	return p
}

func TestAuthorizePreAuthValidation(t *testing.T) {
	l, _ := preauthFixture(t)
	now := time.Now()
	cases := []struct {
		name    string
		mutate  func(*PreAuth)
		wantErr error
	}{
		{"empty ID", func(p *PreAuth) { p.ID = "" }, ErrEmptyPreAuthID},
		{"whitespace ID", func(p *PreAuth) { p.ID = "pa 1" }, ErrPreAuthIDWhitespace},
		{"empty mandate ID", func(p *PreAuth) { p.MandateID = "" }, ErrEmptyPreAuthMandateID},
		{"zero amount", func(p *PreAuth) { p.AmountCents = 0 }, ErrPreAuthNonPositiveAmount},
		{"negative amount", func(p *PreAuth) { p.AmountCents = -5 }, ErrPreAuthNonPositiveAmount},
		{"bad currency", func(p *PreAuth) { p.Currency = "US" }, ErrInvalidCurrency},
		{"missing expiry", func(p *PreAuth) { p.ExpiresAt = time.Time{} }, ErrPreAuthMissingExpiry},
		{"unknown mandate", func(p *PreAuth) { p.MandateID = "nope" }, ErrMandateNotFound},
		{"over cap", func(p *PreAuth) { p.AmountCents = 5001 }, ErrMandateAmountExceeded},
		{"currency mismatch", func(p *PreAuth) { p.Currency = "EUR" }, ErrMandateCurrencyMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ll, _ := preauthFixture(t)
			p := PreAuth{
				ID:          "pa-x",
				MandateID:   "m-sub",
				AmountCents: 2000,
				Currency:    "USD",
				ExpiresAt:   now.Add(time.Hour),
			}
			tc.mutate(&p)
			if _, _, err := ll.AuthorizePreAuth(p); !errors.Is(err, tc.wantErr) {
				t.Errorf("AuthorizePreAuth err = %v, want %v", err, tc.wantErr)
			}
		})
	}
	_ = l
}

func TestAuthorizePreAuthHappyPath(t *testing.T) {
	l, _ := preauthFixture(t)
	versionBefore := l.version
	p := authorizeOne(t, l, "pa-1")
	if p.Status != PreAuthStatusActive {
		t.Errorf("status = %q, want active", p.Status)
	}
	if p.MandateID != "m-sub" || p.AmountCents != 2000 || p.Currency != "USD" {
		t.Errorf("preauth = %+v, want m-sub/2000/USD", p)
	}
	// An authorization books nothing: no journal row, no version bump,
	// no balance movement.
	if _, ok := l.entries["pa-1"]; ok {
		t.Errorf("authorize journaled an entry")
	}
	if l.version != versionBefore {
		t.Errorf("version = %d, want %d (authorize is off-journal)", l.version, versionBefore)
	}
	if got := l.BalanceIn("cust", "USD"); got != 100000 {
		t.Errorf("cust balance = %d, want 100000 (authorize moves no money)", got)
	}
}

func TestAuthorizePreAuthIDConflict(t *testing.T) {
	l, _ := preauthFixture(t)
	authorizeOne(t, l, "pa-1")
	_, _, err := l.AuthorizePreAuth(PreAuth{
		ID:          "pa-1",
		MandateID:   "m-sub",
		AmountCents: 100,
		Currency:    "USD",
		ExpiresAt:   time.Now().Add(time.Hour),
	})
	if !errors.Is(err, ErrPreAuthIDConflict) {
		t.Errorf("err = %v, want ErrPreAuthIDConflict", err)
	}
}

func TestAuthorizePreAuthMandateGates(t *testing.T) {
	now := time.Now()
	// Suspended mandate authorizes nothing.
	l, _ := preauthFixture(t)
	if _, err := l.SuspendMandate("m-sub"); err != nil {
		t.Fatalf("SuspendMandate: %v", err)
	}
	if _, _, err := l.AuthorizePreAuth(PreAuth{ID: "pa-s", MandateID: "m-sub", AmountCents: 100, Currency: "USD", ExpiresAt: now.Add(time.Hour)}); !errors.Is(err, ErrMandateNotActive) {
		t.Errorf("suspended mandate err = %v, want ErrMandateNotActive", err)
	}

	// A mandate already past its ValidUntil is expired (lazy): the
	// creation gate only requires ValidFrom < ValidUntil, both may lie
	// in the past.
	l2 := New()
	if _, _, err := l2.Post(JournalEntry{ID: "fund", DebitAccount: "cust", CreditAccount: "bank", AmountCents: 100000, Currency: "USD"}); err != nil {
		t.Fatalf("fund Post: %v", err)
	}
	if _, err := l2.CreateMandate(Mandate{ID: "m-old", Payer: "cust", Payee: "merchant", MaxAmountCents: 5000, Currency: "USD", Frequency: MandateFrequencyPerDay, ValidFrom: now.Add(-48 * time.Hour), ValidUntil: now.Add(-time.Hour)}); err != nil {
		t.Fatalf("CreateMandate: %v", err)
	}
	if _, _, err := l2.AuthorizePreAuth(PreAuth{ID: "pa-e", MandateID: "m-old", AmountCents: 100, Currency: "USD", ExpiresAt: now.Add(time.Hour)}); !errors.Is(err, ErrMandateExpired) {
		t.Errorf("expired mandate err = %v, want ErrMandateExpired", err)
	}

	// A mandate whose ValidFrom is in the future is not yet effective.
	l3 := New()
	if _, _, err := l3.Post(JournalEntry{ID: "fund", DebitAccount: "cust", CreditAccount: "bank", AmountCents: 100000, Currency: "USD"}); err != nil {
		t.Fatalf("fund Post: %v", err)
	}
	if _, err := l3.CreateMandate(Mandate{ID: "m-new", Payer: "cust", Payee: "merchant", MaxAmountCents: 5000, Currency: "USD", Frequency: MandateFrequencyPerDay, ValidFrom: now.Add(time.Hour), ValidUntil: now.Add(48 * time.Hour)}); err != nil {
		t.Fatalf("CreateMandate: %v", err)
	}
	if _, _, err := l3.AuthorizePreAuth(PreAuth{ID: "pa-f", MandateID: "m-new", AmountCents: 100, Currency: "USD", ExpiresAt: now.Add(time.Hour)}); !errors.Is(err, ErrMandateNotActive) {
		t.Errorf("future mandate err = %v, want ErrMandateNotActive", err)
	}
}

func TestAuthorizePreAuthIdempotentReplay(t *testing.T) {
	l, _ := preauthFixture(t)
	now := time.Now()
	first, dup, err := l.AuthorizePreAuth(PreAuth{
		ID:             "pa-1",
		MandateID:      "m-sub",
		AmountCents:    2000,
		Currency:       "USD",
		ExpiresAt:      now.Add(time.Hour),
		IdempotencyKey: "auth-key-1",
	})
	if err != nil || dup {
		t.Fatalf("first authorize: err=%v dup=%v", err, dup)
	}
	second, dup, err := l.AuthorizePreAuth(PreAuth{
		ID:             "pa-other",
		MandateID:      "m-sub",
		AmountCents:    999,
		Currency:       "USD",
		ExpiresAt:      now.Add(time.Hour),
		IdempotencyKey: "auth-key-1",
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !dup {
		t.Errorf("replay dup = false, want true")
	}
	if second.ID != first.ID || second.AmountCents != 2000 {
		t.Errorf("replay returned %+v, want the original pa-1/2000", second)
	}
	if got := len(l.preauths); got != 1 {
		t.Errorf("preauths = %d, want 1 (replay books nothing new)", got)
	}
	auth, _, _ := l.PreAuthCounts()
	if auth != 1 {
		t.Errorf("authorized count = %d, want 1", auth)
	}
}

// The reservation occupies the frequency window exactly like a debit:
// a pre-authorization blocks a parallel debit and a parallel
// authorization, and a debit blocks a later authorization.
func TestPreAuthFrequencyGate(t *testing.T) {
	// Authorize, then a direct debit in the same window fails.
	l, _ := preauthFixture(t)
	authorizeOne(t, l, "pa-1")
	_, err := l.DebitWithMandate("m-sub", MandateDebit{ID: "d-1", AmountCents: 1000, Currency: "USD"})
	if !errors.Is(err, ErrMandateFrequencyExceeded) {
		t.Errorf("debit after authorize err = %v, want ErrMandateFrequencyExceeded", err)
	}

	// A second authorization in the same window fails too.
	l2, _ := preauthFixture(t)
	authorizeOne(t, l2, "pa-1")
	_, _, err = l2.AuthorizePreAuth(PreAuth{ID: "pa-2", MandateID: "m-sub", AmountCents: 1000, Currency: "USD", ExpiresAt: time.Now().Add(time.Hour)})
	if !errors.Is(err, ErrMandateFrequencyExceeded) {
		t.Errorf("second authorize err = %v, want ErrMandateFrequencyExceeded", err)
	}

	// A debit first, then an authorization in the same window fails.
	l3, _ := preauthFixture(t)
	if _, err := l3.DebitWithMandate("m-sub", MandateDebit{ID: "d-1", AmountCents: 1000, Currency: "USD"}); err != nil {
		t.Fatalf("DebitWithMandate: %v", err)
	}
	_, _, err = l3.AuthorizePreAuth(PreAuth{ID: "pa-1", MandateID: "m-sub", AmountCents: 1000, Currency: "USD", ExpiresAt: time.Now().Add(time.Hour)})
	if !errors.Is(err, ErrMandateFrequencyExceeded) {
		t.Errorf("authorize after debit err = %v, want ErrMandateFrequencyExceeded", err)
	}
}

func TestCapturePreAuthHappyPath(t *testing.T) {
	l, _ := preauthFixture(t)
	versionBefore := l.version
	authorizeOne(t, l, "pa-1")
	receipt, err := l.CapturePreAuth("m-sub", PreAuthCapture{ID: "cap-1", PreAuthID: "pa-1"})
	if err != nil {
		t.Fatalf("CapturePreAuth: %v", err)
	}
	if receipt.CaptureID != "cap-1" || receipt.PreAuthID != "pa-1" || receipt.MandateID != "m-sub" {
		t.Errorf("receipt = %+v, want cap-1/pa-1/m-sub", receipt)
	}
	if receipt.AmountCents != 2000 {
		t.Errorf("amount = %d, want 2000", receipt.AmountCents)
	}
	// The debit carries both stamps: preauth_of ahead of mandate_of —
	// mandate_of stays the suffix so the memo parse stays
	// unambiguous.
	wantMemo := "preauth_of:pa-1 mandate_of:m-sub"
	if receipt.Entry.Memo != wantMemo {
		t.Errorf("memo = %q, want %q", receipt.Entry.Memo, wantMemo)
	}
	if mid, ok := mandateIDFromMemo(receipt.Entry.Memo); !ok || mid != "m-sub" {
		t.Errorf("mandateIDFromMemo = %q,%v, want m-sub,true", mid, ok)
	}
	// The capture is a real debit: balances move, the version bumps,
	// the mandate_of link is declared, the debit index grows.
	if got := l.BalanceIn("cust", "USD"); got != 100000-2000 {
		t.Errorf("cust balance = %d, want %d", got, 100000-2000)
	}
	if got := l.BalanceIn("merchant", "USD"); got != 2000 {
		t.Errorf("merchant balance = %d, want 2000", got)
	}
	if l.version != versionBefore+1 {
		t.Errorf("version = %d, want %d (capture journals the debit)", l.version, versionBefore+1)
	}
	links := l.EntryLinks("cap-1")
	if len(links) != 1 || links[0].LinkType != LinkTypeMandateOf || links[0].ToEntryID != "m-sub" {
		t.Errorf("links = %+v, want one mandate_of cap-1 -> m-sub", links)
	}
	page, _, err := l.MandateDebits("m-sub", "", 100)
	if err != nil || len(page) != 1 || page[0].ID != "cap-1" {
		t.Errorf("MandateDebits = %+v, err = %v, want one cap-1", page, err)
	}
	// The pre-authorization is consumed.
	p, ok := l.GetPreAuth("pa-1")
	if !ok || p.Status != PreAuthStatusCaptured {
		t.Errorf("preauth status = %+v,%v, want captured,true", p, ok)
	}
	auth, cap, rel := l.PreAuthCounts()
	if auth != 1 || cap != 1 || rel != 0 {
		t.Errorf("counts = %d/%d/%d, want 1/1/0", auth, cap, rel)
	}
}

func TestCapturePreAuthIdempotentReplay(t *testing.T) {
	l, _ := preauthFixture(t)
	authorizeOne(t, l, "pa-1")
	first, err := l.CapturePreAuth("m-sub", PreAuthCapture{ID: "cap-1", PreAuthID: "pa-1", IdempotencyKey: "cap-key-1"})
	if err != nil {
		t.Fatalf("first capture: %v", err)
	}
	second, err := l.CapturePreAuth("m-sub", PreAuthCapture{ID: "cap-other", PreAuthID: "pa-1", IdempotencyKey: "cap-key-1"})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !second.Duplicate {
		t.Errorf("replay dup = false, want true")
	}
	if second.Entry.ID != first.Entry.ID {
		t.Errorf("replay entry = %q, want %q", second.Entry.ID, first.Entry.ID)
	}
	// Nothing booked twice: exactly one journal entry for the capture.
	page, _, _ := l.MandateDebits("m-sub", "", 100)
	if len(page) != 1 {
		t.Errorf("debits = %d, want 1", len(page))
	}
	_, cap, _ := l.PreAuthCounts()
	if cap != 1 {
		t.Errorf("captured count = %d, want 1", cap)
	}
}

func TestCapturePreAuthStateErrors(t *testing.T) {
	// Unknown pre-authorization.
	l, _ := preauthFixture(t)
	if _, err := l.CapturePreAuth("m-sub", PreAuthCapture{ID: "cap-1", PreAuthID: "nope"}); !errors.Is(err, ErrPreAuthNotFound) {
		t.Errorf("unknown preauth err = %v, want ErrPreAuthNotFound", err)
	}
	// Pre-authorization under a different mandate is not found on this
	// route.
	if _, _, err := l.Post(JournalEntry{ID: "fund-x", DebitAccount: "alt-cust", CreditAccount: "bank", AmountCents: 50000, Currency: "USD"}); err != nil {
		t.Fatalf("fund: %v", err)
	}
	if _, err := l.CreateMandate(Mandate{ID: "m-2", Payer: "alt-cust", Payee: "merchant", MaxAmountCents: 5000, Currency: "USD", Frequency: MandateFrequencyPerDay, ValidFrom: time.Now().Add(-time.Hour), ValidUntil: time.Now().Add(30 * 24 * time.Hour)}); err != nil {
		t.Fatalf("CreateMandate m-2: %v", err)
	}
	authorizeOne(t, l, "pa-1")
	if _, err := l.CapturePreAuth("m-2", PreAuthCapture{ID: "cap-1", PreAuthID: "pa-1"}); !errors.Is(err, ErrPreAuthNotFound) {
		t.Errorf("wrong mandate err = %v, want ErrPreAuthNotFound", err)
	}
	// Double capture is single-shot.
	if _, err := l.CapturePreAuth("m-sub", PreAuthCapture{ID: "cap-1", PreAuthID: "pa-1"}); err != nil {
		t.Fatalf("first capture: %v", err)
	}
	if _, err := l.CapturePreAuth("m-sub", PreAuthCapture{ID: "cap-2", PreAuthID: "pa-1"}); !errors.Is(err, ErrPreAuthNotActive) {
		t.Errorf("second capture err = %v, want ErrPreAuthNotActive", err)
	}
	// Capture ID collides with a journal entry.
	l2, _ := preauthFixture(t)
	authorizeOne(t, l2, "pa-1")
	if _, _, err := l2.Post(JournalEntry{ID: "cap-1", DebitAccount: "merchant", CreditAccount: "cust", AmountCents: 10, Currency: "USD"}); err != nil {
		t.Fatalf("conflicting Post: %v", err)
	}
	if _, err := l2.CapturePreAuth("m-sub", PreAuthCapture{ID: "cap-1", PreAuthID: "pa-1"}); !errors.Is(err, ErrPreAuthCaptureIDConflict) {
		t.Errorf("capture ID conflict err = %v, want ErrPreAuthCaptureIDConflict", err)
	}
	// An authorization born expired cannot be captured — and the
	// failed capture leaves the pre-authorization retryable (still
	// active/expired, nothing booked).
	l3, _ := preauthFixture(t)
	_, _, err := l3.AuthorizePreAuth(PreAuth{ID: "pa-1", MandateID: "m-sub", AmountCents: 2000, Currency: "USD", ExpiresAt: time.Now().Add(-time.Hour)})
	if err != nil {
		t.Fatalf("born-expired authorize: %v", err)
	}
	if _, err := l3.CapturePreAuth("m-sub", PreAuthCapture{ID: "cap-1", PreAuthID: "pa-1"}); !errors.Is(err, ErrPreAuthExpired) {
		t.Errorf("expired capture err = %v, want ErrPreAuthExpired", err)
	}
	page, _, _ := l3.MandateDebits("m-sub", "", 100)
	if len(page) != 0 {
		t.Errorf("debits = %d after failed capture, want 0", len(page))
	}
}

// Cancelling the mandate after the authorization withdraws the pull
// permission: the capture is refused like a card scheme refuses
// settlement after the auth window.
func TestCapturePreAuthMandateCancelled(t *testing.T) {
	l, _ := preauthFixture(t)
	authorizeOne(t, l, "pa-1")
	if _, err := l.CancelMandate("m-sub"); err != nil {
		t.Fatalf("CancelMandate: %v", err)
	}
	if _, err := l.CapturePreAuth("m-sub", PreAuthCapture{ID: "cap-1", PreAuthID: "pa-1"}); !errors.Is(err, ErrMandateNotActive) {
		t.Errorf("capture on cancelled mandate err = %v, want ErrMandateNotActive", err)
	}
}

// The capture runs the full Post validation sequence: a freeze between
// the authorization and the capture refuses the settlement, and a
// closed period refuses a back-dated capture.
func TestCapturePreAuthFullPostSequence(t *testing.T) {
	l, _ := preauthFixture(t)
	authorizeOne(t, l, "pa-1")
	l.Freeze("cust")
	if _, err := l.CapturePreAuth("m-sub", PreAuthCapture{ID: "cap-1", PreAuthID: "pa-1"}); !errors.Is(err, ErrAccountFrozen) {
		t.Errorf("capture on frozen payer err = %v, want ErrAccountFrozen", err)
	}
	// The failed capture did not consume the authorization.
	if p, _ := l.GetPreAuth("pa-1"); p.Status != PreAuthStatusActive {
		t.Errorf("preauth status = %q after failed capture, want active", p.Status)
	}

	l2 := New()
	now := time.Now()
	if _, _, err := l2.Post(JournalEntry{ID: "fund", DebitAccount: "cust", CreditAccount: "bank", AmountCents: 100000, Currency: "USD"}); err != nil {
		t.Fatalf("fund Post: %v", err)
	}
	// The mandate's validity window must cover the back-dated capture
	// timestamp, so the mandate gates pass and the period gate fires.
	if _, err := l2.CreateMandate(Mandate{ID: "m-wide", Payer: "cust", Payee: "merchant", MaxAmountCents: 5000, Currency: "USD", Frequency: MandateFrequencyPerDay, ValidFrom: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), ValidUntil: now.Add(30 * 24 * time.Hour)}); err != nil {
		t.Fatalf("CreateMandate: %v", err)
	}
	if err := l2.ClosePeriod("2025-01"); err != nil {
		t.Fatalf("ClosePeriod: %v", err)
	}
	if _, _, err := l2.AuthorizePreAuth(PreAuth{ID: "pa-1", MandateID: "m-wide", AmountCents: 2000, Currency: "USD", ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatalf("authorize: %v", err)
	}
	backdated := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	if _, err := l2.CapturePreAuth("m-wide", PreAuthCapture{ID: "cap-1", PreAuthID: "pa-1", CreatedAt: backdated}); !errors.Is(err, ErrPeriodClosed) {
		t.Errorf("back-dated capture err = %v, want ErrPeriodClosed", err)
	}
}

func TestReleasePreAuth(t *testing.T) {
	l, _ := preauthFixture(t)
	authorizeOne(t, l, "pa-1")
	p, err := l.ReleasePreAuth("m-sub", "pa-1")
	if err != nil {
		t.Fatalf("ReleasePreAuth: %v", err)
	}
	if p.Status != PreAuthStatusReleased {
		t.Errorf("status = %q, want released", p.Status)
	}
	// Release is idempotent: a second release is a silent no-op.
	p2, err := l.ReleasePreAuth("m-sub", "pa-1")
	if err != nil || p2.Status != PreAuthStatusReleased {
		t.Errorf("second release = %+v, err = %v, want released,nil", p2, err)
	}
	_, _, rel := l.PreAuthCounts()
	if rel != 1 {
		t.Errorf("released count = %d, want 1", rel)
	}
	// A released reservation cannot be captured, and its window is
	// free again.
	if _, err := l.CapturePreAuth("m-sub", PreAuthCapture{ID: "cap-1", PreAuthID: "pa-1"}); !errors.Is(err, ErrPreAuthNotActive) {
		t.Errorf("capture after release err = %v, want ErrPreAuthNotActive", err)
	}
	authorizeOne(t, l, "pa-2")
	// Unknown pre-authorization, and one under another mandate.
	if _, err := l.ReleasePreAuth("m-sub", "nope"); !errors.Is(err, ErrPreAuthNotFound) {
		t.Errorf("release unknown err = %v, want ErrPreAuthNotFound", err)
	}
	if _, err := l.ReleasePreAuth("m-2", "pa-2"); !errors.Is(err, ErrPreAuthNotFound) {
		t.Errorf("release wrong mandate err = %v, want ErrPreAuthNotFound", err)
	}
}

// One mandate per pre-authorization: the per-day frequency gate
// allows exactly one live reservation per window, so sweeping several
// pre-authorizations needs several mandates.
func expireSweepFixture(t *testing.T, now time.Time, ids ...string) *Ledger {
	t.Helper()
	l := New()
	if _, _, err := l.Post(JournalEntry{ID: "fund", DebitAccount: "cust", CreditAccount: "bank", AmountCents: 100000, Currency: "USD"}); err != nil {
		t.Fatalf("fund Post: %v", err)
	}
	for _, id := range ids {
		if _, err := l.CreateMandate(Mandate{ID: "m-" + id, Payer: "cust", Payee: "merchant", MaxAmountCents: 5000, Currency: "USD", Frequency: MandateFrequencyPerDay, ValidFrom: now.Add(-time.Hour), ValidUntil: now.Add(30 * 24 * time.Hour)}); err != nil {
			t.Fatalf("CreateMandate: %v", err)
		}
	}
	return l
}

func TestExpirePreAuthsSweep(t *testing.T) {
	now := time.Now()
	l := expireSweepFixture(t, now, "a", "b", "c")
	base := now
	// pa-a expires in 30m, pa-b in 2h, pa-c already expired at
	// authorization (born expired).
	if _, _, err := l.AuthorizePreAuth(PreAuth{ID: "pa-a", MandateID: "m-a", AmountCents: 1000, Currency: "USD", ExpiresAt: base.Add(30 * time.Minute)}); err != nil {
		t.Fatalf("authorize pa-a: %v", err)
	}
	if _, _, err := l.AuthorizePreAuth(PreAuth{ID: "pa-b", MandateID: "m-b", AmountCents: 1000, Currency: "USD", ExpiresAt: base.Add(2 * time.Hour)}); err != nil {
		t.Fatalf("authorize pa-b: %v", err)
	}
	if _, _, err := l.AuthorizePreAuth(PreAuth{ID: "pa-c", MandateID: "m-c", AmountCents: 1000, Currency: "USD", ExpiresAt: base.Add(-time.Hour)}); err != nil {
		t.Fatalf("authorize pa-c: %v", err)
	}
	// Lazy expiry: pa-c already reads as expired before any sweep.
	if p, _ := l.GetPreAuth("pa-c"); p.Status != PreAuthStatusExpired {
		t.Errorf("pa-c status = %q, want expired (lazy)", p.Status)
	}
	if got := l.ExpirePreAuthsAt(base.Add(time.Hour)); got != 2 {
		t.Errorf("sweep marked %d, want 2 (pa-a and pa-c)", got)
	}
	if got := l.ExpirePreAuthsAt(base.Add(time.Hour)); got != 0 {
		t.Errorf("second sweep marked %d, want 0", got)
	}
	if p, _ := l.GetPreAuth("pa-a"); p.Status != PreAuthStatusExpired {
		t.Errorf("pa-a status = %q, want expired", p.Status)
	}
	if p, _ := l.GetPreAuth("pa-b"); p.Status != PreAuthStatusActive {
		t.Errorf("pa-b status = %q, want active", p.Status)
	}
	// A freed window accepts a fresh authorization.
	if _, _, err := l.AuthorizePreAuth(PreAuth{ID: "pa-d", MandateID: "m-a", AmountCents: 500, Currency: "USD", ExpiresAt: base.Add(2 * time.Hour)}); err != nil {
		t.Errorf("authorize after expiry: %v", err)
	}
	// Captured and released rows are never swept. (m-c's window is
	// free: pa-c expired in the first sweep.)
	if _, _, err := l.AuthorizePreAuth(PreAuth{ID: "pa-e", MandateID: "m-c", AmountCents: 500, Currency: "USD", ExpiresAt: base.Add(30 * time.Minute)}); err != nil {
		t.Fatalf("authorize pa-e: %v", err)
	}
	if _, err := l.ReleasePreAuth("m-c", "pa-e"); err != nil {
		t.Fatalf("release pa-e: %v", err)
	}
	if got := l.ExpirePreAuthsAt(base.Add(3 * time.Hour)); got != 2 {
		t.Errorf("sweep marked %d, want 2 (pa-b and pa-d; released pa-e untouched)", got)
	}
}

func TestGetPreAuthLazyExpiry(t *testing.T) {
	l, _ := preauthFixture(t)
	_, _, err := l.AuthorizePreAuth(PreAuth{ID: "pa-1", MandateID: "m-sub", AmountCents: 1000, Currency: "USD", ExpiresAt: time.Now().Add(-time.Minute)})
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	p, ok := l.GetPreAuth("pa-1")
	if !ok {
		t.Fatalf("GetPreAuth: not found")
	}
	if p.Status != PreAuthStatusExpired {
		t.Errorf("status = %q, want expired (lazy predicate)", p.Status)
	}
	if _, ok := l.GetPreAuth("nope"); ok {
		t.Errorf("GetPreAuth unknown = true, want false")
	}
}

func TestListMandatePreAuths(t *testing.T) {
	l, _ := preauthFixture(t)
	now := time.Now()
	if _, err := l.CreateMandate(Mandate{ID: "m-2", Payer: "cust", Payee: "merchant", MaxAmountCents: 5000, Currency: "USD", Frequency: MandateFrequencyPerDay, ValidFrom: now.Add(-time.Hour), ValidUntil: now.Add(30 * 24 * time.Hour)}); err != nil {
		t.Fatalf("CreateMandate: %v", err)
	}
	authorizeOne(t, l, "pa-b")
	if _, _, err := l.AuthorizePreAuth(PreAuth{ID: "pa-a", MandateID: "m-2", AmountCents: 500, Currency: "USD", ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatalf("authorize pa-a: %v", err)
	}
	list, err := l.ListMandatePreAuths("m-sub")
	if err != nil {
		t.Fatalf("ListMandatePreAuths: %v", err)
	}
	if len(list) != 1 || list[0].ID != "pa-b" {
		t.Errorf("list = %+v, want [pa-b]", list)
	}
	if _, err := l.ListMandatePreAuths("nope"); !errors.Is(err, ErrMandateNotFound) {
		t.Errorf("unknown mandate err = %v, want ErrMandateNotFound", err)
	}
}

func TestPreAuthReconcileReport(t *testing.T) {
	l, _ := preauthFixture(t)
	now := time.Now()
	if _, err := l.CreateMandate(Mandate{ID: "m-2", Payer: "cust", Payee: "merchant", MaxAmountCents: 5000, Currency: "USD", Frequency: MandateFrequencyPerDay, ValidFrom: now.Add(-time.Hour), ValidUntil: now.Add(30 * 24 * time.Hour)}); err != nil {
		t.Fatalf("CreateMandate: %v", err)
	}
	authorizeOne(t, l, "pa-1")
	if _, _, err := l.AuthorizePreAuth(PreAuth{ID: "pa-2", MandateID: "m-2", AmountCents: 1500, Currency: "USD", ExpiresAt: now.Add(-time.Minute)}); err != nil {
		t.Fatalf("authorize pa-2: %v", err)
	}
	if _, err := l.CapturePreAuth("m-sub", PreAuthCapture{ID: "cap-1", PreAuthID: "pa-1"}); err != nil {
		t.Fatalf("capture: %v", err)
	}
	rep := l.Reconcile(now)
	if len(rep.PreAuths) != 2 {
		t.Fatalf("PreAuths = %d, want 2", len(rep.PreAuths))
	}
	// Sorted by ID: pa-1 captured, pa-2 expired (lazy).
	if rep.PreAuths[0].ID != "pa-1" || rep.PreAuths[0].Status != PreAuthStatusCaptured || rep.PreAuths[0].AmountCents != 2000 {
		t.Errorf("row0 = %+v, want pa-1/captured/2000", rep.PreAuths[0])
	}
	if rep.PreAuths[1].ID != "pa-2" || rep.PreAuths[1].Status != PreAuthStatusExpired {
		t.Errorf("row1 = %+v, want pa-2/expired", rep.PreAuths[1])
	}
}

// Pre-authorization idempotency keys expire with the ledger's TTL in
// their own namespace: after the TTL, reposting a key books a
// brand-new authorization.
func TestPreAuthKeyTTLPruning(t *testing.T) {
	l := New(WithIdempotencyTTL(time.Hour))
	now := time.Now()
	if _, _, err := l.Post(JournalEntry{ID: "fund", DebitAccount: "cust", CreditAccount: "bank", AmountCents: 100000, Currency: "USD"}); err != nil {
		t.Fatalf("fund Post: %v", err)
	}
	if _, err := l.CreateMandate(Mandate{ID: "m-sub", Payer: "cust", Payee: "merchant", MaxAmountCents: 5000, Currency: "USD", Frequency: MandateFrequencyPerDay, ValidFrom: now.Add(-3 * time.Hour), ValidUntil: now.Add(30 * 24 * time.Hour)}); err != nil {
		t.Fatalf("CreateMandate: %v", err)
	}
	// The authorization is 2 hours old (and already expired): its key
	// is past the TTL.
	if _, _, err := l.authorizePreAuthLocked(PreAuth{
		ID:             "pa-old",
		MandateID:      "m-sub",
		AmountCents:    1000,
		Currency:       "USD",
		ExpiresAt:      now.Add(-time.Hour),
		CreatedAt:      now.Add(-2 * time.Hour),
		IdempotencyKey: "auth-key-ttl",
	}, now.Add(-2*time.Hour)); err != nil {
		t.Fatalf("authorize: %v", err)
	}
	// Prune: the key index shrinks but the pre-authorization row stays.
	if n := l.pruneIdempotencyKeysLocked(now); n < 1 {
		t.Fatalf("pruned %d keys, want >= 1", n)
	}
	if _, ok := l.GetPreAuth("pa-old"); !ok {
		t.Errorf("pre-authorization row evicted with its key — rows must survive")
	}
	// Reposting the key now books a brand-new authorization (the old
	// one is expired, so the window is free).
	p, dup, err := l.AuthorizePreAuth(PreAuth{
		ID:             "pa-new",
		MandateID:      "m-sub",
		AmountCents:    1000,
		Currency:       "USD",
		ExpiresAt:      now.Add(time.Hour),
		IdempotencyKey: "auth-key-ttl",
	})
	if err != nil || dup || p.ID != "pa-new" {
		t.Errorf("repost after TTL = %+v, dup=%v, err=%v; want new pa-new", p, dup, err)
	}
}
