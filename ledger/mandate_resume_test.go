package ledger

import (
	"errors"
	"testing"
	"time"
)

// TestResumeMandate covers the happy path: a suspended mandate resumes
// to active, audited as mandate_resumed with the currency in details,
// and announced on the outbox as mandate_resumed. The transition is
// structural — the journal version does not move.
func TestResumeMandate(t *testing.T) {
	al := mustAuditLog(t)
	dir := t.TempDir()
	ob, err := NewOutbox(dir)
	if err != nil {
		t.Fatalf("NewOutbox: %v", err)
	}
	t.Cleanup(func() { _ = ob.Close() })
	l := New(WithAuditLog(al), WithOutbox(ob))
	if _, _, err := l.Post(JournalEntry{ID: "fund-cust", DebitAccount: "cust", CreditAccount: "bank", AmountCents: 100000, Currency: "USD"}); err != nil {
		t.Fatalf("fund Post: %v", err)
	}
	now := time.Now()
	if _, err := l.CreateMandate(Mandate{
		ID: "m-sub", Payer: "cust", Payee: "merchant",
		MaxAmountCents: 5000, Currency: "USD", Frequency: MandateFrequencyPerDay,
		ValidFrom: now.Add(-time.Hour), ValidUntil: now.Add(30 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("CreateMandate: %v", err)
	}
	if _, err := l.SuspendMandate("m-sub"); err != nil {
		t.Fatalf("SuspendMandate: %v", err)
	}
	before := l.Version()
	m, err := l.ResumeMandate("m-sub")
	if err != nil {
		t.Fatalf("ResumeMandate: %v", err)
	}
	if m.Status != MandateStatusActive {
		t.Errorf("status = %q, want active", m.Status)
	}
	if l.Version() != before {
		t.Errorf("version = %d, want %d (resume is structural)", l.Version(), before)
	}
	got, ok := l.GetMandate("m-sub")
	if !ok || got.Status != MandateStatusActive {
		t.Errorf("GetMandate status = %q, want active", got.Status)
	}

	events := readAuditOps(t, al)
	if countOp(events, "mandate_resumed") != 1 {
		t.Errorf("mandate_resumed count = %d, want 1", countOp(events, "mandate_resumed"))
	}
	resumed := findOp(events, "mandate_resumed")
	if resumed == nil {
		t.Fatal("mandate_resumed event missing")
	}
	if resumed.Actor != "ResumeMandate" || resumed.TraceID != "m-sub" {
		t.Errorf("resumed actor/trace = %q/%q, want ResumeMandate/m-sub", resumed.Actor, resumed.TraceID)
	}
	if resumed.Details["currency"] != "USD" {
		t.Errorf("resumed details currency = %v, want USD", resumed.Details["currency"])
	}

	evs := readAllOutbox(t, dir)
	var out *OutboxEvent
	for i, e := range evs {
		if e.Type == OutboxMandateResumed {
			out = &evs[i]
		}
	}
	if out == nil {
		t.Fatalf("mandate_resumed outbox event missing (types %v)", eventTypes(evs))
	}
	if out.Account != "cust" || out.Currency != "USD" {
		t.Errorf("outbox account/currency = %q/%q, want cust/USD", out.Account, out.Currency)
	}
	if out.Details["mandate_id"] != "m-sub" {
		t.Errorf("outbox mandate_id = %v, want m-sub", out.Details["mandate_id"])
	}
}

// TestResumeMandateRejects covers every non-resumable state: active,
// cancelled, and unknown mandates, plus a suspended mandate whose
// validity window has already passed — the window is dead, so the
// resume is refused with ErrMandateExpired and the row stays suspended.
func TestResumeMandateRejects(t *testing.T) {
	t.Run("active", func(t *testing.T) {
		l, _ := mandateFixture(t)
		if _, err := l.ResumeMandate("m-sub"); !errors.Is(err, ErrMandateNotActive) {
			t.Errorf("resume active err = %v, want ErrMandateNotActive", err)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		l, _ := mandateFixture(t)
		if _, err := l.CancelMandate("m-sub"); err != nil {
			t.Fatalf("CancelMandate: %v", err)
		}
		if _, err := l.ResumeMandate("m-sub"); !errors.Is(err, ErrMandateNotActive) {
			t.Errorf("resume cancelled err = %v, want ErrMandateNotActive", err)
		}
	})
	t.Run("unknown", func(t *testing.T) {
		l, _ := mandateFixture(t)
		if _, err := l.ResumeMandate("nope"); !errors.Is(err, ErrMandateNotFound) {
			t.Errorf("resume unknown err = %v, want ErrMandateNotFound", err)
		}
	})
	t.Run("expired window", func(t *testing.T) {
		l, _ := mandateFixture(t)
		if _, err := l.SuspendMandate("m-sub"); err != nil {
			t.Fatalf("SuspendMandate: %v", err)
		}
		// Age the window past its end in place: the row is stored
		// suspended, so neither the lazy-expiry predicate nor the
		// sweep sees it — only ResumeMandate's explicit gate does.
		m := l.mandates["m-sub"]
		m.ValidUntil = time.Now().Add(-time.Hour)
		l.mandates["m-sub"] = m
		if _, err := l.ResumeMandate("m-sub"); !errors.Is(err, ErrMandateExpired) {
			t.Errorf("resume expired-window err = %v, want ErrMandateExpired", err)
		}
		if l.mandates["m-sub"].Status != MandateStatusSuspended {
			t.Errorf("status after refused resume = %q, want suspended", l.mandates["m-sub"].Status)
		}
	})
}

// TestResumeMandateReenablesDebit closes the suspend-resume loop: a
// debit is refused while suspended and posts again after the resume.
func TestResumeMandateReenablesDebit(t *testing.T) {
	l, _ := mandateFixture(t)
	if _, err := l.SuspendMandate("m-sub"); err != nil {
		t.Fatalf("SuspendMandate: %v", err)
	}
	if _, err := l.DebitWithMandate("m-sub", MandateDebit{ID: "d-held", AmountCents: 100}); !errors.Is(err, ErrMandateNotActive) {
		t.Errorf("suspended debit err = %v, want ErrMandateNotActive", err)
	}
	if _, err := l.ResumeMandate("m-sub"); err != nil {
		t.Fatalf("ResumeMandate: %v", err)
	}
	receipt, err := l.DebitWithMandate("m-sub", MandateDebit{ID: "d-1", AmountCents: 1200, Memo: "subscription"})
	if err != nil {
		t.Fatalf("DebitWithMandate after resume: %v", err)
	}
	if receipt.Entry.AmountCents != 1200 {
		t.Errorf("debit amount = %d, want 1200", receipt.Entry.AmountCents)
	}
}

// TestResumeMandateReSuspend shows resume is not a one-way door: a
// resumed mandate can be suspended again.
func TestResumeMandateReSuspend(t *testing.T) {
	l, _ := mandateFixture(t)
	if _, err := l.SuspendMandate("m-sub"); err != nil {
		t.Fatalf("SuspendMandate: %v", err)
	}
	if _, err := l.ResumeMandate("m-sub"); err != nil {
		t.Fatalf("ResumeMandate: %v", err)
	}
	m, err := l.SuspendMandate("m-sub")
	if err != nil {
		t.Fatalf("re-suspend: %v", err)
	}
	if m.Status != MandateStatusSuspended {
		t.Errorf("status = %q, want suspended", m.Status)
	}
}

// TestExpireMandatesLeavesSuspended pins the existing sweep behavior
// the resume gate relies on: ExpireMandates never touches suspended
// rows, even one whose ValidUntil has passed — the row stays suspended
// and ResumeMandate is the backstop that refuses it as expired.
func TestExpireMandatesLeavesSuspended(t *testing.T) {
	l, _ := mandateFixture(t)
	if _, err := l.SuspendMandate("m-sub"); err != nil {
		t.Fatalf("SuspendMandate: %v", err)
	}
	m := l.mandates["m-sub"]
	m.ValidUntil = time.Now().Add(-time.Hour)
	l.mandates["m-sub"] = m
	if n := l.ExpireMandates(); n != 0 {
		t.Errorf("ExpireMandates = %d, want 0 (suspended rows untouched)", n)
	}
	if l.mandates["m-sub"].Status != MandateStatusSuspended {
		t.Errorf("status after sweep = %q, want suspended", l.mandates["m-sub"].Status)
	}
	if _, err := l.ResumeMandate("m-sub"); !errors.Is(err, ErrMandateExpired) {
		t.Errorf("resume err = %v, want ErrMandateExpired", err)
	}
}
