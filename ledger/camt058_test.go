package ledger

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
)

// camt058TestLedger reuses the camt.057 fixture: a ledger with
// registered BICs on both legs (alice DEUTDEFF, bob NWBKGB2L) and
// one daily schedule, 10000 cents USD, next run 2026-11-12 09:00
// UTC — the same schedule the camt.057 notice and the MT210
// pre-advice are built on, so the cancellation advice cross-checks
// against both. Callers pause, cancel, or complete the schedule
// before exporting: camt.058 is the withdrawal of that notice,
// and only a schedule whose notification premise is dead exports.
func camt058TestLedger(t *testing.T) (*Ledger, TransferSchedule) {
	t.Helper()
	return camt057TestLedger(t)
}

// stripCAMT058CreDtTm removes the single wall-clock CreDtTm line
// so repeat exports compare byte-identical: the deterministic
// MsgId pins the document, CreDtTm honestly records when each
// document was created (the camt.060 precedent).
var camt058CreDtTmLine = regexp.MustCompile(`(?m)^\s*<CreDtTm>[^<]*</CreDtTm>\n`)

func stripCAMT058CreDtTm(doc string) string {
	return camt058CreDtTmLine.ReplaceAllString(doc, "")
}

func TestExportCAMT058ExactTags(t *testing.T) {
	l, sched := camt058TestLedger(t)
	if _, err := l.PauseTransferSchedule(sched.ID); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	doc, err := l.ExportCAMT058(CAMT058ExportOptions{
		ScheduleID: sched.ID, ServicingBIC: "CITIUS33",
		Reason: "schedule paused by operator",
	})
	if err != nil {
		t.Fatalf("ExportCAMT058: %v", err)
	}
	for _, want := range []string{
		// Registry-latest version: camt.058.001.08,
		// NotificationToReceiveCancellationAdviceV08 (see
		// camt058.go).
		`xmlns="urn:iso:std:iso:20022:tech:xsd:camt.058.001.08"`,
		"<NtfctnToRcvCxlAdvc>",
		// GrpHdr MsgId is deterministic: camt058-<schedule>-<runSeq>.
		"<MsgId>camt058-sched-1-0</MsgId>",
		"<CreDtTm>",
		// OrgnlNtfctn carries only the original notification's
		// identifiers: both repeat the camt.057 deterministic
		// MsgId for the same schedule and run.
		"<OrgnlMsgId>camt057-sched-1-0</OrgnlMsgId>",
		"<OrgnlNtfctnId>camt057-sched-1-0</OrgnlNtfctnId>",
		// The reason rides Rsn/Prtry (the pain.011 precedent):
		// free-form, no invented code table. (MarshalIndent
		// pretty-prints the block across lines, so the tags are
		// asserted separately, the camt.057 convention.)
		"<CxlRsn>",
		"<Rsn>",
		"<Prtry>schedule paused by operator</Prtry>",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("export missing %q, body:\n%s", want, doc)
		}
	}
	// No amount is restated anywhere — that is what makes an FX
	// schedule revocable — and no unused V08 elements: no original
	// creation time (the ledger never recorded when camt.057 was
	// sent), no NtfctnCxl indicator, no MsgSndr, no servicing
	// BIC inside the body (the BIC names the recipient at the
	// transport/header level), no supplementary data.
	for _, banned := range []string{"<TtlAmt>", "<Amt>", "<Acct>", "<Dbtr>", "<BICFI>", "<SplmtryData>", "<OrgnlNtfctnRef>", "<OrgnlCreDtTm>", "<NtfctnCxl>", "<MsgSndr>"} {
		if strings.Contains(doc, banned) {
			t.Errorf("export must not carry %s, body:\n%s", banned, doc)
		}
	}
}

func TestExportCAMT058NoReasonOmitsCxlRsn(t *testing.T) {
	l, sched := camt058TestLedger(t)
	if _, err := l.PauseTransferSchedule(sched.ID); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	for _, reason := range []string{"", "   "} {
		doc, err := l.ExportCAMT058(CAMT058ExportOptions{
			ScheduleID: sched.ID, ServicingBIC: "CITIUS33", Reason: reason,
		})
		if err != nil {
			t.Fatalf("ExportCAMT058 reason %q: %v", reason, err)
		}
		if strings.Contains(doc, "<CxlRsn>") {
			t.Errorf("reason %q: export must omit CxlRsn, body:\n%s", reason, doc)
		}
		if !strings.Contains(doc, "<OrgnlMsgId>camt057-sched-1-0</OrgnlMsgId>") {
			t.Errorf("reason %q: export missing OrgnlMsgId, body:\n%s", reason, doc)
		}
	}
}

func TestExportCAMT058RevocableStatuses(t *testing.T) {
	// Paused, cancelled, and completed schedules each export
	// with 200: the deliberate opposite of camt.057's
	// active-only gate — their notification premise is dead.
	t.Run("paused", func(t *testing.T) {
		l, sched := camt058TestLedger(t)
		if _, err := l.PauseTransferSchedule(sched.ID); err != nil {
			t.Fatalf("PauseTransferSchedule: %v", err)
		}
		if _, err := l.ExportCAMT058(CAMT058ExportOptions{ScheduleID: sched.ID, ServicingBIC: "CITIUS33"}); err != nil {
			t.Errorf("ExportCAMT058: %v, want no error", err)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		l, sched := camt058TestLedger(t)
		if _, err := l.CancelTransferSchedule(sched.ID); err != nil {
			t.Fatalf("CancelTransferSchedule: %v", err)
		}
		if _, err := l.ExportCAMT058(CAMT058ExportOptions{ScheduleID: sched.ID, ServicingBIC: "CITIUS33"}); err != nil {
			t.Errorf("ExportCAMT058: %v, want no error", err)
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
		if _, err := l.ExportCAMT058(CAMT058ExportOptions{ScheduleID: "sched-1", ServicingBIC: "CITIUS33"}); err != nil {
			t.Errorf("ExportCAMT058: %v, want no error", err)
		}
	})
}

func TestExportCAMT058StillActive(t *testing.T) {
	// An active schedule with a live next run (no EndsAt) refuses:
	// the money is still coming, so withdrawing the notice would
	// mislead liquidity planning.
	l, sched := camt058TestLedger(t)
	_, err := l.ExportCAMT058(CAMT058ExportOptions{ScheduleID: sched.ID, ServicingBIC: "CITIUS33"})
	if !errors.Is(err, ErrCAMT058ScheduleStillActive) {
		t.Errorf("err = %v, want ErrCAMT058ScheduleStillActive", err)
	}
}

func TestExportCAMT058EndsAtExhausted(t *testing.T) {
	l, sched := camt058TestLedger(t)
	// White-box the exhausted state: the schedule is still
	// active, but its NextRunAt has moved past EndsAt, so the
	// notification premise is dead. (The public API cannot reach
	// this state: AmendNextRunAt and AmendEndsAt both guard the
	// boundary, and SweepDue completes the schedule instead —
	// the export's guard is defensive, and the test pins it
	// directly.)
	nextRun := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	l.schedules[sched.ID].EndsAt = nextRun.Add(2 * time.Hour)
	l.schedules[sched.ID].NextRunAt = nextRun.Add(3 * time.Hour)
	if _, err := l.ExportCAMT058(CAMT058ExportOptions{ScheduleID: sched.ID, ServicingBIC: "CITIUS33"}); err != nil {
		t.Errorf("ExportCAMT058: %v, want no error", err)
	}
}

func TestExportCAMT058FXSchedule(t *testing.T) {
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
	_, err := l.ExportCAMT058(CAMT058ExportOptions{ScheduleID: "sched-fx", ServicingBIC: "CITIUS33"})
	if !errors.Is(err, ErrCAMT058ScheduleStillActive) {
		t.Errorf("active FX: err = %v, want ErrCAMT058ScheduleStillActive", err)
	}
	// Once paused, the FX schedule is revocable: OrgnlNtfctn
	// carries only the original notification's identifiers, never
	// an amount, so the unknowable-until-fire-time FX amount is
	// never written down.
	if _, err := l.PauseTransferSchedule("sched-fx"); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	doc, err := l.ExportCAMT058(CAMT058ExportOptions{ScheduleID: "sched-fx", ServicingBIC: "CITIUS33"})
	if err != nil {
		t.Fatalf("ExportCAMT058: %v", err)
	}
	for _, banned := range []string{"<TtlAmt>", "<Amt>", "EUR"} {
		if strings.Contains(doc, banned) {
			t.Errorf("FX export must not carry %s, body:\n%s", banned, doc)
		}
	}
	if !strings.Contains(doc, "<OrgnlMsgId>camt057-sched-fx-0</OrgnlMsgId>") {
		t.Errorf("FX export missing OrgnlMsgId, body:\n%s", doc)
	}
}

func TestExportCAMT058UnknownSchedule(t *testing.T) {
	l, _ := camt058TestLedger(t)
	_, err := l.ExportCAMT058(CAMT058ExportOptions{ScheduleID: "sched-nope", ServicingBIC: "CITIUS33"})
	if !errors.Is(err, ErrScheduleNotFound) {
		t.Errorf("err = %v, want ErrScheduleNotFound", err)
	}
}

func TestExportCAMT058BadServicingBIC(t *testing.T) {
	l, sched := camt058TestLedger(t)
	for _, bad := range []string{"", "CITI", "CITIUS33XXX1", "citi us33"} {
		_, err := l.ExportCAMT058(CAMT058ExportOptions{ScheduleID: sched.ID, ServicingBIC: bad})
		if !errors.Is(err, ErrInvalidCAMT058Export) {
			t.Errorf("servicing BIC %q: err = %v, want ErrInvalidCAMT058Export", bad, err)
		}
	}
}

func TestExportCAMT058RepeatExport(t *testing.T) {
	l, sched := camt058TestLedger(t)
	if _, err := l.PauseTransferSchedule(sched.ID); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	first, err := l.ExportCAMT058(CAMT058ExportOptions{ScheduleID: sched.ID, ServicingBIC: "CITIUS33", Reason: "schedule paused by operator"})
	if err != nil {
		t.Fatalf("ExportCAMT058: %v", err)
	}
	second, err := l.ExportCAMT058(CAMT058ExportOptions{ScheduleID: sched.ID, ServicingBIC: "CITIUS33", Reason: "schedule paused by operator"})
	if err != nil {
		t.Fatalf("ExportCAMT058: %v", err)
	}
	// Byte-identical modulo the honest wall-clock CreDtTm line; the
	// deterministic MsgId pins the rest of the document.
	if stripCAMT058CreDtTm(first) != stripCAMT058CreDtTm(second) {
		t.Errorf("repeat export differs (modulo CreDtTm):\n%s\nvs\n%s", first, second)
	}
}

func TestExportCAMT058CrossCheckCAMT057(t *testing.T) {
	// The advice references exactly the notification camt.057
	// rendered for the same schedule and run: the camt.057 MsgId
	// is the camt.058 OrgnlMsgId.
	l, sched := camt058TestLedger(t)
	notice, err := l.ExportCAMT057(CAMT057ExportOptions{ScheduleID: sched.ID, ServicingBIC: "CITIUS33"})
	if err != nil {
		t.Fatalf("ExportCAMT057: %v", err)
	}
	if !strings.Contains(notice, "<MsgId>camt057-sched-1-0</MsgId>") {
		t.Fatalf("camt.057 missing MsgId camt057-sched-1-0, body:\n%s", notice)
	}
	if _, err := l.PauseTransferSchedule(sched.ID); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	advice, err := l.ExportCAMT058(CAMT058ExportOptions{ScheduleID: sched.ID, ServicingBIC: "CITIUS33"})
	if err != nil {
		t.Fatalf("ExportCAMT058: %v", err)
	}
	for _, want := range []string{
		"<OrgnlMsgId>camt057-sched-1-0</OrgnlMsgId>",
		"<OrgnlNtfctnId>camt057-sched-1-0</OrgnlNtfctnId>",
		"<MsgId>camt058-sched-1-0</MsgId>",
	} {
		if !strings.Contains(advice, want) {
			t.Errorf("advice missing %q, body:\n%s", want, advice)
		}
	}
}
