package ledger

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
)

// RTGS liquidity-saving queue with gridlock resolution (LG-202).
//
// The problem this answers is the one every real-time gross settlement
// operator knows: a payment arrives, the payer's account is short, and
// the payment cannot settle — yet. Dropping it would lose the
// instruction; settling it partially would corrupt the amount; booking
// it against an overdraft would invent central-bank money. So the
// payment waits, in full, in a queue — and the queue periodically looks
// for a set of waiting payments that can settle together.
//
// A payment is never partially settled and never silently discarded:
// it leaves the queue only by settling in full inside one atomic batch,
// and a failed resolution leaves the queue exactly as it was.
//
// Feasibility, honestly defined. Overdraft protection in this ledger is
// opt-in per account (see overdraft.go), so the queue cannot borrow
// the Post gate's answer to "can this payer pay". The queue defines
// its own rule, the RTGS rule: a payer may pay only out of a
// non-negative balance, and the batch settles only if its legs can be
// ordered so that no payer's running balance ever goes negative. That
// is a zero floor, applied to simulated balances that start from the
// books (see BalanceIn) and move with each applied leg.
//
// The search is a greedy fixpoint, and the fixpoint is the right
// answer, not an approximation: repeatedly apply any queued payment
// whose payer can currently pay, in queue order, until no queued
// payment can pay. If a feasible ordering of some subset exists, this
// finds a maximal one — moving any currently payable payment to the
// front of a feasible ordering keeps it feasible (the payer was able
// to pay up front, and every payment that follows it in the original
// ordering still sees at least the balance it saw there, except a
// payment from the same payer that preceded it — which the payer could
// still afford, because in the original ordering that payment left the
// payer with enough to pay the moved payment afterwards). When the
// fixpoint stops with payments remaining, no non-empty subset of the
// remainder is feasible at all: any feasible subset would need a first
// payment, and no remaining payment's payer can pay. So the settled
// set is maximal, and the remainder is genuinely stuck, not merely
// unordered.
//
// The deliberate limit is the zero-balance cycle: A owes B 100 and
// B owes A 100, both balances zero. Together they net to zero, but
// no ordering of sequential legs clears them under a zero floor —
// whichever leg goes first drives its payer negative — so the
// fixpoint settles nothing and the cycle stays queued for an operator
// funding decision. That is the queue refusing to pretend: netting
// obligations down to a smaller settlement is a different operation
// (the netting session, LG-56, settles already-booked positions;
// this queue settles payment instructions at full value). What the
// queue does resolve is everything sequential settlement can
// actually clear: chains unlocked by an inflow, and cycles primed by
// at least one funded payer.
//
// Settlement is one atomic batch through PostBatch (see batch.go), so
// every leg clears the full Post gate order and any failing leg
// rejects the whole resolution: no journal rows, no chain links, no
// version bump, and the queue is untouched. A payment whose payer or
// payee is frozen or closed is skipped for this run — it stays queued
// — so one blocked account cannot stall the rest of the queue; the
// batch's own gates remain the backstop for anything the pre-filter
// cannot see (denylist, chart direction), and a failure there fails
// the whole run closed.
//
// Currency isolation is absolute: a resolution names one currency
// and only ever looks at, simulates, and settles payments in that
// currency. Queues are per currency in effect, never mixed, like the
// accounting equation itself.
//
// The queue is in-memory operational state, like the auto-sweep rules
// (see autosweep.go): it does not round-trip through
// disaster-recovery snapshots. The journal rows of settled batches
// do — the money is safe; only the waiting list is lost, and a
// re-enqueue after a restore re-validates from the restored books.
// Admission is the caller's routing decision: the RTGS operator
// routes a payment here when the payer's available balance is short.
// Enqueue itself validates shape, never affordability — a payment
// that could settle immediately still queues, and the next Resolve
// settles it. Queueing is never a silent drop and never a partial
// settlement, whatever the caller routes in.

// Gridlock queue errors.
var (
	// ErrGridlockEmptyPaymentID is returned when a queued payment carries no ID.
	ErrGridlockEmptyPaymentID = errors.New("ledger: gridlock payment ID must not be empty")
	// ErrGridlockEmptyAccount is returned when a queued payment names an empty payer or payee.
	ErrGridlockEmptyAccount = errors.New("ledger: gridlock payment account must not be empty")
	// ErrGridlockSameAccount is returned when a queued payment's payer and payee are the same account.
	ErrGridlockSameAccount = errors.New("ledger: gridlock payment payer and payee must differ")
	// ErrGridlockNonPositiveAmount is returned when a queued payment's amount is not positive.
	ErrGridlockNonPositiveAmount = errors.New("ledger: gridlock payment amount must be positive")
	// ErrGridlockDuplicatePayment is returned when a payment ID is enqueued twice, including after it settled.
	ErrGridlockDuplicatePayment = errors.New("ledger: gridlock payment ID already queued or settled")
)

// QueuedPayment is one RTGS payment instruction waiting for liquidity:
// the full amount, never a partial one.
type QueuedPayment struct {
	ID          string    `json:"payment_id"`
	From        AccountID `json:"from_account"`
	To          AccountID `json:"to_account"`
	AmountCents int64     `json:"amount_cents"`
	Currency    string    `json:"currency"`
	EnqueuedAt  time.Time `json:"enqueued_at"`
}

// GridlockQueue is the liquidity-saving queue: FIFO per currency,
// drained only by atomic all-or-nothing resolutions.
type GridlockQueue struct {
	mu     sync.Mutex
	queued []QueuedPayment
	seen   map[string]bool
}

// NewGridlockQueue returns an empty liquidity-saving queue.
func NewGridlockQueue() *GridlockQueue {
	return &GridlockQueue{seen: make(map[string]bool)}
}

// Enqueue admits one payment instruction to the queue. It validates
// shape only — ID, distinct non-empty accounts, positive amount, valid
// currency (empty means the default currency, see normalizeCurrency) —
// never affordability: admission is the operator's routing decision.
// A payment ID is single-use for the life of the queue: re-enqueueing
// an ID that already queued, or already settled, is rejected, so a
// settled payment can never re-enter and double-book.
func (q *GridlockQueue) Enqueue(p QueuedPayment) error {
	if p.ID == "" {
		return ErrGridlockEmptyPaymentID
	}
	if p.From == "" || p.To == "" {
		return ErrGridlockEmptyAccount
	}
	if p.From == p.To {
		return ErrGridlockSameAccount
	}
	if p.AmountCents <= 0 {
		return ErrGridlockNonPositiveAmount
	}
	currency, err := normalizeCurrency(p.Currency)
	if err != nil {
		return err
	}
	p.Currency = currency
	if p.EnqueuedAt.IsZero() {
		p.EnqueuedAt = time.Now()
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	if q.seen[p.ID] {
		return ErrGridlockDuplicatePayment
	}
	q.seen[p.ID] = true
	q.queued = append(q.queued, p)
	return nil
}

// Queued returns a snapshot of the payments still waiting in one
// currency, in queue order. An empty currency returns every currency.
func (q *GridlockQueue) Queued(currency string) []QueuedPayment {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]QueuedPayment, 0, len(q.queued))
	for _, p := range q.queued {
		if currency == "" || p.Currency == currency {
			out = append(out, p)
		}
	}
	return out
}

// GridlockResolution is what one Resolve run did: the payments it
// settled, in settlement order, and the payments still waiting in
// that currency, in queue order.
type GridlockResolution struct {
	Currency     string          `json:"currency"`
	Settled      []QueuedPayment `json:"settled"`
	SettledTotal int64           `json:"settled_total_cents"`
	Remaining    []QueuedPayment `json:"remaining"`
	BatchID      string          `json:"batch_id,omitempty"`
	EntryIDs     []string        `json:"entry_ids,omitempty"`
}

// Resolve settles the maximal feasible set of queued payments in one
// currency as a single atomic batch, and leaves the rest queued. When
// nothing can settle it books nothing and returns an empty settled
// set — that is a normal outcome, not an error. When the batch commit
// itself fails (a gate the pre-filter cannot see), the error is
// returned and the queue is left exactly as it was: no partial
// settlement, no silent drop.
//
// The plan is computed from a point-in-time snapshot of balances: a
// concurrent posting between planning and commit can change what the
// batch sees. The commit is still atomic, and the batch's own gates
// are the backstop — but Resolve does not re-plan on commit failure,
// it reports it. Callers that need strict serialization should hold
// their own posting discipline around Resolve, the way the netting
// session holds the write lock around its plan-and-commit.
func (q *GridlockQueue) Resolve(l *Ledger, currency string) (GridlockResolution, error) {
	currency, err := normalizeCurrency(currency)
	if err != nil {
		return GridlockResolution{}, err
	}

	q.mu.Lock()
	candidates := make([]QueuedPayment, 0)
	for _, p := range q.queued {
		if p.Currency == currency {
			candidates = append(candidates, p)
		}
	}
	q.mu.Unlock()

	// Greedy fixpoint over simulated balances. Blocked accounts
	// (frozen or closed) make their payments unpayable for this run;
	// they stay queued and never block the rest.
	sim := make(map[AccountID]int64)
	balance := func(a AccountID) int64 {
		if b, ok := sim[a]; ok {
			return b
		}
		b := l.BalanceIn(a, currency)
		sim[a] = b
		return b
	}
	blocked := func(p QueuedPayment) bool {
		return l.IsFrozen(p.From) || l.IsFrozen(p.To) || l.IsClosed(p.From) || l.IsClosed(p.To)
	}

	remaining := candidates
	var settled []QueuedPayment
	for len(remaining) > 0 {
		progress := false
		next := remaining[:0]
		// Reuse the backing array: settled payments are dropped as
		// the scan goes, remaining ones compact in place.
		for _, p := range remaining {
			if !blocked(p) && balance(p.From) >= p.AmountCents {
				sim[p.From] -= p.AmountCents
				sim[p.To] = saturatingAdd(balance(p.To), p.AmountCents)
				settled = append(settled, p)
				progress = true
			} else {
				next = append(next, p)
			}
		}
		remaining = next
		if !progress {
			break
		}
	}

	res := GridlockResolution{
		Currency:  currency,
		Settled:   settled,
		Remaining: remaining,
	}
	if len(settled) == 0 {
		return res, nil
	}

	// One atomic batch, in settlement order. Entry IDs derive from
	// the payment IDs, which are single-use for the queue's life, so
	// a settled payment's entry ID can never collide with a later
	// batch. The debit/credit mapping mirrors PostTransfer: the
	// payee is debited (its balance rises) and the payer credited
	// (its balance falls).
	batchID := fmt.Sprintf("gridlock:%s:%d", currency, time.Now().UnixNano())
	entries := make([]JournalEntry, 0, len(settled))
	for _, p := range settled {
		res.SettledTotal += p.AmountCents
		entryID := "gridlock:" + p.ID
		entries = append(entries, JournalEntry{
			ID:            entryID,
			DebitAccount:  p.To,
			CreditAccount: p.From,
			AmountCents:   p.AmountCents,
			Currency:      currency,
		})
		res.EntryIDs = append(res.EntryIDs, entryID)
	}
	if _, err := l.PostBatch(Batch{ID: batchID, Entries: entries}); err != nil {
		// Fail closed: the queue is untouched, the caller sees the
		// gate error, and nothing was partially settled.
		return GridlockResolution{}, err
	}
	res.BatchID = batchID

	// Commit the queue change only after the batch landed.
	settledIDs := make(map[string]bool, len(settled))
	for _, p := range settled {
		settledIDs[p.ID] = true
	}
	q.mu.Lock()
	kept := q.queued[:0]
	for _, p := range q.queued {
		if !settledIDs[p.ID] {
			kept = append(kept, p)
		}
	}
	q.queued = kept
	q.mu.Unlock()
	return res, nil
}

// saturatingAdd adds two balances without wrapping: the simulation
// only ever moves toward real balances, but a saturating cap keeps a
// pathological input from wrapping a payer's simulated balance
// negative and unlocking payments that cannot settle.
func saturatingAdd(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	if b < 0 && a < math.MinInt64-b {
		return math.MinInt64
	}
	return a + b
}
