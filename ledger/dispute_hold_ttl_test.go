package ledger

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Dispute-hold TTL runtime revision tests (LG-358). They reuse
// mustAuditLog from ledger_audit_test.go, seedDisputeLedger from
// ledger_dispute_test.go, and ReadAuditLog from the audit log reader.

// retunedDisputeHoldTTLEvents closes the audit log (which drains the
// async writer) and returns every dispute_hold_ttl_retuned event
// recorded in it. The ledger must not be used after this call.
func retunedDisputeHoldTTLEvents(t *testing.T, al *AuditLog) []AuditEvent {
	t.Helper()
	if err := al.Close(); err != nil {
		t.Fatalf("Close audit log: %v", err)
	}
	events, corrupt, err := ReadAuditLog(al.Dir(), time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt events = %d, want 0", corrupt)
	}
	var out []AuditEvent
	for _, ev := range events {
		if ev.Op == "dispute_hold_ttl_retuned" {
			out = append(out, ev)
		}
	}
	return out
}

func TestSetDisputeHoldTTLDefault(t *testing.T) {
	l := New()
	if got := l.DisputeHoldTTL(); got != 120*24*time.Hour {
		t.Fatalf("default TTL = %v, want 120 days", got)
	}
}

func TestSetDisputeHoldTTLInvalidRejectedAndKeepsLive(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	defer func() { _ = al.Close() }()

	for _, d := range []time.Duration{0, -time.Hour, -120 * 24 * time.Hour} {
		prev, err := l.SetDisputeHoldTTL(d)
		if !errors.Is(err, ErrInvalidDisputeHoldTTL) {
			t.Fatalf("SetDisputeHoldTTL(%v): err = %v, want ErrInvalidDisputeHoldTTL", d, err)
		}
		if prev != 120*24*time.Hour {
			t.Fatalf("SetDisputeHoldTTL(%v) returned %v, want the live 120 days", d, prev)
		}
		if got := l.DisputeHoldTTL(); got != 120*24*time.Hour {
			t.Fatalf("TTL = %v after rejected %v, want still 120 days", got, d)
		}
	}
	// The Ctx variant rejects the same way.
	if prev, err := l.SetDisputeHoldTTLCtx(context.Background(), 0); !errors.Is(err, ErrInvalidDisputeHoldTTL) || prev != 120*24*time.Hour {
		t.Fatalf("SetDisputeHoldTTLCtx(0) = (%v, %v), want (120 days, ErrInvalidDisputeHoldTTL)", prev, err)
	}
	// Zero is rejected here because a zero TTL would make a new
	// freeze born expired — a runtime revision never silently
	// disables the dispute freeze.
	if events := retunedDisputeHoldTTLEvents(t, al); len(events) != 0 {
		t.Fatalf("retuned events = %d, want 0 after only rejected revisions", len(events))
	}
}

func TestSetDisputeHoldTTLSameValueNoOpNoAudit(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	defer func() { _ = al.Close() }()

	prev, err := l.SetDisputeHoldTTL(120 * 24 * time.Hour)
	if err != nil {
		t.Fatalf("same-value SetDisputeHoldTTL: %v", err)
	}
	if prev != 120*24*time.Hour {
		t.Fatalf("same-value revision returned %v, want the live 120 days", prev)
	}
	// One real revision, then the same value again: only the real
	// one audits.
	if _, err := l.SetDisputeHoldTTL(60 * 24 * time.Hour); err != nil {
		t.Fatalf("SetDisputeHoldTTL(60d): %v", err)
	}
	if prev, err := l.SetDisputeHoldTTL(60 * 24 * time.Hour); err != nil || prev != 60*24*time.Hour {
		t.Fatalf("same-value SetDisputeHoldTTL(60d) = (%v, %v), want (60 days, nil)", prev, err)
	}
	if events := retunedDisputeHoldTTLEvents(t, al); len(events) != 1 {
		t.Fatalf("retuned events = %d, want 1 after the real revision plus the no-ops", len(events))
	}
}

func TestSetDisputeHoldTTLReturnsPreviousValue(t *testing.T) {
	l := New()
	prev, err := l.SetDisputeHoldTTL(60 * 24 * time.Hour)
	if err != nil {
		t.Fatalf("SetDisputeHoldTTL: %v", err)
	}
	if prev != 120*24*time.Hour {
		t.Fatalf("returned previous TTL = %v, want 120 days", prev)
	}
	if got := l.DisputeHoldTTL(); got != 60*24*time.Hour {
		t.Fatalf("TTL = %v after the revision, want 60 days", got)
	}
}

func TestSetDisputeHoldTTLNewDisputeUsesNewTTL(t *testing.T) {
	l := seedDisputeLedger(t)
	if _, err := l.SetDisputeHoldTTL(60 * 24 * time.Hour); err != nil {
		t.Fatalf("SetDisputeHoldTTL(60d): %v", err)
	}
	if _, _, err := l.RaiseDispute(Dispute{
		ID:          "d1",
		TransferID:  "t1",
		ReasonCode:  DisputeReasonUnauthorized,
		AmountCents: 6000,
	}); err != nil {
		t.Fatalf("RaiseDispute after retune: %v", err)
	}
	hold, ok := l.GetHold("dispute/d1")
	if !ok {
		t.Fatalf("dispute freeze hold dispute/d1 missing")
	}
	// The freeze now expires ~60 days out, not 120.
	now := time.Now()
	if hold.ExpiresAt.Before(now.Add(59*24*time.Hour)) || hold.ExpiresAt.After(now.Add(61*24*time.Hour)) {
		t.Errorf("dispute freeze hold expiry = %v, want ~60 days out", hold.ExpiresAt)
	}
}

func TestSetDisputeHoldTTLAlreadyRaisedUnaffected(t *testing.T) {
	l := seedDisputeLedger(t)
	if _, _, err := l.RaiseDispute(Dispute{
		ID:          "d1",
		TransferID:  "t1",
		ReasonCode:  DisputeReasonUnauthorized,
		AmountCents: 6000,
	}); err != nil {
		t.Fatalf("RaiseDispute d1: %v", err)
	}
	before, _ := l.GetHold("dispute/d1")
	// Tighten below the filed freeze: the stored hold keeps its
	// original expiry — the revision governs only future raises.
	if _, err := l.SetDisputeHoldTTL(60 * 24 * time.Hour); err != nil {
		t.Fatalf("SetDisputeHoldTTL(60d): %v", err)
	}
	if after, _ := l.GetHold("dispute/d1"); !after.ExpiresAt.Equal(before.ExpiresAt) {
		t.Errorf("d1 hold expiry moved from %v to %v, want untouched", before.ExpiresAt, after.ExpiresAt)
	}
	// A dispute raised after the revision gets the new TTL.
	if _, _, err := l.RaiseDispute(Dispute{
		ID:          "d2",
		TransferID:  "t1",
		ReasonCode:  DisputeReasonFraud,
		AmountCents: 1000,
	}); err != nil {
		t.Fatalf("RaiseDispute d2: %v", err)
	}
	hold2, _ := l.GetHold("dispute/d2")
	now := time.Now()
	if hold2.ExpiresAt.Before(now.Add(59*24*time.Hour)) || hold2.ExpiresAt.After(now.Add(61*24*time.Hour)) {
		t.Errorf("d2 hold expiry = %v, want ~60 days out", hold2.ExpiresAt)
	}
}

func TestSetDisputeHoldTTLAuditPreviousCurrentExact(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	defer func() { _ = al.Close() }()

	prev, err := l.SetDisputeHoldTTL(60 * 24 * time.Hour)
	if err != nil {
		t.Fatalf("SetDisputeHoldTTL: %v", err)
	}
	if prev != 120*24*time.Hour {
		t.Fatalf("returned previous TTL = %v, want 120 days", prev)
	}
	events := retunedDisputeHoldTTLEvents(t, al)
	if len(events) != 1 {
		t.Fatalf("retuned events = %d, want 1", len(events))
	}
	if got := events[0].Details["previous_dispute_hold_ttl"]; got != (120 * 24 * time.Hour).String() {
		t.Fatalf("previous_dispute_hold_ttl = %v, want %q", got, (120 * 24 * time.Hour).String())
	}
	if got := events[0].Details["dispute_hold_ttl"]; got != (60 * 24 * time.Hour).String() {
		t.Fatalf("dispute_hold_ttl = %v, want %q", got, (60 * 24 * time.Hour).String())
	}
}

func TestSetDisputeHoldTTLCtxActorAttribution(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	defer func() { _ = al.Close() }()

	ctx := ContextWithActor(context.Background(), "ops-ana")
	if _, err := l.SetDisputeHoldTTLCtx(ctx, 60*24*time.Hour); err != nil {
		t.Fatalf("SetDisputeHoldTTLCtx: %v", err)
	}
	// A plain call keeps the entrypoint name as the actor.
	if _, err := l.SetDisputeHoldTTL(180 * 24 * time.Hour); err != nil {
		t.Fatalf("SetDisputeHoldTTL: %v", err)
	}
	events := retunedDisputeHoldTTLEvents(t, al)
	if len(events) != 2 {
		t.Fatalf("retuned events = %d, want 2", len(events))
	}
	if events[0].Actor != "ops-ana" {
		t.Fatalf("first event Actor = %q, want ops-ana", events[0].Actor)
	}
	if got := events[0].Details["previous_dispute_hold_ttl"]; got != (120 * 24 * time.Hour).String() {
		t.Fatalf("first previous_dispute_hold_ttl = %v, want 120 days", got)
	}
	if got := events[0].Details["dispute_hold_ttl"]; got != (60 * 24 * time.Hour).String() {
		t.Fatalf("first dispute_hold_ttl = %v, want 60 days", got)
	}
	if events[1].Actor != "SetDisputeHoldTTL" {
		t.Fatalf("second event Actor = %q, want SetDisputeHoldTTL", events[1].Actor)
	}
	if got := events[1].Details["previous_dispute_hold_ttl"]; got != (60 * 24 * time.Hour).String() {
		t.Fatalf("second previous_dispute_hold_ttl = %v, want 60 days", got)
	}
	if got := events[1].Details["dispute_hold_ttl"]; got != (180 * 24 * time.Hour).String() {
		t.Fatalf("second dispute_hold_ttl = %v, want 180 days", got)
	}
}

func TestSetDisputeHoldTTLGetterFollowsLiveValue(t *testing.T) {
	l := New()
	for _, d := range []time.Duration{60 * 24 * time.Hour, 180 * 24 * time.Hour, 24 * time.Hour} {
		if _, err := l.SetDisputeHoldTTL(d); err != nil {
			t.Fatalf("SetDisputeHoldTTL(%v): %v", d, err)
		}
		if got := l.DisputeHoldTTL(); got != d {
			t.Fatalf("getter = %v after revising to %v", got, d)
		}
	}
}

func TestSetDisputeHoldTTLShortCapRaiseStillSucceeds(t *testing.T) {
	l := seedDisputeLedger(t)
	// The LG-312 cap is shorter than the dispute TTL: the dispute
	// freeze stays exempt — raising must still succeed.
	if err := l.SetMaxHoldDuration(30 * 24 * time.Hour); err != nil {
		t.Fatalf("SetMaxHoldDuration(30d): %v", err)
	}
	if _, _, err := l.RaiseDispute(Dispute{
		ID:          "d1",
		TransferID:  "t1",
		ReasonCode:  DisputeReasonUnauthorized,
		AmountCents: 6000,
	}); err != nil {
		t.Fatalf("RaiseDispute under a 30-day cap: %v (the dispute freeze must stay exempt)", err)
	}
	hold, ok := l.GetHold("dispute/d1")
	if !ok {
		t.Fatalf("dispute freeze hold dispute/d1 missing")
	}
	if hold.ExpiresAt.Before(time.Now().Add(119 * 24 * time.Hour)) {
		t.Errorf("dispute freeze hold expiry = %v, want ~120 days out (the TTL, not the cap)", hold.ExpiresAt)
	}
}
