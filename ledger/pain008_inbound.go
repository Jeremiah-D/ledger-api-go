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

// ISO 20022 pain.008.001.02 customer direct debit initiation
// inbound processing (LG-428): the inbound half of the pain.008
// lifecycle, the mirror of the LG-166 export.
//
// ExportPain008 renders this ledger's own booked collections as the
// instruction file the creditor (the merchant) sends to its bank —
// the creditor side telling its bank "collect these". This module
// consumes the initiation file a creditor's customer sent to this
// ledger, acting as the creditor's bank: it books each instructed
// collection as a mandate debit, and nothing else. The two
// directions are orthogonal by construction:
//
//   - LG-166 exports: the ledger renders its own booked debits for
//     the bank (outbound, read-only).
//   - LG-428 imports: a customer file asks the ledger to book
//     collections (inbound, state-changing: one journal entry per
//     DrctDbtTxInf, plus the instruction lifecycle record).
//   - LG-406 answers: the bank's pain.002 verdict on an instruction
//     (inbound, state-changing: exported -> accepted | rejected).
//
// ParsePain008 builds new parse structs — the outbound export
// renders with strings.Builder, so there is no wire struct to
// reuse in reverse (the pain.009/pain.013 inbound convention,
// LG-422/LG-427). The parse structs mirror the export's rendered
// fields exactly (xmlns pain.008.001.02, GrpHdr MsgId/CreDtTm/
// NbOfTxs/CtrlSum, PmtInf PmtInfId/PmtMtd DD/NbOfTxs/CtrlSum/
// PmtTpInf (SvcLvl SEPA/LclInstrm CORE/SeqTp)/ReqdColltnDt/Cdtr/
// CdtrAcct/CdtrAgt/ChrgBr SLEV/CdtrSchmeId, DrctDbtTxInf PmtId
// (InstrId/EndToEndId)/InstdAmt/DrctDbtTx/MndtRltdInf (MndtId/
// DtOfSgntr/AmdmntInd)/DbtrAgt/Dbtr/DbtrAcct/RmtInf), so a document
// this ledger's export renders parses back; no sub-element spelling
// is invented beyond what pain008.go renders. The namespace rides
// the Document element, so a document in the wrong schema version
// fails the unmarshal — the version gate is the namespace, not a
// string check (the camt.056 inbound convention, LG-414).
//
// The document's fields, and what they route to:
//
//   - One mandate per file, like the export: a file naming two
//     mandates is parked whole in the repair queue, never silently
//     split into batches. The mandate is named per transaction
//     (DrctDbtTx/MndtRltdInf/MndtId); the file is single-mandate
//     when every transaction names the same one.
//   - CdtrSchmeId is required, non-empty, per PmtInf (SEPA
//     mandates it). The ledger stores no creditor-scheme registry,
//     so only non-emptiness is verified and the stated value rides
//     the audit event verbatim — the ExportPain008
//     ErrMissingPain008CreditorScheme idea, applied to the wire.
//   - ReqdColltnDt must be today's UTC date. The ledger has no
//     future-date collection scheduling, so a transaction dated
//     any other day parks in the repair queue instead of inventing
//     a holding position. The debit books at that date (UTC
//     midnight), so the mandate validity and frequency windows are
//     evaluated at the collection day.
//   - SeqTp: the wire value must be in the FRST/RCUR/FNAL/OOFF
//     vocabulary, but the booked sequence type is derived by
//     pain008SeqTp from the mandate's own debit history — the
//     ledger is the system of record, so it can state each
//     collection's position honestly. A wire value that disagrees
//     with the derived value parks that transaction in the repair
//     queue; it is never silently rewritten.
//   - The wire DbtrAcct/CdtrAcct IBANs must equal the mandate
//     payer/payee's registered IBANs (AccountIBAN, pain001.go).
//     A transaction naming any other IBAN parks in the repair
//     queue: the mandate names the legs, so a file that misnames
//     an account cannot misdirect the booking.
//   - Each transaction books through DebitWithMandate, so all of
//     its gates apply — unknown/non-active/expired mandate, amount
//     over the per-debit cap, currency mismatch, used frequency
//     window, and the full Post validation sequence. The
//     IdempotencyKey is "pain008-<MsgId>-<EndToEndId>" and the
//     journal entry ID is the EndToEndId, so the bank can dedupe
//     on it exactly like an exported collection.
//   - A booked transaction registers the instruction's lifecycle
//     record (registerPain008InstructionLocked) under the wire
//     MsgId, so LG-406's pain.002 answer path can answer this
//     inbound file: OrgnlMsgId is the wire MsgId, and each
//     EndToEndId is a booked entry ID. One LG-406 boundary stays:
//     ParsePain002 pairs the pain.008 name with the "pain008-"
//     MsgId namespace, so a bank report reaches this file through
//     the full parse path only when the wire MsgId carries that
//     prefix; ApplyPain002ToPain008 itself answers any registered
//     MsgId. The prefix gate is LG-406's family separation and is
//     not relaxed here.
//
// Fail-closed parking (the pacs.004 convention, LG-413): a
// transaction that fails any gate parks in the repair queue for
// operator resolution and never blocks the other transactions.
// A document the ledger cannot even parse is parked the same way
// by ParkPain008Unparsed. Repair records parked by this module
// are visibility parking, not resubmittable instructions: their
// Maker is deliberately empty, so the CreatePayout gate can never
// pass them.
//
// Idempotency is the GrpHdr MsgId with its terms (the pain.013/
// pain.014 convention, LG-427/LG-416): re-applying the same MsgId
// with the same transactions returns the stored outcome with
// Duplicate set, booking nothing new and emitting no second audit
// event. A MsgId reused for different terms is refused with
// ErrPain008MsgIDConflict — never silently overwritten. The
// per-transaction idempotency keys are a second layer: even if
// the in-memory replay record is lost (a restart), DebitWithMandate
// replays the original receipts instead of booking twice. A MsgId
// that already names an instruction the ledger exported is a
// conflict too: the export's record owns that MsgId, and the
// inbound collections must not silently join it.
//
// No pain.008 export change; no DebitWithMandate semantic change;
// no pain.002 answer change; no multi-mandate file; no future-date
// scheduling; no creditor-scheme registry invented.

// pain.008 inbound errors.
var (
	// ErrInvalidPain008 is returned by ParsePain008 for malformed
	// XML or a document missing a required field, and by
	// ApplyPain008 for a hand-built document that fails the same
	// gates: the customer sent something the ledger cannot even
	// read as a direct debit initiation. The HTTP layer parks it
	// in the repair queue and maps it to 400.
	ErrInvalidPain008 = errors.New("ledger: invalid pain.008 customer direct debit initiation")
	// ErrPain008MultiMandate is returned by ApplyPain008 when a
	// document names more than one mandate: the intake is
	// single-mandate, like the export, so the whole file parks in
	// the repair queue instead of being silently split. It is a
	// 422-class error.
	ErrPain008MultiMandate = errors.New("ledger: pain.008 inbound: document names more than one mandate")
	// ErrPain008MsgIDConflict is returned by ApplyPain008 when a
	// GrpHdr MsgId already applied carries different terms, or
	// already names an exported instruction: the message ID names
	// one initiation, and a second, different initiation under it
	// is refused rather than silently overwritten (the pain.013
	// ErrPain013MsgIDConflict convention). It is a 422-class
	// error.
	ErrPain008MsgIDConflict = errors.New("ledger: pain.008 inbound: message ID already applied with different terms")
	// ErrPain008CollectionDateNotToday is returned per transaction
	// when ReqdColltnDt is not today's UTC date: the ledger has
	// no future-date (or past-date) collection scheduling, so the
	// transaction parks in the repair queue.
	ErrPain008CollectionDateNotToday = errors.New("ledger: pain.008 inbound: collection date is not today (UTC)")
	// ErrPain008SeqTpMismatch is returned per transaction when
	// the wire SeqTp disagrees with the sequence type derived
	// from the mandate's debit history: the ledger is the system
	// of record, so the wire value is never silently rewritten —
	// the transaction parks in the repair queue.
	ErrPain008SeqTpMismatch = errors.New("ledger: pain.008 inbound: sequence type disagrees with mandate debit history")
	// ErrPain008IBANMismatch is returned per transaction when a
	// wire IBAN disagrees with the mandate payer/payee's
	// registered IBAN: the mandate names the legs, so a file that
	// misnames an account cannot misdirect the booking — the
	// transaction parks in the repair queue.
	ErrPain008IBANMismatch = errors.New("ledger: pain.008 inbound: account IBAN disagrees with mandate registration")
)

// Pain008DirectDebit is one parsed inbound pain.008 direct debit
// transaction: a DrctDbtTxInf with its PmtInf context (sequence
// type, collection date, creditor identity) attached, in document
// order.
type Pain008DirectDebit struct {
	// MsgID is the GrpHdr MsgId: the audit trace and the replay
	// key — one MsgId names one initiation.
	MsgID string `json:"msg_id"`
	// PmtInfID is PmtInf/PmtInfId, carried verbatim.
	PmtInfID string `json:"pmt_inf_id"`
	// SeqTp is PmtInf/PmtTpInf/SeqTp as the wire states it, one
	// of FRST/RCUR/FNAL/OOFF. The booked sequence type is the
	// pain008SeqTp derivation, never this value on trust.
	SeqTp string `json:"seq_tp"`
	// ReqdColltnDt is PmtInf/ReqdColltnDt: the collection date,
	// always today's UTC date for a booked transaction.
	ReqdColltnDt time.Time `json:"reqd_colltn_dt"`
	// CreditorName is PmtInf/Cdtr/Nm, CreditorIBAN is
	// PmtInf/CdtrAcct/Id/IBAN, CreditorBIC is
	// PmtInf/CdtrAgt/FinInstnId/BIC when stated, and
	// CreditorSchemeID is
	// PmtInf/CdtrSchmeId/Id/PrtryId/Id, required non-empty.
	// The service level and local instrument ride the PmtTpInf
	// verbatim when stated (the export renders SEPA/CORE).
	CreditorName     string `json:"creditor_name,omitempty"`
	CreditorIBAN     string `json:"creditor_iban"`
	CreditorBIC      string `json:"creditor_bic,omitempty"`
	CreditorSchemeID string `json:"creditor_scheme_id"`
	ServiceLevel     string `json:"service_level,omitempty"`
	LocalInstrument  string `json:"local_instrument,omitempty"`
	// InstrID is DrctDbtTxInf/PmtId/InstrId when stated, carried
	// verbatim; EndToEndID is PmtId/EndToEndId — the journal
	// entry ID and the per-transaction idempotency key's tail.
	InstrID    string `json:"instr_id,omitempty"`
	EndToEndID string `json:"end_to_end_id"`
	// AmountCents is DrctDbtTxInf/InstdAmt in minor units;
	// Currency is its Ccy attribute, normalized.
	AmountCents int64  `json:"amount_cents"`
	Currency    string `json:"currency"`
	// MandateID is DrctDbtTx/MndtRltdInf/MndtId: the mandate
	// the collection books under. DtOfSgntr is
	// MndtRltdInf/DtOfSgntr (YYYY-MM-DD) when stated.
	MandateID string `json:"mandate_id"`
	DtOfSgntr string `json:"dt_of_sgntr,omitempty"`
	// DebtorName is DrctDbtTxInf/Dbtr/Nm, DebtorIBAN is
	// DbtrAcct/Id/IBAN, DebtorBIC is
	// DbtrAgt/FinInstnId/BIC when stated.
	DebtorName string `json:"debtor_name,omitempty"`
	DebtorIBAN string `json:"debtor_iban"`
	DebtorBIC  string `json:"debtor_bic,omitempty"`
	// Remittance is DrctDbtTxInf/RmtInf/Ustrd when stated,
	// verbatim: the mandate debit's memo.
	Remittance string `json:"remittance,omitempty"`
}

// Pain008Document is one parsed inbound pain.008.001.02 customer
// direct debit initiation: the GrpHdr identity plus every direct
// debit transaction it carries, in document order.
type Pain008Document struct {
	// MsgID is the GrpHdr MsgId: the audit trace and the replay
	// key — one MsgId names one initiation.
	MsgID string `json:"msg_id"`
	// TxCount is the GrpHdr NbOfTxs as parsed (or the item count
	// when the header omits it); CtrlSumCents is the GrpHdr
	// CtrlSum in minor units. Both are re-checked by
	// validatePain008Document, so a hand-built document cannot
	// smuggle in a batch that disagrees with its own header.
	TxCount      int   `json:"tx_count"`
	CtrlSumCents int64 `json:"ctrl_sum_cents"`
	// Items are the DrctDbtTxInf transactions, in document
	// order, each carrying its PmtInf context.
	Items []Pain008DirectDebit `json:"items"`
}

// Pain008ItemResult is the per-transaction outcome of
// ApplyPain008: a transaction is either booked (Receipt set,
// EntryID set), an idempotent replay (Duplicate true), or
// rejected (Err set, RepairID set). One transaction's failure
// never blocks the others.
type Pain008ItemResult struct {
	// Instruction is the parsed transaction this result is about.
	Instruction Pain008DirectDebit `json:"instruction"`
	// Receipt is the DebitWithMandate receipt on success or
	// replay; empty when rejected.
	Receipt MandateDebitReceipt `json:"receipt,omitempty"`
	// EntryID is the booked entry's ID (the transaction's
	// EndToEndId), empty when rejected.
	EntryID string `json:"entry_id,omitempty"`
	// Duplicate reports an idempotent replay: the same
	// transaction was applied before, so the earlier entry is
	// returned and nothing new was booked — no new audit event.
	Duplicate bool `json:"duplicate"`
	// RepairID is the repair-queue record holding a rejected
	// transaction for operator resolution, empty on success.
	RepairID string `json:"repair_id,omitempty"`
	// DerivedSeqTp is the sequence type pain008SeqTp derived
	// from the mandate's debit history at booking time: the
	// booked value, carried so the instruction registration
	// records the same value the wire was checked against. It
	// is empty when the transaction was rejected.
	DerivedSeqTp string `json:"-"`
	// Err is the per-transaction rejection reason, nil on success.
	Err error `json:"-"`
}

// Pain008ApplyResult is the outcome of ApplyPain008.
type Pain008ApplyResult struct {
	// MsgID is the GrpHdr MsgId of the applied document.
	MsgID string `json:"msg_id"`
	// Items are the per-transaction outcomes, in document order.
	Items []Pain008ItemResult `json:"items"`
	// RepairID is set on the parked fail-closed path for the
	// whole batch (a multi-mandate file): the repair-queue
	// record holding the file for operator resolution.
	RepairID string `json:"repair_id,omitempty"`
	// Duplicate reports whether the whole document was already
	// applied under the same MsgId with the same terms: a no-op
	// success, not an error.
	Duplicate bool `json:"duplicate,omitempty"`
}

// pain008AppliedRecord is the replay record for one applied
// pain.008 initiation, keyed by GrpHdr MsgId in pain008Applied:
// the transaction terms it carried and the apply outcome, so a
// replay returns the same outcome without re-booking or
// re-parking.
type pain008AppliedRecord struct {
	MsgID  string
	Items  []Pain008DirectDebit
	Result Pain008ApplyResult
}

// pain008ItemsEqual reports whether two transaction lists carry
// exactly the same terms, in document order: the same EndToEndId,
// mandate, amount and currency, collection date, sequence type,
// parties (by normalized IBAN), creditor scheme, and remittance.
// The collection-date comparison uses Equal — the wire carries a
// YYYY-MM-DD date, and both sides are UTC midnights.
func pain008ItemsEqual(a, b []Pain008DirectDebit) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if x.PmtInfID != y.PmtInfID ||
			x.SeqTp != y.SeqTp ||
			!x.ReqdColltnDt.Equal(y.ReqdColltnDt) ||
			x.CreditorName != y.CreditorName ||
			normalizeIBAN(x.CreditorIBAN) != normalizeIBAN(y.CreditorIBAN) ||
			x.CreditorBIC != y.CreditorBIC ||
			x.CreditorSchemeID != y.CreditorSchemeID ||
			x.ServiceLevel != y.ServiceLevel ||
			x.LocalInstrument != y.LocalInstrument ||
			x.InstrID != y.InstrID ||
			x.EndToEndID != y.EndToEndID ||
			x.AmountCents != y.AmountCents ||
			x.Currency != y.Currency ||
			x.MandateID != y.MandateID ||
			x.DtOfSgntr != y.DtOfSgntr ||
			x.DebtorName != y.DebtorName ||
			normalizeIBAN(x.DebtorIBAN) != normalizeIBAN(y.DebtorIBAN) ||
			x.DebtorBIC != y.DebtorBIC ||
			x.Remittance != y.Remittance {
			return false
		}
	}
	return true
}

// pain008ValidSeqTp reports whether s is in the pain.008 SeqTp
// vocabulary (the Pain008Seq* constants in pain008.go).
func pain008ValidSeqTp(s string) bool {
	switch s {
	case Pain008SeqFRST, Pain008SeqRCUR, Pain008SeqFNAL, Pain008SeqOOFF:
		return true
	}
	return false
}

// The pain008x* structs are the inbound-side XML model for
// pain.008.001.02: the outbound export renders with
// strings.Builder, so this is the first struct form of the
// message — shaped to match the export field for field
// (GrpHdr/MsgId/CreDtTm/NbOfTxs/CtrlSum/InitgPty, PmtInf/
// PmtInfId/PmtMtd/NbOfTxs/CtrlSum/PmtTpInf (SvcLvl/LclInstrm/
// SeqTp)/ReqdColltnDt/Cdtr/CdtrAcct/CdtrAgt/ChrgBr/CdtrSchmeId,
// DrctDbtTxInf/PmtId/InstdAmt/DrctDbtTx/MndtRltdInf/DbtrAgt/
// Dbtr/DbtrAcct/RmtInf) so a document this ledger's export
// renders parses back. The namespace on Document gates the schema
// version.
type pain008xDocument struct {
	XMLName xml.Name      `xml:"urn:iso:std:iso:20022:tech:xsd:pain.008.001.02 Document"`
	Initn   pain008xInitn `xml:"CstmrDrctDbtInitn"`
}

type pain008xInitn struct {
	GrpHdr  pain008xGrpHdr   `xml:"GrpHdr"`
	PmtInfs []pain008xPmtInf `xml:"PmtInf"`
}

type pain008xGrpHdr struct {
	MsgID   string  `xml:"MsgId"`
	CreDtTm string  `xml:"CreDtTm"`
	NbOfTxs *int    `xml:"NbOfTxs"`
	CtrlSum *string `xml:"CtrlSum"`
}

type pain008xPmtInf struct {
	PmtInfID     string              `xml:"PmtInfId"`
	PmtMtd       string              `xml:"PmtMtd"`
	NbOfTxs      *int                `xml:"NbOfTxs"`
	CtrlSum      *string             `xml:"CtrlSum"`
	PmtTpInf     pain008xPmtTpInf    `xml:"PmtTpInf"`
	ReqdColltnDt string              `xml:"ReqdColltnDt"`
	Cdtr         pain008xParty       `xml:"Cdtr"`
	CdtrAcct     pain008xAcct        `xml:"CdtrAcct"`
	CdtrAgt      *pain008xAgt        `xml:"CdtrAgt,omitempty"`
	ChrgBr       string              `xml:"ChrgBr"`
	CdtrSchmeId  pain008xCdtrSchmeID `xml:"CdtrSchmeId"`
	Txs          []pain008xTx        `xml:"DrctDbtTxInf"`
}

type pain008xPmtTpInf struct {
	SvcLvl    pain008xCd `xml:"SvcLvl"`
	LclInstrm pain008xCd `xml:"LclInstrm"`
	SeqTp     string     `xml:"SeqTp"`
}

type pain008xCd struct {
	Cd string `xml:"Cd"`
}

type pain008xCdtrSchmeID struct {
	ID pain008xSchmeID `xml:"Id"`
}

type pain008xSchmeID struct {
	PrtryID pain008xPrtryID `xml:"PrtryId"`
}

type pain008xPrtryID struct {
	ID string `xml:"Id"`
}

type pain008xTx struct {
	PmtID     pain008xPmtID     `xml:"PmtId"`
	InstdAmt  pain008xAmt       `xml:"InstdAmt"`
	DrctDbtTx pain008xDrctDbtTx `xml:"DrctDbtTx"`
	DbtrAgt   *pain008xAgt      `xml:"DbtrAgt,omitempty"`
	Dbtr      pain008xParty     `xml:"Dbtr"`
	DbtrAcct  pain008xAcct      `xml:"DbtrAcct"`
	RmtInf    pain008xRmtInf    `xml:"RmtInf"`
}

type pain008xPmtID struct {
	InstrID    string `xml:"InstrId"`
	EndToEndID string `xml:"EndToEndId"`
}

type pain008xAmt struct {
	Ccy   string `xml:"Ccy,attr"`
	Value string `xml:",chardata"`
}

type pain008xDrctDbtTx struct {
	MndtRltdInf pain008xMndtRltdInf `xml:"MndtRltdInf"`
}

type pain008xMndtRltdInf struct {
	MndtID    string `xml:"MndtId"`
	DtOfSgntr string `xml:"DtOfSgntr"`
	AmdmntInd string `xml:"AmdmntInd"`
}

type pain008xParty struct {
	Nm string `xml:"Nm"`
}

type pain008xAcct struct {
	ID pain008xAcctID `xml:"Id"`
}

type pain008xAcctID struct {
	IBAN string `xml:"IBAN"`
}

type pain008xAgt struct {
	FinInstnId pain008xFinInstnID `xml:"FinInstnId"`
}

type pain008xFinInstnID struct {
	BIC string `xml:"BIC"`
}

type pain008xRmtInf struct {
	Ustrd string `xml:"Ustrd"`
}

// pain008ParseAmount parses an ISO 20022 decimal amount
// ("1234.56") into minor units, the pacs010ParseAmount rules:
// the integer part holds at most 12 digits, the fraction at most
// two digits and is right-padded ("5" means 50 cents). A signed
// or malformed amount is a parse refusal — an amount the ledger
// cannot read exactly is never rounded into a booking, and a
// non-positive amount is refused by validatePain008Document, not
// smuggled through the parser.
func pain008ParseAmount(body string) (int64, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return 0, fmt.Errorf("%w: empty amount", ErrInvalidPain008)
	}
	intPart := body
	fracPart := ""
	if i := strings.IndexByte(body, '.'); i >= 0 {
		intPart, fracPart = body[:i], body[i+1:]
		if strings.IndexByte(fracPart, '.') >= 0 {
			return 0, fmt.Errorf("%w: amount %q has two decimal points", ErrInvalidPain008, body)
		}
	}
	if intPart == "" || len(intPart) > 12 || len(fracPart) > 2 {
		return 0, fmt.Errorf("%w: amount %q", ErrInvalidPain008, body)
	}
	for _, c := range intPart + fracPart {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("%w: amount %q is not numeric", ErrInvalidPain008, body)
		}
	}
	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: amount %q", ErrInvalidPain008, body)
	}
	frac := int64(0)
	if fracPart != "" {
		if len(fracPart) == 1 {
			fracPart += "0"
		}
		frac, err = strconv.ParseInt(fracPart, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%w: amount %q", ErrInvalidPain008, body)
		}
	}
	return whole*100 + frac, nil
}

// validatePain008Document fail-closes on a document ParsePain008
// would refuse, so ApplyPain008 stays safe on hand-built
// documents too: the parse gate and the apply gate share one
// check, never a copy. The business gates (collection date is
// today, SeqTp matches the mandate history, IBANs match the
// mandate registration, the mandate gates inside
// DebitWithMandate) are deliberately NOT here: they are
// per-transaction apply decisions, and a refusal parks that
// transaction in the repair queue instead of refusing the file.
func validatePain008Document(doc Pain008Document) error {
	if strings.TrimSpace(doc.MsgID) == "" {
		return fmt.Errorf("%w: missing GrpHdr/MsgId", ErrInvalidPain008)
	}
	if len(doc.Items) == 0 {
		return fmt.Errorf("%w: no DrctDbtTxInf", ErrInvalidPain008)
	}
	if doc.TxCount != len(doc.Items) {
		return fmt.Errorf("%w: GrpHdr/NbOfTxs %d disagrees with %d DrctDbtTxInf", ErrInvalidPain008, doc.TxCount, len(doc.Items))
	}
	var sum int64
	for _, d := range doc.Items {
		if strings.TrimSpace(d.EndToEndID) == "" {
			return fmt.Errorf("%w: missing DrctDbtTxInf/PmtId/EndToEndId", ErrInvalidPain008)
		}
		if strings.TrimSpace(d.MandateID) == "" {
			return fmt.Errorf("%w: missing DrctDbtTx/MndtRltdInf/MndtId", ErrInvalidPain008)
		}
		if d.AmountCents <= 0 {
			return fmt.Errorf("%w: non-positive instructed amount", ErrInvalidPain008)
		}
		if _, err := normalizeCurrency(d.Currency); err != nil {
			return fmt.Errorf("%w: currency %q", ErrInvalidPain008, d.Currency)
		}
		if strings.TrimSpace(d.PmtInfID) == "" {
			return fmt.Errorf("%w: missing PmtInf/PmtInfId", ErrInvalidPain008)
		}
		if !pain008ValidSeqTp(d.SeqTp) {
			return fmt.Errorf("%w: PmtInf/PmtTpInf/SeqTp %q outside FRST/RCUR/FNAL/OOFF", ErrInvalidPain008, d.SeqTp)
		}
		if d.ReqdColltnDt.IsZero() {
			return fmt.Errorf("%w: missing PmtInf/ReqdColltnDt", ErrInvalidPain008)
		}
		if strings.TrimSpace(d.CreditorSchemeID) == "" {
			return fmt.Errorf("%w: missing PmtInf/CdtrSchmeId", ErrInvalidPain008)
		}
		if strings.TrimSpace(d.CreditorIBAN) == "" {
			return fmt.Errorf("%w: missing PmtInf/CdtrAcct/Id/IBAN", ErrInvalidPain008)
		}
		if strings.TrimSpace(d.DebtorIBAN) == "" {
			return fmt.Errorf("%w: missing DrctDbtTxInf/DbtrAcct/Id/IBAN", ErrInvalidPain008)
		}
		if d.DtOfSgntr != "" {
			if _, err := time.Parse("2006-01-02", d.DtOfSgntr); err != nil {
				return fmt.Errorf("%w: mandate signature date %q must be YYYY-MM-DD", ErrInvalidPain008, d.DtOfSgntr)
			}
		}
		var ok bool
		sum, ok = addCents(sum, d.AmountCents)
		if !ok {
			return ErrPain008AmountOverflow
		}
	}
	if sum != doc.CtrlSumCents {
		return fmt.Errorf("%w: GrpHdr/CtrlSum %d disagrees with instructed total %d", ErrInvalidPain008, doc.CtrlSumCents, sum)
	}
	return nil
}

// ParsePain008 parses one inbound pain.008.001.02 customer direct
// debit initiation into a Pain008Document. It validates the whole
// document before returning anything: a missing MsgId, a PmtInf
// without an ID or with a PmtMtd other than DD, a SeqTp outside
// the FRST/RCUR/FNAL/OOFF vocabulary, a missing creditor scheme
// ID, a missing or malformed collection date, a NbOfTxs or
// CtrlSum that disagrees with the transactions (a missing GrpHdr
// CtrlSum is refused the same way — an unverifiable batch is
// never applied on trust), no transactions at all, or any
// transaction with a missing EndToEndId or mandate ID, an
// unreadable or non-positive amount, an empty or malformed
// currency, or a missing debtor or creditor IBAN are all parse
// refusals — ApplyPain008 never sees a half-read document. The
// party names, the agent BICs, the service level, the local
// instrument, the charge bearer, and the amendment indicator are
// carried verbatim when stated and never gated: the mandate names
// the legs, not the names. A collection date that is not today, a
// SeqTp that disagrees with the mandate history, and an IBAN that
// disagrees with the mandate registration all parse fine: they
// are per-transaction apply refusals, parked by ApplyPain008.
func ParsePain008(data []byte) (Pain008Document, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return Pain008Document{}, fmt.Errorf("%w: empty document", ErrInvalidPain008)
	}
	var doc pain008xDocument
	if err := xml.Unmarshal(data, &doc); err != nil {
		return Pain008Document{}, fmt.Errorf("%w: %v", ErrInvalidPain008, err)
	}
	msgID := strings.TrimSpace(doc.Initn.GrpHdr.MsgID)
	if msgID == "" {
		return Pain008Document{}, fmt.Errorf("%w: missing GrpHdr/MsgId", ErrInvalidPain008)
	}
	if len(doc.Initn.PmtInfs) == 0 {
		return Pain008Document{}, fmt.Errorf("%w: no PmtInf", ErrInvalidPain008)
	}
	items := make([]Pain008DirectDebit, 0)
	var sum int64
	for _, pmt := range doc.Initn.PmtInfs {
		pmtInfID := strings.TrimSpace(pmt.PmtInfID)
		if pmtInfID == "" {
			return Pain008Document{}, fmt.Errorf("%w: missing PmtInf/PmtInfId", ErrInvalidPain008)
		}
		if strings.TrimSpace(pmt.PmtMtd) != "DD" {
			return Pain008Document{}, fmt.Errorf("%w: PmtInf/PmtMtd %q is not DD", ErrInvalidPain008, strings.TrimSpace(pmt.PmtMtd))
		}
		seqTp := strings.TrimSpace(pmt.PmtTpInf.SeqTp)
		if !pain008ValidSeqTp(seqTp) {
			return Pain008Document{}, fmt.Errorf("%w: PmtInf/PmtTpInf/SeqTp %q outside FRST/RCUR/FNAL/OOFF", ErrInvalidPain008, seqTp)
		}
		rawColltnDt := strings.TrimSpace(pmt.ReqdColltnDt)
		if rawColltnDt == "" {
			return Pain008Document{}, fmt.Errorf("%w: missing PmtInf/ReqdColltnDt", ErrInvalidPain008)
		}
		colltnDt, err := time.Parse("2006-01-02", rawColltnDt)
		if err != nil {
			return Pain008Document{}, fmt.Errorf("%w: collection date %q must be YYYY-MM-DD", ErrInvalidPain008, rawColltnDt)
		}
		schemeID := strings.TrimSpace(pmt.CdtrSchmeId.ID.PrtryID.ID)
		if schemeID == "" {
			return Pain008Document{}, fmt.Errorf("%w: missing PmtInf/CdtrSchmeId", ErrInvalidPain008)
		}
		creditorIBAN := normalizeIBAN(strings.TrimSpace(pmt.CdtrAcct.ID.IBAN))
		if creditorIBAN == "" {
			return Pain008Document{}, fmt.Errorf("%w: missing PmtInf/CdtrAcct/Id/IBAN", ErrInvalidPain008)
		}
		creditorBIC := ""
		if pmt.CdtrAgt != nil {
			creditorBIC = strings.TrimSpace(pmt.CdtrAgt.FinInstnId.BIC)
		}
		if len(pmt.Txs) == 0 {
			return Pain008Document{}, fmt.Errorf("%w: PmtInf %q carries no DrctDbtTxInf", ErrInvalidPain008, pmtInfID)
		}
		if pmt.NbOfTxs != nil && *pmt.NbOfTxs != len(pmt.Txs) {
			return Pain008Document{}, fmt.Errorf("%w: PmtInf/NbOfTxs %d disagrees with %d DrctDbtTxInf", ErrInvalidPain008, *pmt.NbOfTxs, len(pmt.Txs))
		}
		var pmtSum int64
		for _, x := range pmt.Txs {
			endToEndID := strings.TrimSpace(x.PmtID.EndToEndID)
			if endToEndID == "" {
				return Pain008Document{}, fmt.Errorf("%w: missing DrctDbtTxInf/PmtId/EndToEndId", ErrInvalidPain008)
			}
			mandateID := strings.TrimSpace(x.DrctDbtTx.MndtRltdInf.MndtID)
			if mandateID == "" {
				return Pain008Document{}, fmt.Errorf("%w: missing DrctDbtTx/MndtRltdInf/MndtId", ErrInvalidPain008)
			}
			debtorIBAN := normalizeIBAN(strings.TrimSpace(x.DbtrAcct.ID.IBAN))
			if debtorIBAN == "" {
				return Pain008Document{}, fmt.Errorf("%w: missing DrctDbtTxInf/DbtrAcct/Id/IBAN", ErrInvalidPain008)
			}
			if x.InstdAmt.Value == "" && strings.TrimSpace(x.InstdAmt.Ccy) == "" {
				return Pain008Document{}, fmt.Errorf("%w: missing DrctDbtTxInf/InstdAmt", ErrInvalidPain008)
			}
			cents, err := pain008ParseAmount(x.InstdAmt.Value)
			if err != nil {
				return Pain008Document{}, err
			}
			ccy := strings.TrimSpace(x.InstdAmt.Ccy)
			if ccy == "" {
				return Pain008Document{}, fmt.Errorf("%w: empty DrctDbtTxInf/InstdAmt currency", ErrInvalidPain008)
			}
			currency, err := normalizeCurrency(ccy)
			if err != nil {
				return Pain008Document{}, fmt.Errorf("%w: currency %q", ErrInvalidPain008, ccy)
			}
			dtOfSgntr := strings.TrimSpace(x.DrctDbtTx.MndtRltdInf.DtOfSgntr)
			if dtOfSgntr != "" {
				if _, err := time.Parse("2006-01-02", dtOfSgntr); err != nil {
					return Pain008Document{}, fmt.Errorf("%w: mandate signature date %q must be YYYY-MM-DD", ErrInvalidPain008, dtOfSgntr)
				}
			}
			debtorBIC := ""
			if x.DbtrAgt != nil {
				debtorBIC = strings.TrimSpace(x.DbtrAgt.FinInstnId.BIC)
			}
			items = append(items, Pain008DirectDebit{
				MsgID:            msgID,
				PmtInfID:         pmtInfID,
				SeqTp:            seqTp,
				ReqdColltnDt:     colltnDt,
				CreditorName:     strings.TrimSpace(pmt.Cdtr.Nm),
				CreditorIBAN:     creditorIBAN,
				CreditorBIC:      creditorBIC,
				CreditorSchemeID: schemeID,
				ServiceLevel:     strings.TrimSpace(pmt.PmtTpInf.SvcLvl.Cd),
				LocalInstrument:  strings.TrimSpace(pmt.PmtTpInf.LclInstrm.Cd),
				InstrID:          strings.TrimSpace(x.PmtID.InstrID),
				EndToEndID:       endToEndID,
				AmountCents:      cents,
				Currency:         currency,
				MandateID:        mandateID,
				DtOfSgntr:        dtOfSgntr,
				DebtorName:       strings.TrimSpace(x.Dbtr.Nm),
				DebtorIBAN:       debtorIBAN,
				DebtorBIC:        debtorBIC,
				Remittance:       strings.TrimSpace(x.RmtInf.Ustrd),
			})
			var ok bool
			pmtSum, ok = addCents(pmtSum, cents)
			if !ok {
				return Pain008Document{}, ErrPain008AmountOverflow
			}
		}
		if pmt.CtrlSum != nil {
			ctrlSum, err := pain008ParseAmount(*pmt.CtrlSum)
			if err != nil {
				return Pain008Document{}, fmt.Errorf("%w: PmtInf/CtrlSum %q", ErrInvalidPain008, strings.TrimSpace(*pmt.CtrlSum))
			}
			if ctrlSum != pmtSum {
				return Pain008Document{}, fmt.Errorf("%w: PmtInf/CtrlSum %d disagrees with instructed total %d", ErrInvalidPain008, ctrlSum, pmtSum)
			}
		}
		var ok bool
		sum, ok = addCents(sum, pmtSum)
		if !ok {
			return Pain008Document{}, ErrPain008AmountOverflow
		}
	}
	grp := doc.Initn.GrpHdr
	if grp.NbOfTxs != nil && *grp.NbOfTxs != len(items) {
		return Pain008Document{}, fmt.Errorf("%w: GrpHdr/NbOfTxs %d disagrees with %d DrctDbtTxInf", ErrInvalidPain008, *grp.NbOfTxs, len(items))
	}
	if grp.CtrlSum == nil {
		return Pain008Document{}, fmt.Errorf("%w: missing GrpHdr/CtrlSum", ErrInvalidPain008)
	}
	ctrlSum, err := pain008ParseAmount(*grp.CtrlSum)
	if err != nil {
		return Pain008Document{}, fmt.Errorf("%w: control sum %q", ErrInvalidPain008, strings.TrimSpace(*grp.CtrlSum))
	}
	if ctrlSum != sum {
		return Pain008Document{}, fmt.Errorf("%w: GrpHdr/CtrlSum %d disagrees with instructed total %d", ErrInvalidPain008, ctrlSum, sum)
	}
	return Pain008Document{
		MsgID:        msgID,
		TxCount:      len(items),
		CtrlSumCents: sum,
		Items:        items,
	}, nil
}

// pain008LenientDocument is the namespace-agnostic wire shape
// ParkPain008Unparsed reads: a document ParsePain008 refused
// for its version may still name the initiation, and the namespace
// is the only thing the strict parse gates on. Unknown elements
// are skipped, the camt.029 convention.
type pain008LenientDocument struct {
	XMLName xml.Name            `xml:"Document"`
	Initn   pain008LenientInitn `xml:"CstmrDrctDbtInitn"`
}

type pain008LenientInitn struct {
	GrpHdr pain008LenientGrpHdr `xml:"GrpHdr"`
}

type pain008LenientGrpHdr struct {
	MsgID string `xml:"MsgId"`
}

// ParkPain008Unparsed best-effort parks a pain.008 document
// ParsePain008 refused, when the document still names an
// initiation: the GrpHdr MsgId it carries. It returns the repair
// record ID, or "" when the document names nothing parkable
// (garbage bytes, an empty shell): there is no initiation for the
// operator to find, so there is nothing to park. A parking failure
// (a full queue) is returned so the caller can fail loud rather
// than silently drop the customer's message. The reason code is
// invalid_format — the document was present but unreadable as a
// customer direct debit initiation.
func (l *Ledger) ParkPain008Unparsed(data []byte, actor string) (string, error) {
	var doc pain008LenientDocument
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
		Destination: "pain008-inbound:" + id,
	}, RepairReasonInvalidFormat, actor)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// parkPain008Document parks a parsed pain.008 document that failed
// closed as a whole (a multi-mandate file), and returns the repair
// record ID. The snapshot's Maker is deliberately empty (see the
// package comment).
func (l *Ledger) parkPain008Document(doc Pain008Document, reasonCode, actor string) (string, error) {
	rec, err := l.EnqueueRepair(PayoutInstruction{
		ID: doc.MsgID,
		// Maker intentionally empty: see the package comment.
		Destination: "pain008-inbound:" + doc.MsgID,
	}, reasonCode, actor)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// parkPain008Item parks one parsed pain.008 transaction that failed
// closed, and returns the repair record ID. The snapshot's ID is
// the transaction's idempotency key, so the parked item is
// traceable to the exact debit it refused; its Maker is
// deliberately empty (see the package comment).
func (l *Ledger) parkPain008Item(d Pain008DirectDebit, reasonCode, actor string) (string, error) {
	rec, err := l.EnqueueRepair(PayoutInstruction{
		ID:          "pain008-" + d.MsgID + "-" + d.EndToEndID,
		AmountCents: d.AmountCents,
		Currency:    d.Currency,
		// Maker intentionally empty: see the package comment.
		Destination: "pain008-inbound:" + d.MsgID,
	}, reasonCode, actor)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// applyPain008Item books one parsed transaction through
// DebitWithMandate and records the per-transaction outcome.
//
// The gates run in order, each fail-closed into the repair queue:
// the mandate must exist; the collection date must be today's UTC
// date; the wire SeqTp must match the sequence type derived from
// the mandate's debit history; the wire IBANs must equal the
// mandate payer/payee's registered IBANs; and DebitWithMandate's
// own gates (status, validity window, per-debit cap, currency,
// frequency window, and the full Post validation sequence) apply
// last. The mandate, its prior debit count, and the registered
// IBANs are read under the read lock; the booking itself runs
// outside it, because DebitWithMandate takes the write lock — the
// frequency gate inside the booking is the atomic guard against
// two collections racing for one window.
func (l *Ledger) applyPain008Item(d Pain008DirectDebit, actor string) Pain008ItemResult {
	res := Pain008ItemResult{Instruction: d}
	fail := func(err error, reasonCode string) Pain008ItemResult {
		res.Err = err
		repairID, parkErr := l.parkPain008Item(d, reasonCode, actor)
		if parkErr != nil {
			res.Err = fmt.Errorf("%w (repair parking failed: %v)", err, parkErr)
			return res
		}
		res.RepairID = repairID
		return res
	}

	// The mandate, its prior debit count, and the registered
	// IBANs, under the read lock.
	l.mu.RLock()
	m, ok := l.mandates[d.MandateID]
	priorDebits := len(l.mandateDebits[d.MandateID])
	debtorIBAN, debtorOK := l.accountIBANs[m.Payer]
	creditorIBAN, creditorOK := l.accountIBANs[m.Payee]
	l.mu.RUnlock()
	if !ok {
		return fail(fmt.Errorf("%w: %q", ErrMandateNotFound, d.MandateID), RepairReasonUnknownRoute)
	}

	// The collection date is today's UTC date — the ledger has no
	// future-date (or past-date) collection scheduling.
	today := time.Now().UTC().Format("2006-01-02")
	if d.ReqdColltnDt.UTC().Format("2006-01-02") != today {
		return fail(fmt.Errorf("%w: ReqdColltnDt %q is not today (UTC)", ErrPain008CollectionDateNotToday, d.ReqdColltnDt.UTC().Format("2006-01-02")), RepairReasonInvalidFormat)
	}

	// The booked sequence type comes from the mandate's own debit
	// history, never from the wire on trust.
	derivedSeqTp := pain008SeqTp(m, priorDebits, d.ReqdColltnDt)
	if d.SeqTp != derivedSeqTp {
		return fail(fmt.Errorf("%w: wire %q, derived %q", ErrPain008SeqTpMismatch, d.SeqTp, derivedSeqTp), RepairReasonInvalidFormat)
	}

	// The wire IBANs must equal the mandate's registered IBANs.
	if !debtorOK || normalizeIBAN(d.DebtorIBAN) != debtorIBAN.IBAN {
		return fail(fmt.Errorf("%w: debtor IBAN %q is not the mandate payer's registered IBAN", ErrPain008IBANMismatch, d.DebtorIBAN), RepairReasonUnknownRoute)
	}
	if !creditorOK || normalizeIBAN(d.CreditorIBAN) != creditorIBAN.IBAN {
		return fail(fmt.Errorf("%w: creditor IBAN %q is not the mandate payee's registered IBAN", ErrPain008IBANMismatch, d.CreditorIBAN), RepairReasonUnknownRoute)
	}

	// Booking reuses DebitWithMandate: its gates (status, validity
	// window, per-debit cap, currency, frequency window, and the
	// full Post validation sequence) apply, and a refusal parks
	// the transaction in the repair queue. The journal entry ID is
	// the EndToEndId and the idempotency key is
	// "pain008-<MsgId>-<EndToEndId>", so the bank can dedupe on
	// it exactly like an exported collection.
	receipt, err := l.DebitWithMandate(d.MandateID, MandateDebit{
		ID:             d.EndToEndID,
		AmountCents:    d.AmountCents,
		Currency:       d.Currency,
		Memo:           d.Remittance,
		IdempotencyKey: "pain008-" + d.MsgID + "-" + d.EndToEndID,
		CreatedAt:      d.ReqdColltnDt,
	})
	if err != nil {
		return fail(err, RepairReasonInvalidFormat)
	}
	res.Receipt = receipt
	res.EntryID = receipt.Entry.ID
	res.Duplicate = receipt.Duplicate
	res.DerivedSeqTp = derivedSeqTp
	return res
}

// ApplyPain008 books each parsed inbound pain.008.001.02 customer
// direct debit initiation: it pulls each instructed amount from
// the mandate's payer to the mandate's payee through
// DebitWithMandate, with the transaction's EndToEndId as both the
// journal entry ID and the idempotency key's tail, and the
// requested collection date (today, UTC) as the booking
// timestamp, so the mandate's validity window is evaluated at the
// day the creditor's bank was asked to collect.
//
// The whole document is validated before anything applies; then
// each transaction applies on its own. A per-transaction failure
// — unknown mandate, a collection date that is not today, a SeqTp
// that disagrees with the mandate history, an IBAN that disagrees
// with the mandate registration, or any DebitWithMandate refusal
// (non-active mandate, a date outside the mandate window, an
// amount over the mandate cap, a currency mismatch, a used
// frequency window) — is recorded in that transaction's result
// and parked in the repair queue, and never blocks the other
// transactions. A file naming more than one mandate is parked
// whole instead: the intake is single-mandate, like the export.
//
// A booked transaction registers the instruction's lifecycle
// record under the wire MsgId, so LG-406's pain.002 answer path
// can answer this inbound file — and the batch emits one
// `pain008_initiation_applied` audit event (details msg_id /
// mandate_id / tx_count / ctrl_sum_cents / creditor_scheme_id,
// the scheme value verbatim). A document that books nothing
// emits no batch event: the repair records are the trail.
//
// Idempotency is the MsgId with its terms: re-applying the same
// MsgId with the same transactions returns the stored outcome
// with Duplicate set, booking nothing new and emitting no second
// audit event. A MsgId reused for different terms — or one that
// already names an exported instruction — is refused with
// ErrPain008MsgIDConflict.
//
// Locking: the structural validation runs on the parsed values
// alone; the replay read runs under the read lock; each
// transaction's booking takes the write lock itself inside
// DebitWithMandate; the repair parking runs outside any held lock
// because EnqueueRepair takes l.mu itself; the instruction
// registration, the MsgId record, and the inbound audit event run
// under one short write lock of their own, after every booking is
// durable.
func (l *Ledger) ApplyPain008(doc Pain008Document, actor string) (Pain008ApplyResult, error) {
	if actor == "" {
		actor = "ApplyPain008"
	}
	if err := validatePain008Document(doc); err != nil {
		return Pain008ApplyResult{}, err
	}

	// MsgId replay: the initiation was already applied under
	// this MsgId. Same terms is a no-op returning the stored
	// outcome; different terms is a conflict.
	l.mu.RLock()
	rec, ok := l.pain008Applied[doc.MsgID]
	l.mu.RUnlock()
	if ok {
		if !pain008ItemsEqual(rec.Items, doc.Items) {
			return Pain008ApplyResult{}, fmt.Errorf("%w: MsgId %q", ErrPain008MsgIDConflict, doc.MsgID)
		}
		out := rec.Result
		out.Duplicate = true
		out.Items = append([]Pain008ItemResult(nil), rec.Result.Items...)
		for i := range out.Items {
			out.Items[i].Duplicate = true
		}
		return out, nil
	}

	// A MsgId that already names an exported instruction is a
	// conflict too: the export's record owns that MsgId, and the
	// inbound collections must not silently join it.
	if _, ok := l.Pain008Instruction(doc.MsgID); ok {
		return Pain008ApplyResult{}, fmt.Errorf("%w: MsgId %q already names an exported instruction", ErrPain008MsgIDConflict, doc.MsgID)
	}

	// Single-mandate intake: a file naming two mandates parks
	// whole in the repair queue, never silently split.
	mandateID := doc.Items[0].MandateID
	for _, d := range doc.Items[1:] {
		if d.MandateID != mandateID {
			repairID, parkErr := l.parkPain008Document(doc, RepairReasonInvalidFormat, actor)
			if parkErr != nil {
				return Pain008ApplyResult{}, fmt.Errorf("%w (repair parking failed: %v)", ErrPain008MultiMandate, parkErr)
			}
			return Pain008ApplyResult{MsgID: doc.MsgID, RepairID: repairID}, fmt.Errorf("%w: mandates %q and %q", ErrPain008MultiMandate, mandateID, d.MandateID)
		}
	}

	results := make([]Pain008ItemResult, 0, len(doc.Items))
	for i := range doc.Items {
		results = append(results, l.applyPain008Item(doc.Items[i], actor))
	}

	// The booked collections, for the instruction lifecycle
	// record: the pain.002 answer path joins on the wire MsgId
	// and the booked entry IDs. The sequence type is the one the
	// per-item path derived from the mandate history at booking
	// time — never re-derived here, where the prior-debit count
	// has already moved — and the currency is the booked
	// entries' own currency (every booked entry matches the
	// mandate currency, so they all agree).
	l.mu.RLock()
	m := l.mandates[mandateID]
	debtorIBAN := l.accountIBANs[m.Payer]
	creditorIBAN := l.accountIBANs[m.Payee]
	l.mu.RUnlock()
	var collections []pain008Collection
	var ctrlSum int64
	currency := ""
	for _, res := range results {
		if res.Err != nil {
			continue
		}
		collections = append(collections, pain008Collection{
			Entry:        res.Receipt.Entry,
			SeqTp:        res.DerivedSeqTp,
			DebtorIBAN:   debtorIBAN,
			CreditorIBAN: creditorIBAN,
		})
		currency = res.Receipt.Entry.Currency
		var ok bool
		ctrlSum, ok = addCents(ctrlSum, res.Receipt.Entry.AmountCents)
		if !ok {
			return Pain008ApplyResult{MsgID: doc.MsgID, Items: results}, ErrPain008AmountOverflow
		}
	}

	out := Pain008ApplyResult{MsgID: doc.MsgID, Items: results}
	l.mu.Lock()
	if len(collections) > 0 {
		l.registerPain008InstructionLocked(doc.MsgID, mandateID, collections, len(collections), ctrlSum, currency, actor)
	}
	if l.pain008Applied == nil {
		l.pain008Applied = make(map[string]pain008AppliedRecord)
	}
	l.pain008Applied[doc.MsgID] = pain008AppliedRecord{
		MsgID:  doc.MsgID,
		Items:  append([]Pain008DirectDebit(nil), doc.Items...),
		Result: out,
	}
	if len(collections) > 0 {
		l.emitAudit(AuditEvent{
			Op:            "pain008_initiation_applied",
			Actor:         actor,
			TraceID:       doc.MsgID,
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Details: map[string]any{
				"msg_id":             doc.MsgID,
				"mandate_id":         mandateID,
				"tx_count":           len(collections),
				"ctrl_sum_cents":     ctrlSum,
				"currency":           currency,
				"creditor_scheme_id": doc.Items[0].CreditorSchemeID,
			},
		})
	}
	l.mu.Unlock()
	return out, nil
}
