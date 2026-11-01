package ledger

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// draftPayoutWithPurpose drafts a payout carrying an explicit ISO
// 20022 purpose code.
func draftPayoutWithPurpose(t *testing.T, l *Ledger, id string, amount int64, purposeCode string) PayoutInstruction {
	t.Helper()
	p, dup, err := l.CreatePayout(PayoutInstruction{
		ID:           id,
		PayeeAccount: "saver",
		AmountCents:  amount,
		Destination:  "FPS:6222000012345678",
		Maker:        "alice",
		PurposeCode:  purposeCode,
	})
	if err != nil {
		t.Fatalf("CreatePayout: %v", err)
	}
	if dup {
		t.Fatal("duplicate = true on first create")
	}
	return p
}

func TestAmendPayoutPurposeCodeValidation(t *testing.T) {
	l := seedPayoutLedger(t)
	orig := draftPayoutWithPurpose(t, l, "po-1", 100_000, "SALA")

	// Unknown ID.
	if _, err := l.AmendPayoutPurposeCode("nope", "SUPP"); !errors.Is(err, ErrPayoutNotFound) {
		t.Errorf("unknown: err = %v, want ErrPayoutNotFound", err)
	}

	// The new code reuses the CreatePayout gate: a code outside the
	// admitted ISO 20022 subset is rejected, and the original code
	// is untouched.
	if _, err := l.AmendPayoutPurposeCode("po-1", "XXXX"); !errors.Is(err, ErrInvalidPurposeCode) {
		t.Errorf("unknown code: err = %v, want ErrInvalidPurposeCode", err)
	}
	if p, _ := l.GetPayout("po-1"); !reflect.DeepEqual(p, orig) {
		t.Errorf("payout after invalid code = %+v, want %+v (untouched)", p, orig)
	}

	// The match is exact and case-sensitive: a lowercase near-miss
	// is not a code.
	if _, err := l.AmendPayoutPurposeCode("po-1", "supp"); !errors.Is(err, ErrInvalidPurposeCode) {
		t.Errorf("lowercase code: err = %v, want ErrInvalidPurposeCode", err)
	}
	if p, _ := l.GetPayout("po-1"); p.PurposeCode != "SALA" {
		t.Errorf("purpose = %q after lowercase code, want SALA (untouched)", p.PurposeCode)
	}
}

func TestAmendPayoutPurposeCodeStateGate(t *testing.T) {
	// Every non-draft status is rejected: approved, submitted,
	// settled, failed, cancelled, and expired.
	t.Run("approved", func(t *testing.T) {
		l := seedPayoutLedger(t)
		draftPayoutWithPurpose(t, l, "po-1", 100_000, "SALA")
		if _, err := l.ApprovePayout("po-1", "bob"); err != nil {
			t.Fatalf("ApprovePayout: %v", err)
		}
		if _, err := l.AmendPayoutPurposeCode("po-1", "SUPP"); !errors.Is(err, ErrPayoutInvalidTransition) {
			t.Errorf("err = %v, want ErrPayoutInvalidTransition", err)
		}
		if p, _ := l.GetPayout("po-1"); p.PurposeCode != "SALA" {
			t.Errorf("purpose moved to %q after rejected amendment", p.PurposeCode)
		}
	})
	t.Run("submitted", func(t *testing.T) {
		l := seedPayoutLedger(t)
		draftPayoutWithPurpose(t, l, "po-1", 100_000, "SALA")
		approveSubmitPayout(t, l, "po-1", "bob")
		if _, err := l.AmendPayoutPurposeCode("po-1", "SUPP"); !errors.Is(err, ErrPayoutInvalidTransition) {
			t.Errorf("err = %v, want ErrPayoutInvalidTransition", err)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		l := seedPayoutLedger(t)
		draftPayoutWithPurpose(t, l, "po-1", 100_000, "SALA")
		if _, err := l.CancelPayout("po-1"); err != nil {
			t.Fatalf("CancelPayout: %v", err)
		}
		if _, err := l.AmendPayoutPurposeCode("po-1", "SUPP"); !errors.Is(err, ErrPayoutInvalidTransition) {
			t.Errorf("err = %v, want ErrPayoutInvalidTransition", err)
		}
	})
	t.Run("expired", func(t *testing.T) {
		l := seedPayoutLedger(t)
		draftPayoutWithExpiry(t, l, "po-1", 100_000, time.Now().Add(time.Hour))
		if _, err := l.ApprovePayout("po-1", "bob"); err != nil {
			t.Fatalf("ApprovePayout: %v", err)
		}
		if expired := l.ExpirePayoutsAt(time.Now().Add(2 * time.Hour)); len(expired) != 1 {
			t.Fatalf("ExpirePayoutsAt expired %v, want [po-1]", expired)
		}
		if _, err := l.AmendPayoutPurposeCode("po-1", "SUPP"); !errors.Is(err, ErrPayoutInvalidTransition) {
			t.Errorf("err = %v, want ErrPayoutInvalidTransition", err)
		}
	})

	// The state gate runs before the purpose validation and the
	// no-op check: even a same-value or invalid-code amendment is
	// rejected on a departed instruction.
	l := seedPayoutLedger(t)
	draftPayoutWithPurpose(t, l, "po-1", 100_000, "SALA")
	if _, err := l.ApprovePayout("po-1", "bob"); err != nil {
		t.Fatalf("ApprovePayout: %v", err)
	}
	if _, err := l.AmendPayoutPurposeCode("po-1", "SALA"); !errors.Is(err, ErrPayoutInvalidTransition) {
		t.Errorf("approved, same code: err = %v, want ErrPayoutInvalidTransition", err)
	}
	if _, err := l.AmendPayoutPurposeCode("po-1", "XXXX"); !errors.Is(err, ErrPayoutInvalidTransition) {
		t.Errorf("approved, invalid code: err = %v, want ErrPayoutInvalidTransition", err)
	}
}

func TestAmendPayoutPurposeCodeNoOp(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithPayoutClearingAccount("payout-clearing"))
	seedBalance(t, l, "saver", 1_000_000)
	orig := draftPayoutWithPurpose(t, l, "po-1", 100_000, "SALA")

	// Amending to the current purpose code is a no-op, not a change.
	if got, err := l.AmendPayoutPurposeCode("po-1", "SALA"); err != nil || !reflect.DeepEqual(got, orig) {
		t.Errorf("same-code amendment = %+v, %v, want %+v, nil", got, err, orig)
	}

	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// The no-op writes no audit event.
	if events := auditEventsByOp(t, dir)["payout_purpose_code_amended"]; len(events) != 0 {
		t.Errorf("amend events = %d, want 0 (no-op is silent)", len(events))
	}
}

func TestAmendPayoutPurposeCodeSuccess(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithPayoutClearingAccount("payout-clearing"))
	seedBalance(t, l, "saver", 1_000_000)
	orig := draftPayoutWithPurpose(t, l, "po-1", 100_000, "SALA")
	versionBefore := l.Version()

	amended, err := l.AmendPayoutPurposeCode("po-1", "SUPP")
	if err != nil {
		t.Fatalf("AmendPayoutPurposeCode: %v", err)
	}
	if amended.PurposeCode != "SUPP" {
		t.Errorf("amended purpose = %q, want SUPP", amended.PurposeCode)
	}
	// Only the purpose code moves: amount, currency, destination,
	// payee account, maker, status, UETR, and idempotency key are
	// untouched.
	amended.PurposeCode = orig.PurposeCode
	if !reflect.DeepEqual(amended, orig) {
		t.Errorf("amended payout = %+v, want %+v except purpose code", amended, orig)
	}
	// Off-journal: the ledger version does not move.
	if got := l.Version(); got != versionBefore {
		t.Errorf("version after amendment = %d, want %d (unchanged)", got, versionBefore)
	}
	// The registry carries the new code, still a draft.
	stored, ok := l.GetPayout("po-1")
	if !ok || stored.PurposeCode != "SUPP" || stored.Status != PayoutDraft {
		t.Errorf("stored payout = %+v,%v, want draft with purpose SUPP", stored, ok)
	}
	// The amendment emits no outbox event: the draft was already
	// reported when created, and the amend path never calls
	// emitOutboxLocked (verified by inspection of the module).

	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["payout_purpose_code_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.TraceID != "po-1" || ev.Actor != "AmendPayoutPurposeCode" {
		t.Errorf("amend event = trace %q actor %q, want po-1/AmendPayoutPurposeCode", ev.TraceID, ev.Actor)
	}
	if ev.Details["previous_purpose_code"] != "SALA" || ev.Details["purpose_code"] != "SUPP" {
		t.Errorf("amend event details = %v, want previous SALA / new SUPP", ev.Details)
	}
	if len(ev.Accounts) != 1 || ev.Accounts[0] != "saver" {
		t.Errorf("amend event accounts = %v, want [saver]", ev.Accounts)
	}
	if ev.VersionBefore != versionBefore || ev.VersionAfter != versionBefore {
		t.Errorf("amend event versions = %d->%d, want %d->%d", ev.VersionBefore, ev.VersionAfter, versionBefore, versionBefore)
	}
}

func TestAmendPayoutPurposeCodeClear(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithPayoutClearingAccount("payout-clearing"))
	seedBalance(t, l, "saver", 1_000_000)
	draftPayoutWithPurpose(t, l, "po-1", 100_000, "SALA")

	// An empty code clears the purpose: "no purpose stated".
	amended, err := l.AmendPayoutPurposeCode("po-1", "")
	if err != nil {
		t.Fatalf("AmendPayoutPurposeCode clear: %v", err)
	}
	if amended.PurposeCode != "" {
		t.Errorf("amended purpose = %q, want empty (cleared)", amended.PurposeCode)
	}
	if p, _ := l.GetPayout("po-1"); p.PurposeCode != "" {
		t.Errorf("stored purpose = %q, want empty (cleared)", p.PurposeCode)
	}

	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["payout_purpose_code_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
	if events[0].Details["previous_purpose_code"] != "SALA" || events[0].Details["purpose_code"] != "" {
		t.Errorf("clear event details = %v, want previous SALA / new empty", events[0].Details)
	}
}

func TestAmendPayoutPurposeCodeExportReadsLiveCode(t *testing.T) {
	l := seedPayoutLedger(t)
	draftPayoutWithPurpose(t, l, "po-1", 100_000, "SALA")

	// The export stamps the purpose live at export time, so a
	// revised code governs the next export with no export-path
	// change — and clearing it drops the Purp element.
	if _, err := l.AmendPayoutPurposeCode("po-1", "SUPP"); err != nil {
		t.Fatalf("AmendPayoutPurposeCode: %v", err)
	}
	doc, err := l.ExportPacs008("po-1")
	if err != nil {
		t.Fatalf("ExportPacs008: %v", err)
	}
	if !strings.Contains(string(doc), "<Cd>SUPP</Cd>") {
		t.Errorf("export after amendment lacks <Cd>SUPP</Cd>:\n%s", doc)
	}
	if strings.Contains(string(doc), "SALA") {
		t.Errorf("export after amendment still carries the old SALA code:\n%s", doc)
	}

	if _, err := l.AmendPayoutPurposeCode("po-1", ""); err != nil {
		t.Fatalf("AmendPayoutPurposeCode clear: %v", err)
	}
	doc, err = l.ExportPacs008("po-1")
	if err != nil {
		t.Fatalf("ExportPacs008 after clear: %v", err)
	}
	if strings.Contains(string(doc), "<Purp>") {
		t.Errorf("export after clear still carries a Purp element:\n%s", doc)
	}
}
