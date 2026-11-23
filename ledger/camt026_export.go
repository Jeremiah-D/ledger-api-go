package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ISO 20022 camt.026.001.07 Unable to Apply outbound export
// (LG-461): the ledger-side "I cannot execute / reconcile this
// instruction" report — the outbound half of the investigation
// family, the mirror of the inbound report the ledger already
// applies (see camt026_inbound.go, LG-417). The inbound half alone
// only proves the ledger can receive an unable-to-apply report;
// this is the document the ledger itself sends when it receives an
// instruction it cannot execute: missing or incorrect information,
// a suspected duplicate. The counterparty answers with a camt.029
// or re-initiates — the "ask—answer" loop closes across the two
// modules, the camt.026 analogue of the MT195/MT196 loop (LG-460 /
// LG-457).
//
// Field set, verified against the inbound side's already-checked
// structure (see camt026_inbound.go, LG-417):
//
//   - Assgnmt (CaseAssignment5): Id [1..1] (this export's own
//     MsgId, derived deterministically — see
//     camt026ExportMsgID), Assgnr / Assgne (names: the reporting
//     side and the side reported to), CreDtTm [1..1] (the export
//     instant).
//   - Case (Case5, [0..1]): Id, when the operator names an
//     investigation case.
//   - Undrlyg (UnderlyingTransaction5Choice, [1..1]): exactly one
//     arm, chosen by what the reference resolved to — a payout or
//     an RfP renders Initn (the original payment initiation), a
//     journaled transfer renders IntrBk (the original interbank
//     transaction). The statement-entry arm is never rendered: no
//     ledger instruction kind resolves to a statement entry.
//   - Justfn (UnableToApplyJustification3Choice, [1..1]): exactly
//     one arm — AnyInf, MssngOrIncrrctInf (one MssngInf entry
//     carrying the reason code and the additional text), or
//     PssblDplctInstr. An inbound report that used IncrrctInf
//     parses to the same ReasonCode / ReasonInfo the export
//     renders as MssngInf, so the round trip is exact at the level
//     the ledger reads.
//
// Identity wiring — scope honesty, the ledger invents nothing:
//   - The reference is the lookup key: a UETR or an EndToEndId,
//     resolved in that order through the inbound association
//     rules (resolveCAMT026Locked) — a gpi track's UETR (payout
//     or linked transfer), then the FedNow EndToEndId association
//     (payout by ID or idempotency key, transfer by idempotency
//     key or entry ID), then an RfP by ID. An unknown reference is
//     refused fail-closed (ErrCAMT026UnknownReference).
//   - The rendered references come from the resolved instruction,
//     never from the caller: a payout renders its gpi UETR and
//     its EndToEndId (the idempotency key, falling back to the
//     payout ID — the pacs.008 convention); a transfer renders
//     its entry ID and the gpi UETR of the track that names it,
//     when one exists; an RfP renders its ID and no UETR, because
//     an RfP carries none.
//   - The amount is the instruction's own amount and currency,
//     rendered with camt053FormatAmount.
//   - OrgnlGrpInf is omitted: the ledger never recorded the
//     original instruction's message ID or message name, so it
//     invents none.
//   - Assgnr / Assgne are envelope, not terms (the MT195 receiver
//     LT precedent): the operator may name them, or they default
//     from ledger state — Assgnr from the instruction account's
//     registered IBAN BIC when one exists, else the account ID
//     (the camt.029 convention); Assgne from the payout's
//     Destination, the transfer's credit account, or the RfP's
//     debtor account. Envelope differences never create a new
//     export: a replay returns the first rendering.
//   - MsgId is a deterministic derivation — "CAMT026-" plus the
//     first 10 lowercase hex characters of the SHA-256 of the
//     resolved kind, the target ID, the justification, the reason
//     code and info, and the case ID, joined with "\n" — so the
//     same instruction reported for the same reason always derives
//     the same MsgId, and a different reason derives a different
//     one. CreDtTm is the export instant, so a re-render after a
//     restart differs only in that timestamp; within one ledger
//     lifetime a replay returns the stored bytes, byte-identical.
//
// The export renders only: no journal entry, no payout status
// change, no gpi advance, no version bump — the audit event is the
// durable record, not a state change. No status gate is applied:
// a report about an already-terminal instruction is still
// renderable, along the inbound precedent (which records terminal
// reports) and the MT195 precedent (a query asks about a payment,
// it does not move it).
//
// Idempotency is the MsgId with its terms (the pain.014
// convention, LG-416, along the inbound camt026Reports): the
// export's own reference names one export. Re-exporting the same
// instruction for the same reason returns the stored rendered
// message with Duplicate set, rendering nothing twice and emitting
// no second audit event. A MsgId reused for different terms is
// refused with ErrCAMT026MsgIDConflict — never silently
// overwritten. The registry check runs again under the write lock
// after the message is rendered, so two concurrent exports of the
// same MsgId still record once.

// camt.026 outbound export errors.
var (
	// ErrCAMT026Export is returned when the export cannot be
	// rendered for a reason outside the field errors: no
	// reference, a missing or invalid justification, a
	// missing-or-incorrect justification with no reason code, or
	// a reason code/info on an arm that carries none. The HTTP
	// layer maps it to 400.
	ErrCAMT026Export = errors.New("ledger: invalid camt.026 unable-to-apply export")
)

// CAMT026ExportOptions names the reason of one camt.026 unable-to-
// apply export: the justification arm and its coded reason. The
// instruction reference is the first argument of ExportCAMT026,
// not an option: it is the lookup key, and keeping it out of the
// options makes the anchor explicit at the call site. The MsgId
// is not an option either: it is derived from the resolved
// instruction and the reason (see camt026ExportMsgID).
type CAMT026ExportOptions struct {
	// Justification names the Justfn arm: any-information,
	// missing-or-incorrect, or possible-duplicate. Required.
	Justification CAMT026Justification
	// ReasonCode is the justification's coded reason, verbatim:
	// the MssngInf Cd. Required for missing-or-incorrect, refused
	// on the other two arms (they carry no code — refused, never
	// silently dropped).
	ReasonCode string
	// ReasonInfo is the additional free text (AddtlMssngInf).
	// Same arm rule as ReasonCode.
	ReasonInfo string
	// CaseID is Case/Id, when the operator names an investigation
	// case.
	CaseID string
	// Assignor and Assignee name Assgnmt/Assgnr and Assgnmt/
	// Assgne. Empty means "derive from ledger state" (see the
	// package doc): envelope, not terms.
	Assignor string
	Assignee string
}

// CAMT026ExportResult is the outcome of ExportCAMT026: the
// rendered report message and the facts it was rendered from.
type CAMT026ExportResult struct {
	// MsgID is the export's Assgnmt/Id, derived deterministically.
	MsgID string `json:"msg_id"`
	// Kind is what the reference resolved to: payout, transfer,
	// or rfp. TargetID is that instruction's ID.
	Kind     CAMT026AssociationKind `json:"kind"`
	TargetID string                 `json:"target_id"`
	// UETR and EndToEndID are the rendered underlying reference.
	UETR       string `json:"uetr,omitempty"`
	EndToEndID string `json:"end_to_end_id,omitempty"`
	// Justification names the Justfn arm; ReasonCode and
	// ReasonInfo are the reason, verbatim.
	Justification CAMT026Justification `json:"justification"`
	ReasonCode    string               `json:"reason_code,omitempty"`
	ReasonInfo    string               `json:"reason_info,omitempty"`
	// CaseID is Case/Id, when stated.
	CaseID string `json:"case_id,omitempty"`
	// Message is the rendered camt.026.001.07 XML document.
	Message string `json:"message"`
	// Duplicate reports whether the export was already rendered
	// under the same MsgId with the same terms: a no-op success,
	// not an error.
	Duplicate bool `json:"duplicate,omitempty"`
}

// camt026ExportRecord is the registry record for one rendered
// camt.026 report, keyed by MsgId in camt026Exports: what it
// reported about, the terms it was rendered with, and the result,
// so a replay returns the same message without rendering twice.
type camt026ExportRecord struct {
	Kind     CAMT026AssociationKind
	TargetID string
	Options  CAMT026ExportOptions
	Result   CAMT026ExportResult
}

// camt026ExportTermsEqual reports whether a stored export record
// carries exactly the terms an export request would render: the
// same instruction (kind and target) and the same reason
// (justification, reason code and info, case). The assignor and
// assignee are envelope, not terms, along the MT195 precedent — a
// replay under different names still returns the first rendering,
// because the MsgId names one export.
func camt026ExportTermsEqual(rec camt026ExportRecord, kind CAMT026AssociationKind, targetID string, opts CAMT026ExportOptions) bool {
	return rec.Kind == kind &&
		rec.TargetID == targetID &&
		rec.Options.Justification == opts.Justification &&
		rec.Options.ReasonCode == opts.ReasonCode &&
		rec.Options.ReasonInfo == opts.ReasonInfo &&
		rec.Options.CaseID == opts.CaseID
}

// camt026ExportMsgID derives the deterministic Assgnmt/Id:
// "CAMT026-" plus the first 10 lowercase hex characters of the
// SHA-256 of the resolved kind, the target ID, the justification,
// the reason code and info, and the case ID, joined with "\n" —
// 18 characters. The separator keeps "po-1" + "MS01" distinct
// from splits that would otherwise hash the same; the same
// instruction reported for the same reason always derives the
// same MsgId, and a different reason derives a different one.
func camt026ExportMsgID(kind CAMT026AssociationKind, targetID string, opts CAMT026ExportOptions) string {
	sum := sha256.Sum256([]byte(string(kind) + "\n" + targetID + "\n" + string(opts.Justification) + "\n" + opts.ReasonCode + "\n" + opts.ReasonInfo + "\n" + opts.CaseID))
	return "CAMT026-" + hex.EncodeToString(sum[:])[:10]
}

// validateCAMT026ExportOptions fail-closes on options
// ExportCAMT026 would refuse, so the render gate and the export
// gate share one check, never a copy. It returns the normalized
// options (whitespace-trimmed strings).
func validateCAMT026ExportOptions(opts CAMT026ExportOptions) (CAMT026ExportOptions, error) {
	opts.Justification = CAMT026Justification(strings.TrimSpace(string(opts.Justification)))
	opts.ReasonCode = strings.TrimSpace(opts.ReasonCode)
	opts.ReasonInfo = strings.TrimSpace(opts.ReasonInfo)
	opts.CaseID = strings.TrimSpace(opts.CaseID)
	opts.Assignor = strings.TrimSpace(opts.Assignor)
	opts.Assignee = strings.TrimSpace(opts.Assignee)
	switch opts.Justification {
	case CAMT026JustificationAnyInformation, CAMT026JustificationMissingOrIncorrect, CAMT026JustificationPossibleDuplicate:
	default:
		return opts, fmt.Errorf("%w: justification is required (any-information, missing-or-incorrect, or possible-duplicate)", ErrCAMT026Export)
	}
	if opts.Justification == CAMT026JustificationMissingOrIncorrect {
		if opts.ReasonCode == "" {
			return opts, fmt.Errorf("%w: a missing-or-incorrect justification requires a reason code", ErrCAMT026Export)
		}
	} else if opts.ReasonCode != "" || opts.ReasonInfo != "" {
		return opts, fmt.Errorf("%w: a %s justification carries no reason code or info", ErrCAMT026Export, opts.Justification)
	}
	return opts, nil
}

// camt026ExportSnapshot is the resolved instruction behind one
// export, read as value copies under the read lock: what it is,
// what to render for it, and the envelope defaults.
type camt026ExportSnapshot struct {
	Kind        CAMT026AssociationKind
	TargetID    string
	UETR        string
	EndToEndID  string
	AmountCents int64
	Currency    string
	Accounts    []AccountID
	Assignor    string
	Assignee    string
}

// camt026TransferUETRLocked finds the gpi UETR of the track that
// names transferID, when one exists. Callers must hold l.mu. The
// candidates are sorted and the first taken, so the choice is
// deterministic even if more than one track ever names the same
// transfer.
func (l *Ledger) camt026TransferUETRLocked(transferID string) string {
	var candidates []string
	for uetr, t := range l.gpiTracks {
		if t != nil && t.TransferID == transferID {
			candidates = append(candidates, uetr)
		}
	}
	if len(candidates) == 0 {
		return ""
	}
	sort.Strings(candidates)
	return candidates[0]
}

// camt026ExportAssignorLocked names the reporting side for the
// instruction account: the account's registered IBAN BIC when one
// exists, else the internal account ID (the camt.029 convention).
// Callers must hold l.mu.
func (l *Ledger) camt026ExportAssignorLocked(account AccountID) string {
	if reg, ok := l.accountIBANs[account]; ok && reg.BIC != "" {
		return reg.BIC
	}
	return string(account)
}

// resolveCAMT026ExportLocked resolves ref (a UETR or an
// EndToEndId) against the ledger's tracked instructions and reads
// the snapshot the render needs. Callers must hold l.mu (either
// lock suffices). The UETR resolution runs first, then the
// EndToEndId one — the inbound check order.
func (l *Ledger) resolveCAMT026ExportLocked(ref string) (camt026ExportSnapshot, bool) {
	none := camt026ExportSnapshot{}
	var assoc camt026Association
	resolved := false
	if a, ok := l.resolveCAMT026Locked(ref, true); ok {
		assoc, resolved = a, true
	} else if a, ok := l.resolveCAMT026Locked(ref, false); ok {
		assoc, resolved = a, true
	}
	if !resolved {
		return none, false
	}
	snap := camt026ExportSnapshot{
		Kind:     assoc.Kind,
		TargetID: assoc.ID,
		Accounts: assoc.Accounts,
	}
	switch assoc.Kind {
	case CAMT026AssociatedPayout:
		p, ok := l.payouts[assoc.ID]
		if !ok {
			return none, false
		}
		snap.UETR = p.UETR
		snap.EndToEndID = p.IdempotencyKey
		if snap.EndToEndID == "" {
			snap.EndToEndID = p.ID
		}
		snap.AmountCents = p.AmountCents
		snap.Currency = p.Currency
		snap.Assignor = l.camt026ExportAssignorLocked(p.PayeeAccount)
		snap.Assignee = p.Destination
	case CAMT026AssociatedTransfer:
		e, ok := l.entries[assoc.ID]
		if !ok {
			return none, false
		}
		snap.UETR = l.camt026TransferUETRLocked(e.ID)
		snap.EndToEndID = e.ID
		snap.AmountCents = e.AmountCents
		snap.Currency = e.Currency
		snap.Assignor = l.camt026ExportAssignorLocked(e.DebitAccount)
		snap.Assignee = string(e.CreditAccount)
	case CAMT026AssociatedRfP:
		r, ok := l.rfps[assoc.ID]
		if !ok {
			return none, false
		}
		snap.EndToEndID = r.ID
		snap.AmountCents = r.AmountCents
		snap.Currency = r.Currency
		snap.Assignor = l.camt026ExportAssignorLocked(r.Creditor)
		snap.Assignee = string(r.Debtor)
	default:
		return none, false
	}
	return snap, true
}

// camt.026 export wire structs: Document/UblToApply. The namespace
// rides the Document element, the camt053_export.go convention.
// The fields are the ones ParseCAMT026 reads, which is what makes
// the round trip exact.
type camt026ExportDocument struct {
	XMLName xml.Name         `xml:"urn:iso:std:iso:20022:tech:xsd:camt.026.001.07 Document"`
	Report  camt026ExportUbl `xml:"UblToApply"`
}

type camt026ExportUbl struct {
	Assgnmt camt026ExportAssgnmt `xml:"Assgnmt"`
	Case    *camt026ExportCase   `xml:"Case,omitempty"`
	Undrlyg camt026ExportUndrlyg `xml:"Undrlyg"`
	Justfn  camt026ExportJustfn  `xml:"Justfn"`
}

type camt026ExportAssgnmt struct {
	ID      string       `xml:"Id"`
	Assgnr  camt026Party `xml:"Assgnr"`
	Assgne  camt026Party `xml:"Assgne"`
	CreDtTm string       `xml:"CreDtTm"`
}

type camt026ExportCase struct {
	ID string `xml:"Id"`
}

type camt026ExportUndrlyg struct {
	Initn  *camt026ExportInitn  `xml:"Initn,omitempty"`
	IntrBk *camt026ExportIntrBk `xml:"IntrBk,omitempty"`
}

type camt026ExportInitn struct {
	OrgnlEndToEndID string            `xml:"OrgnlEndToEndId,omitempty"`
	OrgnlUETR       string            `xml:"OrgnlUETR,omitempty"`
	OrgnlInstdAmt   *camt026ExportAmt `xml:"OrgnlInstdAmt,omitempty"`
}

type camt026ExportIntrBk struct {
	OrgnlEndToEndID     string            `xml:"OrgnlEndToEndId,omitempty"`
	OrgnlUETR           string            `xml:"OrgnlUETR,omitempty"`
	OrgnlIntrBkSttlmAmt *camt026ExportAmt `xml:"OrgnlIntrBkSttlmAmt,omitempty"`
}

type camt026ExportAmt struct {
	Ccy   string `xml:"Ccy,attr"`
	Value string `xml:",chardata"`
}

type camt026ExportJustfn struct {
	AnyInf            *string                      `xml:"AnyInf,omitempty"`
	MssngOrIncrrctInf *camt026ExportMssngOrIncrrct `xml:"MssngOrIncrrctInf,omitempty"`
	PssblDplctInstr   *string                      `xml:"PssblDplctInstr,omitempty"`
}

type camt026ExportMssngOrIncrrct struct {
	MssngInf []camt026ExportInfo `xml:"MssngInf"`
}

type camt026ExportInfo struct {
	Cd            string `xml:"Cd"`
	AddtlMssngInf string `xml:"AddtlMssngInf,omitempty"`
}

// camt026RenderExport renders the resolved instruction and the
// reason as a camt.026.001.07 UblToApply document. The caller
// supplies the export instant (CreDtTm) so the render is a pure
// function of its inputs.
func camt026RenderExport(msgID string, snap camt026ExportSnapshot, opts CAMT026ExportOptions, createdAt time.Time) ([]byte, error) {
	amt := &camt026ExportAmt{Ccy: snap.Currency, Value: camt053FormatAmount(snap.AmountCents)}
	var undrlyg camt026ExportUndrlyg
	switch snap.Kind {
	case CAMT026AssociatedPayout, CAMT026AssociatedRfP:
		undrlyg.Initn = &camt026ExportInitn{
			OrgnlEndToEndID: snap.EndToEndID,
			OrgnlUETR:       snap.UETR,
			OrgnlInstdAmt:   amt,
		}
	case CAMT026AssociatedTransfer:
		undrlyg.IntrBk = &camt026ExportIntrBk{
			OrgnlEndToEndID:     snap.EndToEndID,
			OrgnlUETR:           snap.UETR,
			OrgnlIntrBkSttlmAmt: amt,
		}
	default:
		return nil, fmt.Errorf("%w: cannot render underlying kind %q", ErrCAMT026Export, snap.Kind)
	}
	var justfn camt026ExportJustfn
	trueStr := "true"
	switch opts.Justification {
	case CAMT026JustificationAnyInformation:
		justfn.AnyInf = &trueStr
	case CAMT026JustificationMissingOrIncorrect:
		justfn.MssngOrIncrrctInf = &camt026ExportMssngOrIncrrct{
			MssngInf: []camt026ExportInfo{{Cd: opts.ReasonCode, AddtlMssngInf: opts.ReasonInfo}},
		}
	case CAMT026JustificationPossibleDuplicate:
		justfn.PssblDplctInstr = &trueStr
	default:
		return nil, fmt.Errorf("%w: justification %q", ErrCAMT026Export, opts.Justification)
	}
	assignor := opts.Assignor
	if assignor == "" {
		assignor = snap.Assignor
	}
	assignee := opts.Assignee
	if assignee == "" {
		assignee = snap.Assignee
	}
	doc := camt026ExportDocument{
		Report: camt026ExportUbl{
			Assgnmt: camt026ExportAssgnmt{
				ID:      msgID,
				Assgnr:  camt026Party{Nm: assignor},
				Assgne:  camt026Party{Nm: assignee},
				CreDtTm: createdAt.UTC().Format(time.RFC3339),
			},
			Undrlyg: undrlyg,
			Justfn:  justfn,
		},
	}
	if opts.CaseID != "" {
		doc.Report.Case = &camt026ExportCase{ID: opts.CaseID}
	}
	body, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidCAMT026, err)
	}
	return append([]byte(xml.Header), body...), nil
}

// ExportCAMT026 renders the ledger's own ISO 20022 camt.026.001.07
// Unable to Apply report about the instruction ref names — a UETR
// or an EndToEndId, resolved in that order through the inbound
// association rules. The report says the ledger cannot execute or
// reconcile the instruction, for the reason opts names.
//
// The reference must resolve (an unknown reference is
// ErrCAMT026UnknownReference, fail-closed). Nothing moves: no
// journal entry, no payout status change, no gpi advance, no
// version bump — rendering a report is read-only, and the audit
// event is the durable record, not a state change. No status gate
// is applied: any known instruction may be reported about, along
// the inbound precedent (which records terminal reports) and the
// MT195 precedent.
//
// Idempotency is the MsgId with its terms (the pain.014
// convention): a replay with the same instruction and reason
// returns the stored rendered message with Duplicate set,
// rendering nothing twice and emitting no second audit event; a
// MsgId reused for different terms is refused with
// ErrCAMT026MsgIDConflict.
//
// Locking: the validation runs on the parsed values alone; the
// replay read, the resolution, and the snapshot read run under
// the read lock; the MsgId record and the export marker event run
// under a short write lock of their own, re-checking the registry
// first so a concurrent double-export still records once.
func (l *Ledger) ExportCAMT026(ref string, opts CAMT026ExportOptions, actor string) (CAMT026ExportResult, error) {
	if actor == "" {
		actor = "ExportCAMT026"
	}
	opts, err := validateCAMT026ExportOptions(opts)
	if err != nil {
		return CAMT026ExportResult{}, err
	}
	if strings.TrimSpace(ref) == "" {
		return CAMT026ExportResult{}, fmt.Errorf("%w: reference is required (a UETR or an EndToEndId)", ErrCAMT026Export)
	}
	ref = strings.TrimSpace(ref)

	// Resolve the reference and derive the MsgId, under the read
	// lock: the MsgId is the replay key, so the replay check runs
	// here too, before anything renders.
	l.mu.RLock()
	snap, ok := l.resolveCAMT026ExportLocked(ref)
	if !ok {
		l.mu.RUnlock()
		return CAMT026ExportResult{}, fmt.Errorf("%w: %q", ErrCAMT026UnknownReference, ref)
	}
	msgID := camt026ExportMsgID(snap.Kind, snap.TargetID, opts)
	if rec, ok := l.camt026Exports[msgID]; ok {
		l.mu.RUnlock()
		if !camt026ExportTermsEqual(rec, snap.Kind, snap.TargetID, opts) {
			return CAMT026ExportResult{}, fmt.Errorf("%w: MsgId %q", ErrCAMT026MsgIDConflict, msgID)
		}
		dup := rec.Result
		dup.Duplicate = true
		return dup, nil
	}
	l.mu.RUnlock()

	// The render is a pure function of the snapshot, the reason,
	// and the export instant.
	now := time.Now().UTC()
	body, err := camt026RenderExport(msgID, snap, opts, now)
	if err != nil {
		return CAMT026ExportResult{}, err
	}
	res := CAMT026ExportResult{
		MsgID:         msgID,
		Kind:          snap.Kind,
		TargetID:      snap.TargetID,
		UETR:          snap.UETR,
		EndToEndID:    snap.EndToEndID,
		Justification: opts.Justification,
		ReasonCode:    opts.ReasonCode,
		ReasonInfo:    opts.ReasonInfo,
		CaseID:        opts.CaseID,
		Message:       string(body),
	}

	// The MsgId record and the export marker event, under one short
	// write lock: the message is already rendered when the marker
	// lands. A concurrent double-export that landed first returns
	// the stored outcome instead of a second marker.
	l.mu.Lock()
	if rec, ok := l.camt026Exports[msgID]; ok {
		l.mu.Unlock()
		if !camt026ExportTermsEqual(rec, snap.Kind, snap.TargetID, opts) {
			return CAMT026ExportResult{}, fmt.Errorf("%w: MsgId %q", ErrCAMT026MsgIDConflict, msgID)
		}
		dup := rec.Result
		dup.Duplicate = true
		return dup, nil
	}
	if l.camt026Exports == nil {
		l.camt026Exports = make(map[string]camt026ExportRecord)
	}
	l.camt026Exports[msgID] = camt026ExportRecord{Kind: snap.Kind, TargetID: snap.TargetID, Options: opts, Result: res}
	details := map[string]any{
		"msg_id":        msgID,
		"uetr":          snap.UETR,
		"end_to_end_id": snap.EndToEndID,
		"kind":          string(snap.Kind),
		"target_id":     snap.TargetID,
		"justification": string(opts.Justification),
		"reason_code":   opts.ReasonCode,
		"reason_info":   opts.ReasonInfo,
	}
	if opts.CaseID != "" {
		details["case_id"] = opts.CaseID
	}
	l.emitAudit(AuditEvent{
		Op:            "camt026_unable_to_apply_exported",
		Actor:         actor,
		TraceID:       msgID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      snap.Accounts,
		Details:       details,
	})
	l.mu.Unlock()
	return res, nil
}

// GetCAMT026Export returns the recorded export for msgID, or false
// when no report was ever rendered under it. The returned record
// is a copy — callers cannot mutate ledger state through it.
func (l *Ledger) GetCAMT026Export(msgID string) (CAMT026ExportResult, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	rec, ok := l.camt026Exports[msgID]
	if !ok {
		return CAMT026ExportResult{}, false
	}
	return rec.Result, true
}
