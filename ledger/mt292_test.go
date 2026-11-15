package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// mt292TestLedger reuses the MT210 fixture: a ledger with
// registered BICs on both legs (alice DEUTDEFF, bob NWBKGB2L) and
// one daily schedule, 10000 cents USD, next run 2026-11-12 09:00
// UTC — the same schedule the MT210 notice is built on, so the
// cancellation request cross-checks against it. Callers pause,
// cancel, or complete the schedule before exporting: MT292 is the
// withdrawal of that notice, and only a schedule whose
// notification premise is dead exports.
func mt292TestLedger(t *testing.T) (*Ledger, TransferSchedule) {
	t.Helper()
	return mt210TestLedger(t)
}

// mt292OriginalDate is the original MT210's issue date for the
// fixture schedule: 2026-11-10, so :11S: renders "210261110".
func mt292OriginalDate() time.Time {
	return time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
}

// mt292ExpectedRef is the deterministic :20: for a schedule and
// run: "MT292-" plus the first 10 lowercase hex characters of the
// SHA-256 of the run transfer ID ("<ID>/run/<seq>", the MT210's
// :21:).
func mt292ExpectedRef(scheduleID string, runSeq uint64) string {
	runID := fmt.Sprintf("%s/run/%d", scheduleID, runSeq)
	sum := sha256.Sum256([]byte(runID))
	return "MT292-" + hex.EncodeToString(sum[:])[:10]
}

func TestExportMT292ExactTags(t *testing.T) {
	l, sched := mt292TestLedger(t)
	if _, err := l.PauseTransferSchedule(sched.ID); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	doc, err := l.ExportMT292(MT292ExportOptions{
		ScheduleID: sched.ID, ServicingBIC: "CITIUS33",
		OriginalDate: mt292OriginalDate(),
		Reason:       "Schedule paused by operator",
	})
	if err != nil {
		t.Fatalf("ExportMT292: %v", err)
	}
	ref := mt292ExpectedRef(sched.ID, sched.RunSeq)
	if len(ref) != 16 {
		t.Fatalf(":20: = %q, want exactly 16 characters", ref)
	}
	if hexPart := ref[len("MT292-"):]; hexPart != strings.ToLower(hexPart) {
		t.Fatalf(":20: hex part = %q, want lowercase hex", hexPart)
	}
	for _, want := range []string{
		// Block framing: sender LT (To BIC, normalized), receiver
		// LT (servicing BIC), MUR = :20:.
		"{1:F01NWBKGB2LXXX0000000000}",
		"{2:I292CITIUS33XXXU3003}",
		"{3:{108:" + ref + "}}",
		"{4:",
		// :20: is the deterministic derivation, exactly 16 chars.
		":20:" + ref,
		// :21: is the schedule ID — the original MT210's :20:.
		":21:sched-1",
		// :11S: is "210" plus the original MT210's issue date.
		":11S:210261110",
		// :79: carries the reason verbatim.
		":79:Schedule paused by operator",
		"-}",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("export missing %q, body:\n%s", want, doc)
		}
	}
	// No trailing newline (the nacha.go convention), no block 5
	// (CHK is network-assigned), and no amount, value date, or
	// account field: the cancellation carries no money.
	if strings.HasSuffix(doc, "\n") {
		t.Error("message must not end with a newline")
	}
	for _, banned := range []string{"{5:", ":32B:", ":30:", ":25:", ":50D:", ":52A:", ":56A:", ":72:"} {
		if strings.Contains(doc, banned) {
			t.Errorf("export must not carry %s, body:\n%s", banned, doc)
		}
	}
	// Parse back: each field occurs exactly once with the exact value.
	f := parseMT103Back(t, doc)
	if got := f[":20:"]; len(got) != 1 || got[0] != ref {
		t.Errorf(":20: = %v, want [%s]", got, ref)
	}
	if got := f[":21:"]; len(got) != 1 || got[0] != sched.ID {
		t.Errorf(":21: = %v, want [%s]", got, sched.ID)
	}
	if got := f[":11S:"]; len(got) != 1 || got[0] != "210261110" {
		t.Errorf(":11S: = %v, want [210261110]", got)
	}
	if got := f[":79:"]; len(got) != 1 || got[0] != "Schedule paused by operator" {
		t.Errorf(":79: = %v, want [Schedule paused by operator]", got)
	}
}

func TestExportMT292NoReasonOmits79(t *testing.T) {
	l, sched := mt292TestLedger(t)
	if _, err := l.PauseTransferSchedule(sched.ID); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	doc, err := l.ExportMT292(MT292ExportOptions{
		ScheduleID: sched.ID, ServicingBIC: "CITIUS33",
		OriginalDate: mt292OriginalDate(),
	})
	if err != nil {
		t.Fatalf("ExportMT292: %v", err)
	}
	f := parseMT103Back(t, doc)
	if _, ok := f[":79:"]; ok {
		t.Errorf(":79: must be omitted without a reason, body:\n%s", doc)
	}
	if got := f[":21:"]; len(got) != 1 || got[0] != sched.ID {
		t.Errorf(":21: = %v, want [%s]", got, sched.ID)
	}
}

func TestExportMT292RevocableStatuses(t *testing.T) {
	// Paused, cancelled, and completed schedules each export:
	// the deliberate opposite of MT210's active-only gate — their
	// notification premise is dead.
	opts := func(id string) MT292ExportOptions {
		return MT292ExportOptions{ScheduleID: id, ServicingBIC: "CITIUS33", OriginalDate: mt292OriginalDate()}
	}
	t.Run("paused", func(t *testing.T) {
		l, sched := mt292TestLedger(t)
		if _, err := l.PauseTransferSchedule(sched.ID); err != nil {
			t.Fatalf("PauseTransferSchedule: %v", err)
		}
		if _, err := l.ExportMT292(opts(sched.ID)); err != nil {
			t.Errorf("ExportMT292: %v, want no error", err)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		l, sched := mt292TestLedger(t)
		if _, err := l.CancelTransferSchedule(sched.ID); err != nil {
			t.Fatalf("CancelTransferSchedule: %v", err)
		}
		if _, err := l.ExportMT292(opts(sched.ID)); err != nil {
			t.Errorf("ExportMT292: %v, want no error", err)
		}
	})
	t.Run("completed", func(t *testing.T) {
		l := New()
		if err := l.SetAccountIBAN("alice", "DE89370400440532013000", "DEUTDEFF"); err != nil {
			t.Fatalf("SetAccountIBAN alice: %v", err)
		}
		if err := l.SetAccountIBAN("bob", "GB29NWBK60161331926819", "NWBKGB2L"); err != nil {
			t.Fatalf("SetAccountIBAN bob: %v", err)
		}
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
		if _, err := l.ExportMT292(opts("sched-1")); err != nil {
			t.Errorf("ExportMT292: %v, want no error", err)
		}
	})
}

func TestExportMT292StillActive(t *testing.T) {
	// An active schedule with a live next run (no EndsAt) refuses:
	// the money is still coming, so withdrawing the notice would
	// mislead liquidity planning.
	l, sched := mt292TestLedger(t)
	_, err := l.ExportMT292(MT292ExportOptions{
		ScheduleID: sched.ID, ServicingBIC: "CITIUS33",
		OriginalDate: mt292OriginalDate(),
	})
	if !errors.Is(err, ErrMT292ScheduleStillActive) {
		t.Errorf("err = %v, want ErrMT292ScheduleStillActive", err)
	}
}

func TestExportMT292EndsAtExhausted(t *testing.T) {
	l, sched := mt292TestLedger(t)
	// White-box the exhausted state: the schedule is still
	// active, but its NextRunAt has moved past EndsAt, so the
	// notification premise is dead. (The public API cannot reach
	// this state: AmendNextRunAt and AmendEndsAt both guard the
	// boundary, and SweepDue completes the schedule instead —
	// the export's guard is defensive, and the test pins it
	// directly — the camt.058 precedent, LG-404.)
	nextRun := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	l.schedules[sched.ID].EndsAt = nextRun.Add(2 * time.Hour)
	l.schedules[sched.ID].NextRunAt = nextRun.Add(3 * time.Hour)
	if _, err := l.ExportMT292(MT292ExportOptions{
		ScheduleID: sched.ID, ServicingBIC: "CITIUS33",
		OriginalDate: mt292OriginalDate(),
	}); err != nil {
		t.Errorf("ExportMT292: %v, want no error", err)
	}
}

func TestExportMT292FXSchedule(t *testing.T) {
	l := New()
	if err := l.SetAccountIBAN("alice", "DE89370400440532013000", "DEUTDEFF"); err != nil {
		t.Fatalf("SetAccountIBAN alice: %v", err)
	}
	if err := l.SetAccountIBAN("bob", "GB29NWBK60161331926819", "NWBKGB2L"); err != nil {
		t.Fatalf("SetAccountIBAN bob: %v", err)
	}
	if _, _, err := l.CreateTransferSchedule(TransferSchedule{
		ID: "sched-fx", From: "alice", To: "bob",
		AmountCents: 10000, Currency: "USD", ToCurrency: "EUR",
		FXAccount: "desk-fx", Interval: 24 * time.Hour,
		NextRunAt: time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("CreateTransferSchedule: %v", err)
	}
	// While active, the FX schedule refuses like any other live
	// schedule — the status gate runs before any FX consideration.
	_, err := l.ExportMT292(MT292ExportOptions{ScheduleID: "sched-fx", ServicingBIC: "CITIUS33", OriginalDate: mt292OriginalDate()})
	if !errors.Is(err, ErrMT292ScheduleStillActive) {
		t.Errorf("active FX: err = %v, want ErrMT292ScheduleStillActive", err)
	}
	// Once paused, the FX schedule is revocable: the MT292 carries
	// no amount, so the unknowable-until-fire-time FX amount is
	// never written down.
	if _, err := l.PauseTransferSchedule("sched-fx"); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	doc, err := l.ExportMT292(MT292ExportOptions{ScheduleID: "sched-fx", ServicingBIC: "CITIUS33", OriginalDate: mt292OriginalDate()})
	if err != nil {
		t.Fatalf("ExportMT292: %v", err)
	}
	for _, banned := range []string{":32B:", "EUR"} {
		if strings.Contains(doc, banned) {
			t.Errorf("FX export must not carry %s, body:\n%s", banned, doc)
		}
	}
	f := parseMT103Back(t, doc)
	if got := f[":21:"]; len(got) != 1 || got[0] != "sched-fx" {
		t.Errorf(":21: = %v, want [sched-fx]", got)
	}
}

func TestExportMT292UnknownSchedule(t *testing.T) {
	l, _ := mt292TestLedger(t)
	_, err := l.ExportMT292(MT292ExportOptions{ScheduleID: "sched-nope", ServicingBIC: "CITIUS33", OriginalDate: mt292OriginalDate()})
	if !errors.Is(err, ErrScheduleNotFound) {
		t.Errorf("err = %v, want ErrScheduleNotFound", err)
	}
}

func TestExportMT292MissingOptions(t *testing.T) {
	l, sched := mt292TestLedger(t)
	if _, err := l.PauseTransferSchedule(sched.ID); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	// Missing servicing BIC: the ledger keeps no routing table.
	_, err := l.ExportMT292(MT292ExportOptions{ScheduleID: sched.ID, OriginalDate: mt292OriginalDate()})
	if !errors.Is(err, ErrInvalidMT292Export) {
		t.Errorf("missing servicing BIC: err = %v, want ErrInvalidMT292Export", err)
	}
	// Missing original date: the ledger records no MT210 issue
	// date, so there is nothing to fall back to — never
	// NextRunAt, never a derived date.
	_, err = l.ExportMT292(MT292ExportOptions{ScheduleID: sched.ID, ServicingBIC: "CITIUS33"})
	if !errors.Is(err, ErrInvalidMT292Export) {
		t.Errorf("missing original date: err = %v, want ErrInvalidMT292Export", err)
	}
}

func TestExportMT292BadServicingBIC(t *testing.T) {
	l, sched := mt292TestLedger(t)
	for _, bad := range []string{"", "CITI", "CITIUS33XXX1", "citi us33"} {
		_, err := l.ExportMT292(MT292ExportOptions{ScheduleID: sched.ID, ServicingBIC: bad, OriginalDate: mt292OriginalDate()})
		if !errors.Is(err, ErrInvalidMT292Export) {
			t.Errorf("servicing BIC %q: err = %v, want ErrInvalidMT292Export", bad, err)
		}
	}
}

func TestExportMT292BadReason(t *testing.T) {
	l, sched := mt292TestLedger(t)
	if _, err := l.PauseTransferSchedule(sched.ID); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	// A reason outside the SWIFT `x` set is refused, never truncated.
	_, err := l.ExportMT292(MT292ExportOptions{ScheduleID: sched.ID, ServicingBIC: "CITIUS33", OriginalDate: mt292OriginalDate(), Reason: "café"})
	if !errors.Is(err, ErrInvalidMT292Export) {
		t.Errorf("non-x reason: err = %v, want ErrInvalidMT292Export", err)
	}
	// A single :79: line over 35 characters is refused.
	_, err = l.ExportMT292(MT292ExportOptions{ScheduleID: sched.ID, ServicingBIC: "CITIUS33", OriginalDate: mt292OriginalDate(), Reason: strings.Repeat("x", 36)})
	if !errors.Is(err, ErrInvalidMT292Export) {
		t.Errorf("over-long reason: err = %v, want ErrInvalidMT292Export", err)
	}
	// A multi-line reason renders as continuation lines.
	doc, err := l.ExportMT292(MT292ExportOptions{ScheduleID: sched.ID, ServicingBIC: "CITIUS33", OriginalDate: mt292OriginalDate(), Reason: "Schedule paused\nPlease cancel"})
	if err != nil {
		t.Fatalf("ExportMT292: %v", err)
	}
	f := parseMT103Back(t, doc)
	if got := f[":79:"]; len(got) != 2 || got[0] != "Schedule paused" || got[1] != "Please cancel" {
		t.Errorf(":79: = %v, want [Schedule paused Please cancel]", got)
	}
}

func TestExportMT292RepeatExport(t *testing.T) {
	l, sched := mt292TestLedger(t)
	if _, err := l.PauseTransferSchedule(sched.ID); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	opts := MT292ExportOptions{
		ScheduleID: sched.ID, ServicingBIC: "CITIUS33",
		OriginalDate: mt292OriginalDate(),
		Reason:       "Schedule paused by operator",
	}
	first, err := l.ExportMT292(opts)
	if err != nil {
		t.Fatalf("ExportMT292: %v", err)
	}
	second, err := l.ExportMT292(opts)
	if err != nil {
		t.Fatalf("ExportMT292 repeat: %v", err)
	}
	// Byte-identical: :20: is pinned to schedule+RunSeq, :11S: to
	// the caller-supplied OriginalDate, no wall clock anywhere.
	if second != first {
		t.Errorf("repeat export differs:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

func TestExportMT292CrossCheckMT210(t *testing.T) {
	// The cancellation references exactly the notification MT210
	// rendered for the same schedule and run: the MT210's :20:
	// (the schedule ID) is the MT292's :21:, and the MT292's
	// :20: derives from the MT210's :21: (the run transfer ID).
	l, sched := mt292TestLedger(t)
	notice, err := l.ExportMT210(MT210ExportOptions{ScheduleID: sched.ID, ServicingBIC: "CITIUS33"})
	if err != nil {
		t.Fatalf("ExportMT210: %v", err)
	}
	nf := parseMT103Back(t, notice)
	if got := nf[":20:"]; len(got) != 1 || got[0] != sched.ID {
		t.Fatalf("MT210 :20: = %v, want [%s]", got, sched.ID)
	}
	if got := nf[":21:"]; len(got) != 1 || got[0] != "sched-1/run/0" {
		t.Fatalf("MT210 :21: = %v, want [sched-1/run/0]", got)
	}
	if _, err := l.PauseTransferSchedule(sched.ID); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	cancel, err := l.ExportMT292(MT292ExportOptions{
		ScheduleID: sched.ID, ServicingBIC: "CITIUS33",
		OriginalDate: mt292OriginalDate(),
	})
	if err != nil {
		t.Fatalf("ExportMT292: %v", err)
	}
	cf := parseMT103Back(t, cancel)
	if got := cf[":21:"]; len(got) != 1 || got[0] != nf[":20:"][0] {
		t.Errorf("MT292 :21: = %v, want MT210 :20: [%s]", got, nf[":20:"][0])
	}
	if got := cf[":20:"]; len(got) != 1 || got[0] != mt292ExpectedRef(sched.ID, sched.RunSeq) {
		t.Errorf("MT292 :20: = %v, want [%s]", got, mt292ExpectedRef(sched.ID, sched.RunSeq))
	}
}
