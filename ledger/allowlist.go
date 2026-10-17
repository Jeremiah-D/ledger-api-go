package ledger

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// Per-account transfer beneficiary allowlists (LG-113).
//
// An allowlist is an opt-in per-payer gate on the beneficiary of a
// transfer: when a payer has one installed, PostTransfer only books the
// transfer when the payee is on the payer's list. The payment-domain
// shape is merchant payout control — a marketplace's settlement account
// pays out only to its registered sub-merchants, so a compromised
// payout service cannot exfiltrate funds to an attacker's account —
// layered as an opt-in control alongside the existing frozen,
// sanctions/denylist, overdraft, and daily-limit gates, and orthogonal
// to the denylist: the denylist screens the accounts money may never
// move through in either direction (compliance), the allowlist limits
// who one payer may pay (risk control).
//
// The gate runs inside PostTransfer's check order between the frozen
// gate and the overdraft gate (see postTransferLocked and
// postTransferFXLocked): field validation, the idempotency replay check,
// the ID-conflict checks, the frozen checks, then the allowlist, then
// overdraft — the same position as the frozen gate, so an idempotent
// replay still returns the original receipt (the replay books nothing
// new), and the frozen gate always runs first (an allowlist never
// un-freezes a stopped payer). Only the principal leg's payee is
// screened: the fee leg's recipient is the operator's own revenue
// account, not a business beneficiary, so it is exempt — the same
// carve-out the sanctions/denylist screening makes for the operator's
// accounts. Split transfers (see PostTransferSplit) screen the same
// way, because every chunk posts through postTransferLocked.
//
// Like every risk control, a rejection records nothing: no journal
// rows, no chain links, no version bump — and no audit event, mirroring
// the frozen and overdraft gates (the trail records posted transfers,
// not refused ones; the denylist's denylist_rejected event is the
// deliberate exception, because sanctions screening has a legal duty
// to show attempts).
//
// Allowlist config is structural: setting, replacing, or clearing a
// list does not bump the ledger version, and every change is recorded
// in the structured audit log (LG-29) as "allowlist_set" (with the
// account, the beneficiaries, and the previous list) or
// "allowlist_cleared". An empty set disables the gate for the payer:
// SetTransferAllowlist with no beneficiaries clears, and a payer with
// no list installed accepts transfers to any payee, byte for byte as
// before allowlists existed. Lists round-trip through the
// disaster-recovery snapshot config record like the frozen-account
// list — a restore that dropped a list would silently re-enable
// payouts to unlisted beneficiaries.

// ErrBeneficiaryNotAllowed is returned by PostTransfer (and its
// variants) when the payer has a transfer allowlist installed and the
// payee is not on it. It is a 403-class rejection, like
// ErrAccountFrozen: the request is well-formed, the risk control
// refuses it.
var ErrBeneficiaryNotAllowed = errors.New("ledger: transfer beneficiary is not on the payer's transfer allowlist")

// ErrInvalidTransferAllowlist is returned by SetTransferAllowlist when
// the payer account is empty or a beneficiary names an empty account —
// an allowlist entry that could never be screened against is an
// operator mistake.
var ErrInvalidTransferAllowlist = errors.New("ledger: transfer allowlist must name a non-empty payer account and non-empty beneficiary accounts")

// TransferAllowlistRow is one installed allowlist, the unit the
// snapshot config record serializes (see snapshot.go).
type TransferAllowlistRow struct {
	// Account is the payer the list gates.
	Account AccountID `json:"account"`
	// Beneficiaries is the sorted list of payees the payer may pay.
	Beneficiaries []AccountID `json:"beneficiaries"`
}

// SetTransferAllowlist installs (or replaces) the transfer beneficiary
// allowlist for one payer account: while it is installed, PostTransfer
// only books transfers whose payee is on the list. An empty
// beneficiaries list disables the gate — it is equivalent to
// ClearTransferAllowlist.
//
// Setting a list is structural: it does not bump the journal version
// (no balances change) and it is visible to audit tooling — the change
// is recorded as "allowlist_set" with the account, the new
// beneficiaries, and the previous list, and the gate is part of the
// disaster-recovery snapshot config. Installing the payer's current
// list again is a no-op — it returns nil without writing an audit
// event, so idempotent retries stay silent in the compliance trail.
func (l *Ledger) SetTransferAllowlist(account AccountID, beneficiaries []AccountID) error {
	return l.SetTransferAllowlistCtx(context.Background(), account, beneficiaries)
}

// SetTransferAllowlistCtx is SetTransferAllowlist with per-call audit
// actor attribution (LG-73): the audit event records the operator
// carried by ctx when set, and the entrypoint name otherwise.
func (l *Ledger) SetTransferAllowlistCtx(ctx context.Context, account AccountID, beneficiaries []AccountID) error {
	if account == "" {
		return ErrInvalidTransferAllowlist
	}
	set := make(map[AccountID]bool, len(beneficiaries))
	for _, b := range beneficiaries {
		if b == "" {
			return ErrInvalidTransferAllowlist
		}
		set[b] = true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(set) == 0 {
		// An empty set disables the gate: clearing is the honest
		// representation, so Set stays the inverse of Get — a payer
		// with no beneficiaries installed is indistinguishable from
		// a payer that never had a list.
		return l.clearTransferAllowlistLocked(ctx, account)
	}
	old, had := l.transferAllowlist[account]
	if had && allowlistSetsEqual(old, set) {
		// Idempotent replay of the current list: nothing changed,
		// so no audit event — the trail records changes, not retries.
		return nil
	}
	l.transferAllowlist[account] = set
	details := map[string]any{
		"account":       string(account),
		"beneficiaries": allowlistSortedList(set),
	}
	if had {
		details["previous_beneficiaries"] = allowlistSortedList(old)
	}
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "allowlist_set",
		Actor:         "SetTransferAllowlist",
		TraceID:       string(account),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{account},
		Details:       details,
	})
	return nil
}

// ClearTransferAllowlist disables the transfer beneficiary allowlist
// gate for one payer account: its transfers may again pay any payee.
// Clearing an account that has no list installed is a silent no-op
// (nil, no audit event), so teardown paths stay idempotent. Like
// SetTransferAllowlist, it is structural and does not bump the journal
// version; a real removal is recorded as "allowlist_cleared".
func (l *Ledger) ClearTransferAllowlist(account AccountID) error {
	return l.ClearTransferAllowlistCtx(context.Background(), account)
}

// ClearTransferAllowlistCtx is ClearTransferAllowlist with per-call
// audit actor attribution.
func (l *Ledger) ClearTransferAllowlistCtx(ctx context.Context, account AccountID) error {
	if account == "" {
		return ErrInvalidTransferAllowlist
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.clearTransferAllowlistLocked(ctx, account)
}

// clearTransferAllowlistLocked removes the payer's list, auditing the
// removal only when a list was actually installed. Callers must hold
// l.mu.
func (l *Ledger) clearTransferAllowlistLocked(ctx context.Context, account AccountID) error {
	old, had := l.transferAllowlist[account]
	if !had {
		return nil
	}
	delete(l.transferAllowlist, account)
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "allowlist_cleared",
		Actor:         "ClearTransferAllowlist",
		TraceID:       string(account),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{account},
		Details: map[string]any{
			"account":                string(account),
			"previous_beneficiaries": allowlistSortedList(old),
		},
	})
	return nil
}

// TransferAllowlistOf reports the installed transfer beneficiary
// allowlist for one payer account, sorted, and whether the gate is
// enabled for the payer at all. A payer with no list installed has the
// gate disabled: its transfers may pay any payee.
func (l *Ledger) TransferAllowlistOf(account AccountID) ([]AccountID, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	set, ok := l.transferAllowlist[account]
	if !ok {
		return nil, false
	}
	return allowlistSortedList(set), true
}

// allowlistRejectedLocked reports whether a transfer from payer to payee
// is refused by the payer's installed transfer allowlist: the gate is
// disabled when the payer has no list, and otherwise the payee must be
// on the list. Callers must hold l.mu (the write lock on the transfer
// write paths, where the check runs).
func (l *Ledger) allowlistRejectedLocked(payer, payee AccountID) bool {
	set, ok := l.transferAllowlist[payer]
	return ok && !set[payee]
}

// allowlistSortedList renders a beneficiary set as a sorted slice, for
// the getter, the audit events, and the snapshot. Deterministic for
// operators and tests.
func allowlistSortedList(set map[AccountID]bool) []AccountID {
	out := make([]AccountID, 0, len(set))
	for b := range set {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// allowlistSetsEqual reports whether two beneficiary sets hold the same
// accounts.
func allowlistSetsEqual(a, b map[AccountID]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for x := range a {
		if !b[x] {
			return false
		}
	}
	return true
}

// allowlistSnapshotLocked returns every installed transfer allowlist,
// sorted by payer account, for the snapshot config record. Callers must
// hold l.mu (either lock suffices; nothing is mutated).
func (l *Ledger) allowlistSnapshotLocked() []TransferAllowlistRow {
	out := make([]TransferAllowlistRow, 0, len(l.transferAllowlist))
	for account, set := range l.transferAllowlist {
		out = append(out, TransferAllowlistRow{Account: account, Beneficiaries: allowlistSortedList(set)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Account < out[j].Account })
	return out
}

// allowlistRejectionLocked builds the ErrBeneficiaryNotAllowed error for
// a screened transfer: the request is well-formed, the risk control
// refuses it. Callers must hold the write lock. Like the frozen and
// overdraft rejections, the refusal emits no audit event — the trail
// records posted transfers, not refused ones.
func (l *Ledger) allowlistRejectionLocked(payer, payee AccountID) error {
	return fmt.Errorf("%w: payer %q is not allowlisted to pay %q", ErrBeneficiaryNotAllowed, payer, payee)
}
