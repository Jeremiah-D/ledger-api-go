package ledger

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Transfer fee estimation (LG-186, fintech).
//
// EstimateTransferFees answers "what would this transfer cost me?" before
// anything is submitted: it prices the ledger's own fee leg (an explicit
// FeeCents or the tiered transfer fee policy, LG-25), the FX dealing
// spread the desk would keep (LG-93), and the correspondent-banking fee
// configured for the corridor (this file), and returns them as one
// itemized estimate with a source-currency total.
//
// Zero side effects, by construction: the method holds only the read
// lock, writes no journal rows, bumps no version, registers no
// idempotency keys, emits no audit events and touches no hash chain. It
// is the read-only twin of the fee/FX part of PostTransfer — the math it
// uses (resolveFeeLocked, effectiveFXRateLocked, fxSpreadCentsOf,
// fxConvertCents) is the same code the commit path uses, so the estimate
// and the receipt's fee numbers agree by construction. The estimate is
// still advisory: a concurrent rate-table change or fee-policy change
// between the estimate and the real submission can move the numbers, so
// clients must read the receipt of the real call.
//
// Orthogonal to LG-86 (admitted FX execution): this file never converts
// and never locks a rate. It is also orthogonal to the LG-36 dry-run:
// DryRunTransfer simulates the whole posting against a ledger copy and
// reports balance deltas; EstimateTransferFees reads only the cost
// inputs and reports a cost breakdown.

// ErrInvalidCorrespondentFee is returned by SetCorrespondentFee when the
// corridor is invalid (bad currency codes, a self-pair) or the fee is
// negative.
var ErrInvalidCorrespondentFee = errors.New("ledger: correspondent fee corridor invalid or fee negative")

// CorrespondentFee is one corridor's flat fee charged by the
// correspondent banks on a cross-currency transfer: it is estimated in
// the source currency, on top of the ledger's own fee leg and the FX
// dealing spread. It is structural config and survives snapshots.
type CorrespondentFee struct {
	FromCurrency string `json:"from_currency"`
	ToCurrency   string `json:"to_currency"`
	// Cents is the flat fee in source-currency cents. Zero removes the
	// corridor's fee.
	Cents int64 `json:"cents"`
}

// SetCorrespondentFee configures the flat correspondent-banking fee for
// corridor (from -> to), in source-currency cents: EstimateTransferFees
// adds it to cross-currency estimates for the corridor, and the real
// transfer books nothing for it (correspondent fees are charged by the
// banks downstream, outside this ledger's books — the estimate is what
// tells the caller their all-in cost). Setting zero removes the
// corridor's fee. An absent corridor estimates zero, never a guess.
//
// A structural change: it takes the write lock, does not bump the ledger
// version, and survives ExportSnapshot/ImportSnapshot. The corridor must
// be two distinct 3-letter ISO 4217 codes and cents must be non-negative.
func (l *Ledger) SetCorrespondentFee(from, to string, cents int64) error {
	f, err := normalizeCurrency(from)
	if err != nil || from == "" {
		return fmt.Errorf("%w: from currency %q", ErrInvalidCorrespondentFee, from)
	}
	t, err := normalizeCurrency(to)
	if err != nil || to == "" {
		return fmt.Errorf("%w: to currency %q", ErrInvalidCorrespondentFee, to)
	}
	if f == t {
		return fmt.Errorf("%w: from and to currencies must differ", ErrInvalidCorrespondentFee)
	}
	if cents < 0 {
		return fmt.Errorf("%w: got %d cents", ErrInvalidCorrespondentFee, cents)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	pair := fxPair{from: f, to: t}
	if cents == 0 {
		delete(l.correspondentFees, pair)
	} else {
		l.correspondentFees[pair] = cents
	}
	return nil
}

// CorrespondentFees returns every configured corridor fee, sorted by
// (from, to), for operator inspection and snapshot export. Callers get a
// copy: mutating it does not affect the ledger.
func (l *Ledger) CorrespondentFees() []CorrespondentFee {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.correspondentFeesLocked()
}

// correspondentFeesLocked lists the corridor fees in deterministic order.
// Callers must hold l.mu; the read lock suffices.
func (l *Ledger) correspondentFeesLocked() []CorrespondentFee {
	out := make([]CorrespondentFee, 0, len(l.correspondentFees))
	for pair, cents := range l.correspondentFees {
		out = append(out, CorrespondentFee{FromCurrency: pair.from, ToCurrency: pair.to, Cents: cents})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].FromCurrency != out[j].FromCurrency {
			return out[i].FromCurrency < out[j].FromCurrency
		}
		return out[i].ToCurrency < out[j].ToCurrency
	})
	return out
}

// FeeEstimateRequest is the transfer to price. Only the fields that move
// the cost are read: From/To exist so the estimate can name the parties
// in errors (accounts do not move money here), the fee fields resolve
// exactly like PostTransfer (explicit FeeCents/FeeAccount/FeeSplits win,
// then SkipFee/policy), and ToCurrency/FXSide resolve exactly like the
// commit path's FX pricing.
type FeeEstimateRequest struct {
	From        AccountID `json:"from_account"`
	To          AccountID `json:"to_account"`
	AmountCents int64     `json:"amount_cents"`
	// Currency is the transfer's denomination (default currency when
	// empty). ToCurrency names the settlement currency; empty means a
	// same-currency transfer, which has no FX spread and no
	// correspondent fee.
	Currency   string `json:"currency,omitempty"`
	ToCurrency string `json:"to_currency,omitempty"`
	// FXSide selects the dealing price ("sell" default / "buy"); it is
	// validated exactly like Transfer.FXSide and ignored for
	// same-currency estimates.
	FXSide     string         `json:"fx_side,omitempty"`
	FeeCents   int64          `json:"fee_cents,omitempty"`
	FeeAccount AccountID      `json:"fee_account,omitempty"`
	FeeSplits  []FeeSplit     `json:"fee_splits,omitempty"`
	SkipFee    bool           `json:"skip_fee,omitempty"`
}

// FeeEstimate is the itemized all-in cost of a transfer.
//
// The three cost components: FeeCents (the ledger's own fee leg, in
// Currency), FXSpreadCents (the desk's dealing spread, in ToCurrency),
// and CorrespondentCents (the corridor's flat correspondent fee, in
// Currency). TotalCents is the source-currency all-in number:
// fee + correspondent + spread converted back to the source currency at
// the pair's reference mid (FXMidNum/FXMidDen), exactly disclosed — no
// hidden rounding. For same-currency estimates the FX fields are zero.
type FeeEstimate struct {
	Currency   string `json:"currency"`
	ToCurrency string `json:"to_currency,omitempty"`
	// FeeCents is the resolved fee: an explicit FeeCents or the tiered
	// policy's floor(amount * rate / 10000). FeeTierIndex/FeeRateBps
	// disclose the policy tier (-1/0 when the fee is explicit, skipped,
	// or the policy is disabled). FeeSplits change the distribution,
	// not the amount — the total is unchanged.
	FeeCents     int64     `json:"fee_cents"`
	FeeAccount   AccountID `json:"fee_account,omitempty"`
	FeeTierIndex int       `json:"fee_tier_index"`
	FeeRateBps   int64     `json:"fee_rate_bps"`
	// FXConvertedCents is the amount the payee would be credited in the
	// settlement currency, at the dealing price (bid for sell, ask for
	// buy — see fxSideOf).
	FXConvertedCents int64 `json:"fx_converted_cents,omitempty"`
	// FXSpreadCents is the desk's dealing spread in ToCurrency
	// (mid − bid for sell, ask − mid for buy), exactly the leg the real
	// transfer would book as "<id>/spread" (LG-93). FXSpreadBps is the
	// dealing spread in basis points, FXSide the priced side.
	FXSpreadCents   int64  `json:"fx_spread_cents"`
	FXSpreadBps     int64  `json:"fx_spread_bps,omitempty"`
	FXSide          string `json:"fx_side,omitempty"`
	FXMidNum        int64  `json:"fx_mid_num,omitempty"`
	FXMidDen        int64  `json:"fx_mid_den,omitempty"`
	FXRateVersion   uint64 `json:"fx_rate_version,omitempty"`
	// CorrespondentCents is the corridor's flat correspondent fee in
	// Currency. Zero when no corridor fee is configured — the estimate
	// never invents one.
	CorrespondentCents int64 `json:"correspondent_cents"`
	// TotalCents is the source-currency all-in cost: FeeCents +
	// CorrespondentCents + FXSpreadCents converted back to Currency at
	// the reference mid. Overflow-safe: an all-in cost that cannot fit
	// in an int64 rejects with ErrAmountOverflow, exactly like the real
	// transfer's fee leg would.
	TotalCents int64 `json:"total_cents"`
}

// EstimateTransferFees prices a transfer without submitting it: it runs
// the same fee and FX math PostTransfer would run, read-only.
//
// The estimate holds the read lock for its whole run, so it observes one
// consistent snapshot of the fee policy, the rate table and the
// correspondent-fee table. It writes nothing: no journal, no version
// bump, no idempotency registration, no audit events, no hash-chain
// links. The HTTP layer counts estimates as ledger_fee_estimates_total.
//
// Errors mirror the real submission's pricing failures: an invalid
// request (bad fields, same-currency FX, unknown FX side) rejects like a
// 400; a missing or expired FX rate rejects with ErrFXRateMissing /
// ErrFXRateExpired (422); a fee configuration the transfer could not pay
// rejects with ErrInvalidFee. Risk gates (frozen, overdraft, daily
// limits, periods, review thresholds) are NOT evaluated — a fee estimate
// is a price quote, not a submission preview; DryRunTransfer (LG-36)
// covers the full check sequence.
func (l *Ledger) EstimateTransferFees(req FeeEstimateRequest) (FeeEstimate, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	if req.From == "" {
		return FeeEstimate{}, ErrEmptyFromAccount
	}
	if req.To == "" {
		return FeeEstimate{}, ErrEmptyToAccount
	}
	if req.From == req.To {
		return FeeEstimate{}, ErrTransferSameAccount
	}
	if req.AmountCents <= 0 {
		return FeeEstimate{}, ErrTransferNonPositiveAmount
	}
	currency, err := normalizeCurrency(req.Currency)
	if err != nil {
		return FeeEstimate{}, err
	}
	toCurrency := ""
	if req.ToCurrency != "" {
		toCurrency, err = normalizeCurrency(req.ToCurrency)
		if err != nil {
			return FeeEstimate{}, err
		}
		if toCurrency == currency {
			return FeeEstimate{}, fmt.Errorf("%w: to currency %q", ErrInvalidFXRate, req.ToCurrency)
		}
	}
	side := strings.ToLower(strings.TrimSpace(req.FXSide))
	if side != "" && side != FXSideBuy && side != FXSideSell {
		return FeeEstimate{}, ErrInvalidFXSide
	}

	// The fee leg resolves exactly like PostTransfer: resolveFeeLocked is
	// a pure function of the request and the fee policy — it records
	// nothing, so it is safe under the read lock.
	t := Transfer{
		AmountCents: req.AmountCents,
		Currency:    currency,
		FXSide:      req.FXSide,
		FeeCents:    req.FeeCents,
		FeeAccount:  req.FeeAccount,
		FeeSplits:   req.FeeSplits,
		SkipFee:     req.SkipFee,
	}
	feeCents, feeAccount, _, tierIndex, rateBps, err := l.resolveFeeLocked(t)
	if err != nil {
		return FeeEstimate{}, err
	}

	est := FeeEstimate{
		Currency:     currency,
		ToCurrency:   toCurrency,
		FeeCents:     feeCents,
		FeeAccount:   feeAccount,
		FeeTierIndex: tierIndex,
		FeeRateBps:   rateBps,
	}

	now := time.Now().UTC()
	if toCurrency != "" {
		// Same effective-price resolution the commit path uses
		// (admission quotes excluded: an estimate is priced now, like
		// the no-admission commit path).
		eff, err := l.effectiveFXRateLocked(fxPair{from: currency, to: toCurrency}, fxSideOf(t), now)
		if err != nil {
			return FeeEstimate{}, err
		}
		side = fxSideOf(t)
		sideConverted, ok := fxConvertCents(req.AmountCents, eff.Num, eff.Den)
		if !ok {
			return FeeEstimate{}, ErrAmountOverflow
		}
		midConverted, ok := fxConvertCents(req.AmountCents, eff.MidNum, eff.MidDen)
		if !ok {
			return FeeEstimate{}, ErrAmountOverflow
		}
		// The spread the desk would keep: exactly the "<id>/spread" leg
		// math (see fxSpreadCentsOf), but no leg is booked.
		spread := fxSpreadCentsOf(side, sideConverted, midConverted)
		est.FXConvertedCents = sideConverted
		est.FXSpreadCents = spread
		est.FXSpreadBps = fxSpreadBps(eff.BidNum, eff.BidDen, eff.AskNum, eff.AskDen, eff.MidNum, eff.MidDen)
		est.FXSide = side
		est.FXMidNum = eff.MidNum
		est.FXMidDen = eff.MidDen
		est.FXRateVersion = eff.EffectiveVersion
		est.CorrespondentCents = l.correspondentFees[fxPair{from: currency, to: toCurrency}]
	}

	// The all-in total, in the source currency: fee + correspondent fee +
	// spread converted back to source at the reference mid (exactly
	// disclosed, no hidden rounding). Overflow-safe like the real fee
	// leg's amount+fee check.
	total, ok := addCents(est.FeeCents, est.CorrespondentCents)
	if !ok {
		return FeeEstimate{}, ErrAmountOverflow
	}
	if est.FXSpreadCents > 0 {
		spreadInSource, ok := fxConvertCents(est.FXSpreadCents, est.FXMidDen, est.FXMidNum)
		if !ok {
			return FeeEstimate{}, ErrAmountOverflow
		}
		total, ok = addCents(total, spreadInSource)
		if !ok {
			return FeeEstimate{}, ErrAmountOverflow
		}
	}
	est.TotalCents = total
	return est, nil
}
