package ledger

import (
	"errors"
	"fmt"
	"strings"
)

// FATF travel rule (Recommendation 16, LG-196) — the originator /
// beneficiary information set that must travel with a cross-border
// transfer.
//
// Before a cross-border transfer is submitted, the transfer must carry
// the complete information set:
//
//   - originator: name + account + address OR an identifying document
//     (either one satisfies the third leg — the rule asks for an
//     address or a national identity number, customer identification
//     number, or date/place of birth, not all of them);
//   - beneficiary: name + account.
//
// The originator name and account come from the transfer itself (the
// payer account and its registered holder name, see
// SetAccountHolderName); the beneficiary name comes from the payee
// account's registered holder name; the originator address-or-identifier
// is supplied per transfer as Transfer.TravelRuleOriginatorID. Nothing
// here mints identities — it only checks that the set the caller
// assembled is complete.
//
// Threshold: the completeness requirement is enforced fail-closed only
// above the configured threshold (default 100000 minor units, i.e.
// 1000.00 in the transfer currency — exactly USD 1000 for USD
// transfers; operators serving non-USD corridors set the local
// equivalent through SetTravelRulePolicy). At or below the threshold
// an incomplete set still books: the information already present
// travels with the transfer (the assessment reports which fields are
// present), it is just not a blocker. "Above" is strict: a transfer
// exactly at the threshold is not forced.
//
// PII discipline: audit events and error strings carry only field
// names ("originator_name", …) and the completeness/threshold
// conclusions — never a name, address, or identifier value. The same
// rule the confirmation-of-payee check follows.
//
// Orthogonality: this is the information set transmitted with the
// transaction. It is not kyc.go (whether the account holder's identity
// was verified) and not purpose.go (what the payment is for); a
// transfer can pass KYC and carry a purpose code and still fail the
// travel rule, and vice versa.
//
// The policy is process-local structural config, like the
// confirmation-of-payee policy: it does not bump the journal version
// and it is not part of the snapshot — an operator sets it at startup.
//
// All entry points are safe for concurrent use; the PostTransfer gate
// runs under the ledger write lock like every other transfer gate.

// DefaultTravelRuleThresholdMinorUnits is the default travel-rule
// threshold in the transfer currency's minor units: 100000, i.e.
// 1000.00 (USD 1000 for USD transfers).
const DefaultTravelRuleThresholdMinorUnits = 100_000

// Travel-rule information-set field names, used in assessments, audit
// details, and error strings. Field names only — never values.
const (
	TravelRuleFieldOriginatorName       = "originator_name"
	TravelRuleFieldOriginatorAccount    = "originator_account"
	TravelRuleFieldOriginatorAddressOrID = "originator_address_or_id"
	TravelRuleFieldBeneficiaryName      = "beneficiary_name"
	TravelRuleFieldBeneficiaryAccount   = "beneficiary_account"
)

// MaxTravelRuleOriginatorIDRunes caps Transfer.TravelRuleOriginatorID:
// an address or document identifier longer than this is rejected at
// field validation, before anything is recorded.
const MaxTravelRuleOriginatorIDRunes = 256

// TravelRulePolicy configures the travel-rule gate.
type TravelRulePolicy struct {
	// ThresholdMinorUnits is the amount, in the transfer currency's
	// minor units, strictly above which a complete information set is
	// mandatory. Transfers at or below it book regardless.
	ThresholdMinorUnits int64
}

// DefaultTravelRulePolicy returns the default policy: USD-1000
// equivalent threshold (100000 minor units), enforced above it.
func DefaultTravelRulePolicy() TravelRulePolicy {
	return TravelRulePolicy{ThresholdMinorUnits: DefaultTravelRuleThresholdMinorUnits}
}

// validate checks the policy; an error names the offending field.
func (p TravelRulePolicy) validate() error {
	if p.ThresholdMinorUnits <= 0 {
		return fmt.Errorf("%w: threshold %d minor units must be positive", ErrInvalidTravelRulePolicy, p.ThresholdMinorUnits)
	}
	return nil
}

// resolved returns the policy with a non-positive threshold replaced
// by the default, so a zero TravelRulePolicy (e.g. a hand-built Ledger
// that bypassed New) behaves like the default policy instead of
// forcing every transfer (a zero threshold would make every amount
// "above threshold").
func (p TravelRulePolicy) resolved() TravelRulePolicy {
	if p.ThresholdMinorUnits <= 0 {
		return DefaultTravelRulePolicy()
	}
	return p
}

// Travel-rule errors.
var (
	// ErrInvalidTravelRulePolicy is returned for a policy with a
	// non-positive threshold.
	ErrInvalidTravelRulePolicy = errors.New("ledger: invalid travel rule policy")
	// ErrTravelRuleIncomplete is returned for a cross-border transfer
	// above the threshold whose information set is incomplete. The
	// error names the missing fields only, never their values.
	ErrTravelRuleIncomplete = errors.New("ledger: travel rule information set incomplete")
	// ErrInvalidTravelRuleOriginatorID is returned for an originator
	// address-or-identifier longer than MaxTravelRuleOriginatorIDRunes.
	ErrInvalidTravelRuleOriginatorID = errors.New("ledger: travel rule originator address-or-identifier must be at most 256 characters")
)

// SetTravelRulePolicy installs the travel-rule policy. Structural
// config, like the confirmation-of-payee policy: it never bumps the
// journal version.
func (l *Ledger) SetTravelRulePolicy(p TravelRulePolicy) error {
	if err := p.validate(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.travelRulePolicy = p
	return nil
}

// TravelRulePolicy returns the ledger's current travel-rule policy.
func (l *Ledger) TravelRulePolicy() TravelRulePolicy {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.travelRulePolicy
}

// TravelRuleInfo is the cross-border information set to assess. All
// fields are values to check for presence, not to store: the ledger
// keeps no copy of the names, address, or identifier beyond the
// transfer itself.
type TravelRuleInfo struct {
	// OriginatorName is the originator's name.
	OriginatorName string
	// OriginatorAccount is the originator's account identifier.
	OriginatorAccount string
	// OriginatorAddressOrID is the originator's address or an
	// identifying document reference — either one satisfies the
	// originator's third leg.
	OriginatorAddressOrID string
	// BeneficiaryName is the beneficiary's name.
	BeneficiaryName string
	// BeneficiaryAccount is the beneficiary's account identifier.
	BeneficiaryAccount string
	// AmountMinor is the transfer amount in minor units, compared
	// against the policy threshold in the same units.
	AmountMinor int64
}

// TravelRuleAssessment is the outcome of assessing an information set.
type TravelRuleAssessment struct {
	// Complete is true when every required field is present.
	Complete bool
	// AboveThreshold is true when the amount is strictly above the
	// policy threshold (and therefore completeness is mandatory).
	AboveThreshold bool
	// Missing lists the required field names that are absent.
	Missing []string
	// Present lists the required field names that are present — the
	// information that travels with the transfer even when the set
	// is incomplete and the transfer books below the threshold.
	Present []string
}

// AssessTravelRule assesses info against policy without booking
// anything and without emitting audit events, so callers can
// pre-screen (e.g. a payer UI warning) before submitting the transfer.
func AssessTravelRule(info TravelRuleInfo, policy TravelRulePolicy) TravelRuleAssessment {
	policy = policy.resolved()
	assessment := TravelRuleAssessment{AboveThreshold: info.AmountMinor > policy.ThresholdMinorUnits}
	fields := []struct {
		name    string
		present bool
	}{
		{TravelRuleFieldOriginatorName, strings.TrimSpace(info.OriginatorName) != ""},
		{TravelRuleFieldOriginatorAccount, strings.TrimSpace(info.OriginatorAccount) != ""},
		{TravelRuleFieldOriginatorAddressOrID, strings.TrimSpace(info.OriginatorAddressOrID) != ""},
		{TravelRuleFieldBeneficiaryName, strings.TrimSpace(info.BeneficiaryName) != ""},
		{TravelRuleFieldBeneficiaryAccount, strings.TrimSpace(info.BeneficiaryAccount) != ""},
	}
	for _, f := range fields {
		if f.present {
			assessment.Present = append(assessment.Present, f.name)
		} else {
			assessment.Missing = append(assessment.Missing, f.name)
		}
	}
	assessment.Complete = len(assessment.Missing) == 0
	return assessment
}

// TravelRuleDisclosure is the travel-rule outcome disclosed on a
// transfer receipt: whether the information set was complete and
// whether the transfer was above the enforcement threshold.
type TravelRuleDisclosure struct {
	Complete       bool `json:"complete"`
	AboveThreshold bool `json:"above_threshold"`
}

// CheckTravelRule assesses info against the ledger's policy, audits
// the outcome (completeness and threshold conclusions only, never
// values), and fails closed when the transfer is above the threshold
// with an incomplete set. It books nothing.
func (l *Ledger) CheckTravelRule(info TravelRuleInfo) (TravelRuleAssessment, error) {
	l.mu.RLock()
	policy := l.travelRulePolicy.resolved()
	l.mu.RUnlock()
	assessment := AssessTravelRule(info, policy)
	allowed := assessment.Complete || !assessment.AboveThreshold
	l.emitAudit(AuditEvent{
		Op:    "travel_rule_checked",
		Actor: "CheckTravelRule",
		Details: map[string]any{
			"complete":        assessment.Complete,
			"above_threshold": assessment.AboveThreshold,
			"allowed":         allowed,
			"missing":         assessment.Missing,
		},
	})
	if !allowed {
		return assessment, fmt.Errorf("%w: missing %s", ErrTravelRuleIncomplete, strings.Join(assessment.Missing, ","))
	}
	return assessment, nil
}

// travelRuleLocked is the PostTransfer travel-rule gate. It runs after
// the idempotency replay check (a replay returns the original receipt
// instead of re-checking) and after the confirmation-of-payee gate —
// both are payee-directed checks — and before the overdraft gate, so
// the check order stays validation → idempotency replay → ID conflict
// → frozen → allowlist → payee-verification → travel-rule → overdraft
// → commit. It returns nil when the check was skipped (not a
// cross-border transfer). The originator and beneficiary names are the
// registered holder names of the payer and payee accounts; an
// unregistered holder name counts as a missing field, exactly like an
// unverifiable payee. Callers must hold the write lock.
func (l *Ledger) travelRuleLocked(t Transfer) (*TravelRuleDisclosure, error) {
	if !t.CrossBorder {
		return nil, nil
	}
	originatorName, _ := l.holderNameLocked(t.From)
	beneficiaryName, _ := l.holderNameLocked(t.To)
	info := TravelRuleInfo{
		OriginatorName:        originatorName,
		OriginatorAccount:     string(t.From),
		OriginatorAddressOrID: t.TravelRuleOriginatorID,
		BeneficiaryName:       beneficiaryName,
		BeneficiaryAccount:    string(t.To),
		AmountMinor:           t.AmountCents,
	}
	assessment := AssessTravelRule(info, l.travelRulePolicy.resolved())
	disclosure := &TravelRuleDisclosure{Complete: assessment.Complete, AboveThreshold: assessment.AboveThreshold}
	if assessment.AboveThreshold && !assessment.Complete {
		l.emitAudit(AuditEvent{
			Op:       "travel_rule_rejected",
			Actor:    "PostTransfer",
			Accounts: []AccountID{t.From, t.To},
			Details: map[string]any{
				"complete":        false,
				"above_threshold": true,
				"missing":         assessment.Missing,
			},
		})
		return nil, fmt.Errorf("%w: missing %s", ErrTravelRuleIncomplete, strings.Join(assessment.Missing, ","))
	}
	l.emitAudit(AuditEvent{
		Op:       "travel_rule_checked",
		Actor:    "PostTransfer",
		Accounts: []AccountID{t.From, t.To},
		Details: map[string]any{
			"complete":        assessment.Complete,
			"above_threshold": assessment.AboveThreshold,
			"missing":         assessment.Missing,
		},
	})
	return disclosure, nil
}
