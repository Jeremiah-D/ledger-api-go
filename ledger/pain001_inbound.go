package ledger

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ISO 20022 pain.001.001.03 customer credit transfer initiation
// inbound processing (LG-430): the inbound half of the pain.001
// lifecycle, the mirror of the LG-96 export.
//
// ExportPain001 renders this ledger's own booked transfers as the
// instruction file the debtor sends to its bank — the debtor side
// telling its bank "pay these". This module consumes the initiation
// file a customer sent to this ledger, acting as the customer's
// bank: it books each instructed credit transfer through
// PostTransfer, and nothing else. The two directions are
// orthogonal by construction:
//
//   - LG-96 exports: the ledger renders its own booked transfers
//     for the bank (outbound, read-only).
//   - LG-430 imports: a customer file asks the ledger to book
//     credit transfers (inbound, state-changing: one journal
//     entry per CdtTrfTxInf, plus the instruction lifecycle
//     record).
//   - LG-99 tracks: the pain.001 status machine on instructions
//     already exported or, from here, already booked (inbound,
//     state-changing: exported -> accepted | rejected -> settled).
//   - LG-162 answers: the bank's pain.002 verdict on an
//     instruction (inbound, state-changing).
//
// ParsePain001 builds new parse structs — the outbound export
// renders with strings.Builder, so there is no wire struct to
// reuse in reverse (the pain.008/pain.013 inbound convention,
// LG-428/LG-427). The parse structs mirror the export's rendered
// fields exactly (xmlns pain.001.001.03, GrpHdr MsgId/CreDtTm/
// NbOfTxs/CtrlSum, PmtInf PmtInfId/PmtMtd TRF/NbOfTxs/CtrlSum/
// ReqdExctnDt/Dbtr/DbtrAcct/DbtrAgt, CdtTrfTxInf PmtId
// (EndToEndId)/Amt InstdAmt/Cdtr/CdtrAcct/CdtrAgt), so a document
// this ledger's export renders parses back; no sub-element spelling
// is invented beyond what pain001.go renders. The namespace rides
// the Document element, so a document in the wrong schema version
// fails the unmarshal — the version gate is the namespace, not a
// string check (the camt.056 inbound convention, LG-414). RmtInf
// is not in the export's shape, so it is not parsed here either.
//
// The document's fields, and what they route to:
//
//   - The wire DbtrAcct/CdtrAcct IBANs are resolved to ledger
//     accounts by the new reverse-lookup helper
//     (accountByIBANLocked, over the registered IBAN table): an
//     IBAN no account registered parks that transaction in the
//     repair queue — the ledger never opens an account from a
//     wire identifier (the pain.013 convention, LG-427).
//   - ReqdExctnDt must be today's UTC date, per PmtInf. The
//     ledger has no future-date (or past-date) execution
//     scheduling, so every transaction of a PmtInf dated any
//     other day parks in the repair queue instead of inventing a
//     holding position.
//   - Each transaction books through PostTransfer, so all of its
//     gates apply — idempotency replay, frozen/overdraft, daily
//     limit, accounting period, dual-control review — and the
//     EndToEndId is both the journal entry ID and the
//     per-transaction idempotency key's tail ("pain001-<MsgId>-
//     <EndToEndId>"), so the customer can dedupe on it exactly
//     like an exported transfer.
//   - A same-party transfer, a non-positive amount, or an empty
//     currency is a per-transaction refusal after parsing: the
//     amount parser accepts a signed value and the currency may
//     arrive empty, and the apply gate parks the transaction
//     (PostTransfer's own field validation, or the empty-currency
//     gate) instead of refusing the whole file.
//   - Single-currency instructions only: ToCurrency is always
//     empty on the booked transfer. A cross-currency payment
//     settles through the FX clearing account as two legs, not
//     one direct debtor-to-creditor payment, so it is not
//     representable as a single pain.001 instruction (the
//     PainTransfer documentation, pain001.go).
//
// Fail-closed parking (the pacs.004 convention, LG-413): a
// transaction that fails any gate parks in the repair queue for
// operator resolution and never blocks the other transactions.
// A document the ledger cannot even parse is parked the same way
// by ParkPain001Unparsed. Repair records parked by this module
// are visibility parking, not resubmittable instructions: their
// Maker is deliberately empty, so the CreatePayout gate can never
// pass them.
//
// Idempotency is the GrpHdr MsgId with its terms (the pain.008/
// pain.013 convention, LG-428/LG-427): re-applying the same MsgId
// with the same transactions returns the stored outcome with
// Duplicate set, booking nothing new and emitting no second audit
// event. A MsgId reused for different terms is refused with
// ErrPain001MsgIDConflict — never silently overwritten. The
// per-transaction idempotency keys are a second layer: even if
// the in-memory replay record is lost (a restart), PostTransfer
// replays the original receipts instead of booking twice. A MsgId
// that already names an instruction the ledger exported is a
// conflict too: the export's record owns that MsgId, and the
// inbound transfers must not silently join it.
//
// Registration is not execution: the inbound booking registers
// the transfer principals (PostTransfer) and the instruction's
// lifecycle record under the wire MsgId, so the already-booked
// transfers enter the LG-99 lifecycle — the bank's pain.002
// verdict and the operator's settle confirmation still drive
// them, exactly like an exported instruction.
//
// No pain.001 export change; no PostTransfer semantic change; no
// pain.002 answer change; no automatic account opening; no
// cross-currency single instruction; no future-date scheduling.

// pain.001 inbound errors.
var (
	// ErrInvalidPain001 is returned by ParsePain001 for malformed
	// XML or a document missing a required field, and by
	// ApplyPain001 for a hand-built document that fails the same
	// gates: the customer sent something the ledger cannot even
	// read as a credit transfer initiation. The HTTP layer parks
	// it in the repair queue and maps it to 400.
	ErrInvalidPain001 = errors.New("ledger: invalid pain.001 customer credit transfer initiation")
	// ErrPain001MsgIDConflict is returned by ApplyPain001 when a
	// GrpHdr MsgId already applied carries different terms, or
	// already names an exported instruction: the message ID names
	// one initiation, and a second, different initiation under it
	// is refused rather than silently overwritten (the pain.008
	// ErrPain008MsgIDConflict convention). It is a 422-class
	// error.
	ErrPain001MsgIDConflict = errors.New("ledger: pain.001 inbound: message ID already applied with different terms")
	// ErrPain001ExecutionDateNotToday is returned per transaction
	// when ReqdExctnDt is not today's UTC date: the ledger has
	// no future-date (or past-date) execution scheduling, so the
	// transaction parks in the repair queue.
	ErrPain001ExecutionDateNotToday = errors.New("ledger: pain.001 inbound: execution date is not today (UTC)")
	// ErrPain001UnknownIBAN is returned per transaction when a
	// wire IBAN names no account the ledger registered an IBAN
	// for: the ledger never opens an account from a wire
	// identifier — the transaction parks in the repair queue.
	ErrPain001UnknownIBAN = errors.New("ledger: pain.001 inbound: IBAN names no registered account")
	// ErrPain001EmptyCurrency is returned per transaction when
	// the wire InstdAmt carries no currency: an amount without a
	// currency cannot book, and the transaction parks in the
	// repair queue.
	ErrPain001EmptyCurrency = errors.New("ledger: pain.001 inbound: instructed amount has no currency")
)

// Pain001CreditTransfer is one parsed inbound pain.001 credit
// transfer transaction: a CdtTrfTxInf with its PmtInf context
// (execution date, debtor identity) attached, in document order.
type Pain001CreditTransfer struct {
	// MsgID is the GrpHdr MsgId: the audit trace and the replay
	// key — one MsgId names one initiation.
	MsgID string `json:"msg_id"`
	// PmtInfID is PmtInf/PmtInfId, carried verbatim.
	PmtInfID string `json:"pmt_inf_id"`
	// ReqdExctnDt is PmtInf/ReqdExctnDt: the execution date,
	// always today's UTC date for a booked transaction.
	ReqdExctnDt time.Time `json:"reqd_exctn_dt"`
	// DebtorName is PmtInf/Dbtr/Nm, DebtorIBAN is
	// PmtInf/DbtrAcct/Id/IBAN, DebtorBIC is
	// PmtInf/DbtrAgt/FinInstnId/BIC when stated.
	DebtorName string `json:"debtor_name,omitempty"`
	DebtorIBAN string `json:"debtor_iban"`
	DebtorBIC  string `json:"debtor_bic,omitempty"`
	// InstrID is CdtTrfTxInf/PmtId/InstrId when stated, carried
	// verbatim; EndToEndID is PmtId/EndToEndId — the journal
	// entry ID and the per-transaction idempotency key's tail.
	InstrID    string `json:"instr_id,omitempty"`
	EndToEndID string `json:"end_to_end_id"`
	// AmountCents is CdtTrfTxInf/Amt/InstdAmt in minor units;
	// Currency is its Ccy attribute, normalized — empty when
	// the wire stated none (an apply-time refusal, never a
	// silent default).
	AmountCents int64  `json:"amount_cents"`
	Currency    string `json:"currency"`
	// CreditorName is CdtTrfTxInf/Cdtr/Nm, CreditorIBAN is
	// CdtrAcct/Id/IBAN, CreditorBIC is
	// CdtrAgt/FinInstnId/BIC when stated.
	CreditorName string `json:"creditor_name,omitempty"`
	CreditorIBAN string `json:"creditor_iban"`
	CreditorBIC  string `json:"creditor_bic,omitempty"`
}

// Pain001Document is one parsed inbound pain.001.001.03 customer
// credit transfer initiation: the GrpHdr identity plus every
// credit transfer transaction it carries, in document order.
type Pain001Document struct {
	// MsgID is the GrpHdr MsgId: the audit trace and the replay
	// key — one MsgId names one initiation.
	MsgID string `json:"msg_id"`
	// TxCount is the GrpHdr NbOfTxs as parsed (or the item count
	// when the header omits it); CtrlSumCents is the GrpHdr
	// CtrlSum in minor units. Both are re-checked by
	// validatePain001Document, so a hand-built document cannot
	// smuggle in a batch that disagrees with its own header.
	TxCount      int   `json:"tx_count"`
	CtrlSumCents int64 `json:"ctrl_sum_cents"`
	// Items are the CdtTrfTxInf transactions, in document
	// order, each carrying its PmtInf context.
	Items []Pain001CreditTransfer `json:"items"`
}

// Pain001ItemResult is the per-transaction outcome of
// ApplyPain001: a transaction is either booked (Receipt set,
// EntryID set), an idempotent replay (Duplicate true), or
// rejected (Err set, RepairID set). One transaction's failure
// never blocks the others.
type Pain001ItemResult struct {
	// Instruction is the parsed transaction this result is about.
	Instruction Pain001CreditTransfer `json:"instruction"`
	// From and To are the ledger accounts the wire IBANs
	// resolved to, empty when the transaction was rejected
	// before the lookup (or the lookup itself failed).
	From AccountID `json:"from_account,omitempty"`
	To   AccountID `json:"to_account,omitempty"`
	// Receipt is the PostTransfer receipt on success or replay;
	// empty when rejected.
	Receipt TransferReceipt `json:"receipt,omitempty"`
	// EntryID is the booked entry's ID (the transaction's
	// EndToEndId), empty when rejected.
	EntryID string `json:"entry_id,omitempty"`
	// Duplicate reports an idempotent replay: the same
	// transaction was applied before, so the earlier entry is
	// returned and nothing new was booked — no new audit event.
	Duplicate bool `json:"duplicate"`
	// ReviewStatus carries the PostTransfer dual-control state
	// ("pending_review" when the transfer was routed into
	// operator review instead of settling); empty otherwise.
	ReviewStatus string `json:"review_status,omitempty"`
	// RepairID is the repair-queue record holding a rejected
	// transaction for operator resolution, empty on success.
	RepairID string `json:"repair_id,omitempty"`
	// Err is the per-transaction rejection reason, nil on success.
	Err error `json:"-"`
}

// Pain001ApplyResult is the outcome of ApplyPain001.
type Pain001ApplyResult struct {
	// MsgID is the GrpHdr MsgId of the applied document.
	MsgID string `json:"msg_id"`
	// Items are the per-transaction outcomes, in document order.
	Items []Pain001ItemResult `json:"items"`
	// RepairID is set on the parked fail-closed path for the
	// whole batch, when one exists; this module has no
	// whole-batch park beyond the unparsed-document path, so it
	// stays empty here and the HTTP layer carries the unparsed
	// repair ID itself.
	RepairID string `json:"repair_id,omitempty"`
	// Duplicate reports whether the whole document was already
	// applied under the same MsgId with the same terms: a no-op
	// success, not an error.
	Duplicate bool `json:"duplicate,omitempty"`
}

// pain001AppliedRecord is the replay record for one applied
// pain.001 initiation, keyed by GrpHdr MsgId in pain001Applied:
// the transaction terms it carried and the apply outcome, so a
// replay returns the same outcome without re-booking or
// re-parking.
type pain001AppliedRecord struct {
	MsgID  string
	Items  []Pain001CreditTransfer
	Result Pain001ApplyResult
}

// pain001ItemsEqual reports whether two transaction lists carry
// exactly the same terms, in document order: the same PmtInf,
// execution date, parties (by normalized IBAN), amounts and
// currencies, and EndToEndIds.
func pain001ItemsEqual(a, b []Pain001CreditTransfer) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if x.PmtInfID != y.PmtInfID ||
			!x.ReqdExctnDt.Equal(y.ReqdExctnDt) ||
			x.DebtorName != y.DebtorName ||
			normalizeIBAN(x.DebtorIBAN) != normalizeIBAN(y.DebtorIBAN) ||
			x.DebtorBIC != y.DebtorBIC ||
			x.InstrID != y.InstrID ||
			x.EndToEndID != y.EndToEndID ||
			x.AmountCents != y.AmountCents ||
			x.Currency != y.Currency ||
			x.CreditorName != y.CreditorName ||
			normalizeIBAN(x.CreditorIBAN) != normalizeIBAN(y.CreditorIBAN) ||
			x.CreditorBIC != y.CreditorBIC {
			return false
		}
	}
	return true
}

// accountByIBANLocked resolves a normalized IBAN to the ledger
// account that registered it, or false when no account did. When
// two accounts registered the same IBAN (the registry does not
// forbid it), the lexicographically first account wins, so the
// resolution is deterministic. Callers must hold l.mu (either
// lock suffices).
func (l *Ledger) accountByIBANLocked(iban string) (AccountID, bool) {
	for _, reg := range l.accountIBANsLocked() {
		if reg.IBAN == iban {
			return reg.Account, true
		}
	}
	return "", false
}

// The pain001x* structs are the inbound-side XML model for
// pain.001.001.03: the outbound export renders with
// strings.Builder, so this is the first struct form of the
// message — shaped to match the export field for field
// (GrpHdr/MsgId/CreDtTm/NbOfTxs/CtrlSum/InitgPty, PmtInf/
// PmtInfId/PmtMtd/NbOfTxs/CtrlSum/ReqdExctnDt/Dbtr/DbtrAcct/
// DbtrAgt, CdtTrfTxInf/PmtId/Amt/InstdAmt/Cdtr/CdtrAcct/
// CdtrAgt) so a document this ledger's export renders parses
// back. The namespace on Document gates the schema version.
type pain001xDocument struct {
	XMLName xml.Name      `xml:"urn:iso:std:iso:20022:tech:xsd:pain.001.001.03 Document"`
	Initn   pain001xInitn `xml:"CstmrCdtTrfInitn"`
}

type pain001xInitn struct {
	GrpHdr  pain001xGrpHdr   `xml:"GrpHdr"`
	PmtInfs []pain001xPmtInf `xml:"PmtInf"`
}

type pain001xGrpHdr struct {
	MsgID   string  `xml:"MsgId"`
	CreDtTm string  `xml:"CreDtTm"`
	NbOfTxs *int    `xml:"NbOfTxs"`
	CtrlSum *string `xml:"CtrlSum"`
}

type pain001xPmtInf struct {
	PmtInfID     string          `xml:"PmtInfId"`
	PmtMtd       string          `xml:"PmtMtd"`
	NbOfTxs      *int            `xml:"NbOfTxs"`
	CtrlSum      *string         `xml:"CtrlSum"`
	ReqdExctnDt  string          `xml:"ReqdExctnDt"`
	Dbtr         pain001xParty   `xml:"Dbtr"`
	DbtrAcct     pain001xAcct    `xml:"DbtrAcct"`
	DbtrAgt      *pain001xAgt    `xml:"DbtrAgt,omitempty"`
	Txs          []pain001xTx    `xml:"CdtTrfTxInf"`
}

type pain001xTx struct {
	PmtID      pain001xPmtID `xml:"PmtId"`
	InstdAmt   pain001xAmt   `xml:"Amt>InstdAmt"`
	Cdtr       pain001xParty `xml:"Cdtr"`
	CdtrAcct   pain001xAcct  `xml:"CdtrAcct"`
	CdtrAgt    *pain001xAgt  `xml:"CdtrAgt,omitempty"`
}

type pain001xPmtID struct {
	InstrID    string `xml:"InstrId"`
	EndToEndID string `xml:"EndToEndId"`
}

type pain001xAmt struct {
	Ccy   string `xml:"Ccy,attr"`
	Value string `xml:",chardata"`
}

type pain001xParty struct {
	Nm string `xml:"Nm"`
}

type pain001xAcct struct {
	ID pain001xAcctID `xml:"Id"`
}

type pain001xAcctID struct {
	IBAN string `xml:"IBAN"`
}

type pain001xAgt struct {
	FinInstnId pain001xFinInstnID `xml:"FinInstnId"`
}

type pain001xFinInstnID struct {
	BIC string `xml:"BIC"`
}

// pain001ParseAmount parses an ISO 20022 decimal amount
// ("1234.56") into minor units, the pacs010ParseAmount rules:
// the integer part holds at most 12 digits, the fraction at most
// two digits and is right-padded ("5" means 50 cents). Unlike
// pacs010ParseAmount it accepts a leading minus sign and a zero
// value: a non-positive instructed amount is a per-transaction
// apply refusal (parked by ApplyPain001), not a parse refusal —
// the whole file must not fail because one transaction names a
// bad amount. A malformed amount is still a parse refusal — an
// amount the ledger cannot read exactly is never rounded into a
// booking.
func pain001ParseAmount(body string) (int64, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return 0, fmt.Errorf("%w: empty amount", ErrInvalidPain001)
	}
	neg := false
	if strings.HasPrefix(body, "-") {
		neg = true
		body = body[1:]
	}
	intPart := body
	fracPart := ""
	if i := strings.IndexByte(body, '.'); i >= 0 {
		intPart, fracPart = body[:i], body[i+1:]
		if strings.IndexByte(fracPart, '.') >= 0 {
			return 0, fmt.Errorf("%w: amount %q has two decimal points", ErrInvalidPain001, body)
		}
	}
	if intPart == "" || len(intPart) > 12 || len(fracPart) > 2 {
		return 0, fmt.Errorf("%w: amount %q", ErrInvalidPain001, body)
	}
	for _, c := range intPart + fracPart {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("%w: amount %q is not numeric", ErrInvalidPain001, body)
		}
	}
	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: amount %q", ErrInvalidPain001, body)
	}
	frac := int64(0)
	if fracPart != "" {
		if len(fracPart) == 1 {
			fracPart += "0"
		}
		frac, err = strconv.ParseInt(fracPart, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%w: amount %q", ErrInvalidPain001, body)
		}
	}
	cents := whole*100 + frac
	if neg {
		cents = -cents
	}
	return cents, nil
}

// validatePain001Document fail-closes on a document ParsePain001
// would refuse, so ApplyPain001 stays safe on hand-built
// documents too: the parse gate and the apply gate share one
// check, never a copy. The business gates (execution date is
// today, IBANs resolve to registered accounts, same-party and
// non-positive amounts, empty currency, and PostTransfer's own
// gates) are deliberately NOT here: they are per-transaction
// apply decisions, and a refusal parks that transaction in the
// repair queue instead of refusing the file.
func validatePain001Document(doc Pain001Document) error {
	if strings.TrimSpace(doc.MsgID) == "" {
		return fmt.Errorf("%w: missing GrpHdr/MsgId", ErrInvalidPain001)
	}
	if len(doc.Items) == 0 {
		return fmt.Errorf("%w: no CdtTrfTxInf", ErrInvalidPain001)
	}
	if doc.TxCount != len(doc.Items) {
		return fmt.Errorf("%w: GrpHdr/NbOfTxs %d disagrees with %d CdtTrfTxInf", ErrInvalidPain001, doc.TxCount, len(doc.Items))
	}
	var sum int64
	for _, d := range doc.Items {
		if strings.TrimSpace(d.EndToEndID) == "" {
			return fmt.Errorf("%w: missing CdtTrfTxInf/PmtId/EndToEndId", ErrInvalidPain001)
		}
		if strings.TrimSpace(d.PmtInfID) == "" {
			return fmt.Errorf("%w: missing PmtInf/PmtInfId", ErrInvalidPain001)
		}
		if d.ReqdExctnDt.IsZero() {
			return fmt.Errorf("%w: missing PmtInf/ReqdExctnDt", ErrInvalidPain001)
		}
		if strings.TrimSpace(d.DebtorIBAN) == "" {
			return fmt.Errorf("%w: missing PmtInf/DbtrAcct/Id/IBAN", ErrInvalidPain001)
		}
		if strings.TrimSpace(d.CreditorIBAN) == "" {
			return fmt.Errorf("%w: missing CdtTrfTxInf/CdtrAcct/Id/IBAN", ErrInvalidPain001)
		}
		if d.Currency != "" {
			if _, err := normalizeCurrency(d.Currency); err != nil {
				return fmt.Errorf("%w: currency %q", ErrInvalidPain001, d.Currency)
			}
		}
		var ok bool
		sum, ok = addCents(sum, d.AmountCents)
		if !ok {
			return ErrPain001AmountOverflow
		}
	}
	if sum != doc.CtrlSumCents {
		return fmt.Errorf("%w: GrpHdr/CtrlSum %d disagrees with instructed total %d", ErrInvalidPain001, doc.CtrlSumCents, sum)
	}
	return nil
}

// ParsePain001 parses one inbound pain.001.001.03 customer credit
// transfer initiation into a Pain001Document. It validates the whole
// document before returning anything: a missing MsgId, a PmtInf
// without an ID or with a PmtMtd other than TRF, a missing or
// malformed execution date, a NbOfTxs or CtrlSum that disagrees
// with the transactions (a missing GrpHdr CtrlSum is refused the
// same way — an unverifiable batch is never applied on trust), no
// transactions at all, or any transaction with a missing
// EndToEndId, an unreadable amount, a malformed currency, or a
// missing debtor or creditor IBAN are all parse refusals —
// ApplyPain001 never sees a half-read document. The party names
// and the agent BICs are carried verbatim when stated and never
// gated: the IBAN registry names the legs, not the names. An
// execution date that is not today, an IBAN that resolves to no
// registered account, a same-party transfer, a non-positive
// amount, and an empty currency all parse fine: they are
// per-transaction apply refusals, parked by ApplyPain001.
func ParsePain001(data []byte) (Pain001Document, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return Pain001Document{}, fmt.Errorf("%w: empty document", ErrInvalidPain001)
	}
	var doc pain001xDocument
	if err := xml.Unmarshal(data, &doc); err != nil {
		return Pain001Document{}, fmt.Errorf("%w: %v", ErrInvalidPain001, err)
	}
	msgID := strings.TrimSpace(doc.Initn.GrpHdr.MsgID)
	if msgID == "" {
		return Pain001Document{}, fmt.Errorf("%w: missing GrpHdr/MsgId", ErrInvalidPain001)
	}
	if len(doc.Initn.PmtInfs) == 0 {
		return Pain001Document{}, fmt.Errorf("%w: no PmtInf", ErrInvalidPain001)
	}
	items := make([]Pain001CreditTransfer, 0)
	var sum int64
	for _, pmt := range doc.Initn.PmtInfs {
		pmtInfID := strings.TrimSpace(pmt.PmtInfID)
		if pmtInfID == "" {
			return Pain001Document{}, fmt.Errorf("%w: missing PmtInf/PmtInfId", ErrInvalidPain001)
		}
		if strings.TrimSpace(pmt.PmtMtd) != "TRF" {
			return Pain001Document{}, fmt.Errorf("%w: PmtInf/PmtMtd %q is not TRF", ErrInvalidPain001, strings.TrimSpace(pmt.PmtMtd))
		}
		rawExctnDt := strings.TrimSpace(pmt.ReqdExctnDt)
		if rawExctnDt == "" {
			return Pain001Document{}, fmt.Errorf("%w: missing PmtInf/ReqdExctnDt", ErrInvalidPain001)
		}
		exctnDt, err := time.Parse("2006-01-02", rawExctnDt)
		if err != nil {
			return Pain001Document{}, fmt.Errorf("%w: execution date %q must be YYYY-MM-DD", ErrInvalidPain001, rawExctnDt)
		}
		debtorIBAN := normalizeIBAN(strings.TrimSpace(pmt.DbtrAcct.ID.IBAN))
		if debtorIBAN == "" {
			return Pain001Document{}, fmt.Errorf("%w: missing PmtInf/DbtrAcct/Id/IBAN", ErrInvalidPain001)
		}
		debtorBIC := ""
		if pmt.DbtrAgt != nil {
			debtorBIC = strings.TrimSpace(pmt.DbtrAgt.FinInstnId.BIC)
		}
		if len(pmt.Txs) == 0 {
			return Pain001Document{}, fmt.Errorf("%w: PmtInf %q carries no CdtTrfTxInf", ErrInvalidPain001, pmtInfID)
		}
		if pmt.NbOfTxs != nil && *pmt.NbOfTxs != len(pmt.Txs) {
			return Pain001Document{}, fmt.Errorf("%w: PmtInf/NbOfTxs %d disagrees with %d CdtTrfTxInf", ErrInvalidPain001, *pmt.NbOfTxs, len(pmt.Txs))
		}
		var pmtSum int64
		for _, x := range pmt.Txs {
			endToEndID := strings.TrimSpace(x.PmtID.EndToEndID)
			if endToEndID == "" {
				return Pain001Document{}, fmt.Errorf("%w: missing CdtTrfTxInf/PmtId/EndToEndId", ErrInvalidPain001)
			}
			creditorIBAN := normalizeIBAN(strings.TrimSpace(x.CdtrAcct.ID.IBAN))
			if creditorIBAN == "" {
				return Pain001Document{}, fmt.Errorf("%w: missing CdtTrfTxInf/CdtrAcct/Id/IBAN", ErrInvalidPain001)
			}
			if strings.TrimSpace(x.InstdAmt.Value) == "" {
				return Pain001Document{}, fmt.Errorf("%w: missing CdtTrfTxInf/Amt/InstdAmt", ErrInvalidPain001)
			}
			cents, err := pain001ParseAmount(x.InstdAmt.Value)
			if err != nil {
				return Pain001Document{}, err
			}
			ccy := strings.TrimSpace(x.InstdAmt.Ccy)
			currency := ""
			if ccy != "" {
				currency, err = normalizeCurrency(ccy)
				if err != nil {
					return Pain001Document{}, fmt.Errorf("%w: currency %q", ErrInvalidPain001, ccy)
				}
			}
			creditorBIC := ""
			if x.CdtrAgt != nil {
				creditorBIC = strings.TrimSpace(x.CdtrAgt.FinInstnId.BIC)
			}
			items = append(items, Pain001CreditTransfer{
				MsgID:        msgID,
				PmtInfID:     pmtInfID,
				ReqdExctnDt:  exctnDt,
				DebtorName:   strings.TrimSpace(pmt.Dbtr.Nm),
				DebtorIBAN:   debtorIBAN,
				DebtorBIC:    debtorBIC,
				InstrID:      strings.TrimSpace(x.PmtID.InstrID),
				EndToEndID:   endToEndID,
				AmountCents:  cents,
				Currency:     currency,
				CreditorName: strings.TrimSpace(x.Cdtr.Nm),
				CreditorIBAN: creditorIBAN,
				CreditorBIC:  creditorBIC,
			})
			var ok bool
			pmtSum, ok = addCents(pmtSum, cents)
			if !ok {
				return Pain001Document{}, ErrPain001AmountOverflow
			}
		}
		if pmt.CtrlSum != nil {
			ctrlSum, err := pain001ParseAmount(*pmt.CtrlSum)
			if err != nil {
				return Pain001Document{}, fmt.Errorf("%w: PmtInf/CtrlSum %q", ErrInvalidPain001, strings.TrimSpace(*pmt.CtrlSum))
			}
			if ctrlSum != pmtSum {
				return Pain001Document{}, fmt.Errorf("%w: PmtInf/CtrlSum %d disagrees with instructed total %d", ErrInvalidPain001, ctrlSum, pmtSum)
			}
		}
		var ok bool
		sum, ok = addCents(sum, pmtSum)
		if !ok {
			return Pain001Document{}, ErrPain001AmountOverflow
		}
	}
	grp := doc.Initn.GrpHdr
	if grp.NbOfTxs != nil && *grp.NbOfTxs != len(items) {
		return Pain001Document{}, fmt.Errorf("%w: GrpHdr/NbOfTxs %d disagrees with %d CdtTrfTxInf", ErrInvalidPain001, *grp.NbOfTxs, len(items))
	}
	if grp.CtrlSum == nil {
		return Pain001Document{}, fmt.Errorf("%w: missing GrpHdr/CtrlSum", ErrInvalidPain001)
	}
	ctrlSum, err := pain001ParseAmount(*grp.CtrlSum)
	if err != nil {
		return Pain001Document{}, fmt.Errorf("%w: control sum %q", ErrInvalidPain001, strings.TrimSpace(*grp.CtrlSum))
	}
	if ctrlSum != sum {
		return Pain001Document{}, fmt.Errorf("%w: GrpHdr/CtrlSum %d disagrees with instructed total %d", ErrInvalidPain001, ctrlSum, sum)
	}
	return Pain001Document{
		MsgID:        msgID,
		TxCount:      len(items),
		CtrlSumCents: sum,
		Items:        items,
	}, nil
}

// pain001LenientDocument is the namespace-agnostic wire shape
// ParkPain001Unparsed reads: a document ParsePain001 refused
// for its version may still name the initiation, and the namespace
// is the only thing the strict parse gates on. Unknown elements
// are skipped, the camt.029 convention.
type pain001LenientDocument struct {
	XMLName xml.Name            `xml:"Document"`
	Initn   pain001LenientInitn `xml:"CstmrCdtTrfInitn"`
}

type pain001LenientInitn struct {
	GrpHdr pain001LenientGrpHdr `xml:"GrpHdr"`
}

type pain001LenientGrpHdr struct {
	MsgID string `xml:"MsgId"`
}

// ParkPain001Unparsed best-effort parks a pain.001 document
// ParsePain001 refused, when the document still names an
// initiation: the GrpHdr MsgId it carries. It returns the repair
// record ID, or "" when the document names nothing parkable
// (garbage bytes, an empty shell): there is no initiation for the
// operator to find, so there is nothing to park. A parking failure
// (a full queue) is returned so the caller can fail loud rather
// than silently drop the customer's message. The reason code is
// invalid_format — the document was present but unreadable as a
// customer credit transfer initiation.
func (l *Ledger) ParkPain001Unparsed(data []byte, actor string) (string, error) {
	var doc pain001LenientDocument
	if err := xml.Unmarshal(data, &doc); err != nil {
		return "", nil
	}
	id := strings.TrimSpace(doc.Initn.GrpHdr.MsgID)
	if id == "" {
		return "", nil
	}
	rec, err := l.EnqueueRepair(PayoutInstruction{
		ID: id,
		// Maker intentionally empty: see the package comment.
		Destination: "pain001-inbound:" + id,
	}, RepairReasonInvalidFormat, actor)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// parkPain001Item parks one parsed pain.001 transaction that failed
// closed, and returns the repair record ID. The snapshot's ID is
// the transaction's idempotency key, so the parked item is
// traceable to the exact transfer it refused; its Maker is
// deliberately empty (see the package comment).
func (l *Ledger) parkPain001Item(d Pain001CreditTransfer, reasonCode, actor string) (string, error) {
	rec, err := l.EnqueueRepair(PayoutInstruction{
		ID:          "pain001-" + d.MsgID + "-" + d.EndToEndID,
		AmountCents: d.AmountCents,
		Currency:    d.Currency,
		// Maker intentionally empty: see the package comment.
		Destination: "pain001-inbound:" + d.MsgID,
	}, reasonCode, actor)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// applyPain001Item books one parsed transaction through
// PostTransfer and records the per-transaction outcome.
//
// The gates run in order, each fail-closed into the repair queue:
// the execution date must be today's UTC date; the wire IBANs
// must each resolve to a registered account; the currency must
// be stated; and PostTransfer's own gates (field validation —
// same-party, non-positive amount — idempotency replay, frozen /
// overdraft, daily limit, accounting period, dual-control review)
// apply last. The IBAN resolution runs under the read lock; the
// booking itself runs outside it, because PostTransfer takes the
// write lock.
func (l *Ledger) applyPain001Item(d Pain001CreditTransfer, actor string) Pain001ItemResult {
	res := Pain001ItemResult{Instruction: d}
	fail := func(err error, reasonCode string) Pain001ItemResult {
		res.Err = err
		repairID, parkErr := l.parkPain001Item(d, reasonCode, actor)
		if parkErr != nil {
			res.Err = fmt.Errorf("%w (repair parking failed: %v)", err, parkErr)
			return res
		}
		res.RepairID = repairID
		return res
	}

	// The execution date is today's UTC date — the ledger has no
	// future-date (or past-date) execution scheduling. The date
	// rides the PmtInf, so a dated block parks its transactions
	// together, never the other blocks.
	today := time.Now().UTC().Format("2006-01-02")
	if d.ReqdExctnDt.UTC().Format("2006-01-02") != today {
		return fail(fmt.Errorf("%w: ReqdExctnDt %q is not today (UTC)", ErrPain001ExecutionDateNotToday, d.ReqdExctnDt.UTC().Format("2006-01-02")), RepairReasonInvalidFormat)
	}

	// The wire IBANs must each resolve to a registered account.
	// No account is ever opened from a wire identifier.
	l.mu.RLock()
	from, fromOK := l.accountByIBANLocked(d.DebtorIBAN)
	to, toOK := l.accountByIBANLocked(d.CreditorIBAN)
	l.mu.RUnlock()
	if !fromOK {
		return fail(fmt.Errorf("%w: debtor IBAN %q", ErrPain001UnknownIBAN, d.DebtorIBAN), RepairReasonUnknownRoute)
	}
	if !toOK {
		return fail(fmt.Errorf("%w: creditor IBAN %q", ErrPain001UnknownIBAN, d.CreditorIBAN), RepairReasonUnknownRoute)
	}
	res.From = from
	res.To = to

	// An amount without a currency cannot book.
	if d.Currency == "" {
		return fail(fmt.Errorf("%w: EndToEndId %q", ErrPain001EmptyCurrency, d.EndToEndID), RepairReasonMissingField)
	}

	// Booking reuses PostTransfer: its gates (field validation,
	// idempotency replay, frozen/overdraft, daily limit,
	// accounting period, dual-control review) apply, and a
	// refusal parks the transaction in the repair queue. The
	// journal entry ID is the EndToEndId and the idempotency key
	// is "pain001-<MsgId>-<EndToEndId>", so the customer can
	// dedupe on it exactly like an exported transfer. The
	// transfer is single-currency by construction: ToCurrency
	// stays empty, so a cross-currency payment can never be
	// booked as one pain.001 instruction.
	receipt, err := l.PostTransfer(Transfer{
		ID:             d.EndToEndID,
		From:           from,
		To:             to,
		AmountCents:    d.AmountCents,
		Currency:       d.Currency,
		IdempotencyKey: "pain001-" + d.MsgID + "-" + d.EndToEndID,
	})
	if err != nil {
		return fail(err, RepairReasonInvalidFormat)
	}
	res.Receipt = receipt
	res.EntryID = receipt.TransferID
	if len(receipt.Entries) > 0 {
		res.EntryID = receipt.Entries[0].ID
	}
	res.Duplicate = receipt.Duplicate
	res.ReviewStatus = receipt.ReviewStatus
	return res
}

// pain001AppliedCurrency names the booked transactions' currency
// for the audit event: the single currency when every booked
// transaction agrees, else "MIXED" — a control total over mixed
// currencies is still reported honestly (the
// pain001DetailsCurrency convention).
func pain001AppliedCurrency(results []Pain001ItemResult) string {
	seen := map[string]bool{}
	for _, res := range results {
		if res.Err != nil {
			continue
		}
		seen[res.Instruction.Currency] = true
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

// ApplyPain001 books each parsed inbound pain.001.001.03 customer
// credit transfer initiation: it moves each instructed amount from
// the debtor account the wire IBAN resolves to, to the creditor
// account the wire IBAN resolves to, through PostTransfer, with
// the transaction's EndToEndId as both the journal entry ID and
// the idempotency key's tail, and today's UTC date as the
// execution day.
//
// The whole document is validated before anything applies; then
// each transaction applies on its own. A per-transaction failure
// — an execution date that is not today, an IBAN that resolves to
// no registered account, an empty currency, or any PostTransfer
// refusal (same-party, non-positive amount, frozen/overdraft,
// daily limit, accounting period, dual-control review) — is
// recorded in that transaction's result and parked in the repair
// queue, and never blocks the other transactions.
//
// A booked transaction registers the instruction's lifecycle
// record under the wire MsgId, so the LG-99 status machine and
// the LG-162 pain.002 answer path can follow this inbound file —
// and the batch emits one `pain001_initiation_applied` audit
// event (details msg_id / tx_count / ctrl_sum_cents / currency /
// rejected_count). A document that books nothing emits no batch
// event: the repair records are the trail.
//
// Idempotency is the MsgId with its terms: re-applying the same
// MsgId with the same transactions returns the stored outcome
// with Duplicate set, booking nothing new and emitting no second
// audit event. A MsgId reused for different terms — or one that
// already names an exported instruction — is refused with
// ErrPain001MsgIDConflict.
//
// Locking: the structural validation runs on the parsed values
// alone; the replay read runs under the read lock; each
// transaction's booking takes the write lock itself inside
// PostTransfer; the repair parking runs outside any held lock
// because EnqueueRepair takes l.mu itself; the instruction
// registration, the MsgId record, and the inbound audit event run
// under one short write lock of their own, after every booking is
// durable.
func (l *Ledger) ApplyPain001(doc Pain001Document, actor string) (Pain001ApplyResult, error) {
	if actor == "" {
		actor = "ApplyPain001"
	}
	if err := validatePain001Document(doc); err != nil {
		return Pain001ApplyResult{}, err
	}

	// MsgId replay: the initiation was already applied under
	// this MsgId. Same terms is a no-op returning the stored
	// outcome; different terms is a conflict.
	l.mu.RLock()
	rec, ok := l.pain001Applied[doc.MsgID]
	l.mu.RUnlock()
	if ok {
		if !pain001ItemsEqual(rec.Items, doc.Items) {
			return Pain001ApplyResult{}, fmt.Errorf("%w: MsgId %q", ErrPain001MsgIDConflict, doc.MsgID)
		}
		out := rec.Result
		out.Duplicate = true
		out.Items = append([]Pain001ItemResult(nil), rec.Result.Items...)
		for i := range out.Items {
			out.Items[i].Duplicate = true
		}
		return out, nil
	}

	// A MsgId that already names an exported instruction is a
	// conflict too: the export's record owns that MsgId, and the
	// inbound transfers must not silently join it.
	if _, ok := l.PainInstruction(doc.MsgID); ok {
		return Pain001ApplyResult{}, fmt.Errorf("%w: MsgId %q already names an exported instruction", ErrPain001MsgIDConflict, doc.MsgID)
	}

	results := make([]Pain001ItemResult, 0, len(doc.Items))
	for i := range doc.Items {
		results = append(results, l.applyPain001Item(doc.Items[i], actor))
	}

	// The booked transfers, for the instruction lifecycle
	// record: the pain.002 answer path joins on the wire MsgId
	// and the booked entry IDs. The IBANs are re-read from the
	// registry under the write lock — the same values the
	// per-item path resolved at booking time.
	l.mu.RLock()
	var payments []pain001Payment
	var ctrlSum int64
	for _, res := range results {
		if res.Err != nil {
			continue
		}
		pt, ok := l.painTransfers[res.Instruction.EndToEndID]
		if !ok {
			continue
		}
		debtorIBAN, debtorOK := l.accountIBANs[res.From]
		creditorIBAN, creditorOK := l.accountIBANs[res.To]
		if !debtorOK || !creditorOK {
			continue
		}
		payments = append(payments, pain001Payment{
			PainTransfer: pt,
			DebtorIBAN:   debtorIBAN,
			CreditorIBAN: creditorIBAN,
		})
		var okSum bool
		ctrlSum, okSum = addCents(ctrlSum, res.Instruction.AmountCents)
		if !okSum {
			l.mu.RUnlock()
			return Pain001ApplyResult{MsgID: doc.MsgID, Items: results}, ErrPain001AmountOverflow
		}
	}
	l.mu.RUnlock()

	out := Pain001ApplyResult{MsgID: doc.MsgID, Items: results}
	rejected := 0
	for _, res := range results {
		if res.Err != nil {
			rejected++
		}
	}
	l.mu.Lock()
	if len(payments) > 0 {
		l.registerPainInstructionLocked(doc.MsgID, payments, len(payments), ctrlSum, pain001AppliedCurrency(results))
	}
	if l.pain001Applied == nil {
		l.pain001Applied = make(map[string]pain001AppliedRecord)
	}
	l.pain001Applied[doc.MsgID] = pain001AppliedRecord{
		MsgID:  doc.MsgID,
		Items:  append([]Pain001CreditTransfer(nil), doc.Items...),
		Result: out,
	}
	if len(payments) > 0 {
		l.emitAudit(AuditEvent{
			Op:            "pain001_initiation_applied",
			Actor:         actor,
			TraceID:       doc.MsgID,
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Details: map[string]any{
				"msg_id":         doc.MsgID,
				"tx_count":       len(payments),
				"ctrl_sum_cents": ctrlSum,
				"currency":       pain001AppliedCurrency(results),
				"rejected_count": rejected,
			},
		})
	}
	l.mu.Unlock()
	return out, nil
}
