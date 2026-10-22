package ledger

import (
	"encoding/xml"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Outbound camt.029.001.03 cancellation status report (LG-174): the
// ledger-side re-issue of a recorded cancellation resolution.
//
// camt.056.001.02 is the request half of the cancellation flow (see
// ledger.ExportCancellationRequest in camt056.go); camt.029.001.03 is
// the resolution half. The ledger already parses the counterparty's
// camt.029 (ParseCAMT029 / ApplyCAMT029Report) — this is the outbound
// document that mirrors a resolution the ledger has already recorded,
// for re-issue, audit hand-off, or correspondent forwarding. It is
// deliberately NOT the counterparty's original byte stream: Assgnr
// names the resolving side with the same identifiers the camt.056
// export uses (the payee account's registered BIC when one exists,
// else the internal account ID), Assgte the payout's destination.
//
// Only a resolved cancellation (confirmed / rejected) has a
// resolution to report: confirmed becomes CxlSts ACCR
// (AcceptedCancellationRequest), rejected becomes RJCR
// (RejectedCancellationRequest). The resolution code rides
// CxlStsRsnInf — a "CODE: free text" code splits into Rsn/Cd +
// AddtlInf, a bare code without a colon rides AddtlInf alone, and an
// empty resolution code omits CxlStsRsnInf entirely (ACCR often
// carries no reason). The split mirrors camt029ResolutionReason, so
// ExportCAMT029Resolution -> ParseCAMT029 is an exact round trip: the
// parsed report re-applies to the same record as an idempotent no-op.
//
// Read-only: the record is read as a snapshot copy under the read
// lock — no version bump, no journal rows, no audit event. A UETR
// with no recorded cancellation, or with a requested-but-unresolved
// cancellation, is refused fail-closed: there is no resolution to
// report.

// ErrCancellationUnresolved is returned when a camt.029 resolution
// document is requested for a cancellation that was never resolved:
// a requested-but-open cancellation has no verdict yet, and a
// resolution document must not invent one.
var ErrCancellationUnresolved = errors.New("ledger: cancellation not resolved: no resolution to report")

// resolutionMsgID builds the deterministic resolution MsgId:
// "<uetr>/cxl/<seq>/rpt" — unique per cancellation attempt, stable
// across re-exports, the cancellationMsgID convention with the report
// suffix.
func resolutionMsgID(uetr string, seq int) string {
	return cancellationMsgID(uetr, seq) + "/rpt"
}

// camt.029 export wire structs: Document/FIToFICxlStsRpt. The
// namespace rides the Document element, the camt053_export.go
// convention. The RslvdCxl block reuses the camt056.go parse structs —
// the same fields the parser reads are the ones the exporter writes,
// which is what makes the round trip exact.
type camt029ExportDocument struct {
	XMLName xml.Name        `xml:"urn:iso:std:iso:20022:tech:xsd:camt.029.001.03 Document"`
	Report  camt029ExportRpt `xml:"FIToFICxlStsRpt"`
}

type camt029ExportRpt struct {
	Assgnmt  camt029ExportAssgnmt `xml:"Assgnmt"`
	RslvdCxl camt029RslvdCxl      `xml:"RslvdCxl"`
}

type camt029ExportAssgnmt struct {
	ID      string      `xml:"Id"`
	Assgnr  camt056Party `xml:"Assgnr"`
	Assgte  camt056Party `xml:"Assgte"`
	CreDtTm string      `xml:"CreDtTm"`
}

// ExportCAMT029Resolution renders the resolved cancellation for uetr
// as an ISO 20022 camt.029.001.03 FIToFICancellationStatusReport
// document — the wire form of the resolution already recorded on the
// ledger (see ResolvePayoutCancellation / ApplyCAMT029Report).
//
// The cancellation must exist and be resolved (confirmed or rejected);
// a requested-but-unresolved cancellation and an unknown UETR are
// refused fail-closed. Read-only: snapshot copy under the read lock,
// no version bump, no journal rows, no audit event.
func (l *Ledger) ExportCAMT029Resolution(uetr string) ([]byte, error) {
	l.mu.RLock()
	c, ok := l.payoutCancellations[uetr]
	if !ok {
		l.mu.RUnlock()
		return nil, fmt.Errorf("%w: %q", ErrNoOpenCancellation, uetr)
	}
	if c.Status != CancellationConfirmed && c.Status != CancellationRejected {
		l.mu.RUnlock()
		return nil, fmt.Errorf("%w: %q is %s", ErrCancellationUnresolved, uetr, c.Status)
	}
	rec := copyCancellation(c)
	// Tracks are never deleted and cancellations only open from a
	// live track, so the payout and IBAN lookups below always hit.
	p := l.payouts[rec.PayoutID]
	iban, hasIBAN := l.accountIBANs[p.PayeeAccount]
	l.mu.RUnlock()

	cxlSts := camt029RJCR
	if rec.Status == CancellationConfirmed {
		cxlSts = camt029ACCR
	}
	rslvd := camt029RslvdCxl{
		OrgnlUETR:  rec.UETR,
		OrgnlMsgID: rec.PayoutID,
		CxlSts:     camt029CxlSts{Cd: cxlSts},
	}
	// The resolution reason, kept verbatim and mirrored back into the
	// wire shape ParseCAMT029 reads: "CODE: free text" splits into
	// Rsn/Cd + AddtlInf, a bare code rides AddtlInf alone, an empty
	// code omits the block entirely.
	if code := strings.TrimSpace(rec.ResolutionCode); code != "" {
		rsn := &camt029CxlStsRsnInf{}
		if i := strings.Index(code, ": "); i >= 0 {
			rsn.Rsn = &camt029Rsn{Cd: strings.TrimSpace(code[:i])}
			rsn.AddtlInf = strings.TrimSpace(code[i+2:])
		} else {
			rsn.AddtlInf = code
		}
		rslvd.CxlStsRsnInf = rsn
	}

	// Assgnr names the resolving side with the same identifiers the
	// camt.056 export uses: the registered BIC when one exists, else
	// the internal account ID — documented identifiers, never
	// invented.
	assignor := string(p.PayeeAccount)
	if hasIBAN && iban.BIC != "" {
		assignor = iban.BIC
	}
	doc := camt029ExportDocument{
		Report: camt029ExportRpt{
			Assgnmt: camt029ExportAssgnmt{
				ID:      resolutionMsgID(rec.UETR, rec.Seq),
				Assgnr:  camt056Party{Nm: assignor},
				Assgte:  camt056Party{Nm: p.Destination},
				CreDtTm: rec.ResolvedAt.UTC().Format(time.RFC3339),
			},
			RslvdCxl: rslvd,
		},
	}
	body, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidCAMT029, err)
	}
	return append([]byte(xml.Header), body...), nil
}
