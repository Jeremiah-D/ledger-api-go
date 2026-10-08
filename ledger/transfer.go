package ledger

import (
	"errors"
	"math"
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
	// is booked in this one currency: the ledger performs no FX
	// conversion, so a transfer spanning currencies is rejected with
	// ErrCrossCurrencyTransfer.
	Currency string `json:"currency,omitempty"`
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
// fee leg). Duplicate replays return the originally posted entries with
// Duplicate == true and book nothing new.
type TransferReceipt struct {
	TransferID string         `json:"transfer_id"`
	Entries    []JournalEntry `json:"entries"`
	FeeCents   int64          `json:"fee_cents"`
	Duplicate  bool           `json:"duplicate"`
}

// WithTransferFeePolicy sets the ledger-wide default fee policy for
// transfers: unless a transfer carries an explicit fee or sets SkipFee,
// PostTransfer books an additional fee leg of
// floor(amount * rateBps / 10000) cents to revenueAccount. rateBps is basis
// points (250 = 2.5%); a zero rate or an empty revenue account disables
// the policy. Negative rates are normalized to 0, and rates above 10000
// bps (100%) are clamped to 10000 — a fee above the transferred amount is
// a configuration bug, and the clamp keeps fee arithmetic overflow-safe.
func WithTransferFeePolicy(rateBps int64, revenueAccount AccountID) Option {
	return func(l *Ledger) {
		if rateBps < 0 {
			rateBps = 0
		}
		if rateBps > 10000 {
			rateBps = 10000
		}
		l.feeRateBps = rateBps
		l.feeRevenueAccount = revenueAccount
	}
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
// fee in cents and the account that receives it ("" when there is no fee
// leg). Explicit fees win over the policy; the policy applies only when no
// explicit fee is given and SkipFee is false. Callers must hold l.mu.
func (l *Ledger) resolveFeeLocked(t Transfer) (feeCents int64, feeAccount AccountID, err error) {
	if t.FeeCents < 0 {
		return 0, "", ErrInvalidFee
	}
	if t.FeeCents > 0 {
		if t.FeeAccount == "" {
			return 0, "", ErrInvalidFee
		}
		return t.FeeCents, t.FeeAccount, nil
	}
	if t.FeeAccount != "" {
		// An explicit fee account without a positive fee is a caller bug:
		// silently ignoring it would misroute policy-computed fees.
		return 0, "", ErrInvalidFee
	}
	if t.SkipFee || l.feeRateBps <= 0 || l.feeRevenueAccount == "" {
		return 0, "", nil
	}
	if fee := policyFeeCents(t.AmountCents, l.feeRateBps); fee > 0 {
		return fee, l.feeRevenueAccount, nil
	}
	// A computed fee of zero (tiny amounts under a low rate) posts no leg.
	return 0, "", nil
}

// PostTransfer records an atomic transfer from one account to another,
// with an optional fee leg charged to the payer on top of the amount.
//
// The risk checks mirror Post and run in the same order: the idempotency
// replay check first (replaying a key that was posted before an account was
// frozen or protected returns the original receipt instead of failing,
// because the replay books nothing new), then the frozen check on every
// account the transfer touches (payer, payee, and fee account), then the
// overdraft check on the payer against its total outflow (amount + fee).
// Rejected transfers — validation failures, ID conflicts, frozen
// rejections, and overdraft rejections alike — record nothing: no journal
// rows, no chain links, no version bump.
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

	feeCents, feeAccount, err := l.resolveFeeLocked(t)
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
		// A transfer that posted a fee leg replays its full receipt via
		// the transfer key index; otherwise fall back to the shared
		// entry-level namespace (covers raw-posted keys and pre-fee-leg
		// transfers).
		if ids, ok := l.transferKeys[t.IdempotencyKey]; ok {
			entries := make([]JournalEntry, 0, len(ids))
			for _, id := range ids {
				entries = append(entries, l.entries[id])
			}
			return TransferReceipt{TransferID: t.ID, Entries: entries, FeeCents: feeCentsOf(entries), Duplicate: true}, nil
		}
		if orig, ok := l.byKey[t.IdempotencyKey]; ok {
			return TransferReceipt{
				TransferID: t.ID,
				Entries:    []JournalEntry{orig},
				Duplicate:  true,
			}, nil
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
	for _, e := range entries {
		l.commitEntryLocked(e)
	}
	if t.IdempotencyKey != "" {
		ids := make([]string, 0, len(entries))
		for _, e := range entries {
			ids = append(ids, e.ID)
		}
		l.transferKeys[t.IdempotencyKey] = ids
	}

	return TransferReceipt{
		TransferID: t.ID,
		Entries:    entries,
		FeeCents:   feeCents,
		Duplicate:  false,
	}, nil
}

// feeCentsOf sums the fee legs of a replayed receipt: every entry after
// the principal is a fee leg by construction.
func feeCentsOf(entries []JournalEntry) int64 {
	var total int64
	for _, e := range entries[1:] {
		total += e.AmountCents
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
