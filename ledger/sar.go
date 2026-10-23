package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"
)

// SAR (Suspicious Activity Report) draft generation (LG-188).
//
// The large-transaction (CTR) machinery in ctr.go answers "did the money
// cross an amount threshold". SAR answers the complementary question:
// "does the *behavior* look suspicious". Where ctr.go is a pure amount
// trigger, sar.go assembles a report draft from the ledger's existing
// behavioral risk signals plus two journal-pattern detectors:
//
//   - velocity.go: an account's rolling-window usage reaching the warning
//     band of a configured velocity limit (VELOCITY_ANOMALY).
//   - denylist.go: the subject account currently matching a denylist
//     entry (DENYLIST_MATCH).
//   - ctr.go: the account's UTC-day inflow/outflow reaching a configured
//     large-transaction threshold (LARGE_TX_FLOW) — computed from the
//     journal here, without generating CTR reports as a side effect.
//   - structuring detector: a cluster of sub-threshold amounts just
//     below a large-tx threshold inside one UTC day (STRUCTURING) — the
//     classic placement pattern of staying under the reporting line.
//   - rapid-movement detector: funds arriving then leaving within a
//     short window (RAPID_MOVEMENT) — the layering pattern.
//
// Report lifecycle: DraftSAR assembles a draft (status "draft") from the
// signals; FileSAR moves it to "filed". Filed reports are immutable —
// there is no update path, and filing twice is rejected. Filing emits a
// sar_filed audit event carrying the report's content hash, so the filed
// report is tamper-evident through the audit log's hash chain
// (audit.go). Drafts are working documents and are not audited.
//
// The reason vocabulary is a fixed GoAML-compatible code list
// (report_code / reason_code fields in goAML STR submissions).
// Drafting books nothing, bumps no version, and touches no balances —
// it is read-only against the journal, like Reconcile.

// SARReasonCode is one GoAML-compatible suspicious-activity reason code.
type SARReasonCode string

const (
	// SARReasonStructuring: sub-threshold amounts clustered just below a
	// large-transaction reporting threshold (placement pattern).
	SARReasonStructuring SARReasonCode = "STRUCTURING"
	// SARReasonVelocityAnomaly: rolling-window usage in the warning band
	// of a configured velocity limit (velocity.go).
	SARReasonVelocityAnomaly SARReasonCode = "VELOCITY_ANOMALY"
	// SARReasonDenylistMatch: the subject matches a denylist entry
	// (denylist.go).
	SARReasonDenylistMatch SARReasonCode = "DENYLIST_MATCH"
	// SARReasonLargeTxFlow: UTC-day inflow/outflow at/above a configured
	// large-transaction threshold (ctr.go).
	SARReasonLargeTxFlow SARReasonCode = "LARGE_TX_FLOW"
	// SARReasonRapidMovement: funds in then out within a short window
	// (layering pattern).
	SARReasonRapidMovement SARReasonCode = "RAPID_MOVEMENT"
)

// sarReasonOrder is the canonical reason-code order in reports.
var sarReasonOrder = []SARReasonCode{
	SARReasonStructuring,
	SARReasonVelocityAnomaly,
	SARReasonDenylistMatch,
	SARReasonLargeTxFlow,
	SARReasonRapidMovement,
}

// SAR errors.
var (
	// ErrNoSuspiciousActivity is returned by DraftSAR when the subject
	// shows no risk signal in the window: a report needs at least one
	// sighting. 422-class.
	ErrNoSuspiciousActivity = errors.New("ledger: no suspicious activity for subject in window")
	// ErrSARNotFound is returned when a report ID is unknown. 404-class.
	ErrSARNotFound = errors.New("ledger: suspicious activity report not found")
	// ErrSARAlreadyFiled is returned by FileSAR on an already-filed
	// report: filed reports are immutable. 409-class.
	ErrSARAlreadyFiled = errors.New("ledger: suspicious activity report already filed")
	// ErrInvalidSARWindow is returned by DraftSAR for an empty,
	// inverted, or over-long window. 400-class.
	ErrInvalidSARWindow = errors.New("ledger: SAR window must be non-empty and at most 31 days")
	// ErrEmptySARSubject is returned by DraftSAR for an empty subject
	// account. 400-class.
	ErrEmptySARSubject = errors.New("ledger: SAR subject account must not be empty")
)

// MaxSARWindow caps the drafting window at 31 days: the detectors walk
// the subject's journal entries, and an unbounded window pins unbounded
// history. Longer lookbacks belong to repeated drafts, not one scan.
const MaxSARWindow = 31 * 24 * time.Hour

// DefaultSARStructuringRatio is the default fraction of a large-tx
// threshold at/above which a sub-threshold amount counts as a
// structuring candidate.
const DefaultSARStructuringRatio = 0.75

// DefaultSARStructuringMinCount is the default number of sub-threshold
// candidate amounts in one UTC day that raises a STRUCTURING sighting.
const DefaultSARStructuringMinCount = 3

// DefaultSARVelocityWarnRatio is the default fraction of a velocity
// limit at/above which usage raises a VELOCITY_ANOMALY sighting — the
// warning band before the limit itself rejects postings.
const DefaultSARVelocityWarnRatio = 0.8

// DefaultSARRapidMoveWindow is the default in-then-out window for the
// RAPID_MOVEMENT detector.
const DefaultSARRapidMoveWindow = 4 * time.Hour

// DefaultSARRapidMoveRatio is the default fraction of arrived funds that
// must leave inside the window to raise a RAPID_MOVEMENT sighting.
const DefaultSARRapidMoveRatio = 0.8

// sarConfig holds the SAR detector tuning. Zero value = defaults.
type sarConfig struct {
	reportingEntity string
	structuringRatio float64
	structuringMinCount int
	velocityWarnRatio float64
	rapidMoveWindow time.Duration
	rapidMoveRatio float64
}

func defaultSARConfig() sarConfig {
	return sarConfig{
		structuringRatio:   DefaultSARStructuringRatio,
		structuringMinCount: DefaultSARStructuringMinCount,
		velocityWarnRatio:  DefaultSARVelocityWarnRatio,
		rapidMoveWindow:    DefaultSARRapidMoveWindow,
		rapidMoveRatio:     DefaultSARRapidMoveRatio,
	}
}

// WithSARReportingEntity sets the reporting entity stamped on SAR
// drafts (the goAML reportingEntity / filing institution). Empty (the
// default) omits the field.
func WithSARReportingEntity(entity string) Option {
	return func(l *Ledger) {
		l.sarCfg.reportingEntity = entity
	}
}

// WithSARStructuring tunes the structuring detector: amounts at/above
// ratio*threshold but below threshold, minCount of them in one UTC day,
// raise STRUCTURING. ratio must be in (0, 1), minCount >= 2; anything
// else panics — fail-fast at construction, like WithLargeTxThresholds,
// so a misconfigured detector can never silently under-detect.
func WithSARStructuring(ratio float64, minCount int) Option {
	if !(ratio > 0 && ratio < 1) {
		panic(fmt.Sprintf("ledger: SAR structuring ratio must be in (0, 1), got %v", ratio))
	}
	if minCount < 2 {
		panic(fmt.Sprintf("ledger: SAR structuring minCount must be >= 2, got %d", minCount))
	}
	return func(l *Ledger) {
		l.sarCfg.structuringRatio = ratio
		l.sarCfg.structuringMinCount = minCount
	}
}

// WithSARVelocityWarnRatio sets the warning-band fraction of a velocity
// limit that raises VELOCITY_ANOMALY, in (0, 1). Panics outside the
// range, fail-fast like every other risk-control option.
func WithSARVelocityWarnRatio(ratio float64) Option {
	if !(ratio > 0 && ratio < 1) {
		panic(fmt.Sprintf("ledger: SAR velocity warn ratio must be in (0, 1), got %v", ratio))
	}
	return func(l *Ledger) {
		l.sarCfg.velocityWarnRatio = ratio
	}
}

// WithSARRapidMovement tunes the rapid-movement detector: outflow
// reaching ratio of the window's inflow within window raises
// RAPID_MOVEMENT. ratio in (0, 1], window > 0; anything else panics.
func WithSARRapidMovement(window time.Duration, ratio float64) Option {
	if window <= 0 {
		panic(fmt.Sprintf("ledger: SAR rapid-move window must be > 0, got %v", window))
	}
	if !(ratio > 0 && ratio <= 1) {
		panic(fmt.Sprintf("ledger: SAR rapid-move ratio must be in (0, 1], got %v", ratio))
	}
	return func(l *Ledger) {
		l.sarCfg.rapidMoveWindow = window
		l.sarCfg.rapidMoveRatio = ratio
	}
}

// SARSighting is one risk-signal observation feeding a report.
type SARSighting struct {
	// Reason is the GoAML-compatible reason code.
	Reason SARReasonCode `json:"reason"`
	// Currency is the sighting's currency ("" when currency-free, e.g.
	// denylist matches).
	Currency string `json:"currency,omitempty"`
	// At is when the signal was observed (the window end for
	// usage-style signals, the event time for entry-style ones).
	At time.Time `json:"at"`
	// Source names the originating signal: "velocity", "denylist",
	// "ctr", or "detector".
	Source string `json:"source"`
	// Details carries the signal's figures (limit vs usage, threshold
	// vs flow, matched entry label, ...).
	Details map[string]any `json:"details,omitempty"`
}

// SARSuspiciousTx is one journal entry cited as suspicious.
type SARSuspiciousTx struct {
	EntryID     string    `json:"entry_id"`
	AmountCents int64     `json:"amount_cents"`
	Currency    string    `json:"currency"`
	At          time.Time `json:"at"`
	// Direction is "inflow" (debit leg) or "outflow" (credit leg) from
	// the subject's side.
	Direction string `json:"direction"`
}

// SARStatus is a report's lifecycle state.
type SARStatus string

const (
	// SARDraft is a working report: assembled, not yet filed.
	SARDraft SARStatus = "draft"
	// SARFiled is a filed report: immutable and hash-sealed into the
	// audit chain.
	SARFiled SARStatus = "filed"
)

// SARSubject is the report's subject block (goAML report subject).
type SARSubject struct {
	Account AccountID `json:"account"`
	// KycStatus / RiskTier come from kyc.go when the subject has a
	// profile; empty when none is on file.
	KycStatus string `json:"kyc_status,omitempty"`
	RiskTier  string `json:"risk_tier,omitempty"`
}

// SARReport is one suspicious-activity report draft or filing.
type SARReport struct {
	ID string `json:"id"`
	// ReportingEntity is the filing institution (goAML reporting
	// entity); empty when unconfigured.
	ReportingEntity string     `json:"reporting_entity,omitempty"`
	Subject         SARSubject `json:"subject"`
	Status          SARStatus  `json:"status"`
	// ReasonCodes is the deduped GoAML reason vocabulary, in canonical
	// order.
	ReasonCodes []SARReasonCode `json:"reason_codes"`
	Sightings   []SARSighting  `json:"sightings"`
	// Transactions cites the journal entries behind the pattern
	// detectors (structuring cluster, rapid-movement legs). Empty when
	// the report rests on usage-style signals alone.
	Transactions []SARSuspiciousTx `json:"transactions,omitempty"`
	// Narrative is the generated human-readable summary.
	Narrative   string    `json:"narrative"`
	WindowStart time.Time `json:"window_start"`
	WindowEnd   time.Time `json:"window_end"`
	CreatedAt   time.Time `json:"created_at"`
	// FiledAt is set when the report moves to filed; nil on drafts.
	FiledAt *time.Time `json:"filed_at,omitempty"`
	// ContentHash is the hex SHA-256 seal of the filed report's
	// canonical JSON; empty on drafts. It is also carried on the
	// sar_filed audit event, sealing the filing into the audit chain.
	ContentHash string `json:"content_hash,omitempty"`
}

// cloneSAR deep-copies a report so callers can never mutate the stored
// original — the filed-immutability guarantee.
func cloneSAR(r *SARReport) *SARReport {
	if r == nil {
		return nil
	}
	out := *r
	out.ReasonCodes = append([]SARReasonCode(nil), r.ReasonCodes...)
	out.Sightings = append([]SARSighting(nil), r.Sightings...)
	for i := range out.Sightings {
		if r.Sightings[i].Details != nil {
			d := make(map[string]any, len(r.Sightings[i].Details))
			for k, v := range r.Sightings[i].Details {
				d[k] = v
			}
			out.Sightings[i].Details = d
		}
	}
	out.Transactions = append([]SARSuspiciousTx(nil), r.Transactions...)
	if r.FiledAt != nil {
		t := *r.FiledAt
		out.FiledAt = &t
	}
	return &out
}

// sarContentHash seals a filed report: SHA-256 over its canonical JSON.
func sarContentHash(r *SARReport) string {
	raw, err := json.Marshal(r)
	if err != nil {
		// The report is JSON-marshalable by construction (plain
		// struct); a marshal failure here would be a programming bug,
		// and failing open would seal a wrong hash. Panic, loudly.
		panic(fmt.Sprintf("ledger: SAR content hash marshal failed: %v", err))
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// DraftSAR assembles a suspicious-activity report draft for account over
// [since, until). It gathers the ledger's behavioral risk signals
// (velocity usage, denylist, large-tx threshold crossings) and runs the
// structuring and rapid-movement detectors over the subject's journal
// entries. At least one sighting is required — a report with no signal
// is ErrNoSuspiciousActivity. Drafting is read-only: it books nothing,
// bumps no version, and is not audited (drafts are working documents).
func (l *Ledger) DraftSAR(account AccountID, since, until time.Time) (*SARReport, error) {
	if account == "" {
		return nil, ErrEmptySARSubject
	}
	if since.IsZero() || until.IsZero() || !since.Before(until) || until.Sub(since) > MaxSARWindow {
		return nil, ErrInvalidSARWindow
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	var sightings []SARSighting
	var txs []SARSuspiciousTx

	// 1. Velocity usage in the warning band (velocity.go). The breach
	// itself rejects postings; the draft cites the approach — the
	// warning band before the limit fires.
	sightings = append(sightings, l.sarVelocitySightingsLocked(account, until)...)
	// 2. Denylist match (denylist.go): the subject itself is a listed
	// entity — the actionable compliance fact.
	sightings = append(sightings, l.sarDenylistSightingsLocked(account, until)...)

	// 3-5. Journal walk: per-day flows for the large-tx crossings,
	// structuring cluster, and rapid movement.
	entries := l.sarSubjectEntriesLocked(account, since, until)
	flows := sarDayFlows(entries, account)
	for _, f := range flows {
		threshold, ok := l.largeTxThresholds[f.currency]
		if !ok {
			continue
		}
		if f.inflow >= threshold {
			sightings = append(sightings, SARSighting{
				Reason:   SARReasonLargeTxFlow,
				Currency: f.currency,
				At:       f.dayEnd,
				Source:   "ctr",
				Details: map[string]any{
					"day":             f.day,
					"direction":       "inflow",
					"amount_cents":    f.inflow,
					"threshold_cents": threshold,
				},
			})
		}
		if f.outflow >= threshold {
			sightings = append(sightings, SARSighting{
				Reason:   SARReasonLargeTxFlow,
				Currency: f.currency,
				At:       f.dayEnd,
				Source:   "ctr",
				Details: map[string]any{
					"day":             f.day,
					"direction":       "outflow",
					"amount_cents":    f.outflow,
					"threshold_cents": threshold,
				},
			})
		}
		// Structuring: sub-threshold amounts clustered just below the
		// threshold inside one UTC day. The comparison never adds, so
		// it cannot overflow: amount < threshold is checked first,
		// then amount >= floor via threshold-floor without adding.
		floor := int64(float64(threshold) * l.sarCfg.structuringRatio)
		var cluster []JournalEntry
		for _, e := range f.entries {
			if e.AmountCents < threshold && e.AmountCents >= floor {
				cluster = append(cluster, e)
			}
		}
		if len(cluster) >= l.sarCfg.structuringMinCount {
			sort.Slice(cluster, func(i, j int) bool { return cluster[i].CreatedAt.Before(cluster[j].CreatedAt) })
			sightings = append(sightings, SARSighting{
				Reason:   SARReasonStructuring,
				Currency: f.currency,
				At:       f.dayEnd,
				Source:   "detector",
				Details: map[string]any{
					"day":               f.day,
					"cluster_count":     len(cluster),
					"threshold_cents":   threshold,
					"floor_cents":       floor,
					"cluster_cents":     sarSumCapped(cluster),
				},
			})
			for _, e := range cluster {
				txs = append(txs, sarTx(e, account))
			}
		}
	}
	// Rapid movement: funds in then out within the window (layering).
	if sighting, legs := l.sarRapidMovementLocked(entries, account); sighting != nil {
		sightings = append(sightings, *sighting)
		txs = append(txs, legs...)
	}

	if len(sightings) == 0 {
		return nil, ErrNoSuspiciousActivity
	}
	sort.Slice(sightings, func(i, j int) bool {
		if sightings[i].Reason != sightings[j].Reason {
			return sarReasonRank(sightings[i].Reason) < sarReasonRank(sightings[j].Reason)
		}
		return sightings[i].At.Before(sightings[j].At)
	})

	subject := SARSubject{Account: account}
	if p, ok := l.kyc[account]; ok {
		subject.KycStatus = string(p.Status)
		subject.RiskTier = p.RiskTier
	}

	l.sarSeq++
	rep := &SARReport{
		ID:              fmt.Sprintf("SAR-%04d", l.sarSeq),
		ReportingEntity: l.sarCfg.reportingEntity,
		Subject:         subject,
		Status:          SARDraft,
		ReasonCodes:     sarDedupeReasons(sightings),
		Sightings:       sightings,
		Transactions:    txs,
		WindowStart:     since,
		WindowEnd:       until,
		CreatedAt:       time.Now(),
	}
	rep.Narrative = sarNarrative(rep)
	l.sarReports[rep.ID] = rep
	return cloneSAR(rep), nil
}

// sarVelocitySightingsLocked scans the subject's configured velocity
// limits and cites any whose rolling-window usage sits in the warning
// band (warnRatio of the limit) as of at. Callers must hold l.mu.
func (l *Ledger) sarVelocitySightingsLocked(account AccountID, at time.Time) []SARSighting {
	var out []SARSighting
	for key, lim := range l.velocityLimits {
		if key.account != account {
			continue
		}
		for _, dir := range []VelocityDirection{VelocityInflow, VelocityOutflow} {
			count, cents := l.velocityUsageLocked(key, dir, lim.Window, at)
			var dims []string
			if lim.MaxCount > 0 && count >= int64(float64(lim.MaxCount)*l.sarCfg.velocityWarnRatio) {
				dims = append(dims, "count")
			}
			if lim.MaxAmountCents > 0 && cents >= int64(float64(lim.MaxAmountCents)*l.sarCfg.velocityWarnRatio) {
				dims = append(dims, "amount")
			}
			if len(dims) == 0 {
				continue
			}
			out = append(out, SARSighting{
				Reason:   SARReasonVelocityAnomaly,
				Currency: key.currency,
				At:       at,
				Source:   "velocity",
				Details: map[string]any{
					"direction":        string(dir),
					"window":           lim.Window.String(),
					"dimensions":       dims,
					"window_count":     count,
					"max_count":        lim.MaxCount,
					"window_cents":     cents,
					"max_amount_cents": lim.MaxAmountCents,
				},
			})
		}
	}
	return out
}

// sarDenylistSightingsLocked cites the subject matching a denylist
// entry. Callers must hold l.mu.
func (l *Ledger) sarDenylistSightingsLocked(account AccountID, at time.Time) []SARSighting {
	entry, ok := l.denylistMatchLocked(account)
	if !ok {
		return nil
	}
	return []SARSighting{{
		Reason: SARReasonDenylistMatch,
		At:     at,
		Source: "denylist",
		Details: map[string]any{
			"matched_entry": entry.Label(),
		},
	}}
}

// sarSubjectEntriesLocked returns the subject's journal entries in
// [since, until), oldest first. Callers must hold l.mu.
func (l *Ledger) sarSubjectEntriesLocked(account AccountID, since, until time.Time) []JournalEntry {
	var out []JournalEntry
	for _, id := range l.byAccount[account] {
		e, ok := l.entries[id]
		if !ok {
			continue
		}
		if e.CreatedAt.Before(since) || !e.CreatedAt.Before(until) {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// sarDayFlow is one (day, currency) bucket of the subject's flows.
type sarDayFlow struct {
	day      string
	dayEnd   time.Time
	currency string
	inflow   int64
	outflow  int64
	entries  []JournalEntry
}

// sarDayFlows buckets entries by UTC day and currency. Sums saturate at
// math.MaxInt64 instead of overflowing: the draft must never wrap a
// flow total into a negative.
func sarDayFlows(entries []JournalEntry, account AccountID) []sarDayFlow {
	byKey := make(map[string]*sarDayFlow)
	for _, e := range entries {
		cur := e.Currency
		if cur == "" {
			cur = DefaultCurrency
		}
		day := e.CreatedAt.UTC().Format("2006-01-02")
		key := day + "\x00" + cur
		f, ok := byKey[key]
		if !ok {
			dayEnd, _ := time.Parse("2006-01-02", day)
			f = &sarDayFlow{day: day, dayEnd: dayEnd.Add(24 * time.Hour).Add(-time.Nanosecond), currency: cur}
			byKey[key] = f
		}
		f.entries = append(f.entries, e)
		if e.DebitAccount == account {
			f.inflow = sarAddCapped(f.inflow, e.AmountCents)
		}
		if e.CreditAccount == account {
			f.outflow = sarAddCapped(f.outflow, e.AmountCents)
		}
	}
	out := make([]sarDayFlow, 0, len(byKey))
	for _, f := range byKey {
		out = append(out, *f)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].day != out[j].day {
			return out[i].day < out[j].day
		}
		return out[i].currency < out[j].currency
	})
	return out
}

// sarRapidMovementLocked walks the subject's entries in time order and
// cites RAPID_MOVEMENT when outflow reaches rapidMoveRatio of the
// inflow seen so far within rapidMoveWindow of the first inflow — the
// layering pattern. Returns the sighting and the cited legs. Callers
// must hold l.mu (entries are already collected; this is pure).
func (l *Ledger) sarRapidMovementLocked(entries []JournalEntry, account AccountID) (*SARSighting, []SARSuspiciousTx) {
	var inflow, outflow int64
	var firstInflow time.Time
	var haveInflow bool
	var legs []SARSuspiciousTx
	for _, e := range entries {
		if e.DebitAccount == account {
			if !haveInflow {
				haveInflow = true
				firstInflow = e.CreatedAt
			}
			inflow = sarAddCapped(inflow, e.AmountCents)
		}
		if e.CreditAccount == account {
			outflow = sarAddCapped(outflow, e.AmountCents)
			legs = append(legs, sarTx(e, account))
		}
		// Compare without dividing (ratio could be fractional) and
		// without multiplying (could overflow): outflow >= ratio*inflow
		// ⟺ outflow/ratio >= inflow is unsafe; instead check the
		// window first (cheap), then use float only for the ratio
		// comparison — amounts are exact in float64 below 2^53.
		if haveInflow && e.CreatedAt.Sub(firstInflow) <= l.sarCfg.rapidMoveWindow &&
			float64(outflow) >= l.sarCfg.rapidMoveRatio*float64(inflow) && inflow > 0 {
			return &SARSighting{
				Reason:   SARReasonRapidMovement,
				Currency: e.Currency,
				At:       e.CreatedAt,
				Source:   "detector",
				Details: map[string]any{
					"window":            l.sarCfg.rapidMoveWindow.String(),
					"inflow_cents":      inflow,
					"outflow_cents":     outflow,
					"first_inflow_at":   firstInflow.UTC().Format(time.RFC3339),
					"triggered_at":      e.CreatedAt.UTC().Format(time.RFC3339),
				},
			}, legs
		}
	}
	return nil, nil
}

// sarTx cites one journal entry from the subject's side.
func sarTx(e JournalEntry, account AccountID) SARSuspiciousTx {
	dir := "inflow"
	if e.CreditAccount == account && e.DebitAccount != account {
		dir = "outflow"
	}
	return SARSuspiciousTx{
		EntryID:     e.ID,
		AmountCents: e.AmountCents,
		Currency:    e.Currency,
		At:          e.CreatedAt,
		Direction:   dir,
	}
}

// sarAddCapped adds b to a, saturating at math.MaxInt64.
func sarAddCapped(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// sarSumCapped sums entry amounts, saturating.
func sarSumCapped(entries []JournalEntry) int64 {
	var total int64
	for _, e := range entries {
		total = sarAddCapped(total, e.AmountCents)
	}
	return total
}

func sarReasonRank(r SARReasonCode) int {
	for i, c := range sarReasonOrder {
		if c == r {
			return i
		}
	}
	return len(sarReasonOrder)
}

func sarDedupeReasons(sightings []SARSighting) []SARReasonCode {
	seen := make(map[SARReasonCode]bool)
	var out []SARReasonCode
	for _, c := range sarReasonOrder {
		for _, s := range sightings {
			if s.Reason == c && !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
	}
	return out
}

// sarNarrative generates the report's human-readable summary.
func sarNarrative(rep *SARReport) string {
	return fmt.Sprintf(
		"Account %s: %d reason code(s) (%s) across %d sighting(s) in window %s to %s; %d transaction(s) cited.",
		rep.Subject.Account,
		len(rep.ReasonCodes),
		joinReasons(rep.ReasonCodes),
		len(rep.Sightings),
		rep.WindowStart.UTC().Format("2006-01-02"),
		rep.WindowEnd.UTC().Format("2006-01-02"),
		len(rep.Transactions),
	)
}

func joinReasons(codes []SARReasonCode) string {
	out := ""
	for i, c := range codes {
		if i > 0 {
			out += ", "
		}
		out += string(c)
	}
	return out
}

// FileSAR moves a draft to filed. Filed reports are immutable: filing
// twice is rejected, and there is no update path. Filing seals the
// report with a SHA-256 content hash and emits a sar_filed audit event
// carrying that hash, sealing the filing into the audit log's hash
// chain (tamper-evident). Like every mutating ledger operation, the
// caller-visible effect is atomic under the write lock.
func (l *Ledger) FileSAR(id string) (*SARReport, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	rep, ok := l.sarReports[id]
	if !ok {
		return nil, ErrSARNotFound
	}
	if rep.Status == SARFiled {
		return nil, ErrSARAlreadyFiled
	}
	now := time.Now()
	rep.Status = SARFiled
	rep.FiledAt = &now
	rep.ContentHash = sarContentHash(rep)
	l.emitAudit(AuditEvent{
		Op:       "sar_filed",
		Accounts: []AccountID{rep.Subject.Account},
		Details: map[string]any{
			"sar_id":       rep.ID,
			"subject":      string(rep.Subject.Account),
			"reason_codes": joinReasons(rep.ReasonCodes),
			"sightings":    len(rep.Sightings),
			"content_hash": rep.ContentHash,
		},
	})
	return cloneSAR(rep), nil
}

// GetSAR returns a copy of the report with id, or false when unknown.
func (l *Ledger) GetSAR(id string) (*SARReport, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	rep, ok := l.sarReports[id]
	if !ok {
		return nil, false
	}
	return cloneSAR(rep), true
}

// ListSARs returns copies of all reports, oldest first.
func (l *Ledger) ListSARs() []*SARReport {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]*SARReport, 0, len(l.sarReports))
	for _, rep := range l.sarReports {
		out = append(out, cloneSAR(rep))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// SARStats returns the draft/filed counts for observability.
func (l *Ledger) SARStats() (drafts, filed int) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, rep := range l.sarReports {
		if rep.Status == SARFiled {
			filed++
		} else {
			drafts++
		}
	}
	return drafts, filed
}
