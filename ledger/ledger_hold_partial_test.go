package ledger

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCapturePartialValidation(t *testing.T) {
	l := New()
	fundAccount(t, l, "card", 10000, "")
	if _, _, err := l.Hold(Hold{ID: "h1", Account: "card", AmountCents: 1000, ExpiresAt: futureExpiry()}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	cases := []struct {
		name string
		pc   PartialCapture
		want error
	}{
		{"empty hold ID", PartialCapture{To: "m", CaptureSeq: 1, AmountCents: 1}, ErrEmptyHoldID},
		{"empty payee", PartialCapture{HoldID: "h1", CaptureSeq: 1, AmountCents: 1}, ErrEmptyCaptureToAccount},
		{"zero seq", PartialCapture{HoldID: "h1", To: "m", AmountCents: 1}, ErrInvalidCaptureSeq},
		{"negative seq", PartialCapture{HoldID: "h1", To: "m", CaptureSeq: -2, AmountCents: 1}, ErrInvalidCaptureSeq},
		{"zero amount", PartialCapture{HoldID: "h1", To: "m", CaptureSeq: 1}, ErrCaptureNonPositiveAmount},
		{"negative amount", PartialCapture{HoldID: "h1", To: "m", CaptureSeq: 1, AmountCents: -5}, ErrCaptureNonPositiveAmount},
		{"unknown hold", PartialCapture{HoldID: "nope", To: "m", CaptureSeq: 1, AmountCents: 1}, ErrHoldNotFound},
		{"same account", PartialCapture{HoldID: "h1", To: "card", CaptureSeq: 1, AmountCents: 1}, ErrCaptureSameAccount},
		{"exceeds hold", PartialCapture{HoldID: "h1", To: "m", CaptureSeq: 1, AmountCents: 1001}, ErrCaptureExceedsHold},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := l.CapturePartial(tc.pc); !errors.Is(err, tc.want) {
				t.Errorf("CapturePartial err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestCapturePartialBasic(t *testing.T) {
	l := New()
	fundAccount(t, l, "card", 10000, "")
	if _, _, err := l.Hold(Hold{ID: "h1", Account: "card", AmountCents: 1000, ExpiresAt: futureExpiry()}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	before := l.version
	r, err := l.CapturePartial(PartialCapture{HoldID: "h1", To: "merchant", CaptureSeq: 1, AmountCents: 300})
	if err != nil {
		t.Fatalf("CapturePartial: %v", err)
	}
	if r.Duplicate {
		t.Errorf("first partial: Duplicate=true, want false")
	}
	if r.CaptureID != "h1/capture/1" {
		t.Errorf("CaptureID = %q, want h1/capture/1", r.CaptureID)
	}
	if r.Entry.ID != "h1/capture/1" {
		t.Errorf("entry ID = %q, want the deterministic h1/capture/1", r.Entry.ID)
	}
	if r.CapturedCents != 300 || r.TotalCapturedCents != 300 || r.RemainingCents != 700 {
		t.Errorf("receipt = captured %d total %d remaining %d, want 300/300/700",
			r.CapturedCents, r.TotalCapturedCents, r.RemainingCents)
	}
	if r.Entry.DebitAccount != "merchant" || r.Entry.CreditAccount != "card" {
		t.Errorf("entry legs = debit %q credit %q, want debit merchant credit card",
			r.Entry.DebitAccount, r.Entry.CreditAccount)
	}
	if r.Entry.AmountCents != 300 || r.Entry.Currency != DefaultCurrency {
		t.Errorf("entry = %+v, want 300 cents in the default currency", r.Entry)
	}
	// The hold stays active: its remaining 700 still reserves funds.
	h, ok := l.GetHold("h1")
	if !ok || h.Status != HoldStatusActive {
		t.Fatalf("hold = %+v, want active", h)
	}
	if h.CapturedCents != 300 {
		t.Errorf("hold CapturedCents = %d, want 300", h.CapturedCents)
	}
	// Money moved: card balance 9700, and available still 9000 — the
	// 300 left through the journal, the 700 is still reserved.
	if got := l.Balance("card"); got != 9700 {
		t.Errorf("Balance(card) = %d, want 9700", got)
	}
	if got := l.Available("card"); got != 9000 {
		t.Errorf("Available(card) = %d, want 9000", got)
	}
	if l.version != before+1 {
		t.Errorf("version advanced by %d, want exactly 1 (one journal row)", l.version-before)
	}
	if _, links := l.ChainHead(); links != 2 {
		t.Errorf("chain links = %d, want 2 (funding + capture)", links)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Errorf("accounting equation: %v", err)
	}
}

func TestCapturePartialCumulativeFill(t *testing.T) {
	l := New()
	fundAccount(t, l, "card", 10000, "")
	if _, _, err := l.Hold(Hold{ID: "h1", Account: "card", AmountCents: 1000, ExpiresAt: futureExpiry()}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if _, err := l.CapturePartial(PartialCapture{HoldID: "h1", To: "m", CaptureSeq: 1, AmountCents: 300}); err != nil {
		t.Fatalf("seq 1: %v", err)
	}
	// A second partial that would overshoot the hold is rejected: the
	// bound is the remaining 700, not the original 1000.
	if _, err := l.CapturePartial(PartialCapture{HoldID: "h1", To: "m", CaptureSeq: 2, AmountCents: 701}); !errors.Is(err, ErrCaptureExceedsHold) {
		t.Errorf("overshoot: err=%v, want ErrCaptureExceedsHold", err)
	}
	r, err := l.CapturePartial(PartialCapture{HoldID: "h1", To: "m", CaptureSeq: 2, AmountCents: 700})
	if err != nil {
		t.Fatalf("seq 2: %v", err)
	}
	if r.TotalCapturedCents != 1000 || r.RemainingCents != 0 {
		t.Errorf("receipt total=%d remaining=%d, want 1000/0", r.TotalCapturedCents, r.RemainingCents)
	}
	// The cumulative capture reached the original hold: the hold is
	// consumed, like a full capture.
	h, _ := l.GetHold("h1")
	if h.Status != HoldStatusCaptured {
		t.Errorf("hold status = %q, want captured", h.Status)
	}
	if got := l.Available("card"); got != 9000 {
		t.Errorf("Available(card) = %d, want 9000 (nothing reserved anymore)", got)
	}
	// No further partial captures on a consumed hold.
	if _, err := l.CapturePartial(PartialCapture{HoldID: "h1", To: "m", CaptureSeq: 3, AmountCents: 1}); !errors.Is(err, ErrHoldNotActive) {
		t.Errorf("post-fill: err=%v, want ErrHoldNotActive", err)
	}
}

func TestCapturePartialReplay(t *testing.T) {
	l := New()
	fundAccount(t, l, "card", 10000, "")
	if _, _, err := l.Hold(Hold{ID: "h1", Account: "card", AmountCents: 1000, ExpiresAt: futureExpiry()}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	r1, err := l.CapturePartial(PartialCapture{HoldID: "h1", To: "m", CaptureSeq: 1, AmountCents: 300})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	versionAfterFirst := l.version
	// The same (holdID, captureSeq) replays the original receipt —
	// even with a different amount, which is a replay, not a new
	// capture.
	r2, err := l.CapturePartial(PartialCapture{HoldID: "h1", To: "m", CaptureSeq: 1, AmountCents: 999})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !r2.Duplicate {
		t.Errorf("replay: Duplicate=false, want true")
	}
	if r2.Entry.ID != r1.Entry.ID || r2.CapturedCents != 300 {
		t.Errorf("replay receipt = %+v, want the original 300-cent receipt", r2)
	}
	if l.version != versionAfterFirst {
		t.Errorf("replay bumped the version: partial captures must not double-book")
	}
	if got := l.Balance("card"); got != 9700 {
		t.Errorf("Balance(card) = %d, want 9700 (no second posting)", got)
	}
}

func TestCapturePartialSparseSeqs(t *testing.T) {
	l := New()
	fundAccount(t, l, "card", 10000, "")
	if _, _, err := l.Hold(Hold{ID: "h1", Account: "card", AmountCents: 1000, ExpiresAt: futureExpiry()}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	// Sequences are caller-allocated and need not be consecutive.
	for _, seq := range []int{5, 1, 9} {
		if _, err := l.CapturePartial(PartialCapture{HoldID: "h1", To: "m", CaptureSeq: seq, AmountCents: 100}); err != nil {
			t.Fatalf("seq %d: %v", seq, err)
		}
	}
	h, _ := l.GetHold("h1")
	if h.CapturedCents != 300 || h.Status != HoldStatusActive {
		t.Errorf("hold = captured %d status %q, want 300/active", h.CapturedCents, h.Status)
	}
}

func TestCapturePartialTerminalStates(t *testing.T) {
	l := New()
	fundAccount(t, l, "card", 10000, "")
	fundAccount(t, l, "card2", 10000, "")
	fundAccount(t, l, "card3", 10000, "")
	// Released hold.
	if _, _, err := l.Hold(Hold{ID: "rel", Account: "card", AmountCents: 500, ExpiresAt: futureExpiry()}); err != nil {
		t.Fatalf("Hold rel: %v", err)
	}
	if _, err := l.Release("rel"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := l.CapturePartial(PartialCapture{HoldID: "rel", To: "m", CaptureSeq: 1, AmountCents: 100}); !errors.Is(err, ErrHoldNotActive) {
		t.Errorf("released hold: err=%v, want ErrHoldNotActive", err)
	}
	// Expired hold.
	if _, _, err := l.Hold(Hold{ID: "exp", Account: "card2", AmountCents: 500, ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatalf("Hold exp: %v", err)
	}
	if _, err := l.CapturePartial(PartialCapture{HoldID: "exp", To: "m", CaptureSeq: 1, AmountCents: 100}); !errors.Is(err, ErrHoldExpired) {
		t.Errorf("expired hold: err=%v, want ErrHoldExpired", err)
	}
	// Full-captured hold.
	if _, _, err := l.Hold(Hold{ID: "cap", Account: "card3", AmountCents: 500, ExpiresAt: futureExpiry()}); err != nil {
		t.Fatalf("Hold cap: %v", err)
	}
	if _, err := l.Capture(Capture{ID: "cap-full", HoldID: "cap", To: "m", AmountCents: 500}); err != nil {
		t.Fatalf("full Capture: %v", err)
	}
	if _, err := l.CapturePartial(PartialCapture{HoldID: "cap", To: "m", CaptureSeq: 1, AmountCents: 100}); !errors.Is(err, ErrHoldNotActive) {
		t.Errorf("captured hold: err=%v, want ErrHoldNotActive", err)
	}
}

func TestCapturePartialEntryIDConflict(t *testing.T) {
	l := New()
	fundAccount(t, l, "card", 10000, "")
	if _, _, err := l.Hold(Hold{ID: "h1", Account: "card", AmountCents: 1000, ExpiresAt: futureExpiry()}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	// A hand-posted journal row squatting the deterministic partial
	// entry ID blocks the partial capture: the journal rows must not
	// collide.
	if _, _, err := l.Post(JournalEntry{ID: "h1/capture/1", DebitAccount: "x", CreditAccount: "y", AmountCents: 1}); err != nil {
		t.Fatalf("squat Post: %v", err)
	}
	if _, err := l.CapturePartial(PartialCapture{HoldID: "h1", To: "m", CaptureSeq: 1, AmountCents: 100}); !errors.Is(err, ErrCaptureIDConflict) {
		t.Errorf("conflict: err=%v, want ErrCaptureIDConflict", err)
	}
}

func TestCapturePartialThenFullCapture(t *testing.T) {
	l := New()
	fundAccount(t, l, "card", 10000, "")
	if _, _, err := l.Hold(Hold{ID: "h1", Account: "card", AmountCents: 1000, ExpiresAt: futureExpiry()}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if _, err := l.CapturePartial(PartialCapture{HoldID: "h1", To: "m", CaptureSeq: 1, AmountCents: 300}); err != nil {
		t.Fatalf("partial: %v", err)
	}
	// A full capture after partials is bounded by the remaining 700 —
	// the old "amount <= original hold" bound would have let it settle
	// 1000 and double-book 300.
	if _, err := l.Capture(Capture{ID: "full-after", HoldID: "h1", To: "m", AmountCents: 701}); !errors.Is(err, ErrCaptureExceedsHold) {
		t.Errorf("full capture overshoot: err=%v, want ErrCaptureExceedsHold", err)
	}
	r, err := l.Capture(Capture{ID: "full-after", HoldID: "h1", To: "m", AmountCents: 700})
	if err != nil {
		t.Fatalf("full capture of the remainder: %v", err)
	}
	if r.ReleasedCents != 0 {
		t.Errorf("ReleasedCents = %d, want 0 (nothing left to release)", r.ReleasedCents)
	}
	h, _ := l.GetHold("h1")
	if h.Status != HoldStatusCaptured || h.CapturedCents != 300 {
		t.Errorf("hold = status %q captured %d, want captured/300 (full capture does not rewrite partial history)",
			h.Status, h.CapturedCents)
	}
	if got := l.Balance("card"); got != 9000 {
		t.Errorf("Balance(card) = %d, want 9000 (300 + 700 settled)", got)
	}
}

func TestCapturePartialFrozen(t *testing.T) {
	l := New()
	fundAccount(t, l, "card", 10000, "")
	if _, _, err := l.Hold(Hold{ID: "h1", Account: "card", AmountCents: 1000, ExpiresAt: futureExpiry()}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	// A frozen account cannot settle, and a replay of the same seq
	// still returns the original receipt (replay precedes the freeze
	// check, like Capture).
	if _, err := l.CapturePartial(PartialCapture{HoldID: "h1", To: "m", CaptureSeq: 1, AmountCents: 300}); err != nil {
		t.Fatalf("partial before freeze: %v", err)
	}
	l.Freeze("card")
	if _, err := l.CapturePartial(PartialCapture{HoldID: "h1", To: "m", CaptureSeq: 1, AmountCents: 300}); err != nil {
		t.Errorf("replay on frozen account: %v, want the original receipt", err)
	}
	if _, err := l.CapturePartial(PartialCapture{HoldID: "h1", To: "m", CaptureSeq: 2, AmountCents: 100}); !errors.Is(err, ErrAccountFrozen) {
		t.Errorf("new partial on frozen account: err=%v, want ErrAccountFrozen", err)
	}
}

func TestCapturePartialOverdraftAdvisoryGap(t *testing.T) {
	l := New()
	fundAccount(t, l, "card", 1000, "")
	l.EnableOverdraftProtection("card")
	if _, _, err := l.Hold(Hold{ID: "h1", Account: "card", AmountCents: 800, ExpiresAt: futureExpiry()}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	// A concurrent posting moves the balance between the hold and the
	// capture: the hold is advisory, not a lock. The partial capture
	// runs the same overdraft check as Post and is rejected.
	if _, _, err := l.Post(JournalEntry{ID: "concurrent", DebitAccount: "rent", CreditAccount: "card", AmountCents: 500, Currency: ""}); err != nil {
		t.Fatalf("concurrent Post: %v", err)
	}
	if _, err := l.CapturePartial(PartialCapture{HoldID: "h1", To: "m", CaptureSeq: 1, AmountCents: 800}); !errors.Is(err, ErrAccountOverdraft) {
		t.Errorf("overdraft capture: err=%v, want ErrAccountOverdraft", err)
	}
	// A smaller partial that fits the balance still settles.
	if _, err := l.CapturePartial(PartialCapture{HoldID: "h1", To: "m", CaptureSeq: 1, AmountCents: 500}); err != nil {
		t.Errorf("fitting partial: %v", err)
	}
}

func TestCapturePartialConcurrent(t *testing.T) {
	l := New()
	fundAccount(t, l, "card", 100000, "")
	if _, _, err := l.Hold(Hold{ID: "h1", Account: "card", AmountCents: 10000, ExpiresAt: futureExpiry()}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	// Ten concurrent partial captures with distinct seqs: the write
	// lock serializes the check-and-commit, so the cumulative capture
	// is exactly the sum and no leg is lost or double-counted.
	var wg sync.WaitGroup
	errs := make([]error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := l.CapturePartial(PartialCapture{
				HoldID: "h1", To: "m", CaptureSeq: i + 1, AmountCents: 1000,
			})
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	h, _ := l.GetHold("h1")
	if h.CapturedCents != 10000 || h.Status != HoldStatusCaptured {
		t.Errorf("hold = captured %d status %q, want 10000/captured", h.CapturedCents, h.Status)
	}
	if got := l.Balance("card"); got != 90000 {
		t.Errorf("Balance(card) = %d, want 90000", got)
	}
}

func TestCapturePartialReconcileReport(t *testing.T) {
	l := New()
	fundAccount(t, l, "card", 10000, "")
	fundAccount(t, l, "card2", 5000, "")
	if _, _, err := l.Hold(Hold{ID: "h1", Account: "card", AmountCents: 1000, ExpiresAt: futureExpiry()}); err != nil {
		t.Fatalf("Hold h1: %v", err)
	}
	if _, _, err := l.Hold(Hold{ID: "h2", Account: "card2", AmountCents: 500, ExpiresAt: futureExpiry()}); err != nil {
		t.Fatalf("Hold h2: %v", err)
	}
	if _, err := l.CapturePartial(PartialCapture{HoldID: "h1", To: "m", CaptureSeq: 1, AmountCents: 300}); err != nil {
		t.Fatalf("partial 1: %v", err)
	}
	if _, err := l.CapturePartial(PartialCapture{HoldID: "h1", To: "m", CaptureSeq: 2, AmountCents: 200}); err != nil {
		t.Fatalf("partial 2: %v", err)
	}
	report := l.Reconcile(time.Now())
	if len(report.PartialCaptures) != 1 {
		t.Fatalf("partial_captures = %d rows, want 1 (only h1)", len(report.PartialCaptures))
	}
	row := report.PartialCaptures[0]
	if row.HoldID != "h1" || row.Account != "card" || row.Currency != DefaultCurrency {
		t.Errorf("row identity = %+v, want h1/card/%s", row, DefaultCurrency)
	}
	if row.HeldCents != 1000 || row.CapturedCents != 500 || row.RemainingCents != 500 {
		t.Errorf("row = held %d captured %d remaining %d, want 1000/500/500",
			row.HeldCents, row.CapturedCents, row.RemainingCents)
	}
	if row.Captures != 2 || row.Status != HoldStatusActive {
		t.Errorf("row = captures %d status %q, want 2/active", row.Captures, row.Status)
	}
	// The held totals count only the still-reserved remainder.
	found := false
	for _, ht := range report.HeldTotals {
		if ht.Currency == DefaultCurrency {
			found = true
			if ht.HeldCents != 1000 {
				t.Errorf("held_totals = %d, want 1000 (500 remaining on h1 + 500 on h2)", ht.HeldCents)
			}
		}
	}
	if !found {
		t.Errorf("no held_totals row for %s", DefaultCurrency)
	}
}

func TestCapturePartialSnapshotRoundTrip(t *testing.T) {
	l := New()
	fundAccount(t, l, "card", 10000, "")
	if _, _, err := l.Hold(Hold{ID: "h1", Account: "card", AmountCents: 1000, ExpiresAt: futureExpiry()}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if _, err := l.CapturePartial(PartialCapture{HoldID: "h1", To: "m", CaptureSeq: 1, AmountCents: 300}); err != nil {
		t.Fatalf("partial: %v", err)
	}
	var buf strings.Builder
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	l2, err := ImportSnapshot(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if !snapshotLedgersEqual(l, l2) {
		t.Errorf("restored ledger not equal to the original")
	}
	// The restored ledger replays the partial capture instead of
	// double-booking it.
	r, err := l2.CapturePartial(PartialCapture{HoldID: "h1", To: "m", CaptureSeq: 1, AmountCents: 300})
	if err != nil || !r.Duplicate {
		t.Errorf("replay after restore: err=%v dup=%v, want a replay", err, r.Duplicate)
	}
	// And it can continue the sequence.
	if _, err := l2.CapturePartial(PartialCapture{HoldID: "h1", To: "m", CaptureSeq: 2, AmountCents: 700}); err != nil {
		t.Errorf("continue after restore: %v", err)
	}
	h, _ := l2.GetHold("h1")
	if h.CapturedCents != 1000 || h.Status != HoldStatusCaptured {
		t.Errorf("hold after restore+fill = captured %d status %q, want 1000/captured",
			h.CapturedCents, h.Status)
	}
}

func TestCapturePartialAuditTrail(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	l := New(WithAuditLog(al))
	fundAccount(t, l, "card", 10000, "")
	if _, _, err := l.Hold(Hold{ID: "h1", Account: "card", AmountCents: 1000, ExpiresAt: futureExpiry()}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if _, err := l.CapturePartial(PartialCapture{HoldID: "h1", To: "m", CaptureSeq: 1, AmountCents: 300}); err != nil {
		t.Fatalf("partial: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatal(err)
	}
	events, corrupt, err := ReadAuditLog(dir, time.Now().UTC().Format("2006-01-02"))
	if err != nil || corrupt != 0 {
		t.Fatalf("ReadAuditLog: %v corrupt=%d", err, corrupt)
	}
	found := 0
	for _, e := range events {
		if e.Op == "hold_capture_partial" {
			found++
			if e.Details["capture_seq"] != float64(1) {
				t.Errorf("audit capture_seq = %v, want 1", e.Details["capture_seq"])
			}
			if e.Details["captured_cents"] != float64(300) {
				t.Errorf("audit captured_cents = %v, want 300", e.Details["captured_cents"])
			}
		}
	}
	if found != 1 {
		t.Errorf("hold_capture_partial events = %d, want 1", found)
	}
}

func TestCapturePartialExternalRefAndMetadata(t *testing.T) {
	l := New()
	fundAccount(t, l, "card", 10000, "")
	if _, _, err := l.Hold(Hold{ID: "h1", Account: "card", AmountCents: 1000, ExpiresAt: futureExpiry()}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	r, err := l.CapturePartial(PartialCapture{
		HoldID: "h1", To: "m", CaptureSeq: 1, AmountCents: 300,
		ExternalRef: "order-42/leg-1",
		Metadata:    map[string]string{"channel": "web"},
	})
	if err != nil {
		t.Fatalf("partial: %v", err)
	}
	if r.Entry.ExternalRef != "order-42/leg-1" {
		t.Errorf("entry external_ref = %q, want order-42/leg-1", r.Entry.ExternalRef)
	}
	if r.Entry.Metadata["channel"] != "web" {
		t.Errorf("entry metadata = %+v, want channel=web", r.Entry.Metadata)
	}
	// The reference is unique across the journal, like in Capture.
	if _, err := l.CapturePartial(PartialCapture{
		HoldID: "h1", To: "m", CaptureSeq: 2, AmountCents: 100, ExternalRef: "order-42/leg-1",
	}); !errors.Is(err, ErrDuplicateExternalRef) {
		t.Errorf("duplicate external ref: err=%v, want ErrDuplicateExternalRef", err)
	}
}

func TestCapturePartialIdempotencyTTLExpiry(t *testing.T) {
	l := New(WithIdempotencyTTL(time.Millisecond))
	fundAccount(t, l, "card", 10000, "")
	if _, _, err := l.Hold(Hold{ID: "h1", Account: "card", AmountCents: 1000, ExpiresAt: futureExpiry()}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if _, err := l.CapturePartial(PartialCapture{HoldID: "h1", To: "m", CaptureSeq: 1, AmountCents: 300}); err != nil {
		t.Fatalf("partial: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	if n := l.ExpireIdempotencyKeys(); n < 1 {
		t.Fatalf("ExpireIdempotencyKeys = %d, want >= 1", n)
	}
	// After the TTL the idempotency key is forgotten, but the
	// deterministic entry ID is permanent: re-sending the same
	// (holdID, seq) cannot double-book — it is rejected as a journal
	// ID conflict instead of replaying or re-booking.
	if _, err := l.CapturePartial(PartialCapture{HoldID: "h1", To: "m", CaptureSeq: 1, AmountCents: 100}); !errors.Is(err, ErrCaptureIDConflict) {
		t.Errorf("post-TTL re-send: err=%v, want ErrCaptureIDConflict", err)
	}
	// A new sequence still books fine after the prune.
	r, err := l.CapturePartial(PartialCapture{HoldID: "h1", To: "m", CaptureSeq: 2, AmountCents: 100})
	if err != nil {
		t.Fatalf("post-TTL new seq: %v", err)
	}
	if r.Duplicate || r.CapturedCents != 100 {
		t.Errorf("post-TTL new seq: dup=%v amount=%d, want false/100", r.Duplicate, r.CapturedCents)
	}
	h, _ := l.GetHold("h1")
	if h.CapturedCents != 400 {
		t.Errorf("hold CapturedCents = %d, want 400 (300 + 100)", h.CapturedCents)
	}
}
