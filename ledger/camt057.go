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
// This is the MX twin of the SWIFT FIN MT210 (LG-393): the same
// pre-advice — "there will be money coming into my account" —
// sent by the account owner to its account bank, in ISO 20022
// syntax instead of FIN. Same source (the next run of one transfer
// schedule, LG-41), same direction (To account, the beneficiary,
// to the servicing bank named by the caller), same limits (one
// run only, fees never netted, FX schedules never notified). It
// is not a camt.054: that one is the bank telling the customer
// about money that already moved; this one is the customer
// telling the bank about money that is about to arrive.
//
// Version pinned by the spec's registry check (LG-403): the ISO
// 20022 registry's latest camt.057 is .001.07 — the
// registry-generated Go binding (UNO-SOFT/iso20022, regenerated
// 2026-09-06 from the registry) carries camt_057_001_07 and no
// .001.06 package, and the registry itself urges using the most
// recent version. The CBPR+ usage guidelines freeze .001.06 for
// the CBPR+ community, but this export follows the registry. The
// structure below is verified against that binding's
// NotificationToReceiveV07: Document > NtfctnToRcv > GrpHdr
// (GroupHeader77: MsgId / CreDtTm) + Ntfctn
// (AccountNotification18: Id / Acct / AcctSvcr / TtlAmt /
// XpctdValDt / Dbtr / Itm) + one Itm (NotificationItem8: Id /
// Amt / XpctdValDt). The spec's shorthand "Amt" is the
// notification-level TtlAmt and the item-level Amt — both carry
// the same principal.
//
//   - Read-only: the lookup takes the read lock only (via
//     GetTransferSchedule and the identifier lookup), bumps no
//     version, books nothing, and appends no audit event. A
//     pre-advice is not an attestation.
//   - Acct is the account to be credited: the To account's
//     registered IBAN when one exists, else the account ID under
//     Othr/Id — the MT210 :25: rule, in MX form.
//   - AcctSvcr names the servicing bank: the caller-given
//     ServicingBIC, the notice's receiver. The ledger holds no
//     bank-identity registry, so the caller names it on every
//     export.
//   - Dbtr is the From party, the MT210 :50a:/:52a: either-or:
//     the From account's registered BIC as an agent (Agt /
//     FinInstnId / BICFI) when one exists, else the account ID
//     as a party name (Pty / Nm).
//   - TtlAmt and Itm/Amt are the principal only. A per-run fee
//     (FeeCents) is a separate fee leg — the run books it as its
//     own entry — so it never reduces the amount the beneficiary
//     is told to expect. An FX schedule (ToCurrency set) cannot
//     be notified at all: the amount to be received is only
//     known when the rate is looked up at fire time (see
//     schedule.go), so a notice would state a number the ledger
//     cannot stand behind; it fails loud with
//     ErrInvalidCAMT057Export instead of guessing.
//   - MsgId is deterministic: "camt057-<scheduleID>-<runSeq>",
//     the MT210 :21: run-transfer-ID logic in MX form. The
//     notification Id repeats it, and the single Itm carries the
//     run transfer ID "<ID>/run/<seq>". Repeat exports are
//     byte-identical modulo the honest wall-clock CreDtTm line
//     (the camt.060 precedent) until a run fires — then RunSeq
//     advances and MsgId / XpctdValDt move with it, which is a
//     content change, not drift.
//   - Only an active schedule notifies: a paused schedule's next
//     run will not fire on its cadence, so a notice would
//     mislead liquidity planning, and cancelled/completed
//     schedules have no next run at all — all three report
//     ErrCAMT057ScheduleNotActive, as does a schedule whose
//     NextRunAt has already passed its EndsAt (no run left to
//     notify).

// CAMT.057 export errors.
var (
	// ErrInvalidCAMT057Export is returned when a schedule cannot be
	// rendered as a camt.057: a servicing BIC that is not a valid
	// 8/11 BIC, an FX schedule whose receivable amount is
	// unknowable until fire time, a message/item/account
	// identifier outside Max35Text, or a non-positive principal.
	// It is a 400-class error: the stored schedule cannot be
	// expressed as a valid notice.
	ErrInvalidCAMT057Export = errors.New("ledger: invalid camt.057 export")
	// ErrCAMT057ScheduleNotActive is returned when a camt.057 is
	// requested for a schedule that has no next run to notify:
	// paused, cancelled, completed, or past its EndsAt. It is a
	// 422-class error: the schedule exists, but there is nothing
	// to announce.
	ErrCAMT057ScheduleNotActive = errors.New("ledger: transfer schedule is not active for camt.057 notice")
)

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
// from the schedule ID and the run sequence: the same run
// re-renders the same ID, and any run advance changes it.
func camt057MsgID(scheduleID string, runSeq uint64) string {
	return fmt.Sprintf("camt057-%s-%d", scheduleID, runSeq)
}

// The camt057x* structs are the export-side XML model, the
// camt060.go convention: a strict writer carrying the
// camt.057.001.07 namespace on the root and omitting every empty
// element, so the emitted document is what a bank expects to
// receive.
type camt057xDocument struct {
	XMLName     xml.Name            `xml:"urn:iso:std:iso:20022:tech:xsd:camt.057.001.07 Document"`
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
	ID         string           `xml:"Id"`
	Acct       camt057xAcct     `xml:"Acct"`
	AcctSvcr   camt057xAcctSvcr `xml:"AcctSvcr"`
	TtlAmt     camt057xAmt      `xml:"TtlAmt"`
	XpctdValDt string           `xml:"XpctdValDt"`
	Dbtr       camt057xDbtr     `xml:"Dbtr"`
	Itm        []camt057xItm    `xml:"Itm"`
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

type camt057xAcctSvcr struct {
	FinInstnId camt057xFinInstnID `xml:"FinInstnId"`
}

type camt057xFinInstnID struct {
	BICFI string `xml:"BICFI"`
}

type camt057xAmt struct {
	Ccy   string `xml:"Ccy,attr"`
	Value string `xml:",chardata"`
}

type camt057xDbtr struct {
	Pty *camt057xPty `xml:"Pty,omitempty"`
	Agt *camt057xAgt `xml:"Agt,omitempty"`
}

type camt057xPty struct {
	Nm string `xml:"Nm"`
}

type camt057xAgt struct {
	FinInstnId camt057xFinInstnID `xml:"FinInstnId"`
}

type camt057xItm struct {
	ID         string      `xml:"Id"`
	Amt        camt057xAmt `xml:"Amt"`
	XpctdValDt string      `xml:"XpctdValDt"`
}

// ExportCAMT057 renders the next run of the transfer schedule as
// an ISO 20022 camt.057.001.07 notification to receive. Unknown
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
	// MsgId / Ntfctn Id / Itm Id are all Max35Text: a long
	// schedule ID fails loud here rather than emitting a
	// non-conformant document.
	msgID := camt057MsgID(sched.ID, sched.RunSeq)
	if len(msgID) > 35 {
		return "", fmt.Errorf("%w: message ID %q exceeds Max35Text", ErrInvalidCAMT057Export, msgID)
	}
	runID := fmt.Sprintf("%s/run/%d", sched.ID, sched.RunSeq)
	if len(runID) > 35 {
		return "", fmt.Errorf("%w: item ID %q exceeds Max35Text", ErrInvalidCAMT057Export, runID)
	}
	if sched.AmountCents <= 0 {
		return "", fmt.Errorf("%w: schedule %q amount %d is not a positive principal", ErrInvalidCAMT057Export, opts.ScheduleID, sched.AmountCents)
	}
	if strings.TrimSpace(sched.Currency) == "" {
		return "", fmt.Errorf("%w: schedule %q has no currency", ErrInvalidCAMT057Export, opts.ScheduleID)
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
	// Max35Text.
	creditAccount := toID
	if toHasIBAN {
		creditAccount = toIBAN.IBAN
	}
	if creditAccount == "" || len(creditAccount) > 35 {
		return "", fmt.Errorf("%w: credit account %q is outside Max35Text", ErrInvalidCAMT057Export, creditAccount)
	}

	acct := camt057xAcct{}
	if toHasIBAN {
		acct.ID.IBAN = toIBAN.IBAN
	} else {
		acct.ID.Othr = &camt057xOthr{ID: toID}
	}
	// Dbtr is the From party, either-or: the registered BIC as
	// an agent, else the account ID as a party name.
	var dbtr camt057xDbtr
	if fromBIC {
		dbtr.Agt = &camt057xAgt{FinInstnId: camt057xFinInstnID{BICFI: fromID}}
	} else {
		dbtr.Pty = &camt057xPty{Nm: fromID}
	}
	amount := camt057xAmt{Ccy: sched.Currency, Value: camt053FormatAmount(sched.AmountCents)}
	valueDate := sched.NextRunAt.UTC().Format("2006-01-02")

	doc := camt057xDocument{
		NtfctnToRcv: camt057xNtfctnToRcv{
			GrpHdr: camt057xGrpHdr{MsgID: msgID, CreDtTm: time.Now().UTC().Format(time.RFC3339)},
			Ntfctn: camt057xNtfctn{
				ID:         msgID,
				Acct:       acct,
				AcctSvcr:   camt057xAcctSvcr{FinInstnId: camt057xFinInstnID{BICFI: servicing}},
				TtlAmt:     amount,
				XpctdValDt: valueDate,
				Dbtr:       dbtr,
				Itm: []camt057xItm{
					{ID: runID, Amt: amount, XpctdValDt: valueDate},
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
