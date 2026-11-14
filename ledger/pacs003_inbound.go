package ledger

import (
	"encoding/xml"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ISO 20022 pacs.003.001.08 Customer Direct Debit inbound processing
// (LG-415): the inbound half of the pacs.003 lifecycle, the mirror of
// the LG-201 export.
//
// ExportPacs003 renders a posted transfer as the instruction the
// creditor's bank sends; this module consumes the instruction the
// creditor's bank sent, on the debtor bank's side: it verifies the
// pull against the mandate that authorizes it and books the debit.
// The two are orthogonal by construction:
//
//   - LG-201 exports: the ledger tells the debtor's bank "pull this
//     amount from the debtor under this mandate" (outbound,
//     read-only, no ledger state).
//   - LG-415 imports: the creditor's bank tells the ledger "pull this
//     amount from the debtor under this mandate" (inbound,
//     state-changing: one mandate debit under the mandate's
//     authorization).
//
// ParsePacs003 reuses the pacs003.go wire structs, reading them in
// the reverse direction: the same `pacs.003.001.08`
// `FIToFICstmrDrctDbt` document, one `DrctDbtTxInf` to start. The
// namespace rides the Document element, so a document in the wrong
// schema version fails the unmarshal — the version gate is the
// namespace, not a string check (the camt.056 inbound convention,
// LG-414).
//
// The collection date: pacs.003.001.08 carries no `ReqdColltnDt`
// element — that element lives on pain.008's `PmtInf` (the
// creditor-side initiation, LG-166), and the pacs.003
// `DrctDbtTx` block carries only `MndtRltdInf` / `CdtrSchmeId` /
// pre-notification fields. The date the creditor's bank asks the
// debtor's bank to settle is `GrpHdr/IntrBkSttlmDt`, and ApplyPacs003
// treats it as the requested collection date: it becomes the debit's
// booking timestamp, so DebitWithMandate's own validity-window gate
// enforces it. A document that states no settlement date books at
// now, the DebitWithMandate default. A malformed settlement date is
// a parse refusal — a pull whose collection day cannot be read is
// never booked on a guessed date.
//
// ApplyPacs003 delegates every authorization gate to
// DebitWithMandate — unknown mandate, non-active mandate, a
// collection date outside [ValidFrom, ValidUntil), an amount above
// the per-debit cap, a currency mismatch, and the frequency window
// are all that function's gates, unmodified. No mandate is ever
// auto-created: an instruction naming no mandate the ledger knows is
// refused, fail-closed. The legs come from the mandate itself (debit
// the payee, credit the payer), so the document's Cdtr/Dbtr parties
// and agents are informational — carried on the parsed struct and
// the audit event — never the source of the booking.
//
// Idempotency: the entry ID and the idempotency key are both the
// instruction's EndToEndId. A repeated MsgId — the same document
// re-sent — replays through DebitWithMandate's byKey check and
// returns the original receipt with Duplicate set, booking nothing
// new, even if the mandate has since been suspended, cancelled, or
// expired (the replay-books-nothing convention). The inbound audit
// event fires only on a fresh booking, never on a replay.
//
// Audit: a fresh booking emits `pacs003_debit_applied`, distinct
// from the `mandate_debit` event DebitWithMandate itself emits, so
// the audit trail tells the inbound pacs.003 direction apart from an
// operator-driven mandate debit (the pacs.004 `pacs004_return_applied`
// convention, LG-413).

// pacs.003 inbound errors.
var (
	// ErrPacs003Parse is returned by ParsePacs003 for malformed XML
	// or a document missing a required field: the creditor's bank
	// sent something the ledger cannot even read as a direct debit
	// instruction.
	ErrPacs003Parse = errors.New("ledger: pacs.003 inbound: unparseable direct debit document")
)

// Pacs003DirectDebit is one parsed inbound pacs.003 direct debit
// instruction: the instruction's identity, the mandate it pulls
// under, the amount, and the requested collection date.
type Pacs003DirectDebit struct {
	// MsgID is the instruction's GrpHdr MsgId: the audit trace for
	// the apply.
	MsgID string
	// InstrID, EndToEndID, and TxID are the PmtId triple. EndToEndId
	// is the business identity of the instruction: it becomes both
	// the journal entry ID and the idempotency key of the mandate
	// debit, so a re-sent instruction converges on the original
	// booking.
	InstrID    string
	EndToEndID string
	TxID       string
	// AmountCents is the InstdAmt in minor units; Currency is its
	// Ccy attribute, a 3-letter uppercase ISO 4217 code.
	AmountCents int64
	Currency    string
	// MandateID is the DrctDbtTx/MndtRltdInf/MndtId: the mandate the
	// pull is authorized under. Never auto-created.
	MandateID string
	// MandateSignatureDate is MndtRltdInf/DtOfSgntr (YYYY-MM-DD),
	// when the creditor's bank states it. Informational: the
	// authorization is the mandate row, not the date on the wire.
	MandateSignatureDate string
	// CollectionDate is GrpHdr/IntrBkSttlmDt (YYYY-MM-DD), the day
	// the creditor's bank asks the debtor's bank to settle. Zero
	// when the document states none: the debit then books at now.
	CollectionDate time.Time
	// Debtor and Creditor are the Dbtr/DbtrAcct and Cdtr/CdtrAcct
	// account references; DebtorAgent and CreditorAgent are the
	// DbtrAgt/CdtrAgt references. Informational: the legs come from
	// the mandate, never from the document.
	Debtor        string
	Creditor      string
	DebtorAgent   string
	CreditorAgent string
}

// pacs003ParseAmount parses an ISO 20022 decimal amount ("1234.56")
// into minor units. The integer part holds at most 12 digits (the
// pacs004ParseAmount cap); the fraction holds at most two digits and
// is right-padded ("5" means 50 cents); the value must be positive.
// Anything else is a parse refusal — a debit amount the ledger
// cannot read exactly is never rounded into a booking.
func pacs003ParseAmount(body string) (int64, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return 0, fmt.Errorf("%w: empty amount", ErrPacs003Parse)
	}
	intPart := body
	fracPart := ""
	if i := strings.IndexByte(body, '.'); i >= 0 {
		intPart, fracPart = body[:i], body[i+1:]
		if strings.IndexByte(fracPart, '.') >= 0 {
			return 0, fmt.Errorf("%w: amount %q has two decimal points", ErrPacs003Parse, body)
		}
	}
	if intPart == "" || len(intPart) > 12 || len(fracPart) > 2 {
		return 0, fmt.Errorf("%w: amount %q", ErrPacs003Parse, body)
	}
	for _, c := range intPart + fracPart {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("%w: amount %q is not numeric", ErrPacs003Parse, body)
		}
	}
	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: amount %q", ErrPacs003Parse, body)
	}
	frac := int64(0)
	if fracPart != "" {
		if len(fracPart) == 1 {
			fracPart += "0"
		}
		frac, err = strconv.ParseInt(fracPart, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%w: amount %q", ErrPacs003Parse, body)
		}
	}
	cents := whole*100 + frac
	if cents <= 0 {
		return 0, fmt.Errorf("%w: amount %q is not positive", ErrPacs003Parse, body)
	}
	return cents, nil
}

// validatePacs003DirectDebit fail-closes on a direct debit that
// ParsePacs003 would refuse, so ApplyPacs003 stays safe on hand-built
// structs too: the parse gate and the apply gate share one check,
// never a copy.
func validatePacs003DirectDebit(d Pacs003DirectDebit) error {
	if strings.TrimSpace(d.MsgID) == "" {
		return fmt.Errorf("%w: missing GrpHdr/MsgId", ErrPacs003Parse)
	}
	if strings.TrimSpace(d.EndToEndID) == "" {
		return fmt.Errorf("%w: missing PmtId/EndToEndId", ErrPacs003Parse)
	}
	if strings.TrimSpace(d.MandateID) == "" {
		return fmt.Errorf("%w: missing DrctDbtTx/MndtRltdInf/MndtId", ErrPacs003Parse)
	}
	if d.AmountCents <= 0 {
		return fmt.Errorf("%w: non-positive instructed amount", ErrPacs003Parse)
	}
	if strings.TrimSpace(d.Currency) == "" {
		return fmt.Errorf("%w: empty instructed amount currency", ErrPacs003Parse)
	}
	if _, err := normalizeCurrency(d.Currency); err != nil {
		return fmt.Errorf("%w: currency %q", ErrPacs003Parse, d.Currency)
	}
	return nil
}

// ParsePacs003 parses one inbound pacs.003 direct debit instruction
// into a Pacs003DirectDebit. It validates the whole document before
// returning anything: a missing MsgId, a missing EndToEndId, a missing
// mandate ID, an unreadable or non-positive amount, an empty or
// malformed currency, or a malformed signature or settlement date are
// all parse refusals — ApplyPacs003 never sees a half-read
// instruction. The creditor/ debtor parties and agents are read but
// not gated: the mandate names the legs, so a document that misnames
// a party cannot misdirect the booking.
func ParsePacs003(data []byte) (Pacs003DirectDebit, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return Pacs003DirectDebit{}, fmt.Errorf("%w: empty document", ErrPacs003Parse)
	}
	var doc pacs003Document
	if err := xml.Unmarshal(data, &doc); err != nil {
		return Pacs003DirectDebit{}, fmt.Errorf("%w: %v", ErrPacs003Parse, err)
	}
	tx := doc.DrctDbt.TxInf
	amount, err := pacs003ParseAmount(tx.InstdAmt.Value)
	if err != nil {
		return Pacs003DirectDebit{}, err
	}
	if strings.TrimSpace(tx.InstdAmt.Ccy) == "" {
		return Pacs003DirectDebit{}, fmt.Errorf("%w: empty instructed amount currency", ErrPacs003Parse)
	}
	currency, err := normalizeCurrency(tx.InstdAmt.Ccy)
	if err != nil {
		return Pacs003DirectDebit{}, fmt.Errorf("%w: currency %q", ErrPacs003Parse, tx.InstdAmt.Ccy)
	}
	var signatureDate string
	if tx.DrctDbtTx.MndtRltdInf.DtOfSgntr != nil {
		signatureDate = strings.TrimSpace(*tx.DrctDbtTx.MndtRltdInf.DtOfSgntr)
		if _, err := time.Parse("2006-01-02", signatureDate); err != nil {
			return Pacs003DirectDebit{}, fmt.Errorf("%w: mandate signature date %q must be YYYY-MM-DD", ErrPacs003Parse, signatureDate)
		}
	}
	var collectionDate time.Time
	if doc.DrctDbt.GrpHdr.IntrBkSttlmDt != nil {
		raw := strings.TrimSpace(*doc.DrctDbt.GrpHdr.IntrBkSttlmDt)
		parsed, err := time.Parse("2006-01-02", raw)
		if err != nil {
			return Pacs003DirectDebit{}, fmt.Errorf("%w: settlement date %q must be YYYY-MM-DD", ErrPacs003Parse, raw)
		}
		collectionDate = parsed
	}
	d := Pacs003DirectDebit{
		MsgID:                strings.TrimSpace(doc.DrctDbt.GrpHdr.MsgID),
		InstrID:              strings.TrimSpace(tx.PmtID.InstrID),
		EndToEndID:           strings.TrimSpace(tx.PmtID.EndToEndID),
		TxID:                 strings.TrimSpace(tx.PmtID.TxID),
		AmountCents:          amount,
		Currency:             currency,
		MandateID:            strings.TrimSpace(tx.DrctDbtTx.MndtRltdInf.MndtID),
		MandateSignatureDate: signatureDate,
		CollectionDate:       collectionDate,
		Debtor:               strings.TrimSpace(tx.DbtrAcct.ID.Othr.ID),
		Creditor:             strings.TrimSpace(tx.CdtrAcct.ID.Othr.ID),
		DebtorAgent:          strings.TrimSpace(tx.DbtrAgt.FinInstnID.Othr.ID),
		CreditorAgent:        strings.TrimSpace(tx.CdtrAgt.FinInstnID.Othr.ID),
	}
	if err := validatePacs003DirectDebit(d); err != nil {
		return Pacs003DirectDebit{}, err
	}
	return d, nil
}

// ApplyPacs003 books one parsed inbound pacs.003 direct debit: it
// pulls the instructed amount from the mandate's payer to the
// mandate's payee through DebitWithMandate, with the instruction's
// EndToEndId as both the journal entry ID and the idempotency key,
// and the requested collection date (when the document states one)
// as the booking timestamp, so the mandate's validity window is
// evaluated at the day the creditor's bank asked to collect.
//
// Every authorization gate is DebitWithMandate's, unmodified: an
// unknown mandate (ErrMandateNotFound), a non-active mandate
// (ErrMandateNotActive), a collection date outside [ValidFrom,
// ValidUntil) (ErrMandateNotActive before ValidFrom,
// ErrMandateExpired after), an amount above the per-debit cap
// (ErrMandateAmountExceeded), a currency mismatch
// (ErrMandateCurrencyMismatch), and a used frequency window
// (ErrMandateFrequencyExceeded) are all refused, fail-closed, with
// nothing booked. A repeated MsgId returns the original receipt with
// Duplicate set and books nothing new — and emits no second
// `pacs003_debit_applied` audit event, so a re-sent instruction
// leaves exactly one debit and one inbound audit event behind.
//
// The audit event is emitted after DebitWithMandate commits, under a
// separately acquired write lock: DebitWithMandate owns its lock
// internally, so the inbound direction marker cannot join its
// atomic commit zone — the debit itself is already durable when the
// marker lands.
func (l *Ledger) ApplyPacs003(d Pacs003DirectDebit, actor string) (MandateDebitReceipt, error) {
	if err := validatePacs003DirectDebit(d); err != nil {
		return MandateDebitReceipt{}, err
	}
	if actor == "" {
		actor = "ApplyPacs003"
	}
	receipt, err := l.DebitWithMandate(d.MandateID, MandateDebit{
		ID:             d.EndToEndID,
		AmountCents:    d.AmountCents,
		Currency:       d.Currency,
		Memo:           "pacs003:" + strings.TrimSpace(d.MsgID),
		IdempotencyKey: d.EndToEndID,
		CreatedAt:      d.CollectionDate,
	})
	if err != nil {
		return MandateDebitReceipt{}, err
	}
	if receipt.Duplicate {
		return receipt, nil
	}
	details := map[string]any{
		"msg_id":        d.MsgID,
		"mandate_id":    d.MandateID,
		"end_to_end_id": d.EndToEndID,
		"amount_cents":  receipt.Entry.AmountCents,
		"currency":      receipt.Entry.Currency,
		"entry_id":      receipt.Entry.ID,
	}
	if !d.CollectionDate.IsZero() {
		details["collection_date"] = d.CollectionDate.UTC().Format("2006-01-02")
	}
	l.mu.Lock()
	l.emitAudit(AuditEvent{
		Op:            "pacs003_debit_applied",
		Actor:         actor,
		TraceID:       d.MsgID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		EntryIDs:      []string{receipt.Entry.ID},
		Accounts:      []AccountID{receipt.Entry.CreditAccount, receipt.Entry.DebitAccount},
		Details:       details,
	})
	l.mu.Unlock()
	return receipt, nil
}
