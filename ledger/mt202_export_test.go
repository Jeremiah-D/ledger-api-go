package ledger

import (
	"errors"
	"testing"
	"time"
)

// mt202ExportTestLedger is a bare ledger wired to an audit log, so
// the export marker event is assertable. Unlike the MT295 fixture
// no schedule or accounts are needed: an MT202 export names two
// banks and a value, and the caller supplies them.
func mt202ExportTestLedger(t *testing.T, al *AuditLog) *Ledger {
	t.Helper()
	return New(WithAuditLog(al))
}

func TestExportMT202RegisteredFirstExportRegisters(t *testing.T) {
	al := mustAuditLog(t)
	l := mt202ExportTestLedger(t, al)
	m := mt202Sample()
	res, err := l.ExportMT202Registered(m, "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT202Registered: %v", err)
	}
	if res.Reference != "MT202-REF-1" || res.RelatedReference != "ORIG-REF-9" {
		t.Errorf("result refs = %q/%q, want MT202-REF-1/ORIG-REF-9", res.Reference, res.RelatedReference)
	}
	if res.Duplicate {
		t.Error("duplicate = true on a first export")
	}
	want, err := ExportMT202(m)
	if err != nil {
		t.Fatalf("ExportMT202: %v", err)
	}
	if res.Message != want {
		t.Errorf("registered message differs from the pure render:\n%q\nwant:\n%q", res.Message, want)
	}
	// The stored export round-trips through the registry, verbatim.
	stored, ok := l.GetMT202Export("MT202-REF-1")
	if !ok {
		t.Fatal("GetMT202Export = false, want the stored export")
	}
	if stored.Message != res.Message || stored.RelatedReference != "ORIG-REF-9" || stored.Duplicate {
		t.Errorf("stored export = %+v, want the rendered message", stored)
	}
}

func TestExportMT202RegisteredRepeatIsDuplicate(t *testing.T) {
	al := mustAuditLog(t)
	l := mt202ExportTestLedger(t, al)
	first, err := l.ExportMT202Registered(mt202Sample(), "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT202Registered: %v", err)
	}
	second, err := l.ExportMT202Registered(mt202Sample(), "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT202Registered repeat: %v", err)
	}
	if !second.Duplicate {
		t.Error("duplicate = false on repeat export")
	}
	if second.Message != first.Message {
		t.Errorf("repeat export differs:\nfirst:\n%s\nsecond:\n%s", first.Message, second.Message)
	}
	// The replay emits no second audit event.
	events := readAuditOps(t, al)
	if countOp(events, "mt202_exported") != 1 {
		t.Errorf("mt202_exported count = %d, want 1 after replay", countOp(events, "mt202_exported"))
	}
}

func TestExportMT202RegisteredConflict(t *testing.T) {
	al := mustAuditLog(t)
	l := mt202ExportTestLedger(t, al)
	first, err := l.ExportMT202Registered(mt202Sample(), "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT202Registered: %v", err)
	}
	cases := []struct {
		name string
		mut  func(*MT202)
	}{
		{"different amount", func(m *MT202) { m.AmountCents = 250_001 }},
		{"different :21:", func(m *MT202) { m.RelatedReference = "OTHER-REF-1" }},
		{"different value date", func(m *MT202) { m.ValueDate = time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC) }},
		{"different currency", func(m *MT202) { m.Currency = "EUR" }},
		{"different ordering BIC", func(m *MT202) { m.OrderingInstitutionBIC = "BNPAFRPP" }},
		{"different beneficiary BIC", func(m *MT202) { m.BeneficiaryInstitutionBIC = "DEUTDEFF" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := mt202Sample()
			tc.mut(&m)
			if _, err := l.ExportMT202Registered(m, "ledger-operator"); !errors.Is(err, ErrMT202ExportMsgIDConflict) {
				t.Errorf("err = %v, want ErrMT202ExportMsgIDConflict", err)
			}
		})
	}
	// The conflict overwrote nothing: the stored message is still
	// the first rendering, and no second audit event landed.
	stored, ok := l.GetMT202Export("MT202-REF-1")
	if !ok || stored.Message != first.Message {
		t.Errorf("stored export after conflicts = %+v, want the first message", stored)
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt202_exported") != 1 {
		t.Errorf("mt202_exported count = %d, want 1 after conflicts", countOp(events, "mt202_exported"))
	}
}

func TestExportMT202RegisteredDifferentLTsReplayFirst(t *testing.T) {
	al := mustAuditLog(t)
	l := mt202ExportTestLedger(t, al)
	first, err := l.ExportMT202Registered(mt202Sample(), "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT202Registered: %v", err)
	}
	// The LTs are envelope, not terms: a replay under different
	// LTs returns the first rendering, not a conflict.
	m := mt202Sample()
	m.SenderLT = "DEUTDEFF"
	m.ReceiverLT = "CHASUS33"
	second, err := l.ExportMT202Registered(m, "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT202Registered under different LTs: %v", err)
	}
	if !second.Duplicate {
		t.Error("duplicate = false on a replay under different LTs")
	}
	if second.Message != first.Message {
		t.Errorf("replay under different LTs differs:\nfirst:\n%s\nsecond:\n%s", first.Message, second.Message)
	}
}

func TestExportMT202RegisteredBICCaseReplayFirst(t *testing.T) {
	al := mustAuditLog(t)
	l := mt202ExportTestLedger(t, al)
	first, err := l.ExportMT202Registered(mt202Sample(), "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT202Registered: %v", err)
	}
	// The render uppercases BICs, so a replay differing only in
	// BIC case renders the byte-identical message and is the same
	// terms, not a conflict.
	m := mt202Sample()
	m.OrderingInstitutionBIC = "bankus33"
	second, err := l.ExportMT202Registered(m, "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT202Registered with lowercase BIC: %v", err)
	}
	if !second.Duplicate {
		t.Error("duplicate = false on a replay differing only in BIC case")
	}
	if second.Message != first.Message {
		t.Errorf("BIC-case replay differs:\nfirst:\n%s\nsecond:\n%s", first.Message, second.Message)
	}
}

func TestExportMT202RegisteredAuditEvent(t *testing.T) {
	al := mustAuditLog(t)
	l := mt202ExportTestLedger(t, al)
	before := l.Version()
	res, err := l.ExportMT202Registered(mt202Sample(), "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT202Registered: %v", err)
	}
	// Audit: exactly one export marker, with the exact details —
	// the :20: on the durable record, no accounts.
	events := readAuditOps(t, al)
	if countOp(events, "mt202_exported") != 1 {
		t.Fatalf("mt202_exported count = %d, want 1", countOp(events, "mt202_exported"))
	}
	ev := findOp(events, "mt202_exported")
	if ev == nil {
		t.Fatal("mt202_exported event missing")
	}
	if ev.Actor != "ledger-operator" {
		t.Errorf("actor = %q, want ledger-operator", ev.Actor)
	}
	if ev.TraceID != res.Reference {
		t.Errorf("trace = %q, want %q", ev.TraceID, res.Reference)
	}
	if ev.Details["msg_id"] != "MT202-REF-1" || ev.Details["related_reference"] != "ORIG-REF-9" ||
		ev.Details["sender_lt"] != "BANKUS33XXX" {
		t.Errorf("details = %v, want msg/related/sender", ev.Details)
	}
	if len(ev.Accounts) != 0 {
		t.Errorf("accounts = %v, want none (a transfer touches no account)", ev.Accounts)
	}
	if ev.VersionBefore != before || ev.VersionAfter != before {
		t.Errorf("version %d -> %d, want %d -> %d (read-only, no bump)", ev.VersionBefore, ev.VersionAfter, before, before)
	}
}

func TestExportMT202RegisteredReadOnly(t *testing.T) {
	al := mustAuditLog(t)
	l := mt202ExportTestLedger(t, al)
	before := l.Version()
	if _, err := l.ExportMT202Registered(mt202Sample(), "ledger-operator"); err != nil {
		t.Fatalf("ExportMT202Registered: %v", err)
	}
	// Rendering is read-only: no version bump, no journal entry.
	if l.Version() != before {
		t.Errorf("version = %d, want %d (read-only, no bump)", l.Version(), before)
	}
	if got := len(l.Entries()); got != 0 {
		t.Errorf("journal entries = %d, want 0 (registration books nothing)", got)
	}
}

func TestGetMT202ExportUnknown(t *testing.T) {
	al := mustAuditLog(t)
	l := mt202ExportTestLedger(t, al)
	if _, ok := l.GetMT202Export("NEVER-EXPORTED"); ok {
		t.Error("GetMT202Export = true for an unknown :20:, want false")
	}
}

func TestExportMT202RegisteredBadFieldRegistersNothing(t *testing.T) {
	al := mustAuditLog(t)
	l := mt202ExportTestLedger(t, al)
	m := mt202Sample()
	m.AmountCents = 0
	if _, err := l.ExportMT202Registered(m, "ledger-operator"); !errors.Is(err, ErrMT202Amount) {
		t.Fatalf("err = %v, want ErrMT202Amount", err)
	}
	// A refused export records nothing: no registry entry, no
	// audit marker — and the same :20: stays exportable.
	if _, ok := l.GetMT202Export("MT202-REF-1"); ok {
		t.Error("GetMT202Export = true for a refused export, want false")
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt202_exported") != 0 {
		t.Errorf("mt202_exported count = %d, want 0 for a refused export", countOp(events, "mt202_exported"))
	}
	if _, err := l.ExportMT202Registered(mt202Sample(), "ledger-operator"); err != nil {
		t.Errorf("ExportMT202Registered after refusal: %v, want success", err)
	}
}
