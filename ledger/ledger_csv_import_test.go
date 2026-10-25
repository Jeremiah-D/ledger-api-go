package ledger

import (
	"strings"
	"testing"
)

const csvImportHeader = "payer,payee,amount,currency,purpose,execution_date,payer_iban,payee_iban,payer_bic,payee_bic\n"

func csvImportFile(t *testing.T, rows ...string) []byte {
	t.Helper()
	return []byte(csvImportHeader + strings.Join(rows, "\n") + "\n")
}

// Happy path: valid rows stage into a pending-review batch and post
// nothing — no journal entries, no version bump — until approval.
func TestCSVImportStagesWithoutPosting(t *testing.T) {
	l := New()
	data := csvImportFile(t,
		"payroll,alice,100.50,USD,October salary,2026-10-25,DE89370400440532013000,GB29NWBK60161331926819,DEUTDEFF,",
		"payroll,bob,200,USD,October salary,2026-10-25,,,,",
	)
	rep, err := l.ImportPaymentCSV(data, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if rep.TotalRows != 2 || rep.ValidRows != 2 || len(rep.BadRows) != 0 {
		t.Fatalf("report = %+v, want 2 total / 2 valid / 0 bad", rep)
	}
	if rep.Duplicate || rep.Status != CSVBatchStatusPendingReview {
		t.Fatalf("report = %+v, want non-duplicate pending_review", rep)
	}
	if got := l.Version(); got != 0 {
		t.Fatalf("version = %d after import, want 0 (nothing posted)", got)
	}
	batch, ok := l.GetCSVBatch(rep.BatchID)
	if !ok {
		t.Fatalf("GetCSVBatch(%s) not found", rep.BatchID)
	}
	if len(batch.Rows) != 2 || batch.Rows[0].AmountCents != 10050 || batch.Rows[1].AmountCents != 20000 {
		t.Fatalf("batch rows = %+v, want cents 10050 and 20000", batch.Rows)
	}
	if batch.Rows[0].RowNumber != 2 || batch.Rows[1].RowNumber != 3 {
		t.Fatalf("row numbers = %d, %d, want 2, 3 (header is line 1)", batch.Rows[0].RowNumber, batch.Rows[1].RowNumber)
	}
	if batch.Rows[0].PayerIBAN != "DE89370400440532013000" || batch.Rows[0].PayerBIC != "DEUTDEFF" {
		t.Fatalf("row 0 IBAN/BIC = %q/%q, want pain.001 values carried", batch.Rows[0].PayerIBAN, batch.Rows[0].PayerBIC)
	}
}

// Per-row isolation: each bad row parks in the repair queue with the
// repair vocabulary's reason, and the valid row still stages.
func TestCSVImportBadRowsParkIndividually(t *testing.T) {
	l := New()
	data := csvImportFile(t,
		"payroll,alice,100.50,USD,Salary,2026-10-25,,,,",
		"payroll,carol,50.00,USD,Salary,2026-10-25,DE89370400440532013001,,,", // bad mod-97 IBAN
		"payroll,dave,10.105,USD,Salary,2026-10-25,,,,",                       // 3 fraction digits
		",erin,10.00,USD,Salary,2026-10-25,,,,",                               // missing payer
		"payroll,frank,10.00,USD,Salary,2026-02-30,,,,",                       // impossible date
		"payroll,grace,10.00,usd,Salary,2026-10-25,,,,",                       // lowercase currency
	)
	rep, err := l.ImportPaymentCSV(data, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if rep.TotalRows != 6 || rep.ValidRows != 1 || len(rep.BadRows) != 5 {
		t.Fatalf("report = %+v, want 6 total / 1 valid / 5 bad", rep)
	}
	wantReasons := map[int]string{
		3: RepairReasonInvalidFormat, // bad IBAN
		4: RepairReasonInvalidFormat, // 3dp amount
		5: RepairReasonMissingField,  // missing payer
		6: RepairReasonInvalidFormat, // impossible date
		7: RepairReasonInvalidFormat, // lowercase currency
	}
	for _, bad := range rep.BadRows {
		if wantReasons[bad.RowNumber] != bad.Reason {
			t.Errorf("row %d reason = %q, want %q (%s)", bad.RowNumber, bad.Reason, wantReasons[bad.RowNumber], bad.Detail)
		}
		if bad.RepairID == "" {
			t.Errorf("row %d has no repair record", bad.RowNumber)
			continue
		}
		rec, ok := l.GetRepair(bad.RepairID)
		if !ok {
			t.Errorf("row %d repair %s not found", bad.RowNumber, bad.RepairID)
			continue
		}
		if rec.Status != RepairStatusOpen || rec.ReasonCode != bad.Reason {
			t.Errorf("row %d repair = %+v, want open with reason %q", bad.RowNumber, rec, bad.Reason)
		}
		if rec.Instruction.Maker != "" {
			t.Errorf("row %d parked instruction has Maker %q, want empty (visibility only)", bad.RowNumber, rec.Instruction.Maker)
		}
	}
	batch, ok := l.GetCSVBatch(rep.BatchID)
	if !ok || len(batch.Rows) != 1 || batch.Rows[0].Payee != "alice" {
		t.Fatalf("batch = %+v, ok=%v, want only the alice row staged", batch, ok)
	}
}

// Whole-file failures: empty input and a missing required column
// error the import instead of staging a partial batch.
func TestCSVImportWholeFileFailures(t *testing.T) {
	l := New()
	if _, err := l.ImportPaymentCSV(nil, "operator"); err == nil {
		t.Error("empty file: want error, got nil")
	}
	if _, err := l.ImportPaymentCSV([]byte("payer,payee,amount\npayroll,alice,10.00\n"), "operator"); err == nil {
		t.Error("missing columns: want error, got nil")
	}
	if _, err := l.ImportPaymentCSV([]byte("payer,payer,amount,currency,purpose,execution_date\npayroll,alice,10.00,USD,x,2026-10-25\n"), "operator"); err == nil {
		t.Error("duplicate header: want error, got nil")
	}
}

// Duplicate file bytes: the second import returns the stored
// report as a duplicate and parks nothing twice.
func TestCSVImportDuplicateFileIsIdempotent(t *testing.T) {
	l := New()
	data := csvImportFile(t,
		"payroll,alice,100.00,USD,Salary,2026-10-25,,,,",
		"payroll,bob,10.00,USD,Salary,2026-10-25,INVALIDIBAN,,,",
	)
	first, err := l.ImportPaymentCSV(data, "operator")
	if err != nil {
		t.Fatal(err)
	}
	second, err := l.ImportPaymentCSV(data, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if !second.Duplicate || second.BatchID != first.BatchID {
		t.Fatalf("second import = %+v, want duplicate of %s", second, first.BatchID)
	}
	if len(second.BadRows) != 1 || second.BadRows[0].RepairID != first.BadRows[0].RepairID {
		t.Fatalf("second bad rows = %+v, want the same single parked record as the first import", second.BadRows)
	}
}

// Approval posts atomically through PostBatchTransfer, carries the
// row purpose as memo, and replays as a duplicate on second approval.
func TestCSVImportApprovePostsAndReplays(t *testing.T) {
	l := New()
	data := csvImportFile(t,
		"payroll,alice,100.50,USD,October salary,2026-10-25,,,,",
		"payroll,bob,200,USD,October salary,2026-10-25,,,,",
	)
	rep, err := l.ImportPaymentCSV(data, "operator")
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := l.ApproveCSVBatch(rep.BatchID, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Duplicate || len(receipt.Receipts) != 2 {
		t.Fatalf("receipt = %+v, want 2 non-duplicate receipts", receipt)
	}
	if got := l.BalanceIn("alice", "USD"); got != 10050 {
		t.Errorf("alice balance = %d, want 10050", got)
	}
	if got := l.BalanceIn("payroll", "USD"); got != -30050 {
		t.Errorf("payroll balance = %d, want -30050", got)
	}
	versionAfterApprove := l.Version()
	replay, err := l.ApproveCSVBatch(rep.BatchID, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Duplicate {
		t.Errorf("second approval duplicate = false, want true")
	}
	if got := l.Version(); got != versionAfterApprove {
		t.Errorf("version moved on replay: %d -> %d", versionAfterApprove, got)
	}
	batch, _ := l.GetCSVBatch(rep.BatchID)
	if batch.Status != CSVBatchStatusApproved {
		t.Errorf("batch status = %q, want approved", batch.Status)
	}
	if _, err := l.ApproveCSVBatch("csvimport-nonexistent", "operator"); err == nil {
		t.Error("unknown batch: want error, got nil")
	}
}

// A failed approval posts nothing and leaves the batch pending, so
// the operator can fix the cause and approve again.
func TestCSVImportFailedApprovalLeavesBatchPending(t *testing.T) {
	l := New()
	data := csvImportFile(t,
		"payroll,alice,100.00,USD,Salary,2026-10-25,,,,",
		"payroll,bob,100.00,USD,Salary,2026-10-25,,,,",
	)
	rep, err := l.ImportPaymentCSV(data, "operator")
	if err != nil {
		t.Fatal(err)
	}
	l.AddDeniedAccount("bob")
	versionBefore := l.Version()
	if _, err := l.ApproveCSVBatch(rep.BatchID, "operator"); err == nil {
		t.Fatal("denylisted payee approval: want error, got nil")
	}
	if got := l.Version(); got != versionBefore {
		t.Errorf("version moved on failed approval: %d -> %d", versionBefore, got)
	}
	batch, _ := l.GetCSVBatch(rep.BatchID)
	if batch.Status != CSVBatchStatusPendingReview {
		t.Errorf("batch status = %q, want pending_review after failed approval", batch.Status)
	}
	if got := l.BalanceIn("alice", "USD"); got != 0 {
		t.Errorf("alice balance = %d after failed approval, want 0 (atomic)", got)
	}
}
