package ledger

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// Automatic retry for failed direct-debit pulls (LG-127, the autopay /
// subscription-billing recovery flow).
//
// A direct debit can fail transiently: the payer's account is short
// (overdraft protection or insufficient available funds), the daily
// outflow limit is spent, or the monthly spend budget is exhausted —
// all conditions that may clear on their own (an incoming payroll, the
// next UTC day, the next month). Without a retry, the merchant's
// subscription billing silently misses a cycle and the operator finds
// out from the customer.
//
// A mandate opts into automatic retry with a MandateRetryPolicy: the
// maximum number of retry attempts (MaxRetries) and a backoff table
// (Backoff) naming the wait before each attempt. When DebitWithMandate
// fails with a retryable error and the mandate carries a policy, the
// failure is recorded as a pending MandateRetry with NextRetryAt set
// from the backoff table — the error is still returned to the caller,
// so the failure is never silent. SweepMandateRetries(now) is the
// single entry point that fires due retries (the schedule.go SweepDue
// convention: snapshot due retries under the lock, fire without it,
// finalize under it, with a per-retry firing flag serializing
// concurrent sweeps).
//
// Attempt accounting and idempotency: the caller-initiated debit is
// attempt 0 and keeps its original idempotency key. Every retry attempt
// n >= 1 fires under an independent idempotency key — the original key
// (or the debit ID when the caller supplied no key) suffixed with
// "#attempt-<n>" — so attempts never replay each other and a crash
// between the commit and the retry-state cleanup replays the same
// attempt key instead of double-booking. The full mandate-gate and
// Post validation sequence runs on every attempt: a mandate suspended,
// cancelled, or expired since the last attempt fails terminally, and
// the retry is dropped.
//
// Failure classification:
//   - retryable: ErrAccountOverdraft / ErrInsufficientAvailableFunds
//     (the payer is short), ErrDailyLimitExceeded, ErrBudgetExceeded —
//     the balance/limit/budget family that may clear on its own;
//   - terminal: everything else — field validation, the mandate gates
//     (unknown/suspended/cancelled/expired mandate, amount cap,
//     currency mismatch, frequency window), and the risk controls
//     (sanctions, frozen/suspended/closed accounts, period). A
//     terminal failure drops the pending retry without another attempt.
//
// Attempt outcomes enter the structured audit chain: scheduled (the
// initial failure), each rescheduled attempt, success, exhaustion, and
// terminal drop. When the attempts run out, the retry is removed and a
// mandate_retry_exhausted outbox event fires — the "mandate event" the
// item requires: downstream consumers can wire it into dunning
// escalation (see EscalateDunning in dunning.go) to chase the payer,
// exactly like a missed subscription payment.
//
// Retry policies and pending retries are in-memory operational state,
// like the sweeper firing flags and the metrics: they do not
// round-trip through disaster-recovery snapshots. A restart drops
// pending retries — the debits themselves were never booked, so there
// is nothing to reconcile; the operator re-triggers them.

// Mandate retry attempt outcomes: what one sweep attempt did,
// reported on the MandateRetryRun.
const (
	// MandateRetryOutcomeSucceeded means the attempt posted the debit.
	MandateRetryOutcomeSucceeded = "succeeded"
	// MandateRetryOutcomeRescheduled means the attempt failed
	// retryably with attempts remaining: the next attempt is scheduled.
	MandateRetryOutcomeRescheduled = "rescheduled"
	// MandateRetryOutcomeExhausted means the attempt failed retryably
	// with no attempts remaining: the retry is dropped and the
	// mandate_retry_exhausted event fired.
	MandateRetryOutcomeExhausted = "exhausted"
	// MandateRetryOutcomeTerminal means the attempt failed
	// terminally: the retry is dropped without another attempt.
	MandateRetryOutcomeTerminal = "terminal"
)

// Validation errors returned by the retry-policy APIs.
var (
	// ErrInvalidMandateRetryPolicy is returned by
	// SetMandateRetryPolicy for a policy that could never be
	// installed: fewer than one retry, an empty backoff table, or a
	// non-positive backoff step.
	ErrInvalidMandateRetryPolicy = errors.New("ledger: invalid mandate retry policy")
)

// MandateRetryPolicy is the opt-in automatic-retry policy of one
// mandate: at most MaxRetries retry attempts after the initial debit,
// waiting Backoff[i] before attempt i+1 (the table is indexed by the
// just-failed attempt number, capped at the last entry — a short table
// like [1m, 5m] reads as "1 minute, then 5 minutes, then 5 minutes").
type MandateRetryPolicy struct {
	MaxRetries int             `json:"max_retries"`
	Backoff    []time.Duration `json:"backoff"`
}

// MandateRetry is one pending automatic-retry: the failed debit, the
// policy snapshot it was scheduled under, which attempt fires next,
// and when. NextAttempt is 1-based: attempt 0 was the
// caller-initiated DebitWithMandate that failed.
type MandateRetry struct {
	ID          string             `json:"id"`
	MandateID   string             `json:"mandate_id"`
	Debit       MandateDebit       `json:"debit"`
	Policy      MandateRetryPolicy `json:"policy"`
	NextAttempt int                `json:"next_attempt"`
	NextRetryAt time.Time          `json:"next_retry_at"`
	LastError   string             `json:"last_error"`
}

// MandateRetryRun is one fired retry attempt: which pending retry it
// was, the attempt number, and what happened.
type MandateRetryRun struct {
	RetryID   string `json:"retry_id"`
	MandateID string `json:"mandate_id"`
	DebitID   string `json:"debit_id"`
	Attempt   int    `json:"attempt"`
	Outcome   string `json:"outcome"`
	EntryID   string `json:"entry_id,omitempty"`
	Error     string `json:"error,omitempty"`
}

// validateMandateRetryPolicy checks a retry policy structurally: at
// least one retry, a non-empty backoff table, every step positive.
func validateMandateRetryPolicy(p *MandateRetryPolicy) error {
	if p.MaxRetries < 1 {
		return fmt.Errorf("max_retries must be >= 1, got %d", p.MaxRetries)
	}
	if len(p.Backoff) == 0 {
		return fmt.Errorf("backoff must not be empty")
	}
	for i, b := range p.Backoff {
		if b <= 0 {
			return fmt.Errorf("backoff[%d] must be positive, got %s", i, b)
		}
	}
	return nil
}

// mandateRetryPolicyError wraps a structural validation failure in
// ErrInvalidMandateRetryPolicy so callers can match it with errors.Is.
func mandateRetryPolicyError(err error) error {
	return fmt.Errorf("%w: %s", ErrInvalidMandateRetryPolicy, err)
}

// retryBackoffFor returns the wait before the attempt following the
// just-failed attempt number: the backoff table indexed by the failed
// attempt, capped at the last entry.
func retryBackoffFor(p MandateRetryPolicy, failedAttempt int) time.Duration {
	i := failedAttempt
	if i >= len(p.Backoff) {
		i = len(p.Backoff) - 1
	}
	return p.Backoff[i]
}

// retryableMandateDebitError classifies a debit failure: the
// balance/limit/budget family may clear on its own and is worth
// retrying; validation and risk-control rejections are terminal.
func retryableMandateDebitError(err error) bool {
	return errors.Is(err, ErrAccountOverdraft) ||
		errors.Is(err, ErrInsufficientAvailableFunds) ||
		errors.Is(err, ErrDailyLimitExceeded) ||
		errors.Is(err, ErrBudgetExceeded)
}

// SetMandateRetryPolicy installs (or replaces) the opt-in automatic
// retry policy for a mandate: a retryable DebitWithMandate failure is
// recorded as a pending retry instead of dropped. The mandate must
// exist (ErrMandateNotFound); the policy is validated structurally. It
// is operational config, like the review sweeper intervals: it does
// not bump the journal version and does not round-trip through
// snapshots.
func (l *Ledger) SetMandateRetryPolicy(mandateID string, p MandateRetryPolicy) error {
	if err := validateMandateRetryPolicy(&p); err != nil {
		return mandateRetryPolicyError(err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.mandates[mandateID]; !ok {
		return ErrMandateNotFound
	}
	l.mandateRetryPolicies[mandateID] = MandateRetryPolicy{
		MaxRetries: p.MaxRetries,
		Backoff:    append([]time.Duration(nil), p.Backoff...),
	}
	return nil
}

// ClearMandateRetryPolicy removes the retry policy: later failures are
// not scheduled. Pending retries already scheduled keep the policy
// snapshot they were scheduled under and run out their schedule —
// clearing the policy is not a cancellation (see CancelMandateRetries).
func (l *Ledger) ClearMandateRetryPolicy(mandateID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.mandateRetryPolicies, mandateID)
}

// MandateRetryPolicyOf reports the retry policy of the mandate. The
// returned policy is a copy; the second return value is false when the
// mandate has no policy (retry disabled).
func (l *Ledger) MandateRetryPolicyOf(mandateID string) (MandateRetryPolicy, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	p, ok := l.mandateRetryPolicies[mandateID]
	if !ok {
		return MandateRetryPolicy{}, false
	}
	return MandateRetryPolicy{MaxRetries: p.MaxRetries, Backoff: append([]time.Duration(nil), p.Backoff...)}, true
}

// cloneMandateDebit deep-copies the slice/map fields of a debit, so a
// stored pending retry never aliases caller memory.
func cloneMandateDebit(d MandateDebit) MandateDebit {
	d.Attachments = append([]string(nil), d.Attachments...)
	if d.Metadata != nil {
		md := make(map[string]string, len(d.Metadata))
		for k, v := range d.Metadata {
			md[k] = v
		}
		d.Metadata = md
	}
	return d
}

// PendingMandateRetries lists every pending automatic retry, sorted by
// (NextRetryAt, ID). The returned debits are copies; mutating them
// does not affect the ledger.
func (l *Ledger) PendingMandateRetries() []MandateRetry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]MandateRetry, 0, len(l.mandateRetries))
	for _, rt := range l.mandateRetries {
		cp := *rt
		cp.Debit = cloneMandateDebit(rt.Debit)
		cp.Policy.Backoff = append([]time.Duration(nil), rt.Policy.Backoff...)
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].NextRetryAt.Equal(out[j].NextRetryAt) {
			return out[i].NextRetryAt.Before(out[j].NextRetryAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// CancelMandateRetries drops every pending retry of the mandate and
// returns how many were dropped. The debits themselves were never
// booked, so cancelling books nothing — it only stops the future
// attempts. A cancellation that drops nothing is silent in the audit
// trail, like the other no-op sweeps.
func (l *Ledger) CancelMandateRetries(mandateID string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	dropped := 0
	for id, rt := range l.mandateRetries {
		if rt.MandateID == mandateID {
			delete(l.mandateRetries, id)
			dropped++
		}
	}
	if dropped > 0 {
		l.emitAudit(AuditEvent{
			Op:            "mandate_debit_retry_cancelled",
			Actor:         "CancelMandateRetries",
			TraceID:       mandateID,
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Details: map[string]any{
				"cancelled": dropped,
			},
		})
	}
	return dropped
}

// debitWithMandateLocked is the attempt-aware debit entry point: the
// lock-held core (see debitMandateCoreLocked) plus the retry
// bookkeeping. attempt is the attempt number: 0 for a
// caller-initiated DebitWithMandate, >= 1 for a retry-sweep attempt
// (see SweepMandateRetries). now is the failure/success instant for
// the backoff clock. Callers must hold the write lock.
func (l *Ledger) debitWithMandateLocked(mandateID string, d MandateDebit, attempt int, now time.Time) (MandateDebitReceipt, string, error) {
	receipt, err := l.debitMandateCoreLocked(mandateID, d)
	if err != nil {
		return receipt, l.noteMandateDebitFailureLocked(mandateID, d, attempt, err, now), err
	}
	if attempt > 0 {
		if _, ok := l.mandateRetries[d.ID]; ok {
			delete(l.mandateRetries, d.ID)
			m, _ := l.mandates[mandateID]
			l.emitAudit(AuditEvent{
				Op:            "mandate_debit_retry_succeeded",
				Actor:         "SweepMandateRetries",
				TraceID:       mandateID,
				VersionBefore: l.version,
				VersionAfter:  l.version,
				EntryIDs:      []string{receipt.Entry.ID},
				Accounts:      []AccountID{m.Payer, m.Payee},
				Details: map[string]any{
					"debit_id":  d.ID,
					"attempt":   attempt,
					"entry_id":  receipt.Entry.ID,
					"duplicate": receipt.Duplicate,
				},
			})
			return receipt, MandateRetryOutcomeSucceeded, nil
		}
		// The retry was cancelled (or already finalized) between the
		// sweep snapshot and the fire: the debit still posted — it was
		// legitimately scheduled — but there is no retry state left to
		// finalize.
		return receipt, "", nil
	}
	return receipt, "", nil
}

// noteMandateDebitFailureLocked handles a failed debit attempt.
// Attempt 0 (caller-initiated) with a retryable error and an opt-in
// policy schedules a pending retry. Attempt >= 1 (sweep) reschedules
// while attempts remain, exhausts the retry when they run out, or
// drops it on a terminal failure. now is the failure instant for the
// backoff clock. Callers must hold the write lock.
func (l *Ledger) noteMandateDebitFailureLocked(mandateID string, d MandateDebit, attempt int, err error, now time.Time) string {
	if !retryableMandateDebitError(err) {
		// Terminal failure: validation and risk-control rejections
		// never retry. For a sweep attempt the pending retry is
		// dropped — the mandate state or the debit itself changed
		// since scheduling.
		if attempt > 0 {
			if _, ok := l.mandateRetries[d.ID]; ok {
				delete(l.mandateRetries, d.ID)
				l.emitAudit(AuditEvent{
					Op:            "mandate_debit_retry_terminal",
					Actor:         "SweepMandateRetries",
					TraceID:       mandateID,
					VersionBefore: l.version,
					VersionAfter:  l.version,
					Details: map[string]any{
						"debit_id": d.ID,
						"attempt":  attempt,
						"error":    err.Error(),
					},
				})
				return MandateRetryOutcomeTerminal
			}
		}
		return ""
	}
	if attempt == 0 {
		policy, ok := l.mandateRetryPolicies[mandateID]
		if !ok {
			return ""
		}
		if _, exists := l.mandateRetries[d.ID]; exists {
			// A retry is already pending for this debit (the caller
			// re-drove the same debit ID): the failure is recorded
			// once.
			return ""
		}
		l.mandateRetries[d.ID] = &MandateRetry{
			ID:          d.ID,
			MandateID:   mandateID,
			Debit:       cloneMandateDebit(d),
			Policy:      MandateRetryPolicy{MaxRetries: policy.MaxRetries, Backoff: append([]time.Duration(nil), policy.Backoff...)},
			NextAttempt: 1,
			NextRetryAt: now.Add(retryBackoffFor(policy, 0)),
			LastError:   err.Error(),
		}
		m, _ := l.mandates[mandateID]
		l.emitAudit(AuditEvent{
			Op:            "mandate_debit_retry_scheduled",
			Actor:         "DebitWithMandate",
			TraceID:       mandateID,
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Accounts:      []AccountID{m.Payer, m.Payee},
			Details: map[string]any{
				"debit_id":      d.ID,
				"next_attempt":  1,
				"max_retries":   policy.MaxRetries,
				"next_retry_at": now.Add(retryBackoffFor(policy, 0)).UTC().Format(time.RFC3339),
				"error":         err.Error(),
			},
		})
		return ""
	}
	rt, ok := l.mandateRetries[d.ID]
	if !ok {
		// Cancelled (or already finalized) between the sweep snapshot
		// and the fire: nothing to reschedule.
		return ""
	}
	if attempt < rt.Policy.MaxRetries {
		wait := retryBackoffFor(rt.Policy, attempt)
		rt.NextAttempt = attempt + 1
		rt.NextRetryAt = now.Add(wait)
		rt.LastError = err.Error()
		l.emitAudit(AuditEvent{
			Op:            "mandate_debit_retry_attempt",
			Actor:         "SweepMandateRetries",
			TraceID:       mandateID,
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Details: map[string]any{
				"debit_id":      d.ID,
				"attempt":       attempt,
				"next_attempt":  attempt + 1,
				"max_retries":   rt.Policy.MaxRetries,
				"next_retry_at": rt.NextRetryAt.UTC().Format(time.RFC3339),
				"error":         err.Error(),
			},
		})
		return MandateRetryOutcomeRescheduled
	}
	// Attempts exhausted: drop the retry and fire the mandate event.
	// Downstream consumers can wire mandate_retry_exhausted into
	// dunning escalation (see EscalateDunning) — the autopay missed a
	// cycle and the payer needs chasing.
	delete(l.mandateRetries, d.ID)
	m, _ := l.mandates[mandateID]
	l.emitAudit(AuditEvent{
		Op:            "mandate_debit_retry_exhausted",
		Actor:         "SweepMandateRetries",
		TraceID:       mandateID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{m.Payer, m.Payee},
		Details: map[string]any{
			"debit_id":    d.ID,
			"attempts":    attempt,
			"max_retries": rt.Policy.MaxRetries,
			"last_error":  err.Error(),
		},
	})
	l.emitOutboxLocked(OutboxEvent{
		Type:     OutboxMandateRetryExhausted,
		Account:  m.Payer,
		Currency: m.Currency,
		Details: map[string]any{
			"mandate_id": mandateID,
			"debit_id":   d.ID,
			"attempts":   attempt,
			"last_error": err.Error(),
		},
	})
	return MandateRetryOutcomeExhausted
}

// SweepMandateRetries fires every pending retry whose NextRetryAt is
// at or before now, returning one MandateRetryRun per fired retry. It
// never holds the ledger lock across the debit: retries are
// snapshotted under the lock, fired without it, and finalized under
// it. The per-retry firing flag serializes concurrent sweeps of the
// same retry, and the attempt's independent idempotency key makes a
// crash between the commit and the finalization replay-safe (see the
// package doc).
func (l *Ledger) SweepMandateRetries(now time.Time) []MandateRetryRun {
	l.mu.Lock()
	ids := make([]string, 0, len(l.mandateRetries))
	for id := range l.mandateRetries {
		ids = append(ids, id)
	}
	sort.Strings(ids) // deterministic fire order
	due := make([]*MandateRetry, 0)
	for _, id := range ids {
		rt := l.mandateRetries[id]
		if l.mandateRetryFiring[id] {
			continue
		}
		if rt.NextRetryAt.After(now) {
			continue
		}
		l.mandateRetryFiring[id] = true
		cp := *rt
		due = append(due, &cp)
	}
	l.mu.Unlock()

	runs := make([]MandateRetryRun, 0, len(due))
	for _, rt := range due {
		runs = append(runs, l.fireMandateRetry(rt, now))
	}
	return runs
}

// fireMandateRetry fires one pending retry attempt: the debit replays
// under an independent idempotency key — the original key (or the
// debit ID when the caller supplied no key) suffixed with
// "#attempt-<n>" — so attempts never replay each other. The full
// mandate-gate and Post validation sequence runs on the attempt.
func (l *Ledger) fireMandateRetry(rt *MandateRetry, now time.Time) MandateRetryRun {
	attempt := rt.NextAttempt
	d := cloneMandateDebit(rt.Debit)
	base := d.IdempotencyKey
	if base == "" {
		base = d.ID
	}
	d.IdempotencyKey = fmt.Sprintf("%s#attempt-%d", base, attempt)
	l.mu.Lock()
	receipt, outcome, err := l.debitWithMandateLocked(rt.MandateID, d, attempt, now)
	delete(l.mandateRetryFiring, rt.ID)
	l.mu.Unlock()
	run := MandateRetryRun{
		RetryID:   rt.ID,
		MandateID: rt.MandateID,
		DebitID:   d.ID,
		Attempt:   attempt,
		Outcome:   outcome,
	}
	if err != nil {
		run.Error = err.Error()
	} else {
		run.EntryID = receipt.Entry.ID
	}
	return run
}
