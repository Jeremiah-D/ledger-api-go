package ledger

import (
	"encoding/xml"
	"errors"
	"fmt"
	"strings"
	"time"
)

// pacs.009.001.06 Financial Institution Credit Transfer export
// (LG-172).
//
// ExportPacs009 renders a *posted transfer* as an ISO 20022
// `pacs.009.001.06` `FinancialInstitutionCreditTransfer` — the
// bank-to-bank (FI-to-FI) credit transfer message used for
// wholesale / correspondent-banking settlement. It is orthogonal to
// the fednow_pacs008 path (LG-163, instant-payment customer
// transfers) and to the gpi tracking path (LG-165): pacs.009 carries
// no customer legs and no UETR — it moves money between financial
// institutions, with the settlement method (SttlmMtd), the agent
// chain (DbtrAgt / CdtrAgt) and the charges bearer (ChrgsBr) as its
// first-class routing data.
//
// The ledger models FI counterparties as accounts: the transfer's
// payer (From) is the debtor's institution and the payee (To) the
// creditor's institution. There is no BIC registry in the ledger, so
// agents ride proprietary identification
// (`FinInstnId/Othr/Id` = the account ID) — an honest, documented
// mapping, not a fabricated BIC. The transfer is direct FI-to-FI:
// Dbtr/DbtrAgt/InstgAgt collapse to the payer account and
// CdtrAgt/Cdtr/InstdAgt to the payee account; intermediary-agent
// chains (IntrmyAgt1..3) are out of scope and never emitted.
//
// Settlement method vocabulary (SttlmMtd): the real ISO 20022
// external settlement-method codes admitted by the machine:
//
//	INDA  instructed agent: settlement through the instructed agent
//	      (the CdtrAgt) — the default, the ordinary correspondent
//	      debit/credit.
//	INGA  instructing agent: settlement through the instructing agent
//	      (the DbtrAgt).
//	COVE  cover method: the transfer is covered separately.
//	CLRG  clearing system: settlement through a clearing system.
//
// Charges bearer vocabulary (ChrgsBr): the real ISO 20022 external
// charges-type codes:
//
//	DEBT  borne by the debtor (OUR)
//	CRED  borne by the creditor (BEN)
//	SHAR  shared (SHA)
//	SLEV  following service level / the agent's own arrangement —
//	      the default when the operator does not say who pays.
//
// Fail-closed: an unknown transfer ID, a transfer that settled
// cross-currency (the principal entry books the payer's leg to the FX
// clearing account in the source currency, so it does not describe the
// full FI-to-FI credit transfer — see fx.go), and a settlement method
// or charges bearer outside the admitted vocabularies are all refused.
// The machine only emits real interbank codes; it does not invent
// them.
//
// MsgId is deterministic: `<transfer ID>/ficdt`. The export opens no
// record, so repeat exports of the same transfer carry the same MsgId;
// the MsgId is stable across identical exports rather than unique per
// call — a statement of fact, not an attempt sequence.
//
// Read-only: the principal journal entry is read as a snapshot copy
// under the read lock — no version bump, no journal rows, no audit
// event — the same contract as ExportPacs004, ExportPacs002 and
// ExportCAMT053. The durable record of the interbank transfer stays
// with the bank that issued the pacs.009; the ledger keeps no copy
// because the export is a statement of fact, not an operator
// attestation.

// Admitted pacs.009 settlement-method codes (SttlmMtd).
const (
	Pacs009SttlmInstructedAgent  = "INDA"
	Pacs009SttlmInstructingAgent = "INGA"
	Pacs009SttlmCover            = "COVE"
	Pacs009SttlmClearingSystem   = "CLRG"
)

// Admitted pacs.009 charges-bearer codes (ChrgsBr).
const (
	Pacs009ChargesDebtor   = "DEBT"
	Pacs009ChargesCreditor = "CRED"
	Pacs009ChargesShared   = "SHAR"
	Pacs009ChargesService  = "SLEV"
)

// DefaultPacs009SettlementMethod is the settlement method used when
// the operator does not name one: the instructed agent settles, the
// ordinary correspondent debit/credit.
const DefaultPacs009SettlementMethod = Pacs009SttlmInstructedAgent

// DefaultPacs009ChargesBearer is the charges bearer used when the
// operator does not name one: following service level — the ledger
// does not presume who pays the agents' charges.
const DefaultPacs009ChargesBearer = Pacs009ChargesService

// pacs.009 export errors.
var (
	// ErrPacs009FXTransfer is returned by ExportPacs009 for a
	// cross-currency transfer: its principal entry books only the
	// payer-side leg against the FX clearing account, so rendering it
	// as a single FI-to-FI credit transfer would misstate the money
	// movement. Fail-closed.
	ErrPacs009FXTransfer = errors.New("ledger: pacs.009 export: cross-currency transfers are not supported")
	// ErrPacs009InvalidSettlementMethod is returned when the
	// settlement method is outside the admitted SttlmMtd vocabulary.
	ErrPacs009InvalidSettlementMethod = errors.New("ledger: pacs.009 export: unknown settlement method")
	// ErrPacs009InvalidChargesBearer is returned when the charges
	// bearer is outside the admitted ChrgsBr vocabulary.
	ErrPacs009InvalidChargesBearer = errors.New("ledger: pacs.009 export: unknown charges bearer")
)

// Pacs009Options tunes one pacs.009 export. The zero value renders
// the ordinary correspondent transfer: INDA settlement, SLEV
// charges.
type Pacs009Options struct {
	// SettlementMethod is one of INDA/INGA/COVE/CLRG; empty means the
	// default (INDA).
	SettlementMethod string
	// ChargesBearer is one of DEBT/CRED/SHAR/SLEV; empty means the
	// default (SLEV).
	ChargesBearer string
}

// pacs009ValidSettlementMethod reports whether code is in the
// admitted SttlmMtd vocabulary.
func pacs009ValidSettlementMethod(code string) bool {
	switch code {
	case Pacs009SttlmInstructedAgent,
		Pacs009SttlmInstructingAgent,
		Pacs009SttlmCover,
		Pacs009SttlmClearingSystem:
		return true
	}
	return false
}

// pacs009ValidChargesBearer reports whether code is in the admitted
// ChrgsBr vocabulary.
func pacs009ValidChargesBearer(code string) bool {
	switch code {
	case Pacs009ChargesDebtor,
		Pacs009ChargesCreditor,
		Pacs009ChargesShared,
		Pacs009ChargesService:
		return true
	}
	return false
}

// pacs009MsgID builds the deterministic export MsgId:
// "<transfer ID>/ficdt". Stable across repeat exports of the same
// transfer — a statement of fact, not an attempt sequence.
func pacs009MsgID(transferID string) string {
	return transferID + "/ficdt"
}

// pacs.009 wire structs: Document/FICdtTrf. The namespace rides the
// Document element, the camt053_export.go convention, so real
// pacs.009 readers accept the document.
type pacs009Document struct {
	XMLName xml.Name      `xml:"urn:iso:std:iso:20022:tech:xsd:pacs.009.001.06 Document"`
	Trf     pacs009FICdtTrf `xml:"FICdtTrf"`
}

type pacs009FICdtTrf struct {
	GrpHdr pacs009GrpHdr  `xml:"GrpHdr"`
	TxInf  pacs009TxInf   `xml:"CdtTrfTxInf"`
}

type pacs009GrpHdr struct {
	MsgID    string          `xml:"MsgId"`
	CreDtTm  string          `xml:"CreDtTm"`
	NbOfTxs  string          `xml:"NbOfTxs"`
	SttlmInf pacs009SttlmInf `xml:"SttlmInf"`
}

type pacs009SttlmInf struct {
	SttlmMtd string `xml:"SttlmMtd"`
}

type pacs009TxInf struct {
	PmtID          pacs009PmtID   `xml:"PmtId"`
	IntrBkSttlmAmt pacs009Amt     `xml:"IntrBkSttlmAmt"`
	IntrBkSttlmDt  string         `xml:"IntrBkSttlmDt"`
	InstgAgt       pacs009Agent   `xml:"InstgAgt"`
	InstdAgt       pacs009Agent   `xml:"InstdAgt"`
	Dbtr           pacs009Agent   `xml:"Dbtr"`
	DbtrAgt        pacs009Agent   `xml:"DbtrAgt"`
	CdtrAgt        pacs009Agent   `xml:"CdtrAgt"`
	Cdtr           pacs009Agent   `xml:"Cdtr"`
	ChrgsBr        string         `xml:"ChrgsBr"`
	// Purp carries the transfer's ISO 20022 business purpose
	// (ExternalPurpose1Code, see purpose.go, LG-180) as
	// <Purp><Cd>. It is omitted when the transfer states no purpose —
	// the wire stays identical to the pre-purpose export.
	Purp *pacs009Purpose `xml:"Purp,omitempty"`
}

// pacs009Purpose renders one ISO 20022 purpose code: <Purp><Cd>SALA</Cd></Purp>.
type pacs009Purpose struct {
	Cd string `xml:"Cd"`
}

// pacs009PurposeFor maps a journaled purpose code to the wire
// element: nil for "no purpose stated", so the export omits Purp
// entirely. The code is already validated at post time (see
// checkPurposeCode), so this is a pure projection.
func pacs009PurposeFor(code string) *pacs009Purpose {
	if code == "" {
		return nil
	}
	return &pacs009Purpose{Cd: code}
}

type pacs009PmtID struct {
	InstrID    string `xml:"InstrId"`
	EndToEndID string `xml:"EndToEndId"`
	TxID       string `xml:"TxId"`
}

type pacs009Amt struct {
	// Ccy rides the amount element as an attribute, the pacs.008
	// IntrBkSttlmAmt convention.
	Ccy   string `xml:"Ccy,attr"`
	Value string `xml:",chardata"`
}

// pacs009Agent identifies one FI counterparty. The ledger keeps no
// BIC registry, so the agent rides proprietary identification with
// the account ID — the honest mapping, documented above; never a
// fabricated BIC.
type pacs009Agent struct {
	FinInstnID pacs009FinInstnID `xml:"FinInstnId"`
}

type pacs009FinInstnID struct {
	Othr pacs009Othr `xml:"Othr"`
}

type pacs009Othr struct {
	ID string `xml:"Id"`
}

func pacs009AgentFor(account AccountID) pacs009Agent {
	return pacs009Agent{FinInstnID: pacs009FinInstnID{Othr: pacs009Othr{ID: string(account)}}}
}

// ExportPacs009 renders the posted transfer behind transferID as an
// ISO 20022 pacs.009.001.06 FinancialInstitutionCreditTransfer
// document, the bank-to-bank credit transfer for wholesale /
// correspondent-banking settlement.
//
// The transfer is located by its principal journal entry (the
// TransferFeeSplit convention): the entry's credit account is the
// debtor's institution (payer), the debit account the creditor's
// institution (payee), and the entry's amount and currency the
// interbank settlement amount.
//
// Fail-closed: an unknown transfer ID (ErrTransferNotFound), a
// cross-currency transfer (ErrPacs009FXTransfer — the principal entry
// books only the payer-side leg against the FX clearing account), a
// settlement method outside the admitted SttlmMtd vocabulary
// (ErrPacs009InvalidSettlementMethod), and a charges bearer outside
// the admitted ChrgsBr vocabulary (ErrPacs009InvalidChargesBearer)
// are all refused.
//
// Read-only: the entry is read as a snapshot copy under the read
// lock — no version bump, no journal rows, no audit event.
func (l *Ledger) ExportPacs009(transferID string, opts Pacs009Options) ([]byte, error) {
	sttlmMtd := strings.TrimSpace(opts.SettlementMethod)
	if sttlmMtd == "" {
		sttlmMtd = DefaultPacs009SettlementMethod
	}
	if !pacs009ValidSettlementMethod(sttlmMtd) {
		return nil, fmt.Errorf("%w: %q", ErrPacs009InvalidSettlementMethod, sttlmMtd)
	}
	chrgsBr := strings.TrimSpace(opts.ChargesBearer)
	if chrgsBr == "" {
		chrgsBr = DefaultPacs009ChargesBearer
	}
	if !pacs009ValidChargesBearer(chrgsBr) {
		return nil, fmt.Errorf("%w: %q", ErrPacs009InvalidChargesBearer, chrgsBr)
	}
	l.mu.RLock()
	e, ok := l.entries[transferID]
	if !ok {
		l.mu.RUnlock()
		return nil, fmt.Errorf("%w: %q", ErrTransferNotFound, transferID)
	}
	if _, isFX := l.entries[transferID+"/fx"]; isFX {
		l.mu.RUnlock()
		return nil, fmt.Errorf("%w: %q", ErrPacs009FXTransfer, transferID)
	}
	l.mu.RUnlock()
	// e is a value copy out of the map: the render below cannot
	// mutate ledger state even by accident.
	settleDate := e.CreatedAt.UTC().Format("2006-01-02")
	if e.CreatedAt.IsZero() {
		settleDate = time.Now().UTC().Format("2006-01-02")
	}
	doc := pacs009Document{
		Trf: pacs009FICdtTrf{
			GrpHdr: pacs009GrpHdr{
				MsgID:   pacs009MsgID(transferID),
				CreDtTm: time.Now().UTC().Format(time.RFC3339),
				NbOfTxs: "1",
				SttlmInf: pacs009SttlmInf{
					SttlmMtd: sttlmMtd,
				},
			},
			TxInf: pacs009TxInf{
				PmtID: pacs009PmtID{
					InstrID:    transferID,
					EndToEndID: transferID,
					TxID:       transferID,
				},
				IntrBkSttlmAmt: pacs009Amt{
					Ccy:   e.Currency,
					Value: pain001Amount(e.AmountCents),
				},
				IntrBkSttlmDt: settleDate,
				InstgAgt:      pacs009AgentFor(e.CreditAccount),
				InstdAgt:      pacs009AgentFor(e.DebitAccount),
				Dbtr:          pacs009AgentFor(e.CreditAccount),
				DbtrAgt:       pacs009AgentFor(e.CreditAccount),
				CdtrAgt:       pacs009AgentFor(e.DebitAccount),
				Cdtr:          pacs009AgentFor(e.DebitAccount),
				ChrgsBr:       chrgsBr,
				Purp:          pacs009PurposeFor(e.PurposeCode),
			},
		},
	}
	body, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("ledger: pacs.009 export: render failed: %v", err)
	}
	return append([]byte(xml.Header), body...), nil
}
