package ledger

import (
	"encoding/xml"
	"errors"
	"fmt"
	"time"
)

// FedNow pacs.008.001.08 OUTBOUND customer credit transfer export
// (LG-176, fintech).
//
// ExportPacs008 renders a payout instruction as an ISO 20022
// `pacs.008.001.08` `FIToFICstmrCdtTrf` customer credit transfer
// document — the FedNow instant-payment instruction the ledger's bank
// would hand to the network. It is the outbound twin of the inbound
// parse path (see fednow_pacs008.go, LG-163): that package reads
// pacs.008 off the wire; this one writes it. The two meet in the
// middle: an exported document must parse as valid pacs.008 through
// ParsePacs008.
//
// Identity wiring:
//
//   - E2E Id: the payout's idempotency key — the same key the inbound
//     association resolves (see associatePacs008Locked), so the
//     bank's acknowledgment round-trips onto the original
//     instruction. A payout drafted without an idempotency key falls
//     back to the payout ID (the association resolves that too);
//     an EndToEndId is never emitted empty.
//   - UETR: the gpi track's Unique End-to-end Transaction Reference,
//     resolved through the PayoutID→UETR index (l.gpiByPayout) opened
//     by CreatePayout (see gpi.go, LG-165). It rides PmtId/UETR —
//     the pacs.008.001.08 slot for the end-to-end reference —
//     associating the outbound instruction with the gpi journey.
//   - MsgId is deterministic: `<payout>/pacs008`. The export opens no
//     record, so repeat exports of the same payout carry the same
//     MsgId; stable across identical exports, like the pacs.009
//     convention.
//
// Amount / currency / agent mapping reuses the pacs.009 conventions
// (see pacs009.go): the interbank settlement amount carries the Ccy
// attribute and renders through pain001Amount, and agents ride
// proprietary identification (`FinInstnId/Othr/Id` = the account ID) —
// the ledger keeps no BIC registry, so a fabricated BIC is never
// emitted. The payer's institution is the payout's payee account
// (the ledger account the instruction leaves); the creditor side is
// the payout's external destination reference, passed through
// verbatim — the ledger has no routing table for the destination, so
// it identifies the creditor agent by the operator's own reference
// rather than inventing one.
//
// Settlement: GrpHdr/SttlmInf carries SttlmMtd INDA — instructed
// agent: the transfer settles through the instructed agent, i.e. the
// FedNow service. IntrBkSttlmDt is the payout's draft date (the
// instruction date), today when the draft time is unknown — an
// instant payment settles same-day.
//
// Fail-closed: an unknown payout ID (ErrPayoutNotFound), a payout
// with no gpi tracking record (ErrPacs008ExportNoGPITrack — without
// the UETR the instruction cannot be tracked end-to-end), a non-USD
// payout (ErrPacs008ExportNonUSD — FedNow settles USD only), and a
// non-positive amount (ErrPacs008ExportNonPositiveAmount) are all
// refused.
//
// Read-only: the payout is read as a snapshot copy under the read
// lock — no version bump, no journal rows, no audit event — the same
// contract as ExportPacs009. The wire counters are the only mutation:
// pacs008Exports is the ledger-side source of the
// ledger_pacs008_exports_total metric (see Pacs008ExportCount), the
// same ledger-counts-at-process-time pattern as FedNowPostings.

// pacs008ExportCurrency is the only currency the export admits: FedNow
// settles USD only.
const pacs008ExportCurrency = "USD"

// pacs008ExportSttlmMtd is the settlement method the export renders:
// INDA — instructed agent, settlement through the instructed agent,
// i.e. the FedNow service.
const pacs008ExportSttlmMtd = "INDA"

// pacs.008 outbound export errors.
var (
	// ErrPacs008ExportNoGPITrack is returned by ExportPacs008 when the
	// payout has no gpi tracking record: the UETR is the gpi join key,
	// and a pacs.008 without it cannot be tracked end-to-end.
	// Fail-closed.
	ErrPacs008ExportNoGPITrack = errors.New("ledger: pacs.008 export: payout has no gpi tracking record")
	// ErrPacs008ExportNonUSD is returned by ExportPacs008 for a
	// non-USD payout: FedNow settles USD only, so rendering a
	// foreign-currency instruction as a FedNow credit transfer would
	// misstate the network. Fail-closed.
	ErrPacs008ExportNonUSD = errors.New("ledger: pacs.008 export: FedNow is USD-only")
	// ErrPacs008ExportNonPositiveAmount is returned by ExportPacs008
	// for a payout with a non-positive amount. Fail-closed.
	ErrPacs008ExportNonPositiveAmount = errors.New("ledger: pacs.008 export: payout amount must be greater than zero")
)

// pacs008ExportMsgID builds the deterministic export MsgId:
// "<payout>/pacs008". Stable across repeat exports of the same
// payout — a statement of fact, not an attempt sequence.
func pacs008ExportMsgID(payoutID string) string {
	return payoutID + "/pacs008"
}

// pacs008Out* structs model the pacs.008.001.08 FIToFICstmrCdtTrf wire
// shape for OUTBOUND generation. The `Out` infix keeps them off the
// parse structs in fednow_pacs008.go (same package): those read
// namespace-tolerant documents off the wire, these write the
// namespaced document the bank would send. The namespace rides the
// Document element, the pacs.009 export convention.
type pacs008OutDocument struct {
	XMLName xml.Name     `xml:"urn:iso:std:iso:20022:tech:xsd:pacs.008.001.08 Document"`
	Trf     pacs008OutTrf `xml:"FIToFICstmrCdtTrf"`
}

type pacs008OutTrf struct {
	GrpHdr pacs008OutGrpHdr `xml:"GrpHdr"`
	TxInf  pacs008OutTxInf  `xml:"CdtTrfTxInf"`
}

type pacs008OutGrpHdr struct {
	MsgID    string             `xml:"MsgId"`
	CreDtTm  string             `xml:"CreDtTm"`
	NbOfTxs  string             `xml:"NbOfTxs"`
	SttlmInf pacs008OutSttlmInf `xml:"SttlmInf"`
}

type pacs008OutSttlmInf struct {
	SttlmMtd string `xml:"SttlmMtd"`
}

type pacs008OutTxInf struct {
	PmtID          pacs008OutPmtID `xml:"PmtId"`
	IntrBkSttlmAmt pacs008OutAmt   `xml:"IntrBkSttlmAmt"`
	IntrBkSttlmDt  string          `xml:"IntrBkSttlmDt"`
	Dbtr           pacs008OutAgent `xml:"Dbtr"`
	DbtrAcct       pacs008OutAcct  `xml:"DbtrAcct"`
	DbtrAgt        pacs008OutAgent `xml:"DbtrAgt"`
	CdtrAgt        pacs008OutAgent `xml:"CdtrAgt"`
	Cdtr           pacs008OutAgent `xml:"Cdtr"`
	CdtrAcct       pacs008OutAcct  `xml:"CdtrAcct"`
}

type pacs008OutPmtID struct {
	InstrID    string `xml:"InstrId"`
	EndToEndID string `xml:"EndToEndId"`
	TxID       string `xml:"TxId"`
	UETR       string `xml:"UETR"`
}

type pacs008OutAmt struct {
	// Ccy rides the amount element as an attribute, the pacs.009
	// IntrBkSttlmAmt convention.
	Ccy   string `xml:"Ccy,attr"`
	Value string `xml:",chardata"`
}

// pacs008OutAgent identifies one party by proprietary identification,
// the pacs.009 convention: the ledger keeps no BIC registry, so the
// agent rides `FinInstnId/Othr/Id` = the account ID (or, for the
// creditor side, the operator's external destination reference passed
// through verbatim) — never a fabricated BIC.
type pacs008OutAgent struct {
	FinInstnID pacs008OutFinInstnID `xml:"FinInstnId"`
}

type pacs008OutFinInstnID struct {
	Othr pacs008OutOthr `xml:"Othr"`
}

type pacs008OutOthr struct {
	ID string `xml:"Id"`
}

// pacs008OutAcct identifies one account by proprietary Other/Id — the
// shape the inbound parser resolves (see pacs008AccountIDOf).
type pacs008OutAcct struct {
	ID pacs008OutAcctID `xml:"Id"`
}

type pacs008OutAcctID struct {
	Othr pacs008OutOthr `xml:"Othr"`
}

func pacs008OutAgentFor(id string) pacs008OutAgent {
	return pacs008OutAgent{FinInstnID: pacs008OutFinInstnID{Othr: pacs008OutOthr{ID: id}}}
}

func pacs008OutAcctFor(id string) pacs008OutAcct {
	return pacs008OutAcct{ID: pacs008OutAcctID{Othr: pacs008OutOthr{ID: id}}}
}

// ExportPacs008 renders the payout instruction behind payoutID as an
// ISO 20022 pacs.008.001.08 FIToFICstmrCdtTrf document: the outbound
// FedNow instant customer credit transfer the ledger's bank would
// send. The exported document parses as valid pacs.008 through
// ParsePacs008.
//
// Identity: EndToEndId is the payout's idempotency key (falling back
// to the payout ID when no key was set — the inbound association
// resolves both, see associatePacs008Locked); the UETR is the gpi
// track's reference resolved through l.gpiByPayout; MsgId is the
// deterministic "<payout>/pacs008". The payer side is the payout's
// payee account, the creditor side the payout's external destination
// reference, both under proprietary identification.
//
// Fail-closed: an unknown payout ID (ErrPayoutNotFound), a payout
// with no gpi tracking record (ErrPacs008ExportNoGPITrack), a
// non-USD payout (ErrPacs008ExportNonUSD — FedNow settles USD only),
// and a non-positive amount (ErrPacs008ExportNonPositiveAmount) are
// all refused.
//
// Read-only: the payout is read as a snapshot copy under the read
// lock — no version bump, no journal rows, no audit event. A
// successful export bumps the pacs.008 export counter (see
// Pacs008ExportCount); failed exports move nothing.
func (l *Ledger) ExportPacs008(payoutID string) ([]byte, error) {
	l.mu.RLock()
	p, ok := l.payouts[payoutID]
	if !ok {
		l.mu.RUnlock()
		return nil, fmt.Errorf("%w: %q", ErrPayoutNotFound, payoutID)
	}
	uetr, tracked := l.gpiByPayout[payoutID]
	if !tracked || uetr == "" {
		l.mu.RUnlock()
		return nil, fmt.Errorf("%w: payout %q", ErrPacs008ExportNoGPITrack, payoutID)
	}
	if p.Currency != pacs008ExportCurrency {
		l.mu.RUnlock()
		return nil, fmt.Errorf("%w: payout %q currency %q", ErrPacs008ExportNonUSD, payoutID, p.Currency)
	}
	if p.AmountCents <= 0 {
		l.mu.RUnlock()
		return nil, fmt.Errorf("%w: payout %q", ErrPacs008ExportNonPositiveAmount, payoutID)
	}
	l.mu.RUnlock()
	// p is a value copy out of the map and uetr a string copy: the
	// render below cannot mutate ledger state even by accident.

	e2e := p.IdempotencyKey
	if e2e == "" {
		// The inbound association resolves the payout ID too (see
		// associatePacs008Locked): an export without an EndToEndId
		// would not round-trip through ParsePacs008, which
		// requires it.
		e2e = p.ID
	}
	settleDate := p.CreatedAt.UTC().Format("2006-01-02")
	if p.CreatedAt.IsZero() {
		settleDate = time.Now().UTC().Format("2006-01-02")
	}
	payer := string(p.PayeeAccount)
	dest := p.Destination
	doc := pacs008OutDocument{
		Trf: pacs008OutTrf{
			GrpHdr: pacs008OutGrpHdr{
				MsgID:   pacs008ExportMsgID(payoutID),
				CreDtTm: time.Now().UTC().Format(time.RFC3339),
				NbOfTxs: "1",
				SttlmInf: pacs008OutSttlmInf{
					SttlmMtd: pacs008ExportSttlmMtd,
				},
			},
			TxInf: pacs008OutTxInf{
				PmtID: pacs008OutPmtID{
					InstrID:    payoutID,
					EndToEndID: e2e,
					TxID:       payoutID,
					UETR:       uetr,
				},
				IntrBkSttlmAmt: pacs008OutAmt{
					Ccy:   p.Currency,
					Value: pain001Amount(p.AmountCents),
				},
				IntrBkSttlmDt: settleDate,
				Dbtr:          pacs008OutAgentFor(payer),
				DbtrAcct:       pacs008OutAcctFor(payer),
				DbtrAgt:        pacs008OutAgentFor(payer),
				CdtrAgt:        pacs008OutAgentFor(dest),
				Cdtr:           pacs008OutAgentFor(dest),
				CdtrAcct:       pacs008OutAcctFor(dest),
			},
		},
	}
	body, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("ledger: pacs.008 export: render failed: %v", err)
	}
	l.pacs008Exports.Add(1)
	return append([]byte(xml.Header), body...), nil
}

// Pacs008ExportCount reports the ledger-side pacs.008.001.08 export
// counter: the source of the ledger_pacs008_exports_total metric.
// Counted at export time inside ExportPacs008 — the HTTP layer only
// mirrors it on scrape, like FedNowPostings. In-memory only, like the
// other export and process counters.
func (l *Ledger) Pacs008ExportCount() uint64 {
	return l.pacs008Exports.Load()
}
