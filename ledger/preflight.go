package ledger

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Close-period preflight check (LG-119).
//
// LG-35's ClosePeriod locks a month's books the moment the operator asks.
// The lock is idempotent and cheap — which made it easy to discover,
// after the fact, that the month still had a pending dual-control
// review, an unexpired authorization hold, an unsettled FX forward, or a
// receivable sitting at collections level L3. ClosePeriodPreflight is the
// read-only counterpart: it answers "what would block a clean close?"
// before the operator locks anything.
//
// Semantics, kept honest:
//
//   - The scan is pure read: one RLock, no version bump, no journal
//     rows, no audit events, no state changes. Running a preflight can
//     never move the books — an operator may run it freely as a
//     readiness probe.
//   - Blockers are operational facts that closing would strand or
//     entrench: pending dual-control reviews (their funds would freeze
//     in approval limbo), unexpired authorization holds (reserved funds
//     whose capture would then book into a locked month), unsettled FX
//     forwards (a settlement-timing promise the ledger still owes), and
//     receivables at dunning level L3 (the collections file is at its
//     last stage; closing the month buries the trail). A lapsed forward
//     is not a blocker: expiry IS cancellation (see forward.go), so a
//     lapsed contract owes nothing — the scan applies the same lazy
//     expiry the forward-book report uses, before the ExpireForwards
//     sweep persists it.
//   - Warnings are smells, not blockers: a month with zero journal rows
//     (probably the wrong month, or a no-activity month), and a missing
//     period-closing account mapping (the ?closing=true close would fail
//     400 — see closing.go).
//   - The opt-in gate ClosePeriodWithPreflight runs the same scan and
//     refuses the close when it finds blockers (ErrPeriodPreflightBlocked,
//     HTTP 422): the period stays open and nothing is recorded. The
//     refusal itself is audit-logged (period_preflight_blocked) so the
//     attempt and its cause are on the record.

// Preflight blocker codes: the stable identifiers operators key on.
const (
	// PreflightBlockerPendingReviews: transfers parked in dual-control
	// review (see review.go) that are still actionable.
	PreflightBlockerPendingReviews = "pending_reviews"
	// PreflightBlockerActiveHolds: authorization holds that still reserve
	// funds (see hold.go); expiry is lazy, so un-swept expired holds do
	// not count.
	PreflightBlockerActiveHolds = "active_holds"
	// PreflightBlockerBookedForwards: FX forward contracts still booked
	// (see forward.go); lapsed contracts are excluded (expiry is
	// cancellation).
	PreflightBlockerBookedForwards = "booked_forwards"
	// PreflightBlockerDunningL3: receivables at dunning level L3 (see
	// dunning.go): the collections file is at its last escalation stage.
	PreflightBlockerDunningL3 = "dunning_level3"
)

// Preflight warning codes: smells that deserve a look, not a refusal.
const (
	// PreflightWarningEmptyPeriod: no journal rows are dated in the
	// period at all.
	PreflightWarningEmptyPeriod = "empty_period"
	// PreflightWarningNoClosingConfig: no period-closing account mapping
	// is configured (see SetClosingAccounts), so a ?closing=true close
	// would fail.
	PreflightWarningNoClosingConfig = "no_closing_accounts"
)

// PreflightBlocker is one reason the period cannot close cleanly: the
// stable code, how many instances, their IDs (transfer IDs, hold IDs,
// forward IDs, or "account:currency"), and a human-readable detail.
type PreflightBlocker struct {
	Code   string   `json:"code"`
	Count  int      `json:"count"`
	IDs    []string `json:"ids,omitempty"`
	Detail string   `json:"detail"`
}

// PreflightWarning is one smell found by the scan: the stable code and a
// human-readable detail.
type PreflightWarning struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// PreflightReport is the result of one ClosePeriodPreflight scan: the
// period, when the scan ran, the blockers and warnings found, and Clean
// (true when there are no blockers — warnings do not dirty a close).
type PreflightReport struct {
	Period      string             `json:"period"`
	GeneratedAt time.Time          `json:"generated_at"`
	Blockers    []PreflightBlocker `json:"blockers"`
	Warnings    []PreflightWarning `json:"warnings"`
	Clean       bool               `json:"clean"`
}

// ErrPeriodPreflightBlocked is returned by ClosePeriodWithPreflight when
// requirePreflightClean is set and the scan finds blockers: the period
// stays open, nothing is recorded. It is a 422-class semantic rejection:
// the request is well-formed, the books are not ready.
var ErrPeriodPreflightBlocked = errors.New("ledger: period close blocked by preflight findings")

// ClosePeriodPreflight scans the ledger for anything that would make a
// period close unclean, without changing anything: one RLock, no version
// bump, no audit events. The returned report's Clean is true exactly when
// there are no blockers.
func (l *Ledger) ClosePeriodPreflight(now time.Time, id string) (PreflightReport, error) {
	if err := validatePeriodID(id); err != nil {
		return PreflightReport{}, err
	}
	now = now.UTC()
	l.mu.RLock()
	defer l.mu.RUnlock()

	rep := PreflightReport{
		Period:      id,
		GeneratedAt: now,
		Blockers:    []PreflightBlocker{},
		Warnings:    []PreflightWarning{},
	}

	// Pending dual-control reviews: their funds are frozen in approval
	// limbo; closing now entrenches the freeze.
	if reviews := l.pendingTransferReviewsLocked(now); len(reviews) > 0 {
		ids := make([]string, 0, len(reviews))
		for _, r := range reviews {
			ids = append(ids, r.TransferID)
		}
		rep.Blockers = append(rep.Blockers, PreflightBlocker{
			Code:   PreflightBlockerPendingReviews,
			Count:  len(reviews),
			IDs:    ids,
			Detail: fmt.Sprintf("%d transfer(s) still awaiting dual-control approval", len(reviews)),
		})
	}

	// Unexpired authorization holds: they reserve available funds, and a
	// later capture would book into a locked month.
	var holdIDs []string
	for hid, h := range l.holds {
		if activeHoldLocked(h, now) {
			holdIDs = append(holdIDs, hid)
		}
	}
	sort.Strings(holdIDs)
	if len(holdIDs) > 0 {
		rep.Blockers = append(rep.Blockers, PreflightBlocker{
			Code:   PreflightBlockerActiveHolds,
			Count:  len(holdIDs),
			IDs:    holdIDs,
			Detail: fmt.Sprintf("%d authorization hold(s) still reserve funds", len(holdIDs)),
		})
	}

	// Booked (unsettled, unrolled-forward) FX forwards: the ledger still
	// owes their settlement. Lapsed contracts are excluded — expiry is
	// cancellation (see forward.go) — applying the same lazy-expiry
	// predicate the forward-book report uses.
	var forwardIDs []string
	for fid, c := range l.forwards {
		if c.Status == ForwardBooked && !forwardLapsed(c, now) {
			forwardIDs = append(forwardIDs, fid)
		}
	}
	sort.Strings(forwardIDs)
	if len(forwardIDs) > 0 {
		rep.Blockers = append(rep.Blockers, PreflightBlocker{
			Code:   PreflightBlockerBookedForwards,
			Count:  len(forwardIDs),
			IDs:    forwardIDs,
			Detail: fmt.Sprintf("%d FX forward contract(s) still booked", len(forwardIDs)),
		})
	}

	// Receivables at dunning level L3: the collections file is at its
	// last escalation stage (see dunning.go).
	var dunningIDs []string
	for k, st := range l.dunningStates {
		if st.Level == DunningLevelL3 {
			dunningIDs = append(dunningIDs, string(k.account)+":"+k.currency)
		}
	}
	sort.Strings(dunningIDs)
	if len(dunningIDs) > 0 {
		rep.Blockers = append(rep.Blockers, PreflightBlocker{
			Code:   PreflightBlockerDunningL3,
			Count:  len(dunningIDs),
			IDs:    dunningIDs,
			Detail: fmt.Sprintf("%d receivable(s) at dunning level L3", len(dunningIDs)),
		})
	}

	// A month with no journal rows at all: probably the wrong month, or a
	// no-activity month — worth a look, not a refusal.
	var rows int
	for _, e := range l.entries {
		if periodIDOf(e.CreatedAt) == id {
			rows++
		}
	}
	if rows == 0 {
		rep.Warnings = append(rep.Warnings, PreflightWarning{
			Code:   PreflightWarningEmptyPeriod,
			Detail: "no journal rows are dated in this period",
		})
	}

	// No period-closing account mapping: a ?closing=true close would fail
	// (see closing.go), so flag it before the operator tries.
	if _, retained := l.closingAccountsLocked(); retained == "" {
		rep.Warnings = append(rep.Warnings, PreflightWarning{
			Code:   PreflightWarningNoClosingConfig,
			Detail: "no period-closing account mapping configured",
		})
	}

	rep.Clean = len(rep.Blockers) == 0
	return rep, nil
}

// ClosePeriodWithPreflight locks the period like ClosePeriodCtx, with an
// opt-in preflight gate: when requirePreflightClean is true the scan runs
// first and any blocker refuses the close with ErrPeriodPreflightBlocked
// — the period stays open and nothing is recorded. The refusal is
// audit-logged (op period_preflight_blocked) so the attempt and its
// cause are on the record; a clean scan closes exactly like ClosePeriodCtx.
func (l *Ledger) ClosePeriodWithPreflight(ctx context.Context, id string, requirePreflightClean bool) error {
	if requirePreflightClean {
		rep, err := l.ClosePeriodPreflight(time.Now().UTC(), id)
		if err != nil {
			return err
		}
		if len(rep.Blockers) > 0 {
			l.mu.Lock()
			l.emitAuditCtx(ctx, AuditEvent{
				Op:            "period_preflight_blocked",
				Actor:         "ClosePeriod",
				TraceID:       id,
				VersionBefore: l.version,
				VersionAfter:  l.version,
				Details: map[string]any{
					"blockers":      len(rep.Blockers),
					"blocker_codes": blockerCodes(rep.Blockers),
				},
			})
			l.mu.Unlock()
			return fmt.Errorf("%w: %d blocker(s), first: %s", ErrPeriodPreflightBlocked, len(rep.Blockers), rep.Blockers[0].Code)
		}
	}
	return l.ClosePeriodCtx(ctx, id)
}

// blockerCodes extracts the stable codes from a blocker list for the
// audit event: the event names the cause, the report carries the detail.
func blockerCodes(blockers []PreflightBlocker) []string {
	codes := make([]string, 0, len(blockers))
	for _, b := range blockers {
		codes = append(codes, b.Code)
	}
	return codes
}
