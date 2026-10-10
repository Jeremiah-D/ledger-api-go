package ledger

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

// mustPostThreshold posts e, failing the test on error.
func mustPostThreshold(t *testing.T, l *Ledger, e JournalEntry) JournalEntry {
	t.Helper()
	posted, _, err := l.Post(e)
	if err != nil {
		t.Fatalf("Post %s: %v", e.ID, err)
	}
	return posted
}

func TestLowBalanceThresholdSetClear(t *testing.T) {
	l := New()
	if err := l.SetLowBalanceThreshold("cust-1", "USD", 50000); err != nil {
		t.Fatalf("SetLowBalanceThreshold: %v", err)
	}
	got, ok := l.LowBalanceThreshold("cust-1", "USD")
	if !ok || got != 50000 {
		t.Fatalf("LowBalanceThreshold = (%d, %v), want (50000, true)", got, ok)
	}
	// Empty currency means the default currency.
	if err := l.SetLowBalanceThreshold("cust-1", "", 100); err != nil {
		t.Fatalf("SetLowBalanceThreshold default currency: %v", err)
	}
	if got, ok := l.LowBalanceThreshold("cust-1", DefaultCurrency); !ok || got != 100 {
		t.Fatalf("LowBalanceThreshold default = (%d, %v), want (100, true)", got, ok)
	}
	// Replace is allowed and does not bump the version: structural config.
	v := l.version
	if err := l.SetLowBalanceThreshold("cust-1", "USD", 60000); err != nil {
		t.Fatalf("SetLowBalanceThreshold replace: %v", err)
	}
	if got, _ := l.LowBalanceThreshold("cust-1", "USD"); got != 60000 {
		t.Fatalf("replaced threshold = %d, want 60000", got)
	}
	if l.version != v {
		t.Fatalf("version moved %d -> %d on threshold change; want no bump", v, l.version)
	}
	// A bad currency fails.
	if err := l.SetLowBalanceThreshold("cust-1", "US", 1); !errors.Is(err, ErrInvalidCurrency) {
		t.Fatalf("SetLowBalanceThreshold bad currency: %v, want ErrInvalidCurrency", err)
	}
	// Clear removes config and reports unset.
	l.ClearLowBalanceThreshold("cust-1", "USD")
	if _, ok := l.LowBalanceThreshold("cust-1", "USD"); ok {
		t.Fatal("LowBalanceThreshold after clear: still configured")
	}
	// Clearing a never-set threshold is a no-op.
	l.ClearLowBalanceThreshold("nobody", "USD")
}

func TestLowBalanceBreachFiresOncePerBreach(t *testing.T) {
	al := mustAuditLog(t)
	defer al.Close()
	l := New(WithAuditLog(al))

	// Seed cust-1 with 1000 USD (credit side decreases; debit cash).
	mustPostThreshold(t, l, JournalEntry{ID: "seed", DebitAccount: "cust-1", CreditAccount: "bank", AmountCents: 1000, Currency: "USD"})
	if err := l.SetLowBalanceThreshold("cust-1", "USD", 500); err != nil {
		t.Fatalf("SetLowBalanceThreshold: %v", err)
	}

	// First fall below 500 fires exactly once.
	mustPostThreshold(t, l, JournalEntry{ID: "e1", DebitAccount: "bank", CreditAccount: "cust-1", AmountCents: 600, Currency: "USD"})
	if n := l.LowBalanceBreachCount(); n != 1 {
		t.Fatalf("breach count = %d, want 1", n)
	}
	// While the balance stays below, further postings are silent.
	mustPostThreshold(t, l, JournalEntry{ID: "e2", DebitAccount: "bank", CreditAccount: "cust-1", AmountCents: 100, Currency: "USD"})
	if n := l.LowBalanceBreachCount(); n != 1 {
		t.Fatalf("breach count = %d, want still 1", n)
	}
	// Recovering to the threshold re-arms, silently.
	mustPostThreshold(t, l, JournalEntry{ID: "e3", DebitAccount: "cust-1", CreditAccount: "bank", AmountCents: 500, Currency: "USD"})
	if got := l.BalanceIn("cust-1", "USD"); got != 800 {
		t.Fatalf("balance = %d, want 800", got)
	}
	if n := l.LowBalanceBreachCount(); n != 1 {
		t.Fatalf("breach count = %d after re-arm, want still 1", n)
	}
	// The next fall below fires again.
	mustPostThreshold(t, l, JournalEntry{ID: "e4", DebitAccount: "bank", CreditAccount: "cust-1", AmountCents: 400, Currency: "USD"})
	if n := l.LowBalanceBreachCount(); n != 2 {
		t.Fatalf("breach count = %d, want 2", n)
	}

	// The breach event carries the required fields and a version bracket
	// of zero movement (advisory: the posting already bumped the
	// version; the alert brackets the state it evaluated).
	now := time.Now()
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, corrupt, err := ReadAuditLog(al.Dir(), now.UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt = %d, want 0", corrupt)
	}
	var breaches []AuditEvent
	for _, ev := range events {
		if ev.Op == "low_balance_breach" {
			breaches = append(breaches, ev)
		}
	}
	if len(breaches) != 2 {
		t.Fatalf("low_balance_breach events = %d, want 2", len(breaches))
	}
	b := breaches[0]
	if b.Actor != "Post" || b.TraceID != "e1" {
		t.Errorf("breach event = actor %q trace %q, want Post/e1", b.Actor, b.TraceID)
	}
	if b.Details["account"] != "cust-1" || b.Details["currency"] != "USD" {
		t.Errorf("breach details account/currency = %v/%v, want cust-1/USD", b.Details["account"], b.Details["currency"])
	}
	if b.Details["threshold_cents"] != float64(500) || b.Details["balance_cents"] != float64(400) {
		t.Errorf("breach details threshold/balance = %v/%v, want 500/400", b.Details["threshold_cents"], b.Details["balance_cents"])
	}
	if b.Details["version"] != float64(2) { // JSON decodes numbers as float64
		t.Errorf("breach details version = %v, want 2", b.Details["version"])
	}
	if b.VersionBefore != b.VersionAfter {
		t.Errorf("breach version bracket = %d..%d, want identical (read-only)", b.VersionBefore, b.VersionAfter)
	}
}

func TestLowBalanceNoAlertWithoutThreshold(t *testing.T) {
	l := New()
	mustPostThreshold(t, l, JournalEntry{ID: "e1", DebitAccount: "a", CreditAccount: "b", AmountCents: 100, Currency: "USD"})
	if n := l.LowBalanceBreachCount(); n != 0 {
		t.Fatalf("breach count = %d without any threshold, want 0", n)
	}
}

func TestLowBalancePerCurrencyIsolation(t *testing.T) {
	l := New()
	if err := l.SetLowBalanceThreshold("cust-1", "USD", 500); err != nil {
		t.Fatalf("SetLowBalanceThreshold: %v", err)
	}
	mustPostThreshold(t, l, JournalEntry{ID: "seed", DebitAccount: "cust-1", CreditAccount: "bank", AmountCents: 1000, Currency: "USD"})
	// A EUR posting that leaves the EUR balance negative does not fire:
	// the threshold is USD-only.
	mustPostThreshold(t, l, JournalEntry{ID: "e1", DebitAccount: "bank", CreditAccount: "cust-1", AmountCents: 100, Currency: "EUR"})
	if n := l.LowBalanceBreachCount(); n != 0 {
		t.Fatalf("breach count = %d for untracked currency, want 0", n)
	}
}

func TestLowBalanceReplaysAndRejectionsSilent(t *testing.T) {
	l := New()
	if err := l.SetLowBalanceThreshold("cust-1", "USD", 500); err != nil {
		t.Fatalf("SetLowBalanceThreshold: %v", err)
	}
	mustPostThreshold(t, l, JournalEntry{ID: "seed", DebitAccount: "cust-1", CreditAccount: "bank", AmountCents: 1000, Currency: "USD", IdempotencyKey: "k0"})
	mustPostThreshold(t, l, JournalEntry{ID: "e1", DebitAccount: "bank", CreditAccount: "cust-1", AmountCents: 900, Currency: "USD", IdempotencyKey: "k1"})
	if n := l.LowBalanceBreachCount(); n != 1 {
		t.Fatalf("breach count = %d, want 1", n)
	}
	// An idempotent replay books nothing: no second alert.
	mustPostThreshold(t, l, JournalEntry{ID: "e1x", DebitAccount: "bank", CreditAccount: "cust-1", AmountCents: 900, Currency: "USD", IdempotencyKey: "k1"})
	if n := l.LowBalanceBreachCount(); n != 1 {
		t.Fatalf("breach count = %d after replay, want still 1", n)
	}
	// A rejected posting books nothing: no alert.
	if _, _, err := l.Post(JournalEntry{ID: "bad", DebitAccount: "bank", CreditAccount: "cust-1", AmountCents: -5, Currency: "USD"}); err == nil {
		t.Fatal("negative amount posted, want rejection")
	}
	if n := l.LowBalanceBreachCount(); n != 1 {
		t.Fatalf("breach count = %d after rejected post, want still 1", n)
	}
}

func TestLowBalanceFiresOnTransferBatchSweepMergeCapture(t *testing.T) {
	newSeeded := func(t *testing.T, acct string) *Ledger {
		t.Helper()
		l := New()
		mustPostThreshold(t, l, JournalEntry{ID: "seed", DebitAccount: AccountID(acct), CreditAccount: "bank", AmountCents: 1000, Currency: "USD"})
		if err := l.SetLowBalanceThreshold(AccountID(acct), "USD", 500); err != nil {
			t.Fatalf("SetLowBalanceThreshold: %v", err)
		}
		return l
	}

	// Transfer: payer outflow below the threshold.
	l := newSeeded(t, "payer")
	if _, err := l.PostTransfer(Transfer{ID: "tr1", From: "payer", To: "payee", AmountCents: 800}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}
	if n := l.LowBalanceBreachCount(); n != 1 {
		t.Fatalf("transfer breach count = %d, want 1", n)
	}

	// Batch: one entry takes the account below.
	l = newSeeded(t, "payer")
	if _, err := l.PostBatch(Batch{ID: "b1", Entries: []JournalEntry{
		{ID: "be1", DebitAccount: "bank", CreditAccount: "payer", AmountCents: 800, Currency: "USD"},
	}}); err != nil {
		t.Fatalf("PostBatch: %v", err)
	}
	if n := l.LowBalanceBreachCount(); n != 1 {
		t.Fatalf("batch breach count = %d, want 1", n)
	}

	// Sweep: drains the source below its threshold.
	l = newSeeded(t, "src")
	if _, err := l.PostSweep(Sweep{ID: "sw1", From: []AccountID{"src"}, To: "treasury"}); err != nil {
		t.Fatalf("PostSweep: %v", err)
	}
	if n := l.LowBalanceBreachCount(); n != 1 {
		t.Fatalf("sweep breach count = %d, want 1", n)
	}

	// Merge: moves the source balance out.
	l = newSeeded(t, "src")
	if _, err := l.PostMerge(Merge{ID: "m1", From: "src", To: "dst"}); err != nil {
		t.Fatalf("PostMerge: %v", err)
	}
	if n := l.LowBalanceBreachCount(); n != 1 {
		t.Fatalf("merge breach count = %d, want 1", n)
	}

	// Hold capture: the held account's outflow below the threshold.
	l = newSeeded(t, "payer")
	now := time.Now()
	if _, _, err := l.Hold(Hold{ID: "h1", Account: "payer", AmountCents: 800, Currency: "USD", ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if _, err := l.Capture(Capture{ID: "c1", HoldID: "h1", To: "payee", AmountCents: 800}); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if n := l.LowBalanceBreachCount(); n != 1 {
		t.Fatalf("capture breach count = %d, want 1", n)
	}
}

func TestLowBalanceReconcileListsThresholds(t *testing.T) {
	l := New()
	if err := l.SetLowBalanceThreshold("cust-b", "EUR", -100); err != nil {
		t.Fatalf("SetLowBalanceThreshold: %v", err)
	}
	if err := l.SetLowBalanceThreshold("cust-a", "USD", 500); err != nil {
		t.Fatalf("SetLowBalanceThreshold: %v", err)
	}
	report := l.Reconcile(time.Now())
	if len(report.LowBalanceThresholds) != 2 {
		t.Fatalf("low_balance_thresholds = %d, want 2", len(report.LowBalanceThresholds))
	}
	// Sorted by (account, currency).
	if report.LowBalanceThresholds[0].Account != "cust-a" || report.LowBalanceThresholds[0].ThresholdCents != 500 {
		t.Errorf("thresholds[0] = %+v, want cust-a/USD/500", report.LowBalanceThresholds[0])
	}
	if report.LowBalanceThresholds[1].Account != "cust-b" || report.LowBalanceThresholds[1].ThresholdCents != -100 {
		t.Errorf("thresholds[1] = %+v, want cust-b/EUR/-100", report.LowBalanceThresholds[1])
	}
}

func TestLowBalanceThresholdSnapshotRoundTrip(t *testing.T) {
	l := New()
	if err := l.SetLowBalanceThreshold("cust-1", "USD", 500); err != nil {
		t.Fatalf("SetLowBalanceThreshold: %v", err)
	}
	mustPostThreshold(t, l, JournalEntry{ID: "e1", DebitAccount: "cust-1", CreditAccount: "bank", AmountCents: 1000, Currency: "USD"})
	mustPostThreshold(t, l, JournalEntry{ID: "e2", DebitAccount: "bank", CreditAccount: "cust-1", AmountCents: 800, Currency: "USD"})
	// Balance 200 < 500: the alert fired once.
	if n := l.LowBalanceBreachCount(); n != 1 {
		t.Fatalf("breach count = %d, want 1", n)
	}

	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if !snapshotLedgersEqual(l, restored) {
		t.Fatal("restored ledger differs from original")
	}
	// Thresholds survive the restore.
	if got, ok := restored.LowBalanceThreshold("cust-1", "USD"); !ok || got != 500 {
		t.Fatalf("restored threshold = (%d, %v), want (500, true)", got, ok)
	}
	// The breach state is re-derived silently: the restored balance is
	// already below the threshold, so the next posting does not backfire
	// a stale alert — and the counter restarts at zero on the restored
	// ledger (alert history lives in the audit log).
	if n := restored.LowBalanceBreachCount(); n != 0 {
		t.Fatalf("restored breach count = %d, want 0 (restore emits nothing)", n)
	}
	mustPostThreshold(t, restored, JournalEntry{ID: "e3", DebitAccount: "bank", CreditAccount: "cust-1", AmountCents: 10, Currency: "USD"})
	if n := restored.LowBalanceBreachCount(); n != 0 {
		t.Fatalf("restored breach count = %d after posting while still below, want 0 (already breached)", n)
	}
}

func TestParseLowBalanceThresholds(t *testing.T) {
	got, err := ParseLowBalanceThresholds("cust-123:USD:100000,cust-456:EUR:-5000")
	if err != nil {
		t.Fatalf("ParseLowBalanceThresholds: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("parsed = %d thresholds, want 2", len(got))
	}
	if got[0].Account != "cust-123" || got[0].Currency != "USD" || got[0].ThresholdCents != 100000 {
		t.Errorf("thresholds[0] = %+v, want cust-123/USD/100000", got[0])
	}
	if got[1].ThresholdCents != -5000 {
		t.Errorf("thresholds[1].ThresholdCents = %d, want -5000", got[1].ThresholdCents)
	}
	if _, err := ParseLowBalanceThresholds(""); err != nil {
		t.Fatalf("empty: %v", err)
	}
	for _, bad := range []string{"cust-123:USD", "cust-123:US:100", ":USD:100", "cust-123:USD:abc"} {
		if _, err := ParseLowBalanceThresholds(bad); err == nil {
			t.Errorf("ParseLowBalanceThresholds(%q): want error, got nil", bad)
		}
	}
}
