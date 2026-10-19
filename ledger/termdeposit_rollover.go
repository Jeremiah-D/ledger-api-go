package ledger

import (
	"errors"
	"strconv"
	"time"
)

// Term-deposit rollover: automatic renewal at maturity (LG-139,
// fintech).
//
// A term deposit (see termdeposit.go) can carry an opt-in RolloverPolicy.
// When the maturity sweep (see SweepTermDeposits) matures such a deposit,
// it immediately opens the next term — same account, same rate, a fresh
// tenor from the policy — instead of just releasing the principal. The
// new deposit is a real deposit: it runs the full OpenTermDeposit commit
// path (closed/frozen/suspended gates plus the available-funds check), so
// a rollover can fail while the original deposit still matures —
// maturing is never held hostage by the renewal. A failed rollover is
// audited as "term_deposit_rollover_skipped" with the reason; a
// successful one as "term_deposit_rolled_over".
//
// Rollover deposit IDs are deterministic: "<deposit ID>/roll/<n>" with
// n = 1-based roll count. Deposit IDs supplied by operators stay
// slash-free (see ErrTermDepositIDSeparator), but rollover IDs are
// system-generated and contain slashes — interest attribution stays
// correct because it matches the deposit-ID segment exactly (see
// termDepositInterestAccruedLocked), not by naive prefix. In HTTP paths
// the slashes must be URL-encoded (%2F); the Go 1.22+ mux decodes them
// back into the {id} segment.
//
// Modes:
//   - none: no rollover (the zero policy; a nil policy means the same).
//   - principal: the new term locks the original principal.
//   - principal_plus_interest: the new term locks principal + the
//     interest the journal booked for the maturing deposit
//     (compounding; the interest rows stay on the books, the new
//     principal just re-locks their sum).
//
// MaxRollovers bounds the chain: a deposit that has rolled MaxRollovers
// times matures without renewal. The policy (with the incremented roll
// count) is copied onto each new deposit, so the chain self-limits.
//
// Rollover runs only in the sweep entry point, not in the direct
// MatureTermDeposit call: maturing one deposit by hand is an explicit
// operator action, and silently opening a new term underneath it would
// be a surprising side effect.

// RolloverMode is the renewal mode of a RolloverPolicy.
type RolloverMode string

// Rollover modes.
const (
	// RolloverNone disables rollover; equivalent to no policy.
	RolloverNone RolloverMode = "none"
	// RolloverPrincipal renews with the original principal.
	RolloverPrincipal RolloverMode = "principal"
	// RolloverPrincipalPlusInterest renews with principal + journaled
	// interest (compounding).
	RolloverPrincipalPlusInterest RolloverMode = "principal_plus_interest"
)

// Validation errors for rollover policies.
var (
	// ErrInvalidRolloverMode is returned for a rollover mode outside
	// none/principal/principal_plus_interest.
	ErrInvalidRolloverMode = errors.New("ledger: invalid rollover mode")
	// ErrInvalidRolloverTermDays is returned when a rollover policy's
	// term is outside 1..36500 days (same bound as the deposit tenor,
	// see ErrInvalidTermDays).
	ErrInvalidRolloverTermDays = errors.New("ledger: rollover term must be between 1 and 36500 days")
	// ErrInvalidRolloverMaxRollovers is returned when MaxRollovers is
	// below 1: a policy that can never roll is a configuration bug, not
	// a clampable value.
	ErrInvalidRolloverMaxRollovers = errors.New("ledger: rollover max rollovers must be at least 1")
)

// RolloverPolicy is the opt-in renewal policy on a TermDeposit. TermDays
// is the new term's tenor; MaxRollovers caps the renewal chain.
type RolloverPolicy struct {
	Mode         RolloverMode `json:"mode"`
	TermDays     int64        `json:"term_days"`
	MaxRollovers int          `json:"max_rollovers"`
}

// validateRolloverPolicy checks a rollover policy. A nil policy or mode
// "none"/"" means no rollover and is always valid; any other mode
// requires a sane term and a positive roll cap.
func validateRolloverPolicy(p *RolloverPolicy) error {
	if p == nil {
		return nil
	}
	switch p.Mode {
	case "", RolloverNone:
		return nil
	case RolloverPrincipal, RolloverPrincipalPlusInterest:
		// Valid modes; term and cap checked below.
	default:
		return ErrInvalidRolloverMode
	}
	if p.TermDays < 1 || p.TermDays > 36500 {
		return ErrInvalidRolloverTermDays
	}
	if p.MaxRollovers < 1 {
		return ErrInvalidRolloverMaxRollovers
	}
	return nil
}

// rolloverDepositID builds the deterministic ID of the n-th rollover of
// a deposit: "<deposit ID>/roll/<n>", n 1-based. The ID is
// system-generated (see the package doc comment on slashes); the
// deposit-ID conflict check in the open path doubles as the idempotency
// guard — a sweep that already rolled never opens a second one.
func rolloverDepositID(parentID string, n int) string {
	return parentID + "/roll/" + strconv.Itoa(n)
}

// rolloverTermDepositLocked attempts the automatic renewal of a just-
// matured deposit. It is a no-op unless the deposit carries an active
// policy (mode != none) with rolls remaining. The new deposit goes
// through the full open commit path — closed/frozen/suspended gates and
// the available-funds check — so a renewal can fail (frozen account,
// spent funds) while the original deposit stays matured; the failure is
// audited, never silent. Callers must hold l.mu (the write lock).
func (l *Ledger) rolloverTermDepositLocked(td TermDeposit, now time.Time) {
	p := td.Rollover
	if p == nil {
		return
	}
	mode := p.Mode
	if mode == "" || mode == RolloverNone {
		return
	}
	if td.RolloversUsed >= p.MaxRollovers {
		return
	}
	n := td.RolloversUsed + 1
	newID := rolloverDepositID(td.ID, n)
	if _, exists := l.termDeposits[newID]; exists {
		// Defensive: the deterministic ID already exists, so the
		// rollover already happened (or the operator claimed the ID).
		// Never open a second term under one rollover ID.
		l.emitAudit(AuditEvent{
			Op:            "term_deposit_rollover_skipped",
			Actor:         "SweepTermDeposits",
			TraceID:       td.ID,
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Accounts:      []AccountID{td.Account},
			Details: map[string]any{
				"roll_number": n,
				"reason":      "rollover ID already in use",
			},
		})
		return
	}

	principal := td.AmountCents
	if mode == RolloverPrincipalPlusInterest {
		var ok bool
		principal, ok = addCents(principal, l.termDepositInterestAccruedLocked(td.ID))
		if !ok {
			l.emitAudit(AuditEvent{
				Op:            "term_deposit_rollover_skipped",
				Actor:         "SweepTermDeposits",
				TraceID:       td.ID,
				VersionBefore: l.version,
				VersionAfter:  l.version,
				Accounts:      []AccountID{td.Account},
				Details: map[string]any{
					"roll_number": n,
					"reason":      "principal+interest overflows int64",
				},
			})
			return
		}
	}

	next := TermDeposit{
		ID:                  newID,
		Account:             td.Account,
		AmountCents:         principal,
		Currency:            td.Currency,
		RateBps:             td.RateBps,
		TermDays:            p.TermDays,
		OpenedAt:            now,
		BusinessDayMaturity: td.BusinessDayMaturity,
		Rollover: &RolloverPolicy{
			Mode:         mode,
			TermDays:     p.TermDays,
			MaxRollovers: p.MaxRollovers,
		},
		RolloversUsed: n,
		RolledFrom:    td.ID,
	}
	// The system-generated ID contains slashes, so it bypasses the
	// operator-ID slash check — but it runs the identical commit path:
	// replay/conflict checks, closed/frozen/suspended gates, and the
	// available-funds check. A renewal that cannot clear the gates
	// (frozen account, spent funds) fails here while the original
	// deposit stays matured.
	if _, _, err := l.openTermDepositLocked(next); err != nil {
		l.emitAudit(AuditEvent{
			Op:            "term_deposit_rollover_skipped",
			Actor:         "SweepTermDeposits",
			TraceID:       td.ID,
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Accounts:      []AccountID{td.Account},
			Details: map[string]any{
				"roll_number":     n,
				"reason":          err.Error(),
				"principal_cents": principal,
			},
		})
		return
	}
	l.emitAudit(AuditEvent{
		Op:            "term_deposit_rolled_over",
		Actor:         "SweepTermDeposits",
		TraceID:       newID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{td.Account},
		Details: map[string]any{
			"rolled_from":     td.ID,
			"roll_number":     n,
			"mode":            string(mode),
			"principal_cents": principal,
			"currency":        td.Currency,
			"term_days":       p.TermDays,
		},
	})
	l.emitOutboxLocked(OutboxEvent{
		Type:        OutboxTermDepositRolledOver,
		Account:     td.Account,
		AmountCents: principal,
		Currency:    td.Currency,
		Details: map[string]any{
			"term_deposit_id": newID,
			"rolled_from":     td.ID,
			"roll_number":     n,
			"mode":            string(mode),
		},
	})
}
