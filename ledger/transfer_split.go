package ledger

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Large-transfer auto-split (LG-75): PostTransferSplit.
//
// A transfer whose amount exceeds the per-transfer chunk cap is split
// into multiple chunk postings that commit atomically — every leg's
// journal row, idempotency index entry, balance, total, chain link, and
// version bump land together under the one write lock, after all checks
// passed. Any failure before the commit — a validation failure, an ID
// conflict, a frozen account, an overdraft rejection, a daily-limit
// rejection, or a closed-period rejection — records nothing: no journal
// rows, no chain links, no version bump. Readers never observe a
// half-posted split.
//
// Chunk sizing: the chunk size is the tighter of the opt-in
// single-transfer cap (WithMaxTransferChunkCents) and the payer's
// per-account daily outflow limit (LG-26) in the transfer's currency. A
// chunk sized above the daily limit would trip the limit's per-posting
// fast path even with budget left, so sizing by the minimum keeps every
// chunk individually admissible — while the cumulative daily check stays
// the hard control: chunks are checked against the day's running total
// in commit order, and any chunk that would breach the limit fails the
// whole split atomically. Chunking can never circumvent the daily limit.
//
// Fees follow the LG-17 convention by default: the fee is computed once
// on the whole transfer amount and booked as a single "<id>/fee" leg.
// With WithSplitFeePerChunk the fee policy is instead resolved per chunk
// (each chunk's fee leg is "<id>/chunk-<i>/fee"); an explicit FeeCents is
// always charged once per transfer, since it is a fixed per-transfer
// amount, not a rate.
//
// Idempotency: each chunk's journal entry carries a key derived per
// (transfer, chunk) — "<key>/chunk-<i>" under the caller's idempotency
// key, mirroring the "<key>/fee" convention — and the whole split
// registers under the caller's key, so replaying it returns the whole
// receipt with Duplicate=true and books nothing new.
//
// Dual-control review is evaluated on the WHOLE transfer amount before
// anything else: splitting a transfer to slip under the review threshold
// chunk by chunk would defeat the control, so such transfers are
// rejected with ErrSplitTransferNeedsReview and belong in PostTransfer's
// review flow instead.
//
// Cross-currency splits keep the FX path's per-leg single-currency
// requirement: every chunk converts at the one rate looked up for the
// transfer (floored per chunk — the receipt discloses the summed
// conversion, which may differ by a cent from converting the whole
// amount at once), the fee leg stays in the source currency, and the
// payer-side risk checks (overdraft, daily limit) run against the source
// currency.

var (
	// ErrSplitTransferNeedsReview is returned by PostTransferSplit when
	// the whole transfer meets the payer's dual-control review
	// threshold: splitting must not bypass review, so such transfers go
	// through PostTransfer's review flow instead.
	ErrSplitTransferNeedsReview = errors.New("ledger: split transfer meets the payer's review threshold: submit via PostTransfer for dual-control review")
)

// TransferChunk describes one chunk of a split transfer: its position
// in the split, its amount, the journal entries it booked (the chunk
// principal, then its fee leg when one was booked), and the fee charged
// on it with the tier that produced it.
type TransferChunk struct {
	ChunkIndex   int      `json:"chunk_index"`
	ChunkCount   int      `json:"chunk_count"`
	AmountCents  int64    `json:"amount_cents"`
	EntryIDs     []string `json:"entry_ids"`
	FeeCents     int64    `json:"fee_cents"`
	FeeTierIndex int      `json:"fee_tier_index"`
	FeeRateBps   int64    `json:"fee_rate_bps"`
}

// SplitTransferReceipt reports what PostTransferSplit committed.
//
// Chunks carries the per-chunk disclosure in commit order; Entries holds
// every journal entry in commit order (chunk principals, per-chunk fee
// legs, then the once-per-transfer fee leg when one was booked).
// FeeCents is the total fee booked; FeeTierIndex/FeeRateBps disclose the
// once-per-transfer tier (-1/0 when the fee ran per chunk — tiers then
// vary per chunk and are disclosed on each TransferChunk). Duplicate
// replays return the originally posted entries with Duplicate == true
// and book nothing new. FX discloses the conversion of a cross-currency
// split on the original posting; replays return FX == nil, like
// TransferReceipt.
type SplitTransferReceipt struct {
	TransferID   string          `json:"transfer_id"`
	Chunks       []TransferChunk `json:"chunks"`
	Entries      []JournalEntry  `json:"entries"`
	FeeCents     int64           `json:"fee_cents"`
	FeeTierIndex int             `json:"fee_tier_index"`
	FeeRateBps   int64           `json:"fee_rate_bps"`
	FeePerChunk  bool            `json:"fee_per_chunk"`
	Duplicate    bool            `json:"duplicate"`
	FX           *FXConversion   `json:"fx,omitempty"`
	ReviewStatus string          `json:"review_status,omitempty"`
}

// WithMaxTransferChunkCents sets the opt-in single-transfer cap for
// PostTransferSplit, in cents: a transfer whose amount exceeds the cap
// is split into chunks of at most this size, committed atomically. Zero
// (the default) disables chunking. A negative cap panics at construction
// — fail-fast, like the fee schedule — so a misconfigured cap can never
// silently disable splitting.
func WithMaxTransferChunkCents(chunkCents int64) Option {
	if chunkCents < 0 {
		panic("ledger: max transfer chunk must not be negative")
	}
	return func(l *Ledger) {
		l.maxTransferChunkCents = chunkCents
	}
}

// WithSplitFeePerChunk makes PostTransferSplit resolve the transfer fee
// policy per chunk instead of once per transfer (see the package doc):
// each chunk's fee leg is "<transfer ID>/chunk-<i>/fee" to the policy's
// revenue account. An explicit FeeCents is always charged once per
// transfer. Off by default: the LG-17 once-per-transfer fee applies.
func WithSplitFeePerChunk() Option {
	return func(l *Ledger) {
		l.splitFeePerChunk = true
	}
}

// PostTransferSplit records a transfer, splitting it into atomic chunks
// when its amount exceeds the chunk cap (see the package doc for the
// sizing rule, fee modes, idempotency, and review interaction).
//
// When no chunking applies — no cap configured, or the amount fits in
// one chunk — the transfer posts exactly as PostTransfer would (same
// entry IDs, same fee leg, same review gating) and the receipt is
// wrapped with a single-chunk disclosure.
func (l *Ledger) PostTransferSplit(t Transfer) (SplitTransferReceipt, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	var err error
	var toCurrency string
	if t, toCurrency, err = validateTransferFields(t); err != nil {
		return SplitTransferReceipt{}, err
	}

	// Sanctions/denylist screening (see denylist.go): like PostTransfer,
	// the payer and payee are screened before any chunk simulation or
	// the idempotency replay check — the no-split delegate path below
	// re-screens inside postTransferLocked, which is a harmless cheap
	// second lookup.
	if acct, entry, denied := l.denylistRejectedLocked(t.From, t.To); denied {
		return SplitTransferReceipt{}, l.deniedRejectionLocked("PostTransferSplit", t.ID, acct, entry, []AccountID{t.From, t.To})
	}

	chunkSize := l.splitChunkSizeLocked(t.From, t.Currency)
	if chunkSize <= 0 || t.AmountCents <= chunkSize {
		// No split needed: delegate so a non-split transfer is
		// byte-for-byte what PostTransfer would have booked. The
		// split path holds the write lock from entry, so there is
		// no admission window: the zero quote keeps commit-time FX
		// rate semantics with no slippage gate (see FXRateQuote).
		receipt, err := l.postTransferLocked(t, FXRateQuote{})
		if err != nil {
			return SplitTransferReceipt{}, err
		}
		return singleChunkReceipt(receipt), nil
	}
	now := time.Now()
	if toCurrency != t.Currency {
		return l.postTransferSplitFXLocked(t, toCurrency, chunkSize, now)
	}
	return l.postTransferSplitLocked(t, chunkSize, now)
}

// splitChunkSizeLocked returns the maximum chunk size for a split
// transfer from account in currency: the tighter of the opt-in
// single-transfer cap and the account's daily outflow limit (LG-26). A
// zero daily limit stops all outflow rather than sizing chunks — the
// limit checks below reject regardless. Returns 0 when neither is
// configured (no chunking). Callers must hold l.mu.
func (l *Ledger) splitChunkSizeLocked(from AccountID, currency string) int64 {
	var size int64
	if l.maxTransferChunkCents > 0 {
		size = l.maxTransferChunkCents
	}
	if limit, ok := l.dailyLimits[dailyLimitKey{account: from, currency: currency}]; ok && limit > 0 {
		if size == 0 || limit < size {
			size = limit
		}
	}
	return size
}

// chunkAmountsFor splits amount into chunks of at most chunkSize: full
// chunks followed by a possibly smaller tail. Callers must ensure
// chunkSize >= 1 and amount > chunkSize, so the result has at least two
// chunks and the division cannot overflow.
func chunkAmountsFor(amount, chunkSize int64) []int64 {
	n := (amount-1)/chunkSize + 1
	chunks := make([]int64, n)
	for i := range chunks {
		chunks[i] = chunkSize
	}
	chunks[n-1] = amount - chunkSize*(n-1)
	return chunks
}

// splitChunkEntryID is the journal entry ID of a chunk principal:
// "<transfer ID>/chunk-<i>", in the "<transfer ID>/fee" namespace family.
func splitChunkEntryID(transferID string, i int) string {
	return transferID + "/chunk-" + strconv.Itoa(i)
}

// splitFee is the resolved fee plan for a split transfer.
type splitFee struct {
	// perChunk is true when the fee policy is applied per chunk
	// (WithSplitFeePerChunk); an explicit FeeCents is always charged
	// once per transfer.
	perChunk bool
	// account receives the fee leg(s).
	account AccountID
	// total is the fee booked across the whole split.
	total int64
	// tierIndex/rateBps disclose the once-per-transfer tier (-1/0 when
	// perChunk: tiers vary per chunk and are disclosed there).
	tierIndex int
	rateBps   int64
	// chunkFees/chunkTier/chunkRate are the per-chunk policy fees and
	// their tiers (zero values unless perChunk).
	chunkFees []int64
	chunkTier []int
	chunkRate []int64
}

// splitFeeLocked resolves the fee plan for a split transfer: it runs
// resolveFeeLocked on the whole transfer first, so explicit fees, fee
// accounts, and policy misconfiguration fail exactly like PostTransfer,
// then — in per-chunk mode — resolves the policy fee per chunk.
// Callers must hold l.mu.
func (l *Ledger) splitFeeLocked(t Transfer, chunks []int64) (splitFee, error) {
	wholeFee, feeAccount, splits, tierIndex, rateBps, err := l.resolveFeeLocked(t)
	if err != nil {
		return splitFee{}, err
	}
	if len(splits) > 0 {
		// Fee splits compose with PostTransfer, not with chunked
		// transfers: splitting a chunked transfer's fee per recipient
		// would need per-chunk split legs, which the journal ID scheme
		// does not define. Fail fast instead of misbooking.
		return splitFee{}, ErrFeeSplitUnsupported
	}
	fee := splitFee{
		account:   feeAccount,
		total:     wholeFee,
		tierIndex: tierIndex,
		rateBps:   rateBps,
		chunkFees: make([]int64, len(chunks)),
		chunkTier: make([]int, len(chunks)),
		chunkRate: make([]int64, len(chunks)),
	}
	for i := range fee.chunkTier {
		fee.chunkTier[i] = -1
	}
	// Per-chunk fees apply only to the policy-computed fee: an explicit
	// FeeCents is a fixed per-transfer amount, and SkipFee (or a
	// disabled policy) means no fee at all.
	if !l.splitFeePerChunk || t.FeeCents > 0 || t.SkipFee || feeAccount == "" {
		return fee, nil
	}
	fee.perChunk = true
	fee.tierIndex, fee.rateBps = -1, 0
	var total int64
	for i, amt := range chunks {
		f, idx, rate := l.policyFeeForLocked(amt)
		fee.chunkFees[i] = f
		fee.chunkTier[i] = idx
		fee.chunkRate[i] = rate
		// Each chunk fee fits under its chunk amount (rates are capped
		// at 10000 bps), so the sum fits under the transfer amount:
		// no overflow.
		total += f
	}
	fee.total = total
	return fee, nil
}

// checkSplitDailyOutflowLocked simulates the daily-limit check for a
// sequence of payer outflows without mutating the buckets: each outflow
// is checked in order against the running day total — the ledger's
// committed buckets plus the earlier outflows in the sequence — with the
// same overflow-safe comparison as dailyLimitRejectedLocked, and the
// running total saturates like addDailyOutflowLocked.
// LG-80: each limited account's budget covers its whole subtree, so the
// simulation checks the payer's own limit and every strict ancestor's.
// Callers must hold l.mu.
func (l *Ledger) checkSplitDailyOutflowLocked(account AccountID, currency string, outflows []int64, at time.Time) error {
	extra := make(map[dailyLimitKey]int64, len(outflows))
	key := dailyLimitKey{account: account, currency: currency, day: utcDay(at)}
	for _, outflow := range outflows {
		if l.subtreeLimitsViolatedLocked(account, currency, outflow, at, extra) {
			return ErrDailyLimitExceeded
		}
		extra[key] = satAddInt64(extra[key], outflow)
	}
	return nil
}

// checkSplitBudgetLocked simulates the monthly-budget check for a
// sequence of payer outflows without mutating the buckets: each outflow
// is checked in order against the running month total — the ledger's
// committed buckets plus the earlier outflows in the sequence — with the
// same overflow-safe comparison as budgetRejectedLocked, and the running
// total saturates like addMonthlySpendLocked. Unlike the daily limit,
// budgets are per-account (no subtree semantics). Callers must hold l.mu.
func (l *Ledger) checkSplitBudgetLocked(account AccountID, currency string, outflows []int64, at time.Time) error {
	extra := make(map[monthlySpendKey]int64, len(outflows))
	for _, outflow := range outflows {
		if l.budgetViolatedLocked(account, currency, outflow, at, extra) {
			return ErrBudgetExceeded
		}
		key := monthlySpendKey{account: account, currency: currency, month: utcMonth(at)}
		extra[key] = satAddInt64(extra[key], outflow)
	}
	return nil
}

// postTransferSplitLocked records a same-currency split transfer: every
// check runs before the first mutation, so a rejection records nothing.
// Callers must hold the write lock.
func (l *Ledger) postTransferSplitLocked(t Transfer, chunkSize int64, now time.Time) (SplitTransferReceipt, error) {
	// CreatedAt is filled before the risk checks (like the FX path), so
	// the daily-limit bucket and the period gate agree with the booked
	// entries' timestamps.
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}

	// Dual-control review is evaluated on the WHOLE transfer amount: a
	// split must never smuggle a large transfer past the review
	// threshold chunk by chunk.
	if threshold, ok := l.effectiveReviewThresholdLocked(t.From); ok && t.AmountCents >= threshold {
		return SplitTransferReceipt{}, ErrSplitTransferNeedsReview
	}

	chunks := chunkAmountsFor(t.AmountCents, chunkSize)
	fee, err := l.splitFeeLocked(t, chunks)
	if err != nil {
		return SplitTransferReceipt{}, err
	}
	totalOutflow, ok := addCents(t.AmountCents, fee.total)
	if !ok {
		return SplitTransferReceipt{}, ErrAmountOverflow
	}

	// The whole-transfer idempotency key replays the whole receipt.
	if t.IdempotencyKey != "" {
		if receipt, ok := l.replayTransferSplitLocked(t); ok {
			return receipt, nil
		}
	}

	// Entry-ID conflicts: every chunk and fee entry ID must be free, and
	// the bare transfer ID must not be taken by a plain transfer — one
	// identity, one transfer.
	for i := range chunks {
		if _, exists := l.entries[splitChunkEntryID(t.ID, i)]; exists {
			return SplitTransferReceipt{}, ErrTransferIDConflict
		}
		if fee.perChunk && fee.chunkFees[i] > 0 {
			if _, exists := l.entries[splitChunkEntryID(t.ID, i)+"/fee"]; exists {
				return SplitTransferReceipt{}, ErrTransferIDConflict
			}
		}
	}
	if _, exists := l.entries[t.ID]; exists {
		return SplitTransferReceipt{}, ErrTransferIDConflict
	}
	if !fee.perChunk && fee.total > 0 {
		if _, exists := l.entries[t.ID+"/fee"]; exists {
			return SplitTransferReceipt{}, ErrTransferIDConflict
		}
	}
	if t.ExternalRef != "" {
		if owner, taken := l.byRef[t.ExternalRef]; taken {
			return SplitTransferReceipt{}, fmt.Errorf("%w: reference %q already journaled on entry %q", ErrDuplicateExternalRef, t.ExternalRef, owner)
		}
	}

	l.releaseExpiredFreezesLocked(now, "write_path")

	if l.closedLocked(t.From) || l.closedLocked(t.To) ||
		(fee.account != "" && l.closedLocked(fee.account)) {
		return SplitTransferReceipt{}, ErrAccountClosed
	}
	if l.frozenLocked(t.From) || l.frozenLocked(t.To) ||
		(fee.account != "" && l.frozenLocked(fee.account)) {
		return SplitTransferReceipt{}, ErrAccountFrozen
	}

	// Overdraft guards the payer's TOTAL outflow: the split commits
	// atomically, so the payer's balance drops by amount + total fee at
	// once — checking per chunk would understate the risk.
	if l.noOverdraft[t.From] &&
		l.balances[accountCurrency{account: t.From, currency: t.Currency}] < totalOutflow {
		return SplitTransferReceipt{}, ErrAccountOverdraft
	}

	// Daily outflow limits apply per chunk, cumulatively, in commit
	// order (the once-per-transfer fee leg is the last posting).
	outflows := make([]int64, 0, len(chunks)+1)
	for i, amt := range chunks {
		outflow, ok := addCents(amt, fee.chunkFees[i])
		if !ok {
			return SplitTransferReceipt{}, ErrAmountOverflow
		}
		outflows = append(outflows, outflow)
	}
	if !fee.perChunk && fee.total > 0 {
		outflows = append(outflows, fee.total)
	}
	if err := l.checkSplitDailyOutflowLocked(t.From, t.Currency, outflows, t.CreatedAt); err != nil {
		return SplitTransferReceipt{}, err
	}
	if err := l.checkSplitBudgetLocked(t.From, t.Currency, outflows, t.CreatedAt); err != nil {
		return SplitTransferReceipt{}, err
	}

	if err := l.periodRejectedLocked(t.CreatedAt); err != nil {
		return SplitTransferReceipt{}, err
	}

	// Build the entries: chunk i books "<id>/chunk-i" (debit To, credit
	// From); a per-chunk fee leg follows its chunk. The memo and the
	// external reference ride chunk 0 only — repeating them on every
	// chunk would duplicate them in journal exports.
	var entries []JournalEntry
	chunkEntryIdx := make([][]int, len(chunks))
	for i, amt := range chunks {
		e := JournalEntry{
			ID:            splitChunkEntryID(t.ID, i),
			DebitAccount:  t.To,
			CreditAccount: t.From,
			AmountCents:   amt,
			Currency:      t.Currency,
			CreatedAt:     t.CreatedAt,
		}
		if t.IdempotencyKey != "" {
			e.IdempotencyKey = fmt.Sprintf("%s/chunk-%d", t.IdempotencyKey, i)
		}
		if i == 0 {
			e.Memo = t.Memo
			e.ExternalRef = t.ExternalRef
		}
		chunkEntryIdx[i] = append(chunkEntryIdx[i], len(entries))
		entries = append(entries, e)
		if fee.perChunk && fee.chunkFees[i] > 0 {
			fe := JournalEntry{
				ID:            splitChunkEntryID(t.ID, i) + "/fee",
				DebitAccount:  fee.account,
				CreditAccount: t.From,
				AmountCents:   fee.chunkFees[i],
				Currency:      t.Currency,
				CreatedAt:     t.CreatedAt,
			}
			if t.IdempotencyKey != "" {
				fe.IdempotencyKey = fmt.Sprintf("%s/chunk-%d/fee", t.IdempotencyKey, i)
			}
			chunkEntryIdx[i] = append(chunkEntryIdx[i], len(entries))
			entries = append(entries, fe)
		}
	}
	if !fee.perChunk && fee.total > 0 {
		fe := JournalEntry{
			ID:            t.ID + "/fee",
			DebitAccount:  fee.account,
			CreditAccount: t.From,
			AmountCents:   fee.total,
			Currency:      t.Currency,
			CreatedAt:     t.CreatedAt,
		}
		if t.IdempotencyKey != "" {
			fe.IdempotencyKey = t.IdempotencyKey + "/fee"
		}
		entries = append(entries, fe)
	}

	// No-FX invariant, like PostTransfer: every leg is booked in the
	// transfer's single currency.
	for _, e := range entries {
		if e.Currency != t.Currency {
			return SplitTransferReceipt{}, ErrCrossCurrencyTransfer
		}
	}

	l.commitSplitLocked(t, entries, outflows, fee.total, now, map[string]any{
		"amount_cents":     t.AmountCents,
		"currency":         t.Currency,
		"chunk_count":      len(chunks),
		"chunk_size_cents": chunkSize,
		"fee_cents":        fee.total,
		"fee_per_chunk":    fee.perChunk,
		"fee_tier_index":   fee.tierIndex,
		"fee_rate_bps":     fee.rateBps,
	}, map[string]any{"split": true, "chunk_count": len(chunks)})

	return assembleSplitReceipt(t, chunks, fee, entries, chunkEntryIdx, nil), nil
}

// postTransferSplitFXLocked records a cross-currency split transfer:
// each chunk converts at the one rate looked up for the transfer, the
// fee leg stays in the source currency, and every leg keeps its single
// currency — the FX path's per-leg single-currency requirement,
// continued. Callers must hold the write lock.
func (l *Ledger) postTransferSplitFXLocked(t Transfer, toCurrency string, chunkSize int64, now time.Time) (SplitTransferReceipt, error) {
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}

	// Review, like the same-currency path, is evaluated on the whole
	// source-currency amount: the threshold applies to the principal the
	// payer parts with.
	if threshold, ok := l.effectiveReviewThresholdLocked(t.From); ok && t.AmountCents >= threshold {
		return SplitTransferReceipt{}, ErrSplitTransferNeedsReview
	}

	chunks := chunkAmountsFor(t.AmountCents, chunkSize)
	fee, err := l.splitFeeLocked(t, chunks)
	if err != nil {
		return SplitTransferReceipt{}, err
	}
	totalOutflow, ok := addCents(t.AmountCents, fee.total)
	if !ok {
		return SplitTransferReceipt{}, ErrAmountOverflow
	}

	// The idempotency replay check runs before the rate lookup, mirroring
	// postTransferFXLocked.
	if t.IdempotencyKey != "" {
		if receipt, ok := l.replayTransferSplitLocked(t); ok {
			return receipt, nil
		}
	}

	// The one rate for the whole split, resolved at commit: the chunk
	// path holds the write lock from entry, so there is no admission
	// window — every chunk prices at this rate (see effectiveFXRateLocked;
	// LG-86 admission quotes and the slippage gate apply only to the
	// PostTransfer path, which admits before taking the write lock).
	// Since LG-93 the resolution is side-aware (see fxSideOf): chunks
	// price at the dealing price like a plain FX transfer.
	side := fxSideOf(t)
	pair := fxPair{from: t.Currency, to: toCurrency}
	commitEff, err := l.effectiveFXRateLocked(pair, side, now)
	if err != nil {
		return SplitTransferReceipt{}, err
	}
	rate := commitEff.Rate
	effNum, effDen := commitEff.Num, commitEff.Den
	effVersion := commitEff.EffectiveVersion
	effExpiresAt := commitEff.ExpiresAt
	override := commitEff.Override
	overridden := commitEff.Overridden
	fxAccount := t.FXAccount
	if fxAccount == "" {
		fxAccount = l.fxAccount
	}
	if fxAccount == "" {
		return SplitTransferReceipt{}, ErrFXAccountNotConfigured
	}
	if fxAccount == t.From || fxAccount == t.To {
		return SplitTransferReceipt{}, ErrInvalidFXAccount
	}

	// Convert per chunk at the one rate, floored per chunk like the
	// single-transfer conversion. The receipt discloses the summed
	// conversion, which may differ by a cent from converting the whole
	// amount at once.
	converted := make([]int64, len(chunks))
	for i, amt := range chunks {
		c, ok := fxConvertCents(amt, effNum, effDen)
		if !ok {
			return SplitTransferReceipt{}, ErrAmountOverflow
		}
		converted[i] = c
	}
	var totalConverted int64
	for _, c := range converted {
		var ok bool
		totalConverted, ok = addCents(totalConverted, c)
		if !ok {
			return SplitTransferReceipt{}, ErrAmountOverflow
		}
	}

	// LG-93: the spread accrues per chunk, against the same reference
	// mid as the plain path (see fxSpreadCentsOf), and books as one
	// aggregated "<transfer ID>/spread" leg — the transfer-level P&L,
	// mirroring the once-per-transfer fee leg's shape. The per-chunk
	// spread may differ by a cent from converting the whole amount at
	// once, exactly like the conversion itself.
	var totalSpread int64
	for _, amt := range chunks {
		midC, ok := fxConvertCents(amt, commitEff.MidNum, commitEff.MidDen)
		if !ok {
			return SplitTransferReceipt{}, ErrAmountOverflow
		}
		sideC, ok := fxConvertCents(amt, effNum, effDen)
		if !ok {
			return SplitTransferReceipt{}, ErrAmountOverflow
		}
		s, ok := addCents(totalSpread, fxSpreadCentsOf(side, sideC, midC))
		if !ok {
			return SplitTransferReceipt{}, ErrAmountOverflow
		}
		totalSpread = s
	}
	// Fail-closed, before any mutation: a positive spread with no usable
	// income account rejects the split — the P&L must have somewhere to
	// go (see resolveFXIncomeAccountLocked).
	incomeAccount, err := l.resolveFXIncomeAccountLocked(t, fxAccount, totalSpread)
	if err != nil {
		return SplitTransferReceipt{}, err
	}

	for i := range chunks {
		for _, id := range []string{splitChunkEntryID(t.ID, i), splitChunkEntryID(t.ID, i) + "/fx"} {
			if _, exists := l.entries[id]; exists {
				return SplitTransferReceipt{}, ErrTransferIDConflict
			}
		}
		if fee.perChunk && fee.chunkFees[i] > 0 {
			if _, exists := l.entries[splitChunkEntryID(t.ID, i)+"/fee"]; exists {
				return SplitTransferReceipt{}, ErrTransferIDConflict
			}
		}
	}
	if _, exists := l.entries[t.ID]; exists {
		return SplitTransferReceipt{}, ErrTransferIDConflict
	}
	if !fee.perChunk && fee.total > 0 {
		if _, exists := l.entries[t.ID+"/fee"]; exists {
			return SplitTransferReceipt{}, ErrTransferIDConflict
		}
	}
	if t.ExternalRef != "" {
		if owner, taken := l.byRef[t.ExternalRef]; taken {
			return SplitTransferReceipt{}, fmt.Errorf("%w: reference %q already journaled on entry %q", ErrDuplicateExternalRef, t.ExternalRef, owner)
		}
	}
	// LG-93: the aggregated spread leg's namespace, like the fee leg's.
	if _, exists := l.entries[t.ID+"/spread"]; exists {
		return SplitTransferReceipt{}, ErrTransferIDConflict
	}

	l.releaseExpiredFreezesLocked(now, "write_path")

	// Frozen and overdraft checks cover every account a leg touches. The
	// payer's risk is entirely in the source currency, like
	// postTransferFXLocked: overdraft and the daily limit both guard the
	// source-currency total outflow, never the converted amounts. The
	// FX income account walks the same frozen/closed checks as the FX
	// account when a spread leg books (LG-93).
	if l.closedLocked(t.From) || l.closedLocked(t.To) || l.closedLocked(fxAccount) ||
		(fee.account != "" && l.closedLocked(fee.account)) ||
		(incomeAccount != "" && l.closedLocked(incomeAccount)) {
		return SplitTransferReceipt{}, ErrAccountClosed
	}
	if l.frozenLocked(t.From) || l.frozenLocked(t.To) || l.frozenLocked(fxAccount) ||
		(fee.account != "" && l.frozenLocked(fee.account)) ||
		(incomeAccount != "" && l.frozenLocked(incomeAccount)) {
		return SplitTransferReceipt{}, ErrAccountFrozen
	}
	if l.noOverdraft[t.From] &&
		l.balances[accountCurrency{account: t.From, currency: t.Currency}] < totalOutflow {
		return SplitTransferReceipt{}, ErrAccountOverdraft
	}

	outflows := make([]int64, 0, len(chunks)+1)
	for i, amt := range chunks {
		outflow, ok := addCents(amt, fee.chunkFees[i])
		if !ok {
			return SplitTransferReceipt{}, ErrAmountOverflow
		}
		outflows = append(outflows, outflow)
	}
	if !fee.perChunk && fee.total > 0 {
		outflows = append(outflows, fee.total)
	}
	if err := l.checkSplitDailyOutflowLocked(t.From, t.Currency, outflows, t.CreatedAt); err != nil {
		return SplitTransferReceipt{}, err
	}
	if err := l.checkSplitBudgetLocked(t.From, t.Currency, outflows, t.CreatedAt); err != nil {
		return SplitTransferReceipt{}, err
	}

	if err := l.periodRejectedLocked(t.CreatedAt); err != nil {
		return SplitTransferReceipt{}, err
	}

	// Build the entries: per chunk, "<id>/chunk-i" (debit To, credit
	// FX account, converted amount, settlement currency) and
	// "<id>/chunk-i/fx" (debit FX account, credit From, source amount,
	// source currency — the memo-carrying leg on chunk 0, mirroring
	// postTransferFXLocked), then per-chunk fee legs and the
	// once-per-transfer fee leg, both in the source currency.
	var entries []JournalEntry
	chunkEntryIdx := make([][]int, len(chunks))
	for i, amt := range chunks {
		toLeg := JournalEntry{
			ID:            splitChunkEntryID(t.ID, i),
			DebitAccount:  t.To,
			CreditAccount: fxAccount,
			AmountCents:   converted[i],
			Currency:      toCurrency,
			CreatedAt:     t.CreatedAt,
		}
		if t.IdempotencyKey != "" {
			toLeg.IdempotencyKey = fmt.Sprintf("%s/chunk-%d", t.IdempotencyKey, i)
		}
		chunkEntryIdx[i] = append(chunkEntryIdx[i], len(entries))
		entries = append(entries, toLeg)

		fxLeg := JournalEntry{
			ID:            splitChunkEntryID(t.ID, i) + "/fx",
			DebitAccount:  fxAccount,
			CreditAccount: t.From,
			AmountCents:   amt,
			Currency:      t.Currency,
			CreatedAt:     t.CreatedAt,
		}
		if i == 0 {
			fxLeg.Memo = t.Memo
			fxLeg.ExternalRef = t.ExternalRef
		}
		chunkEntryIdx[i] = append(chunkEntryIdx[i], len(entries))
		entries = append(entries, fxLeg)

		if fee.perChunk && fee.chunkFees[i] > 0 {
			fe := JournalEntry{
				ID:            splitChunkEntryID(t.ID, i) + "/fee",
				DebitAccount:  fee.account,
				CreditAccount: t.From,
				AmountCents:   fee.chunkFees[i],
				Currency:      t.Currency,
				CreatedAt:     t.CreatedAt,
			}
			if t.IdempotencyKey != "" {
				fe.IdempotencyKey = fmt.Sprintf("%s/chunk-%d/fee", t.IdempotencyKey, i)
			}
			chunkEntryIdx[i] = append(chunkEntryIdx[i], len(entries))
			entries = append(entries, fe)
		}
	}
	if !fee.perChunk && fee.total > 0 {
		fe := JournalEntry{
			ID:            t.ID + "/fee",
			DebitAccount:  fee.account,
			CreditAccount: t.From,
			AmountCents:   fee.total,
			Currency:      t.Currency,
			CreatedAt:     t.CreatedAt,
		}
		if t.IdempotencyKey != "" {
			fe.IdempotencyKey = t.IdempotencyKey + "/fee"
		}
		entries = append(entries, fe)
	}
	// LG-93: the aggregated spread leg — one "<transfer ID>/spread" leg
	// for the whole split, debit FX account, credit the FX income
	// account, target currency. Like the plain path, it books after the
	// conversion legs; it is deliberately outside chunkEntryIdx (a
	// transfer-level leg, like the once-per-transfer fee leg).
	if totalSpread > 0 {
		entries = append(entries, buildFXSpreadLeg(t.ID, fxAccount, incomeAccount, totalSpread, toCurrency, t.CreatedAt))
	}

	// Every leg is single-currency by construction; the check is the
	// explicit seam keeping mixed-currency legs rejected.
	for _, e := range entries {
		if e.Currency != t.Currency && e.Currency != toCurrency {
			return SplitTransferReceipt{}, ErrCrossCurrencyTransfer
		}
	}

	fxConv := &FXConversion{
		FromCurrency:     t.Currency,
		ToCurrency:       toCurrency,
		SourceCents:      t.AmountCents,
		RateNum:          effNum,
		RateDen:          effDen,
		ConvertedCents:   totalConverted,
		EffectiveVersion: effVersion,
		RateExpiresAt:    effExpiresAt,
		// LG-45: pin the conversion to the current snapshot version.
		RateSnapshotVersion: l.fxSnapshotVersion,
		// LG-93: the dealing side, the two-sided quote, and the
		// aggregated spread, like the plain path.
		Direction:       side,
		MidNum:          commitEff.MidNum,
		MidDen:          commitEff.MidDen,
		BidNum:          commitEff.BidNum,
		BidDen:          commitEff.BidDen,
		AskNum:          commitEff.AskNum,
		AskDen:          commitEff.AskDen,
		SpreadBps:       fxSpreadBps(commitEff.BidNum, commitEff.BidDen, commitEff.AskNum, commitEff.AskDen, commitEff.MidNum, commitEff.MidDen),
		SpreadCents:     totalSpread,
		FXIncomeAccount: incomeAccount,
	}
	if overridden {
		fxConv.OverrideApplied = true
		fxConv.MarketRateNum = rate.Num
		fxConv.MarketRateDen = rate.Den
		fxConv.OverrideApprovedBy = override.Approver
	}
	// An override-priced conversion is individually auditable, like in
	// postTransferFXLocked. Nothing after this point can fail, so
	// emitting here covers the path exactly once.
	if overridden {
		l.fxOverrideConversions++
		l.emitRateOverriddenAuditLocked(t, toCurrency, override, rate, totalConverted)
	}

	auditDetails := map[string]any{
		"amount_cents":     t.AmountCents,
		"currency":         t.Currency,
		"to_currency":      toCurrency,
		"chunk_count":      len(chunks),
		"chunk_size_cents": chunkSize,
		"fee_cents":        fee.total,
		"fee_per_chunk":    fee.perChunk,
		"fee_tier_index":   fee.tierIndex,
		"fee_rate_bps":     fee.rateBps,
		// LG-93: the dealing side and the spread, sealed with the rest
		// of the conversion's provenance.
		"direction":    side,
		"spread_bps":   fxConv.SpreadBps,
		"spread_cents": totalSpread,
	}
	if incomeAccount != "" {
		auditDetails["fx_income_account"] = string(incomeAccount)
	}
	l.commitSplitLocked(t, entries, outflows, fee.total, now, auditDetails,
		map[string]any{"split": true, "chunk_count": len(chunks), "direction": side, "spread_cents": totalSpread})

	// LG-93: the spread booking counters, like the plain path.
	if totalSpread > 0 {
		l.fxSpreadBookings++
		l.fxSpreadCentsTotal += totalSpread
	}

	receipt := assembleSplitReceipt(t, chunks, fee, entries, chunkEntryIdx, fxConv)
	return receipt, nil
}

// commitSplitLocked atomically commits validated split entries: journal
// rows, idempotency index entries, balances, totals, chain links, and
// version bumps land together, then the daily-outflow adds (per posting,
// in commit order), the transfer key index, the audit event, the outbox
// envelope, and the balance hooks and watches. Callers must hold the
// write lock and must have run every check first; after this returns the
// split is fully committed — there is nothing to roll back.
func (l *Ledger) commitSplitLocked(t Transfer, entries []JournalEntry, outflows []int64, feeTotal int64, now time.Time, auditDetails map[string]any, outboxExtra map[string]any) {
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
	// The payer's outflow is recorded per posting, in commit order, in
	// the transfer's (source) currency — mirroring the cumulative check.
	for _, outflow := range outflows {
		l.addDailyOutflowLocked(t.From, t.Currency, outflow, t.CreatedAt)
		l.addMonthlySpendLocked(t.From, t.Currency, outflow, t.CreatedAt)
	}
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
	l.evaluateBalanceWatchesLocked(touchedAccounts(entries), t.ID, "PostTransferSplit")
	l.emitAudit(AuditEvent{
		Op:            "transfer_split",
		Actor:         "PostTransferSplit",
		TraceID:       t.ID,
		VersionBefore: versionBefore,
		VersionAfter:  l.version,
		EntryIDs:      entryIDs,
		Accounts:      []AccountID{t.From, t.To},
		Details:       auditDetails,
	})
	// Domain outbox: entry_posted per leg, then the transfer_posted
	// envelope with the split disclosure.
	l.emitOutboxTransferLocked(t, feeTotal, entries, outboxExtra)
	// Balance-change notification: strictly after the atomic commit
	// zone, advisory only — async dispatch, hook failures isolated.
	l.fireBalanceHooksLocked(hookTouched, hookOld, t.ID)
}

// assembleSplitReceipt builds the SplitTransferReceipt for a committed
// split from the chunk plan and the committed entries. chunkEntryIdx maps
// each chunk to its entry positions in entries, in commit order.
func assembleSplitReceipt(t Transfer, chunks []int64, fee splitFee, entries []JournalEntry, chunkEntryIdx [][]int, fx *FXConversion) SplitTransferReceipt {
	splitChunks := make([]TransferChunk, len(chunks))
	for i, amt := range chunks {
		ids := make([]string, 0, len(chunkEntryIdx[i]))
		for _, ei := range chunkEntryIdx[i] {
			ids = append(ids, entries[ei].ID)
		}
		tierIdx, tierRate := -1, int64(0)
		if fee.perChunk {
			tierIdx, tierRate = fee.chunkTier[i], fee.chunkRate[i]
		}
		splitChunks[i] = TransferChunk{
			ChunkIndex:   i,
			ChunkCount:   len(chunks),
			AmountCents:  amt,
			EntryIDs:     ids,
			FeeCents:     fee.chunkFees[i],
			FeeTierIndex: tierIdx,
			FeeRateBps:   tierRate,
		}
	}
	return SplitTransferReceipt{
		TransferID:   t.ID,
		Chunks:       splitChunks,
		Entries:      entries,
		FeeCents:     fee.total,
		FeeTierIndex: fee.tierIndex,
		FeeRateBps:   fee.rateBps,
		FeePerChunk:  fee.perChunk,
		Duplicate:    false,
		FX:           fx,
	}
}

// singleChunkReceipt wraps a PostTransfer receipt from the no-split
// delegate path with a single-chunk disclosure.
func singleChunkReceipt(r TransferReceipt) SplitTransferReceipt {
	entryIDs := make([]string, 0, len(r.Entries))
	for _, e := range r.Entries {
		entryIDs = append(entryIDs, e.ID)
	}
	amount := int64(0)
	if len(r.Entries) > 0 {
		amount = r.Entries[0].AmountCents
		for _, e := range r.Entries {
			if strings.HasSuffix(e.ID, "/fx") {
				amount = e.AmountCents // FX transfers: the source-currency amount
				break
			}
		}
	}
	return SplitTransferReceipt{
		TransferID: r.TransferID,
		Chunks: []TransferChunk{{
			ChunkIndex:   0,
			ChunkCount:   1,
			AmountCents:  amount,
			EntryIDs:     entryIDs,
			FeeCents:     r.FeeCents,
			FeeTierIndex: r.FeeTierIndex,
			FeeRateBps:   r.FeeRateBps,
		}},
		Entries:      r.Entries,
		FeeCents:     r.FeeCents,
		FeeTierIndex: r.FeeTierIndex,
		FeeRateBps:   r.FeeRateBps,
		Duplicate:    r.Duplicate,
		FX:           r.FX,
		ReviewStatus: r.ReviewStatus,
	}
}

// replayTransferSplitLocked rebuilds the whole split receipt from the
// idempotency key without booking anything new. A key that belongs to a
// plain PostTransfer (the no-split delegate path, or a transfer posted
// before chunking existed) replays as a single-chunk receipt, mirroring
// replayTransferLocked's byKey fallback. Callers must hold the write
// lock.
func (l *Ledger) replayTransferSplitLocked(t Transfer) (SplitTransferReceipt, bool) {
	if ids, ok := l.transferKeys[t.IdempotencyKey]; ok {
		return l.rebuildSplitReceiptLocked(t, ids), true
	}
	if orig, ok := l.byKey[t.IdempotencyKey]; ok {
		return singleChunkReceipt(TransferReceipt{
			TransferID: t.ID,
			Entries:    []JournalEntry{orig},
			Duplicate:  true,
		}), true
	}
	return SplitTransferReceipt{}, false
}

// splitChunkRef parses a split chunk entry ID into its chunk index and
// leg kind, without relying on the transfer ID (the replay request's ID
// may differ from the original's): "<id>/chunk-<i>" is the chunk
// principal, "<id>/chunk-<i>/fx" the FX source leg, "<id>/chunk-<i>/fee"
// a per-chunk fee leg. ok is false for anything else (including the
// once-per-transfer "<id>/fee" leg).
func splitChunkRef(id string) (index int, kind string, ok bool) {
	j := strings.Index(id, "/chunk-")
	if j < 0 {
		return 0, "", false
	}
	rest := id[j+len("/chunk-"):]
	num, tail, _ := strings.Cut(rest, "/")
	i, err := strconv.Atoi(num)
	if err != nil || i < 0 || (tail != "" && tail != "fx" && tail != "fee") {
		return 0, "", false
	}
	return i, tail, true
}

// splitBaseID recovers a split transfer's base transfer ID from one of
// its chunk leg IDs ("<base>/chunk-<i>[kind]" -> "<base>"); a non-chunk
// leg ID is returned unchanged. It lets fee-leg matching anchor on the
// transfer ID instead of a fuzzy "/fee" suffix (see isFeeLegID).
func splitBaseID(legID string) string {
	if j := strings.Index(legID, "/chunk-"); j >= 0 {
		return legID[:j]
	}
	return legID
}

// rebuildSplitReceiptLocked regroups the journal entries of a split
// transfer (looked up by idempotency key) into per-chunk disclosure. It
// handles both the true split shape ("<id>/chunk-<i>" entries) and the
// no-split delegate shape ("<id>", "<id>/fx", "<id>/fee" entries).
// Callers must hold the write lock.
func (l *Ledger) rebuildSplitReceiptLocked(t Transfer, ids []string) SplitTransferReceipt {
	entries := make([]JournalEntry, 0, len(ids))
	for _, id := range ids {
		entries = append(entries, l.entries[id])
	}
	split := false
	for _, e := range entries {
		if _, _, ok := splitChunkRef(e.ID); ok {
			split = true
			break
		}
	}
	if !split {
		// No-split delegate shape: re-derive the fee tier disclosure
		// like replayTransferLocked and wrap as a single chunk.
		feeCents := feeCentsOf(splitBaseID(entries[0].ID), entries)
		feeBase := entries[0].AmountCents
		for _, e := range entries {
			if strings.HasSuffix(e.ID, "/fx") {
				feeBase = e.AmountCents
				break
			}
		}
		tierIndex, rateBps := l.rediscloseFeeTierLocked(splitBaseID(entries[0].ID), entries, feeBase)
		r := singleChunkReceipt(TransferReceipt{
			TransferID:   t.ID,
			Entries:      entries,
			FeeCents:     feeCents,
			FeeTierIndex: tierIndex,
			FeeRateBps:   rateBps,
			Duplicate:    true,
			ReviewStatus: l.reviewStatusLocked(entries[0].ID),
		})
		return r
	}

	// Group entries by chunk index.
	type chunkGroup struct {
		principal *JournalEntry // "" kind, or the "fx" kind on FX splits
		fx        *JournalEntry
		fee       *JournalEntry
	}
	groups := map[int]*chunkGroup{}
	maxIdx := -1
	var onceFee *JournalEntry
	for i := range entries {
		e := &entries[i]
		idx, kind, ok := splitChunkRef(e.ID)
		if !ok {
			// The once-per-transfer fee leg.
			if strings.HasSuffix(e.ID, "/fee") {
				onceFee = e
			}
			continue
		}
		g := groups[idx]
		if g == nil {
			g = &chunkGroup{}
			groups[idx] = g
		}
		switch kind {
		case "":
			g.principal = e
		case "fx":
			g.fx = e
		case "fee":
			g.fee = e
		}
		if idx > maxIdx {
			maxIdx = idx
		}
	}
	n := maxIdx + 1
	chunks := make([]TransferChunk, n)
	// Mode inference: a "<id>/chunk-<i>/fee" leg means the fee ran
	// per chunk; a "<id>/fee" leg (or no fee leg at all) means the
	// once-per-transfer mode. The all-dust per-chunk edge — every
	// chunk's policy fee was zero, so no fee leg exists — replays as
	// once-mode: amounts, entries, and Duplicate stay exact, only the
	// FeePerChunk flag and the per-chunk tier disclosure differ from
	// the original receipt.
	perChunk := false
	for _, g := range groups {
		if g != nil && g.fee != nil {
			perChunk = true
			break
		}
	}
	var totalFee int64
	for i := 0; i < n; i++ {
		g := groups[i]
		ids := []string{}
		var amount, chunkFee int64
		tierIdx, tierRate := -1, int64(0)
		if g != nil {
			if g.principal != nil {
				ids = append(ids, g.principal.ID)
			}
			if g.fx != nil {
				ids = append(ids, g.fx.ID)
			}
			// The chunk amount is the source-currency principal: the
			// "fx" leg on FX splits, the plain principal otherwise.
			if g.fx != nil {
				amount = g.fx.AmountCents
			} else if g.principal != nil {
				amount = g.principal.AmountCents
			}
			if g.fee != nil {
				ids = append(ids, g.fee.ID)
				chunkFee = g.fee.AmountCents
				totalFee += chunkFee
				// The fee policy is construction-time immutable, so the
				// per-chunk tier re-derives exactly.
				if f, idx, rate := l.policyFeeForLocked(amount); f == chunkFee {
					tierIdx, tierRate = idx, rate
				}
			} else if perChunk {
				// A dust chunk books no fee leg; the tier that
				// produced the zero fee is still disclosed, like on
				// the original receipt.
				if _, idx, rate := l.policyFeeForLocked(amount); len(l.feeTiers) > 0 && l.feeRevenueAccount != "" {
					tierIdx, tierRate = idx, rate
				}
			}
		}
		chunks[i] = TransferChunk{
			ChunkIndex:   i,
			ChunkCount:   n,
			AmountCents:  amount,
			EntryIDs:     ids,
			FeeCents:     chunkFee,
			FeeTierIndex: tierIdx,
			FeeRateBps:   tierRate,
		}
	}
	var onceFeeCents int64
	tierIndex, rateBps := -1, int64(0)
	if onceFee != nil {
		onceFeeCents = onceFee.AmountCents
		// Re-derive the once-per-transfer tier disclosure, like
		// replayTransferLocked: the fee base is the source-currency
		// total.
		var feeBase int64
		for _, c := range chunks {
			feeBase += c.AmountCents
		}
		tierIndex, rateBps = l.rediscloseFeeTierLocked(splitBaseID(entries[0].ID), entries, feeBase)
	}
	return SplitTransferReceipt{
		TransferID:   t.ID,
		Chunks:       chunks,
		Entries:      entries,
		FeeCents:     totalFee + onceFeeCents,
		FeeTierIndex: tierIndex,
		FeeRateBps:   rateBps,
		FeePerChunk:  perChunk,
		Duplicate:    true,
		// FX is deliberately omitted on replays: the rate table may have
		// changed since the original posting, like TransferReceipt.
		ReviewStatus: "",
	}
}

// rediscloseFeeTierLocked re-derives the fee tier disclosure for
// replayed entries: when a fee leg went to the policy's revenue account
// and the fee matches the policy computation on the source-currency
// base amount, the tier re-derives exactly (the fee policy is
// construction-time immutable). Otherwise -1/0. Callers must hold l.mu.
func (l *Ledger) rediscloseFeeTierLocked(transferID string, entries []JournalEntry, feeBase int64) (tierIndex int, rateBps int64) {
	feeCents := feeCentsOf(transferID, entries)
	tierIndex, rateBps = -1, int64(0)
	if len(l.feeTiers) > 0 && l.feeRevenueAccount != "" {
		for _, e := range entries {
			if strings.HasSuffix(e.ID, "/fee") && e.DebitAccount == l.feeRevenueAccount {
				if idx, rate := l.feeTierFor(feeBase); policyFeeCents(feeBase, rate) == feeCents {
					tierIndex, rateBps = idx, rate
				}
				break
			}
		}
		// Fee-split legs ("<id>/fee/<i>", see fee_split.go) carry no
		// revenue-account marker — a policy fee split across arbitrary
		// recipients re-derives its tier from the total instead, under
		// the same documented caveat as the single-leg case (an
		// explicit split whose total equals the policy fee is
		// indistinguishable, and the disclosed rate still matches the
		// fee math).
		if tierIndex == -1 {
			for _, e := range entries {
				if isFeeLegID(transferID, e.ID) && !strings.HasSuffix(e.ID, "/fee") {
					if idx, rate := l.feeTierFor(feeBase); policyFeeCents(feeBase, rate) == feeCents {
						tierIndex, rateBps = idx, rate
					}
					break
				}
			}
		}
	}
	return tierIndex, rateBps
}
