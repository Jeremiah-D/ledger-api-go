package ledger

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// SWIFT FIN MT103 single customer credit transfer OUTBOUND generation
// (LG-183, fintech: SWIFT 跨境汇款).
//
// ExportMT103 renders an approved or submitted payout instruction as a
// SWIFT FIN MT103 single customer credit transfer — the wire
// instruction the ledger's bank would hand to its correspondent: block
// 1 (basic header) names the sender's LT address, block 2
// (application header) the receiver's, block 3 carries the Message User
// Reference, and block 4 the field set :20: / :23B: / :32A: / :50K: /
// :52A: / :57A: / :59: / :70: / :71A:.
//
// This is the SWIFT FIN twin of the ISO 20022 pacs.008 outbound (see
// pacs008_export.go, LG-176): that one speaks to the FedNow
// instant-payment rail in XML; this one speaks to the correspondent
// banking network in FIN. The two are complementary, never
// overlapping — pacs.008 is USD-only FedNow, MT103 is multi-currency
// SWIFT. Together with nacha.go (ACH, LG-157) and pain001.go (SEPA,
// LG-150) the ledger now covers the domestic/regional outbound
// channels plus SWIFT cross-border.
//
// Identity wiring — scope honesty, the ledger is not a SWIFT member:
//   - The sender's BIC comes from MT103ExportOptions.SenderBIC, or
//     falls back to the payee account's registered IBAN BIC (see
//     SetAccountIBAN, LG-150): the operator registers the bank's own
//     BIC once, instead of the export inventing one. No BIC anywhere
//     is a fail-closed refusal, not a guessed value.
//   - The receiver's LT address is always operator-supplied (the
//     correspondent the message is addressed to); the ledger keeps no
//     routing table, so it never picks a correspondent on its own.
//   - :52A: (ordering institution) and :57A: (account with
//     institution, the beneficiary's bank) are opt-in per-export: the
//     direct-credit path omits them, the correspondent path names
//     them.
//
// Lifecycle honesty: the MT103 is the instruction the bank sends
// before settlement, so the payout must be approved or submitted —
// the checker authorized it and/or the submit leg booked it. Draft
// (not yet authorized), failed, cancelled, and settled (the money
// already moved — a second instruction would double-pay) are all
// refused fail-closed. The settled-side mirror is the pacs.004 return
// (LG-169): that message reports money that came back; this one moves
// money out.
//
// Field discipline is fail-loud, the nacha.go convention: every text
// field must fit the SWIFT `x` character set (see mt103XCharset) and
// its length; over-length or non-`x` input is an error, never silently
// truncated — a payment instruction must not lose digits or rename
// payees quietly. Braces are additionally refused in block-4 field
// content: `{`/`}` would break the FIN block framing even though they
// are technically in the `x` set.
//
// Wire honesty notes:
//   - Block 3 carries `{108:<MUR>}` with the :20: reference, tying the
//     message back to the payout (and through it to the gpi UETR —
//     MT103 has no UETR field of its own, so the :20: payout ID is the
//     ledger's join key back to the gpi track).
//   - Block 5 (trailer) is omitted: CHK is computed by the SWIFT
//     network on receipt; a generator emitting a fake checksum would
//     be dishonest.
//   - Block 1 session/sequence are zero-filled: they are assigned by
//     the sender's SWIFT interface on transmission, and the export
//     documents them as interface-assigned rather than inventing
//     plausible numbers.
//   - Lines are joined with "\n", the nacha.go convention; convert to
//     CRLF for the bank if the uploader requires it.
//
// Read-only: the payout and the IBAN registry are read as snapshot
// copies under the read lock — no version bump, no journal rows, no
// audit event — the same contract as ExportPacs008 and ExportPacs004.
// The export is deterministic per payout: repeat exports render byte-
// identical output, a statement of fact, not an attempt sequence.

// MT103 outbound export errors.
var (
	// ErrMT103PayoutNotFound is returned by ExportMT103 for a payout
	// ID the ledger never tracked. Fail-closed, like the gpi query
	// path.
	ErrMT103PayoutNotFound = errors.New("ledger: mt103 export: unknown payout")
	// ErrMT103BadStatus is returned by ExportMT103 when the payout is
	// not approved or submitted: a draft is not yet authorized, and a
	// failed, cancelled, or settled payout must never produce a
	// second payment instruction.
	ErrMT103BadStatus = errors.New("ledger: mt103 export: payout is not approved or submitted")
	// ErrMT103NoSenderBIC is returned by ExportMT103 when neither
	// MT103ExportOptions.SenderBIC nor the payee account's registered
	// IBAN BIC names the sender's bank: a SWIFT instruction without a
	// sender is refused, never guessed.
	ErrMT103NoSenderBIC = errors.New("ledger: mt103 export: sender BIC is not configured")
	// ErrMT103LT is returned for an invalid SWIFT LT address (not a
	// valid BIC8/BIC11/BIC12 form).
	ErrMT103LT = errors.New("ledger: mt103 export: invalid SWIFT LT address")
	// ErrMT103Reference is returned when the :20: sender's reference
	// (the payout ID) is empty, over 16 characters, or carries a
	// leading/trailing colon or slash, or a double slash.
	ErrMT103Reference = errors.New("ledger: mt103 export: invalid :20: sender's reference")
	// ErrMT103Charset is returned when a text field carries a
	// character outside the SWIFT `x` character set (or a brace in
	// block-4 content). The caller must transliterate first.
	ErrMT103Charset = errors.New("ledger: mt103 export: field is not in the SWIFT x character set")
	// ErrMT103Amount is returned for a non-positive amount or one
	// that does not fit the :32A: 15d field.
	ErrMT103Amount = errors.New("ledger: mt103 export: invalid :32A: amount")
	// ErrMT103Config is returned for an invalid export option: an
	// unknown bank operation code or details-of-charges code, an
	// over-length field, or too many name/remittance lines.
	ErrMT103Config = errors.New("ledger: mt103 export: invalid option")
)

// MT103 bank operation codes (:23B:), the admitted vocabulary — a
// real SWIFT code, never invented.
var mt103BankOperationCodes = map[string]bool{
	"CRED": true, // standard credit transfer
	"SPAY": true, // SWIFT pay — urgent/priority handling
	"SSTD": true, // standard
	"SPRI": true, // SWIFT priority
}

// MT103 details-of-charges codes (:71A:), the admitted vocabulary.
var mt103DetailsOfCharges = map[string]bool{
	"OUR": true, // all charges borne by the ordering customer
	"BEN": true, // all charges borne by the beneficiary
	"SHA": true, // charges shared
}

// MT103 field length limits (SWIFT network validated rules, single
// customer credit transfer).
const (
	mt103MaxSenderReference = 16 // :20:
	mt103MaxAccountLine     = 34 // :50K:/:59: account line
	mt103MaxNameLine        = 35 // :50K:/:59: name/address lines
	mt103MaxNameLines       = 3  // plus the account line
	mt103MaxRemitLine       = 35 // :70:
	mt103MaxRemitLines      = 4
	mt103MaxAmountDigits    = 12 // :32A: 15d = 12 integer + comma + 2 fraction
)

// mt103XExtra holds the SWIFT `x` character set members beyond
// letters, digits, and space. The `x` set is: a-z A-Z 0-9 plus
// "/-?:().,'+" CR LF and "*=!"%_#&$@<>{}". Braces are in the set but
// are refused separately in block-4 content (see mt103NoBraces).
const mt103XExtra = "/-?:().,'+*=!\"%_#&$@<>{}\r\n"

// mt103IsXChar reports whether c is in the SWIFT `x` character set.
func mt103IsXChar(c byte) bool {
	if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == ' ' {
		return true
	}
	return strings.IndexByte(mt103XExtra, c) >= 0
}

// mt103CheckX validates that s uses only SWIFT `x` characters and
// carries no braces (field content; see the package doc). The empty
// string is valid — emptiness is a separate option-level decision.
func mt103CheckX(s string) error {
	for i := 0; i < len(s); i++ {
		if !mt103IsXChar(s[i]) {
			return fmt.Errorf("%w: %q", ErrMT103Charset, s)
		}
		if s[i] == '{' || s[i] == '}' {
			return fmt.Errorf("%w: braces would break FIN framing: %q", ErrMT103Charset, s)
		}
	}
	return nil
}

// mt103NormalizeLT normalizes a SWIFT BIC (8 or 11 characters) or a
// full LT address (12 characters) to the 12-character LT address used
// in blocks 1 and 2: BIC8 → BIC8+"XXX", BIC11 → BIC11+"X".
func mt103NormalizeLT(raw string) (string, error) {
	s := strings.ToUpper(strings.TrimSpace(raw))
	switch len(s) {
	case 8:
		s += "XXX"
	case 11:
		s += "X"
	case 12:
		// Already an LT address.
	default:
		return "", fmt.Errorf("%w: %q", ErrMT103LT, raw)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return "", fmt.Errorf("%w: %q", ErrMT103LT, raw)
		}
	}
	return s, nil
}

// mt103Amount formats cents as the :32A: 15d amount: up to 12 integer
// digits, a comma decimal separator, two fraction digits — e.g.
// "2500,00". Amounts over 12 integer digits are refused rather than
// rounded: a payment instruction must not lose value quietly.
func mt103Amount(cents int64) (string, error) {
	if cents <= 0 {
		return "", fmt.Errorf("%w: %d", ErrMT103Amount, cents)
	}
	whole, frac := cents/100, cents%100
	if whole > 999999999999 {
		return "", fmt.Errorf("%w: %d cents exceeds :32A: 15d", ErrMT103Amount, cents)
	}
	return fmt.Sprintf("%d,%02d", whole, frac), nil
}

// mt103ValueDate formats t as the :32A: YYMMDD value date.
func mt103ValueDate(t time.Time) string {
	return t.UTC().Format("060102")
}

// MT103ExportOptions tunes ExportMT103.
//
// PayoutID is required: the approved or submitted payout to render.
// SenderBIC is optional: the sender's BIC, defaulting to the payee
// account's registered IBAN BIC (see SetAccountIBAN). Missing
// everywhere is a fail-closed refusal — unless SenderLT is passed,
// which names the sender on its own.
// SenderLT is optional: the sender's own 12-character LT address for
// block 1 (BIC8/11 accepted, normalized); it defaults from SenderBIC.
// ReceiverLT is required: the correspondent's 12-character LT address
// for block 2 (BIC8/11 accepted, normalized). The ledger keeps no
// routing table, so the operator always names the correspondent
// explicitly.
// BankOperationCode is optional: CRED/SPAY/SSTD/SPRI, default CRED.
// OrderingCustomerName holds up to 3 name/address lines for :50K:,
// defaulting to the payee account ID alone.
// OrderingInstitutionBIC is optional: :52A:, the ordering
// institution's BIC.
// BeneficiaryInstitutionBIC is optional: :57A:, the beneficiary's
// bank BIC (the correspondent path).
// BeneficiaryAccount is optional: :59: account line, defaulting to
// the payout destination with any "<scheme>:" prefix stripped.
// BeneficiaryName holds up to 3 name lines for :59:.
// RemittanceInfo is optional: :70:, newline-separated, up to 4 lines.
// DetailsOfCharges is optional: OUR/BEN/SHA, default SHA.
// ValueDate is optional: the :32A: value date, defaulting to the
// payout's submit time, then approval time, then creation time — the
// instruction date, which the bank may re-date on actual settlement.
type MT103ExportOptions struct {
	PayoutID                 string
	SenderBIC                string
	SenderLT                 string
	ReceiverLT               string
	BankOperationCode        string
	OrderingCustomerName     []string
	OrderingInstitutionBIC   string
	BeneficiaryInstitutionBIC string
	BeneficiaryAccount       string
	BeneficiaryName          []string
	RemittanceInfo           string
	DetailsOfCharges         string
	ValueDate                time.Time
}

// mt103PartyLines validates up to mt103MaxNameLines lines of at most
// mt103MaxNameLine `x` characters.
func mt103PartyLines(lines []string, field string) error {
	if len(lines) > mt103MaxNameLines {
		return fmt.Errorf("%w: %s: at most %d lines", ErrMT103Config, field, mt103MaxNameLines)
	}
	for _, ln := range lines {
		if len(ln) > mt103MaxNameLine {
			return fmt.Errorf("%w: %s line exceeds %d characters: %q", ErrMT103Config, field, mt103MaxNameLine, ln)
		}
		if err := mt103CheckX(ln); err != nil {
			return fmt.Errorf("%s %s: %w", field, "line", err)
		}
	}
	return nil
}

// mt103SenderReference validates the :20: sender's reference: 1–16 `x`
// characters, no leading/trailing colon or slash, no double slash.
func mt103SenderReference(ref string) error {
	if ref == "" || len(ref) > mt103MaxSenderReference {
		return fmt.Errorf("%w: want 1-%d characters, got %q", ErrMT103Reference, mt103MaxSenderReference, ref)
	}
	if err := mt103CheckX(ref); err != nil {
		return fmt.Errorf("%w: %w", ErrMT103Reference, err)
	}
	if strings.HasPrefix(ref, ":") || strings.HasPrefix(ref, "/") ||
		strings.HasSuffix(ref, ":") || strings.HasSuffix(ref, "/") ||
		strings.Contains(ref, "//") {
		return fmt.Errorf("%w: leading/trailing colon or slash, or double slash: %q", ErrMT103Reference, ref)
	}
	return nil
}

// mt103BICField validates an optional BIC field (:52A:/:57A:): empty
// is allowed (the field is omitted), otherwise a valid ISO 9362 code.
func mt103BICField(bic, field string) error {
	if bic == "" {
		return nil
	}
	if !validBIC(bic) {
		return fmt.Errorf("%w: %s: %q", ErrMT103Config, field, bic)
	}
	return nil
}

// ExportMT103 renders the payout as a SWIFT FIN MT103 single customer
// credit transfer message, the outbound cross-border instruction. See
// the package doc for the identity wiring, lifecycle rules, and wire
// honesty notes.
//
// The returned string is the FIN text with blocks 1–4, lines joined
// with "\n", no trailing newline (the nacha.go convention). The
// export is read-only and deterministic per payout: repeat exports
// render byte-identical output.
func (l *Ledger) ExportMT103(payoutID string, opts MT103ExportOptions) (string, error) {
	if payoutID == "" {
		return "", fmt.Errorf("%w: %q", ErrMT103PayoutNotFound, payoutID)
	}
	l.mu.RLock()
	p, ok := l.payouts[payoutID]
	if !ok {
		l.mu.RUnlock()
		return "", fmt.Errorf("%w: %q", ErrMT103PayoutNotFound, payoutID)
	}
	var senderBIC string
	if reg, ok := l.accountIBANs[p.PayeeAccount]; ok {
		senderBIC = reg.BIC
	}
	l.mu.RUnlock()
	// p and senderBIC are value copies out of the maps: the render
	// below cannot mutate ledger state even by accident.

	if p.Status != PayoutApproved && p.Status != PayoutSubmitted {
		return "", fmt.Errorf("%w: payout %q is %s", ErrMT103BadStatus, p.ID, p.Status)
	}

	// Sender BIC: explicit option wins, then the payee account's
	// registered IBAN BIC. A SWIFT instruction without a sender is
	// refused, never guessed — unless the operator passed a full
	// SenderLT address, which names the sender on its own.
	if opts.SenderBIC != "" {
		if !validBIC(opts.SenderBIC) {
			return "", fmt.Errorf("%w: invalid SenderBIC %q", ErrMT103Config, opts.SenderBIC)
		}
		senderBIC = strings.ToUpper(opts.SenderBIC)
	}
	if senderBIC == "" && opts.SenderLT == "" {
		return "", fmt.Errorf("%w: register the payee account's BIC (SetAccountIBAN) or pass SenderBIC/SenderLT", ErrMT103NoSenderBIC)
	}
	senderLT, err := mt103NormalizeLT(firstNonEmpty(opts.SenderLT, senderBIC))
	if err != nil {
		return "", err
	}
	receiverLT, err := mt103NormalizeLT(opts.ReceiverLT)
	if err != nil {
		return "", fmt.Errorf("receiver: %w", err)
	}
	if opts.ReceiverLT == "" {
		return "", fmt.Errorf("%w: ReceiverLT is required", ErrMT103Config)
	}

	bankOp := opts.BankOperationCode
	if bankOp == "" {
		bankOp = "CRED"
	}
	if !mt103BankOperationCodes[bankOp] {
		return "", fmt.Errorf("%w: unknown bank operation code %q", ErrMT103Config, bankOp)
	}
	charges := opts.DetailsOfCharges
	if charges == "" {
		charges = "SHA"
	}
	if !mt103DetailsOfCharges[charges] {
		return "", fmt.Errorf("%w: unknown details of charges %q", ErrMT103Config, charges)
	}

	// :20: — the payout ID is the sender's reference and the MUR.
	if err := mt103SenderReference(p.ID); err != nil {
		return "", err
	}

	amount, err := mt103Amount(p.AmountCents)
	if err != nil {
		return "", err
	}
	if p.Currency == "" {
		return "", fmt.Errorf("%w: payout %q has no currency", ErrMT103Config, p.ID)
	}
	valueDate := opts.ValueDate
	if valueDate.IsZero() {
		valueDate = p.SubmittedAt
		if valueDate.IsZero() {
			valueDate = p.ApprovedAt
		}
		if valueDate.IsZero() {
			valueDate = p.CreatedAt
		}
	}

	if err := mt103PartyLines(opts.OrderingCustomerName, ":50K:"); err != nil {
		return "", err
	}
	if err := mt103BICField(opts.OrderingInstitutionBIC, ":52A:"); err != nil {
		return "", err
	}
	if err := mt103BICField(opts.BeneficiaryInstitutionBIC, ":57A:"); err != nil {
		return "", err
	}

	// :59: beneficiary account: explicit option wins, then the payout
	// destination with any "<scheme>:" prefix stripped ("FPS:..." →
	// the external account reference).
	beneficiaryAccount := opts.BeneficiaryAccount
	if beneficiaryAccount == "" {
		beneficiaryAccount = p.Destination
		if i := strings.Index(beneficiaryAccount, ":"); i >= 0 {
			beneficiaryAccount = beneficiaryAccount[i+1:]
		}
	}
	if beneficiaryAccount == "" {
		return "", fmt.Errorf("%w: :59: beneficiary account is empty", ErrMT103Config)
	}
	if len(beneficiaryAccount) > mt103MaxAccountLine {
		return "", fmt.Errorf("%w: :59: account exceeds %d characters", ErrMT103Config, mt103MaxAccountLine)
	}
	if err := mt103CheckX(beneficiaryAccount); err != nil {
		return "", fmt.Errorf(":59: account: %w", err)
	}
	if err := mt103PartyLines(opts.BeneficiaryName, ":59:"); err != nil {
		return "", err
	}

	// :50K: account line: the payee (ordering customer) account.
	if len(string(p.PayeeAccount)) > mt103MaxAccountLine {
		return "", fmt.Errorf("%w: :50K: account exceeds %d characters", ErrMT103Config, mt103MaxAccountLine)
	}
	if err := mt103CheckX(string(p.PayeeAccount)); err != nil {
		return "", fmt.Errorf(":50K: account: %w", err)
	}

	// :70: remittance: newline-separated, up to 4 lines of 35 `x`.
	var remitLines []string
	if opts.RemittanceInfo != "" {
		remitLines = strings.Split(opts.RemittanceInfo, "\n")
		if len(remitLines) > mt103MaxRemitLines {
			return "", fmt.Errorf("%w: :70: at most %d lines", ErrMT103Config, mt103MaxRemitLines)
		}
		for _, ln := range remitLines {
			if len(ln) > mt103MaxRemitLine {
				return "", fmt.Errorf("%w: :70: line exceeds %d characters: %q", ErrMT103Config, mt103MaxRemitLine, ln)
			}
			if err := mt103CheckX(ln); err != nil {
				return "", fmt.Errorf(":70: line: %w", err)
			}
		}
	}

	var b strings.Builder
	// Block 1: basic header. Session/sequence are zero-filled —
	// assigned by the sender's SWIFT interface on transmission.
	fmt.Fprintf(&b, "{1:F01%s0000000000}\n", senderLT)
	// Block 2: application header, input to SWIFT. Priority U
	// (urgent), delivery monitoring 3 (non-delivery warning and
	// delivery notification), obsolescence period 003 — the common
	// MT103 application header.
	fmt.Fprintf(&b, "{2:I103%sU3003}\n", receiverLT)
	// Block 3: user header — the Message User Reference ties the
	// message back to the payout.
	fmt.Fprintf(&b, "{3:{108:%s}}\n", p.ID)
	// Block 4: text.
	b.WriteString("{4:\n")
	fmt.Fprintf(&b, ":20:%s\n", p.ID)
	fmt.Fprintf(&b, ":23B:%s\n", bankOp)
	fmt.Fprintf(&b, ":32A:%s%s%s\n", mt103ValueDate(valueDate), p.Currency, amount)
	fmt.Fprintf(&b, ":50K:/%s\n", p.PayeeAccount)
	for _, ln := range opts.OrderingCustomerName {
		fmt.Fprintf(&b, "%s\n", ln)
	}
	if opts.OrderingInstitutionBIC != "" {
		fmt.Fprintf(&b, ":52A:%s\n", strings.ToUpper(opts.OrderingInstitutionBIC))
	}
	if opts.BeneficiaryInstitutionBIC != "" {
		fmt.Fprintf(&b, ":57A:%s\n", strings.ToUpper(opts.BeneficiaryInstitutionBIC))
	}
	fmt.Fprintf(&b, ":59:/%s\n", beneficiaryAccount)
	for _, ln := range opts.BeneficiaryName {
		fmt.Fprintf(&b, "%s\n", ln)
	}
	if len(remitLines) > 0 {
		b.WriteString(":70:")
		b.WriteString(strings.Join(remitLines, "\n"))
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, ":71A:%s\n", charges)
	b.WriteString("-}")
	return b.String(), nil
}

// firstNonEmpty returns the first non-empty string.
func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
