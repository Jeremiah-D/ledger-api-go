package ledger

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// Confirmation of payee (CoP, LG-193) — the UK-style payee-name check
// for transfers.
//
// Before PostTransfer books anything, the payee account's registered
// holder name (see SetAccountHolderName in pii.go) is compared against
// the name the requester expects to pay (Transfer.ExpectedPayeeName).
// The comparison yields one of four tiers:
//
//   - PayeeMatchExact: the names agree after normalization.
//   - PayeeMatchClose: the names agree within the configured fuzzy
//     threshold (typos, transliteration wobble) — still a pass.
//   - PayeeMatchNoMatch: the names disagree.
//   - PayeeMatchUnverifiable: no holder name is registered for the
//     payee account, so there is nothing to compare against.
//
// The check is opt-in per transfer: an empty ExpectedPayeeName skips
// it entirely (backward compatible). A no-match is handled per the
// ledger's PayeeVerificationPolicy — reject the transfer, or require
// the requester's secondary confirmation (Transfer.PayeeNameConfirmed,
// "I checked out-of-band and this is the right payee").
//
// PII discipline: holder names never appear in audit events or error
// strings — the audit trail carries the accounts and the match tier
// only (the same rule SetAccountHolderName follows: "the audit event
// carries the account only, never the name").
//
// The policy is process-local structural config, like noOverdraft: it
// does not bump the journal version and it is not part of the
// snapshot — an operator sets it at startup.
//
// All entry points are safe for concurrent use; the gate below runs
// under the ledger write lock like every other transfer risk gate.

// Payee match tiers returned by the confirmation-of-payee check.
const (
	// PayeeMatchExact: normalized names are equal (case-insensitive).
	PayeeMatchExact = "exact"
	// PayeeMatchClose: names are similar enough (see
	// PayeeVerificationPolicy.CloseMatchThreshold).
	PayeeMatchClose = "close"
	// PayeeMatchNoMatch: names disagree.
	PayeeMatchNoMatch = "no_match"
	// PayeeMatchUnverifiable: no holder name is registered for the
	// payee account.
	PayeeMatchUnverifiable = "unverifiable"
)

// Actions the payee-verification policy can take on a no-match or an
// unverifiable payee.
const (
	// PayeeMismatchReject: the transfer is rejected with
	// ErrPayeeNameMismatch.
	PayeeMismatchReject = "reject"
	// PayeeMismatchAudit: the transfer proceeds; the outcome is
	// recorded in the audit trail only.
	PayeeMismatchAudit = "audit"
)

// DefaultPayeeCloseMatchThreshold is the default normalized similarity
// (0,1] at or above which two names count as a close match: 0.8, i.e.
// up to ~20% of the longer name may differ (typos, transliteration).
const DefaultPayeeCloseMatchThreshold = 0.8

// PayeeVerificationPolicy configures the confirmation-of-payee check.
type PayeeVerificationPolicy struct {
	// CloseMatchThreshold is the normalized name similarity in (0,1]
	// at or above which a non-exact comparison counts as
	// PayeeMatchClose. Similarities below it are PayeeMatchNoMatch.
	CloseMatchThreshold float64
	// OnNoMatch is PayeeMismatchReject (reject the transfer) or
	// PayeeMismatchAudit (book it, audit the mismatch). A caller-set
	// Transfer.PayeeNameConfirmed always overrides a rejection: the
	// transfer books and the audit records the confirmation.
	OnNoMatch string
	// OnUnverifiable is PayeeMismatchReject or PayeeMismatchAudit for
	// payees with no registered holder name. The default audits and
	// proceeds: holder names are opt-in, so an absent name is not a
	// mismatch.
	OnUnverifiable string
}

// DefaultPayeeVerificationPolicy returns the default policy: 0.8 close
// threshold, no-matches rejected, unverifiable payees audited.
func DefaultPayeeVerificationPolicy() PayeeVerificationPolicy {
	return PayeeVerificationPolicy{
		CloseMatchThreshold: DefaultPayeeCloseMatchThreshold,
		OnNoMatch:           PayeeMismatchReject,
		OnUnverifiable:      PayeeMismatchAudit,
	}
}

// validate checks the policy; an error names the offending field.
func (p PayeeVerificationPolicy) validate() error {
	if p.CloseMatchThreshold <= 0 || p.CloseMatchThreshold > 1 {
		return fmt.Errorf("%w: close match threshold %v must be in (0,1]", ErrInvalidPayeeVerificationPolicy, p.CloseMatchThreshold)
	}
	if p.OnNoMatch != PayeeMismatchReject && p.OnNoMatch != PayeeMismatchAudit {
		return fmt.Errorf("%w: on-no-match action %q must be %q or %q", ErrInvalidPayeeVerificationPolicy, p.OnNoMatch, PayeeMismatchReject, PayeeMismatchAudit)
	}
	if p.OnUnverifiable != PayeeMismatchReject && p.OnUnverifiable != PayeeMismatchAudit {
		return fmt.Errorf("%w: on-unverifiable action %q must be %q or %q", ErrInvalidPayeeVerificationPolicy, p.OnUnverifiable, PayeeMismatchReject, PayeeMismatchAudit)
	}
	return nil
}

// resolved returns the policy with zero values replaced by defaults,
// so a zero PayeeVerificationPolicy (e.g. a hand-built Ledger that
// bypassed New) behaves like the default policy instead of a
// degenerate one (a zero threshold would make every name "close").
func (p PayeeVerificationPolicy) resolved() PayeeVerificationPolicy {
	if p.CloseMatchThreshold <= 0 || p.CloseMatchThreshold > 1 {
		p.CloseMatchThreshold = DefaultPayeeCloseMatchThreshold
	}
	if p.OnNoMatch != PayeeMismatchReject && p.OnNoMatch != PayeeMismatchAudit {
		p.OnNoMatch = PayeeMismatchReject
	}
	if p.OnUnverifiable != PayeeMismatchReject && p.OnUnverifiable != PayeeMismatchAudit {
		p.OnUnverifiable = PayeeMismatchAudit
	}
	return p
}

var (
	// ErrPayeeNameMismatch is returned when the confirmation-of-payee
	// check finds no match and the policy rejects (or the policy
	// audits but the requester did not confirm — see
	// Transfer.PayeeNameConfirmed). The error names the account, never
	// the names: holder names are PII and stay out of error strings.
	ErrPayeeNameMismatch = errors.New("ledger: payee name does not match the expected name")
	// ErrInvalidPayeeVerificationPolicy is returned for a policy with
	// an out-of-range threshold or an unknown action.
	ErrInvalidPayeeVerificationPolicy = errors.New("ledger: invalid payee verification policy")
	// ErrInvalidExpectedPayeeName is returned for an expected payee
	// name longer than 128 characters (the holder-name limit).
	ErrInvalidExpectedPayeeName = errors.New("ledger: expected payee name must be at most 128 characters")
)

// SetPayeeVerificationPolicy installs the confirmation-of-payee
// policy. Structural config, like noOverdraft: it never bumps the
// journal version.
func (l *Ledger) SetPayeeVerificationPolicy(p PayeeVerificationPolicy) error {
	if err := p.validate(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.payeeVerificationPolicy = p
	return nil
}

// PayeeVerificationPolicy returns the ledger's current
// confirmation-of-payee policy.
func (l *Ledger) PayeeVerificationPolicy() PayeeVerificationPolicy {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.payeeVerificationPolicy
}

// normalizePayeeName trims surrounding whitespace and collapses inner
// whitespace runs to single spaces. Case is left alone: the exact
// comparison is case-insensitive via EqualFold, and the fuzzy
// comparison folds per rune.
func normalizePayeeName(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// levenshteinDistance is the rune-wise edit distance between a and b.
// Names are at most 128 runes, so the O(n*m) table is tiny.
func levenshteinDistance(a, b []rune) int {
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 0
			if unicode.ToLower(a[i-1]) != unicode.ToLower(b[j-1]) {
				cost = 1
			}
			del := prev[j] + 1
			ins := cur[j-1] + 1
			sub := prev[j-1] + cost
			cur[j] = del
			if ins < cur[j] {
				cur[j] = ins
			}
			if sub < cur[j] {
				cur[j] = sub
			}
		}
		prev = cur
	}
	return prev[len(b)]
}

// payeeNameSimilarity is the normalized similarity of two names in
// [0,1]: 1 for identical, 0 for nothing in common. Case-insensitive;
// the distance is scaled by the longer name so a one-letter typo in a
// short name scores lower than the same typo in a long one.
func payeeNameSimilarity(a, b string) float64 {
	ra, rb := []rune(normalizePayeeName(a)), []rune(normalizePayeeName(b))
	longest := len(ra)
	if len(rb) > longest {
		longest = len(rb)
	}
	if longest == 0 {
		return 1
	}
	return 1 - float64(levenshteinDistance(ra, rb))/float64(longest)
}

// MatchPayeeName compares a registered holder name against the
// requester's expected name, returning the match tier. closeThreshold
// is the normalized similarity in (0,1] for PayeeMatchClose. An empty
// registered name is PayeeMatchUnverifiable — there is nothing to
// compare against.
func MatchPayeeName(actual, expected string, closeThreshold float64) string {
	normActual := normalizePayeeName(actual)
	normExpected := normalizePayeeName(expected)
	if normActual == "" {
		return PayeeMatchUnverifiable
	}
	if strings.EqualFold(normActual, normExpected) {
		return PayeeMatchExact
	}
	if payeeNameSimilarity(normActual, normExpected) >= closeThreshold {
		return PayeeMatchClose
	}
	return PayeeMatchNoMatch
}

// holderNameLocked reads the holder-name registry. Callers must hold
// l.mu (either lock suffices).
func (l *Ledger) holderNameLocked(account AccountID) (string, bool) {
	rec, ok := l.holderNames[account]
	if !ok {
		return "", false
	}
	return string(rec.name), true
}

// VerifyPayeeName is the standalone confirmation-of-payee check: it
// compares expected against the payee account's registered holder
// name and returns the match tier, without booking anything. An empty
// expected name returns "" and no error — the check is opt-in.
// Callers use it to pre-screen (e.g. a payer UI warning) before
// submitting the transfer with Transfer.ExpectedPayeeName.
func (l *Ledger) VerifyPayeeName(account AccountID, expected string) (string, error) {
	if normalizePayeeName(expected) == "" {
		return "", nil
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	actual, ok := l.holderNameLocked(account)
	if !ok {
		return PayeeMatchUnverifiable, nil
	}
	return MatchPayeeName(actual, expected, l.payeeVerificationPolicy.resolved().CloseMatchThreshold), nil
}

// payeeVerificationLocked is the PostTransfer confirmation-of-payee
// gate. It runs after the idempotency replay check (a replay returns
// the original receipt instead of re-checking) and after VAN
// attribution (the check sees the real payee account), alongside the
// other payee-directed gates (allowlist) and before the overdraft
// gate. It returns the match tier for the receipt, or "" when the
// check was skipped (no expected name).
//
// Every outcome is audited — verified, close, mismatch (rejected,
// confirmed, or allowed), and unverifiable — with the accounts and the
// tier only, never the names. A rejection records nothing else: no
// journal rows, no chain links, no version bump. Callers must hold the
// write lock.
func (l *Ledger) payeeVerificationLocked(t Transfer) (string, error) {
	if normalizePayeeName(t.ExpectedPayeeName) == "" {
		return "", nil
	}
	policy := l.payeeVerificationPolicy.resolved()
	actual, ok := l.holderNameLocked(t.To)
	if !ok {
		const tier = PayeeMatchUnverifiable
		l.emitAudit(AuditEvent{
			Op:       "payee_name_unverifiable",
			Actor:    "PostTransfer",
			Accounts: []AccountID{t.From, t.To},
			Details:  map[string]any{"payee": string(t.To), "match": tier},
		})
		if policy.OnUnverifiable == PayeeMismatchReject {
			return "", fmt.Errorf("%w: no holder name registered for payee %q", ErrPayeeNameMismatch, t.To)
		}
		return tier, nil
	}
	tier := MatchPayeeName(actual, t.ExpectedPayeeName, policy.CloseMatchThreshold)
	switch tier {
	case PayeeMatchExact, PayeeMatchClose:
		l.emitAudit(AuditEvent{
			Op:       "payee_name_verified",
			Actor:    "PostTransfer",
			Accounts: []AccountID{t.From, t.To},
			Details:  map[string]any{"payee": string(t.To), "match": tier},
		})
		return tier, nil
	default: // PayeeMatchNoMatch
		if t.PayeeNameConfirmed {
			// Secondary confirmation: the requester checked
			// out-of-band and vouches for the payee. The transfer
			// books; the audit records the override.
			l.emitAudit(AuditEvent{
				Op:       "payee_name_mismatch_confirmed",
				Actor:    "PostTransfer",
				Accounts: []AccountID{t.From, t.To},
				Details:  map[string]any{"payee": string(t.To), "match": tier, "confirmed": true},
			})
			return tier, nil
		}
		if policy.OnNoMatch == PayeeMismatchReject {
			l.emitAudit(AuditEvent{
				Op:       "payee_name_mismatch_rejected",
				Actor:    "PostTransfer",
				Accounts: []AccountID{t.From, t.To},
				Details:  map[string]any{"payee": string(t.To), "match": tier},
			})
			return "", fmt.Errorf("%w: payee %q", ErrPayeeNameMismatch, t.To)
		}
		l.emitAudit(AuditEvent{
			Op:       "payee_name_mismatch_allowed",
			Actor:    "PostTransfer",
			Accounts: []AccountID{t.From, t.To},
			Details:  map[string]any{"payee": string(t.To), "match": tier},
		})
		return tier, nil
	}
}
