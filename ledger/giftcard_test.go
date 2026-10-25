package ledger

import (
	"errors"
	"testing"
	"time"
)

func giftCardConfig() GiftCardConfig {
	return GiftCardConfig{
		DormancyDays:     365,
		BreakageBps:      5000,
		BreakageAccount:  "breakage-giftcard",
		LiabilityAccount: "liability-giftcard",
	}
}

func mustRegisterGiftCard(t *testing.T, l *Ledger, c GiftCardConfig) {
	t.Helper()
	if err := l.RegisterGiftCardConfig(c); err != nil {
		t.Fatalf("RegisterGiftCardConfig: %v", err)
	}
}

func TestGiftCardConfigValidation(t *testing.T) {
	l := New()
	bad := []GiftCardConfig{
		{DormancyDays: 0, BreakageBps: 5000, BreakageAccount: "b", LiabilityAccount: "l"},
		{DormancyDays: -1, BreakageBps: 5000, BreakageAccount: "b", LiabilityAccount: "l"},
		{DormancyDays: 365, BreakageBps: -1, BreakageAccount: "b", LiabilityAccount: "l"},
		{DormancyDays: 365, BreakageBps: 10001, BreakageAccount: "b", LiabilityAccount: "l"},
		{DormancyDays: 365, BreakageBps: 5000, BreakageAccount: "", LiabilityAccount: "l"},
		{DormancyDays: 365, BreakageBps: 5000, BreakageAccount: "b", LiabilityAccount: ""},
	}
	for i, c := range bad {
		if err := l.RegisterGiftCardConfig(c); !errors.Is(err, ErrInvalidGiftCardConfig) {
			t.Fatalf("bad config %d: err = %v, want ErrInvalidGiftCardConfig", i, err)
		}
	}
	// Boundary bps values are valid.
	for _, bps := range []int{0, 10000} {
		c := giftCardConfig()
		c.BreakageBps = bps
		if err := l.RegisterGiftCardConfig(c); err != nil {
			t.Fatalf("bps %d: err = %v, want nil", bps, err)
		}
	}
}

func TestGiftCardIssueHappyPath(t *testing.T) {
	l := New()
	mustRegisterGiftCard(t, l, giftCardConfig())
	card, err := l.IssueGiftCard("GC-1", 10000, "USD")
	if err != nil {
		t.Fatalf("IssueGiftCard: %v", err)
	}
	if card.FaceCents != 10000 || card.BalanceCents != 10000 || card.Status != GiftCardActive || card.Currency != "USD" {
		t.Fatalf("card = %+v, want face/balance 10000 active USD", card)
	}
	if b, ok := l.GiftCardBalance("GC-1"); !ok || b != 10000 {
		t.Fatalf("GiftCardBalance = %d, %v, want 10000, true", b, ok)
	}
	// The float lives apart from cash: no cash balance moved, the
	// books still balance.
	if b := l.Balance("acct-1"); b != 0 {
		t.Fatalf("cash Balance = %d, want 0 (float is not cash)", b)
	}
	if err := l.VerifyAccountingEquation(); err != nil {
		t.Fatalf("VerifyAccountingEquation: %v", err)
	}
}

func TestGiftCardIssueErrors(t *testing.T) {
	l := New()
	mustRegisterGiftCard(t, l, giftCardConfig())
	if _, err := l.IssueGiftCard("GC-1", 10000, "USD"); err != nil {
		t.Fatalf("first issue: %v", err)
	}
	if _, err := l.IssueGiftCard("GC-1", 5000, "USD"); !errors.Is(err, ErrDuplicateGiftCard) {
		t.Fatalf("duplicate: err = %v, want ErrDuplicateGiftCard", err)
	}
	if _, err := l.IssueGiftCard("GC-2", 0, "USD"); !errors.Is(err, ErrInvalidGiftCardAmount) {
		t.Fatalf("zero amount: err = %v, want ErrInvalidGiftCardAmount", err)
	}
	if _, err := l.IssueGiftCard("GC-2", -5, "USD"); !errors.Is(err, ErrInvalidGiftCardAmount) {
		t.Fatalf("negative amount: err = %v, want ErrInvalidGiftCardAmount", err)
	}
	if _, err := l.IssueGiftCard("GC-2", 5000, ""); !errors.Is(err, ErrInvalidGiftCardAmount) {
		t.Fatalf("empty currency: err = %v, want ErrInvalidGiftCardAmount", err)
	}
	if _, err := l.IssueGiftCard("", 5000, "USD"); err == nil {
		t.Fatal("empty card number must fail")
	}
}

func TestGiftCardRedeemPartialAndExhaust(t *testing.T) {
	l := New()
	mustRegisterGiftCard(t, l, giftCardConfig())
	if _, err := l.IssueGiftCard("GC-1", 10000, "USD"); err != nil {
		t.Fatalf("IssueGiftCard: %v", err)
	}
	// Partial redemption: face stays, balance drops, still active.
	m, err := l.RedeemGiftCard("GC-1", 4000)
	if err != nil {
		t.Fatalf("RedeemGiftCard: %v", err)
	}
	if m.Kind != "redeem" || m.AmountCents != 4000 || m.BalanceCents != 6000 {
		t.Fatalf("movement = %+v, want redeem 4000 -> 6000", m)
	}
	card, _ := l.GiftCard("GC-1")
	if card.FaceCents != 10000 || card.BalanceCents != 6000 || card.Status != GiftCardActive {
		t.Fatalf("card = %+v, want face 10000 balance 6000 active", card)
	}
	// Over-redemption is rejected and books nothing.
	if _, err := l.RedeemGiftCard("GC-1", 6001); !errors.Is(err, ErrInsufficientGiftCardBalance) {
		t.Fatalf("over-redeem: err = %v, want ErrInsufficientGiftCardBalance", err)
	}
	if b, _ := l.GiftCardBalance("GC-1"); b != 6000 {
		t.Fatalf("balance after failed redeem = %d, want 6000", b)
	}
	// Redeeming the remainder exhausts the card.
	if _, err := l.RedeemGiftCard("GC-1", 6000); err != nil {
		t.Fatalf("final redeem: %v", err)
	}
	card, _ = l.GiftCard("GC-1")
	if card.BalanceCents != 0 || card.Status != GiftCardExhausted {
		t.Fatalf("card = %+v, want balance 0 exhausted", card)
	}
	if _, err := l.RedeemGiftCard("GC-1", 1); !errors.Is(err, ErrGiftCardNotRedeemable) {
		t.Fatalf("exhausted redeem: err = %v, want ErrGiftCardNotRedeemable", err)
	}
	if _, err := l.RedeemGiftCard("GC-nope", 1); !errors.Is(err, ErrUnknownGiftCard) {
		t.Fatalf("unknown card: err = %v, want ErrUnknownGiftCard", err)
	}
	if _, err := l.RedeemGiftCard("GC-1", 0); !errors.Is(err, ErrInvalidGiftCardAmount) {
		t.Fatalf("zero redeem: err = %v, want ErrInvalidGiftCardAmount", err)
	}
}

func TestGiftCardBreakagePartialAndExpired(t *testing.T) {
	l := New()
	mustRegisterGiftCard(t, l, giftCardConfig())
	if _, err := l.IssueGiftCard("GC-1", 10001, "USD"); err != nil {
		t.Fatalf("IssueGiftCard: %v", err)
	}
	if _, err := l.IssueGiftCard("GC-2", 8000, "USD"); err != nil {
		t.Fatalf("IssueGiftCard: %v", err)
	}
	// A sweep at issue time touches nothing: no dormancy yet.
	if rep := l.BreakageGiftCards(time.Now().UTC()); len(rep.Broken) != 0 {
		t.Fatalf("fresh sweep: broken = %v, want none", rep.Broken)
	}
	// Past the dormancy window, GC-1's 50% breakage floors:
	// floor(10001 * 5000 / 10000) = 5000, residual 5001, expired.
	future := time.Now().UTC().Add(400 * 24 * time.Hour)
	rep := l.BreakageGiftCards(future)
	if len(rep.Broken) != 2 {
		t.Fatalf("broken = %d cards, want 2", len(rep.Broken))
	}
	// Sorted by card number.
	if rep.Broken[0].CardNo != "GC-1" || rep.Broken[0].BreakageCents != 5000 ||
		rep.Broken[0].ResidualCents != 5001 || rep.Broken[0].Status != GiftCardExpired {
		t.Fatalf("GC-1 breakage = %+v, want 5000/5001/expired", rep.Broken[0])
	}
	if rep.Broken[1].CardNo != "GC-2" || rep.Broken[1].BreakageCents != 4000 ||
		rep.Broken[1].ResidualCents != 4000 || rep.Broken[1].Status != GiftCardExpired {
		t.Fatalf("GC-2 breakage = %+v, want 4000/4000/expired", rep.Broken[1])
	}
	if rep.TotalBreakage != 9000 || rep.TotalResidual != 9001 {
		t.Fatalf("totals = %d/%d, want 9000/9001", rep.TotalBreakage, rep.TotalResidual)
	}
	// Expired cards are not redeemable, and a second sweep never
	// re-sweeps them.
	if _, err := l.RedeemGiftCard("GC-1", 1); !errors.Is(err, ErrGiftCardNotRedeemable) {
		t.Fatalf("expired redeem: err = %v, want ErrGiftCardNotRedeemable", err)
	}
	if rep2 := l.BreakageGiftCards(future.Add(24 * time.Hour)); len(rep2.Broken) != 0 {
		t.Fatalf("second sweep: broken = %v, want none", rep2.Broken)
	}
	card, _ := l.GiftCard("GC-1")
	if card.BreakageCents != 5000 || card.BalanceCents != 5001 {
		t.Fatalf("card after sweep = %+v, want breakage 5000 balance 5001", card)
	}
}

func TestGiftCardBreakageFullGoesExhausted(t *testing.T) {
	l := New()
	cfg := giftCardConfig()
	cfg.BreakageBps = 10000
	mustRegisterGiftCard(t, l, cfg)
	if _, err := l.IssueGiftCard("GC-9", 7000, "USD"); err != nil {
		t.Fatalf("IssueGiftCard: %v", err)
	}
	rep := l.BreakageGiftCards(time.Now().UTC().Add(400 * 24 * time.Hour))
	if len(rep.Broken) != 1 || rep.Broken[0].BreakageCents != 7000 ||
		rep.Broken[0].ResidualCents != 0 || rep.Broken[0].Status != GiftCardExhausted {
		t.Fatalf("full breakage = %+v, want 7000/0/exhausted", rep.Broken)
	}
}

func TestGiftCardBreakageWithoutConfigIsEmpty(t *testing.T) {
	l := New()
	if _, err := l.IssueGiftCard("GC-1", 10000, "USD"); err != nil {
		t.Fatalf("IssueGiftCard: %v", err)
	}
	rep := l.BreakageGiftCards(time.Now().UTC().Add(400 * 24 * time.Hour))
	if len(rep.Broken) != 0 || rep.TotalBreakage != 0 {
		t.Fatalf("no-config sweep = %+v, want empty", rep)
	}
}

func TestGiftCardMovements(t *testing.T) {
	l := New()
	mustRegisterGiftCard(t, l, giftCardConfig())
	if _, err := l.IssueGiftCard("GC-1", 10000, "USD"); err != nil {
		t.Fatalf("IssueGiftCard: %v", err)
	}
	if _, err := l.RedeemGiftCard("GC-1", 3000); err != nil {
		t.Fatalf("RedeemGiftCard: %v", err)
	}
	l.BreakageGiftCards(time.Now().UTC().Add(400 * 24 * time.Hour))
	movements := l.GiftCardMovements("GC-1")
	if len(movements) != 3 {
		t.Fatalf("movements = %d, want 3", len(movements))
	}
	kinds := []string{movements[0].Kind, movements[1].Kind, movements[2].Kind}
	if kinds[0] != "issue" || kinds[1] != "redeem" || kinds[2] != "breakage" {
		t.Fatalf("kinds = %v, want issue/redeem/breakage", kinds)
	}
	if movements[2].AmountCents != 3500 || movements[2].BalanceCents != 3500 {
		t.Fatalf("breakage movement = %+v, want 3500 -> 3500", movements[2])
	}
	if got := l.GiftCardMovements("GC-nope"); len(got) != 0 {
		t.Fatalf("unknown card movements = %d, want 0", len(got))
	}
}
