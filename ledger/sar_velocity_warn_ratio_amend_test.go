package ledger

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// sarVelocityWarnLedger builds a ledger with a 10-posting / 24h
// velocity limit and eight 1.00 postings inside the window — usage
// sits exactly on the 0.8 warn band (8 >= 10 * 0.8), so a draft flags
// VELOCITY_ANOMALY under the defaults and nothing else does (the
// amounts are far below the 10,000.00 large-tx threshold).
func sarVelocityWarnLedger(t *testing.T, opts ...Option) (*Ledger, time.Time, time.Time) {
	t.Helper()
	l := sarTestLedger(t, opts...)
	if err := l.SetVelocityLimit("acct", "USD", 24*time.Hour, 10, 0); err != nil {
		t.Fatalf("SetVelocityLimit: %v", err)
	}
	since, until := sarWindow()
	now := time.Now()
	for i := 0; i < 8; i++ {
		sarPost(t, l, fmt.Sprintf("vel-amend-%d", i), "acct", "equity", 100, now.Add(-time.Duration(i)*time.Minute))
	}
	return l, since, until
}

func TestSetSARVelocityWarnRatioValidation(t *testing.T) {
	l, since, until := sarVelocityWarnLedger(t)

	invalid := []struct {
		name  string
		ratio float64
	}{
		{"ratio zero", 0},
		{"ratio one", 1},
		{"ratio negative", -0.5},
		{"ratio above one", 1.5},
	}
	for _, tc := range invalid {
		if err := l.SetSARVelocityWarnRatio(tc.ratio); !errors.Is(err, ErrInvalidSARVelocityWarnRatio) {
			t.Errorf("%s: SetSARVelocityWarnRatio(%v) = %v, want ErrInvalidSARVelocityWarnRatio", tc.name, tc.ratio, err)
		}
	}

	// The rejected revisions leave the previous fraction exactly as
	// it was — field-level and behavioral.
	if l.sarCfg.velocityWarnRatio != DefaultSARVelocityWarnRatio {
		t.Errorf("ratio after rejected revisions = %v, want %v",
			l.sarCfg.velocityWarnRatio, DefaultSARVelocityWarnRatio)
	}
	rep, err := l.DraftSAR("acct", since, until)
	if err != nil {
		t.Fatalf("DraftSAR after rejected revisions: %v", err)
	}
	if !hasReason(rep, SARReasonVelocityAnomaly) {
		t.Errorf("reason codes %v: VELOCITY_ANOMALY lost after rejected revisions (defaults must still govern)", rep.ReasonCodes)
	}

	// A boundary-adjacent value installs: ratio just inside (0, 1).
	if err := l.SetSARVelocityWarnRatio(0.99); err != nil {
		t.Errorf("SetSARVelocityWarnRatio(0.99): want nil, got %v", err)
	}
	if l.sarCfg.velocityWarnRatio != 0.99 {
		t.Errorf("ratio = %v, want 0.99", l.sarCfg.velocityWarnRatio)
	}
}

func TestSetSARVelocityWarnRatioNoOpWritesNoAudit(t *testing.T) {
	al := mustAuditLog(t)
	l, _, _ := sarVelocityWarnLedger(t, WithAuditLog(al))

	// Revising to the value already in force is a no-op.
	if err := l.SetSARVelocityWarnRatio(DefaultSARVelocityWarnRatio); err != nil {
		t.Fatalf("SetSARVelocityWarnRatio(default) = %v, want nil no-op", err)
	}
	if err := l.SetSARVelocityWarnRatio(0.9); err != nil {
		t.Fatalf("SetSARVelocityWarnRatio(0.9): %v", err)
	}
	if err := l.SetSARVelocityWarnRatio(0.9); err != nil {
		t.Fatalf("SetSARVelocityWarnRatio(0.9) again = %v, want nil no-op", err)
	}

	events := readAuditOps(t, al)
	if n := countOp(events, "sar_velocity_warn_ratio_amended"); n != 1 {
		t.Errorf("sar_velocity_warn_ratio_amended events = %d, want 1 (no-ops write none)", n)
	}
}

func TestSetSARVelocityWarnRatioChangesSubsequentDraftSAR(t *testing.T) {
	l, since, until := sarVelocityWarnLedger(t)

	// Defaults: 8 of 10 postings sits on the 0.8 band.
	rep, err := l.DraftSAR("acct", since, until)
	if err != nil {
		t.Fatalf("DraftSAR (defaults): %v", err)
	}
	if !hasReason(rep, SARReasonVelocityAnomaly) {
		t.Fatalf("reason codes %v: want VELOCITY_ANOMALY under defaults", rep.ReasonCodes)
	}

	// Raising the band to 0.9 lifts the floor to 9 postings, above
	// the 8 in the window: the draft finds nothing suspicious at
	// all.
	before := l.version
	if err := l.SetSARVelocityWarnRatio(0.9); err != nil {
		t.Fatalf("SetSARVelocityWarnRatio(0.9): %v", err)
	}
	if l.version != before {
		t.Errorf("version = %d after amendment, want %d (structural: no version bump)", l.version, before)
	}
	if _, err := l.DraftSAR("acct", since, until); !errors.Is(err, ErrNoSuspiciousActivity) {
		t.Errorf("DraftSAR (ratio 0.9) err = %v, want ErrNoSuspiciousActivity", err)
	}

	// Lowering the band to 0.7 drops the floor to 7 postings: the
	// same 8 postings flag again — the revision governs every
	// subsequent draft.
	if err := l.SetSARVelocityWarnRatio(0.7); err != nil {
		t.Fatalf("SetSARVelocityWarnRatio(0.7): %v", err)
	}
	rep, err = l.DraftSAR("acct", since, until)
	if err != nil {
		t.Fatalf("DraftSAR (ratio 0.7): %v", err)
	}
	if !hasReason(rep, SARReasonVelocityAnomaly) {
		t.Errorf("reason codes %v: want VELOCITY_ANOMALY again under ratio 0.7", rep.ReasonCodes)
	}
}

func TestSetSARVelocityWarnRatioCtxRecordsOperator(t *testing.T) {
	al := mustAuditLog(t)
	l, _, _ := sarVelocityWarnLedger(t, WithAuditLog(al))

	ctx := ContextWithActor(context.Background(), "ops-desk")
	if err := l.SetSARVelocityWarnRatioCtx(ctx, 0.9); err != nil {
		t.Fatalf("SetSARVelocityWarnRatioCtx: %v", err)
	}

	events := readAuditOps(t, al)
	ev := findOp(events, "sar_velocity_warn_ratio_amended")
	if ev == nil {
		t.Fatal("no sar_velocity_warn_ratio_amended event")
	}
	if ev.Actor != "ops-desk" {
		t.Errorf("event actor = %q, want ops-desk", ev.Actor)
	}
	if ev.Details["previous_ratio"] != DefaultSARVelocityWarnRatio || ev.Details["ratio"] != 0.9 {
		t.Errorf("event ratio details = %+v, want 0.8 -> 0.9", ev.Details)
	}
	if ev.VersionBefore != ev.VersionAfter {
		t.Errorf("version %d -> %d, want unchanged (structural amendment)", ev.VersionBefore, ev.VersionAfter)
	}
}

func TestSetSARVelocityWarnRatioFiledReportUnaffected(t *testing.T) {
	l, since, until := sarVelocityWarnLedger(t)

	rep, err := l.DraftSAR("acct", since, until)
	if err != nil {
		t.Fatalf("DraftSAR: %v", err)
	}
	if !hasReason(rep, SARReasonVelocityAnomaly) {
		t.Fatalf("reason codes %v: want VELOCITY_ANOMALY before filing", rep.ReasonCodes)
	}
	filed, err := l.FileSAR(rep.ID)
	if err != nil {
		t.Fatalf("FileSAR: %v", err)
	}
	if filed.Status != SARFiled || filed.ContentHash == "" {
		t.Fatalf("filed report status = %q hash = %q, want filed + sealed", filed.Status, filed.ContentHash)
	}

	// Retune so the same entries would no longer flag: the filed
	// report must keep its reason codes, sightings, and hash.
	if err := l.SetSARVelocityWarnRatio(0.9); err != nil {
		t.Fatalf("SetSARVelocityWarnRatio(0.9): %v", err)
	}
	after, ok := l.GetSAR(rep.ID)
	if !ok {
		t.Fatal("GetSAR: filed report vanished after retuning")
	}
	if after.Status != SARFiled {
		t.Errorf("filed status = %q, want filed", after.Status)
	}
	if after.ContentHash != filed.ContentHash {
		t.Errorf("content hash changed by retuning:\nbefore %s\nafter  %s", filed.ContentHash, after.ContentHash)
	}
	if !hasReason(after, SARReasonVelocityAnomaly) {
		t.Errorf("filed reason codes %v: VELOCITY_ANOMALY lost after retuning (filed reports are immutable)", after.ReasonCodes)
	}
	if len(after.Sightings) != len(filed.Sightings) {
		t.Errorf("filed sightings = %d, want %d", len(after.Sightings), len(filed.Sightings))
	}

	// A fresh draft after the retune finds nothing suspicious —
	// the tuning did change; only the filed report is frozen.
	if _, err := l.DraftSAR("acct", since, until); !errors.Is(err, ErrNoSuspiciousActivity) {
		t.Errorf("DraftSAR after retune err = %v, want ErrNoSuspiciousActivity", err)
	}
}

func TestSetSARVelocityWarnRatioSnapshotGuardrail(t *testing.T) {
	l, since, until := sarVelocityWarnLedger(t)

	// Amend away from the default: the window no longer flags.
	if err := l.SetSARVelocityWarnRatio(0.9); err != nil {
		t.Fatalf("SetSARVelocityWarnRatio(0.9): %v", err)
	}
	if _, err := l.DraftSAR("acct", since, until); !errors.Is(err, ErrNoSuspiciousActivity) {
		t.Errorf("DraftSAR (amended) err = %v, want ErrNoSuspiciousActivity", err)
	}

	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}

	// The tuning does NOT ride snapshots: the restored ledger is
	// back on the 0.8 default and the same journal entries flag
	// VELOCITY_ANOMALY again — the operator must re-set any
	// amended tuning after a restore. Note the restored ledger's
	// velocity *usage* deque is live-only too (velocity.go): the
	// journal round-trips but the deque does not, so eight fresh
	// postings rebuild the window usage before the draft.
	if restored.sarCfg.velocityWarnRatio != DefaultSARVelocityWarnRatio {
		t.Errorf("restored ratio = %v, want %v (default after restore)",
			restored.sarCfg.velocityWarnRatio, DefaultSARVelocityWarnRatio)
	}
	now := time.Now()
	for i := 0; i < 8; i++ {
		sarPost(t, restored, fmt.Sprintf("vel-restore-%d", i), "acct", "equity", 100, now.Add(-time.Duration(i)*time.Minute))
	}
	rep, err := restored.DraftSAR("acct", since, until)
	if err != nil {
		t.Fatalf("DraftSAR (restored): %v", err)
	}
	if !hasReason(rep, SARReasonVelocityAnomaly) {
		t.Errorf("restored reason codes %v: want VELOCITY_ANOMALY under defaults", rep.ReasonCodes)
	}

	// And the restored ledger accepts a fresh revision.
	if err := restored.SetSARVelocityWarnRatio(0.9); err != nil {
		t.Fatalf("re-set ratio on restored ledger: %v", err)
	}
}
