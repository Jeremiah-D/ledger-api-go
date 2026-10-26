package ledger

import (
	"context"
	"errors"
	"sort"
	"time"
)

// Account-level partial freeze expiry (LG-225): an opt-in
// automatic-release deadline on an existing partial freeze.
//
// A partial freeze (LG-224, see partial_freeze.go) lasts until
// explicitly released and never settles into a posting. An expiry
// adds a clock to that reservation: SetPartialFreezeExpiry stamps a
// deadline on one (account, currency) partial freeze, and
// ExpirePartialFreezes releases every freeze whose deadline has
// passed, restoring Available and recording a
// partial_freeze_expired audit event — distinct from the manual
// UnfreezeAmount release (partial_freeze_released), so operators
// can tell an automatic release from a human one.
//
// Semantics, deliberately fail-closed throughout:
//   - An expiry may only be set on a partial freeze that already
//     exists: setting one on an account/currency with nothing
//     frozen is rejected with ErrPartialFreezeNoFreeze, and a
//     past (or present) deadline is rejected with
//     ErrPartialFreezeExpiryNotFuture — in both cases the freeze
//     itself is untouched, never created, moved, or cleared by a
//     failed set. Re-setting a future deadline on an already
//     expiring freeze replaces it; the frozen amount is unaffected
//     either way.
//   - While an expiry is set, FreezeAmount for that same
//     (account, currency) is rejected with
//     ErrPartialFreezeExpirySet until the expiry releases or is
//     manually cleared. The expiry releases the whole frozen
//     amount at its deadline, so admitting new funds under it
//     would leave their release ambiguous — and a release must
//     never sweep up funds that were frozen after the deadline
//     was set. The block is per (account, currency) and is
//     fail-closed on a stale deadline: an expiry whose instant
//     has passed but whose sweep has not run still blocks, so
//     no new funds can slip in ahead of the release.
//   - Expiry release is an explicit sweep, not a lazy write-path
//     check: ExpirePartialFreezes(now) releases every freeze
//     whose deadline is at or before now, in (account, currency)
//     order, and returns how many it released. Like every other
//     freeze operation it moves no money: it bumps no ledger
//     version and appends no chain link — the audit event is
//     the record. A sweep with nothing due releases nothing and
//     audits nothing.
//   - Partial release keeps the expiry: UnfreezeAmount of part of
//     the frozen amount leaves the remaining amount expiring at
//     the same instant (see partial_freeze.go). A full release
//     clears the expiry with the freeze, and a whole-account
//     Unfreeze clears every expiry on the account — an expiry
//     without a freeze can never survive, so a later FreezeAmount
//     never trips over a dangling deadline.
//   - The expiry round-trips through snapshots on the
//     PartialFrozenBalance row (ExpiresAt) and is disclosed on the
//     reconcile report with the frozen amount, so a restored
//     ledger never silently drops a deadline. A snapshot row
//     carrying an expiry for a zero amount is skipped like any
//     zero freeze row, and its expiry is not restored.
//
// Orthogonality notes: this is the amount-level counterpart of
// FreezeUntil (LG-58, the whole-account automatic unfreeze) —
// that releases a whole-account stop, this releases one amount
// reservation — and it is orthogonal to the amount-level freeze
// itself (LG-224, which has no deadline) and to dispute Hold
// (dispute.go, which reserves on the dispute dimension with its
// own expiry and capture path, while a partial-freeze expiry
// releases only, never captures).

// Partial-freeze expiry errors.
var (
	// ErrPartialFreezeNoFreeze is returned by
	// SetPartialFreezeExpiry when the (account, currency) has no
	// partial freeze to put a deadline on.
	ErrPartialFreezeNoFreeze = errors.New("ledger: no partial freeze to set expiry on")
	// ErrPartialFreezeExpiryNotFuture is returned by
	// SetPartialFreezeExpiry when the deadline is not after now.
	ErrPartialFreezeExpiryNotFuture = errors.New("ledger: partial freeze expiry must be in the future")
	// ErrPartialFreezeExpirySet is returned by FreezeAmount when
	// the (account, currency) already carries a partial-freeze
	// expiry: no new amount may freeze under a pending deadline.
	ErrPartialFreezeExpirySet = errors.New("ledger: partial freeze expiry is set; new freezes rejected until expiry release or clearance")
)

// SetPartialFreezeExpiry stamps an automatic-release deadline on
// the account's existing partial freeze in the default currency.
// See SetPartialFreezeExpiryIn for the currency-scoped form.
func (l *Ledger) SetPartialFreezeExpiry(a AccountID, expiresAt time.Time) error {
	return l.SetPartialFreezeExpiryCtx(context.Background(), a, expiresAt)
}

// SetPartialFreezeExpiryCtx is SetPartialFreezeExpiry with per-call
// audit actor attribution (LG-73).
func (l *Ledger) SetPartialFreezeExpiryCtx(ctx context.Context, a AccountID, expiresAt time.Time) error {
	return l.setPartialFreezeExpiryInCtx(ctx, a, DefaultCurrency, expiresAt)
}

// SetPartialFreezeExpiryIn stamps an automatic-release deadline on
// the account's existing partial freeze in the given currency; an
// empty currency means the default currency.
func (l *Ledger) SetPartialFreezeExpiryIn(a AccountID, currency string, expiresAt time.Time) error {
	return l.SetPartialFreezeExpiryInCtx(context.Background(), a, currency, expiresAt)
}

// SetPartialFreezeExpiryInCtx is SetPartialFreezeExpiryIn with
// per-call audit actor attribution (LG-73).
func (l *Ledger) SetPartialFreezeExpiryInCtx(ctx context.Context, a AccountID, currency string, expiresAt time.Time) error {
	return l.setPartialFreezeExpiryInCtx(ctx, a, currency, expiresAt)
}

// setPartialFreezeExpiryInCtx is the shared body of the
// SetPartialFreezeExpiry entry points: validation, then the
// existing-freeze check, then the commit. A failed set mutates
// nothing — neither the frozen amount nor any previously set
// expiry moves. It does not bump the ledger version: an expiry
// moves no money. Callers must not hold l.mu; the write lock is
// taken here.
func (l *Ledger) setPartialFreezeExpiryInCtx(ctx context.Context, a AccountID, currency string, expiresAt time.Time) error {
	if a == "" {
		return ErrPartialFreezeEmptyAccount
	}
	cur, err := normalizeCurrency(currency)
	if err != nil {
		return err
	}
	if !expiresAt.After(time.Now()) {
		return ErrPartialFreezeExpiryNotFuture
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	key := accountCurrency{account: a, currency: cur}
	frozen := l.partialFrozenLocked(a, cur)
	if frozen == 0 {
		return ErrPartialFreezeNoFreeze
	}
	l.partialFreezeExpiry[key] = expiresAt
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "partial_freeze_expiry_set",
		Actor:         "SetPartialFreezeExpiry",
		TraceID:       string(a),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{a},
		Details: map[string]any{
			"currency":     cur,
			"frozen_cents": frozen,
			"expires_at":   expiresAt.UTC().Format(time.RFC3339),
		},
	})
	return nil
}

// ClearPartialFreezeExpiry drops the automatic-release deadline
// from the account's partial freeze in the default currency,
// leaving the frozen amount itself untouched: after clearing, the
// freeze lasts until explicitly released again, and FreezeAmount
// is admitted for the (account, currency) again. Clearing an
// (account, currency) with no expiry is a no-op. See
// ClearPartialFreezeExpiryIn for the currency-scoped form.
func (l *Ledger) ClearPartialFreezeExpiry(a AccountID) error {
	return l.ClearPartialFreezeExpiryCtx(context.Background(), a)
}

// ClearPartialFreezeExpiryCtx is ClearPartialFreezeExpiry with
// per-call audit actor attribution (LG-73).
func (l *Ledger) ClearPartialFreezeExpiryCtx(ctx context.Context, a AccountID) error {
	return l.clearPartialFreezeExpiryInCtx(ctx, a, DefaultCurrency)
}

// ClearPartialFreezeExpiryIn drops the automatic-release deadline
// from the account's partial freeze in the given currency; an
// empty currency means the default currency.
func (l *Ledger) ClearPartialFreezeExpiryIn(a AccountID, currency string) error {
	return l.ClearPartialFreezeExpiryInCtx(context.Background(), a, currency)
}

// ClearPartialFreezeExpiryInCtx is ClearPartialFreezeExpiryIn with
// per-call audit actor attribution (LG-73).
func (l *Ledger) ClearPartialFreezeExpiryInCtx(ctx context.Context, a AccountID, currency string) error {
	return l.clearPartialFreezeExpiryInCtx(ctx, a, currency)
}

// clearPartialFreezeExpiryInCtx is the shared body of the
// ClearPartialFreezeExpiry entry points. Only an actual clear is
// audited; a no-op (no expiry set) audits nothing. It does not
// bump the ledger version. Callers must not hold l.mu; the write
// lock is taken here.
func (l *Ledger) clearPartialFreezeExpiryInCtx(ctx context.Context, a AccountID, currency string) error {
	if a == "" {
		return ErrPartialFreezeEmptyAccount
	}
	cur, err := normalizeCurrency(currency)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	key := accountCurrency{account: a, currency: cur}
	if _, ok := l.partialFreezeExpiry[key]; !ok {
		return nil
	}
	delete(l.partialFreezeExpiry, key)
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "partial_freeze_expiry_cleared",
		Actor:         "ClearPartialFreezeExpiry",
		TraceID:       string(a),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{a},
		Details: map[string]any{
			"currency":     cur,
			"frozen_cents": l.partialFrozenLocked(a, cur),
		},
	})
	return nil
}

// PartialFreezeExpiry reports the automatic-release deadline on
// the account's partial freeze in the default currency, and
// whether one is set. Zero and false when nothing is expiring.
func (l *Ledger) PartialFreezeExpiry(a AccountID) (time.Time, bool) {
	return l.PartialFreezeExpiryIn(a, DefaultCurrency)
}

// PartialFreezeExpiryIn reports the automatic-release deadline on
// the account's partial freeze in the given currency, and whether
// one is set; an empty currency means the default currency.
func (l *Ledger) PartialFreezeExpiryIn(a AccountID, currency string) (time.Time, bool) {
	if currency == "" {
		currency = DefaultCurrency
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	exp, ok := l.partialFreezeExpiry[accountCurrency{account: a, currency: currency}]
	return exp, ok
}

// ExpirePartialFreezes releases every partial freeze whose
// automatic-release deadline is at or before now, in (account,
// currency) order, and returns how many it released. A released
// freeze restores Available by exactly its frozen amount, is
// recorded as a partial_freeze_expired audit event (never as a
// manual partial_freeze_released), and — like every freeze
// operation — bumps no ledger version and appends no chain link.
// A sweep with nothing due releases nothing and audits nothing.
// Release is exempt from the account-state gates, like manual
// unfreezing: freeing funds is never blocked by a freeze, close,
// or suspension stop.
func (l *Ledger) ExpirePartialFreezes(now time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	type dueKey struct {
		key accountCurrency
		exp time.Time
	}
	due := make([]dueKey, 0, len(l.partialFreezeExpiry))
	for k, exp := range l.partialFreezeExpiry {
		if now.Before(exp) {
			continue
		}
		due = append(due, dueKey{key: k, exp: exp})
	}
	sort.Slice(due, func(i, j int) bool {
		if due[i].key.account != due[j].key.account {
			return due[i].key.account < due[j].key.account
		}
		return due[i].key.currency < due[j].key.currency
	})
	released := 0
	for _, d := range due {
		amount := l.partialFrozen[d.key]
		delete(l.partialFreezeExpiry, d.key)
		if amount == 0 {
			// Defensive only: the public API never leaves an
			// expiry without a freeze (a full release clears
			// it), so a zero row here is a dangling deadline,
			// not a release — drop it without counting or
			// auditing a release that freed nothing.
			continue
		}
		delete(l.partialFrozen, d.key)
		released++
		l.emitAudit(AuditEvent{
			Op:            "partial_freeze_expired",
			Actor:         "ExpirePartialFreezes",
			TraceID:       string(d.key.account),
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Accounts:      []AccountID{d.key.account},
			Details: map[string]any{
				"currency":       d.key.currency,
				"released_cents": amount,
				"expires_at":     d.exp.UTC().Format(time.RFC3339),
			},
		})
	}
	return released
}
