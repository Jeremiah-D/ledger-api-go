package ledger

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"
)

// Partial captures (LG-115): settling an authorization hold in pieces.
//
// A full Capture (see hold.go) consumes the hold in one shot: one
// capture, one journal entry, the hold moves to captured. E-commerce and
// hotel pre-authorizations settle differently — a $500 hold settles as a
// $320 room charge on day 3 and a $95 minibar charge on day 5, each a
// real money movement the moment it posts. CapturePartial is that flow:
// the n-th partial capture of a hold settles amountCents as an ordinary
// double-entry posting (debit the payee, credit the held account, in the
// hold's currency — partial captures never cross currencies), and the
// hold stays active with its remaining cents still reserving available
// funds until the cumulative captures reach the original hold amount.
//
// The (holdID, captureSeq) pair is the operation's identity: captureSeq
// is caller-allocated (1-based, need not be consecutive) and the journal
// entry ID is the deterministic "<holdID>/capture/<seq>", so the same
// logical partial capture can never book twice — a replay returns the
// original receipt with Duplicate == true and books nothing new, and a
// different amount under the same (holdID, seq) is a replay, not a new
// capture. The idempotency namespace is partialCaptureKeys, independent
// of the full-capture captureKeys: "h1/capture/1" as a full-capture ID
// and the first partial capture of hold h1 are different keys, and the
// entry-ID conflict check (see ErrCaptureIDConflict) keeps the journal
// rows from colliding either way.
//
// Each partial capture runs the same risk checks as a full Capture, in
// the same order — field validation, hold lookup, denylist screening,
// idempotency replay, hold state, frozen, overdraft, budget, period —
// with the capture bound checked against the hold's *remaining* cents
// (original amount minus settled partials), computed overflow-safe:
// amount > remaining is rejected with ErrCaptureExceedsHold. Partial
// captures journal real postings, so each one bumps the ledger version
// and appends one audit-chain link, exactly like Post.
//
// Interplay with the full Capture: Capture's bound is now also the
// remaining cents (see hold.go), so a full capture after partials can
// only settle what is left. A partial capture on a captured, released,
// or expired hold is rejected with ErrHoldNotActive / ErrHoldExpired,
// exactly like a second full capture.

// ErrInvalidCaptureSeq is returned by CapturePartial when captureSeq is
// below 1: sequences are caller-allocated 1-based identifiers for the
// hold's partial captures.
var ErrInvalidCaptureSeq = errors.New("ledger: partial capture sequence must be >= 1")

// PartialCapture is one partial settlement of an authorization hold.
// HoldID names the hold, To is the payee, CaptureSeq is the
// caller-allocated 1-based sequence number of this partial capture, and
// AmountCents is the settled amount. ExternalRef, Attachments, and
// Metadata ride on the settlement journal entry, like in Capture.
type PartialCapture struct {
	HoldID         string
	To             AccountID
	CaptureSeq     int
	AmountCents    int64
	ExternalRef    string
	Attachments    []string
	Metadata       map[string]string
	CreatedAt      time.Time
}

// PartialCaptureReceipt reports what CapturePartial committed.
//
// CaptureID is the deterministic journal entry ID
// "<holdID>/capture/<seq>". CapturedCents is this leg's amount;
// TotalCapturedCents is the cumulative settled amount on the hold and
// RemainingCents is what the hold still reserves. Duplicate replays
// return the original receipt with Duplicate == true and book nothing
// new.
type PartialCaptureReceipt struct {
	CaptureID          string       `json:"capture_id"`
	HoldID             string       `json:"hold_id"`
	CaptureSeq         int          `json:"capture_seq"`
	Entry              JournalEntry `json:"entry"`
	CapturedCents      int64        `json:"captured_cents"`
	TotalCapturedCents int64        `json:"total_captured_cents"`
	RemainingCents     int64        `json:"remaining_cents"`
	Duplicate          bool         `json:"duplicate"`
}

// partialCaptureEntryID is the deterministic journal entry ID of the
// n-th partial capture of a hold: "<holdID>/capture/<seq>". It doubles
// as the idempotency key in the partial-capture namespace.
func partialCaptureEntryID(holdID string, seq int) string {
	return holdID + "/capture/" + strconv.Itoa(seq)
}

// CapturePartial settles part of an authorization hold as a real money
// movement and keeps the hold active for further partial captures.
//
// The risk checks mirror Capture and run in the same order: field
// validation first, then the hold lookup, then the denylist screen on
// both settlement legs (screening runs before the replay check, so a
// denied capture leaves no idempotency trace), then the idempotency
// replay check on the (holdID, captureSeq) namespace (a replay returns
// the original receipt with Duplicate == true and books nothing new),
// then the hold state checks (unknown -> ErrHoldNotFound,
// captured/released -> ErrHoldNotActive, expired -> ErrHoldExpired,
// amount above the hold's remaining cents -> ErrCaptureExceedsHold),
// then the capture-ID conflict check, then the frozen check on both
// settlement legs, then the overdraft check on the held (payer)
// account, then the monthly budget and the period gate.
//
// The commit is atomic under the write lock: the settlement journal
// entry (DebitAccount = To, CreditAccount = the hold's account, in the
// hold's currency), the hold's CapturedCents bump, and the partial
// idempotency index entry land together. When the cumulative captures
// reach the original hold amount the hold moves to captured; otherwise
// it stays active and its remaining cents keep reserving available
// funds. Like Capture, a partial capture journals a real posting, so it
// bumps the ledger version and appends one audit-chain link.
func (l *Ledger) CapturePartial(pc PartialCapture) (PartialCaptureReceipt, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if pc.HoldID == "" {
		return PartialCaptureReceipt{}, ErrEmptyHoldID
	}
	if pc.To == "" {
		return PartialCaptureReceipt{}, ErrEmptyCaptureToAccount
	}
	if pc.CaptureSeq < 1 {
		return PartialCaptureReceipt{}, ErrInvalidCaptureSeq
	}
	if pc.AmountCents <= 0 {
		return PartialCaptureReceipt{}, ErrCaptureNonPositiveAmount
	}
	// The external reference, attachments, and metadata are field
	// validation, like in Capture: rejected before anything is
	// recorded.
	if err := checkExternalRefLength(pc.ExternalRef); err != nil {
		return PartialCaptureReceipt{}, err
	}
	if err := checkAttachments(pc.Attachments); err != nil {
		return PartialCaptureReceipt{}, err
	}
	if err := checkMetadata(pc.Metadata); err != nil {
		return PartialCaptureReceipt{}, err
	}

	// The hold is looked up before the idempotency replay check: the
	// sanctions/denylist screen below needs the hold's account (the
	// payer leg), and screening runs before replay by design — a denied
	// capture leaves no idempotency trace (see ErrAccountDenied). Holds
	// are never deleted, so a genuine replay's hold is always found and
	// the replay check below still returns its receipt.
	h, ok := l.holds[pc.HoldID]
	if !ok {
		return PartialCaptureReceipt{}, ErrHoldNotFound
	}

	// Sanctions/denylist screening (see denylist.go): the hold's account
	// (payer) and the capture target (payee) are screened before the
	// idempotency replay check — a hit rejects the capture and the
	// attempt is audited as "denylist_rejected".
	if acct, entry, denied := l.denylistRejectedLocked(h.Account, pc.To); denied {
		return PartialCaptureReceipt{}, l.deniedRejectionLocked("CapturePartial", pc.HoldID, acct, entry, []AccountID{h.Account, pc.To})
	}

	// The (holdID, captureSeq) identity: the deterministic entry ID is
	// the idempotency key, so a retried partial capture replays the
	// original receipt even though the caller never minted a key.
	entryID := partialCaptureEntryID(pc.HoldID, pc.CaptureSeq)
	if r, ok := l.partialCaptureKeys[entryID]; ok {
		r.Duplicate = true
		return r, nil
	}

	now := time.Now()
	if h.Status == HoldStatusCaptured || h.Status == HoldStatusReleased {
		return PartialCaptureReceipt{}, ErrHoldNotActive
	}
	if !now.Before(h.ExpiresAt) {
		return PartialCaptureReceipt{}, ErrHoldExpired
	}
	if pc.To == h.Account {
		return PartialCaptureReceipt{}, ErrCaptureSameAccount
	}
	// The capture bound is the hold's remaining cents, computed
	// overflow-safe: CapturedCents never exceeds AmountCents (every
	// commit enforces it), so the difference is non-negative and exact.
	remaining := heldRemainingLocked(h)
	if pc.AmountCents > remaining {
		return PartialCaptureReceipt{}, ErrCaptureExceedsHold
	}
	if _, exists := l.entries[entryID]; exists {
		return PartialCaptureReceipt{}, ErrCaptureIDConflict
	}
	// External-reference uniqueness, like in Capture: after the
	// idempotency replay check (a replay returns the original receipt
	// without consulting the reference index), before anything books.
	if pc.ExternalRef != "" {
		if owner, taken := l.byRef[pc.ExternalRef]; taken {
			return PartialCaptureReceipt{}, fmt.Errorf("%w: reference %q already journaled on entry %q", ErrDuplicateExternalRef, pc.ExternalRef, owner)
		}
	}

	if l.closedLocked(h.Account) || l.closedLocked(pc.To) {
		return PartialCaptureReceipt{}, ErrAccountClosed
	}
	if l.frozenLocked(h.Account) || l.frozenLocked(pc.To) {
		return PartialCaptureReceipt{}, ErrAccountFrozen
	}

	entry := JournalEntry{
		ID:            entryID,
		DebitAccount:  pc.To,
		CreditAccount: h.Account,
		AmountCents:   pc.AmountCents,
		Currency:      h.Currency,
		ExternalRef:   pc.ExternalRef,
		Attachments:   pc.Attachments,
		Metadata:      pc.Metadata,
		CreatedAt:     pc.CreatedAt,
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = now
	}
	// Overdraft protection guards the payer's settlement posting,
	// exactly like Capture and Post: the comparison never subtracts,
	// so it cannot overflow (see overdraft.go).
	if err := l.overdraftRejectedLocked(entry); err != nil {
		return PartialCaptureReceipt{}, err
	}

	// Monthly spend budgets guard the hold account's settlement spend:
	// a partial capture that would take the account's cumulative spend
	// for the UTC calendar month above its configured budget is
	// rejected with ErrBudgetExceeded. Runs after overdraft and before
	// the period gate — a replay books nothing new, so it returned
	// above.
	if l.budgetRejectedLocked(h.Account, h.Currency, pc.AmountCents, entry.CreatedAt) {
		return PartialCaptureReceipt{}, ErrBudgetExceeded
	}

	// The period gate is keyed on the capture entry's final timestamp
	// (see period.go): a partial capture posting into a closed
	// accounting period is rejected with ErrPeriodClosed. It runs after
	// the idempotency replay check above, so replaying a partial
	// capture posted before the period closed returns the original
	// receipt.
	if err := l.periodRejectedLocked(entry.CreatedAt); err != nil {
		return PartialCaptureReceipt{}, err
	}

	l.maybePruneIdempotencyKeys(now)
	l.maybePruneMonthlySpendLocked(now)
	versionBefore := l.version
	// Pre-commit balance snapshot for the opt-in balance-change hook
	// (see balance_hook.go): nil when no hook is registered.
	hookTouched := touchedAccounts([]JournalEntry{entry})
	hookOld := l.balanceSnapshotLocked(hookTouched)
	l.commitEntryLocked(entry)
	l.addMonthlySpendLocked(h.Account, h.Currency, pc.AmountCents, entry.CreatedAt)
	// The cumulative capture can never exceed the original hold: the
	// bound above ran against the pre-commit remaining, and the whole
	// check-and-commit is under the write lock.
	h.CapturedCents += pc.AmountCents
	if h.CapturedCents == h.AmountCents {
		// The hold is fully settled: it moves to captured and stops
		// reserving available funds, like a full Capture.
		h.Status = HoldStatusCaptured
	}
	l.holds[pc.HoldID] = h

	receipt := PartialCaptureReceipt{
		CaptureID:          entryID,
		HoldID:             pc.HoldID,
		CaptureSeq:         pc.CaptureSeq,
		Entry:              entry,
		CapturedCents:      pc.AmountCents,
		TotalCapturedCents: h.CapturedCents,
		RemainingCents:     heldRemainingLocked(h),
	}
	l.partialCaptureKeys[entryID] = receipt
	// Low-balance alert evaluation: strictly after the atomic commit
	// zone, read-only (see low_balance.go). Advisory only.
	l.evaluateBalanceWatchesLocked(touchedAccounts([]JournalEntry{entry}), entryID, "CapturePartial")
	l.emitAudit(AuditEvent{
		Op:            "hold_capture_partial",
		Actor:         "CapturePartial",
		TraceID:       pc.HoldID,
		VersionBefore: versionBefore,
		VersionAfter:  l.version,
		EntryIDs:      []string{entry.ID},
		Accounts:      []AccountID{h.Account, pc.To},
		Details: map[string]any{
			"capture_seq":          pc.CaptureSeq,
			"captured_cents":       entry.AmountCents,
			"total_captured_cents": receipt.TotalCapturedCents,
			"remaining_cents":      receipt.RemainingCents,
			"currency":             h.Currency,
		},
	})
	// Domain outbox: the partial capture settles money — one
	// entry_posted for the settlement leg, then the hold_captured
	// envelope marked partial so downstream consumers can tell a
	// still-active hold from a consumed one.
	l.emitOutboxEntriesLocked([]JournalEntry{entry})
	l.emitOutboxLocked(OutboxEvent{
		Type:        OutboxHoldCaptured,
		Account:     h.Account,
		EntryID:     entry.ID,
		AmountCents: entry.AmountCents,
		Currency:    h.Currency,
		Details: map[string]any{
			"hold_id":              pc.HoldID,
			"to":                   string(pc.To),
			"partial":              true,
			"capture_seq":          pc.CaptureSeq,
			"captured_cents":       entry.AmountCents,
			"total_captured_cents": receipt.TotalCapturedCents,
			"remaining_cents":      receipt.RemainingCents,
		},
	})
	// Balance-change notification: strictly after the atomic commit
	// zone, advisory only — async dispatch, hook failures isolated.
	l.fireBalanceHooksLocked(hookTouched, hookOld, pc.HoldID)

	return receipt, nil
}

// partialCaptureSummaryLocked builds the Reconcile report's
// partial_captures section: one row per hold with at least one partial
// capture, sorted by hold ID. The per-hold capture count is derived from
// the partial-capture idempotency registry (keys are the deterministic
// "<holdID>/capture/<seq>" entry IDs). Callers must hold l.mu; the read
// lock suffices because the scan mutates nothing.
func (l *Ledger) partialCaptureSummaryLocked() []PartialCaptureSummary {
	capturesByHold := make(map[string]int)
	for _, r := range l.partialCaptureKeys {
		capturesByHold[r.HoldID]++
	}
	out := make([]PartialCaptureSummary, 0)
	for id, h := range l.holds {
		n, ok := capturesByHold[id]
		if !ok && h.CapturedCents == 0 {
			continue
		}
		out = append(out, PartialCaptureSummary{
			HoldID:         id,
			Account:        h.Account,
			Currency:       h.Currency,
			HeldCents:      h.AmountCents,
			CapturedCents:  h.CapturedCents,
			RemainingCents: heldRemainingLocked(h),
			Captures:       n,
			Status:         h.Status,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].HoldID < out[j].HoldID })
	return out
}
