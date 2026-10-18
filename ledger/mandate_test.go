package ledger

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

// mandateFixture creates a ledger with a funded payer and one active
// mandate: payer "cust" holds 100000 USD, payee "merchant", cap 5000
// USD per debit, per-day frequency, valid for 30 days from now.
func mandateFixture(t *testing.T) (*Ledger, Mandate) {
	t.Helper()
	l := New()
	if _, _, err := l.Post(JournalEntry{ID: "fund-cust", DebitAccount: "cust", CreditAccount: "bank", AmountCents: 100000, Currency: "USD"}); err != nil {
		t.Fatalf("fund Post: %v", err)
	}
	now := time.Now()
	m, err := l.CreateMandate(Mandate{
		ID:             "m-sub",
		Payer:          "cust",
		Payee:          "merchant",
		MaxAmountCents: 5000,
		Currency:       "USD",
		Frequency:      MandateFrequencyPerDay,
		ValidFrom:      now.Add(-time.Hour),
		ValidUntil:     now.Add(30 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateMandate: %v", err)
	}
	return l, m
}

func TestCreateMandateValidation(t *testing.T) {
	now := time.Now()
	valid := Mandate{
		ID:             "m-1",
		Payer:          "cust",
		Payee:          "merchant",
		MaxAmountCents: 5000,
		Currency:       "USD",
		Frequency:      MandateFrequencyPerDay,
		ValidFrom:      now.Add(-time.Hour),
		ValidUntil:     now.Add(24 * time.Hour),
	}
	cases := []struct {
		name    string
		mutate  func(*Mandate)
		wantErr error
	}{
		{"empty ID", func(m *Mandate) { m.ID = "" }, ErrEmptyMandateID},
		{"whitespace ID", func(m *Mandate) { m.ID = "m 1" }, ErrMandateIDWhitespace},
		{"empty payer", func(m *Mandate) { m.Payer = "" }, ErrEmptyMandatePayer},
		{"empty payee", func(m *Mandate) { m.Payee = "" }, ErrEmptyMandatePayee},
		{"same party", func(m *Mandate) { m.Payee = m.Payer }, ErrMandateSameParty},
		{"zero max", func(m *Mandate) { m.MaxAmountCents = 0 }, ErrMandateNonPositiveMaxAmount},
		{"negative max", func(m *Mandate) { m.MaxAmountCents = -1 }, ErrMandateNonPositiveMaxAmount},
		{"bad frequency", func(m *Mandate) { m.Frequency = "per-year" }, ErrInvalidMandateFrequency},
		{"empty frequency", func(m *Mandate) { m.Frequency = "" }, ErrInvalidMandateFrequency},
		{"zero validUntil", func(m *Mandate) { m.ValidUntil = time.Time{} }, ErrMandateInvalidWindow},
		{"validFrom after validUntil", func(m *Mandate) { m.ValidFrom = now.Add(48 * time.Hour) }, ErrMandateInvalidWindow},
		{"validFrom equals validUntil", func(m *Mandate) { m.ValidFrom = m.ValidUntil }, ErrMandateInvalidWindow},
		{"bad currency", func(m *Mandate) { m.Currency = "US" }, ErrInvalidCurrency},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := New()
			m := valid
			tc.mutate(&m)
			if _, err := l.CreateMandate(m); !errors.Is(err, tc.wantErr) {
				t.Errorf("CreateMandate err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestCreateMandateIDConflict(t *testing.T) {
	l := New()
	if _, _, err := l.Post(JournalEntry{ID: "taken", DebitAccount: "a", CreditAccount: "b", AmountCents: 100, Currency: "USD"}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	now := time.Now()
	mk := func(id string) Mandate {
		return Mandate{ID: id, Payer: "cust", Payee: "merchant", MaxAmountCents: 100, Currency: "USD", Frequency: MandateFrequencyPerDay, ValidFrom: now.Add(-time.Hour), ValidUntil: now.Add(time.Hour)}
	}
	// A mandate ID colliding with a journal entry ID is rejected: the
	// mandate/entry namespaces must stay disjoint for the mandate_of
	// link endpoints.
	if _, err := l.CreateMandate(mk("taken")); !errors.Is(err, ErrMandateIDConflict) {
		t.Errorf("entry-ID collision err = %v, want ErrMandateIDConflict", err)
	}
	if _, err := l.CreateMandate(mk("m-1")); err != nil {
		t.Fatalf("CreateMandate: %v", err)
	}
	if _, err := l.CreateMandate(mk("m-1")); !errors.Is(err, ErrMandateIDConflict) {
		t.Errorf("duplicate mandate err = %v, want ErrMandateIDConflict", err)
	}
	// Currency normalizes to the canonical code (empty -> default).
	m, err := l.CreateMandate(mk("m-2"))
	if err != nil {
		t.Fatalf("CreateMandate: %v", err)
	}
	if m.Currency != "USD" {
		t.Errorf("currency = %q, want USD", m.Currency)
	}
	if m.Status != MandateStatusActive {
		t.Errorf("status = %q, want active", m.Status)
	}
}

func TestMandateLifecycle(t *testing.T) {
	l, _ := mandateFixture(t)

	// Suspend: active -> suspended.
	m, err := l.SuspendMandate("m-sub")
	if err != nil {
		t.Fatalf("SuspendMandate: %v", err)
	}
	if m.Status != MandateStatusSuspended {
		t.Errorf("status = %q, want suspended", m.Status)
	}
	// Suspending a suspended mandate fails.
	if _, err := l.SuspendMandate("m-sub"); !errors.Is(err, ErrMandateNotActive) {
		t.Errorf("re-suspend err = %v, want ErrMandateNotActive", err)
	}
	// Cancel from suspended: terminal.
	m, err = l.CancelMandate("m-sub")
	if err != nil {
		t.Fatalf("CancelMandate: %v", err)
	}
	if m.Status != MandateStatusCancelled {
		t.Errorf("status = %q, want cancelled", m.Status)
	}
	if _, err := l.CancelMandate("m-sub"); !errors.Is(err, ErrMandateNotActive) {
		t.Errorf("re-cancel err = %v, want ErrMandateNotActive", err)
	}
	if _, err := l.SuspendMandate("m-sub"); !errors.Is(err, ErrMandateNotActive) {
		t.Errorf("suspend cancelled err = %v, want ErrMandateNotActive", err)
	}
	// Unknown mandate.
	if _, err := l.SuspendMandate("nope"); !errors.Is(err, ErrMandateNotFound) {
		t.Errorf("unknown suspend err = %v, want ErrMandateNotFound", err)
	}
	if _, err := l.CancelMandate("nope"); !errors.Is(err, ErrMandateNotFound) {
		t.Errorf("unknown cancel err = %v, want ErrMandateNotFound", err)
	}
}

func TestMandateLazyExpiry(t *testing.T) {
	l := New()
	now := time.Now()
	m, err := l.CreateMandate(Mandate{
		ID: "m-exp", Payer: "cust", Payee: "merchant",
		MaxAmountCents: 100, Currency: "USD", Frequency: MandateFrequencyPerDay,
		ValidFrom: now.Add(-2 * time.Hour), ValidUntil: now.Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateMandate: %v", err)
	}
	if m.Status != MandateStatusActive {
		t.Fatalf("stored status = %q, want active (expiry is lazy)", m.Status)
	}
	// Reads report the mandate as expired even before the sweep.
	got, ok := l.GetMandate("m-exp")
	if !ok {
		t.Fatalf("GetMandate: not found")
	}
	if got.Status != MandateStatusExpired {
		t.Errorf("effective status = %q, want expired", got.Status)
	}
	// Debits under it are rejected as expired.
	if _, err := l.DebitWithMandate("m-exp", MandateDebit{ID: "d-x", AmountCents: 10}); !errors.Is(err, ErrMandateExpired) {
		t.Errorf("debit err = %v, want ErrMandateExpired", err)
	}
	// Suspending an expired mandate is meaningless.
	if _, err := l.SuspendMandate("m-exp"); !errors.Is(err, ErrMandateExpired) {
		t.Errorf("suspend err = %v, want ErrMandateExpired", err)
	}
	// The sweep marks the row and reports the count.
	if n := l.ExpireMandates(); n != 1 {
		t.Errorf("ExpireMandates = %d, want 1", n)
	}
	got, _ = l.GetMandate("m-exp")
	if got.Status != MandateStatusExpired {
		t.Errorf("status after sweep = %q, want expired", got.Status)
	}
	// A sweep that expires nothing is silent in the audit trail.
	if n := l.ExpireMandates(); n != 0 {
		t.Errorf("second ExpireMandates = %d, want 0", n)
	}
}

func TestDebitWithMandateHappyPath(t *testing.T) {
	l, _ := mandateFixture(t)
	before := l.Version()
	receipt, err := l.DebitWithMandate("m-sub", MandateDebit{ID: "d-1", AmountCents: 1200, Memo: "subscription"})
	if err != nil {
		t.Fatalf("DebitWithMandate: %v", err)
	}
	if receipt.Duplicate {
		t.Errorf("duplicate = true, want false")
	}
	if receipt.MandateID != "m-sub" {
		t.Errorf("mandate_id = %q, want m-sub", receipt.MandateID)
	}
	e := receipt.Entry
	// Money direction: the payee is debited (increased), the payer
	// credited (decreased) — the pull moves funds payer -> payee.
	if e.DebitAccount != "merchant" || e.CreditAccount != "cust" {
		t.Errorf("legs = %s/%s, want merchant/cust", e.DebitAccount, e.CreditAccount)
	}
	if e.AmountCents != 1200 || e.Currency != "USD" {
		t.Errorf("entry = %+v, want 1200 USD", e)
	}
	// Memo channel: caller memo kept, mandate stamp appended.
	if e.Memo != "subscription mandate_of:m-sub" {
		t.Errorf("memo = %q, want %q", e.Memo, "subscription mandate_of:m-sub")
	}
	// The debit is a real journal posting: version bumped, balances moved.
	if l.Version() != before+1 {
		t.Errorf("version = %d, want %d", l.Version(), before+1)
	}
	if got := l.BalanceIn("cust", "USD"); got != 100000-1200 {
		t.Errorf("cust balance = %d, want %d", got, 100000-1200)
	}
	if got := l.BalanceIn("merchant", "USD"); got != 1200 {
		t.Errorf("merchant balance = %d, want 1200", got)
	}
	// Link channel: the debit entry carries an outgoing mandate_of link
	// to the mandate's reference record.
	links := l.EntryLinks("d-1")
	if len(links) != 1 {
		t.Fatalf("links = %d, want 1", len(links))
	}
	if links[0].LinkType != LinkTypeMandateOf || links[0].FromEntryID != "d-1" || links[0].ToEntryID != "m-sub" || links[0].Direction != LinkDirectionOutgoing {
		t.Errorf("link = %+v, want mandate_of d-1 -> m-sub outgoing", links[0])
	}
	// The mandate -> entries index backs the reconciliation query.
	page, next, err := l.MandateDebits("m-sub", "", 100)
	if err != nil {
		t.Fatalf("MandateDebits: %v", err)
	}
	if len(page) != 1 || page[0].ID != "d-1" || next != "" {
		t.Errorf("page = %v next = %q, want [d-1] and empty", page, next)
	}
	// Operational counter counts the posted debit.
	if n := l.MandateDebitCount(); n != 1 {
		t.Errorf("MandateDebitCount = %d, want 1", n)
	}
}

func TestDebitWithMandateGates(t *testing.T) {
	now := time.Now()
	setup := func(t *testing.T) (*Ledger, Mandate) {
		t.Helper()
		l := New()
		if _, _, err := l.Post(JournalEntry{ID: "fund", DebitAccount: "cust", CreditAccount: "bank", AmountCents: 100000, Currency: "USD"}); err != nil {
			t.Fatalf("fund: %v", err)
		}
		m, err := l.CreateMandate(Mandate{
			ID: "m-g", Payer: "cust", Payee: "merchant",
			MaxAmountCents: 5000, Currency: "USD", Frequency: MandateFrequencyPerDay,
			ValidFrom: now.Add(-time.Hour), ValidUntil: now.Add(24 * time.Hour),
		})
		if err != nil {
			t.Fatalf("CreateMandate: %v", err)
		}
		return l, m
	}
	t.Run("unknown mandate", func(t *testing.T) {
		l, _ := setup(t)
		if _, err := l.DebitWithMandate("nope", MandateDebit{ID: "d-1", AmountCents: 10}); !errors.Is(err, ErrMandateNotFound) {
			t.Errorf("err = %v, want ErrMandateNotFound", err)
		}
	})
	t.Run("amount over cap", func(t *testing.T) {
		l, _ := setup(t)
		if _, err := l.DebitWithMandate("m-g", MandateDebit{ID: "d-1", AmountCents: 5001}); !errors.Is(err, ErrMandateAmountExceeded) {
			t.Errorf("err = %v, want ErrMandateAmountExceeded", err)
		}
		// At the cap is fine.
		if _, err := l.DebitWithMandate("m-g", MandateDebit{ID: "d-1", AmountCents: 5000}); err != nil {
			t.Errorf("at-cap debit err = %v, want nil", err)
		}
	})
	t.Run("currency mismatch", func(t *testing.T) {
		l, _ := setup(t)
		if _, err := l.DebitWithMandate("m-g", MandateDebit{ID: "d-1", AmountCents: 10, Currency: "EUR"}); !errors.Is(err, ErrMandateCurrencyMismatch) {
			t.Errorf("err = %v, want ErrMandateCurrencyMismatch", err)
		}
	})
	t.Run("suspended", func(t *testing.T) {
		l, _ := setup(t)
		if _, err := l.SuspendMandate("m-g"); err != nil {
			t.Fatalf("suspend: %v", err)
		}
		if _, err := l.DebitWithMandate("m-g", MandateDebit{ID: "d-1", AmountCents: 10}); !errors.Is(err, ErrMandateNotActive) {
			t.Errorf("err = %v, want ErrMandateNotActive", err)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		l, _ := setup(t)
		if _, err := l.CancelMandate("m-g"); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		if _, err := l.DebitWithMandate("m-g", MandateDebit{ID: "d-1", AmountCents: 10}); !errors.Is(err, ErrMandateNotActive) {
			t.Errorf("err = %v, want ErrMandateNotActive", err)
		}
	})
	t.Run("frequency window used", func(t *testing.T) {
		l, _ := setup(t)
		if _, err := l.DebitWithMandate("m-g", MandateDebit{ID: "d-1", AmountCents: 10}); err != nil {
			t.Fatalf("first debit: %v", err)
		}
		if _, err := l.DebitWithMandate("m-g", MandateDebit{ID: "d-2", AmountCents: 10}); !errors.Is(err, ErrMandateFrequencyExceeded) {
			t.Errorf("err = %v, want ErrMandateFrequencyExceeded", err)
		}
	})
	t.Run("rejected debits record nothing", func(t *testing.T) {
		l, _ := setup(t)
		before := l.Version()
		l.DebitWithMandate("m-g", MandateDebit{ID: "d-1", AmountCents: 999999})
		if l.Version() != before {
			t.Errorf("version moved on rejected debit")
		}
		if l.EntryExists("d-1") {
			t.Errorf("rejected debit left a journal row")
		}
		if n := l.MandateDebitCount(); n != 0 {
			t.Errorf("counter = %d, want 0", n)
		}
	})
}

func TestDebitWithMandateFrequencyWindows(t *testing.T) {
	newMandate := func(t *testing.T, freq string) *Ledger {
		t.Helper()
		l := New()
		if _, _, err := l.Post(JournalEntry{ID: "fund", DebitAccount: "cust", CreditAccount: "bank", AmountCents: 1000000, Currency: "USD"}); err != nil {
			t.Fatalf("fund: %v", err)
		}
		now := time.Now()
		if _, err := l.CreateMandate(Mandate{
			ID: "m-w", Payer: "cust", Payee: "merchant",
			MaxAmountCents: 5000, Currency: "USD", Frequency: freq,
			ValidFrom: now.Add(-400 * 24 * time.Hour), ValidUntil: now.Add(400 * 24 * time.Hour),
		}); err != nil {
			t.Fatalf("CreateMandate: %v", err)
		}
		return l
	}
	debitAt := func(t *testing.T, l *Ledger, id string, at time.Time) error {
		t.Helper()
		_, err := l.DebitWithMandate("m-w", MandateDebit{ID: id, AmountCents: 10, CreatedAt: at})
		return err
	}
	t.Run("per-day resets at UTC midnight", func(t *testing.T) {
		l := newMandate(t, MandateFrequencyPerDay)
		day1 := time.Date(2026, 5, 4, 23, 0, 0, 0, time.UTC)
		sameDay := day1.Add(30 * time.Minute)  // 2026-05-04 23:30 UTC
		nextDay := day1.Add(2 * time.Hour)     // 2026-05-05 01:00 UTC
		if err := debitAt(t, l, "d-1", day1); err != nil {
			t.Fatalf("d-1: %v", err)
		}
		if err := debitAt(t, l, "d-2", sameDay); !errors.Is(err, ErrMandateFrequencyExceeded) {
			t.Errorf("same-day d-2 err = %v, want ErrMandateFrequencyExceeded", err)
		}
		if err := debitAt(t, l, "d-3", nextDay); err != nil {
			t.Errorf("next-day d-3 err = %v, want nil", err)
		}
	})
	t.Run("per-week resets Monday 00:00 UTC", func(t *testing.T) {
		l := newMandate(t, MandateFrequencyPerWeek)
		// 2026-05-04 is a Monday.
		mon := time.Date(2026, 5, 4, 10, 0, 0, 0, time.UTC)
		sun := time.Date(2026, 5, 10, 23, 0, 0, 0, time.UTC)
		nextMon := time.Date(2026, 5, 11, 1, 0, 0, 0, time.UTC)
		if err := debitAt(t, l, "d-1", mon); err != nil {
			t.Fatalf("d-1: %v", err)
		}
		if err := debitAt(t, l, "d-2", sun); !errors.Is(err, ErrMandateFrequencyExceeded) {
			t.Errorf("same-week d-2 err = %v, want ErrMandateFrequencyExceeded", err)
		}
		if err := debitAt(t, l, "d-3", nextMon); err != nil {
			t.Errorf("next-week d-3 err = %v, want nil", err)
		}
	})
	t.Run("per-month resets on the 1st UTC", func(t *testing.T) {
		l := newMandate(t, MandateFrequencyPerMonth)
		may31 := time.Date(2026, 5, 31, 23, 0, 0, 0, time.UTC)
		jun1 := time.Date(2026, 6, 1, 1, 0, 0, 0, time.UTC)
		if err := debitAt(t, l, "d-1", may31); err != nil {
			t.Fatalf("d-1: %v", err)
		}
		if err := debitAt(t, l, "d-2", jun1.Add(-2*time.Hour)); !errors.Is(err, ErrMandateFrequencyExceeded) {
			t.Errorf("same-month d-2 err = %v, want ErrMandateFrequencyExceeded", err)
		}
		if err := debitAt(t, l, "d-3", jun1); err != nil {
			t.Errorf("next-month d-3 err = %v, want nil", err)
		}
	})
}

func TestDebitWithMandateIdempotency(t *testing.T) {
	l, _ := mandateFixture(t)
	r1, err := l.DebitWithMandate("m-sub", MandateDebit{ID: "d-1", AmountCents: 100, IdempotencyKey: "k-1"})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	r2, err := l.DebitWithMandate("m-sub", MandateDebit{ID: "d-other", AmountCents: 999, IdempotencyKey: "k-1"})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !r2.Duplicate {
		t.Errorf("duplicate = false, want true")
	}
	if r2.Entry.ID != r1.Entry.ID {
		t.Errorf("replay entry = %s, want %s", r2.Entry.ID, r1.Entry.ID)
	}
	if r2.MandateID != "m-sub" {
		t.Errorf("replay mandate = %q, want m-sub", r2.MandateID)
	}
	// The replay booked nothing new: one journal row, one index row,
	// one counter tick, one link.
	if l.Version() != 2 { // fund + d-1
		t.Errorf("version = %d, want 2", l.Version())
	}
	page, _, _ := l.MandateDebits("m-sub", "", 100)
	if len(page) != 1 {
		t.Errorf("debits = %d, want 1", len(page))
	}
	if n := l.MandateDebitCount(); n != 1 {
		t.Errorf("counter = %d, want 1", n)
	}
	if links := l.EntryLinks("d-1"); len(links) != 1 {
		t.Errorf("links = %d, want 1", len(links))
	}
	// A replay succeeds even after the mandate is suspended: it books
	// nothing new.
	if _, err := l.SuspendMandate("m-sub"); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	r3, err := l.DebitWithMandate("m-sub", MandateDebit{ID: "d-x", AmountCents: 1, IdempotencyKey: "k-1"})
	if err != nil {
		t.Fatalf("replay after suspend: %v", err)
	}
	if !r3.Duplicate || r3.Entry.ID != "d-1" {
		t.Errorf("replay after suspend = %+v, want duplicate of d-1", r3)
	}
}

func TestDebitWithMandateRunsFullPostGates(t *testing.T) {
	l, _ := mandateFixture(t)
	// A frozen payer rejects the debit with the Post gate's own error:
	// the mandate debit cannot dodge booking controls.
	l.Freeze("cust")
	_, err := l.DebitWithMandate("m-sub", MandateDebit{ID: "d-1", AmountCents: 100})
	if !errors.Is(err, ErrAccountFrozen) {
		t.Errorf("err = %v, want ErrAccountFrozen", err)
	}
	if l.EntryExists("d-1") {
		t.Errorf("frozen debit left a journal row")
	}
}

func TestMandateDebitMemoSearchable(t *testing.T) {
	l, _ := mandateFixture(t)
	if _, err := l.DebitWithMandate("m-sub", MandateDebit{ID: "d-1", AmountCents: 100}); err != nil {
		t.Fatalf("debit: %v", err)
	}
	// The memo channel: debits are findable via the entries memo filter.
	page, _, err := l.ListEntriesFiltered(time.Time{}, time.Now().Add(time.Hour), "", 100, "mandate_of:m-sub", "")
	if err != nil {
		t.Fatalf("ListEntriesFiltered: %v", err)
	}
	if len(page) != 1 || page[0].ID != "d-1" {
		t.Errorf("memo search = %v, want [d-1]", page)
	}
}

func TestMandateSnapshotRoundTrip(t *testing.T) {
	l, m := mandateFixture(t)
	if _, err := l.DebitWithMandate("m-sub", MandateDebit{ID: "d-1", AmountCents: 100}); err != nil {
		t.Fatalf("debit: %v", err)
	}
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	l2, err := ImportSnapshot(&buf)
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	// The mandate restores with its fields and the debit index.
	got, ok := l2.GetMandate("m-sub")
	if !ok {
		t.Fatalf("restored mandate missing")
	}
	if got.Payer != m.Payer || got.Payee != m.Payee || got.MaxAmountCents != m.MaxAmountCents ||
		got.Currency != m.Currency || got.Frequency != m.Frequency || got.Status != MandateStatusActive {
		t.Errorf("restored mandate = %+v, want %+v", got, m)
	}
	if !got.ValidFrom.Equal(m.ValidFrom) || !got.ValidUntil.Equal(m.ValidUntil) {
		t.Errorf("restored window = %v/%v, want %v/%v", got.ValidFrom, got.ValidUntil, m.ValidFrom, m.ValidUntil)
	}
	page, _, err := l2.MandateDebits("m-sub", "", 100)
	if err != nil {
		t.Fatalf("restored MandateDebits: %v", err)
	}
	if len(page) != 1 || page[0].ID != "d-1" {
		t.Errorf("restored debits = %v, want [d-1]", page)
	}
	// The frequency gate still applies after restore: the window is used.
	if _, err := l2.DebitWithMandate("m-sub", MandateDebit{ID: "d-2", AmountCents: 100}); !errors.Is(err, ErrMandateFrequencyExceeded) {
		t.Errorf("post-restore debit err = %v, want ErrMandateFrequencyExceeded", err)
	}
	// The mandate_of link round-trips with the entry links.
	links := l2.EntryLinks("d-1")
	if len(links) != 1 || links[0].LinkType != LinkTypeMandateOf || links[0].ToEntryID != "m-sub" {
		t.Errorf("restored links = %+v, want one mandate_of -> m-sub", links)
	}
	// The link chain still verifies.
	if err := l2.VerifyChain(); err != nil {
		t.Errorf("VerifyChain after restore: %v", err)
	}
	// Balances survived.
	if got := l2.BalanceIn("cust", "USD"); got != 100000-100 {
		t.Errorf("restored cust balance = %d, want %d", got, 100000-100)
	}
}

func TestMandateReconcileReport(t *testing.T) {
	l, _ := mandateFixture(t)
	if _, err := l.DebitWithMandate("m-sub", MandateDebit{ID: "d-1", AmountCents: 100}); err != nil {
		t.Fatalf("debit: %v", err)
	}
	rep := l.Reconcile(time.Now())
	if len(rep.Mandates) != 1 {
		t.Fatalf("mandates = %d, want 1", len(rep.Mandates))
	}
	row := rep.Mandates[0]
	if row.ID != "m-sub" || row.Payer != "cust" || row.Payee != "merchant" {
		t.Errorf("row identity = %+v", row)
	}
	if row.Status != MandateStatusActive {
		t.Errorf("status = %q, want active", row.Status)
	}
	if row.TotalDebits != 1 || row.WindowDebits != 1 {
		t.Errorf("usage = %d/%d, want 1/1", row.TotalDebits, row.WindowDebits)
	}
	if row.MaxAmountCents != 5000 || row.Currency != "USD" || row.Frequency != MandateFrequencyPerDay {
		t.Errorf("row terms = %+v", row)
	}
}

func TestMandateOfLinkValidation(t *testing.T) {
	l, _ := mandateFixture(t)
	if _, err := l.DebitWithMandate("m-sub", MandateDebit{ID: "d-1", AmountCents: 100}); err != nil {
		t.Fatalf("debit: %v", err)
	}
	// A hand-declared mandate_of link to an unknown mandate fails.
	if _, _, err := l.DeclareEntryLink("d-1", "nope", LinkTypeMandateOf); !errors.Is(err, ErrMandateNotFound) {
		t.Errorf("err = %v, want ErrMandateNotFound", err)
	}
	// Re-declaring the identical mandate_of triple is a silent no-op.
	link, declared, err := l.DeclareEntryLink("d-1", "m-sub", LinkTypeMandateOf)
	if err != nil {
		t.Fatalf("redeclare: %v", err)
	}
	if declared {
		t.Errorf("declared = true, want false (idempotent replay)")
	}
	if link.LinkType != LinkTypeMandateOf {
		t.Errorf("link type = %q", link.LinkType)
	}
	// mandate_of is a declared link type now.
	if !validLinkType(LinkTypeMandateOf) {
		t.Errorf("validLinkType(mandate_of) = false")
	}
}

func TestMandateAuditTrail(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	now := time.Now()
	if _, err := l.CreateMandate(Mandate{
		ID: "m-audit", Payer: "cust", Payee: "merchant",
		MaxAmountCents: 5000, Currency: "USD", Frequency: MandateFrequencyPerDay,
		ValidFrom: now.Add(-time.Hour), ValidUntil: now.Add(24 * time.Hour),
	}); err != nil {
		t.Fatalf("CreateMandate: %v", err)
	}
	if _, _, err := l.Post(JournalEntry{ID: "fund", DebitAccount: "cust", CreditAccount: "bank", AmountCents: 100000, Currency: "USD"}); err != nil {
		t.Fatalf("fund: %v", err)
	}
	if _, err := l.DebitWithMandate("m-audit", MandateDebit{ID: "d-1", AmountCents: 100}); err != nil {
		t.Fatalf("debit: %v", err)
	}
	if _, err := l.SuspendMandate("m-audit"); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	// Every mandate change entered the structured audit chain (LG-32).
	events := readAuditOps(t, al)
	for _, op := range []string{"mandate_created", "mandate_debit", "mandate_suspended"} {
		if countOp(events, op) != 1 {
			t.Errorf("op %q count = %d, want 1", op, countOp(events, op))
		}
	}
	debit := findOp(events, "mandate_debit")
	if debit == nil {
		t.Fatalf("mandate_debit event missing")
	}
	if debit.TraceID != "m-audit" {
		t.Errorf("mandate_debit trace = %q, want m-audit", debit.TraceID)
	}
	if len(debit.EntryIDs) != 1 || debit.EntryIDs[0] != "d-1" {
		t.Errorf("mandate_debit entries = %v, want [d-1]", debit.EntryIDs)
	}
}
