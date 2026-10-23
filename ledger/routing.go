package ledger

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Smart payment routing (LG-187, fintech / backend engineering).
//
// RoutePayment answers "which settlement rail should this payment take?"
// for a payment of an amount in a currency with an optional deadline,
// under a routing strategy (cost-first, speed-first, or hard-deadline).
// It considers the six rails of the closed rail vocabulary (see sla.go):
// FedNow, RTP, SEPA, SWIFT, SWIFT gpi, and ACH — each with a capability
// profile of supported currencies, per-payment amount limits, expected
// settlement duration, and a cost model.
//
// Orthogonal to LG-185 (SLA monitoring watches payments *after* they are
// submitted) and to the rail *message* exports (pacs.008, MT103, ...):
// this file only *selects* the rail. A routed transfer posts with
// Transfer.Rail set, which is exactly what opens the LG-185 SLA watch
// for its settlement deadline.
//
// The routing decision is durable: every decision — routed or rejected
// as infeasible — is written to the structured audit log as a
// routing_decision event (see emitAudit), with the full feasibility
// list. The ledger's journal, version, balances and idempotency state
// are untouched: routing decides, it does not book. The HTTP layer
// counts decisions as ledger_routing_decisions_total{rail,reason}.

var (
	// ErrInvalidRouteRequest is returned when the routing request is
	// malformed: a non-positive amount, an invalid currency, an unknown
	// strategy, or the deadline strategy with no deadline set. It is a
	// 400-class error.
	ErrInvalidRouteRequest = errors.New("ledger: invalid routing request")
	// ErrNoFeasibleRail is returned when no rail can carry the payment
	// under the request's constraints (currency, amount, deadline). It
	// is a 422-class error: the constraints, not the request, are the
	// problem. The returned RouteDecision carries the per-rail
	// feasibility list so the caller can see which constraint excluded
	// which rail.
	ErrNoFeasibleRail = errors.New("ledger: no payment rail is feasible for the routing request")
	// ErrInvalidRailConfig is returned by SetRailConfig for a rail
	// outside the vocabulary or an unusable capability (negative costs,
	// fee above 10000 bps, non-positive duration, negative amount cap,
	// invalid currency codes).
	ErrInvalidRailConfig = errors.New("ledger: invalid rail configuration")
)

// Routing strategies.
const (
	// RouteStrategyCost picks the cheapest feasible rail: the strategy
	// for predictable, non-urgent payments. The default when no
	// strategy is named.
	RouteStrategyCost = "cost"
	// RouteStrategySpeed picks the fastest feasible rail: the strategy
	// for urgent payments. Ties break toward the cheaper rail.
	RouteStrategySpeed = "speed"
	// RouteStrategyDeadline treats the deadline as a hard constraint —
	// only rails that settle before it are feasible — and picks the
	// cheapest of those. It requires RouteRequest.Deadline to be set.
	RouteStrategyDeadline = "deadline"
)

// RouteDecisionReasons label how a routing decision was reached; they
// are also the `reason` label of ledger_routing_decisions_total.
const (
	// RouteReasonCheapest means the cost strategy picked the cheapest
	// feasible rail.
	RouteReasonCheapest = "cheapest"
	// RouteReasonFastest means the speed strategy picked the fastest
	// feasible rail.
	RouteReasonFastest = "fastest"
	// RouteReasonDeadlineCheapest means the deadline strategy picked
	// the cheapest rail among those settling before the deadline.
	RouteReasonDeadlineCheapest = "deadline-cheapest"
	// RouteReasonOnlyFeasible means exactly one rail was feasible, so
	// there was nothing to optimize.
	RouteReasonOnlyFeasible = "only-feasible"
	// RouteReasonNoFeasibleRail means no rail was feasible; the
	// decision carries Rail "" and the feasibility list.
	RouteReasonNoFeasibleRail = "no-feasible-rail"
)

// RailCapability is one settlement rail's routing profile: which
// currencies it carries, the largest single payment it admits, how long
// settlement takes, and what the operator charges per payment. The flat
// and bps figures are operator estimates for *routing* decisions — the
// rail's real downstream charges are outside this ledger's books — and
// are fully overridable with SetRailConfig; the defaults are honest,
// documented, and labeled as estimates in the defaults table.
type RailCapability struct {
	Rail string `json:"rail"`
	// Currencies lists the ISO 4217 codes the rail carries. Empty means
	// "any currency" (the cross-border rails).
	Currencies []string `json:"currencies,omitempty"`
	// MaxAmountCents is the largest single payment the rail admits, in
	// minor units. Zero means unlimited.
	MaxAmountCents int64 `json:"max_amount_cents,omitempty"`
	// SettlementDuration is the expected settlement time: the 20s
	// instant-settlement promise for FedNow/RTP, one business day for
	// SEPA, the gpi rulebook's 24h for gpi, two days for SWIFT and ACH
	// batch settlement.
	SettlementDuration time.Duration `json:"settlement_duration"`
	// FlatFeeCents is the operator's per-payment rail fee estimate in
	// minor units of the payment currency.
	FlatFeeCents int64 `json:"flat_fee_cents"`
	// FeeBps is the operator's per-payment rail fee estimate in basis
	// points of the payment amount, on top of the flat fee.
	FeeBps int64 `json:"fee_bps"`
}

// defaultRailCapabilities is the honest, documented default routing
// table. Amounts limits follow the rails' published per-payment caps
// (FedNow/RTP $1,000,000 per transfer; SEPA credit transfer €100,000);
// durations follow the LG-185 SLA promises; the cost figures are
// operator estimates, deliberately round and conservative, and every
// one of them is overridable with SetRailConfig.
func defaultRailCapabilities() map[string]RailCapability {
	const million = 100_000_000 // $1,000,000 in cents
	return map[string]RailCapability{
		RailFedNow: {
			Rail:               RailFedNow,
			Currencies:         []string{"USD"},
			MaxAmountCents:     million,
			SettlementDuration: 20 * time.Second,
			FlatFeeCents:       25,
		},
		RailRTP: {
			Rail:               RailRTP,
			Currencies:         []string{"USD"},
			MaxAmountCents:     million,
			SettlementDuration: 20 * time.Second,
			FlatFeeCents:       25,
		},
		RailSEPA: {
			Rail:               RailSEPA,
			Currencies:         []string{"EUR"},
			MaxAmountCents:     10_000_000, // €100,000 in cents
			SettlementDuration: 24 * time.Hour,
			FlatFeeCents:       10,
		},
		RailSWIFT: {
			Rail:               RailSWIFT,
			SettlementDuration: 48 * time.Hour,
			FlatFeeCents:       1500,
			FeeBps:             5,
		},
		RailGPI: {
			Rail:               RailGPI,
			SettlementDuration: 24 * time.Hour,
			FlatFeeCents:       1500,
			FeeBps:             5,
		},
		RailACH: {
			Rail:               RailACH,
			Currencies:         []string{"USD"},
			MaxAmountCents:     million,
			SettlementDuration: 48 * time.Hour,
			FlatFeeCents:       5,
		},
	}
}

// validateRailCapability checks one operator-supplied capability.
// The rail name must be in the vocabulary; costs must be non-negative
// with the fee at most 10000 bps; the duration must be positive; the
// amount cap must be non-negative; every currency code must be a valid
// ISO 4217 code. cfg.Rail may be empty — SetRailConfig takes the rail
// name separately — but when set it must match.
func validateRailCapability(rail string, cfg RailCapability) error {
	r, err := normalizeRail(rail)
	if err != nil {
		return fmt.Errorf("%w: %q", ErrInvalidRailConfig, rail)
	}
	if cfg.Rail != "" {
		if cr, err := normalizeRail(cfg.Rail); err != nil || cr != r {
			return fmt.Errorf("%w: capability names rail %q, called for %q", ErrInvalidRailConfig, cfg.Rail, rail)
		}
	}
	if cfg.FlatFeeCents < 0 {
		return fmt.Errorf("%w: negative flat fee", ErrInvalidRailConfig)
	}
	if cfg.FeeBps < 0 || cfg.FeeBps > 10000 {
		return fmt.Errorf("%w: fee_bps %d outside [0, 10000]", ErrInvalidRailConfig, cfg.FeeBps)
	}
	if cfg.SettlementDuration <= 0 {
		return fmt.Errorf("%w: non-positive settlement duration", ErrInvalidRailConfig)
	}
	if cfg.MaxAmountCents < 0 {
		return fmt.Errorf("%w: negative amount cap", ErrInvalidRailConfig)
	}
	seen := make(map[string]bool, len(cfg.Currencies))
	for _, c := range cfg.Currencies {
		if _, err := normalizeCurrency(c); err != nil {
			return fmt.Errorf("%w: currency %q", ErrInvalidRailConfig, c)
		}
		if seen[c] {
			return fmt.Errorf("%w: duplicate currency %q", ErrInvalidRailConfig, c)
		}
		seen[c] = true
	}
	return nil
}

// SetRailConfig overrides one rail's routing capability for this ledger.
// The override is operational config — like payout instructions and gpi
// tracks it is in-memory only, does not bump the ledger version, and
// does not survive snapshots. ResetRailConfig restores the default.
func (l *Ledger) SetRailConfig(rail string, cfg RailCapability) error {
	if err := validateRailCapability(rail, cfg); err != nil {
		return err
	}
	r, _ := normalizeRail(rail)
	l.mu.Lock()
	defer l.mu.Unlock()
	cfg.Rail = r
	l.railConfigs[r] = cfg
	return nil
}

// ResetRailConfig drops a rail's capability override, restoring the
// default table entry. Resetting a rail with no override is a no-op
// returning true when an override was actually removed.
func (l *Ledger) ResetRailConfig(rail string) (bool, error) {
	r, err := normalizeRail(rail)
	if err != nil {
		return false, fmt.Errorf("%w: %q", ErrInvalidRailConfig, rail)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.railConfigs[r]; !ok {
		return false, nil
	}
	delete(l.railConfigs, r)
	return true, nil
}

// RailConfig returns the effective capability of one rail: the
// operator's override when set, the default otherwise.
func (l *Ledger) RailConfig(rail string) (RailCapability, error) {
	r, err := normalizeRail(rail)
	if err != nil {
		return RailCapability{}, fmt.Errorf("%w: %q", ErrInvalidRailConfig, rail)
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.railConfigLocked(r), nil
}

// railConfigLocked returns the effective capability for a normalized
// rail. Callers must hold l.mu; either lock suffices.
func (l *Ledger) railConfigLocked(rail string) RailCapability {
	if cfg, ok := l.railConfigs[rail]; ok {
		return cfg
	}
	return defaultRailCapabilities()[rail]
}

// RouteRequest is one payment to route.
type RouteRequest struct {
	// AmountCents is the payment amount in minor units. Must be
	// positive.
	AmountCents int64 `json:"amount_cents"`
	// Currency is the ISO 4217 code of the payment (default currency
	// when empty).
	Currency string `json:"currency,omitempty"`
	// Deadline is the latest acceptable settlement time. Zero means "no
	// deadline": every rail is time-feasible. Required by the deadline
	// strategy.
	Deadline time.Time `json:"deadline,omitempty"`
	// Strategy is cost, speed, or deadline (see RouteStrategy*). Empty
	// means cost.
	Strategy string `json:"strategy,omitempty"`
	// Now is the decision time: it anchors deadline feasibility and
	// makes routing deterministic under test. Zero means "now".
	Now time.Time `json:"now,omitempty"`
}

// FeasibleRail is one rail's row in the decision's feasibility list:
// whether it could carry the payment, and if not, which constraint
// excluded it (currency-not-supported, amount-exceeds-cap, or
// deadline-missed). Eligible rails carry their estimated cost and
// duration, so the caller can audit the choice.
type FeasibleRail struct {
	Rail           string `json:"rail"`
	Eligible       bool   `json:"eligible"`
	ExcludedReason string `json:"excluded_reason,omitempty"`
	CostCents      int64  `json:"cost_cents,omitempty"`
	DurationMs     int64  `json:"duration_ms,omitempty"`
}

// RouteDecision is the routing outcome: the chosen rail, why it was
// chosen, its estimated cost and duration, and the full per-rail
// feasibility list. Rail is "" when no rail was feasible.
type RouteDecision struct {
	Rail               string `json:"rail"`
	Reason             string `json:"reason"`
	EstimatedCostCents int64  `json:"estimated_cost_cents"`
	EstimatedDurationMs int64 `json:"estimated_duration_ms"`
	// WouldMeetDeadline is true when the chosen rail settles before the
	// deadline — or when no deadline was set. Always false when no rail
	// was feasible.
	WouldMeetDeadline bool           `json:"would_meet_deadline"`
	Feasible          []FeasibleRail `json:"feasible"`
}

// RoutePayment selects the settlement rail for one payment under the
// request's strategy. The full feasibility list is evaluated in the
// rails' canonical order, so ties always resolve deterministically.
//
// Every decision — routed or rejected as infeasible — is written to
// the structured audit log as a routing_decision event with the
// strategy, the request, the chosen rail and reason, and the whole
// feasibility list. The journal, version, balances and idempotency
// state are untouched: routing decides, it never books.
//
// The error is ErrInvalidRouteRequest for malformed requests (400) or
// ErrNoFeasibleRail when nothing can carry the payment (422); in the
// latter case the returned decision still carries the feasibility list
// so the caller can see the blocking constraint.
func (l *Ledger) RoutePayment(req RouteRequest) (RouteDecision, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := req.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	decision := RouteDecision{}
	strategy := strings.ToLower(strings.TrimSpace(req.Strategy))
	if strategy == "" {
		strategy = RouteStrategyCost
	}
	switch strategy {
	case RouteStrategyCost, RouteStrategySpeed, RouteStrategyDeadline:
	default:
		l.emitAuditRoutingDecision(req, strategy, decision, "invalid-strategy")
		return decision, fmt.Errorf("%w: unknown strategy %q", ErrInvalidRouteRequest, req.Strategy)
	}
	if strategy == RouteStrategyDeadline && req.Deadline.IsZero() {
		l.emitAuditRoutingDecision(req, strategy, decision, "missing-deadline")
		return decision, fmt.Errorf("%w: deadline strategy requires a deadline", ErrInvalidRouteRequest)
	}
	if req.AmountCents <= 0 {
		l.emitAuditRoutingDecision(req, strategy, decision, "non-positive-amount")
		return decision, fmt.Errorf("%w: amount must be positive", ErrInvalidRouteRequest)
	}
	currency, err := normalizeCurrency(req.Currency)
	if err != nil {
		l.emitAuditRoutingDecision(req, strategy, decision, "invalid-currency")
		return decision, fmt.Errorf("%w: %v", ErrInvalidRouteRequest, err)
	}

	// Evaluate every rail in canonical order: deterministic ties, and a
	// complete feasibility list for the audit trail.
	var feasible []FeasibleRail
	for _, rail := range slaRails {
		cfg := l.railConfigLocked(rail)
		row := FeasibleRail{Rail: rail, DurationMs: cfg.SettlementDuration.Milliseconds()}
		switch {
		case !railCarriesCurrency(cfg, currency):
			row.ExcludedReason = "currency-not-supported"
		case cfg.MaxAmountCents > 0 && req.AmountCents > cfg.MaxAmountCents:
			row.ExcludedReason = "amount-exceeds-cap"
		case !req.Deadline.IsZero() && now.Add(cfg.SettlementDuration).After(req.Deadline):
			row.ExcludedReason = "deadline-missed"
		default:
			cost, ok := addCents(cfg.FlatFeeCents, policyFeeCents(req.AmountCents, cfg.FeeBps))
			if !ok {
				row.ExcludedReason = "cost-overflow"
				break
			}
			row.Eligible = true
			row.CostCents = cost
		}
		feasible = append(feasible, row)
	}
	decision.Feasible = feasible

	var eligible []FeasibleRail
	for _, f := range feasible {
		if f.Eligible {
			eligible = append(eligible, f)
		}
	}
	if len(eligible) == 0 {
		decision.Reason = RouteReasonNoFeasibleRail
		l.emitAuditRoutingDecision(req, strategy, decision, "no-feasible-rail")
		return decision, ErrNoFeasibleRail
	}

	// Choose: cost minimizes cost, speed minimizes duration, deadline
	// minimizes cost among the deadline-feasible. Ties always resolve
	// by duration, then cost, then canonical rail order — the feasible
	// list is already in canonical order, so the first extremum wins.
	best := eligible[0]
	for _, f := range eligible[1:] {
		switch strategy {
		case RouteStrategySpeed:
			if f.DurationMs < best.DurationMs ||
				(f.DurationMs == best.DurationMs && f.CostCents < best.CostCents) {
				best = f
			}
		default: // cost and deadline both minimize cost
			if f.CostCents < best.CostCents ||
				(f.CostCents == best.CostCents && f.DurationMs < best.DurationMs) {
				best = f
			}
		}
	}
	decision.Rail = best.Rail
	decision.EstimatedCostCents = best.CostCents
	decision.EstimatedDurationMs = best.DurationMs
	decision.WouldMeetDeadline = req.Deadline.IsZero() ||
		!now.Add(time.Duration(best.DurationMs)*time.Millisecond).After(req.Deadline)
	switch {
	case len(eligible) == 1:
		decision.Reason = RouteReasonOnlyFeasible
	case strategy == RouteStrategySpeed:
		decision.Reason = RouteReasonFastest
	case strategy == RouteStrategyDeadline:
		decision.Reason = RouteReasonDeadlineCheapest
	default:
		decision.Reason = RouteReasonCheapest
	}
	l.emitAuditRoutingDecision(req, strategy, decision, "routed")
	return decision, nil
}

// railCarriesCurrency reports whether a rail's capability admits the
// currency: an empty Currencies list means "any currency" (the
// cross-border rails).
func railCarriesCurrency(cfg RailCapability, currency string) bool {
	if len(cfg.Currencies) == 0 {
		return true
	}
	for _, c := range cfg.Currencies {
		if c == currency {
			return true
		}
	}
	return false
}

// emitAuditRoutingDecision writes one routing_decision audit event: the
// durable record of every routing decision, routed or not. The details
// carry the full request and the whole feasibility list, so an auditor
// can replay the decision. outcome is routed, no-feasible-rail, or the
// request-validation failure. Callers must hold l.mu.
func (l *Ledger) emitAuditRoutingDecision(req RouteRequest, strategy string, decision RouteDecision, outcome string) {
	feasible := make([]string, 0, len(decision.Feasible))
	for _, f := range decision.Feasible {
		row := f.Rail + ":"
		if f.Eligible {
			row += "eligible"
		} else {
			row += f.ExcludedReason
		}
		feasible = append(feasible, row)
	}
	sort.Strings(feasible)
	l.emitAudit(AuditEvent{
		Op:    "routing_decision",
		Actor: "RoutePayment",
		Details: map[string]any{
			"outcome":                 outcome,
			"strategy":                strategy,
			"amount_cents":            req.AmountCents,
			"currency":                req.Currency,
			"rail":                    decision.Rail,
			"reason":                  decision.Reason,
			"estimated_cost_cents":    decision.EstimatedCostCents,
			"estimated_duration_ms":   decision.EstimatedDurationMs,
			"would_meet_deadline":     decision.WouldMeetDeadline,
			"deadline":                req.Deadline.UTC().Format(time.RFC3339),
			"feasible":                strings.Join(feasible, ","),
		},
	})
}
