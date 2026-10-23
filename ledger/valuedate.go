package ledger

import (
	"errors"
	"fmt"
	"time"
)

// Value dating / 起息日 (LG-181, fintech / backend).
//
// A journal entry's booking date (CreatedAt) is when the ledger recorded
// it; its value date is the economic date the money moves — the date
// interest starts accruing (资金起息). Post accepts an optional value
// date (JournalEntry.ValueDate); empty means "the same as the booking
// date", which is the historical behavior and journals nothing extra.
//
// When an explicit value date is given, Post resolves it before anything
// is journaled — fail-closed:
//
//   - The window: the value date may stray from the booking date by at
//     most valueDateWindowAhead into the future and valueDateWindowBack
//     into the past (30 days each by default, see SetValueDateWindow).
//     Past either edge the posting is rejected with
//     ErrValueDateOutOfWindow and records nothing — no journal row, no
//     chain link, no version bump.
//   - The business-day convention (following / preceding): a value date
//     landing on a non-business day is moved to the next / previous
//     business day under the ledger's holiday calendar (see
//     businessday.go, LG-95). The calendar only supplies the dates —
//     the dating logic lives here, orthogonal to the calendar itself.
//   - A value date that resolves to the booking date is the default: it
//     is stored as zero, so the audit-chain section is skipped and the
//     encoding stays byte-identical to a plain posting.
//
// Accruals anchor their day-count and period logic on the value date,
// not the booking date (see accruedTotalLocked in accrual.go): interest
// accrues from the value date. The accrual entry's deterministic ID
// still anchors on the booking day (asof), so per-day idempotency is
// unchanged.

// ValueDateConvention names the business-day convention applied to a
// value date that lands on a non-business day.
type ValueDateConvention string

const (
	// ValueDateConventionNone leaves the value date untouched, even on
	// a weekend or a holiday. It is the zero value and the default.
	ValueDateConventionNone ValueDateConvention = ""
	// ValueDateConventionFollowing moves a non-business-day value date
	// forward to the next business day.
	ValueDateConventionFollowing ValueDateConvention = "following"
	// ValueDateConventionPreceding moves a non-business-day value date
	// back to the previous business day.
	ValueDateConventionPreceding ValueDateConvention = "preceding"
)

// Validation errors for value dating.
var (
	// ErrInvalidValueDateConvention is returned when a posting names an
	// unknown business-day convention, or names a convention without an
	// explicit value date (a convention with nothing to adjust is a
	// caller bug). It is a 422-class error.
	ErrInvalidValueDateConvention = errors.New("ledger: invalid value-date convention")
	// ErrValueDateOutOfWindow is returned when a posting's value date
	// strays further from its booking date than the ledger's value-date
	// window allows, ahead or back. Fail-closed: nothing is journaled.
	// It is a 422-class error.
	ErrValueDateOutOfWindow = errors.New("ledger: value date outside the allowed window from the booking date")
	// ErrInvalidValueDateWindow is returned by WithValueDateWindow and
	// SetValueDateWindow for a negative window edge: an edge of zero
	// pins that side to the booking date exactly.
	ErrInvalidValueDateWindow = errors.New("ledger: value-date window edges must not be negative")
)

// defaultValueDateWindowAhead and defaultValueDateWindowBack bound how
// far a value date may stray from its booking date: 30 days ahead, 30
// days back. Generous enough for back-valued corrections and
// forward-valued settlement instructions, tight enough that a wild date
// is a fail-closed rejection rather than a silent misbooking.
const (
	defaultValueDateWindowAhead = 30 * 24 * time.Hour
	defaultValueDateWindowBack  = 30 * 24 * time.Hour
)

// checkValueDateConvention enforces the convention contract: empty
// ("none"), "following", or "preceding" — anything else is rejected
// fail-closed. Callers: checkValueDateFields (Post and PostBatch
// entries).
func checkValueDateConvention(c ValueDateConvention) error {
	switch c {
	case ValueDateConventionNone, ValueDateConventionFollowing, ValueDateConventionPreceding:
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrInvalidValueDateConvention, string(c))
	}
}

// checkValueDateFields enforces the value-date field contract: the
// convention must be a known value, and a convention other than "none"
// requires an explicit value date (adjusting nothing is a caller bug,
// rejected fail-closed). The window and the business-day adjustment are
// resolved at Post time, when the booking date is final (see
// resolveValueDateLocked). Callers: validateJournalEntry (Post and
// PostBatch entries).
func checkValueDateFields(e *JournalEntry) error {
	if err := checkValueDateConvention(e.ValueDateConvention); err != nil {
		return err
	}
	if e.ValueDateConvention != ValueDateConventionNone && e.ValueDate.IsZero() {
		return fmt.Errorf("%w: convention %q requires an explicit value_date", ErrInvalidValueDateConvention, string(e.ValueDateConvention))
	}
	return nil
}

// WithValueDateWindow sets the value-date window at construction (see
// SetValueDateWindow). A negative edge panics — fail-fast at
// construction, like every other calendar input.
func WithValueDateWindow(ahead, back time.Duration) Option {
	if ahead < 0 || back < 0 {
		panic(fmt.Sprintf("ledger: %v", ErrInvalidValueDateWindow))
	}
	return func(l *Ledger) {
		l.valueDateWindowAhead = ahead
		l.valueDateWindowBack = back
	}
}

// SetValueDateWindow replaces the ledger's value-date window: how far a
// posting's value date may stray from its booking date, ahead and back.
// An edge of zero pins that side to the booking date exactly. The change
// is structural (no journal version bump) and audited (op
// "value_date_window_updated").
func (l *Ledger) SetValueDateWindow(ahead, back time.Duration) error {
	if ahead < 0 || back < 0 {
		return ErrInvalidValueDateWindow
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.valueDateWindowAhead = ahead
	l.valueDateWindowBack = back
	l.emitAudit(AuditEvent{
		Op:            "value_date_window_updated",
		Actor:         "SetValueDateWindow",
		TraceID:       "value-date-window",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"ahead": ahead.String(),
			"back":  back.String(),
		},
	})
	return nil
}

// ValueDateWindow reports the ledger's value-date window: the maximum
// the value date may run ahead of and behind the booking date.
func (l *Ledger) ValueDateWindow() (ahead, back time.Duration) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.valueDateWindowAhead, l.valueDateWindowBack
}

// resolveValueDateLocked resolves an entry's value date against its
// booking date (CreatedAt, final by the time this runs): the window
// gates the requested date, the business-day convention adjusts it, and
// a date that resolves to the booking date is stored as zero so the
// audit-chain section is skipped. Callers must hold l.mu (the write
// lock): the holiday calendar is read directly. A zero ValueDate is the
// default — the booking date — and needs no resolution.
func (l *Ledger) resolveValueDateLocked(e *JournalEntry) error {
	if e.ValueDate.IsZero() {
		return nil
	}
	// The window gates the requested value date against the booking
	// date. A date past the leading edge — or before the trailing edge
	// — is rejected fail-closed before anything is journaled: no
	// journal row, no chain link, no version bump.
	delta := e.ValueDate.Sub(e.CreatedAt)
	if delta > l.valueDateWindowAhead || -delta > l.valueDateWindowBack {
		return fmt.Errorf("%w: value date %s is %s from booking date %s (window +%s/-%s)",
			ErrValueDateOutOfWindow,
			e.ValueDate.UTC().Format(time.RFC3339), delta,
			e.CreatedAt.UTC().Format(time.RFC3339),
			l.valueDateWindowAhead, l.valueDateWindowBack)
	}
	// The business-day convention adjusts the requested date against
	// the ledger's holiday calendar (see businessday.go, LG-95): the
	// calendar only supplies the dates — the dating logic lives here,
	// orthogonal to the calendar itself.
	adjusted := e.ValueDate
	switch e.ValueDateConvention {
	case ValueDateConventionNone:
		// No adjustment: the value date stands, weekend or not.
	case ValueDateConventionFollowing:
		if !l.businessCalendar.IsBusinessDay(adjusted) {
			adjusted = l.businessCalendar.NextBusinessDay(adjusted)
		}
	case ValueDateConventionPreceding:
		if !l.businessCalendar.IsBusinessDay(adjusted) {
			adjusted = l.businessCalendar.PrevBusinessDay(adjusted)
		}
	default:
		// Unreachable: validateJournalEntry's checkValueDateFields
		// rejects unknown conventions before this runs.
		return checkValueDateConvention(e.ValueDateConvention)
	}
	// A value date that resolves to the booking date is the default —
	// it is stored as zero so the audit-chain section is skipped and
	// the encoding stays byte-identical to a plain posting.
	if adjusted.Equal(e.CreatedAt) {
		e.ValueDate = time.Time{}
		return nil
	}
	e.ValueDate = adjusted
	return nil
}

// EffectiveValueDate returns the entry's economic date for interest
// accrual (see accruedTotalLocked in accrual.go): the explicit value
// date when one was journaled, the booking date otherwise.
func (e JournalEntry) EffectiveValueDate() time.Time {
	if e.ValueDate.IsZero() {
		return e.CreatedAt
	}
	return e.ValueDate
}
