package ledger

import (
	"encoding/xml"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ISO 20022 pacs.010.001.02 FinancialInstitutionDirectDebit inbound
// processing (LG-423): the inbound half of the pacs.010 lifecycle,
// the mirror of the LG-190 export.
//
// ExportPacs010 renders this ledger's own booked mandate collections
// as the instruction the creditor's bank sends to the debtor's bank;
// this module consumes the instruction a counterparty creditor's bank
// sent, on the debtor bank's side: it verifies each pull against the
// mandate that authorizes it and books the debit. The two are
// orthogonal by construction:
//
//   - LG-190 exports: the ledger tells the debtor's bank "pull this
//     amount from the debtor under this mandate" (outbound,
//     read-only, no ledger state).
//   - LG-423 imports: the creditor's bank tells the ledger "pull this
//     amount from the debtor under this mandate" (inbound,
//     state-changing: one mandate debit per covered instruction,
//     under the mandate's authorization).
//
// ParsePacs010 builds its own parse structs: the outbound export is
// a strings.Builder rendering (pacs010.go:298), so there is no wire
// struct to read in reverse — the parse structs below model the same
// `pacs.010.001.02` `FIDrctDbt` document (GrpHdr plus one CdtInstr
// per instruction). The namespace rides the Document element, so a
// document in the wrong schema version fails the unmarshal — the
// version gate is the namespace, not a string check (the camt.056
// inbound convention, LG-414).
//
// The collection date: pacs.010.001.02 carries it per instruction as
// `CdtInstr/ReqdColltnDt` — the day the creditor's bank asks the
// debtor's bank to collect — and ApplyPacs010 treats it as the
// booking timestamp, so DebitWithMandate's own validity-window gate
// enforces it. A document that states no collection date for an
// instruction books that instruction at now, the DebitWithMandate
// default. A malformed collection date is a parse refusal — a pull
// whose collection day cannot be read is never booked on a guessed
// date.
//
// ApplyPacs010 follows ProcessPacs008's per-item shape
// (fednow_pacs008.go:474): the whole batch must parse before
// anything applies — a half-readable bank file must not partially
// apply — then each instruction applies on its own, a per-item
// failure is recorded in its result (plus an explicit
// `pacs010_debit_rejected` audit event) and never blocks the other
// instructions. The top-level error is nil unless the document
// itself is unusable.
//
// Every authorization gate is DebitWithMandate's, unmodified:
// unknown mandate, non-active mandate, a collection date outside
// [ValidFrom, ValidUntil), an amount above the per-debit cap, a
// currency mismatch, and the frequency window are all that
// function's gates. No mandate is ever auto-created: an instruction
// naming no mandate the ledger knows is refused, fail-closed. The
// legs come from the mandate itself (debit the payee, credit the
// payer), so the document's Cdtr/Dbtr parties, accounts, and agents
// are informational — carried on the parsed struct and the audit
// event — never the source of the booking.
//
// Idempotency: the entry ID and the idempotency key are both the
// instruction's EndToEndId. A repeated MsgId — the same document
// re-sent — replays through DebitWithMandate's byKey check and
// returns the original receipts with Duplicate set, booking nothing
// new, even if a mandate has since been suspended, cancelled, or
// expired (the replay-books-nothing convention). The inbound audit
// event fires only on a fresh booking, never on a replay.
//
// Audit: a fresh booking emits `pacs010_debit_applied`, distinct
// from the `mandate_debit` event DebitWithMandate itself emits, so
// the audit trail tells the inbound pacs.010 direction apart from
// an operator-driven mandate debit (the pacs.003
// `pacs003_debit_applied` convention, LG-415).
//
// This does not go through pain.002: a pain.002 status report
// naming a pacs.010 MsgId is refused by ErrPain002ForeignMsgID
// (pain002.go:201-202, LG-406) — the apply endpoint here is itself
// the debtor bank's handling of the instruction, not a status
// report about it.

// pacs.010 inbound errors.
var (
	// ErrPacs010Parse is returned by ParsePacs010 for malformed XML
	// or a document missing a required field: the creditor's bank
	// sent something the ledger cannot even read as a direct debit
	// instruction.
	ErrPacs010Parse = errors.New("ledger: pacs.010 inbound: unparseable direct debit document")
)

// Pacs010DirectDebit is one parsed inbound pacs.010 credit
// instruction (one CdtInstr): the instruction's identity, the
// mandate it pulls under, the amount, and the requested collection
// date.
type Pacs010DirectDebit struct {
	// MsgID is the batch's GrpHdr MsgId, carried on every
	// instruction: the audit trace for the apply.
	MsgID string `json:"msg_id"`
	// InstrID and EndToEndID are the PmtId pair. EndToEndId is the
	// business identity of the instruction: it becomes both the
	// journal entry ID and the idempotency key of the mandate
	// debit, so a re-sent instruction converges on the original
	// booking.
	InstrID    string `json:"instr_id,omitempty"`
	EndToEndID string `json:"end_to_end_id"`
	// AmountCents is the InstdAmt in minor units; Currency is its
	// Ccy attribute, a 3-letter uppercase ISO 4217 code.
	AmountCents int64  `json:"amount_cents"`
	Currency    string `json:"currency"`
	// MandateID is the DrctDbtTx/MndtRltdInf/MndtId: the mandate
	// the pull is authorized under. Never auto-created.
	MandateID string `json:"mandate_id"`
	// MandateSignatureDate is MndtRltdInf/DtOfSgntr (YYYY-MM-DD),
	// when the creditor's bank states it. Informational: the
	// authorization is the mandate row, not the date on the wire.
	MandateSignatureDate string `json:"mandate_signature_date,omitempty"`
	// CollectionDate is CdtInstr/ReqdColltnDt (YYYY-MM-DD), the day
	// the creditor's bank asks the debtor's bank to collect. Zero
	// when the document states none: the debit then books at now.
	CollectionDate time.Time `json:"collection_date,omitempty"`
	// Creditor and Debtor are the Cdtr/Dbtr party names;
	// CreditorAccount and DebtorAccount are the CdtrAcct/DbtrAcct
	// references (IBAN when stated, else Othr/Id);
	// CreditorAgent and DebtorAgent are the CdtrAgt/DbtrAgt
	// references (BIC/BICFI when stated, else Othr/Id).
	// Informational: the legs come from the mandate, never from
	// the document.
	Creditor        string `json:"creditor,omitempty"`
	Debtor          string `json:"debtor,omitempty"`
	CreditorAccount string `json:"creditor_account,omitempty"`
	DebtorAccount   string `json:"debtor_account,omitempty"`
	CreditorAgent   string `json:"creditor_agent,omitempty"`
	DebtorAgent     string `json:"debtor_agent,omitempty"`
	// Remittance is RmtInf/Ustrd when stated, verbatim.
	Remittance string `json:"remittance,omitempty"`
}

// Pacs010Document is one parsed inbound pacs.010.001.02 document:
// the GrpHdr identity and settlement method plus every credit
// instruction it carries, in document order.
type Pacs010Document struct {
	// MsgID is the GrpHdr MsgId: the audit trace for the apply.
	MsgID string `json:"msg_id"`
	// SettlementMethod is GrpHdr/SttlmInf/SttlmMtd, one of
	// INDA/INGA/COVE/CLRG; an unstated method reads as INDA, the
	// export's default.
	SettlementMethod string `json:"settlement_method"`
	// Items are the CdtInstr instructions, in document order.
	Items []Pacs010DirectDebit `json:"items"`
}

// Pacs010ItemResult is the per-item outcome of ApplyPacs010: an
// instruction is either booked (Receipt set, EntryID set), an
// idempotent replay (Duplicate true), or rejected (Err set). One
// item's failure never blocks the others.
type Pacs010ItemResult struct {
	// Instruction is the parsed instruction this result is about.
	Instruction Pacs010DirectDebit `json:"instruction"`
	// Receipt is the DebitWithMandate receipt on success or replay;
	// empty when rejected.
	Receipt MandateDebitReceipt `json:"receipt,omitempty"`
	// EntryID is the booked entry's ID (the instruction's
	// EndToEndId), empty when rejected.
	EntryID string `json:"entry_id,omitempty"`
	// Duplicate reports an idempotent replay: the same instruction
	// was applied before, so the earlier entry is returned and
	// nothing new was booked — no new audit event.
	Duplicate bool `json:"duplicate"`
	// Err is the per-item rejection reason, nil on success.
	Err error `json:"-"`
}

// The pacs010x* structs model the ISO 20022 pacs.010.001.02
// FIDrctDbt shape: the same document ExportPacs010 renders, read in
// the reverse direction. The Document element carries the namespace,
// so a document in the wrong schema version fails the unmarshal.
// Unknown elements are skipped, which is what makes the parser
// tolerant of the optional blocks real instructions carry.
type pacs010xDocument struct {
	XMLName xml.Name          `xml:"urn:iso:std:iso:20022:tech:xsd:pacs.010.001.02 Document"`
	DrctDbt pacs010xFIDrctDbt `xml:"FIDrctDbt"`
}

type pacs010xFIDrctDbt struct {
	GrpHdr pacs010xGrpHdr     `xml:"GrpHdr"`
	Instrs []pacs010xCdtInstr `xml:"CdtInstr"`
}

type pacs010xGrpHdr struct {
	MsgID    string           `xml:"MsgId"`
	CreDtTm  string           `xml:"CreDtTm"`
	NbOfTxs  *int             `xml:"NbOfTxs"`
	CtrlSum  *string          `xml:"CtrlSum"`
	SttlmInf pacs010xSttlmInf `xml:"SttlmInf"`
}

type pacs010xSttlmInf struct {
	SttlmMtd string `xml:"SttlmMtd"`
}

type pacs010xCdtInstr struct {
	PmtID        pacs010xPmtID     `xml:"PmtId"`
	InstdAmt     pacs010xAmt       `xml:"InstdAmt"`
	ReqdColltnDt string            `xml:"ReqdColltnDt"`
	Cdtr         pacs010xParty     `xml:"Cdtr"`
	CdtrAcct     pacs010xAcct      `xml:"CdtrAcct"`
	CdtrAgt      pacs010xAgent     `xml:"CdtrAgt"`
	Dbtr         pacs010xParty     `xml:"Dbtr"`
	DbtrAcct     pacs010xAcct      `xml:"DbtrAcct"`
	DbtrAgt      pacs010xAgent     `xml:"DbtrAgt"`
	DrctDbtTx    pacs010xDrctDbtTx `xml:"DrctDbtTx"`
	RmtInf       pacs010xRmtInf    `xml:"RmtInf"`
}

type pacs010xPmtID struct {
	InstrID    string `xml:"InstrId"`
	EndToEndID string `xml:"EndToEndId"`
}

type pacs010xAmt struct {
	Ccy   string `xml:"Ccy,attr"`
	Value string `xml:",chardata"`
}

type pacs010xParty struct {
	Name string `xml:"Nm"`
}

type pacs010xAcct struct {
	ID pacs010xAcctID `xml:"Id"`
}

type pacs010xAcctID struct {
	IBAN string        `xml:"IBAN"`
	Othr *pacs010xOthr `xml:"Othr"`
}

type pacs010xOthr struct {
	ID string `xml:"Id"`
}

type pacs010xAgent struct {
	FinInstnID pacs010xFinInstnID `xml:"FinInstnId"`
}

type pacs010xFinInstnID struct {
	BIC   string        `xml:"BIC"`
	BICFI string        `xml:"BICFI"`
	Othr  *pacs010xOthr `xml:"Othr"`
}

type pacs010xDrctDbtTx struct {
	MndtRltdInf pacs010xMndtRltdInf `xml:"MndtRltdInf"`
}

type pacs010xMndtRltdInf struct {
	MndtID    string `xml:"MndtId"`
	DtOfSgntr string `xml:"DtOfSgntr"`
}

type pacs010xRmtInf struct {
	Ustrd string `xml:"Ustrd"`
}

// pacs010ParseAmount parses an ISO 20022 decimal amount ("1234.56")
// into minor units. The integer part holds at most 12 digits (the
// pacs004ParseAmount cap); the fraction holds at most two digits and
// is right-padded ("5" means 50 cents); the value must be positive.
// Anything else is a parse refusal — a debit amount the ledger
// cannot read exactly is never rounded into a booking.
func pacs010ParseAmount(body string) (int64, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return 0, fmt.Errorf("%w: empty amount", ErrPacs010Parse)
	}
	intPart := body
	fracPart := ""
	if i := strings.IndexByte(body, '.'); i >= 0 {
		intPart, fracPart = body[:i], body[i+1:]
		if strings.IndexByte(fracPart, '.') >= 0 {
			return 0, fmt.Errorf("%w: amount %q has two decimal points", ErrPacs010Parse, body)
		}
	}
	if intPart == "" || len(intPart) > 12 || len(fracPart) > 2 {
		return 0, fmt.Errorf("%w: amount %q", ErrPacs010Parse, body)
	}
	for _, c := range intPart + fracPart {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("%w: amount %q is not numeric", ErrPacs010Parse, body)
		}
	}
	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: amount %q", ErrPacs010Parse, body)
	}
	frac := int64(0)
	if fracPart != "" {
		if len(fracPart) == 1 {
			fracPart += "0"
		}
		frac, err = strconv.ParseInt(fracPart, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%w: amount %q", ErrPacs010Parse, body)
		}
	}
	cents := whole*100 + frac
	if cents <= 0 {
		return 0, fmt.Errorf("%w: amount %q is not positive", ErrPacs010Parse, body)
	}
	return cents, nil
}

// pacs010AccountRef reads one CdtrAcct/DbtrAcct reference: the IBAN
// when stated, else the Othr/Id account reference. Empty when the
// document states neither — the account is informational, so a
// missing reference is carried as empty, never invented.
func pacs010AccountRef(a pacs010xAcct) string {
	if iban := strings.TrimSpace(a.ID.IBAN); iban != "" {
		return iban
	}
	if a.ID.Othr != nil {
		return strings.TrimSpace(a.ID.Othr.ID)
	}
	return ""
}

// pacs010AgentRef reads one CdtrAgt/DbtrAgt reference: the BIC/BICFI
// when stated, else the Othr/Id reference. Empty when the document
// states neither.
func pacs010AgentRef(a pacs010xAgent) string {
	if bic := strings.TrimSpace(a.FinInstnID.BIC); bic != "" {
		return bic
	}
	if bicfi := strings.TrimSpace(a.FinInstnID.BICFI); bicfi != "" {
		return bicfi
	}
	if a.FinInstnID.Othr != nil {
		return strings.TrimSpace(a.FinInstnID.Othr.ID)
	}
	return ""
}

// validatePacs010DirectDebit fail-closes on an instruction that
// ParsePacs010 would refuse, so ApplyPacs010 stays safe on
// hand-built documents too: the parse gate and the apply gate share
// one check, never a copy.
func validatePacs010DirectDebit(d Pacs010DirectDebit) error {
	if strings.TrimSpace(d.MsgID) == "" {
		return fmt.Errorf("%w: missing GrpHdr/MsgId", ErrPacs010Parse)
	}
	if strings.TrimSpace(d.EndToEndID) == "" {
		return fmt.Errorf("%w: missing CdtInstr/PmtId/EndToEndId", ErrPacs010Parse)
	}
	if strings.TrimSpace(d.MandateID) == "" {
		return fmt.Errorf("%w: missing DrctDbtTx/MndtRltdInf/MndtId", ErrPacs010Parse)
	}
	if d.AmountCents <= 0 {
		return fmt.Errorf("%w: non-positive instructed amount", ErrPacs010Parse)
	}
	if strings.TrimSpace(d.Currency) == "" {
		return fmt.Errorf("%w: empty instructed amount currency", ErrPacs010Parse)
	}
	if _, err := normalizeCurrency(d.Currency); err != nil {
		return fmt.Errorf("%w: currency %q", ErrPacs010Parse, d.Currency)
	}
	if d.MandateSignatureDate != "" {
		if _, err := time.Parse("2006-01-02", d.MandateSignatureDate); err != nil {
			return fmt.Errorf("%w: mandate signature date %q must be YYYY-MM-DD", ErrPacs010Parse, d.MandateSignatureDate)
		}
	}
	return nil
}

// validatePacs010Document fail-closes on a document that
// ParsePacs010 would refuse: a missing MsgId, a settlement method
// outside the admitted vocabulary, no instructions at all, or any
// instruction the per-item gate refuses.
func validatePacs010Document(doc Pacs010Document) error {
	if strings.TrimSpace(doc.MsgID) == "" {
		return fmt.Errorf("%w: missing GrpHdr/MsgId", ErrPacs010Parse)
	}
	if !pacs009ValidSettlementMethod(doc.SettlementMethod) {
		return fmt.Errorf("%w: %q", ErrPacs010InvalidSettlementMethod, doc.SettlementMethod)
	}
	if len(doc.Items) == 0 {
		return fmt.Errorf("%w: no CdtInstr", ErrPacs010Parse)
	}
	for i := range doc.Items {
		if err := validatePacs010DirectDebit(doc.Items[i]); err != nil {
			return err
		}
	}
	return nil
}

// ParsePacs010 parses one inbound pacs.010.001.02 direct debit
// document into a Pacs010Document. It validates the whole document
// before returning anything: a missing MsgId, a settlement method
// outside the INDA/INGA/COVE/CLRG vocabulary, a NbOfTxs that
// disagrees with the instruction count, a CtrlSum that disagrees
// with the instructed total (a missing CtrlSum is refused the same
// way — an unverifiable batch is never applied on trust), no
// instructions at all, or any instruction with a missing EndToEndId
// or mandate ID, an unreadable or non-positive amount, an empty or
// malformed currency, or a malformed signature or collection date
// are all parse refusals — ApplyPacs010 never sees a half-read
// document. The creditor/debtor parties, accounts, and agents are
// read but not gated: the mandate names the legs, so a document
// that misnames a party cannot misdirect the booking.
func ParsePacs010(data []byte) (Pacs010Document, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return Pacs010Document{}, fmt.Errorf("%w: empty document", ErrPacs010Parse)
	}
	var doc pacs010xDocument
	if err := xml.Unmarshal(data, &doc); err != nil {
		return Pacs010Document{}, fmt.Errorf("%w: %v", ErrPacs010Parse, err)
	}
	grp := doc.DrctDbt.GrpHdr
	msgID := strings.TrimSpace(grp.MsgID)
	if msgID == "" {
		return Pacs010Document{}, fmt.Errorf("%w: missing GrpHdr/MsgId", ErrPacs010Parse)
	}
	sttlmMtd := strings.TrimSpace(grp.SttlmInf.SttlmMtd)
	if sttlmMtd == "" {
		// The export's default: an unstated method is the
		// ordinary correspondent debit/credit.
		sttlmMtd = DefaultPacs009SettlementMethod
	}
	if !pacs009ValidSettlementMethod(sttlmMtd) {
		return Pacs010Document{}, fmt.Errorf("%w: %q", ErrPacs010InvalidSettlementMethod, sttlmMtd)
	}
	if len(doc.DrctDbt.Instrs) == 0 {
		return Pacs010Document{}, fmt.Errorf("%w: no CdtInstr", ErrPacs010Parse)
	}
	if grp.NbOfTxs != nil && *grp.NbOfTxs != len(doc.DrctDbt.Instrs) {
		return Pacs010Document{}, fmt.Errorf("%w: GrpHdr/NbOfTxs %d disagrees with %d CdtInstr", ErrPacs010Parse, *grp.NbOfTxs, len(doc.DrctDbt.Instrs))
	}
	items := make([]Pacs010DirectDebit, 0, len(doc.DrctDbt.Instrs))
	var sum int64
	for _, x := range doc.DrctDbt.Instrs {
		amount, err := pacs010ParseAmount(x.InstdAmt.Value)
		if err != nil {
			return Pacs010Document{}, err
		}
		if strings.TrimSpace(x.InstdAmt.Ccy) == "" {
			return Pacs010Document{}, fmt.Errorf("%w: empty instructed amount currency", ErrPacs010Parse)
		}
		currency, err := normalizeCurrency(x.InstdAmt.Ccy)
		if err != nil {
			return Pacs010Document{}, fmt.Errorf("%w: currency %q", ErrPacs010Parse, x.InstdAmt.Ccy)
		}
		var signatureDate string
		if raw := strings.TrimSpace(x.DrctDbtTx.MndtRltdInf.DtOfSgntr); raw != "" {
			if _, err := time.Parse("2006-01-02", raw); err != nil {
				return Pacs010Document{}, fmt.Errorf("%w: mandate signature date %q must be YYYY-MM-DD", ErrPacs010Parse, raw)
			}
			signatureDate = raw
		}
		var collectionDate time.Time
		if raw := strings.TrimSpace(x.ReqdColltnDt); raw != "" {
			parsed, err := time.Parse("2006-01-02", raw)
			if err != nil {
				return Pacs010Document{}, fmt.Errorf("%w: collection date %q must be YYYY-MM-DD", ErrPacs010Parse, raw)
			}
			collectionDate = parsed
		}
		d := Pacs010DirectDebit{
			MsgID:                msgID,
			InstrID:              strings.TrimSpace(x.PmtID.InstrID),
			EndToEndID:           strings.TrimSpace(x.PmtID.EndToEndID),
			AmountCents:          amount,
			Currency:             currency,
			MandateID:            strings.TrimSpace(x.DrctDbtTx.MndtRltdInf.MndtID),
			MandateSignatureDate: signatureDate,
			CollectionDate:       collectionDate,
			Creditor:             strings.TrimSpace(x.Cdtr.Name),
			Debtor:               strings.TrimSpace(x.Dbtr.Name),
			CreditorAccount:      pacs010AccountRef(x.CdtrAcct),
			DebtorAccount:        pacs010AccountRef(x.DbtrAcct),
			CreditorAgent:        pacs010AgentRef(x.CdtrAgt),
			DebtorAgent:          pacs010AgentRef(x.DbtrAgt),
			Remittance:           strings.TrimSpace(x.RmtInf.Ustrd),
		}
		if err := validatePacs010DirectDebit(d); err != nil {
			return Pacs010Document{}, err
		}
		items = append(items, d)
		var ok bool
		sum, ok = addCents(sum, amount)
		if !ok {
			return Pacs010Document{}, ErrPacs010AmountOverflow
		}
	}
	if grp.CtrlSum == nil {
		return Pacs010Document{}, fmt.Errorf("%w: missing GrpHdr/CtrlSum", ErrPacs010Parse)
	}
	ctrlSum, err := pacs010ParseAmount(*grp.CtrlSum)
	if err != nil {
		return Pacs010Document{}, fmt.Errorf("%w: control sum %q", ErrPacs010Parse, strings.TrimSpace(*grp.CtrlSum))
	}
	if ctrlSum != sum {
		return Pacs010Document{}, fmt.Errorf("%w: GrpHdr/CtrlSum %d disagrees with instructed total %d", ErrPacs010Parse, ctrlSum, sum)
	}
	return Pacs010Document{
		MsgID:            msgID,
		SettlementMethod: sttlmMtd,
		Items:            items,
	}, nil
}

// ApplyPacs010 books each parsed inbound pacs.010 direct debit
// instruction: it pulls the instructed amount from the mandate's
// payer to the mandate's payee through DebitWithMandate, with the
// instruction's EndToEndId as both the journal entry ID and the
// idempotency key, and the requested collection date (when the
// document states one) as the booking timestamp, so the mandate's
// validity window is evaluated at the day the creditor's bank asked
// to collect.
//
// The whole document is validated before anything applies; then
// each instruction applies on its own. A per-item failure —
// unknown mandate (ErrMandateNotFound), non-active mandate
// (ErrMandateNotActive), a collection date outside [ValidFrom,
// ValidUntil) (ErrMandateNotActive before ValidFrom,
// ErrMandateExpired after), an amount above the per-debit cap
// (ErrMandateAmountExceeded), a currency mismatch
// (ErrMandateCurrencyMismatch), or a used frequency window
// (ErrMandateFrequencyExceeded) — is recorded in that item's
// result with an explicit `pacs010_debit_rejected` audit event, and
// never blocks the other instructions. A repeated MsgId returns
// the original receipts with Duplicate set and books nothing new —
// and emits no second `pacs010_debit_applied` audit event, so a
// re-sent document leaves exactly one debit and one inbound audit
// event per instruction.
//
// The audit event is emitted after DebitWithMandate commits, under
// a separately acquired write lock: DebitWithMandate owns its lock
// internally, so the inbound direction marker cannot join its
// atomic commit zone — the debit itself is already durable when the
// marker lands.
func (l *Ledger) ApplyPacs010(doc Pacs010Document, actor string) ([]Pacs010ItemResult, error) {
	if actor == "" {
		actor = "ApplyPacs010"
	}
	if strings.TrimSpace(doc.SettlementMethod) == "" {
		doc.SettlementMethod = DefaultPacs009SettlementMethod
	}
	if err := validatePacs010Document(doc); err != nil {
		return nil, err
	}
	results := make([]Pacs010ItemResult, 0, len(doc.Items))
	for i := range doc.Items {
		results = append(results, l.applyPacs010Item(doc.Items[i], actor))
	}
	return results, nil
}

// applyPacs010Item books one parsed instruction through
// DebitWithMandate and records the per-item outcome.
func (l *Ledger) applyPacs010Item(d Pacs010DirectDebit, actor string) Pacs010ItemResult {
	res := Pacs010ItemResult{Instruction: d}
	receipt, err := l.DebitWithMandate(d.MandateID, MandateDebit{
		ID:             d.EndToEndID,
		AmountCents:    d.AmountCents,
		Currency:       d.Currency,
		Memo:           "pacs010:" + strings.TrimSpace(d.MsgID),
		IdempotencyKey: d.EndToEndID,
		CreatedAt:      d.CollectionDate,
	})
	if err != nil {
		res.Err = err
		l.mu.Lock()
		l.emitAudit(AuditEvent{
			Op:      "pacs010_debit_rejected",
			Actor:   actor,
			TraceID: d.MsgID,
			Details: map[string]any{
				"msg_id":        d.MsgID,
				"end_to_end_id": d.EndToEndID,
				"mandate_id":    d.MandateID,
				"amount_cents":  d.AmountCents,
				"currency":      d.Currency,
				"reason":        err.Error(),
			},
		})
		l.mu.Unlock()
		return res
	}
	res.Receipt = receipt
	res.EntryID = receipt.Entry.ID
	res.Duplicate = receipt.Duplicate
	if receipt.Duplicate {
		return res
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
		Op:            "pacs010_debit_applied",
		Actor:         actor,
		TraceID:       d.MsgID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		EntryIDs:      []string{receipt.Entry.ID},
		Accounts:      []AccountID{receipt.Entry.CreditAccount, receipt.Entry.DebitAccount},
		Details:       details,
	})
	l.mu.Unlock()
	return res
}
