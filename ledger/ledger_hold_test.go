package ledger

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// fundAccount posts a single balancing entry so tests start from a known
// balance. It mirrors the seed style used across the suite.
func fundAccount(t *testing.T, l *Ledger, account AccountID, cents int64, currency string) {
	t.Helper()
	if _, _, err := l.Post(JournalEntry{
		ID:            "fund-" + string(account) + "-" + currency,
		DebitAccount:  account,
		CreditAccount: "equity",
		AmountCents:   cents,
		Currency:      currency,
		CreatedAt:     time.Now(),
	}); err != nil {
		t.Fatalf("fund %s: %v", account, err)
	}
}

func futureExpiry() time.Time { return time.Now().Add(time.Hour) }

func TestHoldReservesAvailable(t *testing.T) {
	l := New()
	fundAccount(t, l, "card", 10000, "")

	before := l.version
	h, dup, err := l.Hold(Hold{ID: "h1", Account: "card", AmountCents: 6000, ExpiresAt: futureExpiry()})
	if err != nil || dup {
		t.Fatalf("Hold = dup=%v err=%v", dup, err)
	}
	if h.Status != HoldStatusActive {
		t.Errorf("hold status = %q, want active", h.Status)
	}
	if h.Currency != DefaultCurrency {
		t.Errorf("hold currency = %q, want normalized %q", h.Currency, DefaultCurrency)
	}
	// The hold reserves available funds without touching the journaled
	// balance, the version, or the chain.
	if got := l.Balance("card"); got != 10000 {
		t.Errorf("Balance = %d, want 10000 (hold must not move money)", got)
	}
	if got := l.Available("card"); got != 4000 {
		t.Errorf("Available = %d, want 4000", got)
	}
	if l.version != before {
		t.Errorf("version changed by Hold: holds are off-journal")
	}
	if _, links := l.ChainHead(); links != 1 {
		t.Errorf("chain links = %d, want 1 (hold appends no link)", links)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Errorf("accounting equation: %v", err)
	}
}

func TestHoldValidation(t *testing.T) {
	l := New()
	cases := []struct {
		name string
		hold Hold
		want error
	}{
		{"empty ID", Hold{Account: "a", AmountCents: 1, ExpiresAt: futureExpiry()}, ErrEmptyHoldID},
		{"empty account", Hold{ID: "h", AmountCents: 1, ExpiresAt: futureExpiry()}, ErrEmptyHoldAccount},
		{"zero amount", Hold{ID: "h", Account: "a", ExpiresAt: futureExpiry()}, ErrHoldNonPositiveAmount},
		{"negative amount", Hold{ID: "h", Account: "a", AmountCents: -5, ExpiresAt: futureExpiry()}, ErrHoldNonPositiveAmount},
		{"bad currency", Hold{ID: "h", Account: "a", AmountCents: 1, Currency: "usd", ExpiresAt: futureExpiry()}, ErrInvalidCurrency},
		{"missing expiry", Hold{ID: "h", Account: "a", AmountCents: 1}, ErrHoldMissingExpiry},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := l.Hold(tc.hold); !errors.Is(err, tc.want) {
				t.Errorf("Hold err = %v, want %v", err, tc.want)
			}
		})
	}
	if len(l.holds) != 0 {
		t.Errorf("failed holds recorded %d rows, want 0", len(l.holds))
	}
}

func TestHoldInsufficientAvailable(t *testing.T) {
	l := New()
	fundAccount(t, l, "card", 10000, "")

	if _, _, err := l.Hold(Hold{ID: "h1", Account: "card", AmountCents: 6000, ExpiresAt: futureExpiry()}); err != nil {
		t.Fatalf("first Hold: %v", err)
	}
	before := l.version
	// 10000 - 6000 held = 4000 available; 5000 does not fit.
	if _, _, err := l.Hold(Hold{ID: "h2", Account: "card", AmountCents: 5000, ExpiresAt: futureExpiry()}); !errors.Is(err, ErrInsufficientAvailableFunds) {
		t.Errorf("second Hold err = %v, want ErrInsufficientAvailableFunds", err)
	}
	// A rejected hold records nothing.
	if _, ok := l.GetHold("h2"); ok {
		t.Error("rejected hold was recorded")
	}
	if l.version != before {
		t.Error("rejected hold bumped the version")
	}
	// Exactly fitting holds still work.
	if _, _, err := l.Hold(Hold{ID: "h3", Account: "card", AmountCents: 4000, ExpiresAt: futureExpiry()}); err != nil {
		t.Errorf("exact-fit Hold: %v", err)
	}
	if got := l.Available("card"); got != 0 {
		t.Errorf("Available = %d, want 0", got)
	}
}

func TestHoldIdempotentReplay(t *testing.T) {
	l := New()
	fundAccount(t, l, "card", 10000, "")

	first, dup, err := l.Hold(Hold{ID: "h1", Account: "card", AmountCents: 3000, ExpiresAt: futureExpiry(), IdempotencyKey: "hold-key-1"})
	if err != nil || dup {
		t.Fatalf("first Hold = dup=%v err=%v", dup, err)
	}
	// Replay with a different ID but the same key returns the original
	// hold and reserves nothing new.
	second, dup, err := l.Hold(Hold{ID: "h2", Account: "card", AmountCents: 3000, ExpiresAt: futureExpiry(), IdempotencyKey: "hold-key-1"})
	if err != nil || !dup {
		t.Fatalf("replay Hold = dup=%v err=%v, want dup=true", dup, err)
	}
	if second.ID != first.ID {
		t.Errorf("replay returned hold %q, want original %q", second.ID, first.ID)
	}
	if got := l.Available("card"); got != 7000 {
		t.Errorf("Available = %d, want 7000 (replay must reserve nothing)", got)
	}
	if len(l.holds) != 1 {
		t.Errorf("holds recorded = %d, want 1", len(l.holds))
	}
}

func TestHoldNamespacesIndependent(t *testing.T) {
	l := New()
	fundAccount(t, l, "card", 10000, "")

	// The same key string in the entry namespace and the hold namespace
	// refers to two unrelated operations: holds use their own index.
	if _, _, err := l.Post(JournalEntry{ID: "e1", DebitAccount: "cash", CreditAccount: "equity", AmountCents: 50, IdempotencyKey: "shared-key"}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	h, dup, err := l.Hold(Hold{ID: "h1", Account: "card", AmountCents: 100, ExpiresAt: futureExpiry(), IdempotencyKey: "shared-key"})
	if err != nil || dup {
		t.Fatalf("Hold with entry-namespace key = dup=%v err=%v, want fresh hold", dup, err)
	}
	if h.ID != "h1" {
		t.Errorf("hold ID = %q, want h1", h.ID)
	}
	// And a capture key does not collide with a hold key either.
	if _, err := l.Capture(Capture{ID: "c1", HoldID: "h1", To: "merchant", AmountCents: 100, IdempotencyKey: "shared-key"}); err != nil {
		t.Fatalf("Capture with shared key: %v", err)
	}
}

func TestHoldCurrencyIsolation(t *testing.T) {
	l := New()
	fundAccount(t, l, "card", 10000, "EUR")
	fundAccount(t, l, "card", 5000, "")

	if _, _, err := l.Hold(Hold{ID: "h1", Account: "card", AmountCents: 9000, Currency: "EUR", ExpiresAt: futureExpiry()}); err != nil {
		t.Fatalf("EUR Hold: %v", err)
	}
	if got := l.AvailableIn("card", "EUR"); got != 1000 {
		t.Errorf("AvailableIn(EUR) = %d, want 1000", got)
	}
	if got := l.Available("card"); got != 5000 {
		t.Errorf("Available(USD) = %d, want 5000 (EUR hold must not touch USD)", got)
	}
	// A USD hold larger than the USD balance fails even though the EUR
	// balance could cover it: holds never cross currencies.
	if _, _, err := l.Hold(Hold{ID: "h2", Account: "card", AmountCents: 6000, ExpiresAt: futureExpiry()}); !errors.Is(err, ErrInsufficientAvailableFunds) {
		t.Errorf("USD Hold err = %v, want ErrInsufficientAvailableFunds", err)
	}
}

func TestHoldFrozenAccount(t *testing.T) {
	l := New()
	fundAccount(t, l, "card", 10000, "")
	l.Freeze("card")

	if _, _, err := l.Hold(Hold{ID: "h1", Account: "card", AmountCents: 100, ExpiresAt: futureExpiry()}); !errors.Is(err, ErrAccountFrozen) {
		t.Errorf("Hold on frozen account err = %v, want ErrAccountFrozen", err)
	}
	if _, ok := l.GetHold("h1"); ok {
		t.Error("frozen-rejected hold was recorded")
	}

	// A replay of a key placed before the freeze still succeeds: the
	// frozen check runs after the replay check, mirroring Post.
	l2 := New()
	fundAccount(t, l2, "card", 10000, "")
	if _, _, err := l2.Hold(Hold{ID: "h1", Account: "card", AmountCents: 100, ExpiresAt: futureExpiry(), IdempotencyKey: "k"}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	l2.Freeze("card")
	if h, dup, err := l2.Hold(Hold{ID: "h2", Account: "card", AmountCents: 100, ExpiresAt: futureExpiry(), IdempotencyKey: "k"}); err != nil || !dup || h.ID != "h1" {
		t.Errorf("replay after freeze = id=%q dup=%v err=%v, want original hold", h.ID, dup, err)
	}
}

func TestRelease(t *testing.T) {
	l := New()
	fundAccount(t, l, "card", 10000, "")
	if _, _, err := l.Hold(Hold{ID: "h1", Account: "card", AmountCents: 6000, ExpiresAt: futureExpiry()}); err != nil {
		t.Fatalf("Hold: %v", err)
	}

	h, err := l.Release("h1")
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	if h.Status != HoldStatusReleased {
		t.Errorf("status = %q, want released", h.Status)
	}
	if got := l.Available("card"); got != 10000 {
		t.Errorf("Available after release = %d, want 10000", got)
	}
	// Release is idempotent: a second release is a no-op, not an error.
	if h2, err := l.Release("h1"); err != nil || h2.Status != HoldStatusReleased {
		t.Errorf("second Release = status=%q err=%v, want idempotent no-op", h2.Status, err)
	}
	if _, err := l.Release("nope"); !errors.Is(err, ErrHoldNotFound) {
		t.Errorf("Release unknown err = %v, want ErrHoldNotFound", err)
	}
}

func TestReleaseOnFrozenAccount(t *testing.T) {
	l := New()
	fundAccount(t, l, "card", 10000, "")
	if _, _, err := l.Hold(Hold{ID: "h1", Account: "card", AmountCents: 6000, ExpiresAt: futureExpiry()}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	l.Freeze("card")
	// Releasing frees reserved funds instead of moving money, so the
	// freeze must not strand it.
	if h, err := l.Release("h1"); err != nil || h.Status != HoldStatusReleased {
		t.Errorf("Release on frozen account = status=%q err=%v", h.Status, err)
	}
	if got := l.Available("card"); got != 10000 {
		t.Errorf("Available = %d, want 10000", got)
	}
}

func TestCaptureSettlesDoubleEntry(t *testing.T) {
	l := New()
	fundAccount(t, l, "card", 10000, "")
	if _, _, err := l.Hold(Hold{ID: "h1", Account: "card", AmountCents: 10000, ExpiresAt: futureExpiry()}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	before := l.version

	r, err := l.Capture(Capture{ID: "c1", HoldID: "h1", To: "merchant", AmountCents: 6000, CreatedAt: time.Now()})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if r.Duplicate {
		t.Error("first capture reported duplicate")
	}
	if r.CapturedCents != 6000 || r.ReleasedCents != 4000 {
		t.Errorf("receipt = captured %d released %d, want 6000/4000", r.CapturedCents, r.ReleasedCents)
	}
	// The settlement is an ordinary double-entry posting in the hold's
	// currency: debit payee, credit held account.
	e := r.Entry
	if e.DebitAccount != "merchant" || e.CreditAccount != "card" || e.AmountCents != 6000 || e.Currency != DefaultCurrency {
		t.Errorf("capture entry = %+v, want debit merchant / credit card / 6000 USD", e)
	}
	if got := l.Balance("card"); got != 4000 {
		t.Errorf("Balance(card) = %d, want 4000", got)
	}
	if got := l.Balance("merchant"); got != 6000 {
		t.Errorf("Balance(merchant) = %d, want 6000", got)
	}
	// The hold is consumed; the remainder is released implicitly.
	h, _ := l.GetHold("h1")
	if h.Status != HoldStatusCaptured {
		t.Errorf("hold status = %q, want captured", h.Status)
	}
	if got := l.Available("card"); got != 4000 {
		t.Errorf("Available(card) = %d, want 4000", got)
	}
	// Capture journals a real posting: exactly one version bump and one
	// chain link.
	if l.version != before+1 {
		t.Errorf("version = %d, want %d (one bump for the settlement)", l.version, before+1)
	}
	if _, links := l.ChainHead(); links != 2 {
		t.Errorf("chain links = %d, want 2", links)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Errorf("accounting equation: %v", err)
	}
	if err := l.VerifyChain(); err != nil {
		t.Errorf("audit chain: %v", err)
	}
}

func TestCaptureValidation(t *testing.T) {
	l := New()
	fundAccount(t, l, "card", 10000, "")
	if _, _, err := l.Hold(Hold{ID: "h1", Account: "card", AmountCents: 5000, ExpiresAt: futureExpiry()}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	cases := []struct {
		name string
		cap  Capture
		want error
	}{
		{"empty ID", Capture{HoldID: "h1", To: "m", AmountCents: 1}, ErrEmptyCaptureID},
		{"empty hold ID", Capture{ID: "c", To: "m", AmountCents: 1}, ErrEmptyCaptureHoldID},
		{"empty payee", Capture{ID: "c", HoldID: "h1", AmountCents: 1}, ErrEmptyCaptureToAccount},
		{"zero amount", Capture{ID: "c", HoldID: "h1", To: "m"}, ErrCaptureNonPositiveAmount},
		{"unknown hold", Capture{ID: "c", HoldID: "nope", To: "m", AmountCents: 1}, ErrHoldNotFound},
		{"payee == held account", Capture{ID: "c", HoldID: "h1", To: "card", AmountCents: 1}, ErrCaptureSameAccount},
		{"exceeds hold", Capture{ID: "c", HoldID: "h1", To: "m", AmountCents: 5001}, ErrCaptureExceedsHold},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := l.version
			if _, err := l.Capture(tc.cap); !errors.Is(err, tc.want) {
				t.Errorf("Capture err = %v, want %v", err, tc.want)
			}
			if l.version != before {
				t.Error("failed capture bumped the version")
			}
		})
	}
	// The hold survived all the failed captures untouched.
	if h, _ := l.GetHold("h1"); h.Status != HoldStatusActive {
		t.Errorf("hold status = %q, want active", h.Status)
	}
}

func TestCaptureSingleShot(t *testing.T) {
	l := New()
	fundAccount(t, l, "card", 10000, "")
	if _, _, err := l.Hold(Hold{ID: "h1", Account: "card", AmountCents: 5000, ExpiresAt: futureExpiry()}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if _, err := l.Capture(Capture{ID: "c1", HoldID: "h1", To: "m", AmountCents: 2000}); err != nil {
		t.Fatalf("first Capture: %v", err)
	}
	// A second capture on the consumed hold fails, even for a smaller
	// amount: captures are single-shot, the remainder was released.
	if _, err := l.Capture(Capture{ID: "c2", HoldID: "h1", To: "m", AmountCents: 1000}); !errors.Is(err, ErrHoldNotActive) {
		t.Errorf("second Capture err = %v, want ErrHoldNotActive", err)
	}
	if got := l.Available("card"); got != 8000 {
		t.Errorf("Available = %d, want 8000", got)
	}
}

func TestCaptureExpiredHold(t *testing.T) {
	l := New()
	fundAccount(t, l, "card", 10000, "")
	// A hold born with an expiry in the past is accepted but born
	// expired: it reserves nothing (lazy expiry from birth).
	h, _, err := l.Hold(Hold{ID: "h1", Account: "card", AmountCents: 5000, ExpiresAt: time.Now().Add(-time.Second)})
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if h.Status != HoldStatusActive {
		t.Errorf("fresh hold status = %q, want active (sweep not run yet)", h.Status)
	}
	if got := l.Available("card"); got != 10000 {
		t.Errorf("Available = %d, want 10000 (expired hold reserves nothing)", got)
	}
	if _, err := l.Capture(Capture{ID: "c1", HoldID: "h1", To: "m", AmountCents: 100}); !errors.Is(err, ErrHoldExpired) {
		t.Errorf("Capture on expired hold err = %v, want ErrHoldExpired", err)
	}
	// The explicit sweep marks it for observability.
	if n := l.ExpireHolds(); n != 1 {
		t.Errorf("ExpireHolds = %d, want 1", n)
	}
	if h, _ := l.GetHold("h1"); h.Status != HoldStatusExpired {
		t.Errorf("status after sweep = %q, want expired", h.Status)
	}
	if n := l.ExpireHolds(); n != 0 {
		t.Errorf("second ExpireHolds = %d, want 0", n)
	}
}

func TestCaptureIdempotentReplay(t *testing.T) {
	l := New()
	fundAccount(t, l, "card", 10000, "")
	if _, _, err := l.Hold(Hold{ID: "h1", Account: "card", AmountCents: 5000, ExpiresAt: futureExpiry()}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	first, err := l.Capture(Capture{ID: "c1", HoldID: "h1", To: "m", AmountCents: 2000, IdempotencyKey: "cap-key"})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	entriesBefore := len(l.Entries())
	second, err := l.Capture(Capture{ID: "c2", HoldID: "h1", To: "m", AmountCents: 2000, IdempotencyKey: "cap-key"})
	if err != nil {
		t.Fatalf("replay Capture: %v", err)
	}
	if !second.Duplicate {
		t.Error("replay did not report duplicate")
	}
	if second.Entry.ID != first.Entry.ID || second.CapturedCents != first.CapturedCents {
		t.Errorf("replay receipt = %+v, want original %+v", second, first)
	}
	if n := len(l.Entries()); n != entriesBefore {
		t.Errorf("entries = %d, want %d (replay journals nothing)", n, entriesBefore)
	}
}

func TestCaptureFrozen(t *testing.T) {
	l := New()
	fundAccount(t, l, "card", 10000, "")
	if _, _, err := l.Hold(Hold{ID: "h1", Account: "card", AmountCents: 5000, ExpiresAt: futureExpiry()}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	l.Freeze("card")
	if _, err := l.Capture(Capture{ID: "c1", HoldID: "h1", To: "m", AmountCents: 100}); !errors.Is(err, ErrAccountFrozen) {
		t.Errorf("Capture with frozen payer err = %v, want ErrAccountFrozen", err)
	}
	l.Unfreeze("card")
	l.Freeze("m")
	if _, err := l.Capture(Capture{ID: "c1", HoldID: "h1", To: "m", AmountCents: 100}); !errors.Is(err, ErrAccountFrozen) {
		t.Errorf("Capture with frozen payee err = %v, want ErrAccountFrozen", err)
	}
	l.Unfreeze("m")
	// The hold survived the frozen rejections.
	if h, _ := l.GetHold("h1"); h.Status != HoldStatusActive {
		t.Errorf("hold status = %q, want active", h.Status)
	}
}

func TestCaptureOverdraftProtected(t *testing.T) {
	l := New(WithOverdraftProtection("card"))
	fundAccount(t, l, "card", 10000, "")
	if _, _, err := l.Hold(Hold{ID: "h1", Account: "card", AmountCents: 10000, ExpiresAt: futureExpiry()}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	// A concurrent posting drains the balance between the hold and the
	// capture: the reservation is advisory, so the capture runs the
	// ordinary overdraft check and is rejected.
	if _, _, err := l.Post(JournalEntry{ID: "drain", DebitAccount: "other", CreditAccount: "card", AmountCents: 6000}); err != nil {
		t.Fatalf("drain Post: %v", err)
	}
	if _, err := l.Capture(Capture{ID: "c1", HoldID: "h1", To: "m", AmountCents: 10000}); !errors.Is(err, ErrAccountOverdraft) {
		t.Errorf("Capture err = %v, want ErrAccountOverdraft", err)
	}
	// The hold is still active: the failed capture settled nothing.
	if h, _ := l.GetHold("h1"); h.Status != HoldStatusActive {
		t.Errorf("hold status = %q, want active", h.Status)
	}
}

func TestHoldOverdraftProtectedAvailable(t *testing.T) {
	l := New(WithOverdraftProtection("card"))
	fundAccount(t, l, "card", 10000, "")
	// On a protected account the available check is the overdraft guard:
	// the hold cannot exceed the balance, since the balance floor is 0.
	if _, _, err := l.Hold(Hold{ID: "h1", Account: "card", AmountCents: 10001, ExpiresAt: futureExpiry()}); !errors.Is(err, ErrInsufficientAvailableFunds) {
		t.Errorf("oversized Hold err = %v, want ErrInsufficientAvailableFunds", err)
	}
	if _, _, err := l.Hold(Hold{ID: "h1", Account: "card", AmountCents: 10000, ExpiresAt: futureExpiry()}); err != nil {
		t.Errorf("full-balance Hold: %v", err)
	}
}

func TestCaptureIDConflict(t *testing.T) {
	l := New()
	fundAccount(t, l, "card", 10000, "")
	if _, _, err := l.Hold(Hold{ID: "h1", Account: "card", AmountCents: 5000, ExpiresAt: futureExpiry()}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if _, _, err := l.Post(JournalEntry{ID: "c1", DebitAccount: "x", CreditAccount: "y", AmountCents: 1}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if _, err := l.Capture(Capture{ID: "c1", HoldID: "h1", To: "m", AmountCents: 100}); !errors.Is(err, ErrCaptureIDConflict) {
		t.Errorf("Capture err = %v, want ErrCaptureIDConflict", err)
	}
}

func TestHoldIdempotencyTTL(t *testing.T) {
	l := New(WithIdempotencyTTL(time.Hour))
	fundAccount(t, l, "card", 100000, "")
	old := time.Now().Add(-2 * time.Hour)
	if _, _, err := l.Hold(Hold{ID: "h1", Account: "card", AmountCents: 1000, ExpiresAt: futureExpiry(), IdempotencyKey: "ttl-key", CreatedAt: old}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if n := l.ExpireIdempotencyKeys(); n != 1 {
		t.Errorf("ExpireIdempotencyKeys = %d, want 1", n)
	}
	// After the TTL, reposting the key books a brand-new hold — the same
	// contract as the entry-level keys.
	h, dup, err := l.Hold(Hold{ID: "h2", Account: "card", AmountCents: 1000, ExpiresAt: futureExpiry(), IdempotencyKey: "ttl-key"})
	if err != nil || dup {
		t.Fatalf("repost after TTL = dup=%v err=%v, want fresh hold", dup, err)
	}
	if h.ID != "h2" {
		t.Errorf("repost hold ID = %q, want h2", h.ID)
	}
	if got := l.Available("card"); got != 98000 {
		t.Errorf("Available = %d, want 98000 (two live holds)", got)
	}
}

// TestHoldConcurrentRace hammers holds, releases, captures, and available
// reads from many goroutines. Run with -race: any unsynchronized access
// to the hold indexes fails the run. The available-funds invariant is
// checked at the end: serialized under the write lock, concurrent holds
// can never reserve more than the balance.
func TestHoldConcurrentRace(t *testing.T) {
	l := New()
	fundAccount(t, l, "card", 2000, "")

	const workers = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	succeeded := 0
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			h, _, err := l.Hold(Hold{
				ID:          fmt.Sprintf("h-race-%d", w),
				Account:     "card",
				AmountCents: 100,
				ExpiresAt:   futureExpiry(),
			})
			if err != nil {
				if !errors.Is(err, ErrInsufficientAvailableFunds) {
					t.Errorf("worker %d: unexpected Hold err: %v", w, err)
				}
				return
			}
			mu.Lock()
			succeeded++
			mu.Unlock()
			// Interleave reads while other goroutines hold the write
			// lock, so the read path is exercised under contention.
			_ = l.Available("card")
			if _, ok := l.GetHold(h.ID); !ok {
				t.Errorf("worker %d: hold %q vanished", w, h.ID)
			}
		}(w)
	}
	wg.Wait()

	// 2000 cents / 100 per hold = exactly 20 holds fit... but the fund
	// entry and interleaving do not change the balance, so all 20 holds
	// of 100 fit into 2000. Every hold must have succeeded.
	if succeeded != workers {
		t.Errorf("succeeded holds = %d, want %d", succeeded, workers)
	}
	if got := l.Available("card"); got != 0 {
		t.Errorf("Available = %d, want 0 (fully reserved)", got)
	}

	// Concurrent releases return everything.
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			if _, err := l.Release(fmt.Sprintf("h-race-%d", w)); err != nil {
				t.Errorf("worker %d: Release: %v", w, err)
			}
			_ = l.Available("card")
		}(w)
	}
	wg.Wait()
	if got := l.Available("card"); got != 2000 {
		t.Errorf("Available after releases = %d, want 2000", got)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Errorf("accounting equation: %v", err)
	}
}

// TestHoldConcurrentOversubscribe proves the available check is atomic:
// more concurrent hold demand than the balance can cover, and exactly the
// fitting subset succeeds — never a cent more.
func TestHoldConcurrentOversubscribe(t *testing.T) {
	l := New()
	fundAccount(t, l, "card", 1000, "")

	const workers = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	succeeded := 0
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			_, _, err := l.Hold(Hold{
				ID:          fmt.Sprintf("h-over-%d", w),
				Account:     "card",
				AmountCents: 100,
				ExpiresAt:   futureExpiry(),
			})
			if err == nil {
				mu.Lock()
				succeeded++
				mu.Unlock()
			} else if !errors.Is(err, ErrInsufficientAvailableFunds) {
				t.Errorf("worker %d: unexpected Hold err: %v", w, err)
			}
		}(w)
	}
	wg.Wait()
	if succeeded != 10 {
		t.Errorf("succeeded holds = %d, want exactly 10 (1000/100)", succeeded)
	}
	if got := l.Available("card"); got != 0 {
		t.Errorf("Available = %d, want 0", got)
	}
}

// TestCaptureConcurrentRace captures distinct holds from many goroutines
// while readers interleave: every capture journals exactly one balanced
// entry, and the books still balance at the end.
func TestCaptureConcurrentRace(t *testing.T) {
	l := New()
	fundAccount(t, l, "card", 10000, "")

	const workers = 10
	for w := 0; w < workers; w++ {
		if _, _, err := l.Hold(Hold{ID: fmt.Sprintf("h-cap-%d", w), Account: "card", AmountCents: 1000, ExpiresAt: futureExpiry()}); err != nil {
			t.Fatalf("Hold %d: %v", w, err)
		}
	}
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			if _, err := l.Capture(Capture{
				ID:          fmt.Sprintf("c-cap-%d", w),
				HoldID:      fmt.Sprintf("h-cap-%d", w),
				To:          "merchant",
				AmountCents: 600,
			}); err != nil {
				t.Errorf("worker %d: Capture: %v", w, err)
				return
			}
			_ = l.Available("card")
			_ = l.Balance("card")
		}(w)
	}
	wg.Wait()
	if got := l.Balance("card"); got != 4000 {
		t.Errorf("Balance(card) = %d, want 4000", got)
	}
	if got := l.Balance("merchant"); got != 6000 {
		t.Errorf("Balance(merchant) = %d, want 6000", got)
	}
	if got := l.Available("card"); got != 4000 {
		t.Errorf("Available(card) = %d, want 4000 (all holds consumed)", got)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Errorf("accounting equation: %v", err)
	}
	if err := l.VerifyChain(); err != nil {
		t.Errorf("audit chain: %v", err)
	}
}

func TestReconcileHeldTotals(t *testing.T) {
	l := New()
	fundAccount(t, l, "card", 10000, "")
	fundAccount(t, l, "card", 5000, "EUR")
	if _, _, err := l.Hold(Hold{ID: "h1", Account: "card", AmountCents: 3000, ExpiresAt: futureExpiry()}); err != nil {
		t.Fatalf("Hold h1: %v", err)
	}
	if _, _, err := l.Hold(Hold{ID: "h2", Account: "card", AmountCents: 2000, ExpiresAt: futureExpiry()}); err != nil {
		t.Fatalf("Hold h2: %v", err)
	}
	if _, _, err := l.Hold(Hold{ID: "h3", Account: "card", AmountCents: 4000, Currency: "EUR", ExpiresAt: futureExpiry()}); err != nil {
		t.Fatalf("Hold h3: %v", err)
	}
	// Born-expired: counted as inactive in the report even before the
	// sweep.
	if _, _, err := l.Hold(Hold{ID: "h4", Account: "card", AmountCents: 999, ExpiresAt: time.Now().Add(-time.Second)}); err != nil {
		t.Fatalf("Hold h4: %v", err)
	}
	if _, err := l.Release("h2"); err != nil {
		t.Fatalf("Release: %v", err)
	}

	report := l.Reconcile(time.Now())
	if len(report.HeldTotals) != 2 {
		t.Fatalf("held_totals rows = %d, want 2 (USD, EUR)", len(report.HeldTotals))
	}
	// Sorted by currency code: EUR first.
	if row := report.HeldTotals[0]; row.Currency != "EUR" || row.HeldCents != 4000 || row.ActiveHolds != 1 {
		t.Errorf("held_totals[0] = %+v, want {EUR 4000 1}", row)
	}
	if row := report.HeldTotals[1]; row.Currency != "USD" || row.HeldCents != 3000 || row.ActiveHolds != 1 {
		t.Errorf("held_totals[1] = %+v, want {USD 3000 1}", row)
	}
}
