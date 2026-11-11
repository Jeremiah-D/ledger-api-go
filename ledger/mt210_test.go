package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// mt210TestLedger builds a ledger with registered BICs on both legs
// (alice DEUTDEFF, bob NWBKGB2L) and one daily schedule: 10000
// cents USD, next run 2026-11-12 09:00 UTC, a 250-cent per-run fee
// on its own leg. The schedule ID is short enough that the run
// transfer ID ("sched-1/run/0") stays within the 16x :21: limit.
func mt210TestLedger(t *testing.T) (*Ledger, TransferSchedule) {
	t.Helper()
	l := New()
	if err := l.SetAccountIBAN("alice", "DE89370400440532013000", "DEUTDEFF"); err != nil {
		t.Fatalf("SetAccountIBAN alice: %v", err)
	}
	if err := l.SetAccountIBAN("bob", "GB29NWBK60161331926819", "NWBKGB2L"); err != nil {
		t.Fatalf("SetAccountIBAN bob: %v", err)
	}
	nextRun := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	sched, _, err := l.CreateTransferSchedule(TransferSchedule{
		ID:          "sched-1",
		From:        "alice",
		To:          "bob",
		AmountCents: 10000,
		Currency:    "USD",
		FeeCents:    250,
		FeeAccount:  "fee-revenue",
		Interval:    24 * time.Hour,
		NextRunAt:   nextRun,
	})
	if err != nil {
		t.Fatalf("CreateTransferSchedule: %v", err)
	}
	return l, sched
}

func TestExportMT210ExactTags(t *testing.T) {
	l, sched := mt210TestLedger(t)
	doc, err := l.ExportMT210(MT210ExportOptions{ScheduleID: sched.ID, ServicingBIC: "CITIUS33"})
	if err != nil {
		t.Fatalf("ExportMT210: %v", err)
	}
	// Block framing: sender LT (To BIC, normalized), receiver LT
	// (servicing BIC), MUR = :20:.
	for _, want := range []string{
		"{1:F01NWBKGB2LXXX0000000000}",
		"{2:I210CITIUS33XXXU3003}",
		"{3:{108:sched-1}}",
		"{4:",
		":20:sched-1",
		// :25: is the To account's registered IBAN.
		":25:GB29NWBK60161331926819",
		// :30: is the next run date, 6!n YYMMDD.
		":30:261112",
		// :21: is the run transfer ID "<ID>/run/<seq>".
		":21:sched-1/run/0",
		// :32B: is the principal: the 250-cent fee is a separate
		// leg and never reduces it.
		":32B:USD100,00",
		// From has a registered BIC, so the ordering institution
		// renders :52A: — never :50D:.
		":52A:DEUTDEFF",
		"-}",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("export missing %q, body:\n%s", want, doc)
		}
	}
	// No :50D: alongside :52A:, no :56a:, no :72: — the repeating
	// sequence stops at :56a: and carries no memo.
	for _, banned := range []string{":50D:", ":50A:", ":50K:", ":56A:", ":56D:", ":72:"} {
		if strings.Contains(doc, banned) {
			t.Errorf("export must not carry %s, body:\n%s", banned, doc)
		}
	}
	if strings.HasSuffix(doc, "\n") {
		t.Error("export must not end with a trailing newline")
	}
}

func TestExportMT210FeeNotDeducted(t *testing.T) {
	l, sched := mt210TestLedger(t)
	doc, err := l.ExportMT210(MT210ExportOptions{ScheduleID: sched.ID, ServicingBIC: "CITIUS33"})
	if err != nil {
		t.Fatalf("ExportMT210: %v", err)
	}
	// The schedule carries a 250-cent fee; the notice still states
	// the full 10000-cent principal — the fee books separately.
	if !strings.Contains(doc, ":32B:USD100,00") {
		t.Errorf("export :32B: must equal the full principal, body:\n%s", doc)
	}
	if strings.Contains(doc, "97,50") {
		t.Errorf("export must not deduct the fee from :32B:, body:\n%s", doc)
	}
}

func TestExportMT210NoIBANFallsBackToAccountID(t *testing.T) {
	l := New()
	if err := l.SetAccountIBAN("alice", "DE89370400440532013000", "DEUTDEFF"); err != nil {
		t.Fatalf("SetAccountIBAN alice: %v", err)
	}
	// "BOBACCT1" is an LT-form account ID (8 alphanumeric
	// characters) with no IBAN registration: :25: falls back to
	// the account ID and block 1 takes its normalized LT.
	if _, _, err := l.CreateTransferSchedule(TransferSchedule{
		ID: "sched-1", From: "alice", To: "BOBACCT1",
		AmountCents: 10000, Currency: "USD", Interval: 24 * time.Hour,
		NextRunAt: time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("CreateTransferSchedule: %v", err)
	}
	doc, err := l.ExportMT210(MT210ExportOptions{ScheduleID: "sched-1", ServicingBIC: "CITIUS33"})
	if err != nil {
		t.Fatalf("ExportMT210: %v", err)
	}
	for _, want := range []string{
		"{1:F01BOBACCT1XXX0000000000}",
		":25:BOBACCT1",
		":52A:DEUTDEFF",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("export missing %q, body:\n%s", want, doc)
		}
	}
}

func TestExportMT210NoBICFallsBackTo50D(t *testing.T) {
	l := New()
	if err := l.SetAccountIBAN("bob", "GB29NWBK60161331926819", "NWBKGB2L"); err != nil {
		t.Fatalf("SetAccountIBAN bob: %v", err)
	}
	// "alice" has no registered BIC: the ordering customer renders
	// :50D: with the account ID — and :52A: must not appear.
	if _, _, err := l.CreateTransferSchedule(TransferSchedule{
		ID: "sched-1", From: "alice", To: "bob",
		AmountCents: 10000, Currency: "USD", Interval: 24 * time.Hour,
		NextRunAt: time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("CreateTransferSchedule: %v", err)
	}
	doc, err := l.ExportMT210(MT210ExportOptions{ScheduleID: "sched-1", ServicingBIC: "CITIUS33"})
	if err != nil {
		t.Fatalf("ExportMT210: %v", err)
	}
	if !strings.Contains(doc, ":50D:ALICE") {
		t.Errorf("export missing :50D:ALICE, body:\n%s", doc)
	}
	if strings.Contains(doc, ":52A:") {
		t.Errorf("export must not carry :52A: without a From BIC, body:\n%s", doc)
	}
}

func TestExportMT210NotActiveStatuses(t *testing.T) {
	// Paused, cancelled, and completed schedules each report
	// ErrMT210ScheduleNotActive: none of them has a next run to
	// notify.
	t.Run("paused", func(t *testing.T) {
		l, sched := mt210TestLedger(t)
		if _, err := l.PauseTransferSchedule(sched.ID); err != nil {
			t.Fatalf("PauseTransferSchedule: %v", err)
		}
		_, err := l.ExportMT210(MT210ExportOptions{ScheduleID: sched.ID, ServicingBIC: "CITIUS33"})
		if !errors.Is(err, ErrMT210ScheduleNotActive) {
			t.Errorf("err = %v, want ErrMT210ScheduleNotActive", err)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		l, sched := mt210TestLedger(t)
		if _, err := l.CancelTransferSchedule(sched.ID); err != nil {
			t.Fatalf("CancelTransferSchedule: %v", err)
		}
		_, err := l.ExportMT210(MT210ExportOptions{ScheduleID: sched.ID, ServicingBIC: "CITIUS33"})
		if !errors.Is(err, ErrMT210ScheduleNotActive) {
			t.Errorf("err = %v, want ErrMT210ScheduleNotActive", err)
		}
	})
	t.Run("completed", func(t *testing.T) {
		l := New()
		now := time.Now().UTC().Truncate(time.Second)
		fundAccount(t, l, "alice", 100000, "USD")
		s := testSchedule(now)
		s.EndsAt = now.Add(90 * time.Minute) // two runs fit: T+0, T+60m
		if _, _, err := l.CreateTransferSchedule(s); err != nil {
			t.Fatalf("create: %v", err)
		}
		if runs := l.SweepDue(now); len(runs) != 1 {
			t.Fatalf("first sweep = %d runs", len(runs))
		}
		if runs := l.SweepDue(now.Add(time.Hour)); len(runs) != 1 {
			t.Fatalf("second sweep = %d runs", len(runs))
		}
		got, _ := l.GetTransferSchedule("sched-1")
		if got.Status != ScheduleCompleted {
			t.Fatalf("status = %q, want completed", got.Status)
		}
		_, err := l.ExportMT210(MT210ExportOptions{ScheduleID: "sched-1", ServicingBIC: "CITIUS33"})
		if !errors.Is(err, ErrMT210ScheduleNotActive) {
			t.Errorf("err = %v, want ErrMT210ScheduleNotActive", err)
		}
	})
}

func TestExportMT210EndsAtExhausted(t *testing.T) {
	l, sched := mt210TestLedger(t)
	// White-box the exhausted state: the schedule is still
	// active, but its NextRunAt has moved past EndsAt, so no run
	// is left to notify. (The public API cannot reach this state:
	// AmendNextRunAt and AmendEndsAt both guard the boundary, and
	// SweepDue completes the schedule instead — the export's guard
	// is defensive, and the test pins it directly.)
	nextRun := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	l.schedules[sched.ID].EndsAt = nextRun.Add(2 * time.Hour)
	l.schedules[sched.ID].NextRunAt = nextRun.Add(3 * time.Hour)
	_, err := l.ExportMT210(MT210ExportOptions{ScheduleID: sched.ID, ServicingBIC: "CITIUS33"})
	if !errors.Is(err, ErrMT210ScheduleNotActive) {
		t.Errorf("err = %v, want ErrMT210ScheduleNotActive", err)
	}
}

func TestExportMT210FXScheduleRejected(t *testing.T) {
	l := New()
	if _, _, err := l.CreateTransferSchedule(TransferSchedule{
		ID: "sched-1", From: "alice", To: "bob",
		AmountCents: 10000, Currency: "USD", ToCurrency: "EUR",
		FXAccount: "fx", Interval: 24 * time.Hour,
		NextRunAt: time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("CreateTransferSchedule FX: %v", err)
	}
	_, err := l.ExportMT210(MT210ExportOptions{ScheduleID: "sched-1", ServicingBIC: "CITIUS33"})
	if !errors.Is(err, ErrInvalidMT210Export) {
		t.Errorf("err = %v, want ErrInvalidMT210Export (receivable unknowable until fire time)", err)
	}
}

func TestExportMT210UnknownSchedule(t *testing.T) {
	l, _ := mt210TestLedger(t)
	_, err := l.ExportMT210(MT210ExportOptions{ScheduleID: "sched-nope", ServicingBIC: "CITIUS33"})
	if !errors.Is(err, ErrScheduleNotFound) {
		t.Errorf("err = %v, want ErrScheduleNotFound", err)
	}
}

func TestExportMT210BadServicingBIC(t *testing.T) {
	l, sched := mt210TestLedger(t)
	for _, bic := range []string{"", "CITI", "CITIUS33XXXX", "CITIUS3!"} {
		_, err := l.ExportMT210(MT210ExportOptions{ScheduleID: sched.ID, ServicingBIC: bic})
		if !errors.Is(err, ErrInvalidMT210Export) {
			t.Errorf("BIC %q: err = %v, want ErrInvalidMT210Export", bic, err)
		}
	}
}

func TestExportMT210RunIDTooLong(t *testing.T) {
	l := New()
	// The schedule ID itself fits :20: (12 characters), but the
	// run transfer ID "<ID>/run/0" is 18 — past the 16x :21:
	// limit, so the notice fails loud instead of truncating.
	if _, _, err := l.CreateTransferSchedule(TransferSchedule{
		ID: "sched-210-01", From: "alice", To: "bob",
		AmountCents: 10000, Currency: "USD", Interval: 24 * time.Hour,
		NextRunAt: time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("CreateTransferSchedule: %v", err)
	}
	_, err := l.ExportMT210(MT210ExportOptions{ScheduleID: "sched-210-01", ServicingBIC: "CITIUS33"})
	if !errors.Is(err, ErrInvalidMT210Export) {
		t.Errorf("err = %v, want ErrInvalidMT210Export (run ID over 16x)", err)
	}
}

func TestExportMT210RepeatIdentical(t *testing.T) {
	l, sched := mt210TestLedger(t)
	opts := MT210ExportOptions{ScheduleID: sched.ID, ServicingBIC: "CITIUS33"}
	first, err := l.ExportMT210(opts)
	if err != nil {
		t.Fatalf("first ExportMT210: %v", err)
	}
	second, err := l.ExportMT210(opts)
	if err != nil {
		t.Fatalf("second ExportMT210: %v", err)
	}
	if first != second {
		t.Errorf("repeat export differs:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}
