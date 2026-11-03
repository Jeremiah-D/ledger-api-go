package ledger

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// FTP internal counterparty accounts amendment (LG-298): the runtime
// revision of the pool/expense/revenue triple, its fail-closed gates,
// and the audit trail. The pricing and posting paths already read
// l.ftpAccounts live, so the tests pin that the revision — not a
// pricing change — moves the charge legs.

// ftpAmendTestLedger builds an FTP ledger with an audit log: the USD
// 12-month curve at 500bps and the test triple (treasury-pool /
// ftp-expense / ftp-revenue). The caller must not use the audit log
// after ftpAccountsAmendAuditEvents closes it.
func ftpAmendTestLedger(t *testing.T) (*Ledger, *AuditLog) {
	t.Helper()
	al := mustAuditLog(t)
	l := New(WithAuditLog(al), WithFtpAccounts(testFtpAccounts()))
	if _, err := l.SetFtpCurve("USD", []FtpTenorPoint{{12, 500}}); err != nil {
		t.Fatal(err)
	}
	return l, al
}

// ftpAccountsAmendAuditEvents returns the ftp_accounts_amended events
// in order, closing the audit log first.
func ftpAccountsAmendAuditEvents(t *testing.T, al *AuditLog) []AuditEvent {
	t.Helper()
	var out []AuditEvent
	for _, e := range providerAuditEvents(t, al) {
		if e.Op == "ftp_accounts_amended" {
			out = append(out, e)
		}
	}
	return out
}

// ftpAmendAccounts is the replacement triple used across the tests.
func ftpAmendAccounts() FtpAccounts {
	return FtpAccounts{
		PoolAccount:    "treasury-pool-2",
		ExpenseAccount: "ftp-expense-2",
		RevenueAccount: "ftp-revenue-2",
	}
}

func TestSetFtpAccountsAmendmentAffectsNextCharge(t *testing.T) {
	l, al := ftpAmendTestLedger(t)
	v := l.Version()

	if err := l.SetFtpAccounts(ftpAmendAccounts()); err != nil {
		t.Fatalf("SetFtpAccounts: %v", err)
	}
	if got := l.FtpAccounts(); got != ftpAmendAccounts() {
		t.Errorf("FtpAccounts() = %+v, want %+v", got, ftpAmendAccounts())
	}
	if got := l.Version(); got != v {
		t.Errorf("Version after amendment = %d, want %d (structural, no bump)", got, v)
	}

	// Gap: the unit pays — debit the NEW expense, credit the NEW pool.
	// floor(1200000 * 500/10000 * 12/12) = 60000.
	got, dup, err := l.ChargeFtp("bu-lending", "USD", 12, 1200000, ftpDay)
	if err != nil || dup {
		t.Fatalf("ChargeFtp gap: dup=%v err=%v", dup, err)
	}
	if got.DebitAccount != "ftp-expense-2" || got.CreditAccount != "treasury-pool-2" {
		t.Errorf("gap legs = %s/%s, want ftp-expense-2/treasury-pool-2", got.DebitAccount, got.CreditAccount)
	}
	if got.AmountCents != 60000 {
		t.Errorf("gap charge = %d, want 60000", got.AmountCents)
	}

	// Surplus: the pool pays — debit the NEW pool, credit the NEW
	// revenue. floor(600000 * 500/10000) = 30000.
	got, dup, err = l.ChargeFtp("bu-deposits", "USD", 12, -600000, ftpDay)
	if err != nil || dup {
		t.Fatalf("ChargeFtp surplus: dup=%v err=%v", dup, err)
	}
	if got.DebitAccount != "treasury-pool-2" || got.CreditAccount != "ftp-revenue-2" {
		t.Errorf("surplus legs = %s/%s, want treasury-pool-2/ftp-revenue-2", got.DebitAccount, got.CreditAccount)
	}
	if got.AmountCents != 30000 {
		t.Errorf("surplus charge = %d, want 30000", got.AmountCents)
	}

	// The old accounts booked nothing.
	if got := l.Balance("ftp-expense"); got != 0 {
		t.Errorf("old expense balance = %d, want 0", got)
	}
	if got := l.Balance("ftp-revenue"); got != 0 {
		t.Errorf("old revenue balance = %d, want 0", got)
	}

	events := ftpAccountsAmendAuditEvents(t, al)
	if len(events) != 1 {
		t.Fatalf("ftp_accounts_amended events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Actor != "SetFtpAccounts" {
		t.Errorf("actor = %q, want SetFtpAccounts", ev.Actor)
	}
	if ev.Details["previous_pool_account"] != "treasury-pool" ||
		ev.Details["previous_expense_account"] != "ftp-expense" ||
		ev.Details["previous_revenue_account"] != "ftp-revenue" {
		t.Errorf("audit previous = %+v, want treasury-pool/ftp-expense/ftp-revenue", ev.Details)
	}
	if ev.Details["pool_account"] != "treasury-pool-2" ||
		ev.Details["expense_account"] != "ftp-expense-2" ||
		ev.Details["revenue_account"] != "ftp-revenue-2" {
		t.Errorf("audit new = %+v, want treasury-pool-2/ftp-expense-2/ftp-revenue-2", ev.Details)
	}
	if ev.VersionBefore != v || ev.VersionAfter != v {
		t.Errorf("audit versions = %d/%d, want %d/%d", ev.VersionBefore, ev.VersionAfter, v, v)
	}
}

func TestSetFtpAccountsSameValueNoOp(t *testing.T) {
	l, al := ftpAmendTestLedger(t)
	v := l.Version()
	if err := l.SetFtpAccounts(testFtpAccounts()); err != nil {
		t.Fatalf("same-value revision: %v", err)
	}
	if got := l.FtpAccounts(); got != testFtpAccounts() {
		t.Errorf("FtpAccounts() = %+v, want %+v", got, testFtpAccounts())
	}
	if got := l.Version(); got != v {
		t.Errorf("Version after no-op = %d, want %d", got, v)
	}
	if events := ftpAccountsAmendAuditEvents(t, al); len(events) != 0 {
		t.Errorf("same-value revision wrote %d audit events, want 0", len(events))
	}
}

func TestSetFtpAccountsInvalidRejected(t *testing.T) {
	l, al := ftpAmendTestLedger(t)
	bad := []struct {
		name string
		a    FtpAccounts
	}{
		{"empty pool", FtpAccounts{"", "ftp-expense", "ftp-revenue"}},
		{"empty expense", FtpAccounts{"treasury-pool", "", "ftp-revenue"}},
		{"empty revenue", FtpAccounts{"treasury-pool", "ftp-expense", ""}},
		{"pool equals expense", FtpAccounts{"same", "same", "ftp-revenue"}},
		{"pool equals revenue", FtpAccounts{"same", "ftp-expense", "same"}},
		{"expense equals revenue", FtpAccounts{"treasury-pool", "same", "same"}},
		{"slash in pool", FtpAccounts{"trea/sury-pool", "ftp-expense", "ftp-revenue"}},
		{"slash in expense", FtpAccounts{"treasury-pool", "ftp/expense", "ftp-revenue"}},
		{"slash in revenue", FtpAccounts{"treasury-pool", "ftp-expense", "ftp/revenue"}},
	}
	for _, tc := range bad {
		if err := l.SetFtpAccounts(tc.a); !errors.Is(err, ErrInvalidFtpAccounts) {
			t.Errorf("%s: err = %v, want ErrInvalidFtpAccounts", tc.name, err)
		}
		// The original triple is untouched after every rejection.
		if got := l.FtpAccounts(); got != testFtpAccounts() {
			t.Errorf("%s: FtpAccounts() = %+v, want %+v (unchanged)", tc.name, got, testFtpAccounts())
		}
	}
	// The next charge still books the original legs.
	got, _, err := l.ChargeFtp("bu-lending", "USD", 12, 1200000, ftpDay)
	if err != nil {
		t.Fatal(err)
	}
	if got.DebitAccount != "ftp-expense" || got.CreditAccount != "treasury-pool" {
		t.Errorf("legs after rejections = %s/%s, want ftp-expense/treasury-pool", got.DebitAccount, got.CreditAccount)
	}
	if events := ftpAccountsAmendAuditEvents(t, al); len(events) != 0 {
		t.Errorf("rejected revisions wrote %d audit events, want 0", len(events))
	}
}

func TestSetFtpAccountsZeroValueRejected(t *testing.T) {
	l, al := ftpAmendTestLedger(t)
	// There is no "clear" operation: the zero triple cannot pass
	// validation, so the accounts can only ever be swapped, never
	// removed.
	if err := l.SetFtpAccounts(FtpAccounts{}); !errors.Is(err, ErrInvalidFtpAccounts) {
		t.Fatalf("zero triple: err = %v, want ErrInvalidFtpAccounts", err)
	}
	if got := l.FtpAccounts(); got != testFtpAccounts() {
		t.Errorf("FtpAccounts() = %+v, want %+v (unchanged)", got, testFtpAccounts())
	}
	if events := ftpAccountsAmendAuditEvents(t, al); len(events) != 0 {
		t.Errorf("rejected clear wrote %d audit events, want 0", len(events))
	}
}

func TestSetFtpAccountsUnconfiguredThenConfigured(t *testing.T) {
	l := New()
	if _, err := l.SetFtpCurve("USD", []FtpTenorPoint{{12, 500}}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.FtpPreview("bu", "USD", 12, 1200000); !errors.Is(err, ErrFtpAccountsNotConfigured) {
		t.Fatalf("preview without accounts: err = %v, want ErrFtpAccountsNotConfigured", err)
	}
	if _, _, err := l.ChargeFtp("bu", "USD", 12, 1200000, ftpDay); !errors.Is(err, ErrFtpAccountsNotConfigured) {
		t.Fatalf("charge without accounts: err = %v, want ErrFtpAccountsNotConfigured", err)
	}
	// Re-setting the zero triple on an unconfigured ledger is a
	// no-op (the no-op check runs before validation), not an error —
	// and the ledger stays unconfigured.
	if err := l.SetFtpAccounts(FtpAccounts{}); err != nil {
		t.Fatalf("zero triple on unconfigured ledger: %v, want nil (no-op)", err)
	}
	if _, err := l.FtpPreview("bu", "USD", 12, 1200000); !errors.Is(err, ErrFtpAccountsNotConfigured) {
		t.Fatalf("preview after zero no-op: err = %v, want ErrFtpAccountsNotConfigured", err)
	}
	// Installing a legal triple opens both preview and charge.
	if err := l.SetFtpAccounts(testFtpAccounts()); err != nil {
		t.Fatalf("SetFtpAccounts: %v", err)
	}
	p, err := l.FtpPreview("bu", "USD", 12, 1200000)
	if err != nil {
		t.Fatalf("preview after install: %v", err)
	}
	if p.ChargeCents != 60000 {
		t.Errorf("preview charge = %d, want 60000", p.ChargeCents)
	}
	got, _, err := l.ChargeFtp("bu", "USD", 12, 1200000, ftpDay)
	if err != nil {
		t.Fatalf("charge after install: %v", err)
	}
	if got.DebitAccount != "ftp-expense" || got.CreditAccount != "treasury-pool" {
		t.Errorf("legs = %s/%s, want ftp-expense/treasury-pool", got.DebitAccount, got.CreditAccount)
	}
}

func TestSetFtpAccountsPreviouslyBookedEntriesUntouched(t *testing.T) {
	l, _ := ftpAmendTestLedger(t)
	first, _, err := l.ChargeFtp("bu-lending", "USD", 12, 1200000, ftpDay)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.SetFtpAccounts(ftpAmendAccounts()); err != nil {
		t.Fatalf("SetFtpAccounts: %v", err)
	}
	// A charge on the next day books the new legs; the first entry
	// keeps the legs it booked.
	second, _, err := l.ChargeFtp("bu-lending", "USD", 12, 1200000, ftpDay.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if first.DebitAccount != "ftp-expense" || first.CreditAccount != "treasury-pool" {
		t.Errorf("first entry legs moved to %s/%s, want ftp-expense/treasury-pool", first.DebitAccount, first.CreditAccount)
	}
	if second.DebitAccount != "ftp-expense-2" || second.CreditAccount != "treasury-pool-2" {
		t.Errorf("second entry legs = %s/%s, want ftp-expense-2/treasury-pool-2", second.DebitAccount, second.CreditAccount)
	}
	if got := l.Balance("ftp-expense"); got != 60000 {
		t.Errorf("old expense balance = %d, want 60000", got)
	}
	if got := l.Balance("ftp-expense-2"); got != 60000 {
		t.Errorf("new expense balance = %d, want 60000", got)
	}
}

func TestSetFtpAccountsSnapshotRoundTrip(t *testing.T) {
	l := New(WithFtpAccounts(testFtpAccounts()))
	if _, err := l.SetFtpCurve("USD", []FtpTenorPoint{{12, 500}}); err != nil {
		t.Fatal(err)
	}
	if err := l.SetFtpAccounts(ftpAmendAccounts()); err != nil {
		t.Fatalf("SetFtpAccounts: %v", err)
	}
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	l2, err := ImportSnapshot(&buf)
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if got := l2.FtpAccounts(); got != ftpAmendAccounts() {
		t.Errorf("restored FtpAccounts() = %+v, want %+v", got, ftpAmendAccounts())
	}
	if !snapshotLedgersEqual(l, l2) {
		t.Error("restored ledger differs (amended triple must round-trip)")
	}
	// Charges work on the restored ledger, on the amended legs.
	got, _, err := l2.ChargeFtp("bu-lending", "USD", 12, 1200000, ftpDay)
	if err != nil {
		t.Fatalf("ChargeFtp on restored ledger: %v", err)
	}
	if got.DebitAccount != "ftp-expense-2" || got.CreditAccount != "treasury-pool-2" {
		t.Errorf("restored legs = %s/%s, want ftp-expense-2/treasury-pool-2", got.DebitAccount, got.CreditAccount)
	}
}

func TestSetFtpAccountsCtxRecordsOperator(t *testing.T) {
	l, al := ftpAmendTestLedger(t)
	ctx := ContextWithActor(context.Background(), "operator-alice")
	if err := l.SetFtpAccountsCtx(ctx, ftpAmendAccounts()); err != nil {
		t.Fatalf("SetFtpAccountsCtx: %v", err)
	}
	events := ftpAccountsAmendAuditEvents(t, al)
	if len(events) != 1 {
		t.Fatalf("ftp_accounts_amended events = %d, want 1", len(events))
	}
	if events[0].Actor != "operator-alice" {
		t.Errorf("actor = %q, want operator-alice", events[0].Actor)
	}
}
