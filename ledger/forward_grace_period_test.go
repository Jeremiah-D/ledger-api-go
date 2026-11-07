package ledger

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestSetForwardSettlementGracePeriodInvalidRejected pins the
// fail-closed gate: a non-positive grace is rejected and the live
// grace is left untouched. Zero is rejected, not a "close the
// window" state — a zero grace would lapse a contract the instant
// SettleAt arrives, while settlement itself requires now >=
// SettleAt, so the contract could never settle at all: a semantic
// trap, not a meaningful "off" state.
func TestSetForwardSettlementGracePeriodInvalidRejected(t *testing.T) {
	l := forwardTestLedger()
	if got := l.ForwardSettlementGracePeriod(); got != ForwardSettlementGracePeriod {
		t.Fatalf("default grace = %v, want %v", got, ForwardSettlementGracePeriod)
	}
	for _, d := range []time.Duration{0, -time.Second, -24 * time.Hour} {
		prev, err := l.SetForwardSettlementGracePeriod(d)
		if !errors.Is(err, ErrInvalidForwardSettlementGracePeriod) {
			t.Fatalf("SetForwardSettlementGracePeriod(%v) = (%v, %v), want ErrInvalidForwardSettlementGracePeriod", d, prev, err)
		}
		if prev != ForwardSettlementGracePeriod {
			t.Fatalf("SetForwardSettlementGracePeriod(%v) prev = %v, want %v (live)", d, prev, ForwardSettlementGracePeriod)
		}
	}
	// The Ctx variant rejects the same way.
	if prev, err := l.SetForwardSettlementGracePeriodCtx(context.Background(), 0); !errors.Is(err, ErrInvalidForwardSettlementGracePeriod) || prev != ForwardSettlementGracePeriod {
		t.Fatalf("SetForwardSettlementGracePeriodCtx(0) = (%v, %v), want (%v, ErrInvalidForwardSettlementGracePeriod)", prev, err, ForwardSettlementGracePeriod)
	}
	if got := l.ForwardSettlementGracePeriod(); got != ForwardSettlementGracePeriod {
		t.Fatalf("grace after rejected revisions = %v, want %v", got, ForwardSettlementGracePeriod)
	}
}

// TestSetForwardSettlementGracePeriodSameValueNoop pins the no-op:
// revising to the current grace returns the live value with a nil
// error and writes no audit event.
func TestSetForwardSettlementGracePeriodSameValueNoop(t *testing.T) {
	auditDir := t.TempDir()
	al, err := NewAuditLog(auditDir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithFXAccount("fx"))
	prev, err := l.SetForwardSettlementGracePeriod(ForwardSettlementGracePeriod)
	if err != nil {
		t.Fatalf("SetForwardSettlementGracePeriod(%v) same value: %v", ForwardSettlementGracePeriod, err)
	}
	if prev != ForwardSettlementGracePeriod {
		t.Fatalf("prev = %v, want %v", prev, ForwardSettlementGracePeriod)
	}
	waitAuditWritten(t, al, 1) // the log's startup alert only
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := len(auditEventsByOp(t, auditDir)["fx_forward_grace_period_retuned"]); n != 0 {
		t.Fatalf("same-value revision wrote %d retune events, want 0", n)
	}
}

// TestForwardGracePeriodWidenRescuesUntouchedContract pins the
// rescue semantics: a booked contract observed as expired (the
// read-only Reconcile view) but never touched by a sweep, a
// settle, or a read that persists the lapse, is re-admitted when
// the grace widens before the next decision — lazy expiry
// recomputes on every call.
//
// NOTE on the probe: the first "expired" observation must be
// read-only. A settle attempt would run the lazy sweep first and
// persist the lapse, and a persisted lapse is terminal (see
// TestForwardGracePeriodWidenDoesNotRevivePersistedLapse) — so
// the pre-revision observation here is Reconcile, not a settle.
func TestForwardGracePeriodWidenRescuesUntouchedContract(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	mustBookForwardLocked(t, l, forwardBooking("fwd-rescue", now.Add(time.Hour)), now)
	mustFundForwardPayer(t, l, 100000, "USD")
	at := now.Add(25 * time.Hour) // past the 24h grace, inside 48h
	rep := l.Reconcile(at)
	if rep.ForwardBook.Expired != 1 || rep.ForwardBook.Booked != 0 {
		t.Fatalf("forward_book before revision = %+v, want expired=1 booked=0", rep.ForwardBook)
	}
	prev, err := l.SetForwardSettlementGracePeriod(48 * time.Hour)
	if err != nil || prev != ForwardSettlementGracePeriod {
		t.Fatalf("SetForwardSettlementGracePeriod(48h) = (%v, %v), want (24h, nil)", prev, err)
	}
	if _, err := l.settleForwardLocked("fwd-rescue", at); err != nil {
		t.Fatalf("settle after widening: %v", err)
	}
	c, ok := l.Forward("fwd-rescue")
	if !ok || c.Status != ForwardSettled {
		t.Fatalf("contract after settle = %+v, %v; want settled", c, ok)
	}
	// The read-only view agrees: after the revision the same
	// instant counts the (already settled) contract as settled,
	// not expired.
	rep = l.Reconcile(at)
	if rep.ForwardBook.Settled != 1 || rep.ForwardBook.Expired != 0 {
		t.Fatalf("forward_book after revision+settle = %+v, want settled=1 expired=0", rep.ForwardBook)
	}
}

// TestForwardGracePeriodWidenDoesNotRevivePersistedLapse pins the
// terminal-state rule: once a sweep has persisted a contract as
// cancelled/expired, widening the grace never revives it.
func TestForwardGracePeriodWidenDoesNotRevivePersistedLapse(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	mustBookForwardLocked(t, l, forwardBooking("fwd-dead", now.Add(time.Hour)), now)
	mustFundForwardPayer(t, l, 100000, "USD")
	at := now.Add(25 * time.Hour)
	if n := l.ExpireForwards(at); n != 1 {
		t.Fatalf("ExpireForwards = %d, want 1", n)
	}
	if _, err := l.SetForwardSettlementGracePeriod(48 * time.Hour); err != nil {
		t.Fatalf("SetForwardSettlementGracePeriod(48h): %v", err)
	}
	if _, err := l.settleForwardLocked("fwd-dead", at); !errors.Is(err, ErrForwardExpired) {
		t.Fatalf("settle after persisted lapse + widening = %v, want ErrForwardExpired", err)
	}
	c, ok := l.Forward("fwd-dead")
	if !ok || c.Status != ForwardCancelled || c.CancelReason != ForwardCancelExpired {
		t.Fatalf("contract = %+v, %v; want cancelled/expired", c, ok)
	}
}

// TestForwardGracePeriodTightenAcceleratesLapse pins the tighten
// direction: a contract inside the old grace but outside the new
// one lapses on the next sweep — and the sweep, not the wall
// clock, is what persists it (the Forward getter reads the wall
// clock, which cannot be fast-forwarded, so the locked core is
// driven directly, along TestForwardGetterAppliesLazyExpiry).
func TestForwardGracePeriodTightenAcceleratesLapse(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	mustBookForwardLocked(t, l, forwardBooking("fwd-tight", now.Add(time.Hour)), now)
	if _, err := l.SetForwardSettlementGracePeriod(time.Hour); err != nil {
		t.Fatalf("SetForwardSettlementGracePeriod(1h): %v", err)
	}
	at := now.Add(3 * time.Hour) // inside old 24h grace, outside new 1h
	l.mu.Lock()
	n := l.expireForwardsLocked(at)
	l.mu.Unlock()
	if n != 1 {
		t.Fatalf("expireForwardsLocked = %d, want 1", n)
	}
	c, ok := l.Forward("fwd-tight")
	if !ok || c.Status != ForwardCancelled || c.CancelReason != ForwardCancelExpired {
		t.Fatalf("contract = %+v, %v; want cancelled/expired", c, ok)
	}
}

// TestForwardGracePeriodSettlePathUsesLiveGrace pins that the
// settle path's lazy expiry judges against the live grace: with
// the grace tightened, a settle attempt inside the old grace
// still reports expired — the sweep the settle runs first reads
// the revised value.
func TestForwardGracePeriodSettlePathUsesLiveGrace(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	mustBookForwardLocked(t, l, forwardBooking("fwd-settle-live", now.Add(time.Hour)), now)
	mustFundForwardPayer(t, l, 100000, "USD")
	if _, err := l.SetForwardSettlementGracePeriod(time.Hour); err != nil {
		t.Fatalf("SetForwardSettlementGracePeriod(1h): %v", err)
	}
	if _, err := l.settleForwardLocked("fwd-settle-live", now.Add(3*time.Hour)); !errors.Is(err, ErrForwardExpired) {
		t.Fatalf("settle under tightened grace = %v, want ErrForwardExpired", err)
	}
}

// TestForwardGracePeriodRollWindowUsesLiveGrace pins that the
// auto-rollover window's anchor (lastSettleable = SettleAt +
// grace) moves with the revision: with the default 24h grace a
// settle 1h past SettleAt sits 23h inside a 36h roll window and
// rolls; with the grace widened to 48h the same settle sits 47h
// out and settles normally.
func TestForwardGracePeriodRollWindowUsesLiveGrace(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	if err := l.EnableForwardRollover(36*time.Hour, 3); err != nil {
		t.Fatalf("EnableForwardRollover: %v", err)
	}
	mustSetRate(t, l, "USD", "EUR", 12, 10) // market 1.2, contracts locked 1.1
	mustFundForwardPayer(t, l, 100000, "USD")
	mustBookForwardLocked(t, l, forwardBooking("fwd-roll-a", now.Add(2*time.Hour)), now)
	mustBookForwardLocked(t, l, forwardBooking("fwd-roll-b", now.Add(2*time.Hour)), now)
	at := now.Add(3 * time.Hour) // 1h past SettleAt
	if _, err := l.settleForwardLocked("fwd-roll-a", at); !errors.Is(err, ErrForwardRolled) {
		t.Fatalf("settle under default grace = %v, want ErrForwardRolled", err)
	}
	if _, err := l.SetForwardSettlementGracePeriod(48 * time.Hour); err != nil {
		t.Fatalf("SetForwardSettlementGracePeriod(48h): %v", err)
	}
	if _, err := l.settleForwardLocked("fwd-roll-b", at); err != nil {
		t.Fatalf("settle after widening: %v, want a normal settlement", err)
	}
	c, ok := l.Forward("fwd-roll-b")
	if !ok || c.Status != ForwardSettled {
		t.Fatalf("fwd-roll-b = %+v, %v; want settled", c, ok)
	}
	if _, ok := l.Forward("roll/fwd-roll-a/1"); !ok {
		t.Fatal("roll/fwd-roll-a/1 missing: the first settle should have rolled")
	}
}

// TestForwardGracePeriodReconcileUsesLiveGrace pins that the
// Reconcile forward_book section judges against the live grace:
// the same instant counts a contract expired before the revision
// and booked after it — with no sweep in between, so the report
// is a pure read of the live value.
func TestForwardGracePeriodReconcileUsesLiveGrace(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	mustBookForwardLocked(t, l, forwardBooking("fwd-rep", now.Add(time.Hour)), now)
	at := now.Add(25 * time.Hour)
	rep := l.Reconcile(at)
	if rep.ForwardBook.Expired != 1 || rep.ForwardBook.Booked != 0 {
		t.Fatalf("forward_book before revision = %+v, want expired=1 booked=0", rep.ForwardBook)
	}
	if _, err := l.SetForwardSettlementGracePeriod(48 * time.Hour); err != nil {
		t.Fatalf("SetForwardSettlementGracePeriod(48h): %v", err)
	}
	rep = l.Reconcile(at)
	if rep.ForwardBook.Expired != 0 || rep.ForwardBook.Booked != 1 {
		t.Fatalf("forward_book after revision = %+v, want expired=0 booked=1", rep.ForwardBook)
	}
	// Still booked, never persisted: a sweep at the same instant
	// now finds nothing to lapse.
	if n := l.ExpireForwards(at); n != 0 {
		t.Fatalf("ExpireForwards after revision = %d, want 0", n)
	}
}

// TestForwardGracePeriodNewBookingUsesLiveGrace pins that a
// contract booked after the revision settles under the live
// grace: the grace acts at the settle/lapse decision points,
// not in the booking terms, so a new booking inherits the
// revised window.
func TestForwardGracePeriodNewBookingUsesLiveGrace(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	if _, err := l.SetForwardSettlementGracePeriod(48 * time.Hour); err != nil {
		t.Fatalf("SetForwardSettlementGracePeriod(48h): %v", err)
	}
	mustBookForwardLocked(t, l, forwardBooking("fwd-new", now.Add(time.Hour)), now)
	mustFundForwardPayer(t, l, 100000, "USD")
	if _, err := l.settleForwardLocked("fwd-new", now.Add(25*time.Hour)); err != nil {
		t.Fatalf("settle of post-revision booking: %v", err)
	}
}

// TestSetForwardSettlementGracePeriodAuditDetailsExact pins the
// audit payload: exactly the two duration strings, previous and
// current — no contract content ever rides in a retune event.
func TestSetForwardSettlementGracePeriodAuditDetailsExact(t *testing.T) {
	auditDir := t.TempDir()
	al, err := NewAuditLog(auditDir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithFXAccount("fx"))
	if _, err := l.SetForwardSettlementGracePeriod(48 * time.Hour); err != nil {
		t.Fatalf("SetForwardSettlementGracePeriod(48h): %v", err)
	}
	if _, err := l.SetForwardSettlementGracePeriod(12 * time.Hour); err != nil {
		t.Fatalf("SetForwardSettlementGracePeriod(12h): %v", err)
	}
	waitAuditWritten(t, al, 3) // alert + 2 retunes
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	retuned := auditEventsByOp(t, auditDir)["fx_forward_grace_period_retuned"]
	if len(retuned) != 2 {
		t.Fatalf("fx_forward_grace_period_retuned events = %d, want 2", len(retuned))
	}
	want := []map[string]any{
		{"previous_grace_period": "24h0m0s", "current_grace_period": "48h0m0s"},
		{"previous_grace_period": "48h0m0s", "current_grace_period": "12h0m0s"},
	}
	for i, ev := range retuned {
		if len(ev.Details) != 2 {
			t.Fatalf("retune[%d] details keys = %v, want exactly previous_grace_period + current_grace_period", i, ev.Details)
		}
		for k, v := range want[i] {
			if ev.Details[k] != v {
				t.Errorf("retune[%d] details[%q] = %v, want %v", i, k, ev.Details[k], v)
			}
		}
	}
}

// TestSetForwardSettlementGracePeriodCtxActorAttribution pins the
// attribution on the Ctx variant: the operator identity replaces
// the default actor, while the plain setter keeps the "system"
// default.
func TestSetForwardSettlementGracePeriodCtxActorAttribution(t *testing.T) {
	auditDir := t.TempDir()
	al, err := NewAuditLog(auditDir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithFXAccount("fx"))
	ctx := ContextWithActor(context.Background(), "ops-grace")
	if prev, err := l.SetForwardSettlementGracePeriodCtx(ctx, 48*time.Hour); err != nil || prev != ForwardSettlementGracePeriod {
		t.Fatalf("SetForwardSettlementGracePeriodCtx(48h) = (%v, %v), want (24h, nil)", prev, err)
	}
	if prev, err := l.SetForwardSettlementGracePeriod(12 * time.Hour); err != nil || prev != 48*time.Hour {
		t.Fatalf("SetForwardSettlementGracePeriod(12h) = (%v, %v), want (48h, nil)", prev, err)
	}
	waitAuditWritten(t, al, 3) // alert + 2 retunes
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	retuned := auditEventsByOp(t, auditDir)["fx_forward_grace_period_retuned"]
	if len(retuned) != 2 {
		t.Fatalf("fx_forward_grace_period_retuned events = %d, want 2", len(retuned))
	}
	if retuned[0].Actor != "ops-grace" {
		t.Errorf("Ctx retune actor = %q, want ops-grace", retuned[0].Actor)
	}
	if retuned[1].Actor != "system" {
		t.Errorf("plain retune actor = %q, want system", retuned[1].Actor)
	}
}

// TestForwardGracePeriodGetterFollowsLiveValue pins that the
// getter tracks the live value through repeated revisions in
// both directions, including a return to the default.
func TestForwardGracePeriodGetterFollowsLiveValue(t *testing.T) {
	l := forwardTestLedger()
	for _, d := range []time.Duration{48 * time.Hour, 12 * time.Hour, 72 * time.Hour, 24 * time.Hour} {
		prev, err := l.SetForwardSettlementGracePeriod(d)
		if err != nil {
			t.Fatalf("SetForwardSettlementGracePeriod(%v): %v", d, err)
		}
		_ = prev
		if got := l.ForwardSettlementGracePeriod(); got != d {
			t.Fatalf("getter after revision to %v = %v", d, got)
		}
	}
}

// TestForwardGracePeriodConcurrentRevisionAndSettle pins that
// concurrent revisions race cleanly with concurrent settles,
// sweeps, and reconciles: every grace read is under l.mu, so
// the race detector is the assertion — either outcome for any
// single contract is legitimate under a moving grace.
func TestForwardGracePeriodConcurrentRevisionAndSettle(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	for i := 0; i < 4; i++ {
		mustBookForwardLocked(t, l, forwardBooking(fmt.Sprintf("fwd-race-%d", i), now.Add(time.Hour)), now)
	}
	mustFundForwardPayer(t, l, 1000000, "USD")
	graces := []time.Duration{time.Hour, 24 * time.Hour, 48 * time.Hour, 72 * time.Hour}
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				_, _ = l.SetForwardSettlementGracePeriod(graces[(g+i)%len(graces)])
			}
		}(g)
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("fwd-race-%d", i)
			at := now.Add(25 * time.Hour)
			// settleForwardLocked is a lock-held core: the
			// test holds l.mu around it, exactly as the public
			// SettleForward does — calling it bare from a
			// goroutine would race the setter, and that race
			// would be the test's, not the implementation's.
			l.mu.Lock()
			_, _ = l.settleForwardLocked(id, at)
			l.mu.Unlock()
			_ = l.ExpireForwards(at)
			_ = l.Reconcile(at)
			_, _ = l.ClosePeriodPreflight(at, "2026-11")
		}(i)
	}
	wg.Wait()
	got := l.ForwardSettlementGracePeriod()
	for _, g := range graces {
		if got == g {
			return
		}
	}
	t.Errorf("live grace = %v, want one of %v", got, graces)
}

// TestForwardGracePeriodSnapshotRoundTripRestoresDefault pins
// that the grace does not round-trip through snapshots: an
// export/import restores the 24h default, and the imported
// ledger's booked contracts lapse on the default grace again.
func TestForwardGracePeriodSnapshotRoundTripRestoresDefault(t *testing.T) {
	l := forwardTestLedger()
	now := time.Now().UTC()
	mustBookForwardLocked(t, l, forwardBooking("fwd-snap", now.Add(time.Hour)), now)
	if _, err := l.SetForwardSettlementGracePeriod(48 * time.Hour); err != nil {
		t.Fatalf("SetForwardSettlementGracePeriod(48h): %v", err)
	}
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	l2, err := ImportSnapshot(&buf)
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if got := l2.ForwardSettlementGracePeriod(); got != ForwardSettlementGracePeriod {
		t.Fatalf("imported grace = %v, want default %v", got, ForwardSettlementGracePeriod)
	}
	if _, ok := l2.Forward("fwd-snap"); !ok {
		t.Fatal("imported ledger lost the booked contract")
	}
	if n := l2.ExpireForwards(now.Add(25 * time.Hour)); n != 1 {
		t.Fatalf("ExpireForwards on imported ledger = %d, want 1 (default grace)", n)
	}
}
