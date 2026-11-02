package ledger

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// sarStructuringLedger builds a ledger with a USD 10,000 large-tx
// threshold and three 8,000.00 inflows in one UTC day — the classic
// placement cluster that flags STRUCTURING under the 0.75 / 3
// defaults (each amount >= 0.75 * threshold, below the threshold,
// count 3 meets the gate). It returns the ledger and its SAR window.
func sarStructuringLedger(t *testing.T, opts ...Option) (*Ledger, time.Time, time.Time) {
	t.Helper()
	l := sarTestLedger(t, opts...)
	since, until := sarWindow()
	now := time.Now()
	for i := 0; i < 3; i++ {
		sarPost(t, l, fmt.Sprintf("struct-amend-%d", i), "acct", "equity", 800000, now.Add(-time.Duration(i)*time.Minute))
	}
	return l, since, until
}

func TestSetSARStructuringValidation(t *testing.T) {
	l, since, until := sarStructuringLedger(t)

	invalid := []struct {
		name     string
		ratio    float64
		minCount int
	}{
		{"ratio zero", 0, 3},
		{"ratio one", 1, 3},
		{"ratio negative", -0.5, 3},
		{"ratio above one", 1.5, 3},
		{"minCount one", 0.5, 1},
		{"minCount zero", 0.5, 0},
		{"minCount negative", 0.5, -2},
		{"both invalid", 0, 1},
	}
	for _, tc := range invalid {
		if err := l.SetSARStructuring(tc.ratio, tc.minCount); !errors.Is(err, ErrInvalidSARStructuring) {
			t.Errorf("%s: SetSARStructuring(%v, %d) = %v, want ErrInvalidSARStructuring", tc.name, tc.ratio, tc.minCount, err)
		}
	}

	// The rejected revisions leave the previous tuning exactly as
	// it was — field-level and behavioral.
	if l.sarCfg.structuringRatio != DefaultSARStructuringRatio || l.sarCfg.structuringMinCount != DefaultSARStructuringMinCount {
		t.Errorf("tuning after rejected revisions = %v/%d, want %v/%d",
			l.sarCfg.structuringRatio, l.sarCfg.structuringMinCount,
			DefaultSARStructuringRatio, DefaultSARStructuringMinCount)
	}
	rep, err := l.DraftSAR("acct", since, until)
	if err != nil {
		t.Fatalf("DraftSAR after rejected revisions: %v", err)
	}
	if !hasReason(rep, SARReasonStructuring) {
		t.Errorf("reason codes %v: STRUCTURING lost after rejected revisions (defaults must still govern)", rep.ReasonCodes)
	}

	// Boundary-adjacent values install: ratio just inside (0, 1),
	// minCount exactly 2.
	if err := l.SetSARStructuring(0.99, 2); err != nil {
		t.Errorf("SetSARStructuring(0.99, 2): want nil, got %v", err)
	}
	if l.sarCfg.structuringRatio != 0.99 || l.sarCfg.structuringMinCount != 2 {
		t.Errorf("tuning = %v/%d, want 0.99/2", l.sarCfg.structuringRatio, l.sarCfg.structuringMinCount)
	}
}

func TestSetSARStructuringNoOpWritesNoAudit(t *testing.T) {
	al := mustAuditLog(t)
	l, _, _ := sarStructuringLedger(t, WithAuditLog(al))

	// Revising to the values already in force is a no-op.
	if err := l.SetSARStructuring(DefaultSARStructuringRatio, DefaultSARStructuringMinCount); err != nil {
		t.Fatalf("SetSARStructuring(defaults) = %v, want nil no-op", err)
	}
	if err := l.SetSARStructuring(0.9, 4); err != nil {
		t.Fatalf("SetSARStructuring(0.9, 4): %v", err)
	}
	if err := l.SetSARStructuring(0.9, 4); err != nil {
		t.Fatalf("SetSARStructuring(0.9, 4) again = %v, want nil no-op", err)
	}
	// Changing only one field is a real revision, not a no-op.
	if err := l.SetSARStructuring(0.9, 3); err != nil {
		t.Fatalf("SetSARStructuring(0.9, 3): %v", err)
	}

	events := readAuditOps(t, al)
	if n := countOp(events, "sar_structuring_amended"); n != 2 {
		t.Errorf("sar_structuring_amended events = %d, want 2 (no-ops write none)", n)
	}
}

func TestSetSARStructuringChangesSubsequentDraftSAR(t *testing.T) {
	l, since, until := sarStructuringLedger(t)

	// Defaults: the 3 x 8,000.00 cluster flags STRUCTURING.
	rep, err := l.DraftSAR("acct", since, until)
	if err != nil {
		t.Fatalf("DraftSAR (defaults): %v", err)
	}
	if !hasReason(rep, SARReasonStructuring) {
		t.Fatalf("reason codes %v: want STRUCTURING under defaults", rep.ReasonCodes)
	}

	// Raising the ratio to 0.9 lifts the floor to 9,000.00, above
	// the 8,000.00 amounts: the cluster no longer qualifies. The
	// day aggregate (24,000.00) still crosses the threshold, so the
	// draft survives on LARGE_TX_FLOW alone.
	before := l.version
	if err := l.SetSARStructuring(0.9, 3); err != nil {
		t.Fatalf("SetSARStructuring(0.9, 3): %v", err)
	}
	if l.version != before {
		t.Errorf("version = %d after amendment, want %d (structural: no version bump)", l.version, before)
	}
	rep, err = l.DraftSAR("acct", since, until)
	if err != nil {
		t.Fatalf("DraftSAR (ratio 0.9): %v", err)
	}
	if hasReason(rep, SARReasonStructuring) {
		t.Errorf("reason codes %v: unexpected STRUCTURING under ratio 0.9", rep.ReasonCodes)
	}
	if !hasReason(rep, SARReasonLargeTxFlow) {
		t.Errorf("reason codes %v: want LARGE_TX_FLOW from the day aggregate", rep.ReasonCodes)
	}

	// Restoring the ratio but raising the count gate to 4 also
	// clears STRUCTURING: only 3 candidates in the day.
	if err := l.SetSARStructuring(0.75, 4); err != nil {
		t.Fatalf("SetSARStructuring(0.75, 4): %v", err)
	}
	rep, err = l.DraftSAR("acct", since, until)
	if err != nil {
		t.Fatalf("DraftSAR (minCount 4): %v", err)
	}
	if hasReason(rep, SARReasonStructuring) {
		t.Errorf("reason codes %v: unexpected STRUCTURING under minCount 4", rep.ReasonCodes)
	}

	// Lowering the gate to 2 re-flags the same cluster: the
	// revision governs every subsequent draft.
	if err := l.SetSARStructuring(0.75, 2); err != nil {
		t.Fatalf("SetSARStructuring(0.75, 2): %v", err)
	}
	rep, err = l.DraftSAR("acct", since, until)
	if err != nil {
		t.Fatalf("DraftSAR (minCount 2): %v", err)
	}
	if !hasReason(rep, SARReasonStructuring) {
		t.Errorf("reason codes %v: want STRUCTURING again under minCount 2", rep.ReasonCodes)
	}
}

func TestSetSARStructuringCtxRecordsOperator(t *testing.T) {
	al := mustAuditLog(t)
	l, _, _ := sarStructuringLedger(t, WithAuditLog(al))

	ctx := ContextWithActor(context.Background(), "ops-desk")
	if err := l.SetSARStructuringCtx(ctx, 0.9, 4); err != nil {
		t.Fatalf("SetSARStructuringCtx: %v", err)
	}

	events := readAuditOps(t, al)
	ev := findOp(events, "sar_structuring_amended")
	if ev == nil {
		t.Fatal("no sar_structuring_amended event")
	}
	if ev.Actor != "ops-desk" {
		t.Errorf("event actor = %q, want ops-desk", ev.Actor)
	}
	if ev.Details["previous_ratio"] != DefaultSARStructuringRatio || ev.Details["ratio"] != 0.9 {
		t.Errorf("event ratio details = %+v, want 0.75 -> 0.9", ev.Details)
	}
	if ev.Details["previous_min_count"] != float64(DefaultSARStructuringMinCount) || ev.Details["min_count"] != float64(4) {
		t.Errorf("event min-count details = %+v, want 3 -> 4", ev.Details)
	}
	if ev.VersionBefore != ev.VersionAfter {
		t.Errorf("version %d -> %d, want unchanged (structural amendment)", ev.VersionBefore, ev.VersionAfter)
	}
}

func TestSetSARStructuringFiledReportUnaffected(t *testing.T) {
	l, since, until := sarStructuringLedger(t)

	rep, err := l.DraftSAR("acct", since, until)
	if err != nil {
		t.Fatalf("DraftSAR: %v", err)
	}
	if !hasReason(rep, SARReasonStructuring) {
		t.Fatalf("reason codes %v: want STRUCTURING before filing", rep.ReasonCodes)
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
	if err := l.SetSARStructuring(0.9, 4); err != nil {
		t.Fatalf("SetSARStructuring(0.9, 4): %v", err)
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
	if !hasReason(after, SARReasonStructuring) {
		t.Errorf("filed reason codes %v: STRUCTURING lost after retuning (filed reports are immutable)", after.ReasonCodes)
	}
	if len(after.Sightings) != len(filed.Sightings) {
		t.Errorf("filed sightings = %d, want %d", len(after.Sightings), len(filed.Sightings))
	}

	// A fresh draft after the retune no longer flags STRUCTURING —
	// the tuning did change; only the filed report is frozen.
	fresh, err := l.DraftSAR("acct", since, until)
	if err != nil {
		t.Fatalf("DraftSAR after retune: %v", err)
	}
	if hasReason(fresh, SARReasonStructuring) {
		t.Errorf("fresh reason codes %v: unexpected STRUCTURING after retune", fresh.ReasonCodes)
	}
}

func TestSetSARStructuringSnapshotGuardrail(t *testing.T) {
	l, since, until := sarStructuringLedger(t)

	// Amend away from the defaults: the cluster no longer flags.
	if err := l.SetSARStructuring(0.9, 4); err != nil {
		t.Fatalf("SetSARStructuring(0.9, 4): %v", err)
	}
	rep, err := l.DraftSAR("acct", since, until)
	if err != nil {
		t.Fatalf("DraftSAR (amended): %v", err)
	}
	if hasReason(rep, SARReasonStructuring) {
		t.Fatalf("reason codes %v: unexpected STRUCTURING under amended tuning", rep.ReasonCodes)
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
	// back on the 0.75 / 3 defaults and the same journal entries
	// flag STRUCTURING again — the operator must re-set any
	// amended tuning after a restore.
	if restored.sarCfg.structuringRatio != DefaultSARStructuringRatio || restored.sarCfg.structuringMinCount != DefaultSARStructuringMinCount {
		t.Errorf("restored tuning = %v/%d, want %v/%d (defaults after restore)",
			restored.sarCfg.structuringRatio, restored.sarCfg.structuringMinCount,
			DefaultSARStructuringRatio, DefaultSARStructuringMinCount)
	}
	rep, err = restored.DraftSAR("acct", since, until)
	if err != nil {
		t.Fatalf("DraftSAR (restored): %v", err)
	}
	if !hasReason(rep, SARReasonStructuring) {
		t.Errorf("restored reason codes %v: want STRUCTURING under defaults", rep.ReasonCodes)
	}

	// And the restored ledger accepts a fresh revision.
	if err := restored.SetSARStructuring(0.9, 4); err != nil {
		t.Fatalf("re-set tuning on restored ledger: %v", err)
	}
}
