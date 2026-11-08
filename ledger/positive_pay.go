package ledger

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Positive pay check fraud prevention (LG-197) — the paper-check
// channel's counterpart to the electronic transfer gates.
//
// The operator registers every check it issues in an issued-check
// ledger (check number, amount, payee, issue date, per account); each
// check presented for payment is then compared against its registered
// entry:
//
//   - full match (same account, number, amount, currency, and payee
//     after case/whitespace normalization) clears for posting;
//   - amount or payee mismatch enters the exception queue;
//   - currency mismatch enters the exception queue too — an amount
//     in the wrong currency is not the amount that was issued;
//   - a check with no registered entry enters the exception queue by
//     default (unregistered checks never clear on first sight).
//
// An exception clears nothing until a human adjudicates it: a "pay"
// decision approves the check for posting (a later present returns
// cleared), a "return" decision bounces it (a later present returns
// the returned disposition). Every step — issue, clear, exception,
// decision — is audited, and the adjudication itself enters the audit
// chain like any other ledger event (see emitAudit).
//
// A stop-payment order (LG-355) is the pre-emptive counterpart for a
// lost or stolen check: StopIssuedCheck marks a registered, uncleared
// check stopped, and its presentment then enters the exception queue
// with reason "stopped" instead of clearing — even on a full field
// match — for the same human adjudication. ReleaseStopIssuedCheck
// lifts the order and restores ordinary comparison, but never
// silently clears an exception the stop already produced.
//
// Payee matching is exact after normalization (case-insensitive,
// whitespace-collapsed, the normalizePayeeName convention) — not
// fuzzy: a misspelled payee on a paper check is exactly what a human
// should look at, not something to auto-clear on similarity.
//
// Orthogonality: this is the paper-check channel. It is not
// mandate_preauth.go (a pre-authorization hold on an electronic
// mandate): a check is presented, compared, and adjudicated here, and
// only a cleared check is posted through the normal posting path by
// the caller — this module books no journal entries itself.
//
// The register is process-local operational state, like the
// confirmation-of-payee policy: it is not part of the snapshot. The
// operator re-registers issued checks at startup from the
// check-issuance system of record, which remains authoritative.
//
// All entry points are safe for concurrent use.

// Positive-pay exception statuses.
const (
	// PositivePayExceptionPending: the exception awaits adjudication.
	PositivePayExceptionPending = "pending"
	// PositivePayExceptionPaid: a human approved the exception for
	// posting.
	PositivePayExceptionPaid = "paid"
	// PositivePayExceptionReturned: a human bounced the exception.
	PositivePayExceptionReturned = "returned"
)

// Positive-pay presentment dispositions.
const (
	// PositivePayCleared: the check may be posted.
	PositivePayCleared = "cleared"
	// PositivePayDispositionException: the check entered (or is still in) the
	// exception queue and must not be posted yet.
	PositivePayDispositionException = "exception"
	// PositivePayReturned: the check was adjudicated "return" and
	// must not be posted.
	PositivePayReturned = "returned"
)

// Positive-pay exception reasons.
const (
	// PositivePayReasonUnregistered: no issued check matches the
	// account + number.
	PositivePayReasonUnregistered = "unregistered"
	// PositivePayReasonAmountMismatch: the presented amount differs
	// from the issued amount.
	PositivePayReasonAmountMismatch = "amount_mismatch"
	// PositivePayReasonPayeeMismatch: the presented payee differs from
	// the issued payee after normalization.
	PositivePayReasonPayeeMismatch = "payee_mismatch"
	// PositivePayReasonCurrencyMismatch: the presented currency
	// differs from the issued currency.
	PositivePayReasonCurrencyMismatch = "currency_mismatch"
	// PositivePayReasonStopped: the check is under a stop-payment
	// order (LG-355) — it enters the exception queue even though
	// every field matches the register.
	PositivePayReasonStopped = "stopped"
)

// Positive-pay decisions.
const (
	// PositivePayDecisionPay approves an exception for posting.
	PositivePayDecisionPay = "pay"
	// PositivePayDecisionReturn bounces an exception.
	PositivePayDecisionReturn = "return"
)

// Positive-pay errors.
var (
	// ErrInvalidIssuedCheck is returned for an issued check with an
	// empty account, number, currency, payee, or issue date, or a
	// non-positive amount.
	ErrInvalidIssuedCheck = errors.New("ledger: invalid issued check")
	// ErrPositivePayDuplicateCheck is returned when registering a
	// check whose account + number is already registered.
	ErrPositivePayDuplicateCheck = errors.New("ledger: positive pay: check already registered")
	// ErrInvalidPresentedCheck is returned for a presented check
	// with an empty account, number, currency, or payee, or a
	// non-positive amount.
	ErrInvalidPresentedCheck = errors.New("ledger: invalid presented check")
	// ErrPositivePayExceptionNotFound is returned when adjudicating
	// or reading an exception that does not exist.
	ErrPositivePayExceptionNotFound = errors.New("ledger: positive pay: exception not found")
	// ErrPositivePayExceptionNotPending is returned when adjudicating
	// an exception that is no longer pending.
	ErrPositivePayExceptionNotPending = errors.New("ledger: positive pay: exception is not pending")
	// ErrPositivePayInvalidDecision is returned for an adjudication
	// decision other than "pay" or "return".
	ErrPositivePayInvalidDecision = errors.New("ledger: positive pay: decision must be pay or return")
	// ErrPositivePayCheckNotRegistered is returned when stopping a
	// check that has no entry in the issued-check register: there is
	// nothing to stop.
	ErrPositivePayCheckNotRegistered = errors.New("ledger: positive pay: check is not registered")
	// ErrPositivePayCheckCleared is returned when stopping a check
	// that already cleared: the money is gone, a stop is meaningless.
	ErrPositivePayCheckCleared = errors.New("ledger: positive pay: check already cleared")
)

// IssuedCheck is one entry in the issued-check register: the check the
// operator wrote, against which presentments are compared.
type IssuedCheck struct {
	// Account is the account the check was drawn on.
	Account AccountID
	// Number is the check number, unique per account.
	Number string
	// AmountMinor is the issued amount in minor units.
	AmountMinor int64
	// Currency is the issued currency code.
	Currency string
	// Payee is the payee name written on the check.
	Payee string
	// IssueDate is the issue date as recorded by the issuer
	// (YYYY-MM-DD by convention; stored verbatim, never parsed for
	// the match decision).
	IssueDate string
}

// PresentedCheck is a check presented for payment, to compare against
// the issued-check register.
type PresentedCheck struct {
	Account     AccountID
	Number      string
	AmountMinor int64
	Currency    string
	Payee       string
}

// PositivePayException is one entry in the exception queue: a
// presented check that did not fully match its issued entry (or had
// none) and awaits — or received — a human pay/return decision.
type PositivePayException struct {
	Account   AccountID      `json:"account"`
	Number    string         `json:"number"`
	Reason    string         `json:"reason"`
	Status    string         `json:"status"`
	Presented PresentedCheck `json:"presented"`
}

// PositivePayResult is the outcome of presenting a check. Exception is
// set whenever the disposition is exception or returned, and nil when
// the check cleared.
type PositivePayResult struct {
	Disposition string
	Exception   *PositivePayException
}

// positivePayKey identifies one check within the register: check
// numbers are only unique per account, so the account is part of the
// key.
type positivePayKey struct {
	account AccountID
	number  string
}

// positivePayIssuedRecord is a registered check plus its clear state.
type positivePayIssuedRecord struct {
	check   IssuedCheck
	cleared bool
}

// ensurePositivePayLocked lazily initializes the positive-pay maps
// for hand-built Ledgers that bypassed New. Callers must hold the
// write lock.
func (l *Ledger) ensurePositivePayLocked() {
	if l.issuedChecks == nil {
		l.issuedChecks = make(map[positivePayKey]*positivePayIssuedRecord)
	}
	if l.positivePayExceptions == nil {
		l.positivePayExceptions = make(map[positivePayKey]*PositivePayException)
	}
	if l.positivePayStops == nil {
		l.positivePayStops = make(map[positivePayKey]bool)
	}
}

// validateIssuedCheck checks an issued check; an error names the
// offending field without carrying the payee value.
func validateIssuedCheck(c IssuedCheck) error {
	if strings.TrimSpace(string(c.Account)) == "" || strings.TrimSpace(c.Number) == "" ||
		c.AmountMinor <= 0 || strings.TrimSpace(c.Currency) == "" ||
		strings.TrimSpace(c.Payee) == "" || strings.TrimSpace(c.IssueDate) == "" {
		return ErrInvalidIssuedCheck
	}
	return nil
}

// validatePresentedCheck checks a presented check.
func validatePresentedCheck(c PresentedCheck) error {
	if strings.TrimSpace(string(c.Account)) == "" || strings.TrimSpace(c.Number) == "" ||
		c.AmountMinor <= 0 || strings.TrimSpace(c.Currency) == "" ||
		strings.TrimSpace(c.Payee) == "" {
		return ErrInvalidPresentedCheck
	}
	return nil
}

// RegisterIssuedCheck adds a check to the issued-check register and
// audits the registration (account + number + amount + currency —
// never the payee name). Registering an already-registered account +
// number is refused fail-closed: a duplicate registration must never
// silently overwrite the entry a presentment will be compared
// against.
func (l *Ledger) RegisterIssuedCheck(c IssuedCheck) error {
	if err := validateIssuedCheck(c); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ensurePositivePayLocked()
	key := positivePayKey{account: c.Account, number: c.Number}
	if _, ok := l.issuedChecks[key]; ok {
		return fmt.Errorf("%w: %s:%s", ErrPositivePayDuplicateCheck, c.Account, c.Number)
	}
	l.issuedChecks[key] = &positivePayIssuedRecord{check: c}
	l.emitAudit(AuditEvent{
		Op:       "positive_pay_issued",
		Actor:    "RegisterIssuedCheck",
		Accounts: []AccountID{c.Account},
		Details: map[string]any{
			"check_number": c.Number,
			"amount_minor": c.AmountMinor,
			"currency":     c.Currency,
		},
	})
	return nil
}

// PresentCheck compares a presented check against the register and
// returns its disposition. A full match clears (and marks the issued
// entry cleared, so a re-presentment is idempotently cleared); a
// mismatch or an unregistered check enters the exception queue as
// pending and returns the exception disposition; a re-presentment of
// a pending exception returns the same exception without duplicating
// it; a re-presentment after a pay decision returns cleared, and after
// a return decision returns returned. Audit events carry the check
// number, reason, and decision only — never the payee name.
func (l *Ledger) PresentCheck(p PresentedCheck) (PositivePayResult, error) {
	if err := validatePresentedCheck(p); err != nil {
		return PositivePayResult{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ensurePositivePayLocked()
	key := positivePayKey{account: p.Account, number: p.Number}
	if exc, ok := l.positivePayExceptions[key]; ok {
		switch exc.Status {
		case PositivePayExceptionPaid:
			return PositivePayResult{Disposition: PositivePayCleared}, nil
		case PositivePayExceptionReturned:
			return PositivePayResult{Disposition: PositivePayReturned, Exception: exc}, nil
		default: // pending: same exception, no duplicate entry
			return PositivePayResult{Disposition: PositivePayDispositionException, Exception: exc}, nil
		}
	}
	rec, ok := l.issuedChecks[key]
	if ok && rec.cleared {
		return PositivePayResult{Disposition: PositivePayCleared}, nil
	}
	if l.positivePayStops[key] {
		// Stop-payment (LG-355): a stopped check never clears on
		// sight, even when every field matches the register. It
		// enters the exception queue as pending (reason "stopped")
		// for a human to adjudicate with DecidePositivePayException,
		// exactly like any other exception; a re-presentment returns
		// the same pending exception through the branch above.
		exc := &PositivePayException{
			Account: p.Account, Number: p.Number,
			Reason: PositivePayReasonStopped, Status: PositivePayExceptionPending,
			Presented: p,
		}
		l.positivePayExceptions[key] = exc
		l.emitAudit(AuditEvent{
			Op:       "positive_pay_exception",
			Actor:    "PresentCheck",
			Accounts: []AccountID{p.Account},
			Details: map[string]any{
				"check_number": p.Number,
				"reason":       exc.Reason,
			},
		})
		return PositivePayResult{Disposition: PositivePayDispositionException, Exception: exc}, nil
	}
	if !ok {
		exc := &PositivePayException{
			Account: p.Account, Number: p.Number,
			Reason: PositivePayReasonUnregistered, Status: PositivePayExceptionPending,
			Presented: p,
		}
		l.positivePayExceptions[key] = exc
		l.emitAudit(AuditEvent{
			Op:       "positive_pay_exception",
			Actor:    "PresentCheck",
			Accounts: []AccountID{p.Account},
			Details: map[string]any{
				"check_number": p.Number,
				"reason":       exc.Reason,
			},
		})
		return PositivePayResult{Disposition: PositivePayDispositionException, Exception: exc}, nil
	}
	var reason string
	switch {
	case !strings.EqualFold(strings.TrimSpace(p.Currency), strings.TrimSpace(rec.check.Currency)):
		reason = PositivePayReasonCurrencyMismatch
	case p.AmountMinor != rec.check.AmountMinor:
		reason = PositivePayReasonAmountMismatch
	case !strings.EqualFold(normalizePayeeName(p.Payee), normalizePayeeName(rec.check.Payee)):
		reason = PositivePayReasonPayeeMismatch
	}
	if reason != "" {
		exc := &PositivePayException{
			Account: p.Account, Number: p.Number,
			Reason: reason, Status: PositivePayExceptionPending, Presented: p,
		}
		l.positivePayExceptions[key] = exc
		l.emitAudit(AuditEvent{
			Op:       "positive_pay_exception",
			Actor:    "PresentCheck",
			Accounts: []AccountID{p.Account},
			Details: map[string]any{
				"check_number": p.Number,
				"reason":       reason,
			},
		})
		return PositivePayResult{Disposition: PositivePayDispositionException, Exception: exc}, nil
	}
	rec.cleared = true
	l.emitAudit(AuditEvent{
		Op:       "positive_pay_cleared",
		Actor:    "PresentCheck",
		Accounts: []AccountID{p.Account},
		Details:  map[string]any{"check_number": p.Number},
	})
	return PositivePayResult{Disposition: PositivePayCleared}, nil
}

// DecidePositivePayException adjudicates a pending exception: "pay"
// approves it for posting, "return" bounces it. The decision is
// audited (check number + decision only) and enters the audit chain.
// Deciding a missing or already-decided exception is an error — a
// decision must never silently apply to the wrong check or double
// apply to the same one.
func (l *Ledger) DecidePositivePayException(account AccountID, number, decision string) error {
	if decision != PositivePayDecisionPay && decision != PositivePayDecisionReturn {
		return ErrPositivePayInvalidDecision
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ensurePositivePayLocked()
	key := positivePayKey{account: account, number: number}
	exc, ok := l.positivePayExceptions[key]
	if !ok {
		return fmt.Errorf("%w: %s:%s", ErrPositivePayExceptionNotFound, account, number)
	}
	if exc.Status != PositivePayExceptionPending {
		return fmt.Errorf("%w: %s:%s is %s", ErrPositivePayExceptionNotPending, account, number, exc.Status)
	}
	if decision == PositivePayDecisionPay {
		exc.Status = PositivePayExceptionPaid
	} else {
		exc.Status = PositivePayExceptionReturned
	}
	l.emitAudit(AuditEvent{
		Op:       "positive_pay_decision",
		Actor:    "DecidePositivePayException",
		Accounts: []AccountID{account},
		Details: map[string]any{
			"check_number": number,
			"decision":     decision,
		},
	})
	return nil
}

// StopIssuedCheck places a stop-payment order on a registered,
// uncleared check (LG-355): the lost/stolen-check counterpart to the
// register. A stopped check's presentment enters the exception queue
// with reason "stopped" instead of clearing, and a human adjudicates
// it with DecidePositivePayException. The stop touches only the
// stop-payment set — the register entry (amount, payee, issue date)
// is never modified. Stopping an unregistered or already-cleared
// check is refused fail-closed (there is nothing to stop, or the
// money is already gone); stopping an already-stopped check is a
// silent no-op with no second audit event. The stop itself is audited
// as "positive_pay_stop" (account + number only).
func (l *Ledger) StopIssuedCheck(account AccountID, number string) error {
	return l.StopIssuedCheckCtx(context.Background(), account, number)
}

// StopIssuedCheckCtx is StopIssuedCheck with per-call audit actor
// attribution (LG-73): the audit event records the operator carried
// by ctx when set, and the entrypoint name otherwise.
func (l *Ledger) StopIssuedCheckCtx(ctx context.Context, account AccountID, number string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ensurePositivePayLocked()
	key := positivePayKey{account: account, number: number}
	rec, ok := l.issuedChecks[key]
	if !ok {
		return fmt.Errorf("%w: %s:%s", ErrPositivePayCheckNotRegistered, account, number)
	}
	if rec.cleared {
		return fmt.Errorf("%w: %s:%s", ErrPositivePayCheckCleared, account, number)
	}
	if l.positivePayStops[key] {
		return nil
	}
	l.positivePayStops[key] = true
	l.emitAuditCtx(ctx, AuditEvent{
		Op:       "positive_pay_stop",
		Actor:    "StopIssuedCheck",
		Accounts: []AccountID{account},
		Details:  map[string]any{"check_number": number},
	})
	return nil
}

// ReleaseStopIssuedCheck lifts a stop-payment order (LG-355):
// presentments go back to the ordinary register comparison. Releasing
// does not silently clear an exception a stopped presentment already
// produced — that exception stays pending until a human adjudicates
// it. Releasing a check that is not stopped is a silent no-op with
// no audit event. The release is audited as
// "positive_pay_stop_released" (account + number only).
func (l *Ledger) ReleaseStopIssuedCheck(account AccountID, number string) error {
	return l.ReleaseStopIssuedCheckCtx(context.Background(), account, number)
}

// ReleaseStopIssuedCheckCtx is ReleaseStopIssuedCheck with per-call
// audit actor attribution (LG-73).
func (l *Ledger) ReleaseStopIssuedCheckCtx(ctx context.Context, account AccountID, number string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ensurePositivePayLocked()
	key := positivePayKey{account: account, number: number}
	if !l.positivePayStops[key] {
		return nil
	}
	delete(l.positivePayStops, key)
	l.emitAuditCtx(ctx, AuditEvent{
		Op:       "positive_pay_stop_released",
		Actor:    "ReleaseStopIssuedCheck",
		Accounts: []AccountID{account},
		Details:  map[string]any{"check_number": number},
	})
	return nil
}

// GetPositivePayException returns a copy of the exception for account
// + number, or false when there is none.
func (l *Ledger) GetPositivePayException(account AccountID, number string) (*PositivePayException, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	exc, ok := l.positivePayExceptions[positivePayKey{account: account, number: number}]
	if !ok {
		return nil, false
	}
	copy := *exc
	return &copy, true
}

// ListPositivePayExceptions returns copies of all exceptions, sorted
// by account then check number, so the queue reads deterministically.
func (l *Ledger) ListPositivePayExceptions() []PositivePayException {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]PositivePayException, 0, len(l.positivePayExceptions))
	for _, exc := range l.positivePayExceptions {
		out = append(out, *exc)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Account != out[j].Account {
			return out[i].Account < out[j].Account
		}
		return out[i].Number < out[j].Number
	})
	return out
}
