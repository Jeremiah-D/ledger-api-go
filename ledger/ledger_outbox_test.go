package ledger

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// newOutboxLedger builds a ledger with a fresh outbox in a temp dir.
// The outbox is closed on test cleanup.
func newOutboxLedger(t *testing.T, opts ...Option) (*Ledger, *Outbox, string) {
	t.Helper()
	dir := t.TempDir()
	ob, err := NewOutbox(dir)
	if err != nil {
		t.Fatalf("NewOutbox: %v", err)
	}
	t.Cleanup(func() { _ = ob.Close() })
	opts = append([]Option{WithOutbox(ob)}, opts...)
	return New(opts...), ob, dir
}

func readAllOutbox(t *testing.T, dir string) []OutboxEvent {
	t.Helper()
	evs, next, corrupt, err := ReadOutboxEvents(dir, OutboxQuery{Limit: 1000})
	if err != nil {
		t.Fatalf("ReadOutboxEvents: %v", err)
	}
	if next != "" {
		t.Fatalf("next cursor = %q, want empty for full read", next)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt = %d, want 0", corrupt)
	}
	return evs
}

func eventTypes(evs []OutboxEvent) []string {
	types := make([]string, len(evs))
	for i, e := range evs {
		types[i] = e.Type
	}
	return types
}

func TestOutboxPostEmitsEntryPosted(t *testing.T) {
	l, _, dir := newOutboxLedger(t)
	e := JournalEntry{
		ID:            "ob-e1",
		DebitAccount:  "cash",
		CreditAccount: "equity",
		AmountCents:   1500,
		Currency:      "USD",
		Memo:          "order-42",
		CreatedAt:     time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC),
	}
	if _, dup, err := l.Post(e); err != nil || dup {
		t.Fatalf("Post = dup=%v err=%v", dup, err)
	}
	evs := readAllOutbox(t, dir)
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	ev := evs[0]
	if ev.Seq != 1 || ev.Type != OutboxEntryPosted {
		t.Errorf("seq/type = %d/%q, want 1/entry_posted", ev.Seq, ev.Type)
	}
	if ev.Account != "cash" || ev.EntryID != "ob-e1" {
		t.Errorf("account/entry = %q/%q, want cash/ob-e1", ev.Account, ev.EntryID)
	}
	if ev.AmountCents != 1500 || ev.Currency != "USD" {
		t.Errorf("amount/currency = %d/%q, want 1500/USD", ev.AmountCents, ev.Currency)
	}
	if ev.Details["debit_account"] != "cash" || ev.Details["credit_account"] != "equity" {
		t.Errorf("legs = %v, want cash/equity", ev.Details)
	}
	if ev.Details["memo"] != "order-42" {
		t.Errorf("memo = %v, want order-42", ev.Details["memo"])
	}
	if ev.At.IsZero() {
		t.Error("At is zero, want commit timestamp")
	}
}

func TestOutboxPostReplayEmitsNothing(t *testing.T) {
	l, _, dir := newOutboxLedger(t)
	e := JournalEntry{ID: "ob-e2", DebitAccount: "cash", CreditAccount: "equity",
		AmountCents: 100, IdempotencyKey: "k-replay"}
	if _, _, err := l.Post(e); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if _, dup, err := l.Post(e); err != nil || !dup {
		t.Fatalf("replay Post = dup=%v err=%v, want dup=true", dup, err)
	}
	if evs := readAllOutbox(t, dir); len(evs) != 1 {
		t.Fatalf("events after replay = %d, want 1 (no duplicate event)", len(evs))
	}
}

func TestOutboxTransferEmitsLegsAndEnvelope(t *testing.T) {
	l, _, dir := newOutboxLedger(t, WithTransferFeePolicy(250, "fee-revenue"))
	tr := Transfer{ID: "ob-t1", From: "alice", To: "bob", AmountCents: 10000,
		Currency: "USD", IdempotencyKey: "k-t1"}
	if _, err := l.PostTransfer(tr); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	evs := readAllOutbox(t, dir)
	// principal + fee leg, then the transfer_posted envelope.
	if len(evs) != 3 {
		t.Fatalf("events = %d (%v), want 3", len(evs), eventTypes(evs))
	}
	for _, ev := range evs[:2] {
		if ev.Type != OutboxEntryPosted {
			t.Errorf("leg event type = %q, want entry_posted", ev.Type)
		}
	}
	env := evs[2]
	if env.Type != OutboxTransferPosted {
		t.Fatalf("envelope type = %q, want transfer_posted", env.Type)
	}
	if env.Account != "alice" || env.AmountCents != 10000 {
		t.Errorf("envelope account/amount = %q/%d, want alice/10000", env.Account, env.AmountCents)
	}
	if env.Details["to"] != "bob" {
		t.Errorf("envelope to = %v, want bob", env.Details["to"])
	}
	if env.EntryID != evs[0].EntryID {
		t.Errorf("envelope entry_id = %q, want principal %q", env.EntryID, evs[0].EntryID)
	}
	// Seq order is commit order: legs before envelope.
	if !(evs[0].Seq < evs[1].Seq && evs[1].Seq < evs[2].Seq) {
		t.Errorf("seqs not increasing: %d %d %d", evs[0].Seq, evs[1].Seq, evs[2].Seq)
	}
}

func TestOutboxHoldLifecycle(t *testing.T) {
	l, _, dir := newOutboxLedger(t)
	if _, _, err := l.Post(JournalEntry{ID: "ob-fund", DebitAccount: "alice",
		CreditAccount: "equity", AmountCents: 5000}); err != nil {
		t.Fatalf("fund: %v", err)
	}
	h, _, err := l.Hold(Hold{ID: "ob-h1", Account: "alice", AmountCents: 1000,
		Currency: "USD", ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	_ = h
	if _, err := l.Capture(Capture{ID: "ob-c1", HoldID: "ob-h1", To: "bob", AmountCents: 600}); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	h2, _, err := l.Hold(Hold{ID: "ob-h2", Account: "alice", AmountCents: 500,
		Currency: "USD", ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatalf("Hold 2: %v", err)
	}
	_ = h2
	if _, err := l.Release("ob-h2"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	h3, _, err := l.Hold(Hold{ID: "ob-h3", Account: "alice", AmountCents: 100,
		Currency: "USD", ExpiresAt: time.Now().Add(-time.Minute)})
	if err != nil {
		t.Fatalf("Hold 3: %v", err)
	}
	_ = h3
	if n := l.ExpireHoldsAt(time.Now()); n != 1 {
		t.Fatalf("ExpireHoldsAt = %d, want 1", n)
	}

	evs := readAllOutbox(t, dir)
	var types []string
	for _, ev := range evs {
		types = append(types, ev.Type)
	}
	want := []string{
		OutboxEntryPosted,  // fund
		OutboxHoldCreated,  // ob-h1
		OutboxEntryPosted,  // capture leg
		OutboxHoldCaptured, // ob-h1
		OutboxHoldCreated,  // ob-h2
		OutboxHoldReleased, // ob-h2
		OutboxHoldCreated,  // ob-h3
		OutboxHoldExpired,  // ob-h3
	}
	if len(types) != len(want) {
		t.Fatalf("types = %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("types = %v, want %v", types, want)
		}
	}
	// Monotonic seq across the whole lifecycle.
	for i := 1; i < len(evs); i++ {
		if evs[i].Seq != evs[i-1].Seq+1 {
			t.Fatalf("seq not contiguous at %d: %d -> %d", i, evs[i-1].Seq, evs[i].Seq)
		}
	}
	// The capture envelope names the hold and the settlement entry.
	var captured *OutboxEvent
	for i := range evs {
		if evs[i].Type == OutboxHoldCaptured {
			captured = &evs[i]
		}
	}
	if captured == nil {
		t.Fatal("no hold_captured event")
	}
	if captured.Details["hold_id"] != "ob-h1" || captured.Details["to"] != "bob" {
		t.Errorf("captured details = %v", captured.Details)
	}
}

func TestOutboxFreezeUnfreeze(t *testing.T) {
	l, _, dir := newOutboxLedger(t)
	l.Freeze("mallory")
	l.Unfreeze("mallory")
	evs := readAllOutbox(t, dir)
	if len(evs) != 2 || evs[0].Type != OutboxAccountFrozen || evs[1].Type != OutboxAccountUnfrozen {
		t.Fatalf("types = %v, want [account_frozen account_unfrozen]", eventTypes(evs))
	}
	if evs[0].Account != "mallory" {
		t.Errorf("account = %q, want mallory", evs[0].Account)
	}
}

func TestOutboxDisabledIsNoop(t *testing.T) {
	l := New() // no outbox attached
	if _, _, err := l.Post(JournalEntry{ID: "x1", DebitAccount: "a", CreditAccount: "b", AmountCents: 1}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	l.Freeze("a")
	if _, _, ok := l.OutboxStats(); ok {
		t.Error("OutboxStats enabled = true, want false")
	}
	if _, ok := l.OutboxDir(); ok {
		t.Error("OutboxDir ok = true, want false")
	}
}

func TestOutboxSeqContinuesAfterRestart(t *testing.T) {
	dir := t.TempDir()
	ob1, err := NewOutbox(dir)
	if err != nil {
		t.Fatalf("NewOutbox: %v", err)
	}
	l1 := New(WithOutbox(ob1))
	if _, _, err := l1.Post(JournalEntry{ID: "r1", DebitAccount: "a", CreditAccount: "b", AmountCents: 1}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if err := ob1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// "Restart": a new Outbox on the same directory continues the seq.
	ob2, err := NewOutbox(dir)
	if err != nil {
		t.Fatalf("NewOutbox 2: %v", err)
	}
	defer ob2.Close()
	l2 := New(WithOutbox(ob2))
	if _, _, err := l2.Post(JournalEntry{ID: "r2", DebitAccount: "a", CreditAccount: "b", AmountCents: 1}); err != nil {
		t.Fatalf("Post 2: %v", err)
	}
	evs := readAllOutbox(t, dir)
	if len(evs) != 2 || evs[0].Seq != 1 || evs[1].Seq != 2 {
		t.Fatalf("seqs = %v, want [1 2] (no rewind across restart)", []uint64{evs[0].Seq, evs[1].Seq})
	}
}

func TestOutboxCrashDurability(t *testing.T) {
	// Crash simulation: emit events, then abandon the handle without
	// Close (like a killed process). The per-event fsync means the
	// events are already durable — reopening must see them all.
	dir := t.TempDir()
	ob, err := NewOutbox(dir)
	if err != nil {
		t.Fatalf("NewOutbox: %v", err)
	}
	l := New(WithOutbox(ob))
	for i := 0; i < 5; i++ {
		e := JournalEntry{ID: "crash-" + string(rune('a'+i)), DebitAccount: "a",
			CreditAccount: "b", AmountCents: int64(i + 1)}
		if _, _, err := l.Post(e); err != nil {
			t.Fatalf("Post: %v", err)
		}
	}
	// No Close — the handle is simply abandoned, like a killed process.
	evs := readAllOutbox(t, dir)
	if len(evs) != 5 {
		t.Fatalf("events after simulated crash = %d, want 5 (fsync per event)", len(evs))
	}
	for i, ev := range evs {
		if ev.Seq != uint64(i+1) {
			t.Fatalf("seq[%d] = %d, want %d", i, ev.Seq, i+1)
		}
	}
}

func TestOutboxCorruptLineSkipped(t *testing.T) {
	l, ob, dir := newOutboxLedger(t)
	if _, _, err := l.Post(JournalEntry{ID: "c1", DebitAccount: "a", CreditAccount: "b", AmountCents: 1}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if err := ob.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Append a corrupt line by hand, plus a blank line.
	matches, _ := filepath.Glob(filepath.Join(dir, "outbox-*.jsonl"))
	fh, err := os.OpenFile(matches[0], os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_, _ = fh.WriteString("this is not json\n\n{\"seq\": 999999}\n")
	_ = fh.Close()

	evs, _, corrupt, err := ReadOutboxEvents(dir, OutboxQuery{Limit: 1000})
	if err != nil {
		t.Fatalf("ReadOutboxEvents: %v", err)
	}
	if corrupt != 2 {
		t.Errorf("corrupt = %d, want 2 (the non-JSON line and the typeless line; blank lines are ignored)", corrupt)
	}
	if len(evs) != 1 || evs[0].EntryID != "c1" {
		t.Errorf("events = %v, want the single intact event", evs)
	}
}

func TestOutboxReviewApproveReject(t *testing.T) {
	l, _, dir := newOutboxLedger(t, WithReviewThreshold(1000))
	// Fund the payer first.
	if _, _, err := l.Post(JournalEntry{ID: "rv-fund", DebitAccount: "payer",
		CreditAccount: "equity", AmountCents: 100000}); err != nil {
		t.Fatalf("fund: %v", err)
	}
	tr := Transfer{ID: "rv-t1", From: "payer", To: "payee", AmountCents: 5000}
	rcpt, err := l.PostTransfer(tr)
	if err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if rcpt.ReviewStatus != "pending_review" {
		t.Fatalf("ReviewStatus = %q, want pending_review", rcpt.ReviewStatus)
	}
	// While pending, no domain events for the transfer: nothing settled.
	if evs := readAllOutbox(t, dir); len(evs) != 1 {
		t.Fatalf("events while pending = %d, want 1 (only the funding post)", len(evs))
	}
	if _, err := l.ApproveTransferReview("rv-t1", "op-1"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	evs := readAllOutbox(t, dir)
	types := eventTypes(evs)
	// funding entry_posted, then the approval's entry_posted + review_approved.
	if len(types) != 3 || types[1] != OutboxEntryPosted || types[2] != OutboxReviewApproved {
		t.Fatalf("types = %v, want [entry_posted entry_posted review_approved]", types)
	}
	if evs[2].Details["reviewed_by"] != "op-1" || evs[2].Details["transfer_id"] != "rv-t1" {
		t.Errorf("review_approved details = %v", evs[2].Details)
	}

	// Reject path: a second transfer, rejected.
	tr2 := Transfer{ID: "rv-t2", From: "payer", To: "payee", AmountCents: 6000}
	if _, err := l.PostTransfer(tr2); err != nil {
		t.Fatalf("PostTransfer 2: %v", err)
	}
	if _, _, err := l.RejectTransferReview("rv-t2", "op-2", "suspicious"); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	evs = readAllOutbox(t, dir)
	last := evs[len(evs)-1]
	if last.Type != OutboxReviewRejected {
		t.Fatalf("last type = %q, want review_rejected", last.Type)
	}
	if last.Details["reason"] != "suspicious" {
		t.Errorf("reject details = %v", last.Details)
	}
	// No entry_posted for the rejected transfer: nothing settled.
	for _, ev := range evs {
		if ev.Type == OutboxEntryPosted && ev.Details["transfer_id"] == "rv-t2" {
			t.Errorf("rejected transfer emitted entry_posted: %+v", ev)
		}
	}
}

func TestOutboxReviewExpired(t *testing.T) {
	l, _, dir := newOutboxLedger(t, WithReviewThreshold(1000), WithReviewExpiry(time.Hour))
	if _, _, err := l.Post(JournalEntry{ID: "rx-fund", DebitAccount: "payer",
		CreditAccount: "equity", AmountCents: 100000}); err != nil {
		t.Fatalf("fund: %v", err)
	}
	if _, err := l.PostTransfer(Transfer{ID: "rx-t1", From: "payer", To: "payee", AmountCents: 5000}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	// Push the clock past the review expiry.
	if n := l.ExpireReviewsAt(time.Now().Add(2 * time.Hour)); n != 1 {
		t.Fatalf("ExpireReviewsAt = %d, want 1", n)
	}
	evs := readAllOutbox(t, dir)
	last := evs[len(evs)-1]
	if last.Type != OutboxReviewExpired {
		t.Fatalf("last type = %q, want review_expired (got %v)", last.Type, eventTypes(evs))
	}
	if last.Details["transfer_id"] != "rx-t1" || last.Account != "payer" {
		t.Errorf("review_expired = %+v", last)
	}
	// Nothing settled: no entry_posted for the expired transfer.
	for _, ev := range evs {
		if ev.Type == OutboxEntryPosted && ev.EntryID == "rx-t1" {
			t.Errorf("expired transfer emitted entry_posted: %+v", ev)
		}
	}
}

func TestOutboxQueryFilters(t *testing.T) {
	_, ob, dir := newOutboxLedger(t)
	base := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	// Emit raw events with pinned timestamps for deterministic filters.
	for i, tc := range []struct {
		typ     string
		account AccountID
		at      time.Time
	}{
		{OutboxEntryPosted, "alice", base},
		{OutboxTransferPosted, "bob", base.Add(time.Hour)},
		{OutboxAccountFrozen, "alice", base.Add(2 * time.Hour)},
	} {
		ev := OutboxEvent{Type: tc.typ, Account: tc.account, At: tc.at,
			EntryID: "q" + string(rune('0'+i))}
		if !ob.Emit(ev) {
			t.Fatalf("Emit %d: dropped", i)
		}
	}

	// Type filter.
	evs, _, _, err := ReadOutboxEvents(dir, OutboxQuery{Types: []string{OutboxTransferPosted}, Limit: 100})
	if err != nil || len(evs) != 1 || evs[0].Type != OutboxTransferPosted {
		t.Fatalf("type filter = %v, %v", evs, err)
	}
	// Multi-type filter.
	evs, _, _, err = ReadOutboxEvents(dir, OutboxQuery{Types: []string{OutboxEntryPosted, OutboxAccountFrozen}, Limit: 100})
	if err != nil || len(evs) != 2 {
		t.Fatalf("multi-type filter = %d, %v", len(evs), err)
	}
	// Account filter.
	evs, _, _, err = ReadOutboxEvents(dir, OutboxQuery{Account: "alice", Limit: 100})
	if err != nil || len(evs) != 2 {
		t.Fatalf("account filter = %d, %v", len(evs), err)
	}
	// Time window: [base+30m, base+90m) catches only the transfer.
	evs, _, _, err = ReadOutboxEvents(dir, OutboxQuery{
		Since: base.Add(30 * time.Minute), Until: base.Add(90 * time.Minute), Limit: 100})
	if err != nil || len(evs) != 1 || evs[0].Type != OutboxTransferPosted {
		t.Fatalf("time filter = %v, %v", evs, err)
	}
	// Cursor pagination: page 1 of 2, then resume.
	page1, next, _, err := ReadOutboxEvents(dir, OutboxQuery{Limit: 2})
	if err != nil || len(page1) != 2 || next == "" {
		t.Fatalf("page1 = %d next=%q err=%v", len(page1), next, err)
	}
	after, err := DecodeOutboxCursor(next)
	if err != nil || after != page1[1].Seq {
		t.Fatalf("cursor decodes to %d, want %d", after, page1[1].Seq)
	}
	page2, next2, _, err := ReadOutboxEvents(dir, OutboxQuery{Limit: 2, AfterSeq: after})
	if err != nil || len(page2) != 1 || next2 != "" {
		t.Fatalf("page2 = %d next=%q err=%v", len(page2), next2, err)
	}
	if page2[0].Seq != 3 {
		t.Fatalf("page2 seq = %d, want 3", page2[0].Seq)
	}
	// Bad cursor and bad limit are 400-class errors.
	if _, _, _, err := ReadOutboxEvents(dir, OutboxQuery{Limit: 0}); err != ErrInvalidLimit {
		t.Fatalf("limit 0 err = %v, want ErrInvalidLimit", err)
	}
	if _, err := DecodeOutboxCursor("!!!not-base64!!!"); err != ErrInvalidCursor {
		t.Fatalf("bad cursor err = %v, want ErrInvalidCursor", err)
	}
}

func TestOutboxAccountFilterMatchesEitherLeg(t *testing.T) {
	l, _, dir := newOutboxLedger(t)
	if _, _, err := l.Post(JournalEntry{ID: "leg1", DebitAccount: "debit-acct",
		CreditAccount: "credit-acct", AmountCents: 10}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	// The event is filed under the debit account, but the credit side
	// must match the account filter too.
	for _, acct := range []string{"debit-acct", "credit-acct"} {
		evs, _, _, err := ReadOutboxEvents(dir, OutboxQuery{Account: AccountID(acct), Limit: 100})
		if err != nil || len(evs) != 1 {
			t.Fatalf("account=%s: events=%d err=%v", acct, len(evs), err)
		}
	}
	evs, _, _, err := ReadOutboxEvents(dir, OutboxQuery{Account: "nobody", Limit: 100})
	if err != nil || len(evs) != 0 {
		t.Fatalf("account=nobody: events=%d err=%v", len(evs), err)
	}
}

func TestOutboxMergeAndReversal(t *testing.T) {
	l, _, dir := newOutboxLedger(t)
	for _, e := range []JournalEntry{
		{ID: "m-f1", DebitAccount: "src", CreditAccount: "equity", AmountCents: 300, Currency: "USD"},
		{ID: "m-f2", DebitAccount: "src", CreditAccount: "equity", AmountCents: 200, Currency: "EUR"},
	} {
		if _, _, err := l.Post(e); err != nil {
			t.Fatalf("fund: %v", err)
		}
	}
	if _, err := l.PostMerge(Merge{ID: "m1", From: "src", To: "dst"}); err != nil {
		t.Fatalf("PostMerge: %v", err)
	}
	// The merge froze "src", so the reversal targets a fresh entry on
	// unfrozen accounts: reversing m-f1 itself would (correctly) hit
	// ErrAccountFrozen.
	if _, _, err := l.Post(JournalEntry{ID: "m-f3", DebitAccount: "dst",
		CreditAccount: "equity", AmountCents: 50}); err != nil {
		t.Fatalf("post m-f3: %v", err)
	}
	if _, _, err := l.ReverseEntry("m-f3", ""); err != nil {
		t.Fatalf("ReverseEntry: %v", err)
	}
	evs := readAllOutbox(t, dir)
	types := eventTypes(evs)
	// 2 funding entry_posted, 2 merge-leg entry_posted, merge's
	// account_frozen, m-f3 entry_posted, then reversal entry_posted +
	// entry_reversed.
	want := []string{
		OutboxEntryPosted, OutboxEntryPosted,
		OutboxEntryPosted, OutboxEntryPosted, OutboxAccountFrozen,
		OutboxEntryPosted,
		OutboxEntryPosted, OutboxEntryReversed,
	}
	if len(types) != len(want) {
		t.Fatalf("types = %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("types = %v, want %v", types, want)
		}
	}
	if evs[4].Account != "src" || evs[4].Details["reason"] != "merge" {
		t.Errorf("merge freeze event = %+v", evs[4])
	}
	if evs[7].Details["reversed_entry_id"] != "m-f3" {
		t.Errorf("reversed details = %v", evs[7].Details)
	}
}

func TestOutboxBatchFreshOnly(t *testing.T) {
	l, _, dir := newOutboxLedger(t)
	// Pre-post one entry with a key; the batch will replay it.
	first := JournalEntry{ID: "b-pre", DebitAccount: "a", CreditAccount: "b",
		AmountCents: 5, IdempotencyKey: "b-key-1"}
	if _, _, err := l.Post(first); err != nil {
		t.Fatalf("pre Post: %v", err)
	}
	batch := Batch{
		ID: "b1",
		Entries: []JournalEntry{
			{ID: "b-pre", DebitAccount: "a", CreditAccount: "b", AmountCents: 5, IdempotencyKey: "b-key-1"},
			{ID: "b-new", DebitAccount: "a", CreditAccount: "b", AmountCents: 7},
		},
	}
	if _, err := l.PostBatch(batch); err != nil {
		t.Fatalf("PostBatch: %v", err)
	}
	evs := readAllOutbox(t, dir)
	// The replayed entry must not emit twice: pre-post + fresh batch leg.
	if len(evs) != 2 {
		t.Fatalf("events = %d (%v), want 2", len(evs), eventTypes(evs))
	}
	if evs[1].EntryID != "b-new" {
		t.Errorf("second event entry = %q, want b-new", evs[1].EntryID)
	}
}

func TestOutboxDryRunEmitsNothing(t *testing.T) {
	l, _, dir := newOutboxLedger(t)
	res, err := l.DryRunPost(JournalEntry{ID: "dry1", DebitAccount: "a",
		CreditAccount: "b", AmountCents: 50})
	if err != nil || !res.WouldSucceed {
		t.Fatalf("DryRunPost = %+v err=%v", res, err)
	}
	if evs := readAllOutbox(t, dir); len(evs) != 0 {
		t.Fatalf("events after dry run = %d, want 0", len(evs))
	}
}

func TestOutboxConcurrentEmits(t *testing.T) {
	l, _, dir := newOutboxLedger(t)
	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e := JournalEntry{
				ID:            "conc-" + string(rune('a'+i/10)) + string(rune('0'+i%10)),
				DebitAccount:  "a",
				CreditAccount:  "b",
				AmountCents:   int64(i + 1),
			}
			if _, _, err := l.Post(e); err != nil {
				t.Errorf("Post %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	evs := readAllOutbox(t, dir)
	if len(evs) != n {
		t.Fatalf("events = %d, want %d", len(evs), n)
	}
	seen := make(map[uint64]bool, n)
	for _, ev := range evs {
		if seen[ev.Seq] {
			t.Fatalf("duplicate seq %d", ev.Seq)
		}
		seen[ev.Seq] = true
	}
	for s := uint64(1); s <= n; s++ {
		if !seen[s] {
			t.Fatalf("missing seq %d", s)
		}
	}
}
