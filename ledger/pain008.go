package ledger

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ISO 20022 pain.008.001.02 customer direct debit initiation export
// (LG-166): the outbound half of the direct-debit mandate lifecycle.
//
// DebitWithMandate (see mandate.go, LG-125) books each authorized pull
// as a journal entry the moment it is authorized; ExportPain008
// renders those booked collections as the pain.008.001.02 instruction
// file the creditor (the merchant) sends to its bank — the SEPA
// direct-debit counterpart to ExportPain001 (see pain001.go), which
// renders credit transfers. The two are orthogonal by construction:
// pain.001 moves money to the creditor (CdtTrfTxInf, the payer is the
// debtor); pain.008 pulls money from the debtor (DrctDbtTxInf, the
// payer is the debtor). pain.002 status reports (see pain002.go) answer
// pain.001 messages only — a pain.002 naming a pain.008 MsgId is
// refused by ErrPain002UnexpectedMsgName, so the .008 lifecycle never
// collides with the .001 one.
//
// Sequence types (SeqTp) are derived per collection from the mandate's
// own debit history — the ledger is the system of record for the
// mandate's usage, so it can state each collection's position honestly:
//
//	FRST  the mandate's first collection (no prior debits)
//	RCUR  a recurring collection (prior debits exist, more windows remain)
//	FNAL  the final collection (this debit sits in the mandate's last
//	      authorizable frequency window)
//	OOFF  a one-off collection (the mandate's whole validity fits in a
//	      single frequency window, so at most one debit can ever exist)
//
// A pain.008.001.02 remittance must group payment orders with the same
// creditor, the same collection date, and the same sequence type, so
// each (SeqTp, ReqdColltnDt) group renders as its own PmtInf carrying
// that SeqTp — the standard shape, not a per-transaction SeqTp.
//
// Fail-closed, like the pain.001 export: the mandate must exist and be
// effectively active (unknown/suspended/cancelled/expired mandates
// refuse the export), the creditor scheme ID must be supplied (SEPA
// requires CdtrSchmeId and the ledger stores none — there is no honest
// default), and both the debtor (payer) and creditor (payee) need
// registered IBANs.
//
// The export is read-only: it takes the read lock, bumps no version,
// appends no chain link, and consumes no idempotency key. Like
// Reconcile, it still emits one structured audit event
// ("pain008_exported") so the compliance trail records every
// generated instruction.

// pain.008 export errors.
var (
	// ErrMissingPain008CreditorScheme is returned by ExportPain008
	// when no creditor scheme ID is supplied. SEPA mandates
	// CdtrSchmeId on every pain.008, and the ledger stores no
	// creditor identifier on the mandate — inventing one would forge
	// the instruction, so the export is refused instead.
	ErrMissingPain008CreditorScheme = errors.New("ledger: pain.008 export refused: creditor scheme ID is required")
	// ErrInvalidPain008Range is returned by ExportPain008 when the
	// requested time range is empty: since is after until. Mirrors
	// ErrInvalidPain001Range.
	ErrInvalidPain008Range = errors.New("ledger: pain.008 export range is invalid: since is after until")
	// ErrPain008AmountOverflow is returned by ExportPain008 when the
	// control sum of the exported collections overflows int64: no real
	// control total could be computed, so the export is refused.
	ErrPain008AmountOverflow = errors.New("ledger: pain.008 control sum overflows int64")
)

// pain.008 sequence types (the SeqTp vocabulary).
const (
	Pain008SeqFRST = "FRST" // first collection of the mandate
	Pain008SeqRCUR = "RCUR" // recurring collection
	Pain008SeqFNAL = "FNAL" // final collection of the mandate
	Pain008SeqOOFF = "OOFF" // one-off collection
)

// Pain008ExportOptions tunes ExportPain008.
//
// CreditorSchemeID is required: the SEPA creditor identifier that
// rides CdtrSchmeId (fail-closed when empty, see
// ErrMissingPain008CreditorScheme). Since and Until bound the
// exported collections (half-open [Since, Until) on the debit's
// booking timestamp, the OFX export convention; zero means
// unbounded).
type Pain008ExportOptions struct {
	CreditorSchemeID string
	Since            time.Time
	Until            time.Time
}

// pain008Collection is one exported direct-debit collection: the
// booked mandate debit plus its derived sequence type and the
// resolved debtor/creditor IBANs.
type pain008Collection struct {
	Entry      JournalEntry
	SeqTp      string
	DebtorIBAN AccountIBAN // the mandate payer (customer)
	CreditorIBAN AccountIBAN // the mandate payee (merchant)
}

// pain008SeqTp derives the sequence type of one collection from the
// mandate's debit history: priorDebits is the number of debits booked
// under the mandate before this one, ts is this debit's booking
// timestamp. The rules mirror the SEPA definitions against the
// mandate's own frequency windows (see mandateWindow):
//
//   - OOFF when the mandate's entire validity fits in a single
//     frequency window: the frequency gate allows at most one debit
//     per window, so at most one collection can ever exist.
//   - FNAL when this debit sits in the last authorizable window: the
//     next window starts at or after ValidUntil, so no further debit
//     is possible.
//   - FRST when no prior debit exists, RCUR otherwise.
func pain008SeqTp(m Mandate, priorDebits int, ts time.Time) string {
	fromStart, _ := mandateWindow(m.Frequency, m.ValidFrom)
	// ValidUntil is exclusive; the last authorizable instant is just
	// before it.
	lastStart, _ := mandateWindow(m.Frequency, m.ValidUntil.Add(-time.Nanosecond))
	if fromStart.Equal(lastStart) {
		return Pain008SeqOOFF
	}
	_, windowEnd := mandateWindow(m.Frequency, ts)
	if !windowEnd.Before(m.ValidUntil) {
		return Pain008SeqFNAL
	}
	if priorDebits == 0 {
		return Pain008SeqFRST
	}
	return Pain008SeqRCUR
}

// pain008MsgID derives the deterministic message ID from the mandate,
// the creditor scheme, the window, and the exported content — the
// pain001MsgID convention: the same export re-renders byte-identical,
// and any content change changes the ID.
func pain008MsgID(mandateID, schemeID string, opts Pain008ExportOptions, entryIDs []string) string {
	var b strings.Builder
	b.WriteString("pain008-")
	b.WriteString(pain001UnixOrZero(opts.Since))
	b.WriteString("-")
	b.WriteString(pain001UnixOrZero(opts.Until))
	b.WriteString("-")
	b.WriteString(mandateID)
	b.WriteString("-")
	b.WriteString(schemeID)
	for _, id := range entryIDs {
		b.WriteString("-")
		b.WriteString(id)
	}
	return b.String()
}

// ExportPain008 renders an ISO 20022 pain.008.001.02 customer direct
// debit initiation for the booked collections of one mandate (see
// DebitWithMandate).
//
// Semantics, made explicit so the creditor's bank can trust them:
//   - One DrctDbtTxInf per booked debit in the window; EndToEndId is
//     the journal entry ID — stable across exports, so the bank can
//     dedupe on it.
//   - The debtor is the mandate's payer (the customer), the creditor
//     the mandate's payee (the merchant); their account identifiers
//     are the registered IBANs, the BICs ride DbtrAgt/CdtrAgt when
//     registered.
//   - MndtId is the mandate ID; DtOfSgntr is the mandate's ValidFrom
//     date — the day the debtor authorized the pull.
//   - SeqTp is derived per collection from the mandate's debit
//     history (see pain008SeqTp); collections group into one PmtInf
//     per (SeqTp, ReqdColltnDt), each carrying its SeqTp.
//   - ReqdColltnDt is the collection's booking date: the day the
//     ledger authorized the pull.
//   - Amounts are the ledger's integer cents rendered as exact
//     decimals (two fraction digits, no floats anywhere).
//   - MsgId is deterministic in (mandate, scheme, window, content).
//   - Fail-closed: an unknown, suspended, cancelled, or expired
//     mandate refuses the export, as do a missing creditor scheme ID
//     and missing debtor/creditor IBANs.
//
// The export is read-only: it takes the read lock, bumps no version,
// appends no chain link, and consumes no idempotency key. Like
// Reconcile, it still emits one structured audit event
// ("pain008_exported", details msg_id/tx_count/ctrl_sum_cents/
// mandate_id) so the compliance trail records every generated
// instruction.
func (l *Ledger) ExportPain008(mandateID string, opts Pain008ExportOptions) ([]byte, error) {
	if strings.TrimSpace(opts.CreditorSchemeID) == "" {
		return nil, ErrMissingPain008CreditorScheme
	}
	if !opts.Since.IsZero() && !opts.Until.IsZero() && opts.Since.After(opts.Until) {
		return nil, ErrInvalidPain008Range
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

	// Walk the full debit history in booking order: the sequence type
	// counts prior debits, so the position is the index in the whole
	// history, not in the filtered window.
	var collections []pain008Collection
	var entryIDs []string
	debitIDs := l.mandateDebits[mandateID]
	for i, id := range debitIDs {
		e, ok := l.entries[id]
		if !ok {
			// Append-only journal: a missing row is data
			// corruption, not a gap to paper over — but the
			// export must not invent a sequence position either.
			// Refuse fail-loud: a half-readable history must not
			// render an instruction.
			l.mu.RUnlock()
			return nil, fmt.Errorf("ledger: pain.008 export refused: mandate debit %q not in journal", id)
		}
		if !opts.Since.IsZero() && e.CreatedAt.Before(opts.Since) {
			continue
		}
		if !opts.Until.IsZero() && !e.CreatedAt.Before(opts.Until) {
			continue
		}
		debtorIBAN, ok := l.accountIBANs[m.Payer]
		if !ok {
			l.mu.RUnlock()
			return nil, fmt.Errorf("%w: debtor %q", ErrMissingAccountIBAN, m.Payer)
		}
		creditorIBAN, ok := l.accountIBANs[m.Payee]
		if !ok {
			l.mu.RUnlock()
			return nil, fmt.Errorf("%w: creditor %q", ErrMissingAccountIBAN, m.Payee)
		}
		collections = append(collections, pain008Collection{
			Entry:        e,
			SeqTp:        pain008SeqTp(m, i, e.CreatedAt),
			DebtorIBAN:   debtorIBAN,
			CreditorIBAN: creditorIBAN,
		})
		entryIDs = append(entryIDs, id)
	}

	doc, msgID, txCount, ctrlSum, err := renderPain008(m, opts, collections, entryIDs)
	if err != nil {
		l.mu.RUnlock()
		return nil, err
	}
	// Read-only, like Reconcile and the pain.001 export: the version
	// brackets the export but does not move.
	l.emitAudit(AuditEvent{
		Op:            "pain008_exported",
		Actor:         "ExportPain008",
		TraceID:       msgID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{m.Payer, m.Payee},
		Details: map[string]any{
			"msg_id":             msgID,
			"mandate_id":         mandateID,
			"tx_count":           txCount,
			"ctrl_sum_cents":     ctrlSum,
			"currency":           m.Currency,
			"creditor_scheme_id": opts.CreditorSchemeID,
		},
	})
	l.mu.RUnlock()
	return doc, nil
}

// renderPain008 builds the pain.008.001.02 document for the resolved
// collections, grouped into one PmtInf per (SeqTp, ReqdColltnDt) —
// the SEPA grouping rule: one remittance per creditor, collection
// date, and sequence type — and returns the document plus the GrpHdr
// facts for the audit event.
func renderPain008(m Mandate, opts Pain008ExportOptions, collections []pain008Collection, entryIDs []string) (doc []byte, msgID string, txCount int, ctrlSum int64, err error) {
	msgID = pain008MsgID(m.ID, opts.CreditorSchemeID, opts, entryIDs)
	// Control sum over every exported collection, overflow-checked:
	// a control total that wrapped would be worse than no export.
	var sum int64
	for _, c := range collections {
		var ok bool
		sum, ok = addCents(sum, c.Entry.AmountCents)
		if !ok {
			return nil, "", 0, 0, ErrPain008AmountOverflow
		}
	}
	txCount = len(collections)
	ctrlSum = sum

	// Group by (SeqTp, collection date), SeqTp-major then date — the
	// document is deterministic in the collection set.
	type groupKey struct {
		seqTp string
		date  string // ReqdColltnDt, YYYY-MM-DD
	}
	groups := map[groupKey][]pain008Collection{}
	var keys []groupKey
	for _, c := range collections {
		k := groupKey{seqTp: c.SeqTp, date: c.Entry.CreatedAt.UTC().Format("2006-01-02")}
		if _, ok := groups[k]; !ok {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], c)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].seqTp != keys[j].seqTp {
			return keys[i].seqTp < keys[j].seqTp
		}
		return keys[i].date < keys[j].date
	})

	now := time.Now().UTC()
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pain.008.001.02">` + "\n")
	b.WriteString("<CstmrDrctDbtInitn>\n")
	b.WriteString("<GrpHdr>\n")
	b.WriteString(fmt.Sprintf("<MsgId>%s</MsgId>\n", ofxEscape(msgID)))
	b.WriteString(fmt.Sprintf("<CreDtTm>%s</CreDtTm>\n", now.Format(time.RFC3339)))
	b.WriteString(fmt.Sprintf("<NbOfTxs>%d</NbOfTxs>\n", txCount))
	b.WriteString(fmt.Sprintf("<CtrlSum>%s</CtrlSum>\n", pain001Amount(ctrlSum)))
	b.WriteString("<InitgPty><Nm>LEDGER</Nm></InitgPty>\n")
	b.WriteString("</GrpHdr>\n")
	for _, k := range keys {
		cs := groups[k]
		var gsum int64
		for _, c := range cs {
			gsum, _ = addCents(gsum, c.Entry.AmountCents) // cannot overflow: bounded by the checked total
		}
		b.WriteString("<PmtInf>\n")
		b.WriteString(fmt.Sprintf("<PmtInfId>%s-%s-%s</PmtInfId>\n",
			ofxEscape(msgID), ofxEscape(k.seqTp), ofxEscape(k.date)))
		b.WriteString("<PmtMtd>DD</PmtMtd>\n")
		b.WriteString(fmt.Sprintf("<NbOfTxs>%d</NbOfTxs>\n", len(cs)))
		b.WriteString(fmt.Sprintf("<CtrlSum>%s</CtrlSum>\n", pain001Amount(gsum)))
		b.WriteString("<PmtTpInf>\n")
		b.WriteString("<SvcLvl><Cd>SEPA</Cd></SvcLvl>\n")
		b.WriteString("<LclInstrm><Cd>CORE</Cd></LclInstrm>\n")
		b.WriteString(fmt.Sprintf("<SeqTp>%s</SeqTp>\n", k.seqTp))
		b.WriteString("</PmtTpInf>\n")
		b.WriteString(fmt.Sprintf("<ReqdColltnDt>%s</ReqdColltnDt>\n", k.date))
		// The creditor is the mandate's payee (the merchant):
		// identical across the group by construction.
		b.WriteString(fmt.Sprintf("<Cdtr><Nm>%s</Nm></Cdtr>\n", ofxEscape(string(m.Payee))))
		b.WriteString(fmt.Sprintf("<CdtrAcct><Id><IBAN>%s</IBAN></Id></CdtrAcct>\n", ofxEscape(cs[0].CreditorIBAN.IBAN)))
		if cs[0].CreditorIBAN.BIC != "" {
			b.WriteString(fmt.Sprintf("<CdtrAgt><FinInstnId><BIC>%s</BIC></FinInstnId></CdtrAgt>\n", ofxEscape(cs[0].CreditorIBAN.BIC)))
		}
		b.WriteString("<ChrgBr>SLEV</ChrgBr>\n")
		b.WriteString(fmt.Sprintf("<CdtrSchmeId><Id><PrtryId><Id>%s</Id></PrtryId></Id></CdtrSchmeId>\n",
			ofxEscape(opts.CreditorSchemeID)))
		for _, c := range cs {
			b.WriteString("<DrctDbtTxInf>\n")
			b.WriteString(fmt.Sprintf("<PmtId><InstrId>%s</InstrId><EndToEndId>%s</EndToEndId></PmtId>\n",
				ofxEscape(c.Entry.ID), ofxEscape(c.Entry.ID)))
			b.WriteString(fmt.Sprintf("<InstdAmt Ccy=\"%s\">%s</InstdAmt>\n",
				ofxEscape(c.Entry.Currency), pain001Amount(c.Entry.AmountCents)))
			b.WriteString("<DrctDbtTx>\n")
			b.WriteString("<MndtRltdInf>\n")
			b.WriteString(fmt.Sprintf("<MndtId>%s</MndtId>\n", ofxEscape(m.ID)))
			b.WriteString(fmt.Sprintf("<DtOfSgntr>%s</DtOfSgntr>\n", m.ValidFrom.UTC().Format("2006-01-02")))
			b.WriteString("<AmdmntInd>false</AmdmntInd>\n")
			b.WriteString("</MndtRltdInf>\n")
			b.WriteString("</DrctDbtTx>\n")
			if c.DebtorIBAN.BIC != "" {
				b.WriteString(fmt.Sprintf("<DbtrAgt><FinInstnId><BIC>%s</BIC></FinInstnId></DbtrAgt>\n", ofxEscape(c.DebtorIBAN.BIC)))
			}
			b.WriteString(fmt.Sprintf("<Dbtr><Nm>%s</Nm></Dbtr>\n", ofxEscape(string(m.Payer))))
			b.WriteString(fmt.Sprintf("<DbtrAcct><Id><IBAN>%s</IBAN></Id></DbtrAcct>\n", ofxEscape(c.DebtorIBAN.IBAN)))
			if strings.TrimSpace(c.Entry.Memo) != "" {
				b.WriteString(fmt.Sprintf("<RmtInf><Ustrd>%s</Ustrd></RmtInf>\n", ofxEscape(c.Entry.Memo)))
			}
			b.WriteString("</DrctDbtTxInf>\n")
		}
		b.WriteString("</PmtInf>\n")
	}
	b.WriteString("</CstmrDrctDbtInitn>\n")
	b.WriteString("</Document>\n")
	return []byte(b.String()), msgID, txCount, ctrlSum, nil
}
