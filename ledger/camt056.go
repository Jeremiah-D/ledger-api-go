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

// camt.056.001.02 FI-to-FI cancellation request + camt.029.001.03
// cancellation status report resolution (LG-168).
//
// A payout that was issued to the bank but is not yet settled can be
// recalled with an ISO 20022 `camt.056.001.02`
// `FIToFICancellationRequest`: the ledger locates the original
// instruction by UETR (and MsgId), attaches the operator's
// cancellation reason code (CxRsn), and opens a cancellation record.
// The counterparty answers with a `camt.029.001.03`
// `FIToFICancellationStatusReport`, which resolves the record:
//
//	requested -> confirmed   the counterparty accepted: the payout is
//	                         marked cancelled.
//	requested -> rejected    the counterparty refused: the payout
//	                         stays submitted, the reason rides the
//	                         audit event.
//
// Two drivers resolve the machine, the gpi.go convention:
//
//  1. The bank, via ApplyCAMT029Report: the parsed camt.029's CxlSts
//     ACCR (AcceptedCancellationRequest) confirms, RJCR
//     (RejectedCancellationRequest) rejects.
//  2. The operator, via ResolvePayoutCancellation: the human-readable
//     leg, for resolutions that arrive out-of-band.
//
// A confirmed cancellation marks the payout cancelled (submitted ->
// cancelled) with the usual payout_cancelled audit event and outbox
// event — the money never left, so no clearing-leg reversal is
// booked. The gpi track (see gpi.go) is deliberately left untouched:
// it mirrors the bank's settlement verdicts, and "withdrawn by
// cancellation request" is not a bank settlement verdict.
//
// Fail-closed: an unknown UETR, a settled track (ACCP), a terminal
// track (RJCT), an already-cancelled payout, a payout that was never
// issued (draft/approved) or already failed, and a camt.029 naming a
// UETR with no open cancellation are all refused — never silently
// dropped. Cancellation without a reason code is refused, the gpi RJCT
// convention: a withdrawal must say why.
//
// Records are in-memory only, like payout instructions and gpi tracks
// (see payout.go, gpi.go): the hash-chained cancellation_requested /
// cancellation_resolved audit events are the durable record, and they
// do not round-trip through disaster-recovery snapshots.

// Cancellation lifecycle states.
const (
	CancellationRequested = "requested"
	CancellationConfirmed = "confirmed"
	CancellationRejected  = "rejected"
)

// camt.029 CxlSts codes (the bank's resolution vocabulary).
const (
	camt029ACCR = "ACCR" // AcceptedCancellationRequest
	camt029RJCR = "RJCR" // RejectedCancellationRequest
)

// Cancellation errors.
var (
	// ErrCancellationUnknownUETR is returned when an operation names a
	// UETR the ledger never tracked: cancellations only address
	// tracked payments.
	ErrCancellationUnknownUETR = errors.New("ledger: unknown gpi UETR for cancellation")
	// ErrCancellationReasonRequired is returned when a cancellation
	// is requested without a reason code (CxRsn): a withdrawal must
	// say why, the gpi RJCT convention.
	ErrCancellationReasonRequired = errors.New("ledger: payout cancellation requires a reason code")
	// ErrCancellationSettled is returned when the payment already
	// settled (gpi ACCP) or the payout instruction already settled:
	// settled money cannot be recalled — the reversal API (see
	// reversal.go) is the honest path for settled funds.
	ErrCancellationSettled = errors.New("ledger: payout already settled: cancellation refused")
	// ErrCancellationAlreadyCancelled is returned when the payout is
	// already cancelled: a second cancellation is refused, not
	// silently absorbed.
	ErrCancellationAlreadyCancelled = errors.New("ledger: payout already cancelled")
	// ErrCancellationNotIssued is returned when the payout was never
	// issued to the bank (draft/approved): there is nothing in flight
	// to recall — CancelPayout covers the pre-issue path.
	ErrCancellationNotIssued = errors.New("ledger: payout not yet issued: nothing to cancel")
	// ErrCancellationTerminal is returned when the payment is in a
	// terminal non-settled state (gpi RJCT, failed payout): dead
	// money cannot be recalled.
	ErrCancellationTerminal = errors.New("ledger: payout in terminal state: cancellation refused")
	// ErrNoOpenCancellation is returned when an operation needs the
	// open cancellation for a UETR and there is none: a resolution
	// without a request is refused, not silently dropped.
	ErrNoOpenCancellation = errors.New("ledger: no open cancellation for UETR")
	// ErrCancellationAlreadyResolved is returned when a resolution is
	// attempted on an already-resolved cancellation with a different
	// outcome: resolutions are terminal, the one exception being an
	// identical duplicate which is an idempotent no-op.
	ErrCancellationAlreadyResolved = errors.New("ledger: cancellation already resolved")
	// ErrInvalidCancellationResolution is returned when a resolution
	// is not "confirmed" or "rejected": unknown resolutions are
	// refused, not coerced.
	ErrInvalidCancellationResolution = errors.New("ledger: invalid cancellation resolution: want confirmed or rejected")
	// ErrInvalidCAMT029 is returned when a document is not a parseable
	// camt.029.001.03 cancellation status report: malformed XML, a
	// missing FIToFICxlStsRpt/RslvdCxl, an empty OrgnlUETR, or a CxlSts
	// outside the ACCR/RJCR vocabulary. Fail-loud: a half-readable
	// bank file must not partially apply.
	ErrInvalidCAMT029 = errors.New("ledger: invalid camt.029 cancellation status report")
)

// PayoutCancellation is the cancellation record of one tracked
// payment: the UETR and payout it recalls, the per-UETR attempt
// sequence, the lifecycle state, the operator's reason code (CxRsn),
// and — once resolved — the resolution and the bank's reason.
type PayoutCancellation struct {
	UETR           string    `json:"uetr"`
	PayoutID       string    `json:"payout_id"`
	Seq            int       `json:"seq"`
	Status         string    `json:"status"`
	ReasonCode     string    `json:"reason_code"`
	MsgID          string    `json:"msg_id"`
	RequestedAt    time.Time `json:"requested_at"`
	RequestedBy    string    `json:"requested_by,omitempty"`
	ResolvedAt     time.Time `json:"resolved_at,omitempty"`
	Resolution     string    `json:"resolution,omitempty"`
	ResolutionCode string    `json:"resolution_code,omitempty"`
}

// cancellationMsgID builds the deterministic cancellation MsgId:
// "<uetr>/cxl/<seq>", unique per attempt, stable across re-exports.
func cancellationMsgID(uetr string, seq int) string {
	return uetr + "/cxl/" + strconv.Itoa(seq)
}

// copyCancellation returns a defensive copy of a cancellation record.
func copyCancellation(c *PayoutCancellation) PayoutCancellation {
	cp := *c
	return cp
}

// RequestPayoutCancellation opens a cancellation for the tracked
// payment: the operator-driven half of the camt.056 flow. The record
// is returned with status "requested"; the camt.056.001.02 document
// itself is rendered by ExportCancellationRequest.
//
// Fail-closed: unknown UETR, settled (ACCP) or terminal (RJCT) gpi
// track, already-cancelled payout, unissued (draft/approved) or failed
// payout, and an empty reason code are all refused. Requesting while a
// cancellation is already open is an idempotent no-op returning the
// open record (the AdvanceGPI same-state convention); a confirmed
// cancellation refuses re-request; a rejected one admits a fresh
// attempt with a new sequence number.
//
// The request emits one hash-chained cancellation_requested audit
// event. The record is in-memory only; the audit event is the durable
// record.
func (l *Ledger) RequestPayoutCancellation(uetr, reasonCode, actor string) (PayoutCancellation, error) {
	reasonCode = strings.TrimSpace(reasonCode)
	if reasonCode == "" {
		return PayoutCancellation{}, ErrCancellationReasonRequired
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.gpiTracks[uetr]
	if !ok {
		return PayoutCancellation{}, fmt.Errorf("%w: %q", ErrCancellationUnknownUETR, uetr)
	}
	if existing, open := l.payoutCancellations[uetr]; open {
		switch existing.Status {
		case CancellationRequested:
			// Idempotent replay: the request is already open,
			// return it unchanged — no duplicate audit event.
			return copyCancellation(existing), nil
		case CancellationConfirmed:
			return PayoutCancellation{}, fmt.Errorf("%w: %q", ErrCancellationAlreadyCancelled, uetr)
		case CancellationRejected:
			// The counterparty refused; the payment is still
			// unsettled, so the operator may try again with a
			// new attempt sequence. Falls through to create a
			// fresh record below.
		}
	}
	switch t.Status {
	case GPIStatusACCP:
		return PayoutCancellation{}, fmt.Errorf("%w: %q", ErrCancellationSettled, uetr)
	case GPIStatusRJCT:
		return PayoutCancellation{}, fmt.Errorf("%w: %q", ErrCancellationTerminal, uetr)
	}
	p, ok := l.payouts[t.PayoutID]
	if !ok {
		// Unreachable: tracks open only from CreatePayout, and
		// payouts are never deleted. Fail loud rather than
		// cancelling a ghost instruction.
		return PayoutCancellation{}, fmt.Errorf("%w: payout %q", ErrPayoutNotFound, t.PayoutID)
	}
	switch p.Status {
	case PayoutSubmitted:
		// The only state with money in flight.
	case PayoutSettled:
		return PayoutCancellation{}, fmt.Errorf("%w: %q", ErrCancellationSettled, uetr)
	case PayoutCancelled:
		return PayoutCancellation{}, fmt.Errorf("%w: %q", ErrCancellationAlreadyCancelled, uetr)
	case PayoutFailed:
		return PayoutCancellation{}, fmt.Errorf("%w: %q", ErrCancellationTerminal, uetr)
	default:
		// draft / approved: the instruction never left the
		// ledger — CancelPayout, not camt.056, is the path.
		return PayoutCancellation{}, fmt.Errorf("%w: %q", ErrCancellationNotIssued, uetr)
	}
	l.cancellationSeqs[uetr]++
	seq := l.cancellationSeqs[uetr]
	now := time.Now().UTC()
	rec := &PayoutCancellation{
		UETR:        uetr,
		PayoutID:    p.ID,
		Seq:         seq,
		Status:      CancellationRequested,
		ReasonCode:  reasonCode,
		MsgID:       cancellationMsgID(uetr, seq),
		RequestedAt: now,
		RequestedBy: actor,
	}
	l.payoutCancellations[uetr] = rec
	l.emitAudit(AuditEvent{
		Op:            "cancellation_requested",
		Actor:         actor,
		TraceID:       uetr,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{p.PayeeAccount},
		Details: map[string]any{
			"uetr":        uetr,
			"payout_id":   p.ID,
			"seq":         seq,
			"reason_code": reasonCode,
			"msg_id":      rec.MsgID,
		},
	})
	return copyCancellation(rec), nil
}

// GetPayoutCancellation returns the cancellation record for uetr, or
// false when no cancellation was ever requested. The returned record
// is a copy — callers cannot mutate ledger state through it.
func (l *Ledger) GetPayoutCancellation(uetr string) (PayoutCancellation, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	c, ok := l.payoutCancellations[uetr]
	if !ok {
		return PayoutCancellation{}, false
	}
	return copyCancellation(c), true
}

// camt.056 wire structs: Document/FIToFICxlReq. The namespace rides
// the Document element, the camt053_export.go convention.
type camt056Document struct {
	XMLName xml.Name     `xml:"urn:iso:std:iso:20022:tech:xsd:camt.056.001.02 Document"`
	Request camt056Req   `xml:"FIToFICxlReq"`
}

type camt056Req struct {
	Assgnmt camt056Assgnmt `xml:"Assgnmt"`
	Undrlyg camt056Undrlyg `xml:"Undrlyg"`
}

type camt056Assgnmt struct {
	ID     string      `xml:"Id"`
	Assgnr camt056Party `xml:"Assgnr"`
	Assgte camt056Party `xml:"Assgte"`
	CreDtTm string     `xml:"CreDtTm"`
}

type camt056Party struct {
	Nm string `xml:"Nm"`
}

type camt056Undrlyg struct {
	TxInf camt056TxInf `xml:"TxInf"`
}

type camt056TxInf struct {
	CxlID      string           `xml:"CxlId"`
	OrgnlUETR  string           `xml:"OrgnlUETR"`
	OrgnlGrpInf camt056OrgnlGrpInf `xml:"OrgnlGrpInf"`
	CxlRsn     camt056CxlRsn    `xml:"CxlRsn"`
}

type camt056OrgnlGrpInf struct {
	OrgnlMsgID   string `xml:"OrgnlMsgId"`
	OrgnlMsgNmID string `xml:"OrgnlMsgNmId"`
}

type camt056CxlRsn struct {
	Rsn *camt056Rsn `xml:"Rsn"`
}

type camt056Rsn struct {
	// Prtry carries the CxRsn reason code verbatim: cancellation
	// reasons are the operator's proprietary vocabulary, not ISO
	// external codes — the pacs002.go StsRsnInf convention.
	Prtry string `xml:"Prtry"`
}

// ExportCancellationRequest renders the open cancellation for uetr as
// an ISO 20022 camt.056.001.02 FIToFICancellationRequest document —
// the message the operator sends to the bank.
//
// The original instruction is located by UETR (OrgnlUETR) and payout
// ID (OrgnlGrpInf/OrgnlMsgId, modelled as pacs.008.001.02, the
// pacs002.go convention). Assgnr names the instructing side's
// best-known identifier — the payee account's registered BIC when one
// exists (see SetAccountIBAN), else the internal account ID — and
// Assgte the payout's external destination reference: these are the
// identifiers the ledger actually holds, documented rather than
// invented.
//
// Read-only: the record is read as a snapshot copy under the read
// lock — no version bump, no journal rows, no audit event. A UETR with
// no open (requested) cancellation is refused fail-closed.
func (l *Ledger) ExportCancellationRequest(uetr string) ([]byte, error) {
	l.mu.RLock()
	c, ok := l.payoutCancellations[uetr]
	if !ok || c.Status != CancellationRequested {
		l.mu.RUnlock()
		if !ok {
			return nil, fmt.Errorf("%w: %q", ErrNoOpenCancellation, uetr)
		}
		return nil, fmt.Errorf("%w: %q is %s", ErrNoOpenCancellation, uetr, c.Status)
	}
	rec := copyCancellation(c)
	// Tracks are never deleted and cancellations only open from a
	// live track, so the payout and IBAN lookups below always hit.
	p := l.payouts[rec.PayoutID]
	iban, hasIBAN := l.accountIBANs[p.PayeeAccount]
	l.mu.RUnlock()

	assignor := string(p.PayeeAccount)
	if hasIBAN && iban.BIC != "" {
		assignor = iban.BIC
	}
	doc := camt056Document{
		Request: camt056Req{
			Assgnmt: camt056Assgnmt{
				ID:      rec.MsgID,
				Assgnr:  camt056Party{Nm: assignor},
				Assgte:  camt056Party{Nm: p.Destination},
				CreDtTm: rec.RequestedAt.UTC().Format(time.RFC3339),
			},
			Undrlyg: camt056Undrlyg{
				TxInf: camt056TxInf{
					CxlID:     rec.MsgID,
					OrgnlUETR: rec.UETR,
					OrgnlGrpInf: camt056OrgnlGrpInf{
						OrgnlMsgID:   rec.PayoutID,
						OrgnlMsgNmID: pacs002OrgnlMsgName,
					},
					CxlRsn: camt056CxlRsn{Rsn: &camt056Rsn{Prtry: rec.ReasonCode}},
				},
			},
		},
	}
	body, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoOpenCancellation, err)
	}
	return append([]byte(xml.Header), body...), nil
}

// camt.029 wire structs: Document/FIToFICxlStsRpt. Namespaces are
// ignored and unknown elements skipped on parse, the pain002.go
// convention — bank extensions must not refuse the resolutions the
// ledger understands.
type camt029Document struct {
	XMLName xml.Name   `xml:"Document"`
	Report  camt029Rpt `xml:"FIToFICxlStsRpt"`
}

type camt029Rpt struct {
	Assgnmt camt029Assgnmt `xml:"Assgnmt"`
	RslvdCxl *camt029RslvdCxl `xml:"RslvdCxl"`
}

type camt029Assgnmt struct {
	ID string `xml:"Id"`
}

type camt029RslvdCxl struct {
	OrgnlUETR    string              `xml:"OrgnlUETR"`
	OrgnlMsgID   string              `xml:"OrgnlMsgId"`
	CxlSts       camt029CxlSts       `xml:"CxlSts"`
	CxlStsRsnInf *camt029CxlStsRsnInf `xml:"CxlStsRsnInf"`
}

type camt029CxlSts struct {
	Cd string `xml:"Cd"`
}

type camt029CxlStsRsnInf struct {
	Rsn      *camt029Rsn `xml:"Rsn"`
	AddtlInf string      `xml:"AddtlInf"`
}

type camt029Rsn struct {
	Cd    string `xml:"Cd"`
	Prtry string `xml:"Prtry"`
}

// CAMT029Report is the parsed counterparty cancellation status
// report: the report's own MsgId, the UETR and MsgId it resolves, the
// resolution (confirmed/rejected), and the bank's reason split into
// code and free text.
type CAMT029Report struct {
	MsgID          string `json:"msg_id"`
	OrgnlUETR      string `json:"orgnl_uetr"`
	OrgnlMsgID     string `json:"orgnl_msg_id"`
	Resolution     string `json:"resolution"`
	ReasonCode     string `json:"reason_code,omitempty"`
	ReasonInfo     string `json:"reason_info,omitempty"`
}

// camt029ValidCxlSts reports whether s is in the camt.029 CxlSts
// vocabulary.
func camt029ValidCxlSts(s string) bool {
	return s == camt029ACCR || s == camt029RJCR
}

// ParseCAMT029 parses a counterparty-returned ISO 20022
// camt.029.001.03 FIToFICancellationStatusReport. It validates the
// document shape — well-formed XML, the FIToFICxlStsRpt root, a
// RslvdCxl block with a non-empty OrgnlUETR, and a CxlSts in the
// ACCR/RJCR vocabulary — and refuses anything else fail-loud: a
// half-readable bank file must not partially apply. ACCR becomes
// "confirmed", RJCR becomes "rejected"; the bank's reason code and
// free text are carried through verbatim for the audit trail.
func ParseCAMT029(data []byte) (*CAMT029Report, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("%w: empty document", ErrInvalidCAMT029)
	}
	var doc camt029Document
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidCAMT029, err)
	}
	if doc.Report.RslvdCxl == nil {
		return nil, fmt.Errorf("%w: missing FIToFICxlStsRpt/RslvdCxl", ErrInvalidCAMT029)
	}
	r := doc.Report.RslvdCxl
	uetr := strings.TrimSpace(r.OrgnlUETR)
	if uetr == "" {
		return nil, fmt.Errorf("%w: missing OrgnlUETR", ErrInvalidCAMT029)
	}
	cxlSts := strings.TrimSpace(r.CxlSts.Cd)
	if !camt029ValidCxlSts(cxlSts) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidCAMT029, r.CxlSts.Cd)
	}
	report := &CAMT029Report{
		MsgID:      strings.TrimSpace(doc.Report.Assgnmt.ID),
		OrgnlUETR:  uetr,
		OrgnlMsgID: strings.TrimSpace(r.OrgnlMsgID),
	}
	if cxlSts == camt029ACCR {
		report.Resolution = CancellationConfirmed
	} else {
		report.Resolution = CancellationRejected
	}
	if r.CxlStsRsnInf != nil {
		// The bank's reason, kept verbatim: code plus free text,
		// never interpreted — the bank's vocabulary, like the
		// pain.002 reason passthrough.
		if r.CxlStsRsnInf.Rsn != nil {
			report.ReasonCode = strings.TrimSpace(r.CxlStsRsnInf.Rsn.Cd)
			if report.ReasonCode == "" {
				report.ReasonCode = strings.TrimSpace(r.CxlStsRsnInf.Rsn.Prtry)
			}
		}
		report.ReasonInfo = strings.TrimSpace(r.CxlStsRsnInf.AddtlInf)
	}
	return report, nil
}

// camt029ResolutionReason renders a rejected resolution's reason the
// way the cancellation_resolved audit event records it: "CODE: free
// text", or whichever half the bank supplied — the
// pain002ReceiptReason convention.
func camt029ResolutionReason(report *CAMT029Report) string {
	switch {
	case report.ReasonCode != "" && report.ReasonInfo != "":
		return report.ReasonCode + ": " + report.ReasonInfo
	case report.ReasonCode != "":
		return report.ReasonCode
	default:
		return report.ReasonInfo
	}
}

// ResolvePayoutCancellation resolves the open cancellation for uetr:
// the operator-driven leg of the resolution. "confirmed" marks the
// payout cancelled (submitted -> cancelled) — the money never left,
// so no clearing-leg reversal is booked; "rejected" leaves the payout
// submitted.
//
// Resolutions are terminal: resolving an already-resolved record with
// the same resolution is an idempotent no-op (the pain.001 receipt
// replay convention); a different resolution is refused with
// ErrCancellationAlreadyResolved. A UETR with no open cancellation is
// refused fail-closed.
//
// Every applied resolution emits one hash-chained
// cancellation_resolved audit event. A confirmed resolution
// additionally emits the payout_cancelled audit event and the
// payout_cancelled outbox event, the CancelPayout convention.
func (l *Ledger) ResolvePayoutCancellation(uetr, resolution, resolutionCode, actor string) (PayoutCancellation, error) {
	if resolution != CancellationConfirmed && resolution != CancellationRejected {
		return PayoutCancellation{}, fmt.Errorf("%w: %q", ErrInvalidCancellationResolution, resolution)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	c, ok := l.payoutCancellations[uetr]
	if !ok {
		return PayoutCancellation{}, fmt.Errorf("%w: %q", ErrNoOpenCancellation, uetr)
	}
	if c.Status != CancellationRequested {
		if c.Resolution == resolution {
			// Idempotent replay of the same resolution.
			return copyCancellation(c), nil
		}
		return PayoutCancellation{}, fmt.Errorf("%w: %q is %s", ErrCancellationAlreadyResolved, uetr, c.Status)
	}
	resolutionCode = strings.TrimSpace(resolutionCode)
	now := time.Now().UTC()
	c.Status = resolution
	c.Resolution = resolution
	c.ResolutionCode = resolutionCode
	c.ResolvedAt = now
	l.emitAudit(AuditEvent{
		Op:            "cancellation_resolved",
		Actor:         actor,
		TraceID:       uetr,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{l.payouts[c.PayoutID].PayeeAccount},
		Details: map[string]any{
			"uetr":            uetr,
			"payout_id":       c.PayoutID,
			"seq":             c.Seq,
			"resolution":      resolution,
			"resolution_code": resolutionCode,
			"msg_id":          c.MsgID,
		},
	})
	if resolution == CancellationConfirmed {
		// The counterparty accepted the recall: the instruction is
		// withdrawn. submitted -> cancelled, the CancelPayout
		// audit/outbox shape, with the resolution as the cause.
		p := l.payouts[c.PayoutID]
		fromStatus := p.Status
		p.Status = PayoutCancelled
		l.payouts[c.PayoutID] = p
		l.emitAudit(AuditEvent{
			Op:            "payout_cancelled",
			Actor:         actor,
			TraceID:       c.PayoutID,
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Accounts:      []AccountID{p.PayeeAccount},
			Details: map[string]any{
				"amount_cents": p.AmountCents,
				"from_status":  fromStatus,
				"resolution":   "camt.029",
				"uetr":         uetr,
			},
		})
		l.emitOutboxLocked(OutboxEvent{
			Type:        OutboxPayoutCancelled,
			Account:     p.PayeeAccount,
			AmountCents: p.AmountCents,
			Currency:    p.Currency,
			Details:     map[string]any{"payout_id": c.PayoutID, "uetr": uetr},
		})
	}
	return copyCancellation(c), nil
}

// ApplyCAMT029Report applies a parsed camt.029 report to the open
// cancellation it resolves: the bank-driven leg. ACCR confirms (the
// payout is marked cancelled), RJCR rejects with the bank's reason
// code in the audit event. The application reuses
// ResolvePayoutCancellation, so idempotency, terminal-state
// protection, and the audit trail are inherited rather than
// reimplemented — the ApplyPain002Report convention.
//
// An unknown UETR or a UETR with no open cancellation is refused
// fail-closed: a resolution without a request is not silently dropped.
func (l *Ledger) ApplyCAMT029Report(report *CAMT029Report, actor string) (PayoutCancellation, error) {
	if report == nil {
		return PayoutCancellation{}, fmt.Errorf("%w: nil report", ErrInvalidCAMT029)
	}
	if _, ok := l.gpiTracks[report.OrgnlUETR]; !ok {
		return PayoutCancellation{}, fmt.Errorf("%w: %q", ErrCancellationUnknownUETR, report.OrgnlUETR)
	}
	return l.ResolvePayoutCancellation(report.OrgnlUETR, report.Resolution, camt029ResolutionReason(report), actor)
}
