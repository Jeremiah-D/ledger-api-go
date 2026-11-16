package ledger

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// SWIFT FIN MT202 general financial institution transfer INBOUND
// processing (LG-442): the inbound half of the MT202 lifecycle,
// the mirror of the LG-378 export.
//
// ExportMT202 renders this ledger's own MT202 instruction for a
// correspondent — the ledger telling the correspondent "move our
// funds". This module consumes the instruction a correspondent
// sent to this ledger, acting as the receiving bank: it books the
// instructed bank-to-bank transfer through PostTransfer, and
// nothing else. The two directions are orthogonal by construction:
//
//   - LG-378 exports: the ledger renders its own MT202 for the
//     correspondent (outbound, read-only).
//   - LG-442 imports: a correspondent file asks the ledger to
//     book a general financial institution transfer (inbound,
//     state-changing: one journal entry per instruction, plus the
//     :20: replay record).
//
// ParseMT202 (LG-204) builds the value object this module
// applies — unlike ParseMT103 there is no separate inbound
// struct: the MT202 wire fields are exactly the apply terms.
// Parsing is strict, along ParseMT202: the block structure, the
// field order, and every field value are validated, and anything
// outside the five-field core set (:20: :21: :32A: :52A: :58A:)
// is refused — a payment instruction must not lose content
// quietly. CRLF line endings are accepted; a trailing newline
// after the `-}` trailer is tolerated.
//
// The wire fields, and what they route to:
//
//   - :20: is the transaction reference: the audit trace and the
//     replay key — one :20: names one instruction. It is also
//     the journal entry ID and the idempotency key's tail
//     ("mt202-"+:20:), so the correspondent can dedupe on it
//     exactly like an exported instruction.
//   - :21: is the related reference: recorded in the audit
//     details only — never used for routing. It names the
//     instruction this transfer relates to (typically the
//     customer leg's MT103), not a leg the ledger picks.
//   - :32A: is the value date, currency, and amount. The value
//     date must be today's UTC date (the LG-430 pain.001 rule):
//     the ledger has no future-date (or past-date) execution
//     scheduling, so an instruction dated any other day parks
//     in the repair queue instead of inventing a holding
//     position. The amount must be positive (the :32A: 15d
//     rule).
//   - :52A: is the ordering institution BIC and :58A: is the
//     beneficiary institution BIC. Each is reverse-looked-up in
//     the nostro registry by EXACT match on the Correspondent
//     string (nostroByCorrespondentLocked): the operator
//     registers Correspondent as the BIC string, so the BIC
//     names the ledger account that holds the correspondent
//     relationship. A BIC no account registered parks the whole
//     instruction in the repair queue — the ledger never opens
//     an account from a wire identifier (the pain.001
//     convention, LG-430), and it never guesses an account.
//   - The kind gate is the direction of the money: the :52A:
//     account is debited only when its registration kind is
//     nostro (our money at the correspondent bank), and the
//     :58A: account is credited only when its kind is vostro
//     (their money with us). A registered account of the wrong
//     kind parks the instruction in the repair queue — the
//     ledger does not move money the wrong way across a
//     nostro/vostro boundary.
//
// Fail-closed parking (the pacs.004 convention, LG-413): an
// instruction that fails any apply gate parks in the repair
// queue for operator resolution — an unregistered BIC, a wrong
// kind, a value date that is not today, or any PostTransfer
// refusal (same-party, frozen/overdraft, daily limit,
// accounting period, dual-control review). Repair records
// parked by this module are visibility parking, not
// resubmittable instructions: their Maker is deliberately
// empty, so the CreatePayout gate can never pass them.
//
// Idempotency is the :20: with its terms (the pain.011
// convention, LG-426): re-applying the same :20: with the same
// terms returns the stored outcome with Duplicate set, booking
// nothing new and emitting no second audit event. A :20:
// reused for different terms is refused with
// ErrMT202MsgIDConflict — never silently overwritten. The
// per-instruction idempotency key is a second layer: even if
// the in-memory replay record is lost (a restart),
// PostTransfer replays the original receipt instead of booking
// twice.
//
// No MT202 export change; no ParseMT202 change; no PostTransfer
// semantic change; no BIC registry (the nostro registry is the
// only routing table, and it is operator-maintained); no
// automatic account opening; no future-date scheduling; no
// MT900 confirmation.

// MT202 inbound errors.
var (
	// ErrMT202MsgIDConflict is returned by ApplyMT202 when a
	// :20: reference already applied carries different terms:
	// the reference names one instruction, and a second,
	// different instruction under it is refused rather than
	// silently overwritten (the pain.011
	// ErrPain011MsgIDConflict convention). It is a 422-class
	// error.
	ErrMT202MsgIDConflict = errors.New("ledger: mt202 inbound: message reference already applied with different terms")
	// ErrMT202UnknownBIC is returned per instruction when a
	// :52A: or :58A: BIC names no registered nostro account:
	// the ledger never opens an account from a wire
	// identifier — the instruction parks in the repair queue.
	ErrMT202UnknownBIC = errors.New("ledger: mt202 inbound: BIC names no registered nostro account")
	// ErrMT202WrongKind is returned per instruction when a BIC
	// resolves to a registered nostro account of the wrong kind
	// for its leg: the :52A: account must be a nostro account
	// (debited) and the :58A: account must be a vostro account
	// (credited). The instruction parks in the repair queue.
	ErrMT202WrongKind = errors.New("ledger: mt202 inbound: nostro account has the wrong kind for its leg")
	// ErrMT202ValueDateNotToday is returned per instruction
	// when the :32A: value date is not today's UTC date: the
	// ledger has no future-date (or past-date) execution
	// scheduling, so the instruction parks in the repair queue.
	ErrMT202ValueDateNotToday = errors.New("ledger: mt202 inbound: value date is not today (UTC)")
)

// MT202ApplyResult is the outcome of ApplyMT202.
type MT202ApplyResult struct {
	// MsgID is the :20: reference of the applied instruction.
	MsgID string `json:"msg_id"`
	// From and To are the ledger accounts the wire BICs
	// resolved to, empty when the instruction was rejected
	// before the lookup (or the lookup itself failed).
	From AccountID `json:"from_account,omitempty"`
	To   AccountID `json:"to_account,omitempty"`
	// Receipt is the PostTransfer receipt on success or
	// replay; empty when rejected.
	Receipt TransferReceipt `json:"receipt,omitempty"`
	// EntryID is the booked entry's ID (the instruction's
	// :20:), empty when rejected.
	EntryID string `json:"entry_id,omitempty"`
	// Duplicate reports an idempotent replay: the same
	// instruction was applied before, so the earlier entry is
	// returned and nothing new was booked — no new audit
	// event.
	Duplicate bool `json:"duplicate,omitempty"`
	// ReviewStatus carries the PostTransfer dual-control state
	// ("pending_review" when the transfer was routed into
	// operator review instead of settling); empty otherwise.
	ReviewStatus string `json:"review_status,omitempty"`
	// RepairID is the repair-queue record holding a rejected
	// instruction for operator resolution, empty on success.
	RepairID string `json:"repair_id,omitempty"`
	// Err is the per-instruction rejection reason, nil on
	// success.
	Err error `json:"-"`
}

// mt202AppliedRecord is the replay record for one applied MT202
// instruction, keyed by :20: in mt202Applied: the instruction
// terms it carried and the apply outcome, so a replay returns
// the same outcome without re-booking or re-parking.
type mt202AppliedRecord struct {
	Reference string
	Request   MT202
	Result    MT202ApplyResult
}

// mt202TermsEqual reports whether a stored instruction record
// carries exactly the terms an inbound request would apply: the
// same transaction and related references, value date (by
// calendar day, as :32A: carries only a day), currency and
// amount, and institution BICs. The block 1/2 LT addresses are
// envelope, not terms, and are not compared.
func mt202TermsEqual(rec mt202AppliedRecord, req MT202) bool {
	a, b := rec.Request, req
	return a.TransactionReference == b.TransactionReference &&
		a.RelatedReference == b.RelatedReference &&
		mt103ValueDate(a.ValueDate) == mt103ValueDate(b.ValueDate) &&
		a.Currency == b.Currency &&
		a.AmountCents == b.AmountCents &&
		a.OrderingInstitutionBIC == b.OrderingInstitutionBIC &&
		a.BeneficiaryInstitutionBIC == b.BeneficiaryInstitutionBIC
}

// mt202NormalizeInbound returns req with its BICs and currency
// upper-cased, so hand-built requests compare equal to parsed
// ones — and so the nostro reverse lookup sees the canonical
// BIC string the operator registered as Correspondent.
func mt202NormalizeInbound(req MT202) MT202 {
	req.OrderingInstitutionBIC = strings.ToUpper(strings.TrimSpace(req.OrderingInstitutionBIC))
	req.BeneficiaryInstitutionBIC = strings.ToUpper(strings.TrimSpace(req.BeneficiaryInstitutionBIC))
	req.Currency = strings.ToUpper(strings.TrimSpace(req.Currency))
	return req
}

// nostroByCorrespondentLocked reverse-looks-up one nostro
// registration by EXACT match on the Correspondent string: the
// operator registers Correspondent as the BIC string, so the
// :52A:/:58A: BIC names the ledger account directly. The caller
// holds l.mu (either lock suffices — the scan mutates nothing).
// A Correspondent registered on two accounts is an operator
// error the registry does not adjudicate: the first match in
// map order wins, so each BIC must be registered exactly once.
func (l *Ledger) nostroByCorrespondentLocked(bic string) (NostroAccount, bool) {
	for _, rec := range l.nostroAccounts {
		if rec.Correspondent == bic {
			cp := *rec
			return cp, true
		}
	}
	return NostroAccount{}, false
}

// parkMT202 parks one parsed MT202 instruction that failed
// closed, and returns the repair record ID. The snapshot's ID is
// the instruction's idempotency key, so the parked item is
// traceable to the exact instruction it refused; its Maker is
// deliberately empty (see the package comment).
func (l *Ledger) parkMT202(req MT202, reasonCode, actor string) (string, error) {
	rec, err := l.EnqueueRepair(PayoutInstruction{
		ID:          "mt202-" + req.TransactionReference,
		AmountCents: req.AmountCents,
		Currency:    req.Currency,
		// Maker intentionally empty: see the package comment.
		Destination: "mt202-inbound:" + req.TransactionReference,
	}, reasonCode, actor)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// ApplyMT202 books one parsed inbound SWIFT FIN MT202 general
// financial institution transfer: it moves the instructed amount
// from the nostro account the :52A: BIC resolves to, to the
// vostro account the :58A: BIC resolves to, through
// PostTransfer, with the :20: as both the journal entry ID and
// the idempotency key's tail ("mt202-"+:20:).
//
// The whole instruction is validated before anything applies
// (mt202Validate, the parse gate's own check, so hand-built
// requests fail closed the same way parsed ones do). Then the
// gates run in order, each fail-closed into the repair queue:
// the :32A: value date must be today's UTC date; each wire BIC
// must reverse-resolve to a registered nostro account of the
// right kind for its leg (nostro for :52A:, vostro for :58A:);
// and PostTransfer's own gates (field validation — same-party,
// non-positive amount — idempotency replay, frozen/overdraft,
// daily limit, accounting period, dual-control review) apply
// last. A refusal parks the instruction and returns its outcome
// with Err set and RepairID set, not an error return — the
// repair record is the trail, exactly like the pain.001 batch
// path. The :21: related reference is recorded in the audit
// details only and never used for routing.
//
// A booked instruction emits one `mt202_inbound_applied` audit
// event (details msg_id / ordering_bic / beneficiary_bic /
// amount_cents / currency / related_reference). An instruction
// that books nothing emits no booked event: the repair record
// is the trail.
//
// Idempotency is the :20: with its terms (the pain.011
// convention): re-applying the same :20: with the same terms
// returns the stored outcome with Duplicate set, booking
// nothing new and emitting no second audit event. A :20:
// reused for different terms is refused with
// ErrMT202MsgIDConflict.
//
// Locking: the structural validation runs on the parsed values
// alone; the replay read and the BIC resolution run under the
// read lock; the booking itself runs outside it, because
// PostTransfer takes the write lock; the repair parking runs
// outside any held lock because EnqueueRepair takes l.mu
// itself; the :20: record and the inbound audit event run under
// one short write lock of their own, after the booking is
// durable — re-checking the registry first so a concurrent
// double-apply still records once.
func (l *Ledger) ApplyMT202(req MT202, actor string) (MT202ApplyResult, error) {
	if actor == "" {
		actor = "ApplyMT202"
	}
	req = mt202NormalizeInbound(req)
	if _, _, err := mt202Validate(req); err != nil {
		return MT202ApplyResult{}, err
	}
	res := MT202ApplyResult{MsgID: req.TransactionReference}

	// :20: replay: the instruction was already applied under
	// this reference. Same terms is a no-op returning the
	// stored outcome; different terms is a conflict.
	l.mu.RLock()
	rec, ok := l.mt202Applied[req.TransactionReference]
	l.mu.RUnlock()
	if ok {
		if !mt202TermsEqual(rec, req) {
			return MT202ApplyResult{}, fmt.Errorf("%w: :20: %q", ErrMT202MsgIDConflict, req.TransactionReference)
		}
		out := rec.Result
		out.Duplicate = true
		return out, nil
	}

	// The value date is today's UTC date — the ledger has no
	// future-date (or past-date) execution scheduling.
	today := time.Now().UTC().Format("2006-01-02")
	if req.ValueDate.UTC().Format("2006-01-02") != today {
		res.Err = fmt.Errorf("%w: :32A: value date %q is not today (UTC)", ErrMT202ValueDateNotToday, mt103ValueDate(req.ValueDate))
		repairID, parkErr := l.parkMT202(req, RepairReasonInvalidFormat, actor)
		if parkErr != nil {
			res.Err = fmt.Errorf("%w (repair parking failed: %v)", res.Err, parkErr)
			return res, nil
		}
		res.RepairID = repairID
		return l.recordMT202Applied(req, res, actor)
	}

	// Each wire BIC must reverse-resolve to a registered nostro
	// account of the right kind for its leg: the :52A: account
	// is debited only when it is a nostro account, the :58A:
	// account is credited only when it is a vostro account. No
	// account is ever opened from a wire identifier, and no
	// account is ever guessed.
	l.mu.RLock()
	ordering, orderingOK := l.nostroByCorrespondentLocked(req.OrderingInstitutionBIC)
	beneficiary, beneficiaryOK := l.nostroByCorrespondentLocked(req.BeneficiaryInstitutionBIC)
	l.mu.RUnlock()
	if !orderingOK {
		res.Err = fmt.Errorf("%w: :52A: BIC %q", ErrMT202UnknownBIC, req.OrderingInstitutionBIC)
		repairID, parkErr := l.parkMT202(req, RepairReasonUnknownRoute, actor)
		if parkErr != nil {
			res.Err = fmt.Errorf("%w (repair parking failed: %v)", res.Err, parkErr)
			return res, nil
		}
		res.RepairID = repairID
		return l.recordMT202Applied(req, res, actor)
	}
	if ordering.Kind != NostroKindNostro {
		res.Err = fmt.Errorf("%w: :52A: BIC %q resolves to %q, want kind nostro", ErrMT202WrongKind, req.OrderingInstitutionBIC, ordering.Account)
		repairID, parkErr := l.parkMT202(req, RepairReasonInvalidFormat, actor)
		if parkErr != nil {
			res.Err = fmt.Errorf("%w (repair parking failed: %v)", res.Err, parkErr)
			return res, nil
		}
		res.RepairID = repairID
		return l.recordMT202Applied(req, res, actor)
	}
	if !beneficiaryOK {
		res.Err = fmt.Errorf("%w: :58A: BIC %q", ErrMT202UnknownBIC, req.BeneficiaryInstitutionBIC)
		repairID, parkErr := l.parkMT202(req, RepairReasonUnknownRoute, actor)
		if parkErr != nil {
			res.Err = fmt.Errorf("%w (repair parking failed: %v)", res.Err, parkErr)
			return res, nil
		}
		res.RepairID = repairID
		return l.recordMT202Applied(req, res, actor)
	}
	if beneficiary.Kind != NostroKindVostro {
		res.Err = fmt.Errorf("%w: :58A: BIC %q resolves to %q, want kind vostro", ErrMT202WrongKind, req.BeneficiaryInstitutionBIC, beneficiary.Account)
		repairID, parkErr := l.parkMT202(req, RepairReasonInvalidFormat, actor)
		if parkErr != nil {
			res.Err = fmt.Errorf("%w (repair parking failed: %v)", res.Err, parkErr)
			return res, nil
		}
		res.RepairID = repairID
		return l.recordMT202Applied(req, res, actor)
	}
	res.From = ordering.Account
	res.To = beneficiary.Account

	// Booking reuses PostTransfer: its gates (field validation,
	// idempotency replay, frozen/overdraft, daily limit,
	// accounting period, dual-control review) apply, and a
	// refusal parks the instruction in the repair queue. The
	// journal entry ID is the :20: and the idempotency key is
	// "mt202-"+:20:, so the correspondent can dedupe on it
	// exactly like an exported instruction. The transfer is
	// single-currency by construction: ToCurrency stays empty,
	// so a cross-currency payment can never be booked as one
	// MT202 instruction.
	receipt, err := l.PostTransfer(Transfer{
		ID:             req.TransactionReference,
		From:           ordering.Account,
		To:             beneficiary.Account,
		AmountCents:    req.AmountCents,
		Currency:       req.Currency,
		IdempotencyKey: "mt202-" + req.TransactionReference,
	})
	if err != nil {
		res.Err = err
		repairID, parkErr := l.parkMT202(req, RepairReasonInvalidFormat, actor)
		if parkErr != nil {
			res.Err = fmt.Errorf("%w (repair parking failed: %v)", err, parkErr)
			return res, nil
		}
		res.RepairID = repairID
		return l.recordMT202Applied(req, res, actor)
	}
	res.Receipt = receipt
	res.EntryID = receipt.TransferID
	if len(receipt.Entries) > 0 {
		res.EntryID = receipt.Entries[0].ID
	}
	res.Duplicate = receipt.Duplicate
	res.ReviewStatus = receipt.ReviewStatus
	return l.recordMT202Applied(req, res, actor)
}

// recordMT202Applied stores the :20: replay record for one
// applied MT202 instruction and, when the instruction booked,
// emits the one `mt202_inbound_applied` audit event — under one
// short write lock, after the booking (or parking) is durable.
// A concurrent double-apply that landed first returns the
// stored outcome instead of a second record: the stored
// outcome wins, so exactly one marker event is ever emitted.
func (l *Ledger) recordMT202Applied(req MT202, res MT202ApplyResult, actor string) (MT202ApplyResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.mt202Applied == nil {
		l.mt202Applied = make(map[string]mt202AppliedRecord)
	}
	if rec, ok := l.mt202Applied[req.TransactionReference]; ok {
		if !mt202TermsEqual(rec, req) {
			return MT202ApplyResult{}, fmt.Errorf("%w: :20: %q", ErrMT202MsgIDConflict, req.TransactionReference)
		}
		out := rec.Result
		out.Duplicate = true
		return out, nil
	}
	l.mt202Applied[req.TransactionReference] = mt202AppliedRecord{
		Reference: req.TransactionReference,
		Request:   req,
		Result:    res,
	}
	if res.Err == nil {
		l.emitAudit(AuditEvent{
			Op:            "mt202_inbound_applied",
			Actor:         actor,
			TraceID:       req.TransactionReference,
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Accounts:      []AccountID{res.From, res.To},
			Details: map[string]any{
				"msg_id":            req.TransactionReference,
				"ordering_bic":      req.OrderingInstitutionBIC,
				"beneficiary_bic":   req.BeneficiaryInstitutionBIC,
				"amount_cents":      req.AmountCents,
				"currency":          req.Currency,
				"related_reference": req.RelatedReference,
			},
		})
	}
	return res, nil
}
