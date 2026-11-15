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

// ISO 20022 camt.057.001.07 Notification to Receive inbound
// processing (LG-420): the inbound half of the camt.057 lifecycle,
// the mirror of the LG-403 export.
//
// ExportCAMT057 renders this ledger's own pre-advice — the account
// owner telling its bank "money is coming into my account". This
// module consumes the pre-advice a counterparty account owner sent
// to this ledger, acting as the account bank: it registers the
// expected receipt so intraday liquidity can forecast it, and
// nothing else. The two directions are orthogonal by construction:
//
//   - LG-403 exports: the ledger tells its bank about the next run
//     of one of its own transfer schedules (outbound, read-only).
//   - LG-420 imports: a counterparty tells the ledger about money
//     it expects to send (inbound, state-changing only in the
//     expected-receipts registry: one forecast record per MsgId).
//
// ParseCAMT057 reuses the camt057.go wire structs, reading them in
// the reverse direction: the same `camt.057.001.07`
// `NotificationToReceive` document, one Ntfctn to start. The
// namespace rides the Document element, so a document in the wrong
// schema version fails the unmarshal — the version gate is the
// namespace, not a string check (the camt.056 inbound convention,
// LG-414).
//
// Association is the account, not a transfer: the Ntfctn/Acct names
// the account to be credited — a registered IBAN when the sender
// states one, else the Othr/Id account ID (the pacs009.go agent
// mapping: FinInstnId/Othr/Id carries the account ID, never a
// fabricated BIC). The account must already be known to the
// ledger — an account with journaled entries, or one with a
// registered IBAN. An unknown account is refused fail-closed and
// parked in the repair queue, never auto-created: the ledger never
// forecasts money into an account it never heard of.
//
// A registered notification is a forecast only, not a booking:
// ApplyCAMT057 writes no journal rows, bumps no version, moves no
// money, and opens no payout — the pre-advice is not an
// attestation, the LG-403 outbound read-only note in reverse. It
// counts as the fifth intraday-liquidity source (see liquidity.go):
// NotifiedInflowCents on the XpctdValDt UTC day, on the credited
// account, in the notified currency. It never books, so it can
// never double-count against a later pacs.008/pacs.009 receipt of
// the same money — the receipt books on arrival, the notification
// only ever forecasted it.
//
// Idempotency is the sender's GrpHdr MsgId, recorded in the
// per-ledger expected-receipts registry (in-memory only, like
// camt055Applied: the hash-chained
// `camt057_notification_received` audit events are the durable
// record). Re-applying the same document returns the stored
// receipt with Duplicate set, registering nothing new and emitting
// no second audit event. A MsgId reused for a different
// notification is refused with ErrCAMT057MsgIDConflict — one
// message ID names one notification.
//
// Fail-closed parking (the pacs.004 convention, LG-413): an
// unknown Acct parks in the repair queue for operator resolution,
// and the typed error carries the repair record ID. A document the
// ledger cannot even parse is parked the same way by
// ParkCAMT057Unparsed — visibility parking, never a silent drop
// of the counterparty's message. Repair records parked by this
// module are visibility parking, not resubmittable instructions:
// their Maker is deliberately empty, so the CreatePayout gate can
// never pass them.
//
// Audit: a fresh registration emits `camt057_notification_received`
// with the notification's identity, account, amount, currency, and
// expected value date, distinct from the export-side silence (the
// export emits no audit event at all) — the audit trail tells the
// inbound direction apart from an outbound pre-advice.

// camt.057 inbound errors.
var (
	// ErrInvalidCAMT057 is returned by ParseCAMT057 for malformed
	// XML or a document missing a required field: the counterparty
	// sent something the ledger cannot even read as a notification
	// to receive. The HTTP layer parks it in the repair queue and
	// maps it to 400.
	ErrInvalidCAMT057 = errors.New("ledger: invalid camt.057 notification to receive")
	// ErrCAMT057UnknownAccount is returned by ApplyCAMT057 when
	// the notification's Acct names no account the ledger knows:
	// the ledger never forecasts money into an account it never
	// heard of. The notification is parked in the repair queue,
	// not dropped.
	ErrCAMT057UnknownAccount = errors.New("ledger: camt.057 inbound: notification account is unknown")
	// ErrCAMT057MsgIDConflict is returned when a GrpHdr MsgId
	// already registered names a different notification (another
	// Ntfctn Id or amount): one message ID names one
	// notification. It is a 422-class error.
	ErrCAMT057MsgIDConflict = errors.New("ledger: camt.057 MsgId already registered for a different notification")
)

// ExpectedReceiptItem is one Itm of a registered camt.057
// notification: the item's own Id, amount, and expected value
// date. Items are informational — the notification-level TtlAmt is
// the forecasted amount; items are carried verbatim so the
// operator can see what the counterparty itemised.
type ExpectedReceiptItem struct {
	ID                string `json:"id"`
	AmountCents       int64  `json:"amount_cents"`
	Currency          string `json:"currency"`
	ExpectedValueDate string `json:"expected_value_date"`
}

// CAMT057Notification is one parsed inbound camt.057.001.07
// notification to receive: the notification's identity, the
// account to be credited (IBAN or Othr/Id, exactly as stated —
// resolution against the ledger happens in ApplyCAMT057), the
// amount, and the expected value date.
type CAMT057Notification struct {
	// MsgID is the GrpHdr MsgId: the audit trace and the replay
	// key. NotificationID is the Ntfctn Id, which repeats the
	// MsgId on documents this ledger's export renders — but an
	// inbound document may state a different one, so both are
	// carried.
	MsgID          string `json:"msg_id"`
	NotificationID string `json:"notification_id"`
	// AccountIBAN is Ntfctn/Acct/Id/IBAN when stated; AccountOthr
	// is Ntfctn/Acct/Id/Othr/Id when stated. Exactly one is set:
	// the export convention (registered IBAN, else account ID).
	AccountIBAN string `json:"account_iban,omitempty"`
	AccountOthr string `json:"account_othr,omitempty"`
	// AmountCents is the Ntfctn/TtlAmt in minor units; Currency is
	// its Ccy attribute, a 3-letter uppercase ISO 4217 code.
	AmountCents int64  `json:"amount_cents"`
	Currency    string `json:"currency"`
	// ExpectedValueDate is Ntfctn/XpctdValDt (YYYY-MM-DD), the day
	// the money is expected: the intraday-liquidity forecast day.
	ExpectedValueDate string `json:"expected_value_date"`
	// Debtor is the Ntfctn/Dbtr — the party name (Pty/Nm) or the
	// agent BIC (Agt/FinInstnId/BICFI) — carried verbatim.
	Debtor string `json:"debtor"`
	// Items are the Ntfctn/Itm entries, in document order.
	Items []ExpectedReceiptItem `json:"items,omitempty"`
}

// ExpectedReceipt is one registered camt.057 notification: the
// parsed notification plus the ledger's own registration — the
// resolved account, the applying actor, and the receipt time.
type ExpectedReceipt struct {
	MsgID             string                `json:"msg_id"`
	NotificationID    string                `json:"notification_id"`
	Account           AccountID             `json:"account"`
	AmountCents       int64                 `json:"amount_cents"`
	Currency          string                `json:"currency"`
	ExpectedValueDate string                `json:"expected_value_date"`
	Debtor            string                `json:"debtor"`
	Items             []ExpectedReceiptItem `json:"items,omitempty"`
	Actor             string                `json:"actor"`
	ReceivedAt        time.Time             `json:"received_at"`
	// Cancelled is set by ApplyCAMT058 (LG-421) when a
	// counterparty's camt.058 advice withdraws this
	// notification: the record is kept for audit, but the
	// forecast stops — a cancelled notification no longer counts
	// in intraday liquidity (see liquidity.go). CancelMsgID is
	// the advice's GrpHdr MsgId and CancelReason its verbatim
	// CxlRsn/Rsn/Prtry.
	Cancelled    bool      `json:"cancelled,omitempty"`
	CancelledAt  time.Time `json:"cancelled_at,omitempty"`
	CancelMsgID  string    `json:"cancel_msg_id,omitempty"`
	CancelReason string    `json:"cancel_reason,omitempty"`
}

// CAMT057ApplyResult is the outcome of ApplyCAMT057.
type CAMT057ApplyResult struct {
	// Receipt is the registered notification (or the stored one on
	// the duplicate path).
	Receipt ExpectedReceipt `json:"receipt"`
	// RepairID is set on the parked fail-closed path (unknown
	// account): the repair-queue record holding the notification
	// for operator resolution.
	RepairID string `json:"repair_id,omitempty"`
	// Duplicate reports whether the notification was already
	// registered under the same MsgId: a no-op success, not an
	// error.
	Duplicate bool `json:"duplicate,omitempty"`
}

// camt057ParseAmount parses an ISO 20022 decimal amount ("1234.56")
// into minor units, the pacs009ParseAmount rules: the integer part
// holds at most 12 digits, the fraction holds at most two digits
// and is right-padded ("5" means 50 cents), the value must be
// positive. Anything else is a parse refusal — a notified amount
// the ledger cannot read exactly is never rounded into a forecast.
func camt057ParseAmount(body string) (int64, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return 0, fmt.Errorf("%w: empty amount", ErrInvalidCAMT057)
	}
	intPart := body
	fracPart := ""
	if i := strings.IndexByte(body, '.'); i >= 0 {
		intPart, fracPart = body[:i], body[i+1:]
		if strings.IndexByte(fracPart, '.') >= 0 {
			return 0, fmt.Errorf("%w: amount %q has two decimal points", ErrInvalidCAMT057, body)
		}
	}
	if intPart == "" || len(intPart) > 12 || len(fracPart) > 2 {
		return 0, fmt.Errorf("%w: amount %q", ErrInvalidCAMT057, body)
	}
	for _, c := range intPart + fracPart {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("%w: amount %q is not numeric", ErrInvalidCAMT057, body)
		}
	}
	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: amount %q", ErrInvalidCAMT057, body)
	}
	frac := int64(0)
	if fracPart != "" {
		if len(fracPart) == 1 {
			fracPart += "0"
		}
		frac, err = strconv.ParseInt(fracPart, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%w: amount %q", ErrInvalidCAMT057, body)
		}
	}
	cents := whole*100 + frac
	if cents <= 0 {
		return 0, fmt.Errorf("%w: amount %q is not positive", ErrInvalidCAMT057, body)
	}
	return cents, nil
}

// ParseCAMT057 parses one inbound camt.057.001.07 notification to
// receive into a CAMT057Notification, reusing the camt057.go wire
// structs in the reverse direction. It validates the whole document
// before returning anything: a missing MsgId, a missing Ntfctn Id,
// an Acct with neither IBAN nor Othr/Id, an unreadable or
// non-positive TtlAmt, an empty or malformed currency, a missing or
// malformed XpctdValDt, a missing Dbtr, or a malformed Itm are all
// parse refusals — ApplyCAMT057 never sees a half-read
// notification. The AcctSvcr is read but not gated: the servicing
// bank is this ledger's side of the notice, so a document that
// misnames it cannot misdirect the registration.
func ParseCAMT057(data []byte) (CAMT057Notification, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return CAMT057Notification{}, fmt.Errorf("%w: empty document", ErrInvalidCAMT057)
	}
	var doc camt057xDocument
	if err := xml.Unmarshal(data, &doc); err != nil {
		return CAMT057Notification{}, fmt.Errorf("%w: %v", ErrInvalidCAMT057, err)
	}
	grp := doc.NtfctnToRcv.GrpHdr
	ntfctn := doc.NtfctnToRcv.Ntfctn
	msgID := strings.TrimSpace(grp.MsgID)
	if msgID == "" {
		return CAMT057Notification{}, fmt.Errorf("%w: missing GrpHdr/MsgId", ErrInvalidCAMT057)
	}
	ntfctnID := strings.TrimSpace(ntfctn.ID)
	if ntfctnID == "" {
		return CAMT057Notification{}, fmt.Errorf("%w: missing Ntfctn/Id", ErrInvalidCAMT057)
	}
	iban := strings.TrimSpace(ntfctn.Acct.ID.IBAN)
	othr := ""
	if ntfctn.Acct.ID.Othr != nil {
		othr = strings.TrimSpace(ntfctn.Acct.ID.Othr.ID)
	}
	if iban == "" && othr == "" {
		return CAMT057Notification{}, fmt.Errorf("%w: missing Ntfctn/Acct/Id (neither IBAN nor Othr/Id)", ErrInvalidCAMT057)
	}
	amount, err := camt057ParseAmount(ntfctn.TtlAmt.Value)
	if err != nil {
		return CAMT057Notification{}, err
	}
	if strings.TrimSpace(ntfctn.TtlAmt.Ccy) == "" {
		return CAMT057Notification{}, fmt.Errorf("%w: empty notification amount currency", ErrInvalidCAMT057)
	}
	currency, err := normalizeCurrency(ntfctn.TtlAmt.Ccy)
	if err != nil {
		return CAMT057Notification{}, fmt.Errorf("%w: currency %q", ErrInvalidCAMT057, ntfctn.TtlAmt.Ccy)
	}
	valueDate := strings.TrimSpace(ntfctn.XpctdValDt)
	if valueDate == "" {
		return CAMT057Notification{}, fmt.Errorf("%w: missing Ntfctn/XpctdValDt", ErrInvalidCAMT057)
	}
	if _, err := time.Parse("2006-01-02", valueDate); err != nil {
		return CAMT057Notification{}, fmt.Errorf("%w: expected value date %q must be YYYY-MM-DD", ErrInvalidCAMT057, valueDate)
	}
	debtor := ""
	if ntfctn.Dbtr.Pty != nil {
		debtor = strings.TrimSpace(ntfctn.Dbtr.Pty.Nm)
	}
	if debtor == "" && ntfctn.Dbtr.Agt != nil {
		debtor = strings.TrimSpace(ntfctn.Dbtr.Agt.FinInstnId.BICFI)
	}
	if debtor == "" {
		return CAMT057Notification{}, fmt.Errorf("%w: missing Ntfctn/Dbtr", ErrInvalidCAMT057)
	}
	items := make([]ExpectedReceiptItem, 0, len(ntfctn.Itm))
	for _, itm := range ntfctn.Itm {
		itemID := strings.TrimSpace(itm.ID)
		if itemID == "" {
			return CAMT057Notification{}, fmt.Errorf("%w: notification item without Id", ErrInvalidCAMT057)
		}
		itemAmount, err := camt057ParseAmount(itm.Amt.Value)
		if err != nil {
			return CAMT057Notification{}, err
		}
		itemCurrency := currency
		if ccy := strings.TrimSpace(itm.Amt.Ccy); ccy != "" {
			itemCurrency, err = normalizeCurrency(ccy)
			if err != nil {
				return CAMT057Notification{}, fmt.Errorf("%w: item currency %q", ErrInvalidCAMT057, ccy)
			}
		}
		itemDate := strings.TrimSpace(itm.XpctdValDt)
		if itemDate == "" {
			return CAMT057Notification{}, fmt.Errorf("%w: notification item %q without XpctdValDt", ErrInvalidCAMT057, itemID)
		}
		if _, err := time.Parse("2006-01-02", itemDate); err != nil {
			return CAMT057Notification{}, fmt.Errorf("%w: item %q expected value date %q must be YYYY-MM-DD", ErrInvalidCAMT057, itemID, itemDate)
		}
		items = append(items, ExpectedReceiptItem{
			ID:                itemID,
			AmountCents:       itemAmount,
			Currency:          itemCurrency,
			ExpectedValueDate: itemDate,
		})
	}
	return CAMT057Notification{
		MsgID:             msgID,
		NotificationID:    ntfctnID,
		AccountIBAN:       iban,
		AccountOthr:       othr,
		AmountCents:       amount,
		Currency:          currency,
		ExpectedValueDate: valueDate,
		Debtor:            debtor,
		Items:             items,
	}, nil
}

// resolveCAMT057AccountLocked resolves a parsed notification's Acct
// against the ledger: a stated IBAN walks the registered-IBAN
// registry (normalized, the SetAccountIBAN canonical form), else
// the Othr/Id is the account ID directly. The account must already
// be known — journaled entries, or a registered IBAN. Callers must
// hold l.mu (either lock suffices).
func (l *Ledger) resolveCAMT057AccountLocked(n CAMT057Notification) (AccountID, error) {
	if iban := strings.TrimSpace(n.AccountIBAN); iban != "" {
		norm := normalizeIBAN(iban)
		for acct, reg := range l.accountIBANs {
			if reg.IBAN == norm {
				return acct, nil
			}
		}
		return "", fmt.Errorf("%w: IBAN %q", ErrCAMT057UnknownAccount, iban)
	}
	acct := AccountID(strings.TrimSpace(n.AccountOthr))
	if len(l.byAccount[acct]) == 0 {
		if _, ok := l.accountIBANs[acct]; !ok {
			return "", fmt.Errorf("%w: %q", ErrCAMT057UnknownAccount, acct)
		}
	}
	return acct, nil
}

// camt057LenientDocument is the namespace-agnostic wire shape
// ParkCAMT057Unparsed reads: a document ParseCAMT057 refused for
// its version may still name the notification, and the namespace
// is the only thing the strict parse gates on. Unknown elements
// are skipped, the camt.029 convention.
type camt057LenientDocument struct {
	XMLName     xml.Name             `xml:"Document"`
	NtfctnToRcv camt057LenientNtfctn `xml:"NtfctnToRcv"`
}

type camt057LenientNtfctn struct {
	GrpHdr camt057LenientGrpHdr      `xml:"GrpHdr"`
	Ntfctn camt057LenientNtfctnInner `xml:"Ntfctn"`
}

type camt057LenientGrpHdr struct {
	MsgID string `xml:"MsgId"`
}

type camt057LenientNtfctnInner struct {
	ID string `xml:"Id"`
}

// ParkCAMT057Unparsed best-effort parks a camt.057 document
// ParseCAMT057 refused, when the document still names a
// notification: the GrpHdr MsgId or the Ntfctn Id it carries. It
// returns the repair record ID, or "" when the document names
// nothing parkable (garbage bytes, an empty shell): there is no
// notification for the operator to find, so there is nothing to
// park. A parking failure (a full queue) is returned so the caller
// can fail loud rather than silently drop the counterparty's
// message. The reason code is invalid_format — the document was
// present but unreadable as a notification.
func (l *Ledger) ParkCAMT057Unparsed(data []byte, actor string) (string, error) {
	var doc camt057LenientDocument
	if err := xml.Unmarshal(data, &doc); err != nil {
		return "", nil
	}
	id := strings.TrimSpace(doc.NtfctnToRcv.GrpHdr.MsgID)
	if id == "" {
		id = strings.TrimSpace(doc.NtfctnToRcv.Ntfctn.ID)
	}
	if id == "" {
		return "", nil
	}
	rec, err := l.EnqueueRepair(PayoutInstruction{
		ID: id,
		// Maker intentionally empty: see the package comment.
		Destination: "camt057-inbound:" + id,
	}, RepairReasonInvalidFormat, actor)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// parkCAMT057Notification parks a parsed camt.057 notification that
// failed closed, and returns the repair record ID. The snapshot's
// Maker is deliberately empty (see the package comment).
func (l *Ledger) parkCAMT057Notification(n CAMT057Notification, reasonCode, actor string) (string, error) {
	rec, err := l.EnqueueRepair(PayoutInstruction{
		ID:          n.MsgID,
		AmountCents: n.AmountCents,
		Currency:    n.Currency,
		Destination: "camt057-inbound:" + n.MsgID,
		// Maker intentionally empty: see the package comment.
	}, reasonCode, actor)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// ApplyCAMT057 registers one parsed inbound camt.057.001.07
// notification to receive as an expected receipt: the inbound half
// of the camt.057 flow, the counterparty-side mirror of the
// outbound export. The notification's Acct must name an account
// the ledger already knows — a registered IBAN resolves through
// the IBAN registry, an Othr/Id names the account directly —
// otherwise the notification is refused fail-closed and parked in
// the repair queue.
//
// Only the forecast registers: no journal rows, no version bump,
// no money moved. The registered receipt counts as the fifth
// intraday-liquidity source on its expected value date (see
// liquidity.go) and nowhere else.
//
// Idempotency is the GrpHdr MsgId: re-applying a MsgId already
// registered returns the stored receipt with Duplicate set, no
// second audit event. A MsgId reused for a different notification
// is refused with ErrCAMT057MsgIDConflict.
//
// Locking: the duplicate read and the account resolution run under
// the read lock; the registration section holds the write lock (the
// re-checks, the registry write, and the audit event are one
// atomic section); the repair parking runs outside any held lock
// because EnqueueRepair takes l.mu itself.
func (l *Ledger) ApplyCAMT057(n CAMT057Notification, actor string) (CAMT057ApplyResult, error) {
	if strings.TrimSpace(n.MsgID) == "" {
		return CAMT057ApplyResult{}, fmt.Errorf("%w: missing GrpHdr/MsgId", ErrInvalidCAMT057)
	}
	if actor == "" {
		actor = "ApplyCAMT057"
	}

	// Duplicate read and account resolution: the same MsgId
	// registered before is a no-op success, whatever the ledger's
	// current state.
	l.mu.RLock()
	stored, dup := l.expectedReceipts[n.MsgID]
	acct, resolveErr := l.resolveCAMT057AccountLocked(n)
	l.mu.RUnlock()
	if dup {
		// One MsgId names one notification: a different
		// Ntfctn Id, amount, or currency under it is a
		// conflict. The account is compared only when it
		// resolves — a failed resolution says nothing about
		// the stored receipt, which resolved when it was
		// registered.
		if stored.NotificationID != n.NotificationID || stored.AmountCents != n.AmountCents || stored.Currency != n.Currency {
			return CAMT057ApplyResult{}, fmt.Errorf("%w: MsgId %q", ErrCAMT057MsgIDConflict, n.MsgID)
		}
		if resolveErr == nil && stored.Account != acct {
			return CAMT057ApplyResult{}, fmt.Errorf("%w: MsgId %q", ErrCAMT057MsgIDConflict, n.MsgID)
		}
		return CAMT057ApplyResult{Receipt: stored, Duplicate: true}, nil
	}
	if resolveErr != nil {
		// The parked fail-closed path, outside any held lock: an
		// unknown account goes to the repair queue.
		repairID, err := l.parkCAMT057Notification(n, RepairReasonUnknownRoute, actor)
		if err != nil {
			return CAMT057ApplyResult{}, fmt.Errorf("%w (repair parking failed: %v)", resolveErr, err)
		}
		return CAMT057ApplyResult{RepairID: repairID}, resolveErr
	}

	// Registration section: re-check the duplicate under the write
	// lock — a racing apply for the same MsgId converges here
	// instead of double-registering.
	l.mu.Lock()
	defer l.mu.Unlock()
	if stored, dup := l.expectedReceipts[n.MsgID]; dup {
		if stored.NotificationID != n.NotificationID || stored.AmountCents != n.AmountCents || stored.Currency != n.Currency || stored.Account != acct {
			return CAMT057ApplyResult{}, fmt.Errorf("%w: MsgId %q", ErrCAMT057MsgIDConflict, n.MsgID)
		}
		return CAMT057ApplyResult{Receipt: stored, Duplicate: true}, nil
	}
	receipt := ExpectedReceipt{
		MsgID:             n.MsgID,
		NotificationID:    n.NotificationID,
		Account:           acct,
		AmountCents:       n.AmountCents,
		Currency:          n.Currency,
		ExpectedValueDate: n.ExpectedValueDate,
		Debtor:            n.Debtor,
		Items:             append([]ExpectedReceiptItem(nil), n.Items...),
		Actor:             actor,
		ReceivedAt:        time.Now().UTC(),
	}
	if l.expectedReceipts == nil {
		l.expectedReceipts = make(map[string]ExpectedReceipt)
	}
	l.expectedReceipts[n.MsgID] = receipt
	l.emitAudit(AuditEvent{
		Op:       "camt057_notification_received",
		Actor:    actor,
		TraceID:  n.MsgID,
		Accounts: []AccountID{acct},
		Details: map[string]any{
			"msg_id":              n.MsgID,
			"notification_id":     n.NotificationID,
			"acct":                string(acct),
			"amount_cents":        n.AmountCents,
			"currency":            n.Currency,
			"expected_value_date": n.ExpectedValueDate,
			"debtor":              n.Debtor,
		},
	})
	return CAMT057ApplyResult{Receipt: receipt}, nil
}

// GetExpectedReceipt returns the expected receipt first registered
// under msgID, or false when no notification was ever registered
// under it. The returned receipt is a copy — callers cannot mutate
// ledger state through it.
func (l *Ledger) GetExpectedReceipt(msgID string) (ExpectedReceipt, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	r, ok := l.expectedReceipts[msgID]
	if !ok {
		return ExpectedReceipt{}, false
	}
	r.Items = append([]ExpectedReceiptItem(nil), r.Items...)
	return r, true
}
