package ledger

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Settlement cutoff windows (LG-101, payments / fintech).
//
// Real-time ledgers book money the moment a transfer commits, but the
// payment rails behind them settle on windows: a per-currency cutoff
// time ("17:00" in a named timezone, default UTC) after which a
// transfer misses the day's settlement run and waits for the next
// window. The booking is never blocked by the cutoff — the journal
// commits exactly as before — but the transfer is flagged as deferred
// and tracked through a small settlement state machine:
//
//	scheduled -> pending -> settled
//
// scheduled: the transfer posted after its currency's cutoff; it waits
// for the next window. pending: the window opened (see
// AdvanceSettlementWindows) and the transfer is in the current
// settlement run. settled: the operator confirmed the window's
// settlement (see SettleDeferredTransfer). Transitions are strict — a
// scheduled deferral cannot jump straight to settled — mirroring the
// review lifecycle's from/to enforcement (see review.go).
//
// The cutoff is evaluated on the transfer's submission time (the
// journaled CreatedAt): strictly after the cutoff instant means the
// window is missed. The cutoff instant itself still belongs to the
// current window. Currencies with no configured cutoff behave exactly
// as before — zero behavior change.
//
// Windows only land on business days: the LG-95 holiday calendar
// (weekend + custom holiday table) is consulted on the cutoff date, so
// a transfer posted on a Friday evening waits for Monday's window,
// and a configured holiday pushes the window past it. The calendar is
// UTC-based (see businessday.go); the cutoff clock is per-currency
// wall time in the configured timezone.
//
// Deferral bookkeeping is settlement metadata, not accounting: creating
// a deferral, advancing it, or settling it never bumps the journal
// version, but every transition is a hash-chained audit event (the
// structured audit log is the durable record) and the deferral count
// is an operational metric (see ledger_settlement_deferred_total).

// Settlement cutoff errors.
var (
	// ErrInvalidSettlementCutoff is returned when a cutoff time is not a
	// strict "HH:MM" wall-clock time (00:00-23:59).
	ErrInvalidSettlementCutoff = errors.New("ledger: settlement cutoff must be \"HH:MM\" (00:00-23:59)")
	// ErrInvalidSettlementTimezone is returned when the cutoff's
	// timezone is not a loadable IANA name.
	ErrInvalidSettlementTimezone = errors.New("ledger: settlement cutoff timezone must be a valid IANA name")
	// ErrNoSettlementCutoff is returned when a currency has no cutoff
	// configured (see NextSettlementWindow).
	ErrNoSettlementCutoff = errors.New("ledger: no settlement cutoff configured for currency")
	// ErrDeferralNotFound is returned when no settlement deferral exists
	// for the transfer.
	ErrDeferralNotFound = errors.New("ledger: no settlement deferral for transfer")
	// ErrDeferralBadTransition is returned when a deferral state
	// transition is not allowed (only scheduled->pending->settled).
	ErrDeferralBadTransition = errors.New("ledger: settlement deferral status transition not allowed")
)

// SettlementDeferralStatus is the lifecycle state of one deferred
// transfer's settlement: scheduled (waiting for the next window) ->
// pending (window opened) -> settled (operator confirmed).
type SettlementDeferralStatus string

const (
	// DeferralScheduled marks a transfer booked after its currency's
	// cutoff, waiting for the next settlement window.
	DeferralScheduled SettlementDeferralStatus = "scheduled"
	// DeferralPending marks a deferral whose window has opened: the
	// transfer is in the current settlement run.
	DeferralPending SettlementDeferralStatus = "pending"
	// DeferralSettled marks a deferral the operator confirmed settled.
	DeferralSettled SettlementDeferralStatus = "settled"
)

// SettlementCutoff is one currency's settlement cutoff: the wall-clock
// time (HH:MM) in Timezone after which same-day settlement is missed.
// An empty Timezone means UTC.
type SettlementCutoff struct {
	Currency string `json:"currency"`
	// Cutoff is the wall-clock cutoff "HH:MM" in Timezone.
	Cutoff string `json:"cutoff"`
	// Timezone is the IANA timezone name the cutoff clock runs in.
	// Empty means UTC.
	Timezone string `json:"timezone"`

	hour     int
	minute   int
	location *time.Location
}

// locationOf returns the cutoff's timezone, defaulting to UTC. The
// location is resolved once at configuration time, so the hot path
// never touches the timezone database.
func (c SettlementCutoff) locationOf() *time.Location {
	if c.location == nil {
		return time.UTC
	}
	return c.location
}

// ParseSettlementCutoff validates a per-currency settlement cutoff: the
// currency must be a 3-letter ISO 4217 code, the cutoff a strict
// "HH:MM" wall-clock time, and the timezone a loadable IANA name (empty
// means UTC, like the cutoff default). It returns the validated cutoff
// or an error — the non-panicking constructor for operator-supplied
// config, mirroring ParseFeeSchedule.
func ParseSettlementCutoff(currency, hhmm, timezone string) (SettlementCutoff, error) {
	cur, err := normalizeCurrency(currency)
	if err != nil {
		return SettlementCutoff{}, err
	}
	hour, minute, err := parseCutoffTime(hhmm)
	if err != nil {
		return SettlementCutoff{}, err
	}
	tz := strings.TrimSpace(timezone)
	loc := time.UTC
	if tz != "" {
		loc, err = time.LoadLocation(tz)
		if err != nil {
			return SettlementCutoff{}, fmt.Errorf("%w: %q", ErrInvalidSettlementTimezone, timezone)
		}
		tz = loc.String()
	} else {
		// The empty timezone is the UTC default, recorded explicitly so
		// the configured value is always a concrete zone name.
		tz = "UTC"
	}
	return SettlementCutoff{
		Currency: cur,
		Cutoff:   fmt.Sprintf("%02d:%02d", hour, minute),
		Timezone: tz,
		hour:     hour,
		minute:   minute,
		location: loc,
	}, nil
}

// parseCutoffTime parses a strict "HH:MM" wall-clock time. "9:5" and
// "24:00" are rejected — the two-digit form is the canonical contract,
// like the settlement day's "2006-01-02".
func parseCutoffTime(hhmm string) (hour, minute int, err error) {
	parts := strings.Split(hhmm, ":")
	if len(parts) != 2 || len(parts[0]) != 2 || len(parts[1]) != 2 {
		return 0, 0, fmt.Errorf("%w: %q", ErrInvalidSettlementCutoff, hhmm)
	}
	hour, err = strconv.Atoi(parts[0])
	if err != nil || hour < 0 || hour > 23 {
		return 0, 0, fmt.Errorf("%w: %q", ErrInvalidSettlementCutoff, hhmm)
	}
	minute, err = strconv.Atoi(parts[1])
	if err != nil || minute < 0 || minute > 59 {
		return 0, 0, fmt.Errorf("%w: %q", ErrInvalidSettlementCutoff, hhmm)
	}
	return hour, minute, nil
}

// WithSettlementCutoff installs a per-currency settlement cutoff at
// construction (see SetSettlementCutoff). Invalid input panics —
// fail-fast at construction, like the fee schedule and the business-day
// calendar.
func WithSettlementCutoff(currency, hhmm, timezone string) Option {
	cutoff, err := ParseSettlementCutoff(currency, hhmm, timezone)
	if err != nil {
		panic(fmt.Sprintf("ledger: %v", err))
	}
	return func(l *Ledger) {
		l.settlementCutoffs[cutoff.Currency] = cutoff
	}
}

// SetSettlementCutoff configures (or replaces) one currency's settlement
// cutoff at runtime. The change is structural config — audited (op
// "settlement_cutoff_set"), no journal version bump — and survives
// disaster-recovery snapshots.
func (l *Ledger) SetSettlementCutoff(currency, hhmm, timezone string) error {
	cutoff, err := ParseSettlementCutoff(currency, hhmm, timezone)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.settlementCutoffs[cutoff.Currency] = cutoff
	l.emitAudit(AuditEvent{
		Op:            "settlement_cutoff_set",
		Actor:         "SetSettlementCutoff",
		TraceID:       "settlement-cutoff:" + cutoff.Currency,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"currency": cutoff.Currency,
			"cutoff":   cutoff.Cutoff,
			"timezone": cutoff.Timezone,
		},
	})
	return nil
}

// ClearSettlementCutoff removes a currency's settlement cutoff:
// transfers in that currency stop being evaluated against a window and
// behave exactly as before any cutoff existed. Clearing a currency with
// no cutoff is a no-op success, like removing an absent holiday. The
// change is audited (op "settlement_cutoff_cleared").
func (l *Ledger) ClearSettlementCutoff(currency string) error {
	cur, err := normalizeCurrency(currency)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.settlementCutoffs[cur]; !ok {
		return nil
	}
	delete(l.settlementCutoffs, cur)
	l.emitAudit(AuditEvent{
		Op:            "settlement_cutoff_cleared",
		Actor:         "ClearSettlementCutoff",
		TraceID:       "settlement-cutoff:" + cur,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details:       map[string]any{"currency": cur},
	})
	return nil
}

// SettlementCutoff reports the configured cutoff for a currency and
// whether one is configured.
func (l *Ledger) SettlementCutoff(currency string) (SettlementCutoff, bool) {
	cur, err := normalizeCurrency(currency)
	if err != nil {
		return SettlementCutoff{}, false
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	c, ok := l.settlementCutoffs[cur]
	return c, ok
}

// SettlementCutoffs lists every configured per-currency cutoff, sorted
// by currency.
func (l *Ledger) SettlementCutoffs() []SettlementCutoff {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.settlementCutoffsLocked()
}

// settlementCutoffsLocked lists the configured cutoffs, sorted by
// currency. Callers must hold l.mu.
func (l *Ledger) settlementCutoffsLocked() []SettlementCutoff {
	out := make([]SettlementCutoff, 0, len(l.settlementCutoffs))
	for _, c := range l.settlementCutoffs {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Currency < out[j].Currency })
	return out
}

// SettlementWindow is the answer to the operator's "when does this
// currency settle next" question: the next settlement window at or after
// the query time.
type SettlementWindow struct {
	Currency string `json:"currency"`
	// Window is the next settlement window's instant, in Timezone.
	Window time.Time `json:"next_window"`
	// Timezone is the IANA timezone the cutoff clock runs in ("UTC"
	// when the cutoff was configured without one).
	Timezone string `json:"timezone"`
	// Cutoff echoes the configured "HH:MM" cutoff, so the response is
	// self-describing about which wall-clock time the window closes.
	Cutoff string `json:"cutoff"`
	// IsToday reports whether the window is today's (still-open) cutoff
	// rather than a future day's: an operator deciding whether to rush a
	// payment reads this, not the timestamp.
	IsToday bool `json:"is_today"`
}

// NextSettlementWindow returns the next settlement window for a currency
// at or after now: today's cutoff when it is a business day and still
// open, otherwise the next business day's cutoff (the LG-95 holiday
// calendar skips weekends and configured holidays). A currency with no
// configured cutoff returns ErrNoSettlementCutoff. Read-only.
func (l *Ledger) NextSettlementWindow(currency string, now time.Time) (SettlementWindow, error) {
	cur, err := normalizeCurrency(currency)
	if err != nil {
		return SettlementWindow{}, err
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	cutoff, ok := l.settlementCutoffs[cur]
	if !ok {
		return SettlementWindow{}, fmt.Errorf("%w: %s", ErrNoSettlementCutoff, cur)
	}
	return l.nextSettlementWindowLocked(cutoff, now), nil
}

// nextSettlementWindowLocked computes the next window for a cutoff at or
// after now. The business-day check runs on the cutoff date as a UTC
// calendar date — the LG-95 calendar is UTC-based by design — while the
// cutoff clock is wall time in the currency's timezone. Callers must
// hold l.mu (either lock suffices); the cutoff table and calendar are
// read-only here.
func (l *Ledger) nextSettlementWindowLocked(cutoff SettlementCutoff, now time.Time) SettlementWindow {
	loc := cutoff.locationOf()
	local := now.In(loc)
	todayCutoff := time.Date(local.Year(), local.Month(), local.Day(), cutoff.hour, cutoff.minute, 0, 0, loc)
	// The business-day unit is the cutoff's local calendar date,
	// evaluated as a UTC date against the LG-95 calendar.
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
	if l.businessCalendar.IsBusinessDay(today) && local.Before(todayCutoff) {
		return SettlementWindow{
			Currency: cutoff.Currency,
			Window:   todayCutoff,
			Timezone: timezoneName(loc),
			Cutoff:   cutoff.Cutoff,
			IsToday:  true,
		}
	}
	date := today
	for {
		date = date.AddDate(0, 0, 1)
		if l.businessCalendar.IsBusinessDay(date) {
			break
		}
	}
	return SettlementWindow{
		Currency: cutoff.Currency,
		Window:   time.Date(date.Year(), date.Month(), date.Day(), cutoff.hour, cutoff.minute, 0, 0, loc),
		Timezone: timezoneName(loc),
		Cutoff:   cutoff.Cutoff,
		IsToday:  false,
	}
}

// timezoneName renders a cutoff's timezone for operator-facing output:
// "UTC" when the cutoff was configured without one (the location is
// time.UTC then).
func timezoneName(loc *time.Location) string {
	if loc == time.UTC {
		return "UTC"
	}
	return loc.String()
}

// SettlementDeferral is one transfer's settlement-window bookkeeping: a
// transfer booked after its currency's cutoff waits here for the next
// window instead of settling in the current one.
type SettlementDeferral struct {
	TransferID string `json:"transfer_id"`
	Currency   string `json:"currency"`
	// SubmittedAt is the transfer's submission time (its journaled
	// CreatedAt): the instant the cutoff was evaluated against.
	SubmittedAt time.Time `json:"submitted_at"`
	// Window is the settlement window the transfer waits for, in
	// Timezone.
	Window   time.Time                `json:"window"`
	Timezone string                   `json:"timezone"`
	Status   SettlementDeferralStatus `json:"status"`
	// UpdatedAt is the last transition time.
	UpdatedAt time.Time `json:"updated_at"`
}

// deferSettlementIfPastCutoffLocked evaluates a just-committed transfer
// against its currency's settlement cutoff and, when the submission
// falls after the cutoff, records a scheduled deferral for the next
// window: the defer audit event (op "transfer_settlement_deferred") and
// the deferred counter. It returns the transfer's settlement status —
// "settled" (booked in the current window) or "scheduled" (waiting for
// the next window) — and the window it settles in. A currency with no
// configured cutoff returns "" and the zero time: zero behavior change,
// and the receipt discloses nothing.
//
// The booking itself is never blocked: this runs after the atomic
// commit, strictly after the "transfer" audit event, so the journal is
// already durable when the settlement flag is evaluated. Callers must
// hold the write lock; submittedAt must be the transfer's journaled
// CreatedAt (never zero at the commit paths).
func (l *Ledger) deferSettlementIfPastCutoffLocked(currency string, submittedAt time.Time, transferID string, entryIDs []string, accounts []AccountID) (status string, window time.Time) {
	cutoff, ok := l.settlementCutoffs[currency]
	if !ok {
		return "", time.Time{}
	}
	if submittedAt.IsZero() {
		submittedAt = time.Now()
	}
	loc := cutoff.locationOf()
	local := submittedAt.In(loc)
	todayCutoff := time.Date(local.Year(), local.Month(), local.Day(), cutoff.hour, cutoff.minute, 0, 0, loc)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
	// The cutoff instant itself still belongs to the current window:
	// only a strictly later submission misses it.
	if l.businessCalendar.IsBusinessDay(today) && !local.After(todayCutoff) {
		return "settled", todayCutoff
	}
	w := l.nextSettlementWindowLocked(cutoff, submittedAt)
	if _, exists := l.settlementDeferrals[transferID]; !exists {
		l.settlementDeferrals[transferID] = &SettlementDeferral{
			TransferID:  transferID,
			Currency:    currency,
			SubmittedAt: submittedAt,
			Window:      w.Window,
			Timezone:    w.Timezone,
			Status:      DeferralScheduled,
			UpdatedAt:   submittedAt,
		}
	}
	l.settlementDeferred.Add(1)
	l.emitAudit(AuditEvent{
		Op:            "transfer_settlement_deferred",
		Actor:         "PostTransfer",
		TraceID:       transferID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		EntryIDs:      entryIDs,
		Accounts:      accounts,
		Details: map[string]any{
			"reason":       "settlement_cutoff",
			"currency":     currency,
			"cutoff":       cutoff.Cutoff,
			"timezone":     w.Timezone,
			"submitted_at": submittedAt.Format(time.RFC3339),
			"window":       w.Window.Format(time.RFC3339),
			"status":       string(DeferralScheduled),
		},
	})
	return "scheduled", w.Window
}

// DeferredSettlements lists settlement deferrals, optionally filtered by
// status, sorted by (window, transfer ID). No filter lists everything,
// including settled history.
func (l *Ledger) DeferredSettlements(statuses ...SettlementDeferralStatus) []SettlementDeferral {
	l.mu.RLock()
	defer l.mu.RUnlock()
	want := map[SettlementDeferralStatus]bool{}
	for _, s := range statuses {
		want[s] = true
	}
	out := make([]SettlementDeferral, 0, len(l.settlementDeferrals))
	for _, d := range l.settlementDeferrals {
		if len(want) > 0 && !want[d.Status] {
			continue
		}
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Window.Equal(out[j].Window) {
			return out[i].Window.Before(out[j].Window)
		}
		return out[i].TransferID < out[j].TransferID
	})
	return out
}

// AdvanceSettlementWindows moves every scheduled deferral whose window
// has opened (window <= now) to pending: the transfer is now in the
// current settlement run. It returns the advanced transfer IDs, sorted.
// Advancing is idempotent — deferrals already pending or settled, or
// whose window is still in the future, are untouched. Each transition is
// audited (op "settlement_deferral_advanced"); the journal version is
// not bumped — this is settlement bookkeeping, not accounting.
func (l *Ledger) AdvanceSettlementWindows(now time.Time) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var advanced []string
	for id, d := range l.settlementDeferrals {
		if d.Status != DeferralScheduled || d.Window.After(now) {
			continue
		}
		d.Status = DeferralPending
		d.UpdatedAt = now
		advanced = append(advanced, id)
		l.emitAudit(AuditEvent{
			Op:            "settlement_deferral_advanced",
			Actor:         "AdvanceSettlementWindows",
			TraceID:       id,
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Details: map[string]any{
				"from":   string(DeferralScheduled),
				"to":     string(DeferralPending),
				"window": d.Window.Format(time.RFC3339),
			},
		})
	}
	sort.Strings(advanced)
	return advanced
}

// SettleDeferredTransfer marks a pending deferral settled: the operator's
// confirmation that the window's settlement run processed the transfer.
// Only pending deferrals settle — a scheduled one must be advanced
// first — and the transition is audited (op
// "settlement_deferral_settled") with the operator's identity. The
// journal version is not bumped.
func (l *Ledger) SettleDeferredTransfer(transferID, actor string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	d, ok := l.settlementDeferrals[transferID]
	if !ok {
		return fmt.Errorf("%w: %q", ErrDeferralNotFound, transferID)
	}
	if d.Status != DeferralPending {
		return fmt.Errorf("%w: cannot move a %s deferral to settled", ErrDeferralBadTransition, d.Status)
	}
	if actor == "" {
		actor = "operator"
	}
	now := time.Now()
	d.Status = DeferralSettled
	d.UpdatedAt = now
	l.emitAudit(AuditEvent{
		Op:            "settlement_deferral_settled",
		Actor:         actor,
		TraceID:       transferID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"from":   string(DeferralPending),
			"to":     string(DeferralSettled),
			"window": d.Window.Format(time.RFC3339),
		},
	})
	return nil
}

// SettlementDeferredCount reports how many transfers have been deferred
// to the next settlement window since the ledger started. The counter
// resets on restart — the audit log (op
// "transfer_settlement_deferred") is the durable record.
func (l *Ledger) SettlementDeferredCount() uint64 {
	return l.settlementDeferred.Load()
}

// settlementCutoffsSnapshotLocked returns the configured cutoffs for the
// snapshot config row, sorted by currency. Callers must hold l.mu; the
// read lock suffices.
func (l *Ledger) settlementCutoffsSnapshotLocked() []SettlementCutoff {
	return l.settlementCutoffsLocked()
}

// settlementDeferralsSnapshotLocked returns the in-flight deferrals for
// the snapshot row, sorted by (window, transfer ID). Settled history
// rides along — a restore keeps the full settlement trail. Callers must
// hold l.mu; the read lock suffices.
func (l *Ledger) settlementDeferralsSnapshotLocked() []SettlementDeferral {
	out := make([]SettlementDeferral, 0, len(l.settlementDeferrals))
	for _, d := range l.settlementDeferrals {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Window.Equal(out[j].Window) {
			return out[i].Window.Before(out[j].Window)
		}
		return out[i].TransferID < out[j].TransferID
	})
	return out
}

// deferredSettlementsLocked lists every deferral, sorted by (window,
// transfer ID), for the reconcile report. Callers must hold l.mu; the
// read lock suffices.
func (l *Ledger) deferredSettlementsLocked() []SettlementDeferral {
	return l.settlementDeferralsSnapshotLocked()
}

// validateSettlementCutoffSnapshot checks one imported cutoff row: a
// valid currency, a strict "HH:MM", and a loadable timezone. It returns
// the normalized cutoff (location resolved) for the restore to install.
func validateSettlementCutoffSnapshot(c SettlementCutoff) (SettlementCutoff, error) {
	return ParseSettlementCutoff(c.Currency, c.Cutoff, c.Timezone)
}

// validateSettlementDeferralSnapshot checks one imported deferral row:
// a known status and a non-empty transfer ID. The window and submitted
// times ride verbatim — a restore keeps the exact settlement trail.
func validateSettlementDeferralSnapshot(d SettlementDeferral) error {
	if d.TransferID == "" {
		return fmt.Errorf("settlement deferral with empty transfer id")
	}
	switch d.Status {
	case DeferralScheduled, DeferralPending, DeferralSettled:
	default:
		return fmt.Errorf("settlement deferral %q has bad status %q", d.TransferID, d.Status)
	}
	return nil
}
