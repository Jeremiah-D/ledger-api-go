package ledger

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// SWIFT FIN MT210 notice to receive export (LG-393, fintech:
// SWIFT 收款通知报文).
//
// The MT300/MT304/MT320 confirm contracts that already exist; this
// module covers the other side of the timeline — a payment that is
// due to arrive. An MT210 is sent by the account owner to its
// account bank ("there will be money coming into my account") so
// the bank can prepare liquidity and reconciliation: the sender is
// the To account (the beneficiary), the receiver is the servicing
// bank named by the caller (ServicingBIC). It is a pre-advice, not
// a receipt confirmation — the bank answers nothing; the money
// either arrives on the value date or it does not.
//
// The source is a transfer schedule (see schedule.go, LG-41): one
// MT210 notifies the single next run. The field set follows the IBM
// SR 2017 MT210 table: :20: the schedule ID, :25: the account to
// be credited (the To account's registered IBAN when one exists,
// else the account ID), :30: the value date = the next run date
// (6!n, the MT210 :30: is a 6-digit date — mt940ValueDate is the
// right width here), then the repeating sequence, once, for the
// next run: :21: the run transfer ID ("<ID>/run/<seq>"), :32B: the
// principal to be received, :50a:/:52a: exactly one of the ordering
// party — :52A: when the From account is a financial institution
// with a registered BIC, :50D: (the ordering customer, named by
// account ID) otherwise. There is no :56a: (no intermediary data),
// no :72: (the repeating sequence stops at :56a:), and no memo:
// the schedule's Memo rides the journal entry, not the notice.
//
// :32B: is the principal only. A per-run fee (FeeCents) is a
// separate fee leg — the run books it as its own entry — so it
// never reduces the amount the beneficiary is told to expect. An
// FX schedule (ToCurrency set) cannot be notified at all: the
// amount to be received is only known when the rate is looked up
// at fire time (see schedule.go), so a notice would state a
// number the ledger cannot stand behind; it fails loud with
// ErrInvalidMT210Export instead of guessing.
//
// Party identifiers follow the MT300 convention (see
// mt300PartyIdentifierLocked): the account's registered BIC when
// one exists, else the internal account ID, uppercased. Blocks 1/2
// take the same identifiers through mt103NormalizeLT — the sender
// LT is the To party, the receiver LT the servicing BIC — so a To
// account without a registered BIC needs an LT-form account ID (8,
// 11, or 12 alphanumeric characters) to export at all; anything
// else fails loud rather than emitting a malformed header.
//
// The export is read-only: it takes the read lock only (via
// GetTransferSchedule and the identifier lookup), bumps no version,
// books nothing, and appends no audit event. Only an active
// schedule notifies: a paused schedule's next run will not fire on
// its cadence, so a notice would mislead liquidity planning, and
// cancelled/completed schedules have no next run at all — all
// three report ErrMT210ScheduleNotActive, as does a schedule whose
// NextRunAt has already passed its EndsAt (no run left to
// notify). Repeat exports are byte-identical while no run fires:
// every field is pinned to the stored schedule and its RunSeq —
// once a run fires, RunSeq advances and :21:/:30: move with it,
// which is a content change, not drift.

// ErrInvalidMT210Export is returned when a schedule cannot be
// rendered as an MT210: a :20: or :21: reference outside 16x, a
// servicing BIC that is not a valid 8/11 BIC, an FX schedule whose
// receivable amount is unknowable until fire time, a :25: account
// line outside 35x, a party identifier that is not a valid LT
// form, or a :50D: account ID outside the SWIFT `x` character
// set. It is a 400-class error: the stored schedule cannot be
// expressed as a valid notice.
var ErrInvalidMT210Export = errors.New("ledger: invalid MT210 export")

// ErrMT210ScheduleNotActive is returned when an MT210 is requested
// for a schedule that has no next run to notify: paused,
// cancelled, completed, or past its EndsAt. It is a 422-class
// error: the schedule exists, but there is nothing to announce.
var ErrMT210ScheduleNotActive = errors.New("ledger: transfer schedule is not active for MT210 notice")

// MT210ExportOptions tunes ExportMT210.
//
// ScheduleID names the transfer schedule whose next run is
// notified. ServicingBIC is the BIC of the bank holding the
// account to be credited (required, 8 or 11 characters): the
// ledger holds no bank-identity registry, so the caller names the
// servicing bank on every export.
type MT210ExportOptions struct {
	ScheduleID   string
	ServicingBIC string
}

// ExportMT210 renders the next run of the transfer schedule as a
// SWIFT FIN MT210 notice to receive. Unknown IDs report
// ErrScheduleNotFound; a paused, cancelled, completed, or
// exhausted schedule reports ErrMT210ScheduleNotActive. Only an
// active schedule with a next run exports.
func (l *Ledger) ExportMT210(opts MT210ExportOptions) (string, error) {
	// Caller-given options fail fast, before any schedule lookup:
	// a servicing bank that is not a valid BIC can never be a
	// valid notice.
	servicing := strings.ToUpper(strings.TrimSpace(opts.ServicingBIC))
	if servicing == "" || !validBIC(servicing) {
		return "", fmt.Errorf("%w: servicing BIC %q: want a valid 8/11-character BIC", ErrInvalidMT210Export, opts.ServicingBIC)
	}
	sched, err := l.GetTransferSchedule(opts.ScheduleID)
	if err != nil {
		return "", err
	}
	if sched.Status != ScheduleActive {
		return "", fmt.Errorf("%w: schedule %q is %s", ErrMT210ScheduleNotActive, opts.ScheduleID, sched.Status)
	}
	if !sched.EndsAt.IsZero() && sched.NextRunAt.After(sched.EndsAt) {
		return "", fmt.Errorf("%w: schedule %q next run %s is past its ends_at %s", ErrMT210ScheduleNotActive, opts.ScheduleID, sched.NextRunAt.UTC().Format(time.RFC3339), sched.EndsAt.UTC().Format(time.RFC3339))
	}
	// An FX schedule's receivable amount is only known when the
	// rate is looked up at fire time: a notice cannot state it.
	if sched.ToCurrency != "" {
		return "", fmt.Errorf("%w: schedule %q is an FX schedule (to_currency %q), the receivable amount is unknowable until fire time", ErrInvalidMT210Export, opts.ScheduleID, sched.ToCurrency)
	}
	// :20: is the schedule ID, 16x.
	if err := mt210TransactionReference(":20:", sched.ID); err != nil {
		return "", err
	}
	// :21: is the run transfer ID, 16x — "<ID>/run/<seq>".
	runID := fmt.Sprintf("%s/run/%d", sched.ID, sched.RunSeq)
	if err := mt210TransactionReference(":21:", runID); err != nil {
		return "", err
	}
	// The parties' best-known identifiers (registered BIC, else
	// the internal account ID, uppercased) and the To account's
	// registered IBAN — read under the lock, like the MT300
	// identifier lookup.
	l.mu.RLock()
	// The To BIC feeds block 1 through toID; the :25: line
	// prefers the IBAN, not the BIC — so only the identifier is
	// kept here.
	toID, _ := l.mt300PartyIdentifierLocked(sched.To)
	fromID, fromBIC := l.mt300PartyIdentifierLocked(sched.From)
	toIBAN, toHasIBAN := l.accountIBANs[sched.To]
	l.mu.RUnlock()

	// :25: is the account to be credited: the registered IBAN
	// when one exists, else the To account ID. Either way 35x and
	// SWIFT `x`.
	creditAccount := toID
	if toHasIBAN {
		creditAccount = toIBAN.IBAN
	}
	if len(creditAccount) > 35 {
		return "", fmt.Errorf("%w: :25: account line %q exceeds 35 characters", ErrInvalidMT210Export, creditAccount)
	}
	if err := mt103CheckX(creditAccount); err != nil {
		return "", fmt.Errorf("%w: :25: %w", ErrInvalidMT210Export, err)
	}
	// Blocks 1/2 LTs derive from the To identifier and the
	// servicing BIC: a To account without a registered BIC needs
	// an LT-form account ID (8, 11, or 12 alphanumeric
	// characters) — anything else fails loud here rather than
	// emitting a malformed header.
	senderLT, err := mt103NormalizeLT(toID)
	if err != nil {
		return "", fmt.Errorf("%w: sender LT from %q: %w", ErrInvalidMT210Export, toID, err)
	}
	receiverLT, err := mt103NormalizeLT(servicing)
	if err != nil {
		return "", fmt.Errorf("%w: receiver LT from %q: %w", ErrInvalidMT210Export, servicing, err)
	}
	// A :50D: ordering customer is named by account ID, which
	// must be SWIFT `x`.
	if !fromBIC {
		if err := mt103CheckX(fromID); err != nil {
			return "", fmt.Errorf("%w: :50D: account ID %q: %w", ErrInvalidMT210Export, fromID, err)
		}
	}

	var b strings.Builder
	// Block 1: session/sequence zero-filled — assigned by the
	// sender's SWIFT interface on transmission (MT103 convention).
	fmt.Fprintf(&b, "{1:F01%s0000000000}\n", senderLT)
	// Block 2: application header, input to SWIFT, I210. Priority
	// U, delivery monitoring 3, obsolescence period 003 — the MT103
	// application header with the message type swapped.
	fmt.Fprintf(&b, "{2:I210%sU3003}\n", receiverLT)
	// Block 3: the Message User Reference carries the :20:
	// reference, the MT103 convention.
	fmt.Fprintf(&b, "{3:{108:%s}}\n", sched.ID)
	// Block 4: the field set, in canonical order. The header
	// fields identify the notice; the single repeating sequence
	// notifies the next run only — :30: is shared across the whole
	// message, so a multi-run notice would mix value dates.
	b.WriteString("{4:\n")
	fmt.Fprintf(&b, ":20:%s\n", sched.ID)
	fmt.Fprintf(&b, ":25:%s\n", creditAccount)
	fmt.Fprintf(&b, ":30:%s\n", mt940ValueDate(sched.NextRunAt))
	fmt.Fprintf(&b, ":21:%s\n", runID)
	fmt.Fprintf(&b, ":32B:%s%s\n", sched.Currency, mt940Amount(sched.AmountCents))
	if fromBIC {
		fmt.Fprintf(&b, ":52A:%s\n", fromID)
	} else {
		fmt.Fprintf(&b, ":50D:%s\n", fromID)
	}
	b.WriteString("-}")
	return b.String(), nil
}

// mt210TransactionReference validates a 16x reference — :20: the
// schedule ID, :21: the run transfer ID ("<ID>/run/<seq>"):
// 1–16 SWIFT `x` characters, no braces, no line breaks (a
// single-line field must not smuggle a second line into block 4),
// no leading/trailing colon or slash, no double slash. It mirrors
// mt300TransactionReference but wraps ErrInvalidMT210Export — the
// MT300 helper's error would map to the wrong message type in the
// handler.
func mt210TransactionReference(field, ref string) error {
	if ref == "" || len(ref) > mt103MaxSenderReference {
		return fmt.Errorf("%w: %s want 1-%d characters, got %q", ErrInvalidMT210Export, field, mt103MaxSenderReference, ref)
	}
	if strings.ContainsAny(ref, "\r\n") {
		return fmt.Errorf("%w: %s line break in a single-line field: %q", ErrInvalidMT210Export, field, ref)
	}
	if err := mt103CheckX(ref); err != nil {
		return fmt.Errorf("%w: %s %w", ErrInvalidMT210Export, field, err)
	}
	if strings.HasPrefix(ref, ":") || strings.HasPrefix(ref, "/") ||
		strings.HasSuffix(ref, ":") || strings.HasSuffix(ref, "/") ||
		strings.Contains(ref, "//") {
		return fmt.Errorf("%w: %s leading/trailing colon or slash, or double slash: %q", ErrInvalidMT210Export, field, ref)
	}
	return nil
}
