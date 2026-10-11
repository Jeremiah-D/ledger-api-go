package ledger

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func TestRegisterAccountTypeValidation(t *testing.T) {
	l := New()
	if err := l.RegisterAccountType("", AccountTypeAsset); !errors.Is(err, ErrEmptyChartAccount) {
		t.Fatalf("empty account = %v, want ErrEmptyChartAccount", err)
	}
	if err := l.RegisterAccountType("cash", "real-estate"); !errors.Is(err, ErrInvalidAccountType) {
		t.Fatalf("bad type = %v, want ErrInvalidAccountType", err)
	}
	if _, ok := l.AccountTypeOf("cash"); ok {
		t.Fatal("failed registration must not leave a row")
	}
}

func TestRegisterAccountTypeReplaceAndClear(t *testing.T) {
	l := New()
	if err := l.RegisterAccountType("cash", AccountTypeAsset); err != nil {
		t.Fatalf("register: %v", err)
	}
	if got, ok := l.AccountTypeOf("cash"); !ok || got != AccountTypeAsset {
		t.Fatalf("AccountTypeOf = (%q, %v), want (asset, true)", got, ok)
	}
	// Re-registering replaces the type.
	if err := l.RegisterAccountType("cash", AccountTypeExpense); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if got, ok := l.AccountTypeOf("cash"); !ok || got != AccountTypeExpense {
		t.Fatalf("after re-register = (%q, %v), want (expense, true)", got, ok)
	}
	l.ClearAccountType("cash")
	if _, ok := l.AccountTypeOf("cash"); ok {
		t.Fatal("cleared account must report unregistered")
	}
	l.ClearAccountType("never-registered") // no-op, must not panic
}

func TestChartDirectionCheck(t *testing.T) {
	l := New()
	for a, typ := range map[AccountID]AccountType{
		"cash":    AccountTypeAsset,
		"loan":    AccountTypeLiability,
		"equity":  AccountTypeEquity,
		"revenue": AccountTypeRevenue,
		"supply":  AccountTypeExpense,
	} {
		if err := l.RegisterAccountType(a, typ); err != nil {
			t.Fatalf("register %s: %v", a, err)
		}
	}

	// Normal directions: debit an asset/expense, credit a
	// liability/equity/revenue.
	for _, e := range []JournalEntry{
		{ID: "ok-1", DebitAccount: "cash", CreditAccount: "loan", AmountCents: 100},
		{ID: "ok-2", DebitAccount: "supply", CreditAccount: "revenue", AmountCents: 100},
		{ID: "ok-3", DebitAccount: "cash", CreditAccount: "equity", AmountCents: 100},
	} {
		if _, _, err := l.Post(e); err != nil {
			t.Fatalf("normal-direction post %s: %v", e.ID, err)
		}
	}
}

func TestChartDirectionRejects(t *testing.T) {
	l := New()
	for a, typ := range map[AccountID]AccountType{
		"cash":    AccountTypeAsset,
		"loan":    AccountTypeLiability,
		"equity":  AccountTypeEquity,
		"revenue": AccountTypeRevenue,
		"supply":  AccountTypeExpense,
	} {
		if err := l.RegisterAccountType(a, typ); err != nil {
			t.Fatalf("register %s: %v", a, err)
		}
	}
	cases := []struct {
		name  string
		debit AccountID
		cred  AccountID
	}{
		{"debit liability", "loan", "revenue"},
		{"debit equity", "equity", "loan"},
		{"debit revenue", "revenue", "loan"},
		{"credit asset", "supply", "cash"},
		{"credit expense", "cash", "supply"},
	}
	verBefore := l.Version()
	for i, c := range cases {
		e := JournalEntry{
			ID:            "rej-" + string(rune('a'+i)),
			DebitAccount:  c.debit,
			CreditAccount: c.cred,
			AmountCents:   100,
		}
		if _, _, err := l.Post(e); !errors.Is(err, ErrAccountDirectionMismatch) {
			t.Fatalf("%s: Post = %v, want ErrAccountDirectionMismatch", c.name, err)
		}
	}
	// Rejected posts record nothing: no journal rows, no chain link, no
	// version bump.
	if l.Version() != verBefore {
		t.Fatalf("version moved on rejected posts: %d -> %d", verBefore, l.Version())
	}
	if n := len(l.Entries()); n != 0 {
		t.Fatalf("rejected posts recorded %d entries", n)
	}
}

func TestChartUnregisteredSkipsCheck(t *testing.T) {
	l := New()
	if err := l.RegisterAccountType("cash", AccountTypeAsset); err != nil {
		t.Fatal(err)
	}
	// Both legs unregistered: any direction books.
	e := JournalEntry{ID: "u-1", DebitAccount: "suspense", CreditAccount: "clearing", AmountCents: 50}
	if _, _, err := l.Post(e); err != nil {
		t.Fatalf("unregistered post: %v", err)
	}
	// One leg registered, other direction-clean: the unregistered leg is
	// skipped, the registered leg is checked.
	e2 := JournalEntry{ID: "u-2", DebitAccount: "cash", CreditAccount: "suspense", AmountCents: 50}
	if _, _, err := l.Post(e2); err != nil {
		t.Fatalf("half-registered post: %v", err)
	}
	// Registered leg in the wrong direction still fails.
	e3 := JournalEntry{ID: "u-3", DebitAccount: "suspense", CreditAccount: "cash", AmountCents: 50}
	if _, _, err := l.Post(e3); !errors.Is(err, ErrAccountDirectionMismatch) {
		t.Fatalf("credit-asset post = %v, want ErrAccountDirectionMismatch", err)
	}
}

func TestChartReplayExemptFromDirectionCheck(t *testing.T) {
	l := New()
	// Post with the key before any registration exists.
	e := JournalEntry{ID: "rp-1", DebitAccount: "loan", CreditAccount: "revenue", AmountCents: 100, IdempotencyKey: "rp-key"}
	if _, dup, err := l.Post(e); err != nil || dup {
		t.Fatalf("initial post = dup=%v err=%v", dup, err)
	}
	// Register a type that would reject this entry as a fresh posting.
	if err := l.RegisterAccountType("loan", AccountTypeLiability); err != nil {
		t.Fatal(err)
	}
	// The replay books nothing new, so it returns the original entry
	// instead of failing the direction check — mirroring the frozen and
	// overdraft replay-first order.
	retry := JournalEntry{ID: "rp-2", DebitAccount: "loan", CreditAccount: "revenue", AmountCents: 100, IdempotencyKey: "rp-key"}
	got, dup, err := l.Post(retry)
	if err != nil || !dup {
		t.Fatalf("replay = dup=%v err=%v, want dup=true", dup, err)
	}
	if got.ID != "rp-1" {
		t.Fatalf("replay returned entry %q, want original rp-1", got.ID)
	}
}

func TestChartBatchDirectionCheck(t *testing.T) {
	l := New()
	if err := l.RegisterAccountType("cash", AccountTypeAsset); err != nil {
		t.Fatal(err)
	}
	b := Batch{
		ID: "batch-chart",
		Entries: []JournalEntry{
			{ID: "bc-1", DebitAccount: "supply", CreditAccount: "cash", AmountCents: 10}, // credit asset
		},
	}
	if _, err := l.PostBatch(b); !errors.Is(err, ErrAccountDirectionMismatch) {
		t.Fatalf("PostBatch = %v, want ErrAccountDirectionMismatch", err)
	}
	if l.Version() != 0 {
		t.Fatalf("rejected batch bumped version to %d", l.Version())
	}
}

func TestTrialBalanceCarriesAccountType(t *testing.T) {
	l := New()
	if err := l.RegisterAccountType("cash", AccountTypeAsset); err != nil {
		t.Fatal(err)
	}
	tb := l.TrialBalance("cash")
	if tb.AccountType != AccountTypeAsset {
		t.Fatalf("TrialBalance(cash).AccountType = %q, want asset", tb.AccountType)
	}
	tb2 := l.TrialBalance("unregistered")
	if tb2.AccountType != "" {
		t.Fatalf("unregistered AccountType = %q, want empty", tb2.AccountType)
	}
}

func TestReconcileAccountTypeTotals(t *testing.T) {
	l := New()
	for a, typ := range map[AccountID]AccountType{
		"cash":    AccountTypeAsset,
		"loan":    AccountTypeLiability,
		"revenue": AccountTypeRevenue,
		"supply":  AccountTypeExpense,
	} {
		if err := l.RegisterAccountType(a, typ); err != nil {
			t.Fatalf("register %s: %v", a, err)
		}
	}
	posts := []JournalEntry{
		{ID: "t-1", DebitAccount: "cash", CreditAccount: "loan", AmountCents: 10000},
		{ID: "t-2", DebitAccount: "supply", CreditAccount: "loan", AmountCents: 2500},
		{ID: "t-3", DebitAccount: "cash", CreditAccount: "revenue", AmountCents: 4000},
	}
	for _, e := range posts {
		if _, _, err := l.Post(e); err != nil {
			t.Fatalf("Post %s: %v", e.ID, err)
		}
	}
	report := l.Reconcile(time.Now())
	want := map[AccountType]struct{ d, c, net int64 }{
		AccountTypeAsset:     {d: 14000, c: 0, net: 14000},
		AccountTypeLiability: {d: 0, c: 12500, net: -12500},
		AccountTypeRevenue:   {d: 0, c: 4000, net: -4000},
		AccountTypeExpense:   {d: 2500, c: 0, net: 2500},
	}
	if len(report.AccountTypeTotals) != len(want) {
		t.Fatalf("AccountTypeTotals has %d rows, want %d", len(report.AccountTypeTotals), len(want))
	}
	for _, row := range report.AccountTypeTotals {
		w, ok := want[row.Type]
		if !ok {
			t.Fatalf("unexpected type row %q", row.Type)
		}
		if row.Currency != DefaultCurrency {
			t.Fatalf("row %q currency = %q, want default", row.Type, row.Currency)
		}
		if row.TotalDebitsCents != w.d || row.TotalCreditsCents != w.c || row.NetBalanceCents != w.net {
			t.Fatalf("row %q = (%d, %d, %d), want (%d, %d, %d)",
				row.Type, row.TotalDebitsCents, row.TotalCreditsCents, row.NetBalanceCents, w.d, w.c, w.net)
		}
	}
	// Sorted by (type, currency).
	for i := 1; i < len(report.AccountTypeTotals); i++ {
		a, b := report.AccountTypeTotals[i-1], report.AccountTypeTotals[i]
		if a.Type > b.Type || (a.Type == b.Type && a.Currency > b.Currency) {
			t.Fatal("AccountTypeTotals not sorted by (type, currency)")
		}
	}
	// The books still balance with the chart registered.
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Fatalf("VerifyAccountingEquation: %v", err)
	}
}

func TestChartSnapshotRoundTrip(t *testing.T) {
	src := New()
	if err := src.RegisterAccountType("cash", AccountTypeAsset); err != nil {
		t.Fatal(err)
	}
	if err := src.RegisterAccountType("loan", AccountTypeLiability); err != nil {
		t.Fatal(err)
	}
	e := JournalEntry{ID: "cs-1", DebitAccount: "cash", CreditAccount: "loan", AmountCents: 777}
	if _, _, err := src.Post(e); err != nil {
		t.Fatalf("Post: %v", err)
	}

	var buf bytes.Buffer
	if err := src.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if got, ok := restored.AccountTypeOf("cash"); !ok || got != AccountTypeAsset {
		t.Fatalf("restored cash type = (%q, %v), want (asset, true)", got, ok)
	}
	if got, ok := restored.AccountTypeOf("loan"); !ok || got != AccountTypeLiability {
		t.Fatalf("restored loan type = (%q, %v), want (liability, true)", got, ok)
	}
	// The direction control survives the restore.
	bad := JournalEntry{ID: "cs-2", DebitAccount: "loan", CreditAccount: "cash", AmountCents: 1}
	if _, _, err := restored.Post(bad); !errors.Is(err, ErrAccountDirectionMismatch) {
		t.Fatalf("restored Post = %v, want ErrAccountDirectionMismatch", err)
	}
	if !snapshotLedgersEqual(src, restored) {
		t.Fatal("restored ledger differs from source")
	}
}

func TestChartSnapshotImportRejectsBadType(t *testing.T) {
	src := New()
	if err := src.RegisterAccountType("cash", AccountTypeAsset); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := src.ExportSnapshot(&buf); err != nil {
		t.Fatal(err)
	}
	// Tamper the exported account_types row with an invalid type.
	tampered := bytes.Replace(buf.Bytes(), []byte(`"type":"asset"`), []byte(`"type":"planet"`), 1)
	if _, err := ImportSnapshot(bytes.NewReader(tampered)); err == nil {
		t.Fatal("import of tampered chart type succeeded, want failure")
	}
}
