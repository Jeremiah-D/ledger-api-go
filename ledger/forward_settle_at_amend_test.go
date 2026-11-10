package ledger

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

// TestAmendForwardSettleAtShortenThenSettle pins the core LG-374
// guarantee: shortening the value date moves the settlement gate —
// the contract is no longer settleable before the new value date,
// and settles at the locked rate once the new value date arrives.
func TestAmendForwardSettleAtShortenThenSettle(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	mustBookForwardLocked(t, l, forwardBooking("fwd-amend-short", now.Add(2*time.Hour)), now)
	shortened := now.Add(30 * time.Minute)
	got, err := l.AmendForwardSettleAt("fwd-amend-short", shortened)
	if err != nil {
		t.Fatalf("AmendForwardSettleAt: %v", err)
	}
	if !got.SettleAt.Equal(shortened) {
		t.Errorf("settle_at = %v, want %v", got.SettleAt, shortened)
	}
	// Before the new value date the contract is not settleable,
	// even though the original +2h date has not arrived either.
	if _, err := l.settleForwardLocked("fwd-amend-short", now.Add(10*time.Minute)); !errors.Is(err, ErrForwardNotSettleable) {
		t.Errorf("settle at +10m: err = %v, want ErrForwardNotSettleable", err)
	}
	// Past the new value date it settles at the locked rate.
	mustFundForwardPayer(t, l, 100000, "USD")
	receipt, err := l.settleForwardLocked("fwd-amend-short", now.Add(31*time.Minute))
	if err != nil {
		t.Fatalf("settle at +31m: %v", err)
	}
	if receipt.FX == nil || receipt.FX.RateNum != 11 || receipt.FX.RateDen != 10 {
		t.Errorf("settled at %+v, want locked 11/10", receipt.FX)
	}
}

// TestAmendForwardSettleAtLengthenSurvivesOldDate: lengthening the
// value date moves the lapse horizon with it — with a short grace,
// the sweep past the original value date must leave the lengthened
// contract alone.
func TestAmendForwardSettleAtLengthenSurvivesOldDate(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	mustBookForwardLocked(t, l, forwardBooking("fwd-amend-long", now.Add(time.Hour)), now)
	if _, err := l.SetForwardSettlementGracePeriod(10 * time.Minute); err != nil {
		t.Fatalf("SetForwardSettlementGracePeriod: %v", err)
	}
	lengthened := now.Add(3 * time.Hour)
	if _, err := l.AmendForwardSettleAt("fwd-amend-long", lengthened); err != nil {
		t.Fatalf("AmendForwardSettleAt: %v", err)
	}
	// The sweep at +2h — past the original +1h value date and its
	// 10m grace — must leave the lengthened contract alone.
	if expired := l.ExpireForwards(now.Add(2 * time.Hour)); expired != 0 {
		t.Fatalf("ExpireForwards(+2h): expired = %d, want 0", expired)
	}
	stored, ok := l.Forward("fwd-amend-long")
	if !ok || stored.Status != ForwardBooked || !stored.SettleAt.Equal(lengthened) {
		t.Errorf("stored contract = %+v, %v, want booked with settle_at %v", stored, ok, lengthened)
	}
}

// TestAmendForwardSettleAtShortenThenSweepExpiresEarly: the mirror
// image — shortening the value date pulls the lapse horizon forward,
// so the sweep past the new date (plus grace) lapses a contract the
// original date would have kept alive.
func TestAmendForwardSettleAtShortenThenSweepExpiresEarly(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	mustBookForwardLocked(t, l, forwardBooking("fwd-amend-sweep", now.Add(2*time.Hour)), now)
	if _, err := l.SetForwardSettlementGracePeriod(10 * time.Minute); err != nil {
		t.Fatalf("SetForwardSettlementGracePeriod: %v", err)
	}
	if _, err := l.AmendForwardSettleAt("fwd-amend-sweep", now.Add(30*time.Minute)); err != nil {
		t.Fatalf("AmendForwardSettleAt: %v", err)
	}
	if expired := l.ExpireForwards(now.Add(45 * time.Minute)); expired != 1 {
		t.Fatalf("ExpireForwards(+45m): expired = %d, want 1", expired)
	}
	stored, ok := l.Forward("fwd-amend-sweep")
	if !ok || stored.Status != ForwardCancelled || stored.CancelReason != ForwardCancelExpired {
		t.Errorf("stored contract = %+v, %v, want cancelled/expired", stored, ok)
	}
}

func TestAmendForwardSettleAtRejections(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	mustBookForwardLocked(t, l, forwardBooking("fwd-amend-rej", now.Add(2*time.Hour)), now)
	original := now.Add(2 * time.Hour)
	for _, tc := range []struct {
		name string
		at   time.Time
	}{
		{"past", now.Add(-time.Hour)},
		{"now", time.Now()},
		{"zero", time.Time{}},
	} {
		if _, err := l.AmendForwardSettleAt("fwd-amend-rej", tc.at); !errors.Is(err, ErrForwardSettleAtAmendInvalid) {
			t.Errorf("%s: err = %v, want ErrForwardSettleAtAmendInvalid", tc.name, err)
		}
		stored, ok := l.Forward("fwd-amend-rej")
		if !ok || !stored.SettleAt.Equal(original) {
			t.Errorf("%s: live settle_at = %v, want untouched %v", tc.name, stored, original)
		}
	}
	if _, err := l.AmendForwardSettleAt("fwd-no-such", now.Add(3*time.Hour)); !errors.Is(err, ErrForwardNotFound) {
		t.Errorf("unknown id: err = %v, want ErrForwardNotFound", err)
	}
}

func TestAmendForwardSettleAtTerminalStates(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	// Settled.
	mustBookForwardLocked(t, l, forwardBooking("fwd-amend-settled", now.Add(time.Hour)), now)
	mustFundForwardPayer(t, l, 100000, "USD")
	if _, err := l.settleForwardLocked("fwd-amend-settled", now.Add(2*time.Hour)); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if _, err := l.AmendForwardSettleAt("fwd-amend-settled", now.Add(3*time.Hour)); !errors.Is(err, ErrForwardNotBooked) {
		t.Errorf("settled: err = %v, want ErrForwardNotBooked", err)
	}
	// Cancelled.
	mustBookForwardLocked(t, l, forwardBooking("fwd-amend-cancelled", now.Add(time.Hour)), now)
	if _, err := l.CancelForward("fwd-amend-cancelled"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := l.AmendForwardSettleAt("fwd-amend-cancelled", now.Add(3*time.Hour)); !errors.Is(err, ErrForwardNotBooked) {
		t.Errorf("cancelled: err = %v, want ErrForwardNotBooked", err)
	}
	// Lapsed: persisted as cancelled/expired by an explicit sweep,
	// and the amendment reports expired, never reviving it.
	mustBookForwardLocked(t, l, forwardBooking("fwd-amend-lapsed", now.Add(time.Hour)), now)
	if _, err := l.SetForwardSettlementGracePeriod(10 * time.Minute); err != nil {
		t.Fatalf("SetForwardSettlementGracePeriod: %v", err)
	}
	if expired := l.ExpireForwards(now.Add(2 * time.Hour)); expired != 1 {
		t.Fatalf("ExpireForwards: expired = %d, want 1", expired)
	}
	if _, err := l.AmendForwardSettleAt("fwd-amend-lapsed", now.Add(5*time.Hour)); !errors.Is(err, ErrForwardExpired) {
		t.Errorf("lapsed: err = %v, want ErrForwardExpired", err)
	}
	stored, ok := l.Forward("fwd-amend-lapsed")
	if !ok || stored.Status != ForwardCancelled || stored.CancelReason != ForwardCancelExpired {
		t.Errorf("lapsed contract = %+v, %v, want cancelled/expired", stored, ok)
	}
}

// TestAmendForwardSettleAtOnlyValueDateMoves: the amendment is a
// value-date revision, never a re-price — every other field,
// including the booking provenance, is untouched.
func TestAmendForwardSettleAtOnlyValueDateMoves(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	before := mustBookForwardLocked(t, l, forwardBooking("fwd-amend-fields", now.Add(time.Hour)), now)
	versionBefore := l.Version()
	got, err := l.AmendForwardSettleAt("fwd-amend-fields", now.Add(4*time.Hour))
	if err != nil {
		t.Fatalf("AmendForwardSettleAt: %v", err)
	}
	if got.ID != before.ID || got.FromAccount != before.FromAccount || got.ToAccount != before.ToAccount ||
		got.FromCurrency != before.FromCurrency || got.ToCurrency != before.ToCurrency ||
		got.RateNum != before.RateNum || got.RateDen != before.RateDen ||
		got.RateSnapshotVersion != before.RateSnapshotVersion ||
		got.NotionalCents != before.NotionalCents || got.NotionalCapCents != before.NotionalCapCents ||
		got.FXSide != before.FXSide || got.Status != before.Status ||
		!got.BookedAt.Equal(before.BookedAt) || got.BookedVersion != before.BookedVersion {
		t.Errorf("amended contract = %+v, want only settle_at moved from %+v", got, before)
	}
	if !got.SettleAt.Equal(now.Add(4 * time.Hour)) {
		t.Errorf("settle_at = %v, want +4h", got.SettleAt)
	}
	if l.Version() != versionBefore {
		t.Error("version moved: amendment is off-journal")
	}
}

func TestAmendForwardSettleAtNoop(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithFXAccount("fx"))
	now := time.Now().UTC()
	mustBookForwardLocked(t, l, forwardBooking("fwd-amend-noop", now.Add(time.Hour)), now)
	versionBefore := l.Version()
	got, err := l.AmendForwardSettleAt("fwd-amend-noop", now.Add(time.Hour))
	if err != nil {
		t.Fatalf("AmendForwardSettleAt same value: %v", err)
	}
	if !got.SettleAt.Equal(now.Add(time.Hour)) || got.Status != ForwardBooked {
		t.Errorf("no-op contract = %+v, want original value date booked", got)
	}
	if l.Version() != versionBefore {
		t.Error("version moved: amendment is off-journal")
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if events := auditEventsByOp(t, dir)["fx_forward_settle_at_amended"]; len(events) != 0 {
		t.Errorf("amend events = %d, want 0 for a no-op", len(events))
	}
}

func TestAmendForwardSettleAtAudit(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithFXAccount("fx"))
	now := time.Now().UTC()
	before := mustBookForwardLocked(t, l, forwardBooking("fwd-amend-audit", now.Add(time.Hour)), now)
	versionBefore := l.Version()
	newSettleAt := now.Add(5 * time.Hour)
	ctx := ContextWithActor(context.Background(), "ops-alice")
	got, err := l.AmendForwardSettleAtCtx(ctx, "fwd-amend-audit", newSettleAt)
	if err != nil {
		t.Fatalf("AmendForwardSettleAtCtx: %v", err)
	}
	if !got.SettleAt.Equal(newSettleAt) {
		t.Errorf("settle_at = %v, want %v", got.SettleAt, newSettleAt)
	}
	if l.Version() != versionBefore {
		t.Error("version moved: amendment is off-journal")
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["fx_forward_settle_at_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
	if events[0].Actor != "ops-alice" {
		t.Errorf("event actor = %q, want ops-alice", events[0].Actor)
	}
	d := events[0].Details
	if d["forward_id"] != "fwd-amend-audit" ||
		d["previous_settle_at"] != before.SettleAt.UTC().Format(time.RFC3339) ||
		d["settle_at"] != newSettleAt.UTC().Format(time.RFC3339) {
		t.Errorf("event details = %+v, want fwd-amend-audit %v->%v", d,
			before.SettleAt.UTC().Format(time.RFC3339), newSettleAt.UTC().Format(time.RFC3339))
	}
}

// TestAmendForwardSettleAtDefaultActor: without a ctx actor the
// audit event keeps the entrypoint name, along
// SetForwardSettlementGracePeriodCtx (the HTTP layer defaults a
// missing x-ledger-actor header to "system").
func TestAmendForwardSettleAtDefaultActor(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithFXAccount("fx"))
	now := time.Now().UTC()
	mustBookForwardLocked(t, l, forwardBooking("fwd-amend-actor", now.Add(time.Hour)), now)
	if _, err := l.AmendForwardSettleAt("fwd-amend-actor", now.Add(2*time.Hour)); err != nil {
		t.Fatalf("AmendForwardSettleAt: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["fx_forward_settle_at_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
	if events[0].Actor != "AmendForwardSettleAt" {
		t.Errorf("event actor = %q, want AmendForwardSettleAt", events[0].Actor)
	}
}

func TestAmendForwardSettleAtSnapshotRoundTrip(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	mustBookForwardLocked(t, l, forwardBooking("fwd-amend-snap", now.Add(time.Hour)), now)
	newSettleAt := now.Add(6 * time.Hour)
	if _, err := l.AmendForwardSettleAt("fwd-amend-snap", newSettleAt); err != nil {
		t.Fatalf("AmendForwardSettleAt: %v", err)
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
	c, ok := restored.Forward("fwd-amend-snap")
	if !ok || !c.SettleAt.Equal(newSettleAt) || c.Status != ForwardBooked {
		t.Errorf("restored contract = %+v, %v, want booked with settle_at %v", c, ok, newSettleAt)
	}
}
