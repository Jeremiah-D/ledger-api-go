package ledger

import (
	"errors"
	"testing"
	"time"
)

// mt202ApplyMsg is one inbound MT202 test instruction: the
// ordering bank DEUTDEFF moves USD 100.00 from its nostro account
// to the beneficiary bank BNPAFRPP's vostro account, value date
// today (UTC), related to ORIG-REF-9 (the customer leg's MT103).
func mt202ApplyMsg(ref string) MT202 {
	return MT202{
		SenderLT:                  "DEUTDEFFXXX",
		ReceiverLT:                "BNPAFRPPXXX",
		TransactionReference:      ref,
		RelatedReference:          "ORIG-REF-9",
		ValueDate:                 time.Now().UTC(),
		Currency:                  "USD",
		AmountCents:               10000,
		OrderingInstitutionBIC:    "DEUTDEFF",
		BeneficiaryInstitutionBIC: "BNPAFRPP",
	}
}

// mt202ApplyLedger builds a ledger with a funded nostro account
// and both BICs registered, ready for MT202 application through
// PostTransfer: nostro-acct (DEUTDEFF, nostro) holds USD
// 10,000.00, vostro-acct (BNPAFRPP, vostro) holds nothing yet.
func mt202ApplyLedger(t *testing.T, al *AuditLog) *Ledger {
	t.Helper()
	l := New(WithAuditLog(al))
	if _, _, err := l.Post(JournalEntry{ID: "fund-nostro", DebitAccount: "nostro-acct", CreditAccount: "bank", AmountCents: 1000000, Currency: "USD"}); err != nil {
		t.Fatalf("fund Post: %v", err)
	}
	if _, err := l.RegisterNostroAccount("nostro-acct", "DEUTDEFF", NostroKindNostro); err != nil {
		t.Fatalf("RegisterNostroAccount nostro: %v", err)
	}
	if _, err := l.RegisterNostroAccount("vostro-acct", "BNPAFRPP", NostroKindVostro); err != nil {
		t.Fatalf("RegisterNostroAccount vostro: %v", err)
	}
	return l
}

func TestApplyMT202_TransfersBetweenNostroAccounts(t *testing.T) {
	al, err := NewAuditLog(t.TempDir())
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := mt202ApplyLedger(t, al)
	res, err := l.ApplyMT202(mt202ApplyMsg("MT202-REF-01"), "")
	if err != nil {
		t.Fatalf("ApplyMT202: %v", err)
	}
	if res.Err != nil {
		t.Fatalf("ApplyMT202 result Err = %v, want nil", res.Err)
	}
	if res.RepairID != "" {
		t.Errorf("RepairID = %q, want empty", res.RepairID)
	}
	if res.From != "nostro-acct" || res.To != "vostro-acct" {
		t.Errorf("legs = %q->%q, want nostro-acct->vostro-acct", res.From, res.To)
	}
	if res.EntryID != "MT202-REF-01" {
		t.Errorf("EntryID = %q, want MT202-REF-01", res.EntryID)
	}
	if res.Duplicate {
		t.Error("duplicate = true on first apply, want false")
	}
	// The money moved: the nostro account paid 100.00, the
	// vostro account received it.
	if got := l.BalanceIn("nostro-acct", "USD"); got != 1000000-10000 {
		t.Errorf("nostro-acct balance = %d, want %d", got, 1000000-10000)
	}
	if got := l.BalanceIn("vostro-acct", "USD"); got != 10000 {
		t.Errorf("vostro-acct balance = %d, want 10000", got)
	}
	// Audit: one mt202_inbound_applied event with the exact
	// details — the :20: on the durable record, both BICs,
	// and the :21: related reference recorded, not routed.
	events := readAuditOps(t, al)
	if countOp(events, "mt202_inbound_applied") != 1 {
		t.Fatalf("mt202_inbound_applied count = %d, want 1", countOp(events, "mt202_inbound_applied"))
	}
	ev := findOp(events, "mt202_inbound_applied")
	if ev.Actor != "ApplyMT202" {
		t.Errorf("actor = %q, want ApplyMT202", ev.Actor)
	}
	if ev.Details["msg_id"] != "MT202-REF-01" {
		t.Errorf("msg_id = %v, want MT202-REF-01", ev.Details["msg_id"])
	}
	if ev.Details["ordering_bic"] != "DEUTDEFF" {
		t.Errorf("ordering_bic = %v, want DEUTDEFF", ev.Details["ordering_bic"])
	}
	if ev.Details["beneficiary_bic"] != "BNPAFRPP" {
		t.Errorf("beneficiary_bic = %v, want BNPAFRPP", ev.Details["beneficiary_bic"])
	}
	if ev.Details["amount_cents"] != float64(10000) {
		t.Errorf("amount_cents = %v, want 10000", ev.Details["amount_cents"])
	}
	if ev.Details["currency"] != "USD" {
		t.Errorf("currency = %v, want USD", ev.Details["currency"])
	}
	if ev.Details["related_reference"] != "ORIG-REF-9" {
		t.Errorf("related_reference = %v, want ORIG-REF-9", ev.Details["related_reference"])
	}
}

func TestApplyMT202_UnregisteredOrderingBICGoesToRepair(t *testing.T) {
	al, err := NewAuditLog(t.TempDir())
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := mt202ApplyLedger(t, al)
	// The ordering BIC is well-formed but no account registered
	// it as its Correspondent.
	m := mt202ApplyMsg("MT202-REF-01")
	m.OrderingInstitutionBIC = "COBADEFF"
	res, err := l.ApplyMT202(m, "")
	if err != nil {
		t.Fatalf("ApplyMT202: %v", err)
	}
	if res.Err == nil {
		t.Fatal("ApplyMT202 result Err = nil, want ErrMT202UnknownBIC")
	}
	if !errors.Is(res.Err, ErrMT202UnknownBIC) {
		t.Errorf("Err = %v, want ErrMT202UnknownBIC", res.Err)
	}
	if res.RepairID == "" {
		t.Fatal("RepairID empty, want a repair-queue record")
	}
	if rec, ok := l.GetRepair(res.RepairID); !ok || rec.Status != RepairStatusOpen {
		t.Errorf("GetRepair(%q) = %+v/%v, want an open record", res.RepairID, rec, ok)
	}
	// Nothing booked: no entry, no money moved, no audit event,
	// and no account was auto-created from the wire BIC.
	if res.EntryID != "" {
		t.Errorf("EntryID = %q, want empty", res.EntryID)
	}
	if got := l.BalanceIn("nostro-acct", "USD"); got != 1000000 {
		t.Errorf("nostro-acct balance = %d, want 1000000", got)
	}
	if got := l.BalanceIn("vostro-acct", "USD"); got != 0 {
		t.Errorf("vostro-acct balance = %d, want 0", got)
	}
	l.mu.RLock()
	_, resolves := l.nostroByCorrespondentLocked("COBADEFF")
	l.mu.RUnlock()
	if resolves {
		t.Error("unregistered BIC resolves after apply, want no auto-created account")
	}
	if countOp(readAuditOps(t, al), "mt202_inbound_applied") != 0 {
		t.Error("mt202_inbound_applied emitted for a parked instruction, want none")
	}
}

func TestApplyMT202_UnregisteredBeneficiaryBICGoesToRepair(t *testing.T) {
	al, err := NewAuditLog(t.TempDir())
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := mt202ApplyLedger(t, al)
	// The beneficiary BIC is well-formed but no account
	// registered it as its Correspondent.
	m := mt202ApplyMsg("MT202-REF-01")
	m.BeneficiaryInstitutionBIC = "CHASGB2L"
	res, err := l.ApplyMT202(m, "")
	if err != nil {
		t.Fatalf("ApplyMT202: %v", err)
	}
	if res.Err == nil {
		t.Fatal("ApplyMT202 result Err = nil, want ErrMT202UnknownBIC")
	}
	if !errors.Is(res.Err, ErrMT202UnknownBIC) {
		t.Errorf("Err = %v, want ErrMT202UnknownBIC", res.Err)
	}
	if res.RepairID == "" {
		t.Fatal("RepairID empty, want a repair-queue record")
	}
	if rec, ok := l.GetRepair(res.RepairID); !ok || rec.Status != RepairStatusOpen {
		t.Errorf("GetRepair(%q) = %+v/%v, want an open record", res.RepairID, rec, ok)
	}
	if got := l.BalanceIn("nostro-acct", "USD"); got != 1000000 {
		t.Errorf("nostro-acct balance = %d, want 1000000", got)
	}
	if countOp(readAuditOps(t, al), "mt202_inbound_applied") != 0 {
		t.Error("mt202_inbound_applied emitted for a parked instruction, want none")
	}
}

func TestApplyMT202_WrongKindGoesToRepair(t *testing.T) {
	al, err := NewAuditLog(t.TempDir())
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := mt202ApplyLedger(t, al)
	// The ordering BIC resolves, but to a vostro account: the
	// :52A: leg may only debit a nostro account.
	if _, err := l.RegisterNostroAccount("nostro-acct", "DEUTDEFF", NostroKindVostro); err != nil {
		t.Fatalf("re-register nostro-acct as vostro: %v", err)
	}
	res, err := l.ApplyMT202(mt202ApplyMsg("MT202-REF-01"), "")
	if err != nil {
		t.Fatalf("ApplyMT202: %v", err)
	}
	if res.Err == nil {
		t.Fatal("ApplyMT202 result Err = nil, want ErrMT202WrongKind")
	}
	if !errors.Is(res.Err, ErrMT202WrongKind) {
		t.Errorf("Err = %v, want ErrMT202WrongKind", res.Err)
	}
	if res.RepairID == "" {
		t.Fatal("RepairID empty, want a repair-queue record")
	}
	if got := l.BalanceIn("nostro-acct", "USD"); got != 1000000 {
		t.Errorf("nostro-acct balance = %d, want 1000000 (wrong kind booked nothing)", got)
	}
	// The beneficiary side wrong kind parks the same way: the
	// :58A: leg may only credit a vostro account.
	if _, err := l.RegisterNostroAccount("nostro-acct", "DEUTDEFF", NostroKindNostro); err != nil {
		t.Fatalf("restore nostro-acct as nostro: %v", err)
	}
	if _, err := l.RegisterNostroAccount("vostro-acct", "BNPAFRPP", NostroKindNostro); err != nil {
		t.Fatalf("re-register vostro-acct as nostro: %v", err)
	}
	res, err = l.ApplyMT202(mt202ApplyMsg("MT202-REF-02"), "")
	if err != nil {
		t.Fatalf("ApplyMT202(beneficiary wrong kind): %v", err)
	}
	if !errors.Is(res.Err, ErrMT202WrongKind) || res.RepairID == "" {
		t.Errorf("beneficiary wrong kind: Err=%v RepairID=%q, want ErrMT202WrongKind + repair", res.Err, res.RepairID)
	}
	// No audit event for either parked instruction (readAuditOps
	// closes the log, so it runs last, after both applies).
	if countOp(readAuditOps(t, al), "mt202_inbound_applied") != 0 {
		t.Error("mt202_inbound_applied emitted for a parked instruction, want none")
	}
}

func TestApplyMT202_RefusesFrozenAccount(t *testing.T) {
	al, err := NewAuditLog(t.TempDir())
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := mt202ApplyLedger(t, al)
	l.Freeze("nostro-acct")
	res, err := l.ApplyMT202(mt202ApplyMsg("MT202-REF-01"), "")
	if err != nil {
		t.Fatalf("ApplyMT202: %v", err)
	}
	if res.Err == nil {
		t.Fatal("ApplyMT202 result Err = nil, want a frozen-account refusal")
	}
	if res.RepairID == "" {
		t.Fatal("RepairID empty, want a repair-queue record")
	}
	if rec, ok := l.GetRepair(res.RepairID); !ok || rec.Status != RepairStatusOpen {
		t.Errorf("GetRepair(%q) = %+v/%v, want an open record", res.RepairID, rec, ok)
	}
	if got := l.BalanceIn("nostro-acct", "USD"); got != 1000000 {
		t.Errorf("nostro-acct balance = %d, want 1000000 (frozen account moved nothing)", got)
	}
	if countOp(readAuditOps(t, al), "mt202_inbound_applied") != 0 {
		t.Error("mt202_inbound_applied emitted for a refused instruction, want none")
	}
}

func TestApplyMT202_RefusesFutureValueDate(t *testing.T) {
	al, err := NewAuditLog(t.TempDir())
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := mt202ApplyLedger(t, al)
	m := mt202ApplyMsg("MT202-REF-01")
	m.ValueDate = time.Now().UTC().Add(24 * time.Hour)
	res, err := l.ApplyMT202(m, "")
	if err != nil {
		t.Fatalf("ApplyMT202: %v", err)
	}
	if res.Err == nil {
		t.Fatal("ApplyMT202 result Err = nil, want ErrMT202ValueDateNotToday")
	}
	if !errors.Is(res.Err, ErrMT202ValueDateNotToday) {
		t.Errorf("Err = %v, want ErrMT202ValueDateNotToday", res.Err)
	}
	if res.RepairID == "" {
		t.Fatal("RepairID empty, want a repair-queue record")
	}
	if got := l.BalanceIn("nostro-acct", "USD"); got != 1000000 {
		t.Errorf("nostro-acct balance = %d, want 1000000 (future-dated instruction booked nothing)", got)
	}
	// A past date parks the same way: the ledger schedules
	// nothing, forward or backward.
	m = mt202ApplyMsg("MT202-REF-PAST")
	m.ValueDate = time.Now().UTC().Add(-24 * time.Hour)
	res, err = l.ApplyMT202(m, "")
	if err != nil {
		t.Fatalf("ApplyMT202(past date): %v", err)
	}
	if !errors.Is(res.Err, ErrMT202ValueDateNotToday) || res.RepairID == "" {
		t.Errorf("past date: Err=%v RepairID=%q, want ErrMT202ValueDateNotToday + repair", res.Err, res.RepairID)
	}
	// No audit event for either parked instruction (readAuditOps
	// closes the log, so it runs last, after both applies).
	if countOp(readAuditOps(t, al), "mt202_inbound_applied") != 0 {
		t.Error("mt202_inbound_applied emitted for a future-dated instruction, want none")
	}
}

func TestApplyMT202_ReplaySameReferenceNoop(t *testing.T) {
	al, err := NewAuditLog(t.TempDir())
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := mt202ApplyLedger(t, al)
	first, err := l.ApplyMT202(mt202ApplyMsg("MT202-REF-01"), "")
	if err != nil {
		t.Fatalf("ApplyMT202: %v", err)
	}
	if first.Err != nil {
		t.Fatalf("first ApplyMT202 Err = %v", first.Err)
	}
	second, err := l.ApplyMT202(mt202ApplyMsg("MT202-REF-01"), "")
	if err != nil {
		t.Fatalf("replay ApplyMT202: %v", err)
	}
	if second.Err != nil {
		t.Fatalf("replay Err = %v, want nil", second.Err)
	}
	if !second.Duplicate {
		t.Error("duplicate = false on replay, want true")
	}
	if second.EntryID != first.EntryID {
		t.Errorf("replay EntryID = %q, want %q", second.EntryID, first.EntryID)
	}
	// Nothing booked twice: the balances moved once, and the
	// audit event fired once.
	if got := l.BalanceIn("nostro-acct", "USD"); got != 1000000-10000 {
		t.Errorf("nostro-acct balance = %d, want %d", got, 1000000-10000)
	}
	if got := l.BalanceIn("vostro-acct", "USD"); got != 10000 {
		t.Errorf("vostro-acct balance = %d, want 10000", got)
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt202_inbound_applied") != 1 {
		t.Errorf("mt202_inbound_applied count = %d, want 1", countOp(events, "mt202_inbound_applied"))
	}
}

func TestApplyMT202_SameReferenceDifferentTermsRefused(t *testing.T) {
	al, err := NewAuditLog(t.TempDir())
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := mt202ApplyLedger(t, al)
	if _, err := l.ApplyMT202(mt202ApplyMsg("MT202-REF-01"), ""); err != nil {
		t.Fatalf("ApplyMT202: %v", err)
	}
	// Same :20:, different amount: a conflict, never a silent
	// overwrite — and the original booking stands.
	m := mt202ApplyMsg("MT202-REF-01")
	m.AmountCents = 20000
	if _, err := l.ApplyMT202(m, ""); err == nil {
		t.Fatal("ApplyMT202(different terms): want error, got nil")
	} else if !errors.Is(err, ErrMT202MsgIDConflict) {
		t.Errorf("err = %v, want ErrMT202MsgIDConflict", err)
	}
	if got := l.BalanceIn("vostro-acct", "USD"); got != 10000 {
		t.Errorf("vostro-acct balance = %d, want 10000 (conflict booked nothing)", got)
	}
	// Same :20:, different value date: a conflict too.
	m = mt202ApplyMsg("MT202-REF-01")
	m.ValueDate = time.Now().UTC().Add(24 * time.Hour)
	if _, err := l.ApplyMT202(m, ""); !errors.Is(err, ErrMT202MsgIDConflict) {
		t.Errorf("different value date: err = %v, want ErrMT202MsgIDConflict", err)
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt202_inbound_applied") != 1 {
		t.Errorf("mt202_inbound_applied count = %d, want 1", countOp(events, "mt202_inbound_applied"))
	}
}

func TestApplyMT202_RelatedReferenceRecorded(t *testing.T) {
	al, err := NewAuditLog(t.TempDir())
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := mt202ApplyLedger(t, al)
	// The :21: names the customer leg this bank-to-bank leg
	// relates to; it is recorded, never routed — so the same
	// :21: on a second instruction with a different :20: still
	// books.
	m := mt202ApplyMsg("MT202-REF-01")
	m.RelatedReference = "CUST-LEG-77"
	res, err := l.ApplyMT202(m, "")
	if err != nil {
		t.Fatalf("ApplyMT202: %v", err)
	}
	if res.Err != nil {
		t.Fatalf("ApplyMT202 result Err = %v, want nil", res.Err)
	}
	ev := findOp(readAuditOps(t, al), "mt202_inbound_applied")
	if ev == nil {
		t.Fatal("mt202_inbound_applied event missing")
	}
	if ev.Details["related_reference"] != "CUST-LEG-77" {
		t.Errorf("related_reference = %v, want CUST-LEG-77", ev.Details["related_reference"])
	}
	if ev.Details["msg_id"] != "MT202-REF-01" {
		t.Errorf("msg_id = %v, want MT202-REF-01", ev.Details["msg_id"])
	}
}

func TestParseMT202_RoundTripViaExport(t *testing.T) {
	al, err := NewAuditLog(t.TempDir())
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := mt202ApplyLedger(t, al)
	want := mt202ApplyMsg("MT202-RT-01")
	// :32A: carries only a calendar day, so the parsed value
	// date is midnight UTC: normalize the expectation the same
	// way before comparing structs.
	now := time.Now().UTC()
	want.ValueDate = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	// SenderLT/ReceiverLT normalize BIC11 → BIC11+"X" on
	// export (the MT103 rule, as in TestParseMT202_RoundTrip).
	want.SenderLT = "DEUTDEFFXXXX"
	want.ReceiverLT = "BNPAFRPPXXXX"
	// Export renders the instruction; ParseMT202 reads it back
	// byte-for-byte, and ApplyMT202 books exactly those terms.
	msg, err := ExportMT202(want)
	if err != nil {
		t.Fatalf("ExportMT202: %v", err)
	}
	got, err := ParseMT202(msg)
	if err != nil {
		t.Fatalf("ParseMT202: %v", err)
	}
	if got != want {
		t.Errorf("ParseMT202(ExportMT202(m)) = %+v, want %+v", got, want)
	}
	res, err := l.ApplyMT202(got, "")
	if err != nil {
		t.Fatalf("ApplyMT202: %v", err)
	}
	if res.Err != nil {
		t.Fatalf("ApplyMT202 result Err = %v, want nil", res.Err)
	}
	if res.From != "nostro-acct" || res.To != "vostro-acct" {
		t.Errorf("legs = %q->%q, want nostro-acct->vostro-acct", res.From, res.To)
	}
	if got := l.BalanceIn("vostro-acct", "USD"); got != 10000 {
		t.Errorf("vostro-acct balance = %d, want 10000", got)
	}
}
