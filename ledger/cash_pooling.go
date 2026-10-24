package ledger

import (
	"errors"
	"fmt"
	"sync"
)

// Group cash pooling: zero-balance concentration (LG-203).
//
// A group header account concentrates the cash of its participating
// accounts: at a cut-off, every participant's positive balance is
// swept to the header in one atomic batch, leaving each participant
// at zero and the header holding the net amount. The next morning the
// participants start from zero and the header holds the group's
// position — the treasury pattern behind physical cash concentration.
//
// What this is not, stated plainly so it is never confused with its
// neighbours:
//
//   - It is not the auto-sweep rule engine (LG-178): a rule sweeps one
//     source under one trigger (a threshold, a schedule, a target
//     float). Pooling sweeps many participants at once, atomically,
//     to zero — one cut-off, one batch, one net header receipt.
//   - It is not the hierarchy rollup (LG-19): a rollup reads balances
//     and reports their sum. Pooling moves money — journal rows land,
//     balances change, and the header's position after the run is a
//     fact on the books, not a computed view.
//   - It is not funding: a participant in debit cannot be swept to
//     zero by moving money out of it. A negative participant fails
//     the whole run closed (see below) rather than being quietly
//     skipped or funded — funding a deficit is a lending decision,
//     not a concentration run, and this module never makes it
//     silently.
//
// Atomicity is the whole point: the run books through PostBatch (see
// batch.go), so every leg clears the full Post gate order and any
// failing leg — a frozen participant, a closed account, a denylist
// hit, a chart-direction violation — rejects the entire run. There
// is no partial concentration: either every participant lands at zero
// or nobody moves. A rejected run records nothing: no journal rows,
// no chain links, no version bump.
//
// Fail-closed cases, each with nothing booked:
//
//   - A participant with a negative balance: the run cannot achieve
//     its stated postcondition (every participant at zero) by
//     sweeping, so it refuses rather than concentrate a subset and
//     report success. The error names the negative participants.
//   - A participant that is frozen or closed, or any other gate
//     failure: PostBatch rejects the batch, and the error propagates
//     with nothing booked. Retrying after the block clears is safe:
//     entry IDs carry the run number (see below), so a retry never
//     collides with an earlier run.
//
// Entry identity across runs: a pool that runs twice must never
// collide with its own history. Entry IDs carry the run sequence
// number — "cashpool:<pool>:<run>:<participant>" — so a second run
// that sweeps the same participant again (new money arrived since)
// books under fresh IDs, and a retried run after a failure books
// under its own run number. The batch ID carries the same run
// number, so the journal rows of one concentration trace back to
// exactly one run.
//
// The header position report is part of the return value, not an
// afterthought: the header's balance before the run, the total swept
// in, and the header's balance after, re-read from the books —
// authoritative, not simulated. Participants already at zero are
// listed, not swept: a zero leg would book a zero-amount entry,
// which the journal rejects, so "already zero" is a report row, not
// a leg.
//
// A pool is in-memory operational config, like the auto-sweep rules:
// it does not round-trip through disaster-recovery snapshots. The
// journal rows of a run do — the money is safe; only the pool
// definition is lost, and re-registering it re-reads the restored
// books. Currency isolation is absolute: a pool names one currency
// and only ever sweeps balances in that currency.

// Cash pooling errors.
var (
	// ErrCashPoolEmptyID is returned when a pool carries no ID.
	ErrCashPoolEmptyID = errors.New("ledger: cash pool ID must not be empty")
	// ErrCashPoolEmptyHeader is returned when a pool names no header account.
	ErrCashPoolEmptyHeader = errors.New("ledger: cash pool header account must not be empty")
	// ErrCashPoolNoParticipants is returned when a pool names no participants.
	ErrCashPoolNoParticipants = errors.New("ledger: cash pool requires at least one participant")
	// ErrCashPoolEmptyParticipant is returned when a pool names an empty participant.
	ErrCashPoolEmptyParticipant = errors.New("ledger: cash pool participant must not be empty")
	// ErrCashPoolDuplicateParticipant is returned when a pool names a participant twice.
	ErrCashPoolDuplicateParticipant = errors.New("ledger: cash pool participants must be distinct")
	// ErrCashPoolHeaderParticipant is returned when the header account is also named as a participant.
	ErrCashPoolHeaderParticipant = errors.New("ledger: cash pool header must not be a participant")
	// ErrCashPoolNegativeParticipant is returned when a concentration run finds a participant in debit.
	ErrCashPoolNegativeParticipant = errors.New("ledger: cash pool cannot sweep a participant with a negative balance")
)

// CashPool is one concentration group: a header account and the
// participants whose positive balances concentrate into it at
// cut-off, in one currency.
type CashPool struct {
	ID           string      `json:"pool_id"`
	Header       AccountID   `json:"header_account"`
	Participants []AccountID `json:"participants"`
	Currency     string      `json:"currency"`

	mu  sync.Mutex
	run uint64
}

// NewCashPool validates and returns a concentration pool. The
// participant list is copied: later mutation of the caller's slice
// cannot change the pool's membership.
func NewCashPool(id string, header AccountID, participants []AccountID, currency string) (*CashPool, error) {
	if id == "" {
		return nil, ErrCashPoolEmptyID
	}
	if header == "" {
		return nil, ErrCashPoolEmptyHeader
	}
	if len(participants) == 0 {
		return nil, ErrCashPoolNoParticipants
	}
	seen := make(map[AccountID]bool, len(participants))
	for _, p := range participants {
		if p == "" {
			return nil, ErrCashPoolEmptyParticipant
		}
		if p == header {
			return nil, ErrCashPoolHeaderParticipant
		}
		if seen[p] {
			return nil, ErrCashPoolDuplicateParticipant
		}
		seen[p] = true
	}
	currency, err := normalizeCurrency(currency)
	if err != nil {
		return nil, err
	}
	return &CashPool{
		ID:           id,
		Header:       header,
		Participants: append([]AccountID(nil), participants...),
		Currency:     currency,
	}, nil
}

// CashPoolLeg is one participant's sweep inside a run.
type CashPoolLeg struct {
	Participant AccountID `json:"participant"`
	AmountCents int64     `json:"amount_cents"`
	EntryID     string    `json:"entry_id"`
}

// CashPoolReport is what one concentration run did — and the header
// position it left behind. Balances are re-read from the books, so
// HeaderBalanceAfter is authoritative, not simulated.
type CashPoolReport struct {
	PoolID              string        `json:"pool_id"`
	Run                 uint64        `json:"run"`
	Header              AccountID     `json:"header_account"`
	Currency            string        `json:"currency"`
	Legs                []CashPoolLeg `json:"legs"`
	TotalCents          int64         `json:"total_cents"`
	AlreadyZero         []AccountID   `json:"already_zero"`
	HeaderBalanceBefore int64         `json:"header_balance_before_cents"`
	HeaderBalanceAfter  int64         `json:"header_balance_after_cents"`
	BatchID             string        `json:"batch_id,omitempty"`
	EntryIDs            []string      `json:"entry_ids,omitempty"`
}

// Run concentrates every participant's positive balance into the
// header in one atomic batch, leaving each participant at zero. A
// run that finds a participant in debit, or whose batch any gate
// rejects, books nothing and returns an error. A run that finds
// every participant already at zero books nothing either, and
// returns an empty-leg report — that is a normal outcome, not an
// error: the group is already concentrated.
//
// Like the gridlock resolution, the plan is computed from a
// point-in-time snapshot of balances: a concurrent posting between
// planning and commit can change what the batch sees. The commit is
// still atomic — a leg that no longer matches the books fails the
// whole run rather than concentrating a stale amount — but Run does
// not re-plan on commit failure, it reports it.
func (p *CashPool) Run(l *Ledger) (CashPoolReport, error) {
	p.mu.Lock()
	p.run++
	run := p.run
	p.mu.Unlock()

	before := l.BalanceIn(p.Header, p.Currency)

	var legs []CashPoolLeg
	var alreadyZero []AccountID
	var negatives []AccountID
	for _, participant := range p.Participants {
		bal := l.BalanceIn(participant, p.Currency)
		switch {
		case bal > 0:
			entryID := fmt.Sprintf("cashpool:%s:%d:%s", p.ID, run, participant)
			legs = append(legs, CashPoolLeg{
				Participant: participant,
				AmountCents: bal,
				EntryID:     entryID,
			})
		case bal == 0:
			alreadyZero = append(alreadyZero, participant)
		default:
			negatives = append(negatives, participant)
		}
	}
	if len(negatives) > 0 {
		return CashPoolReport{}, fmt.Errorf("%w: %v", ErrCashPoolNegativeParticipant, negatives)
	}

	rep := CashPoolReport{
		PoolID:              p.ID,
		Run:                 run,
		Header:              p.Header,
		Currency:            p.Currency,
		Legs:                legs,
		AlreadyZero:         alreadyZero,
		HeaderBalanceBefore: before,
		HeaderBalanceAfter:  before,
	}
	if len(legs) == 0 {
		return rep, nil
	}

	// One atomic batch, in participant order. The debit/credit
	// mapping mirrors PostTransfer: the header is debited (its
	// balance rises by the swept amount) and the participant credited
	// (its balance falls to zero).
	batchID := fmt.Sprintf("cashpool:%s:%d", p.ID, run)
	entries := make([]JournalEntry, 0, len(legs))
	for _, leg := range legs {
		rep.TotalCents += leg.AmountCents
		entries = append(entries, JournalEntry{
			ID:            leg.EntryID,
			DebitAccount:  p.Header,
			CreditAccount: leg.Participant,
			AmountCents:   leg.AmountCents,
			Currency:      p.Currency,
		})
		rep.EntryIDs = append(rep.EntryIDs, leg.EntryID)
	}
	if _, err := l.PostBatch(Batch{ID: batchID, Entries: entries}); err != nil {
		// Fail closed: nothing booked, the pool untouched, the
		// caller sees the gate error and can retry after the block
		// clears — under a fresh run number, never a collision.
		return CashPoolReport{}, err
	}
	rep.BatchID = batchID
	rep.HeaderBalanceAfter = l.BalanceIn(p.Header, p.Currency)
	return rep, nil
}
