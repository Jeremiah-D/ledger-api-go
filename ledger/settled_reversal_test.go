package ledger

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

// seedSettledLedger funds a payer and posts one settled same-currency
// transfer t1 (payer -> payee, 10000 USD): the fixture every settled
// reversal test reverses against.
func seedSettledLedger(t *testing.T) *Ledger {
	t.Helper()
	l := New()
	seedBalance(t, l, "payer", 20000)
	if _, err := l.PostTransfer(Transfer{
		ID: "t1", From: "payer", To: "payee", AmountCents: 10000, Currency: "USD",
	}); err != nil {
		t.Fatalf("seed PostTransfer: %v", err)
	}
	return l
}

func TestReverseSettledTransfer(t *testing.T) {
	l := seedSettledLedger(t)

	reversal, dup, err := l.ReverseSettledTransfer("t1", 4000, "duplicate payment", "sr-key-1")
	if err != nil {
		t.Fatalf("ReverseSettledTransfer: %v", err)
	}
	if dup {
		t.Error("first reversal reports duplicate = true")
	}
	if reversal.ID != "t1/reversal/1" {
		t.Errorf("reversal ID = %q, want %q", reversal.ID, "t1/reversal/1")
	}
	if reversal.AmountCents != 4000 {
		t.Errorf("reversal amount = %d, want 4000", reversal.AmountCents)
	}
	// The swapped legs move the slice back: payer recovers 4000.
	if got := l.Balance("payer"); got != 14000 {
		t.Errorf("payer balance = %d, want 14000", got)
	}
	if got := l.Balance("payee"); got != 6000 {
		t.Errorf("payee balance = %d, want 6000", got)
	}

	// Idempotent replay: the same key returns the original reversal
	// and books nothing new.
	replay, dup, err := l.ReverseSettledTransfer("t1", 4000, "duplicate payment", "sr-key-1")
	if err != nil {
		t.Fatalf("replay ReverseSettledTransfer: %v", err)
	}
	if !dup {
		t.Error("replay reports duplicate = false")
	}
	if replay.ID != reversal.ID {
		t.Errorf("replay ID = %q, want %q", replay.ID, reversal.ID)
	}
	if got := l.Balance("payer"); got != 14000 {
		t.Errorf("payer balance after replay = %d, want 14000", got)
	}

	// The remainder reverses, then the cap bites: 6000 left, 6001 over.
	if _, _, err := l.ReverseSettledTransfer("t1", 6000, "remainder", "sr-key-2"); err != nil {
		t.Fatalf("remainder ReverseSettledTransfer: %v", err)
	}
	if _, _, err := l.ReverseSettledTransfer("t1", 1, "over cap", "sr-key-3"); !errors.Is(err, ErrAlreadyReversed) {
		t.Errorf("over-cap reversal: err = %v, want ErrAlreadyReversed", err)
	}
	if got := l.Balance("payee"); got != 0 {
		t.Errorf("payee balance = %d, want 0 (fully reversed)", got)
	}
}

func TestReverseSettledTransferRejectsUnsettled(t *testing.T) {
	// Unknown transfer: no registry row, no principal.
	l := seedSettledLedger(t)
	if _, _, err := l.ReverseSettledTransfer("nope", 100, "x", "k"); !errors.Is(err, ErrSettledTransferNotFound) {
		t.Errorf("unknown transfer: err = %v, want ErrSettledTransferNotFound", err)
	}

	// Raw journal post: an entry, but never a transfer.
	if _, _, err := l.Post(JournalEntry{ID: "raw1", DebitAccount: "payee", CreditAccount: "payer", AmountCents: 500, Currency: "USD", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("seed raw post: %v", err)
	}
	if _, _, err := l.ReverseSettledTransfer("raw1", 100, "x", "k"); !errors.Is(err, ErrSettledTransferNotFound) {
		t.Errorf("raw post: err = %v, want ErrSettledTransferNotFound", err)
	}

	// Pending-review transfer: registered, but the money never moved.
	lr := New(WithReviewThreshold(5000))
	seedBalance(t, lr, "payer", 20000)
	if _, err := lr.PostTransfer(Transfer{ID: "tx-review", From: "payer", To: "payee", AmountCents: 10000, Currency: "USD"}); err != nil {
		t.Fatalf("seed review transfer: %v", err)
	}
	if _, _, err := lr.ReverseSettledTransfer("tx-review", 100, "x", "k"); !errors.Is(err, ErrReversalOfPendingReview) {
		t.Errorf("pending-review transfer: err = %v, want ErrReversalOfPendingReview", err)
	}

	// Cross-currency transfer: two FX legs, no single-currency
	// principal in the registry.
	lfx := New(WithFXAccount("fx-pnl"))
	seedBalance(t, lfx, "payer", 20000)
	mustSetRate(t, lfx, "USD", "EUR", 108, 100)
	if _, err := lfx.PostTransfer(Transfer{ID: "fx1", From: "payer", To: "payee", AmountCents: 10000, Currency: "USD", ToCurrency: "EUR"}); err != nil {
		t.Fatalf("seed FX transfer: %v", err)
	}
	if _, _, err := lfx.ReverseSettledTransfer("fx1", 100, "x", "k"); !errors.Is(err, ErrSettledTransferNotFound) {
		t.Errorf("FX transfer: err = %v, want ErrSettledTransferNotFound", err)
	}

	// Non-positive slices fail before any lookup.
	if _, _, err := l.ReverseSettledTransfer("t1", 0, "x", "k"); !errors.Is(err, ErrNonPositiveAmount) {
		t.Errorf("zero amount: err = %v, want ErrNonPositiveAmount", err)
	}
}

func TestReverseSettledTransferCountsDisputeRefunds(t *testing.T) {
	l := seedSettledLedger(t)

	// An upheld dispute refunds 3000 through a partial reversal of
	// the same principal.
	if _, _, err := l.RaiseDispute(Dispute{ID: "d1", TransferID: "t1", ReasonCode: DisputeReasonUnauthorized, AmountCents: 3000}); err != nil {
		t.Fatalf("RaiseDispute: %v", err)
	}
	if _, err := l.ResolveDispute("d1", true); err != nil {
		t.Fatalf("ResolveDispute uphold: %v", err)
	}
	if got := l.Balance("payee"); got != 7000 {
		t.Fatalf("payee balance after dispute refund = %d, want 7000", got)
	}

	// The dispute refund already consumed 3000 of the cap: 7000 is
	// the remainder, 7001 over-corrects.
	if _, _, err := l.ReverseSettledTransfer("t1", 7001, "over dispute cap", "sr-dispute"); !errors.Is(err, ErrAlreadyReversed) {
		t.Errorf("over-dispute-cap reversal: err = %v, want ErrAlreadyReversed", err)
	}
	if _, _, err := l.ReverseSettledTransfer("t1", 7000, "remainder after dispute", "sr-dispute-2"); err != nil {
		t.Fatalf("remainder after dispute: %v", err)
	}
	if got := l.Balance("payee"); got != 0 {
		t.Errorf("payee balance = %d, want 0", got)
	}
}

func TestReverseSettledTransferReusesRiskGates(t *testing.T) {
	l := seedSettledLedger(t)

	// A frozen payee blocks the way back: the reversal's swapped
	// credit leg runs through the payee.
	l.Freeze("payee")
	if _, _, err := l.ReverseSettledTransfer("t1", 1000, "frozen", "sr-frozen"); !errors.Is(err, ErrAccountFrozen) {
		t.Errorf("frozen payee: err = %v, want ErrAccountFrozen", err)
	}
	// A rejection records nothing: the cap is untouched.
	l.Unfreeze("payee")
	if _, _, err := l.ReverseSettledTransfer("t1", 10000, "full after unfreeze", "sr-full"); err != nil {
		t.Fatalf("full reversal after unfreeze: %v", err)
	}
}

func TestReverseSettledTransferAudit(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	seedBalance(t, l, "payer", 20000)
	if _, err := l.PostTransfer(Transfer{ID: "t1", From: "payer", To: "payee", AmountCents: 10000, Currency: "USD"}); err != nil {
		t.Fatalf("seed PostTransfer: %v", err)
	}
	if _, _, err := l.ReverseSettledTransfer("t1", 4000, "duplicate payment", "sr-audit"); err != nil {
		t.Fatalf("ReverseSettledTransfer: %v", err)
	}
	// A replay books nothing new, so it stays silent: still one event.
	if _, _, err := l.ReverseSettledTransfer("t1", 4000, "duplicate payment", "sr-audit"); err != nil {
		t.Fatalf("replay ReverseSettledTransfer: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["settled_transfer_reversed"]
	if len(events) != 1 {
		t.Fatalf("settled_transfer_reversed events = %d, want 1", len(events))
	}
	d := events[0].Details
	if d["transfer_id"] != "t1" {
		t.Errorf("transfer_id = %v, want t1", d["transfer_id"])
	}
	if d["reason"] != "duplicate payment" {
		t.Errorf("reason = %v, want duplicate payment", d["reason"])
	}
	if d["reversal_entry_id"] != "t1/reversal/1" {
		t.Errorf("reversal_entry_id = %v, want t1/reversal/1", d["reversal_entry_id"])
	}
	if d["remaining_reversible_cents"] != float64(6000) {
		t.Errorf("remaining_reversible_cents = %v, want 6000", d["remaining_reversible_cents"])
	}
}

func TestReverseSettledTransferSnapshotRoundTrip(t *testing.T) {
	l := seedSettledLedger(t)
	if _, _, err := l.ReverseSettledTransfer("t1", 4000, "before snapshot", "sr-snap"); err != nil {
		t.Fatalf("ReverseSettledTransfer: %v", err)
	}

	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if got := restored.Balance("payer"); got != 14000 {
		t.Errorf("restored payer balance = %d, want 14000", got)
	}
	if got := restored.Balance("payee"); got != 6000 {
		t.Errorf("restored payee balance = %d, want 6000", got)
	}
	// The cap survived the round-trip: 6001 over-corrects, 6000 closes it.
	if _, _, err := restored.ReverseSettledTransfer("t1", 6001, "over cap", "sr-snap-2"); !errors.Is(err, ErrAlreadyReversed) {
		t.Errorf("restored over-cap: err = %v, want ErrAlreadyReversed", err)
	}
	if _, _, err := restored.ReverseSettledTransfer("t1", 6000, "remainder", "sr-snap-3"); err != nil {
		t.Fatalf("restored remainder: %v", err)
	}
	if got := restored.Balance("payee"); got != 0 {
		t.Errorf("restored payee balance = %d, want 0", got)
	}
}
