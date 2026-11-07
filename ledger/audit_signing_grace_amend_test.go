package ledger

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// graceTestLedger builds a signed ledger and returns it with its log
// and dir, failing the test on setup error.
func graceTestLedger(t *testing.T, keys []AuditSigningKey, grace time.Duration) (*Ledger, *AuditLog, string) {
	t.Helper()
	return signAmendTestLedger(t, keys, grace)
}

// graceRetiredLine signs line with a throwaway signer whose primary is
// the retired key, so the live signer can judge it by the retired-key
// window. The MAC is valid under the retired key's material.
func graceRetiredLine(t *testing.T, retired AuditSigningKey, grace time.Duration, line []byte) AuditSig {
	t.Helper()
	signer, err := newAuditSigner([]AuditSigningKey{retired}, grace)
	if err != nil {
		t.Fatalf("newAuditSigner(retired-only): %v", err)
	}
	return signer.sign(line)
}

func TestSetAuditSigningGraceInvalidRejected(t *testing.T) {
	k1, _, _ := amendTestKeys()
	l, al, dir := graceTestLedger(t, []AuditSigningKey{k1}, 24*time.Hour)
	defer func() {
		if err := al.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}()
	_ = dir
	for _, bad := range []time.Duration{0, -time.Hour, -time.Second} {
		prev, err := l.SetAuditSigningGrace(bad)
		if !errors.Is(err, ErrInvalidAuditSigningGrace) {
			t.Fatalf("SetAuditSigningGrace(%v) = %v, %v; want ErrInvalidAuditSigningGrace", bad, prev, err)
		}
		if prev != 24*time.Hour {
			t.Fatalf("SetAuditSigningGrace(%v) returned previous %v, want live 24h", bad, prev)
		}
		if got, ok := l.AuditSigningGrace(); !ok || got != 24*time.Hour {
			t.Fatalf("AuditSigningGrace() = %v, %v after rejected revision; want 24h, true", got, ok)
		}
	}
	// The zero-grace rejection reason, pinned: a zero grace makes any
	// row signed by a retired key fail as key_retired the instant
	// RetiredAt arrives, voiding all history verification for retired
	// keys — so 0 is a semantic trap, not a legal "off" state.
	retiredAt := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	retired := AuditSigningKey{ID: "old", Key: []byte("0123456789abcdef0123456789abcdef"), RetiredAt: retiredAt}
	signer, err := newAuditSigner([]AuditSigningKey{retired}, 0)
	if err != nil {
		t.Fatalf("newAuditSigner(grace 0): %v", err)
	}
	line := []byte(`{"op":"post"}`)
	sig := signer.sign(line)
	if reason := signer.verifyReason(&sig, line, retiredAt.Add(time.Nanosecond)); reason != "key_retired" {
		t.Fatalf("grace 0 verifyReason just past RetiredAt = %q, want key_retired", reason)
	}
}

func TestSetAuditSigningGraceSameValueNoop(t *testing.T) {
	k1, _, _ := amendTestKeys()
	l, al, dir := graceTestLedger(t, []AuditSigningKey{k1}, 24*time.Hour)
	amendTestPost(t, l, "e1")
	waitAuditWritten(t, al, 1)
	prev, err := l.SetAuditSigningGrace(24 * time.Hour)
	if err != nil {
		t.Fatalf("SetAuditSigningGrace(24h) = %v, %v; want 24h, nil", prev, err)
	}
	if prev != 24*time.Hour {
		t.Fatalf("same-value revision returned %v, want 24h", prev)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if byOp := auditEventsByOp(t, dir); len(byOp["audit_signing_grace_retuned"]) != 0 {
		t.Fatal("same-value revision wrote an audit_signing_grace_retuned event, want none")
	}
}

func TestSetAuditSigningGraceNoAuditLog(t *testing.T) {
	l := New()
	prev, err := l.SetAuditSigningGrace(2 * time.Hour)
	if !errors.Is(err, ErrAuditLogNotConfigured) {
		t.Fatalf("SetAuditSigningGrace on ledger without audit log = %v, %v; want ErrAuditLogNotConfigured", prev, err)
	}
	if prev != 0 {
		t.Fatalf("no-audit-log revision returned %v, want 0 (no live value)", prev)
	}
	if got, ok := l.AuditSigningGrace(); ok || got != 0 {
		t.Fatalf("AuditSigningGrace() = %v, %v; want 0, false", got, ok)
	}
}

func TestSetAuditSigningGraceRebuildsLiveSigner(t *testing.T) {
	k1, _, _ := amendTestKeys()
	l, al, _ := graceTestLedger(t, []AuditSigningKey{k1}, 24*time.Hour)
	defer func() {
		if err := al.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}()
	prev, err := l.SetAuditSigningGrace(2 * time.Hour)
	if err != nil || prev != 24*time.Hour {
		t.Fatalf("SetAuditSigningGrace(2h) = %v, %v; want 24h, nil", prev, err)
	}
	if got := al.snapshotSigner().grace; got != 2*time.Hour {
		t.Fatalf("live signer grace = %v, want 2h (revision must rebuild the signer immediately)", got)
	}
	if got, ok := l.AuditSigningGrace(); !ok || got != 2*time.Hour {
		t.Fatalf("AuditSigningGrace() = %v, %v; want 2h, true", got, ok)
	}
}

func TestSetAuditSigningGraceSurvivesRotation(t *testing.T) {
	k1, k2, _ := amendTestKeys()
	l, al, _ := graceTestLedger(t, []AuditSigningKey{k1}, 24*time.Hour)
	defer func() {
		if err := al.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}()
	if _, err := l.SetAuditSigningGrace(48 * time.Hour); err != nil {
		t.Fatalf("SetAuditSigningGrace(48h): %v", err)
	}
	if err := l.RotateAuditSigningKeys([]AuditSigningKey{k2}); err != nil {
		t.Fatalf("RotateAuditSigningKeys: %v", err)
	}
	if got := al.snapshotSigner().grace; got != 48*time.Hour {
		t.Fatalf("signer grace after rotation = %v, want 48h (rotation rebuilds with the revised grace)", got)
	}
}

func TestSetAuditSigningGraceTightenFailsRetiredRows(t *testing.T) {
	k1, _, _ := amendTestKeys()
	retiredAt := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	k2 := AuditSigningKey{ID: "k2", Key: []byte("fedcba9876543210fedcba9876543210"), RetiredAt: retiredAt}
	l, al, _ := graceTestLedger(t, []AuditSigningKey{k1, k2}, 24*time.Hour)
	defer func() {
		if err := al.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}()
	line := []byte(`{"op":"post"}`)
	sig := graceRetiredLine(t, k2, 24*time.Hour, line)
	eventTS := retiredAt.Add(2 * time.Hour)
	// Inside the 24h window the retired key's row still verifies.
	if reason := al.snapshotSigner().verifyReason(&sig, line, eventTS); reason != "" {
		t.Fatalf("verifyReason inside 24h grace = %q, want empty", reason)
	}
	// Tighten to 1h: the same row is now past RetiredAt+grace.
	if _, err := l.SetAuditSigningGrace(time.Hour); err != nil {
		t.Fatalf("SetAuditSigningGrace(1h): %v", err)
	}
	if reason := al.snapshotSigner().verifyReason(&sig, line, eventTS); reason != "key_retired" {
		t.Fatalf("verifyReason after tightening to 1h = %q, want key_retired", reason)
	}
	// A row inside the new window still verifies.
	if reason := al.snapshotSigner().verifyReason(&sig, line, retiredAt.Add(30*time.Minute)); reason != "" {
		t.Fatalf("verifyReason inside 1h grace = %q, want empty", reason)
	}
}

func TestSetAuditSigningGraceWidenRescuesRetiredRows(t *testing.T) {
	k1, _, _ := amendTestKeys()
	retiredAt := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	k2 := AuditSigningKey{ID: "k2", Key: []byte("fedcba9876543210fedcba9876543210"), RetiredAt: retiredAt}
	l, al, _ := graceTestLedger(t, []AuditSigningKey{k1, k2}, time.Hour)
	defer func() {
		if err := al.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}()
	line := []byte(`{"op":"post"}`)
	sig := graceRetiredLine(t, k2, time.Hour, line)
	eventTS := retiredAt.Add(2 * time.Hour)
	if reason := al.snapshotSigner().verifyReason(&sig, line, eventTS); reason != "key_retired" {
		t.Fatalf("verifyReason under 1h grace = %q, want key_retired", reason)
	}
	if _, err := l.SetAuditSigningGrace(48 * time.Hour); err != nil {
		t.Fatalf("SetAuditSigningGrace(48h): %v", err)
	}
	if reason := al.snapshotSigner().verifyReason(&sig, line, eventTS); reason != "" {
		t.Fatalf("verifyReason after widening to 48h = %q, want empty (row rescued)", reason)
	}
}

func TestSetAuditSigningGraceInFlightRowsUnaffected(t *testing.T) {
	k1, _, _ := amendTestKeys()
	l, al, dir := graceTestLedger(t, []AuditSigningKey{k1}, 24*time.Hour)
	amendTestPost(t, l, "e1")
	waitAuditWritten(t, al, 1)
	if _, err := l.SetAuditSigningGrace(2 * time.Hour); err != nil {
		t.Fatalf("SetAuditSigningGrace(2h): %v", err)
	}
	amendTestPost(t, l, "e2")
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Both rows keep the same primary kid: the grace governs
	// verification only, never signing.
	byOp := auditEventsByOp(t, dir)
	posts := byOp["post"]
	if len(posts) != 2 {
		t.Fatalf("post events = %d, want 2", len(posts))
	}
	for _, ev := range posts {
		if ev.Sig == nil || ev.Sig.KeyID != "k1" {
			t.Fatalf("post event sig kid = %v, want k1", ev.Sig)
		}
	}
	// Offline verification with the new grace still passes for the
	// whole trail, including the pre-revision row.
	rep, err := VerifyAuditSignatures(dir, []AuditSigningKey{k1}, 2*time.Hour)
	if err != nil {
		t.Fatalf("VerifyAuditSignatures: %v", err)
	}
	if !rep.Ok {
		t.Fatalf("offline verification failed: %+v", rep.BadLines)
	}
}

func TestSetAuditSigningGraceUnsignedLedgerThenRotate(t *testing.T) {
	k1, _, _ := amendTestKeys()
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	defer func() {
		if err := al.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}()
	if !al.Signed() {
		// Unsigned at construction, as expected.
	} else {
		t.Fatal("unsigned ledger Signed() = true, want false")
	}
	prev, err := l.SetAuditSigningGrace(2 * time.Hour)
	if err != nil || prev != 24*time.Hour {
		t.Fatalf("SetAuditSigningGrace(2h) on unsigned ledger = %v, %v; want 24h, nil", prev, err)
	}
	if got, ok := l.AuditSigningGrace(); !ok || got != 2*time.Hour {
		t.Fatalf("AuditSigningGrace() = %v, %v; want 2h, true (stored grace must move)", got, ok)
	}
	if err := l.RotateAuditSigningKeys([]AuditSigningKey{k1}); err != nil {
		t.Fatalf("RotateAuditSigningKeys: %v", err)
	}
	if !al.Signed() {
		t.Fatal("Signed() = false after rotation, want true")
	}
	if got := al.snapshotSigner().grace; got != 2*time.Hour {
		t.Fatalf("signer installed by rotation has grace %v, want 2h (revised grace)", got)
	}
}

func TestSetAuditSigningGraceKeepsKeysAndPrimary(t *testing.T) {
	k1, k2, _ := amendTestKeys()
	l, al, dir := graceTestLedger(t, []AuditSigningKey{k1, k2}, 24*time.Hour)
	amendTestPost(t, l, "e1")
	if _, err := l.SetAuditSigningGrace(3 * time.Hour); err != nil {
		t.Fatalf("SetAuditSigningGrace(3h): %v", err)
	}
	if got, ok := l.AuditSigningKeyIDs(); !ok || !equalStringSlice(got, []string{"k1", "k2"}) {
		t.Fatalf("AuditSigningKeyIDs() = %v, %v; want [k1 k2], true", got, ok)
	}
	amendTestPost(t, l, "e2")
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	byOp := auditEventsByOp(t, dir)
	for _, ev := range byOp["post"] {
		if ev.Sig == nil || ev.Sig.KeyID != "k1" {
			t.Fatalf("post event sig kid = %v, want k1 (primary unchanged)", ev.Sig)
		}
	}
}

func TestSetAuditSigningGraceConcurrent(t *testing.T) {
	k1, _, _ := amendTestKeys()
	l, al, _ := graceTestLedger(t, []AuditSigningKey{k1}, 24*time.Hour)
	defer func() {
		if err := al.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				grace := time.Duration(1+n%5) * time.Hour
				if _, err := l.SetAuditSigningGrace(grace); err != nil {
					t.Errorf("SetAuditSigningGrace(%v): %v", grace, err)
					return
				}
				if _, _, err := l.Post(JournalEntry{ID: fmt.Sprintf("conc-%d-%d", n, j), DebitAccount: "cash", CreditAccount: "rev", AmountCents: 1}); err != nil {
					t.Errorf("Post: %v", err)
					return
				}
				_ = al.Signed()
				_, _ = l.AuditSigningGrace()
			}
		}(i)
	}
	wg.Wait()
	if got, ok := l.AuditSigningGrace(); !ok || got <= 0 {
		t.Fatalf("AuditSigningGrace() after concurrent revisions = %v, %v; want positive, true", got, ok)
	}
}

func TestSetAuditSigningGraceCtxActor(t *testing.T) {
	k1, _, _ := amendTestKeys()
	l, al, dir := graceTestLedger(t, []AuditSigningKey{k1}, 24*time.Hour)
	// Plain call: the actor defaults to "system".
	if _, err := l.SetAuditSigningGrace(2 * time.Hour); err != nil {
		t.Fatalf("SetAuditSigningGrace(2h): %v", err)
	}
	// Ctx call: the operator identity is attributed.
	ctx := ContextWithActor(context.Background(), "ops-alan")
	if _, err := l.SetAuditSigningGraceCtx(ctx, 3*time.Hour); err != nil {
		t.Fatalf("SetAuditSigningGraceCtx(3h): %v", err)
	}
	// Repeated revisions back and forth all take effect; the getter
	// follows the live value.
	if _, err := l.SetAuditSigningGrace(24 * time.Hour); err != nil {
		t.Fatalf("SetAuditSigningGrace(24h): %v", err)
	}
	if got, ok := l.AuditSigningGrace(); !ok || got != 24*time.Hour {
		t.Fatalf("AuditSigningGrace() = %v, %v; want 24h, true", got, ok)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["audit_signing_grace_retuned"]
	if len(events) != 3 {
		t.Fatalf("audit_signing_grace_retuned events = %d, want 3", len(events))
	}
	if events[0].Actor != DefaultAuditActor {
		t.Fatalf("first revision Actor = %q, want %q (default system)", events[0].Actor, DefaultAuditActor)
	}
	if events[0].Details["previous_grace"] != "24h0m0s" || events[0].Details["current_grace"] != "2h0m0s" {
		t.Fatalf("first revision details = %v, want previous 24h0m0s / current 2h0m0s", events[0].Details)
	}
	if events[1].Actor != "ops-alan" {
		t.Fatalf("second revision Actor = %q, want ops-alan", events[1].Actor)
	}
	if events[1].Details["previous_grace"] != "2h0m0s" || events[1].Details["current_grace"] != "3h0m0s" {
		t.Fatalf("second revision details = %v, want previous 2h0m0s / current 3h0m0s", events[1].Details)
	}
	if events[2].Details["previous_grace"] != "3h0m0s" || events[2].Details["current_grace"] != "24h0m0s" {
		t.Fatalf("third revision details = %v, want previous 3h0m0s / current 24h0m0s", events[2].Details)
	}
}

func TestAuditSigningGraceDefaultsToConstant(t *testing.T) {
	// A restart restores the env/default value: a freshly constructed
	// ledger always seeds DefaultAuditSigningGrace.
	k1, _, _ := amendTestKeys()
	l, al, _ := graceTestLedger(t, []AuditSigningKey{k1}, 0)
	defer func() {
		if err := al.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}()
	if got, ok := l.AuditSigningGrace(); !ok || got != DefaultAuditSigningGrace {
		t.Fatalf("AuditSigningGrace() = %v, %v; want %v, true", got, ok, DefaultAuditSigningGrace)
	}
	if got := al.snapshotSigner().grace; got != DefaultAuditSigningGrace {
		t.Fatalf("live signer grace = %v, want %v", got, DefaultAuditSigningGrace)
	}
}
