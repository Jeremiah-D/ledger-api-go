package ledger

import (
	"errors"
	"testing"
)

// seedClientMoneyBalance funds account with cents in currency by
// posting debit account / credit "funding": the funding leg is
// unmarked, so callers must seed before marking the account, or
// seed a same-class source, or the segregation gate rejects the
// seed itself.
func seedClientMoneyBalance(t *testing.T, l *Ledger, account AccountID, cents int64, currency string) {
	t.Helper()
	if _, _, err := l.Post(JournalEntry{
		ID:            "seed-" + string(account) + "-" + currency,
		DebitAccount:  account,
		CreditAccount: "funding",
		AmountCents:   cents,
		Currency:      currency,
	}); err != nil {
		t.Fatalf("seed %s %s: %v", account, currency, err)
	}
}

func TestMarkAndUnmarkClientMoneyAccount(t *testing.T) {
	l := New()
	if err := l.MarkClientMoneyAccount(""); err != ErrEmptyClientMoneyAccount {
		t.Fatalf("empty mark err = %v, want ErrEmptyClientMoneyAccount", err)
	}
	if err := l.UnmarkClientMoneyAccount(""); err != ErrEmptyClientMoneyAccount {
		t.Fatalf("empty unmark err = %v, want ErrEmptyClientMoneyAccount", err)
	}
	if l.IsClientMoneyAccount("client-acme") {
		t.Fatal("unmarked account reported as client money")
	}
	if err := l.MarkClientMoneyAccount("client-beta"); err != nil {
		t.Fatalf("MarkClientMoneyAccount(beta): %v", err)
	}
	if err := l.MarkClientMoneyAccount("client-acme"); err != nil {
		t.Fatalf("MarkClientMoneyAccount(acme): %v", err)
	}
	// Re-marking is an idempotent no-op, not an error.
	if err := l.MarkClientMoneyAccount("client-acme"); err != nil {
		t.Fatalf("re-mark: %v", err)
	}
	marked := l.ClientMoneyAccounts()
	if len(marked) != 2 || marked[0] != "client-acme" || marked[1] != "client-beta" {
		t.Fatalf("marked = %v, want [client-acme client-beta]", marked)
	}
	if !l.IsClientMoneyAccount("client-acme") || l.IsClientMoneyAccount("operating") {
		t.Fatal("IsClientMoneyAccount mismatch after marking")
	}
	if err := l.UnmarkClientMoneyAccount("client-acme"); err != nil {
		t.Fatalf("UnmarkClientMoneyAccount: %v", err)
	}
	// Un-marking an unmarked account is a no-op, not an error.
	if err := l.UnmarkClientMoneyAccount("client-acme"); err != nil {
		t.Fatalf("re-unmark: %v", err)
	}
	if l.IsClientMoneyAccount("client-acme") {
		t.Fatal("unmarked account still reported as client money")
	}
	if got := l.ClientMoneyAccounts(); len(got) != 1 || got[0] != "client-beta" {
		t.Fatalf("marked after unmark = %v, want [client-beta]", got)
	}
}

func TestClientMoneyCrossClassTransferRejectedZeroSideEffects(t *testing.T) {
	l := New()
	seedClientMoneyBalance(t, l, "client-acme", 100000, "USD")
	seedClientMoneyBalance(t, l, "operating", 50000, "USD")
	if err := l.MarkClientMoneyAccount("client-acme"); err != nil {
		t.Fatalf("MarkClientMoneyAccount: %v", err)
	}
	versionBefore := l.Version()
	entriesBefore := len(l.Entries())

	// Client money out to an own account.
	_, err := l.PostTransfer(Transfer{
		ID: "xfer-out", From: "client-acme", To: "operating",
		AmountCents: 10000, Currency: "USD",
	})
	if !errors.Is(err, ErrClientMoneySegregationViolation) {
		t.Fatalf("client->own err = %v, want ErrClientMoneySegregationViolation", err)
	}
	// Own money into a client account.
	_, err = l.PostTransfer(Transfer{
		ID: "xfer-in", From: "operating", To: "client-acme",
		AmountCents: 10000, Currency: "USD",
	})
	if !errors.Is(err, ErrClientMoneySegregationViolation) {
		t.Fatalf("own->client err = %v, want ErrClientMoneySegregationViolation", err)
	}
	if l.Version() != versionBefore {
		t.Fatalf("version %d -> %d after rejected transfers", versionBefore, l.Version())
	}
	if len(l.Entries()) != entriesBefore {
		t.Fatalf("entries %d -> %d after rejected transfers", entriesBefore, len(l.Entries()))
	}
	if got := l.BalanceIn("client-acme", "USD"); got != 100000 {
		t.Fatalf("client balance = %d, want 100000", got)
	}
	if got := l.BalanceIn("operating", "USD"); got != 50000 {
		t.Fatalf("own balance = %d, want 50000", got)
	}
}

func TestClientMoneySameClassTransfersAllowed(t *testing.T) {
	l := New()
	seedClientMoneyBalance(t, l, "client-acme", 100000, "USD")
	seedClientMoneyBalance(t, l, "operating", 50000, "USD")
	if err := l.MarkClientMoneyAccount("client-acme"); err != nil {
		t.Fatalf("mark acme: %v", err)
	}
	if err := l.MarkClientMoneyAccount("client-beta"); err != nil {
		t.Fatalf("mark beta: %v", err)
	}
	if _, err := l.PostTransfer(Transfer{
		ID: "xfer-client", From: "client-acme", To: "client-beta",
		AmountCents: 25000, Currency: "USD",
	}); err != nil {
		t.Fatalf("client->client transfer: %v", err)
	}
	if _, err := l.PostTransfer(Transfer{
		ID: "xfer-own", From: "operating", To: "payroll",
		AmountCents: 10000, Currency: "USD",
	}); err != nil {
		t.Fatalf("own->own transfer: %v", err)
	}
	if got := l.BalanceIn("client-acme", "USD"); got != 75000 {
		t.Fatalf("client-acme = %d, want 75000", got)
	}
	if got := l.BalanceIn("client-beta", "USD"); got != 25000 {
		t.Fatalf("client-beta = %d, want 25000", got)
	}
	if got := l.BalanceIn("operating", "USD"); got != 40000 {
		t.Fatalf("operating = %d, want 40000", got)
	}
}

func TestClientMoneyPostGateRejectsCrossClass(t *testing.T) {
	l := New()
	seedClientMoneyBalance(t, l, "client-acme", 100000, "USD")
	if err := l.MarkClientMoneyAccount("client-acme"); err != nil {
		t.Fatalf("mark: %v", err)
	}
	versionBefore := l.Version()
	// A direct journal entry is a movement too: debit client,
	// credit own must fail exactly like a transfer.
	_, _, err := l.Post(JournalEntry{
		ID: "direct-xfer", DebitAccount: "client-acme",
		CreditAccount: "operating", AmountCents: 5000, Currency: "USD",
	})
	if !errors.Is(err, ErrClientMoneySegregationViolation) {
		t.Fatalf("cross-class Post err = %v, want ErrClientMoneySegregationViolation", err)
	}
	if l.Version() != versionBefore {
		t.Fatalf("version moved after rejected Post")
	}
	// Same-class Post booking is covered by
	// TestClientMoneyPostGateSameClassAfterMark.
}

func TestClientMoneyPostGateSameClassAfterMark(t *testing.T) {
	l := New()
	seedClientMoneyBalance(t, l, "client-acme", 100000, "USD")
	if err := l.MarkClientMoneyAccount("client-acme"); err != nil {
		t.Fatalf("mark acme: %v", err)
	}
	if err := l.MarkClientMoneyAccount("client-beta"); err != nil {
		t.Fatalf("mark beta: %v", err)
	}
	if _, _, err := l.Post(JournalEntry{
		ID: "same-class", DebitAccount: "client-beta",
		CreditAccount: "client-acme", AmountCents: 5000, Currency: "USD",
	}); err != nil {
		t.Fatalf("same-class Post: %v", err)
	}
	if got := l.BalanceIn("client-beta", "USD"); got != 5000 {
		t.Fatalf("client-beta = %d, want 5000", got)
	}
}

func TestClientMoneySegregationReport(t *testing.T) {
	l := New()
	seedClientMoneyBalance(t, l, "client-acme", 100000, "USD")
	seedClientMoneyBalance(t, l, "client-acme", 40000, "EUR")
	seedClientMoneyBalance(t, l, "client-beta", 60000, "USD")
	seedClientMoneyBalance(t, l, "operating", 999999, "USD")
	if err := l.MarkClientMoneyAccount("client-acme"); err != nil {
		t.Fatalf("mark acme: %v", err)
	}
	if err := l.MarkClientMoneyAccount("client-beta"); err != nil {
		t.Fatalf("mark beta: %v", err)
	}
	report := l.SegregationReport()
	if len(report.Accounts) != 3 {
		t.Fatalf("report accounts = %d, want 3: %+v", len(report.Accounts), report.Accounts)
	}
	if report.Accounts[0].Account != "client-acme" || report.Accounts[0].Currency != "EUR" || report.Accounts[0].BalanceCents != 40000 {
		t.Fatalf("first row = %+v, want acme/EUR/40000", report.Accounts[0])
	}
	totals := map[string]SegregationCurrencyTotal{}
	for _, total := range report.Totals {
		totals[total.Currency] = total
	}
	if totals["USD"].ClientMoneyCents != 160000 || totals["USD"].RequiredCents != 160000 ||
		totals["USD"].SegregatedCents != 160000 || totals["USD"].GapCents != 0 {
		t.Fatalf("USD total = %+v, want 160000/160000/160000/0", totals["USD"])
	}
	if totals["EUR"].ClientMoneyCents != 40000 || totals["EUR"].GapCents != 0 {
		t.Fatalf("EUR total = %+v, want 40000/0 gap", totals["EUR"])
	}
	if !report.GapZero {
		t.Fatal("GapZero = false, want true")
	}
	// The own account's balance never enters the report.
	for _, row := range report.Accounts {
		if row.Account == "operating" {
			t.Fatal("own account listed in segregation report")
		}
	}
	// Un-marking drops the account (and its balance) from the report.
	if err := l.UnmarkClientMoneyAccount("client-beta"); err != nil {
		t.Fatalf("unmark beta: %v", err)
	}
	report = l.SegregationReport()
	if len(report.Accounts) != 2 {
		t.Fatalf("after unmark accounts = %d, want 2", len(report.Accounts))
	}
	if !report.GapZero {
		t.Fatal("GapZero = false after unmark, want true")
	}
}

func TestClientMoneyMarkUnmarkAudited(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	if err := l.MarkClientMoneyAccount("client-acme"); err != nil {
		t.Fatalf("mark: %v", err)
	}
	if err := l.UnmarkClientMoneyAccount("client-acme"); err != nil {
		t.Fatalf("unmark: %v", err)
	}
	events := readAuditOps(t, al)
	marked, unmarked := 0, 0
	for _, ev := range events {
		switch ev.Op {
		case "client_money_marked":
			marked++
			if len(ev.Accounts) != 1 || ev.Accounts[0] != "client-acme" {
				t.Fatalf("marked event accounts = %v", ev.Accounts)
			}
		case "client_money_unmarked":
			unmarked++
			if len(ev.Accounts) != 1 || ev.Accounts[0] != "client-acme" {
				t.Fatalf("unmarked event accounts = %v", ev.Accounts)
			}
		}
	}
	if marked != 1 || unmarked != 1 {
		t.Fatalf("marked = %d unmarked = %d, want 1/1", marked, unmarked)
	}
}

func TestClientMoneyStateGatePrecedence(t *testing.T) {
	l := New()
	seedClientMoneyBalance(t, l, "client-acme", 100000, "USD")
	seedClientMoneyBalance(t, l, "operating", 50000, "USD")
	if err := l.MarkClientMoneyAccount("client-acme"); err != nil {
		t.Fatalf("mark: %v", err)
	}
	l.Freeze("operating")
	// Frozen still wins over the segregation gate: the state gate
	// is unchanged by marking.
	_, err := l.PostTransfer(Transfer{
		ID: "xfer-frozen", From: "client-acme", To: "operating",
		AmountCents: 10000, Currency: "USD",
	})
	if !errors.Is(err, ErrAccountFrozen) {
		t.Fatalf("frozen+cross-class err = %v, want ErrAccountFrozen", err)
	}
}

func TestClientMoneyCrossCurrencyTransferRejected(t *testing.T) {
	l, _ := mustOverrideLedger(t)
	mustSetRate(t, l, "USD", "EUR", 108, 100)
	seedPayerUSD(t, l, "payer", 100000)
	if err := l.MarkClientMoneyAccount("payer"); err != nil {
		t.Fatalf("mark payer: %v", err)
	}
	_, err := l.PostTransfer(Transfer{
		ID: "xfer-fx", From: "payer", To: "payee",
		AmountCents: 10000, Currency: "USD", ToCurrency: "EUR",
	})
	if !errors.Is(err, ErrClientMoneySegregationViolation) {
		t.Fatalf("cross-class FX err = %v, want ErrClientMoneySegregationViolation", err)
	}
	if got := l.BalanceIn("payer", "USD"); got != 100000 {
		t.Fatalf("payer balance = %d, want 100000", got)
	}
}
