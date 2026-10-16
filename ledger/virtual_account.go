package ledger

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
)

// Validation errors for virtual accounts (see RegisterVirtualAccount).
var (
	// ErrVANEmptyParent is returned by RegisterVirtualAccount when the
	// parent account is empty: a virtual account must attribute to a
	// real account.
	ErrVANEmptyParent = errors.New("ledger: virtual account parent must not be empty")
	// ErrVANSelfParent is returned by RegisterVirtualAccount when the
	// VAN is registered to itself.
	ErrVANSelfParent = errors.New("ledger: virtual account cannot be its own parent")
	// ErrVANParentIsVAN is returned by RegisterVirtualAccount when the
	// proposed parent is itself a registered virtual account:
	// attribution is exactly one hop, so chains are rejected at
	// registration instead of silently misrouting.
	ErrVANParentIsVAN = errors.New("ledger: virtual account parent must not itself be a virtual account")
	// ErrVANHasAttributedChildren is returned by RegisterVirtualAccount
	// when the proposed VAN already parents another registered virtual
	// account: allowing it would chain that child's attribution through
	// a zero-balance account.
	ErrVANHasAttributedChildren = errors.New("ledger: account already parents a virtual account and cannot itself become one")
	// ErrVANNoDebit is returned when a posting names a registered
	// virtual account on the paying leg: a virtual account never holds
	// a balance — receipts attribute to its parent, so there is nothing
	// to pay from.
	ErrVANNoDebit = errors.New("ledger: virtual account holds no balance and cannot pay")
)

// VirtualAccount is one virtual-account-number to parent-account
// mapping row: receipts naming the VAN attribute to the parent.
type VirtualAccount struct {
	VAN    AccountID `json:"van"`
	Parent AccountID `json:"parent"`
}

// RegisterVirtualAccount maps van to parent: every receipt that would
// credit van (the receiving leg in business terms) is booked to parent
// instead, with the journal row marked "van-attribution:<van>" and a
// virtual_account_attribution audit event. The VAN itself never holds a
// balance — a posting that would debit it (pay from it) is rejected
// with ErrVANNoDebit.
//
// The mapping is structural, not bookkeeping: like Freeze, it does not
// bump the ledger version. Every change bumps the mapping's own version
// counter (see VirtualAccountVersion), re-registering an identical
// mapping is an idempotent no-op, and the table round-trips through
// snapshots and is disclosed by Reconcile.
//
// Validation, the minimal safe set (see hierarchy.go's SetParent for
// the convention): the VAN and the parent must be non-empty, must
// differ, and attribution stays exactly one hop — the parent must not
// itself be a registered VAN, and the VAN must not already parent
// another VAN.
func (l *Ledger) RegisterVirtualAccount(van, parent AccountID) error {
	if van == "" {
		return ErrEmptyAccountID
	}
	if parent == "" {
		return ErrVANEmptyParent
	}
	if van == parent {
		return ErrVANSelfParent
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if _, isVAN := l.virtualAccounts[parent]; isVAN {
		return fmt.Errorf("%w: parent %q", ErrVANParentIsVAN, parent)
	}
	for v, p := range l.virtualAccounts {
		if p == van {
			return fmt.Errorf("%w: %q already parents VAN %q", ErrVANHasAttributedChildren, van, v)
		}
	}
	if cur, ok := l.virtualAccounts[van]; ok && cur == parent {
		return nil // idempotent: identical mapping, no version bump
	}
	l.virtualAccounts[van] = parent
	l.virtualAccountVersion++
	l.emitAudit(AuditEvent{
		Op:            "virtual_account_registered",
		Actor:         "RegisterVirtualAccount",
		TraceID:       string(van),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{van, parent},
		Details: map[string]any{
			"van":     string(van),
			"parent":  string(parent),
			"version": l.virtualAccountVersion,
		},
	})
	return nil
}

// UnregisterVirtualAccount removes van's mapping: new receipts naming
// it book to it directly again (and, with no balance behind it, behave
// like any other account). Unregistering an unknown VAN is an
// idempotent no-op, like Unfreeze. Every actual removal bumps the
// mapping version counter.
func (l *Ledger) UnregisterVirtualAccount(van AccountID) error {
	if van == "" {
		return ErrEmptyAccountID
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	parent, ok := l.virtualAccounts[van]
	if !ok {
		return nil
	}
	delete(l.virtualAccounts, van)
	l.virtualAccountVersion++
	l.emitAudit(AuditEvent{
		Op:            "virtual_account_unregistered",
		Actor:         "UnregisterVirtualAccount",
		TraceID:       string(van),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{van, parent},
		Details: map[string]any{
			"van":     string(van),
			"parent":  string(parent),
			"version": l.virtualAccountVersion,
		},
	})
	return nil
}

// VirtualAccountParent returns the parent account van attributes to, or
// false when van is not a registered virtual account.
func (l *Ledger) VirtualAccountParent(van AccountID) (AccountID, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	p, ok := l.virtualAccounts[van]
	return p, ok
}

// ListVirtualAccounts returns every van -> parent mapping, sorted by
// VAN.
func (l *Ledger) ListVirtualAccounts() []VirtualAccount {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.virtualAccountsLocked()
}

// VirtualAccountVersion is the mapping mutation counter: every
// registration, re-registration to a different parent, and removal bumps
// it. Idempotent no-ops do not.
func (l *Ledger) VirtualAccountVersion() uint64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.virtualAccountVersion
}

// VANAttributionCount reports how many virtual-account attributions were
// applied since process start. Operational metric (surfaced on GET
// /metrics), not books: in-memory only, no snapshot round-trip.
func (l *Ledger) VANAttributionCount() uint64 {
	return l.vanAttributions.Load()
}

// virtualAccountsLocked returns the mappings sorted by VAN. Callers must
// hold l.mu (either lock suffices).
func (l *Ledger) virtualAccountsLocked() []VirtualAccount {
	out := make([]VirtualAccount, 0, len(l.virtualAccounts))
	for van, parent := range l.virtualAccounts {
		out = append(out, VirtualAccount{VAN: van, Parent: parent})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].VAN < out[j].VAN })
	return out
}

// vanAttribution describes one applied virtual-account attribution: a
// receipt that named VAN on its receiving leg was booked to Parent
// instead.
type vanAttribution struct {
	VAN         AccountID
	Parent      AccountID
	AmountCents int64
	Currency    string
	EntryID     string
}

// vanDebitRejectedLocked reports ErrVANNoDebit when acct is a
// registered virtual account: the paying leg of a posting. Callers must
// hold the write lock.
func (l *Ledger) vanDebitRejectedLocked(acct AccountID) error {
	if _, isVAN := l.virtualAccounts[acct]; isVAN {
		return fmt.Errorf("%w: %q", ErrVANNoDebit, acct)
	}
	return nil
}

// vanParentLocked returns the parent acct attributes to when it is a
// registered virtual account. Callers must hold l.mu (either lock
// suffices).
func (l *Ledger) vanParentLocked(acct AccountID) (AccountID, bool) {
	p, ok := l.virtualAccounts[acct]
	return p, ok
}

// vanAttributionMemo builds the journal memo marker for an attributed
// leg: "van-attribution:<van>", following the LG-47 "rule:<id>" and
// LG-87 "dividend:<bps>" marker conventions. An existing caller memo is
// preserved in front, so business notes survive attribution.
func vanAttributionMemo(memo string, van AccountID) string {
	marker := "van-attribution:" + string(van)
	if memo == "" {
		return marker
	}
	return memo + ";" + marker
}

// resolveVANEntryLocked applies virtual-account attribution to one
// journal entry, in place: a VAN on the paying (credit) leg is rejected
// with ErrVANNoDebit — a virtual account never holds a balance to pay
// from; a VAN on the receiving (debit) leg is rewritten to its parent,
// the journal row marked "van-attribution:<van>". It returns the
// attribution record, or nil when neither leg named a VAN.
//
// The check runs after the idempotency replay check on every posting
// path: a replay books nothing new, so it returns the original entry
// instead of failing on a VAN registered after the original posting.
// Callers must hold the write lock.
func (l *Ledger) resolveVANEntryLocked(e *JournalEntry) (*vanAttribution, error) {
	if err := l.vanDebitRejectedLocked(e.CreditAccount); err != nil {
		return nil, err
	}
	parent, isVAN := l.vanParentLocked(e.DebitAccount)
	if !isVAN {
		return nil, nil
	}
	attr := &vanAttribution{
		VAN:         e.DebitAccount,
		Parent:      parent,
		AmountCents: e.AmountCents,
		Currency:    e.Currency,
		EntryID:     e.ID,
	}
	e.DebitAccount = parent
	memo := vanAttributionMemo(e.Memo, attr.VAN)
	if err := checkMemoLength(memo); err != nil {
		return nil, err
	}
	e.Memo = memo
	return attr, nil
}

// attributeVANRecipientLocked rewrites one receiving account through
// the VAN registry, returning the resolved account and the attribution
// record (nil when the account is not a VAN). Callers must hold the
// write lock.
func (l *Ledger) attributeVANRecipientLocked(acct AccountID, amountCents int64, currency, entryID string) (AccountID, *vanAttribution) {
	parent, ok := l.vanParentLocked(acct)
	if !ok {
		return acct, nil
	}
	return parent, &vanAttribution{
		VAN:         acct,
		Parent:      parent,
		AmountCents: amountCents,
		Currency:    currency,
		EntryID:     entryID,
	}
}

// resolveTransferVANsLocked rewrites a transfer's receiving accounts
// through the VAN registry: the payer (From) must not be a VAN
// (ErrVANNoDebit — a virtual account never holds a balance to pay
// from); the payee (To), the single fee account, and every fee-split
// recipient attribute to their parents when they are VANs. It returns
// the rewritten transfer, the rewritten fee account, the rewritten
// split recipients, and one attribution record per rewritten account.
//
// Callers must invoke it after the idempotency replay check and before
// every account risk gate, so frozen/overdraft/daily-limit checks see
// the real (parent) accounts. The journal memo marker is applied
// separately by annotateVANMemos once the entries are built, because
// fee-split legs carry a machine-readable memo format that the marker
// must not corrupt (see below). Callers must hold the write lock.
func (l *Ledger) resolveTransferVANsLocked(t Transfer, feeCents int64, feeAccount AccountID, splits []FeeSplitShare) (Transfer, AccountID, []FeeSplitShare, []*vanAttribution, error) {
	if err := l.vanDebitRejectedLocked(t.From); err != nil {
		return t, feeAccount, splits, nil, err
	}
	var attrs []*vanAttribution
	var a *vanAttribution
	if t.To, a = l.attributeVANRecipientLocked(t.To, t.AmountCents, t.Currency, t.ID); a != nil {
		attrs = append(attrs, a)
	}
	if feeAccount != "" {
		if feeAccount, a = l.attributeVANRecipientLocked(feeAccount, feeCents, t.Currency, t.ID+"/fee"); a != nil {
			attrs = append(attrs, a)
		}
	}
	for i := range splits {
		if splits[i].Account, a = l.attributeVANRecipientLocked(splits[i].Account, splits[i].FeeCents, t.Currency, t.ID+"/fee/"+strconv.Itoa(i)); a != nil {
			attrs = append(attrs, a)
		}
	}
	return t, feeAccount, splits, attrs, nil
}

// annotateVANMemos appends the "van-attribution:<van>" marker to the
// journal entries named by attrs. Fee-split legs are skipped: their memo
// is a machine-readable "fee-split:<i>/<n>:<account>:<bps>" record owned
// by parseFeeSplitMemo, and any suffix would corrupt the bps parse —
// their attribution is disclosed by the audit event instead. A marker
// that would overflow the journal memo limit fails closed with
// ErrMemoTooLong rather than silently truncating the caller's note.
func annotateVANMemos(entries []JournalEntry, attrs []*vanAttribution) error {
	byID := make(map[string]*vanAttribution, len(attrs))
	for _, a := range attrs {
		byID[a.EntryID] = a
	}
	for i := range entries {
		a, ok := byID[entries[i].ID]
		if !ok {
			continue
		}
		if _, _, _, _, isSplit := parseFeeSplitMemo(entries[i].Memo); isSplit {
			continue
		}
		memo := vanAttributionMemo(entries[i].Memo, a.VAN)
		if err := checkMemoLength(memo); err != nil {
			return err
		}
		entries[i].Memo = memo
	}
	return nil
}

// emitVANAttributionsLocked emits one virtual_account_attribution audit
// event per attribution, after the posting's own audit event. Callers
// must hold l.mu (either lock suffices).
func (l *Ledger) emitVANAttributionsLocked(attrs []*vanAttribution, traceID, actor string) {
	for _, a := range attrs {
		l.emitVANAttributionAuditLocked(a, traceID, actor)
	}
}

// emitVANAttributionAuditLocked emits the virtual_account_attribution
// audit event for one applied attribution: which VAN attributed to
// which parent, the amount, and the journal entry that booked it. The
// ledger version does not move — attribution is aliasing, not
// bookkeeping — so both brackets name the post-commit version.
// Callers must hold l.mu (either lock suffices).
func (l *Ledger) emitVANAttributionAuditLocked(a *vanAttribution, traceID, actor string) {
	l.emitAudit(AuditEvent{
		Op:            "virtual_account_attribution",
		Actor:         actor,
		TraceID:       traceID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		EntryIDs:      []string{a.EntryID},
		Accounts:      []AccountID{a.VAN, a.Parent},
		Details: map[string]any{
			"van":          string(a.VAN),
			"parent":       string(a.Parent),
			"amount_cents": a.AmountCents,
			"currency":     a.Currency,
		},
	})
	l.vanAttributions.Add(1)
}
