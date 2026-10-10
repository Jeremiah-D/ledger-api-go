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
)

// fxPair is the directional lookup key of the rate table: (from, to) and
// (to, from) are independent entries.
type fxPair struct {
	from string
	to   string
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
	er := ExchangeRate{
		FromCurrency:     f,
		ToCurrency:       t,
		Num:              num.Int64(),
		Den:              den.Int64(),
		EffectiveVersion: l.version,
	}
	if ttl > 0 {
		er.ExpiresAt = time.Now().Add(ttl)
	}
	l.fxRates[fxPair{from: f, to: t}] = er
	return nil
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
// both sides — SourceCents before conversion, ConvertedCents after. It is
// carried on TransferReceipt for the original posting; idempotent replays
// return FX == nil, because the rate table may have changed since — the
// replayed journal entries are the authoritative record.
type FXConversion struct {
	FromCurrency     string    `json:"from_currency"`
	ToCurrency       string    `json:"to_currency"`
	SourceCents      int64     `json:"source_cents"`
	RateNum          int64     `json:"rate_num"`
	RateDen          int64     `json:"rate_den"`
	ConvertedCents   int64     `json:"converted_cents"`
	EffectiveVersion uint64    `json:"effective_version"`
	RateExpiresAt    time.Time `json:"rate_expires_at,omitempty"`
}

// postTransferFXLocked records a cross-currency transfer: t.Currency is
// the source currency, toCurrency the settlement currency
// (toCurrency != t.Currency). Callers must hold the write lock; every
// check runs before the first mutation, so a rejection records nothing.
func (l *Ledger) postTransferFXLocked(t Transfer, toCurrency string, now time.Time) (TransferReceipt, error) {
	feeCents, feeAccount, tierIndex, rateBps, err := l.resolveFeeLocked(t)
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

	rate, ok := l.fxRates[fxPair{from: t.Currency, to: toCurrency}]
	if !ok {
		return TransferReceipt{}, ErrFXRateMissing
	}
	// A rate past its expiry is never applied silently: the transfer is
	// rejected until the operator installs a fresh rate. The check runs
	// against the transfer's post time and after the idempotency replay
	// check above, so replaying a key posted while the rate was live
	// still returns the original receipt.
	if !rate.ExpiresAt.IsZero() && !now.Before(rate.ExpiresAt) {
		return TransferReceipt{}, ErrFXRateExpired
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
	converted, ok := fxConvertCents(t.AmountCents, rate.Num, rate.Den)
	if !ok {
		return TransferReceipt{}, ErrAmountOverflow
	}

	feeEntryID := ""
	if feeCents > 0 {
		feeEntryID = t.ID + "/fee"
	}
	for _, id := range []string{t.ID, t.ID + "/fx", feeEntryID} {
		if id == "" {
			continue
		}
		if _, exists := l.entries[id]; exists {
			return TransferReceipt{}, ErrTransferIDConflict
		}
	}

	// Frozen and overdraft checks cover every account a leg touches. The
	// payer's risk is entirely in the source currency: overdraft and the
	// daily limit both guard the total outflow (amount + fee) against the
	// source-currency balance, never against the converted amount.
	if l.frozenLocked(t.From) || l.frozenLocked(t.To) || l.frozenLocked(fxAccount) ||
		(feeAccount != "" && l.frozenLocked(feeAccount)) {
		return TransferReceipt{}, ErrAccountFrozen
	}
	if l.noOverdraft[t.From] &&
		l.balances[accountCurrency{account: t.From, currency: t.Currency}] < totalOutflow {
		return TransferReceipt{}, ErrAccountOverdraft
	}
	if l.dailyLimitRejectedLocked(t.From, t.Currency, totalOutflow, t.CreatedAt) {
		return TransferReceipt{}, ErrDailyLimitExceeded
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

	// Every leg is single-currency by construction; the two journal
	// entries keep each currency's books balanced independently, with the
	// FX account as the shared counterparty.
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
			CreatedAt:     t.CreatedAt,
		},
	}
	if feeCents > 0 {
		feeKey := ""
		if t.IdempotencyKey != "" {
			feeKey = t.IdempotencyKey + "/fee"
		}
		entries = append(entries, JournalEntry{
			ID:             feeEntryID,
			DebitAccount:   feeAccount,
			CreditAccount:  t.From,
			AmountCents:    feeCents,
			Currency:       t.Currency,
			IdempotencyKey: feeKey,
			CreatedAt:      t.CreatedAt,
		})
	}

	// Atomic commit: journal rows, idempotency index, balances, totals,
	// chain links, and version bumps land together under the one write
	// lock, after all checks passed.
	l.maybePruneIdempotencyKeys(now)
	l.maybePruneDailyOutflowLocked(now)
	versionBefore := l.version
	for _, e := range entries {
		l.commitEntryLocked(e)
	}
	l.addDailyOutflowLocked(t.From, t.Currency, totalOutflow, t.CreatedAt)
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
	// Low-balance alert evaluation: strictly after the atomic commit
	// zone, read-only (see low_balance.go). Advisory only.
	l.evaluateLowBalanceLocked(touchedAccounts(entries), t.ID, "PostTransfer")
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
	}
	if t.Memo != "" {
		fxDetails["memo"] = t.Memo
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

	return TransferReceipt{
		TransferID:   t.ID,
		Entries:      entries,
		FeeCents:     feeCents,
		FeeTierIndex: tierIndex,
		FeeRateBps:   rateBps,
		Duplicate:    false,
		FX: &FXConversion{
			FromCurrency:     t.Currency,
			ToCurrency:       toCurrency,
			SourceCents:      t.AmountCents,
			RateNum:          rate.Num,
			RateDen:          rate.Den,
			ConvertedCents:   converted,
			EffectiveVersion: rate.EffectiveVersion,
			RateExpiresAt:    rate.ExpiresAt,
		},
	}, nil
}
