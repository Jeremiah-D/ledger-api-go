package ledger

import "errors"

// DefaultCurrency is the ledger's home currency: entries and transfers
// that do not name a currency are booked in USD. The default keeps the
// API backward compatible — every entry posted before multi-currency
// support existed behaves exactly as before.
const DefaultCurrency = "USD"

// Validation errors for currency handling.
var (
	// ErrInvalidCurrency is returned when an entry or transfer names a
	// currency that is not a three-letter uppercase ISO 4217 code
	// (e.g. "usd", "US", "USDD", "U$D"). It is a 400-class field
	// validation error: the request is malformed, not semantically
	// rejected.
	ErrInvalidCurrency = errors.New("ledger: currency must be a 3-letter uppercase ISO 4217 code")
	// ErrCrossCurrencyTransfer is returned by PostTransfer when a
	// transfer's legs would be booked in different currencies. This
	// ledger performs no FX conversion: a transfer is always
	// single-currency, so a cross-currency transfer is semantically
	// unprocessable (HTTP 422), not malformed.
	ErrCrossCurrencyTransfer = errors.New("ledger: cross-currency transfers are not supported; all legs of a transfer must share one currency")
)

// accountCurrency is the composite key for per-currency bookkeeping.
// Balances, debit totals, and credit totals are tracked per
// (account, currency) pair, so one account can hold USD, EUR, and CNY
// side by side without the currencies ever mixing: the accounting
// equation is verified per currency, never across them (summing cents
// across currencies would be meaningless).
type accountCurrency struct {
	account  AccountID
	currency string
}

// normalizeCurrency validates a caller-supplied currency code and returns
// its canonical form. An empty code means "the default currency" and is
// normalized to DefaultCurrency, so the journal always stores an explicit
// code — including on the audit-chain hash, which covers it like every
// other journaled field. A non-empty code must be exactly three ASCII
// uppercase letters (ISO 4217 alpha-3, e.g. "USD", "EUR", "CNY").
func normalizeCurrency(code string) (string, error) {
	if code == "" {
		return DefaultCurrency, nil
	}
	if len(code) != 3 {
		return "", ErrInvalidCurrency
	}
	for i := 0; i < 3; i++ {
		if c := code[i]; c < 'A' || c > 'Z' {
			return "", ErrInvalidCurrency
		}
	}
	return code, nil
}

// CurrencyTrialBalance is the double-entry breakdown of one account in one
// currency: every cent ever debited to it in that currency, every cent
// ever credited from it in that currency, and the resulting net balance.
// NetBalance always equals TotalDebits - TotalCredits within the row.
type CurrencyTrialBalance struct {
	Currency     string `json:"currency"`
	TotalDebits  int64  `json:"total_debits_cents"`
	TotalCredits int64  `json:"total_credits_cents"`
	NetBalance   int64  `json:"net_balance_cents"`
}

// CurrencyTotals is the per-currency rollup of the whole ledger's debit
// and credit totals: the accounting equation (total debits == total
// credits) holds independently inside each row.
type CurrencyTotals struct {
	Currency     string `json:"currency"`
	TotalDebits  int64  `json:"total_debits_cents"`
	TotalCredits int64  `json:"total_credits_cents"`
}
