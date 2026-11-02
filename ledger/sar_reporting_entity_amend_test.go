package ledger

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// sarReportingEntityLedger builds a ledger with a USD 10,000 large-tx
// threshold and one 20,000.00 inflow in one UTC day — the day aggregate
// crosses the threshold, so DraftSAR assembles a draft on
// LARGE_TX_FLOW alone. It returns the ledger and its SAR window.
func sarReportingEntityLedger(t *testing.T, opts ...Option) (*Ledger, time.Time, time.Time) {
	t.Helper()
	l := sarTestLedger(t, opts...)
	since, until := sarWindow()
	sarPost(t, l, "sar-entity-1", "acct", "equity", 2000000, time.Now())
	return l, since, until
}

func TestSetSARReportingEntityNoOpWritesNoAudit(t *testing.T) {
	al := mustAuditLog(t)
	l, _, _ := sarReportingEntityLedger(t, WithAuditLog(al))

	// Revising to the entity already in force is a no-op — including
	// the empty default, which omits the field.
	if err := l.SetSARReportingEntity(""); err != nil {
		t.Fatalf("SetSARReportingEntity(\"\") = %v, want nil no-op", err)
	}
	if err := l.SetSARReportingEntity("ACME BANK LTD"); err != nil {
		t.Fatalf("SetSARReportingEntity(ACME BANK LTD): %v", err)
	}
	if err := l.SetSARReportingEntity("ACME BANK LTD"); err != nil {
		t.Fatalf("SetSARReportingEntity(ACME BANK LTD) again = %v, want nil no-op", err)
	}
	// Revising back to empty is a real revision, not a no-op: it
	// restores the default omission.
	if err := l.SetSARReportingEntity(""); err != nil {
		t.Fatalf("SetSARReportingEntity(\"\") after set: %v", err)
	}

	events := readAuditOps(t, al)
	if n := countOp(events, "sar_reporting_entity_amended"); n != 2 {
		t.Errorf("sar_reporting_entity_amended events = %d, want 2 (no-ops write none)", n)
	}
}

func TestSetSARReportingEntityChangesSubsequentDraftSAR(t *testing.T) {
	l, since, until := sarReportingEntityLedger(t)

	// Default: no entity configured, the field is omitted.
	rep, err := l.DraftSAR("acct", since, until)
	if err != nil {
		t.Fatalf("DraftSAR (default): %v", err)
	}
	if rep.ReportingEntity != "" {
		t.Errorf("draft reporting entity = %q, want empty (omitted by default)", rep.ReportingEntity)
	}
	if !hasReason(rep, SARReasonLargeTxFlow) {
		t.Fatalf("reason codes %v: want LARGE_TX_FLOW from the day aggregate", rep.ReasonCodes)
	}

	// Stamping a new entity governs the next draft, and nothing
	// else: no version bump, no ledger entries.
	before := l.version
	if err := l.SetSARReportingEntity("ACME BANK LTD"); err != nil {
		t.Fatalf("SetSARReportingEntity(ACME BANK LTD): %v", err)
	}
	if l.version != before {
		t.Errorf("version = %d after amendment, want %d (structural: no version bump)", l.version, before)
	}
	rep, err = l.DraftSAR("acct", since, until)
	if err != nil {
		t.Fatalf("DraftSAR (entity set): %v", err)
	}
	if rep.ReportingEntity != "ACME BANK LTD" {
		t.Errorf("draft reporting entity = %q, want ACME BANK LTD", rep.ReportingEntity)
	}

	// Revising to a different entity re-stamps the next draft.
	if err := l.SetSARReportingEntity("FIRST NATIONAL"); err != nil {
		t.Fatalf("SetSARReportingEntity(FIRST NATIONAL): %v", err)
	}
	rep, err = l.DraftSAR("acct", since, until)
	if err != nil {
		t.Fatalf("DraftSAR (entity revised): %v", err)
	}
	if rep.ReportingEntity != "FIRST NATIONAL" {
		t.Errorf("draft reporting entity = %q, want FIRST NATIONAL", rep.ReportingEntity)
	}

	// Clearing back to empty restores the omission on the next
	// draft — the revision governs every subsequent draft.
	if err := l.SetSARReportingEntity(""); err != nil {
		t.Fatalf("SetSARReportingEntity(\"\"): %v", err)
	}
	rep, err = l.DraftSAR("acct", since, until)
	if err != nil {
		t.Fatalf("DraftSAR (entity cleared): %v", err)
	}
	if rep.ReportingEntity != "" {
		t.Errorf("draft reporting entity = %q, want empty after clearing", rep.ReportingEntity)
	}
}

func TestSetSARReportingEntityCtxRecordsOperator(t *testing.T) {
	al := mustAuditLog(t)
	l, _, _ := sarReportingEntityLedger(t, WithAuditLog(al))

	ctx := ContextWithActor(context.Background(), "ops-desk")
	if err := l.SetSARReportingEntityCtx(ctx, "ACME BANK LTD"); err != nil {
		t.Fatalf("SetSARReportingEntityCtx: %v", err)
	}

	events := readAuditOps(t, al)
	ev := findOp(events, "sar_reporting_entity_amended")
	if ev == nil {
		t.Fatal("no sar_reporting_entity_amended event")
	}
	if ev.Actor != "ops-desk" {
		t.Errorf("event actor = %q, want ops-desk", ev.Actor)
	}
	if ev.Details["previous_entity"] != "" || ev.Details["entity"] != "ACME BANK LTD" {
		t.Errorf("event entity details = %+v, want \"\" -> ACME BANK LTD", ev.Details)
	}
	if ev.VersionBefore != ev.VersionAfter {
		t.Errorf("version %d -> %d, want unchanged (structural amendment)", ev.VersionBefore, ev.VersionAfter)
	}
}

func TestSetSARReportingEntityFiledReportUnaffected(t *testing.T) {
	l, since, until := sarReportingEntityLedger(t, WithSARReportingEntity("ACME BANK LTD"))

	rep, err := l.DraftSAR("acct", since, until)
	if err != nil {
		t.Fatalf("DraftSAR: %v", err)
	}
	if rep.ReportingEntity != "ACME BANK LTD" {
		t.Fatalf("draft reporting entity = %q, want ACME BANK LTD before filing", rep.ReportingEntity)
	}
	filed, err := l.FileSAR(rep.ID)
	if err != nil {
		t.Fatalf("FileSAR: %v", err)
	}
	if filed.Status != SARFiled || filed.ContentHash == "" {
		t.Fatalf("filed report status = %q hash = %q, want filed + sealed", filed.Status, filed.ContentHash)
	}

	// Revise the entity: the filed report must keep its stamping,
	// reason codes, sightings, and hash.
	if err := l.SetSARReportingEntity("FIRST NATIONAL"); err != nil {
		t.Fatalf("SetSARReportingEntity(FIRST NATIONAL): %v", err)
	}
	after, ok := l.GetSAR(rep.ID)
	if !ok {
		t.Fatal("GetSAR: filed report vanished after revising the entity")
	}
	if after.Status != SARFiled {
		t.Errorf("filed status = %q, want filed", after.Status)
	}
	if after.ReportingEntity != "ACME BANK LTD" {
		t.Errorf("filed reporting entity = %q, want ACME BANK LTD (filed reports are immutable)", after.ReportingEntity)
	}
	if after.ContentHash != filed.ContentHash {
		t.Errorf("content hash changed by revising the entity:\nbefore %s\nafter  %s", filed.ContentHash, after.ContentHash)
	}
	if !hasReason(after, SARReasonLargeTxFlow) {
		t.Errorf("filed reason codes %v: LARGE_TX_FLOW lost after revising the entity (filed reports are immutable)", after.ReasonCodes)
	}
	if len(after.Sightings) != len(filed.Sightings) {
		t.Errorf("filed sightings = %d, want %d", len(after.Sightings), len(filed.Sightings))
	}

	// A fresh draft after the revision stamps the new entity — the
	// entity did change; only the filed report is frozen.
	fresh, err := l.DraftSAR("acct", since, until)
	if err != nil {
		t.Fatalf("DraftSAR after revision: %v", err)
	}
	if fresh.ReportingEntity != "FIRST NATIONAL" {
		t.Errorf("fresh draft reporting entity = %q, want FIRST NATIONAL", fresh.ReportingEntity)
	}
}

func TestSetSARReportingEntitySnapshotGuardrail(t *testing.T) {
	l, since, until := sarReportingEntityLedger(t)

	// Amend away from the default: drafts stamp the new entity.
	if err := l.SetSARReportingEntity("ACME BANK LTD"); err != nil {
		t.Fatalf("SetSARReportingEntity(ACME BANK LTD): %v", err)
	}
	rep, err := l.DraftSAR("acct", since, until)
	if err != nil {
		t.Fatalf("DraftSAR (amended): %v", err)
	}
	if rep.ReportingEntity != "ACME BANK LTD" {
		t.Fatalf("draft reporting entity = %q, want ACME BANK LTD", rep.ReportingEntity)
	}

	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}

	// The entity does NOT ride snapshots: the restored ledger is
	// back on the empty default and drafts omit the field again —
	// the operator must re-set any amended entity after a restore.
	if restored.sarCfg.reportingEntity != "" {
		t.Errorf("restored entity = %q, want empty (default after restore)", restored.sarCfg.reportingEntity)
	}
	rep, err = restored.DraftSAR("acct", since, until)
	if err != nil {
		t.Fatalf("DraftSAR (restored): %v", err)
	}
	if rep.ReportingEntity != "" {
		t.Errorf("restored draft reporting entity = %q, want empty", rep.ReportingEntity)
	}

	// And the restored ledger accepts a fresh revision.
	if err := restored.SetSARReportingEntity("ACME BANK LTD"); err != nil {
		t.Fatalf("re-set entity on restored ledger: %v", err)
	}
	rep, err = restored.DraftSAR("acct", since, until)
	if err != nil {
		t.Fatalf("DraftSAR (restored, re-set): %v", err)
	}
	if rep.ReportingEntity != "ACME BANK LTD" {
		t.Errorf("restored draft reporting entity after re-set = %q, want ACME BANK LTD", rep.ReportingEntity)
	}
}
