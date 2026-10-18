package ledger

import (
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"
)

// FX forward contracts (LG-110).
//
// LG-86's FXRateQuote locks the rate only for a single request's
// admission window: it answers "what price did this one transfer get?".
// It cannot express a bilateral agreement to convert at a fixed rate on
// a future date — the desk and the customer agree a rate today, the
// customer is guaranteed that rate at settlement, and market moves in
// between are the desk's risk, not the customer's. That is the forward
// contract: BookForward records the agreement, SettleForward executes it
// at the locked rate.
//
// The locked rate is exact (a reduced *big.Rat at booking, stored as
// int64 num/den like every other rate in this ledger — float64 never
// touches money) and frozen for the contract's life: a later SetFXRate
// cannot re-price a booked forward. Settlement reuses the ordinary
// cross-currency transfer path (see fx.go) with the contract's price
// pinned on the transfer (see forwardLockedRate): the frozen/overdraft/
// daily-limit/budget/period checks run in their usual order before
// anything books — the task's "frozen 403/overdraft 422 in front" — the
// journal legs, the audit event, the outbox envelope, and the
// settlement-cutoff handling are exactly a normal FX transfer's, and
// the receipt discloses slippage_bps=0 with admitted == committed ==
// locked rate. The slippage is zero by construction, not by
// measurement: the customer never faced the live price, so there is no
// drift to measure — reporting the live rate's drift as "slippage"
// would misstate what the customer got.
//
// Lifecycle: booked -> settled | cancelled. Expiry is lazy: a booked
// contract past its settlement grace period (ForwardSettlementGracePeriod
// after SettleAt) is lapsed. The explicit ExpireForwards(now) sweep
// persists the transition; SettleForward, CancelForward, and the Forward
// getter apply the same check first, so a lapsed contract can never be
// settled or re-cancelled — it reports expired. A lapsed contract moves
// to the cancelled terminal state with reason "expired" (there is no
// separate "expired" state: expiry IS cancellation, distinguished only
// by the fx_forward_expired audit event and the CancelReason field —
// one terminal state keeps the state machine, the snapshot schema, and
// every consumer's switch statement small).
//
// Cancel and expiry record no journal entries — the money never moved,
// so there is nothing to book — only their domain audit events
// (fx_forward_cancelled / fx_forward_expired), sealed like every other
// audit event. (The audit log seals every event by construction; an
// unsealed line would fail VerifyAuditLog as missing_hash. "No chain
// links" for cancel/expiry means no journal-entry postings were created,
// not that the events skip the seal.)
//
// The forward ID doubles as the booking idempotency key: replaying a
// BookForward with a known ID returns the recorded contract, exactly
// like the transfer idempotency replay — even when the replay's
// parameters differ, because the replay books nothing new. The
// settlement transfer reuses the forward ID as its transfer ID, so one
// identity traces booking -> journal -> audit.
//
// Rate provenance follows the LG-45 convention: BookForward records the
// current fxSnapshotVersion on the contract, so an auditor can pull
// FXRateTableSnapshot(version) for the market table the agreement was
// priced against, and the contract round-trips through
// ExportSnapshot/ImportSnapshot like the rate table itself. The locked
// rate is NOT the market rate — it is the bilateral agreement — so the
// receipt's RateNum/RateDen name the locked price while
// RateSnapshotVersion names the market table at booking.
//
// Concurrency follows the ledger's usual rule: the public methods take
// the write lock; the locked variants assume it is held.

// ForwardSettlementGracePeriod is how long after SettleAt a booked
// forward may still settle before it is considered lapsed: the value
// date is the agreement, and the grace covers operational delay (a
// settlement that misses the value date by minutes is still the same
// economic event; one that misses it by a day is stale). Past the
// grace, SettleForward reports ErrForwardExpired and the contract
// lapses to cancelled (reason "expired").
const ForwardSettlementGracePeriod = 24 * time.Hour

// Forward lifecycle states.
const (
	// ForwardBooked is a recorded agreement awaiting settlement.
	ForwardBooked = "booked"
	// ForwardSettled is a contract whose settlement transfer posted.
	ForwardSettled = "settled"
	// ForwardCancelled is a terminal contract: manually cancelled or
	// lapsed past the settlement grace period (see CancelReason).
	ForwardCancelled = "cancelled"
)

// Forward cancel reasons: how a contract reached the cancelled state.
const (
	// ForwardCancelManual is an operator CancelForward call.
	ForwardCancelManual = "manual"
	// ForwardCancelExpired is a lapse past the settlement grace period
	// (ExpireForwards sweep or a lazy-expiry check).
	ForwardCancelExpired = "expired"
)

// Validation and lifecycle errors for FX forward contracts.
var (
	// ErrForwardInvalid is returned for a malformed booking: empty ID,
	// bad currency codes, equal currencies, empty/equal accounts, a
	// non-positive rate, a non-positive notional, a negative cap, a
	// settle_at that is not in the future, or a bad FX side. It is a
	// 400-class field validation error: the request is malformed, not
	// semantically rejected.
	ErrForwardInvalid = errors.New("ledger: invalid FX forward booking")
	// ErrForwardNotFound is returned when no contract exists for the
	// given forward ID. It is a 404-class error.
	ErrForwardNotFound = errors.New("ledger: FX forward contract not found")
	// ErrForwardNotBooked is returned when an operation needs a booked
	// contract and finds a settled or cancelled one. It is a 409-class
	// error: the contract's lifecycle already moved on.
	ErrForwardNotBooked = errors.New("ledger: FX forward is not in booked state")
	// ErrForwardNotSettleable is returned by SettleForward before
	// SettleAt: the value date has not arrived. It is a 422-class
	// semantic rejection: the request is well-formed, the time is not.
	ErrForwardNotSettleable = errors.New("ledger: FX forward is not yet settleable")
	// ErrForwardExpired is returned when a booked contract is past its
	// settlement grace period: it has lapsed to cancelled (reason
	// "expired") and can no longer settle. It is a 410-class error:
	// the agreement existed and is now gone.
	ErrForwardExpired = errors.New("ledger: FX forward contract has expired")
	// ErrForwardCapExceeded is returned when the settlement amount
	// exceeds the contract's opt-in notional cap. It is a 422-class
	// semantic rejection; the contract stays booked.
	ErrForwardCapExceeded = errors.New("ledger: FX forward settlement amount exceeds notional cap")
	// ErrForwardReviewRouted is returned when the settlement transfer
	// would route into dual-control review: the contract stays booked
	// and nothing is recorded. A forward is a bilateral rate agreement
	// with a settlement-timing promise; parking its settlement in an
	// approval queue breaks that promise, so settlement fails closed
	// and the operator decides (cancel, or settle the obligation
	// through the review flow manually). It is a 422-class rejection.
	ErrForwardReviewRouted = errors.New("ledger: FX forward settlement would route to dual-control review")
)

// FXForward is one FX forward contract: a bilateral agreement to convert
// NotionalCents of FromCurrency into ToCurrency at the locked RateNum/
// RateDen on SettleAt. The rate is the agreed price, frozen at booking —
// not a market rate lookup — and the settlement converts at exactly
// this price (see the package comment).
type FXForward struct {
	// ID is the contract's identity: the booking idempotency key and
	// the settlement transfer's ID, so booking -> journal -> audit
	// trace through one identifier.
	ID string `json:"id"`
	// FromAccount is the payer (the source-currency account debited at
	// settlement); ToAccount is the payee (credited the converted
	// amount in the settlement currency).
	FromAccount AccountID `json:"from_account"`
	ToAccount   AccountID `json:"to_account"`
	// FromCurrency is the source currency, ToCurrency the settlement
	// currency: normalized 3-letter uppercase ISO 4217 codes, always
	// different.
	FromCurrency string `json:"from_currency"`
	ToCurrency   string `json:"to_currency"`
	// RateNum/RateDen is the locked rate: converting X source cents
	// books floor(X * RateNum / RateDen) target cents. The exact
	// reduced ratio from the booking's *big.Rat — no float64.
	RateNum int64 `json:"rate_num"`
	RateDen int64 `json:"rate_den"`
	// RateSnapshotVersion pins the booking to one versioned copy of
	// the market rate table (the LG-45 convention, see fx_snapshot.go):
	// FXRateTableSnapshot(RateSnapshotVersion) reproduces the market
	// table the agreement was priced against.
	RateSnapshotVersion uint64 `json:"rate_snapshot_version"`
	// NotionalCents is the source-currency amount converted at
	// settlement; NotionalCapCents is the opt-in cap (0 = none): a
	// settlement whose notional exceeds it is rejected with
	// ErrForwardCapExceeded.
	NotionalCents    int64 `json:"notional_cents"`
	NotionalCapCents int64 `json:"notional_cap_cents,omitempty"`
	// SettleAt is the value date: SettleForward before it is rejected
	// (ErrForwardNotSettleable); past SettleAt + the grace period the
	// contract lapses (ErrForwardExpired).
	SettleAt time.Time `json:"settle_at"`
	// FXSide is the dealing side the agreement was struck on
	// ("buy"/"sell", default "sell" — see fx_spread.go): disclosure on
	// the settlement receipt. The locked price is side-agnostic (one
	// agreed price, no spread); the side records the commercial terms.
	FXSide string `json:"fx_side,omitempty"`
	// Status is the lifecycle state: "booked", "settled", "cancelled".
	Status string `json:"status"`
	// CancelReason is "manual" or "expired" once Status is "cancelled";
	// empty otherwise.
	CancelReason string `json:"cancel_reason,omitempty"`
	// BookedAt is when the agreement was recorded; BookedVersion is
	// the ledger version then (structural provenance, like
	// ExchangeRate.EffectiveVersion).
	BookedAt      time.Time `json:"booked_at"`
	BookedVersion uint64    `json:"booked_version"`
	// SettledTransferID is the settlement transfer's ID once Status is
	// "settled" (it equals the forward ID); SettledAt is when it
	// posted. Empty before settlement.
	SettledTransferID string    `json:"settled_transfer_id,omitempty"`
	SettledAt         time.Time `json:"settled_at,omitempty"`
	// Rolls is the roll sequence number (LG-117): 0 for originally
	// booked contracts, n for the contract created by the n-th
	// rollover of its lineage. The roll key "roll/<id>/<n>" derives
	// from it, so a retried roll replays to the same contract.
	Rolls int `json:"rolls,omitempty"`
	// RolledFromID names the contract this one was rolled from; empty
	// for originally booked contracts.
	RolledFromID string `json:"rolled_from_id,omitempty"`
}

// copy returns a deep copy: callers can never mutate the ledger's
// stored contract through a returned pointer.
func (c *FXForward) copy() *FXForward {
	out := *c
	return &out
}

// ForwardBooking is the parameters of one BookForward call.
type ForwardBooking struct {
	// ID is the contract's identity and its idempotency key: replaying
	// a booking with a known ID returns the recorded contract.
	ID string
	// From is the payer account, To the payee account; they must
	// differ and be non-empty.
	From AccountID
	To   AccountID
	// FromCurrency is the source currency, ToCurrency the settlement
	// currency: 3-letter uppercase ISO 4217 codes, different from each
	// other. Empty is rejected here (unlike transfers, a forward has
	// no "default currency" — the pair is the agreement's core term).
	FromCurrency string
	ToCurrency   string
	// Rate is the agreed locked rate: exact, positive, no float. It is
	// copied at booking — the caller's Rat is never aliased.
	Rate *big.Rat
	// NotionalCents is the source-currency amount converted at
	// settlement (positive); NotionalCapCents is the opt-in cap (0 =
	// none): the notional may not exceed it.
	NotionalCents    int64
	NotionalCapCents int64
	// SettleAt is the value date and must be in the future at booking.
	SettleAt time.Time
	// FXSide is "buy"/"sell" (default "sell"): the dealing side the
	// agreement was struck on, carried as disclosure on the settlement
	// receipt.
	FXSide string
}

// forwardLockedRate pins a cross-currency transfer's conversion to a
// contractually agreed price (see the package comment): it is set only
// by SettleForward, never by callers — an ordinary transfer cannot
// smuggle a locked price past the rate table. Unexported, so it stays
// out of the JSON API and snapshots; it lives only for the duration of
// the settlement call.
type forwardLockedRate struct {
	Num, Den int64
	// RateSnapshotVersion is the contract's pinned market-table
	// version (the LG-45 convention): the settlement receipt discloses
	// it instead of the live version.
	RateSnapshotVersion uint64
	// ForwardID is the contract being settled, disclosed on the
	// transfer's audit event.
	ForwardID string
	// BookedAt is the contract's booking time: the synthesized quote's
	// admitted_at, so the quote's provenance names the agreement, not
	// a live admission (a forward has no admission window).
	BookedAt time.Time
}

// forwardLockedEffectiveRate synthesizes the effectiveFXRate the locked
// price converts at (see effectiveFXRateLocked in fx.go): the locked
// price is the whole quote — mid = bid = ask = locked, because a single
// agreed price has no spread — and it carries no table version or
// expiry, because it is not a table rate.
func forwardLockedEffectiveRate(fl *forwardLockedRate, from, to string) effectiveFXRate {
	return effectiveFXRate{
		Rate:             ExchangeRate{FromCurrency: from, ToCurrency: to, Num: fl.Num, Den: fl.Den},
		MidNum:           fl.Num,
		MidDen:           fl.Den,
		BidNum:           fl.Num,
		BidDen:           fl.Den,
		AskNum:           fl.Num,
		AskDen:           fl.Den,
		Num:              fl.Num,
		Den:              fl.Den,
		EffectiveVersion: 0,
	}
}

// quote synthesizes the FXRateQuote the locked settlement discloses
// (see FXRateQuote in fx.go): admitted == committed == locked, because
// the agreement IS the quote — there was no admission window, only the
// bilateral price. Slippage against it is therefore zero by
// construction, never measured.
func (fl *forwardLockedRate) quote(from, to string) FXRateQuote {
	return FXRateQuote{
		From:                from,
		To:                  to,
		Num:                 fl.Num,
		Den:                 fl.Den,
		BidNum:              fl.Num,
		BidDen:              fl.Den,
		AskNum:              fl.Num,
		AskDen:              fl.Den,
		MidNum:              fl.Num,
		MidDen:              fl.Den,
		AdmittedAt:          fl.BookedAt,
		RateSnapshotVersion: fl.RateSnapshotVersion,
	}
}

// BookForward records an FX forward contract: the bilateral agreement
// to convert b.NotionalCents of b.FromCurrency into b.ToCurrency at
// b.Rate on b.SettleAt. The rate is frozen at booking — a later
// SetFXRate cannot re-price the contract — and the current
// fxSnapshotVersion is pinned on it (the LG-45 convention).
//
// The forward ID is the booking idempotency key: replaying a booking
// with a known ID returns the recorded contract, mirroring the
// transfer idempotency replay — even when the replay's parameters
// differ, because the replay books nothing new. Booking records no
// journal entries (no money moves yet): only the fx_forward_booked
// audit event.
//
// Booking is field validation only: frozen accounts, overdraft, and
// the other risk gates run at settlement, when money actually moves.
func (l *Ledger) BookForward(b ForwardBooking) (*FXForward, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.bookForwardLocked(b, time.Now())
}

// bookForwardLocked is the lock-held core of BookForward; now is the
// booking time (time.Now() from BookForward, pinned in tests). Callers
// must hold the write lock.
func (l *Ledger) bookForwardLocked(b ForwardBooking, now time.Time) (*FXForward, error) {
	// Idempotency first, mirroring PostTransfer: a known forward ID
	// returns the recorded contract instead of booking again.
	if c, ok := l.forwards[b.ID]; ok {
		return c.copy(), nil
	}
	if b.ID == "" {
		return nil, fmt.Errorf("%w: forward ID must not be empty", ErrForwardInvalid)
	}
	from, err := normalizeCurrency(b.FromCurrency)
	if err != nil || b.FromCurrency == "" {
		return nil, fmt.Errorf("%w: from currency %q", ErrForwardInvalid, b.FromCurrency)
	}
	to, err := normalizeCurrency(b.ToCurrency)
	if err != nil || b.ToCurrency == "" {
		return nil, fmt.Errorf("%w: to currency %q", ErrForwardInvalid, b.ToCurrency)
	}
	if from == to {
		return nil, fmt.Errorf("%w: from and to currencies must differ", ErrForwardInvalid)
	}
	if b.From == "" {
		return nil, fmt.Errorf("%w: from account must not be empty", ErrForwardInvalid)
	}
	if b.To == "" {
		return nil, fmt.Errorf("%w: to account must not be empty", ErrForwardInvalid)
	}
	if b.From == b.To {
		return nil, fmt.Errorf("%w: from and to accounts must differ", ErrForwardInvalid)
	}
	if b.Rate == nil || b.Rate.Sign() <= 0 {
		return nil, fmt.Errorf("%w: rate must be positive", ErrForwardInvalid)
	}
	// Num/Denom are the reduced numerator and denominator: the exact
	// ratio, with no float rounding anywhere in the pipeline (the same
	// convention as SetFXRateRat).
	num, den := b.Rate.Num(), b.Rate.Denom()
	if !num.IsInt64() || !den.IsInt64() {
		return nil, fmt.Errorf("%w: rate %s/%s does not fit int64", ErrForwardInvalid, num, den)
	}
	if b.NotionalCents <= 0 {
		return nil, fmt.Errorf("%w: notional must be positive", ErrForwardInvalid)
	}
	if b.NotionalCapCents < 0 {
		return nil, fmt.Errorf("%w: notional cap must not be negative", ErrForwardInvalid)
	}
	if b.NotionalCapCents > 0 && b.NotionalCents > b.NotionalCapCents {
		return nil, fmt.Errorf("%w: notional %d exceeds cap %d", ErrForwardCapExceeded, b.NotionalCents, b.NotionalCapCents)
	}
	if b.SettleAt.IsZero() || !now.Before(b.SettleAt) {
		return nil, fmt.Errorf("%w: settle_at must be in the future", ErrForwardInvalid)
	}
	side := strings.ToLower(b.FXSide)
	switch side {
	case "", FXSideBuy, FXSideSell:
		if side == "" {
			side = FXSideSell
		}
	default:
		return nil, fmt.Errorf("%w: fx side must be %q or %q", ErrForwardInvalid, FXSideBuy, FXSideSell)
	}
	// The settlement transfer reuses the forward ID as its transfer
	// ID: fail fast at booking when the ID is already a journal entry,
	// instead of failing at settlement time.
	if _, exists := l.entries[b.ID]; exists {
		return nil, ErrTransferIDConflict
	}
	c := &FXForward{
		ID:                  b.ID,
		FromAccount:         b.From,
		ToAccount:           b.To,
		FromCurrency:        from,
		ToCurrency:          to,
		RateNum:             num.Int64(),
		RateDen:             den.Int64(),
		RateSnapshotVersion: l.fxSnapshotVersion,
		NotionalCents:       b.NotionalCents,
		NotionalCapCents:    b.NotionalCapCents,
		SettleAt:            b.SettleAt.UTC(),
		FXSide:              side,
		Status:              ForwardBooked,
		BookedAt:            now.UTC(),
		BookedVersion:       l.version,
	}
	l.forwards[b.ID] = c
	details := map[string]any{
		"forward_id":            b.ID,
		"from_currency":         from,
		"to_currency":           to,
		"locked_rate":           fmt.Sprintf("%d/%d", c.RateNum, c.RateDen),
		"rate_snapshot_version": c.RateSnapshotVersion,
		"notional_cents":        c.NotionalCents,
		"settle_at":             c.SettleAt.Format(time.RFC3339),
		"fx_side":               side,
	}
	if c.NotionalCapCents > 0 {
		details["notional_cap_cents"] = c.NotionalCapCents
	}
	l.emitAudit(AuditEvent{
		Op:            "fx_forward_booked",
		Actor:         "BookForward",
		TraceID:       b.ID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{b.From, b.To},
		Details:       details,
	})
	return c.copy(), nil
}

// SettleForward executes the forward's settlement at the locked rate:
// now must be at or past SettleAt (earlier is ErrForwardNotSettleable),
// and a contract past its settlement grace period has lapsed
// (ErrForwardExpired — the lazy-expiry check runs first, so a lapsed
// contract can never settle).
//
// With auto-rollover enabled (LG-117, see forward_roll.go), a booked
// contract inside its roll window rolls into a replacement contract at
// the current market price instead of settling, and the call reports
// ErrForwardRolled naming the new contract: the obligation was
// extended, not settled.
//
// The settlement is an ordinary cross-currency transfer at the locked
// price (see the package comment): the frozen/overdraft/daily-limit/
// budget/period checks run in their usual order before anything books,
// the journal legs are the standard "<id>" + "<id>/fx" pair (the
// transfer reuses the forward ID), and the receipt discloses
// slippage_bps=0 with admitted == committed == locked rate. A notional
// above the opt-in cap is rejected (ErrForwardCapExceeded); a
// settlement that would route into dual-control review is rejected
// up front (ErrForwardReviewRouted) — the contract stays booked and
// nothing is recorded.
//
// On success the contract moves to settled and the fx_forward_settled
// audit event binds it to the settlement transfer.
func (l *Ledger) SettleForward(id string) (TransferReceipt, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.settleForwardLocked(id, time.Now())
}

// settleForwardLocked is the lock-held core of SettleForward; now is
// the settlement time. Callers must hold the write lock.
func (l *Ledger) settleForwardLocked(id string, now time.Time) (TransferReceipt, error) {
	// Lazy expiry first: a lapsed contract reports expired, never
	// settles — the sweep persists the same transition, this just
	// applies it on the settlement path too.
	l.expireForwardsLocked(now)
	c, ok := l.forwards[id]
	if !ok {
		return TransferReceipt{}, ErrForwardNotFound
	}
	if c.Status != ForwardBooked {
		// A contract the lazy sweep just lapsed (or one a previous
		// sweep lapsed) reports expired, not merely "not booked".
		if c.CancelReason == ForwardCancelExpired {
			// ...unless it lapsed because it was rolled: a retried
			// settlement after a roll replays to the same
			// replacement (idempotent), instead of failing with
			// ErrForwardExpired. The RolledFromID check in
			// rolledReplayLocked keeps genuine expiries on the
			// expired path.
			if rolled, ok := l.rolledReplayLocked(id); ok {
				return TransferReceipt{}, fmt.Errorf("%w: forward %q rolled to %q (settle_at %s)",
					ErrForwardRolled, id, rolled.ID, rolled.SettleAt.UTC().Format(time.RFC3339))
			}
			return TransferReceipt{}, fmt.Errorf("%w: forward %q lapsed at %s", ErrForwardExpired, id, c.SettleAt.UTC().Format(time.RFC3339))
		}
		return TransferReceipt{}, fmt.Errorf("%w: forward %q is %s", ErrForwardNotBooked, id, c.Status)
	}
	// Opt-in auto-rollover (LG-117): a booked contract inside its roll
	// window rolls into a replacement at the current market price
	// instead of settling. A rolled settlement reports ErrForwardRolled
	// naming the new contract: the obligation was extended, not
	// settled — settle the replacement when its value date arrives.
	if newC, rolled, err := l.maybeRollForwardLocked(c, now); err != nil {
		return TransferReceipt{}, err
	} else if rolled {
		return TransferReceipt{}, fmt.Errorf("%w: forward %q rolled to %q (settle_at %s)",
			ErrForwardRolled, id, newC.ID, newC.SettleAt.UTC().Format(time.RFC3339))
	}
	if now.Before(c.SettleAt) {
		return TransferReceipt{}, fmt.Errorf("%w: forward %q settles at %s", ErrForwardNotSettleable, id, c.SettleAt.UTC().Format(time.RFC3339))
	}
	if c.NotionalCapCents > 0 && c.NotionalCents > c.NotionalCapCents {
		return TransferReceipt{}, fmt.Errorf("%w: notional %d exceeds cap %d", ErrForwardCapExceeded, c.NotionalCents, c.NotionalCapCents)
	}
	t := Transfer{
		ID:             id,
		From:           c.FromAccount,
		To:             c.ToAccount,
		AmountCents:    c.NotionalCents,
		Currency:       c.FromCurrency,
		ToCurrency:     c.ToCurrency,
		FXSide:         c.FXSide,
		IdempotencyKey: "fx-forward-settle/" + id,
		Memo:           "FX forward " + id + " settlement",
		forwardLocked: &forwardLockedRate{
			Num:                 c.RateNum,
			Den:                 c.RateDen,
			RateSnapshotVersion: c.RateSnapshotVersion,
			ForwardID:           id,
			BookedAt:            c.BookedAt,
		},
	}
	// Fail-closed on the dual-control review gate (see review.go): the
	// predicate mirrors maybeReviewTransferLocked exactly — a
	// settlement at or above the payer's review threshold would freeze
	// instead of settling, which breaks the forward's settlement-timing
	// promise, so it is rejected up front with the contract staying
	// booked and nothing recorded. Checked under the same write lock
	// as the posting below, so the threshold cannot change between the
	// check and the commit.
	if threshold, ok := l.effectiveReviewThresholdLocked(t.From); ok && t.AmountCents >= threshold {
		return TransferReceipt{}, fmt.Errorf("%w: notional %d at or above review threshold %d", ErrForwardReviewRouted, t.AmountCents, threshold)
	}
	receipt, err := l.postTransferLocked(t, FXRateQuote{})
	if err != nil {
		return TransferReceipt{}, err
	}
	// The review gate was pre-checked above, so a review-routed receipt
	// here would be a logic bug — fail closed rather than marking a
	// frozen settlement "settled".
	if receipt.ReviewStatus != "" {
		return TransferReceipt{}, fmt.Errorf("%w: unexpected review_status %q", ErrForwardReviewRouted, receipt.ReviewStatus)
	}
	converted := int64(0)
	if receipt.FX != nil {
		converted = receipt.FX.ConvertedCents
	}
	c.Status = ForwardSettled
	c.SettledTransferID = receipt.TransferID
	c.SettledAt = now.UTC()
	entryIDs := make([]string, 0, len(receipt.Entries))
	for _, e := range receipt.Entries {
		entryIDs = append(entryIDs, e.ID)
	}
	l.emitAudit(AuditEvent{
		Op:            "fx_forward_settled",
		Actor:         "SettleForward",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		EntryIDs:      entryIDs,
		Accounts:      []AccountID{c.FromAccount, c.ToAccount},
		Details: map[string]any{
			"forward_id":            id,
			"settled_transfer_id":   receipt.TransferID,
			"locked_rate":           fmt.Sprintf("%d/%d", c.RateNum, c.RateDen),
			"source_cents":          c.NotionalCents,
			"converted_cents":       converted,
			"slippage_bps":          0,
			"rate_snapshot_version": c.RateSnapshotVersion,
			"fx_side":               c.FXSide,
		},
	})
	return receipt, nil
}

// CancelForward cancels a booked forward: the agreement is voided and
// the contract moves to the cancelled terminal state (reason
// "manual"). Cancelling records no journal entries — the money never
// moved, so there is nothing to book — only the fx_forward_cancelled
// audit event. A settled or already-cancelled contract reports
// ErrForwardNotBooked; a lapsed contract reports ErrForwardExpired
// (the lazy-expiry check runs first: the operator cannot "cancel" what
// already lapsed).
func (l *Ledger) CancelForward(id string) (*FXForward, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cancelForwardLocked(id, time.Now())
}

// cancelForwardLocked is the lock-held core of CancelForward; now is
// the cancellation time. Callers must hold the write lock.
func (l *Ledger) cancelForwardLocked(id string, now time.Time) (*FXForward, error) {
	// Lazy expiry first, like SettleForward: a lapsed contract reports
	// expired instead of cancelling.
	l.expireForwardsLocked(now)
	c, ok := l.forwards[id]
	if !ok {
		return nil, ErrForwardNotFound
	}
	if c.Status != ForwardBooked {
		// A contract the lazy sweep just lapsed (or one a previous
		// sweep lapsed) reports expired, not merely "not booked".
		if c.CancelReason == ForwardCancelExpired {
			return nil, fmt.Errorf("%w: forward %q lapsed at %s", ErrForwardExpired, id, c.SettleAt.UTC().Format(time.RFC3339))
		}
		return nil, fmt.Errorf("%w: forward %q is %s", ErrForwardNotBooked, id, c.Status)
	}
	c.Status = ForwardCancelled
	c.CancelReason = ForwardCancelManual
	l.emitAudit(AuditEvent{
		Op:            "fx_forward_cancelled",
		Actor:         "CancelForward",
		TraceID:       id,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{c.FromAccount, c.ToAccount},
		Details: map[string]any{
			"forward_id":    id,
			"cancel_reason": ForwardCancelManual,
		},
	})
	return c.copy(), nil
}

// ExpireForwards lapses every booked forward past its settlement grace
// period as of now: each moves to the cancelled terminal state (reason
// "expired") with an fx_forward_expired audit event, and the call
// returns how many lapsed. Expiry records no journal entries — the
// money never moved — and bumps no ledger version (a structural state
// transition, like the fx_rate_change event). It is idempotent: only
// booked contracts transition, so a second sweep changes nothing.
func (l *Ledger) ExpireForwards(now time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.expireForwardsLocked(now)
}

// expireForwardsLocked is the lock-held core of ExpireForwards.
// Callers must hold the write lock.
func (l *Ledger) expireForwardsLocked(now time.Time) int {
	expired := 0
	for _, c := range l.forwards {
		if c.Status != ForwardBooked || !forwardLapsed(c, now) {
			continue
		}
		c.Status = ForwardCancelled
		c.CancelReason = ForwardCancelExpired
		expired++
		l.emitAudit(AuditEvent{
			Op:            "fx_forward_expired",
			Actor:         "ExpireForwards",
			TraceID:       c.ID,
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Accounts:      []AccountID{c.FromAccount, c.ToAccount},
			Details: map[string]any{
				"forward_id":   c.ID,
				"settle_at":    c.SettleAt.UTC().Format(time.RFC3339),
				"grace_period": ForwardSettlementGracePeriod.String(),
			},
		})
	}
	return expired
}

// forwardLapsed reports whether a booked contract is past its
// settlement grace period as of now: SettleAt + the grace is the last
// instant it may still settle.
func forwardLapsed(c *FXForward, now time.Time) bool {
	return !now.Before(c.SettleAt.Add(ForwardSettlementGracePeriod))
}

// forwardEffectiveStatus returns the contract's status as of now: a
// booked contract past its settlement grace period reads as expired
// (the ExpireForwards sweep persists the same transition). Read-only —
// used by the Reconcile forward_book section, which must not mutate.
func forwardEffectiveStatus(c *FXForward, now time.Time) (status string, expired bool) {
	if c.Status == ForwardBooked && forwardLapsed(c, now) {
		return ForwardCancelled, true
	}
	return c.Status, false
}

// Forward returns the contract's current view. The lazy expiry runs
// first, so a lapsed contract reads as cancelled (reason "expired")
// even before an explicit ExpireForwards sweep — the transition is
// persisted, exactly as the sweep would persist it.
func (l *Ledger) Forward(id string) (*FXForward, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.expireForwardsLocked(time.Now())
	c, ok := l.forwards[id]
	if !ok {
		return nil, false
	}
	return c.copy(), true
}

// FXForwardPairTotals is the per-pair breakdown of forward contracts in
// the Reconcile forward_book section: currencies are never summed
// together, so each directional pair gets its own row, sorted by
// (from, to).
type FXForwardPairTotals struct {
	FromCurrency string `json:"from_currency"`
	ToCurrency   string `json:"to_currency"`
	Booked       int    `json:"booked"`
	Settled      int    `json:"settled"`
}

// FXForwardBookReport is the Reconcile view of the FX forward-contract
// book (the "forward_book" section): how many contracts exist and in
// which lifecycle state. Expired counts contracts that lapsed past the
// settlement grace period (terminal state "cancelled", reason
// "expired"); Cancelled counts operator-cancelled ones.
type FXForwardBookReport struct {
	Total     int                   `json:"total"`
	Booked    int                   `json:"booked"`
	Settled   int                   `json:"settled"`
	Expired   int                   `json:"expired"`
	Cancelled int                   `json:"cancelled"`
	ByPair    []FXForwardPairTotals `json:"by_pair,omitempty"`
}

// forwardBookReportLocked builds the Reconcile forward_book section.
// Expiry is lazy here (see forwardEffectiveStatus): a booked contract
// past its grace period counts as expired even before the sweep
// persists the transition — the same lazy-expiry convention as the
// hold totals. Callers must hold l.mu; the read lock suffices.
func (l *Ledger) forwardBookReportLocked(now time.Time) FXForwardBookReport {
	rep := FXForwardBookReport{}
	byPair := make(map[fxPair]*FXForwardPairTotals)
	for _, c := range l.forwards {
		rep.Total++
		status, expired := forwardEffectiveStatus(c, now)
		switch status {
		case ForwardBooked:
			rep.Booked++
		case ForwardSettled:
			rep.Settled++
		case ForwardCancelled:
			if expired || c.CancelReason == ForwardCancelExpired {
				rep.Expired++
			} else {
				rep.Cancelled++
			}
		}
		if status == ForwardBooked || status == ForwardSettled {
			p := fxPair{from: c.FromCurrency, to: c.ToCurrency}
			row := byPair[p]
			if row == nil {
				row = &FXForwardPairTotals{FromCurrency: p.from, ToCurrency: p.to}
				byPair[p] = row
			}
			if status == ForwardBooked {
				row.Booked++
			} else {
				row.Settled++
			}
		}
	}
	for _, row := range byPair {
		rep.ByPair = append(rep.ByPair, *row)
	}
	sort.Slice(rep.ByPair, func(i, j int) bool {
		if rep.ByPair[i].FromCurrency != rep.ByPair[j].FromCurrency {
			return rep.ByPair[i].FromCurrency < rep.ByPair[j].FromCurrency
		}
		return rep.ByPair[i].ToCurrency < rep.ByPair[j].ToCurrency
	})
	return rep
}

// forwardsLocked returns every stored contract sorted by ID, for
// snapshots and operator inspection. The returned contracts are
// copies: mutating them cannot affect the ledger. Callers must hold
// l.mu; the read lock suffices.
func (l *Ledger) forwardsLocked() []*FXForward {
	out := make([]*FXForward, 0, len(l.forwards))
	for _, c := range l.forwards {
		out = append(out, c.copy())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// fxForwardsSnapshotLocked returns every stored contract sorted by ID
// as values, for the snapshot config line. Callers must hold l.mu;
// the read lock suffices.
func (l *Ledger) fxForwardsSnapshotLocked() []FXForward {
	ptrs := l.forwardsLocked()
	out := make([]FXForward, 0, len(ptrs))
	for _, p := range ptrs {
		out = append(out, *p)
	}
	return out
}

// forwardsEqualLocked reports whether two ledgers carry the same
// forward contracts: same IDs and same per-contract state. Times are
// compared by instant (see journalEntriesEqual): a snapshot round trip
// drops the monotonic clock reading, so == would report a false
// difference. Callers must hold both ledgers' read locks (used inside
// snapshotLedgersEqual).
func forwardsEqualLocked(a, b *Ledger) bool {
	if len(a.forwards) != len(b.forwards) {
		return false
	}
	for id, ca := range a.forwards {
		cb, ok := b.forwards[id]
		if !ok {
			return false
		}
		cc := *ca
		cd := *cb
		cc.SettleAt, cd.SettleAt = time.Time{}, time.Time{}
		cc.BookedAt, cd.BookedAt = time.Time{}, time.Time{}
		cc.SettledAt, cd.SettledAt = time.Time{}, time.Time{}
		if cc != cd {
			return false
		}
		if !ca.SettleAt.Equal(cb.SettleAt) || !ca.BookedAt.Equal(cb.BookedAt) || !ca.SettledAt.Equal(cb.SettledAt) {
			return false
		}
	}
	return true
}
