package ledger

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func TestPendingReviewEscalation(t *testing.T) {
	l := reviewSeed(t, 10000)
	if _, err := l.PostTransfer(Transfer{ID: "tx-old", From: "payer", To: "payee", AmountCents: 30000}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	created := mustReview(t, l, "tx-old").CreatedAt

	// Disabled by default: even a long-overdue review pages nothing.
	if got := l.EscalatePendingReviews(created.Add(48 * time.Hour)); len(got) != 0 {
		t.Fatalf("disabled escalation escalated %d reviews, want 0", len(got))
	}
	if got := l.PendingReviewEscalationCount(); got != 0 {
		t.Errorf("disabled escalation count = %d, want 0", got)
	}

	// Invalid policies fail fast: an enabled policy needs a positive age.
	if err := l.SetPendingReviewEscalationConfig(PendingReviewEscalationConfig{Enabled: true}); !errors.Is(err, ErrInvalidPendingReviewEscalationConfig) {
		t.Errorf("zero threshold: err = %v, want ErrInvalidPendingReviewEscalationConfig", err)
	}
	if err := l.SetPendingReviewEscalationConfig(PendingReviewEscalationConfig{Enabled: true, ThresholdAge: -time.Hour}); !errors.Is(err, ErrInvalidPendingReviewEscalationConfig) {
		t.Errorf("negative threshold: err = %v, want ErrInvalidPendingReviewEscalationConfig", err)
	}
	if err := l.SetPendingReviewEscalationConfig(PendingReviewEscalationConfig{Enabled: true, ThresholdAge: 24 * time.Hour}); err != nil {
		t.Fatalf("SetPendingReviewEscalationConfig: %v", err)
	}
	if cfg := l.PendingReviewEscalationConfig(); !cfg.Enabled || cfg.ThresholdAge != 24*time.Hour {
		t.Errorf("config = %+v, want enabled 24h", cfg)
	}

	// Not yet overdue: 23h old stays silent, and exactly 24h old is
	// not yet over the threshold (strictly-over rule).
	if got := l.EscalatePendingReviews(created.Add(23 * time.Hour)); len(got) != 0 {
		t.Fatalf("23h escalation = %d reviews, want 0", len(got))
	}
	if got := l.EscalatePendingReviews(created.Add(24 * time.Hour)); len(got) != 0 {
		t.Fatalf("exactly-24h escalation = %d reviews, want 0", len(got))
	}

	// Overdue: 25h old escalates exactly once.
	versionBefore := l.Version()
	balanceBefore := l.Balance("payer")
	availableBefore := l.Available("payer")
	at := created.Add(25 * time.Hour)
	escalated := l.EscalatePendingReviews(at)
	if len(escalated) != 1 || escalated[0].TransferID != "tx-old" {
		t.Fatalf("25h escalation = %v, want [tx-old]", escalated)
	}
	if !escalated[0].EscalatedAt.Equal(at) {
		t.Errorf("escalated review EscalatedAt = %v, want %v", escalated[0].EscalatedAt, at)
	}
	if got := l.PendingReviewEscalationCount(); got != 1 {
		t.Errorf("escalation count = %d, want 1", got)
	}
	// Alert-only: the review is still pending, the money and the
	// version did not move.
	rev := mustReview(t, l, "tx-old")
	if rev.Status != ReviewStatusPending {
		t.Errorf("review status = %q, want pending", rev.Status)
	}
	if !rev.EscalatedAt.Equal(at) {
		t.Errorf("stored EscalatedAt = %v, want %v", rev.EscalatedAt, at)
	}
	if got := l.Version(); got != versionBefore {
		t.Errorf("version = %d, want %d (escalation bumps nothing)", got, versionBefore)
	}
	if got := l.Balance("payer"); got != balanceBefore {
		t.Errorf("payer balance = %d, want %d", got, balanceBefore)
	}
	if got := l.Available("payer"); got != availableBefore {
		t.Errorf("payer available = %d, want %d", got, availableBefore)
	}

	// Edge-triggered: a later sweep pages nothing for the same review.
	if got := l.EscalatePendingReviews(created.Add(72 * time.Hour)); len(got) != 0 {
		t.Fatalf("re-sweep escalated %d reviews, want 0", len(got))
	}
	if got := l.PendingReviewEscalationCount(); got != 1 {
		t.Errorf("escalation count after re-sweep = %d, want 1", got)
	}
}

func mustReview(t *testing.T, l *Ledger, transferID string) TransferReview {
	t.Helper()
	rev, ok := l.GetTransferReview(transferID)
	if !ok {
		t.Fatalf("no review for %q", transferID)
	}
	return rev
}

func TestPendingReviewEscalationClearsOnDecision(t *testing.T) {
	l := reviewSeed(t, 10000)
	for _, id := range []string{"tx-approve", "tx-reject"} {
		if _, err := l.PostTransfer(Transfer{ID: id, From: "payer", To: "payee", AmountCents: 20000}); err != nil {
			t.Fatalf("PostTransfer %s: %v", id, err)
		}
	}
	if err := l.SetPendingReviewEscalationConfig(PendingReviewEscalationConfig{Enabled: true, ThresholdAge: 24 * time.Hour}); err != nil {
		t.Fatalf("SetPendingReviewEscalationConfig: %v", err)
	}
	at := time.Now().Add(25 * time.Hour)
	if got := l.EscalatePendingReviews(at); len(got) != 2 {
		t.Fatalf("escalated = %d reviews, want 2", len(got))
	}

	// Approval clears the stamp.
	if _, err := l.ApproveTransferReview("tx-approve", "ops"); err != nil {
		t.Fatalf("ApproveTransferReview: %v", err)
	}
	if rev := mustReview(t, l, "tx-approve"); !rev.EscalatedAt.IsZero() {
		t.Errorf("approved review EscalatedAt = %v, want zero", rev.EscalatedAt)
	}
	// Rejection clears the stamp.
	if _, _, err := l.RejectTransferReview("tx-reject", "ops", "no"); err != nil {
		t.Fatalf("RejectTransferReview: %v", err)
	}
	if rev := mustReview(t, l, "tx-reject"); !rev.EscalatedAt.IsZero() {
		t.Errorf("rejected review EscalatedAt = %v, want zero", rev.EscalatedAt)
	}
}

func TestPendingReviewEscalationClearsOnExpiry(t *testing.T) {
	l := New(WithReviewThreshold(10000), WithReviewExpiry(48*time.Hour))
	seedBalance(t, l, "payer", 100000)
	if _, err := l.PostTransfer(Transfer{ID: "tx-expire", From: "payer", To: "payee", AmountCents: 20000}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	created := mustReview(t, l, "tx-expire").CreatedAt
	if err := l.SetPendingReviewEscalationConfig(PendingReviewEscalationConfig{Enabled: true, ThresholdAge: 24 * time.Hour}); err != nil {
		t.Fatalf("SetPendingReviewEscalationConfig: %v", err)
	}
	// At 25h the review is overdue but not yet expired (48h expiry):
	// it escalates. At 49h the expiry sweep decides it and clears.
	if got := l.EscalatePendingReviews(created.Add(25 * time.Hour)); len(got) != 1 {
		t.Fatalf("25h escalation = %d reviews, want 1", len(got))
	}
	if got := l.ExpireReviewsAt(created.Add(49 * time.Hour)); got != 1 {
		t.Fatalf("ExpireReviewsAt = %d, want 1", got)
	}
	rev := mustReview(t, l, "tx-expire")
	if rev.Status != ReviewStatusExpired {
		t.Errorf("review status = %q, want expired", rev.Status)
	}
	if !rev.EscalatedAt.IsZero() {
		t.Errorf("expired review EscalatedAt = %v, want zero", rev.EscalatedAt)
	}
}

func TestPendingReviewEscalationAudit(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithReviewThreshold(10000))
	seedBalance(t, l, "payer", 100000)
	if _, err := l.PostTransfer(Transfer{ID: "tx-audit", From: "payer", To: "payee", AmountCents: 20000}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	created := mustReview(t, l, "tx-audit").CreatedAt
	if err := l.SetPendingReviewEscalationConfig(PendingReviewEscalationConfig{Enabled: true, ThresholdAge: 24 * time.Hour}); err != nil {
		t.Fatalf("SetPendingReviewEscalationConfig: %v", err)
	}
	at := created.Add(25 * time.Hour)
	if got := l.EscalatePendingReviews(at); len(got) != 1 {
		t.Fatalf("escalated = %d reviews, want 1", len(got))
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["pending_review_escalated"]
	if len(events) != 1 {
		t.Fatalf("pending_review_escalated events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.TraceID != "tx-audit" {
		t.Errorf("trace_id = %q, want tx-audit", ev.TraceID)
	}
	if ev.VersionBefore != ev.VersionAfter {
		t.Errorf("version bracket = %d -> %d, want equal (alert-only)", ev.VersionBefore, ev.VersionAfter)
	}
	d := ev.Details
	if d["transfer_id"] != "tx-audit" {
		t.Errorf("transfer_id = %v, want tx-audit", d["transfer_id"])
	}
	if d["pending_since"] != created.UTC().Format(time.RFC3339) {
		t.Errorf("pending_since = %v, want %v", d["pending_since"], created.UTC().Format(time.RFC3339))
	}
	if d["escalated_at"] != at.UTC().Format(time.RFC3339) {
		t.Errorf("escalated_at = %v, want %v", d["escalated_at"], at.UTC().Format(time.RFC3339))
	}
}

func TestPendingReviewEscalationSnapshotRoundTrip(t *testing.T) {
	l := reviewSeed(t, 10000)
	if _, err := l.PostTransfer(Transfer{ID: "tx-snap", From: "payer", To: "payee", AmountCents: 20000}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	created := mustReview(t, l, "tx-snap").CreatedAt
	if err := l.SetPendingReviewEscalationConfig(PendingReviewEscalationConfig{Enabled: true, ThresholdAge: 24 * time.Hour}); err != nil {
		t.Fatalf("SetPendingReviewEscalationConfig: %v", err)
	}
	at := created.Add(25 * time.Hour)
	if got := l.EscalatePendingReviews(at); len(got) != 1 {
		t.Fatalf("escalated = %d reviews, want 1", len(got))
	}

	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	// The stamp survived: the restored ledger does not re-page it,
	// even with escalation re-enabled.
	if rev := mustReview(t, restored, "tx-snap"); !rev.EscalatedAt.Equal(at) {
		t.Errorf("restored EscalatedAt = %v, want %v", rev.EscalatedAt, at)
	}
	if err := restored.SetPendingReviewEscalationConfig(PendingReviewEscalationConfig{Enabled: true, ThresholdAge: 24 * time.Hour}); err != nil {
		t.Fatalf("restored SetPendingReviewEscalationConfig: %v", err)
	}
	if got := restored.EscalatePendingReviews(created.Add(26 * time.Hour)); len(got) != 0 {
		t.Fatalf("restored re-sweep escalated %d reviews, want 0", len(got))
	}
	// The counter is operational state and does not round-trip.
	if got := restored.PendingReviewEscalationCount(); got != 0 {
		t.Errorf("restored escalation count = %d, want 0", got)
	}
}
