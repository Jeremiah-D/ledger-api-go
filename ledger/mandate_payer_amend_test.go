package ledger

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestAmendMandatePayerValidation(t *testing.T) {
	if _, err := New().AmendMandatePayer("m-unknown", "cust-2"); !errors.Is(err, ErrMandateNotFound) {
		t.Errorf("unknown mandate: err = %v, want ErrMandateNotFound", err)
	}
	l, m := mandateFixture(t)
	// Empty payer and payer equal to the payee reuse the creation
	// gates — an amendment can never install a payer the mandate
	// could not have been created with.
	if _, err := l.AmendMandatePayer("m-sub", ""); !errors.Is(err, ErrEmptyMandatePayer) {
		t.Errorf("empty payer: err = %v, want ErrEmptyMandatePayer", err)
	}
	if _, err := l.AmendMandatePayer("m-sub", "merchant"); !errors.Is(err, ErrMandateSameParty) {
		t.Errorf("payer equal to payee: err = %v, want ErrMandateSameParty", err)
	}
	// Every rejected amendment leaves the mandate exactly as it was.
	if got, ok := l.GetMandate("m-sub"); !ok || got != m {
		t.Errorf("mandate after rejected amendments = %+v ok=%v, want %+v", got, ok, m)
	}
	// A legal amendment changes the payer and nothing else.
	amended, err := l.AmendMandatePayer("m-sub", "cust-2")
	if err != nil {
		t.Fatalf("AmendMandatePayer: %v", err)
	}
	if amended.Payer != "cust-2" {
		t.Errorf("amended payer = %q, want cust-2", amended.Payer)
	}
	amended.Payer = m.Payer
	if amended != m {
		t.Errorf("amended mandate = %+v, want %+v except the payer", amended, m)
	}
	// Amending to the current payer is a no-op.
	if same, err := l.AmendMandatePayer("m-sub", "cust-2"); err != nil {
		t.Fatalf("AmendMandatePayer same payer: %v", err)
	} else if same.Payer != "cust-2" {
		t.Errorf("same-payer amend = %+v, want cust-2", same)
	}
}

func TestAmendMandatePayerTerminalStates(t *testing.T) {
	// Suspended.
	l, _ := mandateFixture(t)
	if _, err := l.SuspendMandate("m-sub"); err != nil {
		t.Fatalf("SuspendMandate: %v", err)
	}
	if _, err := l.AmendMandatePayer("m-sub", "cust-2"); !errors.Is(err, ErrMandateNotActive) {
		t.Errorf("suspended mandate: err = %v, want ErrMandateNotActive", err)
	}
	// Cancelled.
	l2, _ := mandateFixture(t)
	if _, err := l2.CancelMandate("m-sub"); err != nil {
		t.Fatalf("CancelMandate: %v", err)
	}
	if _, err := l2.AmendMandatePayer("m-sub", "cust-2"); !errors.Is(err, ErrMandateNotActive) {
		t.Errorf("cancelled mandate: err = %v, want ErrMandateNotActive", err)
	}
	// Swept-expired: the stored status is "expired" (the validity
	// end itself is still in the future, so the lazy-expiry
	// predicate does not fire) — the LG-243 reading, not a lazy
	// expiry.
	l3, _ := mandateFixture(t)
	if got := l3.ExpireMandatesAt(time.Now().Add(31 * 24 * time.Hour)); got != 1 {
		t.Fatalf("sweep expired = %d, want 1", got)
	}
	if _, err := l3.AmendMandatePayer("m-sub", "cust-2"); !errors.Is(err, ErrMandateNotActive) {
		t.Errorf("swept-expired mandate: err = %v, want ErrMandateNotActive", err)
	}
	// Lapsed but unswept: still expired, never resurrected.
	l4 := New()
	if _, _, err := l4.Post(JournalEntry{ID: "fund-cust", DebitAccount: "cust", CreditAccount: "bank", AmountCents: 100000, Currency: "USD"}); err != nil {
		t.Fatalf("fund Post: %v", err)
	}
	now := time.Now()
	if _, err := l4.CreateMandate(Mandate{
		ID:             "m-old",
		Payer:          "cust",
		Payee:          "merchant",
		MaxAmountCents: 5000,
		Currency:       "USD",
		Frequency:      MandateFrequencyPerDay,
		ValidFrom:      now.Add(-48 * time.Hour),
		ValidUntil:     now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("CreateMandate lapsed: %v", err)
	}
	if _, err := l4.AmendMandatePayer("m-old", "cust-2"); !errors.Is(err, ErrMandateExpired) {
		t.Errorf("lapsed mandate: err = %v, want ErrMandateExpired", err)
	}
}

func TestAmendMandatePayerDebitFollowsNewPayer(t *testing.T) {
	l, _ := mandateFixture(t)
	// One debit under the original payer settles from cust.
	before, err := l.DebitWithMandate("m-sub", MandateDebit{ID: "d-1", AmountCents: 1200, Memo: "subscription"})
	if err != nil {
		t.Fatalf("DebitWithMandate before amend: %v", err)
	}
	if before.Entry.DebitAccount != "merchant" || before.Entry.CreditAccount != "cust" {
		t.Errorf("debit before amend legs = %q/%q, want merchant/cust", before.Entry.DebitAccount, before.Entry.CreditAccount)
	}
	// Amend the payer; the settled debit is untouched, the next pull
	// settles from the new payer. The new payer is funded first — a
	// pull credits the payer, so it needs a balance — and the second
	// debit is booked next day so the per-day frequency window is
	// free.
	versionBefore := l.Version()
	if _, err := l.AmendMandatePayer("m-sub", "cust-2"); err != nil {
		t.Fatalf("AmendMandatePayer: %v", err)
	}
	if l.Version() != versionBefore {
		t.Errorf("version moved: amendment is off-journal")
	}
	if _, _, err := l.Post(JournalEntry{ID: "fund-cust-2", DebitAccount: "cust-2", CreditAccount: "bank", AmountCents: 100000, Currency: "USD"}); err != nil {
		t.Fatalf("fund new payer Post: %v", err)
	}
	after, err := l.DebitWithMandate("m-sub", MandateDebit{ID: "d-2", AmountCents: 1200, Memo: "subscription", CreatedAt: time.Now().Add(24 * time.Hour)})
	if err != nil {
		t.Fatalf("DebitWithMandate after amend: %v", err)
	}
	if after.Entry.DebitAccount != "merchant" || after.Entry.CreditAccount != "cust-2" {
		t.Errorf("debit after amend legs = %q/%q, want merchant/cust-2", after.Entry.DebitAccount, after.Entry.CreditAccount)
	}
	if after.Entry.AmountCents != 1200 || after.Entry.Currency != "USD" {
		t.Errorf("debit after amend = %+v, want 1200 USD", after.Entry)
	}
}

func TestAmendMandatePayerAuditAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	if _, _, err := l.Post(JournalEntry{ID: "fund-cust", DebitAccount: "cust", CreditAccount: "bank", AmountCents: 100000, Currency: "USD"}); err != nil {
		t.Fatalf("fund Post: %v", err)
	}
	now := time.Now()
	if _, err := l.CreateMandate(Mandate{
		ID:             "m-sub",
		Payer:          "cust",
		Payee:          "merchant",
		MaxAmountCents: 5000,
		Currency:       "USD",
		Frequency:      MandateFrequencyPerDay,
		ValidFrom:      now.Add(-time.Hour),
		ValidUntil:     now.Add(30 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("CreateMandate: %v", err)
	}
	ctx := ContextWithActor(context.Background(), "ops-desk")
	if _, err := l.AmendMandatePayerCtx(ctx, "m-sub", "cust-2"); err != nil {
		t.Fatalf("AmendMandatePayer: %v", err)
	}
	// The same-value no-op writes no second event.
	if _, err := l.AmendMandatePayerCtx(ctx, "m-sub", "cust-2"); err != nil {
		t.Fatalf("AmendMandatePayer same payer: %v", err)
	}
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	after, ok := restored.GetMandate("m-sub")
	if !ok {
		t.Fatal("restored mandate missing")
	}
	if after.Payer != "cust-2" || after.Payee != "merchant" || after.Status != MandateStatusActive {
		t.Errorf("restored mandate = %+v, want payer cust-2, payee merchant, active", after)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["mandate_payer_amended"]
	if len(events) != 1 {
		t.Fatalf("amend events = %d, want 1 (same-value no-op writes none)", len(events))
	}
	if events[0].Actor != "ops-desk" {
		t.Errorf("event actor = %q, want ops-desk", events[0].Actor)
	}
	d := events[0].Details
	if d["mandate_id"] != "m-sub" || d["previous_payer"] != "cust" ||
		d["payer"] != "cust-2" || d["currency"] != "USD" {
		t.Errorf("event details = %+v, want m-sub cust->cust-2 USD", d)
	}
}
