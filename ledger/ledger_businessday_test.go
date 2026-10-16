package ledger

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

// Fixed test week: 2026-10-16 is a Friday (UTC).
var (
	bdFriday   = time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC)
	bdSaturday = time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC)
	bdSunday   = time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	bdMonday   = time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	bdTuesday  = time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	bdThursday = time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
)

func TestBusinessDayCalendarOps(t *testing.T) {
	cal := DefaultBusinessDayCalendar()
	if !cal.IsBusinessDay(bdFriday) || !cal.IsBusinessDay(bdMonday) {
		t.Fatal("weekdays must be business days")
	}
	if cal.IsBusinessDay(bdSaturday) || cal.IsBusinessDay(bdSunday) {
		t.Fatal("weekend must not be business days")
	}
	if got := cal.NextBusinessDay(bdFriday); !got.Equal(bdMonday) {
		t.Fatalf("NextBusinessDay(Friday) = %v, want %v", got, bdMonday)
	}
	if got := cal.NextBusinessDay(bdSaturday); !got.Equal(bdMonday) {
		t.Fatalf("NextBusinessDay(Saturday) = %v, want %v", got, bdMonday)
	}
	if got := cal.PrevBusinessDay(bdMonday); !got.Equal(bdFriday) {
		t.Fatalf("PrevBusinessDay(Monday) = %v, want %v", got, bdFriday)
	}
	// AddBusinessDays preserves the clock and skips the weekend.
	if got := cal.AddBusinessDays(bdThursday, 2); !got.Equal(bdMonday) {
		t.Fatalf("AddBusinessDays(Thu, 2) = %v, want %v", got, bdMonday)
	}
	if got := cal.AddBusinessDays(bdMonday, -1); !got.Equal(bdFriday) {
		t.Fatalf("AddBusinessDays(Mon, -1) = %v, want %v", got, bdFriday)
	}
	if got := cal.AddBusinessDays(bdFriday, 0); !got.Equal(bdFriday) {
		t.Fatalf("AddBusinessDays(Fri, 0) = %v, want identity", got)
	}
	// Clock preservation with a non-midnight time.
	late := time.Date(2026, 10, 16, 23, 30, 45, 0, time.UTC)
	want := time.Date(2026, 10, 19, 23, 30, 45, 0, time.UTC)
	if got := cal.NextBusinessDay(late); !got.Equal(want) {
		t.Fatalf("NextBusinessDay clock: got %v, want %v", got, want)
	}
}

func TestBusinessDayCalendarHolidays(t *testing.T) {
	cal, err := normalizeBusinessDayCalendar(
		[]time.Weekday{time.Saturday, time.Sunday},
		[]string{"2026-10-19"},
	)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if cal.IsBusinessDay(bdMonday) {
		t.Fatal("holiday Monday must not be a business day")
	}
	if got := cal.NextBusinessDay(bdFriday); !got.Equal(bdTuesday) {
		t.Fatalf("NextBusinessDay(Friday) = %v, want %v (holiday Monday)", got, bdTuesday)
	}
	// Duplicates and unsorted input canonicalize.
	cal2, err := normalizeBusinessDayCalendar(
		[]time.Weekday{time.Sunday, time.Saturday, time.Saturday},
		[]string{"2026-10-19", "2026-10-19"},
	)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(cal2.Weekend) != 2 || len(cal2.Holidays) != 1 {
		t.Fatalf("canonical calendar = %+v", cal2)
	}
	if _, err := normalizeBusinessDayCalendar(nil, []string{"not-a-date"}); !errors.Is(err, ErrInvalidHoliday) {
		t.Fatalf("bad holiday: err = %v", err)
	}
	if _, err := normalizeBusinessDayCalendar([]time.Weekday{time.Weekday(9)}, nil); !errors.Is(err, ErrInvalidBusinessDayCalendar) {
		t.Fatalf("bad weekday: err = %v", err)
	}
}

func TestParseBusinessDayWeekend(t *testing.T) {
	got, err := ParseBusinessDayWeekend("Sat,Sun")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got) != 2 || got[0] != time.Saturday || got[1] != time.Sunday {
		t.Fatalf("got = %v", got)
	}
	got, err = ParseBusinessDayWeekend("friday,saturday")
	if err != nil {
		t.Fatalf("Parse full names: %v", err)
	}
	if len(got) != 2 || got[0] != time.Friday || got[1] != time.Saturday {
		t.Fatalf("got = %v", got)
	}
	if got, err := ParseBusinessDayWeekend(""); err != nil || got != nil {
		t.Fatalf("empty: got=%v err=%v", got, err)
	}
	if _, err := ParseBusinessDayWeekend("Sat,Funday"); err == nil {
		t.Fatal("unknown weekday: want error, got nil")
	}
}

func TestParseBusinessDayHolidays(t *testing.T) {
	got, err := ParseBusinessDayHolidays("2026-01-01, 2026-10-01")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got) != 2 || got[0] != "2026-01-01" || got[1] != "2026-10-01" {
		t.Fatalf("got = %v", got)
	}
	if _, err := ParseBusinessDayHolidays("2026-13-01"); err == nil {
		t.Fatal("bad date: want error, got nil")
	}
	if got, err := ParseBusinessDayHolidays(""); err != nil || got != nil {
		t.Fatalf("empty: got=%v err=%v", got, err)
	}
}

func TestSetBusinessDayCalendar(t *testing.T) {
	l := New()
	v0 := l.BusinessDayCalendarVersion()
	if v0 != 1 {
		t.Fatalf("initial version = %d, want 1", v0)
	}
	if err := l.SetBusinessDayCalendar(
		[]time.Weekday{time.Friday, time.Saturday},
		[]string{"2026-10-19"},
	); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := l.BusinessDayCalendarVersion(); got != v0+1 {
		t.Fatalf("version = %d, want %d", got, v0+1)
	}
	cal := l.BusinessDayCalendar()
	if len(cal.Weekend) != 2 || cal.Weekend[0] != time.Friday {
		t.Fatalf("weekend = %v", cal.Weekend)
	}
	if len(cal.Holidays) != 1 || cal.Holidays[0] != "2026-10-19" {
		t.Fatalf("holidays = %v", cal.Holidays)
	}
	if l.IsBusinessDay(bdFriday) {
		t.Fatal("Friday must not be a business day under Fri/Sat weekend")
	}
	if l.IsBusinessDay(bdMonday) {
		t.Fatal("holiday Monday must not be a business day")
	}
	if err := l.SetBusinessDayCalendar(nil, []string{"bad-date"}); !errors.Is(err, ErrInvalidHoliday) {
		t.Fatalf("bad holiday: err = %v", err)
	}
	// Failed set leaves the calendar untouched.
	if got := l.BusinessDayCalendarVersion(); got != v0+1 {
		t.Fatalf("version after failed set = %d, want %d", got, v0+1)
	}
}

func TestAddRemoveHoliday(t *testing.T) {
	l := New()
	v0 := l.BusinessDayCalendarVersion()
	if err := l.AddHoliday("2026-12-25"); err != nil {
		t.Fatalf("AddHoliday: %v", err)
	}
	if got := l.BusinessDayCalendarVersion(); got != v0+1 {
		t.Fatalf("version = %d, want %d", got, v0+1)
	}
	// Duplicate add is a no-op: no version bump.
	if err := l.AddHoliday("2026-12-25"); err != nil {
		t.Fatalf("AddHoliday duplicate: %v", err)
	}
	if got := l.BusinessDayCalendarVersion(); got != v0+1 {
		t.Fatalf("version after duplicate add = %d, want %d", got, v0+1)
	}
	if err := l.AddHoliday("not-a-date"); !errors.Is(err, ErrInvalidHoliday) {
		t.Fatalf("bad date: err = %v", err)
	}
	if err := l.RemoveHoliday("2026-12-25"); err != nil {
		t.Fatalf("RemoveHoliday: %v", err)
	}
	if got := l.BusinessDayCalendarVersion(); got != v0+2 {
		t.Fatalf("version = %d, want %d", got, v0+2)
	}
	// Removing an absent date is a no-op.
	if err := l.RemoveHoliday("2026-12-25"); err != nil {
		t.Fatalf("RemoveHoliday absent: %v", err)
	}
	if got := l.BusinessDayCalendarVersion(); got != v0+2 {
		t.Fatalf("version after absent remove = %d, want %d", got, v0+2)
	}
}

func TestWithBusinessDayCalendarPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("want panic on invalid calendar, got none")
		}
	}()
	_ = New(WithBusinessDayCalendar(nil, []string{"bad-date"}))
}

func TestAccrualBusinessDaysOnly(t *testing.T) {
	l := New(WithAccrualBusinessDaysOnly(true))
	if !l.AccrualBusinessDaysOnly() {
		t.Fatal("flag must be on")
	}
	// Saturday accrual is rejected.
	if _, _, err := l.AccrueInterest("exp", "pay", 1_000_000, 500, bdSaturday, 0); !errors.Is(err, ErrNotBusinessDay) {
		t.Fatalf("Saturday AccrueInterest: err = %v", err)
	}
	if _, _, err := l.AccrueFee("exp", "pay", 100, bdSunday, 0); !errors.Is(err, ErrNotBusinessDay) {
		t.Fatalf("Sunday AccrueFee: err = %v", err)
	}
	// Monday books normally.
	e, dup, err := l.AccrueInterest("exp", "pay", 1_000_000, 500, bdMonday, 0)
	if err != nil || dup {
		t.Fatalf("Monday AccrueInterest: e=%+v dup=%v err=%v", e, dup, err)
	}
	// Default ledgers keep calendar-day accruals.
	l2 := New()
	if l2.AccrualBusinessDaysOnly() {
		t.Fatal("default flag must be off")
	}
	if _, _, err := l2.AccrueInterest("exp", "pay", 1_000_000, 500, bdSaturday, 0); err != nil {
		t.Fatalf("default Saturday AccrueInterest: err = %v", err)
	}
	// Runtime toggle.
	l2.SetAccrualBusinessDaysOnly(true)
	if _, _, err := l2.AccrueFee("exp2", "pay2", 100, bdSaturday, 0); !errors.Is(err, ErrNotBusinessDay) {
		t.Fatalf("toggled Saturday AccrueFee: err = %v", err)
	}
}

func TestAccrueTermInterestBusinessDay(t *testing.T) {
	l := seedTermDepositLedger(t)
	l.SetAccrualBusinessDaysOnly(true)
	td, _, err := l.OpenTermDeposit(TermDeposit{
		ID: "td-bd", Account: "saver", AmountCents: 400_000, TermDays: 30, RateBps: 500,
	})
	if err != nil {
		t.Fatalf("OpenTermDeposit: %v", err)
	}
	_ = td
	if _, _, err := l.AccrueTermInterest("td-bd", bdSaturday); !errors.Is(err, ErrNotBusinessDay) {
		t.Fatalf("Saturday AccrueTermInterest: err = %v", err)
	}
	if _, _, err := l.AccrueTermInterest("td-bd", bdMonday); err != nil {
		t.Fatalf("Monday AccrueTermInterest: err = %v", err)
	}
}

func TestTermDepositBusinessDayMaturity(t *testing.T) {
	l := seedTermDepositLedger(t)
	// Thursday + 2 days = Saturday: with business-day settlement the
	// maturity moves to Monday, clock preserved.
	td, _, err := l.OpenTermDeposit(TermDeposit{
		ID: "td-bdm", Account: "saver", AmountCents: 100_000,
		TermDays: 2, RateBps: 500,
		OpenedAt:            bdThursday,
		BusinessDayMaturity: true,
	})
	if err != nil {
		t.Fatalf("OpenTermDeposit: %v", err)
	}
	if !td.MaturesAt.Equal(bdMonday) {
		t.Fatalf("MaturesAt = %v, want %v (next business day)", td.MaturesAt, bdMonday)
	}
	// Without the flag the Saturday maturity stands.
	td2, _, err := l.OpenTermDeposit(TermDeposit{
		ID: "td-plain", Account: "saver", AmountCents: 100_000,
		TermDays: 2, RateBps: 500,
		OpenedAt: bdThursday,
	})
	if err != nil {
		t.Fatalf("OpenTermDeposit: %v", err)
	}
	want := bdThursday.Add(2 * 24 * time.Hour)
	if !td2.MaturesAt.Equal(want) {
		t.Fatalf("MaturesAt = %v, want %v", td2.MaturesAt, want)
	}
}

func TestScheduleBusinessDayCadence(t *testing.T) {
	l := New()
	seedBalance(t, l, "payer", 10_000_000)
	sched, _, err := l.CreateTransferSchedule(TransferSchedule{
		ID:                "sched-bd",
		From:              "payer",
		To:                "payee",
		AmountCents:       1000,
		Currency:          "USD",
		Interval:          24 * time.Hour,
		NextRunAt:         bdFriday,
		BusinessDayCadence: true,
	})
	if err != nil {
		t.Fatalf("CreateTransferSchedule: %v", err)
	}
	if !sched.BusinessDayCadence {
		t.Fatal("BusinessDayCadence must round-trip the create call")
	}
	// Fire on Friday: the next run lands on Monday, skipping the weekend.
	runs := l.SweepDue(bdFriday.Add(2 * time.Hour))
	if len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("runs = %+v", runs)
	}
	after, err := l.GetTransferSchedule("sched-bd")
	if err != nil {
		t.Fatalf("GetTransferSchedule: %v", err)
	}
	if !after.NextRunAt.Equal(bdMonday) {
		t.Fatalf("NextRunAt = %v, want %v (T+1 business day)", after.NextRunAt, bdMonday)
	}
	if runs[0].SkippedRuns != 0 {
		t.Fatalf("SkippedRuns = %d, want 0", runs[0].SkippedRuns)
	}
	// A holiday Monday pushes the next run to Tuesday.
	if err := l.AddHoliday("2026-10-19"); err != nil {
		t.Fatalf("AddHoliday: %v", err)
	}
	runs = l.SweepDue(bdMonday.Add(2 * time.Hour))
	if len(runs) != 1 || runs[0].Status != "fired" {
		t.Fatalf("runs = %+v", runs)
	}
	after, _ = l.GetTransferSchedule("sched-bd")
	if !after.NextRunAt.Equal(bdTuesday) {
		t.Fatalf("NextRunAt = %v, want %v (holiday skip)", after.NextRunAt, bdTuesday)
	}
	// Missed runs skip forward by business days, counted honestly.
	// Nominal Tue 10-20 fires now (Wed 10-28 12:00); Wed 10-21, Thu
	// 10-22, Fri 10-23, Mon 10-26, Tue 10-27, Wed 10-28 skip: 6.
	runs = l.SweepDue(bdTuesday.Add(8*24*time.Hour + 2*time.Hour))
	if len(runs) != 1 {
		t.Fatalf("runs = %+v", runs)
	}
	after, _ = l.GetTransferSchedule("sched-bd")
	want := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	if !after.NextRunAt.Equal(want) {
		t.Fatalf("NextRunAt = %v, want %v", after.NextRunAt, want)
	}
	if runs[0].SkippedRuns != 6 {
		t.Fatalf("SkippedRuns = %d, want 6", runs[0].SkippedRuns)
	}
}

func TestScheduleBusinessDayCadenceSnapshot(t *testing.T) {
	l := New()
	seedBalance(t, l, "payer", 10_000_000)
	if _, _, err := l.CreateTransferSchedule(TransferSchedule{
		ID: "sched-bd-snap", From: "payer", To: "payee",
		AmountCents: 1000, Currency: "USD",
		Interval: 24 * time.Hour, NextRunAt: bdFriday,
		BusinessDayCadence: true,
	}); err != nil {
		t.Fatalf("CreateTransferSchedule: %v", err)
	}
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	dst, err := ImportSnapshot(&buf)
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	sched, err := dst.GetTransferSchedule("sched-bd-snap")
	if err != nil {
		t.Fatalf("GetTransferSchedule: %v", err)
	}
	if !sched.BusinessDayCadence {
		t.Fatal("BusinessDayCadence must survive the snapshot round-trip")
	}
}

func TestBusinessDayCalendarSnapshotRoundTrip(t *testing.T) {
	src := New(
		WithBusinessDayCalendar(
			[]time.Weekday{time.Friday, time.Saturday},
			[]string{"2026-10-19", "2026-12-25"},
		),
		WithAccrualBusinessDaysOnly(true),
	)
	v0 := src.BusinessDayCalendarVersion()
	var buf bytes.Buffer
	if err := src.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	dst, err := ImportSnapshot(&buf)
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if got := dst.BusinessDayCalendarVersion(); got != v0 {
		t.Fatalf("restored version = %d, want %d", got, v0)
	}
	cal := dst.BusinessDayCalendar()
	if len(cal.Weekend) != 2 || cal.Weekend[0] != time.Friday || cal.Weekend[1] != time.Saturday {
		t.Fatalf("restored weekend = %v", cal.Weekend)
	}
	if len(cal.Holidays) != 2 || cal.Holidays[0] != "2026-10-19" || cal.Holidays[1] != "2026-12-25" {
		t.Fatalf("restored holidays = %v", cal.Holidays)
	}
	if !dst.AccrualBusinessDaysOnly() {
		t.Fatal("restored accrual flag must be true")
	}
	if dst.IsBusinessDay(bdFriday) {
		t.Fatal("restored calendar: Friday must not be a business day")
	}
	// A snapshot written before business-day support existed imports as
	// the default Saturday/Sunday calendar.
	legacy := New()
	var lbuf bytes.Buffer
	if err := legacy.ExportSnapshot(&lbuf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	stripped := strings.Replace(lbuf.String(), `"business_day_calendar"`, `"business_day_calendar_absent"`, 1)
	stripped = strings.Replace(stripped, `"accrual_business_days_only"`, `"accrual_business_days_only_absent"`, 1)
	dst2, err := ImportSnapshot(strings.NewReader(stripped))
	if err != nil {
		t.Fatalf("ImportSnapshot (legacy): %v", err)
	}
	if got := dst2.BusinessDayCalendarVersion(); got != 1 {
		t.Fatalf("legacy version = %d, want 1 (default)", got)
	}
	if !dst2.IsBusinessDay(bdMonday) || dst2.IsBusinessDay(bdSaturday) {
		t.Fatal("legacy calendar must be the default Sat/Sun table")
	}
}

func TestBusinessDayCalendarSnapshotRejectsBadTable(t *testing.T) {
	src := New()
	var buf bytes.Buffer
	if err := src.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	// Corrupt a holiday date; the import must fail.
	bad := strings.Replace(buf.String(), `"holidays":null`, `"holidays":["not-a-date"]`, 1)
	if _, err := ImportSnapshot(strings.NewReader(bad)); err == nil {
		t.Fatal("bad holiday: want import error, got nil")
	}
}
