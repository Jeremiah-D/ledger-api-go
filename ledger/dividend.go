package ledger

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Dividend declaration: corporate-action dividend bookings (LG-87).
//
// DeclareDividend books one double-entry journal row per shareholder:
// debit the retained-earnings pool account, credit the holder's account,
// for floor(total * sharesBps / 10000) cents each, with the dust
// remainder (total minus the floored shares) landing on the first
// holder so the declared total is always fully distributed and the
// books stay balanced.
//
// The declaration is idempotent by construction: the dividend ID is
// deterministic — "dividend/<pool>/<YYYY-MM-DD>" (recordDate, UTC) —
// and each holder's row carries the deterministic ID
// "<dividendID>/<index>". Re-running a declaration replays the original
// rows (duplicate == true) instead of double-booking; a declaration
// interrupted by a failed leg is resumable — the retry skips the rows
// already journaled and books the rest.
//
// The record-date shareholding snapshot is tamper-evident: the holder
// table (account, sharesBps) is journaled — the amount on each row plus
// the sharesBps riding in the memo — and the audit-chain hash covers
// every journaled field. A later-changed holder table cannot silently
// replace a declared dividend: re-declaring the same (pool, recordDate)
// returns the original rows.
//
// Each leg runs the full Post gate order (validation, idempotency
// replay, chart direction, frozen, overdraft, daily-limit, budget,
// period) but, like accruals (see accrual.go), never fires the journal
// rules engine and never enters dual-control review: dividends are
// system-generated corporate actions, and operator-configured rules
// cascading derived legs off them would double-count the distribution.
// Review routing lives on PostTransfer's path only.
//
// A dividend is ordinary bookkeeping: every leg carries its own
// audit-chain link, bumps the ledger version, flows through
// VerifyAccountingEquation and Reconcile (which discloses every
// declared dividend), and emits the usual entry_posted outbox events.
// Dividends are single-currency rows; the currency defaults to the
// ledger's default currency when empty (see normalizeCurrency).

// DividendHolder is one shareholder's slice of a declared dividend:
// the account to credit and its share of the total in basis points.
type DividendHolder struct {
	Account   AccountID `json:"account"`
	SharesBps int64     `json:"shares_bps"`
}

// DividendPayout is the booked result for one holder: the journaled
// row's identity and the distributed amount.
type DividendPayout struct {
	Account     AccountID `json:"account"`
	SharesBps   int64     `json:"shares_bps"`
	AmountCents int64     `json:"amount_cents"`
	EntryID     string    `json:"entry_id"`
}

// DividendReceipt is the immutable record of one dividend declaration:
// the deterministic dividend ID plus the per-holder payouts. A receipt
// with Duplicate == true is a replay of an earlier declaration — the
// journaled rows are the original ones.
type DividendReceipt struct {
	DividendID string           `json:"dividend_id"`
	Pool       AccountID        `json:"pool"`
	TotalCents int64            `json:"total_cents"`
	Currency   string           `json:"currency"`
	RecordDate time.Time        `json:"record_date"`
	Payouts    []DividendPayout `json:"payouts"`
	Duplicate  bool             `json:"duplicate"`
}

// Validation errors for dividend declarations.
var (
	// ErrInvalidDividendShares is returned when the holders' shares do
	// not sum to exactly 10000 basis points: a dividend must distribute
	// exactly the declared total, no more and no less.
	ErrInvalidDividendShares = errors.New("ledger: dividend holder shares must sum to 10000 basis points")
	// ErrInvalidDividendHolderShares is returned when a holder's share
	// is outside 1..10000 basis points: a holder with no shares is not
	// a holder, and a single holder cannot exceed the whole.
	ErrInvalidDividendHolderShares = errors.New("ledger: dividend holder shares must be between 1 and 10000 basis points")
	// ErrNoDividendHolders is returned when a dividend is declared with
	// an empty holder table.
	ErrNoDividendHolders = errors.New("ledger: dividend declaration requires at least one holder")
	// ErrDividendAccountSeparator is returned when the pool or a holder
	// account ID contains '/': the deterministic dividend entry IDs
	// join their components with '/', so a '/' inside an account would
	// make two different declarations share one entry ID (see the
	// accrual convention in accrual.go).
	ErrDividendAccountSeparator = errors.New("ledger: dividend account IDs must not contain '/'")
)

// dividendEntryID builds the deterministic dividend declaration ID.
// The UTC record date makes one declaration idempotent while a new
// record date gets a fresh ID.
func dividendEntryID(pool AccountID, recordDate time.Time) string {
	return "dividend/" + string(pool) + "/" + recordDate.UTC().Format("2006-01-02")
}

// dividendLegEntryID builds the deterministic journal entry ID for one
// holder leg of a dividend declaration.
func dividendLegEntryID(dividendID string, index int) string {
	return dividendID + "/" + strconv.Itoa(index)
}

// dividendMemo encodes the holder's share in the journal row's memo, so
// the record-date shareholding snapshot is recoverable from the journal
// alone: "dividend:<sharesBps>bps".
func dividendMemo(sharesBps int64) string {
	return "dividend:" + strconv.FormatInt(sharesBps, 10) + "bps"
}

// parseDividendMemo recovers the share encoded by dividendMemo. It
// reports false when the memo does not carry a dividend share —
// defensive, since the dividend ID namespace is the only writer.
func parseDividendMemo(memo string) (int64, bool) {
	rest, ok := strings.CutPrefix(memo, "dividend:")
	if !ok {
		return 0, false
	}
	num, ok := strings.CutSuffix(rest, "bps")
	if !ok {
		return 0, false
	}
	bps, err := strconv.ParseInt(num, 10, 64)
	if err != nil {
		return 0, false
	}
	return bps, true
}

// checkDividendInputs validates a dividend declaration before anything
// is booked: pool and holder accounts, the share table, the total, and
// the currency. It mirrors the field-validation half of
// validateJournalEntry so the caller gets the error before any leg is
// attempted.
func checkDividendInputs(pool AccountID, totalCents int64, currency string, holders []DividendHolder) error {
	if pool == "" {
		return ErrEmptyDebitAccount
	}
	if strings.Contains(string(pool), "/") {
		return ErrDividendAccountSeparator
	}
	if totalCents <= 0 {
		return ErrNonPositiveAmount
	}
	if _, err := normalizeCurrency(currency); err != nil {
		return err
	}
	if len(holders) == 0 {
		return ErrNoDividendHolders
	}
	var sum int64
	for i, h := range holders {
		if h.Account == "" {
			return fmt.Errorf("ledger: dividend holder %d account must not be empty", i)
		}
		if strings.Contains(string(h.Account), "/") {
			return ErrDividendAccountSeparator
		}
		if h.SharesBps < 1 || h.SharesBps > 10000 {
			return ErrInvalidDividendHolderShares
		}
		var ok bool
		sum, ok = addCents(sum, h.SharesBps)
		if !ok {
			return ErrInvalidDividendShares
		}
	}
	if sum != 10000 {
		return ErrInvalidDividendShares
	}
	return nil
}

// dividendShareAmounts splits totalCents across the holders' basis-point
// shares: floor(total * bps / 10000) per holder with the dust remainder
// landing on the first holder. The sum of the returned amounts always
// equals totalCents exactly, so the declaration is always fully
// distributed. Overflow-safe: policyFeeCents never overflows for
// non-negative amounts and bps <= 10000, and the dust addition is
// checked.
func dividendShareAmounts(totalCents int64, holders []DividendHolder) ([]int64, error) {
	amounts := make([]int64, len(holders))
	var distributed int64
	for i, h := range holders {
		amounts[i] = policyFeeCents(totalCents, h.SharesBps)
		var ok bool
		distributed, ok = addCents(distributed, amounts[i])
		if !ok {
			return nil, errors.New("ledger: dividend share amounts overflow int64")
		}
	}
	dust := totalCents - distributed
	if dust != 0 {
		var ok bool
		amounts[0], ok = addCents(amounts[0], dust)
		if !ok || amounts[0] <= 0 {
			return nil, errors.New("ledger: dividend dust remainder overflows int64")
		}
	}
	return amounts, nil
}

// DeclareDividend books a dividend declaration: one double-entry row per
// holder (debit the retained-earnings pool, credit the holder) for the
// holder's basis-point share of totalCents. recordDate is both the
// entries' timestamp and the declaration's identity (a zero recordDate
// means now); currency defaults to the ledger's default currency when
// empty.
//
// The declaration is idempotent: re-running the same (pool,
// recordDate) returns the original receipt with Duplicate == true and
// books nothing new. A declaration whose leg fails partway is resumable:
// the retry skips the rows already journaled (their deterministic IDs
// are found) and books the remaining legs; the returned receipt carries
// the legs booked so far and the error names the failed leg. Each leg
// runs the full Post gate order but never fires journal rules and never
// enters dual-control review (see the package doc comment above).
func (l *Ledger) DeclareDividend(pool AccountID, totalCents int64, currency string, holders []DividendHolder, recordDate time.Time) (DividendReceipt, error) {
	if err := checkDividendInputs(pool, totalCents, currency, holders); err != nil {
		return DividendReceipt{}, err
	}
	cur, err := normalizeCurrency(currency)
	if err != nil {
		return DividendReceipt{}, err
	}
	if recordDate.IsZero() {
		recordDate = time.Now()
	}
	amounts, err := dividendShareAmounts(totalCents, holders)
	if err != nil {
		return DividendReceipt{}, err
	}
	dividendID := dividendEntryID(pool, recordDate)

	l.mu.Lock()
	defer l.mu.Unlock()
	receipt := DividendReceipt{
		DividendID: dividendID,
		Pool:       pool,
		TotalCents: totalCents,
		Currency:   cur,
		RecordDate: recordDate,
		Payouts:    make([]DividendPayout, 0, len(holders)),
		Duplicate:  true,
	}
	for i, h := range holders {
		legID := dividendLegEntryID(dividendID, i)
		if orig, ok := l.entries[legID]; ok {
			// Already journaled by an earlier (possibly partial) run:
			// recover the payout from the journaled row instead of
			// re-booking. The share rides in the memo (see
			// dividendMemo); amounts and accounts are journaled.
			bps, _ := parseDividendMemo(orig.Memo)
			receipt.Payouts = append(receipt.Payouts, DividendPayout{
				Account:     orig.CreditAccount,
				SharesBps:   bps,
				AmountCents: orig.AmountCents,
				EntryID:     orig.ID,
			})
			continue
		}
		e := JournalEntry{
			ID:             legID,
			DebitAccount:   pool,
			CreditAccount:  h.Account,
			AmountCents:    amounts[i],
			Currency:       cur,
			IdempotencyKey: legID,
			Memo:           dividendMemo(h.SharesBps),
			CreatedAt:      recordDate,
		}
		// fireRules=false: dividends are corporate actions, never
		// operator postings; journal rules and dual-control review do
		// not apply (see the package doc comment). Each leg still runs
		// the full Post gate order.
		posted, _, err := l.postLocked(e, false)
		if err != nil {
			receipt.Duplicate = false
			return receipt, fmt.Errorf("ledger: dividend %q leg %d (holder %q): %w", dividendID, i, h.Account, err)
		}
		receipt.Duplicate = false
		receipt.Payouts = append(receipt.Payouts, DividendPayout{
			Account:     posted.CreditAccount,
			SharesBps:   h.SharesBps,
			AmountCents: posted.AmountCents,
			EntryID:     posted.ID,
		})
	}
	return receipt, nil
}

// Dividends lists every declared dividend, sorted by dividend ID: the
// per-holder payouts recovered from the journaled rows. It is the
// read-side of the dividend ID namespace and backs the Reconcile
// disclosure. Callers must hold l.mu; the read lock suffices.
func (l *Ledger) dividendsLocked() []DividendReceipt {
	byDividend := make(map[string][]DividendPayout)
	meta := make(map[string]DividendReceipt)
	for id, e := range l.entries {
		if !strings.HasPrefix(id, "dividend/") {
			continue
		}
		// Leg IDs are "<dividendID>/<index>": the dividend ID is the
		// prefix up to the last '/'. Pool account IDs cannot contain
		// '/', and the record date is fixed-width, so the last '/'
		// always separates the holder index.
		slash := strings.LastIndex(id, "/")
		if slash < 0 {
			continue
		}
		dividendID := id[:slash]
		bps, ok := parseDividendMemo(e.Memo)
		if !ok {
			continue
		}
		byDividend[dividendID] = append(byDividend[dividendID], DividendPayout{
			Account:     e.CreditAccount,
			SharesBps:   bps,
			AmountCents: e.AmountCents,
			EntryID:     e.ID,
		})
		if _, seen := meta[dividendID]; !seen {
			meta[dividendID] = DividendReceipt{
				DividendID: dividendID,
				Pool:       e.DebitAccount,
				Currency:   e.Currency,
				RecordDate: e.CreatedAt,
			}
		}
	}
	out := make([]DividendReceipt, 0, len(byDividend))
	for dividendID, payouts := range byDividend {
		// Recover the declaration total and record date from the
		// journaled legs: the total is the sum of the legs (dust went
		// to the first holder, so the sum is exact), the record date
		// is the legs' shared timestamp.
		var total int64
		for _, p := range payouts {
			var ok bool
			total, ok = addCents(total, p.AmountCents)
			if !ok {
				total = 0
				break
			}
		}
		sort.Slice(payouts, func(i, j int) bool { return payouts[i].EntryID < payouts[j].EntryID })
		r := meta[dividendID]
		r.TotalCents = total
		r.Payouts = payouts
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DividendID < out[j].DividendID })
	return out
}

// Dividends lists every declared dividend, sorted by dividend ID.
func (l *Ledger) Dividends() []DividendReceipt {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.dividendsLocked()
}
