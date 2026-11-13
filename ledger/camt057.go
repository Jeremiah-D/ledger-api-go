package ledger

import (
	"encoding/xml"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ISO 20022 camt.057.001 Notification to Receive export (LG-403,
// fintech: ISO 20022 收款通知 MX 导出面).
//
// This is the MX twin of the MT210 FIN notice (LG-393, mt210.go):
// same source (one transfer schedule's next run), same direction
// (account owner → account bank — "there will be money coming
// into my account"), same limits (one next run only, the fee is a
// separate leg that never reduces the notified principal, an FX
// schedule cannot be notified because the receivable amount is
// only known at fire time). It is a pre-advice, not a receipt
// confirmation: camt.054 covers money that has already landed
// (bank → customer, after the fact); camt.057 covers money that
// is about to land (customer → bank, before the fact). camt.060
// asks the bank for a report; this one tells the bank to expect
// funds. The two notices must agree field for field on the same
// schedule and run — see TestExportCAMT057CrossCheckMT210.
//
// Structure, verified against the ISO 20022 camt.057 message
// definition (NotificationToReceiveV06, namespace
// camt.057.001.06 — the CBPR+ current version; IBM SWIFT Standard
// Release 2025 and the Deutsche Bank CBPR+ portfolio list
// camt.057.001.06 as the live version, and no .001.07 revision
// exists in the registry — the .001.05 XSD-generated types at
// pkg.go.dev/github.com/mgruener/iso20022tpl confirm the element
// names, which are stable across the maintenance):
//
//	Document (xmlns camt.057.001.06)
//	  NtfctnToRcv
//	    GrpHdr: MsgId (deterministic, see below), CreDtTm
//	    Ntfctn: Id (= MsgId), Acct, AcctSvcr, TtlAmt, XpctdValDt,
//	      Dbtr, Itm (exactly one, the next run)
//
// Two corrections to the LG-403 draft's tag spellings, from that
// verification — the draft deferred spellings to this check:
//   - The notification-level amount is TtlAmt, not Amt. Ntfctn
//     carries no direct Amt child; Amt lives inside each Itm
//     (NotificationItem6: Id, Amt mandatory, XpctdValDt, Dbtr).
//     Both are written here and both equal the principal, so the
//     total and the single item agree.
//   - Itm is mandatory (1..n): a camt.057 with no item is not a
//     valid notification, so the single next run is always
//     rendered as one Itm (Id = the run transfer ID).
//
// Field mapping, following the MT210 precedent exactly:
//   - MsgId is deterministic: "camt057-<scheduleID>-<runSeq>"
//     (Max35Text — a schedule whose ID makes it longer fails
//     loud with ErrInvalidCAMT057Export rather than truncating).
//     CreDtTm is the honest wall-clock creation time, so two
//     exports a second apart differ in that one line only; repeat
//     exports within the same second are byte-identical, and every
//     other field is pinned to the stored schedule and its RunSeq
//     — once a run fires, RunSeq advances and MsgId, Ntfctn/Id,
//     Itm/Id, and XpctdValDt move with it, which is a content
//     change, not drift.
//   - Acct is the account to be credited: the To account's
//     registered IBAN when one exists (Acct/Id/IBAN), else the
//     account ID (Acct/Id/Othr/Id, uppercased) — the MT210 :25:
//     rule. IBAN is Max34 and Othr/Id is Max34Text, so a longer
//     identifier fails loud.
//   - AcctSvcr names the account servicing institution: the
//     caller-given ServicingBIC (FinInstnId/BICFI). Without it the
//     BIC would be validated but never rendered.
//   - TtlAmt / Itm/Amt are the principal only:
//     AmountCents + Currency (camt053FormatAmount, "100.00"),
//     FeeCents is a separate fee leg and never deducted.
//   - XpctdValDt is the next run date (ISODate, UTC).
//   - Dbtr is the From party, the MT210 :50a:/:52a: rule in MX
//     form: a registered BIC renders Dbtr/Agt/FinInstnId/BICFI,
//     otherwise Dbtr/Pty/Nm names the ordering customer by
//     account ID (uppercased) — never both.
//
// The export is read-only: it takes the read lock only (via
// GetTransferSchedule and the identifier lookup), bumps no
// version, books nothing, and appends no audit event. Only an
// active schedule notifies: paused, cancelled, and completed
// schedules have no next run to announce, as does a schedule
// whose NextRunAt has already passed its EndsAt — all report
// ErrCAMT057ScheduleNotActive.

// ErrInvalidCAMT057Export is returned when a schedule cannot be
// rendered as a camt.057: a servicing BIC that is not a valid
// 8/11 BIC, an FX schedule whose receivable amount is unknowable
// until fire time, a MsgId / run transfer ID / credit account
// outside its Max35/Max34 bound, or a reference carrying a line
// break. It is a 400-class error: the stored schedule cannot be
// expressed as a valid notice.
var ErrInvalidCAMT057Export = errors.New("ledger: invalid camt.057 export")

// ErrCAMT057ScheduleNotActive is returned when a camt.057 is
// requested for a schedule that has no next run to notify:
// paused, cancelled, completed, or past its EndsAt. It is a
// 422-class error: the schedule exists, but there is nothing to
// announce.
var ErrCAMT057ScheduleNotActive = errors.New("ledger: transfer schedule is not active for camt.057 notice")

// CAMT057ExportOptions tunes ExportCAMT057.
//
// ScheduleID names the transfer schedule whose next run is
// notified. ServicingBIC is the BIC of the bank holding the
// account to be credited (required, 8 or 11 characters): the
// ledger holds no bank-identity registry, so the caller names the
// servicing bank on every export.
type CAMT057ExportOptions struct {
	ScheduleID   string
	ServicingBIC string
}

// camt057MsgID derives the deterministic message/notification ID
// from the schedule and the run sequence: the same schedule at
// the same RunSeq re-renders the same ID, and any run firing
// changes it.
func camt057MsgID(scheduleID string, runSeq uint64) string {
	return fmt.Sprintf("camt057-%s-%d", scheduleID, runSeq)
}

// camt057RunID derives the run transfer ID ("<ID>/run/<seq>"),
// the MT210 :21: value, used as the notification item ID.
func camt057RunID(scheduleID string, runSeq uint64) string {
	return fmt.Sprintf("%s/run/%d", scheduleID, runSeq)
}

// camt057Reference validates a Max35Text reference — the MsgId
// and the item ID: 1–35 characters, no line breaks (a reference
// must not smuggle a second line into the document). Anything
// else fails loud rather than emitting a malformed notice.
func camt057Reference(field, ref string) error {
	if ref == "" || len(ref) > 35 {
		return fmt.Errorf("%w: %s want 1-35 characters, got %q", ErrInvalidCAMT057Export, field, ref)
	}
	if strings.ContainsAny(ref, "\r\n") {
		return fmt.Errorf("%w: %s line break in a single-line field: %q", ErrInvalidCAMT057Export, field, ref)
	}
	return nil
}

// The camt057x* structs are the export-side XML model, the
// camt060.go convention: a strict writer carrying the
// camt.057.001.06 namespace on the root and omitting every empty
// element, so the emitted document is what a bank expects to
// receive.
type camt057xDocument struct {
	XMLName     xml.Name            `xml:"urn:iso:std:iso:20022:tech:xsd:camt.057.001.06 Document"`
	NtfctnToRcv camt057xNtfctnToRcv `xml:"NtfctnToRcv"`
}

type camt057xNtfctnToRcv struct {
	GrpHdr camt057xGrpHdr `xml:"GrpHdr"`
	Ntfctn camt057xNtfctn `xml:"Ntfctn"`
}

type camt057xGrpHdr struct {
	MsgID   string `xml:"MsgId"`
	CreDtTm string `xml:"CreDtTm"`
}

type camt057xNtfctn struct {
	ID         string       `xml:"Id"`
	Acct       camt057xAcct `xml:"Acct"`
	AcctSvcr   camt057xAgt  `xml:"AcctSvcr"`
	TtlAmt     camt057xAmt  `xml:"TtlAmt"`
	XpctdValDt string       `xml:"XpctdValDt"`
	Dbtr       camt057xDbtr `xml:"Dbtr"`
	Itm        camt057xItm  `xml:"Itm"`
}

// camt057xItm is the single notification item: the next run.
// Its Amt is mandatory (NotificationItem6), and it repeats the
// notification-level amount and value date so the item stands
// alone as the advice for that run.
type camt057xItm struct {
	ID         string       `xml:"Id"`
	Amt        camt057xAmt  `xml:"Amt"`
	XpctdValDt string       `xml:"XpctdValDt"`
	Dbtr       camt057xDbtr `xml:"Dbtr"`
}

type camt057xAcct struct {
	ID camt057xAcctID `xml:"Id"`
}

type camt057xAcctID struct {
	IBAN string        `xml:"IBAN,omitempty"`
	Othr *camt057xOthr `xml:"Othr,omitempty"`
}

type camt057xOthr struct {
	ID string `xml:"Id"`
}

type camt057xAmt struct {
	Ccy   string `xml:"Ccy,attr"`
	Value string `xml:",chardata"`
}

// camt057xDbtr is the Party12Choice debtor: exactly one of Pty
// (ordering customer, named by account ID) or Agt (ordering
// institution, by BIC).
type camt057xDbtr struct {
	Pty *camt057xPty `xml:"Pty,omitempty"`
	Agt *camt057xAgt `xml:"Agt,omitempty"`
}

type camt057xPty struct {
	Nm string `xml:"Nm"`
}

type camt057xAgt struct {
	FinInstnID camt057xFinInstnID `xml:"FinInstnId"`
}

type camt057xFinInstnID struct {
	BICFI string `xml:"BICFI"`
}

// camt057Dbtr renders the From party under the MT210 :50a:/:52a:
// rule: a registered BIC becomes Dbtr/Agt, otherwise the
// account ID (uppercased) becomes Dbtr/Pty/Nm.
func camt057Dbtr(fromID string, fromBIC bool) camt057xDbtr {
	if fromBIC {
		return camt057xDbtr{Agt: &camt057xAgt{FinInstnID: camt057xFinInstnID{BICFI: fromID}}}
	}
	return camt057xDbtr{Pty: &camt057xPty{Nm: fromID}}
}

// ExportCAMT057 renders the next run of the transfer schedule as
// an ISO 20022 camt.057.001.06 notification to receive. Unknown
// IDs report ErrScheduleNotFound; a paused, cancelled, completed,
// or exhausted schedule reports ErrCAMT057ScheduleNotActive. Only
// an active schedule with a next run exports.
func (l *Ledger) ExportCAMT057(opts CAMT057ExportOptions) (string, error) {
	// Caller-given options fail fast, before any schedule lookup:
	// a servicing bank that is not a valid BIC can never be a
	// valid notice.
	servicing := strings.ToUpper(strings.TrimSpace(opts.ServicingBIC))
	if servicing == "" || !validBIC(servicing) {
		return "", fmt.Errorf("%w: servicing BIC %q: want a valid 8/11-character BIC", ErrInvalidCAMT057Export, opts.ServicingBIC)
	}
	sched, err := l.GetTransferSchedule(opts.ScheduleID)
	if err != nil {
		return "", err
	}
	if sched.Status != ScheduleActive {
		return "", fmt.Errorf("%w: schedule %q is %s", ErrCAMT057ScheduleNotActive, opts.ScheduleID, sched.Status)
	}
	if !sched.EndsAt.IsZero() && sched.NextRunAt.After(sched.EndsAt) {
		return "", fmt.Errorf("%w: schedule %q next run %s is past its ends_at %s", ErrCAMT057ScheduleNotActive, opts.ScheduleID, sched.NextRunAt.UTC().Format(time.RFC3339), sched.EndsAt.UTC().Format(time.RFC3339))
	}
	// An FX schedule's receivable amount is only known when the
	// rate is looked up at fire time: a notice cannot state it.
	if sched.ToCurrency != "" {
		return "", fmt.Errorf("%w: schedule %q is an FX schedule (to_currency %q), the receivable amount is unknowable until fire time", ErrInvalidCAMT057Export, opts.ScheduleID, sched.ToCurrency)
	}
	// MsgId and the item ID are Max35Text.
	msgID := camt057MsgID(sched.ID, sched.RunSeq)
	if err := camt057Reference("MsgId", msgID); err != nil {
		return "", err
	}
	runID := camt057RunID(sched.ID, sched.RunSeq)
	if err := camt057Reference("Itm/Id", runID); err != nil {
		return "", err
	}
	// The parties' best-known identifiers (registered BIC, else
	// the internal account ID, uppercased) and the To account's
	// registered IBAN — read under the lock, like the MT300
	// identifier lookup.
	l.mu.RLock()
	toID, _ := l.mt300PartyIdentifierLocked(sched.To)
	fromID, fromBIC := l.mt300PartyIdentifierLocked(sched.From)
	toIBAN, toHasIBAN := l.accountIBANs[sched.To]
	l.mu.RUnlock()

	// Acct is the account to be credited: the registered IBAN
	// when one exists, else the To account ID. Either way
	// Max34Text (IBAN2007Identifier / GenericAccountIdentification
	// Id), so a longer identifier fails loud here.
	creditAccount := toID
	if toHasIBAN {
		creditAccount = toIBAN.IBAN
	}
	if len(creditAccount) > 34 {
		return "", fmt.Errorf("%w: Acct account %q exceeds 34 characters", ErrInvalidCAMT057Export, creditAccount)
	}
	acct := camt057xAcct{}
	if toHasIBAN {
		acct.ID.IBAN = creditAccount
	} else {
		acct.ID.Othr = &camt057xOthr{ID: creditAccount}
	}

	// The principal only: the per-run fee is a separate leg and
	// never reduces the amount the beneficiary is told to expect.
	amt := camt057xAmt{Ccy: sched.Currency, Value: camt053FormatAmount(sched.AmountCents)}
	valDt := sched.NextRunAt.UTC().Format("2006-01-02")
	dbtr := camt057Dbtr(fromID, fromBIC)

	doc := camt057xDocument{
		NtfctnToRcv: camt057xNtfctnToRcv{
			GrpHdr: camt057xGrpHdr{MsgID: msgID, CreDtTm: time.Now().UTC().Format(time.RFC3339)},
			Ntfctn: camt057xNtfctn{
				ID:         msgID,
				Acct:       acct,
				AcctSvcr:   camt057xAgt{FinInstnID: camt057xFinInstnID{BICFI: servicing}},
				TtlAmt:     amt,
				XpctdValDt: valDt,
				Dbtr:       dbtr,
				Itm: camt057xItm{
					ID:         runID,
					Amt:        amt,
					XpctdValDt: valDt,
					Dbtr:       dbtr,
				},
			},
		},
	}
	body, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidCAMT057Export, err)
	}
	return string(append([]byte(xml.Header), body...)), nil
}
