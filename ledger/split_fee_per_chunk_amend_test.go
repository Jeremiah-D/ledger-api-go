package ledger

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
)

// splitFeeAmendLedger builds a ledger with an audit log in dir, a
// 10000-cent chunk cap, a 250 bps fee policy to "fee-rev", and alice
// funded with 1,000,000 cents. perChunk selects the construction-time
// WithSplitFeePerChunk mode.
func splitFeeAmendLedger(t *testing.T, dir string, perChunk bool) (*Ledger, *AuditLog) {
	t.Helper()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	opts := []Option{
		WithAuditLog(al),
		WithMaxTransferChunkCents(10000),
		WithTransferFeePolicy(250, "fee-rev"),
	}
	if perChunk {
		opts = append(opts, WithSplitFeePerChunk())
	}
	l := New(opts...)
	seedBalance(t, l, "alice", 1000000)
	return l, al
}

// splitFeeAmendEvents returns the split_fee_per_chunk_amended audit
// events recorded in dir, closing the log first so the read sees
// every sealed event.
func splitFeeAmendEvents(t *testing.T, al *AuditLog, dir string) []AuditEvent {
	t.Helper()
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return auditEventsByOp(t, dir)["split_fee_per_chunk_amended"]
}

func TestSetSplitFeePerChunkEnablesPerChunkFees(t *testing.T) {
	dir := t.TempDir()
	l, al := splitFeeAmendLedger(t, dir, false)

	// Default mode: the fee is computed once on the whole amount.
	receipt, err := l.PostTransferSplit(Transfer{
		ID: "t-off", From: "alice", To: "bob", AmountCents: 25000,
	})
	if err != nil {
		t.Fatalf("PostTransferSplit: %v", err)
	}
	if receipt.FeePerChunk {
		t.Error("FeePerChunk = true, want false (default mode)")
	}
	if receipt.FeeCents != 625 { // floor(25000 * 250 / 10000)
		t.Errorf("FeeCents = %d, want 625", receipt.FeeCents)
	}
	if fe := findEntry(l, "t-off/fee"); fe.AmountCents != 625 {
		t.Errorf("once-per-transfer fee leg = %+v, want 625", fe)
	}

	// The version is captured after the pre-amend split: the amendment
	// itself must not bump it.
	versionBefore := l.Version()
	l.SetSplitFeePerChunk(true)
	if got := l.Version(); got != versionBefore {
		t.Errorf("Version after amend = %d, want %d (structural, no bump)", got, versionBefore)
	}

	// Only transfers posted after the call are fee'd per chunk.
	receipt, err = l.PostTransferSplit(Transfer{
		ID: "t-on", From: "alice", To: "bob", AmountCents: 25000,
	})
	if err != nil {
		t.Fatalf("PostTransferSplit: %v", err)
	}
	if !receipt.FeePerChunk {
		t.Error("FeePerChunk = false, want true (amended mode)")
	}
	// Per-chunk policy fees: 250 + 250 + 125.
	wantFees := []int64{250, 250, 125}
	var total int64
	for i, c := range receipt.Chunks {
		if c.FeeCents != wantFees[i] {
			t.Errorf("chunk %d fee = %d, want %d", i, c.FeeCents, wantFees[i])
		}
		total += c.FeeCents
		feeID := "t-on/chunk-" + itoa(i) + "/fee"
		if fe := findEntry(l, feeID); fe.AmountCents != wantFees[i] {
			t.Errorf("fee leg %s amount = %d, want %d", feeID, fe.AmountCents, wantFees[i])
		}
	}
	if receipt.FeeCents != total || total != 625 {
		t.Errorf("total fee = %d (receipt %d), want 625", total, receipt.FeeCents)
	}
	// No once-per-transfer fee leg for the amended split.
	if e := findEntry(l, "t-on/fee"); e.ID != "" {
		t.Errorf("unexpected once-per-transfer fee leg: %+v", e)
	}

	events := splitFeeAmendEvents(t, al, dir)
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Actor != "SetSplitFeePerChunk" {
		t.Errorf("actor = %q, want SetSplitFeePerChunk", ev.Actor)
	}
	if ev.Details["previous_enabled"] != false || ev.Details["enabled"] != true {
		t.Errorf("details = %+v, want previous false / new true", ev.Details)
	}
	if ev.VersionBefore != versionBefore || ev.VersionAfter != versionBefore {
		t.Errorf("versions = %d/%d, want %d/%d (no bump)", ev.VersionBefore, ev.VersionAfter, versionBefore, versionBefore)
	}
}

func TestSetSplitFeePerChunkDisablesPerChunkFees(t *testing.T) {
	dir := t.TempDir()
	l, al := splitFeeAmendLedger(t, dir, true)

	// Construction-time mode is per-chunk.
	receipt, err := l.PostTransferSplit(Transfer{
		ID: "t-perchunk", From: "alice", To: "bob", AmountCents: 25000,
	})
	if err != nil {
		t.Fatalf("PostTransferSplit: %v", err)
	}
	if !receipt.FeePerChunk {
		t.Error("FeePerChunk = false, want true (construction-time mode)")
	}

	l.SetSplitFeePerChunk(false)

	receipt, err = l.PostTransferSplit(Transfer{
		ID: "t-once", From: "alice", To: "bob", AmountCents: 25000,
	})
	if err != nil {
		t.Fatalf("PostTransferSplit: %v", err)
	}
	if receipt.FeePerChunk {
		t.Error("FeePerChunk = true, want false (amended mode)")
	}
	if receipt.FeeCents != 625 {
		t.Errorf("FeeCents = %d, want 625", receipt.FeeCents)
	}
	if fe := findEntry(l, "t-once/fee"); fe.AmountCents != 625 {
		t.Errorf("once-per-transfer fee leg = %+v, want 625", fe)
	}
	for _, e := range l.Entries() {
		if strings.HasPrefix(e.ID, "t-once/chunk-") && strings.HasSuffix(e.ID, "/fee") {
			t.Errorf("unexpected per-chunk fee leg %q", e.ID)
		}
	}

	events := splitFeeAmendEvents(t, al, dir)
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
	if events[0].Details["previous_enabled"] != true || events[0].Details["enabled"] != false {
		t.Errorf("details = %+v, want previous true / new false", events[0].Details)
	}
}

func TestSetSplitFeePerChunkNoOp(t *testing.T) {
	dir := t.TempDir()
	l, al := splitFeeAmendLedger(t, dir, false)
	defer al.Close()

	// Amending to the current mode writes no audit event.
	l.SetSplitFeePerChunk(false)
	l.SetSplitFeePerChunk(true)
	l.SetSplitFeePerChunk(true)
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["split_fee_per_chunk_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1 (only the false->true change)", len(events))
	}
}

func TestSetSplitFeePerChunkCtxOperator(t *testing.T) {
	dir := t.TempDir()
	l, al := splitFeeAmendLedger(t, dir, false)

	ctx := ContextWithActor(context.Background(), "ops-desk")
	l.SetSplitFeePerChunkCtx(ctx, true)

	events := splitFeeAmendEvents(t, al, dir)
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
	if events[0].Actor != "ops-desk" {
		t.Errorf("actor = %q, want ops-desk (LG-73 attribution)", events[0].Actor)
	}
}

func TestSetSplitFeePerChunkAlreadySplitUntouched(t *testing.T) {
	dir := t.TempDir()
	l, al := splitFeeAmendLedger(t, dir, false)
	defer al.Close()

	before, err := l.PostTransferSplit(Transfer{
		ID: "t-before", From: "alice", To: "bob", AmountCents: 25000,
	})
	if err != nil {
		t.Fatalf("PostTransferSplit: %v", err)
	}
	if before.FeePerChunk || before.FeeCents != 625 {
		t.Fatalf("pre-amend receipt = %+v, want once-per-transfer 625", before)
	}

	l.SetSplitFeePerChunk(true)

	// The split committed before the amendment keeps its journaled
	// fee legs: one once-per-transfer leg, no per-chunk legs.
	if fe := findEntry(l, "t-before/fee"); fe.AmountCents != 625 {
		t.Errorf("pre-amend fee leg = %+v, want 625 (never rewritten)", fe)
	}
	for _, e := range l.Entries() {
		if strings.HasPrefix(e.ID, "t-before/chunk-") && strings.HasSuffix(e.ID, "/fee") {
			t.Errorf("pre-amend split gained a per-chunk fee leg %q", e.ID)
		}
	}

	// An explicit FeeCents is always charged once per transfer,
	// whatever the mode.
	receipt, err := l.PostTransferSplit(Transfer{
		ID: "t-exp", From: "alice", To: "bob", AmountCents: 25000, FeeCents: 101,
		FeeAccount: "fee-rev",
	})
	if err != nil {
		t.Fatalf("PostTransferSplit: %v", err)
	}
	if receipt.FeePerChunk {
		t.Error("FeePerChunk = true, want false (explicit fee is always once-per-transfer)")
	}
	if receipt.FeeCents != 101 {
		t.Errorf("FeeCents = %d, want 101", receipt.FeeCents)
	}
	if fe := findEntry(l, "t-exp/fee"); fe.AmountCents != 101 {
		t.Errorf("explicit fee leg = %+v, want 101", fe)
	}
}

func TestSetSplitFeePerChunkSnapshotRoundTrip(t *testing.T) {
	dir := t.TempDir()
	l, al := splitFeeAmendLedger(t, dir, false)
	defer al.Close()

	l.SetSplitFeePerChunk(true)
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	if !strings.Contains(buf.String(), `"split_fee_per_chunk":true`) {
		t.Error("snapshot config line lacks the amended split_fee_per_chunk true")
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if !snapshotLedgersEqual(l, restored) {
		t.Fatal("restored ledger state differs from source")
	}

	// The restored ledger fees per chunk under the amended mode, not
	// the construction-time one.
	seedBalance(t, restored, "alice", 1000000)
	receipt, err := restored.PostTransferSplit(Transfer{
		ID: "t-restored", From: "alice", To: "bob", AmountCents: 25000,
	})
	if err != nil {
		t.Fatalf("PostTransferSplit on restored: %v", err)
	}
	if !receipt.FeePerChunk {
		t.Error("FeePerChunk = false, want true (amended mode survived)")
	}
	if receipt.FeeCents != 625 {
		t.Errorf("FeeCents = %d, want 625", receipt.FeeCents)
	}
}

func TestSetSplitFeePerChunkConcurrent(t *testing.T) {
	dir := t.TempDir()
	l, al := splitFeeAmendLedger(t, dir, false)
	defer al.Close()

	var wg sync.WaitGroup
	// Two goroutines flip the mode while two post splits: every split
	// commits wholly under one live mode, and the final state must
	// stay consistent.
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 8; j++ {
				l.SetSplitFeePerChunk(j%2 == 0)
			}
		}()
	}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 4; j++ {
				id := "t-conc-" + itoa(i*4+j)
				if _, err := l.PostTransferSplit(Transfer{
					ID: id, From: "alice", To: "bob", AmountCents: 25000,
				}); err != nil {
					t.Errorf("PostTransferSplit %s: %v", id, err)
				}
			}
		}(i)
	}
	wg.Wait()
}
