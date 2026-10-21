package ledger

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ACH return (退票/拒付) processing (LG-161, fintech).
//
// The bank answers an ACH origination file with a return file when a
// receiver's bank dishonors an entry: insufficient funds, closed
// account, no such account, and so on. The return file is still a
// 94-character fixed-width NACHA file — File Header (1), Batch
// Header (5), Entry Detail (6), Addenda (7), Batch Control (8), File
// Control (9) — but each dishonored Entry Detail (6) is followed by an
// Addenda Type 98 record (record type "7", positions 2-3 = "98")
// carrying the NACHA return reason code (R01–R85), the original entry's
// trace number, and the original receiving DFI.
//
// ParseNACHAReturn turns such a file into []ACHReturn; entries with no
// 98 addenda are not returns and are skipped. ProcessACHReturns books
// each return: it finds the payout instruction by the entry's
// individual ID (PayoutToNACHAEntry stamps the payout ID there), and
// reverses the payout's clearing entry ("payout/<id>/clearing") through
// ReverseEntry — the audit chain stays append-only: the original
// clearing row, the reversal row, and the reversal_of entry link all
// stay in the journal, so a dishonored payout leaves a fully
// reconcilable trail.
//
// The module is fail-loud: a malformed line or an invalid reason code
// rejects the whole file (no partial application at parse time), while
// processing is per-item — one bad return is recorded in its result,
// not applied to the other returns in the file.
//
// A return of a return (二次退回) is explicitly rejected: NACHA forbids
// returning a returned entry, and the ledger's deterministic reversal
// IDs mean the second return cannot book a second correction — it is
// refused with ErrACHReturnOfReturn, not silently swallowed.

// ACH return errors.
var (
	// ErrNACHAReturnLine is returned when a return-file line is not a
	// 94-character NACHA record (and not block padding): the file is
	// rejected whole, never partially applied.
	ErrNACHAReturnLine = errors.New("ledger: invalid NACHA return file line")
	// ErrNACHAInvalidReasonCode is returned when an addenda 98 record
	// carries a return reason code outside R01–R85: the file is
	// rejected whole — an unknown code must never be silently skipped,
	// because a return that goes unbooked leaves the books claiming
	// money that never moved.
	ErrNACHAInvalidReasonCode = errors.New("ledger: invalid NACHA return reason code")
	// ErrACHReturnPayoutNotFound is returned for a return whose
	// individual ID names no payout instruction: recorded per-item,
	// other returns still process.
	ErrACHReturnPayoutNotFound = errors.New("ledger: ACH return names unknown payout")
	// ErrACHReturnPayoutNotSubmitted is returned for a return whose
	// payout has no submitted clearing entry to correct: only a
	// submitted payout (status submitted, clearing entry journaled)
	// can be dishonored. Recorded per-item.
	ErrACHReturnPayoutNotSubmitted = errors.New("ledger: ACH return targets a payout that is not submitted")
	// ErrACHReturnOfReturn is returned when the return would correct
	// an entry that is already a correction: the clearing entry is
	// itself a reversal, or it already carries a genuine reversal
	// (a previous return, or a manual reversal) — NACHA forbids
	// returning a returned entry, and booking a second correction
	// would double-count the dishonor. Recorded per-item.
	ErrACHReturnOfReturn = errors.New("ledger: ACH return-of-return rejected")
	// ErrACHReturnAmountMismatch is returned when the return file's
	// amount differs from the clearing entry's amount: the return is
	// evidence about a different entry than the one it claims to be,
	// so it must not correct it. Recorded per-item.
	ErrACHReturnAmountMismatch = errors.New("ledger: ACH return amount does not match the clearing entry")
)

// ACHReturn is one dishonored entry parsed from a bank return file:
// the Entry Detail (6) fields plus its Addenda 98 record.
type ACHReturn struct {
	// ReasonCode is the NACHA return reason code, R01–R85.
	ReasonCode string `json:"reason_code"`
	// TraceNumber is the original entry's trace number (addenda 98,
	// positions 8-22).
	TraceNumber string `json:"trace_number"`
	// IndividualID is the entry's individual identification number
	// (positions 40-54): the payout ID, stamped by PayoutToNACHAEntry.
	IndividualID string `json:"individual_id"`
	// AmountCents is the entry amount in cents (positions 30-39).
	AmountCents int64 `json:"amount_cents"`
	// TransactionCode is the entry's transaction code (positions 2-3).
	TransactionCode string `json:"transaction_code"`
	// Routing is the receiver's 9-digit routing number (positions
	// 4-12: 8-digit DFI plus check digit).
	Routing string `json:"routing"`
	// AccountNumber is the receiver's account number (positions
	// 13-29).
	AccountNumber string `json:"account_number"`
	// AddendaInfo is the addenda 98 free text (positions 43-79).
	AddendaInfo string `json:"addenda_info"`
}

// ACHReturnResult is the per-item outcome of ProcessACHReturns: a
// return is either applied (ReversalID set), a duplicate replay
// (Duplicate true), or rejected (Err set). One item's failure never
// blocks the others.
type ACHReturnResult struct {
	// Return is the parsed return this result is about.
	Return ACHReturn `json:"return"`
	// PayoutID is the payout instruction the return was matched to
	// (the individual ID).
	PayoutID string `json:"payout_id"`
	// ReversalID is the booked reversal entry's ID
	// ("<clearing entry ID>/reversal"), empty when rejected.
	ReversalID string `json:"reversal_id,omitempty"`
	// AmountCents is the return's amount in cents.
	AmountCents int64 `json:"amount_cents"`
	// Duplicate reports an idempotent replay: the same return was
	// processed before, so the earlier reversal is returned and
	// nothing new was booked — no new audit event, no metric bump.
	Duplicate bool `json:"duplicate"`
	// Err is the per-item rejection reason, nil on success.
	Err error `json:"-"`
}

// validACHReturnReasonCode reports whether code is a NACHA return
// reason code: R01 through R85.
func validACHReturnReasonCode(code string) bool {
	if len(code) != 3 || code[0] != 'R' {
		return false
	}
	if code[1] < '0' || code[1] > '9' || code[2] < '0' || code[2] > '9' {
		return false
	}
	n := int(code[1]-'0')*10 + int(code[2]-'0')
	return n >= 1 && n <= 85
}

// nachaReturnEntry accumulates one Entry Detail (6) record and the
// Addenda 98 records that follow it.
type nachaReturnEntry struct {
	transactionCode string
	routing         string
	accountNumber   string
	amountCents     int64
	individualID    string
}

// ParseNACHAReturn parses a bank NACHA return file into the dishonored
// entries it carries. Lines are split on "\n" (a trailing newline is
// tolerated); every other line must be exactly 94 characters — block
// padding ("9" records) is skipped, anything else malformed rejects
// the whole file with ErrNACHAReturnLine. Each Entry Detail (6)
// followed by at least one Addenda Type 98 record yields one ACHReturn
// per 98 record; entries with no 98 addenda are not returns and yield
// nothing. An addenda 98 record with an invalid reason code rejects
// the whole file with ErrNACHAInvalidReasonCode — never silently
// skipped. An orphan addenda (no preceding entry detail) also rejects
// the file.
func ParseNACHAReturn(data string) ([]ACHReturn, error) {
	lines := strings.Split(data, "\n")
	// Tolerate one trailing newline; any other empty line is a
	// malformed record (fail-loud).
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	var out []ACHReturn
	var pending *nachaReturnEntry
	flush := func() {
		// Entries without 98 addenda are not returns: nothing to do.
		pending = nil
	}
	for i, raw := range lines {
		line := strings.TrimSuffix(raw, "\r")
		if line == strings.Repeat("9", nachaRecordLen) {
			continue // block padding
		}
		if len(line) != nachaRecordLen {
			return nil, fmt.Errorf("%w: line %d has length %d, want 94", ErrNACHAReturnLine, i+1, len(line))
		}
		switch line[0] {
		case '1', '5', '8', '9':
			flush()
		case '6':
			flush()
			amount, err := nachaReturnAmount(line[29:39])
			if err != nil {
				return nil, fmt.Errorf("%w: line %d: %v", ErrNACHAReturnLine, i+1, err)
			}
			pending = &nachaReturnEntry{
				transactionCode: line[1:3],
				routing:         line[3:12],
				accountNumber:   strings.TrimSpace(line[12:29]),
				amountCents:     amount,
				individualID:    strings.TrimSpace(line[39:54]),
			}
		case '7':
			if pending == nil {
				return nil, fmt.Errorf("%w: line %d: orphan addenda record", ErrNACHAReturnLine, i+1)
			}
			if line[1:3] != "98" {
				continue // payment-related "05" addenda, not a return
			}
			reason := line[3:6]
			if !validACHReturnReasonCode(reason) {
				return nil, fmt.Errorf("%w: line %d: %q", ErrNACHAInvalidReasonCode, i+1, reason)
			}
			out = append(out, ACHReturn{
				ReasonCode:      reason,
				TraceNumber:     line[7:22],
				IndividualID:    pending.individualID,
				AmountCents:     pending.amountCents,
				TransactionCode: pending.transactionCode,
				Routing:         pending.routing,
				AccountNumber:   pending.accountNumber,
				AddendaInfo:     strings.TrimSpace(line[43:80]),
			})
		default:
			return nil, fmt.Errorf("%w: line %d: unknown record type %q", ErrNACHAReturnLine, i+1, line[0])
		}
	}
	return out, nil
}

// nachaReturnAmount parses the 10-digit cents amount field of an entry
// detail record.
func nachaReturnAmount(field string) (int64, error) {
	if len(field) != 10 {
		return 0, fmt.Errorf("amount field %q is not 10 digits", field)
	}
	for i := 0; i < 10; i++ {
		if field[i] < '0' || field[i] > '9' {
			return 0, fmt.Errorf("amount field %q is not numeric", field)
		}
	}
	v, err := strconv.ParseInt(field, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("amount field %q: %v", field, err)
	}
	return v, nil
}

// achReturnIdempotencyKey builds the deterministic idempotency key for
// a return: trace number + reason code (the bank's stable identity for
// this dishonor) scoped by payout ID — trace numbers are unique only
// per originator per date, so the payout ID keeps two payouts' returns
// from sharing one key. Replaying the same return file yields the same
// key, so ReverseEntry returns the earlier reversal with
// duplicate == true instead of booking twice.
func achReturnIdempotencyKey(ret ACHReturn, payoutID string) string {
	return "ach-return/" + ret.TraceNumber + "/" + ret.ReasonCode + "/" + payoutID
}

// ProcessACHReturns books a batch of parsed ACH returns against their
// payout instructions. Each return is processed independently: it is
// matched to its payout by individual ID, the payout must be
// submitted (its clearing entry journaled), the return amount must
// equal the clearing entry's amount, and the clearing entry must not
// itself be (or already carry) a reversal — a return of a return is
// explicitly rejected with ErrACHReturnOfReturn. Successful returns
// reverse the clearing entry through ReverseEntry with the
// deterministic ach-return idempotency key, emit an "ach_return"
// audit event, and bump the ledger's per-reason-code return counter
// (the source of ledger_ach_returns_total{reason_code}).
//
// The whole batch runs under one write lock via the internal locked
// path; a rejection records nothing for that item but does not block
// the others. The top-level error is nil unless the batch itself is
// unprocessable (it never is today — all per-item failures land in
// the results).
func (l *Ledger) ProcessACHReturns(returns []ACHReturn, actor string) ([]ACHReturnResult, error) {
	if actor == "" {
		actor = "ProcessACHReturns"
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	results := make([]ACHReturnResult, 0, len(returns))
	for i := range returns {
		results = append(results, l.processACHReturnLocked(returns[i], actor))
	}
	return results, nil
}

// processACHReturnLocked applies one return. Callers must hold l.mu
// (the write lock).
func (l *Ledger) processACHReturnLocked(ret ACHReturn, actor string) ACHReturnResult {
	res := ACHReturnResult{Return: ret, AmountCents: ret.AmountCents}
	payoutID := strings.TrimSpace(ret.IndividualID)
	res.PayoutID = payoutID
	p, ok := l.payouts[payoutID]
	if !ok {
		res.Err = fmt.Errorf("%w: individual ID %q", ErrACHReturnPayoutNotFound, payoutID)
		return res
	}
	if p.Status != PayoutSubmitted || p.ClearingEntryID == "" {
		res.Err = fmt.Errorf("%w: payout %q status %q", ErrACHReturnPayoutNotSubmitted, payoutID, p.Status)
		return res
	}
	clearingID := p.ClearingEntryID
	clearing, ok := l.entries[clearingID]
	if !ok {
		res.Err = fmt.Errorf("%w: clearing entry %q", ErrEntryNotFound, clearingID)
		return res
	}
	// Return-of-return, first line of defense: the corrected entry
	// must be an original posting, never itself a reversal — a return
	// corrects a payment, not a correction.
	if strings.Contains(clearingID, "/reversal") || strings.Contains(clearing.Memo, "reversal_of:") {
		res.Err = fmt.Errorf("%w: clearing entry %q is itself a reversal", ErrACHReturnOfReturn, clearingID)
		return res
	}
	if clearing.AmountCents != ret.AmountCents {
		res.Err = fmt.Errorf("%w: return %d vs clearing %d", ErrACHReturnAmountMismatch, ret.AmountCents, clearing.AmountCents)
		return res
	}
	key := achReturnIdempotencyKey(ret, payoutID)
	reversal, duplicate, err := l.reverseEntryLocked(clearingID, key, nil)
	if err != nil {
		if errors.Is(err, ErrAlreadyReversed) {
			// The clearing entry already carries a genuine reversal —
			// a previous return (or a manual reversal): the second
			// return cannot book a second correction, so it is an
			// explicit return-of-return rejection, not a silent skip.
			res.Err = fmt.Errorf("%w: clearing entry %q already reversed", ErrACHReturnOfReturn, clearingID)
		} else {
			res.Err = err
		}
		return res
	}
	res.ReversalID = reversal.ID
	res.Duplicate = duplicate
	if duplicate {
		// Idempotent replay: the reversal, its audit event, and the
		// metric were all recorded on the first pass — book nothing
		// new, report the replay.
		return res
	}
	// The ledger-side return counter, by reason code: the source of
	// ledger_ach_returns_total{reason_code} on GET /metrics.
	l.achReturnCounts[ret.ReasonCode]++
	l.emitAudit(AuditEvent{
		Op:            "ach_return",
		Actor:         actor,
		TraceID:       reversal.ID,
		VersionBefore: l.version,
		VersionAfter:  l.version,
		EntryIDs:      []string{reversal.ID},
		Accounts:      []AccountID{reversal.DebitAccount, reversal.CreditAccount},
		Details: map[string]any{
			"reason_code":     ret.ReasonCode,
			"trace_number":    ret.TraceNumber,
			"payout_id":       payoutID,
			"reversal_id":     reversal.ID,
			"amount_cents":    ret.AmountCents,
			"idempotency_key": key,
		},
	})
	return res
}

// ACHReturnCounts returns the ledger's processed ACH return counts by
// return reason code: the authoritative source for the
// ledger_ach_returns_total{reason_code} metric series. Only
// first-time returns count — idempotent replays return duplicate and
// move nothing. In-memory only, like payout instructions: the counter
// does not round-trip through disaster-recovery snapshots; the
// journaled reversal rows and the ach_return audit events are the
// durable record.
func (l *Ledger) ACHReturnCounts() map[string]uint64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make(map[string]uint64, len(l.achReturnCounts))
	for code, n := range l.achReturnCounts {
		out[code] = n
	}
	return out
}
