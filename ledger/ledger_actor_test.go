package ledger

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// auditEventsByOp reads the audit dir's events for today, grouped by op.
func auditEventsByOp(t *testing.T, dir string) map[string][]AuditEvent {
	t.Helper()
	events, corrupt, err := ReadAuditLog(dir, time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt = %d", corrupt)
	}
	byOp := map[string][]AuditEvent{}
	for _, ev := range events {
		byOp[ev.Op] = append(byOp[ev.Op], ev)
	}
	return byOp
}

func TestActorContextRoundTrip(t *testing.T) {
	if got := ActorFromContext(context.Background()); got != "" {
		t.Fatalf("empty ctx actor = %q, want empty", got)
	}
	if got := ActorFromContext(nil); got != "" {
		t.Fatalf("nil ctx actor = %q, want empty", got)
	}
	ctx := ContextWithActor(context.Background(), "ops-alan")
	if got := ActorFromContext(ctx); got != "ops-alan" {
		t.Fatalf("actor = %q, want ops-alan", got)
	}
	// The key is unexported: a foreign key cannot smuggle an identity in.
	if got := ActorFromContext(context.WithValue(context.Background(), "actor", "mallory")); got != "" {
		t.Fatalf("foreign key leaked actor %q", got)
	}
}

func TestNormalizeAuditActor(t *testing.T) {
	if got := NormalizeAuditActor(""); got != DefaultAuditActor {
		t.Fatalf("empty = %q, want %q", got, DefaultAuditActor)
	}
	if got := NormalizeAuditActor("   "); got != DefaultAuditActor {
		t.Fatalf("blank = %q, want %q", got, DefaultAuditActor)
	}
	if got := NormalizeAuditActor("  ops-alan  "); got != "ops-alan" {
		t.Fatalf("trimmed = %q, want ops-alan", got)
	}
}

func TestFreezeCtxActorAttribution(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))

	ctx := ContextWithActor(context.Background(), "ops-alan")
	l.FreezeCtx(ctx, "cash")
	l.FreezeUntilCtx(ctx, "vault", time.Now().Add(time.Hour))
	l.UnfreezeCtx(ctx, "cash")
	l.Freeze("plain") // no ctx: entrypoint name kept
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	byOp := auditEventsByOp(t, dir)
	freezes := byOp["freeze"]
	if len(freezes) != 3 {
		t.Fatalf("freeze events = %d, want 3", len(freezes))
	}
	actors := map[string]int{}
	for _, ev := range freezes {
		actors[ev.Actor]++
	}
	if actors["ops-alan"] != 2 || actors["Freeze"] != 1 {
		t.Fatalf("freeze actors = %v, want 2x ops-alan + 1x Freeze", actors)
	}
	if unfreezes := byOp["unfreeze"]; len(unfreezes) != 1 || unfreezes[0].Actor != "ops-alan" {
		t.Fatalf("unfreeze events = %+v", unfreezes)
	}
}

func TestReviewCtxActorAttribution(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithReviewThreshold(10000))
	seedBalance(t, l, "payer", 100000)

	ctx := ContextWithActor(context.Background(), "risk-lead")
	if _, err := l.PostTransfer(Transfer{ID: "tx1", From: "payer", To: "payee", AmountCents: 50000}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if _, err := l.ApproveTransferReviewCtx(ctx, "tx1", "risk-lead"); err != nil {
		t.Fatalf("ApproveTransferReviewCtx: %v", err)
	}
	if _, err := l.PostTransfer(Transfer{ID: "tx2", From: "payer", To: "payee", AmountCents: 40000}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if _, _, err := l.RejectTransferReviewCtx(ctx, "tx2", "risk-lead", "suspicious"); err != nil {
		t.Fatalf("RejectTransferReviewCtx: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	byOp := auditEventsByOp(t, dir)
	if got := byOp["transfer_review_approved"]; len(got) != 1 || got[0].Actor != "risk-lead" {
		t.Fatalf("approved events = %+v", got)
	}
	if got := byOp["transfer_review_rejected"]; len(got) != 1 || got[0].Actor != "risk-lead" {
		t.Fatalf("rejected events = %+v", got)
	}
	// The reviewer business record is untouched by actor attribution.
	rev, _ := l.GetTransferReview("tx1")
	if rev.ReviewedBy != "risk-lead" {
		t.Fatalf("ReviewedBy = %q", rev.ReviewedBy)
	}
}

func TestCloseAccountCtxActorAttribution(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	ctx := ContextWithActor(context.Background(), "ops-alan")
	if _, _, err := l.CloseAccountCtx(ctx, "old-acct", false); err != nil {
		t.Fatalf("CloseAccountCtx: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	byOp := auditEventsByOp(t, dir)
	if got := byOp["account_close"]; len(got) != 1 || got[0].Actor != "ops-alan" {
		t.Fatalf("account_close events = %+v", got)
	}
}

func TestExpireReviewsCtxActorAttribution(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithReviewThreshold(5000), WithReviewExpiry(time.Hour))
	seedBalance(t, l, "payer", 100000)
	if _, err := l.PostTransfer(Transfer{ID: "tx-exp", From: "payer", To: "payee", AmountCents: 50000}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	ctx := ContextWithActor(context.Background(), "ops-alan")
	if n := l.ExpireReviewsAtCtx(ctx, time.Now().Add(2*time.Hour)); n != 1 {
		t.Fatalf("expired = %d, want 1", n)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	byOp := auditEventsByOp(t, dir)
	if got := byOp["transfer_review_expire"]; len(got) != 1 || got[0].Actor != "ops-alan" {
		t.Fatalf("expire events = %+v", got)
	}
}

func TestPeriodCtxActorAttribution(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	ctx := ContextWithActor(context.Background(), "ops-alan")
	if err := l.ClosePeriodCtx(ctx, "2026-10"); err != nil {
		t.Fatalf("ClosePeriodCtx: %v", err)
	}
	if err := l.ReopenPeriodCtx(ctx, "2026-10"); err != nil {
		t.Fatalf("ReopenPeriodCtx: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	byOp := auditEventsByOp(t, dir)
	if got := byOp["period_close"]; len(got) != 1 || got[0].Actor != "ops-alan" {
		t.Fatalf("period_close events = %+v", got)
	}
	if got := byOp["period_reopen"]; len(got) != 1 || got[0].Actor != "ops-alan" {
		t.Fatalf("period_reopen events = %+v", got)
	}
}

func TestActorCoveredByChainSeal(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	l.FreezeCtx(ContextWithActor(context.Background(), "ops-alan"), "cash")
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The attributed actor verifies under the hash chain: the seal
	// covers the actor like every other field (LG-32).
	rep, err := VerifyAuditLog(dir)
	if err != nil {
		t.Fatalf("VerifyAuditLog: %v", err)
	}
	if !rep.Ok {
		t.Fatalf("chain broken before tampering: %+v", rep.FirstBreak)
	}

	// Rewrite the freeze line with a forged actor: verification must
	// pinpoint the tamper as a hash mismatch.
	day := time.Now().UTC().Format("2006-01-02")
	path := filepath.Join(dir, "audit-"+day+".jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read audit file: %v", err)
	}
	if !strings.Contains(string(raw), `"actor":"ops-alan"`) {
		t.Fatalf("freeze event actor not found in audit file")
	}
	tampered := bytes.Replace(raw, []byte(`"actor":"ops-alan"`), []byte(`"actor":"mallory"`), 1)
	if err := os.WriteFile(path, tampered, 0o644); err != nil {
		t.Fatalf("rewrite audit file: %v", err)
	}
	rep, err = VerifyAuditLog(dir)
	if err != nil {
		t.Fatalf("VerifyAuditLog: %v", err)
	}
	if rep.Ok || rep.FirstBreak == nil {
		t.Fatalf("tampered actor not detected: %+v", rep)
	}
	if rep.FirstBreak.Reason != "hash_mismatch" {
		t.Fatalf("break reason = %q, want hash_mismatch", rep.FirstBreak.Reason)
	}
}

func TestActorSnapshotAgnostic(t *testing.T) {
	// Actor attribution is call-scoped metadata: it never enters
	// snapshot state, so a snapshot round-trip is actor-agnostic.
	l := New()
	seedBalance(t, l, "payer", 100000)
	l.FreezeCtx(ContextWithActor(context.Background(), "ops-alan"), "payer")

	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(&buf)
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if !snapshotLedgersEqual(l, restored) {
		t.Fatalf("snapshot round-trip diverged after actor-attributed ops")
	}
	if restored.IsFrozen("payer") != l.IsFrozen("payer") {
		t.Fatalf("freeze state lost in round-trip")
	}
}
