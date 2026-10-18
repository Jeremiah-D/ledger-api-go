package ledger

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ErrInvalidOFXRange is returned by ExportOFX when the requested time
// range is empty: since is after until. Both bounds are optional (zero
// means unbounded); only a strictly inverted range is an error.
var ErrInvalidOFXRange = errors.New("ledger: OFX export range is invalid: since is after until")

// OFXExportOptions tunes ExportOFX.
//
// Account is required. Currency selects one statement segment: an empty
// currency exports every currency the account holds as separate
// BANKMSGSRSV1/STMTTRNRS segments, sorted by currency code. Since and
// Until bound the exported transactions (half-open [Since, Until);
// zero means unbounded). AfterEntryID is the exclusive resume
// checkpoint returned by a previous ExportOFX call: entries at or before
// it in (CreatedAt, ID) order are skipped, so a long statement can be
// exported in pages without duplicating or skipping rows. Transactions
// are journal rows where the account is either leg, excluding
// pending-review rows (they never touched balances, so a bank statement
// must not show them).
type OFXExportOptions struct {
	Account      AccountID
	Currency     string
	Since        time.Time
	Until        time.Time
	AfterEntryID string
}

// ExportOFX renders an OFX 2.x bank statement for one account from the
// journal. The document is the OFX hybrid form — the classic SGML-style
// header block followed by the OFX 2.x XML body — which is what most
// importers (accounting packages, bank uploaders) parse.
//
// Semantics, made explicit so importers can trust them:
//   - One STMTTRNRS segment per (account, currency); CURDEF names the
//     ISO 4217 currency. Currencies never mix inside a segment, matching
//     the ledger's per-currency books.
//   - One STMTTRN per effective journal row. FITID is the journal entry
//     ID — stable across exports, so importers can dedupe on it.
//   - TRNTYPE and the TRNAMT sign are account-centric: the account as the
//     debit leg means money was added to it (TRNTYPE CREDIT, positive
//     amount); the account as the credit leg means money left it
//     (TRNTYPE DEBIT, negative amount). Debit increases the account's
//     balance in this ledger's bookkeeping, so CREDIT always adds.
//   - DTPOSTED is the entry's CreatedAt in UTC (OFX datetime
//     YYYYMMDDHHMMSS). The statement window is [Since, Until).
//   - LEDGERBAL is the account's balance in that currency after every
//     effective journal row with CreatedAt < Until (or all rows when
//     Until is zero) — the true ledger state at DTEND, not just the sum
//     of the exported page. DTASOF names the same instant as DTEND when
//     Until is set, otherwise the export time.
//   - Memo carries the journal memo, REFNUM the entry's external
//     reference (when present), both XML-escaped. The OFX header block is
//     pure ASCII; the body is UTF-8.
//   - ACCTID names the account's external identifier when one is mapped
//     in the "ofx" system (see SetExternalMapping) — the identifier the
//     importing bank or accounting package expects. An unmapped account
//     exports with its internal account ID, as before.
//
// Resume: the returned checkpoint is the last exported entry's ID (""
// when nothing was exported). Pass it as AfterEntryID in the next call
// to continue exactly where this page stopped. It returns an error for
// an invalid currency or an inverted time range; an unknown account
// exports a valid empty statement (accounts are implicit in this
// ledger, like every other read path).
func (l *Ledger) ExportOFX(opts OFXExportOptions) (doc []byte, checkpoint string, err error) {
	if opts.Account == "" {
		return nil, "", ErrEmptyAccountID
	}
	currency := ""
	if opts.Currency != "" {
		var cerr error
		currency, cerr = normalizeCurrency(opts.Currency)
		if cerr != nil {
			return nil, "", cerr
		}
	}
	if !opts.Since.IsZero() && !opts.Until.IsZero() && opts.Since.After(opts.Until) {
		return nil, "", ErrInvalidOFXRange
	}

	l.mu.RLock()
	defer l.mu.RUnlock()

	// Gather every journal row touching the account, in (CreatedAt, ID)
	// order — the same ordering the cursor-paged entry lists use, so the
	// checkpoint contract matches ListAccountEntries semantics.
	var rows []JournalEntry
	for _, id := range l.byAccount[opts.Account] {
		e := l.entries[id]
		if e.PendingReview {
			// A pending-review row never touched balances: a statement
			// showing it would disagree with the ledger balance. It is
			// a journal row, but not an effective one, so the statement
			// skips it. Re-export after approval shows it.
			continue
		}
		if currency != "" && e.Currency != currency {
			continue
		}
		rows = append(rows, e)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].CreatedAt.Equal(rows[j].CreatedAt) {
			return rows[i].ID < rows[j].ID
		}
		return rows[i].CreatedAt.Before(rows[j].CreatedAt)
	})

	// Window + resume filters. The checkpoint is exclusive: entries at
	// or before it in the sorted order are skipped.
	inWindow := func(e JournalEntry) bool {
		if !opts.Since.IsZero() && e.CreatedAt.Before(opts.Since) {
			return false
		}
		if !opts.Until.IsZero() && !e.CreatedAt.Before(opts.Until) {
			return false
		}
		return true
	}
	var page []JournalEntry
	if opts.AfterEntryID == "" {
		for _, e := range rows {
			if inWindow(e) {
				page = append(page, e)
			}
		}
	} else {
		// Linear scan, not a binary search: rows are sorted by
		// (CreatedAt, ID), which is not ID-monotonic, so a binary
		// search on the ID would silently miss valid checkpoints
		// (non-monotonic predicate). A scan is O(rows) — the OFX
		// path is already O(rows) from the gather above.
		found, passed := false, false
		for _, e := range rows {
			if !passed {
				if e.ID == opts.AfterEntryID {
					passed, found = true, true
				}
				continue
			}
			if inWindow(e) {
				page = append(page, e)
			}
		}
		if !found {
			// Entries are never deleted, so an unknown checkpoint is
			// a caller error, not an empty page: failing loud beats
			// silently re-exporting from the start (which would
			// duplicate rows downstream). ErrInvalidCursor keeps the
			// contract of the cursor-paged entry lists.
			return nil, "", ErrInvalidCursor
		}
	}
	if len(page) > 0 {
		checkpoint = page[len(page)-1].ID
	}

	// Per-currency segments, sorted by currency code. A requested single
	// currency always yields exactly one segment, even when the account
	// never posted in it.
	currencies := []string{}
	if currency != "" {
		currencies = []string{currency}
	} else {
		seen := make(map[string]bool)
		for _, e := range rows {
			if !seen[e.Currency] {
				seen[e.Currency] = true
				currencies = append(currencies, e.Currency)
			}
		}
		sort.Strings(currencies)
	}

	var b strings.Builder
	b.WriteString(ofxHeaderBlock)
	var stmts strings.Builder
	// The statement names the account by its external identifier when
	// one is mapped in the "ofx" system (see SetExternalMapping): the
	// importing bank or accounting package keys on that identifier,
	// not on the internal account ID. An unmapped account exports with
	// the internal account ID, as before.
	acctID := string(opts.Account)
	if ext, ok := l.externalAccountIDLocked(opts.Account, ExternalSystemOFX); ok {
		acctID = ext
	}
	for _, c := range currencies {
		var seg []JournalEntry
		for _, e := range page {
			if e.Currency == c {
				seg = append(seg, e)
			}
		}
		stmts.WriteString(ofxStatement(opts.Account, acctID, c, seg, l.balanceAtLocked(opts.Account, c, opts.Until), opts, checkpoint))
	}
	// Zero currencies (unknown account, or an account with no postings
	// and no currency requested) still yields a valid empty document —
	// accounts are implicit in this ledger, like every other read path.
	b.WriteString("<OFX>\n")
	b.WriteString(ofxSignon())
	b.WriteString("<BANKMSGSRSV1>\n")
	b.WriteString(stmts.String())
	b.WriteString("</BANKMSGSRSV1>\n")
	b.WriteString("</OFX>\n")
	return []byte(b.String()), checkpoint, nil
}

// balanceAtLocked folds every effective journal row of (account,
// currency) with CreatedAt < until (or all rows when until is zero)
// into the balance — the ledger state at the statement's DTEND. Callers
// must hold l.mu. Pending-review rows never touched balances and are
// skipped so the folded balance agrees with BalanceIn.
func (l *Ledger) balanceAtLocked(a AccountID, currency string, until time.Time) int64 {
	var bal int64
	for _, id := range l.byAccount[a] {
		e := l.entries[id]
		if e.PendingReview || e.Currency != currency {
			continue
		}
		if !until.IsZero() && !e.CreatedAt.Before(until) {
			continue
		}
		if e.DebitAccount == a {
			bal += e.AmountCents
		} else {
			bal -= e.AmountCents
		}
	}
	return bal
}

// ofxHeaderBlock is the classic OFX SGML-style header that precedes the
// OFX 2.x XML body: OFX 2.x keeps this header for backward compatibility,
// and most importers key on it.
const ofxHeaderBlock = `OFXHEADER:100
DATA:OFXSGML
VERSION:211
SECURITY:NONE
ENCODING:UTF-8
CHARSET:UTF-8
COMPRESSION:NONE
OLDFILEUID:NONE
NEWFILEUID:NONE

<?xml version="1.0" encoding="UTF-8"?>
<?OFX OFXHEADER="200" VERSION="211" SECURITY="NONE" OLDFILEUID="NONE" NEWFILEUID="NONE"?>

`

// ofxSignon is a minimal sign-on response: this ledger is not a bank, so
// there is no session to authenticate — the block exists because OFX
// requires SIGNONMSGSRSV1 before the banking messages.
func ofxSignon() string {
	return fmt.Sprintf(`<SIGNONMSGSRSV1>
<SONRS>
<STATUS><CODE>0</CODE><SEVERITY>INFO</SEVERITY></STATUS>
<DTSERVER>%s</DTSERVER>
<LANGUAGE>ENG</LANGUAGE>
</SONRS>
</SIGNONMSGSRSV1>
`, time.Now().UTC().Format(ofxDateTimeFormat))
}

const ofxDateTimeFormat = "20060102150405"

// ofxEscape XML-escapes element text for the OFX XML body.
func ofxEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		"\"", "&quot;",
		"'", "&apos;",
	)
	return r.Replace(s)
}

// ofxAmount formats integer cents as a decimal dollars string with the
// sign in front: -105 -> "-1.05", 50 -> "0.50". No floats anywhere.
func ofxAmount(cents int64) string {
	sign := ""
	if cents < 0 {
		sign = "-"
		cents = -cents
	}
	return fmt.Sprintf("%s%d.%02d", sign, cents/100, cents%100)
}

// ofxStatement renders one STMTTRNRS segment for (account, currency).
// The TRNUID is the export checkpoint, so a re-exported segment carries
// the same statement identity only when the page content is identical.
// acctID is the account identifier the statement names in <ACCTID>:
// the external identifier when one is mapped in the "ofx" system (see
// ExportOFX), the internal account ID otherwise. The account-centric
// TRNTYPE/TRNAMT signs still use the ledger account itself.
func ofxStatement(account AccountID, acctID string, currency string, entries []JournalEntry, balanceCents int64, opts OFXExportOptions, trnUID string) string {
	var b strings.Builder
	b.WriteString("<STMTTRNRS>\n")
	b.WriteString(fmt.Sprintf("<TRNUID>%s</TRNUID>\n", ofxEscape(trnUID)))
	b.WriteString("<STATUS><CODE>0</CODE><SEVERITY>INFO</SEVERITY></STATUS>\n")
	b.WriteString("<STMTRS>\n")
	b.WriteString(fmt.Sprintf("<CURDEF>%s</CURDEF>\n", ofxEscape(currency)))
	b.WriteString("<BANKACCTFROM>\n")
	b.WriteString("<BANKID>LEDGER</BANKID>\n")
	b.WriteString(fmt.Sprintf("<ACCTID>%s</ACCTID>\n", ofxEscape(acctID)))
	b.WriteString("<ACCTTYPE>CHECKING</ACCTTYPE>\n")
	b.WriteString("</BANKACCTFROM>\n")
	b.WriteString("<BANKTRANLIST>\n")
	if !opts.Since.IsZero() {
		b.WriteString(fmt.Sprintf("<DTSTART>%s</DTSTART>\n", opts.Since.UTC().Format(ofxDateTimeFormat)))
	} else if len(entries) > 0 {
		b.WriteString(fmt.Sprintf("<DTSTART>%s</DTSTART>\n", entries[0].CreatedAt.UTC().Format(ofxDateTimeFormat)))
	}
	dtEnd := opts.Until
	if dtEnd.IsZero() && len(entries) > 0 {
		dtEnd = entries[len(entries)-1].CreatedAt
	}
	if !dtEnd.IsZero() {
		b.WriteString(fmt.Sprintf("<DTEND>%s</DTEND>\n", dtEnd.UTC().Format(ofxDateTimeFormat)))
	}
	for _, e := range entries {
		trnType := "CREDIT"
		signed := e.AmountCents
		if e.CreditAccount == account {
			// Money left the account: OFX DEBIT, negative amount.
			trnType = "DEBIT"
			signed = -signed
		}
		b.WriteString("<STMTTRN>\n")
		b.WriteString(fmt.Sprintf("<TRNTYPE>%s</TRNTYPE>\n", trnType))
		b.WriteString(fmt.Sprintf("<DTPOSTED>%s</DTPOSTED>\n", e.CreatedAt.UTC().Format(ofxDateTimeFormat)))
		b.WriteString(fmt.Sprintf("<TRNAMT>%s</TRNAMT>\n", ofxAmount(signed)))
		b.WriteString(fmt.Sprintf("<FITID>%s</FITID>\n", ofxEscape(e.ID)))
		b.WriteString("<NAME>JOURNAL</NAME>\n")
		if e.Memo != "" {
			b.WriteString(fmt.Sprintf("<MEMO>%s</MEMO>\n", ofxEscape(e.Memo)))
		}
		if e.ExternalRef != "" {
			b.WriteString(fmt.Sprintf("<REFNUM>%s</REFNUM>\n", ofxEscape(e.ExternalRef)))
		}
		b.WriteString("</STMTTRN>\n")
	}
	b.WriteString("</BANKTRANLIST>\n")
	asof := dtEnd
	if asof.IsZero() {
		asof = time.Now().UTC()
	}
	b.WriteString("<LEDGERBAL>\n")
	b.WriteString(fmt.Sprintf("<BALAMT>%s</BALAMT>\n", ofxAmount(balanceCents)))
	b.WriteString(fmt.Sprintf("<DTASOF>%s</DTASOF>\n", asof.UTC().Format(ofxDateTimeFormat)))
	b.WriteString("</LEDGERBAL>\n")
	b.WriteString("</STMTRS>\n")
	b.WriteString("</STMTTRNRS>\n")
	return b.String()
}
