package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SWIFT FIN MT900/MT910 debit/credit confirmation import (LG-451):
// the inbound half of the MT900/MT910 lifecycle. A bank hands us an
// MT900 (confirmation of debit) or MT910 (confirmation of credit);
// ParseMT900910 parses it and ImportMT900910 returns a pre-posting
// reconciliation report: the single confirmed movement is matched
// against the journal by its :21: related reference, and the :32A:
// amount and direction are compared with the matched row.
//
// Like the MT941 import (LG-444), the MT942 import (LG-449) and
// the MT950 import (LG-450), this is deliberately a draft, not a
// posting: it writes no journal rows, enters no hash chain, and
// bumps no version. A confirmation that disagrees with its journal
// row is reported, never fail-closed — the import is evidence,
// not a booking — and the ledger is never adjusted to close the
// gap. Duplicate imports of the same document are idempotent via
// the document's SHA-256 (see mt900910Imports on Ledger). No audit
// event is emitted, along the MT941 import.
//
// An MT900/MT910 is a post-settlement confirmation, not an
// instruction (see the mt900.go package doc): it reports a movement
// that already settled. The import therefore never posts, and it
// never touches the payout state machine — a confirmation that
// cites a payout ID is matched against the journal row that
// payout settled into, not re-applied to the payout.
//
// Matching follows mt942MatchLineLocked in form — reference, then
// amount and direction — but :21: is an external reference, so the
// match runs over a four-form key set, each truncated to 16x via
// mt940Truncate: (a) the entry's mt940LineRef (idempotency key,
// else entry ID); (b) the same key with an "mt103-"/"mt202-"
// prefix stripped — inbound MT103/MT202 post with
// "mt103-"+ref / "mt202-"+ref as their idempotency key, while the
// bank cites the original :20: in :21:, so without the strip the
// confirmation would never hit; (c) the payout-derived key —
// when mt940LineRef is "payout/<id>/settle|clearing|fail" the
// payout ID <id> is a key too, because the outbound MT103 :20:
// is the payout ID, so a bank confirming that payout cites the
// payout ID in :21:; (d) the outbound MT202 export registry —
// when no journal row carries the :21:, a :20: registered in
// mt202Exports (LG-470, LG-486) is the fourth form: the bank
// confirms our own outbound MT202, which posts no journal row,
// so the export record is compared on amount and direction
// instead, with the :20: carried as ExportReference and EntryID
// left empty. The journal is always consulted first: a :21:
// that is both a journal key and an outbound :20: matches the
// journal row. Direction is account-centric, along
// mt942MatchLineLocked: the account as the debit leg reads C
// (money in), as the credit leg D (money out). An MT900 debit
// confirmation therefore requires the account to be the entry's
// credit leg, an MT910 credit confirmation the debit leg. An
// outbound MT202 is the credit leg of its own account (money
// out), so an MT900 agrees with it on direction and an MT910
// does not.
//
// When :21: is "NONREF" — the export default when there is no
// related reference to cite — there is no reference to match, and
// the import falls back to an amount+currency+direction+value-date
// single-candidate search: entries whose effective value date
// falls on the :32A: value date's calendar day. Zero candidates
// is unmatched, one is matched or mismatched on amount, several
// is mismatched with a candidate count.
//
// The :32A: value date is carried verbatim into the report in the
// reference-match path and is NOT a match key there: the booking
// date and the value date may differ by T+1, and a confirmation
// must not be refused for that (the :61: family precedent of not
// comparing dates). Pending-review rows never touched balances
// and are excluded from candidacy, like the export.
//
// The :25: account must name a known ledger account: the
// two-step rule along resolveCAMT057AccountLocked — a well-formed
// IBAN walks the registered-IBAN registry, else the account ID
// names the account directly, which must carry journaled rows or
// an IBAN registration. An unknown account is refused fail-closed
// with ErrMT900910UnknownAccount — the ledger never reconciles a
// confirmation for an account it does not hold.

// ErrMT900910UnknownAccount is returned by ImportMT900910 when the
// :25: account names no ledger account: no journaled rows, no
// registered IBAN. The HTTP layer maps it to 422.
var ErrMT900910UnknownAccount = errors.New("ledger: mt900910 inbound: account is unknown")

// MT900910ImportReport is the full pre-posting report for one
// imported MT900/MT910 confirmation. Amounts are absolute cents —
// the message type carries the side, so the report reads without
// re-deriving the marks. Match carries the single confirmation's
// reconciliation outcome against the journal (a reused
// MT942LineMatch: Reference is the :21: related reference,
// Supplementary is left empty — a confirmation carries no :86:).
// OrderingInstitutionBIC carries :52A: when the confirmation
// names one; SenderToReceiverInfo carries the :72: lines verbatim
// when the bank attached any. Duplicate is true when the report
// is a replay of an earlier import of the same document (see
// ImportMT900910).
type MT900910ImportReport struct {
	DocumentSHA256         string         `json:"document_sha256"`
	Account                AccountID      `json:"account"`
	MessageType            string         `json:"message_type"`
	TransactionReference   string         `json:"transaction_reference"`
	RelatedReference       string         `json:"related_reference"`
	ValueDate              time.Time      `json:"value_date"`
	Currency               string         `json:"currency"`
	AmountCents            int64          `json:"amount_cents"`
	OrderingInstitutionBIC string         `json:"ordering_institution_bic,omitempty"`
	SenderToReceiverInfo   []string       `json:"sender_to_receiver_info,omitempty"`
	Match                  MT942LineMatch `json:"match"`
	Duplicate              bool           `json:"duplicate,omitempty"`
}

// mt900910ResolveAccountLocked resolves a :25: account
// identification to the ledger account it names: a well-formed
// IBAN walks the registered-IBAN registry, else the account ID
// names the account directly, which must carry journaled rows or
// an IBAN registration (the resolveCAMT057AccountLocked two-step
// rule). Callers must hold l.mu (either lock suffices).
func (l *Ledger) mt900910ResolveAccountLocked(id string) (AccountID, error) {
	id = strings.TrimSpace(id)
	if validIBAN(normalizeIBAN(id)) {
		if acct, ok := l.accountByIBANLocked(normalizeIBAN(id)); ok {
			return acct, nil
		}
		return "", fmt.Errorf("%w: IBAN %q", ErrMT900910UnknownAccount, id)
	}
	acct := AccountID(id)
	if len(l.byAccount[acct]) == 0 {
		if _, ok := l.accountIBANs[acct]; !ok {
			return "", fmt.Errorf("%w: %q", ErrMT900910UnknownAccount, id)
		}
	}
	return acct, nil
}

// mt900910RefKeys returns the three-form key set a journal entry
// may be cited by in a confirmation's :21:, each truncated to 16x
// (the :21: field width): the entry's mt940LineRef as-is, the
// same key with an "mt103-"/"mt202-" prefix stripped, and — when
// the key is a payout leg ("payout/<id>/settle|clearing|fail") —
// the payout ID itself. The strip runs on the full key before
// truncation: the inbound MT103/MT202 idempotency key is
// "mt103-"+ref / "mt202-"+ref with ref up to 16x, so truncating
// first would cut the cited reference itself.
func mt900910RefKeys(e JournalEntry) []string {
	ref := mt940LineRef(e)
	seen := map[string]bool{}
	var keys []string
	add := func(k string) {
		k = mt940Truncate(k, 16)
		if k == "" || seen[k] {
			return
		}
		seen[k] = true
		keys = append(keys, k)
	}
	add(ref)
	if stripped, ok := strings.CutPrefix(ref, "mt103-"); ok {
		add(stripped)
	} else if stripped, ok := strings.CutPrefix(ref, "mt202-"); ok {
		add(stripped)
	}
	if parts := strings.Split(ref, "/"); len(parts) == 3 && parts[0] == "payout" &&
		(parts[2] == "settle" || parts[2] == "clearing" || parts[2] == "fail") {
		add(parts[1])
	}
	return keys
}

// mt900910MatchLocked matches one MT900/MT910 confirmation against
// the account's effective journal rows. In the reference path
// (:21: other than "NONREF") candidates are the rows whose
// three-form key set contains the :21:; when no row does, the
// fourth form (LG-486) consults the outbound MT202 export
// registry (mt202Exports, read directly under the held lock —
// never via GetMT202Export, which would take the read lock
// under this write lock and deadlock): a :20: registered there
// is compared on amount and direction, with the :20: carried as
// ExportReference and EntryID left empty — the export posts no
// journal row, so there is no entry to name. In the NONREF
// fallback candidates are the rows in the :32A: currency, on the
// expected direction leg, whose effective value date falls on
// the :32A: value date's calendar day. Pending-review rows never
// touched balances and are excluded in both paths, like the
// export. The :32A: value date is carried verbatim into the
// report in the reference path and is not a match key there.
// Callers must hold l.mu (either lock suffices).
func (l *Ledger) mt900910MatchLocked(account AccountID, doc MT900910) MT942LineMatch {
	// Direction is account-centric, mirroring the export: the
	// account as the debit leg reads C (money in), as the credit
	// leg D (money out). An MT910 credit confirmation therefore
	// expects the debit leg (entryCredit true); an MT900 debit
	// confirmation expects the credit leg (entryCredit false).
	credit := doc.MessageType == "910"
	match := MT942LineMatch{
		Reference:   doc.RelatedReference,
		ValueDate:   doc.ValueDate,
		AmountCents: doc.AmountCents,
		Credit:      credit,
	}
	wantEntryCredit := credit

	var cands []JournalEntry
	if doc.RelatedReference == "NONREF" {
		for _, id := range l.byAccount[account] {
			e := l.entries[id]
			if e.PendingReview || e.Currency != doc.Currency {
				continue
			}
			if (e.DebitAccount == account) != wantEntryCredit {
				continue
			}
			vd := e.EffectiveValueDate()
			if vd.Year() != doc.ValueDate.Year() || vd.Month() != doc.ValueDate.Month() || vd.Day() != doc.ValueDate.Day() {
				continue
			}
			cands = append(cands, e)
		}
	} else {
		for _, id := range l.byAccount[account] {
			e := l.entries[id]
			if e.PendingReview {
				continue
			}
			hit := false
			for _, k := range mt900910RefKeys(e) {
				if k == doc.RelatedReference {
					hit = true
					break
				}
			}
			if !hit {
				continue
			}
			cands = append(cands, e)
		}
	}
	switch len(cands) {
	case 0:
		match.Status = CAMT053Unmatched
		if doc.RelatedReference == "NONREF" {
			match.Reason = "no journal entry matches this amount, currency, direction and value date"
		} else if rec, ok := l.mt202Exports[doc.RelatedReference]; ok {
			// Fourth form (LG-486): the :21: names an outbound
			// MT202 this ledger rendered, registered in
			// mt202Exports under its :20:. The export posts no
			// journal row, so there is no entry to name —
			// EntryID stays empty and ExportReference carries
			// the :20:. The export is the credit leg of its
			// own account (money out), so an MT900 debit
			// confirmation agrees on direction and an MT910
			// credit confirmation does not. Only amount and
			// direction are compared — never currency (the
			// journal path does not compare it either) and
			// never the value date.
			exportCredit := false
			match.ExportReference = doc.RelatedReference
			match.EntryAmountCents = rec.Request.AmountCents
			match.EntryCredit = exportCredit
			if rec.Request.AmountCents == doc.AmountCents && exportCredit == wantEntryCredit {
				match.Status = CAMT053Matched
			} else {
				match.Status = CAMT053Mismatched
				if rec.Request.AmountCents != doc.AmountCents {
					match.Reason = fmt.Sprintf("amount differs: confirmation %d, export %d", doc.AmountCents, rec.Request.AmountCents)
				} else {
					match.Reason = "direction differs: confirmation and export disagree on the C/D mark"
				}
			}
		} else {
			match.Reason = "no journal entry carries this reference"
		}
	case 1:
		e := cands[0]
		entryCredit := e.DebitAccount == account
		match.EntryID = e.ID
		match.EntryAmountCents = e.AmountCents
		match.EntryCredit = entryCredit
		if e.AmountCents == doc.AmountCents && entryCredit == wantEntryCredit {
			match.Status = CAMT053Matched
			break
		}
		match.Status = CAMT053Mismatched
		if e.AmountCents != doc.AmountCents {
			match.Reason = fmt.Sprintf("amount differs: confirmation %d, journal %d", doc.AmountCents, e.AmountCents)
		} else {
			match.Reason = "direction differs: confirmation and journal disagree on the C/D mark"
		}
	default:
		match.Status = CAMT053Mismatched
		match.CandidateCount = len(cands)
		if doc.RelatedReference == "NONREF" {
			match.Reason = fmt.Sprintf("value date, currency and direction match %d journal entries", len(cands))
		} else {
			match.Reason = fmt.Sprintf("reference matches %d journal entries", len(cands))
		}
	}
	return match
}

// ImportMT900910 imports a SWIFT FIN MT900/MT910 debit/credit
// confirmation as a pre-posting reconciliation report for account:
// it parses the document, resolves the :25: account (which must
// be the import target), and matches the single confirmed movement
// against the journal by its :21: related reference — or, when
// :21: is "NONREF", by amount, currency, direction and value
// date. Nothing is posted, no hash chain is entered, and the
// journal version does not move — the report is a draft for the
// operator to review.
//
// A confirmation whose reference names no journaled row is
// unmatched; one whose row disagrees on amount or direction is
// mismatched, with both sides in the report. Neither fails the
// import — the report is evidence, not a booking. The :32A:
// value date is carried verbatim into the report and is not a
// match key in the reference path.
//
// Idempotency: the ledger keeps every import report keyed by the
// document's SHA-256. Importing the same document twice returns
// the original report with Duplicate set — a re-uploaded
// confirmation never double-counts.
//
// A confirmation naming a different account than the import
// target is an error: importing another account's confirmation
// into this one would corrupt the reconciliation.
func (l *Ledger) ImportMT900910(account AccountID, data []byte) (*MT900910ImportReport, error) {
	if account == "" {
		return nil, ErrEmptyAccountID
	}
	doc, err := ParseMT900910(string(data))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	key := hex.EncodeToString(sum[:])

	l.mu.Lock()
	defer l.mu.Unlock()

	resolved, err := l.mt900910ResolveAccountLocked(doc.Account)
	if err != nil {
		return nil, err
	}
	if resolved != account {
		return nil, fmt.Errorf("ledger: mt900910 import: confirmation names account %q, import targets %q", doc.Account, account)
	}

	if prev, ok := l.mt900910Imports[key]; ok {
		dup := *prev
		dup.Duplicate = true
		return &dup, nil
	}

	report := &MT900910ImportReport{
		DocumentSHA256:         key,
		Account:                account,
		MessageType:            doc.MessageType,
		TransactionReference:   doc.TransactionReference,
		RelatedReference:       doc.RelatedReference,
		ValueDate:              doc.ValueDate,
		Currency:               doc.Currency,
		AmountCents:            doc.AmountCents,
		OrderingInstitutionBIC: doc.OrderingInstitutionBIC,
		SenderToReceiverInfo:   append([]string(nil), doc.SenderToReceiverInfo...),
		Match:                  l.mt900910MatchLocked(account, doc),
	}

	stored := *report
	stored.SenderToReceiverInfo = append([]string(nil), report.SenderToReceiverInfo...)
	if l.mt900910Imports == nil {
		l.mt900910Imports = make(map[string]*MT900910ImportReport)
	}
	l.mt900910Imports[key] = &stored
	return report, nil
}
