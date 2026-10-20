package ledger

import (
	"context"
	"errors"
	"sort"
	"time"
)

// KYC/尽调状态门控 (LG-147, 支付 fintech).
//
// Every account may carry a KYC (know-your-customer / 尽调) profile: the
// due-diligence state the compliance team recorded for the account's
// beneficial owner. The profile gates the posting paths:
//
//   - An account whose profile is expired blocks ALL new Post/Transfer
//     bookings through it (ErrKycExpired), whatever the amount: a lapsed
//     KYC means the ledger no longer knows who it is dealing with, so
//     the account is stopped until re-verified.
//   - An account with no profile, or a profile in unverified/pending
//     state, is rejected only when the posting amount exceeds the
//     ledger's opt-in KYC threshold (see SetKycThreshold):
//     small-value activity may proceed while the review is pending, but
//     material amounts need a verified counterparty. A zero threshold
//     disables the threshold gate entirely (the default).
//
// Ordering inside the write paths is deliberate: the idempotency replay
// check runs first, so replaying a key that was posted before the
// account's KYC lapsed returns the original entry/receipt instead of
// failing — a replay books nothing new, and breaking the idempotency
// contract on a KYC-lapsed account would turn a compliance control into
// a data-availability incident for callers that retry. The KYC gate
// itself runs after the overdraft check: both are account risk controls,
// and a frozen account still fails 403 before the KYC question arises.
//
// Expiry is lazy: every write path sweeps expired profiles before the
// KYC gate (like the timed-freeze release), and ExpireKyc is the explicit
// operator sweep. Structural config, like noOverdraft: profile changes
// do not bump the journal version and survive disaster-recovery
// snapshots.

// KycStatus is the KYC/尽调 state of an account.
type KycStatus string

// KYC states. Unverified is the default for accounts with no profile.
// Expired is derived: it is set only by the expiry sweep (see
// ExpireKyc), never by SetKyc directly — an operator cannot
// backdate-expiry an account without the sweep's audit trail.
const (
	KycUnverified KycStatus = "unverified"
	KycPending    KycStatus = "pending"
	KycVerified   KycStatus = "verified"
	KycExpired    KycStatus = "expired"
)

// KycProfile is the per-account KYC/尽调 record.
type KycProfile struct {
	Account  AccountID `json:"account"`
	Status   KycStatus `json:"status"`
	RiskTier string    `json:"risk_tier,omitempty"`
	// VerifiedAt is when the account was last verified (zero when the
	// profile never reached verified).
	VerifiedAt time.Time `json:"verified_at,omitempty"`
	// ExpiresAt is when the verification lapses (zero = no expiry).
	// When it passes, the profile moves to expired on the next write
	// path or ExpireKyc sweep.
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

// KYC validation and gate errors.
var (
	// ErrKycUnverified is returned by Post/Transfer when a leg names an
	// account with no (or a non-verified) KYC profile and the posting
	// amount exceeds the ledger's KYC threshold. It is a 422-class
	// semantic rejection: the request is well-formed, the counterparty
	// just is not cleared for that size.
	ErrKycUnverified = errors.New("ledger: unverified account exceeds KYC posting threshold")
	// ErrKycExpired is returned by Post/Transfer when a leg names an
	// account whose KYC profile has expired. Any amount is rejected:
	// the account is stopped until re-verified. 422-class, like the
	// threshold rejection.
	ErrKycExpired = errors.New("ledger: account KYC expired; re-verification required before posting")
	// ErrKycInvalidStatus is returned by SetKyc when the profile names
	// an unknown status, or the derived expired state (set only by the
	// expiry sweep). 400-class field validation.
	ErrKycInvalidStatus = errors.New("ledger: KYC status must be unverified, pending, or verified")
	// ErrKycInvalidThreshold is returned by SetKycThreshold for a
	// negative threshold. 400-class field validation.
	ErrKycInvalidThreshold = errors.New("ledger: KYC threshold must not be negative")
)

// validKycStatus reports whether s is a status SetKyc accepts. The
// expired state is derived-only (see ExpireKyc), never set directly.
func validKycStatus(s KycStatus) bool {
	return s == KycUnverified || s == KycPending || s == KycVerified
}

// SetKyc installs (or replaces) the KYC profile of account a. Setting a
// profile does not bump the journal version: balances are unchanged by a
// compliance marking. An empty account is rejected; the status must be
// unverified, pending, or verified (expired is derived-only). VerifiedAt
// is stamped with the current time when the status is verified and the
// caller left it zero, so operators do not have to clock it by hand;
// passing an explicit VerifiedAt preserves the operator's record (e.g.
// importing a profile verified in another system).
func (l *Ledger) SetKyc(a AccountID, p KycProfile) error {
	return l.SetKycCtx(context.Background(), a, p)
}

// SetKycCtx is SetKyc with per-call audit actor attribution (LG-73).
func (l *Ledger) SetKycCtx(ctx context.Context, a AccountID, p KycProfile) error {
	if a == "" {
		return ErrKycInvalidStatus
	}
	if !validKycStatus(p.Status) {
		return ErrKycInvalidStatus
	}
	if p.Status == KycVerified && p.VerifiedAt.IsZero() {
		p.VerifiedAt = time.Now().UTC()
	}
	p.Account = a
	l.mu.Lock()
	defer l.mu.Unlock()
	l.kyc[a] = p
	details := map[string]any{"status": string(p.Status)}
	if p.RiskTier != "" {
		details["risk_tier"] = p.RiskTier
	}
	if !p.ExpiresAt.IsZero() {
		details["expires_at"] = p.ExpiresAt.UTC().Format(time.RFC3339)
	}
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "kyc_set",
		Actor:         "SetKyc",
		TraceID:       string(a),
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Accounts:      []AccountID{a},
		Details:       details,
	})
	l.emitOutboxLocked(OutboxEvent{
		Type:    OutboxKycStatusSet,
		Account: a,
		Details: map[string]any{"status": string(p.Status)},
	})
	return nil
}

// GetKyc returns the KYC profile of account a. The second return value
// is false for accounts with no profile: they post as unverified.
func (l *Ledger) GetKyc(a AccountID) (KycProfile, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	p, ok := l.kyc[a]
	return p, ok
}

// SetKycThreshold sets the ledger-wide posting threshold (in cents)
// above which accounts with no (or a non-verified) KYC profile are
// rejected. A zero threshold disables the threshold gate (the default):
// only expired profiles block postings. A negative threshold is
// rejected. Structural config, like the profile map itself: it does not
// bump the journal version and survives snapshots.
func (l *Ledger) SetKycThreshold(cents int64) error {
	if cents < 0 {
		return ErrKycInvalidThreshold
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.kycThresholdCents = cents
	l.emitAudit(AuditEvent{
		Op:            "kyc_threshold_set",
		Actor:         "SetKycThreshold",
		TraceID:       "kyc-threshold",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details:       map[string]any{"threshold_cents": cents},
	})
	return nil
}

// KycThreshold reports the current ledger-wide KYC posting threshold in
// cents (0 = the threshold gate is disabled).
func (l *Ledger) KycThreshold() int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.kycThresholdCents
}

// ExpireKyc sweeps every profile whose ExpiresAt has passed as of now to
// the expired state and returns the number of profiles expired. It is the
// explicit operator twin of the lazy write-path sweep (see
// expireKycLocked): the same scan, the same audit events. Each expiry is
// recorded as a "kyc_expired" structured audit event and announced on
// the outbox, so the compliance trail shows exactly when the account was
// stopped.
func (l *Ledger) ExpireKyc(now time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.expireKycLocked(now)
}

// expireKycLocked moves every verified/pending profile with a non-zero
// ExpiresAt at or before now to the expired state, and returns how many
// profiles moved. Expired profiles stay expired (the sweep is idempotent),
// and profiles already expired are never re-emitted. Callers must hold
// the write lock.
func (l *Ledger) expireKycLocked(now time.Time) int {
	expired := 0
	for a, p := range l.kyc {
		if p.Status != KycVerified && p.Status != KycPending {
			continue
		}
		if p.ExpiresAt.IsZero() || now.Before(p.ExpiresAt) {
			continue
		}
		p.Status = KycExpired
		l.kyc[a] = p
		expired++
		l.emitAudit(AuditEvent{
			Op:            "kyc_expired",
			Actor:         "KycExpiry",
			TraceID:       string(a),
			VersionBefore: l.version,
			VersionAfter:  l.version,
			Accounts:      []AccountID{a},
			Details: map[string]any{
				"risk_tier":  p.RiskTier,
				"expired_at": p.ExpiresAt.UTC().Format(time.RFC3339),
			},
		})
		l.emitOutboxLocked(OutboxEvent{
			Type:    OutboxKycExpired,
			Account: a,
		})
	}
	return expired
}

// kycRejectedLocked reports whether a posting of amountCents through the
// debit/credit legs would violate the KYC gate, returning the rejection
// error or nil. An expired profile on either leg blocks any amount; a
// leg with no profile (or an unverified/pending profile) is rejected only
// when the amount exceeds the ledger's opt-in threshold. Callers must
// hold l.mu. The comparison never subtracts, so it cannot overflow.
func (l *Ledger) kycRejectedLocked(debit, credit AccountID, amountCents int64) error {
	for _, a := range []AccountID{debit, credit} {
		p, ok := l.kyc[a]
		if !ok {
			// No profile: the account posts as unverified.
			if l.kycThresholdCents > 0 && amountCents > l.kycThresholdCents {
				return ErrKycUnverified
			}
			continue
		}
		switch p.Status {
		case KycExpired:
			return ErrKycExpired
		case KycUnverified, KycPending:
			if l.kycThresholdCents > 0 && amountCents > l.kycThresholdCents {
				return ErrKycUnverified
			}
		case KycVerified:
			// Cleared: verified accounts post at any amount.
		}
	}
	return nil
}

// KycSummary is the Reconcile disclosure of the KYC position (see
// ReconciliationReport.Kyc): the configured threshold, the profile
// counts by state, and the currently expired accounts. Accounts with no
// profile are not tracked (they are unbounded), so the counts cover
// only accounts with a recorded profile.
type KycSummary struct {
	ThresholdCents  int64       `json:"threshold_cents"`
	Unverified      int         `json:"unverified"`
	Pending         int         `json:"pending"`
	Verified        int         `json:"verified"`
	Expired         int         `json:"expired"`
	ExpiredAccounts []AccountID `json:"expired_accounts"`
}

// kycSummaryLocked builds the Reconcile KYC disclosure. Callers must
// hold l.mu.
func (l *Ledger) kycSummaryLocked() KycSummary {
	s := KycSummary{ThresholdCents: l.kycThresholdCents}
	for a, p := range l.kyc {
		switch p.Status {
		case KycUnverified:
			s.Unverified++
		case KycPending:
			s.Pending++
		case KycVerified:
			s.Verified++
		case KycExpired:
			s.Expired++
			s.ExpiredAccounts = append(s.ExpiredAccounts, a)
		}
	}
	sort.Slice(s.ExpiredAccounts, func(i, j int) bool { return s.ExpiredAccounts[i] < s.ExpiredAccounts[j] })
	return s
}

// kycProfilesLocked returns every recorded KYC profile, sorted by
// account, for the snapshot config record. Callers must hold l.mu.
func (l *Ledger) kycProfilesLocked() []KycProfile {
	out := make([]KycProfile, 0, len(l.kyc))
	for _, p := range l.kyc {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Account < out[j].Account })
	return out
}
