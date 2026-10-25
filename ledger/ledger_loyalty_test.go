package ledger

import (
	"errors"
	"testing"
	"time"
)

func loyaltyProgram() LoyaltyProgram {
	return LoyaltyProgram{
		Name:            "gold",
		CentsPerPoint:   10,
		InactivityDays:  30,
		RevenueAccount:  "revenue-loyalty",
		BreakageAccount: "breakage-loyalty",
	}
}

func mustRegisterLoyalty(t *testing.T, l *Ledger, p LoyaltyProgram) {
	t.Helper()
	if err := l.RegisterLoyaltyProgram(p); err != nil {
		t.Fatalf("RegisterLoyaltyProgram: %v", err)
	}
}

func TestLoyaltyRegisterValidation(t *testing.T) {
	l := New()
	bad := []LoyaltyProgram{
		{Name: "", CentsPerPoint: 10, InactivityDays: 30, RevenueAccount: "r", BreakageAccount: "b"},
		{Name: "a/b", CentsPerPoint: 10, InactivityDays: 30, RevenueAccount: "r", BreakageAccount: "b"},
		{Name: "x", CentsPerPoint: 0, InactivityDays: 30, RevenueAccount: "r", BreakageAccount: "b"},
		{Name: "x", CentsPerPoint: 10, InactivityDays: 0, RevenueAccount: "r", BreakageAccount: "b"},
		{Name: "x", CentsPerPoint: 10, InactivityDays: 30, RevenueAccount: "", BreakageAccount: "b"},
		{Name: "x", CentsPerPoint: 10, InactivityDays: 30, RevenueAccount: "r", BreakageAccount: ""},
	}
	for i, p := range bad {
		if err := l.RegisterLoyaltyProgram(p); !errors.Is(err, ErrInvalidLoyaltyProgram) {
			t.Fatalf("bad program %d: err = %v, want ErrInvalidLoyaltyProgram", i, err)
		}
	}
}

func TestLoyaltyIssueHappyPath(t *testing.T) {
	l := New()
	mustRegisterLoyalty(t, l, loyaltyProgram())
	m, err := l.IssuePoints("acct-1", 250, "gold")
	if err != nil {
		t.Fatalf("IssuePoints: %v", err)
	}
	if m.Kind != "issue" || m.Points != 250 || m.CashCents != 0 || m.CashAccount != "" {
		t.Fatalf("movement = %+v, want issue of 250 with no cash leg", m)
	}
	if b := l.PointsBalance("acct-1", "gold"); b != 250 {
		t.Fatalf("PointsBalance = %d, want 250", b)
	}
	// Points live apart from cash: no cash balance moved, the books
	// still balance.
	if b := l.Balance("acct-1"); b != 0 {
		t.Fatalf("cash Balance = %d, want 0 (points are not cash)", b)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Fatalf("VerifyAccountingEquation: %v", err)
	}
}

func TestLoyaltyIssueErrors(t *testing.T) {
	l := New()
	mustRegisterLoyalty(t, l, loyaltyProgram())
	if _, err := l.IssuePoints("acct-1", 10, "nope"); !errors.Is(err, ErrUnknownLoyaltyProgram) {
		t.Fatalf("unknown program: err = %v, want ErrUnknownLoyaltyProgram", err)
	}
	if _, err := l.IssuePoints("acct-1", 0, "gold"); !errors.Is(err, ErrInvalidLoyaltyPoints) {
		t.Fatalf("zero points: err = %v, want ErrInvalidLoyaltyPoints", err)
	}
	if _, err := l.IssuePoints("acct-1", -5, "gold"); !errors.Is(err, ErrInvalidLoyaltyPoints) {
		t.Fatalf("negative points: err = %v, want ErrInvalidLoyaltyPoints", err)
	}
	if _, err := l.IssuePoints("", 10, "gold"); err == nil {
		t.Fatal("empty account must fail")
	}
}

func TestLoyaltyRedeemFIFOAtIssueTimeRates(t *testing.T) {
	l := New()
	mustRegisterLoyalty(t, l, loyaltyProgram())
	if _, err := l.IssuePoints("acct-1", 100, "gold"); err != nil {
		t.Fatalf("issue 1: %v", err)
	}
	// Rate change: future issues value at 20c, the first lot stays 10c.
	p := loyaltyProgram()
	p.CentsPerPoint = 20
	mustRegisterLoyalty(t, l, p)
	if _, err := l.IssuePoints("acct-1", 100, "gold"); err != nil {
		t.Fatalf("issue 2: %v", err)
	}
	// Redeem 150: 100 @ 10c (first lot) + 50 @ 20c (second lot).
	m, err := l.RedeemPoints("acct-1", 150, "gold")
	if err != nil {
		t.Fatalf("RedeemPoints: %v", err)
	}
	if m.Kind != "redeem" || m.Points != 150 {
		t.Fatalf("movement = %+v, want redeem of 150", m)
	}
	if m.CashCents != 2000 {
		t.Fatalf("CashCents = %d, want 2000 (100*10 + 50*20, issue-time rates)", m.CashCents)
	}
	if m.CashAccount != "revenue-loyalty" {
		t.Fatalf("CashAccount = %q, want revenue-loyalty", m.CashAccount)
	}
	if b := l.PointsBalance("acct-1", "gold"); b != 50 {
		t.Fatalf("PointsBalance = %d, want 50", b)
	}
	if b := l.Balance("acct-1"); b != 0 {
		t.Fatalf("cash Balance = %d, want 0 (redemption credits revenue by valuation only)", b)
	}
}

func TestLoyaltyRedeemInsufficientLeavesBalance(t *testing.T) {
	l := New()
	mustRegisterLoyalty(t, l, loyaltyProgram())
	if _, err := l.IssuePoints("acct-1", 40, "gold"); err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := l.RedeemPoints("acct-1", 41, "gold"); !errors.Is(err, ErrInsufficientLoyaltyPoints) {
		t.Fatalf("over-redeem: err = %v, want ErrInsufficientLoyaltyPoints", err)
	}
	if b := l.PointsBalance("acct-1", "gold"); b != 40 {
		t.Fatalf("PointsBalance = %d, want 40 (failed redeem books nothing)", b)
	}
	if _, err := l.RedeemPoints("acct-1", 10, "nope"); !errors.Is(err, ErrUnknownLoyaltyProgram) {
		t.Fatalf("unknown program: err = %v, want ErrUnknownLoyaltyProgram", err)
	}
}

func TestLoyaltyExpireInactivityWindow(t *testing.T) {
	l := New()
	mustRegisterLoyalty(t, l, loyaltyProgram())
	if _, err := l.IssuePoints("acct-old", 100, "gold"); err != nil {
		t.Fatalf("issue old: %v", err)
	}
	if _, err := l.IssuePoints("acct-new", 60, "gold"); err != nil {
		t.Fatalf("issue new: %v", err)
	}
	now := time.Now().UTC()
	// In-package: backdate activity so only acct-old is past the
	// 30-day window; acct-new stays active.
	l.loyaltyActivity[loyaltyKey{account: "acct-old", program: "gold"}] = now.AddDate(0, 0, -40)
	l.loyaltyActivity[loyaltyKey{account: "acct-new", program: "gold"}] = now.AddDate(0, 0, -5)

	rep := l.ExpirePoints(now)
	if len(rep.Expired) != 1 {
		t.Fatalf("Expired = %d entries, want 1 (only the inactive balance)", len(rep.Expired))
	}
	e := rep.Expired[0]
	if e.Account != "acct-old" || e.Points != 100 {
		t.Fatalf("expiry = %+v, want acct-old 100 points", e)
	}
	if e.CashCents != 1000 {
		t.Fatalf("CashCents = %d, want 1000 (100 @ 10c issue-time rate)", e.CashCents)
	}
	if e.BreakageAccount != "breakage-loyalty" {
		t.Fatalf("BreakageAccount = %q, want breakage-loyalty", e.BreakageAccount)
	}
	if rep.TotalPoints != 100 || rep.TotalCash != 1000 {
		t.Fatalf("totals = %d/%d, want 100/1000", rep.TotalPoints, rep.TotalCash)
	}
	if b := l.PointsBalance("acct-old", "gold"); b != 0 {
		t.Fatalf("acct-old balance = %d, want 0 after expiry", b)
	}
	if b := l.PointsBalance("acct-new", "gold"); b != 60 {
		t.Fatalf("acct-new balance = %d, want 60 (recent activity keeps it alive)", b)
	}
	// A second sweep finds nothing: the expired balance is gone.
	if rep2 := l.ExpirePoints(now); len(rep2.Expired) != 0 {
		t.Fatalf("second sweep expired %d, want 0", len(rep2.Expired))
	}
	// Expiry never touches cash or the escheat path: balances stay 0.
	if b := l.Balance("acct-old"); b != 0 {
		t.Fatalf("cash Balance = %d, want 0", b)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Fatalf("VerifyAccountingEquation: %v", err)
	}
}

func TestLoyaltyMovementsLog(t *testing.T) {
	l := New()
	mustRegisterLoyalty(t, l, loyaltyProgram())
	if _, err := l.IssuePoints("acct-1", 100, "gold"); err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := l.RedeemPoints("acct-1", 30, "gold"); err != nil {
		t.Fatalf("redeem: %v", err)
	}
	ms := l.LoyaltyMovements("acct-1", "gold")
	if len(ms) != 2 {
		t.Fatalf("movements = %d, want 2", len(ms))
	}
	if ms[0].Kind != "issue" || ms[1].Kind != "redeem" {
		t.Fatalf("kinds = %q/%q, want issue/redeem in order", ms[0].Kind, ms[1].Kind)
	}
	if ms[1].CashCents != 300 {
		t.Fatalf("redeem cash = %d, want 300 (30 @ 10c)", ms[1].CashCents)
	}
	if len(l.LoyaltyMovements("acct-1", "other")) != 0 {
		t.Fatal("other program must have no movements")
	}
	// The returned slice is a copy: mutating it must not corrupt the log.
	ms[0].Points = 9999
	if again := l.LoyaltyMovements("acct-1", "gold"); again[0].Points != 100 {
		t.Fatalf("log corrupted through returned slice: %d", again[0].Points)
	}
}
