package ledger

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

// TestSetMaxHoldDurationOverCapRejected pins the enforcement: a hold
// whose expiry lies strictly after now + the live cap is rejected
// with ErrHoldExceedsMaxDuration, records nothing, reserves nothing,
// and bumps neither the version nor the available balance.
func TestSetMaxHoldDurationOverCapRejected(t *testing.T) {
	l := New()
	fundAccount(t, l, "card", 10000, "")
	if err := l.SetMaxHoldDuration(24 * time.Hour); err != nil {
		t.Fatalf("SetMaxHoldDuration(24h): %v", err)
	}
	before := l.version
	if _, _, err := l.Hold(Hold{ID: "h-over", Account: "card", AmountCents: 1000, ExpiresAt: time.Now().Add(48 * time.Hour)}); !errors.Is(err, ErrHoldExceedsMaxDuration) {
		t.Fatalf("over-cap Hold err = %v, want ErrHoldExceedsMaxDuration", err)
	}
	if _, ok := l.GetHold("h-over"); ok {
		t.Error("rejected hold was recorded")
	}
	if got := l.Available("card"); got != 10000 {
		t.Errorf("Available = %d, want 10000 (rejected hold reserves nothing)", got)
	}
	if l.version != before {
		t.Error("rejected hold bumped the version")
	}
}

// TestSetMaxHoldDurationBoundaryExact pins the inclusive boundary: a
// hold expiring exactly at now + max is admitted. The expiry is
// computed just before the call, so the hold's own now is a hair
// later and now + max sits a hair past the expiry — After is false
// and the hold passes, deterministically.
func TestSetMaxHoldDurationBoundaryExact(t *testing.T) {
	l := New()
	fundAccount(t, l, "card", 10000, "")
	if err := l.SetMaxHoldDuration(24 * time.Hour); err != nil {
		t.Fatalf("SetMaxHoldDuration(24h): %v", err)
	}
	if _, _, err := l.Hold(Hold{ID: "h-edge", Account: "card", AmountCents: 1000, ExpiresAt: time.Now().Add(24 * time.Hour)}); err != nil {
		t.Fatalf("boundary Hold: %v", err)
	}
	if got := l.Available("card"); got != 9000 {
		t.Errorf("Available = %d, want 9000 (boundary hold reserves)", got)
	}
}

// TestSetMaxHoldDurationReplayUnaffectedByLaterCap pins the replay
// ordering: a hold placed before the cap, replayed by idempotency
// key after the cap tightened past its expiry, returns the original
// with duplicate == true and reserves nothing new — the replay must
// not fail on a cap that landed after the original hold.
func TestSetMaxHoldDurationReplayUnaffectedByLaterCap(t *testing.T) {
	l := New()
	fundAccount(t, l, "card", 10000, "")
	orig, _, err := l.Hold(Hold{ID: "h-replay", Account: "card", AmountCents: 1000, ExpiresAt: time.Now().Add(48 * time.Hour), IdempotencyKey: "k-replay"})
	if err != nil {
		t.Fatalf("pre-cap Hold: %v", err)
	}
	if err := l.SetMaxHoldDuration(24 * time.Hour); err != nil {
		t.Fatalf("SetMaxHoldDuration(24h): %v", err)
	}
	replayed, dup, err := l.Hold(Hold{ID: "h-replay-2", Account: "card", AmountCents: 1000, ExpiresAt: time.Now().Add(48 * time.Hour), IdempotencyKey: "k-replay"})
	if err != nil || !dup {
		t.Fatalf("replay Hold = dup=%v err=%v, want dup=true err=nil", dup, err)
	}
	if replayed.ID != orig.ID {
		t.Errorf("replay returned %q, want the original %q", replayed.ID, orig.ID)
	}
	if got := l.Available("card"); got != 9000 {
		t.Errorf("Available = %d, want 9000 (replay reserves nothing new)", got)
	}
}

// TestSetMaxHoldDurationTightenDoesNotExpireLiveHold pins
// future-only tightening: after the cap is cut below a live hold's
// expiry, the hold keeps its own ExpiresAt, still reserves its
// amount, and a sweep past the new cap but before its own expiry
// does not expire it.
func TestSetMaxHoldDurationTightenDoesNotExpireLiveHold(t *testing.T) {
	l := New()
	fundAccount(t, l, "card", 10000, "")
	expiry := time.Now().Add(48 * time.Hour)
	if _, _, err := l.Hold(Hold{ID: "h-live", Account: "card", AmountCents: 1000, ExpiresAt: expiry}); err != nil {
		t.Fatalf("pre-cap Hold: %v", err)
	}
	if err := l.SetMaxHoldDuration(time.Hour); err != nil {
		t.Fatalf("SetMaxHoldDuration(1h): %v", err)
	}
	kept, ok := l.GetHold("h-live")
	if !ok {
		t.Fatalf("GetHold(h-live) missing after tightening")
	}
	if !kept.ExpiresAt.Equal(expiry) {
		t.Errorf("live hold expiry = %v, want its own %v (tightening must not move it)", kept.ExpiresAt, expiry)
	}
	if got := l.Available("card"); got != 9000 {
		t.Errorf("Available = %d, want 9000 (live hold still reserves)", got)
	}
	if marked := l.ExpireHoldsAt(time.Now().Add(2 * time.Hour)); marked != 0 {
		t.Fatalf("sweep at +2h marked %d, want 0 (the hold expires on its own 48h clock)", marked)
	}
	if kept, _ := l.GetHold("h-live"); kept.Status != HoldStatusActive {
		t.Errorf("live hold status = %q after +2h sweep, want active", kept.Status)
	}
}

// TestSetMaxHoldDurationLoosenDoesNotExtendHold pins future-only
// loosening: raising the cap after a hold was placed does not move
// the hold's expiry — a sweep past its own expiry still expires
// it, even though the new cap would have admitted a longer hold.
func TestSetMaxHoldDurationLoosenDoesNotExtendHold(t *testing.T) {
	l := New()
	fundAccount(t, l, "card", 10000, "")
	if _, _, err := l.Hold(Hold{ID: "h-short", Account: "card", AmountCents: 1000, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if err := l.SetMaxHoldDuration(24 * time.Hour); err != nil {
		t.Fatalf("SetMaxHoldDuration(24h): %v", err)
	}
	if marked := l.ExpireHoldsAt(time.Now().Add(2 * time.Hour)); marked != 1 {
		t.Fatalf("sweep at +2h marked %d, want 1 (loosening must not extend the hold)", marked)
	}
	if kept, _ := l.GetHold("h-short"); kept.Status != HoldStatusExpired {
		t.Errorf("hold status = %q after +2h sweep, want expired", kept.Status)
	}
}

// TestSetMaxHoldDurationInvalidRejected pins the fail-closed gate:
// a negative cap is rejected with ErrInvalidMaxHoldDuration, the
// live value is left untouched, and no amendment event is written.
// Zero is not an error — it is the off switch (unlimited).
func TestSetMaxHoldDurationInvalidRejected(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	for _, d := range []time.Duration{-time.Hour, -24 * time.Hour, -time.Nanosecond} {
		if err := l.SetMaxHoldDuration(d); !errors.Is(err, ErrInvalidMaxHoldDuration) {
			t.Fatalf("SetMaxHoldDuration(%v) = %v, want ErrInvalidMaxHoldDuration", d, err)
		}
	}
	if got := l.MaxHoldDuration(); got != 0 {
		t.Fatalf("MaxHoldDuration() after rejected revisions = %v, want 0 (unlimited)", got)
	}
	// Zero disables the cap and is accepted.
	if err := l.SetMaxHoldDuration(0); err != nil {
		t.Fatalf("SetMaxHoldDuration(0): %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := len(auditEventsByOp(t, dir)["hold_max_duration_amended"]); n != 0 {
		t.Fatalf("rejected revisions wrote %d amendment events, want 0", n)
	}
}

// TestSetMaxHoldDurationSameValueNoop pins the no-op: revising to
// the current cap returns nil and writes no audit event — both for
// the default (0) and after a real revision.
func TestSetMaxHoldDurationSameValueNoop(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	if err := l.SetMaxHoldDuration(0); err != nil {
		t.Fatalf("SetMaxHoldDuration(0) same value: %v", err)
	}
	if err := l.SetMaxHoldDuration(24 * time.Hour); err != nil {
		t.Fatalf("SetMaxHoldDuration(24h): %v", err)
	}
	if err := l.SetMaxHoldDuration(24 * time.Hour); err != nil {
		t.Fatalf("SetMaxHoldDuration(24h) same value: %v", err)
	}
	waitAuditWritten(t, al, 1)
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := len(auditEventsByOp(t, dir)["hold_max_duration_amended"]); n != 1 {
		t.Fatalf("amendment events = %d, want 1 (only the real revision)", n)
	}
}

// TestSetMaxHoldDurationCtxActorAttribution pins LG-73 attribution
// on the Ctx variant: the operator identity replaces the entrypoint
// name, while the plain setter keeps it. The amendment details
// carry the previous and new caps as duration strings.
func TestSetMaxHoldDurationCtxActorAttribution(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	ctx := ContextWithActor(context.Background(), "ops-cap")
	if err := l.SetMaxHoldDurationCtx(ctx, 24*time.Hour); err != nil {
		t.Fatalf("SetMaxHoldDurationCtx: %v", err)
	}
	if err := l.SetMaxHoldDuration(0); err != nil {
		t.Fatalf("SetMaxHoldDuration: %v", err)
	}
	waitAuditWritten(t, al, 2)
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	amended := auditEventsByOp(t, dir)["hold_max_duration_amended"]
	if len(amended) != 2 {
		t.Fatalf("amendment events = %d, want 2", len(amended))
	}
	if amended[0].Actor != "ops-cap" {
		t.Errorf("Ctx amendment actor = %q, want ops-cap", amended[0].Actor)
	}
	if amended[0].Details["previous_max_hold_duration"] != (0 * time.Second).String() {
		t.Errorf("Ctx amendment previous = %v, want %q", amended[0].Details["previous_max_hold_duration"], (0 * time.Second).String())
	}
	if amended[0].Details["max_hold_duration"] != (24 * time.Hour).String() {
		t.Errorf("Ctx amendment cap = %v, want %q", amended[0].Details["max_hold_duration"], (24 * time.Hour).String())
	}
	if amended[1].Actor != "SetMaxHoldDuration" {
		t.Errorf("plain amendment actor = %q, want SetMaxHoldDuration", amended[1].Actor)
	}
	if amended[1].Details["previous_max_hold_duration"] != (24 * time.Hour).String() {
		t.Errorf("plain amendment previous = %v, want %q", amended[1].Details["previous_max_hold_duration"], (24 * time.Hour).String())
	}
	if amended[1].Details["max_hold_duration"] != (0 * time.Second).String() {
		t.Errorf("plain amendment cap = %v, want %q", amended[1].Details["max_hold_duration"], (0 * time.Second).String())
	}
}

// TestMaxHoldDurationGetterFollowsLiveValue pins the live getter:
// the default is 0 (unlimited), a runtime revision moves it, and
// revising back to 0 re-disables the cap.
func TestMaxHoldDurationGetterFollowsLiveValue(t *testing.T) {
	if got := New().MaxHoldDuration(); got != 0 {
		t.Fatalf("default MaxHoldDuration() = %v, want 0 (unlimited)", got)
	}
	l := New()
	if err := l.SetMaxHoldDuration(48 * time.Hour); err != nil {
		t.Fatalf("SetMaxHoldDuration(48h): %v", err)
	}
	if got := l.MaxHoldDuration(); got != 48*time.Hour {
		t.Fatalf("MaxHoldDuration() after revision = %v, want 48h", got)
	}
	if err := l.SetMaxHoldDuration(0); err != nil {
		t.Fatalf("SetMaxHoldDuration(0): %v", err)
	}
	if got := l.MaxHoldDuration(); got != 0 {
		t.Fatalf("MaxHoldDuration() after disabling = %v, want 0", got)
	}
	// With the cap disabled again, an over-long hold is admitted.
	fundAccount(t, l, "card", 10000, "")
	if _, _, err := l.Hold(Hold{ID: "h-unlimited", Account: "card", AmountCents: 1000, ExpiresAt: time.Now().Add(365 * 24 * time.Hour)}); err != nil {
		t.Fatalf("Hold after disabling cap: %v", err)
	}
}

// TestSetMaxHoldDurationSnapshotRestoresDefault pins the disaster-
// recovery guard: snapshots carry no hold cap, so a restored ledger
// reports the default (unlimited) — the amendment must be
// re-applied.
func TestSetMaxHoldDurationSnapshotRestoresDefault(t *testing.T) {
	src := New()
	if err := src.SetMaxHoldDuration(24 * time.Hour); err != nil {
		t.Fatalf("SetMaxHoldDuration(24h): %v", err)
	}
	var buf bytes.Buffer
	if err := src.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if got := restored.MaxHoldDuration(); got != 0 {
		t.Fatalf("restored MaxHoldDuration() = %v, want 0 (unlimited default)", got)
	}
}

// TestSetMaxHoldDurationDisputeHoldExempt pins the verified
// exemption: with the cap tightened to 1 hour, raising a dispute
// still succeeds — the 120-day freeze hold bypasses the cap through
// holdLockedUncapped, so a hold-length guard can never block the
// dispute risk-control flow.
func TestSetMaxHoldDurationDisputeHoldExempt(t *testing.T) {
	l := seedDisputeLedger(t)
	if err := l.SetMaxHoldDuration(time.Hour); err != nil {
		t.Fatalf("SetMaxHoldDuration(1h): %v", err)
	}
	d, _, err := l.RaiseDispute(Dispute{
		ID:          "d-cap",
		TransferID:  "t1",
		ReasonCode:  DisputeReasonUnauthorized,
		AmountCents: 6000,
	})
	if err != nil {
		t.Fatalf("RaiseDispute under a 1h cap: %v (the dispute freeze hold must be exempt)", err)
	}
	if d.HoldID != "dispute/d-cap" {
		t.Errorf("hold ID = %q, want dispute/d-cap", d.HoldID)
	}
	hold, ok := l.GetHold(d.HoldID)
	if !ok {
		t.Fatalf("dispute freeze hold %s missing", d.HoldID)
	}
	if hold.ExpiresAt.Before(time.Now().Add(119 * 24 * time.Hour)) {
		t.Errorf("dispute freeze hold expiry = %v, want ~120d out (the fixed TTL, not the cap)", hold.ExpiresAt)
	}
	if got := l.AvailableIn("payee", "USD"); got != 4000 {
		t.Errorf("payee available = %d, want 4000 (freeze still reserves)", got)
	}
}
