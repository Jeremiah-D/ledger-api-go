package ledger

import (
	"errors"
	"fmt"
)

// Payment-versus-payment (PvP) bilateral synchronous settlement (LG-200).
//
// PvP answers the Herstatt-risk question for an FX exchange: two
// counterparties swap value in two different currencies, and neither
// leg may settle without the other. If the USD leg paid out while the
// EUR leg failed, the payer of the settled leg would hold an unsecured
// claim on a counterparty that may never deliver — the loss Herstatt
// Bank's 1974 failure made famous. PvP removes that window by making
// the two legs settle together or not at all.
//
// Flow, in order:
//
//  1. Hold — both legs are frozen (held) before either is posted.
//     A settlement that cannot hold both legs never reaches commit.
//  2. Commit — only from the held state, only when the caller supplies
//     a distinct journal entry ID for each leg. Commit is all-or-nothing
//     at this layer: a missing, empty, or duplicated entry ID rejects
//     the whole commit and the settlement stays held, so the caller
//     can retry or roll back — it never reports a half-committed PvP.
//  3. Rollback — from the created or held state, when either leg
//     fails. Both holds are released together; a rolled-back
//     settlement can never commit afterwards.
//
// Audit linkage: a committed settlement yields one audit link per
// leg, and each link names the other leg's journal entry
// (CounterpartyEntryID). The two links therefore point at each other,
// so an auditor can walk from either leg's journal entry to its PvP
// counterpart without any out-of-band mapping. Audit links carry
// settlement, leg, and entry IDs plus currency and amount only —
// never an account holder name or any other PII, and never an
// account ID: the link is between entries, not between people.
//
// Orthogonality: this is full-amount synchronous settlement, not
// netting.go (which replaces a set of obligations with a smaller
// minimal transfer set — a PvP pair nets to nothing useful, since
// the legs are in different currencies) and not settlement.go
// (LG-43, the merchant end-of-day settlement view, which reports
// after the fact rather than executing settlement). It is also not
// the split path (PostTransferSplit): a split is one payment in
// pieces; a PvP pair is two payments that must land together.
//
// Boundary, stated plainly: this module is the settlement decision
// and hold state. It books no journal entries itself — the caller
// posts both legs through the normal posting path after Hold and
// hands the resulting entry IDs to Commit, exactly like the
// positive-pay module books nothing and the caller posts only
// cleared checks. A settlement instance is owned by a single
// settlement flow; concurrent use of one instance needs external
// synchronization.

// PvP settlement statuses.
const (
	// PvPStatusCreated: the pair was validated but neither leg is held yet.
	PvPStatusCreated = "created"
	// PvPStatusHeld: both legs are held; commit or rollback is next.
	PvPStatusHeld = "held"
	// PvPStatusCommitted: both legs committed together; terminal.
	PvPStatusCommitted = "committed"
	// PvPStatusRolledBack: both holds were released; terminal.
	PvPStatusRolledBack = "rolled_back"
)

// PvP errors.
var (
	// ErrPvPInvalidSettlement is returned for a settlement whose ID is empty.
	ErrPvPInvalidSettlement = errors.New("ledger: pvp settlement ID must not be empty")
	// ErrPvPInvalidLeg is returned for a leg that fails validation
	// (empty leg ID or account, non-positive amount, or a leg pair
	// that is not two distinct legs in two distinct currencies).
	ErrPvPInvalidLeg = errors.New("ledger: invalid pvp leg")
	// ErrPvPInvalidState is returned when an operation is attempted
	// from a state that does not allow it (e.g. committing a
	// rolled-back settlement, or holding twice).
	ErrPvPInvalidState = errors.New("ledger: pvp settlement is in a state that does not allow this operation")
	// ErrPvPCommitIncomplete is returned when Commit is called
	// without a distinct, non-empty journal entry ID for each leg.
	ErrPvPCommitIncomplete = errors.New("ledger: pvp commit requires a distinct journal entry ID for each leg")
)

// PvPLeg is one side of a PvP pair: one counterparty pays AmountCents
// in Currency from Account. The two legs of a settlement must be in
// different currencies and from different accounts — a same-currency,
// same-account pair is a plain transfer, not a PvP exchange.
type PvPLeg struct {
	// LegID identifies this leg within the settlement; it must be
	// distinct from the other leg's ID.
	LegID string `json:"leg_id"`
	// Account is the payer account for this leg.
	Account AccountID `json:"account"`
	// Currency is the ISO currency code for this leg.
	Currency string `json:"currency"`
	// AmountCents is the leg amount in minor units; must be positive.
	AmountCents int64 `json:"amount_cents"`
}

// PvPAuditLink is one leg's audit record for a committed settlement.
// Each leg's link names the other leg's journal entry, so the pair
// of links is bidirectional. No account IDs and no holder names:
// the link is between entries.
type PvPAuditLink struct {
	// SettlementID is the PvP settlement both legs belong to.
	SettlementID string `json:"settlement_id"`
	// LegID is this link's leg.
	LegID string `json:"leg_id"`
	// EntryID is this leg's journal entry ID.
	EntryID string `json:"entry_id"`
	// CounterpartyLegID is the other leg's ID.
	CounterpartyLegID string `json:"counterparty_leg_id"`
	// CounterpartyEntryID is the other leg's journal entry ID.
	CounterpartyEntryID string `json:"counterparty_entry_id"`
	// Currency and AmountCents describe this leg's value.
	Currency    string `json:"currency"`
	AmountCents int64  `json:"amount_cents"`
}

// PvPSettlement is the hold/commit/rollback state machine for one
// two-legged PvP exchange. The zero value is unusable: build one
// with NewPvPSettlement, which validates the pair up front.
type PvPSettlement struct {
	id         string
	legs       [2]PvPLeg
	status     string
	failedLeg  string
	failReason string
	links      []PvPAuditLink
}

// NewPvPSettlement validates a two-legged PvP pair and returns a
// settlement in the created state. Validation is fail-closed: an
// empty settlement ID, an empty leg ID or account, a non-positive
// amount, duplicate leg IDs, duplicate payer accounts, or legs in
// the same currency all reject the pair before anything is held.
func NewPvPSettlement(id string, a, b PvPLeg) (*PvPSettlement, error) {
	if id == "" {
		return nil, ErrPvPInvalidSettlement
	}
	if err := validatePvPLeg(a); err != nil {
		return nil, err
	}
	if err := validatePvPLeg(b); err != nil {
		return nil, err
	}
	if a.LegID == b.LegID {
		return nil, fmt.Errorf("%w: duplicate leg ID %q", ErrPvPInvalidLeg, a.LegID)
	}
	if a.Account == b.Account {
		return nil, fmt.Errorf("%w: both legs pay from the same account", ErrPvPInvalidLeg)
	}
	if a.Currency == b.Currency {
		return nil, fmt.Errorf("%w: both legs are in %s, a pvp pair needs two currencies", ErrPvPInvalidLeg, a.Currency)
	}
	return &PvPSettlement{id: id, legs: [2]PvPLeg{a, b}, status: PvPStatusCreated}, nil
}

func validatePvPLeg(leg PvPLeg) error {
	if leg.LegID == "" {
		return fmt.Errorf("%w: leg ID must not be empty", ErrPvPInvalidLeg)
	}
	if leg.Account == "" {
		return fmt.Errorf("%w: leg %q account must not be empty", ErrPvPInvalidLeg, leg.LegID)
	}
	if leg.Currency == "" {
		return fmt.Errorf("%w: leg %q currency must not be empty", ErrPvPInvalidLeg, leg.LegID)
	}
	if leg.AmountCents <= 0 {
		return fmt.Errorf("%w: leg %q amount %d must be positive", ErrPvPInvalidLeg, leg.LegID, leg.AmountCents)
	}
	return nil
}

// ID returns the settlement ID.
func (s *PvPSettlement) ID() string { return s.id }

// Status returns the current status: created, held, committed, or
// rolled_back.
func (s *PvPSettlement) Status() string { return s.status }

// Legs returns a copy of the two legs in construction order.
func (s *PvPSettlement) Legs() [2]PvPLeg { return s.legs }

// FailedLeg returns the leg ID recorded by FailAndRollback, or ""
// when the settlement was not failed through that path.
func (s *PvPSettlement) FailedLeg() string { return s.failedLeg }

// FailReason returns the reason recorded by FailAndRollback or
// Rollback, or "" when neither was called.
func (s *PvPSettlement) FailReason() string { return s.failReason }

// AuditLinks returns the bidirectional audit links for a committed
// settlement, or nil before commit. The returned slice is a copy.
func (s *PvPSettlement) AuditLinks() []PvPAuditLink {
	if s.links == nil {
		return nil
	}
	out := make([]PvPAuditLink, len(s.links))
	copy(out, s.links)
	return out
}

// Hold freezes both legs at once: it moves a created settlement to
// held. Holding twice, or holding a terminal settlement, is an
// error — a hold is not silently re-applied, because a second hold
// would double-count the frozen value.
func (s *PvPSettlement) Hold() error {
	if s.status != PvPStatusCreated {
		return fmt.Errorf("%w: cannot hold a settlement in state %q", ErrPvPInvalidState, s.status)
	}
	s.status = PvPStatusHeld
	return nil
}

// Commit settles both legs together. The caller passes one journal
// entry ID per leg, keyed by leg ID; both entries must be present,
// non-empty, and distinct. Any gap rejects the whole commit and
// leaves the settlement held, so the caller can retry the post or
// roll back — a partial commit is never reported. On success the
// settlement is committed and AuditLinks returns the bidirectional
// pair.
func (s *PvPSettlement) Commit(entryIDs map[string]string) error {
	if s.status != PvPStatusHeld {
		return fmt.Errorf("%w: cannot commit a settlement in state %q", ErrPvPInvalidState, s.status)
	}
	if len(entryIDs) != 2 {
		return fmt.Errorf("%w: expected entry IDs for both legs, got %d", ErrPvPCommitIncomplete, len(entryIDs))
	}
	first, okFirst := entryIDs[s.legs[0].LegID]
	second, okSecond := entryIDs[s.legs[1].LegID]
	if !okFirst || !okSecond || first == "" || second == "" {
		return fmt.Errorf("%w: each leg needs a non-empty journal entry ID", ErrPvPCommitIncomplete)
	}
	if first == second {
		return fmt.Errorf("%w: both legs share journal entry ID %q", ErrPvPCommitIncomplete, first)
	}
	s.links = []PvPAuditLink{
		{
			SettlementID: s.id, LegID: s.legs[0].LegID, EntryID: first,
			CounterpartyLegID: s.legs[1].LegID, CounterpartyEntryID: second,
			Currency: s.legs[0].Currency, AmountCents: s.legs[0].AmountCents,
		},
		{
			SettlementID: s.id, LegID: s.legs[1].LegID, EntryID: second,
			CounterpartyLegID: s.legs[0].LegID, CounterpartyEntryID: first,
			Currency: s.legs[1].Currency, AmountCents: s.legs[1].AmountCents,
		},
	}
	s.status = PvPStatusCommitted
	return nil
}

// Rollback releases both holds together and moves a created or held
// settlement to rolled_back. A committed settlement can never roll
// back, and a rolled-back settlement can never commit afterwards —
// terminal means terminal.
func (s *PvPSettlement) Rollback(reason string) error {
	if s.status != PvPStatusCreated && s.status != PvPStatusHeld {
		return fmt.Errorf("%w: cannot roll back a settlement in state %q", ErrPvPInvalidState, s.status)
	}
	s.failReason = reason
	s.status = PvPStatusRolledBack
	return nil
}

// FailAndRollback records which leg failed and rolls both legs back
// together: the failing leg's failure is the whole pair's failure.
// It is allowed from the created or held state, so a leg that fails
// before its hold completes still unwinds the pair. The failed leg
// ID must be one of the settlement's legs.
func (s *PvPSettlement) FailAndRollback(failedLegID, reason string) error {
	if failedLegID != s.legs[0].LegID && failedLegID != s.legs[1].LegID {
		return fmt.Errorf("%w: unknown leg %q", ErrPvPInvalidLeg, failedLegID)
	}
	if err := s.Rollback(reason); err != nil {
		return err
	}
	s.failedLeg = failedLegID
	return nil
}
