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
	// FXSide selects the dealing price of a cross-currency transfer
	// (LG-93, see fx_spread.go): "sell" (the default) converts at the
	// pair's BID price — the customer sells the source currency — and
	// "buy" converts at the ASK price — the customer buys the target
	// currency. Anything but "buy"/"sell" (case-insensitive) is
	// rejected at field validation (ErrInvalidFXSide). The side is
	// ignored for same-currency transfers.
	FXSide string `json:"fx_side,omitempty"`
	// FXIncomeAccount overrides the ledger's configured FX
	// spread-income account for this cross-currency transfer: the
	// credit counterparty of the "<transfer ID>/spread" leg that books
	// the dealer's spread (see fx_spread.go). It must differ from the
	// FX clearing account when a spread books — enforced fail-closed
	// (ErrFXIncomeAccountNotConfigured) before any mutation. It is
	// ignored for same-currency transfers and when the spread is zero.
	FXIncomeAccount AccountID `json:"fx_income_account,omitempty"`
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
	// FeeSplits divides the fee across several recipients in basis
	// points instead of booking it to one FeeAccount (see
	// ledger/fee_split.go): each entry names a recipient and its
	// basis-point share, and the shares must sum to exactly 10000. It
	// replaces FeeAccount — the two are mutually exclusive — and it
	// needs a fee to divide, so it is incompatible with SkipFee. The
	// split applies to the finally resolved fee: an explicit FeeCents
	// or the fee policy's charge. FeeSplits with a zero resolved fee
	// is rejected (see ErrFeeSplitZeroFee). Orthogonal to cost
	// allocations (see allocation.go): allocation divides the
	// principal, fee splits divide the fee.
	FeeSplits []FeeSplit `json:"fee_splits,omitempty"`
	// SkipFee suppresses the ledger's default fee policy for this
	// transfer. An explicit FeeCents still applies.
	SkipFee        bool   `json:"skip_fee,omitempty"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	// Memo is a free-form business note carried onto the principal
	// journal entry (see JournalEntry.Memo): an order ID, an invoice
	// reference, a reconciliation tag. At most 255 UTF-8 characters
	// (see ErrMemoTooLong); the fee leg carries no memo. On
	// cross-currency transfers the memo rides the source-currency leg
	// (the payer-outflow leg, ID "<transfer ID>/fx"), mirroring the
	// same-currency principal whose credit leg is the payer.
	Memo      string    `json:"memo,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	// ExternalRef is the caller's external business reference for the
	// transfer (see JournalEntry.ExternalRef): it is stamped on the
	// principal journal entry (the fee leg carries none). On
	// cross-currency transfers it rides the source-currency leg (the
	// /fx leg, ID "<transfer ID>/fx"), mirroring the memo. At most 64
	// UTF-8 characters (see ErrExternalRefTooLong); a reference that
	// is already journaled is rejected with ErrDuplicateExternalRef.
	ExternalRef string `json:"external_ref,omitempty"`
	// Attachments are caller-supplied evidence references for the
	// transfer (see JournalEntry.Attachments): they are stamped on the
	// principal journal entry (the fee leg carries none), mirroring the
	// memo. On cross-currency transfers they ride the source-currency
	// leg (the /fx leg), mirroring the memo.
	Attachments []string `json:"attachments,omitempty"`
	// Metadata is caller-supplied structured metadata for the transfer
	// (see JournalEntry.Metadata): it is stamped on the principal
	// journal entry (the fee leg, the FX conversion legs, and the
	// spread leg carry none), mirroring the memo and the attachments.
	// On cross-currency transfers it rides the source-currency leg
	// (the /fx leg), mirroring the memo.
	Metadata map[string]string `json:"metadata,omitempty"`
	// MaxSlippageBps is the opt-in LG-86 slippage cap for
	// cross-currency transfers, in basis points: the request's FX rate
	// is locked at admission (see FXRateQuote), and when the effective
	// rate has drifted more than this from the admitted quote by commit
	// time the transfer is rejected with ErrFXSlippageExceeded (HTTP
	// 422) and records nothing. Zero (the default) means no cap: the
	// transfer still settles at the admitted price, and the drift is
	// disclosed on the receipt (FXConversion.SlippageBps) and the audit
	// event. A negative cap is rejected at validation
	// (ErrInvalidMaxSlippage). The field is ignored on same-currency
	// transfers — a non-FX transfer carrying it is neither rejected nor
	// gated.
	MaxSlippageBps int64 `json:"max_slippage_bps,omitempty"`
	// forwardLocked pins this transfer's FX conversion to a
	// contractually agreed price (LG-110, see forward.go): it is set
	// only by SettleForward, never by callers — an ordinary transfer
	// cannot smuggle a locked price past the rate table. Unexported,
	// so it stays out of the JSON API and snapshots; it lives only for
	// the duration of the settlement call.
	forwardLocked *forwardLockedRate
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
	// ReviewStatus reports the dual-control review state of the
	// transfer (see review.go): "pending_review" when the transfer was
	// routed into operator review instead of settling (its legs are
	// journaled but marked pending, and the payer's funds are frozen),
	// "approved" once an operator settled it. Empty for transfers that
	// never entered review.
	ReviewStatus string `json:"review_status,omitempty"`
	// FeeSplits discloses the fee split when the transfer divided its
	// fee across recipients (see ledger/fee_split.go): one row per
	// recipient with the promised basis points and the cents actually
	// booked. Nil when the fee went to a single fee account (or there
	// was no fee). Idempotent replays rebuild the same breakdown from
	// the journaled legs' memos.
	FeeSplits []FeeSplitShare `json:"fee_splits,omitempty"`
	// RuleLegs discloses the automatic posting-rule legs the transfer
	// booked (see ledger/posting_rules.go): one row per derived leg
	// with the rule name, the journal entry ID, the accounts, and the
	// cents booked. Nil when no rule fired. Idempotent replays rebuild
	// the same disclosure from the journaled legs' "/rule:" IDs.
	RuleLegs []PostingRuleLegInfo `json:"rule_legs,omitempty"`
	// SettlementStatus discloses the transfer's settlement-window
	// outcome (see settlement_cutoff.go): "settled" when the transfer
	// booked in its currency's current settlement window, "scheduled"
	// when it posted after the cutoff and waits for the next window
	// (see SettlementWindow for the window). Empty when the transfer's
	// currency has no cutoff configured — zero behavior change — or
	// when the transfer was routed into dual-control review (its
	// settlement is the operator's concern at approval).
	SettlementStatus string `json:"settlement_status,omitempty"`
	// SettlementWindow is the settlement window the transfer settles
	// in: today's still-open cutoff when SettlementStatus is
	// "settled", the next window when it is "scheduled". Zero when the
	// currency has no cutoff configured.
	SettlementWindow time.Time `json:"settlement_window,omitempty"`
	// QuorumTally discloses the live N-of-M vote count when the review
	// decision went through a quorum policy (see review_quorum.go):
	// the named votes, the threshold, and what this vote did. It is set
	// on approve-vote receipts while the review is pending and on the
	// settlement receipt when a vote reached the threshold. Nil for
	// direct single-operator approvals and for idempotent replays of an
	// already-settled review (the tally described the settling call,
	// not the replay).
	QuorumTally *ReviewQuorumTally `json:"quorum_tally,omitempty"`
	// ChainTally discloses the live per-stage vote count when the
	// review decision went through an approval chain (see
	// review_chain.go): the current stage, the stage's named votes,
	// and what this vote did. It is set on approve-vote receipts while
	// the review is pending (including stage advances). Nil for
	// single-operator and flat-quorum decisions, and for idempotent
	// replays of an already-settled review.
	ChainTally *ReviewChainTally `json:"chain_tally,omitempty"`
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
// leg), the per-recipient split shares (nil unless FeeSplits is set —
// the split replaces the single fee account, so exactly one of
// feeAccount and splits is set when the fee is positive), and the
// schedule tier that applied (index -1 and rate 0 when the fee came
// from an explicit FeeCents, SkipFee suppressed the policy, or the
// policy is disabled). Explicit fees win over the policy; the policy
// applies only when no explicit fee is given and SkipFee is false.
// Callers must hold l.mu.
func (l *Ledger) resolveFeeLocked(t Transfer) (feeCents int64, feeAccount AccountID, splits []FeeSplitShare, tierIndex int, rateBps int64, err error) {
	if t.FeeCents < 0 {
		return 0, "", nil, -1, 0, ErrInvalidFee
	}
	// Fee splits (see fee_split.go) replace the single fee account: the
	// finally resolved fee — explicit or policy-computed — is divided
	// across the recipients. Mutual exclusivity with FeeAccount and
	// SkipFee is enforced by validateTransferFields; the zero-fee
	// rejection below is the misconfiguration guard: a split of
	// nothing would silently drop the intended revenue shares, so the
	// transfer is rejected instead (see ErrFeeSplitZeroFee).
	if len(t.FeeSplits) > 0 {
		var fee int64
		tierIndex, rateBps = -1, 0
		switch {
		case t.FeeCents > 0:
			fee = t.FeeCents
		case t.SkipFee || len(l.feeTiers) == 0 || l.feeRevenueAccount == "":
			fee = 0
		default:
			fee, tierIndex, rateBps = l.policyFeeForLocked(t.AmountCents)
		}
		if fee <= 0 {
			return 0, "", nil, -1, 0, ErrFeeSplitZeroFee
		}
		return fee, "", computeFeeSplitShares(fee, t.FeeSplits), tierIndex, rateBps, nil
	}
	if t.FeeCents > 0 {
		if t.FeeAccount == "" {
			return 0, "", nil, -1, 0, ErrInvalidFee
		}
		return t.FeeCents, t.FeeAccount, nil, -1, 0, nil
	}
	if t.FeeAccount != "" {
		// An explicit fee account without a positive fee is a caller bug:
		// silently ignoring it would misroute policy-computed fees.
		return 0, "", nil, -1, 0, ErrInvalidFee
	}
	if t.SkipFee || len(l.feeTiers) == 0 || l.feeRevenueAccount == "" {
		return 0, "", nil, -1, 0, nil
	}
	fee, tierIndex, rateBps := l.policyFeeForLocked(t.AmountCents)
	if fee > 0 {
		return fee, l.feeRevenueAccount, nil, tierIndex, rateBps, nil
	}
	// A computed fee of zero (a free dust tier, or a tiny amount under a
	// low rate) posts no leg, but the tier that produced it is still
	// disclosed on the receipt.
	return 0, "", nil, tierIndex, rateBps, nil
}

// policyFeeForLocked computes the ledger's default fee-policy charge for
// one amount: floor(amount * tierRate / 10000) to feeRevenueAccount, where
// the tier is the last one whose minimum does not exceed the amount. It
// returns the fee (0 when the policy is disabled or the tier is free),
// the tier index and rate (-1 and 0 when the policy is disabled), so
// callers can disclose the tier even when no leg is booked. Callers must
// hold l.mu.
func (l *Ledger) policyFeeForLocked(amount int64) (feeCents int64, tierIndex int, rateBps int64) {
	if len(l.feeTiers) == 0 || l.feeRevenueAccount == "" {
		return 0, -1, 0
	}
	tierIndex, rateBps = l.feeTierFor(amount)
	return policyFeeCents(amount, rateBps), tierIndex, rateBps
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

// validateTransferFields checks a transfer's fields and normalizes the
// currency codes, returning the normalized copy and the settlement
// currency. It runs no risk checks and records nothing; PostTransfer and
// PostTransferSplit share it so both reject the same malformed transfers
// the same way, in the same order.
func validateTransferFields(t Transfer) (Transfer, string, error) {
	if t.ID == "" {
		return t, "", ErrEmptyTransferID
	}
	if t.From == "" {
		return t, "", ErrEmptyFromAccount
	}
	if t.To == "" {
		return t, "", ErrEmptyToAccount
	}
	if t.From == t.To {
		return t, "", ErrTransferSameAccount
	}
	if t.AmountCents <= 0 {
		return t, "", ErrTransferNonPositiveAmount
	}
	// Currency is field validation, like the accounts and the amount: an
	// empty code normalizes to the default currency, anything else must
	// be a 3-letter uppercase ISO 4217 code. Normalization happens
	// before the idempotency replay check, mirroring Post, so replays
	// compare canonical codes.
	currency, err := normalizeCurrency(t.Currency)
	if err != nil {
		return t, "", err
	}
	t.Currency = currency

	// ToCurrency empty means "settle in the transfer's own currency";
	// anything else must be a valid code naming a different currency,
	// taking the cross-currency path (see fx.go).
	toCurrency := currency
	if t.ToCurrency != "" {
		toCurrency, err = normalizeCurrency(t.ToCurrency)
		if err != nil {
			return t, "", err
		}
		t.ToCurrency = toCurrency
	}
	// The memo is field validation, like the legs and the amount: a note
	// longer than maxMemoRunes UTF-8 characters is rejected before
	// anything is recorded. Validated once here, before the same/FX
	// currency branch, so both paths enforce it.
	if err := checkMemoLength(t.Memo); err != nil {
		return t, "", err
	}
	// The external reference is field validation, like the memo:
	// longer than maxExternalRefRunes UTF-8 characters is rejected
	// before anything is recorded, on both paths.
	if err := checkExternalRefLength(t.ExternalRef); err != nil {
		return t, "", err
	}
	// Attachments are field validation, like the memo: more than
	// maxAttachments entries, a non-https URI, or a URI longer than
	// maxAttachmentRunes characters is rejected before anything is
	// recorded, on both paths.
	if err := checkAttachments(t.Attachments); err != nil {
		return t, "", err
	}
	// Metadata is field validation, like the attachments: more than
	// maxMetadataPairs pairs, a key outside the tag-segment charset,
	// or a value longer than maxMetadataValueRunes UTF-8 characters is
	// rejected before anything is recorded, on both paths.
	if err := checkMetadata(t.Metadata); err != nil {
		return t, "", err
	}
	// The slippage cap (see Transfer.MaxSlippageBps) is field
	// validation, like the fee config: a negative cap is a caller bug,
	// rejected before any risk check. A positive cap on a
	// same-currency transfer is ignored, not rejected.
	if t.MaxSlippageBps < 0 {
		return t, "", ErrInvalidMaxSlippage
	}
	// The FX dealing side (see Transfer.FXSide) is field validation,
	// like the slippage cap: anything but "buy"/"sell"
	// (case-insensitive) is a caller bug, rejected before any risk
	// check. The value is normalized to lowercase; it is ignored on
	// same-currency transfers — the side only prices cross-currency
	// conversions.
	switch strings.ToLower(t.FXSide) {
	case "", FXSideBuy, FXSideSell:
		t.FXSide = strings.ToLower(t.FXSide)
	default:
		return t, "", ErrInvalidFXSide
	}
	// Fee splits (see fee_split.go) are field validation, like the legs
	// and the amount: FeeSplits replaces the single FeeAccount, so the
	// two are mutually exclusive, and a split needs a fee to divide,
	// so it is incompatible with SkipFee. The zero-fee rejection
	// (ErrFeeSplitZeroFee) lives in resolveFeeLocked, the first place
	// the resolved fee is known.
	if len(t.FeeSplits) > 0 {
		if t.FeeAccount != "" {
			return t, "", ErrFeeSplitFeeAccountConflict
		}
		if t.SkipFee {
			return t, "", ErrFeeSplitSkipFeeConflict
		}
		if err := validateFeeSplits(t.FeeSplits); err != nil {
			return t, "", err
		}
	}
	return t, toCurrency, nil
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
// then the daily outflow limit check on the same total outflow, then the
// period gate (see period.go): a backdated transfer landing in a closed
// accounting period is rejected with ErrPeriodClosed. Rejected
// transfers — validation failures, ID conflicts, frozen rejections,
// overdraft rejections, daily-limit rejections, and closed-period
// rejections alike — record
// nothing: no journal rows, no chain links, no version bump.
//
// After every risk check passes, the dual-control review gate (see
// review.go) runs last: a transfer at or above the payer's review
// threshold freezes instead of settling — its legs are journaled with
// the pending-review marker (visible in journal exports, excluded from
// balances), the payer's total outflow is deducted from available and
// reserved against the daily limit, and the receipt carries review_status
// "pending_review". An operator settles or releases it with
// ApproveTransferReview / RejectTransferReview.
//
// Idempotency shares the ledger-wide key namespace with Post: a key already
// used by either API replays the original entry instead of booking again.
// A transfer that posted a fee leg replays the full receipt (principal +
// fee entries). A zero CreatedAt is filled with the current time.

func (l *Ledger) PostTransfer(t Transfer) (TransferReceipt, error) {
	// LG-86: the FX rate quote is admitted before the write lock is
	// taken (see admitFXRateQuote): the price a cross-currency transfer
	// settles at is locked at admission, and any drift of the live
	// rate by commit time is measured against it. The zero quote means
	// "no admission" — the transfer converts at the commit-time rate,
	// as before.
	quote := l.admitFXRateQuote(t)
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.postTransferLocked(t, quote)
}

// postTransferLocked is the lock-held core of PostTransfer; see its doc
// comment for the full contract. quote is the LG-86 admission quote
// (see FXRateQuote); internal callers with no admission context pass
// the zero quote, which keeps commit-time rate semantics and carries
// no slippage gate. PostTransferSplit delegates to it when a transfer
// needs no splitting, so a non-split transfer posts exactly as
// PostTransfer would (at the commit-time rate — the split path holds
// the write lock from entry and admits no quote).
func (l *Ledger) postTransferLocked(t Transfer, quote FXRateQuote) (TransferReceipt, error) {
	var err error
	var toCurrency string
	if t, toCurrency, err = validateTransferFields(t); err != nil {
		return TransferReceipt{}, err
	}

	// Sanctions/denylist screening (see denylist.go): the payer and the
	// payee are screened before the FX branch and the idempotency replay
	// check — a hit rejects the transfer and leaves no idempotency
	// trace, and the attempt is audited as "denylist_rejected". The fee
	// leg's revenue account is deliberately not screened: it is the
	// operator's own account, and screening it would let the operator
	// deny itself into a self-inflicted outage.
	if acct, entry, denied := l.denylistRejectedLocked(t.From, t.To); denied {
		return TransferReceipt{}, l.deniedRejectionLocked("PostTransfer", t.ID, acct, entry, []AccountID{t.From, t.To})
	}

	if toCurrency != t.Currency {
		return l.postTransferFXLocked(t, toCurrency, time.Now(), quote)
	}

	feeCents, feeAccount, feeSplits, tierIndex, rateBps, err := l.resolveFeeLocked(t)
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

	// Virtual-account attribution (see virtual_account.go): a VAN payer
	// is rejected (ErrVANNoDebit); a VAN payee, fee account, or
	// fee-split recipient attributes to its parent. Runs after the
	// idempotency replay check — a replay returns the original receipt
	// instead of failing on a VAN registered after the original posting
	// — and before every account risk gate below, so frozen/overdraft/
	// daily-limit checks see the real (parent) accounts.
	var vanAttrs []*vanAttribution
	if t, feeAccount, feeSplits, vanAttrs, err = l.resolveTransferVANsLocked(t, feeCents, feeAccount, feeSplits); err != nil {
		return TransferReceipt{}, err
	}

	feeEntryIDs := feeLegIDs(t.ID, feeCents, feeSplits)
	if _, exists := l.entries[t.ID]; exists {
		return TransferReceipt{}, ErrTransferIDConflict
	}
	// A split transfer reserves the "<id>/chunk-0" namespace for its
	// chunk entries (see PostTransferSplit): a plain transfer reusing a
	// split transfer's ID would create two transfers sharing one
	// identity.
	if _, exists := l.entries[t.ID+"/chunk-0"]; exists {
		return TransferReceipt{}, ErrTransferIDConflict
	}
	// Every fee leg ID — the single "<id>/fee" or each split leg
	// "<id>/fee/<i>" — must be free: silently overwriting a journaled
	// row is never an option.
	for _, id := range feeEntryIDs {
		if _, exists := l.entries[id]; exists {
			return TransferReceipt{}, ErrTransferIDConflict
		}
	}
	// External-reference uniqueness, like in Post: after the
	// idempotency replay check (a replay returns the original receipt
	// without consulting the reference index) and the ID-conflict
	// checks, before anything books. The reference rides the
	// principal entry on this path.
	if t.ExternalRef != "" {
		if owner, taken := l.byRef[t.ExternalRef]; taken {
			return TransferReceipt{}, fmt.Errorf("%w: reference %q already journaled on entry %q", ErrDuplicateExternalRef, t.ExternalRef, owner)
		}
	}

	// Expired timed freezes release lazily before the frozen gate, like in
	// Post (see FreezeUntil): a deadline that passed must not keep
	// rejecting transfers.
	l.releaseExpiredFreezesLocked(time.Now(), "write_path")

	// Expired KYC profiles sweep lazily before the KYC gate below, like
	// in Post (see expireKycLocked in kyc.go): a lapsed verification
	// stops new transfers from this transfer on.
	l.expireKycLocked(time.Now())

	if l.closedLocked(t.From) || l.closedLocked(t.To) {
		return TransferReceipt{}, ErrAccountClosed
	}
	for _, fa := range feeLegAccounts(feeAccount, feeSplits) {
		if l.closedLocked(fa) {
			return TransferReceipt{}, ErrAccountClosed
		}
	}
	// Dormant accounts (see dormancy.go) reject operator transfers with
	// ErrAccountDormant, after the idempotency replay and closed checks
	// and before the frozen check — the same position as in Post's gate
	// order. Only the transfer's own legs (From/To) are gated: fee legs
	// are system-routed to operator-configured revenue accounts, not
	// operator activity through those accounts.
	if l.dormantLocked(t.From) || l.dormantLocked(t.To) {
		return TransferReceipt{}, ErrAccountDormant
	}
	if l.frozenLocked(t.From) || l.frozenLocked(t.To) {
		return TransferReceipt{}, ErrAccountFrozen
	}
	if l.suspendedLocked(t.From) || l.suspendedLocked(t.To) {
		return TransferReceipt{}, ErrAccountSuspended
	}
	// Every fee recipient walks the same frozen check the single fee
	// account always walked: a frozen recipient rejects the transfer
	// before anything books.
	for _, fa := range feeLegAccounts(feeAccount, feeSplits) {
		if l.frozenLocked(fa) {
			return TransferReceipt{}, ErrAccountFrozen
		}
		if l.suspendedLocked(fa) {
			return TransferReceipt{}, ErrAccountSuspended
		}
	}

	// Transfer beneficiary allowlist (see allowlist.go): after the
	// frozen gate — an allowlist never un-freezes a stopped payer —
	// and before the overdraft gate, so the check order stays
	// validation → idempotency replay → ID conflict → frozen →
	// allowlist → overdraft → commit. Only the principal leg's payee
	// is screened: the fee leg's recipient is the operator's own
	// revenue account, not a business beneficiary, so it is exempt
	// (the same carve-out the sanctions/denylist screening makes for
	// the operator's accounts). A hit records nothing and emits no
	// audit event, like the frozen and overdraft gates; an idempotent
	// replay above returns the original receipt instead of failing.
	if l.allowlistRejectedLocked(t.From, t.To) {
		return TransferReceipt{}, l.allowlistRejectionLocked(t.From, t.To)
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

	// KYC/尽调门控 (see kyc.go): after the overdraft check — both are
	// account risk controls — and after the idempotency replay check
	// above: a replay books nothing new, so it must not fail on an
	// account whose KYC lapsed after the original transfer. Only the
	// principal legs are gated: the fee legs are system-routed to the
	// operator's own revenue accounts (the same carve-out the dormant
	// gate makes). The threshold compares the payer's total outflow
	// (amount + fee), like the overdraft gate.
	if err := l.kycRejectedLocked(t.From, t.To, totalOutflow); err != nil {
		return TransferReceipt{}, err
	}

	// Daily outflow limits guard the same total outflow (amount + fee):
	// the payer's cumulative outflow for the UTC calendar day may not
	// exceed the configured limit. Runs last among the risk controls, so
	// it only evaluates transfers that book something new.
	if l.dailyLimitRejectedLocked(t.From, t.Currency, totalOutflow, t.CreatedAt) {
		return TransferReceipt{}, ErrDailyLimitExceeded
	}

	// Monthly spend budgets guard the same total outflow (amount + fee)
	// per UTC calendar month: the payer's cumulative spend for the month
	// may not exceed the configured budget. Runs right after the daily
	// outflow limit — the two controls are orthogonal (rolling outflow
	// window vs accounting-period spend) and both are checked.
	if l.budgetRejectedLocked(t.From, t.Currency, totalOutflow, t.CreatedAt) {
		return TransferReceipt{}, ErrBudgetExceeded
	}

	// The transfer's effective post time is finalized before the
	// velocity and period gates: a backdated transfer's window usage
	// counts toward its own timestamp, and an un-timestamped transfer
	// counts toward now.
	vat := t.CreatedAt
	if vat.IsZero() {
		vat = time.Now()
	}

	// Rolling velocity limits (see velocity.go) run after the monthly
	// spend budget and before the period gate: the payer's total
	// outflow (amount + fee) counts toward the payer's outflow window,
	// the principal counts toward the payee's inflow window. Only
	// transfers that book something new reach this gate — a replay
	// returned above.
	if err := l.velocityRejectedLocked(t.To, t.Currency, VelocityInflow, t.AmountCents, vat, "PostTransfer"); err != nil {
		return TransferReceipt{}, err
	}
	if err := l.velocityRejectedLocked(t.From, t.Currency, VelocityOutflow, totalOutflow, vat, "PostTransfer"); err != nil {
		return TransferReceipt{}, err
	}

	// The period gate is keyed on the transfer's effective post time: a
	// backdated transfer landing in a closed accounting period is rejected
	// with ErrPeriodClosed. It runs after the idempotency replay check
	// above, so replaying a key posted before the period closed returns
	// the original receipt.
	if err := l.periodRejectedLocked(vat); err != nil {
		return TransferReceipt{}, err
	}

	now := time.Now()
	principal := JournalEntry{
		ID:             t.ID,
		DebitAccount:   t.To,
		CreditAccount:  t.From,
		AmountCents:    t.AmountCents,
		Currency:       t.Currency,
		IdempotencyKey: t.IdempotencyKey,
		Memo:           t.Memo,
		ExternalRef:    t.ExternalRef,
		Attachments:    t.Attachments,
		Metadata:       t.Metadata,
		CreatedAt:      t.CreatedAt,
	}
	if principal.CreatedAt.IsZero() {
		principal.CreatedAt = now
	}

	entries := []JournalEntry{principal}
	// The fee legs — one "<transfer ID>/fee" leg for a single fee
	// account, or one "<transfer ID>/fee/<i>" leg per split recipient
	// (see buildFeeLegEntries): every leg debits its recipient and
	// credits the payer, on top of the principal.
	entries = append(entries, buildFeeLegEntries(t, feeCents, feeAccount, feeSplits, principal.CreatedAt)...)

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

	// The dual-control review gate (see review.go) runs last, after the
	// period gate: a transfer at or above the payer's review threshold
	// freezes instead of settling — journal rows marked pending review,
	// funds deducted from available, nothing touching balances. The
	// returned receipt carries review_status "pending_review".
	//
	// The pain.001 registry records the principal first, so
	// review-routed transfers are registered too: ExportPain001
	// excludes pending-review rows live (see pain001.go), and approval
	// flips them exportable without any registry change.
	l.registerPainTransferLocked(PainTransfer{
		TransferID:  t.ID,
		From:        t.From,
		To:          t.To,
		AmountCents: t.AmountCents,
		Currency:    t.Currency,
		CreatedAt:   principal.CreatedAt,
	})
	// The journal memo marker for attributed legs (see
	// virtual_account.go): applied before the review gate so
	// review-frozen rows carry it too.
	if err := annotateVANMemos(entries, vanAttrs); err != nil {
		return TransferReceipt{}, err
	}
	if receipt, reviewed, err := l.maybeReviewTransferLocked(t, entries, feeCents, tierIndex, rateBps, nil, totalOutflow, now); reviewed || err != nil {
		return receipt, err
	}

	// Automatic posting rules (see posting_rules.go): transfers that
	// match an installed rule append their derived legs to the commit.
	// Evaluated after every gate — including the review gate, so
	// review-frozen transfers book no rule legs — and before the first
	// mutation, so a failing rule leg rejects the whole transfer with
	// nothing recorded.
	ruleLegs, ruleLegInfos, err := l.resolvePostingRuleLegsLocked(postingRuleContext{
		kind:        PostingEventTransfer,
		payer:       t.From,
		payee:       t.To,
		amountCents: t.AmountCents,
		currency:    t.Currency,
		triggerID:   t.ID,
	}, principal.CreatedAt)
	if err != nil {
		return TransferReceipt{}, err
	}
	entries = append(entries, ruleLegs...)

	// Atomic commit: every leg's journal row, idempotency index entries,
	// balances, totals, chain links, and version bumps land together, under
	// the one write lock, after all checks passed. Any failure above
	// returned before the first mutation, so there is nothing to roll back.
	l.maybePruneIdempotencyKeys(now)
	l.maybePruneDailyOutflowLocked(now)
	l.maybePruneMonthlySpendLocked(now)
	versionBefore := l.version
	// Pre-commit balance snapshot for the opt-in balance-change hook
	// (see balance_hook.go): nil when no hook is registered.
	hookTouched := touchedAccounts(entries)
	hookOld := l.balanceSnapshotLocked(hookTouched)
	for _, e := range entries {
		l.commitEntryLocked(e)
	}
	l.addDailyOutflowLocked(t.From, t.Currency, totalOutflow, principal.CreatedAt)
	l.addMonthlySpendLocked(t.From, t.Currency, totalOutflow, principal.CreatedAt)
	l.countPostingRuleLegsLocked(ruleLegInfos)
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
	// Low-balance alert evaluation: strictly after the atomic commit
	// zone, read-only (see low_balance.go). Advisory only.
	l.evaluateBalanceWatchesLocked(touchedAccounts(entries), t.ID, "PostTransfer")
	transferDetails := map[string]any{
		"amount_cents":   t.AmountCents,
		"currency":       t.Currency,
		"fee_cents":      feeCents,
		"fee_tier_index": tierIndex,
		"fee_rate_bps":   rateBps,
	}
	if len(feeSplits) > 0 {
		// The per-recipient split breakdown enters the structured
		// audit: account, promised basis points, and cents booked.
		transferDetails["fee_splits"] = feeSplitAuditDetails(feeSplits)
	}
	if len(ruleLegInfos) > 0 {
		// The automatic posting-rule legs (see posting_rules.go)
		// enter the structured audit like the fee splits: rule,
		// entry, accounts, and cents booked.
		transferDetails["posting_rule_legs"] = postingRuleDetails(ruleLegInfos)
	}
	if t.Memo != "" {
		transferDetails["memo"] = t.Memo
	}
	if t.ExternalRef != "" {
		transferDetails["external_ref"] = t.ExternalRef
	}
	l.emitAudit(AuditEvent{
		Op:            "transfer",
		Actor:         "PostTransfer",
		TraceID:       t.ID,
		VersionBefore: versionBefore,
		VersionAfter:  l.version,
		EntryIDs:      entryIDs,
		Accounts:      []AccountID{t.From, t.To},
		Details:       transferDetails,
	})
	// Settlement cutoff evaluation (see settlement_cutoff.go): runs
	// after the atomic commit, so the booking is never blocked by the
	// cutoff — the money is journaled first, the settlement flag second.
	settlementStatus, settlementWindow := l.deferSettlementIfPastCutoffLocked(
		t.Currency, principal.CreatedAt, t.ID, entryIDs, []AccountID{t.From, t.To})
	// Virtual-account attributions ride their own audit events (see
	// virtual_account.go), after the transfer's own event. Review-routed
	// transfers return above, so only settled transfers attribute here.
	l.emitVANAttributionsLocked(vanAttrs, t.ID, "PostTransfer")
	// Domain outbox: entry_posted per leg, then the transfer_posted
	// envelope. Pending-review transfers never reach here (they return
	// from maybeReviewTransferLocked above); their legs emit at
	// ApproveTransferReview instead.
	l.emitOutboxTransferLocked(t, feeCents, entries, nil)
	// Balance-change notification: strictly after the atomic commit
	// zone, advisory only — async dispatch, hook failures isolated.
	l.fireBalanceHooksLocked(hookTouched, hookOld, t.ID)

	return TransferReceipt{
		TransferID:       t.ID,
		Entries:          entries,
		FeeCents:         feeCents,
		FeeTierIndex:     tierIndex,
		FeeRateBps:       rateBps,
		Duplicate:        false,
		FeeSplits:        feeSplits,
		RuleLegs:         ruleLegInfos,
		SettlementStatus: settlementStatus,
		SettlementWindow: settlementWindow,
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
		// A replay of a transfer that is still (or was) in dual-control
		// review returns the review receipt: the review status is looked
		// up by the original transfer ID (the principal entry's ID),
		// not the replay request's ID, which may differ.
		reviewStatus := ""
		if len(entries) > 0 {
			reviewStatus = l.reviewStatusLocked(entries[0].ID)
		}
		feeCents := feeCentsOf(entries[0].ID, entries)
		// Re-disclose the tier the original transfer was charged under
		// (see rediscloseFeeTierLocked): the fee base is the
		// source-currency amount — the FX leg's amount for FX transfers,
		// the principal's amount otherwise. (An explicit fee routed to
		// the same account for an identical amount is indistinguishable
		// — and the disclosed rate still matches the fee math.)
		feeBase := entries[0].AmountCents
		for _, e := range entries {
			if strings.HasSuffix(e.ID, "/fx") {
				feeBase = e.AmountCents
				break
			}
		}
		tierIndex, rateBps := l.rediscloseFeeTierLocked(entries[0].ID, entries, feeBase)
		// The fee-split breakdown rebuilds from the journaled legs'
		// memos (see feeSplitSharesFromLegs): the replay discloses
		// exactly what the original posted, and books nothing new.
		// entries[0] is the principal entry, whose ID is the original
		// transfer ID — the replay request's ID may differ.
		feeSplits := feeSplitSharesFromLegs(entries[0].ID, entries)
		// The posting-rule legs rebuild from the journaled legs'
		// "/rule:" IDs (see postingRuleLegInfosFromEntries): the
		// replay discloses exactly what the original posted, and
		// books nothing new.
		ruleLegs := postingRuleLegInfosFromEntries(entries)
		// Replays deliberately omit the FX disclosure (see
		// TransferReceipt.FX): the rate table may have changed since the
		// original posting.
		return TransferReceipt{TransferID: t.ID, Entries: entries, FeeCents: feeCents, FeeTierIndex: tierIndex, FeeRateBps: rateBps, FeeSplits: feeSplits, RuleLegs: ruleLegs, Duplicate: true, ReviewStatus: reviewStatus}, true
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
// single "<transfer ID>/fee" entries and the fee-split
// "<transfer ID>/fee/<i>" legs, by construction (see isFeeLegID and
// postTransferFXLocked). Anchoring on the transfer ID keeps a
// user-chosen transfer ID that merely ends in "/fee/<digits>" from
// being misread as a fee leg.
func feeCentsOf(transferID string, entries []JournalEntry) int64 {
	var total int64
	for _, e := range entries {
		if isFeeLegID(transferID, e.ID) {
			total += e.AmountCents
		}
	}
	return total
}

// isolateAttachments returns a copy of e whose attachments slice has a
// private backing array. Journal rows stored in the ledger must never
// alias caller-owned slices: see commitEntryLocked and
// commitReviewEntryLocked.
func isolateAttachments(e JournalEntry) JournalEntry {
	if e.Attachments != nil {
		atts := make([]string, len(e.Attachments))
		copy(atts, e.Attachments)
		e.Attachments = atts
	}
	return e
}

// commitEntryLocked applies every effect of one validated journal entry —
// the journal row, the idempotency index, both net balances, both
// debit/credit totals, the audit-chain link, and the version bump.
// Callers must hold the write lock and must have run all validation
// and risk checks first; after this call returns the entry is fully
// committed — there is no partial state to roll back.
//
// The journaled row carries a private copy of the attachments slice
// (see isolateAttachments): the stored row must never alias a
// caller-owned backing array, or a caller mutating the entry it got back
// could rewrite journaled evidence references behind the audit chain's
// back.
func (l *Ledger) commitEntryLocked(e JournalEntry) {
	e = isolateAttachments(e)
	l.entries[e.ID] = e
	if e.IdempotencyKey != "" {
		l.byKey[e.IdempotencyKey] = e
	}
	// The external-reference index is maintained on the same commit
	// path as the idempotency index: every journal row that carries a
	// non-empty reference registers it here. Callers run the duplicate
	// check before committing, so a row here never collides — except in
	// restore folds (see snapshot.go and incremental.go), which rebuild
	// the index from the journaled rows instead.
	if e.ExternalRef != "" {
		l.byRef[e.ExternalRef] = e.ID
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
	// Rolling velocity windows (see velocity.go) feed from this single
	// commit funnel: every journal write — Post, transfer legs, batch,
	// sweep, split, merge, reversal, hold capture, FX, closing, and
	// system postings — records its two directional events, so the
	// windows reflect real booked movement. Idempotent replays skip
	// the commit and never record; accounts without a configured
	// limit record nothing, keeping memory bounded.
	l.recordVelocityLocked(e.DebitAccount, e.CreditAccount, e.Currency, e.AmountCents, e.CreatedAt)
}
