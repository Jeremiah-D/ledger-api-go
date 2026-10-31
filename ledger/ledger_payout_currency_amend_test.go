package ledger

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestAmendPayoutCurrencyValidation(t *testing.T) {
	l := seedPayoutLedger(t)
	orig := draftPayout(t, l, "po-1", 100_000)
	if orig.Currency != "USD" {
		t.Fatalf("draft currency = %q, want USD default", orig.Currency)
	}

	// Unknown ID.
	if _, err := l.AmendPayoutCurrency("nope", "EUR"); !errors.Is(err, ErrPayoutNotFound) {
		t.Errorf("unknown: err = %v, want ErrPayoutNotFound", err)
	}

	// The new currency must be exactly three ASCII uppercase letters
	// (ISO 4217 alpha-3, the CreatePayout / LG-276 gate): lowercase,
	// 2-letter, 4-letter, empty, and non-ASCII codes are rejected,
	// and the original currency is untouched.
	for _, bad := range []string{"eur", "EU", "EURR", "", "€UR", "U$D", "E1R"} {
		if _, err := l.AmendPayoutCurrency("po-1", bad); !errors.Is(err, ErrInvalidCurrency) {
			t.Errorf("currency %q: err = %v, want ErrInvalidCurrency", bad, err)
		}
		if got, _ := l.GetPayout("po-1"); !reflect.DeepEqual(got, orig) {
			t.Errorf("payout after currency %q = %+v, want %+v (untouched)", bad, got, orig)
		}
	}
}

func TestAmendPayoutCurrencyStateGate(t *testing.T) {
	// Every non-draft status is rejected: approved, submitted,
	// cancelled, and expired.
	t.Run("approved", func(t *testing.T) {
		l := seedPayoutLedger(t)
		draftPayout(t, l, "po-1", 100_000)
		if _, err := l.ApprovePayout("po-1", "bob"); err != nil {
			t.Fatalf("ApprovePayout: %v", err)
		}
		if _, err := l.AmendPayoutCurrency("po-1", "EUR"); !errors.Is(err, ErrPayoutInvalidTransition) {
			t.Errorf("err = %v, want ErrPayoutInvalidTransition", err)
		}
		if p, _ := l.GetPayout("po-1"); p.Currency != "USD" {
			t.Errorf("currency moved to %q after rejected amendment", p.Currency)
		}
	})
	t.Run("submitted", func(t *testing.T) {
		l := seedPayoutLedger(t)
		draftPayout(t, l, "po-1", 100_000)
		approveSubmitPayout(t, l, "po-1", "bob")
		if _, err := l.AmendPayoutCurrency("po-1", "EUR"); !errors.Is(err, ErrPayoutInvalidTransition) {
			t.Errorf("err = %v, want ErrPayoutInvalidTransition", err)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		l := seedPayoutLedger(t)
		draftPayout(t, l, "po-1", 100_000)
		if _, err := l.CancelPayout("po-1"); err != nil {
			t.Fatalf("CancelPayout: %v", err)
		}
		if _, err := l.AmendPayoutCurrency("po-1", "EUR"); !errors.Is(err, ErrPayoutInvalidTransition) {
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
		if _, err := l.AmendPayoutCurrency("po-1", "EUR"); !errors.Is(err, ErrPayoutInvalidTransition) {
			t.Errorf("err = %v, want ErrPayoutInvalidTransition", err)
		}
	})

	// The state gate runs before the currency validation and the
	// no-op check: even a same-value or malformed amendment is
	// rejected on a departed instruction.
	l := seedPayoutLedger(t)
	draftPayout(t, l, "po-1", 100_000)
	if _, err := l.ApprovePayout("po-1", "bob"); err != nil {
		t.Fatalf("ApprovePayout: %v", err)
	}
	if _, err := l.AmendPayoutCurrency("po-1", "USD"); !errors.Is(err, ErrPayoutInvalidTransition) {
		t.Errorf("approved, same currency: err = %v, want ErrPayoutInvalidTransition", err)
	}
	if _, err := l.AmendPayoutCurrency("po-1", "eur"); !errors.Is(err, ErrPayoutInvalidTransition) {
		t.Errorf("approved, lowercase currency: err = %v, want ErrPayoutInvalidTransition", err)
	}
}

func TestAmendPayoutCurrencyNoOp(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithPayoutClearingAccount("payout-clearing"))
	seedBalance(t, l, "saver", 1_000_000)
	orig := draftPayout(t, l, "po-1", 100_000)

	// Amending to the current currency is a no-op, not a change.
	if got, err := l.AmendPayoutCurrency("po-1", "USD"); err != nil || !reflect.DeepEqual(got, orig) {
		t.Errorf("same-currency amendment = %+v, %v, want %+v, nil", got, err, orig)
	}

	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// The no-op writes no audit event.
	if events := auditEventsByOp(t, dir)["payout_currency_amended"]; len(events) != 0 {
		t.Errorf("amend events = %d, want 0 (no-op is silent)", len(events))
	}
}

func TestAmendPayoutCurrencySuccess(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al), WithPayoutClearingAccount("payout-clearing"))
	seedBalance(t, l, "saver", 1_000_000)
	orig := draftPayout(t, l, "po-1", 100_000)
	versionBefore := l.Version()

	amended, err := l.AmendPayoutCurrency("po-1", "EUR")
	if err != nil {
		t.Fatalf("AmendPayoutCurrency: %v", err)
	}
	if amended.Currency != "EUR" {
		t.Errorf("amended currency = %q, want EUR", amended.Currency)
	}
	// Only the currency moves: payee, amount, destination, maker,
	// status, UETR, and purpose code are untouched.
	amended.Currency = orig.Currency
	if !reflect.DeepEqual(amended, orig) {
		t.Errorf("amended payout = %+v, want %+v except currency", amended, orig)
	}
	// Off-journal: the ledger version does not move.
	if got := l.Version(); got != versionBefore {
		t.Errorf("version after amendment = %d, want %d (unchanged)", got, versionBefore)
	}
	// The registry carries the new currency, still a draft.
	stored, ok := l.GetPayout("po-1")
	if !ok || stored.Currency != "EUR" || stored.Status != PayoutDraft {
		t.Errorf("stored payout = %+v,%v, want draft with currency EUR", stored, ok)
	}
	// The amendment emits no outbox event: the draft was already
	// reported when created, and the amend path never calls
	// emitOutboxLocked (verified by inspection of the module).

	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["payout_currency_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.TraceID != "po-1" || ev.Actor != "AmendPayoutCurrency" {
		t.Errorf("amend event = trace %q actor %q, want po-1/AmendPayoutCurrency", ev.TraceID, ev.Actor)
	}
	if ev.Details["previous_currency"] != "USD" || ev.Details["currency"] != "EUR" {
		t.Errorf("amend event details = %v, want previous USD / new EUR", ev.Details)
	}
	if ev.VersionBefore != versionBefore || ev.VersionAfter != versionBefore {
		t.Errorf("amend event versions = %d->%d, want %d->%d", ev.VersionBefore, ev.VersionAfter, versionBefore, versionBefore)
	}
}

func TestAmendPayoutCurrencyEffectiveAtSubmit(t *testing.T) {
	// SubmitPayout reads the live currency when it journals the
	// clearing leg: a draft amended USD -> EUR submits in EUR, and
	// the clearing entry is booked in EUR.
	l := seedPayoutLedger(t)
	draftPayout(t, l, "po-1", 100_000)
	if _, err := l.AmendPayoutCurrency("po-1", "EUR"); err != nil {
		t.Fatalf("AmendPayoutCurrency: %v", err)
	}
	if _, err := l.ApprovePayout("po-1", "bob"); err != nil {
		t.Fatalf("ApprovePayout: %v", err)
	}
	submitted, err := l.SubmitPayout("po-1")
	if err != nil {
		t.Fatalf("SubmitPayout: %v", err)
	}
	if submitted.Currency != "EUR" {
		t.Errorf("submitted currency = %q, want EUR", submitted.Currency)
	}
	var clearing *JournalEntry
	for _, e := range l.Entries() {
		if e.ID == submitted.ClearingEntryID {
			e := e
			clearing = &e
		}
	}
	if clearing == nil {
		t.Fatalf("clearing entry %q not found", submitted.ClearingEntryID)
	}
	if clearing.Currency != "EUR" {
		t.Errorf("clearing entry currency = %q, want EUR", clearing.Currency)
	}
	if clearing.AmountCents != 100_000 {
		t.Errorf("clearing entry amount = %d, want 100000 (amount untouched)", clearing.AmountCents)
	}
}
