package ledger

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestAmendInvoiceCurrencyValidation(t *testing.T) {
	l := New()
	issuedAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	due := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	orig := issueTestInvoice(t, l, "inv-1", "acme", 5000, issuedAt, due)

	// Unknown ID.
	if _, err := l.AmendInvoiceCurrency("nope", "EUR"); !errors.Is(err, ErrInvoiceNotFound) {
		t.Errorf("unknown invoice: err = %v, want ErrInvoiceNotFound", err)
	}

	// The new currency must be exactly three ASCII uppercase letters
	// (ISO 4217 alpha-3): lowercase, two letters, four letters, a
	// digit, and the empty string are all rejected, and the original
	// currency is untouched.
	for _, bad := range []string{"eur", "EU", "EURO", "US1", "", "U$D"} {
		if _, err := l.AmendInvoiceCurrency("inv-1", bad); !errors.Is(err, ErrInvalidCurrency) {
			t.Errorf("currency %q: err = %v, want ErrInvalidCurrency", bad, err)
		}
		got, _ := l.GetInvoice("inv-1")
		if !reflect.DeepEqual(got, orig) {
			t.Errorf("invoice after bad currency %q = %+v, want %+v (untouched)", bad, got, orig)
		}
	}
}

func TestAmendInvoiceCurrencyStateGate(t *testing.T) {
	l := New()
	issuedAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	due := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	asof := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)

	// Paid: the money moved, so the record must stay as-is — the
	// same rejection VoidInvoice returns.
	issueTestInvoice(t, l, "inv-paid", "acme", 1000, issuedAt, due)
	if _, _, err := l.ApplyPayment("acme", "USD", 1000, issuedAt); err != nil {
		t.Fatalf("ApplyPayment: %v", err)
	}
	if got, _ := l.GetInvoice("inv-paid"); got.Status != InvoicePaid {
		t.Fatalf("inv-paid status = %v, want paid", got.Status)
	}
	if _, err := l.AmendInvoiceCurrency("inv-paid", "EUR"); !errors.Is(err, ErrInvoiceHasPayments) {
		t.Errorf("paid invoice: err = %v, want ErrInvoiceHasPayments", err)
	}
	// The state gate runs before the no-op check: even a same-value
	// amendment is rejected on a terminal invoice.
	if _, err := l.AmendInvoiceCurrency("inv-paid", "USD"); !errors.Is(err, ErrInvoiceHasPayments) {
		t.Errorf("paid invoice, same currency: err = %v, want ErrInvoiceHasPayments", err)
	}

	// Voided: closed, so its currency cannot move.
	issueTestInvoice(t, l, "inv-void", "acme", 1000, issuedAt, due)
	if err := l.VoidInvoice("inv-void"); err != nil {
		t.Fatalf("VoidInvoice: %v", err)
	}
	if _, err := l.AmendInvoiceCurrency("inv-void", "EUR"); !errors.Is(err, ErrInvoiceNotOpen) {
		t.Errorf("voided invoice: err = %v, want ErrInvoiceNotOpen", err)
	}
	if _, err := l.AmendInvoiceCurrency("inv-void", "USD"); !errors.Is(err, ErrInvoiceNotOpen) {
		t.Errorf("voided invoice, same currency: err = %v, want ErrInvoiceNotOpen", err)
	}

	// Partially paid is still open and amendable; the paid cents
	// stay applied and the partial status stays.
	issueTestInvoice(t, l, "inv-part", "acme", 2000, issuedAt, due)
	if _, _, err := l.ApplyPayment("acme", "USD", 500, issuedAt); err != nil {
		t.Fatalf("ApplyPayment: %v", err)
	}
	amended, err := l.AmendInvoiceCurrency("inv-part", "EUR")
	if err != nil {
		t.Fatalf("partial invoice: err = %v, want nil", err)
	}
	if amended.Status != InvoicePartial || amended.Currency != "EUR" || amended.PaidCents != 500 || amended.AmountCents != 2000 {
		t.Errorf("amended partial = %+v, want partial/EUR/paid 500/amount 2000", amended)
	}

	// Overdue is still open and amendable.
	issueTestInvoice(t, l, "inv-overdue", "acme", 3000, issuedAt, due)
	swept := l.SweepOverdueInvoices(asof)
	found := false
	for _, id := range swept {
		if id == "inv-overdue" {
			found = true
		}
	}
	if !found {
		t.Fatalf("sweep = %v, want it to contain inv-overdue", swept)
	}
	amended, err = l.AmendInvoiceCurrency("inv-overdue", "EUR")
	if err != nil {
		t.Fatalf("overdue invoice: err = %v, want nil", err)
	}
	if amended.Status != InvoiceOverdue || amended.Currency != "EUR" {
		t.Errorf("amended overdue = %+v, want overdue/EUR", amended)
	}
}

func TestAmendInvoiceCurrencyNoOp(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	issuedAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	due := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	orig := issueTestInvoice(t, l, "inv-1", "acme", 5000, issuedAt, due)

	// Amending to the current currency is a no-op, not a change.
	if got, err := l.AmendInvoiceCurrency("inv-1", "USD"); err != nil || !reflect.DeepEqual(got, orig) {
		t.Errorf("same-currency amendment = %+v, %v, want %+v, nil", got, err, orig)
	}

	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// The no-op writes no audit event (the issue event is the only
	// invoice_currency_amended-free trail here).
	if events := auditEventsByOp(t, dir)["invoice_currency_amended"]; len(events) != 0 {
		t.Errorf("amend events = %d, want 0 (no-op is silent)", len(events))
	}
}

func TestAmendInvoiceCurrencySuccess(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	issuedAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	due := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	orig := issueTestInvoice(t, l, "inv-1", "acme", 5000, issuedAt, due)
	versionBefore := l.Version()

	amended, err := l.AmendInvoiceCurrency("inv-1", "EUR")
	if err != nil {
		t.Fatalf("AmendInvoiceCurrency: %v", err)
	}
	if amended.Currency != "EUR" {
		t.Errorf("amended currency = %q, want EUR", amended.Currency)
	}
	// Only the currency moves: amount, paid amount, status, due date,
	// memo, and account are untouched.
	amended.Currency = orig.Currency
	if !reflect.DeepEqual(amended, orig) {
		t.Errorf("amended invoice = %+v, want %+v except currency", amended, orig)
	}
	// Off-journal: the ledger version does not move.
	if got := l.Version(); got != versionBefore {
		t.Errorf("version after amendment = %d, want %d (unchanged)", got, versionBefore)
	}

	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["invoice_currency_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.TraceID != "inv-1" || ev.Actor != "AmendInvoiceCurrency" {
		t.Errorf("amend event = trace %q actor %q, want inv-1/AmendInvoiceCurrency", ev.TraceID, ev.Actor)
	}
	if ev.Details["previous_currency"] != "USD" || ev.Details["currency"] != "EUR" {
		t.Errorf("amend event details = %v, want previous USD / new EUR", ev.Details)
	}
	if ev.VersionBefore != versionBefore || ev.VersionAfter != versionBefore {
		t.Errorf("amend event versions = %d->%d, want %d->%d", ev.VersionBefore, ev.VersionAfter, versionBefore, versionBefore)
	}
}

func TestAmendInvoiceCurrencyLiveReads(t *testing.T) {
	l := New()
	issuedAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	due := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	issueTestInvoice(t, l, "inv-1", "acme", 5000, issuedAt, due)
	if _, err := l.AmendInvoiceCurrency("inv-1", "EUR"); err != nil {
		t.Fatalf("AmendInvoiceCurrency: %v", err)
	}

	// ListInvoices filters by the new currency from the next read.
	if got := l.ListInvoices("acme", "EUR"); len(got) != 1 || got[0].ID != "inv-1" {
		t.Errorf("ListInvoices(acme, EUR) = %v, want [inv-1]", got)
	}
	if got := l.ListInvoices("acme", "USD"); len(got) != 0 {
		t.Errorf("ListInvoices(acme, USD) = %v, want [] (invoice moved to EUR)", got)
	}

	// ApplyPayment follows the new currency: a USD payment no longer
	// sees the invoice (full remainder), a EUR payment retires it.
	if _, rem, err := l.ApplyPayment("acme", "USD", 5000, issuedAt); err != nil || rem != 5000 {
		t.Fatalf("USD ApplyPayment: remainder = %d, err = %v, want 5000, nil", rem, err)
	}
	applied, rem, err := l.ApplyPayment("acme", "EUR", 5000, issuedAt)
	if err != nil {
		t.Fatalf("EUR ApplyPayment: %v", err)
	}
	if rem != 0 || len(applied) != 1 || applied[0].InvoiceID != "inv-1" || applied[0].StatusAfter != InvoicePaid {
		t.Fatalf("EUR ApplyPayment: applied = %+v remainder = %d, want [inv-1 paid] remainder 0", applied, rem)
	}
	// The amount and paid cents are unchanged by the re-denomination:
	// the invoice was 5000 cents and is now fully paid in EUR.
	if got, _ := l.GetInvoice("inv-1"); got.AmountCents != 5000 || got.PaidCents != 5000 {
		t.Errorf("invoice after EUR payment = %+v, want amount 5000 / paid 5000", got)
	}
}
