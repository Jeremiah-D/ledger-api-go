package ledger

import (
	"encoding/xml"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// ISO 20022 pacs.009.001.06 Financial Institution Credit Transfer
// inbound processing (LG-418): the inbound half of the pacs.009
// lifecycle, the mirror of the LG-172 export.
//
// ExportPacs009 renders a posted transfer as the instruction this
// ledger sends to its correspondent bank; this module consumes the
// instruction a correspondent bank sent, on the receiving bank's
// side: it verifies the FI-to-FI credit transfer against the
// instruction it answers and books the receipt. The two are
// orthogonal by construction:
//
//   - LG-172 exports: the ledger tells its correspondent bank "pay
//     this amount to the creditor's institution" (outbound,
//     read-only, no ledger state).
//   - LG-418 imports: the correspondent bank tells the ledger "this
//     amount was paid to your institution" (inbound,
//     state-changing: one receipt entry under the creditor agent's
//     account).
//
// ParsePacs009 reuses the pacs009.go wire structs, reading them in
// the reverse direction: the same `pacs.009.001.06`
// `FinancialInstitutionCreditTransfer` document, one `CdtTrfTxInf`
// to start (GrpHdr NbOfTxs must be 1). The namespace rides the
// Document element, so a document in the wrong schema version fails
// the unmarshal — the version gate is the namespace, not a string
// check (the camt.056 inbound convention, LG-414).
//
// Association follows the ProcessPacs008 receipt shape
// (fednow_pacs008.go, LG-163): the transfer's EndToEndId — then its
// InstrId — is looked up against the original transfer/payout
// idempotency key (see associatePacs008Locked). An associated
// transfer is validated against its original — amount and currency
// must match, and the CdtrAgt must be one of the original's
// accounts — otherwise the document is evidence about a different
// instruction than the one it claims to be. With no association the
// transfer books standalone, but only against a CdtrAgt account the
// ledger already knows: the ledger never credits an account it never
// heard of. pacs.009 carries no UETR (pacs009.go documents this),
// so the association never uses one — unlike the pacs.004/pacs.007
// paths, there is no gpi track to advance here (LG-165 is
// untouched).
//
// Booking follows the ProcessPacs008 receipt entry shape: debit the
// creditor agent's account (the inflow) / credit the pacs.009
// settlement account (fail-closed config, see
// SetPacs009SettlementAccount), through the full Post gate, so
// frozen/overdraft/daily-limit/period/denylist controls all apply.
// The entry ID is deterministic, "pacs009/<endtoendid>", so the
// idempotency key converges on the same row instead of
// double-booking. No FX: a transfer whose original settled
// cross-currency (the principal entry books only the payer-side leg
// against the FX clearing account) is refused with
// ErrPacs009FXTransfer — a single-currency pacs.009 cannot describe
// it, the ExportPacs009 fail-closed rule.
//
// Idempotency: the sender's GrpHdr MsgId is the replay key,
// recorded in the per-ledger applied-receipt registry (the repair.go
// registry pattern: package-level, keyed by *Ledger, lock order l.mu
// then the registry mutex). Re-applying the same document returns
// the original receipt with Duplicate set, booking nothing new, and
// emits no second `pacs009_receipt_applied` audit event. A MsgId
// reused for a different transfer is refused with
// ErrPacs009MsgIDConflict — one message ID names one transfer. An
// EndToEndId replayed under a new MsgId converges on the original
// entry too (the pacs.008 entry-ID convention).
//
// Fail-closed parking (the pacs.004 convention, LG-413): an unknown
// CdtrAgt account and an amount, currency, or creditor mismatch
// against the associated original are parked in the repair queue for
// operator resolution, and the typed error carries the repair record
// ID. A document the ledger cannot even parse is parked the same
// way by ParkPacs009Unparsed — visibility parking, never a silent
// drop of the bank's message. Repair records parked by this module
// are visibility parking, not resubmittable instructions: their
// Maker is deliberately empty, so the CreatePayout gate can never
// pass them. The plain fail-closed paths (a cross-currency
// original, an unconfigured settlement account, a MsgId reused for
// a different transfer) return the typed error with no repair
// record: there is nothing for an operator to repair through the
// queue in those states.
//
// Audit: a fresh booking emits `pacs009_receipt_applied`, distinct
// from the `fednow_pacs008_posted` event ProcessPacs008 emits, so
// the audit trail tells the inbound pacs.009 direction apart from
// an operator-driven entry or a FedNow receipt.

// pacs.009 inbound errors.
var (
	// ErrPacs009Parse is returned by ParsePacs009 for malformed XML
	// or a document missing a required field: the correspondent bank
	// sent something the ledger cannot even read as a credit
	// transfer. The HTTP layer parks it in the repair queue and
	// maps it to 400.
	ErrPacs009Parse = errors.New("ledger: pacs.009 inbound: unparseable financial institution credit transfer document")
	// ErrPacs009UnknownCreditorAccount is returned by
	// ApplyPacs009 when the transfer names no original instruction
	// and its CdtrAgt account is unknown to the ledger: the ledger
	// never credits an account it never heard of. The transfer is
	// parked in the repair queue, not dropped.
	ErrPacs009UnknownCreditorAccount = errors.New("ledger: pacs.009 inbound: creditor agent account is unknown")
	// ErrPacs009AmountMismatch is returned when a transfer names an
	// original transfer/payout but carries a different amount: it
	// is evidence about a different instruction, so it must not
	// book against it. The transfer is parked in the repair queue.
	ErrPacs009AmountMismatch = errors.New("ledger: pacs.009 amount does not match the original instruction")
	// ErrPacs009CurrencyMismatch is returned when a transfer names
	// an original transfer/payout but carries a different currency.
	// The transfer is parked in the repair queue.
	ErrPacs009CurrencyMismatch = errors.New("ledger: pacs.009 currency does not match the original instruction")
	// ErrPacs009CreditorMismatch is returned when a transfer names
	// an original transfer/payout but the creditor agent is none of
	// the original's accounts. The transfer is parked in the repair
	// queue.
	ErrPacs009CreditorMismatch = errors.New("ledger: pacs.009 creditor agent is not an account of the original instruction")
	// ErrPacs009MsgIDConflict is returned when a GrpHdr MsgId
	// already recorded names a different transfer (another
	// EndToEndId): one message ID names one transfer. It is a
	// 422-class error.
	ErrPacs009MsgIDConflict = errors.New("ledger: pacs.009 MsgId already applied to a different transfer")
	// ErrPacs009SettlementAccountNotConfigured is returned when a
	// transfer would book but no pacs.009 settlement account is
	// configured: the counterparty leg has nowhere to go, so the
	// transfer is refused fail-closed instead of booking a half-leg.
	// It is a 422-class error.
	ErrPacs009SettlementAccountNotConfigured = errors.New("ledger: pacs.009 settlement account not configured")
)

// Pacs009CreditTransfer is one parsed inbound pacs.009 FI-to-FI
// credit transfer: the transfer's identity, the amount, the
// settlement routing, and the agent chain.
type Pacs009CreditTransfer struct {
	// MsgID is the transfer's GrpHdr MsgId: the audit trace and
	// the replay key.
	MsgID string `json:"msg_id"`
	// InstrID, EndToEndID, and TxID are the PmtId triple.
	// EndToEndId is the business identity of the transfer: it
	// associates the transfer with the original instruction and
	// becomes the receipt entry's ID and idempotency key.
	InstrID    string `json:"instr_id,omitempty"`
	EndToEndID string `json:"end_to_end_id"`
	TxID       string `json:"tx_id,omitempty"`
	// AmountCents is the IntrBkSttlmAmt in minor units; Currency
	// is its Ccy attribute, a 3-letter uppercase ISO 4217 code.
	AmountCents int64  `json:"amount_cents"`
	Currency    string `json:"currency"`
	// SettlementMethod is GrpHdr/SttlmInf/SttlmMtd, one of
	// INDA/INGA/COVE/CLRG; ChargesBearer is CdtTrfTxInf/ChrgsBr,
	// one of DEBT/CRED/SHAR/SLEV.
	SettlementMethod string `json:"settlement_method"`
	ChargesBearer    string `json:"charges_bearer"`
	// SettlementDate is CdtTrfTxInf/IntrBkSttlmDt (YYYY-MM-DD),
	// when the document states one. Informational: the receipt
	// books at now, the ProcessPacs008 convention — the
	// settlement date is carried on the audit event, never used
	// to backdate a booking.
	SettlementDate string `json:"settlement_date,omitempty"`
	// InstructingAgent / InstructedAgent are InstgAgt / InstdAgt;
	// Debtor / DebtorAgent are Dbtr / DbtrAgt; CreditorAgent /
	// Creditor are CdtrAgt / Cdtr. The agents ride proprietary
	// identification (FinInstnId/Othr/Id = the account ID, the
	// pacs009.go mapping) — never a fabricated BIC.
	// CreditorAgent is the booking leg: the account the receipt
	// credits. The rest are informational.
	InstructingAgent string `json:"instructing_agent,omitempty"`
	InstructedAgent  string `json:"instructed_agent,omitempty"`
	Debtor           string `json:"debtor,omitempty"`
	DebtorAgent      string `json:"debtor_agent,omitempty"`
	CreditorAgent    string `json:"creditor_agent"`
	Creditor         string `json:"creditor,omitempty"`
	// PurposeCode is CdtTrfTxInf/Purp/Cd, when the document
	// states one. Informational: carried verbatim, never
	// interpreted — the ledger does not re-validate the sender's
	// purpose code.
	PurposeCode string `json:"purpose_code,omitempty"`
}

// Pacs009ReceiptResult is the outcome of ApplyPacs009.
type Pacs009ReceiptResult struct {
	// Applied reports whether this call booked the receipt. False
	// on both the duplicate no-op path and the error paths.
	Applied bool `json:"applied"`
	// Duplicate reports whether the transfer was already applied
	// under the same MsgId (or the same EndToEndId): a no-op
	// success, not an error.
	Duplicate bool `json:"duplicate"`
	// EntryID is the booked receipt journal row
	// ("pacs009/<endtoendid>"), set on the applied and duplicate
	// paths.
	EntryID string `json:"entry_id,omitempty"`
	// Association is the EndToEndId (then InstrId) resolution
	// against the original transfer/payout idempotency key — the
	// pacs.008 association shape, reused: the resolution is
	// identical.
	Association Pacs008Association `json:"association"`
	// Transfer is the parsed transfer this result is about.
	Transfer Pacs009CreditTransfer `json:"transfer"`
	// RepairID is set on the parked fail-closed paths (unknown
	// creditor agent account, amount/currency/creditor
	// mismatch): the repair-queue record holding the transfer for
	// operator resolution.
	RepairID string `json:"repair_id,omitempty"`
}

// pacs009AppliedReceipt is one applied receipt: the transfer it
// booked and the receipt row it booked.
type pacs009AppliedReceipt struct {
	EndToEndID  string
	EntryID     string
	Association Pacs008Association
}

// pacs009AppliedReceipts is the per-ledger applied-receipt registry
// (see the package comment): the idempotency record for
// ApplyPacs009. Callers must hold l.mu before taking
// pacs009AppliedReceipts.Mutex — the repair.go lock order.
var pacs009AppliedReceipts = struct {
	sync.Mutex
	m map[*Ledger]map[string]pacs009AppliedReceipt
}{m: make(map[*Ledger]map[string]pacs009AppliedReceipt)}

// pacs009AppliedForLocked returns l's applied-receipt map, creating
// it on first use. Callers must hold l.mu and
// pacs009AppliedReceipts.Mutex.
func pacs009AppliedForLocked(l *Ledger) map[string]pacs009AppliedReceipt {
	applied, ok := pacs009AppliedReceipts.m[l]
	if !ok {
		applied = make(map[string]pacs009AppliedReceipt)
		pacs009AppliedReceipts.m[l] = applied
	}
	return applied
}

// pacs009EntryID builds the deterministic receipt entry ID for a
// transfer: the EndToEndId is the sending bank's stable identity
// for this FI-to-FI credit transfer, so the ID doubles as the
// idempotency key — replaying the same transfer returns the
// original entry.
func pacs009EntryID(e2e string) string {
	return "pacs009/" + e2e
}

// pacs009ParseAmount parses an ISO 20022 decimal amount ("1234.56")
// into minor units, the pacs003ParseAmount rules: the integer part
// holds at most 12 digits, the fraction holds at most two digits
// and is right-padded ("5" means 50 cents), the value must be
// positive. Anything else is a parse refusal — a transfer amount
// the ledger cannot read exactly is never rounded into a booking.
func pacs009ParseAmount(body string) (int64, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return 0, fmt.Errorf("%w: empty amount", ErrPacs009Parse)
	}
	intPart := body
	fracPart := ""
	if i := strings.IndexByte(body, '.'); i >= 0 {
		intPart, fracPart = body[:i], body[i+1:]
		if strings.IndexByte(fracPart, '.') >= 0 {
			return 0, fmt.Errorf("%w: amount %q has two decimal points", ErrPacs009Parse, body)
		}
	}
	if intPart == "" || len(intPart) > 12 || len(fracPart) > 2 {
		return 0, fmt.Errorf("%w: amount %q", ErrPacs009Parse, body)
	}
	for _, c := range intPart + fracPart {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("%w: amount %q is not numeric", ErrPacs009Parse, body)
		}
	}
	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: amount %q", ErrPacs009Parse, body)
	}
	frac := int64(0)
	if fracPart != "" {
		if len(fracPart) == 1 {
			fracPart += "0"
		}
		frac, err = strconv.ParseInt(fracPart, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%w: amount %q", ErrPacs009Parse, body)
		}
	}
	cents := whole*100 + frac
	if cents <= 0 {
		return 0, fmt.Errorf("%w: amount %q is not positive", ErrPacs009Parse, body)
	}
	return cents, nil
}

// validatePacs009CreditTransfer fail-closes on a transfer that
// ParsePacs009 would refuse, so ApplyPacs009 stays safe on
// hand-built structs too: the parse gate and the apply gate share
// one check, never a copy.
func validatePacs009CreditTransfer(t Pacs009CreditTransfer) error {
	if strings.TrimSpace(t.MsgID) == "" {
		return fmt.Errorf("%w: missing GrpHdr/MsgId", ErrPacs009Parse)
	}
	if strings.TrimSpace(t.EndToEndID) == "" {
		return fmt.Errorf("%w: missing PmtId/EndToEndId", ErrPacs009Parse)
	}
	if t.AmountCents <= 0 {
		return fmt.Errorf("%w: non-positive interbank settlement amount", ErrPacs009Parse)
	}
	if _, err := normalizeCurrency(t.Currency); err != nil {
		return fmt.Errorf("%w: currency %q", ErrPacs009Parse, t.Currency)
	}
	if !pacs009ValidSettlementMethod(t.SettlementMethod) {
		return fmt.Errorf("%w: settlement method %q is outside the admitted vocabulary (%w)", ErrPacs009Parse, t.SettlementMethod, ErrPacs009InvalidSettlementMethod)
	}
	if !pacs009ValidChargesBearer(t.ChargesBearer) {
		return fmt.Errorf("%w: charges bearer %q is outside the admitted vocabulary (%w)", ErrPacs009Parse, t.ChargesBearer, ErrPacs009InvalidChargesBearer)
	}
	if strings.TrimSpace(t.CreditorAgent) == "" {
		return fmt.Errorf("%w: missing CdtTrfTxInf/CdtrAgt", ErrPacs009Parse)
	}
	return nil
}

// ParsePacs009 parses one inbound pacs.009 FI-to-FI credit transfer
// into a Pacs009CreditTransfer, reusing the pacs009.go wire structs
// in the reverse direction. It validates the whole document before
// returning anything: a missing MsgId, a NbOfTxs other than 1, a
// SttlmMtd or ChrgsBr outside the admitted vocabularies, an
// unreadable or non-positive amount, an empty or malformed currency,
// a missing CdtrAgt, or a malformed settlement date are all parse
// refusals — ApplyPacs009 never sees a half-read transfer. The
// agent chain is read but, apart from CdtrAgt, not gated: the
// CdtrAgt names the booking leg, so a document that misnames the
// other agents cannot misdirect the booking.
func ParsePacs009(data []byte) (Pacs009CreditTransfer, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return Pacs009CreditTransfer{}, fmt.Errorf("%w: empty document", ErrPacs009Parse)
	}
	var doc pacs009Document
	if err := xml.Unmarshal(data, &doc); err != nil {
		return Pacs009CreditTransfer{}, fmt.Errorf("%w: %v", ErrPacs009Parse, err)
	}
	grp := doc.Trf.GrpHdr
	tx := doc.Trf.TxInf
	if strings.TrimSpace(grp.NbOfTxs) != "1" {
		return Pacs009CreditTransfer{}, fmt.Errorf("%w: GrpHdr/NbOfTxs %q is not 1 (single CdtTrfTxInf)", ErrPacs009Parse, grp.NbOfTxs)
	}
	sttlmMtd := strings.TrimSpace(grp.SttlmInf.SttlmMtd)
	if !pacs009ValidSettlementMethod(sttlmMtd) {
		return Pacs009CreditTransfer{}, fmt.Errorf("%w: settlement method %q is outside the admitted vocabulary (%w)", ErrPacs009Parse, sttlmMtd, ErrPacs009InvalidSettlementMethod)
	}
	chrgsBr := strings.TrimSpace(tx.ChrgsBr)
	if !pacs009ValidChargesBearer(chrgsBr) {
		return Pacs009CreditTransfer{}, fmt.Errorf("%w: charges bearer %q is outside the admitted vocabulary (%w)", ErrPacs009Parse, chrgsBr, ErrPacs009InvalidChargesBearer)
	}
	amount, err := pacs009ParseAmount(tx.IntrBkSttlmAmt.Value)
	if err != nil {
		return Pacs009CreditTransfer{}, err
	}
	if strings.TrimSpace(tx.IntrBkSttlmAmt.Ccy) == "" {
		return Pacs009CreditTransfer{}, fmt.Errorf("%w: empty interbank settlement amount currency", ErrPacs009Parse)
	}
	currency, err := normalizeCurrency(tx.IntrBkSttlmAmt.Ccy)
	if err != nil {
		return Pacs009CreditTransfer{}, fmt.Errorf("%w: currency %q", ErrPacs009Parse, tx.IntrBkSttlmAmt.Ccy)
	}
	settlementDate := strings.TrimSpace(tx.IntrBkSttlmDt)
	if settlementDate != "" {
		if _, err := time.Parse("2006-01-02", settlementDate); err != nil {
			return Pacs009CreditTransfer{}, fmt.Errorf("%w: settlement date %q must be YYYY-MM-DD", ErrPacs009Parse, settlementDate)
		}
	}
	purposeCode := ""
	if tx.Purp != nil {
		purposeCode = strings.TrimSpace(tx.Purp.Cd)
	}
	t := Pacs009CreditTransfer{
		MsgID:            strings.TrimSpace(grp.MsgID),
		InstrID:          strings.TrimSpace(tx.PmtID.InstrID),
		EndToEndID:       strings.TrimSpace(tx.PmtID.EndToEndID),
		TxID:             strings.TrimSpace(tx.PmtID.TxID),
		AmountCents:      amount,
		Currency:         currency,
		SettlementMethod: sttlmMtd,
		ChargesBearer:    chrgsBr,
		SettlementDate:   settlementDate,
		InstructingAgent: strings.TrimSpace(tx.InstgAgt.FinInstnID.Othr.ID),
		InstructedAgent:  strings.TrimSpace(tx.InstdAgt.FinInstnID.Othr.ID),
		Debtor:           strings.TrimSpace(tx.Dbtr.FinInstnID.Othr.ID),
		DebtorAgent:      strings.TrimSpace(tx.DbtrAgt.FinInstnID.Othr.ID),
		CreditorAgent:    strings.TrimSpace(tx.CdtrAgt.FinInstnID.Othr.ID),
		Creditor:         strings.TrimSpace(tx.Cdtr.FinInstnID.Othr.ID),
		PurposeCode:      purposeCode,
	}
	if err := validatePacs009CreditTransfer(t); err != nil {
		return Pacs009CreditTransfer{}, err
	}
	return t, nil
}

// associatePacs009Locked resolves a transfer's EndToEndId — then
// its InstrId — against the original payout/transfer idempotency
// key, the ProcessPacs008 association: a payout instruction (by
// payout ID, then by idempotency key) or a journaled transfer (by
// transfer ID, then by idempotency key). Callers must hold l.mu
// (either lock suffices).
func (l *Ledger) associatePacs009Locked(t Pacs009CreditTransfer) Pacs008Association {
	if assoc := l.associatePacs008Locked(t.EndToEndID); assoc.Kind != Pacs008AssociatedNone {
		return assoc
	}
	if strings.TrimSpace(t.InstrID) != "" {
		return l.associatePacs008Locked(t.InstrID)
	}
	return Pacs008Association{Kind: Pacs008AssociatedNone}
}

// validatePacs009ReceiptLocked checks one parsed transfer against
// its association and the settlement config. Callers must hold l.mu
// (either lock suffices; the booking section re-runs it under the
// write lock).
func (l *Ledger) validatePacs009ReceiptLocked(t Pacs009CreditTransfer, assoc Pacs008Association) error {
	if assoc.Kind != Pacs008AssociatedNone {
		// A cross-currency original cannot be described by a
		// single-currency pacs.009: refuse before any value
		// comparison, the ExportPacs009 fail-closed rule.
		if assoc.Kind == Pacs008AssociatedTransfer {
			if _, isFX := l.entries[assoc.ID+"/fx"]; isFX {
				return fmt.Errorf("%w: %q", ErrPacs009FXTransfer, assoc.ID)
			}
		}
		if t.AmountCents != assoc.AmountCents {
			return fmt.Errorf("%w: transfer %d vs %s %q %d",
				ErrPacs009AmountMismatch, t.AmountCents, assoc.Kind, assoc.ID, assoc.AmountCents)
		}
		if t.Currency != assoc.Currency {
			return fmt.Errorf("%w: transfer %q vs %s %q %q",
				ErrPacs009CurrencyMismatch, t.Currency, assoc.Kind, assoc.ID, assoc.Currency)
		}
		found := false
		for _, a := range assoc.Accounts {
			if a == AccountID(t.CreditorAgent) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%w: creditor agent %q not among %s %q accounts",
				ErrPacs009CreditorMismatch, t.CreditorAgent, assoc.Kind, assoc.ID)
		}
	} else {
		// No original instruction: the creditor agent must already be
		// a known ledger account — the ledger never credits an account
		// it never heard of.
		if len(l.byAccount[AccountID(t.CreditorAgent)]) == 0 {
			return fmt.Errorf("%w: %q", ErrPacs009UnknownCreditorAccount, t.CreditorAgent)
		}
	}
	if l.pacs009SettlementAccount == "" {
		return ErrPacs009SettlementAccountNotConfigured
	}
	return nil
}

// pacs009Memo builds the receipt entry's memo: the pacs.009 message
// ID plus the debtor-agent→creditor-agent direction. Capped at the
// journal memo limit (maxMemoRunes) — overlong purpose text is
// truncated, never rejected.
func pacs009Memo(t Pacs009CreditTransfer) string {
	m := "pacs009"
	if t.MsgID != "" {
		m += " " + t.MsgID
	}
	if t.DebtorAgent != "" {
		m += " " + t.DebtorAgent + "->" + t.CreditorAgent
	}
	if t.PurposeCode != "" {
		m += " " + t.PurposeCode
	}
	if utf8.RuneCountInString(m) > maxMemoRunes {
		runes := []rune(m)
		m = string(runes[:maxMemoRunes])
	}
	return m
}

// parkPacs009Transfer parks a fail-closed parsed transfer in the
// repair queue and returns the record ID. The snapshot's Maker is
// deliberately empty (see the package comment): the record is
// operator visibility, never a resubmittable instruction. A parking
// failure (a full queue) is returned so the caller can fail loud
// rather than silently drop the bank's message.
func (l *Ledger) parkPacs009Transfer(t Pacs009CreditTransfer, reasonCode, actor string) (string, error) {
	rec, err := l.EnqueueRepair(PayoutInstruction{
		ID:          t.EndToEndID,
		AmountCents: t.AmountCents,
		Currency:    t.Currency,
		Destination: "pacs009-inbound:" + t.MsgID,
		// Maker intentionally empty: see the package comment.
	}, reasonCode, actor)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// ParkPacs009Unparsed parks a pacs.009 document that failed to parse
// in the repair queue and returns the record ID, so an unreadable
// bank message is operator-visible instead of silently dropped. The
// document was unreadable, so the snapshot carries no instruction
// fields — the destination names the inbound channel, and the parse
// error rides the caller's 400 body. A parking failure (a full
// queue) is returned so the caller can fail loud.
func (l *Ledger) ParkPacs009Unparsed(actor string) (string, error) {
	if actor == "" {
		actor = "ParkPacs009Unparsed"
	}
	rec, err := l.EnqueueRepair(PayoutInstruction{
		Destination: "pacs009-inbound:unparsed",
		// Maker intentionally empty: see the package comment.
	}, RepairReasonInvalidFormat, actor)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// SetPacs009SettlementAccount configures (or clears, with the empty
// account) the ledger-wide pacs.009 settlement account — the credit
// leg of every pacs.009 receipt posting. Empty means unconfigured:
// ApplyPacs009 is fail-closed until the operator configures it.
// Structural config — it does not bump the journal version and it
// survives disaster-recovery snapshots, like the FedNow settlement
// account.
func (l *Ledger) SetPacs009SettlementAccount(a AccountID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pacs009SettlementAccount = a
	l.emitAudit(AuditEvent{
		Op:      "set_pacs009_settlement_account",
		Actor:   "SetPacs009SettlementAccount",
		TraceID: string(a),
		Details: map[string]any{"pacs009_settlement_account": string(a)},
	})
}

// ApplyPacs009 books one parsed inbound pacs.009 FI-to-FI credit
// transfer: it associates the EndToEndId (then the InstrId) with
// the original transfer/payout idempotency key, validates amount,
// currency, and creditor agent against that original — or, with no
// association, against the ledger's known accounts — and posts the
// receipt (debit the CdtrAgt account / credit the pacs.009
// settlement account) through the full Post gate.
//
// The duplicate no-op path (same MsgId applied before, or the same
// EndToEndId already receipted) returns success with Duplicate set
// and no new booking. The parked fail-closed paths (unknown
// creditor agent account, amount/currency/creditor mismatch
// against the associated original) return the typed error with
// RepairID set. The plain fail-closed paths (a cross-currency
// original, an unconfigured settlement account, a MsgId reused for
// a different transfer) return the typed error with no repair
// record: there is nothing for an operator to repair through the
// queue in those states.
//
// Locking: the duplicate read and the validation run under the read
// lock; the booking section holds the write lock (the re-checks,
// postLocked, the registry write, and the audit event are one
// atomic section); the repair parking runs outside any held lock
// because EnqueueRepair takes l.mu itself.
func (l *Ledger) ApplyPacs009(t Pacs009CreditTransfer, actor string) (Pacs009ReceiptResult, error) {
	if err := validatePacs009CreditTransfer(t); err != nil {
		return Pacs009ReceiptResult{}, err
	}
	if actor == "" {
		actor = "ApplyPacs009"
	}
	entryID := pacs009EntryID(t.EndToEndID)

	// Duplicate read and validation: the same MsgId applied before
	// is a no-op success, whatever the ledger's current state.
	l.mu.RLock()
	pacs009AppliedReceipts.Lock()
	applied, dup := pacs009AppliedForLocked(l)[t.MsgID]
	pacs009AppliedReceipts.Unlock()
	assoc := l.associatePacs009Locked(t)
	orig, entryExists := l.entries[entryID]
	validateErr := l.validatePacs009ReceiptLocked(t, assoc)
	l.mu.RUnlock()
	if dup {
		if applied.EndToEndID != t.EndToEndID {
			return Pacs009ReceiptResult{}, fmt.Errorf("%w: MsgId %q", ErrPacs009MsgIDConflict, t.MsgID)
		}
		return Pacs009ReceiptResult{
			Duplicate:   true,
			EntryID:     applied.EntryID,
			Association: applied.Association,
			Transfer:    t,
		}, nil
	}
	if entryExists {
		// EndToEndId replay under a new MsgId: the entry ID is
		// the idempotency key, so the original receipt is the
		// answer and nothing new is booked.
		return Pacs009ReceiptResult{
			Duplicate:   true,
			EntryID:     orig.ID,
			Association: assoc,
			Transfer:    t,
		}, nil
	}
	if validateErr != nil {
		// The parked fail-closed paths, outside any held lock:
		// an unknown creditor agent account and the association
		// mismatches go to the repair queue.
		var reasonCode string
		switch {
		case errors.Is(validateErr, ErrPacs009UnknownCreditorAccount):
			reasonCode = RepairReasonUnknownRoute
		case errors.Is(validateErr, ErrPacs009AmountMismatch),
			errors.Is(validateErr, ErrPacs009CurrencyMismatch),
			errors.Is(validateErr, ErrPacs009CreditorMismatch):
			reasonCode = RepairReasonInvalidFormat
		}
		if reasonCode != "" {
			repairID, err := l.parkPacs009Transfer(t, reasonCode, actor)
			if err != nil {
				return Pacs009ReceiptResult{}, fmt.Errorf("%w (repair parking failed: %v)", validateErr, err)
			}
			return Pacs009ReceiptResult{RepairID: repairID}, validateErr
		}
		return Pacs009ReceiptResult{}, validateErr
	}

	// Booking section: re-check the duplicate and the association
	// under the write lock — a racing apply for the same MsgId, or
	// a racing payout amendment, converges here instead of
	// double-booking. The registry mutex is held for the whole
	// section (lock order l.mu then the registry mutex, the
	// repair.go order); nothing in the post path takes the
	// registry mutex.
	l.mu.Lock()
	defer l.mu.Unlock()
	pacs009AppliedReceipts.Lock()
	defer pacs009AppliedReceipts.Unlock()
	appliedMap := pacs009AppliedForLocked(l)
	if applied, dup := appliedMap[t.MsgID]; dup {
		if applied.EndToEndID != t.EndToEndID {
			return Pacs009ReceiptResult{}, fmt.Errorf("%w: MsgId %q", ErrPacs009MsgIDConflict, t.MsgID)
		}
		return Pacs009ReceiptResult{
			Duplicate:   true,
			EntryID:     applied.EntryID,
			Association: applied.Association,
			Transfer:    t,
		}, nil
	}
	if orig, ok := l.entries[entryID]; ok {
		return Pacs009ReceiptResult{
			Duplicate:   true,
			EntryID:     orig.ID,
			Association: l.associatePacs009Locked(t),
			Transfer:    t,
		}, nil
	}
	assoc = l.associatePacs009Locked(t)
	if err := l.validatePacs009ReceiptLocked(t, assoc); err != nil {
		// A racing amendment moved the original under this
		// apply: fail-closed with the plain error, no second
		// parking — the first pass already parked the stale
		// read, and parking under the held write lock would
		// deadlock (EnqueueRepair takes l.mu itself).
		return Pacs009ReceiptResult{}, err
	}
	posted, duplicate, err := l.postLocked(JournalEntry{
		ID:             entryID,
		DebitAccount:   AccountID(t.CreditorAgent),
		CreditAccount:  l.pacs009SettlementAccount,
		AmountCents:    t.AmountCents,
		Currency:       t.Currency,
		IdempotencyKey: entryID,
		ExternalRef:    t.EndToEndID,
		Memo:           pacs009Memo(t),
	}, true)
	if err != nil {
		return Pacs009ReceiptResult{}, err
	}
	if duplicate {
		// Lost the race with a concurrent identical receipt:
		// the entry is already there — report the replay, book
		// nothing new.
		return Pacs009ReceiptResult{
			Duplicate:   true,
			EntryID:     posted.ID,
			Association: assoc,
			Transfer:    t,
		}, nil
	}
	appliedMap[t.MsgID] = pacs009AppliedReceipt{
		EndToEndID:  t.EndToEndID,
		EntryID:     posted.ID,
		Association: assoc,
	}
	l.emitAudit(AuditEvent{
		Op:            "pacs009_receipt_applied",
		Actor:         actor,
		TraceID:       t.MsgID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		EntryIDs:      []string{posted.ID},
		Accounts:      []AccountID{AccountID(t.CreditorAgent), l.pacs009SettlementAccount},
		Details: map[string]any{
			"msg_id":            t.MsgID,
			"end_to_end_id":     t.EndToEndID,
			"instr_id":          t.InstrID,
			"tx_id":             t.TxID,
			"amount_cents":      t.AmountCents,
			"currency":          t.Currency,
			"settlement_method": t.SettlementMethod,
			"charges_bearer":    t.ChargesBearer,
			"settlement_date":   t.SettlementDate,
			"association_kind":  string(assoc.Kind),
			"association_id":    assoc.ID,
			"entry_id":          posted.ID,
		},
	})
	return Pacs009ReceiptResult{
		Applied:     true,
		EntryID:     posted.ID,
		Association: assoc,
		Transfer:    t,
	}, nil
}
