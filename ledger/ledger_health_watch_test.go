package ledger

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func postSimple(t *testing.T, l *Ledger, id, debit, credit string, cents int64) {
	t.Helper()
	if _, _, err := l.Post(JournalEntry{
		ID:            id,
		DebitAccount:  AccountID(debit),
		CreditAccount: AccountID(credit),
		AmountCents:   cents,
		Currency:      "USD",
	}); err != nil {
		t.Fatalf("Post %s: %v", id, err)
	}
}

func TestScanBalanceHealthClean(t *testing.T) {
	l := New()
	postSimple(t, l, "e1", "cash", "equity", 1000)
	postSimple(t, l, "e2", "expenses", "cash", 200)
	l.EnableOverdraftProtection("cash")

	if vs := l.ScanBalanceHealth(); len(vs) != 0 {
		t.Fatalf("healthy ledger: violations = %+v", vs)
	}
	// Repeated scans (rotation walking the key space) stay clean.
	for i := 0; i < 8; i++ {
		if vs := l.ScanBalanceHealth(); len(vs) != 0 {
			t.Fatalf("scan %d: violations = %+v", i, vs)
		}
	}
}

func TestScanBalanceHealthNegativeProtected(t *testing.T) {
	l := New()
	// Payer goes negative: -1000 on equity. Without protection this is
	// normal books (credit-normal account); the watchdog stays quiet.
	postSimple(t, l, "e1", "cash", "equity", 1000)
	if vs := l.ScanBalanceHealth(); len(vs) != 0 {
		t.Fatalf("unprotected negative: violations = %+v", vs)
	}

	// Enabling protection on an account that already sits negative is
	// operator drift the watchdog must catch — the write path would
	// never allow this state to be created fresh.
	l.EnableOverdraftProtection("equity")
	vs := l.ScanBalanceHealth()
	if len(vs) != 1 {
		t.Fatalf("violations = %+v, want exactly one", vs)
	}
	v := vs[0]
	if v.Check != HealthCheckNegativeBalance || v.Account != "equity" || v.Currency != "USD" {
		t.Fatalf("violation = %+v", v)
	}
	if v.Details["balance_cents"] != int64(-1000) {
		t.Fatalf("details = %v", v.Details)
	}
	if v.DetectedAt.IsZero() {
		t.Fatal("DetectedAt not set")
	}
}

func TestScanBalanceHealthEquationCorruption(t *testing.T) {
	l := New()
	postSimple(t, l, "e1", "cash", "equity", 1000)
	postSimple(t, l, "e2", "expenses", "cash", 200)

	// Simulate corruption the write path cannot produce: mutate a net
	// balance directly, breaking balance == debits - credits for one
	// (account, currency). The rotating spot check must catch it within
	// one full rotation (healthWatchWindowDivisor scans).
	l.mu.Lock()
	l.balances[accountCurrency{account: "cash", currency: "USD"}] += 1
	l.mu.Unlock()

	var found *HealthViolation
	for i := 0; i < healthWatchWindowDivisor; i++ {
		for _, v := range l.ScanBalanceHealth() {
			if v.Check == HealthCheckAccountingEquation && v.Account == "cash" {
				vv := v
				found = &vv
			}
		}
		if found != nil {
			break
		}
	}
	if found == nil {
		t.Fatal("equation corruption not detected within one rotation")
	}
	if found.Details["balance_cents"] != int64(801) ||
		found.Details["total_debits_cents"] != int64(1000) ||
		found.Details["total_credits_cents"] != int64(200) {
		t.Fatalf("details = %v", found.Details)
	}
}

func TestScanBalanceHealthVersionContinuity(t *testing.T) {
	l := New()
	postSimple(t, l, "e1", "cash", "equity", 1000)

	// Simulate a restore that bumped the version without appending
	// chain links: links (1) != version (2).
	l.mu.Lock()
	l.version++
	l.mu.Unlock()

	vs := l.ScanBalanceHealth()
	if len(vs) != 1 {
		t.Fatalf("violations = %+v, want exactly one", vs)
	}
	v := vs[0]
	if v.Check != HealthCheckVersionContinuity {
		t.Fatalf("check = %q", v.Check)
	}
	if v.Account != "" || v.Currency != "" {
		t.Fatalf("ledger-wide check should not name an account: %+v", v)
	}
	if v.Details["version"] != uint64(2) || v.Details["chain_links"] != 1 || v.Details["head_seq"] != uint64(1) {
		t.Fatalf("details = %v", v.Details)
	}
}

func TestScanBalanceHealthRotationCoversAllKeys(t *testing.T) {
	l := New()
	// 8 distinct (account, currency) pairs: window size ceil(8/4)=2,
	// so a full rotation is 4 scans.
	for i := 0; i < 4; i++ {
		postSimple(t, l, "e-usd-"+string(rune('a'+i)), "acct", "equity", 100)
	}
	l.mu.Lock()
	for i := 0; i < 4; i++ {
		a := AccountID("fx" + string(rune('a'+i)))
		l.balances[accountCurrency{account: a, currency: "EUR"}] = 50
		l.debitTotals[accountCurrency{account: a, currency: "EUR"}] = 50
	}
	// Corrupt the LAST key in sort order — it must be found within one
	// full rotation even though scan 1 does not cover it.
	l.balances[accountCurrency{account: "fxd", currency: "EUR"}] = 51
	l.mu.Unlock()

	found := false
	for i := 0; i < healthWatchWindowDivisor; i++ {
		for _, v := range l.ScanBalanceHealth() {
			if v.Check == HealthCheckAccountingEquation && v.Account == "fxd" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("corrupted key fxd not detected within one rotation")
	}
}

func TestRunHealthCheckAuditsAndCounts(t *testing.T) {
	al := mustAuditLog(t)
	defer al.Close()
	l := New(WithAuditLog(al))
	postSimple(t, l, "e1", "cash", "equity", 1000)
	l.EnableOverdraftProtection("equity") // -1000 on a protected account

	violations := l.RunHealthCheck()
	if len(violations) != 1 {
		t.Fatalf("violations = %+v", len(violations))
	}
	if got := l.HealthViolationCount(); got != 1 {
		t.Fatalf("HealthViolationCount = %d, want 1", got)
	}

	// A second run finds the same still-broken invariant and counts it
	// again — the watchdog reports state, it does not deduplicate.
	violations = l.RunHealthCheck()
	if len(violations) != 1 || l.HealthViolationCount() != 2 {
		t.Fatalf("second run: violations=%d count=%d", len(violations), l.HealthViolationCount())
	}

	// The audit log carries both violation events.
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, corrupt, err := ReadAuditLog(al.Dir(), time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt = %d, want 0", corrupt)
	}
	found := 0
	for _, ev := range events {
		if ev.Op == "balance_health_violation" {
			found++
			if ev.Details["check"] != "negative_balance" || ev.Actor != "HealthWatch" {
				t.Fatalf("violation event = %+v", ev)
			}
			if len(ev.Accounts) != 1 || ev.Accounts[0] != "equity" {
				t.Fatalf("violation accounts = %v", ev.Accounts)
			}
		}
	}
	if found != 2 {
		t.Fatalf("audit violation events = %d, want 2", found)
	}
}

func TestRunHealthCheckHealthyEmitsNothing(t *testing.T) {
	al := mustAuditLog(t)
	defer al.Close()
	l := New(WithAuditLog(al))
	postSimple(t, l, "e1", "cash", "equity", 1000)

	if vs := l.RunHealthCheck(); len(vs) != 0 {
		t.Fatalf("violations = %+v", vs)
	}
	if got := l.HealthViolationCount(); got != 0 {
		t.Fatalf("count = %d, want 0", got)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, _, err := ReadAuditLog(al.Dir(), time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	for _, ev := range events {
		if ev.Op == "balance_health_violation" {
			t.Fatalf("healthy run emitted a violation event: %+v", ev)
		}
	}
}

func TestStartHealthWatchDisabled(t *testing.T) {
	l := New()
	if hw := StartHealthWatch(context.Background(), l, 0, nil); hw != nil {
		t.Fatal("zero interval must return nil")
	}
	if hw := StartHealthWatch(context.Background(), l, -time.Second, nil); hw != nil {
		t.Fatal("negative interval must return nil")
	}
}

func TestStartHealthWatchTicksAndFinds(t *testing.T) {
	l := New()
	postSimple(t, l, "e1", "cash", "equity", 1000)
	l.EnableOverdraftProtection("equity")

	var got atomic.Uint64
	hw := StartHealthWatch(context.Background(), l, 10*time.Millisecond, func(vs []HealthViolation) {
		got.Add(uint64(len(vs)))
	})
	if hw == nil {
		t.Fatal("expected a watchdog")
	}
	defer hw.Stop()

	deadline := time.Now().Add(3 * time.Second)
	for hw.Ticks() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if hw.Ticks() == 0 {
		t.Fatal("watchdog never ticked")
	}
	deadline = time.Now().Add(3 * time.Second)
	for got.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got.Load() == 0 {
		t.Fatal("onViolation never fired")
	}
	if hw.ViolationsTotal() == 0 {
		t.Fatal("ViolationsTotal = 0")
	}
	if l.HealthViolationCount() == 0 {
		t.Fatal("ledger violation counter = 0")
	}

	// Stop is idempotent.
	hw.Stop()
	hw.Stop()
}

func TestStartHealthWatchContextCancel(t *testing.T) {
	l := New()
	ctx, cancel := context.WithCancel(context.Background())
	hw := StartHealthWatch(ctx, l, 10*time.Millisecond, nil)
	if hw == nil {
		t.Fatal("expected a watchdog")
	}
	cancel()
	hw.Stop() // must return promptly after cancellation
}

func TestHealthWatchAuditEventShape(t *testing.T) {
	// A version-continuity violation carries ledger-wide details and no
	// account — the audit event must stay well-formed.
	al := mustAuditLog(t)
	defer al.Close()
	l := New(WithAuditLog(al))
	postSimple(t, l, "e1", "cash", "equity", 1000)
	l.mu.Lock()
	l.version += 5
	l.mu.Unlock()

	vs := l.RunHealthCheck()
	if len(vs) != 1 || vs[0].Check != HealthCheckVersionContinuity {
		t.Fatalf("violations = %+v", vs)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, corrupt, err := ReadAuditLog(al.Dir(), time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt = %d, want 0", corrupt)
	}
	ev := events[0]
	found := 0
	for _, e := range events {
		if e.Op == "balance_health_violation" {
			found++
			ev = e
		}
	}
	if found != 1 {
		t.Fatalf("violation events = %d, want 1", found)
	}
	if ev.Actor != "HealthWatch" {
		t.Fatalf("actor = %q, want HealthWatch", ev.Actor)
	}
	if ev.Details["check"] != "version_continuity" {
		t.Fatalf("details = %v", ev.Details)
	}
	if ev.Details["version"] != float64(6) || ev.Details["chain_links"] != float64(1) {
		t.Fatalf("details = %v", ev.Details)
	}
	if len(ev.Accounts) != 0 {
		t.Fatalf("ledger-wide event should name no accounts: %+v", ev.Accounts)
	}
}
