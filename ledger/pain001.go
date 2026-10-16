package ledger

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Validation and export errors for the ISO 20022 pain.001 payment
// instruction export (see ExportPain001) and the account IBAN registry
// (see SetAccountIBAN).
var (
	// ErrInvalidIBAN is returned by SetAccountIBAN when the IBAN fails
	// ISO 13616 validation: wrong length, non-letter country code,
	// non-alphanumeric characters, or a failing mod-97 check digit.
	ErrInvalidIBAN = errors.New("ledger: invalid IBAN")
	// ErrInvalidBIC is returned by SetAccountIBAN when the BIC is
	// neither empty nor a valid 8/11-character ISO 9362 code.
	ErrInvalidBIC = errors.New("ledger: invalid BIC: want empty or 8/11 alphanumeric characters")
	// ErrMissingAccountIBAN is returned by ExportPain001 when a payment's
	// debtor or creditor has no registered IBAN. The export is
	// fail-closed: a payment instruction without a real debtor/creditor
	// account identifier is refused rather than emitted with a forged
	// or empty IBAN.
	ErrMissingAccountIBAN = errors.New("ledger: pain.001 export refused: account has no registered IBAN")
	// ErrInvalidPain001Range is returned by ExportPain001 when the
	// requested time range is empty: since is after until. Both bounds
	// are optional (zero means unbounded); only a strictly inverted
	// range is an error. Mirrors ErrInvalidOFXRange.
	ErrInvalidPain001Range = errors.New("ledger: pain.001 export range is invalid: since is after until")
	// ErrPain001AmountOverflow is returned by ExportPain001 when the
	// control sum of the exported payments overflows int64: no real
	// control total could be computed, so the export is refused.
	ErrPain001AmountOverflow = errors.New("ledger: pain.001 control sum overflows int64")
)

// AccountIBAN is one opt-in IBAN registration (see SetAccountIBAN): the
// account identifier a pain.001 payment instruction names for the
// debtor or creditor, plus the optional BIC of the account's bank.
type AccountIBAN struct {
	Account AccountID `json:"account"`
	IBAN    string    `json:"iban"`
	BIC     string    `json:"bic,omitempty"`
}

// normalizeIBAN strips spaces and uppercases the input, the canonical
// form the registry stores. IBANs are case-insensitive and commonly
// written in groups of four characters.
func normalizeIBAN(raw string) string {
	return strings.ToUpper(strings.ReplaceAll(raw, " ", ""))
}

// validIBAN reports whether s is a well-formed ISO 13616 IBAN: 15 to 34
// characters, a two-letter country code, two check digits, and a mod-97
// remainder of 1 over the rearranged digit string. The caller passes the
// already-normalized form (see normalizeIBAN).
func validIBAN(s string) bool {
	if len(s) < 15 || len(s) > 34 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if i < 2 {
			// Country code: ASCII letters only.
			if c < 'A' || c > 'Z' {
				return false
			}
		} else if i < 4 {
			// Check digits.
			if c < '0' || c > '9' {
				return false
			}
		} else if (c < '0' || c > '9') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	// mod-97 over the rearranged string (first four characters moved to
	// the end), letters expanded A=10 .. Z=35, computed digit by digit
	// so no bignum is needed.
	rearranged := s[4:] + s[:4]
	rem := 0
	for i := 0; i < len(rearranged); i++ {
		c := rearranged[i]
		if c >= 'A' && c <= 'Z' {
			v := int(c-'A') + 10
			rem = (rem*10 + v/10) % 97
			rem = (rem*10 + v%10) % 97
		} else {
			rem = (rem*10 + int(c-'0')) % 97
		}
	}
	return rem == 1
}

// validBIC reports whether s is a valid ISO 9362 BIC: empty (not
// provided) or 8/11 alphanumeric characters with a 4-letter bank code
// and 2-letter country code.
func validBIC(s string) bool {
	if s == "" {
		return true
	}
	if len(s) != 8 && len(s) != 11 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		isLetter := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
		isDigit := c >= '0' && c <= '9'
		if !isLetter && !isDigit {
			return false
		}
		// Bank code (4) and country code (2) are letters.
		if i < 6 && !isLetter {
			return false
		}
	}
	return true
}

// SetAccountIBAN registers (or replaces) the opt-in IBAN for account,
// the identifier pain.001 payment instructions name for the debtor or
// creditor. The IBAN is validated to ISO 13616 (mod-97, length,
// character set); the BIC is optional but, when given, must be a valid
// ISO 9362 code (8 or 11 alphanumeric characters). The IBAN is stored
// in canonical form (uppercased, spaces stripped).
//
// The registry is structural config, like the account tags: it does not
// bump the ledger version, and it round-trips through snapshots, because
// a restore that dropped it would turn previously exportable payments
// into fail-closed refusals with no explanation.
func (l *Ledger) SetAccountIBAN(account AccountID, iban, bic string) error {
	if account == "" {
		return ErrEmptyAccountID
	}
	norm := normalizeIBAN(iban)
	if !validIBAN(norm) {
		return fmt.Errorf("%w: %q", ErrInvalidIBAN, iban)
	}
	if !validBIC(bic) {
		return fmt.Errorf("%w: %q", ErrInvalidBIC, bic)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.accountIBANs[account] = AccountIBAN{Account: account, IBAN: norm, BIC: strings.ToUpper(bic)}
	return nil
}

// AccountIBAN returns the registered IBAN for account, or false when
// none is registered.
func (l *Ledger) AccountIBAN(account AccountID) (AccountIBAN, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	reg, ok := l.accountIBANs[account]
	return reg, ok
}

// accountIBANsLocked returns the IBAN registrations sorted by account,
// the snapshot record form of the registry. Callers must hold l.mu
// (either lock suffices; export holds the read lock).
func (l *Ledger) accountIBANsLocked() []AccountIBAN {
	out := make([]AccountIBAN, 0, len(l.accountIBANs))
	for _, reg := range l.accountIBANs {
		out = append(out, reg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Account < out[j].Account })
	return out
}

// PainTransfer is one same-currency transfer principal registered for
// ISO 20022 pain.001 export: the payer (From, the pain.001 debtor) and
// the payee (To, the pain.001 creditor), the amount, and the booking
// time. Registration happens when the transfer commits — the plain
// PostTransfer path and same-currency split chunks; cross-currency
// transfers settle through the FX clearing account as two legs, not one
// direct debtor-to-creditor payment, so they are not representable as a
// single-currency pain.001 instruction and are excluded (documented,
// not silent).
type PainTransfer struct {
	TransferID  string    `json:"transfer_id"`
	From        AccountID `json:"from"`
	To          AccountID `json:"to"`
	AmountCents int64     `json:"amount_cents"`
	Currency    string    `json:"currency"`
	CreatedAt   time.Time `json:"created_at"`
}

// registerPainTransferLocked records a transfer principal for pain.001
// export. Callers must hold the write lock and must call it only after
// every validation gate passed, next to the commit — a transfer that
// fails any gate records nothing, registry included.
func (l *Ledger) registerPainTransferLocked(pt PainTransfer) {
	l.painTransfers[pt.TransferID] = pt
}

// painTransfersLocked returns the registered transfers sorted by
// (CreatedAt, TransferID), the deterministic export order. Callers must
// hold l.mu (either lock suffices).
func (l *Ledger) painTransfersLocked() []PainTransfer {
	out := make([]PainTransfer, 0, len(l.painTransfers))
	for _, pt := range l.painTransfers {
		out = append(out, pt)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].TransferID < out[j].TransferID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

// Pain001ExportOptions tunes ExportPain001.
//
// Debtor and Creditor optionally filter by the transfer's payer and
// payee (empty means unfiltered). Currency selects one currency for the
// export: empty exports every currency as separate PmtInf blocks, one
// per (debtor, currency). Since and Until bound the exported payments
// (half-open [Since, Until), the OFX export convention; zero means
// unbounded). Pending-review transfers are excluded, like the OFX
// export: their rows never touched balances, so a payment instruction
// must not be generated from them.
type Pain001ExportOptions struct {
	Debtor   AccountID
	Creditor AccountID
	Currency string
	Since    time.Time
	Until    time.Time
}

// pain001Payment is one exported credit transfer: the registry record
// plus the resolved debtor/creditor IBANs.
type pain001Payment struct {
	PainTransfer
	DebtorIBAN   AccountIBAN
	CreditorIBAN AccountIBAN
}

// ExportPain001 renders an ISO 20022 pain.001.001.03 customer credit
// transfer initiation from the registered transfer principals (see
// PainTransfer).
//
// Semantics, made explicit so downstream banks and importers can trust
// them:
//   - One CdtTrfTxInf per transfer; EndToEndId is the transfer ID —
//     stable across exports, so importers can dedupe on it.
//   - The debtor is the transfer's payer (From), the creditor the payee
//     (To); their account identifiers are the registered IBANs, the
//     BICs ride DbtrAgt/CdtrAgt when registered.
//   - Amounts are the ledger's integer cents rendered as exact decimals
//     (two fraction digits, no floats anywhere).
//   - Payments are grouped into one PmtInf per (debtor, currency), each
//     with its own NbOfTxs and CtrlSum; GrpHdr carries the totals.
//   - MsgId is deterministic in (window, filters, exported content): the
//     same export re-renders byte-identical, and any content change
//     changes the ID.
//   - Pending-review transfers are excluded: a payment instruction must
//     never be generated from a frozen transfer. Re-export after
//     approval includes it.
//   - Fail-closed on missing IBANs: a payment whose debtor or creditor
//     has no registered IBAN refuses the whole export with
//     ErrMissingAccountIBAN rather than emitting a forged identifier.
//
// The export is read-only: it takes the read lock, bumps no version,
// appends no chain link, and consumes no idempotency key. Like
// Reconcile, it still emits one structured audit event
// ("pain001_exported", details msg_id/tx_count/ctrl_sum_cents/currency)
// so the compliance trail records every generated instruction.
func (l *Ledger) ExportPain001(opts Pain001ExportOptions) ([]byte, error) {
	currency := ""
	if opts.Currency != "" {
		var cerr error
		currency, cerr = normalizeCurrency(opts.Currency)
		if cerr != nil {
			return nil, cerr
		}
	}
	if !opts.Since.IsZero() && !opts.Until.IsZero() && opts.Since.After(opts.Until) {
		return nil, ErrInvalidPain001Range
	}

	l.mu.RLock()

	var payments []pain001Payment
	for _, pt := range l.painTransfersLocked() {
		if opts.Debtor != "" && pt.From != opts.Debtor {
			continue
		}
		if opts.Creditor != "" && pt.To != opts.Creditor {
			continue
		}
		if currency != "" && pt.Currency != currency {
			continue
		}
		if !opts.Since.IsZero() && pt.CreatedAt.Before(opts.Since) {
			continue
		}
		if !opts.Until.IsZero() && !pt.CreatedAt.Before(opts.Until) {
			continue
		}
		// Pending-review transfers never touched balances: a payment
		// instruction generated from one would order money the ledger
		// has not released. The OFX export applies the same rule.
		if e, ok := l.entries[pt.TransferID]; ok && e.PendingReview {
			continue
		}
		debtorIBAN, ok := l.accountIBANs[pt.From]
		if !ok {
			l.mu.RUnlock()
			return nil, fmt.Errorf("%w: debtor %q", ErrMissingAccountIBAN, pt.From)
		}
		creditorIBAN, ok := l.accountIBANs[pt.To]
		if !ok {
			l.mu.RUnlock()
			return nil, fmt.Errorf("%w: creditor %q", ErrMissingAccountIBAN, pt.To)
		}
		payments = append(payments, pain001Payment{
			PainTransfer: pt,
			DebtorIBAN:   debtorIBAN,
			CreditorIBAN: creditorIBAN,
		})
	}

	doc, msgID, txCount, ctrlSum, err := renderPain001(payments, opts, currency)
	if err != nil {
		l.mu.RUnlock()
		return nil, err
	}
	details := map[string]any{
		"msg_id":         msgID,
		"tx_count":       txCount,
		"ctrl_sum_cents": ctrlSum,
		"currency":       pain001DetailsCurrency(payments, currency),
	}
	if opts.Debtor != "" {
		details["debtor"] = string(opts.Debtor)
	}
	if opts.Creditor != "" {
		details["creditor"] = string(opts.Creditor)
	}
	// Read-only, like Reconcile: the version brackets the export but
	// does not move — the audit event proves the log line was written
	// against a known ledger state.
	l.emitAudit(AuditEvent{
		Op:            "pain001_exported",
		Actor:         "ExportPain001",
		TraceID:       msgID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      pain001Accounts(payments),
		Details:       details,
	})
	l.mu.RUnlock()
	// Register the instruction's lifecycle record (see
	// registerPainInstructionLocked in pain001_status.go): the write
	// lock is taken here, after the read-only render, because the
	// record write is the export's only mutation. Registration is
	// idempotent in MsgId — a duplicate export re-derives the same
	// MsgId and returns the existing record.
	l.mu.Lock()
	l.registerPainInstructionLocked(msgID, payments, txCount, ctrlSum, pain001DetailsCurrency(payments, currency))
	l.mu.Unlock()
	return doc, nil
}

// pain001DetailsCurrency names the export's currency for the audit
// event: the requested filter, or the single currency when every
// payment shares one, else "MIXED" — a control total over mixed
// currencies is still reported honestly.
func pain001DetailsCurrency(payments []pain001Payment, filter string) string {
	if filter != "" {
		return filter
	}
	seen := map[string]bool{}
	for _, p := range payments {
		seen[p.Currency] = true
	}
	if len(seen) == 1 {
		for c := range seen {
			return c
		}
	}
	if len(seen) == 0 {
		return ""
	}
	return "MIXED"
}

// pain001Accounts collects the distinct debtor/creditor accounts of an
// export, sorted, for the audit event's account attribution.
func pain001Accounts(payments []pain001Payment) []AccountID {
	seen := map[AccountID]bool{}
	for _, p := range payments {
		seen[p.From] = true
		seen[p.To] = true
	}
	out := make([]AccountID, 0, len(seen))
	for a := range seen {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// pain001Amount renders integer cents as an exact decimal string with
// two fraction digits: 105 -> "1.05". Amounts are always positive here —
// transfers reject non-positive amounts — so no sign handling is needed.
// No floats anywhere.
func pain001Amount(cents int64) string {
	return fmt.Sprintf("%d.%02d", cents/100, cents%100)
}

// pain001MsgID derives the deterministic message ID from the export
// window, the filters, and the exported content: re-exporting the same
// window with the same data renders byte-identical output, and any
// content change (a new payment, an approval flipping a transfer
// effective) changes the ID.
func pain001MsgID(payments []pain001Payment, opts Pain001ExportOptions, currency string) string {
	var b strings.Builder
	b.WriteString("pain001-")
	b.WriteString(pain001UnixOrZero(opts.Since))
	b.WriteString("-")
	b.WriteString(pain001UnixOrZero(opts.Until))
	b.WriteString("-")
	b.WriteString(pain001FilterOrAll(string(opts.Debtor)))
	b.WriteString("-")
	b.WriteString(pain001FilterOrAll(string(opts.Creditor)))
	b.WriteString("-")
	b.WriteString(pain001FilterOrAll(currency))
	for _, p := range payments {
		b.WriteString("-")
		b.WriteString(p.TransferID)
	}
	return b.String()
}

func pain001UnixOrZero(t time.Time) string {
	if t.IsZero() {
		return "0"
	}
	return fmt.Sprintf("%d", t.UTC().Unix())
}

func pain001FilterOrAll(s string) string {
	if s == "" {
		return "ALL"
	}
	return s
}

// renderPain001 builds the pain.001.001.03 document for the resolved
// payments, grouped into one PmtInf per (debtor, currency) — the
// standard shape, one debit account per payment-information block — and
// returns the document plus the GrpHdr facts for the audit event.
func renderPain001(payments []pain001Payment, opts Pain001ExportOptions, currency string) (doc []byte, msgID string, txCount int, ctrlSum int64, err error) {
	msgID = pain001MsgID(payments, opts, currency)
	// Control sum over every exported instruction, overflow-checked:
	// a control total that wrapped would be worse than no export.
	var sum int64
	for _, p := range payments {
		var ok bool
		sum, ok = addCents(sum, p.AmountCents)
		if !ok {
			return nil, "", 0, 0, ErrPain001AmountOverflow
		}
	}
	txCount = len(payments)
	ctrlSum = sum

	// Group by (debtor, currency), debtor-major, both sorted — the
	// document is deterministic in the payment set.
	type groupKey struct {
		debtor   AccountID
		currency string
	}
	groups := map[groupKey][]pain001Payment{}
	var keys []groupKey
	for _, p := range payments {
		k := groupKey{debtor: p.From, currency: p.Currency}
		if _, ok := groups[k]; !ok {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], p)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].debtor != keys[j].debtor {
			return keys[i].debtor < keys[j].debtor
		}
		return keys[i].currency < keys[j].currency
	})

	now := time.Now().UTC()
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pain.001.001.03">` + "\n")
	b.WriteString("<CstmrCdtTrfInitn>\n")
	b.WriteString("<GrpHdr>\n")
	b.WriteString(fmt.Sprintf("<MsgId>%s</MsgId>\n", ofxEscape(msgID)))
	b.WriteString(fmt.Sprintf("<CreDtTm>%s</CreDtTm>\n", now.Format(time.RFC3339)))
	b.WriteString(fmt.Sprintf("<NbOfTxs>%d</NbOfTxs>\n", txCount))
	b.WriteString(fmt.Sprintf("<CtrlSum>%s</CtrlSum>\n", pain001Amount(ctrlSum)))
	b.WriteString("<InitgPty><Nm>LEDGER</Nm></InitgPty>\n")
	b.WriteString("</GrpHdr>\n")
	for _, k := range keys {
		ps := groups[k]
		var gsum int64
		for _, p := range ps {
			gsum, _ = addCents(gsum, p.AmountCents) // cannot overflow: bounded by the checked total
		}
		b.WriteString("<PmtInf>\n")
		b.WriteString(fmt.Sprintf("<PmtInfId>%s-%s-%s</PmtInfId>\n",
			ofxEscape(msgID), ofxEscape(string(k.debtor)), ofxEscape(k.currency)))
		b.WriteString("<PmtMtd>TRF</PmtMtd>\n")
		b.WriteString(fmt.Sprintf("<NbOfTxs>%d</NbOfTxs>\n", len(ps)))
		b.WriteString(fmt.Sprintf("<CtrlSum>%s</CtrlSum>\n", pain001Amount(gsum)))
		b.WriteString(fmt.Sprintf("<ReqdExctnDt>%s</ReqdExctnDt>\n", now.Format("2006-01-02")))
		b.WriteString(fmt.Sprintf("<Dbtr><Nm>%s</Nm></Dbtr>\n", ofxEscape(string(k.debtor))))
		b.WriteString(fmt.Sprintf("<DbtrAcct><Id><IBAN>%s</IBAN></Id></DbtrAcct>\n", ofxEscape(ps[0].DebtorIBAN.IBAN)))
		if ps[0].DebtorIBAN.BIC != "" {
			b.WriteString(fmt.Sprintf("<DbtrAgt><FinInstnId><BIC>%s</BIC></FinInstnId></DbtrAgt>\n", ofxEscape(ps[0].DebtorIBAN.BIC)))
		}
		for _, p := range ps {
			b.WriteString("<CdtTrfTxInf>\n")
			b.WriteString(fmt.Sprintf("<PmtId><EndToEndId>%s</EndToEndId></PmtId>\n", ofxEscape(p.TransferID)))
			b.WriteString(fmt.Sprintf("<Amt><InstdAmt Ccy=\"%s\">%s</InstdAmt></Amt>\n", ofxEscape(p.Currency), pain001Amount(p.AmountCents)))
			b.WriteString(fmt.Sprintf("<Cdtr><Nm>%s</Nm></Cdtr>\n", ofxEscape(string(p.To))))
			b.WriteString(fmt.Sprintf("<CdtrAcct><Id><IBAN>%s</IBAN></Id></CdtrAcct>\n", ofxEscape(p.CreditorIBAN.IBAN)))
			if p.CreditorIBAN.BIC != "" {
				b.WriteString(fmt.Sprintf("<CdtrAgt><FinInstnId><BIC>%s</BIC></FinInstnId></CdtrAgt>\n", ofxEscape(p.CreditorIBAN.BIC)))
			}
			b.WriteString("</CdtTrfTxInf>\n")
		}
		b.WriteString("</PmtInf>\n")
	}
	b.WriteString("</CstmrCdtTrfInitn>\n")
	b.WriteString("</Document>\n")
	return []byte(b.String()), msgID, txCount, ctrlSum, nil
}
