package ledger

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ISO 20022 pain.012.001.03 mandate acceptance report parsing and
// application (LG-405): the inbound half of the mandate lifecycle's
// confirmation move.
//
// ExportPain009 (LG-199) initiates a mandate with the debtor's bank,
// ExportPain010 (LG-207) amends it, ExportPain011 (LG-402) asks the
// bank to cancel it — and the bank answers each of those requests
// with a pain.012 mandate acceptance report naming the original
// request's MsgId (OrgnlMsgInf/MsgId) and message name
// (OrgnlMsgInf/MsgNmId) plus, per mandate, one verdict:
//
//	accepted — Accptd true: the bank took the request.
//	rejected — Accptd false: the bank refused it, with the bank's
//	  reason code (RjctRsn/Cd or Prtry) and free text
//	  (AddtlRjctRsnInf) riding the receipt verbatim — the bank's
//	  vocabulary, not the ledger's, like the pain.002 reason rule.
//
// Version note (checked 2026-11-13, not assumed): pain.012.001.02
// is not a registered ISO 20022 version. Exact-phrase searches for
// "pain.012.001.02" / "MandateAcceptanceReportV02" return no such
// document; the registry knows V01 (2009, CGI-MP template
// pain.012.001.01, MandateAcceptanceReportV01), V03 (Payments
// Mandates MDR, May 2013, MandateAcceptanceReportV03 — Nordea MIG
// pain.012.001.03, 2017-01-02, which also confirms the shape below:
// <MndtAccptncRpt> [1..1] with GrpHdr + UndrlygAccptncDtls [1..n],
// each UndrlygAccptncDtls carrying OrgnlMsgInf [0..1] (MsgId [1..1],
// MsgNmId [1..1]), AccptncRslt [1..1] (Accptd [1..1] YesNoIndicator,
// RjctRsn [0..1] MandateReason1Choice Cd/Prtry, AddtlRjctRsnInf
// [0..n]) and OrgnlMndt [1..1] OriginalMandate2Choice whose
// identification arm is OrgnlMndtId [1..1]), V04 (generated
// bindings, MandateAcceptanceReportV04, same shape) and V06. Per the
// LG-402 fallback rule the parser therefore pins .001.03. The wire
// struct tags name local elements only, like ParsePain002, so a
// bank's namespace spelling is accepted without matching it — the
// .001.03 xmlns is the pinned version, not a parse gate.
//
// Application reuses the pain.002 inbound pattern: the whole batch
// is validated before anything is applied (unknown mandate, illegal
// verdict, or a prefix/name mismatch refuses the report, never a
// single mandate), a re-applied report MsgId is a no-op returning
// the original receipt, and every applied verdict emits one
// hash-chained audit event (pain012_mandate_accepted /
// pain012_mandate_rejected) and is recorded in the mandateAcceptances
// registry — the latest bank answer per mandate, for operators to
// read. The verdict never moves the mandate row: a rejection does
// not cancel or suspend anything — the operator still decides through
// CancelMandate (mandate.go). A report never invents a state the
// bank did not report, and pain.012 has no PDNG semantics: every
// verdict is binary, accepted or rejected, so there is no pending
// branch to skip.
//
// Deliberately out of scope: generating a pain.012 (this module only
// consumes the bank's report), any pain.012 -> pain.008 linkage, and
// any reason-code allowlist — no code vocabulary is asserted here,
// so a code the ledger has never seen still rides the receipt.

// pain.012 report errors.
var (
	// ErrInvalidPain012 is returned when the report is not a parseable
	// pain.012.001.03 document: malformed XML, a missing
	// MndtAccptncRpt, a missing GrpHdr MsgId, no
	// UndrlygAccptncDtls, or a detail missing its original message
	// reference or its mandate identification. Fail-loud: a
	// half-readable bank file must not partially apply.
	ErrInvalidPain012 = errors.New("ledger: invalid pain.012 mandate acceptance report")
	// ErrPain012UnexpectedMsgName is returned when OrgnlMsgNmId is not
	// one of pain.009.001.02, pain.010.001.02, or pain.011.001.03: the
	// report answers a message this ledger does not send, and
	// applying it to a mandate would misattribute the bank's verdict.
	ErrPain012UnexpectedMsgName = errors.New("ledger: pain.012 report is not for a pain.009, pain.010 or pain.011 message")
	// ErrPain012MsgNameMismatch is returned when OrgnlMsgId's prefix
	// does not match OrgnlMsgNmId (pain009- answers pain.009.001.02,
	// pain010- answers pain.010.001.02, pain011- answers
	// pain.011.001.03): a mis-addressed report fails loud instead of
	// attributing one family's verdict to another's request.
	ErrPain012MsgNameMismatch = errors.New("ledger: pain.012 report OrgnlMsgId prefix does not match OrgnlMsgNmId")
	// ErrInvalidPain012Verdict is returned when a detail carries no
	// verdict — a missing AccptncRslt or a missing Accptd — or when
	// ApplyPain012Report is handed a verdict outside the
	// accepted/rejected vocabulary. An empty verdict is fail-closed,
	// never read as a rejection or an acceptance.
	ErrInvalidPain012Verdict = errors.New("ledger: invalid pain.012 mandate verdict: Accptd is required")
)

// pain012XMLNS is the pinned ISO 20022 namespace for the
// pain.012.001.03 mandate acceptance report (see the version note
// above): V01 is the 2009 original, V03 the Maintenance 2013
// revision, and no V02 was ever registered.
const pain012XMLNS = "urn:iso:std:iso:20022:tech:xsd:pain.012.001.03"

// pain012OrgnlMsgPrefixes maps each accepted OrgnlMsgNmId to the
// MsgId prefix the ledger's own exports of that message carry
// (pain009.go, pain010.go, pain011.go): the prefix is the proof the
// report answers a request this ledger actually sent.
var pain012OrgnlMsgPrefixes = map[string]string{
	"pain.009.001.02": "pain009-",
	"pain.010.001.02": "pain010-",
	"pain.011.001.03": "pain011-",
}

// pain.012 verdicts (the ledger's vocabulary for Accptd).
const (
	// Pain012VerdictAccepted is the verdict for Accptd true: the
	// bank took the mandate request.
	Pain012VerdictAccepted = "accepted"
	// Pain012VerdictRejected is the verdict for Accptd false: the
	// bank refused the mandate request.
	Pain012VerdictRejected = "rejected"
)

// pain012Document is the wire form: Document/MndtAccptncRpt. The xml
// tags name local elements only, so the bank's namespace declaration
// is accepted without matching it, and unknown elements are skipped —
// banks extend these reports, and an extension must not refuse the
// verdicts the ledger understands.
type pain012Document struct {
	XMLName xml.Name   `xml:"Document"`
	Report  pain012Rpt `xml:"MndtAccptncRpt"`
}

type pain012Rpt struct {
	GrpHdr  pain012GrpHdr   `xml:"GrpHdr"`
	Details []pain012Detail `xml:"UndrlygAccptncDtls"`
}

type pain012GrpHdr struct {
	MsgID   string `xml:"MsgId"`
	CreDtTm string `xml:"CreDtTm"`
}

type pain012Detail struct {
	OrgnlMsgInf *pain012OrgnlMsgInf `xml:"OrgnlMsgInf"`
	AccptncRslt *pain012AccptncRslt `xml:"AccptncRslt"`
	OrgnlMndt   pain012OrgnlMndt    `xml:"OrgnlMndt"`
}

type pain012OrgnlMsgInf struct {
	MsgID   string `xml:"MsgId"`
	MsgNmID string `xml:"MsgNmId"`
	CreDtTm string `xml:"CreDtTm"`
}

type pain012AccptncRslt struct {
	Accptd          *bool       `xml:"Accptd"`
	RjctRsn         *pain012Rsn `xml:"RjctRsn"`
	AddtlRjctRsnInf []string    `xml:"AddtlRjctRsnInf"`
}

type pain012Rsn struct {
	Cd    string `xml:"Cd"`
	Prtry string `xml:"Prtry"`
}

// pain012OrgnlMndt is the OriginalMandate2Choice: either the
// identification arm (OrgnlMndtId, the mandate ID on its own) or the
// full-mandate arm (OrgnlMndt/MndtId). The identification arm is the
// one the Nordea MIG lists; the full arm is accepted so a bank that
// echoes the whole mandate still resolves to the same mandate ID.
type pain012OrgnlMndt struct {
	OrgnlMndtID string       `xml:"OrgnlMndtId"`
	Mndt        *pain012Mndt `xml:"OrgnlMndt"`
}

type pain012Mndt struct {
	MndtID string `xml:"MndtId"`
}

// Pain012Verdict is one mandate verdict inside a pain.012 report: the
// mandate it answers, the original request's MsgId and message name,
// the binary verdict, and — for a rejection — the bank's reason code
// and free text, verbatim.
type Pain012Verdict struct {
	MandateID    string `json:"mandate_id"`
	OrgnlMsgID   string `json:"orgnl_msg_id"`
	OrgnlMsgNmID string `json:"orgnl_msg_nm_id"`
	Verdict      string `json:"verdict"`
	ReasonCode   string `json:"reason_code,omitempty"`
	ReasonInfo   string `json:"reason_info,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

// Pain012Report is the parsed bank acceptance report: the report's own
// MsgId and the per-mandate verdicts in document order — one verdict
// per mandate.
type Pain012Report struct {
	MsgID    string           `json:"msg_id"`
	Verdicts []Pain012Verdict `json:"verdicts"`
}

// Pain012Receipt is the receipt for an applied pain.012 report: the
// report MsgId, the verdicts as applied, and whether this apply was a
// replay of an already-applied report (a replay returns the original
// receipt and records nothing new).
type Pain012Receipt struct {
	MsgID    string           `json:"msg_id"`
	Verdicts []Pain012Verdict `json:"verdicts"`
	Replayed bool             `json:"replayed"`
}

// MandateAcceptance is the latest bank acceptance report recorded for
// one mandate: the report MsgId, the original request it answers, the
// verdict, the bank's reason verbatim, and who recorded it and when.
type MandateAcceptance struct {
	ReportMsgID  string    `json:"report_msg_id"`
	OrgnlMsgID   string    `json:"orgnl_msg_id"`
	OrgnlMsgNmID string    `json:"orgnl_msg_nm_id"`
	Verdict      string    `json:"verdict"`
	ReasonCode   string    `json:"reason_code,omitempty"`
	ReasonInfo   string    `json:"reason_info,omitempty"`
	Reason       string    `json:"reason,omitempty"`
	Actor        string    `json:"actor"`
	At           time.Time `json:"at"`
}

// pain012VerdictReason renders a rejected verdict's reason the way the
// pain012_mandate_rejected audit event records it: "CODE: free text",
// or whichever half the bank supplied. The bank's wording rides
// verbatim — it is the bank's vocabulary, like the receipt Reason in
// pain001_status.go.
func pain012VerdictReason(reasonCode, reasonInfo string) string {
	switch {
	case reasonCode != "" && reasonInfo != "":
		return reasonCode + ": " + reasonInfo
	case reasonCode != "":
		return reasonCode
	default:
		return reasonInfo
	}
}

// ParsePain012 parses a bank-returned ISO 20022 pain.012.001.03
// mandate acceptance report. It validates the document shape —
// well-formed XML, the MndtAccptncRpt root, a non-empty GrpHdr MsgId,
// at least one UndrlygAccptncDtls entry, and per detail an
// OrgnlMsgInf whose MsgNmId is one of pain.009.001.02,
// pain.010.001.02, or pain.011.001.03 with a matching OrgnlMsgId
// prefix, an OrgnlMndt identification, and a present Accptd — and
// refuses anything else fail-loud: a half-readable bank file must not
// partially apply. Namespaces are ignored and unknown elements
// skipped, so bank extensions do not break parsing.
func ParsePain012(data []byte) (*Pain012Report, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("%w: empty document", ErrInvalidPain012)
	}
	var doc pain012Document
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidPain012, err)
	}
	msgID := strings.TrimSpace(doc.Report.GrpHdr.MsgID)
	if msgID == "" {
		return nil, fmt.Errorf("%w: missing GrpHdr MsgId", ErrInvalidPain012)
	}
	if len(doc.Report.Details) == 0 {
		return nil, fmt.Errorf("%w: no UndrlygAccptncDtls", ErrInvalidPain012)
	}
	report := &Pain012Report{MsgID: msgID}
	seen := make(map[string]bool, len(doc.Report.Details))
	for i, d := range doc.Report.Details {
		if d.OrgnlMsgInf == nil {
			return nil, fmt.Errorf("%w: UndrlygAccptncDtls[%d] missing OrgnlMsgInf", ErrInvalidPain012, i)
		}
		nmID := strings.TrimSpace(d.OrgnlMsgInf.MsgNmID)
		prefix, ok := pain012OrgnlMsgPrefixes[nmID]
		if !ok {
			return nil, fmt.Errorf("%w: got %q", ErrPain012UnexpectedMsgName, d.OrgnlMsgInf.MsgNmID)
		}
		orgnlMsgID := strings.TrimSpace(d.OrgnlMsgInf.MsgID)
		if orgnlMsgID == "" {
			return nil, fmt.Errorf("%w: UndrlygAccptncDtls[%d] missing OrgnlMsgId", ErrInvalidPain012, i)
		}
		if !strings.HasPrefix(orgnlMsgID, prefix) {
			return nil, fmt.Errorf("%w: OrgnlMsgId %q is not a %q message", ErrPain012MsgNameMismatch, orgnlMsgID, nmID)
		}
		mandateID := strings.TrimSpace(d.OrgnlMndt.OrgnlMndtID)
		if mandateID == "" && d.OrgnlMndt.Mndt != nil {
			mandateID = strings.TrimSpace(d.OrgnlMndt.Mndt.MndtID)
		}
		if mandateID == "" {
			return nil, fmt.Errorf("%w: UndrlygAccptncDtls[%d] missing OrgnlMndtId", ErrInvalidPain012, i)
		}
		if seen[mandateID] {
			return nil, fmt.Errorf("%w: mandate %q carries two verdicts in one report", ErrInvalidPain012, mandateID)
		}
		seen[mandateID] = true
		if d.AccptncRslt == nil || d.AccptncRslt.Accptd == nil {
			return nil, fmt.Errorf("%w: UndrlygAccptncDtls[%d] missing Accptd", ErrInvalidPain012Verdict, i)
		}
		v := Pain012Verdict{
			MandateID:    mandateID,
			OrgnlMsgID:   orgnlMsgID,
			OrgnlMsgNmID: nmID,
		}
		if *d.AccptncRslt.Accptd {
			v.Verdict = Pain012VerdictAccepted
		} else {
			v.Verdict = Pain012VerdictRejected
			if d.AccptncRslt.RjctRsn != nil {
				v.ReasonCode = strings.TrimSpace(d.AccptncRslt.RjctRsn.Cd)
				if v.ReasonCode == "" {
					v.ReasonCode = strings.TrimSpace(d.AccptncRslt.RjctRsn.Prtry)
				}
			}
			var infos []string
			for _, info := range d.AccptncRslt.AddtlRjctRsnInf {
				if trimmed := strings.TrimSpace(info); trimmed != "" {
					infos = append(infos, trimmed)
				}
			}
			v.ReasonInfo = strings.Join(infos, "; ")
			v.Reason = pain012VerdictReason(v.ReasonCode, v.ReasonInfo)
		}
		report.Verdicts = append(report.Verdicts, v)
	}
	return report, nil
}

// MandateAcceptance returns the latest bank acceptance report recorded
// for a mandate (LG-405), if any: the most recent pain.012 verdict
// applied to it. The acceptance is a record of the bank's answer, not
// a mandate state — a rejection leaves the mandate row untouched.
func (l *Ledger) MandateAcceptance(mandateID string) (MandateAcceptance, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	a, ok := l.mandateAcceptances[mandateID]
	return a, ok
}

// ApplyPain012Report applies a parsed pain.012 report to the mandates
// it answers, recording one verdict per mandate: the acceptance goes
// into the mandateAcceptances registry and one hash-chained audit
// event (pain012_mandate_accepted / pain012_mandate_rejected) is
// emitted per verdict, with the bank's reason code verbatim.
//
// The batch is validated before anything is applied — an unknown
// mandate or an illegal verdict refuses the whole report, never a
// single mandate — and a re-applied report MsgId is a no-op returning
// the original receipt. The mandate row is never moved: a rejection
// does not cancel or suspend anything, and the operator still decides
// through CancelMandate.
func (l *Ledger) ApplyPain012Report(report *Pain012Report, actor string) (Pain012Receipt, error) {
	if report == nil {
		return Pain012Receipt{}, fmt.Errorf("%w: nil report", ErrInvalidPain012)
	}
	if strings.TrimSpace(report.MsgID) == "" {
		return Pain012Receipt{}, fmt.Errorf("%w: missing report MsgId", ErrInvalidPain012)
	}
	if len(report.Verdicts) == 0 {
		return Pain012Receipt{}, fmt.Errorf("%w: no verdicts", ErrInvalidPain012)
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if prev, ok := l.pain012AppliedReports[report.MsgID]; ok {
		receipt := prev
		receipt.Replayed = true
		return receipt, nil
	}
	mandates := make([]Mandate, 0, len(report.Verdicts))
	for _, v := range report.Verdicts {
		if v.Verdict != Pain012VerdictAccepted && v.Verdict != Pain012VerdictRejected {
			return Pain012Receipt{}, fmt.Errorf("%w: %q for mandate %q", ErrInvalidPain012Verdict, v.Verdict, v.MandateID)
		}
		m, ok := l.mandates[v.MandateID]
		if !ok {
			return Pain012Receipt{}, fmt.Errorf("%w: %q", ErrMandateNotFound, v.MandateID)
		}
		mandates = append(mandates, m)
	}

	now := time.Now().UTC()
	receipt := Pain012Receipt{
		MsgID:    report.MsgID,
		Verdicts: append([]Pain012Verdict(nil), report.Verdicts...),
	}
	for i, v := range report.Verdicts {
		m := mandates[i]
		op := "pain012_mandate_accepted"
		if v.Verdict == Pain012VerdictRejected {
			op = "pain012_mandate_rejected"
		}
		l.mandateAcceptances[v.MandateID] = MandateAcceptance{
			ReportMsgID:  report.MsgID,
			OrgnlMsgID:   v.OrgnlMsgID,
			OrgnlMsgNmID: v.OrgnlMsgNmID,
			Verdict:      v.Verdict,
			ReasonCode:   v.ReasonCode,
			ReasonInfo:   v.ReasonInfo,
			Reason:       v.Reason,
			Actor:        actor,
			At:           now,
		}
		l.emitAudit(AuditEvent{
			Op:            op,
			Actor:         actor,
			TraceID:       report.MsgID,
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Accounts:      []AccountID{m.Payer, m.Payee},
			Details: map[string]any{
				"msg_id":          report.MsgID,
				"mandate_id":      v.MandateID,
				"orgnl_msg_nm_id": v.OrgnlMsgNmID,
				"verdict":         v.Verdict,
				"reason_code":     v.ReasonCode,
			},
		})
	}
	l.pain012AppliedReports[report.MsgID] = receipt
	return receipt, nil
}
