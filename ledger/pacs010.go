package ledger

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ISO 20022 pacs.010.001.02 FinancialInstitutionDirectDebit export
// (LG-190): the interbank half of the direct-debit mandate lifecycle.
//
// pain.008 (see pain008.go, LG-166) renders the booked mandate
// collections as the customer direct-debit initiation the creditor
// sends to its bank; ExportPacs010 renders the same booked
// collections as the pacs.010.001.02 FinancialInstitutionDirectDebit
// the creditor's bank sends to the debtor's bank — the bank-to-bank
// pull that actually moves the money between the two institutions. It
// is the pacs.009 credit transfer's (see pacs009.go, LG-172) mirror
// image: pacs.009 pushes between FIs (DbtrAgt -> CdtrAgt with the
// debtor instructing), pacs.010 pulls (the creditor's agent instructs
// the debtor's agent).
//
// Message shape, honestly mapped onto what the ledger knows:
//
//   - GrpHdr carries MsgId (deterministic in mandate, window, and
//     content — the pain001MsgID convention), CreDtTm, NbOfTxs,
//     CtrlSum, and SttlmInf/SttlmMtd from the pacs.009 settlement
//     vocabulary (INDA/INGA/COVE/CLRG, default INDA).
//   - One CdtInstr per booked collection: PmtId (InstrId = the journal
//     entry ID, EndToEndId = the journal entry ID — stable across
//     exports, so the debtor's bank can dedupe and the camt.054
//     reconciliation can join on it), InstdAmt, ReqdColltnDt (the
//     collection's booking date), Cdtr/CdtrAcct (the mandate's payee —
//     the instructing creditor institution), Dbtr/DbtrAcct (the
//     mandate's payer — the pulled debtor institution), and
//     DrctDbtTx/MndtRltdInf with MndtId and DtOfSgntr (the mandate's
//     ValidFrom date, the day the debtor authorized the pull).
//   - Agent identification follows the pacs.009 convention: there is no
//     BIC registry in the ledger, so agents ride proprietary
//     identification (FinInstnId/Othr/Id = the account ID) unless the
//     account has a registered BIC, which rides FinInstnId/BIC — an
//     honest, documented mapping, not a fabricated BIC.
//
// Reconciliation with the camt.054 debit notifications: the bank
// answers each collection with a camt.054 BankToCustomerDebitCredit
// notification that echoes the EndToEndId; ReconcilePacs010 joins the
// mandate's booked collections against the imported notifications in
// the camt054Matches registry and reports per collection whether the
// bank confirmed it (notified) or it is still outstanding (pending).
//
// pain.002 status reports answer pain.001 messages only: a report
// naming a pacs.010 MsgId (the "pacs010-" namespace) is refused by
// ErrPain002ForeignMsgID — the same rule that has always kept
// pain.008 MsgIds out of the pain.002 path, now stated explicitly.
//
// Fail-closed, like the pain.008 export: the mandate must exist and be
// effectively active (unknown/suspended/cancelled/expired mandates
// refuse the export), and both the debtor and creditor need registered
// IBANs — the ledger stores no BIC/IBAN on the mandate itself, and
// there is no honest default.
//
// The export is read-only: it takes the read lock, bumps no version,
// appends no chain link, and consumes no idempotency key. Like
// Reconcile, it still emits one structured audit event
// ("pacs010_exported", details msg_id/tx_count/ctrl_sum_cents/
// mandate_id) so the compliance trail records every generated
// instruction.

// pacs.010 export errors.
var (
	// ErrInvalidPacs010Range is returned by ExportPacs010 and
	// ReconcilePacs010 when the requested time range is empty: since is
	// after until. Mirrors ErrInvalidPain008Range.
	ErrInvalidPacs010Range = errors.New("ledger: pacs.010 range is invalid: since is after until")
	// ErrPacs010AmountOverflow is returned by ExportPacs010 when the
	// control sum of the exported collections overflows int64: no real
	// control total could be computed, so the export is refused.
	ErrPacs010AmountOverflow = errors.New("ledger: pacs.010 control sum overflows int64")
	// ErrPacs010InvalidSettlementMethod is returned by ExportPacs010
	// for a settlement method outside the admitted SttlmMtd vocabulary
	// (INDA/INGA/COVE/CLRG). The machine only emits real interbank
	// codes; it does not invent them.
	ErrPacs010InvalidSettlementMethod = errors.New("ledger: pacs.010 settlement method must be one of INDA, INGA, COVE, CLRG")
)

// Pacs010ExportOptions tunes ExportPacs010.
//
// SettlementMethod is one of INDA/INGA/COVE/CLRG (the pacs.009
// vocabulary); empty means INDA, the ordinary correspondent
// debit/credit. Since and Until bound the exported collections
// (half-open [Since, Until) on the debit's booking timestamp, the OFX
// export convention; zero means unbounded).
type Pacs010ExportOptions struct {
	SettlementMethod string
	Since            time.Time
	Until            time.Time
}

// pacs010Collection is one exported direct-debit collection: the
// booked mandate debit plus the resolved debtor/creditor IBANs.
type pacs010Collection struct {
	Entry        JournalEntry
	DebtorIBAN   AccountIBAN // the mandate payer (the pulled institution)
	CreditorIBAN AccountIBAN // the mandate payee (the instructing institution)
}

// pacs010MsgID derives the deterministic message ID from the mandate,
// the window, and the exported content — the pain001MsgID convention:
// the same export re-renders byte-identical, and any content change
// changes the ID. The "pacs010-" namespace keeps pacs.010 MsgIds out
// of the pain.001/pain.008 families (see ErrPain002ForeignMsgID).
func pacs010MsgID(mandateID string, opts Pacs010ExportOptions, entryIDs []string) string {
	var b strings.Builder
	b.WriteString("pacs010-")
	b.WriteString(pain001UnixOrZero(opts.Since))
	b.WriteString("-")
	b.WriteString(pain001UnixOrZero(opts.Until))
	b.WriteString("-")
	b.WriteString(mandateID)
	for _, id := range entryIDs {
		b.WriteString("-")
		b.WriteString(id)
	}
	return b.String()
}

// pacs010Agent renders one FI agent: the registered BIC when the
// account has one, else the pacs.009 proprietary identification
// (FinInstnId/Othr/Id = the account ID).
func pacs010Agent(tag string, account AccountID, reg AccountIBAN) string {
	var b strings.Builder
	b.WriteString("<" + tag + "><FinInstnId>")
	if strings.TrimSpace(reg.BIC) != "" {
		b.WriteString(fmt.Sprintf("<BIC>%s</BIC>", ofxEscape(reg.BIC)))
	} else {
		b.WriteString(fmt.Sprintf("<Othr><Id>%s</Id></Othr>", ofxEscape(string(account))))
	}
	b.WriteString("</FinInstnId></" + tag + ">")
	return b.String()
}

// pacs010CollectionsLocked resolves the mandate's booked collections
// in the window: every debit booked under the mandate whose booking
// timestamp falls in [Since, Until), in debit order. Callers must hold
// l.mu. The mandate must already be located and checked.
func pacs010CollectionsLocked(l *Ledger, m Mandate, since, until time.Time) ([]pacs010Collection, []string, error) {
	var collections []pacs010Collection
	var entryIDs []string
	for _, id := range l.mandateDebits[m.ID] {
		e, ok := l.entries[id]
		if !ok {
			// Append-only journal: a missing row is data
			// corruption, not a gap to paper over — but the
			// export must not invent a position either.
			// Refuse fail-loud: a half-readable history must not
			// render an instruction.
			return nil, nil, fmt.Errorf("ledger: pacs.010 export refused: mandate debit %q not in journal", id)
		}
		if !since.IsZero() && e.CreatedAt.Before(since) {
			continue
		}
		if !until.IsZero() && !e.CreatedAt.Before(until) {
			continue
		}
		debtorIBAN, ok := l.accountIBANs[m.Payer]
		if !ok {
			return nil, nil, fmt.Errorf("%w: debtor %q", ErrMissingAccountIBAN, m.Payer)
		}
		creditorIBAN, ok := l.accountIBANs[m.Payee]
		if !ok {
			return nil, nil, fmt.Errorf("%w: creditor %q", ErrMissingAccountIBAN, m.Payee)
		}
		collections = append(collections, pacs010Collection{
			Entry:        e,
			DebtorIBAN:   debtorIBAN,
			CreditorIBAN: creditorIBAN,
		})
		entryIDs = append(entryIDs, id)
	}
	return collections, entryIDs, nil
}

// ExportPacs010 renders an ISO 20022 pacs.010.001.02
// FinancialInstitutionDirectDebit for the booked collections of one
// mandate (see DebitWithMandate).
//
// Semantics, made explicit so the debtor's bank can trust them:
//   - One CdtInstr per booked debit in the window; EndToEndId is the
//     journal entry ID — stable across exports, so the bank can
//     dedupe on it and the camt.054 reconciliation can join on it.
//   - The creditor (instructing institution) is the mandate's payee,
//     the debtor (pulled institution) the mandate's payer; their
//     account identifiers are the registered IBANs, the agents ride
//     the registered BIC or the proprietary account-ID mapping.
//   - MndtId is the mandate ID; DtOfSgntr is the mandate's ValidFrom
//     date — the day the debtor authorized the pull.
//   - ReqdColltnDt is the collection's booking date: the day the
//     ledger authorized the pull.
//   - Amounts are the ledger's integer cents rendered as exact
//     decimals (two fraction digits, no floats anywhere).
//   - MsgId is deterministic in (mandate, window, content).
//   - Fail-closed: an unknown, suspended, cancelled, or expired
//     mandate refuses the export, as do missing debtor/creditor
//     IBANs and a settlement method outside the vocabulary.
//
// The export is read-only: it takes the read lock, bumps no version,
// appends no chain link, and consumes no idempotency key. Like
// Reconcile, it still emits one structured audit event
// ("pacs010_exported", details msg_id/tx_count/ctrl_sum_cents/
// mandate_id) so the compliance trail records every generated
// instruction.
func (l *Ledger) ExportPacs010(mandateID string, opts Pacs010ExportOptions) ([]byte, error) {
	sttlmMtd := strings.TrimSpace(opts.SettlementMethod)
	if sttlmMtd == "" {
		sttlmMtd = DefaultPacs009SettlementMethod
	}
	if !pacs009ValidSettlementMethod(sttlmMtd) {
		return nil, fmt.Errorf("%w: %q", ErrPacs010InvalidSettlementMethod, opts.SettlementMethod)
	}
	if !opts.Since.IsZero() && !opts.Until.IsZero() && opts.Since.After(opts.Until) {
		return nil, ErrInvalidPacs010Range
	}

	l.mu.RLock()
	m, ok := l.mandates[mandateID]
	if !ok {
		l.mu.RUnlock()
		return nil, fmt.Errorf("%w: %q", ErrMandateNotFound, mandateID)
	}
	now := time.Now()
	if eff := mandateEffectiveStatusLocked(m, now); eff != MandateStatusActive {
		l.mu.RUnlock()
		if eff == MandateStatusExpired {
			return nil, fmt.Errorf("%w: mandate %q", ErrMandateExpired, mandateID)
		}
		return nil, fmt.Errorf("%w: mandate %q status %q", ErrMandateNotActive, mandateID, eff)
	}
	collections, entryIDs, err := pacs010CollectionsLocked(l, m, opts.Since, opts.Until)
	if err != nil {
		l.mu.RUnlock()
		return nil, err
	}

	doc, msgID, txCount, ctrlSum, err := renderPacs010(m, sttlmMtd, opts, collections, entryIDs)
	if err != nil {
		l.mu.RUnlock()
		return nil, err
	}
	// Read-only, like Reconcile and the pain.008 export: the version
	// brackets the export but does not move.
	l.emitAudit(AuditEvent{
		Op:            "pacs010_exported",
		Actor:         "ExportPacs010",
		TraceID:       msgID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{m.Payer, m.Payee},
		Details: map[string]any{
			"msg_id":         msgID,
			"mandate_id":     mandateID,
			"tx_count":       txCount,
			"ctrl_sum_cents": ctrlSum,
			"currency":       m.Currency,
			"sttlm_mtd":      sttlmMtd,
		},
	})
	l.mu.RUnlock()
	return doc, nil
}

// renderPacs010 builds the pacs.010.001.02 document for the resolved
// collections — GrpHdr plus one CdtInstr per collection in debit
// order — and returns the document plus the GrpHdr facts for the
// audit event.
func renderPacs010(m Mandate, sttlmMtd string, opts Pacs010ExportOptions, collections []pacs010Collection, entryIDs []string) (doc []byte, msgID string, txCount int, ctrlSum int64, err error) {
	msgID = pacs010MsgID(m.ID, opts, entryIDs)
	// Control sum over every exported collection, overflow-checked:
	// a control total that wrapped would be worse than no export.
	var sum int64
	for _, c := range collections {
		var ok bool
		sum, ok = addCents(sum, c.Entry.AmountCents)
		if !ok {
			return nil, "", 0, 0, ErrPacs010AmountOverflow
		}
	}
	txCount = len(collections)
	ctrlSum = sum

	now := time.Now().UTC()
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pacs.010.001.02">` + "\n")
	b.WriteString("<FIDrctDbt>\n")
	b.WriteString("<GrpHdr>\n")
	b.WriteString(fmt.Sprintf("<MsgId>%s</MsgId>\n", ofxEscape(msgID)))
	b.WriteString(fmt.Sprintf("<CreDtTm>%s</CreDtTm>\n", now.Format(time.RFC3339)))
	b.WriteString(fmt.Sprintf("<NbOfTxs>%d</NbOfTxs>\n", txCount))
	b.WriteString(fmt.Sprintf("<CtrlSum>%s</CtrlSum>\n", pain001Amount(ctrlSum)))
	b.WriteString(fmt.Sprintf("<SttlmInf><SttlmMtd>%s</SttlmMtd></SttlmInf>\n", sttlmMtd))
	b.WriteString("</GrpHdr>\n")
	for _, c := range collections {
		b.WriteString("<CdtInstr>\n")
		b.WriteString(fmt.Sprintf("<PmtId><InstrId>%s</InstrId><EndToEndId>%s</EndToEndId></PmtId>\n",
			ofxEscape(c.Entry.ID), ofxEscape(c.Entry.ID)))
		b.WriteString(fmt.Sprintf("<InstdAmt Ccy=\"%s\">%s</InstdAmt>\n",
			ofxEscape(c.Entry.Currency), pain001Amount(c.Entry.AmountCents)))
		b.WriteString(fmt.Sprintf("<ReqdColltnDt>%s</ReqdColltnDt>\n", c.Entry.CreatedAt.UTC().Format("2006-01-02")))
		// The creditor is the mandate's payee (the instructing
		// institution); the debtor is the mandate's payer (the
		// pulled institution).
		b.WriteString(fmt.Sprintf("<Cdtr><Nm>%s</Nm></Cdtr>\n", ofxEscape(string(m.Payee))))
		b.WriteString(fmt.Sprintf("<CdtrAcct><Id><IBAN>%s</IBAN></Id></CdtrAcct>\n", ofxEscape(c.CreditorIBAN.IBAN)))
		b.WriteString(pacs010Agent("CdtrAgt", m.Payee, c.CreditorIBAN) + "\n")
		b.WriteString(fmt.Sprintf("<Dbtr><Nm>%s</Nm></Dbtr>\n", ofxEscape(string(m.Payer))))
		b.WriteString(fmt.Sprintf("<DbtrAcct><Id><IBAN>%s</IBAN></Id></DbtrAcct>\n", ofxEscape(c.DebtorIBAN.IBAN)))
		b.WriteString(pacs010Agent("DbtrAgt", m.Payer, c.DebtorIBAN) + "\n")
		b.WriteString("<DrctDbtTx>\n")
		b.WriteString("<MndtRltdInf>\n")
		b.WriteString(fmt.Sprintf("<MndtId>%s</MndtId>\n", ofxEscape(m.ID)))
		b.WriteString(fmt.Sprintf("<DtOfSgntr>%s</DtOfSgntr>\n", m.ValidFrom.UTC().Format("2006-01-02")))
		b.WriteString("</MndtRltdInf>\n")
		b.WriteString("</DrctDbtTx>\n")
		if strings.TrimSpace(c.Entry.Memo) != "" {
			b.WriteString(fmt.Sprintf("<RmtInf><Ustrd>%s</Ustrd></RmtInf>\n", ofxEscape(c.Entry.Memo)))
		}
		b.WriteString("</CdtInstr>\n")
	}
	b.WriteString("</FIDrctDbt>\n")
	b.WriteString("</Document>\n")
	return []byte(b.String()), msgID, txCount, ctrlSum, nil
}

// Pacs010 reconciliation statuses: the bank's camt.054 answer for one
// exported collection.
const (
	// Pacs010ReconNotified: a camt.054 debit/credit notification was
	// imported naming this collection's EndToEndId — the bank
	// confirmed the pull.
	Pacs010ReconNotified = "notified"
	// Pacs010ReconPending: no camt.054 notification names this
	// collection's EndToEndId yet — the pull is still outstanding.
	Pacs010ReconPending = "pending"
)

// Pacs010ReconOptions tunes ReconcilePacs010: the half-open
// [Since, Until) window on the collection's booking timestamp, the
// same convention as the export.
type Pacs010ReconOptions struct {
	Since time.Time
	Until time.Time
}

// Pacs010ReconItem is one exported collection with its camt.054
// notification state.
type Pacs010ReconItem struct {
	EntryID     string `json:"entry_id"`
	AmountCents int64  `json:"amount_cents"`
	Currency    string `json:"currency"`
	// Status is Pacs010ReconNotified or Pacs010ReconPending.
	Status string `json:"status"`
	// NtfctnID names the imported camt.054 notification
	// ("<Ntfctn Id>/<seq>") when one exists.
	NtfctnID string `json:"ntfctn_id,omitempty"`
	// MatchStatus is the notification's camt.054 triple-match outcome
	// (matched/mismatched/unmatched) when a notification exists: a
	// notified-but-unmatched collection tells the operator the bank
	// answered with something the ledger could not join to an
	// outstanding item.
	MatchStatus string `json:"match_status,omitempty"`
}

// Pacs010ReconReport is the full camt.054 reconciliation report for
// one mandate's collections.
type Pacs010ReconReport struct {
	MandateID string             `json:"mandate_id"`
	Items     []Pacs010ReconItem `json:"items"`
	Notified  int                `json:"notified"`
	Pending   int                `json:"pending"`
}

// ReconcilePacs010 joins one mandate's booked collections in the
// window against the imported camt.054 debit/credit notifications and
// reports, per collection, whether the debtor's bank confirmed the
// pull (notified) or it is still outstanding (pending).
//
// The join key is the EndToEndId the pacs.010 export carries — the
// journal entry ID — which the bank echoes in its notification's
// TxDtls/Refs/EndToEndId. Unlike the export, the reconciliation does
// not require the mandate to be active: a suspended mandate's past
// collections still reconcile; it only requires the mandate to exist.
// A bank that answers the same collection twice contributes its
// matched notification when there is one, else the first recorded —
// the report names what it saw, it does not invent a verdict.
//
// Read-only: the version brackets the report but does not move. One
// structured audit event ("pacs010_reconciled", details mandate_id /
// notified / pending) records the reconciliation statement.
func (l *Ledger) ReconcilePacs010(mandateID string, opts Pacs010ReconOptions) (*Pacs010ReconReport, error) {
	if !opts.Since.IsZero() && !opts.Until.IsZero() && opts.Since.After(opts.Until) {
		return nil, ErrInvalidPacs010Range
	}
	l.mu.RLock()
	m, ok := l.mandates[mandateID]
	if !ok {
		l.mu.RUnlock()
		return nil, fmt.Errorf("%w: %q", ErrMandateNotFound, mandateID)
	}
	collections, _, err := pacs010CollectionsLocked(l, m, opts.Since, opts.Until)
	if err != nil {
		l.mu.RUnlock()
		return nil, err
	}
	report := &Pacs010ReconReport{MandateID: mandateID}
	for _, c := range collections {
		item := Pacs010ReconItem{
			EntryID:     c.Entry.ID,
			AmountCents: c.Entry.AmountCents,
			Currency:    c.Entry.Currency,
			Status:      Pacs010ReconPending,
		}
		// Join against the camt.054 match registry: any imported
		// notification whose EndToEndId names this collection.
		// Notifications sort by stable ID for determinism; a matched
		// one wins over an unmatched re-send.
		var ids []string
		for id, rec := range l.camt054Matches {
			if rec.notification.EndToEndID == c.Entry.ID {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		chosen := ""
		for _, id := range ids {
			if chosen == "" || l.camt054Matches[id].status == CAMT054Matched {
				chosen = id
			}
		}
		if chosen != "" {
			rec := l.camt054Matches[chosen]
			item.Status = Pacs010ReconNotified
			item.NtfctnID = rec.notification.NtfctnID + "/" + fmt.Sprint(rec.notification.Seq)
			item.MatchStatus = string(rec.status)
			report.Notified++
		} else {
			report.Pending++
		}
		report.Items = append(report.Items, item)
	}
	l.emitAudit(AuditEvent{
		Op:            "pacs010_reconciled",
		Actor:         "ReconcilePacs010",
		TraceID:       mandateID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{m.Payer, m.Payee},
		Details: map[string]any{
			"mandate_id": mandateID,
			"notified":   report.Notified,
			"pending":    report.Pending,
		},
	})
	l.mu.RUnlock()
	return report, nil
}
