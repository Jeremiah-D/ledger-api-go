package ledger

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"strings"
)

// ISO 20022 pacs.028.001.03 FI-to-FI payment status request answer
// (LG-412): the inbound half of the pacs.028 lifecycle — the answer
// to the question ExportPacs028 (see pacs028.go, LG-195) asks.
//
// A correspondent bank sends a pacs.028.001.03
// `FIToFIPmtStsReq` asking about one payment it names by UETR; this
// module parses that request and answers it with the pacs.002 report
// the track already renders (see ExportPacs002, pacs002.go, LG-167).
// Answering is "read the track, render pacs.002": the track's current
// status becomes the report's TxSts, the UETR rides OrgnlUETR, and the
// reply is returned verbatim — the gpi track is not advanced, nothing
// is booked, no version is bumped, and no audit event is appended
// (an answer is a statement of fact about the track, not an operator
// attestation, the same contract as ExportPacs028/ExportPacs002).
//
// The join is the UETR, cross-checked against the payout the request
// names: OrgnlMsgNmId must be `pacs.008.001.02` (the same
// OrgnlMsgNmId gate as the pacs.002 apply path, see pacs002_apply.go,
// LG-409, and the pain.002 precedent, pain002.go, LG-406), and
// OrgnlMsgId must be that UETR track's PayoutID — per the ledger's
// own export convention (ExportPacs028 names the original message by
// payout ID), never invented. A request without an identified
// requester is refused (ErrPacs028RequesterRequired, the export's
// error): a request with nobody to answer to has no answer.
//
// Repeat answers for the same track state render the same pacs.002
// MsgId (`<uetr>/<seq>`, seq the recorded-transition count) and the
// same TxSts; only the report's wall-clock CreDtTm moves (see
// ExportPacs002).
//
// Deliberately out of scope: advancing the gpi track, booking or
// attesting anything, scheduling automatic answers, and any
// pacs.028-to-pain.002 linkage. ExportPacs028 is untouched.

// pacs.028 status-request answer errors.
var (
	// ErrInvalidPacs028 is returned when the request is not a
	// parseable pacs.028.001.03 document: malformed XML, a missing
	// FIToFIPmtStsReq, or a missing/empty OrgnlMsgId or OrgnlUETR.
	// Fail-loud: a half-readable bank file must not partially
	// answer.
	ErrInvalidPacs028 = errors.New("ledger: invalid pacs.028 status request")
	// ErrPacs028UnexpectedMsgName is returned when OrgnlMsgNmId is
	// not "pacs.008.001.02": the request asks about a different
	// message type, and answering it from a gpi track would
	// misattribute the reply.
	ErrPacs028UnexpectedMsgName = errors.New("ledger: pacs.028 request is not for a pacs.008 message")
	// ErrPacs028ForeignMsgID is returned when OrgnlMsgId is not the
	// PayoutID of the track the request's UETR names: a status
	// request for another payment is a category error, not a
	// question this ledger can answer.
	ErrPacs028ForeignMsgID = errors.New("ledger: pacs.028 request names a message of another payment")
)

// Pacs028Request is the parsed correspondent-bank status request:
// the request's own MsgId, the original pacs.008 MsgId it asks about
// (OrgnlMsgID, a payout ID per the ledger's export convention), the
// original message name, the UETR the question names, and the
// requesting party's identification.
type Pacs028Request struct {
	MsgID        string `json:"msg_id"`
	OrgnlMsgID   string `json:"orgnl_msg_id"`
	OrgnlMsgNmID string `json:"orgnl_msg_nm_id"`
	UETR         string `json:"uetr"`
	Requester    string `json:"requester"`
}

// ParsePacs028 parses a correspondent bank's ISO 20022
// pacs.028.001.03 FI-to-FI payment status request. It reuses the
// export wire struct (see pacs028.go) and validates the document
// shape — well-formed XML carrying FIToFIPmtStsReq under the
// pacs.028.001.03 namespace, OrgnlMsgNmId == "pacs.008.001.02", a
// non-empty OrgnlMsgId, a non-empty OrgnlUETR, and a non-empty
// requester Id (GrpHdr or OrgnlGrpInfAndSts) — and refuses anything
// else fail-loud. The OrgnlMsgId-to-track cross-check needs the
// ledger, so it lives in AnswerPacs028, not here. Namespaces are
// carried on the Document element, the export convention, and
// unknown elements are skipped, so bank extensions do not break
// parsing.
func ParsePacs028(data []byte) (*Pacs028Request, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("%w: empty document", ErrInvalidPacs028)
	}
	var doc pacs028Document
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidPacs028, err)
	}
	grp := doc.Request.OrgnlGrpInf
	if strings.TrimSpace(doc.Request.GrpHdr.MsgID) == "" &&
		strings.TrimSpace(grp.OrgnlMsgID) == "" &&
		strings.TrimSpace(grp.OrgnlMsgNmID) == "" &&
		strings.TrimSpace(grp.OrgnlUETR) == "" {
		// Nothing asked about: either the document carries no
		// FIToFIPmtStsReq at all or it is under a different
		// namespace than pacs.028.001.03.
		return nil, fmt.Errorf("%w: missing FIToFIPmtStsReq", ErrInvalidPacs028)
	}
	msgNmID := strings.TrimSpace(grp.OrgnlMsgNmID)
	if msgNmID != pacs028OrgnlMsgName {
		return nil, fmt.Errorf("%w: got %q", ErrPacs028UnexpectedMsgName, grp.OrgnlMsgNmID)
	}
	orgnlMsgID := strings.TrimSpace(grp.OrgnlMsgID)
	if orgnlMsgID == "" {
		return nil, fmt.Errorf("%w: missing OrgnlMsgId", ErrInvalidPacs028)
	}
	uetr := strings.TrimSpace(grp.OrgnlUETR)
	if uetr == "" {
		return nil, fmt.Errorf("%w: missing OrgnlUETR", ErrInvalidPacs028)
	}
	// The requester rides GrpHdr and OrgnlGrpInfAndSts alike (the
	// export writes both); either copy identifies the requester.
	requester := strings.TrimSpace(doc.Request.GrpHdr.Reqstr.ID)
	if requester == "" {
		requester = strings.TrimSpace(grp.Reqstr.ID)
	}
	if requester == "" {
		return nil, ErrPacs028RequesterRequired
	}
	return &Pacs028Request{
		MsgID:        strings.TrimSpace(doc.Request.GrpHdr.MsgID),
		OrgnlMsgID:   orgnlMsgID,
		OrgnlMsgNmID: msgNmID,
		UETR:         uetr,
		Requester:    requester,
	}, nil
}

// AnswerPacs028 answers a parsed pacs.028 status request with the
// pacs.002.001.03 FI-to-FI payment status report for the track the
// request's UETR names (see ExportPacs002): the track's current
// status rendered as TxSts, the UETR as OrgnlUETR, a rejection
// reason code when the track is RJCT.
//
// The request is validated before anything is rendered: an unknown
// UETR is refused (ErrGPIUnknownUETR), an OrgnlMsgId that is not
// this track's PayoutID is refused (ErrPacs028ForeignMsgID), and
// the message-name and requester checks are re-applied for
// hand-built requests. Answering is read-only — the gpi track is
// not advanced, nothing is booked, no version is bumped, and no
// audit event is appended.
func (l *Ledger) AnswerPacs028(req *Pacs028Request) ([]byte, error) {
	if req == nil {
		return nil, fmt.Errorf("%w: nil request", ErrInvalidPacs028)
	}
	if strings.TrimSpace(req.OrgnlMsgNmID) != pacs028OrgnlMsgName {
		return nil, fmt.Errorf("%w: got %q", ErrPacs028UnexpectedMsgName, req.OrgnlMsgNmID)
	}
	if strings.TrimSpace(req.Requester) == "" {
		return nil, ErrPacs028RequesterRequired
	}
	if strings.TrimSpace(req.OrgnlMsgID) == "" {
		return nil, fmt.Errorf("%w: missing OrgnlMsgId", ErrInvalidPacs028)
	}
	track, ok := l.GetGPITrack(strings.TrimSpace(req.UETR))
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrGPIUnknownUETR, req.UETR)
	}
	// The UETR is the join key; the MsgId is the cross-check. A
	// track always carries its payout (see registerGPITrackLocked),
	// so the expected MsgId is always defined — an empty payout
	// fails closed rather than matching anything.
	if track.PayoutID == "" || strings.TrimSpace(req.OrgnlMsgID) != track.PayoutID {
		return nil, fmt.Errorf("%w: %q is not the payout of UETR %q", ErrPacs028ForeignMsgID, req.OrgnlMsgID, req.UETR)
	}
	return l.ExportPacs002(track.UETR)
}
