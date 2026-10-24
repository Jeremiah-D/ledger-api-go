package ledger

import (
	"encoding/xml"
	"errors"
	"fmt"
	"strings"
	"time"
)

// pacs.003.001.08 FIToFI Customer Direct Debit export (LG-201).
//
// ExportPacs003 renders a *posted transfer* as an ISO 20022
// `pacs.003.001.08` `FIToFICstmrDrctDbt` — the FI-to-FI customer
// direct debit instruction the creditor's bank sends to the debtor's
// bank: the creditor pulls the money from the debtor under a mandate,
// instead of the debtor pushing it (the pacs.008 credit transfer,
// LG-176) or the two institutions settling between themselves (the
// pacs.009 FI credit transfer, LG-172, and the pacs.010 FI direct
// debit, LG-190, which pulls between institutions, not customers).
//
// CORRECTION, READ FIRST: the backlog line for LG-201 described this
// export as carrying a `FIToFICstmrCdtTrf` root with a `CdtTrfTxInf`
// block. That is pacs.008's message, not pacs.003's — pacs.003 is the
// customer direct debit (`FIToFICstmrDrctDbt` / `DrctDbtTxInf`), and
// emitting a credit-transfer body under a pacs.003 namespace would be
// a false standards claim. This module implements the real pacs.003:
// the instruction is a pull (DrctDbtTxInf), not a push. Everything
// else the backlog specified holds — a posted transfer as the
// payment, a deterministic GrpHdr MsgId, PmtId / amount / debtor /
// creditor / both agents in the tx block, and fail-closed behavior
// for a missing agent bank or an unknown payment.
//
// Message shape, honestly mapped onto what the ledger knows:
//
//   - GrpHdr carries MsgId (deterministic "<transfer ID>/drctdbt",
//     the pacs.009 convention), CreDtTm, NbOfTxs, and InitgPty — the
//     initiating party, the creditor's bank, which is the party that
//     sends a direct-debit instruction.
//   - One DrctDbtTxInf: PmtId (InstrId / EndToEndId / TxId = the
//     transfer ID, the pacs.009 convention), InstdAmt (the transfer's
//     amount and currency, the instructed amount being pulled),
//     DrctDbtTx/MndtRltdInf (MndtId, mandatory — the debtor's
//     authorization for the pull — plus DtOfSgntr when the operator
//     states it), CdtrSchmeId (when the operator states a scheme
//     identifier), CdtrAgt / Cdtr / CdtrAcct (the creditor side: the
//     transfer's payee account), DbtrAgt / Dbtr / DbtrAcct (the
//     debtor side: the transfer's payer account), and Purp (the
//     transfer's ISO 20022 business purpose, see purpose.go, LG-180).
//
// Party identification follows the pacs.009 convention: the ledger
// keeps no BIC registry, so agents ride proprietary identification
// (`FinInstnId/Othr/Id` = the operator-supplied agent reference) and
// customer parties ride their ledger account (`Id/PrvtId/Othr/Id` =
// the account ID, with Nm when the operator states a name) — never a
// fabricated BIC, IBAN, or scheme identifier. The agents are
// operator-supplied because the ledger has no agent registry: the
// transfer's accounts name the customers, not their banks.
//
// Fail-closed: an unknown transfer ID (ErrTransferNotFound), a
// transfer that settled cross-currency (ErrPacs003FXTransfer — the
// principal entry books only the payer-side leg against the FX
// clearing account, so it does not describe the full customer debit),
// a missing debtor agent (ErrPacs003MissingDebtorAgent), a missing
// creditor agent (ErrPacs003MissingCreditorAgent), a missing mandate
// ID (ErrPacs003MissingMandate — a direct debit without a mandate is
// not an instruction, it is an unauthorized pull), and a malformed
// mandate signature date (ErrPacs003InvalidMandateDate) are all
// refused.
//
// Read-only: the principal journal entry is read as a snapshot copy
// under the read lock — no version bump, no journal rows, no audit
// event — the same contract as ExportPacs009. The durable record of
// the direct debit stays with the bank that issued the pacs.003; the
// ledger keeps no copy because the export is a statement of fact, not
// an operator attestation.

// pacs.003 export errors.
var (
	// ErrPacs003FXTransfer is returned by ExportPacs003 for a
	// cross-currency transfer: its principal entry books only the
	// payer-side leg against the FX clearing account, so rendering it
	// as a single customer direct debit would misstate the money
	// movement. Fail-closed.
	ErrPacs003FXTransfer = errors.New("ledger: pacs.003 export: cross-currency transfers are not supported")
	// ErrPacs003MissingDebtorAgent is returned when the operator
	// names no debtor agent: the ledger has no agent registry to fall
	// back on, so the instruction cannot name the bank being pulled
	// from. Fail-closed.
	ErrPacs003MissingDebtorAgent = errors.New("ledger: pacs.003 export: debtor agent is required")
	// ErrPacs003MissingCreditorAgent is returned when the operator
	// names no creditor agent: the instruction cannot name the bank
	// doing the pulling. Fail-closed.
	ErrPacs003MissingCreditorAgent = errors.New("ledger: pacs.003 export: creditor agent is required")
	// ErrPacs003MissingMandate is returned when the operator names no
	// mandate ID: a direct debit without a mandate is an unauthorized
	// pull, so the export refuses rather than emit a mandate-less
	// instruction. Fail-closed.
	ErrPacs003MissingMandate = errors.New("ledger: pacs.003 export: mandate ID is required")
	// ErrPacs003InvalidMandateDate is returned when the mandate
	// signature date is not a YYYY-MM-DD calendar date.
	ErrPacs003InvalidMandateDate = errors.New("ledger: pacs.003 export: mandate signature date must be YYYY-MM-DD")
)

// Pacs003Options tunes one pacs.003 export. DebtorAgent,
// CreditorAgent, and MandateID are required — the export is
// fail-closed without them (see the errors above). The remaining
// fields are optional projections: empty means the element is
// omitted from the wire.
type Pacs003Options struct {
	// DebtorAgent identifies the debtor's bank (DbtrAgt): the
	// operator's agent reference, rendered under proprietary
	// identification. Required.
	DebtorAgent string
	// CreditorAgent identifies the creditor's bank (CdtrAgt and
	// GrpHdr/InitgPty): the operator's agent reference, rendered
	// under proprietary identification. Required.
	CreditorAgent string
	// MandateID is the debtor's authorization for the pull
	// (DrctDbtTx/MndtRltdInf/MndtId). Required.
	MandateID string
	// MandateSignatureDate is the day the debtor signed the mandate
	// (MndtRltdInf/DtOfSgntr), YYYY-MM-DD. Empty omits the element.
	MandateSignatureDate string
	// DebtorName is the debtor customer's name (Dbtr/Nm). Empty
	// omits Nm; the debtor is still identified by account.
	DebtorName string
	// CreditorName is the creditor customer's name (Cdtr/Nm). Empty
	// omits Nm; the creditor is still identified by account.
	CreditorName string
	// CreditorSchemeID identifies the creditor scheme
	// (CdtrSchmeId/Id/PrvtId/Othr/Id). Empty omits the element.
	CreditorSchemeID string
}

// pacs003MsgID builds the deterministic export MsgId:
// "<transfer ID>/drctdbt". Stable across repeat exports of the same
// transfer — a statement of fact, not an attempt sequence.
func pacs003MsgID(transferID string) string {
	return transferID + "/drctdbt"
}

// pacs.003 wire structs: Document/FIToFICstmrDrctDbt. The namespace
// rides the Document element, the pacs.009 export convention, so
// real pacs.003 readers accept the document.
type pacs003Document struct {
	XMLName xml.Name       `xml:"urn:iso:std:iso:20022:tech:xsd:pacs.003.001.08 Document"`
	DrctDbt pacs003DrctDbt `xml:"FIToFICstmrDrctDbt"`
}

type pacs003DrctDbt struct {
	GrpHdr pacs003GrpHdr `xml:"GrpHdr"`
	TxInf  pacs003TxInf  `xml:"DrctDbtTxInf"`
}

type pacs003GrpHdr struct {
	MsgID    string       `xml:"MsgId"`
	CreDtTm  string       `xml:"CreDtTm"`
	NbOfTxs  string       `xml:"NbOfTxs"`
	InitgPty pacs003Agent `xml:"InitgPty"`
}

type pacs003TxInf struct {
	PmtID       pacs003PmtID     `xml:"PmtId"`
	InstdAmt    pacs003Amt       `xml:"InstdAmt"`
	DrctDbtTx   pacs003DrctDbtTx `xml:"DrctDbtTx"`
	CdtrSchmeId *pacs003SchemeId `xml:"CdtrSchmeId,omitempty"`
	CdtrAgt     pacs003Agent     `xml:"CdtrAgt"`
	Cdtr        pacs003Party     `xml:"Cdtr"`
	CdtrAcct    pacs003Acct      `xml:"CdtrAcct"`
	DbtrAgt     pacs003Agent     `xml:"DbtrAgt"`
	Dbtr        pacs003Party     `xml:"Dbtr"`
	DbtrAcct    pacs003Acct      `xml:"DbtrAcct"`
	// Purp carries the transfer's ISO 20022 business purpose
	// (ExternalPurpose1Code, see purpose.go, LG-180) as
	// <Purp><Cd>. It is omitted when the transfer states no purpose.
	Purp *pacs003Purpose `xml:"Purp,omitempty"`
}

// pacs003Purpose renders one ISO 20022 purpose code: <Purp><Cd>SALA</Cd></Purp>.
type pacs003Purpose struct {
	Cd string `xml:"Cd"`
}

// pacs003PurposeFor maps a journaled purpose code to the wire
// element: nil for "no purpose stated", so the export omits Purp
// entirely. The code is already validated at post time (see
// checkPurposeCode), so this is a pure projection.
func pacs003PurposeFor(code string) *pacs003Purpose {
	if code == "" {
		return nil
	}
	return &pacs003Purpose{Cd: code}
}

type pacs003PmtID struct {
	InstrID    string `xml:"InstrId"`
	EndToEndID string `xml:"EndToEndId"`
	TxID       string `xml:"TxId"`
}

type pacs003Amt struct {
	// Ccy rides the amount element as an attribute, the pacs.009
	// IntrBkSttlmAmt convention.
	Ccy   string `xml:"Ccy,attr"`
	Value string `xml:",chardata"`
}

// pacs003DrctDbtTx renders the mandate block: the mandate ID is
// mandatory (the export refuses without one), the signature date
// optional.
type pacs003DrctDbtTx struct {
	MndtRltdInf pacs003MndtRltdInf `xml:"MndtRltdInf"`
}

type pacs003MndtRltdInf struct {
	MndtID    string  `xml:"MndtId"`
	DtOfSgntr *string `xml:"DtOfSgntr,omitempty"`
}

// pacs003SchemeId renders the creditor scheme identification:
// <CdtrSchmeId><Id><PrvtId><Othr><Id>… — the pain.008
// CdtrSchmeId convention (a private scheme identifier, never a
// fabricated registered scheme).
type pacs003SchemeId struct {
	ID pacs003SchemePrvtID `xml:"Id"`
}

type pacs003SchemePrvtID struct {
	PrvtID pacs003SchemeOthr `xml:"PrvtId"`
}

type pacs003SchemeOthr struct {
	Othr pacs003Othr `xml:"Othr"`
}

// pacs003Agent identifies one FI counterparty. The ledger keeps no
// BIC registry, so the agent rides proprietary identification with
// the operator-supplied reference — the honest mapping, documented
// above; never a fabricated BIC.
type pacs003Agent struct {
	FinInstnID pacs003FinInstnID `xml:"FinInstnId"`
}

type pacs003FinInstnID struct {
	Othr pacs003Othr `xml:"Othr"`
}

type pacs003Othr struct {
	ID string `xml:"Id"`
}

// pacs003Party identifies one customer party: an optional name plus
// the ledger account under private proprietary identification. The
// account is the identity; the name is decoration.
type pacs003Party struct {
	Nm *string        `xml:"Nm,omitempty"`
	ID pacs003PartyID `xml:"Id"`
}

type pacs003PartyID struct {
	PrvtID pacs003PartyOthr `xml:"PrvtId"`
}

type pacs003PartyOthr struct {
	Othr pacs003Othr `xml:"Othr"`
}

// pacs003Acct identifies one account by proprietary Other/Id — the
// pacs.008 export convention.
type pacs003Acct struct {
	ID pacs003AcctID `xml:"Id"`
}

type pacs003AcctID struct {
	Othr pacs003Othr `xml:"Othr"`
}

func pacs003AgentFor(reference string) pacs003Agent {
	return pacs003Agent{FinInstnID: pacs003FinInstnID{Othr: pacs003Othr{ID: reference}}}
}

func pacs003PartyFor(name string, account AccountID) pacs003Party {
	var nm *string
	if strings.TrimSpace(name) != "" {
		v := strings.TrimSpace(name)
		nm = &v
	}
	return pacs003Party{
		Nm: nm,
		ID: pacs003PartyID{PrvtID: pacs003PartyOthr{Othr: pacs003Othr{ID: string(account)}}},
	}
}

func pacs003AcctFor(account AccountID) pacs003Acct {
	return pacs003Acct{ID: pacs003AcctID{Othr: pacs003Othr{ID: string(account)}}}
}

func pacs003SchemeIdFor(schemeID string) *pacs003SchemeId {
	if strings.TrimSpace(schemeID) == "" {
		return nil
	}
	return &pacs003SchemeId{
		ID: pacs003SchemePrvtID{PrvtID: pacs003SchemeOthr{Othr: pacs003Othr{ID: strings.TrimSpace(schemeID)}}},
	}
}

// ExportPacs003 renders the posted transfer behind transferID as an
// ISO 20022 pacs.003.001.08 FIToFICstmrDrctDbt document, the
// FI-to-FI customer direct debit instruction: the creditor's bank
// pulls the transfer amount from the debtor's bank under the
// operator-named mandate.
//
// The transfer's payer account (the principal entry's credit account,
// the pacs.009 payer convention) is the debtor; the payee account
// (the entry's debit account) is the creditor. The debtor and
// creditor agents and the mandate are operator-supplied — the ledger
// has no agent registry and no mandate registry to derive them from —
// so the export refuses rather than invent them.
//
// Fail-closed: an unknown transfer ID (ErrTransferNotFound), a
// cross-currency transfer (ErrPacs003FXTransfer — the principal entry
// books only the payer-side leg against the FX clearing account), a
// missing debtor agent (ErrPacs003MissingDebtorAgent), a missing
// creditor agent (ErrPacs003MissingCreditorAgent), a missing mandate
// ID (ErrPacs003MissingMandate), and a malformed mandate signature
// date (ErrPacs003InvalidMandateDate) are all refused.
//
// Read-only: the entry is read as a snapshot copy under the read
// lock — no version bump, no journal rows, no audit event.
func (l *Ledger) ExportPacs003(transferID string, opts Pacs003Options) ([]byte, error) {
	dbtrAgt := strings.TrimSpace(opts.DebtorAgent)
	if dbtrAgt == "" {
		return nil, ErrPacs003MissingDebtorAgent
	}
	cdtrAgt := strings.TrimSpace(opts.CreditorAgent)
	if cdtrAgt == "" {
		return nil, ErrPacs003MissingCreditorAgent
	}
	mndtID := strings.TrimSpace(opts.MandateID)
	if mndtID == "" {
		return nil, ErrPacs003MissingMandate
	}
	var dtOfSgntr *string
	if strings.TrimSpace(opts.MandateSignatureDate) != "" {
		d := strings.TrimSpace(opts.MandateSignatureDate)
		if _, err := time.Parse("2006-01-02", d); err != nil {
			return nil, fmt.Errorf("%w: %q", ErrPacs003InvalidMandateDate, opts.MandateSignatureDate)
		}
		dtOfSgntr = &d
	}
	l.mu.RLock()
	e, ok := l.entries[transferID]
	if !ok {
		l.mu.RUnlock()
		return nil, fmt.Errorf("%w: %q", ErrTransferNotFound, transferID)
	}
	if _, isFX := l.entries[transferID+"/fx"]; isFX {
		l.mu.RUnlock()
		return nil, fmt.Errorf("%w: %q", ErrPacs003FXTransfer, transferID)
	}
	l.mu.RUnlock()
	// e is a value copy out of the map: the render below cannot
	// mutate ledger state even by accident.
	// Direction, the pacs.009 payer convention: the entry's credit
	// account is the payer (here the debtor, whose account is pulled)
	// and the debit account the payee (here the creditor, who pulls).
	dbtr := e.CreditAccount
	cdtr := e.DebitAccount
	doc := pacs003Document{
		DrctDbt: pacs003DrctDbt{
			GrpHdr: pacs003GrpHdr{
				MsgID:    pacs003MsgID(transferID),
				CreDtTm:  time.Now().UTC().Format(time.RFC3339),
				NbOfTxs:  "1",
				InitgPty: pacs003AgentFor(cdtrAgt),
			},
			TxInf: pacs003TxInf{
				PmtID: pacs003PmtID{
					InstrID:    transferID,
					EndToEndID: transferID,
					TxID:       transferID,
				},
				InstdAmt: pacs003Amt{
					Ccy:   e.Currency,
					Value: pain001Amount(e.AmountCents),
				},
				DrctDbtTx: pacs003DrctDbtTx{
					MndtRltdInf: pacs003MndtRltdInf{
						MndtID:    mndtID,
						DtOfSgntr: dtOfSgntr,
					},
				},
				CdtrSchmeId: pacs003SchemeIdFor(opts.CreditorSchemeID),
				CdtrAgt:     pacs003AgentFor(cdtrAgt),
				Cdtr:        pacs003PartyFor(opts.CreditorName, cdtr),
				CdtrAcct:    pacs003AcctFor(cdtr),
				DbtrAgt:     pacs003AgentFor(dbtrAgt),
				Dbtr:        pacs003PartyFor(opts.DebtorName, dbtr),
				DbtrAcct:    pacs003AcctFor(dbtr),
				Purp:        pacs003PurposeFor(e.PurposeCode),
			},
		},
	}
	body, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("ledger: pacs.003 export: render failed: %v", err)
	}
	return append([]byte(xml.Header), body...), nil
}
