package ledger

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// SWIFT MT950 interbank statement message export (LG-385, fintech:
// 银行间对账单格式).
//
// ExportMT950 renders one account's effective journal rows as a SWIFT
// MT950 interbank statement message — the tag block a correspondent
// bank's statement downloader would produce: :20: (transaction
// reference), :25: (account identification), :28C:
// (statement/sequence number), :60F: (opening balance), one :61: +
// optional :86: per statement line, :62F: (closing balance), :64:
// (available balance), :65: (forward available balance), and a
// trailing :86: (information to account owner) when the statement
// carries owner info.
//
// MT950 is MT940's interbank counterpart: the same accounting fact in
// a different message role — MT940 goes to the customer, MT950 goes to
// a correspondent bank. The block therefore mirrors ExportMT940
// (LG-379) with two deliberate differences: there is no :21: related
// reference (MT950 has no such field), and the balances carry both
// :64: and :65: (the ledger has no intraday credit-line or forward
// concept, so both honestly repeat the closing balance).
//
// The :60a:/:62a: tags take the F (final) variant: this export is a
// statement for a closed window, not an interim (:60M:/:62M:) report.
//
// The export is read-only like ExportMT940 and ExportCAMT053: the scan
// holds the read lock from start to finish, bumps no version, books
// nothing, and appends no audit-chain link. Pending-review rows never
// touched balances and are excluded, exactly like the other statement
// exports.
//
// Direction is account-centric, mirroring MT940 and camt.053: the
// account as the debit leg reads C (credit — money in), as the credit
// leg D (debit — money out). Amounts are absolute; the sign lives in
// the C/D mark. A self-transfer (both legs the account) reads as a
// credit, the same bias the import-side drafts use.
//
// Reconciliation is structural, not asserted after the fact: the closing
// balance is computed as opening + signed sum of the lines, and
// RenderMT950Statement re-derives it and fails loud if the two disagree
// (a bookkeeping-arithmetic bug must never ship a statement that does
// not foot). The per-line fail-loud cases are a missing value date, a
// missing amount, and a missing reference — a bank line without those
// is not a bank line.
//
// Lines are joined with "\n" and there is no trailing newline, the same
// convention as the MT940 builder: convert to CRLF for the bank if the
// uploader requires it. This module is Go-only (like MT940 and
// camt.053 export): the operator builds the file and hands it to the
// bank's statement processor.

// ErrInvalidMT950Export is returned by ExportMT950 and
// RenderMT950Statement when the request cannot name a valid statement:
// an invalid currency code, an inverted time range, a statement line
// missing its value date, amount, or reference, or a statement whose
// closing balance does not reconcile with the opening balance and the
// lines. An empty account is ErrEmptyAccountID, not this error.
var ErrInvalidMT950Export = errors.New("ledger: invalid MT950 export options")

// MT950ExportOptions tunes ExportMT950.
//
// Account is required. Currency is required: an MT950 statement is
// single-currency, and every balance tag names its currency. StatementID
// and SequenceID default to "1". Since/Until bound the exported lines
// (half-open [Since, Until); zero means unbounded). Lines are the
// account's effective journal rows in (CreatedAt, ID) order.
type MT950ExportOptions struct {
	Account     AccountID
	StatementID string
	SequenceID  string
	Since       time.Time
	Until       time.Time
	Currency    string
}

// MT950Balance is one balance tag (:60F:/:62F:/:64:/:65:): the C/D
// mark carries the sign, the amount is absolute, in cents.
type MT950Balance struct {
	Credit      bool
	ValueDate   time.Time
	Currency    string
	AmountCents int64
}

// MT950Line is one :61: statement line plus its optional :86:
// supplementary details.
type MT950Line struct {
	ValueDate     time.Time
	EntryDate     time.Time
	Credit        bool
	AmountCents   int64
	Reference     string // ≤16x after truncation
	Supplementary string // :86: free text; empty omits the tag
}

// MT950Statement is the export document model: header tags, the lines,
// the reconciled closing balances, and the optional trailing :86:
// information to account owner.
type MT950Statement struct {
	TransactionReference  string
	AccountIdentification string
	StatementNumber       string
	SequenceNumber        string
	OpeningBalance        MT950Balance
	Lines                 []MT950Line
	ClosingBalance        MT950Balance
	AvailableBalance      MT950Balance
	ForwardBalance        MT950Balance
	OwnerInfo             string // trailing :86: free text; empty omits the tag
}

// mt950BalanceLine renders one balance tag (:60F:/:62F:/:64:/:65:).
// The formatting is the MT940 one: the C/D mark carries the sign, the
// amount is absolute with a comma decimal separator, the date is 6!n
// (YYMMDD, UTC).
func mt950BalanceLine(tag string, b MT950Balance) (string, error) {
	if b.ValueDate.IsZero() {
		return "", fmt.Errorf("%w: balance %s missing value date", ErrInvalidMT950Export, tag)
	}
	currency, err := normalizeCurrency(b.Currency)
	if err != nil {
		return "", fmt.Errorf("%w: balance %s: %v", ErrInvalidMT950Export, tag, err)
	}
	if b.AmountCents < 0 {
		return "", fmt.Errorf("%w: balance %s: negative amount", ErrInvalidMT950Export, tag)
	}
	return fmt.Sprintf(":%s:%s%s%s%s", tag, mt940DC(b.Credit),
		mt940ValueDate(b.ValueDate), currency, mt940Amount(b.AmountCents)), nil
}

// mt950LineOf renders one :61: statement line plus its optional :86:.
func mt950LineOf(l MT950Line) ([]string, error) {
	if l.ValueDate.IsZero() {
		return nil, fmt.Errorf("%w: statement line missing value date", ErrInvalidMT950Export)
	}
	if l.EntryDate.IsZero() {
		l.EntryDate = l.ValueDate
	}
	if l.AmountCents <= 0 {
		return nil, fmt.Errorf("%w: statement line missing amount", ErrInvalidMT950Export)
	}
	if strings.TrimSpace(l.Reference) == "" {
		return nil, fmt.Errorf("%w: statement line missing reference", ErrInvalidMT950Export)
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

// RenderMT950Statement renders s as an MT950 interbank statement
// message: :20:, :25:, :28C:, :60F:, one :61: (+ optional :86:) per
// line, :62F:, :64:, :65:, and a trailing :86: when the statement
// carries owner info. There is deliberately no :21: tag — MT950 has
// no related-reference field. Lines join with "\n"; there is no
// trailing newline.
//
// The reconciliation invariant is re-derived here and fails loud: the
// closing balance must equal opening + signed sum of the lines, and
// both :64: and :65: must repeat the closing balance. A statement
// that does not foot is never rendered.
func RenderMT950Statement(s MT950Statement) (string, error) {
	if strings.TrimSpace(s.AccountIdentification) == "" {
		return "", fmt.Errorf("%w: account identification required", ErrInvalidMT950Export)
	}
	if strings.TrimSpace(s.StatementNumber) == "" {
		return "", fmt.Errorf("%w: statement number required", ErrInvalidMT950Export)
	}
	if strings.TrimSpace(s.SequenceNumber) == "" {
		return "", fmt.Errorf("%w: sequence number required", ErrInvalidMT950Export)
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
			return "", fmt.Errorf("%w: line %d overflows reconciliation", ErrInvalidMT950Export, i+1)
		}
	}
	got := signed(s.ClosingBalance.Credit, s.ClosingBalance.AmountCents)
	if expected != got {
		return "", fmt.Errorf("%w: closing balance %d does not reconcile with opening %d and %d lines",
			ErrInvalidMT950Export, got, signed(s.OpeningBalance.Credit, s.OpeningBalance.AmountCents), len(s.Lines))
	}
	// Available and forward balances must track the closing balance:
	// the ledger has no intraday credit-line or forward concept, so
	// :64: and :65: repeat :62F: exactly.
	if signed(s.AvailableBalance.Credit, s.AvailableBalance.AmountCents) != got {
		return "", fmt.Errorf("%w: available balance does not match closing balance", ErrInvalidMT950Export)
	}
	if signed(s.ForwardBalance.Credit, s.ForwardBalance.AmountCents) != got {
		return "", fmt.Errorf("%w: forward available balance does not match closing balance", ErrInvalidMT950Export)
	}

	opening, err := mt950BalanceLine("60F", s.OpeningBalance)
	if err != nil {
		return "", err
	}
	closing, err := mt950BalanceLine("62F", s.ClosingBalance)
	if err != nil {
		return "", err
	}
	available, err := mt950BalanceLine("64", s.AvailableBalance)
	if err != nil {
		return "", err
	}
	forward, err := mt950BalanceLine("65", s.ForwardBalance)
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
		rendered, err := mt950LineOf(l)
		if err != nil {
			return "", err
		}
		lines = append(lines, rendered...)
	}
	lines = append(lines, closing, available, forward)
	if strings.TrimSpace(s.OwnerInfo) != "" {
		lines = append(lines, ":86:"+mt940Truncate(strings.TrimSpace(s.OwnerInfo), 65))
	}
	return strings.Join(lines, "\n"), nil
}

// ExportMT950 renders an MT950 interbank statement for one account
// from the journal. The account named in :25: is the external
// identifier mapped under the "mt950" system when one exists (see
// ExternalSystemMT950 — usually the account number the correspondent
// bank expects), else the internal account ID. A mapping declared
// under the "mt940" system does not apply here: the two messages go
// to different recipients and may name the account differently.
//
// The opening balance is folded from every effective journal row with
// CreatedAt < Since in the statement currency — not just the exported
// page — so the statement foots with the ledger's true state at the
// window start. Pending-review rows never touched balances and are
// excluded, like every other statement export.
//
// The trailing :86: is omitted: the ledger holds no account-owner
// record, so there is no honest owner info to print — the field is
// left to RenderMT950Statement callers that do hold it, never
// fabricated here.
//
// Read-only: the scan holds the read lock from start to finish. It
// bumps no version, books nothing, and appends no audit-chain link.
func (l *Ledger) ExportMT950(opts MT950ExportOptions) (string, error) {
	if opts.Account == "" {
		return "", ErrEmptyAccountID
	}
	currency, err := normalizeCurrency(opts.Currency)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidMT950Export, err)
	}
	if !opts.Since.IsZero() && !opts.Until.IsZero() && opts.Since.After(opts.Until) {
		return "", fmt.Errorf("%w: since is after until", ErrInvalidMT950Export)
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

	var lines []MT950Line
	var openingCents int64
	for _, id := range l.byAccount[opts.Account] {
		e := l.entries[id]
		if e.PendingReview {
			// Same rule as ExportMT940 and ExportCAMT053: a
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
		lines = append(lines, MT950Line{
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
	// construction; RenderMT950Statement re-derives it and fails loud
	// if the two disagree.
	closingCents := openingCents
	for _, ln := range lines {
		if ln.Credit {
			closingCents = satAddInt64(closingCents, ln.AmountCents)
		} else {
			closingCents = satAddInt64(closingCents, -ln.AmountCents)
		}
	}
	balanceOf := func(cents int64, date time.Time) MT950Balance {
		credit := cents >= 0
		if !credit {
			cents = -cents
		}
		return MT950Balance{Credit: credit, ValueDate: date, Currency: currency, AmountCents: cents}
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
	if ext, ok := l.externalAccountIDLocked(opts.Account, ExternalSystemMT950); ok {
		acctID = ext
	}

	return RenderMT950Statement(MT950Statement{
		TransactionReference:  stmtID,
		AccountIdentification: acctID,
		StatementNumber:       stmtID,
		SequenceNumber:        seqID,
		OpeningBalance:        balanceOf(openingCents, openingDate),
		Lines:                 lines,
		ClosingBalance:        balanceOf(closingCents, closingDate),
		AvailableBalance:      balanceOf(closingCents, closingDate),
		ForwardBalance:        balanceOf(closingCents, closingDate),
	})
}
