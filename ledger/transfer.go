package ledger

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Validation errors returned by PostTransfer.
var (
	ErrEmptyTransferID           = errors.New("ledger: transfer ID must not be empty")
	ErrEmptyFromAccount          = errors.New("ledger: transfer from-account must not be empty")
	ErrEmptyToAccount            = errors.New("ledger: transfer to-account must not be empty")
	ErrTransferSameAccount       = errors.New("ledger: transfer from- and to-accounts must differ")
	ErrTransferNonPositiveAmount = errors.New("ledger: transfer amount must be greater than zero")
	// ErrTransferIDConflict is returned when the transfer ID is already
	// used as a journal entry ID (transfers book their principal entry
	// under the transfer ID, so the two namespaces must not collide).
	ErrTransferIDConflict = errors.New("ledger: transfer ID already used as a journal entry ID")
	// ErrInvalidFee is returned when the fee leg is misconfigured: a
	// negative fee, a positive fee without a fee account, or a fee
	// account without a positive fee.
	ErrInvalidFee = errors.New("ledger: invalid transfer fee leg")
	// ErrAmountOverflow is returned when the transfer amount plus the fee
	// overflows int64: no account balance could cover the total, so the
	// transfer is rejected before anything is recorded.
	ErrAmountOverflow = errors.New("ledger: transfer amount plus fee overflows int64")
)

// Transfer describes one atomic money movement from one account to another.
//
// It is the payment-domain view of a double-entry posting: callers name the
// payer (From) and the payee (To) instead of debit/credit legs. PostTransfer
// books a single principal entry — debit To (its balance grows), credit
// From (its balance shrinks) — following the ledger's sign convention. The
// journal entry carries the transfer's ID, so transfers are directly
// visible in journal exports and the audit chain.
//
// A transfer may carry a fee leg (see FeeCents, FeeAccount, and the
// ledger-wide fee policy in WithTransferFeePolicy): the payer additionally
// pays FeeCents to the fee account, booked as a second journal entry
// (debit fee account, credit payer) with ID "<transfer ID>/fee". The fee is
// charged on top of the transfer amount — the payee receives the full
// amount, the payer's total outflow is amount + fee.
//
// A transfer may also settle in a different currency (see ToCurrency and
// fx.go): the payee is debited the converted amount in the target
// currency while the payer is credited the original amount in the source
// currency, with the FX clearing account as the counterparty of both legs
// (entry "<transfer ID>" and "<transfer ID>/fx"). The fee leg, when
// present, stays in the source currency. The receipt discloses the
// conversion in its FX field.
//
// The transfer commits atomically: field validation, fee resolution, the
// idempotency replay check, and the frozen/overdraft risk checks all run
// before anything is recorded, so a transfer that fails on any leg —
// principal or fee — records nothing: no journal rows, no chain links, no
// version bump. Readers never observe a half-posted transfer.
type Transfer struct {
	ID          string    `json:"transfer_id"`
	From        AccountID `json:"from_account"`
	To          AccountID `json:"to_account"`
	AmountCents int64     `json:"amount_cents"`
	// Currency is the ISO 4217 alpha-3 code the transfer is denominated
	// in (e.g. "USD", "EUR"). Empty means the default currency (see
	// DefaultCurrency). Every leg of the transfer — principal and fee —
	// is booked in this one currency, unless ToCurrency names a
	// different settlement currency (see below).
	Currency string `json:"currency,omitempty"`
	// ToCurrency names the settlement currency of a cross-currency
	// transfer: the payee is debited the converted amount in this
	// currency while the payer is credited the original amount in
	// Currency, with the configured FX account as the counterparty of
	// both legs (see fx.go). Empty means "same as Currency": a plain
	// single-currency transfer that needs no rate. A non-empty code must
	// be a 3-letter uppercase ISO 4217 code, must differ from Currency,
	// and needs a configured rate for (Currency, ToCurrency), or
	// PostTransfer rejects the transfer with ErrFXRateMissing (HTTP
	// 422). The fee leg, when present, is always booked in Currency on
	// top of the amount.
	ToCurrency string `json:"to_currency,omitempty"`
	// FXAccount overrides the ledger's configured FX clearing account
	// for this cross-currency transfer. It must differ from From and To.
	// It is ignored for same-currency transfers.
	FXAccount AccountID `json:"fx_account,omitempty"`
	// FeeCents is an explicit per-transfer fee, in cents, charged to the
	// payer (From) on top of AmountCents and booked to FeeAccount. It must
	// be non-negative; a positive fee requires FeeAccount. An explicit fee
	// always wins over the ledger's fee policy. Zero means "no explicit
	// fee": the fee policy applies unless SkipFee is set.
	FeeCents int64 `json:"fee_cents,omitempty"`
	// FeeAccount receives the fee leg. It must be set if and only if an
	// explicit positive fee is given; the fee policy supplies its own
	// revenue account.
	FeeAccount AccountID `json:"fee_account,omitempty"`
	// SkipFee suppresses the ledger's default fee policy for this
	// transfer. An explicit FeeCents still applies.
	SkipFee        bool      `json:"skip_fee,omitempty"`
	IdempotencyKey string    `json:"idempotency_key,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// TransferReceipt reports what PostTransfer committed.
//
// Entries holds the journal entries the transfer posted, in commit order:
// the principal entry, followed by the fee entry when the transfer carried
// a fee leg. FeeCents reports the fee actually booked (0 when there is no
// fee leg). FeeTierIndex and FeeRateBps disclose the fee schedule tier
// that applied: the 0-based index of the tier and its basis-point rate,
// exactly as the fee was computed. They are -1 and 0 when no policy tier
// applied — no fee leg was booked, the fee came from an explicit FeeCents,
// or SkipFee/the policy was disabled. Duplicate replays return the
// originally posted entries with Duplicate == true and book nothing new,
// carrying the same tier disclosure as the original receipt.
type TransferReceipt struct {
	TransferID   string         `json:"transfer_id"`
	Entries      []JournalEntry `json:"entries"`
	FeeCents     int64          `json:"fee_cents"`
	FeeTierIndex int            `json:"fee_tier_index"`
	FeeRateBps   int64          `json:"fee_rate_bps"`
	Duplicate    bool           `json:"duplicate"`
	// FX discloses the conversion of a cross-currency transfer (see
	// fx.go). It is set on the original posting; idempotent replays
	// return FX == nil because the rate table may have changed since —
	// the replayed journal entries are the authoritative record.
	FX *FXConversion `json:"fx,omitempty"`
}

// FeeTier is one band of the tiered transfer fee schedule: transfers of
// at least MinAmountCents (and below the next tier's minimum) are charged
// floor(amount * RateBps / 10000) cents. MinAmountCents is in cents and
// RateBps is basis points (250 = 2.5%). A RateBps of 0 makes the band
// fee-free — the standard way to model a dust allowance.
type FeeTier struct {
	MinAmountCents int64 `json:"min_amount_cents"`
	RateBps        int64 `json:"rate_bps"`
}

// WithTransferFeePolicy sets the ledger-wide default fee policy for
// transfers as a single flat tier: unless a transfer carries an explicit
// fee or sets SkipFee, PostTransfer books an additional fee leg of
// floor(amount * rateBps / 10000) cents to revenueAccount. It is kept as
// the one-tier convenience over WithTransferFeeSchedule: a flat rate is a
// schedule with a single tier starting at 0. rateBps is basis points
// (250 = 2.5%); a zero rate or an empty revenue account disables the
// policy. Negative rates are normalized to 0, and rates above 10000 bps
// (100%) are clamped to 10000 — a fee above the transferred amount is a
// configuration bug, and the clamp keeps fee arithmetic overflow-safe.
func WithTransferFeePolicy(rateBps int64, revenueAccount AccountID) Option {
	if rateBps < 0 {
		rateBps = 0
	}
	if rateBps > 10000 {
		rateBps = 10000
	}
	return WithTransferFeeSchedule([]FeeTier{{MinAmountCents: 0, RateBps: rateBps}}, revenueAccount)
}

// WithTransferFeeSchedule sets the ledger-wide default fee policy as an
// amount-tiered table: a transfer of amount A falls into the last tier
// whose MinAmountCents <= A and is charged
// floor(A * tier.RateBps / 10000) cents to revenueAccount, unless the
// transfer carries an explicit fee or sets SkipFee. The classic shape is
// fee-free dust, a standard middle band, and a cheaper top band that acts
// as an effective cap on large transfers, e.g.
//
//	[]ledger.FeeTier{
//		{MinAmountCents: 0, RateBps: 0},        // dust: free
//		{MinAmountCents: 10000, RateBps: 250},  // $100+: 2.5%
//		{MinAmountCents: 1000000, RateBps: 100}, // $10k+: 1% (effective cap)
//	}
//
// An empty tier list or an empty revenue account disables the policy.
// Tiers must be sorted by strictly increasing MinAmountCents and the first
// tier must start at 0, so every positive amount matches exactly one
// tier. Per-tier rates are normalized like the flat policy: negatives
// become 0, anything above 10000 bps clamps to 10000.
//
// Misconfiguration is fail-fast: an invalid tier table panics at
// construction time, so a bad schedule can never silently under- or
// over-charge. (Operator-supplied config should go through
// ParseFeeSchedule, which reports the same problems as an error instead.)
func WithTransferFeeSchedule(tiers []FeeTier, revenueAccount AccountID) Option {
	return func(l *Ledger) {
		l.feeTiers, l.feeRevenueAccount = normalizeFeeSchedule(tiers, revenueAccount)
	}
}

// normalizeFeeSchedule validates and canonicalizes a fee schedule: it
// returns a defensive copy of the tiers with rates clamped to
// [0, 10000], or (nil, "") when the policy is disabled. Structural
// problems panic — fail-fast at construction, never silent mispricing.
func normalizeFeeSchedule(tiers []FeeTier, revenueAccount AccountID) ([]FeeTier, AccountID) {
	if len(tiers) == 0 || revenueAccount == "" {
		return nil, ""
	}
	out := make([]FeeTier, len(tiers))
	for i, t := range tiers {
		if t.MinAmountCents < 0 {
			panic("ledger: fee schedule tier minimum must not be negative")
		}
		if i > 0 && t.MinAmountCents <= out[i-1].MinAmountCents {
			panic("ledger: fee schedule tier minimums must be strictly increasing")
		}
		rate := t.RateBps
		if rate < 0 {
			rate = 0
		}
		if rate > 10000 {
			rate = 10000
		}
		out[i] = FeeTier{MinAmountCents: t.MinAmountCents, RateBps: rate}
	}
	if out[0].MinAmountCents != 0 {
		panic("ledger: fee schedule first tier must start at 0")
	}
	return out, revenueAccount
}

// feeTierFor returns the 0-based index and rate of the tier a transfer of
// the given amount falls into: the last tier whose MinAmountCents is at
// most amount. Tiers are validated at construction (strictly increasing,
// first starting at 0), so every positive amount matches exactly one.
func (l *Ledger) feeTierFor(amount int64) (index int, rateBps int64) {
	idx := 0
	for i, t := range l.feeTiers {
		if amount < t.MinAmountCents {
			break
		}
		idx = i
	}
	return idx, l.feeTiers[idx].RateBps
}

// policyFeeCents computes floor(amount * rateBps / 10000) without
// overflowing int64 for any non-negative amount and rateBps <= 10000:
// amount = 10000*q + r, so amount*rate/10000 = q*rate + r*rate/10000
// exactly, and each term fits (q*rate <= amount <= MaxInt64 when
// rate <= 10000; r*rate <= 9999*10000).
func policyFeeCents(amount, rateBps int64) int64 {
	return amount/10000*rateBps + (amount%10000)*rateBps/10000
}

// addCents returns a+b and whether the sum fits in an int64.
func addCents(a, b int64) (int64, bool) {
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return 0, false
	}
	return a + b, true
}

// resolveFeeLocked determines the fee leg for a transfer: it returns the
// fee in cents, the account that receives it ("" when there is no fee
// leg), and the schedule tier that applied (index -1 and rate 0 when the
// fee came from an explicit FeeCents, SkipFee suppressed the policy, or
// the policy is disabled). Explicit fees win over the policy; the policy
// applies only when no explicit fee is given and SkipFee is false.
// Callers must hold l.mu.
func (l *Ledger) resolveFeeLocked(t Transfer) (feeCents int64, feeAccount AccountID, tierIndex int, rateBps int64, err error) {
	if t.FeeCents < 0 {
		return 0, "", -1, 0, ErrInvalidFee
	}
	if t.FeeCents > 0 {
		if t.FeeAccount == "" {
			return 0, "", -1, 0, ErrInvalidFee
		}
		return t.FeeCents, t.FeeAccount, -1, 0, nil
	}
	if t.FeeAccount != "" {
		// An explicit fee account without a positive fee is a caller bug:
		// silently ignoring it would misroute policy-computed fees.
		return 0, "", -1, 0, ErrInvalidFee
	}
	if t.SkipFee || len(l.feeTiers) == 0 || l.feeRevenueAccount == "" {
		return 0, "", -1, 0, nil
	}
	tierIndex, rateBps = l.feeTierFor(t.AmountCents)
	if fee := policyFeeCents(t.AmountCents, rateBps); fee > 0 {
		return fee, l.feeRevenueAccount, tierIndex, rateBps, nil
	}
	// A computed fee of zero (a free dust tier, or a tiny amount under a
	// low rate) posts no leg, but the tier that produced it is still
	// disclosed on the receipt.
	return 0, "", tierIndex, rateBps, nil
}

// ParseFeeSchedule parses the LEDGER_TRANSFER_FEE environment variable
// into a fee schedule and revenue account. Two syntaxes are accepted:
//
//	Flat (legacy):   "<rateBps>:<revenueAccount>"
//	                 e.g. "250:fee-revenue" for a flat 2.5%
//	Tiered:          "<min>:<bps>,<min>:<bps>,...@<revenueAccount>"
//	                 e.g. "0:0,10000:250,1000000:100@fee-revenue"
//	                 for fee-free dust, 2.5% from $100, 1% from $10k.
//
// Amounts are integer cents, rates are basis points. The tiered form is
// selected by the presence of "@" (everything after the first "@" is the
// revenue account); anything else is parsed as the flat form. Unlike the
// Go options, parsing is strict — a negative rate, a rate above 10000
// bps, a non-integer, an unsorted or duplicate tier minimum, a first tier
// not starting at 0, or a missing revenue account is an error, so a
// misconfigured deployment fails fast at startup instead of silently
// mispricing transfers.
func ParseFeeSchedule(raw string) ([]FeeTier, AccountID, error) {
	fail := func(format string, args ...any) ([]FeeTier, AccountID, error) {
		return nil, "", fmt.Errorf("ledger: invalid transfer fee schedule %q: "+format, append([]any{raw}, args...)...)
	}
	if strings.Contains(raw, "@") {
		// The revenue account is everything after the first "@", so an
		// account name containing "@" still parses; the tier list itself
		// never contains "@" (integer minimums and rates).
		spec, account, _ := strings.Cut(raw, "@")
		return parseTieredFeeSchedule(spec, account, fail)
	}
	rate, account, ok := strings.Cut(raw, ":")
	if !ok || account == "" {
		return fail("want \"<rateBps>:<revenueAccount>\" or \"<min>:<bps>,...@<revenueAccount>\"")
	}
	// The flat form takes a single account token: a "," or ":" in the
	// account means the caller meant the tiered form but forgot the "@",
	// so fail fast instead of misreading the schedule.
	if strings.ContainsAny(account, ",:") {
		return fail("revenue account %q must not contain \",\" or \":\" (want \"<rateBps>:<revenueAccount>\" or \"<min>:<bps>,...@<revenueAccount>\")", account)
	}
	bps, err := strconv.ParseInt(rate, 10, 64)
	if err != nil || bps < 0 || bps > 10000 {
		return fail("rate %q must be an integer 0..10000 (basis points)", rate)
	}
	return []FeeTier{{MinAmountCents: 0, RateBps: bps}}, AccountID(account), nil
}

// parseTieredFeeSchedule parses the tiered form selected by ParseFeeSchedule.
func parseTieredFeeSchedule(spec, account string, fail func(string, ...any) ([]FeeTier, AccountID, error)) ([]FeeTier, AccountID, error) {
	if account == "" {
		return fail("revenue account after \"@\" must not be empty")
	}
	if spec == "" {
		return fail("tier list before \"@\" must not be empty")
	}
	var tiers []FeeTier
	for _, seg := range strings.Split(spec, ",") {
		minRaw, bpsRaw, ok := strings.Cut(seg, ":")
		if !ok {
			return fail("tier %q must be \"<minAmountCents>:<rateBps>\"", seg)
		}
		min, err := strconv.ParseInt(strings.TrimSpace(minRaw), 10, 64)
		if err != nil || min < 0 {
			return fail("tier minimum %q must be a non-negative integer (cents)", minRaw)
		}
		bps, err := strconv.ParseInt(strings.TrimSpace(bpsRaw), 10, 64)
		if err != nil || bps < 0 || bps > 10000 {
			return fail("tier rate %q must be an integer 0..10000 (basis points)", bpsRaw)
		}
		tiers = append(tiers, FeeTier{MinAmountCents: min, RateBps: bps})
	}
	for i := 1; i < len(tiers); i++ {
		if tiers[i].MinAmountCents <= tiers[i-1].MinAmountCents {
			return fail("tier minimums must be strictly increasing")
		}
	}
	if tiers[0].MinAmountCents != 0 {
		return fail("first tier minimum must be 0")
	}
	return tiers, AccountID(account), nil
}

// PostTransfer records an atomic transfer from one account to another,
// with an optional fee leg charged to the payer on top of the amount.
//
// The risk checks mirror Post and run in the same order: the idempotency
// replay check first (replaying a key that was posted before an account was
// frozen or protected returns the original receipt instead of failing,
// because the replay books nothing new), then the frozen check on every
// account the transfer touches (payer, payee, and fee account), then the
// overdraft check on the payer against its total outflow (amount + fee),
// then the daily outflow limit check on the same total outflow. Rejected
// transfers — validation failures, ID conflicts, frozen rejections,
// overdraft rejections, and daily-limit rejections alike — record
// nothing: no journal rows, no chain links, no version bump.
//
// Idempotency shares the ledger-wide key namespace with Post: a key already
// used by either API replays the original entry instead of booking again.
// A transfer that posted a fee leg replays the full receipt (principal +
// fee entries). A zero CreatedAt is filled with the current time.
func (l *Ledger) PostTransfer(t Transfer) (TransferReceipt, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if t.ID == "" {
		return TransferReceipt{}, ErrEmptyTransferID
	}
	if t.From == "" {
		return TransferReceipt{}, ErrEmptyFromAccount
	}
	if t.To == "" {
		return TransferReceipt{}, ErrEmptyToAccount
	}
	if t.From == t.To {
		return TransferReceipt{}, ErrTransferSameAccount
	}
	if t.AmountCents <= 0 {
		return TransferReceipt{}, ErrTransferNonPositiveAmount
	}
	// Currency is field validation, like the accounts and the amount: an
	// empty code normalizes to the default currency, anything else must
	// be a 3-letter uppercase ISO 4217 code. Normalization happens
	// before the idempotency replay check, mirroring Post, so replays
	// compare canonical codes.
	currency, err := normalizeCurrency(t.Currency)
	if err != nil {
		return TransferReceipt{}, err
	}
	t.Currency = currency

	// ToCurrency empty means "settle in the transfer's own currency";
	// anything else must be a valid code naming a different currency,
	// taking the cross-currency path (see fx.go).
	toCurrency := currency
	if t.ToCurrency != "" {
		toCurrency, err = normalizeCurrency(t.ToCurrency)
		if err != nil {
			return TransferReceipt{}, err
		}
		t.ToCurrency = toCurrency
	}
	if toCurrency != currency {
		return l.postTransferFXLocked(t, toCurrency, time.Now())
	}

	feeCents, feeAccount, tierIndex, rateBps, err := l.resolveFeeLocked(t)
	if err != nil {
		return TransferReceipt{}, err
	}
	// The payer's total outflow must fit in int64; an overflowing total
	// could never be covered by any balance.
	totalOutflow, ok := addCents(t.AmountCents, feeCents)
	if !ok {
		return TransferReceipt{}, ErrAmountOverflow
	}

	if t.IdempotencyKey != "" {
		if receipt, ok := l.replayTransferLocked(t); ok {
			return receipt, nil
		}
	}

	feeEntryID := ""
	if feeCents > 0 {
		feeEntryID = t.ID + "/fee"
	}
	if _, exists := l.entries[t.ID]; exists {
		return TransferReceipt{}, ErrTransferIDConflict
	}
	if feeEntryID != "" {
		if _, exists := l.entries[feeEntryID]; exists {
			return TransferReceipt{}, ErrTransferIDConflict
		}
	}

	if l.frozenLocked(t.From) || l.frozenLocked(t.To) ||
		(feeAccount != "" && l.frozenLocked(feeAccount)) {
		return TransferReceipt{}, ErrAccountFrozen
	}

	// Overdraft protection guards the payer's total outflow (amount + fee),
	// not just the principal: a transfer whose fee alone would overdraw a
	// protected payer is rejected. The check runs against the payer's
	// balance in the transfer's currency — a EUR balance cannot cover a
	// USD outflow. The comparison never subtracts, so it cannot overflow
	// (see overdraft.go); totalOutflow is known to fit.
	if l.noOverdraft[t.From] &&
		l.balances[accountCurrency{account: t.From, currency: t.Currency}] < totalOutflow {
		return TransferReceipt{}, ErrAccountOverdraft
	}

	// Daily outflow limits guard the same total outflow (amount + fee):
	// the payer's cumulative outflow for the UTC calendar day may not
	// exceed the configured limit. Runs last among the risk controls, so
	// it only evaluates transfers that book something new.
	if l.dailyLimitRejectedLocked(t.From, t.Currency, totalOutflow, t.CreatedAt) {
		return TransferReceipt{}, ErrDailyLimitExceeded
	}

	now := time.Now()
	principal := JournalEntry{
		ID:             t.ID,
		DebitAccount:   t.To,
		CreditAccount:  t.From,
		AmountCents:    t.AmountCents,
		Currency:       t.Currency,
		IdempotencyKey: t.IdempotencyKey,
		CreatedAt:      t.CreatedAt,
	}
	if principal.CreatedAt.IsZero() {
		principal.CreatedAt = now
	}

	entries := []JournalEntry{principal}
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
			CreatedAt:      principal.CreatedAt,
		})
	}

	// No-FX invariant: every leg of a transfer is booked in the
	// transfer's single currency. Both legs above derive from t.Currency
	// so this holds by construction; the check is the explicit seam that
	// keeps cross-currency (FX) transfers rejected — this ledger performs
	// no currency conversion — if per-leg currencies are ever introduced.
	for _, e := range entries {
		if e.Currency != t.Currency {
			return TransferReceipt{}, ErrCrossCurrencyTransfer
		}
	}

	// Atomic commit: every leg's journal row, idempotency index entries,
	// balances, totals, chain links, and version bumps land together, under
	// the one write lock, after all checks passed. Any failure above
	// returned before the first mutation, so there is nothing to roll back.
	l.maybePruneIdempotencyKeys(now)
	l.maybePruneDailyOutflowLocked(now)
	versionBefore := l.version
	for _, e := range entries {
		l.commitEntryLocked(e)
	}
	l.addDailyOutflowLocked(t.From, t.Currency, totalOutflow, principal.CreatedAt)
	if t.IdempotencyKey != "" {
		ids := make([]string, 0, len(entries))
		for _, e := range entries {
			ids = append(ids, e.ID)
		}
		l.transferKeys[t.IdempotencyKey] = ids
	}
	entryIDs := make([]string, 0, len(entries))
	for _, e := range entries {
		entryIDs = append(entryIDs, e.ID)
	}
	l.emitAudit(AuditEvent{
		Op:            "transfer",
		Actor:         "PostTransfer",
		TraceID:       t.ID,
		VersionBefore: versionBefore,
		VersionAfter:  l.version,
		EntryIDs:      entryIDs,
		Accounts:      []AccountID{t.From, t.To},
		Details: map[string]any{
			"amount_cents":   t.AmountCents,
			"currency":       t.Currency,
			"fee_cents":      feeCents,
			"fee_tier_index": tierIndex,
			"fee_rate_bps":   rateBps,
		},
	})

	return TransferReceipt{
		TransferID:   t.ID,
		Entries:      entries,
		FeeCents:     feeCents,
		FeeTierIndex: tierIndex,
		FeeRateBps:   rateBps,
		Duplicate:    false,
	}, nil
}

// replayTransferLocked rebuilds the receipt of a previously posted
// transfer from its idempotency key, without booking anything new.
// Callers must hold the write lock.
//
// A transfer that posted a fee or FX leg replays its full receipt via the
// transfer key index; otherwise it falls back to the shared entry-level
// namespace (covers raw-posted keys and pre-fee-leg transfers).
//
// The fee leg is found by its "<transfer ID>/fee" entry ID, not by
// position: FX transfers carry an additional "<transfer ID>/fx" leg, so
// positional lookup would mistake the FX leg for the fee leg. The tier
// re-derives from the source-currency amount — the FX leg's amount for FX
// transfers, the principal's amount otherwise.
func (l *Ledger) replayTransferLocked(t Transfer) (TransferReceipt, bool) {
	if ids, ok := l.transferKeys[t.IdempotencyKey]; ok {
		entries := make([]JournalEntry, 0, len(ids))
		for _, id := range ids {
			entries = append(entries, l.entries[id])
		}
		feeCents := feeCentsOf(entries)
		// Re-disclose the tier the original transfer was charged under:
		// the fee policy is construction-time immutable, so the tier
		// re-derives exactly from the source-currency amount when the
		// fee leg went to the policy's revenue account. (An explicit fee
		// routed to the same account for an identical amount is
		// indistinguishable — and the disclosed rate still matches the
		// fee math.)
		tierIndex, rateBps := -1, int64(0)
		feeBase := entries[0].AmountCents
		for _, e := range entries {
			if strings.HasSuffix(e.ID, "/fx") {
				feeBase = e.AmountCents // FX transfers: fee computed on the source amount
				break
			}
		}
		if len(l.feeTiers) > 0 && l.feeRevenueAccount != "" {
			for _, e := range entries {
				if strings.HasSuffix(e.ID, "/fee") && e.DebitAccount == l.feeRevenueAccount {
					if idx, rate := l.feeTierFor(feeBase); policyFeeCents(feeBase, rate) == feeCents {
						tierIndex, rateBps = idx, rate
					}
					break
				}
			}
		}
		// Replays deliberately omit the FX disclosure (see
		// TransferReceipt.FX): the rate table may have changed since the
		// original posting.
		return TransferReceipt{TransferID: t.ID, Entries: entries, FeeCents: feeCents, FeeTierIndex: tierIndex, FeeRateBps: rateBps, Duplicate: true}, true
	}
	if orig, ok := l.byKey[t.IdempotencyKey]; ok {
		return TransferReceipt{
			TransferID: t.ID,
			Entries:    []JournalEntry{orig},
			Duplicate:  true,
		}, true
	}
	return TransferReceipt{}, false
}

// feeCentsOf sums the fee legs of a replayed receipt: fee legs are the
// entries whose ID ends in "/fee" by construction (see
// postTransferFXLocked and PostTransfer).
func feeCentsOf(entries []JournalEntry) int64 {
	var total int64
	for _, e := range entries {
		if strings.HasSuffix(e.ID, "/fee") {
			total += e.AmountCents
		}
	}
	return total
}

// commitEntryLocked applies every effect of one validated journal entry —
// the journal row, the idempotency index, the per-account index, both net
// balances, both debit/credit totals, the audit-chain link, and the version
// bump. Callers must hold the write lock and must have run all validation
// and risk checks first; after this call returns the entry is fully
// committed — there is no partial state to roll back.
func (l *Ledger) commitEntryLocked(e JournalEntry) {
	l.entries[e.ID] = e
	if e.IdempotencyKey != "" {
		l.byKey[e.IdempotencyKey] = e
	}
	l.byAccount[e.DebitAccount] = append(l.byAccount[e.DebitAccount], e.ID)
	l.byAccount[e.CreditAccount] = append(l.byAccount[e.CreditAccount], e.ID)
	debitKey := accountCurrency{account: e.DebitAccount, currency: e.Currency}
	creditKey := accountCurrency{account: e.CreditAccount, currency: e.Currency}
	l.balances[debitKey] += e.AmountCents
	l.balances[creditKey] -= e.AmountCents
	l.debitTotals[debitKey] += e.AmountCents
	l.creditTotals[creditKey] += e.AmountCents
	l.version++
	l.appendChainLink(e)
}
