package ledger

import (
	"errors"
	"math"
	"math/bits"
	"sort"
	"time"
)

// Gift card float: prepaid-card liability kept strictly apart from
// cash balances (LG-216).
//
// A merchant sells a gift card — cash in, a promise out. The cash
// received is not revenue: it is a liability owed to whoever holds
// the card, so it lives in its own sub-ledger, like loyalty points
// (see loyalty.go, LG-215), and never touches the journal or the
// cash balances. Issuing a card books the liability (face value
// recorded separately from the remaining balance, so a card's
// original value stays queryable after partial redemptions);
// redeeming spends down the balance; a card that hits zero is
// exhausted and can never be redeemed again.
//
// Breakage is the merchant's recognition that some dormant-card
// value will never be redeemed. BreakageGiftCards sweeps active
// cards whose last activity is strictly older than the configured
// dormancy window and confirms a configured proportion of each
// card's remaining balance as breakage revenue, credited (by
// valuation) to the configured breakage account. The proportion is
// in basis points so a merchant can recognize, say, 50% (5000 bps)
// rather than all-or-nothing: the unrecognized remainder stays on
// the card as a liability, and the card is closed to further
// redemption (status expired). This is deliberately not the
// escheat path (see escheat.go, LG-109): escheatment remits dormant
// funds to the state; breakage keeps the residual as the merchant's
// own liability and never remits it. A swept card is never swept
// twice — expired and exhausted cards are skipped — so breakage is
// recognized at most once per card.
//
// State is in-memory only, like loyalty programs and payout
// instructions: cards, the config, and the movement log do not
// round-trip through disaster-recovery snapshots. The movement log
// plus the gift card audit events are the durable record when the
// audit log is enabled (see audit.go); without an audit log the
// movements still survive in memory for the process lifetime.

// GiftCardConfig is the ledger-wide gift card configuration: the
// dormancy window after which an untouched card is swept, the
// proportion of a swept card's balance confirmed as breakage, and
// the accounts named on the liability and breakage legs (reporting
// targets — no cash moves).
type GiftCardConfig struct {
	// DormancyDays is the inactivity window in days: an active card
	// whose last activity is strictly older than this many days is
	// swept by BreakageGiftCards.
	DormancyDays int `json:"dormancy_days"`
	// BreakageBps is the proportion of a swept card's remaining
	// balance confirmed as breakage revenue, in basis points
	// (0..10000: 0 recognizes nothing, 10000 recognizes it all).
	BreakageBps int `json:"breakage_bps"`
	// BreakageAccount is credited (by valuation) when breakage is
	// confirmed: the breakage-revenue account.
	BreakageAccount AccountID `json:"breakage_account"`
	// LiabilityAccount is the reporting target for the gift card
	// liability leg: the account the outstanding float is owed
	// from, named on issue audit events.
	LiabilityAccount AccountID `json:"liability_account"`
}

// GiftCardStatus values: active cards are redeemable, exhausted
// cards have a zero balance, expired cards were closed by a
// breakage sweep with a residual liability still on the books.
const (
	GiftCardActive    = "active"
	GiftCardExhausted = "exhausted"
	GiftCardExpired   = "expired"
)

// GiftCard is one issued card: the face value it was sold for, the
// balance still owed, the breakage already confirmed against it,
// and its lifecycle status.
type GiftCard struct {
	CardNo         string    `json:"card_no"`
	Currency       string    `json:"currency"`
	FaceCents      int64     `json:"face_cents"`
	BalanceCents   int64     `json:"balance_cents"`
	BreakageCents  int64     `json:"breakage_cents"`
	Status         string    `json:"status"`
	IssuedAt       time.Time `json:"issued_at"`
	LastActivityAt time.Time `json:"last_activity_at"`
}

// GiftCardMovement is one atomic card movement: Kind is "issue",
// "redeem", or "breakage". AmountCents is the movement's size and
// BalanceCents the card's balance immediately after it.
type GiftCardMovement struct {
	Kind         string    `json:"kind"`
	CardNo       string    `json:"card_no"`
	AmountCents  int64     `json:"amount_cents"`
	BalanceCents int64     `json:"balance_cents"`
	At           time.Time `json:"at"`
}

// GiftCardBreakage is one card swept by BreakageGiftCards: the
// breakage revenue confirmed, the residual liability left on the
// card, and the card's status after the sweep.
type GiftCardBreakage struct {
	CardNo        string `json:"card_no"`
	BreakageCents int64  `json:"breakage_cents"`
	ResidualCents int64  `json:"residual_cents"`
	Status        string `json:"status"`
}

// GiftCardBreakageReport is BreakageGiftCards' result: the sweep
// time and the cards swept, sorted by card number.
type GiftCardBreakageReport struct {
	SweptAt       time.Time          `json:"swept_at"`
	Broken        []GiftCardBreakage `json:"broken"`
	TotalBreakage int64              `json:"total_breakage_cents"`
	TotalResidual int64              `json:"total_residual_cents"`
}

// Gift card validation errors.
var (
	// ErrInvalidGiftCardConfig is returned when a gift card config
	// fails validation: a non-positive dormancy window, a
	// breakage proportion outside 0..10000 bps, or an empty
	// breakage or liability account.
	ErrInvalidGiftCardConfig = errors.New("ledger: invalid gift card configuration")
	// ErrDuplicateGiftCard is returned when a card number is issued
	// twice: card numbers are unique, and re-issuing one would
	// double-book the float.
	ErrDuplicateGiftCard = errors.New("ledger: duplicate gift card number")
	// ErrUnknownGiftCard is returned when a movement or query names
	// a card number that was never issued.
	ErrUnknownGiftCard = errors.New("ledger: unknown gift card")
	// ErrInvalidGiftCardAmount is returned when an issue or redeem
	// amount is not positive, or the currency is empty: zero-value
	// movements carry no information and negative value is not a
	// thing.
	ErrInvalidGiftCardAmount = errors.New("ledger: gift card amount must be positive")
	// ErrInsufficientGiftCardBalance is returned when a redemption
	// exceeds the card's remaining balance: partial redemption is
	// allowed, over-redemption is not.
	ErrInsufficientGiftCardBalance = errors.New("ledger: insufficient gift card balance")
	// ErrGiftCardNotRedeemable is returned when a redemption names
	// an exhausted or expired card: exhausted cards hold nothing,
	// expired cards were closed by a breakage sweep.
	ErrGiftCardNotRedeemable = errors.New("ledger: gift card is not redeemable")
	// ErrGiftCardOverflow is returned when a breakage valuation
	// overflows int64: the card is skipped rather than swept at a
	// wrapped figure.
	ErrGiftCardOverflow = errors.New("ledger: gift card breakage valuation overflows int64")
)

// checkGiftCardConfig validates a gift card config before it is stored.
func checkGiftCardConfig(c GiftCardConfig) error {
	if c.DormancyDays <= 0 {
		return ErrInvalidGiftCardConfig
	}
	if c.BreakageBps < 0 || c.BreakageBps > 10000 {
		return ErrInvalidGiftCardConfig
	}
	if c.BreakageAccount == "" || c.LiabilityAccount == "" {
		return ErrInvalidGiftCardConfig
	}
	return nil
}

// RegisterGiftCardConfig stores the ledger-wide gift card config,
// upserting: re-registering changes the dormancy window and
// breakage proportion for future sweeps only — cards already
// issued keep their face and balance, and cards already swept are
// never swept again.
func (l *Ledger) RegisterGiftCardConfig(c GiftCardConfig) error {
	if err := checkGiftCardConfig(c); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.giftCardConfig = c
	l.giftCardConfigSet = true
	l.emitAudit(AuditEvent{
		Op:       "gift_card_config_registered",
		Actor:    "ledger",
		Accounts: []AccountID{c.LiabilityAccount, c.BreakageAccount},
		Details: map[string]any{
			"dormancy_days":     c.DormancyDays,
			"breakage_bps":      c.BreakageBps,
			"breakage_account":  string(c.BreakageAccount),
			"liability_account": string(c.LiabilityAccount),
		},
	})
	return nil
}

// GiftCardConfig returns the registered gift card config, reporting
// whether one exists.
func (l *Ledger) GiftCardConfig() (GiftCardConfig, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.giftCardConfig, l.giftCardConfigSet
}

// IssueGiftCard sells a card: cash in, a liability out. The card
// number must be unique, the amount positive, the currency
// non-empty. Face and balance both start at the issued amount; the
// face never changes, so the card's original value stays queryable
// after partial redemptions. No cash balance moves: the float is a
// liability of this sub-ledger, not a journal row.
func (l *Ledger) IssueGiftCard(cardNo string, amountCents int64, currency string) (GiftCard, error) {
	if cardNo == "" {
		return GiftCard{}, ErrUnknownGiftCard
	}
	if amountCents <= 0 || currency == "" {
		return GiftCard{}, ErrInvalidGiftCardAmount
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, exists := l.giftCards[cardNo]; exists {
		return GiftCard{}, ErrDuplicateGiftCard
	}
	now := time.Now().UTC()
	card := GiftCard{
		CardNo:         cardNo,
		Currency:       currency,
		FaceCents:      amountCents,
		BalanceCents:   amountCents,
		Status:         GiftCardActive,
		IssuedAt:       now,
		LastActivityAt: now,
	}
	l.giftCards[cardNo] = card
	l.giftCardMovements = append(l.giftCardMovements, GiftCardMovement{
		Kind:         "issue",
		CardNo:       cardNo,
		AmountCents:  amountCents,
		BalanceCents: amountCents,
		At:           now,
	})
	var issueAccounts []AccountID
	if l.giftCardConfigSet {
		issueAccounts = []AccountID{l.giftCardConfig.LiabilityAccount}
	}
	l.emitAudit(AuditEvent{
		Op:       "gift_card_issued",
		Actor:    "ledger",
		Accounts: issueAccounts,
		Details: map[string]any{
			"card_no":       cardNo,
			"currency":      currency,
			"face_cents":    amountCents,
			"balance_cents": amountCents,
		},
	})
	return card, nil
}

// GiftCard returns a card's current record, reporting whether it
// exists. The returned copy cannot mutate the ledger's record.
func (l *Ledger) GiftCard(cardNo string) (GiftCard, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	card, ok := l.giftCards[cardNo]
	return card, ok
}

// GiftCardBalance returns a card's remaining balance, reporting
// whether the card exists. An unknown card reports 0, like Balance
// for an unknown account.
func (l *Ledger) GiftCardBalance(cardNo string) (int64, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	card, ok := l.giftCards[cardNo]
	if !ok {
		return 0, false
	}
	return card.BalanceCents, true
}

// RedeemGiftCard spends down a card's balance: partial redemptions
// are allowed, over-redemption is rejected, and a redemption that
// empties the card exhausts it. Expired cards (closed by a
// breakage sweep) and exhausted cards are not redeemable. The
// movement is atomic under the ledger write lock: a failed
// redemption books nothing.
func (l *Ledger) RedeemGiftCard(cardNo string, amountCents int64) (GiftCardMovement, error) {
	if amountCents <= 0 {
		return GiftCardMovement{}, ErrInvalidGiftCardAmount
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	card, ok := l.giftCards[cardNo]
	if !ok {
		return GiftCardMovement{}, ErrUnknownGiftCard
	}
	if card.Status != GiftCardActive {
		return GiftCardMovement{}, ErrGiftCardNotRedeemable
	}
	if amountCents > card.BalanceCents {
		return GiftCardMovement{}, ErrInsufficientGiftCardBalance
	}
	now := time.Now().UTC()
	card.BalanceCents -= amountCents
	if card.BalanceCents == 0 {
		card.Status = GiftCardExhausted
	}
	card.LastActivityAt = now
	l.giftCards[cardNo] = card
	m := GiftCardMovement{
		Kind:         "redeem",
		CardNo:       cardNo,
		AmountCents:  amountCents,
		BalanceCents: card.BalanceCents,
		At:           now,
	}
	l.giftCardMovements = append(l.giftCardMovements, m)
	var redeemAccounts []AccountID
	if l.giftCardConfigSet {
		redeemAccounts = []AccountID{l.giftCardConfig.LiabilityAccount}
	}
	l.emitAudit(AuditEvent{
		Op:       "gift_card_redeemed",
		Actor:    "ledger",
		Accounts: redeemAccounts,
		Details: map[string]any{
			"card_no":       cardNo,
			"amount_cents":  amountCents,
			"balance_cents": card.BalanceCents,
			"status":        card.Status,
		},
	})
	return m, nil
}

// breakageCents computes floor(balance * bps / 10000), reporting
// overflow instead of wrapping.
func breakageCents(balance int64, bps int) (int64, bool) {
	hi, lo := bits.Mul64(uint64(balance), uint64(bps))
	if hi != 0 || lo > uint64(math.MaxInt64) {
		return 0, false
	}
	return int64(lo) / 10000, true
}

// BreakageGiftCards sweeps dormant active cards: a card whose last
// activity is strictly older than the configured dormancy window
// has a configured proportion of its remaining balance confirmed
// as breakage revenue, and is then closed to further redemption.
// A card swept to a zero balance is exhausted; one with a residual
// balance is expired, and the residual stays on the card as a
// liability — it is never remitted to the escheat path. Cards
// already expired or exhausted are skipped, so a card is swept at
// most once; a card whose breakage valuation overflows is skipped
// rather than swept at a wrapped figure. With no config registered
// there is nothing to sweep and the report is empty.
func (l *Ledger) BreakageGiftCards(now time.Time) GiftCardBreakageReport {
	l.mu.Lock()
	defer l.mu.Unlock()
	report := GiftCardBreakageReport{SweptAt: now, Broken: []GiftCardBreakage{}}
	if !l.giftCardConfigSet {
		return report
	}
	cfg := l.giftCardConfig
	window := time.Duration(cfg.DormancyDays) * 24 * time.Hour
	for cardNo, card := range l.giftCards {
		if card.Status != GiftCardActive || card.BalanceCents <= 0 {
			continue
		}
		if now.Sub(card.LastActivityAt) <= window {
			continue
		}
		breakage, ok := breakageCents(card.BalanceCents, cfg.BreakageBps)
		if !ok {
			continue
		}
		card.BreakageCents += breakage
		card.BalanceCents -= breakage
		if card.BalanceCents == 0 {
			card.Status = GiftCardExhausted
		} else {
			card.Status = GiftCardExpired
		}
		l.giftCards[cardNo] = card
		l.giftCardMovements = append(l.giftCardMovements, GiftCardMovement{
			Kind:         "breakage",
			CardNo:       cardNo,
			AmountCents:  breakage,
			BalanceCents: card.BalanceCents,
			At:           now,
		})
		l.emitAudit(AuditEvent{
			Op:       "gift_card_breakage",
			Actor:    "ledger",
			Accounts: []AccountID{cfg.BreakageAccount},
			Details: map[string]any{
				"card_no":        cardNo,
				"breakage_cents": breakage,
				"residual_cents": card.BalanceCents,
				"status":         card.Status,
			},
		})
		report.Broken = append(report.Broken, GiftCardBreakage{
			CardNo:        cardNo,
			BreakageCents: breakage,
			ResidualCents: card.BalanceCents,
			Status:        card.Status,
		})
		var okAdd bool
		report.TotalBreakage, okAdd = addCents(report.TotalBreakage, breakage)
		if !okAdd {
			report.TotalBreakage = 0
		}
		report.TotalResidual, okAdd = addCents(report.TotalResidual, card.BalanceCents)
		if !okAdd {
			report.TotalResidual = 0
		}
	}
	sort.Slice(report.Broken, func(i, j int) bool {
		return report.Broken[i].CardNo < report.Broken[j].CardNo
	})
	return report
}

// GiftCardMovements returns the card movement log for one card, in
// append order: the audit trail of every issue, redeem, and
// breakage. It returns a copy — callers cannot mutate the log
// through it.
func (l *Ledger) GiftCardMovements(cardNo string) []GiftCardMovement {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]GiftCardMovement, 0)
	for _, m := range l.giftCardMovements {
		if m.CardNo == cardNo {
			out = append(out, m)
		}
	}
	return out
}
