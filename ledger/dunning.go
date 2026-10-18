package ledger

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Dunning (collections) escalation (LG-118).
//
// LG-50's aging view decomposes outstanding receivables by age bucket but
// stops at reporting: it never acts. Dunning turns the same FIFO
// attribution into a collections workflow: an overdue receivable on a
// configured (account, currency) escalates through collections levels
// L1/L2/L3 as the oldest outstanding lot's age crosses configurable day
// thresholds, and every escalation is a tamper-evident structured audit
// event (dunning_escalated), so the collections cadence is auditable —
// the dunning state tells a collector exactly which (account, currency)
// sits at which level, and the audit chain tells them when each level
// was reached.
//
// Semantics, kept honest:
//
//   - Level assignment is automatic (derived from the books), level
//     transitions are one-way only: EscalateDunning never lowers a
//     level, even when the outstanding balance drops to zero. A level
//     is cleared only by the operator's explicit ResetDunning call,
//     which is itself audit-logged (dunning_reset). "Paid but still
//     flagged" is a real collections state — the money moved, the file
//     is not closed until a human closes it.
//   - The escalation candidate scan is read-only on the journal: it
//     reads the entries under one RLock the same way Aging does (pending
//     dual-control-review rows are booked but not effective, so they
//     never age a receivable — exactly like the Aging and settlement
//     views). Only the dunning state, the escalation counter, and the
//     audit events are written; the ledger version never moves.
//   - "Days past due" is the age of the oldest outstanding FIFO lot:
//     the oldest cents still owed. A payment retires the oldest lots
//     first (see aging.go), so a freshly paid 90-day invoice does not
//     keep the account at L3 — the lot is gone, the age drops.
//   - Overdue cents on the event and the state is the (account,
//     currency) outstanding balance at escalation time: the receivable
//     being escalated, not a bucket slice of it. The level is driven by
//     age, the cents are the exposure.
//   - Dunning is strictly opt-in per (account, currency): no config,
//     no escalation, no state, no events. The scan iterates only
//     configured pairs, so an unconfigured ledger pays zero cost.
//
// Dunning state is structural config + operational state, so both the
// configs and the states round-trip through ExportSnapshot/ImportSnapshot
// like the other structural config (see snapshot.go): a
// disaster-recovery restore keeps the collections positions it had.

// Dunning levels: 0 means not escalated, 1-3 are the collections levels.
const (
	DunningLevelNone = 0
	DunningLevelL1   = 1
	DunningLevelL2   = 2
	DunningLevelL3   = 3
)

// dunningLevelLabel pins the level labels used by the audit events, the
// Reconcile disclosure, and the tests.
var dunningLevelLabel = map[int]string{
	DunningLevelL1: "L1",
	DunningLevelL2: "L2",
	DunningLevelL3: "L3",
}

// dunningKey identifies one collections position: an (account, currency)
// pair with an opt-in dunning policy.
type dunningKey struct {
	account  AccountID
	currency string
}

// DunningConfig is the opt-in collections escalation policy for one
// (account, currency): the days-past-due thresholds at which an overdue
// receivable escalates to L1, L2, L3. Thresholds must satisfy
// 0 < L1 <= L2 <= L3.
type DunningConfig struct {
	Account    AccountID `json:"account"`
	Currency   string    `json:"currency"`
	Level1Days int64     `json:"level1_days"`
	Level2Days int64     `json:"level2_days"`
	Level3Days int64     `json:"level3_days"`
}

// DunningState is the current collections position of one (account,
// currency): which level it sits at, when it reached that level, the
// overdue exposure that triggered the escalation, and how many
// escalations the position has seen (a position can skip levels when a
// scan catches up with a long-overdue receivable).
type DunningState struct {
	Account      AccountID `json:"account"`
	Currency     string    `json:"currency"`
	Level        int       `json:"level"`
	LevelSince   time.Time `json:"level_since"`
	DaysPastDue  int64     `json:"days_past_due"`
	OverdueCents int64     `json:"overdue_cents"`
	Escalations  int       `json:"escalations"`
}

// DunningEscalation is one level transition performed by a scan: the
// (account, currency), the level before and after, and the exposure that
// triggered it.
type DunningEscalation struct {
	Account       AccountID `json:"account"`
	Currency      string    `json:"currency"`
	PreviousLevel int       `json:"previous_level"`
	Level         int       `json:"level"`
	DaysPastDue   int64     `json:"days_past_due"`
	OverdueCents  int64     `json:"overdue_cents"`
}

// Validation and lifecycle errors for dunning.
var (
	// ErrDunningInvalidConfig is returned by SetDunningConfig when the
	// policy is malformed: an empty account, a bad currency code, or
	// thresholds that do not satisfy 0 < L1 <= L2 <= L3. It is a
	// 400-class field validation error.
	ErrDunningInvalidConfig = errors.New("ledger: dunning config requires a non-empty account, a valid currency, and 0 < L1 <= L2 <= L3 days")
	// ErrDunningNotConfigured is returned by ResetDunning when no
	// dunning policy is configured for the (account, currency): there
	// is no collections position to clear. It is a 404-class error.
	ErrDunningNotConfigured = errors.New("ledger: dunning is not configured for this account and currency")
)

// validateDunningConfig checks the policy fields without normalizing
// them; SetDunningConfig normalizes (currency) before validating.
func validateDunningConfig(cfg DunningConfig) error {
	if cfg.Account == "" {
		return ErrDunningInvalidConfig
	}
	if _, err := normalizeCurrency(cfg.Currency); err != nil {
		return ErrDunningInvalidConfig
	}
	if cfg.Level1Days <= 0 || cfg.Level2Days < cfg.Level1Days || cfg.Level3Days < cfg.Level2Days {
		return ErrDunningInvalidConfig
	}
	return nil
}

// dunningLevelFor maps days-past-due to a dunning level under the given
// thresholds. Callers pass a validated config, so the comparisons are
// total: days >= L3 -> L3, >= L2 -> L2, >= L1 -> L1, else none.
func dunningLevelFor(days int64, cfg DunningConfig) int {
	switch {
	case days >= cfg.Level3Days:
		return DunningLevelL3
	case days >= cfg.Level2Days:
		return DunningLevelL2
	case days >= cfg.Level1Days:
		return DunningLevelL1
	default:
		return DunningLevelNone
	}
}

// SetDunningConfig installs (or replaces) the opt-in collections
// escalation policy for cfg's (account, currency). An existing
// escalation state is kept: tightening thresholds can escalate on the
// next scan, loosening them never de-escalates (levels only move up,
// cleared only by ResetDunning). The config change is recorded in the
// structured audit log's hash chain (op dunning_config_set) like the
// other structural config changes. The currency is normalized (empty ->
// DefaultCurrency, exactly like the journal).
func (l *Ledger) SetDunningConfig(cfg DunningConfig) error {
	return l.SetDunningConfigCtx(context.Background(), cfg)
}

// SetDunningConfigCtx is SetDunningConfig with per-call audit actor
// attribution (LG-73).
func (l *Ledger) SetDunningConfigCtx(ctx context.Context, cfg DunningConfig) error {
	cur, err := normalizeCurrency(cfg.Currency)
	if err != nil {
		return ErrDunningInvalidConfig
	}
	cfg.Currency = cur
	if err := validateDunningConfig(cfg); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.dunningConfigs[dunningKey{account: cfg.Account, currency: cfg.Currency}] = cfg
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "dunning_config_set",
		Actor:         "SetDunningConfig",
		TraceID:       fmt.Sprintf("dunning:%s:%s", cfg.Account, cfg.Currency),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{cfg.Account},
		Details: map[string]any{
			"currency":    cfg.Currency,
			"level1_days": cfg.Level1Days,
			"level2_days": cfg.Level2Days,
			"level3_days": cfg.Level3Days,
		},
	})
	return nil
}

// ClearDunningConfig removes the dunning policy (and any escalation
// state) for the (account, currency): the collections position is gone
// and the pair stops escalating. The removal is audit-logged (op
// dunning_config_cleared). Clearing a pair that was never configured is
// a no-op.
func (l *Ledger) ClearDunningConfig(account AccountID, currency string) error {
	return l.ClearDunningConfigCtx(context.Background(), account, currency)
}

// ClearDunningConfigCtx is ClearDunningConfig with per-call audit actor
// attribution (LG-73).
func (l *Ledger) ClearDunningConfigCtx(ctx context.Context, account AccountID, currency string) error {
	cur, err := normalizeCurrency(currency)
	if err != nil {
		return ErrDunningInvalidConfig
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	k := dunningKey{account: account, currency: cur}
	if _, ok := l.dunningConfigs[k]; !ok {
		return nil
	}
	delete(l.dunningConfigs, k)
	delete(l.dunningStates, k)
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "dunning_config_cleared",
		Actor:         "ClearDunningConfig",
		TraceID:       fmt.Sprintf("dunning:%s:%s", account, cur),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{account},
		Details: map[string]any{
			"currency": cur,
		},
	})
	return nil
}

// DunningConfigOf returns the configured dunning policy for the
// (account, currency), or false when the pair has no policy (dunning is
// strictly opt-in).
func (l *Ledger) DunningConfigOf(account AccountID, currency string) (DunningConfig, bool) {
	cur, err := normalizeCurrency(currency)
	if err != nil {
		return DunningConfig{}, false
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	cfg, ok := l.dunningConfigs[dunningKey{account: account, currency: cur}]
	return cfg, ok
}

// dunningAgeLocked computes the collections age of one (account,
// currency): the age in whole days of the oldest outstanding FIFO lot
// and the outstanding cents. It reuses aging.go's FIFO attribution
// (pending-review rows excluded, oldest-first lot retirement), so the
// dunning age and the aging view can never disagree about which cents
// are still owed. ok is false when nothing is outstanding (the pair is
// not a receivable at asof): paid-off accounts never escalate.
//
// Callers must hold l.mu; the read lock suffices.
func (l *Ledger) dunningAgeLocked(a AccountID, currency string, asof time.Time) (daysPastDue int64, outstanding int64, ok bool) {
	deltas := make([]agingDelta, 0, 64)
	for _, e := range l.entries {
		// Pending-review rows are booked but not effective: balances
		// never see them, so neither may the dunning age — or a parked
		// transfer would escalate a receivable the books do not carry.
		if e.PendingReview {
			continue
		}
		if e.Currency != currency {
			continue
		}
		if e.DebitAccount == a {
			deltas = append(deltas, agingDelta{createdAt: e.CreatedAt, id: e.ID, cents: e.AmountCents})
		} else if e.CreditAccount == a {
			deltas = append(deltas, agingDelta{createdAt: e.CreatedAt, id: e.ID, cents: -e.AmountCents})
		}
	}
	if len(deltas) == 0 {
		return 0, 0, false
	}
	sort.Slice(deltas, func(i, j int) bool {
		if deltas[i].createdAt.Equal(deltas[j].createdAt) {
			return deltas[i].id < deltas[j].id
		}
		return deltas[i].createdAt.Before(deltas[j].createdAt)
	})
	// FIFO lot attribution with the lots' timestamps retained, mirroring
	// attributeAgingLots: debits open lots, credits retire the oldest
	// lots first, an over-applied credit parks as negative carry at its
	// own age.
	type lot struct {
		createdAt time.Time
		cents     int64
	}
	lots := make([]lot, 0, len(deltas))
	var negCarry int64
	for _, d := range deltas {
		switch {
		case d.cents > 0:
			amt := d.cents
			if negCarry < 0 {
				if off := -negCarry; off < amt {
					amt -= off
					negCarry = 0
				} else {
					negCarry += amt
					amt = 0
				}
			}
			if amt > 0 {
				lots = append(lots, lot{createdAt: d.createdAt, cents: amt})
			}
		case d.cents < 0:
			amt := -d.cents
			for amt > 0 && len(lots) > 0 {
				if lots[0].cents <= amt {
					amt -= lots[0].cents
					lots = lots[1:]
				} else {
					lots[0].cents -= amt
					amt = 0
				}
			}
			if amt > 0 {
				negCarry -= amt
			}
		}
	}
	var total int64
	for _, lt := range lots {
		total += lt.cents
	}
	total += negCarry
	if total <= 0 || len(lots) == 0 {
		// Overpaid or settled: not a receivable, nothing to age. The
		// FIFO lots are in oldest-first order, so lots[0] is the oldest
		// outstanding lot whenever one exists.
		return 0, total, false
	}
	days := int64(asof.Sub(lots[0].createdAt) / (24 * time.Hour))
	if days < 0 {
		// Future-dated lot (backdated-future entry): clamp to zero,
		// the aging view's convention (see agingBucketFor). A lot that
		// is not yet due cannot escalate.
		days = 0
	}
	return days, total, true
}

// EscalateDunning runs one collections escalation scan at now: for every
// configured (account, currency) with an outstanding receivable, it
// derives the dunning level from the oldest outstanding lot's age and
// escalates the stored state when the derived level is strictly higher
// than the current one. Every escalation emits a dunning_escalated
// structured audit event (account/currency/level/overdue_cents, plus
// days_past_due and the previous level) and bumps the escalation
// counter; a receivable that jumps two thresholds in one scan emits one
// event at the final level. Levels never move down here — not even when
// the balance hits zero — and a pair with no outstanding receivable is
// simply skipped: the state survives until ResetDunning. The returned
// slice lists the escalations this scan performed, in (account, currency)
// order; it is empty (not nil) when nothing escalated.
func (l *Ledger) EscalateDunning(now time.Time) []DunningEscalation {
	asof := now.UTC()
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]DunningEscalation, 0)
	for k, cfg := range l.dunningConfigs {
		days, outstanding, ok := l.dunningAgeLocked(k.account, k.currency, asof)
		if !ok {
			continue
		}
		level := dunningLevelFor(days, cfg)
		state := l.dunningStates[k]
		if level <= state.Level {
			// Same level, a younger receivable (a payment retired the
			// oldest lot), or loosened thresholds: transitions are
			// one-way up, so nothing to do.
			continue
		}
		prev := state.Level
		l.dunningStates[k] = DunningState{
			Account:      k.account,
			Currency:     k.currency,
			Level:        level,
			LevelSince:   asof,
			DaysPastDue:  days,
			OverdueCents: outstanding,
			Escalations:  state.Escalations + 1,
		}
		l.dunningEscalations++
		l.emitAudit(AuditEvent{
			Op:            "dunning_escalated",
			Actor:         "EscalateDunning",
			TraceID:       fmt.Sprintf("dunning:%s:%s", k.account, k.currency),
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Accounts:      []AccountID{k.account},
			Details: map[string]any{
				"currency":       k.currency,
				"level":          level,
				"level_label":    dunningLevelLabel[level],
				"previous_level": prev,
				"days_past_due":  days,
				"overdue_cents":  outstanding,
			},
		})
		out = append(out, DunningEscalation{
			Account:       k.account,
			Currency:      k.currency,
			PreviousLevel: prev,
			Level:         level,
			DaysPastDue:   days,
			OverdueCents:  outstanding,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Account != out[j].Account {
			return out[i].Account < out[j].Account
		}
		return out[i].Currency < out[j].Currency
	})
	return out
}

// ResetDunning clears the escalation state of a configured (account,
// currency): the operator's explicit "the file is closed" after the
// money came back. The reset is audit-logged (op dunning_reset,
// carrying the level that was cleared), and the escalation counter is
// left alone — it counts escalations, not resets. Resetting a pair with
// no active escalation still logs the reset (the action is the record),
// as long as the pair has a policy; an unconfigured pair returns
// ErrDunningNotConfigured.
func (l *Ledger) ResetDunning(account AccountID, currency string) error {
	return l.ResetDunningCtx(context.Background(), account, currency)
}

// ResetDunningCtx is ResetDunning with per-call audit actor attribution
// (LG-73).
func (l *Ledger) ResetDunningCtx(ctx context.Context, account AccountID, currency string) error {
	cur, err := normalizeCurrency(currency)
	if err != nil {
		return ErrDunningNotConfigured
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	k := dunningKey{account: account, currency: cur}
	if _, ok := l.dunningConfigs[k]; !ok {
		return ErrDunningNotConfigured
	}
	prev := l.dunningStates[k]
	delete(l.dunningStates, k)
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "dunning_reset",
		Actor:         "ResetDunning",
		TraceID:       fmt.Sprintf("dunning:%s:%s", account, cur),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{account},
		Details: map[string]any{
			"currency":      cur,
			"cleared_level": prev.Level,
		},
	})
	return nil
}

// DunningStatusOf returns the current escalation state of the (account,
// currency), or false when the pair has no active escalation.
func (l *Ledger) DunningStatusOf(account AccountID, currency string) (DunningState, bool) {
	cur, err := normalizeCurrency(currency)
	if err != nil {
		return DunningState{}, false
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	st, ok := l.dunningStates[dunningKey{account: account, currency: cur}]
	return st, ok
}

// DunningEscalationCount reports how many dunning escalations have ever
// fired on this ledger: the process-level /metrics scrape mirrors it
// into ledger_dunning_escalations_total.
func (l *Ledger) DunningEscalationCount() uint64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.dunningEscalations
}

// DunningDisclosure is one escalated collections position as disclosed
// by Reconcile: which (account, currency), at which collections level,
// the overdue exposure that triggered the escalation, and when the
// level was reached.
type DunningDisclosure struct {
	Account      AccountID `json:"account"`
	Currency     string    `json:"currency"`
	Level        int       `json:"level"`
	LevelLabel   string    `json:"level_label"`
	DaysPastDue  int64     `json:"days_past_due"`
	OverdueCents int64     `json:"overdue_cents"`
	LevelSince   time.Time `json:"level_since"`
	Escalations  int       `json:"escalations"`
}

// dunningStatesLocked returns the active escalation states sorted by
// (account, currency). Callers must hold l.mu; the read lock suffices.
func (l *Ledger) dunningStatesLocked() []DunningState {
	out := make([]DunningState, 0, len(l.dunningStates))
	for _, st := range l.dunningStates {
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Account != out[j].Account {
			return out[i].Account < out[j].Account
		}
		return out[i].Currency < out[j].Currency
	})
	return out
}

// dunningDisclosuresLocked builds the Reconcile collections disclosure:
// every (account, currency) with an active escalation, sorted by
// (account, currency). Callers must hold l.mu; the read lock suffices.
func (l *Ledger) dunningDisclosuresLocked() []DunningDisclosure {
	states := l.dunningStatesLocked()
	out := make([]DunningDisclosure, 0, len(states))
	for _, st := range states {
		out = append(out, DunningDisclosure{
			Account:      st.Account,
			Currency:     st.Currency,
			Level:        st.Level,
			LevelLabel:   dunningLevelLabel[st.Level],
			DaysPastDue:  st.DaysPastDue,
			OverdueCents: st.OverdueCents,
			LevelSince:   st.LevelSince,
			Escalations:  st.Escalations,
		})
	}
	return out
}

// dunningConfigsLocked returns the configured dunning policies sorted by
// (account, currency), for the snapshot export. Callers must hold l.mu;
// the read lock suffices.
func (l *Ledger) dunningConfigsLocked() []DunningConfig {
	out := make([]DunningConfig, 0, len(l.dunningConfigs))
	for _, cfg := range l.dunningConfigs {
		out = append(out, cfg)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Account != out[j].Account {
			return out[i].Account < out[j].Account
		}
		return out[i].Currency < out[j].Currency
	})
	return out
}
