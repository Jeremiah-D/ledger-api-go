package ledger

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// Net settlement batches: atomic clearing of aggregated transfer
// intents (LG-135, fintech).
//
// A clearing batch collects off-book transfer intents over time —
// payouts queued through the day, marketplace seller settlements,
// payroll legs — and settles them later in one atomic batch. It is the
// workflow container around multilateral netting: where netting.go
// (LG-56) nets on-book positions at cut-off, a clearing batch nets the
// intents it holds, so the operator sees exactly what will move before
// anything moves.
//
// Lifecycle: open -> settled | cancelled. Opening validates and stores
// the legs without touching the journal. Settling aggregates the legs
// per (account, currency) — each account's net across the batch — and
// books the minimal greedy transfer set (largest debtor against
// largest creditor per currency, deterministic by account ID), one
// journal row per transfer, through the atomic batch path
// (postBatchLocked). Any failing leg rejects the whole batch before
// the first mutation: the batch stays open and zero partial rows land.
// Cancelling an open batch discards the intents; cancelling is
// terminal.
//
// Settlement leg economics follow the transfer convention (see
// PostTransfer) applied to the netted amounts: the net payee is
// debited (their balance grows) and the net payer is credited. Entry
// IDs are deterministic:
// "clearing/<batchID>/<n>" (n = 1-based leg index in the emitted
// transfer order), so a retried settle replays through the shared
// entry idempotency namespace instead of double-booking. The batch
// carries its own idempotency key ("clearing/<batchID>") through
// PostBatch's two-level semantics.
//
// Idempotency: batch opens carry their own key namespace
// (clearingKeys), independent of Post's byKey and every other
// operation's namespace, expiring with the ledger's idempotency TTL
// like the others. Re-settling a settled batch returns it unchanged.
// In-memory only: like installment plans and netting sessions, batches
// do not round-trip through disaster-recovery snapshots — the
// journaled net rows do, so the money is safe; only the batch metadata
// is lost. Reconcile lists every batch (see ClearingBatchSummary on
// the reconciliation report), so an operator can always see which
// batches produced the rows on the books.

// Clearing batch statuses. Only open batches can settle or cancel;
// the other two are terminal.
const (
	ClearingOpen      = "open"
	ClearingSettled   = "settled"
	ClearingCancelled = "cancelled"
)

// Validation and state errors returned by the clearing-batch APIs.
var (
	ErrEmptyClearingBatchID     = errors.New("ledger: clearing batch ID must not be empty")
	ErrClearingBatchIDTaken     = errors.New("ledger: clearing batch ID already in use")
	ErrEmptyClearingBatch       = errors.New("ledger: clearing batch must hold at least one leg")
	ErrEmptyClearingLegID       = errors.New("ledger: clearing leg ID must not be empty")
	ErrDuplicateClearingLegID   = errors.New("ledger: clearing batch holds duplicate leg IDs")
	ErrEmptyClearingLegFrom     = errors.New("ledger: clearing leg payer must not be empty")
	ErrEmptyClearingLegTo       = errors.New("ledger: clearing leg payee must not be empty")
	ErrClearingLegSameAccount   = errors.New("ledger: clearing leg payer and payee must differ")
	ErrInvalidClearingLegAmount = errors.New("ledger: clearing leg amount must be positive")
	ErrClearingBatchNotFound    = errors.New("ledger: clearing batch not found")
	ErrClearingBatchNotOpen     = errors.New("ledger: clearing batch is not open")
	ErrClearingBatchTerminal    = errors.New("ledger: clearing batch is terminal")
)

// maxClearingBatchIDLen caps batch IDs. Deterministic net-entry IDs
// embed the batch ID ("clearing/<id>/<n>"), so a bounded batch ID
// keeps entry IDs bounded too.
const maxClearingBatchIDLen = 64

// maxClearingLegIDLen caps leg IDs for the same reason batch IDs are
// capped: bounded inputs, bounded records.
const maxClearingLegIDLen = 64

// ClearingLeg is one off-book transfer intent held by a batch: From
// pays To AmountCents in Currency.
type ClearingLeg struct {
	ID          string    `json:"id"`
	From        AccountID `json:"from"`
	To          AccountID `json:"to"`
	AmountCents int64     `json:"amount_cents"`
	Currency    string    `json:"currency"`
	Memo        string    `json:"memo,omitempty"`
}

// ClearingBatch is one net-settlement batch: the held intents, their
// lifecycle state, and — once settled — the journaled net entries.
type ClearingBatch struct {
	ID        string       `json:"id"`
	Legs      []ClearingLeg `json:"legs"`
	Status    string       `json:"status"`
	CreatedAt time.Time    `json:"created_at"`
	// SettledAt is set when the batch settles; zero otherwise.
	SettledAt time.Time `json:"settled_at,omitempty"`
	// EntryIDs are the journaled net-settlement entries, in posting
	// order; nil until the batch settles.
	EntryIDs []string `json:"entry_ids,omitempty"`
}

// ClearingBatchSummary is one batch's row on the reconciliation
// report (see ReconciliationReport.ClearingBatches): enough for the
// operator to see which batches produced the rows on the books.
type ClearingBatchSummary struct {
	ID         string    `json:"id"`
	Status     string    `json:"status"`
	Legs       int       `json:"legs"`
	Currencies []string  `json:"currencies"`
	CreatedAt  time.Time `json:"created_at"`
	// SettledAt is set when the batch settles; zero otherwise.
	SettledAt time.Time `json:"settled_at,omitempty"`
}

// OpenClearingBatch validates and stores one batch of transfer intents
// in the open state. Nothing is journaled: the legs are intents, not
// money movements.
//
// Opening with a replayed idempotency key returns the original batch.
// Opening under a taken batch ID is rejected with
// ErrClearingBatchIDTaken. Leg currencies are normalized (empty means
// the default). The transition is audited.
func (l *Ledger) OpenClearingBatch(id string, legs []ClearingLeg, idempotencyKey string) (ClearingBatch, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if idempotencyKey != "" {
		if rec, ok := l.clearingKeys[idempotencyKey]; ok {
			if batch, ok := l.clearingBatches[rec.batchID]; ok {
				return batch, nil
			}
		}
	}
	if id == "" || len(id) > maxClearingBatchIDLen {
		return ClearingBatch{}, ErrEmptyClearingBatchID
	}
	if _, taken := l.clearingBatches[id]; taken {
		return ClearingBatch{}, ErrClearingBatchIDTaken
	}
	if len(legs) == 0 {
		return ClearingBatch{}, ErrEmptyClearingBatch
	}
	seen := make(map[string]bool, len(legs))
	stored := make([]ClearingLeg, len(legs))
	for i, leg := range legs {
		if leg.ID == "" || len(leg.ID) > maxClearingLegIDLen {
			return ClearingBatch{}, ErrEmptyClearingLegID
		}
		if seen[leg.ID] {
			return ClearingBatch{}, ErrDuplicateClearingLegID
		}
		seen[leg.ID] = true
		if leg.From == "" {
			return ClearingBatch{}, ErrEmptyClearingLegFrom
		}
		if leg.To == "" {
			return ClearingBatch{}, ErrEmptyClearingLegTo
		}
		if leg.From == leg.To {
			return ClearingBatch{}, ErrClearingLegSameAccount
		}
		if leg.AmountCents <= 0 {
			return ClearingBatch{}, ErrInvalidClearingLegAmount
		}
		currency, err := normalizeCurrency(leg.Currency)
		if err != nil {
			return ClearingBatch{}, err
		}
		if len(leg.Memo) > 255 {
			return ClearingBatch{}, ErrMemoTooLong
		}
		stored[i] = leg
		stored[i].Currency = currency
	}

	now := time.Now()
	batch := ClearingBatch{
		ID:        id,
		Legs:      stored,
		Status:    ClearingOpen,
		CreatedAt: now,
	}
	l.clearingBatches[batch.ID] = batch
	if idempotencyKey != "" {
		l.clearingKeys[idempotencyKey] = clearingKeyRecord{batchID: batch.ID, createdAt: now}
	}
	l.emitAudit(AuditEvent{
		Op:            "clearing_batch_open",
		Actor:         "OpenClearingBatch",
		TraceID:       batch.ID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"legs": len(batch.Legs),
		},
	})
	return batch, nil
}

// ClearingBatch returns one batch by ID.
func (l *Ledger) ClearingBatch(id string) (ClearingBatch, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	batch, ok := l.clearingBatches[id]
	return batch, ok
}

// ClearingBatches lists every batch, sorted by ID.
func (l *Ledger) ClearingBatches() []ClearingBatch {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]ClearingBatch, 0, len(l.clearingBatches))
	for _, batch := range l.clearingBatches {
		out = append(out, batch)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// CancelClearingBatch moves an open batch to cancelled, discarding its
// intents. Nothing was ever journaled, so there is nothing to unwind.
// The transition is audited.
//
// Cancelling an already-cancelled batch returns it unchanged
// (idempotent). Cancelling a settled batch is rejected with
// ErrClearingBatchTerminal: settled rows stay on the books.
func (l *Ledger) CancelClearingBatch(id string) (ClearingBatch, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	batch, ok := l.clearingBatches[id]
	if !ok {
		return ClearingBatch{}, ErrClearingBatchNotFound
	}
	if batch.Status == ClearingCancelled {
		return batch, nil
	}
	if batch.Status != ClearingOpen {
		return ClearingBatch{}, ErrClearingBatchTerminal
	}
	batch.Status = ClearingCancelled
	l.clearingBatches[id] = batch
	l.emitAudit(AuditEvent{
		Op:            "clearing_batch_cancelled",
		Actor:         "CancelClearingBatch",
		TraceID:       batch.ID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"legs": len(batch.Legs),
		},
	})
	return batch, nil
}

// clearingNetKey names one netting bucket: an account's net position
// in one currency across the batch.
type clearingNetKey struct {
	account  AccountID
	currency string
}

// SettleClearingBatch nets an open batch's legs per (account, currency)
// and books the minimal greedy transfer set atomically through the
// batch path: any failing leg rejects the whole batch before the
// first mutation, the batch stays open, and zero partial rows land.
// Retrying after a rejection is safe — failed batches leave no
// idempotency trace.
//
// The batch transitions to settled, recording the journaled entry IDs
// and the settle time; the transition is audited. Re-settling a
// settled batch returns it unchanged.
func (l *Ledger) SettleClearingBatch(id string, now time.Time) (ClearingBatch, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	batch, ok := l.clearingBatches[id]
	if !ok {
		return ClearingBatch{}, ErrClearingBatchNotFound
	}
	if batch.Status == ClearingSettled {
		return batch, nil
	}
	if batch.Status != ClearingOpen {
		return ClearingBatch{}, ErrClearingBatchTerminal
	}

	// Aggregate nets per (account, currency): payers go negative,
	// payees go positive.
	nets := make(map[clearingNetKey]int64)
	for _, leg := range batch.Legs {
		from := clearingNetKey{account: leg.From, currency: leg.Currency}
		to := clearingNetKey{account: leg.To, currency: leg.Currency}
		var ok bool
		if nets[from], ok = addCents(nets[from], -leg.AmountCents); !ok {
			return ClearingBatch{}, fmt.Errorf("ledger: clearing batch %s overflows net for %s", id, leg.From)
		}
		if nets[to], ok = addCents(nets[to], leg.AmountCents); !ok {
			return ClearingBatch{}, fmt.Errorf("ledger: clearing batch %s overflows net for %s", id, leg.To)
		}
	}

	// Group buckets by currency; each currency settles independently,
	// like the accounting equation itself.
	byCurrency := make(map[string][]clearingNetKey)
	for key, net := range nets {
		if net != 0 {
			byCurrency[key.currency] = append(byCurrency[key.currency], key)
		}
	}
	currencies := make([]string, 0, len(byCurrency))
	for c := range byCurrency {
		currencies = append(currencies, c)
	}
	sort.Strings(currencies)

	entries := make([]JournalEntry, 0)
	seq := 0
	for _, currency := range currencies {
		var debtors, creditors []clearingNetKey
		for _, key := range byCurrency[currency] {
			if nets[key] < 0 {
				debtors = append(debtors, key)
			} else {
				creditors = append(creditors, key)
			}
		}
		// Greedy: largest debtor against largest creditor, ties by
		// account ID, so identical batches always emit the identical
		// transfer set.
		sort.Slice(debtors, func(i, j int) bool {
			if nets[debtors[i]] != nets[debtors[j]] {
				return nets[debtors[i]] < nets[debtors[j]]
			}
			return debtors[i].account < debtors[j].account
		})
		sort.Slice(creditors, func(i, j int) bool {
			if nets[creditors[i]] != nets[creditors[j]] {
				return nets[creditors[i]] > nets[creditors[j]]
			}
			return creditors[i].account < creditors[j].account
		})
		di, ci := 0, 0
		for di < len(debtors) && ci < len(creditors) {
			d, c := debtors[di], creditors[ci]
			amount := -nets[d]
			if nets[c] < amount {
				amount = nets[c]
			}
			seq++
			entryID := fmt.Sprintf("clearing/%s/%d", id, seq)
			// The net payee receives: debit the creditor (their
			// balance grows) and credit the debtor (their balance
			// falls), per the transfer convention (see PostTransfer)
			// applied to the netted amounts.
			entries = append(entries, JournalEntry{
				ID:             entryID,
				DebitAccount:   c.account,
				CreditAccount:  d.account,
				AmountCents:    amount,
				Currency:       currency,
				IdempotencyKey: entryID,
				BatchID:        "clearing/" + id,
				Memo:           fmt.Sprintf("clearing batch %s net settlement", id),
				CreatedAt:      now,
			})
			nets[d] += amount
			nets[c] -= amount
			if nets[d] == 0 {
				di++
			}
			if nets[c] == 0 {
				ci++
			}
		}
	}

	versionBefore := l.version
	if len(entries) > 0 {
		receipt, err := l.postBatchLocked(Batch{
			ID:             "clearing/" + id,
			IdempotencyKey: "clearing/" + id,
			Entries:        entries,
			CreatedAt:      now,
		})
		if err != nil {
			// The batch stays open: nothing was recorded, so the
			// operator can fix the cause and settle again.
			return ClearingBatch{}, fmt.Errorf("ledger: clearing batch %s: %w", id, err)
		}
		batch.EntryIDs = make([]string, 0, len(receipt.Entries))
		for _, e := range receipt.Entries {
			batch.EntryIDs = append(batch.EntryIDs, e.ID)
		}
	}
	batch.Status = ClearingSettled
	batch.SettledAt = now
	l.clearingBatches[id] = batch
	l.emitAudit(AuditEvent{
		Op:            "clearing_batch_settled",
		Actor:         "SettleClearingBatch",
		TraceID:       batch.ID,
		VersionBefore: versionBefore,
		VersionAfter:  l.version,
		EntryIDs:      append([]string(nil), batch.EntryIDs...),
		Details: map[string]any{
			"legs":           len(batch.Legs),
			"net_entry_rows": len(batch.EntryIDs),
		},
	})
	return batch, nil
}

// clearingBatchSummariesLocked builds the Reconcile batch listing,
// sorted by batch ID. Callers must hold the read lock.
func (l *Ledger) clearingBatchSummariesLocked() []ClearingBatchSummary {
	out := make([]ClearingBatchSummary, 0, len(l.clearingBatches))
	for _, batch := range l.clearingBatches {
		cset := make(map[string]bool)
		for _, leg := range batch.Legs {
			cset[leg.Currency] = true
		}
		currencies := make([]string, 0, len(cset))
		for c := range cset {
			currencies = append(currencies, c)
		}
		sort.Strings(currencies)
		out = append(out, ClearingBatchSummary{
			ID:         batch.ID,
			Status:     batch.Status,
			Legs:       len(batch.Legs),
			Currencies: currencies,
			CreatedAt:  batch.CreatedAt,
			SettledAt:  batch.SettledAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// clearingKeyRecord remembers a batch open's idempotency key so
// replays rebuild the original batch.
type clearingKeyRecord struct {
	batchID   string
	createdAt time.Time
}

// pruneClearingKeysLocked evicts batch-open idempotency keys older
// than the ledger's idempotency TTL. Callers must hold the write lock.
func (l *Ledger) pruneClearingKeysLocked(now time.Time) int {
	if l.idempotencyTTL <= 0 {
		return 0
	}
	cutoff := now.Add(-l.idempotencyTTL)
	removed := 0
	for key, rec := range l.clearingKeys {
		if rec.createdAt.Before(cutoff) {
			delete(l.clearingKeys, key)
			removed++
		}
	}
	return removed
}
