package ledger

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strings"
	"time"
)

// Two-sided FX dealing prices and spread P&L (LG-93).
//
// A real FX desk never deals at one price: it buys the customer's source
// currency at the BID and sells the target currency at the ASK, and the
// difference — the spread — is the desk's revenue. Until now the rate
// table held a single mid price (see fx.go): every conversion settled at
// Num/Den, and any implicit spread between the booked legs accumulated
// invisibly in the FX clearing account's per-currency balances. This file
// makes the spread explicit and double-entered.
//
// The model:
//
//   - ExchangeRate keeps Num/Den as the reference MID price (backward
//     compatible: snapshots, receipts, fx_consistency, and the provider
//     feed all keep speaking mid). Two optional dealing prices ride
//     alongside: BidNum/BidDen and AskNum/AskDen. A legacy mid-only rate
//     leaves both at zero and behaves exactly as before: bid = ask = mid,
//     zero spread, no new leg, byte-identical receipts.
//   - A cross-currency transfer names its dealing side
//     (Transfer.FXSide): "sell" (the default) means the customer sells
//     the source currency, so the conversion prices at BID;
//     "buy" means the customer buys the target currency, priced at ASK.
//     The side is validated at field validation (400 on garbage) and
//     ignored for same-currency transfers.
//   - When the spread is positive it books as its own double-entry leg,
//     ID "<transfer ID>/spread" (one aggregated leg per split transfer):
//     debit FXAccount@toCurrency spreadCents, credit
//     FXIncomeAccount@toCurrency spreadCents. Each currency's books stay
//     balanced independently — the leg debits and credits within the
//     target currency — and the dealer's revenue lands in a real income
//     account instead of hiding in the clearing account.
//   - The spread is measured against the mid, always dealer-favorable:
//     sell-side spread = floor(amount*mid) − floor(amount*bid) ≥ 0,
//     buy-side spread  = floor(amount*ask) − floor(amount*mid) ≥ 0.
//     The ≥ 0 is by construction: SetFXBidAskRate enforces
//     0 < bid ≤ mid ≤ ask as exact rationals, and the provider feed
//     installs mid-only rates (see fx_provider.go), so a moved mid can
//     never invert a dealing spread behind the operator's back.
//   - Fail-closed: a positive spread with no usable income account
//     (none configured, or the income account == the FX account, which
//     would book a self-leg) rejects the transfer with
//     ErrFXIncomeAccountNotConfigured (400-class) before any mutation —
//     a transfer must never settle while its P&L has nowhere to go.
//
// Spread dust: when the two-sided prices exist but the floored spread is
// zero (a tiny amount inside a tight spread), no leg books — a zero-cent
// journal row is noise, not accounting. The receipt still discloses the
// dealing prices and the zero spread.
//
// The spread leg is part of the transfer's atomic unit: it is built
// after every check passes, frozen by the dual-control review gate like
// the other legs (see maybeReviewTransferLocked), journaled with its own
// audit-chain link by commitEntryLocked, indexed under the transfer's
// idempotency key (replays return it with the other legs), and scanned
// by Reconcile's spread-P&L section (settled legs only — pending-review
// legs are unrealized).
//
// Exact integer math throughout: floor(amount*num/den) via
// fxConvertCents (128-bit intermediates), bid/ask derivation and the
// spread-bps disclosure via exact big.Rat. No float64 touches money.

// The two dealing sides of a cross-currency transfer (see Transfer.FXSide).
const (
	// FXSideSell (the default) prices the conversion at the pair's BID:
	// the customer sells the source currency to the desk.
	FXSideSell = "sell"
	// FXSideBuy prices the conversion at the pair's ASK: the customer
	// buys the target currency from the desk.
	FXSideBuy = "buy"
)

var (
	// ErrInvalidFXSide is returned at field validation when
	// Transfer.FXSide is anything but ""/"buy"/"sell" (case-insensitive).
	// It is a 400-class error: the caller can fix it by naming a valid
	// side. The side is ignored for same-currency transfers.
	ErrInvalidFXSide = errors.New("ledger: fx_side must be \"buy\" or \"sell\"")
	// ErrFXIncomeAccountNotConfigured is returned when a cross-currency
	// transfer earned a positive spread but no usable FX income account
	// exists: neither the transfer nor the ledger configured one, or the
	// configured one equals the FX clearing account (which would book a
	// self-leg: debit and credit the same account). It is a 400-class
	// error — the caller or operator can fix it — and it is checked
	// before any mutation, so the rejection records nothing: a transfer
	// never settles while its spread P&L has nowhere to go.
	ErrFXIncomeAccountNotConfigured = errors.New("ledger: FX spread requires an fx_income_account on the transfer or a configured FX income account distinct from the FX account")
)

// WithFXIncomeAccount configures the ledger-wide FX spread-income
// account: the credit counterparty of cross-currency spread legs (see
// the package comment). A transfer may override it with
// Transfer.FXIncomeAccount. It must differ from the FX clearing account
// whenever a spread books — enforced fail-closed at post time, not here,
// because a mid-only ledger legitimately configures none.
func WithFXIncomeAccount(a AccountID) Option {
	return func(l *Ledger) {
		l.fxIncomeAccount = a
	}
}

// fxSideOf resolves the dealing side of a cross-currency transfer:
// FXSideBuy converts at ask, anything else (""/FXSideSell, the default)
// at bid. The enum itself is validated in validateTransferFields;
// this is the canonical resolution both the admission quote and the
// commit path share.
func fxSideOf(t Transfer) string {
	if strings.EqualFold(t.FXSide, FXSideBuy) {
		return FXSideBuy
	}
	return FXSideSell
}

// SetFXBidAskRate installs (or replaces) the two-sided dealing prices
// for the directional pair (from, to): a "sell" transfer (the default)
// converts at bidNum/bidDen, a "buy" transfer at askNum/askDen, while
// Num/Den remains the reference mid price.
//
// Validation, all exact (big.Rat compares, no float):
//   - currencies normalize like SetFXRate; from and to must differ;
//   - bid and ask must be positive ratios with 0 < bid ≤ ask;
//   - the dealing spread must bracket the reference mid:
//     bid ≤ mid ≤ ask, where mid is the pair's existing Num/Den when the
//     pair already has a rate, or the exact midpoint (bid+ask)/2 when
//     the pair is new. A spread that does not bracket the mid is
//     rejected with ErrInvalidFXRate: the desk must not buy above (or
//     sell below) its own reference price — re-set the mid with
//     SetFXRate/SetFXRateRat first if the market moved.
//
// Like SetFXRate this is a structural change: it takes the write lock,
// does not bump the ledger version, snapshots the table (LG-45), and
// survives ExportSnapshot/ImportSnapshot (bid/ask included). An existing
// rate's expiry is preserved; a new pair never expires.
func (l *Ledger) SetFXBidAskRate(from, to string, bidNum, bidDen, askNum, askDen int64) error {
	f, t, err := normalizeFXRate(from, to, bidNum, bidDen)
	if err != nil {
		return err
	}
	if askNum <= 0 || askDen <= 0 {
		return fmt.Errorf("%w: ask ratio must be positive, got %d/%d", ErrInvalidFXRate, askNum, askDen)
	}
	bid := new(big.Rat).SetFrac64(bidNum, bidDen)
	ask := new(big.Rat).SetFrac64(askNum, askDen)
	if bid.Cmp(ask) > 0 {
		return fmt.Errorf("%w: bid %d/%d exceeds ask %d/%d", ErrInvalidFXRate, bidNum, bidDen, askNum, askDen)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	pair := fxPair{from: f, to: t}
	er, existed := l.fxRates[pair]
	if !existed {
		// No reference mid yet: the exact midpoint of the quote is the
		// honest mid — bid ≤ mid ≤ ask holds by construction, and the
		// reduced ratio must fit int64 like every other rate.
		mid := new(big.Rat).Add(bid, ask)
		mid.Quo(mid, big.NewRat(2, 1))
		mn, md := mid.Num(), mid.Denom()
		if !mn.IsInt64() || !md.IsInt64() {
			return fmt.Errorf("%w: derived mid %s/%s does not fit int64", ErrInvalidFXRate, mn, md)
		}
		er = ExchangeRate{FromCurrency: f, ToCurrency: t, Num: mn.Int64(), Den: md.Int64()}
	} else {
		mid := new(big.Rat).SetFrac64(er.Num, er.Den)
		if bid.Cmp(mid) > 0 || ask.Cmp(mid) < 0 {
			return fmt.Errorf("%w: dealing spread %d/%d-%d/%d does not bracket the reference mid %d/%d",
				ErrInvalidFXRate, bidNum, bidDen, askNum, askDen, er.Num, er.Den)
		}
	}
	er.BidNum, er.BidDen = bidNum, bidDen
	er.AskNum, er.AskDen = askNum, askDen
	er.EffectiveVersion = l.version
	l.fxRates[pair] = er
	// LG-45: the table changed — snapshot like SetFXRate does.
	l.snapshotFXRateTableLocked(fmt.Sprintf("set bid/ask %s->%s %d/%d-%d/%d", f, t, bidNum, bidDen, askNum, askDen))
	return nil
}

// TwoSidedFromMidSpread derives exact two-sided dealing prices from a
// reference mid price and a spread width: bid = mid·(1 − s/20000),
// ask = mid·(1 + s/20000), so the full bid-ask spread is exactly
// mid·s/10000 — spreadBps is the full spread in basis points of mid,
// split symmetrically around it.
//
// The prices are exact rationals (big.Rat, reduced to lowest terms):
// with a 100 bps spread on a 11/10 mid, bid is exactly 10945/10000 and
// ask exactly 11055/10000 — no float64 rounding anywhere. A zero spread
// returns bid = ask = mid. spreadBps must satisfy 0 ≤ spreadBps < 20000
// (20000 would zero the bid); the reduced prices must fit int64.
// Feed the result to SetFXBidAskRate.
func TwoSidedFromMidSpread(midNum, midDen int64, spreadBps int64) (bidNum, bidDen, askNum, askDen int64, err error) {
	fail := func(format string, args ...any) (int64, int64, int64, int64, error) {
		return 0, 0, 0, 0, fmt.Errorf("%w: "+format, append([]any{ErrInvalidFXRate}, args...)...)
	}
	if midNum <= 0 || midDen <= 0 {
		return fail("mid ratio must be positive, got %d/%d", midNum, midDen)
	}
	if spreadBps < 0 || spreadBps >= 20000 {
		return fail("spread_bps must satisfy 0 <= spread_bps < 20000, got %d", spreadBps)
	}
	mid := new(big.Rat).SetFrac64(midNum, midDen)
	half := new(big.Rat).SetFrac64(spreadBps, 20000)
	one := big.NewRat(1, 1)
	bid := new(big.Rat).Mul(mid, new(big.Rat).Sub(one, half))
	ask := new(big.Rat).Mul(mid, new(big.Rat).Add(one, half))
	bn, bd := bid.Num(), bid.Denom()
	an, ad := ask.Num(), ask.Denom()
	if !bn.IsInt64() || !bd.IsInt64() || !an.IsInt64() || !ad.IsInt64() {
		return fail("derived bid/ask do not fit int64")
	}
	return bn.Int64(), bd.Int64(), an.Int64(), ad.Int64(), nil
}

// fxSpreadBps measures the full bid-ask spread against the mid:
// floor((ask − bid)/mid · 10000), in basis points. Exact big.Rat math —
// no float64 touches money — matching the LG-69 override-deviation and
// LG-86 slippage conventions (see fxSlippageBps). All six inputs are
// positive ratios; the result saturates at math.MaxInt64 rather than
// overflowing on pathological inputs. A mid-only rate (bid = ask = mid)
// measures zero.
func fxSpreadBps(bidNum, bidDen, askNum, askDen, midNum, midDen int64) int64 {
	bid := new(big.Rat).SetFrac64(bidNum, bidDen)
	ask := new(big.Rat).SetFrac64(askNum, askDen)
	mid := new(big.Rat).SetFrac64(midNum, midDen)
	spread := new(big.Rat).Sub(ask, bid)
	spread.Quo(spread, mid)
	spread.Mul(spread, big.NewRat(10000, 1))
	// The spread is non-negative (bid ≤ ask by construction), so integer
	// division floors.
	bps := new(big.Int).Quo(spread.Num(), spread.Denom())
	if !bps.IsInt64() {
		return math.MaxInt64
	}
	return bps.Int64()
}

// fxSpreadCentsOf computes the dealer's spread on amount source-currency
// cents dealt at sideConverted (the floored conversion at the dealing
// price) against midConverted (the floored conversion at the reference
// mid): sell-side the desk keeps mid − bid, buy-side ask − mid. Both are
// ≥ 0 when the dealing spread brackets the mid (enforced by
// SetFXBidAskRate); the defensive floor keeps a hand-built table from
// ever booking a negative spread leg — a negative spread is a
// data-quality problem for the operator, never a journal row.
func fxSpreadCentsOf(side string, sideConverted, midConverted int64) int64 {
	var spread int64
	if side == FXSideBuy {
		spread = sideConverted - midConverted
	} else {
		spread = midConverted - sideConverted
	}
	if spread < 0 {
		spread = 0
	}
	return spread
}

// resolveFXIncomeAccountLocked resolves the FX spread-income account for
// a transfer earning spreadCents of spread: the transfer's override, or
// the ledger-wide account (see WithFXIncomeAccount). A non-positive
// spread needs no income account ("", nil). Fail-closed, before any
// mutation: an empty account, or one equal to the FX clearing account
// (which would debit and credit the same account — a self-leg), rejects
// with ErrFXIncomeAccountNotConfigured. Callers must hold l.mu; pure
// read, no mutation.
func (l *Ledger) resolveFXIncomeAccountLocked(t Transfer, fxAccount AccountID, spreadCents int64) (AccountID, error) {
	if spreadCents <= 0 {
		return "", nil
	}
	acct := t.FXIncomeAccount
	if acct == "" {
		acct = l.fxIncomeAccount
	}
	if acct == "" || acct == fxAccount {
		return "", ErrFXIncomeAccountNotConfigured
	}
	return acct, nil
}

// buildFXSpreadLeg builds the spread P&L leg of a cross-currency
// transfer: ID "<transfer ID>/spread", debit FXAccount@toCurrency,
// credit FXIncomeAccount@toCurrency, spreadCents. Single-currency by
// construction (target currency), like every other leg.
func buildFXSpreadLeg(transferID string, fxAccount, incomeAccount AccountID, spreadCents int64, toCurrency string, createdAt time.Time) JournalEntry {
	return JournalEntry{
		ID:            transferID + "/spread",
		DebitAccount:  fxAccount,
		CreditAccount: incomeAccount,
		AmountCents:   spreadCents,
		Currency:      toCurrency,
		CreatedAt:     createdAt,
	}
}

// FXSpreadBookingCount reports how many FX spread legs have been journaled
// since process start (direct commits and review-frozen legs alike — the
// row is booked in both cases; see the package comment). The HTTP layer
// syncs this into ledger_fx_spread_bookings_total on every /metrics
// scrape. In-memory only — it does not round-trip through snapshots.
func (l *Ledger) FXSpreadBookingCount() uint64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.fxSpreadBookings
}

// FXSpreadCentsTotal reports the total spread cents journaled since
// process start, across all settlement currencies (currencies are never
// summed on the books — this is an operational counter, not an
// accounting total; the per-currency P&L lives in Reconcile's
// fx_spread_pnl section). The HTTP layer syncs this into
// ledger_fx_spread_cents_total on every /metrics scrape. In-memory only.
func (l *Ledger) FXSpreadCentsTotal() int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.fxSpreadCentsTotal
}

// FXSpreadPNLTotals is one row of Reconcile's spread-P&L section: the
// dealer's realized FX spread income in one settlement currency,
// recovered from the journaled "<transfer ID>/spread" legs.
type FXSpreadPNLTotals struct {
	// Currency is the settlement (target) currency the spread was
	// booked in.
	Currency string `json:"currency"`
	// SpreadBookings counts the settled spread legs in this currency.
	SpreadBookings int `json:"spread_bookings"`
	// SpreadCentsTotal is the net spread income in this currency:
	// settled spread legs minus their settled direct reversals
	// ("<id>/spread/reversal"), so a reversed transfer un-books its
	// P&L instead of leaving phantom income.
	SpreadCentsTotal int64 `json:"spread_cents_total"`
}

// fxSpreadPNLLocked aggregates the spread P&L from the journal, for
// Reconcile. A spread leg is genuine when its ID ends in "/spread" and
// its base names a journaled transfer — the principal "<base>" for plain
// transfers, or a "<base>/chunk-<i>" principal for split transfers
// (which journal no "<base>" row). The base check keeps a caller-chosen
// transfer ID that merely ends in "/spread" from being misread as P&L,
// mirroring isFeeLegID's anchoring (see fee_split.go).
//
// Pending-review legs are excluded: the P&L is realized when the
// transfer settles, not when the dual-control gate freezes it. Direct
// reversals net out: a reversed spread leg contributed nothing.
//
// Callers must hold l.mu; the read lock suffices.
func (l *Ledger) fxSpreadPNLLocked() []FXSpreadPNLTotals {
	// Chunk bases first: split transfers journal "<base>/chunk-<i>"
	// principals, so a "<base>/spread" leg's base never appears as its
	// own row.
	chunkBases := make(map[string]bool)
	for id := range l.entries {
		if j := strings.Index(id, "/chunk-"); j >= 0 {
			chunkBases[id[:j]] = true
		}
	}
	byCurrency := make(map[string]*FXSpreadPNLTotals)
	for id, e := range l.entries {
		if !strings.HasSuffix(id, "/spread") {
			continue
		}
		base := strings.TrimSuffix(id, "/spread")
		if _, ok := l.entries[base]; !ok && !chunkBases[base] {
			continue
		}
		if e.PendingReview {
			continue
		}
		row := byCurrency[e.Currency]
		if row == nil {
			row = &FXSpreadPNLTotals{Currency: e.Currency}
			byCurrency[e.Currency] = row
		}
		row.SpreadBookings++
		row.SpreadCentsTotal += e.AmountCents
		// A settled direct reversal un-books the leg's P&L.
		if rev, ok := l.entries[id+"/reversal"]; ok && !rev.PendingReview {
			row.SpreadCentsTotal -= rev.AmountCents
		}
	}
	out := make([]FXSpreadPNLTotals, 0, len(byCurrency))
	for _, row := range byCurrency {
		out = append(out, *row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Currency < out[j].Currency })
	return out
}
