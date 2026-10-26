package ledger

import (
	"context"
	"errors"
	"sort"
	"time"
)

// Account-level partial freeze (LG-224): freeze a specified amount
// of an account's funds while the rest keeps moving.
//
// Freeze (freeze.go) is all-or-nothing: a frozen account rejects
// every new posting through it. A partial freeze is the amount-level
// counterpart — FreezeAmount reserves AmountCents of the account's
// available funds, and the remainder stays fully usable for Post and
// PostTransfer. The two are orthogonal: Freeze stops the account,
// FreezeAmount stops only the frozen cents.
//
// Semantics, deliberately following the Hold (LG-20) pattern rather
// than inventing a new reservation shape:
//   - A partial freeze reserves funds off-journal: it moves no money,
//     bumps no ledger version, and appends no chain link. It does
//     change the account's available balance (see Available): the
//     frozen amount is deducted from Available exactly once, as its
//     own term — balance minus active holds minus pending-review
//     freezes minus term-deposit principals minus the partially
//     frozen amount. It is NOT also recorded as a Hold, so the same
//     reservation is never deducted twice: a hold and a partial
//     freeze on the same account are two distinct reservations and
//     deduct as the sum of the two, never as either one counted
//     twice. This is the Available口径 distinction the Hold
//     semantics require: Hold reserves per authorization, partial
//     freeze reserves per account amount, and Available deducts
//     each reservation source once.
//   - FreezeAmount is fail-closed on coverage: the amount must fit
//     inside the account's available funds at freeze time (balance
//     minus holds, review freezes, term principals, and any partial
//     freeze already in force). An amount above available is
//     rejected with ErrPartialFreezeExceedsAvailable and freezes
//     nothing — a partial freeze can never over-reserve into funds
//     that are already spent or reserved.
//   - A fully frozen, closed, or suspended account cannot take a new
//     partial freeze (ErrAccountFrozen / ErrAccountClosed /
//     ErrAccountSuspended, mirroring Hold): a whole-account stop
//     already covers the account, and a terminal account takes no
//     new reservations. Unfreezing is exempt, like Hold release:
//     freeing funds is never blocked by a stop, so a frozen account
//     can still shed its partial freeze.
//   - UnfreezeAmount releases part of the frozen amount; releasing
//     more than is frozen is rejected with
//     ErrPartialFreezeExceedsFrozen and releases nothing — a
//     partial release can never drive the frozen total negative or
//     silently release funds that were never frozen. Releasing the
//     full frozen amount clears the record. Full release also rides
//     on Unfreeze: lifting a whole-account freeze clears any partial
//     freeze on the account too (see freeze.go), so an account never
//     comes out of a full freeze still partially frozen.
//   - Enforcement on the posting path: for overdraft-protected
//     accounts, the partially frozen amount raises the posting
//     floor (see overdraft.go): a posting that would take the
//     protected account's balance below its frozen amount is
//     rejected with ErrPartialFreezeBlockedPosting. Unprotected
//     accounts keep the ledger's sign convention — their partial
//     freeze deducts from Available exactly like a hold, advisory
//     for Post, because unprotected accounts may carry negative
//     balances and no floor exists to raise.
//   - Partial freezes are structural config, like Freeze: they
//     survive disaster-recovery snapshots and are disclosed on the
//     reconcile report, so a restored ledger never silently
//     unfreezes funds. Every freeze and release lands in the
//     structured audit chain (partial_freeze /
//     partial_freeze_released), with the amount, the currency, and
//     the remaining frozen total.
//
// Orthogonality notes: Freeze is the whole-account stop; dispute
// Hold (dispute.go) is a hold on the dispute dimension — it reserves
// through the Hold machinery, with an expiry and a capture path,
// while a partial freeze has neither (it lasts until explicitly
// released, and it never settles into a posting). A cent frozen
// partially and also held by a dispute is reserved by two
// independent mechanisms and deducts as their sum; neither mechanism
// knows about the other, and neither double-counts itself.

// Partial-freeze errors.
var (
	// ErrPartialFreezeEmptyAccount is returned by FreezeAmount for
	// an empty account.
	ErrPartialFreezeEmptyAccount = errors.New("ledger: partial freeze account must not be empty")
	// ErrPartialFreezeNonPositiveAmount is returned by FreezeAmount
	// and UnfreezeAmount for a non-positive amount.
	ErrPartialFreezeNonPositiveAmount = errors.New("ledger: partial freeze amount must be greater than zero")
	// ErrPartialFreezeExceedsAvailable is returned by FreezeAmount
	// when the amount does not fit inside the account's available
	// funds at freeze time.
	ErrPartialFreezeExceedsAvailable = errors.New("ledger: partial freeze amount exceeds available funds")
	// ErrPartialFreezeExceedsFrozen is returned by UnfreezeAmount
	// when the release amount is larger than the frozen amount.
	ErrPartialFreezeExceedsFrozen = errors.New("ledger: partial unfreeze amount exceeds frozen amount")
	// ErrPartialFreezeBlockedPosting is returned by the protected-
	// account posting gate when a posting would take a protected
	// account's balance below its partially frozen amount.
	ErrPartialFreezeBlockedPosting = errors.New("ledger: posting would spend partially frozen funds")
)

// PartialFrozenBalance is one account's partially frozen amount in
// one currency: the snapshot config row and the reconcile disclosure
// row for a partial freeze (see partial_freeze.go). Rows are sorted
// by (account, currency) for determinism.
type PartialFrozenBalance struct {
	Account     AccountID `json:"account"`
	Currency    string    `json:"currency"`
	AmountCents int64     `json:"amount_cents"`
	// ExpiresAt is the opt-in automatic-release deadline for this
	// partial freeze (LG-225, see partial_freeze_expiry.go). Nil
	// means the freeze lasts until explicitly released; a set
	// value round-trips through snapshots and is disclosed on the
	// reconcile report alongside the frozen amount.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// partialFrozenLocked reports the partially frozen cents on (account,
// currency). Callers must hold l.mu (either lock suffices).
func (l *Ledger) partialFrozenLocked(a AccountID, currency string) int64 {
	return l.partialFrozen[accountCurrency{account: a, currency: currency}]
}

// partialFrozenSnapshotLocked returns every partial freeze, sorted by
// (account, currency), for snapshots and the reconcile report.
// Callers must hold l.mu.
func (l *Ledger) partialFrozenSnapshotLocked() []PartialFrozenBalance {
	out := make([]PartialFrozenBalance, 0, len(l.partialFrozen))
	for k, v := range l.partialFrozen {
		if v == 0 {
			continue
		}
		row := PartialFrozenBalance{Account: k.account, Currency: k.currency, AmountCents: v}
		if exp, ok := l.partialFreezeExpiry[k]; ok {
			at := exp
			row.ExpiresAt = &at
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Account != out[j].Account {
			return out[i].Account < out[j].Account
		}
		return out[i].Currency < out[j].Currency
	})
	return out
}

// clearPartialFrozenLocked drops every partial freeze on the account
// (all currencies) and returns what was cleared, for Unfreeze: a
// whole-account unfreeze must not leave a partial freeze behind.
// Callers must hold the write lock.
func (l *Ledger) clearPartialFrozenLocked(a AccountID) []PartialFrozenBalance {
	var cleared []PartialFrozenBalance
	for k, v := range l.partialFrozen {
		if k.account == a && v != 0 {
			cleared = append(cleared, PartialFrozenBalance{Account: k.account, Currency: k.currency, AmountCents: v})
			delete(l.partialFrozen, k)
			// LG-225: a whole-account unfreeze must not leave a
			// dangling expiry behind for a freeze that no longer
			// exists — an expiry without a freeze could never be
			// set through the public API, so it is dropped here.
			delete(l.partialFreezeExpiry, k)
		}
	}
	sort.Slice(cleared, func(i, j int) bool { return cleared[i].Currency < cleared[j].Currency })
	return cleared
}

// availableBeforePartialLocked computes the account's available funds
// in the given currency before any partial freeze is counted — that
// is, AvailableIn's figure with the partial term added back — so
// FreezeAmount can test coverage against the funds the new freeze
// would reserve. Callers must hold l.mu (the write lock; the
// helpers read only).
func (l *Ledger) availableBeforePartialLocked(a AccountID, currency string, now time.Time) int64 {
	key := accountCurrency{account: a, currency: currency}
	return l.balances[key] - l.heldLocked(a, currency, now) -
		l.reviewHeldLocked(a, currency, now) - l.termLockedLocked(a, currency, now)
}

// FreezeAmount freezes AmountCents of the account's available funds
// in the default currency; the remaining available balance stays
// fully usable for Post and PostTransfer. See FreezeAmountIn for
// the currency-scoped form.
func (l *Ledger) FreezeAmount(a AccountID, amountCents int64) error {
	return l.FreezeAmountCtx(context.Background(), a, amountCents)
}

// FreezeAmountCtx is FreezeAmount with per-call audit actor
// attribution (LG-73).
func (l *Ledger) FreezeAmountCtx(ctx context.Context, a AccountID, amountCents int64) error {
	return l.freezeAmountInCtx(ctx, a, DefaultCurrency, amountCents)
}

// FreezeAmountIn freezes AmountCents of the account's available funds
// in the given currency; an empty currency means the default
// currency. The remainder stays fully usable for Post and
// PostTransfer.
func (l *Ledger) FreezeAmountIn(a AccountID, currency string, amountCents int64) error {
	return l.FreezeAmountInCtx(context.Background(), a, currency, amountCents)
}

// FreezeAmountInCtx is FreezeAmountIn with per-call audit actor
// attribution (LG-73).
func (l *Ledger) FreezeAmountInCtx(ctx context.Context, a AccountID, currency string, amountCents int64) error {
	return l.freezeAmountInCtx(ctx, a, currency, amountCents)
}

// freezeAmountInCtx is the shared body of the FreezeAmount entry
// points: validation, then the account-state gates (mirroring Hold),
// then the available-funds coverage check, then the commit. It does
// not bump the ledger version: a partial freeze moves no money.
// Callers must not hold l.mu; the write lock is taken here.
func (l *Ledger) freezeAmountInCtx(ctx context.Context, a AccountID, currency string, amountCents int64) error {
	if a == "" {
		return ErrPartialFreezeEmptyAccount
	}
	if amountCents <= 0 {
		return ErrPartialFreezeNonPositiveAmount
	}
	cur, err := normalizeCurrency(currency)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	// A frozen account cannot take on a new reservation, like Hold:
	// the whole-account stop already covers it. Expired timed
	// freezes release lazily before the gate, like in Post.
	now := time.Now()
	l.releaseExpiredFreezesLocked(now, "write_path")
	if l.closedLocked(a) {
		return ErrAccountClosed
	}
	if l.frozenLocked(a) {
		return ErrAccountFrozen
	}
	if l.suspendedLocked(a) {
		return ErrAccountSuspended
	}

	// LG-225: while a partial-freeze expiry is set for this
	// (account, currency), no new amount may freeze here. The
	// expiry releases the whole frozen amount at its deadline, so
	// admitting new funds under the same expiry would make their
	// release ambiguous — fail-safe, the new freeze is rejected
	// until the expiry releases (ExpirePartialFreezes) or is
	// manually cleared (ClearPartialFreezeExpiry). The gate is
	// fail-closed on a stale deadline too: an expiry whose instant
	// has passed but whose sweep has not run still blocks, so no
	// new funds can slip in ahead of the release. The gate is
	// per (account, currency): other currencies keep freezing.
	key := accountCurrency{account: a, currency: cur}
	if _, ok := l.partialFreezeExpiry[key]; ok {
		return ErrPartialFreezeExpirySet
	}

	// Fail-closed coverage: the new amount must fit inside the
	// available funds that are not already reserved — including by
	// an existing partial freeze, which Available already deducts.
	// The comparison is overflow-safe via coversCents against the
	// pre-partial available figure: a required total that does not
	// fit in int64 can never be covered.
	avail := l.availableBeforePartialLocked(a, cur, now) - l.partialFrozenLocked(a, cur)
	if amountCents > avail {
		return ErrPartialFreezeExceedsAvailable
	}
	total, ok := addCents(l.partialFrozen[key], amountCents)
	if !ok {
		return ErrPartialFreezeExceedsAvailable
	}
	l.partialFrozen[key] = total
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "partial_freeze",
		Actor:         "FreezeAmount",
		TraceID:       string(a),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{a},
		Details: map[string]any{
			"amount_cents":    amountCents,
			"currency":        cur,
			"frozen_cents":    total,
			"available_cents": avail - amountCents,
		},
	})
	return nil
}

// UnfreezeAmount releases AmountCents of the account's partially
// frozen funds in the default currency. Releasing more than is
// frozen is rejected and releases nothing. See UnfreezeAmountIn for
// the currency-scoped form. Releasing the full frozen amount clears
// the record; full release of every currency rides on Unfreeze.
func (l *Ledger) UnfreezeAmount(a AccountID, amountCents int64) error {
	return l.UnfreezeAmountCtx(context.Background(), a, amountCents)
}

// UnfreezeAmountCtx is UnfreezeAmount with per-call audit actor
// attribution (LG-73).
func (l *Ledger) UnfreezeAmountCtx(ctx context.Context, a AccountID, amountCents int64) error {
	return l.unfreezeAmountInCtx(ctx, a, DefaultCurrency, amountCents)
}

// UnfreezeAmountIn releases AmountCents of the account's partially
// frozen funds in the given currency; an empty currency means the
// default currency.
func (l *Ledger) UnfreezeAmountIn(a AccountID, currency string, amountCents int64) error {
	return l.unfreezeAmountInCtx(context.Background(), a, currency, amountCents)
}

// UnfreezeAmountInCtx is UnfreezeAmountIn with per-call audit actor
// attribution (LG-73).
func (l *Ledger) UnfreezeAmountInCtx(ctx context.Context, a AccountID, currency string, amountCents int64) error {
	return l.unfreezeAmountInCtx(ctx, a, currency, amountCents)
}

// unfreezeAmountInCtx is the shared body of the UnfreezeAmount entry
// points: validation, then the release. Callers must not hold l.mu;
// the write lock is taken here.
func (l *Ledger) unfreezeAmountInCtx(ctx context.Context, a AccountID, currency string, amountCents int64) error {
	if a == "" {
		return ErrPartialFreezeEmptyAccount
	}
	if amountCents <= 0 {
		return ErrPartialFreezeNonPositiveAmount
	}
	cur, err := normalizeCurrency(currency)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.unfreezeAmountLocked(ctx, a, cur, amountCents)
}

// unfreezeAmountLocked is the shared release body: the amount must
// not exceed the frozen amount, and only an actual release is
// audited. Releasing is exempt from the account-state gates —
// freeing funds is never blocked by a freeze, close, or suspension
// stop, like Hold release. Callers must hold the write lock.
func (l *Ledger) unfreezeAmountLocked(ctx context.Context, a AccountID, currency string, amountCents int64) error {
	key := accountCurrency{account: a, currency: currency}
	current := l.partialFrozen[key]
	if amountCents > current {
		return ErrPartialFreezeExceedsFrozen
	}
	remaining := current - amountCents
	if remaining == 0 {
		delete(l.partialFrozen, key)
		// LG-225: a full release clears the expiry with the
		// freeze — a partial release keeps it (the remaining
		// amount still expires at the same instant), but a
		// zero frozen amount must never carry a dangling expiry
		// that a later FreezeAmount would trip over.
		delete(l.partialFreezeExpiry, key)
	} else {
		l.partialFrozen[key] = remaining
	}
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "partial_freeze_released",
		Actor:         "UnfreezeAmount",
		TraceID:       string(a),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{a},
		Details: map[string]any{
			"amount_cents":    amountCents,
			"currency":        currency,
			"remaining_cents": remaining,
		},
	})
	return nil
}

// PartialFrozenAmount reports the account's partially frozen cents in
// the default currency. Zero when nothing is partially frozen.
func (l *Ledger) PartialFrozenAmount(a AccountID) int64 {
	return l.PartialFrozenAmountIn(a, DefaultCurrency)
}

// PartialFrozenAmountIn reports the account's partially frozen cents
// in the given currency; an empty currency means the default
// currency. Zero when nothing is partially frozen.
func (l *Ledger) PartialFrozenAmountIn(a AccountID, currency string) int64 {
	if currency == "" {
		currency = DefaultCurrency
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.partialFrozenLocked(a, currency)
}
