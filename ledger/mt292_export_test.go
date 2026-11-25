package ledger

import (
	"errors"
	"testing"
	"time"
)

// mt292ExportTestLedger reuses the MT210 fixture — BICs on both
// legs (alice DEUTDEFF, bob NWBKGB2L) and one daily schedule,
// 10000 cents USD, next run 2026-11-12 09:00 UTC — wired to an
// audit log, so the export marker event is assertable. Callers
// pause the schedule before exporting: MT292 is the withdrawal
// of that notice, and only a schedule whose notification premise
// is dead exports.
func mt292ExportTestLedger(t *testing.T, al *AuditLog) (*Ledger, TransferSchedule) {
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

// mt292ExportTestOptions is the fixture export: the paused
// schedule's cancellation request to CITIUS33 for the original
// MT210 of 2026-11-10, with a reason.
func mt292ExportTestOptions(schedID string) MT292ExportOptions {
	return MT292ExportOptions{
		ScheduleID:   schedID,
		ServicingBIC: "CITIUS33",
		OriginalDate: mt292OriginalDate(),
		Reason:       "Schedule paused by operator",
	}
}

func TestExportMT292RegisteredFirstExportRegisters(t *testing.T) {
	al := mustAuditLog(t)
	l, sched := mt292ExportTestLedger(t, al)
	if _, err := l.PauseTransferSchedule(sched.ID); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	res, err := l.ExportMT292Registered(mt292ExportTestOptions(sched.ID), "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT292Registered: %v", err)
	}
	wantRef := mt292ExpectedRef(sched.ID, sched.RunSeq)
	if res.Reference != wantRef {
		t.Errorf("reference = %q, want %q", res.Reference, wantRef)
	}
	if res.RelatedReference != sched.ID || res.ScheduleID != sched.ID || res.RunSeq != sched.RunSeq {
		t.Errorf("result refs = %q/%q/%d, want %q/%q/%d", res.RelatedReference, res.ScheduleID, res.RunSeq, sched.ID, sched.ID, sched.RunSeq)
	}
	if res.Duplicate {
		t.Error("duplicate = true on a first export")
	}
	want, err := l.ExportMT292(mt292ExportTestOptions(sched.ID))
	if err != nil {
		t.Fatalf("ExportMT292: %v", err)
	}
	if res.Message != want {
		t.Errorf("registered message differs from the pure render:\n%q\nwant:\n%q", res.Message, want)
	}
}

func TestGetMT292ExportReturnsStored(t *testing.T) {
	al := mustAuditLog(t)
	l, sched := mt292ExportTestLedger(t, al)
	if _, err := l.PauseTransferSchedule(sched.ID); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	res, err := l.ExportMT292Registered(mt292ExportTestOptions(sched.ID), "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT292Registered: %v", err)
	}
	// The stored export round-trips through the registry, verbatim.
	stored, ok := l.GetMT292Export(res.Reference)
	if !ok {
		t.Fatal("GetMT292Export = false, want the stored export")
	}
	if stored.Message != res.Message || stored.RelatedReference != sched.ID ||
		stored.ScheduleID != sched.ID || stored.RunSeq != sched.RunSeq || stored.Duplicate {
		t.Errorf("stored export = %+v, want the rendered message", stored)
	}
}

func TestExportMT292RegisteredRepeatIsDuplicate(t *testing.T) {
	al := mustAuditLog(t)
	l, sched := mt292ExportTestLedger(t, al)
	if _, err := l.PauseTransferSchedule(sched.ID); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	first, err := l.ExportMT292Registered(mt292ExportTestOptions(sched.ID), "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT292Registered: %v", err)
	}
	second, err := l.ExportMT292Registered(mt292ExportTestOptions(sched.ID), "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT292Registered repeat: %v", err)
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
	lower := mt292ExportTestOptions(sched.ID)
	lower.ServicingBIC = "citius33"
	third, err := l.ExportMT292Registered(lower, "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT292Registered with lowercase BIC: %v", err)
	}
	if !third.Duplicate || third.Message != first.Message {
		t.Errorf("BIC-case replay = duplicate %v, message differs from first", third.Duplicate)
	}
	// The replays emit no second audit event.
	events := readAuditOps(t, al)
	if countOp(events, "mt292_exported") != 1 {
		t.Errorf("mt292_exported count = %d, want 1 after replays", countOp(events, "mt292_exported"))
	}
}

func TestExportMT292RegisteredConflict(t *testing.T) {
	al := mustAuditLog(t)
	l, sched := mt292ExportTestLedger(t, al)
	if _, err := l.PauseTransferSchedule(sched.ID); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	first, err := l.ExportMT292Registered(mt292ExportTestOptions(sched.ID), "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT292Registered: %v", err)
	}
	cases := []struct {
		name string
		mut  func(*MT292ExportOptions)
	}{
		{"different servicing BIC", func(o *MT292ExportOptions) { o.ServicingBIC = "DEUTDEFF" }},
		{"different original date", func(o *MT292ExportOptions) { o.OriginalDate = time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC) }},
		{"different reason", func(o *MT292ExportOptions) { o.Reason = "Operator changed their mind" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := mt292ExportTestOptions(sched.ID)
			tc.mut(&opts)
			if _, err := l.ExportMT292Registered(opts, "ledger-operator"); !errors.Is(err, ErrMT292ExportMsgIDConflict) {
				t.Errorf("err = %v, want ErrMT292ExportMsgIDConflict", err)
			}
		})
	}
	// The conflicts overwrote nothing: the stored message is still
	// the first rendering, and no second audit event landed.
	stored, ok := l.GetMT292Export(first.Reference)
	if !ok || stored.Message != first.Message {
		t.Errorf("stored export after conflicts = %+v, want the first message", stored)
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt292_exported") != 1 {
		t.Errorf("mt292_exported count = %d, want 1 after conflicts", countOp(events, "mt292_exported"))
	}
}

func TestExportMT292RegisteredRunSeqAdvanceNewRef(t *testing.T) {
	al := mustAuditLog(t)
	l, sched := mt292ExportTestLedger(t, al)
	if _, err := l.PauseTransferSchedule(sched.ID); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	first, err := l.ExportMT292Registered(mt292ExportTestOptions(sched.ID), "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT292Registered: %v", err)
	}
	// The next run fires: the :20: derives from the run sequence,
	// so the new run exports under a new reference and never
	// collides with the old registration.
	l.schedules[sched.ID].RunSeq++
	second, err := l.ExportMT292Registered(mt292ExportTestOptions(sched.ID), "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT292Registered after RunSeq advance: %v", err)
	}
	if second.Reference == first.Reference {
		t.Errorf("reference = %q after RunSeq advance, want a new :20:", second.Reference)
	}
	if second.Duplicate {
		t.Error("duplicate = true on a new run's first export")
	}
	// Both registrations coexist: the old :20: still returns the
	// first rendering.
	stored, ok := l.GetMT292Export(first.Reference)
	if !ok || stored.Message != first.Message {
		t.Errorf("old export after advance = %+v, want the first message", stored)
	}
	stored2, ok := l.GetMT292Export(second.Reference)
	if !ok || stored2.Message != second.Message {
		t.Errorf("new export = %+v, want the second message", stored2)
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt292_exported") != 2 {
		t.Errorf("mt292_exported count = %d, want 2 (one per run)", countOp(events, "mt292_exported"))
	}
}

func TestExportMT292RegisteredUnknownSchedule(t *testing.T) {
	al := mustAuditLog(t)
	l, _ := mt292ExportTestLedger(t, al)
	if _, err := l.ExportMT292Registered(mt292ExportTestOptions("sched-nope"), "ledger-operator"); !errors.Is(err, ErrScheduleNotFound) {
		t.Errorf("err = %v, want ErrScheduleNotFound", err)
	}
	// A refused export records nothing: no registry entry, no
	// audit marker.
	if _, ok := l.GetMT292Export(mt292ExpectedRef("sched-nope", 0)); ok {
		t.Error("GetMT292Export = true for a refused export, want false")
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt292_exported") != 0 {
		t.Errorf("mt292_exported count = %d, want 0 for a refused export", countOp(events, "mt292_exported"))
	}
}

func TestExportMT292RegisteredStillActive(t *testing.T) {
	al := mustAuditLog(t)
	l, sched := mt292ExportTestLedger(t, al)
	// The schedule is still active with a live next run: the money
	// is still coming, so withdrawing the notice would mislead.
	if _, err := l.ExportMT292Registered(mt292ExportTestOptions(sched.ID), "ledger-operator"); !errors.Is(err, ErrMT292ScheduleStillActive) {
		t.Errorf("err = %v, want ErrMT292ScheduleStillActive", err)
	}
	// A refused export records nothing: no registry entry, no
	// audit marker — and the same :20: stays exportable once the
	// schedule is paused.
	if _, ok := l.GetMT292Export(mt292ExpectedRef(sched.ID, sched.RunSeq)); ok {
		t.Error("GetMT292Export = true for a refused export, want false")
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt292_exported") != 0 {
		t.Errorf("mt292_exported count = %d, want 0 for a refused export", countOp(events, "mt292_exported"))
	}
	if _, err := l.PauseTransferSchedule(sched.ID); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	if _, err := l.ExportMT292Registered(mt292ExportTestOptions(sched.ID), "ledger-operator"); err != nil {
		t.Errorf("ExportMT292Registered after pause: %v, want success", err)
	}
}

func TestExportMT292RegisteredAuditEvent(t *testing.T) {
	al := mustAuditLog(t)
	l, sched := mt292ExportTestLedger(t, al)
	if _, err := l.PauseTransferSchedule(sched.ID); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	before := l.Version()
	res, err := l.ExportMT292Registered(mt292ExportTestOptions(sched.ID), "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT292Registered: %v", err)
	}
	// Audit: exactly one export marker, with the exact details —
	// the :20: on the durable record, no accounts.
	events := readAuditOps(t, al)
	if countOp(events, "mt292_exported") != 1 {
		t.Fatalf("mt292_exported count = %d, want 1", countOp(events, "mt292_exported"))
	}
	ev := findOp(events, "mt292_exported")
	if ev == nil {
		t.Fatal("mt292_exported event missing")
	}
	if ev.Actor != "ledger-operator" {
		t.Errorf("actor = %q, want ledger-operator", ev.Actor)
	}
	if ev.TraceID != res.Reference {
		t.Errorf("trace = %q, want %q", ev.TraceID, res.Reference)
	}
	// Details round-trip through JSON, so run_seq reads back as
	// a float64 (the schedule_*_amend precedent).
	if ev.Details["msg_id"] != res.Reference || ev.Details["related_reference"] != sched.ID ||
		ev.Details["schedule_id"] != sched.ID || ev.Details["run_seq"] != float64(sched.RunSeq) ||
		ev.Details["sender_lt"] != "NWBKGB2LXXX" {
		t.Errorf("details = %v, want msg/related/schedule/run_seq/sender", ev.Details)
	}
	if len(ev.Accounts) != 0 {
		t.Errorf("accounts = %v, want none (a cancellation request touches no account)", ev.Accounts)
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

func TestGetMT292ExportUnknown(t *testing.T) {
	al := mustAuditLog(t)
	l, _ := mt292ExportTestLedger(t, al)
	if _, ok := l.GetMT292Export("NEVER-EXPORTED"); ok {
		t.Error("GetMT292Export = true for an unknown :20:, want false")
	}
}
