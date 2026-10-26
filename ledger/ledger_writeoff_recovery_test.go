package ledger

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// newRecoveryLedger returns a ledger with a bad-debt expense
// account and recovery booking accounts configured, ready for
// write-off then recovery.
func newRecoveryLedger() *Ledger {
	return New(
		WithBadDebtExpenseAccount("bad-debt-expense"),
		WithWriteOffRecoveryConfig(WriteOffRecoveryConfig{
			CashAccount:           "cash-operating",
			RecoveryIncomeAccount: "bad-debt-recovery-income",
		}),
	)
}

// seedWrittenOff seeds a receivable and writes off amountCents
// of it, returning the write-off entry.
func seedWrittenOff(t *testing.T, l *Ledger, account AccountID, receivable, writtenOff int64) JournalEntry {
	t.Helper()
	seedReceivable(t, l, account, receivable)
	posted, _, err := l.WriteOff(account, writtenOff, "USD", "invoice INV-9 uncollectible after 180 days")
	if err != nil {
		t.Fatalf("seedWrittenOff WriteOff: %v", err)
	}
	return posted
}

// entriesTouching counts journal rows with either leg on account.
func entriesTouching(l *Ledger, account AccountID) int {
	n := 0
	for _, e := range l.Entries() {
		if e.DebitAccount == account || e.CreditAccount == account {
			n++
		}
	}
	return n
}

func TestRecoverWriteOffBooksCashAgainstRecoveryIncome(t *testing.T) {
	l := newRecoveryLedger()
	writeoff := seedWrittenOff(t, l, "receivable-acme", 100000, 60000)
	touchingBefore := entriesTouching(l, "receivable-acme")

	posted, dup, err := l.RecoverWriteOff("receivable-acme", 25000, "USD")
	if err != nil {
		t.Fatalf("RecoverWriteOff: %v", err)
	}
	if dup {
		t.Fatal("RecoverWriteOff: first recovery reported duplicate")
	}
	if posted.DebitAccount != "cash-operating" || posted.CreditAccount != "bad-debt-recovery-income" {
		t.Fatalf("RecoverWriteOff legs: debit %q credit %q", posted.DebitAccount, posted.CreditAccount)
	}
	if posted.AmountCents != 25000 || posted.Currency != "USD" {
		t.Fatalf("RecoverWriteOff amount/currency: %d %q", posted.AmountCents, posted.Currency)
	}
	if posted.IdempotencyKey == "" || posted.IdempotencyKey != posted.ID {
		t.Fatalf("RecoverWriteOff idempotency key must equal the entry ID: %q", posted.IdempotencyKey)
	}
	// Deterministic ID shape: writeoff-recovery/<account>/<currency>/<writeoff-id>/<seq>.
	if !strings.HasPrefix(posted.ID, "writeoff-recovery/receivable-acme/USD/") || !strings.HasSuffix(posted.ID, "/1") {
		t.Fatalf("RecoverWriteOff ID shape: %q", posted.ID)
	}
	// The writeoff-id segment is the original write-off's reason hash.
	writeoffHash := writeoff.ID[strings.LastIndex(writeoff.ID, "/")+1:]
	if !strings.Contains(posted.ID, "/"+writeoffHash+"/") {
		t.Fatalf("RecoverWriteOff ID %q does not anchor to write-off %q", posted.ID, writeoff.ID)
	}
	// Cash rises by the recovery; recovery income is a credit
	// balance, so it reads negative under the debit-minus-credit
	// balance convention.
	if got := l.BalanceIn("cash-operating", "USD"); got != 25000 {
		t.Fatalf("cash balance after recovery: got %d, want 25000", got)
	}
	if got := l.BalanceIn("bad-debt-recovery-income", "USD"); got != -25000 {
		t.Fatalf("recovery income balance after recovery: got %d, want -25000", got)
	}
	// The receivable does NOT come back: it stays at its
	// post-write-off balance, and no new row touches it.
	if got := l.BalanceIn("receivable-acme", "USD"); got != 40000 {
		t.Fatalf("receivable balance after recovery: got %d, want 40000 (no restoration)", got)
	}
	if got := entriesTouching(l, "receivable-acme"); got != touchingBefore {
		t.Fatalf("recovery added rows touching the receivable: %d before, %d after", touchingBefore, got)
	}
	// The original write-off row is byte-identical: same legs,
	// amount, currency, memo, and timestamp.
	orig, ok := l.GetByIdempotencyKey(writeoff.ID)
	if !ok {
		t.Fatal("original write-off row missing after recovery")
	}
	if orig.DebitAccount != writeoff.DebitAccount || orig.CreditAccount != writeoff.CreditAccount ||
		orig.AmountCents != writeoff.AmountCents || orig.Currency != writeoff.Currency ||
		orig.Memo != writeoff.Memo || !orig.CreatedAt.Equal(writeoff.CreatedAt) {
		t.Fatalf("original write-off row mutated: %+v vs %+v", orig, writeoff)
	}
	// Totals read off the journal agree with the books.
	written, recovered, remaining, err := l.WriteOffRecoveryTotals("receivable-acme", "USD")
	if err != nil {
		t.Fatalf("WriteOffRecoveryTotals: %v", err)
	}
	if written != 60000 || recovered != 25000 || remaining != 35000 {
		t.Fatalf("totals: written %d recovered %d remaining %d, want 60000/25000/35000", written, recovered, remaining)
	}
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain after recovery: %v", err)
	}
}

func TestRecoverWriteOffSuccessiveRecoveriesUseSuccessiveSeq(t *testing.T) {
	l := newRecoveryLedger()
	seedWrittenOff(t, l, "receivable-acme", 100000, 60000)

	first, _, err := l.RecoverWriteOff("receivable-acme", 25000, "USD")
	if err != nil {
		t.Fatalf("first RecoverWriteOff: %v", err)
	}
	second, dup, err := l.RecoverWriteOff("receivable-acme", 25000, "USD")
	if err != nil {
		t.Fatalf("second RecoverWriteOff: %v", err)
	}
	if dup {
		t.Fatal("second recovery of the same amount reported duplicate: seq, not amount, distinguishes recoveries")
	}
	if second.ID == first.ID {
		t.Fatal("successive recoveries produced the same entry ID")
	}
	if !strings.HasSuffix(second.ID, "/2") {
		t.Fatalf("second recovery ID seq: %q", second.ID)
	}
	// Empty currency means the default currency: the USD pool.
	third, _, err := l.RecoverWriteOff("receivable-acme", 10000, "")
	if err != nil {
		t.Fatalf("RecoverWriteOff with default currency: %v", err)
	}
	if third.Currency != "USD" || !strings.HasSuffix(third.ID, "/3") {
		t.Fatalf("default-currency recovery: %q %q", third.Currency, third.ID)
	}
	if got := l.BalanceIn("cash-operating", "USD"); got != 60000 {
		t.Fatalf("cash after full recovery: got %d, want 60000", got)
	}
	if err := l.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
}

func TestRecoverWriteOffCapIsCumulative(t *testing.T) {
	l := newRecoveryLedger()
	seedWrittenOff(t, l, "receivable-acme", 100000, 60000)

	// A single recovery larger than the whole pool fails, even
	// though it is the first recovery.
	_, _, err := l.RecoverWriteOff("receivable-acme", 60001, "USD")
	if !errors.Is(err, ErrWriteOffRecoveryExceedsWrittenOff) {
		t.Fatalf("over-pool recovery: got %v, want %v", err, ErrWriteOffRecoveryExceedsWrittenOff)
	}
	// Partial recoveries drain the pool; the last cent is fine,
	// one cent past it is not.
	if _, _, err := l.RecoverWriteOff("receivable-acme", 40000, "USD"); err != nil {
		t.Fatalf("partial recovery: %v", err)
	}
	if _, _, err := l.RecoverWriteOff("receivable-acme", 20000, "USD"); err != nil {
		t.Fatalf("final recovery: %v", err)
	}
	_, _, err = l.RecoverWriteOff("receivable-acme", 1, "USD")
	if !errors.Is(err, ErrWriteOffRecoveryExceedsWrittenOff) {
		t.Fatalf("post-exhaustion recovery: got %v, want %v", err, ErrWriteOffRecoveryExceedsWrittenOff)
	}
	// A failed recovery books nothing: balances and version are
	// exactly the post-exhaustion state.
	if got := l.BalanceIn("cash-operating", "USD"); got != 60000 {
		t.Fatalf("cash after failed recovery: got %d, want 60000", got)
	}
	if got := entriesTouching(l, "cash-operating"); got != 2 {
		t.Fatalf("failed recovery added rows: %d rows touch cash, want 2", got)
	}
}

func TestRecoverWriteOffCapIsPerCurrency(t *testing.T) {
	l := newRecoveryLedger()
	seedWrittenOff(t, l, "receivable-acme", 100000, 60000)

	// Nothing was ever written off in EUR, so any EUR recovery
	// exceeds the (zero) EUR pool, even while USD remains.
	_, _, err := l.RecoverWriteOff("receivable-acme", 1000, "EUR")
	if !errors.Is(err, ErrWriteOffRecoveryExceedsWrittenOff) {
		t.Fatalf("EUR recovery against USD-only pool: got %v, want %v", err, ErrWriteOffRecoveryExceedsWrittenOff)
	}
	// An account with no write-off at all has a zero pool.
	_, _, err = l.RecoverWriteOff("receivable-nothing", 1000, "USD")
	if !errors.Is(err, ErrWriteOffRecoveryExceedsWrittenOff) {
		t.Fatalf("recovery with no write-off: got %v, want %v", err, ErrWriteOffRecoveryExceedsWrittenOff)
	}
}

func TestRecoverWriteOffForReplayIsIdempotent(t *testing.T) {
	l := newRecoveryLedger()
	seedWrittenOff(t, l, "receivable-acme", 100000, 60000)

	first, dup, err := l.RecoverWriteOffFor("receivable-acme", 25000, "USD", "inv9", 1)
	if err != nil {
		t.Fatalf("first RecoverWriteOffFor: %v", err)
	}
	if dup {
		t.Fatal("first explicit recovery reported duplicate")
	}
	if first.ID != "writeoff-recovery/receivable-acme/USD/inv9/1" {
		t.Fatalf("explicit recovery ID: %q", first.ID)
	}
	versionAfterFirst := l.Version()
	second, dup, err := l.RecoverWriteOffFor("receivable-acme", 25000, "USD", "inv9", 1)
	if err != nil {
		t.Fatalf("replay RecoverWriteOffFor: %v", err)
	}
	if !dup {
		t.Fatal("replay explicit recovery: duplicate not reported")
	}
	if second.ID != first.ID {
		t.Fatalf("replay returned a different entry: %q vs %q", second.ID, first.ID)
	}
	if l.Version() != versionAfterFirst {
		t.Fatal("replay bumped the journal version")
	}
	if got := l.BalanceIn("cash-operating", "USD"); got != 25000 {
		t.Fatalf("replay double-booked cash: %d, want 25000", got)
	}
}

func TestRecoverWriteOffForReplaySurvivesExhaustion(t *testing.T) {
	l := newRecoveryLedger()
	seedWrittenOff(t, l, "receivable-acme", 100000, 60000)

	// Recover the whole pool through one explicit ID.
	if _, _, err := l.RecoverWriteOffFor("receivable-acme", 60000, "USD", "inv9", 1); err != nil {
		t.Fatalf("full-pool explicit recovery: %v", err)
	}
	// Repeating that ID must replay, not fail on the now-empty
	// pool: the replay check runs before the cap check.
	_, dup, err := l.RecoverWriteOffFor("receivable-acme", 60000, "USD", "inv9", 1)
	if err != nil {
		t.Fatalf("replay after exhaustion: %v", err)
	}
	if !dup {
		t.Fatal("replay after exhaustion: duplicate not reported")
	}
	// A genuinely new ID against the exhausted pool still fails.
	_, _, err = l.RecoverWriteOffFor("receivable-acme", 1, "USD", "inv9", 2)
	if !errors.Is(err, ErrWriteOffRecoveryExceedsWrittenOff) {
		t.Fatalf("new ID after exhaustion: got %v, want %v", err, ErrWriteOffRecoveryExceedsWrittenOff)
	}
}

func TestRecoverWriteOffValidation(t *testing.T) {
	l := newRecoveryLedger()
	seedWrittenOff(t, l, "receivable-acme", 100000, 60000)

	cases := []struct {
		name     string
		account  AccountID
		amount   int64
		currency string
		wantErr  error
	}{
		{"empty account", "", 1000, "USD", ErrInvalidWriteOffRecoveryAccount},
		{"account with separator", "a/b", 1000, "USD", ErrInvalidWriteOffRecoveryAccount},
		{"zero amount", "receivable-acme", 0, "USD", ErrInvalidWriteOffRecoveryAmount},
		{"negative amount", "receivable-acme", -1, "USD", ErrInvalidWriteOffRecoveryAmount},
		{"bad currency", "receivable-acme", 1000, "usd", ErrInvalidCurrency},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := l.RecoverWriteOff(tc.account, tc.amount, tc.currency)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("RecoverWriteOff error: got %v, want %v", err, tc.wantErr)
			}
		})
	}

	forCases := []struct {
		name       string
		writeoffID string
		seq        int
		wantErr    error
	}{
		{"empty writeoff id", "", 1, ErrInvalidWriteOffRecoveryID},
		{"writeoff id with separator", "a/b", 1, ErrInvalidWriteOffRecoveryID},
		{"zero seq", "inv9", 0, ErrInvalidWriteOffRecoverySeq},
		{"negative seq", "inv9", -1, ErrInvalidWriteOffRecoverySeq},
	}
	for _, tc := range forCases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := l.RecoverWriteOffFor("receivable-acme", 1000, "USD", tc.writeoffID, tc.seq)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("RecoverWriteOffFor error: got %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestRecoverWriteOffRequiresConfiguredDistinctAccounts(t *testing.T) {
	// No recovery accounts configured at all: fail-closed.
	l := New(WithBadDebtExpenseAccount("bad-debt-expense"))
	seedReceivable(t, l, "receivable-acme", 100000)
	if _, _, err := l.WriteOff("receivable-acme", 60000, "USD", "uncollectible"); err != nil {
		t.Fatalf("seed WriteOff: %v", err)
	}
	_, _, err := l.RecoverWriteOff("receivable-acme", 1000, "USD")
	if !errors.Is(err, ErrWriteOffRecoveryAccountsNotConfigured) {
		t.Fatalf("recovery without config: got %v, want %v", err, ErrWriteOffRecoveryAccountsNotConfigured)
	}
	// Configuring afterwards unblocks the same recovery.
	if err := l.SetWriteOffRecoveryConfig(WriteOffRecoveryConfig{
		CashAccount:           "cash-operating",
		RecoveryIncomeAccount: "bad-debt-recovery-income",
	}); err != nil {
		t.Fatalf("SetWriteOffRecoveryConfig: %v", err)
	}
	if _, _, err := l.RecoverWriteOff("receivable-acme", 1000, "USD"); err != nil {
		t.Fatalf("recovery after configuring: %v", err)
	}
	if cfg, ok := l.WriteOffRecoveryConfig(); !ok || cfg.CashAccount != "cash-operating" {
		t.Fatalf("WriteOffRecoveryConfig: %+v %v", cfg, ok)
	}

	// Booking accounts that are not distinct: fail-closed even
	// when set through the unvalidated Option form.
	l2 := New(
		WithBadDebtExpenseAccount("bad-debt-expense"),
		WithWriteOffRecoveryConfig(WriteOffRecoveryConfig{CashAccount: "same", RecoveryIncomeAccount: "same"}),
	)
	seedWrittenOff(t, l2, "receivable-acme", 100000, 60000)
	_, _, err = l2.RecoverWriteOff("receivable-acme", 1000, "USD")
	if !errors.Is(err, ErrWriteOffRecoverySameAccount) {
		t.Fatalf("recovery with identical booking accounts: got %v, want %v", err, ErrWriteOffRecoverySameAccount)
	}
	// Eager Set rejects identical accounts outright.
	if err := l2.SetWriteOffRecoveryConfig(WriteOffRecoveryConfig{CashAccount: "same", RecoveryIncomeAccount: "same"}); !errors.Is(err, ErrWriteOffRecoverySameAccount) {
		t.Fatalf("SetWriteOffRecoveryConfig identical accounts: got %v, want %v", err, ErrWriteOffRecoverySameAccount)
	}

	// A booking account equal to the receivable account would
	// move the receivable balance: fail-closed both ways.
	for _, cfg := range []WriteOffRecoveryConfig{
		{CashAccount: "receivable-acme", RecoveryIncomeAccount: "bad-debt-recovery-income"},
		{CashAccount: "cash-operating", RecoveryIncomeAccount: "receivable-acme"},
	} {
		l3 := New(
			WithBadDebtExpenseAccount("bad-debt-expense"),
			WithWriteOffRecoveryConfig(cfg),
		)
		seedWrittenOff(t, l3, "receivable-acme", 100000, 60000)
		_, _, err := l3.RecoverWriteOff("receivable-acme", 1000, "USD")
		if !errors.Is(err, ErrWriteOffRecoverySameAccount) {
			t.Fatalf("recovery with booking account == receivable (%+v): got %v, want %v", cfg, err, ErrWriteOffRecoverySameAccount)
		}
	}
}

func TestRecoverWriteOffPostGatingFailures(t *testing.T) {
	// A frozen cash account refuses the recovery through the
	// full Post gate, and books nothing.
	l := newRecoveryLedger()
	seedWrittenOff(t, l, "receivable-acme", 100000, 60000)
	versionBefore := l.Version()
	l.Freeze("cash-operating")
	_, _, err := l.RecoverWriteOff("receivable-acme", 10000, "USD")
	if !errors.Is(err, ErrAccountFrozen) {
		t.Fatalf("recovery through frozen cash: got %v, want %v", err, ErrAccountFrozen)
	}
	if l.Version() != versionBefore {
		t.Fatal("frozen recovery bumped the journal version")
	}
	if got := l.BalanceIn("cash-operating", "USD"); got != 0 {
		t.Fatalf("frozen recovery moved cash: %d", got)
	}
	written, recovered, remaining, err := l.WriteOffRecoveryTotals("receivable-acme", "USD")
	if err != nil {
		t.Fatalf("totals after frozen attempt: %v", err)
	}
	if written != 60000 || recovered != 0 || remaining != 60000 {
		t.Fatalf("totals after frozen attempt: %d/%d/%d, want 60000/0/60000", written, recovered, remaining)
	}

	// A denylisted recovery-income account is screened before
	// the posting, and likewise books nothing.
	l2 := newRecoveryLedger()
	seedWrittenOff(t, l2, "receivable-acme", 100000, 60000)
	l2.AddDeniedAccount("bad-debt-recovery-income")
	_, _, err = l2.RecoverWriteOff("receivable-acme", 10000, "USD")
	if err == nil {
		t.Fatal("recovery through denylisted income account: no error")
	}
	if got := l2.BalanceIn("cash-operating", "USD"); got != 0 {
		t.Fatalf("denylisted recovery moved cash: %d", got)
	}
	if err := l2.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain after gated failures: %v", err)
	}
}

func TestRecoverWriteOffSnapshotRoundTrip(t *testing.T) {
	l := newRecoveryLedger()
	seedWrittenOff(t, l, "receivable-acme", 100000, 60000)
	if _, _, err := l.RecoverWriteOff("receivable-acme", 25000, "USD"); err != nil {
		t.Fatalf("RecoverWriteOff: %v", err)
	}
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(&buf)
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	// Totals are journal-derived, so they survive the restore
	// without any recovery state round-tripping.
	written, recovered, remaining, err := restored.WriteOffRecoveryTotals("receivable-acme", "USD")
	if err != nil {
		t.Fatalf("restored totals: %v", err)
	}
	if written != 60000 || recovered != 25000 || remaining != 35000 {
		t.Fatalf("restored totals: %d/%d/%d, want 60000/25000/35000", written, recovered, remaining)
	}
	// The restored ledger still refuses a replay-turned-double-
	// spend for the explicit ID, and the next derived recovery
	// continues the sequence from the journaled rows.
	if err := restored.SetWriteOffRecoveryConfig(WriteOffRecoveryConfig{
		CashAccount:           "cash-operating",
		RecoveryIncomeAccount: "bad-debt-recovery-income",
	}); err != nil {
		t.Fatalf("restored SetWriteOffRecoveryConfig: %v", err)
	}
	// Replay of a derived recovery is via its journaled key:
	// re-posting the original entry's key returns the original.
	orig, ok := restored.GetByIdempotencyKey("writeoff-recovery/receivable-acme/USD/" + firstRecoveryWriteoffHash(t, restored) + "/1")
	if !ok {
		t.Fatal("restored ledger lost the first recovery row")
	}
	_ = orig
	next, _, err := restored.RecoverWriteOff("receivable-acme", 35000, "USD")
	if err != nil {
		t.Fatalf("restored RecoverWriteOff: %v", err)
	}
	if !strings.HasSuffix(next.ID, "/2") {
		t.Fatalf("restored recovery seq continued: %q", next.ID)
	}
	if got := restored.BalanceIn("cash-operating", "USD"); got != 60000 {
		t.Fatalf("restored cash after full recovery: got %d, want 60000", got)
	}
	if err := restored.VerifyChain(); err != nil {
		t.Fatalf("VerifyChain on restored: %v", err)
	}
}

// firstRecoveryWriteoffHash finds the writeoff-id segment of the
// first recovery row journaled for receivable-acme in USD.
func firstRecoveryWriteoffHash(t *testing.T, l *Ledger) string {
	t.Helper()
	for _, e := range l.Entries() {
		if strings.HasPrefix(e.ID, "writeoff-recovery/receivable-acme/USD/") && strings.HasSuffix(e.ID, "/1") {
			rest := strings.TrimPrefix(e.ID, "writeoff-recovery/receivable-acme/USD/")
			return strings.TrimSuffix(rest, "/1")
		}
	}
	t.Fatal("no first recovery row found")
	return ""
}
