package ledger

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Business-day calendar (LG-95, fintech / backend).
//
// A configurable holiday calendar: weekend days (default Saturday and
// Sunday — e.g. Friday and Saturday in some regions) plus a custom
// holiday table of UTC dates ("2006-01-02"). The calendar drives three
// opt-in behaviors:
//
//   - Interest accrual skips non-business days (see
//     WithAccrualBusinessDaysOnly): AccrueInterest, AccrueFee, and
//     AccrueTermInterest reject a non-business-day asof with
//     ErrNotBusinessDay instead of booking. Interest accrues per
//     business day, not per calendar day.
//   - Transfer schedules can advance on a business-day cadence (see
//     TransferSchedule.BusinessDayCadence): after firing, NextRunAt
//     moves T+N business days instead of a fixed duration, so payroll
//     and settlement runs never land on a holiday.
//   - Term deposits can settle maturity on business days (see
//     TermDeposit.BusinessDayMaturity): a MaturesAt landing on a
//     non-business day moves to the next business day at open time.
//
// The calendar is structural config with a version counter bumped on
// every change; both the calendar and the version round-trip through
// disaster-recovery snapshots (see snapshot.go), so a restored ledger
// keeps the exact holiday table the original ran on.

// Validation errors for the business-day calendar.
var (
	// ErrInvalidBusinessDayCalendar is returned when a weekend day is
	// not a valid time.Weekday or a holiday is not a "2006-01-02" date.
	ErrInvalidBusinessDayCalendar = errors.New("ledger: invalid business-day calendar")
	// ErrInvalidHoliday is returned by AddHoliday for a malformed date.
	ErrInvalidHoliday = errors.New("ledger: holiday must be a UTC date \"2006-01-02\"")
	// ErrNotBusinessDay is returned by the accrual paths when the
	// ledger runs with business-day-only accruals (see
	// WithAccrualBusinessDaysOnly) and the accrual date is not a
	// business day.
	ErrNotBusinessDay = errors.New("ledger: accrual date is not a business day")
)

// BusinessDayCalendar is the ledger's holiday calendar: the weekly
// weekend plus the custom holiday table. Dates are UTC "2006-01-02".
type BusinessDayCalendar struct {
	// Weekend lists the weekly non-business days (default Saturday and
	// Sunday). Empty means no weekly rest days.
	Weekend []time.Weekday `json:"weekend"`
	// Holidays lists extra non-business days, sorted, deduplicated.
	Holidays []string `json:"holidays"`
	// Version bumps on every calendar change; snapshots carry it so a
	// restored ledger keeps the exact table the original ran on.
	Version uint64 `json:"version"`
}

// DefaultBusinessDayCalendar returns the conventional calendar:
// Saturday/Sunday weekend, no custom holidays, version 1.
func DefaultBusinessDayCalendar() BusinessDayCalendar {
	return BusinessDayCalendar{
		Weekend:  []time.Weekday{time.Saturday, time.Sunday},
		Holidays: nil,
		Version:  1,
	}
}

// normalizeBusinessDayCalendar validates and canonicalizes a calendar:
// weekend sorted and deduplicated, holidays validated, sorted, and
// deduplicated. It never mutates its argument.
func normalizeBusinessDayCalendar(weekend []time.Weekday, holidays []string) (BusinessDayCalendar, error) {
	seen := map[time.Weekday]bool{}
	var wd []time.Weekday
	for _, d := range weekend {
		if d < time.Sunday || d > time.Saturday {
			return BusinessDayCalendar{}, fmt.Errorf("%w: weekend day %d out of range", ErrInvalidBusinessDayCalendar, int(d))
		}
		if !seen[d] {
			seen[d] = true
			wd = append(wd, d)
		}
	}
	sort.Slice(wd, func(i, j int) bool { return wd[i] < wd[j] })
	hset := map[string]bool{}
	var hs []string
	for _, h := range holidays {
		h = strings.TrimSpace(h)
		if _, err := time.Parse("2006-01-02", h); err != nil {
			return BusinessDayCalendar{}, fmt.Errorf("%w: %q", ErrInvalidHoliday, h)
		}
		if !hset[h] {
			hset[h] = true
			hs = append(hs, h)
		}
	}
	sort.Strings(hs)
	return BusinessDayCalendar{Weekend: wd, Holidays: hs, Version: 1}, nil
}

// isBusinessDate reports whether the UTC date (midnight) is a business
// day under the calendar.
func (c BusinessDayCalendar) isBusinessDate(date time.Time) bool {
	day := date.Format("2006-01-02")
	for _, h := range c.Holidays {
		if h == day {
			return false
		}
	}
	wd := date.Weekday()
	for _, w := range c.Weekend {
		if w == wd {
			return false
		}
	}
	return true
}

// IsBusinessDay reports whether t falls on a business day. The check
// runs on t's UTC calendar date; the clock is irrelevant.
func (c BusinessDayCalendar) IsBusinessDay(t time.Time) bool {
	u := t.UTC()
	return c.isBusinessDate(time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC))
}

// NextBusinessDay returns the first business day strictly after t's UTC
// date, preserving t's clock.
func (c BusinessDayCalendar) NextBusinessDay(t time.Time) time.Time {
	return c.AddBusinessDays(t, 1)
}

// PrevBusinessDay returns the first business day strictly before t's UTC
// date, preserving t's clock.
func (c BusinessDayCalendar) PrevBusinessDay(t time.Time) time.Time {
	return c.AddBusinessDays(t, -1)
}

// AddBusinessDays moves t by n business days, preserving the clock. n
// may be negative (backward) or zero (identity). A calendar with every
// day marked non-business would loop forever, so the walk is capped at
// 366*n+8 calendar days — beyond that the calendar is degenerate and
// the walk reports it instead of hanging.
func (c BusinessDayCalendar) AddBusinessDays(t time.Time, n int) time.Time {
	if n == 0 {
		return t
	}
	u := t.UTC()
	midnight := time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
	clock := u.Sub(midnight)
	step := 1
	if n < 0 {
		step = -1
		n = -n
	}
	date := midnight
	// Cap: at most 366 calendar days per requested business day plus a
	// small margin — a sane calendar always terminates far earlier.
	for i := 0; i < 366*n+8; i++ {
		date = date.AddDate(0, 0, step)
		if c.isBusinessDate(date) {
			n--
			if n == 0 {
				return date.Add(clock)
			}
		}
	}
	// Degenerate calendar (no business day within a year per requested
	// day): return the input unchanged rather than looping forever. The
	// calendar setters cannot create this state through valid input —
	// every weekday marked weekend plus 366 holidays — so this is
	// defensive only.
	return t
}

// ParseBusinessDayWeekend parses the LEDGER_BUSINESS_DAY_WEEKEND
// environment variable: a comma-separated list of weekday names, e.g.
// "Sat,Sun" or "Fri,Sat". Full names ("Saturday") and three-letter
// abbreviations ("Sat") are accepted, case-insensitively. Empty means no
// weekly rest days. An unknown name is an error — fail fast, like every
// other calendar input.
func ParseBusinessDayWeekend(raw string) ([]time.Weekday, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var out []time.Weekday
	for _, seg := range strings.Split(raw, ",") {
		name := strings.ToLower(strings.TrimSpace(seg))
		var found *time.Weekday
		for d := time.Sunday; d <= time.Saturday; d++ {
			full := strings.ToLower(d.String())
			if name == full || name == full[:3] {
				dd := d
				found = &dd
				break
			}
		}
		if found == nil {
			return nil, fmt.Errorf("%w: unknown weekday %q", ErrInvalidBusinessDayCalendar, seg)
		}
		out = append(out, *found)
	}
	return out, nil
}

// ParseBusinessDayHolidays parses the LEDGER_BUSINESS_DAY_HOLIDAYS
// environment variable: comma-separated UTC dates "2006-01-02", e.g.
// "2026-01-01,2026-10-01". A malformed date is an error — fail fast.
func ParseBusinessDayHolidays(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var out []string
	for _, seg := range strings.Split(raw, ",") {
		h := strings.TrimSpace(seg)
		if _, err := time.Parse("2006-01-02", h); err != nil {
			return nil, fmt.Errorf("%w: %q", ErrInvalidHoliday, seg)
		}
		out = append(out, h)
	}
	return out, nil
}

// WithBusinessDayCalendar installs a holiday calendar at construction
// (see SetBusinessDayCalendar). Invalid input panics — fail-fast at
// construction, like every other calendar input.
func WithBusinessDayCalendar(weekend []time.Weekday, holidays []string) Option {
	cal, err := normalizeBusinessDayCalendar(weekend, holidays)
	if err != nil {
		panic(fmt.Sprintf("ledger: %v", err))
	}
	return func(l *Ledger) {
		l.businessCalendar = cal
	}
}

// WithAccrualBusinessDaysOnly makes interest and fee accruals
// business-day-only at construction (see SetAccrualBusinessDaysOnly).
func WithAccrualBusinessDaysOnly(only bool) Option {
	return func(l *Ledger) {
		l.accrualBusinessDaysOnly = only
	}
}

// SetBusinessDayCalendar replaces the ledger's holiday calendar. The
// version bumps on every successful change; the change is structural
// (no version bump of the journal) and audited
// (op "business_day_calendar_updated").
func (l *Ledger) SetBusinessDayCalendar(weekend []time.Weekday, holidays []string) error {
	cal, err := normalizeBusinessDayCalendar(weekend, holidays)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	cal.Version = l.businessCalendar.Version + 1
	l.businessCalendar = cal
	l.emitAudit(AuditEvent{
		Op:            "business_day_calendar_updated",
		Actor:         "SetBusinessDayCalendar",
		TraceID:       "business-day-calendar",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"weekend":  weekendStrings(cal.Weekend),
			"holidays": cal.Holidays,
			"version":  cal.Version,
		},
	})
	return nil
}

// weekendStrings renders weekdays for audit details.
func weekendStrings(wd []time.Weekday) []string {
	out := make([]string, 0, len(wd))
	for _, d := range wd {
		out = append(out, d.String())
	}
	return out
}

// AddHoliday adds one UTC date ("2006-01-02") to the holiday table.
// Adding an existing holiday is a no-op success. The version bumps and
// the change is audited.
func (l *Ledger) AddHoliday(date string) error {
	if _, err := time.Parse("2006-01-02", date); err != nil {
		return fmt.Errorf("%w: %q", ErrInvalidHoliday, date)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, h := range l.businessCalendar.Holidays {
		if h == date {
			return nil
		}
	}
	l.businessCalendar.Holidays = append(l.businessCalendar.Holidays, date)
	sort.Strings(l.businessCalendar.Holidays)
	l.businessCalendar.Version++
	l.emitAudit(AuditEvent{
		Op:            "business_day_holiday_added",
		Actor:         "AddHoliday",
		TraceID:       "business-day-calendar",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details:       map[string]any{"holiday": date, "version": l.businessCalendar.Version},
	})
	return nil
}

// RemoveHoliday drops one date from the holiday table. Removing an
// absent date is a no-op success. The version bumps and the change is
// audited.
func (l *Ledger) RemoveHoliday(date string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	kept := l.businessCalendar.Holidays[:0]
	removed := false
	for _, h := range l.businessCalendar.Holidays {
		if h == date {
			removed = true
			continue
		}
		kept = append(kept, h)
	}
	if !removed {
		return nil
	}
	l.businessCalendar.Holidays = kept
	l.businessCalendar.Version++
	l.emitAudit(AuditEvent{
		Op:            "business_day_holiday_removed",
		Actor:         "RemoveHoliday",
		TraceID:       "business-day-calendar",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details:       map[string]any{"holiday": date, "version": l.businessCalendar.Version},
	})
	return nil
}

// BusinessDayCalendar returns a copy of the ledger's holiday calendar.
func (l *Ledger) BusinessDayCalendar() BusinessDayCalendar {
	l.mu.RLock()
	defer l.mu.RUnlock()
	cal := l.businessCalendar
	cal.Weekend = append([]time.Weekday(nil), cal.Weekend...)
	cal.Holidays = append([]string(nil), cal.Holidays...)
	return cal
}

// BusinessDayCalendarVersion reports the calendar's version: it bumps on
// every change, so operators can tell whether the table moved.
func (l *Ledger) BusinessDayCalendarVersion() uint64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.businessCalendar.Version
}

// IsBusinessDay reports whether t falls on a business day under the
// ledger's calendar.
func (l *Ledger) IsBusinessDay(t time.Time) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.businessCalendar.IsBusinessDay(t)
}

// SetAccrualBusinessDaysOnly toggles business-day-only accruals at
// runtime: when on, AccrueInterest, AccrueFee, and AccrueTermInterest
// reject a non-business-day asof with ErrNotBusinessDay instead of
// booking. Off by default (backward compatible). Structural config —
// audited, no journal version bump.
func (l *Ledger) SetAccrualBusinessDaysOnly(only bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.accrualBusinessDaysOnly = only
	l.emitAudit(AuditEvent{
		Op:            "accrual_business_days_only_set",
		Actor:         "SetAccrualBusinessDaysOnly",
		TraceID:       "accrual-business-days",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details:       map[string]any{"only": only},
	})
}

// AccrualBusinessDaysOnly reports whether accruals are restricted to
// business days.
func (l *Ledger) AccrualBusinessDaysOnly() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.accrualBusinessDaysOnly
}

// accrualBusinessDayGateLocked rejects a non-business-day accrual date
// when the ledger runs with business-day-only accruals. Callers must
// hold l.mu (either lock suffices); the calendar is read-only here.
func (l *Ledger) accrualBusinessDayGateLocked(asof time.Time) error {
	if l.accrualBusinessDaysOnly && !l.businessCalendar.IsBusinessDay(asof) {
		return ErrNotBusinessDay
	}
	return nil
}

// businessCalendarSnapshotLocked returns a copy of the holiday calendar
// for the snapshot config row. Callers must hold l.mu; the read lock
// suffices.
func (l *Ledger) businessCalendarSnapshotLocked() *BusinessDayCalendar {
	cal := l.businessCalendar
	cal.Weekend = append([]time.Weekday(nil), cal.Weekend...)
	cal.Holidays = append([]string(nil), cal.Holidays...)
	return &cal
}

// validateBusinessDayCalendarSnapshot checks an imported calendar: every
// weekend day in range and every holiday a valid date. The version rides
// verbatim — a restore keeps the exact table the original ran on.
func validateBusinessDayCalendarSnapshot(cal *BusinessDayCalendar) error {
	for _, d := range cal.Weekend {
		if d < time.Sunday || d > time.Saturday {
			return fmt.Errorf("bad business-day calendar weekend day %d", int(d))
		}
	}
	for _, h := range cal.Holidays {
		if _, err := time.Parse("2006-01-02", h); err != nil {
			return fmt.Errorf("bad business-day calendar holiday %q", h)
		}
	}
	return nil
}
