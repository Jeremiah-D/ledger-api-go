package ledger

import (
	"context"
	"fmt"
	"time"
)

// Scheduled-report subscription runtime revision (LG-315).
//
// CreateReportSchedule fixes every field of a subscription at creation
// time; after that only Pause/Resume/Cancel exist. Until now the only
// way to change a cadence, the next run time, the end time, the report
// parameters, or the delivery target was Cancel + recreate: a new
// subscription whose RunSeq and bounded run history (maxReportRuns)
// start over, and whose ID still collides with the cancelled record.
// This module is the orthogonal runtime path: an operator revises any
// subset of a live subscription's fields in place, keeping its
// identity, its RunSeq, and its run history.
//
// Semantics, deliberately fail-closed throughout, along the LG-308
// ReviseAPIKey patch shape and the LG-253/254 transfer-schedule amend
// family:
//
//   - The revision is in place: the subscription keeps its ID, Kind,
//     Status, RunSeq, CreatedAt, and IdempotencyKey — none of them
//     has a field on the patch. Status moves only through
//     Pause/Resume/Cancel. Only the patched fields move.
//   - Check order is unknown -> state -> per-field validation ->
//     apply. An unknown ID returns ErrReportScheduleNotFound;
//     cancelled or completed subscriptions return
//     ErrReportScheduleBadTransition (terminal states, along
//     CancelReportSchedule); paused subscriptions are revisable —
//     pausing is a hold, not a terminal state, and the revision waits
//     with the schedule so the first run after resume uses the new
//     values.
//   - Validation is merge-then-install: the partial patch is merged
//     over the live subscription and the merged record reuses the
//     creation gates via validateReportScheduleRecord — Interval must
//     be positive (ErrReportScheduleBadInterval), NextRunAt non-zero
//     (ErrReportScheduleBadNextRunAt), EndsAt after the effective
//     NextRunAt (patch value or live value, ErrReportScheduleBadEndsAt),
//     Params validated for the schedule's Kind
//     (ErrReportScheduleBadParams), Sink via ReportSink.Validate()
//     (ErrReportScheduleBadSink). A merged record that fails any gate
//     leaves the live subscription exactly as it was: validate all
//     first, apply once, under a single l.mu write lock.
//   - A non-nil zero EndsAt clears the end time (the subscription
//     never ends): zero is the only value that cannot be a live
//     EndsAt, since the creation gate rejects an EndsAt at or before
//     NextRunAt. A non-nil zero NextRunAt is rejected instead —
//     NextRunAt has no "clear" meaning.
//   - An all-nil patch, and a revision identical to the live
//     subscription on every revisable field, are no-ops: they return
//     the live subscription with a nil error and write no audit
//     event, so a retried or duplicated revision cannot spam the
//     audit trail.
//   - Interval only changes the cadence, never NextRunAt: the new
//     interval applies from the next advance. Ordering note —
//     SweepReportsDue snapshots each due schedule (including its
//     Interval) under the lock and finalizes the run later, so a
//     revision that lands after a sweep snapshot but before that
//     run's finalize still advances that run by the snapshot
//     interval; the new interval governs from the following advance
//     (the same in-window snapshot ordering as LG-314's in-flight
//     sleep).
//   - A NextRunAt revision moves the next trigger directly. A past
//     value fires on the next sweep, along the creation-time "past
//     NextRunAt fires first sweep" semantics. Tightening EndsAt to
//     at or before the effective NextRunAt is rejected by the
//     creation gate — a revision never manufactures a state creation
//     would have refused. Widening only extends the lifetime.
//   - A Sink revision affects the next delivery only: already
//     delivered files and webhooks are untouched. A URL sink's Secret
//     never enters the audit trail or serialization (ReportSink.Secret
//     is json:"-"); the audit event records only the sink name.
//   - The amendment is structural config, not ledger state: it bumps
//     neither the version nor the audit chain, writes no journal
//     entry, and emits no outbox event. It lands one
//     report_schedule_revised audit event (previous_*/current_* per
//     changed field). The Ctx variant attributes the operator
//     (LG-73). RunSeq and run history are untouched by a revision.
//   - Snapshots do not carry report schedules (snapshot.go): a
//     revision shares the subscription's fate — both are
//     process-lifetime state, recreated after a restore.
//   - There is no HTTP endpoint for the revision (along LG-241/242
//     and the transfer-schedule amend family): subscription tuning
//     is in-process structural config.
//
// Orthogonality: LG-72 owns the subscription mechanism itself;
// LG-308 owns the patch shape (this module's precedent, different
// object); LG-41 and the AmendTransferSchedule* family own transfer
// schedules (same revision pattern, different object); LG-316 owns
// the sweeper's own poll interval (a different cadence — the
// sweeper interval decides how often the table is scanned, a
// subscription's Interval decides how often it fires).

/*
	Note: the file ledger/report_schedule.go owns the subscription
	lifecycle; this file owns only the revision entry points, so the
	two stay independently reviewable.
*/

// ReportScheduleRevision is a partial revision of one report
// subscription. A nil field keeps the live value; a non-nil field
// replaces it. All fields nil is a no-op. A non-nil zero EndsAt
// clears the end time (the subscription never ends). Kind, ID,
// Status, RunSeq, CreatedAt, and IdempotencyKey are deliberately
// absent: they are not revisable.
type ReportScheduleRevision struct {
	// Interval replaces the fixed cadence between runs. It must be
	// positive; it never moves NextRunAt — the new cadence applies
	// from the next advance.
	Interval *time.Duration
	// NextRunAt replaces the nominal due time of the next run. A
	// past value fires on the next sweep.
	NextRunAt *time.Time
	// EndsAt replaces the optional end time. A non-nil zero value
	// clears it (no end); any other value must fall after the
	// effective NextRunAt.
	EndsAt *time.Time
	// Params replaces the kind-specific inputs wholesale, validated
	// against the subscription's Kind.
	Params *ReportParams
	// Sink replaces the delivery target wholesale. A URL sink
	// requires a signing secret; the secret never enters the audit
	// trail.
	Sink *ReportSink
}

// mergeReportScheduleRevision overlays a partial revision on the live
// subscription: nil fields keep the live value. Params and Sink are
// replaced wholesale (value copies, so the installed subscription
// never aliases the caller's values).
func mergeReportScheduleRevision(live ReportSchedule, patch ReportScheduleRevision) ReportSchedule {
	merged := live
	if patch.Interval != nil {
		merged.Interval = *patch.Interval
	}
	if patch.NextRunAt != nil {
		merged.NextRunAt = patch.NextRunAt.UTC()
	}
	if patch.EndsAt != nil {
		merged.EndsAt = patch.EndsAt.UTC()
	}
	if patch.Params != nil {
		merged.Params = *patch.Params
	}
	if patch.Sink != nil {
		merged.Sink = *patch.Sink
	}
	return merged
}

// reportScheduleRevisionEqual reports whether two subscriptions agree
// on every revisable field. ID, Kind, Status, RunSeq, CreatedAt, and
// IdempotencyKey are never revised and are deliberately excluded —
// only the fields a patch can move decide whether a revision is a
// no-op.
func reportScheduleRevisionEqual(a, b ReportSchedule) bool {
	return a.Interval == b.Interval &&
		a.NextRunAt.Equal(b.NextRunAt) &&
		a.EndsAt.Equal(b.EndsAt) &&
		a.Params == b.Params &&
		a.Sink == b.Sink
}

// formatReportScheduleTime renders a schedule time for the audit
// trail: RFC3339, or "" when the time is zero (no end time).
func formatReportScheduleTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// reportParamsSummary renders the kind-specific params as a short
// audit summary. It carries values, never secrets — ReportParams has
// no secret field.
func reportParamsSummary(kind ReportKind, p ReportParams) string {
	switch kind {
	case ReportKindStatement:
		return fmt.Sprintf("account=%s from=%s to=%s", p.Account, p.From, p.To)
	default:
		return fmt.Sprintf("base_currency=%s", p.BaseCurrency)
	}
}

// ReviseReportSchedule revises one report subscription's fields in
// place and returns the revised subscription. See
// ReviseReportScheduleCtx.
func (l *Ledger) ReviseReportSchedule(id string, patch ReportScheduleRevision) (ReportSchedule, error) {
	return l.ReviseReportScheduleCtx(context.Background(), id, patch)
}

// ReviseReportScheduleCtx is ReviseReportSchedule with per-call audit
// actor attribution (LG-73). The patch merges over the live
// subscription and the merged record is re-validated with the
// creation gates (validateReportScheduleRecord); an illegal merge
// returns the corresponding ErrReportSchedule* sentinel with the live
// subscription untouched. An unknown ID returns
// ErrReportScheduleNotFound; a cancelled or completed subscription
// returns ErrReportScheduleBadTransition; a paused subscription is
// revisable. An all-nil patch, and a revision identical to the live
// subscription, are no-ops that return the live subscription with a
// nil error and no audit event. A real revision installs the merged
// fields in place — identity, status, RunSeq, CreatedAt, run history,
// and IdempotencyKey are preserved — and emits one
// report_schedule_revised audit event (schedule_id, kind, and
// previous_*/current_* per changed field: interval as a duration
// string, next_run_at / ends_at as RFC3339, the kind params summary,
// and the sink name). The revision bumps neither the version nor the
// audit chain, writes no journal entry, and emits no outbox event.
func (l *Ledger) ReviseReportScheduleCtx(ctx context.Context, id string, patch ReportScheduleRevision) (ReportSchedule, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	sched, ok := l.reportSchedules[id]
	if !ok {
		return ReportSchedule{}, ErrReportScheduleNotFound
	}
	if sched.Status == ScheduleCancelled || sched.Status == ScheduleCompleted {
		return ReportSchedule{}, fmt.Errorf("%w: cannot revise a %s schedule", ErrReportScheduleBadTransition, sched.Status)
	}
	previous := *sched
	merged := mergeReportScheduleRevision(previous, patch)
	if err := validateReportScheduleRecord(&merged); err != nil {
		return ReportSchedule{}, err
	}
	if reportScheduleRevisionEqual(previous, merged) {
		return previous, nil
	}
	*sched = merged
	details := map[string]any{
		"schedule_id": sched.ID,
		"kind":        string(sched.Kind),
	}
	if merged.Interval != previous.Interval {
		details["previous_interval"] = previous.Interval.String()
		details["current_interval"] = merged.Interval.String()
	}
	if !previous.NextRunAt.Equal(merged.NextRunAt) {
		details["previous_next_run_at"] = formatReportScheduleTime(previous.NextRunAt)
		details["current_next_run_at"] = formatReportScheduleTime(merged.NextRunAt)
	}
	if !previous.EndsAt.Equal(merged.EndsAt) {
		details["previous_ends_at"] = formatReportScheduleTime(previous.EndsAt)
		details["current_ends_at"] = formatReportScheduleTime(merged.EndsAt)
	}
	if merged.Params != previous.Params {
		details["previous_params"] = reportParamsSummary(previous.Kind, previous.Params)
		details["current_params"] = reportParamsSummary(merged.Kind, merged.Params)
	}
	if merged.Sink != previous.Sink {
		details["previous_sink"] = previous.Sink.sinkName()
		details["current_sink"] = merged.Sink.sinkName()
	}
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "report_schedule_revised",
		Actor:         "ReviseReportSchedule",
		TraceID:       sched.ID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details:       details,
	})
	return *sched, nil
}
