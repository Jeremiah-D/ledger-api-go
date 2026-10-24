package ledger

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"math/bits"
	"sort"
	"strconv"
	"strings"
	"time"
)

// FX rate table and cross-currency transfers.
//
// A transfer is always denominated in one currency (Transfer.Currency,
// the payer's/source currency), but it may now settle in a different
// target currency (Transfer.ToCurrency). Same-currency transfers take the
// original single-entry path unchanged. A cross-currency transfer books
// two single-currency journal entries that keep each currency's books
// balanced independently:
//
//	principal: ID "<transfer ID>",      debit To@toCurrency convertedCents,
//	                                    credit FXAccount@toCurrency convertedCents
//	fx leg:    ID "<transfer ID>/fx",    debit FXAccount@fromCurrency amountCents,
//	                                    credit From@fromCurrency amountCents
//
// plus the usual optional fee leg (ID "<transfer ID>/fee"), booked in the
// source currency on top of the amount, exactly like a same-currency
// transfer's fee leg.
//
// The FXAccount is the ledger's configured FX P&L / clearing account (see
// WithFXAccount, overridable per transfer via Transfer.FXAccount). It is
// the counterparty of both legs: the converted settlement leaves the
// payer in the source currency and arrives at the payee in the target
// currency, and any implicit spread between the booked legs accumulates in
// the FX account's per-currency balances — visible in TrialBalance,
// Reconcile, and the audit chain like any other account.
//
// The conversion is exact integer math: convertedCents =
// floor(amount * rateNum / rateDen), computed with 128-bit intermediate
// precision (math/bits — standard library), so no float64 ever touches
// money and no amount/rate combination can silently overflow: an
// overflowing conversion is rejected with ErrAmountOverflow before
// anything is recorded.
//
// Rates are opt-in and structural (see SetFXRate): a cross-currency
// transfer with no configured rate for (from, to) is rejected with
// ErrFXRateMissing (HTTP 422). Rates are directional — (USD,EUR) and
// (EUR,USD) are independent entries — and each rate records the ledger
// version at which it took effect (ExchangeRate.EffectiveVersion), so an
// FX receipt discloses exactly which rate converted it.
//
// The risk checks mirror PostTransfer and run in the same order:
// idempotency replay first, then frozen checks on every account touched
// (payer, payee, fee account, FX account), then overdraft on the payer's
// source-currency balance against the total outflow (amount + fee), then
// the daily outflow limit on the same source-currency total. A rejected FX
// transfer records nothing: no journal rows, no chain links, no version
// bump.

// Validation errors for FX transfers.
var (
	// ErrFXRateMissing is returned when a cross-currency transfer names a
	// (from, to) currency pair with no configured rate. It is a
	// 422-class semantic rejection: the request is well-formed, but the
	// ledger cannot convert without a rate.
	ErrFXRateMissing = errors.New("ledger: no FX rate configured for currency pair")
	// ErrFXRateExpired is returned when a cross-currency transfer names a
	// (from, to) pair whose rate exists but has passed its expiry. An
	// expired rate is never used silently: the transfer is rejected with
	// 422 until the operator installs a fresh rate (see SetFXRateRat).
	// The rejection records nothing — no journal rows, no chain links, no
	// version bump — exactly like ErrFXRateMissing.
	ErrFXRateExpired = errors.New("ledger: FX rate for currency pair has expired")
	// ErrFXAccountNotConfigured is returned when a cross-currency
	// transfer needs an FX clearing account and neither the transfer nor
	// the ledger configured one. It is a 400-class error: the caller can
	// fix it by passing fx_account.
	ErrFXAccountNotConfigured = errors.New("ledger: FX transfer requires an fx_account on the transfer or a configured FX account")
	// ErrInvalidFXAccount is returned when the FX clearing account equals
	// the payer or the payee: it must be a distinct counterparty, or one
	// of the two journal legs would debit and credit the same account.
	ErrInvalidFXAccount = errors.New("ledger: FX account must differ from the payer and the payee")
	// ErrInvalidFXRate is returned by SetFXRate and ParseFXRates for a
	// malformed rate: bad currency codes, equal from/to, or a
	// non-positive numerator or denominator.
	ErrInvalidFXRate = errors.New("ledger: invalid FX rate")
	// ErrFXSlippageExceeded is returned by a cross-currency transfer
	// that opted into a slippage cap (see Transfer.MaxSlippageBps, LG-86)
	// when the effective rate moved beyond the cap between request
	// admission and commit. It is a 422-class semantic rejection: the
	// request was well-formed and the admission quote was live, but the
	// price the transfer would settle at drifted past the caller's
	// tolerance. The rejection records nothing — no journal rows, no
	// chain links, no version bump — exactly like ErrFXRateMissing.
	ErrFXSlippageExceeded = errors.New("ledger: FX rate moved beyond the transfer's max slippage since admission")
	// ErrInvalidMaxSlippage is returned at field validation when
	// Transfer.MaxSlippageBps is negative. It is a 400-class error: the
	// caller can fix it by passing a non-negative cap (or omitting it).
	ErrInvalidMaxSlippage = errors.New("ledger: max_slippage_bps must not be negative")
)

// fxPair is the directional lookup key of the rate table: (from, to) and
// (to, from) are independent entries.
type fxPair struct {
	from string
	to   string
}

// effectiveFXRate is the full price of one directional FX pair at one
// instant: the market-table rate plus any active LG-69 manual override.
type effectiveFXRate struct {
	// Rate is the market-table entry, kept for provenance disclosure
	// (the market price the override was measured against).
	Rate ExchangeRate
	// MidNum/MidDen is the reference mid price: the table's Num/Den, or
	// the override price while an override is active. The spread is
	// measured against the mid (see fx_spread.go).
	MidNum, MidDen int64
	// BidNum/BidDen and AskNum/AskDen are the dealing prices: the
	// table's two-sided quote when set, the mid otherwise (a mid-only
	// rate deals at mid on both sides). While an LG-69 override is
	// active all three prices collapse to the override price — an
	// override is a single manual price, so there is no spread to book.
	BidNum, BidDen int64
	AskNum, AskDen int64
	// Num/Den is the price conversions actually settle at: the dealing
	// price for the requested side (bid for a sell, ask for a buy),
	// the mid when the pair is mid-only, or the override price while an
	// override is active.
	Num, Den int64
	// EffectiveVersion and ExpiresAt describe the price above: the
	// override's while active, the market rate's otherwise.
	EffectiveVersion uint64
	ExpiresAt        time.Time
	// Overridden reports whether the price is an active LG-69 manual
	// override; Override is that override (a copy).
	Overridden bool
	Override   RateOverride
}

// effectiveFXRateLocked resolves the full effective rate for pair at
// now: the market rate from the rate table, priced through any active
// LG-69 manual override (see fx_override.go). side selects the dealing
// price conversions settle at (see fxSideOf): FXSideBuy settles at ask,
// anything else at bid. A pair with no configured rate reports
// ErrFXRateMissing; a rate past its expiry reports ErrFXRateExpired — the
// expiry check runs against now, like the transfer gate, and the
// override's lazy expiry predicate works the same way. Callers must hold
// l.mu; the read lock suffices (pure read, no mutation). Admission
// (admitFXRateQuote) and commit (postTransferFXLocked, the split FX chunk
// path) share this body: one definition of the effective price, zero
// logic fork.
func (l *Ledger) effectiveFXRateLocked(pair fxPair, side string, now time.Time) (effectiveFXRate, error) {
	rate, ok := l.fxRates[pair]
	if !ok {
		return effectiveFXRate{}, ErrFXRateMissing
	}
	if !rate.ExpiresAt.IsZero() && !now.Before(rate.ExpiresAt) {
		return effectiveFXRate{}, ErrFXRateExpired
	}
	eff := effectiveFXRate{
		Rate:             rate,
		MidNum:           rate.Num,
		MidDen:           rate.Den,
		BidNum:           rate.BidNum,
		BidDen:           rate.BidDen,
		AskNum:           rate.AskNum,
		AskDen:           rate.AskDen,
		EffectiveVersion: rate.EffectiveVersion,
		ExpiresAt:        rate.ExpiresAt,
	}
	// A mid-only rate (zero bid/ask) deals at mid on both sides.
	if eff.BidNum == 0 || eff.BidDen == 0 {
		eff.BidNum, eff.BidDen = eff.MidNum, eff.MidDen
	}
	if eff.AskNum == 0 || eff.AskDen == 0 {
		eff.AskNum, eff.AskDen = eff.MidNum, eff.MidDen
	}
	if side == FXSideBuy {
		eff.Num, eff.Den = eff.AskNum, eff.AskDen
	} else {
		eff.Num, eff.Den = eff.BidNum, eff.BidDen
	}
	if override, overridden := l.activeRateOverrideLocked(pair, now); overridden {
		// An override is a single manual price: bid, ask, and mid all
		// collapse to it, so the spread is zero and no spread leg
		// books — the override's own audit event
		// (fx_rate_overridden) carries the re-pricing.
		eff.Num, eff.Den = override.Num, override.Den
		eff.MidNum, eff.MidDen = override.Num, override.Den
		eff.BidNum, eff.BidDen = override.Num, override.Den
		eff.AskNum, eff.AskDen = override.Num, override.Den
		eff.EffectiveVersion = override.EffectiveVersion
		eff.ExpiresAt = override.ExpiresAt
		eff.Overridden = true
		eff.Override = override
	}
	return eff, nil
}

// FXRateQuote is a rate quote locked in at request admission (LG-86).
// The zero quote means "no quote": the transfer is not cross-currency,
// its currency fields are invalid (validation rejects it later), or the
// pair had no live rate at admission — missing or expired. A transfer
// posted with a zero quote converts at the commit-time rate, exactly as
// before: the no-admission paths (PostTransferSplit's no-split delegate
// and chunk path, which hold the write lock from entry, and therefore
// have no admission window) keep commit-time semantics and carry no
// slippage gate. Never invent a quote for a path that did not admit
// one: the quote must be measured, never synthesized.
//
// Since LG-93 the quote is side-correct: Num/Den is the dealing price
// the transfer settles at (bid for a sell, ask for a buy — see
// Transfer.FXSide), and the quote also pins the bid, the ask, and the
// reference mid it was measured against, so the spread disclosure on
// the receipt is exact even after the table moves.
type FXRateQuote struct {
	From string `json:"from"`
	To   string `json:"to"`
	// Num/Den is the full effective price at admission: the market rate
	// resolved through any active LG-69 manual override, exactly what
	// effectiveFXRateLocked returned — admission and commit share one
	// definition of the effective price. Since LG-93 it is the
	// SIDE-correct dealing price (bid for a sell, ask for a buy — see
	// Transfer.FXSide), not the mid.
	Num int64 `json:"num"`
	Den int64 `json:"den"`
	// BidNum/BidDen, AskNum/AskDen, and MidNum/MidDen are the dealing
	// prices and the reference mid at admission (see fx_spread.go): the
	// spread the transfer settles at is measured against this mid, so
	// the quote pins all three. A mid-only rate quotes bid = ask = mid.
	BidNum int64 `json:"bid_num,omitempty"`
	BidDen int64 `json:"bid_den,omitempty"`
	AskNum int64 `json:"ask_num,omitempty"`
	AskDen int64 `json:"ask_den,omitempty"`
	MidNum int64 `json:"mid_num,omitempty"`
	MidDen int64 `json:"mid_den,omitempty"`
	// EffectiveVersion is the ledger version at which the admitted
	// price took effect (the override's while overridden, like
	// ExchangeRate.EffectiveVersion).
	EffectiveVersion uint64 `json:"effective_version"`
	// ExpiresAt is the admitted price's expiry (the override's while
	// overridden); the zero time means the price never expires.
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	// AdmittedAt is the wall-clock time the quote was taken.
	AdmittedAt time.Time `json:"admitted_at"`
	// OverrideApplied reports whether the admitted price was an active
	// LG-69 override; OverrideApprovedBy and MarketNum/MarketDen carry
	// its provenance (the checker, and the market rate it priced
	// against at admission).
	OverrideApplied    bool   `json:"override_applied,omitempty"`
	OverrideApprovedBy string `json:"override_approved_by,omitempty"`
	// OverrideProposedBy and OverrideReason carry the admitted
	// override's full provenance: the fx_rate_overridden audit event
	// binds every overridden conversion to its proposer and reason,
	// and the admission quote must preserve that binding — the
	// commit-time table may hold a different override by commit time,
	// so the commit-time override's provenance must never be mixed
	// into a conversion that settled at the admitted price.
	OverrideProposedBy string `json:"override_proposed_by,omitempty"`
	OverrideReason     string `json:"override_reason,omitempty"`
	MarketNum          int64  `json:"market_rate_num,omitempty"`
	MarketDen          int64  `json:"market_rate_den,omitempty"`
	// RateSnapshotVersion pins the quote to one versioned copy of the
	// rate table (see fx_snapshot.go), like FXConversion.
	RateSnapshotVersion uint64 `json:"rate_snapshot_version,omitempty"`
}

// admitFXRateQuote snapshots the effective FX rate for t at request
// admission: it runs before the write lock is taken, so the price a
// transfer settles at is the price quoted when the request arrived —
// not the live rate at commit time. The rate table moves under the
// write lock (SetFXRate), so the admission→commit window is real: the
// commit re-resolves the live rate, measures the drift (see
// fxSlippageBps), gates it against the caller's opt-in cap, and
// discloses both prices.
//
// The caller must not hold l.mu: the rate table is read under the read
// lock. Currency normalization is pure (no lock needed) and mirrors
// validateTransferFields, so the admitted pair is the pair the commit
// validates — a transfer whose currencies validate admits the quote
// for the pair it converts.
func (l *Ledger) admitFXRateQuote(t Transfer) FXRateQuote {
	currency, err := normalizeCurrency(t.Currency)
	if err != nil {
		return FXRateQuote{}
	}
	to := currency
	if t.ToCurrency != "" {
		if to, err = normalizeCurrency(t.ToCurrency); err != nil {
			return FXRateQuote{}
		}
	}
	if to == currency {
		return FXRateQuote{}
	}
	now := time.Now()
	l.mu.RLock()
	defer l.mu.RUnlock()
	// The quote is side-correct: a "buy" transfer admits the ask, a
	// "sell" (the default) the bid (see fxSideOf) — the slippage gate at
	// commit measures drift on the dealing price the transfer settles
	// at, not the mid.
	eff, err := l.effectiveFXRateLocked(fxPair{from: currency, to: to}, fxSideOf(t), now)
	if err != nil {
		// No live rate at admission: the commit path surfaces
		// ErrFXRateMissing/ErrFXRateExpired exactly as before.
		return FXRateQuote{}
	}
	q := FXRateQuote{
		From:                currency,
		To:                  to,
		Num:                 eff.Num,
		Den:                 eff.Den,
		BidNum:              eff.BidNum,
		BidDen:              eff.BidDen,
		AskNum:              eff.AskNum,
		AskDen:              eff.AskDen,
		MidNum:              eff.MidNum,
		MidDen:              eff.MidDen,
		EffectiveVersion:    eff.EffectiveVersion,
		ExpiresAt:           eff.ExpiresAt,
		AdmittedAt:          now,
		RateSnapshotVersion: l.fxSnapshotVersion,
	}
	if eff.Overridden {
		q.OverrideApplied = true
		q.OverrideApprovedBy = eff.Override.Approver
		q.OverrideProposedBy = eff.Override.Proposer
		q.OverrideReason = eff.Override.Reason
		q.MarketNum, q.MarketDen = eff.Rate.Num, eff.Rate.Den
	}
	return q
}

// fxSlippageBps measures how far the commit-time effective rate drifted
// from the admitted quote: floor(|commit − admitted| / admitted * 10000),
// in basis points. Exact big.Rat math — no float64 touches money —
// matching the LG-69 override-deviation convention (see
// fx_override.go). All four inputs are positive ratios (the admitted
// quote and the commit-time effective rate always are); the result
// saturates at math.MaxInt64 rather than overflowing on pathological
// inputs.
func fxSlippageBps(commitNum, commitDen, admitNum, admitDen int64) int64 {
	commit := new(big.Rat).SetFrac64(commitNum, commitDen)
	admitted := new(big.Rat).SetFrac64(admitNum, admitDen)
	dev := new(big.Rat).Sub(commit, admitted)
	dev.Abs(dev)
	dev.Quo(dev, admitted)
	dev.Mul(dev, big.NewRat(10000, 1))
	// The deviation is non-negative, so integer division floors.
	bps := new(big.Int).Quo(dev.Num(), dev.Denom())
	if !bps.IsInt64() {
		return math.MaxInt64
	}
	return bps.Int64()
}

// ExchangeRate is one directional conversion rate: converting
// fromCurrency to toCurrency multiplies by Num/Den and floors to whole
// cents. EffectiveVersion is the ledger version at which the rate was set
// (structural change, not a posting, so it does not bump the version);
// receipts disclose the rate that converted them together with this
// version for auditability.
//
// ExpiresAt is the rate's expiry: a cross-currency transfer posted at or
// after ExpiresAt is rejected with ErrFXRateExpired — a stale rate is
// never applied silently. The zero time means the rate never expires
// (the behavior of SetFXRate and the LEDGER_FX_RATES startup config).
// Expiry is wall-clock time, checked against the transfer's post time;
// like every other piece of structural config it survives
// ExportSnapshot/ImportSnapshot, so a restored ledger enforces the same
// expiry the original would have.
type ExchangeRate struct {
	FromCurrency     string    `json:"from_currency"`
	ToCurrency       string    `json:"to_currency"`
	Num              int64     `json:"num"`
	Den              int64     `json:"den"`
	EffectiveVersion uint64    `json:"effective_version"`
	ExpiresAt        time.Time `json:"expires_at,omitempty"`
	// BidNum/BidDen and AskNum/AskDen are the pair's two-sided dealing
	// prices (LG-93, see fx_spread.go): a "sell" transfer converts at
	// bid, a "buy" transfer at ask, while Num/Den stays the reference
	// mid. Zero bid/ask means a mid-only legacy rate: bid = ask = mid,
	// zero spread, no spread leg — exactly the pre-LG-93 behavior.
	// The dealing spread always brackets the mid
	// (0 < bid <= mid <= ask, enforced by SetFXBidAskRate), so the
	// spread is dealer-favorable by construction.
	BidNum int64 `json:"bid_num,omitempty"`
	BidDen int64 `json:"bid_den,omitempty"`
	AskNum int64 `json:"ask_num,omitempty"`
	AskDen int64 `json:"ask_den,omitempty"`
}

// WithFXAccount configures the ledger-wide FX clearing account used as
// the counterparty of cross-currency transfer legs (see the package
// comment). A transfer may override it with Transfer.FXAccount.
func WithFXAccount(a AccountID) Option {
	return func(l *Ledger) {
		l.fxAccount = a
	}
}

// WithFXRateOption installs an FX rate at construction time (for
// LEDGER_FX_RATES startup config): converting fromCurrency to toCurrency
// books floor(amount * num / den) target-currency cents. Inputs are
// validated like SetFXRate — a bad rate panics, fail-fast at
// construction. The rate takes effect at ledger version 0 (genesis).
func WithFXRateOption(from, to string, num, den int64) Option {
	f, t, err := normalizeFXRate(from, to, num, den)
	if err != nil {
		panic(err)
	}
	return func(l *Ledger) {
		l.fxRates[fxPair{from: f, to: t}] = ExchangeRate{
			FromCurrency: f, ToCurrency: t, Num: num, Den: den,
		}
	}
}

// SetFXRate installs (or replaces) the conversion rate for the
// directional pair (from, to): converting X cents of fromCurrency books
// floor(X * num / den) cents of toCurrency. Both codes are normalized
// like any other currency input (empty from/to is rejected; anything else
// must be a 3-letter uppercase ISO 4217 code), from and to must differ,
// and num and den must be positive. The rate takes effect immediately and
// records the current ledger version as its effective version. A rate of
// (from, from) or a non-positive ratio is rejected with ErrInvalidFXRate.
//
// The installed rate is mid-only: it clears any LG-93 bid/ask dealing
// prices on the pair (see SetFXBidAskRate) — re-install them afterwards
// when a spread is wanted.
//
// SetFXRate is a structural change, like Freeze or SetDailyLimit: it
// takes the write lock, does not bump the ledger version, and survives
// ExportSnapshot/ImportSnapshot.
func (l *Ledger) SetFXRate(from, to string, num, den int64) error {
	from, to, err := normalizeFXRate(from, to, num, den)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fxRates[fxPair{from: from, to: to}] = ExchangeRate{
		FromCurrency:     from,
		ToCurrency:       to,
		Num:              num,
		Den:              den,
		EffectiveVersion: l.version,
	}
	// LG-45: the table changed — bump the snapshot version and persist
	// the new table, so transfers converting at this rate stay
	// reproducible after the next change.
	l.snapshotFXRateTableLocked(fmt.Sprintf("set %s->%s %d/%d", from, to, num, den))
	return nil
}

// SetFXRateRat installs (or replaces) the conversion rate for the
// directional pair (from, to) from an exact rational: converting X cents
// of fromCurrency books floor(X * num / den) cents of toCurrency, where
// num/den is rate reduced to lowest terms by math/big.
//
// Decimal rates are exact here, which is the whole point of the big.Rat
// path: a rate parsed from "1.10" is exactly 11/10, while the same rate
// rounded through float64 would be 1.1000000000000000888 — a ratio no
// integer num/den pair represents, so float64 can never name it. Use
// ParseFXRateDecimal to turn operator-supplied decimal strings into the
// *big.Rat this takes.
//
// ttl is the rate's lifetime: a non-positive ttl means the rate never
// expires (same as SetFXRate); a positive ttl sets ExpiresAt to now+ttl,
// after which cross-currency transfers for the pair are rejected with
// ErrFXRateExpired instead of converting at a stale rate. A nil or
// non-positive rate, a ratio whose reduced numerator or denominator does
// not fit int64, and a negative ttl are rejected with ErrInvalidFXRate.
//
// Like SetFXRate, this is a structural change: it takes the write lock,
// does not bump the ledger version, and survives ExportSnapshot/
// ImportSnapshot (expiry included).
func (l *Ledger) SetFXRateRat(from, to string, rate *big.Rat, ttl time.Duration) error {
	if rate == nil || rate.Sign() <= 0 {
		return fmt.Errorf("%w: rate must be positive", ErrInvalidFXRate)
	}
	// Num/Denom return the reduced numerator and denominator: the exact
	// ratio, with no float rounding anywhere in the pipeline.
	num, den := rate.Num(), rate.Denom()
	if !num.IsInt64() || !den.IsInt64() {
		return fmt.Errorf("%w: rate %s/%s does not fit int64", ErrInvalidFXRate, num, den)
	}
	f, t, err := normalizeFXRate(from, to, num.Int64(), den.Int64())
	if err != nil {
		return err
	}
	if ttl < 0 {
		return fmt.Errorf("%w: ttl must not be negative", ErrInvalidFXRate)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.setFXRateRatLocked(f, t, num.Int64(), den.Int64(), ttl, time.Now())
	return nil
}

// setFXRateRatLocked is the locked write half of SetFXRateRat: it
// installs the (already validated) rate, snapshots the table (LG-45),
// and audits fx_rate_change. now is the quote time the TTL is measured
// from. Callers must hold the write lock. The provider refresh path
// (see fx_provider.go) writes through this same body, so
// provider-installed rates are indistinguishable from
// operator-installed ones downstream.
func (l *Ledger) setFXRateRatLocked(f, t string, num, den int64, ttl time.Duration, now time.Time) {
	er := ExchangeRate{
		FromCurrency:     f,
		ToCurrency:       t,
		Num:              num,
		Den:              den,
		EffectiveVersion: l.version,
	}
	if ttl > 0 {
		er.ExpiresAt = now.Add(ttl)
	}
	l.fxRates[fxPair{from: f, to: t}] = er
	// LG-45: snapshot the new table like SetFXRate does.
	change := fmt.Sprintf("set %s->%s %d/%d", f, t, er.Num, er.Den)
	if ttl > 0 {
		change += fmt.Sprintf(" ttl=%s", ttl)
	}
	l.snapshotFXRateTableLocked(change)
}

// ParseFXRateDecimal parses a decimal rate string ("1.10", "7.2015",
// "0.85") into an exact *big.Rat for SetFXRateRat. The result is the
// mathematically exact decimal — "1.10" is 11/10, not the nearest
// float64 — so the booked conversion is exactly what the operator typed.
// big.Rat's SetString also accepts "a/b" fractions and exponents; the
// only hard requirements are that the string parses and the value is
// positive. An empty or unparsable string, or a non-positive value, is an
// error.
func ParseFXRateDecimal(s string) (*big.Rat, error) {
	s = strings.TrimSpace(s)
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, fmt.Errorf("%w: cannot parse decimal rate %q", ErrInvalidFXRate, s)
	}
	if r.Sign() <= 0 {
		return nil, fmt.Errorf("%w: rate must be positive", ErrInvalidFXRate)
	}
	return r, nil
}

// RemoveFXRate deletes the conversion rate for (from, to). Cross-currency
// transfers for the pair are rejected with ErrFXRateMissing afterwards.
// Removing a rate that was never set is a no-op returning true when a
// rate was actually removed.
func (l *Ledger) RemoveFXRate(from, to string) (bool, error) {
	f, err := normalizeCurrency(from)
	if err != nil {
		return false, err
	}
	t, err := normalizeCurrency(to)
	if err != nil {
		return false, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.fxRates[fxPair{from: f, to: t}]; !ok {
		return false, nil
	}
	delete(l.fxRates, fxPair{from: f, to: t})
	// LG-45: the table changed — snapshot the removal, so an auditor
	// can still reproduce the table (with this rate) from the previous
	// version.
	l.snapshotFXRateTableLocked(fmt.Sprintf("remove %s->%s", f, t))
	return true, nil
}

// FXRate returns the configured rate for (from, to), or false when none
// is set.
func (l *Ledger) FXRate(from, to string) (ExchangeRate, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	r, ok := l.fxRates[fxPair{from: from, to: to}]
	return r, ok
}

// fxRatesLocked returns every configured rate sorted by (from, to), for
// snapshots, reconciliation, and operator inspection. Callers must hold
// l.mu; the read lock suffices.
func (l *Ledger) fxRatesLocked() []ExchangeRate {
	out := make([]ExchangeRate, 0, len(l.fxRates))
	for _, r := range l.fxRates {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].FromCurrency != out[j].FromCurrency {
			return out[i].FromCurrency < out[j].FromCurrency
		}
		return out[i].ToCurrency < out[j].ToCurrency
	})
	return out
}

// normalizeFXRate validates one rate's inputs and returns the canonical
// currency codes.
func normalizeFXRate(from, to string, num, den int64) (string, string, error) {
	f, err := normalizeCurrency(from)
	if err != nil || from == "" {
		return "", "", fmt.Errorf("%w: from currency %q", ErrInvalidFXRate, from)
	}
	t, err := normalizeCurrency(to)
	if err != nil || to == "" {
		return "", "", fmt.Errorf("%w: to currency %q", ErrInvalidFXRate, to)
	}
	if f == t {
		return "", "", fmt.Errorf("%w: from and to currencies must differ", ErrInvalidFXRate)
	}
	if num <= 0 || den <= 0 {
		return "", "", fmt.Errorf("%w: rate ratio must be positive, got %d/%d", ErrInvalidFXRate, num, den)
	}
	return f, t, nil
}

// ParseFXRates parses the LEDGER_FX_RATES environment variable into
// exchange rates. The syntax is a comma-separated list of
// "<from>:<to>=<num>/<den>", e.g.
//
//	"USD:EUR=108/100,USD:CNY=720/100"
//
// meaning 1 USD = 1.08 EUR and 1 USD = 7.20 CNY. Currency codes follow the
// usual rules (empty not allowed here; 3-letter uppercase ISO 4217); num
// and den are positive integers. Unlike the Go API, parsing is strict —
// a malformed pair, a non-positive ratio, a duplicate pair, or a
// same-currency pair is an error, so a misconfigured deployment fails fast
// at startup instead of converting at a wrong rate.
func ParseFXRates(raw string) ([]ExchangeRate, error) {
	fail := func(format string, args ...any) ([]ExchangeRate, error) {
		return nil, fmt.Errorf("ledger: invalid FX rates %q: "+format, append([]any{raw}, args...)...)
	}
	if strings.TrimSpace(raw) == "" {
		return fail("empty rate list")
	}
	seen := make(map[fxPair]bool)
	var out []ExchangeRate
	for _, seg := range strings.Split(raw, ",") {
		pair, ratio, ok := strings.Cut(strings.TrimSpace(seg), "=")
		if !ok {
			return fail("segment %q must be \"<from>:<to>=<num>/<den>\"", seg)
		}
		from, to, ok := strings.Cut(pair, ":")
		if !ok {
			return fail("pair %q must be \"<from>:<to>\"", pair)
		}
		numRaw, denRaw, ok := strings.Cut(ratio, "/")
		if !ok {
			return fail("ratio %q must be \"<num>/<den>\"", ratio)
		}
		num, err := strconv.ParseInt(strings.TrimSpace(numRaw), 10, 64)
		if err != nil || num <= 0 {
			return fail("rate numerator %q must be a positive integer", numRaw)
		}
		den, err := strconv.ParseInt(strings.TrimSpace(denRaw), 10, 64)
		if err != nil || den <= 0 {
			return fail("rate denominator %q must be a positive integer", denRaw)
		}
		f, t, err := normalizeFXRate(strings.TrimSpace(from), strings.TrimSpace(to), num, den)
		if err != nil {
			return fail("%v", err)
		}
		if seen[fxPair{from: f, to: t}] {
			return fail("duplicate rate for %s->%s", f, t)
		}
		seen[fxPair{from: f, to: t}] = true
		out = append(out, ExchangeRate{FromCurrency: f, ToCurrency: t, Num: num, Den: den})
	}
	return out, nil
}

// fxConvertCents converts amount cents at the ratio num/den, flooring to
// whole cents: floor(amount * num / den). The multiplication runs in
// 128-bit (math/bits) so any non-negative amount and any positive ratio
// compute exactly; it reports false when the converted value does not fit
// in an int64 — a conversion that overflows is rejected before booking,
// because no account balance could hold it either.
func fxConvertCents(amount, num, den int64) (int64, bool) {
	if amount < 0 || num <= 0 || den <= 0 {
		return 0, false
	}
	hi, lo := bits.Mul64(uint64(amount), uint64(num))
	// bits.Div64 panics when the quotient would not fit in 64 bits, i.e.
	// when hi >= den: then the result exceeds 2^64 and certainly does not
	// fit in an int64.
	if hi >= uint64(den) {
		return 0, false
	}
	q, _ := bits.Div64(hi, lo, uint64(den))
	if q > math.MaxInt64 {
		return 0, false
	}
	return int64(q), true
}

// FXConversion discloses how a cross-currency transfer was converted: the
// rate that applied (with the ledger version at which it took effect and
// the rate's expiry, when the rate carries one) and the settled amounts on
// both sides — SourceCents before conversion, ConvertedCents after. Since
// LG-93 the rate that applied is the SIDE-correct dealing price (bid for
// a sell, ask for a buy — see Direction); the reference mid and the full
// two-sided quote ride alongside, with the spread the desk earned. It is
// carried on TransferReceipt for the original posting; idempotent replays
// return FX == nil, because the rate table may have changed since — the
// replayed journal entries are the authoritative record.
//
// RateSnapshotVersion (LG-45) is the FX rate-table snapshot version in
// effect when the transfer posted: FXRateTableSnapshot(RateSnapshotVersion)
// reproduces exactly the table that converted it, even after the rate was
// replaced or removed.
type FXConversion struct {
	FromCurrency     string    `json:"from_currency"`
	ToCurrency       string    `json:"to_currency"`
	SourceCents      int64     `json:"source_cents"`
	RateNum          int64     `json:"rate_num"`
	RateDen          int64     `json:"rate_den"`
	ConvertedCents   int64     `json:"converted_cents"`
	EffectiveVersion uint64    `json:"effective_version"`
	RateExpiresAt    time.Time `json:"rate_expires_at,omitempty"`
	// RateSnapshotVersion pins the conversion to one versioned copy of
	// the rate table (see fx_snapshot.go); 0 is never emitted for a
	// real conversion (a conversion implies a populated table). It
	// refers to the market rate table; an overridden conversion's
	// full provenance (proposer, approver, reason, both rates) lands
	// on the fx_rate_overridden audit event instead.
	RateSnapshotVersion uint64 `json:"rate_snapshot_version"`
	// OverrideApplied reports whether the conversion priced at an
	// approved manual rate override (see fx_override.go) instead of
	// the market rate. When true, RateNum/RateDen, EffectiveVersion,
	// and RateExpiresAt describe the override; MarketRateNum/
	// MarketRateDen carry the market rate in effect at post time, and
	// OverrideApprovedBy names the checker who approved it — the
	// payee-side settlement is never silently re-priced.
	OverrideApplied    bool   `json:"override_applied,omitempty"`
	MarketRateNum      int64  `json:"market_rate_num,omitempty"`
	MarketRateDen      int64  `json:"market_rate_den,omitempty"`
	OverrideApprovedBy string `json:"override_approved_by,omitempty"`
	// AdmittedRateNum/AdmittedRateDen is the LG-86 admission quote: the
	// full effective price locked when the request was admitted, before
	// the write lock was taken. When the rate moved between admission
	// and commit, the transfer still settled at this price — RateNum/
	// RateDen always name the price that converted, and SlippageBps
	// measures how far the live rate had drifted from this quote. Zero
	// (omitted) on the no-admission paths, which convert at the
	// commit-time rate (see FXRateQuote).
	AdmittedRateNum int64 `json:"admitted_rate_num,omitempty"`
	AdmittedRateDen int64 `json:"admitted_rate_den,omitempty"`
	// AdmittedEffectiveVersion is the ledger version at which the
	// admitted price took effect; AdmittedAt is when the quote was
	// taken. Both are omitted when no quote was admitted.
	AdmittedEffectiveVersion uint64    `json:"admitted_effective_version,omitempty"`
	AdmittedAt               time.Time `json:"admitted_at,omitempty"`
	// SlippageBps is floor(|commit − admitted| / admitted * 10000): how
	// far the live effective rate had drifted from the admitted quote
	// by commit time, in basis points (exact big.Rat math, see
	// fxSlippageBps). Omitted when no quote was admitted.
	SlippageBps int64 `json:"slippage_bps,omitempty"`
	// Direction is the dealing side the transfer priced at (LG-93, see
	// fx_spread.go): "sell" (the default) converts at the bid, "buy" at
	// the ask. RateNum/RateDen above name the dealing price that
	// converted; MidNum/MidDen is the reference mid it was measured
	// against, and BidNum/BidDen / AskNum/AskDen are the pair's
	// two-sided quote. A mid-only rate discloses bid = ask = mid.
	Direction string `json:"direction,omitempty"`
	MidNum    int64  `json:"mid_num,omitempty"`
	MidDen    int64  `json:"mid_den,omitempty"`
	BidNum    int64  `json:"bid_num,omitempty"`
	BidDen    int64  `json:"bid_den,omitempty"`
	AskNum    int64  `json:"ask_num,omitempty"`
	AskDen    int64  `json:"ask_den,omitempty"`
	// SpreadBps is the full bid-ask spread against the mid, in basis
	// points: floor((ask − bid)/mid · 10000), exact big.Rat math (see
	// fxSpreadBps). Zero for a mid-only rate.
	SpreadBps int64 `json:"spread_bps,omitempty"`
	// SpreadCents is the dealer's spread on this transfer, in target
	// currency cents: floor(amount·mid) − floor(amount·bid) on a sell,
	// floor(amount·ask) − floor(amount·mid) on a buy — always ≥ 0, and
	// zero when the rate is mid-only or the spread is dust. A positive
	// spread books the "<transfer ID>/spread" leg (debit FX account,
	// credit FXIncomeAccount, target currency); zero books no leg.
	SpreadCents int64 `json:"spread_cents,omitempty"`
	// FXIncomeAccount is the account the spread leg credited: the
	// transfer's override, or the ledger's configured FX income
	// account. Empty when no spread leg booked.
	FXIncomeAccount AccountID `json:"fx_income_account,omitempty"`
}

// postTransferFXLocked records a cross-currency transfer: t.Currency is
// the source currency, toCurrency the settlement currency
// (toCurrency != t.Currency). Callers must hold the write lock; every
// check runs before the first mutation, so a rejection records nothing.
//
// quote is the LG-86 admission quote (see FXRateQuote): when it names
// this transfer's pair, the transfer settles at the admitted price —
// not the live rate — and the drift between the two is measured,
// gated against the opt-in slippage cap, and disclosed on the receipt
// and the audit event. The zero quote means commit-time semantics:
// the transfer converts at the rate resolved here, with no slippage
// gate (the no-admission paths, e.g. PostTransferSplit).
func (l *Ledger) postTransferFXLocked(t Transfer, toCurrency string, now time.Time, quote FXRateQuote) (TransferReceipt, error) {
	feeCents, feeAccount, feeSplits, tierIndex, rateBps, err := l.resolveFeeLocked(t)
	if err != nil {
		return TransferReceipt{}, err
	}
	totalOutflow, ok := addCents(t.AmountCents, feeCents)
	if !ok {
		return TransferReceipt{}, ErrAmountOverflow
	}
	// CreatedAt is filled before the daily-limit check: the outflow is
	// booked against the transfer's own UTC calendar day, mirroring
	// PostTransfer.
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}

	// The idempotency replay check runs before the rate lookup, mirroring
	// PostTransfer: replaying a key that was posted before its rate was
	// removed returns the original receipt instead of failing, because
	// the replay books nothing new.
	if t.IdempotencyKey != "" {
		if receipt, ok := l.replayTransferLocked(t); ok {
			return receipt, nil
		}
	}

	// Virtual-account attribution (see virtual_account.go): after the
	// idempotency replay check — a replay returns the original receipt
	// instead of failing on a VAN registered after the original posting
	// — and before every account risk gate, so frozen/overdraft/
	// daily-limit checks see the real (parent) accounts. The payer leg
	// rejects on a VAN (ErrVANNoDebit); the payee leg resolves to its
	// parent, with the attribution amount filled once the conversion is
	// priced below.
	if err := l.vanDebitRejectedLocked(t.From); err != nil {
		return TransferReceipt{}, err
	}
	var vanAttrs []*vanAttribution
	var toAttr *vanAttribution
	if parent, ok := l.vanParentLocked(t.To); ok {
		toAttr = &vanAttribution{VAN: t.To, Parent: parent, Currency: toCurrency, EntryID: t.ID}
		vanAttrs = append(vanAttrs, toAttr)
		t.To = parent
	}

	// The commit-time effective rate, re-resolved live: the market rate
	// from the table priced through any active LG-69 manual override
	// (see effectiveFXRateLocked — the same body the admission quote
	// was resolved with). Since LG-93 the resolution is side-aware (see
	// fxSideOf): a "buy" transfer resolves the ask, a "sell" (the
	// default) the bid — the price conversions settle at. A pair with no
	// configured rate reports ErrFXRateMissing; a rate past its expiry
	// reports ErrFXRateExpired: a stale rate is never applied silently,
	// and the check runs against the transfer's post time, after the
	// idempotency replay check above, so replaying a key posted while
	// the rate was live still returns the original receipt.
	side := fxSideOf(t)
	pair := fxPair{from: t.Currency, to: toCurrency}
	// LG-110: a forward-locked settlement (see forward.go) prices at the
	// contract's locked rate without consulting the live rate table: the
	// rate is contractually fixed, so the table is irrelevant — the
	// transfer settles even when the pair has no configured rate, and a
	// later SetFXRate can never re-price it. The locked price is the
	// whole quote (mid = bid = ask = locked, a single agreed price has
	// no spread), and slippage is zero by construction — the customer
	// never faced the live price, so there is no drift to measure.
	locked := t.forwardLocked != nil
	var commitEff effectiveFXRate
	if locked {
		commitEff = forwardLockedEffectiveRate(t.forwardLocked, t.Currency, toCurrency)
	} else {
		var err error
		commitEff, err = l.effectiveFXRateLocked(pair, side, now)
		if err != nil {
			return TransferReceipt{}, err
		}
	}
	rate := commitEff.Rate // the market-table entry, for provenance disclosure
	effNum, effDen := commitEff.Num, commitEff.Den
	effVersion := commitEff.EffectiveVersion
	effExpiresAt := commitEff.ExpiresAt
	override := commitEff.Override
	overridden := commitEff.Overridden
	// LG-86: settle at the admitted price. The admission quote and the
	// commit-time rate can differ — the rate table moves under the same
	// write lock, so a concurrent SetFXRate between admission and this
	// commit is a real window. The drift is measured in basis points
	// (exact big.Rat math, see fxSlippageBps) and gated against the
	// caller's opt-in cap: a breach records nothing, exactly like the
	// other commit-time rejections. The quote is only honored when it
	// names this transfer's pair — an admission taken for other
	// currencies (or a zero quote) means commit-time semantics.
	var admitted *FXRateQuote
	var slippageBps int64
	switch {
	case locked:
		// The locked price is the quote: admitted == committed ==
		// locked, slippage zero by construction (see the note
		// above). The admission provenance (admitted_at, the
		// rate-table snapshot version) comes from the contract's
		// booking, not from a live admission — a forward has no
		// admission window, only the bilateral agreement.
		q := t.forwardLocked.quote(t.Currency, toCurrency)
		admitted = &q
	case quote.From != "" && quote.From == t.Currency && quote.To == toCurrency:
		slippageBps = fxSlippageBps(commitEff.Num, commitEff.Den, quote.Num, quote.Den)
		if t.MaxSlippageBps > 0 && slippageBps > t.MaxSlippageBps {
			return TransferReceipt{}, ErrFXSlippageExceeded
		}
		// The rate move after admission changes disclosure, never the
		// booked legs: the transfer settles at the admitted price.
		effNum, effDen = quote.Num, quote.Den
		effVersion = quote.EffectiveVersion
		effExpiresAt = quote.ExpiresAt
		overridden = quote.OverrideApplied
		if overridden {
			// The admitted price was an override: rebind its
			// provenance from the quote (the commit-time table may
			// have moved on since admission). Proposer and reason
			// ride the quote too: the fx_rate_overridden audit
			// event binds every overridden conversion to the
			// override that priced it.
			override = RateOverride{
				Num:              quote.Num,
				Den:              quote.Den,
				Approver:         quote.OverrideApprovedBy,
				Proposer:         quote.OverrideProposedBy,
				Reason:           quote.OverrideReason,
				EffectiveVersion: quote.EffectiveVersion,
				ExpiresAt:        quote.ExpiresAt,
			}
		} else {
			override = RateOverride{}
		}
		admitted = &quote
	}
	fxAccount := t.FXAccount
	if fxAccount == "" {
		fxAccount = l.fxAccount
	}
	if fxAccount == "" {
		return TransferReceipt{}, ErrFXAccountNotConfigured
	}
	if fxAccount == t.From || fxAccount == t.To {
		return TransferReceipt{}, ErrInvalidFXAccount
	}
	converted, ok := fxConvertCents(t.AmountCents, effNum, effDen)
	if !ok {
		return TransferReceipt{}, ErrAmountOverflow
	}
	// The payee attribution (if any) books the converted amount in the
	// settlement currency.
	if toAttr != nil {
		toAttr.AmountCents = converted
	}

	// LG-93: the dealing spread. midNum/midDen is the reference mid the
	// spread is measured against — the admission mid on the admitted
	// path (the transfer settled at the admitted dealing price, so its
	// spread is measured against the admitted mid), the commit-time mid
	// otherwise. sideConverted is the floored conversion at the dealing
	// price the transfer settled at (converted, above); midConverted
	// the floored conversion at mid. Sell-side the desk keeps
	// mid − bid, buy-side ask − mid — always ≥ 0 (see
	// fxSpreadCentsOf). A mid-only rate, an active override (bid = ask
	// = mid), or spread dust all measure zero: no leg books.
	midNum, midDen := commitEff.MidNum, commitEff.MidDen
	bidNum, bidDen := commitEff.BidNum, commitEff.BidDen
	askNum, askDen := commitEff.AskNum, commitEff.AskDen
	if admitted != nil {
		midNum, midDen = admitted.MidNum, admitted.MidDen
		bidNum, bidDen = admitted.BidNum, admitted.BidDen
		askNum, askDen = admitted.AskNum, admitted.AskDen
	}
	midConverted, ok := fxConvertCents(t.AmountCents, midNum, midDen)
	if !ok {
		return TransferReceipt{}, ErrAmountOverflow
	}
	spreadCents := fxSpreadCentsOf(side, converted, midConverted)
	spreadBps := fxSpreadBps(bidNum, bidDen, askNum, askDen, midNum, midDen)
	// Fail-closed, before any mutation: a positive spread with no usable
	// income account rejects the transfer — the P&L must have somewhere
	// to go (see resolveFXIncomeAccountLocked).
	incomeAccount, err := l.resolveFXIncomeAccountLocked(t, fxAccount, spreadCents)
	if err != nil {
		return TransferReceipt{}, err
	}

	// Virtual-account attribution, continued: the FX clearing account
	// counters both legs and the spread-income account pays the spread
	// leg — a VAN on either paying leg is rejected (ErrVANNoDebit), a
	// zero-balance account can be neither a clearing account nor a P&L
	// source. Fee recipients attribute to their parents, like the plain
	// path.
	if err := l.vanDebitRejectedLocked(fxAccount); err != nil {
		return TransferReceipt{}, err
	}
	if err := l.vanDebitRejectedLocked(incomeAccount); err != nil {
		return TransferReceipt{}, err
	}
	var fa *vanAttribution
	if feeAccount != "" {
		if feeAccount, fa = l.attributeVANRecipientLocked(feeAccount, feeCents, t.Currency, t.ID+"/fee"); fa != nil {
			vanAttrs = append(vanAttrs, fa)
		}
	}
	for i := range feeSplits {
		if feeSplits[i].Account, fa = l.attributeVANRecipientLocked(feeSplits[i].Account, feeSplits[i].FeeCents, t.Currency, t.ID+"/fee/"+strconv.Itoa(i)); fa != nil {
			vanAttrs = append(vanAttrs, fa)
		}
	}

	feeEntryIDs := feeLegIDs(t.ID, feeCents, feeSplits)
	// A split transfer reserves the "<id>/chunk-0" namespace for its
	// chunk entries (see PostTransferSplit): a plain transfer reusing a
	// split transfer's ID would create two transfers sharing one
	// identity — the non-FX path checks this too (transfer.go).
	for _, id := range append([]string{t.ID, t.ID + "/fx", t.ID + "/spread", t.ID + "/chunk-0"}, feeEntryIDs...) {
		if id == "" {
			continue
		}
		if _, exists := l.entries[id]; exists {
			return TransferReceipt{}, ErrTransferIDConflict
		}
	}
	// External-reference uniqueness, like in PostTransfer: after the
	// idempotency replay check and the ID-conflict checks, before
	// anything books. The reference rides the /fx leg (the memo-carrying
	// leg) on this path, not the principal.
	if t.ExternalRef != "" {
		if owner, taken := l.byRef[t.ExternalRef]; taken {
			return TransferReceipt{}, fmt.Errorf("%w: reference %q already journaled on entry %q", ErrDuplicateExternalRef, t.ExternalRef, owner)
		}
	}

	// Frozen and overdraft checks cover every account a leg touches. The
	// payer's risk is entirely in the source currency: overdraft and the
	// daily limit both guard the total outflow (amount + fee) against the
	// source-currency balance, never against the converted amount.
	if l.closedLocked(t.From) || l.closedLocked(t.To) || l.closedLocked(fxAccount) ||
		(incomeAccount != "" && l.closedLocked(incomeAccount)) {
		return TransferReceipt{}, ErrAccountClosed
	}
	for _, fa := range feeLegAccounts(feeAccount, feeSplits) {
		if l.closedLocked(fa) {
			return TransferReceipt{}, ErrAccountClosed
		}
	}
	if l.frozenLocked(t.From) || l.frozenLocked(t.To) || l.frozenLocked(fxAccount) ||
		(incomeAccount != "" && l.frozenLocked(incomeAccount)) {
		return TransferReceipt{}, ErrAccountFrozen
	}
	if l.suspendedLocked(t.From) || l.suspendedLocked(t.To) || l.suspendedLocked(fxAccount) ||
		(incomeAccount != "" && l.suspendedLocked(incomeAccount)) {
		return TransferReceipt{}, ErrAccountSuspended
	}
	// Every fee recipient walks the same frozen check the single fee
	// account always walked, like the non-FX path (transfer.go).
	for _, fa := range feeLegAccounts(feeAccount, feeSplits) {
		if l.frozenLocked(fa) {
			return TransferReceipt{}, ErrAccountFrozen
		}
		if l.suspendedLocked(fa) {
			return TransferReceipt{}, ErrAccountSuspended
		}
	}
	// Transfer beneficiary allowlist (see allowlist.go): after the
	// frozen gate and before the overdraft gate, like the non-FX path
	// (transfer.go) — only the principal leg's payee is screened, and
	// the fee leg's recipient is exempt.
	if l.allowlistRejectedLocked(t.From, t.To) {
		return TransferReceipt{}, l.allowlistRejectionLocked(t.From, t.To)
	}
	// Confirmation of payee (see payee_verification.go): after the
	// allowlist gate and before the overdraft gate, like the non-FX
	// path (transfer.go). t.To is already VAN-resolved above.
	payeeMatch, err := l.payeeVerificationLocked(t)
	if err != nil {
		return TransferReceipt{}, err
	}
	if l.noOverdraft[t.From] &&
		l.balances[accountCurrency{account: t.From, currency: t.Currency}] < totalOutflow {
		return TransferReceipt{}, ErrAccountOverdraft
	}
	// KYC/尽调门控 (see kyc.go): after the overdraft check — both are
	// account risk controls — and after the idempotency replay check
	// above: a replay books nothing new, so it must not fail on an
	// account whose KYC lapsed after the original transfer. Expired KYC
	// profiles sweep lazily here (the FX path branches before the main
	// transfer path's sweep), so this transfer sees the lapsed account
	// expired. Only the principal legs are gated: the FX clearing
	// account and the operator's fee recipients are exempt, like the
	// fee-leg carve-out on the non-FX path.
	l.expireKycLocked(now)
	if err := l.kycRejectedLocked(t.From, t.To, totalOutflow); err != nil {
		return TransferReceipt{}, err
	}
	if l.dailyLimitRejectedLocked(t.From, t.Currency, totalOutflow, t.CreatedAt) {
		return TransferReceipt{}, ErrDailyLimitExceeded
	}
	if l.budgetRejectedLocked(t.From, t.Currency, totalOutflow, t.CreatedAt) {
		return TransferReceipt{}, ErrBudgetExceeded
	}

	// The period gate is keyed on the transfer's effective post time (see
	// period.go): a backdated transfer landing in a closed accounting
	// period is rejected with ErrPeriodClosed. It runs after the
	// idempotency replay check, so replaying a key posted before the
	// period closed returns the original receipt.
	at := t.CreatedAt
	if at.IsZero() {
		at = time.Now()
	}
	if err := l.periodRejectedLocked(at); err != nil {
		return TransferReceipt{}, err
	}

	// Every leg is single-currency by construction; the journal entries
	// keep each currency's books balanced independently, with the FX
	// account as the shared counterparty. The optional spread leg
	// (LG-93) books right after the /fx leg: it is part of the
	// conversion, so the review gate freezes it with the other legs.
	entries := []JournalEntry{
		{
			ID:             t.ID,
			DebitAccount:   t.To,
			CreditAccount:  fxAccount,
			AmountCents:    converted,
			Currency:       toCurrency,
			IdempotencyKey: t.IdempotencyKey,
			CreatedAt:      t.CreatedAt,
		},
		{
			ID:            t.ID + "/fx",
			DebitAccount:  fxAccount,
			CreditAccount: t.From,
			AmountCents:   t.AmountCents,
			Currency:      t.Currency,
			Memo:          t.Memo,
			ExternalRef:   t.ExternalRef,
			Attachments:   t.Attachments,
			Metadata:      t.Metadata,
			PurposeCode:   t.PurposeCode,
			CreatedAt:     t.CreatedAt,
		},
	}
	if spreadCents > 0 {
		entries = append(entries, buildFXSpreadLeg(t.ID, fxAccount, incomeAccount, spreadCents, toCurrency, t.CreatedAt))
	}
	if feeCents > 0 {
		// The fee legs — one "<transfer ID>/fee" leg for a single fee
		// account, or one "<transfer ID>/fee/<i>" leg per split
		// recipient — book in the source currency, on top of the
		// amount, like the non-FX path.
		entries = append(entries, buildFeeLegEntries(t, feeCents, feeAccount, feeSplits, t.CreatedAt)...)
	}

	// The dual-control review gate (see review.go) runs last, after the
	// period gate, like in PostTransfer: a cross-currency transfer at or
	// above the payer's review threshold freezes instead of settling.
	// The threshold applies to the source-currency principal amount.
	fxConv := &FXConversion{
		FromCurrency:     t.Currency,
		ToCurrency:       toCurrency,
		SourceCents:      t.AmountCents,
		RateNum:          effNum,
		RateDen:          effDen,
		ConvertedCents:   converted,
		EffectiveVersion: effVersion,
		RateExpiresAt:    effExpiresAt,
		// LG-45: pin the conversion to the current snapshot version —
		// the receipt stays reproducible after the rate changes.
		// LG-110: a forward settlement pins the contract's booking-time
		// snapshot version instead (see forward.go) — the market table
		// the agreement was priced against, not the live one.
		RateSnapshotVersion: l.fxSnapshotVersion,
	}
	if locked {
		fxConv.RateSnapshotVersion = t.forwardLocked.RateSnapshotVersion
	}
	if admitted != nil {
		// LG-86: disclose the admission quote the transfer settled
		// at and the drift the commit-time rate had from it.
		fxConv.AdmittedRateNum = admitted.Num
		fxConv.AdmittedRateDen = admitted.Den
		fxConv.AdmittedEffectiveVersion = admitted.EffectiveVersion
		fxConv.AdmittedAt = admitted.AdmittedAt
		fxConv.SlippageBps = slippageBps
	}
	if overridden {
		fxConv.OverrideApplied = true
		fxConv.MarketRateNum = rate.Num
		fxConv.MarketRateDen = rate.Den
		fxConv.OverrideApprovedBy = override.Approver
	}
	// LG-93: disclose the dealing side, the two-sided quote, and the
	// spread. RateNum/RateDen above name the dealing price that
	// converted; the mid and the full quote ride alongside so the
	// spread is auditable without the rate table.
	fxConv.Direction = side
	fxConv.MidNum, fxConv.MidDen = midNum, midDen
	fxConv.BidNum, fxConv.BidDen = bidNum, bidDen
	fxConv.AskNum, fxConv.AskDen = askNum, askDen
	fxConv.SpreadBps = spreadBps
	fxConv.SpreadCents = spreadCents
	fxConv.FXIncomeAccount = incomeAccount
	// The journal memo marker for attributed legs (see
	// virtual_account.go): applied before the review gate so
	// review-frozen rows carry it too.
	if err := annotateVANMemos(entries, vanAttrs); err != nil {
		return TransferReceipt{}, err
	}
	if receipt, reviewed, err := l.maybeReviewTransferLocked(t, entries, feeCents, tierIndex, rateBps, fxConv, totalOutflow, now); reviewed || err != nil {
		// The override priced the frozen legs too: record the
		// re-pricing before returning the review receipt.
		if overridden {
			l.fxOverrideConversions++
			l.emitRateOverriddenAuditLocked(t, toCurrency, override, rate, converted)
		}
		// The spread leg is journaled (frozen) on the review path too:
		// count the booking here, like the direct path below.
		if reviewed && spreadCents > 0 {
			l.fxSpreadBookings++
			l.fxSpreadCentsTotal += spreadCents
		}
		return receipt, err
	}

	// Every conversion priced at an approved override is individually
	// auditable: the event binds the transfer to the override's
	// provenance (proposer, approver, reason, both rates, the measured
	// deviation). Nothing after the review gate can fail, so emitting
	// here covers the direct path exactly once — the review-frozen path
	// emitted its event above. The counter feeds
	// ledger_fx_rate_overridden_total.
	if overridden {
		l.fxOverrideConversions++
		l.emitRateOverriddenAuditLocked(t, toCurrency, override, rate, converted)
	}

	// Atomic commit: journal rows, idempotency index, balances, totals,
	// chain links, and version bumps land together under the one write
	// lock, after all checks passed.
	//
	// Automatic posting rules (see posting_rules.go): cross-currency
	// transfers that match an installed rule append their derived
	// legs to the commit, like the non-FX path. Evaluated after every
	// gate — including the review gate, so review-frozen transfers
	// book no rule legs — and before the first mutation, so a failing
	// rule leg rejects the whole transfer with nothing recorded. The
	// rule context is the source currency: rule legs book in the
	// currency the payer is charged in.
	ruleLegs, ruleLegInfos, err := l.resolvePostingRuleLegsLocked(postingRuleContext{
		kind:        PostingEventTransfer,
		payer:       t.From,
		payee:       t.To,
		amountCents: t.AmountCents,
		currency:    t.Currency,
		triggerID:   t.ID,
	}, t.CreatedAt)
	if err != nil {
		return TransferReceipt{}, err
	}
	entries = append(entries, ruleLegs...)

	l.maybePruneIdempotencyKeys(now)
	l.maybePruneDailyOutflowLocked(now)
	l.maybePruneMonthlySpendLocked(now)
	versionBefore := l.version // Pre-commit balance snapshot for the opt-in balance-change hook
	// (see balance_hook.go): nil when no hook is registered.
	hookTouched := touchedAccounts(entries)
	hookOld := l.balanceSnapshotLocked(hookTouched)
	for _, e := range entries {
		l.commitEntryLocked(e)
	}
	l.addDailyOutflowLocked(t.From, t.Currency, totalOutflow, t.CreatedAt)
	l.addMonthlySpendLocked(t.From, t.Currency, totalOutflow, t.CreatedAt)
	if t.IdempotencyKey != "" {
		ids := make([]string, 0, len(entries))
		for _, e := range entries {
			ids = append(ids, e.ID)
		}
		l.transferKeys[t.IdempotencyKey] = ids
	}
	fxEntryIDs := make([]string, 0, len(entries))
	for _, e := range entries {
		fxEntryIDs = append(fxEntryIDs, e.ID)
	}
	// LG-93: the spread booking counters feed ledger_fx_spread_bookings_total
	// and ledger_fx_spread_cents_total. Counted at journal time, like the
	// override counter above.
	if spreadCents > 0 {
		l.fxSpreadBookings++
		l.fxSpreadCentsTotal += spreadCents
	}
	l.countPostingRuleLegsLocked(ruleLegInfos)
	// Low-balance alert evaluation: strictly after the atomic commit
	// zone, read-only (see low_balance.go). Advisory only.
	l.evaluateBalanceWatchesLocked(touchedAccounts(entries), t.ID, "PostTransfer")
	fxDetails := map[string]any{
		"amount_cents":    t.AmountCents,
		"currency":        t.Currency,
		"to_currency":     toCurrency,
		"source_cents":    t.AmountCents,
		"converted_cents": converted,
		"rate_num":        rate.Num,
		"rate_den":        rate.Den,
		// The conversion's full provenance, sealed by the audit log's
		// hash chain (see audit.go): the pre-conversion amount, the
		// post-conversion amount, and the exact rate that produced
		// it. The journal's own audit chain already carries both
		// legs (the /fx leg books the source amount, the principal
		// the converted amount); this event binds them to the rate.
		"rate_effective_version": rate.EffectiveVersion,
		"fee_cents":              feeCents,
		// LG-93: the dealing side, the two-sided quote, and the spread —
		// sealed with the rest of the conversion's provenance.
		"direction":    side,
		"mid_rate":     fmt.Sprintf("%d/%d", midNum, midDen),
		"bid_rate":     fmt.Sprintf("%d/%d", bidNum, bidDen),
		"ask_rate":     fmt.Sprintf("%d/%d", askNum, askDen),
		"spread_bps":   spreadBps,
		"spread_cents": spreadCents,
	}
	if incomeAccount != "" {
		fxDetails["fx_income_account"] = string(incomeAccount)
	}
	if admitted != nil {
		// LG-86: the admission quote the transfer settled at, the live
		// rate at commit it was measured against, the drift in basis
		// points, and the caller's cap when one was set — sealed by
		// the audit log's hash chain with the rest of the conversion's
		// provenance. The no-admission paths omit these keys and keep
		// the pre-LG-86 disclosure shape.
		fxDetails["admitted_rate"] = map[string]any{
			"num":               admitted.Num,
			"den":               admitted.Den,
			"effective_version": admitted.EffectiveVersion,
			"admitted_at":       admitted.AdmittedAt.UTC().Format(time.RFC3339Nano),
		}
		fxDetails["committed_rate"] = map[string]any{
			"num":               commitEff.Num,
			"den":               commitEff.Den,
			"effective_version": commitEff.EffectiveVersion,
		}
		fxDetails["slippage_bps"] = slippageBps
		if t.MaxSlippageBps > 0 {
			fxDetails["max_slippage_bps"] = t.MaxSlippageBps
		}
		if locked {
			// LG-110: bind the settlement transfer to its forward
			// contract. admitted_rate == committed_rate == the
			// locked rate here (see the locked case above), and
			// slippage_bps is 0 by construction.
			fxDetails["forward_id"] = t.forwardLocked.ForwardID
		}
	}
	if len(feeSplits) > 0 {
		// The per-recipient split breakdown enters the structured
		// audit, like the non-FX path.
		fxDetails["fee_splits"] = feeSplitAuditDetails(feeSplits)
	}
	if len(ruleLegInfos) > 0 {
		// The automatic posting-rule legs enter the structured
		// audit, like the non-FX path (see posting_rules.go).
		fxDetails["posting_rule_legs"] = postingRuleDetails(ruleLegInfos)
	}
	if overridden {
		fxDetails["override_applied"] = true
		fxDetails["override_rate"] = fmt.Sprintf("%d/%d", effNum, effDen)
		fxDetails["market_rate"] = fmt.Sprintf("%d/%d", rate.Num, rate.Den)
		fxDetails["override_approved_by"] = override.Approver
	}
	if t.Memo != "" {
		fxDetails["memo"] = t.Memo
	}
	if t.ExternalRef != "" {
		fxDetails["external_ref"] = t.ExternalRef
	}
	if t.Rail != "" {
		// LG-185: the settlement rail rides the audit trail, like
		// the memo and the external reference.
		fxDetails["rail"] = t.Rail
	}
	l.emitAudit(AuditEvent{
		Op:            "transfer",
		Actor:         "PostTransfer",
		TraceID:       t.ID,
		VersionBefore: versionBefore,
		VersionAfter:  l.version,
		EntryIDs:      fxEntryIDs,
		Accounts:      []AccountID{t.From, t.To},
		Details:       fxDetails,
	})
	// Settlement cutoff evaluation (see settlement_cutoff.go): the
	// booking is never blocked — the journal commits first, the
	// settlement flag second. The cutoff is evaluated on the
	// transfer's own (source) currency.
	settlementStatus, settlementWindow := l.deferSettlementIfPastCutoffLocked(
		t.Currency, t.CreatedAt, t.ID, fxEntryIDs, []AccountID{t.From, t.To})
	// Virtual-account attributions ride their own audit events (see
	// virtual_account.go), after the transfer's own event. Review-routed
	// transfers return above, so only settled transfers attribute here.
	l.emitVANAttributionsLocked(vanAttrs, t.ID, "PostTransfer")
	// Domain outbox: entry_posted per leg (principal + FX leg + fee
	// leg), then the transfer_posted envelope with the conversion's
	// provenance.
	l.emitOutboxTransferLocked(t, feeCents, entries, map[string]any{
		"to_currency":     toCurrency,
		"converted_cents": converted,
		"rate_num":        rate.Num,
		"rate_den":        rate.Den,
		// LG-93: the dealing side and the spread on the envelope, so
		// downstream consumers see the P&L without parsing legs.
		"direction":    side,
		"spread_bps":   spreadBps,
		"spread_cents": spreadCents,
	})
	// Balance-change notification: strictly after the atomic commit
	// zone, advisory only — async dispatch, hook failures isolated.
	l.fireBalanceHooksLocked(hookTouched, hookOld, t.ID)

	// LG-185: a rail-bound FX transfer opens its settlement-SLA watch
	// at commit, like the same-currency path above.
	if t.Rail != "" {
		l.openSLAWatchLocked(t.ID, SLAWatchKindTransfer, t.Rail, now)
	}

	return TransferReceipt{
		TransferID:       t.ID,
		Entries:          entries,
		FeeCents:         feeCents,
		FeeTierIndex:     tierIndex,
		FeeRateBps:       rateBps,
		Duplicate:        false,
		FX:               fxConv,
		FeeSplits:        feeSplits,
		RuleLegs:         ruleLegInfos,
		SettlementStatus: settlementStatus,
		SettlementWindow: settlementWindow,
		PayeeMatch:       payeeMatch,
	}, nil
}
