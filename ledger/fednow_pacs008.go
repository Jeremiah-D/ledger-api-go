package ledger

import (
	"encoding/xml"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// FedNow pacs.008.001.08 inbound customer credit transfer notification
// (LG-163, fintech).
//
// The ledger acts as the creditor agent's (receiver bank's) book: the
// bank forwards each inbound instant credit transfer as a
// pacs.008.001.08 FIToFICstmrCdtTrf notification. ProcessPacs008 parses
// the document and, per CdtTrfTxInf, associates / validates / books:
//
//  1. Association: the transaction's EndToEndId is looked up against
//     the original payout/transfer idempotency key — the ledger's
//     transfer principal entry (ID == transfer ID, the pain.001
//     EndToEndId convention), a transfer's idempotency key, a payout
//     instruction (ID), or a payout's idempotency key. The association
//     is recorded on the result; the posting itself is idempotent on
//     the EndToEndId-derived entry ID, so replaying the same
//     notification never double-books.
//  2. Validation: amount > 0, currency a valid ISO 4217 code, payee
//     (creditor) account non-empty. When an original is associated,
//     the amount and currency must equal the original's and the payee
//     must be one of the original's accounts — otherwise the
//     notification is evidence about a different instruction than the
//     one it claims to be, and it is explicitly rejected. With no
//     association, the payee must already be a known ledger account
//     (fail-closed: the ledger never credits an account it never heard
//     of).
//  3. Booking: debit payee (inflow — the camt.053 direction convention:
//     an inflow is a debit to the asset account) / credit the FedNow
//     settlement account (fail-closed config, see
//     SetFedNowSettlementAccount), through the full Post gate, so
//     frozen/overdraft/daily-limit/period/denylist controls all apply.
//
// Illegal documents and per-item validation failures are explicitly
// rejected: a malformed document fails the whole parse (no partial
// application), while per-item rejections are recorded in the result
// and in the hash-chained audit log (fednow_pacs008_rejected) without
// blocking the other items.

// FedNow pacs.008 errors.
var (
	// ErrInvalidPacs008 is returned when the document is not a readable
	// pacs.008.001.08 notification: empty input, malformed XML, no
	// FIToFICstmrCdtTrf/CdtTrfTxInf blocks, a transaction missing its
	// EndToEndId, amount, currency, or creditor account, or an
	// unparseable amount. The HTTP layer maps it to 400.
	ErrInvalidPacs008 = errors.New("ledger: invalid pacs.008 document")
	// ErrFedNowAmountMismatch is returned when a notification names an
	// original transfer/payout but carries a different amount: it is
	// evidence about a different instruction, so it must not book
	// against it. Recorded per-item.
	ErrFedNowAmountMismatch = errors.New("ledger: pacs.008 amount does not match the original instruction")
	// ErrFedNowCurrencyMismatch is returned when a notification names
	// an original transfer/payout but carries a different currency.
	// Recorded per-item.
	ErrFedNowCurrencyMismatch = errors.New("ledger: pacs.008 currency does not match the original instruction")
	// ErrFedNowPayeeMismatch is returned when a notification names an
	// original transfer/payout but the creditor account is none of the
	// original's accounts. Recorded per-item.
	ErrFedNowPayeeMismatch = errors.New("ledger: pacs.008 creditor is not an account of the original instruction")
	// ErrFedNowUnknownPayeeAccount is returned when a notification
	// names no original instruction and its creditor account is unknown
	// to the ledger: the ledger never credits an account it never heard
	// of. Recorded per-item.
	ErrFedNowUnknownPayeeAccount = errors.New("ledger: pacs.008 creditor account is unknown")
	// ErrFedNowSettlementAccountNotConfigured is returned when a
	// notification would book but no FedNow settlement account is
	// configured: the counterparty leg has nowhere to go, so the item
	// is rejected fail-closed instead of booking a half-leg.
	ErrFedNowSettlementAccountNotConfigured = errors.New("ledger: FedNow settlement account not configured")
)

// Pacs008AssociationKind names what the notification's EndToEndId
// resolved to: a transfer, a payout, or nothing the ledger knows.
type Pacs008AssociationKind string

const (
	// Pacs008AssociatedTransfer: the EndToEndId names a journaled
	// transfer (by transfer ID or by idempotency key).
	Pacs008AssociatedTransfer Pacs008AssociationKind = "transfer"
	// Pacs008AssociatedPayout: the EndToEndId names a payout
	// instruction (by payout ID or by idempotency key).
	Pacs008AssociatedPayout Pacs008AssociationKind = "payout"
	// Pacs008AssociatedNone: the EndToEndId names nothing the ledger
	// knows — an ad-hoc inbound payment, still booked after payee
	// validation.
	Pacs008AssociatedNone Pacs008AssociationKind = "none"
)

// Pacs008Association is the EndToEndId resolution: the kind of
// original instruction, its ID, and the amount/currency/accounts the
// notification is validated against.
type Pacs008Association struct {
	Kind       Pacs008AssociationKind `json:"kind"`
	ID         string                 `json:"id,omitempty"`
	AmountCents int64                 `json:"amount_cents,omitempty"`
	Currency   string                 `json:"currency,omitempty"`
	Accounts   []AccountID            `json:"accounts,omitempty"`
}

// Pacs008Transaction is one parsed CdtTrfTxInf: an inbound instant
// credit transfer notification. AmountCents is the absolute value in
// cents. CreditorAccount is the payee — the account being credited.
type Pacs008Transaction struct {
	MsgID           string    `json:"msg_id"`
	EndToEndID      string    `json:"end_to_end_id"`
	TxID            string    `json:"tx_id,omitempty"`
	AmountCents     int64     `json:"amount_cents"`
	Currency        string    `json:"currency"`
	DebtorAccount   string    `json:"debtor_account,omitempty"`
	DebtorName      string    `json:"debtor_name,omitempty"`
	CreditorAccount AccountID `json:"creditor_account"`
	CreditorName    string    `json:"creditor_name,omitempty"`
	Remittance      string    `json:"remittance,omitempty"`
}

// Pacs008Result is the per-item outcome of ProcessPacs008: a
// notification is either booked (EntryID set), an idempotent replay
// (Duplicate true), or rejected (Err set). One item's failure never
// blocks the others.
type Pacs008Result struct {
	// Transaction is the parsed notification this result is about.
	Transaction Pacs008Transaction `json:"transaction"`
	// Association is the EndToEndId resolution against the original
	// payout/transfer idempotency key.
	Association Pacs008Association `json:"association"`
	// EntryID is the booked receipt entry's ID
	// ("fednow-pacs008/<endtoendid>"), empty when rejected.
	EntryID string `json:"entry_id,omitempty"`
	// Duplicate reports an idempotent replay: the same notification was
	// processed before, so the earlier entry is returned and nothing
	// new was booked — no new audit event, no metric bump.
	Duplicate bool `json:"duplicate"`
	// Err is the per-item rejection reason, nil on success.
	Err error `json:"-"`
}

// The pacs008* structs model the ISO 20022 pacs.008.001.08
// FIToFICstmrCdtTrf shape. The struct tags name bare local element
// names, so encoding/xml matches them in any namespace — the FedNow
// network ships the default
// `urn:iso:std:iso:20022:tech:xsd:pacs.008.001.08` namespace, and a
// tolerant reader does not care about the exact suffix. Unknown
// elements are skipped, which is what makes the parser tolerant of the
// optional blocks (SttlmInf, ChrgsInf, …) real notifications carry.
type pacs008Document struct {
	XMLName xml.Name      `xml:"Document"`
	Trf     pacs008FIToFI `xml:"FIToFICstmrCdtTrf"`
}

type pacs008FIToFI struct {
	GrpHdr pacs008GrpHdr  `xml:"GrpHdr"`
	Txs    []pacs008TxInf `xml:"CdtTrfTxInf"`
}

type pacs008GrpHdr struct {
	MsgID   string `xml:"MsgId"`
	CreDtTm string `xml:"CreDtTm"`
	NbOfTxs string `xml:"NbOfTxs"`
}

type pacs008TxInf struct {
	PmtID    pacs008PmtID `xml:"PmtId"`
	Amt      pacs008Amt   `xml:"IntrBkSttlmAmt"`
	Dbtr     pacs008Party `xml:"Dbtr"`
	DbtrAcct pacs008Acct  `xml:"DbtrAcct"`
	Cdtr     pacs008Party `xml:"Cdtr"`
	CdtrAcct pacs008Acct  `xml:"CdtrAcct"`
	RmtInf   pacs008Rmt   `xml:"RmtInf"`
}

type pacs008PmtID struct {
	EndToEndID string `xml:"EndToEndId"`
	TxID       string `xml:"TxId"`
}

type pacs008Amt struct {
	Ccy   string `xml:"Ccy,attr"`
	Value string `xml:",chardata"`
}

type pacs008Party struct {
	Name string `xml:"Nm"`
}

type pacs008Acct struct {
	ID pacs008AcctID `xml:"Id"`
}

type pacs008AcctID struct {
	IBAN string         `xml:"IBAN"`
	Othr pacs008OthrAcct `xml:"Othr"`
}

type pacs008OthrAcct struct {
	ID string `xml:"Id"`
}

type pacs008Rmt struct {
	Ustrd []string `xml:"Ustrd"`
}

// pacs008AccountIDOf resolves a party account identifier: the IBAN when
// present, else the Other/Id. Empty when the account names nothing.
func pacs008AccountIDOf(a pacs008Acct) string {
	if id := strings.TrimSpace(a.ID.IBAN); id != "" {
		return id
	}
	return strings.TrimSpace(a.ID.Othr.ID)
}

// ParsePacs008 parses a FedNow pacs.008.001.08 inbound customer credit
// transfer notification into its transactions. It is
// namespace-tolerant and skips the optional blocks real notifications
// carry.
//
// Per CdtTrfTxInf it collects PmtId (EndToEndId required, TxId
// optional), IntrBkSttlmAmt (+ Ccy), the debtor/creditor account
// identifiers (IBAN, else Other/Id), party names, and remittance text.
// A transaction missing its EndToEndId, amount, currency, or creditor
// account — or carrying a value that does not parse — makes the whole
// document invalid: a notification the reader cannot fully understand
// must not half-apply. A document with no transactions at all is
// likewise invalid.
func ParsePacs008(data []byte) ([]Pacs008Transaction, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, fmt.Errorf("%w: empty document", ErrInvalidPacs008)
	}
	var doc pacs008Document
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidPacs008, err)
	}
	if len(doc.Trf.Txs) == 0 {
		return nil, fmt.Errorf("%w: no CdtTrfTxInf blocks found", ErrInvalidPacs008)
	}
	msgID := strings.TrimSpace(doc.Trf.GrpHdr.MsgID)
	out := make([]Pacs008Transaction, 0, len(doc.Trf.Txs))
	for i, tx := range doc.Trf.Txs {
		e2e := strings.TrimSpace(tx.PmtID.EndToEndID)
		if e2e == "" {
			return nil, fmt.Errorf("%w: CdtTrfTxInf[%d] missing EndToEndId", ErrInvalidPacs008, i)
		}
		amount, err := parsePacs008Amount(tx.Amt.Value)
		if err != nil {
			return nil, fmt.Errorf("%w: CdtTrfTxInf[%d] (%q): %v", ErrInvalidPacs008, i, e2e, err)
		}
		if amount <= 0 {
			return nil, fmt.Errorf("%w: CdtTrfTxInf[%d] (%q): non-positive amount %d", ErrInvalidPacs008, i, e2e, amount)
		}
		ccy := strings.TrimSpace(tx.Amt.Ccy)
		if ccy == "" {
			return nil, fmt.Errorf("%w: CdtTrfTxInf[%d] (%q): missing Ccy", ErrInvalidPacs008, i, e2e)
		}
		currency, err := normalizeCurrency(ccy)
		if err != nil {
			return nil, fmt.Errorf("%w: CdtTrfTxInf[%d] (%q): bad Ccy %q", ErrInvalidPacs008, i, e2e, tx.Amt.Ccy)
		}
		creditor := AccountID(pacs008AccountIDOf(tx.CdtrAcct))
		if creditor == "" {
			return nil, fmt.Errorf("%w: CdtTrfTxInf[%d] (%q): missing creditor account", ErrInvalidPacs008, i, e2e)
		}
		var remit []string
		for _, u := range tx.RmtInf.Ustrd {
			if t := strings.TrimSpace(u); t != "" {
				remit = append(remit, t)
			}
		}
		out = append(out, Pacs008Transaction{
			MsgID:           msgID,
			EndToEndID:      e2e,
			TxID:            strings.TrimSpace(tx.PmtID.TxID),
			AmountCents:     amount,
			Currency:        currency,
			DebtorAccount:   strings.TrimSpace(pacs008AccountIDOf(tx.DbtrAcct)),
			DebtorName:      strings.TrimSpace(tx.Dbtr.Name),
			CreditorAccount: creditor,
			CreditorName:    strings.TrimSpace(tx.Cdtr.Name),
			Remittance:      strings.Join(remit, " | "),
		})
	}
	return out, nil
}

// parsePacs008Amount converts a decimal amount string to integer cents
// with no floats — the same half-away-from-zero rounding as the camt.053
// parser. The sign is preserved (the caller rejects non-positive
// amounts); anything that is not a decimal number is an error.
func parsePacs008Amount(s string) (int64, error) {
	bad := func() (int64, error) {
		return 0, fmt.Errorf("bad IntrBkSttlmAmt %q", s)
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return bad()
	}
	neg := false
	if s[0] == '+' {
		s = s[1:]
	} else if s[0] == '-' {
		neg = true
		s = s[1:]
	}
	intPart, fracPart := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, fracPart = s[:i], s[i+1:]
	}
	if intPart == "" {
		intPart = "0"
	}
	for i := 0; i < len(intPart); i++ {
		if intPart[i] < '0' || intPart[i] > '9' {
			return bad()
		}
	}
	for i := 0; i < len(fracPart); i++ {
		if fracPart[i] < '0' || fracPart[i] > '9' {
			return bad()
		}
	}
	dollars, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil || dollars > (1<<62)/100 {
		return bad()
	}
	padded := fracPart + "000"
	cents, err := strconv.ParseInt(padded[:2], 10, 64)
	if err != nil {
		return bad()
	}
	if padded[2] >= '5' {
		cents++
	}
	total := dollars*100 + cents
	if neg {
		total = -total
	}
	return total, nil
}

// pacs008EntryID builds the deterministic receipt entry ID for a
// notification: the EndToEndId is the bank's stable identity for this
// instant payment, so the ID doubles as the idempotency key —
// replaying the same notification returns the original entry.
func pacs008EntryID(e2e string) string {
	return "fednow-pacs008/" + e2e
}

// associatePacs008Locked resolves a notification's EndToEndId against
// the original payout/transfer idempotency key: a payout instruction
// (by payout ID, then by idempotency key) or a journaled transfer (by
// transfer ID — the pain.001 EndToEndId convention — then by
// idempotency key). Callers must hold l.mu (either lock suffices).
func (l *Ledger) associatePacs008Locked(e2e string) Pacs008Association {
	none := Pacs008Association{Kind: Pacs008AssociatedNone}
	if p, ok := l.payouts[e2e]; ok {
		return Pacs008Association{
			Kind:        Pacs008AssociatedPayout,
			ID:          p.ID,
			AmountCents: p.AmountCents,
			Currency:    p.Currency,
			Accounts:    []AccountID{p.PayeeAccount},
		}
	}
	if pid, ok := l.payoutKeys[e2e]; ok {
		if p, ok := l.payouts[pid]; ok {
			return Pacs008Association{
				Kind:        Pacs008AssociatedPayout,
				ID:          p.ID,
				AmountCents: p.AmountCents,
				Currency:    p.Currency,
				Accounts:    []AccountID{p.PayeeAccount},
			}
		}
	}
	// A transfer's principal journal entry carries the transfer ID —
	// the pain.001 EndToEndId convention — so entries[e2e] is the
	// transfer-by-ID lookup. The idempotency-key lookup goes through
	// transferKeys, whose first entry ID is the principal (see
	// replayTransferLocked).
	if ids, ok := l.transferKeys[e2e]; ok && len(ids) > 0 {
		if e, ok := l.entries[ids[0]]; ok {
			return Pacs008Association{
				Kind:        Pacs008AssociatedTransfer,
				ID:          e.ID,
				AmountCents: e.AmountCents,
				Currency:    e.Currency,
				Accounts:    []AccountID{e.DebitAccount, e.CreditAccount},
			}
		}
	}
	if e, ok := l.entries[e2e]; ok {
		return Pacs008Association{
			Kind:        Pacs008AssociatedTransfer,
			ID:          e.ID,
			AmountCents: e.AmountCents,
			Currency:    e.Currency,
			Accounts:    []AccountID{e.DebitAccount, e.CreditAccount},
		}
	}
	return none
}

// validatePacs008Locked checks one parsed notification against its
// association. Callers must hold l.mu (the write lock; the known-
// account check reads the journal index).
func (l *Ledger) validatePacs008Locked(tx Pacs008Transaction, assoc Pacs008Association) error {
	if assoc.Kind != Pacs008AssociatedNone {
		if tx.AmountCents != assoc.AmountCents {
			return fmt.Errorf("%w: notification %d vs %s %q %d",
				ErrFedNowAmountMismatch, tx.AmountCents, assoc.Kind, assoc.ID, assoc.AmountCents)
		}
		if tx.Currency != assoc.Currency {
			return fmt.Errorf("%w: notification %q vs %s %q %q",
				ErrFedNowCurrencyMismatch, tx.Currency, assoc.Kind, assoc.ID, assoc.Currency)
		}
		for _, a := range assoc.Accounts {
			if a == tx.CreditorAccount {
				return nil
			}
		}
		return fmt.Errorf("%w: creditor %q not among %s %q accounts",
			ErrFedNowPayeeMismatch, tx.CreditorAccount, assoc.Kind, assoc.ID)
	}
	// No original instruction: the payee must already be a known ledger
	// account — the ledger never credits an account it never heard of.
	if len(l.byAccount[tx.CreditorAccount]) == 0 {
		return fmt.Errorf("%w: %q", ErrFedNowUnknownPayeeAccount, tx.CreditorAccount)
	}
	return nil
}

// pacs008Memo builds the receipt entry's memo: the FedNow message ID
// plus the debtor→creditor direction. Capped at the journal memo
// limit (maxMemoRunes) — overlong remittance is truncated, never
// rejected.
func pacs008Memo(tx Pacs008Transaction) string {
	m := "fednow-pacs008"
	if tx.MsgID != "" {
		m += " " + tx.MsgID
	}
	if tx.DebtorAccount != "" {
		m += " " + tx.DebtorAccount + "->" + string(tx.CreditorAccount)
	}
	if tx.Remittance != "" {
		m += " " + tx.Remittance
	}
	if utf8.RuneCountInString(m) > maxMemoRunes {
		runes := []rune(m)
		m = string(runes[:maxMemoRunes])
	}
	return m
}

// ProcessPacs008 parses a FedNow pacs.008.001.08 inbound notification
// and books each transaction: associate the EndToEndId with the
// original payout/transfer idempotency key, validate amount/currency/
// payee, then post the dual-entry receipt (debit payee / credit the
// FedNow settlement account) through the full Post gate.
//
// The whole batch runs under one write lock via the internal locked
// path; a rejection is recorded per-item (result Err plus an explicit
// fednow_pacs008_rejected audit event) and never blocks the other
// items. The top-level error is nil unless the document itself is
// unparseable (ErrInvalidPacs008) — a half-readable bank file must not
// partially apply.
func (l *Ledger) ProcessPacs008(data []byte, actor string) ([]Pacs008Result, error) {
	if actor == "" {
		actor = "ProcessPacs008"
	}
	txs, err := ParsePacs008(data)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	results := make([]Pacs008Result, 0, len(txs))
	for i := range txs {
		results = append(results, l.processPacs008Locked(txs[i], actor))
	}
	return results, nil
}

// processPacs008Locked applies one notification. Callers must hold
// l.mu (the write lock).
func (l *Ledger) processPacs008Locked(tx Pacs008Transaction, actor string) Pacs008Result {
	res := Pacs008Result{Transaction: tx}
	res.Association = l.associatePacs008Locked(tx.EndToEndID)
	entryID := pacs008EntryID(tx.EndToEndID)

	// Idempotent replay: the same EndToEndId already booked — return
	// the original entry, book nothing new.
	if orig, ok := l.entries[entryID]; ok {
		res.EntryID = orig.ID
		res.Duplicate = true
		return res
	}

	reject := func(err error) Pacs008Result {
		res.Err = err
		l.fedNowRejections[reasonKey(err)]++
		l.emitAudit(AuditEvent{
			Op:      "fednow_pacs008_rejected",
			Actor:   actor,
			TraceID: tx.EndToEndID,
			Details: map[string]any{
				"end_to_end_id": tx.EndToEndID,
				"msg_id":        tx.MsgID,
				"amount_cents":  tx.AmountCents,
				"currency":      tx.Currency,
				"payee":         string(tx.CreditorAccount),
				"reason":        err.Error(),
			},
		})
		return res
	}

	if err := l.validatePacs008Locked(tx, res.Association); err != nil {
		return reject(err)
	}
	if l.fedNowSettlementAccount == "" {
		return reject(ErrFedNowSettlementAccountNotConfigured)
	}

	posted, duplicate, err := l.postLocked(JournalEntry{
		ID:             entryID,
		DebitAccount:   tx.CreditorAccount,
		CreditAccount:  l.fedNowSettlementAccount,
		AmountCents:    tx.AmountCents,
		Currency:       tx.Currency,
		IdempotencyKey: entryID,
		ExternalRef:    tx.EndToEndID,
		Memo:           pacs008Memo(tx),
	}, true)
	if err != nil {
		return reject(err)
	}
	res.EntryID = posted.ID
	res.Duplicate = duplicate
	if duplicate {
		// Lost the race with a concurrent identical notification:
		// the entry is already there — report the replay, book
		// nothing new.
		return res
	}
	l.fedNowPostings++
	l.emitAudit(AuditEvent{
		Op:            "fednow_pacs008_posted",
		Actor:         actor,
		TraceID:       posted.ID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		EntryIDs:      []string{posted.ID},
		Accounts:      []AccountID{tx.CreditorAccount, l.fedNowSettlementAccount},
		Details: map[string]any{
			"end_to_end_id":    tx.EndToEndID,
			"msg_id":           tx.MsgID,
			"tx_id":            tx.TxID,
			"amount_cents":     tx.AmountCents,
			"currency":         tx.Currency,
			"payee":            string(tx.CreditorAccount),
			"debtor":           tx.DebtorAccount,
			"association_kind": string(res.Association.Kind),
			"association_id":   res.Association.ID,
			"idempotency_key":  entryID,
		},
	})
	return res
}

// SetFedNowSettlementAccount configures (or clears, with the empty
// account) the ledger-wide FedNow settlement account — the credit leg
// of every pacs.008 receipt posting. Empty means unconfigured:
// ProcessPacs008 is fail-closed until the operator configures it.
// Structural config — it does not bump the journal version and it
// survives disaster-recovery snapshots, like the payout clearing
// account.
func (l *Ledger) SetFedNowSettlementAccount(a AccountID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fedNowSettlementAccount = a
	l.emitAudit(AuditEvent{
		Op:      "set_fednow_settlement_account",
		Actor:   "SetFedNowSettlementAccount",
		TraceID: string(a),
		Details: map[string]any{"fednow_settlement_account": string(a)},
	})
}

// FedNowStats returns the ledger's FedNow processing counters: total
// first-time postings and rejections by reason. Only first-time
// postings count — idempotent replays return duplicate and move
// nothing. In-memory only, like the ACH return counters: the counters
// do not round-trip through disaster-recovery snapshots; the journaled
// receipt rows and the fednow_pacs008_* audit events are the durable
// record.
func (l *Ledger) FedNowStats() (postings uint64, rejections map[string]uint64) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make(map[string]uint64, len(l.fedNowRejections))
	for reason, n := range l.fedNowRejections {
		out[reason] = n
	}
	return l.fedNowPostings, out
}

// reasonKey renders an error's stable rejection-reason label for the
// per-reason counters: the sentinel's message without the "ledger: "
// prefix, so "pacs.008 amount does not match the original instruction"
// becomes the metric label.
func reasonKey(err error) string {
	s := err.Error()
	return strings.TrimPrefix(s, "ledger: ")
}
