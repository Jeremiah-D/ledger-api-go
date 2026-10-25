package ledger

import (
	"errors"
	"math"
	"math/bits"
	"sort"
	"strings"
	"time"
)

// Loyalty points: a points liability kept strictly apart from cash
// balances (LG-215).
//
// A loyalty program issues points — not cash — to an account, and the
// account later redeems them for a cash-value credit or lets them
// expire into breakage revenue. Points are a liability of the program,
// not of any cash account, so they never touch the journal: booking a
// redemption as a journal entry would move cash that never moved and
// break the accounting equation. Instead the points sub-ledger below
// keeps its own double entry: every movement records both legs at
// once — the points-liability debit and the cash-value credit to the
// program's revenue (redeem) or breakage (expire) account — applied
// atomically under the ledger write lock, so a movement can never
// leave one leg booked without the other. The cash value is a
// valuation, not a cash movement: the cash accounts named here are
// reporting targets for the credit leg, and cash balances are
// untouched (see the test asserting Balance and
// VerifyAccountingEquation are unchanged by points operations).
//
// Rate changes never rewrite issued points: each IssuePoints call mints
// one lot carrying the program's conversion rate at issue time, and
// redemptions consume lots oldest-first (FIFO), valuing each consumed
// lot at its own issue-time rate. Re-registering a program changes
// the rate for future issues only; lots already minted keep theirs.
// A redemption's cash value is therefore the sum over consumed lots
// of points times lot rate, never the current rate times the total.
//
// Expiry is inactivity-based, not age-based: ExpirePoints expires the
// whole unused balance of an (account, program) pair whose last
// points activity (issue or redeem) is strictly older than the
// program's inactivity window. A single recent redemption keeps the
// whole balance alive — the window measures the account's
// relationship with the program, not each lot's birthday. Expired
// points become breakage revenue at their lots' issue-time rates,
// exactly like a redemption, except the credit lands on the
// program's breakage account instead of its revenue account.
//
// Orthogonality: dividend (see dividend.go) and rebate tiers (see
// rebate_tiers.go) are cash distributions — they move journal money.
// Escheatment (see escheat.go, LG-109) remits dormant cash to the
// state. Points expiry is neither: it writes off a points liability
// to the merchant's own breakage revenue, and it never touches the
// escheat path.
//
// State is in-memory only, like payout instructions and gpi tracks:
// programs, lots, activity stamps, and the movement log do not
// round-trip through disaster-recovery snapshots. The movement log
// plus the loyalty audit events are the durable record when the
// audit log is enabled (see audit.go); without an audit log the
// movements still survive in memory for the process lifetime.

// LoyaltyProgram is one program's configuration: the cash value of a
// point, the inactivity window after which unused points expire, and
// the accounts credited on redemption and on expiry.
type LoyaltyProgram struct {
	// Name identifies the program. It must be non-empty and must not
	// contain '/': movement IDs join their components with '/'.
	Name string `json:"name"`
	// CentsPerPoint is the cash value of one point in cents, fixed
	// into each lot at issue time.
	CentsPerPoint int64 `json:"cents_per_point"`
	// InactivityDays is the inactivity window in days: an (account,
	// program) balance whose last points activity is strictly older
	// than this many days expires wholesale.
	InactivityDays int `json:"inactivity_days"`
	// RevenueAccount is credited (by valuation) when points are
	// redeemed: the income/discount account for redeemed value.
	RevenueAccount AccountID `json:"revenue_account"`
	// BreakageAccount is credited (by valuation) when points expire:
	// the breakage-revenue account for expired value.
	BreakageAccount AccountID `json:"breakage_account"`
}

// loyaltyKey identifies one account's balance in one program.
type loyaltyKey struct {
	account AccountID
	program string
}

// loyaltyLot is one IssuePoints minting: the points still unused and
// the conversion rate fixed at issue time. Lots are consumed
// oldest-first; the slice order is the minting order.
type loyaltyLot struct {
	points        int64
	centsPerPoint int64
}

// LoyaltyMovement is one atomic points movement: the points leg and
// the cash-value leg, recorded together. Kind is "issue", "redeem",
// or "expire". For an issue the cash leg is empty (CashCents 0, no
// cash account): issuing points creates a liability but credits no
// revenue. For a redeem the cash leg credits RevenueAccount; for an
// expire it credits BreakageAccount.
type LoyaltyMovement struct {
	Kind        string    `json:"kind"`
	Account     AccountID `json:"account"`
	Program     string    `json:"program"`
	Points      int64     `json:"points"`
	CashCents   int64     `json:"cash_cents"`
	CashAccount AccountID `json:"cash_account"`
	At          time.Time `json:"at"`
}

// LoyaltyExpiry is one (account, program) balance expired by
// ExpirePoints: the points written off and their breakage value.
type LoyaltyExpiry struct {
	Account         AccountID `json:"account"`
	Program         string    `json:"program"`
	Points          int64     `json:"points"`
	CashCents       int64     `json:"cash_cents"`
	BreakageAccount AccountID `json:"breakage_account"`
}

// LoyaltyExpiryReport is ExpirePoints' result: the sweep time and the
// balances expired, sorted by account then program.
type LoyaltyExpiryReport struct {
	ExpiredAt   time.Time       `json:"expired_at"`
	Expired     []LoyaltyExpiry `json:"expired"`
	TotalPoints int64           `json:"total_points"`
	TotalCash   int64           `json:"total_cash_cents"`
}

// Loyalty validation errors.
var (
	// ErrUnknownLoyaltyProgram is returned when points are issued or
	// redeemed under a program that was never registered: points
	// cannot exist without a program fixing their value.
	ErrUnknownLoyaltyProgram = errors.New("ledger: unknown loyalty program")
	// ErrInvalidLoyaltyProgram is returned when a program's config
	// fails validation: empty name, a '/' in the name, a
	// non-positive conversion rate or inactivity window, or an
	// empty revenue or breakage account.
	ErrInvalidLoyaltyProgram = errors.New("ledger: invalid loyalty program configuration")
	// ErrInvalidLoyaltyPoints is returned when an issue or redeem
	// amount is not positive: zero-point movements carry no
	// information and negative points are not a thing.
	ErrInvalidLoyaltyPoints = errors.New("ledger: loyalty points must be positive")
	// ErrInsufficientLoyaltyPoints is returned when a redemption
	// exceeds the account's unused balance in the program.
	ErrInsufficientLoyaltyPoints = errors.New("ledger: insufficient loyalty points")
	// ErrLoyaltyPointsOverflow is returned when a points-to-cash
	// valuation overflows int64: the movement is rejected rather
	// than booked at a wrapped value.
	ErrLoyaltyPointsOverflow = errors.New("ledger: loyalty points valuation overflows int64")
)

// mulPointsCents multiplies non-negative points by a non-negative
// cents-per-point rate, reporting overflow instead of wrapping.
func mulPointsCents(points, rate int64) (int64, bool) {
	hi, lo := bits.Mul64(uint64(points), uint64(rate))
	if hi != 0 || lo > uint64(math.MaxInt64) {
		return 0, false
	}
	return int64(lo), true
}

// checkLoyaltyProgram validates a program config before it is stored.
func checkLoyaltyProgram(p LoyaltyProgram) error {
	if p.Name == "" || strings.Contains(p.Name, "/") {
		return ErrInvalidLoyaltyProgram
	}
	if p.CentsPerPoint <= 0 || p.InactivityDays <= 0 {
		return ErrInvalidLoyaltyProgram
	}
	if p.RevenueAccount == "" || p.BreakageAccount == "" {
		return ErrInvalidLoyaltyProgram
	}
	return nil
}

// RegisterLoyaltyProgram stores a program's config, upserting by name:
// re-registering changes the conversion rate and window for future
// issues only — lots already minted keep their issue-time rate, so a
// rate change never rewrites the value of issued points.
func (l *Ledger) RegisterLoyaltyProgram(p LoyaltyProgram) error {
	if err := checkLoyaltyProgram(p); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.loyaltyPrograms[p.Name] = p
	l.emitAudit(AuditEvent{
		Op:       "loyalty_program_registered",
		Actor:    "ledger",
		Accounts: []AccountID{p.RevenueAccount, p.BreakageAccount},
		Details: map[string]any{
			"program":         p.Name,
			"cents_per_point": p.CentsPerPoint,
			"inactivity_days": p.InactivityDays,
		},
	})
	return nil
}

// LoyaltyProgram returns a registered program's config, reporting
// whether it exists.
func (l *Ledger) LoyaltyProgram(name string) (LoyaltyProgram, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	p, ok := l.loyaltyPrograms[name]
	return p, ok
}

// loyaltyBalanceLocked sums the unused lots for one (account,
// program) pair. Callers must hold l.mu.
func (l *Ledger) loyaltyBalanceLocked(key loyaltyKey) int64 {
	var total int64
	for _, lot := range l.loyaltyLots[key] {
		var ok bool
		total, ok = addCents(total, lot.points)
		if !ok {
			return 0
		}
	}
	return total
}

// PointsBalance returns an account's unused points in a program: the
// points balance, kept apart from the cash balance (Balance is
// untouched by points operations). An unregistered program or an
// account with no lots reports 0, like Balance for an unknown
// account.
func (l *Ledger) PointsBalance(account AccountID, program string) int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.loyaltyBalanceLocked(loyaltyKey{account: account, program: program})
}

// IssuePoints mints points into an account's balance under a
// registered program: one lot at the program's current conversion
// rate, stamped with the account's latest activity time. The cash
// leg is empty — issuing points creates a liability, not revenue.
func (l *Ledger) IssuePoints(account AccountID, points int64, program string) (LoyaltyMovement, error) {
	if account == "" {
		return LoyaltyMovement{}, ErrEmptyCreditAccount
	}
	if points <= 0 {
		return LoyaltyMovement{}, ErrInvalidLoyaltyPoints
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	prog, ok := l.loyaltyPrograms[program]
	if !ok {
		return LoyaltyMovement{}, ErrUnknownLoyaltyProgram
	}
	key := loyaltyKey{account: account, program: program}
	l.loyaltyLots[key] = append(l.loyaltyLots[key], loyaltyLot{
		points:        points,
		centsPerPoint: prog.CentsPerPoint,
	})
	now := time.Now().UTC()
	l.loyaltyActivity[key] = now
	m := LoyaltyMovement{
		Kind:    "issue",
		Account: account,
		Program: program,
		Points:  points,
		At:      now,
	}
	l.loyaltyMovements = append(l.loyaltyMovements, m)
	l.emitAudit(AuditEvent{
		Op:       "loyalty_points_issued",
		Actor:    "ledger",
		Accounts: []AccountID{account},
		Details: map[string]any{
			"program":         program,
			"points":          points,
			"cents_per_point": prog.CentsPerPoint,
		},
	})
	return m, nil
}

// RedeemPoints spends unused points: lots are consumed oldest-first
// and each consumed lot is valued at its own issue-time rate, so a
// program rate change never rewrites issued value. The movement's
// two legs are applied together under one lock — the points debit
// and the revenue credit — and the caller sees both or neither: a
// failed valuation or an insufficient balance books nothing.
func (l *Ledger) RedeemPoints(account AccountID, points int64, program string) (LoyaltyMovement, error) {
	if account == "" {
		return LoyaltyMovement{}, ErrEmptyCreditAccount
	}
	if points <= 0 {
		return LoyaltyMovement{}, ErrInvalidLoyaltyPoints
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	prog, ok := l.loyaltyPrograms[program]
	if !ok {
		return LoyaltyMovement{}, ErrUnknownLoyaltyProgram
	}
	key := loyaltyKey{account: account, program: program}
	lots := l.loyaltyLots[key]
	if l.loyaltyBalanceLocked(key) < points {
		return LoyaltyMovement{}, ErrInsufficientLoyaltyPoints
	}
	// Consume oldest-first, valuing each lot at its issue-time rate.
	// Work on a copy so a valuation overflow leaves the lots intact.
	remaining := points
	var cash int64
	consumed := make([]loyaltyLot, len(lots))
	copy(consumed, lots)
	for i := range consumed {
		if remaining == 0 {
			break
		}
		take := consumed[i].points
		if take > remaining {
			take = remaining
		}
		value, ok := mulPointsCents(take, consumed[i].centsPerPoint)
		if !ok {
			return LoyaltyMovement{}, ErrLoyaltyPointsOverflow
		}
		var okAdd bool
		cash, okAdd = addCents(cash, value)
		if !okAdd {
			return LoyaltyMovement{}, ErrLoyaltyPointsOverflow
		}
		consumed[i].points -= take
		remaining -= take
	}
	// Commit: drop fully consumed lots, keep the partial remainder.
	kept := consumed[:0]
	for _, lot := range consumed {
		if lot.points > 0 {
			kept = append(kept, lot)
		}
	}
	l.loyaltyLots[key] = kept
	now := time.Now().UTC()
	l.loyaltyActivity[key] = now
	m := LoyaltyMovement{
		Kind:        "redeem",
		Account:     account,
		Program:     program,
		Points:      points,
		CashCents:   cash,
		CashAccount: prog.RevenueAccount,
		At:          now,
	}
	l.loyaltyMovements = append(l.loyaltyMovements, m)
	l.emitAudit(AuditEvent{
		Op:       "loyalty_points_redeemed",
		Actor:    "ledger",
		Accounts: []AccountID{account, prog.RevenueAccount},
		Details: map[string]any{
			"program":      program,
			"points":       points,
			"cash_cents":   cash,
			"cash_account": string(prog.RevenueAccount),
		},
	})
	return m, nil
}

// ExpirePoints writes off the unused balances of (account, program)
// pairs whose last points activity is strictly older than the
// program's inactivity window at now. Each expired balance becomes
// breakage revenue at its lots' issue-time rates, credited to the
// program's breakage account — never to the escheat path (see
// escheat.go: escheatment remits dormant cash to the state; points
// expiry writes off a merchant points liability to the merchant).
// Pairs with no remaining balance, with recent activity, or under a
// program that no longer exists are left alone: expiry only ever
// writes off live, provably inactive points.
func (l *Ledger) ExpirePoints(now time.Time) LoyaltyExpiryReport {
	l.mu.Lock()
	defer l.mu.Unlock()
	report := LoyaltyExpiryReport{ExpiredAt: now, Expired: []LoyaltyExpiry{}}
	for key, lots := range l.loyaltyLots {
		prog, ok := l.loyaltyPrograms[key.program]
		if !ok {
			continue
		}
		balance := l.loyaltyBalanceLocked(key)
		if balance <= 0 {
			continue
		}
		last, seen := l.loyaltyActivity[key]
		if !seen {
			continue
		}
		window := time.Duration(prog.InactivityDays) * 24 * time.Hour
		if now.Sub(last) <= window {
			continue
		}
		var cash int64
		var okAdd bool
		for _, lot := range lots {
			if lot.points == 0 {
				continue
			}
			value, ok := mulPointsCents(lot.points, lot.centsPerPoint)
			if !ok {
				// A valuation overflow must not silently write off
				// value: skip this pair rather than booking a
				// wrapped breakage figure.
				cash = -1
				break
			}
			cash, okAdd = addCents(cash, value)
			if !okAdd {
				cash = -1
				break
			}
		}
		if cash < 0 {
			continue
		}
		delete(l.loyaltyLots, key)
		m := LoyaltyMovement{
			Kind:        "expire",
			Account:     key.account,
			Program:     key.program,
			Points:      balance,
			CashCents:   cash,
			CashAccount: prog.BreakageAccount,
			At:          now,
		}
		l.loyaltyMovements = append(l.loyaltyMovements, m)
		l.emitAudit(AuditEvent{
			Op:       "loyalty_points_expired",
			Actor:    "ledger",
			Accounts: []AccountID{key.account, prog.BreakageAccount},
			Details: map[string]any{
				"program":          key.program,
				"points":           balance,
				"cash_cents":       cash,
				"breakage_account": string(prog.BreakageAccount),
			},
		})
		report.Expired = append(report.Expired, LoyaltyExpiry{
			Account:         key.account,
			Program:         key.program,
			Points:          balance,
			CashCents:       cash,
			BreakageAccount: prog.BreakageAccount,
		})
		var ok2 bool
		report.TotalPoints, ok2 = addCents(report.TotalPoints, balance)
		if !ok2 {
			report.TotalPoints = 0
		}
		report.TotalCash, ok2 = addCents(report.TotalCash, cash)
		if !ok2 {
			report.TotalCash = 0
		}
	}
	sort.Slice(report.Expired, func(i, j int) bool {
		if report.Expired[i].Account != report.Expired[j].Account {
			return report.Expired[i].Account < report.Expired[j].Account
		}
		return report.Expired[i].Program < report.Expired[j].Program
	})
	return report
}

// LoyaltyMovements returns the points movement log for one (account,
// program) pair, in append order: the audit trail of every issue,
// redeem, and expire. It returns a copy — callers cannot mutate the
// log through it.
func (l *Ledger) LoyaltyMovements(account AccountID, program string) []LoyaltyMovement {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]LoyaltyMovement, 0)
	for _, m := range l.loyaltyMovements {
		if m.Account == account && m.Program == program {
			out = append(out, m)
		}
	}
	return out
}
