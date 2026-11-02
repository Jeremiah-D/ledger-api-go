package ledger

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

// sarRapidMovementLedger builds a ledger with 5,000 in then 4,500
// out within the default 4h window — outflow sits at 0.9 of inflow,
// so a draft flags RAPID_MOVEMENT under the defaults (0.8) and
// nothing else does (the amounts are far below the 10,000.00
// large-tx threshold, and there is a single inflow/outflow leg each,
// so no structuring sighting).
func sarRapidMovementLedger(t *testing.T, opts ...Option) (*Ledger, time.Time, time.Time) {
	t.Helper()
	l := sarTestLedger(t, opts...)
	since, until := sarWindow()
	now := time.Now()
	sarPost(t, l, "rm-amend-in", "acct", "equity", 500000, now.Add(-2*time.Hour))
	sarPost(t, l, "rm-amend-out", "equity", "acct", 450000, now.Add(-time.Hour))
	return l, since, until
}

func TestSetSARRapidMovementValidation(t *testing.T) {
	l, since, until := sarRapidMovementLedger(t)

	invalid := []struct {
		name   string
		window time.Duration
		ratio  float64
	}{
		{"window zero", 0, 0.8},
		{"window negative", -time.Hour, 0.8},
		{"ratio zero", 4 * time.Hour, 0},
		{"ratio one plus", 4 * time.Hour, 1.0 + 1e-9},
		{"ratio negative", 4 * time.Hour, -0.5},
		{"ratio above one", 4 * time.Hour, 1.5},
	}
	for _, tc := range invalid {
		if err := l.SetSARRapidMovement(tc.window, tc.ratio); !errors.Is(err, ErrInvalidSARRapidMovement) {
			t.Errorf("%s: SetSARRapidMovement(%v, %v) = %v, want ErrInvalidSARRapidMovement", tc.name, tc.window, tc.ratio, err)
		}
	}

	// The rejected revisions leave the previous tuning exactly as
	// it was — field-level and behavioral.
	if l.sarCfg.rapidMoveWindow != DefaultSARRapidMoveWindow || l.sarCfg.rapidMoveRatio != DefaultSARRapidMoveRatio {
		t.Errorf("tuning after rejected revisions = (%v, %v), want (%v, %v)",
			l.sarCfg.rapidMoveWindow, l.sarCfg.rapidMoveRatio,
			DefaultSARRapidMoveWindow, DefaultSARRapidMoveRatio)
	}
	rep, err := l.DraftSAR("acct", since, until)
	if err != nil {
		t.Fatalf("DraftSAR after rejected revisions: %v", err)
	}
	if !hasReason(rep, SARReasonRapidMovement) {
		t.Errorf("reason codes %v: RAPID_MOVEMENT lost after rejected revisions (defaults must still govern)", rep.ReasonCodes)
	}

	// Boundary-adjacent values install: ratio exactly 1.0 is in
	// (0, 1], and a 2h window is a valid positive window.
	if err := l.SetSARRapidMovement(2*time.Hour, 1.0); err != nil {
		t.Errorf("SetSARRapidMovement(2h, 1.0): want nil, got %v", err)
	}
	if l.sarCfg.rapidMoveWindow != 2*time.Hour || l.sarCfg.rapidMoveRatio != 1.0 {
		t.Errorf("tuning = (%v, %v), want (2h, 1.0)", l.sarCfg.rapidMoveWindow, l.sarCfg.rapidMoveRatio)
	}
}

func TestSetSARRapidMovementNoOpWritesNoAudit(t *testing.T) {
	al := mustAuditLog(t)
	l, _, _ := sarRapidMovementLedger(t, WithAuditLog(al))

	// Revising to the values already in force is a no-op.
	if err := l.SetSARRapidMovement(DefaultSARRapidMoveWindow, DefaultSARRapidMoveRatio); err != nil {
		t.Fatalf("SetSARRapidMovement(defaults) = %v, want nil no-op", err)
	}
	if err := l.SetSARRapidMovement(2*time.Hour, 0.9); err != nil {
		t.Fatalf("SetSARRapidMovement(2h, 0.9): %v", err)
	}
	if err := l.SetSARRapidMovement(2*time.Hour, 0.9); err != nil {
		t.Fatalf("SetSARRapidMovement(2h, 0.9) again = %v, want nil no-op", err)
	}

	events := readAuditOps(t, al)
	if n := countOp(events, "sar_rapid_movement_amended"); n != 1 {
		t.Errorf("sar_rapid_movement_amended events = %d, want 1 (no-ops write none)", n)
	}
}

func TestSetSARRapidMovementChangesSubsequentDraftSAR(t *testing.T) {
	l, since, until := sarRapidMovementLedger(t)

	// Defaults: 4,500 of 5,000 out within 4h sits at 0.9 >= 0.8.
	rep, err := l.DraftSAR("acct", since, until)
	if err != nil {
		t.Fatalf("DraftSAR (defaults): %v", err)
	}
	if !hasReason(rep, SARReasonRapidMovement) {
		t.Fatalf("reason codes %v: want RAPID_MOVEMENT under defaults", rep.ReasonCodes)
	}

	// Raising the ratio to 0.95 lifts the floor above the 0.9
	// actually moved: the draft finds nothing suspicious at all.
	before := l.version
	if err := l.SetSARRapidMovement(DefaultSARRapidMoveWindow, 0.95); err != nil {
		t.Fatalf("SetSARRapidMovement(4h, 0.95): %v", err)
	}
	if l.version != before {
		t.Errorf("version = %d after amendment, want %d (structural: no version bump)", l.version, before)
	}
	if _, err := l.DraftSAR("acct", since, until); !errors.Is(err, ErrNoSuspiciousActivity) {
		t.Errorf("DraftSAR (ratio 0.95) err = %v, want ErrNoSuspiciousActivity", err)
	}

	// Narrowing the window to 30m instead — the outflow lands 1h
	// after the first inflow, outside the window — also clears it.
	if err := l.SetSARRapidMovement(30*time.Minute, DefaultSARRapidMoveRatio); err != nil {
		t.Fatalf("SetSARRapidMovement(30m, 0.8): %v", err)
	}
	if _, err := l.DraftSAR("acct", since, until); !errors.Is(err, ErrNoSuspiciousActivity) {
		t.Errorf("DraftSAR (window 30m) err = %v, want ErrNoSuspiciousActivity", err)
	}

	// Restoring the defaults flags again — the revision governs
	// every subsequent draft.
	if err := l.SetSARRapidMovement(DefaultSARRapidMoveWindow, DefaultSARRapidMoveRatio); err != nil {
		t.Fatalf("SetSARRapidMovement(defaults): %v", err)
	}
	rep, err = l.DraftSAR("acct", since, until)
	if err != nil {
		t.Fatalf("DraftSAR (defaults restored): %v", err)
	}
	if !hasReason(rep, SARReasonRapidMovement) {
		t.Errorf("reason codes %v: want RAPID_MOVEMENT again under restored defaults", rep.ReasonCodes)
	}
}

func TestSetSARRapidMovementCtxRecordsOperator(t *testing.T) {
	al := mustAuditLog(t)
	l, _, _ := sarRapidMovementLedger(t, WithAuditLog(al))

	ctx := ContextWithActor(context.Background(), "ops-desk")
	if err := l.SetSARRapidMovementCtx(ctx, 2*time.Hour, 0.9); err != nil {
		t.Fatalf("SetSARRapidMovementCtx: %v", err)
	}

	events := readAuditOps(t, al)
	ev := findOp(events, "sar_rapid_movement_amended")
	if ev == nil {
		t.Fatal("no sar_rapid_movement_amended event")
	}
	if ev.Actor != "ops-desk" {
		t.Errorf("event actor = %q, want ops-desk", ev.Actor)
	}
	if ev.Details["previous_window"] != DefaultSARRapidMoveWindow.String() || ev.Details["window"] != (2*time.Hour).String() {
		t.Errorf("event window details = %+v, want 4h0m0s -> 2h0m0s", ev.Details)
	}
	if ev.Details["previous_ratio"] != DefaultSARRapidMoveRatio || ev.Details["ratio"] != 0.9 {
		t.Errorf("event ratio details = %+v, want 0.8 -> 0.9", ev.Details)
	}
	if ev.VersionBefore != ev.VersionAfter {
		t.Errorf("version %d -> %d, want unchanged (structural amendment)", ev.VersionBefore, ev.VersionAfter)
	}
}

func TestSetSARRapidMovementFiledReportUnaffected(t *testing.T) {
	l, since, until := sarRapidMovementLedger(t)

	rep, err := l.DraftSAR("acct", since, until)
	if err != nil {
		t.Fatalf("DraftSAR: %v", err)
	}
	if !hasReason(rep, SARReasonRapidMovement) {
		t.Fatalf("reason codes %v: want RAPID_MOVEMENT before filing", rep.ReasonCodes)
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
	if err := l.SetSARRapidMovement(DefaultSARRapidMoveWindow, 0.95); err != nil {
		t.Fatalf("SetSARRapidMovement(4h, 0.95): %v", err)
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
	if !hasReason(after, SARReasonRapidMovement) {
		t.Errorf("filed reason codes %v: RAPID_MOVEMENT lost after retuning (filed reports are immutable)", after.ReasonCodes)
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

func TestSetSARRapidMovementSnapshotGuardrail(t *testing.T) {
	l, since, until := sarRapidMovementLedger(t)

	// Amend away from the defaults: the legs no longer flag.
	if err := l.SetSARRapidMovement(2*time.Hour, 0.95); err != nil {
		t.Fatalf("SetSARRapidMovement(2h, 0.95): %v", err)
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
	// back on the 4h / 0.8 defaults. The journal entries do
	// round-trip, and the rapid-movement detector walks the
	// entries themselves, so the same legs flag again under the
	// defaults with no fresh postings — the operator must re-set
	// any amended tuning after a restore.
	if restored.sarCfg.rapidMoveWindow != DefaultSARRapidMoveWindow || restored.sarCfg.rapidMoveRatio != DefaultSARRapidMoveRatio {
		t.Errorf("restored tuning = (%v, %v), want (%v, %v) (defaults after restore)",
			restored.sarCfg.rapidMoveWindow, restored.sarCfg.rapidMoveRatio,
			DefaultSARRapidMoveWindow, DefaultSARRapidMoveRatio)
	}
	rep, err := restored.DraftSAR("acct", since, until)
	if err != nil {
		t.Fatalf("DraftSAR (restored): %v", err)
	}
	if !hasReason(rep, SARReasonRapidMovement) {
		t.Errorf("restored reason codes %v: want RAPID_MOVEMENT under defaults", rep.ReasonCodes)
	}

	// And the restored ledger accepts a fresh revision.
	if err := restored.SetSARRapidMovement(2 * time.Hour, 0.95); err != nil {
		t.Fatalf("re-set tuning on restored ledger: %v", err)
	}
}
