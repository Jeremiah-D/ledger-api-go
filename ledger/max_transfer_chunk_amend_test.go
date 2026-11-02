package ledger

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// chunkAmendLedger builds a ledger with an audit log in dir, a
// 10000-cent chunk cap, and alice funded with 1,000,000 cents.
func chunkAmendLedger(t *testing.T, dir string) (*Ledger, *AuditLog) {
	t.Helper()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithMaxTransferChunkCents(10000))
	seedBalance(t, l, "alice", 1000000)
	return l, al
}

// chunkAmendEvents returns the max_transfer_chunk_cents_amended audit
// events recorded in dir, closing the log first so the read sees
// every sealed event.
func chunkAmendEvents(t *testing.T, al *AuditLog, dir string) []AuditEvent {
	t.Helper()
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return auditEventsByOp(t, dir)["max_transfer_chunk_cents_amended"]
}

func TestSetMaxTransferChunkCentsAppliesToLaterSplits(t *testing.T) {
	dir := t.TempDir()
	l, al := chunkAmendLedger(t, dir)
	versionBefore := l.Version()

	if err := l.SetMaxTransferChunkCents(4000); err != nil {
		t.Fatalf("SetMaxTransferChunkCents: %v", err)
	}
	if got := l.Version(); got != versionBefore {
		t.Errorf("Version after amend = %d, want %d (structural, no bump)", got, versionBefore)
	}

	receipt, err := l.PostTransferSplit(Transfer{
		ID: "t-amended", From: "alice", To: "bob", AmountCents: 9000,
	})
	if err != nil {
		t.Fatalf("PostTransferSplit: %v", err)
	}
	wantAmounts := []int64{4000, 4000, 1000}
	if len(receipt.Chunks) != len(wantAmounts) {
		t.Fatalf("chunks = %d, want %d (new cap must govern later splits)", len(receipt.Chunks), len(wantAmounts))
	}
	for i, c := range receipt.Chunks {
		if c.AmountCents != wantAmounts[i] {
			t.Errorf("chunk %d amount = %d, want %d", i, c.AmountCents, wantAmounts[i])
		}
	}
	if l.Balance("bob") != 9000 || l.Balance("alice") != 991000 {
		t.Errorf("balances: bob=%d alice=%d, want 9000/991000", l.Balance("bob"), l.Balance("alice"))
	}

	events := chunkAmendEvents(t, al, dir)
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Actor != "SetMaxTransferChunkCents" {
		t.Errorf("actor = %q, want SetMaxTransferChunkCents", ev.Actor)
	}
	if ev.Details["previous_chunk_cents"] != float64(10000) || ev.Details["chunk_cents"] != float64(4000) {
		t.Errorf("details = %+v, want previous 10000 / new 4000", ev.Details)
	}
	if ev.VersionBefore != versionBefore || ev.VersionAfter != versionBefore {
		t.Errorf("versions = %d/%d, want %d/%d (no bump)", ev.VersionBefore, ev.VersionAfter, versionBefore, versionBefore)
	}
}

func TestSetMaxTransferChunkCentsNegativeRejected(t *testing.T) {
	dir := t.TempDir()
	l, al := chunkAmendLedger(t, dir)

	err := l.SetMaxTransferChunkCents(-1)
	if !errors.Is(err, ErrInvalidMaxTransferChunkCents) {
		t.Fatalf("negative amend error = %v, want ErrInvalidMaxTransferChunkCents", err)
	}

	// The rejection leaves the previous cap exactly as it was: a
	// later split still chunks at 10000, never silently normalized.
	receipt, err := l.PostTransferSplit(Transfer{
		ID: "t-still-capped", From: "alice", To: "bob", AmountCents: 25000,
	})
	if err != nil {
		t.Fatalf("PostTransferSplit: %v", err)
	}
	if len(receipt.Chunks) != 3 {
		t.Fatalf("chunks = %d, want 3 (original cap must survive a rejected amend)", len(receipt.Chunks))
	}
	for i, c := range receipt.Chunks {
		want := int64(10000)
		if i == 2 {
			want = 5000
		}
		if c.AmountCents != want {
			t.Errorf("chunk %d amount = %d, want %d", i, c.AmountCents, want)
		}
	}

	// A rejected amendment writes no audit event.
	if events := chunkAmendEvents(t, al, dir); len(events) != 0 {
		t.Fatalf("amend events = %d, want 0 (rejection is silent)", len(events))
	}
}

func TestSetMaxTransferChunkCentsZeroDisablesChunking(t *testing.T) {
	dir := t.TempDir()
	l, al := chunkAmendLedger(t, dir)

	if err := l.SetMaxTransferChunkCents(0); err != nil {
		t.Fatalf("SetMaxTransferChunkCents(0): %v", err)
	}
	receipt, err := l.PostTransferSplit(Transfer{
		ID: "t-uncapped", From: "alice", To: "bob", AmountCents: 25000,
	})
	if err != nil {
		t.Fatalf("PostTransferSplit: %v", err)
	}
	if len(receipt.Chunks) != 1 || receipt.Chunks[0].AmountCents != 25000 {
		t.Fatalf("chunks = %+v, want one 25000 chunk (0 closes chunking)", receipt.Chunks)
	}
	if len(receipt.Chunks[0].EntryIDs) != 1 || receipt.Chunks[0].EntryIDs[0] != "t-uncapped" {
		t.Errorf("chunk entry IDs = %v, want [t-uncapped] (plain PostTransfer shape)", receipt.Chunks[0].EntryIDs)
	}

	events := chunkAmendEvents(t, al, dir)
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
	if events[0].Details["previous_chunk_cents"] != float64(10000) || events[0].Details["chunk_cents"] != float64(0) {
		t.Errorf("details = %+v, want previous 10000 / new 0", events[0].Details)
	}
}

func TestSetMaxTransferChunkCentsSameValueNoOp(t *testing.T) {
	dir := t.TempDir()
	l, al := chunkAmendLedger(t, dir)
	versionBefore := l.Version()

	// Amending to the current cap, and amending again to a cap that
	// was already installed, are both silent.
	if err := l.SetMaxTransferChunkCents(10000); err != nil {
		t.Fatalf("same-value amend: %v", err)
	}
	if err := l.SetMaxTransferChunkCents(4000); err != nil {
		t.Fatalf("SetMaxTransferChunkCents(4000): %v", err)
	}
	if err := l.SetMaxTransferChunkCents(4000); err != nil {
		t.Fatalf("repeat amend: %v", err)
	}
	if got := l.Version(); got != versionBefore {
		t.Errorf("Version = %d, want %d (no bump, including no-ops)", got, versionBefore)
	}

	events := chunkAmendEvents(t, al, dir)
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1 (both same-value installs are silent)", len(events))
	}
	if events[0].Details["previous_chunk_cents"] != float64(10000) || events[0].Details["chunk_cents"] != float64(4000) {
		t.Errorf("details = %+v, want previous 10000 / new 4000", events[0].Details)
	}
}

func TestSetMaxTransferChunkCentsCtxOperator(t *testing.T) {
	dir := t.TempDir()
	l, al := chunkAmendLedger(t, dir)

	ctx := ContextWithActor(context.Background(), "ops-desk")
	if err := l.SetMaxTransferChunkCentsCtx(ctx, 4000); err != nil {
		t.Fatalf("SetMaxTransferChunkCentsCtx: %v", err)
	}
	// The plain twin keeps the entrypoint actor when no operator is
	// carried.
	if err := l.SetMaxTransferChunkCents(6000); err != nil {
		t.Fatalf("SetMaxTransferChunkCents: %v", err)
	}

	events := chunkAmendEvents(t, al, dir)
	if len(events) != 2 {
		t.Fatalf("amend events = %d, want 2", len(events))
	}
	if events[0].Actor != "ops-desk" {
		t.Errorf("Ctx event actor = %q, want ops-desk (LG-73)", events[0].Actor)
	}
	if events[1].Actor != "SetMaxTransferChunkCents" {
		t.Errorf("plain event actor = %q, want SetMaxTransferChunkCents", events[1].Actor)
	}
	if events[0].Details["chunk_cents"] != float64(4000) || events[1].Details["chunk_cents"] != float64(6000) {
		t.Errorf("chunk caps = %v / %v, want 4000 / 6000", events[0].Details["chunk_cents"], events[1].Details["chunk_cents"])
	}
}

func TestSetMaxTransferChunkCentsAlreadySplitUntouched(t *testing.T) {
	dir := t.TempDir()
	l, al := chunkAmendLedger(t, dir)

	first, err := l.PostTransferSplit(Transfer{
		ID: "t-before", From: "alice", To: "bob", AmountCents: 25000,
		IdempotencyKey: "t-before",
	})
	if err != nil {
		t.Fatalf("PostTransferSplit: %v", err)
	}
	if len(first.Chunks) != 3 {
		t.Fatalf("chunks before amend = %d, want 3", len(first.Chunks))
	}
	entriesBefore := len(l.Entries())

	if err := l.SetMaxTransferChunkCents(4000); err != nil {
		t.Fatalf("SetMaxTransferChunkCents: %v", err)
	}

	// Replaying the pre-amendment split returns the original receipt
	// — same chunk sizes, Duplicate — not a re-chunk under the new
	// cap, and books nothing new.
	replay, err := l.PostTransferSplit(Transfer{
		ID: "t-before", From: "alice", To: "bob", AmountCents: 25000,
		IdempotencyKey: "t-before",
	})
	if err != nil {
		t.Fatalf("replay PostTransferSplit: %v", err)
	}
	if !replay.Duplicate {
		t.Error("replay Duplicate = false, want true")
	}
	if len(replay.Chunks) != 3 {
		t.Fatalf("replay chunks = %d, want 3 (original chunking preserved)", len(replay.Chunks))
	}
	for i, c := range replay.Chunks {
		if c.AmountCents != first.Chunks[i].AmountCents {
			t.Errorf("replay chunk %d amount = %d, want %d", i, c.AmountCents, first.Chunks[i].AmountCents)
		}
	}
	if n := len(l.Entries()); n != entriesBefore {
		t.Errorf("journal entries = %d, want unchanged %d", n, entriesBefore)
	}

	// A fresh split after the amendment uses the new cap.
	second, err := l.PostTransferSplit(Transfer{
		ID: "t-after", From: "alice", To: "bob", AmountCents: 9000,
	})
	if err != nil {
		t.Fatalf("PostTransferSplit after amend: %v", err)
	}
	if len(second.Chunks) != 3 || second.Chunks[0].AmountCents != 4000 {
		t.Fatalf("chunks after amend = %+v, want 3 starting at 4000", second.Chunks)
	}
	if err := l.VerifyChain(); err != nil {
		t.Errorf("VerifyChain: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestSetMaxTransferChunkCentsSnapshotRoundTrip(t *testing.T) {
	dir := t.TempDir()
	l, al := chunkAmendLedger(t, dir)
	defer al.Close()

	if err := l.SetMaxTransferChunkCents(4000); err != nil {
		t.Fatalf("SetMaxTransferChunkCents: %v", err)
	}
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	if !strings.Contains(buf.String(), `"max_transfer_chunk_cents":4000`) {
		t.Error("snapshot config line lacks the amended max_transfer_chunk_cents 4000")
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if !snapshotLedgersEqual(l, restored) {
		t.Fatal("restored ledger state differs from source")
	}

	// The restored ledger splits under the amended cap, not the
	// construction-time one.
	seedBalance(t, restored, "alice", 1000000)
	receipt, err := restored.PostTransferSplit(Transfer{
		ID: "t-restored", From: "alice", To: "bob", AmountCents: 9000,
	})
	if err != nil {
		t.Fatalf("PostTransferSplit on restored: %v", err)
	}
	if len(receipt.Chunks) != 3 || receipt.Chunks[0].AmountCents != 4000 {
		t.Fatalf("restored chunks = %+v, want 3 starting at 4000 (amended cap survived)", receipt.Chunks)
	}
}

func TestSetMaxTransferChunkCentsConcurrent(t *testing.T) {
	dir := t.TempDir()
	l, al := chunkAmendLedger(t, dir)
	defer al.Close()

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	// Four goroutines revise the cap while four post splits: every
	// split commits wholly under one live cap, and the final state
	// must stay consistent.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for _, cap := range []int64{4000, 6000, 8000, 10000} {
				if err := l.SetMaxTransferChunkCents(cap); err != nil {
					errs <- err
					return
				}
			}
		}(i)
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 4; j++ {
				id := "t-conc-" + string(rune('a'+i)) + "-" + string(rune('0'+j))
				receipt, err := l.PostTransferSplit(Transfer{
					ID: id, From: "alice", To: "bob", AmountCents: 12000,
				})
				if err != nil {
					errs <- err
					return
				}
				var total int64
				for _, c := range receipt.Chunks {
					total += c.AmountCents
				}
				if total != 12000 {
					errs <- errors.New("chunk amounts do not sum to the transfer amount")
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent amend/split: %v", err)
	}
	if got := l.Balance("bob"); got != 16*12000 {
		t.Errorf("bob balance = %d, want %d", got, 16*12000)
	}
	if got := l.Balance("alice"); got != 1000000-16*12000 {
		t.Errorf("alice balance = %d, want %d", got, 1000000-16*12000)
	}
	if err := l.VerifyChain(); err != nil {
		t.Errorf("VerifyChain: %v", err)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Errorf("VerifyAccountingEquation: %v", err)
	}
}
