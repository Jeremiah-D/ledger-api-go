package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SWIFT FIN MT101 customer request-for-transfer export (LG-391,
// fintech: SWIFT 客户转账请求报文).
//
// pain.001 (ExportPain001, pain001.go) is the ISO 20022 form of a
// customer credit transfer initiation; the MT101 is the same
// instruction in FIN form — SWIFT Category 1, "Request for
// Transfer", sent by a financial institution on behalf of a
// non-financial account owner to the account-servicing bank,
// instructing it to debit the owner's account and transfer the
// funds. The two exports share one source (the registered
// PainTransfer principals) and one filter vocabulary; the MT101
// carries the payments as repeated sequence B blocks, one per
// transfer.
//
// Sequence A identifies the instruction (:15A: :20: :28D: :50H:
// :52A: :30:). :20: is a deterministic derivation — "MT101-" plus
// the first 10 lowercase hex characters of the SHA-256 of the
// window, the filters, the exported transfer IDs, and the
// execution date — 16 characters, 16x legal, so the same execution
// day re-renders byte-identical and any content change changes the
// reference. :28D: is always "1/1" (a single message). :50H: is
// the debtor as "/<debtor IBAN>" — account line only, no name line:
// the ledger holds no legal holder name, and only evidenced data
// is written. :52A: is the debtor's registered BIC when one
// exists, omitted otherwise. :30: is the execution date
// (ExecuteOn, defaulting to the export day UTC): the date the
// account-servicing bank is asked to execute.
//
// Sequence B repeats per payment (:15B: :21: :32B: :57a: :59:
// :71A:). :21: is the TransferID, 16x-validated fail-loud (a
// TransferID is caller-given free text, so an unrenderable one
// refuses the whole export rather than emitting a malformed
// field). :32B: is the currency plus the amount in cents, comma
// decimal, two fraction digits (see mt940Amount). :57a: is the
// creditor's registered BIC (:57A:) when one exists, else :57D:
// with the To account ID as the name (which must pass
// mt103CheckX). :59: is the beneficiary as "/<creditor IBAN>".
// :71A: is the caller-given charge bearer (OUR/BEN/SHA, default
// SHAR — the pain.001 default). Sequence B carries no :50a: (it
// would contradict sequence A's :50H:), no :33B:/:36: (a single
// currency transfer has no exchange leg), and no :70: (a
// PainTransfer has no memo field); :23E:/:77B: are likewise
// omitted.
//
// Semantics shared with ExportPain001: the Debtor/Creditor/
// Currency/Since/Until filter vocabulary and the half-open
// [Since, Until) window; pending-review transfers are excluded
// (their rows never touched balances, so an instruction must not
// be generated from them); a debtor or creditor with no registered
// IBAN refuses the whole export with ErrMissingAccountIBAN rather
// than emitting a forged identifier. Zero payments is an explicit
// refusal (ErrInvalidMT101Export): an MT101 with no sequence B is
// not an instruction a bank can act on — unlike renderPain001,
// which renders an empty GrpHdr document.
//
// The export is read-only, like Reconcile: it takes the read lock,
// bumps no version, books nothing, appends no chain link, and
// consumes no idempotency key — but it still emits one structured
// audit event ("mt101_exported", details msg_id/tx_count/
// ctrl_sum_cents/currency) so the compliance trail records every
// generated instruction. It does not register into the pain.001
// instruction lifecycle (see registerPainInstructionLocked): no
// pain.002 can answer an MT101, so instruction state tracking
// stays on the ISO side.
//
// Framing follows the MT300 convention (see mt300.go): blocks
// 1/2/3/4, block 3 `{108:MUR}` carrying the :20: reference, no
// block 5, lines joined with "\n", no trailing newline. Both
// header LTs derive from the debtor side — the ledger holds no
// separate instructing-party/account-servicing-bank identity, so
// block 1 takes the debtor's identifier and block 2 the debtor's
// registered servicing-bank BIC when one exists, else the same
// debtor identifier.
//
// A note on scope, stated plainly in the README too: cross-border
// payment instructions completed the MT→MX migration on
// 2025-11-22, so the MT101 serves counterparties and scenarios
// that still accept the FIN form; it is not presented as the
// default channel for a new cross-border onboarding.

// ErrInvalidMT101Export is returned when a transfer set cannot be
// rendered as an MT101: no debtor, an unknown charge bearer, zero
// payments, a TransferID outside 16x, an account line over the
// 34x limit, a :57D: name outside the SWIFT `x` character set, or
// a control sum that overflows int64. It is a 400-class error:
// the request cannot be expressed as a valid instruction.
var ErrInvalidMT101Export = errors.New("ledger: invalid MT101 export")

// MT101ExportOptions tunes ExportMT101.
//
// Debtor is required: sequence A :50H: holds a single debit
// account. Creditor and Currency optionally filter by the
// transfer's payee and currency (empty means unfiltered). Since
// and Until bound the exported payments (half-open [Since,
// Until), the OFX export convention; zero means unbounded).
// ExecuteOn is the :30: execution date; zero defaults to the
// export day UTC. ChargeBearer is the :71A: charge bearer —
// OUR/BEN/SHA; empty defaults to SHAR.
type MT101ExportOptions struct {
	Debtor       AccountID
	Creditor     AccountID
	Currency     string
	Since        time.Time
	Until        time.Time
	ExecuteOn    time.Time
	ChargeBearer string
}

// ExportMT101 renders a SWIFT FIN MT101 request for transfer from
// the registered transfer principals (see PainTransfer). The
// return is the FIN text; the :20: reference is deterministic in
// (window, filters, exported content, execution date).
func (l *Ledger) ExportMT101(opts MT101ExportOptions) (string, error) {
	currency := ""
	if opts.Currency != "" {
		var cerr error
		currency, cerr = normalizeCurrency(opts.Currency)
		if cerr != nil {
			return "", cerr
		}
	}
	if !opts.Since.IsZero() && !opts.Until.IsZero() && opts.Since.After(opts.Until) {
		return "", ErrInvalidPain001Range
	}
	if opts.Debtor == "" {
		return "", fmt.Errorf("%w: debtor is required: sequence A :50H: holds a single debit account", ErrInvalidMT101Export)
	}
	chargeBearer := strings.ToUpper(strings.TrimSpace(opts.ChargeBearer))
	if chargeBearer == "" {
		chargeBearer = "SHA"
	}
	switch chargeBearer {
	case "OUR", "BEN", "SHA":
		// Valid charge bearers (see the package comment).
	default:
		return "", fmt.Errorf("%w: charge bearer %q: want OUR, BEN, or SHA", ErrInvalidMT101Export, opts.ChargeBearer)
	}
	executeOn := opts.ExecuteOn
	if executeOn.IsZero() {
		executeOn = time.Now().UTC()
	}

	l.mu.RLock()

	// The debtor's registered IBAN is the :50H: account line and
	// the source of the :52A: servicing-bank BIC; fail-closed
	// before any payment renders.
	debtorIBAN, ok := l.accountIBANs[opts.Debtor]
	if !ok {
		l.mu.RUnlock()
		return "", fmt.Errorf("%w: debtor %q", ErrMissingAccountIBAN, opts.Debtor)
	}
	if len("/"+debtorIBAN.IBAN) > mt103MaxAccountLine {
		l.mu.RUnlock()
		return "", fmt.Errorf("%w: :50H: account line %q exceeds %d characters", ErrInvalidMT101Export, "/"+debtorIBAN.IBAN, mt103MaxAccountLine)
	}

	// Both header LTs derive from the debtor side (see the package
	// comment): the sender LT is the debtor's best-known identifier
	// (the MT300 convention), the receiver LT the debtor's
	// registered servicing-bank BIC when one exists, else the same
	// debtor identifier. A debtor without a registered BIC needs an
	// LT-form account ID (8, 11, or 12 alphanumeric characters) —
	// anything else fails loud here rather than emitting a
	// malformed header.
	debtorID, _ := l.mt300PartyIdentifierLocked(opts.Debtor)
	senderLT, err := mt103NormalizeLT(debtorID)
	if err != nil {
		l.mu.RUnlock()
		return "", fmt.Errorf("%w: sender LT from %q: %w", ErrInvalidMT101Export, debtorID, err)
	}
	receiverLT := senderLT
	if validBIC(debtorIBAN.BIC) && debtorIBAN.BIC != "" {
		receiverLT, err = mt103NormalizeLT(strings.ToUpper(debtorIBAN.BIC))
		if err != nil {
			l.mu.RUnlock()
			return "", fmt.Errorf("%w: receiver LT from %q: %w", ErrInvalidMT101Export, debtorIBAN.BIC, err)
		}
	}

	var payments []pain001Payment
	for _, pt := range l.painTransfersLocked() {
		if pt.From != opts.Debtor {
			continue
		}
		if opts.Creditor != "" && pt.To != opts.Creditor {
			continue
		}
		if currency != "" && pt.Currency != currency {
			continue
		}
		if !opts.Since.IsZero() && pt.CreatedAt.Before(opts.Since) {
			continue
		}
		if !opts.Until.IsZero() && !pt.CreatedAt.Before(opts.Until) {
			continue
		}
		// Pending-review transfers never touched balances: a
		// payment instruction generated from one would order
		// money the ledger has not released. The pain.001 and
		// OFX exports apply the same rule.
		if e, ok := l.entries[pt.TransferID]; ok && e.PendingReview {
			continue
		}
		creditorIBAN, ok := l.accountIBANs[pt.To]
		if !ok {
			l.mu.RUnlock()
			return "", fmt.Errorf("%w: creditor %q", ErrMissingAccountIBAN, pt.To)
		}
		if len("/"+creditorIBAN.IBAN) > mt103MaxAccountLine {
			l.mu.RUnlock()
			return "", fmt.Errorf("%w: :59: account line %q exceeds %d characters", ErrInvalidMT101Export, "/"+creditorIBAN.IBAN, mt103MaxAccountLine)
		}
		payments = append(payments, pain001Payment{
			PainTransfer: pt,
			DebtorIBAN:   debtorIBAN,
			CreditorIBAN: creditorIBAN,
		})
	}
	// Zero payments is a refusal, not an empty instruction: an
	// MT101 with no sequence B is not an instruction a bank can
	// act on.
	if len(payments) == 0 {
		l.mu.RUnlock()
		return "", fmt.Errorf("%w: no payments match the export filters", ErrInvalidMT101Export)
	}
	// Every :21: must be a valid 16x reference, and every :57D:
	// name must be SWIFT `x` — validated before any rendering so
	// a bad row refuses the whole export.
	for _, p := range payments {
		if err := mt101TransactionReference(p.TransferID); err != nil {
			l.mu.RUnlock()
			return "", err
		}
		// A creditor without a registered BIC renders :57D: with
		// the To account ID as the name: it must be SWIFT `x`.
		if p.CreditorIBAN.BIC == "" {
			if err := mt103CheckX(string(p.To)); err != nil {
				l.mu.RUnlock()
				return "", fmt.Errorf("%w: :57D: account ID %q: %w", ErrInvalidMT101Export, string(p.To), err)
			}
		}
	}

	// Control sum over every exported instruction,
	// overflow-checked: a control total that wrapped would be
	// worse than no export.
	var ctrlSum int64
	for _, p := range payments {
		var ok bool
		ctrlSum, ok = addCents(ctrlSum, p.AmountCents)
		if !ok {
			l.mu.RUnlock()
			return "", fmt.Errorf("%w: control sum overflows int64", ErrInvalidMT101Export)
		}
	}

	msgID := mt101MessageReference(payments, opts, currency, executeOn)
	doc := renderMT101(payments, debtorIBAN, msgID, senderLT, receiverLT, executeOn, chargeBearer)

	details := map[string]any{
		"msg_id":         msgID,
		"tx_count":       len(payments),
		"ctrl_sum_cents": ctrlSum,
		"currency":       pain001DetailsCurrency(payments, currency),
	}
	if opts.Creditor != "" {
		details["creditor"] = string(opts.Creditor)
	}
	// Read-only, like Reconcile: the version brackets the export
	// but does not move — the audit event proves the log line was
	// written against a known ledger state.
	l.emitAudit(AuditEvent{
		Op:            "mt101_exported",
		Actor:         "ExportMT101",
		TraceID:       msgID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      pain001Accounts(payments),
		Details:       details,
	})
	l.mu.RUnlock()
	return doc, nil
}

// mt101MessageReference derives the deterministic :20: reference:
// "MT101-" plus the first 10 lowercase hex characters of the
// SHA-256 of the window, the filters, the exported transfer IDs,
// and the execution date — 16 characters, 16x legal. The same
// execution day re-renders byte-identical, and any content change
// changes the reference.
func mt101MessageReference(payments []pain001Payment, opts MT101ExportOptions, currency string, executeOn time.Time) string {
	h := sha256.New()
	h.Write([]byte("mt101|"))
	h.Write([]byte(pain001UnixOrZero(opts.Since)))
	h.Write([]byte("|"))
	h.Write([]byte(pain001UnixOrZero(opts.Until)))
	h.Write([]byte("|"))
	h.Write([]byte(string(opts.Debtor)))
	h.Write([]byte("|"))
	h.Write([]byte(pain001FilterOrAll(string(opts.Creditor))))
	h.Write([]byte("|"))
	h.Write([]byte(pain001FilterOrAll(currency)))
	h.Write([]byte("|"))
	h.Write([]byte(executeOn.UTC().Format("20060102")))
	for _, p := range payments {
		h.Write([]byte("|"))
		h.Write([]byte(p.TransferID))
	}
	return "MT101-" + hex.EncodeToString(h.Sum(nil))[:10]
}

// renderMT101 builds the FIN MT101 text for the resolved payments.
// debtorIBAN is the debtor's registered IBAN record (the :50H:
// account line and the :52A: servicing-bank BIC source); msgID is
// the deterministic :20: reference; senderLT/receiverLT are the
// validated block 1/2 LTs; executeOn the :30: execution date;
// chargeBearer the :71A: value.
func renderMT101(payments []pain001Payment, debtorIBAN AccountIBAN, msgID, senderLT, receiverLT string, executeOn time.Time, chargeBearer string) string {
	var b strings.Builder
	// Block 1: session/sequence zero-filled — assigned by the
	// sender's SWIFT interface on transmission (MT103 convention).
	fmt.Fprintf(&b, "{1:F01%s0000000000}\n", senderLT)
	// Block 2: application header, input to SWIFT, I101. Priority
	// U, delivery monitoring 3, obsolescence period 003 — the MT103
	// application header with the message type swapped.
	fmt.Fprintf(&b, "{2:I101%sU3003}\n", receiverLT)
	// Block 3: the Message User Reference carries the :20:
	// reference, the MT103 convention.
	fmt.Fprintf(&b, "{3:{108:%s}}\n", msgID)
	// Block 4: the field set, in canonical order. Sequence A
	// identifies the instruction; sequence B repeats per payment.
	b.WriteString("{4:\n")
	b.WriteString(":15A:\n")
	fmt.Fprintf(&b, ":20:%s\n", msgID)
	b.WriteString(":28D:1/1\n")
	fmt.Fprintf(&b, ":50H:/%s\n", debtorIBAN.IBAN)
	if validBIC(debtorIBAN.BIC) && debtorIBAN.BIC != "" {
		fmt.Fprintf(&b, ":52A:%s\n", strings.ToUpper(debtorIBAN.BIC))
	}
	fmt.Fprintf(&b, ":30:%s\n", mt940ValueDate(executeOn))
	for _, p := range payments {
		b.WriteString(":15B:\n")
		fmt.Fprintf(&b, ":21:%s\n", p.TransferID)
		fmt.Fprintf(&b, ":32B:%s%s\n", p.Currency, mt940Amount(p.AmountCents))
		if validBIC(p.CreditorIBAN.BIC) && p.CreditorIBAN.BIC != "" {
			fmt.Fprintf(&b, ":57A:%s\n", strings.ToUpper(p.CreditorIBAN.BIC))
		} else {
			fmt.Fprintf(&b, ":57D:%s\n", string(p.To))
		}
		fmt.Fprintf(&b, ":59:/%s\n", p.CreditorIBAN.IBAN)
		fmt.Fprintf(&b, ":71A:%s\n", chargeBearer)
	}
	b.WriteString("-}")
	return b.String()
}

// mt101TransactionReference validates a :21: reference (the
// transfer ID): 1–16 SWIFT `x` characters, no braces, no line
// breaks (a single-line field must not smuggle a second line into
// block 4), no leading/trailing colon or slash, no double slash.
// The MT300 shape (see mt300TransactionReference) under this
// export's sentinel.
func mt101TransactionReference(ref string) error {
	if ref == "" || len(ref) > mt103MaxSenderReference {
		return fmt.Errorf("%w: :21: want 1-%d characters, got %q", ErrInvalidMT101Export, mt103MaxSenderReference, ref)
	}
	if strings.ContainsAny(ref, "\r\n") {
		return fmt.Errorf("%w: :21: line break in a single-line field: %q", ErrInvalidMT101Export, ref)
	}
	if err := mt103CheckX(ref); err != nil {
		return fmt.Errorf("%w: :21: %w", ErrInvalidMT101Export, err)
	}
	if strings.HasPrefix(ref, ":") || strings.HasPrefix(ref, "/") ||
		strings.HasSuffix(ref, ":") || strings.HasSuffix(ref, "/") ||
		strings.Contains(ref, "//") {
		return fmt.Errorf("%w: :21: leading/trailing colon or slash, or double slash: %q", ErrInvalidMT101Export, ref)
	}
	return nil
}
