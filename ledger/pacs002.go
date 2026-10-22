package ledger

import (
	"encoding/xml"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// pacs.002.001.03 FI-to-FI payment status report export (LG-167).
//
// ExportPacs002 renders the gpi tracking record (see gpi.go, LG-165) as
// an ISO 20022 `pacs.002.001.03` `FIToFIPaymentStatusReport` — the
// bank-to-bank status report. It is the FI-to-FI counterpart of the
// customer-level `pain.002` report (see pain002.go, LG-162): pain.002
// answers a pain.001 instruction for the customer, pacs.002 reports
// the settlement status of the underlying FI-to-FI transfer for the
// correspondent chain.
//
// TxSts is the gpi mirror, not a new verdict: `tracked` and `ACSP`
// both report `ACSP` (the instruction is in the chain), `ACSC` reports
// `ACSC`, `ACCP` reports `ACCP`, and `RJCT` reports `RJCT` carrying the
// rejection reason code in `StsRsnInf`. The original instruction is
// modelled as `pacs.008.001.02` (the FI-to-FI customer credit transfer
// the payout settles) in `OrgnlGrpInfAndSts`, and the UETR rides
// `TxInfAndSts/OrgnlUETR` — the UETR is the end-to-end key of the
// whole pacs chain.
//
// `OrgnlEndToEndId` is the linked transfer ID when the track was
// linked to one (see LinkGPITransfer), else the payout ID: the best
// end-to-end reference the ledger actually holds.
//
// MsgId is deterministic: `<uetr>/<seq>` where seq is the count of
// recorded gpi transitions, so re-exports of the same track state
// carry the same MsgId and every advance yields a new one.
//
// Read-only: the export holds the read lock on a snapshot copy of the
// track (see GetGPITrack), bumps no version, books nothing, and appends
// no audit-chain link — the same contract as ExportCAMT053 and
// GenerateStatement. The durable record of a track's journey stays the
// hash-chained `gpi_status` audit events.
//
// Deliberately out of scope: driving the payout instruction. Like the
// pain.002 report, a status report is a statement of fact, not an
// operator attestation — SettlePayout/FailPayout stay operator-driven.

// pacs.002 export errors.
var (
	// ErrPacs002UnknownUETR is returned by ExportPacs002 for a UETR
	// the ledger never tracked: a status report can only describe a
	// tracked payment. Fail-closed, like the gpi query path.
	ErrPacs002UnknownUETR = errors.New("ledger: pacs.002 export: unknown gpi UETR")
	// ErrPacs002InvalidStatus is returned when the tracked record
	// carries a status outside the gpi vocabulary: the machine only
	// admits validated states (see AdvanceGPI), so this is a loud
	// internal invariant failure, not a coercion.
	ErrPacs002InvalidStatus = errors.New("ledger: pacs.002 export: invalid gpi status")
)

// pacs002OrgnlMsgName is the OrgnlMsgNmId the report answers: the
// FI-to-FI customer credit transfer instruction the payout settles.
const pacs002OrgnlMsgName = "pacs.008.001.02"

// pacs002TxSts maps a gpi tracking status to the pacs.002 TxSts
// vocabulary. `tracked` — the initial state, instruction opened but no
// bank verdict yet — maps to ACSP (AcceptedSettlementInProcess): the
// payment is in the chain. RJCT carries the reason code; the reason
// rides StsRsnInf, not TxSts.
func pacs002TxSts(gpiStatus string) (string, bool) {
	switch gpiStatus {
	case GPIStatusTracked, GPIStatusACSP:
		return "ACSP", true
	case GPIStatusACSC:
		return "ACSC", true
	case GPIStatusACCP:
		return "ACCP", true
	case GPIStatusRJCT:
		return "RJCT", true
	}
	return "", false
}

// pacs002MsgID builds the deterministic report MsgId: "<uetr>/<seq>"
// with seq the count of recorded gpi transitions. Repeat exports of
// the same track state share the MsgId; every advance changes it.
func pacs002MsgID(uetr string, seq int) string {
	return uetr + "/" + strconv.Itoa(seq)
}

// pacs.002 wire structs: Document/FIToFIPmtStsRpt. The namespace rides
// the Document element, the camt053_export.go convention, so real
// pacs.002 readers accept the document.
type pacs002Document struct {
	XMLName xml.Name  `xml:"urn:iso:std:iso:20022:tech:xsd:pacs.002.001.03 Document"`
	Report  pacs002Rpt `xml:"FIToFIPmtStsRpt"`
}

type pacs002Rpt struct {
	GrpHdr      pacs002GrpHdr      `xml:"GrpHdr"`
	OrgnlGrpInf pacs002OrgnlGrpInf `xml:"OrgnlGrpInfAndSts"`
	Tx          pacs002Tx          `xml:"TxInfAndSts"`
}

type pacs002GrpHdr struct {
	MsgID   string `xml:"MsgId"`
	CreDtTm string `xml:"CreDtTm"`
}

type pacs002OrgnlGrpInf struct {
	OrgnlMsgID   string `xml:"OrgnlMsgId"`
	OrgnlMsgNmID string `xml:"OrgnlMsgNmId"`
}

type pacs002Tx struct {
	OrgnlEndToEndID string            `xml:"OrgnlEndToEndId"`
	OrgnlUETR       string            `xml:"OrgnlUETR"`
	TxSts           string            `xml:"TxSts"`
	StsRsnInf       *pacs002StsRsnInf `xml:"StsRsnInf"`
}

type pacs002StsRsnInf struct {
	Rsn *pacs002Rsn `xml:"Rsn"`
}

type pacs002Rsn struct {
	// Prtry carries the reason code verbatim: gpi reason codes are
	// the bank's/operator's proprietary vocabulary (like the
	// pain001_status receipt reasons), not ISO external codes.
	Prtry string `xml:"Prtry"`
}

// ExportPacs002 renders the gpi tracking record for uetr as an ISO
// 20022 pacs.002.001.03 FIToFIPaymentStatusReport document.
//
// Read-only: the track is read as a snapshot copy under the read lock
// (see GetGPITrack) — no version bump, no journal rows, no audit
// event. An unknown UETR is refused fail-closed; a track whose status
// falls outside the gpi vocabulary is refused rather than coerced.
func (l *Ledger) ExportPacs002(uetr string) ([]byte, error) {
	track, ok := l.GetGPITrack(uetr)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrPacs002UnknownUETR, uetr)
	}
	txSts, ok := pacs002TxSts(track.Status)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrPacs002InvalidStatus, track.Status)
	}
	endToEndID := track.PayoutID
	if track.TransferID != "" {
		// The linked transfer is the stronger end-to-end
		// reference: it is the ID the correspondent chain
		// actually settles on.
		endToEndID = track.TransferID
	}
	tx := pacs002Tx{
		OrgnlEndToEndID: endToEndID,
		OrgnlUETR:       track.UETR,
		TxSts:           txSts,
	}
	if track.Status == GPIStatusRJCT && strings.TrimSpace(track.ReasonCode) != "" {
		// The rejection reason rides the report verbatim — it is
		// the bank's vocabulary, like the pain.002 reason
		// passthrough.
		tx.StsRsnInf = &pacs002StsRsnInf{
			Rsn: &pacs002Rsn{Prtry: strings.TrimSpace(track.ReasonCode)},
		}
	}
	doc := pacs002Document{
		Report: pacs002Rpt{
			GrpHdr: pacs002GrpHdr{
				MsgID:   pacs002MsgID(track.UETR, len(track.History)),
				CreDtTm: time.Now().UTC().Format(time.RFC3339),
			},
			OrgnlGrpInf: pacs002OrgnlGrpInf{
				OrgnlMsgID:   track.PayoutID,
				OrgnlMsgNmID: pacs002OrgnlMsgName,
			},
			Tx: tx,
		},
	}
	body, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPacs002InvalidStatus, err)
	}
	return append([]byte(xml.Header), body...), nil
}
