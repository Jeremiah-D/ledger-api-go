package ledger

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Payout instructions: the operator lifecycle of money leaving the
// ledger to an external destination (LG-140, fintech).
//
// A payout is an instruction, not just a journal row. The operator
// drafts it, a checker approves it (maker-checker), the ledger submits
// it by earmarking the funds in a clearing account, and settlement is
// confirmed against the bank statement — the same statement-matching
// machinery the reconciliation report uses (see statement_match.go,
// LG-107). Lifecycle:
//
//	draft -> approved -> submitted -> settled | failed
//	draft | approved -> cancelled
//
// Clearing economics: submit books debit payout-clearing / credit
// payee (the "待清算户"), earmarking the payer's funds as a suspense
// balance — the legs mirror a transfer payee -> clearing under the
// ledger's transfer convention (Dr To / Cr From, see PostTransfer), so
// the payee's balance drops and the clearing account holds the earmark.
// Settling (or failing) reverses it — debit payee / credit clearing,
// "冲待清算" — so the clearing account nets to zero per payout. The
// economic outflow itself is evidenced by the matched bank statement
// line, not by a second journal leg: the ledger tracks the instruction
// and the suspense, and the statement is the external proof the money
// moved. A failed payout (bank rejected) reverses the suspense the same
// way but keeps the failed status with the reason, so the books never
// show money as left when it did not.
//
// Maker-checker (LG-42): approval runs through the dual-control review
// threshold configured for the payee account (see
// effectiveReviewThresholdLocked). At or above the threshold the checker
// must differ from the maker — the classic separation of duties;
// below the threshold (or with no threshold configured) any approver
// may sign, and the approver is still recorded.
//
// The clearing account is fail-closed (see SetPayoutClearingAccount):
// submitting without one configured is rejected instead of booking
// against a zero account. The submit leg posts through the full Post
// gate order (postLocked): denylist, replay, VAN, external-ref,
// chart direction, closed, dormant, frozen, suspended, overdraft,
// daily-limit, period — the instruction cannot route around the
// ledger's risk controls.
//
// Idempotency: payout opens carry their own key namespace (payoutKeys),
// independent of Post's byKey and every other operation's namespace,
// expiring with the ledger's idempotency TTL like the others. Opening
// with a replayed key returns the original instruction. Submit/settle/
// fail/cancel are idempotent state transitions: re-running them on an
// already-transitioned instruction returns it as-is.
//
// In-memory only: like clearing batches (see clearing.go), payout
// instructions do not round-trip through disaster-recovery snapshots —
// the journaled clearing/settlement rows do, so the money is safe; only
// the instruction metadata is lost. The payout clearing account, being
// fail-closed structural config, does round-trip like the other
// interest-expense accounts.

// Payout instruction statuses.
const (
	PayoutDraft     = "draft"
	PayoutApproved  = "approved"
	PayoutSubmitted = "submitted"
	PayoutSettled   = "settled"
	PayoutFailed    = "failed"
	PayoutCancelled = "cancelled"
	// PayoutReturned is set by ApplyPacs004Return (LG-206) when the
	// bank reports a settled payout's funds back via pacs.004: the
	// money came back, so the instruction is terminal — it can
	// neither settle again nor be cancelled (camt.056's default
	// branch refuses it fail-closed).
	PayoutReturned = "returned"
)

// Validation and state errors returned by the payout APIs.
var (
	// ErrEmptyPayoutID is returned when the payout ID is empty.
	ErrEmptyPayoutID = errors.New("ledger: payout ID must not be empty")
	// ErrPayoutIDSeparator is returned when the payout ID contains '/':
	// the deterministic clearing/settlement entry IDs join their
	// components with '/', so a '/' inside the ID would let two
	// different payouts share one clearing row (same rationale as the
	// term-deposit IDs, see ErrTermDepositIDSeparator).
	ErrPayoutIDSeparator = errors.New("ledger: payout ID must not contain '/'")
	// ErrEmptyPayoutAccount is returned when the payee account is empty.
	ErrEmptyPayoutAccount = errors.New("ledger: payout payee account must not be empty")
	// ErrPayoutNonPositiveAmount is returned for a non-positive payout
	// amount.
	ErrPayoutNonPositiveAmount = errors.New("ledger: payout amount must be greater than zero")
	// ErrEmptyPayoutDestination is returned when the external
	// destination reference is empty.
	ErrEmptyPayoutDestination = errors.New("ledger: payout destination must not be empty")
	// ErrEmptyPayoutMaker is returned when the creating maker is empty:
	// every instruction needs a maker for the maker-checker trail.
	ErrEmptyPayoutMaker = errors.New("ledger: payout maker must not be empty")
	// ErrEmptyPayoutApprover is returned when the approver is empty.
	ErrEmptyPayoutApprover = errors.New("ledger: payout approver must not be empty")
	// ErrPayoutIDConflict is returned when the payout ID is already
	// used: instructions are never deleted, so a colliding ID is always
	// a caller bug, never a replay.
	ErrPayoutIDConflict = errors.New("ledger: payout ID already in use")
	// ErrPayoutNotFound is returned for an unknown payout ID.
	ErrPayoutNotFound = errors.New("ledger: payout not found")
	// ErrPayoutInvalidTransition is returned when the instruction is not
	// in a status the operation can run from (e.g. submitting a draft,
	// settling a cancelled payout).
	ErrPayoutInvalidTransition = errors.New("ledger: payout is not in a status for this operation")
	// ErrPayoutMakerCheckerViolation is returned when the approver is
	// the maker on a payout at or above the LG-42 review threshold:
	// maker-checker separation is mandatory there.
	ErrPayoutMakerCheckerViolation = errors.New("ledger: payout approver must differ from maker at or above the review threshold")
	// ErrPayoutClearingAccountNotConfigured is returned by SubmitPayout
	// when no payout clearing account is configured (see
	// SetPayoutClearingAccount): fail-closed, so the clearing leg is
	// never booked against a zero account.
	ErrPayoutClearingAccountNotConfigured = errors.New("ledger: payout clearing account not configured")
	// ErrPayoutUnknownDestination is returned when the destination
	// parses as "<system>:<externalID>" and the ledger has a registered
	// external mapping (LG-122) for the payee under that system, but
	// the external ID does not match the registered one.
	ErrPayoutUnknownDestination = errors.New("ledger: payout destination does not match the registered external account")
	// ErrPayoutStatementMismatch is returned by SettlePayout when the
	// supplied bank statement line does not evidence this payout's
	// clearing leg: wrong direction, no candidate, ambiguous
	// candidates, or a line that matched a different journal row.
	ErrPayoutStatementMismatch = errors.New("ledger: payout statement line did not match the clearing entry")
	// ErrEmptyPayoutFailReason is returned when failing a payout
	// without a reason: a failed instruction must say why.
	ErrEmptyPayoutFailReason = errors.New("ledger: payout fail reason must not be empty")
)

// PayoutInstruction is one operator-driven payout: money leaving the
// ledger's payee account for an external destination.
//
// Destination is the external account reference, LG-122 style
// (e.g. "FPS:6222000012345678"): free-form, but validated against the
// registered external mappings when it parses as "<system>:<externalID>".
type PayoutInstruction struct {
	ID             string    `json:"id"`
	PayeeAccount   AccountID `json:"payee_account"`
	AmountCents    int64     `json:"amount_cents"`
	Currency       string    `json:"currency"`
	Destination    string    `json:"destination"`
	IdempotencyKey string    `json:"idempotency_key,omitempty"`
	Maker          string    `json:"maker"`
	Approver       string    `json:"approver,omitempty"`
	Status         string    `json:"status"`
	CreatedAt      time.Time `json:"created_at"`
	ApprovedAt     time.Time `json:"approved_at,omitempty"`
	SubmittedAt    time.Time `json:"submitted_at,omitempty"`
	SettledAt      time.Time `json:"settled_at,omitempty"`
	// ClearingEntryID is the submit leg's journal row (debit payee /
	// credit payout-clearing). SettlementEntryID is the clearing
	// reversal (debit clearing / credit payee) booked on settle or
	// fail.
	ClearingEntryID   string `json:"clearing_entry_id,omitempty"`
	SettlementEntryID string `json:"settlement_entry_id,omitempty"`
	// ReturnEntryID is the pacs.004 return reversal row (debit
	// clearing / credit payee, "payout/<id>/return") booked by
	// ApplyPacs004Return (LG-206) when a settled payout's funds
	// come back. Empty until the payout is returned.
	ReturnEntryID string `json:"return_entry_id,omitempty"`
	// StatementLineID is the bank statement line that evidenced the
	// settlement (see SettlePayout).
	StatementLineID string `json:"statement_line_id,omitempty"`
	FailReason      string `json:"fail_reason,omitempty"`
	// UETR is the SWIFT gpi Unique End-to-end Transaction Reference
	// (UUID v4, RFC 4122), assigned automatically at draft time (see
	// gpi.go, LG-165): the cross-border payment's end-to-end
	// identifier from inception, tracked through the gpi state
	// machine.
	UETR string `json:"uetr"`
	// PurposeCode is the payout's ISO 20022 business purpose (see
	// purpose.go, LG-180): one of the admitted ExternalPurpose1Code
	// entries, e.g. SALA (salaries), SUPP (suppliers). Empty means
	// "no purpose stated"; a code outside the admitted subset is
	// rejected fail-closed with ErrInvalidPurposeCode. It rides the
	// pacs.008 outbound export as <Purp><Cd> (see pacs008_export.go).
	PurposeCode string `json:"purpose_code,omitempty"`
	// Rail is the settlement rail the payout travels (see sla.go,
	// LG-185/LG-187): one of fednow, rtp, sepa, swift, gpi, ach. Empty means "no
	// rail stated": the SLA watch opened at submit then uses the gpi
	// rail, since every payout is a cross-border payment initiation
	// with a UETR and a gpi track (LG-165). A name outside the rail
	// vocabulary is rejected fail-closed with ErrInvalidRail.
	Rail string `json:"rail,omitempty"`
}

// payoutClearingEntryID builds the deterministic submit-leg entry ID:
// "payout/<id>/clearing". One instruction admits at most one submit, so
// at most one clearing row exists; a retried submit replays it instead
// of double-booking.
func payoutClearingEntryID(id string) string {
	return "payout/" + id + "/clearing"
}

// payoutSettlementEntryID builds the deterministic clearing-reversal
// entry ID: "payout/<id>/settle" on settle, "payout/<id>/fail" on fail.
func payoutSettlementEntryID(id, kind string) string {
	return "payout/" + id + "/" + kind
}

// validatePayoutDestination checks the destination reference: non-empty,
// and — when it parses as "<system>:<externalID>" and the ledger holds
// a registered external mapping (LG-122) for the payee under that
// system — the external ID must equal the registered one. Callers must
// hold l.mu; the read lock suffices.
func (l *Ledger) validatePayoutDestinationLocked(payee AccountID, destination string) error {
	if destination == "" {
		return ErrEmptyPayoutDestination
	}
	system, externalID, ok := strings.Cut(destination, ":")
	if !ok || system == "" || externalID == "" {
		return nil
	}
	registered, found := l.externalAccountIDLocked(payee, system)
	if !found {
		return nil
	}
	if registered != externalID {
		return fmt.Errorf("%w: destination %q, registered %q", ErrPayoutUnknownDestination, destination, system+":"+registered)
	}
	return nil
}

// CreatePayout drafts a payout instruction. Validation runs first
// (non-empty slash-free ID, non-empty payee, positive amount, valid
// currency, non-empty destination validated against the LG-122 external
// mappings when applicable, non-empty maker), then the idempotency
// replay check on the payout namespace (a replay returns the original
// instruction with duplicate == true), then the payout-ID conflict
// check. The instruction is off-journal: drafting locks nothing and
// bumps nothing.
func (l *Ledger) CreatePayout(p PayoutInstruction) (PayoutInstruction, bool, error) {
	if p.ID == "" {
		return PayoutInstruction{}, false, ErrEmptyPayoutID
	}
	if strings.Contains(p.ID, "/") {
		return PayoutInstruction{}, false, ErrPayoutIDSeparator
	}
	if p.PayeeAccount == "" {
		return PayoutInstruction{}, false, ErrEmptyPayoutAccount
	}
	if p.AmountCents <= 0 {
		return PayoutInstruction{}, false, ErrPayoutNonPositiveAmount
	}
	currency, err := normalizeCurrency(p.Currency)
	if err != nil {
		return PayoutInstruction{}, false, err
	}
	p.Currency = currency
	if p.Maker == "" {
		return PayoutInstruction{}, false, ErrEmptyPayoutMaker
	}
	// The purpose code is field validation, like the currency: empty
	// means "no purpose stated"; anything else must be an exact match
	// in the admitted ISO 20022 subset (see checkPurposeCode), or the
	// payout is rejected fail-closed before anything is recorded.
	if err := checkPurposeCode(p.PurposeCode); err != nil {
		return PayoutInstruction{}, false, err
	}
	// The rail is field validation, like the purpose code: empty
	// means "no rail stated"; anything else must be an exact match in
	// the SLA rail vocabulary (see sla.go), or the payout is rejected
	// fail-closed before anything is recorded.
	rail, err := normalizeRail(p.Rail)
	if err != nil {
		return PayoutInstruction{}, false, err
	}
	p.Rail = rail

	l.mu.Lock()
	defer l.mu.Unlock()

	if err := l.validatePayoutDestinationLocked(p.PayeeAccount, p.Destination); err != nil {
		return PayoutInstruction{}, false, err
	}
	if p.IdempotencyKey != "" {
		if id, ok := l.payoutKeys[p.IdempotencyKey]; ok {
			return l.payouts[id], true, nil
		}
	}
	if _, exists := l.payouts[p.ID]; exists {
		return PayoutInstruction{}, false, ErrPayoutIDConflict
	}

	now := time.Now()
	l.maybePruneIdempotencyKeys(now)
	p.Status = PayoutDraft
	p.CreatedAt = now
	// LG-165: every payout is a cross-border payment initiation, so it
	// gets a UETR at draft time and opens a gpi tracking record in the
	// "tracked" state — the payment is tracked end-to-end from
	// inception.
	p.UETR = newUETR()
	l.registerGPITrackLocked(p.UETR, p.ID, now)
	l.payouts[p.ID] = p
	if p.IdempotencyKey != "" {
		l.payoutKeys[p.IdempotencyKey] = p.ID
	}
	openDetails := map[string]any{
		"amount_cents": p.AmountCents,
		"currency":     p.Currency,
		"destination":  p.Destination,
		"maker":        p.Maker,
		"uetr":         p.UETR,
	}
	if p.Rail != "" {
		openDetails["rail"] = p.Rail
	}
	l.emitAudit(AuditEvent{
		Op:            "payout_open",
		Actor:         "CreatePayout",
		TraceID:       p.ID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{p.PayeeAccount},
		Details:       openDetails,
	})
	l.emitOutboxLocked(OutboxEvent{
		Type:        OutboxPayoutCreated,
		Account:     p.PayeeAccount,
		AmountCents: p.AmountCents,
		Currency:    p.Currency,
		Details: map[string]any{
			"payout_id":   p.ID,
			"destination": p.Destination,
			"maker":       p.Maker,
		},
	})
	return p, false, nil
}

// GetPayout returns the instruction with the given ID, or false when no
// such instruction exists.
func (l *Ledger) GetPayout(id string) (PayoutInstruction, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	p, ok := l.payouts[id]
	return p, ok
}

// Payouts lists every payout instruction, sorted by ID: the read side
// of the instruction registry, backing the Reconcile disclosure.
func (l *Ledger) Payouts() []PayoutInstruction {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]PayoutInstruction, 0, len(l.payouts))
	for _, p := range l.payouts {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ApprovePayout moves a draft instruction to approved, recording the
// approver. Maker-checker (LG-42): at or above the payee account's
// effective review threshold the approver must differ from the maker —
// separation of duties is mandatory there; below the threshold (or
// with no threshold configured) any approver may sign. Approving an
// already-approved instruction is a no-op success; approving from any
// other status is rejected.
func (l *Ledger) ApprovePayout(id, approver string) (PayoutInstruction, error) {
	if approver == "" {
		return PayoutInstruction{}, ErrEmptyPayoutApprover
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	p, ok := l.payouts[id]
	if !ok {
		return PayoutInstruction{}, ErrPayoutNotFound
	}
	if p.Status == PayoutApproved {
		return p, nil
	}
	if p.Status != PayoutDraft {
		return PayoutInstruction{}, ErrPayoutInvalidTransition
	}
	// The LG-42 dual-control threshold decides where maker-checker is
	// mandatory: at or above it, the checker must be someone other than
	// the maker. effectiveReviewThresholdLocked reports ok=false when
	// no threshold is configured, in which case any approver may sign.
	if threshold, configured := l.effectiveReviewThresholdLocked(p.PayeeAccount); configured &&
		p.AmountCents >= threshold && approver == p.Maker {
		return PayoutInstruction{}, ErrPayoutMakerCheckerViolation
	}

	now := time.Now()
	p.Status = PayoutApproved
	p.Approver = approver
	p.ApprovedAt = now
	l.payouts[id] = p
	l.emitAudit(AuditEvent{
		Op:            "payout_approved",
		Actor:         "ApprovePayout",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{p.PayeeAccount},
		Details: map[string]any{
			"maker":        p.Maker,
			"approver":     approver,
			"amount_cents": p.AmountCents,
		},
	})
	l.emitOutboxLocked(OutboxEvent{
		Type:        OutboxPayoutApproved,
		Account:     p.PayeeAccount,
		AmountCents: p.AmountCents,
		Currency:    p.Currency,
		Details:     map[string]any{"payout_id": id, "approver": approver},
	})
	return p, nil
}

// SubmitPayout moves an approved instruction to submitted, earmarking
// the funds: it posts debit payout-clearing / credit payee through the
// full Post gate order (postLocked), so every account risk control —
// closed, dormant, frozen, suspended, overdraft, daily-limit, period —
// applies exactly as to an operator posting. The legs mirror a transfer
// payee -> clearing under the ledger's transfer convention (Dr To / Cr
// From): the payee's balance drops by the payout amount and the clearing
// account holds the earmark. The clearing account is fail-closed:
// submitting without one configured is rejected. The entry ID is the
// deterministic "payout/<id>/clearing", so a retried submit replays the
// original row instead of double-booking. Submitting an
// already-submitted instruction is a no-op success.
func (l *Ledger) SubmitPayout(id string) (PayoutInstruction, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	p, ok := l.payouts[id]
	if !ok {
		return PayoutInstruction{}, ErrPayoutNotFound
	}
	if p.Status == PayoutSubmitted {
		return p, nil
	}
	if p.Status != PayoutApproved {
		return PayoutInstruction{}, ErrPayoutInvalidTransition
	}
	if l.payoutClearingAccount == "" {
		return PayoutInstruction{}, ErrPayoutClearingAccountNotConfigured
	}

	now := time.Now()
	entryID := payoutClearingEntryID(id)
	e := JournalEntry{
		ID:             entryID,
		DebitAccount:   l.payoutClearingAccount,
		CreditAccount:  p.PayeeAccount,
		AmountCents:    p.AmountCents,
		Currency:       p.Currency,
		IdempotencyKey: entryID,
		Memo:           "payout:" + id + ":clearing",
		CreatedAt:      now,
	}
	posted, _, err := l.postLocked(e, true)
	if err != nil {
		return PayoutInstruction{}, err
	}
	p.Status = PayoutSubmitted
	p.SubmittedAt = now
	p.ClearingEntryID = posted.ID
	l.payouts[id] = p
	// LG-185: submitting opens the settlement-SLA watch — the
	// payment's timeliness is tracked from here. A payout with no
	// stated rail rides the gpi rail: every payout is a cross-border
	// initiation with a UETR and a gpi track (LG-165), so the
	// gpi/UETR settlement expectation governs.
	rail := p.Rail
	if rail == "" {
		rail = RailGPI
	}
	l.openSLAWatchLocked(p.ID, SLAWatchKindPayout, rail, now)
	l.emitAudit(AuditEvent{
		Op:            "payout_submitted",
		Actor:         "SubmitPayout",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{p.PayeeAccount, l.payoutClearingAccount},
		Details: map[string]any{
			"amount_cents":      p.AmountCents,
			"currency":          p.Currency,
			"clearing_entry_id": posted.ID,
		},
	})
	l.emitOutboxLocked(OutboxEvent{
		Type:        OutboxPayoutSubmitted,
		Account:     p.PayeeAccount,
		AmountCents: p.AmountCents,
		Currency:    p.Currency,
		Details:     map[string]any{"payout_id": id, "clearing_entry_id": posted.ID},
	})
	return p, nil
}

// SettlePayout moves a submitted instruction to settled once the bank
// statement evidences the money movement: the supplied statement line
// must be an outflow that matches this payout's clearing entry through
// the statement-matching machinery (see MatchStatement, LG-107) —
// exactly one candidate, and it must be the clearing row itself. An
// ambiguous line, a line matching a different row, or a non-outflow
// line is rejected with ErrPayoutStatementMismatch and the instruction
// stays submitted. On settle the clearing suspense is reversed (debit
// payee / credit clearing, "冲待清算") under the deterministic
// "payout/<id>/settle" ID, so the clearing account nets to zero; the
// matched line is recorded in the statement-match registry like any
// other import, keeping the Reconcile discrepancies honest. Settling an
// already-settled instruction is a no-op success.
func (l *Ledger) SettlePayout(id string, line StatementLine, opts StatementMatchOptions) (PayoutInstruction, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	p, ok := l.payouts[id]
	if !ok {
		return PayoutInstruction{}, ErrPayoutNotFound
	}
	if p.Status == PayoutSettled {
		return p, nil
	}
	if p.Status != PayoutSubmitted {
		return PayoutInstruction{}, ErrPayoutInvalidTransition
	}
	// A payout settlement is an outflow: an inflow line can never
	// evidence money leaving.
	if line.Direction != StatementDirectionOut {
		return PayoutInstruction{}, fmt.Errorf("%w: statement line %q is not an outflow",
			ErrPayoutStatementMismatch, line.ID)
	}
	report, err := l.matchStatementLocked(p.PayeeAccount, []StatementLine{line}, opts)
	if err != nil {
		return PayoutInstruction{}, err
	}
	item := report.Items[0]
	if item.Status != StatementMatched || item.MatchedEntryID != p.ClearingEntryID {
		return PayoutInstruction{}, fmt.Errorf("%w: line %q status %s matched %q",
			ErrPayoutStatementMismatch, line.ID, item.Status, item.MatchedEntryID)
	}

	now := time.Now()
	settleID := payoutSettlementEntryID(id, "settle")
	e := JournalEntry{
		ID:             settleID,
		DebitAccount:   p.PayeeAccount,
		CreditAccount:  l.payoutClearingAccount,
		AmountCents:    p.AmountCents,
		Currency:       p.Currency,
		IdempotencyKey: settleID,
		Memo:           "payout:" + id + ":settle",
		CreatedAt:      now,
	}
	posted, _, err := l.postLocked(e, true)
	if err != nil {
		return PayoutInstruction{}, err
	}
	p.Status = PayoutSettled
	p.SettledAt = now
	p.SettlementEntryID = posted.ID
	p.StatementLineID = line.ID
	l.payouts[id] = p
	// LG-185: settlement closes the SLA watch — the payment met its
	// rail's settlement expectation (the breach check runs on open
	// watches only, so a watch that closed on time never breaches).
	l.closeSLAWatchLocked(id, SLACloseSettled, now)
	l.emitAudit(AuditEvent{
		Op:            "payout_settled",
		Actor:         "SettlePayout",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{p.PayeeAccount, l.payoutClearingAccount},
		Details: map[string]any{
			"amount_cents":          p.AmountCents,
			"currency":              p.Currency,
			"settlement_entry_id":   posted.ID,
			"statement_line_id":     line.ID,
			"statement_match_score": item.Score,
		},
	})
	l.emitOutboxLocked(OutboxEvent{
		Type:        OutboxPayoutSettled,
		Account:     p.PayeeAccount,
		AmountCents: p.AmountCents,
		Currency:    p.Currency,
		Details:     map[string]any{"payout_id": id, "statement_line_id": line.ID},
	})
	return p, nil
}

// FailPayout moves a submitted instruction to failed: the bank rejected
// the payout, so the clearing suspense is reversed (debit payee / credit
// clearing, like the settle leg) under the deterministic
// "payout/<id>/fail" ID — the funds never left, and the books must show
// that. A reason is required: a failed instruction must say why.
// Failing an already-failed instruction is a no-op success.
func (l *Ledger) FailPayout(id, reason string) (PayoutInstruction, error) {
	if reason == "" {
		return PayoutInstruction{}, ErrEmptyPayoutFailReason
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	p, ok := l.payouts[id]
	if !ok {
		return PayoutInstruction{}, ErrPayoutNotFound
	}
	if p.Status == PayoutFailed {
		return p, nil
	}
	if p.Status != PayoutSubmitted {
		return PayoutInstruction{}, ErrPayoutInvalidTransition
	}

	now := time.Now()
	failID := payoutSettlementEntryID(id, "fail")
	e := JournalEntry{
		ID:             failID,
		DebitAccount:   p.PayeeAccount,
		CreditAccount:  l.payoutClearingAccount,
		AmountCents:    p.AmountCents,
		Currency:       p.Currency,
		IdempotencyKey: failID,
		Memo:           "payout:" + id + ":fail",
		CreatedAt:      now,
	}
	posted, _, err := l.postLocked(e, true)
	if err != nil {
		return PayoutInstruction{}, err
	}
	p.Status = PayoutFailed
	p.SettledAt = now
	p.SettlementEntryID = posted.ID
	p.FailReason = reason
	l.payouts[id] = p
	// LG-185: a failed payout resolves the SLA watch as failed — the
	// money is not moving, so the timeliness question is moot.
	l.closeSLAWatchLocked(id, SLACloseFailed, now)
	l.emitAudit(AuditEvent{
		Op:            "payout_failed",
		Actor:         "FailPayout",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{p.PayeeAccount, l.payoutClearingAccount},
		Details: map[string]any{
			"amount_cents":        p.AmountCents,
			"currency":            p.Currency,
			"settlement_entry_id": posted.ID,
			"reason":              reason,
		},
	})
	l.emitOutboxLocked(OutboxEvent{
		Type:        OutboxPayoutFailed,
		Account:     p.PayeeAccount,
		AmountCents: p.AmountCents,
		Currency:    p.Currency,
		Details:     map[string]any{"payout_id": id, "reason": reason},
	})
	return p, nil
}

// CancelPayout cancels a draft or approved instruction: nothing was ever
// booked, so cancelling touches no journal row. Cancelling from any
// other status is rejected — a submitted instruction must settle or
// fail, never vanish.
func (l *Ledger) CancelPayout(id string) (PayoutInstruction, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	p, ok := l.payouts[id]
	if !ok {
		return PayoutInstruction{}, ErrPayoutNotFound
	}
	if p.Status == PayoutCancelled {
		return p, nil
	}
	if p.Status != PayoutDraft && p.Status != PayoutApproved {
		return PayoutInstruction{}, ErrPayoutInvalidTransition
	}
	fromStatus := p.Status
	p.Status = PayoutCancelled
	l.payouts[id] = p
	l.emitAudit(AuditEvent{
		Op:            "payout_cancelled",
		Actor:         "CancelPayout",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{p.PayeeAccount},
		Details: map[string]any{
			"amount_cents": p.AmountCents,
			"from_status":  fromStatus,
		},
	})
	l.emitOutboxLocked(OutboxEvent{
		Type:        OutboxPayoutCancelled,
		Account:     p.PayeeAccount,
		AmountCents: p.AmountCents,
		Currency:    p.Currency,
		Details:     map[string]any{"payout_id": id},
	})
	return p, nil
}

// PayoutReport is one payout instruction's Reconcile disclosure.
type PayoutReport struct {
	ID              string    `json:"id"`
	PayeeAccount    AccountID `json:"payee_account"`
	AmountCents     int64     `json:"amount_cents"`
	Currency        string    `json:"currency"`
	Destination     string    `json:"destination"`
	Maker           string    `json:"maker"`
	Approver        string    `json:"approver,omitempty"`
	Status          string    `json:"status"`
	CreatedAt       time.Time `json:"created_at"`
	ClearingEntryID string    `json:"clearing_entry_id,omitempty"`
	StatementLineID string    `json:"statement_line_id,omitempty"`
	FailReason      string    `json:"fail_reason,omitempty"`
}

// payoutReportsLocked builds the Reconcile payout disclosure: every
// instruction, sorted by ID. Callers must hold l.mu; the read lock
// suffices.
func (l *Ledger) payoutReportsLocked() []PayoutReport {
	out := make([]PayoutReport, 0, len(l.payouts))
	for id, p := range l.payouts {
		out = append(out, PayoutReport{
			ID:              id,
			PayeeAccount:    p.PayeeAccount,
			AmountCents:     p.AmountCents,
			Currency:        p.Currency,
			Destination:     p.Destination,
			Maker:           p.Maker,
			Approver:        p.Approver,
			Status:          p.Status,
			CreatedAt:       p.CreatedAt,
			ClearingEntryID: p.ClearingEntryID,
			StatementLineID: p.StatementLineID,
			FailReason:      p.FailReason,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// WithPayoutClearingAccount configures the ledger-wide clearing account
// that payout submit legs credit (see SubmitPayout).
func WithPayoutClearingAccount(a AccountID) Option {
	return func(l *Ledger) {
		l.payoutClearingAccount = a
	}
}

// SetPayoutClearingAccount configures (or clears, with the empty
// account) the ledger-wide payout clearing account. Empty means
// unconfigured: SubmitPayout is fail-closed until the operator
// configures it. Structural config — it does not bump the journal
// version and it survives disaster-recovery snapshots, like the
// term-deposit interest-expense account.
func (l *Ledger) SetPayoutClearingAccount(a AccountID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.payoutClearingAccount = a
	l.emitAudit(AuditEvent{
		Op:      "set_payout_clearing_account",
		Actor:   "SetPayoutClearingAccount",
		TraceID: string(a),
		Details: map[string]any{"payout_clearing_account": string(a)},
	})
}

// prunePayoutKeysLocked evicts payout idempotency keys older than the
// TTL, mirroring the entry-level key expiry: after the TTL, reposting a
// key drafts a brand-new instruction. The instructions themselves are
// untouched — only the replay-detection index shrinks. A non-positive
// TTL disables pruning. Callers must hold l.mu (the write lock).
func (l *Ledger) prunePayoutKeysLocked(now time.Time) int {
	if l.idempotencyTTL <= 0 {
		return 0
	}
	cutoff := now.Add(-l.idempotencyTTL)
	removed := 0
	for key, id := range l.payoutKeys {
		if p := l.payouts[id]; p.CreatedAt.Before(cutoff) {
			delete(l.payoutKeys, key)
			removed++
		}
	}
	return removed
}
