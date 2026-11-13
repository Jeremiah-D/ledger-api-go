package ledger

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// Payment-instruction state machine for the pain.008 direct-debit
// lifecycle (LG-406). See Pain008Instruction.
//
// ExportPain008 (see pain008.go, LG-166) renders the instruction the
// ledger sends to the bank; the bank answers with a pain.002.001.03
// status report naming that instruction's MsgId (OrgnlMsgId) and, per
// transaction, the EndToEndId the ledger exported (the journal entry
// ID) plus a status:
//
//	ACSC  AcceptedSettlementCompleted — the money moved.
//	ACCP  AcceptedCustomerProfile     — accepted, settlement pending.
//	RJCT  Rejected                    — refused, with a reason code.
//	PDNG  Pending                     — the bank has not decided yet.
//
// Status mapping into the pain.008 state machine: ACSC/ACCP become
// "accepted", RJCT becomes "rejected" (the bank's StsRsnInf reason
// code and text ride the receipt's Reason into the hash-chained
// pain008_status audit event, verbatim — the bank's vocabulary, not
// the ledger's). PDNG generates no receipt: the entry stays
// "exported" until the bank reports a verdict, so a pending report
// never invents a state the bank did not report. A report whose every
// transaction is PDNG is an explicit no-op: the instruction is
// returned unchanged, no audit event.
//
// The application mirrors ApplyPain002Report (see pain002.go): the
// batch is validated before anything is applied, receipt replay is
// idempotent, and every applied transition emits its pain008_status
// audit event plus the domain outbox event.
//
// Deliberately out of scope: driving PayoutInstruction (see payout.go)
// or the mandate itself. A pain.002 names journal entry IDs
// (EndToEndId), not payout IDs, and a bank verdict is evidence only —
// SettlePayout/FailPayout still demand a bank statement line as
// evidence, and a rejected collection never cancels or suspends the
// mandate: the operator decides that through CancelMandate. The
// verdict never moves funds, never amends a journal entry, and never
// advances any payout state machine.

// pain.008 instruction lifecycle errors.
var (
	// ErrUnknownPain008Instruction is returned when a status report
	// names a msg_id the ledger never exported: verdicts are keyed by
	// the deterministic export MsgId, so a name that was never
	// generated is a client error, not a silent drop. Mirrors
	// ErrUnknownPainInstruction.
	ErrUnknownPain008Instruction = errors.New("ledger: unknown pain.008 direct debit instruction")
	// ErrUnknownPain008Tx is returned when a bank report names an
	// entry ID that is not part of the instruction's exported
	// collection set: the verdict cannot be attributed, so the whole
	// batch is refused rather than partially applied. Mirrors
	// ErrUnknownPain001Tx.
	ErrUnknownPain008Tx = errors.New("ledger: pain.008 report names an entry outside the instruction")
	// ErrIllegalPain008Transition is returned when a receipt asks for
	// a state change the transition map does not allow (see
	// pain008TxTransitions). The record is left untouched. Mirrors
	// ErrIllegalPain001Transition.
	ErrIllegalPain008Transition = errors.New("ledger: illegal pain.008 status transition")
	// ErrInvalidPain008ReceiptStatus is returned when a receipt
	// carries a bank status outside the pain.002-style vocabulary
	// (accepted/rejected): unknown statuses are refused, not coerced.
	ErrInvalidPain008ReceiptStatus = errors.New("ledger: invalid pain.008 receipt status: want accepted or rejected")
	// ErrEmptyPain008Receipts is returned when a receipts batch carries
	// no receipts: an empty batch records nothing, so it is a client
	// error rather than a no-op success.
	ErrEmptyPain008Receipts = errors.New("ledger: pain.008 receipts batch is empty")
)

// Payment-instruction lifecycle states for pain.008 (see
// Pain008Instruction). The instruction-level state is derived from
// the per-entry states, but every derived move is checked against
// pain008InstructionTransitions, so the stored state can never jump
// outside the lifecycle. There is no settled state: the bank's
// verdict is evidence only, and settlement confirmation stays an
// operator attestation on the payout/mandate side, never a side
// effect of a status report.
const (
	// Pain008StateRegistered is the record's birth state: the
	// deterministic MsgId was computed for an export. It never lasts —
	// ExportPain008 moves the record straight to exported — but it
	// exists so the lifecycle has a named start, like the pain.001
	// record.
	Pain008StateRegistered = "registered"
	// Pain008StateExported: the instruction document was generated and
	// handed to the banking channel; every entry is unreceipted.
	Pain008StateExported = "exported"
	// Pain008StateAccepted: the bank receipted every entry as
	// accepted. Entries receipted as rejected move to rejected, not
	// accepted: a partially accepted instruction stays exported so the
	// pending disclosure keeps naming the unreceipted remainder.
	Pain008StateAccepted = "accepted"
	// Pain008StateRejected: the bank rejected every entry. Terminal:
	// a rejected instruction is never re-exported under the same
	// MsgId (the MsgId is deterministic in the export content, and the
	// content is unchanged), so the operator fixes the cause and
	// re-exports, which yields a new MsgId.
	Pain008StateRejected = "rejected"
)

// pain008TxTransitions is the legal per-entry transition map. The
// bank's pain.002-style receipts drive exported -> accepted/rejected.
// Same-state receipt replay is a no-op (idempotent), not a transition,
// and rejected is terminal: once the bank refused, the entry's content
// is history, and any new instruction is a new export with a new
// MsgId.
var pain008TxTransitions = map[string][]string{
	Pain008StateExported: {Pain008StateAccepted, Pain008StateRejected},
}

// pain008InstructionTransitions is the legal instruction-level
// transition map, checked after every per-entry move. Because receipts
// only advance entries along pain008TxTransitions, the derived
// instruction state can only move along these edges; the check is a
// defensive invariant, not a second state machine.
var pain008InstructionTransitions = map[string][]string{
	Pain008StateRegistered: {Pain008StateExported},
	Pain008StateExported:   {Pain008StateAccepted, Pain008StateRejected},
}

// pain008ValidState reports whether s is a known lifecycle state: used
// to validate snapshot records, where a corrupt file must not install
// a state the machine cannot transition out of.
func pain008ValidState(s string) bool {
	switch s {
	case Pain008StateRegistered, Pain008StateExported, Pain008StateAccepted, Pain008StateRejected:
		return true
	}
	return false
}

// Pain008TxState is one exported collection's lifecycle state inside a
// pain.008 instruction: the journal entry ID (the pain.008
// EndToEndId), the amount and currency it was exported with, the
// current state, the bank's optional rejection reason, and the last
// transition time.
type Pain008TxState struct {
	EntryID     string    `json:"entry_id"`
	State       string    `json:"state"`
	AmountCents int64     `json:"amount_cents"`
	Currency    string    `json:"currency"`
	Reason      string    `json:"reason,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Pain008Instruction is the lifecycle record of one pain.008 direct
// debit instruction: the deterministic export MsgId (see pain008MsgID
// — the same export parameters re-render the same MsgId), the mandate
// it collects under, the current instruction-level state, the export
// timestamp, and the per-entry states in EntryID order (the snapshot
// record form).
//
// Semantics, made explicit:
//   - Registration happens inside ExportPain008: the record is born in
//     "registered" and immediately moves to "exported", one
//     pain008_status audit event (from=registered, to=exported). A
//     duplicate export (same params, same content, same MsgId) is
//     idempotent: it returns the existing record and records nothing.
//   - Bank verdicts (see ApplyPain002ToPain008) move entries along
//     pain008TxTransitions. Replaying the same verdict for an entry
//     already in that state is a no-op: no transition, no audit event.
//   - Every status transition — per-entry and instruction-level —
//     emits a hash-chained audit event with op "pain008_status" and
//     details msg_id/entry_id/from/to/verdict (reason_code and reason
//     when the bank supplied them), so the compliance trail shows
//     exactly how each instruction reached its state.
type Pain008Instruction struct {
	MsgID      string           `json:"msg_id"`
	MandateID  string           `json:"mandate_id"`
	State      string           `json:"state"`
	ExportedAt time.Time        `json:"exported_at"`
	TxCount    int              `json:"tx_count"`
	CtrlSum    int64            `json:"ctrl_sum_cents"`
	Currency   string           `json:"currency"`
	Txs        []Pain008TxState `json:"txs"`
}

// Pain008Receipt is one bank verdict in pain.002 style: the entry ID
// inside the instruction's MsgId, the bank's status for it ("accepted"
// or "rejected"), and the bank's reason split into code and free text
// (only set for RJCT, kept verbatim — it is the bank's vocabulary,
// not the ledger's).
type Pain008Receipt struct {
	EntryID    string `json:"entry_id"`
	Status     string `json:"status"`
	ReasonCode string `json:"reason_code,omitempty"`
	ReasonInfo string `json:"reason_info,omitempty"`
}

// Reason renders a receipt's reason the way the pain008_status audit
// event records it: "CODE: free text", or whichever half the bank
// supplied. Mirrors pain002ReceiptReason.
func (r Pain008Receipt) Reason() string {
	switch {
	case r.ReasonCode != "" && r.ReasonInfo != "":
		return r.ReasonCode + ": " + r.ReasonInfo
	case r.ReasonCode != "":
		return r.ReasonCode
	default:
		return r.ReasonInfo
	}
}

// pain008InstructionLocked returns the lifecycle record for msgID.
// Callers must hold l.mu (either lock suffices). The returned pointer
// is the live record: write paths mutate it under the write lock; read
// paths copy it out with pain008InstructionSnapshotLocked.
func (l *Ledger) pain008InstructionLocked(msgID string) (*Pain008Instruction, bool) {
	rec, ok := l.pain008Instructions[msgID]
	return rec, ok
}

// Pain008Instruction returns a copy of the lifecycle record for msgID,
// or false when the ledger never exported that instruction.
func (l *Ledger) Pain008Instruction(msgID string) (Pain008Instruction, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	rec, ok := l.pain008Instructions[msgID]
	if !ok {
		return Pain008Instruction{}, false
	}
	return pain008InstructionSnapshotLocked(rec), true
}

// pain008InstructionSnapshotLocked deep-copies a record. Callers must
// hold l.mu (either lock suffices).
func pain008InstructionSnapshotLocked(rec *Pain008Instruction) Pain008Instruction {
	out := *rec
	out.Txs = make([]Pain008TxState, len(rec.Txs))
	copy(out.Txs, rec.Txs)
	return out
}

// pain008InstructionsLocked returns every lifecycle record sorted by
// MsgId, the snapshot record form. Callers must hold l.mu (either lock
// suffices).
func (l *Ledger) pain008InstructionsLocked() []Pain008Instruction {
	out := make([]Pain008Instruction, 0, len(l.pain008Instructions))
	for _, rec := range l.pain008Instructions {
		out = append(out, pain008InstructionSnapshotLocked(rec))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MsgID < out[j].MsgID })
	return out
}

// registerPain008InstructionLocked creates the lifecycle record for a
// freshly rendered export and moves it registered -> exported. It is
// idempotent in MsgId: a re-export of the same parameters and content
// re-derives the same MsgId and returns the existing record with no
// new audit event — the instruction was already registered, and
// registering it again would invent a second birth the bank never saw.
// Callers must hold the write lock.
func (l *Ledger) registerPain008InstructionLocked(msgID, mandateID string, collections []pain008Collection, txCount int, ctrlSum int64, currency string) *Pain008Instruction {
	if rec, ok := l.pain008Instructions[msgID]; ok {
		return rec
	}
	rec := &Pain008Instruction{
		MsgID:      msgID,
		MandateID:  mandateID,
		State:      Pain008StateRegistered,
		ExportedAt: time.Now().UTC(),
		TxCount:    txCount,
		CtrlSum:    ctrlSum,
		Currency:   currency,
		Txs:        make([]Pain008TxState, 0, len(collections)),
	}
	for _, c := range collections {
		rec.Txs = append(rec.Txs, Pain008TxState{
			EntryID:     c.Entry.ID,
			State:       Pain008StateExported,
			AmountCents: c.Entry.AmountCents,
			Currency:    c.Entry.Currency,
			UpdatedAt:   rec.ExportedAt,
		})
	}
	// Deterministic entry order: the snapshot record form sorts by
	// EntryID, so identical exports always serialize identically.
	sort.Slice(rec.Txs, func(i, j int) bool { return rec.Txs[i].EntryID < rec.Txs[j].EntryID })
	l.pain008Instructions[msgID] = rec
	l.transitionPain008InstructionLocked(rec, Pain008StateExported, "ExportPain008")
	return rec
}

// transitionPain008InstructionLocked moves a record to a new state after
// checking the instruction-level transition map, and emits the
// hash-chained "pain008_status" audit event for the move. Callers must
// hold the write lock. The audit event is emitted under the write
// lock; the enqueue never blocks (see emitAudit).
func (l *Ledger) transitionPain008InstructionLocked(rec *Pain008Instruction, to, actor string) error {
	from := rec.State
	if from == to {
		return nil
	}
	legal := false
	for _, next := range pain008InstructionTransitions[from] {
		if next == to {
			legal = true
			break
		}
	}
	if !legal {
		return fmt.Errorf("%w: instruction %q %s -> %s", ErrIllegalPain008Transition, rec.MsgID, from, to)
	}
	rec.State = to
	l.emitAudit(AuditEvent{
		Op:      "pain008_status",
		Actor:   actor,
		TraceID: rec.MsgID,
		// The version brackets are identical: tracking an
		// instruction's lifecycle books nothing, like Reconcile, but
		// the audit event records the state against which ledger
		// version it was observed.
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"msg_id":   rec.MsgID,
			"entry_id": "",
			"from":     from,
			"to":       to,
		},
	})
	// Domain outbox (LG-102): the instruction's status move is a
	// domain fact — webhook subscribers watch it to track bank-side
	// collection progress without polling.
	l.emitOutboxPain008StatusLocked(rec.MsgID, "", from, to, "", "", actor)
	return nil
}

// rollupPain008InstructionLocked recomputes the instruction-level state
// from the per-entry states after a batch of transitions, and applies
// the derived move (if any) through the transition map. Callers must
// hold the write lock.
func (l *Ledger) rollupPain008InstructionLocked(rec *Pain008Instruction, actor string) error {
	to := Pain008StateExported
	if len(rec.Txs) > 0 {
		accepted, rejected := 0, 0
		for _, tx := range rec.Txs {
			switch tx.State {
			case Pain008StateAccepted:
				accepted++
			case Pain008StateRejected:
				rejected++
			}
		}
		switch {
		case accepted == len(rec.Txs):
			to = Pain008StateAccepted
		case rejected == len(rec.Txs):
			to = Pain008StateRejected
		default:
			// A mix of unreceipted and receipted entries (or a
			// rejected entry alongside accepted ones): the
			// instruction is still in flight, so it stays exported
			// and the pending disclosure keeps naming it.
			to = Pain008StateExported
		}
	}
	return l.transitionPain008InstructionLocked(rec, to, actor)
}

// pain008TxByIDLocked finds an entry's state record inside an
// instruction. Callers must hold the write lock (callers mutate the
// returned pointer's fields); the lookup itself needs no lock.
func pain008TxByIDLocked(rec *Pain008Instruction, entryID string) *Pain008TxState {
	for i := range rec.Txs {
		if rec.Txs[i].EntryID == entryID {
			return &rec.Txs[i]
		}
	}
	return nil
}

// RecordPain008Receipts applies a batch of bank verdicts (pain.002
// style) to an instruction's entries. Every receipt is validated
// first — status vocabulary, entry membership, transition legality —
// and only then applied: a batch with one bad receipt applies nothing,
// so the record can never half-absorb a bank file. Receipt replay is
// idempotent: a receipt naming a state the entry already holds is a
// no-op (no transition, no audit event), so re-delivered bank files
// are safe. Each applied transition emits a "pain008_status" audit
// event; when the batch moves the instruction-level state, that move
// gets its own event too.
func (l *Ledger) RecordPain008Receipts(msgID string, receipts []Pain008Receipt, actor string) (Pain008Instruction, error) {
	if len(receipts) == 0 {
		return Pain008Instruction{}, ErrEmptyPain008Receipts
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	rec, ok := l.pain008Instructions[msgID]
	if !ok {
		return Pain008Instruction{}, fmt.Errorf("%w: %q", ErrUnknownPain008Instruction, msgID)
	}
	// Validate the whole batch before touching the record: a batch is
	// one bank file, and applying half of it would invent a state the
	// bank never reported.
	type plannedMove struct {
		tx      *Pain008TxState
		to      string
		receipt Pain008Receipt
	}
	moves := make([]plannedMove, 0, len(receipts))
	seen := make(map[string]bool, len(receipts))
	for i, r := range receipts {
		if r.Status != Pain008StateAccepted && r.Status != Pain008StateRejected {
			return Pain008Instruction{}, fmt.Errorf("%w: receipt %d for %q", ErrInvalidPain008ReceiptStatus, i, r.EntryID)
		}
		if r.EntryID == "" {
			return Pain008Instruction{}, fmt.Errorf("%w: receipt %d names an empty entry", ErrUnknownPain008Tx, i)
		}
		tx := pain008TxByIDLocked(rec, r.EntryID)
		if tx == nil {
			return Pain008Instruction{}, fmt.Errorf("%w: %q not in instruction %q", ErrUnknownPain008Tx, r.EntryID, msgID)
		}
		if seen[r.EntryID] {
			// The same entry receipted twice in one batch: the
			// batch is the bank's single statement of fact, so a
			// duplicate that agrees is harmless, but one that
			// contradicts itself refuses the batch — the bank file
			// contradicts itself, and applying either reading would
			// invent a state the bank did not report.
			for _, m := range moves {
				if m.tx == tx && (m.to != r.Status || m.receipt.Reason() != r.Reason()) {
					return Pain008Instruction{}, fmt.Errorf("%w: receipt %d contradicts an earlier receipt for %q", ErrIllegalPain008Transition, i, r.EntryID)
				}
			}
			continue
		}
		seen[r.EntryID] = true
		if tx.State == r.Status {
			// Same-state replay: idempotent no-op, like a
			// duplicate export or a duplicate idempotency key.
			continue
		}
		legal := false
		for _, next := range pain008TxTransitions[tx.State] {
			if next == r.Status {
				legal = true
				break
			}
		}
		if !legal {
			return Pain008Instruction{}, fmt.Errorf("%w: entry %q %s -> %s", ErrIllegalPain008Transition, r.EntryID, tx.State, r.Status)
		}
		moves = append(moves, plannedMove{tx: tx, to: r.Status, receipt: r})
	}
	// Apply the validated batch: per-entry transitions with their
	// audit events, then the instruction-level rollup.
	for _, m := range moves {
		from := m.tx.State
		m.tx.State = m.to
		m.tx.Reason = m.receipt.Reason()
		m.tx.UpdatedAt = time.Now().UTC()
		l.emitAudit(AuditEvent{
			Op:            "pain008_status",
			Actor:         actor,
			TraceID:       rec.MsgID,
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Details: map[string]any{
				"msg_id":      rec.MsgID,
				"entry_id":    m.tx.EntryID,
				"from":        from,
				"to":          m.to,
				"verdict":     m.to,
				"reason_code": m.receipt.ReasonCode,
				"reason":      m.tx.Reason,
			},
		})
		// Domain outbox (LG-102): the bank's accepted/rejected
		// verdict on one entry is the per-entry status move webhook
		// subscribers watch.
		l.emitOutboxPain008StatusLocked(rec.MsgID, m.tx.EntryID, from, m.to, m.receipt.ReasonCode, m.tx.Reason, actor)
	}
	if err := l.rollupPain008InstructionLocked(rec, actor); err != nil {
		return Pain008Instruction{}, err
	}
	return pain008InstructionSnapshotLocked(rec), nil
}

// ApplyPain002ToPain008 applies a parsed pain.002 report to the
// pain.008 instruction it answers, advancing each decided entry along
// the pain.008 state machine: ACSC/ACCP become "accepted", RJCT becomes
// "rejected" with the bank's reason code in the audit event, PDNG is
// skipped (the entry stays "exported" — the bank has not decided).
//
// The application reuses RecordPain008Receipts, so the batch is
// validated before anything is applied, receipt replay is idempotent,
// and every transition emits its hash-chained pain008_status audit
// event plus the domain outbox event. A report whose every transaction
// is PDNG is an explicit no-op: the instruction is returned unchanged
// and nothing is recorded — a pending report is information, not a
// verdict.
//
// The verdict never moves funds, never amends a journal entry, and
// never advances any payout state machine: it is the bank's evidence
// about a collection, and settlement confirmation stays an operator
// attestation (see the file header).
func (l *Ledger) ApplyPain002ToPain008(report *Pain002Report, actor string) (Pain008Instruction, error) {
	if report == nil {
		return Pain008Instruction{}, fmt.Errorf("%w: nil report", ErrInvalidPain002)
	}
	if _, ok := l.Pain008Instruction(report.OrgnlMsgID); !ok {
		return Pain008Instruction{}, fmt.Errorf("%w: %q", ErrUnknownPain008Instruction, report.OrgnlMsgID)
	}
	receipts := make([]Pain008Receipt, 0, len(report.Txs))
	for _, tx := range report.Txs {
		var status string
		switch tx.TxSts {
		case Pain002TxACSC, Pain002TxACCP:
			// AcceptedSettlementCompleted and
			// AcceptedCustomerProfile both mean the bank took the
			// collection: the ledger's vocabulary has one accepted
			// state, and settlement completion is confirmed by the
			// operator, not inferred from the bank's wording.
			status = Pain008StateAccepted
		case Pain002TxRJCT:
			status = Pain008StateRejected
		default:
			// PDNG: the bank has not decided. No receipt, no
			// transition — the entry stays exported until a
			// verdict arrives.
			continue
		}
		receipts = append(receipts, Pain008Receipt{
			EntryID:    tx.EndToEndID,
			Status:     status,
			ReasonCode: tx.ReasonCode,
			ReasonInfo: tx.ReasonInfo,
		})
	}
	if len(receipts) == 0 {
		// Every transaction is PDNG (or the report names none): a
		// pending report records nothing.
		rec, _ := l.Pain008Instruction(report.OrgnlMsgID)
		return rec, nil
	}
	return l.RecordPain008Receipts(report.OrgnlMsgID, receipts, actor)
}
