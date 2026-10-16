package ledger

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// Payment-instruction state-machine errors for the pain.001 lifecycle
// tracker (LG-99). See PainInstruction.
var (
	// ErrUnknownPainInstruction is returned when a status, receipt, or
	// settle call names a msg_id the ledger never exported: receipts
	// and transitions are keyed by the deterministic export MsgId, so
	// a name that was never generated is a client error, not a silent
	// drop.
	ErrUnknownPainInstruction = errors.New("ledger: unknown pain.001 payment instruction")
	// ErrUnknownPain001Tx is returned when a bank receipt names a
	// transfer ID that is not part of the instruction's exported
	// payment set: the receipt cannot be attributed, so the whole
	// batch is refused rather than partially applied.
	ErrUnknownPain001Tx = errors.New("ledger: pain.001 receipt names a transfer outside the instruction")
	// ErrIllegalPain001Transition is returned when a receipt or settle
	// confirmation asks for a state change the transition map does not
	// allow (see pain001TxTransitions). The record is left untouched.
	ErrIllegalPain001Transition = errors.New("ledger: illegal pain.001 status transition")
	// ErrInvalidPain001ReceiptStatus is returned when a receipt carries
	// a bank status outside the pain.002-style vocabulary
	// (accepted/rejected): unknown statuses are refused, not coerced.
	ErrInvalidPain001ReceiptStatus = errors.New("ledger: invalid pain.001 receipt status: want accepted or rejected")
	// ErrEmptyPain001Receipts is returned when a receipts batch carries
	// no receipts: an empty batch records nothing, so it is a client
	// error rather than a no-op success.
	ErrEmptyPain001Receipts = errors.New("ledger: pain.001 receipts batch is empty")
)

// Payment-instruction lifecycle states (see PainInstruction). The
// instruction-level state is derived from the per-transfer states, but
// every derived move is checked against pain001InstructionTransitions,
// so the stored state can never jump outside the lifecycle.
const (
	// Pain001StateRegistered is the record's birth state: the
	// deterministic MsgId was computed for an export. It never lasts —
	// ExportPain001 moves the record straight to exported — but it
	// exists so the lifecycle has a named start, like idempotency
	// keys have a named "unknown" state.
	Pain001StateRegistered = "registered"
	// Pain001StateExported: the instruction document was generated and
	// handed to the banking channel; every transfer is unreceipted.
	Pain001StateExported = "exported"
	// Pain001StateAccepted: the bank receipted every transfer as
	// accepted. Transfers receipted as rejected move to rejected, not
	// accepted: a partially accepted instruction stays exported so the
	// pending disclosure keeps naming the unreceipted remainder.
	Pain001StateAccepted = "accepted"
	// Pain001StateRejected: the bank rejected every transfer. Terminal:
	// a rejected instruction is never re-exported under the same
	// MsgId (the MsgId is deterministic in the export content, and the
	// content is unchanged), so the operator fixes the cause and
	// re-exports, which yields a new MsgId.
	Pain001StateRejected = "rejected"
	// Pain001StateSettled: the operator confirmed the settlement run
	// (see SettlePain001Instruction) for every transfer. Terminal: the
	// money has moved, so no further transition is legal.
	Pain001StateSettled = "settled"
)

// pain001TxTransitions is the legal per-transfer transition map. The
// bank's pain.002-style receipts drive exported -> accepted/rejected;
// the operator's reconcile confirmation drives accepted -> settled.
// Same-state receipt replay is a no-op (idempotent), not a transition,
// and rejected/settled are terminal: once the bank refused or the
// money settled, the instruction's content is history, and any new
// payment instruction is a new export with a new MsgId.
var pain001TxTransitions = map[string][]string{
	Pain001StateExported: {Pain001StateAccepted, Pain001StateRejected},
	Pain001StateAccepted: {Pain001StateSettled},
}

// pain001InstructionTransitions is the legal instruction-level
// transition map, checked after every per-transfer move. Because
// receipts only advance transfers along pain001TxTransitions, the
// derived instruction state can only move along these edges; the check
// is a defensive invariant, not a second state machine.
var pain001InstructionTransitions = map[string][]string{
	Pain001StateRegistered: {Pain001StateExported},
	Pain001StateExported:   {Pain001StateAccepted, Pain001StateRejected},
	Pain001StateAccepted:   {Pain001StateSettled},
}

// pain001ValidState reports whether s is a known lifecycle state: used
// to validate snapshot records, where a corrupt file must not install
// a state the machine cannot transition out of.
func pain001ValidState(s string) bool {
	switch s {
	case Pain001StateRegistered, Pain001StateExported, Pain001StateAccepted, Pain001StateRejected, Pain001StateSettled:
		return true
	}
	return false
}

// Pain001TxState is one exported transfer's lifecycle state inside a
// payment instruction: the transfer ID (the pain.001 EndToEndId), the
// amount and currency it was exported with, the current state, the
// bank's optional rejection reason, and the last transition time.
type Pain001TxState struct {
	TxID        string    `json:"tx_id"`
	State       string    `json:"state"`
	AmountCents int64     `json:"amount_cents"`
	Currency    string    `json:"currency"`
	Reason      string    `json:"reason,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// PainInstruction is the lifecycle record of one pain.001 payment
// instruction: the deterministic export MsgId (see pain001MsgID — the
// same export parameters re-render the same MsgId), the current
// instruction-level state, the export timestamp, and the per-transfer
// states in TxID order (the snapshot record form).
//
// Semantics, made explicit:
//   - Registration happens inside ExportPain001: the record is born in
//     "registered" and immediately moves to "exported", one
//     pain001_status audit event (from=registered, to=exported). A
//     duplicate export (same params, same content, same MsgId) is
//     idempotent: it returns the existing record and records nothing.
//   - Bank receipts (see RecordPain001Receipts) move transfers along
//     pain001TxTransitions. Replaying the same receipt for a transfer
//     already in that state is a no-op: no transition, no audit event.
//   - The operator's settle confirmation (see SettlePain001Instruction)
//     moves accepted transfers to settled — the reconcile-confirmation
//     half of the lifecycle. It never accepts on the bank's behalf:
//     an unreceipted (exported) transfer stays exported, and a rejected
//     one stays rejected. This is deliberate: settling without a bank
//     receipt would book money the bank may never have moved.
//   - Every status transition — per-transfer and instruction-level —
//     emits a hash-chained audit event with op "pain001_status" and
//     details msg_id/tx_id/from/to (reason when the bank supplied
//     one), so the compliance trail shows exactly how each
//     instruction reached its state.
type PainInstruction struct {
	MsgID      string           `json:"msg_id"`
	State      string           `json:"state"`
	ExportedAt time.Time        `json:"exported_at"`
	TxCount    int              `json:"tx_count"`
	CtrlSum    int64            `json:"ctrl_sum_cents"`
	Currency   string           `json:"currency"`
	Txs        []Pain001TxState `json:"txs"`
}

// Pain001Receipt is one bank receipt in pain.002 style: the transfer
// ID inside the instruction's MsgId, the bank's status for it
// ("accepted" or "rejected"), and the bank's reason code or text for a
// rejection (kept verbatim, never interpreted — it is the bank's
// vocabulary, not the ledger's).
type Pain001Receipt struct {
	TxID   string `json:"tx_id"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// painInstructionLocked returns the lifecycle record for msgID. Callers
// must hold l.mu (either lock suffices). The returned pointer is the
// live record: write paths mutate it under the write lock; read paths
// copy it out with painInstructionSnapshotLocked.
func (l *Ledger) painInstructionLocked(msgID string) (*PainInstruction, bool) {
	rec, ok := l.painInstructions[msgID]
	return rec, ok
}

// PainInstruction returns a copy of the lifecycle record for msgID, or
// false when the ledger never exported that instruction.
func (l *Ledger) PainInstruction(msgID string) (PainInstruction, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	rec, ok := l.painInstructions[msgID]
	if !ok {
		return PainInstruction{}, false
	}
	return painInstructionSnapshotLocked(rec), true
}

// painInstructionSnapshotLocked deep-copies a record. Callers must hold
// l.mu (either lock suffices).
func painInstructionSnapshotLocked(rec *PainInstruction) PainInstruction {
	out := *rec
	out.Txs = make([]Pain001TxState, len(rec.Txs))
	copy(out.Txs, rec.Txs)
	return out
}

// painInstructionsLocked returns every lifecycle record sorted by
// MsgId, the snapshot record form. Callers must hold l.mu (either lock
// suffices).
func (l *Ledger) painInstructionsLocked() []PainInstruction {
	out := make([]PainInstruction, 0, len(l.painInstructions))
	for _, rec := range l.painInstructions {
		out = append(out, painInstructionSnapshotLocked(rec))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MsgID < out[j].MsgID })
	return out
}

// registerPainInstructionLocked creates the lifecycle record for a
// freshly rendered export and moves it registered -> exported. It is
// idempotent in MsgId: a re-export of the same parameters and content
// re-derives the same MsgId and returns the existing record with no
// new audit event — the instruction was already registered, and
// registering it again would invent a second birth the bank never saw.
// Callers must hold the write lock.
func (l *Ledger) registerPainInstructionLocked(msgID string, payments []pain001Payment, txCount int, ctrlSum int64, currency string) *PainInstruction {
	if rec, ok := l.painInstructions[msgID]; ok {
		return rec
	}
	rec := &PainInstruction{
		MsgID:      msgID,
		State:      Pain001StateRegistered,
		ExportedAt: time.Now().UTC(),
		TxCount:    txCount,
		CtrlSum:    ctrlSum,
		Currency:   currency,
		Txs:        make([]Pain001TxState, 0, len(payments)),
	}
	for _, p := range payments {
		rec.Txs = append(rec.Txs, Pain001TxState{
			TxID:        p.TransferID,
			State:       Pain001StateExported,
			AmountCents: p.AmountCents,
			Currency:    p.Currency,
			UpdatedAt:   rec.ExportedAt,
		})
	}
	// Deterministic tx order: the snapshot record form sorts by TxID,
	// so identical exports always serialize identically.
	sort.Slice(rec.Txs, func(i, j int) bool { return rec.Txs[i].TxID < rec.Txs[j].TxID })
	l.painInstructions[msgID] = rec
	l.transitionPainInstructionLocked(rec, Pain001StateExported, "ExportPain001")
	return rec
}

// transitionPainInstructionLocked moves a record to a new state after
// checking the instruction-level transition map, and emits the
// hash-chained "pain001_status" audit event for the move. Callers must
// hold the write lock. The audit event is emitted under the write
// lock; the enqueue never blocks (see emitAudit).
func (l *Ledger) transitionPainInstructionLocked(rec *PainInstruction, to, actor string) error {
	from := rec.State
	if from == to {
		return nil
	}
	legal := false
	for _, next := range pain001InstructionTransitions[from] {
		if next == to {
			legal = true
			break
		}
	}
	if !legal {
		return fmt.Errorf("%w: instruction %q %s -> %s", ErrIllegalPain001Transition, rec.MsgID, from, to)
	}
	rec.State = to
	l.emitAudit(AuditEvent{
		Op:      "pain001_status",
		Actor:   actor,
		TraceID: rec.MsgID,
		// The version brackets are identical: tracking an
		// instruction's lifecycle books nothing, like Reconcile, but
		// the audit event records the state against which ledger
		// version it was observed.
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"msg_id": rec.MsgID,
			"tx_id":  "",
			"from":   from,
			"to":     to,
		},
	})
	// Domain outbox (LG-102): the instruction's status move is a
	// domain fact — webhook subscribers watch it to track bank-side
	// settlement progress without polling.
	l.emitOutboxPain001StatusLocked(rec.MsgID, "", from, to, "", actor)
	return nil
}

// rollupPainInstructionLocked recomputes the instruction-level state
// from the per-transfer states after a batch of transitions, and
// applies the derived move (if any) through the transition map.
// Callers must hold the write lock.
func (l *Ledger) rollupPainInstructionLocked(rec *PainInstruction, actor string) error {
	to := Pain001StateExported
	if len(rec.Txs) > 0 {
		settled, accepted, rejected := 0, 0, 0
		for _, tx := range rec.Txs {
			switch tx.State {
			case Pain001StateSettled:
				settled++
			case Pain001StateAccepted:
				accepted++
			case Pain001StateRejected:
				rejected++
			}
		}
		switch {
		case settled == len(rec.Txs):
			to = Pain001StateSettled
		case accepted+settled == len(rec.Txs):
			to = Pain001StateAccepted
		case rejected == len(rec.Txs):
			to = Pain001StateRejected
		default:
			// A mix of unreceipted and receipted transfers (or a
			// rejected transfer alongside accepted ones): the
			// instruction is still in flight, so it stays exported
			// and the pending disclosure keeps naming it.
			to = Pain001StateExported
		}
	}
	return l.transitionPainInstructionLocked(rec, to, actor)
}

// pain001TxByIDLocked finds a transfer's state record inside an
// instruction. Callers must hold the write lock (callers mutate the
// returned pointer's fields); the lookup itself needs no lock.
func pain001TxByIDLocked(rec *PainInstruction, txID string) *Pain001TxState {
	for i := range rec.Txs {
		if rec.Txs[i].TxID == txID {
			return &rec.Txs[i]
		}
	}
	return nil
}

// RecordPain001Receipts applies a batch of bank receipts (pain.002
// style) to an instruction's transfers. Every receipt is validated
// first — status vocabulary, transfer membership, transition legality —
// and only then applied: a batch with one bad receipt applies nothing,
// so the record can never half-absorb a bank file. Receipt replay is
// idempotent: a receipt naming a state the transfer already holds is a
// no-op (no transition, no audit event), so re-delivered bank files
// are safe. Each applied transition emits a "pain001_status" audit
// event; when the batch moves the instruction-level state, that move
// gets its own event too.
func (l *Ledger) RecordPain001Receipts(msgID string, receipts []Pain001Receipt, actor string) (PainInstruction, error) {
	if len(receipts) == 0 {
		return PainInstruction{}, ErrEmptyPain001Receipts
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	rec, ok := l.painInstructions[msgID]
	if !ok {
		return PainInstruction{}, fmt.Errorf("%w: %q", ErrUnknownPainInstruction, msgID)
	}
	// Validate the whole batch before touching the record: a batch is
	// one bank file, and applying half of it would invent a state the
	// bank never reported.
	type plannedMove struct {
		tx      *Pain001TxState
		to      string
		receipt Pain001Receipt
	}
	moves := make([]plannedMove, 0, len(receipts))
	seen := make(map[string]bool, len(receipts))
	for i, r := range receipts {
		if r.Status != Pain001StateAccepted && r.Status != Pain001StateRejected {
			return PainInstruction{}, fmt.Errorf("%w: receipt %d for %q", ErrInvalidPain001ReceiptStatus, i, r.TxID)
		}
		if r.TxID == "" {
			return PainInstruction{}, fmt.Errorf("%w: receipt %d names an empty transfer", ErrUnknownPain001Tx, i)
		}
		tx := pain001TxByIDLocked(rec, r.TxID)
		if tx == nil {
			return PainInstruction{}, fmt.Errorf("%w: %q not in instruction %q", ErrUnknownPain001Tx, r.TxID, msgID)
		}
		if seen[r.TxID] {
			// The same transfer receipted twice in one batch: the
			// batch is the bank's single statement of fact, so a
			// duplicate that agrees is harmless, but one that
			// contradicts itself refuses the batch — the bank file
			// contradicts itself, and applying either reading would
			// invent a state the bank did not report.
			for _, m := range moves {
				if m.tx == tx && (m.to != r.Status || m.receipt.Reason != r.Reason) {
					return PainInstruction{}, fmt.Errorf("%w: receipt %d contradicts an earlier receipt for %q", ErrIllegalPain001Transition, i, r.TxID)
				}
			}
			continue
		}
		seen[r.TxID] = true
		if tx.State == r.Status {
			// Same-state replay: idempotent no-op, like a
			// duplicate export or a duplicate idempotency key.
			continue
		}
		legal := false
		for _, next := range pain001TxTransitions[tx.State] {
			if next == r.Status {
				legal = true
				break
			}
		}
		if !legal {
			return PainInstruction{}, fmt.Errorf("%w: transfer %q %s -> %s", ErrIllegalPain001Transition, r.TxID, tx.State, r.Status)
		}
		moves = append(moves, plannedMove{tx: tx, to: r.Status, receipt: r})
	}
	// Apply the validated batch: per-transfer transitions with their
	// audit events, then the instruction-level rollup.
	for _, m := range moves {
		from := m.tx.State
		m.tx.State = m.to
		m.tx.Reason = m.receipt.Reason
		m.tx.UpdatedAt = time.Now().UTC()
		l.emitAudit(AuditEvent{
			Op:            "pain001_status",
			Actor:         actor,
			TraceID:       rec.MsgID,
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Details: map[string]any{
				"msg_id": rec.MsgID,
				"tx_id":  m.tx.TxID,
				"from":   from,
				"to":     m.to,
				"reason": m.tx.Reason,
			},
		})
		// Domain outbox (LG-102): the bank's accepted/rejected
		// verdict on one transfer is the per-transfer status move
		// webhook subscribers watch.
		l.emitOutboxPain001StatusLocked(rec.MsgID, m.tx.TxID, from, m.to, m.tx.Reason, actor)
	}
	if err := l.rollupPainInstructionLocked(rec, actor); err != nil {
		return PainInstruction{}, err
	}
	return painInstructionSnapshotLocked(rec), nil
}

// SettlePain001Instruction is the operator's reconcile confirmation for
// a payment instruction: every transfer the bank accepted advances to
// settled. It never accepts on the bank's behalf — unreceipted
// (exported) transfers stay exported, rejected transfers stay
// rejected — so the operator cannot mark money settled that the bank
// never confirmed. Each accepted -> settled move emits a
// "pain001_status" audit event; when every transfer settles, the
// instruction-level move to settled gets its own event. The semantics
// are deliberately explicit rather than hooked into the automatic
// Reconcile scan: settlement confirmation is an operator attestation,
// not something a read-only report should perform as a side effect.
// The reconcile report instead discloses unsettled instructions (see
// Pain001Pending in ReconciliationReport), so the operator knows which
// instructions still await this confirmation.
func (l *Ledger) SettlePain001Instruction(msgID, actor string) (PainInstruction, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	rec, ok := l.painInstructions[msgID]
	if !ok {
		return PainInstruction{}, fmt.Errorf("%w: %q", ErrUnknownPainInstruction, msgID)
	}
	for i := range rec.Txs {
		tx := &rec.Txs[i]
		if tx.State != Pain001StateAccepted {
			continue
		}
		tx.State = Pain001StateSettled
		tx.UpdatedAt = time.Now().UTC()
		l.emitAudit(AuditEvent{
			Op:            "pain001_status",
			Actor:         actor,
			TraceID:       rec.MsgID,
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Details: map[string]any{
				"msg_id": rec.MsgID,
				"tx_id":  tx.TxID,
				"from":   Pain001StateAccepted,
				"to":     Pain001StateSettled,
			},
		})
	}
	if err := l.rollupPainInstructionLocked(rec, actor); err != nil {
		return PainInstruction{}, err
	}
	return painInstructionSnapshotLocked(rec), nil
}

// Pain001PendingInstruction is one row of the reconcile report's
// pain001_pending section: an instruction that has not reached the
// terminal settled state, with the transfers still awaiting a bank
// receipt or a settle confirmation named explicitly.
type Pain001PendingInstruction struct {
	MsgID          string    `json:"msg_id"`
	State          string    `json:"state"`
	TxCount        int       `json:"tx_count"`
	UnsettledTxIDs []string  `json:"unsettled_tx_ids"`
	ExportedAt     time.Time `json:"exported_at"`
}

// pain001PendingLocked builds the reconcile report's pain001_pending
// section: every instruction whose state is not settled, sorted by
// MsgId. A rejected instruction is disclosed, not hidden: its transfers
// never settled, and the operator needs to see it to fix the cause and
// re-export. Callers must hold l.mu; the read lock suffices because
// nothing is mutated.
func (l *Ledger) pain001PendingLocked() []Pain001PendingInstruction {
	out := make([]Pain001PendingInstruction, 0, len(l.painInstructions))
	for _, rec := range l.painInstructions {
		if rec.State == Pain001StateSettled {
			continue
		}
		unsettled := make([]string, 0, len(rec.Txs))
		for _, tx := range rec.Txs {
			if tx.State != Pain001StateSettled {
				unsettled = append(unsettled, tx.TxID)
			}
		}
		sort.Strings(unsettled)
		out = append(out, Pain001PendingInstruction{
			MsgID:          rec.MsgID,
			State:          rec.State,
			TxCount:        rec.TxCount,
			UnsettledTxIDs: unsettled,
			ExportedAt:     rec.ExportedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MsgID < out[j].MsgID })
	return out
}
