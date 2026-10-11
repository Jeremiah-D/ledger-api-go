package ledger

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// Multilateral netting settlement (LG-56).
//
// Netting answers the clearinghouse question a payments operator asks at
// cut-off time: given a set of participant accounts with bilateral
// obligations already on the books, what is the smallest set of transfers
// that settles everybody, and can it be booked atomically?
//
// Net position: an account's net position in a currency is its current net
// balance in that currency (see BalanceIn) — the honest on-book primitive.
// A positive position means the account is a net creditor (the books owe
// it money); a negative position means it is a net debtor (it owes money
// into the books). Positions never mix currencies: netting runs per
// currency, isolated like the accounting equation itself (see
// VerifyAccountingEquation).
//
// Greedy minimal transfer set: within one currency, repeatedly match the
// largest creditor against the largest debtor and transfer
// min(claim, debt). Each transfer zeroes at least one participant's
// remaining position — the side with the smaller magnitude — so with K
// nonzero positions at most K-1 transfers are needed before at most one
// participant is left holding the residual (the set's total, owed to or
// from parties outside the netting set). K <= N (N = account count), so
// the transfer count is always <= N-1. When the positions sum to zero the
// final transfer zeroes both remaining participants and the set settles
// completely. Ties break by account ID, and legs are emitted sorted by
// (currency, from, to), so identical position sets always produce the
// identical plan.
//
// Posting: PostNetting books every leg as one atomic batch through
// postBatchLocked (see batch.go), so the legs clear the full Post gate
// order — field validation, idempotency replay, chart direction, frozen,
// overdraft, daily-limit, period — in the batch's sequential simulation
// semantics, and any failing leg rejects the whole session: no journal
// rows, no chain links, no version bump. Like PostBatch, netting legs
// carry no fee policy and no dual-control review: settlement is a
// bookkeeping operation, not a customer payment.
//
// A settlement leg moves money from the debtor to the creditor: the
// journal entry debits the debtor (its negative balance rises toward
// zero) and credits the creditor (its positive balance falls toward
// zero), mirroring PostTransfer's payer/payee mapping (debit the payee,
// credit the payer) with the debtor as the payer.
//
// Idempotency: the session key is deterministic — "netting:<sessionID>" —
// and lives in its own namespace (nettingKeys), expiring with the
// idempotency TTL like every other key index. Reposting a session key
// returns the original report with Duplicate == true and books nothing
// new. The index is in-memory only: unlike the entry/transfer/batch key
// namespaces it does not round-trip through disaster-recovery snapshots
// (the journal rows themselves do — the money is safe; only the replay
// shortcut is lost, and reposting a session after a restore recomputes
// from the restored books). Session IDs are single-use per settlement
// round: after TTL expiry, reposting a session whose accounts regained
// obligations fails loudly with ErrBatchEntryIDConflict on the
// deterministic leg IDs rather than double-booking.

// Netting validation errors.
var (
	// ErrEmptyNettingSession is returned when the netting session ID is empty.
	ErrEmptyNettingSession = errors.New("ledger: netting session ID must not be empty")
	// ErrEmptyNettingAccounts is returned when the netting set names no accounts.
	ErrEmptyNettingAccounts = errors.New("ledger: netting requires at least one account")
	// ErrEmptyNettingAccount is returned when the netting set names an empty account.
	ErrEmptyNettingAccount = errors.New("ledger: netting account must not be empty")
	// ErrNettingDuplicateAccount is returned when the netting set names an account twice.
	ErrNettingDuplicateAccount = errors.New("ledger: netting accounts must be distinct")
)

// NetPosition is one account's net position in one currency: its current
// net balance (see BalanceIn). Cents > 0 means the account is a net
// creditor (to receive in settlement); Cents < 0 means it is a net debtor
// (to pay). Zero positions are omitted — they need no settlement.
type NetPosition struct {
	Account  AccountID `json:"account"`
	Currency string    `json:"currency"`
	Cents    int64     `json:"net_cents"`
}

// NetTransfer is one settlement leg of a netting plan: From (the debtor,
// net payer) pays To (the creditor, net payee) AmountCents in Currency.
// The journal entry books debit From / credit To, so the debtor's balance
// rises toward zero and the creditor's falls toward zero. EntryID is the
// deterministic journal entry ID: "<sessionID>/net/<currency>/<index>".
type NetTransfer struct {
	From        AccountID `json:"from_account"`
	To          AccountID `json:"to_account"`
	AmountCents int64     `json:"amount_cents"`
	Currency    string    `json:"currency"`
	EntryID     string    `json:"entry_id"`
}

// NettingPlan is the read-only settlement plan for one session: the
// positions before settlement, the greedy minimal transfer set, and the
// simulated positions after applying it. Planning changes nothing.
type NettingPlan struct {
	SessionID string        `json:"session_id"`
	Accounts  []AccountID   `json:"accounts"`
	Before    []NetPosition `json:"before"`
	Transfers []NetTransfer `json:"transfers"`
	// After is Before with every transfer applied, simulated: any
	// nonzero rows are the residual the set cannot settle internally
	// (owed to or from parties outside the netting set).
	After []NetPosition `json:"after"`
}

// NettingReport is what PostNetting returns: the plan it committed (or,
// on replay, the plan it originally committed) plus the journal outcome.
// Before/After are the on-book positions around the settlement; Transfers
// carries the per-leg detail (from/to/amount/currency/entry ID).
type NettingReport struct {
	SessionID   string    `json:"session_id"`
	GeneratedAt time.Time `json:"generated_at"`
	// Version is the ledger version right after the session committed
	// (the original commit's version on replay).
	Version   uint64        `json:"version"`
	Before    []NetPosition `json:"before"`
	Transfers []NetTransfer `json:"transfers"`
	After     []NetPosition `json:"after"`
	EntryIDs  []string      `json:"entry_ids"`
	Duplicate bool          `json:"duplicate"`
}

// nettingRecord remembers what a netting session posted so idempotent
// replays rebuild the exact original report. The key is deterministic —
// "netting:<sessionID>" — in its own namespace, expiring with the
// idempotency TTL like every other key index (see
// pruneNettingKeysLocked). In-memory only: it does not round-trip through
// snapshots.
type nettingRecord struct {
	sessionID    string
	before       []NetPosition
	transfers    []NetTransfer
	after        []NetPosition
	entryIDs     []string
	versionAfter uint64
	createdAt    time.Time
}

// nettingSessionKey derives the deterministic session-level idempotency
// key from the session ID.
func nettingSessionKey(sessionID string) string {
	return "netting:" + sessionID
}

// validateNettingAccounts rejects empty and duplicate accounts, returning
// the deduplicated list in input order. Callers must hold l.mu.
func validateNettingAccounts(accounts []AccountID) ([]AccountID, error) {
	if len(accounts) == 0 {
		return nil, ErrEmptyNettingAccounts
	}
	seen := make(map[AccountID]bool, len(accounts))
	out := make([]AccountID, 0, len(accounts))
	for _, a := range accounts {
		if a == "" {
			return nil, ErrEmptyNettingAccount
		}
		if seen[a] {
			return nil, ErrNettingDuplicateAccount
		}
		seen[a] = true
		out = append(out, a)
	}
	return out, nil
}

// netPositionsLocked computes the net position of each account per
// currency from the current books: one row per (account, currency) with a
// nonzero balance, sorted by (currency, account). Callers must hold l.mu;
// the read lock suffices.
func (l *Ledger) netPositionsLocked(accounts []AccountID) []NetPosition {
	var out []NetPosition
	for _, a := range accounts {
		for k, bal := range l.balances {
			if k.account == a && bal != 0 {
				out = append(out, NetPosition{Account: a, Currency: k.currency, Cents: bal})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Currency != out[j].Currency {
			return out[i].Currency < out[j].Currency
		}
		return out[i].Account < out[j].Account
	})
	return out
}

// NetPositions computes the net position of each given account per
// currency, isolated per currency like the accounting equation. Only the
// named accounts are considered; zero positions are omitted. Read-only.
func (l *Ledger) NetPositions(accounts []AccountID) ([]NetPosition, error) {
	valid, err := validateNettingAccounts(accounts)
	if err != nil {
		return nil, err
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.netPositionsLocked(valid), nil
}

// greedyNettingLegs builds the minimal settlement transfer set for one
// session's positions: per currency, repeatedly match the largest
// creditor against the largest debtor for min(claim, debt), until one
// side is exhausted. Legs come out sorted by (currency, from, to) with
// deterministic entry IDs "<sessionID>/net/<currency>/<index>".
//
// Bound proof: each leg zeroes at least one participant's remaining
// position (the side with the smaller magnitude settles exactly), so the
// count of participants with a nonzero remainder drops by at least one
// per leg. From K nonzero positions, at most K-1 legs leave at most one
// participant holding the residual — the set's total, owed to or from
// parties outside the netting set — and K <= N, so the leg count is
// always <= N-1. When the positions sum to zero the last leg zeroes both
// remaining participants and the currency settles completely.
func greedyNettingLegs(sessionID string, positions []NetPosition) []NetTransfer {
	type side struct {
		account   AccountID
		remaining int64 // >0 creditor claim, <0 debtor debt
	}
	var legs []NetTransfer
	// Positions arrive sorted by (currency, account); group the run per
	// currency so each currency nets in isolation.
	for start := 0; start < len(positions); {
		end := start + 1
		for end < len(positions) && positions[end].Currency == positions[start].Currency {
			end++
		}
		currency := positions[start].Currency
		var creditors, debtors []side
		for _, p := range positions[start:end] {
			if p.Cents > 0 {
				creditors = append(creditors, side{p.Account, p.Cents})
			} else if p.Cents < 0 {
				debtors = append(debtors, side{p.Account, p.Cents})
			}
		}
		// Largest claim against largest debt; ties break by account ID
		// so identical position sets always match identically.
		sort.Slice(creditors, func(i, j int) bool {
			if creditors[i].remaining != creditors[j].remaining {
				return creditors[i].remaining > creditors[j].remaining
			}
			return creditors[i].account < creditors[j].account
		})
		sort.Slice(debtors, func(i, j int) bool {
			if debtors[i].remaining != debtors[j].remaining {
				return debtors[i].remaining < debtors[j].remaining
			}
			return debtors[i].account < debtors[j].account
		})
		for i, j := 0, 0; i < len(creditors) && j < len(debtors); {
			c, d := &creditors[i], &debtors[j]
			amt := min(c.remaining, -d.remaining)
			legs = append(legs, NetTransfer{
				From:        d.account,
				To:          c.account,
				AmountCents: amt,
				Currency:    currency,
			})
			c.remaining -= amt
			d.remaining += amt
			if c.remaining == 0 {
				i++
			}
			if d.remaining == 0 {
				j++
			}
		}
		start = end
	}
	sort.Slice(legs, func(i, j int) bool {
		if legs[i].Currency != legs[j].Currency {
			return legs[i].Currency < legs[j].Currency
		}
		if legs[i].From != legs[j].From {
			return legs[i].From < legs[j].From
		}
		return legs[i].To < legs[j].To
	})
	for i := range legs {
		legs[i].EntryID = fmt.Sprintf("%s/net/%s/%d", sessionID, legs[i].Currency, i)
	}
	return legs
}

// applyNettingLegs simulates a transfer set against positions: the
// debtor's (negative) position rises by the amount, the creditor's
// (positive) position falls by it. Zero rows drop out, sorted by
// (currency, account) like netPositionsLocked.
func applyNettingLegs(before []NetPosition, legs []NetTransfer) []NetPosition {
	remaining := make(map[NetPosition]int64, len(before))
	order := make([]NetPosition, 0, len(before))
	for _, p := range before {
		k := NetPosition{Account: p.Account, Currency: p.Currency}
		remaining[k] = p.Cents
		order = append(order, k)
	}
	for _, leg := range legs {
		dk := NetPosition{Account: leg.From, Currency: leg.Currency}
		ck := NetPosition{Account: leg.To, Currency: leg.Currency}
		remaining[dk] += leg.AmountCents
		remaining[ck] -= leg.AmountCents
	}
	var out []NetPosition
	for _, k := range order {
		if v := remaining[k]; v != 0 {
			out = append(out, NetPosition{Account: k.Account, Currency: k.Currency, Cents: v})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Currency != out[j].Currency {
			return out[i].Currency < out[j].Currency
		}
		return out[i].Account < out[j].Account
	})
	return out
}

// PlanNetting builds the read-only settlement plan for one session:
// current positions, the greedy minimal transfer set, and the simulated
// after-positions. Planning books nothing.
func (l *Ledger) PlanNetting(sessionID string, accounts []AccountID) (NettingPlan, error) {
	if sessionID == "" {
		return NettingPlan{}, ErrEmptyNettingSession
	}
	valid, err := validateNettingAccounts(accounts)
	if err != nil {
		return NettingPlan{}, err
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	before := l.netPositionsLocked(valid)
	legs := greedyNettingLegs(sessionID, before)
	return NettingPlan{
		SessionID: sessionID,
		Accounts:  append([]AccountID(nil), valid...),
		Before:    before,
		Transfers: legs,
		After:     applyNettingLegs(before, legs),
	}, nil
}

// copyNetPositions defensively copies a position slice for storage or
// reporting, so later mutations cannot alias the session record.
func copyNetPositions(in []NetPosition) []NetPosition {
	if in == nil {
		return nil
	}
	return append([]NetPosition(nil), in...)
}

// copyNetTransfers defensively copies a transfer slice for storage or
// reporting.
func copyNetTransfers(in []NetTransfer) []NetTransfer {
	if in == nil {
		return nil
	}
	return append([]NetTransfer(nil), in...)
}

// PostNetting atomically settles one multilateral netting session: it
// plans from the current books and books every settlement leg in one
// atomic batch (see postBatchLocked), so every leg clears the full Post
// gate order — field validation, idempotency replay, chart direction,
// frozen, overdraft, daily-limit, period — and any failing leg rejects
// the whole session with zero bookings: no journal rows, no chain links,
// no version bump.
//
// The session key is deterministic ("netting:<sessionID>"): reposting it
// returns the original report with Duplicate == true and books nothing
// new — even if an account was frozen or protected after the original
// settlement, because the replay books nothing. A session with no
// obligations to settle commits nothing and returns an empty-leg report
// (still registered, so its replay is stable).
//
// Like PostBatch, netting legs carry no fee policy and no dual-control
// review: settlement is a bookkeeping operation, not a customer payment.
func (l *Ledger) PostNetting(sessionID string, accounts []AccountID) (NettingReport, error) {
	if sessionID == "" {
		return NettingReport{}, ErrEmptyNettingSession
	}
	valid, err := validateNettingAccounts(accounts)
	if err != nil {
		return NettingReport{}, err
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	key := nettingSessionKey(sessionID)
	if rec, ok := l.nettingKeys[key]; ok {
		return NettingReport{
			SessionID:   rec.sessionID,
			GeneratedAt: rec.createdAt,
			Version:     rec.versionAfter,
			Before:      copyNetPositions(rec.before),
			Transfers:   copyNetTransfers(rec.transfers),
			After:       copyNetPositions(rec.after),
			EntryIDs:    append([]string(nil), rec.entryIDs...),
			Duplicate:   true,
		}, nil
	}

	now := time.Now()
	before := l.netPositionsLocked(valid)
	legs := greedyNettingLegs(sessionID, before)
	versionBefore := l.version

	var entryIDs []string
	if len(legs) > 0 {
		entries := make([]JournalEntry, 0, len(legs))
		for _, leg := range legs {
			// The debtor (net payer) is debited — its negative balance
			// rises toward zero — and the creditor (net payee) is
			// credited — its positive balance falls toward zero.
			entries = append(entries, JournalEntry{
				ID:            leg.EntryID,
				DebitAccount:  leg.From,
				CreditAccount: leg.To,
				AmountCents:   leg.AmountCents,
				Currency:      leg.Currency,
				CreatedAt:     now,
			})
			entryIDs = append(entryIDs, leg.EntryID)
		}
		// Atomic commit through the batch path: the full Post gate
		// order runs in batch order against a sequential simulation of
		// the session's own balance effects, and any rejection lands
		// before the first mutation — there is nothing to roll back.
		if _, err := l.postBatchLocked(Batch{ID: sessionID, Entries: entries, CreatedAt: now}); err != nil {
			return NettingReport{}, err
		}
	}

	// After-positions are re-read from the books: authoritative, not
	// simulated. Any nonzero rows are the residual the set cannot settle
	// internally (owed to or from parties outside the netting set).
	after := l.netPositionsLocked(valid)
	l.nettingKeys[key] = nettingRecord{
		sessionID:    sessionID,
		before:       copyNetPositions(before),
		transfers:    copyNetTransfers(legs),
		after:        copyNetPositions(after),
		entryIDs:     append([]string(nil), entryIDs...),
		versionAfter: l.version,
		createdAt:    now,
	}

	settled := make(map[string]int64)
	for _, leg := range legs {
		settled[leg.Currency] += leg.AmountCents
	}
	currencies := make([]string, 0, len(settled))
	for c := range settled {
		currencies = append(currencies, c)
	}
	sort.Strings(currencies)
	l.emitAudit(AuditEvent{
		Op:            "netting",
		Actor:         "PostNetting",
		TraceID:       sessionID,
		VersionBefore: versionBefore,
		VersionAfter:  l.version,
		EntryIDs:      append([]string(nil), entryIDs...),
		Accounts:      append([]AccountID(nil), valid...),
		Details: map[string]any{
			"legs":          len(legs),
			"currencies":    currencies,
			"settled_cents": settled,
			"residual_legs": len(after),
			"duplicate":     false,
		},
	})

	return NettingReport{
		SessionID:   sessionID,
		GeneratedAt: now,
		Version:     l.version,
		Before:      copyNetPositions(before),
		Transfers:   copyNetTransfers(legs),
		After:       copyNetPositions(after),
		EntryIDs:    append([]string(nil), entryIDs...),
		Duplicate:   false,
	}, nil
}

// pruneNettingKeysLocked evicts netting session keys older than the TTL,
// mirroring every other idempotency namespace: after the TTL, reposting
// a session key recomputes from the current books instead of replaying.
// The journaled settlement entries are untouched — only the replay index
// shrinks. A non-positive TTL disables pruning. Callers must hold l.mu.
func (l *Ledger) pruneNettingKeysLocked(now time.Time) int {
	if l.idempotencyTTL <= 0 {
		return 0
	}
	cutoff := now.Add(-l.idempotencyTTL)
	removed := 0
	for key, rec := range l.nettingKeys {
		if rec.createdAt.Before(cutoff) {
			delete(l.nettingKeys, key)
			removed++
		}
	}
	return removed
}
