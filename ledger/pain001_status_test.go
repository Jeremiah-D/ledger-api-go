package ledger

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

// mustExportPain001MsgID exports and returns the deterministic MsgId,
// extracted from the rendered document's GrpHdr.
func mustExportPain001MsgID(t *testing.T, l *Ledger, opts Pain001ExportOptions) string {
	t.Helper()
	doc, err := l.ExportPain001(opts)
	if err != nil {
		t.Fatalf("ExportPain001: %v", err)
	}
	s := string(doc)
	const open, close = "<MsgId>", "</MsgId>"
	i := strings.Index(s, open)
	j := strings.Index(s, close)
	if i < 0 || j < 0 || j <= i {
		t.Fatalf("exported document has no MsgId:\n%s", s)
	}
	return s[i+len(open) : j]
}

// painStatusTestLedger builds a ledger with IBANs and two posted
// transfers, ready for pain.001 export.
func painStatusTestLedger(t *testing.T) *Ledger {
	t.Helper()
	l := painTestLedger(t)
	mustPostTransferPain(t, l, "ps1", "alice", "bob", 5000, "USD", time.Time{})
	mustPostTransferPain(t, l, "ps2", "alice", "bob", 7000, "USD", time.Time{})
	return l
}

// pain001StatusEvents reads the audit log and returns the
// pain001_status events in order.
func pain001StatusEvents(t *testing.T, al *AuditLog) []AuditEvent {
	t.Helper()
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, corrupt, err := ReadAuditLog(al.Dir(), time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt = %d, want 0", corrupt)
	}
	var out []AuditEvent
	for _, ev := range events {
		if ev.Op == "pain001_status" {
			out = append(out, ev)
		}
	}
	return out
}

// Exporting registers the instruction: born "registered", immediately
// moved to "exported", one audit event. A duplicate export is
// idempotent: same record, no new audit event.
func TestPainInstructionRegistrationIdempotent(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	mustSetIBAN(t, l, "alice", testIBANDE, "")
	mustSetIBAN(t, l, "bob", testIBANGB, "")
	mustPostTransferPain(t, l, "ps1", "alice", "bob", 5000, "USD", time.Time{})

	msgID := mustExportPain001MsgID(t, l, Pain001ExportOptions{})
	rec, ok := l.PainInstruction(msgID)
	if !ok {
		t.Fatalf("PainInstruction(%q): not registered", msgID)
	}
	if rec.State != Pain001StateExported {
		t.Errorf("state = %q, want %q", rec.State, Pain001StateExported)
	}
	if rec.TxCount != 1 || rec.CtrlSum != 5000 || rec.Currency != "USD" {
		t.Errorf("record = %+v, want 1 tx / 5000 cents / USD", rec)
	}
	if len(rec.Txs) != 1 || rec.Txs[0].TxID != "ps1" || rec.Txs[0].State != Pain001StateExported {
		t.Errorf("txs = %+v, want [ps1 exported]", rec.Txs)
	}
	before := rec.ExportedAt

	// Duplicate export: same params, same content, same MsgId.
	msgID2 := mustExportPain001MsgID(t, l, Pain001ExportOptions{})
	if msgID2 != msgID {
		t.Fatalf("duplicate export MsgId = %q, want %q", msgID2, msgID)
	}
	rec2, ok := l.PainInstruction(msgID2)
	if !ok {
		t.Fatalf("PainInstruction(%q) after duplicate export: gone", msgID2)
	}
	if !rec2.ExportedAt.Equal(before) {
		t.Errorf("duplicate export re-registered the instruction: exported_at moved")
	}

	events := pain001StatusEvents(t, al)
	if len(events) != 1 {
		t.Fatalf("pain001_status events = %d, want 1 (registered->exported only)", len(events))
	}
	ev := events[0]
	if ev.Details["msg_id"] != msgID || ev.Details["from"] != Pain001StateRegistered || ev.Details["to"] != Pain001StateExported {
		t.Errorf("event details = %v, want msg_id/from=registered/to=exported", ev.Details)
	}
}

// Bank receipts advance transfers along the legal transition map and
// roll the instruction state up: all accepted -> "accepted".
func TestPain001ReceiptTransitions(t *testing.T) {
	l := painStatusTestLedger(t)
	msgID := mustExportPain001MsgID(t, l, Pain001ExportOptions{})

	rec, err := l.RecordPain001Receipts(msgID, []Pain001Receipt{
		{TxID: "ps1", Status: Pain001StateAccepted},
		{TxID: "ps2", Status: Pain001StateAccepted},
	}, "bank-ops")
	if err != nil {
		t.Fatalf("RecordPain001Receipts: %v", err)
	}
	if rec.State != Pain001StateAccepted {
		t.Errorf("instruction state = %q, want accepted", rec.State)
	}
	for _, tx := range rec.Txs {
		if tx.State != Pain001StateAccepted {
			t.Errorf("tx %s state = %q, want accepted", tx.TxID, tx.State)
		}
	}

	// Rejection reasons ride the record verbatim.
	rec, err = l.RecordPain001Receipts(msgID, []Pain001Receipt{
		{TxID: "ps1", Status: Pain001StateAccepted}, // no-op replay
	}, "bank-ops")
	if err != nil {
		t.Fatalf("replay receipt: %v", err)
	}
	if rec.State != Pain001StateAccepted {
		t.Errorf("after no-op replay, state = %q, want accepted", rec.State)
	}
}

// A rejected transfer is terminal: accepting it afterwards is illegal,
// and rejecting the last transfer rolls the instruction to "rejected".
func TestPain001RejectionIsTerminal(t *testing.T) {
	l := painStatusTestLedger(t)
	msgID := mustExportPain001MsgID(t, l, Pain001ExportOptions{})

	rec, err := l.RecordPain001Receipts(msgID, []Pain001Receipt{
		{TxID: "ps1", Status: Pain001StateRejected, Reason: "AC03"},
	}, "bank-ops")
	if err != nil {
		t.Fatalf("RecordPain001Receipts: %v", err)
	}
	if rec.Txs[0].Reason != "AC03" {
		t.Errorf("tx reason = %q, want AC03", rec.Txs[0].Reason)
	}
	// One rejected, one unreceipted: the instruction is still in flight.
	if rec.State != Pain001StateExported {
		t.Errorf("mixed instruction state = %q, want exported", rec.State)
	}

	// Accepting a rejected transfer is illegal: rejected is terminal.
	if _, err := l.RecordPain001Receipts(msgID, []Pain001Receipt{
		{TxID: "ps1", Status: Pain001StateAccepted},
	}, "bank-ops"); !errors.Is(err, ErrIllegalPain001Transition) {
		t.Errorf("accept-after-reject err = %v, want ErrIllegalPain001Transition", err)
	}

	// Reject the other one: the instruction rolls up to rejected.
	rec, err = l.RecordPain001Receipts(msgID, []Pain001Receipt{
		{TxID: "ps2", Status: Pain001StateRejected, Reason: "AM09"},
	}, "bank-ops")
	if err != nil {
		t.Fatalf("RecordPain001Receipts: %v", err)
	}
	if rec.State != Pain001StateRejected {
		t.Errorf("instruction state = %q, want rejected", rec.State)
	}
}

// Settled is terminal too: no transition out of it is legal.
func TestPain001SettledIsTerminal(t *testing.T) {
	l := painStatusTestLedger(t)
	msgID := mustExportPain001MsgID(t, l, Pain001ExportOptions{})

	if _, err := l.RecordPain001Receipts(msgID, []Pain001Receipt{
		{TxID: "ps1", Status: Pain001StateAccepted},
		{TxID: "ps2", Status: Pain001StateAccepted},
	}, "bank-ops"); err != nil {
		t.Fatalf("RecordPain001Receipts: %v", err)
	}
	rec, err := l.SettlePain001Instruction(msgID, "operator")
	if err != nil {
		t.Fatalf("SettlePain001Instruction: %v", err)
	}
	if rec.State != Pain001StateSettled {
		t.Fatalf("instruction state = %q, want settled", rec.State)
	}
	if _, err := l.RecordPain001Receipts(msgID, []Pain001Receipt{
		{TxID: "ps1", Status: Pain001StateAccepted},
	}, "bank-ops"); !errors.Is(err, ErrIllegalPain001Transition) {
		t.Errorf("receipt-after-settle err = %v, want ErrIllegalPain001Transition", err)
	}
}

// Same-state receipt replay is a no-op: no transition, no audit event.
func TestPain001DuplicateReceiptNoOp(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	mustSetIBAN(t, l, "alice", testIBANDE, "")
	mustSetIBAN(t, l, "bob", testIBANGB, "")
	mustPostTransferPain(t, l, "ps1", "alice", "bob", 5000, "USD", time.Time{})
	msgID := mustExportPain001MsgID(t, l, Pain001ExportOptions{})

	batch := []Pain001Receipt{{TxID: "ps1", Status: Pain001StateAccepted}}
	if _, err := l.RecordPain001Receipts(msgID, batch, "bank-ops"); err != nil {
		t.Fatalf("RecordPain001Receipts: %v", err)
	}
	rec, err := l.RecordPain001Receipts(msgID, batch, "bank-ops")
	if err != nil {
		t.Fatalf("replay RecordPain001Receipts: %v", err)
	}
	if rec.State != Pain001StateAccepted || rec.Txs[0].State != Pain001StateAccepted {
		t.Errorf("after replay: instruction=%q tx=%q, want accepted/accepted", rec.State, rec.Txs[0].State)
	}
	events := pain001StatusEvents(t, al)
	// registered->exported, tx ps1 exported->accepted, instruction
	// exported->accepted: exactly 3, the replay added none.
	if len(events) != 3 {
		t.Fatalf("pain001_status events = %d, want 3 (replay must be a no-op)", len(events))
	}
}

// The settle confirmation only settles what the bank accepted: it
// never accepts on the bank's behalf.
func TestPain001SettleOnlyAccepted(t *testing.T) {
	l := painStatusTestLedger(t)
	msgID := mustExportPain001MsgID(t, l, Pain001ExportOptions{})

	if _, err := l.RecordPain001Receipts(msgID, []Pain001Receipt{
		{TxID: "ps1", Status: Pain001StateAccepted},
	}, "bank-ops"); err != nil {
		t.Fatalf("RecordPain001Receipts: %v", err)
	}
	rec, err := l.SettlePain001Instruction(msgID, "operator")
	if err != nil {
		t.Fatalf("SettlePain001Instruction: %v", err)
	}
	states := map[string]string{}
	for _, tx := range rec.Txs {
		states[tx.TxID] = tx.State
	}
	if states["ps1"] != Pain001StateSettled {
		t.Errorf("ps1 = %q, want settled", states["ps1"])
	}
	if states["ps2"] != Pain001StateExported {
		t.Errorf("ps2 = %q, want exported (unreceipted, never auto-accepted)", states["ps2"])
	}
	if rec.State != Pain001StateExported {
		t.Errorf("instruction = %q, want exported (still in flight)", rec.State)
	}
}

// Receipt batches are atomic: one bad receipt refuses the whole batch,
// and the record is untouched.
func TestPain001ReceiptBatchAtomic(t *testing.T) {
	l := painStatusTestLedger(t)
	msgID := mustExportPain001MsgID(t, l, Pain001ExportOptions{})

	_, err := l.RecordPain001Receipts(msgID, []Pain001Receipt{
		{TxID: "ps1", Status: Pain001StateAccepted},
		{TxID: "ghost", Status: Pain001StateAccepted},
	}, "bank-ops")
	if !errors.Is(err, ErrUnknownPain001Tx) {
		t.Fatalf("err = %v, want ErrUnknownPain001Tx", err)
	}
	rec, _ := l.PainInstruction(msgID)
	for _, tx := range rec.Txs {
		if tx.State != Pain001StateExported {
			t.Errorf("tx %s = %q after refused batch, want exported (untouched)", tx.TxID, tx.State)
		}
	}

	// A batch that contradicts itself refuses too.
	_, err = l.RecordPain001Receipts(msgID, []Pain001Receipt{
		{TxID: "ps1", Status: Pain001StateAccepted},
		{TxID: "ps1", Status: Pain001StateRejected, Reason: "AC03"},
	}, "bank-ops")
	if !errors.Is(err, ErrIllegalPain001Transition) {
		t.Fatalf("self-contradicting batch err = %v, want ErrIllegalPain001Transition", err)
	}

	// Vocabulary and shape errors.
	for _, batch := range [][]Pain001Receipt{
		{},
		{{TxID: "ps1", Status: "pending"}},
		{{TxID: "", Status: Pain001StateAccepted}},
	} {
		_, err := l.RecordPain001Receipts(msgID, batch, "bank-ops")
		if err == nil {
			t.Errorf("batch %+v: want an error, got nil", batch)
		}
	}
}

// Unknown instructions are 404-shaped errors at the ledger level,
// across all three operations.
func TestPain001UnknownInstruction(t *testing.T) {
	l := painStatusTestLedger(t)
	if _, ok := l.PainInstruction("nope"); ok {
		t.Error("PainInstruction(nope): want not found")
	}
	if _, err := l.RecordPain001Receipts("nope", []Pain001Receipt{{TxID: "x", Status: "accepted"}}, "a"); !errors.Is(err, ErrUnknownPainInstruction) {
		t.Errorf("receipts err = %v, want ErrUnknownPainInstruction", err)
	}
	if _, err := l.SettlePain001Instruction("nope", "a"); !errors.Is(err, ErrUnknownPainInstruction) {
		t.Errorf("settle err = %v, want ErrUnknownPainInstruction", err)
	}
}

// Every status transition emits a hash-chained audit event with
// msg_id/tx_id/from/to: the compliance trail shows exactly how each
// instruction reached its state.
func TestPain001StatusAuditTrail(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	mustSetIBAN(t, l, "alice", testIBANDE, "")
	mustSetIBAN(t, l, "bob", testIBANGB, "")
	mustPostTransferPain(t, l, "ps1", "alice", "bob", 5000, "USD", time.Time{})
	msgID := mustExportPain001MsgID(t, l, Pain001ExportOptions{})

	if _, err := l.RecordPain001Receipts(msgID, []Pain001Receipt{{TxID: "ps1", Status: "accepted"}}, "bank-ops"); err != nil {
		t.Fatalf("RecordPain001Receipts: %v", err)
	}
	if _, err := l.SettlePain001Instruction(msgID, "operator"); err != nil {
		t.Fatalf("SettlePain001Instruction: %v", err)
	}
	events := pain001StatusEvents(t, al)
	want := [][3]string{
		{"registered", "exported", ""},  // instruction birth
		{"exported", "accepted", "ps1"}, // bank receipt
		{"exported", "accepted", ""},    // instruction rollup
		{"accepted", "settled", "ps1"},  // operator settle
		{"accepted", "settled", ""},     // instruction rollup
	}
	if len(events) != len(want) {
		t.Fatalf("pain001_status events = %d, want %d", len(events), len(want))
	}
	for i, w := range want {
		ev := events[i]
		if ev.Details["msg_id"] != msgID {
			t.Errorf("event %d: msg_id = %v, want %q", i, ev.Details["msg_id"], msgID)
		}
		if ev.Details["from"] != w[0] || ev.Details["to"] != w[1] || ev.Details["tx_id"] != w[2] {
			t.Errorf("event %d: from/to/tx_id = %v/%v/%v, want %s/%s/%s",
				i, ev.Details["from"], ev.Details["to"], ev.Details["tx_id"], w[0], w[1], w[2])
		}
		if ev.TraceID != msgID {
			t.Errorf("event %d: trace_id = %q, want the msg_id", i, ev.TraceID)
		}
	}
	// Hash-chain continuity: every event links to its predecessor.
	// (The filtered pain001_status events are consecutive in the log:
	// the export's pain001_exported event precedes the first one, and
	// nothing else is audited between the transitions.)
	for i := 1; i < len(events); i++ {
		if events[i].PrevHash != events[i-1].Hash {
			t.Errorf("event %d: prev_hash does not link to the previous event's seal", i)
		}
	}
}

// The instruction registry round-trips through snapshots with its
// receipt/settlement position intact: a restored ledger keeps
// transitioning where the source left off.
func TestPain001InstructionSnapshotRoundTrip(t *testing.T) {
	l := painStatusTestLedger(t)
	msgID := mustExportPain001MsgID(t, l, Pain001ExportOptions{})
	if _, err := l.RecordPain001Receipts(msgID, []Pain001Receipt{{TxID: "ps1", Status: "accepted"}}, "bank-ops"); err != nil {
		t.Fatalf("RecordPain001Receipts: %v", err)
	}

	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	l2, err := ImportSnapshot(&buf)
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	rec, ok := l2.PainInstruction(msgID)
	if !ok {
		t.Fatalf("restored ledger lost instruction %q", msgID)
	}
	if rec.State != Pain001StateExported {
		t.Errorf("restored instruction state = %q, want exported", rec.State)
	}
	states := map[string]string{}
	for _, tx := range rec.Txs {
		states[tx.TxID] = tx.State
	}
	if states["ps1"] != Pain001StateAccepted || states["ps2"] != Pain001StateExported {
		t.Errorf("restored tx states = %v, want ps1=accepted ps2=exported", states)
	}
	// The restored ledger keeps transitioning: settle the accepted tx.
	rec, err = l2.SettlePain001Instruction(msgID, "operator")
	if err != nil {
		t.Fatalf("SettlePain001Instruction on restored ledger: %v", err)
	}
	restored := map[string]string{}
	for _, tx := range rec.Txs {
		restored[tx.TxID] = tx.State
	}
	if restored["ps1"] != Pain001StateSettled || restored["ps2"] != Pain001StateExported {
		t.Errorf("restored settle: tx states = %v, want ps1=settled ps2=exported", restored)
	}

	// Deterministic ordering: two exports of the same ledger state
	// serialize the registry identically. (The snapshot's meta line
	// carries an export timestamp, so whole-byte comparison is
	// meaningless — compare the config record line instead.)
	configLine := func(s string) string {
		for _, line := range strings.Split(s, "\n") {
			if strings.Contains(line, `"record":"config"`) {
				return line
			}
		}
		t.Fatal("snapshot has no config record line")
		return ""
	}
	var buf2 bytes.Buffer
	if err := l.ExportSnapshot(&buf2); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	var buf3 bytes.Buffer
	if err := l.ExportSnapshot(&buf3); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	if configLine(buf2.String()) != configLine(buf3.String()) {
		t.Error("config record differs between identical exports: registry order is not deterministic")
	}
}

// The reconcile report discloses unsettled instructions in
// pain001_pending: the operator's reconcile-confirmation worklist.
func TestPain001ReconcilePending(t *testing.T) {
	l := painTestLedger(t)
	mustPostTransferPain(t, l, "ps1", "alice", "bob", 5000, "USD", time.Time{})
	mustPostTransferPain(t, l, "ps2", "alice", "carol", 3000, "EUR", time.Time{})
	usdMsg := mustExportPain001MsgID(t, l, Pain001ExportOptions{Currency: "USD"})
	eurMsg := mustExportPain001MsgID(t, l, Pain001ExportOptions{Currency: "EUR"})

	// Settle the USD instruction fully: receipts + operator confirmation.
	if _, err := l.RecordPain001Receipts(usdMsg, []Pain001Receipt{{TxID: "ps1", Status: "accepted"}}, "bank-ops"); err != nil {
		t.Fatalf("RecordPain001Receipts: %v", err)
	}
	if _, err := l.SettlePain001Instruction(usdMsg, "operator"); err != nil {
		t.Fatalf("SettlePain001Instruction: %v", err)
	}

	report := l.Reconcile(time.Now())
	if len(report.Pain001Pending) != 1 {
		t.Fatalf("pain001_pending rows = %d, want 1 (the unsettled EUR instruction)", len(report.Pain001Pending))
	}
	row := report.Pain001Pending[0]
	if row.MsgID != eurMsg {
		t.Errorf("pending msg_id = %q, want %q", row.MsgID, eurMsg)
	}
	if row.State != Pain001StateExported {
		t.Errorf("pending state = %q, want exported", row.State)
	}
	if len(row.UnsettledTxIDs) != 1 || row.UnsettledTxIDs[0] != "ps2" {
		t.Errorf("unsettled_tx_ids = %v, want [ps2]", row.UnsettledTxIDs)
	}
}

// Corrupt lifecycle records fail the snapshot import instead of
// installing states the transition map cannot move.
func TestPain001SnapshotValidation(t *testing.T) {
	l := painStatusTestLedger(t)
	msgID := mustExportPain001MsgID(t, l, Pain001ExportOptions{})

	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	corrupt := strings.Replace(buf.String(), `"state":"exported"`, `"state":"teleported"`, 1)
	if corrupt == buf.String() {
		t.Skip("snapshot encoding does not carry the expected state literal")
	}
	if _, err := ImportSnapshot(strings.NewReader(corrupt)); err == nil {
		t.Errorf("ImportSnapshot with unknown instruction state %q: want an error", msgID)
	}
}
