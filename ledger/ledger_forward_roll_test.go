package ledger

import (
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"
)

// forwardRolloverBooking books a forward with a pinned booking time and
// an opt-in notional cap, for the rollover tests: the cap exercises the
// "inherits notionalCap" contract of the roll.
func forwardRolloverBooking(id string, settleAt time.Time, capCents int64) ForwardBooking {
	b := forwardBooking(id, settleAt)
	b.NotionalCapCents = capCents
	return b
}

// TestForwardRolloverConfig: the rollover config is opt-in and
// fail-fast — a non-positive window or a maxRolls below 1 is rejected.
func TestForwardRolloverConfig(t *testing.T) {
	l := forwardTestLedger()
	if _, ok := l.ForwardRolloverConfig(); ok {
		t.Fatal("rollover enabled by default, want disabled")
	}
	for _, tc := range []struct {
		name   string
		window time.Duration
		max    int
	}{
		{"zero window", 0, 1},
		{"negative window", -time.Hour, 1},
		{"zero maxRolls", time.Hour, 0},
		{"negative maxRolls", time.Hour, -2},
	} {
		if err := l.EnableForwardRollover(tc.window, tc.max); !errors.Is(err, ErrInvalidForwardRollover) {
			t.Errorf("%s: err = %v, want ErrInvalidForwardRollover", tc.name, err)
		}
	}
	if err := l.EnableForwardRollover(48*time.Hour, 3); err != nil {
		t.Fatalf("EnableForwardRollover: %v", err)
	}
	cfg, ok := l.ForwardRolloverConfig()
	if !ok {
		t.Fatal("rollover not enabled after EnableForwardRollover")
	}
	if cfg.RollWindow != 48*time.Hour || cfg.MaxRolls != 3 {
		t.Fatalf("config = %+v, want {48h, 3}", cfg)
	}
	l.DisableForwardRollover()
	if _, ok := l.ForwardRolloverConfig(); ok {
		t.Fatal("rollover still enabled after DisableForwardRollover")
	}
}

// TestForwardRolloverHappyPath: SettleForward inside the roll window
// books a replacement at the current market price and lapses the
// original, reporting ErrForwardRolled naming the new contract.
func TestForwardRolloverHappyPath(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	if err := l.EnableForwardRollover(48*time.Hour, 3); err != nil {
		t.Fatalf("EnableForwardRollover: %v", err)
	}
	mustBookForwardLocked(t, l, forwardRolloverBooking("fwd-roll", now.Add(2*time.Hour), 50000), now)
	mustSetRate(t, l, "USD", "EUR", 12, 10) // market 1.2, contract locked 1.1

	_, err := l.settleForwardLocked("fwd-roll", now.Add(time.Hour))
	if !errors.Is(err, ErrForwardRolled) {
		t.Fatalf("settle inside roll window: err = %v, want ErrForwardRolled", err)
	}
	if !strings.Contains(err.Error(), "roll/fwd-roll/1") {
		t.Fatalf("roll error = %q, want it to name roll/fwd-roll/1", err)
	}

	// The original lapsed: cancelled with reason "expired".
	orig, ok := l.Forward("fwd-roll")
	if !ok {
		t.Fatal("original contract gone after roll")
	}
	if orig.Status != ForwardCancelled || orig.CancelReason != ForwardCancelExpired {
		t.Fatalf("original status = %s/%s, want cancelled/expired", orig.Status, orig.CancelReason)
	}

	// The replacement: booked at the live market price, inheriting the
	// commercial dimensions and the lineage.
	got, ok := l.Forward("roll/fwd-roll/1")
	if !ok {
		t.Fatal("replacement contract missing")
	}
	if got.Status != ForwardBooked {
		t.Fatalf("replacement status = %s, want booked", got.Status)
	}
	if got.RateNum != 6 || got.RateDen != 5 {
		t.Fatalf("replacement rate = %d/%d, want market 12/10 reduced to 6/5", got.RateNum, got.RateDen)
	}
	if got.FromAccount != "payer" || got.ToAccount != "payee" {
		t.Fatalf("replacement accounts = %s/%s, want payer/payee", got.FromAccount, got.ToAccount)
	}
	if got.FromCurrency != "USD" || got.ToCurrency != "EUR" {
		t.Fatalf("replacement pair = %s/%s, want USD/EUR", got.FromCurrency, got.ToCurrency)
	}
	if got.NotionalCents != 10000 || got.NotionalCapCents != 50000 {
		t.Fatalf("replacement notional = %d cap = %d, want 10000/50000", got.NotionalCents, got.NotionalCapCents)
	}
	if got.FXSide != "sell" {
		t.Fatalf("replacement fx_side = %q, want sell", got.FXSide)
	}
	if got.Rolls != 1 || got.RolledFromID != "fwd-roll" {
		t.Fatalf("replacement lineage = rolls %d from %q, want 1/fwd-roll", got.Rolls, got.RolledFromID)
	}
	// Value date pushed forward by the original tenor (2h).
	if !got.SettleAt.Equal(now.Add(4 * time.Hour)) {
		t.Fatalf("replacement settle_at = %s, want %s", got.SettleAt, now.Add(4*time.Hour))
	}
	// The replacement pins the current market-table version (1), not
	// the original's (0 — booked before any rate existed).
	if got.RateSnapshotVersion != 1 {
		t.Fatalf("replacement snapshot version = %d, want 1", got.RateSnapshotVersion)
	}
	if orig.RateSnapshotVersion != 0 {
		t.Fatalf("original snapshot version = %d, want 0", orig.RateSnapshotVersion)
	}
	// No journal entries: the roll moves the obligation, not money.
	if n := len(l.entries); n != 0 {
		t.Fatalf("journal entries after roll = %d, want 0", n)
	}
}

// TestForwardRolloverIdempotent: a retried settlement after a roll
// replays to the same replacement contract — one roll, one audit
// event, no duplicate booking.
func TestForwardRolloverIdempotent(t *testing.T) {
	al := mustAuditLog(t)
	defer al.Close()
	l := New(WithAuditLog(al), WithFXAccount("fx"))
	now := time.Now().UTC()
	if err := l.EnableForwardRollover(48*time.Hour, 3); err != nil {
		t.Fatalf("EnableForwardRollover: %v", err)
	}
	mustBookForwardLocked(t, l, forwardRolloverBooking("fwd-replay", now.Add(2*time.Hour), 0), now)
	mustSetRate(t, l, "USD", "EUR", 12, 10)

	if _, err := l.settleForwardLocked("fwd-replay", now.Add(time.Hour)); !errors.Is(err, ErrForwardRolled) {
		t.Fatalf("first settle: err = %v, want ErrForwardRolled", err)
	}
	err := func() error {
		_, err := l.settleForwardLocked("fwd-replay", now.Add(90*time.Minute))
		return err
	}()
	if !errors.Is(err, ErrForwardRolled) {
		t.Fatalf("replayed settle: err = %v, want ErrForwardRolled", err)
	}
	if !strings.Contains(err.Error(), "roll/fwd-replay/1") {
		t.Fatalf("replayed roll error = %q, want roll/fwd-replay/1", err)
	}
	if n := len(l.forwardsLocked()); n != 2 {
		t.Fatalf("contracts = %d, want 2 (original + one replacement)", n)
	}

	now2 := time.Now()
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, corrupt, err := ReadAuditLog(al.Dir(), now2.UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt events = %d, want 0", corrupt)
	}
	rolled := 0
	for _, ev := range events {
		if ev.Op == "fx_forward_rolled" {
			rolled++
			if ev.Details["new_forward_id"] != "roll/fwd-replay/1" || ev.Details["forward_id"] != "fwd-replay" {
				t.Fatalf("roll audit details = %v", ev.Details)
			}
			if ev.Details["new_locked_rate"] != "6/5" {
				t.Fatalf("roll audit new rate = %v, want 6/5", ev.Details["new_locked_rate"])
			}
			if ev.VersionBefore != ev.VersionAfter {
				t.Fatalf("roll moved version %d -> %d, want structural (no bump)", ev.VersionBefore, ev.VersionAfter)
			}
		}
	}
	if rolled != 1 {
		t.Fatalf("fx_forward_rolled events = %d, want 1", rolled)
	}
}

// TestForwardRolloverNoMarketRate: without a live rate for the pair
// the roll cannot be priced — it fails closed and the contract stays
// booked.
func TestForwardRolloverNoMarketRate(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	if err := l.EnableForwardRollover(48*time.Hour, 3); err != nil {
		t.Fatalf("EnableForwardRollover: %v", err)
	}
	mustBookForwardLocked(t, l, forwardBooking("fwd-norate", now.Add(2*time.Hour)), now)

	_, err := l.settleForwardLocked("fwd-norate", now.Add(time.Hour))
	if !errors.Is(err, ErrFXRateMissing) {
		t.Fatalf("roll without market rate: err = %v, want ErrFXRateMissing", err)
	}
	got, ok := l.Forward("fwd-norate")
	if !ok || got.Status != ForwardBooked {
		t.Fatalf("contract status after failed roll = %v, want booked", got)
	}
	if _, ok := l.Forward("roll/fwd-norate/1"); ok {
		t.Fatal("replacement booked despite failed roll")
	}
}

// TestForwardRolloverMaxRollsFallsBackToSettle: when the roll budget is
// exhausted the contract settles normally at its locked rate — the
// settlement promise outranks the roll preference.
func TestForwardRolloverMaxRollsFallsBackToSettle(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	if err := l.EnableForwardRollover(48*time.Hour, 1); err != nil {
		t.Fatalf("EnableForwardRollover: %v", err)
	}
	mustBookForwardLocked(t, l, forwardBooking("fwd-max", now.Add(time.Hour)), now)
	mustSetRate(t, l, "USD", "EUR", 12, 10)
	mustFundForwardPayer(t, l, 100000, "USD")
	// Exhaust this contract's roll budget: the next settle is inside
	// the window but must not roll.
	l.forwards["fwd-max"].Rolls = 1

	receipt, err := l.settleForwardLocked("fwd-max", now.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("settle with exhausted roll budget: %v", err)
	}
	if receipt.FX == nil || receipt.FX.RateNum != 11 || receipt.FX.RateDen != 10 {
		t.Fatalf("settled at %+v, want the locked 11/10 rate", receipt.FX)
	}
	if _, ok := l.Forward("roll/fwd-max/2"); ok {
		t.Fatal("rolled despite exhausted roll budget")
	}
}

// TestForwardRolloverOutsideWindow: a contract far from its last
// settleable instant does not roll — the settlement path is unchanged.
func TestForwardRolloverOutsideWindow(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	if err := l.EnableForwardRollover(48*time.Hour, 3); err != nil {
		t.Fatalf("EnableForwardRollover: %v", err)
	}
	mustBookForwardLocked(t, l, forwardBooking("fwd-far", now.Add(100*time.Hour)), now)
	mustSetRate(t, l, "USD", "EUR", 12, 10)

	if _, err := l.settleForwardLocked("fwd-far", now.Add(time.Hour)); !errors.Is(err, ErrForwardNotSettleable) {
		t.Fatalf("early settle: err = %v, want ErrForwardNotSettleable", err)
	}
	if _, ok := l.Forward("roll/fwd-far/1"); ok {
		t.Fatal("rolled outside the roll window")
	}
}

// TestForwardRolloverChain: consecutive rolls extend the obligation
// until the roll budget runs out, then the contract settles normally.
func TestForwardRolloverChain(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	if err := l.EnableForwardRollover(48*time.Hour, 2); err != nil {
		t.Fatalf("EnableForwardRollover: %v", err)
	}
	mustBookForwardLocked(t, l, forwardBooking("fwd-chain", now.Add(time.Hour)), now)
	mustSetRate(t, l, "USD", "EUR", 12, 10)
	mustFundForwardPayer(t, l, 100000, "USD")

	if _, err := l.settleForwardLocked("fwd-chain", now); !errors.Is(err, ErrForwardRolled) {
		t.Fatalf("roll 1: err = %v, want ErrForwardRolled", err)
	}
	r1, ok := l.Forward("roll/fwd-chain/1")
	if !ok || r1.Rolls != 1 || r1.RolledFromID != "fwd-chain" {
		t.Fatalf("roll 1 contract = %+v", r1)
	}
	// Second roll: the replacement is inside its own window (25h of
	// remaining grace < 48h) and has budget left.
	if _, err := l.settleForwardLocked("roll/fwd-chain/1", now.Add(time.Hour)); !errors.Is(err, ErrForwardRolled) {
		t.Fatalf("roll 2: err = %v, want ErrForwardRolled", err)
	}
	r2, ok := l.Forward("roll/roll/fwd-chain/1/2")
	if !ok {
		t.Fatal("roll 2 contract missing")
	}
	if r2.Rolls != 2 || r2.RolledFromID != "roll/fwd-chain/1" {
		t.Fatalf("roll 2 lineage = rolls %d from %q", r2.Rolls, r2.RolledFromID)
	}
	if r2.RateNum != 6 || r2.RateDen != 5 {
		t.Fatalf("roll 2 rate = %d/%d, want market 12/10 reduced to 6/5", r2.RateNum, r2.RateDen)
	}
	// Roll budget exhausted: the third attempt settles the twice-rolled
	// contract at its locked (market-at-roll) rate instead of rolling
	// a third time.
	receipt, err := l.settleForwardLocked("roll/roll/fwd-chain/1/2", now.Add(4*time.Hour))
	if err != nil {
		t.Fatalf("settle after roll budget exhausted: %v", err)
	}
	if receipt.FX == nil || receipt.FX.RateNum != 6 || receipt.FX.RateDen != 5 {
		t.Fatalf("settled at %+v, want the rolled 12/10 rate (reduced 6/5)", receipt.FX)
	}
	if _, ok := l.Forward("roll/roll/roll/fwd-chain/1/2/3"); ok {
		t.Fatal("rolled past maxRolls")
	}
}

// TestForwardRolloverDisabledByDefault: without opt-in, a near-expiry
// contract settles normally — rollover changes nothing unless enabled.
func TestForwardRolloverDisabledByDefault(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	mustBookForwardLocked(t, l, forwardBooking("fwd-plain", now.Add(time.Hour)), now)
	mustFundForwardPayer(t, l, 100000, "USD")

	receipt, err := l.settleForwardLocked("fwd-plain", now.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("settle without rollover: %v", err)
	}
	if receipt.FX == nil || receipt.FX.ConvertedCents != 11000 {
		t.Fatalf("unexpected conversion: %+v", receipt.FX)
	}
}

// TestForwardRolloverPinsCurrentSnapshot: the replacement pins the
// market-table version current at roll time, not the original's.
func TestForwardRolloverPinsCurrentSnapshot(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	if err := l.EnableForwardRollover(48*time.Hour, 3); err != nil {
		t.Fatalf("EnableForwardRollover: %v", err)
	}
	mustSetRate(t, l, "USD", "EUR", 10, 10) // version 1
	mustBookForwardLocked(t, l, forwardBooking("fwd-snap", now.Add(2*time.Hour)), now)
	mustSetRate(t, l, "USD", "EUR", 13, 10) // version 2: the roll prices here

	orig, _ := l.Forward("fwd-snap")
	if orig.RateSnapshotVersion != 1 {
		t.Fatalf("original snapshot version = %d, want 1", orig.RateSnapshotVersion)
	}
	if _, err := l.settleForwardLocked("fwd-snap", now.Add(time.Hour)); !errors.Is(err, ErrForwardRolled) {
		t.Fatalf("roll: err = %v, want ErrForwardRolled", err)
	}
	got, ok := l.Forward("roll/fwd-snap/1")
	if !ok {
		t.Fatal("replacement missing")
	}
	if got.RateSnapshotVersion != 2 {
		t.Fatalf("replacement snapshot version = %d, want 2", got.RateSnapshotVersion)
	}
	if got.RateNum != 13 || got.RateDen != 10 {
		t.Fatalf("replacement rate = %d/%d, want 13/10", got.RateNum, got.RateDen)
	}
}

// TestForwardRolloverTenorExtends: the replacement's value date is the
// original's plus the original tenor — a 30-day forward rolled near
// expiry settles ~30 days after the original value date.
func TestForwardRolloverTenorExtends(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	if err := l.EnableForwardRollover(72*time.Hour, 3); err != nil {
		t.Fatalf("EnableForwardRollover: %v", err)
	}
	booked := now
	settleAt := now.Add(30 * 24 * time.Hour)
	mustBookForwardLocked(t, l, forwardBooking("fwd-tenor", settleAt), booked)
	mustSetRate(t, l, "USD", "EUR", 12, 10)

	rollAt := settleAt.Add(-time.Hour) // inside the 72h window
	if _, err := l.settleForwardLocked("fwd-tenor", rollAt); !errors.Is(err, ErrForwardRolled) {
		t.Fatalf("roll: err = %v, want ErrForwardRolled", err)
	}
	got, ok := l.Forward("roll/fwd-tenor/1")
	if !ok {
		t.Fatal("replacement missing")
	}
	if !got.SettleAt.Equal(settleAt.Add(30 * 24 * time.Hour)) {
		t.Fatalf("replacement settle_at = %s, want original + 30d", got.SettleAt)
	}
}

// TestForwardRolloverRateExactness: the rolled rate is the exact
// reduced ratio from the market table — no float64 touches the money.
func TestForwardRolloverRateExactness(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	if err := l.EnableForwardRollover(48*time.Hour, 3); err != nil {
		t.Fatalf("EnableForwardRollover: %v", err)
	}
	mustBookForwardLocked(t, l, forwardBooking("fwd-exact", now.Add(2*time.Hour)), now)
	// Set the rate as a Rat so the table holds a reduced fraction.
	if err := l.SetFXRateRat("USD", "EUR", big.NewRat(7, 3), 0); err != nil {
		t.Fatalf("SetFXRateRat: %v", err)
	}
	if _, err := l.settleForwardLocked("fwd-exact", now.Add(time.Hour)); !errors.Is(err, ErrForwardRolled) {
		t.Fatalf("roll: err = %v, want ErrForwardRolled", err)
	}
	got, ok := l.Forward("roll/fwd-exact/1")
	if !ok {
		t.Fatal("replacement missing")
	}
	if got.RateNum != 7 || got.RateDen != 3 {
		t.Fatalf("replacement rate = %d/%d, want exact 7/3", got.RateNum, got.RateDen)
	}
}

// TestForwardRolloverReportCountsRolled: a rolled contract counts as
// expired in the reconcile forward book — expiry IS cancellation.
func TestForwardRolloverReportCountsRolled(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	if err := l.EnableForwardRollover(48*time.Hour, 3); err != nil {
		t.Fatalf("EnableForwardRollover: %v", err)
	}
	mustBookForwardLocked(t, l, forwardBooking("fwd-rep", now.Add(2*time.Hour)), now)
	mustSetRate(t, l, "USD", "EUR", 12, 10)
	if _, err := l.settleForwardLocked("fwd-rep", now.Add(time.Hour)); !errors.Is(err, ErrForwardRolled) {
		t.Fatalf("roll: err = %v, want ErrForwardRolled", err)
	}
	rep := l.Reconcile(now.Add(time.Hour))
	if rep.ForwardBook.Total != 2 || rep.ForwardBook.Booked != 1 || rep.ForwardBook.Expired != 1 {
		t.Fatalf("forward book = %+v, want total 2 / booked 1 / expired 1", rep.ForwardBook)
	}
}
