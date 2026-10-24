package ledger

import (
	"encoding/xml"
	"errors"
	"fmt"
	"strings"
)

// pacs.028.001.03 FI-to-FI payment status request export (LG-195).
//
// ExportPacs028 renders the gpi tracking record (see gpi.go, LG-165)
// as an ISO 20022 `pacs.028.001.03` `FIToFIPaymentStatusRequest` —
// the request side of the pacs.002 report (LG-167): where pacs.002
// states what the chain did with a payment, pacs.028 asks the chain
// what happened to it. The request concerns exactly one payment,
// named end-to-end by its UETR, so the UETR rides the original-group
// info at group level rather than in a per-transaction block: there
// is one payment in question, and one group describing it.
//
// GrpHdr carries the deterministic MsgId, the creation timestamp, and
// the requester identification; OrgnlGrpInfAndSts names the original
// instruction (modelled as `pacs.008.001.02`, the FI-to-FI customer
// credit transfer the payout settles, the pacs.002 convention) and
// repeats the UETR and the requester, so the answering bank can route
// the reply without re-reading the header.
//
// Deterministic and read-only: every field comes from the snapshot
// copy of the track (see GetGPITrack) plus the caller-supplied
// requester — MsgId is `<uetr>/sts-req`, CreDtTm is the tracked
// payment's creation timestamp — so re-issuing the request for the
// same payment yields byte-identical output: a retry never mints a
// second, distinguishable request. No version bump, no journal rows,
// no audit event, the same contract as ExportPacs002.
//
// Fail-closed: an unknown UETR is refused with
// ErrPacs028UnknownUETR — a status request must never be fabricated
// for a payment the ledger never tracked — and an empty requester
// is refused with ErrPacs028RequesterRequired, because a request
// without an identified requester has nobody to answer to.
//
// Deliberately out of scope: answering the request. This module only
// builds the outbound question; an inbound pacs.002 report is applied
// through the existing pain.002/gpi mirror path, not here.

// pacs.028 export errors.
var (
	// ErrPacs028UnknownUETR is returned by ExportPacs028 for a UETR
	// the ledger never tracked: a status request can only ask about
	// a tracked payment. Fail-closed, like the gpi query path.
	ErrPacs028UnknownUETR = errors.New("ledger: pacs.028 export: unknown gpi UETR")
	// ErrPacs028RequesterRequired is returned by ExportPacs028 when
	// the requester identification is empty: the answering bank
	// needs an identified requester to reply to, so the request is
	// refused rather than sent anonymously.
	ErrPacs028RequesterRequired = errors.New("ledger: pacs.028 export: requester is required")
)

// pacs028OrgnlMsgName is the OrgnlMsgNmId the request asks about: the
// FI-to-FI customer credit transfer instruction the payout settles,
// the same modelling as the pacs.002 report (LG-167).
const pacs028OrgnlMsgName = "pacs.008.001.02"

// pacs028MsgID builds the deterministic request MsgId: "<uetr>/sts-req".
// One payment, one request identity — retries are byte-identical.
func pacs028MsgID(uetr string) string {
	return uetr + "/sts-req"
}

// pacs028Reqstr identifies the party asking for the status: the Id is
// carried verbatim, the same passthrough convention as the pacs.002
// rejection reason code — the ledger does not mint identities.
type pacs028Reqstr struct {
	ID string `xml:"Id"`
}

// pacs.028 wire structs: Document/FIToFIPmtStsReq. The namespace rides
// the Document element, the camt053_export.go convention, so real
// pacs.028 readers accept the document.
type pacs028Document struct {
	XMLName xml.Name  `xml:"urn:iso:std:iso:20022:tech:xsd:pacs.028.001.03 Document"`
	Request pacs028Req `xml:"FIToFIPmtStsReq"`
}

type pacs028Req struct {
	GrpHdr      pacs028GrpHdr      `xml:"GrpHdr"`
	OrgnlGrpInf pacs028OrgnlGrpInf `xml:"OrgnlGrpInfAndSts"`
}

type pacs028GrpHdr struct {
	MsgID   string        `xml:"MsgId"`
	CreDtTm string        `xml:"CreDtTm"`
	Reqstr  pacs028Reqstr `xml:"Reqstr"`
}

type pacs028OrgnlGrpInf struct {
	OrgnlMsgID   string        `xml:"OrgnlMsgId"`
	OrgnlMsgNmID string        `xml:"OrgnlMsgNmId"`
	OrgnlUETR    string        `xml:"OrgnlUETR"`
	Reqstr       pacs028Reqstr `xml:"Reqstr"`
}

// ExportPacs028 renders the gpi tracking record for uetr as an ISO
// 20022 pacs.028.001.03 FIToFIPaymentStatusRequest document asking
// about that payment, identified as coming from requester.
//
// Read-only and deterministic: the track is read as a snapshot copy
// under the read lock (see GetGPITrack) — no version bump, no journal
// rows, no audit event — and every document field derives from that
// snapshot plus the requester, so identical inputs yield identical
// bytes. An unknown UETR is refused fail-closed; an empty requester
// is refused rather than defaulted.
func (l *Ledger) ExportPacs028(uetr, requester string) ([]byte, error) {
	requester = strings.TrimSpace(requester)
	if requester == "" {
		return nil, ErrPacs028RequesterRequired
	}
	track, ok := l.GetGPITrack(uetr)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrPacs028UnknownUETR, uetr)
	}
	reqstr := pacs028Reqstr{ID: requester}
	doc := pacs028Document{
		Request: pacs028Req{
			GrpHdr: pacs028GrpHdr{
				MsgID:   pacs028MsgID(track.UETR),
				CreDtTm: track.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
				Reqstr:  reqstr,
			},
			OrgnlGrpInf: pacs028OrgnlGrpInf{
				OrgnlMsgID:   track.PayoutID,
				OrgnlMsgNmID: pacs028OrgnlMsgName,
				OrgnlUETR:    track.UETR,
				Reqstr:       reqstr,
			},
		},
	}
	body, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("ledger: pacs.028 export: marshal: %w", err)
	}
	return append([]byte(xml.Header), body...), nil
}
