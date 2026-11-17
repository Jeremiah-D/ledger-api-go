package ledger

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// SWIFT FIN MT210 notice to receive inbound processing (LG-448):
// the inbound half of the MT210 lifecycle, the mirror of the
// LG-393 export — and the FIN equivalent of the camt.057 inbound
// half (LG-420).
//
// ExportMT210 renders this ledger's own pre-advice — the account
// owner telling its bank "money is coming into my account". This
// module consumes the pre-advice a counterparty account owner sent
// to this ledger, acting as the account bank: it registers the
// expected receipt so intraday liquidity can forecast it, and
// nothing else. The two directions are orthogonal by construction:
//
//   - LG-393 exports: the ledger tells its bank about the next run
//     of one of its own transfer schedules (outbound, read-only).
//   - LG-448 imports: a counterparty tells the ledger about money
//     it expects to send (inbound, state-changing only in the
//     expected-receipts registry: one forecast record per :20:).
//
// ParseMT210 builds a new value object — the outbound export
// renders with strings.Builder, so there is no wire struct to
// reuse in reverse (the pain.009 inbound convention, LG-422).
// Parsing is strict, along ParseMT320 (LG-446): the block
// structure, the field order, and every field value are validated,
// and anything outside the six-field core set (:20: :25: :30:
// :21: :32B: :50a:/:52a:) is refused — a notice must not lose
// content quietly. CRLF line endings are accepted; a trailing
// newline after the `-}` trailer is tolerated. The :50a: / :52a:
// `a` option means "BIC or name-and-address": this module
// implements the A (BIC) variant and the D (account-ID) variant
// the export renders, and refuses any other option fail-closed
// rather than silently dropping the party identity.
//
// ApplyMT210 registers the parsed notice as an ExpectedReceipt in
// the SAME registry ApplyCAMT057 writes (see camt057_inbound.go):
// the :20: is the MsgId (the replay key), the :21: the
// NotificationID, the :25: the account to be credited (a
// registered IBAN when the value is a well-formed IBAN, else the
// account ID — the camt.057 convention), the :30: the expected
// value date (6!n, rendered YYYY-MM-DD), the :32B: the principal
// only (no fee deduction, the export's rule), and the :50a: /
// :52a: the debtor, carried verbatim. Registration reuses
// ApplyCAMT057's three paths: a replayed :20: with the same
// notification is a no-op returning the stored receipt with
// Duplicate set; a :20: reused for a different notification is
// refused with ErrMT210MsgIDConflict; an unknown :25: account is
// refused fail-closed and parked in the repair queue, never
// dropped. A registered notice is a forecast only — no journal
// rows, no version bump, no money moved — and counts as the fifth
// intraday-liquidity source on its expected value date (see
// liquidity.go), exactly like a camt.057 registration, so it can
// never double-count against a later pacs.008/pacs.009 receipt of
// the same money.
//
// No cancellation is built here: camt.058 cancellation (LG-421)
// already cancels ExpectedReceipts by MsgId, so an MT210
// registration is naturally cancellable by a counterparty's
// camt.058 advice — the cross-message consistency the backlog
// item calls out. AMND/CANC variants are out of scope: the export
// only ever issues a single NEWT-style notice, and a funds-arrival
// confirmation is a different message (a notice is not a receipt).
//
// Success emits `mt210_notification_received` (details msg_id /
// notification_id / acct / amount_cents / currency /
// expected_value_date / debtor), distinct from the camt.057
// event — the audit trail tells the FIN direction apart from the
// MX one even though both land in the same registry.

// MT210 inbound errors.
var (
	// ErrInvalidMT210 is returned by ParseMT210 for a document the
	// ledger cannot read as a notice to receive, and by
	// ApplyMT210 for a notification missing its MsgId. The HTTP
	// layer parks it in the repair queue and maps it to 400.
	ErrInvalidMT210 = errors.New("ledger: invalid MT210 notice to receive")
	// ErrMT210Framing is returned when the FIN block structure is
	// broken: a missing block, a malformed block 1/2/3 header, a
	// missing or unterminated block 4, or a line count other than
	// the eleven the six-field core set renders.
	ErrMT210Framing = errors.New("ledger: mt210: malformed FIN framing")
	// ErrMT210Field is returned for a block-4 field problem that
	// is not a value problem: a missing required field, a field
	// outside the six-field core set, a :50a:/:52a: option other
	// than :52A: or :50D:, or both :50a: and :52a: present.
	ErrMT210Field = errors.New("ledger: mt210: unexpected or missing field")
	// ErrMT210Reference is returned for an invalid :20: or :21:
	// reference: empty, over 16 characters, outside the SWIFT `x`
	// character set, with a leading/trailing colon or slash, or a
	// double slash.
	ErrMT210Reference = errors.New("ledger: mt210: invalid reference")
	// ErrMT210ValueDate is returned for an invalid :30: date: not
	// a 6!n YYMMDD calendar date.
	ErrMT210ValueDate = errors.New("ledger: mt210: invalid :30: date")
	// ErrMT210Currency is returned for an invalid :32B: currency:
	// not exactly three uppercase ASCII letters (the LG-18
	// currency rule — no lowercase, no digits, no ISO lookup
	// table the ledger does not own).
	ErrMT210Currency = errors.New("ledger: mt210: invalid :32B: currency")
	// ErrMT210Amount is returned for an invalid :32B: amount:
	// non-positive, not in `<integer>,<2 fraction digits>` form,
	// or with more than 12 integer digits (the MT103 15d rule).
	ErrMT210Amount = errors.New("ledger: mt210: invalid :32B: amount")
	// ErrMT210LT is returned for an invalid sender or receiver LT
	// address (not a valid BIC8/BIC11/BIC12 form).
	ErrMT210LT = errors.New("ledger: mt210: invalid SWIFT LT address")
	// ErrMT210UnknownAccount is returned by ApplyMT210 when the
	// notification's :25: names no account the ledger knows: the
	// ledger never forecasts money into an account it never heard
	// of. The notification is parked in the repair queue, not
	// dropped.
	ErrMT210UnknownAccount = errors.New("ledger: mt210 inbound: notification account is unknown")
	// ErrMT210MsgIDConflict is returned when a :20: reference
	// already registered names a different notification (another
	// :21:, amount, or currency): one message reference names one
	// notification. It is a 422-class error.
	ErrMT210MsgIDConflict = errors.New("ledger: mt210 :20: already registered for a different notification")
)

// MT210Notification is one parsed inbound SWIFT FIN MT210 notice
// to receive: the block-4 core fields plus the block 1/2 LT
// addresses. ExpectedValueDate is the :30: date rendered
// YYYY-MM-DD (the registry's day form); AmountCents is the :32B:
// principal in minor units, always positive.
type MT210Notification struct {
	SenderLT   string
	ReceiverLT string
	// MsgID is :20:, the schedule ID — this message's own
	// reference and the replay key.
	MsgID string `json:"msg_id"`
	// NotificationID is :21:, the run transfer ID
	// ("<ID>/run/<seq>").
	NotificationID string `json:"notification_id"`
	// AccountIBAN is :25: when the value is a well-formed IBAN;
	// AccountOthr is :25: otherwise (the account ID). Exactly one
	// is set: the camt.057 convention (registered IBAN, else
	// account ID).
	AccountIBAN string `json:"account_iban,omitempty"`
	AccountOthr string `json:"account_othr,omitempty"`
	// AmountCents is the :32B: principal in minor units; Currency
	// is its 3-letter uppercase ISO 4217 code.
	AmountCents int64  `json:"amount_cents"`
	Currency    string `json:"currency"`
	// ExpectedValueDate is :30: (6!n YYMMDD) rendered YYYY-MM-DD,
	// the day the money is expected: the intraday-liquidity
	// forecast day.
	ExpectedValueDate string `json:"expected_value_date"`
	// Debtor is the :52A: BIC or the :50D: ordering customer,
	// carried verbatim; DebtorBIC reports which form it is.
	Debtor    string `json:"debtor"`
	DebtorBIC bool   `json:"debtor_bic,omitempty"`
}

// MT210ApplyResult is the outcome of ApplyMT210.
type MT210ApplyResult struct {
	// Receipt is the registered notification (or the stored one on
	// the duplicate path).
	Receipt ExpectedReceipt `json:"receipt"`
	// RepairID is set on the parked fail-closed path (unknown
	// account): the repair-queue record holding the notification
	// for operator resolution.
	RepairID string `json:"repair_id,omitempty"`
	// Duplicate reports whether the notification was already
	// registered under the same :20:: a no-op success, not an
	// error.
	Duplicate bool `json:"duplicate,omitempty"`
}

// mt210Reference validates a :20: or :21: reference: 1–16 SWIFT
// `x` characters, no braces in block-4 content (see
// mt103CheckX), no leading/trailing colon or slash, no double
// slash. The MT202 rule, under the MT210 error identity.
func mt210Reference(ref, field string) error {
	if ref == "" || len(ref) > mt103MaxSenderReference {
		return fmt.Errorf("%w: %s: want 1-%d characters, got %q", ErrMT210Reference, field, mt103MaxSenderReference, ref)
	}
	if err := mt103CheckX(ref); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrMT210Reference, field, err)
	}
	if strings.HasPrefix(ref, ":") || strings.HasPrefix(ref, "/") ||
		strings.HasSuffix(ref, ":") || strings.HasSuffix(ref, "/") ||
		strings.Contains(ref, "//") {
		return fmt.Errorf("%w: %s: leading/trailing colon or slash, or double slash: %q", ErrMT210Reference, field, ref)
	}
	return nil
}

// mt210Currency validates a :32B: currency code: exactly three
// uppercase ASCII letters. The MT202 rule, under the MT210 error
// identity.
func mt210Currency(code string) error {
	if len(code) != 3 {
		return fmt.Errorf("%w: want 3 letters, got %q", ErrMT210Currency, code)
	}
	for i := 0; i < 3; i++ {
		if code[i] < 'A' || code[i] > 'Z' {
			return fmt.Errorf("%w: %q", ErrMT210Currency, code)
		}
	}
	return nil
}

// mt210ParseLT extracts the LT address from a block 1 or block 2
// header line and returns it exactly as written. Block 1 has the
// form `{1:F01<LT><10 session/sequence digits>}`; block 2 has the
// form `{2:I210<LT><5 header characters>}` (priority U, delivery
// monitoring 3, obsolescence period 003). The MT202 rule, under
// the MT210 error identity.
func mt210ParseLT(line, prefix string, tailLen int) (string, error) {
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, "}") {
		return "", fmt.Errorf("%w: %q", ErrMT210Framing, line)
	}
	inner := line[len(prefix) : len(line)-1]
	if len(inner) <= tailLen {
		return "", fmt.Errorf("%w: %q", ErrMT210Framing, line)
	}
	lt, tail := inner[:len(inner)-tailLen], inner[len(inner)-tailLen:]
	// Block 1's tail is the session/sequence digits (digit-checked);
	// block 2's tail is header flags (any 5 characters: U3003).
	if tailLen == 10 {
		for _, c := range tail {
			if c < '0' || c > '9' {
				return "", fmt.Errorf("%w: %q", ErrMT210Framing, line)
			}
		}
	}
	if !mt202ValidLT(lt) {
		return "", fmt.Errorf("%w: %q", ErrMT210LT, lt)
	}
	return lt, nil
}

// mt210ParseAmountField parses a :32B: body (3-letter currency +
// `<integer>,<2 fraction digits>`) into its currency and
// minor-unit amount. The 15d amount rule rides mt202ParseAmount,
// under the MT210 error identity.
func mt210ParseAmountField(body, field string) (currency string, cents int64, err error) {
	if len(body) < 3+4 {
		return "", 0, fmt.Errorf("%w: %s too short: %q", ErrMT210Framing, field, body)
	}
	currency = body[:3]
	if err := mt210Currency(currency); err != nil {
		return "", 0, fmt.Errorf("%s: %w", field, err)
	}
	amount, err := mt202ParseAmount(body[3:])
	if err != nil {
		return "", 0, fmt.Errorf("%s: %w: %v", field, ErrMT210Amount, err)
	}
	return currency, amount, nil
}

// mt210ParseValueDate parses a :30: 6!n YYMMDD body into the
// registry's YYYY-MM-DD day form (UTC).
func mt210ParseValueDate(body, field string) (string, error) {
	if len(body) != 6 {
		return "", fmt.Errorf("%w: %s want 6!n YYMMDD, got %q", ErrMT210ValueDate, field, body)
	}
	d, err := time.Parse("060102", body)
	if err != nil {
		return "", fmt.Errorf("%w: %s %q: %w", ErrMT210ValueDate, field, body, err)
	}
	return d.UTC().Format("2006-01-02"), nil
}

// mt210FieldBody extracts the body of a single-line field at a
// fixed position: the line must start with the tag and carry a
// non-empty body.
func mt210FieldBody(line, tag string, lineNo int) (string, error) {
	if !strings.HasPrefix(line, tag) {
		return "", fmt.Errorf("%w: line %d: want %s field, got %q", ErrMT210Field, lineNo, tag, line)
	}
	body := line[len(tag):]
	if body == "" {
		return "", fmt.Errorf("%w: %s field is empty", ErrMT210Field, tag)
	}
	return body, nil
}

// mt210HasDebtorLine reports whether any line opens a :50a: or
// :52a: party field — the both-present check on a document whose
// line count already failed.
func mt210HasDebtorLine(lines []string, tag string) bool {
	for _, line := range lines {
		if strings.HasPrefix(line, tag) {
			return true
		}
	}
	return false
}

// ParseMT210 parses a SWIFT FIN MT210 message produced by
// ExportMT210 (or an equivalent counterparty message) into its
// business fields. Parsing is strict: the block structure, the
// field order, and every field value are validated, and anything
// outside the six-field core set is refused — a notice must not
// lose content quietly. CRLF line endings are accepted; a trailing
// newline after the `-}` trailer is tolerated.
func ParseMT210(text string) (MT210Notification, error) {
	var zero MT210Notification
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	lines := strings.Split(text, "\n")
	if len(lines) != 11 {
		// A notice carrying both :50a: and :52a: fails the count
		// with one extra line: name the real refusal instead of
		// the line count.
		if mt210HasDebtorLine(lines, ":50") && mt210HasDebtorLine(lines, ":52") {
			return zero, fmt.Errorf("%w: both :50a: and :52a: present, exactly one ordering party is allowed", ErrMT210Field)
		}
		return zero, fmt.Errorf("%w: want 11 lines (3 headers + {4: + 6 fields + -}), got %d", ErrMT210Framing, len(lines))
	}
	senderLT, err := mt210ParseLT(lines[0], "{1:F01", 10)
	if err != nil {
		return zero, err
	}
	receiverLT, err := mt210ParseLT(lines[1], "{2:I210", 5)
	if err != nil {
		return zero, err
	}
	// Block 3: shape-checked only (see the MT202 package doc).
	mur := lines[2]
	if !strings.HasPrefix(mur, "{3:{108:") || !strings.HasSuffix(mur, "}}") {
		return zero, fmt.Errorf("%w: malformed block 3: %q", ErrMT210Framing, mur)
	}
	if err := mt210Reference(mur[len("{3:{108:"):len(mur)-2], "{108:}"); err != nil {
		return zero, err
	}
	if lines[3] != "{4:" {
		return zero, fmt.Errorf("%w: want {4: on line 4, got %q", ErrMT210Framing, lines[3])
	}
	if lines[10] != "-}" {
		return zero, fmt.Errorf("%w: want -} trailer, got %q", ErrMT210Framing, lines[10])
	}
	var m MT210Notification
	m.SenderLT, m.ReceiverLT = senderLT, receiverLT
	// :20: — the schedule ID, the addressing key and replay key.
	body, err := mt210FieldBody(lines[4], ":20:", 5)
	if err != nil {
		return zero, err
	}
	if err := mt210Reference(body, ":20:"); err != nil {
		return zero, err
	}
	m.MsgID = body
	// :25: — the account to be credited: a well-formed IBAN, else
	// the account ID (the camt.057 convention).
	account, err := mt210FieldBody(lines[5], ":25:", 6)
	if err != nil {
		return zero, err
	}
	if len(account) > 35 {
		return zero, fmt.Errorf("%w: :25: account line %q exceeds 35 characters", ErrMT210Field, account)
	}
	if err := mt103CheckX(account); err != nil {
		return zero, fmt.Errorf("%w: :25: %w", ErrMT210Field, err)
	}
	if validIBAN(normalizeIBAN(account)) {
		m.AccountIBAN = normalizeIBAN(account)
	} else {
		m.AccountOthr = account
	}
	// :30: — the next value date, 6!n.
	dateBody, err := mt210FieldBody(lines[6], ":30:", 7)
	if err != nil {
		return zero, err
	}
	valueDate, err := mt210ParseValueDate(dateBody, ":30:")
	if err != nil {
		return zero, err
	}
	m.ExpectedValueDate = valueDate
	// :21: — the run transfer ID ("<ID>/run/<seq>").
	runBody, err := mt210FieldBody(lines[7], ":21:", 8)
	if err != nil {
		return zero, err
	}
	if err := mt210Reference(runBody, ":21:"); err != nil {
		return zero, err
	}
	m.NotificationID = runBody
	// :32B: — the principal to be received.
	amountBody, err := mt210FieldBody(lines[8], ":32B:", 9)
	if err != nil {
		return zero, err
	}
	currency, cents, err := mt210ParseAmountField(amountBody, ":32B:")
	if err != nil {
		return zero, err
	}
	m.Currency, m.AmountCents = currency, cents
	// :50a: / :52a: — exactly one ordering party: the :52A: BIC
	// when the ordering party is a financial institution, else
	// the :50D: ordering customer named by account ID. Any other
	// option is refused fail-closed.
	debtorLine := lines[9]
	switch {
	case strings.HasPrefix(debtorLine, ":52A:"):
		bic := debtorLine[len(":52A:"):]
		if bic == "" {
			return zero, fmt.Errorf("%w: :52A: field is empty", ErrMT210Field)
		}
		if !validBIC(bic) {
			return zero, fmt.Errorf("%w: :52A: %q is not a valid BIC", ErrMT210Field, bic)
		}
		m.Debtor, m.DebtorBIC = bic, true
	case strings.HasPrefix(debtorLine, ":50D:"):
		name := debtorLine[len(":50D:"):]
		if name == "" {
			return zero, fmt.Errorf("%w: :50D: field is empty", ErrMT210Field)
		}
		if err := mt103CheckX(name); err != nil {
			return zero, fmt.Errorf("%w: :50D: %w", ErrMT210Field, err)
		}
		m.Debtor = name
	default:
		return zero, fmt.Errorf("%w: line 10: want :52A: or :50D: field, got %q", ErrMT210Field, debtorLine)
	}
	return m, nil
}

// mt210ToCAMT057Notification converts a parsed MT210 notice to
// the camt.057 shape ApplyCAMT057's shared helpers read: only the
// account fields are carried — resolution never looks at the
// rest. The conversion keeps one resolution rule for both
// syntaxes of the same notice.
func mt210ToCAMT057Notification(n MT210Notification) CAMT057Notification {
	return CAMT057Notification{
		AccountIBAN: n.AccountIBAN,
		AccountOthr: n.AccountOthr,
	}
}

// parkMT210Notification parks a parsed MT210 notice that failed
// closed, and returns the repair record ID. The snapshot's Maker
// is deliberately empty (see the camt057_inbound.go package
// comment).
func (l *Ledger) parkMT210Notification(n MT210Notification, reasonCode, actor string) (string, error) {
	rec, err := l.EnqueueRepair(PayoutInstruction{
		ID:          n.MsgID,
		AmountCents: n.AmountCents,
		Currency:    n.Currency,
		Destination: "mt210-inbound:" + n.MsgID,
		// Maker intentionally empty: see the package comment.
	}, reasonCode, actor)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// ParkMT210Unparsed best-effort parks an MT210 document ParseMT210
// refused, when the document still names a notification: the :20:
// reference it carries. It returns the repair record ID, or ""
// when the document names nothing parkable (garbage bytes, an
// empty shell): there is no notification for the operator to find,
// so there is nothing to park. A parking failure (a full queue)
// is returned so the caller can fail loud rather than silently
// drop the counterparty's message. The reason code is
// invalid_format — the document was present but unreadable as a
// notification.
func (l *Ledger) ParkMT210Unparsed(text, actor string) (string, error) {
	id := ""
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(line, ":20:") {
			id = strings.TrimSpace(line[len(":20:"):])
			break
		}
	}
	if id == "" {
		return "", nil
	}
	rec, err := l.EnqueueRepair(PayoutInstruction{
		ID: id,
		// Maker intentionally empty: see the package comment.
		Destination: "mt210-inbound:" + id,
	}, RepairReasonInvalidFormat, actor)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// ApplyMT210 registers one parsed inbound SWIFT FIN MT210 notice
// to receive as an expected receipt: the inbound half of the MT210
// flow, the counterparty-side mirror of the outbound export — the
// FIN equivalent of ApplyCAMT057, writing the same registry. The
// notification's :25: must name an account the ledger already
// knows — a registered IBAN resolves through the IBAN registry,
// an account ID names the account directly — otherwise the
// notification is refused fail-closed and parked in the repair
// queue.
//
// Only the forecast registers: no journal rows, no version bump,
// no money moved. The registered receipt counts as the fifth
// intraday-liquidity source on its expected value date (see
// liquidity.go) and nowhere else.
//
// Idempotency is the :20:: re-applying a :20: already registered
// returns the stored receipt with Duplicate set, no second audit
// event. A :20: reused for a different notification is refused
// with ErrMT210MsgIDConflict.
//
// Locking: the duplicate read and the account resolution run under
// the read lock; the registration section holds the write lock
// (the re-checks, the registry write, and the audit event are one
// atomic section); the repair parking runs outside any held lock
// because EnqueueRepair takes l.mu itself.
func (l *Ledger) ApplyMT210(n MT210Notification, actor string) (MT210ApplyResult, error) {
	if strings.TrimSpace(n.MsgID) == "" {
		return MT210ApplyResult{}, fmt.Errorf("%w: missing :20:", ErrInvalidMT210)
	}
	if actor == "" {
		actor = "ApplyMT210"
	}

	// Duplicate read and account resolution: the same :20:
	// registered before is a no-op success, whatever the ledger's
	// current state.
	l.mu.RLock()
	stored, dup := l.expectedReceipts[n.MsgID]
	acct, resolveErr := l.resolveCAMT057AccountLocked(mt210ToCAMT057Notification(n))
	l.mu.RUnlock()
	if dup {
		// One :20: names one notification: a different :21:,
		// amount, or currency under it is a conflict. The
		// account is compared only when it resolves — a failed
		// resolution says nothing about the stored receipt,
		// which resolved when it was registered.
		if stored.NotificationID != n.NotificationID || stored.AmountCents != n.AmountCents || stored.Currency != n.Currency {
			return MT210ApplyResult{}, fmt.Errorf("%w: :20: %q", ErrMT210MsgIDConflict, n.MsgID)
		}
		if resolveErr == nil && stored.Account != acct {
			return MT210ApplyResult{}, fmt.Errorf("%w: :20: %q", ErrMT210MsgIDConflict, n.MsgID)
		}
		return MT210ApplyResult{Receipt: stored, Duplicate: true}, nil
	}
	if resolveErr != nil {
		// The parked fail-closed path, outside any held lock: an
		// unknown account goes to the repair queue.
		repairID, err := l.parkMT210Notification(n, RepairReasonUnknownRoute, actor)
		if err != nil {
			return MT210ApplyResult{}, fmt.Errorf("%w (repair parking failed: %v)", resolveErr, err)
		}
		account := n.AccountIBAN
		if account == "" {
			account = n.AccountOthr
		}
		return MT210ApplyResult{RepairID: repairID}, fmt.Errorf("%w: :25: %q", ErrMT210UnknownAccount, account)
	}

	// Registration section: re-check the duplicate under the write
	// lock — a racing apply for the same :20: converges here
	// instead of double-registering.
	l.mu.Lock()
	defer l.mu.Unlock()
	if stored, dup := l.expectedReceipts[n.MsgID]; dup {
		if stored.NotificationID != n.NotificationID || stored.AmountCents != n.AmountCents || stored.Currency != n.Currency || stored.Account != acct {
			return MT210ApplyResult{}, fmt.Errorf("%w: :20: %q", ErrMT210MsgIDConflict, n.MsgID)
		}
		return MT210ApplyResult{Receipt: stored, Duplicate: true}, nil
	}
	receipt := ExpectedReceipt{
		MsgID:             n.MsgID,
		NotificationID:    n.NotificationID,
		Account:           acct,
		AmountCents:       n.AmountCents,
		Currency:          n.Currency,
		ExpectedValueDate: n.ExpectedValueDate,
		Debtor:            n.Debtor,
		Actor:             actor,
		ReceivedAt:        time.Now().UTC(),
	}
	if l.expectedReceipts == nil {
		l.expectedReceipts = make(map[string]ExpectedReceipt)
	}
	l.expectedReceipts[n.MsgID] = receipt
	l.emitAudit(AuditEvent{
		Op:       "mt210_notification_received",
		Actor:    actor,
		TraceID:  n.MsgID,
		Accounts: []AccountID{acct},
		Details: map[string]any{
			"msg_id":              n.MsgID,
			"notification_id":     n.NotificationID,
			"acct":                string(acct),
			"amount_cents":        n.AmountCents,
			"currency":            n.Currency,
			"expected_value_date": n.ExpectedValueDate,
			"debtor":              n.Debtor,
		},
	})
	return MT210ApplyResult{Receipt: receipt}, nil
}
