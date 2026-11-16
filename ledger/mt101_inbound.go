package ledger

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SWIFT FIN MT101 customer request-for-transfer inbound
// processing (LG-439): the inbound half of the MT101 lifecycle,
// the mirror of the LG-391 export.
//
// ExportMT101 renders this ledger's own booked transfers as the
// instruction file the debtor sends to its bank — the debtor side
// telling its bank "pay these". This module consumes the
// instruction a customer sent to this ledger, acting as the
// customer's bank: it books each instructed credit transfer
// through PostTransfer, and nothing else. The two directions are
// orthogonal by construction:
//
//   - LG-391 exports: the ledger renders its own booked transfers
//     for the bank (outbound, read-only).
//   - LG-439 imports: a customer file asks the ledger to book
//     credit transfers (inbound, state-changing: one journal
//     entry per :21:, plus one mt101_transfer_booked audit event
//     per booked transfer).
//   - LG-430 tracks the same transfer body in ISO form (pain.001
//     inbound): the FIN and ISO forms share one transfer body
//     (PainTransfer) and one booked/parked apply entry —
//     PostTransfer's gates apply identically, and a refusal
//     parks that :21: in the repair queue instead of blocking
//     the other payments.
//
// ParseMT101 builds a new value object — the outbound export
// renders with strings.Builder, so there is no wire struct to
// reuse in reverse (the pain.009 inbound convention, LG-422).
// Parsing is strict, along ParseMT202 (LG-204): the block
// structure, the field order, and every field value are
// validated, and anything outside the core set (sequence A
// :15A: :20: :28D: :50H: :52A: :30:, repeated sequence B :15B:
// :21: :32B: :57a: :59: :71A:) is refused — a payment
// instruction must not lose content quietly. CRLF line endings
// are accepted; a trailing newline after the `-}` trailer is
// tolerated. The :57a: `a` option means "BIC or name-and-
// address": this module implements the A (BIC) variant and the
// D (account ID) variant the export renders, and refuses any
// other :57a: form fail-closed. Block 3 is envelope framing,
// validated for shape only, the MT202 convention: the :20:
// reference is the business key.
//
// The document's fields, and what they route to:
//
//   - :20: is the message reference (MsgId) and the first half
//     of the replay key — one :20: names one instruction.
//   - :28D: must be "1/1": a single message, the export's rule.
//   - :50H: is the debtor as "/<debtor IBAN>", resolved to the
//     ledger account that registered it. An IBAN no account
//     registered refuses the whole document with
//     ErrMT101UnknownAccount — the ledger never debits an
//     unknown account (the fail-closed rule, 422).
//   - :52A:, when present, is the debtor's servicing-bank BIC:
//     it must equal the debtor account's registered BIC.
//   - :30: is the execution date (YYMMDD): it must be a valid
//     calendar date. It is carried on the parsed document and
//     the replay record; booking itself happens at apply time,
//     the ledger having no future-date scheduling.
//   - Each :21: is the TransferID and the second half of the
//     replay key, the journal entry ID, and the per-transfer
//     idempotency key's tail ("mt101-<MsgId>-<:21:>"), so the
//     customer can dedupe on it exactly like an exported
//     transfer.
//   - :32B: is the currency plus the amount in cents, comma
//     decimal: the currency must be ISO 4217 alpha-3 and the
//     amount must be positive, both parse gates — a malformed
//     :32B: refuses the whole document, never a single payment.
//   - :57a: names the creditor's bank: :57A: must be the
//     creditor account's registered BIC, :57D: must be the
//     creditor account ID itself — the :57a: must name a known
//     account, never an unverifiable third party.
//   - :59: is the beneficiary as "/<creditor IBAN>", resolved
//     like :50H:: unknown refuses the whole document with
//     ErrMT101UnknownAccount.
//   - :71A: is the charge bearer (OUR/BEN/SHA), defaulting to
//     SHA when the sequence B omits it — the FIN code for the
//     pain.001 SHAR default, the export's default.
//
// Each transfer books through PostTransfer, so all of its gates
// apply — field validation (same-party), idempotency replay,
// frozen/overdraft, daily limit, accounting period, denylist,
// dual-control review — and a refusal parks that transfer in
// the repair queue (the pain.001 fail-closed parking, LG-430)
// and never blocks the other transfers. The transfer is
// single-currency by construction: ToCurrency stays empty, so a
// cross-currency payment can never be booked as one MT101
// instruction (the pain.001 convention).
//
// Idempotency is the :20: plus :21: with its terms (the
// pain.014 convention, LG-416, per transfer): re-applying the
// same :20:+:21: with the same debtor, value date, creditor,
// amount, currency, and charge bearer returns the stored
// outcome with Duplicate set, booking nothing new and emitting
// no second audit event. A :20:+:21: reused for different terms
// is refused with ErrMT101MsgIDConflict — never silently
// overwritten. The per-transfer idempotency keys are a second
// layer: even if the in-memory replay record is lost (a
// restart), PostTransfer replays the original receipts instead
// of booking twice.
//
// Registration is not execution, and there is no pain.001
// instruction lifecycle registration here: an MT101 has no
// pain.002 answer path (see the mt101.go header note), so the
// booked transfers enter no LG-99 status machine — the bank's
// verdict on an MT101 arrives out of band, not through this
// ledger.
//
// No MT101 export change; no PostTransfer semantic change; no
// automatic account opening; no cross-currency single
// instruction; no :70: narrative (a PainTransfer has no memo
// field); no :23E:/:77B:; no new repair reason codes; no
// already-booked transfer is touched.

// MT101 inbound errors.
var (
	// ErrInvalidMT101 is returned by ParseMT101 for a malformed
	// FIN document or a document missing a required field, and
	// by ApplyMT101 for a hand-built document that fails the
	// same gates: the customer sent something the ledger cannot
	// even read as a request for transfer. The HTTP layer maps
	// it to 400.
	ErrInvalidMT101 = errors.New("ledger: invalid inbound MT101 request for transfer")
	// ErrMT101Framing is returned when the FIN block structure
	// is broken: a missing block, a malformed block 1/2/3
	// header, a missing or unterminated block 4, an empty line
	// inside block 4, or a field out of the canonical :15A: →
	// :20: → :28D: → :50H: → :52A: → :30: → (:15B: → :21: →
	// :32B: → :57a: → :59: → :71A:)* order.
	ErrMT101Framing = errors.New("ledger: mt101: malformed FIN framing")
	// ErrMT101Field is returned for a block-4 field problem that
	// is not a value problem: a missing required field, a
	// duplicate field, a :28D: other than "1/1", a sequence B
	// without a :57a:, or a field outside the core set
	// (including :70:, :23E:, :77B:, and :72:).
	ErrMT101Field = errors.New("ledger: mt101: unexpected or missing field")
	// ErrMT101Reference is returned for an invalid :20: or :21:
	// reference: empty, over 16 characters, outside the SWIFT
	// `x` character set, carrying a brace, or with a
	// leading/trailing colon or slash, or a double slash.
	ErrMT101Reference = errors.New("ledger: mt101: invalid reference")
	// ErrMT101ValueDate is returned for an invalid :30:
	// execution date: not a YYMMDD calendar date.
	ErrMT101ValueDate = errors.New("ledger: mt101: invalid :30: value date")
	// ErrMT101Currency is returned for an invalid :32B:
	// currency: not exactly three uppercase ASCII letters (the
	// LG-18 currency rule — no lowercase, no digits, no ISO
	// lookup table the ledger does not own).
	ErrMT101Currency = errors.New("ledger: mt101: invalid :32B: currency")
	// ErrMT101Amount is returned for an invalid :32B: amount:
	// non-positive, not in `<integer>,<2 fraction digits>` form,
	// or with more than 12 integer digits (the MT103 15d rule).
	ErrMT101Amount = errors.New("ledger: mt101: invalid :32B: amount")
	// ErrMT101BIC is returned for an invalid :52A: or :57A:
	// institution: empty, or not a valid ISO 9362 BIC (see
	// validBIC in pain001.go).
	ErrMT101BIC = errors.New("ledger: mt101: invalid institution BIC")
	// ErrMT101LT is returned for an invalid sender or receiver
	// LT address (not a valid BIC8/BIC11/BIC12 form).
	ErrMT101LT = errors.New("ledger: mt101: invalid SWIFT LT address")
	// ErrMT101ChargeBearer is returned for an invalid :71A:
	// charge bearer: not OUR, BEN, or SHA.
	ErrMT101ChargeBearer = errors.New("ledger: mt101: invalid :71A: charge bearer")
	// ErrMT101MsgIDConflict is returned by ApplyMT101 when a
	// :20:+:21: key already applied carries different terms:
	// the reference names one transfer, and a second, different
	// transfer under it is refused rather than silently
	// overwritten (the pain.014 ErrPain014MsgIDConflict
	// convention). It is a 422-class error.
	ErrMT101MsgIDConflict = errors.New("ledger: mt101 inbound: message reference already applied with different terms")
	// ErrMT101UnknownAccount is returned by ApplyMT101 when a
	// :50H: or :59: IBAN names no account the ledger registered
	// an IBAN for: the ledger never debits or credits an unknown
	// account — the document is refused fail-closed. It is a
	// 422-class error.
	ErrMT101UnknownAccount = errors.New("ledger: mt101 inbound: IBAN names no registered account")
)

// MT101Transfer is one parsed inbound MT101 sequence B payment:
// the :21: transfer identity plus the :32B: amount, the :57a:
// creditor bank, the :59: beneficiary IBAN, and the :71A:
// charge bearer, in document order.
type MT101Transfer struct {
	// TransferID is :21:: the journal entry ID, the
	// per-transfer idempotency key's tail, and the second half
	// of the replay key — one :21: names one transfer.
	TransferID string `json:"transfer_id"`
	// AmountCents is :32B: in minor units, always positive;
	// Currency is its ISO 4217 alpha-3 code.
	AmountCents int64  `json:"amount_cents"`
	Currency    string `json:"currency"`
	// CreditorBIC is :57A: when the message carries it, the
	// creditor account's registered BIC; CreditorAccount is
	// :57D: when the message carries that instead, the creditor
	// account ID itself. Exactly one is set.
	CreditorBIC     string `json:"creditor_bic,omitempty"`
	CreditorAccount string `json:"creditor_account,omitempty"`
	// CreditorIBAN is :59:, the beneficiary IBAN, normalized.
	CreditorIBAN string `json:"creditor_iban"`
	// ChargeBearer is :71A: (OUR/BEN/SHA), defaulting to SHA
	// when the sequence B omits it.
	ChargeBearer string `json:"charge_bearer"`
}

// MT101Document is one parsed inbound MT101 request for
// transfer: the sequence A identity plus every sequence B
// payment it carries, in document order.
type MT101Document struct {
	// SenderLT and ReceiverLT are the block 1/2 LT addresses.
	SenderLT   string `json:"sender_lt"`
	ReceiverLT string `json:"receiver_lt"`
	// MsgID is :20:: the audit trace and the replay key — one
	// :20: names one instruction.
	MsgID string `json:"msg_id"`
	// ValueDate is :30:, the execution date as a UTC calendar
	// date (time-of-day is not carried by :30:).
	ValueDate time.Time `json:"value_date"`
	// DebtorIBAN is :50H:, the debtor IBAN, normalized;
	// DebtorBIC is :52A: when the message carries it, the
	// debtor account's registered BIC.
	DebtorIBAN string `json:"debtor_iban"`
	DebtorBIC  string `json:"debtor_bic,omitempty"`
	// Items are the sequence B payments, in document order.
	Items []MT101Transfer `json:"items"`
}

// MT101ItemResult is the per-transfer outcome of ApplyMT101: a
// transfer is either booked (Receipt set, EntryID set), an
// idempotent replay (Duplicate true), or rejected (Err set,
// RepairID set). One transfer's failure never blocks the
// others.
type MT101ItemResult struct {
	// Instruction is the parsed transfer this result is about.
	Instruction MT101Transfer `json:"instruction"`
	// From and To are the ledger accounts the wire IBANs
	// resolved to, empty when the transfer was rejected before
	// the lookup (or the lookup itself failed).
	From AccountID `json:"from_account,omitempty"`
	To   AccountID `json:"to_account,omitempty"`
	// Receipt is the PostTransfer receipt on success or replay;
	// empty when rejected.
	Receipt TransferReceipt `json:"receipt,omitempty"`
	// EntryID is the booked entry's ID (the transfer's :21:),
	// empty when rejected.
	EntryID string `json:"entry_id,omitempty"`
	// Duplicate reports an idempotent replay: the same transfer
	// was applied before, so the earlier entry is returned and
	// nothing new was booked — no new audit event.
	Duplicate bool `json:"duplicate"`
	// ReviewStatus carries the PostTransfer dual-control state
	// ("pending_review" when the transfer was routed into
	// operator review instead of settling); empty otherwise.
	ReviewStatus string `json:"review_status,omitempty"`
	// RepairID is the repair-queue record holding a rejected
	// transfer for operator resolution, empty on success.
	RepairID string `json:"repair_id,omitempty"`
	// Err is the per-transfer rejection reason, nil on success.
	Err error `json:"-"`
}

// MT101ApplyResult is the outcome of ApplyMT101.
type MT101ApplyResult struct {
	// MsgID is the :20: reference of the applied document.
	MsgID string `json:"msg_id"`
	// Items are the per-transfer outcomes, in document order.
	Items []MT101ItemResult `json:"items"`
	// Duplicate reports whether the whole document was already
	// applied under the same :20: with the same terms: a no-op
	// success, not an error.
	Duplicate bool `json:"duplicate,omitempty"`
}

// mt101AppliedRecord is the replay record for one applied MT101
// transfer, keyed by :20:+:21: in mt101Applied: the document
// terms it carried and the apply outcome, so a replay returns
// the same outcome without re-booking or re-parking.
type mt101AppliedRecord struct {
	MsgID      string
	DebtorIBAN string
	ValueDate  time.Time
	Transfer   MT101Transfer
	Result     MT101ItemResult
}

// mt101AppliedKey names one transfer's replay record: the :20:
// plus the :21:, joined with a NUL byte — a byte no 16x
// reference can carry, so the join is collision-free.
func mt101AppliedKey(msgID, transferID string) string {
	return msgID + "\x00" + transferID
}

// mt101TermsEqual reports whether a stored transfer record
// carries exactly the terms an inbound request would apply: the
// same debtor (by normalized IBAN), the same value date (by
// calendar day, as :30: carries only a day), and the same
// transfer — the same creditor (by normalized IBAN and :57a:),
// amount, currency, and charge bearer.
func mt101TermsEqual(rec mt101AppliedRecord, doc MT101Document, item MT101Transfer) bool {
	return normalizeIBAN(rec.DebtorIBAN) == normalizeIBAN(doc.DebtorIBAN) &&
		mt103ValueDate(rec.ValueDate) == mt103ValueDate(doc.ValueDate) &&
		rec.Transfer.TransferID == item.TransferID &&
		rec.Transfer.AmountCents == item.AmountCents &&
		rec.Transfer.Currency == item.Currency &&
		rec.Transfer.CreditorBIC == item.CreditorBIC &&
		rec.Transfer.CreditorAccount == item.CreditorAccount &&
		normalizeIBAN(rec.Transfer.CreditorIBAN) == normalizeIBAN(item.CreditorIBAN) &&
		rec.Transfer.ChargeBearer == item.ChargeBearer
}

// mt101Reference validates a :20:/:21: reference: 1–16 SWIFT `x`
// characters, no braces in block-4 content (see mt103CheckX), no
// leading/trailing colon or slash, no double slash. The MT202
// rule, under the MT101 error identity.
func mt101Reference(ref, field string) error {
	if ref == "" || len(ref) > mt103MaxSenderReference {
		return fmt.Errorf("%w: %s: want 1-%d characters, got %q", ErrMT101Reference, field, mt103MaxSenderReference, ref)
	}
	if err := mt103CheckX(ref); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrMT101Reference, field, err)
	}
	if strings.HasPrefix(ref, ":") || strings.HasPrefix(ref, "/") ||
		strings.HasSuffix(ref, ":") || strings.HasSuffix(ref, "/") ||
		strings.Contains(ref, "//") {
		return fmt.Errorf("%w: %s: leading/trailing colon or slash, or double slash: %q", ErrMT101Reference, field, ref)
	}
	return nil
}

// mt101Currency validates a :32B: currency code: exactly three
// uppercase ASCII letters. The MT202 rule, under the MT101 error
// identity.
func mt101Currency(code string) error {
	if len(code) != 3 {
		return fmt.Errorf("%w: want 3 letters, got %q", ErrMT101Currency, code)
	}
	for i := 0; i < 3; i++ {
		if code[i] < 'A' || code[i] > 'Z' {
			return fmt.Errorf("%w: %q", ErrMT101Currency, code)
		}
	}
	return nil
}

// mt101ParseAmount parses a :32B: amount body
// (`<integer>,<2 fraction digits>`) into cents. The integer part
// holds at most 12 digits; the fraction part holds exactly two
// digits; the value must be positive. The MT202 rule, under the
// MT101 error identity.
func mt101ParseAmount(body string) (int64, error) {
	parts := strings.Split(body, ",")
	if len(parts) != 2 {
		return 0, fmt.Errorf("%w: want <integer>,<2 fraction digits>, got %q", ErrMT101Amount, body)
	}
	intPart, fracPart := parts[0], parts[1]
	if intPart == "" || fracPart == "" || len(fracPart) != 2 || len(intPart) > 12 {
		return 0, fmt.Errorf("%w: %q", ErrMT101Amount, body)
	}
	for _, c := range intPart + fracPart {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("%w: %q", ErrMT101Amount, body)
		}
	}
	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", ErrMT101Amount, body)
	}
	frac, err := strconv.ParseInt(fracPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", ErrMT101Amount, body)
	}
	cents := whole*100 + frac
	if cents <= 0 {
		return 0, fmt.Errorf("%w: %q is not positive", ErrMT101Amount, body)
	}
	return cents, nil
}

// mt101ParseLT extracts the LT address from a block 1 or block 2
// header line and returns it exactly as written, so Export ∘
// Parse is stable: Export normalizes (BIC8 → BIC8+"XXX", the
// MT103 rule), and Parse returns that normalized form verbatim
// rather than re-normalizing it a second time (which would append
// another "X"). Block 1 has the form
// `{1:F01<LT><10 session/sequence digits>}`; block 2 has the
// form `{2:I101<LT><5 header characters>}` (priority U, delivery
// monitoring 3, obsolescence period 003). The MT202 rule, under
// the MT101 error identity.
func mt101ParseLT(line, prefix string, tailLen int) (string, error) {
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, "}") {
		return "", fmt.Errorf("%w: %q", ErrMT101Framing, line)
	}
	inner := line[len(prefix) : len(line)-1]
	if len(inner) <= tailLen {
		return "", fmt.Errorf("%w: %q", ErrMT101Framing, line)
	}
	lt, tail := inner[:len(inner)-tailLen], inner[len(inner)-tailLen:]
	// Block 1's tail is the session/sequence digits (digit-checked);
	// block 2's tail is header flags (any 5 characters: U3003).
	if tailLen == 10 {
		for _, c := range tail {
			if c < '0' || c > '9' {
				return "", fmt.Errorf("%w: %q", ErrMT101Framing, line)
			}
		}
	}
	if !mt202ValidLT(lt) {
		return "", fmt.Errorf("%w: %q", ErrMT101LT, lt)
	}
	return lt, nil
}

// mt101ParseIBANField parses a :50H: or :59: account line:
// "/<IBAN>", single line, the IBAN normalized and format-checked
// (a malformed IBAN is a malformed field — whether it names a
// registered account is the apply gate's question, not the
// parser's).
func mt101ParseIBANField(body, field string) (string, error) {
	if !strings.HasPrefix(body, "/") {
		return "", fmt.Errorf("%w: %s: want /<IBAN>, got %q", ErrMT101Field, field, body)
	}
	iban := normalizeIBAN(body[1:])
	if iban == "" {
		return "", fmt.Errorf("%w: %s field is empty", ErrMT101Field, field)
	}
	if err := mt103CheckX(iban); err != nil {
		return "", fmt.Errorf("%w: %s: %w", ErrMT101Field, field, err)
	}
	if !validIBAN(iban) {
		return "", fmt.Errorf("%w: %s: %q is not a valid IBAN", ErrMT101Field, field, iban)
	}
	return iban, nil
}

// ParseMT101 parses a SWIFT FIN MT101 message produced by
// ExportMT101 (or an equivalent customer message) into its
// business fields. Parsing is strict: the block structure, the
// field order, and every field value are validated, and anything
// outside the core set is refused — a payment instruction must
// not lose content quietly. CRLF line endings are accepted; a
// trailing newline after the `-}` trailer is tolerated.
func ParseMT101(text string) (MT101Document, error) {
	var zero MT101Document
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	lines := strings.Split(text, "\n")
	// Minimum shape: 3 headers + {4: + sequence A (5 lines
	// without :52A:) + one sequence B (5 lines without :71A:)
	// + the -} trailer = 15 lines.
	if len(lines) < 15 {
		return zero, fmt.Errorf("%w: want at least 15 lines (3 headers + {4: + sequence A + one sequence B + -}), got %d", ErrMT101Framing, len(lines))
	}
	senderLT, err := mt101ParseLT(lines[0], "{1:F01", 10)
	if err != nil {
		return zero, err
	}
	receiverLT, err := mt101ParseLT(lines[1], "{2:I101", 5)
	if err != nil {
		return zero, err
	}
	// Block 3: shape-checked only (see the MT202 package doc).
	mur := lines[2]
	if !strings.HasPrefix(mur, "{3:{108:") || !strings.HasSuffix(mur, "}}") {
		return zero, fmt.Errorf("%w: malformed block 3: %q", ErrMT101Framing, mur)
	}
	if err := mt101Reference(mur[len("{3:{108:"):len(mur)-2], "{108:}"); err != nil {
		return zero, err
	}
	if lines[3] != "{4:" {
		return zero, fmt.Errorf("%w: want {4: on line 4, got %q", ErrMT101Framing, lines[3])
	}
	i := 4
	// Sequence A, canonical order: :15A: :20: :28D: :50H:
	// (:52A:) :30:.
	expect := func(tag string) (string, error) {
		if i >= len(lines) {
			return "", fmt.Errorf("%w: missing %s field", ErrMT101Field, tag)
		}
		line := lines[i]
		if line == "" {
			return "", fmt.Errorf("%w: empty line inside block 4", ErrMT101Framing)
		}
		if !strings.HasPrefix(line, tag) {
			return "", fmt.Errorf("%w: line %d: want %s field, got %q", ErrMT101Field, i+1, tag, line)
		}
		body := line[len(tag):]
		if body == "" {
			return "", fmt.Errorf("%w: %s field is empty", ErrMT101Field, tag)
		}
		i++
		return body, nil
	}
	if i >= len(lines) || lines[i] != ":15A:" {
		got := ""
		if i < len(lines) {
			got = lines[i]
		}
		return zero, fmt.Errorf("%w: line %d: want :15A: field, got %q", ErrMT101Field, i+1, got)
	}
	i++
	var doc MT101Document
	doc.SenderLT, doc.ReceiverLT = senderLT, receiverLT
	body, err := expect(":20:")
	if err != nil {
		return zero, err
	}
	if err := mt101Reference(body, ":20:"); err != nil {
		return zero, err
	}
	doc.MsgID = body
	body, err = expect(":28D:")
	if err != nil {
		return zero, err
	}
	if body != "1/1" {
		return zero, fmt.Errorf("%w: :28D: want 1/1 (a single message), got %q", ErrMT101Field, body)
	}
	body, err = expect(":50H:")
	if err != nil {
		return zero, err
	}
	debtorIBAN, err := mt101ParseIBANField(body, ":50H:")
	if err != nil {
		return zero, err
	}
	doc.DebtorIBAN = debtorIBAN
	// :52A: is optional; when present it carries the debtor's
	// servicing-bank BIC.
	if i < len(lines) && strings.HasPrefix(lines[i], ":52A:") {
		body, err = expect(":52A:")
		if err != nil {
			return zero, err
		}
		if !validBIC(body) {
			return zero, fmt.Errorf("%w: :52A: %q", ErrMT101BIC, body)
		}
		doc.DebtorBIC = strings.ToUpper(body)
	}
	body, err = expect(":30:")
	if err != nil {
		return zero, err
	}
	vd, err := time.Parse("060102", body)
	if err != nil {
		return zero, fmt.Errorf("%w: %q: %w", ErrMT101ValueDate, body, err)
	}
	doc.ValueDate = vd.UTC()
	// Sequence B repeats per payment until the trailer: :15B:
	// :21: :32B: :57a: :59: (:71A:).
	seen := map[string]bool{}
	for {
		if i >= len(lines) {
			return zero, fmt.Errorf("%w: missing -} trailer", ErrMT101Framing)
		}
		if lines[i] == "-}" {
			if i != len(lines)-1 {
				return zero, fmt.Errorf("%w: content after -} trailer", ErrMT101Framing)
			}
			break
		}
		if lines[i] != ":15B:" {
			return zero, fmt.Errorf("%w: line %d: want :15B: field or -} trailer, got %q", ErrMT101Field, i+1, lines[i])
		}
		i++
		var item MT101Transfer
		body, err = expect(":21:")
		if err != nil {
			return zero, err
		}
		if err := mt101Reference(body, ":21:"); err != nil {
			return zero, err
		}
		if seen[body] {
			return zero, fmt.Errorf("%w: duplicate :21: %q in one document", ErrInvalidMT101, body)
		}
		seen[body] = true
		item.TransferID = body
		body, err = expect(":32B:")
		if err != nil {
			return zero, err
		}
		if len(body) < 3+4 {
			return zero, fmt.Errorf("%w: :32B: too short: %q", ErrMT101Framing, body)
		}
		item.Currency = body[:3]
		if err := mt101Currency(item.Currency); err != nil {
			return zero, err
		}
		amount, err := mt101ParseAmount(body[3:])
		if err != nil {
			return zero, err
		}
		item.AmountCents = amount
		// :57a:: exactly one of :57A: (creditor BIC) or :57D:
		// (creditor account ID), the export's two forms.
		if i >= len(lines) {
			return zero, fmt.Errorf("%w: missing :57a: field", ErrMT101Field)
		}
		switch {
		case strings.HasPrefix(lines[i], ":57A:"):
			body, err = expect(":57A:")
			if err != nil {
				return zero, err
			}
			if !validBIC(body) {
				return zero, fmt.Errorf("%w: :57A: %q", ErrMT101BIC, body)
			}
			item.CreditorBIC = strings.ToUpper(body)
		case strings.HasPrefix(lines[i], ":57D:"):
			body, err = expect(":57D:")
			if err != nil {
				return zero, err
			}
			if err := mt103CheckX(body); err != nil {
				return zero, fmt.Errorf("%w: :57D: %w", ErrMT101Field, err)
			}
			item.CreditorAccount = body
		default:
			return zero, fmt.Errorf("%w: line %d: want :57A: or :57D: field, got %q", ErrMT101Field, i+1, lines[i])
		}
		body, err = expect(":59:")
		if err != nil {
			return zero, err
		}
		creditorIBAN, err := mt101ParseIBANField(body, ":59:")
		if err != nil {
			return zero, err
		}
		item.CreditorIBAN = creditorIBAN
		// :71A: is optional per sequence B; absent it defaults
		// to SHA, the export's default.
		item.ChargeBearer = "SHA"
		if i < len(lines) && strings.HasPrefix(lines[i], ":71A:") {
			body, err = expect(":71A:")
			if err != nil {
				return zero, err
			}
			switch body {
			case "OUR", "BEN", "SHA":
				item.ChargeBearer = body
			default:
				return zero, fmt.Errorf("%w: %q: want OUR, BEN, or SHA", ErrMT101ChargeBearer, body)
			}
		}
		doc.Items = append(doc.Items, item)
	}
	if len(doc.Items) == 0 {
		return zero, fmt.Errorf("%w: no sequence B payment", ErrMT101Field)
	}
	return doc, nil
}

// validateMT101Document fail-closes on a document ParseMT101
// would refuse, so ApplyMT101 stays safe on hand-built
// documents too: the parse gate and the apply gate share one
// check, never a copy. The business gates (the wire IBANs
// resolve to registered accounts, the :52A:/:57a: BICs agree
// with the registry, and PostTransfer's own gates) are
// deliberately NOT here: they are apply decisions, and an
// unknown account refuses the whole document while a
// PostTransfer refusal parks that transfer in the repair queue
// instead of refusing the file.
func validateMT101Document(doc MT101Document) error {
	if err := mt101Reference(doc.MsgID, ":20:"); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidMT101, err)
	}
	if doc.ValueDate.IsZero() {
		return fmt.Errorf("%w: missing :30: execution date", ErrInvalidMT101)
	}
	if !validIBAN(normalizeIBAN(doc.DebtorIBAN)) {
		return fmt.Errorf("%w: missing or invalid :50H: debtor IBAN", ErrInvalidMT101)
	}
	if doc.DebtorBIC != "" && !validBIC(doc.DebtorBIC) {
		return fmt.Errorf("%w: :52A: %q", ErrInvalidMT101, doc.DebtorBIC)
	}
	if len(doc.Items) == 0 {
		return fmt.Errorf("%w: no sequence B payment", ErrInvalidMT101)
	}
	seen := map[string]bool{}
	for _, item := range doc.Items {
		if err := mt101Reference(item.TransferID, ":21:"); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidMT101, err)
		}
		if seen[item.TransferID] {
			return fmt.Errorf("%w: duplicate :21: %q in one document", ErrInvalidMT101, item.TransferID)
		}
		seen[item.TransferID] = true
		if item.AmountCents <= 0 {
			return fmt.Errorf("%w: :32B: amount must be positive, got %d", ErrInvalidMT101, item.AmountCents)
		}
		if _, err := normalizeCurrency(item.Currency); err != nil {
			return fmt.Errorf("%w: currency %q", ErrInvalidMT101, item.Currency)
		}
		if !validIBAN(normalizeIBAN(item.CreditorIBAN)) {
			return fmt.Errorf("%w: missing or invalid :59: creditor IBAN", ErrInvalidMT101)
		}
		if item.CreditorBIC != "" && item.CreditorAccount != "" {
			return fmt.Errorf("%w: :57a: carries both :57A: and :57D:", ErrInvalidMT101)
		}
		if item.CreditorBIC == "" && item.CreditorAccount == "" {
			return fmt.Errorf("%w: missing :57a: field", ErrInvalidMT101)
		}
		if item.CreditorBIC != "" && !validBIC(item.CreditorBIC) {
			return fmt.Errorf("%w: :57A: %q", ErrInvalidMT101, item.CreditorBIC)
		}
		if item.CreditorAccount != "" {
			if err := mt103CheckX(item.CreditorAccount); err != nil {
				return fmt.Errorf("%w: :57D: %w", ErrInvalidMT101, err)
			}
		}
		switch item.ChargeBearer {
		case "OUR", "BEN", "SHA":
			// Valid charge bearers (see the package comment).
		default:
			return fmt.Errorf("%w: :71A: %q: want OUR, BEN, or SHA", ErrInvalidMT101, item.ChargeBearer)
		}
	}
	return nil
}

// parkMT101Item parks one parsed MT101 transfer that failed
// closed, and returns the repair record ID. The snapshot's ID is
// the transfer's idempotency key, so the parked item is
// traceable to the exact transfer it refused; its Maker is
// deliberately empty (see the pain.001 inbound package comment).
func (l *Ledger) parkMT101Item(d MT101Transfer, msgID, actor string) (string, error) {
	rec, err := l.EnqueueRepair(PayoutInstruction{
		ID:          "mt101-" + msgID + "-" + d.TransferID,
		AmountCents: d.AmountCents,
		Currency:    d.Currency,
		// Maker intentionally empty: see the package comment.
		Destination: "mt101-inbound:" + msgID,
	}, RepairReasonInvalidFormat, actor)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// applyMT101Item books one parsed transfer through PostTransfer
// and records the per-transfer outcome.
//
// The transfer books through PostTransfer, so all of its gates
// apply — field validation (same-party), idempotency replay,
// frozen/overdraft, daily limit, accounting period, denylist,
// dual-control review — and a refusal parks the transfer in the
// repair queue. The journal entry ID is the :21: and the
// idempotency key is "mt101-<MsgId>-<:21:>", so the customer
// can dedupe on it exactly like an exported transfer. The
// transfer is single-currency by construction: ToCurrency stays
// empty, so a cross-currency payment can never be booked as one
// MT101 instruction.
func (l *Ledger) applyMT101Item(d MT101Transfer, from, to AccountID, msgID, actor string) MT101ItemResult {
	res := MT101ItemResult{Instruction: d, From: from, To: to}
	receipt, err := l.PostTransfer(Transfer{
		ID:             d.TransferID,
		From:           from,
		To:             to,
		AmountCents:    d.AmountCents,
		Currency:       d.Currency,
		IdempotencyKey: "mt101-" + msgID + "-" + d.TransferID,
	})
	if err != nil {
		res.Err = err
		repairID, parkErr := l.parkMT101Item(d, msgID, actor)
		if parkErr != nil {
			res.Err = fmt.Errorf("%w (repair parking failed: %v)", err, parkErr)
			return res
		}
		res.RepairID = repairID
		return res
	}
	res.Receipt = receipt
	res.EntryID = receipt.TransferID
	if len(receipt.Entries) > 0 {
		res.EntryID = receipt.Entries[0].ID
	}
	res.Duplicate = receipt.Duplicate
	res.ReviewStatus = receipt.ReviewStatus
	return res
}

// ApplyMT101 books each parsed inbound MT101 request for
// transfer: it moves each instructed amount from the debtor
// account the :50H: IBAN resolves to, to the creditor account
// the :59: IBAN resolves to, through PostTransfer, with the
// transfer's :21: as both the journal entry ID and the
// idempotency key's tail.
//
// The whole document is validated before anything applies; the
// debtor and every creditor resolve, and the :52A:/:57a: BICs
// agree with the registry, before any transfer books — a
// document naming an unknown account or a mismatched BIC books
// nothing. Then each transfer applies on its own. A
// per-transfer failure — any PostTransfer refusal (same-party,
// frozen/overdraft, daily limit, accounting period, denylist,
// dual-control review) — is recorded in that transfer's result
// and parked in the repair queue, and never blocks the other
// transfers.
//
// Each booked transfer emits one `mt101_transfer_booked` audit
// event (details msg_id / transfer_id / amount / currency). A
// document that books nothing emits no booked event: the repair
// records are the trail.
//
// Idempotency is the :20: plus :21: with its terms: re-applying
// the same :20:+:21: with the same terms returns the stored
// outcome with Duplicate set, booking nothing new and emitting
// no second audit event. A :20:+:21: reused for different terms
// is refused with ErrMT101MsgIDConflict.
//
// Locking: the structural validation runs on the parsed values
// alone; the account resolution and the replay read run under
// the read lock; each transfer's booking takes the write lock
// itself inside PostTransfer; the repair parking runs outside
// any held lock because EnqueueRepair takes l.mu itself; the
// :20:+:21: records and the inbound audit events run under one
// short write lock of their own, after every booking is durable
// — re-checking the registry first so a concurrent double-apply
// still records once.
func (l *Ledger) ApplyMT101(doc MT101Document, actor string) (MT101ApplyResult, error) {
	if actor == "" {
		actor = "ApplyMT101"
	}
	if err := validateMT101Document(doc); err != nil {
		return MT101ApplyResult{}, err
	}

	// The debtor and every creditor resolve, and the :52A:/:57a:
	// BICs agree with the registry, before anything books: a
	// document naming an unknown account or a mismatched BIC
	// books nothing. No account is ever opened from a wire
	// identifier.
	l.mu.RLock()
	from, fromOK := l.accountByIBANLocked(doc.DebtorIBAN)
	toByItem := make([]AccountID, len(doc.Items))
	for i, item := range doc.Items {
		to, ok := l.accountByIBANLocked(item.CreditorIBAN)
		if !ok {
			l.mu.RUnlock()
			return MT101ApplyResult{}, fmt.Errorf("%w: creditor IBAN %q", ErrMT101UnknownAccount, item.CreditorIBAN)
		}
		toByItem[i] = to
	}
	if !fromOK {
		l.mu.RUnlock()
		return MT101ApplyResult{}, fmt.Errorf("%w: debtor IBAN %q", ErrMT101UnknownAccount, doc.DebtorIBAN)
	}
	debtorReg := l.accountIBANs[from]
	creditorRegs := make([]AccountIBAN, len(doc.Items))
	for i := range doc.Items {
		creditorRegs[i] = l.accountIBANs[toByItem[i]]
	}
	l.mu.RUnlock()
	if doc.DebtorBIC != "" {
		if debtorReg.BIC == "" || strings.ToUpper(debtorReg.BIC) != doc.DebtorBIC {
			return MT101ApplyResult{}, fmt.Errorf("%w: :52A: %q does not match the debtor account's registered BIC", ErrInvalidMT101, doc.DebtorBIC)
		}
	}
	for i, item := range doc.Items {
		if item.CreditorBIC != "" {
			if creditorRegs[i].BIC == "" || strings.ToUpper(creditorRegs[i].BIC) != item.CreditorBIC {
				return MT101ApplyResult{}, fmt.Errorf("%w: :57A: %q does not match the creditor account's registered BIC", ErrInvalidMT101, item.CreditorBIC)
			}
		}
		if item.CreditorAccount != "" && string(toByItem[i]) != item.CreditorAccount {
			return MT101ApplyResult{}, fmt.Errorf("%w: :57D: %q does not name the creditor account", ErrInvalidMT101, item.CreditorAccount)
		}
	}

	// :20:+:21: replay: a transfer already applied under this
	// key. Same terms is a no-op returning the stored outcome;
	// different terms is a conflict — checked for every item
	// before anything books.
	results := make([]MT101ItemResult, len(doc.Items))
	replayed := make([]bool, len(doc.Items))
	l.mu.RLock()
	for i, item := range doc.Items {
		if rec, ok := l.mt101Applied[mt101AppliedKey(doc.MsgID, item.TransferID)]; ok {
			if !mt101TermsEqual(rec, doc, item) {
				l.mu.RUnlock()
				return MT101ApplyResult{}, fmt.Errorf("%w: :20: %q :21: %q", ErrMT101MsgIDConflict, doc.MsgID, item.TransferID)
			}
			results[i] = rec.Result
			results[i].Duplicate = true
			replayed[i] = true
		}
	}
	l.mu.RUnlock()

	// Each fresh transfer applies on its own: a refusal parks
	// that transfer and never blocks the others.
	for i, item := range doc.Items {
		if replayed[i] {
			continue
		}
		results[i] = l.applyMT101Item(item, from, toByItem[i], doc.MsgID, actor)
	}

	// The :20:+:21: records and the per-transfer booked events,
	// under one short write lock: every booking is already
	// durable when the marker lands. A concurrent double-apply
	// that landed first returns the stored outcome instead of a
	// second marker.
	allDuplicate := len(results) > 0
	l.mu.Lock()
	if l.mt101Applied == nil {
		l.mt101Applied = make(map[string]mt101AppliedRecord)
	}
	for i, item := range doc.Items {
		if replayed[i] {
			continue
		}
		key := mt101AppliedKey(doc.MsgID, item.TransferID)
		if rec, ok := l.mt101Applied[key]; ok {
			if !mt101TermsEqual(rec, doc, item) {
				l.mu.Unlock()
				return MT101ApplyResult{}, fmt.Errorf("%w: :20: %q :21: %q", ErrMT101MsgIDConflict, doc.MsgID, item.TransferID)
			}
			// The stored record is the outcome: no refresh
			// here — the write lock is held, and a refresh
			// would take the read lock.
			results[i] = rec.Result
			results[i].Duplicate = true
			replayed[i] = true
			continue
		}
		l.mt101Applied[key] = mt101AppliedRecord{
			MsgID:      doc.MsgID,
			DebtorIBAN: doc.DebtorIBAN,
			ValueDate:  doc.ValueDate,
			Transfer:   item,
			Result:     results[i],
		}
		if results[i].Err == nil {
			l.emitAudit(AuditEvent{
				Op:            "mt101_transfer_booked",
				Actor:         actor,
				TraceID:       doc.MsgID,
				VersionBefore: l.version,
				VersionAfter:  l.version,
				Accounts:      []AccountID{from, toByItem[i]},
				Details: map[string]any{
					"msg_id":      doc.MsgID,
					"transfer_id": item.TransferID,
					"amount":      item.AmountCents,
					"currency":    item.Currency,
				},
			})
		}
	}
	l.mu.Unlock()
	for _, dup := range replayed {
		if !dup {
			allDuplicate = false
			break
		}
	}
	return MT101ApplyResult{MsgID: doc.MsgID, Items: results, Duplicate: allDuplicate}, nil
}
