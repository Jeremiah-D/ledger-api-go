package ledger

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestSetReviewExpiryAppliesToLaterReviews(t *testing.T) {
	l := New(WithReviewThreshold(5000), WithReviewExpiry(time.Hour))
	seedBalance(t, l, "payer", 100000)

	if _, err := l.PostTransfer(Transfer{ID: "tx-exp-old", From: "payer", To: "payee", AmountCents: 9000}); err != nil {
		t.Fatalf("PostTransfer old: %v", err)
	}
	old, ok := l.GetTransferReview("tx-exp-old")
	if !ok {
		t.Fatal("old transfer has no review record")
	}
	if got := old.ExpiresAt.Sub(old.CreatedAt); got != time.Hour {
		t.Fatalf("old review lifetime = %s, want 1h", got)
	}

	// Shortening the lifetime governs only reviews created after
	// the amendment.
	l.SetReviewExpiry(30 * time.Minute)
	if _, err := l.PostTransfer(Transfer{ID: "tx-exp-new", From: "payer", To: "payee", AmountCents: 9000}); err != nil {
		t.Fatalf("PostTransfer new: %v", err)
	}
	rev, ok := l.GetTransferReview("tx-exp-new")
	if !ok {
		t.Fatal("new transfer has no review record")
	}
	if got := rev.ExpiresAt.Sub(rev.CreatedAt); got != 30*time.Minute {
		t.Fatalf("new review lifetime = %s, want 30m", got)
	}

	// The old review keeps the hour stamped at its creation: at
	// the new review's 45-minute mark only the new review lapses.
	oldAfter, _ := l.GetTransferReview("tx-exp-old")
	if got := oldAfter.ExpiresAt.Sub(oldAfter.CreatedAt); got != time.Hour {
		t.Fatalf("old review lifetime after amendment = %s, want 1h (stamped, not re-dated)", got)
	}
	if n := l.ExpireReviewsAt(rev.CreatedAt.Add(45 * time.Minute)); n != 1 {
		t.Fatalf("ExpireReviewsAt at +45m = %d, want 1 (new review only)", n)
	}
	if rev2, _ := l.GetTransferReview("tx-exp-new"); rev2.Status != ReviewStatusExpired {
		t.Errorf("new review status = %q, want expired", rev2.Status)
	}
	if rev2, _ := l.GetTransferReview("tx-exp-old"); rev2.Status != ReviewStatusPending {
		t.Errorf("old review status = %q, want pending", rev2.Status)
	}
}

func TestSetReviewExpiryDisablesExpiry(t *testing.T) {
	l := New(WithReviewThreshold(5000), WithReviewExpiry(time.Hour))
	seedBalance(t, l, "payer", 100000)

	// Zero disables auto-expiry: the new review never lapses.
	l.SetReviewExpiry(0)
	if _, err := l.PostTransfer(Transfer{ID: "tx-exp-zero", From: "payer", To: "payee", AmountCents: 9000}); err != nil {
		t.Fatalf("PostTransfer zero: %v", err)
	}
	rev, _ := l.GetTransferReview("tx-exp-zero")
	if !rev.ExpiresAt.IsZero() {
		t.Errorf("zero-lifetime review ExpiresAt = %v, want zero (never expires)", rev.ExpiresAt)
	}

	// A negative lifetime disables expiry the same way.
	l.SetReviewExpiry(-time.Hour)
	if _, err := l.PostTransfer(Transfer{ID: "tx-exp-neg", From: "payer", To: "payee", AmountCents: 9000}); err != nil {
		t.Fatalf("PostTransfer negative: %v", err)
	}
	rev, _ = l.GetTransferReview("tx-exp-neg")
	if !rev.ExpiresAt.IsZero() {
		t.Errorf("negative-lifetime review ExpiresAt = %v, want zero (never expires)", rev.ExpiresAt)
	}

	if n := l.ExpireReviewsAt(time.Now().Add(24 * time.Hour)); n != 0 {
		t.Errorf("ExpireReviewsAt far future = %d, want 0 (expiry disabled)", n)
	}
}

func TestSetReviewExpirySameValueNoOpAndAudit(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithReviewThreshold(5000), WithReviewExpiry(time.Hour))
	seedBalance(t, l, "payer", 100000)
	versionBefore := l.Version()

	// Revising to the current lifetime is a no-op: silent.
	l.SetReviewExpiry(time.Hour)

	// A real change lands exactly one event, attributed to the
	// operator carried by ctx, and bumps neither the version nor
	// anything else.
	ctx := ContextWithActor(context.Background(), "ops-desk")
	l.SetReviewExpiryCtx(ctx, 30*time.Minute)
	// Same value again: still silent.
	l.SetReviewExpiry(30 * time.Minute)
	if got := l.Version(); got != versionBefore {
		t.Errorf("Version after amendment = %d, want %d (structural, no bump)", got, versionBefore)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["review_expiry_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1 (both same-value revises are silent)", len(events))
	}
	if events[0].Actor != "ops-desk" {
		t.Errorf("event actor = %q, want ops-desk", events[0].Actor)
	}
	if events[0].Details["previous_ttl"] != "1h0m0s" || events[0].Details["ttl"] != "30m0s" {
		t.Errorf("event details = %+v, want 1h0m0s -> 30m0s", events[0].Details)
	}
}

func TestSetReviewExpiryAuditAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithReviewThreshold(5000), WithReviewExpiry(time.Hour))
	seedBalance(t, l, "payer", 100000)

	ctx := ContextWithActor(context.Background(), "ops-desk")
	l.SetReviewExpiryCtx(ctx, 30*time.Minute)

	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	if !strings.Contains(buf.String(), `"review_expiry"`) || !strings.Contains(buf.String(), "30m0s") {
		t.Errorf("snapshot config line lacks the amended review_expiry 30m0s")
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if !snapshotLedgersEqual(l, restored) {
		t.Fatal("restored ledger state differs from source")
	}
	// A review created on the restored ledger stamps the amended
	// 30-minute lifetime, not the original hour.
	if _, err := restored.PostTransfer(Transfer{ID: "tx-exp-restored", From: "payer", To: "payee", AmountCents: 9000}); err != nil {
		t.Fatalf("PostTransfer restored: %v", err)
	}
	rev, ok := restored.GetTransferReview("tx-exp-restored")
	if !ok {
		t.Fatal("restored transfer has no review record")
	}
	if got := rev.ExpiresAt.Sub(rev.CreatedAt); got != 30*time.Minute {
		t.Fatalf("restored review lifetime = %s, want 30m (amended TTL survived)", got)
	}

	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["review_expiry_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
}
