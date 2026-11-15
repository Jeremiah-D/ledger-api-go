package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SWIFT FIN MT192 request for cancellation export (LG-434, fintech:
// SWIFT 付款取消请求).
//
// ExportMT192 renders an approved or submitted payout instruction's
// cancellation request as a SWIFT FIN MT192 — the wire message that
// asks the correspondent bank to cancel the MT103 the ledger already
// sent (see mt103.go, LG-183). The field set follows the SWIFT MT192
// table: :20: this message's own reference, :21: the related
// reference (the original MT103's :20:, i.e. the payout ID), :11S:
// the original message type (103) and its value date, and :79: an
// optional free-text narrative.
//
// The export is the cancellation *request*, not the cancellation
// itself: the payout keeps its status until the bank confirms, and
// the ledger parses no MT192 receipt — whether the bank honours the
// request is confirmed by its own reply, outside this ledger. The
// settled-side mirror is the pacs.007 (LG-411): once the money has
// moved, a cancellation request is too late and a return is the
// honest message instead.
//
// Identity wiring — scope honesty, the ledger is not a SWIFT member:
//   - The sender's BIC comes from the payee account's registered
//     IBAN BIC (see SetAccountIBAN, LG-150): the operator registers
//     the bank's own BIC once, instead of the export inventing one.
//     No BIC anywhere is a fail-closed refusal, not a guessed value.
//   - The receiver's LT address is always operator-supplied (the
//     correspondent the original MT103 went to); the ledger records
//     nowhere where an MT103 was sent, so the caller names the
//     correspondent on every export. The ledger keeps no routing
//     table, so it never picks a correspondent on its own.
//   - :20: is a deterministic derivation — "MT192-" plus the first
//     10 lowercase hex characters of the SHA-256 of the payout ID —
//     16 characters, 16x legal (the MT101 :20: precedent, LG-391),
//     so repeat exports render byte-identical and the reference is
//     stable per payout without inventing a sequence number.
//   - :11S: reuses mt103ValueDate (mt103.go): the original MT103
//     :32A: value date, explicit when the caller gives one, else the
//     MT103 default rule (submit time, then approval time, then
//     creation time).
//
// Lifecycle honesty: only an approved or submitted payout can be
// cancelled — the instruction is out there to be withdrawn. Draft
// (nothing was ever sent), failed, cancelled, and settled (the
// money already moved — cancel via pacs.007, LG-411) are all
// refused fail-closed, mirroring ExportMT103's status gate.
//
// Field discipline is fail-loud, the nacha.go convention: every
// text field must fit the SWIFT `x` character set (see mt103XCharset)
// and its length; over-length or non-`x` input is an error, never
// silently truncated. Braces are additionally refused in block-4
// field content: `{`/`}` would break the FIN block framing even
// though they are technically in the `x` set. :79: is free text —
// no reason-code table is invented — but it is still validated
// (at most 35 lines of 35 `x` characters, the SWIFT :79: shape).
//
// Wire honesty notes:
//   - Block 3 carries `{108:<MUR>}` with the :20: reference, tying
//     the message back to the payout (the MT103 convention).
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
// audit event — the same contract as ExportMT103 and ExportMT210.
// The export is deterministic per payout and options: repeat exports
// render byte-identical output, a statement of fact, not an attempt
// sequence.

// MT192 outbound export errors.
var (
	// ErrMT192BadStatus is returned by ExportMT192 when the payout is
	// not approved or submitted: a draft was never sent anywhere, and
	// a failed, cancelled, or settled payout has no live instruction
	// left to cancel (settled goes via pacs.007, LG-411).
	ErrMT192BadStatus = errors.New("ledger: mt192 export: payout is not approved or submitted")
	// ErrInvalidMT192Export is returned when the payout cannot be
	// rendered as an MT192: a missing or invalid ReceiverLT, a
	// payout ID outside 16x, a reason outside the :79: shape, or no
	// sender BIC registered for the payee account.
	ErrInvalidMT192Export = errors.New("ledger: invalid MT192 export")
)

// MT192 :79: narrative shape (SWIFT field 79: 35x * 35 lines).
const (
	mt192MaxReasonLine  = 35
	mt192MaxReasonLines = 35
)

// MT192ExportOptions tunes ExportMT192.
//
// PayoutID is required: the approved or submitted payout whose
// MT103 this export asks to cancel.
// ReceiverLT is required: the correspondent's 12-character LT
// address for block 2 (BIC8/11 accepted, normalized) — the bank
// the original MT103 went to, named by the caller every time
// because the ledger records no MT103 destination.
// OriginalValueDate is optional: the original MT103 :32A: value
// date, defaulting to the payout's submit time, then approval time,
// then creation time (the MT103 default rule).
// Reason is optional: the :79: narrative, verbatim free text. Empty
// omits :79: entirely; no default is pre-filled.
type MT192ExportOptions struct {
	PayoutID          string
	ReceiverLT        string
	OriginalValueDate time.Time
	Reason            string
}

// mt192MessageReference derives the deterministic :20: reference:
// "MT192-" plus the first 10 lowercase hex characters of the
// SHA-256 of the payout ID — 16 characters, 16x legal (the MT101
// :20: precedent, LG-391).
func mt192MessageReference(payoutID string) string {
	sum := sha256.Sum256([]byte(payoutID))
	return "MT192-" + hex.EncodeToString(sum[:])[:10]
}

// mt192ReasonLines validates the :79: narrative: at most 35 lines
// of at most 35 SWIFT `x` characters each (braces refused, the
// block-4 framing rule). It returns the lines to render.
func mt192ReasonLines(reason string) ([]string, error) {
	lines := strings.Split(reason, "\n")
	if len(lines) > mt192MaxReasonLines {
		return nil, fmt.Errorf("%w: :79: at most %d lines", ErrInvalidMT192Export, mt192MaxReasonLines)
	}
	for _, ln := range lines {
		if len(ln) > mt192MaxReasonLine {
			return nil, fmt.Errorf("%w: :79: line exceeds %d characters: %q", ErrInvalidMT192Export, mt192MaxReasonLine, ln)
		}
		if err := mt103CheckX(ln); err != nil {
			return nil, fmt.Errorf("%w: :79: line: %v", ErrInvalidMT192Export, err)
		}
	}
	return lines, nil
}

// ExportMT192 renders the payout's cancellation request as a SWIFT
// FIN MT192 message, asking the correspondent to cancel the MT103
// already sent for this payout. See the package doc for the identity
// wiring, lifecycle rules, and wire honesty notes.
//
// The returned string is the FIN text with blocks 1–4, lines joined
// with "\n", no trailing newline (the nacha.go convention). The
// export is read-only and deterministic per payout and options:
// repeat exports render byte-identical output.
func (l *Ledger) ExportMT192(opts MT192ExportOptions) (string, error) {
	if opts.PayoutID == "" {
		return "", fmt.Errorf("%w: %q", ErrMT103PayoutNotFound, opts.PayoutID)
	}
	l.mu.RLock()
	p, ok := l.payouts[opts.PayoutID]
	if !ok {
		l.mu.RUnlock()
		return "", fmt.Errorf("%w: %q", ErrMT103PayoutNotFound, opts.PayoutID)
	}
	var senderBIC string
	if reg, ok := l.accountIBANs[p.PayeeAccount]; ok {
		senderBIC = reg.BIC
	}
	l.mu.RUnlock()
	// p and senderBIC are value copies out of the maps: the render
	// below cannot mutate ledger state even by accident.

	if p.Status != PayoutApproved && p.Status != PayoutSubmitted {
		return "", fmt.Errorf("%w: payout %q is %s", ErrMT192BadStatus, p.ID, p.Status)
	}

	// Sender BIC: the payee account's registered IBAN BIC. A SWIFT
	// cancellation request without a sender is refused, never
	// guessed.
	if senderBIC == "" {
		return "", fmt.Errorf("%w: sender BIC is not configured: register the payee account's BIC (SetAccountIBAN)", ErrInvalidMT192Export)
	}
	senderLT, err := mt103NormalizeLT(senderBIC)
	if err != nil {
		return "", fmt.Errorf("%w: sender LT: %v", ErrInvalidMT192Export, err)
	}
	if opts.ReceiverLT == "" {
		return "", fmt.Errorf("%w: ReceiverLT is required", ErrInvalidMT192Export)
	}
	receiverLT, err := mt103NormalizeLT(opts.ReceiverLT)
	if err != nil {
		return "", fmt.Errorf("%w: receiver LT: %v", ErrInvalidMT192Export, err)
	}

	// :21: — the related reference is the payout ID, the original
	// MT103's :20:. It must itself fit 16x, or the request cannot be
	// rendered.
	if len(p.ID) > mt103MaxSenderReference {
		return "", fmt.Errorf("%w: :21: payout ID exceeds %d characters: %q", ErrInvalidMT192Export, mt103MaxSenderReference, p.ID)
	}
	if err := mt103CheckX(p.ID); err != nil {
		return "", fmt.Errorf("%w: :21: payout ID: %v", ErrInvalidMT192Export, err)
	}

	// :11S: — the original message (103) and its value date: explicit
	// when given, else the MT103 default rule (submit, then approval,
	// then creation).
	valueDate := opts.OriginalValueDate
	if valueDate.IsZero() {
		valueDate = p.SubmittedAt
		if valueDate.IsZero() {
			valueDate = p.ApprovedAt
		}
		if valueDate.IsZero() {
			valueDate = p.CreatedAt
		}
	}

	// :79: — the narrative, only when the caller gives a reason.
	var reasonLines []string
	if opts.Reason != "" {
		reasonLines, err = mt192ReasonLines(opts.Reason)
		if err != nil {
			return "", err
		}
	}

	// :20: — the deterministic message reference, also the MUR.
	ref := mt192MessageReference(p.ID)

	var b strings.Builder
	// Block 1: basic header. Session/sequence are zero-filled —
	// assigned by the sender's SWIFT interface on transmission.
	fmt.Fprintf(&b, "{1:F01%s0000000000}\n", senderLT)
	// Block 2: application header, input to SWIFT. Priority U
	// (urgent), delivery monitoring 3 (non-delivery warning and
	// delivery notification), obsolescence period 003 — the MT103
	// application header with the message type swapped.
	fmt.Fprintf(&b, "{2:I192%sU3003}\n", receiverLT)
	// Block 3: user header — the Message User Reference ties the
	// message back to the payout.
	fmt.Fprintf(&b, "{3:{108:%s}}\n", ref)
	// Block 4: text.
	b.WriteString("{4:\n")
	fmt.Fprintf(&b, ":20:%s\n", ref)
	fmt.Fprintf(&b, ":21:%s\n", p.ID)
	fmt.Fprintf(&b, ":11S:103%s\n", mt103ValueDate(valueDate))
	if len(reasonLines) > 0 {
		b.WriteString(":79:")
		b.WriteString(strings.Join(reasonLines, "\n"))
		b.WriteString("\n")
	}
	b.WriteString("-}")
	return b.String(), nil
}
