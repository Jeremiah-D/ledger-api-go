package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// The camt.057 tests reuse mt210TestLedger (ledger/mt210_test.go):
// BICs registered on both legs (alice DEUTDEFF, bob NWBKGB2L),
// one daily schedule, 10000 cents USD, next run 2026-11-12 09:00
// UTC, a 250-cent per-run fee on its own leg.

func TestExportCAMT057ExactTags(t *testing.T) {
	l, sched := mt210TestLedger(t)
	doc, err := l.ExportCAMT057(CAMT057ExportOptions{ScheduleID: sched.ID, ServicingBIC: "CITIUS33"})
	if err != nil {
		t.Fatalf("ExportCAMT057: %v", err)
	}
	// Namespace and root: camt.057.001.06, the CBPR+ live version.
	for _, want := range []string{
		`<?xml version="1.0" encoding="UTF-8"?>`,
		`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.057.001.06">`,
		"<NtfctnToRcv>",
		"<GrpHdr>",
		// MsgId is deterministic: camt057-<schedule>-<runSeq>.
		"<MsgId>camt057-sched-1-0</MsgId>",
		"<CreDtTm>",
		"<Ntfctn>",
		// Ntfctn/Id repeats the MsgId.
		"<Id>camt057-sched-1-0</Id>",
		// Acct is the To account's registered IBAN.
		"<Acct>",
		"<IBAN>GB29NWBK60161331926819</IBAN>",
		// AcctSvcr names the servicing bank.
		"<AcctSvcr>",
		"<BICFI>CITIUS33</BICFI>",
		// TtlAmt is the principal: the 250-cent fee is a
		// separate leg and never reduces it. (Ntfctn carries no
		// direct Amt child per the XSD — the amount is TtlAmt
		// here and Amt inside the item.)
		`<TtlAmt Ccy="USD">100.00</TtlAmt>`,
		// XpctdValDt is the next run date, ISODate.
		"<XpctdValDt>2026-11-12</XpctdValDt>",
		// From has a registered BIC, so the debtor renders
		// Dbtr/Agt — never Dbtr/Pty.
		"<Dbtr>",
		"<Agt>",
		"<BICFI>DEUTDEFF</BICFI>",
		// The single item is the next run: Id is the run
		// transfer ID, Amt the principal.
		"<Itm>",
		"<Id>sched-1/run/0</Id>",
		`<Amt Ccy="USD">100.00</Amt>`,
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("export missing %q, body:\n%s", want, doc)
		}
	}
	// No Dbtr/Pty alongside Dbtr/Agt, and exactly one Amt (inside
	// the item) plus one TtlAmt: the XSD puts the per-item amount
	// under Itm only, never directly under Ntfctn.
	if strings.Contains(doc, "<Pty>") {
		t.Errorf("export must not carry Dbtr/Pty with a From BIC, body:\n%s", doc)
	}
	if n := strings.Count(doc, "<Amt "); n != 1 {
		t.Errorf("export carries %d Amt elements, want 1 (inside Itm), body:\n%s", n, doc)
	}
	if n := strings.Count(doc, "<TtlAmt "); n != 1 {
		t.Errorf("export carries %d TtlAmt elements, want 1, body:\n%s", n, doc)
	}
}

func TestExportCAMT057FeeNotDeducted(t *testing.T) {
	l, sched := mt210TestLedger(t)
	doc, err := l.ExportCAMT057(CAMT057ExportOptions{ScheduleID: sched.ID, ServicingBIC: "CITIUS33"})
	if err != nil {
		t.Fatalf("ExportCAMT057: %v", err)
	}
	// The schedule carries a 250-cent fee; the notice still
	// states the full 10000-cent principal — the fee books
	// separately.
	if !strings.Contains(doc, `<TtlAmt Ccy="USD">100.00</TtlAmt>`) {
		t.Errorf("export TtlAmt must equal the full principal, body:\n%s", doc)
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
	// "BOBACCT1" has no IBAN registration: Acct falls back to
	// Othr/Id with the account ID.
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

func TestExportCAMT057NoBICFallsBackToPty(t *testing.T) {
	l := New()
	if err := l.SetAccountIBAN("bob", "GB29NWBK60161331926819", "NWBKGB2L"); err != nil {
		t.Fatalf("SetAccountIBAN bob: %v", err)
	}
	// "alice" has no registered BIC: the debtor renders
	// Dbtr/Pty/Nm with the account ID — and Dbtr/Agt must not
	// appear (AcctSvcr still carries the servicing BIC).
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
	if !strings.Contains(doc, "<Nm>ALICE</Nm>") {
		t.Errorf("export missing Dbtr/Pty/Nm ALICE, body:\n%s", doc)
	}
	// Exactly one BICFI: the servicing bank's. A Dbtr/Agt would
	// add a second.
	if strings.Count(doc, "<BICFI>") != 1 || !strings.Contains(doc, "<BICFI>CITIUS33</BICFI>") {
		t.Errorf("export must carry exactly the servicing BICFI, body:\n%s", doc)
	}
}

func TestExportCAMT057NotActiveStatuses(t *testing.T) {
	// Paused, cancelled, and completed schedules each report
	// ErrCAMT057ScheduleNotActive: none of them has a next run
	// to notify.
	t.Run("paused", func(t *testing.T) {
		l, sched := mt210TestLedger(t)
		if _, err := l.PauseTransferSchedule(sched.ID); err != nil {
			t.Fatalf("PauseTransferSchedule: %v", err)
		}
		_, err := l.ExportCAMT057(CAMT057ExportOptions{ScheduleID: sched.ID, ServicingBIC: "CITIUS33"})
		if !errors.Is(err, ErrCAMT057ScheduleNotActive) {
			t.Errorf("err = %v, want ErrCAMT057ScheduleNotActive", err)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		l, sched := mt210TestLedger(t)
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
	l, sched := mt210TestLedger(t)
	// White-box the exhausted state: the schedule is still
	// active, but its NextRunAt has moved past EndsAt, so no run
	// is left to notify. (Same defensive-guard note as the MT210
	// test: the public API cannot reach this state.)
	nextRun := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	l.schedules[sched.ID].EndsAt = nextRun.Add(2 * time.Hour)
	l.schedules[sched.ID].NextRunAt = nextRun.Add(3 * time.Hour)
	_, err := l.ExportCAMT057(CAMT057ExportOptions{ScheduleID: sched.ID, ServicingBIC: "CITIUS33"})
	if !errors.Is(err, ErrCAMT057ScheduleNotActive) {
		t.Errorf("err = %v, want ErrCAMT057ScheduleNotActive", err)
	}
}

func TestExportCAMT057FXScheduleRejected(t *testing.T) {
	l := New()
	if _, _, err := l.CreateTransferSchedule(TransferSchedule{
		ID: "sched-1", From: "alice", To: "bob",
		AmountCents: 10000, Currency: "USD", ToCurrency: "EUR",
		FXAccount: "fx", Interval: 24 * time.Hour,
		NextRunAt: time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("CreateTransferSchedule FX: %v", err)
	}
	_, err := l.ExportCAMT057(CAMT057ExportOptions{ScheduleID: "sched-1", ServicingBIC: "CITIUS33"})
	if !errors.Is(err, ErrInvalidCAMT057Export) {
		t.Errorf("err = %v, want ErrInvalidCAMT057Export (receivable unknowable until fire time)", err)
	}
}

func TestExportCAMT057UnknownSchedule(t *testing.T) {
	l, _ := mt210TestLedger(t)
	_, err := l.ExportCAMT057(CAMT057ExportOptions{ScheduleID: "sched-nope", ServicingBIC: "CITIUS33"})
	if !errors.Is(err, ErrScheduleNotFound) {
		t.Errorf("err = %v, want ErrScheduleNotFound", err)
	}
}

func TestExportCAMT057BadServicingBIC(t *testing.T) {
	l, sched := mt210TestLedger(t)
	for _, bic := range []string{"", "CITI", "CITIUS33XXXX", "CITIUS3!"} {
		_, err := l.ExportCAMT057(CAMT057ExportOptions{ScheduleID: sched.ID, ServicingBIC: bic})
		if !errors.Is(err, ErrInvalidCAMT057Export) {
			t.Errorf("BIC %q: err = %v, want ErrInvalidCAMT057Export", bic, err)
		}
	}
}

func TestExportCAMT057MsgIDTooLong(t *testing.T) {
	l := New()
	// A 28-character schedule ID fits the ledger, but the
	// deterministic MsgId "camt057-<id>-0" is 38 characters —
	// past the Max35Text bound, so the notice fails loud
	// instead of truncating.
	id := "sched-camt057-too-long-00001"
	if len(id) != 28 {
		t.Fatalf("test fixture ID is %d characters, want 28", len(id))
	}
	if _, _, err := l.CreateTransferSchedule(TransferSchedule{
		ID: id, From: "alice", To: "bob",
		AmountCents: 10000, Currency: "USD", Interval: 24 * time.Hour,
		NextRunAt: time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("CreateTransferSchedule: %v", err)
	}
	_, err := l.ExportCAMT057(CAMT057ExportOptions{ScheduleID: id, ServicingBIC: "CITIUS33"})
	if !errors.Is(err, ErrInvalidCAMT057Export) {
		t.Errorf("err = %v, want ErrInvalidCAMT057Export (MsgId over Max35Text)", err)
	}
}

func TestExportCAMT057RepeatIdentical(t *testing.T) {
	l, sched := mt210TestLedger(t)
	opts := CAMT057ExportOptions{ScheduleID: sched.ID, ServicingBIC: "CITIUS33"}
	first, err := l.ExportCAMT057(opts)
	if err != nil {
		t.Fatalf("first ExportCAMT057: %v", err)
	}
	second, err := l.ExportCAMT057(opts)
	if err != nil {
		t.Fatalf("second ExportCAMT057: %v", err)
	}
	if first != second {
		t.Errorf("repeat export differs:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

func TestExportCAMT057CrossCheckMT210(t *testing.T) {
	// The MX twin must agree with the FIN notice on the same
	// schedule and run: :20: vs MsgId (same schedule ID), :25:
	// vs Acct (same IBAN), :30: vs XpctdValDt (same next run
	// date), :32B: vs TtlAmt/Itm Amt (same principal, fee not
	// deducted), :21: vs Itm/Id (same run transfer ID).
	l, sched := mt210TestLedger(t)
	opts := MT210ExportOptions{ScheduleID: sched.ID, ServicingBIC: "CITIUS33"}
	mt210, err := l.ExportMT210(opts)
	if err != nil {
		t.Fatalf("ExportMT210: %v", err)
	}
	camt057, err := l.ExportCAMT057(CAMT057ExportOptions{ScheduleID: sched.ID, ServicingBIC: "CITIUS33"})
	if err != nil {
		t.Fatalf("ExportCAMT057: %v", err)
	}
	checks := [][2]string{
		{":20:sched-1", "<MsgId>camt057-sched-1-0</MsgId>"},
		{":25:GB29NWBK60161331926819", "<IBAN>GB29NWBK60161331926819</IBAN>"},
		{":30:261112", "<XpctdValDt>2026-11-12</XpctdValDt>"},
		{":32B:USD100,00", `<TtlAmt Ccy="USD">100.00</TtlAmt>`},
		{":32B:USD100,00", `<Amt Ccy="USD">100.00</Amt>`},
		{":21:sched-1/run/0", "<Id>sched-1/run/0</Id>"},
		{":52A:DEUTDEFF", "<BICFI>DEUTDEFF</BICFI>"},
	}
	for _, c := range checks {
		if !strings.Contains(mt210, c[0]) {
			t.Errorf("MT210 missing %q, body:\n%s", c[0], mt210)
		}
		if !strings.Contains(camt057, c[1]) {
			t.Errorf("camt.057 missing %q, body:\n%s", c[1], camt057)
		}
	}
}
