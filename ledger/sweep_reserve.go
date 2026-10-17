package ledger

import (
	"errors"
	"sort"
)

// Sweep reserve floors (LG-111).
//
// A sweep reserve is an opt-in per-(account, currency) minimum balance
// that PostSweep leaves untouched: only the excess over the floor moves
// to the treasury target. It is the operating-float side of the payment
// domain — a merchant's sub-merchant keeps its settlement float for
// next-day refunds while the surplus still rolls up at day end.
//
// Reserves are structural config, like daily limits: setting or clearing
// a reserve does not bump the journal version, and every change is
// recorded in the structured audit log (LG-29) as "sweep_reserve_set"
// (with account, currency, floor, and the previous floor) or
// "sweep_reserve_cleared". Reserves round-trip through the
// disaster-recovery snapshot config record, like the daily limits.

// ErrInvalidSweepReserve is returned by SetSweepReserve when the floor is
// negative. A zero floor is legal but pointless — it behaves like no
// reserve — so operators clear reserves with ClearSweepReserve instead;
// zero floors are rejected to keep the config table free of no-op rows.
var ErrInvalidSweepReserve = errors.New("ledger: sweep reserve floor must be a positive amount in cents")

// SweepReserve is one configured sweep reserve floor, the unit the
// snapshot config record serializes (see snapshot.go) and the
// reconciliation report discloses (see Reconcile).
type SweepReserve struct {
	Account    AccountID `json:"account"`
	Currency   string    `json:"currency"`
	FloorCents int64     `json:"floor_cents"`
}

// SetSweepReserve configures (or replaces) the sweep reserve floor for
// one account in one currency: PostSweep moves only the balance in
// excess of floorCents to the target and leaves the floor behind. A
// zero (or missing) floor means "no reserve" — full positive balances
// sweep as before; a positive floor keeps the account's operating float.
// Negative floors are rejected with ErrInvalidSweepReserve.
//
// Setting a reserve is structural: it does not bump the journal version
// (no balances change) and it is visible to audit tooling — the change
// is recorded as "sweep_reserve_set" with the old and new floor, and
// Reconcile lists every configured reserve. Setting the account's
// current floor again is a no-op — it returns nil without writing an
// audit event, so idempotent retries stay silent in the compliance trail.
func (l *Ledger) SetSweepReserve(account AccountID, currency string, floorCents int64) error {
	currency, err := normalizeCurrency(currency)
	if err != nil {
		return err
	}
	if floorCents <= 0 {
		return ErrInvalidSweepReserve
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	key := accountCurrency{account: account, currency: currency}
	old, had := l.sweepReserves[key]
	if had && old == floorCents {
		// Idempotent replay of the current floor: nothing changed,
		// so no audit event — the trail records changes, not retries.
		return nil
	}
	l.sweepReserves[key] = floorCents

	details := map[string]any{
		"account":     string(account),
		"currency":    currency,
		"floor_cents": floorCents,
	}
	op := "sweep_reserve_set"
	if had {
		op = "sweep_reserve_changed"
		details["old_floor_cents"] = old
	}
	l.emitAudit(AuditEvent{
		Op:            op,
		Actor:         "SetSweepReserve",
		TraceID:       string(account),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{account},
		Details:       details,
	})
	return nil
}

// ClearSweepReserve removes the sweep reserve floor for one account in
// one currency. Clearing a reserve that was never set is a silent no-op
// (nil, no audit event), so teardown paths stay idempotent. Like
// SetSweepReserve, it is structural and does not bump the journal
// version; a real removal is recorded as "sweep_reserve_cleared".
func (l *Ledger) ClearSweepReserve(account AccountID, currency string) error {
	currency, err := normalizeCurrency(currency)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	key := accountCurrency{account: account, currency: currency}
	old, had := l.sweepReserves[key]
	if !had {
		return nil
	}
	delete(l.sweepReserves, key)
	l.emitAudit(AuditEvent{
		Op:            "sweep_reserve_cleared",
		Actor:         "ClearSweepReserve",
		TraceID:       string(account),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{account},
		Details: map[string]any{
			"account":         string(account),
			"currency":        currency,
			"old_floor_cents": old,
		},
	})
	return nil
}

// SweepReserve reports the configured sweep reserve floor for one account
// in one currency, and whether a reserve is configured at all.
func (l *Ledger) SweepReserve(account AccountID, currency string) (int64, bool) {
	currency, err := normalizeCurrency(currency)
	if err != nil {
		return 0, false
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	floor, ok := l.sweepReserves[accountCurrency{account: account, currency: currency}]
	return floor, ok
}

// SweepReserves lists every configured sweep reserve floor, sorted by
// (account, currency), for the HTTP layer and reporting tooling.
func (l *Ledger) SweepReserves() []SweepReserve {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.sweepReservesLocked()
}

// sweepReservesLocked returns every configured sweep reserve floor,
// sorted by (account, currency), for the reconciliation report and the
// snapshot. Callers must hold l.mu.
func (l *Ledger) sweepReservesLocked() []SweepReserve {
	out := make([]SweepReserve, 0, len(l.sweepReserves))
	for k, floor := range l.sweepReserves {
		out = append(out, SweepReserve{Account: k.account, Currency: k.currency, FloorCents: floor})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Account != out[j].Account {
			return out[i].Account < out[j].Account
		}
		return out[i].Currency < out[j].Currency
	})
	return out
}

// applySweepReservesLocked shrinks the planned sweep legs to the balance
// in excess of each leg's configured reserve floor: for a leg whose
// (from, currency) carries a reserve, the leg moves balance−floor and
// the floor stays behind. A leg whose floor meets or exceeds the
// balance contributes no leg at all — the floor consumes the whole
// balance, and there is no zero-amount journal row to record. Legs
// without a configured reserve move their full positive balance, byte
// for byte as before reserves existed. Callers must hold l.mu.
//
// Reserves are applied after the frozen checks and before the budget
// checks in PostSweep: a frozen reserve is already rejected above, and
// the monthly spend budget guards the money that actually moves.
func (l *Ledger) applySweepReservesLocked(legs []SweepLeg) []SweepLeg {
	if len(l.sweepReserves) == 0 {
		return legs
	}
	out := legs[:0]
	for _, leg := range legs {
		floor, ok := l.sweepReserves[accountCurrency{account: leg.From, currency: leg.Currency}]
		if !ok || floor <= 0 {
			out = append(out, leg)
			continue
		}
		if leg.AmountCents <= floor {
			// The reserve consumes the whole balance: no leg lands,
			// and the sweep still succeeds (the receipt's leg list is
			// simply shorter).
			continue
		}
		leg.AmountCents -= floor
		out = append(out, leg)
	}
	// legs is nil or empty only when every leg was consumed: return a
	// non-nil empty slice so callers that range over it behave
	// identically either way.
	if out == nil {
		out = []SweepLeg{}
	}
	return out
}
