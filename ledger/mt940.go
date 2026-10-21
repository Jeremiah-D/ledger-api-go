package ledger

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// SWIFT MT940 customer statement message export (LG-159, fintech: 银企直连对账单格式).
//
// ExportMT940 renders one account's effective journal rows as a SWIFT
// MT940 customer statement message — the tag block the bank's statement
// downloader would produce: :20: (transaction reference), :25:
// (account identification), :28C: (statement/sequence number), :60F:
// (opening balance), one :61: + optional :86: per statement line,
// :62F: (closing balance), and :64: (available balance, equal to the
// closing balance — the ledger has no intraday credit-line concept, so
// the available balance is the honest closing balance).
//
// The export is read-only like ExportCAMT053 (LG-158) and GenerateStatement:
// the scan holds the read lock from start to finish, bumps no version,
// books nothing, and appends no audit-chain link. Pending-review rows
// never touched balances and are excluded, exactly like the other
// statement exports.
//
// Direction is account-centric, mirroring camt.053 and OFX: the account
// as the debit leg reads C (credit — money in), as the credit leg D
// (debit — money out). Amounts are absolute; the sign lives in the
// C/D mark. A self-transfer (both legs the account) reads as a credit,
// the same bias the import-side drafts use.
//
// Reconciliation is structural, not asserted after the fact: the closing
// balance is computed as opening + signed sum of the lines, and
// RenderMT940Statement re-derives it and fails loud if the two disagree
// (a bookkeeping-arithmetic bug must never ship a statement that does
// not foot). The per-line fail-loud cases are a missing value date, a
// missing amount, and a missing reference — a bank line without those
// is not a bank line.
//
// Lines are joined with "\n" and there is no trailing newline, the same
// convention as the NACHA builder: convert to CRLF for the bank if the
// uploader requires it. This module is Go-only (like NACHA and camt.053
// export): the operator builds the file and hands it to the bank's
// statement processor.

// ErrInvalidMT940Export is returned by ExportMT940 and
// RenderMT940Statement when the request cannot name a valid statement:
// an empty account or currency, an invalid currency code, an inverted
// time range, a statement line missing its value date, amount, or
// reference, or a statement whose closing balance does not reconcile
// with the opening balance and the lines.
var ErrInvalidMT940Export = errors.New("ledger: invalid MT940 export options")

// MT940ExportOptions tunes ExportMT940.
//
// Account is required. Currency is required: an MT940 statement is
// single-currency, and every balance tag names its currency. StatementID
// and SequenceID default to "1". Since/Until bound the exported lines
// (half-open [Since, Until); zero means unbounded). Lines are the
// account's effective journal rows in (CreatedAt, ID) order.
type MT940ExportOptions struct {
	Account     AccountID
	StatementID string
	SequenceID  string
	Since       time.Time
	Until       time.Time
	Currency    string
}

// MT940Balance is one balance tag (:60F:/:62F:/:64:): the C/D mark
// carries the sign, the amount is absolute, in cents.
type MT940Balance struct {
	Credit      bool
	ValueDate   time.Time
	Currency    string
	AmountCents int64
}

// MT940Line is one :61: statement line plus its optional :86:
// supplementary details.
type MT940Line struct {
	ValueDate    time.Time
	EntryDate    time.Time
	Credit       bool
	AmountCents  int64
	Reference    string // ≤16x after truncation
	Supplementary string // :86: free text; empty omits the tag
}

// MT940Statement is the export document model: header tags, the lines,
// and the reconciled closing balances.
type MT940Statement struct {
	TransactionReference  string
	AccountIdentification string
	StatementNumber       string
	SequenceNumber        string
	OpeningBalance        MT940Balance
	Lines                 []MT940Line
	ClosingBalance        MT940Balance
	AvailableBalance      MT940Balance
}

// mt940Amount renders integer cents as an MT940 amount: the absolute
// value with a comma decimal separator ("150000" -> "1500,00"). The
// sign lives in the C/D mark, never in the amount.
func mt940Amount(amountCents int64) string {
	if amountCents < 0 {
		amountCents = -amountCents
	}
	return fmt.Sprintf("%d,%02d", amountCents/100, amountCents%100)
}

// mt940DC renders the debit/credit mark: "C" for a credit, "D" for a
// debit. The mark is the only place the sign lives.
func mt940DC(credit bool) string {
	if credit {
		return "C"
	}
	return "D"
}

// mt940ValueDate renders the 6!n value date (YYMMDD, UTC).
func mt940ValueDate(t time.Time) string {
	return t.UTC().Format("060102")
}

// mt940EntryDate renders the 4!n entry date (MMDD, UTC).
func mt940EntryDate(t time.Time) string {
	return t.UTC().Format("0102")
}

// mt940Truncate limits s to max runes, keeping the result rune-safe.
// MT940 fields are max-width, not exact-width: truncation is the
// documented norm (the bank reference in :61: is 16x).
func mt940Truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}

// mt940BalanceLine renders one balance tag (:60F:/:62F:/:64:).
func mt940BalanceLine(tag string, b MT940Balance) (string, error) {
	if b.ValueDate.IsZero() {
		return "", fmt.Errorf("%w: balance %s missing value date", ErrInvalidMT940Export, tag)
	}
	currency, err := normalizeCurrency(b.Currency)
	if err != nil {
		return "", fmt.Errorf("%w: balance %s: %v", ErrInvalidMT940Export, tag, err)
	}
	if b.AmountCents < 0 {
		return "", fmt.Errorf("%w: balance %s: negative amount", ErrInvalidMT940Export, tag)
	}
	return fmt.Sprintf(":%s:%s%s%s%s", tag, mt940DC(b.Credit),
		mt940ValueDate(b.ValueDate), currency, mt940Amount(b.AmountCents)), nil
}

// mt940LineOf renders one :61: statement line plus its optional :86:.
func mt940LineOf(l MT940Line) ([]string, error) {
	if l.ValueDate.IsZero() {
		return nil, fmt.Errorf("%w: statement line missing value date", ErrInvalidMT940Export)
	}
	if l.EntryDate.IsZero() {
		l.EntryDate = l.ValueDate
	}
	if l.AmountCents <= 0 {
		return nil, fmt.Errorf("%w: statement line missing amount", ErrInvalidMT940Export)
	}
	if strings.TrimSpace(l.Reference) == "" {
		return nil, fmt.Errorf("%w: statement line missing reference", ErrInvalidMT940Export)
	}
	line := fmt.Sprintf(":61:%s%s%s%sNTRF//%s",
		mt940ValueDate(l.ValueDate), mt940EntryDate(l.EntryDate),
		mt940DC(l.Credit), mt940Amount(l.AmountCents),
		mt940Truncate(strings.TrimSpace(l.Reference), 16))
	out := []string{line}
	if strings.TrimSpace(l.Supplementary) != "" {
		out = append(out, ":86:"+mt940Truncate(strings.TrimSpace(l.Supplementary), 65))
	}
	return out, nil
}

// RenderMT940Statement renders s as an MT940 customer statement
// message: :20:, :25:, :28C:, :60F:, one :61: (+ optional :86:) per
// line, :62F:, :64:. Lines join with "\n"; there is no trailing
// newline.
//
// The reconciliation invariant is re-derived here and fails loud: the
// closing balance must equal opening + signed sum of the lines. A
// statement that does not foot is never rendered.
func RenderMT940Statement(s MT940Statement) (string, error) {
	if strings.TrimSpace(s.AccountIdentification) == "" {
		return "", fmt.Errorf("%w: account identification required", ErrInvalidMT940Export)
	}
	if strings.TrimSpace(s.StatementNumber) == "" {
		return "", fmt.Errorf("%w: statement number required", ErrInvalidMT940Export)
	}
	if strings.TrimSpace(s.SequenceNumber) == "" {
		return "", fmt.Errorf("%w: sequence number required", ErrInvalidMT940Export)
	}

	// Reconciliation: closing == opening + signed lines. The amounts
	// are absolute in the model; the C/D marks carry the signs.
	signed := func(credit bool, cents int64) int64 {
		if credit {
			return cents
		}
		return -cents
	}
	expected := signed(s.OpeningBalance.Credit, s.OpeningBalance.AmountCents)
	for i, l := range s.Lines {
		expected += signed(l.Credit, l.AmountCents)
		if expected > 1<<62 || expected < -(1<<62) {
			return "", fmt.Errorf("%w: line %d overflows reconciliation", ErrInvalidMT940Export, i+1)
		}
	}
	got := signed(s.ClosingBalance.Credit, s.ClosingBalance.AmountCents)
	if expected != got {
		return "", fmt.Errorf("%w: closing balance %d does not reconcile with opening %d and %d lines",
			ErrInvalidMT940Export, got, signed(s.OpeningBalance.Credit, s.OpeningBalance.AmountCents), len(s.Lines))
	}
	// Available must track the closing balance: the ledger has no
	// intraday credit-line concept, so :64: repeats :62F: exactly.
	if signed(s.AvailableBalance.Credit, s.AvailableBalance.AmountCents) != got {
		return "", fmt.Errorf("%w: available balance does not match closing balance", ErrInvalidMT940Export)
	}

	opening, err := mt940BalanceLine("60F", s.OpeningBalance)
	if err != nil {
		return "", err
	}
	closing, err := mt940BalanceLine("62F", s.ClosingBalance)
	if err != nil {
		return "", err
	}
	available, err := mt940BalanceLine("64", s.AvailableBalance)
	if err != nil {
		return "", err
	}

	lines := []string{
		":20:" + mt940Truncate(strings.TrimSpace(s.TransactionReference), 16),
		":25:" + mt940Truncate(strings.TrimSpace(s.AccountIdentification), 35),
		":28C:" + strings.TrimSpace(s.StatementNumber) + "/" + strings.TrimSpace(s.SequenceNumber),
		opening,
	}
	for _, l := range s.Lines {
		rendered, err := mt940LineOf(l)
		if err != nil {
			return "", err
		}
		lines = append(lines, rendered...)
	}
	lines = append(lines, closing, available)
	return strings.Join(lines, "\n"), nil
}

// mt940LineRef picks the statement-line reference: the entry's
// idempotency key when it has one, else the journal entry ID. Both are
// stable across exports, so the bank can dedupe on them. The :61:
// reference field is 16x; longer values truncate (documented above).
func mt940LineRef(e JournalEntry) string {
	if e.IdempotencyKey != "" {
		return e.IdempotencyKey
	}
	return e.ID
}

// ExportMT940 renders an MT940 customer statement for one account from
// the journal. The account named in :25: is the external identifier
// mapped under the "mt940" system when one exists (see
// ExternalSystemMT940 — usually the IBAN the bank expects), else the
// internal account ID.
//
// The opening balance is folded from every effective journal row with
// CreatedAt < Since in the statement currency — not just the exported
// page — so the statement foots with the ledger's true state at the
// window start. Pending-review rows never touched balances and are
// excluded, like every other statement export.
//
// Read-only: the scan holds the read lock from start to finish. It
// bumps no version, books nothing, and appends no audit-chain link.
func (l *Ledger) ExportMT940(opts MT940ExportOptions) (string, error) {
	if opts.Account == "" {
		return "", ErrEmptyAccountID
	}
	currency, err := normalizeCurrency(opts.Currency)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidMT940Export, err)
	}
	if !opts.Since.IsZero() && !opts.Until.IsZero() && opts.Since.After(opts.Until) {
		return "", fmt.Errorf("%w: since is after until", ErrInvalidMT940Export)
	}

	stmtID := strings.TrimSpace(opts.StatementID)
	if stmtID == "" {
		stmtID = "1"
	}
	seqID := strings.TrimSpace(opts.SequenceID)
	if seqID == "" {
		seqID = "1"
	}

	l.mu.RLock()
	defer l.mu.RUnlock()

	var lines []MT940Line
	var openingCents int64
	for _, id := range l.byAccount[opts.Account] {
		e := l.entries[id]
		if e.PendingReview {
			// Same rule as ExportCAMT053 and ExportOFX: a
			// pending-review row never touched balances, so the
			// statement excludes it. Re-export after approval.
			continue
		}
		if e.Currency != currency {
			continue
		}
		credit := e.DebitAccount == opts.Account
		signed := e.AmountCents
		if !credit {
			signed = -signed
		}
		if !opts.Since.IsZero() && e.CreatedAt.Before(opts.Since) {
			// Folds into the opening balance. Saturating add: an
			// adversarial amount history can never wrap the fold
			// into a false balance.
			openingCents = satAddInt64(openingCents, signed)
			continue
		}
		if !opts.Until.IsZero() && !e.CreatedAt.Before(opts.Until) {
			continue
		}
		lines = append(lines, MT940Line{
			ValueDate:     e.CreatedAt.UTC(),
			EntryDate:     e.CreatedAt.UTC(),
			Credit:        credit,
			AmountCents:   e.AmountCents,
			Reference:     mt940LineRef(e),
			Supplementary: e.Memo,
		})
	}
	sort.Slice(lines, func(i, j int) bool {
		if lines[i].ValueDate.Equal(lines[j].ValueDate) {
			return lines[i].Reference < lines[j].Reference
		}
		return lines[i].ValueDate.Before(lines[j].ValueDate)
	})

	// Reconciliation: closing = opening + signed lines, by
	// construction; RenderMT940Statement re-derives it and fails loud
	// if the two disagree.
	closingCents := openingCents
	for _, ln := range lines {
		if ln.Credit {
			closingCents = satAddInt64(closingCents, ln.AmountCents)
		} else {
			closingCents = satAddInt64(closingCents, -ln.AmountCents)
		}
	}
	balanceOf := func(cents int64, date time.Time) MT940Balance {
		credit := cents >= 0
		if !credit {
			cents = -cents
		}
		return MT940Balance{Credit: credit, ValueDate: date, Currency: currency, AmountCents: cents}
	}
	openingDate := opts.Since.UTC()
	if openingDate.IsZero() {
		openingDate = time.Now().UTC()
		if len(lines) > 0 {
			openingDate = lines[0].ValueDate
		}
	}
	closingDate := opts.Until.UTC()
	if closingDate.IsZero() {
		closingDate = time.Now().UTC()
	}

	acctID := string(opts.Account)
	if ext, ok := l.externalAccountIDLocked(opts.Account, ExternalSystemMT940); ok {
		acctID = ext
	}

	return RenderMT940Statement(MT940Statement{
		TransactionReference:  stmtID,
		AccountIdentification: acctID,
		StatementNumber:       stmtID,
		SequenceNumber:        seqID,
		OpeningBalance:        balanceOf(openingCents, openingDate),
		Lines:                 lines,
		ClosingBalance:        balanceOf(closingCents, closingDate),
		AvailableBalance:      balanceOf(closingCents, closingDate),
	})
}
