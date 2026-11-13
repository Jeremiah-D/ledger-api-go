package ledger

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
)

// camt057TestLedger builds a ledger with registered BICs on both legs
// (alice DEUTDEFF, bob NWBKGB2L) and one daily schedule: 10000
// cents USD, next run 2026-11-12 09:00 UTC, a 250-cent per-run fee
// on its own leg — the mt210TestLedger twin, so the MT210 and
// camt.057 notices cross-check on the same schedule.
func camt057TestLedger(t *testing.T) (*Ledger, TransferSchedule) {
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

// stripCAMT057CreDtTm removes the single wall-clock CreDtTm line so
// repeat exports compare byte-identical: the deterministic MsgId
// pins the document, CreDtTm honestly records when each document
// was created (the camt.060 precedent).
var camt057CreDtTmLine = regexp.MustCompile(`(?m)^\s*<CreDtTm>[^<]*</CreDtTm>\n`)

func stripCAMT057CreDtTm(doc string) string {
	return camt057CreDtTmLine.ReplaceAllString(doc, "")
}

func TestExportCAMT057ExactTags(t *testing.T) {
	l, sched := camt057TestLedger(t)
	doc, err := l.ExportCAMT057(CAMT057ExportOptions{ScheduleID: sched.ID, ServicingBIC: "CITIUS33"})
	if err != nil {
		t.Fatalf("ExportCAMT057: %v", err)
	}
	for _, want := range []string{
		// Registry-latest version: camt.057.001.07,
		// NotificationToReceiveV07 (see camt057.go).
		`xmlns="urn:iso:std:iso:20022:tech:xsd:camt.057.001.07"`,
		"<NtfctnToRcv>",
		// GrpHdr MsgId is deterministic: camt057-<schedule>-<runSeq>.
		"<MsgId>camt057-sched-1-0</MsgId>",
		"<CreDtTm>",
		// Ntfctn Id repeats the MsgId.
		"<Id>camt057-sched-1-0</Id>",
		// Acct is the To account's registered IBAN.
		"<IBAN>GB29NWBK60161331926819</IBAN>",
		// AcctSvcr is the servicing bank named by the caller.
		"<BICFI>CITIUS33</BICFI>",
		// TtlAmt is the principal: 10000 cents -> 100.00 USD.
		`<TtlAmt Ccy="USD">100.00</TtlAmt>`,
		// XpctdValDt is the next run date, ISO date.
		"<XpctdValDt>2026-11-12</XpctdValDt>",
		// From has a registered BIC, so the debtor renders as an
		// agent — never as a party name.
		"<BICFI>DEUTDEFF</BICFI>",
		// The single mandatory Itm carries the run transfer ID
		// and the same principal and value date.
		"<Id>sched-1/run/0</Id>",
		`<Amt Ccy="USD">100.00</Amt>`,
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("export missing %q, body:\n%s", want, doc)
		}
	}
	// No debtor party name alongside the agent BIC, no unused
	// V07 elements: no UETR, no MsgSndr, no remittance, no
	// supplementary data.
	for _, banned := range []string{"<Pty>", "<UETR>", "<MsgSndr>", "<Purp>", "<RmtInf>", "<SplmtryData>", "<RltdAcct>"} {
		if strings.Contains(doc, banned) {
			t.Errorf("export must not carry %s, body:\n%s", banned, doc)
		}
	}
}

func TestExportCAMT057FeeNotDeducted(t *testing.T) {
	l, sched := camt057TestLedger(t)
	doc, err := l.ExportCAMT057(CAMT057ExportOptions{ScheduleID: sched.ID, ServicingBIC: "CITIUS33"})
	if err != nil {
		t.Fatalf("ExportCAMT057: %v", err)
	}
	// The schedule carries a 250-cent fee; the notice still states
	// the full 10000-cent principal — the fee books separately.
	if !strings.Contains(doc, `<TtlAmt Ccy="USD">100.00</TtlAmt>`) {
		t.Errorf("export TtlAmt must equal the full principal, body:\n%s", doc)
	}
	if !strings.Contains(doc, `<Amt Ccy="USD">100.00</Amt>`) {
		t.Errorf("export Itm Amt must equal the full principal, body:\n%s", doc)
	}
	if strings.Contains(doc, "97.50") {
		t.Errorf("export must not deduct the fee from the amount, body:\n%s", doc)
	}
}

func TestExportCAMT057NoIBANFallsBackToAccountID(t *testing.T) {
	l := New()
	if err := l.SetAccountIBAN("alice", "DE89370400440532013000", "DEUTDEFF"); err != nil {
		t.Fatalf("SetAccountIBAN alice: %v", err)
	}
	// "BOBACCT1" has no IBAN registration: Acct falls back to the
	// account ID under Othr/Id.
	if _, _, err := l.CreateTransferSchedule(TransferSchedule{
		ID: "sched-1", From: "alice", To: "BOBACCT1",
		AmountCents: 10000, Currency: "USD", Interval: 24 * time.Hour,
		NextRunAt: time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("CreateTransferSchedule: %v", err)
	}
	doc, err := l.ExportCAMT057(CAMT057ExportOptions{ScheduleID: "sched-1", ServicingBIC: "CITIUS33"})
	if err != nil {
		t.Fatalf("ExportCAMT057: %v", err)
	}
	for _, want := range []string{
		"<Othr>",
		"<Id>BOBACCT1</Id>",
		"<BICFI>DEUTDEFF</BICFI>",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("export missing %q, body:\n%s", want, doc)
		}
	}
	if strings.Contains(doc, "<IBAN>") {
		t.Errorf("export must not carry an IBAN without a registration, body:\n%s", doc)
	}
}

func TestExportCAMT057NoBICFallsBackToPartyName(t *testing.T) {
	l := New()
	if err := l.SetAccountIBAN("bob", "GB29NWBK60161331926819", "NWBKGB2L"); err != nil {
		t.Fatalf("SetAccountIBAN bob: %v", err)
	}
	// "alice" has no registered BIC: the debtor renders as a party
	// name — and no agent BICFI for the debtor must appear. (The
	// servicing BICFI under AcctSvcr is still there.)
	if _, _, err := l.CreateTransferSchedule(TransferSchedule{
		ID: "sched-1", From: "alice", To: "bob",
		AmountCents: 10000, Currency: "USD", Interval: 24 * time.Hour,
		NextRunAt: time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("CreateTransferSchedule: %v", err)
	}
	doc, err := l.ExportCAMT057(CAMT057ExportOptions{ScheduleID: "sched-1", ServicingBIC: "CITIUS33"})
	if err != nil {
		t.Fatalf("ExportCAMT057: %v", err)
	}
	if !strings.Contains(doc, "<Dbtr>") || !strings.Contains(doc, "<Nm>ALICE</Nm>") {
		t.Errorf("export missing Dbtr party name ALICE, body:\n%s", doc)
	}
	if strings.Contains(doc, "<Dbtr>\n        <Agt>") {
		t.Errorf("export must not carry a debtor agent without a From BIC, body:\n%s", doc)
	}
}

func TestExportCAMT057NotActiveStatuses(t *testing.T) {
	// Paused, cancelled, and completed schedules each report
	// ErrCAMT057ScheduleNotActive: none of them has a next run to
	// notify.
	t.Run("paused", func(t *testing.T) {
		l, sched := camt057TestLedger(t)
		if _, err := l.PauseTransferSchedule(sched.ID); err != nil {
			t.Fatalf("PauseTransferSchedule: %v", err)
		}
		_, err := l.ExportCAMT057(CAMT057ExportOptions{ScheduleID: sched.ID, ServicingBIC: "CITIUS33"})
		if !errors.Is(err, ErrCAMT057ScheduleNotActive) {
			t.Errorf("err = %v, want ErrCAMT057ScheduleNotActive", err)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		l, sched := camt057TestLedger(t)
		if _, err := l.CancelTransferSchedule(sched.ID); err != nil {
			t.Fatalf("CancelTransferSchedule: %v", err)
		}
		_, err := l.ExportCAMT057(CAMT057ExportOptions{ScheduleID: sched.ID, ServicingBIC: "CITIUS33"})
		if !errors.Is(err, ErrCAMT057ScheduleNotActive) {
			t.Errorf("err = %v, want ErrCAMT057ScheduleNotActive", err)
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
		_, err := l.ExportCAMT057(CAMT057ExportOptions{ScheduleID: "sched-1", ServicingBIC: "CITIUS33"})
		if !errors.Is(err, ErrCAMT057ScheduleNotActive) {
			t.Errorf("err = %v, want ErrCAMT057ScheduleNotActive", err)
		}
	})
}

func TestExportCAMT057EndsAtExhausted(t *testing.T) {
	l, sched := camt057TestLedger(t)
	// White-box the exhausted state: the schedule is still
	// active, but its NextRunAt has moved past EndsAt, so no run
	// is left to notify. (The public API cannot reach this state:
	// AmendNextRunAt and AmendEndsAt both guard the boundary, and
	// SweepDue completes the schedule instead — the export's guard
	// is defensive, and the test pins it directly.)
	nextRun := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	l.schedules[sched.ID].EndsAt = nextRun.Add(2 * time.Hour)
	l.schedules[sched.ID].NextRunAt = nextRun.Add(3 * time.Hour)
	_, err := l.ExportCAMT057(CAMT057ExportOptions{ScheduleID: sched.ID, ServicingBIC: "CITIUS33"})
	if !errors.Is(err, ErrCAMT057ScheduleNotActive) {
		t.Errorf("err = %v, want ErrCAMT057ScheduleNotActive", err)
	}
}

func TestExportCAMT057FXSchedule(t *testing.T) {
	l := New()
	if err := l.SetAccountIBAN("alice", "DE89370400440532013000", "DEUTDEFF"); err != nil {
		t.Fatalf("SetAccountIBAN alice: %v", err)
	}
	if err := l.SetAccountIBAN("bob", "GB29NWBK60161331926819", "NWBKGB2L"); err != nil {
		t.Fatalf("SetAccountIBAN bob: %v", err)
	}
	// An FX schedule's receivable amount is only known when the
	// rate is looked up at fire time: it cannot be notified.
	if _, _, err := l.CreateTransferSchedule(TransferSchedule{
		ID: "sched-fx", From: "alice", To: "bob",
		AmountCents: 10000, Currency: "USD", ToCurrency: "EUR",
		FXAccount: "desk-fx", Interval: 24 * time.Hour,
		NextRunAt: time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("CreateTransferSchedule: %v", err)
	}
	_, err := l.ExportCAMT057(CAMT057ExportOptions{ScheduleID: "sched-fx", ServicingBIC: "CITIUS33"})
	if !errors.Is(err, ErrInvalidCAMT057Export) {
		t.Errorf("err = %v, want ErrInvalidCAMT057Export", err)
	}
}

func TestExportCAMT057UnknownSchedule(t *testing.T) {
	l, _ := camt057TestLedger(t)
	_, err := l.ExportCAMT057(CAMT057ExportOptions{ScheduleID: "sched-nope", ServicingBIC: "CITIUS33"})
	if !errors.Is(err, ErrScheduleNotFound) {
		t.Errorf("err = %v, want ErrScheduleNotFound", err)
	}
}

func TestExportCAMT057BadServicingBIC(t *testing.T) {
	l, sched := camt057TestLedger(t)
	for _, bad := range []string{"", "CITI", "CITIUS33XXX1", "citi us33"} {
		_, err := l.ExportCAMT057(CAMT057ExportOptions{ScheduleID: sched.ID, ServicingBIC: bad})
		if !errors.Is(err, ErrInvalidCAMT057Export) {
			t.Errorf("servicing BIC %q: err = %v, want ErrInvalidCAMT057Export", bad, err)
		}
	}
}

func TestExportCAMT057RepeatExport(t *testing.T) {
	l, sched := camt057TestLedger(t)
	first, err := l.ExportCAMT057(CAMT057ExportOptions{ScheduleID: sched.ID, ServicingBIC: "CITIUS33"})
	if err != nil {
		t.Fatalf("ExportCAMT057: %v", err)
	}
	second, err := l.ExportCAMT057(CAMT057ExportOptions{ScheduleID: sched.ID, ServicingBIC: "CITIUS33"})
	if err != nil {
		t.Fatalf("ExportCAMT057: %v", err)
	}
	// Byte-identical modulo the honest wall-clock CreDtTm line; the
	// deterministic MsgId pins the rest of the document.
	if stripCAMT057CreDtTm(first) != stripCAMT057CreDtTm(second) {
		t.Errorf("repeat export differs (modulo CreDtTm):\n%s\nvs\n%s", first, second)
	}
}

func TestExportCAMT057CrossCheckMT210(t *testing.T) {
	// The MX twin agrees with the FIN original on the same
	// schedule, same run: :20:/:25:/:30:/:32B: against
	// Ntfctn Id / Acct / XpctdValDt / TtlAmt.
	l, sched := camt057TestLedger(t)
	fin, err := l.ExportMT210(MT210ExportOptions{ScheduleID: sched.ID, ServicingBIC: "CITIUS33"})
	if err != nil {
		t.Fatalf("ExportMT210: %v", err)
	}
	mx, err := l.ExportCAMT057(CAMT057ExportOptions{ScheduleID: sched.ID, ServicingBIC: "CITIUS33"})
	if err != nil {
		t.Fatalf("ExportCAMT057: %v", err)
	}
	for _, want := range []string{
		":20:sched-1",
		":25:GB29NWBK60161331926819",
		":30:261112",
		":21:sched-1/run/0",
		":32B:USD100,00",
		":52A:DEUTDEFF",
	} {
		if !strings.Contains(fin, want) {
			t.Errorf("MT210 missing %q, body:\n%s", want, fin)
		}
	}
	for _, want := range []string{
		"<Id>camt057-sched-1-0</Id>",
		"<IBAN>GB29NWBK60161331926819</IBAN>",
		"<XpctdValDt>2026-11-12</XpctdValDt>",
		"<Id>sched-1/run/0</Id>",
		`<TtlAmt Ccy="USD">100.00</TtlAmt>`,
		"<BICFI>DEUTDEFF</BICFI>",
	} {
		if !strings.Contains(mx, want) {
			t.Errorf("camt.057 missing %q, body:\n%s", want, mx)
		}
	}
}
