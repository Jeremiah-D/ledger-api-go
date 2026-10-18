package ledger

import (
	"errors"
	"testing"
	"time"
)

// seedDisputeLedger builds a ledger with a funded payer and one posted
// transfer t1 (payer -> payee, 10000 USD): the fixture every dispute
// test disputes against.
func seedDisputeLedger(t *testing.T) *Ledger {
	t.Helper()
	l := New()
	if _, _, err := l.Post(JournalEntry{
		ID:            "fund-payer",
		DebitAccount:  "payer",
		CreditAccount: "equity",
		AmountCents:   20000,
		Currency:      "USD",
		CreatedAt:     time.Now(),
	}); err != nil {
		t.Fatalf("seed Post: %v", err)
	}
	if _, err := l.PostTransfer(Transfer{
		ID:          "t1",
		From:        "payer",
		To:          "payee",
		AmountCents: 10000,
		Currency:    "USD",
	}); err != nil {
		t.Fatalf("seed PostTransfer: %v", err)
	}
	return l
}

func TestRaiseDispute(t *testing.T) {
	l := seedDisputeLedger(t)
	d, dup, err := l.RaiseDispute(Dispute{
		ID:          "d1",
		TransferID:  "t1",
		ReasonCode:  DisputeReasonUnauthorized,
		AmountCents: 6000,
	})
	if err != nil {
		t.Fatalf("RaiseDispute: %v", err)
	}
	if dup {
		t.Error("first raise reports duplicate = true")
	}
	if d.Status != DisputeStatusOpen {
		t.Errorf("status = %q, want open", d.Status)
	}
	if d.HoldID != "dispute/d1" {
		t.Errorf("hold ID = %q, want dispute/d1", d.HoldID)
	}
	if d.Currency != "USD" {
		t.Errorf("currency = %q, want USD (the transfer's)", d.Currency)
	}
	// The payee's available balance drops by the disputed amount while
	// the net balance is untouched: the freeze is off-journal.
	if got := l.AvailableIn("payee", "USD"); got != 4000 {
		t.Errorf("payee available = %d, want 4000", got)
	}
	if got := l.BalanceIn("payee", "USD"); got != 10000 {
		t.Errorf("payee balance = %d, want 10000 (untouched)", got)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Fatalf("books out of balance after raise: %v", err)
	}
}

func TestRaiseDisputeIdempotentReplay(t *testing.T) {
	l := seedDisputeLedger(t)
	orig, _, err := l.RaiseDispute(Dispute{
		ID:          "d1",
		TransferID:  "t1",
		ReasonCode:  DisputeReasonUnauthorized,
		AmountCents: 6000,
	})
	if err != nil {
		t.Fatalf("RaiseDispute: %v", err)
	}
	// Replaying the dispute ID with different terms returns the
	// original and reserves nothing new.
	replay, dup, err := l.RaiseDispute(Dispute{
		ID:          "d1",
		TransferID:  "t1",
		ReasonCode:  DisputeReasonFraud,
		AmountCents: 999,
	})
	if err != nil {
		t.Fatalf("replay RaiseDispute: %v", err)
	}
	if !dup {
		t.Error("replay reports duplicate = false")
	}
	if replay.ReasonCode != orig.ReasonCode || replay.AmountCents != orig.AmountCents {
		t.Errorf("replay = %+v, want the original %+v", replay, orig)
	}
	if got := l.AvailableIn("payee", "USD"); got != 4000 {
		t.Errorf("payee available = %d, want 4000 (no second reservation)", got)
	}
}

func TestRaiseDisputeIdempotencyKey(t *testing.T) {
	l := seedDisputeLedger(t)
	orig, _, err := l.RaiseDispute(Dispute{
		ID:             "d1",
		TransferID:     "t1",
		ReasonCode:     DisputeReasonDuplicate,
		AmountCents:    1000,
		IdempotencyKey: "key-1",
	})
	if err != nil {
		t.Fatalf("RaiseDispute: %v", err)
	}
	// A repeated idempotency key under a new dispute ID returns the
	// dispute the key created.
	replay, dup, err := l.RaiseDispute(Dispute{
		ID:             "d2",
		TransferID:     "t1",
		ReasonCode:     DisputeReasonDuplicate,
		AmountCents:    1000,
		IdempotencyKey: "key-1",
	})
	if err != nil {
		t.Fatalf("replay RaiseDispute: %v", err)
	}
	if !dup || replay.ID != orig.ID {
		t.Errorf("key replay = (%+v, dup=%v), want the original %q", replay, dup, orig.ID)
	}
	if _, ok := l.GetDispute("d2"); ok {
		t.Error("key replay booked a second dispute d2")
	}
}

func TestRaiseDisputeValidation(t *testing.T) {
	l := seedDisputeLedger(t)
	cases := []struct {
		name string
		d    Dispute
		want error
	}{
		{"empty ID", Dispute{TransferID: "t1", ReasonCode: DisputeReasonFraud, AmountCents: 1}, ErrEmptyDisputeID},
		{"empty transfer", Dispute{ID: "d1", ReasonCode: DisputeReasonFraud, AmountCents: 1}, ErrEmptyDisputeTransferID},
		{"zero amount", Dispute{ID: "d1", TransferID: "t1", ReasonCode: DisputeReasonFraud}, ErrDisputeNonPositiveAmount},
		{"bad reason", Dispute{ID: "d1", TransferID: "t1", ReasonCode: "typo", AmountCents: 1}, ErrUnknownDisputeReasonCode},
		{"unknown transfer", Dispute{ID: "d1", TransferID: "nope", ReasonCode: DisputeReasonFraud, AmountCents: 1}, ErrDisputeTransferNotFound},
		{"amount exceeds transfer", Dispute{ID: "d1", TransferID: "t1", ReasonCode: DisputeReasonFraud, AmountCents: 10001}, ErrDisputeAmountExceedsTransfer},
		{"currency mismatch", Dispute{ID: "d1", TransferID: "t1", ReasonCode: DisputeReasonFraud, AmountCents: 1, Currency: "EUR"}, ErrDisputeCurrencyMismatch},
		{"raw post is not a transfer", Dispute{ID: "d1", TransferID: "fund-payer", ReasonCode: DisputeReasonFraud, AmountCents: 1}, ErrDisputeTransferNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := l.RaiseDispute(c.d); !errors.Is(err, c.want) {
				t.Errorf("RaiseDispute err = %v, want %v", err, c.want)
			}
		})
	}
	// A full-amount dispute is allowed (the cap is "at most").
	if _, _, err := l.RaiseDispute(Dispute{ID: "dfull", TransferID: "t1", ReasonCode: DisputeReasonFraud, AmountCents: 10000}); err != nil {
		t.Errorf("full-amount dispute: %v", err)
	}
}

func TestResolveDisputeUphold(t *testing.T) {
	l := seedDisputeLedger(t)
	if _, _, err := l.RaiseDispute(Dispute{ID: "d1", TransferID: "t1", ReasonCode: DisputeReasonNotReceived, AmountCents: 6000}); err != nil {
		t.Fatalf("RaiseDispute: %v", err)
	}
	d, err := l.ResolveDispute("d1", true)
	if err != nil {
		t.Fatalf("ResolveDispute: %v", err)
	}
	if d.Status != DisputeStatusResolved || !d.Uphold {
		t.Errorf("resolved = %+v, want status resolved + upheld", d)
	}
	if d.ResolvedAt.IsZero() {
		t.Error("resolved dispute has no ResolvedAt")
	}
	// The refund nets the disputed cents back to the payer: payer
	// 10000 -> 16000, payee 10000 -> 4000.
	if got := l.BalanceIn("payer", "USD"); got != 16000 {
		t.Errorf("payer balance = %d, want 16000", got)
	}
	if got := l.BalanceIn("payee", "USD"); got != 4000 {
		t.Errorf("payee balance = %d, want 4000", got)
	}
	// The freeze hold is released: the payee's available balance is
	// back to its (post-refund) net balance.
	if got := l.AvailableIn("payee", "USD"); got != 4000 {
		t.Errorf("payee available = %d, want 4000 (hold released)", got)
	}
	h, ok := l.GetHold("dispute/d1")
	if !ok || h.Status != HoldStatusReleased {
		t.Errorf("dispute hold = %+v, want released", h)
	}
	// The refund is a partial reversal of the transfer principal,
	// visible in the journal.
	rev, ok := l.entries["t1/reversal/1"]
	if !ok {
		t.Fatal("no t1/reversal/1 journal entry")
	}
	if rev.AmountCents != 6000 || rev.DebitAccount != "payer" || rev.CreditAccount != "payee" {
		t.Errorf("reversal = %+v, want 6000 debit payer / credit payee", rev)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Fatalf("books out of balance after uphold: %v", err)
	}
}

func TestResolveDisputeReject(t *testing.T) {
	l := seedDisputeLedger(t)
	if _, _, err := l.RaiseDispute(Dispute{ID: "d1", TransferID: "t1", ReasonCode: DisputeReasonDuplicate, AmountCents: 6000}); err != nil {
		t.Fatalf("RaiseDispute: %v", err)
	}
	d, err := l.ResolveDispute("d1", false)
	if err != nil {
		t.Fatalf("ResolveDispute: %v", err)
	}
	if d.Status != DisputeStatusResolved || d.Uphold {
		t.Errorf("resolved = %+v, want status resolved + rejected", d)
	}
	// No money moves: the payee keeps the funds.
	if got := l.BalanceIn("payer", "USD"); got != 10000 {
		t.Errorf("payer balance = %d, want 10000 (unchanged)", got)
	}
	if got := l.BalanceIn("payee", "USD"); got != 10000 {
		t.Errorf("payee balance = %d, want 10000 (unchanged)", got)
	}
	// The freeze lifts: available is back to the full balance.
	if got := l.AvailableIn("payee", "USD"); got != 10000 {
		t.Errorf("payee available = %d, want 10000 (hold released)", got)
	}
	if _, ok := l.entries["t1/reversal/1"]; ok {
		t.Error("reject booked a reversal entry")
	}
}

func TestResolveDisputeTerminal(t *testing.T) {
	l := seedDisputeLedger(t)
	if _, _, err := l.RaiseDispute(Dispute{ID: "d1", TransferID: "t1", ReasonCode: DisputeReasonFraud, AmountCents: 100}); err != nil {
		t.Fatalf("RaiseDispute: %v", err)
	}
	if _, err := l.ResolveDispute("d1", false); err != nil {
		t.Fatalf("ResolveDispute: %v", err)
	}
	if _, err := l.ResolveDispute("d1", true); !errors.Is(err, ErrDisputeAlreadyResolved) {
		t.Errorf("second resolve err = %v, want ErrDisputeAlreadyResolved", err)
	}
	if _, err := l.ResolveDispute("nope", true); !errors.Is(err, ErrDisputeNotFound) {
		t.Errorf("unknown resolve err = %v, want ErrDisputeNotFound", err)
	}
	if _, ok := l.GetDispute("nope"); ok {
		t.Error("GetDispute returns a dispute for an unknown ID")
	}
}

func TestDisputeHoldBlocksPayeeSpending(t *testing.T) {
	l := seedDisputeLedger(t)
	// Dispute the full transfer: the payee's available balance is
	// frozen to zero.
	if _, _, err := l.RaiseDispute(Dispute{ID: "d1", TransferID: "t1", ReasonCode: DisputeReasonUnauthorized, AmountCents: 10000}); err != nil {
		t.Fatalf("RaiseDispute: %v", err)
	}
	if got := l.AvailableIn("payee", "USD"); got != 0 {
		t.Fatalf("payee available = %d, want 0", got)
	}
	// The payee cannot reserve even one more cent while the dispute
	// is open: the hold consumes available funds exactly like an
	// authorization hold.
	_, _, err := l.Hold(Hold{
		ID:          "h2",
		Account:     "payee",
		AmountCents: 1,
		Currency:    "USD",
		ExpiresAt:   time.Now().Add(time.Hour),
	})
	if !errors.Is(err, ErrInsufficientAvailableFunds) {
		t.Errorf("payee hold err = %v, want ErrInsufficientAvailableFunds", err)
	}
	// Resolving the dispute lifts the freeze: the payee can reserve
	// again.
	if _, err := l.ResolveDispute("d1", false); err != nil {
		t.Fatalf("ResolveDispute: %v", err)
	}
	if _, _, err := l.Hold(Hold{
		ID:          "h3",
		Account:     "payee",
		AmountCents: 10000,
		Currency:    "USD",
		ExpiresAt:   time.Now().Add(time.Hour),
	}); err != nil {
		t.Errorf("post-resolution hold: %v", err)
	}
}

func TestRaiseDisputeInsufficientPayeeFunds(t *testing.T) {
	l := seedDisputeLedger(t)
	// The payee spends the transfer away first: there is nothing left
	// to freeze, so the raise is a 422-class rejection.
	if _, err := l.PostTransfer(Transfer{ID: "t2", From: "payee", To: "payer", AmountCents: 10000, Currency: "USD"}); err != nil {
		t.Fatalf("payee spend: %v", err)
	}
	if _, _, err := l.RaiseDispute(Dispute{ID: "d1", TransferID: "t1", ReasonCode: DisputeReasonFraud, AmountCents: 100}); !errors.Is(err, ErrInsufficientAvailableFunds) {
		t.Errorf("RaiseDispute err = %v, want ErrInsufficientAvailableFunds", err)
	}
	// The failed raise recorded nothing.
	if _, ok := l.GetDispute("d1"); ok {
		t.Error("failed raise left a dispute record")
	}
	if _, ok := l.GetHold("dispute/d1"); ok {
		t.Error("failed raise left a hold record")
	}
}

func TestResolveDisputePartialTwice(t *testing.T) {
	l := seedDisputeLedger(t)
	// Two partial disputes on one transfer both uphold: each refunds
	// its own slice through its own partial reversal.
	if _, _, err := l.RaiseDispute(Dispute{ID: "d1", TransferID: "t1", ReasonCode: DisputeReasonDuplicate, AmountCents: 6000}); err != nil {
		t.Fatalf("raise d1: %v", err)
	}
	if _, _, err := l.RaiseDispute(Dispute{ID: "d2", TransferID: "t1", ReasonCode: DisputeReasonFraud, AmountCents: 4000}); err != nil {
		t.Fatalf("raise d2: %v", err)
	}
	if _, err := l.ResolveDispute("d1", true); err != nil {
		t.Fatalf("resolve d1: %v", err)
	}
	if _, err := l.ResolveDispute("d2", true); err != nil {
		t.Fatalf("resolve d2: %v", err)
	}
	if got := l.BalanceIn("payer", "USD"); got != 20000 {
		t.Errorf("payer balance = %d, want 20000 (full refund)", got)
	}
	if got := l.BalanceIn("payee", "USD"); got != 0 {
		t.Errorf("payee balance = %d, want 0", got)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Fatalf("books out of balance: %v", err)
	}
}
