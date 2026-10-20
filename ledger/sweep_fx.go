package ledger

import (
	"errors"
	"sort"
	"strings"
	"time"
)

// Cross-currency balance sweeps (LG-153).
//
// PostSweepFX is the FX twin of PostSweep (see sweep.go): every source
// account's positive per-currency balance is converted into one target
// currency at the ledger's configured FX rate table (see fx.go, LG-27)
// and moved to the target account — one FX leg-pair per
// (source, currency) — under a single sweep ID, atomically.
//
// Booking shape reuses transfer FX semantics: per (source, currency)
// leg, two single-currency journal entries that keep each currency's
// books balanced independently, with the FX clearing account as the
// shared counterparty:
//
//	principal: ID "<sweep ID>/<source>/<currency>",
//	          debit To@toCurrency convertedCents,
//	          credit FXAccount@toCurrency convertedCents
//	fx leg:    ID "<sweep ID>/<source>/<currency>/fx",
//	          debit FXAccount@sourceCurrency amountCents,
//	          credit From@sourceCurrency amountCents
//
// A (source, currency) that is already in the target currency books a
// single same-currency entry (debit To, credit From) under the principal
// ID: no rate lookup, no FX leg, no conversion — identity, not a rate of
// 1. The receipt marks it with Direction "same_currency" in the per-leg
// FX disclosure, so no consumer mistakes it for a consulted rate.
//
// Pricing mirrors a cross-currency transfer with the default FXSide
// (see fx_spread.go): the sweep "sells" its foreign balances, so the
// pair's bid is the dealing price (FXSideSell). A mid-only rate deals
// at mid on both sides (bid == mid), so legacy single-rate tables
// behave exactly as before. Unlike a transfer, no spread leg is booked:
// the target receives the full dealing-price conversion, and the
// spread is disclosed on the receipt, never taken. Any implicit
// spread between the booked legs accumulates in the FX account's
// per-currency balances, visible in TrialBalance and Reconcile like any
// other account.
//
// Like a plain sweep, SweepFX never charges a transfer fee: it is an
// internal treasury movement, so the ledger-wide transfer fee policy
// does not apply. Only positive balances move; zero and negative
// balances are skipped (a negative balance means the account owes money
// — the caller settles negatives explicitly). The check order mirrors
// PostSweep: field validation first, then the idempotency replay check
// (a replayed key returns the original receipt even if an account was
// frozen after the original sweep), then the sweep-ID conflict check,
// then the closed/frozen/suspended checks on every account touched
// (all sources, the target, and the FX account), then sweep reserve
// floors, then the rate resolution per (source, currency) — a pair with
// no configured rate is rejected with ErrFXRateMissing (HTTP 422), an
// expired rate with ErrFXRateExpired — then the monthly spend budget
// (source currency), the period gate, and the automatic posting rules
// (PostingEventSweep, in the source currency, like the plain sweep).
// A rejected FX sweep records nothing: no journal rows, no chain links,
// no version bump.

// FX-side marker for the per-leg receipt disclosure: an identity
// conversion, not a consulted rate.
const sweepFXSameCurrency = "same_currency"

// Validation errors returned by PostSweepFX.
var (
	// ErrEmptySweepFXCurrency is returned when the FX sweep names no
	// target currency. Unlike journal fields, the target currency has no
	// ledger-wide default here: an empty target would silently define
	// the whole conversion, so it is a 400-class caller error instead.
	ErrEmptySweepFXCurrency = errors.New("ledger: sweep_fx requires a target currency")
)

// SweepFX describes one atomic cross-currency balance sweep: the
// positive per-currency balances of the source accounts are converted
// into ToCurrency and moved to the target account, one FX leg-pair per
// (source, currency).
type SweepFX struct {
	ID string `json:"sweep_id"`
	// From lists the source accounts in sweep order. Entries commit in
	// this order, with currencies sorted within each source, so the
	// receipt is deterministic.
	From []AccountID `json:"from_accounts"`
	// To is the target account that receives every converted leg.
	To AccountID `json:"to_account"`
	// ToCurrency is the settlement currency every leg converts into
	// (ISO 4217, normalized like any other currency input).
	ToCurrency string `json:"to_currency"`
	// FXAccount is the FX clearing account used as the counterparty of
	// the conversion legs. Empty selects the ledger-wide configured
	// account (see WithFXAccount); none configured is a 400-class
	// caller error (ErrFXAccountNotConfigured).
	FXAccount      AccountID `json:"fx_account,omitempty"`
	IdempotencyKey string    `json:"idempotency_key,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// SweepFXLeg is one committed conversion inside a cross-currency sweep:
// the full positive balance of From in SourceCurrency, converted into
// ToCurrency for the sweep's target.
type SweepFXLeg struct {
	From             AccountID `json:"from_account"`
	SourceCurrency   string    `json:"source_currency"`
	AmountCents      int64     `json:"amount_cents"`
	ConvertedCents   int64     `json:"converted_cents"`
	ToCurrency       string    `json:"to_currency"`
	PrincipalEntryID string    `json:"principal_entry_id"`
	// FXEntryID is the conversion leg's journal entry:
	// "<principal entry ID>/fx". Empty for same-currency legs, which
	// book a single entry.
	FXEntryID string `json:"fx_entry_id,omitempty"`
}

// SweepFXReceipt reports what PostSweepFX committed.
//
// Legs, Entries, and FX are in commit order: source order, currencies
// sorted within each source, the FX leg right after its principal. A
// sweep whose sources all had non-positive balances commits nothing —
// Legs, Entries, and FX are empty — but still registers its idempotency
// key, so a replay returns the same empty receipt instead of sweeping
// balances that arrived later. Duplicate replays return the originally
// posted legs with Duplicate == true and book nothing new.
type SweepFXReceipt struct {
	SweepID    string         `json:"sweep_id"`
	ToCurrency string         `json:"to_currency"`
	FXAccount  AccountID      `json:"fx_account"`
	Legs       []SweepFXLeg   `json:"legs"`
	Entries    []JournalEntry `json:"entries"`
	// FX discloses the conversion behind each leg, in leg order (see
	// FXConversion): the rate that applied, its effective version and
	// expiry, the dealing side, and the settled amounts on both sides.
	// Same-currency legs carry Direction "same_currency" (identity, no
	// rate consulted). Idempotent replays return FX == nil: the rate
	// table may have moved since, and the replayed journal entries are
	// the authoritative record (the transfer-FX convention).
	FX []FXConversion `json:"fx,omitempty"`
	// RuleLegs discloses the automatic posting-rule legs the sweep
	// booked (see ledger/posting_rules.go). Nil when no rule fired.
	// Idempotent replays rebuild the same disclosure from the
	// journaled legs' "/rule:" IDs.
	RuleLegs  []PostingRuleLegInfo `json:"rule_legs,omitempty"`
	Duplicate bool                 `json:"duplicate"`
}

// sweepFXRecord remembers what an FX sweep posted so idempotent replays
// can rebuild the receipt. Sweep-FX keys live in their own namespace
// (sweepFXKeys), independent of the sweep/entry/transfer/hold key
// namespaces, and expire with the TTL like every other key index (see
// pruneSweepFXKeysLocked).
type sweepFXRecord struct {
	sweepID    string
	toCurrency string
	fxAccount  AccountID
	entryIDs   []string
	createdAt  time.Time
}

// PostSweepFX atomically sweeps the positive per-currency balances of
// the source accounts into the target account, converting every leg
// into ToCurrency at the ledger's configured FX rate table.
//
// The check order mirrors PostSweep and postTransferFXLocked: field
// validation first, then the idempotency replay check (a replayed key
// returns the original receipt even if an account was frozen after the
// original sweep, because the replay books nothing new), then the
// sweep-ID conflict check, then the closed/frozen/suspended checks on
// every account the sweep touches (all sources, the target, and the FX
// clearing account), then sweep reserve floors, then the rate
// resolution per (source, currency) leg — a missing rate is
// ErrFXRateMissing (422), an expired rate ErrFXRateExpired (422) — then
// the monthly spend budget (source currency), the period gate, and the
// automatic posting rules (PostingEventSweep). Rejected sweeps record
// nothing: no journal rows, no chain links, no version bump. A zero
// CreatedAt is filled with the current time.
func (l *Ledger) PostSweepFX(s SweepFX) (SweepFXReceipt, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if s.ID == "" {
		return SweepFXReceipt{}, ErrEmptySweepID
	}
	if len(s.From) == 0 {
		return SweepFXReceipt{}, ErrEmptySweepSources
	}
	if s.To == "" {
		return SweepFXReceipt{}, ErrEmptySweepTarget
	}
	if s.ToCurrency == "" {
		return SweepFXReceipt{}, ErrEmptySweepFXCurrency
	}
	toCurrency, err := normalizeCurrency(s.ToCurrency)
	if err != nil {
		return SweepFXReceipt{}, err
	}
	seen := make(map[AccountID]bool, len(s.From))
	for _, from := range s.From {
		if from == "" {
			return SweepFXReceipt{}, ErrEmptySweepSource
		}
		if from == s.To {
			return SweepFXReceipt{}, ErrSweepTargetIsSource
		}
		if seen[from] {
			return SweepFXReceipt{}, ErrDuplicateSweepSource
		}
		seen[from] = true
	}
	fxAccount := s.FXAccount
	if fxAccount == "" {
		fxAccount = l.fxAccount
	}
	if fxAccount == "" {
		return SweepFXReceipt{}, ErrFXAccountNotConfigured
	}
	if fxAccount == s.To || seen[fxAccount] {
		return SweepFXReceipt{}, ErrInvalidFXAccount
	}

	// The leg plan is computed before any check that depends on ledger
	// state, but after field validation: balances are read under the
	// write lock, so the plan describes exactly what the commit will
	// book — no concurrent Post can slip in between.
	legs := l.sweepFXLegsLocked(s, toCurrency)

	if s.IdempotencyKey != "" {
		if rec, ok := l.sweepFXKeys[s.IdempotencyKey]; ok {
			entries := make([]JournalEntry, 0, len(rec.entryIDs))
			for _, id := range rec.entryIDs {
				entries = append(entries, l.entries[id])
			}
			return SweepFXReceipt{
				SweepID:    rec.sweepID,
				ToCurrency: rec.toCurrency,
				FXAccount:  rec.fxAccount,
				Legs:       sweepFXLegsOf(entries),
				Entries:    entries,
				Duplicate:  true,
				RuleLegs:   postingRuleLegInfosFromEntries(entries),
			}, nil
		}
	}

	if _, exists := l.entries[s.ID]; exists {
		return SweepFXReceipt{}, ErrSweepIDConflict
	}
	for _, leg := range legs {
		if _, exists := l.entries[leg.PrincipalEntryID]; exists {
			return SweepFXReceipt{}, ErrSweepIDConflict
		}
		if leg.FXEntryID != "" {
			if _, exists := l.entries[leg.FXEntryID]; exists {
				return SweepFXReceipt{}, ErrSweepIDConflict
			}
		}
	}

	if l.closedLocked(s.To) || l.closedLocked(fxAccount) {
		return SweepFXReceipt{}, ErrAccountClosed
	}
	for _, from := range s.From {
		if l.closedLocked(from) {
			return SweepFXReceipt{}, ErrAccountClosed
		}
	}
	if l.frozenLocked(s.To) || l.frozenLocked(fxAccount) {
		return SweepFXReceipt{}, ErrAccountFrozen
	}
	if l.suspendedLocked(s.To) || l.suspendedLocked(fxAccount) {
		return SweepFXReceipt{}, ErrAccountSuspended
	}
	for _, from := range s.From {
		if l.frozenLocked(from) {
			return SweepFXReceipt{}, ErrAccountFrozen
		}
		if l.suspendedLocked(from) {
			return SweepFXReceipt{}, ErrAccountSuspended
		}
	}

	// Sweep reserve floors (see sweep_reserve.go) shrink the planned legs
	// after the frozen checks and before the rate resolution: a leg whose
	// (source, currency) carries a reserve converts only the balance in
	// excess of the floor, and a leg the floor consumes entirely
	// contributes no conversion. The reserve calculation runs here — not
	// in sweepFXLegsLocked — so a frozen source rejects first: a frozen
	// account never silently sweeps a reduced amount, and a zero-leg
	// sweep still succeeds with an empty receipt.
	plainLegs := sweepFXToPlainLegs(legs)
	reserved := l.applySweepReservesLocked(plainLegs)
	byPrincipal := make(map[string]*SweepFXLeg, len(legs))
	for i := range legs {
		byPrincipal[legs[i].PrincipalEntryID] = &legs[i]
	}
	legs = legs[:0]
	for _, pl := range reserved {
		fxLeg := byPrincipal[pl.EntryID]
		fxLeg.AmountCents = pl.AmountCents
		legs = append(legs, *fxLeg)
	}

	// The sweep's effective post time is finalized before the rate
	// resolution and the budget and period checks: conversions are
	// priced and gated at the sweep's own instant, and the period gate
	// is keyed on the same instant.
	createdAt := s.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}

	// Rate resolution per leg, before any budget or period check: a
	// semantic FX rejection (missing or expired rate) records nothing.
	// Same-currency legs skip the table entirely — identity, not a rate.
	type legConversion struct {
		converted   int64
		rate        ExchangeRate
		num, den    int64
		bidNum      int64
		bidDen      int64
		askNum      int64
		askDen      int64
		midNum      int64
		midDen      int64
		effVersion  uint64
		expiresAt   time.Time
		rateSnapVer uint64
	}
	conversions := make([]legConversion, len(legs))
	for i, leg := range legs {
		if leg.SourceCurrency == toCurrency {
			conversions[i] = legConversion{converted: leg.AmountCents}
			continue
		}
		eff, err := l.effectiveFXRateLocked(
			fxPair{from: leg.SourceCurrency, to: toCurrency}, FXSideSell, createdAt)
		if err != nil {
			// ErrFXRateMissing / ErrFXRateExpired: the transfer-FX
			// gate shapes, 422-class semantic rejections.
			return SweepFXReceipt{}, err
		}
		converted, ok := fxConvertCents(leg.AmountCents, eff.Num, eff.Den)
		if !ok {
			return SweepFXReceipt{}, ErrAmountOverflow
		}
		conversions[i] = legConversion{
			converted:   converted,
			rate:        eff.Rate,
			num:         eff.Num,
			den:         eff.Den,
			bidNum:      eff.BidNum,
			bidDen:      eff.BidDen,
			askNum:      eff.AskNum,
			askDen:      eff.AskDen,
			midNum:      eff.MidNum,
			midDen:      eff.MidDen,
			effVersion:  eff.EffectiveVersion,
			expiresAt:   eff.ExpiresAt,
			rateSnapVer: l.fxSnapshotVersion,
		}
	}

	// Monthly spend budgets guard every source leg: a sweep whose source
	// leg would take the account's cumulative spend for the UTC calendar
	// month above its configured budget is rejected with
	// ErrBudgetExceeded. Legs are checked in order with the sweep's
	// planned spend carried forward, so earlier legs consume the
	// month's budget for later ones. The check runs after the frozen
	// checks and before the period gate — like the other risk controls,
	// it only evaluates sweeps that book something new (idempotent
	// replays returned above). Budgets are tracked in the source
	// currency: the outflow the budget guards is what leaves the
	// source, not what arrives converted.
	for i := range legs {
		legs[i].ConvertedCents = conversions[i].converted
	}
	sweepBudgetSpend := make(map[monthlySpendKey]int64)
	for _, leg := range legs {
		if l.budgetViolatedLocked(leg.From, leg.SourceCurrency, leg.AmountCents, createdAt, sweepBudgetSpend) {
			return SweepFXReceipt{}, ErrBudgetExceeded
		}
		bucket := monthlySpendKey{account: leg.From, currency: leg.SourceCurrency, month: utcMonth(createdAt)}
		sweepBudgetSpend[bucket] = satAddInt64(sweepBudgetSpend[bucket], leg.AmountCents)
	}

	// The period gate is keyed on the sweep's effective post time (see
	// period.go): a backdated sweep landing in a closed accounting period
	// is rejected with ErrPeriodClosed. It runs after the idempotency
	// replay check above, so replaying a key swept before the period
	// closed returns the original receipt.
	if err := l.periodRejectedLocked(createdAt); err != nil {
		return SweepFXReceipt{}, err
	}

	// Automatic posting rules (see posting_rules.go): each sweep leg
	// that matches an installed rule appends its derived legs to the
	// commit — one rule evaluation per (source, currency) leg. Evaluated
	// after every gate and before the first mutation, so a failing rule
	// leg rejects the whole sweep with nothing recorded. The rule
	// context is the source currency: rule legs book in the currency
	// the source is charged in, like the transfer-FX path.
	var ruleLegs []JournalEntry
	var ruleLegInfos []PostingRuleLegInfo
	for _, leg := range legs {
		rl, ri, err := l.resolvePostingRuleLegsLocked(postingRuleContext{
			kind:        PostingEventSweep,
			payer:       leg.From,
			payee:       s.To,
			amountCents: leg.AmountCents,
			currency:    leg.SourceCurrency,
			triggerID:   leg.PrincipalEntryID,
		}, createdAt)
		if err != nil {
			return SweepFXReceipt{}, err
		}
		ruleLegs = append(ruleLegs, rl...)
		ruleLegInfos = append(ruleLegInfos, ri...)
	}

	// Atomic commit: every leg's journal rows, per-account index
	// entries, balances, totals, chain links, and version bumps land
	// together, under the one write lock, after all checks passed. Any
	// failure above returned before the first mutation, so there is
	// nothing to roll back.
	now := time.Now()
	l.maybePruneIdempotencyKeys(now)
	l.maybePruneMonthlySpendLocked(now)
	versionBefore := l.version
	entries := make([]JournalEntry, 0, 2*len(legs)+len(ruleLegs))
	fxDisclosures := make([]FXConversion, 0, len(legs))
	for i, leg := range legs {
		conv := conversions[i]
		if leg.SourceCurrency == toCurrency {
			entries = append(entries, JournalEntry{
				ID:            leg.PrincipalEntryID,
				DebitAccount:  s.To,
				CreditAccount: leg.From,
				AmountCents:   leg.AmountCents,
				Currency:      toCurrency,
				CreatedAt:     createdAt,
			})
			fxDisclosures = append(fxDisclosures, FXConversion{
				FromCurrency:   leg.SourceCurrency,
				ToCurrency:     toCurrency,
				SourceCents:    leg.AmountCents,
				RateNum:        1,
				RateDen:        1,
				ConvertedCents: leg.AmountCents,
				Direction:      sweepFXSameCurrency,
			})
			continue
		}
		entries = append(entries, JournalEntry{
			ID:            leg.PrincipalEntryID,
			DebitAccount:  s.To,
			CreditAccount: fxAccount,
			AmountCents:   conv.converted,
			Currency:      toCurrency,
			CreatedAt:     createdAt,
		}, JournalEntry{
			ID:            leg.FXEntryID,
			DebitAccount:  fxAccount,
			CreditAccount: leg.From,
			AmountCents:   leg.AmountCents,
			Currency:      leg.SourceCurrency,
			CreatedAt:     createdAt,
		})
		fxDisclosures = append(fxDisclosures, FXConversion{
			FromCurrency:        leg.SourceCurrency,
			ToCurrency:          toCurrency,
			SourceCents:         leg.AmountCents,
			RateNum:             conv.num,
			RateDen:             conv.den,
			ConvertedCents:      conv.converted,
			EffectiveVersion:    conv.effVersion,
			RateExpiresAt:       conv.expiresAt,
			RateSnapshotVersion: conv.rateSnapVer,
			Direction:           FXSideSell,
			MidNum:              conv.midNum,
			MidDen:              conv.midDen,
			BidNum:              conv.bidNum,
			BidDen:              conv.bidDen,
			AskNum:              conv.askNum,
			AskDen:              conv.askDen,
			SpreadBps:           fxSpreadBps(conv.bidNum, conv.bidDen, conv.askNum, conv.askDen, conv.midNum, conv.midDen),
		})
	}
	entries = append(entries, ruleLegs...)
	// Pre-commit balance snapshot for the opt-in balance-change hook
	// (see balance_hook.go): the legs are fully planned above, so the
	// touched set is exact. Nil when no hook is registered.
	hookTouched := touchedAccounts(entries)
	hookOld := l.balanceSnapshotLocked(hookTouched)
	for _, e := range entries {
		l.commitEntryLocked(e)
	}
	for _, leg := range legs {
		l.addMonthlySpendLocked(leg.From, leg.SourceCurrency, leg.AmountCents, createdAt)
	}
	l.countPostingRuleLegsLocked(ruleLegInfos)
	if s.IdempotencyKey != "" {
		ids := make([]string, 0, len(entries))
		for _, e := range entries {
			ids = append(ids, e.ID)
		}
		l.sweepFXKeys[s.IdempotencyKey] = sweepFXRecord{
			sweepID:    s.ID,
			toCurrency: toCurrency,
			fxAccount:  fxAccount,
			entryIDs:   ids,
			createdAt:  createdAt,
		}
	}
	sweepFXEntryIDs := make([]string, 0, len(entries))
	for _, e := range entries {
		sweepFXEntryIDs = append(sweepFXEntryIDs, e.ID)
	}
	// Low-balance alert evaluation: strictly after the atomic commit
	// zone, read-only (see low_balance.go). Advisory only.
	l.evaluateBalanceWatchesLocked(touchedAccounts(entries), s.ID, "PostSweepFX")
	l.emitAudit(AuditEvent{
		Op:            "sweep_fx",
		Actor:         "PostSweepFX",
		TraceID:       s.ID,
		VersionBefore: versionBefore,
		VersionAfter:  l.version,
		EntryIDs:      sweepFXEntryIDs,
		Accounts:      append(append([]AccountID{}, s.From...), s.To, fxAccount),
		Details:       sweepFXDetails(len(legs), toCurrency, fxAccount, legs, fxDisclosures, ruleLegInfos),
	})
	// Domain outbox: one entry_posted per committed journal row
	// (principal, FX leg, rule legs) — the plain-sweep convention: no
	// sweep-level envelope, the audit event above carries the full
	// conversion disclosure.
	l.emitOutboxEntriesLocked(entries)
	// Balance-change notification: strictly after the atomic commit
	// zone, advisory only — async dispatch, hook failures isolated.
	l.fireBalanceHooksLocked(hookTouched, hookOld, s.ID)

	return SweepFXReceipt{
		SweepID:    s.ID,
		ToCurrency: toCurrency,
		FXAccount:  fxAccount,
		Legs:       legs,
		Entries:    entries,
		FX:         fxDisclosures,
		Duplicate:  false,
		RuleLegs:   ruleLegInfos,
	}, nil
}

// sweepFXLegsLocked plans the legs of a cross-currency sweep: for each
// source in From order, one leg per currency with a positive balance,
// currencies sorted for determinism. Zero and negative balances
// contribute no leg. Callers must hold l.mu.
func (l *Ledger) sweepFXLegsLocked(s SweepFX, toCurrency string) []SweepFXLeg {
	var legs []SweepFXLeg
	for _, from := range s.From {
		var currencies []string
		for k, bal := range l.balances {
			if k.account == from && bal > 0 {
				currencies = append(currencies, k.currency)
			}
		}
		sort.Strings(currencies)
		for _, currency := range currencies {
			principalID := s.ID + "/" + string(from) + "/" + currency
			leg := SweepFXLeg{
				From:             from,
				SourceCurrency:   currency,
				AmountCents:      l.balances[accountCurrency{account: from, currency: currency}],
				ToCurrency:       toCurrency,
				PrincipalEntryID: principalID,
			}
			if currency != toCurrency {
				leg.FXEntryID = principalID + "/fx"
			}
			legs = append(legs, leg)
		}
	}
	return legs
}

// sweepFXToPlainLegs adapts FX sweep legs to the plain SweepLeg shape
// for the sweep-reserve floor calculation (see sweep_reserve.go): a
// reserve is configured per (source, currency) and shrinks the source
// amount before conversion, so it operates on the pre-conversion leg.
// The adapted legs share no backing store with the FX legs.
func sweepFXToPlainLegs(legs []SweepFXLeg) []SweepLeg {
	out := make([]SweepLeg, 0, len(legs))
	for _, leg := range legs {
		out = append(out, SweepLeg{
			From:        leg.From,
			Currency:    leg.SourceCurrency,
			AmountCents: leg.AmountCents,
			EntryID:     leg.PrincipalEntryID,
		})
	}
	return out
}

// sweepFXDetails renders the FX sweep's structured audit disclosure:
// the leg count, the settlement currency, the FX clearing account, the
// per-leg conversion provenance (rate, side, settled amounts), and the
// automatic posting-rule legs.
func sweepFXDetails(legCount int, toCurrency string, fxAccount AccountID, legs []SweepFXLeg, fx []FXConversion, ruleLegs []PostingRuleLegInfo) map[string]any {
	d := map[string]any{
		"legs":        legCount,
		"to_currency": toCurrency,
		"fx_account":  string(fxAccount),
	}
	conv := make([]map[string]any, 0, len(legs))
	for i, leg := range legs {
		row := map[string]any{
			"from_account":    string(leg.From),
			"source_currency": leg.SourceCurrency,
			"amount_cents":    leg.AmountCents,
			"converted_cents": leg.ConvertedCents,
			"principal_entry": leg.PrincipalEntryID,
		}
		if leg.FXEntryID != "" {
			row["fx_entry"] = leg.FXEntryID
		}
		if i < len(fx) {
			f := fx[i]
			row["direction"] = f.Direction
			if f.Direction != sweepFXSameCurrency {
				row["rate"] = map[string]any{
					"num":               f.RateNum,
					"den":               f.RateDen,
					"effective_version": f.EffectiveVersion,
					"rate_expires_at":   f.RateExpiresAt.UTC().Format(time.RFC3339Nano),
					"rate_snapshot_ver": f.RateSnapshotVersion,
				}
			}
		}
		conv = append(conv, row)
	}
	d["conversions"] = conv
	if len(ruleLegs) > 0 {
		d["posting_rule_legs"] = postingRuleDetails(ruleLegs)
	}
	return d
}

// sweepFXLegsOf rebuilds the leg descriptors of a replayed FX sweep
// from its committed entries. The entry ID convention
// "<sweep ID>/<source>/<currency>" (principal) with
// "<sweep ID>/<source>/<currency>/fx" (conversion leg) carries
// everything the leg needs: a principal with a matching "/fx" companion
// is a converted leg, a principal without one is a same-currency leg.
// Rule legs (see posting_rules.go, "<...>/rule:<name>") are skipped:
// they are not sweep legs and rebuild separately into the receipt's
// RuleLegs.
func sweepFXLegsOf(entries []JournalEntry) []SweepFXLeg {
	byID := make(map[string]JournalEntry, len(entries))
	for _, e := range entries {
		byID[e.ID] = e
	}
	legs := make([]SweepFXLeg, 0, len(entries))
	for _, e := range entries {
		if strings.Contains(e.ID, "/rule:") || strings.HasSuffix(e.ID, "/fx") {
			continue
		}
		leg := SweepFXLeg{
			From:             e.CreditAccount,
			SourceCurrency:   e.Currency,
			ToCurrency:       e.Currency,
			PrincipalEntryID: e.ID,
		}
		if fxEntry, ok := byID[e.ID+"/fx"]; ok {
			// Converted leg: the principal books the converted amount
			// in the target currency; the "/fx" leg books the source
			// amount in the source currency (credit = source).
			leg.From = fxEntry.CreditAccount
			leg.SourceCurrency = fxEntry.Currency
			leg.AmountCents = fxEntry.AmountCents
			leg.ConvertedCents = e.AmountCents
			leg.FXEntryID = fxEntry.ID
		} else {
			// Same-currency leg: debit To, credit From, one entry.
			leg.AmountCents = e.AmountCents
			leg.ConvertedCents = e.AmountCents
		}
		legs = append(legs, leg)
	}
	return legs
}

// pruneSweepFXKeysLocked evicts FX-sweep idempotency keys older than
// the TTL, mirroring the entry-level key expiry: after the TTL,
// reposting a sweep-FX key books a brand-new sweep. The journaled sweep
// legs themselves are untouched — only the replay-detection index
// shrinks. A non-positive TTL disables pruning. Callers must hold l.mu
// (the write lock; the sweep mutates the index).
func (l *Ledger) pruneSweepFXKeysLocked(now time.Time) int {
	if l.idempotencyTTL <= 0 {
		return 0
	}
	cutoff := now.Add(-l.idempotencyTTL)
	removed := 0
	for key, rec := range l.sweepFXKeys {
		if rec.createdAt.Before(cutoff) {
			delete(l.sweepFXKeys, key)
			removed++
		}
	}
	return removed
}
