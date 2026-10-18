package ledger

import (
	"errors"
	"fmt"
	"time"
)

// Scheduled-transfer preview (LG-89).
//
// PreviewSchedule answers "what would the next <horizon> of this plan
// look like?" without firing anything: it computes the nominal firing
// times from the schedule's recurrence (Interval cadence, NextRunAt,
// EndsAt, pause/cancel status — the same arithmetic SweepDue uses) and
// then dry-runs each firing's transfer on a private deep copy of the
// ledger, exactly like DryRunTransfer does (see dryrun.go): the same
// PostTransfer admission chain (frozen → overdraft → daily-limit →
// period → budget) runs against the twin, so the validation order and
// the per-firing reasons match what the real sweeper would report.
//
// Sequential, cumulative simulation: firings are evaluated in nominal
// order on the one twin, so a passing firing's balance movement is
// visible to the later ones — the preview shows, for example, a plan
// whose fifth run drains the payer and whose sixth run would fail
// overdraft. A failing firing commits nothing (PostTransfer rejects
// before journaling), so it leaves the twin exactly as it found it and
// never poisons the later firings.
//
// Zero side effects on the live ledger, by construction:
//   - the simulation runs on the twin from cloneForDryRunLocked, whose
//     audit log is nil (no audit events, no audit-chain entries) and
//     whose outbox is nil (no domain events);
//   - idempotency keys are registered on the twin only, never on the
//     live ledger;
//   - the schedule itself is never mutated: NextRunAt, RunSeq and
//     Status are read from a snapshot, so the sweeper's state is
//     untouched and StartScheduleSweeper never sees a preview run.
//
// Missed-run skip semantics apply: an overdue NextRunAt yields one
// nominal firing (the next sweep would fire it once and skip the gap,
// exactly as fireSchedule's arithmetic advance does), not a backfill of
// every missed interval. A paused, cancelled or completed schedule
// previews zero firings.

var (
	// ErrScheduleBadHorizon is returned when a preview horizon is not
	// positive or exceeds maxSchedulePreviewHorizon.
	ErrScheduleBadHorizon = errors.New("ledger: preview horizon must be positive and at most 8760h")
)

// maxSchedulePreviewHorizon caps a preview window at one year: beyond
// that the advisory value collapses (state will have changed long before
// the last firing) and the nominal-time list would just burn CPU.
const maxSchedulePreviewHorizon = 8760 * time.Hour

// maxPreviewFirings bounds the nominal-time list against pathological
// cadences (a sub-second interval over a year-long horizon would
// otherwise produce billions of firings). Real plans — payroll, rent,
// loan servicing — never approach it; when it bites, the preview is
// truncated and the caller sees the cap in the returned slice length.
const maxPreviewFirings = 10000

// SchedulePreviewFiring is one nominal run inside a preview: whether
// the twin's admission chain would accept it, why not if it wouldn't,
// and the simulated accounting movement when it would.
type SchedulePreviewFiring struct {
	// RunSeq is the nominal run sequence: RunSeq+1 for the first
	// firing, counting up. It matches the "<ID>/run/<seq>" transfer ID
	// the real sweeper would use for this nominal run (the twin's
	// idempotency keys are discarded with the twin, so preview keys are
	// never consumed on the live ledger).
	RunSeq uint64 `json:"run_seq"`
	// TransferID is the nominal transfer ID for the run.
	TransferID string `json:"transfer_id"`
	// DueAt is the nominal due time of the run — the schedule's
	// recurrence time, mirroring ScheduleRun.DueAt. An overdue
	// schedule's first firing carries its stale NextRunAt here; the
	// simulation itself is booked at preview time (see PreviewSchedule),
	// because that is when the real sweeper would fire it.
	DueAt time.Time `json:"due_at"`
	// Admitted reports whether the full PostTransfer admission chain
	// accepted the run on the twin.
	Admitted bool `json:"admitted"`
	// Reason is the exact PostTransfer rejection on a refused run —
	// the same error string the real sweep would record. Empty when
	// admitted.
	Reason string `json:"reason,omitempty"`
	// Code is a stable machine-readable failure class for ops
	// dashboards (e.g. "account_frozen", "daily_limit_exceeded").
	// Empty when admitted.
	Code string `json:"code,omitempty"`
	// Legs is the simulated accounting movement of an admitted run,
	// in commit order with per-leg before/after balances — the same
	// shape DryRunTransfer returns. Empty on a refused run.
	Legs []DryRunLeg `json:"legs,omitempty"`
	// BudgetImpact previews the monthly spend budgets (LG-81) this
	// firing moved, relative to the twin state before this firing.
	BudgetImpact []BudgetImpact `json:"budget_impact,omitempty"`
}

// SchedulePreview is the result of PreviewSchedule: the plan's nominal
// firings inside the horizon, each with its dry-run admission verdict.
type SchedulePreview struct {
	ScheduleID string                  `json:"schedule_id"`
	Now        time.Time               `json:"now"`
	Horizon    string                  `json:"horizon"`
	Firings    []SchedulePreviewFiring `json:"firings"`
}

// scheduleFailureCode maps a PostTransfer rejection to a stable,
// machine-readable class for ops. Unknown rejections fall through to
// "rejected": the human-readable Reason always carries the full error.
func scheduleFailureCode(err error) string {
	switch {
	case errors.Is(err, ErrAccountFrozen):
		return "account_frozen"
	case errors.Is(err, ErrAccountSuspended):
		return "account_suspended"
	case errors.Is(err, ErrAccountClosed):
		return "account_closed"
	case errors.Is(err, ErrAccountDenied):
		return "account_denied"
	case errors.Is(err, ErrAccountDormant):
		return "account_dormant"
	case errors.Is(err, ErrAccountOverdraft):
		return "overdraft_not_allowed"
	case errors.Is(err, ErrDailyLimitExceeded):
		return "daily_limit_exceeded"
	case errors.Is(err, ErrPeriodClosed):
		return "period_closed"
	case errors.Is(err, ErrBudgetExceeded):
		return "budget_exceeded"
	case errors.Is(err, ErrFXRateMissing):
		return "fx_rate_missing"
	case errors.Is(err, ErrFXRateExpired):
		return "fx_rate_expired"
	case errors.Is(err, ErrFXAccountNotConfigured):
		return "fx_account_missing"
	case errors.Is(err, ErrFXSlippageExceeded):
		return "fx_slippage_exceeded"
	default:
		return "rejected"
	}
}

// previewFiringTimes computes the schedule's nominal firing times inside
// (now, now+horizon], mirroring the recurrence logic SweepDue and
// fireSchedule use. A nominal time past EndsAt completes the plan
// instead of firing (SweepDue marks it completed without a run), so it
// is not listed. An overdue NextRunAt yields one nominal time in the
// past and then advances with fireSchedule's exact skip formula —
// now + (Interval - elapsed%Interval) — so the sequence shows the one
// real run the next sweep would fire, followed by the future cadence,
// never a backfill of the missed gap.
func previewFiringTimes(s TransferSchedule, now, horizonEnd time.Time) []time.Time {
	var out []time.Time
	due := s.NextRunAt
	for len(out) < maxPreviewFirings {
		if due.After(horizonEnd) {
			break
		}
		if !s.EndsAt.IsZero() && due.After(s.EndsAt) {
			break
		}
		out = append(out, due)
		if due.After(now) {
			due = due.Add(s.Interval)
		} else {
			elapsed := now.Sub(due) // >= 0: the run is due or overdue
			due = now.Add(s.Interval - elapsed%s.Interval)
		}
	}
	return out
}

// PreviewSchedule previews the schedule's firings inside the horizon:
// the nominal firing times plus, per firing, a dry-run verdict from the
// full PostTransfer admission chain on a private twin of the ledger.
// The live ledger is untouched — no journal writes, no version bump, no
// audit-chain entries, no idempotency keys consumed, no audit events —
// and the schedule's own state (NextRunAt, RunSeq, Status) is never
// mutated. A paused, cancelled or completed schedule returns zero
// firings.
func (l *Ledger) PreviewSchedule(id string, horizon time.Duration) (SchedulePreview, error) {
	if horizon <= 0 || horizon > maxSchedulePreviewHorizon {
		return SchedulePreview{}, ErrScheduleBadHorizon
	}
	l.mu.RLock()
	sched, ok := l.schedules[id]
	if !ok {
		l.mu.RUnlock()
		return SchedulePreview{}, ErrScheduleNotFound
	}
	schedCopy := *sched
	// One twin for the whole preview: firings evaluate sequentially so
	// an admitted run's balance movement is visible to the later ones.
	twin := l.cloneForDryRunLocked()
	// Copy the budget config under the read lock: the budget-impact
	// shim below reads it without holding l.mu, and a concurrent
	// SetAccountBudget must not race it (PostTransfer on the twin
	// never mutates budget config).
	budgets := make(map[budgetKey]int64, len(l.budgets))
	for k, v := range l.budgets {
		budgets[k] = v
	}
	l.mu.RUnlock()

	now := time.Now().UTC()
	preview := SchedulePreview{
		ScheduleID: id,
		Now:        now,
		Horizon:    horizon.String(),
	}
	if schedCopy.Status != ScheduleActive {
		// Paused, cancelled and completed plans fire nothing: there is
		// nothing to preview, and this is not an error.
		preview.Firings = []SchedulePreviewFiring{}
		return preview, nil
	}

	times := previewFiringTimes(schedCopy, now, now.Add(horizon))
	firings := make([]SchedulePreviewFiring, 0, len(times))
	for i, due := range times {
		seq := schedCopy.RunSeq + 1 + uint64(i)
		transferID := fmt.Sprintf("%s/run/%d", schedCopy.ID, seq)
		// Money moves when the sweep fires, not backdated to the
		// nominal time: an overdue run is simulated at preview time
		// (when the real sweeper would pick it up), a future run at
		// its nominal due time. This is the same CreatedAt the real
		// fireSchedule would carry, which is what the period,
		// daily-limit and budget gates evaluate against.
		fireTime := due
		if fireTime.Before(now) {
			fireTime = now
		}
		t := Transfer{
			ID:             transferID,
			From:           schedCopy.From,
			To:             schedCopy.To,
			AmountCents:    schedCopy.AmountCents,
			Currency:       schedCopy.Currency,
			ToCurrency:     schedCopy.ToCurrency,
			FXAccount:      schedCopy.FXAccount,
			FeeCents:       schedCopy.FeeCents,
			FeeAccount:     schedCopy.FeeAccount,
			SkipFee:        schedCopy.SkipFee,
			Memo:           schedCopy.Memo,
			IdempotencyKey: transferID,
			CreatedAt:      fireTime,
		}

		firing := SchedulePreviewFiring{
			RunSeq:     seq,
			TransferID: transferID,
			DueAt:      due,
		}
		// Snapshot the twin's balance and spend state before the
		// firing: legs and budget impact are reported relative to it,
		// and a refused firing must leave the twin exactly as found.
		balBefore := make(map[accountCurrency]int64, len(twin.balances))
		for k, v := range twin.balances {
			balBefore[k] = v
		}
		spendBefore := make(map[monthlySpendKey]int64, len(twin.monthlySpend))
		for k, v := range twin.monthlySpend {
			spendBefore[k] = v
		}

		receipt, err := twin.PostTransfer(t)
		if err != nil {
			// A refused firing commits nothing: the twin is unchanged
			// and the next firing evaluates against the same state.
			firing.Admitted = false
			firing.Reason = err.Error()
			firing.Code = scheduleFailureCode(err)
			firings = append(firings, firing)
			continue
		}
		firing.Admitted = true
		shim := &Ledger{balances: balBefore}
		firing.Legs = shim.dryRunLegsLocked(receipt.Entries, true)
		spendShim := &Ledger{monthlySpend: spendBefore, budgets: budgets}
		firing.BudgetImpact = spendShim.budgetImpactLocked(twin)
		firings = append(firings, firing)
	}
	preview.Firings = firings
	return preview, nil
}
