package ledger

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

// valueDateTestEntry builds a minimal valid entry with an explicit
// booking date, so value-date tests never depend on the wall clock.
func valueDateTestEntry(id string, booking time.Time) JournalEntry {
	return JournalEntry{
		ID:            id,
		DebitAccount:  "cash",
		CreditAccount: "equity",
		AmountCents:   1000,
		Currency:      "USD",
		CreatedAt:     booking,
	}
}

// The tests below pin to real calendar dates: 2026-10-23 is a Friday,
// 2026-10-24 a Saturday, 2026-10-25 a Sunday, 2026-10-26 a Monday.
var (
	vdFriday   = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	vdSaturday = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	vdSunday   = time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	vdMonday   = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	vdTuesday  = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
)

func TestValueDateDefaultEqualsBooking(t *testing.T) {
	l := New()
	e := valueDateTestEntry("vd-default", vdFriday)
	posted, dup, err := l.Post(e)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if dup {
		t.Fatal("first post must not be a duplicate")
	}
	// No explicit value date: nothing extra is journaled, and the
	// effective value date is the booking date.
	if !posted.ValueDate.IsZero() {
		t.Errorf("posted.ValueDate = %v, want zero (default = booking date)", posted.ValueDate)
	}
	if got := posted.EffectiveValueDate(); !got.Equal(vdFriday) {
		t.Errorf("EffectiveValueDate = %v, want booking date %v", got, vdFriday)
	}
	if err := l.VerifyChain(); err != nil {
		t.Errorf("VerifyChain: %v", err)
	}
}

func TestValueDateExplicitBusinessDay(t *testing.T) {
	l := New()
	e := valueDateTestEntry("vd-explicit", vdFriday)
	e.ValueDate = vdMonday // next Monday, a business day, no convention
	posted, _, err := l.Post(e)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if !posted.ValueDate.Equal(vdMonday) {
		t.Errorf("posted.ValueDate = %v, want %v", posted.ValueDate, vdMonday)
	}
	if got := posted.EffectiveValueDate(); !got.Equal(vdMonday) {
		t.Errorf("EffectiveValueDate = %v, want %v", got, vdMonday)
	}
	if err := l.VerifyChain(); err != nil {
		t.Errorf("VerifyChain: %v", err)
	}
}

func TestValueDateFollowingAcrossWeekend(t *testing.T) {
	l := New()
	e := valueDateTestEntry("vd-following", vdFriday)
	e.ValueDate = vdSaturday
	e.ValueDateConvention = ValueDateConventionFollowing
	posted, _, err := l.Post(e)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	// Saturday + following = Monday.
	if !posted.ValueDate.Equal(vdMonday) {
		t.Errorf("posted.ValueDate = %v, want Monday %v", posted.ValueDate, vdMonday)
	}
	if err := l.VerifyChain(); err != nil {
		t.Errorf("VerifyChain: %v", err)
	}
}

func TestValueDatePrecedingAcrossWeekend(t *testing.T) {
	l := New()
	// Booking on Thursday so the preceding-adjusted Friday does not
	// collapse to the booking date.
	thursday := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	e := valueDateTestEntry("vd-preceding", thursday)
	e.ValueDate = vdSunday
	e.ValueDateConvention = ValueDateConventionPreceding
	posted, _, err := l.Post(e)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	// Sunday + preceding = Friday.
	if !posted.ValueDate.Equal(vdFriday) {
		t.Errorf("posted.ValueDate = %v, want Friday %v", posted.ValueDate, vdFriday)
	}
}

func TestValueDateFollowingSkipsHoliday(t *testing.T) {
	l := New()
	// The ledger's LG-95 calendar is the only date source: a custom
	// holiday on the Monday pushes following to Tuesday.
	if err := l.AddHoliday("2026-10-26"); err != nil {
		t.Fatalf("AddHoliday: %v", err)
	}
	e := valueDateTestEntry("vd-holiday", vdFriday)
	e.ValueDate = vdSaturday
	e.ValueDateConvention = ValueDateConventionFollowing
	posted, _, err := l.Post(e)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if !posted.ValueDate.Equal(vdTuesday) {
		t.Errorf("posted.ValueDate = %v, want Tuesday %v (holiday-skipped)", posted.ValueDate, vdTuesday)
	}
}

func TestValueDateBusinessDayNeedsNoAdjustment(t *testing.T) {
	l := New()
	// A convention on a date that is already a business day is a no-op:
	// the journaled date equals the requested date.
	e := valueDateTestEntry("vd-noop", vdFriday)
	e.ValueDate = vdMonday
	e.ValueDateConvention = ValueDateConventionFollowing
	posted, _, err := l.Post(e)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if !posted.ValueDate.Equal(vdMonday) {
		t.Errorf("posted.ValueDate = %v, want %v", posted.ValueDate, vdMonday)
	}
}

func TestValueDateEqualsBookingStoredZero(t *testing.T) {
	l := New()
	e := valueDateTestEntry("vd-equal", vdFriday)
	e.ValueDate = vdFriday // explicit, but identical to the booking date
	posted, _, err := l.Post(e)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	// Resolves to the booking date: stored as zero, so the audit-chain
	// section is skipped exactly like a plain posting.
	if !posted.ValueDate.IsZero() {
		t.Errorf("posted.ValueDate = %v, want zero (equals booking date)", posted.ValueDate)
	}
	if err := l.VerifyChain(); err != nil {
		t.Errorf("VerifyChain: %v", err)
	}
}

func TestValueDateWindowViolationFailClosed(t *testing.T) {
	l := New()
	before := l.Version()
	// 60 days ahead of the booking date: past the default 30-day
	// leading edge.
	e := valueDateTestEntry("vd-window-ahead", vdFriday)
	e.ValueDate = vdFriday.Add(60 * 24 * time.Hour)
	if _, _, err := l.Post(e); !errors.Is(err, ErrValueDateOutOfWindow) {
		t.Fatalf("Post ahead = %v, want ErrValueDateOutOfWindow", err)
	}
	// 60 days back: past the trailing edge too.
	e = valueDateTestEntry("vd-window-back", vdFriday)
	e.ValueDate = vdFriday.Add(-60 * 24 * time.Hour)
	if _, _, err := l.Post(e); !errors.Is(err, ErrValueDateOutOfWindow) {
		t.Fatalf("Post back = %v, want ErrValueDateOutOfWindow", err)
	}
	// Fail-closed: nothing was journaled — no row, no chain link, no
	// version bump.
	if l.Version() != before {
		t.Errorf("version = %d, want %d (nothing journaled)", l.Version(), before)
	}
	if _, ok := l.entries["vd-window-ahead"]; ok {
		t.Error("window-violating entry was journaled")
	}
	if err := l.VerifyChain(); err != nil {
		t.Errorf("VerifyChain: %v", err)
	}
}

func TestValueDateWindowEdgesAreInclusive(t *testing.T) {
	l := New()
	// Exactly on the 30-day edges: admitted.
	e := valueDateTestEntry("vd-edge-ahead", vdFriday)
	e.ValueDate = vdFriday.Add(30 * 24 * time.Hour)
	if _, _, err := l.Post(e); err != nil {
		t.Errorf("Post on leading edge: %v", err)
	}
	e = valueDateTestEntry("vd-edge-back", vdFriday)
	e.ValueDate = vdFriday.Add(-30 * 24 * time.Hour)
	if _, _, err := l.Post(e); err != nil {
		t.Errorf("Post on trailing edge: %v", err)
	}
}

func TestValueDateInvalidConvention(t *testing.T) {
	l := New()
	e := valueDateTestEntry("vd-bad-conv", vdFriday)
	e.ValueDate = vdMonday
	e.ValueDateConvention = "nearest"
	if _, _, err := l.Post(e); !errors.Is(err, ErrInvalidValueDateConvention) {
		t.Fatalf("Post = %v, want ErrInvalidValueDateConvention", err)
	}
}

func TestValueDateConventionWithoutDate(t *testing.T) {
	l := New()
	// A convention with no value date to adjust is a caller bug:
	// fail-closed at field validation, before anything is recorded.
	e := valueDateTestEntry("vd-conv-nodate", vdFriday)
	e.ValueDateConvention = ValueDateConventionFollowing
	if _, _, err := l.Post(e); !errors.Is(err, ErrInvalidValueDateConvention) {
		t.Fatalf("Post = %v, want ErrInvalidValueDateConvention", err)
	}
}

func TestValueDateChainTamperDetected(t *testing.T) {
	l := New()
	e := valueDateTestEntry("vd-tamper", vdFriday)
	e.ValueDate = vdMonday
	if _, _, err := l.Post(e); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain before tamper: %v", err)
	}
	// Rewrite the journaled value date after posting: the chain must
	// break exactly like a rewritten amount.
	tampered := l.entries["vd-tamper"]
	tampered.ValueDate = vdTuesday
	l.entries["vd-tamper"] = tampered
	if err := l.VerifyChain(); err == nil {
		t.Fatal("VerifyChain passed after rewriting the journaled value date")
	}
}

func TestValueDateWindowConfig(t *testing.T) {
	l := New()
	ahead, back := l.ValueDateWindow()
	if ahead != 30*24*time.Hour || back != 30*24*time.Hour {
		t.Fatalf("default window = +%v/-%v, want +720h/-720h", ahead, back)
	}
	if err := l.SetValueDateWindow(48*time.Hour, 48*time.Hour); err != nil {
		t.Fatalf("SetValueDateWindow: %v", err)
	}
	ahead, back = l.ValueDateWindow()
	if ahead != 48*time.Hour || back != 48*time.Hour {
		t.Fatalf("window = +%v/-%v, want +48h/-48h", ahead, back)
	}
	// 3 days ahead now exceeds the tightened window.
	e := valueDateTestEntry("vd-tight", vdFriday)
	e.ValueDate = vdFriday.Add(72 * time.Hour)
	if _, _, err := l.Post(e); !errors.Is(err, ErrValueDateOutOfWindow) {
		t.Fatalf("Post = %v, want ErrValueDateOutOfWindow", err)
	}
	if err := l.SetValueDateWindow(-time.Hour, time.Hour); !errors.Is(err, ErrInvalidValueDateWindow) {
		t.Fatalf("SetValueDateWindow negative = %v, want ErrInvalidValueDateWindow", err)
	}
}

func TestValueDateWindowOptionPanicsOnNegative(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("WithValueDateWindow(-1, 1) did not panic")
		}
	}()
	_ = New(WithValueDateWindow(-time.Hour, time.Hour))
}

func TestValueDateBatchResolution(t *testing.T) {
	l := New()
	// Batch legs resolve like Post: Saturday + following = Monday.
	b := Batch{
		ID: "vd-batch",
		Entries: []JournalEntry{
			{ID: "vd-batch-1", DebitAccount: "cash", CreditAccount: "equity", AmountCents: 100, Currency: "USD",
				ValueDate: vdSaturday, ValueDateConvention: ValueDateConventionFollowing, CreatedAt: vdFriday},
		},
	}
	rec, err := l.PostBatch(b)
	if err != nil {
		t.Fatalf("PostBatch: %v", err)
	}
	if !rec.Entries[0].ValueDate.Equal(vdMonday) {
		t.Errorf("batch leg ValueDate = %v, want Monday %v", rec.Entries[0].ValueDate, vdMonday)
	}
	// A window-violating leg fails the whole batch closed.
	b = Batch{
		ID: "vd-batch-bad",
		Entries: []JournalEntry{
			{ID: "vd-batch-2", DebitAccount: "cash", CreditAccount: "equity", AmountCents: 100, Currency: "USD",
				ValueDate: vdFriday.Add(60 * 24 * time.Hour), CreatedAt: vdFriday},
		},
	}
	if _, err := l.PostBatch(b); !errors.Is(err, ErrValueDateOutOfWindow) {
		t.Fatalf("PostBatch = %v, want ErrValueDateOutOfWindow", err)
	}
	if _, ok := l.entries["vd-batch-2"]; ok {
		t.Error("window-violating batch leg was journaled")
	}
}

func TestValueDateDryRunResolves(t *testing.T) {
	l := New()
	e := valueDateTestEntry("vd-dryrun", vdFriday)
	e.ValueDate = vdSaturday
	e.ValueDateConvention = ValueDateConventionFollowing
	res, err := l.DryRunPost(e)
	if err != nil {
		t.Fatalf("DryRunPost: %v", err)
	}
	if !res.WouldSucceed {
		t.Fatal("dry run should succeed")
	}
	if len(res.Legs) != 1 {
		t.Fatalf("legs = %d, want 1", len(res.Legs))
	}
	// The dry run resolves the same convention the real Post would
	// journal — against the same calendar.
	if !res.Legs[0].ValueDate.Equal(vdMonday) {
		t.Errorf("dry-run leg ValueDate = %v, want Monday %v", res.Legs[0].ValueDate, vdMonday)
	}
	// And the dry run journals nothing.
	if l.Version() != 0 {
		t.Errorf("version = %d, want 0 (dry run journals nothing)", l.Version())
	}
}

func TestValueDateSnapshotRoundTrip(t *testing.T) {
	l := New(WithValueDateWindow(7*24*time.Hour, 14*24*time.Hour))
	e := valueDateTestEntry("vd-snap", vdFriday)
	e.ValueDate = vdMonday
	if _, _, err := l.Post(e); err != nil {
		t.Fatalf("Post: %v", err)
	}
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	l2, err := ImportSnapshot(&buf)
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	// The window restores verbatim.
	ahead, back := l2.ValueDateWindow()
	if ahead != 7*24*time.Hour || back != 14*24*time.Hour {
		t.Errorf("restored window = +%v/-%v, want +168h/-336h", ahead, back)
	}
	// The journaled value date restores, and the restored chain
	// verifies over it.
	got := l2.entries["vd-snap"]
	if !got.ValueDate.Equal(vdMonday) {
		t.Errorf("restored ValueDate = %v, want %v", got.ValueDate, vdMonday)
	}
	if err := l2.VerifyChain(); err != nil {
		t.Errorf("VerifyChain after restore: %v", err)
	}
	// The restored window still gates: 20 days ahead now exceeds 7.
	e = valueDateTestEntry("vd-snap-bad", vdFriday)
	e.ValueDate = vdFriday.Add(20 * 24 * time.Hour)
	if _, _, err := l2.Post(e); !errors.Is(err, ErrValueDateOutOfWindow) {
		t.Errorf("Post on restored ledger = %v, want ErrValueDateOutOfWindow", err)
	}
}

func TestAccrualValueDateAnchoring(t *testing.T) {
	l := New()
	// Book the fee accrual with booking date 2026-10-20 (a Tuesday)
	// and an explicit value date of 2026-10-26 (the next Monday).
	asof := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	vd := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	posted, dup, err := l.AccrueFeeWithValueDate("fee-exp", "fee-pay", 500, asof, vd, ValueDateConventionNone, 0)
	if err != nil {
		t.Fatalf("AccrueFeeWithValueDate: %v", err)
	}
	if dup {
		t.Fatal("first accrual must not be a duplicate")
	}
	if !posted.ValueDate.Equal(vd) {
		t.Errorf("accrual ValueDate = %v, want %v", posted.ValueDate, vd)
	}
	// The AccruedTotal window anchors on the value date: the accrual
	// lands in the value-date week, not the booking week.
	inValueWeek, err := l.AccruedTotal("fee-exp", "fee-pay", AccrualKindFee,
		time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("AccruedTotal: %v", err)
	}
	if inValueWeek != 500 {
		t.Errorf("AccruedTotal(value week) = %d, want 500", inValueWeek)
	}
	inBookingWeek, err := l.AccruedTotal("fee-exp", "fee-pay", AccrualKindFee,
		time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("AccruedTotal: %v", err)
	}
	if inBookingWeek != 0 {
		t.Errorf("AccruedTotal(booking week) = %d, want 0 (anchored on value date)", inBookingWeek)
	}
	// The per-day idempotency still anchors on the booking day: a
	// second run of the same booking day replays, it does not double
	// the total.
	if _, dup, err := l.AccrueFeeWithValueDate("fee-exp", "fee-pay", 500, asof, vd, ValueDateConventionNone, 0); err != nil || !dup {
		t.Fatalf("replay = (dup=%v, err=%v), want dup=true", dup, err)
	}
	// A plain accrual without a value date anchors on its booking date,
	// exactly as before LG-181.
	if _, _, err := l.AccrueFee("fee-exp2", "fee-pay2", 300, asof, 0); err != nil {
		t.Fatalf("AccrueFee: %v", err)
	}
	inBookingWeek, err = l.AccruedTotal("fee-exp2", "fee-pay2", AccrualKindFee,
		time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("AccruedTotal: %v", err)
	}
	if inBookingWeek != 300 {
		t.Errorf("AccruedTotal(booking week, plain) = %d, want 300", inBookingWeek)
	}
	if err := l.VerifyChain(); err != nil {
		t.Errorf("VerifyChain: %v", err)
	}
}

func TestAccrueInterestWithValueDate(t *testing.T) {
	l := New()
	asof := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	// Saturday value date with following: the accrual journals Monday's
	// date while the deterministic ID keeps the booking day.
	posted, _, err := l.AccrueInterestWithValueDate("int-exp", "int-pay", 3650000, 100, asof, vdSaturday, ValueDateConventionFollowing, 0)
	if err != nil {
		t.Fatalf("AccrueInterestWithValueDate: %v", err)
	}
	if !posted.ValueDate.Equal(vdMonday) {
		t.Errorf("accrual ValueDate = %v, want Monday %v", posted.ValueDate, vdMonday)
	}
	wantID := "accrual/interest/int-exp/int-pay/2026-10-20"
	if posted.ID != wantID {
		t.Errorf("accrual ID = %q, want %q (booking day)", posted.ID, wantID)
	}
	// The amount is the ordinary daily math: floor(3650000*100/10000/365)
	// = 100 cents.
	if posted.AmountCents != 100 {
		t.Errorf("accrual amount = %d, want 100", posted.AmountCents)
	}
	// A value date past the window fails the accrual closed. A fresh
	// booking day keeps the deterministic ID distinct, so the run
	// reaches the window gate instead of the idempotency replay.
	asof2 := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	if _, _, err := l.AccrueInterestWithValueDate("int-exp", "int-pay", 3650000, 100, asof2, asof2.Add(60*24*time.Hour), ValueDateConventionNone, 0); !errors.Is(err, ErrValueDateOutOfWindow) {
		t.Errorf("accrual = %v, want ErrValueDateOutOfWindow", err)
	}
	if err := l.VerifyChain(); err != nil {
		t.Errorf("VerifyChain: %v", err)
	}
}
