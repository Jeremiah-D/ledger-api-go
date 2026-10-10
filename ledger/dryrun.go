package ledger

// What-if dry-run simulation (LG-36).
//
// A dry run answers "what would happen if I submitted this now?" without
// changing anything: it runs the exact production code path (field
// validation, idempotency replay, frozen/overdraft/daily-limit/period
// risk checks, fee resolution, FX conversion) against a private deep copy
// of the ledger, then discards the copy. Because the simulated operation
// executes the same methods as the real one — Post, PostTransfer,
// PostSweep on the clone — the validation order, risk-check precedence,
// fee math, and FX conversion are identical by construction; there is no
// parallel planning logic to drift.
//
// Zero side effects, by construction:
//   - the clone carries no audit log, so the dry run emits no audit
//     events and advances no hash chain;
//   - the clone's maps are deep copies, so the live ledger's journal,
//     balances, version, and idempotency indexes are untouched;
//   - no version bump, no key registration, no prune sweeps on the live
//     ledger.
//
// The result is advisory: it describes the ledger at the instant the
// simulation ran. A concurrent writer can change the outcome between the
// dry run and the real submission, so clients must still handle rejections
// on the real call. The dry run's value is catching deterministic
// rejections (bad fields, frozen accounts, closed periods, overdrafts,
// exhausted daily budgets) before they touch production state.
//
// Idempotency replays are first-class: when the dry run's key was already
// posted, the result reports Duplicate=true with the original entries and
// zero balance movement, exactly what the real call would return.
type DryRunLeg struct {
	EntryID       string    `json:"entry_id"`
	DebitAccount  AccountID `json:"debit_account"`
	CreditAccount AccountID `json:"credit_account"`
	AmountCents   int64     `json:"amount_cents"`
	Currency      string    `json:"currency"`
	// Memo echoes the entry's business note (see JournalEntry.Memo), so
	// a dry run shows exactly what the real call would journal.
	Memo string `json:"memo,omitempty"`
	// DebitBalanceBefore/After (and the credit pair) are the account's net
	// balance in the leg's currency around the simulated posting. Legs
	// apply sequentially in commit order, so a leg's "before" already
	// reflects the earlier legs' movement — the "after" of the last leg
	// touching an account is the balance the real call would leave.
	DebitBalanceBefore  int64 `json:"debit_balance_before_cents"`
	DebitBalanceAfter   int64 `json:"debit_balance_after_cents"`
	CreditBalanceBefore int64 `json:"credit_balance_before_cents"`
	CreditBalanceAfter  int64 `json:"credit_balance_after_cents"`
}

// DryRunResult reports what a simulated posting would do.
//
// On a would-be failure (the returned error), WouldSucceed is false and
// the error is the exact error the real call would return — the HTTP
// layer maps it to the same status code, so a client can swap the
// /dry-run suffix for the real endpoint and keep its error handling.
// Legs and version fields are zero on failure: nothing would have been
// recorded.
type DryRunResult struct {
	WouldSucceed  bool        `json:"would_succeed"`
	Duplicate     bool        `json:"duplicate"`
	VersionBefore uint64      `json:"version_before"`
	VersionAfter  uint64      `json:"version_after"`
	Legs          []DryRunLeg `json:"legs"`
}

// cloneForDryRunLocked deep-copies the ledger into a private, disposable
// twin. The twin's audit log is nil — dry runs must not emit audit events
// or advance the audit hash chain. Mutable containers are copied deeply
// enough that the twin's commits cannot alias the live ledger's storage
// (append-only per-account indexes included). Callers must hold l.mu; the
// read lock suffices because nothing on the live ledger is mutated.
//
// NOTE: this copy lists every state field explicitly. When a new field is
// added to Ledger, it must be copied here too, or dry runs will simulate
// against stale state.
func (l *Ledger) cloneForDryRunLocked() *Ledger {
	c := &Ledger{
		balances:             make(map[accountCurrency]int64, len(l.balances)),
		entries:              make(map[string]JournalEntry, len(l.entries)),
		byKey:                make(map[string]JournalEntry, len(l.byKey)),
		byAccount:            make(map[AccountID][]string, len(l.byAccount)),
		debitTotals:          make(map[accountCurrency]int64, len(l.debitTotals)),
		creditTotals:         make(map[accountCurrency]int64, len(l.creditTotals)),
		frozen:               make(map[AccountID]bool, len(l.frozen)),
		noOverdraft:          make(map[AccountID]bool, len(l.noOverdraft)),
		dailyLimits:          make(map[dailyLimitKey]int64, len(l.dailyLimits)),
		dailyOutflow:         make(map[dailyLimitKey]int64, len(l.dailyOutflow)),
		parents:              make(map[AccountID]AccountID, len(l.parents)),
		transferKeys:         make(map[string][]string, len(l.transferKeys)),
		holds:                make(map[string]Hold, len(l.holds)),
		holdsByAccount:       make(map[AccountID][]string, len(l.holdsByAccount)),
		holdKeys:             make(map[string]string, len(l.holdKeys)),
		captureKeys:          make(map[string]CaptureReceipt, len(l.captureKeys)),
		sweepKeys:            make(map[string]sweepRecord, len(l.sweepKeys)),
		batchKeys:            make(map[string]batchRecord, len(l.batchKeys)),
		merges:               make(map[string]mergeRecord, len(l.merges)),
		mergeKeys:            make(map[string]string, len(l.mergeKeys)),
		fxRates:              make(map[fxPair]ExchangeRate, len(l.fxRates)),
		closedPeriods:        make(map[string]bool, len(l.closedPeriods)),
		lowBalanceThresholds: make(map[lowBalanceKey]int64, len(l.lowBalanceThresholds)),
		lowBalanceBreached:   make(map[lowBalanceKey]bool, len(l.lowBalanceBreached)),
		chain:                append([]chainLink(nil), l.chain...),
		version:              l.version,
		idempotencyTTL:       l.idempotencyTTL,
		pruneInterval:        l.pruneInterval,
		lastKeyPrune:         l.lastKeyPrune,
		lastDailyPrune:       l.lastDailyPrune,
		// audit intentionally nil: the dry run emits nothing.
	}
	for k, v := range l.balances {
		c.balances[k] = v
	}
	for k, v := range l.entries {
		c.entries[k] = v
	}
	for k, v := range l.byKey {
		c.byKey[k] = v
	}
	for k, v := range l.byAccount {
		c.byAccount[k] = append([]string(nil), v...)
	}
	for k, v := range l.debitTotals {
		c.debitTotals[k] = v
	}
	for k, v := range l.creditTotals {
		c.creditTotals[k] = v
	}
	for k, v := range l.frozen {
		c.frozen[k] = v
	}
	for k, v := range l.noOverdraft {
		c.noOverdraft[k] = v
	}
	for k, v := range l.dailyLimits {
		c.dailyLimits[k] = v
	}
	for k, v := range l.dailyOutflow {
		c.dailyOutflow[k] = v
	}
	for k, v := range l.parents {
		c.parents[k] = v
	}
	for k, v := range l.transferKeys {
		c.transferKeys[k] = append([]string(nil), v...)
	}
	for k, v := range l.holds {
		c.holds[k] = v
	}
	for k, v := range l.holdsByAccount {
		c.holdsByAccount[k] = append([]string(nil), v...)
	}
	for k, v := range l.holdKeys {
		c.holdKeys[k] = v
	}
	for k, v := range l.captureKeys {
		c.captureKeys[k] = v
	}
	for k, v := range l.sweepKeys {
		c.sweepKeys[k] = v
	}
	for k, v := range l.batchKeys {
		c.batchKeys[k] = v
	}
	for k, v := range l.merges {
		c.merges[k] = v
	}
	for k, v := range l.mergeKeys {
		c.mergeKeys[k] = v
	}
	c.feeTiers = append([]FeeTier(nil), l.feeTiers...)
	c.feeRevenueAccount = l.feeRevenueAccount
	for k, v := range l.fxRates {
		c.fxRates[k] = v
	}
	c.fxAccount = l.fxAccount
	for k, v := range l.closedPeriods {
		c.closedPeriods[k] = v
	}
	for k, v := range l.lowBalanceThresholds {
		c.lowBalanceThresholds[k] = v
	}
	for k, v := range l.lowBalanceBreached {
		c.lowBalanceBreached[k] = v
	}
	c.lowBalanceBreaches = l.lowBalanceBreaches
	return c
}

// dryRunLegsLocked turns the entries a simulated operation would commit
// into per-leg balance views. Balances are read from the live ledger (the
// simulation's starting point) and each leg's movement is applied
// sequentially, in commit order, so legs touching the same account chain
// correctly. When apply is false — an idempotent replay, which books
// nothing — before and after are the same live balance. Callers must hold
// l.mu; the read lock suffices.
func (l *Ledger) dryRunLegsLocked(entries []JournalEntry, apply bool) []DryRunLeg {
	legs := make([]DryRunLeg, 0, len(entries))
	// running tracks the simulated balance per (account, currency) as
	// earlier legs of this same dry run move it.
	running := make(map[accountCurrency]int64)
	balance := func(a AccountID, currency string) int64 {
		k := accountCurrency{account: a, currency: currency}
		if v, ok := running[k]; ok {
			return v
		}
		v := l.balances[k]
		running[k] = v
		return v
	}
	move := func(a AccountID, currency string, delta int64) int64 {
		k := accountCurrency{account: a, currency: currency}
		v := balance(a, currency) + delta
		running[k] = v
		return v
	}
	for _, e := range entries {
		leg := DryRunLeg{
			EntryID:             e.ID,
			DebitAccount:        e.DebitAccount,
			CreditAccount:       e.CreditAccount,
			AmountCents:         e.AmountCents,
			Currency:            e.Currency,
			Memo:                e.Memo,
			DebitBalanceBefore:  balance(e.DebitAccount, e.Currency),
			CreditBalanceBefore: balance(e.CreditAccount, e.Currency),
		}
		leg.DebitBalanceAfter = leg.DebitBalanceBefore
		leg.CreditBalanceAfter = leg.CreditBalanceBefore
		if apply {
			leg.DebitBalanceAfter = move(e.DebitAccount, e.Currency, e.AmountCents)
			leg.CreditBalanceAfter = move(e.CreditAccount, e.Currency, -e.AmountCents)
		}
		legs = append(legs, leg)
	}
	return legs
}

// dryRunResultLocked packages a successful simulation: the committed
// entries become legs (with movement applied unless the simulation was an
// idempotent replay), and the version bracket comes from the live ledger
// and the twin. Callers must hold l.mu; the read lock suffices.
func (l *Ledger) dryRunResultLocked(entries []JournalEntry, twin *Ledger, duplicate bool) DryRunResult {
	after := twin.version
	if duplicate {
		// A replay books nothing: the version would not move and neither
		// would any balance.
		after = l.version
	}
	return DryRunResult{
		WouldSucceed:  true,
		Duplicate:     duplicate,
		VersionBefore: l.version,
		VersionAfter:  after,
		Legs:          l.dryRunLegsLocked(entries, !duplicate),
	}
}

// DryRunPost simulates Post(e): it runs the full Post check sequence
// (field validation, idempotency replay, frozen, overdraft, period, and
// daily-limit checks) and reports the balance and version changes the
// posting would cause. Nothing is recorded: no journal row, no chain
// link, no version bump, no idempotency-key registration, no audit event.
func (l *Ledger) DryRunPost(e JournalEntry) (DryRunResult, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	twin := l.cloneForDryRunLocked()
	posted, duplicate, err := twin.Post(e)
	if err != nil {
		return DryRunResult{WouldSucceed: false}, err
	}
	return l.dryRunResultLocked([]JournalEntry{posted}, twin, duplicate), nil
}

// DryRunTransfer simulates PostTransfer(t): the full transfer check
// sequence — field validation, fee resolution (explicit fee or the fee
// schedule), FX conversion when ToCurrency is set, idempotency replay,
// frozen/overdraft/daily-limit/period risk checks — and reports the per-leg
// balance and version changes the transfer would cause. Nothing is
// recorded.
func (l *Ledger) DryRunTransfer(t Transfer) (DryRunResult, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	twin := l.cloneForDryRunLocked()
	receipt, err := twin.PostTransfer(t)
	if err != nil {
		return DryRunResult{WouldSucceed: false}, err
	}
	return l.dryRunResultLocked(receipt.Entries, twin, receipt.Duplicate), nil
}

// DryRunSweep simulates PostSweep(s): the full sweep check sequence —
// field validation, leg planning against current balances, idempotency
// replay, ID conflicts, frozen checks, and the period gate — and reports
// the per-leg balance and version changes the sweep would cause. Nothing
// is recorded.
func (l *Ledger) DryRunSweep(s Sweep) (DryRunResult, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	twin := l.cloneForDryRunLocked()
	receipt, err := twin.PostSweep(s)
	if err != nil {
		return DryRunResult{WouldSucceed: false}, err
	}
	return l.dryRunResultLocked(receipt.Entries, twin, receipt.Duplicate), nil
}
