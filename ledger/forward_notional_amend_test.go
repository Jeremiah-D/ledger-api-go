package ledger

import (
	"bytes"
	"context"
	"errors"
	"math/big"
	"testing"
	"time"
)

// forwardBookingWithCap books like forwardBooking but with an opt-in
// notional cap, so the cap gate has something to bite on.
func forwardBookingWithCap(id string, settleAt time.Time, capCents int64) ForwardBooking {
	b := forwardBooking(id, settleAt)
	b.NotionalCapCents = capCents
	return b
}

// TestAmendForwardNotionalSuccess pins the core LG-382 guarantee:
// resizing a booked forward changes the notional and nothing else,
// off-journal.
func TestAmendForwardNotionalSuccess(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	before := mustBookForwardLocked(t, l, forwardBooking("fwd-notional-ok", now.Add(2*time.Hour)), now)
	versionBefore := l.Version()
	got, err := l.AmendForwardNotional("fwd-notional-ok", 15000)
	if err != nil {
		t.Fatalf("AmendForwardNotional: %v", err)
	}
	if got.NotionalCents != 15000 {
		t.Errorf("notional_cents = %d, want 15000", got.NotionalCents)
	}
	if got.RateNum != before.RateNum || got.RateDen != before.RateDen {
		t.Errorf("locked rate moved: %d/%d, want %d/%d", got.RateNum, got.RateDen, before.RateNum, before.RateDen)
	}
	if !got.SettleAt.Equal(before.SettleAt) {
		t.Errorf("settle_at moved: %v, want %v", got.SettleAt, before.SettleAt)
	}
	if l.Version() != versionBefore {
		t.Error("version moved: amendment is off-journal")
	}
	stored, ok := l.Forward("fwd-notional-ok")
	if !ok || stored.NotionalCents != 15000 {
		t.Errorf("stored contract = %+v, %v, want booked with notional 15000", stored, ok)
	}
}

// TestAmendForwardNotionalSettleUsesNewNotional: settlement converts
// the live notional at the locked rate — the next sweep settles the
// new size, never the booked size.
func TestAmendForwardNotionalSettleUsesNewNotional(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	mustBookForwardLocked(t, l, forwardBooking("fwd-notional-settle", now.Add(30*time.Minute)), now)
	if _, err := l.AmendForwardNotional("fwd-notional-settle", 20000); err != nil {
		t.Fatalf("AmendForwardNotional: %v", err)
	}
	mustFundForwardPayer(t, l, 100000, "USD")
	receipt, err := l.settleForwardLocked("fwd-notional-settle", now.Add(31*time.Minute))
	if err != nil {
		t.Fatalf("settle at +31m: %v", err)
	}
	if receipt.FX == nil || receipt.FX.SourceCents != 20000 || receipt.FX.ConvertedCents != 22000 {
		t.Errorf("settled FX = %+v, want source 20000 converted 22000", receipt.FX)
	}
	if receipt.FX.RateNum != 11 || receipt.FX.RateDen != 10 {
		t.Errorf("settled at %d/%d, want locked 11/10", receipt.FX.RateNum, receipt.FX.RateDen)
	}
}

// TestAmendForwardNotionalCap: the opt-in cap is a hard ceiling on
// the amended size, and the cap itself never moves.
func TestAmendForwardNotionalCap(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	mustBookForwardLocked(t, l, forwardBookingWithCap("fwd-notional-cap", now.Add(2*time.Hour), 15000), now)
	if _, err := l.AmendForwardNotional("fwd-notional-cap", 15001); !errors.Is(err, ErrForwardCapExceeded) {
		t.Errorf("over cap: err = %v, want ErrForwardCapExceeded", err)
	}
	stored, ok := l.Forward("fwd-notional-cap")
	if !ok || stored.NotionalCents != 10000 {
		t.Errorf("over-cap attempt moved live notional: %+v, %v", stored, ok)
	}
	got, err := l.AmendForwardNotional("fwd-notional-cap", 15000)
	if err != nil {
		t.Fatalf("AmendForwardNotional to cap: %v", err)
	}
	if got.NotionalCents != 15000 || got.NotionalCapCents != 15000 {
		t.Errorf("at-cap contract = %+v, want notional 15000 cap 15000", got)
	}
}

func TestAmendForwardNotionalRejections(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	mustBookForwardLocked(t, l, forwardBooking("fwd-notional-rej", now.Add(2*time.Hour)), now)
	for _, tc := range []struct {
		name string
		v    int64
	}{
		{"zero", 0},
		{"negative", -1},
	} {
		if _, err := l.AmendForwardNotional("fwd-notional-rej", tc.v); !errors.Is(err, ErrForwardNotionalAmendInvalid) {
			t.Errorf("%s: err = %v, want ErrForwardNotionalAmendInvalid", tc.name, err)
		}
		stored, ok := l.Forward("fwd-notional-rej")
		if !ok || stored.NotionalCents != 10000 {
			t.Errorf("%s: live notional = %v, want untouched 10000", tc.name, stored)
		}
	}
	if _, err := l.AmendForwardNotional("fwd-no-such", 15000); !errors.Is(err, ErrForwardNotFound) {
		t.Errorf("unknown id: err = %v, want ErrForwardNotFound", err)
	}
}

func TestAmendForwardNotionalTerminalStates(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	// Settled.
	mustBookForwardLocked(t, l, forwardBooking("fwd-notional-settled", now.Add(time.Hour)), now)
	mustFundForwardPayer(t, l, 100000, "USD")
	if _, err := l.settleForwardLocked("fwd-notional-settled", now.Add(2*time.Hour)); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if _, err := l.AmendForwardNotional("fwd-notional-settled", 15000); !errors.Is(err, ErrForwardNotBooked) {
		t.Errorf("settled: err = %v, want ErrForwardNotBooked", err)
	}
	// Cancelled.
	mustBookForwardLocked(t, l, forwardBooking("fwd-notional-cancelled", now.Add(time.Hour)), now)
	if _, err := l.CancelForward("fwd-notional-cancelled"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := l.AmendForwardNotional("fwd-notional-cancelled", 15000); !errors.Is(err, ErrForwardNotBooked) {
		t.Errorf("cancelled: err = %v, want ErrForwardNotBooked", err)
	}
	// Lapsed: persisted as cancelled/expired by an explicit sweep,
	// and the amendment reports expired, never reviving it.
	mustBookForwardLocked(t, l, forwardBooking("fwd-notional-lapsed", now.Add(time.Hour)), now)
	if _, err := l.SetForwardSettlementGracePeriod(10 * time.Minute); err != nil {
		t.Fatalf("SetForwardSettlementGracePeriod: %v", err)
	}
	if expired := l.ExpireForwards(now.Add(2 * time.Hour)); expired != 1 {
		t.Fatalf("ExpireForwards: expired = %d, want 1", expired)
	}
	if _, err := l.AmendForwardNotional("fwd-notional-lapsed", 15000); !errors.Is(err, ErrForwardExpired) {
		t.Errorf("lapsed: err = %v, want ErrForwardExpired", err)
	}
	stored, ok := l.Forward("fwd-notional-lapsed")
	if !ok || stored.Status != ForwardCancelled || stored.CancelReason != ForwardCancelExpired {
		t.Errorf("lapsed contract = %+v, %v, want cancelled/expired", stored, ok)
	}
}

// TestAmendForwardNotionalOnlyNotionalMoves: the amendment is a
// notional revision, never a re-price — every other field, including
// the booking provenance, is untouched.
func TestAmendForwardNotionalOnlyNotionalMoves(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	b := forwardBookingWithCap("fwd-notional-fields", now.Add(time.Hour), 20000)
	b.FXSide = "buy"
	before := mustBookForwardLocked(t, l, b, now)
	versionBefore := l.Version()
	got, err := l.AmendForwardNotional("fwd-notional-fields", 12000)
	if err != nil {
		t.Fatalf("AmendForwardNotional: %v", err)
	}
	if got.ID != before.ID || got.FromAccount != before.FromAccount || got.ToAccount != before.ToAccount ||
		got.FromCurrency != before.FromCurrency || got.ToCurrency != before.ToCurrency ||
		got.RateNum != before.RateNum || got.RateDen != before.RateDen ||
		got.RateSnapshotVersion != before.RateSnapshotVersion ||
		got.NotionalCapCents != before.NotionalCapCents ||
		!got.SettleAt.Equal(before.SettleAt) ||
		got.FXSide != before.FXSide || got.Status != before.Status ||
		!got.BookedAt.Equal(before.BookedAt) || got.BookedVersion != before.BookedVersion {
		t.Errorf("amended contract = %+v, want only notional moved from %+v", got, before)
	}
	if got.NotionalCents != 12000 {
		t.Errorf("notional_cents = %d, want 12000", got.NotionalCents)
	}
	if l.Version() != versionBefore {
		t.Error("version moved: amendment is off-journal")
	}
}

func TestAmendForwardNotionalNoop(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithFXAccount("fx"))
	now := time.Now().UTC()
	mustBookForwardLocked(t, l, forwardBooking("fwd-notional-noop", now.Add(time.Hour)), now)
	versionBefore := l.Version()
	got, err := l.AmendForwardNotional("fwd-notional-noop", 10000)
	if err != nil {
		t.Fatalf("AmendForwardNotional same value: %v", err)
	}
	if got.NotionalCents != 10000 || got.Status != ForwardBooked {
		t.Errorf("no-op contract = %+v, want original notional booked", got)
	}
	if l.Version() != versionBefore {
		t.Error("version moved: amendment is off-journal")
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if events := auditEventsByOp(t, dir)["fx_forward_notional_amended"]; len(events) != 0 {
		t.Errorf("amend events = %d, want 0 for a no-op", len(events))
	}
}

func TestAmendForwardNotionalAudit(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithFXAccount("fx"))
	now := time.Now().UTC()
	before := mustBookForwardLocked(t, l, forwardBooking("fwd-notional-audit", now.Add(time.Hour)), now)
	versionBefore := l.Version()
	ctx := ContextWithActor(context.Background(), "ops-alice")
	got, err := l.AmendForwardNotionalCtx(ctx, "fwd-notional-audit", 18000)
	if err != nil {
		t.Fatalf("AmendForwardNotionalCtx: %v", err)
	}
	if got.NotionalCents != 18000 {
		t.Errorf("notional_cents = %d, want 18000", got.NotionalCents)
	}
	if l.Version() != versionBefore {
		t.Error("version moved: amendment is off-journal")
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["fx_forward_notional_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
	if events[0].Actor != "ops-alice" {
		t.Errorf("event actor = %q, want ops-alice", events[0].Actor)
	}
	d := events[0].Details
	if d["forward_id"] != "fwd-notional-audit" ||
		d["previous_notional_cents"] != float64(before.NotionalCents) ||
		d["notional_cents"] != float64(18000) {
		t.Errorf("event details = %+v, want fwd-notional-audit %d->18000", d, before.NotionalCents)
	}
}

// TestAmendForwardNotionalDefaultActor: without a ctx actor the
// audit event keeps the entrypoint name, along
// SetForwardSettlementGracePeriodCtx (the HTTP layer defaults a
// missing x-ledger-actor header to "system").
func TestAmendForwardNotionalDefaultActor(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithFXAccount("fx"))
	now := time.Now().UTC()
	mustBookForwardLocked(t, l, forwardBooking("fwd-notional-actor", now.Add(time.Hour)), now)
	if _, err := l.AmendForwardNotional("fwd-notional-actor", 15000); err != nil {
		t.Fatalf("AmendForwardNotional: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["fx_forward_notional_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
	if events[0].Actor != "AmendForwardNotional" {
		t.Errorf("event actor = %q, want AmendForwardNotional", events[0].Actor)
	}
}

func TestAmendForwardNotionalSnapshotRoundTrip(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	mustBookForwardLocked(t, l, forwardBooking("fwd-notional-snap", now.Add(time.Hour)), now)
	if _, err := l.AmendForwardNotional("fwd-notional-snap", 25000); err != nil {
		t.Fatalf("AmendForwardNotional: %v", err)
	}
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(&buf)
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if !snapshotLedgersEqual(l, restored) {
		t.Fatal("snapshot round-trip diverged after amendment")
	}
	c, ok := restored.Forward("fwd-notional-snap")
	if !ok || c.NotionalCents != 25000 || c.Status != ForwardBooked {
		t.Errorf("restored contract = %+v, %v, want booked with notional 25000", c, ok)
	}
}

// TestAmendForwardNotionalRateUnmoved pins the frozen-rate
// invariant under a resize: the locked 11/10 survives, and the
// amended notional prices at exactly that rate.
func TestAmendForwardNotionalRateUnmoved(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	mustBookForwardLocked(t, l, forwardBooking("fwd-notional-rate", now.Add(time.Hour)), now)
	if _, err := l.AmendForwardNotional("fwd-notional-rate", 30000); err != nil {
		t.Fatalf("AmendForwardNotional: %v", err)
	}
	// RateNum/RateDen are int64 on the contract; the booking rate
	// was the reduced 11/10 from big.NewRat(11, 10).
	stored, ok := l.Forward("fwd-notional-rate")
	if !ok {
		t.Fatal("contract missing after amendment")
	}
	if stored.RateNum != 11 || stored.RateDen != 10 {
		t.Errorf("locked rate = %d/%d, want 11/10", stored.RateNum, stored.RateDen)
	}
	// The conversion the desk would price at the new size is
	// floor(30000 * 11/10) = 33000, computed exactly with big.Rat
	// the way settlement does — no float ever touches money.
	rate := big.NewRat(stored.RateNum, stored.RateDen)
	converted := new(big.Rat).Mul(big.NewRat(stored.NotionalCents, 1), rate)
	if !converted.IsInt() {
		t.Fatalf("converted %v is not integral", converted)
	}
	if converted.Num().Int64() != 33000 {
		t.Errorf("converted = %v, want 33000", converted.Num())
	}
}
