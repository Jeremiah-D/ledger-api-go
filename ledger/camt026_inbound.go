package ledger

import (
	"encoding/xml"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ISO 20022 camt.026.001.07 Unable to Apply inbound processing
// (LG-417): the counterparty bank's "I cannot execute / reconcile
// this instruction" report — the terminal-state half of the
// investigation family the ledger already speaks.
//
// camt.055 asks to cancel an in-flight instruction (LG-389),
// camt.056 is the FI-to-FI cancellation request (LG-168/414), and
// camt.029 resolves the cancellation (LG-168/174). camt.026 is the
// report that arrives when the agent on the other side cannot even
// execute the instruction: missing or incorrect information, a
// truncated instruction, an unexpected entry it cannot reconcile.
// Until now the ledger had no inbound port for it — a bank's
// unable-to-apply report had nowhere to land.
//
// Version note (checked 2026-11-14, not assumed): the registry
// knows camt.026.001.07 and later versions; the .07 version is the
// UnableToApplyV07 message (root <UblToApply>), confirmed by two
// sources that agree: the IBM world-wire
// iso20022/camt02600107 binding (UnableToApplyV07 =
// Assgnmt + Case? + Undrlyg + Justfn + SplmtryData*) and the
// open-payments-fednow Rust iso/camt_026_001_07 binding (the same
// five blocks in the same order). The parse therefore gates on the
// .001.07 namespace, not a string check (the camt.056 inbound
// convention, LG-414).
//
// Structure, verified against the IBM world-wire camt02600107
// binding:
//
//   - Assgnmt (CaseAssignment5): Id [1..1] (the report's MsgId,
//     the replay key), Assgnr [1..1], Assgne [1..1], CreDtTm [1..1].
//   - Case (Case5, [0..1]): Id, Cretr, ReopCaseIndctn — the
//     investigation case, when the assigner opens one.
//   - Undrlyg (UnderlyingTransaction5Choice, [1..1]): exactly one
//     of Initn (UnderlyingPaymentInstruction5: OrgnlGrpInf
//     {OrgnlMsgId, OrgnlMsgNmId, OrgnlCreDtTm}, OrgnlPmtInfId,
//     OrgnlInstrId, OrgnlEndToEndId, OrgnlUETR, OrgnlInstdAmt,
//     ReqdExctnDt, ReqdColltnDt, OrgnlTxRef), IntrBk
//     (UnderlyingPaymentTransaction4: OrgnlGrpInf, OrgnlInstrId,
//     OrgnlEndToEndId, OrgnlTxId, OrgnlUETR, OrgnlIntrBkSttlmAmt,
//     OrgnlIntrBkSttlmDt, OrgnlTxRef), or StmtNtry
//     (UnderlyingStatementEntry3: OrgnlGrpInf, OrgnlStmtId,
//     OrgnlNtryId, OrgnlUETR) — the instruction or entry the
//     counterparty cannot apply.
//   - Justfn (UnableToApplyJustification3Choice, [1..1]): exactly
//     one of AnyInf (all available information is requested),
//     MssngOrIncrrctInf (MissingOrIncorrectInformation3: MssngInf[]
//     / IncrrctInf[] each with Cd + AddtlMssngInf/AddtlIncrrctInf),
//     or PssblDplctInstr (the referred item may duplicate a
//     previous instruction or entry).
//   - SplmtryData [0..*]: additional information — not read.
//
// Association: the report names its instruction by UETR and/or
// EndToEndId, resolved in this order — a gpi track's UETR (payout
// or linked transfer, the GPITrack's PayoutID/TransferID), then
// the EndToEndId association the FedNow pacs.008 path already
// owns (associatePacs008Locked: payout by ID or idempotency key,
// transfer by idempotency key or entry ID), then an RfP by ID
// (the pain.013 EndToEndId is the RfP ID). When both references
// are stated they must agree on one target; a reference that
// resolves to nothing, or two references that disagree, is refused
// fail-closed (ErrCAMT026UnknownReference): an unable-to-apply
// report about an instruction the ledger never issued is evidence
// about someone else's payment.
//
// The apply records state and moves no money: no journal rows, no
// version bump, no freeze/overdraft/limit/period gate — the report
// is a fact about the counterparty's inability, not an instruction
// to act. What lands is the CAMT026ApplyResult (in-memory,
// keyed by MsgId) plus one hash-chained `camt026_unable_to_apply`
// audit event carrying the UETR, the reason code verbatim, and the
// justification kind. A report about an already-terminal
// instruction (a cancelled/settled/failed/expired payout, a gpi
// track already ACCP/RJCT, a posted transfer, a paid/rejected/
// expired RfP) is recorded the same way and flagged Terminal —
// audited, then a no-op: the terminal state is never revived.
//
// Idempotency: the Assgnmt Id (MsgId) is the replay key, recorded
// in camt026Reports. Re-applying the same document returns the
// stored result with Duplicate set, no second audit event (the
// pacs.003 replay convention). A MsgId reused for a different
// report (another reference or another reason) is refused with
// ErrCAMT026MsgIDConflict.
//
// Per-item failure, the LG-163 FedNow shape: a parseable report
// whose reference cannot be resolved is refused (404) but the
// refusal itself is written to the audit trail as
// `camt026_unable_to_apply_rejected` — and it writes no state, so
// a refused report never blocks a later, valid one. An illegal
// document (wrong version, no underlying arm, no reference, no
// justification) is refused as a whole at parse time (400) and
// never reaches the apply.

// camt.026 inbound errors.
var (
	// ErrInvalidCAMT026 is returned by ParseCAMT026 when a
	// document is not a parseable camt.026.001.07 Unable to Apply
	// report: empty input, malformed XML, a missing Assgnmt/Id,
	// no underlying arm (or two arms — the choice is exclusive),
	// no UETR or EndToEndId reference, or a missing
	// justification (or two — the choice is exclusive). The HTTP
	// layer maps it to 400.
	ErrInvalidCAMT026 = errors.New("ledger: invalid camt.026 unable-to-apply report")
	// ErrCAMT026UnknownReference is returned when a report names
	// a UETR or EndToEndId the ledger never tracked, or when the
	// two references disagree on the instruction: the report is
	// about an instruction the ledger cannot identify. It is a
	// 404-class error.
	ErrCAMT026UnknownReference = errors.New("ledger: camt.026 names an unknown instruction reference")
	// ErrCAMT026MsgIDConflict is returned when an Assgnmt Id
	// already recorded names a different report (another
	// reference or another reason): one message ID names one
	// report. It is a 422-class error.
	ErrCAMT026MsgIDConflict = errors.New("ledger: camt.026 MsgId already applied to a different report")
)

// CAMT026UnderlyingKind names which Undrlyg arm a report carried.
type CAMT026UnderlyingKind string

const (
	// CAMT026UnderlyingInitiation is the Undrlyg/Initn arm: the
	// original payment initiation.
	CAMT026UnderlyingInitiation CAMT026UnderlyingKind = "initiation"
	// CAMT026UnderlyingInterbank is the Undrlyg/IntrBk arm: the
	// original interbank payment transaction.
	CAMT026UnderlyingInterbank CAMT026UnderlyingKind = "interbank"
	// CAMT026UnderlyingStatementEntry is the Undrlyg/StmtNtry arm:
	// the original statement cash entry.
	CAMT026UnderlyingStatementEntry CAMT026UnderlyingKind = "statement-entry"
)

// CAMT026AssociationKind names what a report's reference resolved
// to: a payout instruction, a journaled transfer, or an RfP.
type CAMT026AssociationKind string

const (
	// CAMT026AssociatedPayout: the reference names a payout
	// instruction.
	CAMT026AssociatedPayout CAMT026AssociationKind = "payout"
	// CAMT026AssociatedTransfer: the reference names a journaled
	// transfer.
	CAMT026AssociatedTransfer CAMT026AssociationKind = "transfer"
	// CAMT026AssociatedRfP: the reference names a request for
	// payment.
	CAMT026AssociatedRfP CAMT026AssociationKind = "rfp"
)

// CAMT026Justification names which Justfn arm a report carried.
type CAMT026Justification string

const (
	// CAMT026JustificationAnyInformation is the Justfn/AnyInf arm:
	// all available information on the instruction is requested.
	CAMT026JustificationAnyInformation CAMT026Justification = "any-information"
	// CAMT026JustificationMissingOrIncorrect is the
	// Justfn/MssngOrIncrrctInf arm: named elements are missing or
	// incorrect.
	CAMT026JustificationMissingOrIncorrect CAMT026Justification = "missing-or-incorrect"
	// CAMT026JustificationPossibleDuplicate is the
	// Justfn/PssblDplctInstr arm: the referred item may duplicate
	// a previous instruction or entry.
	CAMT026JustificationPossibleDuplicate CAMT026Justification = "possible-duplicate"
)

// CAMT026Report is the parsed counterparty camt.026.001.07 Unable
// to Apply report: the report's own MsgId and case, the underlying
// instruction it cannot apply, and the reason the assigner gave,
// carried verbatim.
type CAMT026Report struct {
	// MsgID is Assgnmt/Id: the audit trace and the replay key.
	MsgID string `json:"msg_id"`
	// CaseID is Case/Id, when the assigner opens a case.
	CaseID string `json:"case_id,omitempty"`
	// Assignor and Assignee are Assgnmt/Assgnr and Assgnmt/Assgne
	// names: who reports, and to whom.
	Assignor string `json:"assignor,omitempty"`
	Assignee string `json:"assignee,omitempty"`
	// UnderlyingKind names the Undrlyg arm: initiation, interbank,
	// or statement-entry.
	UnderlyingKind CAMT026UnderlyingKind `json:"underlying_kind"`
	// UETR and EndToEndID are the underlying reference, from
	// whichever arm carried them. At least one is present.
	UETR       string `json:"uetr,omitempty"`
	EndToEndID string `json:"end_to_end_id,omitempty"`
	// OrgnlMsgID and OrgnlMsgNmID are the underlying message
	// reference (OrgnlGrpInf), when stated.
	OrgnlMsgID   string `json:"orgnl_msg_id,omitempty"`
	OrgnlMsgNmID string `json:"orgnl_msg_nm_id,omitempty"`
	// HasAmount reports whether the underlying arm stated an
	// amount (OrgnlInstdAmt / OrgnlIntrBkSttlmAmt); AmountCents /
	// Currency are its value. Informational only: the apply never
	// gates on the amount — the report records inability, it does
	// not re-price the instruction.
	HasAmount   bool   `json:"has_amount"`
	AmountCents int64  `json:"amount_cents,omitempty"`
	Currency    string `json:"currency,omitempty"`
	// ReasonCode is the justification's coded reason, verbatim:
	// the first MssngInf or IncrrctInf Cd. Empty when the
	// justification carries no code (AnyInf / PssblDplctInstr).
	// ReasonInfo is the first AddtlMssngInf / AddtlIncrrctInf
	// free text, when stated.
	ReasonCode string `json:"reason_code,omitempty"`
	ReasonInfo string `json:"reason_info,omitempty"`
	// Justification names the Justfn arm: any-information,
	// missing-or-incorrect, or possible-duplicate.
	Justification CAMT026Justification `json:"justification"`
}

// CAMT026ApplyResult is the recorded outcome of one applied
// camt.026 report: what it named, what it resolved to, and whether
// the target was already terminal.
type CAMT026ApplyResult struct {
	// MsgID is the report's Assgnmt/Id.
	MsgID string `json:"msg_id"`
	// UETR and EndToEndID are the report's reference, as parsed.
	UETR       string `json:"uetr,omitempty"`
	EndToEndID string `json:"end_to_end_id,omitempty"`
	// Kind is what the reference resolved to: payout, transfer,
	// or rfp. TargetID is that instruction's ID.
	Kind     CAMT026AssociationKind `json:"kind"`
	TargetID string                 `json:"target_id"`
	// ReasonCode and ReasonInfo are the report's reason, verbatim.
	ReasonCode string `json:"reason_code,omitempty"`
	ReasonInfo string `json:"reason_info,omitempty"`
	// Justification names the Justfn arm.
	Justification CAMT026Justification `json:"justification"`
	// Terminal reports that the target was already in a terminal
	// state when the report landed: the report was recorded and
	// audited, and nothing was revived.
	Terminal bool `json:"terminal"`
	// Duplicate reports an idempotent replay: the same report was
	// applied before, so the stored result is returned and nothing
	// new was recorded — no new audit event.
	Duplicate bool `json:"duplicate"`
	// AppliedAt is the receipt time of the first application.
	AppliedAt time.Time `json:"applied_at"`
}

// camt026Document is the wire shape ParseCAMT026 accepts:
// camt.026.001.07 UblToApply. The namespace rides the Document
// element, so a document in the wrong schema version fails the
// unmarshal — the version gate is the namespace, not a string
// check (the camt.056 inbound convention, LG-414). Only the
// fields the apply reads are modelled; unknown elements (notably
// SplmtryData) are skipped.
type camt026Document struct {
	XMLName xml.Name   `xml:"urn:iso:std:iso:20022:tech:xsd:camt.026.001.07 Document"`
	Report  camt026Ubl `xml:"UblToApply"`
}

type camt026Ubl struct {
	Assgnmt camt026Assgnmt `xml:"Assgnmt"`
	Case    *camt026Case   `xml:"Case"`
	Undrlyg camt026Undrlyg `xml:"Undrlyg"`
	Justfn  camt026Justfn  `xml:"Justfn"`
}

type camt026Assgnmt struct {
	ID      string       `xml:"Id"`
	Assgnr  camt026Party `xml:"Assgnr"`
	Assgne  camt026Party `xml:"Assgne"`
	CreDtTm string       `xml:"CreDtTm"`
}

type camt026Party struct {
	Nm string `xml:"Nm"`
}

type camt026Case struct {
	ID string `xml:"Id"`
}

type camt026Undrlyg struct {
	Initn    *camt026Initn    `xml:"Initn"`
	IntrBk   *camt026IntrBk   `xml:"IntrBk"`
	StmtNtry *camt026StmtNtry `xml:"StmtNtry"`
}

type camt026OrgnlGrpInf struct {
	OrgnlMsgID   string `xml:"OrgnlMsgId"`
	OrgnlMsgNmID string `xml:"OrgnlMsgNmId"`
	OrgnlCreDtTm string `xml:"OrgnlCreDtTm"`
}

type camt026Initn struct {
	OrgnlGrpInf     *camt026OrgnlGrpInf `xml:"OrgnlGrpInf"`
	OrgnlPmtInfID   string              `xml:"OrgnlPmtInfId"`
	OrgnlInstrID    string              `xml:"OrgnlInstrId"`
	OrgnlEndToEndID string              `xml:"OrgnlEndToEndId"`
	OrgnlUETR       string              `xml:"OrgnlUETR"`
	OrgnlInstdAmt   *camt026Amount      `xml:"OrgnlInstdAmt"`
}

type camt026IntrBk struct {
	OrgnlGrpInf         *camt026OrgnlGrpInf `xml:"OrgnlGrpInf"`
	OrgnlInstrID        string              `xml:"OrgnlInstrId"`
	OrgnlEndToEndID     string              `xml:"OrgnlEndToEndId"`
	OrgnlTxID           string              `xml:"OrgnlTxId"`
	OrgnlUETR           string              `xml:"OrgnlUETR"`
	OrgnlIntrBkSttlmAmt *camt026Amount      `xml:"OrgnlIntrBkSttlmAmt"`
}

type camt026StmtNtry struct {
	OrgnlGrpInf *camt026OrgnlGrpInf `xml:"OrgnlGrpInf"`
	OrgnlStmtID string              `xml:"OrgnlStmtId"`
	OrgnlNtryID string              `xml:"OrgnlNtryId"`
	OrgnlUETR   string              `xml:"OrgnlUETR"`
}

type camt026Amount struct {
	Ccy   string `xml:"Ccy,attr"`
	Value string `xml:",chardata"`
}

type camt026Justfn struct {
	AnyInf            *string                `xml:"AnyInf"`
	MssngOrIncrrctInf *camt026MssngOrIncrrct `xml:"MssngOrIncrrctInf"`
	PssblDplctInstr   *string                `xml:"PssblDplctInstr"`
}

type camt026MssngOrIncrrct struct {
	MssngInf   []camt026Info `xml:"MssngInf"`
	IncrrctInf []camt026Info `xml:"IncrrctInf"`
}

type camt026Info struct {
	Cd              string `xml:"Cd"`
	AddtlMssngInf   string `xml:"AddtlMssngInf"`
	AddtlIncrrctInf string `xml:"AddtlIncrrctInf"`
}

// camt026ParseAmount parses an ISO 20022 decimal amount ("1234.56")
// into minor units, the pacs003ParseAmount rules: the integer part
// holds at most 12 digits, the fraction at most two digits and is
// right-padded, the value must be positive. An unreadable amount is
// a parse refusal — the report is about an instruction whose amount
// the ledger cannot even read.
func camt026ParseAmount(body string) (int64, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return 0, fmt.Errorf("%w: empty amount", ErrInvalidCAMT026)
	}
	intPart := body
	fracPart := ""
	if i := strings.IndexByte(body, '.'); i >= 0 {
		intPart, fracPart = body[:i], body[i+1:]
		if strings.IndexByte(fracPart, '.') >= 0 {
			return 0, fmt.Errorf("%w: amount %q has two decimal points", ErrInvalidCAMT026, body)
		}
	}
	if intPart == "" || len(intPart) > 12 || len(fracPart) > 2 {
		return 0, fmt.Errorf("%w: amount %q", ErrInvalidCAMT026, body)
	}
	for _, c := range intPart + fracPart {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("%w: amount %q is not numeric", ErrInvalidCAMT026, body)
		}
	}
	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: amount %q", ErrInvalidCAMT026, body)
	}
	frac := int64(0)
	if fracPart != "" {
		if len(fracPart) == 1 {
			fracPart += "0"
		}
		frac, err = strconv.ParseInt(fracPart, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%w: amount %q", ErrInvalidCAMT026, body)
		}
	}
	cents := whole*100 + frac
	if cents <= 0 {
		return 0, fmt.Errorf("%w: amount %q is not positive", ErrInvalidCAMT026, body)
	}
	return cents, nil
}

// ParseCAMT026 parses a counterparty-sent ISO 20022 camt.026.001.07
// Unable to Apply report. It validates the document shape — the
// .001.07 namespace, well-formed XML, an Assgnmt/Id, exactly one
// Undrlyg arm carrying a UETR or EndToEndId reference, and exactly
// one Justfn arm — and refuses anything else fail-loud: a
// half-readable bank file must not partially apply. The reason code
// is the first MssngInf / IncrrctInf Cd, carried verbatim; the
// justification kind names the Justfn arm for reports that carry
// no code.
func ParseCAMT026(data []byte) (*CAMT026Report, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, fmt.Errorf("%w: empty document", ErrInvalidCAMT026)
	}
	var doc camt026Document
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidCAMT026, err)
	}
	msgID := strings.TrimSpace(doc.Report.Assgnmt.ID)
	if msgID == "" {
		return nil, fmt.Errorf("%w: missing Assgnmt/Id", ErrInvalidCAMT026)
	}
	// Exactly one Undrlyg arm: the choice is exclusive.
	arms := 0
	var kind CAMT026UnderlyingKind
	var uetr, endToEndID, orgnlMsgID, orgnlMsgNmID string
	var amount *camt026Amount
	if doc.Report.Undrlyg.Initn != nil {
		arms++
		kind = CAMT026UnderlyingInitiation
		u := doc.Report.Undrlyg.Initn
		uetr = strings.TrimSpace(u.OrgnlUETR)
		endToEndID = strings.TrimSpace(u.OrgnlEndToEndID)
		amount = u.OrgnlInstdAmt
		if u.OrgnlGrpInf != nil {
			orgnlMsgID = strings.TrimSpace(u.OrgnlGrpInf.OrgnlMsgID)
			orgnlMsgNmID = strings.TrimSpace(u.OrgnlGrpInf.OrgnlMsgNmID)
		}
	}
	if doc.Report.Undrlyg.IntrBk != nil {
		arms++
		kind = CAMT026UnderlyingInterbank
		u := doc.Report.Undrlyg.IntrBk
		uetr = strings.TrimSpace(u.OrgnlUETR)
		endToEndID = strings.TrimSpace(u.OrgnlEndToEndID)
		amount = u.OrgnlIntrBkSttlmAmt
		if u.OrgnlGrpInf != nil {
			orgnlMsgID = strings.TrimSpace(u.OrgnlGrpInf.OrgnlMsgID)
			orgnlMsgNmID = strings.TrimSpace(u.OrgnlGrpInf.OrgnlMsgNmID)
		}
	}
	if doc.Report.Undrlyg.StmtNtry != nil {
		arms++
		kind = CAMT026UnderlyingStatementEntry
		u := doc.Report.Undrlyg.StmtNtry
		uetr = strings.TrimSpace(u.OrgnlUETR)
		if u.OrgnlGrpInf != nil {
			orgnlMsgID = strings.TrimSpace(u.OrgnlGrpInf.OrgnlMsgID)
			orgnlMsgNmID = strings.TrimSpace(u.OrgnlGrpInf.OrgnlMsgNmID)
		}
	}
	if arms != 1 {
		return nil, fmt.Errorf("%w: Undrlyg must carry exactly one of Initn/IntrBk/StmtNtry, got %d", ErrInvalidCAMT026, arms)
	}
	if uetr == "" && endToEndID == "" {
		return nil, fmt.Errorf("%w: underlying instruction carries no UETR or EndToEndId", ErrInvalidCAMT026)
	}
	// Exactly one Justfn arm: the choice is exclusive within the
	// block. (A second <Justfn> element would overwrite the first
	// under encoding/xml, so only the in-block arms are countable
	// here — the same collapse the Undrlyg arms avoid by having
	// distinct element names.)
	justArms := 0
	var justification CAMT026Justification
	var reasonCode, reasonInfo string
	if doc.Report.Justfn.AnyInf != nil {
		justArms++
		justification = CAMT026JustificationAnyInformation
	}
	if doc.Report.Justfn.MssngOrIncrrctInf != nil {
		justArms++
		justification = CAMT026JustificationMissingOrIncorrect
		m := doc.Report.Justfn.MssngOrIncrrctInf
		for _, inf := range m.MssngInf {
			if reasonCode == "" {
				reasonCode = strings.TrimSpace(inf.Cd)
			}
			if reasonInfo == "" {
				reasonInfo = strings.TrimSpace(inf.AddtlMssngInf)
			}
		}
		for _, inf := range m.IncrrctInf {
			if reasonCode == "" {
				reasonCode = strings.TrimSpace(inf.Cd)
			}
			if reasonInfo == "" {
				reasonInfo = strings.TrimSpace(inf.AddtlIncrrctInf)
			}
		}
	}
	if doc.Report.Justfn.PssblDplctInstr != nil {
		justArms++
		justification = CAMT026JustificationPossibleDuplicate
	}
	if justArms != 1 {
		return nil, fmt.Errorf("%w: Justfn must carry exactly one of AnyInf/MssngOrIncrrctInf/PssblDplctInstr, got %d", ErrInvalidCAMT026, justArms)
	}
	rep := &CAMT026Report{
		MsgID:          msgID,
		Assignor:       strings.TrimSpace(doc.Report.Assgnmt.Assgnr.Nm),
		Assignee:       strings.TrimSpace(doc.Report.Assgnmt.Assgne.Nm),
		UnderlyingKind: kind,
		UETR:           uetr,
		EndToEndID:     endToEndID,
		OrgnlMsgID:     orgnlMsgID,
		OrgnlMsgNmID:   orgnlMsgNmID,
		ReasonCode:     reasonCode,
		ReasonInfo:     reasonInfo,
		Justification:  justification,
	}
	if doc.Report.Case != nil {
		rep.CaseID = strings.TrimSpace(doc.Report.Case.ID)
	}
	if amount != nil {
		cents, err := camt026ParseAmount(amount.Value)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(amount.Ccy) == "" {
			return nil, fmt.Errorf("%w: empty underlying amount currency", ErrInvalidCAMT026)
		}
		currency, err := normalizeCurrency(amount.Ccy)
		if err != nil {
			return nil, fmt.Errorf("%w: underlying currency %q", ErrInvalidCAMT026, amount.Ccy)
		}
		rep.HasAmount = true
		rep.AmountCents = cents
		rep.Currency = currency
	}
	return rep, nil
}

// camt026Association is the resolved target of a report's reference:
// what the instruction is, its ID, the accounts it touches, and
// whether it is already terminal.
type camt026Association struct {
	Kind     CAMT026AssociationKind
	ID       string
	Accounts []AccountID
	Terminal bool
}

// resolveCAMT026Locked resolves one reference (a UETR or an
// EndToEndId) against the ledger's tracked instructions. Callers
// must hold l.mu (either lock suffices).
func (l *Ledger) resolveCAMT026Locked(ref string, byUETR bool) (camt026Association, bool) {
	none := camt026Association{}
	if byUETR {
		if t, ok := l.gpiTracks[ref]; ok {
			if t.PayoutID != "" {
				if p, ok := l.payouts[t.PayoutID]; ok {
					return camt026Association{
						Kind:     CAMT026AssociatedPayout,
						ID:       p.ID,
						Accounts: []AccountID{p.PayeeAccount},
						Terminal: camt026PayoutTerminalLocked(p, t),
					}, true
				}
			}
			if t.TransferID != "" {
				if e, ok := l.entries[t.TransferID]; ok {
					return camt026Association{
						Kind:     CAMT026AssociatedTransfer,
						ID:       e.ID,
						Accounts: []AccountID{e.DebitAccount, e.CreditAccount},
						Terminal: true, // a journaled transfer is settled by construction
					}, true
				}
			}
		}
		return none, false
	}
	// EndToEndId: the FedNow association first (payout by ID/key,
	// transfer by key/entry ID), then an RfP by ID — the pain.013
	// EndToEndId is the RfP ID.
	assoc := l.associatePacs008Locked(ref)
	switch assoc.Kind {
	case Pacs008AssociatedPayout:
		p, ok := l.payouts[assoc.ID]
		if !ok {
			return none, false
		}
		terminal := false
		switch p.Status {
		case PayoutSettled, PayoutCancelled, PayoutFailed, PayoutExpired:
			terminal = true
		}
		if t, ok := l.gpiTracks[p.UETR]; ok && (t.Status == GPIStatusACCP || t.Status == GPIStatusRJCT) {
			terminal = true
		}
		return camt026Association{
			Kind:     CAMT026AssociatedPayout,
			ID:       p.ID,
			Accounts: []AccountID{p.PayeeAccount},
			Terminal: terminal,
		}, true
	case Pacs008AssociatedTransfer:
		return camt026Association{
			Kind:     CAMT026AssociatedTransfer,
			ID:       assoc.ID,
			Accounts: assoc.Accounts,
			Terminal: true, // a journaled transfer is settled by construction
		}, true
	}
	if r, ok := l.rfps[ref]; ok {
		terminal := false
		switch r.Status {
		case RfPStatusPaid, RfPStatusRejected, RfPStatusExpired:
			terminal = true
		}
		if rfpExpiredLocked(r, time.Now()) {
			terminal = true
		}
		return camt026Association{
			Kind:     CAMT026AssociatedRfP,
			ID:       r.ID,
			Accounts: []AccountID{r.Creditor, r.Debtor},
			Terminal: terminal,
		}, true
	}
	return none, false
}

// camt026PayoutTerminalLocked reports whether a payout is terminal:
// a settled/cancelled/failed/expired instruction, or a gpi track
// the bank already closed (ACCP/RJCT). Callers must hold l.mu.
func camt026PayoutTerminalLocked(p PayoutInstruction, t *GPITrack) bool {
	switch p.Status {
	case PayoutSettled, PayoutCancelled, PayoutFailed, PayoutExpired:
		return true
	}
	return t.Status == GPIStatusACCP || t.Status == GPIStatusRJCT
}

// ApplyCAMT026 applies a parsed counterparty camt.026.001.07 Unable
// to Apply report: the inbound half of the investigation family,
// the mirror of the operator-driven camt.056 cancellation request.
// The bank says it cannot execute or reconcile the instruction the
// report names; the ledger records that fact and moves no money.
//
// Fail-closed: an unknown reference (ErrCAMT026UnknownReference),
// or two references that disagree on the instruction, are refused
// — but the refusal is written to the audit trail as
// `camt026_unable_to_apply_rejected` and writes no state, so a
// refused report never blocks a later, valid one (the LG-163
// FedNow per-item shape). A terminal target is recorded the same
// way as a live one and flagged Terminal — audited, then a no-op:
// the terminal state is never revived, and the payout, transfer,
// or RfP itself is untouched (no status change, no amount change,
// no new legs).
//
// Idempotency is the Assgnmt Id (MsgId): re-applying the same
// document returns the stored result with Duplicate set, no second
// audit event. A MsgId reused for a different report is refused
// with ErrCAMT026MsgIDConflict.
//
// The record is in-memory only, like payout instructions and gpi
// tracks: the hash-chained camt026_unable_to_apply audit events
// are the durable record.
func (l *Ledger) ApplyCAMT026(rep *CAMT026Report, actor string) (*CAMT026ApplyResult, error) {
	if rep == nil {
		return nil, fmt.Errorf("%w: nil report", ErrInvalidCAMT026)
	}
	if strings.TrimSpace(rep.MsgID) == "" {
		return nil, fmt.Errorf("%w: missing Assgnmt/Id", ErrInvalidCAMT026)
	}
	if rep.UETR == "" && rep.EndToEndID == "" {
		return nil, fmt.Errorf("%w: report carries no UETR or EndToEndId", ErrInvalidCAMT026)
	}
	if actor == "" {
		actor = "ApplyCAMT026"
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if rec, ok := l.camt026Reports[rep.MsgID]; ok {
		if rec.UETR != rep.UETR || rec.EndToEndID != rep.EndToEndID || rec.ReasonCode != rep.ReasonCode {
			return nil, fmt.Errorf("%w: MsgId %q", ErrCAMT026MsgIDConflict, rep.MsgID)
		}
		out := rec
		out.Duplicate = true
		return &out, nil
	}
	// Resolve the reference: UETR first, then EndToEndId. When
	// both are stated they must agree on one target.
	var assoc camt026Association
	resolved := false
	if rep.UETR != "" {
		if a, ok := l.resolveCAMT026Locked(rep.UETR, true); ok {
			assoc, resolved = a, true
		}
	}
	if rep.EndToEndID != "" {
		if a, ok := l.resolveCAMT026Locked(rep.EndToEndID, false); ok {
			if resolved && (a.Kind != assoc.Kind || a.ID != assoc.ID) {
				l.emitAudit(AuditEvent{
					Op:            "camt026_unable_to_apply_rejected",
					Actor:         actor,
					TraceID:       rep.MsgID,
					VersionBefore: l.version,
					VersionAfter:  l.version,
					Details: map[string]any{
						"msg_id":        rep.MsgID,
						"uetr":          rep.UETR,
						"end_to_end_id": rep.EndToEndID,
						"reason_code":   rep.ReasonCode,
						"error":         "uetr and end-to-end id disagree",
					},
				})
				return nil, fmt.Errorf("%w: UETR %q and EndToEndId %q disagree", ErrCAMT026UnknownReference, rep.UETR, rep.EndToEndID)
			}
			assoc, resolved = a, true
		}
	}
	if !resolved {
		l.emitAudit(AuditEvent{
			Op:            "camt026_unable_to_apply_rejected",
			Actor:         actor,
			TraceID:       rep.MsgID,
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Details: map[string]any{
				"msg_id":        rep.MsgID,
				"uetr":          rep.UETR,
				"end_to_end_id": rep.EndToEndID,
				"reason_code":   rep.ReasonCode,
				"error":         "unknown instruction reference",
			},
		})
		return nil, fmt.Errorf("%w: uetr %q end-to-end id %q", ErrCAMT026UnknownReference, rep.UETR, rep.EndToEndID)
	}
	now := time.Now().UTC()
	result := CAMT026ApplyResult{
		MsgID:         rep.MsgID,
		UETR:          rep.UETR,
		EndToEndID:    rep.EndToEndID,
		Kind:          assoc.Kind,
		TargetID:      assoc.ID,
		ReasonCode:    rep.ReasonCode,
		ReasonInfo:    rep.ReasonInfo,
		Justification: rep.Justification,
		Terminal:      assoc.Terminal,
		AppliedAt:     now,
	}
	l.camt026Reports[rep.MsgID] = result
	details := map[string]any{
		"msg_id":        rep.MsgID,
		"uetr":          rep.UETR,
		"end_to_end_id": rep.EndToEndID,
		"kind":          string(assoc.Kind),
		"target_id":     assoc.ID,
		"reason_code":   rep.ReasonCode,
		"reason_info":   rep.ReasonInfo,
		"justification": string(rep.Justification),
		"terminal":      assoc.Terminal,
	}
	if rep.CaseID != "" {
		details["case_id"] = rep.CaseID
	}
	l.emitAudit(AuditEvent{
		Op:            "camt026_unable_to_apply",
		Actor:         actor,
		TraceID:       rep.MsgID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      assoc.Accounts,
		Details:       details,
	})
	out := result
	return &out, nil
}

// GetCAMT026Report returns the recorded camt.026 report for msgID,
// or false when no report was ever applied under it. The returned
// record is a copy — callers cannot mutate ledger state through it.
func (l *Ledger) GetCAMT026Report(msgID string) (CAMT026ApplyResult, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	rec, ok := l.camt026Reports[msgID]
	if !ok {
		return CAMT026ApplyResult{}, false
	}
	return rec, true
}
