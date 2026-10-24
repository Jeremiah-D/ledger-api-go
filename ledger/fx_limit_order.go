package ledger

import (
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"
)

// FX limit orders (LG-191): resting price-triggered FX transfers.
//
// A limit order parks a cross-currency transfer instruction in the
// ledger's order book: convert AmountCents of FromCurrency into
// ToCurrency for FromAccount -> ToAccount, but only when the market
// price reaches the caller's limit. A market order (PostTransfer with
// to_currency) converts at whatever the rate table says right now; a
// limit order waits — it is orthogonal to market orders, and the two
// share one settlement engine: a triggered fill posts through
// postTransferFXLocked, the exact write path a market order takes, so
// fees, overdraft and daily-limit gates, the dual-control review
// threshold check below, the audit chain, and receipts behave
// identically. No parallel settlement logic exists here.
//
// Side semantics (the dealing side of the customer, the Transfer.FXSide
// convention — see fx_spread.go):
//   - "sell" (LimitOrderSell, the default): the customer sells the
//     source currency. The order triggers when the market BID for
//     (FromCurrency, ToCurrency) is at or above the limit price, and
//     the fill settles at the bid — the desk buys at the price the
//     customer asked for or better.
//   - "buy" (LimitOrderBuy): the customer buys the target currency.
//     The order triggers when the market ASK is at or below the limit
//     price, and the fill settles at the ask.
//
// The limit price is an integer ratio LimitNum/LimitDen quoted in
// target currency per source currency — the ExchangeRate.Num/Den
// convention — so a USD->EUR order with limit 110/100 triggers a sell
// when 1 USD bids at or above 1.10 EUR, or a buy when it asks at or
// below 1.10 EUR. All comparisons are exact big.Rat: no float64 ever
// touches a price, and the touch test compares the side's DEALING
// price (bid for a sell, ask for a buy — see effectiveFXRateLocked),
// never the mid.
//
// Fills and partial fills: MatchLimitOrders(now) is the only thing
// that moves the book. On a touch it fills up to the remaining amount,
// bounded by the payer's available source-currency balance — a fill is
// funded money, never leverage — so one order may fill across many
// sweeps: each fill accumulates into FilledCents, the remainder stays
// pending, and the order leaves the book as "filled" only when
// FilledCents reaches AmountCents. A touch with nothing funded (zero
// available) books nothing and the order keeps resting: fund the
// account and the next sweep fills it ("fund and fill").
//
// Expiry: a sweep auto-expires orders with now >= Expiry (the rate-table
// convention: a zero Expiry means the order never expires). Cancel is
// manual and succeeds only from the pending state: a filled, cancelled,
// or expired order rejects CancelLimitOrder with ErrLimitOrderNotPending
// — the state machine is pending -> filled | cancelled | expired, and it
// moves in exactly one direction.
//
// Concurrency: the whole book lives under l.mu; MatchLimitOrders takes
// the write lock once, walks the orders in insertion order, and posts
// fills through the locked transfer path. No goroutines, no background
// matching: the caller decides when the market is checked. Sweep cost
// is O(pending orders x rate-table lookup) — one effective-rate
// resolution per resting order per sweep — stated here so nobody is
// surprised by a large book.
//
// Review gate: a fill that would route into dual-control review is NOT
// posted. A limit fill is a settlement promise; parking its legs in an
// approval queue breaks that promise (the ErrForwardReviewRouted
// rationale, see forward.go), and repeatedly freezing fresh funds on
// every sweep would be worse — so a fill at or above the payer's review
// threshold is skipped for this sweep, recorded on the order, and left
// pending for the operator to handle.
//
// A fill that the transfer path rejects for a funding reason
// (overdraft, daily limit) or a missing rate keeps the order pending
// with LastError set: the order is patient, the ledger is honest, and
// the next sweep tries again. A sweep never fails because of one
// order's problem — MatchLimitOrders returns the counts, not an error.
//
// The book is structural: it survives ExportSnapshot/ImportSnapshot
// (see FXLimitOrders in snapshot.go), because a restore that dropped
// resting orders would silently void the customer's price instructions.
// The fills themselves are ordinary journal entries, so the accounting
// equation and the audit chain cover them with no special cases.

// FX limit-order dealing sides (see Transfer.FXSide / fx_spread.go).
const (
	// LimitOrderSell triggers on the bid: the customer sells the source
	// currency and the fill settles at the bid.
	LimitOrderSell = "sell"
	// LimitOrderBuy triggers on the ask: the customer buys the target
	// currency and the fill settles at the ask.
	LimitOrderBuy = "buy"
)

// LimitOrderState is the lifecycle state of an FX limit order:
// pending -> filled | cancelled | expired, one direction only.
type LimitOrderState string

const (
	// LimitOrderPending is a resting order: still triggerable, still
	// cancellable, still expirable. Every order starts here.
	LimitOrderPending LimitOrderState = "pending"
	// LimitOrderFilled is a fully filled order: FilledCents reached
	// AmountCents. Terminal.
	LimitOrderFilled LimitOrderState = "filled"
	// LimitOrderCancelled is a manually cancelled order (only from
	// pending). Terminal.
	LimitOrderCancelled LimitOrderState = "cancelled"
	// LimitOrderExpired is an order the sweep retired at Expiry (only
	// from pending). Terminal.
	LimitOrderExpired LimitOrderState = "expired"
)

// Validation errors for FX limit orders.
var (
	// ErrInvalidLimitOrder is returned for a malformed order: empty ID,
	// bad currency codes, equal currencies, empty/equal accounts, a bad
	// side, a non-positive limit price, a non-positive amount, or an
	// expiry that is not in the future. It is a 400-class field
	// validation error: the request is malformed, not semantically
	// rejected.
	ErrInvalidLimitOrder = errors.New("ledger: invalid FX limit order")
	// ErrDuplicateLimitOrder is returned when an order is placed with an
	// ID that already exists in the book. It is a 409-class error.
	ErrDuplicateLimitOrder = errors.New("ledger: FX limit order ID already exists")
	// ErrLimitOrderNotFound is returned when no order exists for the
	// given ID. It is a 404-class error.
	ErrLimitOrderNotFound = errors.New("ledger: FX limit order not found")
	// ErrLimitOrderNotPending is returned when CancelLimitOrder targets
	// an order that already left the pending state (filled, cancelled,
	// or expired). It is a 409-class error: the lifecycle moved on.
	ErrLimitOrderNotPending = errors.New("ledger: FX limit order is not pending")
)

// LimitOrder is one resting FX limit order: a price-triggered
// cross-currency transfer instruction.
//
// FromAccount is the payer (the source-currency account the fills debit)
// and ToAccount the payee (credited the converted amount in the target
// currency). FromCurrency/ToCurrency are normalized 3-letter uppercase
// ISO 4217 codes, always different — the rate-table (from, to) key.
// Side is "buy" or "sell" (LimitOrderBuy/LimitOrderSell), canonicalized
// lowercase at placement. LimitNum/LimitDen is the trigger price in
// target currency per source currency, a positive integer ratio.
// AmountCents is the total order size in source-currency cents;
// FilledCents accumulates across fills. State is the lifecycle state;
// Expiry is the auto-cancel deadline (zero = never expires); CreatedAt
// is the placement time; FillCount is the number of fills so far (the
// fill transfer's idempotency key is "<ID>/fill/<FillCount+1>");
// LastError is the last sweep/post failure for this order, cleared on
// the next successful fill — observability, not books.
type LimitOrder struct {
	ID           string          `json:"id"`
	FromAccount  AccountID       `json:"from_account"`
	ToAccount    AccountID       `json:"to_account"`
	FromCurrency string          `json:"from_currency"`
	ToCurrency   string          `json:"to_currency"`
	Side         string          `json:"side"`
	LimitNum     int64           `json:"limit_num"`
	LimitDen     int64           `json:"limit_den"`
	AmountCents  int64           `json:"amount_cents"`
	FilledCents  int64           `json:"filled_cents"`
	FillCount    int             `json:"fill_count"`
	State        LimitOrderState `json:"state"`
	Expiry       time.Time       `json:"expiry,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	LastError    string          `json:"last_error,omitempty"`
}

// RemainingCents reports the unfilled remainder of the order in
// source-currency cents: AmountCents - FilledCents.
func (o *LimitOrder) RemainingCents() int64 {
	return o.AmountCents - o.FilledCents
}

// pair returns the rate-table key of the order: the directional pair
// (FromCurrency, ToCurrency).
func (o *LimitOrder) pair() fxPair {
	return fxPair{from: o.FromCurrency, to: o.ToCurrency}
}

// limitRat returns the limit price as an exact *big.Rat.
func (o *LimitOrder) limitRat() *big.Rat {
	return new(big.Rat).SetFrac64(o.LimitNum, o.LimitDen)
}

// LimitOrderSweepResult is what MatchLimitOrders reports: how many
// fills it posted and how many orders it auto-expired. One sweep is one
// pass; per-order failures are recorded on the orders (LastError), not
// returned — a sweep never fails because of one order's problem.
type LimitOrderSweepResult struct {
	Fills   int `json:"fills"`
	Expires int `json:"expires"`
}

// limitOrderSideOf canonicalizes the dealing side: FXSideBuy (case-
// insensitive) is a buy, anything else validated at placement is a
// sell.
func limitOrderSideOf(o *LimitOrder) string {
	if strings.EqualFold(o.Side, FXSideBuy) {
		return FXSideBuy
	}
	return FXSideSell
}

// PlaceLimitOrder parks a limit order in the book. The order is
// validated before anything is stored: a malformed order is rejected
// with ErrInvalidLimitOrder, a reused ID with ErrDuplicateLimitOrder.
// Placement is structural — no journal entries, no version bump — and
// emits the fx_limit_order_placed audit event.
func (l *Ledger) PlaceLimitOrder(o LimitOrder) error {
	now := time.Now()
	if o.ID == "" {
		return fmt.Errorf("%w: order ID is required", ErrInvalidLimitOrder)
	}
	from, to, err := normalizeLimitOrderPair(o.FromCurrency, o.ToCurrency)
	if err != nil {
		return err
	}
	if o.FromAccount == "" || o.ToAccount == "" {
		return fmt.Errorf("%w: from and to accounts are required", ErrInvalidLimitOrder)
	}
	if o.FromAccount == o.ToAccount {
		return fmt.Errorf("%w: from and to accounts must differ", ErrInvalidLimitOrder)
	}
	switch {
	case strings.EqualFold(o.Side, FXSideBuy):
		o.Side = FXSideBuy
	case o.Side == "" || strings.EqualFold(o.Side, FXSideSell):
		o.Side = FXSideSell
	default:
		return fmt.Errorf("%w: side must be %q or %q, got %q", ErrInvalidLimitOrder, FXSideBuy, FXSideSell, o.Side)
	}
	if o.LimitNum <= 0 || o.LimitDen <= 0 {
		return fmt.Errorf("%w: limit price must be a positive ratio, got %d/%d", ErrInvalidLimitOrder, o.LimitNum, o.LimitDen)
	}
	if o.AmountCents <= 0 {
		return fmt.Errorf("%w: amount must be positive, got %d", ErrInvalidLimitOrder, o.AmountCents)
	}
	if !o.Expiry.IsZero() && !o.Expiry.After(now) {
		return fmt.Errorf("%w: expiry must be in the future", ErrInvalidLimitOrder)
	}
	o.FromCurrency, o.ToCurrency = from, to
	o.CreatedAt = now
	o.State = LimitOrderPending

	l.mu.Lock()
	defer l.mu.Unlock()
	if _, dup := l.limitOrders[o.ID]; dup {
		return fmt.Errorf("%w: %q", ErrDuplicateLimitOrder, o.ID)
	}
	// Store a copy: the caller's struct must not alias book state.
	stored := o
	l.limitOrders[o.ID] = &stored
	l.limitOrderSeq = append(l.limitOrderSeq, o.ID)
	l.emitAudit(AuditEvent{
		Op:       "fx_limit_order_placed",
		Actor:    "PlaceLimitOrder",
		TraceID:  o.ID,
		Accounts: []AccountID{o.FromAccount, o.ToAccount},
		Details: map[string]any{
			"order_id":      o.ID,
			"from_currency": o.FromCurrency,
			"to_currency":   o.ToCurrency,
			"side":          o.Side,
			"limit_num":     o.LimitNum,
			"limit_den":     o.LimitDen,
			"amount_cents":  o.AmountCents,
			"expiry":        o.Expiry.Format(time.RFC3339),
		},
	})
	return nil
}

// normalizeLimitOrderPair validates the currency pair of a limit order:
// both codes must be present 3-letter uppercase ISO 4217 codes (empty
// is rejected here — unlike normalizeCurrency, a pair has no default
// currency), and they must differ.
func normalizeLimitOrderPair(from, to string) (string, string, error) {
	if from == "" || to == "" {
		return "", "", fmt.Errorf("%w: from and to currencies are required", ErrInvalidLimitOrder)
	}
	f, err := normalizeCurrency(from)
	if err != nil {
		return "", "", fmt.Errorf("%w: bad from currency %q", ErrInvalidLimitOrder, from)
	}
	t, err := normalizeCurrency(to)
	if err != nil {
		return "", "", fmt.Errorf("%w: bad to currency %q", ErrInvalidLimitOrder, to)
	}
	if f == t {
		return "", "", fmt.Errorf("%w: from and to currencies must differ", ErrInvalidLimitOrder)
	}
	return f, t, nil
}

// CancelLimitOrder manually cancels a pending order. Cancelling a
// filled, cancelled, or expired order is rejected with
// ErrLimitOrderNotPending — the state machine moves one way. An
// unknown ID reports ErrLimitOrderNotFound. A successful cancel emits
// the fx_limit_order_cancelled audit event; cancellation is structural
// (no version bump).
func (l *Ledger) CancelLimitOrder(id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	o, ok := l.limitOrders[id]
	if !ok {
		return fmt.Errorf("%w: %q", ErrLimitOrderNotFound, id)
	}
	if o.State != LimitOrderPending {
		return fmt.Errorf("%w: order %q is %s", ErrLimitOrderNotPending, id, o.State)
	}
	o.State = LimitOrderCancelled
	o.LastError = ""
	l.limitOrderCancels.Add(1)
	l.emitAudit(AuditEvent{
		Op:       "fx_limit_order_cancelled",
		Actor:    "CancelLimitOrder",
		TraceID:  id,
		Accounts: []AccountID{o.FromAccount, o.ToAccount},
		Details: map[string]any{
			"order_id":        id,
			"filled_cents":    o.FilledCents,
			"remaining_cents": o.RemainingCents(),
			"reason":          "manual",
		},
	})
	return nil
}

// LimitOrder returns a copy of the order with the given ID, or false
// when no order exists. Read-only.
func (l *Ledger) LimitOrder(id string) (LimitOrder, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	o, ok := l.limitOrders[id]
	if !ok {
		return LimitOrder{}, false
	}
	return *o, true
}

// LimitOrders returns a copy of every order in the book, in placement
// order. Read-only.
func (l *Ledger) LimitOrders() []LimitOrder {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.limitOrdersLocked()
}

// limitOrdersLocked returns copies of the book in insertion order.
// Callers must hold l.mu (either lock suffices).
func (l *Ledger) limitOrdersLocked() []LimitOrder {
	out := make([]LimitOrder, 0, len(l.limitOrderSeq))
	for _, id := range l.limitOrderSeq {
		if o, ok := l.limitOrders[id]; ok {
			out = append(out, *o)
		}
	}
	return out
}

// MatchLimitOrders sweeps the book once at now: it auto-expires orders
// whose deadline arrived and fills orders the market touched. See the
// package comment for the semantics; the method-level contract:
//   - one pass over the book in insertion order — no order is visited
//     twice, so the sweep always terminates;
//   - expiry is checked before the touch: an expired order never fills
//     on its last instant (now >= Expiry means expired, not filled);
//   - a fill posts through postTransferFXLocked with the zero quote,
//     i.e. at the commit-time dealing price the touch was measured
//     against — no slippage gate, because no admission quote exists;
//   - per-order failures (missing/expired rate, funding rejection,
//     review-threshold skip) are recorded on the order and do not fail
//     the sweep.
//
// The caller must NOT hold l.mu: the sweep takes the write lock.
func (l *Ledger) MatchLimitOrders(now time.Time) LimitOrderSweepResult {
	l.mu.Lock()
	defer l.mu.Unlock()
	var res LimitOrderSweepResult
	for _, id := range l.limitOrderSeq {
		o, ok := l.limitOrders[id]
		if !ok || o.State != LimitOrderPending {
			continue
		}
		// Expiry first: an order whose deadline arrived leaves the
		// book as expired — it never fills on its last instant.
		if !o.Expiry.IsZero() && !now.Before(o.Expiry) {
			o.State = LimitOrderExpired
			o.LastError = ""
			l.limitOrderCancels.Add(1)
			l.emitAudit(AuditEvent{
				Op:       "fx_limit_order_expired",
				Actor:    "MatchLimitOrders",
				TraceID:  o.ID,
				Accounts: []AccountID{o.FromAccount, o.ToAccount},
				Details: map[string]any{
					"order_id":        o.ID,
					"filled_cents":    o.FilledCents,
					"remaining_cents": o.RemainingCents(),
					"expiry":          o.Expiry.Format(time.RFC3339),
				},
			})
			res.Expires++
			continue
		}
		if l.fillLimitOrderLocked(o, now) {
			res.Fills++
		}
	}
	return res
}

// fillLimitOrderLocked evaluates one pending order against the current
// market and fills it when the market touched the limit. It reports
// whether a fill posted. Callers must hold the write lock.
func (l *Ledger) fillLimitOrderLocked(o *LimitOrder, now time.Time) bool {
	side := limitOrderSideOf(o)
	eff, err := l.effectiveFXRateLocked(o.pair(), side, now)
	if err != nil {
		// No live rate for the pair: the order waits. The sweep is
		// patient — this is a state, not a failure of the sweep.
		o.LastError = err.Error()
		return false
	}
	// The touch test compares the side's dealing price (bid for a sell,
	// ask for a buy) against the limit, exact big.Rat — never the mid,
	// never a float.
	var dealing *big.Rat
	if side == FXSideBuy {
		dealing = new(big.Rat).SetFrac64(eff.AskNum, eff.AskDen)
	} else {
		dealing = new(big.Rat).SetFrac64(eff.BidNum, eff.BidDen)
	}
	limit := o.limitRat()
	touched := dealing.Cmp(limit) >= 0 // sell: bid >= limit
	if side == FXSideBuy {
		touched = dealing.Cmp(limit) <= 0 // buy: ask <= limit
	}
	if !touched {
		return false
	}
	// Funded fills only: the fill is bounded by the payer's available
	// source-currency balance. An unfunded touch books nothing — the
	// order keeps resting until the account is funded. The transfer's
	// own overdraft/daily-limit/frozen gates stay authoritative: a
	// rejection there keeps the order pending with LastError set.
	available := l.balances[accountCurrency{account: o.FromAccount, currency: o.FromCurrency}]
	fillCents := o.RemainingCents()
	if fillCents > available {
		fillCents = available
	}
	if fillCents <= 0 {
		o.LastError = "limit order touched but the payer has no available source-currency balance"
		return false
	}
	// A fill that would route into dual-control review is not posted:
	// a limit fill is a settlement promise, and parking its legs in
	// an approval queue breaks that promise (see the package comment).
	if threshold, ok := l.effectiveReviewThresholdLocked(o.FromAccount); ok && fillCents >= threshold {
		o.LastError = "limit order fill would route to dual-control review; left pending for the operator"
		return false
	}
	seq := o.FillCount + 1
	fillID := fmt.Sprintf("%s/fill/%d", o.ID, seq)
	t := Transfer{
		ID:             fillID,
		From:           o.FromAccount,
		To:             o.ToAccount,
		AmountCents:    fillCents,
		Currency:       o.FromCurrency,
		ToCurrency:     o.ToCurrency,
		FXSide:         side,
		IdempotencyKey: fillID,
		Memo:           fmt.Sprintf("fx limit order %s fill %d", o.ID, seq),
		CreatedAt:      now,
	}
	// The zero quote means commit-time semantics: the fill settles at
	// the dealing price this sweep measured — the touched price — with
	// no slippage gate, because no admission quote exists for a
	// resting order.
	receipt, err := l.postTransferFXLocked(t, o.ToCurrency, now, FXRateQuote{})
	if err != nil {
		o.LastError = err.Error()
		return false
	}
	if receipt.ReviewStatus == ReviewStatusPending {
		// Belt and suspenders: the pre-check above should have caught
		// this, but if the fill still routed to review the fill did
		// not settle — the order stays pending so the funds are not
		// re-frozen by a retry. An operator settles or rejects the
		// frozen legs.
		o.LastError = "limit order fill routed to dual-control review after posting; left pending for the operator"
		return false
	}
	o.FilledCents += fillCents
	o.FillCount = seq
	o.LastError = ""
	l.limitOrderFills.Add(1)
	partial := o.FilledCents < o.AmountCents
	if !partial {
		o.State = LimitOrderFilled
	}
	l.emitAudit(AuditEvent{
		Op:       "fx_limit_order_fill",
		Actor:    "MatchLimitOrders",
		TraceID:  o.ID,
		Accounts: []AccountID{o.FromAccount, o.ToAccount},
		EntryIDs: []string{fillID},
		Details: map[string]any{
			"order_id":        o.ID,
			"fill_seq":        seq,
			"fill_cents":      fillCents,
			"filled_cents":    o.FilledCents,
			"remaining_cents": o.RemainingCents(),
			"partial":         partial,
			"side":            side,
			"rate_num":        eff.Num,
			"rate_den":        eff.Den,
		},
	})
	return true
}

// LimitOrderFills reports how many FX limit-order fills posted (see
// MatchLimitOrders): every fill, partial or final. Operational metric
// (surfaced on GET /metrics as ledger_fx_limit_order_fills_total), not
// books: in-memory only, does not round-trip through snapshots.
func (l *Ledger) LimitOrderFills() uint64 {
	return l.limitOrderFills.Load()
}

// LimitOrderCancels reports how many FX limit orders left the book
// without filling: manual cancels plus sweep auto-expiries. Operational
// metric (surfaced on GET /metrics as
// ledger_fx_limit_order_cancels_total), not books: in-memory only, does
// not round-trip through snapshots.
func (l *Ledger) LimitOrderCancels() uint64 {
	return l.limitOrderCancels.Load()
}

// fxLimitOrdersSnapshotLocked returns the book sorted by (CreatedAt,
// ID) for the snapshot meta record. Callers must hold l.mu (the read
// lock suffices).
func (l *Ledger) fxLimitOrdersSnapshotLocked() []LimitOrder {
	out := l.limitOrdersLocked()
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// limitOrdersEqualLocked reports whether two ledgers carry the same FX
// limit-order book: same IDs and same per-order state. Times are
// compared by instant (see journalEntriesEqual): a snapshot round trip
// drops the monotonic clock reading, so == would report a false
// difference. Callers must hold both ledgers' read locks (used inside
// snapshotLedgersEqual).
func limitOrdersEqualLocked(a, b *Ledger) bool {
	if len(a.limitOrders) != len(b.limitOrders) {
		return false
	}
	for id, ca := range a.limitOrders {
		cb, ok := b.limitOrders[id]
		if !ok {
			return false
		}
		cc, cd := *ca, *cb
		cc.CreatedAt, cd.CreatedAt = time.Time{}, time.Time{}
		cc.Expiry, cd.Expiry = time.Time{}, time.Time{}
		if cc != cd {
			return false
		}
		if !ca.CreatedAt.Equal(cb.CreatedAt) || !ca.Expiry.Equal(cb.Expiry) {
			return false
		}
	}
	return true
}
