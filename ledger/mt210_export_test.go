package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// mt210ExportTestLedger reuses the MT210 fixture — BICs on both
// legs (alice DEUTDEFF, bob NWBKGB2L) and one daily schedule,
// 10000 cents USD, next run 2026-11-12 09:00 UTC, a 250-cent
// per-run fee on its own leg — wired to an audit log, so the
// export marker event is assertable. Unlike the MT292 fixture,
// the schedule stays active: MT210 notifies the next run, it does
// not withdraw the notice.
func mt210ExportTestLedger(t *testing.T, al *AuditLog) (*Ledger, TransferSchedule) {
	t.Helper()
	l := New(WithAuditLog(al))
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

// mt210ExportTestOptions is the fixture export: the active
// schedule's notice to receive, serviced by CITIUS33.
func mt210ExportTestOptions(schedID string) MT210ExportOptions {
	return MT210ExportOptions{
		ScheduleID:   schedID,
		ServicingBIC: "CITIUS33",
	}
}

func TestExportMT210RegisteredFirstExportRegisters(t *testing.T) {
	al := mustAuditLog(t)
	l, sched := mt210ExportTestLedger(t, al)
	res, err := l.ExportMT210Registered(mt210ExportTestOptions(sched.ID), "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT210Registered: %v", err)
	}
	// The :20: is the schedule ID itself, stable across runs.
	if res.Reference != sched.ID || res.ScheduleID != sched.ID || res.RunSeq != sched.RunSeq {
		t.Errorf("result refs = %q/%q/%d, want %q/%q/%d", res.Reference, res.ScheduleID, res.RunSeq, sched.ID, sched.ID, sched.RunSeq)
	}
	if res.Duplicate {
		t.Error("duplicate = true on a first export")
	}
	want, err := l.ExportMT210(mt210ExportTestOptions(sched.ID))
	if err != nil {
		t.Fatalf("ExportMT210: %v", err)
	}
	if res.Message != want {
		t.Errorf("registered message differs from the pure render:\n%q\nwant:\n%q", res.Message, want)
	}
	// The stored export round-trips through the registry, verbatim.
	stored, ok := l.GetMT210Export(sched.ID)
	if !ok {
		t.Fatal("GetMT210Export = false, want the stored export")
	}
	if stored.Message != res.Message || stored.ScheduleID != sched.ID ||
		stored.RunSeq != sched.RunSeq || stored.Duplicate {
		t.Errorf("stored export = %+v, want the rendered message", stored)
	}
}

func TestExportMT210RegisteredRepeatIsDuplicate(t *testing.T) {
	al := mustAuditLog(t)
	l, sched := mt210ExportTestLedger(t, al)
	first, err := l.ExportMT210Registered(mt210ExportTestOptions(sched.ID), "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT210Registered: %v", err)
	}
	second, err := l.ExportMT210Registered(mt210ExportTestOptions(sched.ID), "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT210Registered repeat: %v", err)
	}
	if !second.Duplicate {
		t.Error("duplicate = false on repeat export")
	}
	if second.Message != first.Message {
		t.Errorf("repeat export differs:\nfirst:\n%s\nsecond:\n%s", first.Message, second.Message)
	}
	// A replay differing only in servicing BIC case renders the
	// byte-identical message and is the same terms, not a
	// conflict: the render uppercases the BIC.
	lower := mt210ExportTestOptions(sched.ID)
	lower.ServicingBIC = "citius33"
	third, err := l.ExportMT210Registered(lower, "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT210Registered with lowercase BIC: %v", err)
	}
	if !third.Duplicate || third.Message != first.Message {
		t.Errorf("BIC-case replay = duplicate %v, message differs from first", third.Duplicate)
	}
	// The replays emit no second audit event.
	events := readAuditOps(t, al)
	if countOp(events, "mt210_exported") != 1 {
		t.Errorf("mt210_exported count = %d, want 1 after replays", countOp(events, "mt210_exported"))
	}
}

func TestExportMT210RegisteredConflict(t *testing.T) {
	al := mustAuditLog(t)
	l, sched := mt210ExportTestLedger(t, al)
	first, err := l.ExportMT210Registered(mt210ExportTestOptions(sched.ID), "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT210Registered: %v", err)
	}
	// The same :20: and run with a different servicing BIC is a
	// conflict, not a second export.
	opts := mt210ExportTestOptions(sched.ID)
	opts.ServicingBIC = "DEUTDEFF"
	if _, err := l.ExportMT210Registered(opts, "ledger-operator"); !errors.Is(err, ErrMT210ExportMsgIDConflict) {
		t.Errorf("err = %v, want ErrMT210ExportMsgIDConflict", err)
	}
	// The conflict overwrote nothing: the stored message is still
	// the first rendering, and no second audit event landed.
	stored, ok := l.GetMT210Export(first.Reference)
	if !ok || stored.Message != first.Message {
		t.Errorf("stored export after conflict = %+v, want the first message", stored)
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt210_exported") != 1 {
		t.Errorf("mt210_exported count = %d, want 1 after conflict", countOp(events, "mt210_exported"))
	}
}

func TestExportMT210RegisteredRunSeqAdvanceSupersedes(t *testing.T) {
	al := mustAuditLog(t)
	l, sched := mt210ExportTestLedger(t, al)
	fundAccount(t, l, "alice", 100000, "USD")
	first, err := l.ExportMT210Registered(mt210ExportTestOptions(sched.ID), "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT210Registered: %v", err)
	}
	// The next run fires for real: RunSeq advances and NextRunAt
	// moves a day, so the next export is a new issue of the same
	// :20:, not a replay.
	nextRun := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	if runs := l.SweepDue(nextRun); len(runs) != 1 {
		t.Fatalf("sweep = %d runs, want 1 fired", len(runs))
	}
	got, _ := l.GetTransferSchedule(sched.ID)
	if got.RunSeq != sched.RunSeq+1 {
		t.Fatalf("run_seq = %d, want %d after the run fired", got.RunSeq, sched.RunSeq+1)
	}
	second, err := l.ExportMT210Registered(mt210ExportTestOptions(sched.ID), "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT210Registered after run fired: %v", err)
	}
	if second.Duplicate {
		t.Error("duplicate = true on a new run's first export")
	}
	if second.Reference != first.Reference {
		t.Errorf("reference = %q after run fired, want the same :20: %q", second.Reference, first.Reference)
	}
	if second.RunSeq != got.RunSeq {
		t.Errorf("run_seq = %d, want %d", second.RunSeq, got.RunSeq)
	}
	// The new issue names the new run: :21: moved to run/1 and
	// :30: to the next run date.
	for _, want := range []string{":21:sched-1/run/1", ":30:261113"} {
		if !strings.Contains(second.Message, want) {
			t.Errorf("superseding export missing %q, body:\n%s", want, second.Message)
		}
	}
	// The old record is overwritten: the registry holds the latest
	// issue, and the first rendering is gone.
	stored, ok := l.GetMT210Export(sched.ID)
	if !ok || stored.Message != second.Message {
		t.Errorf("stored export after supersede = %+v, want the second message", stored)
	}
	if stored.RunSeq != got.RunSeq || stored.Duplicate {
		t.Errorf("stored export = %+v, want run %d, duplicate false", stored, got.RunSeq)
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt210_exported") != 2 {
		t.Errorf("mt210_exported count = %d, want 2 (one per issue)", countOp(events, "mt210_exported"))
	}
}

func TestExportMT210RegisteredRefusedNotRegistered(t *testing.T) {
	t.Run("paused schedule", func(t *testing.T) {
		al := mustAuditLog(t)
		l, sched := mt210ExportTestLedger(t, al)
		if _, err := l.PauseTransferSchedule(sched.ID); err != nil {
			t.Fatalf("PauseTransferSchedule: %v", err)
		}
		if _, err := l.ExportMT210Registered(mt210ExportTestOptions(sched.ID), "ledger-operator"); !errors.Is(err, ErrMT210ScheduleNotActive) {
			t.Errorf("err = %v, want ErrMT210ScheduleNotActive", err)
		}
		if _, ok := l.GetMT210Export(sched.ID); ok {
			t.Error("GetMT210Export = true for a refused export, want false")
		}
		events := readAuditOps(t, al)
		if countOp(events, "mt210_exported") != 0 {
			t.Errorf("mt210_exported count = %d, want 0 for a refused export", countOp(events, "mt210_exported"))
		}
	})
	t.Run("FX schedule", func(t *testing.T) {
		al := mustAuditLog(t)
		l := New(WithAuditLog(al))
		if _, _, err := l.CreateTransferSchedule(TransferSchedule{
			ID: "sched-1", From: "alice", To: "bob",
			AmountCents: 10000, Currency: "USD", ToCurrency: "EUR",
			FXAccount: "fx", Interval: 24 * time.Hour,
			NextRunAt: time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC),
		}); err != nil {
			t.Fatalf("CreateTransferSchedule FX: %v", err)
		}
		if _, err := l.ExportMT210Registered(mt210ExportTestOptions("sched-1"), "ledger-operator"); !errors.Is(err, ErrInvalidMT210Export) {
			t.Errorf("err = %v, want ErrInvalidMT210Export", err)
		}
		if _, ok := l.GetMT210Export("sched-1"); ok {
			t.Error("GetMT210Export = true for a refused export, want false")
		}
		events := readAuditOps(t, al)
		if countOp(events, "mt210_exported") != 0 {
			t.Errorf("mt210_exported count = %d, want 0 for a refused export", countOp(events, "mt210_exported"))
		}
	})
}

func TestExportMT210RegisteredAuditEvent(t *testing.T) {
	al := mustAuditLog(t)
	l, sched := mt210ExportTestLedger(t, al)
	before := l.Version()
	res, err := l.ExportMT210Registered(mt210ExportTestOptions(sched.ID), "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT210Registered: %v", err)
	}
	// Audit: exactly one export marker, with the exact details —
	// the :20: on the durable record, no accounts.
	events := readAuditOps(t, al)
	if countOp(events, "mt210_exported") != 1 {
		t.Fatalf("mt210_exported count = %d, want 1", countOp(events, "mt210_exported"))
	}
	ev := findOp(events, "mt210_exported")
	if ev == nil {
		t.Fatal("mt210_exported event missing")
	}
	if ev.Actor != "ledger-operator" {
		t.Errorf("actor = %q, want ledger-operator", ev.Actor)
	}
	if ev.TraceID != res.Reference {
		t.Errorf("trace = %q, want %q", ev.TraceID, res.Reference)
	}
	// Details round-trip through JSON, so run_seq reads back as
	// a float64 (the schedule_*_amend precedent).
	if ev.Details["msg_id"] != res.Reference || ev.Details["schedule_id"] != sched.ID ||
		ev.Details["run_seq"] != float64(sched.RunSeq) ||
		ev.Details["sender_lt"] != "NWBKGB2LXXX" {
		t.Errorf("details = %v, want msg/schedule/run_seq/sender", ev.Details)
	}
	if len(ev.Accounts) != 0 {
		t.Errorf("accounts = %v, want none (a notice to receive touches no account)", ev.Accounts)
	}
	if ev.VersionBefore != before || ev.VersionAfter != before {
		t.Errorf("version %d -> %d, want %d -> %d (read-only, no bump)", ev.VersionBefore, ev.VersionAfter, before, before)
	}
	// Rendering is read-only: no version bump, no journal entry.
	if l.Version() != before {
		t.Errorf("version = %d, want %d (read-only, no bump)", l.Version(), before)
	}
	if got := len(l.Entries()); got != 0 {
		t.Errorf("journal entries = %d, want 0 (registration books nothing)", got)
	}
}

func TestGetMT210ExportUnknown(t *testing.T) {
	al := mustAuditLog(t)
	l, _ := mt210ExportTestLedger(t, al)
	if _, ok := l.GetMT210Export("NEVER-EXPORTED"); ok {
		t.Error("GetMT210Export = true for an unknown :20:, want false")
	}
}
