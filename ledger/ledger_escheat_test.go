package ledger

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// fundDormant posts a backdated credit to account (positive balance)
// and marks it dormant via DetectDormant. Returns the account's
// dormancy record state for assertions.
func fundDormant(t *testing.T, l *Ledger, account string, cents int64, currency string, daysAgo int, threshold int) {
	t.Helper()
	at := time.Now().UTC().AddDate(0, 0, -daysAgo)
	e := JournalEntry{
		ID:            "escheat-setup-" + account + "-" + currency,
		DebitAccount:  AccountID(account),
		CreditAccount: "funding",
		AmountCents:   cents,
		Currency:      currency,
		CreatedAt:     at,
	}
	if _, _, err := l.Post(e); err != nil {
		t.Fatalf("setup post: %v", err)
	}
	if _, err := l.DetectDormant(threshold); err != nil {
		t.Fatalf("DetectDormant: %v", err)
	}
	if !l.IsDormant(AccountID(account)) {
		t.Fatalf("%s not marked dormant", account)
	}
}

func TestEscheatDormantBasic(t *testing.T) {
	l := New()
	fundDormant(t, l, "saver", 50_000, "USD", 60, 30)

	report, err := l.EscheatDormant(30, "unclaimed")
	if err != nil {
		t.Fatalf("EscheatDormant: %v", err)
	}
	if len(report.Escheated) != 1 {
		t.Fatalf("escheated = %d, want 1: %+v", len(report.Escheated), report)
	}
	rec := report.Escheated[0]
	if rec.Account != "saver" || rec.Currency != "USD" || rec.AmountCents != 50_000 {
		t.Fatalf("record = %+v", rec)
	}
	if !strings.HasPrefix(rec.EntryID, "escheat/saver/USD/") {
		t.Fatalf("entry id = %q", rec.EntryID)
	}
	if rec.Reversed {
		t.Fatal("fresh sweep must not be marked reversed")
	}

	// Money moved: saver zeroed, escheat account credited.
	if b := l.BalanceIn("saver", "USD"); b != 0 {
		t.Fatalf("saver balance = %d, want 0", b)
	}
	if b := l.BalanceIn("unclaimed", "USD"); b != 50_000 {
		t.Fatalf("unclaimed balance = %d, want 50000", b)
	}
	// The swept account is frozen but keeps its dormant mark.
	if !l.IsFrozen("saver") {
		t.Fatal("saver must be frozen after escheat")
	}
	if !l.IsDormant("saver") {
		t.Fatal("saver must stay dormant after escheat")
	}
	// The entry is a full journal row with the escheat memo.
	e, ok := l.entries[rec.EntryID]
	if !ok {
		t.Fatalf("entry %q not in journal", rec.EntryID)
	}
	if e.Memo != "escheat" || e.DebitAccount != "unclaimed" || e.CreditAccount != "saver" {
		t.Fatalf("entry = %+v", e)
	}
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}

	// A second run finds nothing to sweep: the balance is zero now.
	report2, err := l.EscheatDormant(30, "unclaimed")
	if err != nil {
		t.Fatal(err)
	}
	if len(report2.Escheated) != 0 {
		t.Fatalf("second run escheated %d, want 0", len(report2.Escheated))
	}
}

func TestEscheatDormantMultiCurrency(t *testing.T) {
	l := New()
	// Both currencies must be posted before DetectDormant marks the
	// account: an operator Post through a dormant account is rejected.
	at := time.Now().UTC().AddDate(0, 0, -60)
	for _, tc := range []struct {
		id, currency string
		cents        int64
	}{
		{"escheat-setup-saver-USD", "USD", 50_000},
		{"escheat-setup-saver-EUR", "EUR", 30_000},
	} {
		if _, _, err := l.Post(JournalEntry{ID: tc.id, DebitAccount: "saver", CreditAccount: "funding", AmountCents: tc.cents, Currency: tc.currency, CreatedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := l.DetectDormant(30); err != nil {
		t.Fatal(err)
	}
	if !l.IsDormant("saver") {
		t.Fatal("saver not marked dormant")
	}

	report, err := l.EscheatDormant(30, "unclaimed")
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Escheated) != 2 {
		t.Fatalf("escheated = %d, want 2: %+v", len(report.Escheated), report)
	}
	// Sorted by currency: EUR before USD.
	if report.Escheated[0].Currency != "EUR" || report.Escheated[1].Currency != "USD" {
		t.Fatalf("order = %+v", report.Escheated)
	}
	if b := l.BalanceIn("saver", "EUR"); b != 0 {
		t.Fatalf("saver EUR = %d, want 0", b)
	}
	if b := l.BalanceIn("unclaimed", "EUR"); b != 30_000 {
		t.Fatalf("unclaimed EUR = %d, want 30000", b)
	}
}

func TestEscheatDormantNotEligible(t *testing.T) {
	l := New()
	// Recent activity: dormant mark exists but the threshold is not met.
	fundDormant(t, l, "active", 10_000, "USD", 10, 5)
	// Old activity but never marked dormant: posted after the last
	// DetectDormant run, through a fresh funding account (the "funding"
	// account above is dormant by now, and an operator Post through it
	// would be rejected).
	at := time.Now().UTC().AddDate(0, 0, -60)
	if _, _, err := l.Post(JournalEntry{ID: "no-mark", DebitAccount: "unmarked", CreditAccount: "funding2", AmountCents: 10_000, CreatedAt: at}); err != nil {
		t.Fatal(err)
	}

	report, err := l.EscheatDormant(30, "unclaimed")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range report.Escheated {
		if r.Account == "active" || r.Account == "unmarked" {
			t.Fatalf("ineligible account swept: %+v", r)
		}
	}
	if b := l.BalanceIn("active", "USD"); b != 10_000 {
		t.Fatalf("active balance moved: %d", b)
	}
}

func TestEscheatDormantValidation(t *testing.T) {
	l := New()
	if _, err := l.EscheatDormant(0, "unclaimed"); err != ErrInvalidEscheatThreshold {
		t.Fatalf("threshold 0: err = %v", err)
	}
	if _, err := l.EscheatDormant(-5, "unclaimed"); err != ErrInvalidEscheatThreshold {
		t.Fatalf("threshold -5: err = %v", err)
	}
	if _, err := l.EscheatDormant(30, ""); err != ErrInvalidEscheatAccount {
		t.Fatalf("empty account: err = %v", err)
	}
	if _, err := l.EscheatDormant(30, "a/b"); err != ErrInvalidEscheatAccount {
		t.Fatalf("slash account: err = %v", err)
	}
}

func TestEscheatDormantFrozenAccountSkipped(t *testing.T) {
	l := New()
	fundDormant(t, l, "saver", 50_000, "USD", 60, 30)
	l.Freeze("saver")

	report, err := l.EscheatDormant(30, "unclaimed")
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Escheated) != 0 {
		t.Fatalf("frozen account must not be swept: %+v", report.Escheated)
	}
	if len(report.Skipped) != 1 || !strings.Contains(report.Skipped[0].Reason, "frozen") {
		t.Fatalf("skipped = %+v, want a frozen rejection", report.Skipped)
	}
	if b := l.BalanceIn("saver", "USD"); b != 50_000 {
		t.Fatalf("saver balance moved: %d", b)
	}
}

func TestReverseEscheatment(t *testing.T) {
	l := New()
	fundDormant(t, l, "saver", 50_000, "USD", 60, 30)
	report, err := l.EscheatDormant(30, "unclaimed")
	if err != nil {
		t.Fatal(err)
	}
	escheatID := report.Escheated[0].EntryID

	rev, dup, err := l.ReverseEscheatment(escheatID, time.Time{})
	if err != nil {
		t.Fatalf("ReverseEscheatment: %v", err)
	}
	if dup {
		t.Fatal("first reversal must not be a duplicate")
	}
	if rev.ID != escheatID+"/reversal" {
		t.Fatalf("reversal id = %q", rev.ID)
	}
	if rev.Memo != "escheat:reversal" {
		t.Fatalf("reversal memo = %q", rev.Memo)
	}
	if rev.DebitAccount != "saver" || rev.CreditAccount != "unclaimed" {
		t.Fatalf("reversal legs = %s/%s", rev.DebitAccount, rev.CreditAccount)
	}
	// Money is back and the account is unfrozen — the exact pre-escheat
	// state — but the dormant mark stays: the account is still quiet.
	if b := l.BalanceIn("saver", "USD"); b != 50_000 {
		t.Fatalf("saver balance = %d, want 50000", b)
	}
	if b := l.BalanceIn("unclaimed", "USD"); b != 0 {
		t.Fatalf("unclaimed balance = %d, want 0", b)
	}
	if l.IsFrozen("saver") {
		t.Fatal("saver must be unfrozen after reversal")
	}
	if !l.IsDormant("saver") {
		t.Fatal("saver must stay dormant after reversal")
	}
	// Reversing twice returns the existing reversal.
	rev2, dup, err := l.ReverseEscheatment(escheatID, time.Time{})
	if err != nil || !dup || rev2.ID != rev.ID {
		t.Fatalf("second reversal: dup=%v err=%v id=%q", dup, err, rev2.ID)
	}
	// Unknown or non-escheat IDs are rejected.
	if _, _, err := l.ReverseEscheatment("escheat/nonexistent/USD/2026-01-01", time.Time{}); err != ErrEscheatNotFound {
		t.Fatalf("unknown id: err = %v", err)
	}
	if _, _, err := l.ReverseEscheatment("pos1", time.Time{}); err != ErrEscheatNotFound {
		t.Fatalf("non-escheat id: err = %v", err)
	}
	if _, _, err := l.ReverseEscheatment(escheatID+"/reversal", time.Time{}); err != ErrEscheatNotFound {
		t.Fatalf("reversal-of-reversal: err = %v", err)
	}
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
}

func TestEscheatDormantReconcileDisclosure(t *testing.T) {
	l := New()
	fundDormant(t, l, "saver", 50_000, "USD", 60, 30)
	report, err := l.EscheatDormant(30, "unclaimed")
	if err != nil {
		t.Fatal(err)
	}

	rep := l.Reconcile(time.Now().UTC())
	if len(rep.Escheats) != 1 {
		t.Fatalf("reconcile escheats = %d, want 1", len(rep.Escheats))
	}
	rec := rep.Escheats[0]
	if rec.Account != "saver" || rec.AmountCents != 50_000 || rec.EntryID != report.Escheated[0].EntryID {
		t.Fatalf("reconcile record = %+v", rec)
	}
	if rec.Reversed {
		t.Fatal("must not be marked reversed before reversal")
	}

	if _, _, err := l.ReverseEscheatment(rec.EntryID, time.Time{}); err != nil {
		t.Fatal(err)
	}
	rep2 := l.Reconcile(time.Now().UTC())
	if len(rep2.Escheats) != 1 || !rep2.Escheats[0].Reversed {
		t.Fatalf("after reversal: %+v", rep2.Escheats)
	}
}

func TestEscheatDormantConcurrent(t *testing.T) {
	l := New()
	fundDormant(t, l, "saver", 50_000, "USD", 60, 30)

	var wg sync.WaitGroup
	results := make(chan EscheatReport, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := l.EscheatDormant(30, "unclaimed")
			results <- r
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent sweep: %v", err)
		}
	}
	total := 0
	for r := range results {
		total += len(r.Escheated)
	}
	// Exactly one sweep books; the other finds a zero balance.
	if total != 1 {
		t.Fatalf("total escheated = %d, want 1", total)
	}
	if b := l.BalanceIn("unclaimed", "USD"); b != 50_000 {
		t.Fatalf("unclaimed = %d, want 50000", b)
	}
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
}
