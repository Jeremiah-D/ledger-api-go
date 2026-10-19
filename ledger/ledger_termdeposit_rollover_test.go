package ledger

import (
	"errors"
	"testing"
	"time"
)

// seedRolloverLedger builds a ledger with a funded account, the
// term-deposit interest-expense account configured, and a file audit log
// so the rollover/skip audit ops can be asserted.
func seedRolloverLedger(t *testing.T) (*Ledger, *AuditLog) {
	t.Helper()
	al := mustAuditLog(t)
	l := New(WithAuditLog(al), WithTermDepositInterestExpense("td-interest-expense"))
	seedBalance(t, l, "saver", 1_000_000)
	return l, al
}

func openRolloverDeposit(t *testing.T, l *Ledger, id string, amount int64, policy *RolloverPolicy) TermDeposit {
	t.Helper()
	td, _, err := l.OpenTermDeposit(TermDeposit{
		ID:          id,
		Account:     "saver",
		AmountCents: amount,
		TermDays:    30,
		RateBps:     500,
		Rollover:    policy,
	})
	if err != nil {
		t.Fatalf("OpenTermDeposit: %v", err)
	}
	return td
}

func auditOpsByName(t *testing.T, al *AuditLog, now time.Time) map[string][]AuditEvent {
	t.Helper()
	if err := al.Close(); err != nil {
		t.Fatalf("audit log close: %v", err)
	}
	events, corrupt, err := ReadAuditLog(al.Dir(), now.UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt = %d, want 0", corrupt)
	}
	byOp := map[string][]AuditEvent{}
	for _, ev := range events {
		byOp[ev.Op] = append(byOp[ev.Op], ev)
	}
	return byOp
}

func TestTermDepositRollover_Principal(t *testing.T) {
	l, al := seedRolloverLedger(t)
	td := openRolloverDeposit(t, l, "td-roll", 400_000, &RolloverPolicy{
		Mode: RolloverPrincipal, TermDays: 30, MaxRollovers: 2,
	})

	matured, accrued := l.SweepTermDeposits(td.MaturesAt)
	if matured != 1 {
		t.Fatalf("matured = %d, want 1", matured)
	}
	_ = accrued

	// The original deposit matured.
	orig, ok := l.GetTermDeposit("td-roll")
	if !ok || orig.Status != TermDepositMatured {
		t.Fatalf("original = %+v, ok=%v, want matured", orig, ok)
	}
	// The renewal opened under the deterministic ID with the same
	// principal, the policy carried over, and the roll count bumped.
	next, ok := l.GetTermDeposit("td-roll/roll/1")
	if !ok {
		t.Fatal("rollover deposit td-roll/roll/1 not found")
	}
	if next.Status != TermDepositActive {
		t.Errorf("rollover status = %q, want active", next.Status)
	}
	if next.AmountCents != 400_000 {
		t.Errorf("rollover principal = %d, want 400000", next.AmountCents)
	}
	if next.Rollover == nil || next.Rollover.Mode != RolloverPrincipal ||
		next.Rollover.TermDays != 30 || next.Rollover.MaxRollovers != 2 {
		t.Errorf("rollover policy = %+v, want carried-over principal/30/2", next.Rollover)
	}
	if next.RolloversUsed != 1 {
		t.Errorf("rollovers_used = %d, want 1", next.RolloversUsed)
	}
	if next.RolledFrom != "td-roll" {
		t.Errorf("rolled_from = %q, want td-roll", next.RolledFrom)
	}
	// The renewal re-locks available funds: 400k locked again.
	if a := l.Available("saver"); a != 600_000 {
		t.Errorf("available = %d, want 600000", a)
	}

	byOp := auditOpsByName(t, al, time.Now())
	rolled := byOp["term_deposit_rolled_over"]
	if len(rolled) != 1 {
		t.Fatalf("term_deposit_rolled_over events = %d, want 1", len(rolled))
	}
	if rolled[0].TraceID != "td-roll/roll/1" {
		t.Errorf("rolled_over trace = %q, want td-roll/roll/1", rolled[0].TraceID)
	}
	if got := rolled[0].Details["roll_number"]; got != float64(1) {
		t.Errorf("roll_number = %v, want 1", got)
	}
}

func TestTermDepositRollover_ChainAndCap(t *testing.T) {
	l, _ := seedRolloverLedger(t)
	td := openRolloverDeposit(t, l, "td-chain", 100_000, &RolloverPolicy{
		Mode: RolloverPrincipal, TermDays: 30, MaxRollovers: 2,
	})

	// First sweep: td-chain matures, td-chain/roll/1 opens.
	l.SweepTermDeposits(td.MaturesAt)
	r1, ok := l.GetTermDeposit("td-chain/roll/1")
	if !ok {
		t.Fatal("td-chain/roll/1 not found after first sweep")
	}
	// Second sweep at the rollover's own maturity: the chain continues
	// with the parent-ID-derived deterministic ID.
	l.SweepTermDeposits(r1.MaturesAt)
	r2, ok := l.GetTermDeposit("td-chain/roll/1/roll/2")
	if !ok {
		t.Fatal("td-chain/roll/1/roll/2 not found after second sweep")
	}
	if r2.RolloversUsed != 2 || r2.RolledFrom != "td-chain/roll/1" {
		t.Errorf("r2 = rollovers_used %d rolled_from %q, want 2 / td-chain/roll/1",
			r2.RolloversUsed, r2.RolledFrom)
	}
	// Re-fetch: the earlier r1 copy predates the second sweep.
	if r1, _ := l.GetTermDeposit("td-chain/roll/1"); r1.Status != TermDepositMatured {
		t.Errorf("r1 status = %q, want matured", r1.Status)
	}
	// Third sweep: the cap is reached — r2 matures with no renewal.
	l.SweepTermDeposits(r2.MaturesAt)
	if r2f, _ := l.GetTermDeposit("td-chain/roll/1/roll/2"); r2f.Status != TermDepositMatured {
		t.Errorf("r2 status = %q, want matured", r2f.Status)
	}
	if _, ok := l.GetTermDeposit("td-chain/roll/1/roll/2/roll/3"); ok {
		t.Error("third rollover opened despite MaxRollovers=2")
	}
	// Re-running a sweep is idempotent: no duplicate renewals.
	before := len(l.TermDeposits())
	l.SweepTermDeposits(r2.MaturesAt.Add(time.Hour))
	if got := len(l.TermDeposits()); got != before {
		t.Errorf("deposits after re-sweep = %d, want %d", got, before)
	}
}

func TestTermDepositRollover_PrincipalPlusInterest(t *testing.T) {
	l, _ := seedRolloverLedger(t)
	td := openRolloverDeposit(t, l, "td-compound", 400_000, &RolloverPolicy{
		Mode: RolloverPrincipalPlusInterest, TermDays: 30, MaxRollovers: 1,
	})
	// Book one day of interest before maturity: floor(400000*500/10000/365) = 54.
	asof := td.OpenedAt.Add(24 * time.Hour)
	if _, _, err := l.AccrueTermInterest("td-compound", asof); err != nil {
		t.Fatalf("AccrueTermInterest: %v", err)
	}
	l.SweepTermDeposits(td.MaturesAt)

	next, ok := l.GetTermDeposit("td-compound/roll/1")
	if !ok {
		t.Fatal("td-compound/roll/1 not found")
	}
	if next.AmountCents != 400_054 {
		t.Errorf("compound principal = %d, want 400054", next.AmountCents)
	}
	// The interest rows stay attributed to the original deposit: the
	// parent's accrued total must not absorb the child's rows and the
	// child starts clean. (Accrue one more day on the child first.)
	if _, _, err := l.AccrueTermInterest("td-compound/roll/1", next.OpenedAt.Add(24*time.Hour)); err != nil {
		t.Fatalf("child accrue: %v", err)
	}
	if got := l.TermDepositInterestAccrued("td-compound"); got != 54 {
		t.Errorf("parent accrued = %d, want 54 (child rows must not leak)", got)
	}
	if got := l.TermDepositInterestAccrued("td-compound/roll/1"); got != 54 {
		t.Errorf("child accrued = %d, want 54", got)
	}
}

func TestTermDepositRollover_SkippedWhenFrozen(t *testing.T) {
	l, al := seedRolloverLedger(t)
	td := openRolloverDeposit(t, l, "td-frozen", 100_000, &RolloverPolicy{
		Mode: RolloverPrincipal, TermDays: 30, MaxRollovers: 1,
	})
	l.Freeze("saver")
	l.SweepTermDeposits(td.MaturesAt)

	// The original still matures — the renewal failure never holds the
	// maturity hostage.
	if orig, _ := l.GetTermDeposit("td-frozen"); orig.Status != TermDepositMatured {
		t.Errorf("original status = %q, want matured", orig.Status)
	}
	if _, ok := l.GetTermDeposit("td-frozen/roll/1"); ok {
		t.Error("rollover opened on a frozen account")
	}
	byOp := auditOpsByName(t, al, time.Now())
	skipped := byOp["term_deposit_rollover_skipped"]
	if len(skipped) != 1 {
		t.Fatalf("term_deposit_rollover_skipped events = %d, want 1", len(skipped))
	}
	if skipped[0].TraceID != "td-frozen" {
		t.Errorf("skipped trace = %q, want td-frozen", skipped[0].TraceID)
	}
}

func TestTermDepositRollover_SkippedWhenInsufficientAvailable(t *testing.T) {
	l, _ := seedRolloverLedger(t)
	td := openRolloverDeposit(t, l, "td-thin", 4_000, &RolloverPolicy{
		Mode: RolloverPrincipal, TermDays: 30, MaxRollovers: 1,
	})
	// Spend the account down after the open: the maturity releases the
	// 4000 lock, but the balance (3000) no longer covers a fresh 4000
	// term, so the renewal must fail the available-funds gate.
	if _, _, err := l.Post(JournalEntry{
		ID: "drain", DebitAccount: "ops", CreditAccount: "saver",
		AmountCents: 997_000, Currency: "USD",
	}); err != nil {
		t.Fatalf("drain post: %v", err)
	}
	l.SweepTermDeposits(td.MaturesAt)
	if orig, _ := l.GetTermDeposit("td-thin"); orig.Status != TermDepositMatured {
		t.Errorf("original status = %q, want matured", orig.Status)
	}
	if _, ok := l.GetTermDeposit("td-thin/roll/1"); ok {
		t.Error("rollover opened despite insufficient available funds")
	}
}

func TestTermDepositRollover_NoPolicyNoRoll(t *testing.T) {
	l, _ := seedRolloverLedger(t)
	// Nil policy and explicit mode "none" both mean no renewal.
	for _, id := range []string{"td-plain", "td-none"} {
		var p *RolloverPolicy
		if id == "td-none" {
			p = &RolloverPolicy{Mode: RolloverNone}
		}
		td := openRolloverDeposit(t, l, id, 50_000, p)
		if td.Rollover != nil {
			t.Errorf("%s: rollover = %+v, want nil", id, td.Rollover)
		}
		l.SweepTermDeposits(td.MaturesAt)
		if _, ok := l.GetTermDeposit(id + "/roll/1"); ok {
			t.Errorf("%s: unexpected rollover opened", id)
		}
	}
}

func TestTermDepositRollover_PolicyValidation(t *testing.T) {
	l, _ := seedRolloverLedger(t)
	cases := []struct {
		name   string
		policy *RolloverPolicy
		want   error
	}{
		{"bad mode", &RolloverPolicy{Mode: "sometimes", TermDays: 30, MaxRollovers: 1}, ErrInvalidRolloverMode},
		{"zero term", &RolloverPolicy{Mode: RolloverPrincipal, TermDays: 0, MaxRollovers: 1}, ErrInvalidRolloverTermDays},
		{"huge term", &RolloverPolicy{Mode: RolloverPrincipal, TermDays: 36501, MaxRollovers: 1}, ErrInvalidRolloverTermDays},
		{"zero cap", &RolloverPolicy{Mode: RolloverPrincipal, TermDays: 30, MaxRollovers: 0}, ErrInvalidRolloverMaxRollovers},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := l.OpenTermDeposit(TermDeposit{
				ID: "td-badpol", Account: "saver", AmountCents: 100,
				TermDays: 30, RateBps: 100, Rollover: c.policy,
			})
			if !errors.Is(err, c.want) {
				t.Errorf("err = %v, want %v", err, c.want)
			}
		})
	}
}

func TestTermDepositRollover_CountersResetOnOpen(t *testing.T) {
	l, _ := seedRolloverLedger(t)
	// An operator cannot smuggle rollover counters past the open gate to
	// dodge the MaxRollovers cap.
	td, _, err := l.OpenTermDeposit(TermDeposit{
		ID: "td-smuggle", Account: "saver", AmountCents: 100,
		TermDays: 30, RateBps: 100,
		Rollover:      &RolloverPolicy{Mode: RolloverPrincipal, TermDays: 30, MaxRollovers: 1},
		RolloversUsed: 99,
		RolledFrom:    "td-smuggle",
	})
	if err != nil {
		t.Fatalf("OpenTermDeposit: %v", err)
	}
	if td.RolloversUsed != 0 || td.RolledFrom != "" {
		t.Errorf("counters = %d/%q, want 0/\"\"", td.RolloversUsed, td.RolledFrom)
	}
}

func TestTermDepositRollover_ReconcileDisclosure(t *testing.T) {
	l, _ := seedRolloverLedger(t)
	td := openRolloverDeposit(t, l, "td-recon", 100_000, &RolloverPolicy{
		Mode: RolloverPrincipal, TermDays: 30, MaxRollovers: 3,
	})
	l.SweepTermDeposits(td.MaturesAt)

	rep := l.Reconcile(time.Now())
	byID := map[string]TermDepositReport{}
	for _, r := range rep.TermDeposits {
		byID[r.ID] = r
	}
	parent, ok := byID["td-recon"]
	if !ok {
		t.Fatal("td-recon missing from reconcile")
	}
	if parent.Rollover == nil || parent.RolloversUsed != 0 {
		t.Errorf("parent disclosure = %+v, want policy with 0 rolls", parent)
	}
	child, ok := byID["td-recon/roll/1"]
	if !ok {
		t.Fatal("td-recon/roll/1 missing from reconcile")
	}
	if child.RolledFrom != "td-recon" || child.RolloversUsed != 1 {
		t.Errorf("child disclosure = rolled_from %q rolls %d, want td-recon/1",
			child.RolledFrom, child.RolloversUsed)
	}
}
